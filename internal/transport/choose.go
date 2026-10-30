package transport

import (
	"fmt"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// fwReachTTL is how long a "no" stays true (D11): the network may have changed since.
const fwReachTTL = 24 * time.Hour

// defaultFullTopic is what a device with an empty FullTopic uses.
const defaultFullTopic = "%prefix%/%topic%/"

// Paths is the answer of Choose: one path per action and the sentence for each.
type Paths struct {
	// Commands are reads (Status N, identity refresh): they may go over MQTT with an unknown own
	// GroupTopic. Writes are commands that change the device (settings, Make compliant, non-read
	// console commands, backlog): they follow the strict rule of the transfers.
	Commands, Writes, Backup, Restore, Update Path
	// Reasons has one entry per action, in the order Commands, Writes, Backup, Restore, Update, each with a
	// non-empty Text.
	Reasons []Reason
}

// Reason returns the sentence for an action.
func (p Paths) Reason(a Action) string {
	for _, r := range p.Reasons {
		if r.Action == a {
			return r.Text
		}
	}
	return ""
}

// Choose decides, from facts alone, how each action reaches the device. It is pure: no clock, no
// I/O (Facts.Now is the clock). The order is fixed (spec §5.x):
//
//   - Commands: HTTP unless HTTP is failing and MQTT is usable.
//   - Backup: HTTP unless failing; else MQTT FileDownload (usable, password set and not refused).
//   - Restore: HTTP WebGetConfig unless HTTP fails, FIRMWARE_BASE_URL is missing or the device is
//     known not to reach the firmware server; else MQTT FileUpload Type 2.
//   - Update, pull: download over HTTP, else download started over MQTT.
//   - Update, push where possible: today's HTTP decision while HTTP works; with HTTP failing the
//     upload over MQTT (only with the experimental checkbox, a fitting image and a usable
//     password), else the download over MQTT.
//
// HTTP comes first because it is the established path with synchronous replies; MQTT is what
// makes devices HTTP cannot reach manageable (#45).
func Choose(f Facts) Paths {
	var p Paths
	var t string
	p.Commands, t = chooseCommands(f)
	p.Reasons = append(p.Reasons, Reason{ActCommands, p.Commands, t})
	p.Writes, t = chooseWrites(f)
	p.Reasons = append(p.Reasons, Reason{ActWrites, p.Writes, t})
	p.Backup, t = chooseBackup(f)
	p.Reasons = append(p.Reasons, Reason{ActBackup, p.Backup, t})
	p.Restore, t = chooseRestore(f)
	p.Reasons = append(p.Reasons, Reason{ActRestore, p.Restore, t})
	p.Update, t = chooseUpdate(f)
	p.Reasons = append(p.Reasons, Reason{ActUpdate, p.Update, t})
	return p
}

func httpFailing(f Facts) bool { return f.HTTP.State == HTTPFailing }

// fwReach is the firmware-server reach with the 24 h expiry of "no" applied.
func fwReach(f Facts) model.FWReachState {
	// a "no" without a time cannot be dated, so it cannot be trusted either
	if f.FWReach.State == model.FWReachNo && (f.FWReach.At.IsZero() || f.Now.Sub(f.FWReach.At) > fwReachTTL) {
		return model.FWReachUnknown
	}
	return f.FWReach.State
}

func fwReachNo(f Facts) bool { return fwReach(f) == model.FWReachNo }

// fwBlocker says why the device cannot fetch from the firmware server, "" when it may.
func fwBlocker(f Facts) string {
	switch {
	case !f.FirmwareBaseURL:
		return "FIRMWARE_BASE_URL is not set, so the device has no firmware server address"
	case fwReachNo(f):
		s := "the device does not reach TasmoFleet's firmware server"
		if !f.FWReach.At.IsZero() {
			s += " (since " + f.FWReach.At.UTC().Format("2006-01-02 15:04 UTC") + ")"
		}
		return s
	}
	return ""
}

// fullTopic normalises a FullTopic for comparison: empty is Tasmota's default, a trailing '/' is
// not significant.
func fullTopic(s string) string {
	if s == "" {
		s = defaultFullTopic
	}
	return strings.TrimRight(s, "/")
}

// groupTopicKnown reports whether the stored GroupTopic can be trusted: set, stamped, and not
// older than the last Status 1 read (that read would have replaced it; an INFO1 or Status 1 that
// carried none leaves the old value behind, review #264 L2).
func groupTopicKnown(m MQTTFacts) bool {
	return m.GroupTopic != "" && !m.GroupTopicReadAt.IsZero() && !m.GroupTopicReadAt.Before(m.LastProbeAt)
}

// mqttBlocker says why the device is not usable over MQTT, "" when it is. strict adds the
// requirement that the GroupTopic is known: anything but a plain command must not go to a topic
// that might be shared, a transfer or an upgrade there would reach the wrong devices.
func mqttBlocker(f Facts, strict bool) string {
	m := f.MQTT
	switch {
	case !m.BusConnected:
		return "TasmoFleet is not connected to its MQTT broker"
	case !m.Ours:
		return "the device does not use TasmoFleet's MQTT broker"
	case m.Topic == "":
		return "the device has no MQTT topic"
	case !TopicShapeOK(m.Topic):
		return "the device's MQTT topic contains characters that are not allowed"
	case m.Topic == DefaultGroupTopic:
		return "the device's topic is the default group topic, which many devices listen on"
	case !m.TopicExclusive:
		return "another device listens on this topic (its Topic or GroupTopic)"
	case fullTopic(m.FullTopic) != fullTopic(m.OurFullTopic):
		return "the device's FullTopic differs from TasmoFleet's MQTT_FULLTOPIC"
	case m.GroupTopic != "" && m.GroupTopic == m.Topic: // even a stale value: it only ever adds a refusal
		return "the device's topic is also its GroupTopic, so a message would reach other devices"
	case strict && m.OthersGroupUnread > 0:
		return othersUnreadText(m.OthersGroupUnread)
	case strict && !groupTopicKnown(m):
		return "the device's GroupTopic has not been read recently, so TasmoFleet cannot rule out that its topic is shared; refresh the device's identity to read it"
	case !m.LWTOnline:
		return "the device is not online on MQTT"
	}
	return ""
}

// passwordBlocker says why a transfer over MQTT cannot carry the device's MqttPassword.
func passwordBlocker(f Facts, what string) string {
	switch {
	case f.Password.Unusable != "":
		return f.Password.Unusable
	case !f.Password.BadAt.IsZero():
		return "the device refused its MQTT password (since " + f.Password.BadAt.UTC().Format("2006-01-02 15:04 UTC") + "); enter the right one to " + what + " over MQTT"
	case f.Password.Mode == model.MQTTPasswordNone:
		return "enter the device's MQTT password to " + what + " over MQTT"
	case f.Password.Mode != model.MQTTPasswordBroker && f.Password.Mode != model.MQTTPasswordOwn:
		return "the device's MQTT password mode is not recognised"
	}
	return ""
}

// transferBlocker combines the MQTT and password requirements of a transfer.
func transferBlocker(f Facts, what string) string {
	if s := mqttBlocker(f, true); s != "" {
		return s
	}
	return passwordBlocker(f, what)
}

// noPath builds the sentence for None: what is wrong with each way.
func noPath(parts ...string) string {
	var out []string
	for _, s := range parts {
		if s != "" {
			out = append(out, s)
		}
	}
	return "no path: " + strings.Join(out, ", and ")
}

func chooseCommands(f Facts) (Path, string) {
	if !httpFailing(f) {
		return HTTP, "over the device's HTTP API"
	}
	if why := mqttBlocker(f, false); why != "" {
		return None, noPath(httpFailText(f.HTTP), "MQTT is not usable: "+why)
	}
	return MQTT, "over MQTT, because " + httpFailText(f.HTTP)
}

func chooseWrites(f Facts) (Path, string) {
	if !httpFailing(f) {
		return HTTP, "over the device's HTTP API"
	}
	if why := mqttBlocker(f, true); why != "" {
		return None, noPath(httpFailText(f.HTTP), "changing the device over MQTT is not possible: "+why)
	}
	return MQTT, "over MQTT, because " + httpFailText(f.HTTP)
}

func chooseBackup(f Facts) (Path, string) {
	if !httpFailing(f) {
		return HTTP, "over the device's HTTP API (/dl)"
	}
	if why := transferBlocker(f, "back up"); why != "" {
		return None, noPath(httpFailText(f.HTTP), "a backup over MQTT is not possible: "+why)
	}
	return MQTT, "over MQTT (FileDownload), because " + httpFailText(f.HTTP)
}

func chooseRestore(f Facts) (Path, string) {
	var httpWhy string
	switch {
	case httpFailing(f):
		httpWhy = httpFailText(f.HTTP)
	default:
		httpWhy = fwBlocker(f)
	}
	if httpWhy == "" {
		return HTTP, "the device fetches the configuration from TasmoFleet (WebGetConfig)"
	}
	if why := transferBlocker(f, "restore"); why != "" {
		return None, noPath(httpWhy, "a restore over MQTT is not possible: "+why)
	}
	return MQTT, "over MQTT (FileUpload), because " + httpWhy
}

func chooseUpdate(f Facts) (Path, string) {
	push := f.Request.Delivery == model.DeliveryPush
	if push && !httpFailing(f) {
		if hp := f.Request.HTTPPush; hp == PushHTTP || hp == PushSafeboot {
			return hp, hp.Describe()
		}
	}
	if !httpFailing(f) {
		if why := fwBlocker(f); why == "" {
			text := DownloadHTTP.Describe()
			if push && f.Request.HTTPPushNote != "" {
				text += "; no upload: " + f.Request.HTTPPushNote
			}
			return DownloadHTTP, text
		}
	}
	// HTTP is failing, or the device cannot fetch over HTTP's download.
	mqtt := mqttBlocker(f, true)
	if push && httpFailing(f) && mqtt == "" {
		if up := uploadBlocker(f); up == "" {
			return PushMQTT, PushMQTT.Describe() + ", because " + httpFailText(f.HTTP)
		} else if fw := fwBlocker(f); fw == "" {
			return DownloadMQTT, DownloadMQTT.Describe() + "; no upload over MQTT: " + up
		}
	}
	if mqtt == "" && fwBlocker(f) == "" {
		return DownloadMQTT, DownloadMQTT.Describe()
	}
	// Nothing works. Say what is wrong with each way.
	var httpWhy string
	if httpFailing(f) {
		httpWhy = httpFailText(f.HTTP)
	} else {
		httpWhy = fwBlocker(f)
	}
	parts := []string{httpWhy}
	if fw := fwBlocker(f); fw != "" && httpFailing(f) {
		parts = append(parts, fw)
	}
	if mqtt != "" {
		parts = append(parts, "MQTT is not usable: "+mqtt)
	}
	if push && httpFailing(f) {
		if up := uploadBlocker(f); up != "" {
			parts = append(parts, "no upload over MQTT: "+up)
		}
	}
	text := noPath(parts...)
	if !push || !f.Request.AllowMQTTUpload {
		// Would the experimental upload have worked? Then say how to ask for it.
		probe := f
		probe.Request.Delivery, probe.Request.AllowMQTTUpload = model.DeliveryPush, true
		if p, _ := chooseUpdate(probe); p == PushMQTT {
			text += "; choose Push where possible and allow upload over MQTT (experimental)"
		}
	}
	return None, text
}

// uploadBlocker says why the upload over MQTT (FileUpload Type 1) cannot be used, "" when it can.
// The caller has checked mqttBlocker already.
func uploadBlocker(f Facts) string {
	switch {
	case !f.Request.AllowMQTTUpload:
		return "upload over MQTT is not allowed for this update (experimental)"
	case !f.Request.MQTTUploadFit.OK:
		if f.Request.MQTTUploadFit.Reason == "" {
			return fmt.Sprintf("the image does not fit for an upload over MQTT (needs %d KB, %d KB room)", f.Request.MQTTUploadFit.NeedKB, f.Request.MQTTUploadFit.RoomKB)
		}
		return f.Request.MQTTUploadFit.Reason
	}
	return passwordBlocker(f, "upload firmware")
}

func othersUnreadText(n int) string {
	if n == 1 {
		return "1 other device's group topic has not been read yet; refresh its identity"
	}
	return fmt.Sprintf("%d other devices' group topics have not been read yet; refresh their identity", n)
}

// MQTTBlocker is mqttBlocker for the router: why the device is not usable over MQTT right now ("" when
// it is). strict is the rule of writes and transfers. The router uses it for calls whose path a job
// pinned, where Choose (which answers HTTP while HTTP works) is not the question.
func MQTTBlocker(f Facts, strict bool) string { return mqttBlocker(f, strict) }
