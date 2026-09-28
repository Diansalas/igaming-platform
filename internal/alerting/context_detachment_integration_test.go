//go:build integration

package alerting

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestRaiseDetached_CancelledCallerContextStillWrites is security IC-5 /
// code review F-2 / LF C-3: RaiseDetached must detach ITS OWN context, so
// a caller context that is ALREADY cancelled before RaiseDetached is even
// called still writes the alert.
func TestRaiseDetached_CancelledCallerContextStillWrites(t *testing.T) {
	pool := testPool(t)
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	discriminator := "switch:" + uuid.NewString()

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the call

	err := RaiseDetached(cancelledCtx, NewTenantRunner(pool, tenantA), Alert{
		Kind:            KindPaymentKillSwitchEngaged,
		SubjectTenantID: tenantA,
		Discriminator:   discriminator,
		Attributes:      map[string]AttrValue{"reason_code": "cancelled-ctx-test"},
	})
	if err != nil {
		t.Fatalf("expected RaiseDetached to succeed despite a cancelled caller context, got: %v", err)
	}

	var count int
	if qerr := pool.WithTenant(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE discriminator = $1`, discriminator).Scan(&count)
	}); qerr != nil {
		t.Fatalf("count: %v", qerr)
	}
	if count != 1 {
		t.Fatalf("expected the alert to persist despite the cancelled caller context, got %d row(s)", count)
	}
}

// TestRaiseDetached_CancelledCallerContext_ExhaustionStillFallsBack is the
// same guarantee on the exhaustion path: even with an already-cancelled
// caller context, a persistently-refused raise still reaches the
// terminal alerting.raise_failed fallback (which itself must use the
// detached context, not the cancelled one).
func TestRaiseDetached_CancelledCallerContext_ExhaustionStillFallsBack(t *testing.T) {
	pool := scratchPool(t, "actxdetach")
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	// A tenant session raising a meta-Kind directly is persistently
	// refused by RLS (42501) on every attempt - the same deterministic
	// exhaustion path used elsewhere in this package's tests.
	cctx := WithClockContext(cancelledCtx, fakeInstantClock{})
	err := RaiseDetached(cctx, NewTenantRunner(pool, tenantA), Alert{
		Kind:          KindAlertingUnrouted,
		Discriminator: "severity:p1",
		Attributes:    map[string]AttrValue{},
	})
	if err == nil {
		t.Fatal("expected RaiseDetached to return the exhausted error")
	}

	var count int
	if qerr := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = $1`, string(KindAlertingRaiseFailed)).Scan(&count)
	}); qerr != nil {
		t.Fatalf("count: %v", qerr)
	}
	if count != 1 {
		t.Fatalf("expected the terminal alerting.raise_failed fallback to persist despite the cancelled caller context, got %d row(s)", count)
	}
}

// TestPendingFlush_CancelledCallerContextStillFlushes is the same
// guarantee through the normal Pending.Flush path (an HTTP handler
// calling Flush after its own request context may already be near
// cancellation, or cancelled outright if the client disconnected right
// after the response was written). The in-tx swallow (a tenant session
// raising a meta-Kind directly) is persistently refused on retry too, so
// this exercises Flush -> RaiseDetached -> exhaustion -> the terminal
// fallback, all under a cancelled caller context.
func TestPendingFlush_CancelledCallerContextStillFlushes(t *testing.T) {
	pool := scratchPool(t, "aflushcancel")
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)

	pending, err := InTx(context.Background(), NewTenantRunner(pool, tenantA), func(ctx context.Context, tx pgx.Tx) error {
		return RaiseGuarded(ctx, tx, Alert{
			Kind:          KindAlertingUnrouted,
			Discriminator: "severity:p2",
			Attributes:    map[string]AttrValue{},
		})
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	pending.Flush(WithClockContext(cancelledCtx, fakeInstantClock{}))

	var count int
	if qerr := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = $1`, string(KindAlertingRaiseFailed)).Scan(&count)
	}); qerr != nil {
		t.Fatalf("count: %v", qerr)
	}
	if count != 1 {
		t.Fatalf("expected Flush to still reach the terminal fallback despite a cancelled caller context, got %d row(s)", count)
	}
}
