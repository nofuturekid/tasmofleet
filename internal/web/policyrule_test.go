package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
)

// rulePolicy is a PolicyService that judges the rules it is given the way the real one does
// (policy.Outcomes over fixed devices and values) and records what the rule pages ask of it.
type rulePolicy struct {
	mu      sync.Mutex
	values  map[model.DeviceID]map[string]string
	prov    map[model.DeviceID]map[string]ValueSource
	live    int      // Report calls with live=true
	limited bool     // Report answers like a rate-limited live check
	evals   int      // Evaluate calls
	reads   []string // ReadValue commands
	readVal string
	readSrc ValueSource
	readErr error
}

func (p *rulePolicy) devices() []model.Device {
	return []model.Device{
		{ID: 1, Name: "kitchen", Identity: model.Identity{Family: model.ChipESP8266}},
		{ID: 2, Name: "attic", Identity: model.Identity{Family: model.ChipESP32}},
		{ID: 3, Name: "shed", Identity: model.Identity{Family: model.ChipESP8266}},
	}
}

func (p *rulePolicy) Report(_ context.Context, _ string, live bool) (PolicyReport, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if live {
		p.live++
	}
	return PolicyReport{LiveThrottled: live && p.limited}, nil
}

func (p *rulePolicy) Evaluate(_ context.Context, rules []policy.Rule) (PolicyReport, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.evals++
	devs := p.devices()
	return PolicyReport{Rules: rules, Devices: devs, Values: p.values, Provenance: p.prov,
		Outcomes: policy.Outcomes(rules, devs, p.values)}, nil
}

func (p *rulePolicy) ReadValue(_ context.Context, _ string, _ model.DeviceID, command string) (string, ValueSource, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reads = append(p.reads, command)
	return p.readVal, p.readSrc, p.readErr
}

// ruleEnvWith is rulesEnv with a rulePolicy as the policy service.
func ruleEnvWith(t *testing.T, rp *rulePolicy) (*env, *rulesMem) {
	t.Helper()
	e, mem, _ := rulesEnvDeps(t, func(d *Deps) { d.Policy = rp })
	return e, mem
}

// ruleIDs reads the ids of the newest stored version, in list order.
func ruleIDs(t *testing.T, mem *rulesMem) []string {
	t.Helper()
	var specs []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(mem.vs[len(mem.vs)-1].Rules), &specs); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(specs))
	for i, s := range specs {
		ids[i] = s.ID
	}
	return ids
}

// ruleSeed stores a rule through the rule page's own form (id=new) and returns its id.
func ruleSeed(t *testing.T, e *env, mem *rulesMem, extra url.Values) string {
	t.Helper()
	f := ruleForm(extra)
	f.Set("id", "new")
	f.Set("version", vstr(int64(len(mem.vs))))
	r, body := e.do(t, "POST", "/policy/rules/save", "admin", f)
	if r.StatusCode != 303 {
		t.Fatalf("seed save: %d\n%s", r.StatusCode, body)
	}
	return ruleIDs(t, mem)[len(ruleIDs(t, mem))-1]
}

func defaultRP() *rulePolicy {
	now := time.Now()
	return &rulePolicy{
		values: map[model.DeviceID]map[string]string{
			1: {"savedata": "1"}, // deviates
			2: {"savedata": "0"}, // matches
			// shed: no value at all -> not judged
		},
		prov: map[model.DeviceID]map[string]ValueSource{
			1: {"savedata": {Kind: "live", At: now.Add(-6 * time.Minute)}},
			2: {"savedata": {Kind: "backup", At: now.Add(-48 * time.Hour)}},
		},
	}
}

// Why: a deep link /policy/rules/{id} must keep naming the same rule after edits, and the page
// must say what the rule does to each device: Deviate and Not judged are never "match".
func TestRulePageShowsTheRuleAndWhatItDoesToEachDevice(t *testing.T) {
	e, mem := ruleEnvWith(t, defaultRP())
	id := ruleSeed(t, e, mem, url.Values{"family": {}, "tag": {}, "tags_extra": {""}, "except": {}})
	r, body := e.do(t, "GET", "/policy/rules/"+id, "admin", nil)
	if r.StatusCode != 200 {
		t.Fatalf("status %d\n%s", r.StatusCode, body)
	}
	for _, w := range []string{"Flash wear", "SaveData = 0", "version 1", `name="id" value="` + id + `"`,
		"live · 6 min", "backup · ", "no backup yet", "Saving creates a new version"} {
		if !strings.Contains(body, w) {
			t.Errorf("rule page lacks %q", w)
		}
	}
	devs := section(t, body, "dc-devs")
	k, a, s := strings.Index(devs, "kitchen"), strings.Index(devs, "attic"), strings.Index(devs, "shed")
	if k < 0 || k >= s || s >= a {
		t.Errorf("deviating first, then not judged, then matching (kitchen, shed, attic):\n%s", devs)
	}
	// tiles: applies 3, match 1, deviate 1, not judged 1
	for _, w := range []string{`data-tile="applies"`, `data-tile="match"`, `data-tile="deviate"`, `data-tile="unknown"`, "required"} {
		if !strings.Contains(body, w) {
			t.Errorf("tile missing %q", w)
		}
	}
	if !strings.Contains(body, "33 % of the devices it applies to") {
		t.Errorf("match tile must say the share of the devices the rule applies to:\n%s", body)
	}
}

// Why: a link to a rule that no longer exists must end in a 404, not in somebody else's rule
// (positions shift, ids do not), on both rule routes.
func TestRulePageUnknownIDIs404(t *testing.T) {
	e, mem := ruleEnvWith(t, defaultRP())
	ruleSeed(t, e, mem, nil)
	for _, p := range []string{"/policy/rules/rzzzzzzzzzz", "/policy/rules/r9", "/policy/rules/NOPE"} {
		if r, _ := e.do(t, "GET", p, "admin", nil); r.StatusCode != 404 {
			t.Errorf("GET %s = %d, want 404", p, r.StatusCode)
		}
	}
	f := ruleForm(url.Values{"version": {"1"}})
	f.Set("id", "rzzzzzzzzzz")
	if r, _ := e.do(t, "POST", "/policy/rules/save", "admin", f); r.StatusCode != 404 {
		t.Errorf("save of an unknown id = %d, want 404", r.StatusCode)
	}
}

// Why: deep links must survive edits. The rule page's form sends the rule's id, so saving it
// keeps the id, lands on the same URL and changes the right rule even when another rule moved
// in front of it meanwhile.
func TestRulePageSaveKeepsTheIDAndNamesItForTheOverview(t *testing.T) {
	e, mem := ruleEnvWith(t, defaultRP())
	first := ruleSeed(t, e, mem, url.Values{"name": {"First"}, "command": {"TelePeriod"}, "want": {"300"}})
	second := ruleSeed(t, e, mem, url.Values{"name": {"Second"}})
	e.do(t, "POST", "/policy/rules/order", "admin", url.Values{"csrf": {"tok"}, "version": {vstr(int64(len(mem.vs)))}, "order": {"1,0"}})
	if ids := ruleIDs(t, mem); ids[0] != second || ids[1] != first {
		t.Fatalf("reorder moved the ids with the rules: %v", ids)
	}
	f := ruleForm(url.Values{"name": {"Second renamed"}, "want": {"1"}, "version": {vstr(int64(len(mem.vs)))}})
	f.Set("id", second)
	r, body := e.do(t, "POST", "/policy/rules/save", "admin", f)
	if r.StatusCode != 303 || r.Header.Get("Location") != "/tf/policy?ok=saved&rule="+second {
		t.Fatalf("save: %d %q\n%s", r.StatusCode, r.Header.Get("Location"), body)
	}
	ids := ruleIDs(t, mem)
	if ids[0] != second || ids[1] != first {
		t.Errorf("ids after the edit = %v, want [%s %s]", ids, second, first)
	}
	if !strings.Contains(mem.vs[len(mem.vs)-1].Rules, "Second renamed") || strings.Count(mem.vs[len(mem.vs)-1].Rules, "First") != 1 {
		t.Errorf("the edit hit the wrong rule: %s", mem.vs[len(mem.vs)-1].Rules)
	}
}

// Why: a new rule's id exists only after it is stored; the save names that id for the overview
// to point at, and the default change note says where the rule came from.
func TestRuleNewSaveRedirectsToTheNewID(t *testing.T) {
	e, mem := ruleEnvWith(t, defaultRP())
	f := ruleForm(url.Values{"name": {"Telemetry every 5 minutes"}, "command": {"TelePeriod"}, "want": {"300"}, "origin": {"template:telemetry"}})
	f.Set("id", "new")
	f.Set("version", "0")
	r, body := e.do(t, "POST", "/policy/rules/save", "admin", f)
	if len(mem.vs) != 1 {
		t.Fatalf("versions = %d\n%s", len(mem.vs), body)
	}
	id := ruleIDs(t, mem)[0]
	if r.StatusCode != 303 || r.Header.Get("Location") != "/tf/policy?ok=saved&rule="+id {
		t.Errorf("save: %d %q (new id %s)", r.StatusCode, r.Header.Get("Location"), id)
	}
	if mem.vs[0].Note != "from template: Telemetry every 5 minutes" {
		t.Errorf("note = %q", mem.vs[0].Note)
	}
}

// Why: opening a template, a command or a copy only shows a draft; the rules change only when
// somebody presses Save. A GET that stored something would turn a mis-click into a policy change.
func TestRuleNewDraftsAreNeverSavedByGet(t *testing.T) {
	rp := defaultRP()
	rp.readVal = "+01:00"
	rp.readSrc = ValueSource{Kind: "live", At: time.Now()}
	e, mem := ruleEnvWith(t, rp)
	for _, p := range []string{"/policy/rules/new", "/policy/rules/new?template=telemetry", "/policy/rules/new?command=SetOption45", "/policy/rules/new?q=time"} {
		r, body := e.do(t, "GET", p, "admin", nil)
		if r.StatusCode != 200 {
			t.Errorf("GET %s = %d\n%s", p, r.StatusCode, body)
		}
	}
	r, body := e.do(t, "GET", "/policy/rules/new?template=telemetry", "admin", nil)
	for _, w := range []string{"TelePeriod", `value="300"`, `name="id" value="new"`, `name="origin" value="template:telemetry"`, "Nothing is saved until you press Save rule", "Draft"} {
		if !strings.Contains(body, w) {
			t.Errorf("draft lacks %q (%d)", w, r.StatusCode)
		}
	}
	if r, _ := e.do(t, "POST", "/policy/rules/new/copy", "admin", url.Values{"csrf": {"tok"}, "device": {"1"}, "command": {"Timezone"}}); r.StatusCode != 200 {
		t.Errorf("copy = %d", r.StatusCode)
	}
	if len(mem.vs) != 0 {
		t.Fatalf("a draft stored %d versions", len(mem.vs))
	}
	if r, _ := e.do(t, "GET", "/policy/rules/new?template=nope", "admin", nil); r.StatusCode != 404 {
		t.Errorf("unknown template = %d, want 404", r.StatusCode)
	}
}

// Why: a template whose command and scope are already a rule must say so, or the person adds a
// duplicate that the lower rule then shadows.
func TestRuleNewMarksTemplatesThatAreAlreadyRules(t *testing.T) {
	e, mem := ruleEnvWith(t, defaultRP())
	ruleSeed(t, e, mem, url.Values{"name": {"Local timezone"}, "command": {"Timezone"}, "want": {"99"}, "family": {}, "tag": {}, "tags_extra": {""}, "except": {}})
	_, body := e.do(t, "GET", "/policy/rules/new", "admin", nil)
	tz := body[strings.Index(body, "?template=timezone"):]
	tz = tz[:strings.Index(tz, "</a>")]
	if !strings.Contains(tz, "Already a rule") {
		t.Errorf("the timezone template is already a rule:\n%s", tz)
	}
	tp := body[strings.Index(body, "?template=telemetry"):]
	tp = tp[:strings.Index(tp, "</a>")]
	if strings.Contains(tp, "Already a rule") {
		t.Errorf("the telemetry template is not a rule yet:\n%s", tp)
	}
	// a rule for one chip family is not the template's whole-fleet rule
	e2, mem2 := ruleEnvWith(t, defaultRP())
	ruleSeed(t, e2, mem2, url.Values{"command": {"Timezone"}, "want": {"99"}, "family": {"esp8266"}, "tag": {}, "tags_extra": {""}, "except": {}})
	_, body2 := e2.do(t, "GET", "/policy/rules/new", "admin", nil)
	tz2 := body2[strings.Index(body2, "?template=timezone"):]
	if strings.Contains(tz2[:strings.Index(tz2, "</a>")], "Already a rule") {
		t.Error("a rule with another scope is not the same rule")
	}
}

// Why: the template search must work without JavaScript (the server filters on ?q=).
func TestRuleNewSearchFiltersTheTemplates(t *testing.T) {
	e, _ := ruleEnvWith(t, defaultRP())
	_, body := e.do(t, "GET", "/policy/rules/new?q=timezone", "admin", nil)
	if !strings.Contains(body, "?template=timezone") || strings.Contains(body, "?template=telemetry") {
		t.Errorf("q=timezone must keep only the timezone template:\n%s", body)
	}
	_, none := e.do(t, "GET", "/policy/rules/new?q=zzzz", "admin", nil)
	if !strings.Contains(none, "No template matches") {
		t.Error("an empty search says so")
	}
}

// Why: a command that must never be sent to a device is refused with its reason before
// anything else, on both entry points, and the device is not asked.
func TestRuleNewRefusesCommandsWithTheReasonAndSendsNothing(t *testing.T) {
	rp := defaultRP()
	e, mem := ruleEnvWith(t, rp)
	for _, cmd := range []string{"Status0", "Password1", "Restart"} {
		r, body := e.do(t, "GET", "/policy/rules/new?command="+cmd, "admin", nil)
		if r.StatusCode != 422 || !strings.Contains(body, `role="alert"`) {
			t.Errorf("any command %s: %d", cmd, r.StatusCode)
		}
		r, body = e.do(t, "POST", "/policy/rules/new/copy", "admin", url.Values{"csrf": {"tok"}, "device": {"1"}, "command": {cmd}})
		if r.StatusCode != 422 || !strings.Contains(body, `id="err-copy"`) {
			t.Errorf("copy %s: %d\n%s", cmd, r.StatusCode, body)
		}
	}
	if len(rp.reads) != 0 {
		t.Errorf("a refused command reached ReadValue (a device read): %v", rp.reads)
	}
	if len(mem.vs) != 0 {
		t.Error("nothing is stored")
	}
}

// Why: Copy from a device proposes the device's own value as the expected value and says where
// it came from; a refusal of ReadValue (credentials, no answer) shows its reason and no draft.
func TestRuleCopyOpensADraftWithTheValueAndItsSource(t *testing.T) {
	rp := defaultRP()
	rp.readVal, rp.readSrc = "+01:00", ValueSource{Kind: "live", At: time.Now()}
	e, mem := ruleEnvWith(t, rp)
	r, body := e.do(t, "POST", "/policy/rules/new/copy", "admin", url.Values{"csrf": {"tok"}, "device": {"1"}, "command": {"Timezone"}})
	if r.StatusCode != 200 {
		t.Fatalf("copy = %d\n%s", r.StatusCode, body)
	}
	for _, w := range []string{`value="+01:00"`, "Copied from kitchen · read live now", `name="origin" value="copy:1"`} {
		if !strings.Contains(body, w) {
			t.Errorf("draft lacks %q", w)
		}
	}
	rp.readSrc = ValueSource{Kind: "backup", At: time.Date(2026, 10, 28, 9, 0, 0, 0, time.Local)}
	_, body = e.do(t, "POST", "/policy/rules/new/copy", "admin", url.Values{"csrf": {"tok"}, "device": {"1"}, "command": {"Timezone"}})
	if !strings.Contains(body, "Copied from kitchen · backup 28 Oct") {
		t.Errorf("a backup value says it is one:\n%s", body)
	}
	rp.readErr = &ValueRefusedError{Reason: "the value contains credentials; enter it by hand"}
	r, body = e.do(t, "POST", "/policy/rules/new/copy", "admin", url.Values{"csrf": {"tok"}, "device": {"1"}, "command": {"MqttHost"}})
	if r.StatusCode != 422 || !strings.Contains(body, "The value contains credentials; enter it by hand.") {
		t.Errorf("refusal: %d\n%s", r.StatusCode, body)
	}
	rp.readErr = errors.New("the device did not answer")
	if r, _ := e.do(t, "POST", "/policy/rules/new/copy", "admin", url.Values{"csrf": {"tok"}, "device": {"1"}, "command": {"Timezone"}}); r.StatusCode != 422 {
		t.Errorf("no answer = %d, want 422", r.StatusCode)
	}
	if r, _ := e.do(t, "POST", "/policy/rules/new/copy", "admin", url.Values{"csrf": {"tok"}, "device": {"99"}, "command": {"Timezone"}}); r.StatusCode != 404 {
		t.Errorf("unknown device = %d, want 404", r.StatusCode)
	}
	if len(mem.vs) != 0 {
		t.Error("nothing is stored")
	}
}

// Why: a firmware URL fits one chip family; copying OtaUrl from an ESP8266 must not propose it
// for the whole fleet (other commands default to all devices).
func TestRuleCopyOtaUrlDefaultsToTheSourceChipFamily(t *testing.T) {
	rp := defaultRP()
	rp.readVal, rp.readSrc = "http://ota.example/tasmota.bin", ValueSource{Kind: "live", At: time.Now()}
	e, _ := ruleEnvWith(t, rp)
	_, body := e.do(t, "POST", "/policy/rules/new/copy", "admin", url.Values{"csrf": {"tok"}, "device": {"1"}, "command": {"OtaUrl"}})
	if !strings.Contains(body, `name="family" value="esp8266" checked`) || !strings.Contains(body, "OtaUrl = http://ota.example/tasmota.bin · ESP8266") {
		t.Errorf("OtaUrl copied from an ESP8266 applies to that family:\n%s", body)
	}
	rp.readVal = "+01:00"
	_, body = e.do(t, "POST", "/policy/rules/new/copy", "admin", url.Values{"csrf": {"tok"}, "device": {"1"}, "command": {"Timezone"}})
	if !strings.Contains(body, "All devices") {
		t.Errorf("another command applies to all devices:\n%s", body)
	}
}

// Why: two admins editing at once. A save made on an old version must not overwrite the newer
// rules; the person sees the rule as it is now, with the reason.
func TestRulePageStaleVersionShowsTheRuleWithABanner(t *testing.T) {
	e, mem := ruleEnvWith(t, defaultRP())
	id := ruleSeed(t, e, mem, nil)
	f := ruleForm(url.Values{"name": {"Late edit"}, "version": {"0"}})
	f.Set("id", id)
	r, body := e.do(t, "POST", "/policy/rules/save", "admin", f)
	if r.StatusCode != 409 || !strings.Contains(body, "Nothing was saved. The rules were changed by someone else") {
		t.Fatalf("stale save: %d\n%s", r.StatusCode, body)
	}
	if !strings.Contains(body, "Flash wear") || strings.Contains(body, "Late edit") {
		t.Error("the page shows the saved rule, not the refused draft")
	}
	if len(mem.vs) != 1 {
		t.Errorf("stale save stored a version: %d", len(mem.vs))
	}
}

// Why: a refused draft opens again with the problem at its field and the person's input kept.
func TestRulePageInvalidDraftShowsTheProblemAtTheField(t *testing.T) {
	e, mem := ruleEnvWith(t, defaultRP())
	id := ruleSeed(t, e, mem, nil)
	f := ruleForm(url.Values{"name": {"Edited"}, "command": {"Status0"}, "version": {"1"}})
	f.Set("id", id)
	r, body := e.do(t, "POST", "/policy/rules/save", "admin", f)
	if r.StatusCode != 422 || !strings.Contains(body, `id="err-command"`) || !strings.Contains(body, `value="Edited"`) {
		t.Errorf("status %d\n%s", r.StatusCode, body)
	}
	if len(mem.vs) != 1 {
		t.Error("nothing is stored")
	}
}

// Why: a rule is named by its id only. The position field of the old editor is gone: a form that
// still sends it is refused (a position names whatever rule sits there now), and so is a bad origin.
func TestRuleSaveRefusesTheOldIdxField(t *testing.T) {
	e, mem := ruleEnvWith(t, defaultRP())
	id := ruleSeed(t, e, mem, nil)
	f := ruleForm(url.Values{"version": {"1"}, "idx": {"0"}})
	f.Set("id", id)
	if r, _ := e.do(t, "POST", "/policy/rules/save", "admin", f); r.StatusCode != 400 {
		t.Errorf("id and idx = %d, want 400", r.StatusCode)
	}
	f = ruleForm(url.Values{"version": {"1"}, "origin": {"javascript:x"}})
	f.Set("id", id)
	if r, _ := e.do(t, "POST", "/policy/rules/save", "admin", f); r.StatusCode != 400 {
		t.Errorf("bad origin = %d, want 400", r.StatusCode)
	}
}

// Why: the devices card re-draws from the draft while it is edited, without a device read and
// without storing anything: that is what makes "updates as you edit" safe.
func TestRulePreviewJudgesTheDraftFromCachedValuesOnly(t *testing.T) {
	rp := defaultRP()
	e, mem := ruleEnvWith(t, rp)
	id := ruleSeed(t, e, mem, url.Values{"family": {}, "tag": {}, "tags_extra": {""}, "except": {}})
	f := ruleForm(url.Values{"want": {"1"}, "family": {}, "tag": {}, "tags_extra": {""}, "except": {}})
	f.Set("id", id)
	rp.live, rp.reads = 0, nil
	r, body := e.do(t, "POST", "/policy/rules/preview?show=deviate", "admin", f)
	if r.StatusCode != 200 || !strings.Contains(body, `id="rule-devices"`) {
		t.Fatalf("preview: %d\n%s", r.StatusCode, body)
	}
	// the draft expects 1: attic (0) now deviates, kitchen (1) matches
	if !strings.Contains(body, "attic") || strings.Contains(body, "kitchen") {
		t.Errorf("filter deviate with the draft want=1 lists attic only:\n%s", body)
	}
	if rp.live != 0 || len(rp.reads) != 0 {
		t.Errorf("preview talked to devices: live=%d reads=%v", rp.live, rp.reads)
	}
	if len(mem.vs) != 1 {
		t.Error("preview stored a version")
	}
	// an incomplete draft says what is missing instead of failing
	f.Set("want", "")
	_, body = e.do(t, "POST", "/policy/rules/preview", "admin", f)
	if !strings.Contains(body, "Enter a command and an expected value") {
		t.Errorf("incomplete draft:\n%s", body)
	}
}

// Why: Check now is the fleet's live check; the rule page asks for it and comes back to itself
// so the numbers it shows are the fresh ones. Operators may run it (as on the policy page).
func TestRuleCheckNowRunsTheLiveCheckAndComesBack(t *testing.T) {
	rp := defaultRP()
	e, mem := ruleEnvWith(t, rp)
	id := ruleSeed(t, e, mem, nil)
	rp.live = 0
	for _, role := range []string{"operator", "admin"} {
		r, _ := e.do(t, "POST", "/policy/live", role, url.Values{"csrf": {"tok"}, "rule": {id}})
		if r.StatusCode != 303 || r.Header.Get("Location") != "/tf/policy/rules/"+id+"?ok=checked" {
			t.Errorf("%s: %d %q", role, r.StatusCode, r.Header.Get("Location"))
		}
	}
	if rp.live != 2 {
		t.Errorf("live checks = %d, want 2", rp.live)
	}
	if r, _ := e.do(t, "POST", "/policy/live", "admin", url.Values{"csrf": {"tok"}, "rule": {"bad id"}}); r.StatusCode != 400 {
		t.Errorf("bad rule = %d, want 400", r.StatusCode)
	}
	_, body := e.do(t, "GET", "/policy/rules/"+id+"?ok=checked", "operator", nil)
	if !strings.Contains(body, "Live check finished") {
		t.Error("the page confirms the check")
	}
}

// Why: a disabled rule is not checked, so Check now is off and says why; the numbers still show
// what it would judge, marked as such.
func TestRulePageDisabledRuleChecksNothing(t *testing.T) {
	e, mem := ruleEnvWith(t, defaultRP())
	id := ruleSeed(t, e, mem, url.Values{"family": {}, "tag": {}, "tags_extra": {""}, "except": {}})
	r, body := e.do(t, "POST", "/policy/rules/disable", "admin", url.Values{"csrf": {"tok"}, "version": {"1"}, "id": {id}, "back": {"rule"}})
	if r.StatusCode != 303 || r.Header.Get("Location") != "/tf/policy/rules/"+id+"?ok=disabled" {
		t.Fatalf("disable with back=rule: %d %q\n%s", r.StatusCode, r.Header.Get("Location"), body)
	}
	_, body = e.do(t, "GET", "/policy/rules/"+id, "admin", nil)
	for _, w := range []string{"Disabled", `disabled aria-describedby="chk-why"`, "Not checked while the rule is disabled", "if it were enabled", ">Enable<"} {
		if !strings.Contains(body, w) {
			t.Errorf("disabled rule page lacks %q", w)
		}
	}
}

// Why: operators look but never change: the page is the rule as read-only rows, with Check now
// as the only action, and the admin-only routes answer 403.
func TestRulePageOperatorIsReadOnly(t *testing.T) {
	e, mem := ruleEnvWith(t, defaultRP())
	id := ruleSeed(t, e, mem, nil)
	r, body := e.do(t, "GET", "/policy/rules/"+id, "operator", nil)
	if r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	for _, bad := range []string{`name="want"`, `name="command"`, "/policy/rules/save", "/policy/rules/delete", "/policy/rules/duplicate", "/policy/rules/disable", "Delete…", "Save rule", "hx-post"} {
		if strings.Contains(body, bad) {
			t.Errorf("operator page renders a write control: %q", bad)
		}
	}
	for _, w := range []string{"Check now", "Flash wear", "SaveData"} {
		if !strings.Contains(body, w) {
			t.Errorf("operator page lacks %q", w)
		}
	}
	for _, p := range []string{"/policy/rules/new", "/policy/rules/new?template=telemetry"} {
		if r, _ := e.do(t, "GET", p, "operator", nil); r.StatusCode != 403 {
			t.Errorf("GET %s as operator = %d, want 403", p, r.StatusCode)
		}
	}
	if r, _ := e.do(t, "POST", "/policy/rules/new/copy", "operator", url.Values{"csrf": {"tok"}, "device": {"1"}, "command": {"Timezone"}}); r.StatusCode != 403 {
		t.Errorf("copy as operator = %d, want 403", r.StatusCode)
	}
}

// Why: a rule that applies to no device, and a fleet without devices, are different situations
// and must not both show an empty table.
func TestRulePageEmptyStates(t *testing.T) {
	e, mem := ruleEnvWith(t, defaultRP())
	id := ruleSeed(t, e, mem, url.Values{"family": {"esp32c3"}, "tag": {}, "tags_extra": {""}, "except": {}})
	_, body := e.do(t, "GET", "/policy/rules/"+id, "admin", nil)
	if !strings.Contains(body, "This rule applies to no device") {
		t.Errorf("a rule for a chip family nobody has:\n%s", section(t, body, "dc-devs"))
	}
	empty := &rulePolicy{}
	e2, mem2, _ := rulesEnvDeps(t, func(d *Deps) { d.Policy = &noDevices{rulePolicy: empty} })
	id2 := ruleSeed(t, e2, mem2, nil)
	_, body = e2.do(t, "GET", "/policy/rules/"+id2, "admin", nil)
	if !strings.Contains(body, "No devices yet") {
		t.Errorf("a fleet without devices:\n%s", section(t, body, "dc-devs"))
	}
}

type noDevices struct{ *rulePolicy }

func (n *noDevices) Evaluate(_ context.Context, rules []policy.Rule) (PolicyReport, error) {
	return PolicyReport{Rules: rules}, nil
}

// Why: a refused save of a new rule must not lose where the draft came from, or the default
// change note ("from template: X") silently disappears on the retry.
func TestRuleNewRefusedSaveKeepsTheOrigin(t *testing.T) {
	e, _ := ruleEnvWith(t, defaultRP())
	f := ruleForm(url.Values{"command": {"Status0"}, "origin": {"template:telemetry"}})
	f.Set("id", "new")
	f.Set("version", "0")
	r, body := e.do(t, "POST", "/policy/rules/save", "admin", f)
	if r.StatusCode != 422 || !strings.Contains(body, `name="origin" value="template:telemetry"`) {
		t.Errorf("status %d, origin lost:\n%s", r.StatusCode, body)
	}
}

// Why: a rate-limited live check ran nothing new; saying "finished" would pass old numbers off
// as fresh ones.
func TestRuleCheckNowSaysWhenTheCheckWasRateLimited(t *testing.T) {
	rp := defaultRP()
	e, mem := ruleEnvWith(t, rp)
	id := ruleSeed(t, e, mem, nil)
	rp.limited = true
	r, _ := e.do(t, "POST", "/policy/live", "admin", url.Values{"csrf": {"tok"}, "rule": {id}})
	if r.Header.Get("Location") != "/tf/policy/rules/"+id+"?ok=throttled" {
		t.Fatalf("location %q", r.Header.Get("Location"))
	}
	_, body := e.do(t, "GET", "/policy/rules/"+id+"?ok=throttled", "admin", nil)
	if !strings.Contains(body, "Live check rate-limited, try again in a moment") {
		t.Error("the page says the check was rate-limited")
	}
}

// Why: a new rule exists only in the form; a stale save must show the typed draft again (on the
// current version) instead of throwing it away.
func TestRuleNewStaleSaveKeepsTheTypedDraft(t *testing.T) {
	e, mem := ruleEnvWith(t, defaultRP())
	ruleSeed(t, e, mem, nil)
	f := ruleForm(url.Values{"name": {"Typed by hand"}, "command": {"TelePeriod"}, "want": {"60"}})
	f.Set("id", "new")
	f.Set("version", "0")
	r, body := e.do(t, "POST", "/policy/rules/save", "admin", f)
	if r.StatusCode != 409 || !strings.Contains(body, "The rules were changed by someone else") {
		t.Fatalf("status %d", r.StatusCode)
	}
	for _, w := range []string{`value="Typed by hand"`, `value="60"`, `name="version" value="1"`} {
		if !strings.Contains(body, w) {
			t.Errorf("stale draft lacks %q", w)
		}
	}
	if len(mem.vs) != 1 {
		t.Error("nothing is stored")
	}
}

// Why: Cancel ends the editor. The rule page always shows the editor to admins, so a Cancel
// that pointed at the rule page itself only reloaded it, and a draft's Cancel led to the
// template picker; both now go back to the policy overview, where the user came from.
func TestRuleEditorCancelGoesBackToTheOverview(t *testing.T) {
	e, mem := ruleEnvWith(t, defaultRP())
	id := ruleSeed(t, e, mem, url.Values{"family": {}, "tag": {}, "tags_extra": {""}, "except": {}})
	for _, p := range []string{"/policy/rules/" + id, "/policy/rules/new?template=telemetry"} {
		_, body := e.do(t, "GET", p, "admin", nil)
		if !strings.Contains(body, `href="/tf/policy">Cancel</a>`) {
			t.Errorf("%s: Cancel does not lead to the overview", p)
		}
	}
}

// Why: Save ends the editor like Cancel does (product owner, 2026-10-30): the overview says which
// rule was saved and marks its row, so its compliance bar is the first thing the user sees.
func TestRuleSaveLandsOnTheOverviewWithTheRuleMarked(t *testing.T) {
	e, mem := ruleEnvWith(t, defaultRP())
	id := ruleSeed(t, e, mem, url.Values{"name": {"Flash wear"}})
	_, body := e.do(t, "GET", "/policy?ok=saved&rule="+id, "admin", nil)
	if !strings.Contains(body, "Rule “Flash wear” saved") {
		t.Error("the flash does not name the saved rule")
	}
	if strings.Count(body, `class="is-saved"`) != 1 {
		t.Error("the saved rule's row is not marked exactly once")
	}
	_, body = e.do(t, "GET", "/policy?ok=saved&rule=rnope", "admin", nil)
	if !strings.Contains(body, "Rule saved") || strings.Contains(body, `class="is-saved"`) {
		t.Error("an unknown rule id must fall back to the plain flash and mark nothing")
	}
}
