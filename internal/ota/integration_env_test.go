//go:build integration

package ota

// Integration environment: the real engine, SQLite store, firmware cache and
// firmware server, fwselect and tasmota client against simulated devices
// (internal/simulator). Only the clock is special: virtual time runs
// 1/timeScale times faster than real time, and the simulator's own timeline
// is scaled by the same factor, so a nominal 8 minute reboot window costs a
// few seconds.

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/devicecmd"
	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/secretbox"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

const (
	timeScale   = 0.03 // real seconds per nominal second, for the engine clock and the simulator
	itRelease   = "16.0.0"
	itFixtures  = "../../testdata/fixtures"
	bigImageLen = 400 * 1024 // above the 352 KB free flash of the 1 MB captures: plan staged
)

// scaledClock runs virtual time faster than real time.
type scaledClock struct {
	start, realStart time.Time
	scale            float64
}

func newScaledClock() *scaledClock {
	now := time.Now()
	return &scaledClock{start: now, realStart: now, scale: timeScale}
}

func (c *scaledClock) Now() time.Time {
	return c.start.Add(time.Duration(float64(time.Since(c.realStart)) / c.scale))
}

func (c *scaledClock) After(d time.Duration) <-chan time.Time {
	return time.After(time.Duration(float64(d) * c.scale))
}

// jobLog is the Notifier: every persisted transition, in order.
type jobLog struct {
	mu   sync.Mutex
	evs  []model.JobEvent
	ch   chan struct{}
	hook func(model.Job, model.JobEvent) // called after the event is recorded, inside the engine
}

func newJobLog() *jobLog { return &jobLog{ch: make(chan struct{}, 1)} }

func (l *jobLog) JobChanged(j model.Job, ev model.JobEvent) {
	l.mu.Lock()
	l.evs = append(l.evs, ev)
	hook := l.hook
	l.mu.Unlock()
	select {
	case l.ch <- struct{}{}:
	default:
	}
	if hook != nil {
		hook(j, ev)
	}
}

func (l *jobLog) entered(id model.JobID, s model.JobState) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, e := range l.evs {
		if e.JobID == id && e.To == s && e.From != s {
			n++
		}
	}
	return n
}

// guardedAPI wraps the production DeviceAPI. It records a violation when
// Upgrade is sent while the simulated device is in the middle of an OTA
// (the crash this engine exists to prevent) and counts Upgrades per device.
type guardedAPI struct {
	DeviceAPI
	env *itEnv
}

func (g *guardedAPI) Upgrade(ctx context.Context, d model.Device) (string, error) {
	if c := g.env.ctrl[d.ID]; c != nil {
		switch p := c.Phase(); p {
		case simulator.PhasePending, simulator.PhaseDownloading, simulator.PhaseRestartWait:
			g.env.violation(fmt.Sprintf("Upgrade sent to %s while it is %s", d.Name, p))
		case simulator.PhaseRun, simulator.PhaseRebooting:
		}
	}
	if f := g.env.beforeUpgrade; f != nil {
		f(d.ID)
	}
	return g.DeviceAPI.Upgrade(ctx, d)
}

// UploadFirmware passes the push upload through: the wrapper embeds the
// DeviceAPI interface, which would otherwise hide the optional capability.
func (g *guardedAPI) UploadFirmware(ctx context.Context, d model.Device, name string, size int64, body io.Reader) error {
	up, ok := g.DeviceAPI.(FirmwareUploader)
	if !ok {
		return errors.New("test: the wrapped DeviceAPI cannot upload")
	}
	return up.UploadFirmware(ctx, d, name, size, body)
}

// UpLayout and SwitchBoot pass the safeboot capability through (see UploadFirmware).
func (g *guardedAPI) UpLayout(ctx context.Context, d model.Device) (tasmota.UpLayout, error) {
	sb, ok := g.DeviceAPI.(SafebootAPI)
	if !ok {
		return "", errors.New("test: the wrapped DeviceAPI cannot read the layout")
	}
	return sb.UpLayout(ctx, d)
}

func (g *guardedAPI) SwitchBoot(ctx context.Context, d model.Device, to tasmota.BootTarget) (tasmota.BootReply, error) {
	sb, ok := g.DeviceAPI.(SafebootAPI)
	if !ok {
		return "", errors.New("test: the wrapped DeviceAPI cannot switch")
	}
	return sb.SwitchBoot(ctx, d, to)
}

type noBackups struct {
	n    atomic.Int32
	hook func() // runs inside Backup
}

func (b *noBackups) Backup(context.Context, string, model.DeviceID) ([]model.Backup, error) {
	b.n.Add(1)
	if b.hook != nil {
		b.hook()
	}
	return nil, nil
}

type itEnv struct {
	t       *testing.T
	fleet   simulator.Fleet
	st      *store.SQLite
	cache   firmware.Cache
	clk     *scaledClock
	api     DeviceAPI
	backups *noBackups
	log     *jobLog
	ctrl    map[model.DeviceID]simulator.Controller
	n       int

	mu         sync.Mutex
	violations []string
	bigImages  atomic.Bool // upstream serves images larger than the 1 MB devices' free flash
	upstream   *httptest.Server
	fwSrv      *httptest.Server
	fwHandler  atomic.Value // http.Handler
	machine    Config
	power      devicecmd.Service // the real power service, as the app wires it
	audits     *auditLog
	override   sync.Map // upstream file name -> []byte served instead of the generated image
	// beforeUpgrade runs right before the Upgrade command is sent.
	beforeUpgrade func(model.DeviceID)
	keepBackup    bool
	safebootPush  bool         // open the gate of the push into safeboot (#30) for the engines of this test
	runCancel     atomic.Value // context.CancelFunc of the running engine
	paths         UpdatePaths  // the path chooser of the engines of this test (nil: HTTP for every update)
	engine        atomic.Pointer[Engine]

	// engine lifecycle
	cancel context.CancelFunc
	done   chan struct{}
}

func (e *itEnv) violation(s string) {
	e.mu.Lock()
	e.violations = append(e.violations, s)
	e.mu.Unlock()
}

// padded returns the image as upstream serves it: for the non-minimal ESP8266
// images optionally padded with incompressible bytes so the plan is staged.
func padded(t *testing.T, name string, big bool) []byte {
	t.Helper()
	img, err := simulator.NewImage(itFixtures, name, itRelease)
	if err != nil {
		t.Fatal(err)
	}
	if !big || !strings.HasSuffix(name, ".bin.gz") || strings.Contains(name, "minimal") {
		return img
	}
	zr, err := gzip.NewReader(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	filler := make([]byte, bigImageLen)
	rand.New(rand.NewSource(1)).Read(filler) //nolint:gosec // deterministic test filler
	raw = append(raw, filler...)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var upstreamFiles = map[string][]string{
	"/tasmota/release/":   {"tasmota.bin.gz", "tasmota-minimal.bin.gz", "tasmota-lite.bin.gz", "tasmota-DE.bin.gz"},
	"/tasmota32/release/": {"tasmota32.bin", "tasmota32c3.bin", "tasmota32s3.bin", "tasmota32s2.bin", "tasmota32s2cdc.bin", "tasmota32-safeboot.bin"},
}

func (e *itEnv) serveUpstream(w http.ResponseWriter, r *http.Request) {
	if names, ok := upstreamFiles[r.URL.Path]; ok {
		var b strings.Builder
		b.WriteString("<html><head><title>Index of " + r.URL.Path + "</title></head><body><h1>Release notes: v" + itRelease + "</h1>\n")
		for _, n := range names {
			fmt.Fprintf(&b, "<a href=%q>%s</a>\n", n, n)
		}
		_, _ = io.WriteString(w, b.String())
		return
	}
	for dir, names := range upstreamFiles {
		for _, n := range names {
			if r.URL.Path == dir+n {
				body := padded(e.t, n, e.bigImages.Load())
				if o, ok := e.override.Load(n); ok {
					body = o.([]byte)
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				_, _ = w.Write(body)
				return
			}
		}
	}
	http.NotFound(w, r)
}

// itSlots bounds the scenarios running at the same time: they mostly sleep,
// but each starts an HTTP stack and a fleet.
var itSlots = make(chan struct{}, 4)

// newItEnv builds the whole stack. Big images are on by default (the 1 MB
// devices are staged); a test can switch them off before the first Preview.
func newItEnv(t *testing.T) *itEnv { return newItEnvMQTT(t, "") }

// newItEnvMQTT is newItEnv with the simulated devices attached to the
// broker at mqttURL (empty = HTTP only).
func newItEnvMQTT(t *testing.T, mqttURL string) *itEnv {
	t.Helper()
	t.Parallel()
	itSlots <- struct{}{}
	t.Cleanup(func() { <-itSlots })
	e := &itEnv{t: t, clk: newScaledClock(), log: newJobLog(), backups: &noBackups{},
		ctrl: map[model.DeviceID]simulator.Controller{}, machine: DefaultConfig()}
	e.bigImages.Store(true)
	e.safebootPush = safebootPushEnabled
	dir := t.TempDir()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if e.st, err = store.Open(filepath.Join(dir, "tasmofleet.db"), box); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.st.Close() })

	e.upstream = httptest.NewServer(http.HandlerFunc(e.serveUpstream))
	t.Cleanup(e.upstream.Close)
	e.fwSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.fwHandler.Load().(http.Handler).ServeHTTP(w, r)
	}))
	t.Cleanup(e.fwSrv.Close)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	e.cache, err = firmware.New(firmware.Options{
		Dir:     filepath.Join(dir, "firmware"),
		BaseURL: e.fwSrv.URL,
		Sources: []firmware.Source{{Name: "ota.tasmota.com", Kind: "ota-tasmota", BaseURL: e.upstream.URL,
			AllowedHosts: []string{strings.TrimPrefix(e.upstream.URL, "http://")}, AllowHTTP: true}},
		Inspect: func(h []byte) (model.ChipFamily, error) {
			info, err := fwselect.InspectImage(h)
			return info.Family, err
		},
		Logger: quiet,
	}, e.st)
	if err != nil {
		t.Fatal(err)
	}
	e.fwHandler.Store(http.HandlerFunc(firmware.Server(e.cache).ServeHTTP)) // one concrete type, so tests can swap in wrappers

	e.fleet, err = simulator.NewFleetWith(simulator.FleetOptions{FixturesDir: itFixtures, MQTTURL: mqttURL, TelePeriod: time.Hour}, simulator.Extra{TimeScale: timeScale})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.fleet.Close() })

	client := tasmota.NewHTTPClient(tasmota.Options{
		AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		Timeout:         1500 * time.Millisecond, // a device that is downloading hangs; do not wait the nominal 10 s
		Logger:          quiet,
	})
	e.audits = &auditLog{}
	e.power = devicecmd.New(devicecmd.Options{Devices: storeDevices{e.st}, Client: client, Audit: e.audits})
	api := NewDeviceAPI(client)
	api.(*deviceAPI).gap = 5 * time.Millisecond
	e.api = &guardedAPI{DeviceAPI: api, env: e}
	t.Cleanup(e.stop)
	return e
}

// addDevice puts a simulated device into the simulator and the inventory.
func (e *itEnv) addDevice(spec simulator.DeviceSpec) model.DeviceID {
	e.t.Helper()
	e.n++
	if spec.Version == "" {
		spec.Version = "15.6.0"
	}
	if spec.Name == "" {
		spec.Name = fmt.Sprintf("dev%d", e.n)
	}
	d, err := e.fleet.Add(context.Background(), spec)
	if err != nil {
		e.t.Fatal(err)
	}
	c, ok := simulator.Control(d)
	if !ok {
		e.t.Fatal("not a controller")
	}
	host, portS, _ := strings.Cut(d.Addr(), ":")
	port, _ := strconv.Atoi(portS)
	id, err := e.st.CreateDevice(context.Background(), model.Device{
		Name: spec.Name, Topic: d.Topic(), Address: host, Port: port, MAC: c.MAC(),
		HTTPUser: spec.User, HTTPPassword: spec.Password,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	e.ctrl[id] = c
	return id
}

func (e *itEnv) newEngine(maxParallel int) *Engine {
	eng := NewEngine(Options{Machine: e.machine, MaxParallel: maxParallel, Power: e.power, Paths: e.paths}, e.st, e.st, e.api, e.cache, e.backups, e.clk, e.log)
	e.engine.Store(eng)
	eng.safebootPush = e.safebootPush // the gate of the push into safeboot, set by the test before start
	return eng
}

// start runs a fresh engine (a "process start"). Any running one is stopped first.
func (e *itEnv) start(maxParallel int) *Engine {
	e.stop()
	eng := e.newEngine(maxParallel)
	e.runEngine(eng)
	return eng
}

// runEngine runs eng like a process start; start is newEngine plus runEngine.
func (e *itEnv) runEngine(eng *Engine) {
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel, e.done = cancel, make(chan struct{})
	e.runCancel.Store(cancel)
	done := e.done
	go func() {
		defer close(done)
		if err := eng.Run(ctx); err != nil {
			e.t.Errorf("Run: %v", err)
		}
	}()
}

// stop ends the running engine like a process exit: workers stop where they are.
func (e *itEnv) stop() {
	if e.cancel == nil {
		return
	}
	e.cancel()
	<-e.done
	e.cancel = nil
}

func (e *itEnv) submit(eng *Engine, reqs ...Request) model.BatchID {
	e.t.Helper()
	for i := range reqs {
		if reqs[i].Release == "" {
			reqs[i].Release = itRelease
		}
		reqs[i].SkipBackup = !e.keepBackup // the real backup service belongs to WP8b
	}
	b, err := eng.Submit(context.Background(), "it", reqs)
	if err != nil {
		e.t.Fatalf("Submit: %v", err)
	}
	return b
}

func (e *itEnv) jobOf(dev model.DeviceID) model.Job {
	e.t.Helper()
	js, err := e.st.ListJobs(context.Background(), store.JobFilter{DeviceID: dev})
	if err != nil || len(js) == 0 {
		e.t.Fatalf("no job for device %d: %v", dev, err)
	}
	return js[0] // ListJobs is newest first
}

// waitTerminal waits (real time) until the device's job is terminal.
func (e *itEnv) waitTerminal(dev model.DeviceID) model.Job {
	e.t.Helper()
	return e.waitJob(dev, func(j model.Job) bool { return j.State.Terminal() }, "terminal")
}

func (e *itEnv) waitJob(dev model.DeviceID, cond func(model.Job) bool, what string) model.Job {
	e.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		js, _ := e.st.ListJobs(context.Background(), store.JobFilter{DeviceID: dev})
		if len(js) > 0 && cond(js[0]) { // newest first: a retry's job is the latest
			return js[0]
		}
		if time.Now().After(deadline) {
			var states []string
			e.log.mu.Lock()
			for _, ev := range e.log.evs {
				states = append(states, fmt.Sprintf("%d:%s>%s(%s %s)", ev.JobID, ev.From, ev.To, ev.Event, ev.Detail))
			}
			e.log.mu.Unlock()
			e.t.Fatalf("device %d: job never became %s; last %+v\nhistory: %v", dev, what, js, states)
		}
		select {
		case <-e.log.ch:
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (e *itEnv) history() string {
	e.log.mu.Lock()
	defer e.log.mu.Unlock()
	return e.historyLocked()
}

func (e *itEnv) historyLocked() string {
	var b strings.Builder
	for _, ev := range e.log.evs {
		fmt.Fprintf(&b, "\n  %s>%s (%s) %s", ev.From, ev.To, ev.Event, ev.Detail)
	}
	return b.String()
}

func (e *itEnv) upgrades(dev model.DeviceID) int {
	n := 0
	for _, c := range e.ctrl[dev].Commands() {
		if strings.EqualFold(strings.TrimSpace(c), "upgrade 1") {
			n++
		}
	}
	return n
}

func (e *itEnv) assertNoViolations() {
	e.t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.violations) > 0 {
		e.t.Fatalf("safety violations: %v", e.violations)
	}
}

func (e *itEnv) fwURL(name string) string {
	return e.fwSrv.URL + "/fw/" + itRelease + "/" + name
}

// settle waits until the simulated device is in its run phase again.
func (e *itEnv) settle(dev model.DeviceID) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.ctrl[dev].WaitFor(ctx, func(c simulator.Controller) bool { return c.Phase() == simulator.PhaseRun }); err != nil {
		e.t.Fatal(err)
	}
}

// crashAt makes the running engine die (context cancelled, like a killed
// process) right after the nth entry into state was persisted and before
// its actions run: the harshest crash point of a write-ahead machine.
func (e *itEnv) crashAt(state model.JobState, nth int) <-chan struct{} {
	crashed := make(chan struct{})
	var once sync.Once
	e.log.mu.Lock()
	e.log.hook = func(j model.Job, ev model.JobEvent) {
		if j.State == state && ev.From != state && e.log.entered(j.ID, state) == nth {
			once.Do(func() {
				e.runCancel.Load().(context.CancelFunc)()
				close(crashed)
			})
		}
	}
	e.log.mu.Unlock()
	return crashed
}

func (e *itEnv) clearHook() {
	e.log.mu.Lock()
	e.log.hook = nil
	e.log.mu.Unlock()
}

// peakBusy is the largest number of jobs that were past queued and not yet
// terminal at the same time, replayed from the persisted transitions.
func (l *jobLog) peakBusy() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	busy := map[model.JobID]bool{}
	peak := 0
	for _, ev := range l.evs {
		busy[ev.JobID] = !ev.To.Terminal() && ev.To != model.JobQueued
		n := 0
		for _, b := range busy {
			if b {
				n++
			}
		}
		if n > peak {
			peak = n
		}
	}
	return peak
}

// storeDevices adapts the store to devicecmd.DeviceGetter.
type storeDevices struct{ st *store.SQLite }

func (s storeDevices) Get(ctx context.Context, id model.DeviceID) (model.Device, error) {
	return s.st.GetDevice(ctx, id)
}

// auditLog records audit events in memory.
type auditLog struct {
	mu  sync.Mutex
	evs []model.AuditEvent
}

func (a *auditLog) Record(_ context.Context, ev model.AuditEvent) {
	a.mu.Lock()
	a.evs = append(a.evs, ev)
	a.mu.Unlock()
}

// powerAudits returns the recorded power switchings of a device.
func (a *auditLog) powerAudits(dev model.DeviceID) []model.AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []model.AuditEvent
	for _, ev := range a.evs {
		if ev.Action == "device.power" && ev.Target == fmt.Sprintf("device:%d", dev) {
			out = append(out, ev)
		}
	}
	return out
}
