package orders

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/auth"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/httpx"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/middleware"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/pagination"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/uuidx"
)

const maxBodyBytes = 8 << 10

type Handler struct {
	svc         *Service
	log         *slog.Logger
	createLimit *middleware.Limiter
}

func NewHandler(svc *Service, log *slog.Logger) *Handler { return &Handler{svc: svc, log: log} }

// WithCreateLimit adds a per-user rate limit on order creation, on top of the per-IP
// limit. A signed-in bot is the realistic way to hoard stock.
func (h *Handler) WithCreateLimit(l *middleware.Limiter) *Handler {
	h.createLimit = l
	return h
}

func (h *Handler) perUser(l *middleware.Limiter) func(http.Handler) http.Handler {
	if l == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return middleware.RateLimit(l, func(r *http.Request) string {
		id, _ := auth.IdentityFrom(r.Context())
		return id.UserID
	})
}

// Routes must be mounted behind auth.Authenticate, directly under /v1.
func (h *Handler) Routes(r chi.Router) {
	r.With(h.perUser(h.createLimit)).Post("/events/{eventID}/orders", h.create)
	r.Get("/orders", h.list)
	r.Get("/orders/{orderID}", h.get)
	r.Post("/orders/{orderID}/cancel", h.cancel)
}

// ---- JSON shapes ----

type createRequest struct {
	Items []lineRequest `json:"items"`
}

type lineRequest struct {
	TicketTypeID string `json:"ticket_type_id"`
	Quantity     int    `json:"quantity"`
}

type itemResponse struct {
	TicketTypeID  string `json:"ticket_type_id"`
	Name          string `json:"name"`
	Quantity      int32  `json:"quantity"`
	UnitPriceKobo int64  `json:"unit_price_kobo"`
	LineTotalKobo int64  `json:"line_total_kobo"`
}

type orderSummary struct {
	ID        string     `json:"id"`
	EventID   string     `json:"event_id"`
	Status    string     `json:"status"`
	TotalKobo int64      `json:"total_kobo"`
	Currency  string     `json:"currency"`
	ExpiresAt time.Time  `json:"expires_at"`
	PaidAt    *time.Time `json:"paid_at"`
	CreatedAt time.Time  `json:"created_at"`
}

type orderResponse struct {
	orderSummary
	Items []itemResponse `json:"items"`
}

func newOrderSummary(o db.Order) orderSummary {
	s := orderSummary{
		ID: uuidx.String(o.ID), EventID: uuidx.String(o.EventID), Status: o.Status,
		TotalKobo: o.TotalKobo, Currency: o.Currency,
		ExpiresAt: o.ExpiresAt.Time.UTC(), CreatedAt: o.CreatedAt.Time.UTC(),
	}
	if o.PaidAt.Valid {
		t := o.PaidAt.Time.UTC()
		s.PaidAt = &t
	}
	return s
}

func newOrderResponse(v View) orderResponse {
	items := make([]itemResponse, 0, len(v.Items))
	for _, it := range v.Items {
		items = append(items, itemResponse{
			TicketTypeID: uuidx.String(it.TicketTypeID), Name: it.Name, Quantity: it.Quantity,
			UnitPriceKobo: it.UnitPriceKobo, LineTotalKobo: it.UnitPriceKobo * int64(it.Quantity),
		})
	}
	return orderResponse{orderSummary: newOrderSummary(v.Order), Items: items}
}

// ---- helpers ----

// Orders are private: they must never be cached by a shared cache.
func writeOrder(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, status, v)
}

func caller(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	if id, ok := auth.IdentityFrom(r.Context()); ok {
		if u, err := id.UserUUID(); err == nil {
			return u, true
		}
	}
	httpx.WriteError(w, http.StatusUnauthorized, "missing_token", "authentication required", nil)
	return pgtype.UUID{}, false
}

func notFound(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusNotFound, "not_found", "not found", nil)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var ve *ValidationError
	var ce *ConflictError
	switch {
	case errors.As(err, &ve):
		httpx.WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "one or more fields are invalid", ve.Fields)
	case errors.As(err, &ce):
		httpx.WriteError(w, http.StatusConflict, ce.Code, ce.Message, nil)
	case errors.Is(err, ErrNotFound):
		notFound(w)
	default:
		h.log.ErrorContext(r.Context(), "order request failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong", nil)
	}
}

func orderParam(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	id, err := uuidx.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		notFound(w)
		return pgtype.UUID{}, false
	}
	return id, true
}

// ---- handlers ----

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	user, ok := caller(w, r)
	if !ok {
		return
	}
	eventID, err := uuidx.Parse(chi.URLParam(r, "eventID"))
	if err != nil {
		notFound(w)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields() // a client-supplied user_id, price or total is a 400, never silently ignored
	var req createRequest
	if err := dec.Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON or has unknown fields", nil)
		return
	}

	lines := make([]Line, 0, len(req.Items))
	fields := map[string]string{}
	for i, it := range req.Items {
		id, err := uuidx.Parse(it.TicketTypeID)
		if err != nil {
			fields[fmt.Sprintf("items[%d].ticket_type_id", i)] = "must be a valid UUID"
			continue
		}
		lines = append(lines, Line{TicketTypeID: id, Quantity: it.Quantity})
	}
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}

	view, err := h.svc.Create(r.Context(), CreateInput{UserID: user, EventID: eventID, Lines: lines})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/orders/"+uuidx.String(view.Order.ID))
	writeOrder(w, http.StatusCreated, newOrderResponse(view))
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	user, ok := caller(w, r)
	if !ok {
		return
	}
	limit, err := pagination.ParseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_query", "limit must be a positive integer", nil)
		return
	}
	var after *pagination.Cursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, err := pagination.Decode(raw)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "invalid_query", "cursor is invalid", nil)
			return
		}
		after = &c
	}

	page, err := h.svc.List(r.Context(), user, limit, after)
	if errors.Is(err, pagination.ErrInvalidCursor) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_query", "cursor is invalid", nil)
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}

	items := make([]orderSummary, 0, len(page.Items))
	for _, o := range page.Items {
		items = append(items, newOrderSummary(o))
	}
	writeOrder(w, http.StatusOK, map[string]any{"items": items, "next_cursor": page.NextCursor})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	user, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := orderParam(w, r)
	if !ok {
		return
	}
	view, err := h.svc.Get(r.Context(), user, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeOrder(w, http.StatusOK, newOrderResponse(view))
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	user, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := orderParam(w, r)
	if !ok {
		return
	}
	view, err := h.svc.Cancel(r.Context(), user, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeOrder(w, http.StatusOK, newOrderResponse(view))
}