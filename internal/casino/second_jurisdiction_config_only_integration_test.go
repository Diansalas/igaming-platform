//go:build integration

package casino

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/risk"
)

// TestReceiveCallback_SecondJurisdictionIsConfigurationOnly proves that
// adding a second jurisdiction is DATA, not code: two synthetic
// jurisdictions (registry rows) each get their own risk rule (configuration
// rows), and the one postBet code path enforces the right rule per session
// jurisdiction - J1's 50-unit ceiling denies a 75 bet, J2's 500-unit ceiling
// lets the same bet post - while a session with NO resolved jurisdiction
// fails closed against the existing jurisdiction-scoped rules (the
// "absent/unresolvable jurisdiction never lets a jurisdiction-dependent
// policy be evaluated as if it did not exist" invariant). Nothing in the
// casino or risk packages names either jurisdiction.
//
// Scope note: the session's jurisdiction is the value LaunchGame persisted
// (CreateLaunchSession seam). In production LaunchGame obtains it from
// jurisdiction.Resolve, which is unresolved(no_signal) for players until
// HDR-J-1/HDR-J-3 - hence the third, fail-closed leg is what a real player
// currently hits when any jurisdiction-scoped rule exists.
func TestReceiveCallback_SecondJurisdictionIsConfigurationOnly(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 100000)
	j1 := seedJurisdiction(t, pool)
	j2 := seedJurisdiction(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider)).WithUngatedMarketOpenForTests()

	for code, threshold := range map[string]int64{j1: 50, j2: 500} {
		createCasinoRiskRule(t, pool, f, risk.CreateRuleParams{
			JurisdictionCode: code, Operation: risk.OperationCasinoBet,
			LimitKind: risk.LimitMaxAmount, TimeWindow: risk.WindowTransaction, Threshold: threshold, RuleKind: risk.RuleHardLimit,
		})
	}

	receive := func(txID string, sessionID uuid.UUID) (ReceiveCallbackResult, error) {
		payload := provider.CallbackPayload(f.tenantID, CallbackEventBet, txID, "", "round-"+txID, "game-1", 75, "EUR", OutcomeSucceeded, "", f.playerRef, sessionID)
		var result ReceiveCallbackResult
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			result, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
			return err
		})
		return result, err
	}

	// Jurisdiction 1: its own 50-unit ceiling denies the 75 bet.
	res, err := receive("bet-j1", mintSessionWithJurisdiction(t, pool, f, "mock-casino", "EUR", j1))
	if err != nil || res.Outcome != OutcomeDeclined || res.DeclineReason != risk.CodeHardLimitBreach {
		t.Fatalf("jurisdiction 1: expected hard-limit decline, got %+v err=%v", res, err)
	}
	// Jurisdiction 2: same code path, its own 500-unit ceiling, bet posts.
	res, err = receive("bet-j2", mintSessionWithJurisdiction(t, pool, f, "mock-casino", "EUR", j2))
	if err != nil || res.Outcome != OutcomeSucceeded {
		t.Fatalf("jurisdiction 2: expected the bet to post under its own rule, got %+v err=%v", res, err)
	}
	if balance := cashBalance(t, pool, f); balance != 100000-75 {
		t.Fatalf("expected exactly the jurisdiction-2 bet to post, got balance %d", balance)
	}
	// No resolved jurisdiction: fail closed, no ledger effect.
	_, err = receive("bet-none", mintSession(t, pool, f, "mock-casino", "EUR"))
	if !errors.Is(err, risk.ErrMissingJurisdiction) {
		t.Fatalf("unresolved jurisdiction must fail closed against jurisdiction-scoped rules, got %v", err)
	}
	if balance := cashBalance(t, pool, f); balance != 100000-75 {
		t.Fatalf("fail-closed leg must have no ledger effect, got balance %d", balance)
	}
}
