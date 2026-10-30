package transport

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/mqttcmd"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

func loopback() []netip.Prefix { return []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")} }

type simRig struct {
	*rig
	ctl simulator.Controller
	dev simulator.Device
}

// newSimRig is a simulated device behind a real broker, a real bus and the real command channel,
// reached by the real HTTP client.
func newSimRig(t *testing.T, faults ...simulator.Fault) *simRig {
	t.Helper()
	br, err := simulator.StartBroker()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = br.Close() }) // test teardown
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	fleet, err := simulator.NewFleetWith(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures", MQTTURL: br.URL(), TelePeriod: time.Hour}, simulator.Extra{TimeScale: 0.02})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() }) // test teardown
	dev, err := fleet.Add(ctx, simulator.DeviceSpec{Name: "sim-plug", Profile: simulator.ProfileESP32, Version: "15.1.0", Faults: faults})
	if err != nil {
		t.Fatal(err)
	}
	ctl, _ := simulator.Control(dev)
	bus, err := mqttbus.Connect(ctx, mqttbus.Options{BrokerURL: br.URL(), ClientID: "router-test"})
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
	host, port := splitAddr(t, dev.Addr())
	mem := NewMemory(DefaultFailFor, time.Now)
	devs := &fakeDevices{d: model.Device{ID: 7, Address: host, Port: port, Topic: dev.Topic(), MQTT: model.MQTTOurs}}
	f := &facts{mem: mem, groupKnown: true}
	cl := tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: loopback()})
	r := NewRouter(Options{HTTP: cl, Devices: devs, Facts: f, Memory: mem})
	r.SetMQTT(ch)
	// wait until the channel has seen the device come online: a command before is refused as offline
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := ch.Do(ctx, devs.d, "Status 5"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("device never answered over MQTT")
		}
	}
	return &simRig{rig: &rig{r: r, devs: devs, mem: mem, f: f}, ctl: ctl, dev: dev}
}

func splitAddr(t *testing.T, addr string) (string, int) {
	t.Helper()
	i := strings.LastIndex(addr, ":")
	var p int
	for _, c := range addr[i+1:] {
		p = p*10 + int(c-'0')
	}
	return addr[:i], p
}

func TestSimDeviceHTTPCannotReachIsManagedOverMQTT(t *testing.T) {
	// Why (#45): a device whose HTTP port refuses everything but which is on our broker answers
	// reads and takes writes, each sent exactly once.
	g := newSimRig(t, simulator.Fault{Kind: simulator.FaultHTTPUnreachable})
	ctx, tr := WithTrace(t.Context())
	raw, err := g.r.Command(ctx, g.target(), "Status 2")
	if err != nil || !strings.Contains(string(raw), "StatusFWR") || tr.Via() != "mqtt" {
		t.Fatalf("read: %s, %v, via %s", raw, err, tr.Via())
	}
	before := len(g.dev.Commands())
	if _, err := g.r.Command(t.Context(), g.target(), "Power1 ON"); err != nil || !g.ctl.Power() {
		t.Fatalf("write: %v, power %v", err, g.ctl.Power())
	}
	if n := len(g.dev.Commands()) - before; n != 1 {
		t.Errorf("Power1 ON reached the device %d times, want once: %v", n, g.dev.Commands())
	}
}

func TestSimStatus0CannotBePublishedByAnExecutor(t *testing.T) {
	// Why (safety rule 2): Json {"Status":0}, Publish, Rule and WebSend each run a Status 0 on the
	// device; over MQTT the console must not be a way around the channel's refusal.
	g := newSimRig(t, simulator.Fault{Kind: simulator.FaultHTTPUnreachable})
	g.mem.Fail(7, HTTPFailUnreachable)
	before := len(g.dev.Commands())
	for _, c := range []string{`Json {"Status":0}`, "Publish cmnd/" + g.dev.Topic() + "/Status 0", `Rule1 ON System#Boot DO Status 0 ENDON`,
		"WebSend [127.0.0.1] Status 0", "Backlog Power ON; Json {\"Status\":0}", "Status 0", "Backlog Status 0"} {
		_, err := g.r.Command(WithConsole(t.Context()), g.target(), c)
		if !errors.Is(err, mqttcmd.ErrRefused) {
			t.Errorf("%q: err = %v, want a refusal", c, err)
		}
	}
	if got := g.dev.Commands(); len(got) != before {
		t.Fatalf("the device received %v", got[before:])
	}
}

func TestSimBacklogAndSaveDataOverMQTT(t *testing.T) {
	// Why (#45): Make compliant sends "Backlog <setting>" and then SaveData; both are writes and
	// both have to reach a device HTTP cannot reach.
	g := newSimRig(t, simulator.Fault{Kind: simulator.FaultHTTPUnreachable})
	g.mem.Fail(7, HTTPFailUnreachable)
	for _, c := range []string{"Backlog PowerOnState 2", "SaveData 1", "PowerOnState"} {
		raw, err := g.r.Command(t.Context(), g.target(), c)
		if err != nil {
			t.Fatalf("%q: %v", c, err)
		}
		t.Logf("%q -> %s", c, raw)
	}
}
