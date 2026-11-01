package ota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// "Apply GPIO template" (#327) runs as a transaction of KindTemplate jobs, one per device and one
// device at a time. Each job: backup (blocking), applying (re-read Module and Template, send the one
// Template command), verify (wait for the restart, read it back). Every step is persisted before it
// runs, so a job found mid-way after a server restart continues without sending twice: the re-read in
// "applying" shows whether the command already took effect.

// TemplateReading is a live reading of a device's Module and Template.
type TemplateReading struct {
	// Module is the active module number; 0 is the user template.
	Module int
	// Name is the template NAME.
	Name string
	// Layout identifies the template's pin map (policy.GPIOTemplate.Fingerprint): GPIO, FLAG, BASE.
	Layout string
	// JSON is the Template reply, for the audit. It holds no secrets.
	JSON string
	// RestartProven is set by Verify when the restart since the baseline was proven; false when there was
	// no usable baseline (the job then says so).
	RestartProven bool
}

// TemplateApplier talks to the device for template jobs (wrapped by the app).
type TemplateApplier interface {
	// Read returns the live Module and Template.
	Read(ctx context.Context, d model.Device) (TemplateReading, error)
	// Baseline reads what a restart is measured against (the boot count) and returns it as an opaque
	// token. The engine stores the token with the job before it sends; an error means nothing is sent.
	Baseline(ctx context.Context, d model.Device) (string, error)
	// Send sends the single Template command. A device that refuses it (Invalid JSON or Bad Chip Type)
	// is an error that says nothing was applied. A lost reply is also an error, but the device restarts
	// and may not answer, so the engine verifies anyway.
	Send(ctx context.Context, d model.Device, command string) error
	// Verify waits for the device to answer again and reads it back, proving the restart against the
	// baseline token. The engine checks the result.
	Verify(ctx context.Context, d model.Device, wantLayout, baseline string) (TemplateReading, error)
}

// TemplateRequest is one device of SubmitTemplate.
type TemplateRequest struct {
	DeviceID model.DeviceID
	// Command is the one Template command, as policy.BuildTemplateCommand made it.
	Command string
	// Name is the template NAME the device had when the plan was made (and the command carries).
	Name string
	// Have is the layout the device had when the plan was made, Want the layout the command sets.
	Have, Want string
}

// TemplateStart describes the transaction for the audit.
type TemplateStart struct {
	Group     string
	Reference model.DeviceID
	How       string // "most-common" or "chosen"
}

const (
	templateNameLine = "name "
	templateHaveLine = "have "
	templateWantLine = "want "
	templateLabel    = "Before GPIO template"
	baselineEvent    = "baseline"
)

func (r TemplateRequest) lines() []string {
	return []string{r.Command, templateNameLine + r.Name, templateHaveLine + r.Have, templateWantLine + r.Want}
}

// templateLines reads a job's Commands back.
func templateLines(commands string) (cmd, name, have, want string) {
	for i, l := range strings.Split(commands, "\n") {
		switch {
		case i == 0:
			cmd = l
		case strings.HasPrefix(l, templateNameLine):
			name = strings.TrimPrefix(l, templateNameLine)
		case strings.HasPrefix(l, templateHaveLine):
			have = strings.TrimPrefix(l, templateHaveLine)
		case strings.HasPrefix(l, templateWantLine):
			want = strings.TrimPrefix(l, templateWantLine)
		}
	}
	return cmd, name, have, want
}

// templateKeys are the only keys of a Template command TasmoFleet builds. CMND, which the device would run
// as a Backlog (T8), and every key left out (the device keeps its old value, T6) are not among them.
var templateKeys = map[string]bool{"NAME": true, "ARCH": true, "GPIO": true, "FLAG": true, "BASE": true}

// templateCommandOK: exactly one Template command whose argument is a JSON object with the keys NAME,
// GPIO, FLAG, BASE and optionally ARCH, each once, in the console policy's own canonical spelling. The
// command is replayed from the stored job, so what is stored is what TasmoFleet builds.
func templateCommandOK(cmd string) bool {
	arg, found := strings.CutPrefix(cmd, "Template ")
	if !found || strings.ContainsAny(cmd, ";\n\r") {
		return false
	}
	dec := json.NewDecoder(strings.NewReader(arg))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		key, isKey := t.(string)
		var v json.RawMessage
		if err != nil || !isKey || !templateKeys[key] || seen[key] || dec.Decode(&v) != nil {
			return false
		}
		seen[key] = true
		if (key == "GPIO") != (len(v) > 0 && v[0] == '[') {
			return false
		}
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) { // nothing after the object
		return false
	}
	if !seen["NAME"] || !seen["GPIO"] || !seen["FLAG"] || !seen["BASE"] {
		return false
	}
	canon, err := cmdpolicy.Canonical(cmd)
	return err == nil && canon == cmd
}

// SubmitTemplate creates a transaction that applies a template command to each device: one at a time,
// the rest cancelled after the first failure. Devices that already have an active job, or whose request
// is not one Template command, are returned with the reason and the others still start;
// ErrNothingToFix when none is left.
func (e *Engine) SubmitTemplate(ctx context.Context, actor string, start TemplateStart, reqs []TemplateRequest) (model.BatchID, []Refusal, error) {
	var bad []Refusal
	var ok []PolicyRequest
	for _, r := range reqs {
		if !templateCommandOK(r.Command) || r.Name == "" || r.Have == "" || r.Want == "" || strings.ContainsAny(r.Name+r.Have+r.Want, "\n\r") {
			bad = append(bad, Refusal{r.DeviceID, "not a single Template command"})
			continue
		}
		ok = append(ok, PolicyRequest{DeviceID: r.DeviceID, Commands: r.lines()})
	}
	ctx = WithBatchOptions(ctx, BatchOptions{Parallel: 1, ReplyTimeout: BatchOptionsOf(ctx).ReplyTimeout})
	id, skipped, err := e.submitPolicy(ctx, actor, model.KindTemplate, ok, 0)
	skipped = append(bad, skipped...)
	if err != nil {
		return 0, skipped, err
	}
	if e.audit != nil {
		skip := map[model.DeviceID]bool{}
		for _, s := range skipped {
			skip[s.DeviceID] = true
		}
		var devs []model.DeviceID
		for _, r := range ok {
			if !skip[r.DeviceID] {
				devs = append(devs, r.DeviceID)
			}
		}
		detail, _ := json.Marshal(map[string]any{"devices": devs, "skipped": len(skipped), "group": start.Group, "reference": start.Reference, "how": start.How}) // a plain map always encodes
		e.audit.Record(ctx, model.AuditEvent{Actor: actor, Action: "policy.template.start", Target: "transaction:" + strconv.FormatInt(int64(id), 10), Detail: string(detail), Result: "ok"})
	}
	return id, skipped, nil
}

// workTemplate drives one template job to its end.
func (e *Engine) workTemplate(ctx context.Context, j model.Job) {
	cmd, name, have, want := templateLines(j.Commands)
	fail := func(reading *TemplateReading, trace *transport.Trace, format string, a ...any) {
		e.finishTemplate(ctx, j, cmd, reading, nil, trace, "", fmt.Sprintf(format, a...))
	}
	d, err := e.devices.GetDevice(ctx, j.DeviceID)
	if err != nil {
		fail(nil, nil, "inventory: %v", err)
		return
	}
	if e.opts.Templates == nil {
		fail(nil, nil, "this server cannot apply GPIO templates")
		return
	}
	var before *TemplateReading
	var trace *transport.Trace
	var sendErr error
	note := ""
	baseline := e.templateBaseline(ctx, j) // stored before the send; empty when no send was ever started
	if j.State != model.JobVerify {        // a job interrupted while verifying only needs the read-back
		if j.State == model.JobQueued {
			first, stop, err := e.failedBefore(ctx, j)
			if err != nil || stop {
				reason := "the transaction could not be looked up (" + fmt.Sprint(err) + ")"
				if err == nil {
					reason = fmt.Sprintf("the transaction stopped after job %d failed", first.ID)
				}
				e.cancelTemplate(ctx, j, "Nothing was sent: "+reason)
				return
			}
		}
		if _, err := e.currentAdmin(ctx, j.CreatedKind, j.CreatedBy); err != nil {
			e.auditCommandRefusal(ctx, j, err)
			fail(nil, nil, "%v", err)
			return
		}
		if j.State == model.JobQueued || j.State == model.JobBackup {
			if e.backups == nil {
				fail(nil, nil, "this server cannot take backups, so no template is applied")
				return
			}
			var ok bool
			if j, ok = e.policyMove(ctx, j, model.JobBackup, "backup", ""); !ok {
				return
			}
			if _, err := e.backups.Backup(backup.WithRun(ctx, backup.Run{Trigger: model.TriggerBulk, Label: templateLabel}), j.CreatedBy, j.DeviceID); err != nil {
				if ctx.Err() != nil {
					return
				}
				fail(nil, nil, "backup failed, nothing was sent: %v", err)
				return
			}
		}
		var ok bool
		if j, ok = e.policyMove(ctx, j, model.JobApplying, "apply", ""); !ok {
			return
		}
		// With a stored baseline the command may already have gone out before this server stopped.
		sentWords := "nothing was sent"
		if baseline != "" {
			sentWords = "the command may already have been sent"
		}
		rctx, cancel := context.WithTimeout(ctx, e.machine().CommandTimeout*policyApplyFactor)
		r, rerr := e.opts.Templates.Read(rctx, d)
		cancel()
		if ctx.Err() != nil {
			return
		}
		switch {
		case rerr != nil && baseline != "":
			note = "after a restart of this server the device did not answer; whether the command was sent is unknown (" + rerr.Error() + ")"
		case rerr != nil:
			fail(nil, nil, "could not read the template before sending, nothing was sent: %v", rerr)
			return
		default:
			before = &r
			switch {
			case r.Module != 0:
				fail(before, nil, "the device no longer runs a user template (module %d), %s", r.Module, sentWords)
				return
			case r.Name != name:
				fail(before, nil, "the device was renamed since the preview (%q, planned %q); a template with the reference's name would rename it back, %s", r.Name, name, sentWords)
				return
			case r.Layout == want:
				// already there: the command took effect before a restart of this server, or someone else applied it
			case r.Layout != have:
				fail(before, nil, "the template changed since the preview, %s", sentWords)
				return
			default:
				if baseline == "" {
					b, err := e.opts.Templates.Baseline(ctx, d)
					if err == nil && !e.templateEvent(ctx, j, baselineEvent, b) {
						err = errors.New("the baseline could not be stored")
					}
					if err != nil {
						if ctx.Err() != nil {
							return
						}
						fail(before, nil, "no restart baseline (%v), nothing was sent", err)
						return
					}
					baseline = b
				}
				actx, cancel := context.WithTimeout(ctx, e.machine().CommandTimeout*policyApplyFactor)
				actx, trace = transport.WithTrace(actx)
				sendErr = e.opts.Templates.Send(actx, d, cmd)
				cancel()
				if ctx.Err() != nil {
					return
				}
				if errors.Is(sendErr, ErrTemplateRefused) {
					fail(before, trace, "the device refused the template, nothing was applied: %v", sendErr)
					return
				}
			}
		}
		if j, ok = e.policyMove(ctx, j, model.JobVerify, "verify", note); !ok {
			return
		}
	}
	vctx, cancel := context.WithTimeout(ctx, policyVerifyLimit)
	after, verr := e.opts.Templates.Verify(vctx, d, want, baseline)
	cancel()
	if ctx.Err() != nil {
		return
	}
	if verr == nil && (after.Module != 0 || after.Layout != want || after.Name != name) {
		verr = fmt.Errorf("the device reports module %d and a template that is not the planned one", after.Module)
	}
	if verr != nil {
		if sendErr != nil {
			verr = fmt.Errorf("sending: %w; verifying: %w", sendErr, verr)
		}
		e.finishTemplate(ctx, j, cmd, before, &after, trace, note, verr.Error())
		return
	}
	if sendErr != nil {
		note = "the reply to the Template command was lost (" + sendErr.Error() + "); the read-back shows the new template"
	}
	if baseline != "" && !after.RestartProven {
		note = strings.TrimSpace(note + " The restart could not be proven (the stored baseline was unusable), so only the template was checked.")
	}
	e.finishTemplate(ctx, j, cmd, before, &after, trace, note, "")
}

// ErrTemplateRefused is what TemplateApplier.Send returns (wrapped) when the device answered that it did
// not take the template: nothing was applied, no restart follows.
var ErrTemplateRefused = errors.New("template refused by the device")

// templateBaseline is the baseline token stored with the job before its send, or "".
func (e *Engine) templateBaseline(ctx context.Context, j model.Job) string {
	evs, err := e.jobs.JobEvents(ctx, j.ID)
	if err != nil {
		e.log.Warn("template job: could not read the events of a resumed job", "job", j.ID, "err", err)
		return ""
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Event == baselineEvent {
			return evs[i].Detail
		}
	}
	return ""
}

// templateEvent persists an event of the job in its current state (write-ahead, like a transition).
func (e *Engine) templateEvent(ctx context.Context, j model.Job, event, detail string) bool {
	now := e.clk.Now()
	next := j
	next.UpdatedAt = now
	if err := e.jobs.SaveTransition(ctx, next, model.JobEvent{JobID: j.ID, At: now, From: j.State, To: j.State, Event: event, Detail: detail}); err != nil {
		e.log.Error("template job: event not persisted", "job", j.ID, "event", event, "err", err)
		return false
	}
	return true
}

// cancelTemplate ends a queued job as cancelled, with the audit record a finished job gets.
func (e *Engine) cancelTemplate(ctx context.Context, j model.Job, reason string) {
	if _, ok := e.policyMove(ctx, j, model.JobCancelled, "cancelled", reason); !ok {
		return
	}
	if e.audit != nil {
		b, _ := json.Marshal(map[string]any{"transaction": j.BatchID, "job": j.ID}) // a plain map always encodes
		e.audit.Record(ctx, model.AuditEvent{Actor: j.CreatedBy, Action: "policy.template", Target: "device:" + strconv.FormatInt(int64(j.DeviceID), 10), Detail: string(b), Result: "cancelled: " + reason})
	}
}

// failedBefore finds a failed job of the same transaction that came before j. A transaction that cannot be
// listed is an error: the caller does not guess.
func (e *Engine) failedBefore(ctx context.Context, j model.Job) (model.Job, bool, error) {
	all, err := e.jobs.ListJobs(ctx, store.JobFilter{BatchID: j.BatchID})
	if err != nil {
		return model.Job{}, false, err
	}
	for _, o := range all {
		if o.ID < j.ID && o.State == model.JobFailed {
			return o, true, nil
		}
	}
	return model.Job{}, false, nil
}

// finishTemplate ends the job as succeeded (failure == "") or failed and audits the change.
func (e *Engine) finishTemplate(ctx context.Context, j model.Job, cmd string, before, after *TemplateReading, trace *transport.Trace, note, failure string) {
	to, event, detail := model.JobSucceeded, "done", note
	if failure != "" {
		to, event, detail = model.JobFailed, "failed", failure
	}
	if _, ok := e.policyMove(ctx, j, to, event, detail); !ok {
		return
	}
	if e.audit == nil {
		return
	}
	fields := map[string]any{"transaction": j.BatchID, "job": j.ID, "command": cmd}
	if before != nil {
		fields["before"] = rawOrText(before.JSON)
	}
	if after != nil {
		fields["after"] = rawOrText(after.JSON)
	}
	if trace != nil {
		fields["via"] = trace.Via()
	}
	if note != "" {
		fields["note"] = note
	}
	b, _ := json.Marshal(fields) // a plain map of JSON values always encodes
	result := "ok"
	if failure != "" {
		result = failure
	}
	e.audit.Record(ctx, model.AuditEvent{Actor: j.CreatedBy, Action: "policy.template", Target: "device:" + strconv.FormatInt(int64(j.DeviceID), 10), Detail: string(b), Result: result})
}

// rawOrText embeds s as JSON when it is, else as a string.
func rawOrText(s string) any {
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	return s
}
