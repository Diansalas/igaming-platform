//go:build integration

// CAS-PLAY-BOOTSTRAP-1, SB-1/BS-0 (ADR 0103 §3.2 step 2a, §9): Redeem+
// Recheck must be the FIRST statement BootstrapLaunch's own transaction
// runs - a credential handle revoked between phase-1 verification and the
// domain transaction gets the uniform credential_unavailable, with NOTHING
// else read or written. Reuses internal/casino's own ADR 0094 §9.3
// recheck-world fixture (resolution_recheck_integration_test.go) and the
// phasecapture test-support package that already proves this property for
// the ordinary bet/win/rollback callback path - the same infrastructure,
// pointed at BootstrapLaunch instead of ReceiveVerifiedCallback.
//
// This is also the kill test for the MUT list's "move Redeem+Recheck
// after the session lookup (SB-1)" mutant: moving the session SELECT
// before the recheck would make it the FIRST recorded statement, so
// stmts[0] would no longer be providercred.HandleRecheckSQL and/or
// len(stmts) would be > 1 even on the revoked-handle path (a session
// SELECT would run and be recorded before the recheck failure).
package casino

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/providercred"
	"github.com/Diansalas/igaming-platform/internal/providercred/providercredtest"
	"github.com/Diansalas/igaming-platform/internal/testsupport/phasecapture"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// bootstrapTenantTxRunner implements providercred.TenantTxRunner
// (BootstrapLaunch's own pool parameter type), wrapping the REAL
// transaction's pgx.Tx in a phasecapture.Tx so every statement
// BootstrapLaunch's transaction runs is recorded, in order - exactly
// recheckWorld.receive's own technique, adapted for BootstrapLaunch's
// self-managed transaction boundary (ReceiveVerifiedCallback instead
// takes an already-open tx directly, which is why that path's own helper
// cannot be reused unmodified here).
type bootstrapTenantTxRunner struct {
	real *db.Pool
	out  **phasecapture.Tx
}

func (r bootstrapTenantTxRunner) WithTenant(ctx context.Context, tenantID uuid.UUID, fn db.TxFunc) error {
	return r.real.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		captured := phasecapture.NewTx(tx)
		*r.out = captured
		return fn(ctx, captured)
	})
}

func TestBootstrapLaunch_SB1_RedeemRecheckIsFirstStatement(t *testing.T) {
	w := newRecheckWorld(t)
	game := seedGame(t, w.pool, "mock-casino", "EUR")
	session, token := mintSessionForBootstrap(t, w.pool, w.f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)

	in := w.provider.BootstrapPayload(w.f.tenantID, token, "req-sb1-1", game.ProviderGameID, "EUR", "real")
	// Signed with the REAL credential's secret (the recheck-world's own
	// registered handle), not the mock provider's own self-derived key -
	// mirrors recheckWorld.bet's identical technique, since this test's
	// whole point is exercising the REAL resolver's Recheck.
	in.Header = in.Header.Clone()
	webhookauth.CasinoScheme().SetHeaders(in.Header, webhookauth.MockKeyID,
		webhookauth.CasinoScheme().Sign(w.secret, w.f.tenantID, "mock-casino", webhookauth.MockKeyID, in.Body))

	v, err := w.orch.VerifyCallback(context.Background(), w.pool, w.f.tenantID, "mock-casino", in)
	if err != nil {
		t.Fatalf("phase 1 must verify: %v", err)
	}

	providercredtest.Revoke(t, w.pool, w.f.tenantID, w.handle.ID, w.principals.Requester)

	var captured *phasecapture.Tx
	runner := bootstrapTenantTxRunner{real: w.pool, out: &captured}
	_, err = w.orch.BootstrapLaunch(context.Background(), runner, w.f.tenantID, "mock-casino", v)
	wantCredentialUnavailable(t, err)

	stmts := captured.Statements()
	if len(stmts) != 1 || stmts[0] != providercred.HandleRecheckSQL {
		t.Fatalf("domain statements = %q, want only the failed re-check", stmts)
	}

	var status LaunchSessionStatus
	var bootstrapCount int
	err = w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT status FROM casino_launch_sessions WHERE id = $1`, session.ID).Scan(&status); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM casino_launch_bootstraps WHERE launch_session_id = $1`, session.ID).Scan(&bootstrapCount)
	})
	if err != nil {
		t.Fatalf("read post-refusal state: %v", err)
	}
	if status != LaunchSessionActive {
		t.Fatalf("expected the session untouched (still active), got %q", status)
	}
	if bootstrapCount != 0 {
		t.Fatalf("expected no bootstrap row, got %d", bootstrapCount)
	}
}
