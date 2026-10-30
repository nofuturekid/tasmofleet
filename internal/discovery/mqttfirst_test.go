package discovery

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// MQTT-first discovery (spec 11.3a, issue #47): devices that publish no
// tasmota/discovery config are found from their own topics.

// fakeBus records the commands discovery publishes.
type fakeBus struct {
	mu   sync.Mutex
	cmds []string // "<topic> <command> <payload>"
	subs map[int]func(mqttbus.Message)
	next int
	err  error
	down bool // Connected() reports false
}

func (b *fakeBus) Subscribe(_ []mqttbus.Kind, h func(mqttbus.Message)) func() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs == nil {
		b.subs = map[int]func(mqttbus.Message){}
	}
	id := b.next
	b.next++
	b.subs[id] = h
	return func() {
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
	}
}

func (b *fakeBus) Command(_ context.Context, topic, command, payload string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	b.cmds = append(b.cmds, topic+" "+command+" "+payload)
	return nil
}

func (b *fakeBus) CommandTopic(topic, command string) string { return "cmnd/" + topic + "/" + command }

func (b *fakeBus) Connected() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.down
}

func (b *fakeBus) sent() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.cmds...)
}

// manualClock is a clock the test moves.
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *manualClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (c *manualClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type mqttEnv struct {
	*env
	bus   *fakeBus
	clk   *manualClock
	unsub func()
}

// busAsker stands in for the command channel: it publishes the question on the fake bus.
type busAsker struct{ b *fakeBus }

func (a busAsker) Do(ctx context.Context, d model.Device, cmd string) (json.RawMessage, error) {
	if cmd != "Status 5" {
		return nil, errors.New("discovery asked for " + cmd)
	}
	return nil, a.b.Command(ctx, d.Topic, "STATUS", "5")
}

func newMQTTEnv(t *testing.T, mut func(*Options)) *mqttEnv {
	t.Helper()
	clk := &manualClock{t: time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)}
	b := &fakeBus{}
	e := newEnv(t, "172.31.0.0/16", func(o *Options) {
		o.Clock = clk
		o.Asker = busAsker{b}
		if mut != nil {
			mut(o)
		}
	})
	me := &mqttEnv{env: e, bus: b, clk: clk}
	me.unsub = e.mgr.Attach(b)
	return me
}

// fixture reads the synthetic MQTT-only capture into decoded messages.
func fixtureMessages(t *testing.T) []mqttbus.Message {
	t.Helper()
	f, err := os.Open("../../testdata/fixtures/mqtt/synthetic-no-discovery.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // read-only fixture
	codec, _ := mqttbus.NewTopicCodec("", [3]string{})
	var out []mqttbus.Message
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 4)
		kind, topic, suffix, ok := codec.Decode(parts[2])
		if !ok {
			t.Fatalf("fixture topic %q does not decode", parts[2])
		}
		out = append(out, mqttbus.Message{Kind: kind, Topic: topic, Suffix: suffix, Payload: []byte(parts[3]), Retained: parts[1] == "1"})
	}
	return out
}

func pick(msgs []mqttbus.Message, topic string, kind mqttbus.Kind) []mqttbus.Message {
	var out []mqttbus.Message
	for _, m := range msgs {
		if m.Topic == topic && (kind == "" || m.Kind == kind) {
			out = append(out, m)
		}
	}
	return out
}

func (e *mqttEnv) feed(msgs ...mqttbus.Message) {
	for _, m := range msgs {
		e.mgr.handleMessage(bg, m)
	}
}

func onlyNew(t *testing.T, e *env) Proposal {
	t.Helper()
	p := mustProposals(t, e)
	if len(p) != 1 || p[0].Kind != KindNew {
		t.Fatalf("proposals = %+v, want one new", p)
	}
	return p[0]
}

// Why: the reporter's missing devices (#47) publish LWT, STATE and INFO but no
// tasmota/discovery config. Discovery must ask such a topic for its MAC with
// one individual Status 5 (never Status 0, safety rule 2) and propose the
// device from the answer, with the topic MQTT saw.
func TestTopicWithoutDiscoveryConfigIsAskedForItsMACAndProposed(t *testing.T) {
	e := newMQTTEnv(t, nil)
	msgs := fixtureMessages(t)
	all := pick(msgs, "garage-door", "")
	e.feed(all[:4]...) // LWT, INFO1..3
	if p := mustProposals(t, e.env); len(p) != 0 {
		t.Fatalf("LWT and INFO carry no MAC, yet proposals = %+v", p)
	}
	e.clk.add(askGrace)
	e.mgr.askDue(bg)
	if got := e.bus.sent(); len(got) != 1 || got[0] != "garage-door STATUS 5" {
		t.Fatalf("commands = %q, want one Status 5 to garage-door", got)
	}
	e.feed(all[4]) // the STATUS5 answer
	p := onlyNew(t, e.env)
	if p.Via != ViaMQTT || p.Device.MAC != "02:00:5e:a1:b2:c3" || p.Device.Topic != "garage-door" || p.Device.Address != "172.31.13.41" || p.Device.Name != "garage-door" {
		t.Fatalf("proposal = %+v", p)
	}
	if !e.audit.has("discovery.identify") {
		t.Error("the automated Status 5 must be audited (which topic, why)")
	}
	for _, c := range e.bus.sent() {
		if strings.HasSuffix(c, " 0") || strings.HasSuffix(c, "STATUS ") {
			t.Errorf("discovery sent %q: Status 0 is never sent by automated code", c)
		}
	}
}

// Why: an ESP32 on Ethernet reports no Wi-Fi address; the proposal must carry
// the Ethernet address, and the device must still be recognised by either MAC.
func TestEthernetDeviceIsProposedAtItsEthernetAddress(t *testing.T) {
	e := newMQTTEnv(t, nil)
	msgs := pick(fixtureMessages(t), "workshop-eth", "")
	e.feed(msgs[:2]...)
	e.clk.add(askGrace)
	e.mgr.askDue(bg)
	e.feed(msgs[2])
	p := onlyNew(t, e.env)
	if p.Device.Address != "172.31.23.40" || p.Device.MAC != "02:00:5e:e7:40:00" {
		t.Fatalf("proposal = %+v, want the Ethernet IP and the Wi-Fi MAC", p)
	}

	// Stored with its Ethernet MAC only: the same device, no proposal.
	e2 := newMQTTEnv(t, nil)
	if _, err := e2.inv.Create(bg, "t", model.Device{Name: "eth", Address: "172.31.23.40", MAC: "02:00:5e:e7:40:01"}); err != nil {
		t.Fatal(err)
	}
	e2.feed(msgs[:2]...)
	e2.clk.add(askGrace)
	e2.mgr.askDue(bg)
	e2.feed(msgs[2])
	if p := mustProposals(t, e2.env); len(p) != 0 {
		t.Fatalf("a device known by its Ethernet MAC came back as %+v", p)
	}
}

// Why: an offline device cannot answer; asking it is noise. It is listed as
// seen but not identified, so "on the broker but not in the list" is visible.
func TestOfflineTopicIsNotAskedButListed(t *testing.T) {
	e := newMQTTEnv(t, nil)
	e.feed(pick(fixtureMessages(t), "attic-fan", "")...)
	e.mgr.askDue(bg)
	if got := e.bus.sent(); len(got) != 0 {
		t.Fatalf("asked an offline topic: %q", got)
	}
	u := e.mgr.Unidentified()
	if len(u) != 1 || u[0].Topic != "attic-fan" || u[0].Online || !strings.Contains(u[0].Reason, "offline") {
		t.Fatalf("unidentified = %+v", u)
	}
}

// Why: Tasmota publishes its discovery config a few seconds after the LWT. A
// device that is about to announce itself must not be asked first.
func TestFreshlyOnlineTopicWaitsForItsDiscoveryConfig(t *testing.T) {
	e := newMQTTEnv(t, nil)
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "couch", Payload: []byte("Online")})
	e.mgr.askDue(bg)
	e.clk.add(askGrace / 2)
	e.feed(discoveryMsg("02005EEA7CA3", "172.31.13.29", "couch"))
	e.clk.add(askGrace)
	e.mgr.askDue(bg)
	if got := e.bus.sent(); len(got) != 0 {
		t.Fatalf("asked a device that announced itself: %q", got)
	}
}

// Why: a flood of fake LWTs must not become a command flood: one command per
// second for the whole bus, one per topic per 10 minutes while unresolved.
func TestStatusQueriesAreRateLimited(t *testing.T) {
	e := newMQTTEnv(t, nil)
	for _, topic := range []string{"a", "b"} {
		e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: topic, Payload: []byte("Online"), Retained: true})
	}
	e.clk.add(askGrace)
	e.mgr.askDue(bg)
	e.mgr.askDue(bg)
	if n := len(e.bus.sent()); n != 1 {
		t.Fatalf("%d commands within a second, want 1", n)
	}
	e.clk.add(askGap)
	e.mgr.askDue(bg)
	if n := len(e.bus.sent()); n != 2 {
		t.Fatalf("%d commands after the gap, want 2", n)
	}
	e.clk.add(askGap)
	e.mgr.askDue(bg)
	if n := len(e.bus.sent()); n != 2 {
		t.Fatalf("a topic was asked again within %v: %q", askEvery, e.bus.sent())
	}
	e.clk.add(askEvery)
	e.mgr.askDue(bg)
	if n := len(e.bus.sent()); n != 3 {
		t.Fatalf("an unresolved topic must be asked again after %v: %q", askEvery, e.bus.sent())
	}
}

// Why: Tasmota mirrors every HTTP command reply on MQTT, so tasmofleet's own
// probes produce STATUS5 messages for inventory devices. Only replies for
// topics discovery asked may become candidates.
func TestUnaskedStatus5IsIgnored(t *testing.T) {
	e := newMQTTEnv(t, nil)
	e.feed(pick(fixtureMessages(t), "garage-door", mqttbus.KindStatus)...)
	if p := mustProposals(t, e.env); len(p) != 0 {
		t.Fatalf("an unasked STATUS5 became %+v", p)
	}
}

// Why: topics an inventory device or a discovery config already accounts for
// need no question.
func TestKnownTopicsAreNotAsked(t *testing.T) {
	e := newMQTTEnv(t, nil)
	if _, err := e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: "02:00:5e:d2:2c:14", Topic: "lamp"}); err != nil {
		t.Fatal(err)
	}
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "lamp", Payload: []byte("Online"), Retained: true})
	e.feed(discoveryMsg("02005EEA7CA3", "172.31.13.29", "couch"))
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "couch", Payload: []byte("Online"), Retained: true})
	e.mgr.askDue(bg)
	if got := e.bus.sent(); len(got) != 0 {
		t.Fatalf("asked known topics: %q", got)
	}
	if u := e.mgr.Unidentified(); len(u) != 0 {
		t.Fatalf("known topics listed as unidentified: %+v", u)
	}
}

// Why: both sources resolve to one proposal by MAC and must not race: a scan
// sighting of a device MQTT saw keeps its topic (and source), and contributes
// the HTTP port; a later MQTT sighting keeps that port.
func TestScanAndMQTTSightingsMergeByMAC(t *testing.T) {
	e := newMQTTEnv(t, nil)
	e.feed(discoveryMsg("02005ED22C14", "172.31.13.31", "lamp"))
	scan := candidate{via: ViaScan, mac: "02:00:5e:d2:2c:14", ip: mustAddr("172.31.13.31"), port: 8080, name: "Lamp lamp"}
	if _, err := e.mgr.consider(bg, scan); err != nil {
		t.Fatal(err)
	}
	p := onlyNew(t, e.env)
	if p.Device.Topic != "lamp" || p.Device.FullTopic == "" || p.Via != ViaMQTT || p.Device.Port != 8080 {
		t.Fatalf("after the scan: %+v", p)
	}
	e.clk.add(minMessageGap)
	e.feed(discoveryMsg("02005ED22C14", "172.31.13.31", "lamp"))
	if p := onlyNew(t, e.env); p.Device.Port != 8080 || p.Device.Topic != "lamp" {
		t.Fatalf("after the next MQTT sighting: %+v", p)
	}
}

// Why: #45, adding an offline device seen on MQTT must not depend on an HTTP
// probe. The device is stored with the MAC and topic MQTT saw, so MQTT
// matches it when it comes back.
func TestOfflineMQTTDeviceIsAcceptedWithoutProbe(t *testing.T) {
	e := newMQTTEnv(t, nil)
	e.feed(discoveryMsg("02005ED22C14", "172.31.13.31", "lamp"))
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "lamp", Payload: []byte("Offline"), Retained: true})
	p := onlyNew(t, e.env)
	if p.LWT != "Offline" {
		t.Fatalf("proposal LWT = %q, want Offline", p.LWT)
	}
	id, err := e.mgr.Accept(bg, "admin", p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n := e.ver.calls.Load(); n != 0 {
		t.Errorf("an offline device was probed %d times at accept", n)
	}
	d, _ := e.inv.Get(bg, id)
	if d.MAC != "02:00:5e:d2:2c:14" || d.Topic != "lamp" || d.Address != "172.31.13.31" {
		t.Fatalf("device = %+v", d)
	}
}

// Why: the fleet default password goes only to an address that proved the
// MAC (security review 2026-10-22). An offline device cannot prove it, so it
// is added without the password (nothing secret goes anywhere) instead of
// not at all, and the audit row says so.
func TestOfflineAcceptWithholdsTheDefaultPassword(t *testing.T) {
	e := newMQTTEnv(t, nil)
	e.feed(discoveryMsg("02005ED22C14", "172.31.13.31", "lamp"))
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "lamp", Payload: []byte("Offline"), Retained: true})
	id, err := e.mgr.Accept(WithDevicePassword(bg, "fake-default"), "admin", onlyNew(t, e.env).ID)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := e.inv.Get(bg, id); d.HTTPPassword != "" {
		t.Fatal("the default password was stored for an address that proved nothing")
	}
	e.audit.mu.Lock()
	defer e.audit.mu.Unlock()
	found := false
	for _, ev := range e.audit.ev {
		if ev.Action == "discovery.accept" && strings.Contains(ev.Detail, "default_password_withheld") {
			found = true
		}
	}
	if !found {
		t.Error("the accept audit must say that the default password was withheld")
	}
}

// Why: a device added while offline has no identity; it must be probed when
// it comes online, not only after a manual "Refresh identity", and not more
// than once per 10 minutes.
func TestDeviceWithoutIdentityIsProbedWhenItComesOnline(t *testing.T) {
	var mu sync.Mutex
	var probed []model.DeviceID
	e := newMQTTEnv(t, func(o *Options) {
		o.OnAccepted = func(id model.DeviceID) { mu.Lock(); probed = append(probed, id); mu.Unlock() }
	})
	id, err := e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: "02:00:5e:d2:2c:14", Topic: "lamp"})
	if err != nil {
		t.Fatal(err)
	}
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "lamp", Payload: []byte("Offline"), Retained: true})
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "lamp", Payload: []byte("Online")})
	e.feed(mqttbus.Message{Kind: mqttbus.KindState, Topic: "lamp", Payload: []byte(`{}`)})
	mu.Lock()
	got := append([]model.DeviceID(nil), probed...)
	mu.Unlock()
	if len(got) != 1 || got[0] != id {
		t.Fatalf("probes = %v, want one of %d", got, id)
	}
	e.clk.add(reprobeEvery)
	e.feed(mqttbus.Message{Kind: mqttbus.KindState, Topic: "lamp", Payload: []byte(`{}`)})
	mu.Lock()
	n := len(probed)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("probes after %v = %d, want 2", reprobeEvery, n)
	}
}

// Why: the MQTT discovery setting switches the whole MQTT side off, the
// automated Status 5 included.
func TestDetachStopsQueries(t *testing.T) {
	e := newMQTTEnv(t, nil)
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "a", Payload: []byte("Online"), Retained: true})
	e.unsub()
	e.mgr.askDue(bg)
	if got := e.bus.sent(); len(got) != 0 {
		t.Fatalf("commands after detach: %q", got)
	}
	if u := e.mgr.Unidentified(); len(u) != 0 {
		t.Fatalf("unidentified after detach: %+v", u)
	}
}

// Why: the unresolved-topic table is bounded; fake topics cannot grow it.
func TestUnidentifiedTopicsAreBounded(t *testing.T) {
	e := newMQTTEnv(t, nil)
	for i := 0; i < maxTopics+10; i++ {
		e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "t" + itoa(i), Payload: []byte("Offline"), Retained: true})
	}
	if n := len(e.mgr.Unidentified()); n != maxTopics {
		t.Fatalf("%d topics kept, want %d", n, maxTopics)
	}
}

// Why: an address outside DEVICE_NETWORKS could never be added; it must say
// so instead of disappearing silently.
func TestAnswerOutsideNetworksIsListedWithReason(t *testing.T) {
	e := newMQTTEnv(t, nil)
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "far", Payload: []byte("Online"), Retained: true})
	e.clk.add(askGrace)
	e.mgr.askDue(bg)
	e.feed(mqttbus.Message{Kind: mqttbus.KindStatus, Topic: "far", Suffix: "STATUS5",
		Payload: []byte(`{"StatusNET":{"Hostname":"far","IPAddress":"10.9.9.9","Mac":"02:00:5E:99:99:99"}}`)})
	if p := mustProposals(t, e.env); len(p) != 0 {
		t.Fatalf("proposals = %+v", p)
	}
	u := e.mgr.Unidentified()
	if len(u) != 1 || !strings.Contains(u[0].Reason, "outside the device networks") {
		t.Fatalf("unidentified = %+v", u)
	}
}

var _ store.DiscoveryStore = (*store.SQLite)(nil)

func mustAddr(s string) netip.Addr { return netip.MustParseAddr(s) }

func itoa(i int) string { return strconv.Itoa(i) }

// ---- security review 2026-10-26 (trust boundary, verification) ----

func askAndAnswer(e *mqttEnv, topic, payload string, wait time.Duration) {
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: topic, Payload: []byte("Online"), Retained: true})
	e.clk.add(askGrace)
	e.mgr.askDue(bg)
	e.clk.add(wait)
	e.feed(mqttbus.Message{Kind: mqttbus.KindStatus, Topic: topic, Suffix: "STATUS5", Payload: []byte(payload)})
}

// Why: a STATUS5 reply is a claim from anyone who can publish. It is taken
// only for the exact topic asked and only shortly after the question, so a
// publisher cannot keep a standing claim open.
func TestLateStatus5IsIgnored(t *testing.T) {
	e := newMQTTEnv(t, nil)
	askAndAnswer(e, "late", `{"StatusNET":{"Hostname":"late","IPAddress":"172.31.13.60","Mac":"02:00:5E:60:60:60"}}`, askReplyWindow+time.Second)
	if p := mustProposals(t, e.env); len(p) != 0 {
		t.Fatalf("a reply after %v became %+v", askReplyWindow, p)
	}
}

// Why: an MQTT claim of a stored device's MAC at another address never moves
// it without the HTTP MAC proof at the new address (inventory rules, audit
// row 14); unproven, it is an address-change proposal for the admin.
func TestStatus5ClaimOfKnownMACDoesNotMoveWithoutProof(t *testing.T) {
	e := newMQTTEnv(t, nil)
	id, err := e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: "02:00:5e:d2:2c:14", Topic: "lamp"})
	if err != nil {
		t.Fatal(err)
	}
	askAndAnswer(e, "evil", `{"StatusNET":{"Hostname":"evil","IPAddress":"172.31.13.66","Mac":"02:00:5E:D2:2C:14"}}`, 0)
	if d, _ := e.inv.Get(bg, id); d.Address != "172.31.13.31" {
		t.Fatalf("an unproven MQTT claim moved the device to %s", d.Address)
	}
	if p := mustProposals(t, e.env); len(p) != 1 || p[0].Kind != KindAddressChange {
		t.Fatalf("proposals = %+v, want one address change", p)
	}
}

// Why: an accept without MAC proof (offline device) must be visible as such
// in the audit.
func TestOfflineAcceptIsAuditedAsMACUnverified(t *testing.T) {
	e := newMQTTEnv(t, nil)
	e.feed(discoveryMsg("02005ED22C14", "172.31.13.31", "lamp"))
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "lamp", Payload: []byte("Offline"), Retained: true})
	if _, err := e.mgr.Accept(bg, "admin", onlyNew(t, e.env).ID); err != nil {
		t.Fatal(err)
	}
	e.audit.mu.Lock()
	defer e.audit.mu.Unlock()
	for _, ev := range e.audit.ev {
		if ev.Action == "discovery.accept" && strings.Contains(ev.Detail, `"mac_unverified":true`) {
			return
		}
	}
	t.Error("the accept audit lacks mac_unverified")
}

// Why: a device added unverified gets its identity from the first probe when
// it comes online. If its stored address now answers with another MAC, the
// probe would store a foreign identity (and firmware selection follows the
// identity), so it is not started and the refusal is audited.
func TestReprobeRefusedWhenTheAddressAnswersWithAnotherMAC(t *testing.T) {
	var probed int
	e := newMQTTEnv(t, func(o *Options) { o.OnAccepted = func(model.DeviceID) { probed++ } })
	if _, err := e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: "02:00:5e:d2:2c:14", Topic: "lamp"}); err != nil {
		t.Fatal(err)
	}
	e.ver.macs["172.31.13.31"] = "02:00:5e:99:99:99"
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "lamp", Payload: []byte("Online")})
	if probed != 0 || !e.audit.has("discovery.identify_refused") {
		t.Fatalf("probes = %d, refusal audited = %v", probed, e.audit.has("discovery.identify_refused"))
	}
	e.ver.macs["172.31.13.31"] = "02:00:5e:d2:2c:14"
	e.clk.add(reprobeEvery)
	e.feed(mqttbus.Message{Kind: mqttbus.KindState, Topic: "lamp", Payload: []byte(`{}`)})
	if probed != 1 {
		t.Fatalf("probes after the MAC matched = %d, want 1", probed)
	}
}

// ---- Opus review of #66 ----

// Why (M1): a device imported with a topic but no MAC (TasmoAdmin import
// whose first probe failed) has nothing to compare; any answer is not
// "another MAC". It must be probed at the address the admin set, not
// refused every 10 minutes forever.
func TestDeviceWithoutMACIsProbedWhenItComesOnline(t *testing.T) {
	var probed int
	e := newMQTTEnv(t, func(o *Options) { o.OnAccepted = func(model.DeviceID) { probed++ } })
	if _, err := e.inv.Create(bg, "import", model.Device{Name: "imported", Address: "172.31.13.31", Topic: "imported"}); err != nil {
		t.Fatal(err)
	}
	e.ver.macs["172.31.13.31"] = "02:00:5e:d2:2c:14"
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "imported", Payload: []byte("Online")})
	if probed != 1 || e.audit.has("discovery.identify_refused") {
		t.Fatalf("probes = %d, refused = %v; want one probe, no refusal", probed, e.audit.has("discovery.identify_refused"))
	}
}

// Why (L1): during a broker outage nothing can be sent; discovery must not
// pick a topic every second and warn, and must ask once the bus is back.
func TestNoQueriesWhileTheBusIsDown(t *testing.T) {
	e := newMQTTEnv(t, nil)
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "a", Payload: []byte("Online"), Retained: true})
	e.clk.add(askGrace)
	e.bus.mu.Lock()
	e.bus.down, e.bus.err = true, mqttbus.ErrNotConnected
	e.bus.mu.Unlock()
	e.mgr.askDue(bg)
	e.mgr.tmu.Lock()
	asked := !e.mgr.topics["a"].asked.IsZero() || !e.mgr.lastAsk.IsZero()
	e.mgr.tmu.Unlock()
	if asked {
		t.Fatal("a query was attempted while the bus is down")
	}
	e.bus.mu.Lock()
	e.bus.down, e.bus.err = false, nil
	e.bus.mu.Unlock()
	e.mgr.askDue(bg)
	if got := e.bus.sent(); len(got) != 1 {
		t.Fatalf("commands after the bus came back = %q, want one", got)
	}
}

// Why (L3): accepting an online device already starts its first probe; a
// STATE arriving before that probe stored the identity must not start a second.
func TestOnlineAcceptIsNotProbedTwice(t *testing.T) {
	var probed int
	e := newMQTTEnv(t, func(o *Options) { o.OnAccepted = func(model.DeviceID) { probed++ } })
	e.ver.macs["172.31.13.31"] = "02:00:5e:d2:2c:14"
	e.feed(discoveryMsg("02005ED22C14", "172.31.13.31", "lamp"))
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "lamp", Payload: []byte("Online")})
	if _, err := e.mgr.Accept(bg, "admin", onlyNew(t, e.env).ID); err != nil {
		t.Fatal(err)
	}
	e.feed(mqttbus.Message{Kind: mqttbus.KindState, Topic: "lamp", Payload: []byte(`{}`)})
	if probed != 1 {
		t.Fatalf("first probes = %d, want 1", probed)
	}
}

// Why (L5): the Discovered tab always sends the default password; when it is
// refused, the message must point to the way that works ("Add…").
func TestDefaultPasswordRefusalPointsToAddForm(t *testing.T) {
	e := newMQTTEnv(t, nil)
	e.feed(discoveryMsg("02005ED22C14", "172.31.13.31", "lamp"))
	_, err := e.mgr.Accept(WithDevicePassword(bg, "fake-default"), "admin", onlyNew(t, e.env).ID)
	if err == nil || !strings.Contains(err.Error(), "Add…") {
		t.Fatalf("err = %v, want a pointer to Add…", err)
	}
}

// ---- push security review before 0.13.0: credentials and identity binding ----

func auditDetails(e *env, action string) []string {
	e.audit.mu.Lock()
	defer e.audit.mu.Unlock()
	var out []string
	for _, ev := range e.audit.ev {
		if ev.Action == action {
			out = append(out, ev.Detail)
		}
	}
	return out
}

// Why: a reprobe is triggered by an MQTT message anyone can publish. For a
// device with a stored password, an address that cannot prove the stored MAC
// (a genuine password-protected Tasmota refuses the credential-less probe, but
// so does an impostor) must not receive the password on that trigger
// (acceptMove rule, audit row 14). It is retried on the normal cadence, and
// probed as soon as the MAC is proven.
func TestReprobeWithholdsStoredPasswordWhenMACIsUnverified(t *testing.T) {
	var probed int
	e := newMQTTEnv(t, func(o *Options) { o.OnAccepted = func(model.DeviceID) { probed++ } })
	if _, err := e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: "02:00:5e:d2:2c:14", Topic: "lamp", HTTPPassword: "fake-stored-pw"}); err != nil {
		t.Fatal(err)
	}
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "lamp", Payload: []byte("Online")})
	if probed != 0 || len(auditDetails(e.env, "discovery.identify_unverified")) != 1 {
		t.Fatalf("probes = %d, identify_unverified audits = %d; want 0 and 1", probed, len(auditDetails(e.env, "discovery.identify_unverified")))
	}
	for _, d := range auditDetails(e.env, "discovery.identify_unverified") {
		if strings.Contains(d, "fake-stored-pw") {
			t.Fatal("the password reached the audit detail")
		}
	}
	e.feed(mqttbus.Message{Kind: mqttbus.KindState, Topic: "lamp", Payload: []byte(`{}`)})
	if n := len(auditDetails(e.env, "discovery.identify_unverified")); n != 1 || probed != 0 {
		t.Fatalf("retried within %v: audits %d, probes %d", reprobeEvery, n, probed)
	}
	e.ver.macs["172.31.13.31"] = "02:00:5e:d2:2c:14"
	e.clk.add(reprobeEvery)
	e.feed(mqttbus.Message{Kind: mqttbus.KindState, Topic: "lamp", Payload: []byte(`{}`)})
	if probed != 1 {
		t.Fatalf("probes after the MAC was proven = %d, want 1", probed)
	}
}

// Why: without a stored password nothing secret goes out, so an unverified
// MAC does not block the probe of a device with a stored MAC.
func TestReprobeWithoutPasswordProbesWhenMACIsUnverified(t *testing.T) {
	var probed int
	e := newMQTTEnv(t, func(o *Options) { o.OnAccepted = func(model.DeviceID) { probed++ } })
	if _, err := e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: "02:00:5e:d2:2c:14", Topic: "lamp"}); err != nil {
		t.Fatal(err)
	}
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "lamp", Payload: []byte("Online")})
	if probed != 1 || len(auditDetails(e.env, "discovery.identify_unverified")) != 0 {
		t.Fatalf("probes = %d, want 1 without an unverified audit", probed)
	}
}

// Why: a device without a stored MAC has nothing to verify; its first probe
// is audited with the MAC its address answered and with the fact that the
// probe binds none (binding a MAC is an admin action, D8).
func TestReprobeOfDeviceWithoutMACIsAuditedWithTheAnsweringMAC(t *testing.T) {
	var probed int
	e := newMQTTEnv(t, func(o *Options) { o.OnAccepted = func(model.DeviceID) { probed++ } })
	if _, err := e.inv.Create(bg, "import", model.Device{Name: "imported", Address: "172.31.13.31", Topic: "imported"}); err != nil {
		t.Fatal(err)
	}
	e.ver.macs["172.31.13.31"] = "02:00:5e:d2:2c:14"
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "imported", Payload: []byte("Online")})
	det := auditDetails(e.env, "discovery.reprobe")
	if probed != 1 || len(det) != 1 || !strings.Contains(det[0], `"answered_mac":"02:00:5e:d2:2c:14"`) || !strings.Contains(det[0], `"mac_bound":false`) {
		t.Fatalf("probes = %d, reprobe audits = %q", probed, det)
	}
}

// ---- #45: claims outside the device networks, re-evaluated when they grow ----

const farMAC = "02:00:5e:99:99:99"

// newGrowEnv is an MQTT env whose device networks (172.31.0.0/16) can grow
// by 10.9.0.0/16 at runtime, as the settings page does.
func newGrowEnv(t *testing.T) (*mqttEnv, func()) {
	t.Helper()
	nets := tasmota.NewNetworkSet([]netip.Prefix{netip.MustParsePrefix("172.31.0.0/16")})
	e := newMQTTEnv(t, func(o *Options) { o.NetworkSet = nets })
	e.mgr.recheckNetworks(bg) // what Run does at its start
	grow := func() {
		nets.Set([]netip.Prefix{netip.MustParsePrefix("172.31.0.0/16"), netip.MustParsePrefix("10.9.0.0/16")})
		e.mgr.recheckNetworks(bg)
	}
	return e, grow
}

func farConfig() mqttbus.Message { return discoveryMsg("02005E999999", "10.9.9.9", "far") }

// Why (review of #45, M1): a reconnect replays the same discovery config
// within the 15 s MAC gap. The rate-limited repeat must not wipe the kept
// claim, or the device is invisible again and adding its network does nothing.
func TestRepeatedOutsideConfigKeepsItsClaim(t *testing.T) {
	e, grow := newGrowEnv(t)
	e.feed(farConfig(), farConfig())
	if u := e.mgr.Unidentified(); len(u) != 1 || !strings.Contains(u[0].Reason, "outside the device networks") {
		t.Fatalf("unidentified = %+v, want far listed as outside", u)
	}
	grow()
	if p := onlyNew(t, e.env); p.Device.MAC != farMAC || p.Device.Address != "10.9.9.9" {
		t.Fatalf("proposal = %+v", p)
	}
	if u := e.mgr.Unidentified(); len(u) != 0 {
		t.Fatalf("a proposed device is still listed: %+v", u)
	}
}

// Why (review of #45, M2): a kept claim ages. An old address must not become
// a proposal (it could be accepted unverified at the wrong host); the topic is
// asked again with one Status 5 instead, and the fresh answer decides.
func TestStaleOutsideClaimIsAskedAgainNotProposed(t *testing.T) {
	e, grow := newGrowEnv(t)
	answer := `{"StatusNET":{"Hostname":"far","IPAddress":"10.9.9.9","Mac":"02:00:5E:99:99:99"}}`
	askAndAnswer(e, "far", answer, 0)
	e.clk.add(claimMaxAge + time.Minute)
	grow()
	if p := mustProposals(t, e.env); len(p) != 0 {
		t.Fatalf("a claim older than %v became %+v", claimMaxAge, p)
	}
	e.mgr.askDue(bg)
	if got := e.bus.sent(); len(got) != 2 || got[1] != "far STATUS 5" {
		t.Fatalf("commands = %q, want a second Status 5 to far", got)
	}
	e.feed(mqttbus.Message{Kind: mqttbus.KindStatus, Topic: "far", Suffix: "STATUS5", Payload: []byte(answer)})
	if p := onlyNew(t, e.env); p.Device.Address != "10.9.9.9" {
		t.Fatalf("proposal = %+v", p)
	}
}

// Why (review of #45, M2): a newer sighting of the same MAC (a scan, say)
// knows better where the device is; the older kept claim must not overwrite
// that proposal when the networks grow.
func TestKeptClaimDoesNotOverwriteNewerSighting(t *testing.T) {
	e, grow := newGrowEnv(t)
	e.feed(farConfig())
	e.clk.add(time.Minute)
	if _, err := e.mgr.consider(bg, candidate{via: ViaScan, mac: farMAC, ip: mustAddr("172.31.13.77"), port: 80, name: "far"}); err != nil {
		t.Fatal(err)
	}
	grow()
	if p := onlyNew(t, e.env); p.Device.Address != "172.31.13.77" {
		t.Fatalf("proposal = %+v, the newer scan sighting must win", p)
	}
}

// Why (review of #45, M2): once the topic belongs to an inventory device, a
// claim kept from before must not be applied to that device later.
func TestKeptClaimIsDroppedWhenTopicBecomesKnown(t *testing.T) {
	e, grow := newGrowEnv(t)
	e.feed(farConfig())
	id, err := e.inv.Create(bg, "t", model.Device{Name: "far", Address: "172.31.13.9", Topic: "far"})
	if err != nil {
		t.Fatal(err)
	}
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "far", Payload: []byte("Online"), Retained: true})
	grow()
	if p := mustProposals(t, e.env); len(p) != 0 {
		t.Fatalf("proposals = %+v", p)
	}
	if d, _ := e.inv.Get(bg, id); d.Address != "172.31.13.9" || d.MAC != "" {
		t.Fatalf("the stored device was changed by an old claim: %+v", d)
	}
}

// Why (review of #45): a kept claim of a stored device's MAC goes through the
// normal rules when the networks grow: for a device with a password it is an
// address-change proposal, never a move (D8).
func TestKeptClaimOfCredentialedDeviceIsAddressChange(t *testing.T) {
	e, grow := newGrowEnv(t)
	id, err := e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: farMAC, HTTPPassword: "fake-test-password"})
	if err != nil {
		t.Fatal(err)
	}
	e.feed(farConfig())
	grow()
	if d, _ := e.inv.Get(bg, id); d.Address != "172.31.13.31" {
		t.Fatalf("a credentialed device was moved to %s", d.Address)
	}
	if p := mustProposals(t, e.env); len(p) != 1 || p[0].Kind != KindAddressChange || p[0].ExistingID != id {
		t.Fatalf("proposals = %+v, want one address change", p)
	}
}

// Why: kept claims live in the topic table; detaching MQTT discovery or
// evicting the topic drops them, so nothing comes back from a dropped table.
func TestDetachAndEvictionDropKeptClaims(t *testing.T) {
	t.Run("detach", func(t *testing.T) {
		e, grow := newGrowEnv(t)
		e.feed(farConfig())
		e.unsub()
		e.unsub = e.mgr.Attach(e.bus)
		grow()
		if p := mustProposals(t, e.env); len(p) != 0 {
			t.Fatalf("proposals = %+v", p)
		}
	})
	t.Run("eviction", func(t *testing.T) {
		e, grow := newGrowEnv(t)
		e.feed(farConfig())
		for i := 0; i < maxTopics; i++ {
			e.clk.add(time.Millisecond)
			e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "t" + itoa(i), Payload: []byte("Offline"), Retained: true})
		}
		grow()
		if p := mustProposals(t, e.env); len(p) != 0 {
			t.Fatalf("proposals = %+v", p)
		}
	})
}

// Why (review #276 M4): Status 5 is a command like any other; it goes through the channel (one in
// flight per topic, a gap between publishes), and without a channel discovery asks nothing instead
// of publishing on its own.
func TestStatus5GoesThroughTheAskerOnly(t *testing.T) {
	e := newMQTTEnv(t, func(o *Options) { o.Asker = nil })
	e.feed(pick(fixtureMessages(t), "garage-door", "")[:4]...)
	e.clk.add(askGrace)
	e.mgr.askDue(bg)
	if got := e.bus.sent(); len(got) != 0 {
		t.Fatalf("published without a channel: %v", got)
	}
}

// Why: a minimal image answers {"Command":"Unknown"} to Status 5. That is an answer: the question went
// out and must not be repeated every gap (the channel reports it as an error, which once looked like "not
// sent" and made discovery ask a minimal device every 1.5 s forever).
func TestStatus5AnsweredUnknownIsNotAskedAgainAtOnce(t *testing.T) {
	e := newMQTTEnv(t, func(o *Options) { o.Asker = unknownAsker{} })
	e.feed(pick(fixtureMessages(t), "garage-door", "")[:4]...)
	e.clk.add(askGrace)
	n := 0
	e.mgr.o.Asker = countingAsker{inner: unknownAsker{}, n: &n}
	e.mgr.askDue(bg)
	e.clk.add(askGap)
	e.mgr.askDue(bg)
	if n != 1 {
		t.Fatalf("asked %d times, want once until askEvery has passed", n)
	}
}

type unknownAsker struct{}

func (unknownAsker) Do(context.Context, model.Device, string) (json.RawMessage, error) {
	return nil, tasmota.ErrUnknownCommand
}

type countingAsker struct {
	inner StatusAsker
	n     *int
}

func (c countingAsker) Do(ctx context.Context, d model.Device, cmd string) (json.RawMessage, error) {
	*c.n++
	return c.inner.Do(ctx, d, cmd)
}
