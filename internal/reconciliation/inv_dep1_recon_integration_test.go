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
// QA adjudication correction (docs/plans/payment-readiness/
// qa-fh3-adjudication.md): migration 0107 now exists and is applied by
// testPool(t)'s TEST_DATABASE_URL, so the legacy shape above can no
// longer be built there - the SAME unique indexes this shape is meant to
// predate would refuse it (that is the correct, intended behaviour of
// 0107 on a fully-migrated DB; it just means this ONE test needs its own
// pre-0107 scratch database, like every other migration-boundary test in
// this repository already does). TestINVDEP1_Recon_M_DuplicateDetector_
// LegacyDataShape now builds its fixture on a dedicated scratch database
// migrated only up to and including 0106 (one migration BEFORE 0107),
// exactly like internal/payments' own migration0101ScratchBefore101/
// migration0106Scratch pattern, and additionally migrates that SAME
// database up to 0107 afterward to confirm the pre-flight refuses it
// (ledger-finance ruling §3(i)) - closing the gap this file's previous
// revision flagged as NOT IMPLEMENTED.
package reconciliation

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
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

// --- pre-0107 scratch database plumbing (legacy-shape detector only) ----

// reconMigrationsDir is the real, on-disk migrations/ directory - the
// same one every other migration-boundary helper in this repository
// (e.g. internal/payments' realMigrationsDir) resolves relative to its
// own package directory.
func reconMigrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// migration0106ReconVersion derives 0107's own predecessor version from
// the real migrations/ directory's filename for 0107 (106 = 107 - 1),
// rather than a hard-coded integer literal, exactly like internal/
// payments' migration0101Version does for 0101 - so a rename or a
// numbering change is caught here instead of silently testing the wrong
// boundary.
func migration0106ReconVersion(t *testing.T) int64 {
	t.Helper()
	dir := reconMigrationsDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "0107_deposit_intent_double_credit_backstop.up.sql") {
			return 106
		}
	}
	t.Fatalf("no 0107_deposit_intent_double_credit_backstop.up.sql found under %s - migration 0107 is expected to exist on this branch", dir)
	return 0
}

// migration0106ReconDirThroughSelf copies every on-disk migration with a
// numeric filename prefix <= through into a fresh temp dir, so a scratch
// database can stop exactly at migration 0106 (one before the 0107
// double-credit backstop) - the same copy-a-prefix-bounded-subset pattern
// internal/payments' migration0101Dir and this package's own
// migration0098DirThroughSelf both already use.
func migration0106ReconDirThroughSelf(t *testing.T, through int64) string {
	t.Helper()
	src := reconMigrationsDir(t)
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") || len(name) < 4 {
			continue
		}
		v, err := strconv.Atoi(name[:4])
		if err != nil || int64(v) > through {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// migration0106ReconScratch creates a PRIVATE scratch database (never the
// shared TEST_DATABASE_URL) migrated up to and including 0106 only - the
// pre-ADR-0095-§28 shape the legacy-data detector test needs to even be
// able to construct its fixture.
func migration0106ReconScratch(t *testing.T, prefix string) *db.Pool {
	t.Helper()
	url := scratchdb.New(t, prefix)
	pool, err := db.Connect(context.Background(), url, 10, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	t.Cleanup(pool.Close)
	through := migration0106ReconVersion(t)
	dir := migration0106ReconDirThroughSelf(t, through)
	applied, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("migrate scratch up through %d: %v", through, err)
	}
	if len(applied) == 0 || applied[len(applied)-1] != through {
		t.Fatalf("expected %d to be the last applied migration, got %v", through, applied)
	}
	return pool
}

// newPayWorldOnPool is newPayWorld's own wiring (mock providers,
// capability registration, orchestrator, statement sources), duplicated
// here - rather than refactoring newPayWorld itself - so this QA
// correction stays confined to this file: newPayWorld always uses
// testPool(t) (the shared, fully-migrated TEST_DATABASE_URL), which is
// exactly what the legacy-shape test below can no longer use once 0107
// is applied to it.
func newPayWorldOnPool(t *testing.T, pool *db.Pool) *payWorld {
	t.Helper()
	w := &payWorld{pool: pool, f: seedFixture(t, pool)}
	w.mockA = payments.NewMockProvider(payProvA, "EUR")
	w.mockB = payments.NewMockProvider(payProvB, "EUR")
	w.mockB.AcceptAllAmounts = true
	w.orch = payments.NewOrchestrator(
		map[string]payments.PaymentProvider{payProvA: w.mockA, payProvB: w.mockB},
		payments.MultiWebhookCredentialResolver{
			payProvA: payments.NewMockWebhookCredentials(w.mockA),
			payProvB: payments.NewMockWebhookCredentials(w.mockB),
		})
	w.registerCapability(t, w.mockA, 10)
	w.registerCapability(t, w.mockB, 50)
	w.srcA = payments.NewMockStatementSource(w.mockA, payments.MockCredentialResolver{})
	w.srcB = payments.NewMockStatementSource(w.mockB, payments.MockCredentialResolver{})
	return w
}

// TestINVDEP1_Recon_M_DuplicateDetector_LegacyDataShape is the ORIGINAL
// TestPaymentStatement_Kind_DuplicatePlatformSuccess fixture, kept
// verbatim in its own construction (ledger-finance ruling §4:
// "pay_duplicate ... is kept as an integrity detector"). It builds the
// pre-§28 "legacy data shape" - two succeeded deposit attempts for one
// intent - WITHOUT going through the receipt path (direct ledger.Post +
// payments.ApplySuccess), on a scratch database migrated only to 0106
// (one migration BEFORE the 0107 backstop that would otherwise refuse
// this exact shape), and proves:
//  1. the detector still reports 'pay_duplicate' on this legacy shape
//     (must keep passing forever - the shape becomes unreachable in NEW
//     data once 0107 is applied, but old data or a dropped index must
//     still be caught);
//  2. migrating that SAME database the rest of the way to 0107
//     afterward is REFUSED by the attempts pre-flight, on this exact
//     data (ledger-finance ruling §3(i): "the migration refuses to apply
//     while any intent has more than one succeeded deposit attempt").
func TestINVDEP1_Recon_M_DuplicateDetector_LegacyDataShape(t *testing.T) {
	pool := migration0106ReconScratch(t, "invdep1_recon_legacy_")
	w := newPayWorldOnPool(t, pool)
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

	// Confirm the 0107 pre-flight refuses THIS exact data: migrate the
	// SAME database (which still carries the two-succeeded-attempts
	// legacy shape just proven above) the rest of the way to the latest
	// on-disk migration (0107).
	_, err = pool.MigrateUp(context.Background(), reconMigrationsDir(t))
	if err == nil {
		t.Fatal("ledger-finance ruling §3(i): expected migration 0107 to REFUSE to apply while this intent has more than one succeeded deposit attempt, but it succeeded")
	}
	if !strings.Contains(err.Error(), "more than one succeeded deposit attempt") {
		t.Fatalf("expected the 0107 attempts pre-flight's own refusal message, got a different error: %v", err)
	}
	t.Logf("migration 0107 correctly refused on the legacy duplicate-succeeded-attempts shape: %v", err)
}
