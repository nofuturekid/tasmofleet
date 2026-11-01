package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/nofuturekid/tasmofleet/internal/audit"
	"github.com/nofuturekid/tasmofleet/internal/clock"
	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// Modes.
const (
	ModeOIDC   = "oidc"
	ModeHeader = "header"
	ModeLocal  = "local" // local accounts are the only login method (standalone)
)

// OIDCOptions configure AUTH_MODE=oidc.
type OIDCOptions struct {
	Issuer        string
	ClientID      string
	ClientSecret  string
	Scopes        []string // default openid, profile, email
	GroupsClaim   string   // default "groups"
	UsernameClaim string   // default "preferred_username"
	NameClaim     string   // the full name, shown instead of the user name; default "name"
	DisplayName   string   // login button: "Sign in with <DisplayName>"; default "Single sign-on"
	PostLogoutURL string   // default PUBLIC_URL + BASE_PATH + "/"
	// HTTPClient talks to the IdP (discovery, token, JWKS). Default: 10 s timeout.
	HTTPClient *http.Client
}

// HeaderOptions configure AUTH_MODE=header.
type HeaderOptions struct {
	User      string // default X-authentik-username
	Name      string // default X-authentik-name
	Email     string // default X-authentik-email
	Groups    string // default X-authentik-groups
	GroupsSep string // default "|"
	LogoutURL string // outpost sign-out URL
}

// Options configure a Service.
type Options struct {
	Mode       string // ModeOIDC, ModeHeader or ModeLocal
	Proxy      httpx.Proxy
	OIDC       OIDCOptions
	Header     HeaderOptions
	Roles      RoleMapping
	LocalLogin bool
	Sessions   store.SessionStore
	LocalUsers store.LocalUserStore
	// DeviceCookies records issued device cookies (per-cookie revocation).
	// Default: LocalUsers, if it also implements the interface (*store.SQLite does).
	DeviceCookies store.DeviceCookieStore
	Audit         audit.Recorder
	// Directory (optional) lists the SSO and proxy users seen at sign-in for
	// the Users page and records each sign-in. Without it the page lists the
	// local accounts only.
	Directory store.UserDirectory
	Clock     clock.Clock   // default clock.Real
	Idle, Max time.Duration // session lifetimes; default 2 h idle, 12 h absolute
	// Recheck is how often an OIDC session is re-evaluated at the IdP via its
	// refresh token (default 15 min).
	Recheck time.Duration
	// AuditLog (optional) lists the last audit entries of a user on the user
	// detail page.
	AuditLog AuditLister
	// DataDir holds the generated initial admin password file (see EnsureInitialAdmin).
	DataDir string
	Pages   Pages    // default: minimal built-in pages
	Limiter *Limiter // default: spec limits
	// SecretKey (>= 32 bytes, persisted by the caller: LoadOrCreateKey,
	// <DATA_DIR>/csrf.key, separate from secret.key) signs device cookies and header-mode CSRF tokens. Without
	// it a random per-process key is used: both then end at restart.
	SecretKey []byte
}

// Service implements Authenticator for both modes plus the break-glass
// local login, and provides the role-check middleware and the CSRF tokens
// (httpx.TokenSource).
type Service struct {
	o             Options
	sessions      *Sessions
	csrfKey       []byte
	deviceKey     []byte
	sessionKey    []byte
	kdf           chan struct{} // see verifyBounded
	accounts      *Accounts
	steps         *stepUps
	oidcSteps     *stepUps // re-authentications at the IdP; never accepted where a local password is meant
	noRefreshOnce sync.Once
	recheckMu     sync.Mutex // serialises OIDC re-checks, see recheck
	seenMu        sync.Mutex
	seen          map[string]time.Time // header subject -> last "last login" write
	setupMu       sync.Mutex
	setupHash     []byte // SHA-256 of the setup token (setuptoken.go); nil: none

	mu         sync.RWMutex
	oauth      *oauth2.Config
	verifier   *oidc.IDTokenVerifier
	endSession string
}

var (
	_ Authenticator     = (*Service)(nil)
	_ httpx.TokenSource = (*Service)(nil)
)

// New validates opts and returns a Service. In OIDC mode it is not ready
// until Run (or Discover) has fetched the discovery document.
func New(o Options) (*Service, error) {
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.Pages == nil {
		o.Pages = DefaultPages{}
	}
	if o.Limiter == nil {
		o.Limiter = &Limiter{}
	}
	if o.Mode == ModeLocal {
		o.LocalLogin = true // the only login method
	}
	if o.Sessions == nil || o.Audit == nil || (o.LocalLogin && o.LocalUsers == nil) {
		return nil, errors.New("auth: Sessions, Audit and (with LocalLogin) LocalUsers are required")
	}
	if o.LocalLogin && o.DeviceCookies == nil {
		dc, ok := o.LocalUsers.(store.DeviceCookieStore)
		if !ok {
			return nil, errors.New("auth: local login needs a DeviceCookieStore")
		}
		o.DeviceCookies = dc
	}
	if o.Idle <= 0 {
		o.Idle = 2 * time.Hour
	}
	if o.Max <= 0 {
		o.Max = 12 * time.Hour
	}
	if o.Recheck <= 0 {
		o.Recheck = 15 * time.Minute
	}
	switch o.Mode {
	case ModeLocal:
		// No PUBLIC_URL, proxy or IdP needed. Nothing in this mode builds an
		// absolute redirect from the request.
	case ModeOIDC:
		if o.Proxy.PublicURL == "" || o.OIDC.Issuer == "" || o.OIDC.ClientID == "" {
			return nil, errors.New("auth: oidc mode needs PUBLIC_URL, OIDC_ISSUER and OIDC_CLIENT_ID")
		}
		def(&o.OIDC.GroupsClaim, "groups")
		def(&o.OIDC.UsernameClaim, "preferred_username")
		def(&o.OIDC.NameClaim, "name")
		def(&o.OIDC.DisplayName, "Single sign-on")
		def(&o.OIDC.PostLogoutURL, o.Proxy.URL("/"))
		if len(o.OIDC.Scopes) == 0 {
			o.OIDC.Scopes = []string{oidc.ScopeOpenID, "profile", "email"}
		}
		// The refresh token drives the group re-check (spec §15.1).
		if !slices.Contains(o.OIDC.Scopes, "offline_access") {
			o.OIDC.Scopes = append(slices.Clone(o.OIDC.Scopes), "offline_access")
		}
		if o.OIDC.HTTPClient == nil {
			o.OIDC.HTTPClient = &http.Client{Timeout: 10 * time.Second}
		}
	case ModeHeader:
		if len(o.Proxy.TrustedProxies) == 0 {
			return nil, errors.New("auth: header mode needs TRUSTED_PROXIES")
		}
		def(&o.Header.User, "X-authentik-username")
		def(&o.Header.Name, "X-authentik-name")
		def(&o.Header.Email, "X-authentik-email")
		def(&o.Header.Groups, "X-authentik-groups")
		def(&o.Header.GroupsSep, "|")
	default:
		return nil, fmt.Errorf("auth: unknown mode %q", o.Mode)
	}
	key := o.SecretKey
	switch {
	case len(key) == 0:
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		slog.Warn("auth: no persisted SecretKey; device cookies and header-mode CSRF tokens end at restart")
	case len(key) < 32:
		return nil, errors.New("auth: SecretKey must have at least 32 bytes")
	}
	return &Service{
		o:          o,
		csrfKey:    subKey(key, labelCSRF),
		deviceKey:  subKey(key, labelDevice),
		sessionKey: subKey(key, labelSession),
		kdf:        sharedKDF,
		sessions:   &Sessions{Store: o.Sessions, Clock: o.Clock, Idle: o.Idle, Max: o.Max, NoRefreshLife: o.Recheck, Cookies: o.DeviceCookies},
		accounts:   &Accounts{Users: o.LocalUsers, Sessions: o.Sessions, Cookies: o.DeviceCookies, Clock: o.Clock},
		steps:      newStepUps(),
		oidcSteps:  newStepUps(),
		seen:       map[string]time.Time{},
	}, nil
}

// Purpose labels: each signing key is derived from the persisted key under
// its own label, so a value signed for one purpose is never valid for another.
const (
	labelCSRF    = "tasmofleet csrf v1"
	labelDevice  = "tasmofleet device-cookie v1"
	labelSession = "tasmofleet session v1"
)

// subKey derives an independent key per purpose.
func subKey(key []byte, label string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(label))
	return m.Sum(nil)
}

func def(s *string, v string) {
	if *s == "" {
		*s = v
	}
}

// Sessions returns the session manager (for the hourly cleanup).
func (s *Service) Sessions() *Sessions { return s.sessions }

// RedirectURL is PUBLIC_URL + BASE_PATH + "/oidc/callback", never derived
// from request headers.
func (s *Service) RedirectURL() string { return s.o.Proxy.URL("/oidc/callback") }

// Ready reports whether logins can be served (OIDC discovery done).
func (s *Service) Ready() bool {
	if s.o.Mode != ModeOIDC {
		return true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.oauth != nil
}

// Discover fetches the OIDC discovery document once.
func (s *Service) Discover(ctx context.Context) error {
	if s.o.Mode != ModeOIDC {
		return nil
	}
	ctx = oidc.ClientContext(ctx, s.o.OIDC.HTTPClient)
	p, err := oidc.NewProvider(ctx, s.o.OIDC.Issuer)
	if err != nil {
		return fmt.Errorf("auth: oidc discovery: %w", err)
	}
	var meta struct {
		EndSession string `json:"end_session_endpoint"`
	}
	if err := p.Claims(&meta); err != nil {
		return fmt.Errorf("auth: oidc discovery: %w", err)
	}
	cfg := &oauth2.Config{
		ClientID: s.o.OIDC.ClientID, ClientSecret: s.o.OIDC.ClientSecret,
		Endpoint: p.Endpoint(), RedirectURL: s.RedirectURL(), Scopes: s.o.OIDC.Scopes,
	}
	// The key set fetches JWKS lazily with this background context (and
	// client), so key rotation keeps working after ctx ends.
	keyCtx := oidc.ClientContext(context.Background(), s.o.OIDC.HTTPClient)
	v := p.VerifierContext(keyCtx, &oidc.Config{ClientID: s.o.OIDC.ClientID, Now: s.o.Clock.Now})
	s.mu.Lock()
	s.oauth, s.verifier, s.endSession = cfg, v, meta.EndSession
	s.mu.Unlock()
	return nil
}

// Run performs OIDC discovery with backoff (1 s doubling to 1 min), then
// cleans expired sessions hourly until ctx ends.
func (s *Service) Run(ctx context.Context) {
	backoff := time.Second
	for {
		err := s.Discover(ctx)
		if err == nil {
			break
		}
		slog.WarnContext(ctx, "oidc discovery failed, retrying", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-s.o.Clock.After(backoff):
		}
		backoff = min(2*backoff, time.Minute)
	}
	s.sessions.RunCleanup(ctx, time.Hour)
}

type ctxInfo struct {
	p           model.Principal
	err         error
	csrf        string
	idToken     string
	sessionID   string // store ID hash; empty for header principals
	passwordGen int64  // local sessions: the generation the session is bound to
	mustChange  bool   // local user has to change the password first
	reserved    string // err is ErrReservedSubject: the refused header user
}

// ErrReservedSubject is the refusal of an SSO or proxy identity whose subject starts with "local:",
// the namespace of local accounts (#380).
var ErrReservedSubject = fmt.Errorf("%w: the subject is reserved for local accounts", ErrUnauthenticated)

// reservedSubject reports whether subject is in the namespace of local accounts ("local:<name>"). The
// audit log, the Users page and the principals table tell local accounts by that prefix alone, so an
// identity provider or proxy must not be able to present one.
func reservedSubject(subject string) bool { return strings.HasPrefix(subject, localSubjectPrefix) }

// localSubjectPrefix starts the subject of every local account.
const localSubjectPrefix = "local:"

// reservedMessage is the page a reserved subject gets instead of a session.
func reservedMessage(base string) MessageData {
	return MessageData{
		BasePath: base, Title: "Sign-in refused",
		Text: "The identity your sign-in provider sent starts with \"local:\", which is reserved for local accounts. Ask the administrator of the identity provider for a different username.",
	}
}

// firstRefusal says whether a refused header user is worth an audit row now: at most one per
// headerSeenEvery and subject, because a header user is refused on every request.
func (s *Service) firstRefusal(subject string) bool {
	key, now := "refused:"+subject, s.o.Clock.Now()
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	if last, ok := s.seen[key]; ok && now.Sub(last) < headerSeenEvery {
		return false
	}
	if len(s.seen) >= 1024 {
		clear(s.seen)
	}
	s.seen[key] = now
	return true
}

type infoKey struct{}

// resolve finds the principal of r: trusted headers (header mode, trusted
// peer only), then the session cookie.
func (s *Service) resolve(r *http.Request) ctxInfo {
	if ci, ok := r.Context().Value(infoKey{}).(ctxInfo); ok {
		return ci
	}
	if s.o.Mode == ModeHeader && s.o.Proxy.Trusted(r) {
		if user := strings.TrimSpace(r.Header.Get(s.o.Header.User)); user != "" {
			if reservedSubject(user) {
				return ctxInfo{err: ErrReservedSubject, reserved: user}
			}
			p := model.Principal{
				Subject: user, Name: r.Header.Get(s.o.Header.Name), Email: r.Header.Get(s.o.Header.Email),
				Groups: splitGroups(r.Header.Get(s.o.Header.Groups), s.o.Header.GroupsSep), Method: ModeHeader,
			}
			if p.Name = s.cleanIdPName(p.Name, "header name"); p.Name == "" {
				p.Name = user
			}
			p.Role = s.o.Roles.Role(p.Groups)
			return ctxInfo{p: p, csrf: s.headerToken(user)}
		}
	}
	sess, err := s.sessions.Get(r)
	if err != nil {
		return ctxInfo{err: err}
	}
	p, mustChange, err := s.revalidate(r.Context(), sess)
	if err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			if derr := s.o.Sessions.DeleteSession(r.Context(), sess.IDHash); derr != nil && !errors.Is(derr, store.ErrNotFound) {
				slog.ErrorContext(r.Context(), "revoke session", "err", derr)
			}
		}
		return ctxInfo{err: err}
	}
	return ctxInfo{p: p, csrf: sess.CSRFToken, idToken: sess.IDToken, sessionID: sess.IDHash, passwordGen: sess.PasswordGen, mustChange: mustChange}
}

// revalidate re-checks a stored session against the current configuration
// and credentials on every request, so revocations take effect at once:
//   - local sessions end when LOCAL_LOGIN is off, or the account was deleted,
//     disabled or had its password changed after the session started. The role
//     is the account's current role, so a role change applies immediately;
//   - OIDC sessions are only valid in oidc mode. Their role is recomputed
//     from the stored groups with the current mapping, and every Recheck
//     interval the groups are re-read at the IdP (see recheck);
//   - any other session method is refused.
func (s *Service) revalidate(ctx context.Context, sess store.Session) (p model.Principal, mustChange bool, err error) {
	p = sess.Principal
	switch p.Method {
	case "local":
		if !s.o.LocalLogin || s.o.LocalUsers == nil {
			return p, false, ErrUnauthenticated
		}
		u, err := s.o.LocalUsers.GetLocalUser(ctx, p.Name)
		if errors.Is(err, store.ErrNotFound) {
			return p, false, ErrUnauthenticated
		}
		if err != nil {
			return p, false, fmt.Errorf("auth: check local user: %w", err)
		}
		// PasswordGen: the session is valid only for the password generation its
		// login verified, so a login that raced a password change or disable
		// (verified before, stored after) is dead on arrival. UpdatedAt covers
		// out-of-band changes that did not go through the store's generation.
		if u.Disabled || u.PasswordGen != sess.PasswordGen || u.UpdatedAt.After(sess.CreatedAt) ||
			(u.Role != model.RoleOperator && u.Role != model.RoleAdmin) {
			return p, false, ErrUnauthenticated
		}
		p.Role = u.Role
		return p, u.MustChangePassword, nil
	case ModeOIDC:
		if s.o.Mode != ModeOIDC {
			return p, false, ErrUnauthenticated
		}
		if sess.RefreshToken != "" && s.o.Clock.Now().Sub(sess.RecheckedAt) >= s.o.Recheck {
			sess, err = s.recheck(ctx, sess)
			if err != nil {
				return p, false, err
			}
			p = sess.Principal
		}
		p.Role = s.o.Roles.Role(p.Groups)
		if p.Role == model.RoleNone {
			return p, false, ErrUnauthenticated
		}
		return p, false, nil
	}
	return p, false, ErrUnauthenticated
}

func splitGroups(v, sep string) []string {
	var out []string
	for g := range strings.SplitSeq(v, sep) {
		if g = strings.TrimSpace(g); g != "" {
			out = append(out, g)
		}
	}
	return out
}

// headerToken derives a CSRF token for header-mode users, who have no
// session. It is bound to the user and changes on every restart.
func (s *Service) headerToken(user string) string {
	m := hmac.New(sha256.New, s.csrfKey)
	m.Write([]byte(user))
	return hex.EncodeToString(m.Sum(nil))
}

// Principal returns the authenticated principal or ErrUnauthenticated.
func (s *Service) Principal(r *http.Request) (model.Principal, error) {
	ci := s.resolve(r)
	return ci.p, ci.err
}

// CSRFToken returns the token expected on unsafe requests: the session's
// (or header user's) token, else the pre-session login cookie (double
// submit for /login/local). Empty means no request can pass.
func (s *Service) CSRFToken(r *http.Request) string {
	if ci := s.resolve(r); ci.err == nil {
		return ci.csrf
	}
	if c, err := r.Cookie(s.preCookie(r)); err == nil {
		return c.Value
	}
	return ""
}

// Middleware resolves the principal once and stores it in the context
// (FromContext). It does not enforce anything; use Require.
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ci := s.resolve(r)
		if ci.err != nil && !errors.Is(ci.err, ErrUnauthenticated) {
			slog.ErrorContext(r.Context(), "resolve principal", "err", ci.err)
			ci.err = ErrUnauthenticated
		}
		ctx := context.WithValue(r.Context(), infoKey{}, ci)
		if ci.err == nil {
			ctx = WithPrincipal(ctx, ci.p)
			if ci.p.Method == ModeHeader {
				s.noteHeaderSeen(r.Context(), ci.p)
			}
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Require returns middleware that admits principals with at least role
// min. Unauthenticated browsers are sent to /login (HTMX gets HX-Redirect),
// authenticated principals below min get 403.
func (s *Service) Require(min model.Role) func(http.Handler) http.Handler {
	return s.require(min, false)
}

// require is Require with the forced-password-change gate: a local user who
// must change the password reaches nothing but /account (which
// passes allowMustChange) until that is done.
func (s *Service) require(min model.Role, allowMustChange bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ci := s.resolve(r)
			base := httpx.BasePath(r.Context())
			if errors.Is(ci.err, ErrReservedSubject) {
				if s.firstRefusal(ci.reserved) {
					s.record(r, ci.reserved, "auth.login.header", "denied: reserved subject", nil)
				}
				s.o.Pages.Message(w, r, http.StatusForbidden, reservedMessage(base))
				return
			}
			if ci.err != nil {
				login := base + "/login?next=" + url.QueryEscape(httpx.LocalPath(r.URL.RequestURI()))
				if errors.Is(ci.err, ErrSessionExpired) {
					login += "&expired=1"
				}
				switch {
				case r.Header.Get("HX-Request") != "":
					w.Header().Set("HX-Redirect", login)
					w.WriteHeader(http.StatusUnauthorized)
				case r.Method == http.MethodGet:
					http.Redirect(w, r, login, http.StatusSeeOther)
				default:
					http.Error(w, "not authenticated", http.StatusUnauthorized)
				}
				return
			}
			if ci.mustChange && !allowMustChange {
				if r.Method == http.MethodGet && r.Header.Get("HX-Request") == "" {
					http.Redirect(w, r, base+"/account", http.StatusSeeOther)
					return
				}
				s.o.Pages.Message(w, r, http.StatusForbidden, MessageData{
					BasePath: base, Title: "Password change required",
					Text: "Choose a new password on the Account page (/account) before using TasmoFleet.",
				})
				return
			}
			if ci.p.Role == model.RoleNone {
				// No role at all: the app shell would only offer links that lead back here, so this
				// stays a standalone page. It carries Sign out, else the user cannot switch accounts.
				s.o.Pages.Message(w, r, http.StatusForbidden, MessageData{
					BasePath: base, Title: "Access denied", SignOut: true, CSRFToken: ci.csrf,
					Text: "You are signed in, but none of your groups grants access to TasmoFleet. Ask an administrator to add you to a mapped group.",
				})
				return
			}
			if ci.p.Role < min {
				// The principal rides along so the page can join the app shell (navigation, way back).
				s.o.Pages.Message(w, r.WithContext(WithPrincipal(r.Context(), ci.p)), http.StatusForbidden, deniedMessage(base, ci.p, min))
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), ci.p)))
		})
	}
}

// deniedMessage explains a page above the user's role: who is signed in, which role the page
// needs, and what the user's role does allow (design v2 screen e403).
func deniedMessage(base string, p model.Principal, min model.Role) MessageData {
	who := p.Name
	if who == "" {
		who = p.Subject
	}
	text := "This page needs the " + roleName(min) + " role. You are signed in as " + who + " with the " + roleName(p.Role) + " role."
	if min == model.RoleAdmin && p.Role == model.RoleOperator {
		text += " Operators can switch, restart and back up devices, but cannot manage firmware, policy, users or settings."
	}
	return MessageData{BasePath: base, Title: "Only " + roleName(min) + "s can open this page", Text: text}
}

// routePatterns are the routes of the login flows. The routes that change
// credentials are listed separately in accountRoutePatterns.
var routePatterns = []string{
	"GET /login",
	"GET /oidc/callback",
	"GET /login/sso",
	"POST /login/local",
	"POST /logout",
	"POST /oidc/stepup",
}

// Routes returns the handlers for /login, /login/sso, /oidc/callback, /login/local,
// /logout, /account/password and /users, relative to the base path.
// Mount them (see MountPaths) behind httpx.Proxy, s.Middleware and the
// httpx.CSRF middleware.
func (s *Service) Routes() http.Handler {
	h := map[string]http.Handler{
		"GET /login":         http.HandlerFunc(s.login),
		"GET /oidc/callback": http.HandlerFunc(s.callback),
		"GET /login/sso":     http.HandlerFunc(s.sso),
		"POST /login/local":  http.HandlerFunc(s.localSubmit),
		"POST /logout":       http.HandlerFunc(s.logout),
		"POST /oidc/stepup":  http.HandlerFunc(s.oidcStepUpStart),
	}
	for p, handler := range s.accountHandlers() {
		h[p] = handler
	}
	mux := http.NewServeMux()
	for p, handler := range h {
		mux.Handle(p, handler)
	}
	return mux
}

func (s *Service) record(r *http.Request, actor, action, result string, detail map[string]string) {
	d := ""
	if len(detail) > 0 {
		b, err := json.Marshal(detail)
		if err == nil {
			d = string(b)
		}
	}
	s.o.Audit.Record(r.Context(), model.AuditEvent{
		At: s.o.Clock.Now(), Actor: actor, Action: action, Result: result, Detail: d,
		RemoteAddr: clientIP(r).String(),
	})
}

func clientIP(r *http.Request) netip.Addr {
	if a := httpx.ClientIP(r.Context()); a.IsValid() {
		return a
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	a, _ := netip.ParseAddr(host) // invalid stays the zero Addr
	return a.Unmap()
}

func (s *Service) redirectLocal(w http.ResponseWriter, r *http.Request, next string) {
	http.Redirect(w, r, httpx.BasePath(r.Context())+httpx.LocalPath(next), http.StatusSeeOther) //nolint:gosec // G710: validated base path + LocalPath (no scheme, host or "//")
}

func (s *Service) login(w http.ResponseWriter, r *http.Request) {
	next := httpx.LocalPath(r.URL.Query().Get("next"))
	if s.o.Mode == ModeLocal {
		s.loginPage(w, r, next)
		return
	}
	if s.o.Mode == ModeHeader {
		if ci := s.resolve(r); ci.err == nil {
			s.redirectLocal(w, r, next)
			return
		}
		if s.localEnabled(r.Context()) { // the proxy did not identify the visitor: local accounts still can sign in
			s.loginPage(w, r, next)
			return
		}
		s.o.Pages.Message(w, r, http.StatusUnauthorized, MessageData{
			BasePath: httpx.BasePath(r.Context()), Title: "Not signed in",
			Text: "The authenticating proxy did not identify you.",
		})
		return
	}
	// One page for both methods; the IdP is the only method (and the browser
	// goes straight there) only when no local sign-in is possible.
	if s.localEnabled(r.Context()) {
		s.loginPage(w, r, next)
		return
	}
	s.startOIDC(w, r, next)
}

// sso starts the OIDC flow from the button on the sign-in page.
func (s *Service) sso(w http.ResponseWriter, r *http.Request) {
	if s.o.Mode != ModeOIDC {
		http.NotFound(w, r)
		return
	}
	s.startOIDC(w, r, httpx.LocalPath(r.URL.Query().Get("next")))
}

func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	ci := s.resolve(r)
	base := httpx.BasePath(r.Context())
	if err := s.sessions.Destroy(w, r); err != nil {
		slog.ErrorContext(r.Context(), "logout", "err", err)
	}
	if ci.err == nil {
		s.record(r, ci.p.Subject, "auth.logout", "ok", nil)
	}
	switch {
	case ci.err == nil && ci.p.Method == ModeHeader && s.o.Header.LogoutURL != "":
		http.Redirect(w, r, s.o.Header.LogoutURL, http.StatusSeeOther)
	case ci.idToken != "":
		s.mu.RLock()
		end := s.endSession
		s.mu.RUnlock()
		if end == "" {
			http.Redirect(w, r, base+"/", http.StatusSeeOther)
			return
		}
		u, err := url.Parse(end)
		if err != nil {
			http.Redirect(w, r, base+"/", http.StatusSeeOther)
			return
		}
		q := u.Query()
		q.Set("id_token_hint", ci.idToken)
		q.Set("post_logout_redirect_uri", s.o.OIDC.PostLogoutURL)
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.String(), http.StatusSeeOther) //nolint:gosec // G710: end_session_endpoint comes from the configured issuer's discovery, not the request
	case ci.err == nil && ci.p.Method == "local":
		http.Redirect(w, r, base+"/login?signedout=1", http.StatusSeeOther)
	default:
		http.Redirect(w, r, base+"/", http.StatusSeeOther)
	}
}
