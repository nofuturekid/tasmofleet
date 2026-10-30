package settings

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// ---- fakes ----

type fakeDevices struct{}

func (fakeDevices) GetDevice(_ context.Context, id model.DeviceID) (model.Device, error) {
	return model.Device{ID: id, Address: "10.0.0.9", HTTPUser: "admin", HTTPPassword: "fake-pw"}, nil
}

type fakeClient struct {
	mu      sync.Mutex
	sent    []string
	replies map[string]string // command (case as sent) -> JSON reply
	errs    map[string]error
}

func (f *fakeClient) Command(_ context.Context, _ tasmota.Target, cmnd string) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, cmnd)
	if err := f.errs[cmnd]; err != nil {
		return nil, err
	}
	if r, ok := f.replies[cmnd]; ok {
		return json.RawMessage(r), nil
	}
	return json.RawMessage(`{"Command":"ok"}`), nil
}
func (*fakeClient) DownloadConfig(context.Context, tasmota.Target) ([]byte, error) { return nil, nil }
func (*fakeClient) RootPage(context.Context, tasmota.Target) (tasmota.RootInfo, error) {
	return tasmota.RootInfo{}, nil
}

type fakeAudit struct{ events []model.AuditEvent }

func (f *fakeAudit) Record(_ context.Context, e model.AuditEvent) { f.events = append(f.events, e) }

func newSvc(c *fakeClient) (*Manager, *fakeAudit) {
	a := &fakeAudit{}
	return New(Options{Devices: fakeDevices{}, Client: c, Audit: a}), a
}

// ---- validators ----

// Why: every value ends up in a device command line. A validator that lets a
// bad value through is a command-injection or bricking risk, so each field has
// accepted and rejected examples.
func TestFieldValidators(t *testing.T) {
	cases := []struct {
		key  string
		good []string
		bad  []string
	}{
		{"FriendlyName1", []string{"Kitchen", "Küche 2"}, []string{"", "0", "1", `"x`, " pad", "a;Reset 1", "a\nb", strings.Repeat("x", 33), "a；Reset", "a;Reset", "tab\there"}},
		{"Hostname", []string{"office-monitor", "a"}, []string{"", "0", "1", "-lead", "trail-", "has space", "under_score", "a.b", strings.Repeat("a", 33), "x;y"}},
		{"Topic", []string{"office_monitor.1", "tasmota-1"}, []string{"", "0", "1", "a/b", "a b", "a#", "a+", "a;b", strings.Repeat("a", 33)}},
		{"FullTopic", []string{"%prefix%/%topic%/", "tele/%hostname%/"}, []string{"", "%prefix%/", "%topic%", "/%topic%/", "a/%topic%/#", "%topic%/ x/", "%topic%/;"}},
		{"TelePeriod", []string{"0", "10", "300", "3600"}, []string{"", "1", "9", "3601", "-1", "abc", "030", "3 0"}},
		{"PowerOnState", []string{"0", "1", "2", "3", "4", "5"}, []string{"", "6", "-1", "off", "0;1"}},
		{"SaveData", []string{"0", "1", "3600"}, []string{"", "-1", "3601", "x"}},
		{"SetOption0", []string{"0", "1"}, []string{"", "2", "ON", "00"}},
		{"SetOption3", []string{"0", "1"}, []string{"2"}},
		{"SetOption4", []string{"0", "1"}, []string{"2"}},
		{"SetOption19", []string{"0", "1"}, []string{"2"}},
		{"MqttHost", []string{"mqtt.example.internal", "172.31.12.61", "fd00::1"}, []string{"", "0", "1", "a b", "a;b", "http://x", "not:ipv6", strings.Repeat("a", 99)}},
		{"MqttPort", []string{"1", "1883", "65535"}, []string{"", "0", "65536", "-1", "18 83"}},
		{"MqttUser", []string{"tasmota", "user.1"}, []string{"", "0", "1", "a b", "a;b", strings.Repeat("a", 65)}},
		{"MqttPassword", []string{"fake-pass-1"}, []string{"", "a b", "a;Reset 1", "a\nb"}},
		{"NtpServer1", []string{"pool.ntp.org", "10.0.0.1"}, []string{"", "0", "1", "a b", "a;b"}},
		{"NtpServer2", []string{"pool.ntp.org"}, []string{"a/b"}},
		{"NtpServer3", []string{"pool.ntp.org"}, []string{"a,b"}},
		{"Timezone", []string{"99", "0", "1", "-5", "+13", "14", "+14:00", "+01:00", "-05:30", "-00:00", "+00:30"}, []string{"", "15", "-14", "-14:00", "-00:30", "+15:00", "1:00", "+01:60", "100", "x"}},
		{"TimeDst", []string{"0,0,3,1,2,120", "1,4,10,7,23,-900"}, []string{"", "0,0,3,1,2", "0,0,3,1,2,120,5", "2,0,3,1,2,120", "0,5,3,1,2,120", "0,0,13,1,2,120", "0,0,3,0,2,120", "0,0,3,1,24,120", "0,0,3,1,2,901", "a,b,c,d,e,f"}},
		{"TimeStd", []string{"0,0,10,1,3,60"}, []string{"0,0,10,1,3"}},
		{"OtaUrl", []string{"http://ota.tasmota.com/tasmota/release/tasmota.bin.gz"}, []string{"", "1", "http://u:p@h/a.bin"}}, // more in TestOtaUrlValidator
	}
	covered := map[string]bool{}
	for _, c := range cases {
		covered[c.key] = true
		f, ok := Lookup(c.key)
		if !ok {
			t.Fatalf("no field %s", c.key)
		}
		for _, v := range c.good {
			if err := f.Validate(v); err != nil {
				t.Errorf("%s: %q rejected: %v", c.key, v, err)
			}
		}
		for _, v := range c.bad {
			if err := f.Validate(v); err == nil {
				t.Errorf("%s: %q accepted", c.key, v)
			}
		}
	}
	for _, f := range Fields {
		if f.Key != "FriendlyName2" && f.Key != "FriendlyName3" && f.Key != "FriendlyName4" && !covered[f.Key] {
			t.Errorf("field %s has no validator test", f.Key)
		}
	}
}

// Why: SetOption numbers alone are meaningless (plan rule 6); the UI must
// always show the internal name and a description.
func TestEverySetOptionLabelHasNameAndDescription(t *testing.T) {
	n := 0
	for _, f := range Fields {
		if !strings.HasPrefix(f.Key, "SetOption") {
			continue
		}
		n++
		if !strings.HasPrefix(f.Label, f.Key+" (") || !strings.Contains(f.Label, "): ") || strings.HasSuffix(f.Label, "): ") || strings.Contains(f.Label, "`") {
			t.Errorf("%s label %q lacks name and description", f.Key, f.Label)
		}
		if Label(f.Key) != f.Label {
			t.Errorf("Label(%s) differs from the field label", f.Key)
		}
	}
	if n != 4 {
		t.Fatalf("expected SetOption0/3/4/19, got %d fields", n)
	}
	if got := Label("SetOption0"); got != "SetOption0 (save_state): restore relay state after power loss" {
		t.Errorf("Label(SetOption0) = %q", got)
	}
	if got := Label("SetOption77"); got != "SetOption77" {
		t.Errorf("unknown SetOption must keep its bare name, got %q", got)
	}
	if got := Label("SaveData"); got != "Save data (s)" {
		t.Errorf("Label(SaveData) = %q", got)
	}
}

func TestQueryable(t *testing.T) {
	for _, ok := range []string{"SaveData", "SetOption0", "SetOption65", "Timezone"} {
		if !Queryable(ok) {
			t.Errorf("%s should be queryable", ok)
		}
	}
	for _, no := range []string{"Reset", "Restart", "Upgrade", "SetOption", "SetOption1234", "SetOptionX", "Backlog", "MqttPassword", ""} {
		if Queryable(no) {
			t.Errorf("%s must not be queryable", no)
		}
	}
}

// ---- Apply ----

// Why: each changed field is exactly one command (no Backlog), in form order.
func TestApplySendsOneCommandPerField(t *testing.T) {
	c := &fakeClient{}
	svc, aud := newSvc(c)
	res, err := svc.Apply(t.Context(), "alice", 7, map[string]string{"TelePeriod": "60", "FriendlyName1": "Lamp"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.sent, "|"); got != "FriendlyName1 Lamp|TelePeriod 60" {
		t.Fatalf("sent %q", got)
	}
	if len(res) != 2 || !res[0].OK() || !res[1].OK() {
		t.Fatalf("results %+v", res)
	}
	if len(aud.events) != 2 || aud.events[0].Actor != "alice" || aud.events[0].Action != "settings.set" || aud.events[0].Target != "device:7" {
		t.Fatalf("audit %+v", aud.events)
	}
	// Why (D5): every device write says which transport carried it; a plain HTTP client is "http".
	if !strings.Contains(aud.events[0].Detail, `"via":"http"`) {
		t.Errorf("audit detail has no via: %s", aud.events[0].Detail)
	}
}

// Why: a crafted POST must not reach the device with a key outside the
// curated list (it could be Reset or Backlog).
func TestApplyUnknownKeySendsNothing(t *testing.T) {
	for _, k := range []string{"Reset", "Backlog", "Upgrade", "SetOption1", "friendlyname1"} {
		c := &fakeClient{}
		svc, aud := newSvc(c)
		_, err := svc.Apply(t.Context(), "alice", 1, map[string]string{"TelePeriod": "60", k: "1"})
		if !errors.Is(err, ErrUnknownField) {
			t.Errorf("%s: err %v", k, err)
		}
		if len(c.sent) != 0 || len(aud.events) != 0 {
			t.Errorf("%s: sent %v, audited %v", k, c.sent, aud.events)
		}
	}
}

// Why: injection. One bad value aborts the whole request before any device I/O,
// so a half-applied form never happens.
func TestApplyInvalidValueSendsNothing(t *testing.T) {
	c := &fakeClient{}
	svc, _ := newSvc(c)
	_, err := svc.Apply(t.Context(), "alice", 1, map[string]string{
		"TelePeriod": "60", "FriendlyName1": "x;Reset 1", "Hostname": "ok", "Topic": "a\nStatus 0",
	})
	var ve *ValidationError
	if !errors.As(err, &ve) || !errors.Is(err, ErrInvalid) {
		t.Fatalf("err %v", err)
	}
	if _, ok := ve.Fields["FriendlyName1"]; !ok || ve.Fields["Topic"] == "" || len(ve.Fields) != 2 {
		t.Fatalf("fields %v", ve.Fields)
	}
	if len(c.sent) != 0 {
		t.Fatalf("sent %v", c.sent)
	}
}

func TestApplyOversizedValueRejected(t *testing.T) {
	c := &fakeClient{}
	svc, _ := newSvc(c)
	_, err := svc.Apply(t.Context(), "a", 1, map[string]string{"FriendlyName1": strings.Repeat("x", MaxValueLen+1)})
	if !errors.Is(err, ErrInvalid) || len(c.sent) != 0 {
		t.Fatalf("err %v sent %v", err, c.sent)
	}
}

// Why: secrets must not leak into results (shown in the UI) or the audit log.
func TestApplySecretNeverLeaks(t *testing.T) {
	const pw = "distinct-fake-mqtt-pw-42"
	c := &fakeClient{}
	svc, aud := newSvc(c)
	res, err := svc.Apply(t.Context(), "alice", 3, map[string]string{"MqttPassword": pw, "MqttUser": "u"})
	if err != nil {
		t.Fatal(err)
	}
	// Why index 2: "MqttPassword <pw>" makes Tasmota echo the password in
	// clear text on stat/<topic>/RESULT and in its web log; "MqttPassword2"
	// answers with asterisks.
	if got := strings.Join(c.sent, "|"); got != "MqttUser u|MqttPassword2 "+pw {
		t.Fatalf("the device must get the real value via the masked index, sent %q", got)
	}
	blob, _ := json.Marshal(res)
	for _, e := range aud.events {
		blob = append(blob, e.Detail+e.Result+e.Action+e.Target...)
	}
	if strings.Contains(string(blob), pw) {
		t.Fatalf("secret leaked: %s", blob)
	}
	if res[1].Command != "MqttPassword ***" {
		t.Fatalf("command shown as %q", res[1].Command)
	}
}

// Why: a dead device must not make the user wait for one timeout per field.
func TestApplyStopsAfterTransportError(t *testing.T) {
	c := &fakeClient{errs: map[string]error{"FriendlyName1 A": tasmota.ErrUnreachable}}
	svc, aud := newSvc(c)
	res, err := svc.Apply(t.Context(), "a", 1, map[string]string{"FriendlyName1": "A", "TelePeriod": "60", "Topic": "t"})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.sent) != 1 || len(res) != 3 || res[0].OK() || res[1].OK() || res[2].OK() {
		t.Fatalf("sent %v results %+v", c.sent, res)
	}
	if !strings.Contains(res[1].Err, "not sent") || len(aud.events) != 1 || aud.events[0].Result == "ok" {
		t.Fatalf("results %+v audit %+v", res, aud.events)
	}
}

// Why: other device errors (unknown command on an old firmware) affect only that field.
func TestApplyContinuesAfterDeviceError(t *testing.T) {
	c := &fakeClient{errs: map[string]error{"Hostname h": tasmota.ErrUnknownCommand}}
	svc, _ := newSvc(c)
	res, _ := svc.Apply(t.Context(), "a", 1, map[string]string{"Hostname": "h", "TelePeriod": "60"})
	if len(c.sent) != 2 || !res[0].OK() || res[1].OK() { // Hostname restarts, so it is sent last
		t.Fatalf("sent %v results %+v", c.sent, res)
	}
}

// ---- Read ----

// Why: policy comparison needs one canonical form; the device answers ON/OFF,
// numbers, strings and a TimeDst object.
func TestReadNormalisesReplies(t *testing.T) {
	c := &fakeClient{
		replies: map[string]string{
			"SetOption3":    `{"SetOption3":"OFF"}`,
			"Status":        `{"Status":{"PowerOnState":0,"SaveData":0,"SaveState":1}}`,
			"Status 3":      `{"StatusLOG":{"TelePeriod":300}}`,
			"Topic":         `{"Topic":"office"}`,
			"Timezone":      `{"Timezone":99}`,
			"TimeDst":       `{"TimeDst":{"Hemisphere":0,"Week":0,"Month":3,"Day":1,"Hour":2,"Offset":120}}`,
			"FriendlyName2": `{"Command":"Unknown"}`,
			"Hostname":      `not json`,
		},
		errs: map[string]error{"FriendlyName2": tasmota.ErrUnknownCommand},
	}
	svc, _ := newSvc(c)
	got, err := svc.Read(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"SetOption0": "1", "SetOption3": "0", "SaveData": "0", "PowerOnState": "0", "TelePeriod": "300", "Topic": "office", "Timezone": "99", "TimeDst": "0,0,3,1,2,120",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["FriendlyName2"]; ok {
		t.Error("an unknown command must be absent, not empty")
	}
	if _, ok := got["Hostname"]; ok {
		t.Error("an unparsable reply must be absent, not guessed")
	}
	for _, s := range c.sent {
		if s == "MqttPassword" || s == "MqttPassword2" {
			t.Errorf("Read must never touch the secret field, sent %q", s)
		}
		// Why: a bare SaveData runs SettingsSaveAll() (flash write) and a bare
		// TelePeriod forces a telemetry publish; Status reports without effect.
		for _, bad := range SideEffectQueries {
			if s == bad {
				t.Errorf("Read sent the side-effect query %q", s)
			}
		}
	}
}

// Why: a Read must be able to fail loudly when the device is down.
func TestReadTransportErrorAborts(t *testing.T) {
	c := &fakeClient{errs: map[string]error{"FriendlyName1": tasmota.ErrUnreachable}}
	svc, _ := newSvc(c)
	if _, err := svc.Read(t.Context(), 1); !errors.Is(err, tasmota.ErrUnreachable) {
		t.Fatalf("err %v", err)
	}
	if len(c.sent) != 1 {
		t.Fatalf("kept querying a dead device: %v", c.sent)
	}
}

func TestReadKeysRefusesNonQueryable(t *testing.T) {
	c := &fakeClient{}
	svc, _ := newSvc(c)
	for _, k := range []string{"Reset", "MqttPassword", "Backlog"} {
		if _, err := svc.ReadKeys(t.Context(), 1, []string{"SaveData", k}); !errors.Is(err, ErrNotQueryable) {
			t.Errorf("%s: err %v", k, err)
		}
	}
	if len(c.sent) != 0 {
		t.Fatalf("sent %v", c.sent)
	}
	got, err := svc.ReadKeys(t.Context(), 1, []string{"SetOption65"})
	if err != nil || len(got) != 0 || len(c.sent) != 1 || c.sent[0] != "SetOption65" {
		t.Fatalf("got %v err %v sent %v", got, err, c.sent)
	}
}

func TestReadKeysSaveDataAndTelePeriodUseStatus(t *testing.T) {
	c := &fakeClient{replies: map[string]string{
		"Status":   `{"Status":{"PowerOnState":3,"SaveData":0}}`,
		"Status 3": `{"StatusLOG":{"TelePeriod":60}}`,
	}}
	svc, _ := newSvc(c)
	got, err := svc.ReadKeys(t.Context(), 1, []string{"SaveData", "PowerOnState", "TelePeriod"})
	if err != nil || got["SaveData"] != "0" || got["PowerOnState"] != "3" || got["TelePeriod"] != "60" {
		t.Fatalf("got %v err %v", got, err)
	}
	if strings.Join(c.sent, "|") != "Status|Status 3" {
		t.Fatalf("one Status per block, nothing bare: %v", c.sent)
	}
}

// Why: Tasmota restarts about a second after Hostname/Topic/Mqtt*; commands
// sent after them race the reboot.
func TestApplySendsRestartingCommandsLast(t *testing.T) {
	c := &fakeClient{}
	svc, _ := newSvc(c)
	_, err := svc.Apply(t.Context(), "a", 1, map[string]string{
		"Hostname": "h-1", "FriendlyName1": "Lamp", "MqttHost": "mqtt.example", "TelePeriod": "60", "Topic": "t-1", "Timezone": "99",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "FriendlyName1 Lamp|TelePeriod 60|Timezone 99|Hostname h-1|Topic t-1|MqttHost mqtt.example"
	if got := strings.Join(c.sent, "|"); got != want {
		t.Fatalf("sent %q", got)
	}
}

// Why: with SaveData 0 nothing persists settings; Save is the one explicit write.
func TestSaveSendsSaveDataWithCurrentValue(t *testing.T) {
	c := &fakeClient{}
	svc, aud := newSvc(c)
	r, err := svc.Save(t.Context(), "alice", 4, "0")
	if err != nil || !r.OK() || strings.Join(c.sent, "|") != "SaveData 0" {
		t.Fatalf("r %+v err %v sent %v", r, err, c.sent)
	}
	if len(aud.events) != 1 || aud.events[0].Target != "device:4" {
		t.Fatalf("audit %+v", aud.events)
	}
	for _, bad := range []string{"", "-1", "3601", "x", "0;Reset 1"} {
		if _, err := svc.Save(t.Context(), "a", 4, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q accepted", bad)
		}
	}
	if len(c.sent) != 1 {
		t.Fatalf("sent %v", c.sent)
	}
}

// strictClient fails the test when a command with a side effect is sent bare:
// a bare SaveData runs SettingsSaveAll() (flash write), a bare TelePeriod
// forces a telemetry publish. Status blocks answer from fixed data.
type strictClient struct {
	t    *testing.T
	sent []string
}

func (c *strictClient) Command(_ context.Context, _ tasmota.Target, cmnd string) (json.RawMessage, error) {
	c.sent = append(c.sent, cmnd)
	for _, bad := range SideEffectQueries {
		if cmnd == bad {
			c.t.Errorf("bare side-effect query sent: %q", cmnd)
		}
	}
	switch cmnd {
	case "Status":
		return json.RawMessage(`{"Status":{"PowerOnState":0,"SaveData":0,"SaveState":0}}`), nil
	case "Status 3":
		return json.RawMessage(`{"StatusLOG":{"TelePeriod":300}}`), nil
	}
	return json.RawMessage(`{"Command":"Unknown"}`), tasmota.ErrUnknownCommand
}
func (*strictClient) DownloadConfig(context.Context, tasmota.Target) ([]byte, error) { return nil, nil }
func (*strictClient) RootPage(context.Context, tasmota.Target) (tasmota.RootInfo, error) {
	return tasmota.RootInfo{}, nil
}

// Why: the policy poller reads every device every hour; one bare SaveData in
// that path would write flash fleet-wide. Every curated setting and a range of
// SetOptions go through the strict client.
func TestNoSideEffectQueryIsEverSentBare(t *testing.T) {
	c := &strictClient{t: t}
	svc := New(Options{Devices: fakeDevices{}, Client: c})
	var keys []string
	for _, f := range Fields {
		if !f.Secret {
			keys = append(keys, f.Key)
		}
	}
	keys = append(keys, "SetOption19", "SetOption65")
	got, err := svc.ReadKeys(t.Context(), 1, keys)
	if err != nil {
		t.Fatal(err)
	}
	if got["SaveData"] != "0" || got["TelePeriod"] != "300" || got["PowerOnState"] != "0" || got["SetOption0"] != "0" {
		t.Fatalf("status-backed settings: %v", got)
	}
	for _, s := range c.sent {
		if s == "SetOption0" {
			t.Error("SetOption0 must come from Status.SaveState, not a query")
		}
	}
	if _, err := svc.Read(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
}

func TestLiveReadable(t *testing.T) {
	for _, c := range []string{"SaveData", "TelePeriod", "PowerOnState", "SetOption0", "SetOption65", "Timezone", "Topic"} {
		if !LiveReadable(c) {
			t.Errorf("%s should be readable live", c)
		}
	}
	for _, c := range []string{"MqttPassword", "Reset", "Backlog", ""} {
		if LiveReadable(c) {
			t.Errorf("%s must not be readable live", c)
		}
	}
}

// Why: MQTT keeps the cached values fresh without any device traffic, but only
// for messages whose meaning is certain.
func TestParseReply(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]string
	}{
		{`{"Status":{"PowerOnState":3,"SaveData":0,"SaveState":1}}`, map[string]string{"PowerOnState": "3", "SaveData": "0", "SetOption0": "1"}},
		{`{"StatusLOG":{"TelePeriod":60}}`, map[string]string{"TelePeriod": "60"}},
		{`{"SetOption0":"ON"}`, map[string]string{"SetOption0": "1"}},
		{`{"setoption19":"OFF"}`, map[string]string{"SetOption19": "0"}},
		{`{"SaveData":"Every 30 second(s)"}`, map[string]string{"SaveData": "30"}},
		{`{"SaveData":"OFF"}`, map[string]string{"SaveData": "0"}},
		{`{"Timezone":"+01:00"}`, map[string]string{"Timezone": "+01:00"}},
		{`{"POWER":"ON"}`, nil},
		{`{"MqttPassword":"x"}`, nil},
		{`{"StatusMEM":{"Heap":20}}`, nil},
		{`{"Status":{"Topic":"x"}}`, nil},
		{`{"A":1,"B":2}`, nil},
		{`not json`, nil},
	}
	for _, c := range cases {
		got := ParseReply([]byte(c.in))
		if len(got) != len(c.want) {
			t.Errorf("%s -> %v, want %v", c.in, got, c.want)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%s: %s = %q, want %q", c.in, k, got[k], v)
			}
		}
	}
}

// Why: OtaUrl ends up in a device command line and decides where the device
// fetches its firmware from. Only a plain http(s) URL may pass: no credentials
// in the URL (they would be stored, shown and logged), no Backlog separator, no
// blanks, and not Tasmota's "1" shortcut that resets the URL.
func TestOtaUrlValidator(t *testing.T) {
	f, ok := Lookup("OtaUrl")
	if !ok {
		t.Fatal("OtaUrl is not in the catalog")
	}
	for _, v := range []string{
		"http://ota.tasmota.com/tasmota/release/tasmota.bin.gz",
		"http://ota.tasmota.com/tasmota32/release/tasmota32.bin",
		"https://fw.example.invalid:8443/a/b.bin",
	} {
		if err := f.Validate(v); err != nil {
			t.Errorf("%q refused: %v", v, err)
		}
	}
	for _, v := range []string{
		"", "1", "0", "ota.tasmota.com/tasmota.bin", "ftp://h/a.bin", "http://u:p@h/a.bin", "http://h/a.bin;Restart 1",
		"http://h/a b.bin", "http://h/a.bin?x=1", "http:///a.bin", "http://h/" + strings.Repeat("a", 100),
	} {
		if err := f.Validate(v); err == nil {
			t.Errorf("%q must be refused", v)
		}
	}
}

// Why: OtaUrl belongs to the fleet policy only. The device form must neither
// read it on every page load nor let a crafted POST rewrite where a device
// updates from; that change goes through "Make compliant" or the confirmed console.
func TestOtaUrlIsPolicyOnly(t *testing.T) {
	c := &fakeClient{replies: map[string]string{"OtaUrl": `{"OtaUrl":"http://h/a.bin"}`}}
	svc, _ := newSvc(c)
	got, err := svc.Read(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["OtaUrl"]; ok {
		t.Error("Read must not return OtaUrl")
	}
	for _, s := range c.sent {
		if s == "OtaUrl" {
			t.Error("Read queried OtaUrl")
		}
	}
	c.sent = nil
	_, err = svc.Apply(t.Context(), "alice", 1, map[string]string{"OtaUrl": "http://h/a.bin"})
	var ve *ValidationError
	if !errors.As(err, &ve) || len(c.sent) != 0 {
		t.Fatalf("Apply must refuse OtaUrl and send nothing: err %v, sent %v", err, c.sent)
	}
}

// Why: the policy needs the live OtaUrl to flag a device that still points at
// the old TasmoAdmin host. A bare "OtaUrl" only reports (cmdpolicy), and a URL
// with credentials must never reach the report in clear text.
func TestOtaUrlIsReadLiveWithoutCredentials(t *testing.T) {
	if !Queryable("OtaUrl") || !LiveReadable("OtaUrl") {
		t.Fatal("OtaUrl must be queryable and live readable")
	}
	c := &fakeClient{replies: map[string]string{"OtaUrl": `{"OtaUrl":"http://old-host.example.invalid:8080/tasmota.bin.gz"}`}}
	svc, _ := newSvc(c)
	got, err := svc.ReadKeys(t.Context(), 1, []string{"OtaUrl"})
	if err != nil || got["OtaUrl"] != "http://old-host.example.invalid:8080/tasmota.bin.gz" {
		t.Fatalf("got %v, %v", got, err)
	}
	c = &fakeClient{replies: map[string]string{"OtaUrl": `{"OtaUrl":"http://user:fake-pw@h/a.bin"}`}}
	svc, _ = newSvc(c)
	got, _ = svc.ReadKeys(t.Context(), 1, []string{"OtaUrl"})
	if v := got["OtaUrl"]; strings.Contains(v, "fake-pw") || strings.Contains(v, "user") || v == "" {
		t.Errorf("credentials leaked or value dropped: %q", v)
	}
	mq := ParseReply([]byte(`{"OtaUrl":"http://user:fake-pw@h/a.bin"}`))
	if v := mq["OtaUrl"]; strings.Contains(v, "fake-pw") || v == "" {
		t.Errorf("MQTT reply leaked credentials or dropped the value: %q", v)
	}
}
