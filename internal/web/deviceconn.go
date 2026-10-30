package web

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/transport"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The "Paths" card of the device page (#261): how TasmoFleet reaches the device (HTTP, MQTT, the
// device's MQTT password, the firmware server) and which way each action takes, or why none.
// Every sentence comes from package transport (Choose and its explainers); this file only
// capitalises, orders and tones them, so the card, the disabled buttons and the errors of the
// router say the same thing.

// reasonOffline is blockReason's sentence for a device that stopped answering.
const reasonOffline = "The device is offline."

// forgetFWReach is the admin route that forgets what is known about the device reaching the
// firmware server (WP-E2, spec §17).
const forgetFWReach = "/fwreach/forget"

// Row keys the summaries look up.
const (
	keyHTTP = "HTTP"
	keyMQTT = "MQTT"
	keyFW   = "Reaches the firmware server"
)

// pathRow is one line of the card: a label, a state with its tone, and a sentence under it.
type pathRow struct {
	Key, Value string
	Tone       ui.Tone
	Sub        string
	Since      time.Time // set: Sub is followed by "since" and this instant
	SinceText  string    // its fallback text
	Experiment bool      // the row carries the "Experimental" badge
	Forget     bool      // the row carries the Forget button
}

// pathCard is the card; Facts are the connection rows, Actions the per-action rows.
type pathCard struct {
	ID      model.DeviceID
	Base    string
	CSRF    string
	Facts   []pathRow
	Actions []pathRow
	Forget  string // action of the Forget form, "" for a viewer who may not use it
}

// FactsSummary is the line of the closed Connection list: the two states that decide the path.
// Both lists are closed so the overview stays within the phone height budget.
func (c pathCard) FactsSummary() string {
	s := "Connection: HTTP " + strings.ToLower(rowByKey(c.Facts, keyHTTP).Value) + ", MQTT " + strings.ToLower(rowByKey(c.Facts, keyMQTT).Value)
	if fw := rowByKey(c.Facts, keyFW); fw.Value == "No" {
		s += ", firmware server no"
	}
	return s
}

// rowByKey finds a row by its label; a missing row is the zero row.
func rowByKey(rows []pathRow, key string) pathRow {
	for _, r := range rows {
		if r.Key == key {
			return r
		}
	}
	return pathRow{}
}

// ActionsSummary is the line of the closed Actions list: how many actions have no way now. The
// list is closed so the overview stays within the phone height budget.
func (c pathCard) ActionsSummary() string {
	n := 0
	for _, r := range c.Actions {
		if r.Value == "Not available" {
			n++
		}
	}
	if n == 0 {
		return "What each action uses (all available)"
	}
	return "What each action uses (" + strconv.Itoa(n) + " not available)"
}

// pathLabel is the short word of a path for the Action rows.
func pathLabel(p transport.Path) string {
	switch p {
	case transport.HTTP:
		return "HTTP"
	case transport.MQTT:
		return "MQTT"
	case transport.DownloadHTTP:
		return "Download over HTTP"
	case transport.DownloadMQTT:
		return "Download over MQTT"
	case transport.PushHTTP:
		return "Upload over HTTP"
	case transport.PushSafeboot:
		return "Upload over HTTP (safeboot)"
	case transport.PushMQTT:
		return "Upload over MQTT"
	case transport.None:
	}
	return "Not available"
}

// transportOf reads the facts of a device and decides its paths. ok is false when the page has no
// transport service, or the facts could not be read: the page then behaves as before (a failed
// read costs the card, not the page).
func (h *handler) transportOf(ctx context.Context, id model.DeviceID) (transport.Facts, transport.Paths, bool) {
	if h.d.Transport == nil {
		return transport.Facts{}, transport.Paths{}, false
	}
	f, err := h.d.Transport.Facts(ctx, id)
	if err != nil {
		h.log.Warn("transport facts", "device", id, "err", err)
		return transport.Facts{}, transport.Paths{}, false
	}
	return f, transport.Choose(f), true
}

// overMQTT says whether a path travels over MQTT. An offline device can only be reached that way:
// every other path (HTTP, a download or upload over HTTP) needs the device to answer HTTP.
func overMQTT(p transport.Path) bool {
	return p == transport.MQTT || p == transport.DownloadMQTT || p == transport.PushMQTT
}

// FailingHTTPSource gives the facts of the devices whose HTTP calls fail, for the ids asked: only
// those can be managed over MQTT, so the device list reads nothing for the others. A source that
// knows of no failure answers without a single read. Optional on Deps.Transport.
type FailingHTTPSource interface {
	FailingHTTP(ctx context.Context, ids []model.DeviceID) (map[model.DeviceID]transport.Facts, error)
}

// failingHTTP reads, once for the whole list, the facts of the devices on our broker whose HTTP
// fails (the "MQTT" marker needs nothing else). A failed read only costs the markers.
func (h *handler) failingHTTP(ctx context.Context, devs []model.Device) map[model.DeviceID]transport.Facts {
	src, ok := h.d.Transport.(FailingHTTPSource)
	if !ok {
		return nil
	}
	var ids []model.DeviceID
	for _, d := range devs {
		if d.MQTT == model.MQTTOurs {
			ids = append(ids, d.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	m, err := src.FailingHTTP(ctx, ids)
	if err != nil {
		h.log.Warn("transport facts for the device list", "err", err)
		return nil
	}
	return m
}

// actionWhy is the reason an action's button is disabled, "" when it may be used. A path of None
// carries Choose's sentence; a device that is offline keeps the offline sentence for every path
// that is not over MQTT (nothing said HTTP fails, but the device does not answer).
func actionWhy(general string, p transport.Path, reason string) string {
	if p == transport.None {
		return sentence(reason)
	}
	if general == reasonOffline && !overMQTT(p) {
		return general
	}
	return ""
}

// applyPaths sets the per-action reasons of the tabs, the Back up button and the card. Without
// paths every action shows the general reason, as before.
func (t *deviceTabs) applyPaths(p transport.Paths, ok bool) {
	t.SettingsWhy, t.ConsoleWhy, t.BackupWhy = t.Reason, t.Reason, t.Reason
	if t.Health == model.HealthOffline {
		t.UpdateWhy = reasonOffline
	}
	if !ok {
		return
	}
	// A running operation or a device that never answered blocks everything, whatever the path.
	general := t.Reason
	if general != "" && general != reasonOffline {
		return
	}
	t.SettingsWhy = actionWhy(general, p.Writes, p.Reason(transport.ActWrites))
	t.ConsoleWhy = actionWhy(general, p.Commands, p.Reason(transport.ActCommands))
	t.BackupWhy = actionWhy(general, p.Backup, p.Reason(transport.ActBackup))
	t.RestoreWhy = actionWhy(general, p.Restore, p.Reason(transport.ActRestore))
	t.UpdateWhy = actionWhy(general, p.Update, p.Reason(transport.ActUpdate))
}

// updateBtnWhy is why the header's "Update to" is disabled. Only the device's state gates it: the
// delivery (and so the path) is chosen on the update page, so the card's pull-only Update path
// must not decide here.
func (t deviceTabs) updateBtnWhy() string {
	if t.Updating || t.Health == model.HealthOffline {
		return t.Reason
	}
	return ""
}

// backupNote is the visible line next to the actions for a Back up that has no path; the offline
// case has its banner already (described by it).
func (t deviceTabs) backupNote() string {
	if t.BackupWhy == "" || t.BackupWhy == reasonOffline {
		return ""
	}
	return t.BackupWhy
}

// updateDescribedBy names the element that says why "Update to" is disabled.
func (t deviceTabs) updateDescribedBy() string {
	if t.Updating {
		return "devhd-why"
	}
	return "devhd-off"
}

// backupDescribedBy names the element that says why Back up is disabled.
func (t deviceTabs) backupDescribedBy() string {
	if t.backupNote() != "" {
		return "devhd-why"
	}
	return "devhd-off"
}

// pathCardOf builds the card for the viewer.
//
// The Actions rows show the reason the buttons carry (t), so a device that never answered or is
// held by an operation reads the same in the card as on its buttons; otherwise the chooser's.
func pathCardOf(v view, d model.Device, f transport.Facts, p transport.Paths, t deviceTabs) *pathCard {
	c := &pathCard{ID: d.ID, Base: v.Base, CSRF: v.CSRF}
	c.Facts = append(c.Facts, httpRow(f), mqttRow(f), passwordRow(f), fwRow(f), uploadRow(d))
	if v.IsAdmin && transport.EffectiveFWReach(f) == model.FWReachNo {
		c.Forget = v.Base + devPath(d.ID, forgetFWReach)
		for i := range c.Facts {
			if c.Facts[i].Key == keyFW {
				c.Facts[i].Forget = true
			}
		}
	}
	for _, a := range []struct {
		key  string
		act  transport.Action
		path transport.Path
		why  string
	}{
		{"Console and Settings: reading", transport.ActCommands, p.Commands, t.ConsoleWhy},
		{"Console and Settings: changing", transport.ActWrites, p.Writes, t.SettingsWhy},
		{"Back up", transport.ActBackup, p.Backup, t.BackupWhy},
		{"Restore", transport.ActRestore, p.Restore, t.RestoreWhy},
		{"Update (download)", transport.ActUpdate, p.Update, t.UpdateWhy},
	} {
		row := pathRow{Key: a.key, Value: pathLabel(a.path), Tone: ui.ToneOK, Sub: sentence(p.Reason(a.act))}
		if a.path == transport.None || a.why != "" {
			row.Value, row.Tone = "Not available", ui.ToneWarn
			if a.why != "" {
				row.Sub = a.why
			}
		}
		c.Actions = append(c.Actions, row)
	}
	return c
}

func httpRow(f transport.Facts) pathRow {
	r := pathRow{Key: keyHTTP}
	switch f.HTTP.State {
	case transport.HTTPFailing:
		r.Value, r.Tone, r.Sub = "Not reachable", ui.ToneDanger, sentence(f.HTTP.Describe())
		r.setSince(f.HTTP.Since, f.Now)
	case transport.HTTPOK:
		r.Value, r.Tone, r.Sub = "Reachable", ui.ToneOK, "TasmoFleet's last HTTP call to the device succeeded."
	case transport.HTTPUnknown:
		r.Value, r.Tone, r.Sub = "Not checked lately", ui.ToneNeutral, "TasmoFleet tries HTTP first and switches to MQTT when it fails."
	}
	return r
}

func (r *pathRow) setSince(at, now time.Time) {
	if !at.IsZero() {
		r.Since, r.SinceText = at, ui.StampDayText(now, at)
	}
}

func mqttRow(f transport.Facts) pathRow {
	r := pathRow{Key: keyMQTT}
	topic := ""
	if f.MQTT.Topic != "" {
		topic = "Topic " + f.MQTT.Topic
	}
	join := func(parts ...string) string {
		var out []string
		for _, s := range parts {
			if s != "" {
				out = append(out, s)
			}
		}
		return strings.Join(out, " · ")
	}
	switch strict, reads := transport.MQTTUnusable(f, true), transport.MQTTUnusable(f, false); {
	case strict == "":
		r.Value, r.Tone, r.Sub = "On our broker", ui.ToneOK, join(topic, "Last will online")
	case reads == "":
		r.Value, r.Tone = "Reading only", ui.ToneWarn
		r.Sub = join(topic, "Changing the device over MQTT needs more: "+strict+".")
	default:
		r.Value, r.Tone = "Not usable", ui.ToneNeutral
		r.Sub = sentence(reads)
	}
	return r
}

func passwordRow(f transport.Facts) pathRow {
	r := pathRow{Key: "Device MQTT password", Tone: ui.ToneNeutral}
	p := f.Password
	switch {
	case p.Unusable != "":
		r.Value, r.Tone, r.Sub = "Unusable", ui.ToneWarn, sentence(p.Unusable)
	case !p.BadAt.IsZero():
		r.Value, r.Tone, r.Sub = "Refused", ui.ToneDanger, "The device refused it."
		r.setSince(p.BadAt, f.Now)
	case p.Mode == model.MQTTPasswordBroker:
		r.Value, r.Tone, r.Sub = "Same as the broker's", ui.ToneOK, "Back up, restore and upload over MQTT send TasmoFleet's broker password."
	case p.Mode == model.MQTTPasswordOwn:
		r.Value, r.Tone, r.Sub = "Set", ui.ToneOK, "Back up, restore and upload over MQTT send the stored password."
	default:
		r.Value, r.Sub = "Not set", "Needed only to back up, restore and upload over MQTT; commands never use it."
	}
	return r
}

func fwRow(f transport.Facts) pathRow {
	r := pathRow{Key: keyFW}
	if !f.FirmwareBaseURL {
		r.Value, r.Tone, r.Sub = "Not available", ui.ToneWarn, sentence(transport.FirmwareServerBlocker(f))
		return r
	}
	switch transport.EffectiveFWReach(f) {
	case model.FWReachYes:
		r.Value, r.Tone, r.Sub = "Yes", ui.ToneOK, "The device fetched from TasmoFleet's firmware server."
	case model.FWReachNo:
		r.Value, r.Tone, r.Sub = "No", ui.ToneDanger, "The device cannot reach TasmoFleet's firmware server."
		r.setSince(f.FWReach.At, f.Now)
	case model.FWReachUnknown:
		r.Value, r.Tone, r.Sub = "Unknown", ui.ToneNeutral, "TasmoFleet learns it when the device fetches a file or fails to."
	}
	return r
}

// uploadRow is the experimental upload over MQTT: whether the chip and its free flash allow it at
// all. The fit of a real image is decided when an update is planned.
func uploadRow(d model.Device) pathRow {
	r := pathRow{Key: "Upload over MQTT", Experiment: true, Tone: ui.ToneNeutral}
	// An image size of 1 byte asks the question "could any image go this way".
	ok, _, room, why := fwselect.MQTTUploadFits(d.Identity, fwselect.UpLayoutUnknown, 1)
	if ok {
		r.Value, r.Tone, r.Sub = "Fits", ui.ToneOK, strconv.Itoa(room)+" KB room for a firmware image."
		return r
	}
	r.Value, r.Sub = "Not available", sentence(why)
	return r
}
