package web

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// tileValue is the big number of the tile data-tile=key.
func tileValue(t *testing.T, body, key string) string {
	t.Helper()
	m := regexp.MustCompile(`data-tile="` + key + `"><div class="dev-tile-l">[^<]*</div><div class="dev-tile-v[^"]*">([^<]*)</div><div class="dev-tile-s">([^<]*)</div>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no tile %q in\n%s", key, body)
	}
	return m[1] + " | " + m[2]
}

// fleetReport is five devices under three rules, chosen so that every tile rule of D3 has a
// device that would be counted wrongly by a naive count:
//
//	alpha   matches everything
//	bravo   deviates from the Required rule
//	charlie deviates only from the Advisory rule (still compliant: a hint is not a breach)
//	delta   cannot be judged for the Required rule (not compliant, not deviating: not judged)
//	echo    deviates from the Advisory rule and cannot be judged for the Required one (it
//	        deviates, so it is not "not judged")
//
// The third rule is disabled and has no outcomes.
func fleetReport() PolicyReport {
	rules := []policy.Rule{
		{ID: "r1", Name: "OtaUrl for ESP8266", Command: "OtaUrl", Want: "http://fw/x.bin"},
		{ID: "r2", Name: "Local timezone", Command: "Timezone", Want: "99", Severity: policy.SeverityInfo},
		{ID: "r3", Name: "Telemetry every 5 minutes", Command: "TelePeriod", Want: "300", Severity: policy.SeverityInfo, Disabled: true},
	}
	o := func(dev int, idx int, k policy.OutcomeKind) policy.Outcome {
		return policy.Outcome{DeviceID: model.DeviceID(dev), RuleIndex: idx, RuleID: rules[idx].ID, Kind: k}
	}
	return PolicyReport{
		Rules:   rules,
		Devices: []model.Device{{ID: 1, Name: "alpha"}, {ID: 2, Name: "bravo"}, {ID: 3, Name: "charlie"}, {ID: 4, Name: "delta"}, {ID: 5, Name: "echo"}},
		Outcomes: []policy.Outcome{
			o(1, 0, policy.OutcomeMatch), o(2, 0, policy.OutcomeDeviate), o(3, 0, policy.OutcomeMatch), o(4, 0, policy.OutcomeUnknown), o(5, 0, policy.OutcomeUnknown),
			o(1, 1, policy.OutcomeMatch), o(2, 1, policy.OutcomeMatch), o(3, 1, policy.OutcomeDeviate), o(4, 1, policy.OutcomeMatch), o(5, 1, policy.OutcomeDeviate),
		},
	}
}

// Why: the tiles decide what an admin fixes first, so each follows its definition and not the
// obvious count: a device with only an Advisory deviation is still compliant, an unjudged device
// is not, and a device that deviates is never also "not judged". A disabled rule is not counted.
func TestPolicyOverviewTilesFollowTheDefinitions(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.Policy = &fakePolicy{rep: fleetReport()} })
	_, body := e.do(t, "GET", "/policy", "operator", nil)
	for key, want := range map[string]string{
		"rules":     "3 | 2 enabled · 1 disabled",
		"compliant": "2 / 5 | all required rules met",
		"deviating": "3 | 1 required · 2 advisory",
		"unjudged":  "1 | no backup or recent reading",
	} {
		if got := tileValue(t, body, key); got != want {
			t.Errorf("tile %s = %q, want %q", key, got, want)
		}
	}
	rows := regexp.MustCompile(`(?s)<tr class[^>]*>.*?</tr>`).FindAllString(body, -1)
	if len(rows) != 3 {
		t.Fatalf("%d rows, want 3", len(rows))
	}
	for i, want := range []string{"2 / 5 · 1 deviate · 2 ?", "3 / 5 · 2 deviate", "not checked while disabled"} {
		if !strings.Contains(rows[i], want) {
			t.Errorf("row %d lacks %q:\n%s", i, want, rows[i])
		}
	}
	if !strings.Contains(rows[2], "is-dim") || strings.Contains(rows[0], "is-dim") || strings.Contains(rows[2], "health-bar") {
		t.Error("only the disabled rule is dimmed, and it draws no bar")
	}
	if !strings.Contains(rows[0], `aria-label="2 match, 1 deviate, 2 not judged"`) {
		t.Errorf("the bar says its numbers:\n%s", rows[0])
	}
}

// Why: the banner is the shortcut to Make compliant; it must name only devices that break a
// Required rule (charlie only has a hint) and say which rule.
func TestPolicyOverviewBannerNamesRequiredDeviationsOnly(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.Policy = &fakePolicy{rep: fleetReport()} })
	_, body := e.do(t, "GET", "/policy", "operator", nil)
	if !strings.Contains(body, `1 device deviates from a required rule: <span class="mono">bravo</span> (OtaUrl for ESP8266)`) {
		t.Errorf("banner:\n%s", body)
	}
	if strings.Contains(body, `<span class="mono">charlie`) || strings.Contains(body, "echo</span>") {
		t.Error("advisory deviations do not make the banner")
	}
	rep := fleetReport()
	rep.Outcomes = rep.Outcomes[5:] // only the Advisory rule has outcomes
	e = setupWith(t, func(d *Deps) { d.Policy = &fakePolicy{rep: rep} })
	if _, body = e.do(t, "GET", "/policy", "operator", nil); strings.Contains(body, "deviates from a required rule") {
		t.Error("no Required deviation, no banner")
	}
}

// Why: old bookmarks (?edit=2, ?new=1) are not redirected (no compatibility shims), but they must
// not turn the overview into an error either: unknown parameters are ignored and the overview
// renders. An unreadable ?v= is ignored too.
func TestPolicyOverviewIgnoresUnknownParameters(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.Policy = &fakePolicy{rep: fleetReport()} })
	for _, q := range []string{"?edit=2", "?edit=x", "?new=1", "?v=abc", "?v=-4", "?foo=bar&show=all"} {
		r, body := e.do(t, "GET", "/policy"+q, "operator", nil)
		if r.StatusCode != 200 || !strings.Contains(body, `data-tile="rules"`) || strings.Contains(body, `name="command"`) {
			t.Errorf("GET /policy%s = %d, want the overview", q, r.StatusCode)
		}
	}
}

// Why: Move up and Move down are how the order changes on a phone and without JavaScript, so
// they sit in the row menu, name the rule by id, and the first rule has no Up, the last no Down.
func TestPolicyOverviewMoveEntriesWorkWithoutJavaScript(t *testing.T) {
	e, mem, _ := rulesEnv(t, "")
	threeRules(t, e)
	ids := ruleIDs(t, mem)
	_, body := e.do(t, "GET", "/policy", "admin", nil)
	rows := regexp.MustCompile(`(?s)<tr class[^>]*>.*?</tr>`).FindAllString(body, -1)
	if len(rows) != 3 {
		t.Fatalf("%d rows", len(rows))
	}
	if strings.Contains(rows[0], `value="up"`) || !strings.Contains(rows[0], `value="down"`) ||
		!strings.Contains(rows[1], `value="up"`) || !strings.Contains(rows[1], `value="down"`) ||
		!strings.Contains(rows[2], `value="up"`) || strings.Contains(rows[2], `value="down"`) {
		t.Error("the first rule cannot move up, the last cannot move down")
	}
	if !strings.Contains(rows[1], `name="id" value="`+ids[1]+`"`) {
		t.Error("the move form must name the rule by id")
	}
	if !strings.Contains(rows[0], `draggable="true"`) || !strings.Contains(rows[0], "⋮⋮") {
		t.Error("an admin can drag a row")
	}
}

// validOnlyPolicy reports on the rules that validate (A = r1, C = r3), as the app does: the broken
// stored rule (r2) never reaches the evaluation.
type validOnlyPolicy struct{ noEval }

func (validOnlyPolicy) Report(context.Context, string, bool) (PolicyReport, error) {
	rules := []policy.Rule{{ID: "r1", Name: "A", Command: "SaveData", Want: "0"}, {ID: "r3", Name: "C", Command: "Timezone", Want: "99"}}
	return PolicyReport{
		Rules:   rules,
		Devices: []model.Device{{ID: 1, Name: "alpha"}, {ID: 2, Name: "bravo"}},
		Outcomes: []policy.Outcome{
			{DeviceID: 1, RuleIndex: 0, RuleID: "r1", Kind: policy.OutcomeMatch}, {DeviceID: 2, RuleIndex: 0, RuleID: "r1", Kind: policy.OutcomeMatch},
			{DeviceID: 1, RuleIndex: 1, RuleID: "r3", Kind: policy.OutcomeMatch}, {DeviceID: 2, RuleIndex: 1, RuleID: "r3", Kind: policy.OutcomeDeviate},
		},
	}, nil
}

// Why: one bad rule must not make the fleet look compliant. A legacy list holds PowerOnState = 7
// (the editor's value check refuses it) between two valid rules. The overview flags that row and
// counts the valid rules only; the pages of the valid rules still show their numbers instead of
// refusing because "another saved rule is invalid".
func TestPolicyOverviewOneBrokenRuleDoesNotHideTheOthers(t *testing.T) {
	e, mem, _ := rulesEnvDeps(t, func(d *Deps) { d.Policy = validOnlyPolicy{} })
	mem.vs = append(mem.vs, store.PolicyRuleVersion{ID: 1, Actor: "old",
		Rules: `[{"name":"A","command":"SaveData","want":"0"},{"name":"Broken","command":"PowerOnState","want":"7"},{"name":"C","command":"Timezone","want":"99"}]`})
	_, body := e.do(t, "GET", "/policy", "admin", nil)
	if got := tileValue(t, body, "rules"); got != "2 | 2 enabled · 0 disabled · 1 invalid" {
		t.Errorf("rules tile = %q: only the valid rules are counted, the invalid one is named", got)
	}
	if got := tileValue(t, body, "compliant"); !strings.HasPrefix(got, "1 / 2") {
		t.Errorf("compliant tile = %q: bravo deviates from C", got)
	}
	rows := regexp.MustCompile(`(?s)<tr class[^>]*>.*?</tr>`).FindAllString(body, -1)
	if len(rows) != 3 || !strings.Contains(rows[1], "invalid: ") || strings.Contains(rows[1], "health-bar") ||
		!strings.Contains(rows[0], "2 / 2") || !strings.Contains(rows[2], "1 / 2 · 1 deviate") {
		t.Fatalf("rows (the broken one flagged, the others counted):\n%s", strings.Join(rows, "\n---\n"))
	}
	for _, id := range []string{"r1", "r3"} {
		_, page := e.do(t, "GET", "/policy/rules/"+id, "admin", nil)
		if strings.Contains(page, "Another saved rule is invalid") || !strings.Contains(page, `data-tile="applies"`) {
			t.Errorf("rule page %s must still judge its devices:\n%s", id, page)
		}
		if v := tileValue(t, page, "applies"); strings.HasPrefix(v, "–") {
			t.Errorf("rule page %s shows no numbers: %q", id, v)
		}
	}
	_, page := e.do(t, "GET", "/policy/rules/r2", "admin", nil)
	if !strings.Contains(page, "one of 0, 1, 2, 3, 4, 5") {
		t.Errorf("the broken rule's own page says why:\n%s", page)
	}
}
