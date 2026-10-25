package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/audit"
	"github.com/nofuturekid/tasmofleet/internal/clock"
	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// Notes set on stored backups.
const (
	NoteDecoderOutdated = "decoder outdated"
	NoteDecodeFailed    = "decode failed"
)

// ErrIntegrity is returned by Open when a file no longer matches its SHA-256.
var ErrIntegrity = errors.New("backup: stored file does not match its checksum")

const defaultKeep = 10

// outFile is one file of a backup run.
type outFile struct {
	kind    model.BackupKind
	ext     string
	data    []byte
	note    string
	decoder string
}

// snapshotCommands are queried one at a time. Status 0 is never sent (heap
// bursts corrupt ESP8266 replies) and Status 12 is skipped.
var snapshotCommands = []struct{ key, cmnd string }{
	{"Status1", "Status 1"}, {"Status2", "Status 2"}, {"Status3", "Status 3"}, {"Status4", "Status 4"},
	{"Status5", "Status 5"}, {"Status6", "Status 6"}, {"Status7", "Status 7"}, {"Status8", "Status 8"},
	{"Status9", "Status 9"}, {"Status10", "Status 10"}, {"Status11", "Status 11"},
	{"Template", "Template"}, {"Module", "Module"},
	{"Rule1", "Rule1"}, {"Rule2", "Rule2"}, {"Rule3", "Rule3"}, {"Timers", "Timers"},
}

// Publisher receives backup events (satisfied by *events.Hub).
type Publisher interface {
	Publish(e events.Event)
}

// DeviceGetter is the part of store.DeviceStore the service needs.
type DeviceGetter interface {
	GetDevice(ctx context.Context, id model.DeviceID) (model.Device, error)
}

// Options configure the service.
type Options struct {
	DataDir string // backups live in <DataDir>/backups/<device-id>/
	Devices DeviceGetter
	Store   store.BackupStore
	Client  tasmota.Client
	Decoder Decoder // optional; nil disables decoded backups
	Keep    int     // newest runs kept per device, default 10
	MaxAge  time.Duration
	Audit   audit.Recorder // optional
	Events  Publisher      // optional
	Clock   clock.Clock    // default clock.Real
	Log     *slog.Logger   // default slog.Default()
}

// Manager implements Service.
type Manager struct {
	o     Options
	ret   atomic.Pointer[retention] // keep and max age in force; replaced by SetRetention
	mu    sync.Mutex
	locks map[model.DeviceID]*sync.Mutex
}

var _ Service = (*Manager)(nil)

// New returns the backup service.
func New(o Options) (*Manager, error) {
	if o.DataDir == "" || o.Devices == nil || o.Store == nil || o.Client == nil {
		return nil, errors.New("backup: DataDir, Devices, Store and Client are required")
	}
	if o.Keep <= 0 {
		o.Keep = defaultKeep
	}
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	m := &Manager{o: o, locks: map[model.DeviceID]*sync.Mutex{}}
	m.ret.Store(&retention{keep: o.Keep, maxAge: o.MaxAge})
	return m, nil
}

type retention struct {
	keep   int
	maxAge time.Duration
}

// SetRetention changes the pruning rule while the service runs (the settings
// page). keep < 1 is ignored. It applies from the next pruning.
func (m *Manager) SetRetention(keep int, maxAge time.Duration) {
	if keep < 1 {
		return
	}
	m.ret.Store(&retention{keep: keep, maxAge: maxAge})
}

func (m *Manager) deviceLock(id model.DeviceID) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.locks[id]
	if l == nil {
		l = &sync.Mutex{}
		m.locks[id] = l
	}
	return l
}

// Backup takes a dump + snapshot (+ decoded JSON) of one device. The dump is
// authoritative: if it cannot be fetched nothing is stored and an error is
// returned. Snapshot and decoding problems never fail the run; they are
// flagged in the Note of the stored backups. Runs for one device are
// serialised (ESP8266 web servers are single-threaded).
func (m *Manager) Backup(ctx context.Context, actor string, id model.DeviceID) ([]model.Backup, error) {
	l := m.deviceLock(id)
	l.Lock()
	defer l.Unlock()

	dev, err := m.o.Devices.GetDevice(ctx, id)
	if err != nil {
		return nil, err
	}
	return m.finishBackup(ctx, actor, dev, dev, "")
}

// NoteReplacementHardware marks the dump of replacement hardware taken before
// "Replace device" restored another device's configuration onto it. The
// backup is filed under the entry it is about to join, but it is not a
// backup of the broken device: Replace never offers it as that device's
// newest configuration.
const NoteReplacementHardware = "replacement hardware, before replace"

// BackupOf backs up src, which need not be (or not yet be) the inventory
// entry owner, and files the result under owner. Used by "Replace device":
// the replacement hardware is backed up before it receives the old
// configuration, and the result belongs to the entry that takes it over. note
// is stored on the dump record. Runs for one owner are serialised like Backup.
func (m *Manager) BackupOf(ctx context.Context, actor string, owner model.DeviceID, src model.Device, note string) ([]model.Backup, error) {
	l := m.deviceLock(owner)
	l.Lock()
	defer l.Unlock()
	o, err := m.o.Devices.GetDevice(ctx, owner)
	if err != nil {
		return nil, err
	}
	return m.finishBackup(ctx, actor, o, src, note)
}

// finishBackup runs a backup of dev filed under owner, audits it and prunes.
// A failed run is recorded as an attempt (shown as a "Failed" row). The
// caller holds the device lock.
func (m *Manager) finishBackup(ctx context.Context, actor string, owner, dev model.Device, note string) ([]model.Backup, error) {
	id := owner.ID
	run := runOf(ctx)
	if run.Label != "" {
		note = strings.TrimSuffix(strings.TrimSpace(run.Label+"; "+note), ";")
	}
	out, version, err := m.backup(ctx, owner, dev, note, run)
	detail := map[string]any{"files": len(out), "firmware": version, "trigger": run.Trigger}
	if run.Keep {
		detail["kept"] = true
	}
	for _, b := range out {
		if b.Note != "" {
			detail["note_"+string(b.Kind)] = b.Note
		}
	}
	if m.o.Audit != nil {
		e := model.AuditEvent{Actor: actor, Action: "backup.create", Target: fmt.Sprintf("device:%d", id), Result: "ok"}
		if b, merr := json.Marshal(detail); merr == nil {
			e.Detail = string(b)
		}
		if err != nil {
			e.Result = err.Error()
		}
		m.o.Audit.Record(ctx, e)
	}
	if err != nil {
		m.recordAttempt(ctx, id, run, version, dev, err)
		return out, err
	}
	m.prune(ctx, id)
	if m.o.Events != nil {
		for _, b := range out {
			m.o.Events.Publish(events.Event{Topic: events.TopicBackup, At: m.o.Clock.Now().UTC(), Payload: b})
		}
	}
	return out, nil
}

// LiveSnapshot reads the Status 1..11 snapshot of a device without storing
// anything (the "changes since the last backup" view). It takes the same
// per-device lock as Backup, so it never overlaps a backup on the
// single-threaded device web server.
func (m *Manager) LiveSnapshot(ctx context.Context, id model.DeviceID) ([]byte, error) {
	l := m.deviceLock(id)
	l.Lock()
	defer l.Unlock()
	dev, err := m.o.Devices.GetDevice(ctx, id)
	if err != nil {
		return nil, err
	}
	data, _, _, err := m.snapshot(ctx, tasmota.TargetFor(dev))
	return data, err
}

// recordAttempt stores a failed backup. A run cut short by shutdown is not a
// failure of the device and is not recorded.
func (m *Manager) recordAttempt(ctx context.Context, id model.DeviceID, run Run, version string, dev model.Device, err error) {
	if ctx.Err() != nil {
		return
	}
	if version == "" {
		version = dev.Identity.Version
	}
	a := model.BackupAttempt{DeviceID: id, At: m.o.Clock.Now().UTC(), Trigger: run.Trigger, TriggerRef: run.Ref, Version: version, Reason: AttemptReason(err)}
	if _, aerr := m.o.Store.AddBackupAttempt(ctx, a); aerr != nil {
		m.o.Log.Warn("backup: record failed attempt", "component", "backup", "device", id, "err", aerr)
	}
}

func (m *Manager) backup(ctx context.Context, owner, dev model.Device, note string, run Run) ([]model.Backup, string, error) {
	id := owner.ID
	target := tasmota.TargetFor(dev)

	dmp, err := m.o.Client.DownloadConfig(ctx, target)
	if err != nil {
		return nil, "", fmt.Errorf("backup: download config: %w", err)
	}
	if len(dmp) == 0 {
		return nil, "", errEmptyDump
	}

	snap, failed, fwVersion, err := m.snapshot(ctx, target)
	if err != nil {
		return nil, "", err
	}
	if fwVersion == "" {
		fwVersion = dev.Identity.Version
	}

	dmpNote, decoded, decoderVersion := m.decode(ctx, dmp, fwVersion)
	if note != "" {
		dmpNote = strings.TrimSpace(note + "; " + dmpNote)
		dmpNote = strings.TrimSuffix(dmpNote, ";")
	}

	dir := filepath.Join(m.o.DataDir, "backups", fmt.Sprint(int64(id)))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fwVersion, fmt.Errorf("backup: %w", err)
	}
	taken := m.freeTimestamp(dir)

	files := []outFile{
		{model.BackupConfigDump, "dmp", dmp, dmpNote, ""},
		{model.BackupSnapshot, "json", snap, snapNote(failed), ""},
	}
	if decoded != nil {
		files = append(files, outFile{model.BackupDecoded, "json", decoded, "", decoderVersion})
	}

	var stored []model.Backup
	for _, f := range files {
		name := taken.Format("20060102T150405Z") + "-" + string(f.kind) + "." + f.ext
		if err := writeAtomic(dir, name, f.data); err != nil {
			return stored, fwVersion, fmt.Errorf("backup: write %s: %w", f.kind, err)
		}
		sum := sha256.Sum256(f.data)
		b := model.Backup{
			DeviceID: id, DeviceName: owner.Name, DeviceMAC: owner.MAC, Kept: run.Keep, Trigger: run.Trigger, TriggerRef: run.Ref,
			Kind: f.kind, TakenAt: taken, Version: fwVersion,
			Path: path.Join("backups", fmt.Sprint(int64(id)), name), Size: int64(len(f.data)),
			SHA256: hex.EncodeToString(sum[:]), DecoderVersion: f.decoder, Note: f.note,
		}
		bid, err := m.o.Store.PutBackup(ctx, b)
		if err != nil {
			_ = os.Remove(filepath.Join(dir, name)) // unindexed file would never be pruned
			return stored, fwVersion, fmt.Errorf("backup: index %s: %w", f.kind, err)
		}
		b.ID = bid
		stored = append(stored, b)
	}
	return stored, fwVersion, nil
}

// freeTimestamp returns now (UTC, whole seconds), moved forward until no file
// of an earlier run in dir carries the same stamp.
func (m *Manager) freeTimestamp(dir string) time.Time {
	t := m.o.Clock.Now().UTC().Truncate(time.Second)
	for {
		_, err := os.Stat(filepath.Join(dir, t.Format("20060102T150405Z")+"-"+string(model.BackupConfigDump)+".dmp"))
		if err != nil {
			return t
		}
		t = t.Add(time.Second)
	}
}

// snapshot queries the status commands one by one. A failing command is
// recorded in place and listed in failed; context errors abort the run.
func (m *Manager) snapshot(ctx context.Context, t tasmota.Target) (data []byte, failed []string, fwVersion string, err error) {
	doc := map[string]json.RawMessage{}
	for _, c := range snapshotCommands {
		if err := ctx.Err(); err != nil {
			return nil, nil, "", err
		}
		raw, cerr := m.o.Client.Command(ctx, t, c.cmnd)
		switch {
		case c.key == "Status9" && errors.Is(cerr, tasmota.ErrCommandError):
			// StatusPTH exists only on devices with energy monitoring; the
			// others answer {"Command":"Error"} by design, so nothing is missing.
			doc[c.key] = errorValue(cerr)
		case cerr != nil:
			failed = append(failed, c.cmnd)
			doc[c.key] = errorValue(cerr)
		case !json.Valid(raw):
			failed = append(failed, c.cmnd)
			doc[c.key] = errorValue(tasmota.ErrBadResponse)
		default:
			doc[c.key] = raw
			if c.key == "Status2" {
				fwVersion = firmwareVersion(raw)
			}
		}
	}
	data, err = json.MarshalIndent(doc, "", "  ")
	return data, failed, fwVersion, err
}

func errorValue(err error) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"_error": err.Error()}) // map of strings cannot fail to encode
	return b
}

func snapNote(failed []string) string {
	if len(failed) == 0 {
		return ""
	}
	return "snapshot incomplete: " + strings.Join(failed, ", ")
}

// firmwareVersion extracts "16.0.0" from {"StatusFWR":{"Version":"16.0.0(release-tasmota)"}}.
func firmwareVersion(raw json.RawMessage) string {
	var v struct {
		StatusFWR struct {
			Version string `json:"Version"`
		} `json:"StatusFWR"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	s := v.StatusFWR.Version
	if i := strings.IndexByte(s, '('); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// decode returns the note for the dump record, the decoded JSON (nil if none)
// and the decoder version.
func (m *Manager) decode(ctx context.Context, dmp []byte, fwVersion string) (note string, decoded []byte, decoderVersion string) {
	if m.o.Decoder == nil {
		return "", nil, ""
	}
	dv, err := m.o.Decoder.Version(ctx)
	if err != nil {
		m.o.Log.Warn("decoder version unavailable", "component", "backup", "err", err)
		return NoteDecodeFailed + ": version", nil, ""
	}
	if err := CheckDecoder(dv, fwVersion); err != nil {
		m.o.Log.Info("decoder older than device firmware, keeping raw dump only", "component", "backup", "decoder", dv, "firmware", fwVersion)
		return NoteDecoderOutdated, nil, dv
	}
	out, err := m.o.Decoder.Decode(ctx, dmp)
	if err != nil {
		m.o.Log.Warn("decoding failed", "component", "backup", "err", err)
		return NoteDecodeFailed, nil, dv
	}
	return "", out, dv
}

// writeAtomic writes data to dir/name with mode 0600 via temp file + rename.
func writeAtomic(dir, name string, data []byte) (err error) {
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close() // already failing; the first error is reported
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(0o600); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

// prune applies the retention (Prunable) to the device's backups: the newest
// Keep runs plus every run younger than MaxAge stay, kept runs always stay and
// do not count. Failures are logged; they never fail the backup that
// triggered the pruning.
func (m *Manager) prune(ctx context.Context, id model.DeviceID) {
	all, err := m.o.Store.ListBackups(ctx, id) // newest first
	if err != nil {
		m.o.Log.Error("backup retention: list failed", "component", "backup", "device", id, "err", err)
		return
	}
	ret := m.ret.Load()
	for _, b := range Prunable(all, ret.keep, ret.maxAge, m.o.Clock.Now()) {
		if full, ok := m.safePath(b.Path); ok {
			if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
				m.o.Log.Error("backup retention: remove failed", "component", "backup", "device", id, "err", err)
				continue
			}
		}
		if err := m.o.Store.DeleteBackup(ctx, b.ID); err != nil {
			m.o.Log.Error("backup retention: delete failed", "component", "backup", "device", id, "err", err)
		}
	}
}

// safePath resolves a stored relative path below the data directory.
func (m *Manager) safePath(rel string) (string, bool) {
	if rel == "" || path.IsAbs(rel) || path.Clean(rel) != rel || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return filepath.Join(m.o.DataDir, filepath.FromSlash(rel)), true
}

// List returns the stored backups of a device, newest first.
func (m *Manager) List(ctx context.Context, id model.DeviceID) ([]model.Backup, error) {
	return m.o.Store.ListBackups(ctx, id)
}

// Open returns the content of a stored backup after verifying its checksum.
func (m *Manager) Open(ctx context.Context, id model.BackupID) ([]byte, model.Backup, error) {
	b, err := m.o.Store.GetBackup(ctx, id)
	if err != nil {
		return nil, model.Backup{}, err
	}
	full, ok := m.safePath(b.Path)
	if !ok {
		return nil, model.Backup{}, fmt.Errorf("backup: refusing stored path %q", b.Path)
	}
	data, err := os.ReadFile(full) //nolint:gosec // full comes from safePath: relative, cleaned, below DataDir
	if err != nil {
		return nil, model.Backup{}, fmt.Errorf("backup: %w", err)
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != b.SHA256 {
		return nil, model.Backup{}, ErrIntegrity
	}
	return data, b, nil
}
