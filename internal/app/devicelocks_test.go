package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/devlock"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/settings"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

type fakeActiveJobs struct {
	store.JobStore
	active map[model.DeviceID]bool
	batch  map[model.BatchID][]model.Job // jobs per batch, for retries
}

func (f fakeActiveJobs) ListJobs(_ context.Context, fl store.JobFilter) ([]model.Job, error) {
	if fl.BatchID != 0 {
		return f.batch[fl.BatchID], nil
	}
	if f.active[fl.DeviceID] {
		return []model.Job{{DeviceID: fl.DeviceID, State: model.JobAwaitReboot}}, nil
	}
	return nil, nil
}

type countingCmd struct{ calls int }

func (c *countingCmd) Power(context.Context, string, model.DeviceID, int, string) (string, error) {
	c.calls++
	return "ON", nil
}
func (c *countingCmd) Run(context.Context, model.Principal, model.DeviceID, string) (json.RawMessage, error) {
	c.calls++
	return json.RawMessage(`{}`), nil
}

type countingSettings struct{ writes, reads int }

func (c *countingSettings) Read(context.Context, model.DeviceID) (map[string]string, error) {
	c.reads++
	return nil, nil
}
func (c *countingSettings) Apply(context.Context, string, model.DeviceID, map[string]string) ([]settings.Result, error) {
	c.writes++
	return nil, nil
}
func (c *countingSettings) Save(context.Context, string, model.DeviceID, string) (settings.Result, error) {
	c.writes++
	return settings.Result{}, nil
}

type countingOTA struct{ submits, retries int }

func (c *countingOTA) Preview(context.Context, []ota.Request) ([]ota.PreviewItem, error) {
	return nil, nil
}
func (c *countingOTA) Submit(context.Context, string, []ota.Request) (model.BatchID, error) {
	c.submits++
	return 1, nil
}
func (c *countingOTA) SubmitRetry(context.Context, string, model.BatchID, []model.DeviceID) (ota.RetryResult, error) {
	c.retries++
	return ota.RetryResult{Batch: 2}, nil
}
func (c *countingOTA) Cancel(context.Context, string, model.JobID) error        { return nil }
func (c *countingOTA) CancelBatch(context.Context, string, model.BatchID) error { return nil }

func busyHolderOf(t *testing.T, err error) string {
	t.Helper()
	var b devlock.BusyError
	if !errors.Is(err, devlock.ErrBusy) || !errors.As(err, &b) {
		t.Fatalf("want BusyError, got %v", err)
	}
	return b.Holder
}

// Why (restore.md L5): a restore, an OTA job, a settings write and a console
// command change the device and may reboot it; two at once corrupt both. All
// writers share one lock per device and refuse instead of queueing.
func TestWritersExcludeEachOther(t *testing.T) {
	ctx := context.Background()
	locks := devlock.New()
	g := deviceGuard{locks: locks, jobs: fakeActiveJobs{active: map[model.DeviceID]bool{}}}
	cmd, set, up := &countingCmd{}, &countingSettings{}, &countingOTA{}
	lc, ls, lo := lockedDeviceCmd{DeviceCmd: cmd, g: g}, lockedSettings{SettingsService: set, g: g}, lockedOTA{OTA: up, g: g}

	release, _, ok := locks.TryAcquire(1, "restore")
	if !ok {
		t.Fatal("setup")
	}
	if _, err := lc.Power(ctx, "u", 1, 1, "ON"); busyHolderOf(t, err) != "restore" {
		t.Fatal("power during restore")
	}
	if _, err := lc.Run(ctx, model.Principal{}, 1, "Status 2"); busyHolderOf(t, err) != "restore" {
		t.Fatal("console during restore")
	}
	if _, err := ls.Apply(ctx, "u", 1, map[string]string{"TelePeriod": "60"}); busyHolderOf(t, err) != "restore" {
		t.Fatal("settings during restore")
	}
	if _, err := lo.Submit(ctx, "u", []ota.Request{{DeviceID: 2}, {DeviceID: 1}}); busyHolderOf(t, err) != "restore" {
		t.Fatal("OTA submit during restore")
	}
	if _, err := ls.Read(ctx, 1); err != nil {
		t.Fatal("reads must not be locked")
	}
	if cmd.calls+set.writes+up.submits != 0 {
		t.Fatalf("a refused writer reached the device: %d %d %d", cmd.calls, set.writes, up.submits)
	}
	release()
	// Submit released the lock it took on device 2 even though it failed.
	if _, err := lc.Power(ctx, "u", 2, 1, "ON"); err != nil {
		t.Fatalf("lock of device 2 leaked: %v", err)
	}
	if _, err := lo.Submit(ctx, "u", []ota.Request{{DeviceID: 1}}); err != nil || up.submits != 1 {
		t.Fatalf("submit after release: %v", err)
	}
	if _, err := lc.Power(ctx, "u", 1, 1, "ON"); err != nil {
		t.Fatalf("lock not released after submit: %v", err)
	}
}

// While an OTA job is active (between Submit and its end) no other writer may
// touch the device, even though the job does not hold the lock.
func TestActiveOTAJobBlocksWriters(t *testing.T) {
	ctx := context.Background()
	g := deviceGuard{locks: devlock.New(), jobs: fakeActiveJobs{active: map[model.DeviceID]bool{1: true}}}
	cmd, set := &countingCmd{}, &countingSettings{}
	if _, err := (lockedDeviceCmd{DeviceCmd: cmd, g: g}).Power(ctx, "u", 1, 1, "TOGGLE"); busyHolderOf(t, err) != "ota" {
		t.Fatal("power during OTA")
	}
	if _, err := (lockedSettings{SettingsService: set, g: g}).Save(ctx, "u", 1, "1"); busyHolderOf(t, err) != "ota" {
		t.Fatal("settings during OTA")
	}
	if cmd.calls+set.writes != 0 {
		t.Fatal("a writer reached the device during OTA")
	}
	// The check released the lock: another device is unaffected, and this
	// device is free again once the job ends.
	if _, err := (lockedDeviceCmd{DeviceCmd: cmd, g: g}).Power(ctx, "u", 2, 1, "ON"); err != nil {
		t.Fatal(err)
	}
}

// Why: two firmware updates at once can brick a device. A typed Upgrade/OtaUrl in the console (a bulk
// command job sends through a.deviceCmd and is excluded by the job that holds the device) while a TasmoFleet update job holds the device never reaches it, and the refusal says
// why instead of "busy". A routine command keeps the generic busy text, and nothing is sent either way.
func TestFirmwareCommandRefusedWhileAnUpdateJobHoldsTheDevice(t *testing.T) {
	ctx := context.Background()
	g := deviceGuard{locks: devlock.New(), jobs: fakeActiveJobs{active: map[model.DeviceID]bool{1: true}}}
	cmd := &countingCmd{}
	lc := lockedDeviceCmd{DeviceCmd: cmd, g: g}
	for _, c := range []string{"Upgrade 1", "OtaUrl http://x/y.bin", "Backlog OtaUrl http://x/y.bin; Upgrade 1"} {
		if _, err := lc.Run(ctx, model.Principal{Role: model.RoleAdmin}, 1, c); !errors.Is(err, cmdpolicy.ErrUpdateJob) {
			t.Errorf("%q: want ErrUpdateJob, got %v", c, err)
		}
	}
	if _, err := lc.Run(ctx, model.Principal{Role: model.RoleAdmin}, 1, "Status 2"); busyHolderOf(t, err) != "ota" {
		t.Error("a routine command keeps the generic busy refusal")
	}
	if _, err := lc.Run(ctx, model.Principal{Role: model.RoleAdmin}, 2, "Upgrade 1"); err != nil {
		t.Errorf("another device is free: %v", err)
	}
	if _, err := lc.Run(ctx, model.Principal{Role: model.RoleOperator}, 1, "Upgrade 1"); errors.Is(err, cmdpolicy.ErrUpdateJob) {
		t.Error("an operator may not send it at all: no text that implies it will work later")
	}
	if cmd.calls != 1 {
		t.Fatalf("only the free device's command may reach the device, got %d", cmd.calls)
	}
}

// Why: a retry is an OTA submit for the failed devices of a batch; it takes
// their device locks like Submit, and only those of the devices it would
// retry (selected, failed or cancelled), so a restore running on one of them
// keeps the retry out, while a busy succeeded device does not matter.
func TestRetryTakesTheLocksOfTheRetriedDevices(t *testing.T) {
	ctx := context.Background()
	locks := devlock.New()
	jobs := fakeActiveJobs{active: map[model.DeviceID]bool{}, batch: map[model.BatchID][]model.Job{
		7: {{DeviceID: 1, State: model.JobFailed}, {DeviceID: 2, State: model.JobSucceeded}, {DeviceID: 3, State: model.JobCancelled}},
	}}
	up := &countingOTA{}
	lo := lockedOTA{OTA: up, g: deviceGuard{locks: locks, jobs: jobs}}

	release, _, _ := locks.TryAcquire(3, "restore")
	if _, err := lo.SubmitRetry(ctx, "u", 7, nil); busyHolderOf(t, err) != "restore" || up.retries != 0 {
		t.Fatalf("retry during a restore of a retried device: %v", err)
	}
	if _, err := lo.SubmitRetry(ctx, "u", 7, []model.DeviceID{1}); err != nil || up.retries != 1 {
		t.Fatalf("retry of an unaffected selection: %v", err)
	}
	release()
	rel2, _, _ := locks.TryAcquire(2, "restore") // device 2 succeeded: not retried, its lock does not matter
	defer rel2()
	if _, err := lo.SubmitRetry(ctx, "u", 7, nil); err != nil || up.retries != 2 {
		t.Fatalf("retry: %v", err)
	}
	if _, _, ok := locks.TryAcquire(1, "x"); !ok {
		t.Fatal("the lock of device 1 leaked")
	}
}

// Why: while a job switches relays back on (restore_power) the device is
// still being written by the OTA job; every non-terminal state must count as
// active, or a console command could slip in between.
func TestEveryRunningJobStateExcludesOtherWriters(t *testing.T) {
	for _, s := range []model.JobState{model.JobQueued, model.JobPreflight, model.JobBackup, model.JobSetOtaURL, model.JobUpgradeIssued,
		model.JobAwaitReboot, model.JobVerify, model.JobRestoreOtaURL, model.JobRestorePower} {
		found := false
		for _, a := range activeStates {
			found = found || a == s
		}
		if !found {
			t.Errorf("state %s does not count as an active OTA job for the device guard", s)
		}
	}
}

// Why: the specific text names an UPDATE job. A command, backup or policy job holding the device is not
// one, so it must keep the generic busy text; otherwise the message would blame an update that is not there.
func TestFirmwareCommandDuringANonUpdateJobKeepsTheGenericBusyText(t *testing.T) {
	for _, k := range []model.JobKind{model.KindCommand, model.KindBackup, model.KindTemplate} {
		jobs := kindJobs{kind: k}
		g := deviceGuard{locks: devlock.New(), jobs: jobs}
		_, err := (lockedDeviceCmd{DeviceCmd: &countingCmd{}, g: g}).Run(context.Background(), model.Principal{Role: model.RoleAdmin}, 1, "Upgrade 1")
		if errors.Is(err, cmdpolicy.ErrUpdateJob) || !errors.Is(err, devlock.ErrBusy) {
			t.Errorf("%s job: want the generic busy text, got %v", k, err)
		}
	}
}

// kindJobs reports one active job of the given kind for every device.
type kindJobs struct {
	store.JobStore
	kind model.JobKind
}

func (k kindJobs) ListJobs(_ context.Context, fl store.JobFilter) ([]model.Job, error) {
	return []model.Job{{DeviceID: fl.DeviceID, Kind: k.kind, State: model.JobQueued}}, nil
}

// Why: a GPIO template job rewrites the pin map and restarts the device. A console command
// arriving meanwhile must be refused as "template", not blamed on a firmware update.
func TestActiveTemplateJobBlocksWritersAsTemplate(t *testing.T) {
	g := deviceGuard{locks: devlock.New(), jobs: kindJobs{kind: model.KindTemplate}}
	_, err := (lockedDeviceCmd{DeviceCmd: &countingCmd{}, g: g}).Power(context.Background(), "u", 1, 1, "TOGGLE")
	if busyHolderOf(t, err) != "template" {
		t.Fatalf("holder = %v", err)
	}
}
