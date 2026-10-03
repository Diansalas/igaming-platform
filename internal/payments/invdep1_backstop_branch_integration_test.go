//go:build integration

// INVDEP1-BACKSTOP-BRANCH-TEST-1 (ledger-finance C-1, E2 review; PRH-2 C).
//
// Mutant M2 disables the backstop branch of postDepositSuccessOrDispute
// (orchestrator.go: `errors.Is(err, ErrDepositIntentAlreadyResolved)` -> T10/T13d
// dispute plus the payments_deposit_intent_index_backstop_fired P1) and used
// to survive the whole payments suite: a regression turning this race into a
// propagated error (a 5xx and a retry loop) would have passed every test.
//
// This test reaches the branch through a REAL race, with no production seam:
// the same construction as TestX5_LedgerBackstopMapping, but entering through
// the choke-point wrapper itself instead of postDepositSuccess.
//
//  1. Racer A (postDepositSuccess for the intent, no intent lock - exactly the
//     bypass the backstop exists to survive) blocks on a held psp_clearing
//     projection row, uncommitted.
//  2. Racer B enters postDepositSuccessOrDispute for a real attempt of the same
//     intent. Its pre-check and postDepositSuccess's own re-check both pass
//     (READ COMMITTED cannot see A's uncommitted posting), then B's ledger.Post
//     queues behind A.
//  3. The blocker releases; A commits; B's INSERT hits the 0107 unique index ->
//     ledger.ErrDepositAlreadyPostedForIntent -> ErrDepositIntentAlreadyResolved
//     -> the backstop branch.
package payments

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
)

func TestINVDEP1_BackstopBranch_PreCheckPassesLedgerIndexFires_DisputesAndCommits(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	mp := NewMockProvider("mock-bb", "EUR")
	registerCapability(t, pool, f, mp, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-bb": mp}, MultiWebhookCredentialResolver{"mock-bb": NewMockWebhookCredentials(mp)})

	// The real attempt racer B will try to resolve: pending, bound to refB.
	res := rvInit(t, pool, orch, f, 5000, "bb")
	intentID, attemptID := res.Intent.ID, res.Attempt.ID
	refB := *res.Attempt.ProviderReference

	var clearingID uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		accounts, err := ledger.GetOrCreateAccounts(ctx, tx, f.tenantID,
			ledger.AccountSpec{WalletID: &f.walletID, AccountType: ledger.AccountPlayerCash, AssetCode: "EUR"},
			ledger.AccountSpec{AccountType: ledger.AccountPSPClearing, AssetCode: "EUR"},
		)
		if err != nil {
			return err
		}
		clearingID = accounts[1]
		// A throwaway posting (different correlation) so both accounts have a
		// projection row to contend over.
		p, ref := "bb-seed-provider", "bb-seed-"+uuid.NewString()
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxDeposit, IdempotencyKey: p + ":" + ref,
			ProviderID: &p, ProviderTxID: &ref, CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: accounts[1], Direction: ledger.Debit, Amount: 1},
				{LedgerAccountID: accounts[0], Direction: ledger.Credit, Amount: 1},
			},
		})
		return err
	}); err != nil {
		t.Fatalf("setup: resolve/seed accounts: %v", err)
	}

	var intent DepositIntent
	var attempt PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if intent, err = GetDepositIntentByID(ctx, tx, intentID); err != nil {
			return err
		}
		attempt, err = GetAttemptByID(ctx, tx, attemptID)
		return err
	}); err != nil {
		t.Fatalf("setup: load intent/attempt: %v", err)
	}
	if attempt.State != AttemptPending {
		t.Fatalf("setup: attempt state=%s, want pending", attempt.State)
	}

	// Capture the P1 log lines.
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	blocker := loHoldProjectionRow(t, pool, f.tenantID, clearingID, "bb-blocker")

	racerA := loStartRacer(t, pool, f.tenantID, "A", func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := orch.postDepositSuccess(ctx, tx, intent, nil, "bb-provider-a", "refA", 5000, "EUR")
		return err
	})
	if _, ok := loWaitBlocked(t, pool, racerA.pid, racerA.done); !ok {
		blocker.release()
		<-racerA.done
		t.Fatalf("racer A never blocked on the held psp_clearing projection row")
	}

	var (
		gotDisputed bool
		gotTxID     uuid.UUID
	)
	racerB := loStartRacer(t, pool, f.tenantID, "B", func(ctx context.Context, tx pgx.Tx) error {
		_, txID, disputed, err := orch.postDepositSuccessOrDispute(ctx, tx, intent, attempt, "mock-bb", refB, 5000, "EUR", EvidenceCallback)
		gotDisputed, gotTxID = disputed, txID
		return err
	})
	if !loWaitBlockedByAnyOf(t, pool, racerB.pid, []int{racerA.pid}, racerB.done) {
		blocker.release()
		<-racerA.done
		<-racerB.done
		t.Fatalf("racer B never blocked behind racer A (its pre-check and re-check must have passed, then ledger.Post queued)")
	}

	blocker.release()
	errA := racerA.wait()
	errB := racerB.wait()

	if errA != nil {
		t.Fatalf("racer A (the winner) must post cleanly, got %v", errA)
	}
	// The transaction commits: the sentinel is consumed by the choke point, never propagated.
	if errB != nil {
		t.Fatalf("racer B: the backstop branch must consume ErrDepositIntentAlreadyResolved and commit, got error %v", errB)
	}
	if !gotDisputed || gotTxID != uuid.Nil {
		t.Fatalf("racer B: want disputed=true and no transaction id, got disputed=%v txID=%s", gotDisputed, gotTxID)
	}

	// The dispute was applied and committed.
	final := mustGetAttempt(t, pool, f.tenantID, attemptID)
	if final.State != AttemptDisputed || final.TerminalReason == nil || *final.TerminalReason != TerminalReasonMultipleSuccessForIntent {
		t.Fatalf("attempt state=%s reason=%v, want disputed/%s", final.State, final.TerminalReason, TerminalReasonMultipleSuccessForIntent)
	}
	// Exactly one payment.attempt_disputed, and it is the backstop's.
	var audits int64
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'payment.attempt_disputed' AND target_id = $2`,
			f.tenantID, attemptID.String()).Scan(&audits)
	}); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Errorf("payment.attempt_disputed audits=%d, want exactly 1", audits)
	}
	// The backstop P1 fired, exactly once, next to the ordinary alert.
	logged := logBuf.String()
	if n := strings.Count(logged, "payments_deposit_intent_index_backstop_fired"); n != 1 {
		t.Errorf("backstop P1 logged %d times, want exactly 1; log:\n%s", n, logged)
	}
	if n := strings.Count(logged, "payments_multiple_success_for_intent_alert"); n != 1 {
		t.Errorf("multiple-success alert logged %d times, want exactly 1", n)
	}
	// No second posting: exactly one deposit posting for the intent, and it is
	// racer A's; nothing is keyed on refB.
	if n := ledgerDepositTxCount(t, pool, f.tenantID, intentID); n != 1 {
		t.Fatalf("deposit postings for the intent=%d, want exactly 1", n)
	}
	var posted int64
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND provider_tx_id = $2`, f.tenantID, refB).Scan(&posted)
	}); err != nil {
		t.Fatal(err)
	}
	if posted != 0 {
		t.Errorf("a ledger transaction exists for the disputed reference (%d)", posted)
	}
	assertLedgerBalanced(t, pool, f.tenantID)
}
