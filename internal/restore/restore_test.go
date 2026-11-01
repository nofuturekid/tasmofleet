package restore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
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

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/devlock"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/secretbox"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// --- token download (handler) -------------------------------------------

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- c.Now().Add(d) // time does not pass by itself in the handler tests
	return ch
}
func (c *fakeClock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func handlerManager(t *testing.T, clk *fakeClock) *Manager {
	t.Helper()
	m, err := New(Options{
		Devices: nopDevices{}, Backups: nopBackups{}, Store: nopStore{}, Client: nopClient{},
		BaseURL: "http://fw.invalid:8081", Clock: clk, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func get(h http.Handler, path, remote string) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://fw"+path, nil)
	r.RemoteAddr = remote
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// Why: the token is the only authentication of the download. It must work
// exactly once, only from the device's own IP, and every refusal must look
// the same so the endpoint cannot be used to probe tokens or devices.
func TestTokenServesDumpOnceToDeviceOnly(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	m := handlerManager(t, clk)
	dump := bytes.Repeat([]byte{7}, 4096)
	tok, err := m.issue(Job{ID: 1, DeviceID: 5}, dump, []netip.Addr{netip.MustParseAddr("172.31.13.40")})
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 32 {
		t.Fatalf("token has %d hex chars, want 32 (128 bit)", len(tok))
	}
	h := m.Handler()

	wrong := get(h, "/restore/"+tok, "172.31.13.99:5000")
	if wrong.Code != 404 {
		t.Fatalf("wrong IP: %d", wrong.Code)
	}
	ok := get(h, "/restore/"+tok, "172.31.13.40:5000")
	if ok.Code != 200 || !bytes.Equal(ok.Body.Bytes(), dump) {
		t.Fatalf("device request after a wrong-IP probe: %d (a stranger must not burn the token)", ok.Code)
	}
	if cc := ok.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control %q", cc)
	}
	if ct := ok.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type %q", ct)
	}
	again := get(h, "/restore/"+tok, "172.31.13.40:5000")
	if again.Code != 404 {
		t.Errorf("second use: %d", again.Code)
	}
	unknown := get(h, "/restore/"+strings.Repeat("0", 32), "172.31.13.40:5000")
	if unknown.Body.String() != again.Body.String() || unknown.Body.String() != wrong.Body.String() || unknown.Code != 404 {
		t.Errorf("refusals differ: %q %q %q", unknown.Body.String(), again.Body.String(), wrong.Body.String())
	}
}

// Why: a token that was not used must not stay valid; five minutes is the
// documented window (spec 12.4).
func TestTokenExpiresAfterFiveMinutes(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	m := handlerManager(t, clk)
	tok, _ := m.issue(Job{ID: 1, DeviceID: 5}, []byte("x"), []netip.Addr{netip.MustParseAddr("172.31.13.40")})
	clk.Advance(5*time.Minute + time.Second)
	if w := get(m.Handler(), "/restore/"+tok, "172.31.13.40:1"); w.Code != 404 {
		t.Errorf("expired token served: %d", w.Code)
	}
}

// Why: only exact, canonical paths and GET are accepted; anything else could
// smuggle a token past the lookup (encoded characters, case, extra segments).
func TestHandlerRejectsMalformedRequests(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	m := handlerManager(t, clk)
	tok, _ := m.issue(Job{ID: 1, DeviceID: 5}, []byte("x"), []netip.Addr{netip.MustParseAddr("172.31.13.40")})
	h := m.Handler()
	for _, p := range []string{"/restore/" + tok + "/", "/restore/" + tok + "/x", "/restore/" + strings.ToUpper(tok) + "0", "/restore/", "/restore", "/restore/%2e%2e", "/restore/" + tok[:31]} {
		if w := get(h, p, "172.31.13.40:1"); w.Code != 404 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "http://fw/restore/"+tok, nil)
	r.RemoteAddr = "172.31.13.40:1"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Errorf("POST: %d", w.Code)
	}
	// None of the above consumed the token.
	if w := get(h, "/restore/"+tok, "172.31.13.40:1"); w.Code != 200 {
		t.Errorf("token was consumed by a malformed request: %d", w.Code)
	}
}

// Why: IPv4-mapped IPv6 peers (dual-stack listeners) are the same device.
func TestTokenMatchesMappedIPv4(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	m := handlerManager(t, clk)
	tok, _ := m.issue(Job{ID: 1, DeviceID: 5}, []byte("x"), []netip.Addr{netip.MustParseAddr("172.31.13.40")})
	if w := get(m.Handler(), "/restore/"+tok, "[::ffff:172.31.13.40]:1"); w.Code != 200 {
		t.Errorf("mapped address: %d", w.Code)
	}
}

type nopDevices struct{}

func (nopDevices) GetDevice(context.Context, model.DeviceID) (model.Device, error) {
	return model.Device{}, store.ErrNotFound
}

type nopBackups struct{}

func (nopBackups) Backup(context.Context, string, model.DeviceID) ([]model.Backup, error) {
	return nil, errors.New("nop")
}
func (nopBackups) BackupOf(context.Context, string, model.DeviceID, model.Device, string) ([]model.Backup, error) {
	return nil, errors.New("nop")
}
func (nopBackups) List(context.Context, model.DeviceID) ([]model.Backup, error) { return nil, nil }
func (nopBackups) Open(context.Context, model.BackupID) ([]byte, model.Backup, error) {
	return nil, model.Backup{}, store.ErrNotFound
}

type nopStore struct{ store.RestoreStore }

type nopClient struct{ tasmota.Client }

// --- full flow against the simulator --------------------------------------

// fakeDecoder maps dump bytes to decoded JSON so the verification diff can be
// steered from the test: byte 0 is a volatile checksum, byte 1 a real setting.
type fakeDecoder struct{}

func (fakeDecoder) Version(context.Context) (string, error) { return "16.0.0.0", nil }
func (fakeDecoder) Decode(_ context.Context, dmp []byte) ([]byte, error) {
	return []byte(fmt.Sprintf(`{"header":{"data":{"hardware":"ESP82xx"}},"cfg_crc":%d,"tele_period":%d,"mqtt_pwd":"secret-%d"}`, dmp[0], dmp[1], dmp[1])), nil
}

// esp32Decoder is fakeDecoder for the flows with ESP32 devices (the family check reads the header).
type esp32Decoder struct{}

func (esp32Decoder) Version(context.Context) (string, error) { return "16.0.0.0", nil }
func (esp32Decoder) Decode(_ context.Context, dmp []byte) ([]byte, error) {
	return []byte(fmt.Sprintf(`{"header":{"data":{"hardware":"ESP32"}},"cfg_crc":%d,"tele_period":%d,"mqtt_pwd":"secret-%d"}`, dmp[0], dmp[1], dmp[1])), nil
}

// hookClient lets a test observe and intercept device commands.
type hookClient struct {
	tasmota.Client
	mu         sync.Mutex
	cmds       []string
	status2    string // when set, the device claims this firmware version
	status5    string // when set, the device claims this MAC
	swallow    bool   // accept WebGetConfig without doing anything
	afterErr   error  // returned after WebGetConfig was really executed
	swallowErr error  // returned instead of executing WebGetConfig
	failDL     atomic.Bool

	// fs is the file system part of the real client. The wrapper exposes it (so the backup and the
	// restore see a client with file access, as in production) and lets a test break one upload.
	fs tasmota.FileSystem
	// order records the writes in the sequence they reached the device: "upload:/path" and the
	// commands that change settings (WebGetConfig).
	order []string
	// uploadHook runs before every UploadFile with the 1-based number of the attempt for this path;
	// a non-nil error is returned instead of the real upload (the hook may have written something
	// itself, the way a device does that deletes the old file and then fails).
	uploadHook func(p string, attempt int) error
	attempts   map[string]int
	// afterUpload runs after a real upload returned without error.
	afterUpload func(p string)
	// failCmd, when set, may fail a command before it reaches the device (return nil to let it pass).
	failCmd func(cmnd string) error
	// fsInfoErr, when set, makes FSInfo fail (the device's file system cannot be read).
	fsInfoErr error
}

func (c *hookClient) FSInfo(ctx context.Context, t tasmota.Target) (tasmota.FSInfo, error) {
	if c.fsInfoErr != nil {
		return tasmota.FSInfo{}, c.fsInfoErr
	}
	return c.fs.FSInfo(ctx, t)
}

func (c *hookClient) ListFiles(ctx context.Context, t tasmota.Target, dir string) ([]tasmota.FileEntry, error) {
	return c.fs.ListFiles(ctx, t, dir)
}

func (c *hookClient) DownloadFile(ctx context.Context, t tasmota.Target, p string, max int64) ([]byte, error) {
	return c.fs.DownloadFile(ctx, t, p, max)
}

func (c *hookClient) UploadFile(ctx context.Context, t tasmota.Target, p string, data []byte) error {
	c.mu.Lock()
	if c.attempts == nil {
		c.attempts = map[string]int{}
	}
	c.attempts[p]++
	n := c.attempts[p]
	c.order = append(c.order, "upload:"+p)
	hook := c.uploadHook
	c.mu.Unlock()
	if hook != nil {
		if err := hook(p, n); err != nil {
			return err
		}
	}
	err := c.fs.UploadFile(ctx, t, p, data)
	if err == nil && c.afterUpload != nil {
		c.afterUpload(p)
	}
	return err
}

// uploads lists the paths of the uploads that reached the client, in order.
func (c *hookClient) uploads() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, o := range c.order {
		if p, ok := strings.CutPrefix(o, "upload:"); ok {
			out = append(out, p)
		}
	}
	return out
}

func (c *hookClient) Command(ctx context.Context, t tasmota.Target, cmnd string) (json.RawMessage, error) {
	if c.failCmd != nil {
		if err := c.failCmd(cmnd); err != nil {
			return nil, err
		}
	}
	c.mu.Lock()
	c.cmds = append(c.cmds, cmnd)
	swallow := c.swallow
	c.mu.Unlock()
	c.mu.Lock()
	s2, s5 := c.status2, c.status5
	c.mu.Unlock()
	if cmnd == "Status 2" && s2 != "" {
		return json.RawMessage(`{"StatusFWR":{"Version":"` + s2 + `(release-tasmota)"}}`), nil
	}
	if cmnd == "Status 5" && s5 != "" {
		return json.RawMessage(`{"StatusNET":{"Mac":"` + s5 + `"}}`), nil
	}
	if strings.HasPrefix(cmnd, "WebGetConfig") {
		c.mu.Lock()
		c.order = append(c.order, "WebGetConfig")
		c.mu.Unlock()
	}
	if strings.HasPrefix(cmnd, "WebGetConfig") && c.swallowErr != nil {
		return nil, c.swallowErr
	}
	if strings.HasPrefix(cmnd, "WebGetConfig") && swallow {
		return json.RawMessage(`{"WebGetConfig":"Done"}`), nil
	}
	raw, err := c.Client.Command(ctx, t, cmnd)
	if err == nil && strings.HasPrefix(cmnd, "WebGetConfig") && c.afterErr != nil {
		return nil, c.afterErr
	}
	return raw, err
}

func (c *hookClient) DownloadConfig(ctx context.Context, t tasmota.Target) ([]byte, error) {
	if c.failDL.Load() {
		return nil, errors.New("download refused")
	}
	return c.Client.DownloadConfig(ctx, t)
}

func (c *hookClient) sent(prefix string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, s := range c.cmds {
		if strings.HasPrefix(s, prefix) {
			out = append(out, s)
		}
	}
	return out
}

// verifyHook runs before the verification backup (the second Backup call).
type hookBackups struct {
	Backups
	mu     sync.Mutex
	calls  int
	hook   func(call int)
	ofCall int
	hookOf func(call int) // before BackupOf, numbered from 1
}

func (b *hookBackups) BackupOf(ctx context.Context, actor string, owner model.DeviceID, src model.Device, note string) ([]model.Backup, error) {
	b.mu.Lock()
	b.ofCall++
	n := b.ofCall
	b.mu.Unlock()
	if b.hookOf != nil {
		b.hookOf(n)
	}
	return b.Backups.BackupOf(ctx, actor, owner, src, note)
}

func (b *hookBackups) Backup(ctx context.Context, actor string, id model.DeviceID) ([]model.Backup, error) {
	b.mu.Lock()
	b.calls++
	n := b.calls
	b.mu.Unlock()
	if b.hook != nil {
		b.hook(n)
	}
	return b.Backups.Backup(ctx, actor, id)
}

type auditSink struct {
	mu sync.Mutex
	ev []model.AuditEvent
}

func (a *auditSink) Record(_ context.Context, e model.AuditEvent) {
	a.mu.Lock()
	a.ev = append(a.ev, e)
	a.mu.Unlock()
}

// waitFor returns the last event with the action. The job goroutine records
// replace.finish right after the job is marked done, so tests wait for it.
func (a *auditSink) waitFor(t *testing.T, action string) model.AuditEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		a.mu.Lock()
		var found model.AuditEvent
		ok := false
		for _, e := range a.ev {
			if e.Action == action {
				found, ok = e, true
			}
		}
		a.mu.Unlock()
		if ok {
			return found
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s audit event", action)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// events is a copy of the recorded events, safe to read while jobs run.
func (a *auditSink) events() []model.AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]model.AuditEvent(nil), a.ev...)
}

func (a *auditSink) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, e := range a.ev {
		out = append(out, e.Action+":"+e.Result)
	}
	return out
}

type flowEnv struct {
	m      *Manager
	st     *store.SQLite
	bk     *backup.Manager
	hb     *hookBackups
	cl     *hookClient
	sim    simulator.Controller
	dev    model.DeviceID
	audit  *auditSink
	dumpA  []byte
	dumpB  []byte
	srv    *httptest.Server
	device model.Device
	fleet  simulator.Fleet
	served chan model.DeviceID // the devices OnServed reported
}

func dumpWith(b0, b1 byte) []byte {
	d := bytes.Repeat([]byte{0x11}, 4096)
	d[0], d[1] = b0, b1
	return d
}

func newFlow(t *testing.T, version string) *flowEnv { return newFlowWith(t, version, fakeDecoder{}) }

// newFlowWith is newFlow with the decoder that turns dumps into decoded backups.
func newFlowWith(t *testing.T, version string, dec backup.Decoder) *flowEnv {
	t.Helper()
	return newFlowSpec(t, version, dec, simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M})
}

// newFlowSpec is newFlowWith for the device the spec describes (profile, seeded files).
func newFlowSpec(t *testing.T, version string, dec backup.Decoder, spec simulator.DeviceSpec) *flowEnv {
	t.Helper()
	dir := t.TempDir()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fleet, err := simulator.NewFleetWith(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures"}, simulator.Extra{TimeScale: 0.02})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() })
	spec.Name, spec.Version = "plug", version
	sd, err := fleet.Add(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	ctl, _ := simulator.Control(sd)
	host, portStr, _ := net.SplitHostPort(sd.Addr())
	port, _ := strconv.Atoi(portStr)
	real := tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	cl := &hookClient{Client: real, fs: real.(tasmota.FileSystem)}
	family := model.ChipESP8266
	if strings.HasPrefix(string(spec.Profile), "esp32") {
		family = model.ChipESP32
	}
	dev := model.Device{Name: "plug", Address: host, Port: port, Identity: model.Identity{Version: version, Family: family}}
	raw5, err := cl.Command(t.Context(), tasmota.TargetFor(dev), "Status 5")
	if err != nil {
		t.Fatal(err)
	}
	var s5 struct{ StatusNET struct{ Mac string } }
	if err := json.Unmarshal(raw5, &s5); err != nil || s5.StatusNET.Mac == "" {
		t.Fatalf("simulator Status 5: %v %s", err, raw5)
	}
	dev.MAC = strings.ToLower(s5.StatusNET.Mac)
	id, err := st.CreateDevice(t.Context(), dev)
	if err != nil {
		t.Fatal(err)
	}
	dev.ID = id
	cl.mu.Lock()
	cl.cmds = nil // the probe above is not part of the flow under test
	cl.mu.Unlock()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	au := &auditSink{}
	bk, err := backup.New(backup.Options{DataDir: dir, Devices: st, Store: st, Client: cl, Decoder: dec, Audit: au, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	hb := &hookBackups{Backups: bk}
	e := &flowEnv{st: st, bk: bk, hb: hb, cl: cl, sim: ctl, dev: id, audit: au, dumpA: dumpWith(1, 10), dumpB: dumpWith(2, 20), device: dev, fleet: fleet}

	mux := http.NewServeMux()
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	e.served = make(chan model.DeviceID, 8)
	m, err := New(Options{
		Devices: st, Backups: hb, Store: st, Client: cl, Files: bk, BaseURL: e.srv.URL, Audit: au, Log: quiet,
		Hardware: st, Describe: e.describe,
		OnServed:     func(id model.DeviceID) { e.served <- id },
		PollInterval: 20 * time.Millisecond, RebootTimeout: 15 * time.Second, TokenTTL: 30 * time.Second, Context: t.Context(),
	})
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle("/restore/", m.Handler())
	e.m = m
	return e
}

// dumpBackup takes a backup while the device holds dump and returns the dmp record.
func (e *flowEnv) dumpBackup(t *testing.T, dump []byte) model.Backup {
	t.Helper()
	e.sim.SetConfigDump(dump)
	made, err := e.bk.Backup(t.Context(), "tester", e.dev)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range made {
		if b.Kind == model.BackupConfigDump {
			return b
		}
	}
	t.Fatal("no dump backup")
	return model.Backup{}
}

func (e *flowEnv) finish(t *testing.T, id store.RestoreID) Job {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		j, err := e.m.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if j.Terminal() {
			return j
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("restore did not finish")
	return Job{}
}

func (e *flowEnv) deviceDump(t *testing.T) []byte {
	t.Helper()
	b, err := e.cl.DownloadConfig(t.Context(), tasmota.TargetFor(e.device))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Why: the end-to-end promise. The admin picks an old backup; the device
// afterwards holds exactly that configuration, the configuration it had
// before is kept as an automatic backup, and the result is verified.
func TestRestoreRoundTripAgainstSimulator(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	e.sim.SetConfigDump(e.dumpB) // the device drifted away from backup A

	job, err := e.m.Start(t.Context(), "alice", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreDone || !done.Verified || done.Changes != 0 {
		t.Fatalf("job: %+v", done)
	}
	if got := e.deviceDump(t); !bytes.Equal(got, e.dumpA) {
		t.Error("device does not hold the restored configuration")
	}
	pre, _, err := e.bk.Open(t.Context(), done.PreBackupID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pre, e.dumpB) {
		t.Error("the pre-restore backup must hold the configuration the device had before")
	}
	// The list shows it as "Automatic, before restore" (N1).
	if _, b, err := e.bk.Open(t.Context(), done.PreBackupID); err != nil || b.Trigger != model.TriggerRestore {
		t.Errorf("pre-restore backup trigger: %+v %v", b, err)
	}
	// Why: the device fetched the dump from the firmware listener, so it reaches it (D11).
	select {
	case id := <-e.served:
		if id != e.dev {
			t.Errorf("OnServed reported device %d, want %d", id, e.dev)
		}
	default:
		t.Error("serving the dump did not report the device")
	}
	cmds := e.cl.sent("WebGetConfig")
	if len(cmds) != 1 || !strings.HasPrefix(cmds[0], "WebGetConfig "+e.srv.URL+"/restore/") {
		t.Errorf("commands: %v", cmds)
	}
	joined := strings.Join(e.audit.actions(), " ")
	for _, want := range []string{"restore.start:ok", "restore.download:ok", "restore.finish:ok", "backup.create:ok"} {
		if !strings.Contains(joined, want) {
			t.Errorf("audit lacks %s: %s", want, joined)
		}
	}
	// The token is spent: replaying the URL gives 404.
	tok := strings.TrimPrefix(cmds[0], "WebGetConfig "+e.srv.URL+"/restore/")
	if w := get(e.m.Handler(), "/restore/"+tok, "127.0.0.1:9"); w.Code != 404 {
		t.Errorf("replay: %d", w.Code)
	}
	for _, ev := range e.audit.events() {
		if strings.Contains(ev.Detail, tok) || strings.Contains(ev.Target, tok) {
			t.Errorf("token in audit record: %+v", ev)
		}
	}
}

// Why: the device rewrites checksums and counters when it saves; that must not
// make a good restore look failed. A real setting that differs must.
func TestVerificationIgnoresVolatileButNotRealDifferences(t *testing.T) {
	for _, tc := range []struct {
		name         string
		after        []byte
		wantVerified bool
		wantChanges  int
	}{
		{"checksum only", dumpWith(9, 10), true, 0},
		{"real setting (and its secret twin)", dumpWith(1, 99), false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFlow(t, "15.6.0")
			a := e.dumpBackup(t, e.dumpA)
			e.sim.SetConfigDump(e.dumpB)
			e.hb.hook = func(call int) {
				if call == 2 { // the verification backup: the firmware re-serialised
					e.sim.SetConfigDump(tc.after)
				}
			}
			job, err := e.m.Start(t.Context(), "alice", a.ID)
			if err != nil {
				t.Fatal(err)
			}
			done := e.finish(t, job.ID)
			if done.State != store.RestoreDone || done.Verified != tc.wantVerified || done.Changes != tc.wantChanges {
				t.Fatalf("job: %+v", done)
			}
			if strings.Contains(done.Detail, "secret-") {
				t.Errorf("detail leaks a value: %q", done.Detail)
			}
		})
	}
}

// Why: without the safety backup a bad restore cannot be undone, so a failing
// pre-restore backup must stop the run before the device is touched.
func TestFailedPreBackupStopsBeforeAnyChange(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	e.sim.SetConfigDump(e.dumpB)
	e.cl.failDL.Store(true)
	job, err := e.m.Start(t.Context(), "alice", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreFailed || !strings.Contains(done.Detail, "nothing was changed") {
		t.Fatalf("job: %+v", done)
	}
	// the progress page marks the step that failed (FailedStep reads it from this text)
	if got := Step(done); got != StepBackup {
		t.Errorf("failed step = %d, want %d (%s)", got, StepBackup, done.Detail)
	}
	if n := len(e.cl.sent("WebGetConfig")); n != 0 {
		t.Errorf("WebGetConfig sent %d times", n)
	}
}

// Why: if the device accepts the command but never fetches the dump (feature
// missing, listener unreachable for it), the job must fail loudly and the token
// must be gone, not linger for later use.
func TestDeviceThatNeverDownloadsFailsAndRevokesToken(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	e.cl.swallow = true
	e.m.o.RebootTimeout = 300 * time.Millisecond
	job, err := e.m.Start(t.Context(), "alice", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreFailed || !strings.Contains(done.Detail, "never downloaded") {
		t.Fatalf("job: %+v", done)
	}
	// the progress page marks the step that failed (FailedStep reads it from this text)
	if got := Step(done); got != StepRestart {
		t.Errorf("failed step = %d, want %d (%s)", got, StepRestart, done.Detail)
	}
	cmd := e.cl.sent("WebGetConfig")[0]
	tok := cmd[strings.LastIndex(cmd, "/")+1:]
	if w := get(e.m.Handler(), "/restore/"+tok, "127.0.0.1:9"); w.Code != 404 {
		t.Errorf("token still valid after failure: %d", w.Code)
	}
	if done.PreBackupID == 0 {
		t.Error("the pre-restore backup must be recorded")
	}
}

// Why: only a config dump can be restored, never to a device running older
// firmware than the backup (Tasmota migrates forward only), and never two at once.
func TestStartRefusals(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	made, _ := e.bk.List(t.Context(), e.dev)
	for _, b := range made {
		if b.Kind == model.BackupSnapshot {
			if _, err := e.m.Start(t.Context(), "alice", b.ID); !errors.Is(err, ErrNotDump) {
				t.Errorf("snapshot: %v", err)
			}
		}
	}
	if _, err := e.m.Start(t.Context(), "alice", 9999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown backup: %v", err)
	}

	e.cl.swallow = true // keeps the first job running
	e.m.o.RebootTimeout = 400 * time.Millisecond
	j1, err := e.m.Start(t.Context(), "alice", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Start(t.Context(), "bob", a.ID); !errors.Is(err, ErrBusy) {
		t.Errorf("second start while running: %v", err)
	}
	e.finish(t, j1.ID)

	// the device now runs older firmware than the backup
	e.cl.mu.Lock()
	e.cl.status2 = "14.0.0"
	e.cl.mu.Unlock()
	if _, err := e.m.Start(t.Context(), "alice", a.ID); !errors.Is(err, ErrNewerBackup) {
		t.Errorf("newer backup: %v", err)
	}
}

// Why: "one restore per device" and "one download per token" must hold when
// requests arrive at the same moment, not only one after the other.
func TestConcurrentStartsAndDownloadsAdmitExactlyOne(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	e.cl.swallow = true
	e.m.o.RebootTimeout = 500 * time.Millisecond
	var ok, busy atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch _, err := e.m.Start(t.Context(), "alice", a.ID); {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrBusy):
				busy.Add(1)
			default:
				t.Errorf("start: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 || busy.Load() != 7 {
		t.Errorf("accepted %d, busy %d; want 1 and 7", ok.Load(), busy.Load())
	}
	e.m.Wait()

	clk := &fakeClock{t: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	m := handlerManager(t, clk)
	tok, _ := m.issue(Job{ID: 1, DeviceID: 5}, []byte("x"), []netip.Addr{netip.MustParseAddr("172.31.13.40")})
	var served atomic.Int32
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if get(m.Handler(), "/restore/"+tok, "172.31.13.40:1").Code == 200 {
				served.Add(1)
			}
		}()
	}
	wg.Wait()
	if served.Load() != 1 {
		t.Errorf("token served %d times, want 1", served.Load())
	}
}

func (e *flowEnv) setStatus(mac, ver string) {
	e.cl.mu.Lock()
	e.cl.status5, e.cl.status2 = mac, ver
	e.cl.mu.Unlock()
}

// Why: M3. The dump holds every secret of one device. If the host at the
// address is not that device (DHCP reassignment, a name taken over), nothing
// must be sent: no command, no token, no pre-backup.
func TestWrongDeviceAtTheAddressIsRefusedBeforeAnything(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	e.setStatus("aa:bb:cc:dd:ee:ff", "")
	if _, err := e.m.Start(t.Context(), "alice", a.ID); !errors.Is(err, ErrIdentity) {
		t.Fatalf("got %v", err)
	}
	if n := len(e.cl.sent("WebGetConfig")); n != 0 {
		t.Errorf("WebGetConfig sent %d times", n)
	}
}

// Why: M3. The address may change hands between Start and the command; the
// check is repeated right before the token is issued.
func TestIdentityIsCheckedAgainBeforeTheTokenIsIssued(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	e.hb.hook = func(call int) {
		if call == 1 { // the pre-restore backup runs after Start's check
			e.setStatus("aa:bb:cc:dd:ee:ff", "")
		}
	}
	job, err := e.m.Start(t.Context(), "alice", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreFailed || !strings.Contains(done.Detail, "nothing was changed") {
		t.Fatalf("job: %+v", done)
	}
	// the progress page marks the step that failed (FailedStep reads it from this text)
	if got := Step(done); got != StepUpload {
		t.Errorf("failed step = %d, want %d (%s)", got, StepUpload, done.Detail)
	}
	if n := len(e.cl.sent("WebGetConfig")); n != 0 {
		t.Errorf("WebGetConfig sent %d times", n)
	}
}

// Why: M3. A name is resolved once; the stored name must never be dialled
// again, and an ambiguous answer is refused.
func TestAddressIsResolvedOnceAndAmbiguityRefused(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	dev := e.device
	dev.Address = "localhost" // the flow must use the one IP from Resolve, not dial the name itself
	if err := e.st.UpdateDevice(t.Context(), dev); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	e.m.o.Resolve = func(context.Context, string) ([]netip.Addr, error) {
		calls.Add(1)
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	job, err := e.m.Start(t.Context(), "alice", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done := e.finish(t, job.ID); done.State != store.RestoreDone {
		t.Fatalf("job: %+v", done)
	}
	if calls.Load() != 1 {
		t.Errorf("address resolved %d times", calls.Load())
	}
	e.m.o.Resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("127.0.0.2")}, nil
	}
	if _, err := e.m.Start(t.Context(), "alice", a.ID); !errors.Is(err, ErrNoAddress) {
		t.Errorf("ambiguous: %v", err)
	}
}

// Why: L1. WebGetConfig runs synchronously on the device; when only the
// reply is lost (timeout, unreadable) the device has been restored anyway and
// the job must follow through and verify, not report a false failure.
func TestLostReplyAfterTheDownloadStillVerifies(t *testing.T) {
	for _, cerr := range []error{tasmota.ErrTimeout, tasmota.ErrBadResponse} {
		e := newFlow(t, "15.6.0")
		a := e.dumpBackup(t, e.dumpA)
		e.sim.SetConfigDump(e.dumpB)
		e.cl.afterErr = cerr
		job, err := e.m.Start(t.Context(), "alice", a.ID)
		if err != nil {
			t.Fatal(err)
		}
		done := e.finish(t, job.ID)
		if done.State != store.RestoreDone || !done.Verified {
			t.Errorf("%v: job %+v", cerr, done)
		}
	}
}

// Why: L1. An error that is not "outcome unknown" and no download is a real
// failure, and an unknown outcome without a download must still end in one.
func TestRealCommandFailureStaysFailure(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	e.cl.swallowErr = tasmota.ErrAuth
	job, err := e.m.Start(t.Context(), "alice", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreFailed || !strings.Contains(done.Detail, "not accepted") {
		t.Fatalf("job: %+v", done)
	}
	// the progress page marks the step that failed (FailedStep reads it from this text)
	if got := Step(done); got != StepUpload {
		t.Errorf("failed step = %d, want %d (%s)", got, StepUpload, done.Detail)
	}
}

// Why: L2. A valid token used from another address is the sign of somebody
// sniffing the IoT VLAN. It must leave an audit record naming the job and the
// source (never the token), but only once per source in a while.
func TestRefusedDownloadFromWrongAddressIsAuditedAndRateLimited(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	au := &auditSink{}
	m, err := New(Options{Devices: nopDevices{}, Backups: nopBackups{}, Store: nopStore{}, Client: nopClient{}, BaseURL: "http://fw.invalid", Clock: clk, Audit: au,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := m.issue(Job{ID: 7, DeviceID: 5}, []byte("x"), []netip.Addr{netip.MustParseAddr("172.31.13.40")})
	h := m.Handler()
	for i := 0; i < 5; i++ {
		get(h, "/restore/"+tok, "172.31.13.99:5000")
	}
	evs := au.actions()
	if len(evs) != 1 || !strings.HasPrefix(evs[0], "restore.download:refused") {
		t.Fatalf("audit after 5 probes: %v", evs)
	}
	au.mu.Lock()
	ev := au.ev[0]
	au.mu.Unlock()
	if ev.RemoteAddr != "172.31.13.99" || strings.Contains(ev.Detail+ev.Target+ev.Result, tok) || !strings.Contains(ev.Detail, `"restore":7`) {
		t.Errorf("event: %+v", ev)
	}
	clk.Advance(11 * time.Second)
	get(h, "/restore/"+tok, "172.31.13.99:5000")
	if len(au.actions()) != 2 {
		t.Errorf("a later probe must be reported again: %v", au.actions())
	}
	// unknown tokens are logged only, not audited
	get(h, "/restore/"+strings.Repeat("a", 32), "172.31.13.98:1")
	if len(au.actions()) != 2 {
		t.Errorf("unknown tokens must not flood the audit log: %v", au.actions())
	}
}

// Why: L5. A restore must exclude and be excluded by other writers on the
// same device: the shared lock, and an active OTA job.
func TestRestoreExcludesOtherWritersOnTheDevice(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	locks := devlock.New()
	e.m.o.Locks = locks
	rel, _, _ := locks.TryAcquire(e.dev, "ota")
	if _, err := e.m.Start(t.Context(), "alice", a.ID); !errors.Is(err, ErrBusy) {
		t.Fatalf("lock held by OTA: %v", err)
	}
	rel()
	e.m.o.Jobs = fakeJobs{{DeviceID: e.dev, State: model.JobQueued}}
	if _, err := e.m.Start(t.Context(), "alice", a.ID); !errors.Is(err, ErrOTAActive) {
		t.Fatalf("active OTA job: %v", err)
	}
	if rel, _, ok := locks.TryAcquire(e.dev, "x"); !ok {
		t.Error("a refused Start must release the lock")
	} else {
		rel()
	}
	e.m.o.Jobs = nil
	job, err := e.m.Start(t.Context(), "alice", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, holder, ok := locks.TryAcquire(e.dev, "settings"); ok || holder != "restore" {
		t.Errorf("the running restore must hold the lock: ok=%v holder=%q", ok, holder)
	}
	e.finish(t, job.ID)
	e.m.Wait()
	if _, _, ok := locks.TryAcquire(e.dev, "x"); !ok {
		t.Error("the lock must be free after the restore")
	}
}

type fakeJobs []model.Job

func (f fakeJobs) ActiveJobs(context.Context) ([]model.Job, error) { return f, nil }

// Why: L4. Unknown versions refuse; a different major version and an
// uncheckable chip family need the admin's explicit confirmation, each
// waived only by its own flag; a family mismatch can never be waived.
func TestVersionAndFamilyGuards(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)

	e.setStatus("", "garbage")
	if _, err := e.m.Start(t.Context(), "alice", a.ID); !errors.Is(err, ErrVersionUnknown) {
		t.Errorf("unknown live version: %v", err)
	}

	e.setStatus("", "16.0.0")
	_, err := e.m.Start(t.Context(), "alice", a.ID)
	var oe *OverrideError
	if !errors.As(err, &oe) || !oe.Major || oe.Family {
		t.Fatalf("major change: %v", err)
	}
	if _, err := e.m.StartWith(t.Context(), "alice", a.ID, StartOptions{AllowUnknownFamily: true}); !errors.As(err, &oe) || !oe.Major {
		t.Errorf("the wrong flag must not waive the major change: %v", err)
	}
	job, err := e.m.StartWith(t.Context(), "alice", a.ID, StartOptions{AllowMajorChange: true})
	if err != nil {
		t.Fatal(err)
	}
	e.finish(t, job.ID)
	e.m.Wait()
	e.setStatus("", "")

	// the device is an ESP32 according to the inventory, the dump is ESP82xx
	dev := e.device
	dev.Identity.Family = model.ChipESP32
	if err := e.st.UpdateIdentity(t.Context(), e.dev, dev.Identity); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.StartWith(t.Context(), "alice", a.ID, StartOptions{AllowMajorChange: true, AllowUnknownFamily: true}); !errors.Is(err, ErrFamilyMismatch) {
		t.Errorf("family mismatch: %v", err)
	}
}

// Why: L4. Without a decoded twin the chip family cannot be read from the
// backup; that is not guessed, the admin has to confirm.
func TestUncheckableFamilyNeedsConfirmation(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	made, _ := e.bk.List(t.Context(), e.dev)
	for _, b := range made {
		if b.Kind == model.BackupDecoded {
			if err := e.st.DeleteBackup(t.Context(), b.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	_, err := e.m.Start(t.Context(), "alice", a.ID)
	var oe *OverrideError
	if !errors.As(err, &oe) || !oe.Family || oe.Major {
		t.Fatalf("got %v", err)
	}
	job, err := e.m.StartWith(t.Context(), "alice", a.ID, StartOptions{AllowUnknownFamily: true})
	if err != nil {
		t.Fatal(err)
	}
	if done := e.finish(t, job.ID); done.State != store.RestoreDone {
		t.Errorf("job: %+v", done)
	}
}
