//go:build integration

// PRH-I5 fix round: code review RV-PRH-I5 (F1 legacy postings, F2 horizon,
// F3 unposted declined reversal, F4 settlement reference, F5 coverage end,
// surviving mutants RM2-RM6) and security review C1/C2.
package reconciliation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

func (w *payWorld) runInfo(t *testing.T, src statement.PaymentStatementSource, opts PaymentStatementOptions) ([]Mismatch, PaymentStatementInfo) {
	t.Helper()
	id, _ := w.fetchIngest(t, src, opts)
	_, ms, info, err := w.matchErr(t, id, opts)
	if err != nil {
		t.Fatalf("RunPaymentStatement: %v", err)
	}
	return ms, info
}

// legacyDeposit simulates a deposit posted BEFORE the payments callback
// cutover (PRH-payments-callback-cutover): a deposit_intents row with no
// payment_attempts row at all, and a ledger_transaction_id set directly -
// exactly the on-disk SHAPE the pre-cutover live path (InitiateDeposit plus
// the old, now-removed deposit_intents-only ReceiveVerifiedCallback) used
// to produce.
//
// After the cutover, ReceiveVerifiedCallback resolves every deposit
// callback through payment_attempts (ApplyReceiptEvidence, ADR 0095 §6.1,
// INV-IO-14) - a deposit_intents row created via the legacy InitiateDeposit
// helper (still exported for this reconciliation-history test, but no
// longer reachable from any live HTTP path) has no attempt to resolve
// against, so a callback for it is now correctly `deferred_unresolved`
// (never posted) rather than posted via the old, now-removed
// intent-only path. This helper therefore constructs the pre-cutover
// on-disk shape directly (InitiateDeposit for the intent row, then a raw
// ledger.Post plus a direct UPDATE for the posting) instead of driving it
// through today's callback path, so this test keeps proving what it always
// proved: reconciliation's legacy_unattempted exclusion (F1) for
// attempt-less historical data, independent of how such data is created.
func (w *payWorld) legacyDeposit(t *testing.T, amount int64) string {
	t.Helper()
	var intent payments.DepositIntent
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = w.orch.InitiateDeposit(ctx, tx, payments.InitiateDepositParams{
			Scope:     payments.DepositScope{TenantID: w.f.tenantID, BrandID: w.f.brandID, PlayerAccountID: w.f.playerAccountID, WalletID: w.f.walletID},
			AssetCode: "EUR", Amount: amount, PaymentMethod: "card", IdempotencyKey: "legacy-" + uuid.NewString(),
		})
		return err
	}); err != nil {
		t.Fatalf("legacy InitiateDeposit: %v", err)
	}
	if intent.ProviderReference == nil {
		t.Fatalf("legacy deposit has no provider reference (status %s)", intent.Status)
	}
	ref := *intent.ProviderReference
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		accounts, err := ledger.GetOrCreateAccounts(ctx, tx, w.f.tenantID,
			ledger.AccountSpec{WalletID: &w.f.walletID, AccountType: ledger.AccountPlayerCash, AssetCode: "EUR"},
			ledger.AccountSpec{AccountType: ledger.AccountPSPClearing, AssetCode: "EUR"},
		)
		if err != nil {
			return err
		}
		cashAccountID, clearingAccountID := accounts[0], accounts[1]
		providerID := payProvA
		postResult, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: w.f.tenantID, TransactionType: ledger.TxDeposit, IdempotencyKey: payProvA + ":" + ref,
			ProviderID: &providerID, ProviderTxID: &ref, CorrelationID: intent.ID,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: clearingAccountID, Direction: ledger.Debit, Amount: amount},
				{LedgerAccountID: cashAccountID, Direction: ledger.Credit, Amount: amount},
			},
		})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE deposit_intents SET status = 'succeeded', ledger_transaction_id = $1 WHERE id = $2`,
			postResult.TransactionID, intent.ID)
		return err
	}); err != nil {
		t.Fatalf("legacy deposit posting: %v", err)
	}
	return ref
}

// legacyWithdrawalCompleted is the live withdrawal completion: a submitted
// request completed through withdrawal.Complete with no payout attempt.
func (w *payWorld) legacyWithdrawalCompleted(t *testing.T, provider, settlement string, amount int64) {
	t.Helper()
	wrID := uuid.New()
	err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		hold, err := ledger.GetOrCreateAccount(ctx, tx, w.f.tenantID, &w.f.walletID, ledger.AccountPlayerWithdrawalHold, "EUR")
		if err != nil {
			return err
		}
		if _, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: w.f.tenantID, TransactionType: ledger.TxWithdrawalRequested, IdempotencyKey: "wreq-" + wrID.String(), CorrelationID: wrID,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: w.f.cashAccountID, Direction: ledger.Debit, Amount: amount},
				{LedgerAccountID: hold, Direction: ledger.Credit, Amount: amount},
			},
		}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO withdrawal_requests (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, state, idempotency_key, provider_id, provider_reference)
			 VALUES ($1,$2,$3,$4,$5,'EUR',$6,'submitted',$7,$8,$9)`,
			wrID, w.f.tenantID, w.f.brandID, w.f.playerAccountID, w.f.walletID, amount, "wd-"+wrID.String(), provider, "instr-"+wrID.String()[:8]); err != nil {
			return err
		}
		return withdrawal.Complete(ctx, tx, wrID, provider, settlement)
	})
	if err != nil {
		t.Fatalf("legacy withdrawal: %v", err)
	}
}

// F1 (ledger-finance ruling): a legacy posting (linked from an intent or
// request with no attempt at all) is not a P1; it is counted in
// legacy_unattempted and audited.
func TestPaymentStatement_LegacyUnattemptedPostingsAreCountedNotFlagged(t *testing.T) {
	w := newPayWorld(t)
	w.legacyDeposit(t, 6000)
	w.legacyWithdrawalCompleted(t, payProvA, "legacy-settle-"+uuid.NewString()[:8], 1000)
	ms, info := w.runInfo(t, w.srcA, PaymentStatementOptions{})
	if len(ms) != 0 {
		t.Fatalf("legacy postings must not raise a P1:\n%s", renderMismatches(ms))
	}
	if info.LegacyUnattempted != 2 {
		t.Fatalf("expected legacy_unattempted=2 (one deposit, one withdrawal), got %d", info.LegacyUnattempted)
	}
	if info.AuditMetadata()["legacy_unattempted"] != 2 {
		t.Fatalf("legacy_unattempted must be in the run audit metadata: %v", info.AuditMetadata())
	}
	out := ReconcilePaymentStatementForTenant(context.Background(), w.pool, nil, w.f.tenantID, time.Now(), time.Now(), w.srcA, PaymentStatementOptions{})
	if out.Err != nil || out.Run.Status != StatusClean {
		t.Fatalf("sweep: %+v", out)
	}
	var n int
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'reconciliation.sweep_run'
		  AND metadata->>'stream' = 'payment_statement' AND (metadata->>'legacy_unattempted')::int = 2`, w.f.tenantID).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("expected one audited run carrying legacy_unattempted=2, got n=%d err=%v", n, err)
	}
}

// The exclusion never covers attempt-linked postings: a v2 (attempt-path)
// deposit counts as 0 legacy, and a completion whose withdrawal HAS a payout
// attempt that did not succeed is still a P1 (RM6: the withdrawal_completed
// half of LF95-C13 direction (b)).
func TestPaymentStatement_AttemptLinkedPostingsStayFullyChecked(t *testing.T) {
	w := newPayWorld(t)
	a := w.deposit(t, 7000)
	w.succeed(t, w.mockA, payProvA, a)
	ms, info := w.runInfo(t, w.srcA, PaymentStatementOptions{})
	if len(ms) != 0 || info.LegacyUnattempted != 0 {
		t.Fatalf("v2 deposit: expected clean with legacy_unattempted=0, got %d mismatches, legacy=%d", len(ms), info.LegacyUnattempted)
	}

	instr, settle := "instr-"+uuid.NewString()[:8], "settle-"+uuid.NewString()[:8]
	w.payoutFixture(t, payProvA, instr, settle, 2500, false) // completed, attempt still pending
	src := payFixedSource{provider: payProvA, stmt: wideCoverage(
		payLineFor(payProvA, *a.ProviderReference, a.MerchantReference, statement.PaymentLineDeposit, statement.PaymentStatusSucceeded, 7000),
		payLineFor(payProvA, instr, "", statement.PaymentLinePayout, statement.PaymentStatusPending, 2500))}
	ms, info = w.runInfo(t, src, PaymentStatementOptions{})
	mustOnePay(t, ms, MismatchKindPayMissingPlatformRecord, "provider_tx_id="+settle, "type=withdrawal_completed", "check=ledger_join")
	if info.LegacyUnattempted != 0 {
		t.Fatalf("an attempt-linked posting must never count as legacy, got %d", info.LegacyUnattempted)
	}
}

// RM2: a posted reversal against a provider reversal line still pending;
// RM5: the reversal's asset is checked (before its amount).
func TestPaymentStatement_Reversal_PostedButProviderPendingAndAsset(t *testing.T) {
	w := newPayWorld(t)
	a := w.deposit(t, 7000)
	w.succeed(t, w.mockA, payProvA, a)
	revRef := "rev-" + uuid.NewString()[:8]
	w.deliverReversal(t, revRef, *a.ProviderReference, a.Amount)
	dep := payLineFor(payProvA, *a.ProviderReference, a.MerchantReference, statement.PaymentLineDeposit, statement.PaymentStatusReversed, 7000)
	rev := payLineFor(payProvA, revRef, "", statement.PaymentLineDepositReversal, statement.PaymentStatusPending, 7000)
	_, ms := w.run(t, payFixedSource{provider: payProvA, stmt: wideCoverage(dep, rev)}, PaymentStatementOptions{})
	mustOnePay(t, ms, MismatchKindPayStatusMismatch, "provider_reference="+revRef, "check=status")

	rev.Status, rev.AssetCode, rev.Amount = statement.PaymentStatusSucceeded, "USD", 1
	_, ms = w.run(t, payFixedSource{provider: payProvA, stmt: wideCoverage(dep, rev)}, PaymentStatementOptions{})
	mustOnePay(t, ms, MismatchKindPayAssetMismatch, "provider_reference="+revRef, "check=asset")
}

// F3: a declined or pending reversal line with no posting is not a P1; a
// succeeded one is.
func TestPaymentStatement_Reversal_UnpostedDeclinedOrPendingIsNotAFinding(t *testing.T) {
	w := newPayWorld(t)
	for _, st := range []string{statement.PaymentStatusDeclined, statement.PaymentStatusPending} {
		l := payLineFor(payProvA, "rev-won-"+st, "", statement.PaymentLineDepositReversal, st, 500)
		l.OriginalProviderReference = "some-deposit"
		w.mustClean(t, payFixedSource{provider: payProvA, stmt: wideCoverage(l)}, PaymentStatementOptions{})
	}
	l := payLineFor(payProvA, "rev-lost", "", statement.PaymentLineDepositReversal, statement.PaymentStatusSucceeded, 500)
	l.OriginalProviderReference = "some-deposit"
	_, ms := w.run(t, payFixedSource{provider: payProvA, stmt: wideCoverage(l)}, PaymentStatementOptions{})
	mustOnePay(t, ms, MismatchKindPayMissingPlatformRecord, "provider_reference=rev-lost", "check=reversal")
}

// RM3: two lines resolving one attempt (instruction ref and settlement ref).
func TestPaymentStatement_Payout_TwoLinesForOneAttemptIsDuplicate(t *testing.T) {
	w := newPayWorld(t)
	instr, settle := "instr-"+uuid.NewString()[:8], "settle-"+uuid.NewString()[:8]
	id := w.payoutFixture(t, payProvA, instr, settle, 3000, true)
	src := payFixedSource{provider: payProvA, stmt: wideCoverage(
		payLineFor(payProvA, instr, "", statement.PaymentLinePayout, statement.PaymentStatusSucceeded, 3000),
		payLineFor(payProvA, settle, "", statement.PaymentLinePayout, statement.PaymentStatusSucceeded, 3000))}
	_, ms := w.run(t, src, PaymentStatementOptions{})
	mustOnePay(t, ms, MismatchKindPayDuplicate, "attempt="+id.String(), "check=duplicate_match")
}

// RM4: a payout line resolved through its separate SettlementReference
// field; F4: a SettlementReference contradicting the ledger is flagged.
func TestPaymentStatement_Payout_SettlementReferenceField(t *testing.T) {
	w := newPayWorld(t)
	instr, settle := "instr-"+uuid.NewString()[:8], "settle-"+uuid.NewString()[:8]
	id := w.payoutFixture(t, payProvA, instr, settle, 3000, true)
	l := payLineFor(payProvA, "psp-own-id-"+uuid.NewString()[:8], "", statement.PaymentLinePayout, statement.PaymentStatusSucceeded, 3000)
	l.SettlementReference = settle
	w.mustClean(t, payFixedSource{provider: payProvA, stmt: wideCoverage(l)}, PaymentStatementOptions{})

	byInstr := payLineFor(payProvA, instr, "", statement.PaymentLinePayout, statement.PaymentStatusSucceeded, 3000)
	byInstr.SettlementReference = "not-the-ledger-one"
	_, ms := w.run(t, payFixedSource{provider: payProvA, stmt: wideCoverage(byInstr)}, PaymentStatementOptions{})
	mustOnePay(t, ms, MismatchKindPayReferenceMismatch, "attempt="+id.String(), "check=settlement_reference")
}

// F2: an in-flight attempt with no line is pay_unresolved past the horizon
// even when it predates the statement's window (window <= horizon).
func TestPaymentStatement_Unresolved_NotCoverageGated(t *testing.T) {
	w := newPayWorld(t)
	a := w.deposit(t, 5000)
	time.Sleep(5 * time.Millisecond)
	later := statement.PaymentStatement{CoverageStart: time.Now().UTC(), CoverageEnd: time.Now().Add(time.Minute).UTC()}
	_, ms := w.run(t, payFixedSource{provider: payProvA, stmt: later}, PaymentStatementOptions{UnresolvedHorizon: time.Millisecond})
	mustOnePay(t, ms, MismatchKindPayUnresolved, "attempt="+a.ID.String(), "check=unresolved")
	// A succeeded attempt before the window is still never "missing".
	w.succeed(t, w.mockA, payProvA, a)
	w.mustClean(t, payFixedSource{provider: payProvA, stmt: later}, PaymentStatementOptions{UnresolvedHorizon: time.Millisecond})
}

// F5: a coverage end beyond fetch time plus the skew is refused.
func TestPaymentStatement_CoverageEndBoundedAtFetch(t *testing.T) {
	w := newPayWorld(t)
	future := statement.PaymentStatement{CoverageStart: time.Now().Add(-time.Hour).UTC(), CoverageEnd: time.Now().Add(PaymentCoverageMaxClockSkew + time.Minute).UTC()}
	_, err := FetchPaymentStatement(context.Background(), payFixedSource{provider: payProvA, stmt: future}, w.f.tenantID, time.Now(), time.Now(), PaymentStatementOptions{})
	if !errors.Is(err, ErrPaymentStatementInvalid) {
		t.Fatalf("expected a future coverage end to be refused, got %v", err)
	}
}

// Security C2: control characters in merchant_reference / asset_code, and
// a non-registry-shaped asset code, refuse the import before anything is
// stored (a NUL can no longer force an ingest failure every run).
func TestPaymentStatement_ControlCharactersRefused(t *testing.T) {
	w := newPayWorld(t)
	for name, mut := range map[string]func(l *statement.PaymentStatementLine){
		"merchant_nul":     func(l *statement.PaymentStatementLine) { l.MerchantReference = "m" + string(rune(0)) },
		"merchant_newline": func(l *statement.PaymentStatementLine) { l.MerchantReference = "m\nINJECT" },
		"merchant_escape":  func(l *statement.PaymentStatementLine) { l.MerchantReference = string(rune(0x1b)) + "[31m" },
		"merchant_c1":      func(l *statement.PaymentStatementLine) { l.MerchantReference = "m" + string(rune(0x85)) },
		"asset_nul":        func(l *statement.PaymentStatementLine) { l.AssetCode = "EU" + string(rune(0)) },
		"asset_lower":      func(l *statement.PaymentStatementLine) { l.AssetCode = "eur" },
		"asset_formula":    func(l *statement.PaymentStatementLine) { l.AssetCode = "=EUR" },
	} {
		l := payLineFor(payProvA, "r1", "m1", statement.PaymentLineDeposit, statement.PaymentStatusPending, 1)
		mut(&l)
		before := capturePay(t, w.pool, w.f.tenantID)
		out := ReconcilePaymentStatementForTenant(context.Background(), w.pool, nil, w.f.tenantID, time.Now(), time.Now(),
			payFixedSource{provider: payProvA, stmt: wideCoverage(l)}, PaymentStatementOptions{})
		if !errors.Is(out.Err, ErrPaymentStatementInvalid) {
			t.Errorf("%s: expected refusal at fetch, got %v", name, out.Err)
		}
		if after := capturePay(t, w.pool, w.f.tenantID); after.imports != before.imports || after.lines != before.lines {
			t.Errorf("%s: a refused statement stored rows", name)
		}
	}
}

// Security C1 at the stream: a source hitting a streaming cap (the MOCK's
// own collector, or a body cap) refuses the import with nothing stored.
func TestPaymentStatement_SourceStreamingCapRefuses(t *testing.T) {
	w := newPayWorld(t)
	w.deposit(t, 5000)
	w.deposit(t, 5100)
	before := capturePay(t, w.pool, w.f.tenantID)
	out := ReconcilePaymentStatementForTenant(context.Background(), w.pool, nil, w.f.tenantID, time.Now(), time.Now(), w.srcA.WithMaxLines(1), PaymentStatementOptions{})
	if !errors.Is(out.Err, ErrPaymentStatementTooLarge) || !errors.Is(out.Err, statement.ErrPaymentStatementTooManyLines) {
		t.Fatalf("expected the streaming line cap to refuse the import, got %v", out.Err)
	}
	body := payFixedSource{provider: payProvA, err: statement.ErrPaymentStatementBodyTooLarge}
	if _, err := FetchPaymentStatement(context.Background(), body, w.f.tenantID, time.Now(), time.Now(), PaymentStatementOptions{}); !errors.Is(err, ErrPaymentStatementTooLarge) {
		t.Fatalf("a body-cap error from a source must refuse as too large, got %v", err)
	}
	if after := capturePay(t, w.pool, w.f.tenantID); after.imports != before.imports || after.lines != before.lines || after.runs != before.runs {
		t.Fatal("a capped statement must store nothing and record no run")
	}
}
