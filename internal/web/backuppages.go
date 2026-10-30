package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The Backups tab of a device (design v2 screen "device", tab "backups", WP P10): one row per
// backup run, newest first, with the failed attempts among them; the retention line, Prune
// now, Back up now; select two rows to compare them, or several to delete them.

// backupsView is the Backups tab.
type backupsView struct {
	Base, CSRF string
	DeviceID   model.DeviceID
	Head       templ.Component // the device header and tabs (P3 owns it; the legacy partial until then)

	Retention    string // "Keeping the last 5 backups and anything kept · 3 will be pruned at the next run"
	CanRetention bool   // admins: "Change retention" opens Settings, Backups
	Total        string // "9 backups · 36.3 KiB"

	CanBackup bool // Back up now and Retry (operators and admins)
	// RestoreBlocked is why Restore… is disabled for every row (no path to the device), "" when it may be used.
	RestoreBlocked string
	Blocked        string // why Back up now cannot be used right now
	CanKeep        bool   // the Keep toggle (N1)
	CanDelete      bool   // Delete per row, Delete selected, Prune now (admins)

	Rows      []backupRow
	Prune     []backupRow // the runs the next pruning removes: the Prune now dialog
	PruneText string      // why they go
	Jobs      []restoreRow
}

// backupRow is one run (dump, snapshot and decoded copy taken together) or one failed attempt.
type backupRow struct {
	ID      model.BackupID // the run's dump (or its snapshot when it has no dump): selection, keep, delete
	Taken   time.Time
	Detail  string // "Manual · Tasmota 14.3.0 · 4.1 KiB"
	Size    string
	Newest  bool
	Kept    bool
	Pruned  bool // "Pruned at next run"
	Missing bool // the file was deleted by hand: no Download or Restore, the row can be deleted

	Failed bool   // a failed attempt: nothing was stored
	Reason string // why it failed (never credentials)

	Download, Restore string // links; empty when the viewer may not or the run has no dump
	DeleteText        string // the delete dialog's text; empty: no Delete
}

// restoreRow is one recent restore of the device.
type restoreRow struct {
	Href, Actor, State, Detail string
	Started                    time.Time
	Tone                       ui.Tone
}

func bkStr(id model.BackupID) string  { return strconv.FormatInt(int64(id), 10) }
func devStr(id model.DeviceID) string { return strconv.FormatInt(int64(id), 10) }

// pruneText says why the listed runs go at the next pruning.
func pruneText(ret backupRetentionView) string {
	why := "These are older than the last " + strconv.Itoa(ret.Keep) + " and not kept."
	if ret.MaxAge > 0 {
		why = "These are outside the retention (the last " + strconv.Itoa(ret.Keep) + " and everything from the last " + longDuration(ret.MaxAge) + ") and not kept."
	}
	return why + " Kept backups and the newest backup are never pruned."
}

// shortDate is the prototype's date: "22 Oct 2026, 09:41".
func shortDate(t time.Time) string { return t.Local().Format("2 Jan 2006, 15:04") }

// backupRun is one backup run as the tab lists it: the dump (or the snapshot of a run without
// a dump) and the missing parts of the run's snapshot, if it is incomplete.
type backupRun struct {
	model.Backup
	Gaps string
}

// backupRuns groups a device's backup files into runs, newest first: one row per run, keyed
// by the dump (or the snapshot of a run without a dump). Decoded copies only feed the
// comparison, so they are not rows.
func backupRuns(list []model.Backup) []backupRun {
	var runs []backupRun
	seen := map[time.Time]int{}
	for _, b := range list {
		if b.Kind == model.BackupDecoded {
			continue
		}
		i, ok := seen[b.TakenAt]
		if !ok {
			i = len(runs)
			seen[b.TakenAt] = i
			runs = append(runs, backupRun{Backup: b})
		} else if b.Kind == model.BackupConfigDump {
			runs[i].Backup = b
		}
		if b.Kind == model.BackupSnapshot {
			runs[i].Gaps = snapshotGaps(b.Note)
		}
	}
	return runs
}

// runDetail is a row's second line: trigger, label, firmware, size.
func runDetail(r backupRun) string {
	var parts []string
	if t := backupTriggerText(r.Trigger, r.TriggerRef); t != "" {
		parts = append(parts, t)
	}
	if r.Note != "" && snapshotGaps(r.Note) == "" {
		parts = append(parts, r.Note)
	}
	if r.Gaps != "" {
		parts = append(parts, "incomplete snapshot, missing "+r.Gaps)
	}
	if r.Version != "" {
		parts = append(parts, "Tasmota "+r.Version)
	}
	parts = append(parts, humanBytes(r.Size))
	return strings.Join(parts, " · ")
}

// attemptDetail is the second line of a failed attempt: nothing was stored, so no size.
func attemptDetail(a model.BackupAttempt) string {
	var parts []string
	if t := backupTriggerText(a.Trigger, a.TriggerRef); t != "" {
		parts = append(parts, t)
	}
	v := "—"
	if a.Version != "" {
		v = a.Version
	}
	return strings.Join(append(parts, "Tasmota "+v), " · ")
}

func (h *handler) backupsPage(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	list, err := h.d.Backups.List(r.Context(), d.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v := h.view(r, d.Name+" backups", "devices", nil)
	tabs := h.tabsOf(r, v, d, "backups")
	_, canDelete := h.backupAdmin()
	_, canKeep := h.backupKeeper()
	p := backupsView{Base: v.Base, CSRF: v.CSRF, DeviceID: d.ID, Head: deviceHeader(tabs),
		CanRetention: v.IsAdmin && v.Features.SysConfig, CanBackup: true, Blocked: tabs.BackupWhy, RestoreBlocked: tabs.RestoreWhy,
		CanKeep: canKeep, CanDelete: canDelete && v.IsAdmin}
	ret, hasRet, err := h.backupRetention(r.Context(), d.ID)
	if err != nil {
		h.log.WarnContext(r.Context(), "backups: retention", "err", err) // only the labels go
	}
	if hasRet {
		p.Retention, p.PruneText = ret.Line(), pruneText(ret)
	}
	newest := backup.NewestRun(list)
	runs := backupRuns(list)
	var total int64
	for _, run := range runs {
		b := run.Backup
		total += b.Size
		row := backupRow{ID: b.ID, Taken: b.TakenAt, Detail: runDetail(run), Size: humanBytes(b.Size),
			Newest: b.TakenAt.Equal(newest), Kept: b.Kept, Pruned: ret.IsPruned(b)}
		row.Missing = h.fileMissing(b)
		if v.IsAdmin && !row.Missing {
			row.Download = fmt.Sprintf("%s/backups/%d", v.Base, b.ID)
		}
		if b.Kind == model.BackupConfigDump {
			if v.IsAdmin && h.d.Restore != nil && !row.Missing {
				row.Restore = fmt.Sprintf("%s/backups/%d/restore", v.Base, b.ID)
			}
			if p.CanDelete {
				row.DeleteText = backupData{}.DeleteText(b)
				if row.Newest {
					row.DeleteText += " This is the newest backup of this device: afterwards you can only restore it to an older state."
				}
			}
		}
		p.Rows = append(p.Rows, row)
		if row.Pruned {
			p.Prune = append(p.Prune, row)
		}
	}
	if len(runs) > 0 {
		p.Total = plural(len(runs), "backup") + " · " + humanBytes(total)
	}
	p.Rows = withAttempts(p.Rows, ret.Attempts)
	if v.IsAdmin && h.d.Restore != nil {
		jobs, err := h.d.Restore.List(r.Context(), d.ID, 10)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		for _, j := range jobs {
			p.Jobs = append(p.Jobs, restoreRow{Href: fmt.Sprintf("%s/restores/%d", v.Base, j.ID), Actor: h.pr.Text(j.Actor),
				State: label(j.State), Detail: sentence(j.Detail), Started: j.CreatedAt, Tone: restoreTone(j.State)})
		}
	}
	v.Flash = backupsNotice(r)
	h.headPage(w, r, http.StatusOK, "backups", v, d, backupsTab(p))
}

// withAttempts puts the failed attempts among the runs by time, newest first.
func withAttempts(rows []backupRow, attempts []model.BackupAttempt) []backupRow {
	for _, a := range attempts {
		row := backupRow{Taken: a.At, Failed: true, Reason: a.Reason, Detail: attemptDetail(a)}
		i := 0
		for i < len(rows) && !rows[i].Taken.Before(a.At) {
			i++
		}
		rows = append(rows[:i], append([]backupRow{row}, rows[i:]...)...)
	}
	return rows
}

// backupsNotice is the notice after an action that came back to the tab.
func backupsNotice(r *http.Request) string {
	q := r.URL.Query()
	switch q.Get("ok") {
	case "deleted", "pruned":
		n, err := strconv.Atoi(q.Get("n"))
		if err != nil || n == 1 {
			return "Backup deleted."
		}
		return plural(n, "backup") + " deleted."
	case "backed-up":
		return "Backup taken."
	case "kept":
		return "Backup kept: the retention never prunes it."
	case "released":
		return "Backup released: the retention may prune it again."
	}
	return ""
}

// restoreTone is the badge tone of a restore job state.
func restoreTone(state string) ui.Tone {
	switch state {
	case store.RestoreDone:
		return ui.ToneOK
	case store.RestoreFailed:
		return ui.ToneDanger
	}
	return ui.ToneAccent
}

// backupIDsOf reads the selected backups (id per row) of a query or form.
func backupIDsOf(vals []string) ([]model.BackupID, bool) {
	var ids []model.BackupID
	for _, s := range vals {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			return nil, false
		}
		ids = append(ids, model.BackupID(n))
	}
	return ids, true
}

// backupDeleteSelectedConfirm is the confirmation of "Delete selected" (and the page form of
// "Prune now" through backupPruneConfirm): it names the count and whether the newest backup
// goes; the form posts the ids to the existing delete route.
func (h *handler) backupDeleteSelectedConfirm(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	ids, ok := backupIDsOf(r.URL.Query()["id"])
	if !ok {
		h.notFound(w, r)
		return
	}
	h.confirmRunDeletion(w, r, d, ids, false)
}

// backupPruneConfirm is "Prune now" as a page (the dialog's twin without JavaScript): the runs
// the next pruning removes, deleted now through the same route as Delete selected.
func (h *handler) backupPruneConfirm(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	k, _ := h.backupKeeper()
	pruned, err := k.PrunedAtNextRun(r.Context(), d.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var ids []model.BackupID
	for _, b := range backupRuns(pruned) {
		ids = append(ids, b.ID)
	}
	if len(ids) == 0 {
		v := h.view(r, "Nothing to prune", "devices", messageData{
			Text: "Nothing to prune: every backup of " + d.Name + " is within the retention or kept.", LinkText: "Back to the backups",
			Link: fmt.Sprintf("/devices/%d/backups", d.ID), LinkPrimary: true})
		v.Crumbs = []ui.Crumb{{Label: "Devices", Path: "/devices"}, {Label: d.Name, Path: fmt.Sprintf("/devices/%d", d.ID)}, {Label: "Backups", Path: fmt.Sprintf("/devices/%d/backups", d.ID)}, {Label: "Prune"}}
		h.message(w, r, http.StatusOK, v)
		return
	}
	h.confirmRunDeletion(w, r, d, ids, true)
}

// confirmRunDeletion shows the confirmation page for deleting the runs of ids: "Delete
// selected", or "Prune now" (prune).
func (h *handler) confirmRunDeletion(w http.ResponseWriter, r *http.Request, d model.Device, ids []model.BackupID, prune bool) {
	back := fmt.Sprintf("/devices/%d/backups", d.ID)
	del, ok := h.backupBulkDeleter()
	if !ok {
		h.notFound(w, r)
		return
	}
	if len(ids) == 0 {
		h.message(w, r, http.StatusUnprocessableEntity, h.view(r, "Nothing selected", "devices", messageData{
			Text: "Nothing to delete: no backup was selected.", LinkText: "Back to the backups", Link: back}))
		return
	}
	plan, err := del.PlanDeleteRuns(r.Context(), d.ID, ids)
	switch {
	case err == nil:
	case errors.Is(err, backup.ErrForeignBackup), errors.Is(err, store.ErrNotFound):
		h.notFound(w, r)
		return
	default:
		h.fail(w, r, err)
		return
	}
	n := plan.Runs
	if n == 0 {
		n = len(ids)
	}
	c := actionConfirm{
		Title: "Delete " + plural(n, "backup") + "?", Heading: "Delete " + plural(n, "backup"),
		Text:   plural(n, "backup") + " of " + d.Name + " (dump, snapshot and decoded copy of each) will be deleted. This cannot be undone.",
		Crumbs: []confirmLink{{"/devices", "Devices"}, {fmt.Sprintf("/devices/%d", d.ID), d.Name}, {back, "Backups"}},
		Last:   "Delete selected", Action: fmt.Sprintf("/devices/%d/backups/delete", d.ID), Verb: "Delete " + plural(n, "backup"),
		CancelHref: back, CancelLabel: "Cancel",
	}
	if prune {
		c.Title, c.Heading, c.Last, c.Verb = "Prune "+plural(n, "backup")+" now?", "Prune "+plural(n, "backup"), "Prune now", "Prune "+plural(n, "backup")
		if k, ok := h.backupKeeper(); ok {
			c.Text = pruneText(backupRetentionView{Keep: k.Retention().Keep, MaxAge: k.Retention().MaxAge}) + " This cannot be undone."
		}
	}
	if plan.Newest {
		c.Warning = "This includes the newest backup of " + d.Name + ". Afterwards the device can only be restored to an older state."
	}
	for _, id := range ids {
		c.Fields = append(c.Fields, confirmField{Name: "id", Value: strconv.FormatInt(int64(id), 10)})
	}
	c.Fields = append(c.Fields, confirmField{Name: "confirm", Value: "1"})
	h.renderActionConfirm(w, r, "devices", c)
}
