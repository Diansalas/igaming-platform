//go:build integration

// KYC-ENF-OUTAGE-1 / FK-1 (code review f-kyc-code-review.md, 2026-09-28):
// a play-path counterpart to internal/withdrawal's own fault-injection
// test. ADR 0096 §16.2/§23.1 marked LF-I3-5 "CLOSED" on the strength of
// the savepoint fix alone, but LF-I3-5 names TWO failure cases -
// EvaluateEnforcement returning an error, and RecordDecision returning
// one - and only the first is proven anywhere in the repo. This test pins
// the first (evaluate-error) half for the casino play call site,
// mirroring internal/withdrawal/kyc_outage_fault_injection_integration_test.go's
// own lock/lock_timeout handshake exactly (deterministic: the blocker's
// own LOCK TABLE statement only returns once the lock is genuinely held,
// no polling, no wall-clock assertion beyond the lock_timeout GUC under
// test).
package casino

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// lockKYCVerificationsTable mirrors internal/withdrawal's own helper of
// the same purpose (package-private test code duplicated per this repo's
// convention) - a blocker transaction on a separate pool connection takes
// `LOCK TABLE kyc_verifications IN ACCESS EXCLUSIVE MODE` and holds it
// until release() is called. Returning from the blocker's own LOCK TABLE
// statement proves the lock is held - there is no contention to wait out.
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

// TestReceiveCallback_KYCStoreOutage_DeclinesUnavailableWithOneDecisionRow
// is FK-1's casino half. A genuine Postgres error inside the play trigger's
// verification read (not a seeded status) must decline the bet with
// `kyc_unavailable:verification_lookup_failed`, write exactly one
// `unavailable` decision row, and leave the balance/ledger untouched -
// never post a bet on the strength of a mere read failure.
func TestReceiveCallback_KYCStoreOutage_DeclinesUnavailableWithOneDecisionRow(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	jid := mustLicenseCasinoTenant(t, pool, f)
	mustActiveCasinoPlayPolicy(t, pool, jid)

	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	decisionsBefore := countRows(t, pool, f.tenantID,
		`SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id = $1 AND player_account_id = $2`,
		f.tenantID, f.playerAccountID)

	release := lockKYCVerificationsTable(t, pool)
	defer release()

	payload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-kyc-outage-1", "", "round-kyc-outage-1", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	var result ReceiveCallbackResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '300ms'`); err != nil {
			return err
		}
		var err error
		result, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	})
	release()
	if err != nil {
		t.Fatalf("expected the outer transaction to commit cleanly (the read failure must be contained to a savepoint), got: %v", err)
	}

	if result.Outcome != OutcomeDeclined {
		t.Fatalf("expected the bet to be declined by a genuine KYC-store outage, got %+v", result)
	}
	if result.DeclineReason != "kyc_unavailable:verification_lookup_failed" {
		t.Fatalf("expected DeclineReason=%q, got %q", "kyc_unavailable:verification_lookup_failed", result.DeclineReason)
	}

	if balance := cashBalance(t, pool, f); balance != 5000 {
		t.Fatalf("expected cash balance UNCHANGED at 5000 (an unavailable outcome must post nothing), got %d", balance)
	}
	debits, credits := sumDebitsCredits(t, pool, f.tenantID)
	if debits != credits {
		t.Fatalf("invariant #1 violated: debits=%d credits=%d", debits, credits)
	}

	decisionsAfter := countRows(t, pool, f.tenantID,
		`SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id = $1 AND player_account_id = $2`,
		f.tenantID, f.playerAccountID)
	if decisionsAfter != decisionsBefore+1 {
		t.Fatalf("expected exactly 1 new kyc_enforcement_decisions row, got %d new", decisionsAfter-decisionsBefore)
	}
	unavailableCount := countRows(t, pool, f.tenantID,
		`SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id = $1 AND player_account_id = $2 AND operation = 'casino_play' AND outcome = 'unavailable'`,
		f.tenantID, f.playerAccountID)
	if unavailableCount != 1 {
		t.Fatalf("expected exactly 1 kyc_enforcement_decisions row with operation='casino_play' outcome='unavailable', got %d", unavailableCount)
	}
}
