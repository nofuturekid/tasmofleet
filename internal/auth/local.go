package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// LocalUserLister is implemented by stores that can tell whether any
// break-glass account exists; without it the form is shown whenever
// LOCAL_LOGIN is on.
type LocalUserLister interface {
	HasLocalUsers(ctx context.Context) (bool, error)
}

func (s *Service) preCookie(r *http.Request) string {
	if s.sessions.secure(r) {
		return CookieSecure + "-csrf"
	}
	return CookieInsecure + "-csrf"
}

func (s *Service) localEnabled(ctx context.Context) bool {
	if !s.o.LocalLogin {
		return false
	}
	if l, ok := s.o.LocalUsers.(LocalUserLister); ok {
		has, err := l.HasLocalUsers(ctx)
		if err != nil {
			slog.ErrorContext(ctx, "check local users", "err", err)
			return false
		}
		return has
	}
	return true
}

// formToken returns the token for the login form: the existing one, or a
// new pre-session double-submit cookie.
func (s *Service) formToken(w http.ResponseWriter, r *http.Request) string {
	if t := s.CSRFToken(r); t != "" {
		return t
	}
	t := random32()
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure follows the request scheme (spec §15.2)
		Name: s.preCookie(r), Value: t, Path: s.sessions.cookiePath(r), MaxAge: int(flowTTL.Seconds()),
		HttpOnly: true, Secure: s.sessions.secure(r), SameSite: http.SameSiteStrictMode,
	})
	return t
}

// loginData is the content of the sign-in page for the request: the local
// form when local sign-in is possible, the single sign-on button when an IdP
// is configured (AUTH_MODE=oidc).
func (s *Service) loginData(r *http.Request, next string) LoginData {
	d := LoginData{BasePath: httpx.BasePath(r.Context()), Next: next, Local: s.localEnabled(r.Context())}
	if s.o.Mode == ModeOIDC {
		d.SSO = s.o.OIDC.DisplayName
	}
	return d
}

// loginPage renders the sign-in page (GET /login) with a fresh form token.
func (s *Service) loginPage(w http.ResponseWriter, r *http.Request, next string) {
	d := s.loginData(r, next)
	switch {
	case r.URL.Query().Get("signedout") != "":
		d.Notice = "You have been signed out."
	case r.URL.Query().Get("expired") != "":
		d.Expired, d.Notice = true, "Your session has expired. Sign in again."
	case next != "" && next != "/":
		d.Notice = "Sign in to continue."
	}
	if d.Local {
		d.CSRFToken = s.formToken(w, r)
	}
	s.o.Pages.Login(w, r, http.StatusOK, d)
}

// localSubmit checks a break-glass login. The CSRF middleware has already
// verified origin and token. It never changes credentials.
func (s *Service) localSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d := s.loginData(r, httpx.LocalPath(r.PostFormValue("next")))
	if !d.Local {
		s.o.Pages.Login(w, r, http.StatusForbidden, d)
		return
	}
	user := NormalizeUsername(r.PostFormValue("username"))
	d.Username = user
	pass := r.PostFormValue("password")
	// The limiter runs before the account lookup and the KDF, for known and
	// unknown usernames alike. The client network comes from the
	// trusted-proxy-aware client address only.
	if len(user) > maxUsernameLen {
		user = user[:maxUsernameLen] // bounds work and log size; such a user cannot exist
	}
	ip := clientIP(r)
	src, limitUser := limitKey(ip), strings.ToLower(user)
	recognised := s.validDeviceCookie(r, user)
	d.Remember = r.PostFormValue("remember") != ""
	// attemptsLeft fills the "attempts left before lock" count of a failed sign-in.
	attemptsLeft := func() {
		d.ShowAttemptsLeft, d.AttemptsLeft = true, s.o.Limiter.Remaining(s.o.Clock.Now(), src, limitUser, recognised)
	}
	if ok, _ := s.o.Limiter.Allow(s.o.Clock.Now(), src, limitUser, recognised); !ok {
		s.record(r, "local:"+user, "auth.login.local", "denied: rate limited", nil)
		attemptsLeft()
		d.CSRFToken, d.Error = s.formToken(w, r), "Too many attempts. Wait a minute and try again."
		s.o.Pages.Login(w, r, http.StatusTooManyRequests, d)
		return
	}
	hash := dummyHash
	u, err := s.o.LocalUsers.GetLocalUser(ctx, user)
	switch {
	case err == nil:
		hash = u.PasswordHash
	case !errors.Is(err, store.ErrNotFound):
		slog.ErrorContext(ctx, "load local user", "err", err)
	}
	// Always run the KDF so the response time does not reveal whether the user exists.
	verr := s.verifyBounded(ctx, hash, pass)
	if errors.Is(verr, context.Canceled) || errors.Is(verr, context.DeadlineExceeded) {
		return // client went away while waiting for a KDF slot
	}
	if err == nil && verr == nil && (u.Disabled || (u.Role != model.RoleOperator && u.Role != model.RoleAdmin)) {
		// Right password, but the account may not sign in. The visitor sees the
		// same answer as for a wrong password; only the audit log tells.
		s.record(r, "local:"+user, "auth.login.local", "denied: account disabled", nil)
		attemptsLeft()
		d.CSRFToken, d.Error = s.formToken(w, r), "Wrong username or password."
		s.o.Pages.Login(w, r, http.StatusUnauthorized, d)
		return
	}
	if err != nil || verr != nil || user == "" {
		if errors.Is(verr, ErrInvalidHash) && err == nil {
			slog.ErrorContext(ctx, "local user has an invalid password hash", "user", user)
		}
		s.record(r, "local:"+user, "auth.login.local", "denied: bad credentials", nil)
		attemptsLeft()
		d.CSRFToken, d.Error = s.formToken(w, r), "Wrong username or password."
		s.o.Pages.Login(w, r, http.StatusUnauthorized, d)
		return
	}
	if OTPExpired(u, s.o.Clock.Now()) {
		// The right one-time password, but too late. Only someone who knows it
		// gets this answer, so it may say what happened.
		s.record(r, "local:"+user, "auth.login.local", "denied: one-time password expired", nil)
		d.CSRFToken, d.Error = s.formToken(w, r), "This one-time password has expired. Ask an administrator for a new one."
		s.o.Pages.Login(w, r, http.StatusUnauthorized, d)
		return
	}
	s.o.Limiter.Success(src, limitUser)
	// u.PasswordGen was read before the KDF ran: session and device cookie
	// are bound to the password that was actually verified.
	if !s.validDeviceCookie(r, u.Username) { // a trusted browser keeps its cookie, so the table does not grow per login
		s.setDeviceCookie(w, r, u.Username, u.PasswordGen)
	}
	p := model.Principal{Subject: SubjectOf(u.Username), Name: u.Username, Role: u.Role, Method: "local"}
	if _, err := s.sessions.CreateLocal(w, r, p, u.PasswordGen, d.Remember); err != nil {
		if errors.Is(err, store.ErrStaleActor) {
			// The password was changed or the account disabled between the
			// credential check and the session insert: the store refused the
			// session. Same answer as for wrong credentials.
			s.record(r, "local:"+user, "auth.login.local", "denied: account changed during login", nil)
			d.CSRFToken, d.Error = s.formToken(w, r), "Wrong username or password."
			s.o.Pages.Login(w, r, http.StatusUnauthorized, d)
			return
		}
		slog.ErrorContext(ctx, "create session", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure follows the request scheme (spec §15.2)
		Name: s.preCookie(r), Path: s.sessions.cookiePath(r), MaxAge: -1,
		HttpOnly: true, Secure: s.sessions.secure(r), SameSite: http.SameSiteStrictMode,
	})
	s.noteLogin(ctx, p)
	s.record(r, p.Subject, "auth.login.local", "ok", nil)
	if u.MustChangePassword {
		s.redirectLocal(w, r, "/account")
		return
	}
	s.redirectLocal(w, r, d.Next)
}

// limitKey is the rate-limit identity of a client: the IPv4 address, or the
// IPv6 /64 (one subscriber typically owns a whole /64 and could rotate
// addresses inside it).
func limitKey(a netip.Addr) string {
	if a.Is6() && !a.Is4In6() {
		p, err := a.Prefix(64)
		if err == nil {
			return p.String()
		}
	}
	return a.Unmap().String()
}

// kdfSlots bounds concurrent PBKDF2 runs (600k iterations each), so a
// login flood from many addresses cannot starve the server's CPU.
const kdfSlots = 2

// sharedKDF is the one set of slots of the process: every Service takes from it,
// so the number of concurrent PBKDF2 runs stays bounded.
var sharedKDF = make(chan struct{}, kdfSlots)

// verifyBounded runs VerifyPassword in one of kdfSlots slots, waiting for
// a free slot as long as ctx allows.
func (s *Service) verifyBounded(ctx context.Context, hash, pass string) error {
	select {
	case s.kdf <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.kdf }()
	return VerifyPassword(hash, pass)
}

// maxUsernameLen caps usernames before they are used anywhere.
const maxUsernameLen = 128

// Device cookies (OWASP "slow down online guessing attacks with device
// cookies", docs/security/login-rate-limiting.md): after a successful
// break-glass login the browser gets the cookie
//
//	v1.<b64url username>.<issued unix ms>.<random id>.<b64url HMAC-SHA256>
//
// The MAC covers everything before the last dot (version, username, issue
// time, random id) and is checked in constant time before any field is
// parsed. The key is derived from the persisted SecretKey with its own
// purpose label (not shared with CSRF). A cookie is valid only for exactly
// its username, not issued in the future (1 min clock skew), younger than
// deviceTTL, and issued at or after LocalUser.UpdatedAt: a password change
// via the CLI revokes all of that user's device cookies, rotating the
// SecretKey revokes all of them.
const (
	deviceTTL     = 180 * 24 * time.Hour
	deviceVersion = "v1"
	deviceSkew    = time.Minute
	deviceMaxLen  = 512
)

func (s *Service) deviceCookieName(r *http.Request) string {
	if s.sessions.secure(r) {
		return CookieSecure + "-device"
	}
	return CookieInsecure + "-device"
}

func (s *Service) deviceMAC(payload string) string {
	m := hmac.New(sha256.New, s.deviceKey)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// devicePayload builds the signed part of a device cookie.
func devicePayload(user string, issued time.Time, id string) string {
	return deviceVersion + "." + base64.RawURLEncoding.EncodeToString([]byte(user)) + "." +
		strconv.FormatInt(issued.UnixMilli(), 10) + "." + id
}

func (s *Service) setDeviceCookie(w http.ResponseWriter, r *http.Request, user string, passwordGen int64) {
	id, now := rand.Text(), s.o.Clock.Now()
	// Recorded first: a cookie without a row is not valid (per-cookie revocation).
	if err := s.o.DeviceCookies.PutDeviceCookie(r.Context(), hashID(id), user, now, passwordGen); err != nil {
		slog.ErrorContext(r.Context(), "record device cookie", "err", err)
		return
	}
	payload := devicePayload(user, now, id)
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure follows the request scheme (spec §15.2)
		Name: s.deviceCookieName(r), Value: payload + "." + s.deviceMAC(payload),
		Path: s.sessions.cookiePath(r), MaxAge: int(deviceTTL.Seconds()),
		HttpOnly: true, Secure: s.sessions.secure(r), SameSite: http.SameSiteStrictMode,
	})
}

// validDeviceCookie reports whether r carries a genuine, current,
// unrevoked device cookie for exactly this username.
func (s *Service) validDeviceCookie(r *http.Request, user string) bool {
	c, err := r.Cookie(s.deviceCookieName(r))
	if err != nil || len(c.Value) > deviceMaxLen {
		return false
	}
	payload, mac, ok := cutLast(c.Value, ".")
	// Authenticate first; nothing in the payload is trusted before this.
	if !ok || mac == "" || !hmac.Equal([]byte(mac), []byte(s.deviceMAC(payload))) {
		return false
	}
	parts := strings.Split(payload, ".")
	if len(parts) != 4 || parts[0] != deviceVersion || parts[3] == "" {
		return false
	}
	name, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || string(name) != user {
		return false // bound to one username: no replay across users
	}
	issuedMS, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return false
	}
	issued, now := time.UnixMilli(issuedMS), s.o.Clock.Now()
	if issued.After(now.Add(deviceSkew)) || now.Sub(issued) > deviceTTL {
		return false
	}
	u, err := s.o.LocalUsers.GetLocalUser(r.Context(), user)
	if err != nil || issued.Before(u.UpdatedAt) {
		return false
	}
	known, err := s.o.DeviceCookies.HasDeviceCookie(r.Context(), hashID(parts[3]), user, u.PasswordGen)
	if err != nil {
		slog.ErrorContext(r.Context(), "check device cookie", "err", err)
		return false
	}
	return known // false: revoked individually or by a password change
}

// RevokeDeviceCookie revokes the one device cookie value (as sent by the
// browser) without touching the user's other devices. An invalid value is
// ignored: there is nothing to revoke.
func (s *Service) RevokeDeviceCookie(ctx context.Context, value string) error {
	payload, mac, ok := cutLast(value, ".")
	if !ok || mac == "" || len(value) > deviceMaxLen || !hmac.Equal([]byte(mac), []byte(s.deviceMAC(payload))) {
		return nil
	}
	parts := strings.Split(payload, ".")
	if len(parts) != 4 || parts[3] == "" {
		return nil
	}
	return s.o.DeviceCookies.DeleteDeviceCookie(ctx, hashID(parts[3]))
}

func cutLast(s, sep string) (before, after string, ok bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return "", "", false
	}
	return s[:i], s[i+len(sep):], true
}
