package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// HealthChecker is satisfied by internal/db.Pool; kept as an interface
// here so this package doesn't need to import db directly.
type HealthChecker interface {
	HealthCheck(ctx context.Context) error
}

// livezHandler answers "is the process up at all" - it never touches the
// database, so a database outage doesn't take liveness down (which would
// cause an orchestrator to kill and restart a perfectly healthy process
// for a dependency problem it can't fix by restarting).
func livezHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// readyzHandler answers "can this instance actually serve traffic" -
// checked against the database, since every foundation endpoint depends
// on it. Used by orchestrators to gate traffic routing, not to decide
// whether to restart the process.
func readyzHandler(db HealthChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err := db.HealthCheck(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "unavailable", "reason": "database unreachable"})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}
