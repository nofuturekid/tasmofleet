package tasmotacmd

import "testing"

// The names and slots decide which pin a diff line points at and whether a change may touch the
// Ethernet PHY, so each case is a value read from a real device or a line of the pinned source.

func TestFunctionNamesOfTheNOUSTemplate(t *testing.T) {
	// The NOUS A1T capture (testdata/fixtures/devices/mr-bambu-plug/template.json) on an ESP8266:
	// the reference for what the user sees in a diff.
	cases := map[int]string{
		0: "None", 1: "User", 32: "Button1", 224: "Relay1", 320: "Led_i1",
		2720: "BL0937 CF", 2656: "HLWBL CF1", 2624: "HLWBL SEL_i",
	}
	for v, want := range cases {
		if got := GPIO().FunctionName("esp8266", v); got != want {
			t.Errorf("value %d: %q, want %q", v, got, want)
		}
	}
}

func TestSecondInstanceIsNumbered(t *testing.T) {
	// Relay 2 is value 224+1; showing it as "Relay1" would send the user to the wrong pin.
	if got := GPIO().FunctionName("esp8266", 225); got != "Relay2" {
		t.Errorf("got %q", got)
	}
}

func TestUnknownValueIsHashCode(t *testing.T) {
	// A firmware newer than the pinned source may send a number we do not know: show it, do not guess.
	for _, fam := range []string{"esp8266", "esp32", "esp32s3"} {
		if got := GPIO().FunctionName(fam, 2000<<5); got != "#64000" {
			t.Errorf("%s: %q", fam, got)
		}
	}
	if got := GPIO().FunctionName("", 224); got != "#224" {
		t.Errorf("unknown family: %q", got)
	}
}

func TestFunctionNumbersDifferBetweenEsp8266AndEsp32(t *testing.T) {
	// The enum has ESP32-only members (webcam, Ethernet) in the middle: one number can name two
	// functions, so the name must be looked up in the list of the device's own family.
	a, b := GPIO().FunctionName("esp8266", 163<<5), GPIO().FunctionName("esp32", 163<<5)
	if a == b || a == "#5216" || b == "#5216" {
		t.Errorf("same name %q/%q for both builds, or an unknown", a, b)
	}
}

func TestEthernetFunctionsExistOnlyOnEsp32(t *testing.T) {
	// Changing an ETH_* slot on a wired board cuts it off; the check needs the ETH members.
	var eth []int
	for i, f := range GPIO().Functions["esp32"] {
		if len(f.ID) > 9 && f.ID[:9] == "GPIO_ETH_" {
			eth = append(eth, i<<5)
		}
	}
	if len(eth) != 3 {
		t.Fatalf("ETH functions: %v", eth)
	}
	for _, v := range eth {
		if !GPIO().IsEthernet("esp32", v) {
			t.Errorf("%d not Ethernet", v)
		}
		if GPIO().IsEthernet("esp8266", v) {
			t.Errorf("%d is Ethernet on an ESP8266", v)
		}
	}
	if GPIO().IsEthernet("esp32", 224) || GPIO().IsEthernet("esp32", 1) {
		t.Error("Relay or User counted as Ethernet")
	}
}

func TestTemplateLengthPerFamily(t *testing.T) {
	// MAX_USER_PINS, tasmota_template.h:1416-1598. ESP32 and S2 have the same length with different
	// pins, so the length alone is no family check (the planner compares families, too).
	want := map[string]int{"esp8266": 14, "esp32": 36, "esp32s2": 36, "esp32s3": 38, "esp32c2": 21,
		"esp32c3": 22, "esp32c5": 29, "esp32c6": 31, "esp32c61": 30, "esp32p4": 55}
	for fam, n := range want {
		if got, ok := GPIO().TemplatePins(fam); !ok || got != n {
			t.Errorf("%s: %d %v, want %d", fam, got, ok, n)
		}
		f, _ := GPIO().family(fam)
		if len(f.Phys) != n {
			t.Errorf("%s: %d physical pins for %d slots", fam, len(f.Phys), n)
		}
	}
	if _, ok := GPIO().TemplatePins("esp9999"); ok {
		t.Error("unknown family has a length")
	}
}

func TestPhysicalPinMapping(t *testing.T) {
	// TemplateGpios (support.ino:1704-1750) and ESP32_TEMPLATE_TO_PHY (tasmota_template.h:1588):
	// slot -> pin. A wrong row makes the diff name the wrong pin on a board the user may wire by it.
	type c struct {
		fam       string
		slot, pin int
	}
	for _, tc := range []c{
		{"esp8266", 5, 5}, {"esp8266", 6, 9}, {"esp8266", 7, 10}, {"esp8266", 8, 12}, {"esp8266", 13, 17}, // skips 6-8 and 11
		{"esp32", 5, 5}, {"esp32", 6, 9}, {"esp32", 7, 10}, {"esp32", 8, 12}, {"esp32", 23, 27},
		{"esp32", 24, 6}, {"esp32", 27, 11}, {"esp32", 28, 32}, {"esp32", 35, 39},
		{"esp32s2", 21, 21}, {"esp32s2", 22, 33}, {"esp32s2", 35, 46},
		{"esp32s3", 22, 33}, {"esp32s3", 37, 48},
		{"esp32c2", 20, 20}, {"esp32c3", 21, 21}, {"esp32c5", 28, 28}, {"esp32c6", 30, 30},
		{"esp32c61", 29, 29}, {"esp32p4", 54, 54},
	} {
		if got, ok := GPIO().PhysicalPin(tc.fam, tc.slot); !ok || got != tc.pin {
			t.Errorf("%s slot %d: pin %d %v, want %d", tc.fam, tc.slot, got, ok, tc.pin)
		}
	}
	if _, ok := GPIO().PhysicalPin("esp8266", 14); ok {
		t.Error("slot beyond the array has a pin")
	}
	if _, ok := GPIO().PhysicalPin("esp8266", -1); ok {
		t.Error("negative slot has a pin")
	}
}

func TestEthernetSPIBusFunctions(t *testing.T) {
	// W5500/DM9051/KSZ8851 boards use the hardware SPI bus for their Ethernet chip (xdrv_82_esp32_ethernet.ino:247-300),
	// so its clock, MOSI and MISO belong to the network just like the ETH_* pins; the display and sensor SPI
	// functions (chip select, data/command) do not.
	ids := map[string]bool{}
	for i, f := range GPIO().Functions["esp32"] {
		got := GPIO().IsEthernetSPI("esp32", i<<5)
		want := f.ID == "GPIO_SPI_CLK" || f.ID == "GPIO_SPI_MOSI" || f.ID == "GPIO_SPI_MISO"
		if got != want {
			t.Errorf("%s: %v", f.ID, got)
		}
		if got {
			ids[f.ID] = true
		}
	}
	if len(ids) != 3 {
		t.Errorf("found %v", ids)
	}
	if GPIO().IsEthernetSPI("esp32", 1) || GPIO().IsEthernetSPI("", 7<<5) {
		t.Error("User or unknown family counted")
	}
}

func TestRedPins(t *testing.T) {
	// support.ino RedPin (1821): the pins Tasmota itself marks as dangerous to change. ESP8266: 9 and 10 (flash on ESP8285 parts);
	// classic ESP32: 6-11 (flash) and 16/17 (PSRAM on some modules).
	red := map[string][]int{"esp8266": {9, 10}, "esp32": {6, 7, 8, 9, 10, 11, 16, 17}}
	for fam, pins := range red {
		for p := 0; p < 40; p++ {
			want := false
			for _, r := range pins {
				want = want || r == p
			}
			if got := GPIO().RedPin(fam, p); got != want {
				t.Errorf("%s pin %d: %v", fam, p, got)
			}
		}
	}
	if GPIO().RedPin("esp32s3", 9) || GPIO().RedPin("", 9) {
		t.Error("only esp8266 and classic esp32 are covered")
	}
}
