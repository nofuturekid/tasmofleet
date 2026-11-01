package web

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// ValueSource says where a checked value came from: "live" (read from the
// device or announced over MQTT) or "backup", and when.
type ValueSource struct {
	Kind string
	At   time.Time
}

// PolicyReport is what the policy page shows. Provenance maps a device and a
// lower-case command to the source of its value; a missing entry means no
// value at all. NotLive holds the lower-case commands that cannot be read
// without side effects.
type PolicyReport struct {
	Rules      []policy.Rule
	Devices    []model.Device
	Findings   []policy.Finding
	Provenance map[model.DeviceID]map[string]ValueSource
	NotLive    map[string]bool
	// Values holds, per device and lower-case command, the value each finding
	// and outcome was judged from (live preferred, else the newest backup).
	Values map[model.DeviceID]map[string]string
	// Outcomes is the one evaluation of Rules over Devices (policy.Outcomes);
	// Findings are its Deviate and Unknown entries.
	Outcomes []policy.Outcome
	// LiveThrottled is true when the live check was rate limited and
	// answered from recent reads only.
	LiveThrottled bool
}

// PolicyService builds the report from live values (read in the background,
// announced over MQTT, or read now when live is true) with backups as the
// fallback for settings that have no live value.
type PolicyService interface {
	Report(ctx context.Context, actor string, live bool) (PolicyReport, error)
	// Evaluate judges the given rules (a draft, not the saved ones) from cached
	// live values and backups. It never talks to a device.
	Evaluate(ctx context.Context, rules []policy.Rule) (PolicyReport, error)
	// ReadValue reads one setting of one device to propose it as an expected
	// value. A command that must not be sent is refused first, with no device
	// contact, as a *ValueRefusedError. Nothing is cached or stored.
	ReadValue(ctx context.Context, actor string, id model.DeviceID, command string) (value string, src ValueSource, err error)
}

// ValueRefusedError says why ReadValue will not read or offer a value. The
// text is safe to show: it never holds a device value.
type ValueRefusedError struct{ Reason string }

func (e *ValueRefusedError) Error() string { return e.Reason }

// policyPath is the policy overview: the fleet tiles, the tabs and the rule table. Each rule has
// its own page under rulesPath; the rule changes (reorder, enable, delete) are form
// routes there and come back here.
const policyPath = "/policy"

// policyFlashes are the confirmations after a change that came back to the page (the
// prototype's toasts).
var policyFlashes = map[string]string{
	"saved":    "Rule saved",
	"deleted":  "Rule deleted",
	"enabled":  "Rule enabled",
	"disabled": "Rule disabled · it no longer flags devices",
	"reverted": "Reverted: the old rules are saved as a new version",
	"imported": "Rules imported as a new version",
	"paused":   "Live check paused",
	"resumed":  "Live check resumed",
}

func (h *handler) registerPolicy(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), op func(http.Handler) http.Handler) {
	if h.d.Policy == nil {
		return
	}
	route("GET "+policyPath, op, func(w http.ResponseWriter, r *http.Request) {
		h.policyRender(w, r, http.StatusOK, policyOpts{})
	})
	route("POST "+policyPath+"/live", op, h.liveCheck)
}

// policyOpts says what the overview shows beyond the saved state: a banner for a refused list
// change, a live check just run.
type policyOpts struct {
	live   bool
	banner string
}

// liveLine is the live-check tile: idle, paused or failed.
type liveLine struct {
	State string // "idle", "paused", "failed"
	Value string // the big word: "Every 1 h", "Paused", "Failed"
	Tone  ui.Tone
	Sub   string // idle and failed: the line under it
	// Paused: "Live check paused by <PausedBy> at <PausedAt>[ · deviations shown are from <Shown>]".
	// The times are components so that time.js shows them in the browser's zone.
	PausedBy        string
	PausedAt, Shown time.Time
	CanPause        bool // Pause / Resume are shown (admin, and the pause service exists)
}

// ruleRow is one rule of the table, in stored order (the order is the precedence).
type ruleRow struct {
	ID       string
	Name     string
	Setting  string // "TelePeriod = 300"
	Applies  string // "ESP8266 · 18", "All devices · 27"
	Required bool
	Severity string // Required / Advisory
	Disabled bool
	Saved    bool // the rule the editor just saved (?ok=saved&rule=<id>): its row is marked
	First    bool
	Last     bool
	// Counted: the numbers below are known (the report ran). A disabled rule is never counted.
	Counted                 bool
	Match, Deviate, Unknown int
	Applied                 int    // devices the rule is evaluated for
	Text                    string // "17 / 18 · 1 deviate · 2 ?"
	Delete                  string // the delete dialog's body
	// Invalid is why the stored rule no longer validates ("" when it does). Such a rule is not
	// evaluated: it is flagged on its row and left out of the tiles.
	Invalid string
}

// fixDialog is "Make compliant (N)…": the reviewed plan the dialog applies.
type fixDialog struct {
	Ready    int
	Commands int
	Changes  []ui.KVRow
	Fields   []ui.KVRow
	Advisory bool // some selected device also has Advisory deviations to choose from
	CRFull   int  // devices whose settings text area cannot take the plan's text values (#289)
}

// devAlert is the banner of a Required rule that devices deviate from.
type devAlert struct {
	Text    string
	Devices string // the device names, shown in mono
	Rules   string
}

type policyView struct {
	Head      policyHead
	Base      string
	CSRF      string
	Admin     bool
	Version   int64
	HasRules  bool
	Rows      []ruleRow
	Conflicts []string
	Banner    string
	Alert     *devAlert
	Fix       *fixDialog
	Templates []ruleTemplate
}

// policyRender writes the overview.
func (h *handler) policyRender(w http.ResponseWriter, r *http.Request, status int, o policyOpts) {
	v := h.view(r, "Policy", "policy", nil)
	p, err := h.policyData(r, v, o)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if f := policyFlashes[r.URL.Query().Get("ok")]; f != "" {
		v.Flash = f
	}
	if r.URL.Query().Get("ok") == "saved" {
		for i := range p.Rows {
			if p.Rows[i].ID == r.URL.Query().Get("rule") {
				p.Rows[i].Saved = true
				v.Flash = "Rule “" + p.Rows[i].Name + "” saved"
			}
		}
	}
	if o.live && p.Head.Line != nil && p.Head.Line.State != "failed" {
		v.Flash = "Live check finished · " + deviateText(p.Head.Fleet.Deviating)
	}
	h.show(w, r, status, "policy", v, policyPage(p))
}

// policyData builds the overview. A failed report (the live check could not run) still shows the
// rules: the live-check tile says what failed and offers "Retry now".
func (h *handler) policyData(r *http.Request, v view, o policyOpts) (policyView, error) {
	ctx := r.Context()
	p := policyView{Base: v.Base, CSRF: v.CSRF, Admin: v.IsAdmin, Banner: o.banner}
	rep, repErr := h.d.Policy.Report(ctx, actor(r), o.live)
	st, err := h.d.PolicyRules.State(ctx)
	if err != nil {
		return p, err
	}
	p.Version = st.VersionID
	p.HasRules = len(st.Specs) > 0
	editable := p.Admin

	// A stored rule that no longer validates (a legacy value the stricter checks refuse) is flagged
	// on its row and left out of everything that is counted: one bad rule must not make the fleet
	// look compliant, and must not hide the others.
	valid, invalid := st.Specs, map[string]string{}
	if p.HasRules {
		var bad []*sysconfig.RuleError
		_, valid, bad = h.d.PolicyRules.ValidRules(st.Specs)
		for _, re := range bad {
			if re.Index >= 0 && re.Index < len(st.Specs) {
				invalid[st.Specs[re.Index].ID] = re.Msg
			}
		}
	}

	// Conflicts between rules come from the preview (backups and recent reads; it reads no
	// device). A failed preview leaves them out, never the list.
	names := make([]string, len(st.Specs))
	for i, s := range st.Specs {
		names[i] = ruleName(s)
	}
	if p.HasRules {
		validNames := make([]string, len(valid))
		for i, s := range valid {
			validNames[i] = ruleName(s)
		}
		if pr, err := h.d.PolicyRules.Preview(ctx, valid); err == nil && len(valid) > 0 {
			for _, c := range pr.Conflicts {
				if c.Higher < len(validNames) && c.Lower < len(validNames) {
					p.Conflicts = append(p.Conflicts, conflictText(validNames[c.Higher], validNames[c.Lower], c))
				}
			}
		}
	}

	// The numbers per rule are read by id: the report's rule list is the saved one, but a slot is
	// not a rule.
	tally := map[string]policy.RuleTally{}
	if repErr == nil {
		for _, t := range policy.Tally(rep.Rules, rep.Outcomes) {
			tally[t.RuleID] = t
		}
	}
	for i, s := range st.Specs {
		row := ruleRow{
			ID: s.ID, Name: names[i], Setting: s.Command + " = " + s.Want, Applies: appliesText(s),
			Required: s.Severity != policy.SeverityInfo, Severity: policy.SeverityLabel(s.Severity),
			Disabled: s.Disabled, First: i == 0, Last: i == len(st.Specs)-1,
		}
		row.Invalid = invalid[s.ID]
		if t, ok := tally[s.ID]; ok && !s.Disabled && row.Invalid == "" {
			row.Counted, row.Match, row.Deviate, row.Unknown, row.Applied = true, t.Match, t.Deviate, t.Unknown, t.Applies()
			row.Applies += " · " + strconv.Itoa(t.Applies())
			row.Text = complianceText(t)
		}
		row.Delete = "It currently judges " + plural(row.Applied, "device") + ". They stop being checked for " + s.Command +
			". The change is saved as a new version you can revert to."
		if s.Disabled {
			row.Delete = "It is disabled and judges no device. The change is saved as a new version you can revert to."
		}
		p.Rows = append(p.Rows, row)
	}

	p.Head = h.policyHead(v, valid, len(invalid), rep, repErr)
	p.Head.HasVersions = len(st.History) > 0
	p.Head.Show = p.HasRules || p.Head.HasVersions
	p.Head.Tabs = h.policyTabs(v.Base, "rules", len(st.Specs), p.Head.Fleet.Deviating, 0)
	if repErr == nil {
		p.Alert = requiredAlert(rep, st.Specs)
		if p.Admin && h.d.PolicyFix != nil {
			p.Fix = h.fixDialogOf(ctx, rep)
		}
	}
	if !p.HasRules && editable {
		// The empty state offers the ready-made templates (a blank want, such as a server, a place or a
		// URL, is typed on the draft page of New rule, which lists every template); three are shown.
		p.Templates = slices.DeleteFunc(policyTemplates(h.brokerHost()), func(t ruleTemplate) bool { return t.Spec.Want == "" })
		if len(p.Templates) > 3 {
			p.Templates = p.Templates[:3]
		}
	}
	return p, nil
}

// complianceText is "17 / 18", with "· 1 deviate · 2 ?" when some devices deviate or cannot be
// judged.
func complianceText(t policy.RuleTally) string {
	s := strconv.Itoa(t.Match) + " / " + strconv.Itoa(t.Applies())
	if t.Deviate > 0 {
		s += " · " + strconv.Itoa(t.Deviate) + " deviate"
	}
	if t.Unknown > 0 {
		s += " · " + strconv.Itoa(t.Unknown) + " ?"
	}
	return s
}

// requiredAlert names the devices that deviate from a Required rule, and those rules. nil when
// none does. The names are cut to three each: the Deviating devices tab lists the rest.
func requiredAlert(rep PolicyReport, specs []sysconfig.RuleSpec) *devAlert {
	devNames := map[model.DeviceID]string{}
	for _, d := range rep.Devices {
		devNames[d.ID] = d.Name
	}
	var devs, rules []string
	seenDev := map[model.DeviceID]bool{}
	seenRule := map[int]bool{}
	for _, o := range rep.Outcomes {
		if o.Kind != policy.OutcomeDeviate || o.RuleIndex >= len(rep.Rules) {
			continue
		}
		r := rep.Rules[o.RuleIndex]
		if r.Severity == policy.SeverityInfo {
			continue
		}
		if !seenDev[o.DeviceID] {
			seenDev[o.DeviceID] = true
			devs = append(devs, devNames[o.DeviceID])
		}
		if !seenRule[o.RuleIndex] {
			seenRule[o.RuleIndex] = true
			name := r.Name
			if i := slices.IndexFunc(specs, func(s sysconfig.RuleSpec) bool { return s.ID == r.ID }); i >= 0 {
				name = ruleName(specs[i])
			}
			rules = append(rules, name)
		}
	}
	if len(devs) == 0 {
		return nil
	}
	a := &devAlert{Devices: nameList(devs), Rules: nameList(rules)}
	if len(devs) == 1 {
		a.Text = "1 device deviates from a required rule: "
	} else {
		a.Text = strconv.Itoa(len(devs)) + " devices deviate from a required rule: "
	}
	return a
}

// nameList joins up to three names and says how many more there are.
func nameList(names []string) string {
	if len(names) <= 3 {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:3], ", ") + " and " + strconv.Itoa(len(names)-3) + " more"
}

// ruleName is the title of a rule: its name, else the setting's label.
func ruleName(s sysconfig.RuleSpec) string {
	if s.Name != "" {
		return s.Name
	}
	l, _ := settingLabel(s.Command)
	return l
}

// appliesText says which devices a rule covers, in the prototype's words.
func appliesText(s sysconfig.RuleSpec) string {
	var parts []string
	if len(s.Families) > 0 {
		parts = append(parts, "Chip family: "+strings.Join(policy.ScopeLabels(s.Families), ", "))
	}
	if len(s.Tags) > 0 {
		parts = append(parts, "Tag: "+strings.Join(s.Tags, ", "))
	}
	switch len(s.Devices) {
	case 0:
	case 1:
		parts = append(parts, "Device: "+s.Devices[0])
	default:
		parts = append(parts, strconv.Itoa(len(s.Devices))+" named devices")
	}
	if len(parts) == 0 {
		return "All devices"
	}
	return strings.Join(parts, " · ")
}

// conflictText is one line of the conflicts banner.
func conflictText(higher, lower string, c policy.Conflict) string {
	if c.SameWant {
		return "“" + higher + "” and “" + lower + "” both expect the same " + c.Command + ". “" + lower +
			"” is redundant on " + plural(c.Devices, "device") + " they share."
	}
	return "“" + higher + "” and “" + lower + "” both set " + c.Command + ". “" + higher +
		"” is higher in the list, so it wins for " + plural(c.Devices, "device") + " they share. Drag to change the order."
}

// deviateText is "1 device deviates", "2 devices deviate".
func deviateText(n int) string {
	if n == 1 {
		return "1 device deviates"
	}
	return plural(n, "device") + " deviate"
}

// advisory says whether a finding comes from an Advisory rule (a hint: in Make compliant only when ticked by hand).
func advisory(f policy.Finding) bool {
	return f.Severity == policy.SeverityInfo || f.Rule.Severity == policy.SeverityInfo
}

// liveLine says what the live check does: paused (by whom, since when), failed (why), or
// running with its interval and the time of the newest live value.
func (h *handler) liveLine(rep PolicyReport, repErr error, admin bool) *liveLine {
	l := &liveLine{State: "idle", Tone: ui.ToneOK, CanPause: admin && h.d.PolicyLivePause != nil}
	if repErr != nil {
		l.State, l.Value, l.Tone = "failed", "Failed", ui.ToneDanger
		l.Sub = "Live check failed: " + sentence(repErr.Error()) + " No deviations can be shown until it runs again."
		return l
	}
	last := lastLiveRead(rep)
	now := h.d.Clock.Now()
	if h.d.PolicyLivePause != nil {
		if s := h.d.PolicyLivePause.State(); s.Paused {
			l.State, l.Value, l.Tone = "paused", "Paused", ui.ToneNeutral
			l.PausedBy, l.PausedAt, l.Shown = h.pr.Text(s.By), s.At, last
			return l
		}
	}
	every := ""
	switch iv := h.liveInterval(); iv {
	case "":
		l.Value = "Running"
	case "0", "0s":
		l.Value = "Manual"
		every = "manual"
	default:
		l.Value = "Running"
		every = intervalWord(iv)
	}
	parts := []string{}
	if every != "" {
		parts = append(parts, every)
	}
	switch {
	case last.IsZero():
		parts = append(parts, "no device read yet")
	case now.Sub(last) < time.Minute:
		parts = append(parts, "just now")
	default:
		parts = append(parts, shortDuration(now.Sub(last))+" ago")
	}
	if rep.LiveThrottled {
		parts = append(parts, "the check ran recently, these are its results")
	}
	l.Sub = strings.Join(parts, " · ")
	return l
}

// intervalWord says the check's interval the way the tile reads it: "hourly", "daily", else "every 10 min".
func intervalWord(iv string) string {
	switch iv {
	case "1 h":
		return "hourly"
	case "24 h":
		return "daily"
	}
	return "every " + iv
}

// liveInterval is the configured interval of the automatic live check ("1h", "10m"), "" when
// the system settings are not available.
func (h *handler) liveInterval() string {
	if h.d.SysConfig == nil {
		return ""
	}
	for _, f := range h.d.SysConfig.State() {
		if f.Key == "POLICY_LIVE_INTERVAL" {
			d, err := time.ParseDuration(f.Value)
			if err != nil {
				return f.Value
			}
			return shortDuration(d)
		}
	}
	return ""
}

// shortDuration writes an interval the way the status line reads it: "10 min", "1 h", "1 h 30 min".
func shortDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "0"
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + " s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + " min"
	case d%time.Hour == 0:
		return strconv.Itoa(int(d.Hours())) + " h"
	}
	return strconv.Itoa(int(d.Hours())) + " h " + strconv.Itoa(int(d.Minutes())%60) + " min"
}

// lastLiveRead is the time of the newest value read live.
func lastLiveRead(rep PolicyReport) time.Time {
	var last time.Time
	for _, m := range rep.Provenance {
		for _, s := range m {
			if s.Kind == "live" && s.At.After(last) {
				last = s.At
			}
		}
	}
	return last
}

// sourceNote is the per-value label: "checked 5m ago (live)" or
// "from backup of 2026-10-01".
func sourceNote(s ValueSource, now time.Time) string {
	if s.Kind == "backup" {
		return "from backup of " + s.At.Local().Format("2006-01-02")
	}
	return "checked " + agoWords(now, s.At) + " (live)"
}

// scopeChipText is a scope choice as people read it.
func scopeChipText(c chip) string {
	if c.Label != "" {
		return c.Label
	}
	return c.Value
}

// changeText is one difference between two versions: what happened to the rule, and how.
func changeText(c sysconfig.RuleChange) string {
	kind := map[string]string{"added": "Added", "removed": "Removed", "changed": "Changed"}[c.Kind]
	if kind == "" {
		kind = c.Kind
	}
	if c.Detail != "" {
		return kind + ": " + c.Detail
	}
	return kind
}

// revertRows are the differences as the revert dialog lists them.
func revertRows(d []sysconfig.RuleChange) []ui.KVRow {
	out := make([]ui.KVRow, 0, len(d))
	for _, c := range d {
		out = append(out, ui.KVRow{Key: c.Rule, Value: changeText(c)})
	}
	return out
}

// countTone is the colour of an import preview's header count.
func countTone(kind string) ui.Tone {
	switch kind {
	case "New":
		return ui.ToneOK
	case "Remove":
		return ui.ToneDanger
	}
	return ui.ToneAccent
}

// fixDialogBody is the text of the Make compliant dialog.
func fixDialogBody(f *fixDialog) string {
	s := "Required deviations are included"
	if f.Advisory {
		s += "; Advisory ones only when you choose them"
	}
	s += ". These commands run as one transaction only when you confirm. Values known only from a backup, or not known at all, are left alone."
	if f.CRFull > 0 {
		s += " " + plural(f.CRFull, "device") + " may not have room in the settings text area for these text values; the preview names them."
	}
	return s
}

// fixDialogAlt is the dialog's second button: the preview with a box per Advisory deviation. "" when
// no selected device has one.
func fixDialogAlt(f *fixDialog) string {
	if f.Advisory {
		return "Choose Advisory rows…"
	}
	return ""
}
