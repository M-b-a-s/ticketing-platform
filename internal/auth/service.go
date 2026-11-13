package auth

import (
	"context"
	"errors"
	"net/mail"
	"net/netip"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
)

const (
	MinPasswordLen = 10
	MaxNameLen     = 100
)

type UserStore interface {
	CreateUser(ctx context.Context, arg db.CreateUserParams) (db.CreateUserRow, error)
	GetUserByEmail(ctx context.Context, email string) (db.GetUserByEmailRow, error)
}

type SessionStore interface {
	CreateSession(ctx context.Context, arg db.CreateSessionParams) (db.CreateSessionRow, error)
	RotateSession(ctx context.Context, arg db.RotateSessionParams) (db.RotateSessionRow, error)
	GetSessionByPreviousHash(ctx context.Context, hash string) (db.GetSessionByPreviousHashRow, error)
	RevokeSession(ctx context.Context, id pgtype.UUID) error
	RevokeSessionByRefreshHash(ctx context.Context, refreshTokenHash string) error
}

type Config struct {
	RefreshTTL          time.Duration // default 30 days
	MaxConcurrentHashes int           // default 4; each hash uses Params.Memory
}

type Service struct {
	users     UserStore
	sessions  SessionStore
	tokens    *TokenManager
	params    Params
	cfg       Config
	sem       chan struct{}
	dummyHash string
	now       func() time.Time
}

func NewService(users UserStore, sessions SessionStore, tokens *TokenManager, params Params, cfg Config) (*Service, error) {
	if cfg.MaxConcurrentHashes <= 0 {
		cfg.MaxConcurrentHashes = 4
	}
	if cfg.RefreshTTL <= 0 {
		cfg.RefreshTTL = 30 * 24 * time.Hour
	}

	// Verified against when the email doesn't exist, so "unknown email" costs
	// the same time as "wrong password" and response time leaks nothing.
	dummy, err := HashPassword("dummy-password-for-timing", params)
	if err != nil {
		return nil, err
	}

	return &Service{
		users:     users,
		sessions:  sessions,
		tokens:    tokens,
		params:    params,
		cfg:       cfg,
		sem:       make(chan struct{}, cfg.MaxConcurrentHashes),
		dummyHash: dummy,
		now:       time.Now,
	}, nil
}

// ---- concurrency cap on password hashing ----

func (s *Service) acquire(ctx context.Context) (func(), error) {
	select {
	case s.sem <- struct{}{}:
		return func() { <-s.sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Service) hashPassword(ctx context.Context, password string) (string, error) {
	release, err := s.acquire(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	return HashPassword(password, s.params)
}

func (s *Service) verifyPassword(ctx context.Context, password, hash string) (bool, error) {
	release, err := s.acquire(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	return VerifyPassword(password, hash)
}

// ---- registration ----

type RegisterInput struct {
	Email    string
	Password string
	FullName string
}

type ValidationError struct {
	Fields map[string]string
}

func (e *ValidationError) Error() string { return "auth: validation failed" }

func (in *RegisterInput) normalizeAndValidate() error {
	in.Email = strings.TrimSpace(in.Email)
	in.FullName = strings.TrimSpace(in.FullName)

	fields := map[string]string{}

	if addr, err := mail.ParseAddress(in.Email); err != nil || addr.Address != in.Email {
		fields["email"] = "must be a valid email address"
	}
	if n := utf8.RuneCountInString(in.Password); n < MinPasswordLen {
		fields["password"] = "must be at least 10 characters"
	} else if len(in.Password) > MaxPasswordLen {
		fields["password"] = "must be at most 128 bytes"
	}
	if in.FullName == "" || utf8.RuneCountInString(in.FullName) > MaxNameLen {
		fields["full_name"] = "is required and must be at most 100 characters"
	}

	if len(fields) > 0 {
		return &ValidationError{Fields: fields}
	}
	return nil
}

func (s *Service) Register(ctx context.Context, in RegisterInput) (db.CreateUserRow, error) {
	if err := in.normalizeAndValidate(); err != nil {
		return db.CreateUserRow{}, err
	}

	hash, err := s.hashPassword(ctx, in.Password)
	if err != nil {
		return db.CreateUserRow{}, err
	}

	user, err := s.users.CreateUser(ctx, db.CreateUserParams{
		Email:        in.Email,
		PasswordHash: hash,
		FullName:     in.FullName,
	})
	if err != nil {
		if isEmailTaken(err) {
			return db.CreateUserRow{}, ErrEmailTaken
		}
		return db.CreateUserRow{}, err
	}
	return user, nil
}

// ---- login ----

type LoginInput struct {
	Email     string
	Password  string
	UserAgent string
	IP        *netip.Addr
}

type UserProfile struct {
	ID       string
	Email    string
	FullName string
	Role     string
}

type LoginResult struct {
	AccessToken     string
	AccessExpiresAt time.Time
	RefreshToken    string
	User            UserProfile
}

// Login returns ErrInvalidCredentials for BOTH an unknown email and a wrong
// password, so callers cannot tell which accounts exist.
func (s *Service) Login(ctx context.Context, in LoginInput) (*LoginResult, error) {
	email := strings.TrimSpace(in.Email)
	if email == "" || in.Password == "" || len(in.Password) > MaxPasswordLen {
		return nil, ErrInvalidCredentials
	}

	user, err := s.users.GetUserByEmail(ctx, email)
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	hash := s.dummyHash
	if found {
		hash = user.PasswordHash
	}

	ok, err := s.verifyPassword(ctx, in.Password, hash)
	if err != nil {
		return nil, err
	}
	if !found || !ok {
		return nil, ErrInvalidCredentials
	}

	refreshToken, refreshHash, err := NewRefreshToken()
	if err != nil {
		return nil, err
	}

	sess, err := s.sessions.CreateSession(ctx, db.CreateSessionParams{
		UserID:           user.ID,
		RefreshTokenHash: refreshHash,
		UserAgent:        pgtype.Text{String: in.UserAgent, Valid: in.UserAgent != ""},
		Ip:               in.IP,
		ExpiresAt:        pgtype.Timestamptz{Time: s.now().Add(s.cfg.RefreshTTL), Valid: true},
	})
	if err != nil {
		return nil, err
	}

	accessToken, accessExp, err := s.tokens.Issue(uuidString(user.ID), uuidString(sess.ID), string(user.Role))
	if err != nil {
		return nil, err
	}

	return &LoginResult{
		AccessToken:     accessToken,
		AccessExpiresAt: accessExp,
		RefreshToken:    refreshToken,
		User: UserProfile{
			ID:       uuidString(user.ID),
			Email:    user.Email,
			FullName: user.FullName,
			Role:     string(user.Role),
		},
	}, nil
}

// ---- refresh ----

type RefreshResult struct {
	AccessToken     string
	AccessExpiresAt time.Time
	RefreshToken    string
}

// Refresh swaps a valid refresh token for a new access token AND a new
// refresh token. The old refresh token stops working immediately.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*RefreshResult, error) {
	if refreshToken == "" || len(refreshToken) > 256 {
		return nil, ErrInvalidToken
	}
	oldHash := HashRefreshToken(refreshToken)

	newToken, newHash, err := NewRefreshToken()
	if err != nil {
		return nil, err
	}

	sess, err := s.sessions.RotateSession(ctx, db.RotateSessionParams{OldHash: oldHash, NewHash: newHash})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.rejectUnrotatable(ctx, oldHash)
	}
	if err != nil {
		return nil, err
	}

	access, exp, err := s.tokens.Issue(uuidString(sess.UserID), uuidString(sess.ID), string(sess.Role))
	if err != nil {
		return nil, err
	}

	return &RefreshResult{AccessToken: access, AccessExpiresAt: exp, RefreshToken: newToken}, nil
}

// rejectUnrotatable runs when no active session holds this token. If the token
// was the PREVIOUS one for a live session, someone is replaying a token that
// was already exchanged. Either the client or an attacker has a stale copy,
// and we can't tell which, so we revoke the whole session.
func (s *Service) rejectUnrotatable(ctx context.Context, oldHash string) error {
	reused, err := s.sessions.GetSessionByPreviousHash(ctx, oldHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidToken
	}
	if err != nil {
		return err
	}
	if err := s.sessions.RevokeSession(ctx, reused.ID); err != nil {
		return err // fail loudly: a failed revoke is a security problem
	}
	return ErrRefreshReuse
}

// -- logout --

// Logout revokes the session holding this refresh token. It is idempotent:
// an unknown, expired or already-revoked token is not an error.
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	if refreshToken == "" || len(refreshToken) > 256 {
		return nil
	}
	return s.sessions.RevokeSessionByRefreshHash(ctx, HashRefreshToken(refreshToken))
}