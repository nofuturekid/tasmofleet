package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttcmd"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// ErrNoInfo is the RootPage error over MQTT when no INFO1 was seen since the device's last boot: a
// device only announces itself once per boot, and an INFO1 from before a restart describes the old
// firmware. It is an ErrBadResponse, so a probe that needs the page fails and nothing is decided on a
// guess.
var ErrNoInfo = fmt.Errorf("%w: no INFO1 seen since the device's last boot, identity unknown", tasmota.ErrBadResponse)

// infoSource is the part of the command channel that keeps the INFO1 cache (mqttcmd.Channel.Info).
type infoSource interface {
	Info(topic string) (mqttcmd.Info, bool)
}

// mqttRootPage builds the RootInfo of a device that only MQTT reaches from its INFO1 (D9): version
// and image tag ("16.0.0(release-minimal)"). Nothing else is known without the web page: LangKnown is
// false (the language build is shown only there), and the title is not part of INFO1.
func (r *Router) mqttRootPage(d model.Device) (tasmota.RootInfo, error) {
	src, ok := r.mqttDoer().(infoSource)
	if !ok {
		return tasmota.RootInfo{}, fmt.Errorf("%w: the MQTT channel keeps no INFO1", tasmota.ErrNotImplemented)
	}
	info, ok := src.Info(d.Topic)
	if !ok || len(info.Info1) == 0 {
		return tasmota.RootInfo{}, ErrNoInfo
	}
	return rootFromInfo1(info.Info1)
}

func rootFromInfo1(payload []byte) (tasmota.RootInfo, error) {
	var m struct {
		Info1 *struct{ Version string }
	}
	if err := json.Unmarshal(payload, &m); err != nil || m.Info1 == nil {
		return tasmota.RootInfo{}, fmt.Errorf("%w: INFO1 is not readable", tasmota.ErrBadResponse)
	}
	semver, rest, ok := strings.Cut(m.Info1.Version, "(")
	tag, _, ok2 := strings.Cut(rest, ")")
	if !ok || !ok2 || semver == "" || tag == "" {
		return tasmota.RootInfo{}, fmt.Errorf("%w: INFO1 version %q is not recognised", tasmota.ErrBadResponse, m.Info1.Version)
	}
	return tasmota.RootInfo{
		Version: semver, Image: tag, LangKnown: false,
		Minimal: strings.HasSuffix(tag, "-minimal") || strings.HasSuffix(tag, "-safeboot"),
	}, nil
}

// RootPage reads the main page over HTTP; for a job that pinned MQTT, or a device whose HTTP is known
// to fail, it is built from INFO1 instead (mqttRootPage).
func (r *Router) RootPage(ctx context.Context, t tasmota.Target) (tasmota.RootInfo, error) {
	id := t.DeviceID()
	if id == 0 {
		return r.http.RootPage(ctx, t)
	}
	if pin, ok := PathFrom(ctx); ok {
		if pin == HTTP {
			return r.http.RootPage(ctx, t)
		}
		return r.pinnedRootPage(ctx, id)
	}
	if r.mqttDoer() != nil && r.o.Facts.HTTPState(id).State == HTTPFailing {
		d, _, p, err := r.decide(ctx, id)
		if err == nil && p.Commands == MQTT {
			return r.mqttRootPage(d)
		}
	}
	return r.http.RootPage(ctx, t)
}

func (r *Router) pinnedRootPage(ctx context.Context, id model.DeviceID) (tasmota.RootInfo, error) {
	if r.mqttDoer() == nil {
		return tasmota.RootInfo{}, fmt.Errorf("%w: MQTT is not connected", tasmota.ErrUnreachable)
	}
	d, err := r.o.Devices.GetDevice(ctx, id)
	if err != nil {
		return tasmota.RootInfo{}, err
	}
	f, err := r.o.Facts.FactsFor(ctx, d)
	if err != nil {
		return tasmota.RootInfo{}, err
	}
	if why := MQTTBlocker(f, false); why != "" {
		return tasmota.RootInfo{}, fmt.Errorf("%w: not usable over MQTT: %s", tasmota.ErrUnreachable, why)
	}
	return r.mqttRootPage(d)
}
