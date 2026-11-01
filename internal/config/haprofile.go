package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// The Home Assistant profile (issue #209, plan C3). Inside a Home Assistant app the Supervisor
// sets SUPERVISOR_TOKEN and writes the options the user filled in on the app page to
// /data/options.json. Outside an app none of this exists, and nothing below runs.
//
// Only bootstrap and security settings become app options; operational settings stay on the
// System settings page (spec 1.1a). Every option maps to one TASMOFLEET_ key.

// supervisorTokenKey is the one variable that switches the profile on. It has no TASMOFLEET_ prefix
// because the Supervisor, not the operator, sets it.
const supervisorTokenKey = "SUPERVISOR_TOKEN"

// haOptionsFile is the Supervisor's options file, relative to the data directory.
const haOptionsFile = "options.json"

// haOptions are the keys of /data/options.json this build understands. They must equal the
// schema of deploy/homeassistant/tasmofleet/config.yaml (a test compares them).
type haOptions struct {
	// InitialAdminPassword seeds the first local admin (TASMOFLEET_INITIAL_ADMIN_PASSWORD).
	// A pointer so that null (an unset optional field) can be told from a wrong type.
	InitialAdminPassword *string `json:"initial_admin_password" key:"INITIAL_ADMIN_PASSWORD"`
}

// haDefaults replace the generic defaults inside an app, unless the environment, the env file
// or an option sets the key. The Log tab shows plain text, not JSON lines; the data directory
// and the two listeners are the container layout the app's port mapping refers to.
var haDefaults = map[string]string{
	"DATA_DIR":        "/data",
	"LISTEN":          ":8080",
	"FIRMWARE_LISTEN": ":80",
	"LOG_FORMAT":      "text",
}

// haOptionKeys lists the option names in declaration order.
func haOptionKeys() []string {
	var keys []string
	rt := reflect.TypeFor[haOptions]()
	for i := range rt.NumField() {
		keys = append(keys, rt.Field(i).Tag.Get("json"))
	}
	return keys
}

// inHomeAssistant reports whether the process runs as a Home Assistant app. An empty token counts
// as not set, like every other empty variable.
func inHomeAssistant(environ []string) bool {
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == supervisorTokenKey && strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

// applyHAProfile fills env (key without prefix) from options.json and the profile defaults. Keys
// already in env win: the environment and the env file are the operator's explicit choice.
// A missing, unreadable or unknown options file is an error, never silently ignored.
func applyHAProfile(env map[string]string, dataDir string) error {
	path := filepath.Join(dataDir, haOptionsFile)
	raw, err := os.ReadFile(path) //nolint:gosec // fixed file name inside the data directory
	if err != nil {
		return fmt.Errorf("home assistant app: %s: %w (SUPERVISOR_TOKEN is set, so the Supervisor must have written it)", path, err)
	}
	var opts haOptions
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&opts); err != nil {
		return fmt.Errorf("home assistant app: %s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("home assistant app: %s: unexpected data after the JSON object", path)
	}
	rv := reflect.ValueOf(opts)
	for i := range rv.NumField() {
		p, _ := rv.Field(i).Interface().(*string)
		if p == nil || strings.TrimSpace(*p) == "" {
			continue // null or empty: not set
		}
		key := rv.Type().Field(i).Tag.Get("key")
		if _, set := env[key]; !set {
			env[key] = *p
		}
	}
	for key, v := range haDefaults {
		if _, set := env[key]; !set {
			env[key] = v
		}
	}
	return nil
}
