//go:build integration

// PAY-PARK-BIND-RACE-1 (A11; S-6 of the B3 review): parkDepositAttempt's reference bind (the
// sync_amount_mismatch park) used to be a bare UPDATE with no savepoint and no audit. A concurrent,
// uncommitted bind of the same reference by another attempt surfaced as a unique violation that rolled
// the whole phase-C transaction back (an error loop on a path that must never loop). It now goes through
// bindParkReference: a lost race leaves the park reference-less (unbound), the transaction commits, and
// the outcome is audited with the hash prefix only.
package payments

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Deterministic race, the park-side twin of TestB3_BindParkReference_ConcurrentUncommittedBind_LoserAbsorbed:
// racer A binds R (uncommitted) and holds; racer B parks a sync_amount_mismatch carrying R, passes its
// pre-check, blocks on the per-tenant unique index and, when A commits, must NOT fail.
func TestParkBindRace_SyncMismatchPark_LostRaceIsAbsorbedAndParkStaysUnbound(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-park-race")
	a1 := e.refLessLive(t, e.f, "park-race-1")
	a2 := e.refLessLive(t, e.f, "park-race-2")
	ref := "park-race-ref-" + uuid.NewString()
	bound, release := make(chan struct{}), make(chan struct{})
	racerA := loStartRacer(t, pool, e.f.tenantID, "A", func(ctx context.Context, tx pgx.Tx) error {
		if err := bindParkReference(ctx, tx, a1, e.id, ref); err != nil {
			return err
		}
		close(bound)
		<-release
		return nil
	})
	<-bound
	racerB := loStartRacer(t, pool, e.f.tenantID, "B", func(ctx context.Context, tx pgx.Tx) error {
		intent, err := GetDepositIntentByID(ctx, tx, *a2.DepositIntentID)
		if err != nil {
			return err
		}
		if _, err := parkDepositAttempt(ctx, tx, intent, a2, e.id, EvidenceSync, TerminalReasonSyncAmountMismatch, OutcomeSucceeded, ref,
			map[string]any{"provider_amount": int64(1)}); err != nil {
			return err
		}
		var one int
		return tx.QueryRow(ctx, `SELECT 1`).Scan(&one) // the transaction must still be usable
	})
	if _, ok := loWaitBlocked(t, pool, racerB.pid, racerB.done); !ok {
		close(release)
		t.Fatalf("racer B never blocked on the uncommitted bind (err=%v)", racerB.wait())
	}
	close(release)
	if err := racerA.wait(); err != nil {
		t.Fatalf("racer A: %v", err)
	}
	if err := racerB.wait(); err != nil {
		t.Fatalf("the park must absorb the lost bind race instead of failing the transaction: %v", err)
	}
	if got := mustGetAttempt(t, pool, e.f.tenantID, a1.ID); got.ProviderReference == nil || *got.ProviderReference != ref {
		t.Fatalf("the winner lost its bind: %v", got.ProviderReference)
	}
	got := b3AssertParked(t, e, e.f, a2.ID, TerminalReasonSyncAmountMismatch, nil)
	if got.ProviderReference != nil {
		t.Fatalf("the loser must not hold the winner's reference: %q", *got.ProviderReference)
	}
	b3WantBindOutcomes(t, e, a2.ID, ref, "lost_race")
	b3NoMoney(t, e, e.f)
}

// The normal sync_amount_mismatch park still binds the reference the provider returned (the reconciliation
// matcher needs it), and the bind is now audited like every other park bind.
func TestParkBindRace_SyncMismatchPark_NormalCaseBindsAndAudits(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-park-bind")
	a := e.refLessLive(t, e.f, "park-bind-1")
	ref := "park-bind-ref-" + uuid.NewString()
	if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		intent, err := GetDepositIntentByID(ctx, tx, *a.DepositIntentID)
		if err != nil {
			return err
		}
		_, err = parkDepositAttempt(ctx, tx, intent, a, e.id, EvidenceSync, TerminalReasonSyncAmountMismatch, OutcomeSucceeded, ref, nil)
		return err
	}); err != nil {
		t.Fatalf("park: %v", err)
	}
	b3AssertParked(t, e, e.f, a.ID, TerminalReasonSyncAmountMismatch, &ref)
	b3WantBindOutcomes(t, e, a.ID, ref, "bound")
	b3NoMoney(t, e, e.f)
}
