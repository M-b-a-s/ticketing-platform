// Package health exposes liveness (and, from Stage 1, readiness) endpoints.
package health

import "net/http"

// Liveness reports that the process is up. It must not touch dependencies:
// a database outage should make /readyz fail, not restart the process.
func Liveness() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
}
