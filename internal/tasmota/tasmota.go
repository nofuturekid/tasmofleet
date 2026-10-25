// Package tasmota speaks the Tasmota HTTP command API (/cm?cmnd=...) and
// the /dl config download, and parses the responses tasmofleet relies on
// (Status 1/2/4/5/11, OtaUrl, Upgrade).
//
// SSRF rule: a Target can only be built from an inventory device
// (TargetFor), and the Client refuses to dial any IP outside the configured
// device networks (checked on the resolved address at dial time, so DNS
// rebinding does not help). There is no API to send a request to an
// arbitrary URL.
//
// The package has no MQTT code; MQTT is package mqttbus.
package tasmota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Errors. Callers match with errors.Is.
var (
	ErrUnknownCommand = errors.New("tasmota: unknown command")       // {"Command":"Unknown"}
	ErrAuth           = errors.New("tasmota: authentication needed") // HTTP 401 or {"WARNING":"Need user=..."}
	ErrUnreachable    = errors.New("tasmota: device unreachable")    // dial/connect failure
	ErrTimeout        = errors.New("tasmota: timeout")
	ErrForbiddenAddr  = errors.New("tasmota: address outside device networks")
	ErrBadResponse    = errors.New("tasmota: unparsable response")
	// ErrCommandError is {"Command":"Error"}: a known command that has nothing to
	// say on this device (Status 9 without energy monitoring). It is a bad
	// response for every caller that needs the data, hence the wrap.
	ErrCommandError   = fmt.Errorf("%w: device answered Command Error", ErrBadResponse)
	ErrNotImplemented = errors.New("tasmota: not implemented")
)

// Target is an opaque, inventory-derived HTTP endpoint of one device.
type Target struct {
	device model.DeviceID
	host   string
	port   int
	user   string
	pass   string
}

// TargetFor builds the target of an inventory device.
func TargetFor(d model.Device) Target {
	port := d.Port
	if port == 0 {
		port = 80
	}
	return Target{device: d.ID, host: d.Address, port: port, user: d.HTTPUser, pass: d.HTTPPassword}
}

// HasCredentials reports whether requests to this target carry a web user or
// password. The MAC probes of an address change must not.
func (t Target) HasCredentials() bool { return t.user != "" || t.pass != "" }

// DeviceID returns the inventory ID the target was built from.
func (t Target) DeviceID() model.DeviceID { return t.device }

// Client is the device protocol used by ota, backup, health (HTTP poll) and
// the web console. Implementations must be safe for concurrent use.
type Client interface {
	// Command sends one Tasmota command (e.g. "Status 2", "OtaUrl http://...",
	// "Upgrade 1", "Backlog ...") and returns the raw JSON object.
	Command(ctx context.Context, t Target, cmnd string) (json.RawMessage, error)
	// DownloadConfig fetches /dl (binary .dmp config dump).
	DownloadConfig(ctx context.Context, t Target) ([]byte, error)
	// RootPage fetches GET / and parses the footer and <html lang>. It is the
	// only way to learn the image name ("release-minimal") and the language
	// build of a device running minimal firmware.
	RootPage(ctx context.Context, t Target) (RootInfo, error)
}

// RootInfo is parsed from the device's main page.
type RootInfo struct {
	Version string // "16.0.0" from "Tasmota 16.0.0 (release-minimal) by Theo Arends"
	Image   string // "release-minimal"
	Lang    string // <html lang="..">, e.g. "en", "de"
	Minimal bool   // page shows "MINIMAL firmware" (or "Safeboot" on ESP32)
	Title   string // "<DeviceName> Main Menu"
}

// Options configure the HTTP client.
type Options struct {
	AllowedNetworks []netip.Prefix // device networks; empty means none allowed
	// Networks, if set, replaces AllowedNetworks and can change while the client runs.
	Networks *NetworkSet
	// Timeout overrides the per-request timeout of every class. Zero uses the
	// classes of the spec: 10 s Status/GET /, 15 s OtaUrl/Upgrade, 20 s /dl.
	Timeout     time.Duration
	MaxResponse int64 // bytes, default 1 MiB (config dumps are ~4 KiB)
	// MaxConcurrent is the global limit of requests in flight across all
	// devices, default 8.
	MaxConcurrent int
	// Logger receives operational notes (the WebLog fix). Default slog.Default().
	Logger *slog.Logger
	// At most one request per device at a time (ESP8266 web servers are
	// single-threaded); requests to the same device queue.
	// Credentials go in an Authorization: Basic header, never in the query
	// string, and are redacted from every error and log line.
}
