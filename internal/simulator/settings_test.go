package simulator

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func dumpOf(t *testing.T, d Device) map[string]any {
	t.Helper()
	b, err := fetchDump(ctxT(t), d)
	if err != nil {
		t.Fatal(err)
	}
	return decodeDumpDoc(t, b)
}

func fetchDump(ctx context.Context, d Device) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+d.Addr()+"/dl", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(resp.Body)
}

// dumpUp is dumpOf once the device runs again: like cmUp, it rides out the
// restart that Hostname, Topic, FullTopic and the MQTT settings queue ~2 s
// after they change, which may still be pending when the last command returned.
func dumpUp(t *testing.T, d Controller) map[string]any {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := d.WaitFor(ctx, func(c Controller) bool { return c.Phase() == PhaseRun })
		cancel()
		if err != nil {
			t.Fatalf("/dl: device did not come back: phase %s", d.Phase())
		}
		b, err := fetchDump(ctxT(t), d)
		if err == nil {
			return decodeDumpDoc(t, b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("/dl: %v", err)
		}
		time.Sleep(50 * time.Millisecond) // a restart was queued after the phase check
	}
}

func decodeDumpDoc(t *testing.T, b []byte) map[string]any {
	t.Helper()
	if len(b) != 4096 {
		t.Fatalf("dump size %d", len(b))
	}
	dec, err := DecodeDump(b)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(dec, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func newSettingsDevice(t *testing.T) Controller {
	return add(t, newTestFleet(t, "", Extra{}), DeviceSpec{Name: "plug", Profile: ProfileESP8266_4M})
}

// cmUp sends cmnd once the device runs again. Hostname, Topic, FullTopic and the
// MQTT settings restart the simulated device shortly after they change (as
// Tasmota does), so a command right after them can meet the reboot gap.
func cmUp(t *testing.T, d Controller, cmnd string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := d.WaitFor(ctx, func(c Controller) bool { return c.Phase() == PhaseRun })
		cancel()
		if err != nil {
			t.Fatalf("%s: device did not come back: phase %s", cmnd, d.Phase())
		}
		b, code, err := cmWith(d, cmnd, 3*time.Second, nil)
		if err == nil && code == 200 {
			return b
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: code %d err %v", cmnd, code, err)
		}
		time.Sleep(50 * time.Millisecond) // a restart was queued after the phase check
	}
}

// Why: every command the settings editor can write must change what the
// device reports, otherwise "change, read back, compare" tests prove nothing.
// (The map order is random, so restart-causing settings land anywhere in the
// loop; every command goes through cmUp.)
func TestEditorFieldsAreStateful(t *testing.T) {
	d := newSettingsDevice(t)
	set := map[string]string{
		"FriendlyName1 Desk": `{"FriendlyName1":"Desk"}`, "Hostname desk-plug": `{"Hostname":"desk-plug"}`,
		"TelePeriod 120": `{"TelePeriod":120}`, "PowerOnState 3": `{"PowerOnState":3}`,
		"SetOption3 0": `{"SetOption3":"OFF"}`, "SetOption4 1": `{"SetOption4":"ON"}`,
		"MqttHost broker.test": `{"MqttHost":"broker.test"}`, "MqttPort 1884": `{"MqttPort":1884}`,
		"MqttUser u1": `{"MqttUser":"u1"}`, "NtpServer1 ntp.test": `{"NtpServer1":"ntp.test"}`,
		"Timezone +01:00": `{"Timezone":"+01:00"}`, "FullTopic %prefix%/%hostname%/": `{"FullTopic":"%prefix%/%hostname%/"}`,
		"TimeDst 0,0,3,1,2,120": `{"TimeDst":{"Hemisphere":0,"Week":0,"Month":3,"Day":1,"Hour":2,"Offset":120}}`,
	}
	for cmd, want := range set {
		if got := cmUp(t, d, cmd); got != want {
			t.Errorf("%s: %s want %s", cmd, got, want)
		}
		q := strings.Fields(cmd)[0]
		if got := cmUp(t, d, q); got != want && !strings.HasPrefix(q, "TimeDst") {
			t.Errorf("query %s: %s want %s", q, got, want)
		}
	}
	if got := cmUp(t, d, "MqttPassword2 secret-pw"); got != `{"MqttPassword":"****"}` || strings.Contains(cmUp(t, d, "Status 6"), "secret-pw") {
		t.Errorf("password must never be echoed: %s", got)
	}
	s0 := cmUp(t, d, "Status 0")
	for _, want := range []string{`"FriendlyName":["Desk"]`, `"PowerOnState":3`, `"TelePeriod":120`, `"MqttHost":"broker.test"`, `"MqttPort":1884`, `"MqttUser":"u1"`, `"Timezone":"+01:00"`, `"Hostname":"desk-plug"`} {
		if !strings.Contains(s0, want) {
			t.Errorf("Status 0 lacks %s", want)
		}
	}
	doc := dumpUp(t, d)
	flag := doc["flag"].(map[string]any)
	if doc["tele_period"] != float64(120) || doc["poweronstate"] != float64(3) || doc["mqtt_host"] != "broker.test" || doc["mqtt_pwd"] != "secret-pw" ||
		flag["mqtt_enabled"] != float64(0) || flag["mqtt_response"] != float64(1) || doc["friendlyname"].([]any)[0] != "Desk" {
		t.Errorf("dump does not reflect the changes: %v", doc)
	}
}

// Why: Hostname, Topic and the MQTT settings queue a restart a moment later. A
// value set in that window must be flashed by the restart, like the device does;
// otherwise a caller that changes several settings in a row (the editor, a
// restore) sees them vanish depending on timing, and a test of it flakes.
func TestSettingChangedWhileRestartIsPendingSurvivesIt(t *testing.T) {
	d := newSettingsDevice(t)
	cm(t, d, "Hostname queued-restart") // queues the restart
	cm(t, d, "TelePeriod 120")          // arrives before it happens
	settled(t, d, 1)
	if got := cm(t, d, "TelePeriod"); got != `{"TelePeriod":120}` {
		t.Errorf("a value set while the restart was pending was lost: %s", got)
	}
}

// Why: SaveData 0 is the fleet policy; its flash semantics decide whether a
// "backup after change" shows the change and whether a power cut loses it.
func TestSaveDataSemantics(t *testing.T) {
	d := newSettingsDevice(t)
	cm(t, d, "TelePeriod 120")
	if dumpOf(t, d)["tele_period"] != float64(120) {
		t.Error("/dl dumps the RAM settings")
	}
	d.PowerCycle()
	settled(t, d, 1)
	if got := cm(t, d, "TelePeriod"); got != `{"TelePeriod":300}` {
		t.Errorf("unsaved value must be lost on power cycle, got %s", got)
	}
	cm(t, d, "TelePeriod 120")
	cm(t, d, "SaveData") // a bare SaveData calls SettingsSaveAll
	d.PowerCycle()
	settled(t, d, 2)
	if got := cm(t, d, "TelePeriod"); got != `{"TelePeriod":120}` {
		t.Errorf("saved value must survive, got %s", got)
	}
	cm(t, d, "SaveData 60") // periodic saving: later changes are flushed
	cm(t, d, "TelePeriod 90")
	d.PowerCycle()
	settled(t, d, 3)
	if got := cm(t, d, "TelePeriod"); got != `{"TelePeriod":90}` {
		t.Errorf("with SaveData > 0 changes persist, got %s", got)
	}
}

// Why: Restart (and OTA) are save points; settings that restart the device
// are saved by that restart.
func TestRestartAndRestartingSettingsSave(t *testing.T) {
	d := newSettingsDevice(t)
	cm(t, d, "TelePeriod 200")
	cm(t, d, "Restart 1")
	settled(t, d, 1)
	eq(t, "after Restart", cm(t, d, "TelePeriod"), `{"TelePeriod":200}`)
	cm(t, d, "Hostname moved")
	cm(t, d, "MqttUser other") // second restarting setting: one restart only
	settled(t, d, 2)
	time.Sleep(300 * time.Millisecond)
	eq(t, "one restart for both", d.Boots(), 2)
	if got := cm(t, d, "Status 5"); !strings.Contains(got, `"Hostname":"moved"`) {
		t.Error(got)
	}
	eq(t, "MqttUser saved", cm(t, d, "MqttUser"), `{"MqttUser":"other"}`)
}

// Why: Tasmota clamps instead of failing; a simulator that stored the raw
// value would hide clamping surprises from the settings editor.
func TestValueRulesLikeTasmota(t *testing.T) {
	d := newSettingsDevice(t)
	for cmd, want := range map[string]string{
		"TelePeriod 5": `{"TelePeriod":10}`, "TelePeriod 9999": `{"TelePeriod":3600}`, "TelePeriod 1": `{"TelePeriod":300}`, "TelePeriod 0": `{"TelePeriod":0}`,
		"PowerOnState 7": `{"PowerOnState":0}`, "SetOption3 maybe": `{"SetOption3":"ON"}`, "Timezone 25": `{"Timezone":99}`,
	} {
		if got := cm(t, d, cmd); got != want {
			t.Errorf("%s: %s want %s", cmd, got, want)
		}
	}
}

// Why: SetOption words in Status 3 must stay consistent with the single
// reads, as the policy view and the editor read both.
func TestSetOptionBitsInStatus3(t *testing.T) {
	d := newSettingsDevice(t)
	before := cm(t, d, "Status 3")
	cm(t, d, "SetOption19 1")
	cm(t, d, "SetOption65 1")
	cm(t, d, "SetOption36 7")
	after := cm(t, d, "Status 3")
	if before == after || !strings.Contains(after, `"SetOption":["40288008"`) && !strings.Contains(after, "SetOption") {
		t.Fatal(after)
	}
	if !strings.Contains(after, `"SetOption":["40288008",`) {
		t.Errorf("word 0 must have bit 19 set: %s", after)
	}
	if !strings.Contains(after, "07") { // SetOption36 is param byte 4
		t.Errorf("param byte missing: %s", after)
	}
	eq(t, "SetOption65", cm(t, d, "SetOption65"), `{"SetOption65":"ON"}`)
	eq(t, "SetOption36", cm(t, d, "SetOption36"), `{"SetOption36":7}`)
}

// Why: minimal firmware has none of these commands, so a settings change
// there is impossible and must look like Unknown.
func TestMinimalHasNoSettings(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "lamp", Profile: ProfileESP8266Min})
	for _, c := range []string{"TelePeriod 120", "SetOption3 1", "Hostname x", "SaveData"} {
		if got := cm(t, d, c); !strings.Contains(got, `"Command":"Unknown"`) {
			t.Errorf("%s: %s", c, got)
		}
	}
	if resp, _, _ := cmWithPath(d, "/dl", ""); !strings.Contains(resp, "File Not Found") {
		t.Error("minimal has no /dl")
	}
}

// Why: the fake decoder must refuse foreign data like the real one would.
func TestDecodeDumpRejectsForeignData(t *testing.T) {
	if _, err := DecodeDump(make([]byte, 4096)); err == nil {
		t.Error("an all-zero dump is not a simulator dump")
	}
}

// Why: the editor list must be fully covered; every key here appears in
// internal/settings.Fields (M2-2) as a command.
func TestAllEditorCommandsKnown(t *testing.T) {
	d := newSettingsDevice(t)
	for _, c := range []string{"FriendlyName1", "FriendlyName2", "FriendlyName3", "FriendlyName4", "Hostname", "Topic", "FullTopic", "TelePeriod",
		"PowerOnState", "SetOption0", "SetOption3", "SetOption4", "SetOption19", "MqttHost", "MqttPort", "MqttUser", "MqttPassword2",
		"NtpServer1", "NtpServer2", "NtpServer3", "Timezone", "TimeDst", "TimeStd"} {
		if got := cm(t, d, c); strings.Contains(got, "Unknown") || strings.Contains(got, "Error") {
			t.Errorf("%s: %s", c, got)
		}
	}
}

// Why: the simulator once answered {"Sleep":50}, so tests passed while a real device never matched
// a Sleep rule. Tasmota v16 (CmndSleep, S_JSON_COMMAND_NVALUE_ACTIVE_NVALUE) answers
// {"Sleep":{"<configured>":{"Active":"<active>"}}}; LedState (ResponseCmndNumber) a plain number.
func TestSleepAndLedStateReplyLikeV16(t *testing.T) {
	d := newSettingsDevice(t)
	if got := cm(t, d, "Sleep 50"); got != `{"Sleep":{"50":{"Active":"50"}}}` {
		t.Errorf("Sleep 50 -> %s", got)
	}
	if got := cm(t, d, "Sleep"); got != `{"Sleep":{"50":{"Active":"50"}}}` {
		t.Errorf("Sleep -> %s", got)
	}
	if got := cm(t, d, "LedState 3"); got != `{"LedState":3}` {
		t.Errorf("LedState 3 -> %s", got)
	}
}
