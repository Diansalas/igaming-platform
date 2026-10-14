//go:build integration

package httpserver

// ADR 0119 S5 + S6 HARD GATE over HTTP: POST /v1/admin/withdrawals/{id}/submit passes the verified token's role and
// `sv` into ClaimForDispatch, whose in-transaction re-assertion refuses a submitter suspended after the door's
// liveness check; the handler answers 401 session_revoked and nothing is written. The request is parked on the
// withdrawal row (held FOR UPDATE by a blocker) after the liveness check, the suspension commits, then the blocker
// lets go. (Both orders against the real ClaimForDispatch: internal/payments/adr0119_submit_reassert_integration_test.go.)

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
)

func TestSubmitWithdrawalHandler_SubmitterSuspendedInFlight_401NothingWritten(t *testing.T) {
	f := setupStage3CResolutionFixture(t, 5000)
	ctx := context.Background()
	var staffID uuid.UUID
	if err := f.pool.WithTenant(ctx, f.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM staff_users WHERE tenant_id = $1 AND role = 'finance'`, f.tenant.ID).Scan(&staffID)
	}); err != nil {
		t.Fatal(err)
	}

	locked := make(chan error, 1)
	release := make(chan struct{})
	blockerDone := make(chan struct{})
	go func() {
		defer close(blockerDone)
		_ = f.pool.WithTenant(ctx, f.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
			var one int
			err := tx.QueryRow(ctx, `SELECT 1 FROM withdrawal_requests WHERE id = $1 FOR UPDATE`, f.withdrawalID).Scan(&one)
			locked <- err
			if err != nil {
				return err
			}
			<-release
			return errors.New("blocker: roll back")
		})
	}()
	released := false
	unblock := func() {
		if !released {
			released = true
			close(release)
			<-blockerDone
		}
	}
	t.Cleanup(unblock)
	if err := <-locked; err != nil {
		t.Fatalf("blocker: %v", err)
	}

	type result struct {
		status int
		code   apierror.Code
	}
	done := make(chan result, 1)
	go func() {
		resp := postJSON(t, f.srv, "/v1/admin/withdrawals/"+f.withdrawalID+"/submit", f.financeToken.AccessToken, map[string]string{})
		defer func() { _ = resp.Body.Close() }()
		var e apierror.Error
		decodeBody(t, resp, &e)
		done <- result{resp.StatusCode, e.Code}
	}()
	// Wait until the submit is parked on the withdrawal row (it has passed the door's liveness check).
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := f.pool.Raw().QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the submit never parked on the withdrawal row")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := f.pool.WithTenant(ctx, f.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, staffID)
		return err
	}); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	unblock()
	r := <-done
	if r.status != http.StatusUnauthorized || r.code != apierror.CodeSessionRevoked {
		t.Fatalf("want 401 session_revoked, got %d %s", r.status, r.code)
	}
	if countRows(t, f.pool, f.tenant.ID, `SELECT count(*) FROM withdrawal_requests WHERE id = $1 AND state = 'approved'`, f.withdrawalID) != 1 ||
		countRows(t, f.pool, f.tenant.ID, `SELECT count(*) FROM payment_attempts WHERE withdrawal_request_id = $1`, f.withdrawalID) != 0 {
		t.Fatal("a refused submit must leave the request approved with no attempt")
	}
	if f.mock.AttemptCount() != 0 {
		t.Fatal("the provider must not be called")
	}
}
