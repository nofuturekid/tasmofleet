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
