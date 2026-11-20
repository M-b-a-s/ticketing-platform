package tickets

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/auth"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/httpx"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/uuidx"
)

const (
	maxBodyBytes       = 8 << 10
	publicCacheControl = "public, max-age=15"
)

type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler { return &Handler{svc: svc, log: log} }

// ManageRoutes must be mounted behind auth.Authenticate, under /v1/manage.
// Viewing needs gate access; changing anything needs manage access.
func (h *Handler) ManageRoutes(r chi.Router) {
	view := auth.RequireEventAccess(h.svc.store, auth.AccessGate, "eventID")
	manage := auth.RequireEventAccess(h.svc.store, auth.AccessManage, "eventID")

	r.With(view).Get("/events/{eventID}/ticket-types", h.list)
	r.With(manage).Post("/events/{eventID}/ticket-types", h.create)
	r.With(view).Get("/events/{eventID}/ticket-types/{ticketTypeID}", h.get)
	r.With(manage).Patch("/events/{eventID}/ticket-types/{ticketTypeID}", h.update)
	r.With(manage).Delete("/events/{eventID}/ticket-types/{ticketTypeID}", h.delete)
}

// PublicRoutes needs no login. Mount it directly under /v1.
func (h *Handler) PublicRoutes(r chi.Router) {
	r.Get("/events/{eventID}/ticket-types", h.listPublic)
}

// ---- JSON shapes ----

type createRequest struct {
	Name          string    `json:"name"`
	PriceKobo     *int64    `json:"price_kobo"`
	Currency      string    `json:"currency"`
	TotalQuantity int       `json:"total_quantity"`
	MaxPerOrder   int       `json:"max_per_order"`
	SalesStart    time.Time `json:"sales_start"`
	SalesEnd      time.Time `json:"sales_end"`
}

type patchRequest struct {
	Name          *string    `json:"name"`
	PriceKobo     *int64     `json:"price_kobo"`
	TotalQuantity *int       `json:"total_quantity"`
	MaxPerOrder   *int       `json:"max_per_order"`
	SalesStart    *time.Time `json:"sales_start"`
	SalesEnd      *time.Time `json:"sales_end"`
}

type ticketTypeResponse struct {
	ID                string    `json:"id"`
	EventID           string    `json:"event_id"`
	Name              string    `json:"name"`
	PriceKobo         int64     `json:"price_kobo"`
	Currency          string    `json:"currency"`
	TotalQuantity     int32     `json:"total_quantity"`
	AvailableQuantity int32     `json:"available_quantity"`
	SoldOrReserved    int32     `json:"sold_or_reserved"`
	MaxPerOrder       int32     `json:"max_per_order"`
	SalesStart        time.Time `json:"sales_start"`
	SalesEnd          time.Time `json:"sales_end"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func newTicketTypeResponse(t db.TicketType) ticketTypeResponse {
	return ticketTypeResponse{
		ID:                uuidx.String(t.ID),
		EventID:           uuidx.String(t.EventID),
		Name:              t.Name,
		PriceKobo:         t.PriceKobo,
		Currency:          t.Currency,
		TotalQuantity:     t.TotalQuantity,
		AvailableQuantity: t.AvailableQuantity,
		SoldOrReserved:    t.TotalQuantity - t.AvailableQuantity,
		MaxPerOrder:       t.MaxPerOrder,
		SalesStart:        t.SalesStart.Time.UTC(),
		SalesEnd:          t.SalesEnd.Time.UTC(),
		CreatedAt:         t.CreatedAt.Time.UTC(),
		UpdatedAt:         t.UpdatedAt.Time.UTC(),
	}
}

// The public shape never exposes stock counts, only a status.
type publicTicketTypeResponse struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	PriceKobo   int64     `json:"price_kobo"`
	Currency    string    `json:"currency"`
	MaxPerOrder int32     `json:"max_per_order"`
	SalesStart  time.Time `json:"sales_start"`
	SalesEnd    time.Time `json:"sales_end"`
	SalesStatus string    `json:"sales_status"`
}

// ---- helpers ----

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields() // event_id, available_quantity and friends are a 400, not silently ignored
	if err := dec.Decode(dst); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_json",
			"request body is not valid JSON, has unknown fields, or a timestamp is not RFC 3339", nil)
		return false
	}
	return true
}

func notFound(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusNotFound, "not_found", "not found", nil)
}

func eventParam(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	id, err := uuidx.Parse(chi.URLParam(r, "eventID"))
	if err != nil {
		notFound(w)
		return pgtype.UUID{}, false
	}
	return id, true
}

func ticketTypeParam(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	id, err := uuidx.Parse(chi.URLParam(r, "ticketTypeID"))
	if err != nil {
		notFound(w)
		return pgtype.UUID{}, false
	}
	return id, true
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
		h.log.ErrorContext(r.Context(), "ticket type request failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong", nil)
	}
}

// ---- handlers ----

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	eventID, ok := eventParam(w, r)
	if !ok {
		return
	}
	rows, err := h.svc.List(r.Context(), eventID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items := make([]ticketTypeResponse, 0, len(rows))
	for _, t := range rows {
		items = append(items, newTicketTypeResponse(t))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	eventID, ok := eventParam(w, r)
	if !ok {
		return
	}
	var req createRequest
	if !decodeBody(w, r, &req) {
		return
	}

	tt, err := h.svc.Create(r.Context(), eventID, TicketTypeInput{
		Name: req.Name, PriceKobo: req.PriceKobo, Currency: req.Currency,
		TotalQuantity: req.TotalQuantity, MaxPerOrder: req.MaxPerOrder,
		SalesStart: req.SalesStart, SalesEnd: req.SalesEnd,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/manage/events/"+uuidx.String(eventID)+"/ticket-types/"+uuidx.String(tt.ID))
	httpx.WriteJSON(w, http.StatusCreated, newTicketTypeResponse(tt))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	eventID, ok := eventParam(w, r)
	if !ok {
		return
	}
	id, ok := ticketTypeParam(w, r)
	if !ok {
		return
	}
	tt, err := h.svc.Get(r.Context(), eventID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, newTicketTypeResponse(tt))
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	eventID, ok := eventParam(w, r)
	if !ok {
		return
	}
	id, ok := ticketTypeParam(w, r)
	if !ok {
		return
	}
	var req patchRequest
	if !decodeBody(w, r, &req) {
		return
	}

	tt, err := h.svc.Update(r.Context(), eventID, id, TicketTypePatch{
		Name: req.Name, PriceKobo: req.PriceKobo, TotalQuantity: req.TotalQuantity,
		MaxPerOrder: req.MaxPerOrder, SalesStart: req.SalesStart, SalesEnd: req.SalesEnd,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, newTicketTypeResponse(tt))
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	eventID, ok := eventParam(w, r)
	if !ok {
		return
	}
	id, ok := ticketTypeParam(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), eventID, id); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listPublic(w http.ResponseWriter, r *http.Request) {
	eventID, ok := eventParam(w, r)
	if !ok {
		return
	}
	rows, err := h.svc.ListPublic(r.Context(), eventID)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	now := h.svc.now()
	items := make([]publicTicketTypeResponse, 0, len(rows))
	for _, t := range rows {
		items = append(items, publicTicketTypeResponse{
			ID:          uuidx.String(t.ID),
			Name:        t.Name,
			PriceKobo:   t.PriceKobo,
			Currency:    t.Currency,
			MaxPerOrder: t.MaxPerOrder,
			SalesStart:  t.SalesStart.Time.UTC(),
			SalesEnd:    t.SalesEnd.Time.UTC(),
			SalesStatus: SalesStatus(t, now),
		})
	}
	w.Header().Set("Cache-Control", publicCacheControl)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}