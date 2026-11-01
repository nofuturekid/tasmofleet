package extract

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/tasmotacmd"
)

// ExtractGPIO reads the GPIO function names and the template layout of every chip family from a
// Tasmota `tasmota/` directory. Like the command extraction it is a static reader of C text, but
// for this table a wrong guess is dangerous (a diff would name the wrong pin), so every assumption
// about the text is checked and a miss is an error, never a fallback.
//
// Sources, all in the tasmota/ directory:
//   - include/tasmota_template.h: enum UserSelectablePins (the function numbers), kSensorNames
//     (their texts, same order), kGpioNiceList (AGMAX marks functions with several instances),
//     MAX_USER_PINS per chip and ESP32_TEMPLATE_TO_PHY;
//   - language/en_GB.h: the D_SENSOR_* texts;
//   - tasmota_support/support.ino: TemplateGpios, the slot -> pin rules per chip family.
func ExtractGPIO(src fs.FS, version, sha256 string) (*tasmotacmd.GPIOTable, error) {
	read := func(p string) (string, error) {
		b, err := fs.ReadFile(src, p)
		if err != nil {
			return "", fmt.Errorf("gpio: %w", err)
		}
		return stripComments(string(b)), nil
	}
	tmpl, err := read("include/tasmota_template.h")
	if err != nil {
		return nil, err
	}
	lang, err := read("language/en_GB.h")
	if err != nil {
		return nil, err
	}
	support, err := read("tasmota_support/support.ino")
	if err != nil {
		return nil, err
	}
	defs, err := nameDefines(lang)
	if err != nil {
		return nil, err
	}
	ids, err := enumIDs(tmpl)
	if err != nil {
		return nil, err
	}
	texts, err := functionTexts(tmpl, defs)
	if err != nil {
		return nil, err
	}
	multi := niceListMulti(tmpl)
	tab := &tasmotacmd.GPIOTable{Tasmota: version, SourceSHA256: sha256, Functions: map[string][]tasmotacmd.GPIOFunction{}}
	for _, build := range []struct {
		name  string
		esp32 bool
	}{{tasmotacmd.GPIOBuild8266, false}, {tasmotacmd.GPIOBuild32, true}} {
		if len(ids[build.esp32]) != len(texts[build.esp32]) {
			return nil, fmt.Errorf("gpio: %s build: %d enum members but %d names in kSensorNames", build.name, len(ids[build.esp32]), len(texts[build.esp32]))
		}
		for i, id := range ids[build.esp32] {
			tab.Functions[build.name] = append(tab.Functions[build.name], tasmotacmd.GPIOFunction{ID: id, Name: texts[build.esp32][i], Multi: multi[id]})
		}
	}
	tab.Families, err = families(tmpl, support)
	if err != nil {
		return nil, err
	}
	return tab, nil
}

var (
	reNameDefine = regexp.MustCompile(`(?m)^[ \t]*#[ \t]*define[ \t]+(D_[A-Z0-9_]+)[ \t]+(.+)$`)
	reToken      = regexp.MustCompile(`^\s*(?:"((?:[^"\\]|\\.)*)"|([A-Za-z_][A-Za-z0-9_]*))`)
	reMembers    = regexp.MustCompile(`^[\sA-Za-z0-9_,]*$`)
)

// concat evaluates a run of string literals and D_ macro names into one string.
func concat(s string, defs map[string]string) (string, error) {
	var out strings.Builder
	for strings.TrimSpace(s) != "" {
		m := reToken.FindStringSubmatchIndex(s)
		if m == nil {
			return "", fmt.Errorf("gpio: cannot read %q as strings and macros", strings.TrimSpace(s))
		}
		if m[2] >= 0 {
			lit := s[m[2]:m[3]]
			if strings.Contains(lit, `\`) {
				return "", fmt.Errorf("gpio: escape in string %q is not handled", lit)
			}
			out.WriteString(lit)
		} else {
			name := s[m[4]:m[5]]
			v, ok := defs[name]
			if !ok {
				return "", fmt.Errorf("gpio: %s is not defined in language/en_GB.h", name)
			}
			out.WriteString(v)
		}
		s = s[m[1]:]
	}
	return out.String(), nil
}

// nameDefines reads the D_* texts. A name defined twice with different texts is an error.
func nameDefines(lang string) (map[string]string, error) {
	raw := map[string]string{}
	for _, m := range reNameDefine.FindAllStringSubmatch(lang, -1) {
		if old, ok := raw[m[1]]; ok && old != m[2] {
			return nil, fmt.Errorf("gpio: %s is defined twice with different text", m[1])
		}
		raw[m[1]] = strings.TrimSpace(m[2])
	}
	out := map[string]string{}
	for k, v := range raw {
		// A define that is not plain string literals (a number, a function) is not a name text: skip it,
		// a name that needs it is reported when the list is read.
		if s, err := concat(v, nil); err == nil {
			out[k] = s
		}
	}
	return out, nil
}

// region returns the text after `start` up to `end` (exclusive).
func region(s, start, end string) (string, error) {
	i := strings.Index(s, start)
	if i < 0 {
		return "", fmt.Errorf("gpio: %q not found", start)
	}
	i += len(start)
	j := strings.Index(s[i:], end)
	if j < 0 {
		return "", fmt.Errorf("gpio: end %q of %q not found", end, start)
	}
	return s[i : i+j], nil
}

// byBuild feeds the lines of a region to f once per build (ESP8266, ESP32). The only conditional
// allowed inside is #ifdef ESP32 ... #endif: the lists are numbered by position, so any other
// condition would make the numbers depend on a build we do not model.
func byBuild(reg string, f func(esp32 bool, line string) error) error {
	for _, esp32 := range []bool{false, true} {
		inESP32 := false
		for _, line := range strings.Split(reg, "\n") {
			t := strings.TrimSpace(line)
			switch {
			case t == "#ifdef ESP32":
				if inESP32 {
					return errors.New("gpio: nested #ifdef ESP32")
				}
				inESP32 = true
			case t == "#endif":
				if !inESP32 {
					return errors.New("gpio: #endif without #ifdef ESP32")
				}
				inESP32 = false
			case strings.HasPrefix(t, "#"):
				return fmt.Errorf("gpio: unexpected %q in a numbered list", t)
			case inESP32 && !esp32:
			default:
				if err := f(esp32, line); err != nil {
					return err
				}
			}
		}
		if inESP32 {
			return errors.New("gpio: unterminated #ifdef ESP32")
		}
	}
	return nil
}

// enumIDs lists the members of enum UserSelectablePins in order, per build, without GPIO_SENSOR_END.
func enumIDs(tmpl string) (map[bool][]string, error) {
	reg, err := region(tmpl, "enum UserSelectablePins {", "GPIO_SENSOR_END")
	if err != nil {
		return nil, err
	}
	ids := map[bool][]string{}
	err = byBuild(reg, func(esp32 bool, line string) error {
		if !reMembers.MatchString(line) {
			return fmt.Errorf("gpio: cannot read enum line %q (an explicit value?)", strings.TrimSpace(line))
		}
		ids[esp32] = append(ids[esp32], strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\r' })...)
		return nil
	})
	return ids, err
}

// functionTexts lists the texts of kSensorNames, per build: strings and D_SENSOR_* macros joined and
// split at '|'.
func functionTexts(tmpl string, defs map[string]string) (map[bool][]string, error) {
	reg, err := region(tmpl, "const char kSensorNames[] PROGMEM =", ";")
	if err != nil {
		return nil, err
	}
	joined := map[bool]*strings.Builder{false: {}, true: {}}
	err = byBuild(reg, func(esp32 bool, line string) error {
		s, err := concat(line, defs)
		joined[esp32].WriteString(s)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := map[bool][]string{}
	for esp32, b := range joined {
		parts := strings.Split(b.String(), "|")
		if parts[len(parts)-1] != "" {
			return nil, errors.New("gpio: kSensorNames does not end with '|'")
		}
		out[esp32] = parts[:len(parts)-1]
	}
	return out, nil
}

var reNice = regexp.MustCompile(`AGPIO\((GPIO_[A-Z0-9_]+)\)(\s*\+\s*AGMAX\()?`)

// niceListMulti marks the functions the nice list gives AGMAX (several instances). The list is
// conditional on many USE_ defines; whether a function has instances does not depend on them, so
// the conditions are ignored here.
func niceListMulti(tmpl string) map[string]bool {
	reg, err := region(tmpl, "const uint16_t kGpioNiceList[] PROGMEM = {", "};")
	if err != nil {
		return nil
	}
	multi := map[string]bool{}
	for _, m := range reNice.FindAllStringSubmatch(reg, -1) {
		multi[m[1]] = multi[m[1]] || m[2] != ""
	}
	return multi
}

var (
	reTarget    = regexp.MustCompile(`^#\s*(?:if|elif)\s+CONFIG_IDF_TARGET_(ESP32[A-Z0-9]*)\s*$`)
	rePins      = regexp.MustCompile(`^#\s*define\s+MAX_USER_PINS\s+(\d+)\s*$`)
	reTable     = regexp.MustCompile(`(?s)#\s*define\s+ESP32_TEMPLATE_TO_PHY((?:[^\n]*\\\n)*[^\n]*)`)
	reSpace     = regexp.MustCompile(`\s+`)
	reNum       = regexp.MustCompile(`\d+`)
	reTargetSet = `((?:CONFIG_IDF_TARGET_ESP32[A-Z0-9]*(?: \|\| )?)+)`
	reRule8266  = regexp.MustCompile(`#ifdef ESP8266 if \(6 == i\) \{ j = 9; \} if \(8 == i\) \{ j = 12; \} dest\[j\] = src\[i\]; j\+\+; #endif`)
	reRuleOne   = regexp.MustCompile(`#if ` + reTargetSet + ` dest\[i\] = src\[i\]; #elif`)
	reRuleSkip  = regexp.MustCompile(`#elif ` + reTargetSet + ` if \(22 == i\) \{ j = 33; \} dest\[j\] = src\[i\]; j\+\+; #else dest\[Esp32TemplateToPhy\[i\]\] = src\[i\]; #endif`)
)

func targets(set string) []string {
	var out []string
	for _, t := range strings.Split(set, "||") {
		out = append(out, strings.ToLower(strings.TrimPrefix(strings.TrimSpace(t), "CONFIG_IDF_TARGET_")))
	}
	return out
}

// families reads MAX_USER_PINS per chip and builds the slot -> pin tables. The rules of
// TemplateGpios are checked in the text; a Tasmota that changes them stops the extraction.
func families(tmpl, support string) ([]tasmotacmd.GPIOFamily, error) {
	pins := map[string]int{}
	section := ""
	for _, line := range strings.Split(tmpl, "\n") {
		t := strings.TrimSpace(line)
		switch m := reTarget.FindStringSubmatch(t); {
		case t == "#ifdef ESP8266":
			section = tasmotacmd.GPIOBuild8266
		case m != nil:
			section = strings.ToLower(m[1])
		case t == "#else" && strings.HasPrefix(section, "esp32"):
			section = "esp32" // the plain ESP32 is the #else of the CONFIG_IDF_TARGET chain
		}
		if m := rePins.FindStringSubmatch(t); m != nil {
			if section == "" {
				return nil, errors.New("gpio: MAX_USER_PINS outside a chip section")
			}
			if _, dup := pins[section]; dup {
				return nil, fmt.Errorf("gpio: MAX_USER_PINS of %s defined twice", section)
			}
			pins[section], _ = strconv.Atoi(m[1])
		}
	}
	if pins[tasmotacmd.GPIOBuild8266] == 0 || pins["esp32"] == 0 {
		return nil, errors.New("gpio: no MAX_USER_PINS for esp8266 or esp32")
	}

	phy32, err := conversionTable(tmpl)
	if err != nil {
		return nil, err
	}
	if len(phy32) != pins["esp32"] {
		return nil, fmt.Errorf("gpio: ESP32_TEMPLATE_TO_PHY has %d entries, MAX_USER_PINS is %d", len(phy32), pins["esp32"])
	}

	// TemplateGpios, as one line of tokens, directives kept.
	body, err := region(support, "void TemplateGpios(myio *gp)", "\ngpio_flag ModuleFlag(void)")
	if err != nil {
		return nil, err
	}
	flat := reSpace.ReplaceAllString(strings.ReplaceAll(body, "\n", " "), " ")
	flat = strings.ReplaceAll(flat, "# ", "#")
	if !reRule8266.MatchString(flat) {
		return nil, errors.New("gpio: TemplateGpios does not skip ESP8266 slots 6-8 and 11 the way the extractor expects")
	}
	rule := map[string]string{}
	m := reRuleOne.FindStringSubmatch(flat)
	if m == nil {
		return nil, errors.New("gpio: TemplateGpios no longer maps the C series 1:1")
	}
	for _, f := range targets(m[1]) {
		rule[f] = "one"
	}
	if m = reRuleSkip.FindStringSubmatch(flat); m == nil {
		return nil, errors.New("gpio: TemplateGpios no longer skips slots 22-32 on S2/S3 and uses the conversion table on ESP32")
	}
	for _, f := range targets(m[1]) {
		if rule[f] != "" {
			return nil, fmt.Errorf("gpio: %s has two slot rules", f)
		}
		rule[f] = "skip"
	}
	rule[tasmotacmd.GPIOBuild8266], rule["esp32"] = "8266", "table"

	var out []tasmotacmd.GPIOFamily
	for fam, n := range pins {
		phys := make([]int, n)
		for i := range phys {
			switch rule[fam] {
			case "8266":
				phys[i] = i
				if i >= 6 { // j = 9 at slot 6, j = 12 at slot 8
					phys[i] = i + 3
				}
				if i >= 8 {
					phys[i] = i + 4
				}
			case "table":
				phys[i] = phy32[i]
			case "skip":
				phys[i] = i
				if i >= 22 {
					phys[i] = i + 11
				}
			case "one":
				phys[i] = i
			default:
				return nil, fmt.Errorf("gpio: no slot rule for %s", fam)
			}
		}
		out = append(out, tasmotacmd.GPIOFamily{Family: fam, Pins: n, Phys: phys})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Family < out[j].Family })
	return out, nil
}

func conversionTable(tmpl string) ([]int, error) {
	m := reTable.FindStringSubmatch(tmpl)
	if m == nil {
		return nil, errors.New("gpio: ESP32_TEMPLATE_TO_PHY not found")
	}
	var out []int
	for _, n := range reNum.FindAllString(m[1], -1) {
		v, _ := strconv.Atoi(n)
		out = append(out, v)
	}
	return out, nil
}
