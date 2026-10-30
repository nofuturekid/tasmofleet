package ota

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// fakeApplier records what the engine asked of the device.
type fakeApplier struct {
	mu        sync.Mutex
	applied   map[model.DeviceID][][]string
	verified  map[model.DeviceID]int
	applyErr  map[model.DeviceID]error
	verifyErr map[model.DeviceID]error
	inFlight  int
	maxFlight int
	hold      chan struct{} // when set, Apply blocks until it is closed
	// verifiedLines are the lines of the latest Verify
	verifiedLines []string
}

func newApplier() *fakeApplier {
	return &fakeApplier{applied: map[model.DeviceID][][]string{}, verified: map[model.DeviceID]int{}, applyErr: map[model.DeviceID]error{}, verifyErr: map[model.DeviceID]error{}}
}

func (a *fakeApplier) Apply(_ context.Context, d model.Device, lines []string) error {
	a.mu.Lock()
	a.inFlight++
	a.maxFlight = max(a.maxFlight, a.inFlight)
	a.applied[d.ID] = append(a.applied[d.ID], lines)
	hold, err := a.hold, a.applyErr[d.ID]
	a.mu.Unlock()
	if hold != nil {
		<-hold
	}
	a.mu.Lock()
	a.inFlight--
	a.mu.Unlock()
	return err
}

func (a *fakeApplier) Verify(_ context.Context, d model.Device, lines []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.verified[d.ID]++
	a.verifiedLines = lines
	return a.verifyErr[d.ID]
}

func policyHarness(t *testing.T, n int, maxParallel int) (*harness, *fakeApplier, *auditRec) {
	t.Helper()
	h := newHarness(t, maxParallel, failingDevices(n)...)
	ap, aud := newApplier(), &auditRec{}
	h.e.opts.Policy, h.e.audit = ap, aud
	return h, ap, aud
}

func states(h *harness, job model.JobID) []model.JobState {
	h.st.mu.Lock()
	defer h.st.mu.Unlock()
	var out []model.JobState
	for _, ev := range h.st.events {
		if ev.JobID == job {
			out = append(out, ev.To)
		}
	}
	return out
}

// Why: "Make compliant" must be a Transaction like an update: the commands of
// the plan reach the applier unchanged and in order, the job walks
// queued -> applying -> verify -> succeeded, and the change is audited with the
// admin who started it (not "system").
func TestPolicyJobAppliesVerifiesAndAudits(t *testing.T) {
	h, ap, aud := policyHarness(t, 1, 2)
	id, skipped, err := h.e.SubmitPolicy(context.Background(), "alice", []PolicyRequest{{DeviceID: 1, Commands: []string{"PowerOnState 0", "SaveData 0"}}})
	if err != nil || len(skipped) != 0 {
		t.Fatalf("%v %v", err, skipped)
	}
	h.run(t, h.e)
	jobs, _ := h.st.ListJobs(context.Background(), store.JobFilter{BatchID: id})
	if len(jobs) != 1 || jobs[0].State != model.JobSucceeded || !jobs[0].IsPolicy() {
		t.Fatalf("jobs %+v", jobs)
	}
	if got := ap.applied[1]; len(got) != 1 || strings.Join(got[0], "|") != "PowerOnState 0|SaveData 0" || ap.verified[1] != 1 {
		t.Fatalf("applied %v, verified %d", got, ap.verified[1])
	}
	want := []model.JobState{model.JobApplying, model.JobVerify, model.JobSucceeded}
	if got := states(h, jobs[0].ID); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("states %v", got)
	}
	aud.mu.Lock()
	defer aud.mu.Unlock()
	if len(aud.evs) != 2 || aud.evs[0].Action != "policy.fix.start" || aud.evs[1].Action != "policy.fix" || aud.evs[1].Actor != "alice" ||
		aud.evs[1].Target != "device:1" || aud.evs[1].Result != "ok" || !strings.Contains(aud.evs[1].Detail, "PowerOnState 0") ||
		!strings.Contains(aud.evs[1].Detail, `"via":"http"`) { // D5: which transport carried the writes
		t.Fatalf("audit %+v", aud.evs)
	}
}

// Why: a device that refuses the commands, or that does not show the wanted
// values afterwards, must fail loudly with the reason; "sent" is not "fixed".
func TestPolicyJobFailsWhenApplyOrVerifyFails(t *testing.T) {
	h, ap, aud := policyHarness(t, 2, 2)
	ap.applyErr[1] = errors.New("sending Backlog: the device is unreachable")
	ap.verifyErr[2] = errors.New("after applying: SetOption0 is 1, expected 0")
	_, _, err := h.e.SubmitPolicy(context.Background(), "alice", []PolicyRequest{
		{DeviceID: 1, Commands: []string{"SetOption0 0"}}, {DeviceID: 2, Commands: []string{"SetOption0 0"}}})
	if err != nil {
		t.Fatal(err)
	}
	h.run(t, h.e)
	j1, j2 := h.job(t, 1), h.job(t, 2)
	if j1.State != model.JobFailed || !strings.Contains(j1.LastError, "unreachable") || ap.verified[1] != 0 {
		t.Fatalf("apply failure: %+v verified %d (must not verify what was never sent)", j1, ap.verified[1])
	}
	if j2.State != model.JobFailed || !strings.Contains(j2.LastError, "expected 0") {
		t.Fatalf("verify failure: %+v", j2)
	}
	aud.mu.Lock()
	defer aud.mu.Unlock()
	bad := 0
	for _, e := range aud.evs {
		if e.Action == "policy.fix" && e.Result != "ok" {
			bad++
		}
	}
	if bad != 2 {
		t.Fatalf("failed fixes must be audited with their reason: %+v", aud.evs)
	}
}

// Why: "Retry failed" re-sends exactly what the transaction was meant to do,
// as a NEW batch linked to the original; the failed job stays as history.
func TestPolicyRetryReplaysTheSameCommands(t *testing.T) {
	h, ap, _ := policyHarness(t, 2, 2)
	ap.applyErr[1] = errors.New("boom")
	orig, _, err := h.e.SubmitPolicy(context.Background(), "alice", []PolicyRequest{
		{DeviceID: 1, Commands: []string{"PowerOnState 0", "SetOption0 0"}}, {DeviceID: 2, Commands: []string{"SaveData 0"}}})
	if err != nil {
		t.Fatal(err)
	}
	h.run(t, h.e)
	delete(ap.applyErr, 1)
	res, err := h.e.SubmitRetry(context.Background(), "bob", orig, nil)
	if err != nil || len(res.Devices) != 1 || res.Devices[0] != 1 {
		t.Fatalf("only the failed device is retried: %+v %v", res, err)
	}
	if b, _ := h.st.GetBatch(context.Background(), res.Batch); b.RetryOf != orig {
		t.Fatalf("batch %+v", b)
	}
	h.run(t, h.e)
	if got := ap.applied[1]; len(got) != 2 || strings.Join(got[1], "|") != "PowerOnState 0|SetOption0 0" {
		t.Fatalf("retry sent %v", got)
	}
	if len(ap.applied[2]) != 1 {
		t.Fatalf("the device that worked was touched again: %v", ap.applied[2])
	}
	jobs, _ := h.st.ListJobs(context.Background(), store.JobFilter{BatchID: res.Batch})
	if len(jobs) != 1 || jobs[0].State != model.JobSucceeded || jobs[0].CreatedBy != "bob" {
		t.Fatalf("retry job %+v", jobs)
	}
	if h.job(t, 1).State != model.JobFailed {
		t.Fatal("the original job must stay failed")
	}
}

// Why: a device with a running update (or any active job) must not get a
// second writer; it is skipped with the reason while the others still start.
func TestSubmitPolicySkipsBusyAndEmptyDevices(t *testing.T) {
	h, _, _ := policyHarness(t, 4, 2)
	h.st.put(model.Job{DeviceID: 1, State: model.JobUpgradeIssued, CreatedBy: "x"})
	id, skipped, err := h.e.SubmitPolicy(context.Background(), "alice", []PolicyRequest{
		{DeviceID: 1, Commands: []string{"SaveData 0"}}, {DeviceID: 2, Commands: []string{"SaveData 0"}},
		{DeviceID: 2, Commands: []string{"SaveData 0"}}, {DeviceID: 3},
	})
	if err != nil || id == 0 {
		t.Fatalf("%v", err)
	}
	reasons := map[model.DeviceID]string{}
	for _, s := range skipped {
		reasons[s.DeviceID] = s.Reason
	}
	if !strings.Contains(reasons[1], "active job") || reasons[3] != "nothing to apply" || len(skipped) != 3 {
		t.Fatalf("skipped %+v", skipped)
	}
	jobs, _ := h.st.ListJobs(context.Background(), store.JobFilter{BatchID: id})
	if len(jobs) != 1 || jobs[0].DeviceID != 2 {
		t.Fatalf("jobs %+v", jobs)
	}
	if _, _, err := h.e.SubmitPolicy(context.Background(), "alice", []PolicyRequest{{DeviceID: 1, Commands: []string{"SaveData 0"}}}); !errors.Is(err, ErrNothingToFix) {
		t.Fatalf("err %v", err)
	}
}

// Why: the parallelism setting protects the network; policy jobs share it
// with updates instead of bypassing it.
func TestPolicyJobsRespectMaxParallel(t *testing.T) {
	h, ap, _ := policyHarness(t, 5, 2)
	ap.hold = make(chan struct{})
	var reqs []PolicyRequest
	for i := 1; i <= 5; i++ {
		reqs = append(reqs, PolicyRequest{DeviceID: model.DeviceID(i), Commands: []string{"SaveData 0"}})
	}
	if _, _, err := h.e.SubmitPolicy(context.Background(), "alice", reqs); err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(50 * time.Millisecond); close(ap.hold) }()
	h.run(t, h.e)
	if ap.maxFlight != 2 {
		t.Fatalf("%d devices were written at the same time, want 2", ap.maxFlight)
	}
}

// Why: a job that already sends commands cannot be stopped halfway without
// leaving a device with unsaved changes, so cancel says no; a queued one is
// cancelled normally.
func TestPolicyCancelOnlyBeforeItStarts(t *testing.T) {
	h, ap, _ := policyHarness(t, 2, 1)
	ap.hold = make(chan struct{})
	id, _, _ := h.e.SubmitPolicy(context.Background(), "alice", []PolicyRequest{
		{DeviceID: 1, Commands: []string{"SaveData 0"}}, {DeviceID: 2, Commands: []string{"SaveData 0"}}})
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.e.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for h.job(t, 1).State != model.JobApplying {
		if time.Now().After(deadline) {
			t.Fatal("job 1 never started")
		}
		time.Sleep(time.Millisecond)
	}
	if err := h.e.Cancel(context.Background(), "alice", 1); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("running job: %v", err)
	}
	if err := h.e.Cancel(context.Background(), "alice", 2); err != nil {
		t.Fatalf("queued job: %v", err)
	}
	close(ap.hold)
	for !h.job(t, 1).State.Terminal() {
		time.Sleep(time.Millisecond)
	}
	stop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if h.job(t, 1).State != model.JobSucceeded || h.job(t, 2).State != model.JobCancelled || len(ap.applied[2]) != 0 {
		t.Fatalf("states %s %s, device 2 applied %v", h.job(t, 1).State, h.job(t, 2).State, ap.applied[2])
	}
	_ = id
}

// Why: after a restart the engine resumes active jobs. Commands set absolute
// values, so an interrupted "applying" job is safe to run again, but one that
// was already verifying must only be read back, never re-sent.
func TestPolicyJobResumes(t *testing.T) {
	h, ap, _ := policyHarness(t, 2, 2)
	for i, st := range []model.JobState{model.JobApplying, model.JobVerify} {
		h.st.put(model.Job{DeviceID: model.DeviceID(i + 1), Kind: model.KindPolicy, Commands: "SaveData 0", State: st, CreatedBy: "alice"})
	}
	h.run(t, h.e)
	if h.job(t, 1).State != model.JobSucceeded || h.job(t, 2).State != model.JobSucceeded {
		t.Fatalf("%s %s", h.job(t, 1).State, h.job(t, 2).State)
	}
	if len(ap.applied[1]) != 1 || ap.verified[1] != 1 {
		t.Fatalf("interrupted apply: applied %v verified %d", ap.applied[1], ap.verified[1])
	}
	if len(ap.applied[2]) != 0 || ap.verified[2] != 1 {
		t.Fatalf("interrupted verify must not re-send: applied %v verified %d", ap.applied[2], ap.verified[2])
	}
}

// Why: an engine without an applier must say so on the job instead of leaving
// it queued forever or pretending success.
func TestPolicyJobWithoutApplierFails(t *testing.T) {
	h, _, _ := policyHarness(t, 1, 1)
	h.e.opts.Policy = nil
	h.e.SubmitPolicy(context.Background(), "alice", []PolicyRequest{{DeviceID: 1, Commands: []string{"SaveData 0"}}}) //nolint:errcheck // checked through the job state
	h.run(t, h.e)
	if j := h.job(t, 1); j.State != model.JobFailed || j.LastError == "" {
		t.Fatalf("%+v", j)
	}
}

// Why: the audit log holds only cmdpolicy.Redacted text. A generic rule's value is an
// admin's own policy, but the log must not depend on that: harmless settings show their
// value, any other command shows as "Name ***".
func TestPolicyJobAuditUsesTheRedactedCommands(t *testing.T) {
	h, _, aud := policyHarness(t, 1, 2)
	if _, _, err := h.e.SubmitPolicy(context.Background(), "alice", []PolicyRequest{{DeviceID: 1, Commands: []string{"Sleep 20", "Pulsetime1 hunter2"}}}); err != nil {
		t.Fatal(err)
	}
	h.run(t, h.e)
	aud.mu.Lock()
	defer aud.mu.Unlock()
	last := aud.evs[len(aud.evs)-1]
	if last.Action != "policy.fix" || !strings.Contains(last.Detail, `"Sleep 20"`) || !strings.Contains(last.Detail, `"Pulsetime1 ***"`) || strings.Contains(last.Detail, "hunter2") {
		t.Fatalf("audit %+v", last)
	}
}

type skipErr struct {
	applied int
	lines   []string
}

func (e *skipErr) Error() string          { return "applied; skipped " + strings.Join(e.lines, ", ") }
func (e *skipErr) SkippedLines() []string { return e.lines }
func (e *skipErr) AppliedAny() bool       { return e.applied > 0 }

// Why (D13): Make compliant leaves out the lines that could cut off a device reached over MQTT only. The
// job still succeeds for the rest, verifies only what was sent (the skipped line would "deviate" forever),
// and the audit and the event say what was left out. When nothing at all was applied it is a failure.
func TestPolicyJobVerifiesOnlyWhatWasSentAndSaysWhatWasSkipped(t *testing.T) {
	h, ap, aud := policyHarness(t, 2, 2)
	ap.applyErr[1] = &skipErr{applied: 1, lines: []string{"Topic other"}}
	ap.applyErr[2] = &skipErr{applied: 0, lines: []string{"Topic other"}}
	id, _, err := h.e.SubmitPolicy(context.Background(), "alice", []PolicyRequest{
		{DeviceID: 1, Commands: []string{"PowerOnState 0", "Topic other"}}, {DeviceID: 2, Commands: []string{"Topic other"}}})
	if err != nil {
		t.Fatal(err)
	}
	h.run(t, h.e)
	jobs, _ := h.st.ListJobs(context.Background(), store.JobFilter{BatchID: id})
	got := map[model.DeviceID]model.JobState{}
	for _, j := range jobs {
		got[j.DeviceID] = j.State
	}
	if got[1] != model.JobSucceeded || got[2] != model.JobFailed {
		t.Fatalf("states %v", got)
	}
	if strings.Join(ap.verifiedLines, "|") != "PowerOnState 0" {
		t.Errorf("verified %v, want only the line that was sent", ap.verifiedLines)
	}
	aud.mu.Lock()
	defer aud.mu.Unlock()
	found := false
	for _, e := range aud.evs {
		if e.Action == "policy.fix" && e.Target == "device:1" && strings.Contains(e.Detail, "skipped Topic other") && !strings.Contains(e.Detail, `"Topic other"]`) {
			found = true
		}
	}
	if !found {
		t.Errorf("audit %+v", aud.evs)
	}
}

// Why: a restart while a job verifies must not verify the lines Apply left out on purpose (they would
// "deviate" for ever) and must not lose the note. The store keeps the planned commands, so the engine
// finds both in the verify event that was written before the restart.
func TestPolicyJobResumedInVerifyKeepsTheSkippedLinesAndTheNote(t *testing.T) {
	h, ap, aud := policyHarness(t, 1, 1)
	id := h.st.put(model.Job{DeviceID: 1, Kind: model.KindPolicy, Commands: "PowerOnState 0\nTopic other", State: model.JobVerify, CreatedBy: "alice"})
	h.st.events = append(h.st.events, model.JobEvent{JobID: id, From: model.JobApplying, To: model.JobVerify, Event: "verify",
		Detail: skipDetail("applied 1; skipped Topic: reached over MQTT only (use the console)", []string{"Topic other"})})
	h.run(t, h.e)
	if h.job(t, id).State != model.JobSucceeded {
		t.Fatalf("state %s", h.job(t, id).State)
	}
	if strings.Join(ap.verifiedLines, "|") != "PowerOnState 0" {
		t.Errorf("verified %v after the restart", ap.verifiedLines)
	}
	aud.mu.Lock()
	defer aud.mu.Unlock()
	for _, e := range aud.evs {
		if e.Action == "policy.fix" && strings.Contains(e.Detail, "skipped Topic") {
			return
		}
	}
	t.Errorf("the note was lost: %+v", aud.evs)
}
