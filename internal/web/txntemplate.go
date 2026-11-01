package web

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The transaction page of "Apply GPIO template" (#327, mockup 3a and 3b): one card per device with the
// four steps the engine takes (backup, read again, send, verify). The engine records one state per job
// (backup, applying, verify), so a step that the state does not name is derived: "read again" is part of
// "applying", "send" is done once the job verifies.

type tmplStep struct {
	Text, Cmd, Label string
	Tone             ui.Tone
}

type tmplCard struct {
	Name, Href, Rank string
	Tone             ui.Tone
	Steps            []tmplStep
	Reason           string
	Stopped          bool // the reason says the device was never touched
}

// tmplColumns deals the cards into three column stacks (card order across, then down), the way the
// mockup lays them out; Rank keeps the phone order.
func tmplColumns(cards []tmplCard) [][]tmplCard {
	cols := make([][]tmplCard, 3)
	for i := range cards {
		cards[i].Rank = ui.StackRank(i + 1)
		cols[i%3] = append(cols[i%3], cards[i])
	}
	for len(cols) > 1 && len(cols[len(cols)-1]) == 0 {
		cols = cols[:len(cols)-1]
	}
	return cols
}

var stoppedAfter = regexp.MustCompile(`after job (\d+) failed`)

// earlyFailure says whether a failed job stopped before the command was sent: the engine's messages for
// "nothing was sent" (templatejob.go).
func earlyFailure(err string) bool {
	for _, p := range []string{"inventory:", "this server cannot apply", "could not read the template", "the device no longer runs", "the template changed since"} {
		if strings.Contains(err, p) {
			return true
		}
	}
	return false
}

// templateCardOf is the card of one device of a template transaction. names maps the jobs of the
// transaction to their device names, for "stopped after X failed".
func templateCardOf(j jobView, base string, names map[model.JobID]string) tmplCard {
	c := tmplCard{Name: j.Device, Href: base + "/devices/" + strconv.FormatInt(int64(j.Job.DeviceID), 10), Tone: ui.ToneNeutral}
	cmd, _, _ := strings.Cut(j.Job.Commands, "\n")
	short := "Template {…}"
	var body struct {
		Name string `json:"NAME"`
	}
	if raw, ok := strings.CutPrefix(cmd, "Template "); ok && json.Unmarshal([]byte(raw), &body) == nil && body.Name != "" {
		short = "Template " + `{"NAME":` + strconv.Quote(body.Name) + ",…}"
	}
	type st struct {
		label string
		tone  ui.Tone
	}
	queued, done := st{"Queued", ui.ToneNeutral}, st{"Done", ui.ToneOK}
	notStarted := st{"Not started", ui.ToneNeutral}
	failed := st{"Failed", ui.ToneDanger}
	steps := [4]st{queued, queued, queued, queued}
	readNote, verifyNote := "", ""
	sent := false
	switch j.Job.State { //nolint:exhaustive // a template job has these states only
	case model.JobBackup:
		steps[0] = st{"Backing up", ui.ToneAccent}
	case model.JobApplying:
		steps[0], steps[1] = done, st{"Reading", ui.ToneAccent}
	case model.JobVerify:
		steps[0], steps[1], steps[2], steps[3] = done, done, st{"Sent", ui.ToneOK}, st{"Verifying", ui.ToneAccent}
		readNote, sent = " · unchanged since the preview", true
	case model.JobSucceeded:
		steps[0], steps[1], steps[2], steps[3] = done, done, st{"Sent", ui.ToneOK}, st{"Applied", ui.ToneOK}
		readNote, verifyNote, sent = " · unchanged since the preview", " · template matches", true
	case model.JobFailed:
		switch err := j.Job.LastError; {
		case strings.HasPrefix(err, "backup failed") || strings.Contains(err, "cannot take backups") || strings.Contains(err, "only an active admin"):
			steps = [4]st{failed, notStarted, notStarted, notStarted}
		case earlyFailure(err):
			steps = [4]st{done, failed, notStarted, notStarted}
		default:
			steps = [4]st{done, done, st{"Sent", ui.ToneOK}, failed}
			readNote, sent = " · unchanged since the preview", true
		}
	case model.JobCancelled:
		steps = [4]st{notStarted, notStarted, notStarted, notStarted}
	}
	c.Tone = ui.ToneNeutral
	switch j.Job.State { //nolint:exhaustive // the dot follows the outcome only
	case model.JobSucceeded:
		c.Tone = ui.ToneOK
	case model.JobFailed:
		c.Tone = ui.ToneDanger
	case model.JobQueued, model.JobCancelled:
	default:
		c.Tone = ui.ToneAccent
	}
	sendText := "Template {…}"
	sendCmd := ""
	if sent {
		sendText, sendCmd = short, cmd
	}
	texts := []string{"Backup", "Read again" + readNote, sendText, "Verify" + verifyNote}
	for i, t := range texts {
		s := tmplStep{Text: t, Label: steps[i].label, Tone: steps[i].tone}
		if i == 2 {
			s.Cmd = sendCmd
		}
		c.Steps = append(c.Steps, s)
	}
	switch j.Job.State { //nolint:exhaustive // only the two endings carry a reason
	case model.JobFailed:
		c.Reason = humanReason(j.Job.LastError)
		if steps[0].label == "Done" {
			c.Reason += " Its backup from this run can be restored from the device's Backups tab."
		}
	case model.JobCancelled:
		c.Stopped = true
		c.Reason = "Stopped. Nothing was sent to this device."
		if m := stoppedAfter.FindStringSubmatch(j.Job.LastError); m != nil {
			if n, err := strconv.ParseInt(m[1], 10, 64); err == nil && names[model.JobID(n)] != "" {
				c.Reason = "Stopped after " + names[model.JobID(n)] + " failed. Nothing was sent to this device."
			}
		}
	}
	return c
}

// templateStartOf reads the reference of a template transaction from its start record in the audit
// log ("policy.template.start"); 0 when it cannot be found.
func (h *handler) templateRefOf(r *http.Request, batch model.BatchID) model.DeviceID {
	if h.d.Audit == nil {
		return 0
	}
	evs, err := h.d.Audit.ListAudit(r.Context(), store.AuditFilter{Target: "transaction:" + bid(batch), ActionPrefix: "policy.template.start", Limit: 1})
	if err != nil || len(evs) == 0 {
		return 0
	}
	var d struct {
		Reference int64 `json:"reference"`
	}
	if json.Unmarshal([]byte(evs[0].Detail), &d) != nil || d.Reference <= 0 {
		return 0
	}
	return model.DeviceID(d.Reference)
}
