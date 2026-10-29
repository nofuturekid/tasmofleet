package policy

import (
	"reflect"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

func always(model.DeviceID, string) bool { return true }

func lines(t *testing.T, ps []FixPlan, id model.DeviceID) []string {
	t.Helper()
	for _, p := range ps {
		if p.DeviceID == id {
			return p.Lines()
		}
	}
	t.Fatalf("no plan for device %d in %+v", id, ps)
	return nil
}

// Why: "Make compliant" must change only what is known to be wrong. A rule the
// device already meets, or one whose value could not be read, must not produce
// a command: sending it would write a value nobody checked.
func TestPlanFixOnlyForAKnownDeviation(t *testing.T) {
	devs := []model.Device{dev(1, "plug", model.ChipESP8266)}
	vals := map[model.DeviceID]map[string]string{1: {"PowerOnState": "3", "SetOption0": "0"}} // SaveData unknown
	ps := PlanFix(Check(flashWear, devs, vals), always)
	if got := lines(t, ps, 1); !reflect.DeepEqual(got, []string{"PowerOnState 0"}) {
		t.Fatalf("commands %v: only the known deviation may be fixed", got)
	}
	skipped := ps[0].Skipped
	if len(skipped) != 1 || skipped[0].Command != "SaveData" || skipped[0].Reason != SkipUnknown {
		t.Fatalf("the unknown SaveData must be listed as skipped: %+v", skipped)
	}
}

// Why: a backup value is what the device had once. Acting on it could undo a
// change somebody made since; the plan must wait for a live value.
func TestPlanFixSkipsValuesThatAreNotLive(t *testing.T) {
	devs := []model.Device{dev(1, "plug", model.ChipESP8266)}
	vals := map[model.DeviceID]map[string]string{1: {"PowerOnState": "3", "SetOption0": "1", "SaveData": "5"}}
	live := func(_ model.DeviceID, key string) bool { return key != "setoption0" }
	ps := PlanFix(Check(flashWear, devs, vals), live)
	if got := lines(t, ps, 1); !reflect.DeepEqual(got, []string{"PowerOnState 0", "SaveData 0"}) {
		t.Fatalf("commands %v", got)
	}
	if s := ps[0].Skipped; len(s) != 1 || s[0].Command != "SetOption0" || s[0].Reason != SkipNotLive {
		t.Fatalf("skipped %+v", s)
	}
}

// Why: the ESP32 exception is the fleet policy's own wording. A QuinLED that
// keeps SetOption0 1 is compliant and must get no command; one that has 0
// must be set to 1, never to the ESP8266 value.
func TestPlanFixRespectsTheESP32Exception(t *testing.T) {
	devs := []model.Device{dev(1, "plug", model.ChipESP8266), dev(2, "quinled", model.ChipESP32), dev(3, "other", model.ChipESP32)}
	vals := map[model.DeviceID]map[string]string{
		1: {"PowerOnState": "0", "SetOption0": "1", "SaveData": "0"},
		2: {"PowerOnState": "0", "SetOption0": "1", "SaveData": "0"},
		3: {"PowerOnState": "0", "SetOption0": "0", "SaveData": "0"},
	}
	ps := PlanFix(Check(flashWear, devs, vals), always)
	if got := lines(t, ps, 1); !reflect.DeepEqual(got, []string{"SetOption0 0"}) {
		t.Fatalf("plug: %v", got)
	}
	for _, p := range ps {
		if p.DeviceID == 2 {
			t.Fatalf("a compliant ESP32 got a plan: %+v", p)
		}
	}
	if got := lines(t, ps, 3); !reflect.DeepEqual(got, []string{"SetOption0 1"}) {
		t.Fatalf("esp32 with 0: %v", got)
	}
}

// Why: SaveData writes everything to flash and a restarting setting cuts the
// command queue, so both must come last, restart after save. Otherwise a value
// set after SaveData would be lost at the next power cut.
func TestOrderCommandsPutsSaveDataThenRestartLast(t *testing.T) {
	rules := []Rule{
		{Command: "Hostname", Want: "plug"}, {Command: "SaveData", Want: "0"},
		{Command: "SetOption0", Want: "0"}, {Command: "Topic", Want: "plug"}, {Command: "PowerOnState", Want: "0"},
	}
	vals := map[model.DeviceID]map[string]string{1: {"Hostname": "x", "SaveData": "9", "SetOption0": "1", "Topic": "x", "PowerOnState": "3"}}
	ps := PlanFix(Check(rules, []model.Device{dev(1, "d", model.ChipESP8266)}, vals), always)
	want := []string{"SetOption0 0", "PowerOnState 0", "SaveData 0", "Hostname plug", "Topic plug"}
	if got := lines(t, ps, 1); !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
}

// Why: two findings that expect different values for the same setting cannot
// both be right; picking one would silently decide for the admin. Check never
// produces such a pair any more (precedence, see the next test), but PlanFix
// takes findings from any caller and must stay safe on its own.
func TestPlanFixConflictingFindingsAreSkipped(t *testing.T) {
	fs := []Finding{
		{DeviceID: 1, Rule: Rule{Command: "SetOption0", Want: "0"}, Have: "7"},
		{DeviceID: 1, Rule: Rule{Command: "SetOption0", Want: "1"}, Have: "7", RuleIndex: 1},
	}
	ps := PlanFix(fs, always)
	if len(ps[0].Commands) != 0 || len(ps[0].Skipped) != 1 || ps[0].Skipped[0].Reason != SkipConflict {
		t.Fatalf("plan %+v", ps[0])
	}
}

// Why: with precedence the admin decides by order which of two overlapping
// rules counts (the overlap is shown as a conflict); Make compliant then sets
// the higher rule's value and nothing else.
func TestPlanFixFollowsPrecedence(t *testing.T) {
	rules := []Rule{{Command: "SetOption0", Want: "0"}, {Command: "SetOption0", Want: "1", Tags: []string{"x"}}}
	d := dev(1, "d", model.ChipESP8266, "x")
	ps := PlanFix(Check(rules, []model.Device{d}, map[model.DeviceID]map[string]string{1: {"SetOption0": "7"}}), always)
	if got := lines(t, ps, 1); !reflect.DeepEqual(got, []string{"SetOption0 0"}) || len(ps[0].Skipped) != 0 {
		t.Fatalf("plan %+v", ps[0])
	}
}

// Why: the rule value goes through the same validation as the settings editor,
// so a rule can never smuggle a Backlog separator or an out-of-range number to
// a device.
func TestCommandTextValidates(t *testing.T) {
	for _, c := range []struct{ cmd, want, text string }{
		{"PowerOnState", "0", "PowerOnState 0"},
		{"SetOption0", "OFF", "SetOption0 0"}, // ON/OFF are the policy's own synonyms
		{"SetOption114", "1", "SetOption114 1"},
		{"SetOption45", "1", "SetOption45 1"}, {"SetOption45", "250", "SetOption45 250"},
		{"SetOption34", "0", "SetOption34 0"}, {"SetOption34", "255", "SetOption34 255"},
		{"SetOption45", "40", "SetOption45 40"}, // SetOption32..49 are numeric parameters
		{"Timezone", "99", "Timezone 99"},
	} {
		got, err := CommandText(c.cmd, c.want)
		if err != nil || got != c.text {
			t.Errorf("%s %s: %q %v", c.cmd, c.want, got, err)
		}
	}
	for _, c := range []struct{ cmd, want string }{
		{"PowerOnState", "9"}, {"SaveData", "9999"}, {"MqttPassword", "x"}, {"Restart", "1"}, {"SetOption0", "1; Restart 1"}, {"SetOption0", "x"},
		{"Hostname", "a;b"}, {"SetOption45", "0"}, {"SetOption45", "251"}, {"SetOption34", "256"}, // Tasmota answers Command Error out of range
		{"SetOption32", "-1"},
	} {
		if got, err := CommandText(c.cmd, c.want); err == nil {
			t.Errorf("%s %q must be refused, got %q", c.cmd, c.want, got)
		}
	}
}
