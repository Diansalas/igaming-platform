//go:build integration

// CAS-PLAY-BOOTSTRAP-1 (ADR 0103) - the remaining missing tests named by
// the architect/QA review, F-3 (docs/plans/prh2-hardening-round/reviews/
// b-architect-qa-pop.md). Each sub-heading below names the exact F-3 list
// item it closes.
//
// One F-3 item is NOT added here, and is recorded rather than silently
// skipped: "a forced revoke returning false -> ErrBootstrapInvariantBroken
// -> 5xx". Under the session row's own FOR NO KEY UPDATE lock (held for
// this function's entire transaction, acquired before the gate check ever
// runs), RevokeLaunchSession's own CAS (`UPDATE ... WHERE status =
// 'active'`) cannot observe any status other than 'active' at that point -
// no other transaction can have changed it, and this function's own step 3
// binding check has already passed by then. This is the SAME "structurally
// unreachable, not merely untested" shape F-4's own two mutants (10/11)
// already are, for the identical reason (see ADR 0103 §13 and the
// mutation-kill evidence file) - forcing it would require a white-box test
// seam this codebase does not have (there is no injectable
// RevokeLaunchSession), and adding one only for this single assertion
// would be new production surface for a test-only need. Reported to the
// orchestrator rather than fabricated.
package casino

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

// --- F-3: "the refusal-does-not-consume sequence (each step 1-3 failure,
// then the legitimate request -> 200)" ---

// TestBootstrapLaunch_RefusalsThenLegitimateRequest_Sequence drives, in
// order, against the SAME session: a step-2 not-found (a garbage token
// that resolves to no session at all), a step-3 binding mismatch (wrong
// provider_game_id), a replay-shaped mismatch (an existing request_id -
// already committed by a genuinely DIFFERENT session - reused with THIS
// session's own token), and finally the legitimate request itself - which
// must still succeed with a fresh 200, proving none of the three prior
// refusals left any mutating trace on the session under test (CLAUDE.md:
// a refusal must never look like a partial write).
func TestBootstrapLaunch_RefusalsThenLegitimateRequest_Sequence(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)
	session, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)

	// A genuinely different session, bootstrapped first under a KNOWN
	// request_id, purely so the replay-mismatch step below has an existing
	// idempotency row to collide with.
	_, otherToken := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)
	const sharedRequestID = "req-f3-seq-shared"
	inOther := provider.BootstrapPayload(f.tenantID, otherToken, sharedRequestID, game.ProviderGameID, "EUR", "real")
	vOther := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", inOther)
	if _, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", vOther); err != nil {
		t.Fatalf("bootstrap the OTHER session (seeds the shared request_id): %v", err)
	}

	// Step-2 "not found": a garbage token resolves to no session at all.
	inNotFound := provider.BootstrapPayload(f.tenantID, "not-a-real-launch-token", "req-f3-seq-notfound", game.ProviderGameID, "EUR", "real")
	vNotFound := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", inNotFound)
	_, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", vNotFound)
	requireBootstrapRefused(t, err, BootstrapRefusalNotFound)
	assertSessionUntouchedAndNoBootstrapRow(t, pool, f, session.ID, LaunchSessionActive)

	// Step-3 binding mismatch on the session under test.
	inMismatch := provider.BootstrapPayload(f.tenantID, token, "req-f3-seq-mismatch", "wrong-game-id", "EUR", "real")
	vMismatch := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", inMismatch)
	_, err = orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", vMismatch)
	requireBootstrapRefused(t, err, BootstrapRefusalBindingMismatch)
	assertSessionUntouchedAndNoBootstrapRow(t, pool, f, session.ID, LaunchSessionActive)

	// Replay-shaped mismatch: the OTHER session's own already-committed
	// request_id, but THIS session's own token - a different token racing
	// an existing request_id, refused as a mismatch.
	inReplayMismatch := provider.BootstrapPayload(f.tenantID, token, sharedRequestID, game.ProviderGameID, "EUR", "real")
	vReplayMismatch := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", inReplayMismatch)
	_, err = orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", vReplayMismatch)
	requireBootstrapRefused(t, err, BootstrapRefusalReplayMismatch)
	assertSessionUntouchedAndNoBootstrapRow(t, pool, f, session.ID, LaunchSessionActive)

	// Finally, the legitimate request - a fresh request_id, this session's
	// own token, the correct binding.
	inOK := provider.BootstrapPayload(f.tenantID, token, "req-f3-seq-legit", game.ProviderGameID, "EUR", "real")
	vOK := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", inOK)
	result, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", vOK)
	if err != nil {
		t.Fatalf("expected the legitimate request to succeed after three refusals, got error: %v", err)
	}
	if result.Denied || result.Replayed {
		t.Fatalf("expected a fresh success, got %+v", result)
	}
}

// --- F-3: "mode, asset and game mismatches via BootstrapLaunch" ---

func TestBootstrapLaunch_BindingMismatch_ByField(t *testing.T) {
	cases := []struct {
		name           string
		requestIDSlug  string
		providerGameID string
		assetCode      string
		mode           string
	}{
		{"provider_game_id mismatch", "req-f3-bind-pgid", "wrong-game-id", "EUR", "real"},
		{"asset_code mismatch", "req-f3-bind-asset", "", "USD", "real"},
		{"mode mismatch", "req-f3-bind-mode", "", "EUR", "demo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool, f, game, provider, orch := setupBootstrapFixture(t)
			session, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)
			providerGameID := tc.providerGameID
			if providerGameID == "" {
				providerGameID = game.ProviderGameID
			}
			in := provider.BootstrapPayload(f.tenantID, token, tc.requestIDSlug, providerGameID, tc.assetCode, tc.mode)
			v := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)
			_, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v)
			requireBootstrapRefused(t, err, BootstrapRefusalBindingMismatch)
			assertSessionUntouchedAndNoBootstrapRow(t, pool, f, session.ID, LaunchSessionActive)
		})
	}
}

// --- F-3: "an already-consumed token with a new request_id (sequential)" ---

// TestBootstrapLaunch_AlreadyConsumedToken_NewRequestID_Sequential proves
// the SAME token, once genuinely consumed by a first bootstrap, cannot be
// bootstrapped a second time under a DIFFERENT request_id - the binding
// still matches (step 3 passes), but step 5's CAS (`WHERE status =
// 'active'`) now finds the session already 'consumed', and refuses. This
// is deliberately SEQUENTIAL (one request fully committing before the
// second is sent), unlike TestBootstrapLaunch_ConcurrentConsumes_
// SameRequestID's forced-interleaving race - a different code path
// (step 5's CAS on an already-settled row, not the row lock's own
// serialization of two IN-FLIGHT attempts).
func TestBootstrapLaunch_AlreadyConsumedToken_NewRequestID_Sequential(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)
	session, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)

	inFirst := provider.BootstrapPayload(f.tenantID, token, "req-f3-consumed-first", game.ProviderGameID, "EUR", "real")
	vFirst := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", inFirst)
	if _, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", vFirst); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}

	inSecond := provider.BootstrapPayload(f.tenantID, token, "req-f3-consumed-second", game.ProviderGameID, "EUR", "real")
	vSecond := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", inSecond)
	_, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", vSecond)
	requireBootstrapRefused(t, err, BootstrapRefusalNotActive)

	var bootstrapCount int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM casino_launch_bootstraps WHERE launch_session_id = $1`, session.ID).Scan(&bootstrapCount)
	}); err != nil {
		t.Fatalf("count bootstrap rows: %v", err)
	}
	if bootstrapCount != 1 {
		t.Fatalf("expected exactly 1 bootstrap row (the first, legitimate one) to survive, got %d", bootstrapCount)
	}
}

// --- F-3: "player_ref differing across tenants" ---

// TestBootstrapLaunch_PlayerRefDiffersAcrossTenants is the cross-tenant
// counterpart to the already-existing TestBootstrapLaunch_
// PlayerRefDiffersAcrossProviders (same provider, different tenants,
// rather than same tenant, different providers) - ADR 0103 §3.5's opacity
// claim ("unlinkable across tenants OR providers") has two independent
// axes, and only one was pinned before this.
func TestBootstrapLaunch_PlayerRefDiffersAcrossTenants(t *testing.T) {
	poolA, fA, gameA, providerA, orchA := setupBootstrapFixture(t)
	_, tokenA := mintSessionForBootstrap(t, poolA, fA, gameA, ModeReal, "EUR", DefaultLaunchTokenTTL)
	inA := providerA.BootstrapPayload(fA.tenantID, tokenA, "req-f3-xtenant-a", gameA.ProviderGameID, "EUR", "real")
	vA := bootstrapVerified(t, orchA, poolA, fA.tenantID, "mock-casino", inA)
	resultA, err := orchA.BootstrapLaunch(context.Background(), poolA, fA.tenantID, "mock-casino", vA)
	if err != nil {
		t.Fatalf("bootstrap tenant A: %v", err)
	}

	poolB, fB, gameB, providerB, orchB := setupBootstrapFixture(t)
	_, tokenB := mintSessionForBootstrap(t, poolB, fB, gameB, ModeReal, "EUR", DefaultLaunchTokenTTL)
	inB := providerB.BootstrapPayload(fB.tenantID, tokenB, "req-f3-xtenant-b", gameB.ProviderGameID, "EUR", "real")
	vB := bootstrapVerified(t, orchB, poolB, fB.tenantID, "mock-casino", inB)
	resultB, err := orchB.BootstrapLaunch(context.Background(), poolB, fB.tenantID, "mock-casino", vB)
	if err != nil {
		t.Fatalf("bootstrap tenant B: %v", err)
	}

	if resultA.PlayerRef == resultB.PlayerRef {
		t.Fatalf("expected player_ref to differ across tenants (same provider), got the SAME ref %s for both", resultA.PlayerRef)
	}
}

// --- F-3: "a gate denial, then a refused postBet" ---

// TestBootstrapLaunch_GateDenialThenPostBetRefused proves a bootstrap's
// own definitive gate denial (which revokes the session, ADR 0103 §3.3)
// has the expected downstream effect on the REAL bet path: a provider bet
// callback against that now-'revoked' session is refused, exactly as any
// other revoked session already is (orchestrator.go's own status
// allow-list), never silently accepted.
func TestBootstrapLaunch_GateDenialThenPostBetRefused(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)
	session, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)

	suspendAccount(t, pool, f)

	in := provider.BootstrapPayload(f.tenantID, token, "req-f3-gate-then-bet", game.ProviderGameID, "EUR", "real")
	v := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)
	result, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v)
	if err != nil {
		t.Fatalf("expected a denial result, not an error: %v", err)
	}
	if !result.Denied || result.DeniedReason != "rg_ineligible" {
		t.Fatalf("expected Denied=true reason=rg_ineligible, got %+v", result)
	}
	assertGateDenied(t, pool, f, session.ID, "rg_ineligible")

	betPayload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "req-f3-gate-then-bet-round", "", "round-f3-gate-then-bet", game.ProviderGameID,
		500, "EUR", OutcomeSucceeded, "", f.playerAccountID, session.ID)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", betPayload)
		return err
	})
	if !errors.Is(err, ErrLaunchSessionRequired) {
		t.Fatalf("expected a bet against a bootstrap-revoked session to be refused with ErrLaunchSessionRequired, got: %v", err)
	}
}
