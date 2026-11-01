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
	r, err := a.Verify(context.Background(), d, wantLayout(t, "NOUS A1T", 52))
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
	_, err := a.Verify(context.Background(), d, wantLayout(t, "NOUS A1T", 52))
	if err == nil || !strings.Contains(err.Error(), "still reports") {
		t.Fatalf("err %v", err)
	}
	f.failRead[id] = true
	if _, err := a.Verify(context.Background(), d, wantLayout(t, "NOUS A1T", 52)); err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("err %v", err)
	}
}

// Why: a shut-down server must stop waiting at once and not report a verdict.
func TestVerifyStopsWhenTheContextEnds(t *testing.T) {
	f := newFleet()
	id := f.add("a", "NOUS A1T", 21)
	d, _ := f.GetDevice(context.Background(), id)
	a := &Applier{Client: f, Wait: func(context.Context, time.Duration) error { return errors.New("canceled") }}
	if _, err := a.Verify(context.Background(), d, "x"); err == nil {
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

// Why: the stored template changes at once, long before the device restarts. A read-back that only
// compared the layout would call the change done while the old pin map is still running. After a send
// from this process, the verify waits until the uptime shows a restart.
func TestVerifyWaitsForTheRestartAfterASend(t *testing.T) {
	f := newFleet()
	id := f.add("a", "NOUS A1T", 21)
	d, _ := f.GetDevice(context.Background(), id)
	now := t0
	bootAt := t0.Add(-time.Hour) // the device booted an hour ago
	f.uptime = func(model.DeviceID) time.Duration { return now.Sub(bootAt) }
	f.template[id] = tplJSON("NOUS A1T", 52) // stored template is new at once
	a := &Applier{Client: f, Now: func() time.Time { return now }, Attempts: 6, Pause: 5 * time.Second, Settle: time.Second,
		Wait: func(_ context.Context, d time.Duration) error {
			now = now.Add(d)
			if now.After(t0.Add(12 * time.Second)) {
				bootAt = t0.Add(10 * time.Second) // restarted at +10 s
			}
			return nil
		}}
	if err := a.Send(context.Background(), d, "Template {}"); err == nil {
		// fakeFleet answers only reads; the send itself fails, which the engine tolerates
		t.Fatal("expected the fake to refuse the write")
	}
	if _, err := a.Verify(context.Background(), d, wantLayout(t, "NOUS A1T", 52)); err != nil {
		t.Fatal(err)
	}
	if now.Before(t0.Add(11 * time.Second)) {
		t.Fatalf("verify returned at +%v, before the restart", now.Sub(t0))
	}
}

// Why: a device that keeps its old pin map running (no restart) must fail the job, not pass on the stored layout.
func TestVerifyFailsWhenTheDeviceNeverRestarts(t *testing.T) {
	f := newFleet()
	id := f.add("a", "NOUS A1T", 21)
	d, _ := f.GetDevice(context.Background(), id)
	now := t0
	f.uptime = func(model.DeviceID) time.Duration { return time.Hour + now.Sub(t0) }
	f.template[id] = tplJSON("NOUS A1T", 52)
	a := &Applier{Client: f, Now: func() time.Time { return now }, Attempts: 3, Pause: 5 * time.Second,
		Wait: func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }}
	_ = a.Send(context.Background(), d, "Template {}")
	_, err := a.Verify(context.Background(), d, wantLayout(t, "NOUS A1T", 52))
	if err == nil || !strings.Contains(err.Error(), "has not restarted") {
		t.Fatalf("err %v", err)
	}
}

// Why: after a server restart a job resumed in verify has no note of the send, and a device whose layout
// already matched needed no send: there the layout alone decides, or those jobs could never finish.
func TestVerifyWithoutASendOfThisProcessChecksTheLayoutOnly(t *testing.T) {
	f := newFleet()
	id := f.add("a", "NOUS A1T", 52)
	d, _ := f.GetDevice(context.Background(), id)
	f.uptime = func(model.DeviceID) time.Duration { return time.Hour }
	a := &Applier{Client: f, Wait: func(context.Context, time.Duration) error { return nil }}
	if _, err := a.Verify(context.Background(), d, wantLayout(t, "NOUS A1T", 52)); err != nil {
		t.Fatal(err)
	}
}
