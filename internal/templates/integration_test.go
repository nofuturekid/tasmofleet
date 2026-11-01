//go:build integration

package templates_test

import (
	"context"
	"encoding/json"
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
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/mqttcmd"
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

// mqttOnly sends every command over MQTT, as the router does for a device whose HTTP fails.
type mqttOnly struct {
	ch *mqttcmd.Channel
	st *store.SQLite
}

func (m mqttOnly) Command(ctx context.Context, t tasmota.Target, cmnd string) (json.RawMessage, error) {
	d, err := m.st.GetDevice(ctx, t.DeviceID())
	if err != nil {
		return nil, err
	}
	return m.ch.Do(ctx, d, cmnd)
}

// rig is the production pieces with only the devices simulated.
type rig struct {
	t      *testing.T
	st     *store.SQLite
	fleet  simulator.Fleet
	m      *templates.Manager
	eng    *ota.Engine
	bk     *fakeBackups
	devs   map[model.DeviceID]simulator.Device
	broker *simulator.Broker
	ch     *mqttcmd.Channel
}

func nousTemplate(relay int) *simulator.UserTemplate {
	return &simulator.UserTemplate{Name: "NOUS A1T", GPIO: []int{32, 0, 0, 0, 2720, 2656, 0, 0, 2624, 320, relay, 0, 0, 0}, Base: 49}
}

// newRig builds the rig; with viaMQTT the service talks to the devices through a broker only.
func newRig(t *testing.T, viaMQTT bool) *rig {
	t.Helper()
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
	r := &rig{t: t, st: st, bk: &fakeBackups{}, devs: map[model.DeviceID]simulator.Device{}}
	opts := simulator.FleetOptions{FixturesDir: "../../testdata/fixtures", TelePeriod: time.Hour}
	var cmd templates.Commander
	if viaMQTT {
		if r.broker, err = simulator.StartBroker(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = r.broker.Close() })
		opts.MQTTURL = r.broker.URL()
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		bus, err := mqttbus.Connect(ctx, mqttbus.Options{BrokerURL: r.broker.URL(), ClientID: "tasmofleet-templates-it"})
		if err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(10 * time.Second); !bus.Connected(); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("bus never connected")
			}
		}
		r.ch = mqttcmd.New(bus, mqttcmd.Options{Gap: 5 * time.Millisecond})
		t.Cleanup(r.ch.Close)
		cmd = mqttOnly{ch: r.ch, st: st}
		time.Sleep(300 * time.Millisecond) // subscriptions are established asynchronously
	} else {
		cmd = tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, Timeout: 3 * time.Second,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	}
	if r.fleet, err = simulator.NewFleetWith(opts, simulator.Extra{TimeScale: 0.02}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.fleet.Close() })
	r.m = templates.New(templates.Options{Devices: st, Client: cmd, Backups: noSnapshots{}})
	ap := r.m.NewApplier()
	ap.Settle, ap.Pause, ap.Attempts = 300*time.Millisecond, 200*time.Millisecond, 100
	r.eng = ota.NewEngine(ota.Options{Machine: ota.DefaultConfig(), MaxParallel: 2, Templates: ap, Principals: admin{}}, st, st, nil, nil, r.bk, clock.Real{}, nil)
	r.m.SetEngine(r.eng)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.eng.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return r
}

// add puts a simulated NOUS A1T into the simulator and the inventory.
func (r *rig) add(name string, relay int, extra ...simulator.Fault) model.DeviceID {
	r.t.Helper()
	spec := simulator.DeviceSpec{Name: name, Profile: simulator.ProfileNOUSA1TPrusa, Version: "15.6.0", Template: nousTemplate(relay),
		RebootGap: 100 * time.Millisecond, Faults: extra}
	d, err := r.fleet.Add(context.Background(), spec)
	if err != nil {
		r.t.Fatal(err)
	}
	c, _ := simulator.Control(d)
	host, portS, _ := strings.Cut(d.Addr(), ":")
	port, _ := strconv.Atoi(portS)
	id, err := r.st.CreateDevice(context.Background(), model.Device{Name: name, Topic: d.Topic(), Address: host, Port: port, MAC: c.MAC(), MQTT: model.MQTTOurs,
		Identity: model.Identity{Family: model.ChipESP8266, Variant: "tasmota", Hardware: "ESP8285N08", Version: "15.6.0"}})
	if err != nil {
		r.t.Fatal(err)
	}
	r.devs[id] = d
	return id
}

// waitOnline waits until the device answers over MQTT (a command before the channel saw it online is refused).
func (r *rig) waitOnline(id model.DeviceID) {
	r.t.Helper()
	d, _ := r.st.GetDevice(context.Background(), id)
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := r.ch.Do(context.Background(), d, "Status 5"); err == nil {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatal("device never answered over MQTT")
		}
	}
}

// run previews and applies the first group's deviating devices and returns the finished jobs.
func (r *rig) run(ctx context.Context) (templates.Preview, []model.Job) {
	r.t.Helper()
	ov, err := r.m.Overview(ctx, nil, true)
	if err != nil || len(ov.Groups) != 1 || ov.Groups[0].Reference == nil {
		r.t.Fatalf("%v %+v", err, ov)
	}
	g := ov.Groups[0]
	var ids []model.DeviceID
	for _, d := range g.Devices {
		if d.Plan != nil && !d.Plan.Matches {
			ids = append(ids, d.ID)
		}
	}
	if len(ids) != 1 {
		r.t.Fatalf("deviating devices %v", ids)
	}
	req := templates.Request{Group: g.Key, Reference: g.Reference.Device.ID, Devices: ids}
	pv, err := r.m.Preview(ctx, req)
	if err != nil || pv.Applicable != 1 || len(pv.Rows[0].Plan.Diff) != 1 {
		r.t.Fatalf("%v %+v", err, pv)
	}
	batch, refused, err := r.m.Apply(ctx, "alice", req, pv.Digest)
	if err != nil || len(refused) != 0 {
		r.t.Fatalf("%v %v", err, refused)
	}
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		jobs, _ := r.st.ListJobs(ctx, store.JobFilter{BatchID: batch})
		if len(jobs) == 1 && jobs[0].State.Terminal() {
			return pv, jobs
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("job did not finish: %+v", jobs)
		}
	}
}

func templateCommands(d simulator.Device) (n int) {
	for _, c := range d.Commands() {
		if strings.HasPrefix(c, "Template {") {
			n++
		}
	}
	return n
}

// Why: the whole chain with production parts and only the device simulated: the tab's service reads the
// fleet over HTTP, plans, the engine takes the backup, sends one Template command that restarts the
// simulated device (Tasmota semantics from WP-A), and the read-back, which proves the restart by BootCount
// and uptime, shows it matching the reference.
func TestApplyOverTheSimulatedFleet(t *testing.T) {
	r := newRig(t, false)
	for i, relay := range []int{224, 224, 224, 320} {
		r.add("nous-"+strconv.Itoa(i+1), relay)
	}
	ctx := context.Background()
	pv, jobs := r.run(ctx)
	if jobs[0].State != model.JobSucceeded {
		t.Fatalf("job %s: %s", jobs[0].State, jobs[0].LastError)
	}
	id := pv.Rows[0].Device.ID
	if len(r.bk.taken) != 1 || r.bk.taken[0] != id || templateCommands(r.devs[id]) != 1 {
		t.Fatalf("backups %v, Template sent %d times: %v", r.bk.taken, templateCommands(r.devs[id]), r.devs[id].Commands())
	}
	ov, _ := r.m.Overview(ctx, nil, true)
	for _, d := range ov.Groups[0].Devices {
		if d.Plan == nil || !d.Plan.Matches {
			t.Errorf("device %s does not match after the apply: %+v", d.Name, d.Plan)
		}
	}
}

// Why (M1): a device that is reached over MQTT only must be readable, changeable and verifiable. Tasmota
// answers a successful Template write with the template itself (no "Template" key), which the strict MQTT
// matcher has to take as the answer; without it the write would time out and the job fail on a device that
// had in fact changed.
func TestApplyOverMQTTOnly(t *testing.T) {
	r := newRig(t, true)
	noHTTP := simulator.Fault{Kind: simulator.FaultHTTPUnreachable}
	var ids []model.DeviceID
	for i, relay := range []int{224, 224, 224, 320} {
		ids = append(ids, r.add("mq-"+strconv.Itoa(i+1), relay, noHTTP))
	}
	for _, id := range ids {
		r.waitOnline(id)
	}
	pv, jobs := r.run(context.Background())
	if jobs[0].State != model.JobSucceeded {
		t.Fatalf("job %s: %s", jobs[0].State, jobs[0].LastError)
	}
	if got := templateCommands(r.devs[pv.Rows[0].Device.ID]); got != 1 {
		t.Fatalf("Template sent %d times", got)
	}
	// the write was answered, not timed out and papered over by the read-back
	evs, _ := r.st.JobEvents(context.Background(), jobs[0].ID)
	for _, e := range evs {
		if strings.Contains(e.Detail, "was lost") {
			t.Fatalf("the reply to the Template write never arrived: %+v", e)
		}
	}
}

// Why (M1): a device that refuses the Template (here: ARCH of another chip family) answers
// {"Template":"Invalid JSON or Bad Chip Type"} and does not restart. The job must fail at once as
// "nothing applied", not wait for a restart that never comes.
func TestApplyRefusedByTheDeviceFailsAtOnce(t *testing.T) {
	r := newRig(t, false)
	for i, relay := range []int{224, 224, 224, 320} {
		r.add("nous-"+strconv.Itoa(i+1), relay)
	}
	ctx := context.Background()
	ov, _ := r.m.Overview(ctx, nil, true)
	g := ov.Groups[0]
	var target model.DeviceID
	for _, d := range g.Devices {
		if d.Plan != nil && !d.Plan.Matches {
			target = d.ID
		}
	}
	pv, err := r.m.Preview(ctx, templates.Request{Group: g.Key, Reference: g.Reference.Device.ID, Devices: []model.DeviceID{target}})
	if err != nil || len(pv.Rows) != 1 {
		t.Fatalf("%v %+v", err, pv)
	}
	// the engine is handed a command with a wrong ARCH: the device refuses it
	bad := strings.Replace(pv.Rows[0].Plan.Commands[0], `"ARCH":"ESP8266"`, `"ARCH":"ESP32"`, 1)
	if bad == pv.Rows[0].Plan.Commands[0] {
		t.Fatalf("the plan has no ARCH to corrupt: %s", bad)
	}
	batch, _, err := r.eng.SubmitTemplate(ctx, "alice", ota.TemplateStart{Group: "x"}, []ota.TemplateRequest{{
		DeviceID: target, Command: bad, Name: pv.ReferenceTemplate.Name,
		Have: pv.Rows[0].Device.Reading.Template.Fingerprint(), Want: pv.ReferenceTemplate.Fingerprint()}})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		jobs, _ := r.st.ListJobs(ctx, store.JobFilter{BatchID: batch})
		if len(jobs) == 1 && jobs[0].State.Terminal() {
			if jobs[0].State != model.JobFailed || !strings.Contains(jobs[0].LastError, "nothing was applied") || !strings.Contains(jobs[0].LastError, "Bad Chip Type") {
				t.Fatalf("job %s: %q", jobs[0].State, jobs[0].LastError)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not finish: %+v", jobs)
		}
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("a refusal took %v: it waited for a restart", time.Since(start))
	}
}
