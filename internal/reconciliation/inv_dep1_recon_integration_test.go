//go:build integration

// PAY-DOUBLE-CREDIT-1 / INV-DEP-1, reconciliation half (ADR 0095 §28.9;
// ledger-finance ruling docs/plans/payment-readiness/lf-q1-supersession.md
// §4; test matrix docs/plans/payment-readiness/double-credit-
// reconciliation.md §5 row M). Companion to
// internal/payments/inv_dep1_matrix_integration_test.go - see that file's
// header for the full "read this before touching a failing test" note and
// the N mutation checklist (item 8 belongs here).
//
// This splits internal/reconciliation/payment_statement_integration_test.go's
// TestPaymentStatement_Kind_DuplicatePlatformSuccess into two, per
// ledger-finance ruling §5 item 3:
//
//   - TestINVDEP1_Recon_M_CapturedUnposted_ReplacesDuplicate drives the
//     REAL receipt path (payments.ApplyReceiptEvidence, exactly as
//     w.succeed/w.applyReceipt already do) for a second capture, and
//     asserts the FUTURE outcome: a new mismatch kind
//     'pay_captured_unposted' (a plain string literal - no
//     MismatchKindPayCapturedUnposted constant exists yet), and NO
//     'pay_duplicate'. EXPECTED TO FAIL against HEAD: today the real
//     path still posts a second, real ledger transaction and produces
//     'pay_duplicate' exactly like the original test asserted.
//   - TestINVDEP1_Recon_M_DuplicateDetector_LegacyDataShape keeps the
//     ORIGINAL test's synthetic, direct-ledger.Post fixture ("legacy data
//     shape": two succeeded attempts for one intent, built without going
//     through the receipt path at all) and asserts 'pay_duplicate' still
//     fires. This is the ledger-finance ruling's "the detector itself
//     stays valid" half (§4: "pay_duplicate ... is kept as an integrity
//     detector ... any occurrence means an index was dropped or the data
//     predates" migration 0107). EXPECTED TO PASS against HEAD (the
//     detector already exists) AND after the fix (it must keep firing on
//     this exact legacy shape forever, since that shape becomes
//     structurally unreachable in new data but is not itself deleted).
//
// Migration 0107 (the two partial unique indexes) does not exist yet, so
// this file cannot exercise its pre-flight refusal on this legacy shape;
// that is a NOT IMPLEMENTED gap here, flagged for whoever lands 0107 to
// close with a dedicated migration-round-trip test (ledger-finance ruling
// §3(i), "the migration refuses to apply while any intent has more than
// one succeeded deposit attempt").
package reconciliation

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/payments"
)

// TestINVDEP1_Recon_M_CapturedUnposted_ReplacesDuplicate drives a real
// second capture through payments.ApplyReceiptEvidence (never a
// synthetic ledger.Post), then runs the payment_statement stream and
// asserts the FUTURE reconciliation shape: exactly one
// 'pay_captured_unposted' mismatch, and no 'pay_duplicate'.
func TestINVDEP1_Recon_M_CapturedUnposted_ReplacesDuplicate(t *testing.T) {
	w := newPayWorld(t)
	declined, child := w.cascade(t)
	w.succeed(t, w.mockB, payProvB, child) // the intent's one real, first capture

	// The first provider's late success on the now-declined parent - the
	// exact same shape the original TestPaymentStatement_Kind_
	// DuplicatePlatformSuccess described as "T13, a second capture",
	// but driven through the REAL receipt path this time (the original
	// test's own comment noted the receipt path used to fail this shape
	// with a unique violation - it no longer does, since postDepositSuccess
	// grew a dedicated "T13 second capture posts" branch; §28 replaces
	// THAT branch, which is exactly what this test red-lines).
	w.mockA.Resolve(*declined.ProviderReference, payments.OutcomeSucceeded, "", false)
	disposition := w.applyReceipt(t, payProvA, payments.ReceiptEvidence{
		EventType: "deposit", ProviderReference: *declined.ProviderReference,
		Outcome: payments.OutcomeSucceeded, Amount: declined.Amount, AssetCode: declined.AssetCode,
	})
	t.Logf("late success on the declined parent: disposition=%s", disposition)

	declinedAfter := w.attempt(t, declined.ID)
	if declinedAfter.State != payments.AttemptDisputed {
		t.Errorf("ADR 0095 §28.4 T13d: expected the parent disputed after its late success, got state=%s (disposition=%s)", declinedAfter.State, disposition)
	}
	if declinedAfter.TerminalReason == nil || *declinedAfter.TerminalReason != "multiple_success_for_intent" {
		t.Errorf("expected terminal_reason=multiple_success_for_intent, got %v", declinedAfter.TerminalReason)
	}
	if declinedAfter.LedgerTransactionID != nil {
		t.Errorf("a T13d attempt must carry NO ledger link, got %s", *declinedAfter.LedgerTransactionID)
	}

	_, ms := w.run(t, w.srcA, PaymentStatementOptions{})
	t.Logf("mismatches after the real second-capture attempt: %s", renderMismatches(ms))

	var sawCapturedUnposted, sawDuplicate bool
	for _, m := range ms {
		switch m.MismatchKind {
		case MismatchKind("pay_captured_unposted"):
			sawCapturedUnposted = true
		case MismatchKindPayDuplicate:
			sawDuplicate = true
		}
	}
	if !sawCapturedUnposted {
		t.Errorf("ADR 0095 §28.9: expected a 'pay_captured_unposted' mismatch for the disputed, never-posted real capture; got:\n%s", renderMismatches(ms))
	}
	if sawDuplicate {
		t.Errorf("ADR 0095 §28.9: 'pay_duplicate' must no longer fire once the second capture is held disputed/unposted; got:\n%s", renderMismatches(ms))
	}
}

// TestINVDEP1_Recon_M_DuplicateDetector_LegacyDataShape is the ORIGINAL
// TestPaymentStatement_Kind_DuplicatePlatformSuccess fixture, kept
// verbatim (ledger-finance ruling §4: "pay_duplicate ... is kept as an
// integrity detector"). It builds the pre-§28 "legacy data shape" - two
// succeeded deposit attempts for one intent - WITHOUT going through the
// receipt path (direct ledger.Post + payments.ApplySuccess), and proves
// the detector still reports it. This must keep passing after migration
// 0107 lands too: 0107 makes this shape unreachable for NEW data, but a
// database that predates it (or a dropped index) must still be caught
// here, per the ruling's own words ("any occurrence means an index was
// dropped or the data predates the migration: a P1 integrity alert").
func TestINVDEP1_Recon_M_DuplicateDetector_LegacyDataShape(t *testing.T) {
	w := newPayWorld(t)
	declined, child := w.cascade(t)
	w.succeed(t, w.mockB, payProvB, child)

	w.mockA.Resolve(*declined.ProviderReference, payments.OutcomeSucceeded, "", false)
	err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		provider, ref := payProvA, *declined.ProviderReference
		res, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: w.f.tenantID, TransactionType: ledger.TxDeposit, IdempotencyKey: provider + ":" + ref,
			ProviderID: &provider, ProviderTxID: &ref, CorrelationID: *declined.DepositIntentID,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: w.f.clearingID, Direction: ledger.Debit, Amount: declined.Amount},
				{LedgerAccountID: w.f.cashAccountID, Direction: ledger.Credit, Amount: declined.Amount},
			},
		})
		if err != nil {
			return err
		}
		return payments.ApplySuccess(ctx, tx, declined.ID, payments.SuccessEvidence{
			Evidence: payments.EvidenceCallback, ProviderReference: ref, LedgerTransactionID: &res.TransactionID,
		})
	})
	if err != nil {
		t.Fatalf("synthetic legacy-shape second capture: %v", err)
	}
	_, ms := w.run(t, w.srcA, PaymentStatementOptions{})
	mustOnePay(t, ms, MismatchKindPayDuplicate, "deposit_intent=", "check=duplicate_success")
}
