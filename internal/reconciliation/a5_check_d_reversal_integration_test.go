//go:build integration

// A5 / WITHDRAWAL-REVERSAL-1: reconciliation check D sees reversals. A governed reversal (two distinct
// Persons, the real Go primitives, as igaming_runtime) reconciles clean; an owner-planted
// withdrawal_reversed that no request links (the 0162 commit trigger switched off: ADR 0110 s9.4) is a
// standing withdrawal_request_ledger_link_invalid finding (violation=WD044) and raises the D P1.
package reconciliation

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/stepup"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
	"github.com/Diansalas/igaming-platform/internal/testsupport/wdpath"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

func a5DStaff(t *testing.T, pool *db.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	person, id := uuid.New(), uuid.New()
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id)
			VALUES ($1, $2, $3, 'x', 'finance', 'active', $4)`, id, tenantID, id.String()+"@example.com", person)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func a5DReverse(t *testing.T, rt *db.Pool, f fixture, id uuid.UUID, approvers ...uuid.UUID) {
	t.Helper()
	p := withdrawal.PayoutReversal{ProviderID: "wdpath-psp", OriginalProviderReference: "wdpath-tx-" + id.String(),
		ReturnReference: "ret-" + id.String(), Amount: 10, AssetCode: "EUR", EvidenceRefHash: strings.Repeat("5c", 32)}
	no := func(uuid.UUID) (bool, error) { return false, nil }
	ok := func(uuid.UUID) (bool, bool, error) { return true, true, nil }
	if err := rt.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var d withdrawal.ReversalDecision
		for _, a := range approvers {
			var err error
			if d, err = withdrawal.RecordPayoutReversalDecision(ctx, tx, id, a, p, no, ok, stepup.Assertion{}); err != nil {
				return err
			}
		}
		if !d.Ready {
			t.Fatalf("setup: the quorum is not reached: %+v", d)
		}
		_, err := withdrawal.ReverseCompletedPayout(ctx, tx, id, p)
		return err
	}); err != nil {
		t.Fatalf("governed reversal: %v", err)
	}
}

func TestA5D_GovernedReversalIsClean_OrphanReversalIsWD044(t *testing.T) {
	pool := testPool(t)
	rt := wdpath.RuntimeFor(t, pool)
	f := seedFixture(t, pool)
	reversed := dWithdrawal(t, pool, f, 10, withdrawal.StateCompleted)
	a5DReverse(t, rt, f, reversed, a5DStaff(t, pool, f.tenantID), a5DStaff(t, pool, f.tenantID))
	dWithdrawal(t, pool, f, 10, withdrawal.StateRequested)

	o := dSweep(t, rt, f)
	if o.Err != nil || o.Run.Status != StatusClean {
		t.Fatalf("a governed reversal must reconcile clean: %s err=%v rows=%+v", o.Run.Status, o.Err, dMismatches(t, pool, f))
	}
	var v []string
	if err := rt.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT withdrawal_request_ledger_link_violation($1, $2)`, reversed, f.tenantID).Scan(&v)
	}); err != nil || len(v) != 0 {
		t.Fatalf("check A on the reversed request: %v %v", v, err)
	}

	// Owner tamper plant: a withdrawal_reversed on another completed request, the 0162 commit check off.
	wdpath.SkipOnPersistentDatabase(t, pool)
	victim := dWithdrawal(t, pool, f, 10, withdrawal.StateCompleted)
	var orphan uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var completion uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT release_ledger_transaction_id FROM withdrawal_requests WHERE id = $1`, victim).Scan(&completion); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `ALTER TABLE ledger_transactions DISABLE TRIGGER ledger_transactions_withdrawal_hold_leg_at_commit`); err != nil {
			return err
		}
		pid, ret := "wdpath-psp", "orphan-ret-"+victim.String()
		res, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: f.tenantID, TransactionType: ledger.TxWithdrawalReversed,
			IdempotencyKey: victim.String() + ":reversed", ProviderID: &pid, ProviderTxID: &ret, CorrelationID: victim,
			ReversesTransactionID: &completion,
			Entries: []ledger.EntryInput{{LedgerAccountID: f.clearingID, Direction: ledger.Debit, Amount: 10},
				{LedgerAccountID: f.cashAccountID, Direction: ledger.Credit, Amount: 10}}})
		if err != nil {
			return err
		}
		orphan = res.TransactionID
		_, err = tx.Exec(ctx, `ALTER TABLE ledger_transactions ENABLE TRIGGER ledger_transactions_withdrawal_hold_leg_at_commit`)
		return err
	}); err != nil {
		t.Fatalf("owner tamper plant (orphan reversal): %v", err)
	}
	o = dSweep(t, rt, f)
	if o.Err != nil || o.Run.Status != StatusMismatchesFound {
		t.Fatalf("an orphan reversal must be found: %s err=%v", o.Run.Status, o.Err)
	}
	found := false
	for _, r := range dKind(dMismatches(t, pool, f), MismatchKindWithdrawalRequestLedgerLinkInvalid) {
		if r.key == "ledger_transaction:"+orphan.String() && strings.HasPrefix(r.actual, "violation=WD044 ") {
			found = true
		} else {
			t.Fatalf("only the orphan may be reported: %+v", r)
		}
	}
	if !found {
		t.Fatal("the orphan withdrawal_reversed must be reported WD044")
	}
	if !hasHoldAlert(alertinject.ForSubject(t, pool, f.tenantID)) {
		t.Fatal("the D finding raises the D P1")
	}
}
