package cmdpolicy

import (
	"errors"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Reasons a command cannot be a fleet policy rule. They are shown to the person
// who edits the rule, so each is one plain sentence without the rejected text.
const (
	reasonBadName   = "not a valid command name"
	reasonStatus    = "Status commands are never sent automatically"
	reasonSecret    = "secret settings cannot be checked"
	reasonActs      = "this command acts when it is sent without a value, so it cannot be read safely"
	reasonSetOption = "SetOption needs its number, 0 to 999, in the exact spelling SetOption<n>"
	reasonSetActs   = "setting this command restarts, locks out or actuates the device, so it cannot be a policy rule"
	reasonSynonym   = "write the full name (SetOption19, not SO19)"
	reasonConfirm   = "this command needs a confirmation in the console and cannot be a policy rule"
)

// secretParts mark a command name that carries a credential or a network name
// (case-insensitive substring), besides the exact names in secretNames. They
// mirror the JSON key parts ScrubReply masks, plus "key" for TLSKey and
// LoRaWanAppKey style names.
var secretParts = []string{"password", "passwd", "pwd", "token", "secret", "apikey", "mqttuser", "ssid", "key"}

// actingExact are commands whose bare form is not a plain read: it restarts,
// resets, saves, publishes, switches, scans or runs something. Digits at the
// end of the name do not matter (Power1, Sensor13, Driver4 are the same
// commands as Power, Sensor, Driver).
var actingExact = map[string]bool{
	"restart": true, "reset": true, "upgrade": true, "upload": true,
	"savedata": true, "teleperiod": true, // read through their Status block, see settings
	"delay": true, "event": true, "subscribe": true, "unsubscribe": true,
	"timers": true, "power": true, "sensor": true, "driver": true,
	"json": true, "if": true, "i2cscan": true, "buzzer": true, "ufsrun": true,
	"webgetconfig": true, "telnet": true, "sendmail": true, "loracommand": true,
	"tcpstart": true, "tcpconnect": true, "blescan": true, "wcinit": true,
}

// actingPrefix are families of acting commands.
var actingPrefix = []string{"backlog", "publish", "rule", "timer", "wifi", "ping", "wakeup", "script", "zb",
	"shutter", "mp3", "rfkey", "br"}

// setActingExact are commands whose bare form only reports but whose set form, which
// Make compliant would send, restarts the device (Gpio, Module, SetSensor, I2CDriver,
// MqttClient, GroupTopic, Prefix, Emulation, TuyaMCU, MqttFingerprint, CpuFrequency),
// can lock it out or act on the device (DeepSleepTime, WebQuery, UfsDelete, UfsRename,
// Exception, FlashMode), actuates it like Power (Dimmer, Channel, Color, CT, White,
// HsbColor, PWM) or corrupts calibration and counters (PowerSet, PowerCal, EnergyReset).
// Digits at the end of the name do not matter.
var setActingExact = map[string]bool{
	"module": true, "modules": true, "setsensor": true, "i2cdriver": true, "mqttclient": true,
	"grouptopic": true, "prefix": true, "emulation": true, "tuyamcu": true, "mqttfingerprint": true,
	"cpufrequency": true, "deepsleeptime": true, "webquery": true, "ufsdelete": true, "ufsrename": true,
	"exception": true, "flashmode": true, "dimmer": true, "channel": true, "color": true, "ct": true,
	"white": true, "hsbcolor": true, "pwm": true, "powerset": true, "powercal": true, "energyreset": true,
}

// setActingPrefix are families of such commands: Gpio and Gpios, Eth*, Cfg*.
var setActingPrefix = []string{"gpio", "eth", "cfg"}

// rawBase is the lower-case command name without its trailing index digits.
func rawBase(name string) string {
	return strings.TrimRight(strings.ToLower(name), "0123456789")
}

// PolicyReadable says whether a fleet policy rule may name this command: tasmofleet
// then reads it from the devices by sending the bare name, and may later set it.
// ok is false, with a reason that is safe to show, for a name that is not one
// plain command name, for every Status spelling (Status 0 is never sent
// automatically), for secrets, and for commands whose bare form acts instead of
// reporting (Backlog, Restart, Reset, Upgrade, Upload, Delay, Event, Publish,
// Subscribe, Rule, Timer, Power, Sensor, Driver, Wifi, Ping, WakeUp, Script, Br*, Shutter*,
// Mp3*, RfKey*,
// and the few more in actingExact). SaveData and TelePeriod are in that list
// too: the curated settings read them through Status blocks. It is a deny list
// over a name, so a new Tasmota command is readable until someone lists it; it also
// holds the commands whose set form restarts or actuates the device (setActingExact);
// the confirmation check (PolicySetText) and the Reply scrubbing still apply.
// Pure: the caller decides whether the name is a curated setting first.
func PolicyReadable(name string) (ok bool, why string) {
	if name != strings.TrimSpace(name) || strings.ContainsAny(name, " \t") {
		return false, reasonBadName
	}
	p, bad := parse(name)
	if bad != "" || p.args != "" {
		return false, reasonBadName
	}
	base := rawBase(name)
	if _, syn := synonyms[base]; syn {
		return false, reasonSynonym
	}
	if strings.HasPrefix(base, "setoption") {
		return false, reasonSetOption // SetOption0 to 999 are curated; the caller resolves them first
	}
	if strings.HasPrefix(base, "status") {
		return false, reasonStatus
	}
	// before the secret check, so RfKey is named for what it does, not for "key"
	for _, pre := range actingPrefix {
		if strings.HasPrefix(base, pre) {
			return false, reasonActs
		}
	}
	switch {
	case secretNames[base] || hasAny(base, secretParts):
		return false, reasonSecret
	case actingExact[base]:
		return false, reasonActs
	case strings.HasSuffix(base, "send"):
		return false, reasonActs
	case setActingExact[base]:
		return false, reasonSetActs
	}
	for _, pre := range setActingPrefix {
		if strings.HasPrefix(base, pre) {
			return false, reasonSetActs
		}
	}
	return true, ""
}

func hasAny(s string, parts []string) bool {
	for _, p := range parts {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// PolicySetText is the command a policy sets for a generic rule: the canonical
// "<name> <want>", authorised for an admin. It refuses a command that needs a
// confirmation (Reset, Module, Wifi 0, ...) or hides its arguments (credentials):
// nothing that asks for a confirmation in the console may be sent without one.
// The error text is a sentence that is safe to show. The caller has checked
// PolicyReadable(name).
func PolicySetText(name, want string) (string, error) {
	send, v, err := Prepare(name+" "+want, model.RoleAdmin, PrepareOpts{})
	switch {
	case errors.Is(err, ErrInvalid):
		return "", errors.New("the expected value is not a plain value for this command")
	case err != nil:
		return "", errors.New(reasonConfirm)
	case v.Redact:
		return "", errors.New(reasonSecret)
	case v.Confirm != "":
		return "", errors.New(reasonConfirm)
	}
	return send, nil
}

// PolicyCommand is PolicyReadable followed by PolicySetText: the one check for a
// generic rule, as a single error.
func PolicyCommand(name, want string) error {
	if ok, why := PolicyReadable(name); !ok {
		return errors.New(why)
	}
	_, err := PolicySetText(name, want)
	return err
}
