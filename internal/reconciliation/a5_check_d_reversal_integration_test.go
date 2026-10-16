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

// Parity with check A on the 'reversed' arm (owner relabels past the state guard): a FAILED request
// relabelled 'reversed' keeps a withdrawal_failed release, which is not the completion shape (WD033); a
// COMPLETED request relabelled 'reversed' without a reversal link is WD035. D reports check A's code.
func TestA5D_ReversedArm_ParityWithCheckA(t *testing.T) {
	pool := testPool(t)
	rt := wdpath.RuntimeFor(t, pool)
	f := seedFixture(t, pool)
	failed := dWithdrawal(t, pool, f, 10, withdrawal.StateFailed)
	ownerRelabel(t, pool, f, failed, `state = 'reversed'`)
	completed := dWithdrawal(t, pool, f, 10, withdrawal.StateCompleted)
	ownerRelabel(t, pool, f, completed, `state = 'reversed'`)
	// A release whose ENTRIES have the completion shape (Dr hold / Cr psp_clearing, this request) but whose
	// TYPE and key are not a completion's (an owner-forged leg): only the type/key rule of the 'reversed'
	// release arm sees it (WD033).
	forgedReq := dWithdrawal(t, pool, f, 10, withdrawal.StateCompleted)
	var forged uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return ownerTamperHoldLegs(ctx, tx, func() error {
			res, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: f.tenantID, TransactionType: ledger.TxWithdrawalRejected,
				IdempotencyKey: forgedReq.String() + ":forged", CorrelationID: forgedReq,
				Entries: []ledger.EntryInput{{LedgerAccountID: holdAccount(t, pool, f), Direction: ledger.Debit, Amount: 10},
					{LedgerAccountID: f.clearingID, Direction: ledger.Credit, Amount: 10}}})
			forged = res.TransactionID
			return err
		})
	}); err != nil {
		t.Fatalf("owner tamper plant (forged release): %v", err)
	}
	ownerRelabel(t, pool, f, forgedReq, `state = 'reversed', release_ledger_transaction_id = '`+forged.String()+`'`)

	// A reversal link to a withdrawal_reversed with the right header but the wrong ENTRIES (11 instead of the
	// request's 10, balanced), planted with the 0162 commit check off: only the entry rule sees it (WD035).
	badEntries := dWithdrawal(t, pool, f, 10, withdrawal.StateCompleted)
	var badRev uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var completion uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT release_ledger_transaction_id FROM withdrawal_requests WHERE id = $1`, badEntries).Scan(&completion); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `ALTER TABLE ledger_transactions DISABLE TRIGGER ledger_transactions_withdrawal_hold_leg_at_commit`); err != nil {
			return err
		}
		pid, ret := "wdpath-psp", "bad-ret-"+badEntries.String()
		res, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: f.tenantID, TransactionType: ledger.TxWithdrawalReversed,
			IdempotencyKey: badEntries.String() + ":reversed", ProviderID: &pid, ProviderTxID: &ret, CorrelationID: badEntries,
			ReversesTransactionID: &completion,
			Entries: []ledger.EntryInput{{LedgerAccountID: f.clearingID, Direction: ledger.Debit, Amount: 11},
				{LedgerAccountID: f.cashAccountID, Direction: ledger.Credit, Amount: 11}}})
		if err != nil {
			return err
		}
		badRev = res.TransactionID
		_, err = tx.Exec(ctx, `ALTER TABLE ledger_transactions ENABLE TRIGGER ledger_transactions_withdrawal_hold_leg_at_commit`)
		return err
	}); err != nil {
		t.Fatalf("owner tamper plant (bad reversal entries): %v", err)
	}
	ownerRelabel(t, pool, f, badEntries, `state = 'reversed', reversal_ledger_transaction_id = '`+badRev.String()+`'`)

	o := dSweep(t, rt, f)
	if o.Err != nil || o.Run.Status != StatusMismatchesFound {
		t.Fatalf("relabelled requests must be found: %s err=%v", o.Run.Status, o.Err)
	}
	got := map[string]string{}
	for _, r := range dKind(dMismatches(t, pool, f), MismatchKindWithdrawalRequestLedgerLinkInvalid) {
		got[r.key] = r.actual
	}
	for name, c := range map[string]struct {
		id   uuid.UUID
		want string
	}{"failed relabelled": {failed, "WD033"}, "completed relabelled, no link": {completed, "WD035"},
		"forged completion-shaped release": {forgedReq, "WD033"}, "reversal with the wrong entries": {badEntries, "WD035"}} {
		var v []string
		if err := rt.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT withdrawal_request_ledger_link_violation($1, $2)`, c.id, f.tenantID).Scan(&v)
		}); err != nil {
			t.Fatal(err)
		}
		if len(v) == 0 || v[0] != c.want {
			t.Fatalf("%s: check A = %v, want %s", name, v, c.want)
		}
		if !strings.HasPrefix(got[c.id.String()], "violation="+c.want+" ") {
			t.Fatalf("%s: D must report %s, got %q", name, c.want, got[c.id.String()])
		}
	}
}

// LF-A5-5: the detective twin of WD026. A well-shaped reversal (check A passes) on a request with NO
// 'reverse' decisions, planted by the owner with the state guard switched off, is a standing finding
// (violation=WD026); a governed reversal whose voter is LATER suspended is not (no false P1).
func TestA5D_ReversedWithoutDecisions_IsWD026(t *testing.T) {
	pool := testPool(t)
	rt := wdpath.RuntimeFor(t, pool)
	f := seedFixture(t, pool)
	governed := dWithdrawal(t, pool, f, 10, withdrawal.StateCompleted)
	s1 := a5DStaff(t, pool, f.tenantID)
	a5DReverse(t, rt, f, governed, s1, a5DStaff(t, pool, f.tenantID))
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, s1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	wdpath.SkipOnPersistentDatabase(t, pool)
	planted := dWithdrawal(t, pool, f, 10, withdrawal.StateCompleted)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var completion uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT release_ledger_transaction_id FROM withdrawal_requests WHERE id = $1`, planted).Scan(&completion); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `ALTER TABLE withdrawal_requests DISABLE TRIGGER withdrawal_requests_aa_state_transition_guard`); err != nil {
			return err
		}
		pid, ret := "wdpath-psp", "plant-ret-"+planted.String()
		res, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: f.tenantID, TransactionType: ledger.TxWithdrawalReversed,
			IdempotencyKey: planted.String() + ":reversed", ProviderID: &pid, ProviderTxID: &ret, CorrelationID: planted,
			ReversesTransactionID: &completion,
			Entries: []ledger.EntryInput{{LedgerAccountID: f.clearingID, Direction: ledger.Debit, Amount: 10},
				{LedgerAccountID: f.cashAccountID, Direction: ledger.Credit, Amount: 10}}})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'reversed', reversal_ledger_transaction_id = $1 WHERE id = $2`, res.TransactionID, planted); err != nil {
			return err
		}
		// Fire the deferred link checks (A, WD043/WD044) now: ALTER TABLE refuses with pending trigger events.
		if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `ALTER TABLE withdrawal_requests ENABLE TRIGGER withdrawal_requests_aa_state_transition_guard`)
		return err
	}); err != nil {
		t.Fatalf("owner tamper plant (reversal without decisions): %v", err)
	}
	o := dSweep(t, rt, f)
	if o.Err != nil || o.Run.Status != StatusMismatchesFound {
		t.Fatalf("the plant must be found: %s err=%v", o.Run.Status, o.Err)
	}
	rows := dKind(dMismatches(t, pool, f), MismatchKindWithdrawalRequestLedgerLinkInvalid)
	if len(rows) != 1 || rows[0].key != planted.String() || !strings.HasPrefix(rows[0].actual, "violation=WD026 ") {
		t.Fatalf("want exactly the planted request reported WD026 (the governed one with a later-suspended voter is clean): %+v", rows)
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
