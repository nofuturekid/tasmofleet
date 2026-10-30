package ota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// commandGap is the pause between two commands to the same device. ESP8266
// web servers with little heap corrupt replies under bursts (the reason
// Status 0 is never sent).
const commandGap = 200 * time.Millisecond

// NewDeviceAPI returns the production DeviceAPI over a tasmota.Client. It
// sends individual Status N queries one at a time, never Status 0.
func NewDeviceAPI(c tasmota.Client) DeviceAPI {
	return &deviceAPI{c: c, gap: commandGap, detect: fwselect.Detect}
}

type deviceAPI struct {
	c      tasmota.Client
	gap    time.Duration
	detect func(fwselect.Inputs) (model.Identity, error)
}

// cmd sends one command after the inter-command gap.
func (a *deviceAPI) cmd(ctx context.Context, t tasmota.Target, c string) (json.RawMessage, error) {
	if a.gap > 0 {
		timer := time.NewTimer(a.gap)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return a.c.Command(ctx, t, c)
}

var unknownStatus2 = json.RawMessage(`{"Command":"Unknown","Input":"STATUS 2"}`)

// firmwareInputs collects the answers Detect needs. Minimal firmware
// (ESP8266) and safeboot (ESP32) answer Status 2 with Unknown; then the bare
// Upgrade probe gives the version and the root page the image and
// language. withRoot fetches the root page for normal firmware too (the
// language build is only visible there).
func (a *deviceAPI) firmwareInputs(ctx context.Context, t tasmota.Target, withRoot bool) (fwselect.Inputs, bool, error) {
	var in fwselect.Inputs
	raw, err := a.cmd(ctx, t, "Status 2")
	unknown := errors.Is(err, tasmota.ErrUnknownCommand)
	if err != nil && !unknown {
		return in, false, fmt.Errorf("status 2: %w", err)
	}
	if err == nil {
		if _, perr := tasmota.ParseStatus2(raw); errors.Is(perr, tasmota.ErrUnknownCommand) {
			unknown = true
		} else if perr != nil {
			return in, false, fmt.Errorf("status 2: %w", perr)
		}
	}
	if unknown {
		if len(raw) == 0 {
			raw = unknownStatus2
		}
		up, err := a.cmd(ctx, t, "Upgrade")
		if err != nil {
			return in, true, fmt.Errorf("upgrade probe: %w", err)
		}
		p, err := tasmota.ParseUpgradeProbe(up)
		if err != nil {
			return in, true, fmt.Errorf("upgrade probe: %w", err)
		}
		in.Upgrade = &fwselect.UpgradeProbe{Version: p.Version}
	}
	in.Status2 = raw
	if unknown || withRoot {
		ri, err := a.c.RootPage(ctx, t)
		if err != nil && !unknown && errors.Is(err, transport.ErrNoInfo) {
			// Over MQTT the page is built from INFO1, which a device sends once per boot. Firmware that
			// answered Status 2 needs the page only for its language, which stays unknown (D10); minimal
			// firmware needs it for its image, so there the missing INFO1 stops the probe.
			return in, unknown, nil
		}
		if err != nil {
			return in, unknown, fmt.Errorf("root page: %w", err)
		}
		in.Root = &fwselect.RootInfo{Version: ri.Version, Image: ri.Image, Lang: ri.Lang, LangKnown: ri.LangKnown, Minimal: ri.Minimal}
	}
	return in, unknown, nil
}

func (a *deviceAPI) uptime(ctx context.Context, t tasmota.Target) (int64, error) {
	raw, err := a.cmd(ctx, t, "Status 11")
	if err != nil {
		return 0, fmt.Errorf("status 11: %w", err)
	}
	sts, err := tasmota.ParseStatus11(raw)
	if err != nil {
		return 0, fmt.Errorf("status 11: %w", err)
	}
	return sts.UptimeSec, nil
}

// ProbeIdentity runs Status 2 (Upgrade probe + root page on minimal),
// Status 4 and Status 11 (normal firmware only), the root page and OtaUrl.
func (a *deviceAPI) ProbeIdentity(ctx context.Context, d model.Device) (model.Identity, string, int64, error) {
	t := tasmota.TargetFor(d)
	in, minimal, err := a.firmwareInputs(ctx, t, true)
	if err != nil {
		return model.Identity{}, "", 0, err
	}
	var up int64
	if !minimal {
		raw, err := a.cmd(ctx, t, "Status 4")
		if err != nil {
			return model.Identity{}, "", 0, fmt.Errorf("status 4: %w", err)
		}
		in.Status4 = raw
		if up, err = a.uptime(ctx, t); err != nil {
			return model.Identity{}, "", 0, err
		}
	}
	raw, err := a.cmd(ctx, t, "OtaUrl")
	if err != nil {
		return model.Identity{}, "", 0, fmt.Errorf("otaurl: %w", err)
	}
	otaURL, err := tasmota.ParseOtaURL(raw)
	if err != nil {
		return model.Identity{}, "", 0, fmt.Errorf("otaurl: %w", err)
	}
	id, err := a.detect(in)
	if err != nil {
		return model.Identity{}, "", 0, fmt.Errorf("detect: %w", err)
	}
	if minimal {
		// Status 4 does not exist on minimal: keep the last known flash data.
		id.FlashSizeKB, id.ProgramFlashSizeKB, id.FreeKB = d.Identity.FlashSizeKB, d.Identity.ProgramFlashSizeKB, d.Identity.FreeKB
	}
	return id, otaURL, up, nil
}

// Probe is the liveness check while waiting: Status 2 (with the minimal
// fallback) and Status 11. Any answer means online; an identity Detect
// cannot build is returned empty, which the machine never reads as change.
func (a *deviceAPI) Probe(ctx context.Context, d model.Device) (model.Identity, int64, error) {
	t := tasmota.TargetFor(d)
	in, minimal, err := a.firmwareInputs(ctx, t, false)
	if err != nil {
		return model.Identity{}, 0, err
	}
	var up int64
	if !minimal {
		if up, err = a.uptime(ctx, t); err != nil {
			return model.Identity{}, 0, err
		}
	}
	id, err := a.detect(in)
	if err != nil {
		return model.Identity{}, up, nil //nolint:nilerr // online is what counts; identity unknown
	}
	return id, up, nil
}

// SetOtaURL sets OtaUrl and checks that the device stored exactly that URL.
func (a *deviceAPI) SetOtaURL(ctx context.Context, d model.Device, u string) error {
	raw, err := a.cmd(ctx, tasmota.TargetFor(d), "OtaUrl "+u)
	if err != nil {
		return fmt.Errorf("otaurl: %w", err)
	}
	got, err := tasmota.ParseOtaURL(raw)
	if err != nil {
		return fmt.Errorf("otaurl: %w", err)
	}
	if got != u {
		return fmt.Errorf("otaurl: device stored %q, sent %q", got, u)
	}
	return nil
}

var _ FirmwareUploader = (*deviceAPI)(nil)

// UploadFirmware pushes an image to the device's /u2 (see push.go). The
// device client must be the production tasmota client.
func (a *deviceAPI) UploadFirmware(ctx context.Context, d model.Device, filename string, size int64, body io.Reader) error {
	u, ok := a.c.(tasmota.Uploader)
	if !ok {
		return errors.New("upload: this device client cannot upload")
	}
	if err := u.UploadFirmware(ctx, tasmota.TargetFor(d), filename, size, body); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	return nil
}

var upgradeEcho = regexp.MustCompile(`^Version \S+ from (\S+)$`)

// Upgrade sends Upgrade 1 and returns the URL the device says it will
// download. A reply without "Version X from <url>" means it did not start.
func (a *deviceAPI) Upgrade(ctx context.Context, d model.Device) (string, error) {
	raw, err := a.cmd(ctx, tasmota.TargetFor(d), "Upgrade 1")
	if err != nil {
		return "", fmt.Errorf("upgrade: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", fmt.Errorf("upgrade: %w", tasmota.ErrBadResponse)
	}
	s, _ := m["Upgrade"].(string)
	g := upgradeEcho.FindStringSubmatch(s)
	if g == nil {
		if len(s) > 80 {
			s = s[:80]
		}
		return "", fmt.Errorf("upgrade not started, device replied %q", s)
	}
	return g[1], nil
}

// ReadPower reads the relays from Status 11 and fails closed: a relay whose
// value is not exactly ON or OFF (a custom StateText, garbage) is reported as
// Unknown, never guessed; a reading with a key that cannot be attributed to a
// relay (POWER0, an index beyond the supported range) or with two keys for
// the same relay is an error. POWER is relay 1, POWERn is relay n.
func (a *deviceAPI) ReadPower(ctx context.Context, d model.Device) (PowerReading, error) {
	raw, err := a.cmd(ctx, tasmota.TargetFor(d), "Status 11")
	if err != nil {
		return PowerReading{}, fmt.Errorf("status 11: %w", err)
	}
	sts, err := tasmota.ParseStatus11(raw)
	if err != nil {
		return PowerReading{}, fmt.Errorf("status 11: %w", err)
	}
	var r PowerReading
	for k, v := range sts.Power {
		n := 1 // plain POWER
		if num := strings.TrimPrefix(k, "POWER"); num != "" {
			if n, err = strconv.Atoi(num); err != nil {
				return PowerReading{}, fmt.Errorf("status 11: relay key %q: %w", k, tasmota.ErrBadResponse)
			}
		}
		if n < 1 || n > model.MaxRelays || r.All.Has(n) {
			return PowerReading{}, fmt.Errorf("status 11: relay key %q is ambiguous: %w", k, tasmota.ErrBadResponse)
		}
		r.All = r.All.Add(n)
		switch {
		case strings.EqualFold(v, "ON"):
			r.On = r.On.Add(n)
		case strings.EqualFold(v, "OFF"):
		default:
			r.Unknown = r.Unknown.Add(n)
		}
	}
	if hasLightKeys(raw) && r.All != 0 {
		// A light is the last device Tasmota registers (Interlock code takes
		// it off the relay count the same way).
		l := r.All.List()
		r.Light = r.Light.Add(l[len(l)-1])
	}
	return r, nil
}

// hasLightKeys reports whether StatusSTS carries light state (Dimmer, Color,
// CT, HSBColor, White, Channel), which only a device with a light driver has.
func hasLightKeys(raw json.RawMessage) bool {
	var w struct{ StatusSTS map[string]json.RawMessage }
	if json.Unmarshal(raw, &w) != nil {
		return false
	}
	for _, k := range []string{"Dimmer", "Color", "CT", "HSBColor", "White", "Channel"} {
		if _, ok := w.StatusSTS[k]; ok {
			return true
		}
	}
	return false
}

// ReadMAC returns the device's MAC address (Status 5), lower case.
func (a *deviceAPI) ReadMAC(ctx context.Context, d model.Device) (string, error) {
	raw, err := a.cmd(ctx, tasmota.TargetFor(d), "Status 5")
	if err != nil {
		return "", fmt.Errorf("status 5: %w", err)
	}
	var s struct{ StatusNET struct{ Mac string } }
	if err := json.Unmarshal(raw, &s); err != nil || s.StatusNET.Mac == "" {
		return "", fmt.Errorf("status 5: %w", tasmota.ErrBadResponse)
	}
	return strings.ToLower(s.StatusNET.Mac), nil
}

// RestrictedRelays returns the relays whose "ON" is an action rather than a
// state: relays with a PulseTime (momentary: ON starts a pulse), relays in
// an Interlock group (ON switches others OFF) and shutter relays (ON runs a
// motor). Restoring them could do harm, so they are never restored. All
// three are bare reads in Tasmota (a command without payload only reports).
// It fails closed: a reply that does not have the expected shape (an older
// firmware format, a missing key) is an error, never "no restriction".
// Firmware without the shutter driver answers ShutterRelay with Unknown,
// which means "no shutters"; a device with at most one relay cannot have an
// interlock, so an unusable Interlock reply does not matter there.
func (a *deviceAPI) RestrictedRelays(ctx context.Context, d model.Device, all model.RelaySet) (model.RelaySet, error) {
	t := tasmota.TargetFor(d)
	var out model.RelaySet
	for _, q := range []struct {
		cmd       string
		parse     func(map[string]json.RawMessage) (model.RelaySet, bool)
		unknownOK bool
		optional  bool
	}{
		{"PulseTime", parsePulseTime, false, false},
		{"Interlock", parseInterlock, false, len(all.List()) <= 1},
		{"ShutterRelay", parseShutterRelay, true, false},
	} {
		raw, err := a.cmd(ctx, t, q.cmd)
		if errors.Is(err, tasmota.ErrUnknownCommand) && q.unknownOK {
			continue
		}
		var m map[string]json.RawMessage
		if err == nil {
			if json.Unmarshal(raw, &m) != nil {
				err = tasmota.ErrBadResponse
			}
		}
		if err != nil {
			if q.optional {
				continue
			}
			return 0, fmt.Errorf("%s: %w", strings.ToLower(q.cmd), err)
		}
		set, ok := q.parse(m)
		if !ok && !q.optional {
			return 0, fmt.Errorf("%s: unexpected reply: %w", strings.ToLower(q.cmd), tasmota.ErrBadResponse)
		}
		out |= set
	}
	return out, nil
}

// {"PulseTime":{"Set":[0,0,100,0,...],"Remaining":[...]}}, or the older
// {"PulseTime1":{"Set":0,...},"PulseTime2":...}. Anything else is not understood.
func parsePulseTime(m map[string]json.RawMessage) (model.RelaySet, bool) {
	var out model.RelaySet
	if raw, ok := m["PulseTime"]; ok {
		var v struct{ Set []int }
		if json.Unmarshal(raw, &v) != nil || v.Set == nil {
			return 0, false
		}
		for i, n := range v.Set {
			if n > 0 {
				out = out.Add(i + 1)
			}
		}
		return out, true
	}
	found := false
	for k, raw := range m {
		num, ok := strings.CutPrefix(k, "PulseTime")
		idx, err := strconv.Atoi(num)
		if !ok || err != nil {
			continue
		}
		var v struct{ Set *int }
		if json.Unmarshal(raw, &v) != nil || v.Set == nil {
			return 0, false
		}
		found = true
		if *v.Set > 0 {
			out = out.Add(idx)
		}
	}
	return out, found
}

// {"Interlock":"ON","Groups":"1,2 3,4"}; ON without groups means all relays.
func parseInterlock(m map[string]json.RawMessage) (model.RelaySet, bool) {
	var on, groups string
	var out model.RelaySet
	if json.Unmarshal(m["Interlock"], &on) != nil || on == "" {
		return 0, false
	}
	if !strings.EqualFold(on, "ON") {
		return 0, true
	}
	_ = json.Unmarshal(m["Groups"], &groups) // absent = no groups listed
	for _, g := range strings.Fields(groups) {
		for _, f := range strings.Split(g, ",") {
			if n, err := strconv.Atoi(f); err == nil {
				out = out.Add(n)
			}
		}
	}
	if out == 0 {
		for n := 1; n <= maxRestoreRelay; n++ {
			out = out.Add(n)
		}
	}
	return out, true
}

// {"ShutterRelay1":1,"ShutterRelay2":3}: a shutter uses relay n and n+1.
func parseShutterRelay(m map[string]json.RawMessage) (model.RelaySet, bool) {
	var out model.RelaySet
	for k, v := range m {
		if !strings.HasPrefix(k, "ShutterRelay") {
			continue
		}
		var n int
		if json.Unmarshal(v, &n) != nil {
			return 0, false
		}
		if n > 0 {
			out = out.Add(n).Add(n + 1)
		}
	}
	return out, true
}
