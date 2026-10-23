package store

import (
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

func netFacts(ssid string) model.NetFacts {
	return model.NetFacts{Hostname: "plug-1", IP: "192.0.2.10", Gateway: "192.0.2.1", SSID: ssid, BSSID: "02:00:5e:00:00:01", Channel: 11,
		ReadAt: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}
}

// Why: the device page shows hostname, gateway and Wi-Fi from the device record, so a device
// created from a probe (import, add) must carry them from the first moment, across a restart.
func TestCreateDeviceStoresNetworkFacts(t *testing.T) {
	s, _, _ := newStore(t)
	id, err := s.CreateDevice(ctx, model.Device{Name: "plug", Address: "192.0.2.10", Net: netFacts("IoT")})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDevice(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if want := netFacts("IoT"); got.Net != want {
		t.Fatalf("Net = %+v, want %+v", got.Net, want)
	}
}

// Why: "Refresh identity" must persist what the probe read, but an admin edit (UpdateDevice
// with a stale copy of the record) must not wipe the facts: they belong to the probe.
func TestUpdateProbeOwnsNetworkFactsAndEditsKeepThem(t *testing.T) {
	s, _, _ := newStore(t)
	id, err := s.CreateDevice(ctx, model.Device{Name: "plug", Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01"})
	if err != nil {
		t.Fatal(err)
	}
	guard := ProbeGuard{Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01"}
	if err := s.UpdateProbe(ctx, id, guard, ProbeResult{SetNet: true, Net: netFacts("IoT")}); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetDevice(ctx, id)
	if d.Net.SSID != "IoT" {
		t.Fatalf("probe did not store the facts: %+v", d.Net)
	}
	d.Notes = "edited"
	d.Net = model.NetFacts{} // a stale copy
	if err := s.UpdateDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	if d, _ = s.GetDevice(ctx, id); d.Net.SSID != "IoT" || d.Notes != "edited" {
		t.Fatalf("edit lost the facts or the note: %+v", d)
	}
	// a probe that read nothing (SetNet false) keeps the stored facts
	if err := s.UpdateProbe(ctx, id, guard, ProbeResult{}); err != nil {
		t.Fatal(err)
	}
	if d, _ = s.GetDevice(ctx, id); d.Net.SSID != "IoT" {
		t.Fatalf("a probe without facts wiped them: %+v", d.Net)
	}
}
