package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/audit"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/settings"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// PolicyRulesService is the part of sysconfig.Rules the editor uses. Optional:
// the editor exists only when it is set.
type PolicyRulesService interface {
	State(ctx context.Context) (sysconfig.RulesState, error)
	Preview(ctx context.Context, specs []sysconfig.RuleSpec) (sysconfig.PreviewResult, error)
	Save(ctx context.Context, actor, note string, specs []sysconfig.RuleSpec, rec audit.Recorder) (int64, error)
	Revert(ctx context.Context, actor string, id int64, rec audit.Recorder) (int64, error)
	Export(ctx context.Context) ([]byte, error)
	ParseImport(data []byte) ([]sysconfig.RuleSpec, error)
	VersionDiff(ctx context.Context, from, to int64) ([]sysconfig.RuleChange, error)
	Reorder(ctx context.Context, actor string, base int64, order []int, rec audit.Recorder) (int64, error)
	Duplicate(ctx context.Context, actor string, base int64, idx int, rec audit.Recorder) (int64, int, error)
	SetEnabled(ctx context.Context, actor string, base int64, idx int, enabled bool, rec audit.Recorder) (int64, error)
}

// rulesPath is the prefix of the rule changes (form routes). The rules have no page of their
// own since design v2 (N3): every change comes back to the merged policy page.
const rulesPath = "/policy/rules"

// policyIOPath is import / export (design v2 "policyio").
const policyIOPath = "/policy/io"

func (h *handler) registerPolicyRules(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), op, adm func(http.Handler) http.Handler) {
	if h.d.PolicyRules == nil {
		return
	}
	route("GET "+policyIOPath, op, h.policyIO)
	route("GET "+policyIOPath+"/export", op, h.rulesExport)
	route("POST "+policyIOPath+"/import", adm, h.rulesImport)
	route("GET "+rulesPath+"/value-control", adm, h.rulesValueControl)
	route("POST "+rulesPath+"/preview", adm, h.rulesPreview)
	route("POST "+rulesPath+"/save", adm, h.rulesSave)
	route("GET "+rulesPath+"/delete", adm, h.rulesDeleteConfirm)
	route("POST "+rulesPath+"/delete", adm, h.rulesDelete)
	route("POST "+rulesPath+"/revert/{id}", adm, h.rulesRevert)
	h.registerPolicyRuleOps(route, adm)
}

type settingChoice struct{ Key, Label string }

type settingGroup struct {
	Name    string
	Choices []settingChoice
}

type chip struct {
	Value string
	Label string // shown instead of Value when set
	On    bool
}

// valueControl is the input for the expected value; its kind follows the
// setting: select (enum), toggle (SetOption), number (with range) or text.
type valueControl struct {
	Kind     string // none, select, toggle, number, text
	Want     string
	Options  []settings.Option
	Min, Max int
	HasRange bool
	Hint     string
	Invalid  bool // the expected value is the failing field of the last save
}

// rulePreview is the live "what would this rule flag" fragment.
type rulePreview struct {
	Hint, Err        string
	OK               bool
	Flagged, Unknown int
	Devices          int
}

// ruleEditor is the editor panel: a new rule or one saved rule.
type ruleEditor struct {
	Idx      string // "new" or the list position
	Title    string
	Spec     sysconfig.RuleSpec
	Version  int64
	Err      string // sentence for the failing field (ErrField), or for the whole rule when ErrField is empty
	ErrField string // "command", "want" or "change_note"; empty: no single field
	Groups   []settingGroup
	Control  valueControl
	Families []chip
	Tags     []chip
	Devices  []chip
	Except   string
	Preview  rulePreview
	Enabled  bool // the Enabled switch
}

// rulesPage carries the editor while it is built.
type rulesPage struct {
	Current int64
	Editor  *ruleEditor
}

var setOptionRe = regexp.MustCompile(`^SetOption[0-9]{1,3}$`)

// settingLabel returns the name shown for a command and, when the label does
// not already contain it, the command itself.
func settingLabel(cmd string) (label, key string) {
	l := settings.Label(cmd)
	if l == cmd || strings.HasPrefix(l, cmd) {
		return l, ""
	}
	return l, cmd
}

func choiceLabel(cmd string) string {
	l, k := settingLabel(cmd)
	if k != "" {
		return l + " (" + k + ")"
	}
	return l
}

// settingGroups lists the curated settings offered as suggestions for the
// command field, grouped like the settings editor. The field takes any
// other command too; the suggestions are only the ones with a label and a
// checked value.
func settingGroups() []settingGroup {
	var out []settingGroup
	idx := map[string]int{}
	for _, f := range settings.Fields {
		if f.Secret || !settings.Queryable(f.Key) {
			continue
		}
		i, ok := idx[f.Group]
		if !ok {
			i = len(out)
			idx[f.Group] = i
			out = append(out, settingGroup{Name: f.Group})
		}
		out[i].Choices = append(out[i].Choices, settingChoice{Key: f.Key, Label: choiceLabel(f.Key)})
	}
	return out
}

// controlFor picks the value control of a setting. keepInvalid keeps a
// value the setting would refuse (to show a draft that failed); otherwise it
// is dropped, which is what changing the setting needs.
func controlFor(command, want string, keepInvalid bool) valueControl {
	vc := valueControl{Kind: "text", Want: want}
	f, ok := settings.Lookup(command)
	switch {
	case command == "":
		return valueControl{Kind: "none"}
	case ok && !f.Secret:
		vc.Hint = f.Help
		switch f.Kind {
		case settings.KindSelect:
			vc.Kind, vc.Options = "select", f.Options
		case settings.KindToggle:
			vc.Kind = "toggle"
		case settings.KindNumber:
			vc.Kind = "number"
			vc.Min, vc.Max, vc.HasRange = f.Min, f.Max, f.HasRange
		case settings.KindText, settings.KindPassword:
			// a plain text input (secrets are excluded above)
		}
		if !keepInvalid && want != "" && f.Validate != nil && f.Validate(want) != nil {
			vc.Want = ""
		}
	case settings.SetOptionParam(command):
		vc.Kind = "number"
		vc.Min, vc.Max, vc.HasRange = 0, settings.SetOptionParamMax, true
		vc.Hint = command + " is a number, not an on/off switch (0 to 255)."
		if !keepInvalid && want != "" && settings.ValidateSetOptionParam(want) != nil {
			vc.Want = ""
		}
	case setOptionRe.MatchString(command) && (want == "" || want == "0" || want == "1"):
		vc.Kind = "toggle"
	case settings.Generic(command):
		vc.Hint = "Compared with exactly what the device reports for " + command + ". ON and OFF count as 1 and 0."
	}
	return vc
}

func wantText(cmd, want string) string {
	vc := controlFor(cmd, want, true)
	switch vc.Kind {
	case "select":
		for _, o := range vc.Options {
			if o.Value == want {
				return o.Label
			}
		}
	case "toggle":
		switch want {
		case "1":
			return "on (1)"
		case "0":
			return "off (0)"
		}
	}
	return want
}

func chips(known []string, on []string) []chip {
	all := slices.Clone(known)
	for _, v := range on {
		if !slices.Contains(all, v) {
			all = append(all, v)
		}
	}
	sort.Strings(all)
	out := make([]chip, len(all))
	for i, v := range all {
		out[i] = chip{Value: v, On: slices.Contains(on, v)}
	}
	return out
}

// buildEditor fills the editor panel. Tags and device names come from the
// inventory; values of the rule that are not in it stay selectable.
func (h *handler) buildEditor(r *http.Request, p *rulesPage, idx string, spec sysconfig.RuleSpec, saved []sysconfig.RuleSpec) error {
	devs, err := h.d.Inventory.List(r.Context())
	if err != nil {
		return err
	}
	var tags, names []string
	for _, d := range devs {
		names = append(names, d.Name)
		for _, t := range d.Tags {
			if !slices.Contains(tags, t) {
				tags = append(tags, t)
			}
		}
	}
	fam := make([]chip, 0, len(policy.FamilyNames()))
	for _, f := range policy.FamilyNames() {
		fam = append(fam, chip{Value: f, Label: policy.FamilyLabel(f), On: slices.Contains(spec.Families, f)})
	}
	title := "New rule"
	if idx != "new" {
		title = "Edit rule"
	}
	d := ruleDraft{Idx: idx, Spec: spec}
	p.Editor = &ruleEditor{
		Idx: idx, Title: title, Spec: spec, Version: p.Current,
		Groups:   settingGroups(),
		Control:  controlFor(spec.Command, spec.Want, true),
		Families: fam, Tags: chips(tags, spec.Tags), Devices: chips(names, spec.Devices),
		Except:  strings.Join(spec.Except, ", "),
		Preview: h.previewFor(r.Context(), saved, d),
	}
	return nil
}

var (
	idxRe     = regexp.MustCompile(`^(new|[0-9]{1,3})$`)
	versionRe = regexp.MustCompile(`^[0-9]{1,18}$`)
)

// strictForm parses a POST body and refuses every key outside single and
// multi, and a repeated key that is not in multi.
func strictForm(w http.ResponseWriter, r *http.Request, single, multi []string) (url.Values, bool) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, false
	}
	for k, vs := range r.PostForm {
		switch {
		case slices.Contains(multi, k) && len(vs) <= 200:
		case slices.Contains(single, k) && len(vs) == 1:
		default:
			http.Error(w, "unknown form field", http.StatusBadRequest)
			return nil, false
		}
	}
	return r.PostForm, true
}

// ruleDraft is what the editor form carries.
type ruleDraft struct {
	Idx        string // "new" or a list position
	Version    int64  // the version the editor was opened on
	Spec       sysconfig.RuleSpec
	ChangeNote string
	State      string // "enabled", "disabled" or "" (keep the rule's state)
}

func uniq(list []string) []string {
	var out []string
	for _, e := range trimmed(list) {
		if !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	return out
}

var draftSingle = []string{"csrf", "idx", "version", "name", "command", "want", "severity", "tags_extra", "except", "note", "change_note"}

// draftForm reads the editor form (save and preview). The Enabled switch is a checkbox after a
// hidden "state=disabled": a ticked box sends both and the last one wins.
func (h *handler) draftForm(w http.ResponseWriter, r *http.Request) (ruleDraft, bool) {
	f, ok := strictForm(w, r, draftSingle, []string{"family", "tag", "device", "state"})
	if !ok {
		return ruleDraft{}, false
	}
	d := ruleDraft{Idx: f.Get("idx"), ChangeNote: strings.TrimSpace(f.Get("change_note"))}
	if d.Idx == "" {
		d.Idx = "new"
	}
	if v := f.Get("version"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || !versionRe.MatchString(v) {
			http.Error(w, "bad request", http.StatusBadRequest)
			return ruleDraft{}, false
		}
		d.Version = n
	}
	if !idxRe.MatchString(d.Idx) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return ruleDraft{}, false
	}
	states := f["state"]
	for _, st := range states {
		if st != "enabled" && st != "disabled" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return ruleDraft{}, false
		}
	}
	if len(states) > 2 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return ruleDraft{}, false
	}
	if len(states) > 0 {
		d.State = states[len(states)-1]
	}
	d.Spec = sysconfig.RuleSpec{
		Name: strings.TrimSpace(f.Get("name")), Command: strings.TrimSpace(f.Get("command")), Want: strings.TrimSpace(f.Get("want")),
		Severity: strings.TrimSpace(f.Get("severity")), Note: strings.TrimSpace(f.Get("note")),
		Families: uniq(f["family"]),
		Tags:     uniq(append(slices.Clone(f["tag"]), splitList(f.Get("tags_extra"))...)),
		Devices:  uniq(f["device"]),
		Except:   uniq(splitList(f.Get("except"))),
	}
	return d, true
}

func splitList(s string) []string {
	return strings.FieldsFunc(s, func(c rune) bool { return c == ',' || c == '\n' || c == '\r' })
}

func trimmed(list []string) []string {
	var out []string
	for _, e := range list {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// draftDisabled is the Enabled state a saved draft gets: the form's "state"
// when it sends one, else the state the rule had (a form without the switch
// must not turn a disabled rule back on), and enabled for a new rule.
func draftDisabled(d ruleDraft, saved []sysconfig.RuleSpec) bool {
	switch d.State {
	case "enabled":
		return false
	case "disabled":
		return true
	}
	if n, err := strconv.Atoi(d.Idx); err == nil && n >= 0 && n < len(saved) {
		return saved[n].Disabled
	}
	return false
}

// place puts spec into a copy of the saved list: appended for "new",
// otherwise replacing the rule at that position. pos is its position.
func place(saved []sysconfig.RuleSpec, idx string, spec sysconfig.RuleSpec) (list []sysconfig.RuleSpec, pos int, ok bool) {
	list = slices.Clone(saved)
	if idx == "new" {
		return append(list, spec), len(list), true
	}
	n, err := strconv.Atoi(idx)
	if err != nil || n < 0 || n >= len(list) {
		return nil, 0, false
	}
	list[n] = spec
	return list, n, true
}

// previewFor says what the draft would flag. It uses Rules.Preview, which
// stores nothing and reads no device.
func (h *handler) previewFor(ctx context.Context, saved []sysconfig.RuleSpec, d ruleDraft) rulePreview {
	if d.Spec.Command == "" || d.Spec.Want == "" {
		return rulePreview{Hint: "Enter a command and an expected value to see which devices the rule would flag."}
	}
	// The editor previews the rule as if enabled, at its place in the list:
	// what it would flag is the point, also for a rule that is still disabled.
	spec := d.Spec
	spec.Disabled = false
	list, pos, ok := place(saved, d.Idx, spec)
	if !ok {
		return rulePreview{Err: "This rule no longer exists. Reload the page."}
	}
	res, err := h.d.PolicyRules.Preview(ctx, list)
	var re *sysconfig.RuleError
	switch {
	case errors.As(err, &re) && re.Index == pos:
		if field, text := ruleProblem(re.Msg); field == "command" {
			return rulePreview{Err: text} // the reason, not the validator's sentence
		}
		return rulePreview{Err: re.Msg}
	case errors.As(err, &re):
		return rulePreview{Err: "Another saved rule is invalid: " + re.Error()}
	case err != nil:
		h.log.ErrorContext(ctx, "policy rules preview failed", "err", err)
		return rulePreview{Err: "The preview is not available."}
	case pos >= len(res.Rules):
		return rulePreview{Err: "The preview is not available."}
	}
	return rulePreview{OK: true, Flagged: res.Rules[pos].Flagged, Unknown: res.Rules[pos].Unknown, Devices: res.Devices}
}

func (h *handler) rulesPreview(w http.ResponseWriter, r *http.Request) {
	d, ok := h.draftForm(w, r)
	if !ok {
		return
	}
	st, err := h.d.PolicyRules.State(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.component(w, r, http.StatusOK, rulePreviewLine(h.previewFor(r.Context(), st.Specs, d)))
}

// rulesValueControl renders the expected-value input for the chosen setting.
func (h *handler) rulesValueControl(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for k, vs := range q {
		if (k != "command" && k != "want") || len(vs) != 1 {
			http.Error(w, "unknown form field", http.StatusBadRequest)
			return
		}
	}
	command := strings.TrimSpace(q.Get("command"))
	h.component(w, r, http.StatusOK, ruleValueSwap(controlFor(command, strings.TrimSpace(q.Get("want")), false)))
}

// staleText is the banner of a change made on an outdated version of the rules.
const staleText = "Nothing was saved. The rules were changed by someone else since you opened this page. Check the list and make your change again."

// rulesStale answers a request made on an outdated version of the rules. The
// draft is not shown again: the position it refers to may now be another rule.
func (h *handler) rulesStale(w http.ResponseWriter, r *http.Request) {
	h.policyRender(w, r, http.StatusConflict, policyOpts{banner: staleText})
}

func (h *handler) rulesSave(w http.ResponseWriter, r *http.Request) {
	d, ok := h.draftForm(w, r)
	if !ok {
		return
	}
	st, err := h.d.PolicyRules.State(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if d.Version != st.VersionID {
		h.rulesStale(w, r)
		return
	}
	d.Spec.Disabled = draftDisabled(d, st.Specs)
	list, pos, ok := place(st.Specs, d.Idx, d.Spec)
	if !ok {
		h.notFound(w, r)
		return
	}
	if field, msg := draftProblem(d.Spec); field != "" {
		h.rulesInvalid(w, r, d, field, msg, http.StatusUnprocessableEntity)
		return
	}
	_, err = h.d.PolicyRules.Save(r.Context(), actor(r), d.ChangeNote, list, h.d.Recorder)
	if err == nil {
		redirect(w, r, policyPath+"?ok=saved&edit="+strconv.Itoa(pos))
		return
	}
	var re *sysconfig.RuleError
	switch {
	case errors.As(err, &re) && (re.Index == pos || re.Index < 0):
		// The problem is in this rule: say it once, at the field.
		field, msg := ruleProblem(re.Msg)
		h.rulesInvalid(w, r, d, field, msg, http.StatusUnprocessableEntity)
	case errors.As(err, &re):
		h.policyRender(w, r, http.StatusUnprocessableEntity, policyOpts{draft: &d, banner: "Nothing was saved. " + re.Error()})
	case errors.Is(err, sysconfig.ErrNoChange):
		h.policyRender(w, r, http.StatusOK, policyOpts{draft: &d, banner: "Nothing changed: the rules are the same as the saved version."})
	default:
		h.fail(w, r, err)
	}
}

// draftProblem finds what the form left empty before anything is validated or
// saved, so the person reads "Enter a command." and not a validator's chain.
func draftProblem(s sysconfig.RuleSpec) (field, msg string) {
	switch {
	case strings.TrimSpace(s.Command) == "":
		return "command", "Enter a command."
	case strings.TrimSpace(s.Want) == "":
		return "want", "Enter the expected value."
	}
	return "", ""
}

// ruleProblem turns a validator message about the rule being edited into the
// input it belongs to and a sentence for the person. Messages it does not know
// stay as they are, without a field, in the editor's own banner.
func ruleProblem(msg string) (field, text string) {
	switch {
	case strings.Contains(msg, "invalid command"):
		return "command", "Enter a command."
	case strings.Contains(msg, "cannot be checked: "):
		_, reason, _ := strings.Cut(msg, "cannot be checked: ")
		return "command", upperFirst(reason) + "."
	case strings.Contains(msg, "cannot be checked"):
		return "command", "A policy rule cannot check this setting."
	case strings.Contains(msg, "invalid or empty want"):
		return "want", "Enter the expected value."
	case strings.Contains(msg, "the note must be"):
		return "change_note", "Use one line of at most 200 characters."
	}
	return "", "Nothing was saved. " + msg
}

// rulesInvalid answers a save whose draft is incomplete or refused: the editor opens again
// with the problem at its field and nothing is stored.
func (h *handler) rulesInvalid(w http.ResponseWriter, r *http.Request, d ruleDraft, field, msg string, status int) {
	h.policyRender(w, r, status, policyOpts{draft: &d, errField: field, errMsg: msg})
}

func (h *handler) rulesDelete(w http.ResponseWriter, r *http.Request) {
	f, ok := strictForm(w, r, []string{"csrf", "idx", "version"}, nil)
	if !ok {
		return
	}
	ver, e1 := strconv.ParseInt(f.Get("version"), 10, 64)
	n, e2 := strconv.Atoi(f.Get("idx"))
	if e1 != nil || e2 != nil || ver < 0 || n < 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	st, err := h.d.PolicyRules.State(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if ver != st.VersionID {
		h.rulesStale(w, r)
		return
	}
	if n >= len(st.Specs) {
		h.notFound(w, r)
		return
	}
	list := slices.Delete(slices.Clone(st.Specs), n, n+1)
	if _, err := h.d.PolicyRules.Save(r.Context(), actor(r), "", list, h.d.Recorder); err != nil {
		h.rulesFail(w, r, err)
		return
	}
	redirect(w, r, policyPath+"?ok=deleted")
}

// rulesFail shows a failed list-level change on the page.
func (h *handler) rulesFail(w http.ResponseWriter, r *http.Request, err error) {
	var re *sysconfig.RuleError
	switch {
	case errors.As(err, &re):
		h.policyRender(w, r, http.StatusUnprocessableEntity, policyOpts{banner: "Nothing was saved. " + re.Error()})
	case errors.Is(err, sysconfig.ErrNoChange):
		h.policyRender(w, r, http.StatusOK, policyOpts{banner: "Nothing changed: the rules are the same as the saved version."})
	default:
		h.fail(w, r, err)
	}
}

// rulesRevert stores the rules of an older version as a new version. Without JavaScript the
// history's button posts here directly (a revert is itself a version and can be undone).
func (h *handler) rulesRevert(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		h.notFound(w, r)
		return
	}
	if _, err := h.d.PolicyRules.Revert(r.Context(), actor(r), id, h.d.Recorder); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.notFound(w, r)
			return
		}
		h.rulesFail(w, r, err)
		return
	}
	redirect(w, r, policyPath+"?ok=reverted")
}

// ---- import / export (GET /policy/io) ----

// ioRow is one line of the import preview.
type ioRow struct {
	Kind   string // New, Update, Remove
	Tone   ui.Tone
	Rule   string
	Detail string
}

// policyIOView is the import / export page: the export as JSON, or the import form with the
// checked file's changes.
type policyIOView struct {
	Base, CSRF string
	Admin      bool
	Import     bool // the Import tab is open
	JSON       string
	Text       string // the pasted import
	Error      string
	Checked    bool
	Count      int
	Rows       []ioRow
	Counts     []ui.KVRow // "2 update", "1 new" for the preview's header
}

func (h *handler) policyIO(w http.ResponseWriter, r *http.Request) {
	tab := r.URL.Query().Get("tab")
	if tab != "" && tab != "import" && tab != "export" {
		h.notFound(w, r)
		return
	}
	h.policyIORender(w, r, http.StatusOK, policyIOView{Import: tab == "import"})
}

func (h *handler) policyIORender(w http.ResponseWriter, r *http.Request, status int, p policyIOView) {
	v := h.view(r, "Import / export rules", "policy", nil)
	v.Crumbs = []ui.Crumb{{Label: "Devices", Path: "/devices"}, {Label: "Policy", Path: policyPath}, {Label: "Import / export"}}
	p.Base, p.CSRF, p.Admin = v.Base, v.CSRF, v.IsAdmin
	if p.Import && !p.Admin {
		p.Import = false // viewers only export
	}
	if !p.Import {
		b, err := h.d.PolicyRules.Export(r.Context())
		if err != nil {
			h.fail(w, r, err)
			return
		}
		p.JSON = string(b)
	}
	h.show(w, r, status, "policyio", v, policyIOPage(p))
}

func (h *handler) rulesExport(w http.ResponseWriter, r *http.Request) {
	b, err := h.d.PolicyRules.Export(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="policy-rules.json"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b) // a failed write means the client went away
}

// rulesImport checks pasted POLICY_RULES JSON and shows how it differs from
// the saved rules; only the confirmed second step stores it as a new version.
func (h *handler) rulesImport(w http.ResponseWriter, r *http.Request) {
	f, ok := strictForm(w, r, []string{"csrf", "json", "confirm"}, nil)
	if !ok {
		return
	}
	data, err := io.ReadAll(io.LimitReader(strings.NewReader(f.Get("json")), 256<<10))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	p := policyIOView{Import: true, Text: string(data)}
	specs, err := h.d.PolicyRules.ParseImport(data)
	if err != nil {
		p.Error = "Nothing was imported. " + err.Error()
		h.policyIORender(w, r, http.StatusUnprocessableEntity, p)
		return
	}
	if f.Get("confirm") == "1" {
		_, err := h.d.PolicyRules.Save(r.Context(), actor(r), "import", specs, h.d.Recorder)
		var re *sysconfig.RuleError
		switch {
		case err == nil:
			redirect(w, r, policyPath+"?ok=imported")
		case errors.As(err, &re):
			p.Error = "Nothing was imported. " + re.Error()
			h.policyIORender(w, r, http.StatusUnprocessableEntity, p)
		case errors.Is(err, sysconfig.ErrNoChange):
			p.Error = "Nothing changed: the file holds the rules that are saved now."
			h.policyIORender(w, r, http.StatusOK, p)
		default:
			h.fail(w, r, err)
		}
		return
	}
	st, err := h.d.PolicyRules.State(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	p.Checked, p.Count = true, len(specs)
	p.Rows, p.Counts = ioRows(sysconfig.DiffSpecs(st.Specs, specs))
	h.policyIORender(w, r, http.StatusOK, p)
}

// ioRows turns the differences into the preview's rows and its header counts.
func ioRows(changes []sysconfig.RuleChange) ([]ioRow, []ui.KVRow) {
	n := map[string]int{}
	var rows []ioRow
	for _, c := range changes {
		row := ioRow{Rule: c.Rule, Detail: c.Detail}
		switch c.Kind {
		case "added":
			row.Kind, row.Tone = "New", ui.ToneOK
		case "removed":
			row.Kind, row.Tone = "Remove", ui.ToneDanger
		default:
			row.Kind, row.Tone = "Update", ui.ToneAccent
		}
		n[row.Kind]++
		rows = append(rows, row)
	}
	var counts []ui.KVRow
	for _, k := range []string{"Update", "New", "Remove"} {
		if n[k] > 0 {
			counts = append(counts, ui.KVRow{Key: k, Value: strconv.Itoa(n[k]) + " " + strings.ToLower(k)})
		}
	}
	return rows, counts
}
