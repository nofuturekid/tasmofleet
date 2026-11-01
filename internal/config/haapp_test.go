package config

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// Why: the options the app form offers (deploy/homeassistant/tasmofleet/config.yaml) and the
// options this build accepts must be the same set. The Supervisor validates the form against
// the schema; the binary rejects unknown keys. A drift means a form field that stops the app
// from starting (or an option that does nothing).
func TestAppConfigOptionsMatchTheCode(t *testing.T) {
	b, err := os.ReadFile("../../deploy/homeassistant/tasmofleet/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Collect the keys of the top-level "schema:" block (two-space indented lines).
	var got []string
	in := false
	for line := range strings.SplitSeq(string(b), "\n") {
		switch {
		case strings.HasPrefix(line, "schema:"):
			in = true
		case in && strings.HasPrefix(line, "  "):
			key, _, _ := strings.Cut(strings.TrimSpace(line), ":")
			got = append(got, strings.TrimSuffix(key, "?"))
		case in && line != "" && !strings.HasPrefix(line, "#"):
			in = false
		}
	}
	want := haOptionKeys()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("config.yaml schema keys %v, code accepts %v", got, want)
	}
}

// Why: the app page shows icon.png and logo.png. They are copies of the branding files; a copy
// that drifts would show an old logo in the store.
func TestAppIconsAreTheBrandingFiles(t *testing.T) {
	for app, brand := range map[string]string{"icon.png": "addon-icon.png", "logo.png": "addon-logo.png"} {
		a, err := os.ReadFile("../../deploy/homeassistant/tasmofleet/" + app)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile("../../assets/branding/" + brand)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Errorf("%s differs from assets/branding/%s", app, brand)
		}
	}
}

// Why: Home Assistant only learns the ports from config.yaml. The UI must be on container port 8080
// (the profile's listener) and the firmware server on 80 (the profile's firmware listener), or the
// buttons and the devices point to nothing. The webui link must use the mapped port.
func TestAppPortsMatchTheProfile(t *testing.T) {
	b, err := os.ReadFile("../../deploy/homeassistant/tasmofleet/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"\n  8080/tcp: 8080\n", "\n  80/tcp: 8081\n", `webui: "https://[HOST]:[PORT:8080]"`, "\nimage: ghcr.io/nofuturekid/tasmofleet-ha\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("config.yaml lacks %q", strings.TrimSpace(want))
		}
	}
	if haDefaults["LISTEN"] != ":8080" || haDefaults["FIRMWARE_LISTEN"] != ":80" {
		t.Errorf("profile listeners %q / %q do not match the app's container ports 8080 / 80", haDefaults["LISTEN"], haDefaults["FIRMWARE_LISTEN"])
	}
}
