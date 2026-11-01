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
	"github.com/nofuturekid/tasmofleet/internal/transport"
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

// Why: the overview is the place to read a device at a glance: what it draws, how it is
// connected, whether it follows the policy, then who it is. The network facts come from the
// newest backup snapshot (the live health has none of them), the energy from the meter's
// telemetry, and every value the pixel comparison cannot reproduce is marked for masking. A
// secret in the snapshot never reaches the page.
func TestDeviceOverviewShowsTheTilesFromRealData(t *testing.T) {
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
		d.Transport = fakeFacts{f: map[model.DeviceID]transport.Facts{1: {Now: time.Now(), HTTP: transport.HTTPState{State: transport.HTTPOK}}}}
	})
	e.bk.list = []model.Backup{{ID: 9, DeviceID: 1, Kind: model.BackupSnapshot, TakenAt: time.Now()}}
	e.bk.data = []byte(snapshotDoc)
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	for _, want := range []string{
		`<h2 id="dc-ident">Identity</h2>`, `<h2 id="dc-conn">Connection · Wi-Fi</h2>`, `<h2 id="dc-act">Activity</h2>`, `<h2 id="dc-relays">Relays</h2>`,
		`<dd class="mono" data-visual-mask>kitchen-lamp</dd>`, // hostname from the snapshot
		`<dd class="mono" data-visual-mask>10.0.0.254</dd>`,   // gateway
		`<dd class="mono" data-visual-mask>lab-iot</dd>`, `<dd class="mono" data-visual-mask>6 (2.4 GHz)</dd>`,
		`data-tile="power"`, `data-visual-mask>12 W</div>`, `data-visual-mask>0.02 kWh</div>`, `Yesterday 0.03 · Total 11.0`,
		`data-visual-mask>-61 dBm</div>`, `78 % · lab-iot · ch 6`,
		`>MQTT · HTTP</div>`, "Connected · Reachable",
		`data-tile="policy"`, `>1 / 2</div>`, "TelePeriod 60 → 300", "Make compliant…", `name="only" value="1"`,
		`Relay 1 <span class="mono">POWER</span>`, `aria-label="Relay 1 is On: switch it Off"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("overview lacks %q", want)
		}
	}
	if strings.Contains(body, "must-not-appear") || strings.Contains(body, "s3cret-pw") {
		t.Fatal("a secret reached the page")
	}
	if strings.Contains(body, "devhd-pwr") {
		t.Error("the header Power button is gone (D4): the relay box is the place to switch")
	}
	// An operator reads the same tiles but cannot start the fix.
	_, op := e.do(t, "GET", "/devices/1", "operator", nil)
	if strings.Contains(op, "Make compliant") || !strings.Contains(op, "TelePeriod 60 → 300") {
		t.Errorf("operator: the deviation without the admin action:\n%s", op)
	}
	fp.rep.Findings = nil
	_, ok := e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(ok, ">2 / 2</div>") || !strings.Contains(ok, "Matches all rules") || strings.Contains(ok, "Make compliant") {
		t.Errorf("a compliant device:\n%s", ok)
	}
}

// Why: an offline device must say so at the top of every tab, with since when, dim what it last
// reported and disable what needs the device; Edit and Delete work on the inventory record alone
// and stay, so does the Activity (history) (#145).
func TestOfflineDeviceShowsTheBannerDimsAndDisablesWhatNeedsIt(t *testing.T) {
	e := setupWith(t, func(d *Deps) {
		h := d.Health.(fakeHealth)
		h[1] = model.Health{DeviceID: 1, State: model.HealthOffline, LastSeen: time.Now().Add(-5 * time.Hour), Since: time.Now().Add(-5 * time.Hour), Power: map[string]string{"POWER": "OFF"}}
		d.DeviceCmd = &fakeCmd{}
	})
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	for _, want := range []string{`<div class="devhd-off" id="devhd-off" role="status">`, `<b>Offline since <time data-visual-mask`, `<span class="devhd-state t-danger">`,
		`role="switch" aria-checked="false" aria-label="Relay 1 is Off: switch it On" disabled>`,
		`id="devlive" class="devlive" data-off`, `data-needs-device disabled>Refresh identity`,
		`id="identity" class="devcard dev-dim is-dim" data-dim aria-disabled="true"`, `id="devconn" class="devcard dev-dim is-dim" data-dim aria-disabled="true"`,
		`<button type="button" class="btn" disabled>Restart</button>`} {
		if !strings.Contains(body, want) {
			t.Errorf("offline overview lacks %q", want)
		}
	}
	if strings.Contains(body, `<section id="devact" class="devcard dev-dim`) {
		t.Error("Activity is history: it must not be dimmed")
	}
	for _, want := range []string{`href="/tf/devices/1/edit"`, `href="/tf/devices/1/delete"`} {
		if !strings.Contains(body, want) {
			t.Errorf("Edit and Delete stay active offline: lacks %s", want)
		}
	}
	_, online := e.do(t, "GET", "/devices/2", "admin", nil)
	if strings.Contains(online, `id="devhd-off"`) {
		t.Error("a device that never answered is not offline: it gets the not-answered notice, not the banner")
	}
	if !strings.Contains(online, `<div id="devhd-slot"></div>`) {
		t.Error("the banner slot must always be on the page, so a live refresh can fill it")
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
	_, body := e.do(t, "GET", "/devices/1/settings/form", "admin", nil)
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

// Why: the overview is a summary: one Activity list with the newest 10 of state changes and
// transactions together, newest first, and a link to the complete, paged list.
func TestDeviceOverviewActivityIsTheNewest10OfBothKinds(t *testing.T) {
	var jobs []model.Job
	base := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC) // newer than every state change below
	for i := 14; i >= 1; i-- {                             // the store hands jobs out newest first
		jobs = append(jobs, model.Job{ID: model.JobID(i), BatchID: model.BatchID(i), DeviceID: 1, State: model.JobSucceeded, TargetRelease: "16.0.0", UpdatedAt: base.Add(time.Duration(i) * time.Hour)})
	}
	e := setupWith(t, func(d *Deps) {
		d.History = fakeHistory{stateChanges(25)}
		d.Jobs, d.OTA = &fakeJobs{jobs: jobs}, &fakeOTA{}
	})
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if n := strings.Count(body, `class="dev-act-at"`); n != 10 {
		t.Errorf("activity: %d rows, want the newest 10", n)
	}
	if !strings.Contains(body, "Transaction #14") || strings.Contains(body, "change 0") {
		t.Errorf("not the newest 10 merged newest first:\n%s", body)
	}
	if !strings.Contains(body, `href="/tf/devices/1/history">All activity`) {
		t.Error("no link to the complete list")
	}
}

// Why: state changes and transactions interleave by time, whichever kind is newer comes first.
func TestMergeActivityIsNewestFirstAcrossBothKinds(t *testing.T) {
	at := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	ev := []store.HealthEvent{{At: at.Add(-1 * time.Hour)}, {At: at.Add(-3 * time.Hour)}}
	jobs := []model.Job{{ID: 7, UpdatedAt: at}, {ID: 6, UpdatedAt: at.Add(-2 * time.Hour)}}
	var got []string
	for _, r := range mergeActivity(ev, jobs) {
		if r.Job != nil {
			got = append(got, "job"+strconv.Itoa(int(r.Job.ID)))
		} else {
			got = append(got, "event")
		}
	}
	if strings.Join(got, ",") != "job7,event,job6,event" {
		t.Fatalf("order %v", got)
	}
}

// Why: the complete list of a device's state changes needs a page of its own, with paging
// (a flapping device has thousands). Newest first, every change reachable, no row twice.
func TestDeviceActivityPagesThroughEveryEntry(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.History = fakeHistory{stateChanges(60)} })
	_, p1 := e.do(t, "GET", "/devices/1/history", "operator", nil)
	_, p3 := e.do(t, "GET", "/devices/1/history?page=3", "operator", nil)
	if a, b := strings.Count(p1, `class="dev-act-at"`), strings.Count(p3, `class="dev-act-at"`); a != 25 || b != 10 {
		t.Fatalf("page 1 has %d rows, page 3 has %d; want 25 and 10", a, b)
	}
	if !strings.Contains(p1, "change 0") || strings.Contains(p1, "change 25<") || !strings.Contains(p3, "change 59") {
		t.Error("the pages are not the newest first, in order")
	}
	if !strings.Contains(p1, "Page 1 of 3, 60 entries") || !strings.Contains(p1, `aria-current="page">Overview</a>`) {
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

// probedNet is what a probe stored on the device (Status 5 and 11).
var probedNet = model.NetFacts{Hostname: "tablet-charger", IP: "10.0.0.77", Gateway: "10.0.0.1", SSID: "iot-net", BSSID: "02:00:5E:00:00:09", Channel: 36,
	ReadAt: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}

// Why: maintainer feedback showed Hostname, Gateway, SSID, BSSID and Channel as "Unknown" on a device that had
// no backup snapshot. The facts a probe stored on the device are shown without any snapshot,
// and "Refresh identity" shows the hostname it just read.
func TestDeviceOverviewShowsProbedNetworkFactsWithoutSnapshot(t *testing.T) {
	e := setup(t)
	e.bk.list = nil // no backup at all
	d := e.inv.devs[1]
	d.Net = probedNet
	e.inv.devs[1] = d
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	for _, want := range []string{
		`<dd class="mono" data-visual-mask>tablet-charger</dd>`, `<dd class="mono" data-visual-mask>10.0.0.1</dd>`,
		`<dd class="mono" data-visual-mask>iot-net</dd>`, `<dd class="mono" data-visual-mask>02:00:5E:00:00:09</dd>`, `<dd class="mono" data-visual-mask>36 (5 GHz)</dd>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("overview lacks %q", want)
		}
	}
	_, probe := e.do(t, "POST", "/devices/1/probe", "admin", url.Values{"csrf": {"tok"}}, "HX-Request", "true")
	if !strings.Contains(probe, "tablet-charger") {
		t.Errorf("the refreshed identity card lacks the hostname:\n%s", probe)
	}
}

// Why: the row said "IP address" but showed the configured host (often a DNS name). The host
// the fleet uses is the "Address"; the real IP comes from the device (Status 5) as its own row.
func TestNetworkCardSeparatesTheConfiguredAddressFromTheDeviceIP(t *testing.T) {
	e := setup(t)
	d := e.inv.devs[1]
	d.Address, d.Net = "tablet-charger.example.internal", probedNet
	e.inv.devs[1] = d
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	for _, want := range []string{`<dt>Address</dt><dd class="mono" data-visual-mask>tablet-charger.example.internal</dd>`, `<dt>IP address</dt><dd class="mono" data-visual-mask>10.0.0.77</dd>`} {
		if !strings.Contains(body, want) {
			t.Errorf("network card lacks %q", want)
		}
	}
}

// Why: devices probed before this version, or never probed, have no stored facts; their
// newest backup snapshot still answers, so the page does not regress to "Unknown".
func TestDeviceOverviewFallsBackToTheSnapshotWhenNothingWasProbed(t *testing.T) {
	e := setup(t)
	e.bk.list = []model.Backup{{ID: 9, DeviceID: 1, Kind: model.BackupSnapshot, TakenAt: time.Now()}}
	e.bk.data = []byte(snapshotDoc)
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(body, `>kitchen-lamp</dd>`) || !strings.Contains(body, `>lab-iot</dd>`) {
		t.Errorf("snapshot facts missing:\n%s", body)
	}
}

// Why (issue #52): an Ethernet board showed "IP address 0.0.0.0", a Wi-Fi card full of
// "Unknown" and the Wi-Fi MAC. A wired device gets an Ethernet card without SSID or signal rows,
// its own (Ethernet) MAC, and the card says how it is connected.
func TestWiredDeviceShowsAnEthernetCardAndItsEthernetMAC(t *testing.T) {
	e := setup(t)
	d := e.inv.devs[1]
	d.MAC, d.MACs = "08:d1:f9:2b:e9:70", []string{"08:d1:f9:2b:e9:70", "08:d1:f9:2b:e9:71"}
	d.Net = model.NetFacts{Hostname: "tasmota-2BE970-eth", IP: "192.168.7.139", Gateway: "192.168.7.1", Wired: true, ReadAt: time.Now()}
	e.inv.devs[1] = d
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	for _, want := range []string{`<h2 id="dc-conn">Connection · Ethernet</h2>`, `<dt>Ethernet MAC</dt><dd class="mono" data-visual-mask>08:d1:f9:2b:e9:71</dd>`, `>192.168.7.1</dd>`, `data-tile="link"`, ">Wi-Fi off</div>"} {
		if !strings.Contains(body, want) {
			t.Errorf("overview lacks %q", want)
		}
	}
	for _, bad := range []string{"<dt>SSID</dt>", "<dt>Signal</dt>", "dev-bar", "0.0.0.0", `data-tile="wifi"`, "Connection · Wi-Fi"} {
		if strings.Contains(body, bad) {
			t.Errorf("wired device page still has %q", bad)
		}
	}
}

// Why (issue #52): "Address" and "IP address" showed the same value. The IP row appears only
// when the device reports something the Address row does not already say.
func TestNetworkCardDropsTheIPRowThatRepeatsTheAddress(t *testing.T) {
	e := setup(t)
	d := e.inv.devs[1]
	d.Address, d.Net = "10.0.0.77", probedNet // probedNet.IP is 10.0.0.77
	e.inv.devs[1] = d
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if strings.Contains(body, "<dt>IP address</dt>") || !strings.Contains(body, "<dt>Address</dt>") {
		t.Errorf("duplicate IP row:\n%s", body)
	}
}

// Why (issue #52): Tasmota calls it the device name, and that is what the user set on the device.
func TestIdentityCardCallsTheStoredNameDeviceName(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(body, "<dt>Device name</dt>") || strings.Contains(body, "<dt>Friendly name</dt>") {
		t.Errorf("identity card labels wrong")
	}
}

// Why (issue #52): a self-compiled build reports a bare tag. The Build row shows that name, while
// the variant stays unknown so OTA never overwrites a custom build without an explicit choice.
func TestBuildRowNamesASelfCompiledBuild(t *testing.T) {
	e := setup(t)
	d := e.inv.devs[1]
	d.Identity.Variant, d.Identity.Image = "unknown", "tasmota32"
	e.inv.devs[1] = d
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(body, "tasmota32 (self-compiled)") {
		t.Error("Build row does not name the self-compiled build")
	}
}

// Why (#126): "State changes" showed the stored UTC time while "Last restart" showed the server's
// zone, so the two columns of one page disagreed. Without JavaScript the fallback text must be
// in the server's zone, and the <time> element must carry the instant and the format key that
// time.js turns into the browser's own zone.
func TestStateChangesTimeIsServerZoneWithBrowserHook(t *testing.T) {
	old := time.Local
	time.Local = time.FixedZone("UTC+2", 2*60*60)
	t.Cleanup(func() { time.Local = old })
	at := time.Date(2026, 10, 22, 9, 41, 7, 0, time.UTC)
	e := setupWith(t, func(d *Deps) {
		d.History = fakeHistory{[]store.HealthEvent{{At: at, From: model.HealthOnline, To: model.HealthOffline, Source: model.SourceHTTP}}}
	})
	_, body := e.do(t, "GET", "/devices/1/history", "operator", nil)
	for _, want := range []string{`datetime="2026-10-22T09:41:07Z"`, `data-ts="datetime-s"`, `>2026-10-22 11:41:07</time>`} {
		if !strings.Contains(body, want) {
			t.Errorf("history page lacks %s", want)
		}
	}
}

// Why: one page must not name one backup at two different clock times. The delete text, the
// confirmation page's "Taken" fact and the storage table's cell all start from the stored UTC
// time and must all be in the server's zone (and say so to time.js).
func TestBackupTimesAreInOneZone(t *testing.T) {
	old := time.Local
	time.Local = time.FixedZone("UTC+2", 2*60*60)
	t.Cleanup(func() { time.Local = old })
	at := time.Date(2026, 10, 22, 22, 41, 7, 0, time.UTC) // 00:41 next day in UTC+2
	if got := (backupData{}).DeleteText(model.Backup{TakenAt: at}); !strings.Contains(got, "2026-10-23 00:41:07") {
		t.Errorf("delete text not in the server zone: %s", got)
	}
	if got := storeDate(at.AddDate(0, 0, 1), at); got != "23 Oct, 00:41" {
		t.Errorf("storage cell not in the server zone: %s", got)
	}
}

// Why (#127): the "Devices" crumb on a device tab must lead back to the list as the user left it
// (filter and sort). The server cannot know that (it is per browser tab), so it only marks the
// crumb for listback.js; the list itself and the other pages carry no marker, so the navigation's
// "Devices" link stays the way to an unfiltered list.
func TestDevicesCrumbIsMarkedOnDeviceTabsOnly(t *testing.T) {
	e := setup(t)
	const mark = `data-keep-query`
	for _, p := range []string{"/devices/1", "/devices/1/backups"} {
		_, body := e.do(t, "GET", p, "admin", nil)
		i := strings.Index(body, `aria-label="Breadcrumb"`)
		if i < 0 {
			t.Fatalf("%s has no crumbs", p)
		}
		nav := body[i : i+strings.Index(body[i:], "</nav>")]
		if !strings.Contains(nav, mark) || strings.Count(nav, mark) != 1 {
			t.Errorf("%s: the Devices crumb is not the one marked crumb: %s", p, nav)
		}
	}
	for _, p := range []string{"/devices", "/firmware", "/devices/1/edit"} {
		_, body := e.do(t, "GET", p, "admin", nil)
		if strings.Contains(body, mark) {
			t.Errorf("%s carries the marker; only device tabs may", p)
		}
	}
}

// Why (#127, CSP): the script that remembers the list's query is an external file, never inline.
func TestListbackScriptIsLoadedAsFile(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/devices", "admin", nil)
	if !strings.Contains(body, `listback.js`) {
		t.Error("listback.js is not loaded")
	}
}
