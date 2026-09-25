//go:build integration

package withdrawal

// Stage 10 F-7 remediation (ADR 0020 amendment 2026-09-25,
// docs/governance/stage-10-f7-ledger-replay-audit.md §3 sites #1-#5,
// §6.4 "withdrawal"): every withdrawal posting is keyed and state-gated
// so a retry never reaches ledger.Post with a different payload. These
// tests pin that each retry is either the idempotent original (Request)
// or ErrStateConflict with one posting (Reject/Complete/Fail/Cancel), and
// close the audit's evidence gap for site #3 (a confirmation reference
// reused across two requests).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

func f7LedgerTxCount(t *testing.T, pool *db.Pool, tenantID uuid.UUID) int {
	t.Helper()
	var n int
	mustRunTx(t, pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenantID).Scan(&n)
	})
	return n
}

// f7Submitted drives one request to `submitted`.
func f7Submitted(t *testing.T, pool *db.Pool, f fixture, amount int64, key, payoutRef string) WithdrawalRequest {
	t.Helper()
	wr := mustRequestWithdrawal(t, pool, f, amount, key)
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error { return MoveToPendingReview(ctx, tx, wr.ID) })
	approverID := mustCreateApprover(t, pool, f.tenantID)
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Approve(ctx, tx, wr.ID, approverID, false, nil, alwaysEligible)
		return err
	})
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MarkSubmitted(ctx, tx, wr.ID, "mockpsp", payoutRef)
	})
	return wr
}

// Site #1: a Request retry with a different amount is rejected at the
// withdrawal_requests layer, before ledger.Post; one hold only.
func TestF7Withdrawal_RequestRetryWithDifferentAmountRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1000)
	mustRequestWithdrawal(t, pool, f, 400, "f7-wd-req")
	before := f7LedgerTxCount(t, pool, f.tenantID)
	if _, err := requestWithdrawal(t, pool, f, 450, "f7-wd-req"); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("want ErrIdempotencyKeyReused, got %v", err)
	}
	if got := f7LedgerTxCount(t, pool, f.tenantID); got != before {
		t.Fatalf("ledger transactions %d -> %d", before, got)
	}
	if got := cashBalance(t, pool, f); got != 600 {
		t.Fatalf("player_cash = %d, want 600", got)
	}
}

// Sites #2-#5: a second Reject/Complete/Fail/Cancel of one request is
// ErrStateConflict with exactly one posting.
func TestF7Withdrawal_TerminalTransitionsDoubleInvokeOnePosting(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 10_000)
	mustSetWithdrawalPolicy(t, pool, f.tenantID, "EUR", 1_000_000, 2, time.Now().Add(-time.Hour))

	type step struct {
		name string
		prep func() uuid.UUID
		call func(ctx context.Context, tx pgx.Tx, id uuid.UUID) error
	}
	approver := mustCreateApprover(t, pool, f.tenantID)
	steps := []step{
		{"reject", func() uuid.UUID {
			wr := mustRequestWithdrawal(t, pool, f, 100, "f7-wd-reject")
			mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error { return MoveToPendingReview(ctx, tx, wr.ID) })
			return wr.ID
		}, func(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
			return Reject(ctx, tx, id, approver, "failed_kyc_check", alwaysEligible)
		}},
		{"complete", func() uuid.UUID { return f7Submitted(t, pool, f, 100, "f7-wd-complete", "f7-payout-complete").ID },
			func(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
				return Complete(ctx, tx, id, "mockpsp", "f7-confirm-complete")
			}},
		{"fail", func() uuid.UUID { return f7Submitted(t, pool, f, 100, "f7-wd-fail", "f7-payout-fail").ID },
			func(ctx context.Context, tx pgx.Tx, id uuid.UUID) error { return Fail(ctx, tx, id, "psp_declined") }},
		{"cancel", func() uuid.UUID { return mustRequestWithdrawal(t, pool, f, 100, "f7-wd-cancel").ID },
			func(ctx context.Context, tx pgx.Tx, id uuid.UUID) error { return Cancel(ctx, tx, id) }},
	}
	for _, s := range steps {
		id := s.prep()
		mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error { return s.call(ctx, tx, id) })
		before := f7LedgerTxCount(t, pool, f.tenantID)
		err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error { return s.call(ctx, tx, id) })
		if !errors.Is(err, ErrStateConflict) {
			t.Fatalf("%s twice: want ErrStateConflict, got %v", s.name, err)
		}
		if got := f7LedgerTxCount(t, pool, f.tenantID); got != before {
			t.Fatalf("%s twice: ledger transactions %d -> %d", s.name, before, got)
		}
	}
}

// Site #3 evidence gap: a send-confirmation reference already used to
// complete request W1, passed to Complete for a DIFFERENT submitted
// request W2. Before F-7, ledger.Post would have returned W1's
// transaction as AlreadyPosted and W2 would have been marked completed
// against it with its hold never released. Now the ledger rejects it and
// nothing changes.
func TestF7Withdrawal_CompleteWithAnotherRequestsConfirmationRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 10_000)
	mustSetWithdrawalPolicy(t, pool, f.tenantID, "EUR", 1_000_000, 2, time.Now().Add(-time.Hour))
	w1 := f7Submitted(t, pool, f, 300, "f7-wd-w1", "f7-payout-w1")
	w2 := f7Submitted(t, pool, f, 300, "f7-wd-w2", "f7-payout-w2")
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return Complete(ctx, tx, w1.ID, "mockpsp", "f7-confirm-shared")
	})

	before := f7LedgerTxCount(t, pool, f.tenantID)
	hold := holdBalance(t, pool, f)
	err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return Complete(ctx, tx, w2.ID, "mockpsp", "f7-confirm-shared")
	})
	if !errors.Is(err, ledger.ErrIdempotencyPayloadMismatch) {
		t.Fatalf("want ledger.ErrIdempotencyPayloadMismatch, got %v", err)
	}
	if got := f7LedgerTxCount(t, pool, f.tenantID); got != before {
		t.Fatalf("ledger transactions %d -> %d", before, got)
	}
	if got := holdBalance(t, pool, f); got != hold {
		t.Fatalf("hold %d -> %d", hold, got)
	}
	var final WithdrawalRequest
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		final, err = GetByID(ctx, tx, w2.ID)
		return err
	})
	if final.State != StateSubmitted || final.ReleaseLedgerTransactionID != nil {
		t.Fatalf("W2 = %s / %v, want submitted with no release transaction", final.State, final.ReleaseLedgerTransactionID)
	}
}
