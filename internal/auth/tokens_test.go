package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func newTestManager(t *testing.T) *TokenManager {
	t.Helper()
	m, err := NewTokenManager(testSecret, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRefreshTokenHashing(t *testing.T) {
	tok1, hash1, _ := NewRefreshToken()
	tok2, hash2, _ := NewRefreshToken()

	if tok1 == tok2 || hash1 == hash2 {
		t.Fatal("tokens must be unique")
	}
	if HashRefreshToken(tok1) != hash1 {
		t.Fatal("hashing must be deterministic")
	}
	if tok1 == hash1 {
		t.Fatal("the stored hash must differ from the token")
	}
}

func TestAccessTokenRoundTrip(t *testing.T) {
	m := newTestManager(t)

	tok, _, err := m.Issue("user-1", "session-1", "customer")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := m.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Subject != "user-1" || claims.SessionID != "session-1" || claims.Role != "customer" {
		t.Fatalf("unexpected claims: %+v", claims)
	}
}

func TestAccessTokenExpires(t *testing.T) {
	m := newTestManager(t)
	tok, _, _ := m.Issue("user-1", "session-1", "customer")

	m.now = func() time.Time { return time.Now().Add(16 * time.Minute) }

	if _, err := m.Verify(tok); err != ErrInvalidToken {
		t.Fatalf("expected expired token to be rejected, got %v", err)
	}
}

func TestAccessTokenWrongSecret(t *testing.T) {
	m := newTestManager(t)
	other, _ := NewTokenManager("ffffffffffffffffffffffffffffffff", 15*time.Minute)
	tok, _, _ := other.Issue("user-1", "session-1", "customer")

	if _, err := m.Verify(tok); err != ErrInvalidToken {
		t.Fatalf("token signed with another secret must be rejected, got %v", err)
	}
}

func TestAccessTokenTampered(t *testing.T) {
	m := newTestManager(t)
	tok, _, _ := m.Issue("user-1", "session-1", "customer")

	if _, err := m.Verify(tok + "x"); err != ErrInvalidToken {
		t.Fatalf("tampered token must be rejected, got %v", err)
	}
}

func TestAccessTokenAlgNoneRejected(t *testing.T) {
	m := newTestManager(t)
	claims := Claims{
		Role: "admin",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    m.issuer,
			Subject:   "user-1",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	tok, _ := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)

	if _, err := m.Verify(tok); err != ErrInvalidToken {
		t.Fatalf("alg=none token must be rejected, got %v", err)
	}
}

func TestTokenManagerRejectsShortSecret(t *testing.T) {
	if _, err := NewTokenManager("too-short", time.Minute); err == nil {
		t.Fatal("expected error for short secret")
	}
}