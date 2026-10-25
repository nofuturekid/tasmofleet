package tasmota

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixturesDir = "../../testdata/fixtures/devices"

// fixtureDevices returns every captured device directory, so a new capture is
// covered by the parser tests without touching them.
func fixtureDevices(t *testing.T) []string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(fixturesDir, "*"))
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no fixture devices found: %v", err)
	}
	return dirs
}

func readFixture(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Identity detection (fwselect) depends on these exact fields; if a parser
// silently dropped one, the safety guard would see an empty hardware string.
func TestParseStatus2_AllFixtures(t *testing.T) {
	want := map[string]struct{ version, hw string }{
		"bedroom-cabinet":        {"16.0.0(release-tasmota)", "ESP8285N08"},
		"kitchen-spotlight":      {"16.0.0(release-tasmota)", "ESP8266EX"},
		"livingroom-readinglamp": {"16.0.0(release-tasmota)", "ESP8285N08"},
		"mr-bambu-plug":          {"16.0.0(release-tasmota)", "ESP8266EX"},
		"office-monitor-guest":   {"16.0.0(release-tasmota32)", "ESP32-D0WDQ6 v1.0"},
		"prusa-plug":             {"16.0.0(release-tasmota)", "ESP8266EX"},
	}
	for _, dir := range fixtureDevices(t) {
		name := filepath.Base(dir)
		t.Run(name, func(t *testing.T) {
			got, err := ParseStatus2(readFixture(t, dir, "status-2.json"))
			if err != nil {
				t.Fatal(err)
			}
			if w, ok := want[name]; ok && (got.Version != w.version || got.Hardware != w.hw) {
				t.Errorf("got %q / %q, want %q / %q", got.Version, got.Hardware, w.version, w.hw)
			}
			if got.CPUFrequency == 0 || got.Core == "" || got.BuildDateTime == "" {
				t.Errorf("incomplete StatusFWR: %+v", got)
			}
		})
	}
}

func TestParseStatus4_AllFixtures(t *testing.T) {
	for _, dir := range fixtureDevices(t) {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			got, err := ParseStatus4(readFixture(t, dir, "status-4.json"))
			if err != nil {
				t.Fatal(err)
			}
			// Flash size picks the 1 MB (minimal-first) versus 4 MB flow.
			if got.FlashSize == 0 || got.ProgramFlashSize == 0 || got.FlashChipID == "" {
				t.Errorf("incomplete StatusMEM: %+v", got)
			}
		})
	}
}

func TestParseStatus11_AllFixtures(t *testing.T) {
	for _, dir := range fixtureDevices(t) {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			got, err := ParseStatus11(readFixture(t, dir, "status-11.json"))
			if err != nil {
				t.Fatal(err)
			}
			if got.UptimeSec == 0 || got.Uptime == "" || got.Wifi == nil {
				t.Errorf("incomplete StatusSTS: %+v", got)
			}
			if _, ok := got.Power["POWER"]; !ok && len(got.Power) == 0 {
				t.Errorf("no power field collected: %+v", got.Power)
			}
		})
	}
}

// Multi-relay devices report POWER1..n and no bare POWER; the health model
// needs all of them, and non-power keys must not leak into the map.
func TestParseStatus11_CollectsNumberedPower(t *testing.T) {
	raw := `{"StatusSTS":{"Time":"t","UptimeSec":5,"POWER1":"ON","POWER2":"OFF","POWERED":"x","Heap":9}}`
	got, err := ParseStatus11(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Power) != 2 || got.Power["POWER1"] != "ON" || got.Power["POWER2"] != "OFF" {
		t.Errorf("Power = %v", got.Power)
	}
}

// Minimal firmware answers every non-minimal command with Unknown. The
// parsers must report that as ErrUnknownCommand: it is the signal that a
// device sits in minimal firmware, not a malformed reply.
func TestParsers_UnknownMeansErrUnknownCommand(t *testing.T) {
	for _, dir := range fixtureDevices(t) {
		raw := readFixture(t, dir, "unknown-foobar.json")
		for name, fn := range map[string]func(json.RawMessage) error{
			"Status2":      func(r json.RawMessage) error { _, e := ParseStatus2(r); return e },
			"Status4":      func(r json.RawMessage) error { _, e := ParseStatus4(r); return e },
			"Status11":     func(r json.RawMessage) error { _, e := ParseStatus11(r); return e },
			"UpgradeProbe": func(r json.RawMessage) error { _, e := ParseUpgradeProbe(r); return e },
			"OtaURL":       func(r json.RawMessage) error { _, e := ParseOtaURL(r); return e },
		} {
			if err := fn(raw); !errors.Is(err, ErrUnknownCommand) {
				t.Errorf("%s/%s: err = %v, want ErrUnknownCommand", filepath.Base(dir), name, err)
			}
		}
	}
}

// A StatusN query that returns another shape (Error, WARNING, empty, wrong
// section) must never be mistaken for data with zero values.
func TestParsers_RejectWrongShapes(t *testing.T) {
	bad := []string{
		``, `null`, `[]`, `{}`, `not json`,
		`{"Command":"Error","Input":"STATUS 9"}`,
		`{"WARNING":"Enter command cmnd="}`,
		`{"StatusMEM":{"Heap":1}}`, // wrong section for Status 2
		`{"StatusFWR":"x"}`,
	}
	for _, b := range bad {
		if _, err := ParseStatus2(json.RawMessage(b)); !errors.Is(err, ErrBadResponse) {
			t.Errorf("ParseStatus2(%q) err = %v, want ErrBadResponse", b, err)
		}
	}
}

// A spliced Status 0 reply (heap bursts on ESP8266) must be rejected, not
// partially parsed.
func TestParseStatus2_SplicedPayloadRejected(t *testing.T) {
	raw := `{"StatusFWR":{"Version":"16.0.0(release-tasmota)","Hardw{"StatusMEM":`
	if _, err := ParseStatus2(json.RawMessage(raw)); !errors.Is(err, ErrBadResponse) {
		t.Errorf("err = %v, want ErrBadResponse", err)
	}
}

// Device output is untrusted and sometimes contains raw control characters
// inside strings (e.g. a device name); strict encoding/json rejects those.
func TestParseStatus2_ToleratesControlCharacters(t *testing.T) {
	raw := "{\"StatusFWR\":{\"Version\":\"16.0.0(release-tasmota)\",\"Hardware\":\"ESP8266EX\x01\x1f\"}}"
	got, err := ParseStatus2(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got.Hardware != "ESP8266EX  " && got.Hardware != "ESP8266EX" {
		t.Errorf("Hardware = %q", got.Hardware)
	}
}

func TestParseUpgradeProbe(t *testing.T) {
	// Real capture from every fixture device.
	for _, dir := range fixtureDevices(t) {
		got, err := ParseUpgradeProbe(readFixture(t, dir, "upgrade.json"))
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		// Normal firmware gives the idle form too: Idle must not be read as
		// "minimal firmware" anywhere (detection lives in fwselect.Detect).
		if !got.Idle || got.Version != "16.0.0" {
			t.Errorf("%s: got %+v, want Idle with 16.0.0", filepath.Base(dir), got)
		}
	}
	tests := []struct {
		name, in string
		want     UpgradeProbe
		err      error
	}{
		// After "Upgrade 1" was accepted a second probe shows the pending form; it
		// is not the idle form, so the engine must not read it as "idle".
		{"accepted", `{"Upgrade":"Version 17.0.0 from http://h/fw.bin"}`, UpgradeProbe{Version: "17.0.0"}, nil},
		{"upload echo", `{"Upload":"1 or >16.0.0 to upgrade"}`, UpgradeProbe{Idle: true, Version: "16.0.0"}, nil},
		{"dev build", `{"Upgrade":"1 or >16.0.0.1 to upgrade"}`, UpgradeProbe{Idle: true, Version: "16.0.0.1"}, nil},
		{"safeboot", `{"Upgrade":"Safeboot"}`, UpgradeProbe{}, nil},
		{"failed", `{"Upgrade":"Failed 3"}`, UpgradeProbe{}, ErrBadResponse},
		{"empty", `{}`, UpgradeProbe{}, ErrBadResponse},
		{"number", `{"Upgrade":1}`, UpgradeProbe{}, ErrBadResponse},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseUpgradeProbe(json.RawMessage(tc.in))
			if !errors.Is(err, tc.err) || got != tc.want {
				t.Errorf("got %+v, %v; want %+v, %v", got, err, tc.want, tc.err)
			}
		})
	}
}

func TestParseOtaURL(t *testing.T) {
	want := map[string]string{
		"kitchen-spotlight":    "http://tasmoadmin.example.internal:80/data/firmwares/tasmota-minimal.bin.gz",
		"office-monitor-guest": "",
	}
	for _, dir := range fixtureDevices(t) {
		got, err := ParseOtaURL(readFixture(t, dir, "otaurl.json"))
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if w, ok := want[filepath.Base(dir)]; ok && w != "" && got != w {
			t.Errorf("%s: got %q", dir, got)
		}
		if !strings.HasPrefix(got, "http") {
			t.Errorf("%s: OtaUrl = %q", dir, got)
		}
	}
	// An empty stored URL is valid and distinct from a bad reply.
	if got, err := ParseOtaURL(json.RawMessage(`{"OtaUrl":""}`)); err != nil || got != "" {
		t.Errorf("empty: %q, %v", got, err)
	}
	if _, err := ParseOtaURL(json.RawMessage(`{"Foo":"x"}`)); !errors.Is(err, ErrBadResponse) {
		t.Errorf("missing key: %v", err)
	}
}

func TestParseReply_FixtureShapes(t *testing.T) {
	d := filepath.Join(fixturesDir, "kitchen-spotlight")
	if err := classifyReply(readFixture(t, d, "empty-cmnd.json")); err != nil {
		t.Errorf("empty-cmnd is a WARNING, not an auth failure: %v", err)
	}
	if err := classifyReply([]byte(`{"WARNING":"Need user=<username>&password=<password>"}`)); !errors.Is(err, ErrAuth) {
		t.Errorf("Need user: %v", err)
	}
	if err := classifyReply(readFixture(t, d, "unknown-foobar.json")); !errors.Is(err, ErrUnknownCommand) {
		t.Errorf("unknown: %v", err)
	}
	if err := classifyReply(readFixture(t, d, "status-2.json")); err != nil {
		t.Errorf("status-2: %v", err)
	}
	if err := classifyReply([]byte(`garbage`)); !errors.Is(err, ErrBadResponse) {
		t.Errorf("garbage: %v", err)
	}
	// Status 9 on a device without energy monitoring: callers that need the data
	// still see a bad response, the backup can tell it apart from a broken one.
	err := classifyReply([]byte(`{"Command":"Error","Input":"STATUS 9"}`))
	if !errors.Is(err, ErrCommandError) || !errors.Is(err, ErrBadResponse) {
		t.Errorf("command error: %v", err)
	}
}

const minimalRoot = `<!DOCTYPE html><html lang="de" class=""><head><meta charset='utf-8'><title>Prusa Plug Main Menu</title></head><body><div style='text-align:left;display:inline-block;color:#eaeaea;min-width:340px;'><div style='text-align:center;color:#eaeaea;'><h3>MINIMAL firmware<br>please upgrade</h3><h3>Sonoff Basic</h3><h2>Prusa Plug</h2></div><form id=but5 action='up' method='get'><button>Firmware Upgrade</button></form><div style='text-align:right;font-size:11px;'><hr><a href='https://github.com/arendst/Tasmota' target='_blank' style='color:#aaa;'>Tasmota 16.0.0 (release-minimal) by Theo Arends</a></div></div></body></html>`

func TestParseRootPage_Fixtures(t *testing.T) {
	for _, dir := range fixtureDevices(t) {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			got, err := ParseRootPage(readFixture(t, dir, "root.html"))
			if err != nil {
				t.Fatal(err)
			}
			footer := strings.TrimSpace(string(readFixture(t, dir, "root-footer.txt")))
			title := strings.TrimSpace(string(readFixture(t, dir, "root-title.txt")))
			if got.Version != "16.0.0" || footer != "Tasmota 16.0.0 ("+got.Image+") by Theo Arends" {
				t.Errorf("version/image %q %q vs footer %q", got.Version, got.Image, footer)
			}
			if "<title>"+got.Title != title {
				t.Errorf("Title %q vs %q", got.Title, title)
			}
			if got.Lang != "en" {
				t.Errorf("Lang = %q", got.Lang)
			}
			// Normal firmware must never be flagged minimal; that would trigger
			// the "upgrade out of minimal" recovery path on a healthy device.
			if got.Minimal {
				t.Error("normal firmware flagged Minimal")
			}
		})
	}
}

func TestParseRootPage_Minimal(t *testing.T) {
	got, err := ParseRootPage([]byte(minimalRoot))
	if err != nil {
		t.Fatal(err)
	}
	want := RootInfo{Version: "16.0.0", Image: "release-minimal", Lang: "de", Minimal: true, Title: "Prusa Plug Main Menu"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseRootPage_SafebootAndEdgeCases(t *testing.T) {
	safeboot := strings.NewReplacer("MINIMAL firmware<br>please upgrade", "Safeboot", "release-minimal", "release-tasmota32").Replace(minimalRoot)
	got, err := ParseRootPage([]byte(safeboot))
	if err != nil || !got.Minimal {
		t.Errorf("safeboot: %+v, %v", got, err)
	}
	// Device names are untrusted and HTML-escaped by the device.
	esc := strings.Replace(minimalRoot, "Prusa Plug Main Menu", "A &amp; B &lt;x&gt; Main Menu", 1)
	got, _ = ParseRootPage([]byte(esc))
	if got.Title != "A & B <x> Main Menu" {
		t.Errorf("Title = %q", got.Title)
	}
	// Dev builds carry a sha in the image name.
	dev := strings.Replace(minimalRoot, "16.0.0 (release-minimal)", "16.0.0 (abc1234-tasmota)", 1)
	got, _ = ParseRootPage([]byte(dev))
	if got.Version != "16.0.0" || got.Image != "abc1234-tasmota" {
		t.Errorf("dev build: %+v", got)
	}
	for _, bad := range []string{"", "<html>no footer</html>", "File Not Found\n\nURI: /\n"} {
		if _, err := ParseRootPage([]byte(bad)); !errors.Is(err, ErrBadResponse) {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}

func FuzzParseStatus2(f *testing.F) {
	for _, dir := range []string{"kitchen-spotlight", "office-monitor-guest"} {
		b, _ := os.ReadFile(filepath.Join(fixturesDir, dir, "status-2.json"))
		f.Add(b)
	}
	f.Add([]byte(`{"Command":"Unknown","Input":"STATUS 2"}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		got, err := ParseStatus2(b)
		if err != nil && got != (StatusFWR{}) {
			t.Fatalf("error with non-zero result: %+v, %v", got, err)
		}
	})
}

func FuzzParseUpgradeProbe(f *testing.F) {
	f.Add([]byte(`{"Upgrade":"1 or >16.0.0 to upgrade"}`))
	f.Add([]byte(`{"Upgrade":"Version 16.0.0 from http://x"}`))
	f.Add([]byte(`{"Command":"Unknown","Input":"UPGRADE"}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		got, err := ParseUpgradeProbe(b)
		if err != nil && got != (UpgradeProbe{}) {
			t.Fatalf("error with non-zero result: %+v, %v", got, err)
		}
	})
}

func FuzzParseRootPage(f *testing.F) {
	f.Add([]byte(minimalRoot))
	b, _ := os.ReadFile(filepath.Join(fixturesDir, "kitchen-spotlight", "root.html"))
	f.Add(b)
	f.Fuzz(func(t *testing.T, b []byte) {
		got, err := ParseRootPage(b)
		if err != nil && got != (RootInfo{}) {
			t.Fatalf("error with non-zero result: %+v, %v", got, err)
		}
	})
}
