package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/mqttcmd"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/settings"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

type gtStore struct {
	mu   sync.Mutex
	devs []model.Device
	now  func() time.Time
}

func (s *gtStore) list(context.Context) ([]model.Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Device(nil), s.devs...), nil
}
func (s *gtStore) GetDevice(_ context.Context, id model.DeviceID) (model.Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.devs {
		if d.ID == id {
			return d, nil
		}
	}
	return model.Device{}, errors.New("not found")
}
func (s *gtStore) ListDevices(ctx context.Context) ([]model.Device, error) { return s.list(ctx) }
func (s *gtStore) UpdateGroupTopicFromMQTT(_ context.Context, id model.DeviceID, _ store.ProbeGuard, _ string, gt string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.devs {
		if s.devs[i].ID == id {
			s.devs[i].GroupTopic, s.devs[i].GroupTopicReadAt = gt, s.now()
		}
	}
	return nil
}

func online(model.DeviceID) model.Health { return model.Health{State: model.HealthOnline} }

type gtClient struct {
	mu    sync.Mutex
	asked []model.DeviceID
	fail  map[model.DeviceID]bool
	paths []transport.Path
}

func (c *gtClient) Command(ctx context.Context, t tasmota.Target, cmd string) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, t.DeviceID())
	p, _ := transport.PathFrom(ctx)
	c.paths = append(c.paths, p)
	if cmd != "Status 1" {
		return nil, errors.New("the reader asked for " + cmd)
	}
	if c.fail[t.DeviceID()] {
		return nil, tasmota.ErrUnreachable
	}
	return json.RawMessage(`{"StatusPRM":{"GroupTopic":"tasmotas"}}`), nil
}
func (c *gtClient) DownloadConfig(context.Context, tasmota.Target) ([]byte, error) { return nil, nil }
func (c *gtClient) RootPage(context.Context, tasmota.Target) (tasmota.RootInfo, error) {
	return tasmota.RootInfo{}, nil
}

func TestGroupTopicReaderReadsOneUnreadDevicePerTick(t *testing.T) {
	// Why (#279): one unread GroupTopic blocks MQTT writes for every MQTT-routed device, so the
	// reader must fill the gaps by itself, slowly (one Status 1 per tick) and only where needed.
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	st := &gtStore{now: func() time.Time { return now }, devs: []model.Device{
		{ID: 1, Topic: "a", Address: "192.0.2.1"},
		{ID: 2, Topic: "b", Address: "192.0.2.2", GroupTopic: "tasmotas", GroupTopicReadAt: now},
		{ID: 3, Address: "192.0.2.3"}, // HTTP only: no topic, no group to rule out
		{ID: 4, Topic: "d", Address: "192.0.2.4", GroupTopic: "tasmotas", GroupTopicReadAt: now.Add(-time.Hour),
			Identity: model.Identity{DetectedAt: now}}, // a probe since: the value may be stale
		{ID: 5, Topic: "e"}, // added from the broker, no HTTP address
	}}
	c := &gtClient{}
	r := newGroupTopicReader(st.list, online, c, st, slog.New(slog.DiscardHandler))
	r.now = func() time.Time { return now }
	for i := range 6 {
		r.tick(t.Context())
		if want := min(i+1, 3); len(c.asked) != want {
			t.Fatalf("after tick %d: asked %v, want %d reads (one per tick, three devices need one)", i+1, c.asked, want)
		}
	}
	seen := map[model.DeviceID]bool{}
	for i, id := range c.asked {
		seen[id] = true
		if want := id == 5; (c.paths[i] == transport.MQTT) != want {
			t.Errorf("device %d pinned to %q: only a device without an HTTP address is read over MQTT outright", id, c.paths[i])
		}
	}
	if !seen[1] || !seen[4] || !seen[5] || seen[2] || seen[3] {
		t.Errorf("asked %v", c.asked)
	}
	devs, _ := st.list(t.Context())
	for _, d := range devs {
		if d.Topic != "" && (d.GroupTopic != "tasmotas" || d.GroupTopicReadAt.Before(now)) {
			t.Errorf("device %d: group topic %q read at %v", d.ID, d.GroupTopic, d.GroupTopicReadAt)
		}
	}
}

func TestGroupTopicReaderBacksOffAfterAFailedRead(t *testing.T) {
	// Why: an unreachable device must not be asked every tick; it is tried again later.
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	st := &gtStore{now: func() time.Time { return now }, devs: []model.Device{{ID: 1, Topic: "a", Address: "192.0.2.1"}}}
	c := &gtClient{fail: map[model.DeviceID]bool{1: true}}
	r := newGroupTopicReader(st.list, online, c, st, slog.New(slog.DiscardHandler))
	r.now = func() time.Time { return now }
	r.tick(t.Context())
	r.tick(t.Context())
	if len(c.asked) != 1 {
		t.Fatalf("asked %d times in a row", len(c.asked))
	}
	// soon at first (the device may not be known as ours on the broker right after start), then rarely
	now = now.Add(groupTopicBackoff[0] + time.Second)
	r.tick(t.Context())
	if len(c.asked) != 2 {
		t.Fatalf("not retried after %v: %d", groupTopicBackoff[0], len(c.asked))
	}
	now = now.Add(groupTopicBackoff[0] + time.Second)
	r.tick(t.Context())
	if len(c.asked) != 2 {
		t.Fatalf("the second retry came after %v, want it to wait %v", groupTopicBackoff[0], groupTopicBackoff[1])
	}
	now = now.Add(groupTopicBackoff[1])
	c.fail[1] = false
	r.tick(t.Context())
	if len(c.asked) != 3 {
		t.Fatalf("not retried after %v: %d", groupTopicBackoff[1], len(c.asked))
	}
}

// blindRig is a simulated device whose HTTP port refuses everything, behind a real broker, bus and command
// channel, with a router and the real facts over an in-memory inventory (a second device is there to be
// "another device with a topic").
type blindRig struct {
	st     *gtStore
	router *transport.Router
	facts  *TransportFacts
	dev    simulator.Device
}

func newBlindRig(t *testing.T) *blindRig {
	t.Helper()
	br, err := simulator.StartBroker()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = br.Close() }) // test teardown
	fleet, err := simulator.NewFleetWith(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures", MQTTURL: br.URL(), TelePeriod: time.Hour}, simulator.Extra{TimeScale: 0.02})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() }) // test teardown
	dev, err := fleet.Add(t.Context(), simulator.DeviceSpec{Name: "blind", Profile: simulator.ProfileESP32, Version: "15.1.0",
		Faults: []simulator.Fault{{Kind: simulator.FaultHTTPUnreachable}}})
	if err != nil {
		t.Fatal(err)
	}
	bus, err := mqttbus.Connect(t.Context(), mqttbus.Options{BrokerURL: br.URL(), ClientID: "gt-test"})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); !bus.Connected(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("bus never connected")
		}
	}
	ch := mqttcmd.New(bus, mqttcmd.Options{Gap: 5 * time.Millisecond})
	t.Cleanup(ch.Close)
	host, ps, _ := net.SplitHostPort(dev.Addr())
	port, _ := strconv.Atoi(ps)
	st := &gtStore{now: time.Now, devs: []model.Device{
		{ID: 1, Topic: dev.Topic(), Address: host, Port: port, MQTT: model.MQTTOurs},
		{ID: 2, Topic: "other", Address: "192.0.2.2", MQTT: model.MQTTOurs, GroupTopic: "tasmotas", GroupTopicReadAt: time.Now()},
	}}
	router := transport.NewRouter(transport.Options{HTTP: tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: loopbackNets()}), Devices: st})
	facts := &TransportFacts{Devices: st, Health: fakeFactsHealth{1: {LWT: "Online"}}, Memory: router.Memory(), BusConnected: bus.Connected,
		FullTopic: func() string { return "%prefix%/%topic%/" }, FirmwareBaseURL: func() string { return "" },
		BrokerPassword: func() string { return "" }, Now: time.Now}
	router.SetFacts(facts)
	router.SetMQTT(ch)
	return &blindRig{st: st, router: router, facts: facts, dev: dev}
}

// Why (#279): after an upgrade every device has an unread GroupTopic, which blocks MQTT writes
// everywhere. The reader reads the one HTTP cannot reach over the MQTT channel, and the facts of
// another device then stop reporting unread group topics.
func TestGroupTopicReaderClearsOthersGroupUnread(t *testing.T) {
	g := newBlindRig(t)
	f, _ := g.facts.Facts(t.Context(), 2)
	if f.MQTT.OthersGroupUnread != 1 {
		t.Fatalf("OthersGroupUnread = %d before the read", f.MQTT.OthersGroupUnread)
	}
	r := newGroupTopicReader(g.st.list, online, g.router, g.st, slog.New(slog.DiscardHandler))
	// the device has to come online on the broker first; the first reads may find it offline
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		r.tried = map[model.DeviceID]groupTry{}
		r.tick(t.Context())
		if f, _ := g.facts.Facts(t.Context(), 2); f.MQTT.OthersGroupUnread == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the group topic of the device HTTP cannot reach was never read over MQTT")
		}
	}
	if d, _ := g.st.GetDevice(t.Context(), 1); d.GroupTopic == "" || d.GroupTopicReadAt.IsZero() {
		t.Errorf("stored %+v", d)
	}
}

// Why (#45, #256): "Make compliant" on a device HTTP cannot reach: the SaveData read, the Backlog and
// the SaveData write all go over MQTT, once the group topics are known.
func TestMakeCompliantReachesADeviceOnlyMQTTReaches(t *testing.T) {
	g := newBlindRig(t)
	set := settings.New(settings.Options{Devices: g.st, Client: g.router})
	fix := &policy.Fixer{Client: g.router, Reader: set}
	d, _ := g.st.GetDevice(t.Context(), 1)
	r := newGroupTopicReader(g.st.list, online, g.router, g.st, slog.New(slog.DiscardHandler))
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		r.tried = map[model.DeviceID]groupTry{}
		r.tick(t.Context())
		if d, _ = g.st.GetDevice(t.Context(), 1); d.GroupTopicReadAt.After(time.Time{}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("group topic never read")
		}
	}
	before := len(g.dev.Commands())
	if err := fix.Apply(t.Context(), d, []string{"PowerOnState 2"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := set.ReadKeys(t.Context(), 1, []string{"PowerOnState"})
	if err != nil || got["PowerOnState"] != "2" {
		t.Fatalf("read back %v, %v; the device saw %v", got, err, g.dev.Commands()[before:])
	}
}

func newTestReader(st *gtStore, c *gtClient, now *time.Time, h func(model.DeviceID) model.Health) *groupTopicReader {
	r := newGroupTopicReader(st.list, h, c, st, slog.New(slog.DiscardHandler))
	r.now = func() time.Time { return *now }
	return r
}

// Why: a device nobody has tried yet comes before a retry that is already due, or a failing device would
// starve the new ones.
func TestGroupTopicReaderPrefersADeviceNeverTried(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	st := &gtStore{now: func() time.Time { return now }, devs: []model.Device{{ID: 1, Topic: "a", Address: "192.0.2.1"}}}
	c := &gtClient{fail: map[model.DeviceID]bool{1: true}}
	r := newTestReader(st, c, &now, online)
	r.tick(t.Context())
	now = now.Add(time.Minute) // device 1's retry is due
	st.devs = append(st.devs, model.Device{ID: 2, Topic: "b", Address: "192.0.2.2"})
	r.tick(t.Context())
	if len(c.asked) != 2 || c.asked[1] != 2 {
		t.Fatalf("asked %v, want the new device 2 before the due retry of 1", c.asked)
	}
}

// Why (M1): an offline device cannot answer, so it is not asked; when it comes online after a failed
// attempt it is read at once, not after its backoff (an MQTT-only device whose LWT arrives late).
func TestGroupTopicReaderSkipsOfflineAndReadsAtOnceWhenOnline(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	st := &gtStore{now: func() time.Time { return now }, devs: []model.Device{{ID: 1, Topic: "a", Address: "192.0.2.1"}}}
	c := &gtClient{fail: map[model.DeviceID]bool{1: true}}
	state := model.HealthOffline
	r := newTestReader(st, c, &now, func(model.DeviceID) model.Health { return model.Health{State: state} })
	r.tick(t.Context())
	if len(c.asked) != 0 {
		t.Fatalf("an offline device was asked: %v", c.asked)
	}
	state = model.HealthUnknown // not yet heard of: tried once, and it fails
	r.tick(t.Context())
	if len(c.asked) != 1 {
		t.Fatalf("an unknown device is tried: %v", c.asked)
	}
	now = now.Add(time.Second)
	c.fail[1] = false
	state = model.HealthOnline // its LWT came Online
	r.tick(t.Context())
	if len(c.asked) != 2 {
		t.Fatalf("not read at once when it came online: %v", c.asked)
	}
}

// Why: the backoff caps at 15 minutes, a success forgets the device, a device gone from the inventory is
// forgotten too (the map must not grow with every id ever seen).
func TestGroupTopicReaderBackoffCapSuccessAndPrune(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	st := &gtStore{now: func() time.Time { return now }, devs: []model.Device{{ID: 1, Topic: "a", Address: "192.0.2.1"}, {ID: 2, Topic: "b", Address: "192.0.2.2"}}}
	c := &gtClient{fail: map[model.DeviceID]bool{1: true, 2: true}}
	r := newTestReader(st, c, &now, online)
	for range 8 { // far more failures than steps
		r.tick(t.Context())
		r.tick(t.Context())
		now = now.Add(groupTopicBackoff[len(groupTopicBackoff)-1] + time.Second)
	}
	if got := r.tried[1].wait(); got != 15*time.Minute {
		t.Fatalf("the backoff is %v after many failures, want it capped at 15 minutes", got)
	}
	c.fail[1] = false
	st.devs = st.devs[:1] // device 2 is deleted
	r.tick(t.Context())
	if _, ok := r.tried[1]; ok {
		t.Error("a successful read must remove the device's backoff entry")
	}
	if _, ok := r.tried[2]; ok {
		t.Error("a device that left the inventory must be forgotten")
	}
}

// Why (L4): when nothing needs a read the reader does not list the inventory every tick.
func TestGroupTopicReaderIdlesWhenNothingIsNeeded(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	lists := 0
	st := &gtStore{now: func() time.Time { return now }, devs: []model.Device{{ID: 1, Topic: "a", GroupTopic: "tasmotas", GroupTopicReadAt: now}}}
	c := &gtClient{}
	r := newGroupTopicReader(func(ctx context.Context) ([]model.Device, error) { lists++; return st.list(ctx) }, online, c, st, slog.New(slog.DiscardHandler))
	r.now = func() time.Time { return now }
	for range 5 {
		r.tick(t.Context())
		now = now.Add(groupTopicTick)
	}
	if lists != 1 {
		t.Fatalf("listed %d times in 5 ticks", lists)
	}
	now = now.Add(groupTopicIdle)
	r.tick(t.Context())
	if lists != 2 {
		t.Fatalf("not listed again after the idle time: %d", lists)
	}
}

func TestGroupTopicReaderRunReturnsWhenCancelled(t *testing.T) {
	st := &gtStore{now: time.Now}
	r := newGroupTopicReader(st.list, online, &gtClient{}, st, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { r.run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return after the context ended")
	}
}
