package discovery

import (
	"net/netip"
	"testing"
)

// Why (#45, "exactly once"): an Ethernet-capable ESP32 announces the Ethernet
// MAC in tasmota/discovery, while a scan and Status 5 name the Wi-Fi MAC as
// the main one and the Ethernet MAC as an additional one. The two sightings
// are one device and must make one proposal, not two.
func TestEthernetDeviceSeenByDiscoveryAndScanIsOneProposal(t *testing.T) {
	e := newEnv(t, "172.31.0.0/16", nil)
	const wifi, eth = "02:00:5e:e7:40:00", "02:00:5e:e7:40:01"
	if _, err := e.mgr.consider(bg, candidate{via: ViaMQTT, mac: eth, ip: netip.MustParseAddr("172.31.23.40"), name: "workshop-eth", topic: "workshop-eth"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.mgr.consider(bg, candidate{via: ViaScan, mac: wifi, macs: []string{eth}, ip: netip.MustParseAddr("172.31.23.40"), port: 80, name: "workshop-eth"}); err != nil {
		t.Fatal(err)
	}
	if p := mustProposals(t, e); len(p) != 1 {
		t.Fatalf("proposals = %+v, want exactly one for the one device", p)
	}
}
