package web

import (
	"context"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
)

// Why: the tabs place every field by a table (sysPlace). A setting that no tab draws can never
// be changed in the UI, which breaks "env = bootstrap only, the rest in the UI"; every field
// must have an input (or its read-only form) on exactly one tab.
//
// The snapshot schedule is on System -> Snapshots (#410), so that page counts as part of the settings here.
func TestSystemSettingsEveryFieldHasOnePlace(t *testing.T) {
	s := snapSetup(t, true)
	e, svc := s.env, s.svc
	_, body := e.do(t, "GET", "/settings/system", "admin", nil)
	_, snaps := e.do(t, "GET", "/settings/system/snapshots", "admin", nil)
	body += snaps
	for _, st := range svc.State() {
		name := `name="v.` + st.Key + `"`
		if st.Key == "MQTT_URL" {
			name = `name="mqtt.host"` // N18: the URL is edited as host, port and TLS
		}
		n := strings.Count(body, name)
		if st.Key == "AUTH_MODE" || st.Key == "LOG_LEVEL" || sysSegmentedEnum(st.Key) {
			n = min(n, 1) // radio inputs: one per choice
		}
		if len(st.Choices) > 0 {
			n = min(n, 1) // one checkbox per choice
		}
		if st.Key == "BACKUP_INTERVAL" {
			n = strings.Count(body, name) // the interval input; the schedule switch is sw.BACKUP_INTERVAL
		}
		if n != 1 {
			t.Errorf("%s: %d inputs on the page, want exactly 1", st.Key, n)
		}
	}
}

// Why (#151): the scrape token is edited on the page as a write-only field. An environment
// value (the Unraid CA template sets one) locks it read-only, and neither state may put the
// token into the HTML.
func TestSystemSettingsMetricsTokenFieldAndLock(t *testing.T) {
	e, svc, _, _ := sysEnv(t, true)
	if _, err := svc.Apply(t.Context(), sysconfig.Request{Actor: "admin", StepUp: sysconfig.StepUp{Done: true, Possible: true},
		Values: map[string]string{"METRICS_TOKEN": "fake-page-token-5"}}); err != nil {
		t.Fatal(err)
	}
	_, body := e.do(t, "GET", "/settings/system", "admin", nil)
	if !strings.Contains(body, `type="password" name="v.METRICS_TOKEN"`) {
		t.Errorf("token must be a write-only password input:\n%s", body)
	}
	if strings.Contains(body, "fake-page-token-5") {
		t.Error("stored token leaked into the page")
	}

	e, _, _, _ = sysEnvFull(t, true, nil, "TASMOFLEET_METRICS_TOKEN=fake-env-token-6")
	_, body = e.do(t, "GET", "/settings/system", "admin", nil)
	if strings.Contains(body, `name="v.METRICS_TOKEN"`) || !strings.Contains(body, "METRICS_TOKEN in the container configuration") {
		t.Errorf("env token must lock the field:\n%s", body)
	}
	if strings.Contains(body, "fake-env-token-6") {
		t.Error("env token leaked into the page")
	}
}

// Why: an on/off setting is a checkbox, and a checkbox that is off sends nothing. The page
// marks each checkbox it drew (b.KEY), so "off" is saved as false instead of being ignored;
// the marker is accepted only for known on/off settings (no mass assignment).
func TestSystemSettingsCheckboxOffIsSaved(t *testing.T) {
	e, svc, _, _ := sysEnv(t, true)
	if r, _ := e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "b.METRICS_DEVICE_LABELS": {"1"}, "v.METRICS_DEVICE_LABELS": {"true"}}); r.StatusCode != 303 || !svc.Snapshot().MetricsDeviceLabels {
		t.Fatalf("checked box: %d", r.StatusCode)
	}
	if r, _ := e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "b.METRICS_DEVICE_LABELS": {"1"}}); r.StatusCode != 303 || svc.Snapshot().MetricsDeviceLabels {
		t.Fatalf("a box that is off must store false: %d", r.StatusCode)
	}
	for _, form := range []url.Values{{"b.NO_SUCH": {"1"}}, {"b.BACKUP_KEEP": {"1"}}, {"b.METRICS_DEVICE_LABELS": {"true"}}} {
		form.Set("csrf", "tok")
		if r, _ := e.do(t, "POST", "/settings/system", "admin", form); r.StatusCode != 400 {
			t.Errorf("%v: status %d, want 400", form, r.StatusCode)
		}
	}
	// "Reset to default" with the box off resets; it is not turned into "false".
	if r, _ := e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "b.METRICS_DEVICE_LABELS": {"1"}, "clear.METRICS_DEVICE_LABELS": {"1"}}); r.StatusCode != 303 {
		t.Fatalf("reset with the box off: %d", r.StatusCode)
	}
}

// Why: a refused sign-in change opens "Confirm it's you" over the tab the person was on. A
// local account confirms with its password (field "current", the route's contract) and comes
// back to that tab with its typed values (restore=1); nothing was saved before that.
func TestSystemSettingsStepUpDialogLocal(t *testing.T) {
	e, svc, _, _ := sysEnv(t, false)
	r, body := e.do(t, "POST", "/settings/system?tab=signin", "admin", url.Values{"csrf": {"tok"}, "v.ADMIN_GROUPS": {"root"}})
	if r.StatusCode != 403 || svc.Snapshot().AdminGroups != nil && strings.Join(svc.Snapshot().AdminGroups, ",") == "root" {
		t.Fatalf("status %d; nothing may be saved", r.StatusCode)
	}
	for _, want := range []string{`<dialog id="stepup" class="udlg stepup"`, `data-autoopen`, `action="/tf/settings/system/stepup?tab=signin"`, `name="current"`, "Changing Admin groups"} {
		if !strings.Contains(body, want) {
			t.Errorf("dialog lacks %q", want)
		}
	}
	if !strings.Contains(body, `id="sec-signin"`) || strings.Contains(body, `id="sec-signin" hidden`) {
		t.Error("the dialog must sit over the tab of the refused save")
	}
	r, body = e.do(t, "POST", "/settings/system/stepup?tab=signin", "admin", url.Values{"csrf": {"tok"}, "current": {"wrong"}})
	if r.StatusCode != 403 || !strings.Contains(body, "The password is wrong.") || !strings.Contains(body, `data-autoopen`) {
		t.Fatalf("wrong password: %d; the dialog must say why and stay open", r.StatusCode)
	}
	// The retry no longer carries the refused keys; the dialog must still name what is confirmed.
	if strings.Contains(body, "<b>Changing </b>") || !strings.Contains(body, "<b>Changing the sign-in settings</b>") {
		t.Error("after a wrong password the dialog must still say what the confirmation is for")
	}
	r, _ = e.do(t, "POST", "/settings/system/stepup?tab=signin", "admin", url.Values{"csrf": {"tok"}, "current": {"right"}})
	if loc := r.Header.Get("Location"); r.StatusCode != 303 || !strings.Contains(loc, "tab=signin") || !strings.Contains(loc, "restore=1") {
		t.Fatalf("right password: %d %q", r.StatusCode, loc)
	}
}

// Why: a single sign-on admin confirms at the identity provider (POST /oidc/stepup with next),
// never with a password field the server would refuse for them.
func TestSystemSettingsStepUpDialogOIDC(t *testing.T) {
	e, _, _, _ := sysEnv(t, false)
	_, body := e.do(t, "POST", "/settings/system?tab=signin", "admin", url.Values{"csrf": {"tok"}, "v.ADMIN_GROUPS": {"root"}}, "X-Test-Method", "oidc")
	if !strings.Contains(body, `formaction="/tf/oidc/stepup"`) || !strings.Contains(body, `name="next" value="/settings/system?tab=signin&amp;restore=1"`) {
		t.Error("the identity provider button must post to /oidc/stepup and come back to the tab")
	}
	if strings.Contains(body, `name="current"`) {
		t.Error("an OIDC account gets no password field")
	}
}

// Why: the Discovery tab tells whether MQTT discovery can find devices right now; "listening"
// while the broker is down would send the admin looking for devices that cannot arrive.
func TestSystemSettingsDiscoveryStatus(t *testing.T) {
	for _, c := range []struct {
		mqtt, on, want string
	}{
		{"connected", "true", "MQTT discovery is listening"},
		{"disconnected", "true", "MQTT discovery is waiting for the broker"},
		{"disabled", "true", "MQTT discovery needs a broker"},
		{"connected", "false", "MQTT discovery is off"},
	} {
		e, _, _, _ := sysEnvWithDeps(t, func(d *Deps) { state := c.mqtt; d.MQTT = func() string { return state } })
		e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "b.DISCOVERY_MQTT": {"1"}, "v.DISCOVERY_MQTT": {c.on}})
		_, body := e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
		if !strings.Contains(body, "<b>"+c.want+"</b>") {
			t.Errorf("MQTT %s, discovery %s: want %q", c.mqtt, c.on, c.want)
		}
	}
}

// Why: an on/off setting posts "true" when it is on; a checkbox without a value posts "on", which
// the server refuses ("strconv.ParseBool"), so saving any tab failed while the switch was on.
func TestSystemSettingsCheckboxesPostTrue(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	_, body := e.do(t, "GET", "/settings/system", "admin", nil)
	boxes := regexp.MustCompile(`<input type="checkbox" role="switch"[^>]*name="v\.[A-Z_]+"[^>]*>`).FindAllString(body, -1)
	if len(boxes) == 0 {
		t.Fatal("no on/off setting drawn as a checkbox")
	}
	for _, b := range boxes {
		if !strings.Contains(b, `value="true"`) {
			t.Errorf("checkbox without value=\"true\": %s", b)
		}
	}
}

// storageBackups is the backup service with the N9 storage summary and orphans on top of
// deleting and keeping (newAdminEnv's fake).
type storageBackups struct {
	*keepBackups
	usage   []store.BackupUsage
	orphans []model.Backup
	gone    map[model.BackupID]bool // rows whose file does not exist
}

// FileMissing and Open model rows whose file was deleted by hand (#205).
func (s *storageBackups) FileMissing(b model.Backup) bool { return s.gone[b.ID] }
func (s *storageBackups) Open(ctx context.Context, id model.BackupID) ([]byte, model.Backup, error) {
	if s.gone[id] {
		for _, b := range append(append([]model.Backup{}, s.list...), s.orphans...) {
			if b.ID == id {
				return nil, b, backup.ErrFileMissing
			}
		}
	}
	return s.keepBackups.Open(ctx, id)
}

func (s *storageBackups) Usage(context.Context) ([]store.BackupUsage, error) { return s.usage, nil }
func (s *storageBackups) Orphans(context.Context) ([]model.Backup, error)    { return s.orphans, nil }

// DeleteRun of an orphan answers with device 0, like the manager does.
func (s *storageBackups) DeleteRun(ctx context.Context, actor string, id model.BackupID) (backup.Deletion, error) {
	d, err := s.keepBackups.DeleteRun(ctx, actor, id)
	for _, o := range s.orphans {
		if o.ID == id {
			d.Device = 0
		}
	}
	return d, err
}

func storageEnv(t *testing.T) (*env, *storageBackups, *sysMem, *atomic.Bool) {
	t.Helper()
	t0 := time.Date(2026, 10, 18, 3, 0, 0, 0, time.UTC)
	sb := &storageBackups{keepBackups: &keepBackups{adminBackups: &adminBackups{}, deviceOf: 1}}
	// 12 devices with backups (sizes 1..12 KiB, device 12 the largest), two without, and the
	// deleted devices' row.
	for i := 1; i <= 12; i++ {
		sb.usage = append(sb.usage, store.BackupUsage{Device: model.DeviceID(i), Name: fmt.Sprintf("plug-%02d", i),
			Runs: 3, Kept: i % 2, Bytes: int64(i) * 1024, Last: t0})
	}
	sb.usage = append(sb.usage, store.BackupUsage{Device: 13, Name: "fresh"}, store.BackupUsage{Device: 14, Name: "new"},
		store.BackupUsage{Device: 0, Runs: 2, Kept: 2, Bytes: 2048, Last: t0})
	sb.orphans = []model.Backup{
		{ID: 70, DeviceName: `garage <b>plug</b>`, DeviceMAC: "AA:BB:CC:00:00:70", Kind: model.BackupConfigDump, TakenAt: t0, Size: 1024, Kept: true},
		{ID: 71, DeviceName: `garage <b>plug</b>`, DeviceMAC: "AA:BB:CC:00:00:70", Kind: model.BackupSnapshot, TakenAt: t0, Size: 100, Kept: true},
		{ID: 72, DeviceName: "", DeviceMAC: "", Kind: model.BackupConfigDump, TakenAt: t0.Add(-time.Hour), Size: 1024},
	}
	sb.list = sb.orphans
	e, _, mem, st := sysEnvWithDeps(t, func(d *Deps) { d.Backups = sb })
	return e, sb, mem, st
}

// Why (N9): the Backups tab answers "how much space do the backups take, and which devices
// have none". The totals include the deleted devices' backups (they are on disk), the table
// shows the ten largest devices first and says how many it leaves out, so a long fleet does
// not push the schedule settings off the page.
func TestSettingsBackupsStorageSummaryAndLargestFirst(t *testing.T) {
	e, _, _, _ := storageEnv(t)
	r, body := e.do(t, "GET", "/settings/system?tab=backups", "admin", nil)
	if r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	sums := regexp.MustCompile(`<div class="sys-sum-label">([^<]+)</div><div class="sys-sum-value">([^<]+)</div>`).FindAllStringSubmatch(body, -1)
	got := map[string]string{}
	for _, m := range sums {
		got[m[1]] = m[2]
	}
	want := map[string]string{"Backups": "38", "Total size": "80.0 KiB", "Kept (never pruned)": "8", "Devices without a backup": "2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("totals %v, want %v", got, want)
	}
	if n := strings.Count(body, `<a class="sys-store-row"`); n != 10 {
		t.Errorf("%d device rows, want the top 10", n)
	}
	if i, j := strings.Index(body, ">plug-12<"), strings.Index(body, ">plug-11<"); i < 0 || j < 0 || i > j {
		t.Error("the largest device must come first")
	}
	if strings.Contains(body, ">plug-02<") || !strings.Contains(body, "Largest first · 2 more devices") {
		t.Error("the two smallest devices are summed up as 2 more devices")
	}
	if !strings.Contains(body, `href="/tf/devices/12/backups"`) {
		t.Error("a row opens the device's Backups tab")
	}
	if strings.Index(body, "Storage</h2>") > strings.Index(body, "Device backup schedule") {
		t.Error("the storage summary comes before the schedule, as in the prototype")
	}
}

// Why (N2/N9): a device deleted with "keep the backups" leaves backups no device page lists;
// Settings -> Backups is their only home. Each run appears once (not once per file), with the
// device's name escaped and its MAC, and the same Download / Keep / Delete as on a device.
func TestSettingsBackupsListsOrphansWithTheirActions(t *testing.T) {
	e, _, _, _ := storageEnv(t)
	_, body := e.do(t, "GET", "/settings/system?tab=backups", "admin", nil)
	if strings.Contains(body, "<b>plug</b>") || !strings.Contains(body, "garage &lt;b&gt;plug&lt;/b&gt;") {
		t.Fatal("the deleted device's name must be escaped")
	}
	if n := strings.Count(body, `class="sys-orphan"`); n != 2 {
		t.Errorf("%d orphan rows, want 2 runs (dump and snapshot of one run are one row)", n)
	}
	for _, want := range []string{
		"AA:BB:CC:00:00:70", "Deleted device",
		`href="/tf/backups/70"`, `form="bk-orph" formaction="/tf/backups/70/keep" name="kept" value="0"`,
		`formaction="/tf/backups/72/keep" name="kept" value="1"`,
		`data-dlg-action="/tf/backups/70/delete"`, `<dialog id="dlg-bkdel"`, `<form id="bk-orph" method="post"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("orphans lack %q", want)
		}
	}
	// The keep form must not sit inside the settings form (nested forms are dropped by the parser).
	if strings.Index(body, `<form id="bk-orph"`) < strings.Index(body, "</form>") {
		t.Error("the keep form is nested in the settings form")
	}
}

// Why (N9): deleting a backup of a deleted device used to send the admin to
// /devices/0/backups, a page that does not exist. Both the no-JS confirmation (its Cancel and
// trail) and the delete itself must lead back to Settings -> Backups, with the notice.
func TestOrphanDeleteGoesBackToSettingsBackups(t *testing.T) {
	e, sb, _, _ := storageEnv(t)
	_, body := e.do(t, "GET", "/backups/70/delete", "admin", nil)
	if strings.Contains(body, "/devices/0") || !strings.Contains(body, `href="/tf/settings/system?tab=backups"`) {
		t.Fatalf("the confirmation must link back to the settings:\n%s", body)
	}
	r, _ := e.do(t, "POST", "/backups/70/delete", "admin", url.Values{"csrf": {"tok"}})
	if loc := r.Header.Get("Location"); r.StatusCode != 303 || !strings.HasSuffix(loc, "/settings/system?tab=backups&ok=deleted&n=1") {
		t.Fatalf("orphan delete: %d %q", r.StatusCode, loc)
	}
	if len(sb.deleted) != 1 || sb.deleted[0] != 70 {
		t.Fatalf("deleted %v", sb.deleted)
	}
	if _, body := e.do(t, "GET", "/settings/system?tab=backups&ok=deleted&n=1", "admin", nil); !strings.Contains(body, "Backup deleted.") {
		t.Error("the settings page must confirm the delete")
	}
	// A device's own backup still goes back to the device.
	r, _ = e.do(t, "POST", "/backups/4/delete", "admin", url.Values{"csrf": {"tok"}})
	if loc := r.Header.Get("Location"); !strings.HasSuffix(loc, "/devices/1/backups?ok=deleted&n=1") {
		t.Errorf("device backup delete: %q", loc)
	}
}

// Why (N9): "Reset settings…" changes the sign-in method, the broker and the device networks
// at once. Without a fresh confirmation it opens "Confirm it's you" naming the reset and
// changes nothing; after it, every field row goes, but the internal rows stay: the setup
// marker (or the first-run setup would start again) and the live check pause (only Resume ends
// a pause). The reset is audited.
func TestSettingsResetNeedsStepUpAndKeepsInternalRows(t *testing.T) {
	var aud *fakeAudit
	e, svc, mem, st := sysEnvFull(t, false, func(d *Deps) { aud = d.Recorder.(*fakeAudit) })
	if r, _ := e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "v.BACKUP_KEEP": {"4"}}); r.StatusCode != 303 {
		t.Fatalf("save: %d", r.StatusCode)
	}
	mem.rows["_setup_done"] = store.SystemSetting{Key: "_setup_done", Value: "1"}
	mem.rows[sysconfig.LivePausedKey] = store.SystemSetting{Key: sysconfig.LivePausedKey, Value: "1"}
	r, body := e.do(t, "POST", "/settings/system/reset", "admin", url.Values{"csrf": {"tok"}})
	if r.StatusCode != 403 || !strings.Contains(body, "<b>Resetting all settings</b>") || !strings.Contains(body, `action="/tf/settings/system/stepup?tab=advanced"`) {
		t.Fatalf("without step-up: %d; the dialog must name the reset and come back to Advanced", r.StatusCode)
	}
	if _, ok := mem.rows["MQTT_URL"]; !ok {
		t.Fatal("a refused reset removed settings")
	}
	st.Store(true)
	r, _ = e.do(t, "POST", "/settings/system/reset", "admin", url.Values{"csrf": {"tok"}})
	if loc := r.Header.Get("Location"); r.StatusCode != 303 || !strings.HasSuffix(loc, "/settings/system?tab=advanced&ok=reset") {
		t.Fatalf("reset: %d %q", r.StatusCode, loc)
	}
	for _, k := range []string{"MQTT_URL", "MQTT_PASSWORD", "BACKUP_KEEP"} {
		if _, ok := mem.rows[k]; ok {
			t.Errorf("%s still stored", k)
		}
	}
	for _, k := range []string{"_setup_done", sysconfig.LivePausedKey} {
		if _, ok := mem.rows[k]; !ok {
			t.Errorf("internal row %s removed", k)
		}
	}
	if svc.Snapshot().MQTTURL != "" || svc.Snapshot().BackupKeep != 10 {
		t.Errorf("not at the defaults: %q keep=%d", svc.Snapshot().MQTTURL, svc.Snapshot().BackupKeep)
	}
	if ev := aud.events[len(aud.events)-1]; ev.Action != "sysconfig.reset" || ev.Result != "ok" || ev.Actor == "" || !strings.Contains(ev.Detail, "BACKUP_KEEP") {
		t.Errorf("audit %+v", ev)
	}
	if _, body := e.do(t, "GET", "/settings/system?tab=advanced&ok=reset", "admin", nil); !strings.Contains(body, "All settings are back at their defaults.") {
		t.Error("the page must confirm the reset")
	}
}

// Why: header principals cannot confirm (the proxy is their only authority), so they cannot
// reset; operators never reach the route.
func TestSettingsResetRefusedForHeaderPrincipalsAndOperators(t *testing.T) {
	e, _, mem, _ := sysEnv(t, true)
	r, body := e.do(t, "POST", "/settings/system/reset", "admin", url.Values{"csrf": {"tok"}}, "X-Test-Method", "header")
	if r.StatusCode != 403 || !strings.Contains(body, "Nothing was reset.") || len(mem.rows) == 0 {
		t.Fatalf("header principal: %d", r.StatusCode)
	}
	for _, m := range []string{"GET", "POST"} {
		if r, _ := e.do(t, m, "/settings/system/reset", "operator", url.Values{"csrf": {"tok"}}); r.StatusCode != 403 {
			t.Errorf("%s as operator: %d", m, r.StatusCode)
		}
	}
}

// Why: the Advanced tab carries the prototype's "Reset all settings" box; the button opens the
// dialog, and without JavaScript its link leads to a confirmation page that posts the reset.
func TestSettingsAdvancedOffersResetWithDialogAndPage(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	_, body := e.do(t, "GET", "/settings/system?tab=advanced", "admin", nil)
	for _, want := range []string{"Reset all settings", "Devices, backups and users are kept.",
		`href="/tf/settings/system/reset" data-dialog="dlg-reset"`, `<dialog id="dlg-reset"`, `action="/tf/settings/system/reset"`} {
		if !strings.Contains(body, want) {
			t.Errorf("advanced tab lacks %q", want)
		}
	}
	r, body := e.do(t, "GET", "/settings/system/reset", "admin", nil)
	if r.StatusCode != 200 || !strings.Contains(body, `action="/tf/settings/system/reset"`) || !strings.Contains(body, `href="/tf/settings/system?tab=advanced"`) {
		t.Fatalf("confirmation page: %d", r.StatusCode)
	}
}

// Why: the last backup sits in a 110 px column; a full date with the year overflows it, so the
// current year is left out (the full time is in the title) and only an older backup names its year.
func TestStoreDateFitsTheColumn(t *testing.T) {
	now := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	if got := storeDate(now, time.Date(2026, 10, 23, 5, 45, 0, 0, time.UTC)); got != "23 Oct, 05:45" {
		t.Errorf("this year: %q", got)
	}
	if got := storeDate(now, time.Date(2025, 12, 31, 23, 0, 0, 0, time.UTC)); got != "31 Dec 2025" {
		t.Errorf("last year: %q", got)
	}
}

// Why (#205): a user who deleted a backup directory by hand is left with rows that point
// nowhere. They must be recognisable in both lists, offer no Download or Restore (which can
// only fail), stay deletable so the entry can be removed, and the direct URLs must answer
// with a reason instead of an internal error.
func TestMissingFileRowsAreMarkedRefusedAndDeletable(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 3, 0, 0, 0, time.UTC)
	sb := &storageBackups{keepBackups: &keepBackups{adminBackups: &adminBackups{}, deviceOf: 1}, gone: map[model.BackupID]bool{70: true, 5: true}}
	sb.orphans = []model.Backup{
		{ID: 70, DeviceName: "gone plug", Kind: model.BackupConfigDump, TakenAt: t0, Size: 1024},
		{ID: 72, DeviceName: "fine plug", Kind: model.BackupConfigDump, TakenAt: t0.Add(-time.Hour), Size: 1024},
	}
	sb.list = []model.Backup{
		{ID: 5, DeviceID: 1, Kind: model.BackupConfigDump, TakenAt: t0, Size: 4096},
		{ID: 6, DeviceID: 1, Kind: model.BackupConfigDump, TakenAt: t0.Add(-time.Hour), Size: 4096},
	}
	e, _, _, _ := sysEnvWithDeps(t, func(d *Deps) { d.Backups = sb; d.Restore = &fakeRestore{} })

	_, body := e.do(t, "GET", "/settings/system?tab=backups", "admin", nil)
	if n := strings.Count(body, "File missing"); n != 1 {
		t.Errorf("kept-backups list: %d File missing badges, want 1", n)
	}
	if strings.Contains(body, `href="/tf/backups/70"`) || !strings.Contains(body, `href="/tf/backups/72"`) {
		t.Error("only the present file may offer a Download")
	}
	if !strings.Contains(body, `data-dlg-action="/tf/backups/70/delete"`) {
		t.Error("the missing row must stay deletable")
	}

	_, body = e.do(t, "GET", "/devices/1/backups", "admin", nil)
	if n := strings.Count(body, "File missing"); n != 1 {
		t.Errorf("device list: %d File missing badges, want 1", n)
	}
	if strings.Contains(body, `/backups/5/restore`) || strings.Contains(body, `href="/tf/backups/5"`) || !strings.Contains(body, `/backups/6/restore`) {
		t.Error("restore and download are offered for the present file only")
	}
	if !strings.Contains(body, `data-dlg-action="/tf/backups/5/delete"`) {
		t.Error("the missing row must stay deletable on the device")
	}

	for _, p := range []string{"/backups/5", "/backups/70", "/backups/5/restore"} {
		resp, b := e.do(t, "GET", p, "admin", nil)
		if resp.StatusCode == 500 || !strings.Contains(b, "missing") {
			t.Errorf("%s: %d, want a page that says the file is missing", p, resp.StatusCode)
		}
	}
	if resp, b := e.do(t, "GET", "/backups/70/delete", "admin", nil); resp.StatusCode != 200 || strings.Contains(b, "internal error") {
		t.Errorf("delete confirmation of a missing row: %d", resp.StatusCode)
	}
}

// unrefStorage adds the optional unreferenced-files list to the storage fake.
type unrefStorage struct {
	*storageBackups
	files []backup.UnreferencedFile
}

func (u *unrefStorage) Unreferenced(context.Context) ([]backup.UnreferencedFile, error) {
	return u.files, nil
}

// Why (#205): files in old backup folders that no backup entry points to used to be
// invisible, which looked like a half-finished migration. Settings -> Backups lists them
// (read-only: no delete or keep control, TasmoFleet never touches them) and says so; with
// none, the section does not exist.
func TestSettingsBackupsListsUnreferencedFilesReadOnly(t *testing.T) {
	_, sb, _, _ := storageEnv(t)
	f := []backup.UnreferencedFile{{Path: "backups/85/20261027T104345Z-dmp.dmp", Size: 2048, ModTime: time.Date(2026, 10, 27, 10, 43, 45, 0, time.UTC)}}
	e, _, _, _ := sysEnvWithDeps(t, func(d *Deps) { d.Backups = &unrefStorage{storageBackups: sb, files: f} })
	_, body := e.do(t, "GET", "/settings/system?tab=backups", "admin", nil)
	for _, want := range []string{"Unreferenced files", "backups/85/20261027T104345Z-dmp.dmp", "2.0 KiB", "you can remove them by hand"} {
		if !strings.Contains(body, want) {
			t.Errorf("section lacks %q", want)
		}
	}
	section := body[strings.Index(body, "Unreferenced files"):]
	if strings.Contains(section, "/delete") || strings.Contains(section, "bk-keep") {
		t.Error("the section is read-only: no delete or keep control")
	}
	e2, _, _, _ := sysEnvWithDeps(t, func(d *Deps) { d.Backups = &unrefStorage{storageBackups: sb} })
	if _, body := e2.do(t, "GET", "/settings/system?tab=backups", "admin", nil); strings.Contains(body, "Unreferenced files") {
		t.Error("no section without files")
	}
}

// Why (#381): the "Devices without a backup" tile on Settings -> Backups must open exactly the
// devices it counts. Tile, list chip and list rows come from one predicate (withoutBackup); if
// any of them drifts (e.g. a chip that also counts devices of deleted backups) the numbers on
// the two pages stop agreeing and the click leads to a list that does not match the tile.
func TestBackupsTileOpensTheListOfDevicesWithoutABackup(t *testing.T) {
	sb := &storageBackups{keepBackups: &keepBackups{adminBackups: &adminBackups{}, deviceOf: 1}}
	sb.usage = []store.BackupUsage{
		{Device: 1, Name: "kitchen", Runs: 2, Bytes: 10, Last: time.Now()}, // has backups
		{Device: 2, Name: "attic"},                       // none
		{Device: 0, Runs: 3, Bytes: 5, Last: time.Now()}, // deleted devices' backups: not a device
	}
	e, _, _, _ := sysEnvWithDeps(t, func(d *Deps) { d.Backups = sb })

	_, page := e.do(t, "GET", "/settings/system?tab=backups", "admin", nil)
	m := regexp.MustCompile(`<a class="sys-sum" href="([^"]+)"><div class="sys-sum-label">Devices without a backup</div><div class="sys-sum-value">(\d+)</div></a>`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("the tile is not a link: %s", page)
	}
	if m[1] != "/tf/devices?chip=nobackup" {
		t.Errorf("tile href %q", m[1])
	}
	r, list := e.do(t, "GET", strings.TrimPrefix(m[1], "/tf"), "admin", nil)
	if r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	rows := regexp.MustCompile(`<li class="dcard" data-id="(\d+)">`).FindAllStringSubmatch(list, -1)
	if len(rows) != 1 || rows[0][1] != "2" {
		t.Fatalf("list shows %v, want only device 2 (attic)", rows)
	}
	if strconv.Itoa(len(rows)) != m[2] {
		t.Errorf("tile counts %s, list shows %d", m[2], len(rows))
	}
	chip := regexp.MustCompile(`<a class="dv-chip"[^>]*aria-current="true"[^>]*>No backup<span class="dv-n">(\d+)</span>`).FindStringSubmatch(list)
	if chip == nil || chip[1] != m[2] {
		t.Errorf("the active chip must say No backup with the tile's count %s, got %v", m[2], chip)
	}
}
