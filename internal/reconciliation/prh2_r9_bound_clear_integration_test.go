//go:build integration

package reconciliation

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// PAY-PAYOUT-BOUND-CLEAR-1 (ADR 0095 §35.6; ledger-finance ruling on the
// STANDING-1 review). A BOUND-class payout park - a payout attempt disputed
// with a bound reason while holding its reference X (here the real callback
// T10 callback_amount_asset_mismatch) - is reported in-run (a line names X)
// and standing (checkUnmatchedAttempts, no line this run). Before this item it
// cleared on DEPOSIT-shaped signals: a tombstone on X, or a completed
// deposit_reversal line naming X of ANY amount (even 1). Those say nothing
// about whether the payout left the platform. Now the bound payout finding
// clears ONLY through the payout rule shared with unbound payout parks
// (clearedRefFor -> payoutCompletedRef): the parked attempt's OWN
// withdrawal_completed (its withdrawal_requests.release_ledger_transaction_id)
// keyed by X, with no other attempt holding X. Deposits are unchanged.
//
// Every reconciliation run here executes as the RUNTIME role (rolsuper and
// rolbypassrls both false, FORCE RLS applies): fetch, ingest and the snapshot
// match. Fixtures are written by the owner pool. Each run asserts it wrote
// only its run and mismatch rows (assertOnlyRunWritten), and the world asserts
// SUM(debits) = SUM(credits) and a clean RunLedgerVsProjection.
//
// The "own completion" in these tests is a TEST STAND-IN (withdrawal.Complete
// called directly): the governed completion against the hold
// (PAY-PAYOUT-UNBOUND-RESOLVE-1) is NOT IMPLEMENTED.

const bcAmount = int64(700)

type bcWorld struct {
	*payWorld
	rt *payWorld // the same world, reconciliation runs through the runtime role
}

func newBCWorld(t *testing.T) *bcWorld {
	t.Helper()
	w := newPayWorld(t)
	rtw := *w
	rtw.pool = ma020RuntimePool(t)
	return &bcWorld{payWorld: w, rt: &rtw}
}

// newBCWorldOn builds a second tenant on the same database (tenant isolation).
func newBCWorldOn(t *testing.T, pool, rt *db.Pool) *bcWorld {
	t.Helper()
	w := newPayWorldOnPool(t, pool)
	rtw := *w
	rtw.pool = rt
	return &bcWorld{payWorld: w, rt: &rtw}
}

type bcPark struct {
	attempt payments.PaymentAttempt
	wrID    uuid.UUID
	x       string
}

// bcPendingPayout writes a payout attempt PENDING with reference x and its
// SUBMITTED withdrawal request (hold funded), owner pool.
func (w *bcWorld) bcPendingPayout(t *testing.T, x string, amount int64) (uuid.UUID, uuid.UUID) {
	t.Helper()
	wrID, attemptID := uuid.New(), uuid.New()
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		hold, err := ledger.GetOrCreateAccount(ctx, tx, w.f.tenantID, &w.f.walletID, ledger.AccountPlayerWithdrawalHold, "EUR")
		if err != nil {
			return err
		}
		if _, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: w.f.tenantID, TransactionType: ledger.TxWithdrawalRequested, IdempotencyKey: "bc-wreq-" + wrID.String(), CorrelationID: wrID,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: w.f.cashAccountID, Direction: ledger.Debit, Amount: amount},
				{LedgerAccountID: hold, Direction: ledger.Credit, Amount: amount},
			},
		}); err != nil {
			return fmt.Errorf("post hold: %w", err)
		}
		// B13-B: a LEGACY (NULL-binding) row, as it existed before migration 0126 (the fixture is not about the binding).
		if err := pitest.WithoutBindingGuard(ctx, tx, func() error {
			_, err := tx.Exec(ctx,
				`INSERT INTO withdrawal_requests (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, state, idempotency_key, provider_id, provider_reference)
			 VALUES ($1,$2,$3,$4,$5,'EUR',$6,'submitted',$7,$8,$9)`,
				wrID, w.f.tenantID, w.f.brandID, w.f.playerAccountID, w.f.walletID, amount, "bc-wd-"+wrID.String(), payProvA, x)
			return err
		}); err != nil {
			return fmt.Errorf("insert withdrawal: %w", err)
		}
		if _, err := payments.InsertSubmittingAttempt(ctx, tx, payments.NewSubmittingAttempt{
			ID: attemptID, TenantID: w.f.tenantID, Operation: payments.AttemptOperationPayout, WithdrawalRequestID: &wrID,
			ProviderID: payProvA, PaymentMethod: "bank_transfer", AssetCode: "EUR", Amount: amount, Interactive: false,
			ClaimToken: uuid.New(), LeaseOwner: "bc-test", LeaseUntil: time.Now().Add(time.Minute),
		}); err != nil {
			return err
		}
		return payments.MarkAccepted(ctx, tx, attemptID, payments.EvidenceSync, x, time.Now().Add(time.Hour))
	}); err != nil {
		t.Fatalf("pending payout fixture: %v", err)
	}
	return wrID, attemptID
}

// bcParkPayout parks a payout holding X through the REAL callback T10: a
// verified payout callback for X whose amount differs from the attempt's
// (callback_amount_asset_mismatch). The withdrawal hold is kept.
func (w *bcWorld) bcParkPayout(t *testing.T) bcPark {
	t.Helper()
	x := "bc-x-" + uuid.NewString()
	wrID, attemptID := w.bcPendingPayout(t, x, bcAmount)
	pend := w.attempt(t, attemptID)
	w.applyReceipt(t, payProvA, payments.ReceiptEvidence{
		EventType: "payout", ProviderReference: x, MerchantReference: pend.MerchantReference,
		Outcome: payments.OutcomeSucceeded, Amount: bcAmount - 1, AssetCode: "EUR",
	})
	a := w.attempt(t, attemptID)
	if a.State != payments.AttemptDisputed || a.TerminalReason == nil || *a.TerminalReason != payments.TerminalReasonCallbackAmountAssetMismatch ||
		a.ProviderReference == nil || *a.ProviderReference != x {
		t.Fatalf("setup: want a disputed callback_amount_asset_mismatch payout holding %s, got state=%s reason=%v ref=%v", x, a.State, a.TerminalReason, a.ProviderReference)
	}
	w.bcHeld(t, bcPark{attempt: a, wrID: wrID, x: x}, "after the park")
	return bcPark{attempt: a, wrID: wrID, x: x}
}

// bcHeld: the parked payout is untouched (disputed, same reason, hold kept, no
// completion or failure posting).
func (w *bcWorld) bcHeld(t *testing.T, p bcPark, what string) {
	t.Helper()
	a := w.attempt(t, p.attempt.ID)
	if a.State != payments.AttemptDisputed || a.TerminalReason == nil || *a.TerminalReason != payments.TerminalReasonCallbackAmountAssetMismatch {
		t.Fatalf("%s: the parked payout attempt moved: %s %v", what, a.State, a.TerminalReason)
	}
	var state string
	var release *uuid.UUID
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state, release_ledger_transaction_id FROM withdrawal_requests WHERE id = $1`, p.wrID).Scan(&state, &release)
	}); err != nil {
		t.Fatal(err)
	}
	if state != string(withdrawal.StateSubmitted) || release != nil {
		t.Fatalf("%s: the parked payout's hold moved: state=%s release=%v", what, state, release)
	}
}

// bcRun is one reconciliation run as the RUNTIME role; it asserts the run wrote
// only its run and mismatch rows, and the ledger invariants.
func (w *bcWorld) bcRun(t *testing.T, src statement.PaymentStatementSource) []Mismatch {
	t.Helper()
	id, _ := w.rt.fetchIngest(t, src, PaymentStatementOptions{})
	before := capturePay(t, w.pool, w.f.tenantID)
	run, ms, _, err := w.rt.matchErr(t, id, PaymentStatementOptions{})
	if err != nil {
		t.Fatalf("RunPaymentStatement (runtime role): %v", err)
	}
	if (len(ms) == 0) != (run.Status == StatusClean) {
		t.Fatalf("run status %s inconsistent with %d mismatches", run.Status, len(ms))
	}
	assertOnlyRunWritten(t, before, capturePay(t, w.pool, w.f.tenantID), len(ms))
	w.bcInvariants(t)
	return ms
}

func (w *bcWorld) bcInvariants(t *testing.T) {
	t.Helper()
	var debits, credits int64
	var drift []Mismatch
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'), 0)::bigint,
			       COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0)::bigint
			  FROM ledger_entries WHERE tenant_id = $1`, w.f.tenantID).Scan(&debits, &credits); err != nil {
			return err
		}
		var err error
		_, drift, err = RunLedgerVsProjection(ctx, tx, w.f.tenantID, time.Now().Add(-time.Hour), time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if debits != credits || len(drift) != 0 {
		t.Fatalf("ledger invariants: SUM(debits)=%d SUM(credits)=%d drift=%v", debits, credits, drift)
	}
}

// bcCU returns the pay_captured_unposted findings for the attempt.
func bcCU(ms []Mismatch, attemptID uuid.UUID) []Mismatch {
	var out []Mismatch
	for _, m := range ms {
		if m.MismatchKind == MismatchKindPayCapturedUnposted && strings.Contains(m.ReconciliationKey, "attempt="+attemptID.String()) {
			out = append(out, m)
		}
	}
	return out
}

// bcRequire asserts exactly one bound PAYOUT finding for p, keyed on X, with
// the R-K3-8 payout wording (never allocation). inRun distinguishes the
// matchPayment site (a line names X this run) from checkUnmatchedAttempts.
func bcRequire(t *testing.T, ms []Mismatch, p bcPark, inRun bool, what string) {
	t.Helper()
	got := bcCU(ms, p.attempt.ID)
	if len(got) != 1 {
		t.Fatalf("%s: want exactly one pay_captured_unposted for the bound payout park %s, got %d:\n%s", what, p.attempt.ID, len(got), renderMismatches(ms))
	}
	m := got[0]
	if m.ExpectedValue != payoutCapturedUnpostedResolutionHint || strings.Contains(m.ExpectedValue, "LEDGER-SUSPENSE") ||
		!strings.Contains(m.ReconciliationKey, "check=captured_unposted") || !strings.Contains(m.ReconciliationKey, "provider_reference="+p.x) ||
		!strings.Contains(m.ActualValue, "op=payout") || !strings.Contains(m.ActualValue, "terminal_reason="+payments.TerminalReasonCallbackAmountAssetMismatch) {
		t.Fatalf("%s: bound payout finding misrepresented: %s | %s | %s", what, m.ReconciliationKey, m.ExpectedValue, m.ActualValue)
	}
	if standing := strings.Contains(m.ActualValue, "no statement line"); standing == inRun {
		t.Fatalf("%s: inRun=%t but the detail is %q", what, inRun, m.ActualValue)
	}
}

func bcNone(t *testing.T, ms []Mismatch, p bcPark, what string) {
	t.Helper()
	if got := bcCU(ms, p.attempt.ID); len(got) != 0 {
		t.Fatalf("%s: want NO pay_captured_unposted for %s, got:\n%s", what, p.attempt.ID, renderMismatches(ms))
	}
}

func bcPayoutLine(ref, merchant, status string, amount int64) statement.PaymentStatementLine {
	return payLineFor(payProvA, ref, merchant, statement.PaymentLinePayout, status, amount)
}

func bcReversal(original, status string, amount int64) statement.PaymentStatementLine {
	l := payLineFor(payProvA, "bc-rev-"+uuid.NewString(), "", statement.PaymentLineDepositReversal, status, amount)
	l.OriginalProviderReference = original
	return l
}

func bcReal(offset int, lines ...statement.PaymentStatementLine) d2RealSource {
	return d2RealSource{k3Cov(offset, lines...)}
}

// bcTombstone posts a tombstone ledger row on (provider, ref) in w's tenant.
func (w *bcWorld) bcTombstone(t *testing.T, provider, ref string) {
	t.Helper()
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: w.f.tenantID, TransactionType: ledger.TxTombstone,
			IdempotencyKey: "tombstone:" + provider + ":" + ref, ProviderID: &provider, ProviderTxID: &ref, CorrelationID: uuid.New(),
		})
		return err
	}); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
}

// bcComplete posts withdrawal_completed for wrID keyed by (provider, ref)
// through the real withdrawal.Complete (TEST STAND-IN for the governed
// completion, see the file comment).
func (w *bcWorld) bcComplete(t *testing.T, wrID uuid.UUID, provider, ref string) {
	t.Helper()
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return withdrawal.Complete(ctx, tx, wrID, provider, ref)
	}); err != nil {
		t.Fatalf("withdrawal.Complete: %v", err)
	}
	w.bcInvariants(t)
}

// bcUnlinkedCompletion posts a withdrawal_completed keyed by (payProvA, ref)
// that no withdrawal request releases (no attribution at all), of amount.
func (w *bcWorld) bcUnlinkedCompletion(t *testing.T, ref string, amount int64) {
	t.Helper()
	pid := payProvA
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		hold, err := ledger.GetOrCreateAccount(ctx, tx, w.f.tenantID, &w.f.walletID, ledger.AccountPlayerWithdrawalHold, "EUR")
		if err != nil {
			return err
		}
		clearing, err := ledger.GetOrCreateAccount(ctx, tx, w.f.tenantID, nil, ledger.AccountPSPClearing, "EUR")
		if err != nil {
			return err
		}
		if _, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: w.f.tenantID, TransactionType: ledger.TxWithdrawalRequested, IdempotencyKey: "bc-unl-req-" + uuid.NewString(), CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: w.f.cashAccountID, Direction: ledger.Debit, Amount: amount},
				{LedgerAccountID: hold, Direction: ledger.Credit, Amount: amount},
			},
		}); err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: w.f.tenantID, TransactionType: ledger.TxWithdrawalCompleted, IdempotencyKey: pid + ":" + ref,
			ProviderID: &pid, ProviderTxID: &ref, CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: hold, Direction: ledger.Debit, Amount: amount},
				{LedgerAccountID: clearing, Direction: ledger.Credit, Amount: amount},
			},
		})
		return err
	}); err != nil {
		t.Fatalf("unlinked withdrawal_completed: %v", err)
	}
	w.bcInvariants(t)
}

// bcLegacyCompletion: a withdrawal with NO payment attempt (legacyUnattempted)
// completed under (payProvA, ref).
func (w *bcWorld) bcLegacyCompletion(t *testing.T, ref string, amount int64) {
	t.Helper()
	wrID := uuid.New()
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		hold, err := ledger.GetOrCreateAccount(ctx, tx, w.f.tenantID, &w.f.walletID, ledger.AccountPlayerWithdrawalHold, "EUR")
		if err != nil {
			return err
		}
		if _, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: w.f.tenantID, TransactionType: ledger.TxWithdrawalRequested, IdempotencyKey: "bc-leg-req-" + wrID.String(), CorrelationID: wrID,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: w.f.cashAccountID, Direction: ledger.Debit, Amount: amount},
				{LedgerAccountID: hold, Direction: ledger.Credit, Amount: amount},
			},
		}); err != nil {
			return err
		}
		// B13-B: a LEGACY (NULL-binding) row, as it existed before migration 0126 (the fixture is not about the binding).
		if err := pitest.WithoutBindingGuard(ctx, tx, func() error {
			_, err := tx.Exec(ctx,
				`INSERT INTO withdrawal_requests (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, state, idempotency_key, provider_id, provider_reference)
			 VALUES ($1,$2,$3,$4,$5,'EUR',$6,'submitted',$7,$8,$9)`,
				wrID, w.f.tenantID, w.f.brandID, w.f.playerAccountID, w.f.walletID, amount, "bc-leg-"+wrID.String(), payProvA, "bc-leg-instr-"+wrID.String()[:8])
			return err
		}); err != nil {
			return err
		}
		return withdrawal.Complete(ctx, tx, wrID, payProvA, ref)
	}); err != nil {
		t.Fatalf("legacy completion: %v", err)
	}
	w.bcInvariants(t)
}

// (1) The finding is outstanding run after run: in-run whenever a line names X
// (succeeded, pending or declined), standing on every run with no line.
func TestBoundClear1_PayoutBoundPark_OutstandingRunAfterRun(t *testing.T) {
	w := newBCWorld(t)
	p := w.bcParkPayout(t)
	line := bcPayoutLine(p.x, p.attempt.MerchantReference, statement.PaymentStatusSucceeded, bcAmount-1)
	bcRequire(t, w.bcRun(t, d2Src(line)), p, true, "in-run, succeeded line")
	for i := 0; i < 3; i++ {
		bcRequire(t, w.bcRun(t, d2PastSrc()), p, false, fmt.Sprintf("standing run %d", i))
	}
	for _, st := range []string{statement.PaymentStatusPending, statement.PaymentStatusDeclined} {
		bcRequire(t, w.bcRun(t, d2Src(bcPayoutLine(p.x, p.attempt.MerchantReference, st, bcAmount-1))), p, true, "in-run, "+st+" line (M-S2)")
	}
	bcRequire(t, w.bcRun(t, bcReal(3, line)), p, true, "in-run, real import")
	bcRequire(t, w.bcRun(t, d2PastSrc()), p, false, "standing after a real import")
	w.bcHeld(t, p, "after the runs")
}

// (2) A tombstone on X (a deposit-shaped signal) never clears - FLIPPED from the
// pre-BOUND-CLEAR-1 behaviour (ADR 0095 §35.6 "the defect"), in-run and standing.
func TestBoundClear1_PayoutBoundPark_TombstoneNeverClears(t *testing.T) {
	w := newBCWorld(t)
	p := w.bcParkPayout(t)
	line := bcPayoutLine(p.x, p.attempt.MerchantReference, statement.PaymentStatusSucceeded, bcAmount-1)
	bcRequire(t, w.bcRun(t, d2PastSrc()), p, false, "setup standing")
	w.bcTombstone(t, payProvA, p.x)
	bcRequire(t, w.bcRun(t, d2PastSrc()), p, false, "tombstone on X, standing")
	bcRequire(t, w.bcRun(t, d2Src(line)), p, true, "tombstone on X, in-run")
	bcRequire(t, w.bcRun(t, bcReal(3, line)), p, true, "tombstone on X, in-run, real import")
	bcRequire(t, w.bcRun(t, d2PastSrc()), p, false, "tombstone on X, standing, real import exists")
	w.bcHeld(t, p, "after the tombstone")
}

// (3) A completed deposit_reversal naming X never clears, whatever its amount
// (1, or exactly the attempt's amount), its status (succeeded or reversed), the
// import (MOCK or non-MOCK), in the same import as the payout line or alone,
// in-run and persisted for later runs. FLIPPED from the pre-BOUND-CLEAR-1
// behaviour ("a deposit_reversal of any amount, even 1, naming X clears").
func TestBoundClear1_PayoutBoundPark_DepositReversalNeverClears(t *testing.T) {
	for _, amt := range []int64{1, bcAmount, bcAmount - 1} {
		for _, st := range []string{statement.PaymentStatusSucceeded, statement.PaymentStatusReversed} {
			t.Run(fmt.Sprintf("amount_%d_%s", amt, st), func(t *testing.T) {
				w := newBCWorld(t)
				p := w.bcParkPayout(t)
				line := bcPayoutLine(p.x, p.attempt.MerchantReference, statement.PaymentStatusSucceeded, bcAmount-1)
				// Alone (MOCK): standing.
				bcRequire(t, w.bcRun(t, k3Cov(2, bcReversal(p.x, st, amt))), p, false, "MOCK deposit_reversal alone")
				// With the payout line in the same import: in-run.
				bcRequire(t, w.bcRun(t, k3Cov(3, line, bcReversal(p.x, st, amt))), p, true, "MOCK deposit_reversal with the payout line")
				bcRequire(t, w.bcRun(t, d2PastSrc()), p, false, "persisted MOCK deposit_reversal")
				// A non-MOCK import (eligible to clear deposit findings).
				bcRequire(t, w.bcRun(t, bcReal(4, bcReversal(p.x, st, amt))), p, false, "real deposit_reversal alone")
				bcRequire(t, w.bcRun(t, bcReal(5, line, bcReversal(p.x, st, amt))), p, true, "real deposit_reversal with the payout line")
				bcRequire(t, w.bcRun(t, d2PastSrc()), p, false, "persisted real deposit_reversal")
				w.bcHeld(t, p, "after the deposit_reversal")
			})
		}
	}
}

// (4) Unrelated amount and unrelated asset never clear: a deposit_reversal
// naming X in another asset; a succeeded payout line naming X of another amount
// or another asset (raises the amount/asset finding as well, never clears); an
// unattributed withdrawal_completed on X of an unrelated amount.
func TestBoundClear1_PayoutBoundPark_UnrelatedAmountOrAssetNeverClears(t *testing.T) {
	t.Run("deposit_reversal_other_asset", func(t *testing.T) {
		w := newBCWorld(t)
		p := w.bcParkPayout(t)
		rev := bcReversal(p.x, statement.PaymentStatusSucceeded, bcAmount)
		rev.AssetCode = "USD"
		bcRequire(t, w.bcRun(t, bcReal(2, rev)), p, false, "deposit_reversal in USD")
		bcRequire(t, w.bcRun(t, d2PastSrc()), p, false, "persisted")
	})
	t.Run("payout_line_other_amount_or_asset", func(t *testing.T) {
		w := newBCWorld(t)
		p := w.bcParkPayout(t)
		other := bcPayoutLine(p.x, p.attempt.MerchantReference, statement.PaymentStatusSucceeded, 5)
		ms := w.bcRun(t, d2Src(other))
		bcRequire(t, ms, p, true, "payout line of another amount")
		if d2Kinds(ms)[MismatchKindPayAmountMismatch] != 1 {
			t.Fatalf("want the amount mismatch alongside:\n%s", renderMismatches(ms))
		}
		usd := bcPayoutLine(p.x, p.attempt.MerchantReference, statement.PaymentStatusSucceeded, bcAmount)
		usd.AssetCode = "USD"
		ms = w.bcRun(t, k3Cov(2, usd))
		bcRequire(t, ms, p, true, "payout line in another asset")
		if d2Kinds(ms)[MismatchKindPayAssetMismatch] != 1 {
			t.Fatalf("want the asset mismatch alongside:\n%s", renderMismatches(ms))
		}
		bcRequire(t, w.bcRun(t, d2PastSrc()), p, false, "standing")
	})
	for _, amt := range []int64{50, bcAmount} {
		t.Run(fmt.Sprintf("unattributed_withdrawal_completed_on_X_amount_%d", amt), func(t *testing.T) {
			w := newBCWorld(t)
			p := w.bcParkPayout(t)
			w.bcUnlinkedCompletion(t, p.x, amt)
			bcRequire(t, w.bcRun(t, d2PastSrc()), p, false, "unlinked completion on X, standing")
			bcRequire(t, w.bcRun(t, d2Src(bcPayoutLine(p.x, p.attempt.MerchantReference, statement.PaymentStatusSucceeded, bcAmount-1))), p, true, "unlinked completion on X, in-run")
			w.bcHeld(t, p, "after the unlinked completion")
		})
	}
}

// (5) Evidence from ANOTHER attempt never clears: another payout attempt's own
// completion under X; a legacy/unattempted withdrawal's completion under X;
// another bound payout park's own completion (which DOES clear that park - the
// control).
func TestBoundClear1_PayoutBoundPark_AnotherAttemptsEvidenceNeverClears(t *testing.T) {
	t.Run("another_payout_attempt_completed_under_X", func(t *testing.T) {
		w := newBCWorld(t)
		p := w.bcParkPayout(t)
		otherWR, _ := w.bcPendingPayout(t, "bc-other-"+uuid.NewString(), 300)
		w.bcComplete(t, otherWR, payProvA, p.x)
		ms := w.bcRun(t, d2PastSrc())
		bcRequire(t, ms, p, false, "another attempt's completion under X, standing")
		bcRequire(t, w.bcRun(t, d2Src(bcPayoutLine(p.x, p.attempt.MerchantReference, statement.PaymentStatusSucceeded, bcAmount-1))), p, true, "another attempt's completion under X, in-run")
		w.bcHeld(t, p, "after the other attempt's completion")
	})
	t.Run("legacy_unattempted_completion_under_X", func(t *testing.T) {
		w := newBCWorld(t)
		p := w.bcParkPayout(t)
		w.bcLegacyCompletion(t, p.x, 120)
		bcRequire(t, w.bcRun(t, d2PastSrc()), p, false, "legacy completion under X, standing")
		bcRequire(t, w.bcRun(t, d2Src(bcPayoutLine(p.x, p.attempt.MerchantReference, statement.PaymentStatusSucceeded, bcAmount-1))), p, true, "legacy completion under X, in-run")
	})
	t.Run("another_bound_park_own_completion_clears_only_that_park", func(t *testing.T) {
		w := newBCWorld(t)
		p := w.bcParkPayout(t)
		q := w.bcParkPayout(t)
		ms := w.bcRun(t, d2PastSrc())
		bcRequire(t, ms, p, false, "setup p")
		bcRequire(t, ms, q, false, "setup q")
		w.bcComplete(t, q.wrID, payProvA, q.x)
		ms = w.bcRun(t, d2PastSrc())
		bcRequire(t, ms, p, false, "q's own completion does not clear p")
		bcNone(t, ms, q, "control: q's own completion clears q")
		// Deposit-shaped signals on q's reference do not touch p either.
		w.bcTombstone(t, payProvA, "bc-unrelated-"+uuid.NewString())
		bcRequire(t, w.bcRun(t, bcReal(2, bcReversal(q.x, statement.PaymentStatusSucceeded, bcAmount))), p, false, "reversal naming q's reference")
	})
}

// (6) The legitimate path, exactly as STANDING-1 authorises it (§35.6, LF F6):
// the parked attempt's OWN withdrawal_completed keyed by X clears the bound
// finding in-run and standing, from then on. Its own completion under ANY
// other reference does not.
func TestBoundClear1_PayoutBoundPark_OwnPositivelyAttributedCompletionClears(t *testing.T) {
	t.Run("own_completion_keyed_by_X", func(t *testing.T) {
		w := newBCWorld(t)
		p := w.bcParkPayout(t)
		line := bcPayoutLine(p.x, p.attempt.MerchantReference, statement.PaymentStatusSucceeded, bcAmount-1)
		bcRequire(t, w.bcRun(t, d2Src(line)), p, true, "setup in-run")
		bcRequire(t, w.bcRun(t, d2PastSrc()), p, false, "setup standing")
		w.bcComplete(t, p.wrID, payProvA, p.x)
		bcNone(t, w.bcRun(t, d2PastSrc()), p, "own completion, standing")
		bcNone(t, w.bcRun(t, k3Cov(2, line)), p, "own completion, in-run")
		bcNone(t, w.bcRun(t, bcReal(3, line)), p, "own completion, in-run, real import")
		bcNone(t, w.bcRun(t, d2PastSrc()), p, "own completion, stays cleared")
	})
	t.Run("own_completion_under_another_reference_does_not_clear", func(t *testing.T) {
		w := newBCWorld(t)
		p := w.bcParkPayout(t)
		w.bcComplete(t, p.wrID, payProvA, "bc-settle-"+uuid.NewString())
		bcRequire(t, w.bcRun(t, d2PastSrc()), p, false, "own completion under another reference, standing")
	})
}

// (7) Deposit bound parks are unchanged: a tombstone on X clears, a completed
// deposit_reversal naming X clears, and the payout signal (a withdrawal_completed
// keyed by X) never clears a deposit park.
func TestBoundClear1_DepositBoundPark_Unchanged(t *testing.T) {
	park := func(t *testing.T, w *bcWorld) (uuid.UUID, string) {
		x := "bc-dep-x-" + uuid.NewString()
		id := w.ma020Park(t, payments.TerminalReasonCallbackAmountAssetMismatch, x, "")
		ms := w.bcRun(t, d2PastSrc())
		if len(bcCU(ms, id)) != 1 {
			t.Fatalf("setup: want the deposit bound finding:\n%s", renderMismatches(ms))
		}
		if ev := bcCU(ms, id)[0].ExpectedValue; ev != boundCapturedUnpostedResolutionHint {
			t.Fatalf("deposit wording changed: %q", ev)
		}
		return id, x
	}
	t.Run("tombstone_clears", func(t *testing.T) {
		w := newBCWorld(t)
		id, x := park(t, w)
		w.bcTombstone(t, payProvA, x)
		bcNone(t, w.bcRun(t, d2PastSrc()), bcPark{attempt: payments.PaymentAttempt{ID: id}}, "deposit: tombstone clears")
	})
	t.Run("deposit_reversal_clears", func(t *testing.T) {
		w := newBCWorld(t)
		id, x := park(t, w)
		bcNone(t, w.bcRun(t, k3Cov(2, bcReversal(x, statement.PaymentStatusSucceeded, ma020Amount))), bcPark{attempt: payments.PaymentAttempt{ID: id}}, "deposit: reversal clears in-run")
		bcNone(t, w.bcRun(t, d2PastSrc()), bcPark{attempt: payments.PaymentAttempt{ID: id}}, "deposit: reversal clears, persisted")
	})
	t.Run("withdrawal_completed_never_clears_a_deposit", func(t *testing.T) {
		w := newBCWorld(t)
		id, x := park(t, w)
		w.bcUnlinkedCompletion(t, x, ma020Amount)
		if ms := w.bcRun(t, d2PastSrc()); len(bcCU(ms, id)) != 1 {
			t.Fatalf("a withdrawal_completed keyed by X must not clear a deposit park:\n%s", renderMismatches(ms))
		}
	})
}

// (8) Tenant isolation: tenant B, under tenant A's provider id and A's X, posts
// every clearing shape (a tombstone, a withdrawal_completed of its own payout,
// a completed deposit_reversal naming X and a payout line naming X with A's
// merchant reference). A's bound finding is untouched; B gets no
// captured-unposted finding. Then A's own completion clears A only.
func TestBoundClear1_PayoutBoundPark_TenantIsolation(t *testing.T) {
	a := newBCWorld(t)
	b := newBCWorldOn(t, a.pool, a.rt.pool)
	p := a.bcParkPayout(t)
	line := bcPayoutLine(p.x, p.attempt.MerchantReference, statement.PaymentStatusSucceeded, bcAmount-1)
	bcRequire(t, a.bcRun(t, d2Src(line)), p, true, "setup A in-run")

	// B: a payout of its own completed under (payProvA, X), and in another B
	// world a tombstone on (payProvA, X) (one ledger key per tenant/provider/ref).
	bwr, _ := b.bcPendingPayout(t, "bc-b-"+uuid.NewString(), 200)
	b.bcComplete(t, bwr, payProvA, p.x)
	c := newBCWorldOn(t, a.pool, a.rt.pool)
	c.bcTombstone(t, payProvA, p.x)
	for _, w := range []*bcWorld{b, c} {
		ms := w.bcRun(t, bcReal(2, line, bcReversal(p.x, statement.PaymentStatusSucceeded, bcAmount)))
		if got := bcCU(ms, p.attempt.ID); len(got) != 0 {
			t.Fatalf("another tenant reported tenant A's attempt:\n%s", renderMismatches(ms))
		}
		for _, m := range ms {
			if m.MismatchKind == MismatchKindPayCapturedUnposted {
				t.Fatalf("another tenant has a captured_unposted finding it should not:\n%s", renderMismatches(ms))
			}
		}
	}
	bcRequire(t, a.bcRun(t, d2PastSrc()), p, false, "A standing after B/C activity")
	bcRequire(t, a.bcRun(t, k3Cov(2, line)), p, true, "A in-run after B/C activity")
	a.bcHeld(t, p, "A after B/C activity")

	a.bcComplete(t, p.wrID, payProvA, p.x)
	bcNone(t, a.bcRun(t, d2PastSrc()), p, "A's own completion clears A")
}

// FLIPPED under PAY-PAYOUT-BOUND-CLEAR-1 F-1 (was
// TestBoundClear1_PayoutBoundPark_ReversedPayoutLine_OneRunSuppressionOnly,
// which pinned the then-current behaviour). Ledger-finance ruled F-1 a
// fail-closed tightening within its authority (security concurs): the R1
// "reversed line = the PSP's own refund" rule is a DEPOSIT rule. Before the fix
// a `reversed` PAYOUT line naming a bound payout park fell to the silent
// "disputed" case and, the attempt being matched, checkUnmatchedAttempts
// skipped it - for EVERY run in which the provider repeated the line; with
// amount and asset equal to the attempt's the whole run was CLEAN (security
// probe P1). Now it raises the bound finding in-run, on every run, by reference
// and by merchant reference only, and the finding keeps standing afterwards.
func TestBoundClear1_PayoutBoundPark_ReversedPayoutLineNeverSilences(t *testing.T) {
	t.Run("by_reference_amount_and_asset_equal", func(t *testing.T) {
		w := newBCWorld(t)
		p := w.bcParkPayout(t)
		bcRequire(t, w.bcRun(t, d2PastSrc()), p, false, "setup standing")
		equal := bcPayoutLine(p.x, p.attempt.MerchantReference, statement.PaymentStatusReversed, bcAmount) // amount AND asset equal (EUR)
		for i := 0; i < 3; i++ {
			ms := w.bcRun(t, k3Cov(i+1, equal))
			bcRequire(t, ms, p, true, fmt.Sprintf("reversed payout line, amount/asset equal, run %d", i))
			if len(ms) == 0 {
				t.Fatalf("run %d: the run must not be CLEAN", i)
			}
		}
		// The line amount differing (the callback's amount) is loud too.
		bcRequire(t, w.bcRun(t, d2Src(bcPayoutLine(p.x, p.attempt.MerchantReference, statement.PaymentStatusReversed, bcAmount-1))), p, true, "reversed payout line, other amount")
		bcRequire(t, w.bcRun(t, bcReal(5, equal)), p, true, "reversed payout line, real import")
		for i := 0; i < 3; i++ {
			bcRequire(t, w.bcRun(t, d2PastSrc()), p, false, fmt.Sprintf("standing after reversed lines, run %d", i))
		}
		w.bcHeld(t, p, "after the reversed lines")
	})
	t.Run("merchant_reference_only_route", func(t *testing.T) {
		w := newBCWorld(t)
		p := w.bcParkPayout(t)
		// A reversed payout line under a reference no attempt holds, naming the
		// park only by merchant reference: matchPayment resolves it by merchant.
		other := bcPayoutLine("bc-psp-other-"+uuid.NewString(), p.attempt.MerchantReference, statement.PaymentStatusReversed, bcAmount)
		for i := 0; i < 3; i++ {
			ms := w.bcRun(t, k3Cov(i+1, other))
			got := bcCU(ms, p.attempt.ID)
			// The in-run key names the LINE's reference (the merchant route).
			if len(got) != 1 || got[0].ExpectedValue != payoutCapturedUnpostedResolutionHint ||
				!strings.Contains(got[0].ReconciliationKey, "provider_reference="+other.ProviderReference) ||
				!strings.Contains(got[0].ActualValue, "op=payout") || strings.Contains(got[0].ActualValue, "no statement line") {
				t.Fatalf("merchant-only reversed payout line, run %d: want one in-run payout finding:\n%s", i, renderMismatches(ms))
			}
			if d2Kinds(ms)[MismatchKindPayReferenceMismatch] != 1 {
				t.Fatalf("run %d: want the reference mismatch alongside:\n%s", i, renderMismatches(ms))
			}
		}
		bcRequire(t, w.bcRun(t, d2PastSrc()), p, false, "standing after merchant-only reversed lines")
		w.bcHeld(t, p, "after the merchant-only reversed lines")
	})
	t.Run("own_completion_still_clears_with_a_reversed_line", func(t *testing.T) {
		w := newBCWorld(t)
		p := w.bcParkPayout(t)
		w.bcComplete(t, p.wrID, payProvA, p.x)
		bcNone(t, w.bcRun(t, d2Src(bcPayoutLine(p.x, p.attempt.MerchantReference, statement.PaymentStatusReversed, bcAmount))), p, "own completion clears even with a reversed line")
	})
}

// Deposit R1 is unchanged: a `reversed` DEPOSIT line naming a bound deposit
// park still clears that run's in-run finding (code review R1).
func TestBoundClear1_DepositBoundPark_ReversedLineR1Unchanged(t *testing.T) {
	w := newBCWorld(t)
	x := "bc-dep-r1-" + uuid.NewString()
	id := w.ma020Park(t, payments.TerminalReasonCallbackAmountAssetMismatch, x, "")
	merchant := w.attempt(t, id).MerchantReference
	if ms := w.bcRun(t, d2PastSrc()); len(bcCU(ms, id)) != 1 {
		t.Fatalf("setup: want the deposit bound finding:\n%s", renderMismatches(ms))
	}
	bcNone(t, w.bcRun(t, d2Src(d2Line(x, merchant, statement.PaymentStatusReversed, ma020Amount))), bcPark{attempt: payments.PaymentAttempt{ID: id}}, "deposit R1: a reversed deposit line clears the in-run finding")
}
