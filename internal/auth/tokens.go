package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	MinJWTSecretLen   = 32
	refreshTokenBytes = 32
)

var ErrInvalidToken = errors.New("auth: invalid or expired token")
// ErrRefreshReuse wraps ErrInvalidToken, so callers that don't care about the
// difference treat it as a plain 401, while the handler can log it loudly.
var ErrRefreshReuse = fmt.Errorf("%w: refresh token reuse detected", ErrInvalidToken)

// ---- Refresh tokens: random, opaque, stored only as a hash ----

// NewRefreshToken returns the token to send to the client and the hash to store.
func NewRefreshToken() (token, hash string, err error) {
	b := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate refresh token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashRefreshToken(token), nil
}

// HashRefreshToken uses plain SHA-256: the token already has 256 bits of
// randomness, so a slow hash like argon2 would add cost without adding safety.
func HashRefreshToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ---- Access tokens: short-lived signed JWTs ----

type Claims struct {
	Role      string `json:"role"`
	SessionID string `json:"sid"`
	jwt.RegisteredClaims
}

type TokenManager struct {
	secret []byte
	issuer string
	ttl    time.Duration
	now    func() time.Time
}

func NewTokenManager(secret string, ttl time.Duration) (*TokenManager, error) {
	if len(secret) < MinJWTSecretLen {
		return nil, fmt.Errorf("JWT secret must be at least %d bytes", MinJWTSecretLen)
	}
	return &TokenManager{
		secret: []byte(secret),
		issuer: "ticketing-platform",
		ttl:    ttl,
		now:    time.Now,
	}, nil
}

func (m *TokenManager) Issue(userID, sessionID, role string) (string, time.Time, error) {
	now := m.now()
	exp := now.Add(m.ttl)

	claims := Claims{
		Role:      role,
		SessionID: sessionID,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    m.issuer,
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign access token: %w", err)
	}
	return signed, exp, nil
}

func (m *TokenManager) Verify(token string) (*Claims, error) {
	var claims Claims
	_, err := jwt.ParseWithClaims(token, &claims,
		func(*jwt.Token) (any, error) { return m.secret, nil },
		jwt.WithValidMethods([]string{"HS256"}), // blocks alg=none and algorithm-swap attacks
		jwt.WithIssuer(m.issuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(m.now),
	)
	if err != nil {
		return nil, ErrInvalidToken
	}
	return &claims, nil
}