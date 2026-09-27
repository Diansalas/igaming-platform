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
//
// ADR 0097 §7 (PRH-I4): also not-ready until the webhook tenant directory
// has completed its first successful load - a webhook route that ran
// before that would have to key every request "_unknown", which this ADR
// treats as a fail-closed startup state, not a normal one. admission is
// always "ready" on this axis when the admission layer itself is
// disabled (WebhookAdmissionRuntime.DirectoryReady()'s own doc comment).
func readyzHandler(db HealthChecker, admission WebhookAdmissionRuntime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err := db.HealthCheck(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "unavailable", "reason": "database unreachable"})
			return
		}
		if !admission.DirectoryReady() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "unavailable", "reason": "webhook tenant directory not loaded"})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}
