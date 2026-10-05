//go:build integration

// PRH-2 R2 (migration 0116, ADR 0108, TRIGGER-SEARCH-PATH-1): the K2 four-eyes
// bypass reproduced by the security review (k3-delta-security.md finding 1) -
// the runtime role TEMP-shadowing staff_users / staff_capability_grants /
// staff_capability_grant_requests to forge a two-person approval of a real
// compensating debit - must now fail at the TEMP creation step; and a genuine
// two-person adjustment must still work, exactly once. Everything below talks
// to the database as the REAL runtime role (TEST_RUNTIME_DATABASE_URL), asserted
// neither superuser nor BYPASSRLS; fixtures use the owner pool.
package adjustment

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func tempRevokeRuntimePool(t *testing.T) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL not set; skipping runtime-role test")
	}
	rt, err := db.Connect(context.Background(), url, 2, 5*time.Second)
	if err != nil {
		t.Fatalf("connect runtime role: %v", err)
	}
	t.Cleanup(rt.Close)
	var super, bypass bool
	if err := rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass)
	}); err != nil {
		t.Fatal(err)
	}
	if super || bypass {
		t.Fatalf("must run as a role that is neither superuser nor BYPASSRLS (super=%v bypassrls=%v)", super, bypass)
	}
	return rt
}

// adjustmentLedgerTxCount counts manual_adjustment ledger transactions that have
// a leg on the world's wallet (read through the owner pool).
func (w *world) adjustmentLedgerTxCount() int {
	w.t.Helper()
	var n int
	if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(DISTINCT lt.id) FROM ledger_transactions lt
			JOIN ledger_entries le ON le.ledger_transaction_id = lt.id
			JOIN ledger_accounts la ON la.id = le.ledger_account_id
			WHERE lt.transaction_type = 'manual_adjustment' AND la.wallet_id = $1`, w.Wallet).Scan(&n)
	}); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// (3) The attack. The three TEMP shadows cannot even be created by the runtime
// role (42501). As defence in depth the forged identities, with no shadow in
// place, are refused by the real guards, and no compensating entry is posted.
func TestTempRevoke_K2ShadowAttack_FailsAtTempCreation_NoCompensatingEntry(t *testing.T) {
	rt := tempRevokeRuntimePool(t)
	w := newWorld(t, worldOpts{base: 1})
	causation := w.fund(10_000)
	before := w.playerCash()

	X, Y := uuid.New(), uuid.New()
	PX, PY := uuid.New(), uuid.New()
	for _, q := range []string{
		`CREATE TEMP TABLE staff_users AS SELECT * FROM public.staff_users WITH NO DATA`,
		`CREATE TEMP TABLE staff_capability_grants AS SELECT * FROM public.staff_capability_grants WITH NO DATA`,
		`CREATE TEMP TABLE staff_capability_grant_requests AS SELECT * FROM public.staff_capability_grant_requests WITH NO DATA`,
	} {
		err := rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q)
			return err
		})
		if err == nil {
			t.Fatalf("the RUNTIME role created a TEMP shadow (%s): the K2 four-eyes bypass is open", q)
		}
		requireCode(t, err, "42501")
	}

	// Defence in depth: forged identities (no public.staff_users row) are refused
	// and nothing is posted.
	svc := NewService(rt)
	h := evidenceFor(ReasonCompensatingEntry)
	in := SubmitInput{WalletID: w.Wallet, AssetCode: w.Asset, Direction: DirectionDebitPlayer, Amount: 300,
		ReasonCode: ReasonCompensatingEntry, CausationTransactionID: &causation, EvidenceRefHash: h, Note: "forged"}
	fx := staffMember{ID: X, PersonID: PX, TenantID: w.Tenant, Role: "finance"}
	fy := staffMember{ID: Y, PersonID: PY, TenantID: w.Tenant, Role: "finance"}
	req, err := svc.Submit(ctxFor(fx), w.target(), in, Meta{RequestID: "temp-revoke"})
	if err == nil {
		out, derr := svc.Decide(ctxFor(fy), w.target(), req.ID,
			DecisionInput{Decision: DecisionApprove, PayloadHash: req.PayloadHash, ReasonCode: "temp-revoke"}, Meta{RequestID: "temp-revoke"})
		if derr == nil && out.Executed {
			t.Fatal("two forged staff executed a compensating debit")
		}
		t.Fatalf("a forged initiator was accepted by Submit (state %s); decide err=%v", req.State, derr)
	}
	if got := w.adjustmentLedgerTxCount(); got != 0 {
		t.Fatalf("a compensating ledger transaction exists after the attack: %d", got)
	}
	if got := w.playerCash(); got != before {
		t.Fatalf("player_cash changed %d -> %d", before, got)
	}
	w.assertInvariants()
}

// (4) Positive: a genuine two-person adjustment, driven through the RUNTIME role,
// still submits, is approved by the second Person and executes exactly once;
// self-approval and single-person attempts remain refused.
func TestTempRevoke_GenuineTwoPersonAdjustment_StillWorks_ExactlyOnce(t *testing.T) {
	rt := tempRevokeRuntimePool(t)
	w := newWorld(t, worldOpts{base: 1})
	w.svc = NewService(rt) // the service speaks as the runtime role; fixtures stay on the owner pool
	causation := w.fund(10_000)

	in := w.credit(300, ReasonCompensatingEntry)
	in.Direction = DirectionDebitPlayer
	in.CausationTransactionID = &causation
	r, err := w.submit(w.F1, in)
	if err != nil {
		t.Fatalf("genuine submit as the runtime role: %v", err)
	}
	if r.State != StatePending || r.RequiredAtSubmission != 1 {
		t.Fatalf("unexpected pinned request: %+v", r)
	}
	// Single person / self-approval: refused, nothing executed.
	if _, err := w.decide(w.F1, r, DecisionApprove); pgCode(err) != "MA031" {
		t.Fatalf("self-approval: want MA031, got %v", err)
	}
	if got := w.adjustmentLedgerTxCount(); got != 0 {
		t.Fatalf("self-approval posted %d transactions", got)
	}
	// A staff member with no grant cannot approve either.
	if _, err := w.decide(w.FinanceNoGrant, r, DecisionApprove); err == nil {
		t.Fatal("a staff member without the approve grant approved")
	}
	// The second, distinct Person approves: executed in the approval's own tx.
	out, err := w.decide(w.F2, r, DecisionApprove)
	if err != nil {
		t.Fatalf("genuine approval as the runtime role: %v", err)
	}
	if !out.Executed || out.Request.State != StateExecuted || out.Request.LedgerTransactionID == nil {
		t.Fatalf("expected executed, got %+v", out)
	}
	if got := w.playerCash(); got != 9_700 {
		t.Fatalf("player_cash after the debit: want 9700 got %d", got)
	}
	// Exactly once: a replayed decision does not execute again.
	if _, err := w.decide(w.F3, r, DecisionApprove); err == nil {
		t.Fatal("a decision on an already-executed request was accepted")
	} else if errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if got := w.adjustmentLedgerTxCount(); got != 1 {
		t.Fatalf("want exactly 1 manual_adjustment transaction, got %d", got)
	}
	if got := w.playerCash(); got != 9_700 {
		t.Fatalf("player_cash after the replay: want 9700 got %d", got)
	}
	w.assertPostingKeys(out.Request)
	w.assertInvariants()
}
