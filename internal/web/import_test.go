package web

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/importer"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// stepGuard models the step-up (used by the system settings and restore
// tests). The principal's method comes from the X-Test-Method header (default
// local), its session from X-Test-Session. The import must not care.
type stepGuard struct {
	fakeGuard
	stepped *atomic.Bool
}

func (g stepGuard) Require(min model.Role) func(http.Handler) http.Handler {
	inner := g.fakeGuard.Require(min)
	return func(next http.Handler) http.Handler {
		return inner(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, _ := auth.FromContext(r.Context())
			p.Method = "local"
			if m := r.Header.Get("X-Test-Method"); m != "" {
				p.Method = m
			}
			next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
		}))
	}
}
func (g stepGuard) SteppedUp(*http.Request) bool { return g.stepped.Load() }
func (g stepGuard) SessionKey(r *http.Request) string {
	return r.Header.Get("X-Test-Session")
}
func (g stepGuard) StepUp(r *http.Request, pw string) (bool, bool) {
	if pw == "right" {
		g.stepped.Store(true)
		return true, false
	}
	return false, false
}

type recInv struct{ upserts []model.Device }

func (recInv) List(context.Context) ([]model.Device, error) { return nil, nil }
func (r *recInv) Upsert(_ context.Context, _ string, d model.Device) (model.DeviceID, bool, error) {
	r.upserts = append(r.upserts, d)
	return model.DeviceID(len(r.upserts)), true, nil
}

type part struct{ field, filename, ctype, body string }

func multipartBody(t *testing.T, parts ...part) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="`+p.field+`"; filename="`+p.filename+`"`)
		h.Set("Content-Type", p.ctype)
		w, _ := mw.CreatePart(h)
		_, _ = io.WriteString(w, p.body)
	}
	_ = mw.Close()
	return mw.FormDataContentType(), buf.Bytes()
}

func (e *env) upload(t *testing.T, role string, parts ...part) (int, string) {
	t.Helper()
	return e.uploadAs(t, role, nil, parts...)
}

func (e *env) uploadAs(t *testing.T, role string, hdr map[string]string, parts ...part) (int, string) {
	t.Helper()
	ct, body := multipartBody(t, parts...)
	req, _ := http.NewRequestWithContext(t.Context(), "POST", e.srv.URL+"/tf/import/tasmoadmin", bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Test-Role", role)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const importCSV = `1,"plug",10.0.0.9,admin,s3cret-in-csv,bulb_1,1,1,0,0,1,80,"plug",0,plug,0` + "\n" +
	`2,"=cmd|' /C calc'!A0<script>",10.0.0.10,,,bulb_1,1,1,0,0,1,80,"=cmd|' /C calc'!A0<script>",0,x,0` + "\n"

func importEnv(t *testing.T) (*env, *recInv) {
	inv := &recInv{}
	e := setupWith(t, func(d *Deps) {
		d.Import = importer.New(importer.Options{Inventory: inv})
		d.Guard = stepGuard{fakeGuard: fakeGuard{}, stepped: &atomic.Bool{}} // never stepped up: the import must not ask
	})
	return e, inv
}

func planToken(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, `name="plan" value="`)
	if i < 0 {
		t.Fatalf("no plan token in %s", body)
	}
	return strings.SplitN(body[i+len(`name="plan" value="`):], `"`, 2)[0]
}

// Why: the import writes credentials, so operators are locked out entirely,
// and cannot apply a plan an admin prepared.
func TestImportIsAdminOnly(t *testing.T) {
	e, inv := importEnv(t)
	if r, _ := e.do(t, "GET", "/devices/new", "operator", nil); r.StatusCode != 403 {
		t.Fatalf("operator: %d", r.StatusCode)
	}
	if c, _ := e.upload(t, "operator", part{"devices_csv", "devices.csv", "text/csv", importCSV}); c != 403 {
		t.Fatalf("operator upload: %d", c)
	}
	_, body := e.upload(t, "admin", part{"devices_csv", "devices.csv", "text/csv", importCSV})
	token := planToken(t, body)
	if r, _ := e.do(t, "POST", "/import/tasmoadmin/apply", "operator", map[string][]string{"plan": {token}}); r.StatusCode != 403 || len(inv.upserts) != 0 {
		t.Fatalf("operator apply: %d writes=%d", r.StatusCode, len(inv.upserts))
	}
	if r, _ := e.do(t, "POST", "/import/tasmoadmin/apply", "admin", map[string][]string{"plan": {token}}); r.StatusCode != 200 || len(inv.upserts) != 2 {
		t.Fatalf("the admin's plan must survive the operator's attempt: %d", r.StatusCode)
	}
}

// Why: with the step-up gone the role check is the only gate. Both routes must
// refuse operators (403) and anonymous requests (401) before anything is
// parsed or written.
func TestImportRoutesRefuseOperatorsAndAnonymous(t *testing.T) {
	e, inv := importEnv(t)
	for role, want := range map[string]int{"operator": 403, "": 401} {
		if c, _ := e.upload(t, role, part{"devices_csv", "devices.csv", "text/csv", importCSV}); c != want {
			t.Errorf("upload as %q: %d, want %d", role, c, want)
		}
		if r, _ := e.do(t, "POST", "/import/tasmoadmin/apply", role, map[string][]string{"plan": {"x"}}); r.StatusCode != want {
			t.Errorf("apply as %q: %d, want %d", role, r.StatusCode, want)
		}
	}
	if len(inv.upserts) != 0 {
		t.Fatalf("nothing may be written: %d", len(inv.upserts))
	}
}

// Why (audit rows 2 and 3, decision 2026-10-21): an admin who is signed in may
// import without a second proof, whatever the sign-in method, header mode
// included. The dry-run plan and its single confirmation stay the safety net;
// there is no step-up route and no prompt any more.
func TestImportNeedsNoStepUp(t *testing.T) {
	for _, m := range []string{"local", "oidc", "header", "future-method"} {
		e, inv := importEnv(t)
		hdr := map[string]string{"X-Test-Method": m}
		c, body := e.uploadAs(t, "admin", hdr, part{"devices_csv", "devices.csv", "text/csv", importCSV})
		if c != 200 || !strings.Contains(body, ">2 create</span>") {
			t.Fatalf("%s: upload %d %s", m, c, body)
		}
		r, _ := e.do(t, "POST", "/import/tasmoadmin/apply", "admin", map[string][]string{"plan": {planToken(t, body)}}, "X-Test-Method", m)
		if r.StatusCode != 200 || len(inv.upserts) != 2 {
			t.Fatalf("%s: apply %d writes=%d", m, r.StatusCode, len(inv.upserts))
		}
	}
	e, _ := importEnv(t)
	_, page := e.do(t, "GET", "/devices/new", "admin", nil)
	if strings.Contains(page, `name="current"`) || strings.Contains(page, "oidc/stepup") || !strings.Contains(page, `type="file"`) {
		t.Fatalf("the page must offer the upload directly: %s", page)
	}
	if r, _ := e.do(t, "POST", "/import/tasmoadmin/stepup", "admin", map[string][]string{"current": {"x"}}); r.StatusCode != 404 && r.StatusCode != 405 {
		t.Fatalf("the step-up route must be gone: %d", r.StatusCode)
	}
}

// Why: upload -> plan -> apply is the feature; the plan is a dry run, the
// device is written only after the confirmation, and the plan is single-use.
func TestImportPlanThenApply(t *testing.T) {
	e, inv := importEnv(t)
	c, body := e.upload(t, "admin", part{"devices_csv", "devices.csv", "text/csv", importCSV})
	if c != 200 || !strings.Contains(body, ">2 create</span>") || len(inv.upserts) != 0 {
		t.Fatalf("plan: %d %s (writes=%d)", c, body, len(inv.upserts))
	}
	i := strings.Index(body, `name="plan" value="`)
	if i < 0 {
		t.Fatal("no plan token")
	}
	token := strings.SplitN(body[i+len(`name="plan" value="`):], `"`, 2)[0]
	r, out := e.do(t, "POST", "/import/tasmoadmin/apply", "admin", map[string][]string{"plan": {token}})
	if r.StatusCode != 200 || len(inv.upserts) != 2 || !strings.Contains(out, "2 devices created") {
		t.Fatalf("apply: %d %s", r.StatusCode, out)
	}
	if r, _ := e.do(t, "POST", "/import/tasmoadmin/apply", "admin", map[string][]string{"plan": {token}}); r.StatusCode != 410 || len(inv.upserts) != 2 {
		t.Fatalf("replayed plan must be refused: %d", r.StatusCode)
	}
}

// Why: decrypted or plaintext device passwords must never be sent back to a
// browser, and cell text is escaped (CSV injection / HTML in names).
func TestImportResponsesHideSecretsAndEscapeCells(t *testing.T) {
	e, _ := importEnv(t)
	_, plan := e.upload(t, "admin", part{"devices_csv", "devices.csv", "text/csv", importCSV})
	if strings.Contains(plan, "s3cret-in-csv") {
		t.Fatal("password in plan response")
	}
	if strings.Contains(plan, "<script>") || !strings.Contains(plan, "&lt;script&gt;") {
		t.Fatalf("cell not escaped: %s", plan)
	}
}

// Why: uploads are untrusted; wrong types, binary content, extra fields and
// oversize files are refused before parsing.
func TestImportRejectsBadUploads(t *testing.T) {
	e, inv := importEnv(t)
	big := strings.Repeat("a", importer.MaxFileBytes+1)
	cases := []struct {
		name  string
		want  int
		parts []part
	}{
		{"html type", 415, []part{{"devices_csv", "d.csv", "text/html", importCSV}}},
		{"zip type", 415, []part{{"devices_csv", "d.zip", "application/zip", importCSV}}},
		{"zip bytes as csv", 415, []part{{"devices_csv", "d.csv", "text/csv", "PK\x03\x04\x00\x00binary\x00\x01\x02"}}},
		{"oversize", 413, []part{{"devices_csv", "d.csv", "text/csv", big}}},
		{"unknown field", 400, []part{{"evil", "d.csv", "text/csv", importCSV}}},
		{"missing csv", 422, []part{{"my_config", "c.json", "application/json", "{}"}}},
	}
	for _, tc := range cases {
		if c, _ := e.upload(t, "admin", tc.parts...); c != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, c, tc.want)
		}
	}
	if len(inv.upserts) != 0 {
		t.Fatal("nothing may be written by a rejected upload")
	}
}

// Why (review F2): a plan holds decrypted passwords and must not outlive its
// TTL even if nobody uploads or applies again.
func TestPendingPlansExpireByThemselves(t *testing.T) {
	plans := &importPlans{ttl: 30 * time.Millisecond}
	plan := importer.Plan{Rows: []importer.Row{{Device: model.Device{Name: "a", HTTPPassword: "pw"}}}}
	if _, ok := plans.put(time.Now(), "s1", plan); !ok {
		t.Fatal("put")
	}
	deadline := time.Now().Add(2 * time.Second)
	for plans.size() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if plans.size() != 0 {
		t.Fatal("expired plan is still held")
	}
	if plan.Rows[0].Device.HTTPPassword != "" {
		t.Fatal("expired plan kept its password reference")
	}
}

// Why (review F3): a plan belongs to one login session, and a full store
// refuses the newcomer instead of evicting somebody else's plan.
func TestPendingPlansAreSessionBoundAndNotEvicted(t *testing.T) {
	plans := &importPlans{}
	now := time.Now()
	var first string
	for i := range importMaxPending {
		id, ok := plans.put(now, fmt.Sprint("s", i), importer.Plan{})
		if !ok {
			t.Fatalf("put %d refused", i)
		}
		if i == 0 {
			first = id
		}
	}
	if _, ok := plans.put(now, "newcomer", importer.Plan{}); ok {
		t.Fatal("a full store must refuse the new plan")
	}
	if _, ok := plans.take(now, first, "other-session"); ok {
		t.Fatal("another session took the plan")
	}
	if _, ok := plans.take(now, first, "s0"); !ok {
		t.Fatal("the owner lost the plan to someone else's failed attempt or to eviction")
	}
	if _, ok := plans.put(now, "s1", importer.Plan{}); !ok {
		t.Fatal("a session can replace its own plan even when the store is full")
	}
	if _, ok := plans.take(now.Add(importPlanTTL), "x", "s2"); ok {
		t.Fatal("unknown token")
	}
}

// Why (review F1): the HTML for a hostile file stays small.
func TestImportJunkFileGivesBoundedPage(t *testing.T) {
	e, _ := importEnv(t)
	c, _ := e.upload(t, "admin", part{"devices_csv", "devices.csv", "text/csv", strings.Repeat("x\n", 5000) + importCSV})
	if c != 422 {
		t.Fatalf("too many records must be refused: %d", c)
	}
	c, body := e.upload(t, "admin", part{"devices_csv", "devices.csv", "text/csv", strings.Repeat("x\n", 300) + importCSV})
	if c != 200 || len(body) > 64<<10 || !strings.Contains(body, "more lines with errors") {
		t.Fatalf("%d, %d bytes", c, len(body))
	}
}

// Why: with the step-up gone the audit trail is what remains after the fact.
// Checking files records who, which file names and the plan counts, and never
// the contents (the CSV carries a password).
func TestImportUploadIsAuditedWithoutSecrets(t *testing.T) {
	e, _ := importEnv(t)
	e.upload(t, "admin", part{"devices_csv", "devices.csv", "text/csv", importCSV})
	var found bool
	for _, ev := range e.aud.events {
		if ev.Action != "import.tasmoadmin.plan" {
			continue
		}
		found = true
		if ev.Actor == "" || !strings.Contains(ev.Detail, `"devices_csv":"devices.csv"`) || !strings.Contains(ev.Detail, `"create":2`) {
			t.Errorf("audit entry: %+v", ev)
		}
		if strings.Contains(ev.Detail+ev.Result, "s3cret-in-csv") || strings.Contains(ev.Detail, "10.0.0.9") {
			t.Errorf("contents leaked into the audit entry: %s", ev.Detail)
		}
	}
	if !found {
		t.Fatalf("upload not audited: %+v", e.aud.events)
	}
}
