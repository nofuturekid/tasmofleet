//go:build integration

package backup

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

const syntheticDump = "testdata/synthetic/synthetic-15.6.0.3.dmp"

// realDecoder returns an ExecDecoder for DECODER_PATH. The CI image carries
// the pinned decode-config; elsewhere the test is skipped loudly (plan WP8b,
// spec §21: skipped only when DECODER_PATH is missing).
func realDecoder(t *testing.T) *ExecDecoder {
	t.Helper()
	p := os.Getenv("DECODER_PATH")
	if p == "" {
		// CI sets REQUIRE_DECODER=1: there a missing decoder is a failure,
		// because a skipped gate is not a green gate.
		if os.Getenv("REQUIRE_DECODER") == "1" {
			t.Fatal("REQUIRE_DECODER=1 but DECODER_PATH is not set")
		}
		t.Skip("DECODER_PATH not set: only a local run skips; CI sets REQUIRE_DECODER=1 (ci.yml) and fails above instead")
	}
	tmp := t.TempDir()
	return NewExecDecoder(p, tmp)
}

type simEnv struct {
	*env
	sim simulator.Controller
}

// newSimEnv wires the Manager to a simulated device through the real HTTP
// client, so /dl, the Status queries and the SSRF guard are all exercised.
func newSimEnv(t *testing.T, version string, dec Decoder, mutate func(*Options)) *simEnv {
	t.Helper()
	dump, err := os.ReadFile(syntheticDump)
	if err != nil {
		t.Fatal(err)
	}
	fleet, err := simulator.NewFleet(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() })
	dev, err := fleet.Add(t.Context(), simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_4M, Version: version})
	if err != nil {
		t.Fatal(err)
	}
	ctl, ok := simulator.Control(dev)
	if !ok {
		t.Fatal("simulator device has no controller")
	}
	ctl.SetConfigDump(dump)
	host, portStr, err := net.SplitHostPort(dev.Addr())
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)
	client := tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	e := newEnv(t, func(o *Options) {
		o.Devices = devs{model.Device{ID: 7, Name: "plug", Address: host, Port: port}}
		o.Client = client
		o.Decoder = dec
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
		if mutate != nil {
			mutate(o)
		}
	})
	return &simEnv{env: e, sim: ctl}
}

func byKind(bs []model.Backup, k model.BackupKind) (model.Backup, bool) {
	for _, b := range bs {
		if b.Kind == k {
			return b, true
		}
	}
	return model.Backup{}, false
}

// Why: the raw .dmp is the only restore input, so it must come back
// byte-identical from the device, and the decoded JSON must really be what the
// pinned decode-config makes of it (flags pinned here, spec §12.2).
func TestRealDecoderBackupViaSimulator(t *testing.T) {
	dec := realDecoder(t)
	s := newSimEnv(t, "15.6.0", dec, nil)

	v, err := dec.Version(t.Context())
	if err != nil || !strings.HasPrefix(v, "16.") {
		t.Fatalf("decoder version %q, %v (the image pins v16.0.0)", v, err)
	}
	got, err := s.m.Backup(t.Context(), "tester", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want dump+snapshot+decoded, got %d: %+v", len(got), got)
	}

	want, _ := os.ReadFile(syntheticDump)
	raw, meta, err := s.m.Open(t.Context(), mustKind(t, got, model.BackupConfigDump).ID)
	if err != nil || !bytes.Equal(raw, want) || withoutFilesNote(meta.Note) != "" {
		t.Fatalf("dump differs from what the device served: %v note=%q", err, meta.Note)
	}

	dm := mustKind(t, got, model.BackupDecoded)
	if dm.DecoderVersion != v {
		t.Errorf("decoded JSON stamped %q, decoder is %q", dm.DecoderVersion, v)
	}
	body, _, err := s.m.Open(t.Context(), dm.ID)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{
		"hostname": "synthetic-plug", "mqtt_host": "mqtt.example.invalid", "mqtt_topic": "synthetic_plug",
		"mqtt_pwd": "synthetic-mqtt-pass-0002", "web_password": "synthetic-web-pass-0003", "tele_period": float64(300),
	} {
		if cfg[k] != want {
			t.Errorf("%s = %v, want %v", k, cfg[k], want)
		}
	}

	// Secrets live in these files: nothing may be readable by others.
	for _, b := range got {
		st, err := os.Stat(filepath.Join(s.dir, filepath.FromSlash(b.Path)))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v mode %v, want 0600", b.Path, err, st.Mode())
		}
	}
	for _, d := range []string{"backups", filepath.Join("backups", "plug")} {
		st, err := os.Stat(filepath.Join(s.dir, d))
		if err != nil || st.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v mode %v, want 0700", d, err, st.Mode())
		}
	}
	if left, _ := filepath.Glob(filepath.Join(s.dir, "backups", "plug", ".tmp-*")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

// withoutFilesNote drops the files sentence a run's dump note ends with ("files not backed up: no
// filesystem" for this ESP8266 simulator): these tests are about the decoder's notes.
func withoutFilesNote(n string) string {
	i := strings.LastIndex(n, NoteFilesPrefix)
	if i < 0 {
		return n
	}
	return strings.TrimSuffix(strings.TrimSpace(n[:i]), ";")
}

func mustKind(t *testing.T, bs []model.Backup, k model.BackupKind) model.Backup {
	t.Helper()
	b, ok := byKind(bs, k)
	if !ok {
		t.Fatalf("no %s file in %+v", k, bs)
	}
	return b
}

// Why: decode-config misreads settings newer than it knows, so a device with
// newer firmware must keep only the dump and snapshot and be flagged instead
// of storing wrong JSON.
func TestRealDecoderOutdatedWhenDeviceIsNewer(t *testing.T) {
	dec := realDecoder(t)
	s := newSimEnv(t, "99.0.0", dec, nil)
	got, err := s.m.Backup(t.Context(), "tester", 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := byKind(got, model.BackupDecoded); ok {
		t.Error("decoded JSON stored although the decoder is older than the device")
	}
	if d := mustKind(t, got, model.BackupConfigDump); withoutFilesNote(d.Note) != NoteDecoderOutdated {
		t.Errorf("dump note %q, want %q", d.Note, NoteDecoderOutdated)
	}
}

// Why: a corrupt dump (flipped byte breaks the CRC) must not fail the backup:
// the raw dump is still the authority and is kept, flagged "decode failed".
func TestRealDecoderRejectsCorruptDumpButKeepsIt(t *testing.T) {
	dec := realDecoder(t)
	s := newSimEnv(t, "15.6.0", dec, nil)
	dump, _ := os.ReadFile(syntheticDump)
	dump[0x400] ^= 0xFF
	s.sim.SetConfigDump(dump)
	got, err := s.m.Backup(t.Context(), "tester", 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := byKind(got, model.BackupDecoded); ok {
		t.Error("decoded JSON stored for a corrupt dump")
	}
	if d := mustKind(t, got, model.BackupConfigDump); withoutFilesNote(d.Note) != NoteDecodeFailed {
		t.Errorf("dump note %q, want %q", d.Note, NoteDecodeFailed)
	}
}

// Why: a hung decoder must cost at most its timeout and never block or fail
// the backup (the device's dump is already in hand).
func TestSlowDecoderStubDoesNotBlockBackup(t *testing.T) {
	stub := writeScript(t, `case "$1" in --version) echo "decode-config v16.0.0.0"; exit 0;; esac; sleep 30`)
	dec := NewExecDecoder(stub, t.TempDir())
	dec.Timeout = 300 * time.Millisecond
	s := newSimEnv(t, "15.6.0", dec, nil)
	start := time.Now()
	got, err := s.m.Backup(t.Context(), "tester", 7)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("backup took %v", time.Since(start))
	}
	if d := mustKind(t, got, model.BackupConfigDump); withoutFilesNote(d.Note) != NoteDecodeFailed {
		t.Errorf("note %q, want %q", d.Note, NoteDecodeFailed)
	}
}

// Why: retention is about disk and secrets - pruned runs must disappear from
// disk (all three files), young runs survive MaxAge, and the survivors still
// open (checksum intact).
func TestRetentionOnDiskWithRealFiles(t *testing.T) {
	dec := realDecoder(t)
	s := newSimEnv(t, "15.6.0", dec, func(o *Options) { o.Keep = 1; o.MaxAge = 36 * time.Hour })
	// Runs 72 h, 48 h, 24 h and 0 h old at the last backup. Keep 1 retains only
	// the newest; MaxAge 36 h additionally protects the 24 h run, so the 48 h
	// and 72 h runs go. Without MaxAge the 24 h run would be lost too.
	var all [][]model.Backup
	for _, age := range []time.Duration{72, 48, 24, 0} {
		s.clk.t = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC).Add(-age * time.Hour)
		got, err := s.m.Backup(t.Context(), "tester", 7)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, got)
	}
	entries, _ := filepath.Glob(filepath.Join(s.dir, "backups", "plug", "*"))
	if len(entries) != 6 {
		t.Fatalf("want 2 runs (newest + MaxAge-protected) x 3 files on disk, got %d: %v", len(entries), entries)
	}
	for _, b := range all[3] {
		if _, _, err := s.m.Open(t.Context(), b.ID); err != nil {
			t.Errorf("newest run %s: %v", b.Kind, err)
		}
	}
	for _, b := range all[2] {
		if _, _, err := s.m.Open(t.Context(), b.ID); err != nil {
			t.Errorf("24 h run %s must survive via MaxAge: %v", b.Kind, err)
		}
	}
	for _, b := range append(all[0], all[1]...) {
		if _, _, err := s.m.Open(t.Context(), b.ID); err == nil {
			t.Errorf("old run %s still openable after pruning", b.Kind)
		}
	}
}

// Why: decoded-secrets.json feeds the leak tests of the diff; it must be what
// the pinned decoder really makes of the synthetic dump with a fake value in
// every secret field, and the diff of the real outputs must carry no value.
func TestRealDecoderSecretsDumpMatchesFixtureAndDiffLeaksNothing(t *testing.T) {
	dec := realDecoder(t)
	dmp, err := os.ReadFile("testdata/synthetic/synthetic-secrets-15.6.0.3.dmp")
	if err != nil {
		t.Fatal(err)
	}
	got, err := dec.Decode(t.Context(), dmp)
	if err != nil {
		t.Fatal(err)
	}
	base, err := dec.Decode(t.Context(), mustRead(t, syntheticDump))
	if err != nil {
		t.Fatal(err)
	}
	fixture, _ := os.ReadFile("testdata/synthetic/decoded-secrets.json")
	if ch, err := Diff(fixture, got); err != nil || len(ch) != 0 {
		t.Errorf("fixture drifted from the real decoder output: %v %v", ch, err)
	}
	changes, err := Diff(base, got)
	if err != nil {
		t.Fatal(err)
	}
	flat := fmtChanges(changes)
	for _, leak := range []string{"FAKE-SECRET", "4242", "777001", "0x5ec7", "ouser"} {
		if strings.Contains(flat, leak) {
			t.Errorf("real diff output contains %q", leak)
		}
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
