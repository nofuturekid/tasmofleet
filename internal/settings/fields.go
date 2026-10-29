// Package settings edits a curated set of Tasmota settings (spec §13).
//
// Every changed field becomes exactly one device command with a validated
// value: values are never concatenated into a Backlog string, and ';' or
// control characters are rejected (TasmoAdmin lesson B3). Secret fields are
// write-only: they are never read back, never shown and never audited.
package settings

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
)

// Kind selects the form control of a field.
type Kind int

// Field kinds.
const (
	KindText Kind = iota
	KindNumber
	KindToggle // 0 or 1 (a SetOption)
	KindSelect // one of Options
	KindPassword
)

// Option is one choice of a KindSelect field.
type Option struct{ Value, Label string }

// Field is one editable setting. Key is the Tasmota command name and the
// form field name.
type Field struct {
	Key, Label, Help string
	Kind             Kind
	Secret           bool
	Validate         func(string) error
	Command          func(v string) string
	Options          []Option // KindSelect only
	// Min and Max bound a KindNumber field when HasRange is set; the number
	// input of a form uses them as its min and max.
	Min, Max int
	HasRange bool
	// Group orders the form sections.
	Group string
	// PolicyOnly marks a setting that only the fleet policy reads and sets
	// ("Make compliant"): the device form neither reads nor writes it.
	PolicyOnly bool
}

// MaxValueLen caps every value, whatever its validator says.
const MaxValueLen = 128

// ErrInvalid is wrapped by every validation failure.
var ErrInvalid = errors.New("settings: invalid value")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// checkText rejects what must never reach a device command line: ';' (the
// Backlog separator, also its look-alikes), control and format characters,
// invalid UTF-8, empty and over-long values, leading or trailing blanks.
func checkText(v string, maxLen int) error {
	switch {
	case v == "":
		return invalid("must not be empty")
	case !utf8.ValidString(v):
		return invalid("must be valid UTF-8")
	case len(v) > maxLen || len(v) > MaxValueLen:
		return invalid("at most %d bytes", min(maxLen, MaxValueLen))
	case strings.TrimSpace(v) != v:
		return invalid("must not start or end with a blank")
	}
	for _, r := range v {
		switch {
		case r == ';' || r == '；' || r == ';':
			return invalid("';' is not allowed")
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ':
			return invalid("control characters are not allowed")
		}
	}
	return nil
}

// noShortcut rejects the values Tasmota treats as commands to itself: 0
// clears and 1 resets to the default for the text settings, and a leading
// '"' makes a FriendlyName empty. The form shows a value, so a value that
// does something else would leave the device in a state the UI does not show.
func noShortcut(v string) error {
	if v == "0" || v == "1" || strings.HasPrefix(v, `"`) {
		return invalid(`0, 1 and a leading '"' have a special meaning on the device`)
	}
	return nil
}

func both(vs ...func(string) error) func(string) error {
	return func(v string) error {
		for _, f := range vs {
			if err := f(v); err != nil {
				return err
			}
		}
		return nil
	}
}

var friendlyName = both(func(v string) error { return checkText(v, 32) }, noShortcut)

var (
	hostnameRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,30}[A-Za-z0-9])?$`)
	topicRe    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)
	fullTopRe  = regexp.MustCompile(`^[A-Za-z0-9_./%-]{1,100}$`)
	hostRe     = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,98}$`)
	tzRe       = regexp.MustCompile(`^(99|[+-]?(1[0-4]|[0-9])|[+-](0[0-9]|1[0-4]):[0-5][0-9])$`)
)

func hostnameV(v string) error {
	if err := noShortcut(v); err != nil {
		return err
	}
	if !hostnameRe.MatchString(v) {
		return invalid("1-32 letters, digits or '-', not starting or ending with '-'")
	}
	return nil
}

func topicV(v string) error {
	if err := noShortcut(v); err != nil {
		return err
	}
	if !topicRe.MatchString(v) {
		return invalid("1-32 letters, digits, '.', '_' or '-'")
	}
	return nil
}

func fullTopicV(v string) error {
	if !fullTopRe.MatchString(v) {
		return invalid("letters, digits, '/', '_', '.', '-' and %%placeholders%%, at most 100 characters")
	}
	if !strings.Contains(v, "%topic%") && !strings.Contains(v, "%hostname%") && !strings.Contains(v, "%id%") {
		return invalid("must contain %%topic%%, %%hostname%% or %%id%%")
	}
	if strings.HasPrefix(v, "/") || !strings.HasSuffix(v, "/") {
		return invalid("must end with '/' and not start with it")
	}
	return nil
}

func intRange(lo, hi int) func(string) error {
	return func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || strconv.Itoa(n) != v || n < lo || n > hi {
			return invalid("a whole number from %d to %d", lo, hi)
		}
		return nil
	}
}

func telePeriod(v string) error {
	if v == "0" {
		return nil
	}
	if err := intRange(10, 3600)(v); err != nil {
		return invalid("0 (off) or 10 to 3600 seconds")
	}
	return nil
}

func oneOf(vals ...string) func(string) error {
	return func(v string) error {
		for _, w := range vals {
			if v == w {
				return nil
			}
		}
		return invalid("one of %s", strings.Join(vals, ", "))
	}
}

func hostV(v string) error {
	if err := noShortcut(v); err != nil {
		return err
	}
	if !hostRe.MatchString(v) {
		return invalid("a host name or IP address")
	}
	if strings.Contains(v, ":") { // only IPv6 may contain ':'
		if _, err := netip.ParseAddr(v); err != nil {
			return invalid("a host name or IP address")
		}
	}
	return nil
}

func credential(v string) error {
	if err := checkText(v, 64); err != nil {
		return err
	}
	if err := noShortcut(v); err != nil {
		return err
	}
	if strings.ContainsAny(v, " \t") {
		return invalid("must not contain blanks")
	}
	return nil
}

// timezoneV accepts what Tasmota parses: 99, whole hours -13..+14 or
// +HH:MM. A negative zero hour with minutes ("-00:30") loses its sign on the
// device (hours are parsed as the number 0), and -14 is below its limit.
func timezoneV(v string) error {
	msg := "99 (use TimeDst/TimeStd), -13 to 14, or +HH:MM (+00:30 style, hours -13 to 14)"
	if !tzRe.MatchString(v) {
		return invalid("%s", msg)
	}
	if v == "-14" || strings.HasPrefix(v, "-14:") || (strings.HasPrefix(v, "-00:") && v != "-00:00") {
		return invalid("%s", msg)
	}
	return nil
}

var otaUrlRe = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?(/[A-Za-z0-9._~%+/-]*)?$`)

// otaUrlV accepts a plain http(s) URL: no credentials (they would be stored,
// shown and logged), no query, no blanks; Tasmota's shortcut "1" (reset the
// URL) and "0" do not match. It decides where a device fetches firmware from,
// so the policy value is checked as strictly as a command argument.
func otaUrlV(v string) error {
	if err := checkText(v, 100); err != nil {
		return err
	}
	if !otaUrlRe.MatchString(v) {
		return invalid("an http:// or https:// URL without credentials, blanks or query (at most 100 characters)")
	}
	return nil
}

// SplitURLCredentials cleans a URL read from a device: it drops user:password, the query and the
// fragment. hadCreds reports whether userinfo was present. ok is false when the value cannot be
// cleaned safely: it does not parse, has no host, or holds an '@' that url.Parse did not read as
// userinfo or a second '@' (a password containing '#', '?' or '/' mis-parses into host, query, fragment or path).
// An empty value is ok with nothing to clean.
func SplitURLCredentials(v string) (clean string, hadCreds, ok bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false, true
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return "", false, false
	}
	// Exactly one '@' is the userinfo separator; any other (or one that is not userinfo) may be
	// part of a password that mis-parsed, e.g. "http://u:@x/@host" read as user "u:", host "x".
	if n := strings.Count(v, "@"); n > 1 || (n == 1 && u.User == nil) {
		return "", false, false
	}
	hadCreds = u.User != nil
	u.User, u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = nil, "", false, "", ""
	return u.String(), hadCreds, true
}

// RedactURLCredentials hides "user:password@" in a URL read from a device, so a URL with
// credentials is shown as a different value ("scheme://***@host/...") and never in clear text.
// A value that may hold a secret but cannot be cleaned (see SplitURLCredentials) becomes "***".
// A value without credentials is returned unchanged, query included, so policy comparison keeps
// working on it.
func RedactURLCredentials(v string) string {
	clean, hadCreds, ok := SplitURLCredentials(v)
	switch {
	case !ok && (strings.Contains(v, "@") || !isPlainNonURL(v)):
		return "***"
	case !ok, !hadCreds:
		return v
	}
	scheme, rest, _ := strings.Cut(clean, "://")
	return scheme + "://***@" + rest
}

// isPlainNonURL reports a value that is not a URL at all and holds no '@': nothing to hide.
func isPlainNonURL(v string) bool {
	u, err := url.Parse(strings.TrimSpace(v))
	return err == nil && u.Host == ""
}

// ruleV validates "Hemisphere,Week,Month,Day,Hour,Offset" of TimeDst/TimeStd.
func ruleV(v string) error {
	parts := strings.Split(v, ",")
	if len(parts) != 6 {
		return invalid("Hemisphere,Week,Month,Day,Hour,Offset (6 numbers)")
	}
	lims := [6][2]int{{0, 1}, {0, 4}, {1, 12}, {1, 7}, {0, 23}, {-900, 900}}
	for i, p := range parts {
		if err := intRange(lims[i][0], lims[i][1])(p); err != nil {
			return invalid("part %d must be a whole number from %d to %d", i+1, lims[i][0], lims[i][1])
		}
	}
	return nil
}

func plain(key string) func(string) string { return func(v string) string { return key + " " + v } }

func textField(key, label, help, group string, v func(string) error) Field {
	return Field{Key: key, Label: label, Help: help, Kind: KindText, Validate: v, Command: plain(key), Group: group}
}

func numberField(key, label, help, group string, v func(string) error) Field {
	return Field{Key: key, Label: label, Help: help, Kind: KindNumber, Validate: v, Command: plain(key), Group: group}
}

// ranged marks a number field with the bounds its validator enforces.
func ranged(f Field, lo, hi int) Field {
	f.Min, f.Max, f.HasRange = lo, hi, true
	return f
}

func toggle(n int, help string) Field {
	key := "SetOption" + strconv.Itoa(n)
	info := setOptions[n]
	return Field{
		Key: key, Label: info.label(n), Help: help, Kind: KindToggle, Group: "Options",
		Validate: oneOf("0", "1"), Command: plain(key),
	}
}

// Group names of the form sections.
const (
	GroupGeneral = "General"
	GroupOptions = "Options"
	GroupMQTT    = "MQTT"
	GroupTime    = "Time"
	// GroupFirmware holds policy-only settings; it has no section in the device form.
	GroupFirmware = "Firmware"
)

// Fields is the curated list (spec §13). The order is the order of the form.
var Fields = buildFields()

func buildFields() []Field {
	fs := []Field{
		textField("FriendlyName1", "Friendly name 1", "Shown in the Tasmota UI and Home Assistant.", GroupGeneral, friendlyName),
		textField("FriendlyName2", "Friendly name 2", "Second relay or channel, if the device has one.", GroupGeneral, friendlyName),
		textField("FriendlyName3", "Friendly name 3", "Third relay or channel.", GroupGeneral, friendlyName),
		textField("FriendlyName4", "Friendly name 4", "Fourth relay or channel.", GroupGeneral, friendlyName),
		textField("Hostname", "Hostname", "Network host name: letters, digits and '-'.", GroupGeneral, hostnameV),
		textField("Topic", "MQTT topic", "Letters, digits, '.', '_' and '-'. Changing it moves the device to other MQTT topics.", GroupMQTT, topicV),
		textField("FullTopic", "MQTT full topic", "Pattern with %topic% or %hostname%, ending in '/'.", GroupMQTT, fullTopicV),
		ranged(numberField("TelePeriod", "Telemetry period (s)", "0 turns telemetry off, otherwise 10 to 3600 seconds.", GroupGeneral, telePeriod), 0, 3600),
		{
			Key: "PowerOnState", Label: "Power on state", Kind: KindSelect, Group: GroupGeneral,
			Help: "Relay state after power loss. Fleet policy: 0, so a power cut never writes the relay state to flash.",
			Options: []Option{
				{"0", "0 (off)"}, {"1", "1 (on)"}, {"2", "2 (toggle)"}, {"3", "3 (last state, saved)"},
				{"4", "4 (on, ignore commands)"}, {"5", "5 (off, ignore commands)"},
			},
			Validate: oneOf("0", "1", "2", "3", "4", "5"), Command: plain("PowerOnState"),
		},
		ranged(numberField("SaveData", "Save data (s)", "0 saves settings only on explicit changes (fleet policy); otherwise seconds between saves (up to 3600).", GroupGeneral, intRange(0, 3600)), 0, 3600),
		toggle(0, "Fleet policy: 0 on ESP8266 to spare the flash; ESP32 devices may use 1."),
		toggle(3, "Turning MQTT off makes the device invisible to tasmofleet's MQTT health."),
		toggle(4, "Changes the shape of command replies on MQTT."),
		toggle(19, "Home Assistant discovery of the classic Tasmota integration."),
		textField("MqttHost", "MQTT host", "Host name or IP address of the broker.", GroupMQTT, hostV),
		ranged(numberField("MqttPort", "MQTT port", "1 to 65535.", GroupMQTT, intRange(1, 65535)), 1, 65535),
		textField("MqttUser", "MQTT user", "No blanks, at most 64 characters.", GroupMQTT, credential),
		{
			Key: "MqttPassword", Label: "MQTT password", Kind: KindPassword, Secret: true, Group: GroupMQTT,
			Help:     "Write-only. Leave empty to keep the stored password.",
			Validate: credential,
			// Index 2 makes Tasmota answer with asterisks. Index 1 echoes the
			// password in clear text in the reply, which is published on
			// stat/<topic>/RESULT and written to the device's web log.
			Command: plain("MqttPassword2"),
		},
		textField("NtpServer1", "NTP server 1", "Host name or IP address.", GroupTime, hostV),
		textField("NtpServer2", "NTP server 2", "Host name or IP address.", GroupTime, hostV),
		textField("NtpServer3", "NTP server 3", "Host name or IP address.", GroupTime, hostV),
		textField("Timezone", "Timezone", "99 uses the TimeDst/TimeStd rules; otherwise -13 to 13 or +HH:MM.", GroupTime, timezoneV),
		textField("TimeDst", "Daylight saving rule", "Hemisphere, Week, Month, Day, Hour, Offset (minutes), separated by commas, e.g. 0,0,3,1,2,120.", GroupTime, ruleV),
		textField("TimeStd", "Standard time rule", "Hemisphere, Week, Month, Day, Hour, Offset (minutes), separated by commas, e.g. 0,0,10,1,3,60.", GroupTime, ruleV),
		{
			Key: "OtaUrl", Label: "OTA URL", Kind: KindText, Group: GroupFirmware, PolicyOnly: true,
			Help:     "Where the device fetches firmware from, e.g. after importing devices from a decommissioned update host. Only a fleet policy sets it; tasmofleet's own updates set and restore it themselves.",
			Validate: otaUrlV, Command: plain("OtaUrl"),
		},
	}
	return fs
}

// Lookup returns the field with the given key.
func Lookup(key string) (Field, bool) {
	for _, f := range Fields {
		if f.Key == key {
			return f, true
		}
	}
	return Field{}, false
}

// SetOption32 to SetOption49 are not flags: they are Settings->param[0..17], one
// byte each (button hold time, boot loop threshold, bistable pulse time, ...).
const (
	setOptionParamFirst = 32
	setOptionParamLast  = 49
)

// SetOptionParam says whether cmd is one of the numeric SetOption32..49.
func SetOptionParam(cmd string) bool {
	if !setOptionKeyRe.MatchString(cmd) {
		return false
	}
	n, err := strconv.Atoi(cmd[len("SetOption"):])
	return err == nil && n >= setOptionParamFirst && n <= setOptionParamLast
}

// SetOptionParamRange is the range Tasmota accepts (CmndSetoptionBase):
// SetOption32, 33 and 45 take 1 to 250, the others 0 to 255. Anything else
// is a Command Error on the device, not clamped.
func SetOptionParamRange(cmd string) (lo, hi int) {
	switch cmd {
	case "SetOption32", "SetOption33", "SetOption45":
		return 1, 250
	}
	return 0, 255
}

// ValidateSetOptionParam checks the value of a numeric SetOption.
func ValidateSetOptionParam(cmd, want string) error {
	lo, hi := SetOptionParamRange(cmd)
	return intRange(lo, hi)(want)
}

var setOptionKeyRe = regexp.MustCompile(`^SetOption[0-9]{1,3}$`)

// Curated says whether cmd names a curated field or a SetOption, in any letter
// case, and returns the spelling the catalog uses. A rule must spell such a
// command exactly like that: another case would send it through the generic
// path, without the curated validation and restart knowledge.
func Curated(cmd string) (canonical string, ok bool) {
	for _, f := range Fields {
		if strings.EqualFold(f.Key, cmd) {
			return f.Key, true
		}
	}
	if len(cmd) > len("SetOption") && strings.EqualFold(cmd[:9], "SetOption") && setOptionKeyRe.MatchString("SetOption"+cmd[9:]) {
		return "SetOption" + cmd[9:], true
	}
	return "", false
}

// Queryable reports whether a command may be read with a bare query: a
// curated non-secret field, a SetOption, or any other command that
// cmdpolicy.PolicyReadable accepts (never Status, secrets or commands that act
// when sent bare). The policy rules use it to refuse commands that would act
// when sent without a value.
func Queryable(cmd string) bool {
	if canon, ok := Curated(cmd); ok {
		if canon != cmd {
			return false
		}
		f, isField := Lookup(cmd)
		return !isField || !f.Secret
	}
	ok, _ := cmdpolicy.PolicyReadable(cmd)
	return ok
}

// Generic reports whether cmd is read and set as a generic policy command:
// not curated, so without a validator, without restart knowledge and never
// restarting the device.
func Generic(cmd string) bool {
	_, curated := Curated(cmd)
	return !curated && Queryable(cmd)
}

// statusSource says where a setting is read when its bare query has a side
// effect: a bare SaveData calls SettingsSaveAll() (a flash write) and a bare
// TelePeriod forces a telemetry publish. Status and Status 3 only report.
var statusSource = map[string]struct{ cmd, section, field string }{
	"PowerOnState": {"Status", "Status", "PowerOnState"},
	"SaveData":     {"Status", "Status", "SaveData"},
	"SetOption0":   {"Status", "Status", "SaveState"}, // SaveState is flag.save_state, i.e. SetOption0
	"TelePeriod":   {"Status 3", "StatusLOG", "TelePeriod"},
}

// SideEffectQueries are commands that must never be sent bare as a read.
var SideEffectQueries = []string{"SaveData", "TelePeriod"}

var restartKeys = map[string]bool{
	"Hostname": true, "Topic": true, "FullTopic": true,
	"MqttHost": true, "MqttPort": true, "MqttUser": true, "MqttPassword": true,
}

// RestartsDevice reports whether Tasmota restarts after the setting changes
// (a restart also saves all settings).
func RestartsDevice(key string) bool { return restartKeys[key] }
