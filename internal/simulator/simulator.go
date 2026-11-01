// Package simulator emulates Tasmota devices for integration and e2e tests:
// the HTTP command API (/cm, /dl, WebPassword auth), MQTT behaviour (LWT,
// tele STATE at TelePeriod, stat RESULT/STATUSn, discovery config), and OTA
// (OtaUrl, Upgrade, download from the given URL, reboot gap, version and
// variant change, the minimal two-step for 1 MB ESP8266).
//
// Device answers are replayed from testdata/fixtures (real, sanitised
// captures) and mutated only where state changes (power, OtaUrl, version,
// uptime). Scenarios inject faults: reboot gaps, timeouts, dropped
// connections, unknown commands, failed downloads, wrong-family firmware.
//
// It is a library (tests start devices in-process on 127.0.0.1:0) and is
// also exposed by cmd/tasmofleet-devenv for Playwright e2e runs.
//
// Test and dev only: no production package may import it (depguard rule
// no-test-helpers-in-prod and TestNotInProductionBinary). Devices listen on
// 127.0.0.1 unless Extra.BindAddr opts in to something else, and their OTA
// downloads only reach loopback or Extra.AllowDownloadHosts, never follow
// redirects and are size- and time-limited.
package simulator

import (
	"context"
	"time"
)

// Profile selects the captured device behaviour a simulated device replays.
type Profile string

// Profiles; each maps to a directory under testdata/fixtures/devices.
const (
	ProfileESP8266_1M  Profile = "esp8266-1m" // ESP8285N08, needs staged update
	ProfileESP8266_4M  Profile = "esp8266-4m" // ESP8266EX, direct
	ProfileESP8266Min  Profile = "esp8266-minimal"
	ProfileESP32       Profile = "esp32"      // ESP32-D0WD-V3, tasmota32
	ProfileESP32Dual   Profile = "esp32-dual" // two app partitions: the push upload goes straight to the running firmware (#30)
	ProfileESP32C3     Profile = "esp32c3"
	ProfileESP32S2     Profile = "esp32s2" // two upstream builds (s2, s2cdc) behind one image tag (#88)
	ProfileESP32S3     Profile = "esp32s3"
	ProfileUnknownChip Profile = "unknown-chip" // Hardware string the detector must refuse
)

// Fault is a scripted misbehaviour.
type Fault struct {
	// After: apply when this command is received (e.g. "Upgrade 1"); "" = always.
	After string
	// Kind: "timeout", "drop", "unknown_command", "auth", "download_fail",
	// "stay_old_version", "long_reboot", "flap".
	Kind string
	// Duration for timeouts and reboot gaps.
	Duration time.Duration
	// Chunks: data chunks acknowledged before FaultMQTTUploadCut falls silent (default 1).
	Chunks int
}

// DeviceSpec describes one simulated device.
type DeviceSpec struct {
	Name      string
	Profile   Profile
	Version   string // starting firmware version
	Variant   string
	User      string
	Password  string
	Faults    []Fault
	RebootGap time.Duration // offline time after Upgrade/Restart, default 3s in tests
	// NoDiscovery leaves out the retained tasmota/discovery messages, like a
	// build without USE_TASMOTA_DISCOVERY or a device with SetOption19 1: the
	// device is on the broker (LWT, STATE, INFO) but announces no MAC.
	NoDiscovery bool
	// Relays is the number of relays (POWER1..n); 0 or 1 is a single relay
	// reported as POWER. Relay 1 starts as captured, the others OFF.
	Relays int
	// NoRelay makes a sensor-only device: no POWER key in Status, STATE or RESULT. Relays
	// and Power are ignored.
	NoRelay bool
	// Light marks a PWM light (MagicHome and the like): after a reboot it
	// reports its old state for about 3 s and then switches itself OFF (unless
	// SetOption0 1), which is what the fleet's lights do after an update.
	Light bool
	// SetOption0 overrides the capture's SetOption0 (nil = as captured; the
	// ESP32 capture has it ON like the fleet's lamps, the ESP8266 captures OFF).
	// With SetOption0 1 the relay state is saved on every change and restored
	// at boot, on every chip and over every image. Without it (PowerOnState 0)
	// an ESP32 relay and a light come back OFF; an ESP8266 relay keeps its
	// state, also through the minimal image (observed on the fleet,
	// 2026-10-19, see docs/research/tasmota-minimal-and-ota.md).
	SetOption0 *bool
	// PulseTimes[i] is the PulseTime of relay i+1 (0 = none), Interlock the
	// groups of an enabled interlock ("1,2 3,4"); both are answered to the
	// bare read-only commands "PulseTime" and "Interlock".
	PulseTimes []int
	Interlock  string
	// Power is the initial state of relay 1..n (nil = as captured; relays
	// beyond len(Power) start OFF).
	Power []bool
	// Settings are initial values by lower-case command key ("teleperiod",
	// "poweronstate", "mqtthost", ...), as if saved to flash before boot. They
	// do not restart the device.
	Settings map[string]string
	// RefererCheck models SetOption128 0, Tasmota's default: /cm, /dl, /up,
	// /u1 and /u2 answer only requests whose Referer starts, right after
	// "http://", with the device's hostname or its IP. Any other request is
	// refused the way the real web server does it: the handler sends nothing
	// and the connection closes without a single byte (the device log reads
	// "HTTP: Referer '' denied. Use 'SO128 1' ..."). The zero value is
	// SetOption128 1, which is what the existing tests assume.
	RefererCheck bool
	// Wired makes the device an Ethernet board: Status 5 reports the Wi-Fi station idle
	// (0.0.0.0) and the real address, gateway, subnet mask, DNS and MAC in "Ethernet".
	Wired bool
	// FriendlyNames are the device's FriendlyName1..n (one per relay), as plain Status and
	// Status 0 report them. Nil keeps the capture's single name.
	FriendlyNames []string
	// FreeKB overrides the "Free" flash space Status 4 reports (0 = as captured). A running
	// partition with almost no free space is what an ESP32 with the safeboot layout looks like to
	// an image upload (#30).
	FreeKB int
	// MQTTPassword is the device's MqttPassword, which FileDownload and FileUpload
	// compare their "Password" with (Error 1 on a mismatch).
	MQTTPassword string
	// Energy makes the device publish an ENERGY block in tele/SENSOR, shaped like the
	// fleet's energy-status.json captures: 0 = no meter, 1 = one meter (a number per
	// field), n >= 2 = n channels the way Tasmota sends them: Power, ApparentPower,
	// ReactivePower, Factor and Current as arrays of n; Total, Yesterday, Today (summed
	// over the channels) and Voltage as one number each.
	Energy int
	// SignalDBm and LinkCount replace the capture's Wi-Fi readings (Signal, the RSSI percentage derived
	// from it, LinkCount) in every STATE and Status 11; 0 keeps the capture. A weak link is -80 with
	// LinkCount 14.
	SignalDBm, LinkCount int
	// Sensors are published in tele/SENSOR as one block per sensor name, for example
	// {"BME280": {"Temperature": 21.5}}, followed by TempUnit "C" (and PressureUnit "hPa" when a block has a Pressure).
	Sensors map[string]map[string]float64
	// Files seeds the flash file system by absolute path ("/autoexec.be", "/lib/x.be"; folders are
	// implied). Nil on an ESP32 profile seeds /autoexec.be, /lib/helper.be and a fake /tlskey; nil on
	// an ESP8266 profile means a build without a file system (UfsType answers {"Command":"Unknown"}).
	// Dirs adds empty folders. NoFS removes the file system from an ESP32 profile. FSSizeKB is the
	// size UfsSize reports (default 320, the 4 MB ESP32 layout). Minimal and safeboot images have none.
	Files    map[string]FSFile
	Dirs     []string
	NoFS     bool
	FSSizeKB int
}

// Device is a running simulated device.
type Device interface {
	Addr() string // host:port of its HTTP API
	Topic() string
	Version() string
	Variant() string
	Commands() []string // every command received, for assertions
	Close() error
}

// Fleet runs simulated devices, optionally attached to an MQTT broker.
type Fleet interface {
	Add(ctx context.Context, spec DeviceSpec) (Device, error)
	Devices() []Device
	Close() error
}

// FleetOptions configure a Fleet.
type FleetOptions struct {
	FixturesDir string // path to testdata/fixtures
	MQTTURL     string // empty = HTTP only
	TelePeriod  time.Duration
}

// Fault kinds. The first group applies to requests (After selects the
// command), the second is armed by the Upgrade command that starts an OTA.
const (
	FaultTimeout        = "timeout"         // command executes, reply delayed by Duration (0 = never)
	FaultDrop           = "drop"            // command executes, connection closed without a reply
	FaultUnknownCommand = "unknown_command" // command not executed, {"Command":"Unknown"} instead
	FaultAuth           = "auth"            // HTTP 401, command not executed
	FaultFlap           = "flap"            // MQTT connection flaps every Duration (default 200 ms)
	FaultRebootHang     = "reboot_hang"     // while rebooting, accept connections and hang instead of refusing

	FaultDownloadFail   = "download_fail"    // download fails with "File Not Found (404)" on every attempt
	FaultStayOldVersion = "stay_old_version" // flash "succeeds", device boots the old version
	FaultLongReboot     = "long_reboot"      // reboot gap extended by Duration (default 10x)
	FaultWrongFamily    = "wrong_family"     // header check fails ("Verify Bin Header Failed") whatever the image
	FaultNeverReturns   = "never_returns"    // device goes offline after the restart and stays there
	FaultHTTPOnly       = "http_only"        // device does not use MQTT even if the fleet has a broker

	// FaultHTTPUnreachable: HTTP refuses every connection (no listener; the address stays and
	// the refusal survives reboots), MQTT works: a device TasmoFleet cannot reach but the broker can.
	FaultHTTPUnreachable = "http_unreachable"
	// FaultFWUnreachable: the device cannot reach the firmware server. Upgrade 1 is accepted;
	// after Duration (default the 66 s the connect attempt takes, scaled) it publishes
	// stat/<t>/UPGRADE {"Upgrade":"Failed HTTP error: connection failed"}. No reboot, the
	// device stays responsive (observed live, 2026-10-30). WebGetConfig answers "Connect failed" after the same time.
	FaultFWUnreachable = "fw_unreachable"
)

// Extra holds fleet options beyond the frozen FleetOptions.
type Extra struct {
	// TimeScale multiplies every nominal duration of the OTA timeline
	// (pending 3 s, download >= 10 s, restart wait 7 s, reboot 8 s, ...).
	// 0 means 1. Tests use 0.01-0.05.
	TimeScale float64
	// Verbatim replays captures unchanged: no live fields, no IP/MAC rewrite.
	// With Name == captured topic and the captured version, replies are
	// byte-identical to the fixtures.
	Verbatim bool
	// FlashVersion is the version a device reports after flashing an image
	// that carries no version marker. Default "16.0.0".
	FlashVersion string
	// AllowDownloadHosts are extra hosts (as written in the OtaUrl) the
	// simulated devices may download from, besides loopback.
	AllowDownloadHosts []string
	// BindAddr is the IP the device listeners bind to. Default 127.0.0.1;
	// anything else is an explicit opt-in and logs a warning.
	BindAddr string
}

// NewFleet starts an empty fleet.
func NewFleet(o FleetOptions) (Fleet, error) { return NewFleetWith(o, Extra{}) }
