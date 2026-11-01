package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/discovery"
	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/importer"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
)

// between returns the part of s after the first from and before the next to ("" when from is
// missing), so an assertion looks at one region of the page only.
func between(s, from, to string) string {
	_, after, ok := strings.Cut(s, from)
	if !ok {
		return ""
	}
	before, _, _ := strings.Cut(after, to)
	return before
}

// dashFleet is a fleet with every case the dashboard words differently: online, offline after
// being seen, never answered, refusing the login, and a policy deviation.
func dashFleet(t *testing.T, mutate func(*Deps)) (*env, *fakeDiscovery) {
	t.Helper()
	disc := &fakeDiscovery{props: []discovery.Proposal{
		{ID: 31, Kind: discovery.KindAddressChange, Via: discovery.ViaMQTT, Version: "v31", ExistingID: 1, Device: model.Device{Name: "kitchen", Address: "10.0.0.11"}},
		{ID: 30, Kind: discovery.KindNew, Via: discovery.ViaScan, Version: "v30", Device: model.Device{Name: "Shed Light", Address: "10.0.0.88"}},
		{ID: 32, Kind: discovery.KindConflict, Via: discovery.ViaScan, Version: "v32", Device: model.Device{Name: "clash", Address: "10.0.0.2"}},
	}}
	seen := time.Now().Add(-5 * time.Hour)
	e := setupWith(t, func(d *Deps) {
		d.Discovery = disc
		d.Policy = &fakePolicy{rep: PolicyReport{Findings: []policy.Finding{{DeviceID: 1, Rule: policy.Rule{Command: "TelePeriod", Want: "300"}, Have: "60"}}}}
		if mutate != nil {
			mutate(d)
		}
	})
	e.inv.devs[3] = model.Device{ID: 3, Name: "hallway", Address: "10.0.0.3", Identity: model.Identity{Version: "15.0.0", DetectedAt: seen}}
	e.inv.devs[4] = model.Device{ID: 4, Name: "annex", Address: "10.0.0.4"}
	e.inv.devs[5] = model.Device{ID: 5, Name: "porch", Address: "10.0.0.5", Identity: model.Identity{DetectedAt: seen}}
	e.health[3] = model.Health{DeviceID: 3, State: model.HealthOffline, LastSeen: seen}
	e.health[4] = model.Health{DeviceID: 4, State: model.HealthOffline} // polled, failed, never answered
	e.health[5] = model.Health{DeviceID: 5, State: model.HealthAuthFailed, LastSeen: seen}
	e.inv.devs[6] = model.Device{ID: 6, Name: "desk", Address: "10.0.0.6", Identity: model.Identity{DetectedAt: seen}}
	e.health[6] = model.Health{DeviceID: 6, State: model.HealthOnline, LastSeen: time.Now()} // online and compliant: nothing to do
	return e, disc
}

// Why (design v2, P1): a device that never answered is "Unknown" (prototype: "never answered",
// "Has not answered since it was added") even after its first polls failed; "Offline" is kept
// for a device that was seen and then lost. Without this the fixture's two never-reached devices
// read as Offline and the Unknown tile is always 0.
func TestFleetSummaryNeverAnsweredIsUnknown(t *testing.T) {
	seen := time.Now().Add(-time.Hour)
	rows := []deviceRow{
		{Device: model.Device{ID: 1}, Health: model.Health{DeviceID: 1, State: model.HealthOffline}},                                                            // never answered
		{Device: model.Device{ID: 2, Identity: model.Identity{DetectedAt: seen}}, Health: model.Health{DeviceID: 2, State: model.HealthOffline}},                // answered before this start
		{Device: model.Device{ID: 3}, Health: model.Health{DeviceID: 3, State: model.HealthOffline, LastSeen: seen}},                                            // answered since this start
		{Device: model.Device{ID: 4}, Health: model.Health{DeviceID: 4, State: model.HealthUnknown}},                                                            // not polled yet
		{Device: model.Device{ID: 5, Identity: model.Identity{DetectedAt: seen}}, Health: model.Health{DeviceID: 5, State: model.HealthOnline, LastSeen: seen}}, // online
	}
	s := summarize(rows, fleetFacts{}, nil, false)
	if s.Online != 1 || s.Offline != 2 || s.Unknown != 2 {
		t.Errorf("online/offline/unknown = %d/%d/%d, want 1/2/2", s.Online, s.Offline, s.Unknown)
	}
	if s.States[model.HealthOffline] != 2 || s.States[model.HealthUnknown] != 2 {
		t.Errorf("states disagree with the tiles: %v", s.States)
	}
}

// Why (design v2, P1): the six tiles are the prototype's, each a link to the list that shows
// exactly what it counts (the chip filters of N7), and the health bar draws the same three numbers.
func TestDashboardTilesLinkToTheirFilters(t *testing.T) {
	e, _ := dashFleet(t, nil)
	_, body := e.do(t, "GET", "/", "admin", nil)
	// templ separates the tile's lines with a space; only the order of the parts matters here
	tiles := strings.ReplaceAll(between(body, `<div class="stat-grid">`, `<svg class="health-bar"`), "</span> <span", "</span><span")
	for _, want := range []string{
		`href="/tf/devices?health=online"`, `>Online</span><span class="stat-value">2</span><span class="stat-hint">of 6 devices</span>`,
		`href="/tf/devices?chip=down"`, `>Offline</span><span class="stat-value">1</span><span class="stat-hint">not reachable</span>`,
		`>Unknown</span><span class="stat-value">2</span><span class="stat-hint">never answered</span>`,
		`>Policy deviations</span><span class="stat-value">1</span><span class="stat-hint">device, 1 rule</span>`,
		`href="/tf/devices?chip=noncompliant"`,
		`href="/tf/devices/new#card-discovered"`, `>New devices found</span><span class="stat-value">1</span><span class="stat-hint">+ 1 address changed</span>`,
	} {
		if !strings.Contains(tiles, want) {
			t.Errorf("tiles: missing %q in %s", want, tiles)
		}
	}
	// No release list is cached: "Firmware behind" must not claim 0 outdated devices.
	if !strings.Contains(tiles, `>Firmware behind</span><span class="stat-value">–</span>`) {
		t.Errorf("firmware tile without a feed: %s", tiles)
	}
	if !strings.Contains(body, `aria-label="2 online, 1 offline, 2 unknown"`) {
		t.Error("the health bar does not draw the tiles' numbers")
	}
	// Operators get no discovery numbers (proposals are admin-only) and no admin buttons.
	_, body = e.do(t, "GET", "/", "operator", nil)
	if strings.Contains(body, "New devices found") || strings.Contains(body, "/discovery/proposals/") {
		t.Error("operator sees discovery proposals")
	}
	if strings.Contains(between(body, `<div class="pghead-acts">`, `</div>`), "Add devices") {
		t.Error("operator is offered Add devices, an admin page")
	}
}

// Why (design v2, P1): "Needs attention" lists what an admin has to act on: proposals first,
// each with its two actions posting to the existing proposal routes (with the shown version,
// or Accept refuses), then the devices with the reason in words. Conflicts are resolved on the
// discovery page and online, compliant devices need no attention.
func TestDashboardNeedsAttention(t *testing.T) {
	e, disc := dashFleet(t, nil)
	_, body := e.do(t, "GET", "/", "admin", nil)
	att := between(body, `<h2>Needs attention</h2>`, `</section>`)
	order := []string{
		"Shed Light", "New device · 10.0.0.88 · Network scan", "/tf/discovery/proposals/30/dismiss", ">Ignore<", "/tf/discovery/proposals/30/accept", ">Add<",
		"kitchen", "Address changed · 10.0.0.1 → 10.0.0.11", ">Dismiss<", "/tf/discovery/proposals/31/accept", ">Apply<",
		"hallway", "Offline · last seen 5h ago",
		"annex", "Has not answered since it was added",
		"porch", "Refuses the stored web login",
		"kitchen", "TelePeriod 60 → 300",
	}
	rest := att
	for _, want := range order {
		i := strings.Index(rest, want)
		if i < 0 {
			t.Fatalf("needs attention: %q missing or out of order in %s", want, att)
		}
		rest = rest[i+len(want):]
	}
	// Within one reason the devices keep the order they were added in (prototype), not the name.
	if strings.Index(att, ">attic<") > strings.Index(att, ">annex<") {
		t.Error("never-answered devices are not in the order they were added (attic is 2, annex 4)")
	}
	if !strings.Contains(att, `name="version" value="v30"`) || !strings.Contains(att, `name="version" value="v31"`) {
		t.Error("the proposal forms do not carry the shown version")
	}
	if strings.Contains(att, "clash") || strings.Contains(att, `href="/tf/devices/6"`) {
		t.Error("a conflict or an online, compliant device is listed")
	}
	// The form really accepts: the version reaches the discovery service.
	resp, _ := e.do(t, "POST", "/discovery/proposals/30/accept", "admin", map[string][]string{"csrf": {"tok"}, "version": {"v30"}})
	if resp.StatusCode != http.StatusSeeOther || len(disc.versions) != 1 || disc.versions[0] != "v30" {
		t.Errorf("accept from the dashboard form: %d %v", resp.StatusCode, disc.versions)
	}
}

// Why (prototype slice(0, 8)): the list is a glance, not the device list; a fleet with many
// offline devices must not push the transactions off the page.
func TestDashboardNeedsAttentionIsCapped(t *testing.T) {
	e := setup(t)
	for i := model.DeviceID(10); i < 30; i++ {
		e.inv.devs[i] = model.Device{ID: i, Name: "dead", Address: "10.0.1.1", Identity: model.Identity{DetectedAt: time.Now()}}
		e.health[i] = model.Health{DeviceID: i, State: model.HealthOffline, LastSeen: time.Now()}
	}
	_, body := e.do(t, "GET", "/", "admin", nil)
	if n := strings.Count(body, `class="att-row att-item"`); n != dashAttentionMax {
		t.Errorf("%d devices listed, want %d", n, dashAttentionMax)
	}
}

// Why (design v2, P1): the transactions card shows the newest transaction with its progress
// and the next ones as rows, each a link to the transaction, worded as the prototype words them.
func TestDashboardTransactionsCard(t *testing.T) {
	job := func(id model.JobID, batch model.BatchID, st model.JobState) model.Job {
		return model.Job{ID: id, BatchID: batch, DeviceID: 1, State: st, TargetRelease: "16.1.0", CreatedBy: "adam", CreatedAt: time.Now()}
	}
	e := setupWith(t, func(d *Deps) {
		d.Jobs = &fakeJobs{jobs: []model.Job{ // newest first
			job(9, 5, model.JobAwaitReboot), job(8, 5, model.JobFailed), job(7, 5, model.JobSucceeded), job(6, 5, model.JobQueued),
			job(4, 4, model.JobSucceeded), job(3, 3, model.JobFailed), job(2, 3, model.JobSucceeded), job(1, 2, model.JobCancelled),
			{ID: 0, BatchID: 1, State: model.JobSucceeded, CreatedAt: time.Now()},
		}}
	})
	_, body := e.do(t, "GET", "/", "operator", nil)
	card := between(body, `<h2>Update transactions</h2>`, `</section>`)
	for _, want := range []string{
		`<a class="txn-card" href="/tf/transactions/5">`, `TX-0005`, "Update to Tasmota 16.1.0", ">Running<", "1 of 4 done · 1 failed · 1 in progress · 1 waiting",
		`href="/tf/transactions/4"`, ">Completed<",
		`href="/tf/transactions/3"`, ">1 done · 1 failed<",
		`href="/tf/transactions/2"`, ">Cancelled<",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("transactions card: missing %q in %s", want, card)
		}
	}
	// Why: "Today"/"Yesterday" is the browser's call (#137), so the when of a row is a <time> time.js repaints
	if !strings.Contains(card, `data-ts="day"`) {
		t.Errorf("history rows are not day stamps for time.js: %s", card)
	}
	if strings.Contains(card, `/tf/transactions/1"`) {
		t.Error("more than three history rows")
	}
	if !strings.Contains(between(body, `<h2>Update transactions</h2>`, `</div>`), `href="/tf/transactions"`) {
		t.Error("History does not lead to the transactions list")
	}
}

// Why (design v2, P1, dashboard-empty): a fleet without devices shows only the empty state with
// the three ways to add devices (admins), not six zero tiles and empty lists.
func TestDashboardEmptyFleet(t *testing.T) {
	e := setupWith(t, func(d *Deps) {
		d.Discovery = &fakeDiscovery{}
		d.Import = importer.New(importer.Options{Inventory: &recInv{}})
	})
	delete(e.inv.devs, 1)
	delete(e.inv.devs, 2)
	_, body := e.do(t, "GET", "/", "admin", nil)
	if !strings.Contains(body, "0 devices · state streams in live") || !strings.Contains(body, "<h2>No devices yet</h2>") {
		t.Fatalf("empty dashboard: %s", body)
	}
	for _, want := range []string{`href="/tf/devices/new#card-scan">Scan network`, `href="/tf/devices/new">Add manually`, `href="/tf/devices/new#card-import">Import from TasmoAdmin`} {
		if !strings.Contains(body, want) {
			t.Errorf("empty state: missing %q", want)
		}
	}
	// Updates are off in this setup: the button stays (prototype) and its page says why.
	if !strings.Contains(body, `href="/tf/updates/new">New update`) {
		t.Error("admin: New update is missing")
	}
	if _, page := e.do(t, "GET", "/updates/new", "admin", nil); !strings.Contains(page, "Updates are switched off") {
		t.Error("New update with updates off does not explain why")
	}
	if strings.Contains(body, "stat-grid") || strings.Contains(body, "Needs attention") {
		t.Error("the empty state still draws tiles or lists")
	}
	_, body = e.do(t, "GET", "/", "operator", nil)
	if strings.Contains(body, "Scan network") || strings.Contains(body, "Add manually") {
		t.Error("operator is offered admin actions in the empty state")
	}
}

// Why (design v2, P1, structure deviation S6 resolved): the subtitle says "state streams in live",
// so the dashboard holds the event stream and reloads its body when a device changes state, a
// device is added or removed, or a transaction moves.
func TestDashboardIsLive(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/", "operator", nil)
	if !strings.Contains(body, `sse-connect="/tf/events?since=`) {
		t.Error("the dashboard opens no event stream")
	}
	if !strings.Contains(body, `<div id="dash" hx-get="/tf/" hx-trigger="sse:state throttle:2s, sse:device throttle:2s, sse:job throttle:2s, sse:resync" hx-select="#dash" hx-swap="outerHTML">`) {
		t.Errorf("the dashboard body does not refresh on stream events: %s", body)
	}
}

// Why: telemetry arrives every few seconds per device; the dashboard must reload only when a
// device's state really changes ("state"), not on every health message.
func TestSSEStateEventOnlyOnChange(t *testing.T) {
	e := setup(t) // kitchen (1) is online
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/tf/events", nil)
	req.Header.Set("X-Test-Role", "operator")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	go func() {
		for _, st := range []model.HealthState{model.HealthOnline, model.HealthOnline, model.HealthOffline, model.HealthOffline} {
			time.Sleep(50 * time.Millisecond)
			e.hub.Publish(events.Event{Topic: events.TopicHealth, Payload: model.Health{DeviceID: 1, State: st}})
		}
		time.Sleep(50 * time.Millisecond)
		e.hub.Publish(events.Event{Topic: events.TopicDevice, Payload: model.Device{ID: 1}})
	}()
	buf := make([]byte, 4096)
	var got string
	for !strings.Contains(got, "event: device") {
		n, err := resp.Body.Read(buf)
		got += string(buf[:n])
		if err != nil {
			t.Fatalf("stream ended: %v (%q)", err, got)
		}
	}
	if n := strings.Count(got, "event: state\n"); n != 1 || !strings.Contains(got, "event: state\ndata: 1\n") {
		t.Errorf("%d state events, want exactly one (online -> offline): %q", n, got)
	}
}

// Why: live policy values are kept in memory only. After a restart, until the first background
// read (spread over up to 10 minutes, never while the live check is paused) and without a decoded
// backup, every value is unknown; the tile must then say so instead of claiming 0 deviations,
// like "Firmware behind" without a release list. One value known (here a compliant one) makes 0 real.
func TestDashboardPolicyTileUnknownBeforeFirstRead(t *testing.T) {
	unread := []policy.Finding{
		{DeviceID: 1, Rule: policy.Rule{Command: "TelePeriod", Want: "300"}, Unknown: true},
		{DeviceID: 3, Rule: policy.Rule{Command: "TelePeriod", Want: "300"}, Unknown: true},
	}
	for _, c := range []struct {
		name string
		prov map[model.DeviceID]map[string]ValueSource
		want string
	}{
		{"nothing read", nil, `>Policy deviations</span><span class="stat-value">–</span><span class="stat-hint">not checked yet</span>`},
		{"one value read", map[model.DeviceID]map[string]ValueSource{6: {"teleperiod": {Kind: "live", At: time.Now()}}},
			`>Policy deviations</span><span class="stat-value">0</span><span class="stat-hint">devices, 0 rules, 2 not checked yet</span>`},
	} {
		e, _ := dashFleet(t, func(d *Deps) { d.Policy = &fakePolicy{rep: PolicyReport{Findings: unread, Provenance: c.prov}} })
		_, body := e.do(t, "GET", "/", "admin", nil)
		tiles := strings.ReplaceAll(between(body, `<div class="stat-grid">`, `<svg class="health-bar"`), "</span> <span", "</span><span")
		if !strings.Contains(tiles, c.want) {
			t.Errorf("%s: missing %q in %s", c.name, c.want, tiles)
		}
	}
}

// Why: the device list's "Not compliant" chip must follow the tile's rule: "–" while nothing was
// read, and the known count with "n not checked yet" beside it (the tooltip, the tile's hint
// wording) while only part of the fleet was, so a 0 never stands alone as a claim of compliance
// nobody checked. "0+" read oddly (v0.11.1 feedback 4): the count stays a plain
// number, the uncertainty is said in words.
func TestDeviceListNotCompliantChipUnknownOrPartial(t *testing.T) {
	unread := []policy.Finding{
		{DeviceID: 1, Rule: policy.Rule{Command: "TelePeriod", Want: "300"}, Unknown: true},
		{DeviceID: 3, Rule: policy.Rule{Command: "TelePeriod", Want: "300"}, Unknown: true},
	}
	for _, c := range []struct {
		name string
		prov map[model.DeviceID]map[string]ValueSource
		want string
	}{
		{"nothing read", nil, `title="2 not checked yet">Not compliant<span class="dv-n">–</span>`},
		{"partly read", map[model.DeviceID]map[string]ValueSource{6: {"teleperiod": {Kind: "live", At: time.Now()}}},
			`title="2 not checked yet">Not compliant<span class="dv-n">0</span>`},
	} {
		e, _ := dashFleet(t, func(d *Deps) { d.Policy = &fakePolicy{rep: PolicyReport{Findings: unread, Provenance: c.prov}} })
		_, body := e.do(t, "GET", "/devices", "admin", nil)
		if !strings.Contains(body, c.want) {
			t.Errorf("%s: missing %q in %s", c.name, c.want, between(body, `class="dv-chips"`, `class="dv-vsep"`))
		}
	}
}

// Why: #294, a discovered device that is offline is a proposal, not an inventory device: it must
// not sit in "Needs attention" (it is not counted as an offline device either). It shows on the
// Discovered page, with its hostname and "offline since".
func TestOfflineProposalLeavesNeedsAttention(t *testing.T) {
	props := []discovery.Proposal{
		{ID: 40, Kind: discovery.KindNew, Via: discovery.ViaMQTT, Version: "v40", LWT: "Offline", Device: model.Device{Name: "Gone Lamp", Address: "10.0.0.40"}},
		{ID: 41, Kind: discovery.KindNew, Via: discovery.ViaMQTT, Version: "v41", LWT: "Online", Device: model.Device{Name: "Live Lamp", Address: "10.0.0.41"}},
	}
	got := dashProposals("", props, nil)
	if len(got) != 1 || got[0].Name != "Live Lamp" {
		t.Fatalf("dashboard proposals = %+v, want only the online one", got)
	}
}

func TestDiscoveredPageShowsOfflineSince(t *testing.T) {
	e, fd := setupDiscovery(t)
	fd.props = []discovery.Proposal{{ID: 1, Version: "v1", Kind: discovery.KindNew, Via: discovery.ViaMQTT, LWT: "Offline",
		FirstSeen: time.Now().Add(-3 * time.Hour), SeenAt: time.Now().Add(-time.Hour), OfflineSince: time.Now().Add(-2 * time.Hour),
		Device: model.Device{Name: "Gone Lamp", Topic: "gone", Address: "10.0.0.40", MAC: "AA:00:00:00:00:09", Net: model.NetFacts{Hostname: "tasmota-gone"}}}}
	_, body := e.do(t, "GET", "/devices/new", "admin", nil)
	for _, want := range []string{"tasmota-gone", ">since <time"} {
		if !strings.Contains(body, want) {
			t.Errorf("Discovered page lacks %q", want)
		}
	}
}

// Why: #294 review. The tile counts every new device, offline ones included, and says how many
// are offline so tile and Discovered page agree even though the dashboard list hides them.
func TestNewDevicesTileCountsOfflineProposals(t *testing.T) {
	props := []discovery.Proposal{
		{Kind: discovery.KindNew, LWT: "Offline"}, {Kind: discovery.KindNew, LWT: "Offline"}, {Kind: discovery.KindNew, LWT: "Online"},
	}
	s := summarize(nil, fleetFacts{}, props, true)
	var tile struct{ Value, Hint string }
	for _, ti := range dashTiles("", s) {
		if ti.Label == "New devices found" {
			tile.Value, tile.Hint = ti.Value, ti.Hint
		}
	}
	if tile.Value != "3" || !strings.Contains(tile.Hint, "2 offline") {
		t.Fatalf("tile = %+v, want 3 with \"2 offline\"", tile)
	}
}
