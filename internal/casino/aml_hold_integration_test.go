//go:build integration

package casino

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// ADR 0116 s5.1 at the casino gate: a NEW stake on a held player is declined with the SHARED public reason (account_review),
// byte-identical to a KYC-pending decline; wins, rollbacks (terminal stake returns) and every other settlement-side callback
// are never gated and never refused for a held player.

func holdCasinoPlayer(t *testing.T, pool *db.Pool, f casinoFixture, scope aml.HoldScope) {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	prooftest.Install(t, u)
	amlfix.HoldFor(t, launchfix.OwnerFor(t, pool), pool, f.tenantID, f.personID, nil, scope)
}

func callback(t *testing.T, pool *db.Pool, f casinoFixture, orch *Orchestrator, payload webhookauth.Inbound) ReceiveCallbackResult {
	t.Helper()
	var result ReceiveCallbackResult
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	}); err != nil {
		t.Fatalf("ReceiveCallback: %v", err)
	}
	return result
}

func TestAMLHold_CasinoStake_HeldAndKYCPending_SameDeclineReason(t *testing.T) {
	pool := testPool(t)

	// KYC pending under an active play policy.
	pf := seedCasinoFixture(t, pool)
	fundWallet(t, pool, pf, 5000)
	mustActiveCasinoPlayPolicy(t, pool, mustLicenseCasinoTenant(t, pool, pf))
	time.Sleep(10 * time.Millisecond)
	mustSeedCasinoVerification(t, pool, pf, "pending")

	// AML held (no KYC policy at all).
	hf := seedCasinoFixture(t, pool)
	fundWallet(t, pool, hf, 5000)
	holdCasinoPlayer(t, pool, hf, aml.ScopeAllValueMovement)

	reasons := map[string]string{}
	for name, f := range map[string]casinoFixture{"pending": pf, "held": hf} {
		provider := NewMockCasinoProvider("mock-casino", "EUR")
		registerCasinoCapability(t, pool, f, provider, 100)
		sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
		orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider)).WithUngatedMarketOpenForTests()
		payload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-aml-"+name, "", "round-aml-"+name, "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerRef, sessionID)
		var result ReceiveCallbackResult
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			result, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
			return err
		}); err != nil {
			t.Fatalf("%s: ReceiveCallback: %v", name, err)
		}
		if result.Outcome != OutcomeDeclined {
			t.Fatalf("%s: the stake must be declined: %+v", name, result)
		}
		reasons[name] = result.DeclineReason
		if bal := cashBalance(t, pool, f); bal != 5000 {
			t.Fatalf("%s: a declined stake posts nothing: %d", name, bal)
		}
	}
	if reasons["pending"] != "account_review" || reasons["held"] != "account_review" {
		t.Fatalf("both declines must carry the shared reason: %v", reasons)
	}
}

// LF: wins and rollbacks (terminal stake returns) are never gated by a hold.
// LF C4: a rollback for a transaction never seen writes a tombstone; that is a replay-protection marker, not value entering play, so
// it is never gated and never refused for a held player (and the late original is then rejected by the tombstone, not by the hold).
func TestAMLHold_CasinoTombstoneRollbackWithNoOriginal_IsNeverGated(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider)).WithUngatedMarketOpenForTests()
	holdCasinoPlayer(t, pool, f, aml.ScopeAllValueMovement)
	before := cashBalance(t, pool, f)
	r := callback(t, pool, f, orch, provider.CallbackPayload(f.tenantID, CallbackEventRollback, "rollback-unseen", "bet-never-seen", "round-x", "game-1", 0, "EUR", "", "", f.playerRef, uuid.Nil))
	if !r.Tombstoned || r.DeclineReason != "" {
		t.Fatalf("a tombstone rollback must not be refused by a hold: %+v", r)
	}
	if got := cashBalance(t, pool, f); got != before {
		t.Fatalf("a tombstone moves no value: %d -> %d", before, got)
	}
	_ = sessionID
}

func TestAMLHold_CasinoWinAndRollback_AreNeverGated(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider)).WithUngatedMarketOpenForTests()

	post := func(p webhookauth.Inbound) ReceiveCallbackResult { return callback(t, pool, f, orch, p) }
	// Two stakes BEFORE the hold exists.
	if r := post(provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-a", "", "round-a", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerRef, sessionID)); r.Outcome != OutcomeSucceeded {
		t.Fatalf("pre-hold stake a: %+v", r)
	}
	if r := post(provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-b", "", "round-b", "game-1", 500, "EUR", OutcomeSucceeded, "", f.playerRef, sessionID)); r.Outcome != OutcomeSucceeded {
		t.Fatalf("pre-hold stake b: %+v", r)
	}
	if bal := cashBalance(t, pool, f); bal != 3500 {
		t.Fatalf("setup balance: %d", bal)
	}
	holdCasinoPlayer(t, pool, f, aml.ScopeAllValueMovement)

	// A NEW stake is refused...
	if r := post(provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-c", "", "round-c", "game-1", 100, "EUR", OutcomeSucceeded, "", f.playerRef, sessionID)); r.Outcome != OutcomeDeclined {
		t.Fatalf("a new stake on a held player must be declined: %+v", r)
	}
	// ...but the win of round a and the rollback of bet b go through.
	if r := post(provider.CallbackPayload(f.tenantID, CallbackEventWin, "win-a", "", "round-a", "game-1", 2500, "EUR", OutcomeSucceeded, "", f.playerRef, uuid.Nil)); r.Outcome != OutcomeSucceeded {
		t.Fatalf("a win must not be gated by a hold: %+v", r)
	}
	if r := post(provider.CallbackPayload(f.tenantID, CallbackEventRollback, "rollback-b", "bet-b", "round-b", "game-1", 0, "EUR", "", "", f.playerRef, uuid.Nil)); r.Outcome != OutcomeSucceeded {
		t.Fatalf("a rollback (terminal stake return) must not be gated by a hold: %+v", r)
	}
	if bal := cashBalance(t, pool, f); bal != 3500+2500+500 {
		t.Fatalf("the win and the returned stake must credit the held player: %d", bal)
	}
}
