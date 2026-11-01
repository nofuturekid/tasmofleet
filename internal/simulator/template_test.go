package simulator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Template and Module as Tasmota 16.0.0 answers them (plan-0270-gpio.md facts T1-T7,
// support_command.ino CmndTemplate/CmndModule, support.ino JsonTemplate). The planner of #327 sends
// these commands, so a simulator that restarts at the wrong moment would let a wrong plan pass.

const (
	nousTemplate = `{"NAME":"NOUS A1T","ARCH":"ESP8266","GPIO":[32,0,0,0,2720,2656,0,0,2624,320,224,0,0,0],"FLAG":0,"BASE":49}`
	nousModule   = `{"Module":{"0":"NOUS A1T"}}`
)

func nousDevice(t *testing.T, spec DeviceSpec) Controller {
	t.Helper()
	if spec.Name == "" {
		spec.Name = "plug"
	}
	if spec.Profile == "" {
		spec.Profile = ProfileNOUSA1TBambu
	}
	return add(t, newTestFleet(t, "", Extra{}), spec)
}

// waitBoots waits (bounded) until the device has restarted n times and runs again.
func waitBoots(t *testing.T, d Controller, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := d.WaitFor(ctx, func(c Controller) bool { return c.Boots() >= n && c.Phase() == PhaseRun }); err != nil {
		t.Fatalf("device did not reach %d boots: boots %d phase %s", n, d.Boots(), d.Phase())
	}
}

// settle gives a queued restart (2 s at 2 % time scale: 40 ms) time to start, so "no restart" can be asserted.
func settle() { time.Sleep(400 * time.Millisecond) }

func TestNOUSProfilesReplayTheirCapturedTemplates(t *testing.T) {
	// The two real NOUS A1T captures are the case of #327: the same model on two devices. Their replies
	// must be exactly what the devices sent, or the comparison tests would run on invented data.
	for profile, dir := range map[Profile]string{ProfileNOUSA1TBambu: "mr-bambu-plug", ProfileNOUSA1TPrusa: "prusa-plug"} {
		d := nousDevice(t, DeviceSpec{Profile: profile})
		for file, cmnd := range map[string]string{"template.json": "Template", "module.json": "Module"} {
			want, err := os.ReadFile(filepath.Join(fixtures, "devices", dir, file))
			if err != nil {
				t.Fatal(err)
			}
			if got := cm(t, d, cmnd); got != string(want) {
				t.Errorf("%s %s: %s want %s", dir, cmnd, got, want)
			}
		}
	}
}

func TestTemplateStaysStoredOnABuiltInModule(t *testing.T) {
	// T1/T3: on a built-in module Template answers and changes only the stored template. It changes
	// nothing that runs, so the device must not restart; Module keeps saying "Sonoff Basic".
	d := add(t, newTestFleet(t, "", Extra{}), DeviceSpec{Name: "lamp", Profile: ProfileESP8266_1M})
	if got := cm(t, d, "Module"); got != `{"Module":{"1":"Sonoff Basic"}}` {
		t.Fatalf("module: %s", got)
	}
	got := cm(t, d, `Template {"NAME":"Stored","GPIO":[32,1,1,1,1,0,0,0,224,320,1,0,0,0],"FLAG":0,"BASE":1}`)
	if !strings.HasPrefix(got, `{"NAME":"Stored","ARCH":"ESP8266"`) {
		t.Fatalf("template reply: %s", got)
	}
	settle()
	if d.Boots() != 0 {
		t.Errorf("restarted %d times: Template on a built-in module must not restart", d.Boots())
	}
	if got := cm(t, d, "Module"); got != `{"Module":{"1":"Sonoff Basic"}}` {
		t.Errorf("module changed: %s", got)
	}
	if got := cm(t, d, "Template"); !strings.Contains(got, `"NAME":"Stored"`) {
		t.Errorf("template not stored: %s", got)
	}
}

func TestTemplateOnModule0RestartsAndIsSaved(t *testing.T) {
	// T3/T5: on Module 0 a Template write changes the active pin map, so the device restarts, and the
	// restart writes it to flash: it is still there afterwards.
	d := nousDevice(t, DeviceSpec{})
	changed := strings.Replace(nousTemplate, "320,224", "224,320", 1)
	if got := cm(t, d, "Template "+changed); got != changed {
		t.Fatalf("reply %s want %s", got, changed)
	}
	waitBoots(t, d, 1)
	if got := cmUp(t, d, "Template"); got != changed {
		t.Errorf("after the restart: %s", got)
	}
	if got := cmUp(t, d, "Module"); got != nousModule {
		t.Errorf("module: %s", got)
	}
}

func TestTemplateNameChangesTheModuleName(t *testing.T) {
	// On Module 0 the module is named after the template; the fleet view groups by this name.
	d := nousDevice(t, DeviceSpec{})
	cm(t, d, `Template {"NAME":"Renamed"}`)
	waitBoots(t, d, 1)
	if got := cmUp(t, d, "Module"); got != `{"Module":{"0":"Renamed"}}` {
		t.Errorf("module: %s", got)
	}
}

func TestModuleZeroAlwaysRestartsAndClearsGpioOnlyOnAChange(t *testing.T) {
	// T4: Module n restarts even when the module does not change, and only a real change clears the per-pin
	// Gpio settings. A planner that sends "Module 0" to a device already on Module 0 must see a restart
	// but keep the user's Gpio choices.
	tpl := UserTemplate{Name: "UserSlots", GPIO: []int{32, 1, 0, 0, 0, 0, 0, 0, 224, 320, 0, 0, 0, 0}, Base: 18}
	d := nousDevice(t, DeviceSpec{Template: &tpl})
	if got := cm(t, d, "Gpio1 224"); got != `{"GPIO1":{"Relay1":224}}` {
		t.Fatalf("Gpio1: %s", got)
	}
	waitBoots(t, d, 1)
	if got := cmUp(t, d, "Gpio"); got != `{"GPIO1":{"Relay1":224}}` {
		t.Fatalf("override lost by the restart: %s", got)
	}

	cm(t, d, "Module 0")
	waitBoots(t, d, 2) // restarted although the module is the same
	if got := cmUp(t, d, "Gpio"); got != `{"GPIO1":{"Relay1":224}}` {
		t.Errorf("Module 0 on Module 0 cleared the override: %s", got)
	}

	if got := cmUp(t, d, "Module 1"); got != `{"Module":{"1":"Sonoff Basic"}}` {
		t.Fatalf("Module 1: %s", got)
	}
	waitBoots(t, d, 3)
	cmUp(t, d, "Module 0")
	waitBoots(t, d, 4)
	if got := cmUp(t, d, "Gpio"); got != `{"GPIO1":{"None":0}}` {
		t.Errorf("changing the module kept the override: %s", got)
	}
}

func TestModuleRefusesAnUnknownNumber(t *testing.T) {
	// An invalid module number changes nothing and does not restart (CmndModule only acts when "present").
	d := nousDevice(t, DeviceSpec{})
	if got := cm(t, d, "Module 9999"); got != nousModule {
		t.Errorf("reply %s", got)
	}
	settle()
	if d.Boots() != 0 {
		t.Error("restarted for an unknown module")
	}
}

func TestTemplateJSONIsPartial(t *testing.T) {
	// T6: every field is optional and a missing one keeps its value; BASE 0 or an unknown module becomes 18
	// (Generic). This is why the planner must always send the full array: a short one would silently keep old pins.
	d := nousDevice(t, DeviceSpec{})
	got := cm(t, d, `Template {"GPIO":[160,160]}`)
	want := `{"NAME":"NOUS A1T","ARCH":"ESP8266","GPIO":[160,160,0,0,2720,2656,0,0,2624,320,224,0,0,0],"FLAG":0,"BASE":49}`
	if got != want {
		t.Fatalf("short array: %s", got)
	}
	waitBoots(t, d, 1)
	for i, base := range []string{"0", "9999"} {
		got = cmUp(t, d, `Template {"BASE":`+base+`}`)
		if !strings.HasSuffix(got, `"BASE":18}`) {
			t.Errorf("BASE %s: %s", base, got)
		}
		waitBoots(t, d, 2+i)
	}
	if got = cmUp(t, d, `Template {"GPIO":[],"FLAG":2,"BASE":1}`); !strings.HasSuffix(got, `"FLAG":2,"BASE":1}`) {
		t.Errorf("flag with GPIO, and base: %s", got)
	}
}

func TestTemplateRefusesTheWrongChip(t *testing.T) {
	// T7: ARCH is checked against the firmware's own. The refusal is the only protection against a template of another chip,
	// so the simulator must apply it, change nothing and not restart.
	d := nousDevice(t, DeviceSpec{})
	for _, in := range []string{
		`Template {"NAME":"X","ARCH":"ESP32","GPIO":[0],"FLAG":0,"BASE":1}`,
		`Template {"NAME"`,
		`Template {}`,
	} {
		if got := cm(t, d, in); got != `{"Template":"Invalid JSON or Bad Chip Type"}` {
			t.Errorf("%s: %s", in, got)
		}
	}
	settle()
	if d.Boots() != 0 {
		t.Error("restarted after a refused template")
	}
	if got := cm(t, d, "Template"); got != nousTemplate {
		t.Errorf("a refused template changed the stored one: %s", got)
	}
	// The device's own ARCH is accepted (ESP8266 reports "ESP8266").
	if got := cm(t, d, `Template {"NAME":"Ok","ARCH":"ESP8266"}`); !strings.Contains(got, `"NAME":"Ok"`) {
		t.Errorf("own ARCH refused: %s", got)
	}
}

func TestTemplateArchFollowsTheChip(t *testing.T) {
	// ArchName upper-cases TASMOTA_ARCH: a device reports the arch of its own chip, so a template for an
	// ESP32-S3 is refused by a C3 although both have more than 21 pins.
	d := add(t, newTestFleet(t, "", Extra{}), DeviceSpec{Name: "c3", Profile: ProfileESP32C3})
	got := cm(t, d, "Template")
	if !strings.Contains(got, `"ARCH":"ESP32C3"`) || strings.Count(got, ",") < 22 {
		t.Fatalf("c3 template: %s", got)
	}
	if got := cm(t, d, `Template {"ARCH":"ESP32S3","NAME":"x"}`); got != `{"Template":"Invalid JSON or Bad Chip Type"}` {
		t.Errorf("S3 template on a C3: %s", got)
	}
}

func TestDeviceSpecTemplateAndModule(t *testing.T) {
	// The devenv makes one NOUS plug deviate by starting it with another template; the module option starts a device on a built-in module.
	deviant := UserTemplate{Name: "NOUS A1T", GPIO: []int{32, 0, 0, 0, 2720, 2656, 0, 0, 2624, 224, 320, 0, 0, 0}, Base: 49}
	d := nousDevice(t, DeviceSpec{Template: &deviant})
	want := `{"NAME":"NOUS A1T","ARCH":"ESP8266","GPIO":[32,0,0,0,2720,2656,0,0,2624,224,320,0,0,0],"FLAG":0,"BASE":49}`
	if got := cm(t, d, "Template"); got != want {
		t.Errorf("template %s", got)
	}
	one := 1
	d2 := nousDevice(t, DeviceSpec{Name: "other", Module: &one})
	if got := cm(t, d2, "Module"); got != `{"Module":{"1":"Sonoff Basic"}}` {
		t.Errorf("module %s", got)
	}
	if got := cm(t, d2, "Template"); got != nousTemplate {
		t.Errorf("stored template of a device on a built-in module: %s", got)
	}
	f := newTestFleet(t, "", Extra{})
	for name, spec := range map[string]DeviceSpec{
		"wrong length":   {Name: "a", Profile: ProfileNOUSA1TBambu, Template: &UserTemplate{Name: "x", GPIO: []int{0}, Base: 1}},
		"unknown module": {Name: "b", Profile: ProfileNOUSA1TBambu, Module: func() *int { n := 9999; return &n }()},
		"base zero":      {Name: "c", Profile: ProfileNOUSA1TBambu, Template: &UserTemplate{Name: "x", GPIO: make([]int, 14)}},
	} {
		if _, err := f.Add(ctxT(t), spec); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestStatusReportsTheModule(t *testing.T) {
	// The prober reads plain Status for the module (T2); after Module 1 it must say 1, or the fleet view
	// would keep treating the device as on its template.
	d := nousDevice(t, DeviceSpec{})
	if !strings.Contains(cm(t, d, "Status"), `"Module":0,`) {
		t.Fatal("start: not Module 0")
	}
	cm(t, d, "Module 1")
	waitBoots(t, d, 1)
	for _, c := range []string{"Status", "Status 0"} {
		if got := cmUp(t, d, c); !strings.Contains(got, `"Module":1,`) {
			t.Errorf("%s: %s", c, got)
		}
	}
}

func TestTemplateFlagNeedsGPIO(t *testing.T) {
	// JsonTemplate reads FLAG only inside the GPIO block (support.ino:1944-1990): a FLAG sent alone is silently
	// ignored. A planner that relied on FLAG alone would believe it had changed something.
	d := nousDevice(t, DeviceSpec{})
	if got := cm(t, d, `Template {"FLAG":2}`); got != nousTemplate {
		t.Errorf("FLAG alone changed the template: %s", got)
	}
	waitBoots(t, d, 1) // it still restarts: the JSON was valid
	if got := cmUp(t, d, `Template {"GPIO":[32],"FLAG":2}`); !strings.Contains(got, `"FLAG":2`) {
		t.Errorf("FLAG with GPIO ignored: %s", got)
	}
}

func TestMinimalImageHasNoTemplateCommands(t *testing.T) {
	// The Template command is not in the command table of the minimal image at all (support_command.ino:25,
	// the #ifndef FIRMWARE_MINIMAL around the table), so it answers Unknown and changes nothing; the
	// read-only guard inside CmndTemplate is unreachable there.
	d := add(t, newTestFleet(t, "", Extra{}), DeviceSpec{Name: "min", Profile: ProfileESP8266Min})
	for _, c := range []string{`Template {"NAME":"X","GPIO":[0],"FLAG":0,"BASE":1}`, "Template", "Module 0", "Module"} {
		if got := cm(t, d, c); !strings.HasPrefix(got, `{"Command":"Unknown"`) {
			t.Errorf("%s: %s", c, got)
		}
	}
}
