package web

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/restore"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

const syn = "../backup/testdata/synthetic/"

func readSyn(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(syn + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// diffBackups serves stored backups by ID and a canned live snapshot.
type diffBackups struct {
	fakeBackups
	files map[model.BackupID][]byte
	live  []byte
	mu    sync.Mutex
	liveN int
}

func (f *diffBackups) Open(_ context.Context, id model.BackupID) ([]byte, model.Backup, error) {
	for _, b := range f.list {
		if b.ID == id {
			return f.files[id], b, nil
		}
	}
	return nil, model.Backup{}, store.ErrNotFound
}

func (f *diffBackups) LiveSnapshot(context.Context, model.DeviceID) ([]byte, error) {
	f.mu.Lock()
	f.liveN++
	f.mu.Unlock()
	return f.live, nil
}

type fakeRestore struct {
	mu   sync.Mutex
	jobs map[store.RestoreID]restore.Job
	busy bool
	err  error
	n    store.RestoreID
	opts []restore.StartOptions
	need *restore.OverrideError
	// assessment and error for the confirmation page
	assess    restore.Assessment
	assessErr error
	progress  map[store.RestoreID]restore.FilesProgress
}

func (f *fakeRestore) FilesProgress(id store.RestoreID) (restore.FilesProgress, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.progress[id]
	return p, ok
}

func (f *fakeRestore) Assess(context.Context, model.BackupID) (restore.Assessment, error) {
	return f.assess, f.assessErr
}

func (f *fakeRestore) StartWith(_ context.Context, actor string, b model.BackupID, o restore.StartOptions) (restore.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opts = append(f.opts, o)
	if f.need != nil && !o.AllowMajorChange && f.need.Major {
		return restore.Job{}, f.need
	}
	if f.err != nil {
		return restore.Job{}, f.err
	}
	if f.busy {
		return restore.Job{}, restore.ErrBusy
	}
	f.n++
	j := restore.Job{ID: f.n, DeviceID: 1, BackupID: b, Actor: actor, State: store.RestoreBackup, CreatedAt: time.Now()}
	if f.jobs == nil {
		f.jobs = map[store.RestoreID]restore.Job{}
	}
	f.jobs[j.ID] = j
	return j, nil
}
func (f *fakeRestore) Get(_ context.Context, id store.RestoreID) (restore.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok {
		return j, store.ErrNotFound
	}
	return j, nil
}
func (f *fakeRestore) List(context.Context, model.DeviceID, int) ([]restore.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []restore.Job
	for _, j := range f.jobs {
		out = append(out, j)
	}
	return out, nil
}

// Every secret value of the synthetic dumps; none may reach any page.
var secretNeedles = []string{
	"FAKE-SECRET", "synthetic-wifi-pass", "synthetic-mqtt-pass", "synthetic-web-pass", "ouser",
	"4242", "777001", "0x5ec7", "0x6d6d", "0x7a7a",
}

func newDiffEnv(t *testing.T) (*env, *diffBackups, *fakeRestore) {
	e, bk, rs, _ := newDiffEnvStep(t)
	return e, bk, rs
}

func newDiffEnvStep(t *testing.T) (*env, *diffBackups, *fakeRestore, *atomic.Bool) {
	t.Helper()
	base, sec := readSyn(t, "decoded.json"), readSyn(t, "decoded-secrets.json")
	t0 := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	bk := &diffBackups{files: map[model.BackupID][]byte{}}
	bk.list = []model.Backup{
		{ID: 4, DeviceID: 1, Kind: model.BackupConfigDump, TakenAt: t0.Add(time.Hour), Version: "15.6.0", Size: 4096},
		{ID: 5, DeviceID: 1, Kind: model.BackupSnapshot, TakenAt: t0.Add(time.Hour), Version: "15.6.0"},
		{ID: 6, DeviceID: 1, Kind: model.BackupDecoded, TakenAt: t0.Add(time.Hour), Version: "15.6.0"},
		{ID: 1, DeviceID: 1, Kind: model.BackupConfigDump, TakenAt: t0, Version: "15.6.0", Size: 4096},
		{ID: 2, DeviceID: 1, Kind: model.BackupSnapshot, TakenAt: t0, Version: "15.6.0"},
		{ID: 3, DeviceID: 1, Kind: model.BackupDecoded, TakenAt: t0, Version: "15.6.0"},
		{ID: 9, DeviceID: 2, Kind: model.BackupDecoded, TakenAt: t0, Version: "15.0.0"},
	}
	bk.files[3], bk.files[6], bk.files[9] = base, sec, base
	bk.files[5] = []byte(`{"Status6":{"StatusMQT":{"MqttHost":"old.invalid","MqttPassword":"FAKE-SECRET-live"}},"Status1":{"StatusPRM":{"Uptime":"1T00:00:00"}}}`)
	bk.live = []byte(`{"Status6":{"StatusMQT":{"MqttHost":"new.invalid","MqttPassword":"FAKE-SECRET-live2"}},"Status1":{"StatusPRM":{"Uptime":"9T00:00:00"}}}`)
	rs := &fakeRestore{}
	stepped := &atomic.Bool{}
	stepped.Store(true)
	e := setupWith(t, func(d *Deps) {
		d.Backups = bk
		d.Restore = rs
		d.Guard = stepGuard{fakeGuard: fakeGuard{}, stepped: stepped}
	})
	return e, bk, rs, stepped
}

// Why: the diff page must show what changed but never a secret, for any role,
// and the same must hold on the drift page (review requirement).
func TestDiffAndDriftPagesNeverRenderSecrets(t *testing.T) {
	e, _, _ := newDiffEnv(t)
	for _, role := range []string{"operator", "admin"} {
		for _, p := range []string{"/devices/1/backups/diff?a=3&b=6", "/devices/1/backups", "/devices/1/drift", "/backups/4/restore"} {
			if p == "/backups/4/restore" && role == "operator" {
				continue
			}
			driftMu.Lock()
			clear(driftLast) // the throttle is tested separately
			driftMu.Unlock()
			resp, body := e.do(t, "GET", p, role, nil)
			if resp.StatusCode != 200 {
				t.Fatalf("%s as %s: %d", p, role, resp.StatusCode)
			}
			for _, n := range secretNeedles {
				if strings.Contains(body, n) {
					t.Errorf("%s as %s leaks %q", p, role, n)
				}
			}
			if strings.Contains(p, "diff?") && (!strings.Contains(body, "secret value changed") || !strings.Contains(body, "tele_period") && !strings.Contains(body, "rules")) {
				t.Errorf("%s as %s: expected secret rows and real changes:\n%s", p, role, body)
			}
		}
	}
}

// Why: rules can hold credentials in shapes no pattern knows, so operators
// only learn that they changed; admins read them. A rule that does carry a
// recognisable credential is a secret row for both.
func TestRuleBodiesOnlyForAdmins(t *testing.T) {
	e, bk, _ := newDiffEnv(t)
	bk.files[3] = []byte(`{"rules":["on power1#state do mem1 old endon"],"mems":["MEMKEY_OLD"]}`)
	bk.files[6] = []byte(`{"rules":["on power1#state do mem1 new endon"],"mems":["MEMKEY_NEW"]}`)
	_, op := e.do(t, "GET", "/devices/1/backups/diff?a=3&b=6", "operator", nil)
	_, ad := e.do(t, "GET", "/devices/1/backups/diff?a=3&b=6", "admin", nil)
	if strings.Contains(op, "do mem1") || strings.Contains(op, "MEMKEY") || !strings.Contains(op, "visible to admins only") {
		t.Errorf("operator view shows a rule or mem, or omits the hint:\n%s", op)
	}
	if !strings.Contains(ad, "do mem1 new") || !strings.Contains(ad, "MEMKEY_NEW") {
		t.Errorf("admin view lacks the rule:\n%s", ad)
	}
	bk.files[6] = []byte(`{"rules":["on a do webquery http://h.invalid/?apikey=FAKE-SECRET-z endon"]}`)
	for _, role := range []string{"operator", "admin"} {
		if _, body := e.do(t, "GET", "/devices/1/backups/diff?a=3&b=6", role, nil); strings.Contains(body, "FAKE-SECRET") {
			t.Errorf("%s sees a credential", role)
		}
	}
}

// Why: a backup ID from another device in the URL must not be comparable
// through this device's page, and values are HTML-escaped.
func TestDiffRejectsForeignAndNonDecodedBackups(t *testing.T) {
	e, _, _ := newDiffEnv(t)
	for _, p := range []string{"/devices/1/backups/diff?a=3&b=9", "/devices/1/backups/diff?a=3&b=4", "/devices/1/backups/diff?a=3", "/devices/1/backups/diff?a=x&b=3"} {
		if resp, _ := e.do(t, "GET", p, "operator", nil); resp.StatusCode != 404 {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
}

func TestDiffEscapesValues(t *testing.T) {
	e, bk, _ := newDiffEnv(t)
	bk.files[3] = []byte(`{"friendlyname":["<script>alert(1)</script>"]}`)
	bk.files[6] = []byte(`{"friendlyname":["<img src=x onerror=1>"]}`)
	_, body := e.do(t, "GET", "/devices/1/backups/diff?a=3&b=6", "admin", nil)
	if strings.Contains(body, "<script>alert") || strings.Contains(body, "<img src=x") {
		t.Errorf("unescaped device text:\n%s", body)
	}
}

// Why: drift compares stored and live status but not the clock; and the 17
// status queries must not be repeatable at will against a small device.
func TestDriftShowsRealChangesIgnoresUptimeAndThrottles(t *testing.T) {
	e, bk, _ := newDiffEnv(t)
	resp, body := e.do(t, "GET", "/devices/1/drift", "operator", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "MqttHost") || strings.Contains(body, "Uptime") {
		t.Fatalf("drift: %d\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "secret value changed") {
		t.Error("changed MqttPassword must appear as a secret row")
	}
	// A reload inside the interval answers from the read just made: the same changes, no second
	// round of status queries (design v2: "Live settings read ... ago").
	resp, body = e.do(t, "GET", "/devices/1/drift", "operator", nil)
	if resp.StatusCode != 200 || bk.liveN != 1 || !strings.Contains(body, "MqttHost") {
		t.Errorf("second call: %d, device queried %d times", resp.StatusCode, bk.liveN)
	}
	// Why (finding 87): while that read is still running nothing was done, so the throttle
	// notice is info (role=status, neutral), never the green success box.
	driftMu.Lock()
	for k, v := range driftLast {
		v.doc = nil
		driftLast[k] = v
	}
	driftMu.Unlock()
	resp, body = e.do(t, "GET", "/devices/1/drift", "operator", nil)
	if resp.StatusCode != 429 || bk.liveN != 1 || !strings.Contains(body, `class="banner tone-neutral" role="status"><span class="banner-dot"></span><div class="banner-text">The device was queried a moment ago`) {
		t.Errorf("throttle notice must be the neutral info: %d %s", resp.StatusCode, body)
	}
}

// Why: restore is the most destructive action in the UI: admin only, an
// explicit confirmation, CSRF-bound form, and a refusal must not start a job.
func TestRestoreRolesConfirmationAndBusy(t *testing.T) {
	e, _, rs := newDiffEnv(t)
	if resp, _ := e.do(t, "GET", "/backups/1/restore", "operator", nil); resp.StatusCode != 403 {
		t.Errorf("operator confirm page: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "POST", "/backups/1/restore", "operator", url.Values{"confirm": {"1"}}); resp.StatusCode != 403 {
		t.Errorf("operator post: %d", resp.StatusCode)
	}
	resp, body := e.do(t, "GET", "/backups/1/restore", "admin", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `name="confirm"`) || !strings.Contains(body, `name="csrf"`) {
		t.Fatalf("confirm page: %d\n%s", resp.StatusCode, body)
	}
	if resp, _ := e.do(t, "POST", "/backups/1/restore", "admin", url.Values{}); resp.StatusCode != 422 || len(rs.jobs) != 0 {
		t.Errorf("without confirmation: %d, jobs %d", resp.StatusCode, len(rs.jobs))
	}
	resp, _ = e.do(t, "POST", "/backups/1/restore", "admin", url.Values{"confirm": {"1"}})
	if resp.StatusCode != 303 || !strings.HasSuffix(resp.Header.Get("Location"), "/restores/1") {
		t.Fatalf("start: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if j := rs.jobs[1]; j.BackupID != 1 || j.Actor != "test:admin" {
		t.Errorf("job: %+v", j)
	}
	rs.busy = true
	if resp, _ := e.do(t, "POST", "/backups/1/restore", "admin", url.Values{"confirm": {"1"}}); resp.StatusCode != 409 {
		t.Errorf("busy: %d", resp.StatusCode)
	}
	rs.busy, rs.err = false, restore.ErrNewerBackup
	if resp, _ := e.do(t, "POST", "/backups/1/restore", "admin", url.Values{"confirm": {"1"}}); resp.StatusCode != 422 {
		t.Errorf("newer backup: %d", resp.StatusCode)
	}
	rs.err = errors.New("boom secret detail")
	if resp, body := e.do(t, "POST", "/backups/1/restore", "admin", url.Values{"confirm": {"1"}}); resp.StatusCode != 500 || strings.Contains(body, "boom") {
		t.Errorf("internal error must not leak: %d %q", resp.StatusCode, body)
	}
	// Only a config dump can be restored.
	if resp, _ := e.do(t, "GET", "/backups/3/restore", "admin", nil); resp.StatusCode != 404 {
		t.Errorf("decoded backup restore page: %d", resp.StatusCode)
	}
}

// Why: the job page polls while the restore runs and stops when it ends.
func TestRestoreJobPagePollsUntilTerminal(t *testing.T) {
	e, _, rs := newDiffEnv(t)
	rs.jobs = map[store.RestoreID]restore.Job{
		1: {ID: 1, DeviceID: 1, BackupID: 1, Actor: "a", State: store.RestoreWaiting, CreatedAt: time.Now()},
		2: {ID: 2, DeviceID: 1, BackupID: 1, Actor: "a", State: store.RestoreDone, Verified: true, PreBackupID: 4, Detail: "restored and verified", CreatedAt: time.Now()},
	}
	_, running := e.do(t, "GET", "/restores/1", "admin", nil)
	if !strings.Contains(running, `hx-trigger="every 2s"`) {
		t.Errorf("running job does not poll:\n%s", running)
	}
	_, done := e.do(t, "GET", "/restores/2/status", "admin", nil)
	// finished: no polling, the backup taken before (#4) is named so the user can roll back, and the verification is stated
	if strings.Contains(done, "hx-trigger") || !strings.Contains(done, "Saved 18 Oct 2026, 11:00") || !strings.Contains(done, "Verified") {
		t.Errorf("finished job:\n%s", done)
	}
	if resp, _ := e.do(t, "GET", "/restores/2", "operator", nil); resp.StatusCode != 403 {
		t.Errorf("operator: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", "/restores/99", "admin", nil); resp.StatusCode != 404 {
		t.Errorf("unknown job: %d", resp.StatusCode)
	}
}

// Why: without the firmware listener (no FIRMWARE_BASE_URL) there is no
// restore; the pages must not exist rather than fail half way.
func TestRestoreRoutesAbsentWithoutService(t *testing.T) {
	e := setup(t)
	e.bk.list = []model.Backup{{ID: 5, DeviceID: 1, Kind: model.BackupConfigDump, Path: "backups/1/a.dmp"}}
	if resp, _ := e.do(t, "GET", "/backups/5/restore", "admin", nil); resp.StatusCode != 404 {
		t.Errorf("got %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", "/devices/1/drift", "operator", nil); resp.StatusCode != 404 {
		t.Errorf("drift without LiveSnapshotter: %d", resp.StatusCode)
	}
}

// Why: the throttle protects a single-threaded device; ten parallel requests
// must still produce exactly one device query (check and reservation are one step).
func TestDriftThrottleHoldsUnderConcurrentRequests(t *testing.T) {
	e, bk, _ := newDiffEnv(t)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.do(t, "GET", "/devices/1/drift", "operator", nil)
		}()
	}
	wg.Wait()
	bk.mu.Lock()
	n := bk.liveN
	bk.mu.Unlock()
	if n != 1 {
		t.Errorf("device queried %d times by 10 parallel requests, want 1", n)
	}
}

// Why: an over-restriction audit. A restore is an admin action behind
// the admin role and CSRF; a password step-up on top only got in the way. One
// confirmation names the risks, and nothing asks for a password.
func TestRestoreNeedsNoStepUpOnlyOneConfirmation(t *testing.T) {
	e, _, rs, stepped := newDiffEnvStep(t)
	stepped.Store(false) // would have blocked the restore before
	resp, body := e.do(t, "GET", "/backups/1/restore", "admin", nil)
	if resp.StatusCode != 200 || strings.Contains(body, `name="current"`) || !strings.Contains(body, `name="confirm"`) {
		t.Fatalf("confirm page:\n%s", body)
	}
	if strings.Count(body, `type="checkbox" name="`) != 1 {
		t.Errorf("want exactly one confirmation checkbox:\n%s", body)
	}
	if resp, _ := e.do(t, "POST", "/backups/1/restore", "admin", url.Values{"confirm": {"1"}}); resp.StatusCode != 303 || len(rs.jobs) != 1 {
		t.Errorf("start: %d, jobs %d", resp.StatusCode, len(rs.jobs))
	}
	if resp, _ := e.do(t, "POST", "/backups/1/restore/stepup", "admin", url.Values{"current": {"right"}}); resp.StatusCode != 404 && resp.StatusCode != 405 {
		t.Errorf("the step-up route must be gone: %d", resp.StatusCode)
	}
}

// Why: a restore the live checks refuse must not offer a form at all; the
// reason is shown instead, in a fixed sentence.
func TestRestoreConfirmPageShowsRefusalWithoutForm(t *testing.T) {
	e, _, rs := newDiffEnv(t)
	rs.assessErr = fmt.Errorf("%w: dial tcp 10.1.2.3: secret-detail", restore.ErrFamilyMismatch)
	resp, body := e.do(t, "GET", "/backups/1/restore", "admin", nil)
	if resp.StatusCode != 200 || strings.Contains(body, `name="confirm"`) || !strings.Contains(body, "another chip family") || strings.Contains(body, "secret-detail") {
		t.Errorf("%d\n%s", resp.StatusCode, body)
	}
	rs.assessErr = errors.New("dial tcp: secret-detail")
	if _, body := e.do(t, "GET", "/backups/1/restore", "admin", nil); strings.Contains(body, "secret-detail") || strings.Contains(body, `name="confirm"`) {
		t.Errorf("device error leaked or form shown:\n%s", body)
	}
}

// Why: the risks the live checks found are named inside the one confirmation
// (and travel as hidden fields), not asked for in a second round.
func TestRestoreConfirmNamesRisksUpFront(t *testing.T) {
	e, _, rs := newDiffEnv(t)
	rs.assess = restore.Assessment{Major: true, Family: true, MACUnknown: true, LiveMAC: "aa:bb:cc:00:00:01", Address: "10.0.0.1", Chip: "esp8266", Detail: "the backup is from firmware 15.6.0, the device runs 16.0.0"}
	_, body := e.do(t, "GET", "/backups/1/restore", "admin", nil)
	for _, want := range []string{`name="major_ok"`, `name="family_ok"`, "another firmware major version", "same chip family", "15.6.0", "Device at 10.0.0.1 reports MAC", "aa:bb:cc:00:00:01", "chip esp8266", "will be recorded as its identity", `name="expect_mac" value="aa:bb:cc:00:00:01"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Count(body, `type="checkbox" name="`) != 1 {
		t.Error("risks must not add checkboxes")
	}
}

// Why: L3. The page must say plainly that old credentials come back.
func TestRestorePageWarnsAboutOldCredentials(t *testing.T) {
	e, _, _ := newDiffEnv(t)
	_, body := e.do(t, "GET", "/backups/1/restore", "admin", nil)
	if !strings.Contains(body, "Old credentials come back") || !strings.Contains(body, "plain HTTP") {
		t.Errorf("warning missing:\n%s", body)
	}
	if !strings.Contains(body, "Secret values differ") {
		t.Error("a preview with changed secrets must say so")
	}
}

// Why: L4. A risk the service cannot rule out is waived by the admin's own
// tick, per risk, and the flags reach the service.
func TestRestoreOverridesAreOfferedPerRiskAndPassedOn(t *testing.T) {
	e, _, rs := newDiffEnv(t)
	rs.need = &restore.OverrideError{Major: true, Family: true, Detail: "the backup is from firmware 15.6.0, the device runs 16.0.0"}
	resp, body := e.do(t, "POST", "/backups/1/restore", "admin", url.Values{"confirm": {"1"}})
	if resp.StatusCode != 422 || !strings.Contains(body, `name="major_ok"`) || !strings.Contains(body, `name="family_ok"`) {
		t.Fatalf("override prompt: %d\n%s", resp.StatusCode, body)
	}
	rs.need = &restore.OverrideError{Major: true}
	resp, _ = e.do(t, "POST", "/backups/1/restore", "admin", url.Values{"confirm": {"1"}, "major_ok": {"1"}, "family_ok": {"1"}})
	if resp.StatusCode != 303 {
		t.Fatalf("with confirmations: %d", resp.StatusCode)
	}
	if rs.opts[len(rs.opts)-1].ExpectMAC != "" {
		t.Errorf("expect_mac must come from the form, not be invented")
	}
	last := rs.opts[len(rs.opts)-1]
	if !last.AllowMajorChange || !last.AllowUnknownFamily {
		t.Errorf("options: %+v", last)
	}
}

// Why: the refusal sentences are fixed texts; device error text never reaches the page.
func TestRestoreRefusalsUseFixedTexts(t *testing.T) {
	e, _, rs := newDiffEnv(t)
	for err, want := range map[error]string{
		restore.ErrIdentity:       "not the device in the inventory",
		restore.ErrFamilyMismatch: "another chip family",
		restore.ErrVersionUnknown: "unknown",
	} {
		rs.err = fmt.Errorf("%w: dial tcp 10.1.2.3: secret-detail", err)
		resp, body := e.do(t, "POST", "/backups/1/restore", "admin", url.Values{"confirm": {"1"}})
		if resp.StatusCode != 422 || !strings.Contains(body, want) || strings.Contains(body, "secret-detail") {
			t.Errorf("%v: %d\n%s", err, resp.StatusCode, body)
		}
	}
}

// Why (spec 12.4): the admin must see how the restore reaches the device. Over MQTT the page
// says so, and it no longer claims a plain HTTP download; a refusal for lack of any path names
// the reason.
func TestRestorePageShowsThePathAndRefusesWithoutOne(t *testing.T) {
	e, _, rs := newDiffEnv(t)
	rs.assess = restore.Assessment{ViaMQTT: true, PathNote: "over MQTT (FileUpload), because the device's HTTP API refuses connections"}
	resp, body := e.do(t, "GET", "/backups/1/restore", "admin", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "Over MQTT (FileUpload), because the device") || !strings.Contains(body, "travels over your MQTT broker") || strings.Contains(body, "plain HTTP") {
		t.Errorf("%d\n%s", resp.StatusCode, body)
	}
	rs.assess = restore.Assessment{}
	rs.assessErr = fmt.Errorf("%w: HTTP is unreachable; the device has no MQTT password", restore.ErrNoPath)
	_, body = e.do(t, "GET", "/backups/1/restore", "admin", nil)
	if strings.Contains(body, `name="confirm"`) || !strings.Contains(body, "no way to restore this device") || !strings.Contains(body, "no MQTT password") {
		t.Errorf("no path must refuse with its reason:\n%s", body)
	}
}

// Why (H1): the lockout risk of a restore over MQTT is named in the one confirmation and travels
// as a hidden field; the start passes the admin's acceptance on to the service.
func TestRestoreConfirmNamesTheMQTTLockout(t *testing.T) {
	e, _, rs := newDiffEnv(t)
	rs.assess = restore.Assessment{ViaMQTT: true, PathNote: "over MQTT (FileUpload)", Lockout: true, Detail: "this backup changes Topic; TasmoFleet reaches the device only over MQTT and may lose it"}
	_, body := e.do(t, "GET", "/backups/1/restore", "admin", nil)
	for _, want := range []string{`name="lockout_ok"`, "changes Topic", "may lose it"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if !strings.Contains(body, "which TasmoFleet reaches only over MQTT, and TasmoFleet may lose it") {
		t.Error("the acknowledgement must name the risk")
	}
	rs.opts = nil
	e.do(t, "POST", "/backups/1/restore", "admin", url.Values{"confirm": {"1"}, "lockout_ok": {"1"}})
	if len(rs.opts) != 1 || !rs.opts[0].AllowMQTTLockout {
		t.Errorf("options %+v", rs.opts)
	}
}

// filesAssess is an assessment whose backup holds three files and lists one it does not hold.
func filesAssess() restore.Assessment {
	return restore.Assessment{Files: restore.FilesPlan{
		Files: []restore.PlannedFile{
			{Path: "/autoexec.be", Size: 1200},
			{Path: "/lib/heating.be", Size: 8600},
			{Path: "/tlskey", Size: 1200, PrivateKey: true},
		},
		Bytes:     11000,
		NotStored: []backup.ManifestSkip{{Path: "/floorplan.jpg", Size: 1700000, Reason: "larger than 1 MiB"}},
	}}
}

// Why (#324): the person confirms with the file list in front of them: what is written first,
// which one is the private key, what stays because the backup does not hold it, and that nothing
// is deleted. The acknowledgement names the file count, because it is what they agree to.
func TestRestoreConfirmShowsTheFilesThatAreWrittenFirst(t *testing.T) {
	e, _, rs := newDiffEnv(t)
	rs.assess = filesAssess()
	_, body := e.do(t, "GET", "/backups/1/restore", "admin", nil)
	for _, want := range []string{"Files written first (3 files, 10.7 KiB)", "autoexec.be", "lib/heating.be", "Private key", "floorplan.jpg",
		"Not in the backup", "Nothing on the device is deleted", "its 3 files are written", "The 3 files are written to the device", "cstacks-2", "· 3 files"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, "/autoexec.be") {
		t.Error("file names are shown as Tasmota's file manager shows them, without the leading slash")
	}
}

// Why (#324): a restore that cannot write the files (MQTT, no file system) must say so on the
// page before the confirmation and must not word the acknowledgement as if files were written.
func TestRestoreConfirmSaysWhenTheFilesAreNotRestored(t *testing.T) {
	e, _, rs := newDiffEnv(t)
	a := filesAssess()
	a.Files.Why = "files need HTTP, and this restore goes over MQTT, so only the settings are restored"
	rs.assess = a
	_, body := e.do(t, "GET", "/backups/1/restore", "admin", nil)
	if !strings.Contains(body, "Files need HTTP, and this restore goes over MQTT") || !strings.Contains(body, "not restored") {
		t.Errorf("the reason is missing:\n%s", body)
	}
	if strings.Contains(body, "are written (files of the same name") || strings.Contains(body, "Files written first") {
		t.Error("the page promises files that will not be written")
	}
}

// Why (#324): the progress page shows the five steps and, beside them, which file the restore is
// at, so a long files step is not a frozen page.
func TestRestoreJobPageListsTheFilesWhileTheyAreWritten(t *testing.T) {
	e, _, rs := newDiffEnv(t)
	rs.jobs = map[store.RestoreID]restore.Job{
		1: {ID: 1, DeviceID: 1, BackupID: 1, Actor: "a", State: store.RestoreFiles, Detail: "writing lib/heating.be (2 of 3)", PreBackupID: 4, CreatedAt: time.Now()},
	}
	rs.progress = map[store.RestoreID]restore.FilesProgress{1: {Files: []restore.FileProgress{
		{PlannedFile: restore.PlannedFile{Path: "/autoexec.be", Size: 1200}, State: restore.FileWritten},
		{PlannedFile: restore.PlannedFile{Path: "/lib/heating.be", Size: 8600}, State: restore.FileWriting},
		{PlannedFile: restore.PlannedFile{Path: "/tlskey", Size: 1200, PrivateKey: true}, State: restore.FileWaiting},
	}, Writes: 1, NotStored: []backup.ManifestSkip{{Path: "/floorplan.jpg", Size: 1700000, Reason: "larger than 1 MiB"}}}}
	_, body := e.do(t, "GET", "/restores/1/status", "admin", nil)
	for _, want := range []string{"Write the files", "Verify settings and files", "Writing lib/heating.be (2 of 3)", "1 of 3 written", "Written", "Writing", "Waiting", "Private key", "floorplan.jpg", `hx-trigger="every 2s"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q\n%s", want, body)
		}
	}
	if n := strings.Count(body, `class="rs-step `); n != 5 {
		t.Errorf("%d steps, want 5", n)
	}
}

// Why (#324): the finished job keeps what the files step did (and the flash write count) in its
// Detail, so it is on the page after TasmoFleet restarted and the in-memory file list is gone.
func TestFinishedRestoreShowsTheFilesSummaryWithoutProgress(t *testing.T) {
	e, bk, rs := newDiffEnv(t)
	// the backup taken before the restore holds the device's files too: they can be put back
	bk.list = append(bk.list, model.Backup{ID: 7, DeviceID: 1, Kind: model.BackupFiles, TakenAt: bk.list[0].TakenAt, Version: "15.6.0"})
	rs.jobs = map[store.RestoreID]restore.Job{
		2: {ID: 2, DeviceID: 1, BackupID: 1, Actor: "a", State: store.RestoreDone, Verified: true, PreBackupID: 4, CreatedAt: time.Now(),
			Detail: "3 files written (3 flash writes)\nrestored; the device's configuration dump is identical to the backup; the 3 files match the backup"},
		3: {ID: 3, DeviceID: 1, BackupID: 1, Actor: "a", State: store.RestoreDone, Verified: true, PreBackupID: 4, CreatedAt: time.Now(), Detail: "restored; identical"},
	}
	_, body := e.do(t, "GET", "/restores/2/status", "admin", nil)
	for _, want := range []string{"3 files written (3 flash writes)", "the 3 files match the backup", "settings and files from", "4.0 KiB and its files"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q\n%s", want, body)
		}
	}
	if strings.Contains(body, "rs-filelist") {
		t.Error("no per-file list without progress")
	}
	_, plain := e.do(t, "GET", "/restores/3/status", "admin", nil)
	if !strings.Contains(plain, "No files in this backup") || strings.Contains(plain, "settings and files from") {
		t.Errorf("a restore without files:\n%s", plain)
	}
}

// Why (#324): a files failure after the first write changed the device, so the page offers the
// rollback; one before it changed nothing and says so.
func TestFilesFailureOffersRollbackOnlyAfterAWrite(t *testing.T) {
	e, _, rs := newDiffEnv(t)
	rs.jobs = map[store.RestoreID]restore.Job{
		1: {ID: 1, DeviceID: 1, BackupID: 1, Actor: "a", State: store.RestoreFailed, PreBackupID: 4, CreatedAt: time.Now(), Detail: "files partly written, settings unchanged: lib/a.be: link lost; lib/a.be was put back as it was"},
		2: {ID: 2, DeviceID: 1, BackupID: 1, Actor: "a", State: store.RestoreFailed, PreBackupID: 4, CreatedAt: time.Now(), Detail: "the files could not be written, the device is unchanged: device busy"},
	}
	_, partly := e.do(t, "GET", "/restores/1/status", "admin", nil)
	if !strings.Contains(partly, "/backups/4/restore") || !strings.Contains(partly, "roll back") {
		t.Errorf("partly written must offer the rollback:\n%s", partly)
	}
	_, none := e.do(t, "GET", "/restores/2/status", "admin", nil)
	if strings.Contains(none, "/backups/4/restore") || !strings.Contains(none, "Nothing was changed") {
		t.Errorf("an unchanged device must not offer a rollback:\n%s", none)
	}
}

// Why (M3): the confirmation carries the plan it showed (the number of files written first) and
// the start passes exactly that on; a form without it restores the settings only.
func TestRestoreConfirmationIsBoundToTheShownFilePlan(t *testing.T) {
	e, _, rs := newDiffEnv(t)
	rs.assess = filesAssess()
	_, body := e.do(t, "GET", "/backups/1/restore", "admin", nil)
	if !strings.Contains(body, `name="files" value="3"`) {
		t.Errorf("the form must carry the file count the page showed:\n%s", body)
	}
	e.do(t, "POST", "/backups/1/restore", "admin", url.Values{"confirm": {"1"}, "files": {"3"}})
	e.do(t, "POST", "/backups/1/restore", "admin", url.Values{"confirm": {"1"}})
	if len(rs.opts) != 2 || rs.opts[0].ConfirmedFiles != 3 || rs.opts[1].ConfirmedFiles != 0 {
		t.Errorf("options: %+v", rs.opts)
	}
	a := filesAssess()
	a.Files.Why = "files need HTTP, and this restore goes over MQTT, so only the settings are restored"
	rs.assess = a
	_, body = e.do(t, "GET", "/backups/1/restore", "admin", nil)
	if !strings.Contains(body, `name="files" value="0"`) {
		t.Errorf("a plan that does not write files confirms 0:\n%s", body)
	}
}

// Why (M3): a reason that stops the files must be on the page even when the file list itself is
// unknown (a damaged archive has no list): the person is told, not left to guess.
func TestRestoreConfirmShowsTheReasonWhenThereIsNoFileList(t *testing.T) {
	e, _, rs := newDiffEnv(t)
	rs.assess = restore.Assessment{Files: restore.FilesPlan{Why: "the files archive of this backup is damaged or missing, so only the settings are restored"}}
	_, body := e.do(t, "GET", "/backups/1/restore", "admin", nil)
	if !strings.Contains(body, "The files archive of this backup is damaged or missing") {
		t.Errorf("missing the reason:\n%s", body)
	}
}

// Why (H2): "so you can undo this" must not promise more than the safety backup holds: files it
// cannot hold are not overwritten, and the page says so.
func TestRestoreConfirmDoesNotPromiseMoreThanTheSafetyBackupHolds(t *testing.T) {
	e, _, rs := newDiffEnv(t)
	rs.assess = filesAssess()
	_, body := e.do(t, "GET", "/backups/1/restore", "admin", nil)
	if !strings.Contains(body, "a file that backup cannot hold is not overwritten") || !strings.Contains(body, "files the device did not have before stay on it after a rollback") {
		t.Errorf("the undo sentence must be conditional:\n%s", body)
	}
}

// Why (M4): while the later steps run, Detail holds their status text; the files line comes from
// the progress, so a finished files step does not read "No files in this backup".
func TestFilesLineStaysWhileLaterStepsRun(t *testing.T) {
	e, _, rs := newDiffEnv(t)
	rs.jobs = map[store.RestoreID]restore.Job{
		1: {ID: 1, DeviceID: 1, BackupID: 1, Actor: "a", State: store.RestoreWaiting, Detail: "waiting for the download and the reboot", PreBackupID: 4, CreatedAt: time.Now()},
	}
	rs.progress = map[store.RestoreID]restore.FilesProgress{1: {Summary: "3 files written (3 flash writes)", Files: []restore.FileProgress{
		{PlannedFile: restore.PlannedFile{Path: "/autoexec.be", Size: 1}, State: restore.FileWritten}}}}
	_, body := e.do(t, "GET", "/restores/1/status", "admin", nil)
	if !strings.Contains(body, "3 files written (3 flash writes)") || strings.Contains(body, "No files in this backup") {
		t.Errorf("files line lost:\n%s", body)
	}
}
