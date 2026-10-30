package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/transport"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The header every device page shares (design v2, WP P3; prototype screen "device"): name and
// state, the address / chip / firmware line with "Update to x.y.z", the buttons Power, Restart,
// Back up, Edit and Delete, the offline banner and the device tabs. Its data is deviceTabs,
// built by headOf for the overview, the settings, the console, the drift and the backups tab.

// uiTone maps the badge tone of a value (labels.go) to the tone of the design system.
func uiTone(v any) ui.Tone {
	switch tone(v) {
	case "ok":
		return ui.ToneOK
	case "warn":
		return ui.ToneWarn
	case "bad":
		return ui.ToneDanger
	case "info":
		return ui.ToneAccent
	}
	return ui.ToneNeutral
}

// devPath is the path of a device page below the base: "/devices/7" plus the rest.
func devPath(id model.DeviceID, rest string) string {
	return "/devices/" + strconv.FormatInt(int64(id), 10) + rest
}

// deviceCrumbs is the trail of a device tab: Devices / the device (every tab, as the prototype).
func deviceCrumbs(d model.Device) []ui.Crumb {
	return []ui.Crumb{{Label: "Devices", Path: "/devices", KeepQuery: true}, {Label: d.Name}}
}

// headOf builds the device header and tabs for the viewer: tabsFor plus what needs the
// handler (the cached release feed for "Update to", the drift and replace capabilities).
func (h *handler) headOf(ctx context.Context, v view, d model.Device, hl model.Health, running *model.Job, active string) deviceTabs {
	_, p, ok := h.transportOf(ctx, d.ID)
	return h.headWith(v, d, hl, running, active, p, ok)
}

// headWith is headOf for a caller that has the paths already.
func (h *handler) headWith(v view, d model.Device, hl model.Health, running *model.Job, active string, p transport.Paths, pathsOK bool) deviceTabs {
	t := tabsFor(v, d, hl, running, active)
	t.applyPaths(p, pathsOK)
	t.CanReplace = h.canReplace(v)
	_, t.Drift = h.d.Backups.(LiveSnapshotter)
	if cl, ok := h.d.Firmware.(firmware.CachedLister); ok && v.Features.Updates {
		if latest := firmware.Latest(cl.CachedReleases()); firmware.UpdateAvailable(latest, d.Identity) {
			t.UpdateTo = latest
		}
	}
	if t.offline() {
		t.OfflineSince = offlineSince(d, hl)
		t.OfflineSinceText = ui.StampDayText(h.d.Clock.Now(), t.OfflineSince)
	}
	return t
}

// headTabs are the device tabs of the header. A tab the device cannot use right now says why.
func (t deviceTabs) headTabs() []ui.Tab {
	p := func(rest string) string { return t.Base + devPath(t.ID, rest) }
	tabs := []ui.Tab{{Label: "Overview", Href: p(""), Current: t.Active == "overview"}}
	if t.Settings {
		tabs = append(tabs, ui.Tab{Label: "Settings", Href: p("/settings"), Current: t.Active == "settings", Reason: t.SettingsWhy})
	}
	if t.Console {
		tabs = append(tabs, ui.Tab{Label: "Console", Href: p("/console"), Current: t.Active == "console", Reason: t.ConsoleWhy})
	}
	tabs = append(tabs, ui.Tab{Label: "Backups", Href: p("/backups"), Current: t.Active == "backups"})
	if t.Drift {
		tabs = append(tabs, ui.Tab{Label: "Drift", Href: p("/drift"), Current: t.Active == "drift"})
	}
	if t.CanReplace {
		tabs = append(tabs, ui.Tab{Label: "Replace device", Href: p("/replace"), Current: t.Active == "replace"})
	}
	return tabs
}

// stateLabel and stateTone are the header's status pill: "Updating" while an operation holds
// the device (it is silent on purpose), else the health state.
func (t deviceTabs) stateLabel() string {
	if t.Updating {
		return "Updating"
	}
	return label(t.Health)
}

func (t deviceTabs) stateTone() ui.Tone {
	if t.Updating {
		return ui.ToneAccent
	}
	return uiTone(t.Health)
}

// offline says whether the header shows the offline banner.
func (t deviceTabs) offline() bool { return !t.Updating && t.Health == model.HealthOffline }

// headPage renders a device tab: the shell with the device trail around content.
func (h *handler) headPage(w http.ResponseWriter, r *http.Request, status int, page string, v view, d model.Device, content templ.Component) {
	v.Crumbs = deviceCrumbs(d)
	h.show(w, r, status, page, v, content)
}

// chipText is a chip family as the prototype writes it: "ESP8266", "ESP32", "ESP32-C3".
func chipText(f model.ChipFamily) string {
	s := strings.ToUpper(string(f))
	if strings.HasPrefix(s, "ESP32") && len(s) > len("ESP32") {
		s = "ESP32-" + s[len("ESP32"):]
	}
	return s
}

// sinceText is a timestamp for running text, "22 Oct 2026, 09:41" (the prototype's format).
func sinceText(t time.Time) string { return t.Local().Format("2 Jan 2006, 15:04") }

// ariaBool is "true" or "false" for aria-pressed and aria-checked.
func ariaBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// backPath is the tab the header's Back up returns to (headBack accepts it).
func (t deviceTabs) backPath() string {
	if t.Active == "overview" || t.Active == "" {
		return ""
	}
	return "/" + t.Active
}

// updatingTitle and updatingText are the overview's banner while an operation holds the device:
// it is silent on purpose, which is not a failure.
func updatingTitle(j model.Job) string {
	switch {
	case j.IsCommand():
		return "A command is being sent to this device:"
	case j.IsBackup():
		return "A backup is running:"
	case j.IsPolicy():
		return "This device is being made compliant with the policy:"
	}
	return "This device is being updated:"
}

func updatingText(j model.Job) string {
	switch {
	case j.IsCommand(), j.IsBackup():
		return "Other commands are paused until it has finished."
	case j.IsPolicy():
		return "Commands are paused until it has finished."
	}
	return "It does not answer while it downloads and reboots, so this is not a failure. Commands are paused until the update finishes."
}

// nameHint warns that Tasmota keeps only 32 characters of a FriendlyName (the edit form).
func nameHint(name string) string {
	if len([]rune(name)) > 32 {
		return "Tasmota stores only the first 32 characters as FriendlyName. TasmoFleet keeps the full name."
	}
	return ""
}

// addressHelp is the line under the address of the edit form.
func addressHelp(hasPassword bool) string {
	s := "IP address or hostname. Doesn’t change the device’s own network settings."
	if hasPassword {
		s += " If you change it, the stored web password will be sent to the new address."
	}
	return s
}

func portValue(p int) string {
	if p == 0 {
		return ""
	}
	return strconv.Itoa(p)
}

func passwordPlaceholder(has bool) string {
	if has {
		return "Unchanged"
	}
	return ""
}

func mqttPasswordPlaceholder(has bool) string {
	if has {
		return "Unchanged"
	}
	return ""
}

func mqttPasswordHelp(has bool) string {
	if has {
		return "Used with \"Own password\". Leave empty to keep the stored password; stored encrypted."
	}
	return "Used with \"Own password\". Stored encrypted, never shown again."
}

func passwordHelp(has bool) string {
	if has {
		return "Leave empty to keep the stored password."
	}
	return "Stored encrypted."
}

// offlineSince is when the device went offline, as far as TasmoFleet knows: the state change, or
// the last time it answered when that is later (the change was recorded before this run).
func offlineSince(d model.Device, hl model.Health) time.Time {
	seen := lastContact(d, hl)
	if hl.Since.After(seen) {
		return hl.Since
	}
	return seen
}

// healthMore is the detail of a state change as the tail of the Activity line.
func healthMore(source model.HealthSource, detail string) string {
	if d := healthDetail(source, detail); d != "" {
		return " · " + d
	}
	return ""
}
