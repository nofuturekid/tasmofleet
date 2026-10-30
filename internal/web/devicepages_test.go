package web

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/restore"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// subPageEnv has every device page: Settings, Console, Backups, Replace.
func subPageEnv(t *testing.T) (*env, *adminBackups, *replRestore) {
	t.Helper()
	ab := &adminBackups{}
	t0 := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	ab.list = []model.Backup{{ID: 4, DeviceID: 1, Kind: model.BackupConfigDump, TakenAt: t0, Version: "15.6.0", Size: 4096}}
	rr := &replRestore{}
	e := setupWith(t, func(d *Deps) {
		d.Backups, d.Restore = ab, rr
		d.Settings = &fakeSettings{values: map[string]string{"TelePeriod": "300"}}
		d.DeviceCmd = &fakeCmd{reply: `{"Status":{}}`}
	})
	return e, ab, rr
}

// Why: the same device must look the same on every tab (name, state, the device
// buttons, the tabs with Replace device, the trail), otherwise the tabs read as
// different pages and the state and the Replace tab vanish as soon as you leave
// the Overview. The tab you are on is the current one (design v2: the trail
// names the device on every tab, the tab row names the section). Delete leads to
// the delete page (N2): a dialog could not carry the backups and the choice.
func TestDeviceHeaderIsTheSameOnEveryTab(t *testing.T) {
	e, _, _ := subPageEnv(t)
	for _, c := range []struct{ path, section string }{
		{"/devices/1", "Overview"}, {"/devices/1/settings", "Settings"}, {"/devices/1/console", "Console"}, {"/devices/1/backups", "Backups"},
	} {
		resp, body := e.do(t, "GET", c.path, "admin", nil)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", c.path, resp.StatusCode)
		}
		for _, want := range []string{`<h1>kitchen</h1><span class="devhd-state t-ok">`, `href="/tf/devices/1/replace">Replace device</a>`, `<nav class="utabs" aria-label="Device pages">`,
			`aria-current="page">` + c.section + `</a>`, `<span class="crumb-here" aria-current="page">kitchen</span>`, `data-dialog="dlg-restart"`, `>Back up</button>`, `href="/tf/devices/1/edit">Edit</a>`,
			`<a class="btn btn-danger" href="/tf/devices/1/delete">Delete</a>`} { // N2: a plain link to the delete page, no dialog
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", c.path, want)
			}
		}
		if strings.Contains(body, "<h1>Settings of") || strings.Contains(body, "<h1>Backups of") {
			t.Errorf("%s still has its own heading", c.path)
		}
	}
	// An operator keeps the name and the state but never gets the admin-only tab and buttons.
	_, op := e.do(t, "GET", "/devices/1/console", "operator", nil)
	if !strings.Contains(op, `<h1>kitchen</h1><span class="devhd-state`) || strings.Contains(op, "Replace device") || strings.Contains(op, ">Edit</a>") || strings.Contains(op, "/devices/1/delete") {
		t.Errorf("operator header:\n%s", op)
	}
}

// Why: design item #50. A device that never answered (or is held by a running
// operation) cannot back up; the button says so and the page gives the reason
// next to it instead of offering something that cannot work. The Transactions
// card is always there so the page has the same shape for every device.
func TestNeverAnsweredDeviceDisablesBackupAndExplains(t *testing.T) {
	e, _, _ := subPageEnv(t)
	_, body := e.do(t, "GET", "/devices/2", "admin", nil) // attic: no health, never probed
	for _, want := range []string{
		`disabled title="The device has not answered yet." aria-disabled="true" aria-describedby="devhd-why">Back up</button>`,
		`<p class="devcard-note" id="devhd-why">Back up: The device has not answered yet.</p>`,
		"This device has not answered yet",
		`<h2 id="dc-act">Activity</h2>`,
		"No state changes or transactions yet.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q:\n%s", want, body)
		}
	}
	_, ok := e.do(t, "GET", "/devices/1", "admin", nil)
	if strings.Contains(ok, `disabled title=`) || strings.Contains(ok, "has not answered yet") {
		t.Errorf("an online device must not be blocked:\n%s", ok)
	}
}

// Why: while an operation runs the page said "commands are paused" yet offered
// Back up now; the reason names the operation, not always "updating".
func TestBackupIsDisabledWhileAnOperationHoldsTheDevice(t *testing.T) {
	e, _, j, _ := setupOTA(t)
	j.jobs = []model.Job{{ID: 1, BatchID: 7, DeviceID: 1, Kind: model.KindBackup, Commands: model.BackupMarker, State: model.JobApplying, CreatedAt: time.Now()}}
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(body, `disabled title="Unavailable while a backup of the device is running." aria-disabled="true" aria-describedby="devhd-why">Back up<`) {
		t.Errorf("Back up now is not disabled with its reason:\n%s", body)
	}
	j.jobs = []model.Job{{ID: 2, BatchID: 8, DeviceID: 1, State: model.JobUpgradeIssued, CreatedAt: time.Now()}}
	_, body = e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(body, `title="Unavailable while the device is being updated."`) {
		t.Errorf("update reason missing:\n%s", body)
	}
}

// Why: a dead end with one red line gives the user nothing to do. The page names
// the way out (retry, back to the overview) and the Settings tab stays the
// current one.
func TestSettingsUnreachableOffersAWayOut(t *testing.T) {
	e := withSettings(t, &fakeSettings{readErr: tasmota.ErrUnreachable})
	resp, body := e.do(t, "GET", "/devices/1/settings", "admin", nil)
	if resp.StatusCode != 502 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for _, want := range []string{`class="devform devform-msg" aria-labelledby="rd-h" role="alert"`, "The device is unreachable.", `>Try again</a>`, `>Back to the overview</a>`, `aria-current="page">Settings</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `<p class="error"`) {
		t.Error("the failure is one bare red line again")
	}
}

// Why: the current tab of an unavailable page was drawn in the disabled grey, so
// the tab you are on looked inactive. The rule that undoes it must come after the
// disabled rules.
func TestCurrentTabIsNeverDrawnDisabled(t *testing.T) {
	css, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	const rule = `.tabs a[aria-disabled="true"][aria-current="page"]`
	i := strings.LastIndex(string(css), rule)
	if i < 0 {
		t.Fatal("no rule keeps the current tab out of the disabled style")
	}
	if j := strings.LastIndex(string(css), `.tabs a[aria-disabled="true"] {`); j > i {
		t.Fatal("a disabled-tab rule comes after the current-tab rule and wins")
	}
}

// Why: a failed probe was bold red text without the Refresh identity button, and
// a good one replaced the button and left the rows stale. The whole card comes
// back: the button stays, the sentence is not a Go error chain. (Edit lives in
// the device header since design v2.)
func TestProbeAnswersWithTheWholeIdentityCard(t *testing.T) {
	e := setup(t)
	e.pr.err = tasmota.ErrTimeout
	_, body := e.do(t, "POST", "/devices/1/probe", "admin", url.Values{"csrf": {"tok"}}, "HX-Request", "true")
	for _, want := range []string{`<section id="identity"`, `role="alert"><span class="banner-dot"></span><div class="banner-text">Identity not refreshed. The device took too long`, `>Refresh identity</button>`} {
		if !strings.Contains(body, want) {
			t.Errorf("failed probe lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Probe failed") || strings.Contains(body, "tasmota:") {
		t.Errorf("a Go error chain reached the page:\n%s", body)
	}
	e.pr.err = nil
	e.inv.devs[1] = model.Device{ID: 1, Name: "kitchen", Address: "10.0.0.1", MAC: "aa:bb:cc:dd:ee:ff", Identity: model.Identity{Family: model.ChipESP32, Version: "16.1.0", Variant: "tasmota32"}}
	_, body = e.do(t, "POST", "/devices/1/probe", "admin", url.Values{"csrf": {"tok"}}, "HX-Request", "true")
	for _, want := range []string{`role="status"><span class="banner-dot"></span><div class="banner-text">Identity refreshed.</div>`, "aa:bb:cc:dd:ee:ff", "16.1.0", `>Refresh identity</button>`} {
		if !strings.Contains(body, want) {
			t.Errorf("refreshed card lacks %q:\n%s", want, body)
		}
	}
}

// Why (#104): "Refresh identity" swaps only the identity card, so the header (name, address, chip,
// firmware) and the breadcrumb kept the old name until a reload. A good refresh now carries them
// out of band, from the same stored device the card shows; a failed one changes nothing.
func TestProbeUpdatesTheHeaderAndBreadcrumbWithTheCard(t *testing.T) {
	e := setup(t)
	e.inv.devs[1] = model.Device{ID: 1, Name: "Irrigation Led Ring", Address: "10.0.0.1", MAC: "aa:bb:cc:dd:ee:ff", Identity: model.Identity{Family: model.ChipESP32, Version: "16.1.0", Variant: "tasmota32"}}
	_, body := e.do(t, "POST", "/devices/1/probe", "admin", url.Values{"csrf": {"tok"}}, "HX-Request", "true")
	for _, want := range []string{`id="devhd" hx-swap-oob="true"`, `<h1>Irrigation Led Ring</h1>`, `<nav class="crumbs" id="crumbs" aria-label="Breadcrumb" hx-swap-oob="true">`, `<span class="crumb-here" aria-current="page">Irrigation Led Ring</span>`, `<span class="ph-title" id="ph-title" hx-swap-oob="true">Irrigation Led Ring</span>`, `<title>Irrigation Led Ring - TasmoFleet</title>`} {
		if !strings.Contains(body, want) {
			t.Errorf("refreshed response lacks %q:\n%s", want, body)
		}
	}
	e.pr.err = tasmota.ErrTimeout
	_, body = e.do(t, "POST", "/devices/1/probe", "admin", url.Values{"csrf": {"tok"}}, "HX-Request", "true")
	if strings.Contains(body, "hx-swap-oob") {
		t.Errorf("a failed refresh must swap only the card:\n%s", body)
	}
}

// Why (#110): the Restart confirmation names the device ("Restart <name>?") and the offline
// banner is part of the same header; after a refresh that renamed the device both kept the old
// text until a reload, so the dialog asked about a device that no longer had that name.
func TestProbeUpdatesTheRestartDialogAndOfflineBannerToo(t *testing.T) {
	for _, tc := range []struct {
		name   string
		health model.Health
		banner bool
	}{
		{"online", model.Health{DeviceID: 1, State: model.HealthOnline, LastSeen: time.Now()}, false},
		{"offline", model.Health{DeviceID: 1, State: model.HealthOffline, LastSeen: time.Now()}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setupWith(t, func(d *Deps) {
				d.Health.(fakeHealth)[1] = tc.health
				d.DeviceCmd = &fakeCmd{}
			})
			e.inv.devs[1] = model.Device{ID: 1, Name: "Irrigation Led Ring", Address: "10.0.0.1", MAC: "aa:bb:cc:dd:ee:ff", Identity: model.Identity{Family: model.ChipESP32, Version: "16.1.0", Variant: "tasmota32"}}
			_, body := e.do(t, "POST", "/devices/1/probe", "admin", url.Values{"csrf": {"tok"}}, "HX-Request", "true")
			// htmx reads a bare hx-swap-oob attribute as an innerHTML swap: it needs the value "true"
			if !regexp.MustCompile(`<dialog id="dlg-restart"[^>]*hx-swap-oob="true"`).MatchString(body) {
				t.Fatalf("no out-of-band restart dialog:\n%s", body)
			}
			if want := `<h2 id="dlg-restart-h">Restart Irrigation Led Ring?</h2>`; !strings.Contains(body, want) {
				t.Errorf("lacks %q:\n%s", want, body)
			}
			if got := strings.Contains(body, `id="devhd-off"`); got != tc.banner {
				t.Errorf("offline banner swapped = %v, want %v", got, tc.banner)
			}
		})
	}
}

// refusedRefresh stands for app.RefreshRefusedError: the web layer knows it only by its method.
type refusedRefresh struct{}

func (refusedRefresh) Error() string { return "identity refresh refused: internal detail" }
func (refusedRefresh) Refusal() string {
	return "The device at this address reports MAC de:ad:be:ef:00:01, not the MAC stored."
}

// Why (#65): when another device answers at the stored address, the device page must say so in
// plain words (not "an answer that did not fit"), so the admin fixes the address or replaces the
// device. The refresh carries who asked: only a person's refresh may bind a MAC.
func TestProbeShowsARefusedRefreshAndPassesTheActor(t *testing.T) {
	e := setup(t)
	e.pr.err = fmt.Errorf("wrapped: %w", refusedRefresh{})
	_, body := e.do(t, "POST", "/devices/1/probe", "admin", url.Values{"csrf": {"tok"}}, "HX-Request", "true")
	if want := "Identity not refreshed. The device at this address reports MAC de:ad:be:ef:00:01, not the MAC stored."; !strings.Contains(body, want) {
		t.Errorf("refused refresh lacks %q:\n%s", want, body)
	}
	if strings.Contains(body, "internal detail") {
		t.Error("the Go error reached the page")
	}
	if len(e.pr.actors) != 1 || e.pr.actors[0] == "" {
		t.Fatalf("actors %q: the refresh must name who asked", e.pr.actors)
	}
}

// keptTopic stands for app.TopicKeptError: the web layer knows it only by its Notice method.
type keptTopic struct{}

func (keptTopic) Error() string { return "topic not changed: internal detail" }
func (keptTopic) Notice() string {
	return "The device reports the MQTT topic taken-topic, which another device already has."
}

// Why (#111): a refresh that could not make the topic follow the device (another device has it)
// is still a refresh, but the admin must see why MQTT stays disconnected, on the page.
func TestProbeShowsATopicThatCouldNotFollowTheDevice(t *testing.T) {
	e := setup(t)
	e.inv.devs[1] = model.Device{ID: 1, Name: "kitchen", Address: "10.0.0.1", MAC: "aa:bb:cc:dd:ee:ff"}
	e.pr.err = fmt.Errorf("wrapped: %w", keptTopic{})
	_, body := e.do(t, "POST", "/devices/1/probe", "admin", url.Values{"csrf": {"tok"}}, "HX-Request", "true")
	if want := "Identity refreshed. The device reports the MQTT topic taken-topic, which another device already has."; !strings.Contains(body, want) {
		t.Errorf("lacks %q:\n%s", want, body)
	}
	if strings.Contains(body, "internal detail") || strings.Contains(body, "Identity not refreshed") {
		t.Error("a kept topic was shown as a failed refresh")
	}
}

// Why: with no backup the compare and "changes since" controls have nothing to
// work on, and the empty state must carry the action that fills it.
func TestEmptyBackupsTabOffersTheActionNotTheControls(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/devices/1/backups", "admin", nil)
	for _, want := range []string{"No backups yet", `<input type="hidden" name="from" value="tab">`, `>Back up now</button>`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q:\n%s", want, body)
		}
	}
	for _, bad := range []string{">Compare</button>", "Prune now", "Delete selected"} {
		if strings.Contains(body, bad) {
			t.Errorf("%q is offered without a backup", bad)
		}
	}
	// A device that cannot answer gets the button disabled, with the reason.
	_, nb := e.do(t, "GET", "/devices/2/backups", "admin", nil)
	if !strings.Contains(nb, `disabled title="The device has not answered yet."`) {
		t.Errorf("never-answered device: %s", nb)
	}
}

// Why: Back up now on the empty tab reloads the page to show the new backup; a
// failure stays under the button in a sentence.
func TestBackupFromTheEmptyTab(t *testing.T) {
	e := setup(t)
	resp, _ := e.do(t, "POST", "/devices/1/backup", "admin", url.Values{"csrf": {"tok"}, "from": {"tab"}}, "HX-Request", "true")
	if resp.Header.Get("HX-Redirect") != "/tf/devices/1/backups?ok=backed-up" {
		t.Fatalf("redirect %q", resp.Header.Get("HX-Redirect"))
	}
	_, page := e.do(t, "GET", "/devices/1/backups?ok=backed-up", "admin", nil)
	if !strings.Contains(page, "Backup taken.") {
		t.Errorf("no confirmation after the reload:\n%s", page)
	}
	e2 := setup(t)
	e2.bk.failErr = tasmota.ErrUnreachable
	resp, body := e2.do(t, "POST", "/devices/1/backup", "admin", url.Values{"csrf": {"tok"}, "from": {"tab"}}, "HX-Request", "true")
	if resp.Header.Get("HX-Redirect") != "" || !strings.Contains(body, "Backup failed. The device did not answer.") || strings.Contains(body, "tasmota:") {
		t.Errorf("failure: %s", body)
	}
}

// Why: the confirmation dialog of the newest backup carries the danger callout the
// prune page uses for the same case, in sentence case (not all-caps body text).
// Restore and Replace name an unchecked chip family as a warning: the user may
// still go ahead, so red would be the wrong tone.
func TestOverridableRisksAreWarningsNotErrors(t *testing.T) {
	e, _, rs := newDiffEnv(t)
	rs.assess = restore.Assessment{Family: true, Detail: "the chip family of the backup could not be checked (no decoded backup)"}
	_, body := e.do(t, "GET", "/backups/1/restore", "admin", nil)
	if !hasBanner(body, "tone-warn", "The chip family of the backup could not be checked") {
		t.Errorf("restore confirm:\n%s", body)
	}
	if hasBanner(body, "tone-danger", "The chip family") {
		t.Error("an overridable risk is drawn as an error")
	}
	e2, _, rr := newAdminEnv(t)
	rr.preview.Detail = "the chip family of the backup could not be checked (no decoded backup)"
	rr.preview.Family = true
	_, body = e2.do(t, "GET", "/devices/1/replace/preview?address=10.0.0.9", "admin", nil)
	if !hasBanner(body, "tone-warn", "The chip family of the backup could not be checked") || hasBanner(body, "tone-danger", "The chip family") {
		t.Errorf("replace confirm:\n%s", body)
	}
}

// hasBanner reports whether the page has a banner of the tone whose text starts with text.
func hasBanner(body, tone, text string) bool {
	for _, part := range strings.Split(body, `<div class="banner `+tone+`"`)[1:] {
		end := strings.Index(part, "</div>")
		if end < 0 {
			continue
		}
		if i := strings.Index(part, `<div class="banner-text">`); i >= 0 && i < end && strings.HasPrefix(strings.TrimSpace(part[i+len(`<div class="banner-text">`):]), text) {
			return true
		}
	}
	return false
}

// Why: "Replace is refused while it still answers" stood on the page twice, once as
// a standing warning and once as the refusal with the same message. The tab now names it only
// as the refusal, as an alert.
func TestReplaceRefusalDoesNotRepeatTheStandingWarning(t *testing.T) {
	e, _, rr := newAdminEnv(t)
	rr.prevErr = restore.ErrOldReachable
	_, body := e.do(t, "GET", "/devices/1/replace/preview?address=10.0.0.9", "admin", nil)
	if n := strings.Count(body, "still answers"); n != 1 {
		t.Errorf("the message appears %d times, want once:\n%s", n, body)
	}
	if !hasBanner(body, "tone-danger", "The old device still answers at its address.") || !strings.Contains(body, `role="alert"`) {
		t.Errorf("the refusal is not shown as the error:\n%s", body)
	}
}

// Why: a restart is reversible. Red is for delete; the restart confirmation uses the
// primary button.
func TestRestartConfirmIsNotADangerButton(t *testing.T) {
	e := cmdEnv(t, &fakeCmd{})
	_, restart := e.do(t, "GET", "/devices/1/restart", "admin", nil)
	if !strings.Contains(restart, `<button type="submit" class="btn btn-primary">Restart device</button>`) {
		t.Errorf("restart:\n%s", restart)
	}
	_, del := e.do(t, "GET", "/devices/1/delete", "admin", nil)
	if !strings.Contains(del, `<button type="submit" class="btn btn-danger-solid dd-go">Delete device</button>`) {
		t.Errorf("delete lost its danger tone:\n%s", del)
	}
}

// Why: on a phone the secondary cells of a table have no labels, so "Before" and "After" were
// two unlabelled values under the setting name. The drift tab (design v2, P3) keeps its header
// row above the values and scrolls sideways on a phone; the comparison (P10) is the prototype's
// grid, whose header row names Older and Newer at every width; the replace preview names every
// value in its row.
func TestComparisonCellsKeepTheirPhoneLabels(t *testing.T) {
	e, _, _ := newDiffEnv(t)
	driftMu.Lock()
	clear(driftLast)
	driftMu.Unlock()
	_, body := e.do(t, "GET", "/devices/1/drift", "admin", nil)
	if !strings.Contains(body, `<div class="devdrift-row devdrift-head"><span>Setting</span><span>In last backup</span><span>Live on the device</span></div>`) || !strings.Contains(body, `class="devdrift-scroll"`) {
		t.Errorf("drift lost its header row or its sideways scroll:\n%s", body)
	}
	_, cmp := e.do(t, "GET", "/devices/1/backups/diff?a=3&b=6", "admin", nil)
	if !strings.Contains(cmp, `<div class="diff-grid diff-headrow"><span>Setting</span><span>Older · 18 Oct 2026, 10:00</span><span>Newer · 18 Oct 2026, 11:00</span></div>`) {
		t.Errorf("the comparison lost its column heads:\n%s", cmp)
	}
	e2, _, _ := newAdminEnv(t)
	_, body = e2.do(t, "GET", "/devices/1/replace/preview?address=10.0.0.9", "admin", nil)
	for _, want := range []string{`<div class="rp-kv"><span>Hostname</span>`, `<div class="rp-skip"><span>MAC address</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the replace preview lacks %q:\n%s", want, body)
		}
	}
	css, err := os.ReadFile("static/app.css")
	if err != nil || !strings.Contains(string(css), "table.dense td.s.lab[data-label]::before") {
		t.Error("no phone rule shows the label of td.s.lab")
	}
}

// Why: a backup's label ("before replace") belongs on its row's second line, not crammed into
// the date; restore jobs show their state as the badge every other state uses, not plain text.
func TestBackupsTabHasNoteColumnAndRestoreBadge(t *testing.T) {
	e, bk, rs := newDiffEnv(t)
	bk.list[0].Note = "replacement hardware, before replace"
	rs.jobs = map[store.RestoreID]restore.Job{1: {ID: 1, DeviceID: 1, State: store.RestoreDone, Actor: "admin", CreatedAt: time.Now()}}
	_, body := e.do(t, "GET", "/devices/1/backups", "admin", nil)
	for _, want := range []string{`<div class="bk-detail">replacement hardware, before replace · Tasmota 15.6.0 · 4.0 KiB</div>`, `<span class="bdg t-ok">Done</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q:\n%s", want, body)
		}
	}
}

// Why: the reply of a device is mostly one long JSON line without spaces. It must wrap inside the
// console (QA: it ran past the pane edge and was cut off, on a phone worse), keeping its spacing;
// it must not scroll the page or hide the end of the line. In the v2 console it is the reply line
// after the command just sent.
func TestConsoleReplyWrapsInsidePanel(t *testing.T) {
	e := cmdEnv(t, &fakeCmd{reply: `{"StatusFWR":{"Version":"16.1.0(release-tasmota)"}}`})
	_, body := e.do(t, "POST", "/devices/1/console", "admin", url.Values{"csrf": {"tok"}, "command": {"Status 2"}})
	if !strings.Contains(body, `class="console-line is-reply" data-visual-mask="console-reply"`) || !strings.Contains(body, `{&#34;StatusFWR&#34;:{&#34;Version&#34;:&#34;16.1.0(release-tasmota)&#34;}}`) {
		t.Errorf("console:\n%s", body)
	}
	css, err := os.ReadFile("static/app.css")
	if err != nil || !strings.Contains(string(css), ".console-line.is-reply .console-text {\n  white-space: pre-wrap;\n  word-break: normal;\n  overflow-wrap: anywhere;") {
		t.Error("the reply line no longer wraps inside the panel")
	}
}
