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
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
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

// mismatchKinds is a small helper shared by the two tests below.
func mismatchKinds(ms []Mismatch) map[MismatchKind]bool {
	out := map[MismatchKind]bool{}
	for _, m := range ms {
		out[m.MismatchKind] = true
	}
	return out
}

// TestINVDEP1_C2_CapturedUnposted_StandsAcrossStatementWindow closes
// ledger-finance review C2/F2 (rv-fh3-ledger.md, 076e42e): before this
// fix, pay_captured_unposted was raised ONLY inside matchPayment, when a
// statement line for THIS run happens to match the disputed attempt - so
// once the capture's own settlement period passed (no line ever names it
// again in a later run), the exposure silently dropped out of
// reconciliation entirely, even though the money is still unrefunded.
// Under HD-LEDGER-UNALLOC-1 (A) this report is the ONLY standing record
// of that off-ledger money, so this is a financial-correctness fix, not
// polish.
//
// Builds the SAME real T13d scenario as TestINVDEP1_Recon_M_
// CapturedUnposted_ReplacesDuplicate, runs it once against the real
// statement source (the matched-line path, already covered there), then
// runs it AGAIN against a statement with NO lines at all for this
// provider (a fresh, empty payFixedSource - simulating exactly "the
// capture's own statement period has passed, no line ever names it
// again") and asserts pay_captured_unposted STILL fires, via
// checkUnmatchedAttempts' new, unwindowed standing check.
func TestINVDEP1_C2_CapturedUnposted_StandsAcrossStatementWindow(t *testing.T) {
	w := newPayWorld(t)
	declined, child := w.cascade(t)
	w.succeed(t, w.mockB, payProvB, child)

	w.mockA.Resolve(*declined.ProviderReference, payments.OutcomeSucceeded, "", false)
	w.applyReceipt(t, payProvA, payments.ReceiptEvidence{
		EventType: "deposit", ProviderReference: *declined.ProviderReference,
		Outcome: payments.OutcomeSucceeded, Amount: declined.Amount, AssetCode: declined.AssetCode,
	})
	declinedAfter := w.attempt(t, declined.ID)
	if declinedAfter.State != payments.AttemptDisputed || declinedAfter.TerminalReason == nil || *declinedAfter.TerminalReason != "multiple_success_for_intent" {
		t.Fatalf("setup: expected disputed/multiple_success_for_intent, got state=%s reason=%v", declinedAfter.State, declinedAfter.TerminalReason)
	}

	// Run 1: the real statement source, which DOES carry a line for this
	// reference (the mock provider's own record of the late success) -
	// the matched-line path, baseline confirmation.
	_, ms1 := w.run(t, w.srcA, PaymentStatementOptions{})
	if !mismatchKinds(ms1)[MismatchKind("pay_captured_unposted")] {
		t.Fatalf("setup: expected pay_captured_unposted on the matched-line run; got:\n%s", renderMismatches(ms1))
	}

	// Run 2: an EMPTY statement for this same provider - no line at all
	// for this reference, exactly the "statement window has passed"
	// scenario. Must STILL raise pay_captured_unposted.
	emptySrc := payFixedSource{provider: payProvA, stmt: wideCoverage()}
	_, ms2 := w.run(t, emptySrc, PaymentStatementOptions{})
	if !mismatchKinds(ms2)[MismatchKind("pay_captured_unposted")] {
		t.Errorf("C2: expected pay_captured_unposted to STAND across a run with no statement line at all for this reference; got:\n%s", renderMismatches(ms2))
	}
}

// TestINVDEP1_C5_CapturedUnposted_ClearsOnReversalLineOrTombstone closes
// ledger-finance review C5: a test that kills mutant RTMB (the
// pay_captured_unposted clearing conditions replaced by `true`, which
// survived the full internal/payments and internal/reconciliation suites
// at a927aed). Exercises BOTH of §28.9's clearing signals independently,
// each on its OWN fresh T13d fixture, each checked via a run with NO
// matching statement line at all (the standing/unmatched path C2 adds),
// so this test is impossible to satisfy by accident via the
// matched-line path alone:
//
//  1. a PSP-reported deposit_reversal STATEMENT line naming this
//     reference as its own original (m.reversalOriginals) clears it;
//  2. a real platform-side TOMBSTONE ledger row for this reference
//     (m.ledgerByRef) clears it - produced here via a genuine reversal
//     callback for a never-posted capture, which applyReversalReceipt
//     Evidence's own tombstone branch takes (LedgerTransactionID is nil
//     on a T13d attempt by construction).
func TestINVDEP1_C5_CapturedUnposted_ClearsOnReversalLineOrTombstone(t *testing.T) {
	buildDisputed := func(t *testing.T) (*payWorld, payments.PaymentAttempt) {
		w := newPayWorld(t)
		declined, child := w.cascade(t)
		w.succeed(t, w.mockB, payProvB, child)
		w.mockA.Resolve(*declined.ProviderReference, payments.OutcomeSucceeded, "", false)
		w.applyReceipt(t, payProvA, payments.ReceiptEvidence{
			EventType: "deposit", ProviderReference: *declined.ProviderReference,
			Outcome: payments.OutcomeSucceeded, Amount: declined.Amount, AssetCode: declined.AssetCode,
		})
		declinedAfter := w.attempt(t, declined.ID)
		if declinedAfter.State != payments.AttemptDisputed || declinedAfter.TerminalReason == nil || *declinedAfter.TerminalReason != "multiple_success_for_intent" {
			t.Fatalf("setup: expected disputed/multiple_success_for_intent, got state=%s reason=%v", declinedAfter.State, declinedAfter.TerminalReason)
		}
		return w, declinedAfter
	}

	t.Run("reversal_line", func(t *testing.T) {
		w, disputed := buildDisputed(t)
		revLine := payLineFor(payProvA, "c5-rev-"+disputed.ID.String()[:8], "", statement.PaymentLineDepositReversal, statement.PaymentStatusSucceeded, disputed.Amount)
		revLine.OriginalProviderReference = *disputed.ProviderReference
		src := payFixedSource{provider: payProvA, stmt: wideCoverage(revLine)}
		_, ms := w.run(t, src, PaymentStatementOptions{})
		if mismatchKinds(ms)[MismatchKind("pay_captured_unposted")] {
			t.Errorf("C5: a PSP-reported reversal line naming this reference as its original must CLEAR pay_captured_unposted; got:\n%s", renderMismatches(ms))
		}
	})

	t.Run("tombstone", func(t *testing.T) {
		w, disputed := buildDisputed(t)
		if disputed.LedgerTransactionID != nil {
			t.Fatalf("setup: a T13d attempt must carry no ledger link, got %s", *disputed.LedgerTransactionID)
		}
		w.deliverReversal(t, "c5-tomb-"+disputed.ID.String()[:8], *disputed.ProviderReference, disputed.Amount)
		var tombCount int64
		if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE transaction_type='tombstone' AND provider_tx_id=$1`, *disputed.ProviderReference).Scan(&tombCount)
		}); err != nil {
			t.Fatal(err)
		}
		if tombCount != 1 {
			t.Fatalf("setup: expected exactly one tombstone for this reference, got %d", tombCount)
		}
		// No statement line at all - the tombstone alone must clear it.
		emptySrc := payFixedSource{provider: payProvA, stmt: wideCoverage()}
		_, ms := w.run(t, emptySrc, PaymentStatementOptions{})
		if mismatchKinds(ms)[MismatchKind("pay_captured_unposted")] {
			t.Errorf("C5: a real tombstone ledger row for this reference must CLEAR pay_captured_unposted even with no statement line at all; got:\n%s", renderMismatches(ms))
		}
	})

	// Code review R1 (rv-fh3-code-review.md, 95a1c34): the disputed
	// attempt's OWN statement line, if the PSP itself now reports THAT
	// line's own status as 'reversed' (not a SEPARATE deposit_reversal
	// line naming it as an original - this is the SAME reference,
	// reported directly as refunded), must ALSO clear pay_captured_
	// unposted - never keep it standing just because SOME line still
	// names the reference. Kills mutant X15 (the "only succeeded counts"
	// narrowing, if ever widened back to accept 'reversed' too).
	t.Run("own_line_reported_reversed", func(t *testing.T) {
		w, disputed := buildDisputed(t)
		ownLine := payLineFor(payProvA, *disputed.ProviderReference, "", statement.PaymentLineDeposit, statement.PaymentStatusReversed, disputed.Amount)
		src := payFixedSource{provider: payProvA, stmt: wideCoverage(ownLine)}
		_, ms := w.run(t, src, PaymentStatementOptions{})
		if mismatchKinds(ms)[MismatchKind("pay_captured_unposted")] {
			t.Errorf("R1: a statement line reporting THIS reference's own status as reversed must CLEAR pay_captured_unposted, never keep it standing; got:\n%s", renderMismatches(ms))
		}
	})
}

// TestINVDEP1_X16_TerminalReasonFilter_PrecedenceCaseNeverReportsCapturedUnposted
// closes code-review R3's X16 gap (rv-fh3-code-review.md, 95a1c34): both
// pay_captured_unposted call sites (matchPayment's matched-line case and
// checkUnmatchedAttempts' standing case) gate on
// a.terminalReason == "multiple_success_for_intent" specifically - a
// disputed attempt with the OTHER T13t/T13d terminal reason
// (reversal_tombstone_precedes_success, ADR 0095 §28.6.3's "the reversal
// precedes the success" precedence case) never occupied the INV-DEP-1
// slot with real, unposted captured money in the first place (the
// tombstone arrived BEFORE any success, so the attempt was disputed with
// NO capture ever recorded) and must never be reported as
// pay_captured_unposted, regardless of statement content.
func TestINVDEP1_X16_TerminalReasonFilter_PrecedenceCaseNeverReportsCapturedUnposted(t *testing.T) {
	w := newPayWorld(t)
	declined, _ := w.cascade(t)

	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return payments.ApplyTombstonePrecedesSuccess(ctx, tx, declined.ID, payments.EvidenceCallback)
	}); err != nil {
		t.Fatalf("setup: ApplyTombstonePrecedesSuccess (T13t): %v", err)
	}
	precedence := w.attempt(t, declined.ID)
	if precedence.State != payments.AttemptDisputed || precedence.TerminalReason == nil || *precedence.TerminalReason != payments.TerminalReasonTombstonePrecedesSuccess {
		t.Fatalf("setup: expected disputed/%s, got state=%s reason=%v", payments.TerminalReasonTombstonePrecedesSuccess, precedence.State, precedence.TerminalReason)
	}
	if precedence.LedgerTransactionID != nil {
		t.Fatalf("setup: a T13t attempt must carry no ledger link, got %s", *precedence.LedgerTransactionID)
	}

	// Both the matched-line run (a statement line naming this reference,
	// still reported succeeded - the PSP's own view before the platform's
	// tombstone caught up) and the standing/unmatched run (no line at all)
	// must never report pay_captured_unposted for this terminal_reason.
	line := payLineFor(payProvA, *precedence.ProviderReference, "", statement.PaymentLineDeposit, statement.PaymentStatusSucceeded, precedence.Amount)
	matchedSrc := payFixedSource{provider: payProvA, stmt: wideCoverage(line)}
	_, msMatched := w.run(t, matchedSrc, PaymentStatementOptions{})
	if mismatchKinds(msMatched)[MismatchKind("pay_captured_unposted")] {
		t.Errorf("X16: a reversal_tombstone_precedes_success attempt must never report pay_captured_unposted on the matched-line path; got:\n%s", renderMismatches(msMatched))
	}

	emptySrc := payFixedSource{provider: payProvA, stmt: wideCoverage()}
	_, msStanding := w.run(t, emptySrc, PaymentStatementOptions{})
	if mismatchKinds(msStanding)[MismatchKind("pay_captured_unposted")] {
		t.Errorf("X16: a reversal_tombstone_precedes_success attempt must never report pay_captured_unposted on the standing/unmatched path; got:\n%s", renderMismatches(msStanding))
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
	// PRH-2 K3: the payment_statement stream now reads migration 0115's
	// persisted-evidence tables, so the detector can no longer run on a database
	// frozen at 0106. Part 1 therefore runs the detector on a FULLY migrated
	// scratch database whose two 0107 backstop indexes are dropped by its owner
	// (the exact "old data or a dropped index must still be caught" case); part
	// 2 builds the same shape on a database migrated only to 0106, where the
	// stream is not run, and proves the 0107 pre-flight refuses it.
	buildLegacyShape := func(t *testing.T, w *payWorld) {
		t.Helper()
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
	}

	// Part 1: the detector on the legacy shape (fully migrated, 0107 indexes dropped).
	headURL := scratchdb.New(t, "invdep1_recon_head_")
	headPool, err := db.Connect(context.Background(), headURL, 10, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	t.Cleanup(headPool.Close)
	if _, err := headPool.MigrateUp(context.Background(), reconMigrationsDir(t)); err != nil {
		t.Fatalf("migrate scratch to the latest: %v", err)
	}
	if err := headPool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DROP INDEX payment_attempts_one_succeeded_deposit_per_intent`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DROP INDEX ledger_transactions_one_deposit_per_intent`)
		return err
	}); err != nil {
		t.Fatalf("drop the 0107 backstop indexes: %v", err)
	}
	wHead := newPayWorldOnPool(t, headPool)
	buildLegacyShape(t, wHead)
	_, ms := wHead.run(t, wHead.srcA, PaymentStatementOptions{})
	mustOnePay(t, ms, MismatchKindPayDuplicate, "deposit_intent=", "check=duplicate_success")

	// Part 2: the 0107 pre-flight refuses THIS exact data on a database migrated
	// only to 0106.
	pool := migration0106ReconScratch(t, "invdep1_recon_legacy_")
	w := newPayWorldOnPool(t, pool)
	// H-SEC-5/11: InitiateDepositAttempt's initiation gate takes migration 0118's advisory-lock key function,
	// which does not exist on this pre-0107 database. Install ONLY that function (byte-identical body); the
	// migrate-forward below stops at 0107's refusal, long before 0118 would CREATE it again.
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `CREATE FUNCTION tenant_status_gate_key(p_tenant_id uuid) RETURNS bigint
			LANGUAGE sql IMMUTABLE PARALLEL SAFE
			SET search_path = pg_catalog, public, pg_temp
			AS $$ SELECT hashtextextended('tenant_status_gate:' || p_tenant_id::text, 0) $$`)
		return err
	}); err != nil {
		t.Fatalf("install tenant_status_gate_key on the pre-0107 scratch database: %v", err)
	}
	buildLegacyShape(t, w)
	// Migrate the SAME database (which carries the two-succeeded-attempts legacy
	// shape) the rest of the way to the latest on-disk migration.
	_, err = pool.MigrateUp(context.Background(), reconMigrationsDir(t))
	if err == nil {
		t.Fatal("ledger-finance ruling §3(i): expected migration 0107 to REFUSE to apply while this intent has more than one succeeded deposit attempt, but it succeeded")
	}
	if !strings.Contains(err.Error(), "more than one succeeded deposit attempt") {
		t.Fatalf("expected the 0107 attempts pre-flight's own refusal message, got a different error: %v", err)
	}
	t.Logf("migration 0107 correctly refused on the legacy duplicate-succeeded-attempts shape: %v", err)
}
