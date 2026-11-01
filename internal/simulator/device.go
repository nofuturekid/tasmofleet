package simulator

import (
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Phase is where a simulated device is in the OTA/reboot timeline.
type Phase string

// Phases, in the order of a normal upgrade.
const (
	PhaseRun         Phase = "run"          // fully responsive (normal or minimal image)
	PhasePending     Phase = "pending"      // Upgrade accepted, ota_state_flag counting down; still responsive
	PhaseDownloading Phase = "downloading"  // blocking download: HTTP accepts and hangs, MQTT silent
	PhaseRestartWait Phase = "restart_wait" // download done, waiting before the restart; still blocked
	PhaseRebooting   Phase = "rebooting"    // HTTP refused (or hangs), MQTT disconnected cleanly
)

// PhaseChange is one entry of a device's timeline.
type PhaseChange struct {
	Phase Phase
	At    time.Time
}

// settings are the values that survive a reboot (flash) or not (RAM).
type settings struct {
	otaURL    string
	bootCount int
	serialLog int // -1 = never changed, replay the capture
	// kv holds every setting changed by a command, by lower-case key
	// ("teleperiod", "setoption3", ...). Unchanged settings replay the capture.
	kv map[string]string
	// tplJSON is the stored user template as JSON (tplState), "" = as captured (template.go).
	tplJSON string
}

func (s settings) clone() settings {
	c := s
	c.kv = make(map[string]string, len(s.kv))
	for k, v := range s.kv {
		c.kv[k] = v
	}
	return c
}

// simDevice is one running simulated device. All fields below mu are
// guarded by it; timeline goroutines take the lock only for short sections.
type simDevice struct {
	f    *fleet
	spec DeviceSpec
	cap  *capture
	def  profileDef

	topic, mac, host string
	esp32            bool
	chipFamily       string // family the bootloader accepts, "" = none
	hardware         string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once

	// wgMu orders wg.Add (spawn) against Close: once closing is set no new
	// goroutine starts, so wg.Add never runs concurrently with wg.Wait.
	wgMu    sync.Mutex
	closing bool

	mu            sync.Mutex
	phase         Phase
	phaseCh       chan struct{}
	history       []PhaseChange
	image         string // image name: tasmota, tasmota32, minimal, safeboot, lite ...
	version       string
	persisted     settings
	runtime       settings
	otaLoader     bool // RTC memory: survives the reboot into minimal
	bootAt        time.Time
	uptimeBase    int
	boots         int // reboots since creation
	mqttCount     int
	reason        string
	nextReason    string // the RestartReason the next boot reports instead of the default (Restart)
	relays        []bool // relay states; Tasmota keeps them in RTC RAM across a soft restart only
	cmds          []string
	faults        []Fault
	armed         []Fault // faults armed by the Upgrade that started the current OTA
	attempts      int     // download attempts of the latest OTA
	nextImage     *flashed
	restartQueued bool // a settings change already scheduled the restart
	// uploadType is Web.upload_file_type: GET /up, /rs and /ufsd set it, a finished /u2 upload
	// clears it. A POST the Referer check drops leaves it as it was.
	uploadType uploadKind
	// appImage and appVersion: the app that was running when /u4?u4=fct switched to safeboot, which
	// /u4?u4=ota boots again. safebootVersion is what the factory partition reports.
	appImage, appVersion string
	safebootVersion      string
	flapping             bool
	configDump           []byte
	httpAddr             string
	srv                  *httpServer
	mq                   *mqttLink
	mf                   mqttFile // MQTT FileDownload/FileUpload state (FMqtt)
	fs                   *simFS   // flash file system, nil = build without one
}

// flashed is a downloaded image waiting for the restart.
type flashed struct {
	kind    string
	version string
}

func (d *simDevice) minimalLocked() bool { return d.image == "minimal" || d.image == "safeboot" }

func newDevice(f *fleet, spec DeviceSpec, def profileDef, c *capture) (*simDevice, error) {
	d := &simDevice{f: f, spec: spec, cap: c, def: def, host: "127.0.0.1"}
	if spec.Sensors != nil { // the caller may keep editing its map while the device publishes
		d.spec.Sensors = make(map[string]map[string]float64, len(spec.Sensors))
		for k, v := range spec.Sensors {
			d.spec.Sensors[k] = maps.Clone(v)
		}
	}
	d.topic = topicOf(spec.Name)
	sum := sha256.Sum256([]byte(spec.Name))
	d.mac = fmt.Sprintf("02:00:5E:%02X:%02X:%02X", sum[0], sum[1], sum[2])
	d.esp32 = strings.HasPrefix(def.Chip, "esp32") || def.Chip == "unknown"
	if def.Chip != "unknown" {
		d.chipFamily = def.Chip
	}
	d.hardware = c.hardware
	d.version = spec.Version
	if d.version == "" {
		d.version = c.version
	}
	d.safebootVersion = d.version
	d.image = c.image
	if variant := strings.ToLower(spec.Variant); variant != "" && variant != "tasmota" && variant != "tasmota32" {
		d.image = variant
	}
	if def.Minimal {
		d.image = "minimal"
	}
	if d.image == "minimal" && d.esp32 {
		d.image = "safeboot"
	}
	d.faults = append(d.faults, spec.Faults...)
	d.persisted = settings{otaURL: capturedOtaURL(c), bootCount: capturedInt(c, "status-1.json", `"BootCount":(\d+)`), serialLog: -1, kv: map[string]string{}}
	if spec.SetOption0 != nil {
		d.persisted.kv["setoption0"] = map[bool]string{true: "1", false: "0"}[*spec.SetOption0]
	}
	for k, v := range spec.Settings {
		d.persisted.kv[strings.ToLower(k)] = v
	}
	for i, n := range spec.FriendlyNames {
		d.persisted.kv["friendlyname"+strconv.Itoa(i+1)] = n
	}
	if spec.MQTTPassword != "" {
		d.persisted.kv["mqttpassword"] = spec.MQTTPassword
	}
	if err := d.initTemplate(); err != nil {
		return nil, err
	}
	d.runtime = d.persisted.clone()
	d.uptimeBase = capturedInt(c, "state.json", `"UptimeSec":(\d+)`)
	d.bootAt = time.Now()
	d.mqttCount = 1
	d.reason = capturedString(c, "status-1.json", `"RestartReason":"([^"]*)"`)
	d.relays = make([]bool, max(spec.Relays, 1))
	d.relays[0] = strings.Contains(string(c.files["state.json"]), `"POWER":"ON"`)
	if spec.Power != nil {
		for i := range d.relays {
			d.relays[i] = i < len(spec.Power) && spec.Power[i]
		}
	}
	d.fs = newSimFS(d.spec, d.esp32, d.minimalLocked(), time.Now())
	d.phase = PhaseRun
	d.phaseCh = make(chan struct{})
	d.history = []PhaseChange{{PhaseRun, time.Now()}}
	d.ctx, d.cancel = context.WithCancel(context.Background())
	return d, nil
}

var reTopicBad = regexp.MustCompile(`[^a-z0-9_.-]+`)

func topicOf(name string) string {
	t := reTopicBad.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
	return strings.Trim(t, "-")
}

func capturedOtaURL(c *capture) string {
	m := regexp.MustCompile(`"OtaUrl":"([^"]*)"`).FindStringSubmatch(string(c.files["otaurl.json"]))
	if m == nil {
		return ""
	}
	return m[1]
}

func capturedInt(c *capture, file, re string) int {
	m := regexp.MustCompile(re).FindStringSubmatch(string(c.files[file]))
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

func capturedString(c *capture, file, re string) string {
	if m := regexp.MustCompile(re).FindStringSubmatch(string(c.files[file])); m != nil {
		return m[1]
	}
	return ""
}

// dur converts a nominal (real device) duration in seconds to wall time.
func (d *simDevice) dur(sec float64) time.Duration {
	return time.Duration(sec * float64(time.Second) * d.f.scale)
}

// sleep waits for a nominal duration; false when the device was closed.
func (d *simDevice) sleep(sec float64) bool { return d.sleepFor(d.dur(sec)) }

func (d *simDevice) sleepFor(w time.Duration) bool {
	t := time.NewTimer(w)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-d.ctx.Done():
		return false
	}
}

func (d *simDevice) setPhaseLocked(p Phase) {
	if d.phase == p {
		return
	}
	d.phase = p
	d.history = append(d.history, PhaseChange{p, time.Now()})
	close(d.phaseCh)
	d.phaseCh = make(chan struct{})
}

func (d *simDevice) setPhase(p Phase) {
	d.mu.Lock()
	d.setPhaseLocked(p)
	d.mu.Unlock()
}

func (d *simDevice) viewLocked() view {
	return view{
		now: time.Now(), startup: d.bootAt.Add(-time.Duration(d.uptimeBase) * time.Second),
		uptimeSec: d.uptimeBase + int(time.Since(d.bootAt).Seconds()),
		relays:    slices.Clone(d.relays), light: d.spec.Light, noRelay: d.spec.NoRelay, otaURL: d.runtime.otaURL, bootCount: d.runtime.bootCount,
		signalDBm: d.spec.SignalDBm, linkCount: d.spec.LinkCount,
		mqttCount: d.mqttCount, reason: d.reason, image: d.image, version: d.version, kv: cloneKV(d.runtime.kv),
	}
}

// commitLocked is Tasmota's SettingsSave: only the normal firmware writes
// flash. The minimal image never persists anything.
func (d *simDevice) commitLocked() {
	if !d.minimalLocked() {
		d.persisted = d.runtime.clone()
	}
}

// faultsFor returns the faults of the given kinds that apply to cmd.
func (d *simDevice) faultsForLocked(cmd string, kinds ...string) []Fault {
	var out []Fault
	for _, f := range d.faults {
		for _, k := range kinds {
			if f.Kind == k && f.matches(cmd) {
				out = append(out, f)
			}
		}
	}
	return out
}

func (f Fault) matches(cmd string) bool {
	if f.After == "" {
		return true
	}
	a, c := normCmd(f.After), normCmd(cmd)
	return c == a || strings.HasPrefix(c, a+" ")
}

func normCmd(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

func (d *simDevice) hasFault(kind string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.hasFaultLocked(kind)
}

func (d *simDevice) hasFaultLocked(kind string) bool {
	for _, f := range d.faults {
		if f.Kind == kind {
			return true
		}
	}
	return false
}

// listen binds the HTTP port (a fixed port on re-bind after a reboot).
func (d *simDevice) listen(addr string) (net.Listener, error) {
	var lc net.ListenConfig
	var err error
	for i := 0; i < 50; i++ {
		var l net.Listener
		l, err = lc.Listen(d.ctx, "tcp", addr)
		if err == nil {
			return l, nil
		}
		if !d.sleepFor(20 * time.Millisecond) {
			break
		}
	}
	return nil, err
}

type httpServer struct {
	srv *http.Server
	l   net.Listener
}

func cloneKV(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}
