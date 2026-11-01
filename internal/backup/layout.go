package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// File layout (0.18): <DataDir>/backups/<dir>/<stamp>_<host>.<kind>, where
// dir and host come from the device's hostname, so a person browsing the data
// directory sees which device a file belongs to. Every row stores its own
// relative path and all reads use it, so files of an older layout stay
// readable wherever they lie; a hostname change only affects new backups.

const (
	stampLayout = "20060102T150405Z"
	maxNameLen  = 63
)

// SanitizeName reduces s to [A-Za-z0-9._-]: every other rune becomes "_",
// leading dots are dropped (no hidden or "." / ".." names), the result is at
// most 63 bytes. "" means nothing usable is left.
func SanitizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.TrimLeft(b.String(), ".")
	if len(out) > maxNameLen {
		out = out[:maxNameLen]
	}
	return out
}

// baseName is the device's name for directory and files: current hostname,
// else its inventory name, else device-<id>.
func baseName(d model.Device) string {
	if n := SanitizeName(d.Net.Hostname); n != "" {
		return n
	}
	if n := SanitizeName(d.Name); n != "" {
		return n
	}
	return fmt.Sprintf("device-%d", int64(d.ID))
}

// fileName is the name of one file of a run; kind "dmp" is the plain dump,
// "files" the tar of the device's files.
func fileName(taken string, base string, kind model.BackupKind) string {
	if kind == model.BackupConfigDump {
		return taken + "_" + base + ".dmp"
	}
	if kind == model.BackupFiles {
		return taken + "_" + base + ".files.tar"
	}
	return taken + "_" + base + "." + string(kind) + ".json"
}

// resolveDir picks the backup directory (relative to DataDir, slash form) and
// the file base name for d. A directory that already holds another device's
// (or a deleted device's) backups is not shared: <base>_<id> is used. The
// caller holds m.dirMu.
func (m *Manager) resolveDir(ctx context.Context, d model.Device) (dir, base string, err error) {
	base = baseName(d)
	owners, err := m.o.Store.BackupDirOwners(ctx, base)
	if err != nil {
		return "", "", fmt.Errorf("backup: directory owners: %w", err)
	}
	for _, o := range owners {
		if o != d.ID {
			return path.Join("backups", fmt.Sprintf("%s_%d", base, int64(d.ID))), base, nil
		}
	}
	return path.Join("backups", base), base, nil
}

// backupsRoot is <DataDir>/backups.
func (m *Manager) backupsRoot() string { return filepath.Join(m.o.DataDir, "backups") }

// removeEmptyDirs removes the (now possibly empty) directories that held the
// given relative file paths. os.Remove only succeeds on an empty directory, so
// nothing with content can be lost; every other error is ignored on purpose.
func (m *Manager) removeEmptyDirs(rels ...string) {
	m.dirMu.Lock()
	defer m.dirMu.Unlock()
	seen := map[string]bool{}
	for _, rel := range rels {
		full, ok := m.safePath(rel)
		if !ok {
			continue
		}
		d := filepath.Dir(full)
		if seen[d] || filepath.Dir(d) != m.backupsRoot() {
			continue
		}
		seen[d] = true
		_ = os.Remove(d) // fails with ENOTEMPTY while files are left: that is the point
	}
}

// MigrationResult summarises MigrateLayout.
type MigrationResult struct {
	Moved    int // files renamed and re-indexed
	Repaired int // a crash had left the file moved: only the row was updated
	Current  int // already in the new layout, or not a 0.17 path
	Left     int // not touched (mismatch, ambiguous, ...); see Reasons and the warnings
	Missing  int // the row's file does not exist (deleted by hand): nothing to move
	// Unindexed counts 0.17-named files in backups/<number>/ that no row points
	// to (a restored older database, a crash before indexing, hand-copied
	// folders). They are reported, never moved or deleted.
	Unindexed int
	// Reasons counts Left by reason, for the one summary line at start.
	Reasons map[string]int
}

// legacyPath matches a 0.17 path; group 1 is the directory's device id, which
// for an orphan row is the only trace of the deleted device's id.
var legacyPath = regexp.MustCompile(`^backups/([0-9]+)/(\d{8}T\d{6}Z)-(dmp|snapshot|decoded)\.(dmp|json)$`)

// MigrateLayout moves the 0.17 files backups/<device-id>/<stamp>-<kind>.<ext>
// to the hostname layout. It is idempotent and crash-safe and never loses a
// backup:
//
//   - a file moves only after its SHA-256 equals the row's; os.Rename keeps the
//     bytes on one filesystem, then the row path is changed compare-and-set;
//   - a crash between rename and update is repaired on the next start (old file
//     gone, new file matches the row);
//   - anything unclear (checksum mismatch, both or neither file present, target
//     used by another row, rename error) is left exactly as it is and counted;
//   - nothing is ever deleted except empty numeric directories (os.Remove).
//
// Orphan rows (backups kept after their device was deleted) move the same way:
// their directory comes from the stored device name (orphan-<former id> when
// none is stored) and never shares a directory with another device's backups.
// Rows whose file does not exist are counted as missing and left alone.
func (m *Manager) MigrateLayout(ctx context.Context) (MigrationResult, error) {
	m.dirMu.Lock()
	defer m.dirMu.Unlock()
	var res MigrationResult
	rows, err := m.o.Store.ListAllBackups(ctx)
	if err != nil {
		return res, fmt.Errorf("backup: list for layout migration: %w", err)
	}
	used := make(map[string]model.BackupID, len(rows))
	shared := map[string]int{} // rows per path: a file two rows point at must not move
	for _, b := range rows {
		used[strings.ToLower(b.Path)] = b.ID
		shared[strings.ToLower(b.Path)]++
	}
	claimed := map[string]string{} // lower-cased dir -> owner key ("d<id>", "o<former id>") that got it in this run
	type target struct{ dir, base string }
	targets := map[string]*target{}
	res.Reasons = map[string]int{}
	warn := func(b model.Backup, why string, err error) {
		res.Left++
		res.Reasons[why]++
		m.o.Log.Warn("backup layout: left in place", "component", "backup", "backup", b.ID, "path", b.Path, "why", why, "err", err)
	}
	for _, b := range rows {
		if b.DeviceID != 0 && legacyPath.FindStringSubmatch(b.Path) == nil {
			b = m.relinkLegacy(ctx, b) // row says new path, file still at the old one (lost rename)
		}
		mt := legacyPath.FindStringSubmatch(b.Path)
		if mt == nil {
			if m.FileMissing(b) {
				res.Missing++
			} else {
				res.Current++
			}
			continue
		}
		former, stamp, kind, ext := mt[1], mt[2], model.BackupKind(mt[3]), mt[4]
		if (kind == model.BackupConfigDump) != (ext == "dmp") {
			warn(b, "kind and extension disagree", nil)
			continue
		}
		if shared[strings.ToLower(b.Path)] > 1 {
			warn(b, "path shared by several rows", nil)
			continue
		}
		// The owner key: a live device by its id, an orphan by the id its 0.17
		// directory carries (its backups all share that directory).
		key, ownerID := "o"+former, former
		if b.DeviceID != 0 {
			ownerID = strconv.FormatInt(int64(b.DeviceID), 10)
			key = "d" + ownerID
		}
		t := targets[key]
		if t == nil {
			var dir, base string
			if b.DeviceID != 0 {
				dev, err := m.o.Devices.GetDevice(ctx, b.DeviceID)
				if err != nil {
					warn(b, "device not readable", err)
					continue
				}
				dir, base, err = m.resolveDir(ctx, dev)
				if err != nil {
					warn(b, "directory not resolvable", err)
					continue
				}
			} else {
				dir, base = orphanDir(rows, b, former)
			}
			if c, ok := claimed[strings.ToLower(dir)]; ok && c != key {
				// Another owner got this directory earlier in this run, even if
				// its rows were left in place and the index does not show it yet.
				dir = path.Join("backups", base+"_"+ownerID)
			}
			claimed[strings.ToLower(dir)] = key
			t = &target{dir, base}
			targets[key] = t
		}
		newRel := path.Join(t.dir, fileName(stamp, t.base, kind))
		if other, taken := used[strings.ToLower(newRel)]; taken && other != b.ID {
			warn(b, "target path belongs to another backup", nil)
			continue
		}
		oldFull, ok1 := m.safePath(b.Path)
		newFull, ok2 := m.safePath(newRel)
		if !ok1 || !ok2 {
			warn(b, "unsafe path", nil)
			continue
		}
		out, why := m.migrateOne(ctx, b, newRel, oldFull, newFull, used)
		switch out {
		case migMoved:
			res.Moved++
			used[strings.ToLower(newRel)] = b.ID
		case migRepaired:
			res.Repaired++
			used[strings.ToLower(newRel)] = b.ID
		case migMissing:
			res.Missing++
		case migLeft:
			res.Left++
			res.Reasons[why]++
		}
	}
	m.removeEmptyNumericDirs()
	unref := m.scanUnreferenced(func(rel string) bool { _, ok := used[strings.ToLower(rel)]; return ok })
	res.Unindexed = len(unref)
	m.o.Log.Info("backup layout", "component", "backup", "moved", res.Moved, "repaired", res.Repaired, "in_place", res.Current,
		"left", res.Left, "left_why", reasonList(res.Reasons), "files_missing", res.Missing, "unindexed", res.Unindexed)
	for _, f := range unref {
		m.o.Log.Warn("backup layout: file in an old folder that no backup entry references, left alone", "component", "backup", "path", f.Path, "size", f.Size)
	}
	return res, nil
}

// UnreferencedFile is a 0.17-named backup file that no backup row points to.
type UnreferencedFile struct {
	Path    string // relative to the data directory, slash form
	Size    int64
	ModTime time.Time
}

// scanUnreferenced lists the files backups/<number>/<stamp>-<kind>.<ext> for
// which referenced(path) is false, sorted by path. Only regular files whose
// name matches legacyPath count: other names in numeric folders are not ours.
func (m *Manager) scanUnreferenced(referenced func(rel string) bool) []UnreferencedFile {
	var out []UnreferencedFile
	dirs, err := os.ReadDir(m.backupsRoot())
	if err != nil {
		return nil
	}
	for _, d := range dirs {
		if !d.IsDir() || !isDigits(d.Name()) {
			continue
		}
		files, err := os.ReadDir(filepath.Join(m.backupsRoot(), d.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			rel := path.Join("backups", d.Name(), f.Name())
			if !f.Type().IsRegular() || legacyPath.FindStringSubmatch(rel) == nil || referenced(rel) {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			out = append(out, UnreferencedFile{Path: rel, Size: info.Size(), ModTime: info.ModTime()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Unreferenced lists the 0.17-named files below backups/<number>/ that no
// backup row points to, for Settings -> Backups. Read-only.
func (m *Manager) Unreferenced(ctx context.Context) ([]UnreferencedFile, error) {
	rows, err := m.o.Store.ListAllBackups(ctx)
	if err != nil {
		return nil, fmt.Errorf("backup: list for unreferenced files: %w", err)
	}
	used := make(map[string]bool, len(rows))
	for _, b := range rows {
		used[strings.ToLower(b.Path)] = true
	}
	return m.scanUnreferenced(func(rel string) bool { return used[strings.ToLower(rel)] }), nil
}

type migOutcome int

const (
	migMoved migOutcome = iota
	migRepaired
	migLeft
	migMissing // neither the old nor the new file exists
)

// orphanDir picks the directory and file base name for the kept backups of a
// deleted device: its stored name, else orphan-<former id>. The name alone is
// used only while no other device's backups (a live device's, or an orphan of
// another name or MAC) are indexed below it; otherwise <base>_<former id>, the
// same rule resolveDir applies to live devices.
func orphanDir(rows []model.Backup, b model.Backup, former string) (dir, base string) {
	base = SanitizeName(b.DeviceName)
	if base == "" {
		base = "orphan-" + former
	}
	prefix := strings.ToLower(path.Join("backups", base)) + "/"
	for _, o := range rows {
		if !strings.HasPrefix(strings.ToLower(o.Path), prefix) {
			continue
		}
		if o.DeviceID != 0 || o.DeviceName != b.DeviceName || o.DeviceMAC != b.DeviceMAC {
			return path.Join("backups", base+"_"+former), base
		}
	}
	return path.Join("backups", base), base
}

// reasonList renders the left-in-place reasons as "why=count, ..." in a stable order.
func reasonList(r map[string]int) string {
	keys := make([]string, 0, len(r))
	for k := range r {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, r[k]))
	}
	return strings.Join(parts, ", ")
}

// FileMissing reports that the row's file does not exist (deleted by hand).
// Any other stat problem is not "missing": the file may well be there.
func (m *Manager) FileMissing(b model.Backup) bool {
	full, ok := m.safePath(b.Path)
	if !ok {
		return false
	}
	_, err := os.Stat(full)
	return errors.Is(err, os.ErrNotExist)
}

// strandedFile looks for b's file below backups/*/ under the name a migration
// would have given it (same stamp and suffix as newRel), in a path no other row
// uses, with the row's SHA-256. Exactly one hit is returned; zero or several
// leave the row alone.
func (m *Manager) strandedFile(b model.Backup, newRel string, used map[string]model.BackupID) (string, bool) {
	name := path.Base(newRel)
	i := strings.IndexByte(name, '_')
	if i < 0 {
		return "", false
	}
	stamp, rest := name[:i], name[i+1:]
	suffix := rest[strings.IndexByte(rest, '.'):]
	hits, err := filepath.Glob(filepath.Join(m.backupsRoot(), "*", stamp+"_*"+suffix))
	if err != nil {
		return "", false
	}
	var found []string
	for _, h := range hits {
		rel, err := filepath.Rel(m.o.DataDir, h)
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(rel)
		if other, taken := used[strings.ToLower(rel)]; taken && other != b.ID {
			continue
		}
		if sum, err := fileSHA(h); err == nil && sum == b.SHA256 {
			found = append(found, rel)
		}
	}
	if len(found) != 1 {
		return "", false
	}
	return found[0], true
}

func (m *Manager) migrateOne(ctx context.Context, b model.Backup, newRel, oldFull, newFull string, used map[string]model.BackupID) (migOutcome, string) {
	left := func(why string, err error) (migOutcome, string) {
		m.o.Log.Warn("backup layout: left in place", "component", "backup", "backup", b.ID, "path", b.Path, "why", why, "err", err)
		return migLeft, why
	}
	newSum, newErr := fileSHA(newFull)
	oldSum, oldErr := fileSHA(oldFull)
	switch {
	case newErr == nil:
		// The target exists. Only a crash between rename and row update looks
		// like this: the old file is gone and the target is the row's file.
		if newSum != b.SHA256 {
			return left("target exists with another checksum", nil)
		}
		if oldErr == nil {
			return left("old and new file both exist", nil)
		}
		if !errors.Is(oldErr, os.ErrNotExist) {
			return left("old file unreadable", oldErr)
		}
		if !m.setPath(ctx, b, newRel) {
			return left("row not updated", nil)
		}
		return migRepaired, ""
	case !errors.Is(newErr, os.ErrNotExist):
		return left("target unreadable", newErr)
	}
	if errors.Is(oldErr, os.ErrNotExist) {
		// Neither file is where expected. A crash after a rename may have put the
		// file in a directory a later run would not choose (the name changed since):
		// find it by checksum before calling the row missing.
		if cand, ok := m.strandedFile(b, newRel, used); ok {
			if !m.setPath(ctx, b, cand) {
				return left("row not updated", nil)
			}
			used[strings.ToLower(cand)] = b.ID
			return migRepaired, ""
		}
		return migMissing, "" // nothing to move: the file was deleted by hand
	}
	if oldErr != nil {
		return left("old file unreadable", oldErr)
	}
	if oldSum != b.SHA256 {
		return left("old file does not match its checksum", nil)
	}
	if err := os.MkdirAll(filepath.Dir(newFull), 0o700); err != nil {
		return left("create directory", err)
	}
	if err := os.Rename(oldFull, newFull); err != nil {
		return left("rename", err)
	}
	// Make the rename durable before the row changes: after a power loss on a
	// journaling/CoW file system the row must never point to a path whose
	// directory entry was not persisted. On failure the row keeps the old path;
	// whichever name survived is found by the next start.
	if err := syncDir(filepath.Dir(newFull)); err != nil {
		return left("sync target directory", err)
	}
	if err := syncDir(filepath.Dir(oldFull)); err != nil {
		return left("sync source directory", err)
	}
	if !m.setPath(ctx, b, newRel) {
		return left("row not updated", nil) // file is at newRel, row says old: the next start repairs it
	}
	return migMoved, ""
}

// setPath updates the row compare-and-set and logs a failure.
func (m *Manager) setPath(ctx context.Context, b model.Backup, newRel string) bool {
	ok, err := m.o.Store.UpdateBackupPath(ctx, b.ID, b.Path, newRel)
	if err != nil || !ok {
		m.o.Log.Warn("backup layout: row not updated", "component", "backup", "backup", b.ID, "from", b.Path, "to", newRel, "changed", !ok, "err", err)
		return false
	}
	return true
}

// removeEmptyNumericDirs removes empty backups/<number>/ directories left by
// the migration. os.Remove refuses a directory with content.
func (m *Manager) removeEmptyNumericDirs() {
	ents, err := os.ReadDir(m.backupsRoot())
	if err != nil {
		return
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() && isDigits(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		_ = os.Remove(filepath.Join(m.backupsRoot(), n)) // ENOTEMPTY: still holds files, keep
	}
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// fileSHA streams the file through SHA-256.
func fileSHA(full string) (string, error) {
	f, err := os.Open(full) //nolint:gosec // full comes from safePath
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// syncDir flushes a directory's entries to disk.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // dir comes from safePath
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close() // the sync error is the one to report
		return err
	}
	return d.Close()
}

var newPath = regexp.MustCompile(`^backups/[^/]+/(\d{8}T\d{6}Z)_[^/]+\.(dmp|snapshot\.json|decoded\.json)$`)

// relinkLegacy is the safety net for a row that already names the hostname
// layout while its file is missing: if the 0.17 file is still there and
// matches the row's checksum, the row is pointed back at it (the normal
// migration then moves it). Anything else returns b unchanged.
func (m *Manager) relinkLegacy(ctx context.Context, b model.Backup) model.Backup {
	mt := newPath.FindStringSubmatch(b.Path)
	full, ok := m.safePath(b.Path)
	if mt == nil || !ok {
		return b
	}
	if _, err := os.Stat(full); !errors.Is(err, os.ErrNotExist) {
		return b
	}
	ext, kind := "json", strings.TrimSuffix(mt[2], ".json")
	if mt[2] == "dmp" {
		ext, kind = "dmp", "dmp"
	}
	rel := fmt.Sprintf("backups/%d/%s-%s.%s", int64(b.DeviceID), mt[1], kind, ext)
	old, ok := m.safePath(rel)
	if !ok {
		return b
	}
	if sum, err := fileSHA(old); err != nil || sum != b.SHA256 {
		return b
	}
	if ok, err := m.o.Store.UpdateBackupPath(ctx, b.ID, b.Path, rel); err != nil || !ok {
		return b
	}
	m.o.Log.Warn("backup layout: file was still at its 0.17 path, row repaired", "component", "backup", "backup", b.ID, "from", b.Path, "to", rel)
	b.Path = rel
	return b
}
