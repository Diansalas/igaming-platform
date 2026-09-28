//go:build integration

package alerting

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// fakeInstantClock never actually sleeps (plan rule T-1: "no sleep-and-
// hope") - it satisfies tests that only care that a bounded number of
// retries happen, not how long they take in wall-clock time.
type fakeInstantClock struct{}

func (fakeInstantClock) Now() time.Time                             { return time.Now() }
func (fakeInstantClock) Sleep(ctx context.Context, _ time.Duration) {}

// TestInTx_ConcurrentRaisersDedupToOneAlert is LF test 9 (partial, without
// the ledger-transaction assertions that only apply once I-wire's real
// call sites exist): N concurrent raisers of one dedup key produce
// exactly one alerts row and N alert_occurrences rows. Run with -race.
func TestInTx_ConcurrentRaisersDedupToOneAlert(t *testing.T) {
	pool := testPool(t)
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	discriminator := "switch:" + uuid.NewString()

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			runner := NewTenantRunner(pool, tenantA)
			pending, err := InTx(context.Background(), runner, func(ctx context.Context, tx pgx.Tx) error {
				return RaiseGuarded(ctx, tx, Alert{
					Kind:            KindPaymentKillSwitchEngaged,
					SubjectTenantID: tenantA,
					Discriminator:   discriminator,
					Attributes:      map[string]AttrValue{"reason_code": "concurrent-test"},
				})
			})
			if err != nil {
				errs[i] = err
				return
			}
			pending.Flush(context.Background())
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent raise failed: %v", err)
		}
	}

	var alertCount, occCount int
	if err := pool.WithTenant(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE discriminator = $1`, discriminator).Scan(&alertCount); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM alert_occurrences o JOIN alerts a ON a.id = o.alert_id WHERE a.discriminator = $1`, discriminator).Scan(&occCount)
	}); err != nil {
		t.Fatalf("count: %v", err)
	}
	if alertCount != 1 {
		t.Fatalf("expected exactly 1 alert, got %d", alertCount)
	}
	if occCount != n {
		t.Fatalf("expected exactly %d occurrences, got %d", n, occCount)
	}
}

// TestInTx_RolledBackTransactionNeverFlushes is LF test 4 / AL-7: a
// swallowed in-tx alert whose OWN transaction rolls back must never be
// re-raised. InTx returns (nil, err) on a non-nil fn error, and Flush on
// a nil Pending is a safe no-op.
func TestInTx_RolledBackTransactionNeverFlushes(t *testing.T) {
	pool := testPool(t)
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	discriminator := "switch:" + uuid.NewString()

	runner := NewTenantRunner(pool, tenantA)
	sentinel := context.Canceled // any non-nil error the business fn returns
	pending, err := InTx(context.Background(), runner, func(ctx context.Context, tx pgx.Tx) error {
		if rerr := RaiseGuarded(ctx, tx, Alert{
			Kind:            KindPaymentKillSwitchEngaged,
			SubjectTenantID: tenantA,
			Discriminator:   discriminator,
			Attributes:      map[string]AttrValue{"reason_code": "rollback-test"},
		}); rerr != nil {
			t.Fatalf("RaiseGuarded must never propagate: %v", rerr)
		}
		return sentinel
	})
	if err == nil {
		t.Fatal("expected InTx to return the business error")
	}
	if pending != nil {
		t.Fatal("expected InTx to return a nil Pending for a rolled-back transaction")
	}
	pending.Flush(context.Background()) // must be a safe no-op on nil

	var count int
	if err := pool.WithTenant(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE discriminator = $1`, discriminator).Scan(&count)
	}); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no alert for a rolled-back transaction, got %d", count)
	}
}

// TestInTx_ValidationFailureInRolledBackTransactionDiscarded is the
// security confirmation's note 1: a Go-validation failure is routed
// through the same per-transaction Pending, so a rolled-back transaction
// discards it too - it must never surface as alerting.raise_failed later.
func TestInTx_ValidationFailureInRolledBackTransactionDiscarded(t *testing.T) {
	pool := testPool(t)
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)

	sentinel := context.Canceled
	pending, err := InTx(context.Background(), NewTenantRunner(pool, tenantA), func(ctx context.Context, tx pgx.Tx) error {
		if rerr := RaiseGuarded(ctx, tx, Alert{
			Kind:            KindPaymentKillSwitchEngaged,
			SubjectTenantID: tenantA,
			Discriminator:   "switch:bad",
			Attributes:      map[string]AttrValue{"not_allowed": "x"}, // invalid: Go validation fails
		}); rerr != nil {
			t.Fatalf("RaiseGuarded must never propagate: %v", rerr)
		}
		return sentinel
	})
	if err == nil || pending != nil {
		t.Fatal("expected InTx to discard the collector for a rolled-back transaction")
	}
	pending.Flush(context.Background())

	// Scoped to THIS test's own original Kind (rather than a global
	// count) so this assertion stays valid even if other tests or prior
	// runs against the same database have their own raise_failed rows.
	var count int
	if err := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = $1 AND attributes->>'kind' = $2`,
			string(KindAlertingRaiseFailed), string(KindPaymentKillSwitchEngaged)).Scan(&count)
	}); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no raise_failed alert for a rolled-back transaction's validation failure, got %d", count)
	}
}

// TestRaiseGuarded_PersistentSwallowedFailure_FallsBackToRaiseFailed
// substitutes for LF test 2's literal "persistent P0001" with a
// deterministic, reproducible persistent failure of the SAME swallow
// class family: a TENANT session attempting to raise a META-KIND directly
// (never a legitimate business call site, but exactly what a
// database-layer defence-in-depth check must survive) fails the
// alerts_subject_tenant_raise policy's WITH CHECK identically on every
// attempt (SQLSTATE 42501, insufficient privilege - exactly in the
// swallow allowlist), because that policy also requires
// alert_kinds.in_tx_raisable_by_tenant, which is false for every
// meta-Kind. checkSubjectMatchesScope does NOT short-circuit this (a
// meta-Kind never RequiresSubject, so there is nothing to check), so the
// detached retry genuinely reaches SQL on every attempt, exhausts, and
// falls back - exactly as a persistent P0001 would. (Injecting a literal,
// deterministic P0001 from inside a trigger would require a test-only
// fault-injection hook in migration 0110 itself, which was not added -
// see the final report's noted test gaps.)
func TestRaiseGuarded_PersistentSwallowedFailure_FallsBackToRaiseFailed(t *testing.T) {
	// scratchPool (not the shared testPool): this alert's dedup key is
	// stable (kind:alerting.unrouted), so a shared database would let a
	// PRIOR successful run's row satisfy this test's count check even if
	// THIS run's fallback never fired - a genuine mutation-testing false
	// negative found and fixed during I-core's own mutant-kill exercise.
	pool := scratchPool(t, "araisefail")
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)

	pending, err := InTx(context.Background(), NewTenantRunner(pool, tenantA), func(ctx context.Context, tx pgx.Tx) error {
		return RaiseGuarded(ctx, tx, Alert{
			Kind:          KindAlertingUnrouted,
			Discriminator: "severity:p1",
			Attributes:    map[string]AttrValue{},
		})
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}

	// T-1: injectable clock, no sleep-and-hope - the 3 detached attempts
	// do not actually wait.
	ctx := WithClockContext(context.Background(), fakeInstantClock{})
	pending.Flush(ctx)

	var count int
	var attrsJSON []byte
	err = pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = $1`, string(KindAlertingRaiseFailed)).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			return nil
		}
		return tx.QueryRow(ctx, `SELECT attributes FROM alerts WHERE kind = $1 ORDER BY created_at DESC LIMIT 1`, string(KindAlertingRaiseFailed)).Scan(&attrsJSON)
	})
	if err != nil {
		t.Fatalf("read raise_failed: %v", err)
	}
	if count == 0 {
		t.Fatal("expected a terminal alerting.raise_failed alert to persist")
	}

	var attrs map[string]string
	if err := json.Unmarshal(attrsJSON, &attrs); err != nil {
		t.Fatalf("unmarshal raise_failed attributes: %v", err)
	}
	if attrs["kind"] != string(KindAlertingUnrouted) {
		t.Fatalf("expected attributes.kind=%q, got %q", KindAlertingUnrouted, attrs["kind"])
	}
	if attrs["sqlstate_class"] != "42" {
		t.Fatalf("expected attributes.sqlstate_class=42 (insufficient_privilege), got %q", attrs["sqlstate_class"])
	}
}
