package web

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/restore"
)

type adminBackups struct {
	diffBackups
	mu2     sync.Mutex
	deleted []model.BackupID
	newest  bool
	planErr error
	// remaining is the number of runs the device keeps after a delete (0: it was the last).
	remaining int
}

func (a *adminBackups) PlanDelete(context.Context, model.BackupID) (backup.Deletion, error) {
	return backup.Deletion{Device: 1, Runs: 1, Files: 3, Newest: a.newest}, a.planErr
}
func (a *adminBackups) DeleteRun(_ context.Context, _ string, id model.BackupID) (backup.Deletion, error) {
	a.mu2.Lock()
	defer a.mu2.Unlock()
	a.deleted = append(a.deleted, id)
	return backup.Deletion{Device: 1, Runs: 1, Files: 3, Newest: a.newest, Remaining: a.remaining}, nil
}

type replRestore struct {
	fakeRestore
	preview  restore.ReplacePreview
	prevErr  error
	startErr error
	started  []restore.ReplaceRequest
	opts     []restore.StartOptions
	cands    []restore.Endpoint
}

func (r *replRestore) Candidates(_ context.Context, eps []restore.Endpoint) []restore.Endpoint {
	return append(r.cands, eps...)
}
func (r *replRestore) PreviewReplace(context.Context, restore.ReplaceRequest) (restore.ReplacePreview, error) {
	return r.preview, r.prevErr
}
func (r *replRestore) StartReplace(_ context.Context, _ string, req restore.ReplaceRequest, o restore.StartOptions) (restore.Job, error) {
	r.started = append(r.started, req)
	r.opts = append(r.opts, o)
	if r.startErr != nil {
		return restore.Job{}, r.startErr
	}
	return restore.Job{ID: 7, DeviceID: req.Device, Kind: "replace"}, nil
}

// newAdminEnv is a device with two config dumps, the backup service with deleting, keeping
// and retention (N1), and the restore service with replace.
func newAdminEnv(t *testing.T) (*env, *keepBackups, *replRestore) {
	t.Helper()
	ab := &adminBackups{}
	k := &keepBackups{adminBackups: ab, deviceOf: 1}
	t0 := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	ab.list = []model.Backup{
		{ID: 4, DeviceID: 1, Kind: model.BackupConfigDump, TakenAt: t0.Add(time.Hour), Version: "15.6.0", Size: 4096},
		{ID: 1, DeviceID: 1, Kind: model.BackupConfigDump, TakenAt: t0, Version: "15.6.0", Size: 4096},
	}
	rr := &replRestore{}
	rr.preview = restore.ReplacePreview{
		Old:    model.Device{ID: 1, Name: "kitchen", MAC: "aa:bb:cc:00:00:01", Address: "10.0.0.1"},
		New:    model.Device{Address: "10.0.0.9", MAC: "aa:bb:cc:00:00:09", Identity: model.Identity{Family: model.ChipESP8266, Version: "15.6.0"}},
		Backup: ab.list[0], Dumps: ab.list,
		Carries: restore.Carries{Known: true, Hostname: "kitchen-plug", StaticIP: "10.0.0.1", MQTTTopic: "kitchen", SSIDs: []string{"home"}},
	}
	e := setupWith(t, func(d *Deps) {
		d.Backups = k
		d.Restore = rr
	})
	return e, k, rr
}

// Why: every new state-changing or sensitive route sits behind the admin
// guard (the same wrapper as the existing admin routes): no session is 401,
// an operator is 403, and only the admin gets through. A route missing from
// this list would be an authorization bypass.
func TestBackupDeleteAndReplaceRoutesAreAdminOnly(t *testing.T) {
	e, ab, rr := newAdminEnv(t)
	routes := []struct {
		method, path string
		form         url.Values
	}{
		{"POST", "/backups/4/delete", url.Values{}},
		{"GET", "/devices/1/backups/prune", nil},
		{"GET", "/devices/1/backups/delete?id=4", nil},
		{"POST", "/devices/1/backups/delete", url.Values{"id": {"4"}, "confirm": {"1"}}},
		{"GET", "/backups/4/restore", nil},
		{"POST", "/backups/4/restore", url.Values{"confirm": {"1"}}},
		{"GET", "/devices/1/replace", nil},
		{"GET", "/devices/1/replace/preview?address=10.0.0.9", nil},
		{"POST", "/devices/1/replace", url.Values{"address": {"10.0.0.9"}, "confirm": {"1"}}},
	}
	for _, rt := range routes {
		if resp, _ := e.do(t, rt.method, rt.path, "", rt.form); resp.StatusCode != 401 {
			t.Errorf("%s %s without a session: %d, want 401", rt.method, rt.path, resp.StatusCode)
		}
		if resp, _ := e.do(t, rt.method, rt.path, "operator", rt.form); resp.StatusCode != 403 {
			t.Errorf("%s %s as operator: %d, want 403", rt.method, rt.path, resp.StatusCode)
		}
	}
	if len(ab.deleted) != 0 || len(ab.selected) != 0 || len(rr.started) != 0 || len(rr.jobs) != 0 {
		t.Fatalf("a refused request changed something: %v %v %v", ab.deleted, ab.selected, rr.started)
	}
	for _, rt := range routes {
		if resp, _ := e.do(t, rt.method, rt.path, "admin", rt.form); resp.StatusCode >= 400 {
			t.Errorf("%s %s as admin: %d", rt.method, rt.path, resp.StatusCode)
		}
	}
	if len(ab.deleted) != 1 || len(ab.selected) != 1 || len(rr.started) != 1 {
		t.Errorf("admin calls: deleted %v selected %v started %v", ab.deleted, ab.selected, rr.started)
	}
}

// Why: without the services behind them the pages must not exist.
func TestBackupDeleteAndReplaceAbsentWithoutServices(t *testing.T) {
	e := setup(t)
	for _, p := range []string{"/devices/1/replace", "/devices/1/backups/prune", "/devices/1/backups/delete?id=4"} {
		if resp, _ := e.do(t, "GET", p, "admin", nil); resp.StatusCode != 404 {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
	if resp, _ := e.do(t, "POST", "/backups/4/delete", "admin", url.Values{}); resp.StatusCode != 404 && resp.StatusCode != 405 {
		t.Errorf("delete: %d", resp.StatusCode)
	}
}

// Why: deleting the newest backup is the one delete that can leave a device
// without a restore point, so its confirmation dialog carries a warning the
// others do not; the delete and restore buttons are admin-only.
func TestDeleteButtonWarnsOnlyForTheNewestBackup(t *testing.T) {
	e, _, _ := newAdminEnv(t)
	_, body := e.do(t, "GET", "/devices/1/backups", "admin", nil)
	// The warning is part of the delete dialog's text of the newest backup only, in sentence case.
	if strings.Count(body, "This is the newest backup of this device") != 1 || strings.Contains(body, "NEWEST") || strings.Contains(body, "WARNING:") {
		t.Errorf("want the warning on exactly one delete dialog, in sentence case:\n%s", body)
	}
	if strings.Count(body, `data-dialog="dlg-bkdel"`) != 2 || !strings.Contains(body, `href="/tf/backups/4/restore"`) {
		t.Errorf("restore/delete controls missing:\n%s", body)
	}
	_, op := e.do(t, "GET", "/devices/1/backups", "operator", nil)
	if strings.Contains(op, "/delete") || strings.Contains(op, "/restore") || strings.Contains(op, "Prune now") {
		t.Errorf("operator sees admin controls:\n%s", op)
	}
	// The overview (design v2) leads to the Backups tab and the Replace device tab; the backup
	// controls live on the Backups tab only.
	_, card := e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(card, `href="/tf/devices/1/backups">Backups</a>`) || !strings.Contains(card, "Replace device") {
		t.Errorf("device page lacks the tabs:\n%s", card)
	}
}

// Why: "Prune now" deletes exactly the backups the tab marks "Pruned at next run" (the ids it
// showed, not whatever the retention says a moment later), one row per run; only the
// confirmed POST deletes, and the page twin of the dialog says the same.
func TestPruneNowDeletesTheMarkedRunsOnly(t *testing.T) {
	e, k, _ := newAdminEnv(t)
	old := k.list[1] // the older dump; its snapshot belongs to the same run
	k.pruned = []model.Backup{old, {ID: 7, DeviceID: 1, Kind: model.BackupSnapshot, TakenAt: old.TakenAt}}
	_, tab := e.do(t, "GET", "/devices/1/backups", "admin", nil)
	if strings.Count(tab, "Pruned at next run") != 1 || !strings.Contains(tab, `id="dlg-prune"`) || !strings.Contains(tab, "Prune 1 backup now?") {
		t.Fatalf("tab:\n%s", tab)
	}
	dlg := tab[strings.Index(tab, `id="dlg-prune"`):]
	dlg = dlg[:strings.Index(dlg, "</dialog>")]
	if strings.Count(dlg, `name="id"`) != 1 || !strings.Contains(dlg, `name="id" value="1"`) || !strings.Contains(dlg, `name="confirm" value="1"`) {
		t.Errorf("the dialog must post the one marked run:\n%s", dlg)
	}
	resp, page := e.do(t, "GET", "/devices/1/backups/prune", "admin", nil)
	if resp.StatusCode != 200 || !strings.Contains(page, "Prune 1 backup") || !strings.Contains(page, `name="id" value="1"`) || strings.Contains(page, `value="4"`) {
		t.Fatalf("page: %d\n%s", resp.StatusCode, page)
	}
	if len(k.selected) != 0 {
		t.Fatal("showing the confirmation deleted")
	}
	if resp, _ := e.do(t, "POST", "/devices/1/backups/delete", "admin", url.Values{"id": {"1"}}); resp.StatusCode != 422 || len(k.selected) != 0 {
		t.Errorf("without the confirmation: %d, deleted %v", resp.StatusCode, k.selected)
	}
	resp, _ = e.do(t, "POST", "/devices/1/backups/delete", "admin", url.Values{"id": {"1"}, "confirm": {"1"}})
	if resp.StatusCode != 303 || len(k.selected) != 1 || len(k.selected[0]) != 1 || k.selected[0][0] != 1 {
		t.Errorf("confirmed: %d, deleted %v", resp.StatusCode, k.selected)
	}
	k.pruned = nil
	_, page = e.do(t, "GET", "/devices/1/backups/prune", "admin", nil)
	// the dead end leads back to the device's backups, with the device in the trail
	if !strings.Contains(page, "Nothing to prune") || !strings.Contains(page, `href="/tf/devices/1/backups">Back to the backups`) || !strings.Contains(page, `>Prune<`) {
		t.Errorf("nothing marked:\n%s", page)
	}
}

// Why: the wizard's refusals use fixed sentences (device error text never
// reaches the page), and the risks ride on the one confirmation.
func TestReplaceWizardRefusalsAndOneConfirmation(t *testing.T) {
	e, _, rr := newAdminEnv(t)
	rr.cands = []restore.Endpoint{{Address: "10.0.0.9", Port: 80, MAC: "aa:bb:cc:00:00:09"}}
	if _, body := e.do(t, "GET", "/devices/1/replace/preview?address=10.0.0.9", "admin", nil); !strings.Contains(strings.ReplaceAll(body, "<wbr>", ""), "kitchen-plug") || !strings.Contains(body, "home") || strings.Count(body, `type="checkbox" name="`) != 1 {
		t.Errorf("preview:\n%s", body)
	}
	rr.preview.Major, rr.preview.Detail = true, "the backup is from firmware 14.0.0, the new device runs 15.6.0"
	if _, body := e.do(t, "GET", "/devices/1/replace/preview?address=10.0.0.9", "admin", nil); !strings.Contains(body, `name="major_ok"`) || !strings.Contains(body, "14.0.0") {
		t.Errorf("major risk:\n%s", body)
	}
	for err, want := range map[error]string{
		restore.ErrOldReachable:     "still answers",
		restore.ErrFamilyMismatch:   "another chip family",
		restore.ErrCandidateBound:   "already belongs",
		restore.ErrCandidateOffline: "did not answer",
	} {
		rr.prevErr = errors.Join(err, errors.New("dial tcp 10.1.2.3: secret-detail"))
		resp, body := e.do(t, "GET", "/devices/1/replace/preview?address=10.0.0.9", "admin", nil)
		if resp.StatusCode != 422 || !strings.Contains(body, want) || strings.Contains(body, "secret-detail") {
			t.Errorf("%v: %d\n%s", err, resp.StatusCode, body)
		}
	}
	rr.prevErr = nil
	if resp, _ := e.do(t, "POST", "/devices/1/replace", "admin", url.Values{"address": {"10.0.0.9"}}); resp.StatusCode != 422 || len(rr.started) != 0 {
		t.Errorf("without the tick: %d, started %v", resp.StatusCode, rr.started)
	}
	resp, _ := e.do(t, "POST", "/devices/1/replace", "admin", url.Values{"address": {"10.0.0.9"}, "port": {"8080"}, "backup": {"4"}, "confirm": {"1"}, "major_ok": {"1"}})
	if resp.StatusCode != 303 || !strings.HasSuffix(resp.Header.Get("Location"), "/restores/7") {
		t.Fatalf("start: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if q := rr.started[0]; q.Device != 1 || q.Address != "10.0.0.9" || q.Port != 8080 || q.Backup != 4 || !rr.opts[0].AllowMajorChange {
		t.Errorf("request: %+v %+v", q, rr.opts[0])
	}
	if resp, _ := e.do(t, "GET", "/devices/1/replace/preview?address=", "admin", nil); resp.StatusCode != 422 {
		t.Errorf("empty address: %d", resp.StatusCode)
	}
}

// Why: the number of backups the device page names must match what the Backups tab
// lists. The page used to list dumps only, so it said 6 above a page with 3 rows.
// Decoded copies only feed the comparison of their dump and are neither counted nor
// listed. Since N2 the number is on the delete page ("This device still has 3 backups");
// the tab itself carries no count, as in the prototype.
func TestBackupsTabCountMatchesTheListedRows(t *testing.T) {
	e, ab, _ := newAdminEnv(t)
	t0 := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	ab.list = append(ab.list,
		model.Backup{ID: 5, DeviceID: 1, Kind: model.BackupSnapshot, TakenAt: t0.Add(time.Hour), Version: "15.6.0", Size: 900, Note: "snapshot incomplete: Status 2"},
		model.Backup{ID: 6, DeviceID: 1, Kind: model.BackupDecoded, TakenAt: t0.Add(time.Hour), Version: "15.6.0", Size: 800},
		model.Backup{ID: 8, DeviceID: 1, Kind: model.BackupSnapshot, TakenAt: t0.Add(-time.Hour), Version: "15.6.0", Size: 700},
	)
	if _, body := e.do(t, "GET", "/devices/1/delete", "admin", nil); !strings.Contains(body, "This device still has 3 backups") || strings.Count(body, `class="dd-row"`) != 3 {
		t.Error("the delete page must count and list 3 runs (2 with a dump, 1 snapshot only), not files")
	}
	_, body := e.do(t, "GET", "/devices/1/backups", "admin", nil)
	if rows := strings.Count(body, `class="bk-row`); rows != 3 {
		t.Errorf("the page lists %d backups, the tab says 3", rows)
	}
	if !strings.Contains(body, "incomplete snapshot, missing Status 2") || !strings.Contains(body, `href="/tf/backups/8"`) {
		t.Errorf("the run's incomplete snapshot is not named, or the snapshot-only run has no download:\n%s", body)
	}
}

// Why (#290): users verify the backup output in the file system, so every row names the file
// as stored (the base name of the run's dump, not the decoded copy or the data dir path) and
// the Download button's hover help names it too.
func TestBackupRowNamesTheStoredFileAndDownloadHoverRepeatsIt(t *testing.T) {
	e, ab, _ := newAdminEnv(t)
	ab.list[0].Path = "backups/kitchen-aabbcc000001/2026-10-18T11-00-00Z-config.dmp"
	ab.list[1].Path = "backups/kitchen-aabbcc000001/2026-10-18T10-00-00Z-config.dmp"
	_, body := e.do(t, "GET", "/devices/1/backups", "admin", nil)
	for _, name := range []string{"2026-10-18T11-00-00Z-config.dmp", "2026-10-18T10-00-00Z-config.dmp"} {
		if !strings.Contains(body, `<div class="bk-file" title="`+name+`">`+name+`</div>`) {
			t.Errorf("the row does not show the file name %s", name)
		}
		if !strings.Contains(body, `title="Download `+name+`"`) {
			t.Errorf("the Download hover help does not name %s", name)
		}
	}
	if strings.Contains(body, "kitchen-aabbcc000001/2026") {
		t.Error("only the file name is shown, not the directory path")
	}
}

// Why (#324): the replacement preview shows the files before the person confirms: which are written
// first (the private key marked), which the backup does not hold, and the acknowledgement names
// the file count. A backup without files must not mention files at all.
func TestReplacePreviewListsTheFilesAndAcknowledgesThem(t *testing.T) {
	e, ab, rr := newAdminEnv(t)
	rr.preview.Files = restore.FilesPlan{
		Files: []restore.PlannedFile{{Path: "/autoexec.be", Size: 1200}, {Path: "/tlskey", Size: 1200, PrivateKey: true}},
		Bytes: 2400, NotStored: []backup.ManifestSkip{{Path: "/floorplan.jpg", Size: 1700000, Reason: "larger than 1 MiB"}},
	}
	rr.preview.DumpFiles = map[model.BackupID]string{ab.list[0].ID: "2 files"}
	_, body := e.do(t, "GET", "/devices/1/replace/preview?address=10.0.0.9", "admin", nil)
	flat := strings.ReplaceAll(body, "<wbr>", "")
	for _, want := range []string{"Files (2, written first)", "autoexec.be", "Private key", "floorplan.jpg (1.6 MiB)", "Over 1 MiB: listed in the backup, not stored",
		"Copy the files of its filesystem", "receives the 2 files and the configuration", "2 files · newest", "files the board did not have before stay on it"} {
		if !strings.Contains(flat, want) {
			t.Errorf("missing %q", want)
		}
	}
	rr.preview.Files = restore.FilesPlan{}
	_, plain := e.do(t, "GET", "/devices/1/replace/preview?address=10.0.0.9", "admin", nil)
	if strings.Contains(plain, "written first") || strings.Contains(plain, "filesystem") {
		t.Errorf("a backup without files must not mention files:\n%s", plain)
	}
	rr.preview.Files = restore.FilesPlan{Files: []restore.PlannedFile{{Path: "/autoexec.be", Size: 10}}, Why: "the device has no flash file system, so only the settings are restored"}
	_, off := e.do(t, "GET", "/devices/1/replace/preview?address=10.0.0.9", "admin", nil)
	if strings.Contains(off, "written first") || !strings.Contains(off, "no flash file system") {
		t.Errorf("files that cannot be written must say why:\n%s", off)
	}
}

// Why (M3): the replacement confirmation is bound to the file plan it showed, like the restore's.
func TestReplaceConfirmationCarriesTheFileCount(t *testing.T) {
	e, _, rr := newAdminEnv(t)
	rr.preview.Files = restore.FilesPlan{Files: []restore.PlannedFile{{Path: "/autoexec.be", Size: 1}, {Path: "/x.be", Size: 1}}}
	_, body := e.do(t, "GET", "/devices/1/replace/preview?address=10.0.0.9", "admin", nil)
	if !strings.Contains(body, `name="files" value="2"`) {
		t.Errorf("form lacks the file count:\n%s", body)
	}
	e.do(t, "POST", "/devices/1/replace", "admin", url.Values{"address": {"10.0.0.9"}, "confirm": {"1"}, "files": {"2"}})
	e.do(t, "POST", "/devices/1/replace", "admin", url.Values{"address": {"10.0.0.9"}, "confirm": {"1"}})
	if len(rr.opts) != 2 || rr.opts[0].ConfirmedFiles != 2 || rr.opts[1].ConfirmedFiles != 0 {
		t.Errorf("options: %+v", rr.opts)
	}
	// a reason without a list is shown too
	rr.preview.Files = restore.FilesPlan{Why: "the files archive of this backup is damaged or missing, so only the settings are restored"}
	_, off := e.do(t, "GET", "/devices/1/replace/preview?address=10.0.0.9", "admin", nil)
	if !strings.Contains(off, "damaged or missing") || !strings.Contains(off, `name="files" value="0"`) {
		t.Errorf("reason missing:\n%s", off)
	}
}
