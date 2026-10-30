package ota

import (
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// The upload over MQTT (D19) reuses the push states. These rows pin what differs from the upload over
// HTTP: an unconfirmed upload is recorded as such instead of as a failure, an interrupted one is never
// continued, and nothing is ever sent twice.

// mqttPushJob is an upload job through the broker in upgrade_issued, as the engine leaves it when it
// starts the upload: 15.6.0 -> 16.0.0 on a dual-OTA ESP32.
func mqttPushJob() model.Job {
	j := job(model.JobUpgradeIssued, model.StageFinal)
	j.Plan, j.Delivery, j.Transport, j.MQTTUpload = model.PlanDirect, model.DeliveryPush, model.TransportMQTT, true
	j.IdentityBefore = sbESP32("15.6.0", "tasmota32")
	j.Attempt, j.UptimeBefore = 1, 5000
	j.DeadlineAt = t0.Add(time.Minute)
	return j
}

func TestMQTTPushRows(t *testing.T) {
	timedOut := mqttPushJob()
	timedOut.MQTTUploadState = model.MQTTUploadUnconfirmed
	cancelledUnknown := timedOut
	cancelledUnknown.CancelRequested = true
	httpPush := mqttPushJob()
	httpPush.Transport = model.TransportHTTP
	probe := []ActionKind{ActProbe}
	runRows(t, []row{
		// Done: probe for the restart, with a fresh window (the upload may have used the old one).
		{name: "command_ok", in: mqttPushJob(), ev: Event{Kind: EvCommandOK}, state: model.JobUpgradeIssued, actions: probe,
			check: func(t *testing.T, j model.Job, _ []Action) {
				wantDeadline(t, j, testCfg().UpgradeAccept)
				if j.LastError != "" {
					t.Errorf("LastError %q after a confirmed upload", j.LastError)
				}
			}},
		// Why: every byte was sent and nothing confirmed it. The new image may become active at the next
		// restart, so the job keeps probing (a restart is then verified) and remembers why.
		{name: "unknown outcome keeps probing", in: mqttPushJob(), ev: Event{Kind: EvCommandFailed, OutcomeUnknown: true, Err: "x"},
			state: model.JobUpgradeIssued, actions: probe, check: func(t *testing.T, j model.Job, _ []Action) {
				if j.MQTTUploadState != model.MQTTUploadUnconfirmed {
					t.Errorf("MQTTUploadState %q, want unconfirmed", j.MQTTUploadState)
				}
				wantDeadline(t, j, testCfg().UpgradeAccept)
			}},
		// Why: the device did not restart: the end is the unknown outcome, not "did not start the upgrade".
		{name: "unknown outcome, no restart", in: timedOut, ev: Event{Kind: EvTimeout}, state: model.JobFailed, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) {
				wantErrContains(t, j, "may switch to the new firmware at its next restart")
			}},
		{name: "unknown outcome, cancelled meanwhile", in: cancelledUnknown, ev: Event{Kind: EvTimeout}, state: model.JobCancelled, actions: []ActionKind{ActNotify}},
		// Why: a restart after an unconfirmed upload is the new firmware taking over: verified as usual.
		{name: "unknown outcome, restarted", in: timedOut, ev: Event{Kind: EvProbeOnline, Identity: sbESP32("16.0.0", "tasmota32"), Uptime: 30},
			state: model.JobVerify, actions: []ActionKind{ActProbeIdentity}},
		// Why: a refused or aborted upload flashed nothing; it ends at once and is not an unknown outcome.
		{name: "not flashed", in: mqttPushJob(), ev: Event{Kind: EvCommandFailed, NotFlashed: true, Err: "cut"}, state: model.JobFailed, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, "nothing was flashed") }},
		// Why: the flag belongs to the upload over MQTT; an upload over HTTP does not become an unknown outcome.
		{name: "unknown flag ignored for an HTTP upload", in: httpPush, ev: Event{Kind: EvCommandFailed, OutcomeUnknown: true, Err: "x"},
			state: model.JobUpgradeIssued, actions: probe, check: func(t *testing.T, j model.Job, _ []Action) {
				if j.MQTTUploadState != model.MQTTUploadClean {
					t.Errorf("MQTTUploadState %q on an HTTP upload", j.MQTTUploadState)
				}
			}},
		// Why: the upload over HTTP keeps its old end.
		{name: "HTTP upload timeout", in: httpPush, ev: Event{Kind: EvTimeout}, state: model.JobFailed, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, ErrTextNoUpgradeStart) }},
		// Why: after Done and no restart nothing says the upload was interrupted.
		{name: "confirmed upload, no restart", in: mqttPushJob(), ev: Event{Kind: EvTimeout}, state: model.JobFailed, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, ErrTextNoUpgradeStart) }},
	})
}

// Why: a partial Type 1 transfer cannot be continued. A resumed job only probes (never uploads) and, if
// the device did not restart, says that the upload was interrupted and the device kept its firmware.
func TestMQTTPushResumeNeverUploadsAgain(t *testing.T) {
	j, as := Step(mqttPushJob(), Event{Kind: EvResume}, t0, testCfg())
	if j.State != model.JobUpgradeIssued || len(as) != 1 || as[0].Kind != ActProbe {
		t.Fatalf("resume: state %s actions %v", j.State, kinds(as))
	}
	if j.MQTTUploadState != model.MQTTUploadInterrupted {
		t.Fatalf("MQTTUploadState %q", j.MQTTUploadState)
	}
	j, _ = Step(j, Event{Kind: EvTimeout}, t0, testCfg())
	if j.State != model.JobFailed || j.LastError != ErrTextUploadInterrupted {
		t.Fatalf("after the window: %s %q", j.State, j.LastError)
	}
	// the wording is hedged: the outcome is not known, so it must not promise the old firmware
	if strings.Contains(ErrTextUploadInterrupted, "kept its firmware") || !strings.Contains(ErrTextUploadInterrupted, "not known") {
		t.Fatalf("unhedged text %q", ErrTextUploadInterrupted)
	}
	// a resume keeps what is already known
	u := mqttPushJob()
	u.MQTTUploadState = model.MQTTUploadUnconfirmed
	if r, _ := Step(u, Event{Kind: EvResume}, t0, testCfg()); r.MQTTUploadState != model.MQTTUploadUnconfirmed {
		t.Fatalf("resume overwrote %q", r.MQTTUploadState)
	}
	// an upload over HTTP is not marked
	h := mqttPushJob()
	h.Transport = model.TransportHTTP
	if r, _ := Step(h, Event{Kind: EvResume}, t0, testCfg()); r.MQTTUploadState != model.MQTTUploadClean {
		t.Fatalf("an HTTP upload was marked %q", r.MQTTUploadState)
	}
}
