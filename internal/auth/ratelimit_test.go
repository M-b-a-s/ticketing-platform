package auth

import (
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/middleware"
)

func limitedRouter(users UserStore, limits Limits) http.Handler {
	h := NewHandler(newSvc(users, &fakeSessions{}), slog.New(slog.NewTextHandler(io.Discard, nil))).WithLimits(limits)
	r := chi.NewRouter()
	h.Routes(r)
	return r
}

func TestRegisterIsLimitedPerIP(t *testing.T) {
	r := limitedRouter(&fakeStore{}, Limits{
		IPKey:    middleware.KeyByIP(false),
		Register: middleware.NewLimiter(time.Minute, 2),
	})

	for i := 0; i < 2; i++ {
		if rec := post(t, r, validBody); rec.Code != http.StatusCreated {
			t.Fatalf("attempt %d should succeed, got %d", i+1, rec.Code)
		}
	}
	rec := post(t, r, validBody)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("3rd registration should be 429 with Retry-After, got %d", rec.Code)
	}
}

func TestLoginIsLimitedPerAccountAcrossCaseAndIP(t *testing.T) {
	r := limitedRouter(existingUser(t, "a-long-enough-password"), Limits{
		LoginByEmail: middleware.NewLimiter(time.Minute, 2),
	})

	wrong1 := loginRequestTo(r, `{"email":"ada@example.com","password":"wrong-password-1"}`)
	wrong2 := loginRequestTo(r, `{"email":"ADA@Example.com","password":"wrong-password-2"}`)
	if wrong1.Code != http.StatusUnauthorized || wrong2.Code != http.StatusUnauthorized {
		t.Fatalf("first two attempts should reach the password check, got %d/%d", wrong1.Code, wrong2.Code)
	}

	// even the CORRECT password is refused while the account is throttled
	blocked := loginRequestTo(r, `{"email":"ada@example.com","password":"a-long-enough-password"}`)
	if blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429 while throttled, got %d", blocked.Code)
	}

	// another account is unaffected
	other := loginRequestTo(r, `{"email":"someone@example.com","password":"wrong-password-3"}`)
	if other.Code != http.StatusUnauthorized {
		t.Fatalf("other accounts must not be affected, got %d", other.Code)
	}
}

func TestLoginIsLimitedPerIP(t *testing.T) {
	r := limitedRouter(&fakeLoginStore{}, Limits{
		IPKey: middleware.KeyByIP(false),
		Login: middleware.NewLimiter(time.Minute, 3),
	})

	for i := 0; i < 3; i++ {
		if rec := loginRequestTo(r, `{"email":"x@example.com","password":"wrong-password-1"}`); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got %d", i+1, rec.Code)
		}
	}
	if rec := loginRequestTo(r, `{"email":"y@example.com","password":"wrong-password-1"}`); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("4th attempt from the same IP should be 429 whatever the email, got %d", rec.Code)
	}
}