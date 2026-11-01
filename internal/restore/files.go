package restore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// The files step of a restore and a replace (#324): the files of the backup's files archive go
// back onto the device's flash file system before the settings, so that the one restart of the
// settings restore already loads autoexec.be and the Berry scripts. Nothing on the device is
// deleted. Every upload is a flash write on the device: files go one after the other, an upload is
// never repeated in a loop, and the writes are counted for the summary.

// FileArchives is the part of backup.Manager the files step reads. Optional: without it a restore
// puts back settings only.
type FileArchives interface {
	OpenFilesArchive(ctx context.Context, id model.BackupID) (*backup.FilesArchive, error)
	RunFiles(ctx context.Context, id model.BackupID) (backup.RunFiles, error)
}

// PlannedFile is one file a restore would write.
type PlannedFile struct {
	Path       string // device path with the leading slash
	Size       int64
	PrivateKey bool // /tlskey: the page marks it, content is never shown
}

// Name is the path the pages show (as Tasmota's file manager does, without the leading slash).
func (f PlannedFile) Name() string { return strings.TrimPrefix(f.Path, "/") }

// FilesPlan is what a restore does with the backup's files.
type FilesPlan struct {
	Files []PlannedFile
	Bytes int64
	// NotStored are files the backup lists but does not hold (over the size limit): they stay as
	// they are on the device.
	NotStored []backup.ManifestSkip
	// Why says, when files exist in the backup but this restore will not write them, the reason
	// ("files need HTTP", "the device has no file system", a damaged archive). Empty: they are written.
	Why string
	// Note says why the backup holds no files at all (its run note); empty when it has files or
	// predates the files step.
	Note    string
	archive *backup.FilesArchive
}

// Restores reports whether the files step writes anything.
func (p FilesPlan) Restores() bool { return p.Why == "" && len(p.Files) > 0 }

// FileState is where one file of the files step stands.
type FileState string

// The states of a file in the progress list.
const (
	FileWaiting FileState = "waiting"
	FileWriting FileState = "writing"
	FileWritten FileState = "written"
	FileSame    FileState = "same"    // the device already holds this content: no write
	FileSkipped FileState = "skipped" // not written; Why says why (folder missing)
	FileFailed  FileState = "failed"  // the upload failed; the old file was put back, see Lost
)

// FileProgress is one file of the files step.
type FileProgress struct {
	PlannedFile
	State FileState
	Why   string
}

// FilesProgress is the live view of the files step of a job.
type FilesProgress struct {
	Files []FileProgress
	// NotStored are the files the backup lists but does not hold; they stay as they are on the device.
	NotStored []backup.ManifestSkip
	// Writes counts the uploads that reached the device (each is a flash write there), put-backs included.
	Writes int
	// Lost names files that could not be put back after a failed upload: their old content is gone
	// from the device.
	Lost []string
}

// Count returns how many files are in the given state.
func (p FilesProgress) Count(s FileState) int {
	n := 0
	for _, f := range p.Files {
		if f.State == s {
			n++
		}
	}
	return n
}

// progressKeep bounds the memory the per-file progress of finished jobs takes.
const progressKeep = 32

// FilesProgress returns the per-file progress of a job this process ran. It is not stored: after
// a restart of TasmoFleet only the summary in the job's Detail is left.
func (m *Manager) FilesProgress(id store.RestoreID) (FilesProgress, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.progress[id]
	if !ok {
		return FilesProgress{}, false
	}
	return p.snapshot(), true
}

type progress struct {
	m  *Manager
	id store.RestoreID
	p  FilesProgress
}

func (p *progress) snapshot() FilesProgress {
	out := p.p
	out.Files = append([]FileProgress(nil), p.p.Files...)
	out.Lost = append([]string(nil), p.p.Lost...)
	return out
}

// track registers the progress of job id, with every planned file waiting.
func (m *Manager) track(id store.RestoreID, plan FilesPlan) *progress {
	pr := &progress{m: m, id: id}
	pr.p.NotStored = plan.NotStored
	for _, f := range plan.Files {
		pr.p.Files = append(pr.p.Files, FileProgress{PlannedFile: f, State: FileWaiting})
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.progress == nil {
		m.progress = map[store.RestoreID]*progress{}
	}
	m.progress[id] = pr
	m.progressOrder = append(m.progressOrder, id)
	for len(m.progressOrder) > progressKeep {
		delete(m.progress, m.progressOrder[0])
		m.progressOrder = m.progressOrder[1:]
	}
	return pr
}

func (pr *progress) set(i int, s FileState, why string) {
	pr.m.mu.Lock()
	defer pr.m.mu.Unlock()
	pr.p.Files[i].State, pr.p.Files[i].Why = s, why
}

func (pr *progress) wrote() {
	pr.m.mu.Lock()
	defer pr.m.mu.Unlock()
	pr.p.Writes++
}

func (pr *progress) lost(name string) {
	pr.m.mu.Lock()
	defer pr.m.mu.Unlock()
	pr.p.Lost = append(pr.p.Lost, name)
}

// planFiles decides what the restore of backup id does with the backup's files. t is the device
// (bare for replacement hardware), version reads its firmware (asked only when needed). A device that cannot be asked is an
// error (the caller must not guess); everything else that stops the files is a reason in Why.
func (m *Manager) planFiles(ctx context.Context, id model.BackupID, t tasmota.Target, version func(context.Context) (string, error), viaMQTT bool) (FilesPlan, error) {
	if m.o.Files == nil {
		return FilesPlan{}, nil
	}
	arc, err := m.o.Files.OpenFilesArchive(ctx, id)
	switch {
	case errors.Is(err, backup.ErrNoFilesArchive):
		rf, rerr := m.o.Files.RunFiles(ctx, id)
		if rerr != nil {
			return FilesPlan{}, nil //nolint:nilerr // no archive and no note: the page says nothing about files
		}
		return FilesPlan{Note: rf.Note}, nil
	case errors.Is(err, backup.ErrBadArchive), errors.Is(err, backup.ErrIntegrity), errors.Is(err, backup.ErrFileMissing):
		// Nothing of a damaged archive is used. The settings are still restorable, and the page says so.
		return FilesPlan{Why: "the files archive of this backup is damaged or missing, so only the settings are restored"}, nil
	case err != nil:
		return FilesPlan{}, err
	}
	plan := FilesPlan{NotStored: arc.Manifest.Skipped, archive: arc}
	for _, f := range arc.Manifest.Files {
		plan.Files = append(plan.Files, PlannedFile{Path: f.Path, Size: f.Size, PrivateKey: f.PrivateKey})
		plan.Bytes += f.Size
	}
	if len(plan.Files) == 0 {
		return plan, nil
	}
	if viaMQTT {
		plan.Why = "files need HTTP, and this restore goes over MQTT, so only the settings are restored"
		return plan, nil
	}
	fs, ok := m.o.Client.(tasmota.FileSystem)
	if !ok {
		plan.Why = "TasmoFleet has no file access to the device, so only the settings are restored"
		return plan, nil
	}
	info, err := fs.FSInfo(ctx, t)
	if err != nil {
		return FilesPlan{}, fmt.Errorf("reading the device's file system: %w", err)
	}
	v, err := version(ctx)
	if err != nil {
		return FilesPlan{}, fmt.Errorf("reading the device's firmware version: %w", err)
	}
	if !tasmota.FilesSupported(v, info) {
		if !info.HasFlash() {
			plan.Why = "the device has no flash file system, so only the settings are restored"
		} else {
			plan.Why = "the device's firmware is older than " + tasmota.MinFilesFirmware + ", so only the settings are restored"
		}
	}
	return plan, nil
}

// filesCheck is what the verification compares with the fresh backup: the files the device holds
// in the backup's version afterwards (written or already the same).
type filesCheck struct {
	want map[string]string // device path -> SHA-256 hex of the backup's content
}

// filesOutcome is what the files step reports to the rest of the run.
type filesOutcome struct {
	plan    FilesPlan
	summary string // the sentence for the finished job; empty when there is nothing to say
	check   *filesCheck
}

// failedFiles is an error of the files step with the failure text to store.
type failedFiles struct{ text string }

func (e *failedFiles) Error() string { return e.text }

func display(p string) string { return strings.TrimPrefix(p, "/") }

// reachedDevice reports whether an upload error leaves the file untouched: the client checked or
// asked before it sent a body.
func untouched(err error) bool {
	return errors.Is(err, tasmota.ErrBadPath) || errors.Is(err, tasmota.ErrFolderNotFound) || errors.Is(err, tasmota.ErrNotAFolder)
}

// sizeOnDevice lists the file's folder and returns its size (the device says "Successful" after a
// short write, so every upload is checked).
func sizeOnDevice(ctx context.Context, fs tasmota.FileSystem, t tasmota.Target, p string) (int64, error) {
	dir, name := path.Split(p)
	if dir != "/" {
		dir = strings.TrimSuffix(dir, "/")
	}
	list, err := fs.ListFiles(ctx, t, dir)
	if err != nil && !errors.Is(err, tasmota.ErrNamesSkipped) {
		return 0, err
	}
	for _, e := range list {
		if e.Name == name && !e.Dir {
			return e.Size, nil
		}
	}
	return 0, tasmota.ErrFileNotFound
}

// writeFiles puts the plan's files on the device, one after the other (see the package comment of
// this file). It returns what happened, or a *failedFiles whose text ends the restore with the
// settings unchanged.
func (m *Manager) writeFiles(ctx context.Context, j *Job, t tasmota.Target, plan FilesPlan) (*filesOutcome, error) {
	fs := m.o.Client.(tasmota.FileSystem) // planFiles only plans files for a client that has it
	pr := m.track(j.ID, plan)
	out := &filesOutcome{plan: plan, check: &filesCheck{want: map[string]string{}}}
	total := len(plan.Files)
	fail := func(i int, state FileState, why string, prefix, text string) (*filesOutcome, error) {
		pr.set(i, state, why)
		return nil, &failedFiles{prefix + text}
	}
	for i, f := range plan.Files {
		if err := ctx.Err(); err != nil {
			return fail(i, FileWaiting, "", failFilesNone, err.Error())
		}
		m.step(ctx, j, store.RestoreFiles, fmt.Sprintf("writing %s (%d of %d)", display(f.Path), i+1, total))
		pr.set(i, FileWriting, "")
		data, err := plan.archive.Read(f.Path)
		if err != nil {
			return fail(i, FileFailed, "", m.filesPrefix(pr), display(f.Path)+": "+err.Error())
		}
		// Keep the device's current file: the upload deletes it first, and a failed upload must not
		// cost the person a file. A file that cannot be read cannot be put back, so none is written.
		old, err := fs.DownloadFile(ctx, t, f.Path, backup.MaxFileBytes)
		hasOld := err == nil
		switch {
		case err == nil:
			if bytes.Equal(old, data) {
				pr.set(i, FileSame, "")
				out.check.want[f.Path] = shaOf(data)
				continue
			}
		case errors.Is(err, tasmota.ErrFileNotFound):
		case errors.Is(err, tasmota.ErrNotAFile):
			pr.set(i, FileSkipped, "a folder of this name exists on the device")
			continue
		default:
			return fail(i, FileFailed, "", m.filesPrefix(pr), display(f.Path)+" on the device could not be read first, so it was not overwritten: "+err.Error())
		}

		err = fs.UploadFile(ctx, t, f.Path, data)
		if !untouched(err) {
			pr.wrote()
		}
		if errors.Is(err, tasmota.ErrFolderNotFound) || errors.Is(err, tasmota.ErrNotAFolder) {
			pr.set(i, FileSkipped, "folder missing on the device; create it in the device's file manager")
			continue
		}
		if err == nil {
			var n int64
			if n, err = sizeOnDevice(ctx, fs, t, f.Path); err == nil && n != int64(len(data)) {
				err = fmt.Errorf("the device holds %d bytes after the upload, not %d", n, len(data))
			}
		}
		if err != nil {
			why := m.putBack(ctx, fs, t, pr, f.Path, old, hasOld)
			pr.set(i, FileFailed, "")
			return nil, &failedFiles{failFilesPartly + display(f.Path) + ": " + err.Error() + "; " + why}
		}
		pr.set(i, FileWritten, "")
		out.check.want[f.Path] = shaOf(data)
	}
	snap := pr.snapshot()
	out.summary = filesSummary(snap)
	return out, nil
}

// filesPrefix is the failure prefix for a failure that happens before a file is touched: "unchanged"
// as long as nothing was written, "partly written" after.
func (m *Manager) filesPrefix(pr *progress) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if pr.p.Writes > 0 {
		return failFilesPartly
	}
	return failFilesNone
}

// putBack writes the old content of a file whose upload failed back, once, and says how it went.
func (m *Manager) putBack(ctx context.Context, fs tasmota.FileSystem, t tasmota.Target, pr *progress, p string, old []byte, hasOld bool) string {
	if !hasOld {
		return display(p) + " did not exist before and may now be partly written"
	}
	err := fs.UploadFile(ctx, t, p, old)
	if !untouched(err) {
		pr.wrote()
	}
	if err == nil {
		var n int64
		if n, err = sizeOnDevice(ctx, fs, t, p); err == nil && n != int64(len(old)) {
			err = errors.New("size differs after the put-back")
		}
	}
	if err != nil {
		pr.lost(display(p))
		return "FILE LOST: " + display(p) + " could not be put back and is lost on the device; restore the automatic backup taken before this restore to get it back"
	}
	return display(p) + " was put back as it was"
}

func shaOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// filesSummary is the sentence the finished job keeps for the files step.
func filesSummary(p FilesProgress) string {
	w, same, skipped := p.Count(FileWritten), p.Count(FileSame), p.Count(FileSkipped)
	parts := []string{fmt.Sprintf("%s written (%s)", plural(w, "file", "files"), plural(p.Writes, "flash write", "flash writes"))}
	if same > 0 {
		parts = append(parts, fmt.Sprintf("%d already the same", same))
	}
	if skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", skipped))
	}
	return strings.Join(parts, ", ")
}

// notWrittenSummary is the files part of a finished job that wrote no file although the backup holds some.
func notWrittenSummary(plan FilesPlan) string {
	switch {
	case plan.Why != "":
		return "Files not restored: " + plan.Why
	case plan.Note != "":
		return "This backup holds no files (" + plan.Note + ")"
	}
	return ""
}

// verifyFiles compares the files a fresh backup read from the device with what the restore put
// there. A file the device rewrote itself (Zigbee data) shows up as a difference, named, never its content.
func (m *Manager) verifyFiles(ctx context.Context, fresh []model.Backup, fc *filesCheck) (string, bool) {
	if len(fc.want) == 0 {
		return "", true
	}
	var row *model.Backup
	for i := range fresh {
		if fresh[i].Kind == model.BackupFiles {
			row = &fresh[i]
		}
	}
	if row == nil || m.o.Files == nil {
		return "the files could not be compared (the fresh backup holds no files)", false
	}
	arc, err := m.o.Files.OpenFilesArchive(ctx, row.ID)
	if err != nil {
		return "the files could not be compared (the fresh backup's files could not be read)", false
	}
	have := map[string]string{}
	for _, f := range arc.Manifest.Files {
		have[f.Path] = f.SHA256
	}
	var diff []string
	for p, sum := range fc.want {
		if have[p] != sum {
			diff = append(diff, display(p))
		}
	}
	slices.Sort(diff)
	if len(diff) == 0 {
		return fmt.Sprintf("the %s match the backup", plural(len(fc.want), "file", "files")), true
	}
	return fmt.Sprintf("%s differ from the backup: %s", plural(len(diff), "file", "files"), strings.Join(diff, ", ")), false
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// filesStep is the whole files step of a restore: plan, check the identity, write. fo is never nil
// on success; a non-nil error is the failure text that ends the job.
func (m *Manager) filesStep(ctx context.Context, j *Job, t tasmota.Target, version func(context.Context) (string, error), viaMQTT bool, identity func() error) (*filesOutcome, error) {
	plan, err := m.planFiles(ctx, j.BackupID, t, version, viaMQTT)
	if err != nil {
		return nil, &failedFiles{failFilesNone + err.Error()}
	}
	if !plan.Restores() {
		return &filesOutcome{plan: plan, summary: notWrittenSummary(plan)}, nil
	}
	m.step(ctx, j, store.RestoreFiles, "checking the device before writing files")
	// The address may have changed hands since Start: the files of this device must never go to another one.
	if err := identity(); err != nil {
		return nil, &failedFiles{failFilesNone + err.Error()}
	}
	return m.writeFiles(ctx, j, t, plan)
}

// withSummary puts the files part in front of the verification sentence (see SplitDetail).
func (fo *filesOutcome) withSummary(detail string) string {
	if fo == nil || fo.summary == "" {
		return detail
	}
	return fo.summary + detailSep + detail
}

// FilesWritten reports whether the files part of a finished job's Detail says that files were
// written (the page then words the result as settings and files).
func FilesWritten(detail string) bool {
	f, _ := SplitDetail(detail)
	return strings.Contains(f, " written (") && !strings.HasPrefix(f, "0 files written")
}

// dumpFiles words, for the replace page's choice of backup, what each dump holds of the device's files.
func (m *Manager) dumpFiles(ctx context.Context, dumps []model.Backup) map[model.BackupID]string {
	if m.o.Files == nil {
		return nil
	}
	out := map[model.BackupID]string{}
	for i, b := range dumps {
		if i == 20 { // each one parses an archive; the newest are the ones people pick
			break
		}
		rf, err := m.o.Files.RunFiles(ctx, b.ID)
		switch {
		case err != nil:
			out[b.ID] = "files unreadable"
		case rf.Archive != nil:
			out[b.ID] = plural(len(rf.Manifest.Files), "file", "files")
		case rf.Note == "they need HTTP":
			out[b.ID] = "no files (went over MQTT)"
		case rf.Note != "":
			out[b.ID] = "no files (" + rf.Note + ")"
		}
	}
	return out
}
