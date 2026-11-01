package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// policyHistoryPath is the History tab.
const policyHistoryPath = "/policy/history"

// historyMaxPages bounds the walk from the newest version to a picked one (50 versions a page).
const historyMaxPages = 200

// histVer is one version inside a folded entry.
type histVer struct {
	ID       int64
	What     string // see histEntry.Note
	WhatFull string
	By       string
	At       time.Time
	Href     string
	Selected bool
}

// histEntry is one line of the version list: a version, or a fold of quick saves.
type histEntry struct {
	Label    string // "v18" or "v11–17"
	Summary  string
	Note     string // the version note, shown in quotes; long identifiers cut to 8 characters (they cannot wrap)
	NoteFull string // the whole note when Note is shortened (a tooltip)
	Folded   bool
	Current  bool
	Selected bool
	Href     string
	By       string
	At, To   time.Time // To: the oldest save of a fold (At is the newest)
	SameTime bool      // a fold within one minute: one time, not a range
	Versions []histVer
}

type histDay struct {
	Title   string
	Entries []histEntry
}

// histDetail is the right card: what the picked version changed, and the revert to the state before it.
type histDetail struct {
	Title   string // "v17 → v18"
	Heading string // "Changes in v18"
	Changes []sysconfig.RuleChange
	// Reordered: the rules only changed their order (the list says "Reordered rules" too).
	Reordered bool
	// Revert is the version before the picked one; 0: none (the first version), no button.
	Revert     int64
	RevertAt   time.Time
	RevertDiff []sysconfig.RuleChange
}

type policyHistoryView struct {
	Head   policyHead
	Base   string
	CSRF   string
	Admin  bool
	Days   []histDay
	Detail *histDetail
	Older  string // href of the older page, "" when none
	Newest string // href back to the newest page, "" on the first page
}

// allVersions flattens the whole history, newest first (the rule page needs all of it: a version
// is diffed against the one before it).
func (h *handler) allVersions(ctx context.Context) ([]store.PolicyRuleVersion, error) {
	var out []store.PolicyRuleVersion
	var before int64
	for i := 0; i < historyMaxPages; i++ {
		pg, err := h.d.PolicyRules.History(ctx, before, sysconfig.DefaultHistoryLimit)
		if err != nil {
			return nil, err
		}
		for _, e := range pg.Entries {
			out = append(out, e.Versions...)
		}
		if pg.NextBefore == 0 {
			break
		}
		before = pg.NextBefore
	}
	return out, nil
}

// policyHistory is GET /policy/history. ?v=ID picks the version whose changes the right card
// shows (an unknown one is a 404, the page that holds it is shown), ?before=ID pages.
func (h *handler) policyHistory(w http.ResponseWriter, r *http.Request) {
	h.historyRender(w, r, false)
}

// historyRender writes the tab; live runs the fleet check first (Run now on this tab).
func (h *handler) historyRender(w http.ResponseWriter, r *http.Request, live bool) {
	ctx := r.Context()
	q := r.URL.Query()
	var pick, before int64
	if s := q.Get("v"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			h.notFound(w, r)
			return
		}
		pick = n
	}
	if s := q.Get("before"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			h.notFound(w, r)
			return
		}
		before = n
	}

	pg, err := h.d.PolicyRules.History(ctx, before, sysconfig.DefaultHistoryLimit)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if pick != 0 && !pageHolds(pg, pick) {
		// The link came without its page cursor (the rule page's history): walk down to it. A version
		// that does not exist is refused first, so a bad link never walks the whole history.
		if _, err := h.d.PolicyRules.VersionDiff(ctx, 0, pick); errors.Is(err, store.ErrNotFound) {
			h.notFound(w, r)
			return
		}
		found := false
		pg, before, found, err = h.pageOf(ctx, pick)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		if !found {
			h.notFound(w, r)
			return
		}
	}

	v := h.view(r, "Policy history", "policy", nil)
	v.Crumbs = []ui.Crumb{{Label: "Devices", Path: "/devices"}, {Label: "Policy", Path: policyPath}, {Label: "History"}}
	switch q.Get("ok") {
	case "reverted":
		v.Flash = policyFlashes["reverted"]
	case "nochange":
		v.Flash = "Nothing changed: the rules already match that version"
	}
	st, err := h.d.PolicyRules.State(ctx)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	rep, repErr := h.d.Policy.Report(ctx, actor(r), live)
	_, valid, bad := h.d.PolicyRules.ValidRules(st.Specs)
	p := policyHistoryView{Base: v.Base, CSRF: v.CSRF, Admin: v.IsAdmin}
	p.Head = h.policyHead(v, valid, len(bad), rep, repErr)
	p.Head.Show = true
	p.Head.Back = "history"
	if live && p.Head.Line != nil && p.Head.Line.State != "failed" {
		v.Flash = "Live check finished · " + deviateText(p.Head.Fleet.Deviating)
	}
	p.Head.Tabs = h.policyTabs(v.Base, "history", len(st.Specs), p.Head.Fleet.Deviating, 0)

	cursor := ""
	if before > 0 {
		cursor = "before=" + vstr(before) + "&"
		p.Newest = v.Base + policyHistoryPath
	}
	if pg.NextBefore != 0 {
		p.Older = v.Base + policyHistoryPath + "?before=" + vstr(pg.NextBefore)
	}
	if len(pg.Entries) > 0 {
		if pick == 0 {
			pick = pg.Entries[0].Versions[0].ID
		}
		p.Days = h.histDays(pg, pick, st.VersionID, cursor, v.Base)
		if p.Detail, err = h.histDetail(ctx, pg, pick, st.VersionID); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	h.show(w, r, http.StatusOK, "policyhistory", v, policyHistoryPage(p))
}

func pageHolds(pg sysconfig.HistoryPage, id int64) bool {
	for _, e := range pg.Entries {
		for _, ver := range e.Versions {
			if ver.ID == id {
				return true
			}
		}
	}
	return false
}

// pageOf walks the pages from the newest down to the one that holds version id.
func (h *handler) pageOf(ctx context.Context, id int64) (sysconfig.HistoryPage, int64, bool, error) {
	var before int64
	for i := 0; i < historyMaxPages; i++ {
		pg, err := h.d.PolicyRules.History(ctx, before, sysconfig.DefaultHistoryLimit)
		if err != nil {
			return pg, 0, false, err
		}
		if pageHolds(pg, id) {
			return pg, before, true, nil
		}
		if pg.NextBefore == 0 {
			break
		}
		before = pg.NextBefore
	}
	return sysconfig.HistoryPage{}, 0, false, nil
}

// shorten cuts long identifiers to 8 characters; full is the whole text when that changed anything.
func shorten(s string) (short, full string) {
	short = longToken.ReplaceAllStringFunc(s, func(t string) string { return t[:8] })
	if short != s {
		full = s
	}
	return short, full
}

// versionWhat is what a version says about itself when its entry has no better words.
func versionWhat(v store.PolicyRuleVersion) string {
	switch {
	case v.Note != "":
		return v.Note
	case v.RevertOf > 0:
		return "Reverted to v" + vstr(v.RevertOf)
	}
	return "Rules saved"
}

func dayTitle(day, now time.Time) string {
	layout := "2 Jan"
	if day.Year() != now.Year() {
		layout = "2 Jan 2006"
	}
	if day.Equal(time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())) {
		return "Today · " + day.Format(layout)
	}
	return day.Format(layout)
}

func (h *handler) histDays(pg sysconfig.HistoryPage, pick, current int64, cursor, base string) []histDay {
	now := h.d.Clock.Now().In(time.Local)
	link := func(id int64) string {
		return base + policyHistoryPath + "?" + cursor + "v=" + vstr(id) + "#diff"
	}
	var days []histDay
	for _, e := range pg.Entries {
		first, last := e.Versions[0], e.Versions[len(e.Versions)-1]
		he := histEntry{Label: "v" + vstr(first.ID), Summary: e.Summary, Folded: e.Folded,
			Current: first.ID == current, Href: link(first.ID), By: h.pr.Text(first.Actor), At: first.At}
		if e.Folded {
			he.Label = "v" + vstr(last.ID) + "–" + vstr(first.ID)
			he.Summary = strconv.Itoa(len(e.Versions)) + " quick saves · " + e.Summary
			he.To = last.At
			he.SameTime = first.At.Format("15:04") == last.At.Format("15:04")
		} else {
			he.Note, he.NoteFull = shorten(first.Note)
		}
		if he.Summary == "" {
			he.Summary = versionWhat(first)
		}
		for _, ver := range e.Versions {
			sel := ver.ID == pick
			he.Selected = he.Selected || sel
			if e.Folded {
				what, full := shorten(versionWhat(ver))
				he.Versions = append(he.Versions, histVer{ID: ver.ID, What: what, WhatFull: full, By: h.pr.Text(ver.Actor), At: ver.At, Href: link(ver.ID), Selected: sel})
			}
		}
		if len(days) == 0 || days[len(days)-1].Title != dayTitle(e.Day, now) {
			days = append(days, histDay{Title: dayTitle(e.Day, now)})
		}
		d := &days[len(days)-1]
		d.Entries = append(d.Entries, he)
	}
	return days
}

// histDetail builds the right card for version pick: its own changes (not the fold's) and the
// revert to the version before it.
func (h *handler) histDetail(ctx context.Context, pg sysconfig.HistoryPage, pick, current int64) (*histDetail, error) {
	// seq is the page's versions, newest first, with the entry each belongs to.
	type item struct {
		v store.PolicyRuleVersion
		e *sysconfig.HistoryEntry
	}
	var seq []item
	for i := range pg.Entries {
		for _, ver := range pg.Entries[i].Versions {
			seq = append(seq, item{ver, &pg.Entries[i]})
		}
	}
	at := -1
	for i, it := range seq {
		if it.v.ID == pick {
			at = i
		}
	}
	if at < 0 {
		return nil, nil
	}
	d := &histDetail{Heading: "Changes in v" + vstr(pick)}
	var prev *store.PolicyRuleVersion
	if at+1 < len(seq) {
		prev = &seq[at+1].v
	} else if pg.NextBefore != 0 {
		older, err := h.d.PolicyRules.History(ctx, pg.NextBefore, 1)
		if err != nil {
			return nil, err
		}
		if len(older.Entries) > 0 {
			prev = &older.Entries[0].Versions[0]
		}
	}
	e := seq[at].e
	if e.Folded {
		// The picked version alone, against the one before it (none: an empty base, 0).
		var from int64
		if prev != nil {
			from = prev.ID
		}
		ch, err := h.d.PolicyRules.VersionDiff(ctx, from, pick)
		if err != nil {
			h.log.WarnContext(ctx, "policy version diff", "err", err)
		}
		d.Changes = ch
	} else {
		d.Changes, d.Reordered = e.Changes, e.Reordered
	}
	if prev == nil {
		d.Title = "v" + vstr(pick) + " (first version)"
		return d, nil
	}
	d.Title = "v" + vstr(prev.ID) + " → v" + vstr(pick)
	d.Revert, d.RevertAt = prev.ID, prev.At
	diff, err := h.d.PolicyRules.VersionDiff(ctx, current, prev.ID)
	if err != nil {
		h.log.WarnContext(ctx, "policy revert diff", "err", err) // the dialog then lists no changes; the revert itself still works
	}
	d.RevertDiff = diff
	return d, nil
}

// ruleHistoryRows is the "History of this rule" card of a rule page: only the versions that touched it.
func (h *handler) ruleHistoryRows(ctx context.Context, id, name string, current int64) []ruleHistRow {
	vs, err := h.allVersions(ctx)
	if err != nil {
		h.log.WarnContext(ctx, "rule history", "err", err)
		return nil
	}
	var rows []ruleHistRow
	for _, e := range sysconfig.RuleHistory(vs, id, name) {
		ver := e.Versions[0]
		note, full := shorten(ver.Note)
		rows = append(rows, ruleHistRow{ID: ver.ID, Summary: e.Summary, Note: note, NoteFull: full, By: h.pr.Text(ver.Actor), At: ver.At, Current: ver.ID == current})
	}
	return rows
}

// ruleHistRow is one version that touched a rule.
type ruleHistRow struct {
	ID       int64
	Summary  string
	Note     string
	NoteFull string
	By       string
	At       time.Time
	Current  bool
}
