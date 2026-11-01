package simulator

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// profileDef maps a Profile to the capture it replays. Real captures live
// in testdata/fixtures/devices/<Dir>; synthetic profiles (testdata/fixtures/profiles/<Dir>, Dir starting with
// "synthetic-") hold only a profile.json that names a real base capture and
// the text replacements that turn it into the other hardware.
type profileDef struct {
	Dir      string
	Chip     string // esp8266, esp32, esp32c3, esp32s2, esp32s3, unknown
	FlashKB  int
	Minimal  bool   // starts in the minimal image
	DualOTA  bool   // ESP32 with two app partitions: no safeboot to switch to, /up shows the plain upload form
	Hardware string // filled from the capture
}

var profileDefs = map[Profile]profileDef{
	ProfileESP8266_1M:  {Dir: "livingroom-readinglamp", Chip: "esp8266", FlashKB: 1024},
	ProfileESP8266_4M:  {Dir: "synthetic-esp8266-4m", Chip: "esp8266", FlashKB: 4096},
	ProfileESP8266Min:  {Dir: "livingroom-readinglamp", Chip: "esp8266", FlashKB: 1024, Minimal: true},
	ProfileESP32:       {Dir: "office-monitor-guest", Chip: "esp32", FlashKB: 4096},
	ProfileESP32Dual:   {Dir: "synthetic-esp32-dual", Chip: "esp32", FlashKB: 4096, DualOTA: true},
	ProfileESP32C3:     {Dir: "synthetic-esp32c3", Chip: "esp32c3", FlashKB: 4096},
	ProfileESP32S2:     {Dir: "synthetic-esp32s2", Chip: "esp32s2", FlashKB: 4096},
	ProfileESP32S3:     {Dir: "synthetic-esp32s3", Chip: "esp32s3", FlashKB: 4096},
	ProfileUnknownChip: {Dir: "synthetic-unknown-chip", Chip: "unknown", FlashKB: 4096},
	// The two NOUS A1T plugs of the fleet: one model, one user template, Module 0 (#327).
	ProfileNOUSA1TBambu: {Dir: "mr-bambu-plug", Chip: "esp8266", FlashKB: 4096},
	ProfileNOUSA1TPrusa: {Dir: "prusa-plug", Chip: "esp8266", FlashKB: 4096},
}

// syntheticFile is the profile.json of a synthetic profile directory.
type syntheticFile struct {
	Base        string `json:"base"`
	Description string `json:"description"`
	Replace     []struct {
		Old string `json:"old"`
		New string `json:"new"`
	} `json:"replace"`
}

// capture is one device capture: file name -> raw bytes, after the
// synthetic replacements of its profile.
type capture struct {
	baseTopic string // MQTT topic of the captured device (= directory of the base capture)
	files     map[string][]byte
	mac       string // colon form, from status-5.json
	ip        string
	hardware  string
	image     string // image name in the capture, e.g. "tasmota" or "tasmota32"
	version   string
}

var (
	reMAC      = regexp.MustCompile(`"Mac":"([0-9A-Fa-f:]{17})"`)
	reIP       = regexp.MustCompile(`"IPAddress":"([0-9.]+)"`)
	reHardware = regexp.MustCompile(`"Hardware":"([^"]+)"`)
	reVersion  = regexp.MustCompile(`"Version":"([0-9.]+)\(release-([a-z0-9]+)\)"`)
)

func loadCapture(fixturesDir string, def profileDef) (*capture, error) {
	devices := filepath.Join(fixturesDir, "devices")
	base := def.Dir
	var syn syntheticFile
	if strings.HasPrefix(def.Dir, "synthetic-") {
		b, err := os.ReadFile(filepath.Join(fixturesDir, "profiles", def.Dir, "profile.json")) //nolint:gosec // fixtures dir is trusted test input
		if err != nil {
			return nil, fmt.Errorf("simulator: synthetic profile %s: %w", def.Dir, err)
		}
		if err := json.Unmarshal(b, &syn); err != nil {
			return nil, fmt.Errorf("simulator: synthetic profile %s: %w", def.Dir, err)
		}
		base = syn.Base
	}
	entries, err := os.ReadDir(filepath.Join(devices, base))
	if err != nil {
		return nil, fmt.Errorf("simulator: capture %s: %w", base, err)
	}
	c := &capture{baseTopic: base, files: map[string][]byte{}}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(devices, base, e.Name())) //nolint:gosec // fixtures dir is trusted test input
		if err != nil {
			return nil, fmt.Errorf("simulator: capture %s: %w", base, err)
		}
		for _, r := range syn.Replace {
			b = []byte(strings.ReplaceAll(string(b), r.Old, r.New))
		}
		c.files[e.Name()] = b
	}
	s5 := string(c.files["status-5.json"])
	if m := reMAC.FindStringSubmatch(s5); m != nil {
		c.mac = m[1]
	}
	if m := reIP.FindStringSubmatch(s5); m != nil {
		c.ip = m[1]
	}
	if m := reHardware.FindStringSubmatch(string(c.files["status-2.json"])); m != nil {
		c.hardware = m[1]
	}
	if m := reVersion.FindStringSubmatch(string(c.files["status-2.json"])); m != nil {
		c.version, c.image = m[1], m[2]
	}
	if c.mac == "" || c.ip == "" || c.hardware == "" || c.version == "" {
		return nil, errors.New("simulator: capture " + base + " lacks Mac, IPAddress, Hardware or Version")
	}
	return c, nil
}

// discoveryFor returns the retained discovery config and sensors payloads
// of the captured device, or nil when the fleet inventory has none.
func discoveryFor(fixturesDir string, c *capture) (config, sensors []byte) {
	b, err := os.ReadFile(filepath.Join(fixturesDir, "mqtt", "discovery-retained.jsonl")) //nolint:gosec // fixtures dir is trusted test input
	if err != nil {
		return nil, nil
	}
	needle := `"hn":"` + c.baseTopic + `"`
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, needle) {
			config = []byte(line)
			break
		}
	}
	macCompact := strings.ReplaceAll(c.mac, ":", "")
	s, err := os.ReadFile(filepath.Join(fixturesDir, "mqtt", "samples", "discovery-sensors.txt")) //nolint:gosec // trusted fixtures dir
	if err == nil {
		for _, line := range strings.Split(string(s), "\n") {
			parts := strings.SplitN(line, "|", 4)
			if len(parts) == 4 && strings.Contains(parts[2], macCompact) {
				sensors = []byte(parts[3])
			}
		}
	}
	return config, sensors
}
