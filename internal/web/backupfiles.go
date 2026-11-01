package web

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// BackupFiles is the read side of the files step (#324): what a run holds of the device's
// flash files (*backup.Manager implements it). Deps.Backups is asserted against it like
// BackupKeeper; without it the Backups tab shows no Files folds.
type BackupFiles interface {
	RunFiles(ctx context.Context, id model.BackupID) (backup.RunFiles, error)
}

func (h *handler) backupFiles() (BackupFiles, bool) {
	f, ok := h.d.Backups.(BackupFiles)
	return f, ok
}

// filesView is the Files fold of one run row.
type filesView struct {
	Meta     string // summary: "6 stored · 21.0 KiB · 1 skipped"
	Rows     []fileRow
	Problem  string // the archive cannot be read: this replaces the list
	Note     string // the filesystem line and the secrecy sentence
	Download string // admins: the .tar; "" otherwise
}

type fileRow struct {
	Path, Size, Why string
	Skipped         bool
	Tags            []fileTag
}

type fileTag struct {
	Tone        ui.Tone
	Text, Title string
}

// runFilesInfo is what the Backups tab shows of a run's files besides the row itself.
type runFilesInfo struct {
	View *filesView // nil: no fold
	Why  string     // the detail line's reason when the run has no archive and its dump note says nothing
}

const (
	whyNoTarNoNote = "no files were stored for this run"
	privateKeyHelp = "TLS private key: the content is never shown, the archive is admin only"
)

// filesWhyText words the reason a run has no files for the row's detail line. reason is the
// sentence after backup.NoteFilesPrefix.
func filesWhyText(reason string) string {
	switch {
	case reason == "":
		return ""
	case reason == "no filesystem":
		return "settings only: this build has no filesystem"
	case reason == "they need HTTP":
		return "files not backed up: they need HTTP, this run went over MQTT"
	case strings.HasPrefix(reason, "firmware older than "):
		return "settings only: " + reason + " has no file access"
	}
	return backup.NoteFilesPrefix + reason
}

// knownFileHints say what Tasmota's reserved file names hold.
var knownFileHints = map[string]string{"zb": "Zigbee device data", "zbv4": "Zigbee device data", "zbdata": "Zigbee device data"}

// runFilesInfos reads the files of every run, oldest first, so that each run is compared with
// the newest earlier run that has files (a run without files, e.g. taken over MQTT, or with a
// damaged archive, is not a base). A run's problem never fails the page.
func (h *handler) runFilesInfos(ctx context.Context, runs []backupRun, base string, admin bool) map[model.BackupID]runFilesInfo {
	fs, ok := h.backupFiles()
	if !ok {
		return nil
	}
	order := append([]backupRun(nil), runs...)
	sort.SliceStable(order, func(i, j int) bool { return order[i].TakenAt.Before(order[j].TakenAt) })
	out := make(map[model.BackupID]runFilesInfo, len(order))
	var prev *backup.Manifest
	sawArchive := false // an earlier run of this device has files: the files step exists for it
	for _, run := range order {
		rf, err := fs.RunFiles(ctx, run.ID)
		switch {
		case err != nil:
			sawArchive = true // a damaged archive is still an archive
			out[run.ID] = runFilesInfo{View: h.filesProblem(ctx, run.ID, err)}
		case rf.Archive != nil && rf.Manifest != nil:
			sawArchive = true
			v := buildFilesView(*rf.Manifest, prev)
			if admin {
				v.Download = fmt.Sprintf("%s/backups/%d", base, rf.Archive.ID)
			}
			out[run.ID] = runFilesInfo{View: v}
			prev = rf.Manifest
		case rf.Note == "" && sawArchive:
			// A crash between the dump row and the archive row leaves neither: not "before the
			// files step" once an earlier run has files.
			out[run.ID] = runFilesInfo{Why: whyNoTarNoNote}
		}
	}
	return out
}

// filesProblem is the fold of a run whose archive cannot be read.
func (h *handler) filesProblem(ctx context.Context, id model.BackupID, err error) *filesView {
	v := &filesView{}
	switch {
	case errors.Is(err, backup.ErrBadArchive), errors.Is(err, backup.ErrIntegrity):
		v.Meta = "archive damaged"
		v.Problem = "The files archive of this run is damaged and cannot be used or downloaded. The settings backup of the run is not affected."
	case errors.Is(err, backup.ErrFileMissing):
		v.Meta = "archive file missing"
		v.Problem = "The files archive is gone from the data directory (deleted by hand?). Delete the backup to remove it; the settings backup of the run is not affected."
	default:
		h.log.WarnContext(ctx, "backups: files of a run could not be read", "backup", int64(id), "err", err)
		v.Meta = "archive could not be read"
		v.Problem = "The files archive of this run could not be read. The settings backup of the run is not affected."
	}
	return v
}

// buildFilesView lists a manifest: stored files first, then the skipped ones. prev is the
// manifest of the newest earlier run that has files, nil for the first one.
func buildFilesView(m backup.Manifest, prev *backup.Manifest) *filesView {
	before := map[string]string{}
	if prev != nil {
		for _, f := range prev.Files {
			before[f.Path] = f.SHA256
		}
	}
	v := &filesView{}
	var total int64
	for _, f := range m.Files {
		total += f.Size
		row := fileRow{Path: strings.TrimPrefix(f.Path, "/"), Size: humanBytes(f.Size), Why: knownFileHints[strings.TrimPrefix(f.Path, "/")]}
		if prev != nil {
			if old, seen := before[f.Path]; !seen {
				row.Tags = append(row.Tags, fileTag{Tone: ui.ToneAccent, Text: "New"})
			} else if old != f.SHA256 {
				row.Tags = append(row.Tags, fileTag{Tone: ui.ToneAccent, Text: "Changed"})
			}
		}
		if f.PrivateKey || backup.IsPrivateKey(f.Path) {
			row.Tags = append(row.Tags, fileTag{Tone: ui.ToneWarn, Text: "Private key", Title: privateKeyHelp})
		}
		v.Rows = append(v.Rows, row)
	}
	for _, s := range m.Skipped {
		row := fileRow{Path: strings.TrimPrefix(s.Path, "/"), Size: humanBytes(s.Size), Skipped: true, Why: skipWhy(s.Reason),
			Tags: []fileTag{{Tone: ui.ToneNeutral, Text: "Skipped"}}}
		if s.PrivateKey || backup.IsPrivateKey(s.Path) {
			row.Tags = append(row.Tags, fileTag{Tone: ui.ToneWarn, Text: "Private key", Title: privateKeyHelp})
		}
		v.Rows = append(v.Rows, row)
	}
	v.Meta = fmt.Sprintf("%d stored · %s", len(m.Files), humanBytes(total))
	if n := len(m.Skipped); n > 0 {
		v.Meta += fmt.Sprintf(" · %d skipped", n)
	}
	v.Note = fmt.Sprintf("Flash filesystem (%s, %d KiB, %d KiB free), read over HTTP. Contents are never shown here: Berry scripts often hold tokens. The archive is as secret as the settings dump.",
		fsTypeName(m.FSType), m.FSSizeKB, m.FSFreeKB)
	return v
}

func skipWhy(reason string) string {
	if reason == "larger than 1 MiB" {
		return "Over 1 MiB: listed, not stored"
	}
	if reason == "" {
		return "Listed, not stored"
	}
	return "Listed, not stored: " + reason
}

// fsTypeName names Tasmota's UfsType for the flash file systems.
func fsTypeName(t int) string {
	switch t {
	case 2:
		return "FFat"
	case 3:
		return "LittleFS"
	}
	return "file system"
}
