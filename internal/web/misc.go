package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// backupDownload serves a stored backup to admins. Dumps and decoded JSON
// contain device secrets (spec 12.3), so every download is audited.
func (h *handler) backupDownload(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || n <= 0 {
		h.notFound(w, r)
		return
	}
	data, b, err := h.d.Backups.Open(r.Context(), model.BackupID(n))
	if errors.Is(err, store.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.d.Recorder.Record(r.Context(), model.AuditEvent{Actor: actor(r), Action: "backup.download",
			Target: fmt.Sprintf("backup:%d", n), Result: "error: " + err.Error()})
		h.fail(w, r, err)
		return
	}
	h.d.Recorder.Record(r.Context(), model.AuditEvent{Actor: actor(r), Action: "backup.download",
		Target: fmt.Sprintf("backup:%d", b.ID), Detail: fmt.Sprintf(`{"device":%d,"kind":%q}`, b.DeviceID, b.Kind)})
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", safeName(b)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data) //nolint:gosec // G705: attachment served as octet-stream with nosniff, not rendered
}

// safeName builds the download name from the stored backup, never from user
// input: "<device>-<UTC stamp>.dmp", with "-snapshot"/"-decoded" before the
// extension for the JSON kinds. The stored file name repeats the kind
// ("...-dmp.dmp") and says nothing about the device, so it is only the fallback.
func safeName(b model.Backup) string {
	ext := path.Ext(b.Path)
	if b.TakenAt.IsZero() || ext == "" {
		return sanitizeName(path.Base(b.Path), fmt.Sprintf("backup-%d", b.ID))
	}
	name := sanitizeName(b.DeviceName, fmt.Sprintf("device-%d", b.DeviceID))
	name += "-" + b.TakenAt.UTC().Format("20060102T150405Z")
	if b.Kind != model.BackupConfigDump {
		name += "-" + string(b.Kind)
	}
	return name + sanitizeName(ext, ".bin")
}

// sanitizeName keeps letters, digits, dot, dash and underscore; everything else
// becomes "_". An empty result yields fallback.
func sanitizeName(s, fallback string) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	if strings.Trim(sb.String(), "_.-") == "" {
		return fallback
	}
	return sb.String()
}

type window struct{ Value, Label string }

type auditPage struct {
	Actor, Target, Since, Action, Result string
	Query                                string // the search box (?q=): actor, action, target or device name
	Windows                              []window
	Actions                              []string
	// ActorChoices and Families fill the actor and action selects (N14): every
	// actor and every action family ("device.*") of the chosen period, not only
	// those of the rows the other filters left. Empty when the store cannot list them.
	ActorChoices []string
	Families     []string
	Choices      []ui.Option // subjects a typed name could mean (value) with their names; asks instead of merging
	Events       []auditRow
	Pager        pager
	Sort         tableSort
	SortBase     string
	Sub          string // the line under the title: how long events are kept
	// The options of the filter row's selects, the chosen one checked.
	ActorOpts, ActionOpts, SinceOpts, ResultOpts []ui.Option
}

// Filtered reports whether any filter narrows the list (the period alone does not).
func (p auditPage) Filtered() bool {
	return p.Actor != "" || p.Action != "" || p.Target != "" || p.Result != "" || p.Query != ""
}

// auditRow is an audit event with its target as a person reads it.
type auditRow struct {
	model.AuditEvent
	TargetText string // "kitchen lamp", "transaction #3"; the raw target when unknown
	TargetHref string // empty when there is nothing to open
	Label      string // readable action name
	Status     string // ok, failed or refused: the whole Result column
	Summary    string // one short line, at most auditSummaryMax characters
	SummaryAll string // the unclipped summary, for the tooltip
	Who        whoParts
}

// auditTarget turns "device:12" and "transaction:3" (older rows: "batch:3")
// into a name and a link. The raw value stays in the cell's tooltip.
func auditTarget(target string, names map[model.DeviceID]string, base string) (text, href string) {
	kind, id, ok := strings.Cut(target, ":")
	n, err := strconv.ParseInt(id, 10, 64)
	if !ok || err != nil {
		return auditTargetName(target), ""
	}
	switch kind {
	case "device":
		if name, known := names[model.DeviceID(n)]; known {
			return name, base + "/devices/" + id
		}
		return "removed device " + id, ""
	case "transaction", "batch":
		return "transaction #" + id, base + "/transactions/" + id
	}
	return auditTargetName(target), ""
}

// auditFetchLimit bounds what one request reads before the action and result
// filters and the paging are applied (the store filters actor, target, time).
const auditFetchLimit = 2000

var auditWindows = []struct {
	window
	d time.Duration
}{
	{window{"today", "today"}, -1}, // since local midnight, see auditSince
	{window{"24h", "last 24 hours"}, 24 * time.Hour},
	{window{"7d", "last 7 days"}, 7 * 24 * time.Hour},
	{window{"30d", "last 30 days"}, 30 * 24 * time.Hour},
	{window{"all", "all time"}, 0},
}

func (h *handler) auditLog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p := auditPage{Actor: strings.TrimSpace(q.Get("actor")), Target: strings.TrimSpace(q.Get("target")), Since: q.Get("since"),
		Action: strings.TrimSpace(q.Get("action")), Result: q.Get("result"), Query: strings.TrimSpace(q.Get("q"))}
	if p.Since == "" {
		p.Since = "7d"
	}
	f := store.AuditFilter{Target: p.Target, Limit: auditFetchLimit, Search: p.Query}
	family, isFamily := auditFamily(p.Action)
	if isFamily {
		f.ActionPrefix = family // in the store: a family filter must not be cut by the fetch limit
	}
	var names map[model.DeviceID]string
	loadNames := func() map[model.DeviceID]string {
		if names == nil {
			names = map[model.DeviceID]string{}
			if rows, err := h.rows(r.Context()); err == nil {
				for _, row := range rows {
					names[row.Device.ID] = row.Device.Name
				}
			}
		}
		return names
	}
	if p.Query != "" {
		f.SearchTargets = auditNameTargets(loadNames(), p.Query)
		f.SearchActors = h.pr.SubjectsNamed(p.Query) // an SSO subject is a hash; people search by name
	}
	subjects, ambiguous := h.pr.Candidates(p.Actor)
	if ambiguous {
		for _, s := range subjects { // several users answer to this name: ask, do not merge them
			w := h.pr.Parts(s)
			p.Choices = append(p.Choices, ui.Option{Value: s, Label: strings.TrimSpace(w.Name + " " + w.Badge)})
		}
	} else {
		f.Actors = subjects
	}
	for _, win := range auditWindows {
		p.Windows = append(p.Windows, win.window)
		if win.Value == p.Since {
			f.Since = auditSince(h.d.Clock.Now(), win.d)
		}
	}
	if fs, ok := h.d.Audit.(store.AuditFacetStore); ok {
		actors, actions, err := fs.AuditFacets(r.Context(), f.Since)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		p.ActorChoices, p.Families = actors, auditFamilies(actions)
	}
	var all []model.AuditEvent
	if !ambiguous {
		var err error
		if all, err = h.d.Audit.ListAudit(r.Context(), f); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	seen := map[string]bool{}
	var kept []model.AuditEvent
	for _, e := range all {
		if !seen[e.Action] {
			seen[e.Action] = true
			p.Actions = append(p.Actions, e.Action)
		}
		if p.Action != "" && !isFamily && e.Action != p.Action {
			continue
		}
		if p.Result == "ok" && auditStatus(e) != auditOK || p.Result == "error" && auditStatus(e) == auditOK {
			continue
		}
		kept = append(kept, e)
	}
	sort.Strings(p.Actions)
	page, _ := strconv.Atoi(q.Get("page"))
	def := savedPageSize(h.userPrefs(r))
	size := pageSize(q, def)
	filters := url.Values{}
	for k, s := range map[string]string{"actor": p.Actor, "target": p.Target, "since": p.Since, "action": p.Action, "result": p.Result, "q": p.Query} {
		if s != "" {
			filters.Set(k, s)
		}
	}
	p.SortBase = httpBase(r) + "/audit"
	if enc := filters.Encode(); enc != "" {
		p.SortBase += "?" + enc
	}
	p.Sort = h.sortOf(r, "audit", tableSort{Key: "when", Dir: "desc"})
	sortAudit(kept, p.Sort)
	p.Pager = newPager(httpBase(r)+"/audit", p.Sort.values(filters), page, size, def, len(kept), "events")
	lo := (p.Pager.Page - 1) * size
	if hi := min(lo+size, len(kept)); lo < hi {
		names := loadNames()
		for _, e := range kept[lo:hi] {
			text, href := auditTarget(e.Target, names, httpBase(r))
			all := auditSummary(e)
			p.Events = append(p.Events, auditRow{AuditEvent: e, TargetText: text, TargetHref: href, Label: auditLabel(e.Action),
				Status: auditStatus(e), Summary: clip(all, auditSummaryMax), SummaryAll: all, Who: h.pr.Parts(e.Actor)})
		}
	}
	h.auditOptions(&p)
	p.Sub = "Who changed what"
	if days := h.auditKeptDays(); days > 0 {
		p.Sub += ", kept for " + plural(days, "day")
	}
	v := h.view(r, "Audit log", "audit", nil)
	h.show(w, r, http.StatusOK, "audit", v, auditView(v.Base, p))
}

// auditOptions fills the selects of the filter row. A chosen value the period does not list
// (an exact action from a link, an actor without events in the period) stays selectable.
func (h *handler) auditOptions(p *auditPage) {
	opt := func(value, label, cur string) ui.Option {
		return ui.Option{Value: value, Label: label, Checked: value == cur}
	}
	p.ActorOpts = []ui.Option{opt("", "All actors", p.Actor)}
	actors := p.ActorChoices
	if p.Actor != "" && !slices.Contains(actors, p.Actor) {
		actors = append([]string{p.Actor}, actors...)
	}
	for _, a := range actors {
		w := h.pr.Parts(a)
		label := w.Name
		if w.Badge != "" {
			label += " · " + w.Badge // the chip of the User column, as text
		}
		p.ActorOpts = append(p.ActorOpts, opt(a, label, p.Actor))
	}
	p.ActionOpts = []ui.Option{opt("", "All actions", p.Action)}
	actions := p.Families
	if p.Action != "" && !slices.Contains(actions, p.Action) {
		actions = append([]string{p.Action}, actions...)
	}
	for _, a := range actions {
		p.ActionOpts = append(p.ActionOpts, opt(a, a, p.Action))
	}
	for _, w := range p.Windows {
		p.SinceOpts = append(p.SinceOpts, opt(w.Value, capitalize(w.Label), p.Since))
	}
	p.ResultOpts = []ui.Option{opt("", "Any result", p.Result), opt("ok", "OK", p.Result), opt("error", "Refused or failed", p.Result)}
}

// auditKeptDays is the audit retention in whole days; 0 when the settings are not available.
func (h *handler) auditKeptDays() int {
	if h.d.SysConfig == nil {
		return 0
	}
	for _, st := range h.d.SysConfig.State() {
		if st.Key == "AUDIT_RETENTION" {
			if d, err := time.ParseDuration(st.Value); err == nil {
				return int(d / (24 * time.Hour))
			}
		}
	}
	return 0
}

// auditSince is the start of a period ending now: d > 0 is a rolling window,
// d < 0 is "today" (since local midnight, the server's time zone), 0 is all time.
func auditSince(now time.Time, d time.Duration) time.Time {
	switch {
	case d > 0:
		return now.Add(-d)
	case d < 0:
		y, m, day := now.Date()
		return time.Date(y, m, day, 0, 0, 0, 0, now.Location())
	}
	return time.Time{}
}

// auditFamily reads an action filter of the form "device.*" as the prefix
// "device.". Any other value is an exact action name.
func auditFamily(action string) (prefix string, ok bool) {
	if f, cut := strings.CutSuffix(action, ".*"); cut && f != "" && !strings.Contains(f, "*") {
		return f + ".", true
	}
	return "", false
}

// auditFamilies turns action names into the sorted, distinct families of the
// action select: "device.restart" and "device.backup" are "device.*". An
// action without a dot is its own entry.
func auditFamilies(actions []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range actions {
		f := a
		if head, _, ok := strings.Cut(a, "."); ok && head != "" {
			f = head + ".*"
		}
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// auditNameTargets returns the targets ("device:N") of the devices whose name
// contains q (case insensitive): the search box finds "Kitchen Plug" although
// the log stores "device:12".
func auditNameTargets(names map[model.DeviceID]string, q string) []string {
	needle := strings.ToLower(q)
	var out []string
	for id, n := range names {
		if strings.Contains(strings.ToLower(n), needle) {
			out = append(out, "device:"+strconv.FormatInt(int64(id), 10))
		}
	}
	sort.Strings(out)
	return out
}

// resultTone colours the result of an audited action.
func resultTone(res string) string {
	l := strings.ToLower(res)
	switch {
	case l == "ok":
		return "ok"
	case strings.Contains(l, "refus") || strings.Contains(l, "denied") || strings.Contains(l, "forbidden"):
		return "warn"
	}
	return "bad"
}

func (h *handler) about(w http.ResponseWriter, r *http.Request) {
	v := h.view(r, "About", "about", nil)
	v.Live = true // About reports health: the top bar says whether the page still hears the server
	p := aboutView{
		Base: v.Base, Version: h.d.Version,
		Platform: runtime.GOOS + "/" + runtime.GOARCH + " · " + runtime.Version(),
		Uptime:   humanDuration(h.d.Clock.Now().Sub(h.started).Round(time.Minute)),
		Comps:    h.aboutComps(r),
	}
	if devs, err := h.d.Inventory.List(r.Context()); err == nil {
		p.Devices = len(devs)
	}
	for _, kv := range h.d.Config {
		val := cfgText(kv.Value)
		p.Config = append(p.Config, aboutSetting{Key: kv.Key, Value: urlText(val), Secret: val == "***", Link: urlLink(val)})
	}
	h.show(w, r, http.StatusOK, "about", v, aboutPage(p))
}

// mqttComp is the MQTT row of About: connected, not connected (the broker is configured but
// unreachable) or not configured (devices are polled over HTTP only).
func mqttComp(state string) aboutComp {
	switch state {
	case "connected":
		return aboutComp{Name: "MQTT broker", Detail: "Connected", State: "Healthy", Tone: "ok"}
	case "disconnected":
		return aboutComp{Name: "MQTT broker", Detail: "Not connected · retrying", State: "Failing", Tone: "danger"}
	}
	return aboutComp{Name: "MQTT broker", Detail: "Not configured · devices are polled over HTTP", State: "Off", Tone: "neutral"}
}

// decoderComp is the backup decoder row: without decode-config backups keep only the raw file.
func decoderComp(version string) aboutComp {
	if version == "" {
		return aboutComp{Name: "Backup decoder", Detail: "Not available · backups keep the raw .dmp file only: no decoded JSON, no comparison", State: "Missing", Tone: "warn"}
	}
	return aboutComp{Name: "Backup decoder", Detail: "decode-config " + version + " available", State: "Healthy", Tone: "ok"}
}

// cfgText words an effective-configuration value for people: Go's list form "[a b]" becomes
// "a, b", an empty list is empty (the page says "not set"), and a duration such as "720h0m0s"
// reads "30d 0h". Everything else is shown as it is.
func cfgText(v string) string {
	if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
		return strings.Join(strings.Fields(v[1:len(v)-1]), ", ")
	}
	if strings.ContainsAny(v, "hms") && !strings.ContainsAny(v, " /:") {
		if d, err := time.ParseDuration(v); err == nil {
			if d == 0 {
				return "0s"
			}
			return humanDuration(d)
		}
	}
	return v
}
