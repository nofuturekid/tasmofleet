package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// haEnv is the environment of an app container: the Supervisor sets SUPERVISOR_TOKEN and
// writes /data/options.json. Tests cannot write /data, so haEnv points the profile's fixed options
// location at <dir>/options.json for the duration of the test.
func haEnv(t *testing.T, dir string, extra ...string) []string {
	t.Helper()
	old := haOptionsPath
	haOptionsPath = filepath.Join(dir, "options.json")
	t.Cleanup(func() { haOptionsPath = old })
	return append([]string{"PATH=/usr/bin", "SUPERVISOR_TOKEN=fake-token-for-tests", "TASMOFLEET_DATA_DIR=" + dir}, extra...)
}

func writeOptions(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "options.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Why: Docker and Unraid installs must behave exactly as before. A stray options.json
// in a data directory (or malformed junk in it) must be invisible without SUPERVISOR_TOKEN.
func TestNoSupervisorTokenMeansNoHAProfile(t *testing.T) {
	dir := t.TempDir()
	writeOptions(t, dir, `{"initial_admin_password": "from-options"}`)
	c, err := Load([]string{"TASMOFLEET_DATA_DIR=" + dir, "TASMOFLEET_CONFIG_FILE=/dev/null"})
	if err != nil {
		t.Fatal(err)
	}
	if c.HAProfile {
		t.Error("HAProfile is on without SUPERVISOR_TOKEN")
	}
	if c.InitialAdminPassword != "" {
		t.Error("options.json was read outside the HA profile")
	}
	if c.LogFormat != "json" {
		t.Errorf("LogFormat = %q, the Docker default is json", c.LogFormat)
	}
	writeOptions(t, dir, `{not json`)
	if _, err := Load([]string{"TASMOFLEET_DATA_DIR=" + dir}); err != nil {
		t.Errorf("a malformed options.json must not matter outside the HA profile: %v", err)
	}
}

// Why: an empty token is "not set", like every other empty variable.
func TestEmptySupervisorTokenIsNotTheHAProfile(t *testing.T) {
	c, err := Load([]string{"SUPERVISOR_TOKEN=", "TASMOFLEET_DATA_DIR=" + t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if c.HAProfile {
		t.Error("an empty SUPERVISOR_TOKEN switched the HA profile on")
	}
}

// Why: the Log tab of an app is plain text; JSON lines are unreadable there. The data
// directory and both listeners are fixed by the container layout (ports are mapped by the Supervisor).
func TestHAProfileDefaults(t *testing.T) {
	dir := t.TempDir()
	writeOptions(t, dir, `{}`)
	c, err := Load(haEnv(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if !c.HAProfile {
		t.Fatal("HAProfile is off with SUPERVISOR_TOKEN set")
	}
	if c.LogFormat != "text" || c.Listen != ":8080" || c.FWListen != ":80" || c.DataDir != dir {
		t.Errorf("profile defaults wrong: format %q listen %q firmware %q data %q", c.LogFormat, c.Listen, c.FWListen, c.DataDir)
	}
	if !c.TLSSelfSigned {
		t.Error("the direct port must serve the self-signed certificate (local auth, no proxy)")
	}
}

// Why: the Supervisor writes its options to /data/options.json whatever TASMOFLEET_DATA_DIR says.
// A container with a different data directory must still get its app options, and a stale
// options.json inside that directory must not be mistaken for them.
func TestHAOptionsLocationIsFixedNotDataDir(t *testing.T) {
	if haOptionsFile != "/data/options.json" {
		t.Fatalf("the Supervisor's options file is /data/options.json, the code says %q", haOptionsFile)
	}
	supervisorDir, otherData := t.TempDir(), t.TempDir()
	writeOptions(t, supervisorDir, `{"initial_admin_password": "from-supervisor"}`)
	writeOptions(t, otherData, `{"initial_admin_password": "stale-in-data-dir"}`)
	env := haEnv(t, supervisorDir) // points the options location at supervisorDir
	env[len(env)-1] = "TASMOFLEET_DATA_DIR=" + otherData
	c, err := Load(env)
	if err != nil {
		t.Fatal(err)
	}
	if c.InitialAdminPassword != "from-supervisor" || c.DataDir != otherData {
		t.Errorf("options come from the Supervisor's location, data from DATA_DIR: password %q data %q", c.InitialAdminPassword, c.DataDir)
	}
}

// Why: an explicit environment variable is the operator's override and wins over the app options.
func TestHAOptionsFeedBootstrapSettings(t *testing.T) {
	dir := t.TempDir()
	writeOptions(t, dir, `{"initial_admin_password": "from-options"}`)
	c, err := Load(haEnv(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if c.InitialAdminPassword != "from-options" {
		t.Errorf("option not applied: %q", c.InitialAdminPassword)
	}
	c, err = Load(haEnv(t, dir, "TASMOFLEET_INITIAL_ADMIN_PASSWORD=from-env", "TASMOFLEET_LOG_FORMAT=json"))
	if err != nil {
		t.Fatal(err)
	}
	if c.InitialAdminPassword != "from-env" || c.LogFormat != "json" {
		t.Errorf("environment must win over options and profile defaults: %q %q", c.InitialAdminPassword, c.LogFormat)
	}
}

// Why: the HA form writes an empty or null value for an unset optional field; that is "not set"
// (a random password is generated), never an empty password.
func TestHAOptionsEmptyOrNullCountAsNotSet(t *testing.T) {
	for _, body := range []string{`{}`, `{"initial_admin_password": ""}`, `{"initial_admin_password": null}`} {
		dir := t.TempDir()
		writeOptions(t, dir, body)
		c, err := Load(haEnv(t, dir))
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if c.InitialAdminPassword != "" {
			t.Errorf("%s: password %q", body, c.InitialAdminPassword)
		}
	}
}

// Why: fail loud (CLAUDE.md). If options.json is missing, unreadable or does not match what this
// build understands (an app/image version mismatch), starting with silent defaults would hide a
// broken install; the error names the file.
func TestHAProfileFailsLoudOnBadOptions(t *testing.T) {
	cases := map[string]string{
		"malformed":      `{not json`,
		"not an object":  `["a"]`,
		"unknown option": `{"initial_admin_pasword": "typo"}`,
		"wrong type":     `{"initial_admin_password": 5}`,
		"trailing data":  `{} {}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeOptions(t, dir, body)
			_, err := Load(haEnv(t, dir))
			if err == nil || !strings.Contains(err.Error(), "options.json") {
				t.Fatalf("want an error naming options.json, got %v", err)
			}
		})
	}
	t.Run("missing", func(t *testing.T) {
		_, err := Load(haEnv(t, t.TempDir()))
		if err == nil || !strings.Contains(err.Error(), "options.json") {
			t.Fatalf("want an error naming options.json, got %v", err)
		}
	})
}

// Why: the configdoc table lists env variables; the app options are a second input and are
// documented next to it. This test keeps docs and code in step for the option names.
func TestHAOptionsAreDocumented(t *testing.T) {
	b, err := os.ReadFile("../../docs/configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(string(b), "\n## Home Assistant app\n")
	if !ok {
		t.Fatal(`docs/configuration.md has no "## Home Assistant app" section`)
	}
	section, _, _ := strings.Cut(after, "\n## ")
	for _, key := range haOptionKeys() {
		if !strings.Contains(section, "`"+key+"`") {
			t.Errorf("app option %s is not documented", key)
		}
	}
}
