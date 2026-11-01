package web

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// filesBackups is the backup service with the read API of the files step (#324).
type filesBackups struct {
	*fakeBackups
	runs  map[model.BackupID]filesAnswer // by the run's dump id
	calls int
}

type filesAnswer struct {
	rf  backup.RunFiles
	err error
}

func (f *filesBackups) RunFiles(_ context.Context, id model.BackupID) (backup.RunFiles, error) {
	f.calls++
	a, ok := f.runs[id]
	if !ok {
		return backup.RunFiles{}, fmt.Errorf("no answer for %d", id)
	}
	return a.rf, a.err
}

func mf(path string, size int64, sha string) backup.ManifestFile {
	return backup.ManifestFile{Path: path, Size: size, SHA256: sha, Stored: true, PrivateKey: backup.IsPrivateKey(path)}
}

func withArchive(id model.BackupID, files []backup.ManifestFile, skipped ...backup.ManifestSkip) filesAnswer {
	return filesAnswer{rf: backup.RunFiles{
		Archive:  &model.Backup{ID: id, DeviceID: 1, Kind: model.BackupFiles},
		Manifest: &backup.Manifest{FSType: 3, FSSizeKB: 320, FSFreeKB: 108, Files: files, Skipped: skipped},
	}}
}

func filesEnv(t *testing.T, list []model.Backup, runs map[model.BackupID]filesAnswer) (*env, *filesBackups) {
	t.Helper()
	fb := &filesBackups{fakeBackups: &fakeBackups{list: list, data: []byte("TAR")}, runs: runs}
	return setupWith(t, func(d *Deps) { d.Backups = fb }), fb
}

var t0 = time.Date(2026, 11, 1, 2, 0, 0, 0, time.UTC)

func dump(id model.BackupID, at time.Time, note string) model.Backup {
	return model.Backup{ID: id, DeviceID: 1, Kind: model.BackupConfigDump, TakenAt: at, Version: "16.0.0", Size: 4096, Path: fmt.Sprintf("backups/1/%d.dmp", id), Note: note}
}

func archive(id model.BackupID, at time.Time) model.Backup {
	return model.Backup{ID: id, DeviceID: 1, Kind: model.BackupFiles, TakenAt: at, Size: 9000, Path: fmt.Sprintf("backups/1/%d.files.tar", id)}
}

// rowOf cuts the HTML of the backup row that has the checkbox value id.
func rowOf(t *testing.T, body string, id int) string {
	t.Helper()
	i := strings.Index(body, fmt.Sprintf(`value="%d" aria-label`, id))
	if i < 0 {
		t.Fatalf("no row %d in:\n%s", id, body)
	}
	start := strings.LastIndex(body[:i], `<div class="bk-row`)
	end := strings.Index(body[i:], `<div class="bk-row`)
	if end < 0 {
		return body[start : i+strings.Index(body[i:], "</section>")]
	}
	return body[start : i+end]
}

// Why (#324, maintainer decision): "Changed" and "New" compare with the newest EARLIER run that
// has files. A run without files (over MQTT) must be skipped: comparing with it would label every
// file of the next run "New" and hide what really changed. The archive row sorts before the dump
// here on purpose: a files row must never become the row of the run.
func TestFilesFoldComparesWithNewestEarlierRunThatHasFiles(t *testing.T) {
	list := []model.Backup{
		archive(6, t0.Add(2*time.Hour)), dump(5, t0.Add(2*time.Hour), ""),
		dump(3, t0.Add(time.Hour), backup.NoteFilesPrefix+"they need HTTP"),
		archive(2, t0), dump(1, t0, ""),
	}
	e, _ := filesEnv(t, list, map[model.BackupID]filesAnswer{
		5: withArchive(6, []backup.ManifestFile{mf("/a.be", 10, "aa"), mf("/b.be", 20, "b2"), mf("/c.be", 30, "cc")}),
		3: {rf: backup.RunFiles{Note: "they need HTTP"}},
		1: withArchive(2, []backup.ManifestFile{mf("/a.be", 10, "aa"), mf("/b.be", 20, "b1")}),
	})
	_, body := e.do(t, "GET", "/devices/1/backups", "admin", nil)
	if n := strings.Count(body, `<div class="bk-row`); n != 3 {
		t.Fatalf("%d rows, want 3 runs (a files row is part of its run):\n%s", n, body)
	}
	newest := rowOf(t, body, 5)
	for path, badge := range map[string]string{"b.be": "Changed", "c.be": "New"} {
		if !hasBadge(newest, path, badge) {
			t.Errorf("%s should carry %q in the newest run:\n%s", path, badge, newest)
		}
	}
	if hasBadge(newest, "a.be", "Changed") || hasBadge(newest, "a.be", "New") {
		t.Errorf("a.be is unchanged:\n%s", newest)
	}
	oldest := rowOf(t, body, 1)
	if strings.Contains(oldest, ">Changed<") || strings.Contains(oldest, ">New<") {
		t.Errorf("the first run with files has nothing to compare with:\n%s", oldest)
	}
}

// hasBadge reports whether the file row of path in html holds a badge with the text.
func hasBadge(html, path, text string) bool {
	i := strings.Index(html, `<span class="bk-fl-path">`+path+`</span>`)
	if i < 0 {
		return false
	}
	end := strings.Index(html[i:], `</div>`)
	return strings.Contains(html[i:i+end], ">"+text+"<")
}

// Why: the fold's summary tells at a glance what a run holds; the TLS private key is marked (its
// content is secret and the archive admin-only); a skipped file says why; only admins get the
// download, and nothing shows file contents (Berry scripts hold tokens).
func TestFilesFoldSummaryPrivateKeySkippedAndDownloadOnlyForAdmins(t *testing.T) {
	list := []model.Backup{archive(2, t0), dump(1, t0, "")}
	runs := map[model.BackupID]filesAnswer{1: withArchive(2,
		[]backup.ManifestFile{mf("/autoexec.be", 1229, "x"), mf("/tlskey", 1200, "y"), mf("/zbdata", 2867, "z")},
		backup.ManifestSkip{Path: "/floorplan.jpg", Size: 1677722, Reason: "larger than 1 MiB"})}
	e, _ := filesEnv(t, list, runs)
	_, body := e.do(t, "GET", "/devices/1/backups", "admin", nil)
	row := rowOf(t, body, 1)
	for _, want := range []string{
		`<details class="fold bk-files">`,
		`<span class="meta">3 stored · 5.2 KiB · 1 skipped</span>`,
		`>Private key<`,
		`Over 1 MiB: listed, not stored`,
		`Zigbee device data`,
		`Flash filesystem (LittleFS, 320 KiB, 108 KiB free)`,
		`as secret as the settings dump`,
		`href="/tf/backups/2"`,
		`Download files (.tar)`,
	} {
		if !strings.Contains(row, want) {
			t.Errorf("missing %q in:\n%s", want, row)
		}
	}
	if strings.Contains(row, `<details class="fold bk-files" open`) {
		t.Errorf("the fold starts closed")
	}
	_, op := e.do(t, "GET", "/devices/1/backups", "operator", nil)
	oprow := rowOf(t, op, 1)
	if !strings.Contains(oprow, "autoexec.be") || strings.Contains(oprow, "Download files") || strings.Contains(oprow, `href="/tf/backups/2"`) {
		t.Errorf("operators see names and sizes, never the download:\n%s", oprow)
	}
}

// Why (plan L4): a damaged or missing archive must not fail the page of the whole device: the
// other runs stay usable, the broken run says so in its fold, and a damaged run is skipped as
// the comparison base.
func TestDamagedArchiveIsShownInTheFoldNotAsAPageError(t *testing.T) {
	list := []model.Backup{
		archive(6, t0.Add(2*time.Hour)), dump(5, t0.Add(2*time.Hour), ""),
		archive(4, t0.Add(time.Hour)), dump(3, t0.Add(time.Hour), ""),
		archive(2, t0), dump(1, t0, ""),
	}
	e, _ := filesEnv(t, list, map[model.BackupID]filesAnswer{
		5: withArchive(6, []backup.ManifestFile{mf("/a.be", 10, "aa")}),
		3: {err: fmt.Errorf("%w: a file does not match its SHA-256", backup.ErrBadArchive)},
		1: withArchive(2, []backup.ManifestFile{mf("/a.be", 10, "aa")}),
	})
	resp, body := e.do(t, "GET", "/devices/1/backups", "admin", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if !strings.Contains(rowOf(t, body, 3), "archive damaged") {
		t.Errorf("the damaged run must say so:\n%s", rowOf(t, body, 3))
	}
	if strings.Contains(rowOf(t, body, 3), "Download files") {
		t.Errorf("no download of a damaged archive")
	}
	// the damaged run is not the base of the comparison: a.be equals the one two runs back
	if hasBadge(rowOf(t, body, 5), "a.be", "New") || hasBadge(rowOf(t, body, 5), "a.be", "Changed") {
		t.Errorf("compared with the damaged run:\n%s", rowOf(t, body, 5))
	}
}

// Why: the row's detail line says why a run has no files, in words a person understands, and the
// fixed note of the dump stays out of the Notes text.
func TestRowDetailSaysWhyThereAreNoFiles(t *testing.T) {
	for _, tc := range []struct{ note, want string }{
		{"no filesystem", "settings only: this build has no filesystem"},
		{"firmware older than 14.6.0", "settings only: firmware older than 14.6.0 has no file access"},
		{"they need HTTP", "files not backed up: they need HTTP, this run went over MQTT"},
		{"the device did not answer", "files not backed up: the device did not answer"},
	} {
		list := []model.Backup{dump(1, t0, "Nightly; "+backup.NoteFilesPrefix+tc.note)}
		e, _ := filesEnv(t, list, map[model.BackupID]filesAnswer{1: {rf: backup.RunFiles{Note: tc.note}}})
		_, body := e.do(t, "GET", "/devices/1/backups", "admin", nil)
		row := rowOf(t, body, 1)
		if !strings.Contains(row, "Nightly") || !strings.Contains(row, tc.want) || strings.Contains(row, "bk-files") {
			t.Errorf("%q: want %q and no fold:\n%s", tc.note, tc.want, row)
		}
	}
}

// Why (plan L5): a crash between the dump row and the tar row leaves a run with neither tar nor
// note. After the files step exists that must not read as "from before the files step"; but a
// run older than every archive of the device really is from before, and says nothing.
func TestRunWithoutTarAndNoteIsOnlyCalledMissingWhenOlderRunsHaveFiles(t *testing.T) {
	list := []model.Backup{
		dump(5, t0.Add(2*time.Hour), ""),
		archive(4, t0.Add(time.Hour)), dump(3, t0.Add(time.Hour), ""),
		dump(1, t0, ""),
	}
	e, _ := filesEnv(t, list, map[model.BackupID]filesAnswer{
		5: {}, 3: withArchive(4, []backup.ManifestFile{mf("/a.be", 10, "aa")}), 1: {},
	})
	_, body := e.do(t, "GET", "/devices/1/backups", "admin", nil)
	if got := rowOf(t, body, 5); !strings.Contains(got, "no files were stored for this run") {
		t.Errorf("newest run lost its files silently:\n%s", got)
	}
	if got := rowOf(t, body, 1); strings.Contains(got, "files") && !strings.Contains(got, "files/") {
		t.Errorf("a run from before the files step must not claim anything:\n%s", got)
	}
}

// Why: the tar is as secret as the dump (spec 12.3): admin only, audited with its kind, served as
// an attachment whose name says it holds files.
func TestFilesArchiveDownloadIsAdminOnlyAndAudited(t *testing.T) {
	list := []model.Backup{
		{ID: 2, DeviceID: 1, DeviceName: "kitchen", Kind: model.BackupFiles, TakenAt: t0, Path: "backups/1/20261101T020000Z-files.tar"},
		dump(1, t0, ""),
	}
	e, _ := filesEnv(t, list, nil)
	resp, _ := e.do(t, "GET", "/backups/2", "operator", nil)
	if resp.StatusCode != 403 || len(e.aud.events) != 0 {
		t.Fatalf("operator: %d, audit %+v", resp.StatusCode, e.aud.events)
	}
	resp, body := e.do(t, "GET", "/backups/2", "admin", nil)
	if resp.StatusCode != 200 || body != "TAR" {
		t.Fatalf("admin: %d %q", resp.StatusCode, body)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "kitchen-20261101T020000Z-files.tar") {
		t.Errorf("Content-Disposition %q", cd)
	}
	if len(e.aud.events) != 1 || e.aud.events[0].Action != "backup.download" || !strings.Contains(e.aud.events[0].Detail, `"kind":"files"`) {
		t.Errorf("audit %+v", e.aud.events)
	}
}

// Why: the Backups tab of a server whose backup service has no files API (older wiring, tests)
// must still render.
func TestBackupsTabWorksWithoutFilesAPI(t *testing.T) {
	e := setup(t)
	e.bk.list = []model.Backup{dump(1, t0, "")}
	resp, body := e.do(t, "GET", "/devices/1/backups", "admin", nil)
	if resp.StatusCode != 200 || strings.Contains(body, "bk-files") {
		t.Errorf("%d %s", resp.StatusCode, body)
	}
}
