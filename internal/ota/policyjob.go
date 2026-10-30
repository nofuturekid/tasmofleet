package ota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// "Make compliant" runs as a transaction: one job per device, shown and
// retried like an update. The job is not an OTA job, so it does not go through
// the OTA state machine: queued, applying (commands sent and saved), verify
// (values read back), then succeeded or failed. Every step is persisted, and a
// job found mid-way after a restart simply runs again from its start: the
// commands set absolute values, so sending them twice changes nothing.

// PolicyApplier sends a policy job's commands to a device and reads the result
// back (policy.Fixer, wrapped by the app).
type PolicyApplier interface {
	// Apply sends the command lines, batched, and saves them to flash once.
	Apply(ctx context.Context, d model.Device, lines []string) error
	// Verify reads the settings back and fails when one differs from the line.
	Verify(ctx context.Context, d model.Device, lines []string) error
}

// verifySkips is the detail of the verify event of a job whose Apply left lines out. The store keeps the
// job's commands as they were planned (a retry replays them), so the engine finds the skipped ones here
// when it resumes in the verify state.
type verifySkips struct {
	Note    string   `json:"note"`
	Skipped []string `json:"skipped"`
}

func skipDetail(note string, skipped []string) string {
	if note == "" && len(skipped) == 0 {
		return ""
	}
	b, _ := json.Marshal(verifySkips{note, skipped}) // strings only
	return string(b)
}

// skippedOf is the lines of all that are not in kept.
func skippedOf(all, kept []string) []string {
	var out []string
	for _, l := range all {
		if !slices.Contains(kept, l) {
			out = append(out, l)
		}
	}
	return out
}

// skippedBeforeVerify reads the skipped lines and the note from the job's verify event.
func (e *Engine) skippedBeforeVerify(ctx context.Context, j model.Job, lines []string) (string, []string) {
	evs, err := e.jobs.JobEvents(ctx, j.ID)
	if err != nil {
		e.log.Warn("policy job: could not read the events of a job resumed in verify", "job", j.ID, "err", err)
		return "", lines
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].To != model.JobVerify || evs[i].Detail == "" {
			continue
		}
		var v verifySkips
		if json.Unmarshal([]byte(evs[i].Detail), &v) == nil {
			return v.Note, withoutLines(lines, v.Skipped)
		}
	}
	return "", lines
}

// skippedApply is what Apply returns when it left out lines on purpose (policy.SkippedError).
type skippedApply interface {
	error
	SkippedLines() []string
	AppliedAny() bool
}

// withoutLines is lines minus the skipped ones.
func withoutLines(lines, skipped []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if !slices.Contains(skipped, l) {
			out = append(out, l)
		}
	}
	return out
}

// PolicyRequest is one device of SubmitPolicy.
type PolicyRequest struct {
	DeviceID model.DeviceID
	Commands []string
}

// ErrNothingToFix: SubmitPolicy was left with no device to work on.
var ErrNothingToFix = errors.New("ota: no device to make compliant")

const (
	policyApplyFactor = 6 // Backlog chunks and the save, in command timeouts
	policyVerifyLimit = 2 * time.Minute
)

// SubmitPolicy creates a transaction that applies the commands to the devices.
// Devices that already have an active job, are listed twice or have nothing to
// apply are not accepted; they are returned with the reason and the others
// still start. ErrNothingToFix when none is left.
func (e *Engine) SubmitPolicy(ctx context.Context, actor string, reqs []PolicyRequest) (model.BatchID, []Refusal, error) {
	id, skipped, err := e.submitPolicy(ctx, actor, model.KindPolicy, reqs, 0)
	if e.audit != nil && err == nil {
		devs := make([]model.DeviceID, 0, len(reqs))
		skip := map[model.DeviceID]bool{}
		for _, s := range skipped {
			skip[s.DeviceID] = true
		}
		for _, r := range reqs {
			if !skip[r.DeviceID] {
				devs = append(devs, r.DeviceID)
			}
		}
		detail, _ := json.Marshal(map[string]any{"devices": devs, "skipped": len(skipped)}) // a plain map always encodes
		e.audit.Record(ctx, model.AuditEvent{Actor: actor, Action: "policy.fix.start", Target: "transaction:" + strconv.FormatInt(int64(id), 10), Detail: string(detail), Result: "ok"})
	}
	return id, skipped, err
}

func (e *Engine) submitPolicy(ctx context.Context, actor string, kind model.JobKind, reqs []PolicyRequest, retryOf model.BatchID) (model.BatchID, []Refusal, error) {
	var skipped []Refusal
	seen := map[model.DeviceID]bool{}
	now := e.clk.Now()
	opts := BatchOptionsOf(ctx)
	var jobs []model.Job
	for _, r := range reqs {
		switch {
		case seen[r.DeviceID]:
			skipped = append(skipped, Refusal{r.DeviceID, "listed twice"})
			continue
		case len(r.Commands) == 0:
			skipped = append(skipped, Refusal{r.DeviceID, "nothing to apply"})
			continue
		}
		seen[r.DeviceID] = true
		act, err := e.jobs.ListJobs(ctx, store.JobFilter{DeviceID: r.DeviceID, States: activeStates})
		if err != nil {
			return 0, nil, fmt.Errorf("ota: submit policy: %w", err)
		}
		if len(act) > 0 {
			skipped = append(skipped, Refusal{r.DeviceID, fmt.Sprintf("already has active job %d", act[0].ID)})
			continue
		}
		jobs = append(jobs, model.Job{DeviceID: r.DeviceID, Kind: kind, Commands: strings.Join(r.Commands, "\n"),
			State: model.JobQueued, Stage: model.StageFinal, CreatedBy: actor, CreatedKind: actorKind(ctx), CreatedAt: now, UpdatedAt: now,
			BatchParallel: opts.Parallel, ReplyTimeout: opts.ReplyTimeout})
	}
	if len(jobs) == 0 {
		return 0, skipped, ErrNothingToFix
	}
	var id model.BatchID
	var err error
	if retryOf != 0 {
		bs, ok := e.jobs.(store.BatchStore)
		if !ok {
			return 0, nil, errors.New("ota: the job store cannot link a retry to its batch")
		}
		id, _, err = bs.CreateRetryBatch(ctx, actor, retryOf, jobs)
	} else {
		id, _, err = e.jobs.CreateBatch(ctx, actor, jobs)
	}
	if err != nil {
		return 0, nil, fmt.Errorf("ota: submit policy: %w", err)
	}
	e.signal()
	return id, skipped, nil
}

// retryPolicy is submitRetry for a batch of policy jobs: the same commands,
// as a new batch linked to the original. A device that has an active job is
// refused (nothing is created), like an update retry.
func (e *Engine) retryPolicy(ctx context.Context, actor string, batchID model.BatchID, picked []model.Job) (RetryResult, error) {
	var res RetryResult
	reqs := make([]PolicyRequest, len(picked))
	for i, j := range picked {
		if j.Kind != picked[0].Kind || j.IsUpdate() {
			return res, fmt.Errorf("ota: retry batch %d: it mixes policy, command, backup and update jobs", batchID)
		}
		reqs[i] = PolicyRequest{DeviceID: j.DeviceID, Commands: strings.Split(j.Commands, "\n")}
	}
	// refuse before creating anything
	var ref []Refusal
	if picked[0].IsCommand() || picked[0].IsBackup() {
		// The retry runs as the retrying user: an active admin, and every
		// stored command must pass the command policy for that role.
		p, err := e.currentAdmin(ctx, actorKind(ctx), actor)
		if err != nil {
			return res, err
		}
		for _, r := range reqs {
			if picked[0].IsBackup() {
				break // the stored marker is no device command
			}
			for _, c := range r.Commands {
				if _, aerr := cmdpolicy.Authorize(c, p.Role); aerr != nil {
					ref = append(ref, Refusal{r.DeviceID, "the command policy refuses it for you"})
					break
				}
			}
		}
	}
	for _, r := range reqs {
		act, err := e.jobs.ListJobs(ctx, store.JobFilter{DeviceID: r.DeviceID, States: activeStates})
		if err != nil {
			return res, fmt.Errorf("ota: retry batch %d: %w", batchID, err)
		}
		if len(act) > 0 {
			ref = append(ref, Refusal{r.DeviceID, fmt.Sprintf("already has active job %d", act[0].ID)})
		}
	}
	if len(ref) > 0 {
		return res, &RefusedError{Refused: ref}
	}
	// The retry keeps the options of the transaction it retries.
	ctx = WithBatchOptions(ctx, BatchOptions{Parallel: picked[0].BatchParallel, ReplyTimeout: picked[0].ReplyTimeout})
	id, _, err := e.submitPolicy(ctx, actor, picked[0].Kind, reqs, batchID)
	if err != nil {
		return res, err
	}
	res.Batch = id
	for _, r := range reqs {
		res.Devices = append(res.Devices, r.DeviceID)
	}
	return res, nil
}

// workPolicy drives one policy job to its end.
func (e *Engine) workPolicy(ctx context.Context, j model.Job) {
	lines := strings.Split(j.Commands, "\n")
	allLines := lines
	d, err := e.devices.GetDevice(ctx, j.DeviceID)
	if err != nil {
		e.finishPolicy(ctx, j, lines, "inventory: "+err.Error(), nil, "")
		return
	}
	if e.opts.Policy == nil {
		e.finishPolicy(ctx, j, lines, "this server cannot apply policy changes", nil, "")
		return
	}
	note := ""                      // what Apply left out on purpose
	var trace *transport.Trace      // which transport carried the writes, for the audit; nil until something is sent
	if j.State != model.JobVerify { // a job interrupted while verifying only needs the read-back
		var ok bool
		if j, ok = e.policyMove(ctx, j, model.JobApplying, "apply", ""); !ok {
			return
		}
		actx, cancel := context.WithTimeout(ctx, e.machine().CommandTimeout*policyApplyFactor)
		actx, trace = transport.WithTrace(actx)
		err = e.opts.Policy.Apply(actx, d, lines)
		cancel()
		if ctx.Err() != nil {
			return // shutting down: the job resumes from its stored state
		}
		var sk skippedApply
		if errors.As(err, &sk) && sk.AppliedAny() {
			// Lines that could cut off a device reached over MQTT only were left out on purpose: the job
			// succeeds for the rest, says so, and verifies only what was sent.
			note = err.Error()
			lines = withoutLines(lines, sk.SkippedLines())
			err = nil
		}
		if err != nil {
			e.finishPolicy(ctx, j, lines, err.Error(), trace, "")
			return
		}
	}
	var ok bool
	if j.State == model.JobVerify {
		// resumed after a restart: what Apply left out was recorded with the verify event
		note, lines = e.skippedBeforeVerify(ctx, j, lines)
	} else if j, ok = e.policyMove(ctx, j, model.JobVerify, "verify", skipDetail(note, skippedOf(allLines, lines))); !ok {
		return
	}
	vctx, cancel := context.WithTimeout(ctx, policyVerifyLimit)
	err = e.opts.Policy.Verify(vctx, d, lines)
	cancel()
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		e.finishPolicy(ctx, j, lines, err.Error(), trace, note)
		return
	}
	e.finishPolicy(ctx, j, lines, "", trace, note)
}

// finishPolicy ends the job as succeeded (failure == "") or failed, and audits
// the change.
func (e *Engine) finishPolicy(ctx context.Context, j model.Job, lines []string, failure string, trace *transport.Trace, note string) {
	to, event, detail := model.JobSucceeded, "done", note
	if failure != "" {
		to, event, detail = model.JobFailed, "failed", failure
	}
	if _, ok := e.policyMove(ctx, j, to, event, detail); !ok {
		return
	}
	if e.audit != nil {
		fields := map[string]any{"transaction": j.BatchID, "job": j.ID, "commands": redactedLines(lines)}
		if trace != nil {
			fields["via"] = trace.Via()
		}
		if note != "" {
			fields["note"] = note
		}
		detail, _ := json.Marshal(fields) // a plain map always encodes
		result := "ok"
		if failure != "" {
			result = failure
		}
		e.audit.Record(ctx, model.AuditEvent{Actor: j.CreatedBy, Action: "policy.fix", Target: "device:" + strconv.FormatInt(int64(j.DeviceID), 10), Detail: string(detail), Result: result})
	}
}

// policyMove persists a transition (write-ahead, like apply) and tells the UI.
func (e *Engine) policyMove(ctx context.Context, j model.Job, to model.JobState, event, detail string) (model.Job, bool) {
	now := e.clk.Now()
	next := j
	next.State, next.UpdatedAt = to, now
	if to == model.JobFailed {
		next.LastError = detail
	}
	jev := model.JobEvent{JobID: j.ID, At: now, From: j.State, To: to, Event: event, Detail: detail}
	if j.State == to { // resumed in the same state: nothing to write
		return j, true
	}
	if err := e.jobs.SaveTransition(ctx, next, jev); err != nil {
		e.log.Error("policy transition not persisted; job stops here and resumes from the stored state", "job", j.ID, "from", j.State, "to", to, "err", err)
		return j, false
	}
	e.notify.JobChanged(next, jev)
	return next, true
}

// redactedLines is what the audit log may hold of the commands: the allowlisted form,
// so a value of a generic rule never reaches it unless its command is known to be harmless.
func redactedLines(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = cmdpolicy.Redacted(l)
	}
	return out
}
