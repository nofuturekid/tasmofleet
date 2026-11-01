package templates

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/policy"
)

func wantLayout(t *testing.T, name string, code int) string {
	t.Helper()
	tpl, err := policy.ParseTemplate([]byte(tplJSON(name, code)))
	if err != nil {
		t.Fatal(err)
	}
	return tpl.Fingerprint()
}

// Why: the restart eats the first answers. The read-back waits for the restart to settle, keeps trying
// while the device is unreachable or still shows the old layout, and ends as soon as it shows the new one.
func TestVerifyWaitsThroughTheRestartUntilTheNewLayoutShows(t *testing.T) {
	f := newFleet()
	id := f.add("a", "NOUS A1T", 21)
	d, _ := f.GetDevice(context.Background(), id)
	var waits []time.Duration
	calls := 0
	a := &Applier{Client: scripted{f, func() {
		calls++
		switch calls {
		case 1, 2: // module and template read while the device is down
			f.failRead[id] = true
		case 3:
			f.failRead[id] = false
			f.template[id] = tplJSON("NOUS A1T", 21) // up again, still the old one
		default:
			f.template[id] = tplJSON("NOUS A1T", 52)
		}
	}}, Settle: 6 * time.Second, Pause: 5 * time.Second,
		Wait: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }}
	r, err := a.Verify(context.Background(), d, wantLayout(t, "NOUS A1T", 52), "")
	if err != nil || r.Module != 0 || r.Layout != wantLayout(t, "NOUS A1T", 52) {
		t.Fatalf("%+v %v", r, err)
	}
	if len(waits) < 3 || waits[0] != 6*time.Second || waits[1] != 5*time.Second {
		t.Fatalf("waits %v: the first read must wait for the restart to settle", waits)
	}
}

// Why: a device that never shows the planned layout must fail the job with what it does show, never pass.
func TestVerifyFailsWhenTheLayoutNeverShows(t *testing.T) {
	f := newFleet()
	id := f.add("a", "NOUS A1T", 21)
	d, _ := f.GetDevice(context.Background(), id)
	a := &Applier{Client: f, Attempts: 3, Wait: func(context.Context, time.Duration) error { return nil }}
	_, err := a.Verify(context.Background(), d, wantLayout(t, "NOUS A1T", 52), "")
	if err == nil || !strings.Contains(err.Error(), "still reports") {
		t.Fatalf("err %v", err)
	}
	f.failRead[id] = true
	if _, err := a.Verify(context.Background(), d, wantLayout(t, "NOUS A1T", 52), ""); err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("err %v", err)
	}
}

// Why: a shut-down server must stop waiting at once and not report a verdict.
func TestVerifyStopsWhenTheContextEnds(t *testing.T) {
	f := newFleet()
	id := f.add("a", "NOUS A1T", 21)
	d, _ := f.GetDevice(context.Background(), id)
	a := &Applier{Client: f, Wait: func(context.Context, time.Duration) error { return errors.New("canceled") }}
	if _, err := a.Verify(context.Background(), d, "x", ""); err == nil {
		t.Fatal("no error")
	}
}

// Why: the audit holds the template as JSON without anything else of the device.
func TestReadReturnsTheLayoutAndAnAuditableJSON(t *testing.T) {
	f := newFleet()
	id := f.add("a", "NOUS A1T", 21)
	d, _ := f.GetDevice(context.Background(), id)
	r, err := (&Applier{Client: f}).Read(context.Background(), d)
	if err != nil || r.Module != 0 || r.Layout == "" || !strings.Contains(r.JSON, `"NAME":"NOUS A1T"`) {
		t.Fatalf("%+v %v", r, err)
	}
	var _ model.DeviceID
}

// scripted runs a hook before every command.
type scripted struct {
	*fakeFleet
	hook func()
}

func (s scripted) Command(ctx context.Context, t tasmota.Target, cmnd string) (json.RawMessage, error) {
	s.hook()
	return s.fakeFleet.Command(ctx, t, cmnd)
}

// restartRig is a device whose stored template is already the new one (it changes at once, the restart
// follows seconds later) and whose boot count and uptime the test scripts.
type restartRig struct {
	f      *fakeFleet
	d      model.Device
	now    time.Time
	boot   int
	bootAt time.Time // when the device last booted
	a      *Applier
}

func newRestartRig(t *testing.T) *restartRig {
	t.Helper()
	f := newFleet()
	id := f.add("a", "NOUS A1T", 21)
	d, _ := f.GetDevice(context.Background(), id)
	r := &restartRig{f: f, d: d, now: t0, boot: 40, bootAt: t0.Add(-time.Hour)}
	f.template[id] = tplJSON("NOUS A1T", 52)
	f.status = func(model.DeviceID) (int, time.Duration) { return r.boot, r.now.Sub(r.bootAt) }
	r.a = &Applier{Client: f, Now: func() time.Time { return r.now }, Attempts: 6, Pause: 5 * time.Second, Settle: time.Second}
	return r
}

func (r *restartRig) baseline(t *testing.T) string {
	t.Helper()
	b, err := r.a.Baseline(context.Background(), r.d)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Why (restore's rule R26): the stored template changes at once, long before the device restarts. A
// read-back of the layout alone would call the change done while the old pin map still runs. After the
// baseline the verify waits until the boot count rose AND the uptime is shorter than the time since the
// baseline. The firmware counts the boot at 10 s of uptime, so the count rises after the device is back.
func TestVerifyWaitsForTheBootCountAndAShortUptime(t *testing.T) {
	r := newRestartRig(t)
	base := r.baseline(t)
	r.a.Wait = func(_ context.Context, d time.Duration) error {
		r.now = r.now.Add(d)
		if r.now.After(t0.Add(8*time.Second)) && r.bootAt.Before(t0) {
			r.bootAt = t0.Add(8 * time.Second) // restarted at +8 s, the count follows at 10 s of uptime
		}
		if r.now.After(t0.Add(19 * time.Second)) {
			r.boot = 41
		}
		return nil
	}
	got, err := r.a.Verify(context.Background(), r.d, wantLayout(t, "NOUS A1T", 52), base)
	if err != nil || !got.RestartProven {
		t.Fatalf("%+v %v", got, err)
	}
	if r.now.Before(t0.Add(19 * time.Second)) {
		t.Fatalf("verify returned at +%v, before the boot count rose", r.now.Sub(t0))
	}
}

// Why: a short uptime alone proves nothing: a watchdog or crash-loop reboot BEFORE the command gives a
// short uptime too, and the count may not have moved yet. "Uptime short but BootCount unchanged" is not verified.
func TestVerifyRefusesAShortUptimeWithTheSameBootCount(t *testing.T) {
	r := newRestartRig(t)
	r.bootAt = t0.Add(-3 * time.Second) // rebooted just before the baseline
	base := r.baseline(t)
	r.a.Wait = func(_ context.Context, d time.Duration) error { r.now = r.now.Add(d); return nil }
	_, err := r.a.Verify(context.Background(), r.d, wantLayout(t, "NOUS A1T", 52), base)
	if err == nil || !strings.Contains(err.Error(), "boot count did not rise") {
		t.Fatalf("err %v", err)
	}
}

// Why: and the count alone proves nothing either: it can rise for another reason. An uptime longer than
// the time since the baseline cannot be a restart caused by the send.
func TestVerifyRefusesARisenBootCountWithALongUptime(t *testing.T) {
	r := newRestartRig(t)
	base := r.baseline(t)
	r.a.Wait = func(_ context.Context, d time.Duration) error { r.now = r.now.Add(d); r.boot = 41; return nil }
	if _, err := r.a.Verify(context.Background(), r.d, wantLayout(t, "NOUS A1T", 52), base); err == nil {
		t.Fatal("a long uptime passed as a restart")
	}
}

// Why: no boot count, no baseline, and the engine sends nothing.
func TestBaselineFailsWhenTheBootCountCannotBeRead(t *testing.T) {
	r := newRestartRig(t)
	r.f.status = nil
	if _, err := r.a.Baseline(context.Background(), r.d); err == nil {
		t.Fatal("a baseline without a boot count")
	}
}

// Why (M3): the baseline is a token the engine stores and hands back after a server restart, so it must
// survive that: a second Applier (a new process) proves the restart from the token alone.
func TestVerifyProvesTheRestartFromAStoredBaselineInAnotherProcess(t *testing.T) {
	r := newRestartRig(t)
	base := r.baseline(t)
	r.bootAt, r.boot = t0.Add(2*time.Second), 41
	r.now = t0.Add(30 * time.Second)
	again := &Applier{Client: r.f, Now: func() time.Time { return r.now }, Wait: func(context.Context, time.Duration) error { return nil }}
	got, err := again.Verify(context.Background(), r.d, wantLayout(t, "NOUS A1T", 52), base)
	if err != nil || !got.RestartProven {
		t.Fatalf("%+v %v", got, err)
	}
}

// Why: with no usable baseline (none stored, or a token nobody can read) the layout alone decides, and the
// reading says the restart was not proven so the job can say so.
func TestVerifyWithoutAUsableBaselineChecksTheLayoutOnly(t *testing.T) {
	f := newFleet()
	id := f.add("a", "NOUS A1T", 52)
	d, _ := f.GetDevice(context.Background(), id)
	a := &Applier{Client: f, Wait: func(context.Context, time.Duration) error { return nil }}
	for _, tok := range []string{"", "garbage"} {
		got, err := a.Verify(context.Background(), d, wantLayout(t, "NOUS A1T", 52), tok)
		if err != nil || got.RestartProven {
			t.Fatalf("token %q: %+v %v", tok, got, err)
		}
	}
}

// Why (M1): the device answers a refused Template with {"Template":"Invalid JSON or Bad Chip Type"} and
// does not restart. That must fail the job at once as "nothing applied", not wait out a restart that never
// comes. A successful write is answered with the template itself.
func TestSendTreatsAStringTemplateAnswerAsRefused(t *testing.T) {
	r := newRestartRig(t)
	r.f.writeReply = `{"Template":"Invalid JSON or Bad Chip Type"}`
	err := r.a.Send(context.Background(), r.d, "Template {}")
	if !errors.Is(err, ota.ErrTemplateRefused) || !strings.Contains(err.Error(), "Bad Chip Type") {
		t.Fatalf("refusal: %v", err)
	}
	r.f.writeReply = tplJSON("NOUS A1T", 52)
	if err := r.a.Send(context.Background(), r.d, "Template {}"); err != nil {
		t.Fatalf("success answer: %v", err)
	}
}
