package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/httpx"
)

// Staff roles must match the CHECK constraint on event_staff.role.
const (
	StaffEventAdmin    = "event_admin"
	StaffGateAttendant = "gate_attendant"
)

// EventAccess is what a caller may do on one specific event. Higher includes lower.
type EventAccess int

const (
	AccessNone   EventAccess = iota
	AccessGate               // check attendees in
	AccessManage             // edit the event, ticket types, reports, staff
)

// resolveEventAccess is the single place the access rules live.
func resolveEventAccess(id Identity, isOwner bool, staffRole string) EventAccess {
	if id.Role == RolePlatformAdmin {
		return AccessManage
	}

	access := AccessNone
	if isOwner && id.Role == RoleOrganizer {
		access = AccessManage
	}

	switch staffRole {
	case StaffEventAdmin:
		access = max(access, AccessManage)
	case StaffGateAttendant:
		access = max(access, AccessGate)
	}
	return access
}

type EventAccessStore interface {
	GetEventAccess(ctx context.Context, arg db.GetEventAccessParams) (db.GetEventAccessRow, error)
}

type eventAccessKey struct{}

// EventAccessFrom returns the access level RequireEventAccess granted.
func EventAccessFrom(ctx context.Context) (EventAccess, bool) {
	a, ok := ctx.Value(eventAccessKey{}).(EventAccess)
	return a, ok
}

// RequireEventAccess guards routes that act on one event. It must run after
// Authenticate, and inside a route that contains the {urlParam} pattern, e.g.
//
//	r.With(auth.Authenticate(tm), auth.RequireEventAccess(q, auth.AccessManage, "eventID")).
//		Put("/events/{eventID}", ...)
//
// Responses:
//   - 404 if the event doesn't exist, the ID is malformed, or the caller has NO
//     relationship to it (so drafts and other people's events are not revealed)
//   - 403 if the caller has some access, but not enough
func RequireEventAccess(store EventAccessStore, need EventAccess, urlParam string) func(http.Handler) http.Handler {
	if need == AccessNone {
		panic("auth: RequireEventAccess needs a real access level")
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := IdentityFrom(r.Context())
			if !ok {
				unauthorized(w, "missing_token", "authentication required")
				return
			}
			userID, err := id.UserUUID()
			if err != nil {
				unauthorized(w, "invalid_token", "access token is invalid or expired")
				return
			}

			var eventID pgtype.UUID
			if err := eventID.Scan(chi.URLParam(r, urlParam)); err != nil {
				eventNotFound(w)
				return
			}

			row, err := store.GetEventAccess(r.Context(), db.GetEventAccessParams{UserID: userID, EventID: eventID})
			if errors.Is(err, pgx.ErrNoRows) {
				eventNotFound(w)
				return
			}
			if err != nil {
				slog.ErrorContext(r.Context(), "event access lookup failed", "error", err)
				httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong", nil)
				return
			}

			access := resolveEventAccess(id, row.IsOwner, row.StaffRole)
			switch {
			case access == AccessNone:
				eventNotFound(w)
				return
			case access < need:
				httpx.WriteError(w, http.StatusForbidden, "forbidden", "you do not have permission to perform this action on this event", nil)
				return
			}

			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), eventAccessKey{}, access)))
		})
	}
}

func eventNotFound(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusNotFound, "event_not_found", "event not found", nil)
}