package web

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/settings"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The rule page (GET /policy/rules/{id}) and the New rule page (GET /policy/rules/new). Both end
// in the same page: a rule with its numbers, its editor and the devices it judges. A new rule is
// an unsaved draft there (id "new"); nothing is stored before "Save rule".

// ruleOpts says what the rule page shows beyond the saved rule.
type ruleOpts struct {
	ID       string              // "new" for a draft, else the rule's id
	Draft    *ruleDraft          // the form as posted (a refused save): shown again with its problem
	Spec     *sysconfig.RuleSpec // a draft the server built (template, command, copy)
	Origin   string              // template:<id>, copy:<device id>, duplicate:<rule id> or command: kept in the form
	Source   string              // the draft's source in words ("Copied from kitchen · read live now")
	ErrField string
	ErrMsg   string
	Banner   string
}

// ruleRows is how many devices the devices card lists before "Show all".
const ruleRows = 25

// ruleList is the saved rules the rule page works on.
type ruleList struct {
	Specs   []sysconfig.RuleSpec
	Version int64
}

func (h *handler) ruleList(r *http.Request) (ruleList, error) {
	st, err := h.d.PolicyRules.State(r.Context())
	return ruleList{Specs: st.Specs, Version: st.VersionID}, err
}

func indexOfRule(specs []sysconfig.RuleSpec, id string) int {
	return slices.IndexFunc(specs, func(s sysconfig.RuleSpec) bool { return s.ID == id })
}

// ruleDevRow is one device of the devices card.
type ruleDevRow struct {
	ID     model.DeviceID
	Name   string
	Have   string
	Source string
	Kind   policy.OutcomeKind
}

// ruleStat is what the rule does to the fleet: the counts of the four tiles and the rows.
type ruleStat struct {
	OK                                                          bool
	Err, Hint                                                   string
	Devices                                                     int // all devices of the fleet
	Applies, Match, Deviate, Unknown, Excepted, Shadowed, Total int
	Rows                                                        []ruleDevRow
}

// ruleStats evaluates the rule at pos of specs (pos == len(specs): a draft appended to the list)
// from cached values and backups; no device is read. The rule counts as if enabled: what it
// would judge is the point, also for a rule that is switched off.
func (h *handler) ruleStats(ctx context.Context, src ruleList, spec sysconfig.RuleSpec, pos int) (ruleStat, error) {
	if strings.TrimSpace(spec.Command) == "" || strings.TrimSpace(spec.Want) == "" {
		return ruleStat{Hint: "Enter a command and an expected value to see which devices the rule would judge."}, nil
	}
	spec.Disabled = false
	list := slices.Clone(src.Specs)
	if pos >= len(list) {
		list = append(list, spec)
		pos = len(list) - 1
	} else {
		list[pos] = spec
	}
	// A rule that does not validate is left out of the evaluation, so the page of a valid rule
	// still works when another stored rule is broken. When the broken one is this rule, say why.
	rules, _, bad := h.d.PolicyRules.ValidRules(list)
	dropped := 0
	for _, re := range bad {
		switch {
		case re.Index < 0:
			return ruleStat{Err: re.Error()}, nil
		case re.Index == pos:
			if field, text := ruleProblem(re.Msg); field == "command" {
				return ruleStat{Err: text}, nil // the reason, not the validator's sentence
			}
			return ruleStat{Err: re.Msg}, nil
		case re.Index < pos:
			dropped++
		}
	}
	pos -= dropped
	rep, err := h.d.Policy.Evaluate(ctx, rules)
	if err != nil {
		return ruleStat{}, err
	}
	return h.ruleStatOf(rep, rules, pos), nil
}

func (h *handler) ruleStatOf(rep PolicyReport, rules []policy.Rule, pos int) ruleStat {
	st := ruleStat{OK: true, Devices: len(rep.Devices)}
	names := make(map[model.DeviceID]string, len(rep.Devices))
	byID := make(map[model.DeviceID]model.Device, len(rep.Devices))
	for _, d := range rep.Devices {
		names[d.ID] = d.Name
		byID[d.ID] = d
	}
	key := strings.ToLower(rules[pos].Command)
	now := h.d.Clock.Now()
	for _, o := range rep.Outcomes {
		if o.RuleIndex != pos {
			continue
		}
		row := ruleDevRow{ID: o.DeviceID, Name: names[o.DeviceID], Kind: o.Kind}
		src, haveSrc := rep.Provenance[o.DeviceID][key]
		switch o.Kind {
		case policy.OutcomeMatch:
			st.Match++
			row.Have, row.Source = o.Have, valueSource(src, haveSrc, now)
		case policy.OutcomeDeviate:
			st.Deviate++
			row.Have, row.Source = o.Have, valueSource(src, haveSrc, now)
		case policy.OutcomeUnknown:
			st.Unknown++
			switch note := notJudgedNote(rules[pos].Command, byID[o.DeviceID]); {
			case note != "":
				row.Source = note // the device's firmware does not know the command: nothing to wait for
			case haveSrc:
				row.Source = "chip family unknown" // the value is there, the family-limited rule cannot be judged
			case rep.NotLive[key]:
				row.Source = "not readable live"
			default:
				row.Source = "no backup yet"
			}
		case policy.OutcomeShadowed:
			st.Shadowed++
			row.Source = "covered by " + ruleName(sysconfig.RuleSpec{Name: rules[o.ByIndex].Name, Command: rules[o.ByIndex].Command})
		case policy.OutcomeExcepted:
			st.Excepted++
			continue // exempt: counted in the tile, not listed
		}
		st.Rows = append(st.Rows, row)
	}
	st.Applies = st.Match + st.Deviate + st.Unknown
	st.Total = len(st.Rows)
	// What differs first, then what could not be judged, then the rest.
	rank := map[policy.OutcomeKind]int{policy.OutcomeDeviate: 0, policy.OutcomeUnknown: 1, policy.OutcomeMatch: 2, policy.OutcomeShadowed: 3}
	sort.SliceStable(st.Rows, func(i, j int) bool {
		a, b := st.Rows[i], st.Rows[j]
		if rank[a.Kind] != rank[b.Kind] {
			return rank[a.Kind] < rank[b.Kind]
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return st
}

// valueSource is the Source cell of a judged device: "live · 6 min" or "backup · 28 Oct".
func valueSource(s ValueSource, ok bool, now time.Time) string {
	switch {
	case !ok:
		return ""
	case s.Kind == "backup":
		return "backup · " + s.At.Local().Format("2 Jan")
	}
	return "live · " + shortDuration(now.Sub(s.At))
}

// ruleTile is one of the four tiles.
type ruleTile struct {
	Key, Label, Value, Sub string
	Tone                   ui.Tone
}

func ruleTiles(st ruleStat, spec sysconfig.RuleSpec) []ruleTile {
	if !st.OK {
		dash := func(key, label string) ruleTile { return ruleTile{Key: key, Label: label, Value: "–"} }
		return []ruleTile{dash("applies", "Applies to"), dash("match", "Match"), dash("deviate", "Deviate"), dash("unknown", "Not judged")}
	}
	exc := plural(len(spec.Except), "exception") // the draft's own count, so it follows the form
	matchSub := "no device in scope"
	if st.Applies > 0 {
		matchSub = strconv.Itoa((st.Match*100+st.Applies/2)/st.Applies) + " % of the devices it applies to"
	}
	devSub := "required"
	if spec.Severity == policy.SeverityInfo {
		devSub = "advisory: hint only"
	}
	return []ruleTile{
		{Key: "applies", Label: "Applies to", Value: strconv.Itoa(st.Applies), Sub: scopeShort(spec, true) + " · " + exc},
		{Key: "match", Label: "Match", Value: strconv.Itoa(st.Match), Sub: matchSub, Tone: ui.ToneOK},
		{Key: "deviate", Label: "Deviate", Value: strconv.Itoa(st.Deviate), Sub: devSub, Tone: ui.ToneWarn},
		{Key: "unknown", Label: "Not judged", Value: strconv.Itoa(st.Unknown), Sub: "no backup or recent reading"},
	}
}

// ruleDevicesView is the devices card (also the htmx answer of the preview).
type ruleDevicesView struct {
	Base, ID  string // ID: the rule's id, "new" for a draft
	Draft     bool
	Edit      bool // the card is re-drawn by the preview while the form is edited
	Show      string
	All       bool
	Stat      ruleStat
	Rows      []ruleDevRow // the rows shown: filtered, and cut to ruleRows unless All
	Shown     int          // rows after the filter
	NAll      int          // rows without a filter
	Disabled  bool
	NoDevices bool
}

func newRuleDevices(base, id string, edit bool, spec sysconfig.RuleSpec, st ruleStat, show string, all bool) ruleDevicesView {
	v := ruleDevicesView{Base: base, ID: id, Draft: id == "new", Edit: edit, Show: show, All: all, Stat: st, Disabled: spec.Disabled, NoDevices: st.OK && st.Devices == 0}
	v.NAll = st.Total
	for _, r := range st.Rows {
		switch {
		case show == "deviate" && r.Kind != policy.OutcomeDeviate:
		case show == "unknown" && r.Kind != policy.OutcomeUnknown:
		default:
			v.Rows = append(v.Rows, r)
		}
	}
	v.Shown = len(v.Rows)
	if !all && len(v.Rows) > ruleRows {
		v.Rows = v.Rows[:ruleRows]
	}
	return v
}

// showParams reads ?show= (all, deviate, unknown) and ?all=1 of the devices card.
func showParams(r *http.Request) (show string, all bool, ok bool) {
	q := r.URL.Query()
	show = q.Get("show")
	if show == "all" {
		show = ""
	}
	if show != "" && show != "deviate" && show != "unknown" {
		return "", false, false
	}
	return show, q.Get("all") == "1", true
}

// rulePageView is the rule page.
type rulePageView struct {
	Base, CSRF string
	Admin      bool // the form and the actions are shown
	ID         string
	Draft      bool
	Name       string
	Required   bool
	Disabled   bool
	Line       string
	Scope      scopeSummary
	Version    int64
	Origin     string
	Banner     string
	BannerTone ui.Tone
	Source     string
	Tiles      []ruleTile
	Editor     *ruleEditor
	KV         []ui.KVRow
	Devices    ruleDevicesView
	DeleteText string
	History    []ruleHistRow // the versions that touched this rule, newest first
}

// rulePage is GET /policy/rules/{id}.
func (h *handler) rulePage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !policy.RuleIDRe.MatchString(id) {
		h.notFound(w, r)
		return
	}
	if _, _, ok := showParams(r); !ok {
		h.notFound(w, r)
		return
	}
	h.rulePageRender(w, r, http.StatusOK, ruleOpts{ID: id})
}

// ruleFlashes are the confirmations that come back to a rule page.
func ruleFlash(ok string) string {
	if ok == "checked" {
		return "Live check finished"
	}
	if ok == "throttled" {
		return "Live check rate-limited, try again in a moment"
	}
	if ok == "checkfailed" {
		return "The live check could not run"
	}
	return policyFlashes[ok]
}

// rulePageRender writes the rule page: a saved rule, or the unsaved draft (id "new") of a
// template, a command or a copy. Without a draft or a spec, id "new" is the New rule page.
func (h *handler) rulePageRender(w http.ResponseWriter, r *http.Request, status int, o ruleOpts) {
	if o.ID == "new" && o.Draft == nil && o.Spec == nil {
		h.ruleNewRender(w, r, status, newRuleOpts{Banner: o.Banner})
		return
	}
	src, err := h.ruleList(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	draft := o.ID == "new"
	pos := len(src.Specs)
	var spec sysconfig.RuleSpec
	switch {
	case draft && o.Draft != nil:
		spec = o.Draft.Spec
	case draft:
		spec = *o.Spec
	default:
		if pos = indexOfRule(src.Specs, o.ID); pos < 0 {
			h.notFound(w, r)
			return
		}
		spec = src.Specs[pos]
		if o.Draft != nil {
			spec = o.Draft.Spec
			spec.ID, spec.Disabled = o.ID, draftDisabled(*o.Draft, src.Specs)
		}
	}
	show, all, _ := showParams(r)
	stat, err := h.ruleStats(r.Context(), src, spec, pos)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if o.ErrMsg != "" && stat.Err != "" {
		// The refused save already says what is wrong at its field: the devices card does not repeat it.
		stat = ruleStat{Hint: "Fix the rule above to see which devices it would judge."}
	}
	name := ruleName(spec)
	if spec.Command == "" {
		name = "New rule"
	}
	v := h.view(r, name, "policy", nil)
	canEdit := v.IsAdmin
	v.Crumbs = []ui.Crumb{{Label: "Devices", Path: "/devices"}, {Label: "Policy", Path: policyPath}, {Label: name}}
	if f := ruleFlash(r.URL.Query().Get("ok")); f != "" {
		v.Flash = f
	}
	p := rulePageView{
		Base: v.Base, CSRF: v.CSRF, Admin: canEdit, ID: o.ID, Draft: draft,
		Name: name, Required: spec.Severity != policy.SeverityInfo, Disabled: spec.Disabled,
		Version: src.Version, Origin: o.Origin, Source: o.Source,
		Banner: o.Banner, BannerTone: ui.ToneDanger,
		Tiles: ruleTiles(stat, spec),
	}
	if strings.HasPrefix(o.Banner, "Nothing changed") {
		p.BannerTone = ui.ToneAccent
	}
	p.Line = ruleLine(spec, draft, src.Version)
	p.Scope = newScopeSummary(spec, stat)
	p.Devices = newRuleDevices(v.Base, o.ID, canEdit, spec, stat, show, all)
	if canEdit {
		e, err := h.editorFields(r, spec)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		e.ID, e.Version = o.ID, src.Version
		e.Err, e.ErrField = o.ErrMsg, o.ErrField
		e.Control.Invalid = o.ErrField == "want"
		e.Enabled = !spec.Disabled // a duplicate keeps its source's state; the other drafts start enabled
		p.Editor = e
		if !draft {
			p.DeleteText = "It currently judges " + plural(stat.Applies, "device") + ". They stop being checked for " + spec.Command +
				". The change is saved as a new version you can revert to."
			if spec.Disabled {
				p.DeleteText = "It is disabled and judges no device. The change is saved as a new version you can revert to."
			}
		}
	} else {
		p.KV = ruleKV(spec)
	}
	if !draft {
		key := spec.Name // the history names a rule by its name, or its command when unnamed
		if key == "" {
			key = spec.Command
		}
		p.History = h.ruleHistoryRows(r.Context(), o.ID, key, src.Version)
	}
	h.show(w, r, status, "policyrule", v, rulePageTempl(p))
}

// ruleKV is the rule as read-only rows (operators).
func ruleKV(s sysconfig.RuleSpec) []ui.KVRow {
	sev := policy.SeverityLabel(s.Severity)
	rows := []ui.KVRow{
		{Key: "Name", Value: ruleName(s)},
		{Key: "Applies to", Value: appliesText(s)},
		{Key: "Command", Value: s.Command, Mono: true},
		{Key: "Expected value", Value: wantText(s.Command, s.Want), Mono: true},
		{Key: "When it deviates", Value: sev},
	}
	if len(s.Except) > 0 {
		rows = append(rows, ui.KVRow{Key: "Exceptions", Value: strings.Join(s.Except, ", ")})
	}
	if s.Note != "" {
		rows = append(rows, ui.KVRow{Key: "Note", Value: s.Note})
	}
	return rows
}

// ruleLine is the line under the title: what the rule checks and whom it applies to.
func ruleLine(spec sysconfig.RuleSpec, draft bool, version int64) string {
	line := spec.Command + " = " + spec.Want + " · " + scopeShort(spec, false)
	if n := len(spec.Except); n > 0 {
		line += " · " + plural(n, "exception")
	}
	if !draft {
		line += " · version " + vstr(version)
	}
	return line
}

// scopeShort is the scope in few words, as the tiles and the line under the title show it:
// "ESP8266, tag lights, 2 devices" (tiles count tags: "ESP8266, 1 tag, 2 devices").
func scopeShort(s sysconfig.RuleSpec, countTags bool) string {
	var parts []string
	if len(s.Families) > 0 {
		parts = append(parts, strings.Join(policy.ScopeLabels(s.Families), ", "))
	}
	if n := len(s.Tags); n > 0 {
		if countTags {
			parts = append(parts, plural(n, "tag"))
		} else if n == 1 {
			parts = append(parts, "tag "+s.Tags[0])
		} else {
			parts = append(parts, "tags "+strings.Join(s.Tags, ", "))
		}
	}
	if n := len(s.Devices); n > 0 {
		parts = append(parts, plural(n, "device"))
	}
	if len(parts) == 0 {
		return "All devices"
	}
	return strings.Join(parts, ", ")
}

// scopeSummary is the line of the Applies to section: how many devices the draft reaches and why.
type scopeSummary struct {
	Count string // "12 devices"; empty while the draft cannot be judged
	Text  string // "ESP8266, tag “lights” or 2 picked devices, minus 1 exception"
}

func newScopeSummary(spec sysconfig.RuleSpec, st ruleStat) scopeSummary {
	var parts []string
	if len(spec.Families) > 0 {
		parts = append(parts, strings.Join(policy.ScopeLabels(spec.Families), ", "))
	}
	if len(spec.Tags) > 0 {
		quoted := make([]string, len(spec.Tags))
		for i, t := range spec.Tags {
			quoted[i] = "“" + t + "”"
		}
		noun := "tag "
		if len(quoted) > 1 {
			noun = "tags "
		}
		parts = append(parts, noun+strings.Join(quoted, ", "))
	}
	text := strings.Join(parts, ", ")
	if n := len(spec.Devices); n > 0 {
		picked := plural(n, "picked device")
		if text == "" {
			text = picked
		} else {
			text += " or " + picked
		}
	}
	if text == "" {
		text = "every device"
	}
	if n := len(spec.Except); n > 0 {
		text += ", minus " + plural(n, "exception")
	}
	sum := scopeSummary{Text: text}
	if st.OK {
		sum.Count = plural(st.Applies, "device")
	}
	return sum
}

// ruleHead is what the preview swaps in the page head: the title, the trail, the phone title and,
// for a draft started from a command, the notice that names it.
type ruleHead struct {
	Name                      string
	Required, Draft, Disabled bool
	Base, Source              string
	WantWarn                  *checkView
	Crumbs                    []ui.Crumb
}

// ruleDevicesPreview answers the editor's htmx post: the devices card for the draft in the
// form. Cached values and backups only, no device is read, nothing is stored.
func (h *handler) ruleDevicesPreview(w http.ResponseWriter, r *http.Request, d ruleDraft, st sysconfig.RulesState) {
	show, all, ok := showParams(r)
	if !ok {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	pos := len(st.Specs)
	spec := d.Spec
	if d.ID != "new" {
		if pos = indexOfRule(st.Specs, d.ID); pos < 0 {
			h.notFound(w, r)
			return
		}
		spec.ID = d.ID
	}
	stat, err := h.ruleStats(r.Context(), ruleList{Specs: st.Specs, Version: st.VersionID}, spec, pos)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	spec.Disabled = d.State == "disabled"
	// The devices card is the swap target; the tiles, the line under the title and the summary of
	// the Applies to section describe the same draft and are swapped out-of-band.
	name := ruleName(spec)
	if spec.Command == "" {
		name = "New rule"
	}
	head := ruleHead{Name: name, Required: spec.Severity != policy.SeverityInfo, Draft: d.ID == "new", Disabled: spec.Disabled, Base: httpBase(r),
		WantWarn: wantWarning(spec.Command, spec.Want),
		Crumbs:   []ui.Crumb{{Label: "Devices", Path: "/devices"}, {Label: "Policy", Path: policyPath}, {Label: name}}}
	if d.Origin == "command" {
		head.Source = "A new rule for " + spec.Command + ". Nothing is saved until you press Save rule."
	}
	h.component(w, r, http.StatusOK, rulePreviewAnswer(
		ruleDevicesCard(newRuleDevices(httpBase(r), d.ID, true, spec, stat, show, all)),
		ruleTiles(stat, spec), ruleLine(spec, d.ID == "new", st.VersionID), newScopeSummary(spec, stat), head))
}

// ---- New rule ----

// newRuleOpts carries what the New rule page shows beyond the templates.
type newRuleOpts struct {
	Q                    string
	Command, CommandErr  string // the Any command card
	CopyDevice           int64
	CopyCommand, CopyErr string
	Banner               string
}

type tplCard struct {
	ID, Title, KV, Hint string
	Already             bool // a rule with this command and scope exists
}

type tplGroup struct {
	Name  string
	Cards []tplCard
}

type copyDevice struct {
	ID   int64
	Name string
	Sel  bool
}

type newRuleView struct {
	Base, CSRF  string
	Q           string
	Groups      []tplGroup
	NoMatch     bool
	Command     string
	CommandErr  string
	Devices     []copyDevice
	CopyCommand string
	CopyErr     string
	Banner      string
}

// sameScope says whether two rules judge the same devices.
func sameScope(a, b sysconfig.RuleSpec) bool {
	eq := func(x, y []string) bool {
		x, y = slices.Clone(x), slices.Clone(y)
		slices.Sort(x)
		slices.Sort(y)
		return slices.Equal(x, y)
	}
	return eq(a.Families, b.Families) && eq(a.Tags, b.Tags) && eq(a.Devices, b.Devices)
}

// ruleNew is GET /policy/rules/new: the chooser, or with ?template= / ?command= the draft rule.
func (h *handler) ruleNew(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for k, vs := range q {
		if (k != "q" && k != "template" && k != "command") || len(vs) != 1 {
			http.Error(w, "unknown form field", http.StatusBadRequest)
			return
		}
	}
	switch {
	case q.Get("template") != "":
		tpls := policyTemplates(h.brokerHost())
		i := slices.IndexFunc(tpls, func(t ruleTemplate) bool { return t.ID == q.Get("template") })
		if i < 0 {
			h.notFound(w, r)
			return
		}
		spec := tpls[i].Spec
		h.rulePageRender(w, r, http.StatusOK, ruleOpts{ID: "new", Spec: &spec, Origin: "template:" + tpls[i].ID,
			Source: "Started from the template “" + tpls[i].Title + "”. Nothing is saved until you press Save rule."})
	case q.Get("command") != "":
		cmd := strings.TrimSpace(q.Get("command"))
		if why := commandRefusal(cmd); why != "" {
			h.ruleNewRender(w, r, http.StatusUnprocessableEntity, newRuleOpts{Command: cmd, CommandErr: why})
			return
		}
		spec := sysconfig.RuleSpec{Name: settingName(cmd), Command: cmd, Severity: policy.SeverityInfo}
		h.rulePageRender(w, r, http.StatusOK, ruleOpts{ID: "new", Spec: &spec, Origin: "command",
			Source: "A new rule for " + cmd + ". Nothing is saved until you press Save rule."})
	default:
		h.ruleNewRender(w, r, http.StatusOK, newRuleOpts{Q: strings.TrimSpace(q.Get("q"))})
	}
}

// settingName is the name a new rule for command starts with: the setting's label.
func settingName(command string) string {
	l, _ := settingLabel(command)
	return l
}

// commandRefusal says why a rule may not name command, "" when it may. The same checks run
// before any device is asked (ReadValue repeats them): a refused command is never sent.
func commandRefusal(command string) string {
	if command == "" {
		return "Enter a command."
	}
	if err := policy.ReadProblem(command); err != nil {
		return upperFirst(err.Error()) + "."
	}
	if !settings.Queryable(command) {
		return "This command cannot be read without side effects."
	}
	return ""
}

// ruleDuplicate is GET /policy/rules/duplicate?id=: an unsaved draft that starts as a copy of the
// rule. Nothing is stored; Save rule creates the copy (below its source, see place).
func (h *handler) ruleDuplicate(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for k, vs := range q {
		if k != "id" || len(vs) != 1 {
			http.Error(w, "unknown form field", http.StatusBadRequest)
			return
		}
	}
	id := q.Get("id")
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
	spec := st.Specs[n]
	spec.ID = ""
	spec.Families, spec.Tags = slices.Clone(spec.Families), slices.Clone(spec.Tags)
	spec.Devices, spec.Except = slices.Clone(spec.Devices), slices.Clone(spec.Except)
	if spec.Name != "" {
		spec.Name = copyName(spec.Name)
	}
	h.rulePageRender(w, r, http.StatusOK, ruleOpts{ID: "new", Spec: &spec, Origin: "duplicate:" + id,
		Source: "A copy of " + ruleName(st.Specs[n]) + ". Nothing is saved until you press Save rule."})
}

// copyName is the name of a duplicated rule: "<name> (copy)", shortened so it stays within the
// name limit (80 bytes).
func copyName(name string) string {
	const suffix, limit = " (copy)", 80
	if len(name)+len(suffix) > limit {
		rs := []rune(name)
		for len(string(rs))+len(suffix) > limit {
			rs = rs[:len(rs)-1]
		}
		name = strings.TrimSpace(string(rs))
	}
	return name + suffix
}

// ruleCopy is POST /policy/rules/new/copy: read one setting of one device and open a draft rule
// that expects exactly that value. Nothing is saved.
func (h *handler) ruleCopy(w http.ResponseWriter, r *http.Request) {
	f, ok := strictForm(w, r, []string{"csrf", "device", "command"}, nil)
	if !ok {
		return
	}
	cmd := strings.TrimSpace(f.Get("command"))
	id, err := strconv.ParseInt(f.Get("device"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	fail := func(status int, msg string) {
		h.ruleNewRender(w, r, status, newRuleOpts{CopyDevice: id, CopyCommand: cmd, CopyErr: msg})
	}
	if why := commandRefusal(cmd); why != "" {
		fail(http.StatusUnprocessableEntity, why)
		return
	}
	dev, found := h.deviceByID(r, model.DeviceID(id))
	if !found {
		h.notFoundDevice(w, r)
		return
	}
	value, src, err := h.d.Policy.ReadValue(r.Context(), actor(r), dev.ID, cmd)
	var refused *ValueRefusedError
	switch {
	case errors.As(err, &refused):
		fail(http.StatusUnprocessableEntity, upperFirst(refused.Reason)+".")
		return
	case err != nil:
		fail(http.StatusUnprocessableEntity, "Could not read "+cmd+" from "+dev.Name+": "+sentence(err.Error()))
		return
	}
	spec := sysconfig.RuleSpec{Name: settingName(cmd), Command: cmd, Want: value, Severity: policy.SeverityInfo}
	if strings.EqualFold(cmd, "OtaUrl") && dev.Identity.Family != model.ChipUnknown && slices.Contains(policy.FamilyNames(), string(dev.Identity.Family)) {
		spec.Families = []string{string(dev.Identity.Family)} // a firmware URL fits one chip family
	}
	from := "read live now"
	if src.Kind == "backup" {
		from = "backup " + src.At.Local().Format("2 Jan")
	}
	h.rulePageRender(w, r, http.StatusOK, ruleOpts{ID: "new", Spec: &spec, Origin: "copy:" + strconv.FormatInt(id, 10),
		Source: "Copied from " + dev.Name + " · " + from + ". Nothing is saved until you press Save rule."})
}

func (h *handler) deviceByID(r *http.Request, id model.DeviceID) (model.Device, bool) {
	devs, err := h.d.Inventory.List(r.Context())
	if err != nil {
		return model.Device{}, false
	}
	for _, d := range devs {
		if d.ID == id {
			return d, true
		}
	}
	return model.Device{}, false
}

func (h *handler) ruleNewRender(w http.ResponseWriter, r *http.Request, status int, o newRuleOpts) {
	v := h.view(r, "New rule", "policy", nil)
	v.Crumbs = []ui.Crumb{{Label: "Devices", Path: "/devices"}, {Label: "Policy", Path: policyPath}, {Label: "New rule"}}
	devs, err := h.d.Inventory.List(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	st, err := h.d.PolicyRules.State(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	saved := st.Specs
	p := newRuleView{Base: v.Base, CSRF: v.CSRF, Q: o.Q, Command: o.Command, CommandErr: o.CommandErr,
		CopyCommand: o.CopyCommand, CopyErr: o.CopyErr, Banner: o.Banner}
	needle := strings.ToLower(o.Q)
	byGroup := map[string][]tplCard{}
	for _, t := range policyTemplates(h.brokerHost()) {
		kv := t.kv()
		if needle != "" && !strings.Contains(strings.ToLower(t.Title+" "+kv+" "+t.Hint), needle) {
			continue
		}
		already := slices.ContainsFunc(saved, func(s sysconfig.RuleSpec) bool {
			return strings.EqualFold(s.Command, t.Spec.Command) && sameScope(s, t.Spec)
		})
		byGroup[t.Group] = append(byGroup[t.Group], tplCard{ID: t.ID, Title: t.Title, KV: kv, Hint: t.Hint, Already: already})
	}
	for _, g := range templateGroups {
		if cards := byGroup[g]; len(cards) > 0 {
			p.Groups = append(p.Groups, tplGroup{Name: g, Cards: cards})
		}
	}
	p.NoMatch = len(p.Groups) == 0
	// every device, online first, then by name
	online := func(d model.Device) bool {
		return h.d.Health != nil && h.d.Health.Get(d.ID).State == model.HealthOnline
	}
	slices.SortStableFunc(devs, func(a, b model.Device) int {
		if oa, ob := online(a), online(b); oa != ob {
			if oa {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	for _, d := range devs {
		p.Devices = append(p.Devices, copyDevice{ID: int64(d.ID), Name: d.Name, Sel: int64(d.ID) == o.CopyDevice})
	}
	h.show(w, r, status, "policynewrule", v, newRulePage(p))
}

// liveCheck is POST /policy/live: the fleet's live check. A rule page asks for it with its id
// (rule=<id>) and comes back to that page; without a rule the policy page shows the result.
func (h *handler) liveCheck(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	id := r.PostForm.Get("rule")
	if id == "" {
		if r.PostForm.Get("back") == "deviations" {
			h.devRender(w, r, http.StatusOK, true)
			return
		}
		if r.PostForm.Get("back") == "history" {
			h.historyRender(w, r, true)
			return
		}
		h.policyRender(w, r, http.StatusOK, policyOpts{live: true})
		return
	}
	if !policy.RuleIDRe.MatchString(id) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	done := "checked"
	switch rep, err := h.d.Policy.Report(r.Context(), actor(r), true); {
	case err != nil:
		h.log.WarnContext(r.Context(), "policy live check failed", "err", err)
		done = "checkfailed"
	case rep.LiveThrottled:
		done = "throttled" // the check ran a moment ago: these are its results
	}
	redirect(w, r, rulesPath+"/"+id+"?ok="+done)
}
