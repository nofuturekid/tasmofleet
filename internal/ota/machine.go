// Package ota runs firmware updates as an explicit, persisted state machine.
//
// machine.go is pure: Step(job, event, now, cfg) returns the next job value
// and the actions to perform. It never does I/O, never reads the clock, and
// is the target of mutation testing (make mutation). engine.go executes
// actions through narrow interfaces (DeviceAPI, Firmware, store.JobStore),
// persists each transition BEFORE executing its actions (write-ahead), feeds
// results back as events, limits concurrency and resumes non-terminal jobs
// after a restart.
//
// The transition table is the contract; it is reproduced in the spec
// (docs/spec, "OTA state machine") and in machine_test.go. Change all three
// together.
package ota

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// ErrNotImplemented marks skeleton code.
var ErrNotImplemented = errors.New("ota: not implemented")

// EventKind enumerates what can happen to a job.
type EventKind string

// Events.
const (
	EvStart         EventKind = "start"          // a slot is free, begin preflight
	EvResume        EventKind = "resume"         // process restarted, job was non-terminal
	EvIdentity      EventKind = "identity"       // preflight probe finished (Identity, OtaURL, Uptime filled)
	EvPlanned       EventKind = "planned"        // artifacts resolved and guard passed (URL filled)
	EvCommandOK     EventKind = "command_ok"     // OtaUrl/Upgrade accepted
	EvCommandFailed EventKind = "command_failed" // transport or device error (Err filled)
	EvGuardRefused  EventKind = "guard_refused"  // fwselect.Guard said no (Err filled) - never retried
	EvProbeOnline   EventKind = "probe_online"   // device answered Status 2/11 (Identity, Uptime filled)
	EvProbeOffline  EventKind = "probe_offline"  // device did not answer
	EvTimeout       EventKind = "timeout"        // DeadlineAt passed
	EvBackupDone    EventKind = "backup_done"    // pre-OTA backup stored (or skipped by choice)
	EvBackupFailed  EventKind = "backup_failed"  // Err filled; fails the job unless SkipBackup
	EvUpgradeReport EventKind = "upgrade_report" // MQTT stat/<t>/UPGRADE seen (Err = "Failed ..." or "" for Successful); hint only
	EvCancel        EventKind = "cancel"         // user cancel
	EvPowerReport   EventKind = "power_report"   // a relay was reported OFF by someone else (MQTT); Power = those relays
)

// Event is the input to Step.
type Event struct {
	Kind     EventKind
	Identity model.Identity
	// Power is the relay state read with the preflight identity (EvIdentity
	// only; empty when it could not be read).
	Power model.RelaySet
	// PowerExcluded: relays that are ON but must not be restored (PulseTime,
	// Interlock, shutter relay, beyond the addressable range); EvIdentity in
	// preflight only.
	PowerExcluded model.RelaySet
	// PowerIgnored: relays that are ON but not candidates because only light
	// channels are restored by default; dropped without a note.
	PowerIgnored model.RelaySet
	// At is when an EvPowerReport was received.
	At time.Time
	// PowerRead says Power is the relay state read for this event: EvIdentity
	// in verify (right after the final boot) and EvCommandOK in set_ota_url
	// (right before the first Upgrade). PowerUnreadable says that read failed.
	PowerRead, PowerUnreadable bool
	// PowerUnknown: relays that answered neither ON nor OFF in that reading.
	// PowerDeny: a reason why the device after the boot cannot be trusted to
	// be the one the relays were read from (MAC); no restore then.
	PowerUnknown model.RelaySet
	PowerDeny    string
	OtaURL       string // EvIdentity: current OtaUrl of the device; EvPlanned: URL to set
	Uptime       int64
	Err          string
	// NotFlashed (EvCommandFailed in upgrade_issued): the device certainly
	// flashed nothing, it refused the upload. The job ends at once.
	NotFlashed bool
	// Addr (EvTimeout only, filled by the engine for a push into safeboot): how to reach the
	// device, for the instruction in the failure text.
	Addr string

	// EvPlanned only: the outcome of the plan action. Skip is true when the
	// update policy decided there is nothing to do.
	Skip            bool
	Plan            model.Plan
	TargetVariant   string
	TargetArtifact  model.ArtifactID
	MinimalArtifact model.ArtifactID
}

// ActionKind enumerates side effects requested by Step.
type ActionKind string

// Actions.
const (
	ActProbeIdentity  ActionKind = "probe_identity"  // Status 2 (+Upgrade probe), Status 4, Status 11, OtaUrl
	ActPlan           ActionKind = "plan"            // fwselect.Select/Plan, firmware.Ensure, fwselect.Guard
	ActBackup         ActionKind = "backup"          // backup.Service.Backup
	ActSetOtaURL      ActionKind = "set_ota_url"     // OtaUrl <URL>
	ActUpgrade        ActionKind = "upgrade"         // Upgrade 1 (engine re-runs Guard first); the image upload of a push job
	ActSafebootSwitch ActionKind = "safeboot_switch" // Guard, then GET /u4?u4=fct&api= once (push into safeboot)
	ActSafebootBack   ActionKind = "safeboot_back"   // GET /u4?u4=ota&api= once: the one recovery attempt
	ActProbe          ActionKind = "probe"           // Status 2 + Status 11 (+Upgrade probe on Unknown)
	ActRestoreOtaURL  ActionKind = "restore_ota_url"
	ActRestorePower   ActionKind = "restore_power" // switch the relays that were ON back on (read, switch, verify)
	ActNotify         ActionKind = "notify"        // publish job update (SSE, audit, metrics)
)

// Action is one requested side effect.
type Action struct {
	Kind  ActionKind
	URL   string        // ActSetOtaURL/ActRestoreOtaURL
	Delay time.Duration // run the action after Delay (probe polling)
}

// Config holds every timeout and limit of the machine. Defaults in
// DefaultConfig; all are configurable.
type Config struct {
	CommandTimeout time.Duration // preflight/set_ota_url/verify/restore: answer expected within
	UpgradeAccept  time.Duration // upgrade_issued: device must drop off or reboot within
	RebootWindow   time.Duration // await_reboot: device must be back within (download + flash + boot)
	ProbeInterval  time.Duration // polling interval while waiting
	MaxAttempts    int           // Upgrade attempts per stage
	// SafebootWindow is how long the push into safeboot waits for the device to answer as safeboot
	// after the switch, and for the app after the switch back. A restart into the other partition
	// takes the ESP32 a few seconds to reboot plus its Wi-Fi join; measured on a real ESP32: about 6 s
	// to safeboot, about 11 s back to the app (#30). The window leaves a wide margin.
	SafebootWindow time.Duration
	RestoreOtaURL  bool // put the original OtaUrl back after success
	// PowerRestoreMaxAge is how old the relay state (read in preflight,
	// refreshed right before the first Upgrade) may be when the restore
	// would run. Older than that, nobody expects the load to come back on.
	PowerRestoreMaxAge time.Duration
}

// DefaultConfig returns the documented defaults.
func DefaultConfig() Config {
	return Config{
		CommandTimeout:     30 * time.Second,
		UpgradeAccept:      3 * time.Minute,
		RebootWindow:       8 * time.Minute,
		ProbeInterval:      5 * time.Second,
		MaxAttempts:        2,
		SafebootWindow:     2 * time.Minute,
		RestoreOtaURL:      true,
		PowerRestoreMaxAge: 20 * time.Minute,
	}
}

// Variants that mean "the device is in an intermediate image": the ESP8266
// minimal firmware and the ESP32 safeboot partition.
const (
	variantMinimal  = "minimal"
	variantSafeboot = "safeboot"
)

// Failure and note texts written to Job.LastError.
const (
	ErrTextOtaURLChanged   = "OtaUrl changed by someone else"
	ErrTextNoUpgradeStart  = "device did not start the upgrade"
	ErrTextFWUnreachable   = "the device cannot reach the firmware server"
	ErrTextNotBack         = "device did not come back"
	ErrTextHardwareChanged = "hardware changed after upgrade (critical)"
	ErrTextNoEffect        = "upgrade did not take effect"
	ErrTextUnexpected      = "unexpected firmware after upgrade"
	ErrTextRestoreFailed   = "could not restore OtaUrl"
	ErrTextSafebootFailed  = "Upload into safeboot failed"
	ErrTextPowerFailed     = "could not restore power state"
	ErrTextPowerUnreadable = "relay state unreadable"
)

// Step is the transition function. It must be total: every (state, event)
// pair returns either a transition or the unchanged job with no actions.
func Step(j model.Job, ev Event, now time.Time, cfg Config) (model.Job, []Action) {
	if j.State.Terminal() {
		return j, nil
	}
	switch ev.Kind {
	case EvResume:
		return resume(j, now, cfg)
	case EvCancel:
		return cancel(j, now)
	case EvPowerReport:
		return powerReport(j, ev, now)
	case EvStart, EvIdentity, EvPlanned, EvCommandOK, EvCommandFailed, EvGuardRefused, EvProbeOnline, EvProbeOffline, EvTimeout, EvBackupDone, EvBackupFailed, EvUpgradeReport:
		// handled per state below
	}
	switch j.State {
	case model.JobQueued:
		return stepQueued(j, ev, now, cfg)
	case model.JobPreflight:
		return stepPreflight(j, ev, now, cfg)
	case model.JobBackup:
		return stepBackup(j, ev, now, cfg)
	case model.JobSetOtaURL:
		return stepSetOtaURL(j, ev, now, cfg)
	case model.JobSafebootSwitch:
		return stepSafebootSwitch(j, ev, now, cfg)
	case model.JobSafebootRecover:
		return stepSafebootRecover(j, ev, now, cfg)
	case model.JobUpgradeIssued:
		return stepUpgradeIssued(j, ev, now, cfg)
	case model.JobAwaitReboot:
		return stepAwaitReboot(j, ev, now, cfg)
	case model.JobVerify:
		return stepVerify(j, ev, now, cfg)
	case model.JobRestoreOtaURL:
		return stepRestore(j, ev, now, cfg)
	case model.JobRestorePower:
		return stepRestorePower(j, ev, now)
	case model.JobApplying, model.JobSucceeded, model.JobFailed, model.JobCancelled:
		// not in the transition table: policy jobs never reach the machine
	}
	return j, nil
}

func probeLater(cfg Config) []Action {
	return []Action{{Kind: ActProbe, Delay: cfg.ProbeInterval}}
}

func notify() []Action { return []Action{{Kind: ActNotify}} }

func to(j model.Job, s model.JobState, now time.Time) model.Job {
	j.State = s
	j.UpdatedAt = now
	return j
}

func fail(j model.Job, now time.Time, reason string) (model.Job, []Action) {
	if j.Attempt > 0 && powerToRestore(j) {
		// The device was told to flash: its relays may be OFF now and this
		// job will not bring them back.
		reason = joinNotes(reason, "relays "+j.PowerOn.String()+" were ON before the update and are not restored")
	}
	j = to(j, model.JobFailed, now)
	j.LastError = reason
	return j, notify()
}

// cancelled ends a job whose cancel request waited for the stage to finish.
func cancelled(j model.Job, now time.Time, reason string) (model.Job, []Action) {
	if j.Attempt > 0 && powerToRestore(j) {
		reason = joinNotes(reason, "relays "+j.PowerOn.String()+" were ON before the update and are not restored")
	}
	j = to(j, model.JobCancelled, now)
	j.LastError = reason
	return j, notify()
}

func withErr(reason, detail string) string {
	if detail == "" {
		return reason
	}
	return reason + ": " + detail
}

// toSetOtaURL enters set_ota_url for the current stage. The engine resolves
// the URL from Stage and the planned artifacts and runs Guard before sending.
func toSetOtaURL(j model.Job, now time.Time, cfg Config) (model.Job, []Action) {
	j = to(j, model.JobSetOtaURL, now)
	j.DeadlineAt = now.Add(cfg.CommandTimeout)
	return j, []Action{{Kind: ActSetOtaURL}}
}

// retry starts the stage again if attempts are left, else fails with reason.
// A cancel request stops it instead: starting the stage again would send
// OtaUrl and Upgrade (or the image) after the user said stop. A push job
// never starts it again: the image is sent once per transaction, because
// after a stalled or lost upload nobody knows whether the device is still
// writing it (safety rule 3).
func retry(j model.Job, now time.Time, cfg Config, reason string) (model.Job, []Action) {
	if j.CancelRequested {
		return cancelled(j, now, reason)
	}
	if j.Attempt < cfg.MaxAttempts && j.Delivery != model.DeliveryPush {
		j.LastError = reason
		return toSetOtaURL(j, now, cfg)
	}
	return fail(j, now, reason)
}

func resume(j model.Job, now time.Time, cfg Config) (model.Job, []Action) {
	switch j.State {
	case model.JobPreflight:
		j.DeadlineAt = now.Add(cfg.CommandTimeout)
		return j, []Action{{Kind: ActProbeIdentity}}
	case model.JobVerify:
		j.DeadlineAt = now.Add(cfg.RebootWindow)
		return j, []Action{{Kind: ActProbeIdentity}}
	case model.JobBackup:
		return j, []Action{{Kind: ActBackup}}
	case model.JobSetOtaURL:
		j.DeadlineAt = now.Add(cfg.CommandTimeout)
		return j, []Action{{Kind: ActSetOtaURL}}
	case model.JobSafebootSwitch, model.JobSafebootRecover:
		// Never send /u4 on resume: whether the first one reached the device is unknown, so look
		// first. A switch that never happened ends in the recovery after the window, which is a
		// no-op on a device that runs the app.
		j.DeadlineAt = now.Add(cfg.SafebootWindow)
		return j, []Action{{Kind: ActProbe}}
	case model.JobUpgradeIssued, model.JobAwaitReboot:
		// Never Upgrade on resume: whether the first Upgrade reached the
		// device is unknown, so look first.
		j.DeadlineAt = now.Add(cfg.RebootWindow)
		return j, []Action{{Kind: ActProbe}}
	case model.JobRestoreOtaURL:
		j.DeadlineAt = now.Add(cfg.CommandTimeout)
		return j, []Action{{Kind: ActRestoreOtaURL, URL: j.OriginalOtaURL}}
	case model.JobRestorePower:
		// A restart can take hours; nobody expects a load to come back on
		// then (and the user may have switched it off since).
		if !powerToRestore(j) || powerStale(j, now, cfg) {
			return afterOtaURL(j, now, cfg) // nothing to restore, or too old: done
		}
		// Idempotent: the action reads the relays first and only switches
		// those that are not ON yet.
		j.DeadlineAt = now.Add(cfg.CommandTimeout)
		return j, []Action{{Kind: ActRestorePower}}
	case model.JobQueued, model.JobApplying, model.JobSucceeded, model.JobFailed, model.JobCancelled:
		// not in the transition table: the job stays unchanged
	}
	return j, nil // queued: the scheduler sends start
}

func cancel(j model.Job, now time.Time) (model.Job, []Action) {
	switch j.State {
	case model.JobQueued, model.JobPreflight, model.JobBackup, model.JobSetOtaURL:
		j = to(j, model.JobCancelled, now)
		return j, notify()
	case model.JobSafebootSwitch, model.JobSafebootRecover, model.JobUpgradeIssued, model.JobAwaitReboot, model.JobVerify:
		// The device may be restarting or flashing; finish the stage, then stop. A cancel during
		// the switch ends in the same recovery as a failure: the device is brought back, never left.
		j.CancelRequested = true
		j.UpdatedAt = now
		return j, nil
	case model.JobRestoreOtaURL, model.JobRestorePower, model.JobApplying, model.JobSucceeded, model.JobFailed, model.JobCancelled:
		// not in the transition table: the job stays unchanged
	}
	return j, nil
}

func stepQueued(j model.Job, ev Event, now time.Time, cfg Config) (model.Job, []Action) {
	if ev.Kind != EvStart {
		return j, nil
	}
	j = to(j, model.JobPreflight, now)
	j.DeadlineAt = now.Add(cfg.CommandTimeout)
	return j, []Action{{Kind: ActProbeIdentity}}
}

func stepPreflight(j model.Job, ev Event, now time.Time, cfg Config) (model.Job, []Action) {
	switch ev.Kind {
	case EvIdentity:
		j.IdentityBefore = ev.Identity
		if j.OriginalOtaURL == "" {
			j.OriginalOtaURL = ev.OtaURL
		}
		j.UptimeBefore = ev.Uptime
		j.PowerOn, j.PowerReadAt = 0, now
		if ev.PowerRead { // a reading that was not taken restores nothing
			j.PowerOn = ev.Power &^ ev.PowerExcluded &^ ev.PowerIgnored
		}
		if skipped := ev.Power&ev.PowerExcluded&^ev.PowerIgnored | ev.PowerUnknown; skipped != 0 && !j.SkipPowerRestore {
			j = powerNote(j, "relays "+skipped.String()+" are ON or unclear but not restored: PulseTime, Interlock, shutter relay, beyond relay 8 or a state that is neither ON nor OFF")
		}
		j.UpdatedAt = now
		j.DeadlineAt = now.Add(cfg.CommandTimeout)
		return j, []Action{{Kind: ActPlan}}
	case EvPlanned:
		if ev.Skip {
			j = to(j, model.JobSucceeded, now)
			return j, notify()
		}
		j.Plan = ev.Plan
		j.TargetVariant = ev.TargetVariant
		j.TargetArtifact = ev.TargetArtifact
		j.MinimalArtifact = ev.MinimalArtifact
		j.Attempt = 0
		j.Stage = model.StageFinal
		if ev.Plan == model.PlanStaged && j.IdentityBefore.Variant != variantMinimal {
			j.Stage = model.StageMinimal
		}
		if j.SkipBackup {
			return toSetOtaURL(j, now, cfg)
		}
		j = to(j, model.JobBackup, now)
		return j, []Action{{Kind: ActBackup}}
	case EvGuardRefused:
		return fail(j, now, withErr("guard refused", ev.Err))
	case EvCommandFailed:
		return fail(j, now, withErr("preflight failed", ev.Err))
	case EvTimeout:
		return fail(j, now, "preflight timed out")
	case EvStart, EvResume, EvCommandOK, EvProbeOnline, EvProbeOffline, EvBackupDone, EvBackupFailed, EvUpgradeReport, EvPowerReport, EvCancel:
		// not in the transition table: the job stays unchanged
	}
	return j, nil
}

func stepBackup(j model.Job, ev Event, now time.Time, cfg Config) (model.Job, []Action) {
	switch ev.Kind {
	case EvBackupDone:
		return toSetOtaURL(j, now, cfg)
	case EvBackupFailed:
		return fail(j, now, withErr("backup failed", ev.Err))
	case EvStart, EvResume, EvIdentity, EvPlanned, EvCommandOK, EvCommandFailed, EvGuardRefused, EvProbeOnline, EvProbeOffline, EvTimeout, EvUpgradeReport, EvPowerReport, EvCancel:
		// not in the transition table: the job stays unchanged
	}
	return j, nil
}

func stepSetOtaURL(j model.Job, ev Event, now time.Time, cfg Config) (model.Job, []Action) {
	switch ev.Kind {
	case EvCommandOK:
		j = checkPowerBeforeUpgrade(j, ev, now)
		if j.ViaSafeboot {
			// Nothing was sent to the device yet: Guard ran, the image was read.
			j = to(j, model.JobSafebootSwitch, now)
			j.SeenOffline = false
			j.DeadlineAt = now.Add(cfg.SafebootWindow)
			return j, []Action{{Kind: ActSafebootSwitch}}
		}
		j = to(j, model.JobUpgradeIssued, now)
		j.Attempt++
		j.SeenOffline = false
		j.DeadlineAt = now.Add(cfg.UpgradeAccept)
		return j, []Action{{Kind: ActUpgrade}}
	case EvCommandFailed:
		return fail(j, now, withErr("OtaUrl failed", ev.Err))
	case EvTimeout:
		return fail(j, now, "OtaUrl timed out")
	case EvGuardRefused:
		return fail(j, now, withErr("guard refused", ev.Err))
	case EvStart, EvResume, EvIdentity, EvPlanned, EvProbeOnline, EvProbeOffline, EvBackupDone, EvBackupFailed, EvUpgradeReport, EvPowerReport, EvCancel:
		// not in the transition table: the job stays unchanged
	}
	return j, nil
}

// FWUnreachableReport reports whether a stat/<t>/UPGRADE text says the device could not even connect
// to the server it was sent to ("Failed HTTP error: connection failed"). Other "Failed" texts (a
// 404, a short download, a bad image) keep the retry rule.
//
// The text is ESPhttpUpdate.getLastErrorString() behind "Failed " (support_tasmota.ino, Upgrade): on the
// ESP8266 core "HTTP error: connection failed" (verified live on an ESP8285). The ESP32 HTTPUpdate lives in
// the Arduino core, not in the Tasmota tree, and its text for an unreachable server ("connection refused"
// for HTTPC_ERROR_CONNECTION_REFUSED) is not proven here, so an ESP32 keeps the retry rule until a capture
// shows its text.
func FWUnreachableReport(text string) bool {
	return strings.HasPrefix(text, "Failed") && strings.Contains(strings.ToLower(text), "connection failed")
}

func stepUpgradeIssued(j model.Job, ev Event, now time.Time, cfg Config) (model.Job, []Action) {
	switch ev.Kind {
	case EvCommandOK:
		// The engine compares the "Version X from <url>" echo with the URL
		// it set and reports a mismatch in Err.
		if ev.Err != "" {
			return fail(j, now, withErr(ErrTextOtaURLChanged, ev.Err))
		}
		return pushUploaded(j, now, cfg), probeLater(cfg)
	case EvCommandFailed:
		if ev.NotFlashed && j.ViaSafeboot {
			// Nothing was flashed, but the device sits in safeboot: bring it back.
			return toSafebootRecover(j, now, cfg, withErr("upload refused, nothing was flashed", ev.Err), ev.Addr)
		}
		if ev.NotFlashed {
			// The device refused the image and keeps running: no reboot
			// will come, and the relays were never touched.
			j = to(j, model.JobFailed, now)
			j.LastError = withErr("upload refused, nothing was flashed", ev.Err)
			return j, notify()
		}
		// Unknown whether the upgrade started: probe, never resend.
		return pushUploaded(j, now, cfg), probeLater(cfg)
	case EvProbeOffline:
		j = to(j, model.JobAwaitReboot, now)
		j.SeenOffline = true
		if j.PowerCutoff.IsZero() {
			j.PowerCutoff = now // from here on the device announces its boot state, not a user's action
		}
		j.DeadlineAt = now.Add(cfg.RebootWindow)
		return j, probeLater(cfg)
	case EvProbeOnline:
		if !rebooted(j, ev) {
			return j, probeLater(cfg)
		}
		j = to(j, model.JobVerify, now)
		j.DeadlineAt = now.Add(cfg.RebootWindow)
		return j, []Action{{Kind: ActProbeIdentity}}
	case EvUpgradeReport:
		if !strings.HasPrefix(ev.Err, "Failed") {
			return j, nil // "Successful" is only a hint; the probe decides
		}
		if FWUnreachableReport(ev.Err) && j.Delivery != model.DeliveryPush {
			// The device was told to download from tasmofleet's firmware server (Guard allows no other
			// URL) and could not connect to it: a second Upgrade 1 would fail the same way, after the
			// same minute of waiting (D9). The app records it as "the device does not reach the server".
			if j.CancelRequested {
				return cancelled(j, now, withErr(ErrTextFWUnreachable, ev.Err))
			}
			return fail(j, now, withErr(ErrTextFWUnreachable, ev.Err))
		}
		return retry(j, now, cfg, ev.Err)
	case EvTimeout:
		if j.ViaSafeboot {
			// Answering the whole window without ever restarting: still in safeboot.
			return toSafebootRecover(j, now, cfg, "the device did not restart after the upload", ev.Addr)
		}
		return retry(j, now, cfg, ErrTextNoUpgradeStart)
	case EvGuardRefused:
		if j.ViaSafeboot {
			// The upload was not sent and the device waits in safeboot.
			return toSafebootRecover(j, now, cfg, withErr("guard refused", ev.Err), ev.Addr)
		}
		// Upgrade was not sent.
		return fail(j, now, withErr("guard refused", ev.Err))
	case EvStart, EvResume, EvIdentity, EvPlanned, EvBackupDone, EvBackupFailed, EvPowerReport, EvCancel:
		// not in the transition table: the job stays unchanged
	}
	return j, nil
}

// pushUploaded restarts the upgrade_issued window when a push upload has
// ended: the upload ran inside the window and may have used all of it, and
// the device needs the full window to drop off or reboot after it. A pull
// job's window started with its short Upgrade command and stays.
func pushUploaded(j model.Job, now time.Time, cfg Config) model.Job {
	if j.Delivery != model.DeliveryPush {
		return j
	}
	j.UpdatedAt = now
	j.DeadlineAt = now.Add(cfg.UpgradeAccept)
	return j
}

// rebooted reports whether a probe answer proves the device restarted or
// changed firmware since Upgrade was issued.
func rebooted(j model.Job, ev Event) bool {
	id := ev.Identity
	if j.ViaSafeboot {
		// The upload went into safeboot: the device has restarted when an app answers. Safeboot's
		// own answers (any uptime) are not a reboot, and the app's uptime before the switch says
		// nothing about this restart.
		return id.Variant != "" && !isIntermediate(id.Variant)
	}
	if ev.Uptime > 0 && ev.Uptime < j.UptimeBefore {
		return true
	}
	if runsIntermediate(j) {
		return id.Variant != "" && id.Variant != variantMinimal
	}
	before := j.IdentityBefore
	return (id.Version != "" && id.Version != before.Version) ||
		(id.Image != "" && id.Image != before.Image)
}

// runsIntermediate reports whether the device is expected to run the
// minimal image right now: the final stage of a staged plan that started
// from a full firmware.
func runsIntermediate(j model.Job) bool {
	return j.Stage == model.StageFinal && j.Plan == model.PlanStaged &&
		j.IdentityBefore.Variant != variantMinimal
}

func stepAwaitReboot(j model.Job, ev Event, now time.Time, cfg Config) (model.Job, []Action) {
	switch ev.Kind {
	case EvProbeOffline:
		if !j.SeenOffline {
			j.SeenOffline = true
			j.UpdatedAt = now
		}
		return j, probeLater(cfg)
	case EvProbeOnline:
		// Waiting without having seen the device drop off (auto-continue,
		// or a download that may still be retrying): an answer on the old
		// or intermediate image means "not yet". Upgrade must never hit a
		// device that is about to download.
		if !j.SeenOffline && looksOld(j, ev.Identity) {
			return j, probeLater(cfg)
		}
		j = to(j, model.JobVerify, now)
		return j, []Action{{Kind: ActProbeIdentity}}
	case EvTimeout:
		if !j.SeenOffline {
			if j.ViaSafeboot {
				return toSafebootRecover(j, now, cfg, "the device did not restart after the upload", ev.Addr)
			}
			// Online on the old image for the whole window: the
			// download is over and failed; the device is idle.
			return retry(j, now, cfg, ErrTextNoEffect)
		}
		if j.ViaSafeboot {
			return fail(j, now, joinNotes(ErrTextNotBack, "it may be in safeboot"+atAddr(ev.Addr)+": open its web page and upload the firmware, or use Upgrade by download"))
		}
		if j.Transport == model.TransportMQTT {
			return fail(j, now, joinNotes(ErrTextNotBack, "restart the device and retry the update"))
		}
		return fail(j, now, ErrTextNotBack)
	case EvStart, EvResume, EvIdentity, EvPlanned, EvCommandOK, EvCommandFailed, EvGuardRefused, EvBackupDone, EvBackupFailed, EvUpgradeReport, EvPowerReport, EvCancel:
		// not in the transition table: the job stays unchanged
	}
	return j, nil
}

// looksOld reports whether a probe identity is not (yet) a new image:
// unknown, intermediate (minimal/safeboot) or unchanged since preflight.
func looksOld(j model.Job, id model.Identity) bool {
	if id.Variant == "" || isIntermediate(id.Variant) {
		return true
	}
	return id.Version == j.IdentityBefore.Version && id.Variant == j.IdentityBefore.Variant
}

func isIntermediate(variant string) bool {
	return variant == variantMinimal || variant == variantSafeboot
}

func hardwareChanged(before, now model.Identity) bool {
	if now.Family != model.ChipUnknown && now.Family != before.Family {
		return true
	}
	return now.Hardware != "" && before.Hardware != "" && now.Hardware != before.Hardware
}

func stepVerify(j model.Job, ev Event, now time.Time, cfg Config) (model.Job, []Action) {
	switch ev.Kind {
	case EvCommandFailed, EvTimeout:
		// Keep the reboot deadline: verify and await_reboot share it.
		j = to(j, model.JobAwaitReboot, now)
		return j, probeLater(cfg)
	case EvIdentity:
		// decided below
	case EvStart, EvResume, EvPlanned, EvCommandOK, EvGuardRefused, EvProbeOnline, EvProbeOffline, EvBackupDone, EvBackupFailed, EvUpgradeReport, EvPowerReport, EvCancel:
		return j, nil // not in the transition table: the job stays unchanged
	}
	id := ev.Identity
	if hardwareChanged(j.IdentityBefore, id) {
		return fail(j, now, ErrTextHardwareChanged)
	}
	if j.Stage == model.StageMinimal {
		if id.Variant == variantMinimal {
			return minimalReached(j, ev, now, cfg)
		}
		return notTaken(j, ev, now, cfg)
	}
	if isIntermediate(id.Variant) {
		if j.ViaSafeboot {
			return toSafebootRecover(j, now, cfg, "the device is back in safeboot after the upload", ev.Addr)
		}
		if runsIntermediate(j) || j.AutoContinueSeen {
			return notTaken(j, ev, now, cfg)
		}
		// Tasmota's own fallback (ota_loader / safeboot) continues with
		// the stored OtaUrl by itself; wait for it once.
		j = to(j, model.JobAwaitReboot, now)
		j.AutoContinueSeen = true
		j.SeenOffline = false
		j.DeadlineAt = now.Add(cfg.RebootWindow)
		return j, probeLater(cfg)
	}
	// A chip with two builds reports the shared image tag, so the variant read back is "unknown"
	// whatever was installed (#88): compare with what Detect will read, not with the selection.
	want := fwselect.DetectedVariant(fwselect.Selection{Family: j.IdentityBefore.Family, Variant: j.TargetVariant})
	if id.Version == j.TargetRelease && id.Variant == want &&
		(id.Language == j.IdentityBefore.Language || !id.LanguageKnown) && id.Family == j.IdentityBefore.Family {
		// A probe over MQTT cannot see the language build (LanguageKnown false): the person's choice or the
		// stored language stays the reference then, and only a language that was read can contradict it.
		return finalReached(j, ev, now, cfg)
	}
	return notTaken(j, ev, now, cfg)
}

func minimalReached(j model.Job, ev Event, now time.Time, cfg Config) (model.Job, []Action) {
	if j.CancelRequested {
		j = to(j, model.JobCancelled, now)
		if powerToRestore(j) {
			j.LastError = "relays " + j.PowerOn.String() + " were ON before the update and are not restored"
		}
		return j, notify()
	}
	j.Stage = model.StageFinal
	j.Attempt = 0
	j.SeenOffline = false
	j.UptimeBefore = ev.Uptime
	j.LastError = ""
	return toSetOtaURL(j, now, cfg)
}

// notTaken handles a device that answers but does not run the expected
// image: still on the old one means retry, anything else fails.
func notTaken(j model.Job, ev Event, now time.Time, cfg Config) (model.Job, []Action) {
	id := ev.Identity
	before := j.IdentityBefore
	if runsIntermediate(j) {
		before = model.Identity{Version: id.Version, Variant: variantMinimal}
	}
	old := id.Version == before.Version && id.Variant == before.Variant
	if j.AutoContinueSeen && isIntermediate(id.Variant) {
		old = true // waited once already; treat like the old image
	}
	if !old {
		return fail(j, now, withErr(ErrTextUnexpected, id.Version+" "+id.Variant))
	}
	// Tasmota does not reboot after a failed download and answers between
	// its download attempts. Without reboot proof, keep waiting until the
	// reboot window is over instead of sending Upgrade mid-download.
	rebootProof := ev.Uptime > 0 && ev.Uptime < j.UptimeBefore
	if !rebootProof && now.Before(j.DeadlineAt) {
		j = to(j, model.JobAwaitReboot, now)
		j.SeenOffline = false
		return j, probeLater(cfg)
	}
	j.UptimeBefore = ev.Uptime
	return retry(j, now, cfg, ErrTextNoEffect)
}

func finalReached(j model.Job, ev Event, now time.Time, cfg Config) (model.Job, []Action) {
	j.LastError = ""
	j = judgePowerAfterBoot(j, ev)
	if !cfg.RestoreOtaURL || j.CancelRequested || j.OriginalOtaURL == "" || ev.OtaURL == j.OriginalOtaURL {
		return afterOtaURL(j, now, cfg)
	}
	j = to(j, model.JobRestoreOtaURL, now)
	j.DeadlineAt = now.Add(cfg.CommandTimeout)
	return j, []Action{{Kind: ActRestoreOtaURL, URL: j.OriginalOtaURL}}
}

// powerToRestore reports whether any relay was ON before the update and the
// restore was not switched off. Relays that were OFF are never touched, so
// without an ON relay there is nothing to do.
func powerToRestore(j model.Job) bool { return !j.SkipPowerRestore && j.PowerOn != 0 }

// powerNote appends a reason why relays are not restored; it ends up in
// LastError of the finished job.
func powerNote(j model.Job, note string) model.Job {
	j.PowerNote = joinNotes(j.PowerNote, note)
	return j
}

// withPowerNote is LastError plus the collected power notes.
func withPowerNote(j model.Job) model.Job {
	if j.PowerNote != "" {
		j.LastError = joinNotes(j.LastError, j.PowerNote)
		j.PowerNote = ""
	}
	return j
}

// firstFullStage reports whether the stage that starts now still runs on
// the device's own full firmware, so Status 11 answers: the minimal stage
// of a staged update, or the only stage of a direct one.
func firstFullStage(j model.Job) bool {
	if isIntermediate(j.IdentityBefore.Variant) {
		return false
	}
	return (j.Plan == model.PlanStaged && j.Stage == model.StageMinimal) ||
		(j.Plan == model.PlanDirect && j.Stage == model.StageFinal)
}

// checkPowerBeforeUpgrade applies the second relay reading, taken right
// before the first Upgrade: only relays that are ON in both readings stay
// candidates. Someone who switched a relay off between preflight and now
// (backup, a slow Wi-Fi) must not see it switched on again.
func checkPowerBeforeUpgrade(j model.Job, ev Event, now time.Time) model.Job {
	if !powerToRestore(j) {
		return j
	}
	switch {
	case ev.PowerUnreadable:
		j.PowerOn = 0
		j = powerNote(j, "power not restored: "+ErrTextPowerUnreadable+" right before the update")
	case !ev.PowerRead:
		if firstFullStage(j) { // the reading that must be taken here is missing: never assume it
			j.PowerOn = 0
			j = powerNote(j, "power not restored: the relay state was not read right before the update")
		}
	case ev.PowerRead:
		if gone := j.PowerOn &^ ev.Power; gone != 0 {
			j = powerNote(j, "relays "+gone.String()+" were not ON right before the update (switched off or state unclear) and are left alone")
		}
		j.PowerOn &= ev.Power
		j.PowerReadAt = now
	}
	return j
}

// powerReport handles "someone switched relay r off" (MQTT). It counts if
// it was received after the state was read and before the device went offline
// for the update: that is the window in which a person or an automation can
// have acted. Reports from before the reading are old news, later ones are
// the device announcing its boot state. The time of reception decides, not the
// state the job happens to be in when the report is processed.
func powerReport(j model.Job, ev Event, now time.Time) (model.Job, []Action) {
	if j.State.Terminal() || j.State == model.JobQueued {
		return j, nil
	}
	if ev.At.Before(j.PowerReadAt) || (!j.PowerCutoff.IsZero() && ev.At.After(j.PowerCutoff)) {
		return j, nil
	}
	gone := j.PowerOn & ev.Power
	if gone == 0 {
		return j, nil
	}
	j.PowerOn &^= gone
	j.UpdatedAt = now
	return powerNote(j, "relays "+gone.String()+" were switched off during the update and are left off"), nil
}

// judgePowerAfterBoot decides from the first reading after the final boot
// whether the device kept its relay state itself (RTC memory on an ESP8266
// without the minimal hop, SetOption0 1): relays that are ON then were kept,
// never touched, and nothing is restored. Only when none of the relays that
// were ON is ON now was the state lost, and only then is a restore due.
func judgePowerAfterBoot(j model.Job, ev Event) model.Job {
	if !powerToRestore(j) {
		return j
	}
	switch {
	case ev.PowerUnreadable:
		j.PowerOn = 0
		return powerNote(j, "power not restored: "+ErrTextPowerUnreadable+" after the update")
	case ev.PowerDeny != "":
		j.PowerOn = 0
		return powerNote(j, "power not restored: "+ev.PowerDeny)
	case !ev.PowerRead:
		j.PowerOn = 0
		return powerNote(j, "power not restored: relay state after the update was not read")
	case j.PowerOn&ev.Power != 0:
		j.PowerOn = 0 // the device kept its state
		return j
	}
	if unclear := j.PowerOn & ev.PowerUnknown; unclear != 0 {
		j.PowerOn &^= unclear
		j = powerNote(j, "relays "+unclear.String()+" answered neither ON nor OFF after the update and are not restored")
	}
	return j
}

// powerStale reports whether the relay state is too old to act on.
func powerStale(j model.Job, now time.Time, cfg Config) bool {
	// No maximum age configured, no reading time recorded: nothing is known
	// about how old the state is, so it counts as too old.
	if cfg.PowerRestoreMaxAge <= 0 || j.PowerReadAt.IsZero() {
		return true
	}
	return now.Sub(j.PowerReadAt) > cfg.PowerRestoreMaxAge
}

// powerAge words the age of the relay state for the job note.
func powerAge(j model.Job, now time.Time) string {
	if j.PowerReadAt.IsZero() {
		return "unknown age"
	}
	return fmt.Sprintf("%d min", int(now.Sub(j.PowerReadAt).Minutes()))
}

// afterOtaURL is the step after the OtaUrl is dealt with: the power restore
// when there is something to restore and the state is recent enough, else
// done. It also runs when the user cancelled during the last stage: the
// device flashed the final image, and leaving its lights off helps nobody.
func afterOtaURL(j model.Job, now time.Time, cfg Config) (model.Job, []Action) {
	if powerToRestore(j) && powerStale(j, now, cfg) {
		j = powerNote(j, fmt.Sprintf("power not restored: the relay state of %s is too old (%s, maximum %d min)",
			j.PowerOn, powerAge(j, now), int(cfg.PowerRestoreMaxAge.Minutes())))
		j.PowerOn = 0
	}
	if powerToRestore(j) {
		j = to(j, model.JobRestorePower, now)
		j.DeadlineAt = now.Add(cfg.CommandTimeout)
		return j, []Action{{Kind: ActRestorePower}}
	}
	j = to(j, model.JobSucceeded, now)
	j = withPowerNote(j)
	return j, notify()
}

func stepRestore(j model.Job, ev Event, now time.Time, cfg Config) (model.Job, []Action) {
	switch ev.Kind {
	case EvCommandOK:
		return afterOtaURL(j, now, cfg)
	case EvCommandFailed, EvTimeout:
		j.LastError = withErr(ErrTextRestoreFailed, ev.Err)
		return afterOtaURL(j, now, cfg)
	case EvStart, EvResume, EvIdentity, EvPlanned, EvGuardRefused, EvProbeOnline, EvProbeOffline, EvBackupDone, EvBackupFailed, EvUpgradeReport, EvPowerReport, EvCancel:
		// not in the transition table: the job stays unchanged
	}
	return j, nil
}

// stepRestorePower ends the job. A failure to switch a relay back on does
// not undo the update: the job succeeds and says what is left to do.
func stepRestorePower(j model.Job, ev Event, now time.Time) (model.Job, []Action) {
	switch ev.Kind {
	case EvCommandOK:
		j = to(j, model.JobSucceeded, now)
		return withPowerNote(j), notify()
	case EvCommandFailed, EvTimeout:
		j = to(j, model.JobSucceeded, now)
		j.LastError = joinNotes(j.LastError, withErr(ErrTextPowerFailed, ev.Err))
		return withPowerNote(j), notify()
	case EvStart, EvResume, EvIdentity, EvPlanned, EvGuardRefused, EvProbeOnline, EvProbeOffline, EvBackupDone, EvBackupFailed, EvUpgradeReport, EvPowerReport, EvCancel:
		// not in the transition table: the job stays unchanged
	}
	return j, nil
}

func joinNotes(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}
