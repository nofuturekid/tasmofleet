package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// Status 2 facts for devices identified before 0.22.0 (#417): the facts column was added then
// without a backfill, so every device probed earlier said "Most of Status 2 is not read yet" for
// good. The reader fills them without a person doing anything, and touches nothing else.

const fwrReply = `{"StatusFWR":{"Version":"15.0.0(release-tasmota)","BuildDateTime":"2026-01-01T00:00:00","Core":"2_7_8","SDK":"2.2.2-dev(38a443e)","CpuFrequency":80,"Hardware":"ESP8266EX","CR":"370/699"}}`

// fwrStore keeps devices in memory and implements only the FWR writer: any other write would not
// compile into the reader, which is the point (identity fields are not its business).
type fwrStore struct {
	mu     sync.Mutex
	devs   []model.Device
	writes int
}

func (s *fwrStore) list(context.Context) ([]model.Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Device(nil), s.devs...), nil
}

func (s *fwrStore) UpdateFWRFacts(_ context.Context, id model.DeviceID, _ store.ProbeGuard, f model.FWRFacts) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.devs {
		if s.devs[i].ID == id {
			s.devs[i].Identity.FWR = f
			s.writes++
			return nil
		}
	}
	return store.ErrConflict
}

func (s *fwrStore) get(id model.DeviceID) model.Device {
	devs, _ := s.list(context.Background())
	for _, d := range devs {
		if d.ID == id {
			return d
		}
	}
	return model.Device{}
}

type fwrClient struct {
	mu    sync.Mutex
	asked []model.DeviceID
	cmds  []string
	paths []transport.Path
	fail  bool
}

func (c *fwrClient) Command(ctx context.Context, t tasmota.Target, cmd string) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked, c.cmds = append(c.asked, t.DeviceID()), append(c.cmds, cmd)
	p, _ := transport.PathFrom(ctx)
	c.paths = append(c.paths, p)
	if c.fail {
		return nil, tasmota.ErrUnreachable
	}
	return json.RawMessage(fwrReply), nil
}
func (c *fwrClient) DownloadConfig(context.Context, tasmota.Target) ([]byte, error) { return nil, nil }
func (c *fwrClient) RootPage(context.Context, tasmota.Target) (tasmota.RootInfo, error) {
	return tasmota.RootInfo{}, nil
}

// preFWR is a device an HTTP probe identified before 0.22.0: identity_at set, no facts.
func preFWR(id model.DeviceID, now time.Time) model.Device {
	return model.Device{ID: id, Name: "plug", Topic: "plug" + strconv.Itoa(int(id)), Address: "192.0.2." + strconv.Itoa(int(id)), MAC: "aa:bb:cc:00:00:0" + strconv.Itoa(int(id)),
		Identity: model.Identity{Family: model.ChipESP8266, Hardware: "ESP8266EX", Version: "15.0.0", Image: "release-tasmota", Variant: "tasmota",
			Language: "en_GB", LanguageKnown: true, DetectedAt: now.Add(-48 * time.Hour)}}
}

func newFWRTest(now *time.Time, c *fwrClient, health func(model.DeviceID) model.Health, devs ...model.Device) (*fwrFactsReader, *fwrStore) {
	st := &fwrStore{devs: devs}
	r := newFWRFactsReader(st.list, health, c, st, nil, slog.New(slog.DiscardHandler))
	r.now = func() time.Time { return *now }
	return r, st
}

func TestFWRFactsReaderFillsAnIdentifiedDeviceOverHTTPAndKeepsItsIdentity(t *testing.T) {
	// Why: the facts must arrive without the "Refresh identity" button, and the identity the probe
	// stored (which the identity_at invariant protects) must be exactly what it was.
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	c := &fwrClient{}
	d := preFWR(1, now)
	r, st := newFWRTest(&now, c, online, d)
	r.tick(t.Context())
	got := st.get(1)
	if got.Identity.FWR.Core != "2_7_8" || got.Identity.FWR.SDK != "2.2.2-dev(38a443e)" || got.Identity.FWR.CPUFrequency != 80 {
		t.Fatalf("facts not stored: %+v", got.Identity.FWR)
	}
	want := d
	want.Identity.FWR = got.Identity.FWR
	if got.Identity != want.Identity || got.Name != d.Name || got.Topic != d.Topic {
		t.Fatalf("identity changed:\n got %+v\nwant %+v", got.Identity, want.Identity)
	}
	if len(c.cmds) != 1 || c.cmds[0] != "Status 2" || c.paths[0] == transport.MQTT {
		t.Fatalf("sent %v on %v, want one Status 2 through the router's own choice (HTTP)", c.cmds, c.paths)
	}
	r.tick(t.Context())
	if len(c.asked) != 1 {
		t.Fatalf("asked again after the facts were stored: %v", c.asked)
	}
}

func TestFWRFactsReaderPinsMQTTForADeviceWithoutAnHTTPAddress(t *testing.T) {
	// Why: a device added from the broker has no HTTP address to try.
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	c := &fwrClient{}
	d := preFWR(1, now)
	d.Address = ""
	r, st := newFWRTest(&now, c, online, d)
	r.tick(t.Context())
	if c.paths[0] != transport.MQTT || !st.get(1).Identity.FWR.Read() {
		t.Fatalf("path %v, facts %+v", c.paths, st.get(1).Identity.FWR)
	}
}

func TestFWRFactsReaderAsksOneDevicePerTickAndOnlyWhoNeedsIt(t *testing.T) {
	// Why: the bus allows one background question per second; a fleet must be filled slowly. Devices
	// that have their facts, were never identified, or run minimal firmware are not asked at all.
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	read := preFWR(2, now)
	read.Identity.FWR = model.FWRFacts{Core: "x"}
	unidentified := preFWR(3, now)
	unidentified.Identity.DetectedAt = time.Time{}
	minimal := preFWR(4, now)
	minimal.Identity.Variant = "minimal"
	c := &fwrClient{}
	r, st := newFWRTest(&now, c, online, preFWR(1, now), read, unidentified, minimal, preFWR(5, now), preFWR(6, now))
	for i := range 6 {
		r.tick(t.Context())
		if want := min(i+1, 3); len(c.asked) != want {
			t.Fatalf("after tick %d: asked %v, want %d (one per tick, three devices need it)", i+1, c.asked, want)
		}
	}
	for _, id := range []model.DeviceID{1, 5, 6} {
		if !st.get(id).Identity.FWR.Read() {
			t.Errorf("device %d got no facts", id)
		}
	}
	if st.writes != 3 {
		t.Errorf("%d writes, want 3", st.writes)
	}
}

func TestFWRFactsReaderDoesNotAskOfflineDevices(t *testing.T) {
	// Why: nothing could answer, and a pile of timeouts would only load the bus and the log.
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	c := &fwrClient{}
	state := model.HealthOffline
	r, st := newFWRTest(&now, c, func(model.DeviceID) model.Health { return model.Health{State: state} }, preFWR(1, now))
	for range 5 {
		r.tick(t.Context())
	}
	if len(c.asked) != 0 {
		t.Fatalf("asked an offline device: %v", c.asked)
	}
	state = model.HealthUnknown
	r.tick(t.Context())
	if len(c.asked) != 0 {
		t.Fatalf("asked a device whose state is not known to be online: %v", c.asked)
	}
	state = model.HealthOnline
	r.tick(t.Context())
	if len(c.asked) != 1 || !st.get(1).Identity.FWR.Read() {
		t.Fatalf("not read once online: asked %v", c.asked)
	}
}

func TestFWRFactsReaderRetriesAFailedReadOnlyAfterTenMinutes(t *testing.T) {
	// Why: a device that does not answer (or answers something unusable) must not be hammered by a
	// reader that ticks every two seconds: no storm, a later retry.
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	c := &fwrClient{fail: true}
	r, st := newFWRTest(&now, c, online, preFWR(1, now))
	for range 300 { // ten minutes of ticks, less a little
		r.tick(t.Context())
		now = now.Add(fwrFactsTick)
	}
	if len(c.asked) != 1 {
		t.Fatalf("asked %d times within %v of a failure, want 1", len(c.asked), 300*fwrFactsTick)
	}
	now = now.Add(fwrFactsEvery)
	c.fail = false
	r.tick(t.Context())
	if len(c.asked) != 2 || !st.get(1).Identity.FWR.Read() {
		t.Fatalf("no retry after %v: asked %d", fwrFactsEvery, len(c.asked))
	}
}

func TestFWRFactsReaderIgnoresAReplyThatCarriesNoFacts(t *testing.T) {
	// Why: a reply that cannot be read must not be stored as "read" (an empty record would look
	// like facts that are there) and is retried later like any failure.
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	c := &fwrClient{}
	st := &fwrStore{devs: []model.Device{preFWR(1, now)}}
	r := newFWRFactsReader(st.list, online, replyClient{c, `{"Command":"Unknown"}`}, st, nil, slog.New(slog.DiscardHandler))
	r.now = func() time.Time { return now }
	r.tick(t.Context())
	if st.writes != 0 {
		t.Fatalf("stored %+v from an unusable reply", st.get(1).Identity.FWR)
	}
}

type replyClient struct {
	*fwrClient
	reply string
}

func (c replyClient) Command(ctx context.Context, t tasmota.Target, cmd string) (json.RawMessage, error) {
	_, _ = c.fwrClient.Command(ctx, t, cmd) // record the question; the reply is the fixed one
	return json.RawMessage(c.reply), nil
}

func TestFWRFactsReaderReadsASimulatedDeviceOverHTTP(t *testing.T) {
	// Why: the fake client proves the rules; this one proves the real reply of a (simulated) device
	// yields the facts through the real HTTP client.
	fleet, err := simulator.NewFleet(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() }) // test teardown
	dev, err := fleet.Add(t.Context(), simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_1M, Version: "15.6.0"})
	if err != nil {
		t.Fatal(err)
	}
	host, ps, _ := net.SplitHostPort(dev.Addr())
	port, _ := strconv.Atoi(ps)
	now := time.Now()
	d := preFWR(1, now)
	d.Address, d.Port = host, port
	st := &fwrStore{devs: []model.Device{d}}
	r := newFWRFactsReader(st.list, online, tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: loopbackNets()}), st, nil, slog.New(slog.DiscardHandler))
	r.tick(t.Context())
	f := st.get(1).Identity.FWR
	if !f.Read() || f.Core == "" || f.Version == "" {
		t.Fatalf("facts from the simulator: %+v (device saw %v)", f, dev.Commands())
	}
	for _, c := range dev.Commands() {
		if c != "Status 2" {
			t.Errorf("the device was sent %q, want only Status 2 (never Status 0)", c)
		}
	}
}

// Why: over MQTT the same must work for a device HTTP cannot reach; the rig is the one the group
// topic reader is tested with (real broker, bus, command channel and router).
func TestFWRFactsReaderReadsADeviceOnlyMQTTReaches(t *testing.T) {
	g := newBlindRig(t)
	g.st.mu.Lock()
	g.st.devs[0].Identity = model.Identity{Family: model.ChipESP32, Hardware: "ESP32-D0WD-V3", Version: "15.1.0", Image: "release-tasmota32", Variant: "tasmota", DetectedAt: time.Now().Add(-time.Hour)}
	before := g.st.devs[0].Identity
	g.st.mu.Unlock()
	fs := &gtFWRStore{gtStore: g.st}
	r := newFWRFactsReader(g.st.list, online, g.router, fs, nil, slog.New(slog.DiscardHandler))
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		r.tried = map[model.DeviceID]time.Time{}
		r.tick(t.Context())
		if d, _ := g.st.GetDevice(t.Context(), 1); d.Identity.FWR.Read() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the facts of the device HTTP cannot reach were never read over MQTT")
		}
	}
	d, _ := g.st.GetDevice(t.Context(), 1)
	want := before
	want.FWR = d.Identity.FWR
	if d.Identity != want {
		t.Fatalf("identity changed: %+v", d.Identity)
	}
	if d.Identity.FWR.Core == "" {
		t.Fatalf("facts: %+v", d.Identity.FWR)
	}
}

// gtFWRStore adds the FWR writer to the group topic test store.
type gtFWRStore struct{ *gtStore }

func (s *gtFWRStore) UpdateFWRFacts(_ context.Context, id model.DeviceID, _ store.ProbeGuard, f model.FWRFacts) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.devs {
		if s.devs[i].ID == id {
			s.devs[i].Identity.FWR = f
		}
	}
	return nil
}
