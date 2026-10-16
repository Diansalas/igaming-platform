//go:build integration

package httpserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// SR10C-8: completion and release act on the reservation's own row id. A late completion or release of an OLD
// reservation (whose row was swept and whose key a newer request reserved) changes and deletes nothing of the newer one.
func TestA10c_SR10C8_LateCompleteOrReleaseNeverTouchesANewerReservation(t *testing.T) {
	a := newCGAPI(t)
	tenantID := a.tenant()
	actor := uuid.New()
	deps := Deps{DB: a.pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, status := range []int{http.StatusCreated, http.StatusConflict} { // completion path, release path
		key := "late-" + strconv.Itoa(status)
		var newerID uuid.UUID
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// While the old request is "running": its row is swept and a newer request reserves the same key.
			if err := a.pool.WithTenant(r.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `DELETE FROM staff_idempotency_keys WHERE idempotency_key = $1`, key); err != nil {
					return err
				}
				return tx.QueryRow(ctx, `INSERT INTO staff_idempotency_keys (tenant_id, actor_id, idempotency_key, route, request_hash, state)
					VALUES ($1, $2, $3, 'POST /newer', $4, 'in_progress') RETURNING id`, tenantID, actor, key, reqHash("POST", "/newer", nil)).Scan(&newerID)
			}); err != nil {
				t.Errorf("simulate the sweep and the newer reservation: %v", err)
			}
			w.WriteHeader(status)
		})
		req := httptest.NewRequest("POST", "/v1/admin/tenants/"+tenantID.String()+"/x", strings.NewReader(`{}`))
		req.SetPathValue("tenantID", tenantID.String())
		req.Header.Set("Idempotency-Key", key)
		req = req.WithContext(tenant.WithContext(req.Context(), tenant.Context{Subject: actor.String(), TenantID: uuid.Nil, Role: string(auth.RolePlatformAdmin)}))
		rec := httptest.NewRecorder()
		staffIdempotent(deps, inner).ServeHTTP(rec, req)
		if rec.Code != status {
			t.Fatalf("status %d: wrapper answered %d", status, rec.Code)
		}
		var state string
		var st *int
		var n int
		if err := a.pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM staff_idempotency_keys WHERE id = $1`, newerID).Scan(&n); err != nil || n == 0 {
				return err
			}
			return tx.QueryRow(ctx, `SELECT state, response_status FROM staff_idempotency_keys WHERE id = $1`, newerID).Scan(&state, &st)
		}); err != nil {
			t.Fatal(err)
		}
		if n != 1 || state != "in_progress" || st != nil {
			t.Errorf("status %d: the newer reservation was touched by the old request (rows %d, state %q, status %v)", status, n, state, st)
		}
	}
}
