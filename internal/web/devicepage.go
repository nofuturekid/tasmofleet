package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/health"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The device overview (design v2, WP P3; prototype screen "device", tab "overview"): the cards
// Identity, Power & energy, Network, Wi-Fi and Policy compliance, then the device's state
// changes and transactions (kept from v1, below the prototype's cards: structure deviation S8).

// deviceOverview is the view of GET /devices/{id}.
type deviceOverview struct {
	Tabs     deviceTabs
	Base     string
	CSRF     string
	Updating *jobView // an operation holds the device: it is silent on purpose
	NotYet   string   // the device never answered: what to check
	Ident    identCard
	Energy   *energyCard // nil: the device reports no energy readings
	Relays   []relayRow
	// RelaysOn says whether the relay switches can be used (command service, device answering).
	RelaysOn bool
	Network  []ui.KVRow
	Wifi     []ui.KVRow
	WifiPct  int // signal quality bar, 0..100; 0 hides the bar's fill
	WifiOK   bool
	Policy   *policyCard
	Events   []store.HealthEvent
	Jobs     []model.Job
	// HistHref is the complete list of the device's state changes; TxHref the transactions
	// filtered by this device ("" without the update feature). The cards show the newest
	// overviewRows of each.
	HistHref, TxHref string
}

// overviewRows is how many state changes and transactions the overview shows; the complete
// lists are one link away.
const overviewRows = 10

// historyView is the page of all state changes of one device.
type historyView struct {
	Tabs   deviceTabs
	Base   string
	Events []store.HealthEvent
	Pager  pager
}

// identCard is the Identity card. "Refresh identity" swaps the whole card, so the rows show what
// the probe found and a failed probe leaves the button where it was.
type identCard struct {
	Base     string
	ID       model.DeviceID
	Rows     []ui.KVRow
	CanProbe bool // not while an operation holds the device
	Notice   string
	Error    string
}

// energyCard is the power reading now, the last 2 hours as a sparkline and the energy counters
// of the device's meter.
type energyCard struct {
	Watts string
	Spark []sparkBar // empty: nothing to draw, SparkNote says why
	// SparkNote replaces the sparkline when there is nothing to draw: no readings yet, or a
	// device that drew nothing for 2 hours (a row of 24 stubs would read as a dashed line).
	SparkNote string
	Rows      []ui.KVRow
}

// sparkBar is one bar of the power sparkline in viewBox units (presentation attributes, D6).
type sparkBar struct{ X, Y, H string }

// The sparkline (N8) draws the 2-hour power history like the prototype: bars, one per 5-minute
// bucket (Tasmota's default TelePeriod 300 gives one reading each), the bucket's mean watts
// scaled to the highest bucket. A bucket without a reading stays empty, so a gap in the history
// is a gap in the line. The smallest bar keeps a stub so a 0 W reading is still visible.
const (
	sparkBuckets = 24
	sparkStep    = 5 * time.Minute
	sparkPitch   = 10 // viewBox units per bucket
	sparkBarW    = "8"
	sparkHeight  = 40
	sparkStub    = 3
	sparkViewBox = "0 0 240 40" // sparkBuckets*sparkPitch x sparkHeight
)

// sparkOf turns the power history (oldest first, at most 2 h back from now) into bars.
func sparkOf(samples []health.PowerSample, now time.Time) []sparkBar {
	var sum [sparkBuckets]float64
	var n [sparkBuckets]int
	start := now.Add(-sparkBuckets * sparkStep)
	for _, s := range samples {
		if !s.At.After(start) || s.At.After(now) {
			continue
		}
		i := min(int(s.At.Sub(start)/sparkStep), sparkBuckets-1)
		sum[i] += s.Watts
		n[i]++
	}
	peak := 0.0
	for i := range sum {
		if n[i] > 0 {
			peak = max(peak, sum[i]/float64(n[i]))
		}
	}
	var out []sparkBar
	for i := range sum {
		if n[i] == 0 {
			continue
		}
		h := float64(sparkStub)
		if peak > 0 {
			h += (sparkHeight - sparkStub) * max(sum[i]/float64(n[i]), 0) / peak
		}
		f := func(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }
		out = append(out, sparkBar{X: strconv.Itoa(i * sparkPitch), Y: f(sparkHeight - h), H: f(h)})
	}
	return out
}

// sparkNote says why the sparkline is not drawn: there are no readings in the window yet, or
// every one of them is 0 W. "" means draw the bars.
func sparkNote(samples []health.PowerSample, now time.Time) string {
	start := now.Add(-sparkBuckets * sparkStep)
	seen, drew := false, false
	for _, s := range samples {
		if !s.At.After(start) || s.At.After(now) {
			continue
		}
		seen = true
		drew = drew || s.Watts > 0
	}
	switch {
	case !seen:
		return "No readings yet"
	case !drew:
		return "No load in the last 2 h"
	}
	return ""
}

// relayRow is one relay switch of the Power & energy card.
type relayRow struct {
	N            int
	Name, Cmd    string
	State        string // "On", "Off" or "Unknown"
	On           bool
	Action, Aria string
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

// snapFacts are the network settings of the device as its newest backup snapshot recorded
// them (Status 5 and 11). The live health has none of them; they change only when someone
// reconfigures the device, and the next backup records that.
type snapFacts struct {
	Hostname, Gateway string
	SSID, BSSID       string
	Channel           int
}

// parseSnapFacts reads the facts from a snapshot document (Status1..Status11 replies by key).
// A failed command is recorded as {"_error": ...} and simply has no facts.
func parseSnapFacts(doc []byte) snapFacts {
	var s struct {
		Status5 struct {
			StatusNET struct{ Hostname, Gateway string }
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
	return snapFacts{Hostname: s.Status5.StatusNET.Hostname, Gateway: s.Status5.StatusNET.Gateway, SSID: w.SSId, BSSID: w.BSSId, Channel: w.Channel}
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

// orUnknown is the value of a row, "Unknown" when there is none.
func orUnknown(s string) string {
	if s == "" {
		return "Unknown"
	}
	return s
}

// identity builds the Identity card for the viewer. Credentials never leave the server: the
// card says only whether a web login is stored.
func (h *handler) identity(v view, d model.Device, updating bool, f snapFacts) identCard {
	chip := chipText(d.Identity.Family)
	if mb := d.Identity.FlashSizeKB / 1024; chip != "" && mb > 0 {
		chip += " · " + strconv.Itoa(mb) + " MB flash"
	}
	fw := ""
	if d.Identity.Version != "" {
		fw = "Tasmota " + d.Identity.Version
	}
	build := d.Identity.Variant
	if build != "" && d.Identity.Language != "" {
		build += " " + d.Identity.Language
	}
	rows := []ui.KVRow{
		{Key: "Friendly name", Value: d.Name, Mono: true},
		{Key: "Hardware", Value: orUnknown(d.Identity.Hardware), Mono: true},
		{Key: "Build", Value: orUnknown(build), Mono: true},
		{Key: "Chip", Value: orUnknown(chip), Mono: true},
		{Key: "Firmware", Value: orUnknown(fw), Mono: true},
		{Key: "Hostname", Value: orUnknown(f.Hostname), Mono: true, Mask: true},
		{Key: "MAC", Value: orUnknown(d.MAC), Mono: true, Mask: true},
		{Key: "MQTT topic", Value: orUnknown(d.Topic), Mono: true},
	}
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
	return identCard{Base: v.Base, ID: d.ID, Rows: rows, CanProbe: !updating}
}

// energyOf is the Power & energy reading of the device's meter (channel "" or the first one).
func energyOf(t health.Telemetry) *energyCard {
	if len(t.Energy) == 0 {
		return nil
	}
	ch, ok := t.Energy[""]
	if !ok {
		ch = t.Energy["1"]
	}
	num := func(p *float64, unit string, prec int) string {
		if p == nil {
			return "—"
		}
		return strconv.FormatFloat(*p, 'f', prec, 64) + " " + unit
	}
	c := &energyCard{Watts: "—"}
	if ch.Power != nil {
		c.Watts = strconv.FormatFloat(*ch.Power, 'f', 0, 64)
	}
	c.Rows = []ui.KVRow{
		{Key: "Today", Value: num(ch.Today, "kWh", 2), Mono: true, Mask: true},
		{Key: "Yesterday", Value: num(ch.Yesterday, "kWh", 2), Mono: true, Mask: true},
		{Key: "Total", Value: num(ch.Total, "kWh", 1), Mono: true, Mask: true},
	}
	return c
}

// relaysOf are the relay switches: "POWER" for a single relay, "POWER1".. for several.
func relaysOf(hl model.Health) []relayRow {
	rs := relayStates(hl)
	out := make([]relayRow, 0, len(rs))
	for _, r := range rs {
		row := relayRow{N: r.N, Name: "Relay " + strconv.Itoa(r.N), Cmd: "POWER", State: "Unknown", On: r.State == "ON", Action: "ON"}
		if len(rs) > 1 {
			row.Cmd += strconv.Itoa(r.N)
		}
		switch r.State {
		case "ON":
			row.State, row.Action = "On", "OFF"
		case "OFF":
			row.State = "Off"
		}
		row.Aria = row.Name + " is " + row.State + ": switch it " + label(row.Action)
		out = append(out, row)
	}
	return out
}

// networkRows is the Network card: the address the fleet uses, the gateway the device was set
// up with, its broker and how the two channels answer now.
func (h *handler) networkRows(d model.Device, hl model.Health, f snapFacts) []ui.KVRow {
	addr := d.Address
	if d.Port != 0 && d.Port != 80 {
		addr = net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	}
	broker := ""
	if d.MQTTHost != "" {
		port := d.MQTTPort
		if port == 0 {
			port = 1883
		}
		broker = net.JoinHostPort(d.MQTTHost, strconv.Itoa(port))
	}
	// Connected / Not connected for a device on our broker; otherwise the sentence says why
	// TasmoFleet does not watch it (disabled, another broker, no broker configured).
	mqtt := h.mqttLine(d)
	switch {
	case hl.LWT == "Online" || hl.LWT == "" && !hl.SeenMQTT.IsZero() && hl.State == model.HealthOnline:
		mqtt = "Connected"
	case hl.LWT == "Offline" || d.MQTT == model.MQTTOurs:
		mqtt = "Not connected"
	}
	httpState := "Not polled yet"
	switch {
	case hl.State == model.HealthAuthFailed:
		httpState = "Refuses the stored credentials"
	case hl.State == model.HealthOffline || hl.State == model.HealthUnknown && !hl.LastSeen.IsZero():
		httpState = "No response"
	case !hl.SeenHTTP.IsZero():
		httpState = "Reachable"
	}
	restart := ""
	if t := restartAt(hl); !t.IsZero() {
		restart = sinceText(t)
	}
	return []ui.KVRow{
		{Key: "IP address", Value: addr, Mono: true, Mask: true},
		{Key: "Gateway", Value: orUnknown(f.Gateway), Mono: true, Mask: true},
		{Key: "MQTT broker", Value: orUnknown(broker), Mono: true, Soft: true},
		{Key: "MQTT", Value: mqtt, Mono: true},
		{Key: "HTTP", Value: httpState, Mono: true},
		{Key: "Last restart", Value: orUnknown(restart), Mono: true, Mask: true},
	}
}

// wifiRows is the Wi-Fi card and its quality bar (the prototype's scale: -92 dBm is empty,
// -40 dBm full; below -70 dBm the bar turns to the warning colour).
func wifiRows(hl model.Health, t health.Telemetry, f snapFacts, now time.Time) (rows []ui.KVRow, pct int, ok bool) {
	dbm, has := t.SignalDBm, t.HasSignal
	if !has && hl.RSSI > 0 {
		dbm, has = hl.RSSI/2-100, true // Tasmota maps -100..-50 dBm to 0..100 %
	}
	signal, quality := "—", "—"
	if has {
		signal = strconv.Itoa(dbm) + " dBm"
		pct = min(100, max(6, (dbm+92)*19/10))
		ok = dbm > -70
	}
	if hl.RSSI > 0 {
		quality = strconv.Itoa(hl.RSSI) + " %"
	}
	channel := ""
	if f.Channel > 0 {
		band := "2.4 GHz"
		if f.Channel > 14 {
			band = "5 GHz"
		}
		channel = fmt.Sprintf("%d (%s)", f.Channel, band)
	}
	uptime := "—"
	switch {
	case hl.State == model.HealthOffline && !hl.LastSeen.IsZero():
		uptime = "seen " + agoWords(now, hl.LastSeen)
	case hl.UptimeSec > 0:
		uptime = humanDuration(time.Duration(hl.UptimeSec) * time.Second)
	}
	return []ui.KVRow{
		{Key: "SSID", Value: orUnknown(f.SSID), Mono: true, Mask: true},
		{Key: "BSSID", Value: orUnknown(f.BSSID), Mono: true, Mask: true},
		{Key: "Channel", Value: orUnknown(channel), Mono: true, Mask: true},
		{Key: "Signal", Value: signal, Mono: true, Mask: true},
		{Key: "Quality", Value: quality, Mono: true, Mask: true},
		{Key: "Uptime", Value: uptime, Mono: true, Mask: true},
	}, pct, ok
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

func (h *handler) deviceDetail(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	v := h.view(r, d.Name, "devices", nil)
	hl := h.d.Health.Get(d.ID)
	if hl.State == "" {
		hl.State = model.HealthUnknown
	}
	p := deviceOverview{Base: v.Base, CSRF: v.CSRF, RelaysOn: h.d.DeviceCmd != nil, HistHref: v.Base + devPath(d.ID, "/history")}
	if v.Features.Updates {
		p.TxHref = v.Base + "/transactions?device=" + strconv.FormatInt(int64(d.ID), 10)
	}
	var err error
	if h.d.History != nil {
		if p.Events, err = h.d.History.ListHealthEvents(ctx, d.ID, overviewRows); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	if h.d.Jobs != nil {
		if p.Jobs, err = h.d.Jobs.ListJobs(ctx, store.JobFilter{DeviceID: d.ID, Limit: overviewRows}); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	for _, j := range p.Jobs {
		if !j.State.Terminal() {
			// During an update the device stops answering (it downloads, then reboots). That is
			// expected, not a failure, so the page says so instead of offering commands.
			uv := jobView{Job: j, Base: v.Base}
			uv.Step, uv.StepLabel = jobStep(j.State, j.Stage)
			switch {
			case j.IsPolicy():
				uv.Step, uv.StepLabel = policyStep(j.State)
			case j.IsCommand():
				uv.Step, uv.StepLabel = commandStep(j.State)
			case j.IsBackup():
				uv.Step, uv.StepLabel = backupStep(j.State)
			}
			p.Updating = &uv
			break
		}
	}
	var running *model.Job
	if p.Updating != nil {
		running = &p.Updating.Job
	}
	list, err := h.d.Backups.List(ctx, d.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	p.Tabs = h.headOf(v, d, hl, running, "overview")
	if p.Updating == nil && hl.LastSeen.IsZero() && d.Identity.DetectedAt.IsZero() {
		p.NotYet = "This device has not answered yet"
		if !d.CreatedAt.IsZero() {
			p.NotYet += " (added " + sinceText(d.CreatedAt) + ")"
		}
		p.NotYet += ". Check that it is powered and that its address (" + d.Address + ") is right."
	}
	facts := h.snapFactsOf(ctx, list)
	var tele health.Telemetry
	if tl, ok := h.d.Health.(interface {
		Telemetry() map[model.DeviceID]health.Telemetry
	}); ok {
		tele = tl.Telemetry()[d.ID]
	}
	p.Ident = h.identity(v, d, p.Updating != nil, facts)
	p.Energy = energyOf(tele)
	if ph, ok := h.d.Health.(interface {
		PowerHistory(model.DeviceID) []health.PowerSample
	}); ok && p.Energy != nil {
		samples, now := ph.PowerHistory(d.ID), h.d.Clock.Now()
		if p.Energy.SparkNote = sparkNote(samples, now); p.Energy.SparkNote == "" {
			p.Energy.Spark = sparkOf(samples, now)
		}
	} else if p.Energy != nil {
		p.Energy.SparkNote = "No readings yet"
	}
	p.Relays = relaysOf(hl)
	p.RelaysOn = p.RelaysOn && hl.State != model.HealthOffline && p.Updating == nil
	p.Network = h.networkRows(d, hl, facts)
	p.Wifi, p.WifiPct, p.WifiOK = wifiRows(hl, tele, facts, h.d.Clock.Now())
	p.Policy = h.policyOf(r, v, d.ID)
	v.Live = true
	h.headPage(w, r, http.StatusOK, "device", v, d, deviceOverviewPage(p))
}

func (h *handler) deviceProbe(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	v := h.view(r, "", "", nil)
	_, running := h.deviceState(r, d)
	var notice, failure string
	if err := h.d.Prober.Refresh(r.Context(), d.ID); err != nil {
		failure = h.refreshProblem(r, err)
	} else {
		notice = "Identity refreshed."
		if fresh, gerr := h.d.Inventory.Get(r.Context(), d.ID); gerr == nil {
			d = fresh
		} else {
			h.log.WarnContext(r.Context(), "probe: reload device", "err", gerr)
		}
	}
	var facts snapFacts
	if list, err := h.d.Backups.List(r.Context(), d.ID); err == nil {
		facts = h.snapFactsOf(r.Context(), list)
	}
	c := h.identity(v, d, running != nil, facts)
	c.Notice, c.Error = notice, failure
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := identityCard(c).Render(r.Context(), w); err != nil {
		h.log.WarnContext(r.Context(), "probe: render", "err", err)
	}
}

// stateHistory is GET /devices/{id}/history: every recorded state change of the device, newest
// first, paged like the other long tables. The overview shows only the newest few.
func (h *handler) stateHistory(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	if h.d.History == nil {
		h.notFound(w, r)
		return
	}
	all, err := h.d.History.ListHealthEvents(r.Context(), d.ID, 0)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v := h.view(r, d.Name+": state changes", "devices", nil)
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	def := savedPageSize(h.userPrefs(r))
	size := pageSize(q, def)
	p := historyView{Base: v.Base, Tabs: h.tabsOf(r, v, d, "overview")}
	p.Pager = newPager(v.Base+devPath(d.ID, "/history"), url.Values{}, page, size, def, len(all), "changes")
	lo := (p.Pager.Page - 1) * size
	if hi := min(lo+size, len(all)); lo < hi {
		p.Events = all[lo:hi]
	}
	h.headPage(w, r, http.StatusOK, "device-history", v, d, deviceHistoryPage(p))
}
