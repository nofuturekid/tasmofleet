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

	// MQTTPassword is the device's own MqttPassword, plaintext in memory only (the store seals it
	// at rest); only FileDownload and FileUpload need it. It is meaningful in MQTTPasswordModeOwn
	// only. Written by Store.SetMQTTPassword, never by UpdateDevice.
	MQTTPassword     string
	MQTTPasswordMode MQTTPasswordMode
	// MQTTPasswordBadAt is when the device answered "Error 1" to a transfer (zero: never or since
	// cleared); saving the password clears it.
	MQTTPasswordBadAt time.Time
	// UpLayout is the flash layout an earlier HTTP read of the device's /up page showed: "direct" (two
	// app partitions), "safeboot" (one app partition) or "" (never read). It cannot be read over MQTT,
	// and the upload of firmware over MQTT is only allowed for a proven "direct" layout.
	UpLayout string
	// UpLayoutAt is when UpLayout was read; zero means never. A layout is only proof while it is set.
	UpLayoutAt time.Time

	// FWReach is whether the device reaches tasmofleet's firmware server. Written by the
	// transport code only.
	FWReach FWReach

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

	// OTAURL is the OtaUrl the device would update from (Status 1), without any credentials in it;
	// "" when never read or unset. Written by the prober only.
	OTAURL string
	// OTAURLReadAt is when OTAURL was read; zero for a value stored before the stamps existed
	// (no age is shown). Written with OTAURL by the prober only.
	OTAURLReadAt time.Time
	// GroupTopic is the GroupTopic setting of the device, as the plain topic ("tasmotas"), from
	// Status 1 or the INFO1 boot announcement; "" when never read or unset. GroupTopicReadAt is
	// when it was read (zero: never). Written by the prober and the MQTT identity reader only.
	GroupTopic       string
	GroupTopicReadAt time.Time
	// RelayNames are the FriendlyNames of the device (plain Status), one per relay, as reported
	// (Tasmota's defaults "Tasmota", "Tasmota2" included). Nil when never read. Written by the
	// prober only.
	RelayNames []string

	// DeviceName is the Tasmota DeviceName the device reported at the last probe ("" when never
	// read), written by the prober only. Name follows it while Name is still the DeviceName
	// recorded before; a Name set by hand differs from it (#125).
	DeviceName string
	// KeptDeviceName is the DeviceName the admin answered "Keep" to: while the device still
	// reports exactly this, the page does not offer it again. Written by that action only.
	KeptDeviceName string

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

// MQTTPasswordMode says where the MqttPassword for FileDownload/FileUpload comes from.
type MQTTPasswordMode string

// Password modes. We never assume the fleet convention: the default is not set.
const (
	MQTTPasswordNone   MQTTPasswordMode = ""       // not set
	MQTTPasswordBroker MQTTPasswordMode = "broker" // tasmofleet's MQTT_PASSWORD, resolved at send time
	MQTTPasswordOwn    MQTTPasswordMode = "own"    // the device's own, stored sealed
)

// FWReachState says whether a device reaches tasmofleet's firmware server.
type FWReachState string

// Reach states. Unknown means "try".
const (
	FWReachUnknown FWReachState = ""
	FWReachYes     FWReachState = "yes"
	FWReachNo      FWReachState = "no"
)

// FWReach is what is known about a device reaching the firmware server, and since when.
// Detail is a short English note for the UI ("" for yes and unknown).
type FWReach struct {
	State  FWReachState
	At     time.Time
	Detail string
}

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

	// LanguageKnown: an HTTP probe read the language build, so Language ("" = English) is a fact.
	// False for identities stored before MQTT transport existed and for identities read over MQTT.
	LanguageKnown bool

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
	// Subnet is the subnet mask and DNS the set DNS servers ("a, b") of the active interface,
	// "" when the device reports none (0.0.0.0).
	Subnet, DNS string
	SSID, BSSID string
	Channel     int
	// Wired is true when the device is on Ethernet: its Wi-Fi station has no address and Status 5
	// reports an Ethernet object with one. Hostname, IP and Gateway are then the Ethernet ones.
	Wired  bool
	ReadAt time.Time // zero: never read
}
