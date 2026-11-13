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