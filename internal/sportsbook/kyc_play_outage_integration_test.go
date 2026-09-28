//go:build integration

// KYC-ENF-OUTAGE-1 / FK-1 (code review f-kyc-code-review.md, 2026-09-28):
// a play-path counterpart to internal/withdrawal's own fault-injection
// test, and internal/casino's own
// TestReceiveCallback_KYCStoreOutage_DeclinesUnavailableWithOneDecisionRow.
// ADR 0096 §16.2/§23.1 marked LF-I3-5 "CLOSED" on the strength of the
// savepoint fix alone, but LF-I3-5 names TWO failure cases -
// EvaluateEnforcement returning an error, and RecordDecision returning
// one - and only the first is proven anywhere in the repo. This test pins
// the first (evaluate-error) half for the sportsbook PlaceBet call site,
// mirroring the withdrawal/casino lock/lock_timeout handshake exactly
// (deterministic - the blocker's own LOCK TABLE statement only returns
// once the lock is genuinely held).
package sportsbook

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// lockKYCVerificationsTable mirrors internal/withdrawal's and
// internal/casino's own helper of the same purpose (package-private test
// code duplicated per this repo's convention).
func lockKYCVerificationsTable(t *testing.T, pool *db.Pool) (release func()) {
	t.Helper()
	ready := make(chan error, 1)
	proceed := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `LOCK TABLE kyc_verifications IN ACCESS EXCLUSIVE MODE`); err != nil {
				ready <- err
				return err
			}
			ready <- nil
			<-proceed
			return nil
		})
	}()

	if err := <-ready; err != nil {
		t.Fatalf("blocker failed to acquire ACCESS EXCLUSIVE lock on kyc_verifications: %v", err)
	}

	var released bool
	return func() {
		if released {
			return
		}
		released = true
		close(proceed)
		if err := <-done; err != nil {
			t.Fatalf("blocker transaction failed: %v", err)
		}
	}
}

// tenantLedgerDebitsCredits sums debits/credits across EVERY ledger entry
// for tenantID - used here to prove a KYC-unavailable PlaceBet posts
// nothing at all, not merely that its own (non-existent) transaction is
// internally balanced.
func tenantLedgerDebitsCredits(t *testing.T, pool *db.Pool, tenantID uuid.UUID) (debits, credits int64) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'), 0), COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0)
			 FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.ledger_account_id WHERE a.tenant_id = $1`,
			tenantID).Scan(&debits, &credits)
	})
	if err != nil {
		t.Fatalf("sum tenant debits/credits: %v", err)
	}
	return debits, credits
}

// TestPlaceBet_KYCStoreOutage_DeclinesUnavailableWithOneDecisionRow is
// FK-1's sportsbook half. A genuine Postgres error inside the play
// trigger's verification read (not a seeded status) must decline the bet
// with `kyc_unavailable:verification_lookup_failed`, write exactly one
// `unavailable` decision row, and leave the balance/ledger untouched.
func TestPlaceBet_KYCStoreOutage_DeclinesUnavailableWithOneDecisionRow(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	jid := mustLicenseSBTenant(t, pool, f)
	mustActiveSportsbookPlayPolicy(t, pool, jid)
	sel := seedSelection(t, pool, seedSelectionParams{OddsNumerator: 250, OddsDenominator: 100})

	decisionsBefore := countRows(t, pool, f.tenantID,
		`SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id = $1 AND player_account_id = $2`,
		f.tenantID, f.playerAccountID)
	// Captured AFTER fundWallet's own deposit posting (not zero - the
	// point is that the ATTEMPT itself posts nothing more, not that the
	// tenant's ledger is pristine).
	debitsBefore, creditsBefore := tenantLedgerDebitsCredits(t, pool, f.tenantID)

	release := lockKYCVerificationsTable(t, pool)
	defer release()

	var result PlaceBetResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '300ms'`); err != nil {
			return err
		}
		var err error
		result, err = PlaceBet(ctx, tx, PlaceBetParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			SelectionID: sel.ID, AssetCode: "EUR", StakeAmount: 1_000,
			ExpectedOddsNumerator: sel.OddsNumerator, ExpectedOddsDenominator: sel.OddsDenominator,
			IdempotencyKey: "place-kyc-outage-1",
		})
		return err
	})
	release()
	if err != nil {
		t.Fatalf("expected the outer transaction to commit cleanly (the read failure must be contained to a savepoint), got: %v", err)
	}

	if result.Accepted {
		t.Fatalf("expected the bet to be declined by a genuine KYC-store outage, got %+v", result)
	}
	if result.RejectionCategory != RejectionKYCDenied {
		t.Fatalf("expected RejectionKYCDenied, got %q (%q)", result.RejectionCategory, result.RejectionMessage)
	}
	if result.RejectionCode != "kyc_unavailable:verification_lookup_failed" {
		t.Fatalf("expected RejectionCode=%q, got %q", "kyc_unavailable:verification_lookup_failed", result.RejectionCode)
	}

	if got := cashBalance(t, pool, f); got != 10_000 {
		t.Fatalf("expected player_cash UNCHANGED at 10000 (an unavailable outcome must post nothing), got %d", got)
	}
	debitsAfter, creditsAfter := tenantLedgerDebitsCredits(t, pool, f.tenantID)
	if debitsAfter != creditsAfter {
		t.Fatalf("invariant #1 violated: debits=%d credits=%d", debitsAfter, creditsAfter)
	}
	if debitsAfter != debitsBefore || creditsAfter != creditsBefore {
		t.Fatalf("expected NO new ledger postings for this tenant (unavailable outcome must post nothing): before debits=%d credits=%d, after debits=%d credits=%d",
			debitsBefore, creditsBefore, debitsAfter, creditsAfter)
	}

	decisionsAfter := countRows(t, pool, f.tenantID,
		`SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id = $1 AND player_account_id = $2`,
		f.tenantID, f.playerAccountID)
	if decisionsAfter != decisionsBefore+1 {
		t.Fatalf("expected exactly 1 new kyc_enforcement_decisions row, got %d new", decisionsAfter-decisionsBefore)
	}
	unavailableCount := countRows(t, pool, f.tenantID,
		`SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id = $1 AND player_account_id = $2 AND operation = 'sportsbook_play' AND outcome = 'unavailable'`,
		f.tenantID, f.playerAccountID)
	if unavailableCount != 1 {
		t.Fatalf("expected exactly 1 kyc_enforcement_decisions row with operation='sportsbook_play' outcome='unavailable', got %d", unavailableCount)
	}
}
