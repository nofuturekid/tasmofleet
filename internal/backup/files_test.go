package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// flakyFS wraps the real HTTP client (which implements tasmota.FileSystem) so a test can make single
// calls fail the way a real device does, and count what reached it.
type flakyFS struct {
	tasmota.Client
	fs tasmota.FileSystem

	mu        sync.Mutex
	downloads []string
	infoErr   error
	listErr   map[string]error // by folder
	getErr    map[string]error // by file
	skipNames map[string][]string
	stall     map[string]bool // downloads that hang until the context ends
	infoStall bool
	maxes     []int64
	listMod   func(dir string, e []tasmota.FileEntry) []tasmota.FileEntry
}

func (f *flakyFS) FSInfo(ctx context.Context, t tasmota.Target) (tasmota.FSInfo, error) {
	if f.infoStall {
		<-ctx.Done()
		return tasmota.FSInfo{}, ctx.Err()
	}
	if f.infoErr != nil {
		return tasmota.FSInfo{}, f.infoErr
	}
	return f.fs.FSInfo(ctx, t)
}

func (f *flakyFS) ListFiles(ctx context.Context, t tasmota.Target, dir string) ([]tasmota.FileEntry, error) {
	if err := f.listErr[dir]; err != nil {
		return nil, err
	}
	e, err := f.fs.ListFiles(ctx, t, dir)
	if f.listMod != nil {
		e = f.listMod(dir, e)
	}
	if names := f.skipNames[dir]; err == nil && len(names) > 0 {
		return e, &tasmota.SkippedNamesError{Names: names}
	}
	return e, err
}

func (f *flakyFS) DownloadFile(ctx context.Context, t tasmota.Target, p string, max int64) ([]byte, error) {
	f.mu.Lock()
	f.downloads = append(f.downloads, p)
	f.maxes = append(f.maxes, max)
	f.mu.Unlock()
	if f.stall[p] {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err := f.getErr[p]; err != nil {
		return nil, err
	}
	return f.fs.DownloadFile(ctx, t, p, max)
}

func (f *flakyFS) UploadFile(ctx context.Context, t tasmota.Target, p string, data []byte) error {
	return f.fs.UploadFile(ctx, t, p, data)
}

type filesEnv struct {
	*env
	flaky *flakyFS
	logs  *bytes.Buffer
}

// newFilesEnv backs up a simulated device through the real HTTP client, so listing, the /ufsd
// download and the SSRF guard are all exercised.
func newFilesEnv(t *testing.T, spec simulator.DeviceSpec, mutate func(*Options)) *filesEnv {
	t.Helper()
	dump, err := os.ReadFile("testdata/synthetic/synthetic-15.6.0.3.dmp")
	if err != nil {
		t.Fatal(err)
	}
	fleet, err := simulator.NewFleet(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() })
	if spec.Name == "" {
		spec.Name = "plug"
	}
	if spec.Version == "" {
		spec.Version = "16.0.0"
	}
	dev, err := fleet.Add(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	ctl, _ := simulator.Control(dev)
	ctl.SetConfigDump(dump)
	host, portStr, err := net.SplitHostPort(dev.Addr())
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)
	client := tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	fl := &flakyFS{Client: client, fs: client.(tasmota.FileSystem)}
	logs := &bytes.Buffer{}
	e := newEnv(t, func(o *Options) {
		o.Devices = devs{model.Device{ID: 7, Name: "plug", Address: host, Port: port}}
		o.Client = fl
		o.Decoder = nil
		o.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		if mutate != nil {
			mutate(o)
		}
	})
	return &filesEnv{env: e, flaky: fl, logs: logs}
}

func esp32(files map[string]simulator.FSFile, dirs ...string) simulator.DeviceSpec {
	return simulator.DeviceSpec{Profile: simulator.ProfileESP32, Files: files, Dirs: dirs, FSSizeKB: 2048}
}

func kindOf(bs []model.Backup, k model.BackupKind) (model.Backup, bool) {
	for _, b := range bs {
		if b.Kind == k {
			return b, true
		}
	}
	return model.Backup{}, false
}

func dumpNote(t *testing.T, bs []model.Backup) string {
	t.Helper()
	d, ok := kindOf(bs, model.BackupConfigDump)
	if !ok {
		t.Fatal("no dump in run")
	}
	return d.Note
}

func skippedReasons(m Manifest) map[string]string {
	out := map[string]string{}
	for _, s := range m.Skipped {
		out[s.Path] = s.Reason
	}
	return out
}

func TestFilesBackupStoresTheFlashFilesNextToTheDump(t *testing.T) {
	// Why (#324): a replacement ESP32 is useless without its Berry scripts and TLS key; the files of
	// a run must be restorable from the archive, byte for byte, and the private key marked.
	e := newFilesEnv(t, esp32(nil), nil) // the ESP32 profile's default files
	got, err := e.m.Backup(t.Context(), "alice", 7)
	if err != nil {
		t.Fatal(err)
	}
	tarRow, ok := kindOf(got, model.BackupFiles)
	if !ok {
		t.Fatalf("no files row in %+v", got)
	}
	d, _ := kindOf(got, model.BackupConfigDump)
	if want := strings.TrimSuffix(d.Path, ".dmp") + ".files.tar"; tarRow.Path != want {
		t.Errorf("archive path %q, want %q (next to the dump)", tarRow.Path, want)
	}
	st, err := os.Stat(filepath.Join(e.dir, filepath.FromSlash(tarRow.Path)))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("archive mode %v, %v: it holds a private key, so 0600 like the dump", st.Mode(), err)
	}
	if d.Note != "" || tarRow.Note != "" {
		t.Errorf("a complete run carries no note: dump %q archive %q", d.Note, tarRow.Note)
	}

	a, err := e.m.OpenFilesArchive(t.Context(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Manifest.Files) != 3 || len(a.Manifest.Skipped) != 0 {
		t.Fatalf("manifest %+v", a.Manifest)
	}
	for _, f := range a.Manifest.Files {
		data, err := a.Read(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if int64(len(data)) != f.Size || hex.EncodeToString(sum[:]) != f.SHA256 || !f.Stored {
			t.Errorf("%s: manifest does not describe the stored bytes: %+v", f.Path, f)
		}
		if f.PrivateKey != (f.Path == "/tlskey") {
			t.Errorf("%s: private_key = %v, only /tlskey is one", f.Path, f.PrivateKey)
		}
	}
	if data, _ := a.Read("/lib/helper.be"); string(data) != "# helper\n" {
		t.Errorf("file in a folder: %q", data)
	}
	if a.Manifest.FSType != tasmota.FSLittleFS || a.Manifest.FSSizeKB != 2048 {
		t.Errorf("filesystem facts: %+v", a.Manifest)
	}
}

func TestFilesNeverReachLogsOrAudit(t *testing.T) {
	// Why (spec 12.3): the archive is a secret like the dump; neither contents nor the key may show up
	// in logs or the audit trail.
	e := newFilesEnv(t, esp32(nil), nil)
	if _, err := e.m.Backup(t.Context(), "alice", 7); err != nil {
		t.Fatal(err)
	}
	all := e.logs.String()
	for _, ev := range e.audit.ev {
		all += ev.Detail + ev.Result
	}
	for _, secret := range []string{"BEGIN FAKE KEY", "not-a-real-key", "print('autoexec')"} {
		if strings.Contains(all, secret) {
			t.Errorf("%q leaked into logs or audit", secret)
		}
	}
}

func TestDeviceWithoutFilesystemGetsNoArchiveButASaidReason(t *testing.T) {
	// Why: an ESP8266 1 MB build has no file system; that is not a failure, but the person looking
	// at the run must be told why there is no Files block.
	e := newFilesEnv(t, simulator.DeviceSpec{Profile: simulator.ProfileESP8266_1M}, nil)
	got, err := e.m.Backup(t.Context(), "alice", 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := kindOf(got, model.BackupFiles); ok {
		t.Error("an archive was written for a device without a file system")
	}
	if n := dumpNote(t, got); !strings.Contains(n, NoteFilesPrefix+"no filesystem") {
		t.Errorf("dump note %q", n)
	}
	d, _ := kindOf(got, model.BackupConfigDump)
	rf, err := e.m.RunFiles(t.Context(), d.ID)
	if err != nil || rf.Archive != nil || rf.Manifest != nil || rf.Note != "no filesystem" {
		t.Errorf("RunFiles = %+v, %v", rf, err)
	}
	if _, err := e.m.OpenFilesArchive(t.Context(), d.ID); !errors.Is(err, ErrNoFilesArchive) {
		t.Errorf("OpenFilesArchive: %v", err)
	}
}

func TestOldFirmwareIsNotAskedForFiles(t *testing.T) {
	// Why: before 14.6.0 UfsList has another shape (no modification time); the files step must not
	// guess, it says "firmware older than 14.6.0" and sends no list or download.
	spec := esp32(nil)
	spec.Version = "14.5.0"
	e := newFilesEnv(t, spec, nil)
	got, err := e.m.Backup(t.Context(), "alice", 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := kindOf(got, model.BackupFiles); ok {
		t.Error("archive written for firmware older than 14.6.0")
	}
	if n := dumpNote(t, got); !strings.Contains(n, NoteFilesPrefix+"firmware older than 14.6.0") {
		t.Errorf("dump note %q", n)
	}
	if len(e.flaky.downloads) != 0 {
		t.Errorf("downloads: %v", e.flaky.downloads)
	}
}

func TestFilesOverTheCapsAreListedNotStored(t *testing.T) {
	// Why: the per-file cap protects the single-threaded web server of the device and the disk of
	// the backup; a file over it must stay visible in the manifest (so the person knows it is
	// missing) without its bytes being stored.
	big := bytes.Repeat([]byte{'x'}, int(MaxFileBytes)+1)
	e := newFilesEnv(t, esp32(map[string]simulator.FSFile{
		"/big.bin":  {Data: big},
		"/small.be": {Data: []byte("ok")},
	}), nil)
	got, err := e.m.Backup(t.Context(), "alice", 7)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := kindOf(got, model.BackupConfigDump)
	a, err := e.m.OpenFilesArchive(t.Context(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Manifest.Files) != 1 || a.Manifest.Files[0].Path != "/small.be" {
		t.Errorf("stored: %+v", a.Manifest.Files)
	}
	if r := skippedReasons(a.Manifest)["/big.bin"]; r != "larger than 1 MiB" {
		t.Errorf("reason %q", r)
	}
	if s := a.Manifest.Skipped[0]; s.Size != int64(len(big)) {
		t.Errorf("skipped size %d, want the listed size %d", s.Size, len(big))
	}
	if _, err := a.Read("/big.bin"); err == nil {
		t.Error("an unstored file can be read")
	}
	for _, p := range e.flaky.downloads {
		if p == "/big.bin" {
			t.Error("the oversized file was downloaded although its size was known")
		}
	}
	if tr, _ := kindOf(got, model.BackupFiles); !strings.Contains(tr.Note, "1 file not stored") {
		t.Errorf("archive note %q: the run must say it is incomplete", tr.Note)
	}
}

func TestRunCapStopsStoringAtFourMiB(t *testing.T) {
	// Why: a 16 MB board with a big file system would make every pre-OTA backup minutes long.
	files := map[string]simulator.FSFile{}
	for i := 0; i < 5; i++ {
		files[fmt.Sprintf("/f%d.bin", i)] = simulator.FSFile{Data: bytes.Repeat([]byte{byte('a' + i)}, 900<<10)}
	}
	e := newFilesEnv(t, esp32(files), nil)
	got, err := e.m.Backup(t.Context(), "alice", 7)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := kindOf(got, model.BackupConfigDump)
	a, _ := e.m.OpenFilesArchive(t.Context(), d.ID)
	var total int64
	for _, f := range a.Manifest.Files {
		total += f.Size
	}
	if len(a.Manifest.Files) != 4 || total > MaxRunBytes {
		t.Fatalf("stored %d files, %d bytes (cap %d)", len(a.Manifest.Files), total, MaxRunBytes)
	}
	if r := skippedReasons(a.Manifest)["/f4.bin"]; r != "run limit of 4 MiB reached" {
		t.Errorf("reason %q (skipped %+v)", r, a.Manifest.Skipped)
	}
}

func TestMoreThanHundredFilesAreCapped(t *testing.T) {
	// Why: each file is one request to a single-threaded device.
	files := map[string]simulator.FSFile{}
	for i := 0; i < 105; i++ {
		files[fmt.Sprintf("/s/f%03d", i)] = simulator.FSFile{Data: []byte("x")}
	}
	e := newFilesEnv(t, esp32(files), nil)
	got, err := e.m.Backup(t.Context(), "alice", 7)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := kindOf(got, model.BackupConfigDump)
	a, _ := e.m.OpenFilesArchive(t.Context(), d.ID)
	if len(a.Manifest.Files) != MaxFiles || len(a.Manifest.Skipped) != 5 || len(e.flaky.downloads) != MaxFiles {
		t.Fatalf("stored %d, skipped %d, downloads %d", len(a.Manifest.Files), len(a.Manifest.Skipped), len(e.flaky.downloads))
	}
	if r := skippedReasons(a.Manifest)["/s/f104"]; r != "more than 100 files" {
		t.Errorf("reason %q", r)
	}
}

func TestFoldersDeeperThanFourAreNotEntered(t *testing.T) {
	// Why: the device's 50 byte name buffer makes deeper trees unlikely; a runaway tree must not
	// become unbounded listing requests.
	e := newFilesEnv(t, esp32(map[string]simulator.FSFile{
		"/a/b/c/d/ok.be":   {Data: []byte("ok")},
		"/a/b/c/d/e/no.be": {Data: []byte("no")},
	}), nil)
	got, err := e.m.Backup(t.Context(), "alice", 7)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := kindOf(got, model.BackupConfigDump)
	a, _ := e.m.OpenFilesArchive(t.Context(), d.ID)
	if len(a.Manifest.Files) != 1 || a.Manifest.Files[0].Path != "/a/b/c/d/ok.be" {
		t.Errorf("stored %+v", a.Manifest.Files)
	}
	if r := skippedReasons(a.Manifest)["/a/b/c/d/e"]; r != "deeper than 4 folders" {
		t.Errorf("reason %q (skipped %+v)", r, a.Manifest.Skipped)
	}
}

func TestAFileGoneBeforeItsDownloadNeverFailsTheRun(t *testing.T) {
	// Why: the dump is authoritative; the files are best effort. A file deleted between listing and
	// download (a Berry script rotating its log) must not cost the person the whole backup.
	e := newFilesEnv(t, esp32(map[string]simulator.FSFile{
		"/a.be": {Data: []byte("a")}, "/b.be": {Data: []byte("b")},
	}), nil)
	e.flaky.getErr = map[string]error{"/a.be": tasmota.ErrFileNotFound}
	got, err := e.m.Backup(t.Context(), "alice", 7)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	d, _ := kindOf(got, model.BackupConfigDump)
	a, _ := e.m.OpenFilesArchive(t.Context(), d.ID)
	if len(a.Manifest.Files) != 1 || a.Manifest.Files[0].Path != "/b.be" {
		t.Errorf("stored %+v", a.Manifest.Files)
	}
	if r := skippedReasons(a.Manifest)["/a.be"]; r != "file no longer on the device" {
		t.Errorf("reason %q", r)
	}
	if tr, _ := kindOf(got, model.BackupFiles); !strings.Contains(tr.Note, "files incomplete") {
		t.Errorf("archive note %q", tr.Note)
	}
}

func TestADeviceThatStopsAnsweringIsNotAskedAgainForEveryFile(t *testing.T) {
	// Why: 100 downloads that each wait for a 60 s timeout would block the backup for an hour.
	files := map[string]simulator.FSFile{}
	for i := 0; i < 10; i++ {
		files[fmt.Sprintf("/f%d", i)] = simulator.FSFile{Data: []byte("x")}
	}
	e := newFilesEnv(t, esp32(files), nil)
	e.flaky.getErr = map[string]error{"/f1": fmt.Errorf("%w: gone", tasmota.ErrUnreachable)}
	got, err := e.m.Backup(t.Context(), "alice", 7)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if len(e.flaky.downloads) != 2 {
		t.Errorf("downloads after the device went away: %v", e.flaky.downloads)
	}
	d, _ := kindOf(got, model.BackupConfigDump)
	a, _ := e.m.OpenFilesArchive(t.Context(), d.ID)
	if len(a.Manifest.Files) != 1 || len(a.Manifest.Skipped) != 9 {
		t.Errorf("stored %d, skipped %d", len(a.Manifest.Files), len(a.Manifest.Skipped))
	}
	if r := skippedReasons(a.Manifest)["/f9"]; r != "not read: the device did not answer" {
		t.Errorf("reason %q", r)
	}
}

func TestSkippedNamesKeepTheEntriesAndAreRecorded(t *testing.T) {
	// Why (finding from #412): ListFiles returns the valid entries together with *SkippedNamesError.
	// Treating that as a failure would drop every file of the folder for one odd name.
	e := newFilesEnv(t, esp32(map[string]simulator.FSFile{"/a.be": {Data: []byte("a")}}), nil)
	e.flaky.skipNames = map[string][]string{"/": {"bad\x01name", "x"}}
	got, err := e.m.Backup(t.Context(), "alice", 7)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := kindOf(got, model.BackupConfigDump)
	a, _ := e.m.OpenFilesArchive(t.Context(), d.ID)
	if len(a.Manifest.Files) != 1 {
		t.Errorf("entries dropped: %+v", a.Manifest.Files)
	}
	r := skippedReasons(a.Manifest)
	if r["/x"] != "name not accepted" || r["/bad?name"] != "name not accepted" {
		t.Errorf("skipped names: %+v (control characters must not reach the manifest)", a.Manifest.Skipped)
	}
}

func TestAnUnlistableRootGivesNoArchiveAndStillASuccessfulRun(t *testing.T) {
	// Why: with no listing there is nothing to describe; the run keeps the dump and says why.
	e := newFilesEnv(t, esp32(nil), nil)
	e.flaky.listErr = map[string]error{"/": fmt.Errorf("%w: http://admin:secret@10.0.0.9/", tasmota.ErrTimeout)}
	got, err := e.m.Backup(t.Context(), "alice", 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := kindOf(got, model.BackupFiles); ok {
		t.Error("archive despite no listing")
	}
	n := dumpNote(t, got)
	if !strings.Contains(n, NoteFilesPrefix+"the device took too long to answer") || strings.Contains(n, "secret") {
		t.Errorf("dump note %q (chosen from the error kind, never copied from its text)", n)
	}
}

func TestFilesNeedHTTPIsSaidNotFailed(t *testing.T) {
	// Why: a device reached over MQTT only cannot hand over files; the person must see "needs HTTP"
	// rather than a failed backup.
	e := newFilesEnv(t, esp32(nil), nil)
	e.flaky.infoErr = transport.ErrFilesNeedHTTP
	got, err := e.m.Backup(t.Context(), "alice", 7)
	if err != nil {
		t.Fatal(err)
	}
	if n := dumpNote(t, got); !strings.Contains(n, NoteFilesPrefix+"they need HTTP") {
		t.Errorf("dump note %q", n)
	}
	d, _ := kindOf(got, model.BackupConfigDump)
	if rf, _ := e.m.RunFiles(t.Context(), d.ID); rf.Note != "they need HTTP" {
		t.Errorf("RunFiles.Note %q", rf.Note)
	}
}

func TestFilesNoteKeepsLabelAndReplacementMarker(t *testing.T) {
	// Why: Replace recognises the replacement hardware's dump by the start of its note; the files
	// sentence is appended and must not hide that marker.
	e := newFilesEnv(t, simulator.DeviceSpec{Profile: simulator.ProfileESP8266_1M}, nil)
	got, err := e.m.BackupOf(t.Context(), "alice", 7, model.Device{ID: 7, Name: "plug", Address: e.m.o.Devices.(devs).d.Address, Port: e.m.o.Devices.(devs).d.Port}, NoteReplacementHardware)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := kindOf(got, model.BackupConfigDump)
	if !IsReplacementHardware(d) || !strings.Contains(d.Note, NoteFilesPrefix+"no filesystem") {
		t.Errorf("note %q", d.Note)
	}
	if FilesNoteOf(d.Note) != "no filesystem" {
		t.Errorf("FilesNoteOf = %q", FilesNoteOf(d.Note))
	}
}

func TestRetentionAndDeleteTreatTheArchiveAsPartOfItsRun(t *testing.T) {
	// Why: the archive holds a private key; a pruned or deleted run must not leave it behind on disk.
	e := newFilesEnv(t, esp32(nil), func(o *Options) { o.Keep = 1 })
	first, err := e.m.Backup(t.Context(), "alice", 7)
	if err != nil {
		t.Fatal(err)
	}
	e.clk.t = e.clk.t.Add(time.Minute)
	if _, err := e.m.Backup(t.Context(), "alice", 7); err != nil {
		t.Fatal(err)
	}
	oldTar, _ := kindOf(first, model.BackupFiles)
	if _, err := os.Stat(filepath.Join(e.dir, filepath.FromSlash(oldTar.Path))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pruned run left its archive on disk: %v", err)
	}
	list, _ := e.m.List(t.Context(), 7)
	for _, b := range list {
		if b.Kind == model.BackupFiles && b.TakenAt.Equal(oldTar.TakenAt) {
			t.Error("pruned run left its archive row")
		}
	}
	if len(list) != 3 { // dump, snapshot, files of the newest run
		t.Fatalf("%d rows kept", len(list))
	}

	newTar, _ := kindOf(list, model.BackupFiles)
	d, err := e.m.DeleteRun(t.Context(), "alice", newTar.ID)
	if err != nil || d.Files != 3 {
		t.Fatalf("delete: %+v, %v", d, err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, filepath.FromSlash(newTar.Path))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("deleted run left its archive on disk: %v", err)
	}
}

func TestAKeptRunKeepsItsArchive(t *testing.T) {
	// Why: Keep pins a restore point; without the files it would no longer be a full one.
	e := newFilesEnv(t, esp32(nil), func(o *Options) { o.Keep = 1 })
	first, err := e.m.Backup(t.Context(), "alice", 7)
	if err != nil {
		t.Fatal(err)
	}
	pinned, _ := kindOf(first, model.BackupConfigDump)
	if _, err := e.m.SetKept(t.Context(), "alice", pinned.ID, true); err != nil {
		t.Fatal(err)
	}
	e.clk.t = e.clk.t.Add(time.Minute)
	for i := 0; i < 2; i++ {
		if _, err := e.m.Backup(t.Context(), "alice", 7); err != nil {
			t.Fatal(err)
		}
		e.clk.t = e.clk.t.Add(time.Minute)
	}
	rf, err := e.m.RunFiles(t.Context(), pinned.ID)
	if err != nil || rf.Archive == nil {
		t.Fatalf("kept run lost its archive: %+v, %v", rf, err)
	}
}

func TestOpeningATamperedArchiveFails(t *testing.T) {
	// Why: the archive feeds restore later; a file changed on disk must be refused (SHA-256 of the row).
	e := newFilesEnv(t, esp32(nil), nil)
	got, _ := e.m.Backup(t.Context(), "alice", 7)
	tr, _ := kindOf(got, model.BackupFiles)
	full := filepath.Join(e.dir, filepath.FromSlash(tr.Path))
	data, _ := os.ReadFile(full)
	data[600] ^= 0xFF
	if err := os.WriteFile(full, data, 0o600); err != nil {
		t.Fatal(err)
	}
	d, _ := kindOf(got, model.BackupConfigDump)
	if _, err := e.m.OpenFilesArchive(t.Context(), d.ID); !errors.Is(err, ErrIntegrity) {
		t.Errorf("got %v, want ErrIntegrity", err)
	}
}

// ---- the archive reader, fed with hand-made tars

type tarEntry struct {
	name string
	data []byte
	typ  byte
}

func rawTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		if err := w.WriteHeader(&tar.Header{Name: e.name, Typeflag: typ, Mode: 0o600, Size: int64(len(e.data)), ModTime: time.Unix(0, 0)}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func manifestFor(t *testing.T, files ...ManifestFile) tarEntry {
	t.Helper()
	b, err := json.Marshal(Manifest{Files: files})
	if err != nil {
		t.Fatal(err)
	}
	return tarEntry{name: ManifestName, data: b}
}

func mf(path string, data []byte) ManifestFile {
	s := sha256.Sum256(data)
	return ManifestFile{Path: path, Size: int64(len(data)), SHA256: hex.EncodeToString(s[:]), Stored: true}
}

func TestParseFilesArchiveRefusesWhatARestoreMustNeverWrite(t *testing.T) {
	// Why (plan 1.5): a hand-edited tar must not write ../ or a dot file anywhere, nor feed content
	// the manifest does not vouch for.
	good := []byte("hello")
	for name, data := range map[string][]byte{
		"traversal":         rawTar(t, manifestFor(t, mf("/../evil", good)), tarEntry{"../evil", good, 0}),
		"dot file":          rawTar(t, manifestFor(t, mf("/.settings", good)), tarEntry{".settings", good, 0}),
		"hash mismatch":     rawTar(t, manifestFor(t, mf("/a.be", good)), tarEntry{"a.be", []byte("HELLO"), 0}),
		"unlisted entry":    rawTar(t, manifestFor(t, mf("/a.be", good)), tarEntry{"a.be", good, 0}, tarEntry{"extra.be", good, 0}),
		"missing entry":     rawTar(t, manifestFor(t, mf("/a.be", good))),
		"no manifest first": rawTar(t, tarEntry{"a.be", good, 0}, manifestFor(t, mf("/a.be", good))),
		"symlink":           rawTar(t, manifestFor(t, mf("/a.be", good)), tarEntry{"a.be", nil, tar.TypeSymlink}),
		"duplicate":         rawTar(t, manifestFor(t, mf("/a.be", good), mf("/a.be", good)), tarEntry{"a.be", good, 0}, tarEntry{"a.be", good, 0}),
		"not a tar":         []byte("this is not a tar archive at all"),
	} {
		if _, err := ParseFilesArchive(data); !errors.Is(err, ErrBadArchive) {
			t.Errorf("%s: got %v, want ErrBadArchive", name, err)
		}
	}
	if _, err := ParseFilesArchive(rawTar(t, manifestFor(t, mf("/a.be", good)), tarEntry{"a.be", good, 0})); err != nil {
		t.Errorf("a good archive is refused: %v", err)
	}
}

func TestParseFilesArchiveFlagsTheKeyWhateverTheManifestSays(t *testing.T) {
	// Why: the "private key" mark protects the UI from showing it; a manifest that clears the flag
	// must not be able to hide the key.
	key := []byte("KEY")
	m := mf("/tlskey", key)
	m.PrivateKey = false
	a, err := ParseFilesArchive(rawTar(t, manifestFor(t, m), tarEntry{"tlskey", key, 0}))
	if err != nil || !a.Manifest.Files[0].PrivateKey {
		t.Fatalf("%+v, %v", a, err)
	}
	if !IsPrivateKey("/tlskey") || IsPrivateKey("/lib/tlskey") || IsPrivateKey("/tlskey2") {
		t.Error("IsPrivateKey must match exactly /tlskey")
	}
}

func TestFilesNoteOf(t *testing.T) {
	for in, want := range map[string]string{
		"":                                "",
		"decoder outdated":                "",
		NoteFilesPrefix + "no filesystem": "no filesystem",
		"Nightly; " + NoteFilesPrefix + "they need HTTP": "they need HTTP",
	} {
		if got := FilesNoteOf(in); got != want {
			t.Errorf("FilesNoteOf(%q) = %q, want %q", in, got, want)
		}
	}
}

var _ io.Writer = (*bytes.Buffer)(nil)

func TestAnEmptyFilesystemStillGetsAnArchiveWithAManifest(t *testing.T) {
	// Why: "the file system was empty at this time" is a fact a later restore or comparison needs;
	// it must not look like "files were never read".
	e := newFilesEnv(t, esp32(map[string]simulator.FSFile{}), nil)
	got, err := e.m.Backup(t.Context(), "alice", 7)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := kindOf(got, model.BackupConfigDump)
	a, err := e.m.OpenFilesArchive(t.Context(), d.ID)
	if err != nil || len(a.Manifest.Files) != 0 || a.Manifest.FSType != tasmota.FSLittleFS {
		t.Fatalf("%+v, %v", a, err)
	}
}

// ---- review findings (PR #424)

func TestDeadlineInTheFilesStepKeepsTheDump(t *testing.T) {
	// Why (H1): the pre-OTA backup runs under a 30 s deadline; a stalling download used to end the
	// whole run with the context error, losing the dump and stopping the update. The files are best
	// effort: the walk gets its own, earlier deadline and the run goes on.
	files := map[string]simulator.FSFile{"/a.be": {Data: []byte("a")}, "/b.be": {Data: []byte("b")}, "/c.be": {Data: []byte("c")}}
	e := newFilesEnv(t, esp32(files), nil)
	e.flaky.stall = map[string]bool{"/b.be": true}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	start := time.Now()
	got, err := e.m.Backup(ctx, "alice", 7)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if time.Since(start) > 3500*time.Millisecond {
		t.Errorf("the walk used the whole deadline (%v)", time.Since(start))
	}
	if _, ok := kindOf(got, model.BackupConfigDump); !ok {
		t.Fatal("dump lost")
	}
	d, _ := kindOf(got, model.BackupConfigDump)
	a, err := e.m.OpenFilesArchive(t.Context(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := skippedReasons(a.Manifest)
	if len(a.Manifest.Files) != 1 || r["/b.be"] != "time limit of the backup reached" || r["/c.be"] != "not read: time limit of the backup reached" {
		t.Errorf("stored %+v skipped %+v", a.Manifest.Files, r)
	}
	if len(e.st.attempts) != 0 {
		t.Errorf("a Failed row was recorded: %+v", e.st.attempts)
	}
}

func TestDeadlineBeforeTheListingGivesANoteNotAFailure(t *testing.T) {
	// Why (H1): same rule when the very first file call hangs.
	e := newFilesEnv(t, esp32(nil), nil)
	e.flaky.infoStall = true
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	got, err := e.m.Backup(ctx, "alice", 7)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if n := dumpNote(t, got); !strings.Contains(n, NoteFilesPrefix+"time limit of the backup reached") {
		t.Errorf("dump note %q", n)
	}
}

func TestACancelledBackupStillStopsInTheFilesStep(t *testing.T) {
	// Why (H1): only the caller's own context ends the run (shutdown, cancelled job); that is not
	// the device's fault and records no Failed row.
	e := newFilesEnv(t, esp32(map[string]simulator.FSFile{"/a.be": {Data: []byte("a")}}), nil)
	e.flaky.stall = map[string]bool{"/a.be": true}
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(300*time.Millisecond, cancel)
	if _, err := e.m.Backup(ctx, "alice", 7); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if len(e.st.attempts) != 0 {
		t.Errorf("attempts: %+v", e.st.attempts)
	}
}

// failFilesStore refuses to index the files row, as a full disk or a broken database would.
type failFilesStore struct{ *memBackups }

func (s failFilesStore) PutBackup(ctx context.Context, b model.Backup) (model.BackupID, error) {
	if b.Kind == model.BackupFiles {
		return 0, errors.New("index broke")
	}
	return s.memBackups.PutBackup(ctx, b)
}

func TestAnArchiveThatCannotBeIndexedNeverFailsTheRun(t *testing.T) {
	// Why (M1): the dump and snapshot are already stored; losing the run over the best-effort
	// archive would also stop a pre-OTA backup. The orphan file goes, the reason goes to the audit.
	e := newFilesEnv(t, esp32(nil), func(o *Options) { o.Store = failFilesStore{o.Store.(*memBackups)} })
	got, err := e.m.Backup(t.Context(), "alice", 7)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("rows %+v", got)
	}
	left, _ := filepath.Glob(filepath.Join(e.dir, "backups", "*", "*.files.tar"))
	if len(left) != 0 {
		t.Errorf("unindexed archive left on disk: %v", left)
	}
	var detail map[string]any
	last := e.audit.ev[len(e.audit.ev)-1]
	if err := json.Unmarshal([]byte(last.Detail), &detail); err != nil || detail["files_error"] == nil || last.Result != "ok" {
		t.Errorf("audit %+v (%v)", last, err)
	}
}

func TestDownloadsStopAtTheRemainingRunBudget(t *testing.T) {
	// Why (M3): the listing can understate a size (a file growing between list and read); the cap
	// must hold for the bytes actually read, so each download gets the remaining budget as its limit.
	files := map[string]simulator.FSFile{}
	for i := 0; i < 5; i++ {
		files[fmt.Sprintf("/f%d.bin", i)] = simulator.FSFile{Data: bytes.Repeat([]byte{'z'}, 900<<10)}
	}
	e := newFilesEnv(t, esp32(files), nil)
	e.flaky.listMod = func(_ string, es []tasmota.FileEntry) []tasmota.FileEntry {
		for i := range es {
			es[i].Size = 1
		}
		return es
	}
	got, err := e.m.Backup(t.Context(), "alice", 7)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := kindOf(got, model.BackupConfigDump)
	a, _ := e.m.OpenFilesArchive(t.Context(), d.ID)
	if len(a.Manifest.Files) != 4 {
		t.Fatalf("stored %d files", len(a.Manifest.Files))
	}
	want := MaxRunBytes - 4*(900<<10)
	if last := e.flaky.maxes[len(e.flaky.maxes)-1]; last != want {
		t.Errorf("last download limit %d, want the remaining budget %d", last, want)
	}
	if first := e.flaky.maxes[0]; first != MaxFileBytes {
		t.Errorf("first download limit %d, want %d", first, MaxFileBytes)
	}
	if r := skippedReasons(a.Manifest)["/f4.bin"]; r != "run limit of 4 MiB reached" {
		t.Errorf("reason %q", r)
	}
}

func TestDuplicateNamesInAListingAreWrittenOnce(t *testing.T) {
	// Why (L3): a duplicate would put two tar entries of one name next to one manifest line, and
	// ParseFilesArchive (rightly) refuses such an archive: the whole run's files would be unusable.
	e := newFilesEnv(t, esp32(map[string]simulator.FSFile{"/a.be": {Data: []byte("a")}}), nil)
	e.flaky.listMod = func(_ string, es []tasmota.FileEntry) []tasmota.FileEntry { return append(es, es...) }
	got, err := e.m.Backup(t.Context(), "alice", 7)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := kindOf(got, model.BackupConfigDump)
	a, err := e.m.OpenFilesArchive(t.Context(), d.ID)
	if err != nil || len(a.Manifest.Files) != 1 || len(e.flaky.downloads) != 1 {
		t.Fatalf("%+v, %v, downloads %v", a, err, e.flaky.downloads)
	}
}

func TestAddressRefusedCountsAsTheDeviceBeingGone(t *testing.T) {
	// Why (L1): a refused address or API will refuse the next file too; do not ask 99 more times.
	for name, err := range map[string]error{"outside networks": tasmota.ErrForbiddenAddr, "api refused": tasmota.ErrHTTPAPIRefused} {
		files := map[string]simulator.FSFile{}
		for i := 0; i < 5; i++ {
			files[fmt.Sprintf("/f%d", i)] = simulator.FSFile{Data: []byte("x")}
		}
		e := newFilesEnv(t, esp32(files), nil)
		e.flaky.getErr = map[string]error{"/f0": err}
		if _, berr := e.m.Backup(t.Context(), "alice", 7); berr != nil {
			t.Fatal(berr)
		}
		if len(e.flaky.downloads) != 1 {
			t.Errorf("%s: %d downloads", name, len(e.flaky.downloads))
		}
	}
}

// mqttDevice is the command channel of a device that is only reachable over MQTT.
type mqttDevice struct{ dump []byte }

func (m mqttDevice) Do(context.Context, model.Device, string) (json.RawMessage, error) {
	return json.RawMessage(`{"Status":{}}`), nil
}
func (m mqttDevice) DoLoose(ctx context.Context, d model.Device, c string) (json.RawMessage, error) {
	return m.Do(ctx, d, c)
}
func (m mqttDevice) Connected() bool { return true }
func (m mqttDevice) Download(context.Context, model.Device, string) ([]byte, error) {
	return m.dump, nil
}

type mqttFacts struct{ mem *transport.Memory }

func (f mqttFacts) HTTPState(id model.DeviceID) transport.HTTPState {
	if kind, since, ok := f.mem.HTTPFailure(id); ok {
		return transport.HTTPState{State: transport.HTTPFailing, Kind: kind, Since: since}
	}
	return transport.HTTPState{}
}

func (f mqttFacts) FactsFor(_ context.Context, d model.Device) (transport.Facts, error) {
	now := time.Now()
	return transport.Facts{Now: now, HTTP: f.HTTPState(d.ID),
		MQTT: transport.MQTTFacts{BusConnected: true, Ours: true, Topic: d.Topic, TopicExclusive: true, LWTOnline: true,
			GroupTopic: "tasmotas", GroupTopicReadAt: now, LastProbeAt: now.Add(-time.Hour)},
		Password: transport.Password{Mode: d.MQTTPasswordMode}}, nil
}

// httpSpy is the raw HTTP client of a device that does not answer; it counts file calls.
type httpSpy struct {
	tasmota.Client
	mu    sync.Mutex
	calls []string
}

func (h *httpSpy) rec(c string) { h.mu.Lock(); h.calls = append(h.calls, c); h.mu.Unlock() }
func (h *httpSpy) Command(context.Context, tasmota.Target, string) (json.RawMessage, error) {
	h.rec("command")
	return nil, tasmota.ErrUnreachable
}
func (h *httpSpy) DownloadConfig(context.Context, tasmota.Target) ([]byte, error) {
	h.rec("dl")
	return nil, tasmota.ErrUnreachable
}
func (h *httpSpy) FSInfo(context.Context, tasmota.Target) (tasmota.FSInfo, error) {
	h.rec("fsinfo")
	return tasmota.FSInfo{}, tasmota.ErrUnreachable
}
func (h *httpSpy) ListFiles(context.Context, tasmota.Target, string) ([]tasmota.FileEntry, error) {
	h.rec("list")
	return nil, tasmota.ErrUnreachable
}
func (h *httpSpy) DownloadFile(context.Context, tasmota.Target, string, int64) ([]byte, error) {
	h.rec("get")
	return nil, tasmota.ErrUnreachable
}
func (h *httpSpy) UploadFile(context.Context, tasmota.Target, string, []byte) error {
	h.rec("put")
	return tasmota.ErrUnreachable
}

func TestBackupOverMQTTThroughTheRealRouterSendsNoFileCallToHTTP(t *testing.T) {
	// Why (M4): when the dump came over MQTT because HTTP is down, the files step must not try HTTP
	// (each call would wait for a timeout) and must say "they need HTTP". Real Router, real trace.
	mem := transport.NewMemory(transport.DefaultFailFor, time.Now)
	mem.Fail(7, transport.HTTPFailUnreachable)
	dev := model.Device{ID: 7, Name: "plug", Address: "192.0.2.7", Topic: "plug", MQTT: model.MQTTOurs,
		MQTTPasswordMode: model.MQTTPasswordOwn, MQTTPassword: "pw", Identity: model.Identity{Version: "16.0.0"}}
	spy := &httpSpy{}
	rt := transport.NewRouter(transport.Options{HTTP: spy, Devices: devs{dev}, Facts: mqttFacts{mem}, Memory: mem})
	rt.SetMQTT(mqttDevice{dump: []byte("DUMP")})
	e := newEnv(t, func(o *Options) {
		o.Devices = devs{dev}
		o.Client = rt
		o.Decoder = nil
	})
	got, err := e.m.Backup(t.Context(), "alice", 7)
	if err != nil {
		t.Fatal(err)
	}
	if n := dumpNote(t, got); !strings.Contains(n, NoteFilesPrefix+"they need HTTP") {
		t.Errorf("dump note %q", n)
	}
	for _, c := range spy.calls {
		if c != "command" && c != "dl" { // anything but what HTTP-first routing may try once is a file call
			t.Errorf("file call reached HTTP: %v", spy.calls)
		}
	}
}
