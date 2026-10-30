package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/inventory"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/mqttcmd"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// MQTT-first discovery (spec 11.3a): devices that publish no
// tasmota/discovery config are found from their own topics. LWT, STATE and
// INFO say that a topic exists and whether it is online; they carry no MAC.
// For such a topic discovery asks once for Status 5 (the smallest reply with
// the MAC) and treats the answer like a discovery config.

// Limits of the topic side (security doc D12).
const (
	maxTopics    = 1024             // topics tracked; the oldest sighting is dropped
	askGap       = time.Second      // between two Status 5 queries on the whole bus
	askEvery     = 10 * time.Minute // between two queries of one unresolved topic
	reprobeEvery = 10 * time.Minute // between two first probes of one device without identity
	// askGrace is how long a topic must be online before it is asked: Tasmota
	// publishes its discovery config a few seconds after the LWT, and a device
	// that announces itself needs no question.
	askGrace = 30 * time.Second
	// askReplyWindow is how long after the question a STATUS5 reply is taken (D13).
	askReplyWindow = 30 * time.Second
	// claimMaxAge bounds how old a kept outside-networks claim may be when the
	// device networks grow (#45). An older one is not proposed (its address may
	// be stale); the topic is asked again with Status 5 instead.
	claimMaxAge = time.Hour
)

// topicState is what MQTT told about one %topic%.
type topicState struct {
	lwt    string // "Online", "Offline" or "" (none seen)
	live   bool   // a non-retained message arrived since the last Offline
	seen   time.Time
	up     time.Time // since when it is online (zero while offline)
	mac    string    // resolved MAC (discovery config or Status 5), "" if unknown
	known  bool      // an inventory device has this topic
	asked  time.Time
	host   string // INFO2 / Status 5 hints, for the list only
	ip     string
	reason string // why it is not a proposal, for the list
	// claim is the device claim of this topic that was refused only because
	// its address lies outside the device networks. It is evaluated again
	// when DEVICE_NETWORKS changes (#45): Tasmota repeats neither its
	// discovery config nor a Status 5 answer on its own.
	claim   *candidate
	claimAt time.Time // when claim was seen
}

func (s *topicState) online() bool { return s.live || s.lwt == "Online" }

// Unidentified is a topic seen on MQTT that is not (yet) a proposal or an
// inventory device: no MAC known, or its address cannot be used.
type Unidentified struct {
	Topic    string
	LWT      string
	Online   bool
	SeenAt   time.Time
	Hostname string
	IP       string
	Reason   string
}

// Unidentified lists the topics seen on MQTT that discovery could not turn
// into a proposal, sorted by topic.
func (m *Manager) Unidentified() []Unidentified {
	m.tmu.Lock()
	defer m.tmu.Unlock()
	var out []Unidentified
	for t, s := range m.topics {
		if s.known || (s.mac != "" && s.reason == "") {
			continue
		}
		out = append(out, Unidentified{Topic: t, LWT: s.lwt, Online: s.online(), SeenAt: s.seen, Hostname: s.host, IP: s.ip, Reason: s.reason})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Topic < out[j].Topic })
	return out
}

// topicLWT returns the last LWT of topic ("" if none or the topic is unknown).
func (m *Manager) topicLWT(topic string) string {
	if topic == "" {
		return ""
	}
	m.tmu.Lock()
	defer m.tmu.Unlock()
	if s := m.topics[topic]; s != nil {
		return s.lwt
	}
	return ""
}

// stateFor returns the entry of topic, creating it (and dropping the oldest
// one when the table is full). The caller holds tmu.
func (m *Manager) stateFor(topic string, now time.Time) *topicState {
	if s := m.topics[topic]; s != nil {
		return s
	}
	if len(m.topics) >= maxTopics {
		oldest := ""
		for t, s := range m.topics {
			if oldest == "" || s.seen.Before(m.topics[oldest].seen) {
				oldest = t
			}
		}
		delete(m.topics, oldest)
	}
	s := &topicState{seen: now}
	m.topics[topic] = s
	return s
}

// noteDiscoveryTopic records that a discovery config accounts for topic.
func (m *Manager) noteDiscoveryTopic(c candidate) {
	if c.topic == "" {
		return
	}
	m.tmu.Lock()
	defer m.tmu.Unlock()
	if !m.attached {
		return
	}
	s := m.stateFor(c.topic, m.clk.Now())
	s.mac, s.ip = c.mac, c.ip.String()
}

// dropClaim forgets the kept claim and reason of topic: a newer claim of the
// topic is being evaluated.
func (m *Manager) dropClaim(topic string) {
	if topic == "" {
		return
	}
	m.tmu.Lock()
	defer m.tmu.Unlock()
	if s := m.topics[topic]; s != nil {
		s.reason, s.claim = "", nil
	}
}

// setReason records why a resolved topic did not become a proposal.
func (m *Manager) setReason(topic, reason string) {
	if topic == "" {
		return
	}
	m.tmu.Lock()
	defer m.tmu.Unlock()
	if s := m.topics[topic]; s != nil {
		s.reason = reason
	}
}

// keepOutsideClaim records that the claim c of topic was refused because its
// address lies outside the device networks, and keeps it for recheckNetworks.
func (m *Manager) keepOutsideClaim(topic, reason string, c candidate, at time.Time) {
	if topic == "" {
		return
	}
	m.tmu.Lock()
	defer m.tmu.Unlock()
	if s := m.topics[topic]; s != nil {
		s.reason, s.claim, s.claimAt = reason, &c, at
	}
}

// keptClaim is a claim taken out of the topic table for re-evaluation.
type keptClaim struct {
	c      candidate
	at     time.Time
	reason string
}

// recheckNetworks evaluates the kept outside-networks claims again when the
// device networks changed since the last call (the settings page changes them
// live). It sends no MQTT command: a claim now inside the networks is treated
// like a new sighting of it (a proposal, or the inventory rules for a known
// MAC), exactly as if the message had arrived now. A claim older than
// claimMaxAge is not used; its topic is asked again with Status 5 (askDue,
// with its rate limits). A newer stored sighting of the same MAC wins over a
// kept claim.
func (m *Manager) recheckNetworks(ctx context.Context) {
	nets := m.networks()
	if slices.Equal(nets, m.seenNets) {
		return
	}
	m.seenNets = nets
	now := m.clk.Now()
	var claims []keptClaim
	m.tmu.Lock()
	for _, s := range m.topics {
		if s.claim == nil || !tasmota.IPAllowed(s.claim.ip, nets) {
			continue
		}
		if now.Sub(s.claimAt) > claimMaxAge {
			s.claim, s.mac, s.asked = nil, "", time.Time{}
			s.reason = "its address was outside the device networks too long ago; asking it again (Status 5)"
			continue
		}
		claims = append(claims, keptClaim{c: *s.claim, at: s.claimAt, reason: s.reason})
		s.claim, s.reason = nil, ""
	}
	m.tmu.Unlock()
	if len(claims) == 0 {
		return
	}
	m.log.Info("discovery: device networks changed, evaluating devices seen outside them again", "devices", len(claims))
	for _, k := range claims {
		prev, ok, err := m.storedRow(ctx, k.c.mac)
		if err == nil && ok && prev.SeenAt.After(k.at) {
			continue // a newer sighting of this MAC knows better
		}
		out := ""
		if err == nil {
			out, err = m.consider(ctx, k.c)
		}
		if err != nil && ctx.Err() == nil {
			m.log.Warn("discovery: could not evaluate device", "mac", k.c.mac, "err", err)
		}
		if err != nil || (out == outIgnored && !tasmota.IPAllowed(k.c.ip, m.networks())) {
			m.keepOutsideClaim(k.c.topic, k.reason, k.c, k.at) // evaluated again at the next change
		}
	}
}

// handleTopic evaluates one LWT, STATE, INFO or STATUS5 message.
func (m *Manager) handleTopic(ctx context.Context, msg mqttbus.Message) {
	now := m.clk.Now()
	m.tmu.Lock()
	if !m.attached {
		m.tmu.Unlock()
		return
	}
	s := m.stateFor(msg.Topic, now)
	s.seen = now
	var answer []byte
	switch msg.Kind {
	case mqttbus.KindLWT:
		switch p := strings.TrimSpace(string(msg.Payload)); {
		case strings.EqualFold(p, "online"):
			s.lwt = "Online"
		case strings.EqualFold(p, "offline"):
			s.lwt, s.live = "Offline", false
		}
	case mqttbus.KindState, mqttbus.KindInfo:
		if !msg.Retained {
			s.live = true // a retained telemetry message says nothing about now
		}
		if msg.Suffix == "INFO2" {
			var p struct {
				Info2 struct{ Hostname, IPAddress string }
			}
			if json.Unmarshal(msg.Payload, &p) == nil {
				s.host, s.ip = cleanName(p.Info2.Hostname, "000000000000"), strings.TrimSpace(p.Info2.IPAddress)
			}
		}
	case mqttbus.KindStatus:
		if !s.asked.IsZero() && now.Sub(s.asked) <= askReplyWindow && s.mac == "" && !s.known {
			answer = msg.Payload // only a timely reply to discovery's own question (D13)
		}
		if !msg.Retained {
			s.live = true
		}
	case mqttbus.KindSensor, mqttbus.KindResult, mqttbus.KindPower, mqttbus.KindDiscovery, mqttbus.KindUpgrade, mqttbus.KindFile:
		// not queued for the topic side (HandleMQTT, handleMessage)
	}
	online := s.online()
	switch {
	case !online:
		s.up = time.Time{}
	case s.up.IsZero():
		s.up = now
	}
	m.tmu.Unlock()

	devs, err := m.o.Inventory.List(ctx)
	if err != nil {
		m.log.Warn("discovery: list devices", "err", err)
		return
	}
	for _, d := range devs {
		if d.Topic == msg.Topic {
			m.tmu.Lock()
			s.known, s.claim = true, nil // a kept claim never applies to a stored device later
			m.tmu.Unlock()
			if online {
				m.reprobe(ctx, d, now)
			}
			return
		}
	}
	m.tmu.Lock()
	s.known = false
	switch {
	case s.mac != "":
	case !online:
		s.reason = "offline: it cannot be asked for its MAC until it comes online"
	case s.asked.IsZero():
		s.reason = "no tasmota/discovery config; asking it for its MAC (Status 5)"
	default:
		s.reason = "no answer to Status 5 yet"
	}
	m.tmu.Unlock()
	if answer != nil {
		m.handleStatus5(ctx, msg.Topic, answer)
	}
}

// handleStatus5 turns the Status 5 answer of an asked topic into a candidate.
func (m *Manager) handleStatus5(ctx context.Context, topic string, payload []byte) {
	c, err := parseStatus5(topic, payload)
	if err != nil {
		m.log.Debug("discovery: Status 5 answer ignored", "topic", topic, "err", err)
		m.setReason(topic, "its Status 5 answer was not usable")
		return
	}
	m.tmu.Lock()
	if s := m.topics[topic]; s != nil {
		s.mac, s.ip, s.host, s.reason, s.claim = c.mac, c.ip.String(), c.name, "", nil
	}
	m.tmu.Unlock()
	out, err := m.consider(ctx, c)
	switch {
	case err != nil && ctx.Err() == nil:
		m.log.Warn("discovery: could not evaluate device", "mac", c.mac, "err", err)
	case out == outIgnored && !tasmota.IPAllowed(c.ip, m.networks()):
		m.keepOutsideClaim(topic, fmt.Sprintf("its address %s is outside the device networks", c.ip), c, m.clk.Now())
	}
}

// parseStatus5 decodes a stat/<topic>/STATUS5 reply. The Wi-Fi MAC is the
// main one (it is the MAC of tasmota/discovery too); the Ethernet MAC is an
// additional one. The address is the Wi-Fi one, or the Ethernet one when the
// device has no Wi-Fi address.
func parseStatus5(topic string, payload []byte) (candidate, error) {
	if len(payload) > maxDiscoveryPayload {
		return candidate{}, badClaim("payload of %d bytes", len(payload))
	}
	var p struct {
		StatusNET struct {
			Hostname, IPAddress, Mac string
			Ethernet                 struct{ IPAddress, Mac string }
		}
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return candidate{}, badClaim("not JSON")
	}
	n := p.StatusNET
	mac, err := inventory.NormaliseMAC(n.Mac)
	if err != nil || mac == "" || mac == "00:00:00:00:00:00" {
		return candidate{}, badClaim("no valid MAC")
	}
	ip := usableIP(n.IPAddress)
	if !ip.IsValid() {
		ip = usableIP(n.Ethernet.IPAddress)
	}
	if !ip.IsValid() {
		return candidate{}, badClaim("no valid IP address")
	}
	c := candidate{via: ViaMQTT, mac: mac, ip: ip, topic: cleanTopic(topic)}
	if eth, err := inventory.NormaliseMAC(n.Ethernet.Mac); err == nil && eth != "" && eth != mac && eth != "00:00:00:00:00:00" {
		c.macs = []string{eth}
	}
	c.name = cleanName(firstNonEmpty(n.Hostname, topic), mac)
	c.host = cleanHostname(n.Hostname)
	return c, nil
}

func usableIP(s string) netip.Addr {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil || a.IsUnspecified() {
		return netip.Addr{}
	}
	return a.Unmap()
}

// askWait bounds how long one question may hold the loop.
const askWait = 3 * time.Second

// notSent says the channel refused the question or could not publish it. Any answer (also
// {"Command":"Unknown"} from a minimal image that does not know Status 5) or a silence after the
// publish means the question went out and is not asked again before askEvery.
func notSent(err error) bool {
	if errors.Is(err, tasmota.ErrTimeout) {
		return false
	}
	return errors.Is(err, mqttcmd.ErrRefused) || errors.Is(err, tasmota.ErrUnreachable) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// askDue sends at most one Status 5 to the oldest unresolved online topic,
// keeping askGap on the bus and askEvery per topic (D12). Never Status 0.
func (m *Manager) askDue(ctx context.Context) {
	now := m.clk.Now()
	m.tmu.Lock()
	bus := m.bus
	m.tmu.Unlock()
	// Connected is asked outside tmu (a bus may take its own lock), and while the
	// broker is away nothing is picked: a query could not be sent anyway.
	if bus == nil || !bus.Connected() {
		return
	}
	m.tmu.Lock()
	if m.bus != bus || !m.attached || (!m.lastAsk.IsZero() && now.Sub(m.lastAsk) < askGap) {
		m.tmu.Unlock()
		return
	}
	topic := ""
	var pick *topicState
	for t, s := range m.topics {
		if s.known || s.mac != "" || !s.online() || now.Sub(s.up) < m.askGrace || (!s.asked.IsZero() && now.Sub(s.asked) < askEvery) {
			continue
		}
		if pick == nil || s.asked.Before(pick.asked) || (s.asked.Equal(pick.asked) && t < topic) {
			topic, pick = t, s
		}
	}
	if pick == nil {
		m.tmu.Unlock()
		return
	}
	pick.asked, m.lastAsk = now, now
	m.tmu.Unlock()

	why := "on the broker without a tasmota/discovery config; Status 5 reports its MAC"
	if m.o.Asker == nil {
		m.tmu.Lock()
		if s := m.topics[topic]; s == pick {
			s.asked = time.Time{}
		}
		m.tmu.Unlock()
		return
	}
	// The channel waits for the reply (the manager sees the same STATUS5 on its own subscription);
	// the wait is capped so a silent topic does not hold up discovery's loop for the full reply wait.
	actx, cancel := context.WithTimeout(ctx, askWait)
	_, err := m.o.Asker.Do(actx, model.Device{Topic: topic}, "Status 5")
	cancel()
	if notSent(err) {
		m.tmu.Lock()
		if s := m.topics[topic]; s == pick {
			s.asked = time.Time{} // not sent: try again at the next gap
		}
		m.tmu.Unlock()
		m.log.Warn("discovery: Status 5 not sent", "topic", topic, "err", err)
		return
	}
	m.log.Info("discovery: asked a topic for its MAC", "topic", topic, "command", "Status 5", "reason", why)
	m.audit(ctx, "discovery", "discovery.identify", "mqtt:"+topic, map[string]any{"command": "Status 5", "reason": why}, nil)
}

// markProbed records that the first probe of id was just started (accept),
// so a message before it stored the identity does not start a second one.
func (m *Manager) markProbed(id model.DeviceID) {
	m.tmu.Lock()
	m.reprobed[id] = m.clk.Now()
	m.tmu.Unlock()
}

// reprobe starts the first probe again for an inventory device that came
// online without a detected identity (added while offline), at most once per
// reprobeEvery per device (spec 11.3a "Filled in later").
//
// The trigger is an MQTT message anyone can publish, and the probe sends the
// stored credentials. So the stored address is first asked without
// credentials (Verifier, Status 5):
//
//   - another MAC than the stored one: another device; refused and audited
//     (discovery.identify_refused);
//   - no proof (error: a password-protected device refuses the probe, a timeout,
//     an impostor) for a device with a stored password: the password is not
//     sent on this trigger (acceptMove rule, audit row 14); audited as
//     discovery.identify_unverified and retried on the normal cadence. Without
//     a stored password nothing secret goes out, so the probe runs;
//   - no stored MAC: nothing to compare. The probe goes to the address the
//     admin bound together with its password (add form, import, edit; the
//     health poller sends them there too). It binds no MAC (an automatic
//     Prober.Refresh never binds one, only a person's refresh does, D8); the audit
//     discovery.reprobe records the MAC that answered.
func (m *Manager) reprobe(ctx context.Context, d model.Device, now time.Time) {
	if m.o.OnAccepted == nil || !d.Identity.DetectedAt.IsZero() {
		return
	}
	m.tmu.Lock()
	if t, ok := m.reprobed[d.ID]; ok && now.Sub(t) < reprobeEvery {
		m.tmu.Unlock()
		return
	}
	if len(m.reprobed) >= maxTopics {
		for id, t := range m.reprobed {
			if now.Sub(t) >= reprobeEvery {
				delete(m.reprobed, id)
			}
		}
	}
	m.reprobed[d.ID] = now
	m.tmu.Unlock()
	target := fmt.Sprintf("device:%d", d.ID)
	answered, verr := "", errors.New("no verifier configured")
	if m.o.Verifier != nil {
		var mac string
		if mac, verr = m.o.Verifier.VerifyMAC(ctx, d.Address, portOf(d)); verr == nil {
			var nerr error
			if answered, nerr = inventory.NormaliseMAC(mac); nerr != nil || answered == "" {
				answered, verr = "", errors.New("unusable MAC in the answer")
			}
		}
	}
	switch {
	case d.MAC != "" && verr == nil && !d.HasMAC(answered):
		m.log.Warn("discovery: stored address answers with another MAC, identity probe not started", "device", d.ID, "address", d.Address)
		m.audit(ctx, "discovery", "discovery.identify_refused", target,
			map[string]any{"address": d.Address, "reason": "the stored address answers with another MAC"}, nil)
		return
	case d.MAC != "" && verr != nil && d.HasCredentials():
		m.log.Info("discovery: MAC of the stored address could not be verified, credentials withheld", "device", d.ID, "address", d.Address)
		m.audit(ctx, "discovery", "discovery.identify_unverified", target,
			map[string]any{"address": d.Address, "reason": "MAC could not be verified, credentials withheld; retried in 10 min"}, nil)
		return
	}
	det := map[string]any{"address": d.Address, "answered_mac": answered, "mac_bound": false, "stored_mac": d.MAC, "credentials": d.HasCredentials()}
	m.audit(ctx, "discovery", "discovery.reprobe", target, det, nil)
	m.log.Info("discovery: device came online without identity, probing it", "device", d.ID, "topic", d.Topic)
	m.o.OnAccepted(d.ID)
}
