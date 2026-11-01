package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/stability"
)

// fakeStab serves canned history.
type fakeStab struct {
	boots   map[model.DeviceID][]model.DeviceBoot // newest first
	samples map[model.DeviceID][]model.WifiSample
}

func (f fakeStab) AppendBoot(context.Context, model.DeviceBoot) error { return nil }
func (f fakeStab) ListBoots(_ context.Context, id model.DeviceID, _ int) ([]model.DeviceBoot, error) {
	return f.boots[id], nil
}
func (f fakeStab) BootsSince(context.Context, time.Time) (map[model.DeviceID][]model.DeviceBoot, error) {
	return f.boots, nil
}
func (f fakeStab) AppendWifiSample(context.Context, model.WifiSample) error { return nil }
func (f fakeStab) WifiSamplesSince(context.Context, time.Time) (map[model.DeviceID][]model.WifiSample, error) {
	return f.samples, nil
}
func (f fakeStab) PruneWifiSamples(context.Context, time.Time) (int64, error) { return 0, nil }

func loopBoots(id model.DeviceID, n int) []model.DeviceBoot {
	var out []model.DeviceBoot
	for i := 0; i < n; i++ {
		out = append(out, model.DeviceBoot{DeviceID: id, At: time.Now().Add(-time.Duration(i+1) * 8 * time.Minute), BootCount: 20 - i,
			Reason: "Exception", UptimeBefore: 9 * time.Minute})
	}
	return out
}

// stabFleet: device 1 stable, 7 boot-looping, 8 weak by signal, 9 weak by reconnects, 10 offline with a
// boot loop in its history.
func stabFleet(t *testing.T) *env {
	t.Helper()
	fs := fakeStab{boots: map[model.DeviceID][]model.DeviceBoot{7: loopBoots(7, 5), 10: loopBoots(10, 5)}}
	e := setupWith(t, func(d *Deps) { d.Stability = fs })
	add := func(id model.DeviceID, name string, h model.Health) {
		e.inv.devs[id] = model.Device{ID: id, Name: name, Address: "10.0.2." + itoa(int(id)), Identity: model.Identity{DetectedAt: time.Now()}}
		h.DeviceID, h.LastSeen = id, time.Now()
		e.health[id] = h
	}
	on := model.HealthOnline
	add(7, "looper", model.Health{State: on, SignalDBm: -56, RSSI: 88, LinkCount: 1, UptimeSec: 300})
	add(8, "farlamp", model.Health{State: on, SignalDBm: -82, RSSI: 36, LinkCount: 15, WifiDowntime: 221 * time.Second, UptimeSec: 9000})
	add(9, "flappy", model.Health{State: on, SignalDBm: -60, RSSI: 80, LinkCount: 12, UptimeSec: 9000})
	add(10, "gone", model.Health{State: model.HealthOffline, SignalDBm: -90, LinkCount: 30})
	e.health[1] = model.Health{DeviceID: 1, State: on, LastSeen: time.Now(), SignalDBm: -60, RSSI: 80, LinkCount: 1, UptimeSec: 3700}
	return e
}

// Why: the list is where a person scans a fleet; the badge must sit on exactly the devices with a
// problem, and an offline device (whose old signal is meaningless) must not get one.
func TestDeviceListShowsStabilityBadgesOnlyOnProblemDevices(t *testing.T) {
	e := stabFleet(t)
	_, body := e.do(t, "GET", "/devices", "admin", nil)
	row := func(id string) string {
		return between(body, `<tr data-id="`+id+`">`, `</tr>`)
	}
	if !strings.Contains(row("7"), ">Boot loop<") || !strings.Contains(row("7"), "Restarted 5 times in the last hour") {
		t.Errorf("looper: %s", row("7"))
	}
	if !strings.Contains(row("8"), ">Weak Wi-Fi<") || !strings.Contains(row("9"), ">Weak Wi-Fi<") {
		t.Error("weak devices carry no badge")
	}
	for _, id := range []string{"1", "10"} {
		if strings.Contains(row(id), "dv-unst") {
			t.Errorf("device %s got a stability badge: %s", id, row(id))
		}
	}
}

// Why: the dashboard shows the two new reasons in plain words, a boot loop (danger) above weak Wi-Fi
// (warning), and does not list stable or offline-with-history devices for them.
func TestDashboardNeedsAttentionListsBootLoopAndWeakWifi(t *testing.T) {
	e := stabFleet(t)
	_, body := e.do(t, "GET", "/", "admin", nil)
	att := between(body, `<h2>Needs attention</h2>`, `</section>`)
	rest := att
	for _, want := range []string{"gone", "Offline", "looper", "Boot loop: restarted 5 times in the last hour (crash)", "farlamp", "Weak Wi-Fi: -82 dBm, 14 reconnects since boot", "flappy", "Weak Wi-Fi: 11 reconnects since boot"} {
		i := strings.Index(rest, want)
		if i < 0 {
			t.Fatalf("%q missing or out of order in %s", want, att)
		}
		rest = rest[i+len(want):]
	}
	if strings.Contains(att, `>kitchen<`) {
		t.Error("a stable device is listed")
	}
}

// Why: the Overview card is the evidence behind the badge: the notice names the cause and advice, the
// numbers come from BootCount, the recent restarts show the device's own words.
func TestOverviewStabilityCardForABootLoop(t *testing.T) {
	e := stabFleet(t)
	_, body := e.do(t, "GET", "/devices/7", "admin", nil)
	card := between(body, `<section id="stability"`, `</section>`)
	for _, want := range []string{">Boot loop<", "last 24 hours", "Restarted 5 times in the last hour", "every time by a crash", "Check the supply first",
		"Crash (exception)", "5 in 1 h", "Longest uptime", "9 min in the last 24 h", "Recent restarts", `<span class="raw">Exception</span>`, "after 9 min", "-56 dBm"} {
		if !strings.Contains(card, want) {
			t.Errorf("card lacks %q:\n%s", want, card)
		}
	}
	if strings.Index(body, `id="devlive"`) > strings.Index(body, `id="stability"`) || strings.Index(body, `id="stability"`) > strings.Index(body, `id="identity"`) {
		t.Error("the card is not between the relays and Identity")
	}
}

func TestOverviewStabilityCardForWeakWifiAndForAStableDevice(t *testing.T) {
	e := stabFleet(t)
	_, body := e.do(t, "GET", "/devices/8", "admin", nil)
	card := between(body, `<section id="stability"`, `</section>`)
	for _, want := range []string{">Weak Wi-Fi<", "weak is below -75 dBm", "14 times", "-82 dBm", "weak", "14 since boot", "Wi-Fi down for 3 min 41 s in total (Tasmota: LinkCount 15, Downtime 0T00:03:41)"} {
		if !strings.Contains(card, want) {
			t.Errorf("weak card lacks %q:\n%s", want, card)
		}
	}
	if strings.Contains(card, "Recent restarts") {
		t.Error("a device with no stored restart has no list of them")
	}
	_, body = e.do(t, "GET", "/devices/1", "admin", nil)
	card = between(body, `<section id="stability"`, `</section>`)
	for _, want := range []string{">Stable<", "last 7 days", "Not seen yet", "0 in 24 h", "no Wi-Fi downtime"} {
		if !strings.Contains(card, want) {
			t.Errorf("stable card lacks %q:\n%s", want, card)
		}
	}
	if strings.Contains(card, "stab-note") {
		t.Error("a stable device gets a problem notice")
	}
}

// Why: without the stability store (older wiring, tests) nothing may break or show an empty card.
func TestNoStabilityStoreMeansNoCardAndNoBadge(t *testing.T) {
	e := setup(t)
	if _, body := e.do(t, "GET", "/devices/1", "admin", nil); strings.Contains(body, `id="stability"`) {
		t.Error("card without store")
	}
	if _, body := e.do(t, "GET", "/devices", "admin", nil); strings.Contains(body, "dv-unst") {
		t.Error("badge without store")
	}
}

func TestStabilityTexts(t *testing.T) {
	r := stability.Result{Level: stability.WeakWiFi, WeakByReconnects: true, Reconnects: 12}
	if got := stabAttention(r); got != "Weak Wi-Fi: 12 reconnects since boot" {
		t.Errorf("%q", got)
	}
	r = stability.Result{Level: stability.WeakWiFi, WeakBySignal: true, AvgDBm: -80}
	if got := stabAttention(r); got != "Weak Wi-Fi: -80 dBm" {
		t.Errorf("%q", got)
	}
}

// Why (maintainer's decision): the list of recent restarts is the evidence for "Last restart" and the
// counts, so it shows whenever TasmoFleet has seen a restart, not only in a boot loop.
func TestRecentRestartsShowForAStableDeviceToo(t *testing.T) {
	fs := fakeStab{boots: map[model.DeviceID][]model.DeviceBoot{1: {{DeviceID: 1, At: time.Now().Add(-26 * time.Hour), BootCount: 4, Reason: "Software/System restart", UptimeBefore: 3 * time.Hour}}}}
	e := setupWith(t, func(d *Deps) { d.Stability = fs })
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	card := between(body, `<section id="stability"`, `</section>`)
	for _, want := range []string{">Stable<", "Recent restarts", "Restarted by command or update", "after 3h 00m"} {
		if !strings.Contains(card, want) {
			t.Errorf("card lacks %q:\n%s", want, card)
		}
	}
}

// Why: the dashboard line must name the number the card names. The card says "averaged -82 dBm over
// 24 hours"; a line with this second's reading (-70, the device just came closer) would contradict it.
func TestNeedsAttentionNamesTheAverageThatMadeTheLinkWeak(t *testing.T) {
	var samples []model.WifiSample
	for i := 1; i <= 5; i++ {
		samples = append(samples, model.WifiSample{DeviceID: 11, At: time.Now().Add(-time.Duration(i) * time.Hour), DBm: -88})
	}
	e := setupWith(t, func(d *Deps) { d.Stability = fakeStab{samples: map[model.DeviceID][]model.WifiSample{11: samples}} })
	e.inv.devs[11] = model.Device{ID: 11, Name: "moved", Address: "10.0.3.1", Identity: model.Identity{DetectedAt: time.Now()}}
	e.health[11] = model.Health{DeviceID: 11, State: model.HealthOnline, LastSeen: time.Now(), SignalDBm: -70, LinkCount: 1}
	_, body := e.do(t, "GET", "/", "admin", nil)
	att := between(body, `<h2>Needs attention</h2>`, `</section>`)
	if !strings.Contains(att, "Weak Wi-Fi: -85 dBm") || strings.Contains(att, "-70") {
		t.Errorf("attention: %s", att)
	}
}

// Why (#422): the restarts list holds up to five entries over days. A restart from three days ago
// that shows only "12:31" reads like one from today; today's keeps the clock time, an older one
// carries its date (same rule as the other day stamps of the page).
func TestRecentRestartsCarryTheDateWhenNotToday(t *testing.T) {
	now := time.Date(2026, 10, 22, 14, 0, 0, 0, time.UTC)
	boots := []model.DeviceBoot{
		{DeviceID: 1, At: now.Add(-2 * time.Hour), Reason: "Software/System restart"},
		{DeviceID: 1, At: time.Date(2026, 10, 19, 12, 31, 0, 0, time.UTC), Reason: "Power On"},
	}
	c := stabCardOf(now, stability.Result{Level: stability.Stable}, boots, model.Health{}, "")
	if len(c.Boots) != 2 {
		t.Fatalf("boots: %d", len(c.Boots))
	}
	if c.Boots[0].AtText != "12:00" {
		t.Errorf("today's restart: %q, want the clock time only", c.Boots[0].AtText)
	}
	if c.Boots[1].AtText != "19 Oct, 12:31" {
		t.Errorf("older restart: %q, want the date with the clock time", c.Boots[1].AtText)
	}
}
