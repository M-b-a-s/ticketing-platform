package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/httpx"
)

// ctxKey is unexported, so no other package can collide with or overwrite our value.
type ctxKey struct{}

// Identity is who is making the request, as proven by a verified access token.
type Identity struct {
	UserID    string
	SessionID string
	Role      string
}

// UserUUID converts the ID for use in sqlc queries.
func (i Identity) UserUUID() (pgtype.UUID, error) {
	var u pgtype.UUID
	err := u.Scan(i.UserID)
	return u, err
}

// IdentityFrom returns the caller's identity, or false if the request did not
// pass through Authenticate.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok
}

// Authenticate rejects requests without a valid access token and stores the
// caller's Identity in the request context for the handlers behind it.
func Authenticate(tm *TokenManager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r)
			if !ok {
				unauthorized(w, "missing_token", "an Authorization header with a Bearer token is required")
				return
			}

			claims, err := tm.Verify(token)
			if err != nil {
				unauthorized(w, "invalid_token", "access token is invalid or expired")
				return
			}

			id := Identity{UserID: claims.Subject, SessionID: claims.SessionID, Role: claims.Role}
			if _, err := id.UserUUID(); err != nil {
				unauthorized(w, "invalid_token", "access token is invalid or expired")
				return
			}

			if !validRole(id.Role) {
				unauthorized(w, "invalid_token", "access token is invalid or expired")
				return
			}

			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
		})
	}
}

func bearerToken(r *http.Request) (string, bool) {
	scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

func unauthorized(w http.ResponseWriter, code, message string) {
	// RFC 6750: a 401 should tell the client which auth scheme to use.
	w.Header().Set("WWW-Authenticate", `Bearer realm="api"`)
	httpx.WriteError(w, http.StatusUnauthorized, code, message, nil)
}


// Role names must match the user_role enum in the database exactly.
const (
	RoleCustomer      = "customer"
	RoleOrganizer     = "organizer"
	RolePlatformAdmin = "platform_admin"
)

func validRole(r string) bool {
	switch r {
	case RoleCustomer, RoleOrganizer, RolePlatformAdmin:
		return true
	}
	return false
}

// HasRole reports whether the caller holds any of the given roles.
func (i Identity) HasRole(roles ...string) bool {
	for _, r := range roles {
		if i.Role == r {
			return true
		}
	}
	return false
}

// RequireRole allows only callers whose platform role is in the list.
// It must run after Authenticate. Roles are explicit, with no hierarchy:
// to let admins through an organizer route, list both.
func RequireRole(roles ...string) func(http.Handler) http.Handler {
	if len(roles) == 0 {
		panic("auth: RequireRole needs at least one role")
	}
	for _, r := range roles {
		if !validRole(r) {
			panic("auth: RequireRole given unknown role " + r) // catches typos at startup
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := IdentityFrom(r.Context())
			if !ok { // used without Authenticate: fail closed
				unauthorized(w, "missing_token", "authentication required")
				return
			}
			if !id.HasRole(roles...) {
				httpx.WriteError(w, http.StatusForbidden, "forbidden", "you do not have permission to perform this action", nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}