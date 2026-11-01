// Package transport decides how tasmofleet reaches a device: over its HTTP API or over MQTT.
//
// The decision (Choose, choose.go) is a pure function of Facts: what tasmofleet reaches, what the
// device reaches, and what the person asked for. Facts are gathered elsewhere (internal/app
// transportfacts.go); the router (WP-A2) acts on the Paths Choose returns. Spec §5.x
// "Transport and path choice".
package transport

import (
	"context"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// HTTPStateKind is whether tasmofleet's HTTP API calls to the device work.
type HTTPStateKind string

// HTTP states. Unknown (never tried, or the last success is older than 15 minutes with no
// failure on record) is treated like ok: "try".
const (
	HTTPUnknown HTTPStateKind = ""
	HTTPOK      HTTPStateKind = "ok"
	HTTPFailing HTTPStateKind = "failing"
)

// HTTPFailKind says why HTTP fails; it only shapes the sentence shown to the person.
type HTTPFailKind string

// Failure kinds.
const (
	HTTPFailUnreachable HTTPFailKind = "unreachable"      // no answer or refused connection
	HTTPFailOutside     HTTPFailKind = "outside_networks" // address outside DEVICE_NETWORKS
	HTTPFailAPIRefused  HTTPFailKind = "api_refused"      // SetOption128 on
	HTTPFailAuth        HTTPFailKind = "auth_failed"      // web password refused
)

// HTTPState is what tasmofleet knows about its HTTP path to the device.
type HTTPState struct {
	State HTTPStateKind
	Kind  HTTPFailKind // only with HTTPFailing
	Since time.Time    // only with HTTPFailing
}

// MQTTFacts is what makes the MQTT path usable. Every field is a fact read elsewhere; Choose only
// combines them.
type MQTTFacts struct {
	BusConnected bool // the connection to our broker is up
	Ours         bool // Device.MQTT == model.MQTTOurs
	Topic        string
	// TopicExclusive: no device in the inventory (this one's GroupTopic included) listens on this
	// topic, as Topic or GroupTopic, compared raw and as expanded command topics. Computed by the
	// facts source; the shape of the topic is checked by Choose.
	TopicExclusive bool
	// OthersGroupUnread is how many other devices with an MQTT topic have no read GroupTopic (empty or
	// never stamped): one of them could be in a group this device's topic belongs to.
	OthersGroupUnread int
	// FullTopic is the device's FullTopic ("" = Tasmota default); OurFullTopic is MQTT_FULLTOPIC.
	FullTopic    string
	OurFullTopic string
	// GroupTopic is the device's GroupTopic as last read, GroupTopicReadAt when ("" or zero: never).
	// LastProbeAt is the time of the last Status 1 read; a GroupTopic stamped before it did not
	// survive that read and counts as unknown (review #264 L2).
	GroupTopic       string
	GroupTopicReadAt time.Time
	LastProbeAt      time.Time
	LWTOnline        bool // last will says Online
}

// Password is the state of the device's MqttPassword, which only transfers need (FileDownload,
// FileUpload). Commands never use it.
type Password struct {
	Mode  model.MQTTPasswordMode
	BadAt time.Time // the device answered Error 1 (zero: never or cleared)
	// Unusable, when set, is the sentence why the password cannot be used although a mode is set
	// (broker mode without a broker password); it takes precedence over Mode and BadAt.
	Unusable string
}

// Delivery request for an update.
type Request struct {
	Delivery        model.Delivery // pull (default) or push where possible
	AllowMQTTUpload bool           // the per-transaction "experimental" checkbox
	// MQTTUploadFit is the verdict of fwselect.MQTTUploadFits for the image the update would send.
	MQTTUploadFit MQTTFit
	// HTTPPush is the HTTP push decision the OTA engine computes today for a device HTTP can reach:
	// PushHTTP, PushSafeboot or DownloadHTTP (the per-device fallback), with HTTPPushNote the
	// engine's sentence for the fallback. The zero value reads as DownloadHTTP.
	HTTPPush     Path
	HTTPPushNote string
}

// MQTTFit is fwselect.MQTTUploadFits' answer, carried as data so Choose stays pure.
type MQTTFit struct {
	OK     bool
	NeedKB int
	RoomKB int
	Reason string
}

// Facts is everything Choose looks at.
type Facts struct {
	Now      time.Time
	HTTP     HTTPState
	MQTT     MQTTFacts
	Password Password
	// FWReach is what the device reaches of the firmware server; FirmwareBaseURL says
	// FIRMWARE_BASE_URL is configured. Without it no firmware download works.
	FWReach         model.FWReach
	FirmwareBaseURL bool
	Identity        model.Identity
	Request         Request
}

// FactsSource gathers the Facts for one device. The Request part is left zero; the caller of an
// update fills it.
type FactsSource interface {
	Facts(ctx context.Context, id model.DeviceID) (Facts, error)
}
