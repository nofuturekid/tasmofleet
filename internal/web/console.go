package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/transport"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

const consoleHistoryRows = 20

type historyRow struct {
	At      time.Time
	Actor   string
	Command string
	Result  string
	Refused bool
}

type consoleData struct {
	Device model.Device
	// Input pre-fills the command box. Secret commands are never pre-filled.
	Input string
	// Shown is the redacted command text for messages.
	Shown string
	// Confirm is set on the confirmation step of a destructive or lock-out
	// command: Consequence names what it does, Token stands for the command,
	// which is kept on the server (its arguments may be a secret).
	Confirm     bool
	Consequence string
	Token       string
	Reply       string
	Error       string
	History     []historyRow
	Tabs        deviceTabs
}

// history reads the console history from the audit log: what was sent to the
// device and what was refused. The audit log only holds redacted text.
func (h *handler) consoleHistory(r *http.Request, id model.DeviceID) []historyRow {
	if h.d.Audit == nil {
		return nil
	}
	target := "device:" + strconv.FormatInt(int64(id), 10)
	evs, err := h.d.Audit.ListAudit(r.Context(), store.AuditFilter{Target: target, Limit: 200})
	if err != nil {
		h.log.WarnContext(r.Context(), "console history unavailable", "err", err)
		return nil
	}
	var rows []historyRow
	for _, e := range evs {
		if e.Target != target || (e.Action != "command.send" && e.Action != "command.rejected") {
			continue
		}
		var d struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(e.Detail), &d) != nil {
			continue
		}
		rows = append(rows, historyRow{At: e.At, Actor: e.Actor, Command: d.Command, Result: e.Result, Refused: e.Action == "command.rejected"})
		if len(rows) == consoleHistoryRows {
			break
		}
	}
	return rows
}

// quickCommands are the console's quick commands (prototype, D9: Status 5 instead of Status 0,
// which automated code never sends). Each is checked by the command policy like a typed one.
var quickCommands = []string{"Status 5", "Power TOGGLE", "Status 11", "TelePeriod 300", "Restart 1"}

// consoleSince reads "since" (Clear): lines older than it are not shown. Zero shows everything.
func consoleSince(s string) time.Time {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

// consoleLines turns the history (newest first) into the console's lines, oldest first, and
// appends the reply or the error of the command just sent.
func consoleLines(data consoleData, since, now time.Time) []ui.ConsoleLine {
	var out []ui.ConsoleLine
	stamp := func(t time.Time) (string, string) { return t.Local().Format("15:04:05"), t.UTC().Format(time.RFC3339) }
	for i := len(data.History) - 1; i >= 0; i-- {
		row := data.History[i]
		if !since.IsZero() && row.At.Before(since) {
			continue
		}
		tm, dt := stamp(row.At)
		out = append(out, ui.ConsoleLine{Time: tm, Datetime: dt, Text: "› " + row.Command, Kind: "cmd", Who: row.Actor})
		switch {
		case row.Refused:
			out = append(out, ui.ConsoleLine{Time: tm, Datetime: dt, Text: "Refused: " + humanReason(row.Result), Kind: "err"})
		case row.Result != "" && row.Result != "ok":
			out = append(out, ui.ConsoleLine{Time: tm, Datetime: dt, Text: humanReason(row.Result), Kind: "err"})
		}
	}
	tm, dt := stamp(now)
	switch {
	case data.Reply != "":
		out = append(out, ui.ConsoleLine{Time: tm, Datetime: dt, Text: data.Reply, Kind: "reply"})
	case data.Error != "":
		out = append(out, ui.ConsoleLine{Time: tm, Datetime: dt, Text: data.Error, Kind: "alert"})
	}
	return out
}

// renderConsole shows the console tab: the device header, a pending confirmation, the console.
func (h *handler) renderConsole(w http.ResponseWriter, r *http.Request, status int, d model.Device, data consoleData) {
	data.Device = d
	data.History = h.consoleHistory(r, d.ID)
	v := h.view(r, d.Name+" console", "devices", nil)
	data.Tabs = h.tabsOf(r, v, d, "console")
	since := r.FormValue("since")
	now := h.d.Clock.Now()
	c := ui.ConsoleData{
		Title:  "Console · HTTP · " + data.Tabs.Address,
		Action: v.Base + devPath(d.ID, "/console"), CSRF: v.CSRF, Input: data.Input,
		ClearHref: v.Base + devPath(d.ID, "/console?since="+strconv.FormatInt(now.Unix(), 10)),
		Lines:     consoleLines(data, consoleSince(since), now),
		Quick:     quickCommands,
	}
	if !consoleSince(since).IsZero() {
		c.Hidden = []ui.KVRow{{Key: "since", Value: since}}
	}
	h.headPage(w, r, status, "console", v, d, consolePage(data, c, v.CSRF))
}

func (h *handler) consolePage(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	h.renderConsole(w, r, http.StatusOK, d, consoleData{})
}

// consoleError maps service errors to a status and a sentence. Device output
// and credentials never reach the sentence.
func consoleError(err error) (int, string) {
	switch {
	case errors.Is(err, cmdpolicy.ErrInvalid):
		return http.StatusUnprocessableEntity, "Malformed command. Send exactly one command: no semicolons, control characters or invisible characters."
	case errors.Is(err, cmdpolicy.ErrForbidden):
		return http.StatusForbidden, "This command is not sent from TasmoFleet."
	case errors.Is(err, cmdpolicy.ErrRole):
		return http.StatusForbidden, "This command needs the admin role."
	case errors.Is(err, transport.ErrCutOff):
		return http.StatusConflict, err.Error() // names the command, never its arguments
	}
	status, msg := deviceErrorText(err)
	if msg == "" {
		return http.StatusInternalServerError, "The command failed."
	}
	return status, msg
}

// confirmation adds to v the consequence of cutting off a device that is reached over MQTT only, and
// says whether it did: that is what a confirmation may consent to (transport.WithCutOffConsent).
func (h *handler) confirmation(ctx context.Context, cmd string, v cmdpolicy.Verdict, ids ...model.DeviceID) (cmdpolicy.Verdict, bool) {
	if h.d.MQTTOnly == nil || !cmdpolicy.CutsMQTTOff(cmd) || !h.d.MQTTOnly(ctx, ids...) {
		return v, false
	}
	return cmdpolicy.WithMQTTOnly(cmd, v), true
}

func (h *handler) consoleSend(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	limitBody(w, r)
	cmd := r.PostFormValue("command")
	p, _ := auth.FromContext(r.Context())
	ctx := r.Context()
	data := consoleData{Shown: cmdpolicy.Redacted(cmd)}
	// Text goes back into the page (input value) only when the policy would
	// also log it: arguments of most commands may be secrets.
	if canon, err := cmdpolicy.Canonical(cmd); err == nil && canon == data.Shown {
		if _, err := cmdpolicy.Classify(cmd); err == nil {
			data.Input = cmd
		}
	}
	// A destructive or lock-out command asks once, naming the consequence.
	// Routine commands go straight through. The flow only decides whether the
	// extra screen is shown: the service still checks the role for every send.
	if token := r.PostFormValue("token"); token != "" {
		pc, ok := h.cmds.take(h.d.Clock.Now(), token, h.bindingOf(r))
		if !ok || len(pc.ids) != 1 || pc.ids[0] != d.ID {
			data.Error = "The confirmation expired. Send the command again."
			h.renderConsole(w, r, http.StatusConflict, d, data)
			return
		}
		cmd, data.Shown, data.Input = pc.cmd, cmdpolicy.Redacted(pc.cmd), ""
		if pc.cutOff {
			ctx = transport.WithCutOffConsent(ctx) // the person was told, and confirmed
		}
	} else if v0, err := cmdpolicy.Classify(cmd); err == nil && p.Role >= model.RoleAdmin {
		v, cut := h.confirmation(ctx, cmd, v0, d.ID)
		canon, err := cmdpolicy.Canonical(cmd)
		if err == nil && v.Confirm != "" {
			data.Confirm, data.Consequence = true, v.Confirm
			if cut {
				data.Token = h.cmds.putCutOff(h.d.Clock.Now(), h.bindingOf(r), canon, []model.DeviceID{d.ID})
			} else {
				data.Token = h.cmds.put(h.d.Clock.Now(), h.bindingOf(r), canon, []model.DeviceID{d.ID})
			}
			h.renderConsole(w, r, http.StatusOK, d, data)
			return
		}
	}
	raw, err := h.d.DeviceCmd.Run(ctx, p, d.ID, cmd)
	status := http.StatusOK
	if err != nil {
		status, data.Error = consoleError(err)
		if status == http.StatusInternalServerError {
			h.log.ErrorContext(r.Context(), "console command failed", "device", d.ID, "err", err)
		}
	} else {
		// Sent: the box is empty for the next command (it keeps the text only for a correction).
		data.Reply, data.Input = compactJSON(raw), ""
	}
	h.renderConsole(w, r, status, d, data)
}

// compactJSON is a device reply on one console line; anything unparsable is shown as is. The
// page escapes it either way.
func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

// prettyJSON indents a device reply; anything unparsable is shown as is.
// The template escapes it either way.
func prettyJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}
