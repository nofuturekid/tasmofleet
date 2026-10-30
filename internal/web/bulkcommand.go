package web

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// Bulk "Send command": an admin sends one command line (a Backlog included) to
// the selected devices. The same command policy and the same confirmation texts
// as the console apply; the preview is the confirmation. The send runs as a
// transaction (per-device result, "Retry failed", audit), see
// ota.Engine.SubmitCommand.

func (h *handler) registerBulkCommand(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), adm func(http.Handler) http.Handler) {
	if h.d.CommandBatch == nil || h.d.DeviceCmd == nil {
		return
	}
	route("POST /devices/bulk/command", adm, h.bulkCommandPage)
	route("POST /devices/bulk/command/run", adm, h.bulkCommandRun)
}

// bulkCommandPage shows the scope page with the command box (the bar's post, a "×" on a row, a
// refused command), or, for a command the policy allows, its preview: the command, who gets it
// and the consequence of a destructive or lock-out command (spec 14.1: a preview step whose
// token binds the command and the devices). Nothing is sent here.
func (h *handler) bulkCommandPage(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return
	}
	sv, ready, err := h.bulkScope(r, bulkCmd, "/devices/bulk/command", "/devices/bulk/command")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// "Wait up to 5 s for each response" starts checked, as in the prototype.
	sv.Wait, sv.Serial = firstVisit(r) || r.PostFormValue("wait") == "1", r.PostFormValue("serial") == "1"
	cmd := r.PostFormValue("command")
	if cmd == "" || dropping(r) || sv.Error != "" {
		sv.Command = safeInput(cmd)
		h.showBulkScope(w, r, sv)
		return
	}
	shown := cmdpolicy.Redacted(cmd)
	p, _ := auth.FromContext(r.Context())
	v, err := cmdpolicy.Authorize(cmd, p.Role)
	switch {
	case err != nil:
		_, sv.CommandError = consoleError(err)
	case v.Redact:
		sv.CommandError = "Credential commands are not sent to several devices at once. Use the console of each device."
	}
	canon, cerr := cmdpolicy.Canonical(cmd)
	if sv.CommandError == "" && cerr != nil {
		_, sv.CommandError = consoleError(cerr)
	}
	if sv.CommandError != "" {
		sv.Command = safeInput(cmd)
		h.showBulkScope(w, r, sv)
		return
	}
	if _, cut := h.confirmation(r.Context(), cmd, v, ready...); cut {
		// The consent of a confirmation is for one device and one person's eyes on it; a transaction of
		// jobs carries none, so the router would refuse the send anyway. Say so before anything is queued.
		sv.CommandError = "This command can cut a device off that TasmoFleet reaches over MQTT only. Use the console of each device, which asks to confirm."
		sv.Command = safeInput(cmd)
		h.showBulkScope(w, r, sv)
		return
	}
	consequence, restart := v.Confirm, false
	if consequence == "" && strings.EqualFold(v.Name, "restart") {
		// Restart 1 is harmless enough for operators, but the devices still drop offline: say so, like the Restart button of the bulk bar does
		restart = true
		consequence = "Each device is offline for a few seconds while it reboots, and its health shows offline until it reports again."
	}
	verb := "Send to " + plural(len(ready), "device")
	if restart {
		verb = "Restart " + plural(len(ready), "device")
	}
	fields := []ui.KVRow{{Key: "token", Value: h.cmds.put(h.d.Clock.Now(), h.bindingOf(r), canon, ready)}}
	for _, o := range []string{"wait", "serial"} {
		if r.PostFormValue(o) == "1" {
			fields = append(fields, ui.KVRow{Key: o, Value: "1"})
		}
	}
	for _, id := range h.formIDs(r) {
		fields = append(fields, ui.KVRow{Key: "id", Value: strconv.FormatInt(int64(id), 10)}) // the result lists the skipped ones
	}
	view := h.view(r, bulkTitle, "devices", nil)
	view.Crumbs = bulkCrumbs
	h.show(w, r, http.StatusOK, "bulk_command", view, ui.ConfirmPage(ui.ConfirmData{
		Title: sv.Title(), Heading: "Send " + shown + " to " + plural(len(ready), "device") + "?",
		Text:    "Devices that are not online are left out. The result page lists the answer of every device.",
		Warning: consequence,
		Facts:   []ui.KVRow{{Key: "Command", Value: shown, Mono: true}, {Key: "Devices", Value: sv.Summary()}},
		Action:  view.Base + "/devices/bulk/command/run", CSRF: view.CSRF, Fields: fields,
		Verb: verb, Danger: consequence != "", CancelHref: view.Base + "/devices", CancelLabel: "Cancel",
	}))
}

// safeInput is the command box's text after a redraw: only a command without a secret comes
// back into the page (cmdpolicy.Redacted leaves it unchanged).
func safeInput(cmd string) string {
	if cmdpolicy.Redacted(cmd) != cmd {
		return ""
	}
	return cmd
}

// bulkCommandRun starts the transaction for what the preview showed: the
// command and the devices come from the server-held token, not from the form.
func (h *handler) bulkCommandRun(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return
	}
	opts, valid := bulkOptionsOf(r)
	if !valid { // checked before the token is used up: the preview stays valid
		h.message(w, r, http.StatusUnprocessableEntity, h.view(r, "Command not sent", "devices", "Unknown number of devices at a time. Nothing was sent."))
		return
	}
	pc, ok := h.cmds.take(h.d.Clock.Now(), r.PostFormValue("token"), h.bindingOf(r))
	if !ok || len(pc.ids) == 0 {
		h.message(w, r, http.StatusConflict, h.view(r, "Command not sent", "devices", "The preview expired. Nothing was sent. Start again from the device list."))
		return
	}
	p, _ := auth.FromContext(r.Context())
	if _, err := cmdpolicy.Authorize(pc.cmd, p.Role); err != nil { // the role may have changed since the preview
		status, msg := consoleError(err)
		h.message(w, r, status, h.view(r, "Command not sent", "devices", msg))
		return
	}
	reqs := make([]ota.PolicyRequest, 0, len(pc.ids))
	for _, id := range pc.ids {
		reqs = append(reqs, ota.PolicyRequest{DeviceID: id, Commands: []string{pc.cmd}})
	}
	ctx := ota.WithBatchOptions(ota.WithActorKind(r.Context(), p.Method), opts)
	batch, skipped, err := h.d.CommandBatch.Submit(ctx, actor(r), reqs)
	switch {
	case errors.Is(err, ota.ErrNothingToFix):
		h.message(w, r, http.StatusConflict, h.view(r, "Nothing sent", "devices", "Nothing was sent. "+h.refusalText(r, skipped)))
	case err != nil:
		h.fail(w, r, err)
	case len(skipped) == 0:
		redirect(w, r, resultURL(batch, h.formIDs(r)))
	default:
		h.message(w, r, http.StatusOK, h.view(r, "Command started", "devices",
			"Transaction #"+strconv.FormatInt(int64(batch), 10)+" was started. Left out: "+h.refusalText(r, skipped)))
	}
}

// bulkReplyWait is the "Wait up to 5 s for each response" option.
const bulkReplyWait = 5 * time.Second

// bulkParallels are the choices of "Devices at a time" on the restart page.
var bulkParallels = []int{1, 3, 5}

// bulkOptionsOf reads the bulk options of the run form (design v2, N15):
// wait=1 ("Wait up to 5 s for each response"), serial=1 ("One device at a
// time") and parallel=1|3|5 ("Devices at a time", restart). Absent fields keep
// the defaults. ok is false for a parallel value that is not offered.
func bulkOptionsOf(r *http.Request) (o ota.BatchOptions, ok bool) {
	if r.PostFormValue("wait") == "1" {
		o.ReplyTimeout = bulkReplyWait
	}
	if r.PostFormValue("serial") == "1" {
		o.Parallel = 1
	}
	if v := r.PostFormValue("parallel"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || !slices.Contains(bulkParallels, n) {
			return ota.BatchOptions{}, false
		}
		if o.Parallel == 0 {
			o.Parallel = n // "One device at a time" wins when both are sent
		}
	}
	return o, true
}
