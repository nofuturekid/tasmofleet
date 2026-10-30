package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/nofuturekid/tasmofleet/internal/netaddr"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/audit"
	"github.com/nofuturekid/tasmofleet/internal/clock"
	"github.com/nofuturekid/tasmofleet/internal/inventory"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// Limits. They bound what an untrusted publisher or scan can cost.
const (
	// Caps per class: proposals for unknown devices cannot crowd out the
	// address-change and conflict proposals about devices we manage.
	maxNewProposals     = 500
	maxRelatedProposals = 500
	proposalTTL         = 30 * 24 * time.Hour
	pruneEvery          = time.Hour
	maxQueuedMessages   = 4096
	maxQueuedBytes      = 8 << 20
	// minMessageGap is the shortest time between two evaluations of one MAC
	// from MQTT. Evaluating a moved device probes its new address, so a
	// publisher repeating one message must not turn into a probe stream.
	minMessageGap = 15 * time.Second
	maxGapEntries = 8192
)

// Inventory is the part of the inventory the manager uses. UpsertExisting
// must never create a device; UpdateIf must write only if the stored record
// still equals expected (*inventory.Manager implements both).
type Inventory interface {
	inventory.Service
	UpsertExisting(ctx context.Context, actor string, id model.DeviceID, d model.Device) (model.DeviceID, bool, error)
	UpdateIf(ctx context.Context, actor string, expected, next model.Device) error
}

// Resolver resolves a stored hostname (*net.Resolver implements it). It is
// used only to compare a device's stored hostname with the IP it was seen at;
// nothing connects to the result.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// resolveTimeout bounds one hostname lookup.
const resolveTimeout = 2 * time.Second

// Options configure the Manager.
type Options struct {
	Inventory Inventory // required
	Store     store.DiscoveryStore
	Audit     audit.Recorder // required
	// Client probes devices during a scan; it must refuse addresses outside
	// the device networks (tasmota.NewHTTPClient does). Without it StartScan
	// fails.
	Client tasmota.Client
	// Verifier, if set, is asked at Accept time which MAC answers at the
	// proposed address of a new device; an answer with another MAC refuses
	// the accept (stale proposal). A device that cannot answer is accepted.
	Verifier inventory.Verifier
	Networks []netip.Prefix // DEVICE_NETWORKS
	// Resolver compares stored hostnames with discovered IPs (default net.DefaultResolver).
	Resolver Resolver
	// NetworkSet, if set, replaces Networks and can change while the manager runs.
	NetworkSet *tasmota.NetworkSet
	// Ports are probed on every scanned address (default 80).
	Ports []int
	Clock clock.Clock // default clock.Real
	// Concurrency is the number of probes in flight (default 16);
	// ProbeTimeout is the limit of one request (default 2 s).
	Concurrency  int
	ProbeTimeout time.Duration
	Logger       *slog.Logger
	// OnDropped, if set, is called when a proposal is not stored because the
	// per-kind limit is reached (metric tasmofleet_discovery_proposals_dropped_total).
	OnDropped func(kind string)
	// Asker sends the Status 5 questions of MQTT-first discovery (mqttcmd.Channel): one command at a
	// time per topic, a gap between publishes on the whole bus, the same refusals as every other
	// command. Without it discovery listens but asks nothing.
	Asker StatusAsker
	// OnAccepted, if set, is called with the ID of every device created by an accept. The
	// proposal carries no identity, so the caller starts the first probe here. It must not block.
	OnAccepted func(id model.DeviceID)
}

// StatusAsker is the command channel as discovery uses it.
type StatusAsker interface {
	Do(ctx context.Context, d model.Device, cmd string) (json.RawMessage, error)
}

// Manager implements Service and consumes MQTT discovery messages.
type Manager struct {
	o   Options
	clk clock.Clock
	log *slog.Logger

	// cmu serialises the evaluation of candidates (MQTT and scan) and Accept,
	// so two sightings cannot race into duplicate decisions and a proposal
	// cannot change between the check and the write of an Accept.
	cmu sync.Mutex

	qmu     sync.Mutex
	pending map[string]mqttbus.Message
	qbytes  int
	order   []string
	wake    chan struct{}

	gapMu sync.Mutex
	last  map[string]time.Time

	smu      sync.Mutex
	base     context.Context
	scans    map[ScanID]*scan
	scanList []ScanID

	// tmu guards the topic side of MQTT discovery (mqttfirst.go).
	tmu      sync.Mutex
	bus      mqttbus.Bus
	attached bool
	topics   map[string]*topicState
	lastAsk  time.Time
	askGrace time.Duration // askGrace; tests shorten it
	reprobed map[model.DeviceID]time.Time
	// seenNets are the device networks recheckNetworks last saw (Run goroutine only).
	seenNets []netip.Prefix
}

var _ Service = (*Manager)(nil)

// New returns the discovery manager. Call Run to start it.
func New(o Options) (*Manager, error) {
	if o.Inventory == nil || o.Store == nil || o.Audit == nil {
		return nil, errors.New("discovery: Inventory, Store and Audit are required")
	}
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 16
	}
	if o.ProbeTimeout <= 0 {
		o.ProbeTimeout = 2 * time.Second
	}
	if len(o.Ports) == 0 {
		o.Ports = []int{80}
	}
	if len(o.Ports) > 8 {
		return nil, errors.New("discovery: at most 8 scan ports")
	}
	for _, p := range o.Ports {
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("discovery: scan port %d", p)
		}
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Resolver == nil {
		o.Resolver = net.DefaultResolver
	}
	return &Manager{
		o: o, clk: o.Clock, log: o.Logger,
		pending: map[string]mqttbus.Message{}, wake: make(chan struct{}, 1),
		last: map[string]time.Time{}, scans: map[ScanID]*scan{},
		topics: map[string]*topicState{}, reprobed: map[model.DeviceID]time.Time{}, askGrace: askGrace,
	}, nil
}

// Run processes queued MQTT messages and expires old proposals until ctx is
// done. Scans started with StartScan live as long as ctx.
func (m *Manager) Run(ctx context.Context) error {
	m.smu.Lock()
	m.base = ctx
	m.smu.Unlock()
	m.prune(ctx)
	m.seenNets = m.networks()
	tick := time.NewTicker(pruneEvery)
	defer tick.Stop()
	ask := time.NewTicker(askGap)
	defer ask.Stop()
	for {
		for {
			msg, ok := m.pop()
			if !ok {
				break
			}
			m.handleMessage(ctx, msg)
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		m.recheckNetworks(ctx)
		m.askDue(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.wake:
		case <-ask.C:
		case <-tick.C:
			m.prune(ctx)
		}
	}
}

func (m *Manager) prune(ctx context.Context) {
	if n, err := m.o.Store.PruneProposals(ctx, m.clk.Now().Add(-proposalTTL)); err != nil {
		m.log.Warn("discovery: expire proposals", "err", err)
	} else if n > 0 {
		m.log.Info("discovery: expired proposals", "n", n)
	}
}

// Attach subscribes to the discovery messages and the device topics (LWT,
// STATE, INFO, STATUS5; spec 11.3a) of bus, and lets discovery send its
// Status 5 queries there. Retained messages are replayed by the bus before
// Attach returns; they are queued, not processed inline. The returned func
// unsubscribes and stops the queries (the MQTT discovery setting).
func (m *Manager) Attach(bus mqttbus.Bus) (unsubscribe func()) {
	m.tmu.Lock()
	m.bus, m.attached = bus, true
	m.tmu.Unlock()
	unsub := bus.Subscribe([]mqttbus.Kind{mqttbus.KindDiscovery, mqttbus.KindLWT, mqttbus.KindState, mqttbus.KindInfo, mqttbus.KindStatus}, m.HandleMQTT)
	return func() {
		unsub()
		m.tmu.Lock()
		m.bus, m.attached = nil, false
		m.topics = map[string]*topicState{}
		m.tmu.Unlock()
	}
}

// HandleMQTT queues a discovery or device-topic message. It never blocks (it
// runs on the bus goroutine). Messages are coalesced per MAC (discovery) or
// per topic and kind, so a flood of one key costs one slot; new keys beyond
// the queue limit are dropped.
func (m *Manager) HandleMQTT(msg mqttbus.Message) {
	if len(msg.Payload) > maxDiscoveryPayload {
		return // real messages are ~1.5 KiB; never hold a big one in memory
	}
	switch msg.Kind {
	case mqttbus.KindDiscovery, mqttbus.KindLWT, mqttbus.KindState, mqttbus.KindInfo:
	case mqttbus.KindStatus:
		if msg.Suffix != "STATUS5" {
			return
		}
	case mqttbus.KindSensor, mqttbus.KindResult, mqttbus.KindPower, mqttbus.KindUpgrade, mqttbus.KindFile:
		return // nothing discovery needs
	}
	key := string(msg.Kind) + "/" + msg.Suffix + "/" + msg.Topic
	if msg.Kind == mqttbus.KindDiscovery {
		key = strings.ToLower(key)
	}
	m.qmu.Lock()
	old, queued := m.pending[key]
	if !queued && len(m.pending) >= maxQueuedMessages || m.qbytes-len(old.Payload)+len(msg.Payload) > maxQueuedBytes {
		m.qmu.Unlock()
		return
	}
	if !queued {
		m.order = append(m.order, key)
	}
	m.qbytes += len(msg.Payload) - len(old.Payload)
	m.pending[key] = msg
	m.qmu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) pop() (mqttbus.Message, bool) {
	m.qmu.Lock()
	defer m.qmu.Unlock()
	if len(m.order) == 0 {
		return mqttbus.Message{}, false
	}
	key := m.order[0]
	m.order = m.order[1:]
	msg := m.pending[key]
	m.qbytes -= len(msg.Payload)
	delete(m.pending, key)
	return msg, true
}

// handleMessage evaluates one queued message.
func (m *Manager) handleMessage(ctx context.Context, msg mqttbus.Message) {
	if msg.Kind != mqttbus.KindDiscovery {
		m.handleTopic(ctx, msg)
		return
	}
	c, err := parseDiscovery(msg.Topic, msg.Payload)
	if err != nil {
		m.log.Debug("discovery: message ignored", "err", err)
		return
	}
	m.noteDiscoveryTopic(c)
	if !m.allowMAC(c.mac) {
		return // a kept claim of the topic stays (a reconnect replays the same config)
	}
	m.dropClaim(c.topic)
	out, err := m.consider(ctx, c)
	switch {
	case err != nil && ctx.Err() == nil:
		m.log.Warn("discovery: could not evaluate device", "mac", c.mac, "err", err)
	case out == outIgnored && !tasmota.IPAllowed(c.ip, m.networks()):
		m.log.Info("discovery: announced address outside the device networks", "mac", c.mac, "topic", c.topic, "ip", c.ip.String())
		m.keepOutsideClaim(c.topic, fmt.Sprintf("its announced address %s is outside the device networks", c.ip), c, m.clk.Now())
	}
}

// allowMAC enforces minMessageGap per MAC. The table never resets: when it
// is full, entries older than the gap are dropped (they carry no information)
// and, if it is still full, the MAC is refused (fail closed).
func (m *Manager) allowMAC(mac string) bool {
	now := m.clk.Now()
	m.gapMu.Lock()
	defer m.gapMu.Unlock()
	if t, ok := m.last[mac]; ok && now.Sub(t) < minMessageGap {
		return false
	}
	if _, known := m.last[mac]; !known && len(m.last) >= maxGapEntries {
		for k, t := range m.last {
			if now.Sub(t) >= minMessageGap {
				delete(m.last, k)
			}
		}
		if len(m.last) >= maxGapEntries {
			return false
		}
	}
	m.last[mac] = now
	return true
}

func portOf(d model.Device) int {
	if d.Port == 0 {
		return 80
	}
	return d.Port
}

type matchKind int

const (
	matchNone matchKind = iota
	matchMAC
	matchTopic
	matchAddress
)

// match mirrors the lookup order of inventory.Upsert: MAC (any MAC of either
// set), topic, address.
func match(devs []model.Device, c candidate) (model.Device, matchKind) {
	ip := c.ip.String()
	for _, d := range devs {
		for _, m := range c.allMACs() {
			if d.HasMAC(m) {
				return d, matchMAC
			}
		}
	}
	if c.topic != "" {
		for _, d := range devs {
			if d.Topic == c.topic {
				return d, matchTopic
			}
		}
	}
	for _, d := range devs {
		if strings.EqualFold(d.Address, ip) && (c.port == 0 || portOf(d) == c.port) {
			return d, matchAddress
		}
	}
	return model.Device{}, matchNone
}

// Outcomes reported per scan hit.
const (
	outNew           = "new proposal"
	outKnown         = "known device"
	outAddressChange = "address-change proposal"
	outConflict      = "conflict proposal"
	outMoved         = "address updated"
	outIgnored       = "ignored"
)

// consider decides what a device claim means. It never changes an existing
// device except through inventory.Upsert, whose identity rules decide
// whether a move is proven; everything else becomes a proposal.
func (m *Manager) consider(ctx context.Context, c candidate) (string, error) {
	if !tasmota.IPAllowed(c.ip, m.networks()) {
		return outIgnored, nil // outside DEVICE_NETWORKS: could never be accepted
	}
	m.cmu.Lock()
	defer m.cmu.Unlock()
	devs, err := m.o.Inventory.List(ctx)
	if err != nil {
		return "", err
	}
	rec, how := match(devs, c)
	if how == matchNone {
		return outNew, m.propose(ctx, c, store.ProposalNew, 0, "not in the inventory")
	}
	if how == matchMAC {
		// Same MAC = same device (spec 11.1). Where it is stored decides what happens.
		if (c.port == 0 || portOf(rec) == c.port) && m.sameHost(ctx, rec.Address, c.ip) {
			return outKnown, nil // a hostname that resolves to the IP is the same place: no-op, no audit
		}
		switch {
		case !isIP(rec.Address):
			// A stored hostname is never replaced by an IP automatically.
			reason := fmt.Sprintf("%q is stored as %s; MAC %s now answers at %s (the name does not resolve there)",
				rec.Name, netaddr.Display(rec.Address, portOf(rec), "http"), c.mac, netaddr.Display(c.ip.String(), c.port, "http"))
			return outAddressChange, m.propose(ctx, c, store.ProposalAddressChange, rec.ID, reason)
		case rec.HasCredentials():
			// Its password would move to whatever answers: an admin decides, not discovery.
			reason := fmt.Sprintf("%q is stored at %s; MAC %s answers at %s. The device has a password: \"Update address\" sends it to the new address",
				rec.Name, netaddr.Display(rec.Address, portOf(rec), "http"), c.mac, netaddr.Display(c.ip.String(), c.port, "http"))
			return outAddressChange, m.propose(ctx, c, store.ProposalAddressChange, rec.ID, reason)
		}
	}
	in := model.Device{Name: rec.Name, Address: c.ip.String(), Port: c.port, MAC: c.mac}
	if in.Port == 0 {
		in.Port = rec.Port
	}
	if how == matchTopic {
		in.Topic = rec.Topic // find the same record; never a topic taken from the claim
	}
	_, _, err = m.o.Inventory.UpsertExisting(ctx, "discovery", rec.ID, in)
	switch {
	case err == nil:
		return outMoved, nil
	case errors.Is(err, inventory.ErrConflict):
		return outIgnored, nil // the device changed or vanished meanwhile; the next sighting decides
	case errors.Is(err, inventory.ErrNeedsVerification):
		reason := fmt.Sprintf("%q is stored at %s; MAC %s answers at %s and must be confirmed (%s)",
			rec.Name, netaddr.Display(rec.Address, portOf(rec), "http"), c.mac, netaddr.Display(c.ip.String(), c.port, "http"), detail(err))
		return outAddressChange, m.propose(ctx, c, store.ProposalAddressChange, rec.ID, reason)
	case errors.Is(err, inventory.ErrIdentityMismatch):
		reason := fmt.Sprintf("%q (%s) is stored with MAC %s, but MAC %s claims its address or topic",
			rec.Name, rec.Address, orNone(rec.MAC), c.mac)
		return outConflict, m.propose(ctx, c, store.ProposalConflict, rec.ID, reason)
	default:
		return "", err
	}
}

// sameHost reports whether the stored address is the IP ip, or a hostname that
// resolves to it. A lookup that fails means "not the same".
func (m *Manager) sameHost(ctx context.Context, stored string, ip netip.Addr) bool {
	if a, err := netip.ParseAddr(stored); err == nil {
		return a.Unmap() == ip.Unmap()
	}
	rctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	addrs, err := m.o.Resolver.LookupNetIP(rctx, "ip", stored)
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if a.Unmap() == ip.Unmap() {
			return true
		}
	}
	return false
}

func isIP(s string) bool {
	_, err := netip.ParseAddr(s)
	return err == nil
}

func orNone(mac string) string {
	if mac == "" {
		return "(none)"
	}
	return mac
}

// detail trims the inventory prefix from an error text for display.
func detail(err error) string {
	s := err.Error()
	if _, after, ok := strings.Cut(s, ": "); ok {
		return after
	}
	return s
}

func (m *Manager) propose(ctx context.Context, c candidate, kind string, existing model.DeviceID, reason string) error {
	limit := maxRelatedProposals
	if kind == store.ProposalNew {
		limit = maxNewProposals
	}
	row := store.ProposalRow{
		Kind: kind, Via: c.via, Reason: reason, ExistingID: existing, SeenAt: m.clk.Now(),
		Device: model.Device{Name: c.name, Address: c.ip.String(), Port: c.port, MAC: c.mac, Topic: c.topic, FullTopic: c.fullTopic, Net: model.NetFacts{Hostname: c.host}},
	}
	if err := m.mergeSighting(ctx, &row); err != nil {
		return err
	}
	_, err := m.o.Store.UpsertProposal(ctx, row, limit)
	if errors.Is(err, store.ErrProposalLimit) {
		m.log.Warn("discovery: proposal limit reached, a device is not shown", "kind", kind, "limit", limit)
		if m.o.OnDropped != nil {
			m.o.OnDropped(kind)
		}
	}
	return err
}

// mergeSighting folds the stored row of the same MAC into row, so the two
// sources describe one device without overwriting each other (D14): MQTT
// owns topic, full topic, name and source, a scan contributes the port.
func (m *Manager) mergeSighting(ctx context.Context, row *store.ProposalRow) error {
	prev, ok, err := m.storedRow(ctx, row.Device.MAC)
	if err != nil || !ok {
		return err
	}
	if row.Via == ViaScan && prev.Via == ViaMQTT {
		row.Via = ViaMQTT
		row.Device.Name, row.Device.Topic, row.Device.FullTopic = prev.Device.Name, prev.Device.Topic, prev.Device.FullTopic
	}
	if row.Device.Net.Hostname == "" {
		row.Device.Net.Hostname = prev.Device.Net.Hostname // a sighting without one keeps the known hostname
	}
	if row.Via == ViaMQTT && row.Device.Port == 0 {
		row.Device.Port = prev.Device.Port
	}
	return nil
}

// storedRow finds the visible or dismissed proposal of mac.
func (m *Manager) storedRow(ctx context.Context, mac string) (store.ProposalRow, bool, error) {
	for _, list := range []func(context.Context) ([]store.ProposalRow, error){m.o.Store.ListProposals, m.o.Store.ListDismissedProposals} {
		rows, err := list(ctx)
		if err != nil {
			return store.ProposalRow{}, false, err
		}
		for _, r := range rows {
			if r.Device.MAC == mac {
				return r, true, nil
			}
		}
	}
	return store.ProposalRow{}, false, nil
}

// Proposals lists the open proposals, newest first. Rows the inventory has
// overtaken (the device was added or moved by hand, or its hostname resolves
// to the proposed address) are dropped.
func (m *Manager) Proposals(ctx context.Context) ([]Proposal, error) {
	rows, err := m.o.Store.ListProposals(ctx)
	if err != nil {
		return nil, err
	}
	return m.visible(ctx, rows)
}

// Ignored lists the dismissed proposals ("Show ignored"), newest first.
func (m *Manager) Ignored(ctx context.Context) ([]Proposal, error) {
	rows, err := m.o.Store.ListDismissedProposals(ctx)
	if err != nil {
		return nil, err
	}
	return m.visible(ctx, rows)
}

func (m *Manager) visible(ctx context.Context, rows []store.ProposalRow) ([]Proposal, error) {
	devs, err := m.o.Inventory.List(ctx)
	if err != nil {
		return nil, err
	}
	byMAC := map[string]model.Device{}
	byID := map[model.DeviceID]model.Device{}
	for _, d := range devs {
		for _, mac := range append([]string{d.MAC}, d.MACs...) {
			if mac != "" {
				byMAC[mac] = d
			}
		}
		byID[d.ID] = d
	}
	var out []Proposal
	for _, r := range rows {
		if m.overtaken(ctx, r, byMAC, byID) {
			if err := m.o.Store.DeleteProposal(ctx, r.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
				m.log.Warn("discovery: drop overtaken proposal", "id", r.ID, "err", err)
			}
			continue
		}
		out = append(out, Proposal{ID: r.ID, Device: r.Device, Via: r.Via, Kind: r.Kind, Reason: r.Reason, SeenAt: r.SeenAt, FirstSeen: r.FirstSeen, ExistingID: r.ExistingID, Version: versionOf(r),
			LWT: m.topicLWT(r.Device.Topic)})
	}
	return out, nil
}

// overtaken reports whether the inventory already reflects the proposal.
func (m *Manager) overtaken(ctx context.Context, r store.ProposalRow, byMAC map[string]model.Device, byID map[model.DeviceID]model.Device) bool {
	_, known := byMAC[r.Device.MAC]
	switch r.Kind {
	case store.ProposalNew:
		return known // a device whose MAC is in the inventory is never new
	case store.ProposalAddressChange:
		cur, ok := byID[r.ExistingID]
		if !ok {
			return true // the device is gone
		}
		if !cur.HasMAC(r.Device.MAC) {
			return false // now bound to another MAC; Accept reports it as stale
		}
		if ip, err := netip.ParseAddr(r.Device.Address); err == nil && (r.Device.Port == 0 || portOf(cur) == r.Device.Port) {
			return m.sameHost(ctx, cur.Address, ip)
		}
	}
	return false
}

func (m *Manager) audit(ctx context.Context, actor, action, target string, detail map[string]any, err error) {
	e := model.AuditEvent{Actor: actor, Action: action, Target: target, Result: "ok"}
	if b, merr := json.Marshal(detail); merr == nil {
		e.Detail = string(b)
	}
	if err != nil {
		e.Result = err.Error()
	}
	m.o.Audit.Record(ctx, e)
}

// Accept turns a proposal into inventory state on admin authority.
//
//   - "new": the device is created through inventory.Create (full
//     validation). If a Verifier is set and the address now answers with
//     another MAC, the proposal is stale and refused.
//   - "address-change": the new address is probed without credentials and
//     must answer with the candidate's and the device's MAC, then the stored
//     device gets it through inventory.Update (see acceptMove).
//   - "conflict": refused, the admin resolves it in the inventory.
func (m *Manager) Accept(ctx context.Context, actor string, id int64) (model.DeviceID, error) {
	return m.AcceptVersion(ctx, actor, id, "")
}

// versionOf identifies the content of a proposal: everything an admin sees
// and everything Accept applies. A new sighting with other content gives
// another version, so a confirmation of the old one is stale.
func versionOf(r store.ProposalRow) string {
	h := sha256.New()
	for _, f := range []string{r.Kind, r.Device.MAC, r.Device.Address, fmt.Sprint(r.Device.Port), fmt.Sprint(int64(r.ExistingID)),
		r.Device.Name, r.Device.Topic, r.Device.FullTopic} {
		_, _ = fmt.Fprintf(h, "%d:%s|", len(f), f) // hash.Hash.Write never fails
	}
	return hex.EncodeToString(h.Sum(nil))
}

// AcceptVersion is Accept for a proposal the admin looked at: version is the
// Proposal.Version that was shown. If the proposal changed since (a newer
// sighting claims another address, say), nothing is applied (ErrStale).
// Accept runs under the same lock as the evaluation of new sightings, so the
// proposal cannot change between this check and the write.
func (m *Manager) AcceptVersion(ctx context.Context, actor string, id int64, version string) (model.DeviceID, error) {
	m.cmu.Lock()
	defer m.cmu.Unlock()
	row, err := m.o.Store.GetProposal(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if row.Dismissed {
		return 0, ErrNotFound // hidden proposals cannot be accepted
	}
	target := fmt.Sprintf("proposal:%d", id)
	det := map[string]any{"kind": row.Kind, "mac": row.Device.MAC, "address": row.Device.Address, "version": versionOf(row)}
	var devID model.DeviceID
	switch {
	case version != "" && version != versionOf(row):
		err = fmt.Errorf("%w: the proposal changed after it was shown", ErrStale)
	case row.Kind == store.ProposalNew:
		devID, err = m.acceptNew(ctx, actor, row, det)
	case row.Kind == store.ProposalAddressChange:
		devID, err = m.acceptMove(ctx, actor, row, det)
	default:
		err = ErrNotAcceptable
	}
	m.audit(ctx, actor, "discovery.accept", target, det, err)
	if err != nil {
		return 0, err
	}
	if derr := m.o.Store.DeleteProposal(ctx, id); derr != nil && !errors.Is(derr, store.ErrNotFound) {
		m.log.Warn("discovery: remove accepted proposal", "id", id, "err", derr)
	}
	return devID, nil
}

type devicePasswordKey struct{}

// WithDevicePassword marks ctx as an accept in which the admin chose the fleet
// default device password (N20) for the new device. Only a new device takes
// it; an address change keeps the stored password.
func WithDevicePassword(ctx context.Context, password string) context.Context {
	return context.WithValue(ctx, devicePasswordKey{}, password)
}

// DevicePassword returns the password WithDevicePassword put into ctx, or "".
func DevicePassword(ctx context.Context) string {
	pw, _ := ctx.Value(devicePasswordKey{}).(string)
	return pw
}

// acceptNew creates the proposed device. Without a password an unanswered
// MAC probe is fine (nothing secret goes anywhere). With the fleet default
// password (N20) it fails closed: the address came from a sighting, not from
// the admin, and the fleet-wide password would go via basic auth to whatever
// answers there, so the host must prove the proposal's MAC first (security
// review 2026-10-22). A device that already asks for a password cannot
// answer the unauthenticated probe; the admin accepts it without the default
// and types the password in the device edit form.
func (m *Manager) acceptNew(ctx context.Context, actor string, row store.ProposalRow, det map[string]any) (model.DeviceID, error) {
	d := row.Device
	d.ID = 0
	pw := DevicePassword(ctx)
	err := errors.New("no verifier configured")
	offline := m.topicLWT(d.Topic) == "Offline"
	switch {
	case offline:
		// Seen on MQTT and offline (D15): the check could only time out. Nothing
		// secret goes anywhere: the fleet default password, which needs a proven
		// MAC, is withheld; the admin sets it in the device edit form if needed.
		// Its identity is read when it comes online (reprobe).
		err = nil
		det["mac_unverified"] = true
		if pw != "" {
			pw = ""
			det["default_password_withheld"] = "offline"
		}
	case m.o.Verifier != nil:
		var mac string
		if mac, err = m.o.Verifier.VerifyMAC(ctx, d.Address, portOf(d)); err == nil {
			if got, nerr := inventory.NormaliseMAC(mac); nerr != nil || got != d.MAC {
				return 0, fmt.Errorf("%w: %s now answers with a different MAC", ErrStale, d.Address)
			}
		}
	}
	if err != nil && pw == "" {
		det["mac_unverified"] = true // accepted as before; the audit says that nothing was proven
	}
	if pw != "" {
		if err != nil {
			return 0, fmt.Errorf("%w: %s did not prove its MAC, so the default password is not sent there; use \"Add…\" and type the device's password (or none), or set it later in the device edit form: %w",
				ErrNotAcceptable, d.Address, err)
		}
		d.HTTPPassword = pw // the fleet default (N20); the user stays empty: Tasmota's admin
	}
	id, err := m.o.Inventory.Create(ctx, actor, d)
	if err == nil && m.o.OnAccepted != nil && !offline {
		m.markProbed(id)
		m.o.OnAccepted(id)
	}
	return id, err
}

// acceptMove applies a confirmed address change (always an admin action; the
// scan itself never gets here for a device with a password). The new address
// is probed WITHOUT credentials and must answer with the MAC of the candidate
// and of the stored device. A password-protected device cannot answer that
// probe, so for it an unproven MAC needs the admin's explicit confirmation for
// exactly this address (inventory.WithUnverifiedMoveConfirmed), because the
// stored password then goes to a host that may be an impostor; the audit entry
// carries "mac_unverified". A wrong MAC stops the move in every case (audit
// row 14, 2026-10-21, security review).
func (m *Manager) acceptMove(ctx context.Context, actor string, row store.ProposalRow, det map[string]any) (model.DeviceID, error) {
	cur, err := m.o.Inventory.Get(ctx, row.ExistingID)
	if errors.Is(err, store.ErrNotFound) {
		return 0, fmt.Errorf("%w: the device was deleted", ErrStale)
	}
	if err != nil {
		return 0, err
	}
	if err := MoveRefusal(cur); err != nil {
		return 0, err
	}
	if !cur.HasMAC(row.Device.MAC) {
		return 0, fmt.Errorf("%w: the device is now bound to another MAC", ErrStale)
	}
	ip, err := netip.ParseAddr(row.Device.Address)
	if err != nil || !tasmota.IPAllowed(ip.Unmap(), m.networks()) {
		return 0, fmt.Errorf("%w: %s is not an address of the device networks", ErrStale, row.Device.Address)
	}
	port := portOf(row.Device)
	if row.Device.Port == 0 {
		port = portOf(cur)
	}
	var got string
	err = errors.New("no verifier configured")
	if m.o.Verifier != nil {
		got, err = m.o.Verifier.VerifyMAC(ctx, row.Device.Address, port)
	}
	if err == nil {
		if got, err = inventory.NormaliseMAC(got); err != nil || got != row.Device.MAC {
			return 0, fmt.Errorf("%w: %s now answers with another MAC", ErrStale, row.Device.Address)
		}
	}
	switch {
	case cur.HasCredentials():
		// An answer without the password is no proof for a password-protected
		// device (see inventory.checkCredentialedMove): always the explicit confirmation.
		target := inventory.MoveTarget(row.Device.Address, port)
		if !inventory.UnverifiedMoveConfirmed(ctx, target) {
			return 0, &inventory.UnverifiedMoveError{Target: target}
		}
		det["mac_unverified"] = true
	case err != nil:
		return 0, fmt.Errorf("%w: %s did not prove its MAC: %w", ErrNotAcceptable, row.Device.Address, err)
	}
	next := cur
	next.Address, next.Port = row.Device.Address, port
	if err := m.o.Inventory.UpdateIf(ctx, actor, cur, next); err != nil {
		if errors.Is(err, inventory.ErrConflict) {
			return 0, fmt.Errorf("%w: the device changed meanwhile", ErrStale)
		}
		return 0, err
	}
	return cur.ID, nil
}

// MoveRefusal says why a stored device's address cannot be changed through
// discovery, or nil. A device without MAC is never bound to a claimed MAC
// (an admin action in the device edit form). A device with a password can be
// moved by an admin ("Update address"); the page says that the password goes
// to the new address.
func MoveRefusal(d model.Device) error {
	if d.MAC == "" {
		return fmt.Errorf("%w: the device has no MAC; set its address and probe it in the device edit form", ErrNotAcceptable)
	}
	return nil
}

// Undismiss makes an ignored proposal visible again.
func (m *Manager) Undismiss(ctx context.Context, actor string, id int64) error {
	err := m.o.Store.UndismissProposal(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		err = ErrNotFound
	}
	m.audit(ctx, actor, "discovery.undismiss", fmt.Sprintf("proposal:%d", id), map[string]any{}, err)
	return err
}

// Dismiss hides a proposal. It stays hidden until the device is seen with
// different data, and is deleted after 30 days without a sighting.
func (m *Manager) Dismiss(ctx context.Context, actor string, id int64) error {
	err := m.o.Store.DismissProposal(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		err = ErrNotFound
	}
	m.audit(ctx, actor, "discovery.dismiss", fmt.Sprintf("proposal:%d", id), map[string]any{}, err)
	return err
}

// networks returns the device networks in force now.
func (m *Manager) networks() []netip.Prefix {
	if m.o.NetworkSet != nil {
		return m.o.NetworkSet.Get()
	}
	return m.o.Networks
}
