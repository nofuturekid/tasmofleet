package web

import (
	"context"
	"html"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/settings"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
)

type rulesMem struct {
	mu sync.Mutex
	vs []store.PolicyRuleVersion
}

func (m *rulesMem) AddPolicyRuleVersion(_ context.Context, v store.PolicyRuleVersion) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v.ID = int64(len(m.vs) + 1)
	m.vs = append(m.vs, v)
	return v.ID, nil
}
func (m *rulesMem) CurrentPolicyRules(context.Context) (store.PolicyRuleVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.vs) == 0 {
		return store.PolicyRuleVersion{}, store.ErrNotFound
	}
	return m.vs[len(m.vs)-1], nil
}
func (m *rulesMem) GetPolicyRuleVersion(_ context.Context, id int64) (store.PolicyRuleVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id < 1 || int(id) > len(m.vs) {
		return store.PolicyRuleVersion{}, store.ErrNotFound
	}
	return m.vs[id-1], nil
}
func (m *rulesMem) ListPolicyRuleVersions(context.Context, int) ([]store.PolicyRuleVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.PolicyRuleVersion
	for i := len(m.vs) - 1; i >= 0; i-- {
		out = append(out, m.vs[i])
	}
	return out, nil
}

func rulesEnv(t *testing.T, envJSON string, tweak ...func(*sysconfig.RulesOptions)) (*env, *rulesMem, *[]policy.Rule) {
	t.Helper()
	return rulesEnvDeps(t, nil, tweak...)
}

// rulesEnvDeps is rulesEnv with more dependencies set by deps.
func rulesEnvDeps(t *testing.T, deps func(*Deps), tweak ...func(*sysconfig.RulesOptions)) (*env, *rulesMem, *[]policy.Rule) {
	t.Helper()
	mem := &rulesMem{}
	live := &[]policy.Rule{}
	aud := &fakeAudit{}
	o := sysconfig.RulesOptions{
		Store: mem, Audit: aud, Queryable: settings.Queryable,
		ValidateWant: func(c, w string) error {
			if f, ok := settings.Lookup(c); ok && f.Validate != nil {
				return f.Validate(w)
			}
			return nil
		},
		Preview: func(_ context.Context, rules []policy.Rule) (sysconfig.PreviewResult, error) {
			// Flagged = position + 1 lets a test see which rule of the list
			// a preview number belongs to.
			res := sysconfig.PreviewResult{Devices: 2, NoData: 1, Rules: make([]sysconfig.RulePreview, len(rules))}
			for i := range rules {
				res.Rules[i] = sysconfig.RulePreview{Flagged: i + 1, Unknown: 1}
			}
			return res, nil
		},
		OnChange: func(r []policy.Rule) { *live = r },
	}
	for _, f := range tweak {
		f(&o)
	}
	svc := sysconfig.NewRules(o)
	e := setupWith(t, func(d *Deps) {
		d.PolicyRules = svc
		d.Recorder = aud
		d.Policy = noPolicy{}
		if deps != nil {
			deps(d)
		}
	})
	return e, mem, live
}

type noPolicy struct{}

func (noPolicy) Report(context.Context, string, bool) (PolicyReport, error) {
	return PolicyReport{}, nil
}

// ruleForm is the editor form of one rule. idx "new" appends, a number
// replaces that rule.
func ruleForm(extra url.Values) url.Values {
	f := url.Values{
		"csrf": {"tok"}, "idx": {"new"}, "version": {"0"}, "name": {"Flash wear"}, "command": {"SaveData"}, "want": {"0"}, "severity": {"warn"},
		"family": {"esp32*"}, "tag": {"kitchen"}, "tags_extra": {"attic, garage"}, "device": {}, "except": {"lamp"}, "note": {"keep flash alive"},
	}
	for k, v := range extra {
		f[k] = v
	}
	return f
}

func saveRule(t *testing.T, e *env, extra url.Values) {
	t.Helper()
	if r, body := e.do(t, "POST", "/policy/rules/save", "admin", ruleForm(extra)); r.StatusCode != 303 {
		t.Fatalf("save: %d\n%s", r.StatusCode, body)
	}
}

// Why: operators may look at the rules but never change them, so the page
// must not even render a write control and every write must be refused.
func TestPolicyRulesOperatorIsReadOnly(t *testing.T) {
	e, mem, _ := rulesEnv(t, "")
	saveRule(t, e, nil)
	r, body := e.do(t, "GET", "/policy?edit=0", "operator", nil)
	if r.StatusCode != 200 || !strings.Contains(body, "Flash wear") {
		t.Fatalf("operator view: %d", r.StatusCode)
	}
	for _, bad := range []string{`name="want"`, `name="command"`, "/policy/rules/save", "/policy/rules/delete", "/policy/io/import", "/policy/rules/revert",
		"/policy/rules/duplicate", "/policy/rules/order", "New rule", "Rule actions", "Import…", "/policy/live/pause"} {
		if strings.Contains(body, bad) {
			t.Errorf("operator page renders a write control: %q", bad)
		}
	}
	for _, p := range []string{"/policy/rules/save", "/policy/rules/delete", "/policy/rules/preview", "/policy/io/import", "/policy/rules/revert/1"} {
		if r, _ := e.do(t, "POST", p, "operator", ruleForm(nil)); r.StatusCode != 403 {
			t.Errorf("POST %s as operator: %d", p, r.StatusCode)
		}
	}
	if r, _ := e.do(t, "GET", "/policy/rules/value-control?command=SaveData", "operator", nil); r.StatusCode != 403 {
		t.Errorf("value control as operator: %d", r.StatusCode)
	}
	if r, _ := e.do(t, "GET", "/policy", "", nil); r.StatusCode != 401 {
		t.Errorf("anonymous: %d", r.StatusCode)
	}
	if len(mem.vs) != 1 {
		t.Fatalf("operator writes stored a version: %d", len(mem.vs))
	}
}

// Why: the expected value must be entered with the control that fits the
// setting, so an admin cannot type a value the settings editor would refuse.
func TestPolicyRulesValueControlMatchesSettingType(t *testing.T) {
	e, _, _ := rulesEnv(t, "")
	get := func(q string) string {
		t.Helper()
		r, body := e.do(t, "GET", "/policy/rules/value-control?"+q, "admin", nil)
		if r.StatusCode != 200 || strings.Contains(body, "<html") {
			t.Fatalf("%s: %d (a fragment is expected)", q, r.StatusCode)
		}
		return body
	}
	enum := get("command=PowerOnState&want=3")
	for _, w := range []string{`<select id="want" name="want"`, `value="0"`, `3 (last state, saved)`, `value="3" selected`} {
		if !strings.Contains(enum, w) {
			t.Errorf("enum control lacks %q:\n%s", w, enum)
		}
	}
	toggle := get("command=SetOption0&want=1")
	for _, w := range []string{`type="radio"`, `value="1" checked`, `value="0"`, "on", "off"} {
		if !strings.Contains(toggle, w) {
			t.Errorf("toggle control lacks %q:\n%s", w, toggle)
		}
	}
	num := get("command=SaveData&want=5")
	for _, w := range []string{`type="number"`, `min="0"`, `max="3600"`, `value="5"`} {
		if !strings.Contains(num, w) {
			t.Errorf("number control lacks %q:\n%s", w, num)
		}
	}
	if txt := get("command=Hostname&want=plug"); !strings.Contains(txt, `type="text"`) || !strings.Contains(txt, `value="plug"`) {
		t.Errorf("text control:\n%s", txt)
	}
	// Changing the setting drops an expected value that no longer fits.
	if sw := get("command=PowerOnState&want=1234"); strings.Contains(sw, "1234") || strings.Contains(sw, " selected") {
		t.Errorf("a value that does not fit the new setting must be dropped:\n%s", sw)
	}
	if sw := get("command=SaveData&want=abc"); strings.Contains(sw, "abc") {
		t.Errorf("a non-number must be dropped:\n%s", sw)
	}
}

// Why: the editor shows what a draft would do before anything is saved; the
// number must belong to the draft rule, and a preview never stores a version.
func TestPolicyRulesPreviewFragmentCountsTheDraftAndStoresNothing(t *testing.T) {
	e, mem, _ := rulesEnv(t, "")
	r, body := e.do(t, "POST", "/policy/rules/preview", "admin", ruleForm(nil))
	if r.StatusCode != 200 || strings.Contains(body, "<html") {
		t.Fatalf("preview: %d", r.StatusCode)
	}
	if !strings.Contains(body, "Would flag <b>1</b> of 2 devices") || !strings.Contains(body, "1 cannot be judged") {
		t.Errorf("preview text:\n%s", body)
	}
	saveRule(t, e, nil)
	saveRule(t, e, url.Values{"version": {"1"}, "name": {"Second"}, "command": {"SetOption0"}, "want": {"1"}})
	before := len(mem.vs)
	// Editing rule 1 of the list [Flash wear, Second]: its number is position+1 = 2.
	_, body = e.do(t, "POST", "/policy/rules/preview", "admin", ruleForm(url.Values{"idx": {"1"}, "version": {"2"}, "name": {"Second"}, "command": {"SetOption0"}, "want": {"1"}}))
	if !strings.Contains(body, "Would flag <b>2</b>") {
		t.Errorf("the draft must replace rule 1 in the list:\n%s", body)
	}
	// A new rule is appended after the two saved ones.
	_, body = e.do(t, "POST", "/policy/rules/preview", "admin", ruleForm(nil))
	if !strings.Contains(body, "Would flag <b>3</b>") {
		t.Errorf("a new draft is appended:\n%s", body)
	}
	if len(mem.vs) != before {
		t.Fatal("a preview must not store a version")
	}
	// An invalid draft explains itself instead of failing.
	r, body = e.do(t, "POST", "/policy/rules/preview", "admin", ruleForm(url.Values{"want": {"abc"}}))
	if r.StatusCode != 200 || !strings.Contains(body, `class="ufield-err"`) || !strings.Contains(body, "whole number") {
		t.Errorf("invalid draft preview: %d\n%s", r.StatusCode, body)
	}
	// An unfinished draft is a hint, not an error.
	_, body = e.do(t, "POST", "/policy/rules/preview", "admin", ruleForm(url.Values{"command": {""}, "want": {""}}))
	if strings.Contains(body, `class="ufield-err"`) || !strings.Contains(body, "Enter a command") {
		t.Errorf("unfinished draft:\n%s", body)
	}
}

// Why: selecting a rule card already opens its editor in the middle column, so the card's "⋯"
// menu has no "Edit" (Alice 2026-10-23: it was redundant). The card itself stays a link to the
// editor, which keeps editing reachable without JavaScript and by keyboard; the menu keeps the
// actions the card cannot do.
func TestPolicyRuleMenuHasNoEditTheCardOpensTheEditor(t *testing.T) {
	e, _, _ := rulesEnv(t, "")
	saveRule(t, e, nil)
	_, page := e.do(t, "GET", "/policy", "admin", nil)
	card := page[strings.Index(page, `class="pol-card`):]
	if !strings.Contains(card[:strings.Index(card, "pol-menu")], `<a class="pol-card-main" href="/tf/policy?edit=0"`) {
		t.Errorf("the rule card does not link to its editor:\n%s", card)
	}
	menu := card[strings.Index(card, "pol-menu"):]
	menu = menu[:strings.Index(menu, "</details>")]
	if strings.Contains(menu, ">Edit<") || strings.Contains(menu, "#editor") {
		t.Errorf("the rule menu still offers Edit:\n%s", menu)
	}
	for _, w := range []string{">Duplicate<", ">Disable<", ">History<", ">Delete…<"} {
		if !strings.Contains(menu, w) {
			t.Errorf("the rule menu lost %q:\n%s", w, menu)
		}
	}
}

// Why: each rule card says what the rule checks (setting and expected value), which devices it
// covers, its exceptions and note, and how many devices it flags now; the editor offers every
// setting a rule can check and the inventory's tags and devices.
func TestPolicyRulesCardsAndEditorChoices(t *testing.T) {
	e, _, _ := rulesEnv(t, "")
	saveRule(t, e, url.Values{"command": {"SetOption0"}, "want": {"0"}, "name": {"No save state"}})
	_, page := e.do(t, "GET", "/policy", "admin", nil)
	card := page[strings.Index(page, `class="pol-card`):]
	card = card[:strings.Index(card, "pol-menu")]
	for _, w := range []string{"No save state", "SetOption0 = 0", "Chip family: ESP32 (all variants)", "Tag: kitchen, attic, garage", "1 exception", ">lamp<", "keep flash alive", "Required", "1 flagged"} {
		if !strings.Contains(card, w) {
			t.Errorf("the rule card lacks %q:\n%s", w, card)
		}
	}
	// New rule editor: setting picker (SetOptions with their description), every family chip and the inventory's tags and devices.
	_, page = e.do(t, "GET", "/policy?new=1", "admin", nil)
	for _, w := range []string{`id="editor"`, `<input id="rule-command" name="command"`, `<datalist id="rule-commands">`, `<option value="PowerOnState">`, `value="esp8266"`, `value="esp32*"`, `value="esp32c3"`, `name="tag" value="kitchen"`, `name="device" value="attic"`, `name="device" value="kitchen"`, `value="info"`} {
		if !strings.Contains(page, w) {
			t.Errorf("new rule editor lacks %q", w)
		}
	}
	if strings.Contains(page, `<option value="MqttPassword"`) {
		t.Error("secret settings must not be offered")
	}
	// Edit: the saved values are pre-selected.
	if !strings.Contains(page, "SetOption0 (save_state)") || !strings.Contains(page, "restore relay state") {
		t.Error("a SetOption is offered with its name and description")
	}
	_, page = e.do(t, "GET", "/policy?edit=0", "admin", nil)
	for _, w := range []string{`value="No save state"`, `name="command" class="mono" list="rule-commands" value="SetOption0"`, `name="family" value="esp32*" checked`, `name="tag" value="attic" checked`, `name="tag" value="garage" checked`, `value="lamp"`, `name="idx" value="0"`} {
		if !strings.Contains(page, w) {
			t.Errorf("edit editor lacks %q", w)
		}
	}
}

func TestPolicyRulesCreateEditDeleteRevert(t *testing.T) {
	e, mem, live := rulesEnv(t, "")
	saveRule(t, e, url.Values{"change_note": {"first"}})
	if len(mem.vs) != 1 || mem.vs[0].Note != "first" || len(*live) != 1 || (*live)[0].Name != "Flash wear" {
		t.Fatalf("create: versions=%d live=%v", len(mem.vs), *live)
	}
	for _, w := range []string{`"tags":["kitchen","attic","garage"]`, `"except":["lamp"]`, `"families":["esp32*"]`} {
		if !strings.Contains(mem.vs[0].Rules, w) {
			t.Errorf("stored rules lack %s: %s", w, mem.vs[0].Rules)
		}
	}
	saveRule(t, e, url.Values{"idx": {"0"}, "version": {"1"}, "want": {"1"}}) // edit
	if len(mem.vs) != 2 || (*live)[0].Want != "1" {
		t.Fatalf("edit: versions=%d", len(mem.vs))
	}
	// The history compares a picked version with the current rules, for viewers too.
	if r, body := e.do(t, "GET", "/policy?v=1", "operator", nil); r.StatusCode != 200 || !strings.Contains(body, "v1 → current") || !strings.Contains(body, "Changed: ") {
		t.Fatalf("diff: %d", r.StatusCode)
	}
	if r, _ := e.do(t, "POST", "/policy/rules/revert/1", "admin", url.Values{"csrf": {"tok"}}); r.StatusCode != 303 || len(mem.vs) != 3 || mem.vs[2].RevertOf != 1 || (*live)[0].Want != "0" {
		t.Fatalf("revert: versions=%d", len(mem.vs))
	}
	if r, _ := e.do(t, "POST", "/policy/rules/delete", "admin", url.Values{"csrf": {"tok"}, "idx": {"0"}, "version": {"3"}}); r.StatusCode != 303 || len(mem.vs) != 4 || len(*live) != 0 {
		t.Fatalf("delete: versions=%d live=%v", len(mem.vs), *live)
	}
}

// Why: the revert dialog is the last look before the rules change, so it must list what the
// revert does (current -> the picked version), not the history's "vN -> current" diff, which
// reads the other way round and would announce the opposite change.
func TestPolicyRevertDialogListsWhatTheRevertChanges(t *testing.T) {
	e, _, _ := rulesEnv(t, "")
	saveRule(t, e, url.Values{"want": {"0"}})
	saveRule(t, e, url.Values{"idx": {"0"}, "version": {"1"}, "want": {"1"}})
	_, body := e.do(t, "GET", "/policy?v=1", "admin", nil)
	i := strings.Index(body, `id="dlg-revert"`)
	if i < 0 {
		t.Fatal("no revert dialog for an older version")
	}
	dlg := html.UnescapeString(body[i:])
	dlg = dlg[:strings.Index(dlg, "</dialog>")]
	if !strings.Contains(dlg, `expected "1" -> "0"`) || strings.Contains(dlg, `expected "0" -> "1"`) {
		t.Fatalf("revert dialog must list current -> v1:\n%s", dlg)
	}
	hist := html.UnescapeString(body[:i])
	if !strings.Contains(hist, `expected "0" -> "1"`) {
		t.Error("the history diff still reads v1 -> current")
	}
}

// Why: two admins editing at once must not overwrite each other: a stale
// editor (it was opened on an older version) is refused, not merged blindly.
func TestPolicyRulesStaleEditorIsRefused(t *testing.T) {
	e, mem, _ := rulesEnv(t, "")
	saveRule(t, e, nil)
	r, body := e.do(t, "POST", "/policy/rules/save", "admin", ruleForm(url.Values{"idx": {"0"}, "version": {"0"}, "want": {"1"}}))
	if r.StatusCode != 409 || !strings.Contains(body, "changed") || len(mem.vs) != 1 {
		t.Fatalf("stale save: %d versions=%d", r.StatusCode, len(mem.vs))
	}
	if r, _ := e.do(t, "POST", "/policy/rules/delete", "admin", url.Values{"csrf": {"tok"}, "idx": {"0"}, "version": {"0"}}); r.StatusCode != 409 || len(mem.vs) != 1 {
		t.Fatalf("stale delete: %d", r.StatusCode)
	}
	if r, _ := e.do(t, "POST", "/policy/rules/delete", "admin", url.Values{"csrf": {"tok"}, "idx": {"7"}, "version": {"1"}}); r.StatusCode != 404 {
		t.Fatalf("delete of a missing rule: %d", r.StatusCode)
	}
}

// Why: no secret or side-effect queries, no injection; the error must appear
// at the rule it belongs to, with the admin's input kept.
func TestPolicyRulesInvalidDraftShowsErrorAtTheRule(t *testing.T) {
	e, mem, _ := rulesEnv(t, "")
	saveRule(t, e, nil)
	saveRule(t, e, url.Values{"version": {"1"}, "name": {"Second"}, "command": {"SetOption0"}, "want": {"1"}})
	for name, f := range map[string]url.Values{
		"secret":    ruleForm(url.Values{"idx": {"1"}, "version": {"2"}, "command": {"MqttPassword"}}),
		"injection": ruleForm(url.Values{"idx": {"1"}, "version": {"2"}, "want": {"0;Restart 1"}}),
		"severity":  ruleForm(url.Values{"idx": {"1"}, "version": {"2"}, "severity": {"fatal"}}),
		"number":    ruleForm(url.Values{"idx": {"1"}, "version": {"2"}, "want": {"99999"}}),
	} {
		r, body := e.do(t, "POST", "/policy/rules/save", "admin", f)
		if r.StatusCode != 422 {
			t.Errorf("%s: %d", name, r.StatusCode)
			continue
		}
		if n := strings.Count(body, `role="alert"`); n != 1 {
			t.Errorf("%s: the problem must be shown once, got %d alerts", name, n)
		}
		ed := body[strings.Index(body, `id="editor"`):]
		if !strings.Contains(ed, `role="alert"`) || !strings.Contains(ed, `name="idx" value="1"`) || !strings.Contains(ed, `value="Flash wear"`) {
			t.Errorf("%s: the error and the draft belong in the editor of rule 1", name)
		}
	}
	if len(mem.vs) != 2 {
		t.Fatal("nothing may be stored")
	}
}

// Why: strict form parsing: an unknown or repeated key is a forged or broken request.
func TestPolicyRulesRejectUnknownFormKeys(t *testing.T) {
	e, mem, _ := rulesEnv(t, "")
	for name, f := range map[string]url.Values{
		"extra key": ruleForm(url.Values{"admin": {"1"}}), "old row key": ruleForm(url.Values{"r.0.name": {"x"}}),
		"two commands": ruleForm(url.Values{"command": {"SaveData", "SetOption0"}}), "bad idx": ruleForm(url.Values{"idx": {"../x"}}),
		"bad version": ruleForm(url.Values{"version": {"x"}}),
	} {
		for _, p := range []string{"/policy/rules/save", "/policy/rules/preview"} {
			if r, _ := e.do(t, "POST", p, "admin", f); r.StatusCode != 400 {
				t.Errorf("%s %s: %d", name, p, r.StatusCode)
			}
		}
	}
	if r, _ := e.do(t, "POST", "/policy/rules/delete", "admin", url.Values{"csrf": {"tok"}, "idx": {"0"}, "version": {"0"}, "x": {"1"}}); r.StatusCode != 400 {
		t.Errorf("delete extra key: %d", r.StatusCode)
	}
	if r, _ := e.do(t, "GET", "/policy/rules/value-control?command=SaveData&x=1", "admin", nil); r.StatusCode != 400 {
		t.Errorf("value control extra key: %d", r.StatusCode)
	}
	if len(mem.vs) != 0 {
		t.Fatal("nothing may be stored")
	}
}

// Why: an import replaces the whole list, so it is checked and compared with
// the saved rules first; only the confirmed step stores a version.
func TestPolicyRulesImportChecksThenSaves(t *testing.T) {
	e, mem, _ := rulesEnv(t, "")
	good := `[{"command":"SetOption0","want":"1","families":["esp32*"],"except":["lamp"]}]`
	r, body := e.do(t, "POST", "/policy/io/import", "admin", url.Values{"csrf": {"tok"}, "json": {good}})
	if r.StatusCode != 200 || len(mem.vs) != 0 || !strings.Contains(body, ">New<") || !strings.Contains(body, `name="confirm"`) || !strings.Contains(body, "Import 1 rule<") {
		t.Fatalf("import check: %d saved=%d", r.StatusCode, len(mem.vs))
	}
	if r, _ := e.do(t, "POST", "/policy/io/import", "admin", url.Values{"csrf": {"tok"}, "json": {good}, "confirm": {"1"}}); r.StatusCode != 303 || len(mem.vs) != 1 || !strings.HasSuffix(r.Header.Get("Location"), "/policy?ok=imported") {
		t.Fatalf("import confirm: %d saved=%d", r.StatusCode, len(mem.vs))
	}
	if r, _ := e.do(t, "POST", "/policy/io/import", "admin", url.Values{"csrf": {"tok"}, "json": {`[{"command":"MqttPassword","want":"x"}]`}, "confirm": {"1"}}); r.StatusCode != 422 || len(mem.vs) != 1 {
		t.Fatalf("secret import: %d", r.StatusCode)
	}
	if r, _ := e.do(t, "POST", "/policy/io/import", "admin", url.Values{"csrf": {"tok"}, "json": {good}, "x": {"1"}}); r.StatusCode != 400 {
		t.Fatalf("extra field: %d", r.StatusCode)
	}
	r, out := e.do(t, "GET", "/policy/io/export", "operator", nil)
	if r.StatusCode != 200 || r.Header.Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") || !strings.Contains(r.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("export headers: %v", r.Header)
	}
	if _, err := policy.ParseRules([]byte(out), nil); err != nil {
		t.Fatalf("export is not POLICY_RULES: %v\n%s", err, out)
	}
}

// Why: a save without a setting used to show a validator's chain ("policy rules:
// rule 6: invalid command") twice and focus the wrong field. The person must read
// one plain sentence under the command field, which is marked invalid and
// takes the focus; nothing is stored.
func TestPolicyRulesSaveWithoutSettingMarksTheSettingField(t *testing.T) {
	e, mem, _ := rulesEnv(t, "")
	saveRule(t, e, nil)
	r, body := e.do(t, "POST", "/policy/rules/save", "admin", ruleForm(url.Values{"idx": {"new"}, "version": {"1"}, "command": {""}, "want": {""}}))
	if r.StatusCode != 422 || len(mem.vs) != 1 {
		t.Fatalf("status %d, versions %d", r.StatusCode, len(mem.vs))
	}
	if strings.Contains(body, "policy rules:") || strings.Contains(body, "invalid command") {
		t.Fatalf("raw validator text shown: %s", body)
	}
	if n := strings.Count(body, "Enter a command."); n != 1 {
		t.Fatalf("the sentence must appear exactly once, got %d", n)
	}
	ed := body[strings.Index(body, `id="editor"`):]
	sel := ed[strings.Index(ed, `<input id="rule-command"`):]
	sel = sel[:strings.Index(sel, ">")]
	for _, want := range []string{`aria-invalid="true"`, `aria-describedby="err-command help-command"`, `autofocus`} {
		if !strings.Contains(sel, want) {
			t.Errorf("command field lacks %s: %s", want, sel)
		}
	}
	if !strings.Contains(ed, `id="err-command"`) {
		t.Error("no message element for the command field")
	}
	if strings.Contains(ed[:strings.Index(ed, `name="command"`)], `autofocus`) {
		t.Error("focus must not stay on the Name field")
	}
}

// Why: the command field takes any Tasmota command, so a refusal must come back at the field
// with the reason in plain words (never the rejected text from a validator chain), nothing may
// be stored, and a plain command must save and show up on the card.
func TestPolicyRuleFreeCommandField(t *testing.T) {
	e, mem, _ := rulesEnv(t, "")
	saveRule(t, e, nil)
	for command, why := range map[string]string{
		"Status0":      "Status commands are never sent automatically.",
		"status 0":     "Enter a command.", // not a name: the pattern check speaks first
		"MqttPassword": "Secret settings cannot be checked.",
		"Reset":        "This command acts when it is sent without a value",
		"Template":     "This command needs a confirmation in the console and cannot be a policy rule.",
	} {
		want := "1"
		r, body := e.do(t, "POST", "/policy/rules/save", "admin", ruleForm(url.Values{"idx": {"new"}, "version": {"1"}, "command": {command}, "want": {want}}))
		ed := body[strings.Index(body, `id="editor"`):]
		if r.StatusCode != 422 || len(mem.vs) != 1 || !strings.Contains(ed, `id="err-command"`) || !strings.Contains(ed, why) && command != "status 0" {
			t.Errorf("%s: status %d, versions %d, reason %q missing in\n%s", command, r.StatusCode, len(mem.vs), why, ed)
		}
		if strings.Contains(body, "policy rules:") {
			t.Errorf("%s: raw validator text shown", command)
		}
	}
	if r, body := e.do(t, "POST", "/policy/rules/save", "admin", ruleForm(url.Values{"idx": {"new"}, "version": {"1"}, "command": {"Sleep"}, "want": {"20"}})); r.StatusCode != 303 {
		t.Fatalf("a plain command must save: %d\n%s", r.StatusCode, body)
	}
	_, page := e.do(t, "GET", "/policy", "admin", nil)
	if !strings.Contains(page, "Sleep = 20") {
		t.Error("the saved generic rule is not on the page")
	}
	// Admin only, like every rule change.
	if r, _ := e.do(t, "POST", "/policy/rules/save", "operator", ruleForm(url.Values{"version": {"2"}, "command": {"Sleep"}, "want": {"30"}})); r.StatusCode != 403 {
		t.Errorf("operator save: %d", r.StatusCode)
	}
}

// Why: the reason of a refused save describes the command that was saved. When the person types
// another command, the reason must go at once (policy.js drops aria-invalid); otherwise it keeps
// accusing a command that is gone. The change handler therefore replaces the reason out of band.
func TestPolicyRuleValueControlClearsTheCommandError(t *testing.T) {
	e, _, _ := rulesEnv(t, "")
	_, body := e.do(t, "GET", "/policy/rules/value-control?command=LedState&want=3", "admin", nil)
	for _, want := range []string{`id="err-command"`, `hx-swap-oob="true"`, ` hidden`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in %s", want, body)
		}
	}
	if strings.Contains(body, "cannot be") || strings.Contains(body, "never sent") {
		t.Errorf("a reason survives in the swap: %s", body)
	}
}

// Why: SetOption32..49 are one-byte numeric parameters in Tasmota (button hold
// time, bistable pulse time, ...). A 0/1 toggle made it impossible to require
// e.g. SetOption45 = 40; every other SetOption stays a flag.
func TestPolicyRuleSetOptionParametersAreNumbers(t *testing.T) {
	for _, c := range []struct{ cmd, kind string }{
		{"SetOption32", "number"}, {"SetOption45", "number"}, {"SetOption49", "number"},
		{"SetOption0", "toggle"}, {"SetOption31", "toggle"}, {"SetOption50", "toggle"}, {"SetOption114", "toggle"},
	} {
		vc := controlFor(c.cmd, "", false)
		if vc.Kind != c.kind {
			t.Errorf("%s: control %q, want %q", c.cmd, vc.Kind, c.kind)
		}
		lo, hi := 0, 255
		if c.cmd == "SetOption32" || c.cmd == "SetOption45" {
			lo, hi = 1, 250
		}
		if c.kind == "number" && (!vc.HasRange || vc.Min != lo || vc.Max != hi) {
			t.Errorf("%s: range %v %d..%d, want %d..%d", c.cmd, vc.HasRange, vc.Min, vc.Max, lo, hi)
		}
	}
	// A saved 40 is kept (it was a toggle-only 0/1 before).
	if vc := controlFor("SetOption45", "40", false); vc.Want != "40" {
		t.Errorf("SetOption45 40 kept as %q", vc.Want)
	}
}

// Why: the editor round trip for a numeric SetOption must save the number.
func TestPolicyRuleSetOptionParameterRoundTrip(t *testing.T) {
	e, _, _ := rulesEnv(t, "")
	saveRule(t, e, url.Values{"command": {"SetOption45"}, "want": {"40"}, "name": {"Bistable pulse"}})
	_, body := e.do(t, "GET", "/policy/rules/value-control?command=SetOption45&want=40", "admin", nil)
	for _, w := range []string{`type="number"`, `min="1"`, `max="250"`, `value="40"`} {
		if !strings.Contains(body, w) {
			t.Errorf("control lacks %q:\n%s", w, body)
		}
	}
}
