//go:build integration

// ADR 0094 §5 / §9.3 tests 8 and 10 (casino, the synchronous bet money
// path), with the REAL resolver: the domain transaction re-checks the
// handle that verified (HandleRecheckSQL) before anything is read or
// written, and the two-phase token is single-use and bound to its tenant.
// QA §11 item 6: a DB error on the re-check rolls the domain transaction
// back exactly like a clean miss. All tests share the ADR 0094 pool-10
// fixture (QA §11 items 1 and 8).
package casino

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/providercred"
	"github.com/Diansalas/igaming-platform/internal/providercred/providercredtest"
	"github.com/Diansalas/igaming-platform/internal/testsupport/phasecapture"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

type recheckWorld struct {
	pool       *db.Pool
	f          casinoFixture
	provider   *MockCasinoProvider
	orch       *Orchestrator
	handle     providercred.Handle
	secret     []byte
	session    uuid.UUID
	principals providercredtest.Principals
}

func newRecheckWorld(t *testing.T) *recheckWorld {
	t.Helper()
	pool := phasecapture.Pool10(t, "TEST_DATABASE_URL")
	w := &recheckWorld{pool: pool, f: seedCasinoFixture(t, pool), provider: NewMockCasinoProvider("mock-casino", "EUR")}
	fundWallet(t, pool, w.f, 5000)
	registerCasinoCapability(t, pool, w.f, w.provider, 100)
	w.session = mintSession(t, pool, w.f, "mock-casino", "EUR")
	sub, mem := realCredentialSubsystem(t)
	w.principals = providercredtest.SeedPrincipals(t, pool)
	w.handle, w.secret = providercredtest.Register(t, pool, sub, mem.Put, w.principals, providercredtest.Spec{
		TenantID: w.f.tenantID, Domain: "casino", ProviderID: "mock-casino", Purpose: providercred.PurposeWebhookVerify, KeyID: webhookauth.MockKeyID,
	})
	w.orch = NewOrchestrator(map[string]CasinoProvider{"mock-casino": w.provider}, sub.Resolver("casino"))
	return w
}

func (w *recheckWorld) bet(ref string) webhookauth.Inbound {
	in := w.provider.CallbackPayload(w.f.tenantID, CallbackEventBet, ref, "", "round-"+ref, "game-1",
		1000, "EUR", OutcomeSucceeded, "", w.f.playerAccountID, w.session)
	in.Header = in.Header.Clone()
	webhookauth.CasinoScheme().SetHeaders(in.Header, webhookauth.MockKeyID,
		webhookauth.CasinoScheme().Sign(w.secret, w.f.tenantID, "mock-casino", webhookauth.MockKeyID, in.Body))
	return in
}

func (w *recheckWorld) verify(t *testing.T, in webhookauth.Inbound) *VerifiedCallback {
	t.Helper()
	v, err := w.orch.VerifyCallback(context.Background(), w.pool, w.f.tenantID, "mock-casino", in)
	if err != nil {
		t.Fatalf("phase 1 must verify: %v", err)
	}
	return v
}

// receive runs phase 2 under txTenant's RLS with tenantID as the route
// value, optionally wrapping the domain transaction; it returns the domain
// statements.
func (w *recheckWorld) receive(txTenant, tenantID uuid.UUID, v *VerifiedCallback, wrap func(pgx.Tx) pgx.Tx) ([]string, error) {
	var captured *phasecapture.Tx
	err := w.pool.WithTenant(context.Background(), txTenant, func(ctx context.Context, tx pgx.Tx) error {
		captured = phasecapture.NewTx(tx)
		var inner pgx.Tx = captured
		if wrap != nil {
			inner = wrap(captured)
		}
		_, err := w.orch.ReceiveVerifiedCallback(ctx, inner, tenantID, "mock-casino", v)
		return err
	})
	return captured.Statements(), err
}

func (w *recheckWorld) ledgerRows(t *testing.T, ref string) int {
	t.Helper()
	var n int
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE provider_id = 'mock-casino' AND provider_tx_id = $1`, ref).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func (w *recheckWorld) transition(t *testing.T, action string, notAfter *time.Time, reason string) {
	t.Helper()
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := providercred.TransitionHandle(ctx, tx, w.handle.ID, action, notAfter, reason, w.principals.Requester,
			providercred.AuditContext{ActorID: w.principals.Requester})
		return err
	}); err != nil {
		t.Fatalf("transition %s: %v", action, err)
	}
}

func wantCredentialUnavailable(t *testing.T, err error) {
	t.Helper()
	var authErr *webhookauth.AuthError
	if !errors.As(err, &authErr) || authErr.Reason != webhookauth.ReasonCredentialUnavailable {
		t.Fatalf("want the uniform credential_unavailable, got %v", err)
	}
}

func (w *recheckWorld) assertNothingWritten(t *testing.T, ref string) {
	t.Helper()
	if n := w.ledgerRows(t, ref); n != 0 {
		t.Fatalf("a rejected callback wrote %d ledger transactions", n)
	}
	if b := cashBalance(t, w.pool, w.f); b != 5000 {
		t.Fatalf("a rejected callback moved the balance to %d", b)
	}
	if debits, credits := sumDebitsCredits(t, w.pool, w.f.tenantID); debits != credits {
		t.Fatalf("SUM(debits)=%d != SUM(credits)=%d", debits, credits)
	}
}

// TestReceiveVerified_RevokedBetweenVerifyAndDomainTx_Casino is ADR 0094
// §9.3 test 8 (casino): revoked between the phases is rejected with 0
// rows; rotated to verify_only inside its window is accepted and posts
// once; a not_after that passes between the phases is rejected.
func TestReceiveVerified_RevokedBetweenVerifyAndDomainTx_Casino(t *testing.T) {
	t.Run("revoked between the phases", func(t *testing.T) {
		w := newRecheckWorld(t)
		v := w.verify(t, w.bet("rv-revoked"))
		providercredtest.Revoke(t, w.pool, w.f.tenantID, w.handle.ID, w.principals.Requester)
		stmts, err := w.receive(w.f.tenantID, w.f.tenantID, v, nil)
		wantCredentialUnavailable(t, err)
		if len(stmts) != 1 || stmts[0] != providercred.HandleRecheckSQL {
			t.Fatalf("domain statements = %q, want only the failed re-check", stmts)
		}
		w.assertNothingWritten(t, "rv-revoked")
	})
	t.Run("rotated to verify_only inside its window", func(t *testing.T) {
		w := newRecheckWorld(t)
		v := w.verify(t, w.bet("rv-rotated"))
		na := time.Now().Add(time.Hour)
		w.transition(t, providercred.ActionVerifyOnly, &na, providercred.TransitionReasonRotation)
		if _, err := w.receive(w.f.tenantID, w.f.tenantID, v, nil); err != nil {
			t.Fatalf("a verify_only handle inside its window must still be accepted: %v", err)
		}
		if n := w.ledgerRows(t, "rv-rotated"); n != 1 {
			t.Fatalf("want exactly one posting, got %d", n)
		}
		if b := cashBalance(t, w.pool, w.f); b != 4000 {
			t.Fatalf("balance = %d, want 4000", b)
		}
	})
	t.Run("not_after passed between the phases", func(t *testing.T) {
		w := newRecheckWorld(t)
		na := time.Now().Add(1500 * time.Millisecond)
		w.transition(t, providercred.ActionVerifyOnly, &na, providercred.TransitionReasonRotation)
		v := w.verify(t, w.bet("rv-expired"))
		time.Sleep(time.Until(na) + 200*time.Millisecond)
		_, err := w.receive(w.f.tenantID, w.f.tenantID, v, nil)
		wantCredentialUnavailable(t, err)
		w.assertNothingWritten(t, "rv-expired")
	})
}

// recheckErrTx fails exactly the re-check statement with a database error
// (QA §11 item 6), passing everything else through.
type recheckErrTx struct{ pgx.Tx }

type errRow struct{}

func (errRow) Scan(...any) error { return errors.New("injected: connection reset during re-check") }

func (r recheckErrTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if sql == providercred.HandleRecheckSQL {
		return errRow{}
	}
	return r.Tx.QueryRow(ctx, sql, args...)
}

// TestReceiveVerified_RecheckDBErrorRollsBack is QA §11 item 6: a DB error
// on HandleRecheckSQL (not a clean miss) fails closed exactly like the
// revoked case and the domain transaction writes nothing.
func TestReceiveVerified_RecheckDBErrorRollsBack(t *testing.T) {
	w := newRecheckWorld(t)
	v := w.verify(t, w.bet("rv-dberr"))
	_, err := w.receive(w.f.tenantID, w.f.tenantID, v, func(tx pgx.Tx) pgx.Tx { return recheckErrTx{Tx: tx} })
	wantCredentialUnavailable(t, err)
	w.assertNothingWritten(t, "rv-dberr")
}

// TestVerifiedCallback_ZeroOrMismatchRejected_Casino is ADR 0094 §9.3 test
// 10 plus the security C5 database variants: a nil/zero token, a tenant or
// provider mismatch, reuse (C1), and a valid tenant-A token presented
// inside a WithTenant(B) transaction (RLS hides A's handle from the
// re-check) all fail closed with nothing written.
func TestVerifiedCallback_ZeroOrMismatchRejected_Casino(t *testing.T) {
	w := newRecheckWorld(t)
	other := seedCasinoFixture(t, w.pool)

	t.Run("nil and zero tokens: no statement at all", func(t *testing.T) {
		for _, v := range []*VerifiedCallback{nil, {}} {
			stmts, err := w.receive(w.f.tenantID, w.f.tenantID, v, nil)
			wantCredentialUnavailable(t, err)
			if len(stmts) != 0 {
				t.Fatalf("a zero token ran %q", stmts)
			}
		}
	})
	t.Run("tenant mismatch", func(t *testing.T) {
		v := w.verify(t, w.bet("mm-tenant"))
		stmts, err := w.receive(other.tenantID, other.tenantID, v, nil)
		wantCredentialUnavailable(t, err)
		if len(stmts) != 0 {
			t.Fatalf("a tenant-mismatched token ran %q", stmts)
		}
		w.assertNothingWritten(t, "mm-tenant")
	})
	t.Run("provider mismatch", func(t *testing.T) {
		v := w.verify(t, w.bet("mm-provider"))
		var err error
		_ = w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err = w.orch.ReceiveVerifiedCallback(ctx, tx, w.f.tenantID, "other-casino", v)
			return err
		})
		wantCredentialUnavailable(t, err)
		w.assertNothingWritten(t, "mm-provider")
	})
	t.Run("tenant-A token inside a WithTenant(B) transaction", func(t *testing.T) {
		v := w.verify(t, w.bet("mm-rls"))
		stmts, err := w.receive(other.tenantID, w.f.tenantID, v, nil)
		wantCredentialUnavailable(t, err)
		if len(stmts) != 1 || stmts[0] != providercred.HandleRecheckSQL {
			t.Fatalf("statements = %q, want only the re-check (0 rows under B's RLS)", stmts)
		}
		w.assertNothingWritten(t, "mm-rls")
	})
	t.Run("reuse (C1)", func(t *testing.T) {
		v := w.verify(t, w.bet("mm-reuse"))
		if _, err := w.receive(w.f.tenantID, w.f.tenantID, v, nil); err != nil {
			t.Fatalf("first redemption: %v", err)
		}
		stmts, err := w.receive(w.f.tenantID, w.f.tenantID, v, nil)
		wantCredentialUnavailable(t, err)
		if len(stmts) != 0 {
			t.Fatalf("a reused token ran %q", stmts)
		}
		if n := w.ledgerRows(t, "mm-reuse"); n != 1 {
			t.Fatalf("want exactly one posting, got %d", n)
		}
	})
}
