// Command tasmofleet-devenv starts a self-contained test environment for
// e2e tests and manual UI work: an embedded MQTT broker, a mock OIDC
// provider (Authentik semantics) and a fleet of simulated Tasmota devices
// (one per simulator profile). It prints the environment variables to start
// tasmofleet against it. Never used in production images.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/testutil/oidcmock"
)

// loginUsers are the accounts the mock IdP can log in as, with the Authentik
// group names of the reference deployment.
var loginUsers = map[string]oidcmock.User{
	"operator": {Subject: "u-operator", Username: "olivia", Email: "olivia@example.test", Groups: []string{"Tasmofleet Users"}},
	"admin":    {Subject: "u-admin", Username: "adam", Email: "adam@example.test", Groups: []string{"Tasmofleet Users", "Tasmofleet Admins"}},
	"nobody":   {Subject: "u-nobody", Username: "nina", Email: "nina@example.test", Groups: []string{"Unrelated"}},
}

// multiFlag collects repeated flag values.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	if len(os.Args) > 1 && os.Args[1] == fakeDecoderArg {
		os.Exit(fakeDecoder(os.Args[2:], os.Stdout, os.Stderr))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "tasmofleet-devenv:", err)
		os.Exit(1)
	}
}

// fleetDevices is the mixed fleet: every profile once, MQTT and HTTP-only,
// one device behind a WebPassword. All start at 16.0.0 so a 16.1.0 upstream
// offers an update to each of them.
func fleetDevices() []simulator.DeviceSpec {
	httpOnly := []simulator.Fault{{Kind: simulator.FaultHTTPOnly}}
	return []simulator.DeviceSpec{
		{Name: "lamp-1m", Profile: simulator.ProfileESP8266_1M},
		{Name: "plug-4m", Profile: simulator.ProfileESP8266_4M},
		{Name: "plug-4m-http", Profile: simulator.ProfileESP8266_4M, Faults: httpOnly},
		{Name: "lamp-minimal", Profile: simulator.ProfileESP8266Min},
		{Name: "esp32-dimmer", Profile: simulator.ProfileESP32},
		{Name: "esp32c3-sensor", Profile: simulator.ProfileESP32C3},
		{Name: "esp32s3-http", Profile: simulator.ProfileESP32S3, Faults: httpOnly},
		{Name: "esp32s2-board", Profile: simulator.ProfileESP32S2},
		// An ESP32 with the safeboot layout: its running partition has no room for a full image (#30).
		{Name: "esp32-safeboot-layout", Profile: simulator.ProfileESP32, FreeKB: 1},
		// An ESP32 with two app partitions: the only layout that takes the plain push into the running app (#191).
		{Name: "esp32-dual-ota", Profile: simulator.ProfileESP32Dual},
		{Name: "mystery-chip", Profile: simulator.ProfileUnknownChip},
		{Name: "locked-plug", Profile: simulator.ProfileESP8266_4M, User: "admin", Password: "devenv-fake-password"},
	}
}

// overviewDevices are the shapes of the Overview mockups, one device each (docs/design/device-overview):
// a metered plug, a plug without a meter, two relays on one meter, four relays metered per channel with
// names, four relays over Ethernet, a sensor-only device, an HTTP-only plug and a device the e2e tests
// take offline.
func overviewDevices() []simulator.DeviceSpec {
	return []simulator.DeviceSpec{
		{Name: "ov-metered-plug", Profile: simulator.ProfileESP8266_4M, Energy: 1},
		{Name: "ov-plain-plug", Profile: simulator.ProfileESP8266_4M},
		{Name: "ov-double-switch", Profile: simulator.ProfileESP8266_4M, Relays: 2, Energy: 1},
		{Name: "ov-power-strip", Profile: simulator.ProfileESP32C3, Relays: 4, Energy: 4, Power: []bool{true, true, true, false},
			FriendlyNames: []string{"Monitor", "PC", "Lamp", "Charger"}},
		{Name: "ov-ethernet-rack", Profile: simulator.ProfileESP32, Relays: 4, Wired: true},
		{Name: "ov-climate", Profile: simulator.ProfileESP32C3, NoRelay: true, Sensors: map[string]map[string]float64{
			"BME280": {"Temperature": 12.4, "Humidity": 71, "Pressure": 1012}, "BH1750": {"Illuminance": 3400}}},
		{Name: "ov-http-only", Profile: simulator.ProfileESP8266_4M, Faults: []simulator.Fault{{Kind: simulator.FaultHTTPOnly}}},
		{Name: "ov-offline", Profile: simulator.ProfileESP8266_1M},
	}
}

// nousGPIO is the captured NOUS A1T template (14 slots); relay is the code on GPIO14 (slot 10): 224 is
// Relay1, 320 is Led_i1.
func nousGPIO(relay int) []int {
	g := []int{32, 0, 0, 0, 2720, 2656, 0, 0, 2624, 320, 224, 0, 0, 0}
	g[10] = relay
	return g
}

// esp32GPIO is a 36-slot ESP32 template with Relay1 (224) or Led_i1 (320) on slot 2.
func esp32GPIO(code int) []int {
	g := make([]int, 36)
	g[1], g[2] = 32, code
	return g
}

// templateDevices are the devices of the GPIO templates tab (#327): three NOUS A1T plugs with the same pin
// map and one that has Led_i1 instead of Relay1 on GPIO14, a minimal-firmware device and an offline-able one
// with the same template name, a tie (2 + 2), a device on the built-in module "Sonoff Basic", and a group of two
// ESP32 boards where one differs. Opt-in because other tests count the fleet.
func templateDevices() []simulator.DeviceSpec {
	nous := func(relay int) *simulator.UserTemplate {
		return &simulator.UserTemplate{Name: "NOUS A1T", GPIO: nousGPIO(relay), Base: 49}
	}
	tie := func(relay int) *simulator.UserTemplate {
		return &simulator.UserTemplate{Name: "Tie Plug", GPIO: nousGPIO(relay), Base: 18}
	}
	esp32 := func(code int) *simulator.UserTemplate {
		return &simulator.UserTemplate{Name: "Dimmer Board", GPIO: esp32GPIO(code), Base: 1}
	}
	builtin := 1
	return []simulator.DeviceSpec{
		{Name: "tpl-nous-1", Profile: simulator.ProfileNOUSA1TPrusa, Template: nous(224)},
		{Name: "tpl-nous-2", Profile: simulator.ProfileNOUSA1TBambu, Template: nous(224)},
		{Name: "tpl-nous-3", Profile: simulator.ProfileNOUSA1TPrusa, Template: nous(224)},
		{Name: "tpl-nous-dev", Profile: simulator.ProfileNOUSA1TBambu, Template: nous(320)},
		{Name: "tpl-minimal", Profile: simulator.ProfileESP8266Min, Template: nous(320)},
		{Name: "tpl-offline", Profile: simulator.ProfileNOUSA1TPrusa, Template: nous(320)},
		{Name: "tpl-tie-1", Profile: simulator.ProfileESP8266_4M, Template: tie(224)},
		{Name: "tpl-tie-2", Profile: simulator.ProfileESP8266_4M, Template: tie(224)},
		{Name: "tpl-tie-3", Profile: simulator.ProfileESP8266_4M, Template: tie(320)},
		{Name: "tpl-tie-4", Profile: simulator.ProfileESP8266_4M, Template: tie(320)},
		{Name: "tpl-builtin", Profile: simulator.ProfileESP8266_4M, Module: &builtin},
		{Name: "tpl-esp32-1", Profile: simulator.ProfileESP32, Template: esp32(224)},
		{Name: "tpl-esp32-2", Profile: simulator.ProfileESP32, Template: esp32(224)},
		{Name: "tpl-esp32-3", Profile: simulator.ProfileESP32, Template: esp32(320)},
	}
}

// stabilityDevices are the devices of the Stability card (#325): one that stays stable, one the tests make
// restart with a reason (POST /device/{name}/restart?reason=...), and one with a weak link from the start
// (-84 dBm, 14 reconnects). Opt-in because other tests count the fleet.
func stabilityDevices() []simulator.DeviceSpec {
	return []simulator.DeviceSpec{
		{Name: "st-stable", Profile: simulator.ProfileESP8266_4M},
		{Name: "st-bootloop", Profile: simulator.ProfileESP32C3, RebootGap: 200 * time.Millisecond},
		{Name: "st-weakwifi", Profile: simulator.ProfileESP8266_1M, SignalDBm: -84, LinkCount: 15},
	}
}

// MQTTPassword is the MqttPassword of the devices below; the devenv broker takes any, so it only has to be
// the value TasmoFleet is told for FileDownload/FileUpload.
const MQTTPassword = "devenv-fake-mqtt-password" //nolint:gosec // G101: a fake value for the dev broker, which takes any password

// mqttTransportDevices are the devices of the MQTT transport (0.20.0, #79): TasmoFleet reaches them only
// over the broker. A staged ESP8266 1 MB; an ESP32 and an ESP32 with two app partitions that cannot reach
// the firmware server either (the dual one can only be updated by Type 1 file upload); a 1 MB device that
// reaches neither.
func mqttTransportDevices() []simulator.DeviceSpec {
	noHTTP := simulator.Fault{Kind: simulator.FaultHTTPUnreachable}
	noFW := simulator.Fault{Kind: simulator.FaultFWUnreachable}
	return []simulator.DeviceSpec{
		{Name: "mq-esp8266-1m", Profile: simulator.ProfileESP8266_1M, MQTTPassword: MQTTPassword, Faults: []simulator.Fault{noHTTP}},
		{Name: "mq-esp32", Profile: simulator.ProfileESP32, MQTTPassword: MQTTPassword, Faults: []simulator.Fault{noHTTP}},
		{Name: "mq-fw-blocked", Profile: simulator.ProfileESP8266_1M, MQTTPassword: MQTTPassword, Faults: []simulator.Fault{noHTTP, noFW}},
		{Name: "mq-esp32-dual", Profile: simulator.ProfileESP32Dual, MQTTPassword: MQTTPassword, Faults: []simulator.Fault{noHTTP, noFW}},
	}
}

// extraProfiles are cycled through by the additional devices of -fleet-size, so
// a large fleet still mixes ESP8266 and ESP32 variants.
var extraProfiles = []simulator.Profile{
	simulator.ProfileESP8266_4M, simulator.ProfileESP32, simulator.ProfileESP8266_1M,
	simulator.ProfileESP32C3, simulator.ProfileESP32S3, simulator.ProfileESP8266_4M,
}

// LongDeviceName is the first extra device of a large fleet: a name as long as
// people really give them, with no break opportunity.
const LongDeviceName = "workshop-ceiling-spotlight-controller-north-wing"

// fleetOfSize is the base fleet plus sim-NN devices up to n devices in total.
func fleetOfSize(n int) []simulator.DeviceSpec {
	specs := fleetDevices()
	for i := 0; len(specs) < n; i++ {
		specs = append(specs, simulator.DeviceSpec{Name: fmt.Sprintf("sim-%02d-%s", i+1, extraProfiles[i%len(extraProfiles)]), Profile: extraProfiles[i%len(extraProfiles)]})
	}
	if len(specs) > len(fleetDevices()) {
		// Real homes name devices like this; long unbreakable names are what stretch tables.
		specs[len(fleetDevices())].Name = LongDeviceName
	}
	return specs
}

// simulatorExtra builds the simulator options from the flags.
func simulatorExtra(scale float64, bind, fwVersion string, allowDownload []string) simulator.Extra {
	return simulator.Extra{TimeScale: scale, BindAddr: bind, FlashVersion: fwVersion, AllowDownloadHosts: allowDownload}
}

func run(ctx context.Context, args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("tasmofleet-devenv", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fixtures := fs.String("fixtures", "testdata/fixtures", "path to the fixtures directory")
	scale := fs.Float64("time-scale", 0.1, "multiplier for the OTA timeline (1 = real device speed)")
	tele := fs.Duration("tele-period", 10*time.Second, "TelePeriod of the simulated devices")
	fwVersion := fs.String("fw-version", "16.1.0", "version of the images the fake upstream serves")
	bind := fs.String("bind", "127.0.0.1", "address all listeners bind to; anything else exposes the OTA download gadget (opt-in)")
	fleetSize := fs.Int("fleet-size", 0, "total number of simulated devices; 0 or less than the base fleet keeps the base fleet")
	releases := fs.Int("releases", 1, "number of releases the fake GitHub releases feed (<upstream>/github) offers")
	stability := fs.Bool("stability-shapes", false, "add the devices of the Stability card (#325): st-stable, st-bootloop (restarts on POST /device/{name}/restart?reason=...) and st-weakwifi")
	templateShapes := fs.Bool("template-shapes", false, "add the devices of the GPIO templates tab (#327): tpl-nous-1..3 + tpl-nous-dev (one pin differs), tpl-minimal, tpl-offline, a tie group tpl-tie-1..4, tpl-builtin (built-in module) and an ESP32 group tpl-esp32-1..3")
	mqttTransport := fs.Bool("mqtt-transport", false, "add the devices TasmoFleet reaches only over MQTT (http_unreachable, fw_unreachable, MqttPassword "+MQTTPassword+")")
	onlyMQTT := fs.Bool("only-mqtt-transport", false, "only the devices of -mqtt-transport, no base fleet: for the specs that need nothing else and should not pay for it")
	overview := fs.Bool("overview-shapes", false, "add the device shapes of the Overview mockups (docs/design/device-overview): meters, several relays, Ethernet, sensors only, HTTP only")
	designFile := fs.String("design-fixture", "", "JSON file (testdata/design/v2-fixture.json) with the prototype's 60 devices and releases; replaces the mixed fleet, -fleet-size and the release list, and enables POST /fixture/offline and /fixture/feed/{down,dl-down,slow,up} on the control server")
	var allowDownload multiFlag
	fs.Var(&allowDownload, "allow-download-host", "host (as written in the OtaUrl, repeatable) the simulated devices may download firmware from besides loopback; needed when -bind is not loopback and the firmware server is elsewhere, for example 172.30.32.1 for a Home Assistant app on the same host")
	var redirects multiFlag
	fs.Var(&redirects, "oidc-redirect", "redirect URI registered at the mock IdP (repeatable); enables the mock IdP")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(errOut, "Usage: tasmofleet-devenv [flags]\n\nStarts an embedded MQTT broker, a fleet of simulated Tasmota devices and a fake\nfirmware upstream, prints the environment for tasmofleet and runs until SIGINT/SIGTERM.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	var design *designFixture
	if *designFile != "" {
		var err error
		if design, err = loadDesignFixture(*designFile); err != nil {
			return err
		}
		set := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
		if !set["fw-version"] {
			*fwVersion = design.Latest
		}
		if !set["releases"] {
			*releases = len(design.Releases)
		}
	}
	if *bind != "127.0.0.1" {
		slog.Warn("devenv: listeners bind to a non-loopback address; simulated devices will download from any URL they are given", "addr", *bind)
	}

	broker, err := simulator.StartBroker()
	if err != nil {
		return err
	}
	defer func() { _ = broker.Close() }()
	fleet, err := simulator.NewFleetWith(simulator.FleetOptions{FixturesDir: *fixtures, MQTTURL: broker.URL(), TelePeriod: *tele},
		simulatorExtra(*scale, *bind, *fwVersion, allowDownload))
	if err != nil {
		return err
	}
	defer func() { _ = fleet.Close() }()

	older := olderReleases
	if design != nil {
		older = design.Releases[1:]
	}
	feed := &upstreamFeed{design: design}
	upstream, err := startUpstream(*fixtures, *fwVersion, *bind, *releases, older, feed)
	if err != nil {
		return err
	}
	defer func() { _ = upstream.Close() }()

	_, _ = fmt.Fprintf(out, "TASMOFLEET_MQTT_URL=%s\n", broker.URL())
	_, _ = fmt.Fprintf(out, "TASMOFLEET_DEVICE_NETWORKS=127.0.0.0/8\n")
	shim, cleanup, err := writeDecoderShim()
	if err != nil {
		return err
	}
	defer cleanup()
	_, _ = fmt.Fprintf(out, "DEVENV_DECODER_PATH=%s\n", shim)
	_, _ = fmt.Fprintf(out, "DEVENV_UPSTREAM_URL=http://%s\n", upstream.Addr)
	_, _ = fmt.Fprintf(out, "DEVENV_UPSTREAM_GITHUB_URL=http://%s/github\n", upstream.Addr)
	_, _ = fmt.Fprintf(out, "DEVENV_FW_VERSION=%s\n", *fwVersion)
	var idp *oidcmock.Server
	if len(redirects) > 0 {
		var err error
		if idp, err = oidcmock.Start(oidcmock.Options{RedirectURIs: redirects}); err != nil {
			return err
		}
		defer idp.Close()
		_, _ = fmt.Fprintf(out, "DEVENV_OIDC_ISSUER=%s\n", idp.Issuer())
		_, _ = fmt.Fprintf(out, "DEVENV_OIDC_CLIENT_ID=tasmofleet\nDEVENV_OIDC_CLIENT_SECRET=mock-secret\n")
	}
	reg := &deviceRegistry{byName: map[string]simulator.Device{}}
	var df *designFleet
	if design != nil {
		var err error
		if df, err = startDesignFleet(ctx, fleet, design, func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }); err != nil {
			return err
		}
	}
	{
		ctl, err := startControl(idp, df, feed, reg, *bind)
		if err != nil {
			return err
		}
		defer func() { _ = ctl.Close() }()
		_, _ = fmt.Fprintf(out, "DEVENV_CONTROL_URL=http://%s\n", ctl.Addr)
	}
	specs := fleetOfSize(*fleetSize)
	if *overview {
		specs = append(specs, overviewDevices()...)
	}
	if *stability {
		specs = append(specs, stabilityDevices()...)
	}
	if *templateShapes {
		specs = append(specs, templateDevices()...)
	}
	if *mqttTransport {
		specs = append(specs, mqttTransportDevices()...)
	}
	if *onlyMQTT {
		specs = mqttTransportDevices()
	}
	if design != nil {
		specs = nil // the design fixture above is the whole fleet
	}
	for _, spec := range specs {
		d, err := fleet.Add(ctx, spec)
		if err != nil {
			return err
		}
		reg.add(spec.Name, d)
		mqtt := "mqtt"
		for _, f := range spec.Faults {
			if f.Kind == simulator.FaultHTTPOnly {
				mqtt = "http-only"
			}
		}
		_, _ = fmt.Fprintf(out, "DEVENV_DEVICE name=%s addr=%s topic=%s profile=%s version=%s %s\n",
			spec.Name, d.Addr(), d.Topic(), spec.Profile, d.Version(), mqtt)
	}
	_, _ = fmt.Fprintln(out, "DEVENV_READY=1")
	<-ctx.Done()
	return nil
}

// upstreamFeed is the state of the fake GitHub feed: the design fixture's release notes, and
// the switch of the control server that makes the feed fail.
type upstreamFeed struct {
	design *designFixture
	down   atomic.Bool  // the release list answers 503
	dlDown atomic.Bool  // image downloads answer 503
	slowTo atomic.Int64 // image downloads wait until this time (unix nanoseconds)
}

// slowDownloads is how long "slow" holds image downloads back: long enough to see a download
// in progress, short enough that nothing is left hanging.
const slowDownloads = 20 * time.Second

// startUpstream serves synthetic release images by file name from any path,
// like ota.tasmota.com, so tasmofleet's upstream source can point at it.
func startUpstream(fixtures, version, bind string, releases int, older []string, feed *upstreamFeed) (*http.Server, error) {
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", net.JoinHostPort(bind, "0"))
	if err != nil {
		return nil, err
	}
	srv := &http.Server{
		Addr: l.Addr().String(), ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/github/releases" {
				if feed.down.Load() {
					http.Error(w, "feed switched off by the control server", http.StatusServiceUnavailable)
					return
				}
				serveGitHubReleases(w, r, fixtures, version, releases, older, feed.design)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/release/") {
				serveListing(w, r, fixtures, version)
				return
			}
			if feed.dlDown.Load() {
				http.Error(w, "downloads switched off by the control server", http.StatusServiceUnavailable)
				return
			}
			if wait := time.Until(time.Unix(0, feed.slowTo.Load())); wait > 0 {
				select {
				case <-time.After(wait):
				case <-r.Context().Done():
					return
				}
			}
			imgVersion := version
			if v, ok := strings.CutPrefix(r.URL.Path, "/github/dl/"); ok {
				imgVersion = path.Dir(v) // /github/dl/<release>/<file>: the image carries its release
			}
			img, err := simulator.NewImage(fixtures, path.Base(r.URL.Path), imgVersion)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			if img, err = realisticSize(img, path.Base(r.URL.Path)); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(img) //nolint:gosec // binary firmware stand-in, not HTML
		}),
	}
	go func() { _ = srv.Serve(l) }() // Serve returns on Close; nothing to report
	return srv, nil
}

// startControl serves POST /login-as/{operator|admin|nobody}: the next
// authorization request at the mock IdP logs in as that user. E2E tests call
// it before clicking "sign in". With a design fleet it also serves
// POST /fixture/offline, which closes the fixture's offline devices once the
// app has seen them online. Either part is absent when its dependency is nil.
func startControl(idp *oidcmock.Server, df *designFleet, feed *upstreamFeed, reg *deviceRegistry, bind string) (*http.Server, error) {
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", net.JoinHostPort(bind, "0"))
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	if idp != nil {
		mux.HandleFunc("POST /login-as/{who}", func(w http.ResponseWriter, r *http.Request) {
			u, ok := loginUsers[r.PathValue("who")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			idp.NextLogin(u)
			w.WriteHeader(http.StatusNoContent)
		})
	}
	// The plain fleet's devices by name: tests pull a plug or press a button, and the app has to
	// notice (the device page follows both live, #126).
	mux.HandleFunc("POST /device/{name}/{action}", reg.act)
	mux.HandleFunc("POST /device/{name}/relay/{n}/{state}", reg.relay)
	mux.HandleFunc("POST /device/{name}/restart", reg.restart)
	mux.HandleFunc("POST /device/{name}/wifi", reg.wifi)
	if df != nil {
		mux.HandleFunc("POST /fixture/offline", func(w http.ResponseWriter, _ *http.Request) {
			df.GoOffline()
			w.WriteHeader(http.StatusNoContent)
		})
		// The firmware page's failure states: the fake GitHub feed's release list answers 503
		// (down), image downloads answer 503 (dl-down) or are held back for a while (slow);
		// up ends all of them.
		mux.HandleFunc("POST /fixture/feed/{state}", func(w http.ResponseWriter, r *http.Request) {
			switch r.PathValue("state") {
			case "down":
				feed.down.Store(true)
			case "dl-down":
				feed.dlDown.Store(true)
			case "slow":
				feed.slowTo.Store(time.Now().Add(slowDownloads).UnixNano())
			case "up":
				feed.down.Store(false)
				feed.dlDown.Store(false)
				feed.slowTo.Store(0)
			default:
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
	}
	srv := &http.Server{Addr: l.Addr().String(), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(l) }() // Serve returns on Close; nothing to report
	return srv, nil
}

// serveListing answers the directory pages ota.tasmota.com serves
// (/tasmota/release/, /tasmota32/release/): a heading with the version and
// one link per image. tasmofleet reaches this server through HTTP_PROXY, so
// the real source definition (and its host allowlist) is what the e2e tests.
func serveListing(w http.ResponseWriter, r *http.Request, fixtures, version string) {
	entries, err := os.ReadDir(path.Join(fixtures, "firmware-headers"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	esp32 := strings.HasPrefix(r.URL.Path, "/tasmota32/")
	var b strings.Builder
	fmt.Fprintf(&b, "<html><head><title>Index of %s</title></head><body><h1>Tasmota %s release</h1><pre>\n", r.URL.Path, version)
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".hdr")
		if !ok || strings.HasPrefix(name, "tasmota32") != esp32 {
			continue
		}
		fmt.Fprintf(&b, "<a href=\"%s\">%s</a>\n", name, name)
	}
	b.WriteString("</pre></body></html>\n")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(b.String())) //nolint:gosec // static listing built from file names
}

// olderReleases are what the fake GitHub feed offers below the newest release.
var olderReleases = []string{"16.0.0", "15.3.0", "15.2.0", "15.1.0", "15.0.1", "14.6.0", "14.5.0", "14.4.1", "14.3.0"}

// serveGitHubReleases answers GET <repo>/releases like api.github.com: the
// newest release is the devenv version, older ones follow. Each has long
// release notes and one asset per image, so the firmware page sees a realistic
// index. tasmofleet selects this feed when the override URL ends in /github.
func serveGitHubReleases(w http.ResponseWriter, r *http.Request, fixtures, version string, n int, older []string, design *designFixture) {
	entries, err := os.ReadDir(path.Join(fixtures, "firmware-headers"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type asset struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	}
	type release struct {
		Tag       string  `json:"tag_name"`
		Body      string  `json:"body"`
		HTMLURL   string  `json:"html_url"`
		Published string  `json:"published_at"`
		Assets    []asset `json:"assets"`
	}
	versions := append([]string{version}, older...)
	var out []release
	for i, v := range versions {
		if i >= n {
			break
		}
		rel := release{
			Tag: "v" + v, HTMLURL: "https://github.com/arendst/Tasmota/releases/tag/v" + v,
			Published: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).AddDate(0, -i, 0).Format(time.RFC3339),
			Body:      "## Changelog " + v + "\n\nBreaking change: the long flag name `SetOption146` now also covers `ThisIsAnUnusuallyLongCommandNameThatMustWrapOnAPhone` and https://example.test/a/very/long/link/that/has/no/natural/break/points/at/all/in/the/release/notes.\n\n- Added support for another sensor\n- Fixed a crash when the Wi-Fi drops while the rule engine runs\n",
		}
		if at, body, ok := design.feedRelease(v); ok {
			rel.Published, rel.Body = at.Format(time.RFC3339), body
		}
		for _, e := range entries {
			if name, ok := strings.CutSuffix(e.Name(), ".hdr"); ok {
				rel.Assets = append(rel.Assets, asset{Name: name, URL: fmt.Sprintf("http://%s/github/dl/%s/%s", r.Host, v, name)})
			}
		}
		out = append(out, rel)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out) // a failed write means the client left
}

// fullImagePad makes the regular ESP8266 image about as large as a real
// tasmota.bin.gz (about 650 KB), so it does not fit next to the running
// firmware on a 1 MB device and the plan becomes "staged" there, while 4 MB
// devices still take it directly. The filler is incompressible and deterministic.
const fullImagePad = 640 << 10

func realisticSize(img []byte, name string) ([]byte, error) {
	if !strings.HasSuffix(name, ".bin.gz") || strings.Contains(name, "minimal") {
		return img, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(img))
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic test filler, not security
	for range fullImagePad / 8 {
		raw = binary.LittleEndian.AppendUint64(raw, rng.Uint64())
	}
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// fakeDecoderArg switches the devenv binary into a stand-in for decode-config
// (the shim script calls it), so e2e runs can decode simulator dumps without
// the real binary. Real dumps are not accepted.
const fakeDecoderArg = "__decode-config"

func fakeDecoder(args []string, out, errOut io.Writer) int {
	var source string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--version":
			_, _ = fmt.Fprintln(out, simulator.FakeDecoderVersion)
			return 0
		case "--source", "-s":
			if i+1 < len(args) {
				source = args[i+1]
				i++
			}
		}
	}
	if source == "" {
		_, _ = fmt.Fprintln(errOut, "fake decode-config: --source required")
		return 2
	}
	dump, err := os.ReadFile(source) //nolint:gosec // path given by the caller, this is a test tool
	if err != nil {
		_, _ = fmt.Fprintln(errOut, "fake decode-config:", err)
		return 1
	}
	doc, err := simulator.DecodeDump(dump)
	if err != nil {
		_, _ = fmt.Fprintln(errOut, "fake decode-config:", err)
		return 1
	}
	_, _ = out.Write(doc)
	return 0
}

// writeDecoderShim creates an executable "decode-config" script that runs
// this binary in fake-decoder mode.
func writeDecoderShim() (path string, cleanup func(), err error) {
	exe, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp("", "devenv-decoder-")
	if err != nil {
		return "", nil, err
	}
	path = dir + "/decode-config"
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(exe, "'", `'\''`) + "' " + fakeDecoderArg + ` "$@"` + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // an executable script is the point
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	return path, func() { _ = os.RemoveAll(dir) }, nil
}

// deviceRegistry holds the simulated devices of the plain fleet by name for the control server.
type deviceRegistry struct {
	mu     sync.Mutex
	byName map[string]simulator.Device
}

func (r *deviceRegistry) add(name string, d simulator.Device) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byName[name] = d
}

func (r *deviceRegistry) control(w http.ResponseWriter, req *http.Request) (simulator.Controller, bool) {
	r.mu.Lock()
	d, ok := r.byName[req.PathValue("name")]
	r.mu.Unlock()
	if ok {
		if c, isCtl := simulator.Control(d); isCtl {
			return c, true
		}
	}
	http.NotFound(w, req)
	return nil, false
}

// act is POST /device/{name}/off (the plug is pulled: LWT Offline, HTTP closed for good).
func (r *deviceRegistry) act(w http.ResponseWriter, req *http.Request) {
	c, ok := r.control(w, req)
	if !ok {
		return
	}
	if req.PathValue("action") != "off" {
		http.NotFound(w, req)
		return
	}
	c.PowerOff()
	w.WriteHeader(http.StatusNoContent)
}

// relay is POST /device/{name}/relay/{n}/{on|off}: as if someone pressed the button.
func (r *deviceRegistry) relay(w http.ResponseWriter, req *http.Request) {
	c, ok := r.control(w, req)
	if !ok {
		return
	}
	n, err := strconv.Atoi(req.PathValue("n"))
	state := req.PathValue("state")
	if err != nil || n < 1 || (state != "on" && state != "off") {
		http.NotFound(w, req)
		return
	}
	c.SetRelay(n, state == "on")
	w.WriteHeader(http.StatusNoContent)
}

// restart is POST /device/{name}/restart?reason=Exception: the device reboots and reports that
// RestartReason (the default of a soft restart when empty).
func (r *deviceRegistry) restart(w http.ResponseWriter, req *http.Request) {
	c, ok := r.control(w, req)
	if !ok {
		return
	}
	c.Restart(req.URL.Query().Get("reason"))
	w.WriteHeader(http.StatusNoContent)
}

// wifi is POST /device/{name}/wifi?signal=-84&links=14: the link gets worse from now on.
func (r *deviceRegistry) wifi(w http.ResponseWriter, req *http.Request) {
	c, ok := r.control(w, req)
	if !ok {
		return
	}
	q := req.URL.Query()
	sig, err1 := strconv.Atoi(q.Get("signal"))
	links, err2 := strconv.Atoi(q.Get("links"))
	if q.Get("signal") != "" && err1 != nil || q.Get("links") != "" && err2 != nil {
		http.Error(w, "signal and links are whole numbers", http.StatusBadRequest)
		return
	}
	c.SetWifi(sig, links)
	w.WriteHeader(http.StatusNoContent)
}
