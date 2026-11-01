package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// Setup step 1 (design v2, N12): "Create the first admin account". At first
// start the initial admin is created with a generated password (bootstrap.go)
// and a one-time setup token is printed to the container log, in the same
// line, as a link <base>/setup/account?token=... Whoever holds the token sets
// the admin's password there and is signed in; then the token is spent.
//
// The token lives in memory only (its SHA-256); it is never stored, shown in
// the UI or written to the audit log. It is valid while the initial admin
// still has its first-start password: marked InitialSetup (set only when the
// first start inserts it, cleared by every password generation change),
// enabled, must change the password and at generation 1. An account created
// later under the same name (by an admin, whatever their subject, or the
// CLI) never qualifies, so the link in the log cannot take it over. Any password change (here, at /account/password,
// by another admin or the CLI) ends it for good, also across restarts; while
// it has not happened, every start prints a fresh token.

// SetupAccountPath is the page of setup step 1.
const SetupAccountPath = "/setup/account"

// SetupAccountData is rendered by SetupPages.SetupAccount.
type SetupAccountData struct {
	BasePath  string
	CSRFToken string // form field "csrf"
	// NeedToken: the page was opened without a valid token; it explains where
	// the link is (the container log) and shows no form. TokenInvalid
	// distinguishes a wrong or spent token from a missing one.
	NeedToken    bool
	TokenInvalid bool
	Token        string // the valid token, for the hidden form field "token"
	Username     string // the account being set up (InitialAdminName)
	DisplayName  string // typed values after a failed submit (never the password)
	Email        string
	Error        string
	ErrField     string // "password", "confirm", "display_name", "email"; empty: page level
}

// SetupPages renders setup step 1. Pages implementations may implement it;
// otherwise the built-in page is used.
type SetupPages interface {
	SetupAccount(w http.ResponseWriter, r *http.Request, status int, d SetupAccountData)
}

func (s *Service) setupPages() SetupPages {
	if p, ok := s.o.Pages.(SetupPages); ok {
		return p
	}
	return DefaultPages{}
}

// setupPending reports whether the initial admin still waits for its first
// password, the condition under which a setup token is valid.
func (s *Service) setupPending(ctx context.Context) bool {
	if !s.o.LocalLogin {
		return false
	}
	u, err := s.o.LocalUsers.GetLocalUser(ctx, InitialAdminName)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.ErrorContext(ctx, "check initial admin", "err", err)
		}
		return false
	}
	return u.InitialSetup && !u.Disabled && u.MustChangePassword && u.PasswordGen == 1
}

// issueSetupToken creates a new setup token, replacing any earlier one, and
// returns the link to setup step 1.
func (s *Service) issueSetupToken() string {
	tok := rand.Text() + rand.Text() // 2x 128 bits
	sum := sha256.Sum256([]byte(tok))
	s.setupMu.Lock()
	s.setupHash = sum[:]
	s.setupMu.Unlock()
	return s.o.Proxy.URL(SetupAccountPath + "?token=" + url.QueryEscape(tok))
}

// setupTokenValid checks tok against the current token in constant time.
func (s *Service) setupTokenValid(ctx context.Context, tok string) bool {
	s.setupMu.Lock()
	ok := s.setupMatchLocked(tok)
	s.setupMu.Unlock()
	return ok && s.setupPending(ctx)
}

func (s *Service) setupMatchLocked(tok string) bool {
	if s.setupHash == nil || tok == "" {
		return false
	}
	sum := sha256.Sum256([]byte(tok))
	return subtle.ConstantTimeCompare(sum[:], s.setupHash) == 1
}

// claimSetupToken checks tok and spends it in one step under the lock, so
// two concurrent submits cannot both get past the token check. It returns
// the spent hash for restoreSetupToken, or nil when tok is not valid.
func (s *Service) claimSetupToken(ctx context.Context, tok string) []byte {
	s.setupMu.Lock()
	if !s.setupMatchLocked(tok) {
		s.setupMu.Unlock()
		return nil
	}
	claimed := s.setupHash
	s.setupHash = nil
	s.setupMu.Unlock()
	if !s.setupPending(ctx) {
		return nil // the admin has a password already: the token stays spent
	}
	return claimed
}

// restoreSetupToken gives a claimed token back after a submit failed before
// it changed anything (form errors), unless a new token was issued meanwhile.
func (s *Service) restoreSetupToken(claimed []byte) {
	s.setupMu.Lock()
	if s.setupHash == nil {
		s.setupHash = claimed
	}
	s.setupMu.Unlock()
}

// setupReferrerPolicy keeps the token in the page URL out of Referer headers,
// also same-origin ones (the global policy is same-origin): "origin" sends
// only scheme://host, never path or query. It must not be no-referrer: the
// Fetch spec then serialises a form POST's Origin as "null", which the CSRF
// check rejects on plain HTTP, where browsers send no Sec-Fetch-Site (#429).
const setupReferrerPolicy = "origin"

func setupReferrer(w http.ResponseWriter) { w.Header().Set("Referrer-Policy", setupReferrerPolicy) }

// setupAccountPage is GET /setup/account?token=...
func (s *Service) setupAccountPage(w http.ResponseWriter, r *http.Request) {
	setupReferrer(w)
	ctx := r.Context()
	if !s.setupPending(ctx) {
		s.redirectLocal(w, r, "/login")
		return
	}
	tok := r.URL.Query().Get("token")
	d := SetupAccountData{BasePath: httpx.BasePath(ctx), Username: InitialAdminName}
	switch {
	case tok == "":
		d.NeedToken = true
		s.setupPages().SetupAccount(w, r, http.StatusOK, d)
	case !s.setupTokenValid(ctx, tok):
		d.NeedToken, d.TokenInvalid = true, true
		s.setupPages().SetupAccount(w, r, http.StatusForbidden, d)
	default:
		d.Token, d.CSRFToken = tok, s.formToken(w, r)
		s.setupPages().SetupAccount(w, r, http.StatusOK, d)
	}
}

// setupAccountSubmit is POST /setup/account (fields token, password,
// confirm, display_name, email). The CSRF middleware has checked the form
// token (pre-session cookie, like the sign-in).
func (s *Service) setupAccountSubmit(w http.ResponseWriter, r *http.Request) {
	setupReferrer(w)
	ctx := r.Context()
	if !s.setupPending(ctx) {
		s.redirectLocal(w, r, "/login")
		return
	}
	tok := r.PostFormValue("token")
	d := SetupAccountData{BasePath: httpx.BasePath(ctx), Username: InitialAdminName,
		DisplayName: r.PostFormValue("display_name"), Email: r.PostFormValue("email")}
	// Check and spend in one step; only form errors below give it back.
	claimed := s.claimSetupToken(ctx, tok)
	if claimed == nil {
		s.record(r, "anonymous", "setup.account", "denied: invalid setup token", nil)
		d.NeedToken, d.TokenInvalid = true, true
		s.setupPages().SetupAccount(w, r, http.StatusForbidden, d)
		return
	}
	d.Token = tok
	fail := func(status int, field, msg string) {
		s.restoreSetupToken(claimed) // nothing changed yet: the link stays usable
		d.CSRFToken, d.Error, d.ErrField = s.formToken(w, r), msg, field
		s.setupPages().SetupAccount(w, r, status, d)
	}
	pass, confirm := r.PostFormValue("password"), r.PostFormValue("confirm")
	display, email, err := CleanProfile(d.DisplayName, d.Email)
	switch {
	case err != nil:
		field, msg := inputProblem(err)
		fail(http.StatusBadRequest, field, msg)
		return
	case pass != confirm:
		fail(http.StatusBadRequest, "confirm", "The two passwords differ.")
		return
	}
	// Compare-and-set on generation 1: a concurrent change elsewhere wins and
	// this one fails as stale.
	if err := s.accounts.ChangeOwnPassword(ctx, InitialAdminName, 1, pass); err != nil {
		switch {
		case errors.Is(err, ErrWeakPassword):
			fail(http.StatusBadRequest, "password", msgWeakPassword)
		case errors.Is(err, ErrStaleActor):
			s.record(r, SubjectOf(InitialAdminName), "setup.account", "denied: account changed meanwhile", nil)
			s.redirectLocal(w, r, "/login")
		default: // the token stays spent; the next start prints a fresh one
			slog.ErrorContext(ctx, "setup: set the admin password", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	s.removeInitialPasswordFile(ctx)
	u, err := s.accounts.Get(ctx, InitialAdminName)
	if err == nil && (display != "" || email != "") {
		if perr := s.accounts.SetOwnProfile(ctx, InitialAdminName, u.PasswordGen, display, email); perr != nil {
			slog.WarnContext(ctx, "setup: profile not saved", "err", perr) // the password is set; the profile can be edited later
		}
	}
	p := model.Principal{Subject: SubjectOf(InitialAdminName), Name: InitialAdminName, Role: model.RoleAdmin, Method: ModeLocal}
	s.record(r, p.Subject, "setup.account", "ok", nil)
	if err == nil {
		_, err = s.sessions.CreateLocal(w, r, p, u.PasswordGen, false)
	}
	if err != nil {
		slog.ErrorContext(ctx, "setup: sign in", "err", err)
		s.redirectLocal(w, r, "/login")
		return
	}
	s.setDeviceCookie(w, r, InitialAdminName, u.PasswordGen)
	s.noteLogin(ctx, p)
	s.redirectLocal(w, r, "/setup")
}
