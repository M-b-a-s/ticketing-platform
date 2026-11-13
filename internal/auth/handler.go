package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
	"context"
	"net"
	"net/netip"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/httpx"
)

const maxBodyBytes = 8 << 10 // 8 KiB is plenty for a registration payload

type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

func (h *Handler) Routes(r chi.Router) {
	r.Post("/auth/register", h.register)
	r.Post("/auth/login", h.login)
	r.Post("/auth/refresh", h.refresh)
}

//  -- register --

type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	FullName string `json:"full_name"`
}

type userResponse struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	FullName  string    `json:"full_name"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

func newUserResponse(u db.CreateUserRow) userResponse {
	return userResponse{
		ID:        uuidString(u.ID),
		Email:     u.Email,
		FullName:  u.FullName,
		Role:      string(u.Role),
		CreatedAt: u.CreatedAt.Time,
	}
}

func uuidString(u pgtype.UUID) string {
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	// Rejecting unknown fields means a client sending {"role":"admin"}
	// gets a 400 instead of having the field silently ignored.
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req registerRequest
	if err := dec.Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON or has unknown fields", nil)
		return
	}

	user, err := h.svc.Register(r.Context(), RegisterInput{
		Email:    req.Email,
		Password: req.Password,
		FullName: req.FullName,
	})

	var ve *ValidationError
	switch {
	case errors.As(err, &ve):
		httpx.WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "one or more fields are invalid", ve.Fields)
	case errors.Is(err, ErrEmailTaken):
		httpx.WriteError(w, http.StatusConflict, "email_taken", "an account with this email already exists", nil)
	case err != nil:
		h.log.ErrorContext(r.Context(), "register failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong", nil)
	default:
		httpx.WriteJSON(w, http.StatusCreated, newUserResponse(user))
	}
}

// -- login --

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginResponse struct {
	AccessToken  string       `json:"access_token"`
	TokenType    string       `json:"token_type"`
	ExpiresAt    time.Time    `json:"expires_at"`
	RefreshToken string       `json:"refresh_token"`
	User         userResponse `json:"user"`
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req loginRequest
	if err := dec.Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON or has unknown fields", nil)
		return
	}

	// The login handler calls the service's Login method with the provided email and password, along with the user agent and client IP address. It handles different error cases, returning appropriate HTTP status codes and messages. If successful, it returns a JSON response containing the access token, refresh token, expiration time, and user profile information.
	res, err := h.svc.Login(r.Context(), LoginInput{
		Email:     req.Email,
		Password:  req.Password,
		UserAgent: r.UserAgent(),
		IP:        clientIP(r),
	})

	switch {
	case errors.Is(err, ErrInvalidCredentials):
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_credentials", "email or password is incorrect", nil)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		httpx.WriteError(w, http.StatusServiceUnavailable, "busy", "server is busy, try again shortly", nil)
	case err != nil:
		h.log.ErrorContext(r.Context(), "login failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong", nil)
	default:
		w.Header().Set("Cache-Control", "no-store")
		httpx.WriteJSON(w, http.StatusOK, loginResponse{
			AccessToken:  res.AccessToken,
			TokenType:    "Bearer",
			ExpiresAt:    res.AccessExpiresAt,
			RefreshToken: res.RefreshToken,
			User: userResponse{
				ID:       res.User.ID,
				Email:    res.User.Email,
				FullName: res.User.FullName,
				Role:     res.User.Role,
			},
		})
	}
}


// clientIP uses the socket address. Behind Nginx this is the proxy's address;
// we'll revisit trusted forwarding headers at deployment (Phase 10).
func clientIP(r *http.Request) *netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return nil
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return nil
	}
	return &addr
}

// -- refresh --
type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type refreshResponse struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type"`
	ExpiresAt    time.Time `json:"expires_at"`
	RefreshToken string    `json:"refresh_token"`
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req refreshRequest
	if err := dec.Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON or has unknown fields", nil)
		return
	}

	res, err := h.svc.Refresh(r.Context(), req.RefreshToken)

	switch {
	case errors.Is(err, ErrRefreshReuse): // must come first: it wraps ErrInvalidToken
		h.log.WarnContext(r.Context(), "refresh token reuse detected; session revoked")
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_token", "refresh token is invalid or expired", nil)
	case errors.Is(err, ErrInvalidToken):
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_token", "refresh token is invalid or expired", nil)
	case err != nil:
		h.log.ErrorContext(r.Context(), "refresh failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong", nil)
	default:
		w.Header().Set("Cache-Control", "no-store")
		httpx.WriteJSON(w, http.StatusOK, refreshResponse{
			AccessToken:  res.AccessToken,
			TokenType:    "Bearer",
			ExpiresAt:    res.AccessExpiresAt,
			RefreshToken: res.RefreshToken,
		})
	}
}