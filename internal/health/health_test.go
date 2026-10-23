package health

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// ---- fakes ----

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}
func (c *fakeClock) After(time.Duration) <-chan time.Time { return make(chan time.Time) }

type fakeStore struct {
	mu     sync.Mutex
	devs   map[model.DeviceID]model.Device
	events []store.HealthEvent
	seen   map[model.DeviceID]model.HealthState
	ver    int64 // DataVersion: bumped by foreignCommit
	lists  int   // ListDevices calls
}

// foreignCommit adds a device the way another process would: no hub event.
func (s *fakeStore) foreignCommit(d model.Device) {
	s.mu.Lock()
	s.devs[d.ID] = d
	s.ver++
	s.mu.Unlock()
}

func (s *fakeStore) DataVersion(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ver, nil
}

func newStore(devs ...model.Device) *fakeStore {
	s := &fakeStore{devs: map[model.DeviceID]model.Device{}, seen: map[model.DeviceID]model.HealthState{}}
	for _, d := range devs {
		s.devs[d.ID] = d
	}
	return s
}

func (s *fakeStore) ListDevices(context.Context) ([]model.Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lists++
	var out []model.Device
	for _, d := range s.devs {
		out = append(out, d)
	}
	return out, nil
}
func (s *fakeStore) GetDevice(_ context.Context, id model.DeviceID) (model.Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devs[id]
	if !ok {
		return model.Device{}, store.ErrNotFound
	}
	return d, nil
}
func (s *fakeStore) remove(id model.DeviceID) {
	s.mu.Lock()
	delete(s.devs, id)
	s.mu.Unlock()
}
func (s *fakeStore) GetDeviceByMAC(context.Context, string) (model.Device, error) {
	return model.Device{}, store.ErrNotFound
}
func (s *fakeStore) GetDeviceByTopic(context.Context, string) (model.Device, error) {
	return model.Device{}, store.ErrNotFound
}
func (s *fakeStore) CreateDevice(context.Context, model.Device) (model.DeviceID, error) {
	return 0, nil
}
func (s *fakeStore) UpdateDevice(context.Context, model.Device) error { return nil }
func (s *fakeStore) UpdateIdentity(context.Context, model.DeviceID, model.Identity) error {
	return nil
}
func (s *fakeStore) DeleteDevice(context.Context, model.DeviceID) error { return nil }
func (s *fakeStore) SetLastSeen(_ context.Context, id model.DeviceID, st model.HealthState, _ time.Time) error {
	s.mu.Lock()
	s.seen[id] = st
	s.mu.Unlock()
	return nil
}
func (s *fakeStore) AppendHealthEvent(_ context.Context, e store.HealthEvent) error {
	s.mu.Lock()
	s.events = append(s.events, e)
	s.mu.Unlock()
	return nil
}
func (s *fakeStore) ListHealthEvents(context.Context, model.DeviceID, int) ([]store.HealthEvent, error) {
	return nil, nil
}
func (s *fakeStore) PruneHealthEvents(context.Context, time.Time) (int64, error) { return 0, nil }
func (s *fakeStore) eventTos(id model.DeviceID) []model.HealthState {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.HealthState
	for _, e := range s.events {
		if e.DeviceID == id {
			out = append(out, e.To)
		}
	}
	return out
}

// fakeClient answers by device address. It fails the test on any command
// that automated health code must never send.
type fakeClient struct {
	t *testing.T

	mu       sync.Mutex
	handler  func(addr, cmnd string) (json.RawMessage, error)
	cmds     []string
	inFlight int
	maxSeen  int
	block    chan struct{} // when non-nil, Command waits on it
}

const sts11 = `{"StatusSTS":{"Time":"2026-10-18T18:00:00","UptimeSec":4210,"Wifi":{"RSSI":74},"POWER":"ON"}}`

func okClient(t *testing.T) *fakeClient {
	return &fakeClient{t: t, handler: func(_, c string) (json.RawMessage, error) { return json.RawMessage(sts11), nil }}
}

func (c *fakeClient) Command(ctx context.Context, tg tasmota.Target, cmnd string) (json.RawMessage, error) {
	norm := strings.ToLower(strings.TrimSpace(cmnd))
	if norm == "status 0" || norm == "status" || norm == "backlog status 0" {
		c.t.Errorf("forbidden command sent: %q (Status 0 corrupts ESP8266 replies)", cmnd)
	}
	if cmnd != "Status 11" && cmnd != "Upgrade" {
		c.t.Errorf("health sent unexpected command %q", cmnd)
	}
	c.mu.Lock()
	c.cmds = append(c.cmds, cmnd)
	c.inFlight++
	if c.inFlight > c.maxSeen {
		c.maxSeen = c.inFlight
	}
	block := c.block
	h := c.handler
	c.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
		}
	}
	c.mu.Lock()
	c.inFlight--
	c.mu.Unlock()
	return h(addrOf(tg), cmnd)
}

// addrOf extracts the address through the only public door: the SSRF-safe
// target is opaque, so tests identify devices by DeviceID instead.
func addrOf(tg tasmota.Target) string { return idStr(tg.DeviceID()) }

func idStr(id model.DeviceID) string { return string(rune('0' + int(id))) }

func (c *fakeClient) DownloadConfig(context.Context, tasmota.Target) ([]byte, error) {
	c.t.Error("health must not download configs")
	return nil, nil
}
func (c *fakeClient) RootPage(context.Context, tasmota.Target) (tasmota.RootInfo, error) {
	c.t.Error("health must not fetch the root page")
	return tasmota.RootInfo{}, nil
}
func (c *fakeClient) commands() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.cmds...)
}
func (c *fakeClient) set(h func(addr, cmnd string) (json.RawMessage, error)) {
	c.mu.Lock()
	c.handler = h
	c.mu.Unlock()
}

type fakeBus struct {
	mu   sync.Mutex
	subs []func(mqttbus.Message)
}

func (b *fakeBus) Subscribe(_ []mqttbus.Kind, h func(mqttbus.Message)) func() {
	b.mu.Lock()
	b.subs = append(b.subs, h)
	b.mu.Unlock()
	return func() {}
}
func (b *fakeBus) Command(context.Context, string, string, string) error { return nil }
func (b *fakeBus) Connected() bool                                       { return true }
func (b *fakeBus) emit(kind mqttbus.Kind, topic, suffix, payload string, retained bool) {
	b.mu.Lock()
	subs := append([]func(mqttbus.Message){}, b.subs...)
	b.mu.Unlock()
	for _, h := range subs {
		h(mqttbus.Message{Kind: kind, Topic: topic, Suffix: suffix, Payload: []byte(payload), Retained: retained})
	}
}

// ---- harness ----

type harness struct {
	t   *testing.T
	clk *fakeClock
	st  *fakeStore
	cl  *fakeClient
	bus *fakeBus
	hub *events.Hub
	svc *Service
	ctx context.Context
}

func newHarness(t *testing.T, withBus bool, o Options, devs ...model.Device) *harness {
	t.Helper()
	h := &harness{t: t, clk: &fakeClock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}, st: newStore(devs...), cl: okClient(t), hub: events.NewHub(64)}
	var bus mqttbus.Bus
	if withBus {
		h.bus = &fakeBus{}
		bus = h.bus
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.ctx = ctx
	h.svc = New(Deps{Store: h.st, Client: h.cl, Bus: bus, Clock: h.clk, Hub: h.hub, Rand: func() float64 { return 0.5 }}, o)
	return h
}

// step runs one scheduler tick and waits for the polls it started.
func (h *harness) step() {
	h.t.Helper()
	h.svc.tick(h.ctx)
	h.svc.wg.Wait()
}

func (h *harness) advance(d time.Duration) { h.clk.Advance(d) }

func (h *harness) state(id model.DeviceID) model.HealthState { return h.svc.Get(id).State }

func (h *harness) want(id model.DeviceID, st model.HealthState) {
	h.t.Helper()
	if got := h.state(id); got != st {
		h.t.Fatalf("device %d: state %s, want %s", id, got, st)
	}
}

// listen registers the message handler the way Run does, without the loop.
func (h *harness) listen() {
	h.svc.reload(h.ctx, h.clk.Now())
	h.svc.d.Bus.Subscribe(nil, h.svc.onMessage)
}

func httpDev(id model.DeviceID) model.Device {
	return model.Device{ID: id, Name: "plug" + idStr(id), Address: "172.31.13.1", MQTT: model.MQTTNone}
}
func mqttDev(id model.DeviceID, topic string) model.Device {
	return model.Device{ID: id, Name: topic, Address: "172.31.13.2", Topic: topic, MQTT: model.MQTTOurs}
}

var errDown = tasmota.ErrUnreachable

func failAll(h *harness, err error) {
	h.cl.set(func(string, string) (json.RawMessage, error) { return nil, err })
}

// ---- HTTP-only devices ----

// HTTP-only devices are first-class: no broker, no topic, still online and
// never "degraded" or "stale" for lack of MQTT (TasmoAdmin mqtt_expected lesson).
func TestHTTPOnlyDeviceIsOnlineWithoutBus(t *testing.T) {
	h := newHarness(t, false, Options{}, httpDev(1))
	h.svc.reload(h.ctx, h.clk.Now())
	h.want(1, model.HealthUnknown) // nothing observed yet
	h.step()
	h.want(1, model.HealthOnline)
	g := h.svc.Get(1)
	if g.Source != model.SourceHTTP || g.UptimeSec != 4210 || g.RSSI != 74 || g.Power["POWER"] != "ON" || g.LastSeen.IsZero() {
		t.Fatalf("health = %+v", g)
	}
	// Why: the devices table shows each channel's last contact; an HTTP-only device has none on MQTT.
	if !g.SeenHTTP.Equal(g.LastSeen) || !g.SeenMQTT.IsZero() {
		t.Fatalf("per-channel last seen: http %v mqtt %v, last seen %v", g.SeenHTTP, g.SeenMQTT, g.LastSeen)
	}
	// Hours pass with polls succeeding: the state stays online, never stale.
	for i := 0; i < 50; i++ {
		h.advance(6 * time.Minute)
		h.step()
		h.want(1, model.HealthOnline)
	}
}

func TestHTTPOnlyDeviceWithTopicIgnoredByMQTTUntilItSpeaks(t *testing.T) {
	// MQTT capability "other": MqttHost points to a foreign broker. Silence on
	// our broker must not matter.
	d := mqttDev(1, "foreign")
	d.MQTT = model.MQTTOtherBroker
	h := newHarness(t, true, Options{}, d)
	h.step()
	h.want(1, model.HealthOnline)
	h.advance(2 * time.Hour)
	h.step()
	h.want(1, model.HealthOnline)
	if h.svc.Get(1).Source != model.SourceHTTP {
		t.Fatal("source should be http")
	}
}

// A minimal-firmware device knows no Status 11; the Upgrade probe proves it is
// reachable. It is marked minimal, not offline.
func TestMinimalDeviceIsReachableAndMarked(t *testing.T) {
	h := newHarness(t, false, Options{}, httpDev(1))
	h.cl.set(func(_, c string) (json.RawMessage, error) {
		if c == "Status 11" {
			return nil, tasmota.ErrUnknownCommand
		}
		return json.RawMessage(`{"Upgrade":"1 or >16.0.0 to upgrade"}`), nil
	})
	h.step()
	h.want(1, model.HealthOnline)
	if !h.svc.Minimal(1) {
		t.Fatal("minimal flag not set")
	}
	if got := h.cl.commands(); len(got) != 2 || got[0] != "Status 11" || got[1] != "Upgrade" {
		t.Fatalf("commands = %v", got)
	}
	// After the device is upgraded the flag clears.
	h.cl.set(func(string, string) (json.RawMessage, error) { return json.RawMessage(sts11), nil })
	h.advance(10 * time.Minute)
	h.step()
	if h.svc.Minimal(1) {
		t.Fatal("minimal flag still set after Status 11 works again")
	}
}

func TestOfflineAfterConsecutiveFailures(t *testing.T) {
	h := newHarness(t, false, Options{}, httpDev(1))
	h.step()
	h.want(1, model.HealthOnline)
	failAll(h, errDown)
	for i := 1; i <= 2; i++ {
		h.advance(6 * time.Minute)
		h.step()
		h.want(1, model.HealthOnline) // one or two lost polls are not an outage
	}
	h.advance(6 * time.Minute)
	h.step()
	h.want(1, model.HealthOffline)
	// Recovery.
	h.cl.set(func(string, string) (json.RawMessage, error) { return json.RawMessage(sts11), nil })
	h.advance(31 * time.Minute)
	h.step()
	h.want(1, model.HealthOnline)
}

// Polling an unreachable device every 5 minutes forever is wasteful and the
// ESP8266 web server is fragile; the delay doubles and stops at 30 minutes.
func TestOfflineBackoffGrowsAndCaps(t *testing.T) {
	h := newHarness(t, false, Options{OfflineAfter: 2}, httpDev(1))
	failAll(h, errDown)
	var delays []time.Duration
	for i := 0; i < 8; i++ {
		h.step()
		h.svc.mu.Lock()
		next := h.svc.devs[1].nextPoll
		h.svc.mu.Unlock()
		d := next.Sub(h.clk.Now())
		delays = append(delays, d)
		h.advance(d)
	}
	for i := 1; i < len(delays); i++ {
		if delays[i] < delays[i-1] {
			t.Fatalf("backoff shrank: %v", delays)
		}
	}
	if delays[0] != 5*time.Minute || delays[1] != 10*time.Minute || delays[2] != 20*time.Minute {
		t.Fatalf("expected 5m,10m,20m start (jitter source is neutral), got %v", delays)
	}
	for _, d := range delays {
		if d > 30*time.Minute {
			t.Fatalf("backoff exceeds the 30 minute cap: %v", delays)
		}
	}
	if delays[len(delays)-1] != 30*time.Minute {
		t.Fatalf("backoff did not reach the cap: %v", delays)
	}
}

func TestPollIntervalIsJittered(t *testing.T) {
	devs := []model.Device{httpDev(1)}
	for _, tc := range []struct {
		rnd      float64
		min, max time.Duration
	}{{0, 239 * time.Second, 241 * time.Second}, {0.9999, 359 * time.Second, 361 * time.Second}} {
		h := newHarness(t, false, Options{}, devs...)
		h.svc.d.Rand = func() float64 { return tc.rnd }
		h.step()
		h.svc.mu.Lock()
		d := h.svc.devs[1].nextPoll.Sub(h.clk.Now())
		h.svc.mu.Unlock()
		if d < tc.min || d > tc.max {
			t.Errorf("rand %v: delay %v outside %v..%v (+-20%% of 5m)", tc.rnd, d, tc.min, tc.max)
		}
	}
}

// A device that answers 401 must never look healthy: that is how TasmoAdmin
// ended up showing devices as fine while every command failed.
func TestAuthFailedIsNeverOnline(t *testing.T) {
	h := newHarness(t, false, Options{}, httpDev(1))
	failAll(h, tasmota.ErrAuth)
	h.step()
	h.want(1, model.HealthAuthFailed)
	h.advance(6 * time.Minute)
	h.step()
	h.want(1, model.HealthAuthFailed)
	h.cl.set(func(string, string) (json.RawMessage, error) { return json.RawMessage(sts11), nil })
	h.advance(31 * time.Minute)
	h.step()
	h.want(1, model.HealthOnline)
}

func TestNoPollBeforeIntervalAndNoStatus0(t *testing.T) {
	h := newHarness(t, false, Options{}, httpDev(1))
	h.step()
	h.advance(time.Minute)
	h.step()
	if n := len(h.cl.commands()); n != 1 {
		t.Fatalf("polled %d times within the interval", n)
	}
	// fakeClient already fails the test on Status 0 or any other command.
}

// ESP8266 web servers are single threaded and the home network is small; a
// burst of polls at startup must respect the configured limit.
func TestPollConcurrencyLimit(t *testing.T) {
	var devs []model.Device
	for i := 1; i <= 9; i++ {
		devs = append(devs, httpDev(model.DeviceID(i)))
	}
	h := newHarness(t, false, Options{PollParallel: 3}, devs...)
	h.cl.block = make(chan struct{})
	h.svc.reload(h.ctx, h.clk.Now())
	h.svc.tick(h.ctx)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.cl.mu.Lock()
		n := h.cl.inFlight
		h.cl.mu.Unlock()
		if n == 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // give surplus polls the chance to (wrongly) start
	close(h.cl.block)
	h.svc.wg.Wait()
	if h.cl.maxSeen != 3 {
		t.Fatalf("max in flight = %d, want exactly the limit 3", h.cl.maxSeen)
	}
	if n := len(h.cl.commands()); n != 9 {
		t.Fatalf("only %d of 9 devices polled", n)
	}
}

func TestVanishedDeviceIsForgotten(t *testing.T) {
	h := newHarness(t, false, Options{}, httpDev(1), httpDev(2))
	h.step()
	h.st.remove(2)
	h.advance(time.Minute)
	h.step()
	if _, ok := h.svc.All()[2]; ok {
		t.Fatal("deleted device still tracked")
	}
	if h.state(2) != model.HealthUnknown {
		t.Fatal("unknown device should read as unknown")
	}
}

// ---- MQTT devices ----

func TestMQTTDeviceRules(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "kitchen-spotlight"))
	h.listen()
	h.want(1, model.HealthUnknown)

	h.bus.emit(mqttbus.KindLWT, "kitchen-spotlight", "LWT", "Online", false)
	h.bus.emit(mqttbus.KindState, "kitchen-spotlight", "STATE", `{"UptimeSec":1}`, false)
	h.want(1, model.HealthOnline)
	g := h.svc.Get(1)
	if g.Source != model.SourceMQTT || g.LWT != "Online" {
		t.Fatalf("health = %+v", g)
	}

	// Fresh telemetry: no HTTP traffic at all.
	h.advance(5 * time.Minute)
	h.step()
	if n := len(h.cl.commands()); n != 0 {
		t.Fatalf("MQTT device polled while fresh: %v", h.cl.commands())
	}
	h.bus.emit(mqttbus.KindState, "kitchen-spotlight", "STATE", `{"UptimeSec":99,"POWER":"OFF","Wifi":{"RSSI":61}}`, false)
	g = h.svc.Get(1)
	if g.UptimeSec != 99 || g.RSSI != 61 || g.Power["POWER"] != "OFF" {
		t.Fatalf("STATE not applied: %+v", g)
	}

	// Silence past StaleAfter: stale, and an HTTP poll is triggered.
	failAll(h, errDown)
	h.advance(11*time.Minute + time.Second)
	h.step()
	h.want(1, model.HealthStale)
	if n := len(h.cl.commands()); n != 1 {
		t.Fatalf("stale device should be polled once, got %v", h.cl.commands())
	}
	// Repeated failed polls: offline.
	for i := 0; i < 2; i++ {
		h.advance(6 * time.Minute)
		h.step()
	}
	h.want(1, model.HealthOffline)
	// Telemetry comes back.
	h.bus.emit(mqttbus.KindState, "kitchen-spotlight", "STATE", `{"UptimeSec":5}`, false)
	h.want(1, model.HealthOnline)
}

func TestStaleDeviceRecoversByPoll(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Online", false)
	h.advance(12 * time.Minute)
	h.step() // polls, device answers
	h.want(1, model.HealthOnline)
	if h.svc.Get(1).Source != model.SourceHTTP {
		t.Fatal("recovery came from the poll, source should be http")
	}
}

func TestLWTOfflineIsOfflineAtOnceAndNotPolled(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Online", false)
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Offline", false)
	h.want(1, model.HealthOffline)
	h.advance(time.Hour)
	h.step()
	if n := len(h.cl.commands()); n != 0 {
		t.Fatalf("offline-by-LWT device was polled: %v", h.cl.commands())
	}
}

// After a restart of tasmofleet the broker replays the retained LWT; a device
// that died while we were down must be offline immediately, not unknown.
func TestRetainedLWTOfflineAtStartup(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Offline", true)
	h.want(1, model.HealthOffline)
}

func TestRetainedTelemetryDoesNotCountAsFresh(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindState, "a", "STATE", `{"UptimeSec":1}`, true)
	h.want(1, model.HealthUnknown)
}

func TestMQTTDeviceNeverHeardIsPolledAfterGrace(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.step()
	h.want(1, model.HealthUnknown)
	if len(h.cl.commands()) != 0 {
		t.Fatal("polled during the startup grace period")
	}
	h.advance(11*time.Minute + time.Second)
	h.step()
	h.want(1, model.HealthOnline) // the poll succeeded
}

func TestUnknownTopicIgnored(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "someone-else", "LWT", "Online", false)
	h.want(1, model.HealthUnknown)
}

// Capability detection (spec 6.4): the first message from a device's topic on
// our broker turns it into an MQTT device, reported once to inventory.
func TestMQTTCapabilityCallbackFiresOnce(t *testing.T) {
	d := mqttDev(1, "a")
	d.MQTT = model.MQTTOtherBroker
	h := newHarness(t, true, Options{}, d)
	var calls atomic.Int32
	var gotID atomic.Int64
	h.svc.d.OnMQTTSeen = func(id model.DeviceID, topic string) {
		if topic != "a" {
			t.Errorf("topic %q", topic)
		}
		gotID.Store(int64(id))
		calls.Add(1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = h.svc.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitUntil(t, func() bool { h.bus.mu.Lock(); defer h.bus.mu.Unlock(); return len(h.bus.subs) == 1 })

	h.bus.emit(mqttbus.KindState, "a", "STATE", `{"UptimeSec":1}`, false)
	h.bus.emit(mqttbus.KindState, "a", "STATE", `{"UptimeSec":2}`, false)
	waitUntil(t, func() bool { return calls.Load() == 1 })
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != 1 || gotID.Load() != 1 {
		t.Fatalf("callback calls = %d id = %d", calls.Load(), gotID.Load())
	}
}

func TestPowerFromStatTopic(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindPower, "a", "POWER2", "on", false)
	h.bus.emit(mqttbus.KindResult, "a", "RESULT", `{"POWER1":"OFF"}`, false)
	g := h.svc.Get(1)
	if g.Power["POWER2"] != "ON" || g.Power["POWER1"] != "OFF" {
		t.Fatalf("power = %v", g.Power)
	}
	h.want(1, model.HealthOnline)
}

func TestGarbagePayloadStillSignOfLife(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindState, "a", "STATE", `{"truncated`, false)
	h.want(1, model.HealthOnline)
}

// ---- persistence and events ----

// The health_events table is a history of changes; writing every poll would
// flood it (spec 10: only transitions are persisted).
func TestOnlyTransitionsPersisted(t *testing.T) {
	h := newHarness(t, false, Options{}, httpDev(1))
	h.step()
	for i := 0; i < 5; i++ {
		h.advance(6 * time.Minute)
		h.step()
	}
	failAll(h, errDown)
	for i := 0; i < 3; i++ {
		h.advance(6 * time.Minute)
		h.step()
	}
	got := h.st.eventTos(1)
	if len(got) != 1 || got[0] != model.HealthOffline {
		t.Fatalf("persisted transitions = %v, want only the change to offline", got)
	}
	h.st.mu.Lock()
	last := h.st.seen[1]
	first := h.st.events[0]
	h.st.mu.Unlock()
	if last != model.HealthOffline {
		t.Fatalf("devices.health_state = %s", last)
	}
	if first.From != model.HealthOnline || first.Source != model.SourceHTTP {
		t.Fatalf("first event = %+v", first)
	}
}

// Why: every app restart forgot every device's state, so each one logged "Unknown -> Online" in
// its State changes (noise, not a change of the device). The first observation after the start
// is the baseline: the summary (devices.health_state) is written, but no event. Decision for the
// feedback of 2026-10-23, item 9; documented in the spec (section 10).
func TestFirstObservationAfterStartIsABaselineNotAChange(t *testing.T) {
	h := newHarness(t, false, Options{}, httpDev(1))
	h.step()
	if got := h.st.eventTos(1); len(got) != 0 {
		t.Fatalf("the first observation was logged as a change: %v", got)
	}
	h.st.mu.Lock()
	seen := h.st.seen[1]
	h.st.mu.Unlock()
	if seen != model.HealthOnline {
		t.Fatalf("devices.health_state = %q, the baseline must still be stored", seen)
	}
}

func TestHealthEventsPublishedToHub(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	ch, cancel := h.hub.Subscribe(events.TopicHealth)
	defer cancel()
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Online", false)
	select {
	case e := <-ch:
		hl, ok := e.Payload.(model.Health)
		if !ok || hl.DeviceID != 1 || hl.State != model.HealthOnline {
			t.Fatalf("payload = %+v", e.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("no health event")
	}
	// Data-only updates (power, RSSI) must reach live views too.
	h.bus.emit(mqttbus.KindPower, "a", "POWER", "ON", false)
	select {
	case e := <-ch:
		if e.Payload.(model.Health).Power["POWER"] != "ON" {
			t.Fatalf("payload = %+v", e.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("no event for power update")
	}
}

// The tracker must also work without hub and bus (spec: app runs without MQTT).
func TestRunWithoutBusOrHub(t *testing.T) {
	clk := &fakeClock{now: time.Now()}
	st := newStore(httpDev(1))
	svc := New(Deps{Store: st, Client: okClient(t), Clock: clk}, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()
	waitUntil(t, func() bool { return svc.Get(1).State == model.HealthOnline })
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

type switchedBus struct {
	fakeBus
	on bool
}

func (b *switchedBus) Enabled() bool { return b.on }

// Why: MQTT can be turned off on the settings page while the server runs. A device
// that reports to our broker must then be polled over HTTP again instead of waiting
// for messages that cannot come.
func TestDisabledBusFallsBackToHTTPPolling(t *testing.T) {
	h := newHarness(t, false, Options{}, mqttDev(1, "a"))
	sb := &switchedBus{on: true}
	h.svc.d.Bus = sb
	ds := &devState{topic: "a", cap: model.MQTTOurs}
	if !h.svc.mqttMode(ds) {
		t.Fatal("an enabled bus means MQTT mode")
	}
	sb.on = false
	if h.svc.mqttMode(ds) {
		t.Fatal("a disabled bus must put the device back on HTTP polling")
	}
}

// Why: the poll interval and the stale threshold can change on the settings page
// while the server runs.
func TestSetIntervalsChangesThePollingRule(t *testing.T) {
	h := newHarness(t, false, Options{PollInterval: time.Minute, StaleAfter: time.Minute}, mqttDev(1, "a"))
	if h.svc.pollInterval() != time.Minute || h.svc.staleAfter() != time.Minute {
		t.Fatal("start values")
	}
	h.svc.SetIntervals(10*time.Minute, 2*time.Minute)
	if h.svc.pollInterval() != 2*time.Minute || h.svc.staleAfter() != 10*time.Minute {
		t.Fatalf("after SetIntervals: %v %v", h.svc.pollInterval(), h.svc.staleAfter())
	}
	h.svc.SetIntervals(0, -1)
	if h.svc.pollInterval() != 2*time.Minute {
		t.Fatal("non-positive values are ignored")
	}
}

// ---- state snapshot after a restart (v0.3.0 bug) ----

// After a restart only the retained LWT arrives; the first tele/STATE would
// follow up to a TelePeriod (300 s) later. Until then the UI showed the device
// online with no uptime, Wi-Fi or power. The tracker must fetch a snapshot
// itself instead of waiting.
func TestLWTOnlyDeviceGetsSnapshotWithoutWaitingForTele(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Online", true)
	h.want(1, model.HealthOnline)
	if g := h.svc.Get(1); g.UptimeSec != 0 || g.Power != nil {
		t.Fatalf("precondition: no snapshot yet, got %+v", g)
	}
	h.step() // far less than TelePeriod
	g := h.svc.Get(1)
	if g.UptimeSec != 4210 || g.RSSI != 74 || g.Power["POWER"] != "ON" {
		t.Fatalf("snapshot not fetched: %+v", g)
	}
	// One fetch is enough; a device with a snapshot is not polled again.
	h.advance(snapshotRetry + time.Second)
	h.step()
	if n := len(h.cl.commands()); n != 1 {
		t.Fatalf("commands = %v, want exactly one Status 11", h.cl.commands())
	}
}

// A snapshot arriving over MQTT first means no HTTP traffic at all.
func TestStateOverMQTTSuppressesSnapshotFetch(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Online", true)
	h.bus.emit(mqttbus.KindState, "a", "STATE", `{"UptimeSec":5,"POWER":"OFF"}`, false)
	h.step()
	if n := len(h.cl.commands()); n != 0 {
		t.Fatalf("fetched despite STATE: %v", h.cl.commands())
	}
}

// An offline-by-LWT device must not be probed for a snapshot.
func TestOfflineLWTDeviceGetsNoSnapshotFetch(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Offline", true)
	h.step()
	if n := len(h.cl.commands()); n != 0 {
		t.Fatalf("offline device probed: %v", h.cl.commands())
	}
}

// Many devices come back at once after a restart; snapshot fetches must be
// paced (at most PollParallel started per tick) and never exceed the
// concurrency limit.
func TestSnapshotFetchesAreRateLimited(t *testing.T) {
	var devs []model.Device
	for i := 1; i <= 9; i++ {
		devs = append(devs, mqttDev(model.DeviceID(i), "t"+idStr(model.DeviceID(i))))
	}
	h := newHarness(t, true, Options{PollParallel: 3}, devs...)
	h.listen()
	for i := 1; i <= 9; i++ {
		h.bus.emit(mqttbus.KindLWT, "t"+idStr(model.DeviceID(i)), "LWT", "Online", true)
	}
	h.step()
	if n := len(h.cl.commands()); n != 3 {
		t.Fatalf("first tick fetched %d snapshots, want the limit 3", n)
	}
	h.advance(5 * time.Second)
	h.step()
	h.advance(5 * time.Second)
	h.step()
	if n := len(h.cl.commands()); n != 9 {
		t.Fatalf("after three ticks %d of 9 fetched", n)
	}
	if h.cl.maxSeen > 3 {
		t.Fatalf("max in flight %d exceeds limit 3", h.cl.maxSeen)
	}
}

// A device that answers without usable STATE data (minimal firmware) must not
// be hammered every tick.
func TestSnapshotFetchRetriesAreSpaced(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.cl.set(func(string, string) (json.RawMessage, error) {
		return json.RawMessage(`{"StatusSTS":{"Time":"x"}}`), nil // no UptimeSec
	})
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Online", true)
	h.step()
	h.advance(5 * time.Second)
	h.step()
	if n := len(h.cl.commands()); n != 1 {
		t.Fatalf("retried within %v: %v", snapshotRetry, h.cl.commands())
	}
}

// HTTP-only devices have no tele stream at all: the first tick fetches them,
// also when they are added while the service runs.
func TestHTTPOnlyDeviceSnapshotAtStartAndWhenAdded(t *testing.T) {
	h := newHarness(t, false, Options{}, httpDev(1))
	h.step()
	if g := h.svc.Get(1); g.UptimeSec != 4210 || g.Power["POWER"] != "ON" {
		t.Fatalf("no snapshot after start: %+v", g)
	}
	h.st.mu.Lock()
	h.st.devs[2] = httpDev(2)
	h.st.mu.Unlock()
	h.svc.mu.Lock()
	h.svc.lastReload = time.Time{} // what the inventory event does
	h.svc.mu.Unlock()
	h.step()
	if g := h.svc.Get(2); g.UptimeSec != 4210 {
		t.Fatalf("added device not fetched at once: %+v", g)
	}
}

// A device added by another process (the CLI import, while the server runs)
// raises no hub event. Why: the e2e suite imports after starting the server and
// users do the same; the device must show live health within about one tick,
// not after the 30 s full reload.
func TestDeviceAddedByAnotherProcessIsPickedUpWithinOneTick(t *testing.T) {
	h := newHarness(t, false, Options{})
	h.step() // empty inventory: first reload
	h.st.foreignCommit(httpDev(1))
	h.advance(tickEvery) // the next scheduler tick, far below reloadEvery
	h.step()             // reload, then the immediate first poll
	h.want(1, model.HealthOnline)
	if g := h.svc.Get(1); g.Power["POWER"] != "ON" {
		t.Fatalf("first state missing right after the pickup: %+v", g)
	}
}

// Why: change detection must be cheap; without a commit the inventory is read
// only by the 30 s backstop, not on every 5 s tick.
func TestNoInventoryReloadWithoutAChange(t *testing.T) {
	h := newHarness(t, false, Options{}, httpDev(1))
	h.step()
	base := h.st.lists
	for i := 0; i < 5; i++ {
		h.advance(tickEvery)
		h.step()
	}
	if got := h.st.lists - base; got != 0 {
		t.Fatalf("%d inventory reloads within 25 s without a change", got)
	}
	h.advance(reloadEvery)
	h.step()
	if h.st.lists-base != 1 {
		t.Fatalf("the backstop reload did not happen once, got %d", h.st.lists-base)
	}
}
