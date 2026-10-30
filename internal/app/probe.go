package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/audit"
	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/inventory"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/settings"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// identityStore is the part of the store the prober needs.
type identityStore interface {
	GetDevice(ctx context.Context, id model.DeviceID) (model.Device, error)
	UpdateProbe(ctx context.Context, id model.DeviceID, g store.ProbeGuard, r store.ProbeResult) error
	UpdateNet(ctx context.Context, id model.DeviceID, g store.ProbeGuard, n model.NetFacts) error
	UpdateProbeName(ctx context.Context, id model.DeviceID, g store.ProbeGuard, deviceName, nameFrom string) error
	UpdateIdentityFromMQTT(ctx context.Context, id model.DeviceID, g store.ProbeGuard, topic string, i model.Identity) error
	UpdateGroupTopicFromMQTT(ctx context.Context, id model.DeviceID, g store.ProbeGuard, topic, groupTopic string) error
}

// mqttAsker asks a device on our broker one Status N over MQTT and returns its answer
// (mqttIdentity.askStatus). errMQTTUnavailable: no connection, or the device is not on our broker.
type mqttAsker interface {
	askStatus(ctx context.Context, d model.Device, n int, actor string) (json.RawMessage, error)
}

// brokerMatcher tells whether a device's MQTT settings point at the broker
// tasmofleet uses (mqttbus.BrokerMatcher).
type brokerMatcher interface {
	Same(host string, port int) bool
}

// Prober learns a device's identity from individual Status queries (never
// Status 0, spec section 5), one at a time with a pause in between.
type Prober struct {
	Client  tasmota.Client
	Devices identityStore
	// Gap is the pause between two commands to the same device (default 200 ms).
	Gap time.Duration
	Now func() time.Time
	// Broker is the tasmofleet broker; nil when none is configured.
	Broker brokerMatcher
	// ReadNetLater queues the background read of a device's network facts (ReadNet) after a
	// refresh; nil in processes without one (the CLI): the server's reader catches up.
	ReadNetLater func(model.DeviceID)
	// Hub, if set, hears of every stored probe result (Refresh, ReadNet) as a device event, so open
	// device tables reload and show the new firmware, module and network facts. Nil in the CLI.
	Hub *events.Hub
	// MQTT, if set, refreshes a device on our broker that HTTP cannot reach (#91). Nil in the CLI.
	MQTT mqttAsker
	// Audit records refused refreshes, MAC bindings and MQTT questions. Nil: not recorded (tests).
	Audit audit.Recorder
	// busyWait is how long an MQTT refresh waits before asking a busy device again (default 2 s).
	busyWait time.Duration
}

// announce tells open pages that the stored facts of a device changed.
func (p *Prober) announce(id model.DeviceID) {
	if p.Hub != nil {
		p.Hub.Publish(events.Event{Topic: events.TopicDevice, At: p.now(), Payload: model.Device{ID: id}})
	}
}

const unknownReply = `{"Command":"Unknown"}`

type probed struct {
	identity model.Identity
	mac      string
	ethMAC   string // Ethernet MAC of boards that have one, else ""
	topic    string // resolved: what the device publishes under, never a %06X placeholder
	hostname string
	devName  string // Tasmota DeviceName
	mqtt     model.MQTTCapability
	mqttHost string
	mqttPort int
	minimal  bool
	net      model.NetFacts // what Status 5 reported (Wi-Fi facts not included)
	// otaURL is the OtaUrl of Status 1 with its credentials removed; readOTAURL tells whether
	// Status 1 gave a usable answer (else the stored value is kept).
	otaURL     string
	readOTAURL bool
	// groupTopic is the GroupTopic of the same Status 1 answer; readGroupTopic tells whether it had one.
	groupTopic     string
	readGroupTopic bool
	relayNames     []string // FriendlyName array of plain Status
}

func (p *Prober) pause(ctx context.Context) error {
	gap := p.Gap
	if gap == 0 {
		gap = 200 * time.Millisecond
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(gap):
		return nil
	}
}

func (p *Prober) cmd(ctx context.Context, t tasmota.Target, c string) (json.RawMessage, error) {
	if err := p.pause(ctx); err != nil {
		return nil, err
	}
	return p.Client.Command(ctx, t, c)
}

// probe queries the device. A device on minimal firmware only answers a few
// commands, so MAC, topic and MQTT capability stay empty there.
//
// Status 5 goes first, without credentials: it carries the MAC, and verify (if set) checks it
// before the stored credentials go anywhere (#65). A device that answers it without them has no
// web password, and the whole probe then runs without credentials. Only a device that asks for
// a password (401) gets Status 5 again with them, so a refused device receives them in that
// one command at most. Minimal firmware knows neither Status 5 nor Status 2: Status 2 still
// decides minimal (spec 5) and goes out (with the credentials if the device asked for them)
// before verify runs, which then sees a minimal device without a MAC; nothing follows a
// refusal.
func (p *Prober) probe(ctx context.Context, d model.Device, verify func(probed) error) (probed, error) {
	t := tasmota.TargetFor(d)
	var out probed
	in := fwselect.Inputs{}

	no5 := false
	raw5, err := p.Client.Command(ctx, tasmota.TargetFor(model.Device{ID: d.ID, Address: d.Address, Port: d.Port}), "Status 5")
	switch {
	case errors.Is(err, tasmota.ErrAuth) && t.HasCredentials():
		raw5, err = p.cmd(ctx, t, "Status 5")
	case err == nil || errors.Is(err, tasmota.ErrUnknownCommand):
		// Answered without a password: the device needs none, so none is sent.
		t = tasmota.TargetFor(model.Device{ID: d.ID, Address: d.Address, Port: d.Port})
	}
	switch {
	case err == nil:
		if out.net, err = parseStatus5(raw5, &out); err != nil {
			return out, err
		}
	case errors.Is(err, tasmota.ErrUnknownCommand):
		no5 = true
	default:
		return out, err
	}
	check := func() error {
		if verify == nil {
			return nil
		}
		return verify(out)
	}
	if !no5 {
		if err := check(); err != nil {
			return out, err
		}
	}
	raw2, err := p.cmd(ctx, t, "Status 2")
	switch {
	case err == nil:
		in.Status2 = raw2
	case errors.Is(err, tasmota.ErrUnknownCommand):
		in.Status2, out.minimal = []byte(unknownReply), true
	default:
		return out, fmt.Errorf("status 2: %w", err)
	}
	if no5 {
		if err := check(); err != nil {
			return out, err
		}
	}
	if !out.minimal {
		raw4, err := p.cmd(ctx, t, "Status 4")
		if err != nil {
			return out, fmt.Errorf("status 4: %w", err)
		}
		in.Status4 = raw4
	} else {
		up, err := p.cmd(ctx, t, "Upgrade")
		if err != nil {
			return out, fmt.Errorf("upgrade probe: %w", err)
		}
		if probe, err := tasmota.ParseUpgradeProbe(up); err == nil {
			in.Upgrade = &fwselect.UpgradeProbe{Version: probe.Version}
		}
	}
	if err := p.pause(ctx); err != nil {
		return out, err
	}
	root, err := p.Client.RootPage(ctx, t)
	if err != nil {
		if out.minimal {
			return out, fmt.Errorf("main page: %w", err)
		}
		// The language only comes from the page; the identity is usable without it.
	} else {
		in.Root = &fwselect.RootInfo{Version: root.Version, Image: root.Image, Lang: root.Lang, LangKnown: root.LangKnown, Minimal: root.Minimal}
	}
	if out.identity, err = fwselect.Detect(in); err != nil {
		return out, fmt.Errorf("identify: %w", err)
	}
	out.identity.DetectedAt = p.now()
	if out.minimal {
		return out, nil
	}

	// Status 6: no MqttHost, or SetOption3 off, means the device does not
	// talk MQTT at all. Otherwise it is "ours" when it is configured for the
	// tasmofleet broker and "other" for any other target (or when tasmofleet
	// has no broker).
	out.mqtt = model.MQTTNone
	if raw, err := p.cmd(ctx, t, "Status 6"); err == nil {
		var s struct {
			StatusMQT struct {
				MqttHost string
				MqttPort int
			}
		}
		if json.Unmarshal(raw, &s) == nil && s.StatusMQT.MqttHost != "" {
			out.mqttHost, out.mqttPort = s.StatusMQT.MqttHost, s.StatusMQT.MqttPort
			out.mqtt = model.MQTTOtherBroker
			if p.Broker != nil && p.Broker.Same(out.mqttHost, out.mqttPort) {
				out.mqtt = model.MQTTOurs
			}
		}
	} else if !errors.Is(err, tasmota.ErrUnknownCommand) {
		return out, fmt.Errorf("status 6: %w", err)
	}
	if out.mqtt != model.MQTTNone && p.mqttSwitchedOff(ctx, t) {
		out.mqtt = model.MQTTNone
	}
	// Status 1 (read-only, once per refresh, never the OtaUrl command) for the OTA URL. Best
	// effort: a failed or unusable answer keeps the stored value.
	if raw, err := p.cmd(ctx, t, "Status 1"); err == nil {
		out.otaURL, out.readOTAURL = parseOtaURL(raw)
		out.groupTopic, out.readGroupTopic = parseGroupTopic(raw)
	}
	// Plain "Status" (not "Status 0") is the Status object only: DeviceName and the topic the
	// device actually uses. The "Topic" command would return the setting with its placeholders
	// ("tasmota_%06X") unresolved, and nothing on the bus ever matches that.
	if raw, err := p.cmd(ctx, t, "Status"); err == nil {
		var s struct {
			Status struct {
				DeviceName, Topic string
				FriendlyName      []string
			}
		}
		if json.Unmarshal(raw, &s) == nil {
			// Both come from the device: only a usable topic is taken, a name is cleaned.
			if inventory.ValidTopic(s.Status.Topic) {
				out.topic = s.Status.Topic
			}
			out.devName = inventory.CleanName(s.Status.DeviceName)
			out.relayNames = relayNames(s.Status.FriendlyName)
		}
	}
	return out, nil
}

// maxRelayNames bounds the stored FriendlyName array (Tasmota has at most 8 relays).
const maxRelayNames = 8

// relayNames cleans the FriendlyName array of Status; nil when there is no usable name.
func relayNames(in []string) []string {
	var out []string
	for i, n := range in {
		if i == maxRelayNames {
			break
		}
		out = append(out, inventory.CleanName(n))
	}
	for _, n := range out {
		if n != "" {
			return out
		}
	}
	return nil
}

// parseOtaURL reads the OtaUrl of a Status 1 answer and removes anything that can hold a secret:
// user:password, the query (tokens, signatures) and the fragment. ok is false when the answer has
// no OtaUrl key or the value cannot be cleaned safely: a value that does not parse, or one with an
// '@' that is not userinfo (a password containing '#', '?' or '/' mis-parses into host, query or
// path) is dropped, never stored raw.
func parseOtaURL(raw json.RawMessage) (clean string, ok bool) {
	var s struct {
		StatusPRM *struct{ OtaUrl *string }
	}
	if json.Unmarshal(raw, &s) != nil || s.StatusPRM == nil || s.StatusPRM.OtaUrl == nil {
		return "", false
	}
	v := strings.TrimSpace(*s.StatusPRM.OtaUrl)
	if v == "" {
		return "", true
	}
	clean, _, ok = settings.SplitURLCredentials(v)
	return clean, ok
}

// parseGroupTopic reads the GroupTopic of a Status 1 answer (the same one that carries the OtaUrl,
// so it costs no read of its own). ok is false when the answer has no such key; an empty value is
// a read value ("not set"). The topic is a MQTT topic, not a secret.
func parseGroupTopic(raw json.RawMessage) (topic string, ok bool) {
	var s struct {
		StatusPRM *struct{ GroupTopic *string }
	}
	if json.Unmarshal(raw, &s) != nil || s.StatusPRM == nil || s.StatusPRM.GroupTopic == nil {
		return "", false
	}
	topic = strings.TrimSpace(*s.StatusPRM.GroupTopic)
	if !validGroupTopic(topic) {
		return "", false
	}
	return topic, true
}

// validGroupTopic says whether a group topic read from a device or from the bus is shown: at most
// Tasmota's TOPSZ (151) bytes, no MQTT wildcard, NUL or control character. Any client on the broker
// can publish an INFO1, and the value is rendered on every device list load, so anything else is
// refused and the stored value stays.
func validGroupTopic(t string) bool {
	if len(t) > 151 {
		return false
	}
	for _, r := range t {
		if r == '+' || r == '#' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// mqttSwitchedOff reads SetOption3 (MQTT on/off). Any failure counts as "on":
// the host is configured, and a missing answer must not hide it.
func (p *Prober) mqttSwitchedOff(ctx context.Context, t tasmota.Target) bool {
	raw, err := p.cmd(ctx, t, "SetOption3")
	if err != nil {
		return false
	}
	var s struct{ SetOption3 string }
	return json.Unmarshal(raw, &s) == nil && strings.EqualFold(s.SetOption3, "OFF")
}

func (p *Prober) status5(ctx context.Context, t tasmota.Target, out *probed) (model.NetFacts, error) {
	raw, err := p.cmd(ctx, t, "Status 5")
	if err != nil {
		return model.NetFacts{}, fmt.Errorf("status 5: %w", err)
	}
	return parseStatus5(raw, out)
}

// parseStatus5 reads MAC, Ethernet MAC and hostname into out and returns the network facts
// (address, gateway, subnet mask and DNS servers of the active interface).
func parseStatus5(raw json.RawMessage, out *probed) (model.NetFacts, error) {
	var s struct {
		StatusNET struct {
			Hostname                           string
			IPAddress                          string
			Gateway                            string
			Subnetmask, DNSServer1, DNSServer2 string
			Mac                                string
			Ethernet                           struct{ Hostname, IPAddress, Gateway, Subnetmask, DNSServer1, DNSServer2, Mac string }
		}
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return model.NetFacts{}, fmt.Errorf("status 5: %w", tasmota.ErrBadResponse)
	}
	out.mac = strings.ToLower(s.StatusNET.Mac)
	if eth := strings.ToLower(s.StatusNET.Ethernet.Mac); eth != "" && eth != out.mac && eth != "00:00:00:00:00:00" {
		out.ethMAC = eth
	}
	out.hostname = s.StatusNET.Hostname
	n := model.NetFacts{Hostname: s.StatusNET.Hostname, IP: s.StatusNET.IPAddress, Gateway: s.StatusNET.Gateway}
	n.Subnet, n.DNS = netMask(s.StatusNET.Subnetmask), dnsServers(s.StatusNET.DNSServer1, s.StatusNET.DNSServer2)
	// An Ethernet build whose Wi-Fi station has no address (0.0.0.0) is wired: the top-level
	// values are the idle Wi-Fi ones, the Ethernet object has the real ones.
	if eth := s.StatusNET.Ethernet; unsetIP(n.IP) && !unsetIP(eth.IPAddress) {
		n.Wired, n.IP, n.Gateway = true, eth.IPAddress, eth.Gateway
		n.Subnet, n.DNS = netMask(eth.Subnetmask), dnsServers(eth.DNSServer1, eth.DNSServer2)
		if eth.Hostname != "" {
			n.Hostname = eth.Hostname
		}
	}
	return n, nil
}

// netMask is a subnet mask as reported, "" when unset.
func netMask(m string) string {
	if unsetIP(m) {
		return ""
	}
	return m
}

// dnsServers joins the DNS servers a device has set ("a, b"); an unset one (0.0.0.0) is left out.
func dnsServers(servers ...string) string {
	var set []string
	for _, s := range servers {
		if !unsetIP(s) {
			set = append(set, s)
		}
	}
	return strings.Join(set, ", ")
}

// withStoredWifi completes the facts of a Status 5 read with the Wi-Fi facts stored earlier
// (Status 11 is read only in the background); a wired board is on no access point.
func (p *Prober) withStoredWifi(stored, n model.NetFacts) model.NetFacts {
	if !n.Wired {
		n.SSID, n.BSSID, n.Channel = stored.SSID, stored.BSSID, stored.Channel
	}
	n.ReadAt = p.now()
	return n
}

// unsetIP is an address Tasmota reports for an interface that has none.
func unsetIP(ip string) bool { return ip == "" || ip == "0.0.0.0" }

// status11 reads the access point the device is connected to. It is best effort: a failed or
// empty reply (a wired board, a flaky answer) keeps the Wi-Fi facts already in net.
func (p *Prober) status11(ctx context.Context, t tasmota.Target, net *model.NetFacts) {
	raw, err := p.cmd(ctx, t, "Status 11")
	if err != nil {
		return
	}
	var s struct {
		StatusSTS struct {
			Wifi struct {
				SSId, BSSId string
				Channel     int
			}
		}
	}
	if json.Unmarshal(raw, &s) != nil || s.StatusSTS.Wifi.SSId == "" {
		return
	}
	w := s.StatusSTS.Wifi
	net.SSID, net.BSSID, net.Channel = w.SSId, w.BSSId, w.Channel
}

// ReadNet reads a stored device's network facts (Status 5, then best effort Status 11, one at a
// time, never Status 0) and stores them. It runs in the background (netFactsReader), never as
// part of first contact or a refresh. Minimal firmware answers neither command: nothing is read.
func (p *Prober) ReadNet(ctx context.Context, id model.DeviceID) error {
	d, err := p.Devices.GetDevice(ctx, id)
	if err != nil {
		return err
	}
	if d.Identity.Variant == "minimal" {
		return nil
	}
	t := tasmota.TargetFor(d)
	var out probed
	net, err := p.status5(ctx, t, &out)
	if err != nil {
		return err
	}
	net = p.withStoredWifi(d.Net, net) // kept when Status 11 gives none
	if !net.Wired {
		p.status11(ctx, t, &net)
	}
	if err := p.Devices.UpdateNet(ctx, id, store.ProbeGuard{Address: d.Address, Port: d.Port, MAC: d.MAC}, net); err != nil {
		return err
	}
	p.announce(id)
	return nil
}

func (p *Prober) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Describe fills MAC, topic, name, MQTT capability and identity of d from the
// device. Nothing is stored. Fields the user typed are kept.
func (p *Prober) Describe(ctx context.Context, d model.Device) (model.Device, error) {
	pr, err := p.probe(ctx, d, nil)
	if err != nil {
		return d, err
	}
	d.Identity = pr.identity
	if d.MAC == "" {
		d.MAC = pr.mac
		if pr.ethMAC != "" && pr.mac != "" && len(d.MACs) == 0 {
			d.MACs = []string{pr.mac, pr.ethMAC} // Wi-Fi plus Ethernet, set once at creation
		}
	}
	if d.Topic == "" {
		d.Topic = pr.topic
	}
	if d.MQTT == model.MQTTUnknown {
		d.MQTT = pr.mqtt
	}
	d.MQTTHost, d.MQTTPort = pr.mqttHost, pr.mqttPort
	if pr.readOTAURL {
		d.OTAURL, d.OTAURLReadAt = pr.otaURL, p.now()
	}
	if pr.readGroupTopic {
		d.GroupTopic, d.GroupTopicReadAt = pr.groupTopic, p.now()
	}
	if pr.relayNames != nil {
		d.RelayNames = pr.relayNames
	}
	if d.Name == "" {
		d.Name = inventory.CleanName(firstNonEmpty(pr.devName, pr.hostname, pr.topic, d.Address))
	}
	return d, nil
}

// Refresh re-probes a stored device and stores what it reports (#91): identity, MQTT facts, the
// network facts of Status 5, the DeviceName and the topic (the stored name and topic follow them
// unless the user chose their own, store.ProbeResult). The device must answer with a MAC of the
// stored set, else the refresh is refused (RefreshRefusedError, audited) before anything else
// is sent and nothing is written (#65). A device without a MAC gets the one it reported bound
// when an admin refreshes it (actor not empty); an automatic refresh (actor "", discovery) never
// binds (discovery D8). A device on our broker that HTTP cannot reach is refreshed over MQTT
// (refreshMQTT). On minimal firmware the stored flash values are kept and no MAC can be read.
func (p *Prober) Refresh(ctx context.Context, id model.DeviceID, actor string) error {
	d, err := p.Devices.GetDevice(ctx, id)
	if err != nil {
		return err
	}
	pr, err := p.probe(ctx, d, func(pr probed) error { return p.checkMAC(ctx, d, pr, actor, "http") })
	if err != nil {
		if !p.overMQTT(d, err) {
			return err
		}
		merr := p.refreshMQTT(ctx, d, actor)
		var refused *RefreshRefusedError
		switch {
		case merr == nil:
			return nil
		case errors.As(merr, &refused):
			return merr
		}
		// The HTTP error is the one the page explains; the MQTT one goes along for the log.
		return fmt.Errorf("%w (over MQTT: %s)", err, merr.Error())
	}
	res := store.ProbeResult{Identity: pr.identity, SetMQTT: !pr.minimal, MQTT: pr.mqtt, MQTTHost: pr.mqttHost, MQTTPort: pr.mqttPort}
	if pr.minimal {
		res.Identity.FlashSizeKB, res.Identity.ProgramFlashSizeKB, res.Identity.FreeKB = d.Identity.FlashSizeKB, d.Identity.ProgramFlashSizeKB, d.Identity.FreeKB
	} else {
		res.DeviceName, res.DeviceTopic, res.NameFrom = pr.devName, pr.topic, fallbackName(d, pr)
		res.SetOTAURL, res.OTAURL, res.RelayNames = pr.readOTAURL, pr.otaURL, pr.relayNames
		res.SetGroupTopic, res.GroupTopic = pr.readGroupTopic, pr.groupTopic
		res.SetNet, res.Net = true, p.withStoredWifi(d.Net, pr.net)
		if d.MAC == "" && actor != "" && bindable(pr.mac) {
			res.BindMACs = []string{pr.mac}
			if bindable(pr.ethMAC) {
				res.BindMACs = append(res.BindMACs, pr.ethMAC)
			}
		}
	}
	// Only probe-owned fields are written, and only if the device is still
	// the one that was probed (a slow probe must not undo an admin edit or
	// land on a device that was moved, rebound or deleted meanwhile).
	err = p.Devices.UpdateProbe(ctx, id, store.ProbeGuard{Address: d.Address, Port: d.Port, MAC: d.MAC}, res)
	var topicKept error
	if err == nil && res.DeviceTopic != "" {
		topicKept = p.auditTopic(ctx, d, res.DeviceTopic, actor)
	}
	switch {
	case errors.Is(err, store.ErrMACInUse):
		return p.refuse(ctx, d, actor, "http", pr.mac, refusedTaken)
	case err != nil:
		return err
	}
	if len(res.BindMACs) > 0 {
		p.record(ctx, actor, "device.mac_bound", d.ID, "ok", map[string]any{"mac": pr.mac, "ethernet_mac": pr.ethMAC, "address": d.Address, "via": "http"})
	}
	p.announce(id)
	if p.ReadNetLater != nil && !pr.minimal {
		p.ReadNetLater(id) // the Wi-Fi facts of Status 11 fill in when they arrive
	}
	return topicKept
}

// TopicKeptError is returned by a refresh that succeeded except for one thing: the device reports
// a topic another device in the inventory already has, so the stored topic stays and MQTT
// messages of this device cannot be matched (#111). Callers that only care about success ignore it.
type TopicKeptError struct {
	Reported string // the topic the device reports
	Stored   string // the topic that stays
}

func (e *TopicKeptError) Error() string {
	return "topic not changed: the device reports " + e.Reported + ", which another device already has"
}

// Notice is the sentence for the device page.
func (e *TopicKeptError) Notice() string {
	return "The device reports the MQTT topic " + e.Reported + ", which another device in the inventory already has, " +
		"so the stored topic " + e.Stored + " was kept and MQTT will not match this device until the topics differ."
}

// auditTopic compares the topic stored by UpdateProbe with the one before the refresh and audits
// a change (device.topic_changed) or a topic that could not follow (device.topic_kept, returned
// as TopicKeptError). A failed read of the device only skips the audit.
func (p *Prober) auditTopic(ctx context.Context, before model.Device, reported, actor string) error {
	after, err := p.Devices.GetDevice(ctx, before.ID)
	if err != nil {
		slog.WarnContext(ctx, "refresh: reload device for the topic audit", "device", before.ID, "err", err)
		return nil
	}
	switch {
	case after.Topic != reported:
		p.record(ctx, actor, "device.topic_kept", before.ID, "refused", map[string]any{"stored": after.Topic, "reported": reported})
		return &TopicKeptError{Reported: reported, Stored: after.Topic}
	case before.Topic != after.Topic:
		p.record(ctx, actor, "device.topic_changed", before.ID, "ok", map[string]any{"old": before.Topic, "new": after.Topic, "via": "http"})
	}
	return nil
}

// bindable reports whether mac is a real MAC (normal form, not all zero) that may be bound.
func bindable(mac string) bool {
	n, err := inventory.NormaliseMAC(mac)
	return err == nil && n != "" && n == mac && n != "00:00:00:00:00:00"
}

// tasmotaDefaultName is the DeviceName of a Tasmota whose name was never set (its FriendlyName1).
const tasmotaDefaultName = "Tasmota"

// fallbackName returns the stored name when it is one tasmofleet filled in itself before it
// recorded the DeviceName: the hostname (how devices were named before 0.12.3), the topic or the
// address (Describe's fallbacks). Such a name follows the DeviceName (#91, refining #52), unless
// the DeviceName is Tasmota's default, which would give many devices the same name. The default
// itself as a stored name is one tasmofleet filled in (Describe, #104) and follows too.
func fallbackName(d model.Device, pr probed) string {
	if pr.devName == "" || pr.devName == tasmotaDefaultName || d.Name == "" {
		return ""
	}
	if d.Name == tasmotaDefaultName {
		return d.Name // Describe named a device after its default DeviceName (#104)
	}
	for _, s := range []string{pr.hostname, d.Net.Hostname, pr.topic, d.Topic, d.Address} {
		if s != "" && d.Name == inventory.CleanName(s) {
			return d.Name
		}
	}
	return ""
}

// Reasons of a refused refresh.
const (
	refusedOther = "another MAC"
	refusedTaken = "the MAC of another device"
	// refusedUnverified: minimal firmware reports no MAC, and the device has one stored.
	refusedUnverified = "no MAC (minimal firmware)"
)

// RefreshRefusedError says that the device that answered is not the stored one (#65). Nothing was
// written. The web layer shows Refusal() on the device page.
type RefreshRefusedError struct {
	Reported string // the MAC the device reported, lower case ("" when it reported none)
	Reason   string // refusedOther or refusedTaken
}

func (e *RefreshRefusedError) Error() string {
	return "identity refresh refused: the device reports " + e.Reason + " (" + e.Reported + ")"
}

// Refusal is the sentence for the device page.
func (e *RefreshRefusedError) Refusal() string {
	mac := "MAC " + e.Reported
	if e.Reported == "" {
		mac = "no MAC"
	}
	if e.Reason == refusedUnverified {
		return "The device runs minimal firmware, which does not report its MAC, so TasmoFleet cannot check that it is this device. " +
			"The stored identity was kept. Refresh again once the update has finished."
	}
	if e.Reason == refusedTaken {
		return "The device at this address reports " + mac + ", which belongs to another device in the inventory. Nothing was changed."
	}
	return "The device at this address reports " + mac + ", not the MAC stored for this device, so it is another device. " +
		"Nothing was changed and nothing more was sent to it. If the device was replaced, use Replace device; " +
		"if the address now belongs to another device, correct it on the Edit page."
}

// checkMAC refuses a device that reports none of the stored MACs. Nothing to compare: a device
// stored without a MAC. Minimal firmware (no Status 5) cannot prove a stored MAC, so its answers
// are not taken as the stored device's either: refused, the stored identity stays (#96 review).
func (p *Prober) checkMAC(ctx context.Context, d model.Device, pr probed, actor, via string) error {
	switch {
	case d.MAC == "" || d.HasMAC(pr.mac) || d.HasMAC(pr.ethMAC):
		return nil
	case pr.minimal:
		return p.refuse(ctx, d, actor, via, "", refusedUnverified)
	}
	return p.refuse(ctx, d, actor, via, pr.mac, refusedOther)
}

// refuse audits a refused refresh and returns its error.
func (p *Prober) refuse(ctx context.Context, d model.Device, actor, via, reported, reason string) error {
	slog.WarnContext(ctx, "identity refresh refused", "device", d.ID, "address", d.Address, "reason", reason, "via", via)
	p.record(ctx, actor, "device.refresh_refused", d.ID, "refused", map[string]any{
		"address": d.Address, "stored_mac": d.MAC, "reported_mac": reported, "reason": reason, "via": via,
	})
	return &RefreshRefusedError{Reported: reported, Reason: reason}
}

// record writes one audit row. Details are addresses, MACs and reasons, never credentials.
func (p *Prober) record(ctx context.Context, actor, action string, id model.DeviceID, result string, detail map[string]any) {
	if p.Audit == nil {
		return
	}
	if actor == "" {
		actor = "system"
	}
	b, err := json.Marshal(detail)
	if err != nil {
		b = []byte("{}") // strings only; cannot happen
	}
	p.Audit.Record(ctx, model.AuditEvent{Actor: actor, Action: action, Target: "device:" + strconv.FormatInt(int64(id), 10), Detail: string(b), Result: result})
}

// overMQTT reports whether a refresh whose HTTP probe failed with err can go over MQTT: the
// device is on our broker and HTTP could not be used at all (not a refusal or a bad answer).
func (p *Prober) overMQTT(d model.Device, err error) bool {
	if p.MQTT == nil || d.MQTT != model.MQTTOurs || d.Topic == "" {
		return false
	}
	for _, e := range []error{tasmota.ErrUnreachable, tasmota.ErrTimeout, tasmota.ErrHTTPAPIRefused, tasmota.ErrForbiddenAddr} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// refreshMQTT refreshes a device on our broker that HTTP cannot reach (#91, #79): Status 5 for
// MAC, hostname and IP, then Status 2 for firmware and hardware, one at a time (askStatus keeps
// the bus limits and audits each question). The MAC rule is the same as over HTTP. MQTT is the
// weaker source (anyone with broker access can answer), so (#87): the identity is written only
// for a device no HTTP probe identified (store.UpdateIdentityFromMQTT), never with DetectedAt,
// never as minimal or safeboot; no MAC is bound; the topic is left alone. The DeviceName is read
// with the plain Status (never Status 0) and stored by store.UpdateProbeName under the same follow
// rule as over HTTP (#125); that call never writes the identity.
func (p *Prober) refreshMQTT(ctx context.Context, d model.Device, actor string) error {
	raw5, err := p.MQTT.askStatus(ctx, d, 5, actor)
	if err != nil {
		return err
	}
	var pr probed
	n, err := parseStatus5(raw5, &pr)
	if err != nil {
		return err
	}
	if err := p.checkMAC(ctx, d, pr, actor, "mqtt"); err != nil {
		return err
	}
	raw2, err := p.MQTT.askStatus(ctx, d, 2, actor)
	if errors.Is(err, errMQTTBusy) {
		// The background reader asked the device meanwhile: wait for its turn once (#96 review).
		wait := p.busyWait
		if wait == 0 {
			wait = 2 * mqttIDGap
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		raw2, err = p.MQTT.askStatus(ctx, d, 2, actor)
	}
	if err != nil {
		return err
	}
	ident, err := fwselect.Detect(fwselect.Inputs{Status2: raw2})
	if err != nil {
		return fmt.Errorf("identify over MQTT: %w", err)
	}
	if ident.Variant == "minimal" || ident.Variant == "safeboot" {
		return fmt.Errorf("identify over MQTT: the answer claims a %s image, which cannot answer Status 2: %w", ident.Variant, tasmota.ErrBadResponse)
	}
	ident.Language, ident.LanguageKnown = keepLanguage(d.Identity, ident)
	ident.DetectedAt = time.Time{}
	g := store.ProbeGuard{Address: d.Address, Port: d.Port, MAC: d.MAC}
	if d.Identity.DetectedAt.IsZero() {
		// ErrConflict: an HTTP probe identified it meanwhile, and that identity wins.
		if err := p.Devices.UpdateIdentityFromMQTT(ctx, d.ID, g, d.Topic, ident); err != nil && !errors.Is(err, store.ErrConflict) {
			return err
		}
	}
	if err := p.Devices.UpdateNet(ctx, d.ID, g, p.withStoredWifi(d.Net, n)); err != nil {
		return err
	}
	p.refreshGroupTopicMQTT(ctx, d, g, actor)
	p.refreshNameMQTT(ctx, d, g, pr)
	slog.InfoContext(ctx, "identity refreshed over MQTT", "device", d.ID, "topic", d.Topic, "http_identity_kept", !d.Identity.DetectedAt.IsZero())
	p.announce(d.ID)
	return nil
}

// refreshGroupTopicMQTT reads Status 1 and stores its GroupTopic and the time it was read. A transfer over MQTT
// needs a fresh GroupTopic (transport.Choose), and a device HTTP cannot reach never gets one from a probe.
// Best effort like the name: a device that does not answer leaves the refresh as it is.
func (p *Prober) refreshGroupTopicMQTT(ctx context.Context, d model.Device, g store.ProbeGuard, actor string) {
	raw, err := p.MQTT.askStatus(ctx, d, 1, actor)
	if err != nil {
		slog.DebugContext(ctx, "refresh over MQTT: GroupTopic not read", "device", d.ID, "err", err)
		return
	}
	gt, ok := parseGroupTopic(raw)
	if !ok {
		return
	}
	if err := p.Devices.UpdateGroupTopicFromMQTT(ctx, d.ID, g, d.Topic, gt); err != nil && !errors.Is(err, store.ErrConflict) {
		slog.WarnContext(ctx, "refresh over MQTT: store the GroupTopic", "device", d.ID, "err", err)
	}
}

// refreshNameMQTT reads the DeviceName with the plain Status (the Status object, never Status 0) and
// stores it under the rule of an HTTP refresh (#125). The name is a nicety: a device that does not
// answer, or answers something unusable, leaves the refresh as it is.
func (p *Prober) refreshNameMQTT(ctx context.Context, d model.Device, g store.ProbeGuard, pr probed) {
	raw, err := p.MQTT.askStatus(ctx, d, statusObject, "")
	if err != nil {
		slog.DebugContext(ctx, "refresh over MQTT: DeviceName not read", "device", d.ID, "err", err)
		return
	}
	var s struct {
		Status struct{ DeviceName string }
	}
	if json.Unmarshal(raw, &s) != nil {
		return
	}
	pr.devName, pr.topic = inventory.CleanName(s.Status.DeviceName), d.Topic
	if pr.devName == "" {
		return
	}
	if err := p.Devices.UpdateProbeName(ctx, d.ID, g, pr.devName, fallbackName(d, pr)); err != nil && !errors.Is(err, store.ErrConflict) {
		slog.WarnContext(ctx, "refresh over MQTT: store the DeviceName", "device", d.ID, "err", err)
	}
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// macVerifier implements inventory.Verifier: Status 5 without credentials,
// IP only, so a rogue host never receives a stored password.
type macVerifier struct{ client tasmota.Client }

func (v macVerifier) VerifyMAC(ctx context.Context, address string, port int) (string, error) {
	t := tasmota.TargetFor(model.Device{Address: address, Port: port}) // no user, no password
	raw, err := v.client.Command(ctx, t, "Status 5")
	if err != nil {
		return "", err
	}
	var s struct {
		StatusNET struct{ Mac string }
	}
	if err := json.Unmarshal(raw, &s); err != nil || s.StatusNET.Mac == "" {
		return "", tasmota.ErrBadResponse
	}
	return strings.ToLower(s.StatusNET.Mac), nil
}
