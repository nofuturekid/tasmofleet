package templates

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	// Attempts is how often the read-back is tried while the device does not answer, does not yet show
	// the template or has not been seen restarting (default 12; the boot count rises at 10 s of uptime).
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
		out.Name = r.Template.Name
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

// bootCount reads Status 1 (never Status 0): a fresh answer from the device.
func (a *Applier) bootCount(ctx context.Context, d model.Device) (int, error) {
	raw, err := a.Client.Command(ctx, tasmota.TargetFor(d), "Status 1")
	if err != nil {
		return 0, err
	}
	var s struct {
		StatusPRM struct{ BootCount *int }
	}
	if err := json.Unmarshal(raw, &s); err != nil || s.StatusPRM.BootCount == nil {
		return 0, tasmota.ErrBadResponse
	}
	return *s.StatusPRM.BootCount, nil
}

// uptimeSec reads Status 11.
func (a *Applier) uptimeSec(ctx context.Context, d model.Device) (int64, error) {
	raw, err := a.Client.Command(ctx, tasmota.TargetFor(d), "Status 11")
	if err != nil {
		return 0, err
	}
	st, err := tasmota.ParseStatus11(raw)
	if err != nil {
		return 0, err
	}
	return st.UptimeSec, nil
}

// Baseline implements ota.TemplateApplier: the boot count now and the time, as the token "boot=<n> at=<unix
// nanoseconds>". The engine stores it with the job before the send, so a restart of this server cannot lose
// it. Reading it is part of the rule of restore (R26, docs/security/restore.md).
func (a *Applier) Baseline(ctx context.Context, d model.Device) (string, error) {
	boot, err := a.bootCount(ctx, d)
	if err != nil {
		return "", fmt.Errorf("could not read the boot count: %w", err)
	}
	return fmt.Sprintf("boot=%d at=%d", boot, a.now().UnixNano()), nil
}

// parseBaseline reads a Baseline token; ok is false for an empty or unusable one.
func parseBaseline(tok string) (boot int, at time.Time, ok bool) {
	var ns int64
	if n, err := fmt.Sscanf(tok, "boot=%d at=%d", &boot, &ns); err != nil || n != 2 || boot < 0 {
		return 0, time.Time{}, false
	}
	return boot, time.Unix(0, ns), true
}

// Send implements ota.TemplateApplier. A device answers a Template it takes with the stored template and
// one it refuses with {"Template":"Invalid JSON or Bad Chip Type"} (CmndTemplate): that reply, a string
// under "Template", means nothing was applied and no restart follows.
func (a *Applier) Send(ctx context.Context, d model.Device, command string) error {
	raw, err := a.Client.Command(ctx, tasmota.TargetFor(d), command)
	if err != nil {
		return err
	}
	var v struct {
		Template *string `json:"Template"`
	}
	if json.Unmarshal(raw, &v) == nil && v.Template != nil {
		return fmt.Errorf("%w: %s", ota.ErrTemplateRefused, strings.TrimSpace(*v.Template))
	}
	return nil
}

// restarted proves that the device restarted since the baseline, the way restore does (R26): a larger
// BootCount than before and an uptime not longer than the time since the baseline (plus two seconds of
// slack), so neither a count that rose for another reason nor a reboot that happened before the command
// passes. The firmware counts a boot at 10 s of uptime, so the count rises a few polls after the device
// answers again.
func (a *Applier) restarted(ctx context.Context, d model.Device, boot int, at time.Time) bool {
	n, err := a.bootCount(ctx, d)
	if err != nil || n <= boot {
		return false
	}
	up, err := a.uptimeSec(ctx, d)
	return err == nil && time.Duration(up)*time.Second <= a.now().Sub(at)+2*time.Second
}

// Verify implements ota.TemplateApplier: wait for the restart, then read until the device shows the
// planned layout and, given a usable baseline, until the restart is proven. It returns the last reading it got, with an error when the device never showed it.
func (a *Applier) Verify(ctx context.Context, d model.Device, wantLayout, baseline string) (ota.TemplateReading, error) {
	boot, at, proof := parseBaseline(baseline)
	attempts, pause := a.Attempts, a.Pause
	if attempts <= 0 {
		attempts = 12
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
		if err == nil && r.Module == 0 && r.Layout == wantLayout && proof && !a.restarted(rctx, d, boot, at) {
			err = errNoRestart
		}
		cancel()
		if err != nil {
			lastErr = err
			if !errors.Is(err, errNoRestart) {
				continue
			}
		} else {
			lastErr = nil
		}
		last = r
		if err == nil && r.Module == 0 && r.Layout == wantLayout {
			r.RestartProven = proof
			return r, nil
		}
	}
	switch {
	case errors.Is(lastErr, errNoRestart):
		return last, errors.New("the device shows the new template but its boot count did not rise as after a restart, so the pins may not be in use yet")
	case lastErr != nil:
		return last, fmt.Errorf("the device did not answer after the restart: %w", lastErr)
	}
	return last, fmt.Errorf("after the restart the device still reports module %d and a template that is not the planned one", last.Module)
}

var errNoRestart = errors.New("not restarted")
