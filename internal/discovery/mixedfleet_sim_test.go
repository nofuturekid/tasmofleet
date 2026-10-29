package discovery

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
)

// Why (#45, mixed fleet): the reporter's fleet has devices added over HTTP and
// devices that are only on the broker. A device stored by address without a
// MAC or topic must not hide its neighbours on the broker: every other
// device that is online on the broker is still proposed.
func TestMixedFleetHTTPDeviceDoesNotHideBrokerDevices(t *testing.T) {
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
	var devs []simulator.Device
	for i := 0; i < 3; i++ {
		d, err := fleet.Add(ctx, simulator.DeviceSpec{Name: fmt.Sprintf("mixed-%d", i), Profile: simulator.ProfileESP8266_4M, Version: "15.1.0", NoDiscovery: i != 0})
		if err != nil {
			t.Fatal(err)
		}
		devs = append(devs, d)
	}
	e := newEnv(t, "127.0.0.0/8", nil)
	host, port, _ := strings.Cut(devs[0].Addr(), ":")
	var p int
	if _, err := fmt.Sscanf(port, "%d", &p); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.inv.Upsert(bg, "admin", model.Device{Name: "http-added", Address: host, Port: p}); err != nil {
		t.Fatal(err)
	}
	e.mgr.askGrace = 500 * time.Millisecond
	bus, err := mqttbus.Connect(ctx, mqttbus.Options{BrokerURL: br.URL(), ClientID: "tasmofleet-test"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, bus.Connected)
	unsub := e.mgr.Attach(bus)
	t.Cleanup(unsub)
	e.run(t)

	macOf := func(d simulator.Device) string { return d.(interface{ MAC() string }).MAC() }
	waitUntil(t, 10*time.Second, func() bool {
		byMAC := map[string]bool{}
		for _, pr := range mustProposals(t, e) {
			byMAC[pr.Device.MAC] = true
		}
		return byMAC[macOf(devs[1])] && byMAC[macOf(devs[2])]
	}, func() string {
		return fmt.Sprintf("proposals = %+v unidentified = %+v", mustProposals(t, e), e.mgr.Unidentified())
	})
}
