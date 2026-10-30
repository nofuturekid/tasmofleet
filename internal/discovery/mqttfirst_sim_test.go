package discovery

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/mqttcmd"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// Why: end to end over a real MQTT connection (embedded broker, simulated
// devices): a device without tasmota/discovery (SetOption19 1, or a build
// without the driver) and a normal one both end up as proposals, the first
// through exactly one Status 5 and never Status 0 (safety rule 2).
func TestSimulatedFleetWithoutDiscoveryConfigIsProposed(t *testing.T) {
	br, err := simulator.StartBroker()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = br.Close() }) // test teardown
	ctx, cancel := context.WithCancel(bg)
	t.Cleanup(cancel)
	fleet, err := simulator.NewFleet(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures", MQTTURL: br.URL()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() }) // test teardown
	plain, err := fleet.Add(ctx, simulator.DeviceSpec{Name: "no-disc", Profile: simulator.ProfileESP8266_4M, Version: "15.1.0", NoDiscovery: true})
	if err != nil {
		t.Fatal(err)
	}
	announced, err := fleet.Add(ctx, simulator.DeviceSpec{Name: "announced", Profile: simulator.ProfileESP32, Version: "15.1.0"})
	if err != nil {
		t.Fatal(err)
	}

	e := newEnv(t, "127.0.0.0/8", nil)
	e.mgr.askGrace = 500 * time.Millisecond // long enough for the discovery configs, short for the test
	bus, err := mqttbus.Connect(ctx, mqttbus.Options{BrokerURL: br.URL(), ClientID: "tasmofleet-test"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, bus.Connected)
	ch := mqttcmd.New(bus, mqttcmd.Options{}) // the questions go through the one command channel
	t.Cleanup(ch.Close)
	e.mgr.o.Asker = ch
	unsub := e.mgr.Attach(bus)
	t.Cleanup(unsub)
	e.run(t)

	macOf := func(d simulator.Device) string { return d.(interface{ MAC() string }).MAC() }
	deadline := time.Now().Add(10 * time.Second)
	for {
		byMAC := map[string]Proposal{}
		for _, p := range mustProposals(t, e) {
			byMAC[p.Device.MAC] = p
		}
		p1, ok1 := byMAC[macOf(plain)]
		p2, ok2 := byMAC[macOf(announced)]
		if ok1 && ok2 {
			if p1.Device.Topic != plain.Topic() || p1.Via != ViaMQTT || p2.Device.Topic != announced.Topic() {
				t.Fatalf("proposals = %+v / %+v", p1, p2)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("proposals = %+v, want both devices", byMAC)
		}
		time.Sleep(50 * time.Millisecond)
	}
	var status5 int
	for _, c := range plain.Commands() {
		if strings.EqualFold(c, "STATUS 5") {
			status5++
		}
		if strings.EqualFold(strings.TrimSpace(c), "STATUS 0") || strings.EqualFold(strings.TrimSpace(c), "STATUS") {
			t.Errorf("automated code sent %q", c)
		}
	}
	if status5 != 1 {
		t.Errorf("Status 5 sent %d times to the device without discovery, want 1: %q", status5, plain.Commands())
	}
	for _, c := range announced.Commands() {
		if strings.HasPrefix(strings.ToUpper(c), "STATUS") {
			t.Errorf("a device with a discovery config was asked %q", c)
		}
	}
}

// Why (#45): devices in another LAN (behind a VPN) report an address outside
// DEVICE_NETWORKS and are listed as "outside the device networks". The admin
// then adds that LAN on the settings page, which changes DEVICE_NETWORKS live.
// Both devices, the one found through its tasmota/discovery config and the one
// identified through Status 5, must become proposals without a restart and
// without a new message from the device (Tasmota republishes neither on its
// own), and without a second Status 5. The variant with the network
// configured from the start is TestSimulatedFleetWithoutDiscoveryConfigIsProposed.
func TestDevicesOutsideNetworksAreProposedWhenNetworksGrow(t *testing.T) {
	br, err := simulator.StartBroker()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = br.Close() }) // test teardown
	ctx, cancel := context.WithCancel(bg)
	t.Cleanup(cancel)
	fleet, err := simulator.NewFleet(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures", MQTTURL: br.URL()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() }) // test teardown
	plain, err := fleet.Add(ctx, simulator.DeviceSpec{Name: "far-no-disc", Profile: simulator.ProfileESP8266_4M, Version: "15.1.0", NoDiscovery: true})
	if err != nil {
		t.Fatal(err)
	}
	announced, err := fleet.Add(ctx, simulator.DeviceSpec{Name: "far-announced", Profile: simulator.ProfileESP32, Version: "15.1.0"})
	if err != nil {
		t.Fatal(err)
	}

	// The simulated devices live at 127.0.0.1: "the other LAN". Only
	// 127.0.0.2/32 is a device network at first.
	nets := tasmota.NewNetworkSet([]netip.Prefix{netip.MustParsePrefix("127.0.0.2/32")})
	e := newEnv(t, "127.0.0.0/8", func(o *Options) { o.Networks, o.NetworkSet = nil, nets })
	e.mgr.askGrace = 500 * time.Millisecond
	bus, err := mqttbus.Connect(ctx, mqttbus.Options{BrokerURL: br.URL(), ClientID: "tasmofleet-test"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, bus.Connected)
	ch := mqttcmd.New(bus, mqttcmd.Options{}) // the questions go through the one command channel
	t.Cleanup(ch.Close)
	e.mgr.o.Asker = ch
	unsub := e.mgr.Attach(bus)
	t.Cleanup(unsub)
	e.run(t)

	outside := func() int {
		n := 0
		for _, u := range e.mgr.Unidentified() {
			if (u.Topic == plain.Topic() || u.Topic == announced.Topic()) && strings.Contains(u.Reason, "outside the device networks") {
				n++
			}
		}
		return n
	}
	waitUntil(t, 10*time.Second, func() bool { return outside() == 2 }, func() string {
		return fmt.Sprintf("unidentified = %+v, want both devices listed as outside the device networks", e.mgr.Unidentified())
	})
	if p := mustProposals(t, e); len(p) != 0 {
		t.Fatalf("proposals = %+v while the devices are outside the device networks", p)
	}

	nets.Set([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}) // the settings page saved the other LAN

	macOf := func(d simulator.Device) string { return d.(interface{ MAC() string }).MAC() }
	waitUntil(t, 10*time.Second, func() bool {
		byMAC := map[string]bool{}
		for _, p := range mustProposals(t, e) {
			byMAC[p.Device.MAC] = true
		}
		return byMAC[macOf(plain)] && byMAC[macOf(announced)]
	}, func() string { return fmt.Sprintf("proposals = %+v, want both devices", mustProposals(t, e)) })
	if u := e.mgr.Unidentified(); len(u) != 0 {
		t.Errorf("proposed devices are still listed as unidentified: %+v", u)
	}
	var status5 int
	for _, c := range plain.Commands() {
		if strings.EqualFold(c, "STATUS 5") {
			status5++
		}
	}
	if status5 != 1 {
		t.Errorf("Status 5 sent %d times, want 1: the answer is kept, a network change asks nothing again", status5)
	}
}

func waitUntil(t *testing.T, d time.Duration, cond func() bool, msg func() string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg())
		}
		time.Sleep(50 * time.Millisecond)
	}
}
