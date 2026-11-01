package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// barOf is the status bar of a page body.
func barOf(body string) string { return between(body, `<footer class="sb`, `</footer>`) }

// Why (#307): the bar is the one place that names the version and the live state, on every page.
// A page that came out of the shell without it would leave the version nowhere on screen.
func TestEveryShellPageHasTheStatusBar(t *testing.T) {
	e := setup(t)
	for _, p := range shellPages {
		resp, body := e.do(t, "GET", p, "operator", nil)
		if resp.StatusCode != 200 || !strings.Contains(body, `<main id="main">`) {
			continue
		}
		bar := barOf(body)
		if bar == "" || !strings.Contains(bar, `href="/tf/whats-new"`) || !strings.Contains(bar, "v-test") {
			t.Errorf("%s: no status bar with the version: %.200s", p, bar)
		}
	}
}

// Why (decision 1 of #307): version and live indicator moved, they are not shown twice. The phone
// drawer keeps its version link (it is not the top bar).
func TestTopBarNoLongerShowsVersionOrLiveIndicator(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	head := between(body, `<header class="top">`, `</header>`)
	for _, gone := range []string{`class="ver"`, `class="live"`, `live-dot`} {
		if strings.Contains(head, gone) {
			t.Errorf("the top bar still has %s", gone)
		}
	}
	if !strings.Contains(between(body, `<dialog id="nav-drawer"`, `</dialog>`), `class="ver"`) {
		t.Error("the drawer lost its version link")
	}
	if !strings.Contains(barOf(body), "Live") {
		t.Error("a live page's bar has no Live item")
	}
}

// Why: a page without a stream is a snapshot (no connection to claim "Live" for, no stream held
// open per tab); its bar then has no Live item and does not ask to be refreshed. A live page's
// bar refreshes itself on the events that change what it says, and on a resync.
func TestStatusBarRefreshesOnlyOnLivePages(t *testing.T) {
	e := setup(t)
	_, live := e.do(t, "GET", "/devices", "operator", nil)
	bar := barOf(live)
	for _, want := range []string{`hx-get="/tf/statusbar"`, `hx-trigger="sb-refresh"`, `sse-swap="job,backup,broker,resync"`, `hx-swap="none"`} {
		if !strings.Contains(bar, want) {
			t.Errorf("live page: bar lacks %q", want)
		}
	}
	if strings.Contains(bar, "sb-static") || strings.Contains(bar, "throttle") {
		t.Errorf("live bar is static or uses htmx throttle, which drops the last event of a burst: %s", bar)
	}
	_, still := e.do(t, "GET", "/users", "admin", nil)
	bar = barOf(still)
	if strings.Contains(still, "sse-connect") || strings.Contains(bar, "hx-get") || strings.Contains(bar, "sb-l-ok") {
		t.Errorf("a page without a stream claims one: %s", bar)
	}
}

// Why: the broker state is what only the server knows; a bar that hid a lost connection would be
// useless. It shows host and port, never more.
func TestStatusBarBrokerState(t *testing.T) {
	since := time.Date(2026, 10, 31, 14, 32, 0, 0, time.UTC)
	for _, c := range []struct {
		name     string
		info     BrokerInfo
		want     []string
		wantNone []string
	}{
		{"connected", BrokerInfo{Enabled: true, Connected: true, Host: "mqtt.lab.home:1883", Since: since},
			[]string{"<b>Broker</b>", "mqtt.lab.home:1883"}, []string{"Broker disconnected", "t-warn"}},
		{"down", BrokerInfo{Enabled: true, Host: "mqtt.lab.home:1883", Since: since},
			[]string{"<b>Broker disconnected</b>", "mqtt.lab.home:1883", "since", "t-warn", "sb-edge sb-warn", "Broker disconnected"}, nil},
		{"off", BrokerInfo{}, nil, []string{"Broker", "mqtt"}},
	} {
		e := setupWith(t, func(d *Deps) { d.MQTTStatus = func() BrokerInfo { return c.info } })
		_, body := e.do(t, "GET", "/devices", "operator", nil)
		bar := barOf(body)
		for _, w := range c.want {
			if !strings.Contains(bar, w) {
				t.Errorf("%s: bar lacks %q", c.name, w)
			}
		}
		for _, w := range c.wantNone {
			if strings.Contains(bar, w) {
				t.Errorf("%s: bar has %q", c.name, w)
			}
		}
	}
}

func barJobs(batches ...model.BatchID) []model.Job {
	var out []model.Job
	for i, b := range batches {
		out = append(out,
			model.Job{ID: model.JobID(10*i + 1), BatchID: b, DeviceID: 1, State: model.JobAwaitReboot, TargetRelease: "16.1.0", CreatedAt: time.Now()},
			model.Job{ID: model.JobID(10*i + 2), BatchID: b, DeviceID: 2, State: model.JobSucceeded, TargetRelease: "16.1.0", CreatedAt: time.Now()})
	}
	return out
}

// Why (decision 3 of #307): two running jobs fit the bar with name, progress and link; a third
// would push Backup and the version off a 1280 bar, so the rest is "+N more" leading to the list.
func TestStatusBarNamesTwoJobsAndCountsTheRest(t *testing.T) {
	e := setupWith(t, func(d *Deps) {
		d.Jobs, d.OTA = &fakeJobs{jobs: barJobs(5, 6, 7, 8)}, &fakeOTA{}
	})
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	bar := barOf(body)
	wide := between(bar, `id="sb-wide"`, `<details`)
	for _, want := range []string{"<b>Update</b> 1 of 2 devices", `href="/tf/transactions/5"`, "TX-0005", `href="/tf/transactions/6"`, "TX-0006", `href="/tf/transactions">+2 more<`, `<progress class="sb-prog" max="2" value="1" aria-label="Update progress">`} {
		if !strings.Contains(wide, want) {
			t.Errorf("wide bar lacks %q: %s", want, wide)
		}
	}
	if strings.Contains(wide, "TX-0007") || strings.Contains(wide, "TX-0008") {
		t.Errorf("more than two jobs are named: %s", wide)
	}
	if !strings.Contains(bar, "Update 1 of 2") || !strings.Contains(bar, "+ 3 more") {
		t.Errorf("the phone line does not say the worst job and the rest: %s", bar)
	}
}

// Why: a command shows its name but never its arguments (they may hold a secret); this is the
// same redaction the audit log uses.
func TestStatusBarCommandJobShowsNoArguments(t *testing.T) {
	jobs := []model.Job{
		{ID: 1, BatchID: 3, DeviceID: 1, Kind: model.KindCommand, Commands: "WifiPassword hunter2-secret", State: model.JobApplying, CreatedAt: time.Now()},
		{ID: 2, BatchID: 4, DeviceID: 1, Kind: model.KindCommand, Commands: "Restart 1", State: model.JobApplying, CreatedAt: time.Now()},
	}
	e := setupWith(t, func(d *Deps) { d.Jobs, d.OTA = &fakeJobs{jobs: jobs}, &fakeOTA{} })
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	if strings.Contains(body, "hunter2") {
		t.Fatal("a command argument reached the page")
	}
	if bar := barOf(body); !strings.Contains(bar, "<b>Command</b> WifiPassword 0 of 1") || !strings.Contains(bar, "<b>Command</b> Restart 0 of 1") {
		t.Errorf("command jobs are not named: %s", bar)
	}
	// the phone line names the single other job by its kind, as the mockup words it
	if bar := barOf(body); !strings.Contains(bar, "Command 0 of 1<small> + 1 command") {
		t.Errorf("the phone line does not name the other job: %s", bar)
	}
}

// Why: without updates there are no transaction pages; the job is still shown but must not link
// to a page that answers 404.
func TestStatusBarJobWithoutTransactionPagesHasNoLink(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.Jobs = &fakeJobs{jobs: barJobs(5)} })
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	bar := barOf(body)
	if !strings.Contains(bar, "<b>Update</b> 1 of 2 devices") || strings.Contains(bar, "/transactions") {
		t.Errorf("job without pages: %s", bar)
	}
}

// Why: a run that backed up 19 of 27 devices is not a failed run (#406). The bar must say how many
// devices are safe, use the warning colour for a partly failed run, and keep red and "Backup failed"
// for a run where nothing was backed up. The phone sheet words it the same way.
func TestStatusBarBackupRun(t *testing.T) {
	done := time.Date(2026, 10, 31, 3, 0, 0, 0, time.UTC)
	run := func(total, failed int) (bar string) {
		e := setupWith(t, func(d *Deps) {
			d.ScheduledBackup = func() BackupRunInfo { return BackupRunInfo{Finished: done, LastTotal: total, LastFailed: failed} }
		})
		_, body := e.do(t, "GET", "/devices", "operator", nil)
		return barOf(body)
	}
	check := func(name, bar string, want, wantNot []string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(bar, w) {
				t.Errorf("%s: bar lacks %q: %s", name, w, bar)
			}
		}
		for _, w := range wantNot {
			if strings.Contains(bar, w) {
				t.Errorf("%s: bar must not contain %q: %s", name, w, bar)
			}
		}
	}
	check("all ok", run(58, 0),
		[]string{"<b>Backup</b>", "58 of 58 backed up", "All good"},
		[]string{"t-bad", "t-warn", "sb-bad", "sb-warn", "failed"})
	check("partly failed", run(27, 8),
		[]string{"<b>Backup</b>", "19 of 27 backed up", "8 failed", "t-warn", "sb-edge sb-warn", "Backup partly failed"},
		[]string{"Backup failed", "t-bad", "sb-bad"})
	check("none ok", run(58, 58),
		[]string{"<b>Backup failed</b>", "0 of 58 backed up", "t-bad", "sb-edge sb-bad", "Last backup failed"},
		[]string{"t-warn"})
	e := setupWith(t, func(d *Deps) {
		d.ScheduledBackup = func() BackupRunInfo { return BackupRunInfo{Running: true, Done: 12, Total: 58} }
	})
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	if bar := barOf(body); !strings.Contains(bar, "<b>Backup</b> 12 of 58 devices") {
		t.Errorf("a running scheduled backup is not shown as a job: %s", bar)
	}
}

// Why: the refresh swaps only the insides of the wide row and of the phone details element, so
// the phone sheet a person has open does not snap shut on every job event.
func TestStatusBarFragmentSwapsInsidesOnly(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.Jobs, d.OTA = &fakeJobs{jobs: barJobs(5)}, &fakeOTA{} })
	resp, body := e.do(t, "GET", "/statusbar", "operator", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for _, want := range []string{`hx-swap-oob="innerHTML:#sb-wide"`, `hx-swap-oob="innerHTML:#sb-narrow"`, `id="sb-edge"`, "TX-0005"} {
		if !strings.Contains(body, want) {
			t.Errorf("fragment lacks %q: %s", want, body)
		}
	}
	if strings.Contains(body, "<footer") || strings.Contains(body, `<details id=`) {
		t.Errorf("the fragment replaces the footer or the details element itself: %s", body)
	}
}

// Why: the operator role must be able to open every page the bar links to, or the bar would offer
// 403s. The links are the transactions list, one transaction and What's new.
func TestStatusBarLinksOpenForTheOperator(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.Jobs, d.OTA = &fakeJobs{jobs: barJobs(5)}, &fakeOTA{} })
	for _, p := range []string{"/transactions", "/transactions/5", "/whats-new", "/statusbar"} {
		if resp, _ := e.do(t, "GET", p, "operator", nil); resp.StatusCode != 200 {
			t.Errorf("operator GET %s: %d", p, resp.StatusCode)
		}
	}
}

// Why: the broker state changes on the server and nowhere in the stream otherwise; the bar
// listens to the "broker" event, so the stream must carry it.
func TestStreamCarriesBrokerChanges(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/tf/events", nil)
	req.Header.Set("X-Test-Role", "operator")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	go func() {
		time.Sleep(50 * time.Millisecond)
		e.hub.Publish(events.Event{Topic: events.TopicBroker, Payload: events.BrokerState{}})
	}()
	buf := make([]byte, 4096)
	var got string
	for !strings.Contains(got, "event: broker") {
		n, err := resp.Body.Read(buf)
		got += string(buf[:n])
		if err != nil {
			t.Fatalf("stream ended without a broker event: %v (%q)", err, got)
		}
	}
}

// Why: a bar on a page without a stream is a snapshot. Its running-job dot must not pulse (it
// would claim live progress), and the job says when the numbers are from.
func TestStaticStatusBarSaysAsOfAndDoesNotPulse(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.Jobs, d.OTA = &fakeJobs{jobs: barJobs(5)}, &fakeOTA{} })
	_, body := e.do(t, "GET", "/users", "admin", nil)
	bar := barOf(body)
	if !strings.Contains(bar, `sb-static"`) || !strings.Contains(bar, "as of <time") {
		t.Errorf("static bar: %s", bar)
	}
	_, live := e.do(t, "GET", "/devices", "operator", nil)
	if strings.Contains(barOf(live), "as of <time data-ts") {
		t.Error("a live bar says 'as of'")
	}
}

// Why: a multi-day outage must read as one; "since 14:32" alone does not say which day.
func TestStatusBarBrokerSinceIsADayStamp(t *testing.T) {
	e := setupWith(t, func(d *Deps) {
		d.MQTTStatus = func() BrokerInfo {
			return BrokerInfo{Enabled: true, Host: "h:1883", Since: time.Now().Add(-72 * time.Hour)}
		}
	})
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	if bar := barOf(body); strings.Count(bar, `data-ts="day"`) < 2 {
		t.Errorf("broker since is not a day stamp: %s", bar)
	}
}
