package ota

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// The tests in this file mirror the transition table in the spec
// (docs/spec, "OTA state machine", §9.3) row by row, plus the resume table.
// Each subtest is named "<from>/<event>[/<condition>]". A failing row means
// the machine no longer does what the spec promises the device fleet.

var t0 = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

const (
	minimalURL = "http://fw.example:8080/fw/16.0.0/tasmota-minimal.bin.gz"
	origURL    = "http://ota.tasmota.com/tasmota/release/tasmota.bin.gz"
)

func testCfg() Config {
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

func plug1M() model.Identity {
	return model.Identity{Family: model.ChipESP8266, Hardware: "ESP8285N08", Version: "15.6.0", Image: "release-tasmota", Variant: "tasmota", FlashSizeKB: 1024, FreeKB: 352}
}

func minimalIdent(version string) model.Identity {
	return model.Identity{Family: model.ChipESP8266, Version: version, Image: "release-minimal", Variant: "minimal"}
}

func finalIdent() model.Identity {
	id := plug1M()
	id.Version = "16.0.0"
	return id
}

// job returns a job in the given state of a staged 1 MB ESP8266 update
// 15.6.0 -> 16.0.0 after preflight.
func job(s model.JobState, stage model.Stage) model.Job {
	return model.Job{
		ID: 7, BatchID: 1, DeviceID: 3,
		State: s, Stage: stage, Plan: model.PlanStaged, Attempt: 1,
		Policy: model.PolicyOnlyNewer, TargetRelease: "16.0.0", TargetVariant: "tasmota",
		TargetArtifact: 11, MinimalArtifact: 12,
		IdentityBefore: plug1M(), OriginalOtaURL: origURL, UptimeBefore: 5000,
		DeadlineAt: t0.Add(-time.Hour),
	}
}

func kinds(as []Action) []ActionKind {
	var out []ActionKind
	for _, a := range as {
		out = append(out, a.Kind)
	}
	return out
}

type row struct {
	name    string
	in      model.Job
	ev      Event
	cfg     func(*Config)
	state   model.JobState
	actions []ActionKind
	check   func(t *testing.T, j model.Job, as []Action)
}

func runRows(t *testing.T, rows []row) {
	t.Helper()
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			cfg := testCfg()
			if r.cfg != nil {
				r.cfg(&cfg)
			}
			got, as := Step(r.in, r.ev, t0, cfg)
			if got.State != r.state {
				t.Fatalf("state = %s, want %s (LastError %q)", got.State, r.state, got.LastError)
			}
			if !reflect.DeepEqual(kinds(as), r.actions) {
				t.Fatalf("actions = %v, want %v", kinds(as), r.actions)
			}
			if r.check != nil {
				r.check(t, got, as)
			}
		})
	}
}

func wantDeadline(t *testing.T, j model.Job, d time.Duration) {
	t.Helper()
	if !j.DeadlineAt.Equal(t0.Add(d)) {
		t.Fatalf("DeadlineAt = %v, want now+%v", j.DeadlineAt, d)
	}
}

func wantErrContains(t *testing.T, j model.Job, s string) {
	t.Helper()
	if !strings.Contains(j.LastError, s) {
		t.Fatalf("LastError = %q, want it to contain %q", j.LastError, s)
	}
}

func wantDelay(t *testing.T, as []Action, d time.Duration) {
	t.Helper()
	if len(as) != 1 || as[0].Delay != d {
		t.Fatalf("actions %+v, want one action with delay %v", as, d)
	}
}

func TestStepQueuedAndPreflight(t *testing.T) {
	queued := model.Job{ID: 1, State: model.JobQueued, TargetRelease: "16.0.0"}
	pre := job(model.JobPreflight, "")
	pre.OriginalOtaURL = ""
	pre.UptimeBefore = 0
	pre.IdentityBefore = model.Identity{}
	planned := Event{Kind: EvPlanned, Plan: model.PlanStaged, TargetVariant: "tasmota", TargetArtifact: 21, MinimalArtifact: 22}

	runRows(t, []row{
		{
			// A free slot starts the job with an identity probe: nothing is
			// decided on stale inventory data.
			name: "queued/start", in: queued, ev: Event{Kind: EvStart},
			state: model.JobPreflight, actions: []ActionKind{ActProbeIdentity},
			check: func(t *testing.T, j model.Job, _ []Action) { wantDeadline(t, j, 30*time.Second) },
		},
		{
			// The identity is recorded before planning so the job can be
			// resumed and verified against what the device was.
			name: "preflight/identity", in: pre,
			ev:    Event{Kind: EvIdentity, Identity: plug1M(), OtaURL: origURL, Uptime: 4242},
			state: model.JobPreflight, actions: []ActionKind{ActPlan},
			check: func(t *testing.T, j model.Job, _ []Action) {
				if j.IdentityBefore != plug1M() || j.OriginalOtaURL != origURL || j.UptimeBefore != 4242 {
					t.Fatalf("not recorded: %+v", j)
				}
				wantDeadline(t, j, 30*time.Second)
			},
		},
		{
			// A resumed preflight must not overwrite the original OtaUrl
			// with whatever an earlier stage left on the device.
			name: "preflight/identity/keeps original OtaUrl", in: job(model.JobPreflight, ""),
			ev:    Event{Kind: EvIdentity, Identity: plug1M(), OtaURL: minimalURL, Uptime: 1},
			state: model.JobPreflight, actions: []ActionKind{ActPlan},
			check: func(t *testing.T, j model.Job, _ []Action) {
				if j.OriginalOtaURL != origURL {
					t.Fatalf("OriginalOtaURL = %q, want %q", j.OriginalOtaURL, origURL)
				}
			},
		},
		{
			name: "preflight/planned/skip", in: pre, ev: Event{Kind: EvPlanned, Skip: true},
			state: model.JobSucceeded, actions: []ActionKind{ActNotify},
		},
		{
			// Staged plan on a full firmware starts with the minimal stage.
			name: "preflight/planned/SkipBackup staged", in: withSkipBackup(job(model.JobPreflight, "")), ev: planned,
			state: model.JobSetOtaURL, actions: []ActionKind{ActSetOtaURL},
			check: func(t *testing.T, j model.Job, _ []Action) {
				if j.Stage != model.StageMinimal || j.Plan != model.PlanStaged || j.TargetArtifact != 21 || j.MinimalArtifact != 22 || j.TargetVariant != "tasmota" || j.Attempt != 0 {
					t.Fatalf("plan not recorded: %+v", j)
				}
				wantDeadline(t, j, 30*time.Second)
			},
		},
		{
			// A device stuck on minimal goes straight to the final image.
			name: "preflight/planned/SkipBackup staged already minimal", in: withIdent(withSkipBackup(job(model.JobPreflight, "")), minimalIdent("16.0.0")), ev: planned,
			state: model.JobSetOtaURL, actions: []ActionKind{ActSetOtaURL},
			check: func(t *testing.T, j model.Job, _ []Action) {
				if j.Stage != model.StageFinal {
					t.Fatalf("stage = %s, want final", j.Stage)
				}
			},
		},
		{
			name: "preflight/planned/SkipBackup direct", in: withSkipBackup(job(model.JobPreflight, "")),
			ev:    Event{Kind: EvPlanned, Plan: model.PlanDirect, TargetVariant: "tasmota", TargetArtifact: 21},
			state: model.JobSetOtaURL, actions: []ActionKind{ActSetOtaURL},
			check: func(t *testing.T, j model.Job, _ []Action) {
				if j.Stage != model.StageFinal || j.Plan != model.PlanDirect {
					t.Fatalf("stage/plan = %s/%s", j.Stage, j.Plan)
				}
			},
		},
		{
			// Backups are the default: a config dump before every flash.
			name: "preflight/planned/otherwise", in: job(model.JobPreflight, ""), ev: planned,
			state: model.JobBackup, actions: []ActionKind{ActBackup},
			check: func(t *testing.T, j model.Job, _ []Action) {
				if j.Stage != model.StageMinimal || j.TargetArtifact != 21 {
					t.Fatalf("plan not recorded before backup: %+v", j)
				}
			},
		},
		{
			name: "preflight/guard_refused", in: pre, ev: Event{Kind: EvGuardRefused, Err: "unknown hardware"},
			state: model.JobFailed, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, "unknown hardware") },
		},
		{
			name: "preflight/command_failed", in: pre, ev: Event{Kind: EvCommandFailed, Err: "unreachable"},
			state: model.JobFailed, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, "unreachable") },
		},
		{
			name: "preflight/timeout", in: pre, ev: Event{Kind: EvTimeout},
			state: model.JobFailed, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, "timed out") },
		},
	})
}

func withSkipBackup(j model.Job) model.Job { j.SkipBackup = true; return j }

func withIdent(j model.Job, id model.Identity) model.Job { j.IdentityBefore = id; return j }

func withAttempt(j model.Job, n int) model.Job { j.Attempt = n; return j }

func withPlan(j model.Job, p model.Plan) model.Job { j.Plan = p; return j }

func TestStepBackupAndSetOtaURL(t *testing.T) {
	set := withAttempt(job(model.JobSetOtaURL, model.StageMinimal), 0)
	set.SeenOffline = true
	runRows(t, []row{
		{
			name: "backup/backup_done", in: job(model.JobBackup, model.StageMinimal), ev: Event{Kind: EvBackupDone},
			state: model.JobSetOtaURL, actions: []ActionKind{ActSetOtaURL},
			check: func(t *testing.T, j model.Job, _ []Action) { wantDeadline(t, j, 30*time.Second) },
		},
		{
			// No flash without a backup unless the user chose so.
			name: "backup/backup_failed", in: job(model.JobBackup, model.StageMinimal), ev: Event{Kind: EvBackupFailed, Err: "dl 401"},
			state: model.JobFailed, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, "dl 401") },
		},
		{
			// Attempt counts Upgrade commands; it rises before Upgrade is
			// sent (write-ahead), so a crash cannot hide an attempt.
			name: "set_ota_url/command_ok", in: set, ev: Event{Kind: EvCommandOK},
			state: model.JobUpgradeIssued, actions: []ActionKind{ActUpgrade},
			check: func(t *testing.T, j model.Job, _ []Action) {
				if j.Attempt != 1 || j.SeenOffline {
					t.Fatalf("Attempt=%d SeenOffline=%v, want 1/false", j.Attempt, j.SeenOffline)
				}
				wantDeadline(t, j, 3*time.Minute)
			},
		},
		{
			name: "set_ota_url/command_failed", in: set, ev: Event{Kind: EvCommandFailed, Err: "timeout"},
			state: model.JobFailed, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, "timeout") },
		},
		{
			name: "set_ota_url/timeout", in: set, ev: Event{Kind: EvTimeout},
			state: model.JobFailed, actions: []ActionKind{ActNotify},
		},
		{
			// Guard re-runs before OtaUrl; a refusal is final.
			name: "set_ota_url/guard_refused", in: set, ev: Event{Kind: EvGuardRefused, Err: "family mismatch"},
			state: model.JobFailed, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, "family mismatch") },
		},
	})
}

func TestStepUpgradeIssued(t *testing.T) {
	ui := job(model.JobUpgradeIssued, model.StageMinimal)
	uiMax := withAttempt(ui, 2)
	online := func(id model.Identity, up int64) Event { return Event{Kind: EvProbeOnline, Identity: id, Uptime: up} }
	finalStaged := job(model.JobUpgradeIssued, model.StageFinal)
	direct := withPlan(job(model.JobUpgradeIssued, model.StageFinal), model.PlanDirect)

	runRows(t, []row{
		{
			name: "upgrade_issued/command_ok/reply echoes the URL", in: ui, ev: Event{Kind: EvCommandOK},
			state: model.JobUpgradeIssued, actions: []ActionKind{ActProbe},
			check: func(t *testing.T, _ model.Job, as []Action) { wantDelay(t, as, 5*time.Second) },
		},
		{
			// Another tool changed OtaUrl between our OtaUrl and Upgrade:
			// the device may be flashing someone else's file. Stop.
			name: "upgrade_issued/command_ok/URL differs", in: ui, ev: Event{Kind: EvCommandOK, Err: "http://other/x.bin.gz"},
			state: model.JobFailed, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, ErrTextOtaURLChanged) },
		},
		{
			// A lost Upgrade reply is expected (download blocks the web
			// server); never resend, look.
			name: "upgrade_issued/command_failed", in: ui, ev: Event{Kind: EvCommandFailed, Err: "timeout"},
			state: model.JobUpgradeIssued, actions: []ActionKind{ActProbe},
			check: func(t *testing.T, j model.Job, as []Action) {
				wantDelay(t, as, 5*time.Second)
				if j.Attempt != 1 {
					t.Fatalf("Attempt changed to %d", j.Attempt)
				}
			},
		},
		{
			// The device answered "Upload Failed" (office-monitor-thomas,
			// 2026-10-24: "Not enough space"): nothing was flashed and the
			// device keeps running, so waiting for a reboot that cannot come
			// only blocks the slot. The relays were never touched, so the
			// failure carries no "not restored" note.
			name: "upgrade_issued/command_failed/upload refused", in: withPower(ui, 1), ev: Event{Kind: EvCommandFailed, Err: "upload: tasmota: device refused the upload: Not enough space", NotFlashed: true},
			state: model.JobFailed, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) {
				wantErrContains(t, j, "nothing was flashed")
				wantErrContains(t, j, "Not enough space")
				if strings.Contains(j.LastError, "not restored") {
					t.Fatalf("refusal claims relays were left off: %q", j.LastError)
				}
			},
		},
		{
			name: "upgrade_issued/probe_offline", in: ui, ev: Event{Kind: EvProbeOffline},
			state: model.JobAwaitReboot, actions: []ActionKind{ActProbe},
			check: func(t *testing.T, j model.Job, as []Action) {
				if !j.SeenOffline {
					t.Fatal("SeenOffline not set")
				}
				wantDeadline(t, j, 8*time.Minute)
				wantDelay(t, as, 5*time.Second)
			},
		},
		{
			// A smaller uptime proves a reboot even when the reboot gap was
			// shorter than the probe interval.
			name: "upgrade_issued/probe_online/uptime < UptimeBefore", in: ui, ev: online(plug1M(), 4999),
			state: model.JobVerify, actions: []ActionKind{ActProbeIdentity},
			check: func(t *testing.T, j model.Job, _ []Action) { wantDeadline(t, j, 8*time.Minute) },
		},
		{
			name: "upgrade_issued/probe_online/version changed", in: ui, ev: online(minimalIdent("16.0.0"), 0),
			state: model.JobVerify, actions: []ActionKind{ActProbeIdentity},
		},
		{
			name: "upgrade_issued/probe_online/image changed", in: ui, ev: online(model.Identity{Version: "15.6.0", Image: "release-minimal"}, 0),
			state: model.JobVerify, actions: []ActionKind{ActProbeIdentity},
		},
		{
			name: "upgrade_issued/probe_online/final stage left minimal", in: finalStaged, ev: online(finalIdent(), 0),
			state: model.JobVerify, actions: []ActionKind{ActProbeIdentity},
		},
		{
			name: "upgrade_issued/probe_online/unchanged", in: ui, ev: online(plug1M(), 5000),
			state: model.JobUpgradeIssued, actions: []ActionKind{ActProbe},
			check: func(t *testing.T, _ model.Job, as []Action) { wantDelay(t, as, 5*time.Second) },
		},
		{
			// Unknown uptime (0) must not count as a reboot.
			name: "upgrade_issued/probe_online/unchanged unknown uptime", in: ui, ev: online(plug1M(), 0),
			state: model.JobUpgradeIssued, actions: []ActionKind{ActProbe},
		},
		{
			name: "upgrade_issued/probe_online/unchanged unknown before uptime", in: withUptime(ui, 0), ev: online(plug1M(), 3),
			state: model.JobUpgradeIssued, actions: []ActionKind{ActProbe},
		},
		{
			// A partial probe answer (no version/image) proves nothing.
			name: "upgrade_issued/probe_online/unchanged empty identity", in: ui, ev: online(model.Identity{}, 6000),
			state: model.JobUpgradeIssued, actions: []ActionKind{ActProbe},
		},
		{
			name: "upgrade_issued/probe_online/unchanged still minimal in final stage", in: finalStaged, ev: online(minimalIdent("16.0.0"), 6000),
			state: model.JobUpgradeIssued, actions: []ActionKind{ActProbe},
		},
		{
			name: "upgrade_issued/probe_online/unchanged empty variant in final stage", in: finalStaged, ev: online(model.Identity{Version: "16.0.0"}, 6000),
			state: model.JobUpgradeIssued, actions: []ActionKind{ActProbe},
		},
		{
			name: "upgrade_issued/probe_online/direct version changed", in: direct, ev: online(finalIdent(), 6000),
			state: model.JobVerify, actions: []ActionKind{ActProbeIdentity},
		},
		{
			name: "upgrade_issued/upgrade_report Failed/Attempt < Max", in: ui, ev: Event{Kind: EvUpgradeReport, Err: "Failed File Not Found (404)"},
			state: model.JobSetOtaURL, actions: []ActionKind{ActSetOtaURL},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, "404") },
		},
		{
			name: "upgrade_issued/upgrade_report Failed/Attempt = Max", in: uiMax, ev: Event{Kind: EvUpgradeReport, Err: "Failed File Not Found (404)"},
			state: model.JobFailed, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, "404") },
		},
		{
			// Why (D9): the device could not even connect to the firmware server. A second Upgrade 1 would
			// fail the same way after the same minute, so the job ends at once, whatever the attempt count.
			name: "upgrade_issued/upgrade_report connection failed/Attempt < Max", in: ui, ev: Event{Kind: EvUpgradeReport, Err: "Failed HTTP error: connection failed"},
			state: model.JobFailed, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, ErrTextFWUnreachable) },
		},
		{
			// Only the connection failure is final; a 404 or a short download keeps the retry rule.
			name: "upgrade_issued/upgrade_report other HTTP error", in: ui, ev: Event{Kind: EvUpgradeReport, Err: "Failed HTTP error: 404"},
			state: model.JobSetOtaURL, actions: []ActionKind{ActSetOtaURL},
		},
		{
			// A cancel that waited for the stage still ends as cancelled, with the reason.
			name: "upgrade_issued/upgrade_report connection failed/cancel requested", in: func() model.Job { j := ui; j.CancelRequested = true; return j }(),
			ev:    Event{Kind: EvUpgradeReport, Err: "Failed HTTP error: connection failed"},
			state: model.JobCancelled, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, ErrTextFWUnreachable) },
		},
		{
			// "Successful" over MQTT is a hint only; the probe decides.
			name: "upgrade_issued/upgrade_report Successful", in: ui, ev: Event{Kind: EvUpgradeReport, Err: ""},
			state: model.JobUpgradeIssued, actions: nil,
		},
		{
			name: "upgrade_issued/timeout/Attempt < Max", in: ui, ev: Event{Kind: EvTimeout},
			state: model.JobSetOtaURL, actions: []ActionKind{ActSetOtaURL},
		},
		{
			name: "upgrade_issued/timeout/Attempt = Max", in: uiMax, ev: Event{Kind: EvTimeout},
			state: model.JobFailed, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, ErrTextNoUpgradeStart) },
		},
		{
			// MaxAttempts is configurable: with 3, a second attempt still retries.
			name: "upgrade_issued/timeout/Attempt < configured Max", in: uiMax, ev: Event{Kind: EvTimeout},
			cfg:   func(c *Config) { c.MaxAttempts = 3 },
			state: model.JobSetOtaURL, actions: []ActionKind{ActSetOtaURL},
		},
		{
			// Guard re-runs before Upgrade; Upgrade was not sent.
			name: "upgrade_issued/guard_refused", in: ui, ev: Event{Kind: EvGuardRefused, Err: "safeboot"},
			state: model.JobFailed, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, "safeboot") },
		},
	})
}

func withUptime(j model.Job, u int64) model.Job { j.UptimeBefore = u; return j }

func TestStepAwaitReboot(t *testing.T) {
	ar := job(model.JobAwaitReboot, model.StageMinimal)
	ar.DeadlineAt = t0.Add(time.Minute)
	ar.SeenOffline = true // entered from upgrade_issued/probe_offline
	// Waiting without having seen the device drop off: after Tasmota's
	// auto-continue or while a download may still be retrying.
	quiet := ar
	quiet.SeenOffline = false
	quietAC := withPlan(quiet, model.PlanDirect)
	quietAC.Stage, quietAC.AutoContinueSeen = model.StageFinal, true
	runRows(t, []row{
		{
			name: "await_reboot/probe_offline/first drop-off", in: quiet, ev: Event{Kind: EvProbeOffline},
			state: model.JobAwaitReboot, actions: []ActionKind{ActProbe},
			check: func(t *testing.T, j model.Job, _ []Action) {
				if !j.SeenOffline {
					t.Fatal("SeenOffline not set")
				}
				wantDeadline(t, j, time.Minute)
			},
		},
		{
			// The minimal image is up but has not started propagating
			// yet: never treat that as a result (it would lead to a second
			// Upgrade into a device that is about to download).
			name: "await_reboot/probe_online/not yet dropped off, intermediate image", in: quietAC, ev: Event{Kind: EvProbeOnline, Identity: minimalIdent("16.0.0")},
			state: model.JobAwaitReboot, actions: []ActionKind{ActProbe},
			check: func(t *testing.T, _ model.Job, as []Action) { wantDelay(t, as, 5*time.Second) },
		},
		{
			name: "await_reboot/probe_online/not yet dropped off, old image", in: quiet, ev: Event{Kind: EvProbeOnline, Identity: plug1M()},
			state: model.JobAwaitReboot, actions: []ActionKind{ActProbe},
		},
		{
			name: "await_reboot/probe_online/not yet dropped off, unknown image", in: quiet, ev: Event{Kind: EvProbeOnline},
			state: model.JobAwaitReboot, actions: []ActionKind{ActProbe},
		},
		{
			name: "await_reboot/probe_online/not dropped off but new image", in: quietAC, ev: Event{Kind: EvProbeOnline, Identity: finalIdent()},
			state: model.JobVerify, actions: []ActionKind{ActProbeIdentity},
		},
		{
			name: "await_reboot/probe_online/not dropped off but other variant", in: quiet, ev: Event{Kind: EvProbeOnline, Identity: withVariant(plug1M(), "sensors")},
			state: model.JobVerify, actions: []ActionKind{ActProbeIdentity},
		},
		{
			// Online on the old image for the whole window: the download
			// failed and is over, so a new attempt is safe.
			name: "await_reboot/timeout/never dropped off, Attempt < Max", in: quiet, ev: Event{Kind: EvTimeout},
			state: model.JobSetOtaURL, actions: []ActionKind{ActSetOtaURL},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, ErrTextNoEffect) },
		},
		{
			name: "await_reboot/timeout/never dropped off, Attempt = Max", in: withAttempt(quiet, 2), ev: Event{Kind: EvTimeout},
			state: model.JobFailed, actions: []ActionKind{ActNotify},
		},
		{
			name: "await_reboot/probe_offline", in: ar, ev: Event{Kind: EvProbeOffline},
			state: model.JobAwaitReboot, actions: []ActionKind{ActProbe},
			check: func(t *testing.T, j model.Job, as []Action) {
				wantDelay(t, as, 5*time.Second)
				wantDeadline(t, j, time.Minute) // the reboot window is not extended by polling
			},
		},
		{
			name: "await_reboot/probe_online", in: ar, ev: Event{Kind: EvProbeOnline, Identity: minimalIdent("16.0.0")},
			state: model.JobVerify, actions: []ActionKind{ActProbeIdentity},
			check: func(t *testing.T, j model.Job, _ []Action) { wantDeadline(t, j, time.Minute) },
		},
		{
			// Why (L5): a staged job over MQTT can leave the device on minimal; the text says what to do.
			name: "await_reboot/timeout/over MQTT", in: func() model.Job { j := ar; j.Transport = model.TransportMQTT; return j }(), ev: Event{Kind: EvTimeout},
			state: model.JobFailed, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, "restart the device and retry") },
		},
		{
			name: "await_reboot/timeout", in: ar, ev: Event{Kind: EvTimeout},
			state: model.JobFailed, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, ErrTextNotBack) },
		},
	})
}

func TestStepVerify(t *testing.T) {
	vMin := job(model.JobVerify, model.StageMinimal)
	vMin.DeadlineAt = t0.Add(time.Minute)
	vMinCancel := vMin
	vMinCancel.CancelRequested = true
	vFinal := job(model.JobVerify, model.StageFinal)
	vFinal.DeadlineAt = t0.Add(time.Minute)
	vDirect := withPlan(vFinal, model.PlanDirect)
	ident := func(id model.Identity, ota string, up int64) Event {
		return Event{Kind: EvIdentity, Identity: id, OtaURL: ota, Uptime: up}
	}
	langDE := finalIdent()
	langDE.Language = "DE"
	sensors := finalIdent()
	sensors.Variant = "sensors"
	esp32 := finalIdent()
	esp32.Family = model.ChipESP32
	otherHW := finalIdent()
	otherHW.Hardware = "ESP8266EX"
	noHW := minimalIdent("16.0.0") // minimal cannot report Hardware
	unknownFam := finalIdent()
	unknownFam.Family = model.ChipUnknown
	safeboot := model.Identity{Family: model.ChipESP8266, Version: "16.0.0", Variant: "safeboot"}
	// ESP32-S2: both builds report the shared image tag, so the device reads back "unknown" (#88).
	s2Job := withPlan(job(model.JobVerify, model.StageFinal), model.PlanDirect)
	s2Job.DeadlineAt = t0.Add(time.Minute)
	s2Job.IdentityBefore = model.Identity{Family: model.ChipESP32S2, Hardware: "ESP32-S2", Version: "15.6.0", Variant: "unknown"}
	s2Job.TargetBuild = "s2cdc"
	s2After := s2Job.IdentityBefore
	s2After.Version = "16.0.0"
	s2Tasmota := s2After
	s2Tasmota.Variant = "tasmota"
	autoNoted := vDirect
	autoNoted.AutoContinueSeen = true
	autoNotedLate := autoNoted
	autoNotedLate.DeadlineAt = t0
	oldNoProof := withAttempt(vDirect, 1)

	runRows(t, []row{
		{
			// Minimal stage done: final stage starts with fresh attempts and
			// the minimal uptime as the new reboot reference.
			name: "verify/identity/stage minimal, variant minimal", in: vMin, ev: ident(noHW, minimalURL, 12),
			state: model.JobSetOtaURL, actions: []ActionKind{ActSetOtaURL},
			check: func(t *testing.T, j model.Job, _ []Action) {
				if j.Stage != model.StageFinal || j.Attempt != 0 || j.UptimeBefore != 12 || j.SeenOffline || j.LastError != "" {
					t.Fatalf("final stage not prepared: %+v", j)
				}
				wantDeadline(t, j, 30*time.Second)
			},
		},
		{
			name: "verify/identity/stage minimal, variant minimal, CancelRequested", in: vMinCancel, ev: ident(noHW, minimalURL, 12),
			state: model.JobCancelled, actions: []ActionKind{ActNotify},
		},
		{
			name: "verify/identity/stage final, as planned", in: vFinal, ev: ident(finalIdent(), minimalURL, 3),
			state: model.JobRestoreOtaURL, actions: []ActionKind{ActRestoreOtaURL},
			check: func(t *testing.T, j model.Job, as []Action) {
				if as[0].URL != origURL {
					t.Fatalf("restore URL = %q, want %q", as[0].URL, origURL)
				}
				wantDeadline(t, j, 30*time.Second)
			},
		},
		{
			// Why (H1): over MQTT the language cannot be read, so the probe says "unknown" and the job's reference
			// (the person's choice) must not make a successful flash look like "not taken".
			name: "verify/identity/stage final, language chosen, probe cannot read it", in: func() model.Job {
				j := vFinal
				j.IdentityBefore.Language, j.IdentityBefore.LanguageKnown = "DE", true
				return j
			}(),
			ev:    ident(finalIdent(), minimalURL, 3),
			state: model.JobRestoreOtaURL, actions: []ActionKind{ActRestoreOtaURL},
		},
		{
			// A language that WAS read and differs still contradicts the plan.
			name: "verify/identity/stage final, language read and different", in: func() model.Job {
				j := vFinal
				j.IdentityBefore.Language, j.IdentityBefore.LanguageKnown = "DE", true
				return j
			}(),
			ev:    func() Event { e := ident(finalIdent(), minimalURL, 3); e.Identity.LanguageKnown = true; return e }(),
			state: model.JobFailed, actions: []ActionKind{ActNotify},
		},
		{
			name: "verify/identity/stage final, as planned, restore disabled", in: vFinal, ev: ident(finalIdent(), minimalURL, 3),
			cfg:   func(c *Config) { c.RestoreOtaURL = false },
			state: model.JobSucceeded, actions: []ActionKind{ActNotify},
		},
		{
			name: "verify/identity/stage final, as planned, nothing to restore (empty)", in: withOrig(vFinal, ""), ev: ident(finalIdent(), minimalURL, 3),
			state: model.JobSucceeded, actions: []ActionKind{ActNotify},
		},
		{
			// No flash write when the device already has the original URL.
			name: "verify/identity/stage final, as planned, nothing to restore (equal)", in: vFinal, ev: ident(finalIdent(), origURL, 3),
			state: model.JobSucceeded, actions: []ActionKind{ActNotify},
		},
		{
			name: "verify/identity/stage final, as planned, CancelRequested", in: withCancel(vFinal), ev: ident(finalIdent(), minimalURL, 3),
			state: model.JobSucceeded, actions: []ActionKind{ActNotify},
		},
		{
			// Tasmota's own fallback flashed minimal/safeboot and continues
			// by itself; give it one reboot window.
			name: "verify/identity/stage final but device on minimal (auto-continue)", in: vDirect, ev: ident(minimalIdent("16.0.0"), "", 0),
			state: model.JobAwaitReboot, actions: []ActionKind{ActProbe},
			check: func(t *testing.T, j model.Job, _ []Action) {
				wantDeadline(t, j, 8*time.Minute)
				if !j.AutoContinueSeen {
					t.Fatal("auto-continue not noted")
				}
			},
		},
		{
			name: "verify/identity/stage final but device on safeboot (auto-continue)", in: vDirect, ev: ident(safeboot, "", 0),
			state: model.JobAwaitReboot, actions: []ActionKind{ActProbe},
		},
		{
			// Only once: the second time it counts as the old image.
			name: "verify/identity/stage final on minimal again, window over (treat as old)", in: autoNotedLate, ev: ident(minimalIdent("16.0.0"), "", 0),
			state: model.JobSetOtaURL, actions: []ActionKind{ActSetOtaURL},
		},
		{
			name: "verify/identity/stage final on minimal again, window over, Attempt = Max", in: withAttempt(autoNotedLate, 2), ev: ident(minimalIdent("16.0.0"), "", 0),
			state: model.JobFailed, actions: []ActionKind{ActNotify},
		},
		{
			// Minimal reports no uptime, so there is no reboot proof: wait.
			name: "verify/identity/stage final on minimal again, window open (keep waiting)", in: autoNoted, ev: ident(minimalIdent("16.0.0"), "", 0),
			state: model.JobAwaitReboot, actions: []ActionKind{ActProbe},
			check: func(t *testing.T, j model.Job, _ []Action) {
				if j.SeenOffline {
					t.Fatal("SeenOffline must be cleared to wait for a new drop-off")
				}
				wantDeadline(t, j, time.Minute)
			},
		},
		{
			// Still old, uptime not reset, window open: Tasmota may be
			// between download attempts. No Upgrade into that.
			name: "verify/identity/still old version without reboot proof, window open", in: oldNoProof, ev: ident(plug1M(), "", 6000),
			state: model.JobAwaitReboot, actions: []ActionKind{ActProbe},
			check: func(t *testing.T, j model.Job, _ []Action) {
				if j.SeenOffline || j.Attempt != 1 || j.UptimeBefore != 5000 {
					t.Fatalf("job %+v", j)
				}
			},
		},
		{
			name: "verify/identity/still old version without reboot proof, unknown uptime", in: oldNoProof, ev: ident(plug1M(), "", 0),
			state: model.JobAwaitReboot, actions: []ActionKind{ActProbe},
		},
		{
			name: "verify/identity/still old version, uptime equal (no proof)", in: oldNoProof, ev: ident(plug1M(), "", 5000),
			state: model.JobAwaitReboot, actions: []ActionKind{ActProbe},
		},
		{
			name: "verify/identity/still old version without reboot proof, window over", in: withDeadline(oldNoProof, t0), ev: ident(plug1M(), "", 6000),
			state: model.JobSetOtaURL, actions: []ActionKind{ActSetOtaURL},
			check: func(t *testing.T, j model.Job, _ []Action) {
				if j.UptimeBefore != 6000 {
					t.Fatalf("UptimeBefore = %d, want 6000", j.UptimeBefore)
				}
			},
		},
		{
			name: "verify/identity/family changed", in: vFinal, ev: ident(esp32, "", 0),
			state: model.JobFailed, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, ErrTextHardwareChanged) },
		},
		{
			name: "verify/identity/hardware changed", in: vFinal, ev: ident(otherHW, "", 0),
			state: model.JobFailed, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, ErrTextHardwareChanged) },
		},
		{
			name: "verify/identity/hardware changed in minimal stage", in: vMin, ev: ident(esp32, "", 0),
			state: model.JobFailed, actions: []ActionKind{ActNotify},
		},
		{
			// A full firmware that cannot name its family is not "as planned".
			name: "verify/identity/stage final, unknown family", in: vFinal, ev: ident(unknownFam, "", 0),
			state: model.JobFailed, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, ErrTextUnexpected) },
		},
		{
			name: "verify/identity/still old version/Attempt < Max", in: withAttempt(vDirect, 1), ev: ident(plug1M(), "", 77),
			state: model.JobSetOtaURL, actions: []ActionKind{ActSetOtaURL},
			check: func(t *testing.T, j model.Job, _ []Action) {
				if j.UptimeBefore != 77 {
					t.Fatalf("UptimeBefore = %d, want 77 (new reboot reference)", j.UptimeBefore)
				}
				wantErrContains(t, j, ErrTextNoEffect)
			},
		},
		{
			name: "verify/identity/still old version/Attempt = Max", in: withAttempt(vDirect, 2), ev: ident(plug1M(), "", 77),
			state: model.JobFailed, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, ErrTextNoEffect) },
		},
		{
			name: "verify/identity/still old version in minimal stage", in: vMin, ev: ident(plug1M(), "", 77),
			state: model.JobSetOtaURL, actions: []ActionKind{ActSetOtaURL},
		},
		{
			// Staged final stage: still on minimal means the final download failed.
			name: "verify/identity/still minimal in staged final stage", in: vFinal, ev: ident(minimalIdent("16.0.0"), "", 77),
			state: model.JobSetOtaURL, actions: []ActionKind{ActSetOtaURL},
		},
		{
			// The verify bug of #88: comparing with TargetVariant "tasmota" ended every correct S2 update as "not taken".
			name: "verify/identity/esp32-s2 reports the shared tag (variant unknown)", in: s2Job, ev: ident(s2After, "", 3),
			state: model.JobRestoreOtaURL, actions: []ActionKind{ActRestoreOtaURL},
		},
		{
			// Version, family and reboot proof still decide: an S2 still on the old version is not a success.
			name: "verify/identity/esp32-s2 still on the old version", in: s2Job, ev: ident(s2Job.IdentityBefore, "", 77),
			state: model.JobSetOtaURL, actions: []ActionKind{ActSetOtaURL},
		},
		{
			// An S2 cannot report "tasmota": a result that does is not what was installed.
			name: "verify/identity/esp32-s2 reporting a provable variant", in: s2Job, ev: ident(s2Tasmota, "", 3),
			state: model.JobFailed, actions: []ActionKind{ActNotify},
		},
		{
			name: "verify/identity/wrong variant", in: vFinal, ev: ident(sensors, "", 0),
			state: model.JobFailed, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, ErrTextUnexpected) },
		},
		{
			name: "verify/identity/wrong language", in: vFinal, ev: ident(func() model.Identity { i := langDE; i.LanguageKnown = true; return i }(), "", 0),
			state: model.JobFailed, actions: []ActionKind{ActNotify},
		},
		{
			name: "verify/identity/wrong version, not old", in: vDirect, ev: ident(withVersion(finalIdent(), "15.7.0"), "", 0),
			state: model.JobFailed, actions: []ActionKind{ActNotify},
		},
		{
			name: "verify/identity/minimal stage, unexpected image", in: vMin, ev: ident(sensors, "", 0),
			state: model.JobFailed, actions: []ActionKind{ActNotify},
		},
		{
			// Verify and await_reboot share one deadline so they cannot
			// ping-pong forever.
			name: "verify/command_failed", in: vFinal, ev: Event{Kind: EvCommandFailed},
			state: model.JobAwaitReboot, actions: []ActionKind{ActProbe},
			check: func(t *testing.T, j model.Job, as []Action) {
				wantDeadline(t, j, time.Minute)
				wantDelay(t, as, 5*time.Second)
			},
		},
		{
			name: "verify/timeout", in: vFinal, ev: Event{Kind: EvTimeout},
			state: model.JobAwaitReboot, actions: []ActionKind{ActProbe},
		},
	})
}

func withDeadline(j model.Job, d time.Time) model.Job       { j.DeadlineAt = d; return j }
func withVariant(i model.Identity, v string) model.Identity { i.Variant = v; return i }
func withOrig(j model.Job, u string) model.Job              { j.OriginalOtaURL = u; return j }
func withCancel(j model.Job) model.Job                      { j.CancelRequested = true; return j }
func withVersion(i model.Identity, v string) model.Identity { i.Version = v; return i }

func TestStepRestoreOtaURL(t *testing.T) {
	r := job(model.JobRestoreOtaURL, model.StageFinal)
	runRows(t, []row{
		{
			name: "restore_ota_url/command_ok", in: r, ev: Event{Kind: EvCommandOK},
			state: model.JobSucceeded, actions: []ActionKind{ActNotify},
		},
		{
			// The firmware is updated; a failed restore is reported, not fatal.
			name: "restore_ota_url/command_failed", in: r, ev: Event{Kind: EvCommandFailed, Err: "timeout"},
			state: model.JobSucceeded, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, ErrTextRestoreFailed) },
		},
		{
			name: "restore_ota_url/timeout", in: r, ev: Event{Kind: EvTimeout},
			state: model.JobSucceeded, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, ErrTextRestoreFailed) },
		},
	})
}

func TestStepCancel(t *testing.T) {
	var rows []row
	for _, s := range []model.JobState{model.JobQueued, model.JobPreflight, model.JobBackup, model.JobSetOtaURL} {
		rows = append(rows, row{
			name: string(s) + "/cancel", in: job(s, model.StageMinimal), ev: Event{Kind: EvCancel},
			state: model.JobCancelled, actions: []ActionKind{ActNotify},
		})
	}
	for _, s := range []model.JobState{model.JobUpgradeIssued, model.JobAwaitReboot, model.JobVerify} {
		in := job(s, model.StageMinimal)
		rows = append(rows, row{
			// The device may be writing flash: the stage must finish.
			name: string(s) + "/cancel", in: in, ev: Event{Kind: EvCancel},
			state: s, actions: nil,
			check: func(t *testing.T, j model.Job, _ []Action) {
				want := in
				want.CancelRequested = true
				want.UpdatedAt = t0
				if !reflect.DeepEqual(j, want) {
					t.Fatalf("got %+v, want only CancelRequested set", j)
				}
			},
		})
	}
	runRows(t, rows)
}

func TestStepResume(t *testing.T) {
	cases := []struct {
		state    model.JobState
		actions  []ActionKind
		deadline time.Duration // 0: unchanged
	}{
		{model.JobQueued, nil, 0},
		{model.JobPreflight, []ActionKind{ActProbeIdentity}, 30 * time.Second},
		{model.JobVerify, []ActionKind{ActProbeIdentity}, 8 * time.Minute},
		{model.JobBackup, []ActionKind{ActBackup}, 0},
		{model.JobSetOtaURL, []ActionKind{ActSetOtaURL}, 30 * time.Second},
		// The core safety rule: after a restart Upgrade is never re-sent.
		{model.JobUpgradeIssued, []ActionKind{ActProbe}, 8 * time.Minute},
		{model.JobAwaitReboot, []ActionKind{ActProbe}, 8 * time.Minute},
		{model.JobRestoreOtaURL, []ActionKind{ActRestoreOtaURL}, 30 * time.Second},
		{model.JobRestorePower, []ActionKind{ActRestorePower}, 30 * time.Second},
	}
	for _, c := range cases {
		t.Run(string(c.state)+"/resume", func(t *testing.T) {
			in := job(c.state, model.StageMinimal)
			if c.state == model.JobRestorePower {
				in.PowerOn, in.PowerReadAt = 1, t0 // something recent to restore
			}
			got, as := Step(in, Event{Kind: EvResume}, t0, testCfg())
			if got.State != c.state {
				t.Fatalf("resume changed state to %s", got.State)
			}
			if !reflect.DeepEqual(kinds(as), c.actions) {
				t.Fatalf("actions = %v, want %v", kinds(as), c.actions)
			}
			for _, a := range as {
				if a.Kind == ActUpgrade || a.Delay != 0 {
					t.Fatalf("resume action %+v: no Upgrade, no delay", a)
				}
				if a.Kind == ActRestoreOtaURL && a.URL != origURL {
					t.Fatalf("restore URL %q", a.URL)
				}
			}
			if c.deadline == 0 {
				if !got.DeadlineAt.Equal(in.DeadlineAt) {
					t.Fatal("deadline changed")
				}
			} else {
				wantDeadline(t, got, c.deadline)
			}
		})
	}
}

var (
	allStates = []model.JobState{
		model.JobQueued, model.JobPreflight, model.JobBackup, model.JobSetOtaURL, model.JobUpgradeIssued,
		model.JobAwaitReboot, model.JobVerify, model.JobRestoreOtaURL, model.JobRestorePower,
		model.JobSucceeded, model.JobFailed, model.JobCancelled,
	}
	allEvents = []EventKind{
		EvStart, EvResume, EvIdentity, EvPlanned, EvCommandOK, EvCommandFailed, EvGuardRefused,
		EvProbeOnline, EvProbeOffline, EvTimeout, EvBackupDone, EvBackupFailed, EvUpgradeReport, EvPowerReport, EvCancel,
	}
)

// handled lists every (state, event) pair the transition table defines,
// besides resume and cancel which are covered by their own tests.
var handled = map[model.JobState][]EventKind{
	model.JobQueued:        {EvStart},
	model.JobPreflight:     {EvIdentity, EvPlanned, EvGuardRefused, EvCommandFailed, EvTimeout, EvPowerReport},
	model.JobBackup:        {EvBackupDone, EvBackupFailed, EvPowerReport},
	model.JobSetOtaURL:     {EvCommandOK, EvCommandFailed, EvTimeout, EvGuardRefused, EvPowerReport},
	model.JobUpgradeIssued: {EvCommandOK, EvCommandFailed, EvProbeOffline, EvProbeOnline, EvUpgradeReport, EvTimeout, EvGuardRefused, EvPowerReport},
	model.JobAwaitReboot:   {EvProbeOffline, EvProbeOnline, EvTimeout},
	model.JobVerify:        {EvIdentity, EvCommandFailed, EvTimeout},
	model.JobRestoreOtaURL: {EvCommandOK, EvCommandFailed, EvTimeout},
	model.JobRestorePower:  {EvCommandOK, EvCommandFailed, EvTimeout},
}

func isHandled(s model.JobState, k EventKind) bool {
	if k == EvResume || k == EvCancel {
		return true
	}
	for _, h := range handled[s] {
		if h == k {
			return true
		}
	}
	return false
}

// fullEvent fills every field so that a buggy machine would have
// something to (wrongly) copy into the job.
func fullEvent(k EventKind) Event {
	return Event{Kind: k, Identity: finalIdent(), OtaURL: "http://x/y.bin.gz", Uptime: 1, Err: "Failed x",
		Skip: true, Power: 5, Plan: model.PlanDirect, TargetVariant: "lite", TargetArtifact: 99, MinimalArtifact: 98}
}

// Unknown (state, event) pairs must not move a job: a stray or late event
// (an old probe result, a duplicate MQTT hint) must never advance an OTA.
func TestStepUnknownPairsLeaveJobUnchanged(t *testing.T) {
	for _, s := range allStates {
		if s.Terminal() {
			continue
		}
		for _, k := range allEvents {
			if isHandled(s, k) {
				continue
			}
			in := job(s, model.StageMinimal)
			in.PowerOn, in.PowerReadAt = 5, t0 // so that a wrongly accepted power report would show
			got, as := Step(in, fullEvent(k), t0, testCfg())
			if !reflect.DeepEqual(got, in) || len(as) != 0 {
				t.Errorf("%s/%s changed the job or produced actions %v", s, k, kinds(as))
			}
		}
	}
	// Cancel during restore is too late to matter.
	for _, st := range []model.JobState{model.JobRestoreOtaURL, model.JobRestorePower} {
		in := job(st, model.StageFinal)
		if got, as := Step(in, Event{Kind: EvCancel}, t0, testCfg()); !reflect.DeepEqual(got, in) || len(as) != 0 {
			t.Errorf("%s/cancel changed the job", st)
		}
	}
	in := job(model.JobRestoreOtaURL, model.StageFinal)
	if got, as := Step(in, Event{Kind: EvCancel}, t0, testCfg()); !reflect.DeepEqual(got, in) || len(as) != 0 {
		t.Error("restore_ota_url/cancel changed the job")
	}
}

// Terminal states absorb everything, including resume and cancel: a
// finished job must never touch a device again.
func TestStepTerminalStatesAbsorbEverything(t *testing.T) {
	for _, s := range []model.JobState{model.JobSucceeded, model.JobFailed, model.JobCancelled} {
		for _, k := range allEvents {
			in := job(s, model.StageFinal)
			got, as := Step(in, fullEvent(k), t0, testCfg())
			if !reflect.DeepEqual(got, in) || len(as) != 0 {
				t.Errorf("terminal %s/%s changed the job or produced actions %v", s, k, kinds(as))
			}
		}
	}
}

// Every handled pair must either change the state or emit actions or
// change the job; otherwise the job could stall with nothing scheduled.
func TestStepHandledPairsNeverStall(t *testing.T) {
	for s, evs := range handled {
		for _, k := range evs {
			in := job(s, model.StageMinimal)
			got, as := Step(in, fullEvent(k), t0, testCfg())
			if reflect.DeepEqual(got, in) && len(as) == 0 && k != EvUpgradeReport && k != EvPowerReport {
				t.Errorf("%s/%s neither changed the job nor scheduled an action", s, k)
			}
			if !got.State.Terminal() && len(as) == 0 && k != EvPowerReport && got.State != model.JobUpgradeIssued && got.State != model.JobAwaitReboot && got.State != model.JobVerify {
				t.Errorf("%s/%s -> %s has no follow-up action", s, k, got.State)
			}
		}
	}
}

// Upgrade is only ever requested by the set_ota_url -> upgrade_issued
// transition. Every other path must not send it.
func TestStepUpgradeOnlyFromSetOtaURL(t *testing.T) {
	for _, s := range allStates {
		for _, k := range allEvents {
			for _, att := range []int{0, 1, 2} {
				_, as := Step(withAttempt(job(s, model.StageMinimal), att), fullEvent(k), t0, testCfg())
				for _, a := range as {
					if a.Kind == ActUpgrade && (s != model.JobSetOtaURL || k != EvCommandOK) {
						t.Errorf("%s/%s requested Upgrade", s, k)
					}
				}
			}
		}
	}
}

// A full staged run through the machine: the minimal hop, the final image
// and the OtaUrl restore, as the 1 MB ESP8266 fleet needs it.
func TestStepStagedHappyPath(t *testing.T) {
	cfg := testCfg()
	j := model.Job{ID: 1, State: model.JobQueued, TargetRelease: "16.0.0", Policy: model.PolicyOnlyNewer}
	var as []Action
	steps := []struct {
		ev   Event
		want model.JobState
	}{
		{Event{Kind: EvStart}, model.JobPreflight},
		{Event{Kind: EvIdentity, Identity: plug1M(), OtaURL: origURL, Uptime: 5000}, model.JobPreflight},
		{Event{Kind: EvPlanned, Plan: model.PlanStaged, TargetVariant: "tasmota", TargetArtifact: 2, MinimalArtifact: 3}, model.JobBackup},
		{Event{Kind: EvBackupDone}, model.JobSetOtaURL},
		{Event{Kind: EvCommandOK}, model.JobUpgradeIssued},
		{Event{Kind: EvCommandFailed}, model.JobUpgradeIssued},
		{Event{Kind: EvProbeOffline}, model.JobAwaitReboot},
		{Event{Kind: EvProbeOnline}, model.JobVerify},
		{Event{Kind: EvIdentity, Identity: minimalIdent("16.0.0"), OtaURL: minimalURL, Uptime: 4}, model.JobSetOtaURL},
		{Event{Kind: EvCommandOK}, model.JobUpgradeIssued},
		{Event{Kind: EvProbeOnline, Identity: finalIdent(), Uptime: 2}, model.JobVerify},
		{Event{Kind: EvIdentity, Identity: finalIdent(), OtaURL: minimalURL, Uptime: 3}, model.JobRestoreOtaURL},
		{Event{Kind: EvCommandOK}, model.JobSucceeded},
	}
	for i, s := range steps {
		j, as = Step(j, s.ev, t0, cfg)
		if j.State != s.want {
			t.Fatalf("step %d (%s): state %s, want %s (%s)", i, s.ev.Kind, j.State, s.want, j.LastError)
		}
	}
	if j.LastError != "" || len(as) != 1 || as[0].Kind != ActNotify {
		t.Fatalf("end: LastError %q actions %v", j.LastError, kinds(as))
	}
}

// The defaults are the documented ones (spec §9.5); the reasons are there:
// UpgradeAccept covers the blocked web server during download, RebootWindow
// covers ten ESP8266 download retries plus boot and Wi-Fi join.
func TestDefaultConfigMatchesSpec(t *testing.T) {
	if got := DefaultConfig(); got != testCfg() {
		t.Fatalf("DefaultConfig() = %+v, want %+v", got, testCfg())
	}
}

// Why: a push upload runs inside upgrade_issued and can use up its whole
// window (live 2026-10-24: three minutes). The window for the device to drop
// off or reboot must start when the upload has ended, or the job times out
// without a single probe. After the window a push job fails instead of
// uploading again: whether the image is still being written is unknown, and
// an image is never sent twice (safety rule 3).
func TestStepUpgradeIssuedPush(t *testing.T) {
	push := job(model.JobUpgradeIssued, model.StageFinal)
	push.Delivery = model.DeliveryPush
	push.DeadlineAt = t0.Add(-time.Second) // the upload took the whole window
	pushCancel := push
	pushCancel.CancelRequested = true
	pull := job(model.JobUpgradeIssued, model.StageFinal)
	pullCancel := pull
	pullCancel.CancelRequested = true

	runRows(t, []row{
		{
			name: "push/command_failed restarts the window and keeps the reason", in: push, ev: Event{Kind: EvCommandFailed, Err: "upload: timeout"},
			state: model.JobUpgradeIssued, actions: []ActionKind{ActProbe},
			check: func(t *testing.T, j model.Job, as []Action) {
				wantDeadline(t, j, 3*time.Minute)
				wantDelay(t, as, 5*time.Second)
				if j.Attempt != push.Attempt {
					t.Fatalf("Attempt changed to %d", j.Attempt)
				}
			},
		},
		{
			name: "push/command_ok restarts the window", in: push, ev: Event{Kind: EvCommandOK},
			state: model.JobUpgradeIssued, actions: []ActionKind{ActProbe},
			check: func(t *testing.T, j model.Job, _ []Action) { wantDeadline(t, j, 3*time.Minute) },
		},
		{
			name: "push/timeout fails, no second upload", in: push, ev: Event{Kind: EvTimeout},
			state: model.JobFailed, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, ErrTextNoUpgradeStart) },
		},
		{
			name: "push/timeout after cancel ends cancelled", in: pushCancel, ev: Event{Kind: EvTimeout},
			state: model.JobCancelled, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, ErrTextNoUpgradeStart) },
		},
		{
			// A cancelled job must not start the stage again: that would
			// send OtaUrl and Upgrade 1 after the user said stop.
			name: "pull/timeout after cancel ends cancelled, no retry", in: pullCancel, ev: Event{Kind: EvTimeout},
			state: model.JobCancelled, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, ErrTextNoUpgradeStart) },
		},
		{
			name: "pull/command_failed keeps the window", in: pull, ev: Event{Kind: EvCommandFailed, Err: "timeout"},
			state: model.JobUpgradeIssued, actions: []ActionKind{ActProbe},
			check: func(t *testing.T, j model.Job, _ []Action) {
				if !j.DeadlineAt.Equal(pull.DeadlineAt) {
					t.Fatalf("DeadlineAt moved to %v", j.DeadlineAt)
				}
			},
		},
	})
}
