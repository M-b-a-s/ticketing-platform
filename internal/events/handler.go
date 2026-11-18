package events

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"
	"context"
	"fmt"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/auth"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/httpx"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/pagination"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/uuidx"
)

const maxBodyBytes = 32 << 10 // a 5000-character description can be ~20 KB of UTF-8

type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

// ManageRoutes must be mounted behind auth.Authenticate, under /v1/manage.
func (h *Handler) ManageRoutes(r chi.Router) {
	creators := auth.RequireRole(auth.RoleOrganizer, auth.RolePlatformAdmin)
	r.With(creators).Post("/events", h.create)
	r.Get("/events", h.listManaged)

	// Per-event routes: the guards resolve the caller's relationship to THIS event.
	r.Route("/events/{eventID}", func(r chi.Router) {
		view := auth.RequireEventAccess(h.svc.store, auth.AccessGate, "eventID")
		manage := auth.RequireEventAccess(h.svc.store, auth.AccessManage, "eventID")

		r.With(view).Get("/", h.get)
		r.With(manage).Patch("/", h.update)
		r.With(manage).Post("/publish", h.publish)
		r.With(manage).Post("/cancel", h.cancel)
	})
}

// ---- JSON shapes ----

type eventRequest struct {
	Title       string    `json:"title"`
	Description string    `json:"description"`
	VenueName   string    `json:"venue_name"`
	StartsAt    time.Time `json:"starts_at"`
	EndsAt      time.Time `json:"ends_at"`
}

type eventResponse struct {
	ID          string    `json:"id"`
	OrganizerID string    `json:"organizer_id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	VenueName   string    `json:"venue_name"`
	StartsAt    time.Time `json:"starts_at"`
	EndsAt      time.Time `json:"ends_at"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func newEventResponse(e db.Event) eventResponse {
	return eventResponse{
		ID:          uuidx.String(e.ID),
		OrganizerID: uuidx.String(e.OrganizerID),
		Title:       e.Title,
		Description: e.Description.String,
		VenueName:   e.VenueName,
		StartsAt:    e.StartsAt.Time.UTC(),
		EndsAt:      e.EndsAt.Time.UTC(),
		Status:      e.Status,
		CreatedAt:   e.CreatedAt.Time.UTC(),
		UpdatedAt:   e.UpdatedAt.Time.UTC(),
	}
}

type managedEventResponse struct {
	eventResponse
	MyRole string `json:"my_role"`
}

type managedListResponse struct {
	Items      []managedEventResponse `json:"items"`
	NextCursor *string                `json:"next_cursor"`
}

// ---- helpers ----
// decodeBody decodes a JSON request body into dst, returning false and writing a 400 response if the body is invalid.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields() // a client-supplied organizer_id or status is a 400, not silently ignored
	if err := dec.Decode(dst); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_json",
			"request body is not valid JSON, has unknown fields, or a timestamp is not RFC 3339", nil)
		return false
	}
	return true
}

// caller returns the authenticated caller's user UUID, or writes a 401 response and returns false.
func caller(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	id, ok := auth.IdentityFrom(r.Context())
	if ok {
		if u, err := id.UserUUID(); err == nil {
			return u, true
		}
	}
	httpx.WriteError(w, http.StatusUnauthorized, "missing_token", "authentication required", nil)
	return pgtype.UUID{}, false
}

// ---- handlers ----

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	user, ok := caller(w, r)
	if !ok {
		return
	}
	var req eventRequest
	if !decodeBody(w, r, &req) {
		return
	}

	ev, err := h.svc.Create(r.Context(), user, EventInput{
		Title: req.Title, Description: req.Description, VenueName: req.VenueName,
		StartsAt: req.StartsAt, EndsAt: req.EndsAt,
	})

	var ve *ValidationError
	switch {
	case errors.As(err, &ve):
		httpx.WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "one or more fields are invalid", ve.Fields)
	case err != nil:
		h.log.ErrorContext(r.Context(), "create event failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong", nil)
	default:
		w.Header().Set("Location", "/v1/manage/events/"+uuidx.String(ev.ID))
		httpx.WriteJSON(w, http.StatusCreated, newEventResponse(ev))
	}
}

func (h *Handler) listManaged(w http.ResponseWriter, r *http.Request) {
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

	page, err := h.svc.ListManaged(r.Context(), user, limit, after)
	switch {
	case errors.Is(err, pagination.ErrInvalidCursor):
		httpx.WriteError(w, http.StatusBadRequest, "invalid_query", "cursor is invalid", nil)
		return
	case err != nil:
		h.log.ErrorContext(r.Context(), "list managed events failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong", nil)
		return
	}

	resp := managedListResponse{Items: make([]managedEventResponse, 0, len(page.Items)), NextCursor: page.NextCursor}
	for _, it := range page.Items {
		resp.Items = append(resp.Items, managedEventResponse{eventResponse: newEventResponse(it.Event), MyRole: it.MyRole})
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

type patchRequest struct {
	Title       *string    `json:"title"`
	Description *string    `json:"description"`
	VenueName   *string    `json:"venue_name"`
	StartsAt    *time.Time `json:"starts_at"`
	EndsAt      *time.Time `json:"ends_at"`
}

type eventDetailResponse struct {
	eventResponse
	Access string `json:"access"` // what the caller may do here: "manage" or "gate"
}

func accessName(a auth.EventAccess) string {
	switch a {
	case auth.AccessManage:
		return "manage"
	case auth.AccessGate:
		return "gate"
	}
	return "none"
}

func eventNotFound(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusNotFound, "event_not_found", "event not found", nil)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := uuidx.Parse(chi.URLParam(r, "eventID"))
	if err != nil {
		eventNotFound(w)
		return
	}

	ev, err := h.svc.Get(r.Context(), id)
	switch {
	case errors.Is(err, ErrNotFound):
		eventNotFound(w)
	case err != nil:
		h.log.ErrorContext(r.Context(), "get event failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong", nil)
	default:
		access, _ := auth.EventAccessFrom(r.Context())
		httpx.WriteJSON(w, http.StatusOK, eventDetailResponse{eventResponse: newEventResponse(ev), Access: accessName(access)})
	}
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	id, err := uuidx.Parse(chi.URLParam(r, "eventID"))
	if err != nil {
		eventNotFound(w)
		return
	}
	var req patchRequest
	if !decodeBody(w, r, &req) {
		return
	}

	ev, err := h.svc.Update(r.Context(), id, EventPatch{
		Title: req.Title, Description: req.Description, VenueName: req.VenueName,
		StartsAt: req.StartsAt, EndsAt: req.EndsAt,
	})

	var ve *ValidationError
	switch {
	case errors.As(err, &ve):
		httpx.WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "one or more fields are invalid", ve.Fields)
	case errors.Is(err, ErrNotFound):
		eventNotFound(w)
	case errors.Is(err, ErrNotEditable):
		httpx.WriteError(w, http.StatusConflict, "event_not_editable", "cancelled or completed events cannot be edited", nil)
	case err != nil:
		h.log.ErrorContext(r.Context(), "update event failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong", nil)
	default:
		httpx.WriteJSON(w, http.StatusOK, newEventResponse(ev))
	}
}

func (h *Handler) publish(w http.ResponseWriter, r *http.Request) { h.transition(w, r, h.svc.Publish) }
func (h *Handler) cancel(w http.ResponseWriter, r *http.Request)  { h.transition(w, r, h.svc.Cancel) }

func (h *Handler) transition(w http.ResponseWriter, r *http.Request, do func(context.Context, pgtype.UUID) (db.Event, error)) {
	id, err := uuidx.Parse(chi.URLParam(r, "eventID"))
	if err != nil {
		eventNotFound(w)
		return
	}

	ev, err := do(r.Context(), id)

	var ve *ValidationError
	var te *TransitionError
	switch {
	case errors.As(err, &ve):
		httpx.WriteError(w, http.StatusUnprocessableEntity, "requirements_not_met", "the event does not meet the requirements for this action", ve.Fields)
	case errors.As(err, &te):
		httpx.WriteError(w, http.StatusConflict, "invalid_status_transition",
			fmt.Sprintf("an event that is %s cannot become %s", te.From, te.To), nil)
	case errors.Is(err, ErrConflict):
		httpx.WriteError(w, http.StatusConflict, "event_changed", "the event changed while your request was running, reload it and try again", nil)
	case errors.Is(err, ErrNotFound):
		eventNotFound(w)
	case err != nil:
		h.log.ErrorContext(r.Context(), "event status change failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong", nil)
	default:
		httpx.WriteJSON(w, http.StatusOK, newEventResponse(ev))
	}
}