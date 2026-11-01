package simulator

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/tasmotacmd"
)

// Template, Module and the per-pin Gpio overrides, as Tasmota 16.0.0 does them (plan-0270-gpio.md
// 1.1; sources: CmndTemplate and CmndModule in support_command.ino, JsonTemplate and TemplateJson
// in support.ino). State lives in the settings kv like every other setting, so it follows the same
// RAM/flash rules; a device nobody changed replays its capture byte for byte.
//
//	kv["template"]  the stored user template (JSON of tplState)
//	kv["module"]    the active module, "0" = the user template
//	kv["gpio.<n>"]  Gpio override of physical pin n (only for User slots)
//
// Known gaps, all of them forms nothing in TasmoFleet sends: Template with a module number
// ("Template 5", "Template 255"), Module2 (fallback module), the CMND key of a template (not
// executed), the old 13-entry ESP8266 template format.

type tplState struct {
	Name string `json:"name"`
	GPIO []int  `json:"gpio"`
	Flag int    `json:"flag"`
	Base int    `json:"base"`
}

const genericBase = 18 // JsonTemplate: an invalid BASE becomes Generic

// templateArch is ArchName(): the upper-cased TASMOTA_ARCH of the chip.
func (d *simDevice) templateArch() string { return strings.ToUpper(d.def.Chip) }

// moduleList reads the capture's Modules reply: module number -> name (number 0 is the template, skipped).
func (d *simDevice) moduleList() map[int]string {
	var raw struct {
		Modules map[string]string `json:"Modules"`
	}
	out := map[int]string{}
	if json.Unmarshal(d.cap.files["modules.json"], &raw) != nil {
		return out
	}
	for k, v := range raw.Modules {
		if n, err := strconv.Atoi(k); err == nil && n >= 1 {
			out[n] = v
		}
	}
	return out
}

// initTemplate applies DeviceSpec.Template and Module and fits a captured template to the chip.
func (d *simDevice) initTemplate() error {
	pins, known := tasmotacmd.GPIO().TemplatePins(d.def.Chip)
	if t := d.spec.Template; t != nil {
		if known && len(t.GPIO) != pins {
			return fmt.Errorf("simulator: template has %d slots, %s has %d", len(t.GPIO), d.def.Chip, pins)
		}
		if _, ok := d.moduleList()[t.Base]; !ok {
			return fmt.Errorf("simulator: template base %d is not a module of this capture", t.Base)
		}
		d.persisted.kv["template"] = encodeTpl(tplState{Name: t.Name, GPIO: slices.Clone(t.GPIO), Flag: t.Flag, Base: t.Base})
	} else if cur, ok := d.captureTemplate(); ok && known && len(cur.GPIO) != pins {
		// A synthetic profile changes the chip but keeps the captured pin map; fit the array so the
		// device is at least self-consistent.
		cur.GPIO = append(slices.Clone(cur.GPIO), make([]int, max(0, pins-len(cur.GPIO)))...)[:pins]
		d.persisted.kv["template"] = encodeTpl(cur)
	}
	if m := d.spec.Module; m != nil {
		if _, ok := d.moduleList()[*m]; *m != 0 && !ok {
			return fmt.Errorf("simulator: module %d is not in this capture's Modules list", *m)
		}
		d.persisted.kv["module"] = strconv.Itoa(*m)
	}
	return nil
}

func encodeTpl(t tplState) string {
	b, _ := json.Marshal(t) // plain struct: cannot fail
	return string(b)
}

func (d *simDevice) captureTemplate() (tplState, bool) {
	var raw struct {
		Name string `json:"NAME"`
		GPIO []int  `json:"GPIO"`
		Flag int    `json:"FLAG"`
		Base int    `json:"BASE"`
	}
	if json.Unmarshal(d.cap.files["template.json"], &raw) != nil || raw.GPIO == nil {
		return tplState{}, false
	}
	return tplState{Name: raw.Name, GPIO: raw.GPIO, Flag: raw.Flag, Base: raw.Base}, true
}

func (d *simDevice) templateLocked() (tplState, bool) {
	if s, ok := d.runtime.kv["template"]; ok {
		var t tplState
		if json.Unmarshal([]byte(s), &t) == nil {
			return t, true
		}
	}
	return d.captureTemplate()
}

func (d *simDevice) moduleNumberLocked() int {
	if s, ok := d.runtime.kv["module"]; ok {
		n, _ := strconv.Atoi(s)
		return n
	}
	var raw struct {
		Module map[string]string `json:"Module"`
	}
	if json.Unmarshal(d.cap.files["module.json"], &raw) == nil {
		for k := range raw.Module {
			n, _ := strconv.Atoi(k)
			return n
		}
	}
	return 0
}

// templateReply is TemplateJson().
func (d *simDevice) templateReply(t tplState) []byte {
	var b strings.Builder
	b.WriteString(`{"NAME":` + jsonString(t.Name) + `,"ARCH":` + jsonString(d.templateArch()) + `,"GPIO":[`)
	for i, v := range t.GPIO {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(v))
	}
	b.WriteString(`],"FLAG":` + strconv.Itoa(t.Flag) + `,"BASE":` + strconv.Itoa(t.Base) + `}`)
	return []byte(b.String())
}

func (d *simDevice) moduleReplyLocked(t tplState) []byte {
	n := d.moduleNumberLocked()
	name := t.Name
	if n != 0 {
		name = d.moduleList()[n]
	}
	return []byte(`{"Module":{"` + strconv.Itoa(n) + `":` + jsonString(name) + `}}`)
}

// afterChangeLocked is the end of a command that changed settings: queue the restart (restart_flag
// = 2, which saves and restarts about 2 s later) or, with SaveData > 0, save at once.
func (d *simDevice) afterChangeLocked(restart bool) {
	if restart {
		if !d.restartQueued && d.phase == PhaseRun {
			d.restartQueued = true
			d.spawn(d.restartSoon)
		}
		return
	}
	if d.get("savedata") != "0" {
		d.commitLocked()
	}
}

// templateCmdLocked executes Template, Module and the Gpio forms that need the template state; ok is
// false for everything else, and for a device nobody has touched, whose capture is replayed.
func (d *simDevice) templateCmdLocked(base string, idx int, hasIdx bool, data string) (body []byte, ok bool) {
	t, have := d.templateLocked()
	if !have {
		return nil, false
	}
	_, touchedT := d.runtime.kv["template"]
	_, touchedM := d.runtime.kv["module"]
	switch base {
	case "template":
		if hasIdx {
			return nil, false
		}
		if !strings.Contains(data, "{") { // bare read, or a parameter form (not modelled: the stored template is shown)
			if !touchedT {
				return nil, false
			}
			return d.templateReply(t), true
		}
		nt, err := d.parseTemplateJSON(t, data)
		if err != nil {
			return []byte(`{"Template":"Invalid JSON or Bad Chip Type"}`), true
		}
		d.set("template", encodeTpl(nt))
		d.afterChangeLocked(d.moduleNumberLocked() == 0)
		return d.templateReply(nt), true
	case "module":
		if hasIdx && idx != 1 {
			return nil, false
		}
		n, err := strconv.Atoi(data)
		restart := false
		if data != "" && err == nil {
			if _, listed := d.moduleList()[n]; n == 0 || listed {
				if d.moduleNumberLocked() != n {
					d.clearGpioLocked()
				}
				d.set("module", strconv.Itoa(n))
				restart = true // CmndModule: restart_flag = 2 even when the module stays the same
			}
		}
		if restart {
			d.afterChangeLocked(true)
		}
		if !restart && !touchedM && !touchedT {
			return nil, false
		}
		return d.moduleReplyLocked(t), true
	case "gpio":
		return d.gpioCmdLocked(t, idx, hasIdx, data)
	}
	return nil, false
}

// parseTemplateJSON is JsonTemplate(): every key is optional, a missing one keeps the old value.
func (d *simDevice) parseTemplateJSON(old tplState, data string) (tplState, error) {
	if len(data) < 9 {
		return tplState{}, fmt.Errorf("too short")
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &root); err != nil || root == nil {
		return tplState{}, fmt.Errorf("invalid JSON")
	}
	t := old
	t.GPIO = slices.Clone(old.GPIO)
	if v, ok := root["ARCH"]; ok {
		var arch string
		if json.Unmarshal(v, &arch) != nil || arch != d.templateArch() {
			return tplState{}, fmt.Errorf("bad chip type")
		}
	}
	if v, ok := root["NAME"]; ok {
		var name string
		if json.Unmarshal(v, &name) == nil {
			t.Name = name
		}
	}
	// FLAG is read inside the GPIO block of JsonTemplate: without a GPIO array it is ignored.
	if v, ok := root["GPIO"]; ok {
		var arr []json.RawMessage
		if json.Unmarshal(v, &arr) == nil {
			for i := 0; i < len(t.GPIO) && i < len(arr); i++ {
				t.GPIO[i] = uintOf(arr[i])
			}
			if f, ok := root["FLAG"]; ok {
				t.Flag = uintOf(f)
			}
		}
	}
	if v, ok := root["BASE"]; ok {
		b := uintOf(v)
		if _, listed := d.moduleList()[b]; b == 0 || !listed {
			b = genericBase
		}
		t.Base = b
	}
	return t, nil
}

// uintOf is JsonParserToken::getUInt(): a number, or a numeric string; anything else is 0.
func uintOf(raw json.RawMessage) int {
	var f float64
	if json.Unmarshal(raw, &f) != nil {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return 0
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0
		}
		f = float64(n)
	}
	if f < 0 {
		return 0
	}
	return int(f)
}

func (d *simDevice) clearGpioLocked() {
	for k := range d.runtime.kv {
		if strings.HasPrefix(k, "gpio.") {
			delete(d.runtime.kv, k)
		}
	}
}

// userPins lists the physical pins whose template slot is User (value 1): the only ones a Gpio setting may take.
func (d *simDevice) userPins(t tplState) []int {
	var pins []int
	for slot, v := range t.GPIO {
		if v != 1 {
			continue
		}
		if p, ok := tasmotacmd.GPIO().PhysicalPin(d.def.Chip, slot); ok {
			pins = append(pins, p)
		}
	}
	sort.Ints(pins)
	return pins
}

// gpioCmdLocked: "Gpio" lists the User pins, "Gpio<n> <code>" sets one (CmndGpio). Only on Module 0
// is the pin map the stored template; on a built-in module the capture is replayed.
func (d *simDevice) gpioCmdLocked(t tplState, idx int, hasIdx bool, data string) (body []byte, ok bool) {
	if d.moduleNumberLocked() != 0 {
		return nil, false
	}
	pins := d.userPins(t)
	restart := false
	switch {
	case hasIdx && data != "":
		code, err := strconv.Atoi(data)
		if err != nil || code < 0 || !slices.Contains(pins, idx) {
			break
		}
		d.set("gpio."+strconv.Itoa(idx), strconv.Itoa(code))
		restart = true
	case hasIdx || data != "":
		return nil, false // other forms of Gpio are not modelled
	}
	d.afterChangeLocked(restart)
	if len(pins) == 0 {
		return []byte(`{"GPIO":"Not supported"}`), true
	}
	parts := make([]string, 0, len(pins))
	for _, p := range pins {
		code, _ := strconv.Atoi(d.runtime.kv["gpio."+strconv.Itoa(p)])
		parts = append(parts, `"GPIO`+strconv.Itoa(p)+`":{`+jsonString(tasmotacmd.GPIO().FunctionName(d.def.Chip, code))+`:`+strconv.Itoa(code)+`}`)
	}
	return []byte("{" + strings.Join(parts, ",") + "}"), true
}
