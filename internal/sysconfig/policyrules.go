package sysconfig

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/nofuturekid/tasmofleet/internal/audit"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// RuleSpec is one fleet policy rule as the editor and the stored versions
// hold it. Its JSON form is exactly the POLICY_RULES format, so a stored
// version can be exported and used as a file.
type RuleSpec struct {
	// ID names the rule across versions and reorders. Empty on a submitted
	// spec means "new rule": a fresh id is assigned when it is stored. A
	// version stored before 0.19 has none; decoding gives it policy.LegacyID.
	ID       string   `json:"id,omitempty"`
	Name     string   `json:"name,omitempty"`
	Command  string   `json:"command"`
	Want     string   `json:"want"`
	Note     string   `json:"note,omitempty"`
	Severity string   `json:"severity,omitempty"`
	Families []string `json:"families,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Devices  []string `json:"devices,omitempty"`
	Except   []string `json:"except,omitempty"`
	// Disabled keeps the rule (and its versions) without evaluating it.
	Disabled bool `json:"disabled,omitempty"`
}

// Errors of the rules service.
var (
	ErrNoChange = errors.New("sysconfig: the rules are unchanged")
	// ErrStale: a list operation named a version that is no longer the newest;
	// its positions may now point at other rules, so nothing was stored.
	ErrStale = errors.New("sysconfig: the rules changed since they were read")
	// ErrNoSuchRule: a list operation named a position outside the list.
	ErrNoSuchRule = errors.New("sysconfig: no rule at that position")
)

// RuleError points at one rule of a submitted list.
type RuleError struct {
	Index int // -1: the list as a whole
	Msg   string
}

func (e *RuleError) Error() string {
	if e.Index < 0 {
		return "policy rules: " + e.Msg
	}
	return "policy rules: rule " + strconv.Itoa(e.Index+1) + ": " + e.Msg
}

// RulePreview is the effect of one rule on the fleet.
type RulePreview struct{ Flagged, Unknown int }

// PreviewResult says what a rule list would flag, from the newest backups and
// recent live reads (a preview never reads devices).
type PreviewResult struct {
	Devices int // devices in the inventory
	NoData  int // devices without any value to judge
	// Rules has one entry per rule, counted with precedence: a rule shadowed
	// by a higher one on a device is not counted there; a disabled rule is 0.
	Rules []RulePreview
	// Conflicts are the overlaps of enabled rules on the same setting.
	Conflicts []policy.Conflict
}

// RulesStore is the part of store.SystemSettingsStore the rules need.
type RulesStore interface {
	AddPolicyRuleVersion(ctx context.Context, v store.PolicyRuleVersion) (int64, error)
	CurrentPolicyRules(ctx context.Context) (store.PolicyRuleVersion, error)
	GetPolicyRuleVersion(ctx context.Context, id int64) (store.PolicyRuleVersion, error)
	ListPolicyRuleVersions(ctx context.Context, limit int) ([]store.PolicyRuleVersion, error)
	ListPolicyRuleVersionsBefore(ctx context.Context, beforeID int64, limit int) ([]store.PolicyRuleVersion, error)
}

// RulesOptions configures NewRules.
type RulesOptions struct {
	Store RulesStore
	Audit audit.Recorder
	// Queryable says whether a command may be queried from a device (no
	// secrets, no side effects); policy.ParseRules asks it.
	Queryable func(command string) bool
	// ValidateWant checks the expected value of a known setting. nil skips it.
	ValidateWant func(command, want string) error
	// Preview evaluates rules against the fleet. nil disables previews.
	Preview func(ctx context.Context, rules []policy.Rule) (PreviewResult, error)
	// OnChange receives the new active rules after every stored change (live
	// reload).
	OnChange func(rules []policy.Rule)
}

// Rules manages the versioned fleet policy rules.
type Rules struct {
	o  RulesOptions
	mu sync.Mutex // serialises saves so versions stay linear
}

// NewRules returns the rules service.
func NewRules(o RulesOptions) *Rules { return &Rules{o: o} }

// RulesState is what the editor shows.
type RulesState struct {
	Specs     []RuleSpec
	VersionID int64 // 0: nothing stored yet
	History   []store.PolicyRuleVersion
}

// decodeRaw reads a rule list as it is, ids included or not.
func decodeRaw(data []byte) ([]RuleSpec, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var specs []RuleSpec
	if err := dec.Decode(&specs); err != nil {
		return nil, fmt.Errorf("policy rules: %w", err)
	}
	return specs, nil
}

// decodeSpecs reads a stored version. Rules without an id (every version
// before 0.19) get policy.LegacyID of their position, the scheme POLICY_RULES
// files get too, so a link to a rule survives the upgrade. The stored row is
// not touched; the next save persists the ids.
func decodeSpecs(data []byte) ([]RuleSpec, error) {
	specs, err := decodeRaw(data)
	if err != nil {
		return nil, err
	}
	taken := make(map[string]bool, len(specs))
	for _, s := range specs {
		taken[s.ID] = true
	}
	for i := range specs {
		if specs[i].ID != "" {
			continue
		}
		id := policy.LegacyID(i)
		if taken[id] { // an explicit id elsewhere in the list already uses it
			id = newRuleID()
		}
		specs[i].ID, taken[id] = id, true
	}
	return specs, nil
}

// newRuleID returns "r" plus 10 random lowercase base32 characters, with at
// least one letter so it can never be mistaken for a legacy position id.
func newRuleID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	for {
		var b [10]byte
		if _, err := rand.Read(b[:]); err != nil {
			panic("sysconfig: no randomness: " + err.Error()) // crypto/rand does not fail on supported platforms
		}
		id := make([]byte, 1, 11)
		id[0] = 'r'
		letter := false
		for _, c := range b {
			ch := alphabet[int(c)%32] // 256 is a multiple of 32: no modulo bias
			letter = letter || ch <= 'z' && ch >= 'a'
			id = append(id, ch)
		}
		if letter {
			return string(id)
		}
	}
}

// isLegacyID reports whether id is a position id (policy.LegacyID form), which
// names a slot rather than a rule.
func isLegacyID(id string) bool {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "r"))
	return err == nil && n > 0 && id == policy.LegacyID(n-1)
}

// withIDs returns specs with a fresh id for every rule that has none; the
// input is not changed.
func withIDs(specs []RuleSpec) []RuleSpec {
	out := slices.Clone(specs)
	for i := range out {
		if out[i].ID == "" {
			out[i].ID = newRuleID()
		}
	}
	return out
}

// IndexOf returns the position of the rule with the given id, or -1.
func (r *Rules) IndexOf(specs []RuleSpec, id string) int {
	if id == "" {
		return -1
	}
	return slices.IndexFunc(specs, func(s RuleSpec) bool { return s.ID == id })
}

// sameRules reports whether next is cur without a real edit: equal in every
// field, where a next rule without an id matches whatever id cur has.
func sameRules(cur, next []RuleSpec) bool {
	if len(cur) != len(next) {
		return false
	}
	for i := range cur {
		if next[i].ID != "" && next[i].ID != cur[i].ID {
			return false
		}
		a, b := cur[i], next[i]
		a.ID, b.ID = "", ""
		ja, _ := json.Marshal(a) // a plain struct always encodes
		jb, _ := json.Marshal(b)
		if !bytes.Equal(ja, jb) {
			return false
		}
	}
	return true
}

func encodeSpecs(specs []RuleSpec) ([]byte, error) {
	if specs == nil {
		specs = []RuleSpec{}
	}
	return json.Marshal(specs)
}

// State returns the active rules: the newest stored version.
func (r *Rules) State(ctx context.Context) (RulesState, error) {
	var st RulesState
	hist, err := r.o.Store.ListPolicyRuleVersions(ctx, 50)
	if err != nil {
		return st, err
	}
	st.History = hist
	if len(hist) > 0 {
		st.VersionID = hist[0].ID
	}
	if len(hist) == 0 {
		return st, nil
	}
	st.Specs, err = decodeSpecs([]byte(hist[0].Rules))
	return st, err
}

// Active returns the rules the fleet check uses now: the stored ones that validate. A stored
// rule that no longer validates (a legacy value the stricter checks refuse) is left out and named
// in the returned error, which comes with the valid rules: one bad rule must not make the whole
// fleet look compliant by dropping every rule.
func (r *Rules) Active(ctx context.Context) ([]policy.Rule, error) {
	st, err := r.State(ctx)
	if err != nil || len(st.Specs) == 0 {
		return nil, err
	}
	rules, _, bad := r.ValidRules(st.Specs)
	if len(bad) == 0 {
		return rules, nil
	}
	msgs := make([]string, len(bad))
	for i, b := range bad {
		msgs[i] = b.Error()
	}
	return rules, errors.New("stored rules left out: " + strings.Join(msgs, "; "))
}

// ValidRules validates specs like Validate, but a rule that fails is dropped and the rest are
// checked again, so one bad rule does not hide the others. rules and kept are the valid rules as
// policy rules and as specs (same order); bad holds one error per dropped rule, its Index the rule's
// position in specs (-1: the list as a whole is refused, nothing is kept).
func (r *Rules) ValidRules(specs []RuleSpec) (rules []policy.Rule, kept []RuleSpec, bad []*RuleError) {
	kept = slices.Clone(specs)
	orig := make([]int, len(specs))
	for i := range orig {
		orig[i] = i
	}
	for {
		if len(kept) == 0 {
			return nil, nil, bad
		}
		rs, err := r.Validate(kept)
		if err == nil {
			return rs, kept, bad
		}
		var re *RuleError
		if !errors.As(err, &re) || re.Index < 0 || re.Index >= len(kept) {
			msg := err.Error()
			if re != nil {
				msg = re.Msg
			}
			return nil, nil, append(bad, &RuleError{Index: -1, Msg: msg})
		}
		bad = append(bad, &RuleError{Index: orig[re.Index], Msg: re.Msg})
		kept = slices.Delete(kept, re.Index, re.Index+1)
		orig = slices.Delete(orig, re.Index, re.Index+1)
	}
}

var indexRe = regexp.MustCompile(`index (\d+)`)

// Validate checks specs with policy.ParseRules (secret, unknown and
// side-effecting queries are refused) and with the per-setting value rules.
func (r *Rules) Validate(specs []RuleSpec) ([]policy.Rule, error) {
	return r.validate(specs, true)
}

// validate is Validate; checkWant=false skips the per-setting value rules (the
// one-time import of a POLICY_RULES file was only ever checked by ParseRules, and
// importing it must not lose a rule over a stricter check).
func (r *Rules) validate(specs []RuleSpec, checkWant bool) ([]policy.Rule, error) {
	// A rule without an id is new and gets its id when stored; validating it
	// must not let ParseRules's position ids collide with explicit ones.
	data, err := encodeSpecs(withIDs(specs))
	if err != nil {
		return nil, err
	}
	rules, err := policy.ParseRules(data, r.o.Queryable)
	if err != nil {
		re := &RuleError{Index: -1, Msg: strings.TrimPrefix(err.Error(), "policy rules: ")}
		if m := indexRe.FindStringSubmatch(err.Error()); m != nil {
			re.Index, _ = strconv.Atoi(m[1])
			if _, after, ok := strings.Cut(err.Error(), ": "); ok {
				_, after, _ = strings.Cut(after, ": ")
				re.Msg = after
			}
		}
		return nil, re
	}
	if r.o.ValidateWant != nil && checkWant {
		for i, s := range specs {
			if err := r.o.ValidateWant(s.Command, s.Want); err != nil {
				return nil, &RuleError{Index: i, Msg: strings.TrimPrefix(err.Error(), "settings: invalid value: ")}
			}
		}
	}
	return rules, nil
}

// Preview says how many devices the rules would flag.
func (r *Rules) Preview(ctx context.Context, specs []RuleSpec) (PreviewResult, error) {
	rules, err := r.Validate(specs)
	if err != nil {
		return PreviewResult{}, err
	}
	if r.o.Preview == nil {
		return PreviewResult{}, errors.New("sysconfig: preview is not available")
	}
	return r.o.Preview(ctx, rules)
}

// AnyVersion as the base of Save means "replace whatever is newest": the caller did not edit a
// shown version (the CLI and the import, which replace the whole list).
const AnyVersion int64 = -1

// Save stores specs as a new version, provided the newest version is still base (the version the
// person edited; 0 when nothing was stored yet), else ErrStale. The check and the store happen
// under one lock, so a change that lands in between cannot be overwritten.
func (r *Rules) Save(ctx context.Context, actor, note string, base int64, specs []RuleSpec, rec audit.Recorder) (int64, error) {
	return r.saveChecked(ctx, actor, note, base, specs, 0, true, rec)
}

func (r *Rules) save(ctx context.Context, actor, note string, specs []RuleSpec, revertOf int64, rec audit.Recorder) (int64, error) {
	return r.saveChecked(ctx, actor, note, AnyVersion, specs, revertOf, true, rec)
}

func (r *Rules) saveChecked(ctx context.Context, actor, note string, base int64, specs []RuleSpec, revertOf int64, checkWant bool, rec audit.Recorder) (int64, error) {
	if err := checkNote(note); err != nil {
		return 0, err
	}
	if _, err := r.validate(specs, checkWant); err != nil {
		return 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, err := r.current(ctx)
	if err != nil {
		return 0, err
	}
	if base != AnyVersion && cur.ID != base {
		return 0, ErrStale
	}
	action := "policy.rules.save"
	if revertOf != 0 {
		action = "policy.rules.revert"
	}
	return r.store(ctx, cur, storeReq{actor: actor, note: note, specs: specs, revertOf: revertOf, checkWant: checkWant, action: action}, rec)
}

func checkNote(note string) error {
	if len(note) > 200 || strings.ContainsAny(note, "\r\n") {
		return &RuleError{Index: -1, Msg: "the note must be one line of at most 200 characters"}
	}
	return nil
}

// currentVersion is the newest stored version; ID 0 and no specs when nothing
// was stored yet.
type currentVersion struct {
	ID    int64
	Raw   string
	Specs []RuleSpec
}

// current reads the newest version. The caller holds r.mu.
func (r *Rules) current(ctx context.Context) (currentVersion, error) {
	cur, err := r.o.Store.CurrentPolicyRules(ctx)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return currentVersion{}, nil
	case err != nil:
		return currentVersion{}, err
	}
	specs, err := decodeSpecs([]byte(cur.Rules))
	return currentVersion{ID: cur.ID, Raw: cur.Rules, Specs: specs}, err
}

type storeReq struct {
	actor, note string
	specs       []RuleSpec
	revertOf    int64
	checkWant   bool
	action      string
	op          string // list operation for the audit detail ("" for a plain save)
}

// store validates and stores q.specs as a new version on top of cur, audits
// it and tells OnChange. The caller holds r.mu.
func (r *Rules) store(ctx context.Context, cur currentVersion, q storeReq, rec audit.Recorder) (int64, error) {
	submitted := q.specs
	q.specs = withIDs(q.specs)
	rules, err := r.validate(q.specs, q.checkWant)
	if err != nil {
		return 0, err
	}
	data, err := encodeSpecs(q.specs)
	if err != nil {
		return 0, err
	}
	// Opening a version stored before 0.19 and saving it unedited is no change:
	// the ids the decoder synthesised do not count as an edit.
	if cur.ID != 0 && q.revertOf == 0 && sameRules(cur.Specs, submitted) {
		return 0, ErrNoChange
	}
	id, err := r.o.Store.AddPolicyRuleVersion(ctx, store.PolicyRuleVersion{Rules: string(data), Note: q.note, Actor: q.actor, RevertOf: q.revertOf})
	if err != nil {
		return 0, err
	}
	detail := map[string]any{"version": id, "revert_of": q.revertOf, "rules": len(q.specs), "changes": DiffSpecs(cur.Specs, q.specs)}
	if q.op != "" {
		detail["op"] = q.op
	}
	d, _ := json.Marshal(detail) // a plain map always encodes
	if rec == nil {
		rec = r.o.Audit
	}
	rec.Record(ctx, model.AuditEvent{Actor: q.actor, Action: q.action, Target: "policy:rules", Detail: string(d), Result: "ok"})
	if r.o.OnChange != nil {
		r.o.OnChange(rules)
	}
	return id, nil
}

// mutate applies a list operation to the newest version, provided it is still
// base (the version the person saw), and stores the result as a new version.
// Reading, checking and storing happen under one lock, so two operations on
// the same version cannot both land.
func (r *Rules) mutate(ctx context.Context, actor string, base int64, op string, fn func([]RuleSpec) ([]RuleSpec, error), rec audit.Recorder) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, err := r.current(ctx)
	if err != nil {
		return 0, err
	}
	if cur.ID != base {
		return 0, ErrStale
	}
	specs, err := fn(slices.Clone(cur.Specs))
	if err != nil {
		return 0, err
	}
	return r.store(ctx, cur, storeReq{actor: actor, specs: specs, checkWant: true, action: "policy.rules.save", op: op}, rec)
}

// Reorder stores the rules of version base in a new order: order[i] is the
// old position of the rule that moves to position i (a permutation). The
// order is the precedence (policy.Check).
func (r *Rules) Reorder(ctx context.Context, actor string, base int64, order []int, rec audit.Recorder) (int64, error) {
	return r.mutate(ctx, actor, base, "reorder", func(specs []RuleSpec) ([]RuleSpec, error) {
		if len(order) != len(specs) {
			return nil, &RuleError{Index: -1, Msg: "the new order must name every rule exactly once"}
		}
		seen := make([]bool, len(specs))
		out := make([]RuleSpec, len(specs))
		for i, from := range order {
			if from < 0 || from >= len(specs) || seen[from] {
				return nil, &RuleError{Index: -1, Msg: "the new order must name every rule exactly once"}
			}
			seen[from] = true
			out[i] = specs[from]
		}
		return out, nil
	}, rec)
}

func cloneSpec(s RuleSpec) RuleSpec {
	s.Families, s.Tags = slices.Clone(s.Families), slices.Clone(s.Tags)
	s.Devices, s.Except = slices.Clone(s.Devices), slices.Clone(s.Except)
	return s
}

// SetEnabled stores version base with the rule at idx enabled or disabled.
func (r *Rules) SetEnabled(ctx context.Context, actor string, base int64, idx int, enabled bool, rec audit.Recorder) (int64, error) {
	op := "disable"
	if enabled {
		op = "enable"
	}
	return r.mutate(ctx, actor, base, op, func(specs []RuleSpec) ([]RuleSpec, error) {
		if idx < 0 || idx >= len(specs) {
			return nil, ErrNoSuchRule
		}
		specs[idx] = cloneSpec(specs[idx])
		specs[idx].Disabled = !enabled
		return specs, nil
	}, rec)
}

// Revert stores the rules of an older version as a new version.
func (r *Rules) Revert(ctx context.Context, actor string, id int64, rec audit.Recorder) (int64, error) {
	v, err := r.o.Store.GetPolicyRuleVersion(ctx, id)
	if err != nil {
		return 0, err
	}
	specs, err := decodeSpecs([]byte(v.Rules))
	if err != nil {
		return 0, err
	}
	return r.save(ctx, actor, "revert to version "+strconv.FormatInt(id, 10), specs, id, rec)
}

// Export returns the active rules in the POLICY_RULES format.
func (r *Rules) Export(ctx context.Context) ([]byte, error) {
	st, err := r.State(ctx)
	if err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(nonNil(st.Specs), "", "  ")
	return append(b, '\n'), err
}

func nonNil(s []RuleSpec) []RuleSpec {
	if s == nil {
		return []RuleSpec{}
	}
	return s
}

// ParseImport reads rules in the POLICY_RULES format (strictly) for the editor.
func (r *Rules) ParseImport(data []byte) ([]RuleSpec, error) {
	if len(data) > 256<<10 {
		return nil, &RuleError{Index: -1, Msg: "the file is too large"}
	}
	specs, err := decodeRaw(data) // ids are kept as given; rules without one get theirs when saved
	if err != nil {
		return nil, &RuleError{Index: -1, Msg: strings.TrimPrefix(err.Error(), "policy rules: ")}
	}
	if _, err := r.Validate(specs); err != nil {
		return nil, err
	}
	return specs, nil
}

// HasVersions reports whether any rules were ever saved.
func (r *Rules) HasVersions(ctx context.Context) (bool, error) {
	_, err := r.o.Store.CurrentPolicyRules(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// RuleChange is one line of a diff between two rule lists.
type RuleChange struct {
	Kind string `json:"kind"` // added, removed, changed
	Rule string `json:"rule"`
	// Name is the rule's name, or its command when it has none; the history
	// summaries use it instead of the longer Rule label.
	Name string `json:"name,omitempty"`
	// RuleID is the id the rule has on the side it is shown from (the new side
	// for added and changed, the old side for removed). A position id of a
	// version stored before 0.19 does not identify a rule across versions.
	RuleID string `json:"ruleId,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Fields names what a changed rule differs in: "command", "expected value",
	// "scope", "severity", "name", "state", "note". Empty for added and removed.
	Fields []string `json:"fields,omitempty"`
}

func ruleKey(s RuleSpec) string {
	if s.Name != "" {
		return s.Name
	}
	return s.Command + " [" + strings.Join(s.Families, ",") + "|" + strings.Join(s.Tags, ",") + "|" + strings.Join(s.Devices, ",") + "]"
}

func ruleName(s RuleSpec) string {
	if s.Name != "" {
		return s.Name
	}
	return s.Command
}

func ruleLabel(s RuleSpec) string {
	if s.Name != "" {
		return s.Name + " (" + s.Command + ")"
	}
	return s.Command
}

// DiffSpecs compares two rule lists. Rules with a real id (not a position id)
// are matched by id, so a renamed rule is "changed", not removed and added; the
// rest are matched by name (or by command and scope). A changed expected value,
// scope, severity or note is "changed".
func DiffSpecs(a, b []RuleSpec) []RuleChange {
	var out []RuleChange
	used := make([]bool, len(b))
	byID := make([]int, len(a)) // index in b of the rule with the same real id, or -1
	for i, x := range a {
		byID[i] = -1
		if x.ID == "" || isLegacyID(x.ID) {
			continue
		}
		byID[i] = slices.IndexFunc(b, func(y RuleSpec) bool { return y.ID == x.ID })
		if byID[i] >= 0 {
			used[byID[i]] = true
		}
	}
	for i, x := range a {
		match := byID[i]
		for j, y := range b {
			if match >= 0 {
				break
			}
			if !used[j] && ruleKey(x) == ruleKey(y) {
				match = j
			}
		}
		if match < 0 {
			out = append(out, RuleChange{Kind: "removed", Rule: ruleLabel(x), Name: ruleName(x), RuleID: x.ID, Detail: "expected " + x.Want})
			continue
		}
		used[match] = true
		if d, fields := specDelta(x, b[match]); d != "" {
			out = append(out, RuleChange{Kind: "changed", Rule: ruleLabel(b[match]), Name: ruleName(b[match]), RuleID: b[match].ID, Detail: d, Fields: fields})
		}
	}
	for j, y := range b {
		if !used[j] {
			out = append(out, RuleChange{Kind: "added", Rule: ruleLabel(y), Name: ruleName(y), RuleID: y.ID, Detail: "expected " + y.Want})
		}
	}
	return out
}

// specDelta describes how y differs from x: the detail text and the changed
// fields in a fixed order.
func specDelta(x, y RuleSpec) (string, []string) {
	var parts, fields []string
	field := func(f string) {
		if !slices.Contains(fields, f) {
			fields = append(fields, f)
		}
	}
	add := func(label, from, to, f string) {
		if from != to {
			parts = append(parts, label+" "+strconv.Quote(from)+" -> "+strconv.Quote(to))
			field(f)
		}
	}
	add("command", x.Command, y.Command, "command")
	add("expected", x.Want, y.Want, "expected value")
	add("severity", orWarn(x.Severity), orWarn(y.Severity), "severity")
	add("note", x.Note, y.Note, "note")
	add("name", x.Name, y.Name, "name")
	add("state", stateWord(x), stateWord(y), "state")
	for label, p := range map[string][2][]string{
		"families": {x.Families, y.Families}, "tags": {x.Tags, y.Tags}, "devices": {x.Devices, y.Devices}, "except": {x.Except, y.Except},
	} {
		if !slices.Equal(p[0], p[1]) {
			parts = append(parts, label+" ["+strings.Join(p[0], ",")+"] -> ["+strings.Join(p[1], ",")+"]")
			field("scope")
		}
	}
	slices.Sort(parts)
	slices.SortFunc(fields, func(a, b string) int { return slices.Index(fieldOrder, a) - slices.Index(fieldOrder, b) })
	return strings.Join(parts, "; "), fields
}

var fieldOrder = []string{"name", "command", "expected value", "scope", "severity", "state", "note"}

func stateWord(s RuleSpec) string {
	if s.Disabled {
		return "disabled"
	}
	return "enabled"
}

func orWarn(s string) string {
	if s == "" {
		return policy.SeverityWarn
	}
	return s
}

// VersionDiff compares two stored versions.
func (r *Rules) VersionDiff(ctx context.Context, from, to int64) ([]RuleChange, error) {
	var sa []RuleSpec // from 0: an empty base, so the first version reads as all added
	if from != 0 {
		a, err := r.o.Store.GetPolicyRuleVersion(ctx, from)
		if err != nil {
			return nil, err
		}
		if sa, err = decodeSpecs([]byte(a.Rules)); err != nil {
			return nil, err
		}
	}
	b, err := r.o.Store.GetPolicyRuleVersion(ctx, to)
	if err != nil {
		return nil, err
	}
	sb, err := decodeSpecs([]byte(b.Rules))
	if err != nil {
		return nil, err
	}
	return DiffSpecs(sa, sb), nil
}

// importNote is the note of the version ImportLegacy stores; the history never
// folds it into other saves.
const importNote = "imported from POLICY_RULES"

// ImportLegacy stores the content of a POLICY_RULES variable or file as the first
// managed version. It is checked like the variable always was (ParseRules with the
// queryable rule) and refused if rules were saved already.
func (r *Rules) ImportLegacy(ctx context.Context, actor string, raw []byte, rec audit.Recorder) (int64, error) {
	specs, err := decodeSpecs(raw)
	if err != nil {
		return 0, err
	}
	if has, err := r.HasVersions(ctx); err != nil || has {
		return 0, errors.Join(err, ErrNoChange)
	}
	return r.saveChecked(ctx, actor, importNote, AnyVersion, specs, 0, false, rec)
}
