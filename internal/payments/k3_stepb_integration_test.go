//go:build integration

package payments

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/adjustment"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// openExposure parks a pending deposit attempt of the world's player with
// multiple_success_for_intent and no tombstone: the K2 MA020 open payment
// exposure (player_open_payment_exposure) becomes true for the player.
func (w *k3World) openExposure() uuid.UUID {
	w.t.Helper()
	res := rvInit(w.t, w.pool, w.orch, w.f.orchFixture, 3000, "k3-exp-"+uuid.NewString())
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		return ApplyDisputeFromNonTerminal(ctx, tx, res.Attempt.ID, EvidenceCallback, TerminalReasonMultipleSuccessForIntent)
	})
	if !w.hasExposure() {
		w.t.Fatal("setup: the player has no open payment exposure")
	}
	return res.Attempt.ID
}

func (w *k3World) hasExposure() bool {
	w.t.Helper()
	var b bool
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT player_open_payment_exposure($1, $2)`, w.f.tenantID, w.f.playerAccountID).Scan(&b)
	})
	return b
}

func (w *k3World) requestAudit(requestID uuid.UUID, action string) map[string]any {
	w.t.Helper()
	var raw []byte
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT metadata FROM audit_log WHERE tenant_id = $1 AND target_id = $2 AND action = $3 ORDER BY created_at DESC LIMIT 1`,
			w.f.tenantID, requestID.String(), action).Scan(&raw)
	})
	var md map[string]any
	if err := json.Unmarshal(raw, &md); err != nil {
		w.t.Fatal(err)
	}
	return md
}

// C-48 (MA020 exemption trio, ADR 0101 18.3; security Q-SEC-2 (a)-(d); LF Q-LF-2):
//
//	(1) a player with an open exposure receives the Step B credit;
//	(2) every other credit - including a compensating_entry with a non-Step-B
//	    causation - is still refused with MA020;
//	(3) the audit records open_payment_exposure_at_execution = true.
//
// Plus LF O-6: the exposure opening between submission and execution.
func TestK3_C48_MA020ExemptionTrio(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	w.k2Setup()
	_, a := w.ambiguousPayout(500)
	res := w.executeM2(a.ID, ResolutionM2DeclarePaid)
	w.openExposure()

	// (2) every other credit is refused with MA020 - at submission.
	h := k3EvidenceHash()
	nonStepB := []struct {
		name string
		in   adjustment.SubmitInput
	}{
		{"operational_error_correction credit, no causation", adjustment.SubmitInput{WalletID: w.f.walletID, AssetCode: "EUR",
			Direction: adjustment.DirectionCreditPlayer, Amount: 10, ReasonCode: adjustment.ReasonOperationalErrorCorrection, Note: "x"}},
		{"external_instruction credit", adjustment.SubmitInput{WalletID: w.f.walletID, AssetCode: "EUR",
			Direction: adjustment.DirectionCreditPlayer, Amount: 10, ReasonCode: adjustment.ReasonExternalInstruction, EvidenceRefHash: &h, Note: "x"}},
	}
	for _, c := range nonStepB {
		_, err := w.k2Service().Submit(k3Ctx(w.f3), w.k2Target(w.f3), c.in, adjustment.Meta{})
		if k3Code(err) != "MA020" {
			t.Errorf("%s: want MA020, got %v", c.name, err)
		}
	}
	// A compensating_entry credit whose causation is a NON-Step-B transaction
	// is refused too.
	// (a deposit causation is type-refused first; use the Step A-shaped release of
	// another withdrawal instead: a withdrawal_failed with a player_cash leg)
	wr2, a2 := w.ambiguousPayout(100)
	res2 := w.executeM2(a2.ID, ResolutionM2DeclareNotPaid)
	_ = wr2
	_, err := w.k2Submit(w.f3, adjustment.DirectionCreditPlayer, 10, *res2.LedgerTransactionID)
	if k3Code(err) != "MA020" {
		t.Errorf("compensating credit on a non-Step-B causation: want MA020, got %v", err)
	}

	// (1) the Step B credit is admitted, and (3) its audit records the exposure.
	r, err := w.k2Submit(w.f3, adjustment.DirectionCreditPlayer, 500, *res.LedgerTransactionID)
	if err != nil {
		t.Fatalf("the Step B credit was refused at submission: %v", err)
	}
	out, err := w.k2Decide(w.f4, r)
	if err != nil || !out.Executed {
		t.Fatalf("the Step B credit did not execute: %v %+v", err, out)
	}
	md := w.requestAudit(r.ID, "ledger_adjustment.executed")
	if v, ok := md["open_payment_exposure_at_execution"].(bool); !ok || !v {
		t.Fatalf("audit open_payment_exposure_at_execution = %v, want true", md["open_payment_exposure_at_execution"])
	}
	w.assertInvariants()

	// LF O-6: no exposure at submission, the exposure opens before execution - the
	// Step B credit still executes (audit true); a NON-Step-B credit is refused at
	// execution.
	w2 := newK3World(t, k3Opts{base: 1})
	w2.k2Setup()
	_, b := w2.ambiguousPayout(300)
	resB := w2.executeM2(b.ID, ResolutionM2DeclarePaid)
	rs, err := w2.k2Submit(w2.f3, adjustment.DirectionCreditPlayer, 300, *resB.LedgerTransactionID)
	if err != nil {
		t.Fatal(err)
	}
	w2.openExposure()
	if out, err := w2.k2Decide(w2.f4, rs); err != nil || !out.Executed {
		t.Fatalf("exposure opened between submission and execution blocked the Step B credit: %v %+v", err, out)
	}
	if md := w2.requestAudit(rs.ID, "ledger_adjustment.executed"); md["open_payment_exposure_at_execution"] != true {
		t.Fatalf("O-6 audit attribute = %v", md["open_payment_exposure_at_execution"])
	}
	// The same shape with no exposure records false.
	w3 := newK3World(t, k3Opts{base: 1})
	w3.k2Setup()
	_, c3 := w3.ambiguousPayout(300)
	resC := w3.executeM2(c3.ID, ResolutionM2DeclarePaid)
	rc, err := w3.k2Submit(w3.f3, adjustment.DirectionCreditPlayer, 300, *resC.LedgerTransactionID)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := w3.k2Decide(w3.f4, rc); err != nil || !out.Executed {
		t.Fatal(err)
	}
	if md := w3.requestAudit(rc.ID, "ledger_adjustment.executed"); md["open_payment_exposure_at_execution"] != false {
		t.Fatalf("no-exposure audit attribute = %v, want false", md["open_payment_exposure_at_execution"])
	}
}

// C-23 / C-24 / C-25 / C-27 (R): the Step B arm's own conditions. A non-prefixed
// withdrawal_completed (a real provider settlement), another wallet's Step B, a
// debit with a Step B causation, a credit without evidence, and a credit above
// the hold leg are refused by K2's payload rules.
func TestK3_C23_C27_StepBArmConditions(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	w.k2Setup()
	_, a := w.ambiguousPayout(500)
	res := w.executeM2(a.ID, ResolutionM2DeclarePaid)

	// C-23: a REAL (non-prefixed) withdrawal_completed is never an admissible causation.
	realWR, _ := w.payout(120)
	var realTx uuid.UUID
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		if err := withdrawal.Complete(ctx, tx, realWR.ID, w.provider, "real-provider-tx-"+uuid.NewString()); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT release_ledger_transaction_id FROM withdrawal_requests WHERE id = $1`, realWR.ID).Scan(&realTx)
	})
	if _, err := w.k2Submit(w.f3, adjustment.DirectionCreditPlayer, 10, realTx); k3Code(err) != "MA022" {
		t.Errorf("C-23 non-prefixed withdrawal_completed causation: want MA022, got %v", err)
	}
	// C-24: another wallet's Step B is not on this wallet.
	other := newK3WorldOn(t, w.pool, k3Opts{base: 1})
	_, oa := other.ambiguousPayout(200)
	oRes := other.executeM2(oa.ID, ResolutionM2DeclarePaid)
	h := k3EvidenceHash()
	if _, err := w.k2Service().Submit(k3Ctx(w.f3), w.k2Target(w.f3), adjustment.SubmitInput{WalletID: w.f.walletID, AssetCode: "EUR",
		Direction: adjustment.DirectionCreditPlayer, Amount: 10, ReasonCode: adjustment.ReasonCompensatingEntry,
		CausationTransactionID: oRes.LedgerTransactionID, EvidenceRefHash: &h, Note: "x"}, adjustment.Meta{}); k3Code(err) != "MA022" {
		t.Errorf("C-24 another wallet's Step B: want MA022, got %v", err)
	}
	// C-25: a DEBIT with a Step B causation is refused (the arm is credit only).
	if _, err := w.k2Submit(w.f3, adjustment.DirectionDebitPlayer, 10, *res.LedgerTransactionID); k3Code(err) != "MA022" {
		t.Errorf("C-25 debit with a Step B causation: want MA022, got %v", err)
	}
	// C-27: a compensating credit without an evidence hash is refused.
	if _, err := w.k2Service().Submit(k3Ctx(w.f3), w.k2Target(w.f3), adjustment.SubmitInput{WalletID: w.f.walletID, AssetCode: "EUR",
		Direction: adjustment.DirectionCreditPlayer, Amount: 10, ReasonCode: adjustment.ReasonCompensatingEntry,
		CausationTransactionID: res.LedgerTransactionID, Note: "x"}, adjustment.Meta{}); k3Code(err) != "MA022" {
		t.Errorf("C-27 no evidence: want MA022, got %v", err)
	}
	// The cap is the hold leg: 501 > 500 is refused; 500 is admitted.
	if _, err := w.k2Submit(w.f3, adjustment.DirectionCreditPlayer, 501, *res.LedgerTransactionID); k3Code(err) != "MA022" {
		t.Errorf("a credit above the hold leg: want MA022, got %v", err)
	}
	if _, err := w.k2Submit(w.f3, adjustment.DirectionCreditPlayer, 500, *res.LedgerTransactionID); err != nil {
		t.Errorf("a credit equal to the hold leg was refused: %v", err)
	}
}

// C-28 (R/AZ; section 26 RC-1): a Person counted on the M2 resolution is refused as
// initiator or approver of its compensation - at insert by the two additive
// triggers, and at execution by the re-check inside the Step B arm.
func TestK3_C28_StepBPersonSeparation(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	w.k2Setup()
	_, a := w.ambiguousPayout(500)
	res := w.executeM2(a.ID, ResolutionM2DeclarePaid) // requester f1, approver f2

	for name, actor := range map[string]k3Staff{"M2 requester": w.f1, "M2 approver": w.f2} {
		if _, err := w.k2Submit(actor, adjustment.DirectionCreditPlayer, 100, *res.LedgerTransactionID); k3Code(err) != "MA033" {
			t.Errorf("%s as initiator: want MA033, got %v", name, err)
		}
	}
	for name, actor := range map[string]k3Staff{"M2 requester": w.f1, "M2 approver": w.f2} {
		r, err := w.k2Submit(w.f3, adjustment.DirectionCreditPlayer, 100, *res.LedgerTransactionID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.k2Decide(actor, r); k3Code(err) != "MA033" {
			t.Errorf("%s as approver: want MA033, got %v", name, err)
		}
		// cancel to free nothing (K2 has no one-pending rule); fine.
	}

	// The execution-time re-check: with BOTH insert triggers disabled (the owner,
	// on a scratch DB) the requester f1 initiates and f4 approves - the arm's own
	// re-check refuses at execution (refused_at_execution, fail closed).
	for _, trg := range []struct{ table, name string }{
		{"ledger_adjustment_requests", "ledger_adjustment_requests_step_b_person_sep"},
		{"ledger_adjustment_approvals", "ledger_adjustment_approvals_step_b_person_sep"},
	} {
		if err := w.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `ALTER TABLE `+trg.table+` DISABLE TRIGGER `+trg.name)
			return err
		}); err != nil {
			t.Fatalf("disable %s: %v", trg.name, err)
		}
	}
	r, err := w.k2Submit(w.f1, adjustment.DirectionCreditPlayer, 100, *res.LedgerTransactionID)
	if err != nil {
		t.Fatalf("with the insert triggers disabled the request must insert: %v", err)
	}
	out, err := w.k2Decide(w.f4, r)
	if err != nil || out.Executed || out.Request.State != adjustment.StateRefusedAtExecution ||
		out.Request.RefusalCode == nil || *out.Request.RefusalCode != "step_b_person_separation" {
		t.Fatalf("the execution-time Person re-check did not refuse: %v %+v", err, out)
	}
	w.assertInvariants()
}

// C-26 (CC): two concurrent credits on one Step B: the cap (the hold leg) holds
// under the L2 causation lock - at most 500 is credited in total, never more.
func TestK3_C26_ConcurrentStepBCreditsRespectTheCap(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	w.k2Setup()
	_, a := w.ambiguousPayout(500)
	res := w.executeM2(a.ID, ResolutionM2DeclarePaid)
	r1, err := w.k2Submit(w.f3, adjustment.DirectionCreditPlayer, 300, *res.LedgerTransactionID)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := w.k2Submit(w.f3, adjustment.DirectionCreditPlayer, 300, *res.LedgerTransactionID)
	if err != nil {
		t.Fatal(err)
	}
	before := w.walletBalance("player_cash")
	var wg sync.WaitGroup
	outs := make([]adjustment.Outcome, 2)
	errs := make([]error, 2)
	for i, r := range []adjustment.Request{r1, r2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], errs[i] = w.k2Decide(w.f4, r)
		}()
	}
	wg.Wait()
	executed := 0
	for i := range outs {
		if errs[i] == nil && outs[i].Executed {
			executed++
		}
	}
	if executed != 1 {
		t.Fatalf("executed = %d (errors %v, %v), want exactly 1 of two 300-credits against a 500 cap", executed, errs[0], errs[1])
	}
	if d := w.walletBalance("player_cash") - before; d != 300 {
		t.Fatalf("credited %d, want 300 (the cap holds)", d)
	}
	w.assertInvariants()
}
