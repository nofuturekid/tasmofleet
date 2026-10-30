package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/clock"
	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/secretbox"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

type fwClock struct {
	clock.Real
	now time.Time
}

func (c *fwClock) Now() time.Time { return c.now }

type fwAudit struct {
	mu sync.Mutex
	ev []model.AuditEvent
}

func (a *fwAudit) Record(_ context.Context, e model.AuditEvent) {
	a.mu.Lock()
	a.ev = append(a.ev, e)
	a.mu.Unlock()
}

func (a *fwAudit) last(t *testing.T) (state, source string) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.ev) == 0 || a.ev[len(a.ev)-1].Action != "device.fwreach" {
		t.Fatalf("no device.fwreach audit event: %+v", a.ev)
	}
	var d map[string]string
	if err := json.Unmarshal([]byte(a.ev[len(a.ev)-1].Detail), &d); err != nil {
		t.Fatal(err)
	}
	return d["state"], d["source"]
}

func fwFixture(t *testing.T, addrs ...string) (*runtime, *store.SQLite, []model.DeviceID, *fwAudit) {
	t.Helper()
	dir := t.TempDir()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() }) // temp DB
	var ids []model.DeviceID
	for i, a := range addrs {
		id, err := st.CreateDevice(t.Context(), model.Device{Name: "d" + string(rune('a'+i)), Address: a, Port: 80 + i})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	au := &fwAudit{}
	a := &runtime{svc: &Services{Store: st}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), bgContext: t.Context()}
	a.fwReach = newFWReachLearner(st, au, &fwClock{now: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}, a.log)
	return a, st, ids, au
}

func reachOf(t *testing.T, st *store.SQLite, id model.DeviceID) model.FWReach {
	t.Helper()
	d, err := st.GetDevice(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return d.FWReach
}

func get200(ip string) firmware.RequestEvent {
	return firmware.RequestEvent{RemoteIP: ip, Method: http.MethodGet, Release: "v15.0.0", Filename: "tasmota.bin.gz", Status: 200}
}

// Why: a firmware GET 200 from the device's own address is the proof that it reaches the server;
// it is the source that works without any OTA, so Choose can trust the download path early.
func TestFirmwareGetFromADeviceIPSetsYesAndIsAudited(t *testing.T) {
	a, st, ids, au := fwFixture(t, "10.0.0.1", "10.0.0.2")
	a.onFirmwareRequest(get200("10.0.0.2"))
	a.bgWG.Wait()
	if r := reachOf(t, st, ids[1]); r.State != model.FWReachYes || r.At.IsZero() {
		t.Fatalf("device 2: %+v", r)
	}
	if r := reachOf(t, st, ids[0]); r.State != model.FWReachUnknown {
		t.Fatalf("device 1 must stay unknown: %+v", r)
	}
	if s, src := au.last(t); s != "yes" || src != FWReachSourceFirmwareServer {
		t.Fatalf("audit %q %q", s, src)
	}
}

// Why: behind NAT many devices share one source address; learning "yes" for a guess would
// show a green path for a device that may never have connected.
func TestFirmwareGetFromAnUnknownOrSharedIPSetsNothing(t *testing.T) {
	a, st, ids, au := fwFixture(t, "10.0.0.1", "10.0.0.1", "10.0.0.3")
	a.onFirmwareRequest(get200("10.0.0.1")) // two devices: ambiguous
	a.onFirmwareRequest(get200("10.9.9.9")) // no device
	a.onFirmwareRequest(get200("not-an-ip"))
	a.bgWG.Wait()
	for _, id := range ids {
		if r := reachOf(t, st, id); r.State != model.FWReachUnknown {
			t.Fatalf("device %d: %+v", id, r)
		}
	}
	if len(au.ev) != 0 {
		t.Fatalf("audit rows: %+v", au.ev)
	}
}

// Why: only a served image counts. A HEAD, a 404 or a request outside /fw/ proves nothing about the
// device, and a refused request must not turn a remembered "no" into "yes".
func TestOnlyAServedFirmwareGetCounts(t *testing.T) {
	a, st, ids, _ := fwFixture(t, "10.0.0.1")
	for _, e := range []firmware.RequestEvent{
		{RemoteIP: "10.0.0.1", Method: http.MethodHead, Release: "v15.0.0", Status: 200},
		{RemoteIP: "10.0.0.1", Method: http.MethodGet, Release: "v15.0.0", Status: 404},
		{RemoteIP: "10.0.0.1", Method: http.MethodGet, Release: "", Status: 200},
	} {
		a.onFirmwareRequest(e)
	}
	a.bgWG.Wait()
	if r := reachOf(t, st, ids[0]); r.State != model.FWReachUnknown {
		t.Fatalf("%+v", r)
	}
}

// Why: a hostname address cannot be compared with the request's IP, but the IP the device reported
// itself (Status 5) can; otherwise hostname devices would never learn.
func TestFirmwareGetMatchesTheDevicesReportedIP(t *testing.T) {
	a, st, ids, _ := fwFixture(t, "fan.home.internal")
	if err := st.UpdateNet(t.Context(), ids[0], store.ProbeGuard{Address: "fan.home.internal", Port: 80}, model.NetFacts{IP: "10.0.0.7", ReadAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	a.onFirmwareRequest(get200("10.0.0.7"))
	a.bgWG.Wait()
	if r := reachOf(t, st, ids[0]); r.State != model.FWReachYes {
		t.Fatalf("%+v", r)
	}
}

// Why (D11): "no" is only the last observation. A device whose firewall was fixed must turn
// "yes" at its next download; and a new failure must turn a "yes" back to "no".
func TestLaterObservationOverridesEarlier(t *testing.T) {
	a, st, ids, au := fwFixture(t, "10.0.0.1")
	if err := a.fwReach.NoteFWReachNo(t.Context(), ids[0], "UPGRADE: connection failed"); err != nil {
		t.Fatal(err)
	}
	if r := reachOf(t, st, ids[0]); r.State != model.FWReachNo || r.Detail != "UPGRADE: connection failed" {
		t.Fatalf("%+v", r)
	}
	a.onFirmwareRequest(get200("10.0.0.1"))
	a.bgWG.Wait()
	if r := reachOf(t, st, ids[0]); r.State != model.FWReachYes || r.Detail != "" {
		t.Fatalf("yes must override no: %+v", r)
	}
	if err := a.fwReach.NoteFWReachNo(t.Context(), ids[0], "again"); err != nil {
		t.Fatal(err)
	}
	if r := reachOf(t, st, ids[0]); r.State != model.FWReachNo {
		t.Fatalf("no must override yes: %+v", r)
	}
	if s, src := au.last(t); s != "no" || src != FWReachSourceOTAFailure {
		t.Fatalf("audit %q %q", s, src)
	}
}

// Why: every download of a fleet would otherwise write the row and the audit log; a known "yes"
// is left alone (it never expires, only Forget or a failure change it).
func TestKnownYesIsNotWrittenAgain(t *testing.T) {
	a, st, ids, au := fwFixture(t, "10.0.0.1")
	a.onFirmwareRequest(get200("10.0.0.1"))
	a.bgWG.Wait()
	first := reachOf(t, st, ids[0])
	a.fwReach.clk = &fwClock{now: first.At.Add(time.Hour)}
	a.onFirmwareRequest(get200("10.0.0.1"))
	a.bgWG.Wait()
	if r := reachOf(t, st, ids[0]); !r.At.Equal(first.At) || len(au.ev) != 1 {
		t.Fatalf("rewritten: %+v, %d audit rows", r, len(au.ev))
	}
}

// Why: a download update that succeeded is the strongest proof, but push jobs, policy jobs and
// the experimental MQTT upload never touch the firmware server and must not claim it.
func TestOnlyASucceededDownloadJobSetsYes(t *testing.T) {
	for _, tc := range []struct {
		name string
		job  model.Job
		want model.FWReachState
	}{
		{"download succeeded", model.Job{State: model.JobSucceeded, Plan: model.PlanDirect}, model.FWReachYes},
		{"skipped, already on target", model.Job{State: model.JobSucceeded}, model.FWReachUnknown},
		{"download failed", model.Job{State: model.JobFailed}, model.FWReachUnknown},
		{"still running", model.Job{State: model.JobAwaitReboot}, model.FWReachUnknown},
		{"push", model.Job{State: model.JobSucceeded, Plan: model.PlanDirect, Delivery: model.DeliveryPush}, model.FWReachUnknown},
		{"mqtt upload", model.Job{State: model.JobSucceeded, Plan: model.PlanDirect, MQTTUpload: true}, model.FWReachUnknown},
		{"policy job", model.Job{State: model.JobSucceeded, Plan: model.PlanDirect, Kind: model.KindPolicy}, model.FWReachUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, st, ids, au := fwFixture(t, "10.0.0.1")
			tc.job.DeviceID = ids[0]
			a.learnFWReachFromJob(tc.job)
			a.bgWG.Wait()
			if r := reachOf(t, st, ids[0]); r.State != tc.want {
				t.Fatalf("%+v", r)
			}
			if tc.want == model.FWReachYes {
				if s, src := au.last(t); s != "yes" || src != FWReachSourceOTADownload {
					t.Fatalf("audit %q %q", s, src)
				}
			}
		})
	}
}

// Why: Forget is the way out of a stale "no" (a fixed firewall); it is audited with the person.
func TestForgetResetsToUnknownAndNamesTheActor(t *testing.T) {
	a, st, ids, au := fwFixture(t, "10.0.0.1")
	if err := a.fwReach.NoteFWReachNo(t.Context(), ids[0], "x"); err != nil {
		t.Fatal(err)
	}
	if err := a.fwReach.Forget(t.Context(), "alice", ids[0]); err != nil {
		t.Fatal(err)
	}
	if r := reachOf(t, st, ids[0]); r != (model.FWReach{}) {
		t.Fatalf("%+v", r)
	}
	s, src := au.last(t)
	if s != "unknown" || src != FWReachSourceAdminForget || au.ev[len(au.ev)-1].Actor != "alice" {
		t.Fatalf("audit %q %q %+v", s, src, au.ev[len(au.ev)-1])
	}
}

// Why: Address and the reported IP disagreeing means one of them is stale (DHCP moved the device);
// the request may come from another device now holding the old address, so nothing is learned. The
// device still counts toward "several devices", so a clean owner of the same IP is not guessed.
func TestFirmwareGetWithDisagreeingStoredIPsLearnsNothing(t *testing.T) {
	a, st, ids, _ := fwFixture(t, "10.0.0.1", "10.0.0.5")
	if err := st.UpdateNet(t.Context(), ids[0], store.ProbeGuard{Address: "10.0.0.1", Port: 80}, model.NetFacts{IP: "10.0.0.9", ReadAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	a.onFirmwareRequest(get200("10.0.0.1"))
	a.bgWG.Wait()
	if r := reachOf(t, st, ids[0]); r.State != model.FWReachUnknown {
		t.Fatalf("stale device learned: %+v", r)
	}
}

// Why: a staged ESP8266 update fetches twice; the second GET must not write and audit a second yes.
func TestConcurrentYesAuditsOnce(t *testing.T) {
	a, _, ids, au := fwFixture(t, "10.0.0.1")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { _ = a.fwReach.NoteYes(t.Context(), ids[0], FWReachSourceFirmwareServer) })
	}
	wg.Wait()
	if len(au.ev) != 1 {
		t.Fatalf("%d audit rows", len(au.ev))
	}
}

// Why (D9, D11): the job that ended on the device's own "connection failed" report is the one source of
// "no"; a failure for any other reason, or any other event, must not teach it, or a flaky download would
// block the device's update paths for 24 hours.
func TestLearnFWReachNoOnlyFromTheConnectionFailedReport(t *testing.T) {
	report := model.JobEvent{Event: string(ota.EvUpgradeReport), Detail: "Failed HTTP error: connection failed"}
	for _, tc := range []struct {
		name string
		job  model.Job
		ev   model.JobEvent
		want model.FWReachState
	}{
		{"connection failed", model.Job{State: model.JobFailed}, report, model.FWReachNo},
		{"other failure text", model.Job{State: model.JobFailed}, model.JobEvent{Event: string(ota.EvUpgradeReport), Detail: "Failed HTTP error: 404"}, ""},
		{"same text on another event", model.Job{State: model.JobFailed}, model.JobEvent{Event: string(ota.EvTimeout), Detail: report.Detail}, ""},
		{"job not failed", model.Job{State: model.JobSetOtaURL}, report, ""},
		{"policy job", model.Job{State: model.JobFailed, Kind: model.KindPolicy}, report, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, st, ids, _ := fwFixture(t, "10.0.0.1")
			tc.job.DeviceID = ids[0]
			a.learnFWReachNoFromJob(tc.job, tc.ev)
			a.bgWG.Wait()
			if r := reachOf(t, st, ids[0]); r.State != tc.want {
				t.Fatalf("%+v", r)
			}
		})
	}
}
