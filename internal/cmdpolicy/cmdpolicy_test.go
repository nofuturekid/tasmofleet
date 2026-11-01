package cmdpolicy

import (
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// TestClassify is the contract of the console: what an operator may send,
// what only an admin sends, and which admin commands name a consequence on a
// confirmation step (decision 2026-10-21: admins may send any Tasmota command;
// destructive and lock-out commands ask once instead of being refused). Every
// row says why.
func TestClassify(t *testing.T) {
	tests := []struct {
		cmd     string
		class   Class
		redact  bool
		confirm bool
		why     string
	}{
		// Reads and the operator set.
		{"Status 1", ClassRead, false, false, "individual Status N queries are the safe way to read"},
		{"Status 11", ClassRead, false, false, "StatusSTS is what the health poll uses"},
		{"status2", ClassRead, false, false, "Tasmota accepts the index glued to the name, and case does not matter"},
		{"Status", ClassRead, false, false, "bare Status returns the abbreviated object only (fixtures README)"},
		{"State", ClassRead, false, false, "state query"},
		{"Power", ClassRead, false, false, "Power without argument only queries"},
		{"Power1", ClassRead, false, false, "indexed query"},
		{"Power ON", ClassOperator, false, false, "switching a relay is the operator's daily job"},
		{"Power1 TOGGLE", ClassOperator, false, false, "per-relay toggle"},
		{"power2 off", ClassOperator, false, false, "case insensitive"},
		{"Power0 1", ClassAdmin, false, false, "Power0 addresses all relays, including ones above 8"},
		{"Power00 1", ClassAdmin, false, false, "same, other spelling"},
		{"SO19", ClassRead, false, false, "SO is the short form of SetOption"},
		{"SO19 1", ClassAdmin, false, false, "writing through the short form is still a write"},
		{"SaveData", ClassAdmin, false, false, "even bare SaveData writes flash"},
		{"SaveData 0", ClassAdmin, false, false, "setting write"},
		{"WebServer", ClassRead, false, false, "bare query"},
		{"Power 2", ClassOperator, false, false, "numeric 2 is TOGGLE"},
		{"Restart 1", ClassOperator, false, false, "plain restart keeps settings"},
		{"Restart 1 ", ClassOperator, false, false, "trailing blanks are trimmed"},
		{"Time", ClassRead, false, false, "time query"},
		{"Uptime", ClassRead, false, false, "uptime query"},
		{"SetOption19", ClassRead, false, false, "an argument-less setting is a query"},
		{"TelePeriod", ClassRead, false, false, "query"},
		{"Hostname", ClassRead, false, false, "query"},
		{"Module", ClassRead, false, false, "query"},
		{"OtaUrl", ClassAdmin, false, false, "the bare form only reads the URL"},
		{"Upgrade", ClassAdmin, false, false, "the bare form is the probe the engine itself sends"},

		// Admin, routine: no second screen. Formerly refused (allowlist, executors).
		{"SetOption19 1", ClassAdmin, false, false, "writes change device behaviour"},
		{"TelePeriod 60", ClassAdmin, false, false, "setting write"},
		{"Power blink", ClassAdmin, false, false, "Blink is outside the toggle set"},
		{"Power9 ON", ClassAdmin, false, false, "relay index beyond the usual range is unusual, not operator business"},
		{"Status 12", ClassAdmin, false, false, "debug status is not part of the operator set"},
		{"Color 255,0,0", ClassAdmin, false, false, "light command"},
		{"FriendlyName1 Küche", ClassAdmin, false, false, "non-ASCII letters are legitimate values"},
		{"FooBar", ClassAdmin, false, false, "admins may send any command: no allowlist"},
		{"Sensor13 1", ClassAdmin, false, false, "sensor and driver commands were off the old allowlist"},
		{"SetSensor1 1", ClassAdmin, false, false, "driver command"},
		{"ZbSend {}", ClassAdmin, false, false, "Zigbee command"},
		{"MqttUser x", ClassAdmin, false, false, "was off the allowlist"},
		{"Json {\"Status\":0}", ClassAdmin, false, false, "executors are no longer a forbidden class"},
		{"Rule1 ON System#Boot DO Power ON ENDON", ClassAdmin, false, false, "rules are an admin tool"},
		{"Rule1 1", ClassAdmin, false, false, "enabling a rule"},
		{"Rule", ClassAdmin, false, false, "bare form"},
		{"RuleTimer1 10", ClassAdmin, false, false, "rule timer"},
		{"Timer1 {\"Enable\":1}", ClassAdmin, false, false, "timers"},
		{"Timers 1", ClassAdmin, false, false, "timers"},
		{"Publish cmnd/x/Power ON", ClassAdmin, false, false, "MQTT publish"},
		{"WebSend [10.0.0.5] Power ON", ClassAdmin, false, false, "sends a command to another device"},
		{"Script >D", ClassAdmin, false, false, "script"},
		{"Br print(1)", ClassAdmin, false, false, "Berry"},
		{"SetOption85 1", ClassAdmin, false, false, "audit row 11: no longer forbidden"},
		{"SO85 1", ClassAdmin, false, false, "short form of the same"},
		{"Backlog Power ON", ClassAdmin, false, false, "Backlog is allowed for admins"},
		{"Backlog", ClassAdmin, false, false, "bare form"},
		{"backlog0 Power ON; Power2 OFF", ClassAdmin, false, false, "Backlog0 and several parts"},
		{"WebServer 2", ClassAdmin, false, false, "admin mode is the one mode that keeps /cm usable"},
		{"Wifi 1", ClassAdmin, false, false, "Wi-Fi settings other than off"},
		{"IPAddress", ClassAdmin, false, false, "bare form is a query"},
		{"Telnet 1", ClassAdmin, false, false, "no forbidden class for admins"},

		// Admin with one confirmation naming the consequence.
		{"Reset 1", ClassAdmin, false, true, "factory reset"},
		{"Reset", ClassAdmin, false, true, "even the bare form"},
		{"Reset 99", ClassAdmin, false, true, "reset variant"},
		{"WifiConfig 2", ClassAdmin, false, true, "puts the device into the Wi-Fi manager"},
		{"Upgrade 1", ClassAdmin, false, true, "firmware outside the OTA engine"},
		{"OtaUrl http://x/y.bin", ClassAdmin, false, true, "firmware URL"},
		{"Restart 2", ClassAdmin, false, true, "Restart 2 halts the device until power is cycled"},
		{"Restart 99", ClassAdmin, false, true, "only Restart 1 is routine"},
		{"Restart", ClassAdmin, false, true, "bare Restart prints help; ask rather than guess"},
		{"Wifi 0", ClassAdmin, false, true, "would cut the device off"},
		{"Wifi off", ClassAdmin, false, true, "same"},
		{"Wifi 00", ClassAdmin, false, true, "atoi reads this as 0"},
		{"Wifi abc", ClassAdmin, false, true, "unclear argument asks"},
		{"WebServer 0", ClassAdmin, false, true, "kills the HTTP API tasmofleet uses"},
		{"webserver OFF", ClassAdmin, false, true, "same"},
		{"WebServer 1", ClassAdmin, false, true, "user mode serves no command, not even WebServer 2 to undo it"},
		{"WebServer 3", ClassAdmin, false, true, "only the bare query and 2 are routine"},
		{"IPAddress1 10.9.9.9", ClassAdmin, false, true, "the device would leave the address we dial"},
		{"IPAddress3 255.0.0.0", ClassAdmin, false, true, "mask"},
		{"Ethernet 0", ClassAdmin, false, true, "interface off"},
		{"Ap 2", ClassAdmin, false, true, "switches the Wi-Fi network"},
		{"Module 18", ClassAdmin, false, true, "changes the pin mapping"},
		{"Template {\"NAME\":\"x\"}", ClassAdmin, false, true, "template change"},
		{"Status 0", ClassAdmin, false, true, "heap burst warning (audit row 10)"},
		{"Status0", ClassAdmin, false, true, "glued form of Status 0"},
		{"Status 00", ClassAdmin, false, true, "atoi makes this Status 0"},
		{"Status +0", ClassAdmin, false, true, "atoi makes this Status 0"},
		{"Status 0x0", ClassAdmin, false, true, "non-canonical number"},
		{"Status 2 3", ClassAdmin, false, true, "extra argument; Tasmota would take the first number"},
		{"Status abc", ClassAdmin, false, true, "non-numeric Status argument"},
		{"Status -1", ClassAdmin, false, true, "negative Status argument"},
		{"Status 1000", ClassAdmin, false, true, "not a plain Status index"},
		{"Password1 hunter2", ClassAdmin, true, true, "Wi-Fi password"},
		{"WebPassword hunter2", ClassAdmin, true, true, "web password"},
		{"MqttPassword hunter2", ClassAdmin, true, true, "secret value"},
		{"SSId2 MyNet", ClassAdmin, true, true, "Wi-Fi name is redacted too"},
		{"TmToken 123:abc", ClassAdmin, true, true, "Telegram bot token"},
		{"IfxPassword x", ClassAdmin, true, true, "InfluxDB password"},
		{"IfxToken x", ClassAdmin, true, true, "InfluxDB token"},
		{"TLSKey1 x", ClassAdmin, true, true, "MQTT client key"},
		{"LoRaWanAppKey1 00", ClassAdmin, true, true, "LoRaWAN app key"},
		{"MqttPassword", ClassAdmin, true, false, "the bare form asks nothing but stays redacted"},
		{"Backlog Power ON; Reset 1", ClassAdmin, false, true, "a Reset inside a Backlog asks like a Reset alone"},
		{"Backlog WebPassword x", ClassAdmin, true, true, "a secret inside a Backlog is redacted and asks"},
		// Tasmota's CmndBacklog strips leading "Backlog" words from every part, case-insensitive, with no
		// space needed; a part spelled that way must be judged as the command behind it.
		{"Backlog BacklogStatus 0", ClassAdmin, false, true, "Status 0 behind a glued Backlog prefix asks"},
		{"Backlog Power1 1;backlogstatus0", ClassAdmin, false, true, "same in the second part"},
		{"Backlog  BACKLOGBacklogStatus 0", ClassAdmin, false, true, "repeated prefixes, any case"},
		{"Backlog Backlog Reset 1", ClassAdmin, false, true, "a Reset behind a prefix asks"},
		{"Backlog BacklogWebPassword x", ClassAdmin, true, true, "a secret behind a prefix is redacted"},
	}
	for _, tc := range tests {
		t.Run(tc.cmd, func(t *testing.T) {
			v, err := Classify(tc.cmd)
			if err != nil {
				t.Fatalf("Classify(%q) error %v (%s)", tc.cmd, err, tc.why)
			}
			if v.Class != tc.class {
				t.Errorf("Classify(%q) class %v, want %v: %s (reason %q)", tc.cmd, v.Class, tc.class, tc.why, v.Reason)
			}
			if v.Redact != tc.redact {
				t.Errorf("Classify(%q) redact %v, want %v: %s", tc.cmd, v.Redact, tc.redact, tc.why)
			}
			if (v.Confirm != "") != tc.confirm {
				t.Errorf("Classify(%q) confirm %q, want confirm=%v: %s", tc.cmd, v.Confirm, tc.confirm, tc.why)
			}
			if v.Name == "" {
				t.Errorf("Classify(%q) has no name", tc.cmd)
			}
		})
	}
}

// TestClassifyRejectsInjection: one command per request. Anything that could
// make the device run a second command, or that hides what is being sent,
// is malformed and fails closed (ErrInvalid + Forbidden verdict).
func TestClassifyRejectsInjection(t *testing.T) {
	bad := map[string]string{
		"semicolon":                     "Power ON;Reset 1",
		"semicolon with blanks":         "Power ON ; Reset 1",
		"percent-encoded":               "Power ON%3BReset 1",
		"percent-encoded lower":         "Power ON%3breset 1",
		"percent in name":               "Power%3BReset",
		"newline":                       "Power ON\nReset 1",
		"carriage return":               "Power ON\rReset 1",
		"tab":                           "Power\tON",
		"NUL":                           "Power ON\x00",
		"DEL":                           "Power ON\x7f",
		"greek question mark":           "Power ON\u037eReset 1",
		"fullwidth semicolon":           "Power ON\uff1bReset 1",
		"small semicolon":               "Power ON\ufe54Reset 1",
		"arabic semicolon":              "Power ON\u061bReset 1",
		"reverse semicolon":             "Power ON\u204fReset 1",
		"line separator":                "Power ON\u2028Reset 1",
		"no-break space":                "Power\u00a0ON",
		"right-to-left override":        "Power \u202eNO",
		"zero width space":              "Power\u200b ON",
		"empty":                         "",
		"blank":                         "   ",
		"leading digit":                 "1Power ON",
		"slash in name":                 "cmnd/Power",
		"dash in name":                  "Power-1 ON",
		"only digits":                   "123",
		"too long":                      "FriendlyName1 " + strings.Repeat("a", 300),
		"invalid utf-8":                 "FriendlyName1 \xff\xfe",
		"backlog encoded":               "Backlog Power ON%3BReset 1",
		"backlog lookalike":             "Backlog Power ON\u037eReset 1",
		"backlog fullwidth":             "Backlog Power ON\uff1bReset 1",
		"backlog newline":               "Backlog Power ON;\nReset 1",
		"backlog invisible part":        "Backlog Power ON;\u200bReset 1",
		"backlog bad part name":         "Backlog Power ON; 9Reset",
		"semicolon in a lookalike name": "Backlogx Power ON;Reset 1",
	}
	for name, cmd := range bad {
		t.Run(name, func(t *testing.T) {
			v, err := Classify(cmd)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Classify(%q) error %v, want ErrInvalid", cmd, err)
			}
			if v.Class != ClassForbidden {
				t.Errorf("a malformed command must come back Forbidden, got %v", v.Class)
			}
			if v.Redact != true {
				t.Errorf("a malformed command may hide a secret and must be redacted in logs")
			}
		})
	}
}

// TestMalformedNeverLeaks: the reason text is shown in the UI and the audit
// log, so it must never echo the rejected input (it may contain a password).
func TestMalformedNeverLeaks(t *testing.T) {
	_, err := Classify("MqttPassword hunter2;Reset 1")
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error %v must not repeat the input", err)
	}
}

func TestAuthorize(t *testing.T) {
	tests := []struct {
		cmd  string
		role model.Role
		want error
		why  string
	}{
		{"Power1 TOGGLE", model.RoleOperator, nil, "operators switch relays"},
		{"Status 2", model.RoleOperator, nil, "operators read"},
		{"SetOption19 1", model.RoleOperator, ErrRole, "operators must not write settings"},
		{"SetOption19 1", model.RoleAdmin, nil, "admins may (with a confirmation in the UI)"},
		{"Reset 1", model.RoleAdmin, nil, "admins may send Reset (the confirmation is the UI's job)"},
		{"Reset 1", model.RoleOperator, ErrRole, "operators never reset"},
		{"Backlog Power ON", model.RoleAdmin, nil, "Backlog is allowed for admins"},
		{"Backlog Power ON", model.RoleOperator, ErrRole, "operators cannot chain commands"},
		{"FooBar", model.RoleAdmin, nil, "no allowlist for admins"},
		{"FooBar", model.RoleOperator, ErrRole, "operators stay restricted: unknown means admin"},
		{"Status 0", model.RoleAdmin, nil, "manual Status 0 is allowed with a warning"},
		{"Status 0", model.RoleOperator, ErrRole, "operators never"},
		{"MqttPassword x", model.RoleOperator, ErrRole, "operators never"},
		{"Power ON", model.RoleNone, ErrRole, "no role, no command"},
		{"Power ON;Reset 1", model.RoleAdmin, ErrInvalid, "malformed is refused for everybody"},
		{"Power ON", model.Role(99), ErrRole, "an unknown role gets nothing it was not granted"},
	}
	for _, tc := range tests {
		_, err := Authorize(tc.cmd, tc.role)
		if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("Authorize(%q, %v) = %v, want %v: %s", tc.cmd, tc.role, err, tc.want, tc.why)
		}
	}
}

func TestClassMinRole(t *testing.T) {
	for c, want := range map[Class]model.Role{ClassRead: model.RoleOperator, ClassOperator: model.RoleOperator, ClassAdmin: model.RoleAdmin} {
		if got, ok := c.MinRole(); !ok || got != want {
			t.Errorf("%v.MinRole() = %v, %v; want %v", c, got, ok, want)
		}
	}
	for _, c := range []Class{ClassForbidden, Class(0), Class(42)} {
		if _, ok := c.MinRole(); ok {
			t.Errorf("%v must have no role that may send it", c)
		}
	}
}

// TestRedactedForLog: the text that reaches the audit log and the console
// history for a secret-bearing command has the value removed.
func TestRedactedForLog(t *testing.T) {
	tests := []struct{ cmd, want string }{
		{"MqttPassword hunter2", "MqttPassword ***"},
		{"WebPassword  hunter2 x", "WebPassword ***"},
		{"ssid1 My Net", "ssid1 ***"},
		{"MqttPassword", "MqttPassword"},
		{"Power1 TOGGLE", "Power1 TOGGLE"},
		{"Status 2", "Status 2"},
		{"Power ON;MqttPassword hunter2", RejectedText},
		{"MqttHost alice:s3cret@broker", "MqttHost ***"},
		{"NtpServer1 a.example", "NtpServer1 ***"},
		{"Backlog WebPassword s3cret", "Backlog WebPassword ***"},
		{"Backlog0 MqttPassword s3cret; Power1 ON", "Backlog0 MqttPassword ***; Power1 ON"},
		{"Backlog Rule1 ON x DO Password1 s3cret ENDON", "Backlog Rule1 ***"},
		{"Backlog Power ON;MqttPassword s3cret", "Backlog Power ON; MqttPassword ***"},
		{"Backlog Power ON; Reset 1", "Backlog Power ON; Reset ***"},
		{"Backlog Power ON; Pass\u200bword x", RejectedText},
		{"Password1 s3cret", "Password1 ***"},
		{`Json {"WebPassword":"s3cret"}`, "Json ***"},
		{"Rule1 ON System#Boot DO MqttPassword s3cret ENDON", "Rule1 ***"},
		{"Publish cmnd/plug/MqttPassword s3cret", "Publish ***"},
		{"WebSend [10.0.0.5,admin:s3cret] Power1 ON", "WebSend ***"},
		{"TmToken 123:abc", "TmToken ***"},
		{"FooBar s3cret", "FooBar ***"},
		{"Reset s3cret", "Reset ***"},
		{"SetOption19 1", "SetOption19 1"},
		{"SO19 1", "SO19 1"},
		{"TelePeriod 60", "TelePeriod 60"},
		{"SaveData 0", "SaveData 0"},
		{"Power1 TOGGLE", "Power1 TOGGLE"},
		{"", RejectedText},
	}
	for _, tc := range tests {
		if got := Redacted(tc.cmd); got != tc.want {
			t.Errorf("Redacted(%q) = %q, want %q", tc.cmd, got, tc.want)
		}
	}
}

// TestPublishedListsMatchClassify keeps DefaultOperator honest: it is what the
// UI shows as the operator set, so it must be what Classify really does.
func TestPublishedListsMatchClassify(t *testing.T) {
	if len(DefaultOperator) == 0 {
		t.Fatal("the published list must not be empty")
	}
	for _, c := range DefaultOperator {
		v, err := Classify(c)
		if err != nil || (v.Class != ClassRead && v.Class != ClassOperator) {
			t.Errorf("DefaultOperator entry %q is %v (%v), not operator-level", c, v.Class, err)
		}
	}
}

// fixtureCommands maps every captured fixture file to the command it answers.
// A new fixture without a row fails the test: somebody must decide what class
// the command has.
var fixtureCommands = map[string]struct {
	cmd   string
	class Class
}{
	"status-0":       {"Status 0", ClassAdmin},
	"status-1":       {"Status 1", ClassRead},
	"status-2":       {"Status 2", ClassRead},
	"status-3":       {"Status 3", ClassRead},
	"status-4":       {"Status 4", ClassRead},
	"status-5":       {"Status 5", ClassRead},
	"status-6":       {"Status 6", ClassRead},
	"status-7":       {"Status 7", ClassRead},
	"status-8":       {"Status 8", ClassRead},
	"status-9":       {"Status 9", ClassRead},
	"status-10":      {"Status 10", ClassRead},
	"status-11":      {"Status 11", ClassRead},
	"energy-status":  {"Status 10", ClassRead},
	"upgrade":        {"Upgrade", ClassAdmin},
	"otaurl":         {"OtaUrl", ClassAdmin},
	"state":          {"State", ClassRead},
	"module":         {"Module", ClassRead},
	"modules":        {"Modules", ClassRead},
	"template":       {"Template", ClassRead},
	"hostname":       {"Hostname", ClassRead},
	"topic":          {"Topic", ClassRead},
	"power":          {"Power", ClassRead},
	"teleperiod":     {"TelePeriod", ClassRead},
	"setoption-0":    {"SetOption0", ClassRead},
	"setoption-65":   {"SetOption65", ClassRead},
	"seriallog":      {"SerialLog", ClassRead},
	"webserver":      {"Webserver", ClassRead},
	"fallbacktopic":  {"FallbackTopic", ClassRead},
	"loglevel":       {"WebLog", ClassRead},
	"gpio":           {"Gpio", ClassRead},
	"gpios":          {"Gpios", ClassRead},
	"unknown-foobar": {"FooBar", ClassAdmin},
	"backlog-empty":  {"Backlog", ClassAdmin},
}

func TestEveryFixtureCommandIsClassified(t *testing.T) {
	files, err := filepath.Glob("../../testdata/fixtures/devices/*/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures found (%v)", err)
	}
	seen := map[string]bool{}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		if name == "empty-cmnd" {
			// `cmnd=` with nothing: the console refuses an empty command.
			if _, err := Classify(""); !errors.Is(err, ErrInvalid) {
				t.Errorf("empty command must be invalid, got %v", err)
			}
			continue
		}
		row, ok := fixtureCommands[name]
		if !ok {
			t.Errorf("fixture %s has no classification row: add one", name)
			continue
		}
		seen[name] = true
		v, err := Classify(row.cmd)
		if err != nil || v.Class != row.class {
			t.Errorf("fixture %s: Classify(%q) = %v, %v; want %v", name, row.cmd, v.Class, err, row.class)
		}
	}
	for name := range fixtureCommands {
		if !seen[name] {
			if _, err := os.Stat(filepath.Join("../../testdata/fixtures/devices/bedroom-cabinet", name+".json")); err != nil {
				t.Errorf("row %q has no fixture any more: remove it", name)
			}
		}
	}
}

// TestBoundaries pins the edges of every limit, so loosening one by a
// character (and so widening what reaches a device) is caught.
func TestBoundaries(t *testing.T) {
	cls := func(cmd string) (Class, error) {
		v, err := Classify(cmd)
		return v.Class, err
	}
	pad := func(n int) string { return "FriendlyName1 " + strings.Repeat("a", n-len("FriendlyName1 ")) }
	if _, err := cls(pad(255)); err != nil {
		t.Errorf("255 bytes must pass: %v", err)
	}
	if _, err := cls(pad(256)); !errors.Is(err, ErrInvalid) {
		t.Errorf("256 bytes must fail: %v", err)
	}
	if _, err := cls(strings.Repeat("a", 40)); err != nil {
		t.Errorf("40 letter name must pass: %v", err)
	}
	if _, err := cls(strings.Repeat("a", 41)); !errors.Is(err, ErrInvalid) {
		t.Errorf("41 letter name must fail: %v", err)
	}
	for _, ok := range []string{"a", "z", "A", "Z", "Za09", "aZ9"} {
		if _, err := cls(ok); err != nil {
			t.Errorf("name %q must be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"`", "{", "@", "[", "a/", "a:", "a@", "a`", "a{", "a[", "a/9"} {
		if _, err := cls(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("name %q must be invalid, got %v", bad, err)
		}
	}
	if c, _ := cls("Power8 ON"); c != ClassOperator {
		t.Errorf("Power8 ON is operator, got %v", c)
	}
	if c, _ := cls("Power9 ON"); c != ClassAdmin {
		t.Errorf("Power9 ON is admin, got %v", c)
	}
	for cmd, want := range map[string]Class{
		"Status 11": ClassRead, "Status 12": ClassAdmin, "Status 999": ClassAdmin,
		"Status 9": ClassRead, "Status 10": ClassRead, "Status 19": ClassAdmin, "Status 5": ClassRead,
	} {
		if c, err := cls(cmd); err != nil || c != want {
			t.Errorf("%q = %v, %v; want %v", cmd, c, err, want)
		}
	}
	// Everything that is not a plain Status 1..999 asks for the Status 0 warning.
	for _, cmd := range []string{"Status 1000", "Status 1a", "Status 1/", "Status 1:", "Status 10:", "Status 2/", "Status 0", "Status 09"} {
		if v, _ := Classify(cmd); v.Confirm == "" {
			t.Errorf("%q must carry the Status 0 warning", cmd)
		}
	}
	for _, cmd := range []string{"Status 1", "Status 11", "Status 12", "Status 999"} {
		if v, _ := Classify(cmd); v.Confirm != "" {
			t.Errorf("%q is a plain query and needs no warning", cmd)
		}
	}
	// Indexed names with a huge number must not overflow into a valid index.
	if _, err := cls("Power99999999999999999999 ON"); err == nil {
		t.Error("an index that overflows int must be invalid")
	}
}

// TestAuthorizeMessages: the user is told which role is missing.
func TestAuthorizeMessages(t *testing.T) {
	_, err := Authorize("SetOption19 1", model.RoleOperator)
	if err == nil || !strings.Contains(err.Error(), "admin role") {
		t.Errorf("want an admin hint, got %v", err)
	}
	_, err = Authorize("Power ON", model.RoleNone)
	if err == nil || !strings.Contains(err.Error(), "operator role") {
		t.Errorf("want an operator hint, got %v", err)
	}
}

// TestCanonicalIsWhatIsJudged: the text that is classified is the text that
// is sent. For every command the table knows, Canonical keeps the verdict,
// is stable, and has no surrounding or doubled name/argument blanks.
func TestCanonicalIsWhatIsJudged(t *testing.T) {
	cmds := []string{
		"Status 2", "  Status   2  ", "Power1   TOGGLE", "power2 off", "Restart 1 ", "Reset 1",
		"Backlog Power ON; Reset 1", "TelePeriod 60", "MqttPassword   hunter2", "FriendlyName1 a  b", "Status 0", "FooBar",
	}
	for _, c := range cmds {
		want, wantErr := Classify(c)
		canon, err := Canonical(c)
		if err != nil {
			t.Fatalf("Canonical(%q): %v", c, err)
		}
		got, err := Classify(canon)
		if (err == nil) != (wantErr == nil) || got != want {
			t.Errorf("verdict changed by canonicalising %q -> %q: %+v vs %+v (%v)", c, canon, got, want, err)
		}
		if again, _ := Canonical(canon); again != canon {
			t.Errorf("Canonical not stable: %q -> %q", canon, again)
		}
		if canon != strings.TrimSpace(canon) || strings.Contains(canon, "  ") && !strings.Contains(c, "a  b") {
			t.Errorf("canonical form %q has stray blanks", canon)
		}
	}
	if got, _ := Canonical("  Power1   TOGGLE "); got != "Power1 TOGGLE" {
		t.Errorf("got %q", got)
	}
	if got, _ := Canonical("FriendlyName1 a  b"); got != "FriendlyName1 a  b" {
		t.Errorf("inner blanks of a value are data: %q", got)
	}
	if _, err := Canonical("Power ON;Reset 1"); !errors.Is(err, ErrInvalid) {
		t.Errorf("malformed must fail: %v", err)
	}
}

// Why: FileDownload, FileUpload and Upload carry the device's MQTT password and drive a transfer
// that tasmofleet's own code runs; a person typing them in the console or a bulk command would
// either leak the password into a reply or corrupt a transfer. No role may send them, and the
// arguments (the password) never reach a log.
func TestFileTransferCommandsAreNeverSent(t *testing.T) {
	for _, cmd := range []string{`FileDownload {"Password":"hunter2","Type":2}`, "filedownload 0", `FileUpload {"Password":"hunter2"}`, "FILEUPLOAD", "Upload 1", "Upload"} {
		v, err := Classify(cmd)
		if err != nil || v.Class != ClassForbidden || !v.Redact {
			t.Errorf("Classify(%q) = %+v, %v", cmd, v, err)
		}
		for _, role := range []model.Role{model.RoleOperator, model.RoleAdmin} {
			if _, err := Authorize(cmd, role); !errors.Is(err, ErrForbidden) {
				t.Errorf("Authorize(%q, %v) = %v, want ErrForbidden", cmd, role, err)
			}
		}
		if got := Redacted(cmd); strings.Contains(got, "hunter2") {
			t.Errorf("Redacted(%q) = %q", cmd, got)
		}
	}
	if _, err := Authorize("Backlog Power ON; FileDownload 0", model.RoleAdmin); err == nil {
		// a Backlog is judged part by part, but the parts keep their own class: the Backlog is only
		// as harmless as its worst part; a file command in it must not slip through
		t.Error("a file command inside a Backlog was accepted")
	}
}

// Why: a device that only answers over MQTT cannot be reached again if its topic, host, login or
// client name changes, or MQTT is switched off; over HTTP the same change is recoverable. The
// console asks once and says so. Queries and unrelated commands stay as they were.
func TestMQTTOnlyNamesTheCutOffConsequence(t *testing.T) {
	cut := []string{"Topic other", "FullTopic %prefix%/x/%topic%/", "GroupTopic g", "MqttHost 10.0.0.1", "MqttPort 8883",
		"MqttUser u", "MqttPassword p", "MqttClient c", "SetOption3 0", "SO3 off", "Backlog Power ON; Topic other",
		"Prefix1 x", "Prefix3 x", "MqttFingerprint aa", "TLSKey1 abc", "SetOption103 1", "SetOption132 1"}
	for _, cmd := range cut {
		_, v, err := Prepare(cmd, model.RoleAdmin, PrepareOpts{MQTTOnly: true})
		if err != nil || !strings.Contains(v.Confirm, "reached over MQTT only") {
			t.Errorf("%q: confirm = %q, %v", cmd, v.Confirm, err)
		}
		_, v, _ = Prepare(cmd, model.RoleAdmin, PrepareOpts{})
		if strings.Contains(v.Confirm, "reached over MQTT only") {
			t.Errorf("%q: a device reached over HTTP must not get the MQTT-only text: %q", cmd, v.Confirm)
		}
	}
	for _, cmd := range []string{"Topic", "FullTopic", "MqttHost", "Prefix1", "SetOption103", "SetOption3 1", "SetOption3", "SetOption4 0", "TelePeriod 60", "Power1 TOGGLE", "Status 2"} {
		if _, v, _ := Prepare(cmd, model.RoleAdmin, PrepareOpts{MQTTOnly: true}); strings.Contains(v.Confirm, "reached over MQTT only") {
			t.Errorf("%q does not cut the device off, got %q", cmd, v.Confirm)
		}
	}
	// the secret and the cut-off consequence both stay
	if _, v, _ := Prepare("MqttPassword p", model.RoleAdmin, PrepareOpts{MQTTOnly: true}); !strings.Contains(v.Confirm, "lock tasmofleet out") || !v.Redact {
		t.Errorf("the credential consequence was lost: %+v", v)
	}
}

func TestPrepare(t *testing.T) {
	send, v, err := Prepare("  Power1   TOGGLE ", model.RoleOperator, PrepareOpts{})
	if err != nil || send != "Power1 TOGGLE" || v.Class != ClassOperator {
		t.Fatalf("%q %+v %v", send, v, err)
	}
	for _, tc := range []struct {
		cmd  string
		role model.Role
		want error
	}{
		{"SetOption19 1", model.RoleOperator, ErrRole},
		{"Reset 1", model.RoleOperator, ErrRole},
		{"Power ON;Reset 1", model.RoleAdmin, ErrInvalid},
	} {
		send, _, err := Prepare(tc.cmd, tc.role, PrepareOpts{})
		if !errors.Is(err, tc.want) || send != "" {
			t.Errorf("Prepare(%q) = %q, %v", tc.cmd, send, err)
		}
		if err != nil && strings.Contains(err.Error(), tc.cmd) {
			t.Errorf("error echoes the command: %v", err)
		}
	}
}

// TestDocListsEveryConfirmedName keeps docs/security/command-policy.md in step
// with the code: a command that asks for a confirmation, or carries a
// credential, must be named in the claims table, so a reviewer judging against
// the document sees it.
func TestDocListsEveryConfirmedName(t *testing.T) {
	raw, err := os.ReadFile("../../docs/security/command-policy.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.ToLower(string(raw))
	names := []string{"reset", "wificonfig", "upgrade", "upload", "otaurl", "wifi", "webserver",
		"ipaddress", "ethernet", "ap", "module", "template", "restart", "status"}
	for n := range secretNames {
		names = append(names, n)
	}
	for _, n := range names {
		if !strings.Contains(doc, "`"+n) {
			t.Errorf("name %q is not named in docs/security/command-policy.md (write it as `%s`)", n, n)
		}
	}
}

// Why: "admins may do everything" means a name nobody listed is not refused. A
// random name must be sendable by an admin, and an operator must still be
// stopped, so the operator set is the only thing that stays a list.
func TestAnyNameIsAdminOnly(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	const letters = "abcdefghijklmnopqrstuvwxyz"
	for range 500 {
		n := make([]byte, 3+r.IntN(12))
		for i := range n {
			n[i] = letters[r.IntN(len(letters))]
		}
		name := string(n)
		if name == "backlog" {
			continue
		}
		if _, err := Authorize(name+" 1", model.RoleAdmin); err != nil {
			t.Fatalf("admin may not send %q: %v", name, err)
		}
		if _, err := Authorize(name+" 1", model.RoleOperator); !errors.Is(err, ErrRole) {
			t.Fatalf("operator must be refused %q with ErrRole, got %v", name, err)
		}
	}
}

// Why: a destructive command must not slip past the confirmation by being
// wrapped. Every confirm row of TestClassify, put inside a Backlog, asks too.
func TestBacklogInheritsConfirmations(t *testing.T) {
	for _, cmd := range []string{"Reset 1", "WifiConfig 2", "Upgrade 1", "Wifi 0", "WebServer 0", "IPAddress1 1.2.3.4", "Module 18", "Restart 2", "Status 0", "Password1 x"} {
		v, err := Classify("Backlog Power ON; " + cmd)
		if err != nil || v.Confirm == "" {
			t.Errorf("Backlog with %q: %+v %v", cmd, v, err)
		}
	}
	if v, _ := Classify("Backlog Power ON; Power2 OFF; TelePeriod 60"); v.Confirm != "" {
		t.Errorf("a routine Backlog must not ask: %q", v.Confirm)
	}
}

// Why: switching devices on and off is the main thing operators do in the UI.
// Every relay 1 to 8 and every spelling of on, off and toggle must stay
// operator-level (lights use Power too), whatever else the policy tightens.
func TestOperatorMaySwitchEveryRelay(t *testing.T) {
	for n := 1; n <= 8; n++ {
		for _, act := range []string{"ON", "OFF", "TOGGLE", "on", "toggle", "0", "1", "2"} {
			for _, cmd := range []string{"Power" + strconv.Itoa(n) + " " + act, "power" + strconv.Itoa(n) + "  " + act} {
				if _, err := Authorize(cmd, model.RoleOperator); err != nil {
					t.Errorf("operator may not send %q: %v", cmd, err)
				}
			}
		}
	}
	for _, act := range []string{"ON", "OFF", "TOGGLE", "0", "1", "2"} {
		if _, err := Authorize("Power "+act, model.RoleOperator); err != nil {
			t.Errorf("operator may not send Power %s: %v", act, err)
		}
	}
	for _, cmd := range []string{"Power", "Power1", "Power8"} {
		if _, err := Authorize(cmd, model.RoleOperator); err != nil {
			t.Errorf("operator may not query %q: %v", cmd, err)
		}
	}
}

// Why: the router refuses a write that can cut an MQTT-only device off unless a person confirmed it;
// it asks CutOffName, so the name it reports and the Backlog handling must be exact.
func TestCutOffName(t *testing.T) {
	for cmd, want := range map[string]string{"Topic x": "Topic", "Backlog Power ON; MqttHost h": "MqttHost", "Backlog backlogTopic x": "Topic",
		"SetOption3 0": "SetOption3", "Prefix2 x": "Prefix2", "Power1 ON": "", "Topic": "", "Backlog Power ON": "", "Status 2": "", "\x00": ""} {
		if got := CutOffName(cmd); got != want {
			t.Errorf("CutOffName(%q) = %q, want %q", cmd, got, want)
		}
	}
}

// Why: the update-job lock must hit exactly the commands that start or redirect a firmware update
// (two updates at once can brick a device), including inside a Backlog, and nothing that merely reads
// them: a lock that is too wide blocks routine work, one that is too narrow lets a second update in.
func TestStartsFirmware(t *testing.T) {
	for cmd, want := range map[string]bool{
		"Upgrade 1":                     true,
		"upgrade 1":                     true,
		"OtaUrl http://x/y.bin":         true,
		"Backlog Power1 ON; Upgrade 1":  true,
		"Backlog OtaUrl http://x/y.bin": true,
		"Upgrade":                       false, // a query
		"OtaUrl":                        false, // a query
		"Status 2":                      false,
		"Restart 1":                     false,
		"Upload http://x/y.bin":         false, // never typed at all (ClassForbidden)
		"Backlog Power1 ON; Status 2":   false,
		"Upgrade 1;Reset 1":             false, // malformed: refused by the policy itself
		"":                              false,
	} {
		if got := StartsFirmware(cmd); got != want {
			t.Errorf("StartsFirmware(%q) = %v, want %v", cmd, got, want)
		}
	}
}

// Why: a typed Upgrade makes the device flash and restart, so a TasmoFleet update must wait for that restart;
// a bare OtaUrl only stores an address and no restart follows. The answer has to be the one cmdpolicy's own
// parser gives (case, index suffix, Backlog prefixes and separators), or a spelling Tasmota accepts would
// slip past the hold, or one it ignores would hold a device for nothing.
func TestStartsFirmwareUpgrade(t *testing.T) {
	for cmd, want := range map[string]bool{
		"Upgrade 1":                    true,
		"upgrade 1":                    true,
		"UPGRADE 1":                    true,
		"Upgrade 2":                    true,
		"Upgrade1 1":                   true, // an index suffix is still the Upgrade command
		"  Upgrade   1  ":              true,
		"Backlog Power1 ON; Upgrade 1": true,
		"Backlog OtaUrl http://x/y.bin;Upgrade 1":   true,
		"Backlog0 OtaUrl http://x/y.bin; Upgrade 1": true,
		"BACKLOG Upgrade 1":                         true,
		"Backlog Backlog Upgrade 1":                 true,
		"Backlog Power1 ON;BacklogUpgrade 1":        true,
		"Backlog Status 2;;Upgrade 1":               true,  // an empty part is skipped
		"OtaUrl http://x/y.bin":                     false, // no restart follows
		"Backlog OtaUrl http://x/y.bin":             false,
		"Backlog Power1 ON; OtaUrl http://x/y.bin":  false,
		"Upgrade":                     false, // a query
		"Backlog Upgrade":             false,
		"Status 2":                    false,
		"Backlog Power1 ON; Status 2": false,
		"Upload http://x/y.bin":       false,
		"Upgrade 1;Reset 1":           false, // malformed
		"":                            false,
	} {
		if got := StartsFirmwareUpgrade(cmd); got != want {
			t.Errorf("StartsFirmwareUpgrade(%q) = %v, want %v", cmd, got, want)
		}
		if want && !StartsFirmware(cmd) {
			t.Errorf("%q: an Upgrade start is always a firmware start", cmd)
		}
	}
}

// Why (#393): the glued "Backlog" prefix is only stripped by Tasmota inside a Backlog part
// (CmndBacklog, tasmota/tasmota_support/support_command.ino lines 549-556, Tasmota 16.0.0). A typed
// "BacklogUpgrade 1" never gets there: ExecuteCommand (lines 339-368) takes the whole alphanumeric run
// "BacklogUpgrade" as the command word, CommandHandler upper-cases it and strips trailing digits
// (lines 389-428), and DecodeCommand/GetCommandCode (tasmota_support/support.ino lines 1133-1163)
// match whole names with strcasecmp. "BACKLOGUPGRADE" is in no command table, so the device answers
// "Unknown" and flashes nothing. The same holds for MQTT cmnd/<topic>/BacklogUpgrade, which reaches
// the same CommandHandler. So cmdpolicy must not treat it as a firmware start (it would hold an
// update job and show the firmware confirmation for a command that does nothing), and it is not an
// Upgrade in any other spelling either. Inside a Backlog part the prefix is stripped, and that stays
// a firmware start (TestStartsFirmwareUpgrade).
func TestGluedBacklogPrefixOutsideBacklogIsUnknown(t *testing.T) {
	for _, cmd := range []string{
		"BacklogUpgrade 1", "backlogupgrade 1", "BacklogUpgrade1", "Backlog0Upgrade 1",
		"BacklogOtaUrl http://x/y.bin", "BacklogBacklogUpgrade 1",
	} {
		if StartsFirmware(cmd) || StartsFirmwareUpgrade(cmd) {
			t.Errorf("%q is an unknown command on Tasmota 16.0.0, not a firmware start", cmd)
		}
		v, err := Classify(cmd)
		if err != nil {
			t.Fatalf("Classify(%q): %v", cmd, err)
		}
		if v.Class != ClassAdmin {
			t.Errorf("Classify(%q) class %v, want admin (unknown names are admin-only)", cmd, v.Class)
		}
		if v.Confirm != "" && strings.Contains(strings.ToLower(v.Confirm), "firmware") {
			t.Errorf("Classify(%q) asks the firmware confirmation for a command Tasmota does not run: %q", cmd, v.Confirm)
		}
	}
}

// Why (#324): the filesystem commands change what the device keeps in flash (UfsDelete removes a
// file, UfsRename moves one, UfsRun executes a script). They must be device writes that only an
// admin sends, alone or hidden in a Backlog; an operator who could send them could delete the
// files a restore puts back. The file transfer itself is TasmoFleet's own code, not these commands.
func TestFilesystemCommandsAreAdminDeviceWrites(t *testing.T) {
	for _, cmd := range []string{"UfsDelete /autoexec.be", "UfsRename /a.be,/b.be", "UfsRun /x.bat", "UfsDelete", "ufsdelete /tlskey", "Backlog UfsDelete /autoexec.be", "Backlog Power ON; UfsRename /a,/b"} {
		v, err := Classify(cmd)
		if err != nil || v.Class != ClassAdmin {
			t.Errorf("Classify(%q) = %v, %v; want admin", cmd, v.Class, err)
		}
		if _, err := Authorize(cmd, model.RoleOperator); !errors.Is(err, ErrRole) {
			t.Errorf("an operator may send %q: %v", cmd, err)
		}
		if _, err := Authorize(cmd, model.RoleAdmin); err != nil {
			t.Errorf("an admin may not send %q: %v", cmd, err)
		}
	}
}

// TestUnsafeText is the one check every caller that builds a command from text it did not type
// itself (the GPIO template planner, #327) uses: the characters parse refuses in a command line are
// refused there, too, so the two cannot drift apart.
func TestUnsafeText(t *testing.T) {
	bad := map[string]string{
		"semicolon":          "a;b",
		"fullwidth ;":        "a\uff1bb",
		"greek question ;":   "a\u037eb",
		"arabic ;":           "a\u061bb",
		"newline":            "a\nb",
		"carriage return":    "a\rb",
		"tab":                "a\tb",
		"NUL":                "a\x00b",
		"DEL":                "a\x7fb",
		"bidi override":      "a\u202eb",
		"zero width":         "a\u200bb",
		"no-break space":     "a\u00a0b",
		"encoded semicolon":  "a%3bb",
		"encoded upper":      "a%3Bb",
		"invalid UTF-8":      "a\xffb",
		"line separator":     "a\u2028b",
		"private use":        "a\ue000b",
		"unassigned control": "a\u0085b",
	}
	for name, s := range bad {
		if UnsafeText(s) == "" {
			t.Errorf("%s: %q accepted", name, s)
		}
	}
	for _, s := range []string{"", "NOUS A1T", "Shelly 2.5", "Plüg 1", "a-b_c/d(e)", "日本", "50% off"} {
		if why := UnsafeText(s); why != "" {
			t.Errorf("%q refused: %s", s, why)
		}
	}
}

// TestUnsafeTextIsWhatClassifyRefuses ties the helper to parse: whatever it flags, a command line made of
// it is malformed, and whatever it passes is not refused for its characters.
func TestUnsafeTextIsWhatClassifyRefuses(t *testing.T) {
	for _, s := range []string{"a;b", "a\uff1bb", "a\nb", "a\u202eb", "a%3bb", "a\x00b"} {
		if _, err := Classify("Topic " + s); !errors.Is(err, ErrInvalid) {
			t.Errorf("Classify accepts %q that UnsafeText flags", s)
		}
	}
	for _, s := range []string{"NOUS A1T", "Plüg 1"} {
		if _, err := Classify("Topic " + s); err != nil {
			t.Errorf("Classify refuses %q: %v", s, err)
		}
	}
}
