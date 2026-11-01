package policy

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Tests for the GPIO template comparison (#327). The rules protect hardware: a template written to
// the wrong chip or the wrong group drives pins the board does not have, so every refusal reason
// has its own test, and the fixtures are real device captures.

const fixtures = "../../testdata/fixtures/devices"

func capture(t *testing.T, dir, file string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtures, dir, file)) //nolint:gosec // fixtures dir is trusted test input
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fixtureDevice builds a live device from a capture's Module and Template replies.
func fixtureDevice(t *testing.T, id model.DeviceID, dir string, family model.ChipFamily) TemplateDevice {
	t.Helper()
	tpl, err := ParseTemplate(capture(t, dir, "template.json"))
	if err != nil {
		t.Fatal(err)
	}
	mod, err := ParseModule(capture(t, dir, "module.json"))
	if err != nil {
		t.Fatal(err)
	}
	return TemplateDevice{ID: id, Family: family, Live: true, Module: &mod, Template: &tpl}
}

func nous(t *testing.T, id model.DeviceID) TemplateDevice {
	return fixtureDevice(t, id, "mr-bambu-plug", model.ChipESP8266)
}

// with returns a copy of d whose template slot i holds code.
func with(d TemplateDevice, slot, code int) TemplateDevice {
	tpl := *d.Template
	tpl.GPIO = append([]int(nil), tpl.GPIO...)
	tpl.GPIO[slot] = code
	d.Template = &tpl
	return d
}

func TestParseTemplateOfRealCaptures(t *testing.T) {
	// The NOUS A1T is the case of the issue: two plugs with one template. Their exact fields are what
	// we compare and later send, so a lost field (FLAG, BASE, ARCH) would make "equal" wrong.
	got, err := ParseTemplate(capture(t, "mr-bambu-plug", "template.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := GPIOTemplate{Name: "NOUS A1T", Arch: "ESP8266", GPIO: []int{32, 0, 0, 0, 2720, 2656, 0, 0, 2624, 320, 224, 0, 0, 0}, Flag: 0, Base: 49}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	e32, err := ParseTemplate(capture(t, "office-monitor-guest", "template.json"))
	if err != nil || len(e32.GPIO) != 36 || e32.Arch != "ESP32" {
		t.Fatalf("esp32: %+v %v", e32, err)
	}
}

func TestParseTemplateWithoutArchAndWithUser(t *testing.T) {
	// ARCH exists from 15.x on (T7): older firmware has none, and then we must not invent one. The
	// JSON value 1 is "User" and has to survive as 1, not as None.
	got, err := ParseTemplate([]byte(`{"NAME":"Generic","GPIO":[32,1,1,1,1,0,0,0,224,320,1,0,0,0],"FLAG":0,"BASE":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Arch != "" || got.GPIO[1] != 1 || got.Base != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestParseTemplateRefusesWhatItCannotTrust(t *testing.T) {
	// A reply we only half understand must never become a template we send to other devices.
	cases := map[string]string{
		"not JSON":         `Command Unknown`,
		"error reply":      `{"Command":"Unknown"}`,
		"no GPIO":          `{"NAME":"x","FLAG":0,"BASE":1}`,
		"no NAME":          `{"GPIO":[0],"FLAG":0,"BASE":1}`,
		"no BASE":          `{"NAME":"x","GPIO":[0],"FLAG":0}`,
		"no FLAG":          `{"NAME":"x","GPIO":[0],"BASE":1}`,
		"BASE zero":        `{"NAME":"x","GPIO":[0],"FLAG":0,"BASE":0}`,
		"negative GPIO":    `{"NAME":"x","GPIO":[-1],"FLAG":0,"BASE":1}`,
		"GPIO above 16bit": `{"NAME":"x","GPIO":[65536],"FLAG":0,"BASE":1}`,
		"fractional GPIO":  `{"NAME":"x","GPIO":[1.5],"FLAG":0,"BASE":1}`,
		"text GPIO":        `{"NAME":"x","GPIO":["a"],"FLAG":0,"BASE":1}`,
		"empty GPIO":       `{"NAME":"x","GPIO":[],"FLAG":0,"BASE":1}`,
		"negative FLAG":    `{"NAME":"x","GPIO":[0],"FLAG":-1,"BASE":1}`,
	}
	for name, in := range cases {
		if _, err := ParseTemplate([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseModule(t *testing.T) {
	// Module n=0 is the user template (T2); n>=1 a built-in module whose stored template is inactive.
	m, err := ParseModule(capture(t, "mr-bambu-plug", "module.json"))
	if err != nil || m.Number != 0 || m.Name != "NOUS A1T" {
		t.Fatalf("%+v %v", m, err)
	}
	m, err = ParseModule(capture(t, "livingroom-readinglamp", "module.json"))
	if err != nil || m.Number != 1 || m.Name != "Sonoff Basic" {
		t.Fatalf("%+v %v", m, err)
	}
	for _, in := range []string{`{}`, `{"Module":{}}`, `{"Module":{"a":"b"}}`, `{"Module":{"-1":"b"}}`, `{"Module":{"0":"a","1":"b"}}`, `{"Module":5}`, `x`} {
		if _, err := ParseModule([]byte(in)); err == nil {
			t.Errorf("%s accepted", in)
		}
	}
}

func TestStateFollowsTheModuleNotTheStoredTemplate(t *testing.T) {
	// T1: Template answers the stored template even on a built-in module. The livingroom lamp runs
	// "Sonoff Basic" and merely stores an inactive "Generic": judging it by that template would compare
	// a pin map the device does not use.
	lamp := fixtureDevice(t, 3, "livingroom-readinglamp", model.ChipESP8266)
	if lamp.State() != OnModule {
		t.Fatalf("lamp: %v", lamp.State())
	}
	if nous(t, 1).State() != OnTemplate {
		t.Fatal("NOUS not OnTemplate")
	}
	noModule := nous(t, 1)
	noModule.Module = nil
	noTemplate := nous(t, 1)
	noTemplate.Template = nil
	noFamily := nous(t, 1)
	noFamily.Family = model.ChipUnknown
	for name, d := range map[string]TemplateDevice{"no Module": noModule, "Module 0 without Template": noTemplate, "unknown family": noFamily} {
		if d.State() != TemplateUnknown {
			t.Errorf("%s: %v", name, d.State())
		}
	}
	// A built-in module needs no Template reply to be judged "on a module".
	lamp.Template = nil
	if lamp.State() != OnModule {
		t.Errorf("lamp without template: %v", lamp.State())
	}
}

func TestGroupsSplitByNameAndFamily(t *testing.T) {
	// Decision Q1: group by template NAME plus chip family. A NAME used on an ESP32 and an ESP32-S2
	// (same 36 slots, different pins) must be two groups, or the S2 would be compared to the ESP32 map.
	a, b := nous(t, 1), nous(t, 2)
	c := nous(t, 3)
	c.Family = model.ChipESP32S2
	other := with(nous(t, 4), 0, 0)
	o := *other.Template
	o.Name = "Other"
	other.Template = &o
	lamp := fixtureDevice(t, 5, "livingroom-readinglamp", model.ChipESP8266)
	lamp2 := fixtureDevice(t, 6, "livingroom-readinglamp", model.ChipESP8266)
	lamp3 := fixtureDevice(t, 7, "livingroom-readinglamp", model.ChipESP32)
	unread := nous(t, 8)
	unread.Module = nil
	groups, not := GroupTemplates([]TemplateDevice{lamp3, c, b, other, lamp, a, unread, lamp2})
	var keys []GroupKey
	counts := map[GroupKey]int{}
	for _, g := range groups {
		keys = append(keys, g.Key)
		counts[g.Key] = len(g.Devices)
	}
	want := []GroupKey{
		{Family: model.ChipESP32, Name: "Sonoff Basic", BuiltIn: true},
		{Family: model.ChipESP32S2, Name: "NOUS A1T"},
		{Family: model.ChipESP8266, Name: "NOUS A1T"},
		{Family: model.ChipESP8266, Name: "Other"},
		{Family: model.ChipESP8266, Name: "Sonoff Basic", BuiltIn: true},
	}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys (sorted by family, name, built-in): %+v", keys)
	}
	if counts[GroupKey{Family: model.ChipESP8266, Name: "NOUS A1T"}] != 2 || counts[GroupKey{Family: model.ChipESP8266, Name: "Sonoff Basic", BuiltIn: true}] != 2 {
		t.Errorf("counts: %v", counts)
	}
	if len(not) != 1 || not[0].ID != 8 {
		t.Errorf("not judged: %+v", not)
	}
	for _, g := range groups {
		for i := 1; i < len(g.Devices); i++ {
			if g.Devices[i-1].ID > g.Devices[i].ID {
				t.Errorf("devices of %+v not sorted by id", g.Key)
			}
		}
	}
}

func TestMostCommonReference(t *testing.T) {
	// The reference is what most live devices use. Guessing between equals would copy a possibly wrong
	// pin map to the fleet, so a tie, or fewer than two live devices, gives no reference at all.
	odd := with(nous(t, 3), 11, 160) // an extra pin
	odd2 := with(nous(t, 4), 12, 160)
	t.Run("majority", func(t *testing.T) {
		g := groupOf(t, nous(t, 1), nous(t, 2), odd)
		ref, ok := MostCommon(g)
		if !ok || ref.Device.ID != 1 || ref.Count != 2 || ref.Of != 3 || ref.How != RefMostCommon {
			t.Fatalf("%+v %v", ref, ok)
		}
	})
	t.Run("tie", func(t *testing.T) {
		odd3 := odd2
		odd3.ID = 5
		for name, g := range map[string]TemplateGroup{
			"1:1":   groupOf(t, nous(t, 1), odd),
			"2:2":   groupOf(t, nous(t, 1), nous(t, 2), odd2, odd3),
			"1:1:1": groupOf(t, nous(t, 1), odd, odd2),
		} {
			if ref, ok := MostCommon(g); ok {
				t.Errorf("%s gave reference %+v", name, ref)
			}
		}
	})
	t.Run("a unique top wins even with several runners-up", func(t *testing.T) {
		if ref, ok := MostCommon(groupOf(t, nous(t, 1), nous(t, 2), odd, odd2)); !ok || ref.Count != 2 || ref.Of != 4 {
			t.Errorf("2:1:1: %+v %v", ref, ok)
		}
	})
	t.Run("one live device", func(t *testing.T) {
		if _, ok := MostCommon(groupOf(t, nous(t, 1))); ok {
			t.Error("a single device is its own reference")
		}
	})
	t.Run("snapshots do not vote", func(t *testing.T) {
		snap1, snap2 := nous(t, 2), nous(t, 3)
		snap1.Live, snap2.Live = false, false
		if _, ok := MostCommon(groupOf(t, nous(t, 1), snap1, snap2)); ok {
			t.Error("one live device plus two snapshots gave a reference")
		}
	})
	t.Run("name and arch do not split votes", func(t *testing.T) {
		// Identity is GPIO, FLAG, BASE (2.2); older firmware has no ARCH.
		noArch := nous(t, 2)
		tpl := *noArch.Template
		tpl.Arch = ""
		noArch.Template = &tpl
		ref, ok := MostCommon(groupOf(t, nous(t, 1), noArch))
		if !ok || ref.Count != 2 {
			t.Errorf("%+v %v", ref, ok)
		}
	})
	t.Run("flag and base are part of the identity", func(t *testing.T) {
		f, b := nous(t, 2), nous(t, 3)
		tf, tb := *f.Template, *b.Template
		tf.Flag, tb.Base = 1, 18
		f.Template, b.Template = &tf, &tb
		if _, ok := MostCommon(groupOf(t, nous(t, 1), f, b)); ok {
			t.Error("three different identities gave a reference")
		}
	})
	t.Run("built-in module groups have none", func(t *testing.T) {
		l1 := fixtureDevice(t, 1, "livingroom-readinglamp", model.ChipESP8266)
		l2 := fixtureDevice(t, 2, "livingroom-readinglamp", model.ChipESP8266)
		if _, ok := MostCommon(groupOf(t, l1, l2)); ok {
			t.Error("a built-in module group has a template reference")
		}
	})
	t.Run("reference is the lowest id of the winners", func(t *testing.T) {
		ref, ok := MostCommon(groupOf(t, nous(t, 9), nous(t, 4), odd))
		if !ok || ref.Device.ID != 4 {
			t.Errorf("%+v", ref)
		}
	})
}

func groupOf(t *testing.T, devs ...TemplateDevice) TemplateGroup {
	t.Helper()
	groups, _ := GroupTemplates(devs)
	if len(groups) != 1 {
		t.Fatalf("%d groups", len(groups))
	}
	return groups[0]
}

func TestChosenReference(t *testing.T) {
	// A person may pick any live device of the group; nothing else (a snapshot, a device of another
	// group, an unknown id) may become the reference.
	odd := with(nous(t, 3), 11, 160)
	snap := nous(t, 4)
	snap.Live = false
	g := groupOf(t, nous(t, 1), nous(t, 2), odd, snap)
	ref, err := ChosenReference(g, 3)
	if err != nil || ref.Device.ID != 3 || ref.How != RefChosen || ref.Count != 1 || ref.Of != 3 {
		t.Fatalf("%+v %v", ref, err)
	}
	for _, id := range []model.DeviceID{4, 99} {
		if _, err := ChosenReference(g, id); err == nil {
			t.Errorf("id %d accepted", id)
		}
	}
	lamp := fixtureDevice(t, 7, "livingroom-readinglamp", model.ChipESP8266)
	if _, err := ChosenReference(groupOf(t, lamp, fixtureDevice(t, 8, "livingroom-readinglamp", model.ChipESP8266)), 7); err == nil {
		t.Error("a built-in module device became a template reference")
	}
}

func TestDiffUsesPhysicalPins(t *testing.T) {
	// The user wires by GPIO number, not by template slot. ESP8266 skips 6-8 and 11, so slot 8 is GPIO12
	// and slot 13 is the ADC; a diff that said "slot 8" would point at the wrong pin.
	want := nous(t, 1).Template
	have := with(with(nous(t, 2), 8, 0), 9, 224).Template
	lines, err := DiffTemplates(model.ChipESP8266, *have, *want)
	if err != nil {
		t.Fatal(err)
	}
	got := lineTexts(lines)
	wantLines := []string{"GPIO12: None -> HLWBL SEL_i", "GPIO13: Relay1 -> Led_i1"}
	if !reflect.DeepEqual(got, wantLines) {
		t.Fatalf("got %q", got)
	}
	if lines[0].Pin != 12 || lines[0].Slot != 8 || lines[0].Kind != DiffGPIO {
		t.Errorf("fields: %+v", lines[0])
	}
}

func lineTexts(ls []DiffLine) []string {
	var out []string
	for _, l := range ls {
		out = append(out, l.Text)
	}
	return out
}

func TestDiffPerFamily(t *testing.T) {
	// One changed slot per family, at a slot where the mapping differs from 1:1, so a wrong table row shows.
	mk := func(n int) GPIOTemplate {
		return GPIOTemplate{Name: "x", GPIO: make([]int, n), Base: 1}
	}
	cases := []struct {
		fam  model.ChipFamily
		n    int
		slot int
		pin  int
	}{
		{model.ChipESP8266, 14, 6, 9}, {model.ChipESP8266, 14, 13, 17},
		{model.ChipESP32, 36, 24, 6}, {model.ChipESP32, 36, 28, 32},
		{model.ChipESP32S2, 36, 22, 33}, {model.ChipESP32S3, 38, 37, 48},
		{model.ChipESP32C2, 21, 20, 20}, {model.ChipESP32C3, 22, 21, 21},
		{model.ChipESP32C5, 29, 28, 28}, {model.ChipESP32C6, 31, 30, 30},
		{model.ChipESP32C61, 30, 29, 29}, {model.ChipESP32P4, 55, 54, 54},
	}
	for _, c := range cases {
		have, want := mk(c.n), mk(c.n)
		want.GPIO[c.slot] = 224
		lines, err := DiffTemplates(c.fam, have, want)
		if err != nil || len(lines) != 1 || lines[0].Pin != c.pin || lines[0].Slot != c.slot {
			t.Errorf("%s slot %d: %+v %v, want pin %d", c.fam, c.slot, lines, err, c.pin)
		}
	}
}

func TestDiffFlagBaseAndUnknownCodes(t *testing.T) {
	// FLAG and BASE decide device code paths (T13), so they are diffed like pins. A function number
	// newer than our table must show as #<code>, not as a wrong name.
	have := GPIOTemplate{Name: "x", GPIO: make([]int, 14), Flag: 0, Base: 49}
	want := GPIOTemplate{Name: "x", GPIO: make([]int, 14), Flag: 1, Base: 18}
	want.GPIO[2] = 1
	have.GPIO[3] = 64000 << 5
	lines, err := DiffTemplates(model.ChipESP8266, have, want)
	if err != nil {
		t.Fatal(err)
	}
	got := lineTexts(lines)
	wantLines := []string{"GPIO2: None -> User", "GPIO3: #2048000 -> None", "FLAG 0 -> 1", "BASE 49 -> 18"}
	if !reflect.DeepEqual(got, wantLines) {
		t.Fatalf("got %q", got)
	}
	if same, _ := DiffTemplates(model.ChipESP8266, want, want); len(same) != 0 {
		t.Errorf("identical templates differ: %v", same)
	}
}

func TestDiffRefusesWhatItCannotName(t *testing.T) {
	// Without the right length or a known family there is no honest pin number.
	a := GPIOTemplate{GPIO: make([]int, 14), Base: 1}
	if _, err := DiffTemplates(model.ChipESP32, a, a); err == nil {
		t.Error("14 slots accepted for an ESP32")
	}
	b := GPIOTemplate{GPIO: make([]int, 13), Base: 1}
	if _, err := DiffTemplates(model.ChipESP8266, a, b); err == nil {
		t.Error("different lengths accepted")
	}
	if _, err := DiffTemplates(model.ChipUnknown, a, a); err == nil {
		t.Error("unknown family accepted")
	}
}

func TestBuildCommandIsExactAndHasNoCMND(t *testing.T) {
	// The JSON is built from parsed fields in the order the device prints them (T6: all fields, full array).
	// Whatever else was in the reply, CMND above all (T8: executed as a Backlog), is never passed on.
	tpl, err := ParseTemplate([]byte(`{"NAME":"NOUS A1T","ARCH":"ESP8266","GPIO":[32,0,0,0,2720,2656,0,0,2624,320,224,0,0,0],"FLAG":0,"BASE":49,"CMND":"Backlog Restart 1"}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := BuildTemplateCommand(tpl, "ESP8266")
	if err != nil {
		t.Fatal(err)
	}
	want := `Template {"NAME":"NOUS A1T","ARCH":"ESP8266","GPIO":[32,0,0,0,2720,2656,0,0,2624,320,224,0,0,0],"FLAG":0,"BASE":49}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if strings.Contains(strings.ToUpper(got), "CMND") {
		t.Error("CMND in the command")
	}
	// ARCH only when the target's own reply had it (T7): older firmware ignores it, so sending it protects nothing.
	got, err = BuildTemplateCommand(tpl, "")
	if err != nil || strings.Contains(got, "ARCH") {
		t.Errorf("without target ARCH: %s %v", got, err)
	}
}

func TestBuildCommandRefusesUnsafeNames(t *testing.T) {
	// NAME is user text that ends up inside a command line. A quote or backslash breaks the JSON, ';'
	// would split the line into a second command, and control characters corrupt the settings text.
	for _, name := range []string{"", `a"b`, `a\b`, "a;b", "a\nb", "a\tb", "a\x7fb", "a\x00b", "a\u202eb"} {
		tpl := GPIOTemplate{Name: name, GPIO: []int{0}, Base: 1}
		if _, err := BuildTemplateCommand(tpl, ""); err == nil {
			t.Errorf("name %q accepted", name)
		}
	}
	for _, name := range []string{"NOUS A1T", "Shelly 2.5", "Plüg 1", "a-b_c/d(e)"} {
		tpl := GPIOTemplate{Name: name, GPIO: []int{0}, Base: 1}
		if _, err := BuildTemplateCommand(tpl, ""); err != nil {
			t.Errorf("name %q refused: %v", name, err)
		}
	}
}

func plan(t *testing.T, ref, dev TemplateDevice, opts PlanOptions) ApplyPlan {
	t.Helper()
	return PlanApply(Reference{Device: ref, How: RefChosen, Count: 1, Of: 1}, dev, opts)
}

func TestPlanEligible(t *testing.T) {
	// The normal case of the issue: a plug whose pin map drifted gets the reference template in one
	// command; Template on Module 0 restarts the device (T3), which the preview must announce.
	ref := nous(t, 1)
	dev := with(with(nous(t, 2), 9, 224), 8, 0)
	p := plan(t, ref, dev, PlanOptions{})
	if p.Refusal != RefuseNone || p.Matches || p.Device != 2 || !p.Restarts || len(p.Diff) != 2 {
		t.Fatalf("%+v", p)
	}
	want := `Template {"NAME":"NOUS A1T","ARCH":"ESP8266","GPIO":[32,0,0,0,2720,2656,0,0,2624,320,224,0,0,0],"FLAG":0,"BASE":49}`
	if !reflect.DeepEqual(p.Commands, []string{want}) {
		t.Errorf("commands: %q", p.Commands)
	}
}

func TestPlanMatchesNeedsNothing(t *testing.T) {
	// A device that already has the reference's layout is not touched: no command, no restart, and no
	// refusal even when it could not be written (a wired or MQTT-only device that is fine stays fine).
	ref := nous(t, 1)
	same := nous(t, 2)
	same.MQTTOnly, same.Wired = true, true
	p := plan(t, ref, same, PlanOptions{})
	if !p.Matches || p.Refusal != RefuseNone || len(p.Commands) != 0 || p.Restarts {
		t.Fatalf("%+v", p)
	}
	self := PlanApply(Reference{Device: ref}, ref, PlanOptions{})
	if !self.Matches {
		t.Errorf("reference vs itself: %+v", self)
	}
}

func TestPlanRefusals(t *testing.T) {
	// Every way a template must not be applied, each with its reason code and a human sentence. The
	// reasons are shown in the preview, so a silent change of cause would mislead the operator.
	ref := nous(t, 1)
	drift := func(d TemplateDevice) TemplateDevice { return with(d, 9, 224) }
	mod := func(d TemplateDevice, f func(*TemplateDevice)) TemplateDevice { f(&d); return d }
	cases := []struct {
		name string
		dev  TemplateDevice
		want Refusal
	}{
		{"value from a backup", mod(drift(nous(t, 2)), func(d *TemplateDevice) { d.Live = false }), RefuseNotLive},
		{"built-in module", mod(drift(nous(t, 2)), func(d *TemplateDevice) { d.Module = &ModuleReading{Number: 1, Name: "Sonoff Basic"} }), RefuseNotOnTemplate},
		{"never read", mod(drift(nous(t, 2)), func(d *TemplateDevice) { d.Module = nil }), RefuseNotOnTemplate},
		{"unknown family", mod(drift(nous(t, 2)), func(d *TemplateDevice) { d.Family = model.ChipUnknown }), RefuseFamily},
		{"other family, same length", mod(drift(nous(t, 2)), func(d *TemplateDevice) { d.Family = model.ChipESP32S2 }), RefuseFamily},
		{"other template name", mod(drift(nous(t, 2)), func(d *TemplateDevice) { c := *d.Template; c.Name = "Other"; d.Template = &c }), RefuseOtherGroup},
		{"shorter array", mod(drift(nous(t, 2)), func(d *TemplateDevice) { c := *d.Template; c.GPIO = c.GPIO[:13]; d.Template = &c }), RefuseLength},
		{"longer array", mod(drift(nous(t, 2)), func(d *TemplateDevice) {
			c := *d.Template
			c.GPIO = append(append([]int(nil), c.GPIO...), 0)
			d.Template = &c
		}), RefuseLength},
		{"other ARCH", mod(drift(nous(t, 2)), func(d *TemplateDevice) { c := *d.Template; c.Arch = "ESP32"; d.Template = &c }), RefuseArch},
		{"minimal firmware", mod(drift(nous(t, 2)), func(d *TemplateDevice) { d.Variant = "minimal" }), RefuseMinimal},
		{"MQTT only", mod(drift(nous(t, 2)), func(d *TemplateDevice) { d.MQTTOnly = true }), RefuseMQTTOnly},
	}
	for _, c := range cases {
		p := plan(t, ref, c.dev, PlanOptions{})
		if p.Refusal != c.want || p.Reason == "" || len(p.Commands) != 0 || p.Restarts || p.Matches {
			t.Errorf("%s: %+v, want refusal %q", c.name, p, c.want)
		}
	}
}

func TestPlanLengthIsNoFamilyCheck(t *testing.T) {
	// ESP32 and ESP32-S2 both have 36 slots with different pin maps (T9). Equal length must never allow a
	// cross-family template: the family itself is compared, exactly.
	esp32 := fixtureDevice(t, 1, "office-monitor-guest", model.ChipESP32)
	s2 := with(fixtureDevice(t, 2, "office-monitor-guest", model.ChipESP32S2), 12, 0)
	if len(esp32.Template.GPIO) != 36 {
		t.Fatal("fixture is not 36 slots")
	}
	g, _ := GroupTemplates([]TemplateDevice{esp32, s2})
	if len(g) != 2 {
		t.Fatalf("one group for two families: %d", len(g))
	}
	p := PlanApply(Reference{Device: esp32, How: RefChosen}, s2, PlanOptions{})
	if p.Refusal != RefuseFamily {
		t.Errorf("%+v", p)
	}
}

func TestPlanArchOnlyWhenBothReported(t *testing.T) {
	// ARCH is compared if both replies had it; if only the reference had it the target is on older
	// firmware: allowed, and the command then has no ARCH (it would be ignored there, T7).
	ref := nous(t, 1)
	old := with(nous(t, 2), 9, 224)
	tpl := *old.Template
	tpl.Arch = ""
	old.Template = &tpl
	p := plan(t, ref, old, PlanOptions{})
	if p.Refusal != RefuseNone || strings.Contains(p.Commands[0], "ARCH") {
		t.Errorf("target without ARCH: %+v", p)
	}
	oldRef := nous(t, 1)
	rt := *oldRef.Template
	rt.Arch = ""
	oldRef.Template = &rt
	p = plan(t, oldRef, with(nous(t, 2), 9, 224), PlanOptions{})
	if p.Refusal != RefuseNone || !strings.Contains(p.Commands[0], `"ARCH":"ESP8266"`) {
		t.Errorf("reference without ARCH: %+v", p)
	}
}

func TestPlanUnsafeNameIsRefused(t *testing.T) {
	// The group NAME is also the name we would send; an unsafe one stops the plan instead of reaching a device.
	bad := func(d TemplateDevice) TemplateDevice {
		c := *d.Template
		c.Name = "a;b"
		d.Template = &c
		return d
	}
	p := plan(t, bad(nous(t, 1)), bad(with(nous(t, 2), 9, 224)), PlanOptions{})
	if p.Refusal != RefuseUnsafeName {
		t.Errorf("%+v", p)
	}
}

func TestPlanEthernet(t *testing.T) {
	// A wired board that loses its ETH_* pins (or gets different ones) falls off the network and cannot be reached
	// to repair it (7). Elsewhere the same diff is fine, and so is a wired board with a diff that leaves ETH alone.
	ref := fixtureDevice(t, 1, "office-monitor-guest", model.ChipESP32)
	eth := 173 << 5 // ETH POWER
	ref = with(ref, 4, eth)
	dev := fixtureDevice(t, 2, "office-monitor-guest", model.ChipESP32)
	wired := func(d TemplateDevice) TemplateDevice { d.Wired = true; return d }

	if p := plan(t, ref, wired(dev), PlanOptions{}); p.Refusal != RefuseEthernet {
		t.Errorf("wired gets ETH pin: %+v", p)
	}
	// The other direction: the device has the ETH pin and the reference would remove it.
	if p := plan(t, dev, wired(ref), PlanOptions{}); p.Refusal != RefuseEthernet {
		t.Errorf("wired loses ETH pin: %+v", p)
	}
	if p := plan(t, ref, dev, PlanOptions{}); p.Refusal != RefuseNone {
		t.Errorf("Wi-Fi device with ETH diff: %+v", p)
	}
	// Same ETH pin on both, other slot differs: the wired board keeps its network.
	ethBoth := with(dev, 4, eth)
	if p := plan(t, ref, wired(with(ethBoth, 12, 0)), PlanOptions{}); p.Refusal != RefuseNone {
		t.Errorf("wired with untouched ETH: %+v", p)
	}
}

func TestPlanMQTTOnlyNeedsTheStrictMatcher(t *testing.T) {
	// An automated Template read over MQTT cannot be matched to its reply today (plan 1.2), so such a device can be
	// neither verified nor safely written; with the strict matcher (WP-B) the caller opts in.
	ref := nous(t, 1)
	dev := with(nous(t, 2), 9, 224)
	dev.MQTTOnly = true
	if p := plan(t, ref, dev, PlanOptions{}); p.Refusal != RefuseMQTTOnly {
		t.Errorf("default: %+v", p)
	}
	if p := plan(t, ref, dev, PlanOptions{StrictMQTT: true}); p.Refusal != RefuseNone {
		t.Errorf("strict: %+v", p)
	}
}

func TestFingerprintCoversLayoutOnly(t *testing.T) {
	// The apply digest covers the template identity as read. Any change of a pin, FLAG or BASE must change
	// it; NAME and ARCH are not part of the layout (2.2).
	a := nous(t, 1).Template
	fp := a.Fingerprint()
	for name, m := range map[string]GPIOTemplate{"pin": *with(nous(t, 1), 3, 160).Template} {
		if m.Fingerprint() == fp {
			t.Errorf("%s change keeps the fingerprint", name)
		}
	}
	f, b := *a, *a
	f.Flag, b.Base = 1, 18
	if f.Fingerprint() == fp || b.Fingerprint() == fp || f.Fingerprint() == b.Fingerprint() {
		t.Error("FLAG or BASE not in the fingerprint")
	}
	n := *a
	n.Name, n.Arch = "x", ""
	if n.Fingerprint() != fp {
		t.Error("NAME or ARCH in the fingerprint")
	}
}
