package app

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/devlock"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/settings"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/web"
)

// Device write exclusion (docs/security/restore.md, L5). Protocol for every
// writer: take the device lock, then make sure no OTA job is active for the
// device, then write, then release. OTA Submit takes the locks of all its
// devices while it creates the jobs; from then on the active job itself
// excludes the others (restore checks ActiveJobs, console/settings check
// below). So restore, console/power, settings and OTA never overlap.
type deviceGuard struct {
	locks *devlock.Locks
	jobs  store.JobStore
	// holds is the set of devices on which a typed firmware command has just started an update (#347); nil
	// holds nothing. Update starts are refused for those until they have restarted.
	holds *devlock.FirmwareHolds
}

var activeStates = []model.JobState{
	model.JobQueued, model.JobPreflight, model.JobBackup, model.JobSetOtaURL, model.JobSafebootSwitch, model.JobSafebootRecover, model.JobUpgradeIssued,
	model.JobAwaitReboot, model.JobVerify, model.JobRestoreOtaURL, model.JobRestorePower, model.JobApplying,
}

// enter takes the device lock for owner and refuses while an OTA job is
// active for the device. The caller must call the returned release.
func (g deviceGuard) enter(ctx context.Context, id model.DeviceID, owner string) (func(), error) {
	release, holder, ok := g.locks.TryAcquire(id, owner)
	if !ok {
		return nil, devlock.BusyError{Holder: holder}
	}
	jobs, err := g.jobs.ListJobs(ctx, store.JobFilter{DeviceID: id, States: activeStates, Limit: 1})
	if err != nil {
		release()
		return nil, err
	}
	if len(jobs) > 0 {
		release()
		if jobs[0].IsPolicy() {
			return nil, devlock.BusyError{Holder: "policy"}
		}
		if jobs[0].IsCommand() {
			return nil, devlock.BusyError{Holder: "command"}
		}
		if jobs[0].IsBackup() {
			return nil, devlock.BusyError{Holder: "backup"}
		}
		if jobs[0].IsTemplate() {
			return nil, devlock.BusyError{Holder: "template"}
		}
		return nil, devlock.BusyError{Holder: "ota"}
	}
	return release, nil
}

// lockedDeviceCmd serialises console commands and power toggles with the
// other writers.
type lockedDeviceCmd struct {
	web.DeviceCmd
	g deviceGuard
}

func (l lockedDeviceCmd) Power(ctx context.Context, actor string, id model.DeviceID, relay int, action string) (string, error) {
	release, err := l.g.enter(ctx, id, "power")
	if err != nil {
		return "", err
	}
	defer release()
	return l.DeviceCmd.Power(ctx, actor, id, relay, action)
}

func (l lockedDeviceCmd) Run(ctx context.Context, p model.Principal, id model.DeviceID, cmd string) (json.RawMessage, error) {
	release, err := l.g.enter(ctx, id, "console")
	if err != nil {
		if p.Role >= model.RoleAdmin && cmdpolicy.StartsFirmware(cmd) && busyBy(err, "ota") { // an operator may not send it at all: keep the generic text
			return nil, cmdpolicy.ErrUpdateJob // the same decision as the console page: say why, not just "busy"
		}
		return nil, err
	}
	defer release()
	return l.DeviceCmd.Run(ctx, p, id, cmd)
}

// busyBy reports whether err says the device is held by holder.
func busyBy(err error, holder string) bool {
	var b devlock.BusyError
	return errors.As(err, &b) && b.Holder == holder
}

// lockedSettings serialises settings writes; reads are not locked.
type lockedSettings struct {
	web.SettingsService
	g deviceGuard
}

func (l lockedSettings) Apply(ctx context.Context, actor string, id model.DeviceID, changes map[string]string) ([]settings.Result, error) {
	release, err := l.g.enter(ctx, id, "settings")
	if err != nil {
		return nil, err
	}
	defer release()
	return l.SettingsService.Apply(ctx, actor, id, changes)
}

func (l lockedSettings) Save(ctx context.Context, actor string, id model.DeviceID, saveData string) (settings.Result, error) {
	release, err := l.g.enter(ctx, id, "settings")
	if err != nil {
		return settings.Result{}, err
	}
	defer release()
	return l.SettingsService.Save(ctx, actor, id, saveData)
}

// lockedOTA holds the locks of all requested devices while the batch is
// created, so a restore or settings write cannot start in between; the
// active jobs exclude them afterwards.
type lockedOTA struct {
	web.OTA
	g deviceGuard
}

// Preview marks a device that a typed firmware command holds as refused, with the reason, so the person
// sees it before confirming; Submit refuses it too.
func (l lockedOTA) Preview(ctx context.Context, reqs []ota.Request) ([]ota.PreviewItem, error) {
	items, err := l.OTA.Preview(ctx, reqs)
	if err != nil || l.g.holds == nil {
		return items, err
	}
	ids := make([]model.DeviceID, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.DeviceID)
	}
	reasons := l.g.heldReasons(ids)
	for i, it := range items {
		if r, ok := reasons[it.DeviceID]; ok {
			items[i].Decision, items[i].Reason = "refuse", r
			items[i].BuildChoice, items[i].LanguageChoice = false, false // a choice cannot lift this refusal
		}
	}
	return items, nil
}

func (l lockedOTA) Submit(ctx context.Context, actor string, reqs []ota.Request) (model.BatchID, error) {
	var releases []func()
	defer func() {
		for _, r := range releases {
			r()
		}
	}()
	seen := map[model.DeviceID]bool{}
	for _, r := range reqs {
		if seen[r.DeviceID] {
			continue // the engine refuses duplicates itself
		}
		seen[r.DeviceID] = true
		release, holder, ok := l.g.locks.TryAcquire(r.DeviceID, "ota")
		if !ok {
			return 0, devlock.BusyError{Holder: holder}
		}
		releases = append(releases, release)
	}
	// Checked with the locks held: a console command that holds a device records its hold before it lets go,
	// so a hold cannot appear between this check and the batch.
	ids := make([]model.DeviceID, 0, len(reqs))
	for _, r := range reqs {
		ids = append(ids, r.DeviceID)
	}
	if err := l.g.refuseHeld(ids); err != nil {
		return 0, err // like any refusal, all or nothing: the person sees which device and why
	}
	return l.OTA.Submit(ctx, actor, reqs)
}

// SubmitRetry takes the locks of the devices that would be retried (the
// selected ones, or every failed or cancelled device of the batch) while the
// retry batch is created, like Submit does.
func (l lockedOTA) SubmitRetry(ctx context.Context, actor string, batch model.BatchID, devices []model.DeviceID) (ota.RetryResult, error) {
	jobs, err := l.g.jobs.ListJobs(ctx, store.JobFilter{BatchID: batch})
	if err != nil {
		return ota.RetryResult{}, err
	}
	want := map[model.DeviceID]bool{}
	for _, d := range devices {
		want[d] = true
	}
	var retried []model.DeviceID
	for _, j := range jobs {
		if (len(want) == 0 || want[j.DeviceID]) && (j.State == model.JobFailed || j.State == model.JobCancelled) {
			retried = append(retried, j.DeviceID)
		}
	}
	var releases []func()
	defer func() {
		for _, r := range releases {
			r()
		}
	}()
	for _, j := range jobs {
		if (len(want) > 0 && !want[j.DeviceID]) || (j.State != model.JobFailed && j.State != model.JobCancelled) {
			continue // the engine refuses what is not retryable
		}
		release, holder, ok := l.g.locks.TryAcquire(j.DeviceID, "ota")
		if !ok {
			return ota.RetryResult{}, devlock.BusyError{Holder: holder}
		}
		releases = append(releases, release)
	}
	if err := l.g.refuseHeld(retried); err != nil { // with the locks held, like Submit
		return ota.RetryResult{}, err
	}
	return l.OTA.SubmitRetry(ctx, actor, batch, devices)
}

// lockedPolicyFix starts "Make compliant" transactions. It takes the lock of
// every device while the jobs are created, like lockedOTA; a device somebody
// else holds is left out with the holder named, the others still start.
type lockedPolicyFix struct {
	e *ota.Engine
	g deviceGuard
}

func (l lockedPolicyFix) Submit(ctx context.Context, actor string, reqs []ota.PolicyRequest) (model.BatchID, []ota.Refusal, error) {
	var releases []func()
	defer func() {
		for _, r := range releases {
			r()
		}
	}()
	var free []ota.PolicyRequest
	var skipped []ota.Refusal
	seen := map[model.DeviceID]bool{}
	for _, r := range reqs {
		if seen[r.DeviceID] {
			free = append(free, r) // the engine reports the duplicate itself
			continue
		}
		seen[r.DeviceID] = true
		release, holder, ok := l.g.locks.TryAcquire(r.DeviceID, "policy")
		if !ok {
			skipped = append(skipped, ota.Refusal{DeviceID: r.DeviceID, Reason: devlock.BusyError{Holder: holder}.Error()})
			continue
		}
		releases = append(releases, release)
		free = append(free, r)
	}
	if len(free) == 0 {
		return 0, skipped, ota.ErrNothingToFix
	}
	id, more, err := l.e.SubmitPolicy(ctx, actor, free)
	return id, append(skipped, more...), err
}
