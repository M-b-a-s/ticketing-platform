package auth

import (
	"net/http"
	"testing"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
)

func chain(tm *TokenManager, roles ...string) (http.Handler, *bool) {
	called := false
	h := Authenticate(tm)(RequireRole(roles...)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})))
	return h, &called
}

func TestRequireRoleMatrix(t *testing.T) {
	tm := newTestManager(t)

	organizerOrAdmin := []string{RoleOrganizer, RolePlatformAdmin}
	adminOnly := []string{RolePlatformAdmin}
	everyone := []string{RoleCustomer, RoleOrganizer, RolePlatformAdmin}

	cases := []struct {
		name    string
		allowed []string
		role    string
		want    int
	}{
		{"customer on organizer route", organizerOrAdmin, RoleCustomer, http.StatusForbidden},
		{"organizer on organizer route", organizerOrAdmin, RoleOrganizer, http.StatusOK},
		{"admin on organizer route", organizerOrAdmin, RolePlatformAdmin, http.StatusOK},
		{"customer on admin route", adminOnly, RoleCustomer, http.StatusForbidden},
		{"organizer on admin route", adminOnly, RoleOrganizer, http.StatusForbidden},
		{"admin on admin route", adminOnly, RolePlatformAdmin, http.StatusOK},
		{"customer on open route", everyone, RoleCustomer, http.StatusOK},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, called := chain(tm, c.allowed...)
			tok, _, _ := tm.Issue(testUserID, testSessionID, c.role)

			rec := requestWith(h, "Bearer "+tok)

			if rec.Code != c.want {
				t.Fatalf("want %d, got %d", c.want, rec.Code)
			}
			if *called != (c.want == http.StatusOK) {
				t.Fatalf("handler called=%v, want %v", *called, c.want == http.StatusOK)
			}
		})
	}
}

func TestRequireRoleWithoutAuthenticateFailsClosed(t *testing.T) {
	called := false
	h := RequireRole(RoleCustomer)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))

	rec := requestWith(h, "")

	if rec.Code != http.StatusUnauthorized || called {
		t.Fatalf("want 401 and no handler call, got %d called=%v", rec.Code, called)
	}
}

func TestRequireRoleRejectsBadConfiguration(t *testing.T) {
	mustPanic := func(name string, f func()) {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected a panic")
				}
			}()
			f()
		})
	}
	mustPanic("no roles", func() { RequireRole() })
	mustPanic("typo", func() { RequireRole("organiser") })
}

func TestAuthenticateRejectsUnknownRole(t *testing.T) {
	tm := newTestManager(t)
	h, called, _ := protected(tm)
	tok, _, _ := tm.Issue(testUserID, testSessionID, "superuser")

	if rec := requestWith(h, "Bearer "+tok); rec.Code != http.StatusUnauthorized || *called {
		t.Fatalf("unknown role must be rejected, got %d", rec.Code)
	}
}

// If the database enum and these constants ever drift apart, this fails.
func TestRoleConstantsMatchDatabaseEnum(t *testing.T) {
	pairs := map[string]string{
		RoleCustomer:      string(db.UserRoleCustomer),
		RoleOrganizer:     string(db.UserRoleOrganizer),
		RolePlatformAdmin: string(db.UserRolePlatformAdmin),
	}
	for constant, fromDB := range pairs {
		if constant != fromDB {
			t.Fatalf("constant %q does not match database enum value %q", constant, fromDB)
		}
	}
}