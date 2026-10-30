package cmdpolicy

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Class is the authority a command needs.
type Class int

// Classes. The zero value is deliberately none of them: an unclassified
// verdict can never be sent (MinRole reports false).
const (
	ClassRead      Class = iota + 1 // queries; operator
	ClassOperator                   // harmless actions (Power, Restart 1); operator
	ClassAdmin                      // everything else; admin only
	ClassForbidden                  // malformed lines and file transfer commands; never sent
)

func (c Class) String() string {
	switch c {
	case ClassRead:
		return "read"
	case ClassOperator:
		return "operator"
	case ClassAdmin:
		return "admin"
	case ClassForbidden:
		return "forbidden"
	}
	return "unclassified"
}

// MinRole is the lowest role that may send commands of this class. ok is
// false for ClassForbidden and for values that are not a class.
func (c Class) MinRole() (role model.Role, ok bool) {
	switch c {
	case ClassRead, ClassOperator:
		return model.RoleOperator, true
	case ClassAdmin:
		return model.RoleAdmin, true
	case ClassForbidden:
		return model.RoleNone, false
	}
	return model.RoleNone, false
}

// Verdict is the classification of one command line.
type Verdict struct {
	Class  Class
	Name   string // command name as typed, with its index ("Power1"); never the arguments
	Reason string // why it is malformed or needs admin; safe to show
	Redact bool   // the arguments are secret: log Redacted(cmd) only
	// Confirm is set for destructive or lock-out commands: the consequence,
	// in one sentence that is safe to show. Callers with a UI ask for exactly
	// one confirmation that names it before they send; it is no authority.
	Confirm string
}

// Errors. Callers match with errors.Is.
var (
	ErrInvalid   = errors.New("cmdpolicy: malformed command")
	ErrForbidden = errors.New("cmdpolicy: command is never sent from tasmofleet")
	ErrRole      = errors.New("cmdpolicy: command needs a higher role")
)

// RejectedText replaces a malformed command in logs and history.
const RejectedText = "[rejected: malformed command]"

const (
	maxCommandLen = 255
	maxNameLen    = 40
)

// DefaultOperator are example commands at operator level, for display and
// for allowlists. Classify stays the authority: a test keeps this list true.
var DefaultOperator = []string{
	"Power", "Power1 TOGGLE", "Power1 ON", "Power1 OFF",
	"Status 1", "Status 2", "Status 3", "Status 4", "Status 5", "Status 6",
	"Status 7", "Status 8", "Status 9", "Status 10", "Status 11",
	"State", "Time", "Uptime", "Restart 1",
}

// secretNames carry credentials or network names in their arguments. Admins
// may send them (after a confirmation); their arguments are never logged.
var secretNames = map[string]bool{
	"password": true, "webpassword": true, "mqttpassword": true, "ssid": true,
	"tmtoken": true, "ifxpassword": true, "ifxtoken": true, "ifxuser": true,
	"tlskey": true, "lorawanappkey": true,
}

// Consequences shown on the confirmation step.
const (
	whyReset    = "Reset erases the device configuration; the device has to be set up again and may drop off the network."
	whyWifiCfg  = "WifiConfig puts the device into its Wi-Fi setup mode; it leaves the network until it is configured again."
	whyFirmware = "This starts or redirects a firmware update on the device without tasmofleet's firmware safety checks."
	whyWifiOff  = "Switching Wi-Fi off cuts the device off from tasmofleet."
	whyWebSrv   = "Any WebServer mode but 2 stops the device from accepting commands over HTTP, so tasmofleet can no longer reach it."
	whyCutOff   = "Changing the address or network settings can leave the device unreachable at the address tasmofleet uses."
	whyModule   = "Changing the module or template changes the pin mapping; the device may restart or stop working until it is corrected."
	whyCredCmd  = "Changing a login or the Wi-Fi name can lock tasmofleet out or take the device off the network; tasmofleet does not store the new value."
	whyRestart  = "Restart other than 1 halts the device or erases settings; it may stay down until power is cycled."
	whyMQTTOnly = "this device is reached over MQTT only; this change can cut TasmoFleet off from it"
	whyFile     = "TasmoFleet's own code handles file transfers; the command carries the device's MQTT password."
	whyStatus0  = "Status 0 makes the device build one very large reply; ESP8266 devices can run out of memory. tasmofleet's own code never sends it."
)

// queryNames are read-only when sent without arguments (Tasmota answers with
// the current value). With arguments they change settings: admin. Their
// arguments are harmless to log (see Redacted).
var queryNames = map[string]bool{
	"time": true, "uptime": true, "module": true, "modules": true, "template": true,
	"gpio": true, "gpios": true, "hostname": true, "topic": true, "fulltopic": true,
	"fallbacktopic": true, "teleperiod": true, "setoption": true, "poweronstate": true,
	"webserver": true, "weblog": true, "seriallog": true,
	"friendlyname": true, "devicename": true, "timezone": true, "timedst": true,
	"timestd": true, "ntpserver": true, "mqtthost": true, "mqttport": true,
	"mqttclient": true, "state": true, "sleep": true, "latitude": true,
	"longitude": true, "tempoffset": true, "ledstate": true,
}

// loggedArgs are the commands whose arguments may appear in logs and history
// besides the operator-level ones: the same settings, plus SaveData (which is
// not a pure query: it writes flash, so it is admin even without arguments).
var loggedArgs = func() map[string]bool {
	m := map[string]bool{"savedata": true}
	for k := range queryNames {
		m[k] = true
	}
	// Hosts are where users paste "user:pass@host".
	delete(m, "mqtthost")
	delete(m, "ntpserver")
	return m
}()

// synonyms maps Tasmota's short names to the command they stand for.
var synonyms = map[string]string{"so": "setoption"}

// semicolonLookalikes would be read as a command separator by a human or by
// a normalising intermediary even though the device ignores them.
var semicolonLookalikes = map[rune]bool{
	';': true, ';': true, '؛': true, '⁏': true, '⸵': true,
	'︔': true, '﹔': true, '；': true,
}

func invalid(why string) (Verdict, error) {
	return Verdict{Class: ClassForbidden, Name: "?", Reason: "malformed command: " + why, Redact: true},
		fmt.Errorf("%w: %s", ErrInvalid, why)
}

// parsed is one split command line.
type parsed struct {
	name string // as typed, e.g. "Power1"
	base string // lower case without trailing digits, e.g. "power"
	idx  int
	has  bool   // name ended in digits
	args string // trimmed
}

func parse(cmd string) (parsed, string) {
	if !utf8.ValidString(cmd) {
		return parsed{}, "not valid UTF-8"
	}
	cmd = strings.Trim(cmd, " ")
	if cmd == "" {
		return parsed{}, "empty"
	}
	if len(cmd) > maxCommandLen {
		return parsed{}, "too long"
	}
	// Backlog is the one command whose arguments legitimately hold ';'. Each
	// part is classified on its own (classifyBacklog), so nothing hides in it.
	head, _, _ := strings.Cut(cmd, " ")
	backlog := strings.EqualFold(strings.TrimRight(head, "0123456789"), "backlog")
	for _, r := range cmd {
		switch {
		case r == ' ':
		case r == ';' && backlog:
		case semicolonLookalikes[r]:
			return parsed{}, "contains a command separator"
		case !unicode.IsPrint(r):
			return parsed{}, "contains a control or invisible character"
		}
	}
	if strings.Contains(strings.ToLower(cmd), "%3b") {
		return parsed{}, "contains an encoded command separator"
	}
	name, args, _ := strings.Cut(cmd, " ")
	if len(name) > maxNameLen || !validName(name) {
		return parsed{}, "bad command name"
	}
	p := parsed{name: name, args: strings.TrimSpace(args)}
	p.base = strings.ToLower(name)
	end := len(p.base)
	for end > 0 && p.base[end-1] >= '0' && p.base[end-1] <= '9' {
		end--
	}
	if end == 0 {
		return parsed{}, "bad command name"
	}
	if end < len(p.base) {
		n, err := strconv.Atoi(p.base[end:])
		if err != nil {
			return parsed{}, "bad command index"
		}
		p.idx, p.has, p.base = n, true, p.base[:end]
	}
	if canon, ok := synonyms[p.base]; ok {
		p.base = canon // SO19 is SetOption19
	}
	return p, ""
}

func validName(s string) bool {
	if s == "" || !isLetter(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isLetter(s[i]) && (s[i] < '0' || s[i] > '9') {
			return false
		}
	}
	return true
}

func isLetter(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' }

// Classify parses one command line and says what it needs. A malformed line
// returns ErrInvalid and a Forbidden verdict, so a caller that ignores the
// error still cannot send it. Everything else is sendable by an admin; the
// verdict's Confirm names the consequence of a destructive or lock-out
// command. Authorize adds the role check.
func Classify(cmd string) (Verdict, error) {
	p, bad := parse(cmd)
	if bad != "" {
		return invalid(bad)
	}
	v, bad := classify(p)
	if bad != "" {
		return invalid(bad)
	}
	return v, nil
}

func classify(p parsed) (Verdict, string) {
	v := Verdict{Name: p.name, Redact: secretNames[p.base]}
	switch p.base {
	case "status":
		return classifyStatus(p, v), ""
	case "restart":
		if p.args == "1" && !p.has {
			v.Class = ClassOperator
			return v, ""
		}
		v.Class, v.Reason, v.Confirm = ClassAdmin, "changes device state", whyRestart
		return v, ""
	case "power":
		return classifyPower(p, v), ""
	case "filedownload", "fileupload", "upload":
		// Never typed by a person, whatever the role: the payload carries the device's MQTT password
		// and the transfer is a protocol of its own (internal/mqttcmd). Redact: the arguments may
		// hold that password.
		v.Class, v.Reason, v.Redact = ClassForbidden, whyFile, true
		return v, ""
	case "backlog":
		return classifyBacklog(p, v)
	}
	v.Class, v.Reason = ClassAdmin, "changes device state"
	v.Confirm = confirmFor(p)
	if v.Confirm == "" && p.args == "" && queryNames[p.base] {
		v.Class, v.Reason = ClassRead, ""
	}
	return v, ""
}

// confirmFor names the consequence of the destructive or lock-out commands,
// or returns "" for everything else.
func confirmFor(p parsed) string {
	switch p.base {
	case "reset":
		return whyReset
	case "wificonfig":
		return whyWifiCfg
	case "upgrade", "otaurl":
		if p.args != "" {
			return whyFirmware
		}
	case "wifi":
		// Tasmota reads "0", "off", "00", "+0" and more as off.
		if p.args != "" && unclearOrOff(p.args) {
			return whyWifiOff
		}
	case "webserver":
		if p.args != "" && p.args != "2" {
			return whyWebSrv
		}
	case "ipaddress", "ethernet", "ap":
		if p.args != "" {
			return whyCutOff
		}
	case "module", "template":
		if p.args != "" {
			return whyModule
		}
	default:
		if secretNames[p.base] && p.args != "" {
			return whyCredCmd
		}
	}
	return ""
}

// classifyBacklog judges every part of a Backlog on its own, so a Reset or a
// password inside it asks for the same confirmation and is redacted the same
// way as when sent alone.
func classifyBacklog(p parsed, v Verdict) (Verdict, string) {
	v.Class, v.Reason = ClassAdmin, "changes device state"
	seen := map[string]bool{}
	for _, part := range strings.Split(p.args, ";") {
		part = stripBacklogPrefix(part)
		if part == "" {
			continue
		}
		sub, bad := parse(part)
		if bad != "" {
			return v, bad
		}
		sv, bad := classify(sub)
		if bad != "" {
			return v, bad
		}
		if sv.Class == ClassForbidden { // a part that is never sent makes the whole Backlog never sent
			v.Class, v.Reason, v.Redact = ClassForbidden, sv.Reason, true
			return v, ""
		}
		if sv.Confirm != "" && !seen[sv.Confirm] {
			seen[sv.Confirm] = true
			if v.Confirm != "" {
				v.Confirm += " "
			}
			v.Confirm += sv.Confirm
		}
		v.Redact = v.Redact || sv.Redact
	}
	return v, ""
}

// stripBacklogPrefix removes the leading "Backlog" words of one Backlog part the way Tasmota's
// CmndBacklog does (support_command.ino): trim, strip "backlog" case-insensitively with or
// without a space after it, repeat. "BacklogStatus 0" and "BACKLOGBacklogStatus 0" are Status 0.
func stripBacklogPrefix(part string) string {
	for {
		part = strings.TrimSpace(part)
		if len(part) < len("backlog") || !strings.EqualFold(part[:len("backlog")], "backlog") {
			return part
		}
		part = part[len("backlog"):]
	}
}

// unclearOrOff reports whether arg is anything but a plain positive number or
// ON/TRUE. Tasmota reads "0", "off", "false", "00", "+0", "0x0" and more as
// off, so only the clearly-on spellings pass.
func unclearOrOff(arg string) bool {
	switch strings.ToLower(arg) {
	case "", "on", "true":
		return false
	}
	n, ok := canonicalNumber(arg)
	return !ok || n == 0
}

func classifyStatus(p parsed, v Verdict) Verdict {
	arg := p.args
	unclear := false
	switch {
	case p.has && arg != "":
		unclear = true // "Status1 2": which one does the device take?
	case p.has:
		arg = strconv.Itoa(p.idx)
		if p.idx == 0 {
			arg = "0"
		}
	}
	if arg == "" {
		v.Class = ClassRead // bare Status is the abbreviated object only
		return v
	}
	n, ok := canonicalNumber(arg)
	switch {
	case unclear || !ok || n == 0:
		// Tasmota's atoi reads "00", "+0", "0x0" and "off" as 0 as well, so
		// anything that is not a plain Status N is treated as Status 0.
		v.Class, v.Reason, v.Confirm = ClassAdmin, "changes device state", whyStatus0
	case n <= 11:
		v.Class = ClassRead
	default:
		v.Class, v.Reason = ClassAdmin, "only Status 1 to 11 are operator queries"
	}
	return v
}

// canonicalNumber accepts 0 and plain decimals without sign, zeros or other
// notation, so that what we classify is what Tasmota's atoi would read.
func canonicalNumber(s string) (int, bool) {
	if s == "0" {
		return 0, true
	}
	if s == "" || s[0] < '1' || s[0] > '9' || len(s) > 3 {
		return 0, false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

func classifyPower(p parsed, v Verdict) Verdict {
	const maxRelay = 8
	// Power0 means "all relays", including ones above 8: not an operator action.
	if p.has && (p.idx < 1 || p.idx > maxRelay) {
		v.Class, v.Reason = ClassAdmin, "only relays 1 to 8 are operator actions"
		return v
	}
	switch strings.ToLower(p.args) {
	case "":
		v.Class = ClassRead
	case "on", "off", "toggle", "0", "1", "2":
		v.Class = ClassOperator
	default:
		v.Class, v.Reason = ClassAdmin, "only ON, OFF and TOGGLE are operator actions"
	}
	return v
}

// Authorize classifies cmd and checks it against role. It returns the verdict
// and nil when the role may send it; otherwise ErrInvalid, ErrForbidden or
// ErrRole (wrapped, with a safe reason).
func Authorize(cmd string, role model.Role) (Verdict, error) {
	v, err := Classify(cmd)
	if err != nil {
		return v, err
	}
	min, ok := v.Class.MinRole()
	if !ok {
		return v, fmt.Errorf("%w: %s", ErrForbidden, v.Reason)
	}
	if role < min || role > model.RoleAdmin {
		return v, fmt.Errorf("%w: %s needs the %s role", ErrRole, v.Name, roleLabel(min))
	}
	return v, nil
}

func roleLabel(r model.Role) string {
	if r == model.RoleAdmin {
		return "admin"
	}
	return "operator"
}

// Redacted is the only form of cmd that may be written to logs, the audit
// log and the console history. It is an allowlist: arguments are shown only
// for operator-level commands and the harmless settings in loggedArgs (with
// credential patterns scrubbed); every other command, refused ones included,
// shows as "Name ***", and a malformed line as RejectedText. A secret wrapped
// in another command (Backlog, Json, Rule, Publish, ...) therefore never
// reaches a log.
func Redacted(cmd string) string {
	p, bad := parse(cmd)
	if bad != "" {
		return RejectedText
	}
	if p.args == "" {
		return p.name
	}
	// An allowlist: arguments are shown only where they are known to be
	// harmless. Everything else, refused commands included, may hold a secret
	// (directly or wrapped in another command) and shows as "Name ***".
	v, bad := classify(p)
	if bad != "" {
		return RejectedText
	}
	if p.base == "backlog" {
		// Each part is redacted on its own, so a log shows what was chained
		// without the secrets inside.
		var parts []string
		for _, part := range strings.Split(p.args, ";") {
			if part = strings.TrimSpace(part); part != "" {
				parts = append(parts, Redacted(part))
			}
		}
		return p.name + " " + strings.Join(parts, "; ")
	}
	if c := v.Class; c != ClassRead && c != ClassOperator && !loggedArgs[p.base] {
		return p.name + " ***"
	}
	p.args = scrubText(p.args)
	return p.canonical()
}

// Canonical is the exact text that is classified and, if allowed, sent: the
// command name, one blank, the arguments, without surrounding blanks.
// Classify(Canonical(x)) always equals Classify(x), so there is no gap between
// what was judged and what reaches the device. Callers send Canonical's
// result, never their original string.
func Canonical(cmd string) (string, error) {
	p, bad := parse(cmd)
	if bad != "" {
		return "", fmt.Errorf("%w: %s", ErrInvalid, bad)
	}
	return p.canonical(), nil
}

func (p parsed) canonical() string {
	if p.args == "" {
		return p.name
	}
	return p.name + " " + p.args
}

// PrepareOpts carries what the policy needs to know about the device.
type PrepareOpts struct {
	// MQTTOnly: tasmofleet reaches the device over MQTT only (HTTP fails). A change of its MQTT
	// identity or connection can then cut it off, which an HTTP-reachable device would survive.
	MQTTOnly bool
}

// cutsMQTTOff reports whether p changes how the device talks MQTT: its topics, prefixes, host, port,
// login, client name, TLS fingerprint or client key (Prefix<n>, MqttFingerprint, TLSKey<n>, SetOption103 TLS, SetOption132
// fingerprint check), or switches MQTT off (SetOption3 0). Queries (no argument) do not.
func cutsMQTTOff(p parsed) bool {
	if p.args == "" {
		return false
	}
	switch p.base {
	case "topic", "fulltopic", "grouptopic", "mqtthost", "mqttport", "mqttuser", "mqttpassword", "mqttclient",
		"prefix", "mqttfingerprint", "tlskey":
		return true
	case "setoption":
		switch {
		case !p.has:
			return false
		case p.idx == 3:
			return unclearOrOff(p.args)
		case p.idx == 103 || p.idx == 132:
			return true
		}
	}
	return false
}

// CutOffName returns the name of the first command in cmd (a Backlog is judged part by part) that can
// cut a device off that tasmofleet reaches over MQTT only, "" when none can.
func CutOffName(cmd string) string {
	p, bad := parse(cmd)
	if bad != "" {
		return ""
	}
	if p.base != "backlog" {
		if cutsMQTTOff(p) {
			return p.name
		}
		return ""
	}
	for _, part := range strings.Split(p.args, ";") {
		if sub, bad := parse(stripBacklogPrefix(part)); bad == "" && cutsMQTTOff(sub) {
			return sub.name
		}
	}
	return ""
}

// CutsMQTTOff reports whether cmd can cut an MQTT-only device off (see CutOffName).
func CutsMQTTOff(cmd string) bool { return CutOffName(cmd) != "" }

// WithMQTTOnly adds the consequence of cutting an MQTT-only device off to the verdict of cmd. It never
// lowers the class and never removes a confirmation.
func WithMQTTOnly(cmd string, v Verdict) Verdict {
	if !CutsMQTTOff(cmd) || v.Class == ClassForbidden || strings.Contains(v.Confirm, whyMQTTOnly) {
		return v
	}
	if v.Confirm != "" {
		v.Confirm += " "
	}
	v.Confirm += whyMQTTOnly
	return v
}

// Prepare authorizes cmd for role and returns the canonical text to send. It
// is the single entry point for code that sends free-form commands.
func Prepare(cmd string, role model.Role, opts PrepareOpts) (send string, v Verdict, err error) {
	if v, err = Authorize(cmd, role); err != nil {
		return "", v, err
	}
	if opts.MQTTOnly {
		v = WithMQTTOnly(cmd, v)
	}
	send, err = Canonical(cmd)
	if err != nil {
		return "", v, err
	}
	return send, v, nil
}
