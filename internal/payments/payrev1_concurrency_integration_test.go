//go:build integration

// Stage 10.1 PAY-REV-1 (ADR 0090, docs/plans/stage-10.1-planning-gate-
// proposal.md §J) concurrency tests. Test #1 is required, by the plan, to
// be run and SHOWN FAILING against the unfixed orchestrator before the S2
// lock exists (evidence saved to docs/plans/stage-10.1-planning/evidence/
// pay-rev-1-test1-prefix-failure.txt) - it is not merely a regression
// test, it is the proof the defect (a check-then-insert race with no lock
// at all) is real and that this specific test shape detects it for the
// right reason, not because it happens to block on something else.
package payments

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// pr1ConfirmedDeposit is loConfirmedDeposit's twin, additionally
// returning the posted ledger_transactions id - needed here to hold the
// S2 row directly, which loConfirmedDeposit's own callers never needed.
func pr1ConfirmedDeposit(t *testing.T, pool *db.Pool, f orchFixture, orch *Orchestrator, provider *MockProvider, amount int64) (ref string, ledgerTxID uuid.UUID) {
	t.Helper()
	var intent DepositIntent
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = orch.InitiateDeposit(ctx, tx, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: amount, PaymentMethod: "card",
			IdempotencyKey: "payrev1-dep-" + uuid.NewString(),
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
	payload := provider.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, amount, "EUR", "", false)
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

// TestPayRev1_ConcurrentDistinctReferenceReversals_ExactlyOnePosts is
// test #1 (§J). Shape: a blocker holds a FOR UPDATE row lock on the
// original deposit's ledger_transactions row from OUTSIDE the code under
// test; a single reversal callback, naming that original under a
// DISTINCT provider reference, is started and must queue; the test
// inspects pg_stat_activity/pg_locks for the queued backend BEFORE
// releasing the blocker, then releases it and asserts the reversal
// eventually posts exactly once.
//
// Pre-fix (no S2 lock in receiveDepositReversalCallback at all), the
// racer does not queue at any explicit lock this package takes - it
// queues on the IMPLICIT `FOR KEY SHARE` lock Postgres takes when its own
// INSERT sets reverses_transaction_id to the blocked row (the
// self-referencing FK from migration 0021), deep inside ledger.Post's
// own INSERT statement - by which point Post has ALREADY taken its L3
// projection locks. Post-fix, the racer queues at S2's own explicit
// `SELECT ... FOR UPDATE`, before ever reaching GetOrCreateAccounts or
// L3. This test's two assertions (WHICH statement is executing, and
// whether a wallet_balance_projection lock is held) are exactly what
// distinguishes these two cases - "blocked on the blocker's pid" alone is
// true in both and would pass for the wrong reason (architect review,
// planning gate §J test #1's own note).
func TestPayRev1_ConcurrentDistinctReferenceReversals_ExactlyOnePosts(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	const amount = int64(7_500)
	depositRef, ledgerTxID := pr1ConfirmedDeposit(t, pool, f, orch, provider, amount)

	blocker := loHoldLedgerTransactionRow(t, pool, f.tenantID, ledgerTxID, "original-deposit-row")

	reversalPayload := provider.CallbackPayload(f.tenantID, CallbackEventDepositReversal,
		"payrev1-race-reversal-ref", depositRef, OutcomeSucceeded, amount, "EUR", "", false)

	racer := loStartRacer(t, pool, f.tenantID, "depositReversal", func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-psp", reversalPayload)
		return err
	})

	blockedBy, ok := loWaitBlocked(t, pool, racer.pid, racer.done)
	if !ok {
		blocker.release()
		t.Fatalf("reversal racer never blocked on the held original-deposit row; err=%v", racer.wait())
	}
	if !loContains(blockedBy, blocker.pid) {
		blocker.release()
		_ = racer.wait()
		t.Fatalf("reversal racer is blocked by %v, not by the blocker (pid %d) - the interleaving this test depends on did not happen",
			blockedBy, blocker.pid)
	}

	query := loBackendQuery(t, pool, racer.pid)
	holdsProjectionLock := loBackendHoldsProjectionLock(t, pool, racer.pid)

	blocker.release()
	if err := racer.wait(); err != nil {
		t.Fatalf("the reversal must post once the original row is released: %v", err)
	}

	const wantQuerySubstring = "SELECT transaction_type FROM ledger_transactions WHERE id = $1 AND tenant_id = $2 FOR UPDATE"
	if !strings.Contains(query, wantQuerySubstring) {
		t.Fatalf("PAY-REV-1 not fixed: the reversal callback must queue on its own S2 `FOR UPDATE` statement, not on an "+
			"incidental lock reached deeper in the call (e.g. the implicit foreign-key KEY SHARE lock inside ledger.Post's "+
			"INSERT). Waiting backend's query was:\n%s", query)
	}
	if holdsProjectionLock {
		t.Fatal("PAY-REV-1 not fixed: the reversal callback must not hold any wallet_balance_projection lock while " +
			"queued at S2 - if it does, it has already passed GetOrCreateAccounts and ledger.Post's own L3 pre-lock, " +
			"meaning it queued somewhere deeper than S2 (pre-fix behaviour)")
	}

	// Financial assertion: the reversal posted exactly once.
	if got := cashBalance(t, pool, f); got != 0 {
		t.Fatalf("player_cash = %d, want 0 (deposited then reversed once)", got)
	}
	var n int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND reverses_transaction_id = $2 AND transaction_type = 'deposit_reversal'`,
			f.tenantID, ledgerTxID).Scan(&n)
	}); err != nil {
		t.Fatalf("count reversals: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 posted deposit_reversal, got %d", n)
	}
}
