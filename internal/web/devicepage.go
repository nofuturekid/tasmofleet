package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The device overview (#145, mockups in docs/design/device-overview): a row of live tiles, the
// relay box, Identity next to Connection and one Activity list. The live part (tiles, relay box,
// Activity, header, offline banner) is rebuilt by GET /devices/{id}/live whenever the device
// changes (#126); the Identity card is never swapped because it holds answers and notices.

// deviceOverview is the view of GET /devices/{id}.
type deviceOverview struct {
	Tabs     deviceTabs
	Base     string
	CSRF     string
	Updating *jobView // an operation holds the device: it is silent on purpose
	NotYet   string   // the device never answered: what to check
	Live     liveView
	Ident    identCard
	Conn     connCard
	Paths    *pathCard // nil without the transport service
	Activity devActivity
}

// liveView is the part of the overview that follows the device: tiles and the relay box.
type liveView struct {
	ID   model.DeviceID
	Base string
	CSRF string
	// Off dims the region: the device does not answer, what shows is its last reported state.
	Off   bool
	Tiles []tile
	Box   *relayBox // nil: the device has no relays
	// Sensors replaces the relay box on a device without relays
	Sensors *sensorBox
}

// tile is one stat tile of the top row. A tile that does not apply is not built at all, so the
// row never has holes.
type tile struct {
	Key, Label, Value, Sub string
	Tone                   ui.Tone // colour of the value
	Bar                    int     // Wi-Fi signal quality 0..100; 0 draws no bar
	BarTone                ui.Tone
	SubPrefix              string    // "Last restart ": the sub line is this text and the time At
	At                     time.Time // zero: the sub line is just Sub
	AtText                 string    // the fallback text of At (StampDay), for a page without JavaScript
	Fix                    bool      // "Make compliant…" for this device
	Mask                   bool      // a reading the pixel comparison cannot reproduce
}

// relayBox is the box of relay switches. Right is the metering line of the box header.
type relayBox struct {
	Right   string
	Rows    []relayRow
	Enabled bool   // the switches can be used now
	Err     string // why the last switch failed (htmx switches show it in place)
}

// sensorBox is the Sensors box of a device without relays.
type sensorBox struct {
	Right string
	Cells []sensorCell
	Note  string // why there are no cells
}

// sensorCell is one reading: "Temperature · BME280" and "12.4 °C".
type sensorCell struct{ Label, Value string }

// relayRow is one relay switch. Line is what shows under the name: the channel's watts when the
// device meters each channel, else On/Off.
type relayRow struct {
	N            int
	Name, Cmd    string
	Line         string
	On           bool
	Action, Aria string
}

// devActivity is the Activity box: the newest state changes and transactions of the device,
// merged. All is the complete, paged list.
type devActivity struct {
	Base string
	Rows []devActRow
	All  string
}

// devActRow is one state change or one transaction. Exactly one of Event and Job is set.
type devActRow struct {
	At    time.Time
	Event *store.HealthEvent
	Job   *model.Job
}

// connCard is the Connection card: how TasmoFleet reaches the device and how the device is
// connected.
type connCard struct {
	Title string // "Connection · Wi-Fi" or "Connection · Ethernet"
	Rows  []ui.KVRow
}

// overviewRows is how many entries the Activity box shows; the complete list is one link away.
const overviewRows = 10

// historyView is the page of all activity of one device.
type historyView struct {
	Tabs  deviceTabs
	Base  string
	Rows  []devActRow
	Pager pager
}

// identCard is the Identity card. "Refresh identity" swaps the whole card, so the rows show what
// the probe found and a failed probe leaves the button where it was.
type identCard struct {
	Base     string
	ID       model.DeviceID
	Rows     []ui.KVRow
	CanProbe bool // not while an operation holds the device
	Off      bool // dimmed: the device does not answer, so Refresh identity is disabled too
	Notice   string
	Error    string
	// NameOffer is the DeviceName the device reports while the stored name differs (#125);
	// "" shows nothing. Admins get the two answers, KeepOK says Keep can be saved.
	NameOffer string
	CanAnswer bool
	KeepOK    bool
}

// policyCard is the device's policy compliance: the rules that apply, the deviations, and
// "Make compliant…" for admins.
type policyCard struct {
	Rules      int
	Deviations []deviation
	Unknown    int // rules whose value could not be read
	CanFix     bool
}

type deviation struct{ Rule, Have, Want string }

// snapFacts are the network settings of the device (Status 5 and 11). The live health has none
// of them; they change only when someone reconfigures the device. A probe stores them on the
// device (model.Device.Net); the newest backup snapshot is the fallback for devices that were
// not probed since (see factsOf).
type snapFacts struct {
	Hostname, IP, Gateway string
	Subnet, DNS           string // the active interface's subnet mask and DNS servers ("" = not reported)
	SSID, BSSID           string
	Channel               int
	Wired                 bool // on Ethernet: Hostname, IP and Gateway are the Ethernet ones
}

// parseSnapFacts reads the facts from a snapshot document (Status1..Status11 replies by key).
// A failed command is recorded as {"_error": ...} and simply has no facts.
func parseSnapFacts(doc []byte) snapFacts {
	var s struct {
		Status5 struct {
			StatusNET struct{ Hostname, IPAddress, Gateway string }
		}
		Status11 struct {
			StatusSTS struct {
				Wifi struct {
					SSId, BSSId string
					Channel     int
				}
			}
		}
	}
	if json.Unmarshal(doc, &s) != nil {
		return snapFacts{}
	}
	w := s.Status11.StatusSTS.Wifi
	return snapFacts{Hostname: s.Status5.StatusNET.Hostname, IP: s.Status5.StatusNET.IPAddress, Gateway: s.Status5.StatusNET.Gateway, SSID: w.SSId, BSSID: w.BSSId, Channel: w.Channel}
}

// snapFactsOf reads the newest snapshot of the device. Without one (or when it cannot be
// read) the facts are empty and the rows say "Unknown"; the page still renders.
func (h *handler) snapFactsOf(ctx context.Context, list []model.Backup) snapFacts {
	for _, b := range list { // newest first
		if b.Kind != model.BackupSnapshot {
			continue
		}
		doc, _, err := h.d.Backups.Open(ctx, b.ID)
		if err != nil {
			h.log.WarnContext(ctx, "device overview: snapshot unreadable", "backup", b.ID, "err", err)
			return snapFacts{}
		}
		return parseSnapFacts(doc)
	}
	return snapFacts{}
}

// factsOf are the device's network facts: what a probe stored on it, field by field; the newest
// backup snapshot only fills in a device that was never probed.
func (h *handler) factsOf(ctx context.Context, d model.Device) snapFacts {
	f := snapFacts{Hostname: d.Net.Hostname, IP: d.Net.IP, Gateway: d.Net.Gateway, Subnet: d.Net.Subnet, DNS: d.Net.DNS, SSID: d.Net.SSID, BSSID: d.Net.BSSID, Channel: d.Net.Channel, Wired: d.Net.Wired}
	if !d.Net.ReadAt.IsZero() {
		return f
	}
	list, err := h.d.Backups.List(ctx, d.ID)
	if err != nil {
		h.log.WarnContext(ctx, "device overview: backups", "err", err)
		return f
	}
	return h.snapFactsOf(ctx, list)
}

// macShown is the MAC of the interface the device is on: the Ethernet one for a wired board,
// else the primary (Wi-Fi) one that identifies the device. A wired board's Ethernet MAC is the
// other entry of its MAC set.
func macShown(d model.Device, wired bool) string {
	if wired {
		for _, m := range d.MACs {
			if m != d.MAC {
				return m
			}
		}
	}
	return d.MAC
}

// orUnknown is the value of a row, "Unknown" when there is none.
func orUnknown(s string) string {
	if s == "" {
		return "Unknown"
	}
	return s
}

// identity builds the Identity card for the viewer. Credentials never leave the server: the
// card says only whether a web login is stored.
func (h *handler) identity(v view, d model.Device, updating, off bool, f snapFacts) identCard {
	hw := d.Identity.Hardware
	if mb := d.Identity.FlashSizeKB / 1024; hw != "" && mb > 0 {
		hw += " · " + strconv.Itoa(mb) + " MB flash"
	}
	fw := ""
	if d.Identity.Version != "" {
		fw = "Tasmota " + d.Identity.Version
	}
	build := d.Identity.Variant
	if build == "unknown" && d.Identity.Image != "" && !strings.Contains(d.Identity.Image, "-") {
		build = d.Identity.Image + " (self-compiled)" // a bare tag: the name the device reports
	} else if build != "" && d.Identity.Language != "" {
		build += " " + d.Identity.Language
	}
	rows := []ui.KVRow{
		{Key: "Device name", Value: d.Name, Mono: true},
		{Key: "Hostname", Value: orUnknown(f.Hostname), Mono: true, Mask: true},
		{Key: "MQTT topic", Value: orUnknown(d.Topic), Mono: true},
		{Key: "Hardware", Value: orUnknown(hw), Mono: true},
		{Key: "Build", Value: orUnknown(build), Mono: true},
		{Key: "Firmware", Value: orUnknown(fw), Mono: true},
	}
	if d.OTAURL != "" { // #127: where the device would update from; the prober stored it without credentials
		rows = append(rows, ui.KVRow{Key: "OTA URL", Value: d.OTAURL, Mono: true, Soft: true})
	}
	rows = append(rows, ui.KVRow{Key: "MAC", Value: orUnknown(d.MAC), Mono: true, Mask: true})
	if d.HTTPUser != "" || d.HTTPPassword != "" {
		login := "password set"
		if d.HTTPPassword == "" {
			login = "no password"
		}
		if d.HTTPUser != "" {
			login = d.HTTPUser + ", " + login
		}
		rows = append(rows, ui.KVRow{Key: "Web login", Value: login, Mono: true})
	}
	if d.OTAExcluded {
		rows = append(rows, ui.KVRow{Key: "Updates", Value: "Excluded from OTA updates"})
	}
	for _, t := range d.Tags {
		rows = append(rows, ui.KVRow{Key: "Tag", Value: t})
	}
	if d.Notes != "" {
		rows = append(rows, ui.KVRow{Key: "Notes", Value: d.Notes})
	}
	return identCard{Base: v.Base, ID: d.ID, Rows: rows, CanProbe: !updating, Off: off,
		NameOffer: nameOffer(d), CanAnswer: v.IsAdmin, KeepOK: h.d.DeviceNames != nil}
}

// policyOf is the device's part of the policy report (values already read; nothing is sent).
func (h *handler) policyOf(r *http.Request, v view, id model.DeviceID) *policyCard {
	if h.d.Policy == nil {
		return nil
	}
	ctx := r.Context()
	rep, err := h.d.Policy.Report(ctx, actor(r), false)
	if err != nil {
		h.log.WarnContext(ctx, "device overview: policy report", "err", err)
		return nil
	}
	c := &policyCard{Rules: len(rep.Rules), CanFix: v.IsAdmin && h.d.PolicyFix != nil}
	for _, f := range rep.Findings {
		switch {
		case f.DeviceID != id:
		case f.Unknown:
			c.Unknown++
		default:
			c.Deviations = append(c.Deviations, deviation{Rule: ruleTitle(f.Rule), Have: f.Rule.Command + " " + f.Have, Want: f.Rule.Want})
		}
	}
	c.CanFix = c.CanFix && len(c.Deviations) > 0
	return c
}

// ruleTitle is a rule's name, or its setting when it has none.
func ruleTitle(r policy.Rule) string {
	if r.Name != "" {
		return r.Name
	}
	return r.Command + " " + r.Want
}

func (h *handler) deviceProbe(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	v := h.view(r, "", "", nil)
	var notice, failure string
	err := h.d.Prober.Refresh(r.Context(), d.ID, actor(r))
	var attention interface{ Notice() string }
	if errors.As(err, &attention) {
		err = nil // refreshed, with one thing to tell the admin (#111)
	}
	if err != nil {
		failure = h.refreshProblem(r, err)
	} else {
		notice = "Identity refreshed."
		if attention != nil {
			notice += " " + attention.Notice()
		}
		if fresh, gerr := h.d.Inventory.Get(r.Context(), d.ID); gerr == nil {
			d = fresh
		} else {
			h.log.WarnContext(r.Context(), "probe: reload device", "err", gerr)
		}
	}
	hl, running := h.deviceState(r, d)
	tabs := h.headOf(r.Context(), v, d, hl, running, "overview")
	c := h.identity(v, d, running != nil, tabs.offline(), h.factsOf(r.Context(), d))
	c.Notice, c.Error = notice, failure
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	parts := []templ.Component{identityCard(c)}
	if failure == "" {
		// The header, breadcrumb and title show the same stored device as the card (#104).
		parts = append(parts, deviceHeadSwap(tabs, deviceCrumbs(d)))
	}
	if err := templ.Join(parts...).Render(r.Context(), w); err != nil {
		h.log.WarnContext(r.Context(), "probe: render", "err", err)
	}
}
