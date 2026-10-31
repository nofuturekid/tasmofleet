package web

import (
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/config"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// formBody is the HTML of the settings form (between its opening tag and its closing tag).
func formBody(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, `<form id="sysform"`)
	j := strings.Index(page[max(i, 0):], "</form>")
	if i < 0 || j < 0 {
		t.Fatal("no settings form on the page")
	}
	return page[i : i+j]
}

// fieldHTML is the markup of one setting's wrapper (up to the next wrapper), so a test can ask
// what sits inside it.
func fieldHTML(t *testing.T, page, key string) string {
	t.Helper()
	marker := `data-key="` + key + `"`
	i := strings.Index(page, marker)
	if i < 0 {
		t.Fatalf("no field wrapper for %s", key)
	}
	rest := page[i+len(marker):]
	if j := strings.Index(rest, `data-key="`); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// Why: the maintainer approved eight tabs (10 -> 8): Metrics joins General, Devices joins Network,
// Updates (OTA) joins Firmware; #249 adds Notifications between Backups and Firmware & updates.
// A field that no tab names must still land somewhere, never vanish.
func TestSystemSettingsHasTheApprovedTabs(t *testing.T) {
	want := []string{"general", "network", "mqtt", "discovery", "backups", "notifications", "firmware", "signin", "advanced"}
	if len(sysLayout) != len(want) {
		t.Fatalf("%d tabs, want %d", len(sysLayout), len(want))
	}
	for i, d := range sysLayout {
		if d.Slug != want[i] {
			t.Errorf("tab %d is %s, want %s", i, d.Slug, want[i])
		}
	}
	if tab, _ := sysPlace("SOMETHING_NEW"); sysLayout[tab].Slug != "advanced" {
		t.Error("a field without a place must land on Advanced, not vanish")
	}
	// The old tab names are gone and not redirected (no compatibility shims before 1.0):
	// an unknown tab shows the first one.
	e, _, _, _ := sysEnv(t, true)
	_, body := e.do(t, "GET", "/settings/system?tab=fwsource", "admin", nil)
	if !strings.Contains(body, `<a class="utab" href="/tf/settings/system?tab=general" aria-current="page">General</a>`) {
		t.Error("an old tab name must open the first tab")
	}
}

// Why (#335): both ways of finding new devices belong on one tab, and the MQTT tab is only the
// connection. The old address ?tab=scan is not redirected (no shims): it opens the first tab.
func TestSystemSettingsDiscoveryTabHoldsBothWaysOfFindingDevices(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	_, page := e.do(t, "GET", "/settings/system", "admin", nil)
	panel := func(slug string) string {
		i := strings.Index(page, `data-sys-tab="`+slug+`"`)
		if i < 0 {
			t.Fatalf("no %s panel", slug)
		}
		rest := page[i+10:]
		if j := strings.Index(rest, `data-sys-tab="`); j >= 0 {
			rest = rest[:j]
		}
		return rest
	}
	mqtt, disc := panel("mqtt"), panel("discovery")
	for _, id := range []string{`id="card-broker"`, `id="card-topics"`} {
		if !strings.Contains(mqtt, id) {
			t.Errorf("the MQTT tab lost %s", id)
		}
	}
	if strings.Contains(mqtt, "MQTT discovery") || strings.Contains(mqtt, "DISCOVERY_MQTT") {
		t.Error("MQTT discovery must have left the MQTT tab")
	}
	for _, want := range []string{`id="card-mqttdiscovery"`, `name="v.DISCOVERY_MQTT"`, `id="card-scan"`, `name="v.DISCOVERY_SCAN"`} {
		if !strings.Contains(disc, want) {
			t.Errorf("the Discovery tab lacks %s", want)
		}
	}
	if strings.Index(disc, `id="card-mqttdiscovery"`) > strings.Index(disc, `id="card-scan"`) {
		t.Error("MQTT discovery comes first, the network scan next to it")
	}
	if !strings.Contains(disc, `href="/tf/settings/system?tab=network#card-networks"`) {
		t.Error("the network scan card must link to the device networks it is limited to")
	}
	if !strings.Contains(page, `href="/tf/settings/system?tab=discovery">Discovery</a>`) || strings.Contains(page, "Discovery scan") {
		t.Error("the tab is called Discovery")
	}
	_, body := e.do(t, "GET", "/settings/system?tab=scan", "admin", nil)
	if !strings.Contains(body, `<a class="utab" href="/tf/settings/system?tab=general" aria-current="page">General</a>`) {
		t.Error("the old tab=scan must open the first tab, not redirect")
	}
	_, body = e.do(t, "GET", "/settings/system?tab=discovery", "admin", nil)
	if !strings.Contains(body, `href="/tf/settings/system?tab=discovery" aria-current="page">Discovery</a>`) {
		t.Error("?tab=discovery opens the Discovery tab")
	}
}

// Why (#333): the option only matters while the host or port is being changed, so it belongs to
// the Host/Port row, not to the password. Without script it stays visible there; sysform.js
// shows it only when host or port differs from the stored value.
func TestSystemSettingsKeepPasswordOptionSitsUnderTheBrokerRow(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	_, page := e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
	const word = "Broker changed: keep the stored password for the new broker"
	if strings.Count(page, word) != 1 || strings.Contains(page, "Keep the stored password if the broker changes") {
		t.Fatalf("want the new wording exactly once, old wording gone")
	}
	opt := strings.Index(page, `name="reuse.MQTT_PASSWORD"`)
	port, pw := strings.Index(page, `name="mqtt.port"`), strings.Index(page, `name="v.MQTT_PASSWORD"`)
	if opt < 0 || port >= opt || opt >= pw {
		t.Errorf("option at %d must sit after the port (%d) and before the password field (%d)", opt, port, pw)
	}
	if !strings.Contains(page, `data-broker-keep`) {
		t.Error("the option needs its script hook")
	}
}

// Why (#334): a field that depends on a switch follows it as a normal field of the card; the
// indented block with a rule on the left is gone. Dimming stays (data-depends).
func TestSettingsDependentFieldsHaveNoIndentedBlock(t *testing.T) {
	css, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)\n\.dep \{(.*?)\}`).FindSubmatch(css)
	if m == nil {
		t.Fatal("no .dep rule")
	}
	for _, bad := range []string{"border-left", "margin-left", "padding-left"} {
		if strings.Contains(string(m[1]), bad) {
			t.Errorf(".dep must not have %s: no indented block under a switch", bad)
		}
	}
}

// Why (#336): the Storage card is on the Backups tab, but the tabs switch without a page load, so
// it must be on the page whichever tab the page was opened on (it was loaded only for ?tab=backups).
func TestSystemSettingsBackupsStorageIsOnThePageWhateverTabOpened(t *testing.T) {
	e, _, _, _ := storageEnv(t)
	for _, path := range []string{"/settings/system", "/settings/system?tab=mqtt"} {
		_, body := e.do(t, "GET", path, "admin", nil)
		if !strings.Contains(body, `id="card-storage"`) {
			t.Errorf("%s: the Storage card is missing", path)
		}
	}
}

// Why (#336): with scheduled backups off the stored 0 means "manual only"; showing "0s" in the
// dimmed Interval reads as a value. It is empty with a 24h placeholder; saving off still stores 0.
func TestSystemSettingsBackupIntervalIsEmptyWhileTheScheduleIsOff(t *testing.T) {
	e, svc, _, _ := sysEnv(t, true)
	_, page := e.do(t, "GET", "/settings/system", "admin", nil)
	m := regexp.MustCompile(`<input[^>]*name="v\.BACKUP_INTERVAL"[^>]*>`).FindString(page)
	if m == "" || strings.Contains(m, `value="0s"`) || !strings.Contains(m, `placeholder="24h"`) {
		t.Errorf("interval input while off: %s", m)
	}
	r, _ := e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "swm.BACKUP_INTERVAL": {"1"}, "v.BACKUP_INTERVAL": {""}})
	if r.StatusCode != 303 || svc.Snapshot().BackupSchedule != 0 {
		t.Errorf("off with an empty interval must store 0: %d, %v", r.StatusCode, svc.Snapshot().BackupSchedule)
	}
}

// Why: the staggered grid came from "Reset ... on save" being its own grid cell next to the field.
// The reset control now belongs to the field that owns it, so no field can push another around.
func TestSystemSettingsResetControlIsInsideItsOwnField(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	// a stored BACKUP_KEEP (the fixture stores MQTT_URL and MQTT_PASSWORD) must offer a reset too
	e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "v.BACKUP_KEEP": {"4"}})
	_, page := e.do(t, "GET", "/settings/system", "admin", nil)
	for _, key := range []string{"BACKUP_KEEP", "MQTT_URL", "MQTT_PASSWORD"} {
		in := fieldHTML(t, page, key)
		if !strings.Contains(in, `name="clear.`+key+`"`) {
			t.Errorf("%s is stored, so its own wrapper must hold the reset control", key)
		}
	}
	if strings.Contains(page, `class="ucheck sys-reset"`) {
		t.Error("a reset checkbox is still a grid cell of its own")
	}
	if strings.Contains(page, "to the default on save") {
		t.Error("the old two-line reset wording is still there")
	}
}

// Why: help text was most of every tab. A field's help is one short line; anything longer is
// behind the info button. "Applies after a restart." nine times is replaced by one badge.
func TestSystemSettingsHelpIsOneShortLine(t *testing.T) {
	for _, st := range sysconfig.Fields() {
		ui := sysFieldCopy(st.Key)
		if len(ui.Help) > 72 {
			t.Errorf("%s: help is %d characters (%q), at most 72; move the rest to Info", st.Key, len(ui.Help), ui.Help)
		}
		if strings.Count(ui.Help, ". ") > 2 {
			t.Errorf("%s: help has several sentences: %q", st.Key, ui.Help)
		}
	}
	e, _, _, _ := sysEnv(t, true)
	_, page := e.do(t, "GET", "/settings/system", "admin", nil)
	if strings.Contains(page, "Applies after a restart.") {
		t.Error("the per-field restart line is back")
	}
}

// Why: touch and keyboard users cannot hover. The longer text is a native popover opened by a
// button with a name, so it works on a phone and a screen reader finds it.
func TestSystemSettingsInfoPopoversAreAccessible(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	_, page := e.do(t, "GET", "/settings/system", "admin", nil)
	targets := regexp.MustCompile(`popovertarget="([^"]+)"`).FindAllStringSubmatch(page, -1)
	if len(targets) < 8 {
		t.Fatalf("only %d info buttons", len(targets))
	}
	seen := map[string]bool{}
	for _, m := range targets {
		id := m[1]
		if seen[id] {
			t.Errorf("duplicate popover target %s", id)
		}
		seen[id] = true
		if n := strings.Count(page, `id="`+id+`" popover`); n != 1 {
			t.Errorf("popover %s exists %d times", id, n)
		}
	}
	if !strings.Contains(page, `<button type="button" class="uinfo" popovertarget="info-DEVICE_NETWORKS" aria-expanded="false" aria-controls="info-DEVICE_NETWORKS" aria-label="About Device networks">i</button>`) {
		t.Error("the info button lacks its name, state or target")
	}
	if !strings.Contains(page, `role="note"`) {
		t.Error("the popover text is not a note")
	}
}

// Why: a setting fixed by the container configuration used to look like an editable input.
// It now shows the Environment badge and the key, and has no input at all.
func TestSystemSettingsLockedFieldIsReadOnlyWithABadge(t *testing.T) {
	e, _, _, _ := sysEnvFull(t, true, nil, "TASMOFLEET_METRICS_TOKEN=fake-env-token-6")
	_, page := e.do(t, "GET", "/settings/system", "admin", nil)
	in := fieldHTML(t, page, "METRICS_TOKEN")
	if strings.Contains(in, "<input") || strings.Contains(in, "<textarea") {
		t.Errorf("a locked field has a control: %.300s", in)
	}
	for _, want := range []string{"Environment", "METRICS_TOKEN", "container configuration"} {
		if !strings.Contains(in, want) {
			t.Errorf("locked field lacks %q: %.400s", want, in)
		}
	}
	if strings.Contains(page, "fake-env-token-6") {
		t.Error("the environment token leaked into the page")
	}
}

// Why: boxes next to switches were inconsistent. Every on/off setting is a switch, and each still
// carries the b.KEY marker so that "off" is saved.
func TestSystemSettingsOnOffSettingsAreSwitches(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	_, page := e.do(t, "GET", "/settings/system", "admin", nil)
	n := 0
	for _, f := range sysconfig.Fields() {
		if !f.Bool {
			continue
		}
		n++
		in := fieldHTML(t, page, f.Key)
		if !strings.Contains(in, `role="switch"`) || !strings.Contains(in, `name="v.`+f.Key+`"`) || !strings.Contains(in, `name="b.`+f.Key+`"`) {
			t.Errorf("%s is not a switch with its marker: %.300s", f.Key, in)
		}
		if strings.Contains(in, `class="ucheck`) {
			t.Errorf("%s is still a checkbox", f.Key)
		}
	}
	if n == 0 {
		t.Fatal("no on/off settings found")
	}
}

// Why: scan interval and networks do nothing while the scan is off. They stay in the form (and are
// posted, so nothing is lost) but are marked as depending on the switch, which the page dims.
func TestSystemSettingsDependentFieldsFollowTheirSwitch(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	_, page := e.do(t, "GET", "/settings/system", "admin", nil)
	dep := regexp.MustCompile(`data-depends="v\.DISCOVERY_SCAN"`).FindStringIndex(page)
	if dep == nil {
		t.Fatal("no dependent group for the scheduled scan")
	}
	rest := page[dep[1]:]
	for _, name := range []string{`name="v.DISCOVERY_SCAN_INTERVAL"`, `name="v.DISCOVERY_SCAN_CIDRS"`} {
		if !strings.Contains(rest, name) {
			t.Errorf("%s is not inside the dependent group", name)
		}
	}
	if !strings.Contains(page, `data-depends="v.AUTH_MODE=oidc"`) {
		t.Error("the identity provider cards must depend on the sign-in mode")
	}
	if !strings.Contains(page, `data-depends="v.METRICS_ENABLED"`) {
		t.Error("the scrape token must depend on the metrics switch")
	}
}

// Why: the danger zone sat inside the form directly above Save, and the container list is
// read-only data. Both belong outside the form, on the Advanced tab.
func TestSystemSettingsDangerZoneAndContainerListAreOutsideTheForm(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	_, page := e.do(t, "GET", "/settings/system?tab=advanced", "admin", nil)
	form := formBody(t, page)
	for _, s := range []string{"Reset all settings", "Reset settings…", "Set by the container"} {
		if strings.Contains(form, s) {
			t.Errorf("%q is inside the settings form", s)
		}
		if !strings.Contains(page, s) {
			t.Errorf("%q is missing from the Advanced tab", s)
		}
	}
	// and only on Advanced: another tab keeps it hidden
	_, page = e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
	i := strings.Index(page, `id="after-advanced"`)
	if i < 0 || !strings.Contains(page[i-80:i+80], "hidden") {
		t.Error("the Advanced extras must be hidden while another tab is shown")
	}
}

// Why: one page-wide Save, in a bar that lists the tabs with unsaved changes (script) and works
// without script: Discard and Save sit in the form, once.
func TestSystemSettingsHasOneSaveBarInsideTheForm(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	_, page := e.do(t, "GET", "/settings/system", "admin", nil)
	form := formBody(t, page)
	if strings.Count(form, `data-sys-bar`) != 1 || strings.Count(form, ">Save settings</button>") != 1 {
		t.Errorf("want one save bar with one Save button in the form")
	}
	if !strings.Contains(form, `>Discard</a>`) || !strings.Contains(form, "No unsaved changes") {
		t.Error("the bar lacks Discard or its idle text")
	}
}

// Why: on a phone eight tabs scroll sideways. A section picker (a select) replaces the strip, and
// lists the same nine sections.
func TestSystemSettingsOffersASectionPickerForPhones(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	_, page := e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
	i := strings.Index(page, `data-sys-picker`)
	if i < 0 {
		t.Fatal("no section picker")
	}
	sel := page[i:]
	sel = sel[:strings.Index(sel, "</select>")]
	if strings.Count(sel, "<option") != 9 || !strings.Contains(sel, `value="mqtt" selected`) {
		t.Errorf("the picker must list all nine sections with the current one selected: %s", sel)
	}
}

// Why: "0s" for "manual only" was cryptic. The interval is behind a switch: off stores 0 (manual
// only), on needs a real interval. The setting itself is unchanged (BACKUP_INTERVAL).
func TestSystemSettingsBackupScheduleSwitchIsAViewOfTheInterval(t *testing.T) {
	post := func(t *testing.T, form url.Values) (*env, *sysconfig.Service, int, string) {
		t.Helper()
		e, svc, _, _ := sysEnv(t, true)
		form.Set("csrf", "tok")
		form.Set("swm.BACKUP_INTERVAL", "1")
		r, body := e.do(t, "POST", "/settings/system", "admin", form)
		return e, svc, r.StatusCode, body
	}
	_, svc, code, _ := post(t, url.Values{"v.BACKUP_INTERVAL": {"6h"}, "sw.BACKUP_INTERVAL": {"1"}})
	if code != 303 || svc.Snapshot().BackupSchedule.Hours() != 6 {
		t.Fatalf("on with 6h: %d, %v", code, svc.Snapshot().BackupSchedule)
	}
	_, svc, code, _ = post(t, url.Values{"v.BACKUP_INTERVAL": {"6h"}})
	if code != 303 || svc.Snapshot().BackupSchedule != 0 {
		t.Fatalf("switch off must store 0 (manual only): %d, %v", code, svc.Snapshot().BackupSchedule)
	}
	_, _, code, body := post(t, url.Values{"v.BACKUP_INTERVAL": {"0"}, "sw.BACKUP_INTERVAL": {"1"}})
	if code != 422 || !strings.Contains(body, "how often") {
		t.Fatalf("on with no interval is a field error: %d", code)
	}
	e, _, _, _ := sysEnv(t, true)
	_, page := e.do(t, "GET", "/settings/system?tab=backups", "admin", nil)
	if !strings.Contains(page, `name="sw.BACKUP_INTERVAL"`) || !strings.Contains(page, `data-depends="sw.BACKUP_INTERVAL"`) {
		t.Error("the schedule switch and its dependent interval are missing")
	}
}

// Why: "Upstream sources" was free text although the set is fixed. One checkbox per known source;
// a save with none ticked stores none instead of silently keeping the old value.
func TestSystemSettingsFirmwareSourcesAreCheckboxes(t *testing.T) {
	e, svc, _, _ := sysEnv(t, true)
	_, page := e.do(t, "GET", "/settings/system?tab=firmware", "admin", nil)
	for _, src := range config.KnownFirmwareSources() {
		if !strings.Contains(page, `name="v.FIRMWARE_SOURCES" value="`+src+`"`) {
			t.Errorf("no checkbox for source %s", src)
		}
	}
	r, _ := e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "m.FIRMWARE_SOURCES": {"1"}, "v.FIRMWARE_SOURCES": {config.KnownFirmwareSources()[0]}})
	if r.StatusCode != 303 {
		t.Fatalf("one source: %d", r.StatusCode)
	}
	if got := svc.Snapshot().FWSources; len(got) != 1 {
		t.Errorf("sources %v", got)
	}
	// Unticking every box is a real choice (no source) and must be saved, not ignored.
	if r, _ := e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "m.FIRMWARE_SOURCES": {"1"}}); r.StatusCode != 303 || len(svc.Snapshot().FWSources) != 0 {
		t.Errorf("no source ticked: %d, sources %v", r.StatusCode, svc.Snapshot().FWSources)
	}
	if r, _ := e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "m.BACKUP_KEEP": {"1"}}); r.StatusCode != 400 {
		t.Errorf("a marker for a setting without choices: %d, want 400", r.StatusCode)
	}
}

// Why: the SSO test button posts the whole form, which now carries the broker parts and the
// schedule switch as well. Before, the form's own extra fields made the SSO test answer 400.
func TestSystemSettingsSSOTestAcceptsTheWholeForm(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	r, body := e.do(t, "POST", "/settings/system/sso-test", "admin", url.Values{"csrf": {"tok"}, "v.OIDC_ISSUER": {"https://idp.example/"},
		"mqtt.host": {"broker.lan"}, "mqtt.port": {"1883"}, "swm.BACKUP_INTERVAL": {"1"}, "sw.BACKUP_INTERVAL": {"1"}, "v.BACKUP_INTERVAL": {"24h"}})
	if r.StatusCode != 200 || !strings.Contains(body, "discovery OK") {
		t.Fatalf("status %d", r.StatusCode)
	}
}

// Why: eight sign-in settings ask the admin to confirm, and nothing said so until Save. Cards
// whose settings need it carry the badge; the discovery test sits next to the issuer it tests.
func TestSystemSettingsSignInCardsSayWhatTheyAsk(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	_, page := e.do(t, "GET", "/settings/system?tab=signin", "admin", nil)
	// The approved mockup puts the badges once in each card's header (#338, overriding #337 item 2).
	page = page[strings.Index(page, `id="sec-signin"`):strings.Index(page, `id="sec-advanced"`)]
	if n := strings.Count(page, "Asks you to confirm"); n != 3 {
		t.Errorf("%d confirm badges, want 3 (mode, identity provider, roles), none on fields", n)
	}
	if n := strings.Count(page, ">Restart<"); n != 3 {
		t.Errorf("%d Restart badges on the Sign-in tab, want 3 (one per card header)", n)
	}
	issuer := strings.Index(page, `data-key="OIDC_ISSUER"`)
	test := strings.Index(page, "Test discovery")
	client := strings.Index(page, `data-key="OIDC_CLIENT_ID"`)
	if issuer >= test || test >= client {
		t.Errorf("Test discovery must sit right after the issuer (issuer %d, test %d, client %d)", issuer, test, client)
	}
}

// Why: the form always posts the broker fields, so "Reset to default" on the broker host used to
// compose a URL and clear it in the same request, which the server refuses. The reset must win.
func TestSystemSettingsResetOfTheBrokerWinsOverTheBrokerFields(t *testing.T) {
	e, svc, _, _ := sysEnv(t, true)
	r, body := e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "clear.MQTT_URL": {"1"}, "clear.MQTT_PASSWORD": {"1"}, "mqtt.host": {"broker.lan"}, "mqtt.port": {"1883"}})
	if r.StatusCode != 303 {
		t.Fatalf("status %d %s", r.StatusCode, body)
	}
	if got := svc.Snapshot().MQTTURL; got != "" {
		t.Errorf("MQTT_URL %q, want the default (off)", got)
	}
}

// Why: the first firmware source is the primary one, but the checkboxes are drawn in the order of
// the known sources. A save that changes nothing must not reorder the stored list (it would count
// as a change: restart notice, audit entry); a real change keeps the stored order.
func TestKeepSourceOrderDoesNotReorderTheStoredSources(t *testing.T) {
	for _, c := range []struct{ posted, current, want string }{
		{"github,ota.tasmota.com", "ota.tasmota.com,github", "ota.tasmota.com,github"}, // same set: untouched
		{"github", "ota.tasmota.com,github", "github"},                                 // one removed
		{"github,ota.tasmota.com", "ota.tasmota.com", "ota.tasmota.com,github"},        // one added: appended
		{"", "github", ""}, // none ticked
	} {
		if got := keepSourceOrder(c.posted, c.current); got != c.want {
			t.Errorf("posted %q, stored %q: %q, want %q", c.posted, c.current, got, c.want)
		}
	}
	e, svc, _, _ := sysEnv(t, true)
	if _, err := svc.Apply(t.Context(), sysconfig.Request{Actor: "admin", StepUp: sysconfig.StepUp{Done: true, Possible: true},
		Values: map[string]string{"FIRMWARE_SOURCES": "ota.tasmota.com,github"}}); err != nil {
		t.Fatal(err)
	}
	e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "m.FIRMWARE_SOURCES": {"1"}, "v.FIRMWARE_SOURCES": {"github", "ota.tasmota.com"}})
	if got := svc.Snapshot().FWSources; len(got) != 2 || got[0] != "ota.tasmota.com" {
		t.Errorf("sources %v: the primary source changed", got)
	}
}

// Why: a stored secret that is not used until it is entered again, and a restart-only change that
// is still waiting, must be visible at the field, not only in the page banner.
func TestSettingFieldShowsNoticeAndPendingRestart(t *testing.T) {
	d := sysFieldData(sysRow{FieldState: sysconfig.FieldState{Field: sysconfig.Field{Key: "OIDC_CLIENT_SECRET", Label: "Client secret"}, NeedsReentry: true, Skipped: "enter the secret again", PendingRestart: true}})
	if d.Notice != "enter the secret again" || !d.Pending {
		t.Errorf("notice %q, pending %v", d.Notice, d.Pending)
	}
}

// Why: a setting that no card names yet (a new sysconfig field) must show up next to its relatives
// by group, not only in Advanced; and the settings text area warning sits with the health checks.
func TestSystemSettingsNewFieldsLandByGroup(t *testing.T) {
	if tab, card := sysPlace("CR_WARN_PERCENT"); sysLayout[tab].Cards[card].ID != "health" {
		t.Errorf("CR_WARN_PERCENT is on card %s", sysLayout[tab].Cards[card].ID)
	}
	// without an explicit key, a health-group field still lands on the health card
	sysGroupOnly := sysconfig.Field{Key: "TEST_ONLY_HEALTH", Group: sysconfig.GroupHealth}
	if sysGroupCard[sysGroupOnly.Group] != "health" {
		t.Error("the Health group has no card")
	}
}

// Why (#337): "Reset to default" on a field whose value is the default has nothing to reset.
func TestSystemSettingsResetIsOfferedOnlyWhenTheValueDiffersFromTheDefault(t *testing.T) {
	stored := func(key, val string) sysRow {
		return sysRow{FieldState: sysconfig.FieldState{Field: sysconfig.Field{Key: key}, Value: val, Source: "stored"}}
	}
	if sysCanReset(stored("OIDC_GROUPS_CLAIM", config.DefaultValue("OIDC_GROUPS_CLAIM"))) {
		t.Error("a stored value equal to the default must not offer a reset")
	}
	if !sysCanReset(stored("OIDC_GROUPS_CLAIM", "roles")) {
		t.Error("a stored value that differs must offer a reset")
	}
	if sysCanReset(sysRow{FieldState: sysconfig.FieldState{Field: sysconfig.Field{Key: "OIDC_GROUPS_CLAIM"}, Source: "default"}}) {
		t.Error("a value that was never stored has nothing to reset")
	}
}

// Why (#338, approved mockup v8): Sign-in mode is a full-width strip, Identity provider takes two
// columns and Roles and claims the third; the mode card holds its Restart and confirm badges in the
// header and says PUBLIC_URL below the choices, with no field label row (no lone reset line).
func TestSystemSettingsSignInTabFollowsTheMockup(t *testing.T) {
	for _, d := range sysLayout {
		if d.Slug != "signin" {
			continue
		}
		if !slices.Equal(d.Spans, []int{ui.SpanAll, 2, 1}) {
			t.Errorf("sign-in stacks %v, want full width, two columns, one", d.Spans)
		}
		for _, c := range d.Cards {
			if want := map[string]int{"mode": 0, "idp": 1, "roles": 2}[c.ID]; c.Stack != want {
				t.Errorf("card %s in stack %d, want %d", c.ID, c.Stack, want)
			}
		}
	}
	e, _, _, _ := sysEnv(t, true)
	_, page := e.do(t, "GET", "/settings/system?tab=signin", "admin", nil)
	i := strings.Index(page, `id="card-mode"`)
	mode := page[i : i+strings.Index(page[i:], `id="card-idp"`)]
	if strings.Count(mode, "Asks you to confirm") != 1 || strings.Count(mode, ">Restart<") != 1 {
		t.Error("the mode card carries one Restart and one confirm badge, in its header")
	}
	if strings.Contains(mode, `class="lrow"`) {
		t.Error("the radio cards have no label row (it only held a lone reset line)")
	}
	if !strings.Contains(mode, "Needs PUBLIC_URL. The local admin always works as a fallback.") || strings.Index(mode, "Needs PUBLIC_URL") < strings.Index(mode, "rcards") {
		t.Error("the PUBLIC_URL note sits below the choices")
	}
}

// Why (#338, mockup): the Backups tab always has the card of deleted devices' backups, with its
// empty text, and Advanced shows the container settings as a card, always visible (#350: no disclosure) in two
// tables side by side, the first half of the list on the left.
func TestSystemSettingsBackupsAndAdvancedFollowTheMockup(t *testing.T) {
	e, _, _, _ := storageEnv(t)
	_, page := e.do(t, "GET", "/settings/system?tab=backups", "admin", nil)
	if !strings.Contains(page, `id="card-orphans"`) {
		t.Error("the deleted devices card is missing")
	}
	e2, _, _, _ := sysEnv(t, true)
	_, page = e2.do(t, "GET", "/settings/system?tab=advanced", "admin", nil)
	for _, w := range []string{`id="card-container"`, "Read-only. Change them in the container configuration.", "Bootstrap settings (listeners"} {
		if !strings.Contains(page, w) {
			t.Errorf("Advanced lacks %q", w)
		}
	}
	// Why: a disclosure hid the whole list; the tables must be visible without a click.
	if strings.Contains(page, "container settings</summary>") || strings.Contains(page, "sys-container") {
		t.Error("the container settings are behind a disclosure again")
	}
	card := page[strings.Index(page, `id="card-container"`):]
	if n := strings.Count(card[:strings.Index(card, "</section>")], `class="dense"`); n != 2 {
		t.Errorf("container card has %d tables, want 2 side by side", n)
	}
}

// Why (review of #333): after a refused save the page shows the NEW broker, but the server still
// wants the person to keep, re-enter or remove the password. The option must compare with the SAVED
// broker (data attributes), stay ticked as it was posted, and exist for the single URL field too.
func TestSystemSettingsKeepPasswordOptionSurvivesARefusedSave(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	r, page := e.do(t, "POST", "/settings/system?tab=mqtt", "admin", url.Values{"csrf": {"tok"},
		"mqtt.host": {"new.lan"}, "mqtt.port": {"1883"}, "reuse.MQTT_PASSWORD": {"1"}, "v.BACKUP_KEEP": {"not-a-number"}})
	if r.StatusCode != 422 {
		t.Fatalf("status %d, want the page again with an error", r.StatusCode)
	}
	if !strings.Contains(page, `data-stored-host="broker.lan"`) || !strings.Contains(page, `value="new.lan"`) {
		t.Error("the option compares with the saved broker while the field shows the new one")
	}
	if !regexp.MustCompile(`name="reuse.MQTT_PASSWORD" value="1" checked`).MatchString(page) {
		t.Error("the ticked option must come back ticked")
	}
	// the single URL field (a stored URL the parts cannot hold) offers the option too
	_, page = e.do(t, "POST", "/settings/system?tab=mqtt", "admin", url.Values{"csrf": {"tok"},
		"v.MQTT_URL": {"tcp://broker.lan/path"}, "v.BACKUP_KEEP": {"not-a-number"}})
	if !strings.Contains(page, `name="v.MQTT_URL"`) || !strings.Contains(page, `name="reuse.MQTT_PASSWORD"`) {
		t.Error("the single URL field lacks the keep option")
	}
}
