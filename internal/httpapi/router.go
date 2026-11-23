// Package httpapi assembles the HTTP router. main and the end-to-end tests both
// build it here, so what the tests exercise is exactly what production serves.
package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/M-b-a-s/ticketing-platform/internal/auth"
	"github.com/M-b-a-s/ticketing-platform/internal/events"
	"github.com/M-b-a-s/ticketing-platform/internal/tickets"
)

type Deps struct {
	Tokens  *auth.TokenManager
	Health  http.Handler
	Auth    *auth.Handler
	Events  *events.Handler
	Tickets *tickets.Handler

	// V1Middleware wraps every /v1 request (the global rate limit). May be nil.
	V1Middleware func(http.Handler) http.Handler
}

func NewRouter(d Deps) chi.Router {
	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	r.Use(chimw.Recoverer)

	r.Method(http.MethodGet, "/healthz", d.Health) // outside /v1: probes are never throttled

	r.Route("/v1", func(r chi.Router) {
		if d.V1Middleware != nil {
			r.Use(d.V1Middleware)
		}

		// public: no login
		d.Auth.Routes(r)
		d.Events.PublicRoutes(r)
		d.Tickets.PublicRoutes(r)

		// everything under /manage requires a signed-in user; the handlers add
		// role and per-event guards on top
		r.Route("/manage", func(r chi.Router) {
			r.Use(auth.Authenticate(d.Tokens))
			d.Events.ManageRoutes(r)
			d.Tickets.ManageRoutes(r)
		})
	})
	return r
}