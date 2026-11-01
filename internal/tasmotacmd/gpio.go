package tasmotacmd

import (
	"embed"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
)

// GPIOTable is the content of gpio-<version>.json: the GPIO function names of a Tasmota version and
// where a template slot sits on the chip. Generated from the Tasmota source by
// cmd/tasmofleet-catalogue (extract.ExtractGPIO); the package stays pure.
//
// Only the newest pinned version is kept. The function enum is append-only ("Only add at the end",
// tasmota_template.h:24), so a number read from an older firmware names the same function here,
// apart from functions that are newer than that firmware.
type GPIOTable struct {
	Tasmota      string `json:"tasmota"`
	SourceSHA256 string `json:"source_sha256"`
	// Functions lists the functions by number (the template value >> 5), per build family. The
	// enum has ESP32-only members in the middle, so the numbers of the two lists differ.
	Functions map[string][]GPIOFunction `json:"functions"`
	Families  []GPIOFamily              `json:"families"`
}

// Build families of GPIOTable.Functions.
const (
	GPIOBuild8266 = "esp8266"
	GPIOBuild32   = "esp32"
)

// GPIOFunction is one member of the enum UserSelectablePins.
type GPIOFunction struct {
	ID   string `json:"id"`   // GPIO_REL1
	Name string `json:"name"` // the text of kSensorNames, e.g. "Relay_i"
	// Multi: the function has several instances (Relay 1..8); the web page and the Gpios
	// command number them from 1, so the template value's low 5 bits are the instance - 1.
	Multi bool `json:"multi,omitempty"`
}

// GPIOFamily is the template layout of one chip family.
type GPIOFamily struct {
	Family string `json:"family"` // esp8266, esp32, esp32s2, ...
	Pins   int    `json:"pins"`   // MAX_USER_PINS: the length of the template's GPIO array
	// Phys[i] is the physical GPIO number of template slot i.
	Phys []int `json:"phys"`
}

//go:embed gpio-16.0.0.json
var gpioFile embed.FS

var (
	gpioOnce sync.Once
	gpioTab  *GPIOTable
	gpioErr  error
)

// GPIO returns the embedded table. It panics on a damaged embedded file, which a test catches.
func GPIO() *GPIOTable {
	gpioOnce.Do(func() {
		b, err := gpioFile.ReadFile("gpio-16.0.0.json")
		if err == nil {
			gpioTab = new(GPIOTable)
			err = json.Unmarshal(b, gpioTab)
		}
		gpioErr = err
	})
	if gpioErr != nil {
		panic("tasmotacmd: embedded gpio table: " + gpioErr.Error())
	}
	return gpioTab
}

func (t *GPIOTable) family(family string) (GPIOFamily, bool) {
	for _, f := range t.Families {
		if f.Family == family {
			return f, true
		}
	}
	return GPIOFamily{}, false
}

// build maps a chip family to the function list it uses; ok is false for an unknown family.
func (t *GPIOTable) build(family string) (string, bool) {
	if _, ok := t.family(family); !ok {
		return "", false
	}
	if family == GPIOBuild8266 {
		return GPIOBuild8266, true
	}
	return GPIOBuild32, true
}

// TemplatePins is the length of a template's GPIO array on the family (MAX_USER_PINS).
func (t *GPIOTable) TemplatePins(family string) (int, bool) {
	f, ok := t.family(family)
	return f.Pins, ok
}

// PhysicalPin is the physical GPIO number of template slot idx.
func (t *GPIOTable) PhysicalPin(family string, idx int) (int, bool) {
	f, ok := t.family(family)
	if !ok || idx < 0 || idx >= len(f.Phys) {
		return 0, false
	}
	return f.Phys[idx], true
}

// userValue is how a template JSON spells "User" (AGPIO(GPIO_NONE) + 1, JsonTemplate/TemplateJson).
const userValue = 1

func (t *GPIOTable) function(family string, value int) (GPIOFunction, bool) {
	b, ok := t.build(family)
	if !ok || value < 0 {
		return GPIOFunction{}, false
	}
	fns := t.Functions[b]
	if i := value >> 5; i < len(fns) {
		return fns[i], true
	}
	return GPIOFunction{}, false
}

// FunctionName is the text for a template value ("Relay1", "Led_i2", "None", "User"); an
// unknown value or family gives "#<value>", never a guess.
func (t *GPIOTable) FunctionName(family string, value int) string {
	if value == userValue {
		return "User"
	}
	fn, ok := t.function(family, value)
	if !ok {
		return "#" + strconv.Itoa(value)
	}
	if inst := value & 31; fn.Multi || inst > 0 {
		return fn.Name + strconv.Itoa(inst+1)
	}
	return fn.Name
}

// IsEthernet reports whether the value is one of the ETH_* pins of the Ethernet PHY.
func (t *GPIOTable) IsEthernet(family string, value int) bool {
	if value == userValue {
		return false
	}
	fn, ok := t.function(family, value)
	return ok && strings.HasPrefix(fn.ID, "GPIO_ETH_")
}
