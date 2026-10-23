package model

import "time"

// DeviceID is the inventory primary key (SQLite rowid).
type DeviceID int64

// Device is one Tasmota device in the inventory. The inventory is the only
// source of network targets: nothing in tasmofleet talks to an address that
// is not stored here (SSRF rule, see spec section "Security").
type Device struct {
	ID      DeviceID
	Name    string // display name, usually Tasmota DeviceName, e.g. "kitchen-spotlight"
	Address string // IP or DNS name, no scheme, no port
	Port    int    // HTTP port, 0 means 80
	MAC     string // lower case, colon separated; primary MAC (Wi-Fi station), stable identity when known
	// MACs is the full MAC set of the device: MAC plus, where present, the
	// Ethernet MAC (ESP32 boards report one MAC per interface). Identity is
	// decided by the set (spec 11.1). The store fills it on load. On create it
	// may carry extra MACs; it changes afterwards only through an explicit
	// action ("Replace device"), never through discovery, MQTT, import or probes.
	MACs      []string
	Topic     string // MQTT %topic%; empty when the device is HTTP only
	FullTopic string // MQTT FullTopic; empty means Tasmota default "%prefix%/%topic%/"
	Tags      []string
	Notes     string

	// HTTPUser/HTTPPassword are the Tasmota WebPassword credentials.
	// HTTPPassword is plaintext in memory only; the store encrypts it at rest.
	HTTPUser     string
	HTTPPassword string

	// OTAExcluded keeps the device out of every OTA batch (TasmoAdmin "isUpdatable" = 0).
	OTAExcluded bool

	// MQTT is the detected MQTT capability. MQTT is an optional accelerator:
	// every feature works over HTTP alone.
	MQTT MQTTCapability

	// MQTTHost/MQTTPort are the broker the device itself is configured for
	// (Status 6 MqttHost/MqttPort), empty/0 when unknown or MQTT is off. They
	// carry no credentials. The UI compares them with the tasmofleet broker.
	MQTTHost string
	MQTTPort int

	Identity Identity // last detected hardware/firmware identity

	// Net is what the device itself reported about its network (Status 5 and 11), written by the
	// prober only. The zero value means "never read".
	Net NetFacts

	CreatedAt time.Time
	UpdatedAt time.Time
}

// HasMAC reports whether mac (lower case, colon separated) is one of the
// device's MACs. An empty mac never matches.
func (d Device) HasMAC(mac string) bool {
	if mac == "" {
		return false
	}
	if d.MAC == mac {
		return true
	}
	for _, m := range d.MACs {
		if m == mac {
			return true
		}
	}
	return false
}

// HasCredentials reports whether the device is protected by a web password.
// A web user without a password means no authentication (Tasmota's user is
// always "admin"), so only a non-empty password counts.
func (d Device) HasCredentials() bool { return d.HTTPPassword != "" }

// MQTTCapability says whether tasmofleet can rely on MQTT for a device.
type MQTTCapability string

// MQTT capabilities. Only MQTTOurs makes health use MQTT; all others are
// HTTP-only devices and are never shown as degraded for missing MQTT.
const (
	MQTTUnknown     MQTTCapability = ""      // not probed yet
	MQTTNone        MQTTCapability = "none"  // Status 6 missing/MqttHost empty or SetOption3 0
	MQTTOtherBroker MQTTCapability = "other" // MqttHost set, but not the broker tasmofleet uses (or none configured)
	MQTTOurs        MQTTCapability = "ours"  // messages from this device seen on our broker
)

// ChipFamily is the coarse hardware class that decides which firmware
// binaries a device can run. It is the primary key of the firmware safety
// guard: an artifact is only ever offered to a device of the same family.
type ChipFamily string

// Known chip families. ChipUnknown is never upgraded.
const (
	ChipUnknown  ChipFamily = ""
	ChipESP8266  ChipFamily = "esp8266" // ESP8266EX and ESP8285
	ChipESP32    ChipFamily = "esp32"   // classic ESP32 (dual core: D0WD..., also the solo1 build)
	ChipESP32S2  ChipFamily = "esp32s2"
	ChipESP32S3  ChipFamily = "esp32s3"
	ChipESP32C2  ChipFamily = "esp32c2"
	ChipESP32C3  ChipFamily = "esp32c3"
	ChipESP32C5  ChipFamily = "esp32c5"
	ChipESP32C6  ChipFamily = "esp32c6"
	ChipESP32C61 ChipFamily = "esp32c61"
	ChipESP32P4  ChipFamily = "esp32p4"
)

// IsESP32 reports whether the family belongs to the ESP32 line (any chip
// whose firmware is a plain .bin app image with an extended header).
func (c ChipFamily) IsESP32() bool {
	return c != ChipUnknown && c != ChipESP8266
}

// Identity is what fwselect.Detect derives from Status 2 and Status 4. It is
// persisted per device and refreshed before every OTA job.
type Identity struct {
	Family     ChipFamily
	Hardware   string // raw StatusFWR.Hardware, e.g. "ESP8285N08", "ESP32-D0WD-V3"
	SingleCore bool   // ESP32 solo1 hardware (needs tasmota32solo1 builds)

	Version  string // semantic version, e.g. "16.0.0"
	Image    string // raw image tag from Version, e.g. "release-tasmota", "tasmota32-sensors"
	Variant  string // normalised variant: "tasmota", "minimal", "sensors", "lite", "ir", "display", "knx", "zbbridge", "zigbee", "4M", "bluetooth", "lvgl", "nspanel", "webcam", "zbbrdgpro", ...
	Language string // two-letter language variant ("DE"), empty for English

	FlashSizeKB        int // Status 4 FlashSize
	ProgramFlashSizeKB int // Status 4 ProgramFlashSize
	FreeKB             int // Status 4 Free (OTA space available)

	DetectedAt time.Time
}

// Known reports whether the identity is complete enough to select firmware.
func (i Identity) Known() bool {
	return i.Family != ChipUnknown && i.Version != "" && i.Variant != ""
}

// NetFacts are the network settings a probe read from the device: Status 5 (hostname, IP
// address, gateway) and Status 11 (the access point it is connected to). They change only when
// someone reconfigures the device, so they are stored on it and re-read by every probe. Empty
// fields are unknown (a wired board has no SSID).
type NetFacts struct {
	Hostname, IP, Gateway string
	SSID, BSSID           string
	Channel               int
	ReadAt                time.Time // zero: never read
}
