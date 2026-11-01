package web

import (
	"bufio"
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/health"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/transport"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// Tests of the reworked device Overview (#145), its live refresh (#126, #160) and the shapes of
// the mockups in docs/design/device-overview.

func meter(w, today, yest, total float64) health.EnergyChannel {
	return health.EnergyChannel{Power: f64(w), Today: f64(today), Yesterday: f64(yest), Total: f64(total)}
}

func tileKeys(tiles []tile) string {
	var keys []string
	for _, t := range tiles {
		keys = append(keys, t.Key)
	}
	return strings.Join(keys, ",")
}

var now0 = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

// onlineHealth is a device that answers on both channels.
func onlineHealth(power map[string]string) model.Health {
	return model.Health{State: model.HealthOnline, LastSeen: now0, SeenHTTP: now0, SeenMQTT: now0, LWT: "Online", UptimeSec: 3*86400 + 2*3600, RSSI: 58, Power: power}
}

// Why: the tile row is one row of equal tiles and never has holes: a tile that does not apply is
// not built. Power and Energy exist only with a meter, Wi-Fi turns into Link on Ethernet, and
// Policy only when there are rules. The order is fixed (the eight mockup shapes of #145).
func TestTilesCoverTheDeviceShapes(t *testing.T) {
	mqtt := model.Device{MQTT: model.MQTTOurs}
	okPolicy := &policyCard{Rules: 7}
	tests := []struct {
		name string
		in   overviewIn
		want string
	}{
		{"1 relay, meter, Wi-Fi", overviewIn{Dev: mqtt, Health: onlineHealth(map[string]string{"POWER": "ON"}),
			Tele: health.Telemetry{Energy: map[string]health.EnergyChannel{"": meter(133, 0.45, 0, 8.5)}}, Policy: okPolicy}, "power,energy,wifi,uptime,reach,policy"},
		{"1 relay, no meter, Wi-Fi", overviewIn{Dev: mqtt, Health: onlineHealth(map[string]string{"POWER": "ON"}), Policy: okPolicy}, "wifi,uptime,reach,policy"},
		{"4 relays over Ethernet, no meter", overviewIn{Dev: mqtt, Health: onlineHealth(nil), Facts: snapFacts{Wired: true}, Policy: okPolicy}, "link,uptime,reach,policy"},
		{"no policy configured", overviewIn{Dev: mqtt, Health: onlineHealth(nil)}, "wifi,uptime,reach"},
		{"policy without rules", overviewIn{Dev: mqtt, Health: onlineHealth(nil), Policy: &policyCard{}}, "wifi,uptime,reach"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tileKeys(tilesOf(tc.in)); got != tc.want {
				t.Fatalf("tiles %s, want %s", got, tc.want)
			}
		})
	}
}

// Why: several channels report as arrays; the Power tile adds them up and says so, and the Energy
// tile adds the counters, with yesterday and the lifetime total as its subline.
func TestPowerTilesSumTheChannels(t *testing.T) {
	tl := powerTiles(health.Telemetry{Energy: map[string]health.EnergyChannel{
		"1": meter(42, 0.5, 0.4, 100), "2": meter(96, 0.7, 0.8, 200), "3": meter(20, 0.2, 0.2, 100), "4": meter(0, 0, 0.64, 12)}})
	if len(tl) != 2 || tl[0].Value != "158 W" || tl[0].Sub != "all channels" {
		t.Fatalf("power: %+v", tl)
	}
	if tl[1].Value != "1.40 kWh" || tl[1].Sub != "Yesterday 2.04 · Total 412" {
		t.Fatalf("energy: %+v", tl[1])
	}
	one := powerTiles(health.Telemetry{Energy: map[string]health.EnergyChannel{"": meter(133, 0.45, 0, 8.5)}})
	if one[0].Value != "133 W" || one[0].Sub != "now" || one[1].Sub != "Yesterday 0.00 · Total 8.5" {
		t.Fatalf("single meter: %+v", one)
	}
	if powerTiles(health.Telemetry{}) != nil {
		t.Fatal("a device without a meter has no Power tile")
	}
}

// Why: the signal bar answers "can I trust this link": green above -60 dBm, amber down to
// -75, red below (the weak Wi-Fi of mockup 4 and 8).
func TestWifiTileBarToneFollowsTheSignal(t *testing.T) {
	for _, tc := range []struct {
		dbm  int
		tone ui.Tone
	}{{-48, ui.ToneOK}, {-59, ui.ToneOK}, {-60, ui.ToneWarn}, {-71, ui.ToneWarn}, {-75, ui.ToneWarn}, {-76, ui.ToneDanger}, {-90, ui.ToneDanger}} {
		in := overviewIn{Health: model.Health{RSSI: 52}, Tele: health.Telemetry{SignalDBm: tc.dbm, HasSignal: true}, Facts: snapFacts{SSID: "IoT", Channel: 11}}
		tl := linkTile(in)
		if tl.BarTone != tc.tone || tl.Bar == 0 {
			t.Errorf("%d dBm: tone %q bar %d, want %q", tc.dbm, tl.BarTone, tl.Bar, tc.tone)
		}
		if tl.Value != strconvDBm(tc.dbm) || tl.Sub != "52 % · IoT · ch 11" {
			t.Errorf("%d dBm: value %q sub %q", tc.dbm, tl.Value, tl.Sub)
		}
	}
	wired := linkTile(overviewIn{Facts: snapFacts{Wired: true}})
	if wired.Key != "link" || wired.Value != "Ethernet" || wired.Sub != "Wi-Fi off" || wired.Bar != 0 {
		t.Errorf("wired: %+v", wired)
	}
}

func strconvDBm(n int) string { return strings.TrimSpace(strings.Join([]string{itoa(n), "dBm"}, " ")) }

// Why: "Reachability" tells HTTP-only devices from devices on our broker, and an offline device
// says Offline instead of looking half-alive ("MQTT not configured" must not read as a fault).
func TestReachTileSaysWhichChannelsAnswer(t *testing.T) {
	ours := model.Device{MQTT: model.MQTTOurs}
	tests := []struct {
		name       string
		d          model.Device
		hl         model.Health
		http       transport.HTTPState
		value, sub string
		tone       ui.Tone
	}{
		{"both", ours, onlineHealth(nil), httpFresh, "MQTT · HTTP", "Connected · Reachable", ui.ToneOK},
		{"HTTP only", model.Device{}, model.Health{State: model.HealthOnline, LastSeen: now0, SeenHTTP: now0}, httpFresh, "HTTP", "Reachable · MQTT not configured", ui.ToneOK},
		{"on our broker but silent", ours, model.Health{State: model.HealthOnline, LastSeen: now0, SeenHTTP: now0}, httpFresh, "HTTP", "Reachable · MQTT not connected", ui.ToneWarn},
		{"offline", ours, model.Health{State: model.HealthOffline, LastSeen: now0, LWT: "Offline"}, transport.HTTPState{}, "Offline", "MQTT LWT offline · HTTP no answer", ui.ToneNeutral},
		{"never answered", model.Device{}, model.Health{State: model.HealthUnknown}, transport.HTTPState{}, "Unknown", "The device has not answered yet", ui.ToneNeutral},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := reachTile(tc.d, tc.hl, tc.http, now0)
			if got.Value != tc.value || got.Sub != tc.sub || got.Tone != tc.tone {
				t.Fatalf("got %q / %q / %q", got.Value, got.Sub, got.Tone)
			}
		})
	}
}

// Why: Policy is "n / m": green when everything matches, amber naming the first failing
// setting (not just a count), and only an admin gets "Make compliant…".
func TestPolicyTileNamesTheFirstFailingSetting(t *testing.T) {
	if tl := policyTile(&policyCard{Rules: 7}, false); tl.Value != "7 / 7" || tl.Tone != ui.ToneOK || tl.Sub != "Matches all rules" || tl.Fix {
		t.Fatalf("compliant: %+v", tl)
	}
	bad := &policyCard{Rules: 7, CanFix: true, Deviations: []deviation{{Rule: "Telemetry", Have: "TelePeriod 60", Want: "300"}, {Rule: "x", Have: "SetOption0 1", Want: "0"}}}
	if tl := policyTile(bad, false); tl.Value != "5 / 7" || tl.Tone != ui.ToneWarn || tl.Sub != "TelePeriod 60 → 300 (+1 more)" || !tl.Fix {
		t.Fatalf("deviating: %+v", tl)
	}
	bad.CanFix = false
	if tl := policyTile(bad, false); tl.Fix {
		t.Fatal("an operator gets no fix")
	}
	if tl := policyTile(&policyCard{Rules: 7}, true); tl.Sub != "Last check before going offline" {
		t.Fatalf("offline: %+v", tl)
	}
	if policyTile(nil, false) != nil {
		t.Fatal("no policy, no tile")
	}
}

// Why: the relay box is the same box for one relay and four. Watts per relay only when the
// device meters each channel (as many channels as relays); otherwise the state, so that one
// shared meter is never shown as a reading of one relay.
func TestRelayBoxMeteringLabelsAndPerChannelWatts(t *testing.T) {
	four := map[string]string{"POWER1": "ON", "POWER2": "ON", "POWER3": "ON", "POWER4": "OFF"}
	perChannel := health.Telemetry{Energy: map[string]health.EnergyChannel{"1": meter(42, 0, 0, 0), "2": meter(96, 0, 0, 0), "3": meter(20, 0, 0, 0), "4": meter(0, 0, 0, 0)}}
	box := relayBoxOf(model.Health{Power: four}, perChannel, nil, true)
	if box.Right != "3 of 4 on · metered per channel" || box.Rows[0].Line != "42 W" || box.Rows[3].Line != "0 W" || box.Rows[1].Cmd != "POWER2" {
		t.Fatalf("per channel: %+v", box)
	}
	two := map[string]string{"POWER1": "ON", "POWER2": "OFF"}
	shared := relayBoxOf(model.Health{Power: two}, health.Telemetry{Energy: map[string]health.EnergyChannel{"": meter(60, 0, 0, 0)}}, nil, true)
	if shared.Right != "1 of 2 on · one meter for all" || shared.Rows[0].Line != "On" || shared.Rows[1].Line != "Off" {
		t.Fatalf("one meter for two relays: %+v", shared)
	}
	// a meter with 3 channels on 2 relays is not "per channel": counts differ
	if odd := relayBoxOf(model.Health{Power: two}, health.Telemetry{Energy: map[string]health.EnergyChannel{"1": meter(1, 0, 0, 0), "2": meter(2, 0, 0, 0), "3": meter(3, 0, 0, 0)}}, nil, true); odd.Rows[0].Line != "On" {
		t.Fatalf("channel count differs from relay count: %+v", odd)
	}
	none := relayBoxOf(model.Health{Power: four}, health.Telemetry{}, nil, true)
	if none.Right != "3 of 4 on · no energy meter" || none.Rows[0].Line != "On" {
		t.Fatalf("no meter: %+v", none)
	}
	one := relayBoxOf(model.Health{Power: map[string]string{"POWER": "ON"}}, health.Telemetry{Energy: map[string]health.EnergyChannel{"": meter(133, 0, 0, 0)}}, nil, true)
	if one.Right != "metered" || one.Rows[0].Cmd != "POWER" || one.Rows[0].Line != "On" {
		t.Fatalf("one relay: %+v", one)
	}
	if relayBoxOf(model.Health{}, health.Telemetry{}, nil, true) != nil {
		t.Fatal("a device without relays has no relay box")
	}
}

// Why: Connection shows how the device is connected: SSID/BSSID/channel on Wi-Fi, the Ethernet MAC
// on a cable, and says so when no broker is configured (HTTP-only devices, mockup 7).
func TestConnectionCardFollowsTheLink(t *testing.T) {
	wifi := connOf(overviewIn{Dev: model.Device{Address: "172.27.13.26", MQTTHost: "mqtt.home.internal"}, Facts: snapFacts{Gateway: "172.27.13.1", SSID: "IoT", BSSID: "8C:30", Channel: 11}})
	if wifi.Title != "Connection · Wi-Fi" || kvKeys(wifi.Rows) != "Address,Gateway,SSID,BSSID,Channel,MQTT broker" || wifi.Rows[5].Value != "mqtt.home.internal:1883" || wifi.Rows[4].Value != "11 (2.4 GHz)" {
		t.Fatalf("wifi: %+v", wifi)
	}
	eth := connOf(overviewIn{Dev: model.Device{Address: "172.27.13.44", MAC: "aa", MACs: []string{"aa", "bb"}}, Facts: snapFacts{Wired: true, Gateway: "172.27.13.1", Subnet: "255.255.255.0", DNS: "172.27.13.1, 9.9.9.9"}})
	if eth.Title != "Connection · Ethernet" || kvKeys(eth.Rows) != "Address,Gateway,Subnet mask,DNS,Ethernet MAC,MQTT broker" || eth.Rows[2].Value != "255.255.255.0" || eth.Rows[3].Value != "172.27.13.1, 9.9.9.9" || eth.Rows[4].Value != "bb" || eth.Rows[5].Value != "not configured" {
		t.Fatalf("ethernet: %+v", eth)
	}
}

func kvKeys(rows []ui.KVRow) string {
	var k []string
	for _, r := range rows {
		k = append(k, r.Key)
	}
	return strings.Join(k, ",")
}

// Why: Identity is the static facts in a fixed order; "Chip" is gone (the family is in the header's
// meta line). The device's own fields (StatusFWR, #289) follow the card's own rows, TasmoFleet's
// reading comes last and carries the flash size.
func TestIdentityRowsFollowTheMockup(t *testing.T) {
	e := setup(t)
	d := e.inv.devs[1]
	d.Identity.Hardware, d.Identity.FlashSizeKB = "ESP8266EX", 2048
	e.inv.devs[1] = d
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	i := strings.Index(body, `id="dc-ident"`)
	j := strings.Index(body, `id="dc-conn"`)
	ident := body[i:j]
	if strings.Contains(ident, "<dt>Chip</dt>") || !strings.Contains(ident, "2 MB") {
		t.Errorf("identity:\n%s", ident)
	}
	last, order := -1, []string{"<dt>Device name", "<dt>Hostname", "<dt>MQTT topic", "<dt>MAC", "<dt>Hardware", "TasmoFleet&#39;s reading", "<dt>Variant", "<dt>Flash size"}
	for _, k := range order {
		at := strings.Index(ident, k)
		if at < last {
			t.Fatalf("%s is out of order", k)
		}
		last = at
	}
}

// Why (#126): the overview must follow the device without a reload. The live region asks for
// itself on the events of this device only (the stream is shared by every page, so the event
// names carry the id) and on resync (#160).
func TestOverviewLiveRegionListensToThisDeviceOnly(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/devices/1", "operator", nil)
	for _, want := range []string{`hx-get="/tf/devices/1/live?act=`, `sse:devstate-1 throttle:2s`, `sse:devjob-1 throttle:2s`, `sse:power-1 throttle:10s`, `sse:uptime-1 throttle:10s`, `sse:resync`} {
		if !strings.Contains(body, want) {
			t.Errorf("live region lacks %q", want)
		}
	}
	if strings.Contains(body, "devstate-2") || strings.Contains(body, "sse:state") {
		t.Error("the region must not listen to other devices' events")
	}
}

// Why (#126): the live answer carries the region, the Activity box, the header and the banner slot,
// and never the Identity card (it holds answers and notices a swap would erase).
func TestLiveAnswerCarriesTheRegionsButNotTheIdentity(t *testing.T) {
	e := setupWith(t, func(d *Deps) {
		d.Health.(fakeHealth)[1] = model.Health{DeviceID: 1, State: model.HealthOffline, LastSeen: now0, Power: map[string]string{"POWER": "ON"}}
	})
	resp, body := e.do(t, "GET", "/devices/1/live", "operator", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for _, want := range []string{`id="devlive"`, `<section id="devact"`, `hx-swap-oob="true"`, `id="devhd"`, `id="devhd-slot"`, `id="devhd-off"`} {
		if !strings.Contains(body, want) {
			t.Errorf("live answer lacks %q", want)
		}
	}
	for _, bad := range []string{`id="identity"`, `id="devconn"`, "<html", "csrf-leak"} {
		if strings.Contains(body, bad) {
			t.Errorf("live answer carries %q", bad)
		}
	}
	if r, _ := e.do(t, "GET", "/devices/99/live", "operator", nil); r.StatusCode != 404 {
		t.Errorf("unknown device: %d", r.StatusCode)
	}
}

// Why (#126): a relay switched on the overview updates the box without a reload: the answer to the
// htmx post is the live region with the new state ("n of m on" included), also before the health
// tracker has heard of it. A failed command is said in place.
func TestRelaySwitchAnswersWithTheLiveRegion(t *testing.T) {
	f := &fakeCmd{state: "OFF"}
	e := setupWith(t, func(d *Deps) {
		d.Health.(fakeHealth)[1] = model.Health{DeviceID: 1, State: model.HealthOnline, LastSeen: time.Now(), SeenHTTP: time.Now(), Power: map[string]string{"POWER1": "ON", "POWER2": "ON"}}
		d.DeviceCmd = f
	})
	resp, body := e.do(t, "POST", "/devices/1/power/2", "operator", url.Values{"csrf": {"tok"}, "action": {"OFF"}, "back": {"device"}}, "HX-Request", "true")
	if resp.StatusCode != 200 || !strings.Contains(body, `id="devlive"`) || !strings.Contains(body, "1 of 2 on") || !strings.Contains(body, `aria-label="Relay 2 is Off: switch it On"`) {
		t.Fatalf("status %d:\n%s", resp.StatusCode, body)
	}
	if strings.Contains(body, `id="identity"`) {
		t.Error("the switch answer carries the Identity card")
	}
	f.powerErr = context.DeadlineExceeded
	_, failed := e.do(t, "POST", "/devices/1/power/2", "operator", url.Values{"csrf": {"tok"}, "action": {"OFF"}, "back": {"device"}}, "HX-Request", "true")
	if !strings.Contains(failed, `role="alert"`) || !strings.Contains(failed, "did not answer in time") {
		t.Errorf("a failed switch must say why in place:\n%s", failed)
	}
}

// Why: telemetry may arrive every few seconds; one refresh reads the policy report exactly once
// (it is the dearest part of the page: all rules over all devices, from stored values) and the
// htmx throttle bounds refreshes to one per 2 s.
func TestLiveRefreshReadsThePolicyReportOnce(t *testing.T) {
	fp := &fakePolicy{rep: PolicyReport{Rules: []policy.Rule{{Command: "TelePeriod", Want: "300"}}}}
	e := setupWith(t, func(d *Deps) { d.Policy = fp })
	e.do(t, "GET", "/devices/1/live", "operator", nil)
	if len(fp.live) != 1 || fp.live[0] {
		t.Fatalf("report calls %v: one, stored values only (live=false)", fp.live)
	}
}

// readStream opens the event stream, publishes ev until the stream has sent until (when ev has a
// topic), and returns what it read. since is the page's sequence number.
func readStream(t *testing.T, e *env, since string, ev *events.Event, until string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/tf/events?since="+since, nil)
	req.Header.Set("X-Test-Role", "operator")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ev != nil {
		go func() {
			for ctx.Err() == nil {
				e.hub.Publish(*ev)
				time.Sleep(20 * time.Millisecond)
			}
		}()
	}
	rd := bufio.NewReader(resp.Body)
	var got string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !strings.Contains(got, until) {
			line, err := rd.ReadString('\n')
			got += line
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
	}
	return got
}

// Why (#126): a state change of device 1 reaches only listeners of device 1; a job of device 1 too.
func TestStreamNamesDeviceEventsWithTheDeviceID(t *testing.T) {
	e := setup(t)
	hl := model.Health{DeviceID: 1, State: model.HealthOffline, LastSeen: time.Now(), Power: map[string]string{"POWER": "ON"}}
	got := readStream(t, e, "999999999", &events.Event{Topic: events.TopicHealth, Payload: hl}, "devstate-1")
	if !strings.Contains(got, "event: devstate-1\n") || strings.Contains(got, "devstate-2") {
		t.Errorf("health change:\n%s", got)
	}
	got = readStream(t, e, "999999999", &events.Event{Topic: events.TopicJob, Payload: model.Job{ID: 5, DeviceID: 1}}, "devjob-1")
	if !strings.Contains(got, "event: devjob-1\n") || strings.Contains(got, "devjob-2") {
		t.Errorf("job:\n%s", got)
	}
}

// Why (#160): the page is built from data older than the stream. A stream opened for a page
// built before events were published must say "resync" at once, or the page keeps showing the
// state of render time (a device Online that went Offline in between) until another event for
// that device arrives. A page that is up to date gets no needless refresh.
func TestStreamResyncsWhenEventsCameBetweenRenderAndConnect(t *testing.T) {
	e := setup(t)
	stale := "0"
	e.hub.Publish(events.Event{Topic: events.TopicBackup}) // seq is now > 0: a change after a page built at 0
	if got := readStream(t, e, stale, nil, "event: resync"); !strings.Contains(got, "event: resync\n") {
		t.Fatalf("a stale page gets no resync:\n%q", got)
	}
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	if !strings.Contains(body, `sse-connect="/tf/events?since=`) {
		t.Fatal("the page does not hand its sequence number to the stream")
	}
	current := strconvSeq(e.hub.Seq())
	if got := readStream(t, e, current, nil, "event: resync"); strings.Contains(got, "event: resync") {
		t.Fatalf("an up-to-date page must not be told to refresh:\n%q", got)
	}
}

func strconvSeq(n uint64) string { return itoa(int(n)) }

// Why (D2): a relay shows the name its device gives it; Tasmota's untouched defaults ("Tasmota",
// "Tasmota2", ...) say nothing and stay "Relay N". A name missing for a relay (fewer FriendlyNames
// than relays) falls back too.
func TestRelayNamesComeFromTheDeviceUnlessTheyAreDefaults(t *testing.T) {
	for _, tc := range []struct {
		names []string
		n     int
		want  string
	}{
		{[]string{"Monitor", "PC"}, 1, "Monitor"},
		{[]string{"Monitor", "PC"}, 2, "PC"},
		{[]string{"Tasmota", "Tasmota2"}, 1, "Relay 1"},
		{[]string{"Tasmota", "Tasmota2"}, 2, "Relay 2"},
		{[]string{"Tasmota", "Tasmota12"}, 2, "Relay 2"},
		{[]string{"Tasmota lamp"}, 1, "Tasmota lamp"},
		{[]string{"Monitor"}, 2, "Relay 2"},
		{[]string{" "}, 1, "Relay 1"},
		{nil, 3, "Relay 3"},
	} {
		if got := relayName(tc.names, tc.n); got != tc.want {
			t.Errorf("%v relay %d: %q, want %q", tc.names, tc.n, got, tc.want)
		}
	}
	box := relayBoxOf(model.Health{Power: map[string]string{"POWER1": "ON", "POWER2": "OFF"}}, health.Telemetry{}, []string{"Monitor", "Tasmota2"}, true)
	if box.Rows[0].Name != "Monitor" || box.Rows[1].Name != "Relay 2" || box.Rows[0].Aria != "Monitor is On: switch it Off" {
		t.Fatalf("%+v", box.Rows)
	}
}

// Why (#127): the OTA URL the device would update from is a row of Identity, only when the prober
// found one (never an empty "Unknown" row for devices that were not read yet).
func TestIdentityShowsTheOTAURLOnlyWhenKnown(t *testing.T) {
	e := setup(t)
	_, none := e.do(t, "GET", "/devices/1", "admin", nil)
	if strings.Contains(none, "OTA URL") {
		t.Error("an OTA URL row without a value")
	}
	d := e.inv.devs[1]
	d.OTAURL = "http://ota.tasmota.local/tasmota32/release/tasmota32s3.bin"
	e.inv.devs[1] = d
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(body, "<dt>OTA URL</dt>") || !strings.Contains(strings.ReplaceAll(body, "<wbr>", ""), "ota.tasmota.local") {
		t.Errorf("OTA URL row missing: %s", between(body, "Firmware", "MAC"))
	}
	if strings.Index(body, "<dt>OTA URL</dt>") < strings.Index(body, "<dt>Firmware</dt>") {
		t.Error("OTA URL belongs after Firmware")
	}
}

// Why (#145, D5): a device without a relay gets a Sensors box instead of the relay box, filled from
// MQTT telemetry only (no Status 10 polling). A device we do not hear over MQTT says so in one
// line instead of showing an empty box that looks like a fault; one that does but has sent nothing
// yet says that.
func TestSensorsBoxReplacesTheRelayBox(t *testing.T) {
	tele := health.Telemetry{Sensors: []health.SensorReading{
		{Sensor: "BME280", Name: "Temperature", Value: 12.4, Unit: "°C"},
		{Sensor: "BME280", Name: "Pressure", Value: 1012, Unit: "hPa"},
		{Sensor: "BH1750", Name: "Illuminance", Value: 3400.256, Unit: "lx"},
	}}
	box := sensorBoxOf(overviewIn{Dev: model.Device{MQTT: model.MQTTOurs}, Health: onlineHealth(nil), Tele: tele})
	if box == nil || len(box.Cells) != 3 || box.Right != "no relay" || box.Note != "" {
		t.Fatalf("%+v", box)
	}
	want := []sensorCell{{"Temperature · BME280", "12.4 °C"}, {"Pressure · BME280", "1012 hPa"}, {"Illuminance · BH1750", "3400.26 lx"}}
	for i, c := range want {
		if box.Cells[i] != c {
			t.Errorf("cell %d: %+v, want %+v", i, box.Cells[i], c)
		}
	}
	httpOnly := sensorBoxOf(overviewIn{Dev: model.Device{}, Health: model.Health{State: model.HealthOnline, SeenHTTP: now0}})
	if len(httpOnly.Cells) != 0 || !strings.Contains(httpOnly.Note, "MQTT") {
		t.Errorf("HTTP-only device: %+v", httpOnly)
	}
	quiet := sensorBoxOf(overviewIn{Dev: model.Device{MQTT: model.MQTTOurs}, Health: onlineHealth(nil)})
	if quiet.Note != "No sensor readings yet." {
		t.Errorf("no readings yet: %q", quiet.Note)
	}
}

// Why: the page shows a sensor box for a relay-less device and a relay box for one with relays,
// never both; the box is dimmed with the rest of the live state when the device is offline (its
// readings are stale).
func TestOverviewShowsSensorsOnlyWithoutRelays(t *testing.T) {
	var hh telemetryHealth
	e := setupWith(t, func(d *Deps) {
		hh = telemetryHealth{fakeHealth: d.Health.(fakeHealth), tele: map[model.DeviceID]health.Telemetry{
			1: {Sensors: []health.SensorReading{{Sensor: "BME280", Name: "Temperature", Value: 12.4, Unit: "°C"}}},
		}}
		hh.fakeHealth[1] = model.Health{DeviceID: 1, State: model.HealthOnline, LastSeen: time.Now(), SeenHTTP: time.Now(), SeenMQTT: time.Now(), LWT: "Online"}
		d.Health = hh
	})
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(body, `<h2 id="dc-sensors">Sensors</h2>`) || !strings.Contains(body, "Temperature · BME280") || !strings.Contains(body, "12.4 °C") || strings.Contains(body, `id="dc-relays"`) {
		t.Errorf("sensor device:\n%s", body)
	}
	if strings.Contains(body, `class="devcard dev-box dev-dim is-dim"`) {
		t.Error("an online device's sensors are not dimmed")
	}
	hh.fakeHealth[1] = model.Health{DeviceID: 1, State: model.HealthOffline, LastSeen: time.Now().Add(-time.Hour)}
	_, off := e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(off, `class="devcard dev-box dev-dim is-dim"`) || !strings.Contains(off, `aria-labelledby="dc-sensors"`) {
		t.Errorf("offline sensors must be dimmed:\n%s", off)
	}
}

// Why: this is the shape a real multi-channel meter reports (and the simulator, from the fleet's
// captures): Power as an array, one value per channel, but Today, Yesterday and Total once, summed
// over the channels. The scalar counters are not a fifth channel: counting them as one made a
// 4-relay strip "5 meter channels" instead of "metered per channel".
func TestMultiChannelMeterWithSummedCountersIsStillPerChannel(t *testing.T) {
	tele := health.Telemetry{Energy: map[string]health.EnergyChannel{
		"1": {Power: f64(42)}, "2": {Power: f64(96)}, "3": {Power: f64(20)}, "4": {Power: f64(0)},
		"": {Today: f64(1.62), Yesterday: f64(2.04), Total: f64(412)},
	}}
	tl := powerTiles(tele)
	if tl[0].Value != "158 W" || tl[0].Sub != "all channels" || tl[1].Value != "1.62 kWh" || tl[1].Sub != "Yesterday 2.04 · Total 412" {
		t.Fatalf("tiles: %+v", tl)
	}
	box := relayBoxOf(model.Health{Power: map[string]string{"POWER1": "ON", "POWER2": "ON", "POWER3": "ON", "POWER4": "OFF"}}, tele, nil, true)
	if box.Right != "3 of 4 on · metered per channel" || box.Rows[1].Line != "96 W" {
		t.Fatalf("box: %+v", box)
	}
	// one relay on a single meter: the entry "" alone is one meter, not zero channels
	one := relayBoxOf(model.Health{Power: map[string]string{"POWER1": "ON", "POWER2": "OFF"}}, health.Telemetry{Energy: map[string]health.EnergyChannel{"": meter(60, 1, 1, 1)}}, nil, true)
	if one.Right != "1 of 2 on · one meter for all" {
		t.Fatalf("shared meter: %+v", one)
	}
}

// publishingHealth publishes a hub event whenever the page reads a device's health: an event that
// arrives while the page is being built.
type publishingHealth struct {
	fakeHealth
	hub *events.Hub
}

func (p publishingHealth) Get(id model.DeviceID) model.Health {
	p.hub.Publish(events.Event{Topic: events.TopicBackup})
	return p.fakeHealth.Get(id)
}

func (p publishingHealth) All() map[model.DeviceID]model.Health {
	p.hub.Publish(events.Event{Topic: events.TopicBackup})
	return p.fakeHealth.All()
}

// Why (#160): the page's event number must be taken before its data is read. Taken after, an event
// published while the page was being built is counted as already seen, and the stream would not
// ask for a refresh although the page shows data older than that event.
func TestPagesTakeTheEventNumberBeforeReadingTheirData(t *testing.T) {
	hub := events.NewHub(8)
	e := setupWith(t, func(d *Deps) {
		d.Hub = hub
		d.Health = publishingHealth{fakeHealth: d.Health.(fakeHealth), hub: hub}
	})
	for _, path := range []string{"/devices/1", "/"} {
		before := hub.Seq()
		_, body := e.do(t, "GET", path, "operator", nil)
		after := hub.Seq()
		if after == before {
			t.Fatalf("%s: the test health published nothing while the page was built", path)
		}
		m := regexp.MustCompile(`events\?since=(\d+)`).FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("%s: no stream address", path)
		}
		if since, _ := strconv.ParseUint(m[1], 10, 64); since != before {
			t.Errorf("%s: since=%d, want %d (the number before the page read anything; now %d)", path, since, before, after)
		}
	}
}

// Why (#308): every telemetry event refreshes the live region, and the Activity box used to be
// swapped with it each time, so its table flickered and lost the reader's place although no
// entry had changed. The page tells the server which Activity it shows; an unchanged list is
// not sent again, a changed one is.
func TestLiveRefreshLeavesAnUnchangedActivityBoxAlone(t *testing.T) {
	hist := &fakeHistory{events: stateChanges(3)}
	e := setupWith(t, func(d *Deps) { d.History = hist })
	_, page := e.do(t, "GET", "/devices/1", "operator", nil)
	m := regexp.MustCompile(`hx-get="/tf/devices/1/live\?act=([0-9a-f]+)"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("the live region does not name the Activity it shows:\n%s", shortDump(page))
	}
	_, same := e.do(t, "GET", "/devices/1/live?act="+m[1], "operator", nil)
	if strings.Contains(same, `id="devact"`) {
		t.Error("the unchanged Activity box was sent again")
	}
	if !strings.Contains(same, `id="devlive"`) || !strings.Contains(same, `id="devhd"`) {
		t.Error("the rest of the live answer must still come")
	}
	hist.events = append([]store.HealthEvent{{DeviceID: 1, At: time.Date(2026, 10, 23, 13, 0, 0, 0, time.UTC), From: model.HealthOnline, To: model.HealthOffline, Source: model.SourceHTTP}}, hist.events...)
	_, changed := e.do(t, "GET", "/devices/1/live?act="+m[1], "operator", nil)
	if !strings.Contains(changed, `id="devact"`) {
		t.Error("a new state change must update the Activity box")
	}
	_, unknown := e.do(t, "GET", "/devices/1/live", "operator", nil)
	if !strings.Contains(unknown, `id="devact"`) {
		t.Error("without a signature the box is always sent")
	}
}

var httpFresh = transport.HTTPState{State: transport.HTTPOK}

// Why (#416): the tile said "Reachable" for days after ONE successful health poll since start,
// while the card "How TasmoFleet reaches this device" (same facts) correctly said "Not checked
// lately". Tile and card must read the same HTTP state, and an old contact shows its age.
func TestReachTileAndConnectionCardAgreeOnHTTP(t *testing.T) {
	ours := model.Device{MQTT: model.MQTTOurs}
	stale := now0.Add(-3*24*time.Hour - 4*time.Hour)
	tests := []struct {
		name     string
		seen     time.Time
		http     transport.HTTPState
		cardWord string
		tileSub  string
	}{
		{"fresh", now0.Add(-time.Minute), httpFresh, "Reachable", "Connected · Reachable"},
		{"stale", stale, transport.HTTPState{}, "Not checked lately", "Connected · HTTP not checked lately, last answered 3d 4h ago"},
		{"never polled", time.Time{}, transport.HTTPState{}, "Not checked lately", "Connected · HTTP not polled yet"},
		{"failing", stale, transport.HTTPState{State: transport.HTTPFailing, Kind: transport.HTTPFailUnreachable, Since: now0.Add(-time.Hour)}, "Not reachable", "Connected · HTTP not reachable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hl := onlineHealth(nil)
			hl.SeenHTTP = tc.seen
			card := httpRow(transport.Facts{HTTP: tc.http, Now: now0})
			tile := reachTile(ours, hl, tc.http, now0)
			if card.Value != tc.cardWord {
				t.Fatalf("card %q, want %q", card.Value, tc.cardWord)
			}
			if tile.Sub != tc.tileSub {
				t.Fatalf("tile sub %q, want %q", tile.Sub, tc.tileSub)
			}
			if (tc.http.State == transport.HTTPOK) != (tile.Value == "MQTT · HTTP") {
				t.Fatalf("tile %q disagrees with card %q", tile.Value, card.Value)
			}
		})
	}
}
