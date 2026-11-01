package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why (design v2, P2): "Unknown" means the device never answered; "Offline" means it answered
// and stopped. A device added with a wrong address fails its polls like a dead one, but calling
// it offline sends people looking for a power cut instead of a typo. Its identity is read on the
// first contact, so a device without one has never answered; one with an identity is offline.
func TestShownHealthKeepsNeverAnsweredDevicesUnknown(t *testing.T) {
	off := model.Health{State: model.HealthOffline}
	if got := shownHealth(model.Device{}, off).State; got != model.HealthUnknown {
		t.Errorf("never answered, polls failed: %s, want unknown", got)
	}
	known := model.Device{Identity: model.Identity{Family: model.ChipESP32, Version: "14.3.0"}}
	if got := shownHealth(known, off).State; got != model.HealthOffline {
		t.Errorf("answered once, then stopped: %s, want offline", got)
	}
	seen := model.Health{State: model.HealthOffline, LastSeen: time.Now()}
	if got := shownHealth(model.Device{}, seen).State; got != model.HealthOffline {
		t.Errorf("heard from in this run: %s, want offline", got)
	}
	if got := shownHealth(model.Device{}, model.Health{State: model.HealthOnline}).State; got != model.HealthOnline {
		t.Errorf("online stays online: %s", got)
	}
}

// Why (#92): after a restart the tracker has no contact time for an offline device, but its
// identity carries the time of the last successful read. Every page must show that, not "-" on
// one page and a different time on the next.
func TestOfflineDeviceShowsItsLastSuccessfulContactEverywhere(t *testing.T) {
	detected := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	d := model.Device{Identity: model.Identity{Version: "14.3.0", DetectedAt: detected}}
	if got := shownHealth(d, model.Health{State: model.HealthOffline}).LastSeen; !got.Equal(detected) {
		t.Errorf("list: last seen %v, want %v", got, detected)
	}
	live := detected.Add(time.Hour)
	if got := shownHealth(d, model.Health{State: model.HealthOffline, LastSeen: live}).LastSeen; !got.Equal(live) {
		t.Errorf("a contact seen in this run wins: %v, want %v", got, live)
	}
	if got := lastContact(model.Device{}, model.Health{State: model.HealthOffline}); !got.IsZero() {
		t.Errorf("never answered: %v, want zero", got)
	}
}

// Why: a device that never answered is no longer called Offline, but it still cannot be updated:
// the update picker must keep refusing it with a reason (its hardware is not known either).
func TestNeverAnsweredDeviceIsBlockedInTheUpdatePicker(t *testing.T) {
	if got := blockedReason(model.Device{}, model.HealthUnknown); got != "Never answered" {
		t.Errorf("never answered: %q", got)
	}
	known := model.Device{Identity: model.Identity{Family: model.ChipESP32, Version: "14.3.0"}}
	if got := blockedReason(known, model.HealthUnknown); got != "" {
		t.Errorf("identified, state not known yet (just started): %q, want selectable", got)
	}
}

// Why: the list, its counts and the live updates must agree, or a never-answered device would
// read "Unknown" on load and flip to "Offline" with the next event.
func TestDeviceListCountsNeverAnsweredAsUnknown(t *testing.T) {
	e := setup(t)
	e.inv.devs[3] = model.Device{ID: 3, Name: "typo", Address: "10.0.0.99"} // no identity: never answered
	e.health[3] = model.Health{DeviceID: 3, State: model.HealthOffline}
	e.health[2] = model.Health{DeviceID: 2, State: model.HealthOffline, LastSeen: time.Now().Add(-time.Hour)}
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	if !strings.Contains(body, "1 online · 1 offline · 1 unknown") {
		t.Errorf("subtitle counts wrong: %s", between(body, `class="pghead-sub"`, "</p>"))
	}
	ev := streamUntil(t, e, events.Event{Topic: events.TopicHealth, Payload: e.health[3]}, "event: wifi-3")
	if !strings.Contains(ev, "event: health-3") || !strings.Contains(strings.SplitN(ev, "event: health-3", 2)[1], "Unknown</span>") {
		t.Errorf("the live update must say Unknown too:\n%s", ev)
	}
	if !strings.Contains(ev, "event: reach-3") {
		t.Errorf("the MQTT · HTTP column must be live as well:\n%s", ev)
	}
}

// streamUntil opens the event stream, publishes ev until the stream has sent until, and returns
// what it read.
func streamUntil(t *testing.T, e *env, ev events.Event, until string) string {
	t.Helper()
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
		for ctx.Err() == nil {
			e.hub.Publish(ev)
			time.Sleep(20 * time.Millisecond)
		}
	}()
	buf := make([]byte, 4096)
	var got string
	for !strings.Contains(got, until) {
		n, err := resp.Body.Read(buf)
		got += string(buf[:n])
		if err != nil {
			t.Fatalf("stream ended: %v (%q)", err, got)
		}
	}
	return got
}

// Why: the filter chips say how many devices each would show before the click (N7 counts), the
// Chip and Tag selects list what the fleet has with its count, and a chip keeps the search and
// the selects (one filter must not silently drop another).
func TestDeviceListToolbarOffersCountedFilters(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/devices?q=a&tag=kitchen", "operator", nil)
	for _, want := range []string{
		`>All<span class="dv-n">`, `>Offline / unknown<span class="dv-n">`, `>Outdated<span class="dv-n">`, `>Not compliant<span class="dv-n">`,
		`<option value="esp32">ESP32 · 1</option>`, `<option value="esp8266">ESP8266 · 1</option>`,
		`<option value="kitchen" selected>kitchen · 1</option>`,
		// the search field is short (the toolbar must fit one row at 1280px, #428), so the placeholder is
		// brief and the full hint stays in the title and the description
		`placeholder="Filter devices"`, `title="Filter by name, IP, chip or firmware"`, `aria-describedby="dv-q-help"`,
		`<span id="dv-q-help" class="sr-only">Filter by name, IP, chip or firmware</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("toolbar: missing %q", want)
		}
	}
	if !strings.Contains(body, `href="/tf/devices?chip=down&amp;dir=asc&amp;q=a&amp;sort=name&amp;tag=kitchen"`) {
		t.Errorf("the Offline chip must keep the search, the tag and the sort: %s", between(body, `class="dv-chips"`, `class="dv-vsep"`))
	}
	if !strings.Contains(body, `<a class="dv-chip" href="/tf/devices?dir=asc&amp;q=a&amp;sort=name&amp;tag=kitchen" aria-current="true">All`) {
		t.Error("the chip in effect must be marked")
	}
}

// Why: an offline device's uptime is stale; the Uptime column says when it was last heard from
// instead, compact like every relative time in the lists ("seen 3h ago"; design v2). This replaces
// the last-seen age the Status column used to carry.
func TestOfflineDeviceShowsWhenItWasLastSeen(t *testing.T) {
	e := setup(t)
	e.health[2] = model.Health{DeviceID: 2, State: model.HealthOffline, LastSeen: time.Now().Add(-3*time.Hour - time.Minute), UptimeSec: 999}
	_, body := e.do(t, "GET", "/devices/table", "operator", nil)
	if !regexp.MustCompile(`<span class="dv-seen" title="Last seen [^"]+">seen 3h ago</span>`).MatchString(body) {
		t.Errorf("offline device: no compact last-seen time in the Uptime column:\n%s", between(body, `sse-swap="uptime-2"`, "</td>"))
	}
	if !strings.Contains(between(body, `sse-swap="uptime-1"`, "</td>"), "1h 1m") {
		t.Errorf("an online device keeps its uptime: %s", between(body, `sse-swap="uptime-1"`, "</td>"))
	}
}

// Why: the chip label is how people write chips ("ESP32-C3"), the filter value stays the
// family id the inventory stores.
func TestChipLabel(t *testing.T) {
	for in, want := range map[model.ChipFamily]string{model.ChipESP8266: "ESP8266", model.ChipESP32: "ESP32", model.ChipESP32C3: "ESP32-C3", model.ChipESP32C61: "ESP32-C61", model.ChipESP32S3: "ESP32-S3"} {
		if got := chipLabel(in); got != want {
			t.Errorf("chipLabel(%s) = %q, want %q", in, got, want)
		}
	}
}

// Why: "Back up now" in a row menu answers with a toast, like the prototype ("Backup of Attic
// Dimmer saved"); a failure stays until dismissed (danger tone). Nothing is swapped into the
// page, so the list does not jump.
func TestBackupFromListAnswersWithAToast(t *testing.T) {
	e := setup(t)
	resp, body := e.do(t, "POST", "/devices/1/backup", "operator", url.Values{"from": {"list"}}, "HX-Request", "true")
	if resp.StatusCode != 200 || strings.TrimSpace(body) != "" {
		t.Fatalf("status %d, body %q", resp.StatusCode, body)
	}
	var trig struct{ Toast struct{ Text, Tone string } }
	if err := json.Unmarshal([]byte(resp.Header.Get("HX-Trigger")), &trig); err != nil {
		t.Fatalf("HX-Trigger %q: %v", resp.Header.Get("HX-Trigger"), err)
	}
	if trig.Toast.Text != "Backup of kitchen saved" || trig.Toast.Tone != "ok" {
		t.Errorf("toast %+v", trig.Toast)
	}
	e.bk.failErr = errors.New("device unreachable")
	resp, _ = e.do(t, "POST", "/devices/2/backup", "operator", url.Values{"from": {"list"}}, "HX-Request", "true")
	_ = json.Unmarshal([]byte(resp.Header.Get("HX-Trigger")), &trig)
	if trig.Toast.Tone != "danger" || !strings.HasPrefix(trig.Toast.Text, "Backup of attic failed: ") {
		t.Errorf("failure toast %+v", trig.Toast)
	}
}

// Why: a device change made within 2 s of another (a rename right after an import, two quick
// edits) must still reach an open list. A throttle drops it, and adding a delay for the same
// event on the same element does not bring it back (htmx handles an event once per element),
// so the list waits for the end of a burst and reloads once.
func TestDeviceListReloadsAfterTheLastChangeOfABurst(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	trig := regexp.MustCompile(`id="devtable"[^>]*hx-trigger="([^"]*)"`).FindStringSubmatch(body)
	if trig == nil {
		t.Fatalf("no #devtable trigger in %s", between(body, `id="devtable"`, ">"))
	}
	if !regexp.MustCompile(`(^|, )sse:device delay:\d+m?s($|,)`).MatchString(trig[1]) || strings.Contains(trig[1], "throttle") {
		t.Errorf("hx-trigger %q: want a delay on sse:device and no throttle", trig[1])
	}
}

// Why: /devices?health=offline (a dashboard link) narrows the list, but no fixed chip stands for
// it. "All" must not look active then (QA: "All 28" was selected over an empty list); the active
// filter shows as its own selected chip, and every other chip stays unselected.
func TestDeviceListHealthFilterHasItsOwnActiveChip(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/devices?health=offline", "operator", nil)
	chips := between(body, `class="dv-chips"`, `class="dv-vsep"`)
	if strings.Contains(chips, `aria-current="true">All`) {
		t.Errorf("All is marked while health=offline narrows the list: %s", chips)
	}
	if n := strings.Count(chips, `aria-current="true"`); n != 1 || !strings.Contains(chips, `aria-current="true" title="Show all states again">Offline<span class="dv-n">0</span>`) {
		t.Errorf("want exactly the Offline chip marked: %s", chips)
	}
	_, body = e.do(t, "GET", "/devices", "operator", nil)
	if strings.Contains(between(body, `class="dv-chips"`, `class="dv-vsep"`), "Show all states again") {
		t.Error("the health chip shows without a health filter")
	}
}

// Why: a dashboard tile opens the list narrowed (?health=online, ?chip=down, ...). "All" is the
// way back to the whole fleet from any entry path (v0.11.1 feedback 5: from the Online tile, All
// kept ?health=online and the list stayed narrowed). So the All link carries neither a health nor
// a chip filter, and no chip link keeps a health filter: the chips and the health filter are one
// group of status filters, picking a chip replaces the health filter instead of intersecting it.
// The search, the selects and the sort are not status filters and stay.
func TestDeviceListAllChipClearsEveryStatusFilter(t *testing.T) {
	e := setup(t)
	for _, q := range []string{"health=online", "health=offline", "health=unknown", "chip=down", "chip=outdated", "chip=noncompliant", "health=online&chip=down"} {
		_, body := e.do(t, "GET", "/devices?"+q+"&tag=kitchen", "operator", nil)
		chips := between(body, `class="dv-chips"`, `class="dv-vsep"`)
		hrefs := regexp.MustCompile(`<a class="dv-chip" href="([^"]*)"[^>]*>([^<]*)<`).FindAllStringSubmatch(chips, -1)
		if len(hrefs) == 0 {
			t.Fatalf("%s: no chips in %s", q, chips)
		}
		sawAll := false
		for _, h := range hrefs {
			href := strings.ReplaceAll(h[1], "&amp;", "&")
			if h[2] == "All" {
				sawAll = true
				if strings.Contains(href, "chip=") || strings.Contains(href, "health=") {
					t.Errorf("%s: All links to %q, which keeps a status filter", q, href)
				}
				if !strings.Contains(href, "tag=kitchen") {
					t.Errorf("%s: All links to %q and drops the tag select", q, href)
				}
			}
			// The Online chip is itself the health filter (health=online), so it is the one chip
			// that may carry it; every other chip must drop it.
			if strings.Contains(href, "health=") && h[2] != "Online" {
				t.Errorf("%s: chip %q links to %q, which keeps the health filter", q, h[2], href)
			}
		}
		if !sawAll {
			t.Errorf("%s: no All chip in %s", q, chips)
		}
	}
}

// Why: Online used to exist only while ?health=online was active (#102), so after switching to
// All the filter could not be picked again, unlike Offline / unknown, Outdated and Not compliant.
// It is a fixed chip: always there with its count, marked only while it filters, and clicking it
// while active drops the filter again.
func TestDeviceListOnlineChipIsAlwaysRendered(t *testing.T) {
	e := setup(t)
	online := regexp.MustCompile(`<a class="dv-chip" href="([^"]*)"([^>]*)>Online<span class="dv-n">(\d+)</span></a>`)
	for _, q := range []string{"", "?chip=down", "?chip=outdated", "?health=offline"} {
		_, body := e.do(t, "GET", "/devices"+q, "operator", nil)
		m := online.FindStringSubmatch(between(body, `class="dv-chips"`, `class="dv-vsep"`))
		if m == nil {
			t.Fatalf("%q: no Online chip among the chips: %s", q, between(body, `class="dv-chips"`, `class="dv-vsep"`))
		}
		if !strings.Contains(m[1], "health=online") || strings.Contains(m[1], "chip=") || strings.Contains(m[2], "aria-current") {
			t.Errorf("%q: inactive Online chip links to %q (%s): want the health=online filter, no other status filter, not marked", q, m[1], m[2])
		}
	}
	_, body := e.do(t, "GET", "/devices?health=online", "operator", nil)
	chips := between(body, `class="dv-chips"`, `class="dv-vsep"`)
	m := online.FindStringSubmatch(chips)
	if m == nil || !strings.Contains(m[2], `aria-current="true"`) || strings.Contains(m[1], "health=") {
		t.Errorf("health=online: want the Online chip marked and linking back to the unfiltered list: %s", chips)
	}
	if n := strings.Count(chips, `aria-current="true"`); n != 1 || strings.Count(chips, ">Online<") != 1 {
		t.Errorf("health=online: want exactly one marked chip and one Online chip: %s", chips)
	}
}

// Why: Online is the healthy state and reads together with All; the problem chips (Offline /
// unknown, Outdated, Not compliant) follow it (maintainer's request with #116).
func TestDeviceListOnlineChipSitsRightAfterAll(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	chips := between(body, `class="dv-chips"`, `class="dv-vsep"`)
	pos := func(label string) int { return strings.Index(chips, ">"+label+"<span") }
	all, online, down := pos("All"), pos("Online"), pos("Offline / unknown")
	if all < 0 || online < 0 || down < 0 || all >= online || online >= down {
		t.Errorf("want the order All, Online, Offline / unknown (positions %d, %d, %d): %s", all, online, down, chips)
	}
	if o, out, nc := online, pos("Outdated"), pos("Not compliant"); o >= out || out >= nc {
		t.Errorf("want Online before Outdated before Not compliant: %s", chips)
	}
}

// Why (#239): OTA URL and Group topic are optional columns: in the table for everyone (the
// column chooser hides them by default), read-only, from what the record already holds. A long
// URL must not stretch the table, so the full value is the cell's title; the age of the reading
// is a <time> that time.js shows in the browser's zone, and a value without an age shows none.
func TestDeviceListHasOtaURLAndGroupTopicColumns(t *testing.T) {
	e := setup(t)
	long := "http://ota.example.internal/firmware/with/a/very/long/path/that/would/stretch/the/table/tasmota32s3.bin.gz"
	d := e.inv.devs[1]
	d.OTAURL, d.OTAURLReadAt = long, time.Date(2026, 10, 30, 8, 15, 0, 0, time.UTC)
	d.GroupTopic = "tasmotas"
	e.inv.devs[1] = d
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	for _, want := range []string{`sort=otaurl`, `sort=grouptopic`} {
		if !strings.Contains(body, want) {
			t.Errorf("header %s missing", want)
		}
	}
	cell := func(label string, has string) string {
		for _, part := range strings.Split(body, `data-label="`+label+`"`)[1:] {
			c, _, _ := strings.Cut(part, "</td>")
			if strings.Contains(c, has) {
				return c
			}
		}
		return ""
	}
	ota := cell("OTA URL", "ota.example.internal")
	if !strings.Contains(ota, `title="`+long+`"`) || !strings.Contains(ota, `datetime="2026-10-30T08:15:00Z"`) || !strings.Contains(ota, `data-ts=`) {
		t.Errorf("OTA URL cell lacks the full value as title or the read time:\n%s", ota)
	}
	gt := cell("Group topic", "tasmotas")
	if gt == "" || strings.Contains(gt, "<time") {
		t.Errorf("a group topic without an age must show the value and no time:\n%s", gt)
	}
	if cell("OTA URL", ">-</span>") == "" || cell("Group topic", ">-</span>") == "" {
		t.Error("a device without a value shows a dash")
	}
}
