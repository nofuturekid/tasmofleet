package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/simulator"
)

// designFixture is the part of testdata/design/v2-fixture.json the devenv
// reproduces: the prototype's 60 devices (design/v2/template.html makeDevices,
// extracted by design/tools/v2-fixture.mjs) and its release list. Rules,
// backups, users and scan results are seeded by the e2e support code.
type designFixture struct {
	Latest   string   `json:"latest"`
	Releases []string `json:"releases"`
	// ReleaseNotes are the date and notes of the newest releases (the firmware page's list).
	ReleaseNotes []designRelease `json:"releaseNotes"`
	Devices      []struct {
		Name string `json:"name"`
		// FriendlyName, when set, is the device's Friendly name 1 (the README media's fleet sets it to the
		// name; without it the simulator keeps the name of the capture it replays).
		FriendlyName string `json:"friendlyName"`
		Chip         string `json:"chip"`
		Firmware     string `json:"firmware"`
		Status       string `json:"status"`
		Power        bool   `json:"power"`
		Relays       int    `json:"relays"`
		TelePeriod   int    `json:"telePeriod"`
		PowerOnState int    `json:"powerOnState"`
		MqttHost     string `json:"mqttHost"`
	} `json:"devices"`
}

// designRelease is one release of the prototype's list: "6 Oct 2026" and its bullet points.
type designRelease struct {
	Version string   `json:"version"`
	Date    string   `json:"date"`
	Notes   []string `json:"notes"`
}

// feedRelease is what the fake GitHub feed says about one release of the fixture: the
// prototype's date and notes as a Markdown list. ok is false for releases the fixture has no
// notes for (they keep the generic body).
func (fx *designFixture) feedRelease(version string) (published time.Time, body string, ok bool) {
	if fx == nil {
		return time.Time{}, "", false
	}
	for _, r := range fx.ReleaseNotes {
		if r.Version != version {
			continue
		}
		t, err := time.Parse("2 Jan 2006", r.Date)
		if err != nil {
			return time.Time{}, "", false
		}
		var b strings.Builder
		for _, n := range r.Notes {
			b.WriteString("- " + n + "\n")
		}
		return t.Add(12 * time.Hour), b.String(), true
	}
	return time.Time{}, "", false
}

func loadDesignFixture(file string) (*designFixture, error) {
	b, err := os.ReadFile(file) //nolint:gosec // path given on the command line of a test tool
	if err != nil {
		return nil, err
	}
	var fx designFixture
	if err := json.Unmarshal(b, &fx); err != nil {
		return nil, fmt.Errorf("design fixture %s: %w", file, err)
	}
	if fx.Latest == "" || len(fx.Releases) == 0 || len(fx.Devices) == 0 {
		return nil, fmt.Errorf("design fixture %s: latest, releases and devices are required", file)
	}
	return &fx, nil
}

// designProfiles maps the prototype's chip names to simulator profiles.
var designProfiles = map[string]simulator.Profile{
	"ESP8266":  simulator.ProfileESP8266_4M,
	"ESP32":    simulator.ProfileESP32,
	"ESP32-C3": simulator.ProfileESP32C3,
}

// designFleet is the running fixture fleet. Online devices are simulators;
// "offline" ones are simulators too, until GoOffline closes them (a device the
// app has seen online and then lost is "Offline", one it never reached is
// "Unknown"); "unknown" ones are never started, only a closed loopback port.
type designFleet struct {
	mu      sync.Mutex
	offline []simulator.Device
}

// GoOffline pulls the plug on every device the fixture lists as offline: the
// broker fires their will (LWT Offline), which is what makes the app show them
// as Offline right away instead of after their telemetry went stale.
func (f *designFleet) GoOffline() {
	f.mu.Lock()
	devs := f.offline
	f.offline = nil
	f.mu.Unlock()
	for _, d := range devs {
		if c, ok := simulator.Control(d); ok {
			c.PowerOff()
		}
	}
}

// startDesignFleet adds the fixture's devices and prints one DEVENV_DEVICE line
// each. Names contain spaces, so the name is URL-escaped.
func startDesignFleet(ctx context.Context, fleet simulator.Fleet, fx *designFixture, out func(format string, a ...any)) (*designFleet, error) {
	df := &designFleet{}
	unknown := 0 // devices that never answer, each on its own closed port
	for _, d := range fx.Devices {
		profile, ok := designProfiles[d.Chip]
		if !ok {
			return nil, fmt.Errorf("design fixture: device %q has unknown chip %q", d.Name, d.Chip)
		}
		if d.Status == "unknown" {
			addr := closedLoopbackAddr(unknown)
			unknown++
			// Nothing listens there: the device never answers, which the app shows as
			// "Unknown" (never answered), also after its first polls have failed (P1).
			out("DEVENV_DEVICE name=%s addr=%s topic=%s profile=%s version=%s unreachable\n", url.PathEscape(d.Name), addr, topicOf(d.Name), profile, d.Firmware)
			continue
		}
		power := make([]bool, max(d.Relays, 1))
		power[0] = d.Power
		if d.Relays == 2 {
			power[1] = !d.Power // the prototype shows relay 2 as the opposite of relay 1
		}
		var friendly []string
		if d.FriendlyName != "" {
			friendly = []string{d.FriendlyName}
		}
		dev, err := fleet.Add(ctx, simulator.DeviceSpec{
			Name: d.Name, Profile: profile, Version: d.Firmware, Relays: d.Relays, Power: power, FriendlyNames: friendly,
			Settings: map[string]string{
				"teleperiod":   fmt.Sprint(d.TelePeriod),
				"poweronstate": fmt.Sprint(d.PowerOnState),
				"mqtthost":     d.MqttHost,
			},
		})
		if err != nil {
			return nil, err
		}
		if d.Status == "offline" {
			df.offline = append(df.offline, dev)
		}
		out("DEVENV_DEVICE name=%s addr=%s topic=%s profile=%s version=%s mqtt\n", url.PathEscape(d.Name), dev.Addr(), dev.Topic(), profile, dev.Version())
	}
	return df, nil
}

var notTopicChars = regexp.MustCompile(`[^a-z0-9_.-]+`)

// topicOf is the topic the simulator gives a device of that name.
func topicOf(name string) string {
	return strings.Trim(notTopicChars.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-"), "-")
}

// closedLoopbackAddr returns the address of the i-th device that never
// answers: a privileged loopback port (1, 2, ...). No unprivileged process can
// bind it, so neither a simulator started later nor anything else can take it
// over; a released ephemeral port could be handed out again (issue #39).
func closedLoopbackAddr(i int) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(i+1))
}
