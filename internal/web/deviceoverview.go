package web

import (
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/health"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// What the Overview of one device is built from. The builders below are pure (no handler, no
// clock of their own) so the table tests can cover every device shape of #145.

// overviewIn is the data of the tiles, the relay box and the connection card.
type overviewIn struct {
	Dev    model.Device
	Health model.Health
	Tele   health.Telemetry
	Facts  snapFacts
	Policy *policyCard
	Now    time.Time
}

// wifi signal bar: -92 dBm is empty, -40 dBm full (the prototype's scale). Green above -60 dBm,
// amber down to -75 dBm, red below.
func signalBar(dbm int) (pct int, tone ui.Tone) {
	pct = min(100, max(6, (dbm+92)*19/10))
	switch {
	case dbm > -60:
		tone = ui.ToneOK
	case dbm >= -75:
		tone = ui.ToneWarn
	default:
		tone = ui.ToneDanger
	}
	return pct, tone
}

// meterChannels splits the telemetry of the meter the way Tasmota sends it: the keyed entries
// "1", "2", ... are the channels of an array (in channel order), and the entry "" holds what the
// device reports once for all channels, for example the summed counters of a multi-channel meter
// or everything of a single meter. A counter is either in the channels or in "", never both, so
// sums run over all of them.
func meterChannels(t health.Telemetry) (channels, all []health.EnergyChannel) {
	keys := make([]string, 0, len(t.Energy))
	for k := range t.Energy {
		if k != "" {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, _ := strconv.Atoi(keys[i])
		b, _ := strconv.Atoi(keys[j])
		return a < b
	})
	for _, k := range keys {
		channels = append(channels, t.Energy[k])
	}
	all = channels
	if c, ok := t.Energy[""]; ok {
		all = append(append([]health.EnergyChannel(nil), channels...), c)
	}
	return channels, all
}

// sumOf adds the readings of a channel field; ok is false when no channel reported it.
func sumOf(ch []health.EnergyChannel, f func(health.EnergyChannel) *float64) (sum float64, ok bool) {
	for _, c := range ch {
		if p := f(c); p != nil {
			sum += *p
			ok = true
		}
	}
	return sum, ok
}

func fmtNum(v float64, ok bool, prec int) string {
	if !ok {
		return "—"
	}
	return strconv.FormatFloat(v, 'f', prec, 64)
}

// totalText is a lifetime counter: one decimal while small, whole kWh from 100 on.
func totalText(v float64, ok bool) string {
	if v >= 100 {
		return fmtNum(v, ok, 0)
	}
	return fmtNum(v, ok, 1)
}

// powerTiles are the Power and Energy today tiles: only for a device with an energy meter. With
// several channels the power is their sum and the tile says so.
func powerTiles(t health.Telemetry) []tile {
	channels, ch := meterChannels(t)
	if len(ch) == 0 {
		return nil
	}
	w, wok := sumOf(ch, func(c health.EnergyChannel) *float64 { return c.Power })
	td, tok := sumOf(ch, func(c health.EnergyChannel) *float64 { return c.Today })
	y, yok := sumOf(ch, func(c health.EnergyChannel) *float64 { return c.Yesterday })
	tot, totok := sumOf(ch, func(c health.EnergyChannel) *float64 { return c.Total })
	power := tile{Key: "power", Label: "Power", Value: "—", Sub: "now", Mask: true}
	if wok {
		power.Value = fmtNum(w, true, 0) + " W"
	}
	if len(channels) > 1 {
		power.Sub = "all channels"
	}
	energy := tile{Key: "energy", Label: "Energy today", Value: "—", Mask: true,
		Sub: "Yesterday " + fmtNum(y, yok, 2) + " · Total " + totalText(tot, totok)}
	if tok {
		energy.Value = fmtNum(td, true, 2) + " kWh"
	}
	return []tile{power, energy}
}

// linkTile is the Wi-Fi signal tile, or the Link tile of a wired device (no signal, Wi-Fi off).
func linkTile(in overviewIn) tile {
	if in.Facts.Wired {
		return tile{Key: "link", Label: "Link", Value: "Ethernet", Sub: "Wi-Fi off"}
	}
	hl, t, f := in.Health, in.Tele, in.Facts
	dbm, has := t.SignalDBm, t.HasSignal
	if !has && hl.RSSI > 0 {
		dbm, has = hl.RSSI/2-100, true // Tasmota maps -100..-50 dBm to 0..100 %
	}
	tl := tile{Key: "wifi", Label: "Wi-Fi signal", Value: "—", Mask: true}
	if has {
		tl.Value = strconv.Itoa(dbm) + " dBm"
		tl.Bar, tl.BarTone = signalBar(dbm)
	}
	var sub []string
	if hl.RSSI > 0 {
		sub = append(sub, strconv.Itoa(hl.RSSI)+" %")
	}
	if f.SSID != "" {
		sub = append(sub, f.SSID)
	}
	if f.Channel > 0 {
		sub = append(sub, "ch "+strconv.Itoa(f.Channel))
	}
	tl.Sub = strings.Join(sub, " · ")
	return tl
}

// uptimeTile is the uptime with the last restart as its sub line. An offline device has no
// uptime, only the restart it last reported.
func uptimeTile(hl model.Health, now time.Time) tile {
	tl := tile{Key: "uptime", Label: "Uptime", Value: "—", Mask: true}
	if hl.State != model.HealthOffline && hl.UptimeSec > 0 {
		tl.Value = humanDuration(time.Duration(hl.UptimeSec) * time.Second)
	}
	if at := restartAt(hl); !at.IsZero() {
		tl.SubPrefix, tl.At, tl.AtText = "Last restart ", at, ui.StampDayText(now, at)
	}
	return tl
}

// reach is how the two channels to the device answer now.
type reach struct {
	MQTTOn   bool // the device reports to our broker (or was heard on it)
	MQTTOK   bool
	LWTOff   bool
	HTTPOK   bool
	HTTPWord string
}

// reachOf words the channels; the Reachability tile and nothing else reads it.
func reachOf(d model.Device, hl model.Health) reach {
	r := reach{MQTTOn: d.MQTT == model.MQTTOurs || hl.LWT != "" || !hl.SeenMQTT.IsZero(), HTTPWord: "Not polled yet"}
	r.MQTTOK = hl.LWT == "Online" || hl.LWT == "" && !hl.SeenMQTT.IsZero() && hl.State == model.HealthOnline
	r.LWTOff = hl.LWT == "Offline"
	switch {
	case hl.State == model.HealthAuthFailed:
		r.HTTPWord = "Refuses the stored credentials"
	case hl.State == model.HealthOffline || hl.State == model.HealthUnknown && !hl.LastSeen.IsZero():
		r.HTTPWord = "No response"
	case !hl.SeenHTTP.IsZero():
		r.HTTPWord, r.HTTPOK = "Reachable", true
	}
	return r
}

// reachTile is Reachability: "MQTT · HTTP", "HTTP" with "MQTT not configured" for a device we do
// not watch over MQTT, "Offline" when it does not answer.
func reachTile(d model.Device, hl model.Health) tile {
	r := reachOf(d, hl)
	tl := tile{Key: "reach", Label: "Reachability"}
	switch {
	case hl.State == model.HealthOffline:
		tl.Value, tl.Tone = "Offline", ui.ToneNeutral
		var sub []string
		switch {
		case r.LWTOff:
			sub = append(sub, "MQTT LWT offline")
		case r.MQTTOn:
			sub = append(sub, "MQTT not connected")
		}
		tl.Sub = strings.Join(append(sub, "HTTP "+strings.ToLower(r.HTTPWord)), " · ")
		if r.HTTPWord == "No response" {
			tl.Sub = strings.Replace(tl.Sub, "HTTP no response", "HTTP no answer", 1)
		}
	case hl.State == model.HealthAuthFailed:
		tl.Value, tl.Tone, tl.Sub = "Login refused", ui.ToneDanger, r.HTTPWord
	case hl.State == model.HealthUnknown:
		tl.Value, tl.Tone, tl.Sub = "Unknown", ui.ToneNeutral, "The device has not answered yet"
	case r.MQTTOK && r.HTTPOK:
		tl.Value, tl.Tone, tl.Sub = "MQTT · HTTP", ui.ToneOK, "Connected · Reachable"
	case r.MQTTOK:
		tl.Value, tl.Tone, tl.Sub = "MQTT", ui.ToneOK, "Connected · HTTP not polled yet"
	case r.MQTTOn:
		tl.Value, tl.Tone, tl.Sub = "HTTP", ui.ToneWarn, "Reachable · MQTT not connected"
	default:
		tl.Value, tl.Tone, tl.Sub = "HTTP", ui.ToneOK, "Reachable · MQTT not configured"
	}
	return tl
}

// policyTile is "n / m" rules matched: green when all match, amber naming the first failing
// setting. Nothing without a policy or without rules.
func policyTile(c *policyCard, off bool) *tile {
	if c == nil || c.Rules == 0 {
		return nil
	}
	tl := tile{Key: "policy", Label: "Policy", Value: fmt.Sprintf("%d / %d", max(c.Rules-len(c.Deviations), 0), c.Rules), Tone: ui.ToneOK}
	switch {
	case len(c.Deviations) > 0:
		d := c.Deviations[0]
		tl.Tone, tl.Sub = ui.ToneWarn, d.Have+" → "+d.Want
		if n := len(c.Deviations) - 1; n > 0 {
			tl.Sub += " (+" + strconv.Itoa(n) + " more)"
		}
		tl.Fix = c.CanFix
	case c.Unknown > 0:
		tl.Tone, tl.Sub = ui.ToneNeutral, plural(c.Unknown, "rule")+" not read yet"
	case off:
		tl.Sub = "Last check before going offline"
	default:
		tl.Sub = "Matches all rules"
	}
	return &tl
}

// tilesOf is the tile row in its fixed order; tiles that do not apply are left out.
func tilesOf(in overviewIn) []tile {
	tiles := powerTiles(in.Tele)
	tiles = append(tiles, linkTile(in), uptimeTile(in.Health, in.Now), reachTile(in.Dev, in.Health))
	if p := policyTile(in.Policy, in.Health.State == model.HealthOffline); p != nil {
		tiles = append(tiles, *p)
	}
	return tiles
}

// relayName is the name of relay n: the device's FriendlyName for it, or "Relay n" while it still
// has Tasmota's default ("Tasmota" for the first, "Tasmota2", "Tasmota3"... for the others) (D2).
func relayName(names []string, n int) string {
	if n >= 1 && n <= len(names) {
		name := strings.TrimSpace(names[n-1])
		if rest, ok := strings.CutPrefix(name, "Tasmota"); name != "" && (!ok || strings.Trim(rest, "0123456789") != "") {
			return name
		}
	}
	return "Relay " + strconv.Itoa(n)
}

// relayBoxOf is the relay box: every relay with its switch, and in the header how many are on
// and how the energy is metered. Nil for a device without relays. Watts show per relay only when
// the device meters each channel (as many channels as relays), else the state.
func relayBoxOf(hl model.Health, t health.Telemetry, names []string, enabled bool) *relayBox {
	rs := relayStates(hl)
	if len(rs) == 0 {
		return nil
	}
	ch, all := meterChannels(t)
	perChannel := len(rs) > 1 && len(ch) == len(rs)
	box := &relayBox{Enabled: enabled}
	on := 0
	for i, r := range rs {
		row := relayRow{N: r.N, Name: relayName(names, r.N), Cmd: "POWER", On: r.State == "ON", Action: "ON"}
		if len(rs) > 1 {
			row.Cmd += strconv.Itoa(r.N)
		}
		state := "Unknown"
		switch r.State {
		case "ON":
			state, row.Action = "On", "OFF"
			on++
		case "OFF":
			state = "Off"
		}
		row.Line = state
		if perChannel {
			if p := ch[i].Power; p != nil {
				row.Line = fmtNum(*p, true, 0) + " W"
			}
		}
		row.Aria = row.Name + " is " + state + ": switch it " + label(row.Action)
		box.Rows = append(box.Rows, row)
	}
	var meter string
	switch {
	case len(all) == 0:
		meter = "no energy meter"
	case len(rs) == 1:
		meter = "metered"
	case perChannel:
		meter = "metered per channel"
	case len(ch) <= 1:
		meter = "one meter for all"
	default:
		meter = plural(len(ch), "meter channel")
	}
	if len(rs) > 1 {
		box.Right = fmt.Sprintf("%d of %d on · %s", on, len(rs), meter)
	} else {
		box.Right = meter
	}
	return box
}

// sensorBoxOf is the Sensors box of a device without relays: the numeric readings of its MQTT
// telemetry (D5: nothing is polled for them). Without readings it says why in one line.
func sensorBoxOf(in overviewIn) *sensorBox {
	box := &sensorBox{Right: "no relay"}
	for _, r := range in.Tele.Sensors {
		v := strconv.FormatFloat(math.Round(r.Value*100)/100, 'f', -1, 64)
		if r.Unit != "" {
			v += " " + r.Unit
		}
		box.Cells = append(box.Cells, sensorCell{Label: r.Name + " · " + r.Sensor, Value: v})
	}
	switch {
	case len(box.Cells) > 0:
	case !reachOf(in.Dev, in.Health).MQTTOn:
		box.Note = "Sensor readings come from MQTT telemetry. This device does not report to TasmoFleet's MQTT broker."
	default:
		box.Note = "No sensor readings yet."
	}
	return box
}

// connOf is the Connection card: how TasmoFleet reaches the device, then how the device is
// connected (Wi-Fi or Ethernet) and its broker.
func connOf(in overviewIn) connCard {
	d, f := in.Dev, in.Facts
	addr := d.Address
	if d.Port != 0 && d.Port != 80 {
		addr = net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	}
	// "Address" is where TasmoFleet reaches the device. The IP the device reports is a second row
	// only when it says something else (the address is a DNS name, or the device moved).
	rows := []ui.KVRow{{Key: "Address", Value: addr, Mono: true, Mask: true}}
	if f.IP != "" && f.IP != d.Address {
		rows = append(rows, ui.KVRow{Key: "IP address", Value: f.IP, Mono: true, Mask: true})
	}
	rows = append(rows, ui.KVRow{Key: "Gateway", Value: orUnknown(f.Gateway), Mono: true, Mask: true})
	c := connCard{Title: "Connection · Wi-Fi"}
	if f.Wired {
		c.Title = "Connection · Ethernet"
		rows = append(rows,
			ui.KVRow{Key: "Subnet mask", Value: orUnknown(f.Subnet), Mono: true, Mask: true},
			ui.KVRow{Key: "DNS", Value: orUnknown(f.DNS), Mono: true, Mask: true},
			ui.KVRow{Key: "Ethernet MAC", Value: orUnknown(macShown(d, true)), Mono: true, Mask: true})
	} else {
		channel := ""
		if f.Channel > 0 {
			band := "2.4 GHz"
			if f.Channel > 14 {
				band = "5 GHz"
			}
			channel = fmt.Sprintf("%d (%s)", f.Channel, band)
		}
		rows = append(rows,
			ui.KVRow{Key: "SSID", Value: orUnknown(f.SSID), Mono: true, Mask: true},
			ui.KVRow{Key: "BSSID", Value: orUnknown(f.BSSID), Mono: true, Mask: true},
			ui.KVRow{Key: "Channel", Value: orUnknown(channel), Mono: true, Mask: true})
	}
	broker := "not configured"
	if d.MQTTHost != "" {
		port := d.MQTTPort
		if port == 0 {
			port = 1883
		}
		broker = net.JoinHostPort(d.MQTTHost, strconv.Itoa(port))
	}
	c.Rows = append(rows, ui.KVRow{Key: "MQTT broker", Value: broker, Mono: true, Soft: true})
	return c
}

// mergeActivity merges the state changes and the transactions of a device, newest first. A tie
// keeps the state change first.
func mergeActivity(events []store.HealthEvent, jobs []model.Job) []devActRow {
	rows := make([]devActRow, 0, len(events)+len(jobs))
	for i := range events {
		rows = append(rows, devActRow{At: events[i].At, Event: &events[i]})
	}
	for i := range jobs {
		rows = append(rows, devActRow{At: jobs[i].UpdatedAt, Job: &jobs[i]})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].At.After(rows[j].At) })
	return rows
}

// devActRows reads the state changes and transactions of a device, merged; limit 0 reads all.
// The jobs come back too: the overview looks in them for an operation that holds the device.
func (h *handler) devActRows(r *http.Request, id model.DeviceID, limit int) ([]devActRow, []model.Job, error) {
	var events []store.HealthEvent
	var jobs []model.Job
	var err error
	if h.d.History != nil {
		if events, err = h.d.History.ListHealthEvents(r.Context(), id, limit); err != nil {
			return nil, nil, err
		}
	}
	if h.d.Jobs != nil {
		if jobs, err = h.d.Jobs.ListJobs(r.Context(), store.JobFilter{DeviceID: id, Limit: limit}); err != nil {
			return nil, nil, err
		}
	}
	rows := mergeActivity(events, jobs)
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, jobs, nil
}

// telemetryOf is the device's cached telemetry (empty when the tracker has none).
func (h *handler) telemetryOf(id model.DeviceID) health.Telemetry {
	if tl, ok := h.d.Health.(interface {
		Telemetry() map[model.DeviceID]health.Telemetry
	}); ok {
		return tl.Telemetry()[id]
	}
	return health.Telemetry{}
}

// overviewOf builds everything the overview and its live refresh show. Errors are a failed read
// of the activity; the caller answers them.
func (h *handler) overviewOf(r *http.Request, v view, d model.Device, just *relayState) (deviceOverview, error) {
	ctx := r.Context()
	hl := h.d.Health.Get(d.ID)
	if just != nil {
		hl.Power = withRelay(hl.Power, *just)
	}
	if hl.State == model.HealthOffline {
		hl.LastSeen = lastContact(d, hl) // the "seen ..." in the header, as on the list
	}
	if hl.State == "" {
		hl.State = model.HealthUnknown
	}
	p := deviceOverview{Base: v.Base, CSRF: v.CSRF}
	rows, jobs, err := h.devActRows(r, d.ID, overviewRows)
	if err != nil {
		return p, err
	}
	p.Activity = devActivity{Base: v.Base, Rows: rows, All: v.Base + devPath(d.ID, "/history")}
	for _, j := range jobs {
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
	tf, tp, tpOK := h.transportOf(ctx, d.ID)
	p.Tabs = h.headWith(v, d, hl, running, "overview", tp, tpOK)
	if p.Updating == nil && hl.LastSeen.IsZero() && d.Identity.DetectedAt.IsZero() {
		p.NotYet = "This device has not answered yet"
		if !d.CreatedAt.IsZero() {
			p.NotYet += " (added " + sinceText(d.CreatedAt) + ")"
		}
		p.NotYet += ". Check that it is powered and that its address (" + d.Address + ") is right."
	}
	facts := h.factsOf(ctx, d)
	tele := h.telemetryOf(d.ID)
	in := overviewIn{Dev: d, Health: hl, Tele: tele, Facts: facts, Policy: h.policyOf(r, v, d.ID), Now: h.d.Clock.Now()}
	off := p.Tabs.offline() // an update silences the device on purpose: that is not "offline"
	p.Live = liveView{ID: d.ID, Base: v.Base, CSRF: v.CSRF, Off: off, Tiles: tilesOf(in),
		Box: relayBoxOf(hl, tele, d.RelayNames, h.d.DeviceCmd != nil && !off && p.Updating == nil)}
	if p.Live.Box == nil {
		p.Live.Sensors = sensorBoxOf(in)
	}
	p.Ident = h.identity(v, d, p.Updating != nil, off, facts)
	p.Conn = connOf(in)
	if tpOK {
		p.Paths = pathCardOf(v, d, tf, tp, p.Tabs)
	}
	return p, nil
}

func (h *handler) deviceDetail(w http.ResponseWriter, r *http.Request) {
	seq := h.eventSeq() // before the device and its state are read (#160)
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	v := h.view(r, d.Name, "devices", nil)
	v.EventSeq = seq
	p, err := h.overviewOf(r, v, d, nil)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v.Live = true
	h.headPage(w, r, http.StatusOK, "device", v, d, deviceOverviewPage(p))
}

// powerLive answers a relay switch of the overview that came by htmx: the live region as it is
// after the command, or in place the reason it failed (status 200, so that htmx swaps it).
func (h *handler) powerLive(w http.ResponseWriter, r *http.Request, d model.Device, just *relayState, failure string) {
	v := h.view(r, "", "", nil)
	p, err := h.overviewOf(r, v, d, just)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if p.Live.Box != nil {
		p.Live.Box.Err = failure
	}
	h.component(w, r, http.StatusOK, deviceLiveParts(p))
}

// deviceLive is GET /devices/{id}/live: the tiles, the relay box and the Activity box, with the
// header and the offline banner out of band. The page asks for it when the device changes
// (#126); it never carries the Identity card.
func (h *handler) deviceLive(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	v := h.view(r, "", "", nil)
	p, err := h.overviewOf(r, v, d, nil)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.component(w, r, http.StatusOK, deviceLiveParts(p))
}

// stateHistory is GET /devices/{id}/history: every state change and transaction of the device,
// merged, newest first, paged like the other long tables. The overview shows only the newest few.
func (h *handler) stateHistory(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	if h.d.History == nil && h.d.Jobs == nil {
		h.notFound(w, r)
		return
	}
	all, _, err := h.devActRows(r, d.ID, 0)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v := h.view(r, d.Name+": activity", "devices", nil)
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	def := savedPageSize(h.userPrefs(r))
	size := pageSize(q, def)
	p := historyView{Base: v.Base, Tabs: h.tabsOf(r, v, d, "overview")}
	p.Pager = newPager(v.Base+devPath(d.ID, "/history"), url.Values{}, page, size, def, len(all), "entries")
	lo := (p.Pager.Page - 1) * size
	if hi := min(lo+size, len(all)); lo < hi {
		p.Rows = all[lo:hi]
	}
	h.headPage(w, r, http.StatusOK, "device-history", v, d, deviceHistoryPage(p))
}

// withRelay is the power map with one relay at the state the device just reported, ahead of the
// health update that follows over SSE. The map is copied: the tracker's own stays as it is.
func withRelay(power map[string]string, r relayState) map[string]string {
	out := make(map[string]string, len(power)+1)
	for k, v := range power {
		out[k] = v
	}
	key := "POWER" + strconv.Itoa(r.N)
	if _, single := out["POWER"]; single && r.N == 1 {
		key = "POWER"
	}
	out[key] = r.State
	return out
}
