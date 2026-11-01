package web

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/discovery"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/restore"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// Tests of the design v2 backup pages (WP P10): the Backups tab, the comparison, the restore
// progress and the replace tab.

// Why: the tab is the place to see whether backups work. A failed attempt stored nothing, so it
// sits among the runs at its time with its reason and a Retry, never a Download or a checkbox;
// kept runs say so and their toggle releases them (the button posts the state it switches to).
func TestBackupsTabListsRunsFailedAttemptsAndKeep(t *testing.T) {
	e, k, _ := newAdminEnv(t)
	k.list[1].Kept = true // the older run
	t0 := k.list[1].TakenAt
	k.attempts = []model.BackupAttempt{{At: t0.Add(2 * time.Hour), Trigger: model.TriggerManual, Version: "15.6.0", Reason: "HTTP 401: the device rejected the web password"}}
	_, body := e.do(t, "GET", "/devices/1/backups", "admin", nil)
	failed := strings.Index(body, "the device rejected the web password")
	newest := strings.Index(body, `value="4"`)
	older := strings.Index(body, `value="1"`)
	if failed < 0 || newest < 0 || older < 0 || failed > newest || newest > older {
		t.Fatalf("rows not newest first with the failed attempt on top (%d %d %d):\n%s", failed, newest, older, body)
	}
	row := body[strings.LastIndex(body[:failed], `<div class="bk-row`):failed]
	if strings.Contains(row, `type="checkbox"`) || !strings.Contains(body[failed:], ">Retry</button>") {
		t.Errorf("the failed row must have no checkbox and a Retry:\n%s", row)
	}
	if !strings.Contains(body, "Keeping the last 5 backups and anything kept") || !strings.Contains(body, "2 backups · 8.0 KiB") {
		t.Errorf("retention line or total missing:\n%s", body)
	}
	for _, want := range []string{
		`formaction="/tf/backups/1/keep" name="kept" value="0"`, // kept: the toggle releases it
		`formaction="/tf/backups/4/keep" name="kept" value="1"`, // not kept: the toggle keeps it
		`<span class="bdg t-accent">Kept</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	// operators may keep (it deletes nothing) but not download, restore or delete
	_, op := e.do(t, "GET", "/devices/1/backups", "operator", nil)
	if !strings.Contains(op, `name="kept"`) || strings.Contains(op, `href="/tf/backups/4"`) || strings.Contains(op, "Delete selected") {
		t.Errorf("operator controls:\n%s", op)
	}
}

// Why: "Delete selected" must say how many backups go and whether the newest is among them
// before anything is deleted, and post exactly the ticked ids.
func TestDeleteSelectedConfirmNamesCountAndNewest(t *testing.T) {
	e, k, _ := newAdminEnv(t)
	k.planNewest = true
	resp, body := e.do(t, "GET", "/devices/1/backups/delete?id=4&id=1", "admin", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "Delete 2 backups") || !strings.Contains(body, "This includes the newest backup of kitchen") {
		t.Fatalf("%d\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `name="id" value="4"`) || !strings.Contains(body, `name="id" value="1"`) || !strings.Contains(body, `name="confirm" value="1"`) {
		t.Errorf("the form must carry the ticked ids and the confirmation:\n%s", body)
	}
	if len(k.selected) != 0 {
		t.Fatal("the confirmation page deleted")
	}
	if resp, _ := e.do(t, "GET", "/devices/1/backups/delete?id=x", "admin", nil); resp.StatusCode != 404 {
		t.Errorf("bad id: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", "/devices/1/backups/delete", "admin", nil); resp.StatusCode != 422 {
		t.Errorf("nothing ticked: %d", resp.StatusCode)
	}
}

// Why: the comparison counts what differs out of all settings, "Show all" adds the unchanged
// ones, two ticked rows of the tab (dump ids) compare their decoded copies, older first; and
// "Restore this backup" restores the older one, for admins only.
func TestComparePickAllAndRestoreOlder(t *testing.T) {
	e, bk, _ := newDiffEnv(t)
	bk.files[3] = []byte(`{"tele_period":60,"ledstate":1,"topic":"kitchen"}`)
	bk.files[6] = []byte(`{"tele_period":300,"ledstate":1,"topic":"kitchen"}`)
	_, body := e.do(t, "GET", "/devices/1/backups/diff?id=4&id=1", "admin", nil) // newer first, as ticked
	if !strings.Contains(body, "1 of 3 settings differ") || !strings.Contains(body, "Older · 18 Oct 2026, 10:00") || strings.Contains(body, ">ledstate<") {
		t.Errorf("changed rows only, older on the left:\n%s", body)
	}
	if !strings.Contains(body, `href="/tf/backups/1/restore"`) {
		t.Errorf("restore must offer the older backup (dump 1):\n%s", body)
	}
	_, all := e.do(t, "GET", "/devices/1/backups/diff?a=3&b=6&all=1", "admin", nil)
	if !strings.Contains(all, ">ledstate<") || !strings.Contains(all, "Show all 3 settings") {
		t.Errorf("show all lacks the unchanged settings:\n%s", all)
	}
	_, def := e.do(t, "GET", "/devices/1/backups/diff", "admin", nil)
	if !strings.Contains(def, "1 of 3 settings differ") {
		t.Errorf("without a choice the two newest are compared:\n%s", def)
	}
	_, op := e.do(t, "GET", "/devices/1/backups/diff", "operator", nil)
	if strings.Contains(op, "/restore") {
		t.Error("operators get no restore")
	}
	if resp, _ := e.do(t, "GET", "/devices/1/backups/diff?id=4", "admin", nil); resp.StatusCode != 404 {
		t.Errorf("one ticked row: %d", resp.StatusCode)
	}
	// Without a decoder the backups have no decoded copies: the page says why instead of a 404.
	bk.list = bk.list[:2] // the dump and the snapshot of one run, no decoded copy
	resp, none := e.do(t, "GET", "/devices/1/backups/diff", "admin", nil)
	if resp.StatusCode != 200 || !strings.Contains(none, "Nothing to compare yet") || !strings.Contains(none, "decoder") {
		t.Errorf("undecoded: %d\n%s", resp.StatusCode, none)
	}
}

// Why: the progress page must say which step a restore is at or failed at, and after a
// failure offer the way back: the backup taken before the restore (only if it was taken).
func TestRestoreProgressStepsAndRollback(t *testing.T) {
	e, _, rs := newDiffEnv(t)
	rs.jobs = map[store.RestoreID]restore.Job{
		1: {ID: 1, DeviceID: 1, BackupID: 1, State: store.RestoreSending, Detail: "asking the device to fetch the configuration", PreBackupID: 4, CreatedAt: time.Now()},
		2: {ID: 2, DeviceID: 1, BackupID: 1, State: store.RestoreFailed, Detail: "the device downloaded the configuration but did not come back in time", PreBackupID: 4, CreatedAt: time.Now()},
		3: {ID: 3, DeviceID: 1, BackupID: 1, State: store.RestoreFailed, Detail: "backup before the restore failed, nothing was changed: timeout", CreatedAt: time.Now()},
	}
	_, running := e.do(t, "GET", "/restores/1", "admin", nil)
	if !strings.Contains(running, `rs-step is-done`) || !strings.Contains(running, `rs-step is-active`) || strings.Count(running, `rs-step is-todo`) != 2 {
		t.Errorf("running at step 3 (upload) of 5:\n%s", running)
	}
	_, failed := e.do(t, "GET", "/restores/2", "admin", nil)
	if strings.Count(failed, `rs-step is-done`) != 3 || !strings.Contains(failed, `rs-step is-failed`) || !strings.Contains(failed, "Restore failed.") {
		t.Errorf("failed at step 4 (restart) of 5:\n%s", failed)
	}
	// a failed job can be opened days later: the backup to roll back to is named with its date, not only a clock time
	if !strings.Contains(failed, `href="/tf/backups/4/restore">Restore the 18 Oct 2026, 11:00 backup`) || !strings.Contains(failed, `href="/tf/backups/1/restore">Retry restore`) {
		t.Errorf("rollback and retry missing:\n%s", failed)
	}
	_, early := e.do(t, "GET", "/restores/3", "admin", nil)
	if !strings.Contains(early, "Nothing was changed on the device.") || strings.Contains(early, "/backups/4/restore") {
		t.Errorf("a failed pre-backup has nothing to roll back to:\n%s", early)
	}
	// The pre-backup exists, but the upload was refused: nothing changed, so offering a
	// rollback would only restart the device for nothing.
	rs.jobs[5] = restore.Job{ID: 5, DeviceID: 1, BackupID: 1, State: store.RestoreFailed, Detail: "WebGetConfig was not accepted: unknown command", PreBackupID: 4, CreatedAt: time.Now()}
	_, refused := e.do(t, "GET", "/restores/5", "admin", nil)
	if !strings.Contains(refused, "Nothing was changed on the device.") || strings.Contains(refused, "/backups/4/restore") || !strings.Contains(refused, "Retry restore") {
		t.Errorf("a refused upload offers retry, not a rollback:\n%s", refused)
	}
}

// Why: the new device is picked from the discovered list as one value (address and port), and a
// typed address wins over it; the request reaching the service is the one shown.
func TestReplaceTargetFromTheList(t *testing.T) {
	_, k, rr := newAdminEnv(t)
	e := setupWith(t, func(d *Deps) {
		d.Backups, d.Restore = k, rr
		d.Discovery = &fakeDiscovery{props: []discovery.Proposal{{ID: 1, Kind: discovery.KindNew, Device: model.Device{Address: "10.0.0.9", Port: 8080}}}}
	})
	_, start := e.do(t, "GET", "/devices/1/replace", "admin", nil)
	if !strings.Contains(start, `<option value="10.0.0.9:8080">10.0.0.9:8080</option>`) {
		t.Fatalf("candidate missing:\n%s", start)
	}
	_, pv := e.do(t, "GET", "/devices/1/replace/preview?target=10.0.0.9:8080", "admin", nil)
	if !strings.Contains(pv, `name="address" value="10.0.0.9"`) || !strings.Contains(pv, `name="port" value="8080"`) {
		t.Errorf("the preview must carry the chosen device:\n%s", pv)
	}
	resp, _ := e.do(t, "POST", "/devices/1/replace", "admin", url.Values{"target": {"10.0.0.9:8080"}, "address": {"10.0.0.7"}, "confirm": {"1"}})
	if resp.StatusCode != 303 || rr.started[0].Address != "10.0.0.7" {
		t.Errorf("a typed address wins: %d %+v", resp.StatusCode, rr.started)
	}
}
