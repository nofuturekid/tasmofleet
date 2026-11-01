package web

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/discovery"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// listFixture is three new devices whose order differs for every sort key, one of them offline.
func listFixture(t *testing.T) (*env, *fakeDiscovery) {
	t.Helper()
	e, fd := setupDiscovery(t)
	now := time.Now()
	mk := func(id int64, name, topic, addr, mac, via, lwt string, first, seen time.Duration) discovery.Proposal {
		return discovery.Proposal{ID: id, Version: fmt.Sprintf("v%d", id), Kind: discovery.KindNew, Via: via, LWT: lwt,
			FirstSeen: now.Add(-first), SeenAt: now.Add(-seen),
			Device: model.Device{Name: name, Topic: topic, Address: addr, MAC: mac, Net: model.NetFacts{Hostname: topic}}}
	}
	fd.props = []discovery.Proposal{
		mk(1, "Charlie", "topic-ccc", "10.0.0.9", "AA:00:00:00:00:02", "mqtt", "Online", 3*time.Hour, 5*time.Minute),
		mk(2, "alpha", "topic-bbb", "10.0.0.10", "AA:00:00:00:00:03", "scan", "Offline", 1*time.Hour, 1*time.Minute),
		mk(3, "", "topic-aaa", "10.0.0.2", "AA:00:00:00:00:01", "mqtt", "Offline", 2*time.Hour, 9*time.Minute),
	}
	return e, fd
}

// tableOrder lists the topics (the name cell's tooltip) of the desktop table in the order shown.
func tableOrder(t *testing.T, body string) []string {
	t.Helper()
	i, j := strings.Index(body, "<tbody>"), strings.Index(body, "</tbody>")
	if i < 0 || j < i {
		t.Fatal("no table body")
	}
	var out []string
	for _, m := range regexp.MustCompile(`topic-[a-z]+`).FindAllString(body[i:j], -1) {
		if len(out) == 0 || out[len(out)-1] != m {
			out = append(out, m)
		}
	}
	return out
}

func joined(s []string) string { return strings.Join(s, ",") }

// Why: #130, with 40 devices on the broker an admin must tell the live ones from the stale ones;
// Offline is the MQTT last-will, so Online is everything that is not offline.
func TestNewDevicesFilterByState(t *testing.T) {
	e, _ := listFixture(t)
	for state, want := range map[string]string{
		"":        "topic-aaa,topic-bbb,topic-ccc",
		"all":     "topic-aaa,topic-bbb,topic-ccc",
		"online":  "topic-ccc",
		"offline": "topic-aaa,topic-bbb",
		"bogus":   "topic-aaa,topic-bbb,topic-ccc", // an unknown state shows everything, no 500
	} {
		_, body := e.do(t, "GET", "/devices/new?state="+state+"&sort=name&dir=asc", "admin", nil)
		if got := joined(tableOrder(t, body)); got != want {
			t.Errorf("state %q: rows %s, want %s", state, got, want)
		}
	}
	_, body := e.do(t, "GET", "/devices/new", "admin", nil)
	for _, want := range []string{`>All<span class="add-chip-n">3</span>`, `>Online<span class="add-chip-n">1</span>`, `>Offline<span class="add-chip-n">2</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("chips lack %q", want)
		}
	}
	// the chips and the ignored switch keep the other parameters
	_, body = e.do(t, "GET", "/devices/new?state=offline&sort=ip&dir=desc", "admin", nil)
	if !strings.Contains(body, "ignored=1") || !strings.Contains(body, "state=online") || !strings.Contains(body, "sort=ip") {
		t.Error("links lose the filter or sort")
	}
}

// Why: a filter that hides every device must say so and keep the chips, or the page looks broken.
func TestNewDevicesFilterWithoutMatchKeepsChips(t *testing.T) {
	e, fd := listFixture(t)
	fd.props = fd.props[1:] // only offline devices left
	_, body := e.do(t, "GET", "/devices/new?state=online", "admin", nil)
	if !strings.Contains(body, "No online devices") || !strings.Contains(body, `class="add-chips"`) {
		t.Error("an empty filter result shows no message or no chips")
	}
}

// Why: #130, headers sort like the device list; addresses sort numerically (10.0.0.9 < 10.0.0.10),
// not as text, and both directions work for every key.
func TestNewDevicesSortEveryKey(t *testing.T) {
	e, _ := listFixture(t)
	asc := map[string]string{
		"name":   "topic-aaa,topic-bbb,topic-ccc", // no name first, then alpha, Charlie (case-blind)
		"ip":     "topic-aaa,topic-ccc,topic-bbb",
		"source": "topic-ccc,topic-aaa,topic-bbb", // MQTT (id 1, 3) before Scan; ties by ID
		"last":   "topic-aaa,topic-ccc,topic-bbb", // 9 min, 5 min, 1 min ago
	}
	for key, want := range asc {
		_, body := e.do(t, "GET", "/devices/new?sort="+key+"&dir=asc", "admin", nil)
		if got := joined(tableOrder(t, body)); got != want {
			t.Errorf("%s asc: %s, want %s", key, got, want)
		}
	}
	// descending reverses, except that equal values keep the ID order (stable, no flicker)
	desc := map[string]string{
		"name":   "topic-ccc,topic-bbb,topic-aaa",
		"ip":     "topic-bbb,topic-ccc,topic-aaa",
		"source": "topic-bbb,topic-ccc,topic-aaa",
		"last":   "topic-bbb,topic-ccc,topic-aaa",
	}
	for key, want := range desc {
		_, body := e.do(t, "GET", "/devices/new?sort="+key+"&dir=desc", "admin", nil)
		if got := joined(tableOrder(t, body)); got != want {
			t.Errorf("%s desc: %s, want %s", key, got, want)
		}
	}
}

// Why: the default is what an admin looks for first, the newest announcement; a mistyped or
// hostile sort parameter must fall back to it instead of failing.
func TestNewDevicesDefaultSortAndUnknownKey(t *testing.T) {
	e, _ := listFixture(t)
	want := "topic-bbb,topic-ccc,topic-aaa"
	for _, q := range []string{"", "?sort=bogus", "?sort=bogus&dir=asc", "?sort=;drop&dir=x", "?dir=asc"} {
		r, body := e.do(t, "GET", "/devices/new"+q, "admin", nil)
		if r.StatusCode != 200 {
			t.Fatalf("%q: status %d", q, r.StatusCode)
		}
		if got := joined(tableOrder(t, body)); got != want {
			t.Errorf("%q: %s, want %s", q, got, want)
		}
	}
}

// Why: the current column's header flips its direction, the others start ascending, and the
// header says which way it is sorted for screen readers.
func TestNewDevicesHeadersAreSortLinks(t *testing.T) {
	e, _ := listFixture(t)
	_, body := e.do(t, "GET", "/devices/new?sort=ip&dir=asc", "admin", nil)
	for _, want := range []string{
		`aria-sort="ascending"><a class="sortlink is-asc" href="/tf/devices/new?dir=desc&amp;sort=ip">IP`,
		`<a class="sortlink" href="/tf/devices/new?dir=asc&amp;sort=source">Source`,
		`<a class="sortlink" href="/tf/devices/new?dir=asc&amp;sort=name">Device name`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("header lacks %q", want)
		}
	}
}

// Why: #130, Model and Firmware were never filled (a column of dashes). #410: the table sits in the
// two-thirds box of the Add page, so MAC, Hostname and First seen are no columns any more; the phone
// card keeps the hostname and the name cell's tooltip the MQTT topic.
func TestNewDevicesTableKeepsOnlyWhatFitsTheBox(t *testing.T) {
	e, _ := listFixture(t)
	_, body := e.do(t, "GET", "/devices/new", "admin", nil)
	i := strings.Index(body, "add-disc-table")
	head := body[i : i+strings.Index(body[i:], "</thead>")]
	for _, gone := range []string{">Model<", ">Firmware<", "Name / hostname", ">Hostname<", ">MAC<", ">First seen<"} {
		if strings.Contains(head, gone) {
			t.Errorf("table still has %q", gone)
		}
	}
	for _, want := range []string{">Device name<span", ">IP<span", ">Source<span", ">Last seen<span"} {
		if !strings.Contains(head, want) {
			t.Errorf("table lacks %q", want)
		}
	}
	// a device without a friendly name shows a dash in its name column
	// (the MQTT topic is no column: it is the name cell's tooltip, and never shown as the hostname)
	if !strings.Contains(body, `<td class="name" title="— · topic topic-aaa">`) {
		t.Error("an unnamed device shows no dash, or its topic is lost")
	}
	if !strings.Contains(body, `class="add-card-meta"><span data-visual-mask>10.0.0.2</span> · topic-aaa`) {
		t.Error("the phone card lost its hostname line")
	}
}

// Why: four devices need no filter (#410); the chips come back when the list holds both online and
// offline devices, or while a filter is chosen (so it can be left again).
func TestNewDevicesChipsOnlyWhenThereIsSomethingToFilter(t *testing.T) {
	e, fd := listFixture(t)
	fd.props = fd.props[:1] // one online device
	_, body := e.do(t, "GET", "/devices/new", "admin", nil)
	if strings.Contains(body, `class="add-chips"`) {
		t.Error("chips for a list of one state")
	}
	_, body = e.do(t, "GET", "/devices/new?state=offline", "admin", nil)
	if !strings.Contains(body, `class="add-chips"`) {
		t.Error("a chosen filter without chips cannot be left")
	}
}

// Why: ticking rows and "Add selected" is the point of the page; sorting and filtering must not
// change what a row's checkbox submits.
func TestNewDevicesSelectionSurvivesSortAndFilter(t *testing.T) {
	e, _ := listFixture(t)
	_, body := e.do(t, "GET", "/devices/new?state=offline&sort=name&dir=desc", "admin", nil)
	for _, want := range []string{`value="2:v2"`, `value="3:v3"`} {
		if !strings.Contains(body, want) {
			t.Errorf("lacks %q", want)
		}
	}
	if strings.Contains(body, `value="1:v1"`) {
		t.Error("the online device is still selectable under the offline filter")
	}
	if !strings.Contains(body, `action="/tf/discovery/proposals/accept"`) {
		t.Error("the bulk form lost its action")
	}
}

// Why: after Add, Ignore or Restore the admin is back on the list as they left it (filter,
// sort, ignored switch), not on the default view; the page carries them in hidden fields.
func TestNewDevicesActionsKeepStateAndSort(t *testing.T) {
	e, _ := listFixture(t)
	_, body := e.do(t, "GET", "/devices/new?state=offline&sort=ip&dir=desc&ignored=1", "admin", nil)
	for _, want := range []string{`name="state" value="offline"`, `name="sort" value="ip"`, `name="dir" value="desc"`, `name="ignored" value="1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("bulk form lacks hidden %s", want)
		}
	}
	form := url.Values{"state": {"offline"}, "sort": {"ip"}, "dir": {"desc"}, "ignored": {"1"}}
	for _, path := range []string{"/discovery/proposals/1/dismiss", "/discovery/proposals/1/undismiss"} {
		r, _ := e.do(t, "POST", path, "admin", form)
		loc := r.Header.Get("Location")
		for _, want := range []string{"state=offline", "sort=ip", "dir=desc", "ignored=1", "ok="} {
			if !strings.Contains(loc, want) {
				t.Errorf("%s redirects to %q, lacking %s", path, loc, want)
			}
		}
	}
	// the default view needs no parameters
	if r, _ := e.do(t, "POST", "/discovery/proposals/1/dismiss", "admin", nil); r.Header.Get("Location") != "/tf/devices/new?ok=dismissed" {
		t.Errorf("default redirect %q", r.Header.Get("Location"))
	}
}
