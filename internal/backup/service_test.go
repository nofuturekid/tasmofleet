package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

const fixtureDir = "../../testdata/fixtures/devices/kitchen-spotlight"

// fakeDevice replays the captured replies of the kitchen-spotlight fixture.
type fakeDevice struct {
	mu       sync.Mutex
	sent     []string
	dmp      []byte
	dlErr    error
	inFlight int32
	maxSeen  int32
	extra    map[string]string // additional canned replies (Rule1.., Timers)
	errs     map[string]error  // commands that fail with this error
	t        *testing.T
}

func newFakeDevice(t *testing.T) *fakeDevice {
	return &fakeDevice{t: t, dmp: append([]byte("DMP-FAKE-"), bytes.Repeat([]byte{0xA5}, 4000)...)}
}

func (f *fakeDevice) reply(cmnd string) (json.RawMessage, error) {
	if err, ok := f.errs[cmnd]; ok {
		return nil, err
	}
	if r, ok := f.extra[cmnd]; ok {
		return json.RawMessage(r), nil
	}
	file := map[string]string{
		"Template": "template.json", "Module": "module.json",
	}[cmnd]
	if strings.HasPrefix(cmnd, "Status ") {
		file = "status-" + strings.TrimPrefix(cmnd, "Status ") + ".json"
	}
	if file == "" {
		return json.RawMessage(`{"Command":"Unknown","Input":"` + strings.ToUpper(cmnd) + `"}`), tasmota.ErrUnknownCommand
	}
	b, err := os.ReadFile(filepath.Join(fixtureDir, file))
	if err != nil {
		f.t.Fatal(err)
	}
	return b, nil
}

func (f *fakeDevice) Command(_ context.Context, _ tasmota.Target, cmnd string) (json.RawMessage, error) {
	f.mu.Lock()
	f.sent = append(f.sent, cmnd)
	f.mu.Unlock()
	return f.reply(cmnd)
}
func (f *fakeDevice) DownloadConfig(context.Context, tasmota.Target) ([]byte, error) {
	n := atomic.AddInt32(&f.inFlight, 1)
	defer atomic.AddInt32(&f.inFlight, -1)
	for {
		m := atomic.LoadInt32(&f.maxSeen)
		if n <= m || atomic.CompareAndSwapInt32(&f.maxSeen, m, n) {
			break
		}
	}
	time.Sleep(5 * time.Millisecond)
	return f.dmp, f.dlErr
}
func (f *fakeDevice) RootPage(context.Context, tasmota.Target) (tasmota.RootInfo, error) {
	return tasmota.RootInfo{}, errors.New("not used")
}

type fakeDecoder struct {
	version string
	err     error
	calls   int
}

func (d *fakeDecoder) Version(context.Context) (string, error) { return d.version, nil }
func (d *fakeDecoder) Decode(_ context.Context, dmp []byte) ([]byte, error) {
	d.calls++
	if d.err != nil {
		return nil, d.err
	}
	return []byte(fmt.Sprintf(`{"decoded_bytes":%d}`, len(dmp))), nil
}

type memBackups struct {
	mu       sync.Mutex
	next     model.BackupID
	rows     map[model.BackupID]model.Backup
	attempts []model.BackupAttempt
}

func (s *memBackups) SetBackupsKept(_ context.Context, ids []model.BackupID, kept bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		if _, ok := s.rows[id]; !ok {
			return store.ErrNotFound
		}
	}
	for _, id := range ids {
		b := s.rows[id]
		b.Kept = kept
		s.rows[id] = b
	}
	return nil
}
func (s *memBackups) ListOrphanBackups(ctx context.Context) ([]model.Backup, error) {
	return s.ListBackups(ctx, 0)
}
func (s *memBackups) AddBackupAttempt(_ context.Context, a model.BackupAttempt) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a.ID = int64(len(s.attempts) + 1)
	s.attempts = append([]model.BackupAttempt{a}, s.attempts...)
	return a.ID, nil
}
func (s *memBackups) ListBackupAttempts(_ context.Context, dev model.DeviceID, _ int) ([]model.BackupAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.BackupAttempt
	for _, a := range s.attempts {
		if a.DeviceID == dev {
			out = append(out, a)
		}
	}
	return out, nil
}
func (s *memBackups) PruneBackupAttempts(context.Context, time.Time) (int64, error) { return 0, nil }

// orphan turns the device's backups into those of a deleted device, as the
// store's ON DELETE SET NULL does.
func (s *memBackups) orphan(dev model.DeviceID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, b := range s.rows {
		if b.DeviceID == dev {
			b.DeviceID = 0
			s.rows[id] = b
		}
	}
}

func (s *memBackups) PutBackup(_ context.Context, b model.Backup) (model.BackupID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = map[model.BackupID]model.Backup{}
	}
	s.next++
	b.ID = s.next
	s.rows[b.ID] = b
	return b.ID, nil
}
func (s *memBackups) GetBackup(_ context.Context, id model.BackupID) (model.Backup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.rows[id]
	if !ok {
		return b, store.ErrNotFound
	}
	return b, nil
}
func (s *memBackups) ListBackups(_ context.Context, dev model.DeviceID) ([]model.Backup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.Backup
	for _, b := range s.rows {
		if b.DeviceID == dev {
			out = append(out, b)
		}
	}
	// newest first; within a run the dump first (stable by ID)
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].TakenAt.After(out[i].TakenAt) || (out[j].TakenAt.Equal(out[i].TakenAt) && out[j].ID < out[i].ID) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}
func (s *memBackups) DeleteBackup(_ context.Context, id model.BackupID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, id)
	return nil
}

type devs struct{ d model.Device }

func (d devs) GetDevice(_ context.Context, id model.DeviceID) (model.Device, error) {
	if id != d.d.ID {
		return model.Device{}, store.ErrNotFound
	}
	return d.d, nil
}

type auditSink struct{ ev []model.AuditEvent }

func (a *auditSink) Record(_ context.Context, e model.AuditEvent) { a.ev = append(a.ev, e) }

type stepClock struct{ t time.Time }

func (c *stepClock) Now() time.Time                       { return c.t }
func (c *stepClock) After(time.Duration) <-chan time.Time { return nil }

type env struct {
	m     *Manager
	dev   *fakeDevice
	st    *memBackups
	dec   *fakeDecoder
	audit *auditSink
	clk   *stepClock
	dir   string
}

func newEnv(t *testing.T, mutate func(*Options)) *env {
	t.Helper()
	e := &env{
		dev: newFakeDevice(t), st: &memBackups{}, dec: &fakeDecoder{version: "16.0.0.0"}, audit: &auditSink{},
		clk: &stepClock{time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}, dir: t.TempDir(),
	}
	o := Options{
		DataDir: e.dir, Devices: devs{model.Device{ID: 7, Name: "kitchen", Address: "172.31.13.28"}}, Store: e.st,
		Client: e.dev, Decoder: e.dec, Keep: 10, Audit: e.audit, Clock: e.clk,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mutate != nil {
		mutate(&o)
	}
	m, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	e.m = m
	return e
}

func TestBackupProducesThreeFilesWithModes(t *testing.T) {
	e := newEnv(t, nil)
	got, err := e.m.Backup(context.Background(), "alice", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want dump + snapshot + decoded, got %d", len(got))
	}
	wantKinds := []model.BackupKind{model.BackupConfigDump, model.BackupSnapshot, model.BackupDecoded}
	for i, b := range got {
		if b.Kind != wantKinds[i] || b.DeviceID != 7 || b.Version != "16.0.0" || b.ID == 0 {
			t.Errorf("backup %d: %+v", i, b)
		}
		full := filepath.Join(e.dir, filepath.FromSlash(b.Path))
		st, err := os.Stat(full)
		if err != nil {
			t.Fatal(err)
		}
		// Backups contain Wi-Fi/MQTT/web passwords (spec 12.3): owner only.
		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v", b.Path, st.Mode().Perm())
		}
		data, _ := os.ReadFile(full)
		sum := sha256.Sum256(data)
		if b.Size != int64(len(data)) || b.SHA256 != hex.EncodeToString(sum[:]) {
			t.Errorf("%s: index does not match file", b.Path)
		}
	}
	if !strings.HasPrefix(got[0].Path, "backups/7/20261018T120000Z-dmp.") || !strings.HasSuffix(got[0].Path, ".dmp") {
		t.Errorf("path layout: %s", got[0].Path)
	}
	if st, _ := os.Stat(filepath.Join(e.dir, "backups", "7")); st.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", st.Mode().Perm())
	}
	if got[2].DecoderVersion != "16.0.0.0" {
		t.Errorf("decoded backup must be stamped with the decoder version: %+v", got[2])
	}
	entries, _ := os.ReadDir(filepath.Join(e.dir, "backups", "7"))
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), ".tmp-") {
			t.Errorf("temp file left behind: %s", en.Name())
		}
	}
	if len(e.audit.ev) != 1 || e.audit.ev[0].Action != "backup.create" || e.audit.ev[0].Actor != "alice" || e.audit.ev[0].Result != "ok" {
		t.Errorf("audit: %+v", e.audit.ev)
	}
}

// Safety rule 2: Status 0 bursts corrupt ESP8266 replies. The snapshot asks
// for each status on its own.
func TestSnapshotNeverSendsStatus0AndQueriesOneByOne(t *testing.T) {
	e := newEnv(t, nil)
	if _, err := e.m.Backup(context.Background(), "t", 7); err != nil {
		t.Fatal(err)
	}
	for _, c := range e.dev.sent {
		if c == "Status 0" || c == "Status" || c == "Status 12" {
			t.Errorf("forbidden command %q", c)
		}
	}
	for i := 1; i <= 11; i++ {
		want := fmt.Sprintf("Status %d", i)
		if e.dev.sent[i-1] != want {
			t.Fatalf("command %d = %q, want %q (sequence %v)", i, e.dev.sent[i-1], want, e.dev.sent)
		}
	}
}

func TestSnapshotContentAndIncompleteNote(t *testing.T) {
	e := newEnv(t, nil)
	got, err := e.m.Backup(context.Background(), "t", 7)
	if err != nil {
		t.Fatal(err)
	}
	snap := got[1]
	// The fixture has no replies for Rule1..3 and Timers; the run must still
	// succeed but say what is missing.
	if !strings.Contains(snap.Note, "snapshot incomplete") || !strings.Contains(snap.Note, "Rule1") || !strings.Contains(snap.Note, "Timers") {
		t.Errorf("note: %q", snap.Note)
	}
	data, _, err := e.m.Open(context.Background(), snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("snapshot is not JSON: %v", err)
	}
	if !strings.Contains(string(doc["Status2"]), "ESP8266EX") || !strings.Contains(string(doc["Rule1"]), "_error") {
		t.Errorf("snapshot content: %s", data)
	}

	e.dev.extra = map[string]string{"Rule1": `{"Rule1":{"State":"OFF"}}`, "Rule2": `{"Rule2":{"State":"OFF"}}`, "Rule3": `{"Rule3":{"State":"OFF"}}`, "Timers": `{"Timers":"OFF"}`}
	e.clk.t = e.clk.t.Add(time.Minute)
	got, err = e.m.Backup(context.Background(), "t", 7)
	if err != nil {
		t.Fatal(err)
	}
	if got[1].Note != "" {
		t.Errorf("complete snapshot must carry no note: %q", got[1].Note)
	}
}

// Why: Status 9 (StatusPTH) exists only on devices with energy monitoring; the
// others answer {"Command":"Error"} by design (issue #55). That is not a
// missing part, while a failing Status 9 for any other reason still is.
func TestStatus9ErrorOnNonEnergyDeviceKeepsSnapshotComplete(t *testing.T) {
	e := newEnv(t, nil)
	e.dev.extra = map[string]string{"Rule1": `{"Rule1":{"State":"OFF"}}`, "Rule2": `{"Rule2":{"State":"OFF"}}`, "Rule3": `{"Rule3":{"State":"OFF"}}`, "Timers": `{"Timers":"OFF"}`}
	e.dev.errs = map[string]error{"Status 9": tasmota.ErrCommandError}
	got, err := e.m.Backup(context.Background(), "t", 7)
	if err != nil {
		t.Fatal(err)
	}
	if got[1].Note != "" {
		t.Errorf("Command Error on Status 9 marked the snapshot incomplete: %q", got[1].Note)
	}

	e.dev.errs = map[string]error{"Status 9": tasmota.ErrTimeout, "Status 11": tasmota.ErrCommandError}
	e.clk.t = e.clk.t.Add(time.Minute)
	got, err = e.m.Backup(context.Background(), "t", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got[1].Note, "Status 9") || !strings.Contains(got[1].Note, "Status 11") {
		t.Errorf("a timeout on Status 9 and Command Error on Status 11 must stay gaps: %q", got[1].Note)
	}
}

// A device that is newer than the decoder would be misread: keep only the raw
// dump and flag it, never store a wrong decode.
func TestDecoderOutdatedKeepsDumpOnly(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Decoder = &fakeDecoder{version: "15.1.0.2"} })
	dec := e.m.o.Decoder.(*fakeDecoder)
	got, err := e.m.Backup(context.Background(), "t", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Kind != model.BackupConfigDump || got[1].Kind != model.BackupSnapshot {
		t.Fatalf("want dump+snapshot only, got %+v", got)
	}
	if got[0].Note != NoteDecoderOutdated {
		t.Errorf("dump must be flagged: %q", got[0].Note)
	}
	if dec.calls != 0 {
		t.Error("decoder must not run on firmware it does not support")
	}
}

func TestDecoderFailureDoesNotFailBackup(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Decoder = &fakeDecoder{version: "16.0.0.0", err: errors.New("boom")} })
	got, err := e.m.Backup(context.Background(), "t", 7)
	if err != nil {
		t.Fatalf("the dump is authoritative; decode errors must not fail the run: %v", err)
	}
	if len(got) != 2 || !strings.HasPrefix(got[0].Note, NoteDecodeFailed) {
		t.Errorf("got %+v", got)
	}
}

func TestNoDecoderConfigured(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Decoder = nil })
	got, err := e.m.Backup(context.Background(), "t", 7)
	if err != nil || len(got) != 2 || got[0].Note != "" {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestDownloadFailureStoresNothing(t *testing.T) {
	e := newEnv(t, nil)
	e.dev.dlErr = tasmota.ErrUnreachable
	got, err := e.m.Backup(context.Background(), "t", 7)
	if !errors.Is(err, tasmota.ErrUnreachable) || len(got) != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if len(e.st.rows) != 0 {
		t.Error("index rows written without a dump")
	}
	if _, err := os.Stat(filepath.Join(e.dir, "backups", "7")); err == nil {
		t.Error("directory created for a failed run")
	}
	if len(e.audit.ev) != 1 || e.audit.ev[0].Result == "ok" {
		t.Errorf("failed run must be audited as failed: %+v", e.audit.ev)
	}
}

func TestUnknownDevice(t *testing.T) {
	e := newEnv(t, nil)
	if _, err := e.m.Backup(context.Background(), "t", 99); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

// Retention counts runs, not files: keeping 2 means 2 x (dump, snapshot,
// decoded), otherwise a run would be cut in half.
func TestRetentionKeepsNewestRunsAndRemovesFiles(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Keep = 2 })
	var first []model.Backup
	for i := 0; i < 4; i++ {
		got, err := e.m.Backup(context.Background(), "t", 7)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = got
		}
		e.clk.t = e.clk.t.Add(time.Hour)
	}
	list, _ := e.m.List(context.Background(), 7)
	if len(list) != 6 {
		t.Fatalf("want 2 runs x 3 files, got %d", len(list))
	}
	for _, b := range first {
		if _, err := os.Stat(filepath.Join(e.dir, filepath.FromSlash(b.Path))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("pruned file still on disk: %s", b.Path)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(e.dir, "backups", "7"))
	if len(entries) != 6 {
		t.Errorf("files on disk: %d", len(entries))
	}
}

func TestRetentionMaxAgeKeepsRecentRuns(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Keep = 1; o.MaxAge = 36 * time.Hour })
	var runs [][]model.Backup
	for i := 0; i < 4; i++ { // runs at 0h, 24h, 48h, 72h
		got, err := e.m.Backup(context.Background(), "t", 7)
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, got)
		e.clk.t = e.clk.t.Add(24 * time.Hour)
	}
	// After the last run (now = 72h): Keep=1 keeps run 3; run 2 (24h old) is
	// inside MaxAge and survives; runs 0 and 1 (72h, 48h old) are gone.
	list, _ := e.m.List(context.Background(), 7)
	if len(list) != 6 {
		t.Fatalf("want 2 runs (newest + one inside MaxAge), got %d rows", len(list))
	}
	for i, wantGone := range []bool{true, true, false, false} {
		_, err := os.Stat(filepath.Join(e.dir, filepath.FromSlash(runs[i][0].Path)))
		if gone := errors.Is(err, os.ErrNotExist); gone != wantGone {
			t.Errorf("run %d gone=%v, want %v", i, gone, wantGone)
		}
	}
}

// Two runs in the same second must not overwrite each other.
func TestSameSecondRunsGetDistinctFiles(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.m.Backup(context.Background(), "t", 7)
	b, err := e.m.Backup(context.Background(), "t", 7)
	if err != nil {
		t.Fatal(err)
	}
	if a[0].Path == b[0].Path {
		t.Fatal("second run overwrote the first")
	}
	if _, _, err := e.m.Open(context.Background(), a[0].ID); err != nil {
		t.Errorf("first run no longer intact: %v", err)
	}
}

// ESP8266 web servers are single-threaded: parallel runs for one device must queue.
func TestRunsForOneDeviceAreSerialised(t *testing.T) {
	e := newEnv(t, nil)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.m.Backup(context.Background(), "t", 7); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if e.dev.maxSeen != 1 {
		t.Errorf("concurrent downloads: %d", e.dev.maxSeen)
	}
}

func TestOpenVerifiesChecksumAndPath(t *testing.T) {
	e := newEnv(t, nil)
	got, _ := e.m.Backup(context.Background(), "t", 7)
	data, b, err := e.m.Open(context.Background(), got[0].ID)
	if err != nil || !bytes.Equal(data, e.dev.dmp) || b.Kind != model.BackupConfigDump {
		t.Fatalf("open: %v", err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, filepath.FromSlash(got[0].Path)), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.m.Open(context.Background(), got[0].ID); !errors.Is(err, ErrIntegrity) {
		t.Errorf("tampered file: %v", err)
	}
	// A poisoned index row must not read outside the data directory.
	id, _ := e.st.PutBackup(context.Background(), model.Backup{DeviceID: 7, Path: "../../etc/passwd"})
	if _, _, err := e.m.Open(context.Background(), id); err == nil {
		t.Error("path traversal accepted")
	}
	if _, _, err := e.m.Open(context.Background(), 999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}

func TestNewRequiresDependencies(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("want error")
	}
}

func TestVersionComparison(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"16.0.0.0", "16.0.0", 0},
		{"16.0.0", "16.0.0.1", -1},
		{"15.1.0.2", "16.0.0", -1},
		{"16.1", "16.0.9", 1},
		{"v16.0.0.0", "16.0.0(release-tasmota)", 0},
		{"9.5.0", "16.0.0", -1}, // numeric, not lexical
	} {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("%s vs %s = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	if err := CheckDecoder("15.1.0.2", "16.0.0"); !errors.Is(err, ErrDecoderOutdated) {
		t.Errorf("older decoder: %v", err)
	}
	for _, ok := range [][2]string{{"16.0.0.0", "16.0.0"}, {"17.0.0", "16.0.0"}, {"15.0.0", ""}} {
		if err := CheckDecoder(ok[0], ok[1]); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
}

// Why: the drift view must not write anything (no files, no index rows, no
// audit entry) and must obey the same Status-one-by-one rule as a backup.
func TestLiveSnapshotStoresNothingAndNeverSendsStatus0(t *testing.T) {
	e := newEnv(t, nil)
	data, err := e.m.LiveSnapshot(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) || !bytes.Contains(data, []byte("Status2")) {
		t.Errorf("snapshot: %s", data)
	}
	if len(e.st.rows) != 0 || len(e.audit.ev) != 0 {
		t.Errorf("LiveSnapshot wrote state: rows=%d audit=%d", len(e.st.rows), len(e.audit.ev))
	}
	if _, err := os.Stat(filepath.Join(e.dir, "backups")); err == nil {
		t.Error("LiveSnapshot created files")
	}
	for _, c := range e.dev.sent {
		if c == "Status 0" {
			t.Error("Status 0 sent")
		}
	}
}

// Why: the retention can be changed on the settings page while the server runs;
// the next pruning must use the new rule.
func TestSetRetentionAppliesFromTheNextPruning(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Keep = 5 })
	for i := 0; i < 4; i++ {
		if _, err := e.m.Backup(context.Background(), "t", 7); err != nil {
			t.Fatal(err)
		}
		e.clk.t = e.clk.t.Add(time.Hour)
	}
	if list, _ := e.m.List(context.Background(), 7); len(list) != 12 {
		t.Fatalf("4 runs x 3 files expected, got %d", len(list))
	}
	e.m.SetRetention(1, 0)
	e.m.SetRetention(0, 0) // invalid: ignored
	if _, err := e.m.Backup(context.Background(), "t", 7); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.m.List(context.Background(), 7); len(list) != 3 {
		t.Fatalf("only the newest run must be left, got %d files", len(list))
	}
}
