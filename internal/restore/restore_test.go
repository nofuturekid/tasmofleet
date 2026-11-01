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
	swallow    bool   // accept the restore upload without doing anything
	afterErr   error  // returned after the restore upload was really executed
	swallowErr error  // returned instead of executing the restore upload
	failDL     atomic.Bool

	// fs is the file system part of the real client. The wrapper exposes it (so the backup and the
	// restore see a client with file access, as in production) and lets a test break one upload.
	fs tasmota.FileSystem
	// order records the writes in the sequence they reached the device: "upload:/path" and the
	// the restore upload of the settings ("RestoreConfig").
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
	return c.Client.Command(ctx, t, cmnd)
}

// RestoreConfig is the HTTP restore upload of the real client, recorded as the pseudo command
// "RestoreConfig" (in cmds and order) and steerable with swallow, swallowErr and afterErr.
func (c *hookClient) RestoreConfig(ctx context.Context, t tasmota.Target, dump []byte) error {
	if c.failCmd != nil {
		if err := c.failCmd("RestoreConfig"); err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.cmds = append(c.cmds, "RestoreConfig")
	c.order = append(c.order, "RestoreConfig")
	swallow, swallowErr, afterErr := c.swallow, c.swallowErr, c.afterErr
	c.mu.Unlock()
	if swallowErr != nil {
		return swallowErr
	}
	if swallow {
		return nil
	}
	err := c.Client.(tasmota.ConfigRestorer).RestoreConfig(ctx, t, dump)
	if err == nil && afterErr != nil {
		return afterErr
	}
	return err
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
	device model.Device
	fleet  simulator.Fleet
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

	m, err := New(Options{
		Devices: st, Backups: hb, Store: st, Client: cl, Files: bk, Audit: au, Log: quiet,
		Hardware: st, Describe: e.describe,
		PollInterval: 20 * time.Millisecond, RebootTimeout: 15 * time.Second, Context: t.Context(),
	})
	if err != nil {
		t.Fatal(err)
	}
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
	// #430: the dump goes over the device's own restore upload, exactly once; the device is never
	// asked to fetch anything (official builds have no WebGetConfig).
	if got := e.cl.sent("RestoreConfig"); len(got) != 1 {
		t.Errorf("restore uploads: %v", got)
	}
	if got := e.cl.sent("WebGetConfig"); len(got) != 0 {
		t.Errorf("WebGetConfig sent: %v", got)
	}
	if got := strings.Join(e.sim.Commands(), "\n"); !strings.Contains(got, "POST /u2 Config.dmp 4096 (settings)") {
		t.Errorf("the simulator saw no restore upload:\n%s", got)
	}
	joined := strings.Join(e.audit.actions(), " ")
	for _, want := range []string{"restore.start:ok", "restore.finish:ok", "backup.create:ok"} {
		if !strings.Contains(joined, want) {
			t.Errorf("audit lacks %s: %s", want, joined)
		}
	}
}

// Why: the trap seen live: a restore upload whose type the device lost answers "Successful" and
// restarts with nothing applied. The restart alone proves nothing; the verification afterwards
// must say that the device does not hold the backup.
func TestRestartWithoutTheSettingsIsNotVerified(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	e.sim.SetConfigDump(e.dumpB)
	e.cl.failCmd = func(c string) error {
		if c != "RestoreConfig" {
			return nil
		}
		// What the device does with a POST /u2 and no upload type: restart, apply nothing.
		_, err := e.cl.Client.Command(t.Context(), tasmota.TargetFor(e.device), "Restart 1")
		return err
	}
	e.cl.swallow = true // and answer "Successful"
	job, err := e.m.Start(t.Context(), "alice", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if done.Verified || !strings.Contains(done.Detail, "differ") {
		t.Fatalf("a restart without the settings passed as verified: %+v", done)
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
	if n := len(e.cl.sent("RestoreConfig")); n != 0 {
		t.Errorf("restore upload sent %d times", n)
	}
}

// Why: if the device answers the upload but never restarts, nothing proves the settings were
// applied: the job must fail loudly at the restart step, and the upload is never sent again.
func TestDeviceThatNeverRestartsFails(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	e.cl.swallow = true
	e.m.o.RebootTimeout = 300 * time.Millisecond
	job, err := e.m.Start(t.Context(), "alice", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreFailed || !strings.Contains(done.Detail, "did not restart") {
		t.Fatalf("job: %+v", done)
	}
	// the progress page marks the step that failed (FailedStep reads it from this text)
	if got := Step(done); got != StepRestart {
		t.Errorf("failed step = %d, want %d (%s)", got, StepRestart, done.Detail)
	}
	if n := len(e.cl.sent("RestoreConfig")); n != 1 {
		t.Errorf("restore upload sent %d times, want exactly 1", n)
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

// Why: "one restore per device" must hold when requests arrive at the same moment, not only one
// after the other.
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
}

func (e *flowEnv) setStatus(mac, ver string) {
	e.cl.mu.Lock()
	e.cl.status5, e.cl.status2 = mac, ver
	e.cl.mu.Unlock()
}

// Why: M3. The dump holds every secret of one device. If the host at the
// address is not that device (DHCP reassignment, a name taken over), nothing
// must be sent: no command, no upload, no pre-backup.
func TestWrongDeviceAtTheAddressIsRefusedBeforeAnything(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	e.setStatus("aa:bb:cc:dd:ee:ff", "")
	if _, err := e.m.Start(t.Context(), "alice", a.ID); !errors.Is(err, ErrIdentity) {
		t.Fatalf("got %v", err)
	}
	if n := len(e.cl.sent("RestoreConfig")); n != 0 {
		t.Errorf("restore upload sent %d times", n)
	}
}

// Why: M3. The address may change hands between Start and the upload; the
// check is repeated right before the dump is sent.
func TestIdentityIsCheckedAgainBeforeTheUpload(t *testing.T) {
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
	if n := len(e.cl.sent("RestoreConfig")); n != 0 {
		t.Errorf("restore upload sent %d times", n)
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

// Why: L1. When only the reply to the restore upload is lost (timeout, unreadable) the device
// has been restored anyway and the job must follow through and verify, not report a false
// failure, and never send the dump again.
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
		if n := len(e.cl.sent("RestoreConfig")); n != 1 {
			t.Errorf("%v: restore upload sent %d times", cerr, n)
		}
	}
}

// Why: L1. A certain refusal (the device's "Failed" page, or an upload that never left
// TasmoFleet) is a real failure at the upload step, at once, without waiting for a restart.
func TestRealUploadFailureStaysFailure(t *testing.T) {
	for _, cerr := range []error{
		fmt.Errorf("%w: File invalid", tasmota.ErrUploadRefused),
		fmt.Errorf("%w: %w", tasmota.ErrNotSent, tasmota.ErrAuth),
	} {
		e := newFlow(t, "15.6.0")
		a := e.dumpBackup(t, e.dumpA)
		e.cl.swallowErr = cerr
		job, err := e.m.Start(t.Context(), "alice", a.ID)
		if err != nil {
			t.Fatal(err)
		}
		done := e.finish(t, job.ID)
		if done.State != store.RestoreFailed || !strings.Contains(done.Detail, "not accepted") {
			t.Fatalf("%v: job %+v", cerr, done)
		}
		// the progress page marks the step that failed (FailedStep reads it from this text)
		if got := Step(done); got != StepUpload {
			t.Errorf("%v: failed step = %d, want %d (%s)", cerr, got, StepUpload, done.Detail)
		}
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
