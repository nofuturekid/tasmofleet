package web

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/secretbox"
	"github.com/nofuturekid/tasmofleet/internal/snapshot"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
)

const fakeKey = "fake-secret-key-0123456789abcdef"

type snapSource struct{}

func (snapSource) VacuumInto(_ context.Context, dst string) error {
	hdr := make([]byte, 100)
	copy(hdr, "SQLite format 3\x00")
	hdr[63] = 39 // user_version, big endian at offset 60
	return os.WriteFile(dst, append(hdr, "fake database body"...), 0o600)
}

type snapEnv struct {
	*env
	svc     *sysconfig.Service
	mgr     *snapshot.Manager
	dir     string
	audit   *fakeAudit
	stepped *atomic.Bool
}

func snapSetup(t *testing.T, stepped bool) *snapEnv {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, secretbox.KeyFile), []byte(fakeKey), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr := snapshot.New(snapSource{}, dir)
	aud := &fakeAudit{}
	e, svc, _, st := sysEnvFull(t, stepped, func(d *Deps) { d.Snapshots, d.Recorder = mgr, aud })
	return &snapEnv{env: e, svc: svc, mgr: mgr, dir: dir, audit: aud, stepped: st}
}

func (s *snapEnv) actions() []string {
	s.audit.mu.Lock()
	defer s.audit.mu.Unlock()
	var out []string
	for _, e := range s.audit.events {
		out = append(out, e.Action+" "+e.Result+" "+e.Detail)
	}
	return out
}

// Why: the file holds every user's password hash and the encrypted device and broker
// passwords. Only an admin may take it, and every attempt from a lower role is a 403/401.
func TestSnapshotRoutesAreAdminOnly(t *testing.T) {
	s := snapSetup(t, true)
	if _, err := s.mgr.Run(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	list, _ := s.mgr.List()
	name := list[0].Name
	for _, role := range []string{"", "operator"} {
		want := 403
		if role == "" {
			want = 401
		}
		for _, c := range []struct{ method, path string }{
			{"POST", "/settings/system/snapshot"},
			{"GET", "/settings/system/snapshots/" + name + "/download"},
			{"GET", "/settings/system/snapshots/" + name + "/delete"},
			{"POST", "/settings/system/snapshots/" + name + "/delete"},
		} {
			r, body := s.do(t, c.method, c.path, role, url.Values{"csrf": {"tok"}})
			if c.method == "GET" {
				r, body = s.do(t, c.method, c.path, role, nil)
			}
			if r.StatusCode != want || strings.Contains(body, "SQLite format") {
				t.Errorf("%q %s %s: status %d, want %d", role, c.method, c.path, r.StatusCode, want)
			}
		}
	}
	if l, _ := s.mgr.List(); len(l) != 1 {
		t.Fatal("a refused delete must delete nothing")
	}
}

// Why: a stolen session must not be enough to take the secrets out. Without a fresh
// confirmation nothing is made or sent; the page asks for the password instead.
func TestSnapshotDownloadNeedsStepUp(t *testing.T) {
	s := snapSetup(t, false)
	r, body := s.do(t, "POST", "/settings/system/snapshot", "admin", url.Values{"csrf": {"tok"}, "include_key": {"1"}})
	if r.StatusCode != 403 || strings.Contains(body, "SQLite format") || r.Header.Get("Content-Disposition") != "" {
		t.Fatalf("status %d: nothing may be sent before the confirmation", r.StatusCode)
	}
	for _, want := range []string{`<dialog id="stepup"`, `data-autoopen`, "Downloading a snapshot of the database", `name="current"`} {
		if !strings.Contains(body, want) {
			t.Errorf("dialog lacks %q", want)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(s.dir, ".snapshot-*")); len(left) != 0 {
		t.Fatalf("no copy may be made before the confirmation: %v", left)
	}
	if len(s.actions()) != 0 {
		t.Fatalf("nothing happened, nothing to audit: %v", s.actions())
	}
	// A stored snapshot is as secret as a fresh one.
	if _, err := s.mgr.Run(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	list, _ := s.mgr.List()
	if r, _ := s.do(t, "GET", "/settings/system/snapshots/"+list[0].Name+"/download", "admin", nil); r.StatusCode != 403 || r.Header.Get("Content-Disposition") != "" {
		t.Fatalf("stored download without confirmation: %d", r.StatusCode)
	}
	// After the password the same request goes through.
	s.stepped.Store(true)
	if r, _ := s.do(t, "POST", "/settings/system/snapshot", "admin", url.Values{"csrf": {"tok"}}); r.StatusCode != 200 {
		t.Fatalf("confirmed: %d", r.StatusCode)
	}
}

// Why: the key is the only thing that makes the encrypted values readable. It is in the
// download only when the admin ticked the box, and the audit says which kind was taken without
// ever holding the key.
func TestSnapshotDownloadWithAndWithoutKeyIsAudited(t *testing.T) {
	s := snapSetup(t, true)
	r, body := s.do(t, "POST", "/settings/system/snapshot", "admin", url.Values{"csrf": {"tok"}})
	if r.StatusCode != 200 || !strings.HasPrefix(body, "SQLite format 3") || strings.Contains(body, fakeKey) {
		t.Fatalf("plain download: %d, %.20q", r.StatusCode, body)
	}
	cd := r.Header.Get("Content-Disposition")
	if !strings.HasPrefix(cd, `attachment; filename="tasmofleet-`) || !strings.HasSuffix(cd, `.db"`) ||
		r.Header.Get("Cache-Control") != "no-store" || r.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers: %v", r.Header)
	}

	r, body = s.do(t, "POST", "/settings/system/snapshot", "admin", url.Values{"csrf": {"tok"}, "include_key": {"1"}})
	if r.StatusCode != 200 || !strings.HasSuffix(r.Header.Get("Content-Disposition"), `.tar"`) || r.Header.Get("Content-Type") != "application/x-tar" {
		t.Fatalf("tar download: %d %v", r.StatusCode, r.Header)
	}
	got := map[string]string{}
	tr := tar.NewReader(bytes.NewReader([]byte(body)))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		got[h.Name] = string(b)
	}
	if got[secretbox.KeyFile] != fakeKey || !strings.HasPrefix(got["tasmofleet.db"], "SQLite format 3") {
		t.Fatalf("tar entries: %v", len(got))
	}

	acts := s.actions()
	if len(acts) != 2 || !strings.Contains(acts[0], `"with_key":false`) || !strings.Contains(acts[1], `"with_key":true`) ||
		!strings.HasPrefix(acts[0], "snapshot.download ok") {
		t.Fatalf("audit: %v", acts)
	}
	for _, a := range acts {
		if strings.Contains(a, fakeKey) {
			t.Fatalf("the key is in the audit: %s", a)
		}
	}
	// The handler removes the copy when it returns, which can be a moment after the client has the body.
	deadline := time.Now().Add(5 * time.Second)
	for {
		left, _ := filepath.Glob(filepath.Join(s.dir, ".snapshot-*"))
		if len(left) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the scratch copy outlived the download: %v", left)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Why: the form posts only csrf and include_key. Anything else is a forged or broken request.
func TestSnapshotDownloadRejectsUnknownFormKeys(t *testing.T) {
	s := snapSetup(t, true)
	if r, _ := s.do(t, "POST", "/settings/system/snapshot", "admin", url.Values{"csrf": {"tok"}, "path": {"/etc/passwd"}}); r.StatusCode != 400 {
		t.Fatalf("status %d", r.StatusCode)
	}
}

// Why: the name comes from the URL. Whatever it says, only a file the snapshot folder lists
// itself may be read or deleted (CodeQL path-injection, and the key sits one level up).
func TestStoredSnapshotNamesCannotLeaveTheSnapshotFolder(t *testing.T) {
	s := snapSetup(t, true)
	if _, err := s.mgr.Run(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"..%2Fsecret.key", "..%2f..%2fetc%2fpasswd", "%2e%2e%2fsecret.key", "secret.key", "tasmofleet.db",
		"tasmofleet-20260101-0000Z.db%00", "tasmofleet-99999999-9999Z.db", "tasmofleet-20261029-0300Z.db%2F..%2F..%2Fsecret.key",
	} {
		for _, c := range []struct{ method, suffix string }{{"GET", "/download"}, {"GET", "/delete"}, {"POST", "/delete"}} {
			r, body := s.do(t, c.method, "/settings/system/snapshots/"+name+c.suffix, "admin", url.Values{"csrf": {"tok"}})
			if c.method == "GET" {
				r, body = s.do(t, c.method, "/settings/system/snapshots/"+name+c.suffix, "admin", nil)
			}
			// 307: the mux cleans a path with ".." and sends the client to the cleaned address, where
			// the name is just another route (checked above: nothing there serves a file).
			if r.StatusCode != 404 && r.StatusCode != 400 && r.StatusCode != 307 || strings.Contains(body, fakeKey) {
				t.Errorf("%s %s%s: status %d", c.method, name, c.suffix, r.StatusCode)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(s.dir, secretbox.KeyFile)); err != nil {
		t.Fatalf("the key was touched: %v", err)
	}
	if l, _ := s.mgr.List(); len(l) != 1 {
		t.Fatal("the stored snapshot must survive")
	}
}

// Why: Download and Delete of a stored snapshot work outside the settings form, are audited,
// and a delete really removes the file.
func TestStoredSnapshotDownloadAndDelete(t *testing.T) {
	s := snapSetup(t, true)
	if _, err := s.mgr.Run(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	list, _ := s.mgr.List()
	name := list[0].Name
	r, body := s.do(t, "GET", "/settings/system/snapshots/"+name+"/download", "admin", nil)
	if r.StatusCode != 200 || !strings.HasPrefix(body, "SQLite format 3") || !strings.Contains(r.Header.Get("Content-Disposition"), name) {
		t.Fatalf("download: %d", r.StatusCode)
	}
	if r, _ := s.do(t, "GET", "/settings/system/snapshots/"+name+"/delete", "admin", nil); r.StatusCode != 200 {
		t.Fatalf("confirmation page: %d", r.StatusCode)
	}
	r, _ = s.do(t, "POST", "/settings/system/snapshots/"+name+"/delete", "admin", url.Values{"csrf": {"tok"}})
	if r.StatusCode != 303 || !strings.Contains(r.Header.Get("Location"), "ok=snapshot-deleted") {
		t.Fatalf("delete: %d %v", r.StatusCode, r.Header)
	}
	if l, _ := s.mgr.List(); len(l) != 0 {
		t.Fatalf("still stored: %v", l)
	}
	acts := strings.Join(s.actions(), "\n")
	if !strings.Contains(acts, "snapshot.download ok") || !strings.Contains(acts, "snapshot.delete ok") || !strings.Contains(acts, name) {
		t.Fatalf("audit:\n%s", acts)
	}
	if r, _ := s.do(t, "POST", "/settings/system/snapshots/"+name+"/delete", "admin", url.Values{"csrf": {"tok"}}); r.StatusCode != 404 {
		t.Fatalf("a second delete: %d", r.StatusCode)
	}
}

// Why: the page is what makes the feature findable (#410: System -> Snapshots, no longer a card on
// the Backups tab); it lists what is stored, says restore is by hand and holds the schedule next to
// the download. The Backups tab is about device backups only.
func TestSnapshotsPageShowsDownloadStoredScheduleAndRestore(t *testing.T) {
	s := snapSetup(t, true)
	if _, err := s.mgr.Run(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	_, body := s.do(t, "GET", "/settings/system/snapshots", "admin", nil)
	for _, want := range []string{
		"<h1>Snapshots</h1>", `id="card-download"`, "Download a snapshot", `<button type="submit" form="db-dl" class="btn" id="btn-dl">Download snapshot</button>`,
		`name="include_key" value="1"`, `id="db-dl" method="post" action="/tf/settings/system/snapshot"`,
		"The file holds secrets. Keep it like a password.", "There is no restore button on purpose.",
		`id="card-stored"`, "Stored snapshots", `id="card-restore"`,
		`id="card-snapshots"`, "<h2>Schedule</h2>", `name="sw.SNAPSHOT_INTERVAL"`, `name="v.SNAPSHOT_KEEP"`,
		`action="/tf/settings/system/snapshots"`, `data-sys-bar`, `data-dlg-action="/tf/settings/system/snapshots/`, "Latest", "schema 39",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, `name="include_key" value="1" checked`) {
		t.Error("the key must be off by default")
	}
	_, backups := s.do(t, "GET", "/settings/system?tab=backups", "admin", nil)
	for _, gone := range []string{`id="card-database"`, `id="db-dl"`, "Download snapshot", "SNAPSHOT_INTERVAL", "Snapshot schedule", "There is no restore button"} {
		if strings.Contains(backups, gone) {
			t.Errorf("the Backups tab still holds the snapshot part: %q", gone)
		}
	}
	if !strings.Contains(backups, "Device backup schedule") {
		t.Error("the device backup schedule is gone")
	}
}

// Why (#410, maintainer decision): "Backups of deleted devices" sits in the narrow column under the
// device backup schedule, so the wide column is the storage table alone.
func TestDeletedDeviceBackupsSitUnderTheDeviceBackupSchedule(t *testing.T) {
	e, _, _, _ := storageEnv(t)
	_, body := e.do(t, "GET", "/settings/system?tab=backups", "admin", nil)
	sched, orph := strings.Index(body, `id="card-schedule"`), strings.Index(body, `id="card-orphans"`)
	narrow := strings.Index(body[strings.Index(body, `id="sec-backups"`):], `<div class="cstack">`) + strings.Index(body, `id="sec-backups"`)
	stor := strings.Index(body, `id="card-storage"`)
	if stor >= narrow || narrow >= sched || sched >= orph {
		t.Errorf("order storage %d, narrow stack %d, schedule %d, deleted devices %d", stor, narrow, sched, orph)
	}
}

// Why: a failed schedule must be seen on the page, with the reason, not only in a log.
func TestSnapshotsPageReportsAFailedScheduledSnapshot(t *testing.T) {
	dir := t.TempDir()
	mgr := snapshot.New(failSource{}, dir)
	e, svc, _, _ := sysEnvFull(t, true, func(d *Deps) { d.Snapshots = mgr })
	if _, err := svc.Apply(t.Context(), sysconfig.Request{Actor: "admin", Values: map[string]string{"SNAPSHOT_INTERVAL": "24h"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Run(t.Context(), 7); err == nil {
		t.Fatal("expected a failure")
	}
	_, body := e.do(t, "GET", "/settings/system/snapshots", "admin", nil)
	for _, want := range []string{"The last scheduled snapshot failed.", "no space left on device", "Last attempt failed", "No good snapshot yet", "see the audit log"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

type failSource struct{}

func (failSource) VacuumInto(context.Context, string) error {
	return &os.PathError{Op: "write", Path: "x", Err: errors.New("no space left on device")}
}

// Why: the switch is a view of SNAPSHOT_INTERVAL like the device schedule's. Off stores 0 (no
// snapshots), on without an interval is refused at the field, on with one stores it; the
// minimum protects the disk from a snapshot a minute.
func TestSnapshotScheduleSwitchIsComposedIntoTheInterval(t *testing.T) {
	s := snapSetup(t, true)
	post := func(f url.Values) (int, string) {
		f.Set("csrf", "tok")
		r, b := s.do(t, "POST", "/settings/system/snapshots", "admin", f)
		return r.StatusCode, b
	}
	if code, body := post(url.Values{"swm.SNAPSHOT_INTERVAL": {"1"}, "sw.SNAPSHOT_INTERVAL": {"1"}, "v.SNAPSHOT_INTERVAL": {""}}); code != 422 || !strings.Contains(body, "enter how often") {
		t.Fatalf("on without an interval: %d", code)
	}
	if code, _ := post(url.Values{"swm.SNAPSHOT_INTERVAL": {"1"}, "sw.SNAPSHOT_INTERVAL": {"1"}, "v.SNAPSHOT_INTERVAL": {"1m"}}); code != 422 {
		t.Fatalf("one minute: %d", code)
	}
	if code, _ := post(url.Values{"swm.SNAPSHOT_INTERVAL": {"1"}, "sw.SNAPSHOT_INTERVAL": {"1"}, "v.SNAPSHOT_INTERVAL": {"24h"}, "v.SNAPSHOT_KEEP": {"5"}}); code != 303 {
		t.Fatalf("on with 24h: %d", code)
	}
	if c := s.svc.Snapshot(); c.SnapshotSchedule.Hours() != 24 || c.SnapshotKeep != 5 {
		t.Fatalf("stored %v / %d", c.SnapshotSchedule, c.SnapshotKeep)
	}
	if code, _ := post(url.Values{"swm.SNAPSHOT_INTERVAL": {"1"}, "v.SNAPSHOT_INTERVAL": {"24h"}}); code != 303 || s.svc.Snapshot().SnapshotSchedule != 0 {
		t.Fatalf("switch off must store 0: %d %v", code, s.svc.Snapshot().SnapshotSchedule)
	}
	if code, _ := post(url.Values{"v.SNAPSHOT_KEEP": {"0"}}); code != 422 {
		t.Fatalf("keep 0: %d", code)
	}
}

// Why (proportionate security): a proxy-header sign-in has no password to confirm with, so it must
// not be blocked, but the file holds every password hash and the encrypted secrets, so the
// download is never one click: a confirmation page with a checkbox comes first, for the fresh and
// the stored download, and the audit says the sign-in was a header.
func TestHeaderSignInsConfirmWithACheckboxBeforeASnapshotDownload(t *testing.T) {
	s := snapSetup(t, true) // the password step-up does not apply to header principals
	if _, err := s.mgr.Run(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	list, _ := s.mgr.List()
	hdr := []string{"X-Test-Method", "header"}
	dl := "/settings/system/snapshots/" + list[0].Name + "/download"

	for _, c := range []struct {
		name, method, path string
		form               url.Values
		action             string
	}{
		{"fresh", "POST", "/settings/system/snapshot", url.Values{"csrf": {"tok"}, "include_key": {"1"}}, "/tf/settings/system/snapshot"},
		{"stored", "GET", dl, nil, "/tf" + dl},
	} {
		r, body := s.do(t, c.method, c.path, "admin", c.form, hdr...)
		if r.StatusCode != 200 || strings.Contains(body, "SQLite format") || r.Header.Get("Content-Disposition") != "" {
			t.Fatalf("%s: the first request must only ask: %d", c.name, r.StatusCode)
		}
		for _, want := range []string{"I understand this file holds secrets.", `name="ack" value="1" required`, `action="` + c.action + `"`, `name="csrf"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: confirmation lacks %q", c.name, want)
			}
		}
	}
	if len(s.actions()) != 0 {
		t.Fatalf("asking is not a download: %v", s.actions())
	}
	// Posting without the tick, or with another value, asks again.
	if r, body := s.do(t, "POST", dl, "admin", url.Values{"csrf": {"tok"}, "ack": {"0"}}, hdr...); r.StatusCode != 200 || strings.Contains(body, "SQLite format") {
		t.Fatalf("no tick: %d", r.StatusCode)
	}
	// Ticked: the file comes, with the key when asked, and the audit says header.
	r, body := s.do(t, "POST", "/settings/system/snapshot", "admin", url.Values{"csrf": {"tok"}, "include_key": {"1"}, "ack": {"1"}}, hdr...)
	if r.StatusCode != 200 || !strings.HasSuffix(r.Header.Get("Content-Disposition"), `.tar"`) || !strings.Contains(body, fakeKey) {
		t.Fatalf("confirmed fresh download: %d", r.StatusCode)
	}
	r, body = s.do(t, "POST", dl, "admin", url.Values{"csrf": {"tok"}, "ack": {"1"}}, hdr...)
	if r.StatusCode != 200 || !strings.HasPrefix(body, "SQLite format 3") {
		t.Fatalf("confirmed stored download: %d", r.StatusCode)
	}
	acts := s.actions()
	if len(acts) != 2 || !strings.Contains(acts[0], `"method":"header"`) || !strings.Contains(acts[1], `"method":"header"`) {
		t.Fatalf("audit: %v", acts)
	}
}

// Why: a HEAD request sends no file, so it is no download and must not fill the audit log.
func TestHeadOfAStoredSnapshotIsNotAudited(t *testing.T) {
	s := snapSetup(t, true)
	if _, err := s.mgr.Run(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	list, _ := s.mgr.List()
	r, _ := s.do(t, "HEAD", "/settings/system/snapshots/"+list[0].Name+"/download", "admin", nil)
	if r.StatusCode != 200 || len(s.actions()) != 0 {
		t.Fatalf("HEAD: %d, audit %v", r.StatusCode, s.actions())
	}
}

// Why: the page names the folders the server really uses (DATA_DIR may not be /data), so the
// restore steps can be followed as written.
func TestSnapshotsPageNamesTheRealDataDirectory(t *testing.T) {
	s := snapSetup(t, true)
	_, body := s.do(t, "GET", "/settings/system/snapshots", "admin", nil)
	if !strings.Contains(body, s.dir+"/tasmofleet.db") || !strings.Contains(body, s.dir+"/secret.key") || !strings.Contains(body, s.dir+"/snapshots") {
		t.Error("the card must show the data directory in use")
	}
	if strings.Contains(body, "/data/snapshots") {
		t.Error("no hard-coded /data")
	}
	if !strings.Contains(body, "You are asked to confirm first (password, or your identity provider for SSO).") {
		t.Error("the copy must say that SSO accounts confirm at their identity provider")
	}
}

// Why (#387): the person ticked "Include secret.key" and clicked Download; the password prompt must not
// make them tick and click again, and a forgotten key would leave them with a file that cannot be read
// later. The choice (a boolean, nothing else) rides along the step-up to the page it returns to, which
// shows the box ticked and the Download button focused. Without the choice nothing is added.
func TestSnapshotStepUpKeepsTheKeyChoice(t *testing.T) {
	s := snapSetup(t, false)
	_, withKey := s.do(t, "POST", "/settings/system/snapshot", "admin", url.Values{"csrf": {"tok"}, "include_key": {"1"}})
	if !strings.Contains(withKey, `/settings/system/snapshots/stepup?dlkey=1`) {
		t.Error("the password prompt must post to a step-up that remembers the key choice")
	}
	_, without := s.do(t, "POST", "/settings/system/snapshot", "admin", url.Values{"csrf": {"tok"}})
	if strings.Contains(without, "dlkey") {
		t.Error("no key chosen: nothing to carry")
	}

	r, _ := s.do(t, "POST", "/settings/system/snapshots/stepup?dlkey=1", "admin", url.Values{"csrf": {"tok"}, "current": {"right"}})
	if loc := r.Header.Get("Location"); r.StatusCode != 303 || !strings.Contains(loc, "/settings/system/snapshots?restore=1") || !strings.Contains(loc, "dlkey=1") {
		t.Fatalf("return address %d %q must carry the choice", r.StatusCode, loc)
	}
	r, _ = s.do(t, "POST", "/settings/system/snapshots/stepup", "admin", url.Values{"csrf": {"tok"}, "current": {"right"}})
	if loc := r.Header.Get("Location"); strings.Contains(loc, "dlkey") {
		t.Errorf("no choice, none carried: %q", loc)
	}
	// A wrong password shows the prompt again, still remembering the choice.
	s.stepped.Store(false)
	_, again := s.do(t, "POST", "/settings/system/snapshots/stepup?dlkey=1", "admin", url.Values{"csrf": {"tok"}, "current": {"wrong"}})
	if !strings.Contains(again, `snapshots/stepup?dlkey=1`) {
		t.Error("a retry after a wrong password must not forget the choice")
	}

	s.stepped.Store(true)
	_, back := s.do(t, "GET", "/settings/system/snapshots?restore=1&dlkey=1", "admin", nil)
	if !strings.Contains(back, `name="include_key" value="1" checked`) || !strings.Contains(back, `id="btn-dl" autofocus`) {
		t.Error("the return page must show the box ticked and the Download button focused")
	}
	_, plain := s.do(t, "GET", "/settings/system/snapshots", "admin", nil)
	if strings.Contains(plain, `name="include_key" value="1" checked`) || strings.Contains(plain, `id="btn-dl" autofocus`) {
		t.Error("an ordinary visit starts unticked and unfocused")
	}
}

// Why (#399): a file where the snapshot folder belongs (a bind mount mistake, a stray file) used to
// make System, Settings answer 500 for every tab, so the person could not even reach the card that
// would explain it. Every page must render, and the Snapshots page must name the problem; the
// scheduled snapshot fails with the same reason.
func TestPagesSurviveAFileWhereTheSnapshotFolderBelongs(t *testing.T) {
	s := snapSetup(t, true)
	if err := os.WriteFile(filepath.Join(s.dir, snapshot.DirName), []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/settings/system?tab=backups", "/settings/system?tab=general", "/settings/system/snapshots"} {
		r, body := s.do(t, "GET", path, "admin", nil)
		if r.StatusCode != 200 {
			t.Fatalf("%s: status %d, want the page", path, r.StatusCode)
		}
		if has := strings.Contains(body, "The snapshot folder is not a directory") && strings.Contains(body, `role="alert"`); has != (path == "/settings/system/snapshots") {
			t.Errorf("%s: the page says what is wrong: %v", path, has)
		}
	}
	if _, err := s.mgr.Run(t.Context(), 7); err == nil || snapshot.ReasonOf(err) == "" {
		t.Fatalf("a scheduled snapshot must fail with a reason, got %v", err)
	}
	_, body := s.do(t, "GET", "/settings/system/snapshots", "admin", nil)
	if !strings.Contains(body, "not a directory") {
		t.Error("the failed attempt's reason is not on the page")
	}
}
