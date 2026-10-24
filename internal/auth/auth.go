// CI probe: measures the safety jobs (do not merge).
// Package auth authenticates web requests and maps identities to roles.
//
// Modes (TASMOFLEET_AUTH_MODE):
//
//   - oidc:   native OpenID Connect, authorization code flow with PKCE (S256),
//     discovery, state + nonce, ID token verification
//     (github.com/coreos/go-oidc/v3 + golang.org/x/oauth2), group claim
//     mapped to roles, RP-initiated logout via end_session_endpoint.
//   - header: trusted forward-auth headers (e.g. Authentik outpost behind
//     Traefik: X-authentik-username, X-authentik-groups "a|b"), accepted only
//     when the TCP peer (RemoteAddr, never X-Forwarded-For) is inside
//     TrustedProxies.
//
// In both modes a local break-glass admin can sign in on the /login page if one
// was created with "tasmofleet user add --role admin" (CLI; there is no
// web form that changes credentials). Passwords are hashed with PBKDF2-SHA256
// (crypto/pbkdf2, stdlib) 600000 iterations, 16-byte salt; login is rate
// limited per remote address.
//
// Sessions are server-side (store.SessionStore); the cookie holds 32 random
// bytes, HttpOnly, SameSite=Lax, Secure when the request is HTTPS (then named
// __Host-tasmofleet), idle timeout 12h, absolute 7d, rotated on login.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Errors.
var (
	ErrUnauthenticated = errors.New("auth: not authenticated")
	// ErrSessionExpired is ErrUnauthenticated for a request whose session
	// cookie named a session that ran out (idle or absolute limit): the
	// sign-in page then says so (design v2, N11).
	ErrSessionExpired = fmt.Errorf("%w: session expired", ErrUnauthenticated)
	ErrForbidden      = errors.New("auth: insufficient role")
	ErrNotImplemented = errors.New("auth: not implemented")
)

// Authenticator resolves the principal of a request and serves the login
// and logout endpoints of its mode.
type Authenticator interface {
	// Principal returns the authenticated principal or ErrUnauthenticated.
	Principal(r *http.Request) (model.Principal, error)
	// Routes returns the handlers for /login, /login/sso, /oidc/callback, /login/local
	// and /logout, relative to the base path.
	Routes() http.Handler
}

// RoleMapping maps groups to roles. A principal gets the highest role of
// any matching group; no match means RoleNone (access denied).
type RoleMapping struct {
	AdminGroups    []string
	OperatorGroups []string
}

// Role returns the role for the given groups. Group names match exactly
// (Authentik group names are case-sensitive).
func (m RoleMapping) Role(groups []string) model.Role {
	role := model.RoleNone
	for _, g := range groups {
		switch {
		case slices.Contains(m.AdminGroups, g):
			return model.RoleAdmin
		case slices.Contains(m.OperatorGroups, g):
			role = max(role, model.RoleOperator)
		}
	}
	return role
}

// FromContext returns the principal stored by the middleware.
func FromContext(ctx context.Context) (model.Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(model.Principal)
	return p, ok
}

// WithPrincipal stores p in ctx (used by the middleware and tests).
func WithPrincipal(ctx context.Context, p model.Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

type principalKey struct{}
