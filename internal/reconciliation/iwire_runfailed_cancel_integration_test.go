//go:build integration

package reconciliation

import (
	"context"
	"errors"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

// LF F3: a run that failed only because the sweep's own context was cancelled
// (graceful shutdown mid-sweep) must NOT page reconciliation.run_failed (it
// would raise a false P1 per remaining tenant and stream at every shutdown).
// Any other failure still raises: a live-context failure (even one carrying
// context.Canceled from elsewhere), a deadline expiry, a database error.
func TestIWire_RunFailed_SuppressedOnlyForShutdownCancellation(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		err   error
		raise bool
	}{
		{"shutdown_cancel_suppressed", cancelled, context.Canceled, false},
		{"wrapped_cancel_suppressed", cancelled, errors.Join(errors.New("tx"), context.Canceled), false},
		{"deadline_with_cancelled_ctx_raises", cancelled, context.DeadlineExceeded, true},
		{"live_ctx_with_cancel_error_raises", context.Background(), context.Canceled, true},
		{"live_ctx_generic_error_raises", context.Background(), errors.New("boom"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testPool(t)
			f := seedFixture(t, pool)
			raiseRunFailed(tc.ctx, pool, f.tenantID, string(StreamLedgerVsProjection), "", "run", tc.err)
			got := alertinject.Find(alertinject.ForSubject(t, pool, f.tenantID), string(alerting.KindReconciliationRunFailed))
			if tc.raise && len(got) != 1 {
				t.Fatalf("expected one run_failed alert, got %+v", got)
			}
			if !tc.raise && len(got) != 0 {
				t.Fatalf("a shutdown cancellation must raise nothing, got %+v", got)
			}
		})
	}
}
