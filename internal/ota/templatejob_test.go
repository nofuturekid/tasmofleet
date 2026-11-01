package ota

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// fakeTemplates is a device pair of Template readings: what the device has now and what it has after
// the command (the device restarts in between in reality).
type fakeTemplates struct {
	mu           sync.Mutex
	h            *harness
	has          map[model.DeviceID]TemplateReading // what Read answers
	after        map[model.DeviceID]TemplateReading // what Verify reads back
	sent         map[model.DeviceID][]string
	sentState    map[model.DeviceID]model.JobState // stored job state when Send ran
	storedAtSend map[model.DeviceID]string         // the baseline event found in the store when Send ran
	verified     map[model.DeviceID]int
	verifiedWith map[model.DeviceID]string // the baseline Verify was given
	baselines    map[model.DeviceID]int
	sendErr      map[model.DeviceID]error
	verifyErr    map[model.DeviceID]error
	readErr      map[model.DeviceID]error
	baseErr      map[model.DeviceID]error
	inFlight     int
	maxFlight    int
}

func newFakeTemplates(h *harness) *fakeTemplates {
	return &fakeTemplates{h: h, has: map[model.DeviceID]TemplateReading{}, after: map[model.DeviceID]TemplateReading{},
		sent: map[model.DeviceID][]string{}, sentState: map[model.DeviceID]model.JobState{}, storedAtSend: map[model.DeviceID]string{},
		verified: map[model.DeviceID]int{}, verifiedWith: map[model.DeviceID]string{}, baselines: map[model.DeviceID]int{},
		sendErr: map[model.DeviceID]error{}, verifyErr: map[model.DeviceID]error{}, readErr: map[model.DeviceID]error{}, baseErr: map[model.DeviceID]error{}}
}

func (f *fakeTemplates) Read(_ context.Context, d model.Device) (TemplateReading, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.has[d.ID], f.readErr[d.ID]
}

func (f *fakeTemplates) Baseline(_ context.Context, d model.Device) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.baselines[d.ID]++
	if err := f.baseErr[d.ID]; err != nil {
		return "", err
	}
	return fmt.Sprintf("boot=40 dev=%d", d.ID), nil
}

func (f *fakeTemplates) Send(_ context.Context, d model.Device, command string) error {
	f.mu.Lock()
	f.inFlight++
	f.maxFlight = max(f.maxFlight, f.inFlight)
	f.sent[d.ID] = append(f.sent[d.ID], command)
	jobs, _ := f.h.st.ListJobs(context.Background(), store.JobFilter{DeviceID: d.ID})
	if len(jobs) > 0 {
		f.sentState[d.ID] = jobs[len(jobs)-1].State
		evs, _ := f.h.st.JobEvents(context.Background(), jobs[len(jobs)-1].ID)
		for _, e := range evs {
			if e.Event == "baseline" {
				f.storedAtSend[d.ID] = e.Detail
			}
		}
	}
	err := f.sendErr[d.ID]
	f.mu.Unlock()
	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()
	return err
}

func (f *fakeTemplates) Verify(_ context.Context, d model.Device, want, baseline string) (TemplateReading, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verified[d.ID]++
	f.verifiedWith[d.ID] = baseline
	return f.after[d.ID], f.verifyErr[d.ID]
}

const (
	layoutOld = "0,1|0|18"
	layoutNew = "0,2|0|18"
	cmdNew    = `Template {"NAME":"NOUS A1T","GPIO":[0,2],"FLAG":0,"BASE":18}`
)

func tplReq(id model.DeviceID) TemplateRequest {
	return TemplateRequest{DeviceID: id, Command: cmdNew, Name: "NOUS A1T", Have: layoutOld, Want: layoutNew}
}

func templateHarness(t *testing.T, n int) (*harness, *fakeTemplates, *flakyBackups, *auditRec) {
	t.Helper()
	h, _, _, aud := commandHarness(t, n)
	ft, fb := newFakeTemplates(h), &flakyBackups{fail: map[model.DeviceID]error{}}
	for i := 1; i <= n; i++ {
		id := model.DeviceID(i)
		ft.has[id] = TemplateReading{Module: 0, Name: "NOUS A1T", Layout: layoutOld, JSON: `{"NAME":"NOUS A1T","GPIO":[0,1]}`}
		ft.after[id] = TemplateReading{Module: 0, Name: "NOUS A1T", Layout: layoutNew, JSON: `{"NAME":"NOUS A1T","GPIO":[0,2]}`, RestartProven: true}
	}
	h.e.opts.Templates, h.e.backups = ft, fb
	return h, ft, fb, aud
}

func submitTemplates(t *testing.T, h *harness, reqs ...TemplateRequest) model.BatchID {
	t.Helper()
	id, skipped, err := h.e.SubmitTemplate(as("local"), "alice", TemplateStart{Group: "esp8266 NOUS A1T", Reference: 1, How: "chosen"}, reqs)
	if err != nil || len(skipped) != 0 {
		t.Fatalf("SubmitTemplate: %v %v", err, skipped)
	}
	return id
}

// Why: the whole safety story of a template change in one run. The backup comes first and is the
// "Before GPIO template" one, the restart baseline is stored BEFORE the command goes out (a restart of this
// server in between must still be able to prove the restart), the command is sent only after "applying" was
// stored, exactly once and exactly as planned, the device is read back against that baseline, and the change
// is audited with before and after.
func TestTemplateJobBacksUpSendsOnceVerifiesAndAudits(t *testing.T) {
	h, ft, fb, aud := templateHarness(t, 1)
	id := submitTemplates(t, h, tplReq(1))
	h.run(t, h.e)
	jobs, _ := h.st.ListJobs(context.Background(), store.JobFilter{BatchID: id})
	if len(jobs) != 1 || !jobs[0].IsTemplate() || jobs[0].IsUpdate() || jobs[0].State != model.JobSucceeded {
		t.Fatalf("jobs %+v", jobs)
	}
	if len(fb.runs) != 1 || fb.runs[0].Label != "Before GPIO template" {
		t.Fatalf("backup runs %+v", fb.runs)
	}
	if got := ft.sent[1]; len(got) != 1 || got[0] != cmdNew || ft.sentState[1] != model.JobApplying || ft.verified[1] != 1 {
		t.Fatalf("sent %v in state %q, verified %d", got, ft.sentState[1], ft.verified[1])
	}
	if ft.storedAtSend[1] != "boot=40 dev=1" || ft.verifiedWith[1] != "boot=40 dev=1" {
		t.Fatalf("baseline stored at send %q, given to verify %q", ft.storedAtSend[1], ft.verifiedWith[1])
	}
	want := []model.JobState{model.JobBackup, model.JobApplying, model.JobApplying, model.JobVerify, model.JobSucceeded} // the second applying is the baseline event
	if got := states(h, jobs[0].ID); strings.Join(strs(got), ",") != strings.Join(strs(want), ",") {
		t.Fatalf("states %v", got)
	}
	aud.mu.Lock()
	defer aud.mu.Unlock()
	if len(aud.evs) != 2 || aud.evs[0].Action != "policy.template.start" || aud.evs[1].Action != "policy.template" ||
		aud.evs[1].Actor != "alice" || aud.evs[1].Target != "device:1" || aud.evs[1].Result != "ok" ||
		!strings.Contains(aud.evs[1].Detail, `"before"`) || !strings.Contains(aud.evs[1].Detail, `"after"`) ||
		!strings.Contains(aud.evs[0].Detail, "NOUS A1T") {
		t.Fatalf("audit %+v", aud.evs)
	}
}

func strs(s []model.JobState) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = string(v)
	}
	return out
}

// Why: no backup, no change (E2). A device whose backup fails keeps its old pin map: nothing may be
// sent, the job fails with the backup's reason.
func TestTemplateJobSendsNothingWhenTheBackupFails(t *testing.T) {
	h, ft, fb, _ := templateHarness(t, 1)
	fb.fail[1] = errors.New("the device is unreachable")
	submitTemplates(t, h, tplReq(1))
	h.run(t, h.e)
	j := h.job(t, 1)
	if j.State != model.JobFailed || !strings.Contains(j.LastError, "unreachable") || len(ft.sent[1]) != 0 {
		t.Fatalf("job %s %q sent %v", j.State, j.LastError, ft.sent)
	}
}

// Why: the preview was judged on the device's template at that time. If somebody changed it since
// (console, another tool), the plan no longer describes what is about to happen, so nothing is sent.
func TestTemplateJobFailsWhenTheDeviceChangedSincePreview(t *testing.T) {
	h, ft, _, _ := templateHarness(t, 1)
	ft.has[1] = TemplateReading{Module: 0, Name: "NOUS A1T", Layout: "0,9|0|18"}
	submitTemplates(t, h, tplReq(1))
	h.run(t, h.e)
	j := h.job(t, 1)
	if j.State != model.JobFailed || !strings.Contains(j.LastError, "changed since the preview") || len(ft.sent[1]) != 0 {
		t.Fatalf("job %s %q sent %v", j.State, j.LastError, ft.sent)
	}
}

// Why: the device must still be on a user template (Module 0). On a built-in module the Template
// command would change nothing active and not restart; the plan assumed otherwise.
func TestTemplateJobFailsWhenTheDeviceLeftModule0(t *testing.T) {
	h, ft, _, _ := templateHarness(t, 1)
	ft.has[1] = TemplateReading{Module: 1, Name: "NOUS A1T", Layout: layoutOld}
	submitTemplates(t, h, tplReq(1))
	h.run(t, h.e)
	if j := h.job(t, 1); j.State != model.JobFailed || len(ft.sent[1]) != 0 {
		t.Fatalf("job %s %q sent %v", j.State, j.LastError, ft.sent)
	}
}

// Why (E4): a wrong reference must break one device, not the fleet. After the first failure the
// queued devices are cancelled and the page says nothing was sent to them. One at a time, even though
// the engine would allow two jobs.
func TestTemplateTransactionStopsAfterTheFirstFailure(t *testing.T) {
	h, ft, _, _ := templateHarness(t, 3)
	ft.verifyErr[1] = errors.New("after applying: the template is still the old one")
	id := submitTemplates(t, h, tplReq(1), tplReq(2), tplReq(3))
	h.run(t, h.e)
	jobs, _ := h.st.ListJobs(context.Background(), store.JobFilter{BatchID: id})
	byDev := map[model.DeviceID]model.Job{}
	for _, j := range jobs {
		byDev[j.DeviceID] = j
		if j.BatchParallel != 1 {
			t.Fatalf("job %d has Parallel %d, want 1", j.ID, j.BatchParallel)
		}
	}
	if byDev[1].State != model.JobFailed || byDev[2].State != model.JobCancelled || byDev[3].State != model.JobCancelled {
		t.Fatalf("states %s %s %s", byDev[1].State, byDev[2].State, byDev[3].State)
	}
	if !strings.Contains(byDev[2].LastError, "Nothing was sent") || len(ft.sent[2])+len(ft.sent[3]) != 0 {
		t.Fatalf("cancelled job: %q sent %v %v", byDev[2].LastError, ft.sent[2], ft.sent[3])
	}
	if ft.maxFlight != 1 {
		t.Fatalf("%d devices were changed at once", ft.maxFlight)
	}
}

// Why: without a failure, Parallel 1 still holds (restarting devices one by one); the engine's own
// MaxParallel of 2 must not widen it.
func TestTemplateTransactionRunsOneDeviceAtATime(t *testing.T) {
	h, ft, _, _ := templateHarness(t, 3)
	id := submitTemplates(t, h, tplReq(1), tplReq(2), tplReq(3))
	h.run(t, h.e)
	jobs, _ := h.st.ListJobs(context.Background(), store.JobFilter{BatchID: id})
	for _, j := range jobs {
		if j.State != model.JobSucceeded {
			t.Fatalf("job %d %s %q", j.ID, j.State, j.LastError)
		}
	}
	if ft.maxFlight != 1 {
		t.Fatalf("%d devices were changed at once", ft.maxFlight)
	}
}

// Why: the restart eats the reply, so a send error is not proof that nothing happened. The read-back
// decides: a device that has the new template is a success (noted), one that does not fails with both.
func TestTemplateJobVerifiesEvenWhenTheSendFailed(t *testing.T) {
	h, ft, _, _ := templateHarness(t, 2)
	ft.sendErr[1] = errors.New("connection reset")
	ft.sendErr[2] = errors.New("connection reset")
	ft.verifyErr[2] = errors.New("the template is still the old one")
	h.e.opts.Principals.(*fakePrincipals).roles["alice"] = model.RoleAdmin
	// separate transactions so device 2 is not cancelled by device 1
	submitTemplates(t, h, tplReq(1))
	h.run(t, h.e)
	submitTemplates(t, h, tplReq(2))
	h.run(t, h.e)
	j1, j2 := h.job(t, 1), h.job(t, 2)
	if j1.State != model.JobSucceeded || j2.State != model.JobFailed ||
		!strings.Contains(j2.LastError, "connection reset") || !strings.Contains(j2.LastError, "still the old one") {
		t.Fatalf("job1 %s %q, job2 %s %q", j1.State, j1.LastError, j2.State, j2.LastError)
	}
}

// Why: the job survives a server restart. Found in "applying" with the device already on the new
// layout, the command went out before the crash; sending it again would restart the device a second
// time for nothing. Found in "verify", only the read-back runs: no second backup, no second send.
func TestTemplateJobResumesWithoutRepeatingTheSend(t *testing.T) {
	h, ft, fb, _ := templateHarness(t, 2)
	ft.has[1] = TemplateReading{Module: 0, Name: "NOUS A1T", Layout: layoutNew}
	for _, c := range []struct {
		dev   model.DeviceID
		state model.JobState
	}{{1, model.JobApplying}, {2, model.JobVerify}} {
		h.st.put(model.Job{DeviceID: c.dev, Kind: model.KindTemplate, Commands: strings.Join(tplReq(c.dev).lines(), "\n"), State: c.state,
			CreatedBy: "alice", CreatedKind: "local", BatchParallel: 1})
	}
	h.run(t, h.e)
	if len(ft.sent[1])+len(ft.sent[2]) != 0 || len(fb.taken) != 0 || ft.verified[1] != 1 || ft.verified[2] != 1 {
		t.Fatalf("sent %v, backups %v, verified %v", ft.sent, fb.taken, ft.verified)
	}
	if h.job(t, 1).State != model.JobSucceeded || h.job(t, 2).State != model.JobSucceeded {
		t.Fatalf("states %s %s", h.job(t, 1).State, h.job(t, 2).State)
	}
}

// Why: found in "applying" with the OLD layout still on the device, the send never happened (or
// never took): it is sent now, once.
func TestTemplateJobResumedInApplyingSendsWhenTheDeviceStillHasTheOldLayout(t *testing.T) {
	h, ft, _, _ := templateHarness(t, 1)
	h.st.put(model.Job{DeviceID: 1, Kind: model.KindTemplate, Commands: strings.Join(tplReq(1).lines(), "\n"), State: model.JobApplying,
		CreatedBy: "alice", CreatedKind: "local", BatchParallel: 1})
	h.run(t, h.e)
	if len(ft.sent[1]) != 1 || h.job(t, 1).State != model.JobSucceeded {
		t.Fatalf("sent %v state %s", ft.sent, h.job(t, 1).State)
	}
}

// Why: only an active admin may change a pin map, also when the account lost the role after the
// preview. Nothing is sent and nothing is backed up for the refused job.
func TestTemplateJobNeedsAnActiveAdmin(t *testing.T) {
	h, ft, fb, _ := templateHarness(t, 1)
	submitTemplates(t, h, tplReq(1))
	h.e.opts.Principals.(*fakePrincipals).roles["alice"] = model.RoleOperator
	h.run(t, h.e)
	if j := h.job(t, 1); j.State != model.JobFailed || len(ft.sent[1]) != 0 || len(fb.taken) != 0 {
		t.Fatalf("job %s %q sent %v backups %v", j.State, j.LastError, ft.sent, fb.taken)
	}
}

// Why: a stored command is replayed by "Retry"; one that is not the single canonical Template
// command (another command, a Backlog, a changed text) must never reach the device.
func TestSubmitTemplateRefusesAnythingButOneTemplateCommand(t *testing.T) {
	h, _, _, _ := templateHarness(t, 1)
	for _, cmd := range []string{"Restart 1", `Backlog Template {"NAME":"x"}; Restart 1`, "Template", `Template {"NAME":"a"};Restart 1`, "",
		// a Template key that is not one of the five TasmoFleet builds: CMND would run as a Backlog on the device (T8)
		`Template {"NAME":"a","GPIO":[0],"FLAG":0,"BASE":18,"CMND":"Restart 1"}`,
		`Template {"NAME":"a","GPIO":[0],"FLAG":0,"BASE":18,"cmnd":"Restart 1"}`,
		`Template {"NAME":"a","GPIO":[0],"FLAG":0,"BASE":18,"EXTRA":1}`,
		`Template {"NAME":"a","NAME":"b","GPIO":[0],"FLAG":0,"BASE":18}`, // a repeated key: which one wins is up to the parser
		`Template {"GPIO":[0],"FLAG":0,"BASE":18}`,                       // a missing key keeps the device's old value (T6)
		`Template {"NAME":"a","GPIO":[0],"FLAG":0,"BASE":18} {}`,
		`Template []`, `Template {"NAME":"a","GPIO":"0","FLAG":0,"BASE":18}`} {
		r := tplReq(1)
		r.Command = cmd
		_, skipped, err := h.e.SubmitTemplate(as("local"), "alice", TemplateStart{}, []TemplateRequest{r})
		if err == nil || len(skipped) != 1 {
			t.Errorf("%q accepted: %v %v", cmd, err, skipped)
		}
	}
}

// Why: a device with an active job (update, command, ...) must not get a template on top of it.
func TestSubmitTemplateSkipsBusyDevices(t *testing.T) {
	h, _, _, _ := templateHarness(t, 2)
	h.st.put(model.Job{DeviceID: 1, Kind: model.KindCommand, Commands: "Power1 ON", State: model.JobQueued, CreatedBy: "alice", CreatedKind: "local"})
	_, skipped, err := h.e.SubmitTemplate(as("local"), "alice", TemplateStart{}, []TemplateRequest{tplReq(1), tplReq(2)})
	if err != nil || len(skipped) != 1 || skipped[0].DeviceID != 1 {
		t.Fatalf("%v %+v", err, skipped)
	}
}

// Why: the applier's read-back is trusted only after the engine checked it too: a reading that is not
// Module 0 with the planned layout is a failure even when the applier reported no error.
func TestTemplateJobDoesNotTrustAReadBackThatDiffers(t *testing.T) {
	h, ft, _, _ := templateHarness(t, 2)
	ft.after[1] = TemplateReading{Module: 0, Name: "NOUS A1T", Layout: layoutOld, RestartProven: true}
	ft.after[2] = TemplateReading{Module: 2, Name: "NOUS A1T", Layout: layoutNew, RestartProven: true}
	for _, id := range []model.DeviceID{1, 2} {
		submitTemplates(t, h, tplReq(id))
		h.run(t, h.e)
		if j := h.job(t, model.JobID(id)); j.State != model.JobFailed {
			t.Fatalf("device %d: job %s %q", id, j.State, j.LastError)
		}
	}
}

// Why: a failed template transaction is never replayed. Its plan (the layouts "have" and "want") belongs to
// the preview the person confirmed; "Check again" opens a fresh preview with a new read and a new digest.
// A generic retry would send a stale plan to a device that may have changed since.
func TestRetryRefusesATemplateTransaction(t *testing.T) {
	h, ft, _, _ := templateHarness(t, 2)
	ft.verifyErr[1] = errors.New("still the old template")
	id := submitTemplates(t, h, tplReq(1), tplReq(2))
	h.run(t, h.e)
	before := len(h.st.jobs)
	_, err := h.e.SubmitRetry(as("local"), "alice", id, nil)
	if !errors.Is(err, ErrNotRetryable) || len(h.st.jobs) != before {
		t.Fatalf("err %v, jobs %d -> %d", err, before, len(h.st.jobs))
	}
}

// Why (M2): the command is replayed from the stored job, so it must be exactly what TasmoFleet builds. The
// five keys NAME, ARCH, GPIO, FLAG, BASE are allowed, in any order, with or without ARCH.
func TestSubmitTemplateAcceptsTheBuiltForms(t *testing.T) {
	h, _, _, _ := templateHarness(t, 1)
	for _, cmd := range []string{cmdNew, `Template {"NAME":"NOUS A1T","ARCH":"ESP8266","GPIO":[0,2],"FLAG":0,"BASE":18}`} {
		r := tplReq(1)
		r.Command = cmd
		if _, skipped, err := h.e.SubmitTemplate(as("local"), "alice", TemplateStart{}, []TemplateRequest{r}); err != nil || len(skipped) != 0 {
			t.Errorf("%q refused: %v %v", cmd, err, skipped)
		}
		h.st.jobs = map[model.JobID]model.Job{} // the next submit must not find this one active
	}
}

// Why (M3): the restart baseline lives with the job. A server restart while the device restarts leaves
// the job in "verify" (or "applying") with the baseline event; the read-back must use it, or the restart
// could never be proven and a job would pass on the layout alone.
func TestTemplateJobResumeUsesTheStoredBaseline(t *testing.T) {
	h, ft, fb, _ := templateHarness(t, 2)
	ft.has[1] = TemplateReading{Module: 0, Name: "NOUS A1T", Layout: layoutNew}
	for _, c := range []struct {
		dev   model.DeviceID
		state model.JobState
	}{{1, model.JobApplying}, {2, model.JobVerify}} {
		id := h.st.put(model.Job{DeviceID: c.dev, Kind: model.KindTemplate, Commands: strings.Join(tplReq(c.dev).lines(), "\n"), State: c.state,
			CreatedBy: "alice", CreatedKind: "local", BatchParallel: 1})
		h.st.events = append(h.st.events, model.JobEvent{JobID: id, From: model.JobApplying, To: model.JobApplying, Event: "baseline", Detail: fmt.Sprintf("stored-%d", c.dev)})
	}
	h.run(t, h.e)
	if ft.verifiedWith[1] != "stored-1" || ft.verifiedWith[2] != "stored-2" || len(ft.sent[1])+len(ft.sent[2]) != 0 || len(fb.taken) != 0 || ft.baselines[1]+ft.baselines[2] != 0 {
		t.Fatalf("verified with %v, sent %v, new baselines %v", ft.verifiedWith, ft.sent, ft.baselines)
	}
}

// Why (M3): when nothing can prove the restart after all, the job must say so. A verify that returns the
// right layout but no proof (an unusable baseline) is a success with a note, never a plain "verified".
func TestTemplateJobNamesAnUnprovenRestart(t *testing.T) {
	h, ft, _, aud := templateHarness(t, 1)
	ft.after[1] = TemplateReading{Module: 0, Name: "NOUS A1T", Layout: layoutNew, JSON: `{}`, RestartProven: false}
	submitTemplates(t, h, tplReq(1))
	h.run(t, h.e)
	evs, _ := h.st.JobEvents(context.Background(), 1)
	last := evs[len(evs)-1]
	if h.job(t, 1).State != model.JobSucceeded || !strings.Contains(last.Detail, "restart could not be proven") {
		t.Fatalf("state %s, last event %+v", h.job(t, 1).State, last)
	}
	aud.mu.Lock()
	defer aud.mu.Unlock()
	if !strings.Contains(aud.evs[len(aud.evs)-1].Detail, "restart could not be proven") {
		t.Fatalf("audit %+v", aud.evs)
	}
}

// Why (L3): found in "applying" with a stored baseline and a device that does not answer, the command may
// have gone out just before the server stopped. The job must not claim "nothing was sent"; it goes on to the
// read-back with the stored baseline and says the outcome was unknown.
func TestTemplateJobResumedInApplyingWithAnUnreachableDeviceVerifies(t *testing.T) {
	h, ft, _, _ := templateHarness(t, 1)
	ft.readErr[1] = errors.New("connection refused")
	id := h.st.put(model.Job{DeviceID: 1, Kind: model.KindTemplate, Commands: strings.Join(tplReq(1).lines(), "\n"), State: model.JobApplying,
		CreatedBy: "alice", CreatedKind: "local", BatchParallel: 1})
	h.st.events = append(h.st.events, model.JobEvent{JobID: id, From: model.JobApplying, To: model.JobApplying, Event: "baseline", Detail: "stored-1"})
	h.run(t, h.e)
	j := h.job(t, 1)
	evs, _ := h.st.JobEvents(context.Background(), id)
	var unknown bool
	for _, e := range evs {
		unknown = unknown || strings.Contains(e.Detail, "unknown")
	}
	if j.State != model.JobSucceeded || len(ft.sent[1]) != 0 || ft.verifiedWith[1] != "stored-1" || !unknown {
		t.Fatalf("job %s %q sent %v verified with %q, unknown noted %v", j.State, j.LastError, ft.sent, ft.verifiedWith[1], unknown)
	}
	// and without a stored baseline nothing can have been sent, which the job may say
	h2, ft2, _, _ := templateHarness(t, 1)
	ft2.readErr[1] = errors.New("connection refused")
	h2.st.put(model.Job{DeviceID: 1, Kind: model.KindTemplate, Commands: strings.Join(tplReq(1).lines(), "\n"), State: model.JobApplying,
		CreatedBy: "alice", CreatedKind: "local", BatchParallel: 1})
	h2.run(t, h2.e)
	if j := h2.job(t, 1); j.State != model.JobFailed || !strings.Contains(j.LastError, "nothing was sent") || len(ft2.sent[1]) != 0 {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
}

// Why (M3): the baseline is stored before the send. If it cannot be read or stored, nothing is sent: a
// change that could not be proven afterwards is the case to avoid.
func TestTemplateJobSendsNothingWithoutABaseline(t *testing.T) {
	h, ft, _, _ := templateHarness(t, 1)
	ft.baseErr[1] = errors.New("no boot count")
	submitTemplates(t, h, tplReq(1))
	h.run(t, h.e)
	if j := h.job(t, 1); j.State != model.JobFailed || len(ft.sent[1]) != 0 || !strings.Contains(j.LastError, "nothing was sent") {
		t.Fatalf("job %s %q sent %v", j.State, j.LastError, ft.sent)
	}
}

// Why (L4): the re-read compares the NAME too. A device renamed since the preview must not be silently
// renamed back by a template that carries the reference's NAME.
func TestTemplateJobFailsWhenTheDeviceWasRenamedSincePreview(t *testing.T) {
	h, ft, _, _ := templateHarness(t, 1)
	ft.has[1] = TemplateReading{Module: 0, Name: "My own name", Layout: layoutOld}
	submitTemplates(t, h, tplReq(1))
	h.run(t, h.e)
	if j := h.job(t, 1); j.State != model.JobFailed || !strings.Contains(j.LastError, "renamed") || len(ft.sent[1]) != 0 {
		t.Fatalf("job %s %q sent %v", j.State, j.LastError, ft.sent)
	}
}

// failList makes listing the jobs of a transaction fail, like a database error would.
type failList struct {
	*fakeStore
	fail bool
}

func (f *failList) ListJobs(ctx context.Context, fl store.JobFilter) ([]model.Job, error) {
	if f.fail && fl.BatchID != 0 {
		return nil, errors.New("database is locked")
	}
	return f.fakeStore.ListJobs(ctx, fl)
}

// Why (L1): whether an earlier device of the transaction failed is the stop rule. If that cannot be looked
// up, the job does not guess "no": it is cancelled and sends nothing.
func TestTemplateJobIsCancelledWhenTheTransactionCannotBeListed(t *testing.T) {
	h, ft, _, _ := templateHarness(t, 1)
	submitTemplates(t, h, tplReq(1))
	h.e.jobs = &failList{fakeStore: h.st, fail: true}
	h.run(t, h.e)
	if j := h.job(t, 1); j.State != model.JobCancelled || len(ft.sent[1]) != 0 || !strings.Contains(j.LastError, "Nothing was sent") {
		t.Fatalf("job %s %q sent %v", j.State, j.LastError, ft.sent)
	}
}

// Why (L5): a device that was not touched because an earlier one failed still gets its audit record, with
// the same sentence the page shows, so the log answers "why did device 2 not change".
func TestCancelledTemplateJobsAreAudited(t *testing.T) {
	h, ft, _, aud := templateHarness(t, 2)
	ft.verifyErr[1] = errors.New("still the old template")
	submitTemplates(t, h, tplReq(1), tplReq(2))
	h.run(t, h.e)
	aud.mu.Lock()
	defer aud.mu.Unlock()
	var got bool
	for _, e := range aud.evs {
		got = got || (e.Action == "policy.template" && e.Target == "device:2" && strings.Contains(e.Result, "Nothing was sent"))
	}
	if !got {
		t.Fatalf("audit %+v", aud.evs)
	}
}
