package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The no-JavaScript pages of the dialog-only confirmations: delete a backup,
// delete a policy rule, cancel a transaction, cancel one update. Each is the
// GET twin of a POST route; opening the page changes nothing, the POST keeps
// its CSRF and role checks. The wording is the dialog's.

type confirmLink struct{ Href, Label string }

type confirmFact struct {
	Name, Value string
	Code        bool
}

type confirmField struct{ Name, Value string }

type actionConfirm struct {
	Title, Heading, Text, Warning string
	Crumbs                        []confirmLink // before the current page
	Last                          string        // the current page in the trail
	Facts                         []confirmFact
	Action                        string // POST path, without the base
	Fields                        []confirmField
	Ack                           string // a required checkbox with this label
	Verb                          string
	CancelHref, CancelLabel       string
}

// renderActionConfirm shows the confirmation page (ui.ConfirmPage): the dialog's look, the
// trail before it and the page itself as the last crumb. Every action here removes or stops
// something, so the verb is in the danger tone.
func (h *handler) renderActionConfirm(w http.ResponseWriter, r *http.Request, nav string, c actionConfirm) {
	v := h.view(r, c.Title, nav, nil)
	for _, l := range c.Crumbs {
		v.Crumbs = append(v.Crumbs, ui.Crumb{Label: l.Label, Path: l.Href})
	}
	v.Crumbs = append(v.Crumbs, ui.Crumb{Label: c.Last})
	d := ui.ConfirmData{
		Title: c.Title, Heading: c.Heading, Text: c.Text, Warning: c.Warning,
		Action: v.Base + c.Action, CSRF: v.CSRF, Verb: c.Verb, Danger: true, Ack: c.Ack,
		CancelHref: v.Base + c.CancelHref, CancelLabel: c.CancelLabel,
	}
	for _, f := range c.Facts {
		d.Facts = append(d.Facts, ui.KVRow{Key: f.Name, Value: f.Value, Mono: f.Code})
	}
	for _, f := range c.Fields {
		d.Fields = append(d.Fields, ui.KVRow{Key: f.Name, Value: f.Value})
	}
	h.show(w, r, http.StatusOK, "action_confirm", v, ui.ConfirmPage(d))
}

func (h *handler) backupDeleteConfirm(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || n <= 0 {
		h.notFound(w, r)
		return
	}
	_, b, err := h.d.Backups.Open(r.Context(), model.BackupID(n))
	if errors.Is(err, store.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	// A backup whose file is gone is exactly what the admin must be able to remove.
	if err != nil && !errors.Is(err, backup.ErrFileMissing) {
		h.fail(w, r, err)
		return
	}
	// Only a dump has the delete control (it takes the whole run with it).
	if b.Kind != "dmp" {
		h.notFound(w, r)
		return
	}
	back := strings.TrimRight(backupsHome(b), "?&") // a deleted device's backup: Settings -> Backups
	crumbs := []confirmLink{{"/devices", "Devices"}, {fmt.Sprintf("/devices/%d", b.DeviceID), "Device"}, {back, "Backups"}}
	if b.DeviceID == 0 {
		crumbs = []confirmLink{{"/settings/system", "Settings"}, {back, "Backups"}}
	}
	c := actionConfirm{
		Title: "Delete this backup?", Heading: "Delete backup",
		Text: backupData{}.DeleteText(b),
		Facts: []confirmFact{
			{Name: "Taken", Value: ui.StampText(b.TakenAt, ui.StampDateTimeSec)},
			{Name: "Firmware", Value: b.Version},
		},
		Crumbs: crumbs,
		Last:   "Delete",
		Action: fmt.Sprintf("/backups/%d/delete", b.ID), Verb: "Delete backup",
		CancelHref: back, CancelLabel: "Cancel",
	}
	if b.DeviceID != 0 {
		if list, err := h.d.Backups.List(r.Context(), b.DeviceID); err == nil && backup.IsNewestRun(list, b) {
			c.Warning = "This is the newest backup of this device. Afterwards you can only restore the device to an older state."
		}
	}
	nav := "devices"
	if b.DeviceID == 0 {
		nav = "system"
	}
	h.renderActionConfirm(w, r, nav, c)
}

func (h *handler) rulesDeleteConfirm(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if !policy.RuleIDRe.MatchString(id) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	st, err := h.d.PolicyRules.State(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	n := indexOfRule(st.Specs, id)
	if n < 0 {
		h.notFound(w, r)
		return
	}
	s := st.Specs[n]
	label, _ := settingLabel(s.Command)
	name := s.Name
	if name == "" {
		name = label
	}
	h.renderActionConfirm(w, r, "policy", actionConfirm{
		Title: "Delete this rule?", Heading: "Delete rule",
		Text: "The rule is removed from the policy. The change is saved as a new version you can revert to.",
		Facts: []confirmFact{
			{Name: "Rule", Value: name},
			{Name: "Expected", Value: wantText(s.Command, s.Want), Code: true},
		},
		Crumbs: []confirmLink{{"/devices", "Devices"}, {policyPath, "Policy"}},
		Last:   "Delete",
		Action: rulesPath + "/delete",
		Fields: []confirmField{{"id", id}, {"version", strconv.FormatInt(st.VersionID, 10)}},
		Verb:   "Delete rule", CancelHref: rulesPath + "/" + id, CancelLabel: "Cancel",
	})
}

func (h *handler) batchCancelConfirm(w http.ResponseWriter, r *http.Request) {
	p, ok := h.batchData(w, r)
	if !ok {
		return
	}
	if len(p.Jobs) > 0 && !h.needAdminFor(w, r, p.Jobs[0].Job) {
		return
	}
	back := "/transactions/" + strconv.FormatInt(int64(p.Batch), 10)
	if !p.Active {
		http.Error(w, "transaction is not running", http.StatusConflict)
		return
	}
	// the same words as the page's dialog (txnBody)
	h.renderActionConfirm(w, r, "transactions", actionConfirm{
		Title:   "Cancel " + txID(p.Batch) + "?",
		Heading: "Cancel remaining",
		Text:    cancelText(p.Queued),
		Crumbs:  []confirmLink{{"/transactions", "Transactions"}, {back, txID(p.Batch)}},
		Last:    "Cancel",
		Action:  back + "/cancel", Verb: "Cancel remaining",
		CancelHref: back, CancelLabel: "Keep running",
	})
}

func (h *handler) jobCancelConfirm(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || n <= 0 {
		h.notFound(w, r)
		return
	}
	j, err := h.d.Jobs.GetJob(r.Context(), model.JobID(n))
	if errors.Is(err, store.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if !h.needAdminFor(w, r, j) {
		return
	}
	if j.State.Terminal() {
		http.Error(w, "job is not active", http.StatusConflict)
		return
	}
	name := fmt.Sprintf("device %d", j.DeviceID)
	if d, err := h.d.Inventory.Get(r.Context(), j.DeviceID); err == nil {
		name = d.Name
	}
	back := "/transactions/" + strconv.FormatInt(int64(j.BatchID), 10)
	h.renderActionConfirm(w, r, "transactions", actionConfirm{
		Title:   "Cancel the update of " + name + "?",
		Heading: "Cancel update",
		Text:    "A running flash cannot be stopped; a device that has not started keeps its previous state.",
		Crumbs:  []confirmLink{{"/transactions", "Transactions"}, {back, txID(j.BatchID)}},
		Last:    "Cancel update",
		Action:  fmt.Sprintf("/jobs/%d/cancel", j.ID), Verb: "Cancel update",
		CancelHref: back, CancelLabel: "Keep running",
	})
}
