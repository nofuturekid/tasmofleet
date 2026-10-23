package web

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/health"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// Tests of the design v2 device tabs (WP P3): what the overview, the console, the settings,
// the drift tab and the edit form must show, and why.

// telemetryHealth is fakeHealth with the telemetry the overview reads (watts, energy, signal).
type telemetryHealth struct {
	fakeHealth
	tele map[model.DeviceID]health.Telemetry
}

func (t telemetryHealth) Telemetry() map[model.DeviceID]health.Telemetry { return t.tele }

func f64(v float64) *float64 { return &v }

// snapshotDoc is a backup snapshot (Status1..Status11 replies) with the network facts.
const snapshotDoc = `{"Status5":{"StatusNET":{"Hostname":"kitchen-lamp","IPAddress":"10.0.0.1","Gateway":"10.0.0.254"}},` +
	`"Status11":{"StatusSTS":{"Wifi":{"SSId":"lab-iot","BSSId":"F0:9F:C2:1A:4E:07","Channel":6,"Signal":-61}}},` +
	`"Status6":{"StatusMQT":{"MqttHost":"x","MqttPassword":"must-not-appear"}}}`

// Why: the overview is the place to read a device at a glance: its identity, what it draws,
// how it is connected and whether it follows the policy. The network and Wi-Fi facts come from
// the newest backup snapshot (the live health has none of them), the energy from the meter's
// telemetry, and every value the pixel comparison cannot reproduce is marked for masking. A
// secret in the snapshot never reaches the page.
func TestDeviceOverviewShowsTheCardsFromRealData(t *testing.T) {
	rule := policy.Rule{Name: "Telemetry every 5 min", Command: "TelePeriod", Want: "300"}
	fp := &fakePolicy{rep: PolicyReport{Rules: []policy.Rule{rule, {Command: "SetOption0", Want: "0"}},
		Findings: []policy.Finding{{DeviceID: 1, Rule: rule, Have: "60"}, {DeviceID: 2, Rule: rule, Have: "10"}}}}
	var hh telemetryHealth
	e := setupWith(t, func(d *Deps) {
		hh = telemetryHealth{fakeHealth: d.Health.(fakeHealth), tele: map[model.DeviceID]health.Telemetry{
			1: {SignalDBm: -61, HasSignal: true, Energy: map[string]health.EnergyChannel{"": {Power: f64(12.4), Today: f64(0.021), Yesterday: f64(0.03), Total: f64(11.04)}}},
		}}
		hh.fakeHealth[1] = model.Health{DeviceID: 1, State: model.HealthOnline, LastSeen: time.Now(), SeenHTTP: time.Now(), RSSI: 78, UptimeSec: 3700, LWT: "Online", Power: map[string]string{"POWER": "ON"}}
		d.Health, d.Policy, d.PolicyFix = hh, fp, &fakeFix{}
		d.DeviceCmd = &fakeCmd{}
	})
	e.bk.list = []model.Backup{{ID: 9, DeviceID: 1, Kind: model.BackupSnapshot, TakenAt: time.Now()}}
	e.bk.data = []byte(snapshotDoc)
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	for _, want := range []string{
		`<h2 id="dc-ident">Identity</h2>`, `<h2 id="dc-power">Power &amp; energy</h2>`, `<h2 id="dc-net">Network</h2>`, `<h2 id="dc-wifi">Wi-Fi</h2>`, `<h2 id="dc-pol">Policy compliance</h2>`,
		`<dd class="mono" data-visual-mask>kitchen-lamp</dd>`, // hostname from the snapshot
		`<dd class="mono" data-visual-mask>10.0.0.254</dd>`,   // gateway
		`<dd class="mono" data-visual-mask>lab-iot</dd>`, `<dd class="mono" data-visual-mask>6 (2.4 GHz)</dd>`, `<dd class="mono" data-visual-mask>-61 dBm</dd>`,
		`<dd class="mono">Connected</dd>`, `<dd class="mono">Reachable</dd>`,
		`<div class="devpow-w" data-visual-mask>12<span>W</span></div>`, `<dd class="mono" data-visual-mask>0.02 kWh</dd>`, `<dd class="mono" data-visual-mask>11.0 kWh</dd>`,
		`Relay 1 <span class="mono">POWER</span>`, `aria-label="Relay 1 is On: switch it Off"`,
		"Telemetry every 5 min", "TelePeriod 60 → 300", "Make compliant…", `name="only" value="1"`,
		`<input type="hidden" name="action" value="OFF">`, // the header switch asks for the opposite state, never a blind toggle
		`>Power On</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("overview lacks %q", want)
		}
	}
	if strings.Contains(body, "must-not-appear") || strings.Contains(body, "s3cret-pw") {
		t.Fatal("a secret reached the page")
	}
	// An operator reads the same cards but cannot start the fix.
	_, op := e.do(t, "GET", "/devices/1", "operator", nil)
	if strings.Contains(op, "Make compliant") || !strings.Contains(op, "TelePeriod 60 → 300") {
		t.Errorf("operator: the deviations without the admin action:\n%s", op)
	}
	// A device without deviations matches all rules (the count of rules, not of findings).
	_, d2 := e.do(t, "GET", "/devices/2", "admin", nil)
	if !strings.Contains(d2, "TelePeriod 10 → 300") {
		t.Error("device 2 deviates too")
	}
	fp.rep.Findings = nil
	_, ok := e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(ok, "Matches all 2 rules") || strings.Contains(ok, "Make compliant") {
		t.Errorf("a compliant device:\n%s", ok)
	}
}

// Why: an offline device must say so at the top of every tab, with when it was last seen, and
// its switches must not pretend they can reach it.
func TestOfflineDeviceShowsTheBannerAndDisablesTheSwitches(t *testing.T) {
	e := setupWith(t, func(d *Deps) {
		h := d.Health.(fakeHealth)
		h[1] = model.Health{DeviceID: 1, State: model.HealthOffline, LastSeen: time.Now().Add(-5 * time.Hour), Power: map[string]string{"POWER": "OFF"}}
		d.DeviceCmd = &fakeCmd{}
	})
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	for _, want := range []string{`<div class="devhd-off" role="status">`, `last seen <span data-visual-mask>5h ago</span>`, `<span class="devhd-state t-danger">`, `aria-pressed="false" disabled>`, `role="switch" aria-checked="false" aria-label="Relay 1 is Off: switch it On" disabled>`} {
		if !strings.Contains(body, want) {
			t.Errorf("offline overview lacks %q", want)
		}
	}
	_, online := e.do(t, "GET", "/devices/2", "admin", nil)
	if strings.Contains(online, "devhd-off") {
		t.Error("a device that never answered is not offline: it gets the not-answered notice, not the banner")
	}
}

// Why: the switches of the device page are plain forms. After switching, the browser must come
// back to the device, not to the device list the row switches return to.
func TestDevicePageSwitchReturnsToTheDevice(t *testing.T) {
	f := &fakeCmd{}
	e := cmdEnv(t, f)
	resp, _ := e.do(t, "POST", "/devices/1/power/1", "operator", url.Values{"csrf": {"tok"}, "action": {"ON"}, "back": {"device"}})
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/tf/devices/1" {
		t.Fatalf("got %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = e.do(t, "POST", "/devices/1/power/1", "operator", url.Values{"csrf": {"tok"}, "action": {"ON"}})
	if resp.Header.Get("Location") != "/tf/devices" {
		t.Errorf("the list's switch still returns to the list: %q", resp.Header.Get("Location"))
	}
}

// Why: the quick commands are the prototype's, except that Status 0 is never offered (safety
// rule 2, D9). Clear hides what was shown so far and keeps hiding it after the next send.
func TestConsoleQuickCommandsAndClear(t *testing.T) {
	e := cmdEnv(t, &fakeCmd{reply: `{"StatusSTS":{"Uptime":"1T00:00:00"}}`})
	old := time.Now().Add(-time.Hour)
	e.aud.Record(t.Context(), model.AuditEvent{At: old, Actor: "oidc:olli", Action: "command.send", Target: "device:1", Detail: `{"command":"Status 2"}`, Result: "ok"})
	_, body := e.do(t, "GET", "/devices/1/console", "operator", nil)
	for _, q := range []string{"Status 5", "Power TOGGLE", "Status 11", "TelePeriod 300", "Restart 1"} {
		if !strings.Contains(body, `name="command" value="`+q+`">`+q+`</button>`) {
			t.Errorf("quick command %q missing", q)
		}
	}
	if strings.Contains(body, "Status 0") {
		t.Fatal("Status 0 is offered")
	}
	if !strings.Contains(body, "› Status 2") || !strings.Contains(body, `placeholder="Type a Tasmota command, e.g. Status 5"`) {
		t.Fatalf("console:\n%s", body)
	}
	since := url.Values{"since": {strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)}}
	_, cleared := e.do(t, "GET", "/devices/1/console?"+since.Encode(), "operator", nil)
	if strings.Contains(cleared, "› Status 2") || !strings.Contains(cleared, `<input type="hidden" name="since"`) {
		t.Errorf("Clear must hide the earlier lines and keep the cut-off in the forms:\n%s", cleared)
	}
	_, sent := e.do(t, "POST", "/devices/1/console", "operator", url.Values{"csrf": {"tok"}, "command": {"Status 11"}, "since": since["since"]})
	if strings.Contains(sent, "› Status 2") || !strings.Contains(sent, `data-visual-mask="console-reply"`) {
		t.Errorf("after a send the cleared lines stay hidden and the reply follows:\n%s", sent)
	}
}

// Why: the settings tab tells the admin when a field deviates from a policy rule, at the field,
// before saving a value that keeps the device flagged.
func TestSettingsFieldNamesThePolicyValue(t *testing.T) {
	rule := policy.Rule{Command: "TelePeriod", Want: "300"}
	fp := &fakePolicy{rep: PolicyReport{Rules: []policy.Rule{rule}, Findings: []policy.Finding{{DeviceID: 1, Rule: rule, Have: "60"}}}}
	e := setupWith(t, func(d *Deps) {
		d.Settings = &fakeSettings{values: map[string]string{"TelePeriod": "60", "Hostname": "lamp-1"}}
		d.Policy = fp
	})
	_, body := e.do(t, "GET", "/devices/1/settings", "admin", nil)
	i := strings.Index(body, `name="TelePeriod"`)
	if i < 0 || !strings.Contains(body[i:i+400], "Policy expects 300.") {
		t.Errorf("TelePeriod lacks the policy hint:\n%s", body)
	}
	if j := strings.Index(body, `name="Hostname"`); j < 0 || strings.Contains(body[j:j+400], "Policy expects") {
		t.Error("only the deviating field gets the hint")
	}
	if !strings.Contains(body, ">Save to device</button>") || !strings.Contains(body, `<button type="reset" class="btn">Reset</button>`) {
		t.Error("the form's buttons are the prototype's")
	}
}

// Why: "Show all" lets the reader see what the drift check compared, not only what changed;
// it is a link, so it works without JavaScript, and the count says how many settings differ.
// Restoring is offered to admins only, from the newest config dump.
func TestDriftShowAllListsTheUnchangedSettings(t *testing.T) {
	e, bk, _ := newDiffEnv(t)
	bk.files[5] = []byte(`{"Status6":{"StatusMQT":{"MqttHost":"old.invalid"}},"Status3":{"StatusLOG":{"SerialLog":0}}}`)
	bk.live = []byte(`{"Status6":{"StatusMQT":{"MqttHost":"new.invalid"}},"Status3":{"StatusLOG":{"SerialLog":0}}}`)
	driftMu.Lock()
	clear(driftLast)
	driftMu.Unlock()
	_, body := e.do(t, "GET", "/devices/1/drift", "admin", nil)
	if !strings.Contains(body, "1 setting changed since the last backup") || strings.Contains(body, "SerialLog") || !strings.Contains(body, `href="/tf/devices/1/drift?all=1" role="checkbox" aria-checked="false"`) {
		t.Fatalf("drift:\n%s", body)
	}
	changed := strings.Count(body, `class="devdrift-row is-changed"`)
	_, all := e.do(t, "GET", "/devices/1/drift?all=1", "admin", nil)
	if rows := strings.Count(all, `class="devdrift-row"`); rows != 1 || !strings.Contains(all, "SerialLog") || strings.Count(all, `class="devdrift-row is-changed"`) != changed {
		t.Errorf("Show all adds the unchanged rows (%d) and keeps the changed ones (%d):\n%s", rows, changed, all)
	}
	if !strings.Contains(all, `aria-checked="true"`) || !strings.Contains(all, "Show all 2</a>") || !strings.Contains(all, "/restore\">Restore backup…</a>") {
		t.Errorf("show all checked, restore offered to the admin:\n%s", all)
	}
	_, op := e.do(t, "GET", "/devices/1/drift", "operator", nil)
	if strings.Contains(op, "Restore backup") {
		t.Error("an operator cannot restore")
	}
}

// Why: while the admin types, the page must keep naming the device as it is saved: an emptied
// name field must not empty the title and the trail, and the failing field gets the focus.
func TestEditFormKeepsTheSavedNameInTheHeader(t *testing.T) {
	e := setup(t)
	resp, body := e.do(t, "POST", "/devices/1", "admin", url.Values{"csrf": {"tok"}, "name": {""}, "address": {"10.0.0.1"}, "port": {"99999"}})
	if resp.StatusCode != 422 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for _, want := range []string{`<h1>Edit device</h1><p class="pghead-sub">kitchen</p>`, `<a href="/tf/devices/1">kitchen</a>`, "1 field needs attention.", "Nothing was saved yet.", `aria-invalid="true" aria-describedby="err-port" autofocus`, `name="name" value="" required`, ">Delete device…</a>"} {
		if !strings.Contains(body, want) {
			t.Errorf("edit form lacks %q", want)
		}
	}
	if strings.Contains(body, "s3cret-pw") {
		t.Fatal("the stored password reached the form")
	}
}

// powerHealth adds the 2-hour power history (N8) to telemetryHealth.
type powerHealth struct {
	telemetryHealth
	hist map[model.DeviceID][]health.PowerSample
}

func (p powerHealth) PowerHistory(id model.DeviceID) []health.PowerSample { return p.hist[id] }

// Why (N8): the sparkline shows what the device drew over the last 2 hours. Each 5-minute
// bucket with a reading is one bar, so a reading per TelePeriod gives a continuous line; a
// bucket without a reading stays empty, so a gap in the history (device offline, meter silent)
// shows as a gap instead of being bridged with invented values. The highest bucket fills the
// height; a 0 W reading still leaves a visible stub, unlike a bucket without a reading.
func TestSparklineBarsFollowTheHistoryAndKeepGaps(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	at := func(minAgo int, w float64) health.PowerSample {
		return health.PowerSample{At: now.Add(-time.Duration(minAgo) * time.Minute), Watts: w}
	}
	if got := sparkOf(nil, now); len(got) != 0 {
		t.Fatalf("empty history drew %v", got)
	}
	// Three readings 5 minutes apart, then a 20-minute gap, then one more; two readings in the
	// newest bucket are averaged; a reading older than 2 hours is not drawn.
	got := sparkOf([]health.PowerSample{at(130, 99), at(42, 10), at(37, 0), at(32, 20), at(12, 40), at(1, 30), at(0, 50)}, now)
	if len(got) != 5 {
		t.Fatalf("bars %d, want 5 (one per bucket with a reading): %+v", len(got), got)
	}
	xs := []string{}
	for _, b := range got {
		xs = append(xs, b.X)
	}
	if strings.Join(xs, ",") != "150,160,170,210,230" {
		t.Errorf("bar positions %v: the gap between 170 and 210 must stay empty", xs)
	}
	if got[3].H != "40.0" || got[3].Y != "0.0" {
		t.Errorf("the peak bucket fills the height: %+v", got[3])
	}
	if got[1].H != "3.0" {
		t.Errorf("a 0 W reading keeps the stub: %+v", got[1])
	}
	if got[4].H != "40.0" { // (30+50)/2 = 40 W, the mean of the bucket
		t.Errorf("two readings in one bucket are averaged: %+v", got[4])
	}
}

// Why (N8, D6): the overview draws the sparkline inside the Power & energy card as SVG with
// presentation attributes (strict CSP, no style=), one rect per bar, and draws nothing until
// there is history.
func TestOverviewDrawsTheSparklineOnlyWithHistory(t *testing.T) {
	var ph powerHealth
	e := setupWith(t, func(d *Deps) {
		ph = powerHealth{
			telemetryHealth: telemetryHealth{fakeHealth: d.Health.(fakeHealth), tele: map[model.DeviceID]health.Telemetry{
				1: {Energy: map[string]health.EnergyChannel{"": {Power: f64(12)}}},
			}},
			hist: map[model.DeviceID][]health.PowerSample{},
		}
		d.Health = ph
	})
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(body, `<div class="devspark" data-visual-mask><span class="devspark-note">No readings yet</span></div>`) || strings.Contains(body, "<svg viewBox=\"0 0 240 40\"") {
		t.Errorf("no history: say so instead of drawing an empty box:\n%s", body)
	}
	if strings.Contains(body, "now · last 2 h") {
		t.Error("without a chart the card must not promise the last 2 h")
	}
	// Readings that are all 0 W would draw 24 stubs that read as a dashed line.
	ph.hist[1] = []health.PowerSample{{At: time.Now().Add(-11 * time.Minute), Watts: 0}, {At: time.Now().Add(-6 * time.Minute), Watts: 0}}
	_, idle := e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(idle, `<span class="devspark-note">No load in the last 2 h</span>`) || strings.Contains(idle, `class="devspark-bar"`) {
		t.Errorf("an idle device draws no dashed chart:\n%s", idle)
	}
	now := time.Now()
	ph.hist[1] = []health.PowerSample{{At: now.Add(-11 * time.Minute), Watts: 5}, {At: now.Add(-6 * time.Minute), Watts: 8}, {At: now.Add(-time.Minute), Watts: 12}}
	_, body = e.do(t, "GET", "/devices/1", "admin", nil)
	i := strings.Index(body, `<div class="devspark"`)
	if i < 0 {
		t.Fatal("no sparkline box")
	}
	spark := body[i : i+strings.Index(body[i:], "</div>")]
	if strings.Count(spark, "<svg") != 1 || strings.Count(spark, `<rect class="devspark-bar"`) != 3 || !strings.Contains(spark, `aria-label="Power over the last 2 hours"`) {
		t.Errorf("want one svg with 3 bars:\n%s", spark)
	}
	if strings.Contains(spark, "style=") {
		t.Error("the sparkline carries an inline style (CSP, D6)")
	}
}

type fakeHistory struct{ events []store.HealthEvent }

func (f fakeHistory) ListHealthEvents(_ context.Context, _ model.DeviceID, limit int) ([]store.HealthEvent, error) {
	if limit <= 0 { // the store's contract: no limit means all
		return f.events, nil
	}
	return f.events[:min(limit, len(f.events))], nil
}

// stateChanges makes n state changes of device 1, newest first, alternating online and offline.
func stateChanges(n int) []store.HealthEvent {
	at := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	out := make([]store.HealthEvent, n)
	for i := range out {
		from, to := model.HealthOnline, model.HealthOffline
		if i%2 == 1 {
			from, to = to, from
		}
		out[i] = store.HealthEvent{DeviceID: 1, At: at.Add(-time.Duration(i) * time.Minute), From: from, To: to, Source: model.SourceHTTP, Detail: "change " + strconv.Itoa(i)}
	}
	return out
}

// Why: Thomas saw the Columns button on a row of its own under the card title, detached from
// the card ("looks lost"). On the device page it sits in the card's head, on the title's line.
func TestStateChangesColumnsButtonSitsInTheCardHead(t *testing.T) {
	at := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	e := setupWith(t, func(d *Deps) {
		d.History = fakeHistory{[]store.HealthEvent{
			{DeviceID: 1, At: at, From: model.HealthOnline, To: model.HealthOffline, Source: model.SourceHTTP},
			{DeviceID: 1, At: at.Add(-time.Hour), From: model.HealthUnknown, To: model.HealthOnline, Source: model.SourceHTTP},
		}}
	})
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	i := strings.Index(body, `<h2 id="dc-hist">State changes</h2>`)
	if i < 0 {
		t.Fatal("no State changes card")
	}
	end := strings.Index(body[i:], "<details class=\"m-fold\">") // the table's fold follows the card head
	if head := body[i : i+end]; !strings.Contains(head, `<details class="colmenu"`) {
		t.Fatalf("the Columns chooser is not in the card head:\n%s", head)
	}
	if strings.Contains(body, `class="tabletools"`) {
		t.Error("a separate Columns bar above the table is back")
	}
}

// rows counts the data rows of the first table of the card that starts at marker.
func rowsAfter(body, marker string) int {
	i := strings.Index(body, marker)
	if i < 0 {
		return -1
	}
	rest := body[i:]
	if j := strings.Index(rest, "</table>"); j >= 0 {
		rest = rest[:j]
	}
	return strings.Count(rest, "<tr><td") + strings.Count(rest, "<tr>\n<td")
}

// Why: Thomas asked how far the lists on the device page can grow. The overview is a summary:
// the newest 10 of each, and a link to the complete list (the state history page, and the
// transactions filtered by this device).
func TestDeviceOverviewListsTheNewest10WithLinksToTheCompleteLists(t *testing.T) {
	var jobs []model.Job
	for i := 1; i <= 14; i++ {
		jobs = append(jobs, model.Job{ID: model.JobID(i), BatchID: model.BatchID(i), DeviceID: 1, State: model.JobSucceeded, TargetRelease: "16.0.0", UpdatedAt: time.Now()})
	}
	e := setupWith(t, func(d *Deps) {
		d.History = fakeHistory{stateChanges(25)}
		d.Jobs, d.OTA = &fakeJobs{jobs: jobs}, &fakeOTA{}
	})
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if n := rowsAfter(body, `<h2 id="dc-hist">`); n != 10 {
		t.Errorf("state changes: %d rows, want the newest 10", n)
	}
	if n := rowsAfter(body, `<h2 id="dc-tx">`); n != 10 {
		t.Errorf("transactions: %d rows, want the newest 10", n)
	}
	for _, want := range []string{`href="/tf/devices/1/history"`, `href="/tf/transactions?device=1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("no link %s to the complete list", want)
		}
	}
}

// Why: the complete list of a device's state changes needs a page of its own, with paging
// (a flapping device has thousands). Newest first, every change reachable, no row twice.
func TestDeviceStateHistoryPagesThroughEveryChange(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.History = fakeHistory{stateChanges(60)} })
	_, p1 := e.do(t, "GET", "/devices/1/history", "operator", nil)
	_, p3 := e.do(t, "GET", "/devices/1/history?page=3", "operator", nil)
	if a, b := strings.Count(p1, "<time datetime"), strings.Count(p3, "<time datetime"); a != 25 || b != 10 {
		t.Fatalf("page 1 has %d rows, page 3 has %d; want 25 and 10", a, b)
	}
	if !strings.Contains(p1, "change 0") || strings.Contains(p1, "change 25<") || !strings.Contains(p3, "change 59") {
		t.Error("the pages are not the newest first, in order")
	}
	if !strings.Contains(p1, "Page 1 of 3, 60 changes") || !strings.Contains(p1, `aria-current="page">Overview</a>`) {
		t.Errorf("the pager or the device tabs are missing:\n%s", p1)
	}
	if r, _ := e.do(t, "GET", "/devices/99/history", "operator", nil); r.StatusCode != 404 {
		t.Errorf("an unknown device: %d", r.StatusCode)
	}
}

// Why: "all transactions of this device" must be complete, which the fleet-wide list is not
// (it shows the newest 500 jobs of the fleet). The filter asks the store for this device.
func TestTransactionsListCanBeFilteredByDevice(t *testing.T) {
	jobs := []model.Job{
		{ID: 1, BatchID: 1, DeviceID: 1, State: model.JobSucceeded, TargetRelease: "16.0.0", CreatedBy: "adam", CreatedAt: time.Now()},
		{ID: 2, BatchID: 2, DeviceID: 2, State: model.JobSucceeded, TargetRelease: "16.0.0", CreatedBy: "adam", CreatedAt: time.Now()},
	}
	e := setupWith(t, func(d *Deps) { d.Jobs, d.OTA = &fakeJobs{jobs: jobs}, &fakeOTA{} })
	_, body := e.do(t, "GET", "/transactions?device=1", "operator", nil)
	if !strings.Contains(body, `href="/tf/transactions/1"`) || strings.Contains(body, `href="/tf/transactions/2"`) {
		t.Errorf("the list is not limited to device 1:\n%s", body)
	}
	if !strings.Contains(body, "kitchen") {
		t.Error("the page does not say whose transactions these are")
	}
}
