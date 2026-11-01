package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/testutil/oidcmock"
)

var (
	alice = oidcmock.User{Subject: "sub-alice", Username: "alice", Email: "alice@example.test", Groups: []string{"family", "fleet-admins"}}
	bob   = oidcmock.User{Subject: "sub-bob", Username: "bob", Groups: []string{"fleet-ops"}}
	eve   = oidcmock.User{Subject: "sub-eve", Username: "eve", Groups: []string{"family"}}
)

func TestRoleMappingHighestWins(t *testing.T) {
	m := RoleMapping{AdminGroups: []string{"a"}, OperatorGroups: []string{"o"}}
	cases := []struct {
		groups []string
		want   model.Role
	}{
		{nil, model.RoleNone},
		{[]string{"x"}, model.RoleNone},
		{[]string{"v"}, model.RoleNone}, // there is no viewer role
		{[]string{"x", "o"}, model.RoleOperator},
		{[]string{"o", "a"}, model.RoleAdmin},
		{[]string{"A"}, model.RoleNone}, // group names are case-sensitive
	}
	for _, c := range cases {
		if got := m.Role(c.groups); got != c.want {
			t.Errorf("Role(%v) = %v, want %v", c.groups, got, c.want)
		}
	}
}

func TestOIDCLoginGrantsSessionAndRole(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	b := e.browser()
	r := b.get(b.startOIDC("/viewer?x=1", alice))
	if r.code != http.StatusSeeOther || r.loc != "/viewer?x=1" {
		t.Fatalf("callback: %d %q %s", r.code, r.loc, r.body)
	}
	if r = b.get("/viewer"); r.code != http.StatusOK || r.body != "ok sub-alice" {
		t.Fatalf("viewer after login: %d %q", r.code, r.body)
	}
	s := e.sessions.all()
	if len(s) != 1 || s[0].Principal.Role != model.RoleAdmin || s[0].Principal.Name != "alice" || s[0].IDToken == "" {
		t.Fatalf("session %+v", s)
	}
	if a := e.audit.last(); a.Action != "auth.login.oidc" || a.Result != "ok" || a.Actor != "sub-alice" {
		t.Fatalf("audit %+v", a)
	}
}

// An attacker must not be able to steer the redirect_uri (and with it the
// authorization code) by forging forwarded headers, even via a trusted proxy.
func TestRedirectURIComesFromPublicURLOnly(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	r := e.browser().do(http.MethodGet, "/login/sso", nil, map[string]string{
		"X-Forwarded-Host": "evil.example", "X-Forwarded-Proto": "https", "X-Forwarded-Prefix": "/evil", "Host": "evil.example",
	})
	u, err := url.Parse(r.loc)
	if err != nil || r.code != http.StatusFound {
		t.Fatalf("%d %q", r.code, r.loc)
	}
	if got := u.Query().Get("redirect_uri"); got != e.srv.URL+"/oidc/callback" {
		t.Fatalf("redirect_uri %q", got)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || len(q.Get("state")) < 43 || len(q.Get("nonce")) < 43 {
		t.Fatalf("PKCE S256 and 32-byte state/nonce required: %v", q)
	}
}

func TestOIDCCallbackRejects(t *testing.T) {
	cases := []struct {
		name   string
		attack func(t *testing.T, e *env, b *browser, callback string) string
		want   int
	}{
		{"PKCE verifier mismatch", func(t *testing.T, e *env, b *browser, cb string) string {
			// Swap the verifier in the flow cookie: the IdP must refuse the code.
			f, err := e.svc.readFlow(cookieReq(b, e.flowName()))
			if err != nil {
				t.Fatal(err)
			}
			// Always change the first character (a random verifier starting
			// with "x" made a plain "x" swap a no-op 1 time in 64).
			first := "x"
			if f.Verifier[0] == 'x' {
				first = "y"
			}
			f.Verifier = first + f.Verifier[1:]
			b.setCookie(e.flowName(), encodeFlow(t, e.svc, f))
			return cb
		}, http.StatusBadGateway},
		{"wrong nonce in ID token", func(_ *testing.T, e *env, _ *browser, cb string) string {
			e.idp.TamperNextIDToken(func(c map[string]any) { c["nonce"] = "replayed-nonce" })
			return cb
		}, http.StatusBadRequest},
		{"ID token for another audience", func(_ *testing.T, e *env, _ *browser, cb string) string {
			e.idp.TamperNextIDToken(func(c map[string]any) { c["aud"] = "other-app" })
			return cb
		}, http.StatusBadGateway},
		{"state mismatch (login CSRF)", func(_ *testing.T, _ *env, _ *browser, cb string) string {
			u, _ := url.Parse(cb)
			q := u.Query()
			q.Set("state", "attacker-state")
			u.RawQuery = q.Encode()
			return u.String()
		}, http.StatusBadRequest},
		{"callback in a browser that never started the flow", func(_ *testing.T, e *env, b *browser, cb string) string {
			b.setCookie(e.flowName(), "")
			return cb
		}, http.StatusBadRequest},
		{"flow older than 10 minutes", func(_ *testing.T, e *env, _ *browser, cb string) string {
			e.clock.Advance(11 * time.Minute)
			return cb
		}, http.StatusBadRequest},
		{"IdP error response", func(_ *testing.T, _ *env, _ *browser, cb string) string {
			u, _ := url.Parse(cb)
			q := u.Query()
			q.Del("code")
			q.Set("error", "access_denied")
			u.RawQuery = q.Encode()
			return u.String()
		}, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, ModeOIDC)
			b := e.browser()
			cb := c.attack(t, e, b, b.startOIDC("/", alice))
			r := b.get(cb)
			if r.code != c.want {
				t.Fatalf("status %d, want %d (%s)", r.code, c.want, r.body)
			}
			if len(e.sessions.all()) != 0 || b.cookie(CookieInsecure) != "" {
				t.Fatal("a failed callback must not create a session")
			}
		})
	}
}

func cookieReq(b *browser, name string) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: name, Value: b.cookie(name)})
	return r
}

func encodeFlow(t *testing.T, svc *Service, f flow) string {
	t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	body := base64.RawURLEncoding.EncodeToString(b)
	return body + "." + svc.flowMAC(body)
}

// A captured callback URL (code + state) must not log anyone in a second time.
func TestCallbackReplayFails(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	b := e.browser()
	cb := b.startOIDC("/", alice)
	if r := b.get(cb); r.code != http.StatusSeeOther {
		t.Fatalf("first callback %d", r.code)
	}
	// Same browser again (flow cookie consumed) and a fresh attacker browser.
	for _, br := range []*browser{b, e.browser()} {
		before := len(e.sessions.all())
		if r := br.get(cb); r.code != http.StatusBadRequest {
			t.Fatalf("replayed callback: %d", r.code)
		}
		if len(e.sessions.all()) != before {
			t.Fatal("replay created a session")
		}
	}
}

func TestNoMatchingGroupMeans403AndNoSession(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	b := e.browser()
	r := b.get(b.startOIDC("/", eve))
	if r.code != http.StatusForbidden || !strings.Contains(r.body, "none of your groups") {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if len(e.sessions.all()) != 0 || b.cookie(CookieInsecure) != "" {
		t.Fatal("user without a mapped group got a session")
	}
	if a := e.audit.last(); !strings.HasPrefix(a.Result, "denied") {
		t.Fatalf("denied login not audited: %+v", a)
	}
}

func TestRoleEnforcement(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	b := e.browser()
	b.get(b.startOIDC("/", bob)) // operator
	if r := b.get("/viewer"); r.code != http.StatusOK {
		t.Fatalf("operator denied viewer page: %d", r.code)
	}
	tok := b.csrfToken()
	if r := b.do(http.MethodPost, "/admin-action", url.Values{"csrf": {tok}}, nil); r.code != http.StatusForbidden {
		t.Fatalf("operator reached admin action: %d", r.code)
	}
}

// Why: a 403 that only says "not enough role" leaves the user guessing which role
// is missing; the page names the role needed and the one the account has.
func TestAccessDeniedNamesTheRoles(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	b := e.browser()
	b.get(b.startOIDC("/", bob)) // operator
	r := b.do(http.MethodPost, "/admin-action", url.Values{"csrf": {b.csrfToken()}}, nil)
	if r.code != http.StatusForbidden || !strings.Contains(r.body, "needs the admin role") || !strings.Contains(r.body, "operator role") {
		t.Fatalf("403 does not name the roles: %d %q", r.code, r.body)
	}
}

// Why: a login page that appears after sign-out or from a protected link must
// say why, or it looks like the app lost the session for no reason.
func TestLoginExplainsWhyItAppeared(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	b := e.browser()
	if r := b.get("/login?signedout=1"); !strings.Contains(r.body, "You have been signed out.") {
		t.Errorf("no sign-out notice: %q", r.body)
	}
	if r := b.get("/login?next=%2Fdevices"); !strings.Contains(r.body, "Sign in to continue.") {
		t.Errorf("no notice for a protected link: %q", r.body)
	}
	if r := b.get("/login"); strings.Contains(r.body, "role=\"status\"") {
		t.Errorf("plain /login carries a notice: %q", r.body)
	}
}

func TestUnauthenticatedRequests(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	b := e.browser()
	if r := b.get("/viewer?a=b"); r.code != http.StatusSeeOther || r.loc != "/login?next=%2Fviewer%3Fa%3Db" {
		t.Fatalf("browser GET: %d %q", r.code, r.loc)
	}
	r := b.do(http.MethodGet, "/viewer", nil, map[string]string{"HX-Request": "true"})
	if r.code != http.StatusUnauthorized || !strings.HasPrefix(r.hdr.Get("HX-Redirect"), "/login") {
		t.Fatalf("HTMX: %d %q", r.code, r.hdr.Get("HX-Redirect"))
	}
}

func TestLogoutIsRPInitiated(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	b := e.browser()
	b.get(b.startOIDC("/", alice))
	idToken := e.sessions.all()[0].IDToken
	tok := b.csrfToken()
	r := b.do(http.MethodPost, "/logout", url.Values{"csrf": {tok}}, nil)
	u, _ := url.Parse(r.loc)
	if r.code != http.StatusSeeOther || !strings.HasPrefix(r.loc, e.idp.Issuer()+"end-session/") {
		t.Fatalf("logout: %d %q", r.code, r.loc)
	}
	if u.Query().Get("id_token_hint") != idToken || u.Query().Get("post_logout_redirect_uri") != e.srv.URL+"/" {
		t.Fatalf("logout params %v", u.Query())
	}
	if len(e.sessions.all()) != 0 {
		t.Fatal("session survived logout")
	}
	if r := b.get("/viewer"); r.code != http.StatusSeeOther {
		t.Fatalf("still logged in after logout: %d", r.code)
	}
}

func TestLogoutNeedsCSRFToken(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	b := e.browser()
	b.get(b.startOIDC("/", alice))
	if r := b.do(http.MethodPost, "/logout", url.Values{}, nil); r.code != http.StatusForbidden {
		t.Fatalf("logout without token: %d", r.code)
	}
	r := b.do(http.MethodPost, "/logout", url.Values{"csrf": {b.csrfToken()}}, map[string]string{"Sec-Fetch-Site": "cross-site"})
	if r.code != http.StatusForbidden {
		t.Fatalf("cross-site logout: %d", r.code)
	}
	if len(e.sessions.all()) != 1 {
		t.Fatal("forged logout ended the session")
	}
}

func TestLoginNotReadyBeforeDiscovery(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	svc, err := New(Options{Mode: ModeOIDC, Proxy: e.proxy, Sessions: e.sessions, Audit: e.audit,
		OIDC: OIDCOptions{Issuer: e.idp.Issuer(), ClientID: "tasmofleet", HTTPClient: e.idp.Client()}})
	if err != nil {
		t.Fatal(err)
	}
	if svc.Ready() {
		t.Fatal("ready before discovery: /readyz must report not ready")
	}
	w := httptest.NewRecorder()
	svc.Routes().ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/login", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("login before discovery: %d", w.Code)
	}
	if err := svc.Discover(t.Context()); err != nil || !svc.Ready() {
		t.Fatalf("discover: %v", err)
	}
}

// Post-login targets are local paths only; anything else falls back to "/".
func TestOpenRedirectAfterLogin(t *testing.T) {
	cases := map[string]string{
		"//evil.example/x":   "/",
		"https://evil":       "/",
		`/\evil.example`:     "/",
		"/devices/7?tab=fw":  "/devices/7?tab=fw",
		"javascript:alert()": "/",
	}
	for next, want := range cases {
		t.Run(next, func(t *testing.T) {
			e := newEnv(t, ModeOIDC)
			b := e.browser()
			if r := b.get(b.startOIDC(next, alice)); r.loc != want {
				t.Fatalf("oidc: Location %q, want %q", r.loc, want)
			}
			b2 := e.browser()
			if r := localLogin(b2, "admin", testPassword, next); r.loc != want {
				t.Fatalf("local: Location %q, want %q", r.loc, want)
			}
		})
	}
}

func localLogin(b *browser, user, pass, next string) resp {
	b.t.Helper()
	b.get("/login")
	return b.do(http.MethodPost, "/login/local", url.Values{
		"csrf": {b.cookie(CookieInsecure + "-csrf")}, "username": {user}, "password": {pass}, "next": {next},
	}, nil)
}

func TestHeaderMode(t *testing.T) {
	e := newEnv(t, ModeHeader)
	h := e.handler()
	get := func(peer string, hdr map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/viewer", nil)
		r.RemoteAddr = peer
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	ident := map[string]string{"X-authentik-username": "carol", "X-authentik-groups": "family|fleet-ops|x"}

	if w := get("127.0.0.1:4000", ident); w.Code != http.StatusOK || w.Body.String() != "ok carol" {
		t.Fatalf("trusted proxy: %d %s", w.Code, w.Body)
	}
	// Spoofing from inside the LAN: untrusted peer, forged identity, and an
	// X-Forwarded-For claiming to be the proxy. Must stay unauthenticated.
	spoof := map[string]string{"X-authentik-username": "mallory", "X-authentik-groups": "fleet-admins", "X-Forwarded-For": "127.0.0.1"}
	if w := get("192.168.1.66:4000", spoof); w.Code != http.StatusSeeOther {
		t.Fatalf("spoofed headers accepted: %d %s", w.Code, w.Body)
	}
	// Trusted proxy, user without mapped group: authenticated but forbidden.
	if w := get("127.0.0.1:4000", map[string]string{"X-authentik-username": "dave", "X-authentik-groups": "family"}); w.Code != http.StatusForbidden {
		t.Fatalf("no group: %d", w.Code)
	}
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("X-authentik-username", "carol")
	r.Header.Set("X-authentik-groups", " a | fleet-ops ||b ")
	p, err := e.svc.Principal(r)
	if err != nil || !slices.Equal(p.Groups, []string{"a", "fleet-ops", "b"}) || p.Role != model.RoleOperator || p.Method != ModeHeader {
		t.Fatalf("principal %+v %v", p, err)
	}
	if len(e.sessions.all()) != 0 {
		t.Fatal("header users must not get a session")
	}
}

func TestHeaderModeLogoutAndCSRF(t *testing.T) {
	e := newEnv(t, ModeHeader)
	h := e.handler()
	post := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/logout", strings.NewReader(url.Values{"csrf": {token}}.Encode()))
		r.RemoteAddr = "127.0.0.1:1"
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("X-authentik-username", "carol")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := post("guess"); w.Code != http.StatusForbidden {
		t.Fatalf("header user POST without valid token: %d", w.Code)
	}
	w := post(e.svc.headerToken("carol"))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "https://auth.example/outpost.goauthentik.io/sign_out" {
		t.Fatalf("logout: %d %q", w.Code, w.Header().Get("Location"))
	}
	if e.svc.headerToken("carol") == e.svc.headerToken("dave") {
		t.Fatal("header CSRF tokens must be per user")
	}
}

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^\$pbkdf2-sha256\$i=600000\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$`).MatchString(h) {
		t.Fatalf("hash format %q (spec: $pbkdf2-sha256$i=600000$<16-byte salt>$<hash>)", h)
	}
	h2, _ := HashPassword(testPassword)
	if h == h2 {
		t.Fatal("same salt twice")
	}
	if err := VerifyPassword(h, testPassword); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPassword(h, testPassword+"x"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := HashPassword("short-pass1"); !errors.Is(err, ErrWeakPassword) {
		t.Fatal("11-character password accepted")
	}
	for _, bad := range []string{"", "plain", "$pbkdf2-sha256$i=0$AAAA$AAAA", "$pbkdf2-sha1$i=1000$AAAA$AAAA", "$pbkdf2-sha256$i=99999999999$AAAA$AAAA"} {
		if err := VerifyPassword(bad, "x"); !errors.Is(err, ErrInvalidHash) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	// Unknown users are checked against dummyHash; it must cost a full KDF
	// run (valid format), or response times would reveal valid usernames.
	if err := VerifyPassword(dummyHash, testPassword); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("dummyHash: %v", err)
	}
}

func TestLocalLogin(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	b := e.browser()
	if r := localLogin(b, "admin", "wrong password!", "/"); r.code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", r.code)
	}
	if r := localLogin(b, "nobody", testPassword, "/"); r.code != http.StatusUnauthorized {
		t.Fatalf("unknown user: %d", r.code)
	}
	r := localLogin(b, "admin", testPassword, "/viewer")
	if r.code != http.StatusSeeOther || r.loc != "/viewer" {
		t.Fatalf("login: %d %q", r.code, r.loc)
	}
	s := e.sessions.all()
	if len(s) != 1 || s[0].Principal.Subject != "local:admin" || s[0].Principal.Role != model.RoleAdmin {
		t.Fatalf("session %+v", s)
	}
	if a := e.audit.last(); a.Action != "auth.login.local" || a.Result != "ok" || a.RemoteAddr != "127.0.0.1" {
		t.Fatalf("audit %+v", a)
	}
	// Logout of a local admin returns to the sign-in page, not to the IdP.
	if r := b.do(http.MethodPost, "/logout", url.Values{"csrf": {b.csrfToken()}}, nil); r.loc != "/login?signedout=1" {
		t.Fatalf("local logout: %q", r.loc)
	}
}

func TestLocalLoginNeedsCSRF(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	b := e.browser()
	b.get("/login")
	form := url.Values{"username": {"admin"}, "password": {testPassword}}
	if r := b.do(http.MethodPost, "/login/local", form, nil); r.code != http.StatusForbidden {
		t.Fatalf("no token: %d", r.code)
	}
	form.Set("csrf", b.cookie(CookieInsecure+"-csrf"))
	if r := b.do(http.MethodPost, "/login/local", form, map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}); r.code != http.StatusForbidden {
		t.Fatalf("cross-site login: %d", r.code)
	}
	// Attacker-made token without the matching cookie (fresh browser).
	b2 := e.browser()
	if r := b2.do(http.MethodPost, "/login/local", form, nil); r.code != http.StatusForbidden {
		t.Fatalf("token without cookie: %d", r.code)
	}
	if len(e.sessions.all()) != 0 {
		t.Fatal("session created")
	}
}

// Brute force on break-glass from one address: attempt 6 is refused, then
// the delay doubles with every further attempt.
func TestLocalLoginRateLimit(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	b := e.browser()
	for i := 1; i <= 5; i++ {
		if r := localLogin(b, "admin", "wrong password!", "/"); r.code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, r.code)
		}
	}
	// Attempt 6 is refused even with the right password, before any hashing.
	if r := localLogin(b, "admin", testPassword, "/"); r.code != http.StatusTooManyRequests {
		t.Fatalf("attempt 6: %d", r.code)
	}
	if a := e.audit.last(); a.Result != "denied: rate limited" {
		t.Fatalf("rate limit not audited: %+v", a)
	}
	steps := []struct {
		advance time.Duration
		want    int
	}{
		{time.Minute, http.StatusUnauthorized},        // 1 min after attempt 5
		{0, http.StatusTooManyRequests},               // immediately again
		{time.Minute, http.StatusTooManyRequests},     // now 2 min are needed
		{time.Minute, http.StatusUnauthorized},        // 2 min reached
		{3 * time.Minute, http.StatusTooManyRequests}, // 4 min needed
		{time.Minute, http.StatusUnauthorized},
	}
	for i, st := range steps {
		e.clock.Advance(st.advance)
		if r := localLogin(b, "admin", "wrong password!", "/"); r.code != st.want {
			t.Fatalf("step %d: %d, want %d", i, r.code, st.want)
		}
	}
	e.clock.Advance(8 * time.Minute)
	if r := localLogin(b, "admin", testPassword, "/"); r.code != http.StatusSeeOther {
		t.Fatalf("correct password after the delay: %d", r.code)
	}
}

// Why (product decision 2026-10-19): the IdP is the only method, so the browser
// goes straight there, only while no local sign-in is possible. With local
// sign-in on, /login must show the choice instead of deciding for the user.
func TestLoginRedirectsToTheIdPOnlyWhenItIsTheOnlyMethod(t *testing.T) {
	e := newEnv(t, ModeOIDC, func(o *Options) { o.LocalLogin = false })
	r := e.browser().get("/login")
	if r.code != http.StatusFound || !strings.Contains(r.loc, "/application/o/authorize/") {
		t.Fatalf("OIDC only: %d %q", r.code, r.loc)
	}
	e = newEnv(t, ModeOIDC)
	if r := e.browser().get("/login"); r.code != http.StatusOK {
		t.Fatalf("OIDC with local sign-in must not redirect: %d %q", r.code, r.loc)
	}
}

// Why: one sign-in page offers every configured method, so nobody has to know a
// second address. The button label is generic and configurable, because the
// product works with any OIDC provider.
func TestLoginPageOffersEveryConfiguredMethod(t *testing.T) {
	cases := []struct {
		name         string
		mode         string
		opt          func(*Options)
		button, form bool
		label        string
	}{
		{"oidc and local, default name", ModeOIDC, nil, true, true, "Sign in with Single sign-on"},
		{"oidc and local, custom name", ModeOIDC, func(o *Options) { o.OIDC.DisplayName = "Acme ID" }, true, true, "Sign in with Acme ID"},
		{"local only", ModeLocal, nil, false, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var opts []envOpt
			if tc.opt != nil {
				opts = append(opts, tc.opt)
			}
			e := newEnv(t, tc.mode, opts...)
			r := e.browser().get("/login?next=%2Fdevices")
			if r.code != http.StatusOK {
				t.Fatalf("/login: %d %q", r.code, r.loc)
			}
			if got := strings.Contains(r.body, "/login/sso?next="); got != tc.button {
				t.Errorf("SSO button shown = %v, want %v: %s", got, tc.button, r.body)
			}
			if tc.label != "" && !strings.Contains(r.body, tc.label) {
				t.Errorf("missing %q in %s", tc.label, r.body)
			}
			if got := strings.Contains(r.body, `action="/login/local"`); got != tc.form {
				t.Errorf("local form shown = %v, want %v", got, tc.form)
			}
			if tc.form && !strings.Contains(r.body, `value="/devices"`) {
				t.Errorf("next is lost on the form: %s", r.body)
			}
		})
	}
}

// Why: the local admin is the break-glass access, so signing in locally must
// work while OIDC is configured. GET /login/local is not a route any more (no redirects for
// moved routes before 1.0): the form posts there, the sign-in page is /login only.
func TestLocalSignInWorksWithOIDCConfigured(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	b := e.browser()
	if r := localLogin(b, "admin", testPassword, "/"); r.code != http.StatusSeeOther || r.loc != "/" {
		t.Fatalf("local login with OIDC configured: %d %q", r.code, r.loc)
	}
	if r := e.browser().get("/login/local?next=%2Fx"); r.code != http.StatusMethodNotAllowed || r.loc != "" {
		t.Fatalf("GET /login/local: %d %q, want 405 and no redirect", r.code, r.loc)
	}
	l := newEnv(t, ModeLocal)
	if r := l.browser().get("/login/sso"); r.code != http.StatusNotFound {
		t.Fatalf("/login/sso without an IdP: %d", r.code)
	}
}

func TestLocalLoginDisabled(t *testing.T) {
	e := newEnv(t, ModeOIDC, func(o *Options) { o.LocalLogin = false })
	b := e.browser()
	if r := b.get("/login/local"); r.code == http.StatusSeeOther {
		t.Fatalf("GET /login/local redirects (%q): the moved route is gone", r.loc)
	}
	if r := b.do(http.MethodPost, "/login/local", url.Values{"username": {"admin"}, "password": {testPassword}}, nil); r.code == http.StatusSeeOther && r.loc == "/" {
		t.Fatal("local login worked while disabled")
	}
}

// Only the account routes change credentials, and none is reachable
// without a session. The lists are the complete sets auth serves, so a new
// route cannot slip in unreviewed (CLAUDE.md safety rule 5).
func TestOnlyAccountRoutesChangeCredentials(t *testing.T) {
	want := []string{"GET /login", "GET /oidc/callback", "GET /login/sso", "POST /login/local", "POST /logout", "POST /oidc/stepup"}
	if !slices.Equal(routePatterns, want) {
		t.Fatalf("login routes changed: %v; review that none changes credentials, then update this test", routePatterns)
	}
	for _, p := range routePatterns {
		if regexp.MustCompile(`(?i)passw|user|admin|credential|account|reset`).MatchString(p) {
			t.Errorf("route %q looks like a credential route", p)
		}
	}
	// The setup routes are the one anonymous way to set a password: only with
	// the setup token, and only while the first admin still has its
	// first-start password (TestSetupTokenCreatesTheFirstAdmin); here no setup
	// is pending, so they redirect like the others.
	wantAcc := []string{"GET /account", "POST /account/password", "GET /users", "GET /users/new", "POST /users",
		"POST /users/stepup", "POST /users/local/{name}", "POST /users/sessions", "GET /users/{name}",
		"GET /users/principal/{subject...}", "POST /account/profile", "POST /account/name", "GET /setup/account", "POST /setup/account"}
	if !slices.Equal(accountRoutePatterns, wantAcc) {
		t.Fatalf("account routes changed: %v; review the role and step-up checks, then update this test", accountRoutePatterns)
	}
	e := newEnv(t, ModeOIDC)
	before := len(e.users.users)
	for _, p := range accountRoutePatterns {
		method, path, _ := strings.Cut(p, " ")
		path = strings.ReplaceAll(path, "{name}", "admin")
		w := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader("username=x&password=newpassword123&action=delete"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		e.svc.Routes().ServeHTTP(w, r)
		if w.Code != http.StatusSeeOther && w.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session: %d, want a login redirect or 401", p, w.Code)
		}
	}
	if len(e.users.users) != before {
		t.Error("an anonymous request changed the accounts")
	}
	// Other credential-looking paths reach no handler at all.
	h := e.svc.Routes()
	for _, p := range []string{"/password", "/admin/set-password", "/login/local/password", "/users", "/users/x/y"} {
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), m, p, strings.NewReader("username=admin&password=newpassword123")))
			if w.Code < 400 {
				t.Errorf("%s %s: %d", m, p, w.Code)
			}
		}
	}
}

// Why: the Secure flag and the __Host- name follow the scheme the browser
// used (own TLS listener, or X-Forwarded-Proto from a trusted proxy), never
// PUBLIC_URL: a Secure cookie on a plain-HTTP request is dropped by the
// browser and sign-in fails (review F10, break-glass over http).
func TestSessionCookieFlags(t *testing.T) {
	for _, c := range []struct {
		desc, target, base, name, path string
		hdr                            map[string]string
		secure                         bool
	}{
		{"TLS listener", "https://fleet.example/tf/login", "/tf", "__Host-tasmofleet", "/", nil, true},
		{"trusted proxy https", "/tf/login", "/tf", "__Host-tasmofleet", "/", map[string]string{"X-Forwarded-Proto": "https"}, true},
		{"plain HTTP", "/tf/login", "/tf", "tasmofleet", "/tf", nil, false},
	} {
		t.Run(c.desc, func(t *testing.T) {
			ss := &Sessions{Store: &fakeSessions{m: map[string]store.Session{}}, Clock: &fakeClock{now: time.Now()}}
			w := httptest.NewRecorder()
			var r *http.Request
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, c.target, nil)
			for k, v := range c.hdr {
				req.Header.Set(k, v)
			}
			httpx.Proxy{BasePath: c.base, PublicURL: "https://fleet.example", TrustedProxies: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}}.
				Middleware(http.HandlerFunc(func(_ http.ResponseWriter, rr *http.Request) { r = rr })).ServeHTTP(httptest.NewRecorder(), req)
			if _, err := ss.Create(w, r, model.Principal{Subject: "x"}, ""); err != nil {
				t.Fatal(err)
			}
			ck := w.Result().Cookies()[0]
			if ck.Name != c.name || ck.Secure != c.secure || ck.Path != c.path || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode {
				t.Fatalf("cookie %+v", ck)
			}
			if len(ck.Value) < 43 {
				t.Fatalf("cookie value too short for 32 random bytes: %d", len(ck.Value))
			}
		})
	}
}

func TestSessionRotationAndExpiry(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	st := &fakeSessions{m: map[string]store.Session{}}
	ss := &Sessions{Store: st, Clock: clk, Idle: 12 * time.Hour, Max: 168 * time.Hour}
	login := func(old string) string {
		w := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		if old != "" {
			r.AddCookie(&http.Cookie{Name: CookieInsecure, Value: old})
		}
		if _, err := ss.Create(w, r, model.Principal{Subject: "x"}, ""); err != nil {
			t.Fatal(err)
		}
		return w.Result().Cookies()[0].Value
	}
	get := func(v string) error {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: CookieInsecure, Value: v})
		_, err := ss.Get(r)
		return err
	}
	// Fixation: a pre-set cookie value is replaced and invalidated at login.
	first := login("")
	second := login(first)
	if first == second || get(first) == nil || get(second) != nil {
		t.Fatal("session ID not rotated at login")
	}
	for _, s := range st.all() {
		if s.IDHash == second {
			t.Fatal("store holds the raw cookie value")
		}
	}
	// Idle expiry: active use keeps it alive, 12 h of silence ends it.
	clk.Advance(11 * time.Hour)
	if get(second) != nil {
		t.Fatal("expired before idle timeout")
	}
	clk.Advance(12 * time.Hour)
	if !errors.Is(get(second), ErrUnauthenticated) || len(st.all()) != 0 {
		t.Fatal("idle session still valid or not deleted")
	}
	// Absolute expiry: even a constantly used session ends after 7 days.
	s := login("")
	for range 16 { // 176 h > 168 h
		clk.Advance(11 * time.Hour)
		if err := get(s); err != nil && clk.now.Sub(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) < 7*24*time.Hour {
			t.Fatalf("expired early: %v", err)
		}
	}
	if get(s) == nil {
		t.Fatal("session outlived the absolute maximum")
	}
}

// Why: a header-mode user whose groups map to no role holds a session but gets the
// standalone 403 page (no app shell); it must carry a CSRF-backed Sign out form.
func TestNoRole403OffersSignOutWithoutAppShell(t *testing.T) {
	e := newEnv(t, ModeHeader)
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/viewer", nil)
	r.RemoteAddr = "127.0.0.1:4000"
	r.Header.Set("X-authentik-username", "dave")
	r.Header.Set("X-authentik-groups", "family")
	w := httptest.NewRecorder()
	e.handler().ServeHTTP(w, r)
	body := w.Body.String()
	if w.Code != http.StatusForbidden || !strings.Contains(body, `action="/logout"`) || !strings.Contains(body, `name="csrf" value="`) || strings.Contains(body, `name="csrf" value=""`) {
		t.Fatalf("no-role 403 lacks a Sign out form: %d %s", w.Code, body)
	}
}
