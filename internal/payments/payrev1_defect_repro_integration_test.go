//go:build integration

// Stage 10.1 PAY-REV-1 (ADR 0090) defect-reproduction test, in the probe
// shape from the Stage 10 ledger-finance sign-off
// (docs/plans/stage-10.1-planning-gate-proposal.md §C): unlike
// TestPayRev1_ConcurrentDistinctReferenceReversals_ExactlyOnePosts (which
// proves WHICH statement a queued racer blocks on), this test proves the
// end-to-end financial defect itself - that a distinct-reference reversal
// race can double-post - by driving TWO real reversal callbacks through
// the real receiveDepositReversalCallback path, one held open
// (uncommitted) while the other runs concurrently, then asserting the
// ledger and the wallet balance directly.
//
// This file is deliberately self-contained (its own small deposit-and-
// confirm helper, not internal/payments' shared pr1ConfirmedDeposit) so
// it can be copied alone into a disposable pre-fix worktree together with
// the ALREADY-EXISTING lockorder harness helpers
// (loBlockingPIDs/loWaitBlocked/loContains, present before Stage 10.1) to
// capture the pre-fix defect-reproduction evidence, per the Orchestrator's
// instruction.
//
// Kept in the repo permanently as a regression test - it must pass on the
// fixed code.
package payments

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// pr1DefectDeposit initiates and confirms one deposit, returning its own
// provider reference and posted ledger_transactions id. A local twin of
// pr1ConfirmedDeposit (payrev1_concurrency_integration_test.go), kept
// separate on purpose - see this file's header comment.
func pr1DefectDeposit(t *testing.T, pool *db.Pool, f orchFixture, orch *Orchestrator, provider *MockProvider, amount int64) (ref string, ledgerTxID uuid.UUID) {
	t.Helper()
	var intent DepositIntent
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = orch.InitiateDeposit(ctx, tx, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: amount, PaymentMethod: "card",
			IdempotencyKey: "payrev1-defect-dep-" + uuid.NewString(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("InitiateDeposit: %v", err)
	}
	if intent.ProviderReference == nil {
		t.Fatal("intent has no provider reference")
	}
	ref = *intent.ProviderReference
	payload := provider.CallbackPayload(CallbackEventDeposit, ref, "", OutcomeSucceeded, amount, "EUR", "", false)
	var res ReceiveCallbackResult
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-psp", payload)
		return err
	})
	if err != nil {
		t.Fatalf("confirm deposit: %v", err)
	}
	if res.LedgerTransactionID == nil {
		t.Fatal("confirmed deposit has no ledger transaction id")
	}
	return ref, *res.LedgerTransactionID
}

// TestPayRev1_DefectRepro_DistinctReferenceRaceNeverDoublePosts is the
// end-to-end financial reproduction of PAY-REV-1: reversal A runs the
// real ReceiveCallback and is held OPEN (uncommitted, still holding
// whatever locks it took) while reversal B - a DIFFERENT provider
// reference for the SAME original deposit - starts, runs its own checks,
// and (on the fixed code) blocks. A is then released to commit, and B is
// allowed to complete. On the unfixed code, B's checks ran (and passed)
// BEFORE A ever committed, so B also posts - two deposit_reversal
// transactions for one original, and player_cash driven below the
// single-reversal value (the exact defect ledger-finance reproduced
// during Stage 10.1 planning: a 1,000 deposit reaching -1,000 after two
// reversals). On the fixed code, exactly one posts.
func TestPayRev1_DefectRepro_DistinctReferenceRaceNeverDoublePosts(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider})

	const amount = int64(1_000)
	depositRef, ledgerTxID := pr1DefectDeposit(t, pool, f, orch, provider, amount)

	reversalAPayload := provider.CallbackPayload(CallbackEventDepositReversal,
		"payrev1-defect-rev-A", depositRef, OutcomeSucceeded, amount, "EUR", "", false)
	reversalBPayload := provider.CallbackPayload(CallbackEventDepositReversal,
		"payrev1-defect-rev-B", depositRef, OutcomeSucceeded, amount, "EUR", "", false)

	// Racer A: runs the real reversal callback to completion, INSIDE its
	// own transaction, then PAUSES before that transaction is allowed to
	// commit - so its effects (whatever they are: the S2 lock if the fix
	// exists, the posted ledger row either way) are held open exactly as
	// they would be for a real in-flight webhook request racing another.
	type aOutcome struct {
		res ReceiveCallbackResult
		err error
	}
	aPidCh := make(chan int, 1)
	aProceed := make(chan struct{})
	aDone := make(chan aOutcome, 1)
	go func() {
		var out aOutcome
		txErr := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var pid int
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				aPidCh <- 0
				return err
			}
			out.res, out.err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-psp", reversalAPayload)
			aPidCh <- pid
			<-aProceed
			return out.err
		})
		if out.err == nil && txErr != nil {
			// A commit-time failure (e.g. a deferred constraint) - surface
			// it exactly like any other reversal-A failure.
			out.err = txErr
		}
		aDone <- out
	}()
	aPid := <-aPidCh
	if aPid == 0 {
		t.Fatalf("reversal A never reported a backend pid: %v", (<-aDone).err)
	}

	// Racer B: a DIFFERENT provider reference for the SAME original,
	// started only after A has run its own reversal (uncommitted).
	type bOutcome struct {
		res ReceiveCallbackResult
		err error
	}
	bPidCh := make(chan int, 1)
	bDone := make(chan bOutcome, 1)
	bFinished := make(chan struct{})
	go func() {
		defer close(bFinished)
		var out bOutcome
		_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var pid int
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				bPidCh <- 0
				return err
			}
			bPidCh <- pid
			out.res, out.err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-psp", reversalBPayload)
			return out.err
		})
		bDone <- out
	}()
	bPid := <-bPidCh
	if bPid == 0 {
		t.Fatal("reversal B never reported a backend pid")
	}

	// On the fixed code, B must queue behind A (either at S2's FOR UPDATE,
	// or - if B raced ahead of the lock somehow - at ledger.Post's own L3
	// pre-lock, since both reversals touch the identical two projection
	// rows). loWaitBlocked/loBlockingPIDs/loContains are the SAME harness
	// helpers TestLockOrder_ConcurrentDepositAndDepositReversal_NoDeadlock
	// already uses - present before Stage 10.1, not new.
	//
	// On the UNFIXED code, B may run to completion without ever blocking
	// on A at all (this is the exact shape of the defect: nothing
	// serializes the two callbacks), so a timeout here is tolerated -
	// the invariant assertions below are what actually detect the
	// double-post, not this wait.
	_, _ = loWaitBlocked(t, pool, bPid, bFinished)

	// Release A: let its transaction commit now.
	close(aProceed)
	aOut := <-aDone
	if aOut.err != nil {
		t.Fatalf("reversal A (the genuine, first reversal) must succeed: %v", aOut.err)
	}

	bOut := <-bDone

	// The defect-reproduction assertions - these must hold on the FIXED
	// code regardless of exactly which error B surfaces (S4's
	// ErrDepositAlreadyReversed, or the migration-0092 backstop mapped to
	// the same sentinel): at most one deposit_reversal transaction for
	// this original, and player_cash never below the single-reversal
	// value (0, since amount was fully deposited then reversed once).
	var reversalCount int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND reverses_transaction_id = $2 AND transaction_type = 'deposit_reversal'`,
			f.tenantID, ledgerTxID).Scan(&reversalCount)
	}); err != nil {
		t.Fatalf("count reversals: %v", err)
	}
	if reversalCount != 1 {
		t.Fatalf("PAY-REV-1 defect reproduced: expected exactly 1 deposit_reversal transaction for this original, got %d "+
			"(reversal A err=%v, reversal B err=%v)", reversalCount, aOut.err, bOut.err)
	}
	if got := cashBalance(t, pool, f); got != 0 {
		t.Fatalf("PAY-REV-1 defect reproduced: player_cash = %d, want 0 (the single-reversal value) - a value below this "+
			"means a second reversal double-debited the wallet", got)
	}

	// B itself must have been denied, never silently reporting success
	// while posting nothing (which would be its own, different bug).
	if !errors.Is(bOut.err, ErrDepositAlreadyReversed) {
		t.Fatalf("expected reversal B to fail with ErrDepositAlreadyReversed, got res=%+v err=%v", bOut.res, bOut.err)
	}
}
