package settings

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Why: a generic policy rule makes tasmofleet send a bare command name to the whole fleet.
// Queryable is the gate for that, so it must open for plain settings and stay closed for
// Status, secrets, commands that act, and for another spelling of a curated setting (which
// would skip the curated validation and the restart knowledge).
func TestQueryableGenericCommands(t *testing.T) {
	for cmd, want := range map[string]bool{
		"Sleep": true, "LedState": true, "WebLog": true,
		"Status0": false, "Status": false, "status": false, "Backlog": false, "Reset": false, "Power1": false,
		"MqttPassword": false, "Password": false, "mqttpassword": false,
		"SaveData": true, "TelePeriod": true, // curated: read through their Status block
		"hostname": false, "HOSTNAME": false, "setoption19": false, "SO19": false, "SetOption19": true,
		"Hostname": true, "": false, "Sleep 5": false,
	} {
		if got := Queryable(cmd); got != want {
			t.Errorf("Queryable(%q) = %v, want %v", cmd, got, want)
		}
	}
	if !Generic("Sleep") || Generic("Hostname") || Generic("SetOption19") || Generic("Status0") {
		t.Error("Generic must be true only for readable commands outside the catalog")
	}
}

// Why: Hostname, Topic and the MQTT settings restart the device. Make compliant orders and
// saves around that; a generic rule has no such knowledge, so it must never claim a restart
// (and must not turn a curated setting's restart off by another spelling).
func TestGenericCommandsDoNotRestartTheDevice(t *testing.T) {
	for _, k := range []string{"Sleep", "LedState", "hostname", "SetOption19"} {
		if RestartsDevice(k) {
			t.Errorf("%s restarts the device", k)
		}
	}
	if !RestartsDevice("Hostname") {
		t.Fatal("Hostname restarts the device")
	}
}

// Why: a generic command is read by its bare name, one at a time, and the reply must be exactly
// {"<Command>": scalar}. Any other shape is "unknown", never a guess, and a reply may carry
// credentials, which must not survive into the policy values.
func TestReadKeysGenericCommand(t *testing.T) {
	c := &fakeClient{replies: map[string]string{
		"Sleep":      `{"SLEEP":{"50":{"Active":"100"}}}`,
		"LedState":   `{"LedState":"ON"}`,
		"WebLog":     `{"Command":"Unknown"}`,
		"Latitude":   `{"Latitude":{"a":1}}`,
		"Pulsetime1": `{"PulseTime1":{"Set":100}}`,
		"Interlock":  `{"Interlock":"ON","Other":1}`,
		"Baudrate":   `{"Baudrate":"http://user:s3cret@host/x"}`,
		"Longitude":  `{"Longitude":"password=hunter2"}`,
	}}
	m, _ := newSvc(c)
	got, err := m.ReadKeys(t.Context(), 1, []string{"Sleep", "LedState", "WebLog", "Latitude", "Pulsetime1", "Interlock", "Baudrate", "Longitude"})
	if err != nil {
		t.Fatal(err)
	}
	if got["Sleep"] != "50" || got["LedState"] != "1" {
		t.Errorf("plain values: %v", got)
	}
	for _, k := range []string{"WebLog", "Latitude", "Pulsetime1", "Interlock"} {
		if v, ok := got[k]; ok {
			t.Errorf("%s = %q, want unknown", k, v)
		}
	}
	all := strings.Join([]string{got["Baudrate"], got["Longitude"]}, "|")
	if strings.Contains(all, "s3cret") || strings.Contains(all, "hunter2") {
		t.Errorf("a credential survived: %q", all)
	}
	if got["Baudrate"] == "" || got["Longitude"] == "" {
		t.Errorf("a scrubbed value is still a value: %v", got)
	}
	if want := []string{"Sleep", "LedState", "WebLog", "Latitude", "Pulsetime1", "Interlock", "Baudrate", "Longitude"}; !reflect.DeepEqual(c.sent, want) {
		t.Errorf("sent %v, want the bare names one at a time", c.sent)
	}
}

// Why: Status 0 builds a reply that crashes ESP8266 devices; it must never leave, however the
// caller got the name here, and neither may a secret or an acting command.
func TestReadKeysNeverSendsADeniedCommand(t *testing.T) {
	c := &fakeClient{}
	m, _ := newSvc(c)
	for _, k := range []string{"Status0", "Status 0", "status00", "Status", "Backlog", "Reset", "Restart", "Power", "MqttPassword", "Password", "hostname", "SO19", ""} {
		if _, err := m.ReadKeys(t.Context(), 1, []string{"Sleep", k}); !errors.Is(err, ErrNotQueryable) {
			t.Errorf("ReadKeys(%q): %v, want ErrNotQueryable", k, err)
		}
	}
	if len(c.sent) != 0 {
		t.Fatalf("sent %v although a key was refused", c.sent)
	}
}

// Why: a device announces setting changes on its own (stat/<topic>/RESULT); a generic rule's
// value must be kept from there, with credentials masked, and a secret's key must not become a
// value at all.
func TestParseReplyKeepsGenericKeys(t *testing.T) {
	got := ParseReply([]byte(`{"LedState":"ON"}`))
	if !reflect.DeepEqual(got, map[string]string{"LedState": "1"}) {
		t.Fatalf("got %v", got)
	}
	if got := ParseReply([]byte(`{"Baudrate":"https://u:pw@h/x"}`)); strings.Contains(got["Baudrate"], "pw") || got["Baudrate"] == "" {
		t.Fatalf("credentials in %v", got)
	}
	for _, p := range []string{`{"Password":"s3cret"}`, `{"MqttPassword":"****"}`, `{"Status0":1}`, `{"StatusSNS":{"x":1}}`, `{"Reset":"1"}`, `{"POWER":"ON"}`} {
		if got := ParseReply([]byte(p)); got != nil {
			t.Errorf("ParseReply(%s) = %v, want nil", p, got)
		}
	}
	if got := ParseReply([]byte(`{"SetOption19":"OFF"}`)); got["SetOption19"] != "0" {
		t.Errorf("a curated key still maps to its catalog name: %v", got)
	}
}

// Why: Tasmota v16 answers a bare Sleep with a nested object, not a number: CmndSleep calls
// Response_P(S_JSON_COMMAND_NVALUE_ACTIVE_NVALUE, command, Settings->sleep, TasmotaGlobal.sleep) and
// the format is {"%s":{"%d":{"Active":"%d"}}} (include/i18n.h). A rule Sleep=50 compares the
// configured value (the first %d, the one Make compliant sets); read as a whole object it never
// matched a real device and Make compliant would resend Sleep 50 forever. The simulator answered
// {"Sleep":50}, which hid it.
func TestSleepReplyInTheV16Format(t *testing.T) {
	const real = `{"Sleep":{"50":{"Active":"50"}}}` // printf '{"%s":{"%d":{"Active":"%d"}}}' Sleep 50 50
	m, _ := newSvc(&fakeClient{replies: map[string]string{"Sleep": real}})
	got, err := m.ReadKeys(t.Context(), 1, []string{"Sleep"})
	if err != nil || got["Sleep"] != "50" {
		t.Fatalf("read %v, %v; want Sleep=50", got, err)
	}
	if v := ParseReply([]byte(real)); v["Sleep"] != "50" {
		t.Errorf("a RESULT message (the reply to Make compliant's Sleep 50) gave %v", v)
	}
}

// Why: with dynamic sleep the active value moves (Settings->sleep 50, active 100); the rule is
// about what is configured, so the active value must not decide the match.
func TestSleepRuleComparesTheConfiguredValueNotTheActiveOne(t *testing.T) {
	const dynamic = `{"Sleep":{"50":{"Active":"100"}}}`
	m, _ := newSvc(&fakeClient{replies: map[string]string{"Sleep": dynamic}})
	got, err := m.ReadKeys(t.Context(), 1, []string{"Sleep"})
	if err != nil || got["Sleep"] != "50" {
		t.Fatalf("read %v, %v; want the configured 50", got, err)
	}
	if v := ParseReply([]byte(`{"SLEEP":{"0":{"Active":"1"}}}`)); v["SLEEP"] != "0" {
		t.Errorf("configured 0 (dynamic sleep off) gave %v", v)
	}
}

// Why: only the exact Sleep shape is unwrapped. Any other nested reply stays unknown, never a guess.
func TestSleepUnwrapIsExact(t *testing.T) {
	for _, in := range []string{
		`{"Sleep":{"50":{"Active":"100","X":1}}}`, `{"Sleep":{"abc":{"Active":"1"}}}`, `{"Sleep":{"50":{"Other":"1"}}}`, `{"Sleep":{"50":{"Active":{"x":1}}}}`, `{"Sleep":{"50":{"Active":"x"}}}`,
		`{"Sleep":{"50":{"Active":"1"},"60":{"Active":"1"}}}`, `{"Sleep":{"050":{"Active":"1"}}}`, `{"Sleep":{"-1":{"Active":"1"}}}`,
	} {
		if v := ParseReply([]byte(in)); v != nil {
			t.Errorf("%s gave %v, want unknown", in, v)
		}
	}
	// SerialLog and SysLog use the same S_JSON_COMMAND_NVALUE_ACTIVE_NVALUE format
	if v := ParseReply([]byte(`{"SysLog":{"2":{"Active":"2"}}}`)); v["SysLog"] != "2" {
		t.Errorf("SysLog gave %v", v)
	}
	if v := ParseReply([]byte(`{"SerialLog":{"2":{"Active":"3"}}}`)); v["SerialLog"] != "2" {
		t.Errorf("SerialLog gave %v", v)
	}
	// no heuristics for other commands: the same shape under another name stays unknown
	if v := ParseReply([]byte(`{"WebLog":{"50":{"Active":"1"}}}`)); v != nil {
		t.Errorf("WebLog gave %v", v)
	}
}
