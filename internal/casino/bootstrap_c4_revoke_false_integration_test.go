//go:build integration

// CAS-PLAY-BOOTSTRAP-1 (ADR 0103) - code review C-4 (re-run of e810ece):
// "revoke returns false" (RevokeLaunchSession's CAS matches zero rows even
// though the row lock read 'active') was previously argued structurally
// unreachable and left untested. The reviewer does not accept "needs a
// seam" for this one: on a SCRATCH database only (never the shared test
// database - DDL here never touches it), a test-only BEFORE UPDATE trigger
// on casino_launch_sessions that returns NULL exactly when NEW.status =
// 'revoked' forces RevokeLaunchSession's own UPDATE to affect zero rows -
// Postgres skips the write for a row whose BEFORE ROW trigger returns NULL
// - with NO production code change at all. Mutant MR (the
// prior/revoked invariant check removed from BootstrapLaunch) survived
// because nothing forced this path.
package casino

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// installTestC4BlockRevokeTrigger installs the test-only trigger described
// above on the scratch pool's casino_launch_sessions table.
func installTestC4BlockRevokeTrigger(t *testing.T, pool *db.Pool) {
	t.Helper()
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			CREATE FUNCTION test_c4_block_revoke() RETURNS TRIGGER AS $$
			BEGIN
				IF NEW.status = 'revoked' THEN
					RETURN NULL;
				END IF;
				RETURN NEW;
			END;
			$$ LANGUAGE plpgsql`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			CREATE TRIGGER test_c4_block_revoke_trigger
				BEFORE UPDATE ON casino_launch_sessions
				FOR EACH ROW EXECUTE FUNCTION test_c4_block_revoke()`)
		return err
	})
	if err != nil {
		t.Fatalf("install test-only block-revoke trigger: %v", err)
	}
}

// TestBootstrapLaunch_RevokeReturnsFalse_InvariantBroken forces C-4's own
// scenario on a scratch database and asserts BootstrapLaunch returns
// ErrBootstrapInvariantBroken (mapped to 5xx at the HTTP layer) with the
// session left completely untouched by the aborted revoke - never a
// silent denial, never a partial write.
func TestBootstrapLaunch_RevokeReturnsFalse_InvariantBroken(t *testing.T) {
	pool, _ := migration0099Scratch(t, "cas_c4_", 115)
	installTestC4BlockRevokeTrigger(t, pool)

	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 10000)
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	session, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)

	// A definitive gate denial (RG-ineligible, via a suspended account) is
	// the ONLY path that calls RevokeLaunchSession from BootstrapLaunch -
	// this is what drives the test-only trigger above.
	suspendAccount(t, pool, f)

	in := provider.BootstrapPayload(f.tenantID, token, "req-c4-revoke-false-1", game.ProviderGameID, "EUR", "real")
	v := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)
	_, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v)

	if !errors.Is(err, ErrBootstrapInvariantBroken) {
		t.Fatalf("expected ErrBootstrapInvariantBroken (mapped to 5xx), got: %v", err)
	}

	// The session must be left EXACTLY as it was before this call - the
	// whole transaction (including the step-5 CAS this denial path never
	// even reaches, since the gate check runs first) rolled back, so
	// status is still 'active' and no denial audit or bootstrap row
	// exists.
	assertSessionUntouchedAndNoBootstrapRow(t, pool, f, session.ID, LaunchSessionActive)
	if auditActionExists(t, pool, f.tenantID, "casino.launch_bootstrap_denied") {
		t.Fatal("expected NO casino.launch_bootstrap_denied audit record - the denial never committed")
	}
}
