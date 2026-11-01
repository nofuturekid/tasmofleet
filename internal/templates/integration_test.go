//go:build integration

package templates_test

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/clock"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/secretbox"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/templates"
)

type admin struct{}

func (admin) Current(context.Context, string, string) (model.Principal, error) {
	return model.Principal{Role: model.RoleAdmin}, nil
}

type fakeBackups struct{ taken []model.DeviceID }

func (f *fakeBackups) Backup(_ context.Context, _ string, id model.DeviceID) ([]model.Backup, error) {
	f.taken = append(f.taken, id)
	return nil, nil
}

type noSnapshots struct{}

func (noSnapshots) List(context.Context, model.DeviceID) ([]model.Backup, error) { return nil, nil }
func (noSnapshots) Open(context.Context, model.BackupID) ([]byte, model.Backup, error) {
	return nil, model.Backup{}, nil
}

// Why: the whole chain with production parts and only the device simulated: the tab's service reads the
// fleet over HTTP, plans, the engine takes the backup, sends one Template command that restarts the
// simulated device (Tasmota semantics from WP-A), and the read-back shows it matching the reference.
func TestApplyOverTheSimulatedFleet(t *testing.T) {
	dir := t.TempDir()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "tasmofleet.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fleet, err := simulator.NewFleetWith(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures", TelePeriod: time.Hour}, simulator.Extra{TimeScale: 0.02})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() })
	nous := func(relay int) *simulator.UserTemplate {
		g := []int{32, 0, 0, 0, 2720, 2656, 0, 0, 2624, 320, relay, 0, 0, 0}
		return &simulator.UserTemplate{Name: "NOUS A1T", GPIO: g, Base: 49}
	}
	var ctrls = map[model.DeviceID]simulator.Device{}
	for i, relay := range []int{224, 224, 224, 320} {
		spec := simulator.DeviceSpec{Name: "nous-" + strconv.Itoa(i+1), Profile: simulator.ProfileNOUSA1TPrusa, Version: "15.6.0", Template: nous(relay), RebootGap: 100 * time.Millisecond}
		d, err := fleet.Add(context.Background(), spec)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := simulator.Control(d)
		host, portS, _ := strings.Cut(d.Addr(), ":")
		port, _ := strconv.Atoi(portS)
		id, err := st.CreateDevice(context.Background(), model.Device{Name: spec.Name, Topic: d.Topic(), Address: host, Port: port, MAC: c.MAC(),
			Identity: model.Identity{Family: model.ChipESP8266, Variant: "tasmota", Hardware: "ESP8285N08", Version: "15.6.0"}})
		if err != nil {
			t.Fatal(err)
		}
		ctrls[id] = d
	}
	// the identity is stored by the prober in production; here the test stores it
	devs, _ := st.ListDevices(context.Background())
	for _, d := range devs {
		d.Identity = model.Identity{Family: model.ChipESP8266, Variant: "tasmota", Hardware: "ESP8285N08", Version: "15.6.0"}
		if err := st.UpdateDevice(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	client := tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, Timeout: 3 * time.Second,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	m := templates.New(templates.Options{Devices: st, Client: client, Backups: noSnapshots{}})
	bk := &fakeBackups{}
	ap := m.NewApplier()
	ap.Settle, ap.Pause = 300*time.Millisecond, 100*time.Millisecond
	eng := ota.NewEngine(ota.Options{Machine: ota.DefaultConfig(), MaxParallel: 2, Templates: ap, Principals: admin{}}, st, st, nil, nil, bk, clock.Real{}, nil)
	m.SetEngine(eng)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = eng.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	ov, err := m.Overview(ctx, nil, true)
	if err != nil || len(ov.Groups) != 1 || ov.Groups[0].Reference == nil {
		t.Fatalf("%v %+v", err, ov)
	}
	g := ov.Groups[0]
	var ids []model.DeviceID
	for _, d := range g.Devices {
		if d.Plan != nil && !d.Plan.Matches {
			ids = append(ids, d.ID)
		}
	}
	if len(ids) != 1 {
		t.Fatalf("deviating devices %v", ids)
	}
	req := templates.Request{Group: g.Key, Reference: g.Reference.Device.ID, Devices: ids}
	pv, err := m.Preview(ctx, req)
	if err != nil || pv.Applicable != 1 || len(pv.Rows[0].Plan.Diff) != 1 {
		t.Fatalf("%v %+v", err, pv)
	}
	batch, refused, err := m.Apply(ctx, "alice", req, pv.Digest)
	if err != nil || len(refused) != 0 {
		t.Fatalf("%v %v", err, refused)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		jobs, _ := st.ListJobs(ctx, store.JobFilter{BatchID: batch})
		if len(jobs) == 1 && jobs[0].State.Terminal() {
			if jobs[0].State != model.JobSucceeded {
				t.Fatalf("job %s: %s", jobs[0].State, jobs[0].LastError)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not finish: %+v", jobs)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(bk.taken) != 1 || bk.taken[0] != ids[0] {
		t.Fatalf("backups %v", bk.taken)
	}
	var templateCmds int
	for _, c := range ctrls[ids[0]].Commands() {
		if strings.HasPrefix(c, "Template {") {
			templateCmds++
		}
	}
	if templateCmds != 1 {
		t.Fatalf("Template sent %d times: %v", templateCmds, ctrls[ids[0]].Commands())
	}
	m2, _ := m.Overview(ctx, nil, true)
	for _, d := range m2.Groups[0].Devices {
		if d.Plan == nil || !d.Plan.Matches {
			t.Errorf("device %s does not match after the apply: %+v", d.Name, d.Plan)
		}
	}
}
