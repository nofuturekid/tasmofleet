package web

import (
	"context"
	"errors"
	"html"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// fakeFacts answers the transport facts per device; err makes the read fail.
type fakeFacts struct {
	f   map[model.DeviceID]transport.Facts
	err error
}

func (s fakeFacts) Facts(_ context.Context, id model.DeviceID) (transport.Facts, error) {
	return s.f[id], s.err
}

// mqttFacts are the facts of a device HTTP cannot reach but MQTT can: on our broker, exclusive
// topic, group topic read, last will online. password sets the device's MQTT password mode.
func mqttFacts(now time.Time, password model.MQTTPasswordMode) transport.Facts {
	return transport.Facts{
		Now:  now,
		HTTP: transport.HTTPState{State: transport.HTTPFailing, Kind: transport.HTTPFailUnreachable, Since: now.Add(-time.Hour)},
		MQTT: transport.MQTTFacts{BusConnected: true, Ours: true, Topic: "kitchen", TopicExclusive: true,
			GroupTopic: "tasmotas", GroupTopicReadAt: now, LastProbeAt: now.Add(-time.Minute), LWTOnline: true},
		Password:        transport.Password{Mode: password},
		FirmwareBaseURL: true,
		Identity:        model.Identity{Family: model.ChipESP32, Version: "16.0.0", Variant: "tasmota32", FreeKB: 1700},
	}
}

// FailingHTTP makes the fake a FailingHTTPSource: the facts of the asked devices whose HTTP fails.
func (s fakeFacts) FailingHTTP(_ context.Context, ids []model.DeviceID) (map[model.DeviceID]transport.Facts, error) {
	out := map[model.DeviceID]transport.Facts{}
	for _, id := range ids {
		if f, ok := s.f[id]; ok && f.HTTP.State == transport.HTTPFailing {
			out[id] = f
		}
	}
	return out, s.err
}

func withFacts(facts fakeFacts) func(*Deps) {
	return func(d *Deps) { d.Transport = facts }
}

// Why: the card, the disabled buttons and the router's errors must say the same thing, so the
// page repeats Choose's sentence for an action verbatim (capitalised at the sentence start). A
// device that HTTP cannot reach and that has no MQTT password shows Commands over MQTT but cannot
// be backed up (#261 acceptance).
func TestCardAndDisabledButtonsCarryTheChooserSentence(t *testing.T) {
	now := time.Now()
	f := mqttFacts(now, model.MQTTPasswordNone)
	want := sentence(transport.Choose(f).Reason(transport.ActBackup))
	if !strings.Contains(want, "nter the device's MQTT password to back up over MQTT") {
		t.Fatalf("the chooser no longer says what the acceptance criterion quotes: %q", want)
	}
	e := setupWith(t, withFacts(fakeFacts{f: map[model.DeviceID]transport.Facts{1: f}}))
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)

	if !strings.Contains(body, "How TasmoFleet reaches this device") {
		t.Fatal("no connection card on the overview")
	}
	if n := strings.Count(body, html.EscapeString(want)); n < 2 {
		t.Fatalf("the Back up sentence must appear in the card and on the disabled button (%d times):\n%s", n, want)
	}
	if !strings.Contains(body, `title="`+html.EscapeString(want)+`"`) {
		t.Fatal("the Back up button must carry the sentence as its title")
	}
	// Commands go over MQTT: the card says so and the Console tab is not disabled.
	if !strings.Contains(body, sentence(transport.Choose(f).Reason(transport.ActCommands))[:20]) {
		t.Fatal("the card does not show the Commands sentence")
	}
	if strings.Contains(body, `class="utab" aria-disabled="true" title="`+html.EscapeString(sentence(transport.Choose(f).Reason(transport.ActCommands)))) {
		t.Fatal("Console must stay usable while Commands go over MQTT")
	}
}

// Why: with a device password set, Back up over MQTT is a real path; the button must not be
// disabled by a leftover reason.
func TestBackupButtonIsEnabledWhenMQTTCarriesIt(t *testing.T) {
	f := mqttFacts(time.Now(), model.MQTTPasswordBroker)
	f.Password.Mode = model.MQTTPasswordOwn
	e := setupWith(t, withFacts(fakeFacts{f: map[model.DeviceID]transport.Facts{1: f}}))
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	i := strings.Index(body, ">Back up</button>")
	if i < 0 {
		t.Fatal("no Back up button")
	}
	btn := body[strings.LastIndex(body[:i], "<button"):i]
	if strings.Contains(btn, "disabled") {
		t.Fatalf("Back up must be enabled over MQTT: %s", btn)
	}
}

// Why: operators may look at how a device is reached but must not change what TasmoFleet
// remembers (Forget) - the same rule as for every admin action. The admin gets Forget only while
// a "no" is on record.
func TestForgetIsAdminOnlyAndOnlyWithARecordedNo(t *testing.T) {
	now := time.Now()
	f := mqttFacts(now, model.MQTTPasswordOwn)
	f.HTTP = transport.HTTPState{State: transport.HTTPOK}
	f.FWReach = model.FWReach{State: model.FWReachNo, At: now.Add(-time.Hour)}
	e := setupWith(t, withFacts(fakeFacts{f: map[model.DeviceID]transport.Facts{1: f, 2: mqttFacts(now, model.MQTTPasswordOwn)}}))

	_, admin := e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(admin, `action="/tf/devices/1/fwreach/forget"`) || !strings.Contains(admin, ">Forget</button>") {
		t.Fatal("an admin must get the Forget form when the device is known not to reach the firmware server")
	}
	_, op := e.do(t, "GET", "/devices/1", "operator", nil)
	if strings.Contains(op, "fwreach/forget") || strings.Contains(op, ">Forget<") {
		t.Fatal("an operator must not get Forget")
	}
	if !strings.Contains(op, "How TasmoFleet reaches this device") {
		t.Fatal("an operator still sees the card, read-only")
	}
	_, none := e.do(t, "GET", "/devices/2", "admin", nil)
	if strings.Contains(none, "fwreach/forget") {
		t.Fatal("Forget makes no sense without a recorded no")
	}
}

// Why: a "no" older than 24 h is unknown to Choose; the card must not claim the device still
// fails to reach the server, and must not offer to forget something that no longer counts.
func TestExpiredNoReadsAsUnknown(t *testing.T) {
	now := time.Now()
	f := mqttFacts(now, model.MQTTPasswordOwn)
	f.FWReach = model.FWReach{State: model.FWReachNo, At: now.Add(-30 * time.Hour)}
	e := setupWith(t, withFacts(fakeFacts{f: map[model.DeviceID]transport.Facts{1: f}}))
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if strings.Contains(body, "fwreach/forget") {
		t.Fatal("an expired no must not offer Forget")
	}
	if !strings.Contains(body, "Reaches the firmware server") {
		t.Fatal("firmware server row missing")
	}
}

// Why: the list marker says "TasmoFleet manages this device over MQTT"; a device that merely
// reports to our broker while HTTP works must not carry it.
func TestListMarkerOnlyWhenCommandsGoOverMQTT(t *testing.T) {
	now := time.Now()
	over := mqttFacts(now, model.MQTTPasswordOwn)
	http := mqttFacts(now, model.MQTTPasswordOwn)
	http.HTTP = transport.HTTPState{State: transport.HTTPOK}
	e := setupWith(t, func(d *Deps) { d.Transport = fakeFacts{f: map[model.DeviceID]transport.Facts{1: over, 2: http}} })
	for _, id := range []model.DeviceID{1, 2} {
		dev := e.inv.devs[id]
		dev.MQTT = model.MQTTOurs
		e.inv.devs[id] = dev
	}
	_, body := e.do(t, "GET", "/devices", "admin", nil)
	if n := strings.Count(body, "data-via-mqtt"); n != 1 {
		t.Fatalf("the MQTT marker must appear for exactly the device whose commands go over MQTT, got %d", n)
	}
}

// Why: a failed read of the facts costs the card, not the page; the actions fall back to the
// general reason like before.
func TestPageRendersWithoutFacts(t *testing.T) {
	e := setupWith(t, withFacts(fakeFacts{err: errors.New("db down")}))
	r, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if r.StatusCode != 200 || strings.Contains(body, "How TasmoFleet reaches this device") {
		t.Fatalf("status %d, card shown without facts", r.StatusCode)
	}
	e = setup(t)
	_, body = e.do(t, "GET", "/devices/1", "admin", nil)
	if strings.Contains(body, "How TasmoFleet reaches this device") {
		t.Fatal("no transport service, no card")
	}
}

// Why: a running operation holds the device whatever the path; the per-action reason must not
// replace that sentence by a path that would work.
func TestRunningOperationStillBlocksEveryAction(t *testing.T) {
	tab := deviceTabs{Reason: "Unavailable while the device is being updated.", Health: model.HealthOnline}
	tab.applyPaths(transport.Choose(mqttFacts(time.Now(), model.MQTTPasswordOwn)), true)
	for name, got := range map[string]string{"settings": tab.SettingsWhy, "console": tab.ConsoleWhy, "backup": tab.BackupWhy} {
		if got != tab.Reason {
			t.Fatalf("%s: %q, want %q", name, got, tab.Reason)
		}
	}
}

// Why (review H1): an offline device answers neither HTTP nor a download from the firmware server, so
// Update and Restore stay disabled with the offline sentence for every path that is not over MQTT;
// only a path over MQTT can still work.
func TestOfflineBlocksEveryPathButMQTT(t *testing.T) {
	for _, tc := range []struct {
		path    transport.Path
		blocked bool
	}{
		{transport.HTTP, true}, {transport.DownloadHTTP, true}, {transport.PushHTTP, true}, {transport.PushSafeboot, true},
		{transport.MQTT, false}, {transport.DownloadMQTT, false}, {transport.PushMQTT, false},
	} {
		p := transport.Paths{Commands: tc.path, Writes: tc.path, Backup: tc.path, Restore: tc.path, Update: tc.path}
		tab := deviceTabs{Health: model.HealthOffline, Reason: reasonOffline}
		tab.applyPaths(p, true)
		for name, got := range map[string]string{"update": tab.UpdateWhy, "restore": tab.RestoreWhy, "backup": tab.BackupWhy} {
			if (got == reasonOffline) != tc.blocked || (got != "" && got != reasonOffline) {
				t.Fatalf("%s over %s while offline: %q (blocked=%v)", name, tc.path, got, tc.blocked)
			}
		}
	}
}

// Why (review M1): the header's "Update to" opens the update page, where the delivery is chosen; the
// card's pull-only Update path must not disable it. Only the device's state does.
func TestUpdateButtonIgnoresThePullPath(t *testing.T) {
	tab := deviceTabs{Health: model.HealthOnline}
	tab.applyPaths(transport.Paths{Update: transport.None, Reasons: []transport.Reason{{Action: transport.ActUpdate, Path: transport.None, Text: "no path: x"}}}, true)
	if tab.UpdateWhy == "" || tab.updateBtnWhy() != "" {
		t.Fatalf("card reason %q, button reason %q", tab.UpdateWhy, tab.updateBtnWhy())
	}
	off := deviceTabs{Health: model.HealthOffline, Reason: reasonOffline}
	if off.updateBtnWhy() != reasonOffline {
		t.Fatal("an offline device keeps Update to disabled")
	}
}

// Why (review M2): the live refresh must not close a fold the person opened, so it swaps the
// summaries and the lists by id and carries no <details>.
func TestLiveRefreshSwapsPartsNotFolds(t *testing.T) {
	f := mqttFacts(time.Now(), model.MQTTPasswordOwn)
	e := setupWith(t, withFacts(fakeFacts{f: map[model.DeviceID]transport.Facts{1: f}}))
	_, body := e.do(t, "GET", "/devices/1/live", "admin", nil)
	for _, id := range []string{"devpaths-facts-sum", "devpaths-facts", "devpaths-actions-sum", "devpaths-actions"} {
		if !strings.Contains(body, `id="`+id+`"`) {
			t.Fatalf("live answer lacks %s", id)
		}
	}
	if strings.Contains(body, "<details") {
		t.Fatal("the live answer must not replace the folds")
	}
}

// Why (review M3): a disabled button says why to a screen reader and a touch user, not only in a
// tooltip: aria-disabled plus aria-describedby to a visible line.
func TestDisabledBackupIsDescribedByAVisibleLine(t *testing.T) {
	f := mqttFacts(time.Now(), model.MQTTPasswordNone)
	e := setupWith(t, withFacts(fakeFacts{f: map[model.DeviceID]transport.Facts{1: f}}))
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(body, `id="devhd-why"`) || !strings.Contains(body, `aria-describedby="devhd-why"`) || !strings.Contains(body, `aria-disabled="true"`) {
		t.Fatal("Back up must be aria-disabled and described by the visible reason line")
	}
}

// Why (review L2, L3): the summary line names the firmware "no" and is built from rows by key, so
// reordering the rows cannot change what it says.
func TestFactsSummaryByKey(t *testing.T) {
	c := pathCard{Facts: []pathRow{{Key: keyFW, Value: "No"}, {Key: keyMQTT, Value: "On our broker"}, {Key: keyHTTP, Value: "Not reachable"}}}
	if got, want := c.FactsSummary(), "Connection: HTTP not reachable, MQTT on our broker, firmware server no"; got != want {
		t.Fatalf("%q, want %q", got, want)
	}
}
