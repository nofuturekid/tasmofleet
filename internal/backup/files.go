package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// Limits of the files step (spec 12.1). A file over a limit is listed in the
// manifest and not stored.
const (
	MaxFileBytes int64 = 1 << 20 // per file
	MaxRunBytes  int64 = 4 << 20 // stored bytes per run
	MaxFiles           = 100     // files read per run
	MaxFolders         = 100     // folders listed per run
	MaxDepth           = 4       // folder depth below the root
)

// ManifestName is the first entry of every files archive. Device dot files are
// never listed, so no device file can carry this name.
const ManifestName = ".tasmofleet-manifest.json"

// NoteFilesPrefix starts the sentence a run's dump note carries when the run
// has no files archive; the rest says why. See FilesNoteOf.
const NoteFilesPrefix = "files not backed up: "

// ErrNoFilesArchive: the run has no files archive (see RunFiles.Note for why).
var ErrNoFilesArchive = errors.New("backup: the run has no files archive")

// ErrBadArchive: a files archive does not match its manifest or breaks the
// path rules (a hand-edited or damaged file). Nothing from it may be used.
var ErrBadArchive = errors.New("backup: files archive is damaged")

// Manifest is the first entry of a files archive. Paths are device paths
// ("/lib/x.be"); the tar entries are the same without the leading slash.
type Manifest struct {
	FSType   int            `json:"fs_type"`
	FSSizeKB int            `json:"fs_size_kb"`
	FSFreeKB int            `json:"fs_free_kb"`
	Files    []ManifestFile `json:"files"`   // stored in the archive
	Skipped  []ManifestSkip `json:"skipped"` // seen on the device, not stored
}

// ManifestFile is a stored file.
type ManifestFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Stored bool   `json:"stored"` // always true here; kept for readers of the raw manifest
	// ModTime is the device's wall clock ("2006-01-02T15:04:05", no zone), empty if unknown.
	ModTime string `json:"mod_time,omitempty"`
	// PrivateKey marks /tlskey: the UI says "private key" and never shows content.
	PrivateKey bool `json:"private_key,omitempty"`
}

// ManifestSkip is a file or folder that is listed but not in the archive.
type ManifestSkip struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	Reason     string `json:"reason"`
	PrivateKey bool   `json:"private_key,omitempty"`
}

// FilesArchive is a parsed, verified files archive.
type FilesArchive struct {
	Backup   model.Backup // the row of the archive
	Manifest Manifest
	contents map[string][]byte
}

// RunFiles describes what a run holds of the device's files.
type RunFiles struct {
	// Archive is the run's files row; nil when the run has none.
	Archive *model.Backup
	// Manifest is the parsed manifest of Archive; nil without an archive.
	Manifest *Manifest
	// Note says why a run has no archive ("no filesystem", "they need HTTP",
	// "firmware older than 14.6.0", ...), without NoteFilesPrefix; "" when
	// there is an archive or the run predates the files step.
	Note string
}

// privateKeyPath is the TLS private key Tasmota keeps in the root of its file system.
const privateKeyPath = "/tlskey"

// IsPrivateKey reports whether the device path is the TLS private key.
func IsPrivateKey(devicePath string) bool { return devicePath == privateKeyPath }

// FilesNoteOf extracts the reason a run has no files archive from its dump
// note (the sentence after NoteFilesPrefix, always the last part of the note);
// "" if the note has none.
func FilesNoteOf(dumpNote string) string {
	i := strings.LastIndex(dumpNote, NoteFilesPrefix)
	if i < 0 {
		return ""
	}
	return dumpNote[i+len(NoteFilesPrefix):]
}

// Reasons shown in the manifest and in notes. They are fixed sentences: error
// text can carry device URLs, and a URL can carry the web password.
const (
	whyTooLarge   = "larger than 1 MiB"
	whyRunCap     = "run limit of 4 MiB reached"
	whyCount      = "more than 100 files"
	whyFolders    = "more than 100 folders"
	whyDepth      = "deeper than 4 folders"
	whyName       = "name not accepted"
	whyGone       = "file no longer on the device"
	whyNotAFile   = "not a file"
	whyFailed     = "could not be read"
	whyList       = "folder could not be listed"
	whyDeviceGone = "not read: the device did not answer"
	whyTimeout    = "not read: the device took too long to answer"
	whyAuth       = "not read: the device rejected the web password"
	whyOutside    = "not read: the device address is outside the configured device networks"
	whyTimeLimit  = "not read: time limit of the backup reached"
)

// reasonOf chooses the sentence for a failed call from the error's kind.
func reasonOf(err error) string {
	switch {
	case errors.Is(err, transport.ErrFilesNeedHTTP):
		return "they need HTTP"
	case errors.Is(err, tasmota.ErrAuth):
		return "the device rejected the web password"
	case errors.Is(err, tasmota.ErrUnreachable):
		return "the device did not answer"
	case errors.Is(err, tasmota.ErrTimeout):
		return "the device took too long to answer"
	case errors.Is(err, tasmota.ErrForbiddenAddr):
		return "the device address is outside the configured device networks"
	case errors.Is(err, tasmota.ErrNotImplemented):
		return "this connection cannot read files"
	}
	return "the file system could not be read"
}

// walker reads the device's file system into a manifest and file contents.
type walker struct {
	parent  context.Context // the caller's: only its error aborts the run
	ctx     context.Context // the walk's own, ending earlier
	fs      tasmota.FileSystem
	t       tasmota.Target
	man     Manifest
	data    map[string][]byte
	total   int64
	reads   int
	folders int
	rootErr error  // the root could not be listed: there is nothing to describe
	dead    string // set once the device stopped answering: the reason for everything left
}

func (w *walker) skip(p string, size int64, reason string) {
	w.man.Skipped = append(w.man.Skipped, ManifestSkip{Path: p, Size: size, Reason: reason, PrivateKey: IsPrivateKey(p)})
}

// dir lists one folder and handles its entries in name order. Only a context
// error is returned: everything else is recorded in the manifest.
func (w *walker) dir(dir string, depth int) error {
	w.folders++
	entries, err := w.fs.ListFiles(w.ctx, w.t, dir)
	var sk *tasmota.SkippedNamesError
	switch {
	case errors.As(err, &sk):
		for _, n := range sk.Names {
			w.skip(join(dir, cleanName(n)), 0, whyName)
		}
	case err != nil:
		if cerr := w.parent.Err(); cerr != nil {
			return cerr
		}
		w.noteDead(err)
		if dir == "/" {
			w.rootErr = err
			return nil
		}
		w.skip(dir, 0, whyList)
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	seen := map[string]bool{}
	for _, e := range entries {
		if seen[e.Name] { // a repeated name would give the tar two entries of one path
			continue
		}
		seen[e.Name] = true
		p := join(dir, e.Name)
		var err error
		switch {
		case e.Dir:
			err = w.folder(p, depth+1)
		default:
			err = w.file(p, e)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *walker) folder(p string, depth int) error {
	switch {
	case w.dead != "":
		w.skip(p, 0, w.dead)
	case depth > MaxDepth:
		w.skip(p, 0, whyDepth)
	case w.folders >= MaxFolders:
		w.skip(p, 0, whyFolders)
	default:
		return w.dir(p, depth)
	}
	return nil
}

func (w *walker) file(p string, e tasmota.FileEntry) error {
	switch {
	case w.dead != "":
		w.skip(p, e.Size, w.dead)
		return nil
	case e.Size > MaxFileBytes:
		w.skip(p, e.Size, whyTooLarge)
		return nil
	case w.reads >= MaxFiles:
		w.skip(p, e.Size, whyCount)
		return nil
	case w.total+e.Size > MaxRunBytes:
		w.skip(p, e.Size, whyRunCap)
		return nil
	}
	w.reads++
	// The limit of this download is what the run may still store: a file that grew since the
	// listing is refused by its Content-Length before a body byte is read.
	limit, runBound := MaxFileBytes, false
	if left := MaxRunBytes - w.total; left < limit {
		limit, runBound = left, true
	}
	if limit <= 0 { // nothing left (a client treats max <= 0 as "its default")
		w.skip(p, e.Size, whyRunCap)
		return nil
	}
	b, err := w.fs.DownloadFile(w.ctx, w.t, p, limit)
	if err != nil {
		if cerr := w.parent.Err(); cerr != nil {
			return cerr
		}
		w.noteDead(err)
		reason := downloadReason(err, w.dead)
		if errors.Is(err, tasmota.ErrFileTooLarge) && runBound {
			reason = whyRunCap
		}
		w.skip(p, e.Size, reason)
		return nil
	}
	if w.total+int64(len(b)) > MaxRunBytes { // the client let more through than asked for
		w.skip(p, int64(len(b)), whyRunCap)
		return nil
	}
	sum := sha256.Sum256(b)
	mt := ""
	if !e.ModTime.IsZero() {
		mt = e.ModTime.Format(modTimeLayout)
	}
	w.man.Files = append(w.man.Files, ManifestFile{Path: p, Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:]), Stored: true, ModTime: mt, PrivateKey: IsPrivateKey(p)})
	w.data[p] = b
	w.total += int64(len(b))
	return nil
}

const modTimeLayout = "2006-01-02T15:04:05"

// noteDead remembers that the device stopped answering, so the rest of the
// walk sends nothing more: every further call would wait for its own timeout.
func (w *walker) noteDead(err error) {
	if w.ctx.Err() != nil { // the walk's own deadline: the call was cut off by us, not by the device
		w.dead = whyTimeLimit
		return
	}
	switch {
	case errors.Is(err, tasmota.ErrForbiddenAddr):
		w.dead = whyOutside
	case errors.Is(err, tasmota.ErrAuth):
		w.dead = whyAuth
	case errors.Is(err, tasmota.ErrUnreachable):
		w.dead = whyDeviceGone
	case errors.Is(err, tasmota.ErrTimeout):
		w.dead = whyTimeout
	}
}

func downloadReason(err error, dead string) string {
	switch {
	case dead != "":
		return strings.TrimPrefix(dead, "not read: ")
	case errors.Is(err, tasmota.ErrFileNotFound):
		return whyGone
	case errors.Is(err, tasmota.ErrNotAFile):
		return whyNotAFile
	case errors.Is(err, tasmota.ErrFileTooLarge):
		return whyTooLarge
	case errors.Is(err, tasmota.ErrBadPath):
		return whyName
	}
	return whyFailed
}

func join(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}

// cleanName makes a name the client refused safe to put into a manifest:
// control characters become "?" and it is cut at 100 bytes.
func cleanName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
	if len(s) > 100 {
		s = strings.ToValidUTF8(s[:100], "?")
	}
	return s
}

// filesResult is what the files step hands to the backup run.
type filesResult struct {
	archive     []byte // nil: no archive
	archiveNote string // Note of the archive row
	runNote     string // sentence for the dump note when there is no archive
}

func notBacked(reason string) filesResult { return filesResult{runNote: NoteFilesPrefix + reason} }

// filesStep reads the device's flash files (best effort: only a cancelled
// context is an error). The caller holds the device lock; the client takes
// its own per-device lock for every call.
func (m *Manager) filesStep(ctx context.Context, t tasmota.Target, fwVersion string, at time.Time) (filesResult, error) {
	fs, ok := m.o.Client.(tasmota.FileSystem)
	if !ok {
		return filesResult{}, nil // a client without file access (tests with fake devices)
	}
	if transport.MQTTOnly(ctx) {
		return notBacked("they need HTTP"), nil
	}
	// The walk ends a few seconds before the caller's deadline (the pre-OTA backup has 30 s for
	// everything), so a stalling device costs the files, never the dump.
	wctx, cancel := walkContext(ctx)
	defer cancel()
	info, err := fs.FSInfo(wctx, t)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return filesResult{}, cerr
		}
		why := reasonOf(err)
		if wctx.Err() != nil {
			why = strings.TrimPrefix(whyTimeLimit, "not read: ")
		}
		m.o.Log.Info("backup: files not read", "component", "backup", "device", int64(t.DeviceID()), "reason", why)
		return notBacked(why), nil
	}
	if !tasmota.FilesSupported(fwVersion, info) {
		if !info.HasFlash() {
			return notBacked("no filesystem"), nil
		}
		return notBacked("firmware older than " + tasmota.MinFilesFirmware), nil
	}

	w := &walker{parent: ctx, ctx: wctx, fs: fs, t: t, data: map[string][]byte{},
		man: Manifest{FSType: info.Type, FSSizeKB: info.SizeKB, FSFreeKB: info.FreeKB, Files: []ManifestFile{}, Skipped: []ManifestSkip{}}}
	if err := w.dir("/", 0); err != nil {
		return filesResult{}, err
	}
	if w.rootErr != nil {
		why := reasonOf(w.rootErr)
		if w.dead == whyTimeLimit {
			why = strings.TrimPrefix(whyTimeLimit, "not read: ")
		}
		m.o.Log.Info("backup: files not read", "component", "backup", "device", int64(t.DeviceID()), "reason", why)
		return notBacked(why), nil //nolint:nilerr // files are best effort: the reason goes into the run note, the run goes on
	}
	sort.Slice(w.man.Files, func(i, j int) bool { return w.man.Files[i].Path < w.man.Files[j].Path })
	sort.Slice(w.man.Skipped, func(i, j int) bool { return w.man.Skipped[i].Path < w.man.Skipped[j].Path })
	arc, err := writeArchive(w.man, w.data, at)
	if err != nil {
		m.o.Log.Error("backup: building the files archive failed", "component", "backup", "device", int64(t.DeviceID()), "err", err)
		return notBacked("the archive could not be built"), nil
	}
	res := filesResult{archive: arc}
	if n := len(w.man.Skipped); n > 0 {
		res.archiveNote = fmt.Sprintf("files incomplete: %d %s not stored", n, plural(n, "file", "files"))
	}
	m.o.Log.Debug("backup: files read", "component", "backup", "device", int64(t.DeviceID()), "stored", len(w.man.Files), "skipped", len(w.man.Skipped), "bytes", w.total)
	return res, nil
}

// walkMargin is how long before the caller's deadline the files walk ends, so the dump and the
// snapshot already read can still be stored.
const walkMargin = 5 * time.Second

// walkContext returns ctx limited to end walkMargin (at most half of the time left) before its
// deadline; without a deadline ctx itself.
func walkContext(ctx context.Context) (context.Context, context.CancelFunc) {
	dl, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	margin := min(walkMargin, time.Until(dl)/2)
	return context.WithDeadline(ctx, dl.Add(-margin))
}

// WithoutFilesNote removes the files sentence (always last) from a dump note, for views that
// do not show it yet.
func WithoutFilesNote(note string) string {
	i := strings.LastIndex(note, NoteFilesPrefix)
	if i < 0 {
		return note
	}
	return strings.TrimSuffix(strings.TrimSpace(note[:i]), ";")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// writeArchive builds the tar: the manifest first, then the files by path.
func writeArchive(man Manifest, data map[string][]byte, at time.Time) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mb, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := putEntry(tw, ManifestName, mb, at); err != nil {
		return nil, err
	}
	for _, f := range man.Files {
		mt := at
		if f.ModTime != "" {
			if t, err := time.Parse(modTimeLayout, f.ModTime); err == nil {
				mt = t
			}
		}
		if err := putEntry(tw, strings.TrimPrefix(f.Path, "/"), data[f.Path], mt); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func putEntry(tw *tar.Writer, name string, b []byte, mt time.Time) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(b)), ModTime: mt.UTC()}); err != nil {
		return err
	}
	_, err := tw.Write(b)
	return err
}

// ParseFilesArchive verifies and indexes a files archive. It is strict because
// a restore writes what it returns to a device: the manifest must be the first
// entry, every manifest path must obey tasmota.CheckFSPath, every stored file
// must be a regular entry of the right size and SHA-256, and the archive may
// hold nothing the manifest does not list. Violations are ErrBadArchive.
func ParseFilesArchive(data []byte) (*FilesArchive, error) {
	tr := tar.NewReader(bytes.NewReader(data))
	hd, err := tr.Next()
	if err != nil || hd.Name != ManifestName || hd.Typeflag != tar.TypeReg {
		return nil, badArchive("the manifest is not the first entry")
	}
	mb, err := io.ReadAll(io.LimitReader(tr, 1<<20+1))
	if err != nil || len(mb) > 1<<20 {
		return nil, badArchive("the manifest cannot be read")
	}
	var man Manifest
	if err := json.Unmarshal(mb, &man); err != nil {
		return nil, badArchive("the manifest is not valid JSON")
	}
	listed := map[string]int{}
	for i := range man.Files {
		f := &man.Files[i]
		if err := tasmota.CheckFSPath(f.Path); err != nil {
			return nil, badArchive("a manifest path is not allowed")
		}
		if _, dup := listed[f.Path]; dup {
			return nil, badArchive("a path is listed twice")
		}
		if f.Size < 0 || f.Size > MaxFileBytes {
			return nil, badArchive("a file is larger than the limit")
		}
		f.PrivateKey = IsPrivateKey(f.Path)
		f.Stored = true
		listed[f.Path] = i
	}
	for i := range man.Skipped {
		man.Skipped[i].PrivateKey = IsPrivateKey(man.Skipped[i].Path)
	}
	contents := map[string][]byte{}
	for {
		hd, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, badArchive("the tar cannot be read")
		}
		p := "/" + hd.Name
		i, ok := listed[p]
		if !ok || hd.Typeflag != tar.TypeReg {
			return nil, badArchive("an entry is not in the manifest or not a regular file")
		}
		if _, dup := contents[p]; dup {
			return nil, badArchive("an entry appears twice")
		}
		b, err := io.ReadAll(io.LimitReader(tr, MaxFileBytes+1))
		if err != nil || int64(len(b)) != man.Files[i].Size {
			return nil, badArchive("a file does not have the size the manifest says")
		}
		if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != man.Files[i].SHA256 {
			return nil, badArchive("a file does not match its SHA-256")
		}
		contents[p] = b
	}
	if len(contents) != len(man.Files) {
		return nil, badArchive("a listed file is missing")
	}
	return &FilesArchive{Manifest: man, contents: contents}, nil
}

func badArchive(why string) error { return fmt.Errorf("%w: %s", ErrBadArchive, why) }

// Read returns the content of a stored file by device path ("/lib/x.be").
func (a *FilesArchive) Read(devicePath string) ([]byte, error) {
	b, ok := a.contents[devicePath]
	if !ok {
		return nil, fmt.Errorf("backup: %q is not stored in the archive", devicePath)
	}
	return append([]byte(nil), b...), nil
}

// RunFiles reports the files part of the run id belongs to: the archive row
// and manifest, or why the run has none. Any file of the run (dump, snapshot,
// archive) names it.
func (m *Manager) RunFiles(ctx context.Context, id model.BackupID) (RunFiles, error) {
	row, files, err := m.runFilesRow(ctx, id)
	if err != nil {
		return RunFiles{}, err
	}
	if row == nil {
		rf := RunFiles{}
		for _, b := range files {
			if b.Kind == model.BackupConfigDump {
				rf.Note = FilesNoteOf(b.Note)
			}
		}
		return rf, nil
	}
	a, err := m.parseRow(ctx, *row)
	if err != nil {
		return RunFiles{}, err
	}
	return RunFiles{Archive: &a.Backup, Manifest: &a.Manifest}, nil
}

// OpenFilesArchive returns the verified files archive of the run id belongs
// to (ErrNoFilesArchive if it has none; ErrIntegrity if the file changed on
// disk; ErrBadArchive if it does not parse). The contents are secret: do not
// log them.
func (m *Manager) OpenFilesArchive(ctx context.Context, id model.BackupID) (*FilesArchive, error) {
	row, _, err := m.runFilesRow(ctx, id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, ErrNoFilesArchive
	}
	return m.parseRow(ctx, *row)
}

func (m *Manager) parseRow(ctx context.Context, row model.Backup) (*FilesArchive, error) {
	data, b, err := m.Open(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	a, err := ParseFilesArchive(data)
	if err != nil {
		return nil, err
	}
	a.Backup = b
	return a, nil
}

// runFilesRow finds the files row among the files of id's run.
func (m *Manager) runFilesRow(ctx context.Context, id model.BackupID) (*model.Backup, []model.Backup, error) {
	b, err := m.o.Store.GetBackup(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	all, err := m.ownerFiles(ctx, b)
	if err != nil {
		return nil, nil, err
	}
	files := runsOf(all, b.TakenAt)
	for i := range files {
		if files[i].Kind == model.BackupFiles {
			return &files[i], files, nil
		}
	}
	return nil, files, nil
}
