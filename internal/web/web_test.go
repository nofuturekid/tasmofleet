package web

import (
	"context"
	"errors"
	"fmt"
	"github.com/nofuturekid/tasmofleet/internal/inventory"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// ---- fakes (consumer-side, per CLAUDE.md) ----

type fakeInv struct {
	mu   sync.Mutex
	devs map[model.DeviceID]model.Device
	next model.DeviceID
	last model.Device // last device passed to Update/Create
	// unproven makes Update demand the unverified-move confirmation; confirmedMoves counts those given.
	unproven       bool
	confirmedMoves int
	lastDelete     inventory.DeleteOptions
}

func newInv(ds ...model.Device) *fakeInv {
	f := &fakeInv{devs: map[model.DeviceID]model.Device{}, next: 100}
	for _, d := range ds {
		f.devs[d.ID] = d
	}
	return f
}
func (f *fakeInv) List(context.Context) ([]model.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.Device
	for _, d := range f.devs {
		out = append(out, d)
	}
	return out, nil
}
func (f *fakeInv) Get(_ context.Context, id model.DeviceID) (model.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.devs[id]
	if !ok {
		return d, store.ErrNotFound
	}
	return d, nil
}
func (f *fakeInv) Create(_ context.Context, _ string, d model.Device) (model.DeviceID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	d.ID = f.next
	f.devs[d.ID], f.last = d, d
	return d.ID, nil
}
func (f *fakeInv) Update(ctx context.Context, _ string, d model.Device) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// A move of a device with a stored password to an address that cannot prove
	// the MAC needs the explicit confirmation, like the real service.
	if old := f.devs[d.ID]; f.unproven && old.HTTPPassword != "" && d.HTTPPassword == old.HTTPPassword && d.Address != old.Address {
		if target := inventory.MoveTarget(d.Address, d.Port); !inventory.UnverifiedMoveConfirmed(ctx, target) {
			return &inventory.UnverifiedMoveError{Target: target}
		}
		f.confirmedMoves++
	}
	f.devs[d.ID], f.last = d, d
	return nil
}
func (f *fakeInv) DeleteWith(ctx context.Context, actor string, id model.DeviceID, opt inventory.DeleteOptions) error {
	f.mu.Lock()
	f.lastDelete = opt
	f.mu.Unlock()
	return f.Delete(ctx, actor, id)
}
func (f *fakeInv) Delete(_ context.Context, _ string, id model.DeviceID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.devs[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.devs, id)
	return nil
}
func (f *fakeInv) Upsert(context.Context, string, model.Device) (model.DeviceID, bool, error) {
	return 0, false, errors.New("unused")
}

type fakeHealth map[model.DeviceID]model.Health

func (f fakeHealth) Get(id model.DeviceID) model.Health   { return f[id] }
func (f fakeHealth) All() map[model.DeviceID]model.Health { return f }
func (f fakeHealth) Run(context.Context) error            { return nil }

type fakeBackups struct {
	list    []model.Backup
	data    []byte
	taken   int
	failErr error
}

func (f *fakeBackups) Backup(_ context.Context, _ string, id model.DeviceID) ([]model.Backup, error) {
	if f.failErr != nil {
		return nil, f.failErr
	}
	f.taken++
	b := model.Backup{ID: model.BackupID(f.taken), DeviceID: id, Kind: model.BackupConfigDump, TakenAt: time.Now(), Path: "backups/1/x.dmp", Size: 4}
	f.list = append([]model.Backup{b}, f.list...)
	return []model.Backup{b}, nil
}
func (f *fakeBackups) List(context.Context, model.DeviceID) ([]model.Backup, error) {
	return f.list, nil
}
func (f *fakeBackups) Open(_ context.Context, id model.BackupID) ([]byte, model.Backup, error) {
	for _, b := range f.list {
		if b.ID == id {
			return f.data, b, nil
		}
	}
	return nil, model.Backup{}, store.ErrNotFound
}

type fakeProber struct {
	describe model.Device
	err      error
	refreshs []model.DeviceID
}

func (f *fakeProber) Describe(_ context.Context, d model.Device) (model.Device, error) {
	if f.err != nil {
		return d, f.err
	}
	d.MAC, d.Name = f.describe.MAC, f.describe.Name
	return d, nil
}
func (f *fakeProber) Refresh(_ context.Context, id model.DeviceID) error {
	f.refreshs = append(f.refreshs, id)
	return f.err
}

type fakeAudit struct {
	mu     sync.Mutex
	events []model.AuditEvent
}

func (f *fakeAudit) Record(_ context.Context, e model.AuditEvent) {
	f.mu.Lock()
	f.events = append(f.events, e)
	f.mu.Unlock()
}
func (f *fakeAudit) ListAudit(_ context.Context, flt store.AuditFilter) ([]model.AuditEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.AuditEvent
	for _, e := range f.events {
		if flt.Actor != "" && e.Actor != flt.Actor || len(flt.Actors) > 0 && !slices.Contains(flt.Actors, e.Actor) {
			continue
		}
		if !strings.HasPrefix(e.Action, flt.ActionPrefix) || !flt.Since.IsZero() && e.At.Before(flt.Since) {
			continue
		}
		if flt.Search != "" || len(flt.SearchTargets) > 0 || len(flt.SearchActors) > 0 {
			q := strings.ToLower(flt.Search)
			hit := flt.Search != "" && (strings.Contains(strings.ToLower(e.Actor), q) || strings.Contains(strings.ToLower(e.Action), q) || strings.Contains(strings.ToLower(e.Target), q))
			if !hit && !slices.Contains(flt.SearchTargets, e.Target) && !slices.Contains(flt.SearchActors, e.Actor) {
				continue
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// AuditFacets mirrors store.SQLite.AuditFacets.
func (f *fakeAudit) AuditFacets(_ context.Context, since time.Time) (actors, actions []string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.events {
		if !since.IsZero() && e.At.Before(since) {
			continue
		}
		if !slices.Contains(actors, e.Actor) {
			actors = append(actors, e.Actor)
		}
		if !slices.Contains(actions, e.Action) {
			actions = append(actions, e.Action)
		}
	}
	slices.Sort(actors)
	slices.Sort(actions)
	return actors, actions, nil
}

// fakeGuard authenticates through the X-Test-Role header ("operator",
// "admin"; absent = anonymous) so the tests exercise the real route table.
type fakeGuard struct{ revoked *atomic.Bool }

func (fakeGuard) CSRFToken(*http.Request) string { return "tok" }

// Principal mimics auth.Service.Principal: a revoked session fails, and so does a
// request that lost the httpx request info (scheme, base path). The real service picks
// the session cookie's name (__Host-tasmofleet over HTTPS) and path from that info, so
// without it the browser's cookie is not found.
func (g fakeGuard) Principal(r *http.Request) (model.Principal, error) {
	if g.revoked != nil && g.revoked.Load() {
		return model.Principal{}, auth.ErrUnauthenticated
	}
	if httpx.BasePath(r.Context()) != "/tf" {
		return model.Principal{}, auth.ErrUnauthenticated
	}
	return model.Principal{Subject: "test:" + r.Header.Get("X-Test-Role")}, nil
}

func (fakeGuard) Require(min model.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var role model.Role
			switch r.Header.Get("X-Test-Role") {
			case "operator":
				role = model.RoleOperator
			case "admin":
				role = model.RoleAdmin
			}
			if role == model.RoleNone {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if role < min {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			p := model.Principal{Subject: "test:" + r.Header.Get("X-Test-Role"), Name: "Tester", Role: role}
			next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
		})
	}
}

type env struct {
	revoked *atomic.Bool
	srv     *httptest.Server
	inv     *fakeInv
	bk      *fakeBackups
	pr      *fakeProber
	aud     *fakeAudit
	hub     *events.Hub
	health  fakeHealth
}

func setup(t *testing.T) *env { return setupWith(t, nil) }

func setupWith(t *testing.T, mutate func(*Deps)) *env {
	t.Helper()
	revoked := &atomic.Bool{}
	e := &env{revoked: revoked,
		inv: newInv(
			model.Device{ID: 1, Name: "kitchen", Address: "10.0.0.1", Tags: []string{"kitchen"}, HTTPUser: "admin", HTTPPassword: "s3cret-pw",
				Identity: model.Identity{Family: model.ChipESP8266, Version: "16.0.0", Variant: "tasmota"}},
			model.Device{ID: 2, Name: "attic", Address: "10.0.0.2", Identity: model.Identity{Family: model.ChipESP32, Version: "15.0.0", Variant: "tasmota32"}},
		),
		bk:     &fakeBackups{data: []byte("DUMP")},
		pr:     &fakeProber{describe: model.Device{MAC: "aa:bb:cc:dd:ee:ff", Name: "probed-name"}},
		aud:    &fakeAudit{},
		hub:    events.NewHub(8),
		health: fakeHealth{1: {DeviceID: 1, State: model.HealthOnline, LastSeen: time.Now(), RSSI: 71, UptimeSec: 3700}},
	}
	deps := Deps{
		Guard: fakeGuard{revoked: revoked}, Inventory: e.inv, Health: e.health, Backups: e.bk, Prober: e.pr,
		Audit: e.aud, Recorder: e.aud, Hub: e.hub, Version: "v-test", BackupCooldown: time.Nanosecond,
		Config: []KV{{"TASMOFLEET_OIDC_CLIENT_SECRET", "***"}},
	}
	if mutate != nil {
		mutate(&deps)
	}
	h, err := Handler(deps)
	if err != nil {
		t.Fatal(err)
	}
	// httpx.Proxy supplies the base path the way production does.
	proxied := httpx.Proxy{BasePath: "/tf"}.Middleware(h)
	mux := http.NewServeMux()
	mux.Handle("/tf/", proxied)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

// result is a closed response: status, headers and the body text.
type result struct {
	StatusCode int
	Header     http.Header
}

func (e *env) do(t *testing.T, method, path, role string, form url.Values, hdr ...string) (*result, string) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(t.Context(), method, e.srv.URL+"/tf"+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if role != "" {
		req.Header.Set("X-Test-Role", role)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return &result{StatusCode: resp.StatusCode, Header: resp.Header}, string(b)
}

// ---- tests ----

// Why: the device list is the main page; links must carry the base path so
// the UI works under /tasmofleet and behind HA ingress.
func TestDeviceListShowsHealthAndBasePathLinks(t *testing.T) {
	e := setup(t)
	resp, body := e.do(t, "GET", "/devices", "operator", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for _, want := range []string{`href="/tf/devices/1"`, "kitchen", "attic", "online", "unknown", "16.0.0", "esp8266", `sse-connect="/tf/events"`, `/tf/static/app.css`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in device list", want)
		}
	}
}

// Why: health is conveyed by text, not colour alone (accessibility rule in the spec).
func TestHealthBadgeHasText(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/devices/table", "operator", nil)
	if !strings.Contains(body, `class="bdg dv-st t-ok" title="Online: `) || !strings.Contains(body, `</span>Online</span>`) {
		t.Fatalf("badge without text: %s", body)
	}
}

func TestDeviceFilters(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/devices/table?health=online", "operator", nil)
	if !strings.Contains(body, "kitchen") || strings.Contains(body, "attic") {
		t.Fatalf("health filter wrong: %s", body)
	}
	_, body = e.do(t, "GET", "/devices/table?family=esp32", "operator", nil)
	if strings.Contains(body, "kitchen") || !strings.Contains(body, "attic") {
		t.Fatalf("family filter wrong")
	}
	_, body = e.do(t, "GET", "/devices/table?tag=kitchen&q=kit", "operator", nil)
	if !strings.Contains(body, "kitchen") || strings.Contains(body, "attic") {
		t.Fatalf("tag/search filter wrong")
	}
}

// Why: roles. Only admins may edit the inventory, read the audit log or
// download backups (they contain device secrets); anonymous gets 401.
func TestRoleMatrix(t *testing.T) {
	e := setup(t)
	e.bk.list = []model.Backup{{ID: 5, DeviceID: 1, Kind: model.BackupConfigDump, Path: "backups/1/a.dmp"}}
	cases := []struct {
		method, path, role string
		want               int
	}{
		{"GET", "/devices", "", 401},
		{"GET", "/devices", "operator", 200},
		{"GET", "/devices/1", "operator", 200},
		{"GET", "/devices/new", "operator", 403},
		{"GET", "/devices/new", "admin", 200},
		{"GET", "/devices/1/edit", "operator", 403},
		{"POST", "/devices/1", "operator", 403},
		{"POST", "/devices/1/delete", "operator", 403},
		{"POST", "/devices/new", "operator", 403},
		{"GET", "/audit", "operator", 403},
		{"GET", "/audit", "admin", 200},
		{"GET", "/backups/5", "operator", 403},
		{"GET", "/backups/5", "admin", 200},
		{"POST", "/devices/1/backup", "operator", 303},
		{"POST", "/devices/1/probe", "operator", 200},
		{"GET", "/about", "operator", 403},
		{"GET", "/about", "admin", 200},
		{"GET", "/", "operator", 200},
	}
	for _, c := range cases {
		var form url.Values
		if c.method == "POST" {
			form = url.Values{}
		}
		resp, _ := e.do(t, c.method, c.path, c.role, form)
		if resp.StatusCode != c.want {
			t.Errorf("%s %s as %q: got %d want %d", c.method, c.path, c.role, resp.StatusCode, c.want)
		}
	}
}

// Why: safety rule 4, device credentials never reach the browser.
func TestDevicePasswordNeverRendered(t *testing.T) {
	e := setup(t)
	for _, p := range []string{"/devices/1", "/devices/1/edit", "/devices", "/devices/table"} {
		_, body := e.do(t, "GET", p, "admin", nil)
		if strings.Contains(body, "s3cret-pw") {
			t.Errorf("%s leaks the device password", p)
		}
	}
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(body, "password set") {
		t.Error("detail page should say that a password is set")
	}
}

// Why: Inventory.Update replaces the record fully; leaving the password
// field blank in the edit form must keep the stored one instead of wiping it.
func TestEditKeepsStoredPassword(t *testing.T) {
	e := setup(t)
	form := url.Values{"name": {"kitchen2"}, "address": {"10.0.0.1"}, "http_user": {"admin"}, "http_password": {""}, "tags": {"a, b"}}
	resp, _ := e.do(t, "POST", "/devices/1", "admin", form)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/tf/devices/1?ok=saved" {
		t.Fatalf("got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if e.inv.last.HTTPPassword != "s3cret-pw" || e.inv.last.Name != "kitchen2" {
		t.Fatalf("password not kept: %+v", e.inv.last)
	}
	if got := strings.Join(e.inv.last.Tags, ","); got != "a,b" {
		t.Fatalf("tags: %q", got)
	}
	if e.inv.last.Identity.Version != "16.0.0" {
		t.Fatal("identity must survive an edit")
	}
	form.Set("clear_password", "1")
	e.do(t, "POST", "/devices/1", "admin", form)
	if e.inv.last.HTTPPassword != "" {
		t.Fatal("clear_password must remove the password")
	}
}

func TestAddDeviceProbesAndCreates(t *testing.T) {
	e := setup(t)
	form := url.Values{"address": {"10.0.0.9"}, "tags": {"new"}}
	resp, _ := e.do(t, "POST", "/devices/new", "admin", form)
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/tf/devices/101?ok=added") {
		t.Fatalf("got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if e.inv.last.MAC != "aa:bb:cc:dd:ee:ff" || e.inv.last.Name != "probed-name" {
		t.Fatalf("probe result not used: %+v", e.inv.last)
	}
}

// Why: a failed probe must not silently create a half-filled device; the
// admin gets the form back with an explicit way to add anyway.
func TestAddDeviceProbeFailureOffersFallback(t *testing.T) {
	e := setup(t)
	e.pr.err = fmt.Errorf("probe: %w", tasmota.ErrUnreachable)
	resp, body := e.do(t, "POST", "/devices/new", "admin", url.Values{"address": {"10.0.0.9"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "No device answered") || !strings.Contains(body, "skip_probe") {
		t.Fatalf("got %d: %s", resp.StatusCode, body)
	}
	if len(e.inv.devs) != 2 {
		t.Fatal("nothing may be created after a failed probe")
	}
	resp, _ = e.do(t, "POST", "/devices/new", "admin", url.Values{"address": {"10.0.0.9"}, "name": {"x"}, "skip_probe": {"1"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("fallback add: %d", resp.StatusCode)
	}
}

func TestDeleteRedirectsHTMX(t *testing.T) {
	e := setup(t)
	resp, _ := e.do(t, "POST", "/devices/2/delete", "admin", url.Values{}, "HX-Request", "true")
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("HX-Redirect") != "/tf/devices?ok=deleted" {
		t.Fatalf("got %d %q", resp.StatusCode, resp.Header.Get("HX-Redirect"))
	}
	if _, ok := e.inv.devs[2]; ok {
		t.Fatal("not deleted")
	}
	resp, _ = e.do(t, "POST", "/devices/2/delete", "admin", url.Values{})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete: %d", resp.StatusCode)
	}
}

// Why: the device header's "Back up" works on every tab without JavaScript: it takes the
// backup and returns to the tab it was pressed on with the result, and only to a tab of
// this device (the form cannot send the browser elsewhere). A failure names the reason.
func TestBackupNowListsNewBackup(t *testing.T) {
	e := setup(t)
	resp, _ := e.do(t, "POST", "/devices/1/backup", "operator", url.Values{"back": {"/console"}})
	if resp.StatusCode != 303 || e.bk.taken != 1 || resp.Header.Get("Location") != "/tf/devices/1/console?ok=backup-taken" {
		t.Fatalf("got %d taken=%d to %q", resp.StatusCode, e.bk.taken, resp.Header.Get("Location"))
	}
	e2 := setup(t)
	resp, _ = e2.do(t, "POST", "/devices/1/backup", "operator", url.Values{"back": {"//evil.example/x"}})
	if loc := resp.Header.Get("Location"); loc != "/tf/devices/1?ok=backup-taken" {
		t.Fatalf("an unknown tab must lead to the overview, got %q", loc)
	}
	_, body := e2.do(t, "GET", "/devices/1?ok=backup-taken", "operator", nil)
	if !strings.Contains(body, "Backup taken.") {
		t.Fatal("the result is not shown after the redirect")
	}
	e3 := setup(t)
	e3.bk.failErr = fmt.Errorf("backup: download config: %w", tasmota.ErrUnreachable)
	_, body = e3.do(t, "POST", "/devices/1/backup", "operator", url.Values{})
	if !strings.Contains(body, "Backup failed. The device did not answer") || !strings.Contains(body, `href="/tf/devices/1"`) {
		t.Fatalf("failure not shown: %s", body)
	}
}

// Why: downloads contain device secrets (spec 12.3): admin only, audited,
// served as an attachment that the browser cannot sniff into HTML.
func TestBackupDownloadIsAuditedAttachment(t *testing.T) {
	e := setup(t)
	e.bk.list = []model.Backup{{ID: 7, DeviceID: 1, Kind: model.BackupConfigDump, Path: "backups/1/20261018 evil\"name.dmp"}}
	resp, body := e.do(t, "GET", "/backups/7", "admin", nil)
	if resp.StatusCode != 200 || body != "DUMP" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	cd := resp.Header.Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment;") || strings.ContainsAny(strings.TrimPrefix(cd, "attachment; filename="), " ") {
		t.Fatalf("unsafe Content-Disposition %q", cd)
	}
	if resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
	}
	if len(e.aud.events) != 1 || e.aud.events[0].Action != "backup.download" || e.aud.events[0].Actor != "test:admin" {
		t.Fatalf("audit: %+v", e.aud.events)
	}
	resp, _ = e.do(t, "GET", "/backups/99", "admin", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("missing backup: %d", resp.StatusCode)
	}
}

// Why: a downloaded dump must say which device it belongs to (issue #55); the
// stored name "<stamp>-dmp.dmp" does not, and repeats the kind. The name is
// device-derived, so it must stay filesystem- and header-safe.
func TestBackupDownloadNameCarriesDeviceNotKindTwice(t *testing.T) {
	e := setup(t)
	at := time.Date(2026, 10, 25, 16, 15, 35, 0, time.UTC)
	e.bk.list = []model.Backup{
		{ID: 7, DeviceID: 1, DeviceName: "Küche \"Spot\"/1", Kind: model.BackupConfigDump, TakenAt: at, Path: "backups/1/20261025T161535Z-dmp.dmp"},
		{ID: 8, DeviceID: 1, DeviceName: "Küche \"Spot\"/1", Kind: model.BackupSnapshot, TakenAt: at, Path: "backups/1/20261025T161535Z-snapshot.json"},
		{ID: 9, DeviceID: 4, DeviceName: "", Kind: model.BackupConfigDump, TakenAt: at, Path: "backups/4/20261025T161535Z-dmp.dmp"},
	}
	for id, want := range map[int]string{
		7: `attachment; filename="K_che__Spot__1-20261025T161535Z.dmp"`,
		8: `attachment; filename="K_che__Spot__1-20261025T161535Z-snapshot.json"`,
		9: `attachment; filename="device-4-20261025T161535Z.dmp"`,
	} {
		resp, _ := e.do(t, "GET", fmt.Sprintf("/backups/%d", id), "admin", nil)
		if got := resp.Header.Get("Content-Disposition"); got != want {
			t.Errorf("backup %d: Content-Disposition %q, want %q", id, got, want)
		}
	}
}

func TestNotFoundAndBadID(t *testing.T) {
	e := setup(t)
	for _, p := range []string{"/devices/999", "/devices/abc", "/devices/0"} {
		if resp, _ := e.do(t, "GET", p, "operator", nil); resp.StatusCode != 404 {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
}

func TestAboutMasksConfigAndShowsVersion(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/about", "admin", nil)
	if !strings.Contains(body, "v-test") || !strings.Contains(body, "***") {
		t.Fatalf("about: %s", body)
	}
}

// Why: the live table relies on ready-made fragments named per device and on
// resync when the hub dropped events.
func TestSSEStreamsHealthFragments(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/tf/events", nil)
	req.Header.Set("X-Test-Role", "operator")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	go func() {
		for ctx.Err() == nil {
			e.hub.Publish(events.Event{Topic: events.TopicHealth, Payload: model.Health{DeviceID: 1, State: model.HealthOffline, RSSI: 40}})
			time.Sleep(20 * time.Millisecond)
		}
	}()
	buf := make([]byte, 4096)
	var got string
	for !strings.Contains(got, "event: wifi-1") {
		n, err := resp.Body.Read(buf)
		got += string(buf[:n])
		if err != nil {
			t.Fatalf("stream ended: %v (%q)", err, got)
		}
	}
	if !strings.Contains(got, "event: health-1\ndata: ") || !strings.Contains(got, "offline") || !strings.Contains(got, "40%") {
		t.Fatalf("unexpected stream: %q", got)
	}
	if strings.Contains(strings.SplitN(got, "event: wifi-1", 2)[0], "\n\n\n") {
		t.Fatalf("malformed event framing: %q", got)
	}
}

// Why: the dashboard counts the fleet per state: kitchen answers, attic never did.
func TestDashboardCountsPerState(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/", "operator", nil)
	if !strings.Contains(body, "2 devices · state streams in live") || !strings.Contains(body, `aria-label="1 online, 0 offline, 1 unknown"`) {
		t.Fatalf("dashboard: %s", body)
	}
}

// Why (review F5): an operator must not be able to evict the restore points
// by clicking "Back up now" repeatedly.
func TestManualBackupIsRateLimitedPerDevice(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.BackupCooldown = time.Hour })
	if resp, _ := e.do(t, "POST", "/devices/1/backup", "operator", url.Values{}); resp.StatusCode != 303 {
		t.Fatalf("first: %d", resp.StatusCode)
	}
	resp, body := e.do(t, "POST", "/devices/1/backup", "operator", url.Values{})
	if resp.StatusCode != http.StatusTooManyRequests || e.bk.taken != 1 || !strings.Contains(body, "Try again in") {
		t.Fatalf("second: %d taken=%d %s", resp.StatusCode, e.bk.taken, body)
	}
	if resp, _ := e.do(t, "POST", "/devices/2/backup", "operator", url.Values{}); resp.StatusCode != 303 {
		t.Fatalf("another device must not be blocked: %d", resp.StatusCode)
	}
}

// stream is an open SSE response; Close ends it.
type stream struct {
	Status int
	body   io.ReadCloser
}

func (s *stream) Close() { _ = s.body.Close() }

func openStream(t *testing.T, e *env, ctx context.Context) *stream {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/tf/events", nil)
	req.Header.Set("X-Test-Role", "operator")
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed by stream.Close
	if err != nil {
		t.Fatal(err)
	}
	return &stream{Status: resp.StatusCode, body: resp.Body}
}

// Why (review F6): a revoked session must not keep receiving live fleet data.
func TestSSEEndsWhenSessionIsRevoked(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.SSERecheck = 30 * time.Millisecond })
	s := openStream(t, e, t.Context())
	defer s.Close()
	e.revoked.Store(true)
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, s.body); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream stayed open after revocation")
	}
}

// Why (v0.11.2, live banner): the periodic session re-check must see the request the way the
// auth middleware saw it. v0.11.1 re-checked with a bare context, the cookie lookup fell back to
// the plain-HTTP name and every stream behind the HTTPS proxy ended at the first re-check (20 s),
// so /devices kept showing "Live update interrupted".
func TestSSESurvivesSessionRecheck(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.SSERecheck = 20 * time.Millisecond })
	s := openStream(t, e, t.Context())
	defer s.Close()
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, s.body); close(done) }()
	select {
	case <-done:
		t.Fatal("stream ended at a session re-check although the session is valid")
	case <-time.After(300 * time.Millisecond): // 15 re-checks
	}
}

func TestSSEPerUserCap(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.MaxStreamsPerUser = 2 })
	for range 2 {
		s := openStream(t, e, t.Context())
		defer s.Close()
	}
	third := openStream(t, e, t.Context())
	defer third.Close()
	if third.Status != http.StatusTooManyRequests {
		t.Fatalf("third stream: %d", third.Status)
	}
}

// doJSON posts a JSON body as role (used by the preference endpoint).
func (e *env) doJSON(t *testing.T, path, role, body string) (*result, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), "POST", e.srv.URL+"/tf"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if role != "" {
		req.Header.Set("X-Test-Role", role)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return &result{StatusCode: resp.StatusCode, Header: resp.Header}, string(b)
}

// Why: a retention shows in words ("30 days"), the way the audit page says it; the short form
// "30d 0h" is for elapsed times only.
func TestLongDurationIsInWords(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * 24 * time.Hour: "30 days", 24 * time.Hour: "1 day", 36 * time.Hour: "36 hours", time.Hour: "1 hour",
		90 * time.Minute: "90 minutes", 45 * time.Second: "45s",
	} {
		if got := longDuration(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}
