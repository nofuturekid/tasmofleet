package auth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// v2Pages keeps the data of the last v2 pages it rendered.
type v2Pages struct {
	DefaultPages
	mu     sync.Mutex
	otp    OTPPageData
	detail UserDetailData
	login  LoginData
	setup  SetupAccountData
	pass   PasswordPageData
}

func (p *v2Pages) OneTimePassword(w http.ResponseWriter, r *http.Request, status int, d OTPPageData) {
	p.mu.Lock()
	p.otp = d
	p.mu.Unlock()
	p.DefaultPages.OneTimePassword(w, r, status, d)
}

func (p *v2Pages) UserDetail(w http.ResponseWriter, r *http.Request, status int, d UserDetailData) {
	p.mu.Lock()
	p.detail = d
	p.mu.Unlock()
	p.DefaultPages.UserDetail(w, r, status, d)
}

func (p *v2Pages) Login(w http.ResponseWriter, r *http.Request, status int, d LoginData) {
	p.mu.Lock()
	p.login = d
	p.mu.Unlock()
	p.DefaultPages.Login(w, r, status, d)
}

func (p *v2Pages) SetupAccount(w http.ResponseWriter, r *http.Request, status int, d SetupAccountData) {
	p.mu.Lock()
	p.setup = d
	p.mu.Unlock()
	p.DefaultPages.SetupAccount(w, r, status, d)
}

func (p *v2Pages) ChangePassword(w http.ResponseWriter, r *http.Request, status int, d PasswordPageData) {
	p.mu.Lock()
	p.pass = d
	p.mu.Unlock()
	p.DefaultPages.ChangePassword(w, r, status, d)
}

// auditMentions reports whether any audit row contains s.
func (e *env) auditMentions(s string) bool {
	e.audit.mu.Lock()
	defer e.audit.mu.Unlock()
	for _, ev := range e.audit.ev {
		if strings.Contains(ev.Detail+ev.Result+ev.Action+ev.Target, s) {
			return true
		}
	}
	return false
}

// Why (N4): a generated one-time password replaces the admin typing a
// password for someone else. It is shown once, never audited, signs in only
// into the forced password change, and stops working after 24 hours, so a
// forgotten hand-over does not stay a valid credential.
func TestOneTimePasswordResetSignsInOnceAndExpires(t *testing.T) {
	pages := &v2Pages{}
	e := localEnvWith(t, pages)
	adm := e.login("admin", testPassword)
	adm.stepUp(testPassword)
	opsBrowser := e.login("ops", opsPassword)
	r := adm.post("/users/local/ops", url.Values{"action": {"otp"}})
	if r.code != http.StatusOK || pages.otp.User != "ops" || len(pages.otp.Password) != generatedPasswordLen || pages.otp.Created {
		t.Fatalf("otp result: %d %+v", r.code, pages.otp)
	}
	if !strings.Contains(r.hdr.Get("Cache-Control"), "no-store") {
		t.Errorf("the page with the clear text may be cached: %q", r.hdr.Get("Cache-Control"))
	}
	otp := pages.otp.Password
	if e.auditMentions(otp) {
		t.Fatal("the one-time password is in the audit log")
	}
	if got := opsBrowser.get("/viewer"); got.code == http.StatusOK {
		t.Error("the user's old session survived the reset")
	}
	if r := postLocal(e.browser(), "192.0.2.200", "ops", opsPassword); r.code != http.StatusUnauthorized {
		t.Errorf("the old password still works: %d", r.code)
	}
	if r := postLocal(e.browser(), "192.0.2.201", "ops", otp); r.code != http.StatusSeeOther || r.loc != "/account" {
		t.Fatalf("sign-in with the one-time password: %d %q, want the forced change", r.code, r.loc)
	}
	e.clock.Advance(OTPValidity)
	r = postLocal(e.browser(), "192.0.2.202", "ops", otp)
	if r.code != http.StatusUnauthorized || !strings.Contains(pages.login.Error, "expired") {
		t.Fatalf("expired one-time password: %d %q", r.code, pages.login.Error)
	}
	if ev := e.audit.last(); ev.Result != "denied: one-time password expired" {
		t.Errorf("audit = %+v", ev)
	}
}

// Why: the own account keeps its "Change password" path; an admin must not
// hand themselves a one-time password (it would also end their own session
// in the middle of the action).
func TestOneTimePasswordNotForOwnAccount(t *testing.T) {
	e := localEnvWith(t, &v2Pages{})
	adm := e.login("admin", testPassword)
	adm.stepUp(testPassword)
	before := e.users.users["admin"]
	if r := adm.post("/users/local/admin", url.Values{"action": {"otp"}}); r.code != http.StatusBadRequest {
		t.Errorf("otp for self: %d", r.code)
	}
	if after := e.users.users["admin"]; after.PasswordGen != before.PasswordGen {
		t.Error("own password changed")
	}
}

// Why (N4): "Add user" offers a generated password; the account records who
// created it, and an empty typed password is refused instead of silently
// becoming a generated one the admin never sees.
func TestCreateUserWithOneTimePassword(t *testing.T) {
	pages := &v2Pages{}
	e := localEnvWith(t, pages)
	adm := e.login("admin", testPassword)
	adm.stepUp(testPassword)
	r := adm.post("/users", url.Values{"username": {"Carol"}, "role": {"operator"}, "otp": {"1"},
		"display_name": {" Carol C. "}, "email": {"carol@example.org"}})
	if r.code != http.StatusOK || pages.otp.User != "carol" || !pages.otp.Created {
		t.Fatalf("create: %d %+v %s", r.code, pages.otp, r.body)
	}
	u := e.users.users["carol"]
	if u.CreatedBy != "local:admin" || !u.CreatedAt.Equal(e.clock.Now()) || u.DisplayName != "Carol C." || u.Email != "carol@example.org" ||
		!u.MustChangePassword || !u.OTPExpiresAt.Equal(e.clock.Now().Add(OTPValidity)) || VerifyPassword(u.PasswordHash, pages.otp.Password) != nil {
		t.Fatalf("carol = %+v", u)
	}
	if e.auditMentions(pages.otp.Password) {
		t.Fatal("the one-time password is in the audit log")
	}
	if r := adm.post("/users", url.Values{"username": {"dave"}, "role": {"operator"}}); r.code != http.StatusBadRequest {
		t.Errorf("empty typed password: %d", r.code)
	}
	if _, ok := e.users.users["dave"]; ok {
		t.Error("dave created without a password")
	}
	if r := adm.post("/users", url.Values{"username": {"erin"}, "role": {"operator"}, "otp": {"1"}, "email": {"not an address"}}); r.code != http.StatusBadRequest {
		t.Errorf("bad e-mail: %d", r.code)
	}
}

// Why (N4): the detail page explains every refused action, and the reasons
// must match the rules the handlers enforce (self, last enabled admin).
func TestLocalGuardsExplainSelfAndLastAdmin(t *testing.T) {
	admin := store.LocalUser{Username: "a", Role: model.RoleAdmin}
	cases := []struct {
		name          string
		u             store.LocalUser
		self          bool
		admins        int
		role, disable bool
		reset         bool
		delete        bool
	}{
		{"other admin, two admins", admin, false, 2, true, true, true, true},
		{"last admin", admin, false, 1, false, false, true, false},
		{"self", admin, true, 2, false, false, false, false},
		{"disabled last admin can be enabled", store.LocalUser{Role: model.RoleAdmin, Disabled: true}, false, 1, true, true, true, true},
		{"operator", store.LocalUser{Role: model.RoleOperator}, false, 1, true, true, true, true},
	}
	for _, c := range cases {
		g := localGuards(c.u, c.self, c.admins)
		if g.Role.Allowed != c.role || g.Disable.Allowed != c.disable || g.Reset.Allowed != c.reset || g.Delete.Allowed != c.delete {
			t.Errorf("%s: %+v", c.name, g)
		}
		for _, x := range []Guard{g.Role, g.Disable, g.Reset, g.Delete, g.Profile} {
			if x.Allowed == (x.Reason != "") {
				t.Errorf("%s: a refused action needs a reason, an allowed one none: %+v", c.name, x)
			}
		}
	}
	if g := localGuards(admin, false, 1); g.Delete.Reason != MsgLastAdmin {
		t.Errorf("last-admin reason: %q", g.Delete.Reason)
	}
}

// Why (N4): the detail page lists the user's live sessions and recent
// activity and is open without step-up, like the list.
func TestUserDetailPage(t *testing.T) {
	pages := &v2Pages{}
	e := localEnvWith(t, pages)
	e.svc.o.AuditLog = auditList{e.audit}
	adm := e.login("admin", testPassword)
	e.login("ops", opsPassword)
	e.login("ops", opsPassword)
	if r := adm.get("/users/ops"); r.code != http.StatusOK {
		t.Fatalf("detail: %d %s", r.code, r.body)
	}
	d := pages.detail
	if d.User.Name != "ops" || len(d.Sessions) != 2 || !d.NeedStepUp || !d.Guards.Delete.Allowed {
		t.Fatalf("detail = %+v", d)
	}
	if len(d.Audit) == 0 || d.Audit[0].Actor != "local:ops" {
		t.Errorf("audit of ops = %+v", d.Audit)
	}
	if r := adm.get("/users/nobody"); r.code != http.StatusNotFound {
		t.Errorf("unknown user: %d", r.code)
	}
	if r := adm.get("/users/admin"); r.code != http.StatusOK || len(pages.detail.Sessions) != 1 || !pages.detail.Sessions[0].Current {
		t.Errorf("own detail: %d %+v", r.code, pages.detail.Sessions)
	}
	if r := e.login("ops", opsPassword).get("/users/ops"); r.code != http.StatusForbidden {
		t.Errorf("an operator opened a detail page: %d", r.code)
	}
}

// auditList serves the fake audit log like the store (actor filter, newest first).
type auditList struct{ a *fakeAudit }

func (l auditList) ListAudit(_ context.Context, f store.AuditFilter) ([]model.AuditEvent, error) {
	l.a.mu.Lock()
	defer l.a.mu.Unlock()
	var out []model.AuditEvent
	for i := len(l.a.ev) - 1; i >= 0 && (f.Limit == 0 || len(out) < f.Limit); i-- {
		if f.Actor == "" || l.a.ev[i].Actor == f.Actor {
			out = append(out, l.a.ev[i])
		}
	}
	return out, nil
}

// Why (N4): "End session" ends exactly one session of exactly that user,
// needs the step-up like every change, and is not the way to end one's own.
func TestEndSingleSession(t *testing.T) {
	pages := &v2Pages{}
	e := localEnvWith(t, pages)
	adm := e.login("admin", testPassword)
	b1, b2 := e.login("ops", opsPassword), e.login("ops", opsPassword)
	id1 := hashID(b1.cookie(CookieInsecure))
	form := url.Values{"subject": {"local:ops"}, "session": {id1}, "from": {"detail"}}
	if r := adm.post("/users/sessions", form); r.code != http.StatusForbidden {
		t.Fatalf("without step-up: %d", r.code)
	}
	adm.stepUp(testPassword)
	if r := adm.post("/users/sessions", url.Values{"subject": {"local:admin"}, "session": {id1}}); r.code != http.StatusNotFound {
		t.Errorf("session named under another subject: %d", r.code)
	}
	if r := adm.post("/users/sessions", form); r.code != http.StatusOK {
		t.Fatalf("end: %d %s", r.code, r.body)
	}
	if b1.get("/viewer").code == http.StatusOK {
		t.Error("the ended session still works")
	}
	if b2.get("/viewer").code != http.StatusOK {
		t.Error("the user's other session ended too")
	}
	own := hashID(adm.cookie(CookieInsecure))
	if r := adm.post("/users/sessions", url.Values{"subject": {"local:admin"}, "session": {own}}); r.code != http.StatusBadRequest {
		t.Errorf("own session: %d", r.code)
	}
	if adm.get("/viewer").code != http.StatusOK {
		t.Error("own session ended")
	}
}

// Why (N4): the account page edits the own profile; the input is checked and
// a forced password change comes first.
func TestOwnProfile(t *testing.T) {
	pages := &v2Pages{}
	e := localEnvWith(t, pages)
	ops := e.login("ops", opsPassword)
	r := ops.post("/account/profile", url.Values{"display_name": {"Olivia Ops"}, "email": {"ops@example.org"}})
	if r.code != http.StatusSeeOther || r.loc != "/account?profile=saved" {
		t.Fatalf("save: %d %q %s", r.code, r.loc, r.body)
	}
	if u := e.users.users["ops"]; u.DisplayName != "Olivia Ops" || u.Email != "ops@example.org" {
		t.Fatalf("ops = %+v", u)
	}
	if r := ops.get(r.loc); r.code != http.StatusOK || !pages.pass.ProfileSaved || pages.pass.Email != "ops@example.org" {
		t.Errorf("account page: %d %+v", r.code, pages.pass)
	}
	if r := ops.post("/account/profile", url.Values{"email": {"Ops <ops@example.org>"}}); r.code != http.StatusBadRequest || pages.pass.ErrField != "email" {
		t.Errorf("bad e-mail: %d %+v", r.code, pages.pass)
	}
	if r := ops.post("/account/profile", url.Values{"display_name": {strings.Repeat("x", 65)}}); r.code != http.StatusBadRequest || pages.pass.ErrField != "display_name" {
		t.Errorf("long name: %d %+v", r.code, pages.pass)
	}
	e.users.cli(func(m map[string]store.LocalUser) { u := m["ops"]; u.MustChangePassword = true; m["ops"] = u })
	if r := ops.post("/account/profile", url.Values{"display_name": {"x"}}); r.code != http.StatusForbidden {
		t.Errorf("profile before the forced change: %d", r.code)
	}
}

// Why (N11): the sign-in page shows how many attempts are left before the
// limiter delays them (a count, no timing), and the count is the limiter's.
func TestSignInShowsAttemptsLeft(t *testing.T) {
	pages := &v2Pages{}
	e := localEnvWith(t, pages)
	b := e.browser()
	for want := pairFree - 1; want >= 0; want-- {
		postLocal(b, "192.0.2.230", "ops", "wrong password!")
		if !pages.login.ShowAttemptsLeft || pages.login.AttemptsLeft != want {
			t.Fatalf("after a failure: show=%v left=%d, want %d", pages.login.ShowAttemptsLeft, pages.login.AttemptsLeft, want)
		}
	}
	if r := postLocal(b, "192.0.2.230", "ops", opsPassword); r.code != http.StatusTooManyRequests || pages.login.AttemptsLeft != 0 {
		t.Errorf("locked: %d %d", r.code, pages.login.AttemptsLeft)
	}
}

// Why: Remaining must agree with Allow, across all tiers, or the page would
// promise attempts the limiter then refuses.
func TestLimiterRemainingAgreesWithAllow(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	for _, recognised := range []bool{false, true} {
		l := &Limiter{}
		// Other users from the same network use up most of the network
		// budget; for a recognised client, its own attempts from other
		// networks use up most of the per-user budget.
		if recognised {
			for i := range recUserBudget - 2 {
				l.Allow(now, "other"+string(rune('a'+i)), "victim", true)
			}
		} else {
			for i := range netBudget - 2 {
				l.Allow(now, "net", "user"+string(rune('a'+i)), false)
			}
		}
		for {
			left := l.Remaining(now, "net", "victim", recognised)
			ok, _ := l.Allow(now, "net", "victim", recognised)
			if ok != (left > 0) {
				t.Fatalf("recognised=%v: Remaining=%d but Allow=%v", recognised, left, ok)
			}
			if !ok {
				break
			}
		}
	}
}

// Why (N11): an unchecked "Keep me signed in" must end the session with the
// browser (no Max-Age); checked keeps today's lifetime.
func TestKeepMeSignedInControlsTheCookieLifetime(t *testing.T) {
	e := localEnvWith(t, &v2Pages{})
	for _, remember := range []bool{false, true} {
		b := e.browser()
		b.do(http.MethodGet, "/login", nil, map[string]string{"X-Forwarded-For": "192.0.2.240"})
		form := url.Values{"csrf": {b.cookie(CookieInsecure + "-csrf")}, "username": {"ops"}, "password": {opsPassword}, "next": {"/"}}
		if remember {
			form.Set("remember", "1")
		}
		r := b.do(http.MethodPost, "/login/local", form, map[string]string{"X-Forwarded-For": "192.0.2.240"})
		c := setCookies(r)[CookieInsecure]
		if r.code != http.StatusSeeOther || c == nil {
			t.Fatalf("login: %d", r.code)
		}
		if persistent := c.MaxAge > 0; persistent != remember {
			t.Errorf("remember=%v: Max-Age %d", remember, c.MaxAge)
		}
	}
}

// Why (N11): someone whose session ran out learns why they are on the
// sign-in page; a visitor without a session sees no such banner.
func TestExpiredSessionIsAnnounced(t *testing.T) {
	pages := &v2Pages{}
	e := localEnvWith(t, pages)
	ops := e.login("ops", opsPassword)
	e.clock.Advance(e.svc.o.Idle + time.Minute)
	r := ops.get("/viewer")
	if r.code != http.StatusSeeOther || !strings.Contains(r.loc, "expired=1") {
		t.Fatalf("expired: %d %q", r.code, r.loc)
	}
	if ops.get(r.loc); !pages.login.Expired {
		t.Error("the sign-in page does not say the session expired")
	}
	if r := e.browser().get("/viewer"); strings.Contains(r.loc, "expired") {
		t.Errorf("a visitor without a session is told it expired: %q", r.loc)
	}
}

var setupLinkRe = regexp.MustCompile(`setup=\S*?/setup/account\?token=([A-Za-z0-9%]+)`)

// Why (N12): the first admin is set up through a one-time link from the
// container log. The link works only while the generated first-start
// password is unchanged, signs the admin in, is spent by use, and the token
// is never in the audit log.
func TestSetupTokenCreatesTheFirstAdmin(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	pages := &v2Pages{}
	e := localEnvWith(t, pages)
	e.users.cli(func(m map[string]store.LocalUser) { clear(m) })
	if created, err := e.svc.EnsureInitialAdmin(t.Context(), ""); err != nil || !created {
		t.Fatal(created, err)
	}
	m := setupLinkRe.FindStringSubmatch(logs.String())
	if m == nil || !strings.Contains(logs.String(), "created the initial admin account") {
		t.Fatalf("no setup link in the account line: %s", logs.String())
	}
	tok, _ := url.QueryUnescape(m[1])
	if u := e.users.users[InitialAdminName]; u.CreatedBy != CreatedByFirstStart {
		t.Errorf("created by %q", u.CreatedBy)
	}

	b := e.browser()
	if r := b.get("/setup/account"); r.code != http.StatusOK || !pages.setup.NeedToken || pages.setup.TokenInvalid {
		t.Fatalf("without token: %d %+v", r.code, pages.setup)
	}
	if r := b.get("/setup/account?token=wrong"); r.code != http.StatusForbidden || !pages.setup.TokenInvalid {
		t.Fatalf("wrong token: %d %+v", r.code, pages.setup)
	}
	if r := b.get("/setup/account?token=" + url.QueryEscape(tok)); r.code != http.StatusOK || pages.setup.NeedToken || pages.setup.Token != tok {
		t.Fatalf("valid token: %d %+v", r.code, pages.setup)
	}
	csrf := b.cookie(CookieInsecure + "-csrf")
	const pw = "first admin password"
	if r := b.do(http.MethodPost, "/setup/account", url.Values{"csrf": {csrf}, "token": {tok}, "password": {pw}, "confirm": {"different"}}, nil); r.code != http.StatusBadRequest || pages.setup.ErrField != "confirm" {
		t.Fatalf("mismatch: %d %+v", r.code, pages.setup)
	}
	r := b.do(http.MethodPost, "/setup/account", url.Values{"csrf": {csrf}, "token": {tok}, "password": {pw}, "confirm": {pw}, "display_name": {"Alice"}}, nil)
	if r.code != http.StatusSeeOther || r.loc != "/setup" {
		t.Fatalf("submit: %d %q %s", r.code, r.loc, r.body)
	}
	u := e.users.users[InitialAdminName]
	if VerifyPassword(u.PasswordHash, pw) != nil || u.MustChangePassword || u.DisplayName != "Alice" {
		t.Fatalf("admin = %+v", u)
	}
	if b.get("/viewer").code != http.StatusOK {
		t.Error("not signed in after the setup")
	}
	if _, err := os.Stat(filepath.Join(e.svc.o.DataDir, InitialPasswordFile)); !os.IsNotExist(err) {
		t.Errorf("the generated password file survived: %v", err)
	}
	// Spent: the same link now leads to the sign-in page, and so does a submit.
	if r := e.browser().get("/setup/account?token=" + url.QueryEscape(tok)); r.code != http.StatusSeeOther || r.loc != "/login" {
		t.Errorf("reused link: %d %q", r.code, r.loc)
	}
	if e.auditMentions(tok) {
		t.Error("the setup token is in the audit log")
	}
	// A restart does not print a new link once the password is set.
	logs.Reset()
	if _, err := e.svc.EnsureInitialAdmin(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "token=") {
		t.Errorf("a set-up installation printed a setup link: %s", logs.String())
	}
}

// Why (N12): while the first admin has not set a password, every start
// prints a fresh link (the old one died with the process); once anyone set
// it (here: the forced change after signing in with the generated password),
// the link is dead even though it was never used.
func TestSetupTokenDiesWithAnyPasswordChange(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	e := localEnvWith(t, &v2Pages{})
	e.users.cli(func(m map[string]store.LocalUser) { clear(m) })
	if _, err := e.svc.EnsureInitialAdmin(t.Context(), "operator chosen 1"); err != nil {
		t.Fatal(err)
	}
	first := setupLinkRe.FindStringSubmatch(logs.String())
	logs.Reset()
	if _, err := e.svc.EnsureInitialAdmin(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	second := setupLinkRe.FindStringSubmatch(logs.String())
	if first == nil || second == nil || first[1] == second[1] {
		t.Fatalf("restart while pending: %v %v", first, second)
	}
	tok, _ := url.QueryUnescape(second[1])
	// Disabled meanwhile: the link stops working (the account cannot sign in).
	e.users.cli(func(m map[string]store.LocalUser) { u := m["admin"]; u.Disabled = true; m["admin"] = u })
	if r := e.browser().get("/setup/account?token=" + url.QueryEscape(tok)); r.code != http.StatusSeeOther {
		t.Errorf("link of a disabled admin: %d", r.code)
	}
	e.users.cli(func(m map[string]store.LocalUser) { u := m["admin"]; u.Disabled = false; m["admin"] = u })
	b := e.login("admin", "operator chosen 1")
	if r := b.post("/account/password", url.Values{"current": {"operator chosen 1"}, "new": {"a new admin password"}, "confirm": {"a new admin password"}}); r.code != http.StatusSeeOther {
		t.Fatalf("change: %d", r.code)
	}
	if r := e.browser().get("/setup/account?token=" + url.QueryEscape(tok)); r.code != http.StatusSeeOther {
		t.Errorf("link after the password was set elsewhere: %d", r.code)
	}
}

// Why (N12): a later admin reset that again forces a password change is not
// a first start: no setup link may appear for it.
func TestNoSetupLinkAfterALaterReset(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	e := localEnvWith(t, &v2Pages{})
	// admin exists at generation 1 without a pending change; a reset with a
	// forced change moves it to generation 2.
	e.users.cli(func(m map[string]store.LocalUser) { u := m["admin"]; u.PasswordGen = 1; m["admin"] = u })
	if err := e.svc.Accounts().SetPassword(t.Context(), "admin", "reset by the cli 1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.EnsureInitialAdmin(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "token=") {
		t.Errorf("setup link after a later reset: %s", logs.String())
	}
}

// Why: an empty password used to be refused (too short). Since create
// generates a one-time password for an empty one, Add (CLI, no way to show
// the generated text) must still refuse it, or the account would end up with
// a password nobody knows.
func TestAddRefusesEmptyPassword(t *testing.T) {
	a, users, _, _, _ := newAccounts(t)
	if err := a.Add(t.Context(), "carol", "", model.RoleOperator, true); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("Add with empty password: %v, want ErrWeakPassword", err)
	}
	if _, err := users.GetLocalUser(t.Context(), "carol"); err == nil {
		t.Error("the account was created anyway")
	}
}

// setupEnv is an installation at first start with a generated admin, its
// setup token from the log, and a hook right before the password write.
func setupEnv(t *testing.T) (*env, *hookedUsers, *v2Pages, string, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	pages := &v2Pages{}
	var h *hookedUsers
	e := newEnv(t, ModeLocal, func(o *Options) {
		o.Proxy.PublicURL = ""
		o.DataDir = t.TempDir()
		o.Pages = pages
		h = &hookedUsers{fakeUsers: o.LocalUsers.(*fakeUsers)}
		o.LocalUsers = h
	})
	e.users.cli(func(m map[string]store.LocalUser) { clear(m) })
	if created, err := e.svc.EnsureInitialAdmin(t.Context(), ""); err != nil || !created {
		t.Fatal(created, err)
	}
	m := setupLinkRe.FindStringSubmatch(logs.String())
	if m == nil {
		t.Fatalf("no setup link: %s", logs.String())
	}
	tok, _ := url.QueryUnescape(m[1])
	return e, h, pages, tok, &logs
}

// Why (security review, N12): the token is single-use on its own. Two
// submits racing with the same link must not both get past the token check
// (the second would choose the admin password, or at least burn a PBKDF2
// hash and reach the store); the first claims it, the second is refused as
// an invalid token and nothing it sent reaches the account.
func TestSetupTokenIsClaimedAtomically(t *testing.T) {
	e, h, pages, tok, _ := setupEnv(t)
	first, second := e.browser(), e.browser()
	for _, b := range []*browser{first, second} {
		if r := b.get("/setup/account?token=" + url.QueryEscape(tok)); r.code != http.StatusOK {
			t.Fatalf("open link: %d", r.code)
		}
	}
	form := func(b *browser, pw string) url.Values {
		return url.Values{"csrf": {b.cookie(CookieInsecure + "-csrf")}, "token": {tok}, "password": {pw}, "confirm": {pw}}
	}
	const firstPW, secondPW = "first racer password", "second racer password"
	var raced resp
	// Right before the first submit writes the password, the second arrives.
	h.before = func() { raced = second.do(http.MethodPost, "/setup/account", form(second, secondPW), nil) }
	if r := first.do(http.MethodPost, "/setup/account", form(first, firstPW), nil); r.code != http.StatusSeeOther || r.loc != "/setup" {
		t.Fatalf("first submit: %d %q", r.code, r.loc)
	}
	if raced.code != http.StatusForbidden || !pages.setup.TokenInvalid {
		t.Fatalf("concurrent second submit: %d %q, want 403 invalid token", raced.code, raced.loc)
	}
	if u := e.users.users[InitialAdminName]; VerifyPassword(u.PasswordHash, firstPW) != nil || u.PasswordGen != 2 {
		t.Fatalf("admin password is not the first submit's: gen %d", u.PasswordGen)
	}
	if second.get("/viewer").code == http.StatusOK {
		t.Error("the refused submit got a session")
	}
}

// Why (security review, N12): a form error must not burn the link (the admin
// would have to restart the container), but the link must still be single
// use afterwards; the page keeps the token out of Referer headers; the log
// carries the link by design but never the generated password.
func TestSetupTokenSurvivesFormErrorsAndStaysPrivate(t *testing.T) {
	e, _, pages, tok, logs := setupEnv(t)
	pw, err := os.ReadFile(filepath.Join(e.svc.o.DataDir, InitialPasswordFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), strings.TrimSpace(string(pw))) {
		t.Fatal("the generated first-start password is in the log")
	}
	b := e.browser()
	r := b.get("/setup/account?token=" + url.QueryEscape(tok))
	if got := r.hdr.Get("Referrer-Policy"); got != "origin" {
		t.Errorf("Referrer-Policy on the setup page = %q", got)
	}
	csrf := b.cookie(CookieInsecure + "-csrf")
	if r := b.do(http.MethodPost, "/setup/account", url.Values{"csrf": {csrf}, "token": {tok}, "password": {"short"}, "confirm": {"short"}}, nil); r.code != http.StatusBadRequest || pages.setup.ErrField != "password" {
		t.Fatalf("weak password: %d %+v", r.code, pages.setup)
	}
	const good = "a good first password"
	if r := b.do(http.MethodPost, "/setup/account", url.Values{"csrf": {csrf}, "token": {tok}, "password": {good}, "confirm": {good}}, nil); r.code != http.StatusSeeOther || r.loc != "/setup" {
		t.Fatalf("retry after a form error: %d %q", r.code, r.loc)
	}
	if e.svc.setupTokenValid(t.Context(), tok) {
		t.Error("the token is still valid after a successful setup")
	}
}

// Why (security triage, N12): the setup link hands whoever reads the
// container log (shipped to Loki, readable by more people than the admins)
// the password of an admin account. It may only ever do that for the account
// the first start created. An account that is later created under the same
// name, by an admin (one-time password) or by the CLI with a forced change,
// also sits at generation 1 with a pending change; neither the link already
// in the log nor a fresh one at the next start may take it over.
func TestSetupLinkNeverAppliesToARecreatedAdmin(t *testing.T) {
	e, _, _, tok, logs := setupEnv(t)
	ctx := t.Context()
	acc := e.svc.Accounts()
	if err := acc.Add(ctx, "alice", "alice password 1", model.RoleAdmin, false); err != nil {
		t.Fatal(err)
	}
	// The operator works as "alice", drops the unused first-start "admin"
	// and later creates a new "admin" for someone else.
	if err := acc.Delete(ctx, InitialAdminName); err != nil {
		t.Fatal(err)
	}
	if _, err := acc.Create(ctx, NewUser{Name: InitialAdminName, Role: model.RoleAdmin}, SubjectOf("alice")); err != nil {
		t.Fatal(err)
	}
	if u := e.users.users[InitialAdminName]; u.PasswordGen != 1 || !u.MustChangePassword {
		t.Fatalf("precondition: recreated admin = gen %d must %v", u.PasswordGen, u.MustChangePassword)
	}
	b := e.browser()
	if r := b.get("/setup/account?token=" + url.QueryEscape(tok)); r.code == http.StatusOK {
		t.Fatalf("the old link from the log opens the form for the recreated admin")
	}
	const evil = "chosen by a log reader"
	b.do(http.MethodPost, "/setup/account", url.Values{"csrf": {b.cookie(CookieInsecure + "-csrf")}, "token": {tok}, "password": {evil}, "confirm": {evil}}, nil)
	if VerifyPassword(e.users.users[InitialAdminName].PasswordHash, evil) == nil || b.get("/viewer").code == http.StatusOK {
		t.Fatal("the old link set the recreated admin's password")
	}
	for _, by := range []string{"admin", "cli"} {
		if by == "cli" {
			if err := acc.Delete(ctx, InitialAdminName); err != nil {
				t.Fatal(err)
			}
			if err := acc.Add(ctx, InitialAdminName, "cli chosen password", model.RoleAdmin, true); err != nil {
				t.Fatal(err)
			}
		}
		logs.Reset()
		if _, err := e.svc.EnsureInitialAdmin(ctx, ""); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(logs.String(), "token=") {
			t.Errorf("restart printed a setup link for an admin created by %s: %s", by, logs.String())
		}
	}
}

// Why (security triage, N12): "created by the first start" must not be read
// from a value someone else can also produce. OIDC and proxy subjects carry
// no prefix, so an admin whose subject is literally "first-start" records
// exactly that as the creator of a new "admin"; that account must still
// never take the setup link, at once or after a restart.
func TestSetupLinkIgnoresACreatorNamedFirstStart(t *testing.T) {
	e, _, _, tok, logs := setupEnv(t)
	ctx := t.Context()
	acc := e.svc.Accounts()
	if err := acc.Add(ctx, "alice", "alice password 1", model.RoleAdmin, false); err != nil {
		t.Fatal(err)
	}
	if err := acc.Delete(ctx, InitialAdminName); err != nil {
		t.Fatal(err)
	}
	if _, err := acc.Create(ctx, NewUser{Name: InitialAdminName, Role: model.RoleAdmin}, CreatedByFirstStart); err != nil {
		t.Fatal(err)
	}
	if r := e.browser().get("/setup/account?token=" + url.QueryEscape(tok)); r.code == http.StatusOK {
		t.Fatal("the old link opens the form for an admin created by a principal named first-start")
	}
	logs.Reset()
	if _, err := e.svc.EnsureInitialAdmin(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "token=") {
		t.Errorf("restart printed a setup link for it: %s", logs.String())
	}
}

// Why (#429): on plain HTTP via an IP a browser sends no Sec-Fetch-Site, so the
// CSRF check falls back to the Origin header. Under the Fetch spec a form POST
// from a page with Referrer-Policy no-referrer carries "Origin: null", which
// the check rightly rejects, so the first-run setup could not be finished. The
// setup page must therefore use a policy under which the browser serialises a
// real Origin (same-origin POST), while "Origin: null" and foreign origins
// stay rejected: the protection is not loosened.
func TestSetupFormWorksOverPlainHTTPWithoutFetchMetadata(t *testing.T) {
	e, _, _, tok, _ := setupEnv(t)
	b := e.browser()
	page := b.get("/setup/account?token=" + url.QueryEscape(tok))
	policy := page.hdr.Get("Referrer-Policy")
	// Fetch spec, "serializing a request origin": no-referrer gives "null";
	// any other policy keeps the origin on a same-origin non-CORS POST.
	browserOrigin := e.srv.URL
	if policy == "no-referrer" {
		browserOrigin = "null"
	}
	const pw = "plain http first password"
	form := func() url.Values {
		return url.Values{"csrf": {b.cookie(CookieInsecure + "-csrf")}, "token": {tok}, "password": {pw}, "confirm": {pw}}
	}
	noFetchMeta := func(origin string) map[string]string {
		return map[string]string{"Sec-Fetch-Site": "", "Origin": origin}
	}
	for _, bad := range []string{"null", "http://evil.example"} {
		if r := b.do(http.MethodPost, "/setup/account", form(), noFetchMeta(bad)); r.code != http.StatusForbidden || !strings.Contains(r.body, "cross-origin") {
			t.Fatalf("Origin %q must stay rejected: %d %q", bad, r.code, r.body)
		}
	}
	if r := b.do(http.MethodPost, "/setup/account", form(), noFetchMeta(browserOrigin)); r.code != http.StatusSeeOther || r.loc != "/setup" {
		t.Fatalf("setup POST as the browser sends it (policy %q, Origin %q): %d %q %q", policy, browserOrigin, r.code, r.loc, r.body)
	}
}
