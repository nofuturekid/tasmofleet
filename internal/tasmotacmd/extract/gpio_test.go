package extract

import (
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/nofuturekid/tasmofleet/internal/tasmotacmd"
)

// miniTemplate is cut down from tasmota_template.h: the enum with an ESP32-only member in the
// middle, the name list with the same #ifdef, the nice list with AGMAX, and the per-chip pin counts.
const miniTemplate = `
// ATTENTION: Only add at the end of this list just before GPIO_SENSOR_END
enum UserSelectablePins {
  GPIO_NONE,                           // Not used
  GPIO_KEY1, GPIO_KEY1_NP, /* two */
#ifdef ESP32
  GPIO_ETH_PHY_POWER,                  // Ethernet
#endif
  GPIO_I2C_SCL,
  GPIO_REL1, GPIO_REL1_INV,
  GPIO_SENSOR_END };

const char kSensorNames[] PROGMEM =
  D_SENSOR_NONE "|"
  D_SENSOR_BUTTON "|" D_SENSOR_BUTTON "_n|"
#ifdef ESP32
  D_SENSOR_ETH_PHY_POWER "|"
#endif
  D_SENSOR_I2C_SCL "|"
  D_SENSOR_RELAY "|" D_SENSOR_RELAY "_i|"
;

const char kSensorNamesFixed[] PROGMEM =
  D_SENSOR_USER;

const uint16_t kGpioNiceList[] PROGMEM = {
  GPIO_NONE,
  AGPIO(GPIO_KEY1) + AGMAX(MAX_KEYS),            // Buttons
  AGPIO(GPIO_KEY1_NP) + AGMAX(MAX_KEYS),
#ifdef ESP32
  AGPIO(GPIO_ETH_PHY_POWER),
#endif
  AGPIO(GPIO_I2C_SCL),
  AGPIO(GPIO_REL1) + AGMAX(MAX_RELAYS),
  AGPIO(GPIO_REL1_INV) + AGMAX(MAX_RELAYS),
};

#ifdef ESP8266
#define MAX_USER_PINS      14   // MAX_GPIO_PIN - MIN_FLASH_PINS
#endif  // ESP8266

#ifdef ESP32
#if CONFIG_IDF_TARGET_ESP32C3
#define MAX_USER_PINS      22   // MAX_GPIO_PIN - MIN_FLASH_PINS
#elif CONFIG_IDF_TARGET_ESP32S2
#define MAX_USER_PINS      36   // MAX_GPIO_PIN - MIN_FLASH_PINS
#elif CONFIG_IDF_TARGET_ESP32S3
#define MAX_USER_PINS      38   // MAX_GPIO_PIN - MIN_FLASH_PINS
#else  // not CONFIG_IDF_TARGET_ESP32C2/C3/C6 nor CONFIG_IDF_TARGET_ESP32S2 - ESP32
#define ESP32_TEMPLATE_TO_PHY         \
   0,  1,  2,  3,  4,  5,             \
   9, 10,                             \
  12, 13, 14, 15, 16, 17, 18, 19,     \
  20, 21, 22, 23, 24, 25, 26, 27,     \
   6,  7,  8, 11,      /* 28-31 */    \
  32, 33, 34, 35, 36, 37, 38, 39
#define MAX_USER_PINS      36   // MAX_GPIO_PIN - MIN_FLASH_PINS
#endif  // ESP32
#endif
`

const miniNames = `
#define D_SENSOR_NONE          "None"
#define D_SENSOR_USER          "User"
#define D_SENSOR_BUTTON        "Button"     // Suffix "1"
#define D_SENSOR_I2C_SCL       "I2C SCL"
#define D_SENSOR_ETH_PHY_POWER "ETH POWER"
#define D_SENSOR_RELAY         "Relay"
`

// miniSupport is TemplateGpios of support.ino with the lines that fix the slot-to-pin mapping.
const miniSupport = `
void TemplateGpios(myio *gp)
{
#ifdef ESP8266
    if (6 == i) { j = 9; }
    if (8 == i) { j = 12; }
    dest[j] = src[i];
    j++;
#endif  // ESP8266
#ifdef ESP32
#if CONFIG_IDF_TARGET_ESP32C2 || CONFIG_IDF_TARGET_ESP32C3 || CONFIG_IDF_TARGET_ESP32C5 || CONFIG_IDF_TARGET_ESP32C6 || CONFIG_IDF_TARGET_ESP32C61 || CONFIG_IDF_TARGET_ESP32P4 || CONFIG_IDF_TARGET_ESP32S31
    dest[i] = src[i];
#elif CONFIG_IDF_TARGET_ESP32S2 || CONFIG_IDF_TARGET_ESP32S3
    if (22 == i) { j = 33; }    // skip 22-32
    dest[j] = src[i];
    j++;
#else  // ESP32
    dest[Esp32TemplateToPhy[i]] = src[i];
#endif  // ESP32C2/C3/C6 and S2/S3
#endif  // ESP32
}

gpio_flag ModuleFlag(void)
`

func miniFS(mut func(map[string]string)) fstest.MapFS {
	files := map[string]string{
		"include/tasmota_template.h":  miniTemplate,
		"language/en_GB.h":            miniNames,
		"tasmota_support/support.ino": miniSupport,
	}
	if mut != nil {
		mut(files)
	}
	fsys := fstest.MapFS{}
	for k, v := range files {
		fsys[k] = &fstest.MapFile{Data: []byte(v)}
	}
	return fsys
}

func names(fns []tasmotacmd.GPIOFunction) []string {
	var out []string
	for _, f := range fns {
		out = append(out, f.Name)
	}
	return out
}

func TestGPIOFunctionsDifferBetweenBuilds(t *testing.T) {
	// The ESP32-only member sits in the middle of the enum, so every number behind it differs between the
	// ESP8266 and the ESP32 build; one list for both would name the wrong function on one of them.
	tab, err := ExtractGPIO(miniFS(nil), "16.0.0", "ab")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(tab.Functions["esp8266"]), []string{"None", "Button", "Button_n", "I2C SCL", "Relay", "Relay_i"}; !reflect.DeepEqual(got, want) {
		t.Errorf("esp8266: %v", got)
	}
	if got, want := names(tab.Functions["esp32"]), []string{"None", "Button", "Button_n", "ETH POWER", "I2C SCL", "Relay", "Relay_i"}; !reflect.DeepEqual(got, want) {
		t.Errorf("esp32: %v", got)
	}
	if tab.Functions["esp32"][3].ID != "GPIO_ETH_PHY_POWER" {
		t.Errorf("id: %+v", tab.Functions["esp32"][3])
	}
	if tab.Tasmota != "16.0.0" || tab.SourceSHA256 != "ab" {
		t.Errorf("provenance: %+v", tab)
	}
}

func TestGPIOMultiFollowsAGMAX(t *testing.T) {
	// Only functions with several instances are shown with a number (Relay1); a single one (I2C SCL) must not get one.
	tab, err := ExtractGPIO(miniFS(nil), "16.0.0", "ab")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range tab.Functions["esp8266"] {
		got[f.ID] = f.Multi
	}
	want := map[string]bool{"GPIO_NONE": false, "GPIO_KEY1": true, "GPIO_KEY1_NP": true, "GPIO_I2C_SCL": false, "GPIO_REL1": true, "GPIO_REL1_INV": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("multi: %v", got)
	}
}

func TestGPIOFamiliesAndSlots(t *testing.T) {
	// The array length and the slot -> pin table per chip, read from the source: ESP8266 skips 6-8 and 11 (two jumps in
	// TemplateGpios), ESP32 uses the conversion table, S2/S3 skip 22-32, the C series is 1:1.
	tab, err := ExtractGPIO(miniFS(nil), "16.0.0", "ab")
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]tasmotacmd.GPIOFamily{}
	for _, f := range tab.Families {
		by[f.Family] = f
	}
	for fam, n := range map[string]int{"esp8266": 14, "esp32": 36, "esp32c3": 22, "esp32s2": 36, "esp32s3": 38} {
		if by[fam].Pins != n || len(by[fam].Phys) != n {
			t.Errorf("%s: pins %d, phys %d, want %d", fam, by[fam].Pins, len(by[fam].Phys), n)
		}
	}
	check := func(fam string, slot, pin int) {
		t.Helper()
		if got := by[fam].Phys[slot]; got != pin {
			t.Errorf("%s slot %d: pin %d, want %d", fam, slot, got, pin)
		}
	}
	check("esp8266", 5, 5)
	check("esp8266", 6, 9)
	check("esp8266", 8, 12)
	check("esp8266", 13, 17)
	check("esp32", 6, 9)
	check("esp32", 24, 6)
	check("esp32", 27, 11)
	check("esp32", 35, 39)
	check("esp32c3", 21, 21)
	check("esp32s2", 21, 21)
	check("esp32s2", 22, 33)
	check("esp32s3", 37, 48)
}

func TestGPIOFailsLoudWhenTheSourceChanges(t *testing.T) {
	// Reading C text is a bet on its shape. When a new Tasmota moves a pin skip, a wrong table would send
	// a user to the wrong pin without any error, so each assumption is checked and a miss stops the build of the table.
	cases := map[string]func(map[string]string){
		"enum member count differs from the names": func(f map[string]string) {
			f["include/tasmota_template.h"] = strings.Replace(f["include/tasmota_template.h"], `D_SENSOR_RELAY "|" D_SENSOR_RELAY "_i|"`, `D_SENSOR_RELAY "|"`, 1)
		},
		"unknown name macro": func(f map[string]string) {
			f["language/en_GB.h"] = strings.Replace(f["language/en_GB.h"], "#define D_SENSOR_RELAY", "#define D_SENSOR_OTHER", 1)
		},
		"unexpected conditional in the enum": func(f map[string]string) {
			f["include/tasmota_template.h"] = strings.Replace(f["include/tasmota_template.h"], "#ifdef ESP32\n  GPIO_ETH_PHY_POWER", "#ifdef USE_FOO\n  GPIO_ETH_PHY_POWER", 1)
		},
		"ESP8266 skip rule moved": func(f map[string]string) {
			f["tasmota_support/support.ino"] = strings.Replace(f["tasmota_support/support.ino"], "if (6 == i) { j = 9; }", "if (7 == i) { j = 9; }", 1)
		},
		"S2 skip rule moved": func(f map[string]string) {
			f["tasmota_support/support.ino"] = strings.Replace(f["tasmota_support/support.ino"], "if (22 == i) { j = 33; }", "if (23 == i) { j = 33; }", 1)
		},
		"conversion table of the wrong length": func(f map[string]string) {
			f["include/tasmota_template.h"] = strings.Replace(f["include/tasmota_template.h"], "32, 33, 34, 35, 36, 37, 38, 39", "32, 33, 34, 35, 36, 37, 38", 1)
		},
		"no pin count": func(f map[string]string) {
			f["include/tasmota_template.h"] = strings.ReplaceAll(f["include/tasmota_template.h"], "#define MAX_USER_PINS      14", "#define MAX_USER_PIN       14")
		},
		"C series no longer 1:1": func(f map[string]string) {
			f["tasmota_support/support.ino"] = strings.Replace(f["tasmota_support/support.ino"], "dest[i] = src[i];", "dest[i+1] = src[i];", 1)
		},
	}
	wants := map[string]string{
		"enum member count differs from the names": "enum members but",
		"unknown name macro":                       "D_SENSOR_RELAY is not defined",
		"unexpected conditional in the enum":       "unexpected \"#ifdef USE_FOO\"",
		"ESP8266 skip rule moved":                  "slots 6-8 and 11",
		"S2 skip rule moved":                       "22-32",
		"conversion table of the wrong length":     "ESP32_TEMPLATE_TO_PHY has 35",
		"no pin count":                             "no MAX_USER_PINS for esp8266",
		"C series no longer 1:1":                   "1:1",
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ExtractGPIO(miniFS(mut), "16.0.0", "ab")
			if err == nil || !strings.Contains(err.Error(), wants[name]) {
				t.Fatalf("error %v, want one containing %q", err, wants[name])
			}
		})
	}
}
