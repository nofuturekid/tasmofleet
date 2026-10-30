package mqttbus

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"
)

type capture struct {
	mu   sync.Mutex
	msgs []Message
}

func (c *capture) add(m Message) {
	c.mu.Lock()
	c.msgs = append(c.msgs, m)
	c.mu.Unlock()
}

func (c *capture) snapshot() []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Message(nil), c.msgs...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	waitForWithin(t, what, 5*time.Second, cond)
}

func waitForWithin(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

var allKinds = []Kind{KindLWT, KindState, KindSensor, KindResult, KindStatus, KindPower, KindDiscovery, KindUpgrade, KindInfo}

func connect(t *testing.T, b *testBroker, o Options) Bus {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	// Cleanups run last-in first-out: this runs before the broker is stopped.
	// Canceling makes Connect's goroutine disconnect asynchronously; wait for it.
	t.Cleanup(func() {
		cancel()
		b.waitNoClients(t)
	})
	o.BrokerURL = b.url()
	bus, err := Connect(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "connection", bus.Connected)
	// Messages published to the broker after connect() returns must reach the
	// bus, so wait until the broker has registered its subscriptions.
	codec, _ := NewTopicCodec(o.FullTopic, o.Prefixes)
	b.waitSubscribed(t, len(codec.Filters()))
	return bus
}

// A recorded production capture is the best proof that the decoder handles
// what a real fleet publishes: every line must come out with the kind and
// device topic the topic structure dictates, and nothing may be lost.
func TestReplayCapture(t *testing.T) {
	f, err := os.Open("../../testdata/fixtures/mqtt/capture.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }() // read-only file
	type line struct{ topic, payload string }
	var lines []line
	wantByKind := map[Kind]int{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), "|", 4)
		if len(parts) != 4 {
			t.Fatalf("bad capture line %q", sc.Text())
		}
		topic := parts[2]
		lines = append(lines, line{topic, parts[3]})
		// Expected kind derived independently from the raw topic text.
		switch {
		case strings.HasPrefix(topic, "tasmota/discovery/") && strings.HasSuffix(topic, "/config"):
			wantByKind[KindDiscovery]++
		case strings.HasPrefix(topic, "tele/") && strings.HasSuffix(topic, "/LWT"):
			wantByKind[KindLWT]++
		case strings.HasPrefix(topic, "tele/") && strings.HasSuffix(topic, "/STATE"):
			wantByKind[KindState]++
		case strings.HasPrefix(topic, "tele/") && strings.HasSuffix(topic, "/SENSOR"):
			wantByKind[KindSensor]++
		case strings.HasPrefix(topic, "stat/") && strings.HasSuffix(topic, "/RESULT"):
			wantByKind[KindResult]++
		case strings.HasPrefix(topic, "stat/") && strings.Contains(topic[strings.LastIndex(topic, "/"):], "/STATUS"):
			wantByKind[KindStatus]++
		case strings.HasPrefix(topic, "stat/") && strings.Contains(topic[strings.LastIndex(topic, "/"):], "/POWER"):
			wantByKind[KindPower]++
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if wantByKind[KindLWT] == 0 || wantByKind[KindStatus] == 0 || wantByKind[KindDiscovery] == 0 {
		t.Fatalf("capture fixture changed shape: %v", wantByKind)
	}

	b := newTestBroker(t)
	bus := connect(t, b, Options{})
	var c capture
	bus.Subscribe(allKinds, c.add)
	total := 0
	for _, w := range wantByKind {
		total += w
	}
	for _, l := range lines {
		b.publish(l.topic, l.payload, false)
	}
	waitFor(t, "all decodable messages", func() bool { return len(c.snapshot()) >= total })
	time.Sleep(100 * time.Millisecond) // let any surplus show up
	got := map[Kind]int{}
	for _, m := range c.snapshot() {
		got[m.Kind]++
		if m.Topic == "" || m.Suffix == "" || m.At.IsZero() {
			t.Fatalf("incomplete message %+v", m)
		}
		if m.Kind == KindStatus && !strings.HasPrefix(m.Suffix, "STATUS") {
			t.Fatalf("status suffix %q", m.Suffix)
		}
		if m.Kind == KindDiscovery && len(m.Topic) != 12 {
			t.Fatalf("discovery topic should be the MAC, got %q", m.Topic)
		}
	}
	for k, w := range wantByKind {
		if got[k] != w {
			t.Errorf("kind %s: got %d messages, want %d", k, got[k], w)
		}
	}
	if len(c.snapshot()) != total {
		t.Errorf("got %d messages, want %d", len(c.snapshot()), total)
	}
}

// After a tasmofleet restart the broker hands over the retained LWT of every
// device immediately; health relies on that to avoid a window of "unknown".
func TestRetainedLWTDeliveredOnSubscribe(t *testing.T) {
	b := newTestBroker(t)
	b.publish("tele/kitchen-downlight/LWT", "Online", true)
	bus := connect(t, b, Options{})
	var c capture
	bus.Subscribe([]Kind{KindLWT}, c.add)
	// Subscribing happens in OnConnect, which may race with our Subscribe call.
	waitFor(t, "retained LWT", func() bool { return len(c.snapshot()) > 0 })
	m := c.snapshot()[0]
	if !m.Retained || m.Kind != KindLWT || m.Topic != "kitchen-downlight" || string(m.Payload) != "Online" {
		t.Fatalf("got %+v", m)
	}
}

func TestSubscribeFiltersByKindAndUnsubscribes(t *testing.T) {
	b := newTestBroker(t)
	bus := connect(t, b, Options{})
	var lwt, state capture
	unLWT := bus.Subscribe([]Kind{KindLWT}, lwt.add)
	bus.Subscribe([]Kind{KindState}, state.add)
	b.publish("tele/a/LWT", "Online", false)
	b.publish("tele/a/STATE", `{"UptimeSec":1}`, false)
	waitFor(t, "both", func() bool { return len(lwt.snapshot()) == 1 && len(state.snapshot()) == 1 })
	unLWT()
	b.publish("tele/a/LWT", "Offline", false)
	b.publish("tele/a/STATE", `{"UptimeSec":2}`, false)
	waitFor(t, "second state", func() bool { return len(state.snapshot()) == 2 })
	if n := len(lwt.snapshot()); n != 1 {
		t.Fatalf("handler called %d times after unsubscribe", n)
	}
}

func TestNonDefaultFullTopic(t *testing.T) {
	b := newTestBroker(t)
	bus := connect(t, b, Options{FullTopic: "home/%topic%/%prefix%/"})
	var c capture
	bus.Subscribe(allKinds, c.add)
	b.publish("home/dev1/tele/LWT", "Online", false)
	b.publish("tele/dev1/LWT", "Offline", false) // default layout must not be seen
	waitFor(t, "message", func() bool { return len(c.snapshot()) >= 1 })
	time.Sleep(100 * time.Millisecond)
	got := c.snapshot()
	if len(got) != 1 || got[0].Topic != "dev1" || string(got[0].Payload) != "Online" {
		t.Fatalf("got %+v", got)
	}
}

// Regression: connect() used to return as soon as the connection was open, so
// a non-retained message published right after was lost whenever the broker
// had not yet processed the bus's SUBSCRIBE (flaky on loaded runners).
func TestPublishRightAfterConnectIsDelivered(t *testing.T) {
	b := newTestBrokerDelayed(t, 300*time.Millisecond)
	bus := connect(t, b, Options{})
	var c capture
	bus.Subscribe(allKinds, c.add)
	b.publish("tele/dev1/LWT", "Online", false)
	waitFor(t, "message", func() bool { return len(c.snapshot()) == 1 })
}

// A command is never published retained: a retained command would be replayed to a device that was
// offline when it was sent and run when it comes back. transport.TransportFacts relies on this when
// it lets a broker-confirmed Offline device out of the group-topic check.
func TestCommandPublishesToCmndTopic(t *testing.T) {
	b := newTestBroker(t)
	got := make(chan string, 1)
	retained := make(chan bool, 1)
	if err := b.srv.Subscribe("cmnd/dev1/#", 1, func(_ *mqtt.Client, _ packets.Subscription, pk packets.Packet) {
		got <- pk.TopicName + "=" + string(pk.Payload)
		retained <- pk.FixedHeader.Retain
	}); err != nil {
		t.Fatal(err)
	}
	bus := connect(t, b, Options{})
	if err := bus.Command(context.Background(), "dev1", "Power", "TOGGLE"); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-got:
		if s != "cmnd/dev1/Power=TOGGLE" {
			t.Fatalf("got %q", s)
		}
		if <-retained {
			t.Fatal("the command was published retained")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("command not received")
	}
}

// A topic from the inventory must not be able to widen a publish into a
// wildcard or another device's tree.
func TestCommandRejectsInjection(t *testing.T) {
	b := newTestBroker(t)
	bus := connect(t, b, Options{})
	for _, tc := range [][2]string{{"a/b", "Power"}, {"+", "Power"}, {"#", "Power"}, {"", "Power"}, {"a", "Power/../x"}, {"a", ""}} {
		if err := bus.Command(context.Background(), tc[0], tc[1], "1"); err == nil {
			t.Errorf("Command(%q,%q) accepted", tc[0], tc[1])
		}
	}
}

// MQTT is optional: an unreachable broker must neither fail Connect nor block
// the application start. Commands fail fast and loudly instead.
func TestMissingBrokerIsNotAnError(t *testing.T) {
	b := newTestBroker(t)
	url := b.url()
	b.stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	bus, err := Connect(ctx, Options{BrokerURL: url})
	if err != nil {
		t.Fatalf("Connect failed without broker: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("Connect blocked on an unreachable broker")
	}
	if bus.Connected() {
		t.Fatal("Connected() true without broker")
	}
	if err := bus.Command(ctx, "a", "Power", "1"); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Command error = %v, want ErrNotConnected", err)
	}
}

func TestConnectRejectsBadOptions(t *testing.T) {
	for _, o := range []Options{{BrokerURL: ""}, {BrokerURL: "not a url"}, {BrokerURL: "tcp://127.0.0.1:1", FullTopic: "fixed/"}} {
		if _, err := Connect(context.Background(), o); err == nil {
			t.Errorf("Connect(%+v) succeeded", o)
		}
	}
}

func TestErrorDoesNotLeakPassword(t *testing.T) {
	_, err := Connect(context.Background(), Options{BrokerURL: "tcp://user:s3cret@", Password: "s3cret"})
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("err = %v", err)
	}
}

// Brokers restart (Unraid updates, docker restarts). The bus must come back
// by itself and resubscribe, otherwise every device would go stale until the
// next tasmofleet restart.
func TestReconnectAfterBrokerRestart(t *testing.T) {
	b := newTestBroker(t)
	bus := connect(t, b, Options{})
	var c capture
	bus.Subscribe([]Kind{KindLWT}, c.add)
	b.stop()
	waitFor(t, "disconnect", func() bool { return !bus.Connected() })
	b.start()
	// paho backs off between reconnect attempts (1 s, 2 s, 4 s, ...), so on a
	// loaded CI runner the first successful attempt can come after more than
	// 5 s. 30 s still proves "comes back by itself" without flaking.
	waitForWithin(t, "reconnect", 75*time.Second, bus.Connected) // backoff 1+2+4+8+16+32 s can exceed 30 s under load
	// OnConnect resubscribes asynchronously to Connected(); publish until delivered.
	waitFor(t, "message after reconnect", func() bool {
		b.publish("tele/a/LWT", "Online", false)
		time.Sleep(50 * time.Millisecond)
		return len(c.snapshot()) > 0
	})
}

func TestHandlerPanicDoesNotKillBus(t *testing.T) {
	b := newTestBroker(t)
	bus := connect(t, b, Options{})
	var c capture
	bus.Subscribe([]Kind{KindLWT}, func(Message) { panic("boom") })
	bus.Subscribe([]Kind{KindLWT}, c.add)
	b.publish("tele/a/LWT", "Online", false)
	b.publish("tele/a/LWT", "Offline", false)
	waitFor(t, "both messages", func() bool { return len(c.snapshot()) == 2 })
}

func anyAddr(netip.Addr) bool { return true }

// Why: the system settings page offers "test connection"; it must tell a
// working broker from a dead one and never put the password in its error.
func TestProbe(t *testing.T) {
	b := newTestBroker(t)
	url := b.url()
	if err := Probe(t.Context(), Options{BrokerURL: url, AllowAddr: anyAddr}); err != nil {
		t.Fatalf("working broker: %v", err)
	}
	b.stop()
	err := Probe(t.Context(), Options{BrokerURL: url, Username: "u", Password: "fake-probe-pw", AllowAddr: anyAddr})
	if err == nil {
		t.Fatal("dead broker must fail")
	}
	if strings.Contains(err.Error(), "fake-probe-pw") {
		t.Fatalf("password in error: %v", err)
	}
	if err := Probe(t.Context(), Options{BrokerURL: "::bad", AllowAddr: anyAddr}); err == nil {
		t.Fatal("bad URL must fail")
	}
}

// Why (review F4): without a caller-supplied address rule nothing is tried, and
// the answers for "closed port", "no route" and "timeout" are the same, so the
// test is no port scanner.
func TestProbeFailsClosedAndAnswersCoarsely(t *testing.T) {
	if err := Probe(t.Context(), Options{BrokerURL: "tcp://10.0.0.1:1883"}); err == nil {
		t.Fatal("no AllowAddr must mean no connection")
	}
	b := newTestBroker(t)
	url := b.url()
	b.stop()
	e1 := Probe(t.Context(), Options{BrokerURL: url, AllowAddr: anyAddr})
	if e1 == nil || strings.Contains(e1.Error(), "refused") || strings.Contains(e1.Error(), "dial") {
		t.Fatalf("closed port: %v", e1)
	}
	if got := classifyProbeError("h:1", errors.New("network Error : dial tcp 10.0.0.9:1883: i/o timeout")).Error(); got != unreachable("h:1").Error() {
		t.Fatalf("timeout: %s", got)
	}
	if got := classifyProbeError("h:1", errors.New("not Authorized")).Error(); !strings.Contains(got, "credentials") {
		t.Fatalf("credentials: %s", got)
	}
}

// Why (security review): the credentials of a connection test must go to the
// address that was checked. The name is resolved once; a second lookup that
// answers differently (DNS rebinding) must never be used.
func TestProbeResolvesOnceAndConnectsToThatAddress(t *testing.T) {
	b := newTestBroker(t)
	_, port, _ := strings.Cut(strings.TrimPrefix(b.url(), "tcp://"), ":")
	calls := 0
	old := lookupIP
	t.Cleanup(func() { lookupIP = old })
	lookupIP = func(context.Context, string) ([]net.IPAddr, error) {
		calls++
		if calls == 1 {
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.9")}}, nil
	}
	if err := Probe(t.Context(), Options{BrokerURL: "tcp://broker.example.test:" + port, AllowAddr: anyAddr}); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if calls != 1 {
		t.Fatalf("the name was resolved %d times", calls)
	}
}

func TestProbeAppliesTheCallersAddressRule(t *testing.T) {
	deny := func(netip.Addr) bool { return false }
	for _, host := range []string{"169.254.169.254", "127.0.0.1", "10.0.0.1", "[64:ff9b::a9fe:a9fe]"} {
		if err := Probe(t.Context(), Options{BrokerURL: "tcp://" + host + ":1883", Username: "u", Password: "pw", AllowAddr: deny}); err == nil {
			t.Errorf("%s accepted", host)
		}
	}
	old := lookupIP
	t.Cleanup(func() { lookupIP = old })
	lookupIP = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("169.254.169.254")}}, nil
	}
	if err := Probe(t.Context(), Options{BrokerURL: "tcp://metadata.example.test:1883", AllowAddr: func(ip netip.Addr) bool { return !ip.IsLinkLocalUnicast() }}); err == nil {
		t.Error("a name that resolves to a refused address was accepted")
	}
}

type fakeBus struct {
	mu   sync.Mutex
	subs map[int]func(Message)
	n    int
	up   bool
	cmds []string
}

func newFakeBus() *fakeBus { return &fakeBus{subs: map[int]func(Message){}, up: true} }
func (f *fakeBus) Subscribe(_ []Kind, h func(Message)) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	id := f.n
	f.subs[id] = h
	return func() { f.mu.Lock(); delete(f.subs, id); f.mu.Unlock() }
}
func (f *fakeBus) Command(_ context.Context, topic, c, p string) error {
	f.cmds = append(f.cmds, topic+c+p)
	return nil
}
func (f *fakeBus) CommandTopic(topic, command string) string { return "cmnd/" + topic + "/" + command }

func (f *fakeBus) Connected() bool { return f.up }
func (f *fakeBus) emit(m Message) {
	f.mu.Lock()
	hs := make([]func(Message), 0, len(f.subs))
	for _, h := range f.subs {
		hs = append(hs, h)
	}
	f.mu.Unlock()
	for _, h := range hs {
		h(m)
	}
}

// Why: the broker can be changed on the settings page while the server runs.
// Consumers subscribe once; they must hear the new broker, stop hearing the old
// one, and a disabled switch must say so instead of pretending to be connected.
func TestSwitchMovesSubscriptionsToTheNewConnection(t *testing.T) {
	sw := NewSwitch()
	var got []string
	unsub := sw.Subscribe([]Kind{KindLWT}, func(m Message) { got = append(got, m.Topic) })
	if sw.Enabled() || sw.Connected() || !errors.Is(sw.Command(t.Context(), "t", "Power", "1"), ErrNotConnected) {
		t.Fatal("a disabled switch is not connected")
	}
	a := newFakeBus()
	stopped := 0
	sw.Replace(a, func() { stopped++ })
	a.emit(Message{Topic: "from-a"})
	b := newFakeBus()
	sw.Replace(b, nil)
	if stopped != 1 {
		t.Fatalf("the replaced connection must be stopped: %d", stopped)
	}
	a.emit(Message{Topic: "late-from-a"})
	b.emit(Message{Topic: "from-b"})
	if strings.Join(got, ",") != "from-a,from-b" {
		t.Fatalf("got %v", got)
	}
	if !sw.Enabled() || !sw.Connected() {
		t.Fatal("enabled and connected")
	}
	if err := sw.Command(t.Context(), "t", "Power", "1"); err != nil || len(b.cmds) != 1 {
		t.Fatalf("command went to %v: %v", b.cmds, err)
	}
	sw.Replace(nil, nil)
	if sw.Enabled() {
		t.Fatal("turned off")
	}
	b.emit(Message{Topic: "after-off"})
	unsub()
	sw.Replace(newFakeBus(), nil)
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}

// Why: the live connection obeys the same address rule as the connection test, checked on the
// resolved address right before connect.
func TestConnectHonoursTheAddressRule(t *testing.T) {
	b := newTestBroker(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	bus, err := Connect(ctx, Options{BrokerURL: b.url(), AllowAddr: func(netip.Addr) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if bus.Connected() {
		t.Fatal("a refused address must never connect")
	}
	ok, err := Connect(ctx, Options{BrokerURL: b.url(), AllowAddr: func(netip.Addr) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !ok.Connected() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !ok.Connected() {
		t.Fatal("an allowed address connects")
	}
}

// Why: paho applies the custom dialer (and with it the address rule) to tcp/tls
// only; websocket brokers would bypass the SSRF filter. They are refused outright,
// and nothing may even touch the listener.
func TestWebsocketBrokerIsRefusedWithoutConnecting(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }() // test cleanup
	accepted := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			_ = c.Close() // only the accept matters
			accepted <- struct{}{}
		}
	}()
	for _, scheme := range []string{"ws", "wss"} {
		u := scheme + "://" + ln.Addr().String()
		o := Options{BrokerURL: u, AllowAddr: anyAddr}
		if _, err := Connect(t.Context(), o); err == nil {
			t.Fatalf("Connect must refuse %s brokers", scheme)
		}
		if err := Probe(t.Context(), o); err == nil {
			t.Fatalf("Probe must refuse %s brokers", scheme)
		}
	}
	select {
	case <-accepted:
		t.Fatal("a refused websocket URL must not open a connection")
	case <-time.After(300 * time.Millisecond):
	}
}

// A handler runs while the new connection replays retained messages inside
// Replace. It (or a lock it takes) must be able to use the switch meanwhile,
// otherwise health's lock order inverts against the switch (v0.3.1 hang).
func TestSwitchHandlerMayUseSwitchDuringReplace(t *testing.T) {
	sw := NewSwitch()
	got := make(chan bool, 1)
	sw.Subscribe(nil, func(Message) { got <- sw.Enabled() && sw.Connected() })
	done := make(chan struct{})
	go func() {
		defer close(done)
		sw.Replace(replayingBus{}, nil)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Replace deadlocked: the handler could not use the switch")
	}
	if !<-got {
		t.Fatal("switch not enabled/connected inside the replayed handler")
	}
}

type replayingBus struct{}

func (replayingBus) Subscribe(_ []Kind, h func(Message)) func()            { h(Message{}); return func() {} }
func (replayingBus) Command(context.Context, string, string, string) error { return nil }
func (replayingBus) CommandTopic(topic, command string) string {
	return "cmnd/" + topic + "/" + command
}

func (replayingBus) Connected() bool { return true }

// Why (#272): paho reports the connection open before the OnConnect handler has subscribed. A
// consumer that publishes a command as soon as Connected() is true then loses the device's reply
// (it arrives before the SUBSCRIBE is in place), which showed up as a flaky 10 s timeout. With a
// slow broker the old Connected() was true for the whole subscribe delay; now a reply published
// the moment Connected() turns true must always arrive.
func TestConnectedMeansSubscribed(t *testing.T) {
	b := newTestBrokerDelayed(t, 150*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		b.waitNoClients(t)
	})
	bus, err := Connect(ctx, Options{BrokerURL: b.url()})
	if err != nil {
		t.Fatal(err)
	}
	var got capture
	bus.Subscribe([]Kind{KindResult}, got.add)
	waitFor(t, "connection", bus.Connected)
	b.publish("stat/plug/RESULT", `{"POWER":"ON"}`, false)
	waitFor(t, "the reply published right after Connected() turned true", func() bool { return len(got.snapshot()) == 1 })
}
