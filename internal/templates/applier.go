package templates

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// Applier runs the device side of a template job (ota.TemplateApplier) over the routing client.
type Applier struct {
	Client Commander
	// Wait pauses between attempts; nil waits for real.
	Wait func(ctx context.Context, d time.Duration) error
	// Settle is the pause before the first read-back: the device restarts about two seconds after the
	// command (restart_flag, T5) and a read before that shows the stored template, not the running one.
	Settle time.Duration
	// Attempts is how often the read-back is tried while the device does not answer or does not yet
	// show the template (default 8).
	Attempts int
	// Pause is the time between attempts (default 5 s).
	Pause time.Duration
	// OnRead is told about every successful reading (the Manager's cache); optional.
	OnRead func(id model.DeviceID, r reading)
	Now    func() time.Time
}

var _ ota.TemplateApplier = (*Applier)(nil)

func (a *Applier) wait(ctx context.Context, d time.Duration) error {
	if a.Wait != nil {
		return a.Wait(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (a *Applier) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Applier) read(ctx context.Context, d model.Device) (ota.TemplateReading, error) {
	r, err := readLive(ctx, a.Client, d, a.now())
	if err != nil {
		return ota.TemplateReading{}, err
	}
	if a.OnRead != nil {
		a.OnRead(d.ID, r)
	}
	out := ota.TemplateReading{Module: r.Module.Number}
	if r.Template != nil {
		out.Layout = r.Template.Fingerprint()
		out.JSON = compactTemplate(*r.Template)
	}
	return out, nil
}

// compactTemplate is the audit form of a template: the fields TasmoFleet reads, as JSON.
func compactTemplate(t policy.GPIOTemplate) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]any{"NAME": t.Name, "ARCH": t.Arch, "GPIO": t.GPIO, "FLAG": t.Flag, "BASE": t.Base}) // plain values always encode
	return string(bytes.TrimSpace(b.Bytes()))
}

// Read implements ota.TemplateApplier.
func (a *Applier) Read(ctx context.Context, d model.Device) (ota.TemplateReading, error) {
	return a.read(ctx, d)
}

// Send implements ota.TemplateApplier.
func (a *Applier) Send(ctx context.Context, d model.Device, command string) error {
	_, err := a.Client.Command(ctx, tasmota.TargetFor(d), command)
	return err
}

// Verify implements ota.TemplateApplier: wait for the restart, then read until the device shows the
// planned layout. It returns the last reading it got, with an error when the device never showed it.
func (a *Applier) Verify(ctx context.Context, d model.Device, wantLayout string) (ota.TemplateReading, error) {
	attempts, pause := a.Attempts, a.Pause
	if attempts <= 0 {
		attempts = 8
	}
	if pause <= 0 {
		pause = 5 * time.Second
	}
	if err := a.wait(ctx, a.Settle); err != nil {
		return ota.TemplateReading{}, err
	}
	var last ota.TemplateReading
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			if err := a.wait(ctx, pause); err != nil {
				return last, err
			}
		}
		rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		r, err := a.read(rctx, d)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		last, lastErr = r, nil
		if r.Module == 0 && r.Layout == wantLayout {
			return r, nil
		}
	}
	if lastErr != nil {
		return last, fmt.Errorf("the device did not answer after the restart: %w", lastErr)
	}
	return last, fmt.Errorf("after the restart the device still reports module %d and a template that is not the planned one", last.Module)
}
