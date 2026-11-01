package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/tasmotacmd"
)

// GPIO templates (#327): compare the pin maps of devices that share a template, name the
// differences, and decide for each device whether the reference template may be applied. Pure:
// readers and the engine are WP-B, the pages WP-C. SAFETY package, mutation tested.
//
// Facts used (Tasmota 16.0.0, plan-0270-gpio.md section 1.1), numbered T1.. as there:
// Template answers the stored template even on a built-in module (T1); Template {json} restarts
// the device only on Module 0 (T3); JSON fields are optional on the device, so all are sent (T6);
// ARCH is checked from 15.x on (T7); CMND would be executed (T8).

// GPIOTemplate is a Template reply, or the template TasmoFleet sends.
type GPIOTemplate struct {
	Name string
	// Arch is the ARCH of the reply ("ESP8266", "ESP32", ...), "" when the firmware does not report it (before 15.x).
	Arch string
	// GPIO holds one value per template slot, the way the JSON spells it: 0 None, 1 User, else function<<5 | instance.
	GPIO []int
	Flag int
	Base int
}

// Fingerprint identifies the layout (GPIO, FLAG, BASE); NAME is the group key and ARCH is not
// reported by old firmware, so neither is part of the identity (plan 2.2).
func (t GPIOTemplate) Fingerprint() string {
	var b strings.Builder
	for i, v := range t.GPIO {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(v))
	}
	return b.String() + "|" + strconv.Itoa(t.Flag) + "|" + strconv.Itoa(t.Base)
}

// ParseTemplate reads the reply of the bare Template command. Anything it cannot fully trust is an error.
func ParseTemplate(reply []byte) (GPIOTemplate, error) {
	var raw struct {
		Name *string           `json:"NAME"`
		Arch string            `json:"ARCH"`
		GPIO []json.RawMessage `json:"GPIO"`
		Flag *json.Number      `json:"FLAG"`
		Base *json.Number      `json:"BASE"`
	}
	dec := json.NewDecoder(bytes.NewReader(reply))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return GPIOTemplate{}, fmt.Errorf("template: %w", err)
	}
	if raw.Name == nil || raw.GPIO == nil || raw.Flag == nil || raw.Base == nil {
		return GPIOTemplate{}, errors.New("template: reply lacks NAME, GPIO, FLAG or BASE")
	}
	if len(raw.GPIO) == 0 {
		return GPIOTemplate{}, errors.New("template: GPIO is empty")
	}
	t := GPIOTemplate{Name: *raw.Name, Arch: raw.Arch}
	for i, v := range raw.GPIO {
		n, err := wholeNumber(string(v), 0xFFFF)
		if err != nil {
			return GPIOTemplate{}, fmt.Errorf("template: GPIO[%d]: %w", i, err)
		}
		t.GPIO = append(t.GPIO, n)
	}
	var err error
	if t.Flag, err = wholeNumber(raw.Flag.String(), 0xFFFF); err != nil {
		return GPIOTemplate{}, fmt.Errorf("template: FLAG: %w", err)
	}
	if t.Base, err = wholeNumber(raw.Base.String(), 0xFF); err != nil || t.Base < 1 {
		return GPIOTemplate{}, errors.New("template: BASE is not a module number")
	}
	return t, nil
}

func wholeNumber(s string, limit int) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > limit {
		return 0, fmt.Errorf("%q is not a whole number between 0 and %d", s, limit)
	}
	return n, nil
}

// ModuleReading is the reply of the bare Module command: {"Module":{"<n>":"<name>"}}.
type ModuleReading struct {
	Number int // 0 is the user template (T2), n >= 1 a built-in module
	Name   string
}

// ParseModule reads the reply of the bare Module command.
func ParseModule(reply []byte) (ModuleReading, error) {
	var raw struct {
		Module map[string]string `json:"Module"`
	}
	if err := json.Unmarshal(reply, &raw); err != nil {
		return ModuleReading{}, fmt.Errorf("module: %w", err)
	}
	if len(raw.Module) != 1 {
		return ModuleReading{}, errors.New("module: reply does not hold exactly one module")
	}
	for k, name := range raw.Module {
		n, err := wholeNumber(k, 0xFF)
		if err != nil {
			return ModuleReading{}, fmt.Errorf("module: %w", err)
		}
		return ModuleReading{Number: n, Name: name}, nil
	}
	panic("unreachable") // the map has exactly one entry
}

// TemplateState says what a device's Module reading means for template comparison.
type TemplateState int

// States.
const (
	// TemplateUnknown: no Module reading, no Template reading on Module 0, or no known chip family.
	TemplateUnknown TemplateState = iota
	// OnTemplate: Module 0, the stored template is the active pin map.
	OnTemplate
	// OnModule: a built-in module; the stored template is inactive (T1) and ignored.
	OnModule
)

// TemplateDevice is what the planner knows about one device: its identity facts and its readings.
type TemplateDevice struct {
	ID      model.DeviceID
	Family  model.ChipFamily
	Variant string // model.Identity.Variant
	Wired   bool   // Ethernet board
	// MQTTOnly: the device is reached over MQTT only (no HTTP).
	MQTTOnly bool
	// Live: the readings were taken now. Readings from a backup are shown but never applied (E5).
	Live     bool
	Module   *ModuleReading
	Template *GPIOTemplate
}

// State classifies the device.
func (d TemplateDevice) State() TemplateState {
	switch {
	case d.Family == model.ChipUnknown || d.Module == nil:
		return TemplateUnknown
	case d.Module.Number >= 1:
		return OnModule
	case d.Template == nil:
		return TemplateUnknown
	}
	return OnTemplate
}

// GroupKey is the group of devices that are compared (decision Q1: template NAME plus chip family).
type GroupKey struct {
	Family model.ChipFamily
	// Name is the template NAME, or for BuiltIn the name of the built-in module.
	Name string
	// BuiltIn groups devices on a built-in module; they are listed, never compared (Q2).
	BuiltIn bool
}

// TemplateGroup is one group with its devices, sorted by device id.
type TemplateGroup struct {
	Key     GroupKey
	Devices []TemplateDevice
}

// GroupTemplates splits the devices into groups, sorted by family, name and built-in flag.
// Devices whose state is unknown are returned separately.
func GroupTemplates(devs []TemplateDevice) (groups []TemplateGroup, notJudged []TemplateDevice) {
	idx := map[GroupKey]int{}
	for _, d := range devs {
		var key GroupKey
		switch d.State() {
		case OnTemplate:
			key = GroupKey{Family: d.Family, Name: d.Template.Name}
		case OnModule:
			key = GroupKey{Family: d.Family, Name: d.Module.Name, BuiltIn: true}
		case TemplateUnknown:
			notJudged = append(notJudged, d)
			continue
		}
		i, ok := idx[key]
		if !ok {
			i = len(groups)
			idx[key] = i
			groups = append(groups, TemplateGroup{Key: key})
		}
		groups[i].Devices = append(groups[i].Devices, d)
	}
	sort.Slice(groups, func(i, j int) bool {
		a, b := groups[i].Key, groups[j].Key
		if a.Family != b.Family {
			return a.Family < b.Family
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return !a.BuiltIn && b.BuiltIn
	})
	for i := range groups {
		sortByID(groups[i].Devices)
	}
	sortByID(notJudged)
	return groups, notJudged
}

func sortByID(ds []TemplateDevice) {
	sort.Slice(ds, func(i, j int) bool { return ds[i].ID < ds[j].ID })
}

// RefHow says how the reference was chosen.
type RefHow string

// Ways to choose a reference.
const (
	RefMostCommon RefHow = "most-common"
	RefChosen     RefHow = "chosen"
)

// Reference is the template the other devices of a group are compared to.
type Reference struct {
	Device TemplateDevice
	How    RefHow
	// Count of the live devices of the group that have the reference's layout, Of the live devices of the group.
	Count, Of int
}

// liveOnTemplate returns the devices that may vote or be chosen.
func (g TemplateGroup) liveOnTemplate() []TemplateDevice {
	if g.Key.BuiltIn {
		return nil
	}
	var out []TemplateDevice
	for _, d := range g.Devices {
		if d.Live && d.State() == OnTemplate {
			out = append(out, d)
		}
	}
	return out
}

func (g TemplateGroup) count(layout string, live []TemplateDevice) int {
	n := 0
	for _, d := range live {
		if d.Template.Fingerprint() == layout {
			n++
		}
	}
	return n
}

// MostCommon returns the layout used by most live devices of the group, represented by the one with
// the lowest id. A tie for the top, or fewer than two live devices, gives no reference: it never guesses.
func MostCommon(g TemplateGroup) (Reference, bool) {
	live := g.liveOnTemplate()
	if len(live) < 2 {
		return Reference{}, false
	}
	counts := map[string]int{}
	for _, d := range live {
		counts[d.Template.Fingerprint()]++
	}
	best, bestN, tie := "", 0, false
	for fp, n := range counts {
		switch {
		case n > bestN:
			best, bestN, tie = fp, n, false
		case n == bestN:
			tie = true
		}
	}
	if tie {
		return Reference{}, false
	}
	for _, d := range live { // live is sorted by id
		if d.Template.Fingerprint() == best {
			return Reference{Device: d, How: RefMostCommon, Count: bestN, Of: len(live)}, true
		}
	}
	return Reference{}, false
}

// ChosenReference makes a live device of the group the reference.
func ChosenReference(g TemplateGroup, id model.DeviceID) (Reference, error) {
	live := g.liveOnTemplate()
	for _, d := range live {
		if d.ID == id {
			return Reference{Device: d, How: RefChosen, Count: g.count(d.Template.Fingerprint(), live), Of: len(live)}, nil
		}
	}
	return Reference{}, fmt.Errorf("device %d cannot be the reference: it is not a live device of this group on a user template", id)
}

// Kinds of DiffLine.
const (
	DiffGPIO = "gpio"
	DiffFlag = "flag"
	DiffBase = "base"
)

// DiffLine is one difference between a device's template and the reference, as "device value -> reference value".
type DiffLine struct {
	Kind string
	// Slot and Pin are set for DiffGPIO: the template slot and the physical GPIO number.
	Slot, Pin int
	// Have is the device's value, Want the reference's, as text ("Relay1", "49").
	Have, Want string
	// HaveCode and WantCode are the raw GPIO values of a DiffGPIO line.
	HaveCode, WantCode int
	// Text is the line as shown: "GPIO4: Relay1 -> Led_i1", "BASE 49 -> 18".
	Text string
}

// DiffTemplates lists how have differs from want: pins by physical number first, then FLAG, then BASE.
// The family must be known and both arrays must have its length, else no pin number can be named.
func DiffTemplates(family model.ChipFamily, have, want GPIOTemplate) ([]DiffLine, error) {
	tab := tasmotacmd.GPIO()
	fam := string(family)
	pins, ok := tab.TemplatePins(fam)
	if !ok {
		return nil, fmt.Errorf("diff: unknown chip family %q", fam)
	}
	if len(have.GPIO) != pins || len(want.GPIO) != pins {
		return nil, fmt.Errorf("diff: %s has %d template slots, got %d and %d", fam, pins, len(have.GPIO), len(want.GPIO))
	}
	var out []DiffLine
	for i := range have.GPIO {
		if have.GPIO[i] == want.GPIO[i] {
			continue
		}
		pin, _ := tab.PhysicalPin(fam, i) // i < pins, so the pin exists
		h, w := tab.FunctionName(fam, have.GPIO[i]), tab.FunctionName(fam, want.GPIO[i])
		out = append(out, DiffLine{Kind: DiffGPIO, Slot: i, Pin: pin, Have: h, Want: w,
			HaveCode: have.GPIO[i], WantCode: want.GPIO[i], Text: fmt.Sprintf("GPIO%d: %s -> %s", pin, h, w)})
	}
	if have.Flag != want.Flag {
		out = append(out, DiffLine{Kind: DiffFlag, Have: strconv.Itoa(have.Flag), Want: strconv.Itoa(want.Flag), Text: fmt.Sprintf("FLAG %d -> %d", have.Flag, want.Flag)})
	}
	if have.Base != want.Base {
		out = append(out, DiffLine{Kind: DiffBase, Have: strconv.Itoa(have.Base), Want: strconv.Itoa(want.Base), Text: fmt.Sprintf("BASE %d -> %d", have.Base, want.Base)})
	}
	return out, nil
}

// SafeTemplateName reports whether name may be sent inside a Template command: non-empty, printable,
// and without the characters that break the JSON (" and \) or split the command line (;).
func SafeTemplateName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r == '"' || r == '\\' || r == ';' || !unicode.IsPrint(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

// BuildTemplateCommand builds the Template command for a device from the parsed fields only:
// NAME, ARCH (only if targetArch, the ARCH the target itself reported, is not empty), GPIO, FLAG, BASE.
// Nothing else of the reply is passed on, CMND least of all (T8).
func BuildTemplateCommand(t GPIOTemplate, targetArch string) (string, error) {
	if !SafeTemplateName(t.Name) {
		return "", fmt.Errorf("template name %q cannot be sent safely", t.Name)
	}
	if len(t.GPIO) == 0 || t.Base < 1 {
		return "", errors.New("template is incomplete")
	}
	var b strings.Builder
	b.WriteString(`Template {"NAME":"` + t.Name + `"`)
	if targetArch != "" {
		if strings.ContainsAny(targetArch, `"\;`) {
			return "", fmt.Errorf("arch %q cannot be sent safely", targetArch)
		}
		b.WriteString(`,"ARCH":"` + targetArch + `"`)
	}
	b.WriteString(`,"GPIO":[`)
	for i, v := range t.GPIO {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(v))
	}
	b.WriteString(`],"FLAG":` + strconv.Itoa(t.Flag) + `,"BASE":` + strconv.Itoa(t.Base) + `}`)
	return b.String(), nil
}

// Refusal is why a template may not be applied to a device; "" means it may.
type Refusal string

// Refusals. The Reason of an ApplyPlan is the sentence for the preview.
const (
	RefuseNone          Refusal = ""
	RefuseNotLive       Refusal = "not-live"        // reading from a backup (E5)
	RefuseNotOnTemplate Refusal = "not-on-template" // built-in module or unread (Q2)
	RefuseFamily        Refusal = "family"          // unknown or different chip family
	RefuseOtherGroup    Refusal = "other-group"     // different template NAME
	RefuseLength        Refusal = "length"          // GPIO array length differs (T9)
	RefuseArch          Refusal = "arch"            // both reported ARCH, they differ (T7)
	RefuseMinimal       Refusal = "minimal"         // Template is read-only there (T14)
	RefuseUnsafeName    Refusal = "unsafe-name"
	RefuseEthernet      Refusal = "ethernet" // wired board, the change touches an ETH_* pin
	RefuseMQTTOnly      Refusal = "mqtt-only"
)

// PlanOptions are decisions of the caller.
type PlanOptions struct {
	// StrictMQTT: the strict Template matcher exists, so an MQTT-only device can be read and verified (E3).
	StrictMQTT bool
}

// ApplyPlan is the verdict for one device.
type ApplyPlan struct {
	Device model.DeviceID
	// Matches: the device already has the reference's layout; nothing to do, no refusal.
	Matches bool
	Diff    []DiffLine
	Refusal Refusal
	Reason  string
	// Commands are sent in order, as plain commands (not a Backlog, E6). One today: Template on Module 0.
	Commands []string
	// Restarts: the device restarts after the commands (T3).
	Restarts bool
}

func refuse(p ApplyPlan, r Refusal, format string, a ...any) ApplyPlan {
	p.Refusal, p.Reason = r, fmt.Sprintf(format, a...)
	return p
}

// PlanApply decides whether the reference template may be applied to dev, and builds the command.
// The checks run in the order that makes the later ones meaningful: a device on another family is
// refused before its array length is looked at.
func PlanApply(ref Reference, dev TemplateDevice, opts PlanOptions) ApplyPlan {
	p := ApplyPlan{Device: dev.ID}
	if dev.ID == ref.Device.ID {
		p.Matches = true
		return p
	}
	if !dev.Live {
		return refuse(p, RefuseNotLive, "the template was read from a backup; only live readings are applied")
	}
	if dev.Family == model.ChipUnknown || dev.Family != ref.Device.Family {
		return refuse(p, RefuseFamily, "chip family %q differs from the reference's %q; a template is never copied across families", dev.Family, ref.Device.Family)
	}
	if ref.Device.State() != OnTemplate || dev.State() != OnTemplate {
		return refuse(p, RefuseNotOnTemplate, "the device runs a built-in module or its template was not read; switching modules is not offered")
	}
	rt, dt := *ref.Device.Template, *dev.Template
	if rt.Name != dt.Name {
		return refuse(p, RefuseOtherGroup, "template name %q differs from the reference's %q", dt.Name, rt.Name)
	}
	pins, known := tasmotacmd.GPIO().TemplatePins(string(dev.Family))
	if !known || len(rt.GPIO) != len(dt.GPIO) || len(dt.GPIO) != pins {
		return refuse(p, RefuseLength, "the GPIO array has %d entries, the reference's %d, the chip's %d", len(dt.GPIO), len(rt.GPIO), pins)
	}
	diff, err := DiffTemplates(dev.Family, dt, rt)
	if err != nil { // checked above; kept so a change in DiffTemplates cannot slip through
		return refuse(p, RefuseLength, "%v", err)
	}
	if len(diff) == 0 {
		p.Matches = true
		return p
	}
	p.Diff = diff
	if rt.Arch != "" && dt.Arch != "" && rt.Arch != dt.Arch {
		return refuse(p, RefuseArch, "the device reports ARCH %s, the reference %s", dt.Arch, rt.Arch)
	}
	if dev.Variant == "minimal" {
		return refuse(p, RefuseMinimal, "the minimal firmware cannot change the template")
	}
	cmd, err := BuildTemplateCommand(rt, dt.Arch)
	if err != nil {
		return refuse(p, RefuseUnsafeName, "%v", err)
	}
	if dev.Wired && touchesEthernet(dev.Family, diff) {
		return refuse(p, RefuseEthernet, "the change touches an Ethernet pin of a wired device; it would cut the device off")
	}
	if dev.MQTTOnly && !opts.StrictMQTT {
		return refuse(p, RefuseMQTTOnly, "the device is reached over MQTT only; its template cannot be read back reliably")
	}
	p.Commands = []string{cmd}
	p.Restarts = true
	return p
}

func touchesEthernet(family model.ChipFamily, diff []DiffLine) bool {
	tab := tasmotacmd.GPIO()
	return slices.ContainsFunc(diff, func(l DiffLine) bool {
		return l.Kind == DiffGPIO && (tab.IsEthernet(string(family), l.HaveCode) || tab.IsEthernet(string(family), l.WantCode))
	})
}
