package ota

import (
	"context"
	"errors"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/netaddr"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// UpLayout reads the upload layout from GET /up (read-only). The device client must be the
// production tasmota client.
func (a *deviceAPI) UpLayout(ctx context.Context, d model.Device) (tasmota.UpLayout, error) {
	s, ok := a.c.(tasmota.BootSwitcher)
	if !ok {
		return "", errors.New("upload layout: this device client cannot read it")
	}
	l, err := s.ReadUpLayout(ctx, tasmota.TargetFor(d))
	if err != nil {
		return "", err
	}
	return l, nil
}

// UpPage reads GET /up with its footer and banner (see tasmota.UpPage).
func (a *deviceAPI) UpPage(ctx context.Context, d model.Device) (tasmota.UpPage, error) {
	s, ok := a.c.(tasmota.UpPageReader)
	if !ok {
		return tasmota.UpPage{}, errors.New("upload page: this device client cannot read it")
	}
	return s.ReadUpPage(ctx, tasmota.TargetFor(d))
}

// SwitchBoot sends the one GET /u4 (see tasmota.BootSwitcher).
func (a *deviceAPI) SwitchBoot(ctx context.Context, d model.Device, to tasmota.BootTarget) (tasmota.BootReply, error) {
	s, ok := a.c.(tasmota.BootSwitcher)
	if !ok {
		return "", errors.New("boot switch: this device client cannot switch")
	}
	return s.SwitchBoot(ctx, tasmota.TargetFor(d), to)
}

var _ SafebootAPI = (*deviceAPI)(nil)

// switchBoot sends the single restart command of a push into safeboot (to safeboot) or of its
// recovery (back to the app) and turns the answer into an event. It never repeats the command:
// only an answer that proves the device did not act (an error status, a firmware without /u4,
// a wrong password, no factory partition) is reported as certain (NotFlashed); a lost reply or
// timeout is not, and the machine probes.
func (e *Engine) switchBoot(ctx context.Context, d model.Device, to tasmota.BootTarget) Event {
	sb, ok := e.api.(SafebootAPI)
	if !ok {
		return Event{Kind: EvCommandFailed, NotFlashed: true, Err: "the device API cannot switch the boot partition"}
	}
	reply, err := sb.SwitchBoot(ctx, d, to)
	switch {
	case err == nil && reply == tasmota.BootNone:
		return Event{Kind: EvCommandFailed, NotFlashed: true, Err: "the device has no safeboot partition"}
	case err == nil:
		// "false": restarting; "true": nothing to switch, the device is on the target already.
		return Event{Kind: EvCommandOK}
	case errors.Is(err, tasmota.ErrSwitchNotDone), errors.Is(err, tasmota.ErrAuth), errors.Is(err, tasmota.ErrForbiddenAddr):
		return Event{Kind: EvCommandFailed, NotFlashed: true, Err: err.Error()}
	}
	return Event{Kind: EvCommandFailed, Err: err.Error()}
}

// safebootUploadReady is the check right before the upload into safeboot: /up must serve the
// plain upload form. It returns the event that stops the upload when not. Nothing was sent, so the
// failure is certain (NotFlashed) and the machine recovers the device.
func (e *Engine) safebootUploadReady(ctx context.Context, d model.Device) (Event, bool) {
	sb, ok := e.api.(SafebootAPI)
	if !ok {
		return Event{Kind: EvCommandFailed, NotFlashed: true, Err: "the device API cannot read the upload layout"}, false
	}
	layout, err := sb.UpLayout(ctx, d)
	switch {
	case err != nil:
		return Event{Kind: EvCommandFailed, NotFlashed: true, Err: "upload page not readable: " + err.Error()}, false
	case layout != tasmota.UpLayoutDirect:
		return Event{Kind: EvCommandFailed, NotFlashed: true, Err: "the device does not offer the safeboot upload page"}, false
	}
	return Event{}, true
}

// safebootAddr is how to reach the device, for the failure text of a push into safeboot; "" for
// every other job.
func (e *Engine) safebootAddr(ctx context.Context, j model.Job) string {
	if !j.ViaSafeboot {
		return ""
	}
	d, err := e.devices.GetDevice(ctx, j.DeviceID)
	if err != nil {
		return ""
	}
	return netaddr.Display(d.Address, d.Port, "http")
}
