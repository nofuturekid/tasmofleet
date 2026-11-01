package ota

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// fakeTemplates is a device pair of Template readings: what the device has now and what it has after
// the command (the device restarts in between in reality).
type fakeTemplates struct {
	mu        sync.Mutex
	h         *harness
	has       map[model.DeviceID]TemplateReading // what Read answers until a Send happened
	after     map[model.DeviceID]TemplateReading // what Verify reads back
	sent      map[model.DeviceID][]string
	sentState map[model.DeviceID]model.JobState // stored job state when Send ran
	verified  map[model.DeviceID]int
	sendErr   map[model.DeviceID]error
	verifyErr map[model.DeviceID]error
	inFlight  int
	maxFlight int
}

func newFakeTemplates(h *harness) *fakeTemplates {
	return &fakeTemplates{h: h, has: map[model.DeviceID]TemplateReading{}, after: map[model.DeviceID]TemplateReading{},
		sent: map[model.DeviceID][]string{}, sentState: map[model.DeviceID]model.JobState{}, verified: map[model.DeviceID]int{},
		sendErr: map[model.DeviceID]error{}, verifyErr: map[model.DeviceID]error{}}
}

func (f *fakeTemplates) Read(_ context.Context, d model.Device) (TemplateReading, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.has[d.ID], nil
}

func (f *fakeTemplates) Send(_ context.Context, d model.Device, command string) error {
	f.mu.Lock()
	f.inFlight++
	f.maxFlight = max(f.maxFlight, f.inFlight)
	f.sent[d.ID] = append(f.sent[d.ID], command)
	jobs, _ := f.h.st.ListJobs(context.Background(), store.JobFilter{DeviceID: d.ID})
	if len(jobs) > 0 {
		f.sentState[d.ID] = jobs[len(jobs)-1].State
	}
	err := f.sendErr[d.ID]
	f.mu.Unlock()
	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()
	return err
}

func (f *fakeTemplates) Verify(_ context.Context, d model.Device, want string) (TemplateReading, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verified[d.ID]++
	return f.after[d.ID], f.verifyErr[d.ID]
}

const (
	layoutOld = "0,1|0|18"
	layoutNew = "0,2|0|18"
	cmdNew    = `Template {"NAME":"NOUS A1T","GPIO":[0,2],"FLAG":0,"BASE":18}`
)

func tplReq(id model.DeviceID) TemplateRequest {
	return TemplateRequest{DeviceID: id, Command: cmdNew, Have: layoutOld, Want: layoutNew}
}

func templateHarness(t *testing.T, n int) (*harness, *fakeTemplates, *flakyBackups, *auditRec) {
	t.Helper()
	h, _, _, aud := commandHarness(t, n)
	ft, fb := newFakeTemplates(h), &flakyBackups{fail: map[model.DeviceID]error{}}
	for i := 1; i <= n; i++ {
		id := model.DeviceID(i)
		ft.has[id] = TemplateReading{Module: 0, Layout: layoutOld, JSON: `{"NAME":"NOUS A1T","GPIO":[0,1]}`}
		ft.after[id] = TemplateReading{Module: 0, Layout: layoutNew, JSON: `{"NAME":"NOUS A1T","GPIO":[0,2]}`}
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
// "Before GPIO template" one, the command is sent only after "applying" was stored (a crash must never
// repeat an unrecorded send), it is sent exactly once and exactly as planned, the device is read back,
// and the change is audited with before and after.
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
	want := []model.JobState{model.JobBackup, model.JobApplying, model.JobVerify, model.JobSucceeded}
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
	ft.has[1] = TemplateReading{Module: 0, Layout: "0,9|0|18"}
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
	ft.has[1] = TemplateReading{Module: 1, Layout: layoutOld}
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
	ft.has[1] = TemplateReading{Module: 0, Layout: layoutNew}
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
	for _, cmd := range []string{"Restart 1", `Backlog Template {"NAME":"x"}; Restart 1`, "Template", `Template {"NAME":"a"};Restart 1`, ""} {
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
	ft.after[1] = TemplateReading{Module: 0, Layout: layoutOld}
	ft.after[2] = TemplateReading{Module: 2, Layout: layoutNew}
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
