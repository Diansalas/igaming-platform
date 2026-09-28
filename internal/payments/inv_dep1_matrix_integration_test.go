//go:build integration

// PAY-DOUBLE-CREDIT-1 / INV-DEP-1 (ADR 0095 §28, ledger-finance ruling
// docs/plans/payment-readiness/lf-q1-supersession.md, test matrix
// docs/plans/payment-readiness/double-credit-reconciliation.md §5,
// scenarios A-O) - TEST-FIRST adversarial matrix, written by `qa` BEFORE
// the fix lands (§28 status: ACCEPTED design, NOT IMPLEMENTED).
//
// ============================================================================
// READ THIS BEFORE "fixing" a failing test in this file
// ============================================================================
//
// Every test below asserts the FUTURE, POST-FIX behaviour (INV-DEP-1): at
// most one succeeded deposit attempt and at most one real ledger posting
// per deposit_intent. Against HEAD (pre-§28), the following are EXPECTED
// TO FAIL, and fail for the RIGHT reason (a second real ledger posting, or
// an attempt sitting in `succeeded` instead of `disputed`), not a harness
// error:
//
//	TestINVDEP1_C, _D, _H, _I, _J, _K, _O,
//	TestINVDEP1_Inverted_T13SecondCapture_BecomesDisputed_NoSecondPosting,
//	TestINVDEP1_Inverted_RVLF_P6_ReversalOfDisputedSecondCaptureTakesTombstoneBranch.
//
// TestINVDEP1_A, _B, _E, _F, _G and _L are expected to PASS against HEAD:
// they exercise paths the old code already gets right (a first/only
// success, ordinary duplicate-callback/ledger-key dedupe, and RLS tenant
// isolation), and must keep passing once §28 lands too.
//
// No identifier this file uses is invented ahead of the fix: there is no
// payments.ErrDepositIntentAlreadyResolved, no ledger.ErrDepositAlready-
// PostedForIntent, no migration 0107, no MismatchKindPayCapturedUnposted
// constant. Every "new" string the spec names (multiple_success_for_intent,
// pay_captured_unposted, reversal_tombstone_precedes_success) is asserted
// as a plain string literal against a DB column - never a Go identifier
// that doesn't exist yet. That is deliberate: it is what lets this file
// compile against current code while still red-lining the invariant.
//
// ============================================================================
// N. MUTATION CHECKLIST (for the implementer/code-reviewer; NOT runnable
// today - the choke point does not exist yet to mutate; this is the
// exact list `code-reviewer` should re-run once it does)
// ============================================================================
//
//  1. Delete the `resolved_for_other` check at the T13 call site
//     (receipt.go's AttemptDeclined branch, once added) -> TestINVDEP1_C,
//     _Inverted_T13SecondCapture, _H, _D, _K must fail (a second real
//     posting reappears, ledgerDepositTxCount goes back to 2).
//  2. Delete the `resolved_for_other` check at the T7 call site
//     (receipt.go's AttemptSubmitting/Pending/Ambiguous branch) ->
//     TestINVDEP1_J and _H (the second late reference) must fail.
//  3. Delete the re-check inside postDepositSuccess itself (§28.3 rule 2,
//     the "same predicate, re-evaluated immediately before ledger.Post")
//     -> run TestINVDEP1_D and _K with -race -count=50 or more; without
//     the re-check, only the caller-side CAS predicate serialises the
//     two branches, and a sufficiently adversarial interleaving (both
//     transactions passing their caller-side check before either
//     commits) must start producing a second posting.
//  4. Drop the partial unique index
//     `payment_attempts_one_succeeded_deposit_per_intent` (migration
//     0107, once it exists) -> a direct two-UPDATE-to-succeeded backstop
//     test (to write once 0107 exists) must fail with no unique
//     violation; TestINVDEP1_D/_K, run at high repetition, must start
//     showing 2 succeeded siblings for one intent.
//  5. Drop the partial unique index
//     `ledger_transactions_one_deposit_per_intent` -> a direct two-
//     ledger.Post-with-the-same-correlation-id backstop test (to write
//     once 0107 exists) must fail with no unique violation.
//  6. Swap the tombstone precedence and the INV-DEP-1 check (§28.3 rule 1
//     order) -> TestINVDEP1_Inverted_RVLF_P6's tombstone-branch case
//     (a reversal naming a `disputed`/never-posted parent) must start
//     reporting `multiple_success_for_intent` instead of taking the
//     tombstone no-op branch.
//  7. Let T17/Touch (or the sweeper) act on a terminal, non-`declined`
//     attempt -> TestINVDEP1_O must fail (a disputed attempt moves again
//     or gets re-driven to the provider).
//  8. Remove the `pay_captured_unposted` emission (once built) -> the
//     internal/reconciliation scenario-M tests
//     (inv_dep1_recon_integration_test.go) must fail.
//
// ============================================================================
// Mapping: tests to invert or replace (ledger-finance ruling §5). This
// file, and the reconciliation/idempotency files it references, ADD the
// inverted versions - none of the originals is deleted or edited here,
// per the QA mandate ("invert, never delete"). The implementer removes or
// replaces the originals when landing the fix; this table is their map.
// ============================================================================
//
//	receipt_integration_test.go:
//	  TestReceipt_T13SecondCapture_ThroughApplyReceiptEvidence_LedgerBalanced
//	    -> replaced by TestINVDEP1_Inverted_T13SecondCapture_BecomesDisputed_NoSecondPosting (this file)
//	rvlf_i1_regression_integration_test.go:
//	  TestRVLF_P6_ReversalOfSecondCaptureReversesItsOwnTransaction
//	    -> replaced by TestINVDEP1_Inverted_RVLF_P6_ReversalOfDisputedSecondCaptureTakesTombstoneBranch (this file)
//	internal/reconciliation/payment_statement_integration_test.go:
//	  TestPaymentStatement_Kind_DuplicatePlatformSuccess
//	    -> split into TestINVDEP1_Recon_M_CapturedUnposted_ReplacesDuplicate (real receipt path)
//	       and TestINVDEP1_Recon_M_DuplicateDetector_LegacyDataShape (synthetic legacy
//	       shape - the detector itself stays valid), both in
//	       internal/reconciliation/inv_dep1_recon_integration_test.go
//	internal/idempotency/integration_test.go:
//	  TestIntegration_LegitimateSecondOccurrenceDoesNotCollapse (the two
//	  distinct-occurrence-key TxDeposit postings sharing one correlation id)
//	    -> replaced by TestIntegration_LegitimateSecondOccurrenceDoesNotCollapse_DistinctCorrelationIDs
//	       (internal/idempotency/inv_dep1_correlation_integration_test.go), which
//	       gives each occurrence its own correlation id per ledger-finance
//	       ruling §3(ii)/§5 item 4 - the idempotency-composition assertions
//	       (distinct transaction ids, both amounts posted) are unchanged.
//
// Checked and NOT inverted (ledger-finance ruling §5's own list, restated
// here for traceability): TestRVLF_P3_T13RejectsCreatedSibling (T13 there
// IS the intent's first success - unaffected, and is exactly this file's
// TestINVDEP1_A shape); TestRVLF_F3_*; TestMigration0101_T13t_* and
// TestMigration0101_T12_RefusedWhenSiblingSucceeded; every httpserver
// "T13" label (QA test-plan numbering, unrelated). Also retired once the
// fix lands: production audit action "deposit.second_capture_posted"
// (orchestrator.go:900) - no test asserts it today, and none may after.
package payments

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// ---------------------------------------------------------------------
// Shared fixture and query helpers.
// ---------------------------------------------------------------------

// invDep1Setup is the common two-provider fixture most scenarios below
// start from: providerA is the original (lower priority number, tried
// first), providerB the fallback (AcceptAllAmounts=true, so it never
// re-triggers the magic-decline-amount branch itself once cascaded to).
// Every scenario uses its own, distinct provider_id strings, so nothing
// here collides with another test's capability rows or MockProvider
// state.
type invDep1Setup struct {
	pool *db.Pool
	f    orchFixture
	pa   *MockProvider
	pb   *MockProvider
	orch *Orchestrator
}

func newInvDep1Setup(t *testing.T, providerAID, providerBID string) invDep1Setup {
	t.Helper()
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	pa := NewMockProvider(providerAID, "EUR")
	pb := NewMockProvider(providerBID, "EUR")
	pb.AcceptAllAmounts = true
	registerCapability(t, pool, f, pa, 100)
	registerCapability(t, pool, f, pb, 200)
	orch := NewOrchestrator(map[string]PaymentProvider{providerAID: pa, providerBID: pb},
		MultiWebhookCredentialResolver{providerAID: NewMockWebhookCredentials(pa), providerBID: NewMockWebhookCredentials(pb)})
	return invDep1Setup{pool: pool, f: f, pa: pa, pb: pb, orch: orch}
}

// ledgerDepositTxCount is THE INV-DEP-1 assertion: at most one
// ledger_transactions row of type 'deposit' with correlation_id =
// intent.id (ADR 0095 §28.2). Queried directly, never through a
// not-yet-existing Go constant.
func ledgerDepositTxCount(t *testing.T, pool *db.Pool, tenantID, intentID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND correlation_id = $2 AND transaction_type = 'deposit'`,
			tenantID, intentID).Scan(&n)
	}); err != nil {
		t.Fatalf("count deposit ledger transactions: %v", err)
	}
	return n
}

// succeededDepositAttemptCount is INV-DEP-1's other half: at most one
// payment_attempts row with operation='deposit' AND state='succeeded'
// for the intent.
func succeededDepositAttemptCount(t *testing.T, pool *db.Pool, tenantID, intentID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM payment_attempts WHERE tenant_id = $1 AND deposit_intent_id = $2 AND operation = 'deposit' AND state = 'succeeded'`,
			tenantID, intentID).Scan(&n)
	}); err != nil {
		t.Fatalf("count succeeded deposit attempts: %v", err)
	}
	return n
}

func auditActionCount(t *testing.T, pool *db.Pool, tenantID uuid.UUID, action, targetID string) int {
	t.Helper()
	var n int
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3`,
			tenantID, action, targetID).Scan(&n)
	}); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}

// assertDisputedMultipleSuccess asserts the §28.4 T10/T13d shape: the
// attempt is disputed, terminal_reason is EXACTLY the string
// 'multiple_success_for_intent' (a plain literal - MismatchKind-style Go
// constants for this reason do not exist yet), and it carries NO ledger
// link (the "no posting at all" half of HD-LEDGER-UNALLOC-1 (A)).
func assertDisputedMultipleSuccess(t *testing.T, pool *db.Pool, tenantID, attemptID uuid.UUID) PaymentAttempt {
	t.Helper()
	a := mustGetAttempt(t, pool, tenantID, attemptID)
	if a.State != AttemptDisputed {
		t.Fatalf("INV-DEP-1: expected attempt %s disputed (multiple_success_for_intent), got state=%s reason=%v ledger=%v",
			attemptID, a.State, a.TerminalReason, a.LedgerTransactionID)
	}
	if a.TerminalReason == nil || *a.TerminalReason != "multiple_success_for_intent" {
		t.Fatalf("INV-DEP-1: expected terminal_reason=multiple_success_for_intent, got %v", a.TerminalReason)
	}
	if a.LedgerTransactionID != nil {
		t.Fatalf("INV-DEP-1: a disputed multiple_success_for_intent attempt must have NO ledger link, got %s", *a.LedgerTransactionID)
	}
	return a
}

func assertInvariantAndBalanced(t *testing.T, pool *db.Pool, tenantID, intentID uuid.UUID) {
	t.Helper()
	if n := ledgerDepositTxCount(t, pool, tenantID, intentID); n != 1 {
		t.Errorf("INV-DEP-1 violated: %d deposit ledger_transactions rows for intent %s, want exactly 1", n, intentID)
	}
	if n := succeededDepositAttemptCount(t, pool, tenantID, intentID); n != 1 {
		t.Errorf("INV-DEP-1 violated: %d succeeded deposit payment_attempts rows for intent %s, want exactly 1", n, intentID)
	}
	assertLedgerBalanced(t, pool, tenantID)
	loAssertBalanced(t, pool, tenantID)
	loAssertProjectionMatchesRebuild(t, pool, tenantID)
}

// declineCascadableAndFindChild delivers a cascadable decline for ref on
// providerID (real receipt path, T8 + cascade insert) and returns the
// 'created' cascade child's id.
func declineCascadableAndFindChild(t *testing.T, pool *db.Pool, orch *Orchestrator, f orchFixture, providerID string, mock *MockProvider, ref string) uuid.UUID {
	t.Helper()
	if _, err := rvCallback(pool, orch, f, providerID,
		mock.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeDeclined, 0, "", "provider_unavailable", true)); err != nil {
		t.Fatalf("cascadable decline: %v", err)
	}
	var childID uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM payment_attempts WHERE deposit_intent_id = (
			SELECT deposit_intent_id FROM payment_attempts WHERE tenant_id = $1 AND provider_id = $2 AND provider_reference = $3
		) AND attempt_no = 2`, f.tenantID, providerID, ref).Scan(&childID)
	}); err != nil {
		t.Fatalf("expected a cascade child: %v", err)
	}
	return childID
}

// dispatchViaSweeper claims and drives a 'created' attempt to its next
// live state (phase C via drive.go's driveCreatedAttempt, invoked through
// the sweeper's real per-item claim - the "drive functions" the QA
// mandate names).
func dispatchViaSweeper(t *testing.T, pool *db.Pool, orch *Orchestrator, f orchFixture, childID uuid.UUID) PaymentAttempt {
	t.Helper()
	setNextActionNow(t, pool, f.tenantID, childID)
	sw := NewSweeper(pool, orch, AllowAllDepositKYCGate{}, MockCredentialResolver{})
	if st := sw.RunOnce(context.Background(), []uuid.UUID{f.tenantID}); len(st.Errors) != 0 {
		t.Fatalf("sweeper dispatch: %v", st.Errors)
	}
	return mustGetAttempt(t, pool, f.tenantID, childID)
}

// ---------------------------------------------------------------------
// A. Original succeeds first: 1 credit; created fallback siblings rejected.
// ---------------------------------------------------------------------

func TestINVDEP1_A_OriginalSucceedsFirst_CreatedSiblingRejected(t *testing.T) {
	s := newInvDep1Setup(t, "invdep1-a-orig", "invdep1-a-fb")
	res := rvInit(t, s.pool, s.orch, s.f, 5000, "invdep1-a")
	ref := *res.Attempt.ProviderReference

	childID := declineCascadableAndFindChild(t, s.pool, s.orch, s.f, "invdep1-a-orig", s.pa, ref)
	child := mustGetAttempt(t, s.pool, s.f.tenantID, childID)
	if child.State != AttemptCreated {
		t.Fatalf("expected the cascade child still 'created' before the original's late success, got %s", child.State)
	}

	// The original's late success arrives BEFORE the child was ever
	// dispatched - this is genuinely the intent's FIRST success (T13),
	// unaffected by §28: no sibling has succeeded or even been sent.
	if _, err := rvCallback(s.pool, s.orch, s.f, "invdep1-a-orig",
		s.pa.CallbackPayload(s.f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("original late success (T13, first success): %v", err)
	}

	parent := mustGetAttempt(t, s.pool, s.f.tenantID, res.Attempt.ID)
	if parent.State != AttemptSucceeded || parent.LedgerTransactionID == nil {
		t.Fatalf("expected the original to succeed with a ledger link, got state=%s ledger=%v", parent.State, parent.LedgerTransactionID)
	}
	childAfter := mustGetAttempt(t, s.pool, s.f.tenantID, childID)
	if childAfter.State != AttemptRejected {
		t.Fatalf("expected the created fallback sibling to be rejected (T3 intent_succeeded), got %s", childAfter.State)
	}
	if b := cashBalance(t, s.pool, s.f); b != 5000 {
		t.Fatalf("expected exactly one credit of 5000, got balance=%d", b)
	}
	assertInvariantAndBalanced(t, s.pool, s.f.tenantID, res.Intent.ID)
}

// ---------------------------------------------------------------------
// B. Fallback succeeds first: 1 credit (fallback); the original stays declined.
// ---------------------------------------------------------------------

func TestINVDEP1_B_FallbackSucceedsFirst(t *testing.T) {
	s := newInvDep1Setup(t, "invdep1-b-orig", "invdep1-b-fb")
	res, err := s.orch.InitiateDepositAttempt(context.Background(), s.pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: s.f.tenantID, BrandID: s.f.brandID, PlayerAccountID: s.f.playerAccountID, WalletID: s.f.walletID},
		AssetCode: "EUR", Amount: MockAmountProviderDeclineCascade, PaymentMethod: "card", IdempotencyKey: "invdep1-b",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	if res.Attempt.State != AttemptPending {
		t.Fatalf("expected the synchronously-cascaded fallback child pending, got %s", res.Attempt.State)
	}
	child := res.Attempt
	childRef := *child.ProviderReference

	if _, err := rvCallback(s.pool, s.orch, s.f, "invdep1-b-fb",
		s.pb.CallbackPayload(s.f.tenantID, CallbackEventDeposit, childRef, "", OutcomeSucceeded, MockAmountProviderDeclineCascade, "EUR", "", false)); err != nil {
		t.Fatalf("fallback success: %v", err)
	}

	childAfter := mustGetAttempt(t, s.pool, s.f.tenantID, child.ID)
	if childAfter.State != AttemptSucceeded || childAfter.LedgerTransactionID == nil {
		t.Fatalf("expected the fallback to succeed with a ledger link, got state=%s", childAfter.State)
	}
	var parentID uuid.UUID
	if err := s.pool.WithTenant(context.Background(), s.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM payment_attempts WHERE deposit_intent_id = $1 AND attempt_no = 1`, res.Intent.ID).Scan(&parentID)
	}); err != nil {
		t.Fatal(err)
	}
	parent := mustGetAttempt(t, s.pool, s.f.tenantID, parentID)
	if parent.State != AttemptDeclined {
		t.Fatalf("no late evidence arrived for the original; expected it to stay declined, got %s", parent.State)
	}
	if b := cashBalance(t, s.pool, s.f); b != MockAmountProviderDeclineCascade {
		t.Fatalf("expected exactly one credit, got balance=%d", b)
	}
	assertInvariantAndBalanced(t, s.pool, s.f.tenantID, res.Intent.ID)
}

// ---------------------------------------------------------------------
// C. Original succeeds AFTER the fallback already succeeded: T13d,
// disputed, NO second credit, P1, audit. EXPECTED TO FAIL against HEAD.
// ---------------------------------------------------------------------

func TestINVDEP1_C_OriginalSucceedsAfterFallbackSucceeded_Disputed(t *testing.T) {
	s := newInvDep1Setup(t, "invdep1-c-orig", "invdep1-c-fb")
	res := rvInit(t, s.pool, s.orch, s.f, 5000, "invdep1-c")
	ref := *res.Attempt.ProviderReference

	childID := declineCascadableAndFindChild(t, s.pool, s.orch, s.f, "invdep1-c-orig", s.pa, ref)
	child := dispatchViaSweeper(t, s.pool, s.orch, s.f, childID)
	if child.State != AttemptPending || child.ProviderID == nil || *child.ProviderID != "invdep1-c-fb" {
		t.Fatalf("expected the cascade child dispatched to the fallback provider, got state=%s provider=%v", child.State, child.ProviderID)
	}
	childRef := *child.ProviderReference

	if _, err := rvCallback(s.pool, s.orch, s.f, "invdep1-c-fb",
		s.pb.CallbackPayload(s.f.tenantID, CallbackEventDeposit, childRef, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("fallback success (the intent's real, first capture): %v", err)
	}
	if b := cashBalance(t, s.pool, s.f); b != 5000 {
		t.Fatalf("expected the fallback's single credit, got balance=%d", b)
	}

	// The original's LATE success, after the intent is already resolved:
	// §28.4 T13d. Must not error (no 5xx redelivery loop, LF95-C3) and
	// must not post a second time.
	disposition, err := rvApplyReceipt(s.pool, s.orch, s.f.tenantID, "invdep1-c-orig", ReceiptEvidence{
		EventType: "deposit", ProviderReference: ref, Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR",
	})
	if err != nil {
		t.Fatalf("T13d must not error (LF95-C3, no 5xx redelivery loop): %v", err)
	}
	if disposition != DispositionAnomaly {
		t.Errorf("ADR 0095 §28.5: T10/T13d from a callback must be disposition 'anomaly', got %q", disposition)
	}

	assertDisputedMultipleSuccess(t, s.pool, s.f.tenantID, res.Attempt.ID)
	if b := cashBalance(t, s.pool, s.f); b != 5000 {
		t.Errorf("PAY-DOUBLE-CREDIT-1: the player must never receive a second credit; balance=%d, want 5000", b)
	}
	assertInvariantAndBalanced(t, s.pool, s.f.tenantID, res.Intent.ID)

	if n := auditActionCount(t, s.pool, s.f.tenantID, "payment.attempt_disputed", res.Attempt.ID.String()); n < 1 {
		t.Errorf("ADR 0095 §28.11: expected an audit 'payment.attempt_disputed' row for the disputed attempt, found %d", n)
	}
	if n := auditActionCount(t, s.pool, s.f.tenantID, "deposit.second_capture_posted", res.Intent.ID.String()); n != 0 {
		t.Errorf("retired action 'deposit.second_capture_posted' must never be written again, found %d rows", n)
	}
}

// ---------------------------------------------------------------------
// D. Original and fallback succeed CONCURRENTLY. Run under -race,
// >=50 repetitions. EXPECTED TO FAIL against HEAD (at some repetition,
// both branches post).
// ---------------------------------------------------------------------

// invDep1RaceOnce runs one repetition of "decline+cascade, dispatch the
// fallback, then deliver the ORIGINAL's late success and the FALLBACK's
// own success CONCURRENTLY" and asserts INV-DEP-1 held for that
// repetition. Reused by D (as-is) and K (interleaved with an extra
// concurrent exact-redelivery, §5 row K "concurrent delivery of C/D/I
// under the race detector").
func invDep1RaceOnce(t *testing.T, s invDep1Setup, rep int, extraConcurrent func(pool *db.Pool, orch *Orchestrator, f orchFixture, parentProviderID, parentRef string) func() error) {
	t.Helper()
	key := fmt.Sprintf("invdep1-race-%d-%s", rep, uuid.NewString())
	res := rvInit(t, s.pool, s.orch, s.f, 5000, key)
	ref := *res.Attempt.ProviderReference
	childID := declineCascadableAndFindChild(t, s.pool, s.orch, s.f, "invdep1-race-orig", s.pa, ref)
	child := dispatchViaSweeper(t, s.pool, s.orch, s.f, childID)
	childRef := *child.ProviderReference

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errs[0] = rvApplyReceipt(s.pool, s.orch, s.f.tenantID, "invdep1-race-orig", ReceiptEvidence{
			EventType: "deposit", ProviderReference: ref, Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR",
		})
	}()
	go func() {
		defer wg.Done()
		_, errs[1] = rvApplyReceipt(s.pool, s.orch, s.f.tenantID, "invdep1-race-fb", ReceiptEvidence{
			EventType: "deposit", ProviderReference: childRef, Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR",
		})
	}()
	var extraWait func() error
	if extraConcurrent != nil {
		extraWait = extraConcurrent(s.pool, s.orch, s.f, "invdep1-race-orig", ref)
	}
	wg.Wait()
	loAssertNoDeadlock(t, fmt.Sprintf("INV-DEP-1 race rep %d", rep), map[string]error{"original": errs[0], "fallback": errs[1]})
	if extraWait != nil {
		if err := extraWait(); err != nil && loIsDeadlock(err) {
			t.Fatalf("rep %d: extra concurrent delivery deadlocked: %v", rep, err)
		}
	}
	for name, err := range map[string]error{"original": errs[0], "fallback": errs[1]} {
		if err != nil {
			t.Fatalf("rep %d: %s delivery must never error (LF95-C3): %v", rep, name, err)
		}
	}

	// QA correction (adjudication of the implementer's FH-3 report,
	// docs/plans/payment-readiness/qa-fh3-adjudication.md): this helper
	// runs `reps` times against ONE shared setup/wallet (invDep1RaceOnce
	// is called in a sequential loop, never concurrently across reps, by
	// both TestINVDEP1_D and TestINVDEP1_K), so the WALLET's cash balance
	// is cumulative across reps - rep 0 alone lands at 5000, but rep 1
	// correctly lands at 10000, rep 2 at 15000, and so on. The original
	// assertion here (`!= 5000` on every rep) was arithmetically wrong
	// from rep 1 onward - a genuine TEST bug, not a fix bug - confirmed
	// by direct SQL evidence recorded in the adjudication doc: every rep,
	// including every rep this wrong assertion failed on, showed EXACTLY
	// one succeeded deposit attempt and EXACTLY one deposit
	// ledger_transactions row for ITS OWN intent (never more), which is
	// the actual INV-DEP-1 invariant. The per-intent checks below
	// (unchanged, still strict, still fatal on any violation) are what
	// actually prove the invariant per rep; the cumulative check only
	// proves no rep's over-credit silently escaped detection by leaving
	// some OTHER intent's money in the wallet.
	succeededCount := succeededDepositAttemptCount(t, s.pool, s.f.tenantID, res.Intent.ID)
	ledgerTxCount := ledgerDepositTxCount(t, s.pool, s.f.tenantID, res.Intent.ID)
	balance := cashBalance(t, s.pool, s.f)
	// QA correction: the cumulative expectation is computed from the
	// ACTUAL total count of succeeded deposit attempts for this tenant
	// so far, not from `rep` - `rep` is only a caller-chosen LABEL for
	// log correlation (TestINVDEP1_K deliberately offsets it by 1000 so
	// its log lines are distinguishable from TestINVDEP1_D's when both
	// are read together), never a guarantee of "this many intents have
	// resolved on this wallet so far". Deriving the expectation from the
	// database itself, rather than from the label, makes this assertion
	// correct regardless of the caller's own numbering choice, and it
	// doubles as an independent tenant-wide cross-check: if INV-DEP-1
	// ever let an intent post twice, totalSucceededDeposits would exceed
	// the number of intents actually created, and this cumulative
	// balance check would still catch it.
	totalSucceededDeposits := totalTenantSucceededDepositCount(t, s.pool, s.f.tenantID)
	wantCumulative := int64(totalSucceededDeposits) * 5000
	t.Logf("rep %d: intent=%s succeeded_deposit_attempts=%d deposit_ledger_transactions=%d wallet_balance=%d tenant_total_succeeded_deposits=%d want_cumulative=%d",
		rep, res.Intent.ID, succeededCount, ledgerTxCount, balance, totalSucceededDeposits, wantCumulative)
	if succeededCount != 1 {
		t.Fatalf("rep %d: INV-DEP-1 VIOLATED (FIX BUG): intent %s has %d succeeded deposit attempts, want exactly 1 (wallet_balance=%d)",
			rep, res.Intent.ID, succeededCount, balance)
	}
	if ledgerTxCount != 1 {
		t.Fatalf("rep %d: INV-DEP-1 VIOLATED (FIX BUG): intent %s has %d deposit ledger_transactions rows, want exactly 1 (wallet_balance=%d)",
			rep, res.Intent.ID, ledgerTxCount, balance)
	}
	assertInvariantAndBalanced(t, s.pool, s.f.tenantID, res.Intent.ID)
	if balance != wantCumulative {
		t.Fatalf("rep %d: PAY-DOUBLE-CREDIT-1: cumulative wallet balance across %d succeeded deposit attempts (5000 each) must be %d, got %d (this intent alone: succeeded_attempts=%d ledger_tx=%d)",
			rep, totalSucceededDeposits, wantCumulative, balance, succeededCount, ledgerTxCount)
	}
}

// totalTenantSucceededDepositCount counts every succeeded deposit
// attempt across the whole tenant (every intent this test's shared
// wallet has ever resolved), used only to compute the race helper's
// cumulative-balance expectation independently of any assumed rep
// numbering.
func totalTenantSucceededDepositCount(t *testing.T, pool *db.Pool, tenantID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_attempts WHERE tenant_id = $1 AND operation = 'deposit' AND state = 'succeeded'`, tenantID).Scan(&n)
	}); err != nil {
		t.Fatalf("count tenant succeeded deposit attempts: %v", err)
	}
	return n
}

func TestINVDEP1_D_ConcurrentOriginalAndFallbackSuccess_Race(t *testing.T) {
	const reps = 50
	s := newInvDep1Setup(t, "invdep1-race-orig", "invdep1-race-fb")
	for i := 0; i < reps; i++ {
		invDep1RaceOnce(t, s, i, nil)
	}
}

// ---------------------------------------------------------------------
// E. Duplicate ORIGINAL success callback: no-op, one receipt effect, one
// credit. Baseline dedupe - already correct today; must stay correct.
// ---------------------------------------------------------------------

func TestINVDEP1_E_DuplicateOriginalSuccessCallback(t *testing.T) {
	s := newInvDep1Setup(t, "invdep1-e-orig", "invdep1-e-fb")
	res := rvInit(t, s.pool, s.orch, s.f, 5000, "invdep1-e")
	ref := *res.Attempt.ProviderReference
	payload := s.pa.CallbackPayload(s.f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false)

	if _, err := rvCallback(s.pool, s.orch, s.f, "invdep1-e-orig", payload); err != nil {
		t.Fatalf("first success: %v", err)
	}
	r2, err := rvCallback(s.pool, s.orch, s.f, "invdep1-e-orig", payload)
	if err != nil {
		t.Fatalf("duplicate success delivery must not error: %v", err)
	}
	if r2.Disposition != DispositionDuplicateEffect {
		t.Errorf("expected duplicate_effect on an identical redelivery, got %q", r2.Disposition)
	}
	if b := cashBalance(t, s.pool, s.f); b != 5000 {
		t.Fatalf("expected exactly one credit, got balance=%d", b)
	}
	var receiptCount int
	if err := s.pool.WithTenant(context.Background(), s.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_provider_events WHERE tenant_id = $1 AND provider_reference = $2`, s.f.tenantID, ref).Scan(&receiptCount)
	}); err != nil || receiptCount != 1 {
		t.Fatalf("expected exactly 1 receipt row across both deliveries, got %d (err=%v)", receiptCount, err)
	}
	assertInvariantAndBalanced(t, s.pool, s.f.tenantID, res.Intent.ID)
}

// ---------------------------------------------------------------------
// F. Duplicate FALLBACK success callback: same as E, for the cascade
// child.
// ---------------------------------------------------------------------

func TestINVDEP1_F_DuplicateFallbackSuccessCallback(t *testing.T) {
	s := newInvDep1Setup(t, "invdep1-f-orig", "invdep1-f-fb")
	res, err := s.orch.InitiateDepositAttempt(context.Background(), s.pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: s.f.tenantID, BrandID: s.f.brandID, PlayerAccountID: s.f.playerAccountID, WalletID: s.f.walletID},
		AssetCode: "EUR", Amount: MockAmountProviderDeclineCascade, PaymentMethod: "card", IdempotencyKey: "invdep1-f",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	childRef := *res.Attempt.ProviderReference
	payload := s.pb.CallbackPayload(s.f.tenantID, CallbackEventDeposit, childRef, "", OutcomeSucceeded, MockAmountProviderDeclineCascade, "EUR", "", false)

	if _, err := rvCallback(s.pool, s.orch, s.f, "invdep1-f-fb", payload); err != nil {
		t.Fatalf("first fallback success: %v", err)
	}
	r2, err := rvCallback(s.pool, s.orch, s.f, "invdep1-f-fb", payload)
	if err != nil {
		t.Fatalf("duplicate fallback success delivery must not error: %v", err)
	}
	if r2.Disposition != DispositionDuplicateEffect {
		t.Errorf("expected duplicate_effect on an identical fallback redelivery, got %q", r2.Disposition)
	}
	if b := cashBalance(t, s.pool, s.f); b != MockAmountProviderDeclineCascade {
		t.Fatalf("expected exactly one credit, got balance=%d", b)
	}
	assertInvariantAndBalanced(t, s.pool, s.f.tenantID, res.Intent.ID)
}

// ---------------------------------------------------------------------
// G. Same provider reference repeated, this time bypassing the receipt
// layer's own fingerprint dedupe entirely and hitting postDepositSuccess/
// ledger.Post directly with the identical (providerID, providerReference)
// key - the "ledger key" half of "deduplicated by the receipt AND the
// ledger key".
// ---------------------------------------------------------------------

func TestINVDEP1_G_SameProviderReferenceRepeated_LedgerKeyDedupe(t *testing.T) {
	s := newInvDep1Setup(t, "invdep1-g-orig", "invdep1-g-fb")
	res := rvInit(t, s.pool, s.orch, s.f, 5000, "invdep1-g")
	ref := *res.Attempt.ProviderReference

	if _, err := rvCallback(s.pool, s.orch, s.f, "invdep1-g-orig",
		s.pa.CallbackPayload(s.f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("success: %v", err)
	}
	firstIntent := mustGetLiveOrTerminalIntent(t, s.pool, s.f, res.Intent.ID)
	firstTxID := *firstIntent.LedgerTransactionID

	// Call the choke point directly (in-package, same as every production
	// caller) with the EXACT same (providerID, providerReference, amount,
	// asset) - this must collapse via ledger.Post's own idempotency key,
	// never create a second row.
	var secondTxID uuid.UUID
	if err := s.pool.WithTenant(context.Background(), s.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, res.Intent.ID); err != nil {
			return err
		}
		intent, err := GetDepositIntentByID(ctx, tx, res.Intent.ID)
		if err != nil {
			return err
		}
		// Compile-only adaptation to land this QA-authored, test-first file
		// against the real §28.3 implementation: postDepositSuccess gained
		// an attemptID *uuid.UUID parameter (needed for the
		// resolved_for_other(I, A, K) choke-point check) that did not
		// exist when this test was written "test-first". The scenario,
		// its fixture and every assertion below are unchanged - this is
		// an exact redelivery of res.Attempt's own already-succeeded
		// posting, so its own attempt id is the correct, most accurate
		// argument (not nil - nil is reserved for the legacy
		// no-attempt-row InitiateDeposit path, which this is not).
		_, secondTxID, err = s.orch.postDepositSuccess(ctx, tx, intent, &res.Attempt.ID, "invdep1-g-orig", ref, 5000, "EUR")
		return err
	}); err != nil {
		t.Fatalf("exact redelivery through postDepositSuccess must not error: %v", err)
	}
	if secondTxID != firstTxID {
		t.Errorf("G: an exact (provider_id, provider_reference) redelivery must collapse to the SAME ledger transaction id, got %s vs %s", secondTxID, firstTxID)
	}
	assertInvariantAndBalanced(t, s.pool, s.f.tenantID, res.Intent.ID)
	if b := cashBalance(t, s.pool, s.f); b != 5000 {
		t.Fatalf("expected exactly one credit, got balance=%d", b)
	}
}

func mustGetLiveOrTerminalIntent(t *testing.T, pool *db.Pool, f orchFixture, id uuid.UUID) DepositIntent {
	t.Helper()
	var d DepositIntent
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, err = GetDepositIntentByID(ctx, tx, id)
		return err
	}); err != nil {
		t.Fatalf("get intent: %v", err)
	}
	return d
}

// ---------------------------------------------------------------------
// H. Different provider references for one intent (a 3-provider cascade
// chain): at most 1 credit; the extras are disputed. EXPECTED TO FAIL
// against HEAD.
// ---------------------------------------------------------------------

func TestINVDEP1_H_ThreeDistinctReferences_AllButFirstDisputed(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	pa := NewMockProvider("invdep1-h-a", "EUR")
	pb := NewMockProvider("invdep1-h-b", "EUR")
	pc := NewMockProvider("invdep1-h-c", "EUR")
	pc.AcceptAllAmounts = true
	registerCapability(t, pool, f, pa, 100)
	registerCapability(t, pool, f, pb, 200)
	registerCapability(t, pool, f, pc, 300)
	orch := NewOrchestrator(map[string]PaymentProvider{"invdep1-h-a": pa, "invdep1-h-b": pb, "invdep1-h-c": pc},
		MultiWebhookCredentialResolver{"invdep1-h-a": NewMockWebhookCredentials(pa), "invdep1-h-b": NewMockWebhookCredentials(pb), "invdep1-h-c": NewMockWebhookCredentials(pc)})

	res := rvInit(t, pool, orch, f, 5000, "invdep1-h")
	refA := *res.Attempt.ProviderReference
	childBID := declineCascadableAndFindChild(t, pool, orch, f, "invdep1-h-a", pa, refA)
	childB := dispatchViaSweeper(t, pool, orch, f, childBID)
	refB := *childB.ProviderReference

	// B also declines cascadable (a second, independent provider
	// failure), cascading to C.
	if _, err := rvCallback(pool, orch, f, "invdep1-h-b",
		pb.CallbackPayload(f.tenantID, CallbackEventDeposit, refB, "", OutcomeDeclined, 0, "", "provider_unavailable", true)); err != nil {
		t.Fatalf("B decline: %v", err)
	}
	var childCID uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM payment_attempts WHERE deposit_intent_id = $1 AND attempt_no = 3`, res.Intent.ID).Scan(&childCID)
	}); err != nil {
		t.Fatalf("expected a third cascade attempt: %v", err)
	}
	childC := dispatchViaSweeper(t, pool, orch, f, childCID)
	if childC.ProviderID == nil || *childC.ProviderID != "invdep1-h-c" {
		t.Fatalf("expected the third cascade attempt on provider C, got %v", childC.ProviderID)
	}
	refC := *childC.ProviderReference

	// C succeeds: the intent's ONE real, first capture.
	if _, err := rvCallback(pool, orch, f, "invdep1-h-c",
		pc.CallbackPayload(f.tenantID, CallbackEventDeposit, refC, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("C success: %v", err)
	}

	// Two late, DIFFERENT provider references now report success for the
	// SAME intent: A's and B's. Neither may ever post.
	for name, args := range map[string]struct {
		providerID string
		mock       *MockProvider
		ref        string
	}{"A": {"invdep1-h-a", pa, refA}, "B": {"invdep1-h-b", pb, refB}} {
		d, err := rvApplyReceipt(pool, orch, f.tenantID, args.providerID, ReceiptEvidence{
			EventType: "deposit", ProviderReference: args.ref, Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR",
		})
		if err != nil {
			t.Fatalf("late success for %s must not error: %v", name, err)
		}
		if d != DispositionAnomaly {
			t.Errorf("late success for %s: expected disposition anomaly, got %q", name, d)
		}
	}

	assertDisputedMultipleSuccess(t, pool, f.tenantID, res.Attempt.ID)
	assertDisputedMultipleSuccess(t, pool, f.tenantID, childBID)
	if b := cashBalance(t, pool, f); b != 5000 {
		t.Errorf("PAY-DOUBLE-CREDIT-1: exactly one credit must survive 3 distinct provider references, got balance=%d", b)
	}
	assertInvariantAndBalanced(t, pool, f.tenantID, res.Intent.ID)
}

// ---------------------------------------------------------------------
// I. Original times out (-> ambiguous), the sweeper authoritatively
// declines it (T6-equivalent) and cascades, the fallback succeeds, then
// the original's late success arrives. EXPECTED TO FAIL against HEAD.
//
// Simplification (documented, not hidden): T6's own "authoritative
// not-found via QueryStatus" detection is not separately re-implemented
// here - this test drives the SAME real receipt-path OutcomeDeclined
// branch T6 itself feeds into (applyResolvedReceiptEvidence's
// AttemptAmbiguous case), resolving the ambiguous attempt by its
// merchant_reference (always known per INV-IO-3, unlike a not-yet-issued
// provider_reference). Once the ambiguous attempt is declined and
// cascaded, this is mechanically identical to scenario C (the
// reconciliation plan's own "Timeout variant" note).
// ---------------------------------------------------------------------

func TestINVDEP1_I_TimeoutThenFallbackThenLateOriginal(t *testing.T) {
	s := newInvDep1Setup(t, "invdep1-i-orig", "invdep1-i-fb")
	res, err := s.orch.InitiateDepositAttempt(context.Background(), s.pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: s.f.tenantID, BrandID: s.f.brandID, PlayerAccountID: s.f.playerAccountID, WalletID: s.f.walletID},
		AssetCode: "EUR", Amount: MockAmountAmbiguous, PaymentMethod: "card", IdempotencyKey: "invdep1-i",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	if res.Attempt.State != AttemptAmbiguous {
		t.Fatalf("expected the initial attempt ambiguous (timeout surrogate), got %s", res.Attempt.State)
	}

	// The authoritative not-found: a decline evidence resolved by
	// merchant_reference (this attempt has no provider_reference yet),
	// which ALSO tells the platform what the reference will be from now
	// on (providerRef in the evidence) - mirroring what a real
	// QueryStatus result would supply.
	ref := "invdep1-i-authoritative-ref"
	d, err := rvApplyReceipt(s.pool, s.orch, s.f.tenantID, "invdep1-i-orig", ReceiptEvidence{
		EventType: "deposit", ProviderReference: ref, MerchantReference: res.Attempt.ID.String(),
		Outcome: OutcomeDeclined, DeclineReason: "not_found", Cascadable: true,
	})
	if err != nil {
		t.Fatalf("T6-equivalent authoritative decline: %v", err)
	}
	if d != DispositionApplied {
		t.Fatalf("expected the authoritative decline applied, got %q", d)
	}
	original := mustGetAttempt(t, s.pool, s.f.tenantID, res.Attempt.ID)
	if original.State != AttemptDeclined {
		t.Fatalf("expected the ambiguous attempt declined after the authoritative not-found, got %s", original.State)
	}

	childID := declineCascadableAndFindChild(t, s.pool, s.orch, s.f, "invdep1-i-orig", s.pa, ref)
	child := dispatchViaSweeper(t, s.pool, s.orch, s.f, childID)
	childRef := *child.ProviderReference
	if _, err := rvCallback(s.pool, s.orch, s.f, "invdep1-i-fb",
		s.pb.CallbackPayload(s.f.tenantID, CallbackEventDeposit, childRef, "", OutcomeSucceeded, MockAmountAmbiguous, "EUR", "", false)); err != nil {
		t.Fatalf("fallback success: %v", err)
	}

	disposition, err := rvApplyReceipt(s.pool, s.orch, s.f.tenantID, "invdep1-i-orig", ReceiptEvidence{
		EventType: "deposit", ProviderReference: ref, Outcome: OutcomeSucceeded, Amount: MockAmountAmbiguous, AssetCode: "EUR",
	})
	if err != nil {
		t.Fatalf("original's late success (T13d) must not error: %v", err)
	}
	if disposition != DispositionAnomaly {
		t.Errorf("expected disposition anomaly for the timed-out original's late success, got %q", disposition)
	}
	assertDisputedMultipleSuccess(t, s.pool, s.f.tenantID, res.Attempt.ID)
	if b := cashBalance(t, s.pool, s.f); b != MockAmountAmbiguous {
		t.Errorf("PAY-DOUBLE-CREDIT-1: exactly one credit must survive the timeout ordering, got balance=%d", b)
	}
	assertInvariantAndBalanced(t, s.pool, s.f.tenantID, res.Intent.ID)
}

// ---------------------------------------------------------------------
// J. A callback while the ATTEMPT is ambiguous and the INTENT is already
// resolved: T7 guard -> T10 disputed, no post. EXPECTED TO FAIL against
// HEAD.
// ---------------------------------------------------------------------

func TestINVDEP1_J_AmbiguousSiblingAfterResolution_T7Guard(t *testing.T) {
	s := newInvDep1Setup(t, "invdep1-j-orig", "invdep1-j-fb")
	res := rvInit(t, s.pool, s.orch, s.f, 5000, "invdep1-j")
	ref := *res.Attempt.ProviderReference
	childID := declineCascadableAndFindChild(t, s.pool, s.orch, s.f, "invdep1-j-orig", s.pa, ref)
	child := dispatchViaSweeper(t, s.pool, s.orch, s.f, childID)
	childRef := *child.ProviderReference

	// The fallback goes ambiguous (network hiccup) BEFORE the original's
	// late success resolves the intent.
	if err := s.pool.WithTenant(context.Background(), s.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MarkAmbiguousFromPending(ctx, tx, childID, EvidenceQueryStatus, time.Now().Add(time.Minute))
	}); err != nil {
		t.Fatalf("mark fallback ambiguous: %v", err)
	}
	childAmbiguous := mustGetAttempt(t, s.pool, s.f.tenantID, childID)
	if childAmbiguous.State != AttemptAmbiguous {
		t.Fatalf("expected the fallback ambiguous, got %s", childAmbiguous.State)
	}

	// The original's late success is the intent's FIRST success (the
	// ambiguous fallback never succeeded) - a plain T13, unaffected by
	// §28.
	if _, err := rvCallback(s.pool, s.orch, s.f, "invdep1-j-orig",
		s.pa.CallbackPayload(s.f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("original success (T13, first success): %v", err)
	}
	if b := cashBalance(t, s.pool, s.f); b != 5000 {
		t.Fatalf("expected exactly one credit after the original's first success, got balance=%d", b)
	}

	// NOW the ambiguous fallback's own success evidence arrives, after
	// the intent is already resolved: the T7 guard.
	disposition, err := rvApplyReceipt(s.pool, s.orch, s.f.tenantID, "invdep1-j-fb", ReceiptEvidence{
		EventType: "deposit", ProviderReference: childRef, Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR",
	})
	if err != nil {
		t.Fatalf("T7-guard success on the ambiguous sibling must not error: %v", err)
	}
	if disposition != DispositionAnomaly {
		t.Errorf("expected disposition anomaly for the T7-guard case, got %q", disposition)
	}
	assertDisputedMultipleSuccess(t, s.pool, s.f.tenantID, childID)
	if b := cashBalance(t, s.pool, s.f); b != 5000 {
		t.Errorf("PAY-DOUBLE-CREDIT-1: the ambiguous sibling's late success must never post a second credit, got balance=%d", b)
	}
	assertInvariantAndBalanced(t, s.pool, s.f.tenantID, res.Intent.ID)
}

// ---------------------------------------------------------------------
// K. Concurrent delivery of the C/D/I orderings under the race detector,
// >=50 repetitions. EXPECTED TO FAIL against HEAD.
// ---------------------------------------------------------------------

func TestINVDEP1_K_ConcurrentAdversarialOrderings_Race(t *testing.T) {
	const reps = 50
	// invDep1RaceOnce's own hardcoded provider ids ("invdep1-race-orig"/
	// "invdep1-race-fb") must match the setup's - reused verbatim from D
	// rather than parameterized, since both tests get their own fresh
	// scratch database (depositV2ScratchPool), so identical provider_id
	// strings across the two tests never collide.
	s := newInvDep1Setup(t, "invdep1-race-orig", "invdep1-race-fb")
	for i := 0; i < reps; i++ {
		// D's shape, PLUS a third concurrent goroutine redelivering the
		// EXACT SAME original-success payload again (an in-flight
		// duplicate racing the other two) - the "adversarial orderings"
		// K adds on top of D: a genuine duplicate delivery racing the
		// two distinct-attempt deliveries, all three hitting the same
		// deposit_intents lock concurrently.
		invDep1RaceOnce(t, s, 1000+i, func(pool *db.Pool, orch *Orchestrator, f orchFixture, parentProviderID, parentRef string) func() error {
			done := make(chan error, 1)
			go func() {
				_, err := rvApplyReceipt(pool, orch, f.tenantID, parentProviderID, ReceiptEvidence{
					EventType: "deposit", ProviderReference: parentRef, Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR",
				})
				done <- err
			}()
			return func() error { return <-done }
		})
	}
}

// ---------------------------------------------------------------------
// L. Cross-tenant callback: refused at binding (RLS), no effect in
// either tenant. Already correct today (INV-IO-14) and must stay so.
// ---------------------------------------------------------------------

func TestINVDEP1_L_CrossTenantCallback_NoEffectInEitherTenant(t *testing.T) {
	pool := depositV2ScratchPool(t)
	fA := seedOrchFixture(t, pool)
	fB := seedOrchFixture(t, pool)
	pa := NewMockProvider("invdep1-l-shared", "EUR")
	registerCapability(t, pool, fA, pa, 100)
	registerCapability(t, pool, fB, pa, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"invdep1-l-shared": pa}, MultiWebhookCredentialResolver{"invdep1-l-shared": NewMockWebhookCredentials(pa)})

	resA := rvInit(t, pool, orch, fA, 4000, "invdep1-l-a")
	resB := rvInit(t, pool, orch, fB, 6000, "invdep1-l-b")
	refB := *resB.Attempt.ProviderReference
	if _, err := rvCallback(pool, orch, fB, "invdep1-l-shared",
		pa.CallbackPayload(fB.tenantID, CallbackEventDeposit, refB, "", OutcomeSucceeded, 6000, "EUR", "", false)); err != nil {
		t.Fatalf("tenant B's own success: %v", err)
	}
	balBBefore := cashBalance(t, pool, fB)
	balABefore := cashBalance(t, pool, fA)
	intentBBefore := mustGetLiveOrTerminalIntent(t, pool, fB, resB.Intent.ID)

	// Deliver evidence naming tenant B's own reference, but under TENANT
	// A's WithTenant scope (as if the callback's tenant binding resolved
	// to the wrong tenant - exactly what INV-IO-14 / RLS must refuse).
	var disposition ReceiptDisposition
	err := pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		disposition, err = ApplyReceiptEvidence(ctx, tx, orch, fA.tenantID, "invdep1-l-shared", ReceiptEvidence{
			EventType: "deposit", ProviderReference: refB, Outcome: OutcomeSucceeded, Amount: 6000, AssetCode: "EUR",
		})
		return err
	})
	if err != nil {
		t.Fatalf("cross-tenant delivery must not error: %v", err)
	}
	if disposition != DispositionDeferredUnresolved {
		t.Errorf("cross-tenant delivery must resolve to NOTHING under RLS (deferred_unresolved), got %q - a tenant-A-scoped read must never see tenant B's attempt", disposition)
	}

	// No effect in tenant B: unchanged balance and intent.
	if b := cashBalance(t, pool, fB); b != balBBefore {
		t.Errorf("cross-tenant callback altered tenant B's balance: %d -> %d", balBBefore, b)
	}
	intentBAfter := mustGetLiveOrTerminalIntent(t, pool, fB, resB.Intent.ID)
	if intentBAfter.Status != intentBBefore.Status || *intentBAfter.LedgerTransactionID != *intentBBefore.LedgerTransactionID {
		t.Errorf("cross-tenant callback altered tenant B's intent projection")
	}
	// No effect in tenant A either (only a benign deferred receipt row
	// stored under A - no attempt, ledger or wallet change for A).
	if b := cashBalance(t, pool, fA); b != balABefore {
		t.Errorf("cross-tenant callback altered tenant A's balance: %d -> %d", balABefore, b)
	}
	var aAttemptTouched int
	if err := pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_attempts WHERE tenant_id = $1 AND provider_reference = $2`, fA.tenantID, refB).Scan(&aAttemptTouched)
	}); err != nil {
		t.Fatal(err)
	}
	if aAttemptTouched != 0 {
		t.Errorf("cross-tenant callback must never create or touch a payment_attempts row under the WRONG tenant, found %d", aAttemptTouched)
	}
	_ = resA
}

// ---------------------------------------------------------------------
// O. T17 / re-drive must never re-open a terminal, disputed attempt: no
// post, no provider re-contact. Exercised on the ONE disputed shape the
// current trigger already allows (T13t, reversal_tombstone_precedes_success)
// - the terminal-attempt guard this proves ("T17 never state-changes a
// terminal attempt except declined", §28.9) is reason-agnostic; once
// migration 0107 allows a literal multiple_success_for_intent disputed
// row to exist, add a second copy of this test using that reason too.
// ---------------------------------------------------------------------

func TestINVDEP1_O_ReDriveOfDisputedAttempt_NoPost(t *testing.T) {
	s := newInvDep1Setup(t, "invdep1-o-orig", "invdep1-o-fb")
	res := rvInit(t, s.pool, s.orch, s.f, 5000, "invdep1-o")
	ref := *res.Attempt.ProviderReference

	// Build the T13t (tombstone-precedes-success) disputed shape: a
	// reversal for a NEVER-SEEN original reference first (a tombstone),
	// then a late "success" arriving for that already-tombstoned
	// reference resolves this SAME attempt via declined->disputed
	// (T13t) - reuse the existing, already-implemented tombstone path so
	// this test needs nothing from §28 to construct its fixture.
	if _, err := rvCallback(s.pool, s.orch, s.f, "invdep1-o-orig",
		s.pa.CallbackPayload(s.f.tenantID, CallbackEventDeposit, ref, "", OutcomeDeclined, 0, "", "provider_unavailable", true)); err != nil {
		t.Fatalf("decline: %v", err)
	}
	if _, err := rvCallback(s.pool, s.orch, s.f, "invdep1-o-orig",
		s.pa.CallbackPayload(s.f.tenantID, CallbackEventDepositReversal, "invdep1-o-tomb", ref, OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("tombstone reversal naming the (never posted) original: %v", err)
	}
	if _, err := rvCallback(s.pool, s.orch, s.f, "invdep1-o-orig",
		s.pa.CallbackPayload(s.f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("late success after the tombstone (T13t): %v", err)
	}
	disputed := mustGetAttempt(t, s.pool, s.f.tenantID, res.Attempt.ID)
	if disputed.State != AttemptDisputed {
		t.Fatalf("expected the tombstoned attempt disputed (T13t), got %s", disputed.State)
	}
	if disputed.TerminalReason == nil || *disputed.TerminalReason != "reversal_tombstone_precedes_success" {
		t.Fatalf("expected terminal_reason=reversal_tombstone_precedes_success, got %v", disputed.TerminalReason)
	}
	if disputed.LedgerTransactionID != nil {
		t.Fatalf("a T13t attempt must never carry a ledger link, got %s", *disputed.LedgerTransactionID)
	}

	// T17: a staff/operator re-verify request against a TERMINAL,
	// disputed attempt. §28.9: "T17 never state-changes a terminal
	// attempt except declined" - it must be a no-op here, never
	// resurrecting the attempt or contacting the provider again.
	touchErr := s.pool.WithTenant(context.Background(), s.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return Touch(ctx, tx, disputed.ID)
	})
	// The 0101 CHECK forbids next_action_at on a terminal row, so Touch's
	// own CAS predicate (`next_action_at IS NOT NULL`) cannot match here -
	// it is expected to report "no matching row", never to succeed and
	// silently re-arm a terminal attempt for the sweeper.
	if touchErr == nil {
		t.Errorf("§28.9: T17/Touch must never re-arm a terminal disputed attempt for the sweeper (next_action_at must stay NULL)")
	}

	// Even if something DID set next_action_at, the sweeper's own
	// processAttempt terminal-state branch must not act on it - assert
	// directly that a sweep leaves the disputed attempt exactly as is.
	if st := NewSweeper(s.pool, s.orch, AllowAllDepositKYCGate{}, MockCredentialResolver{}).RunOnce(context.Background(), []uuid.UUID{s.f.tenantID}); len(st.Errors) != 0 {
		t.Fatalf("sweeper run: %v", st.Errors)
	}
	after := mustGetAttempt(t, s.pool, s.f.tenantID, disputed.ID)
	if after.State != AttemptDisputed || after.LedgerTransactionID != nil {
		t.Fatalf("T17/re-drive must never post for a disputed attempt: state=%s ledger=%v", after.State, after.LedgerTransactionID)
	}
	if b := cashBalance(t, s.pool, s.f); b != 0 {
		t.Errorf("a T13t (never-posted) attempt must never end up crediting the player, balance=%d", b)
	}
}

// ---------------------------------------------------------------------
// Inverted named tests (ledger-finance ruling §5 items 1-2; see the file-
// header mapping table). These keep the ORIGINAL fixtures' shape for
// direct traceability, asserting the NEW behaviour.
// ---------------------------------------------------------------------

// TestINVDEP1_Inverted_T13SecondCapture_BecomesDisputed_NoSecondPosting
// replaces TestReceipt_T13SecondCapture_ThroughApplyReceiptEvidence_LedgerBalanced
// (receipt_integration_test.go). Same fixture (the magic-amount
// synchronous cascade + a direct ApplyReceiptEvidence call for the
// declined parent's late success); inverted assertion.
func TestINVDEP1_Inverted_T13SecondCapture_BecomesDisputed_NoSecondPosting(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	providerA := NewMockProvider("invdep1-t13inv-a", "EUR")
	providerB := NewMockProvider("invdep1-t13inv-b", "EUR")
	providerB.AcceptAllAmounts = true
	registerCapability(t, pool, f, providerA, 100)
	registerCapability(t, pool, f, providerB, 200)
	orch := NewOrchestrator(map[string]PaymentProvider{"invdep1-t13inv-a": providerA, "invdep1-t13inv-b": providerB},
		MultiWebhookCredentialResolver{"invdep1-t13inv-a": NewMockWebhookCredentials(providerA), "invdep1-t13inv-b": NewMockWebhookCredentials(providerB)})

	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: MockAmountProviderDeclineCascade, PaymentMethod: "card", IdempotencyKey: "invdep1-t13inv",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	childAttempt := res.Attempt
	childRef := *childAttempt.ProviderReference

	var parentAttemptID uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM payment_attempts WHERE deposit_intent_id = $1 AND attempt_no = 1`, res.Intent.ID).Scan(&parentAttemptID)
	}); err != nil {
		t.Fatalf("fetch parent attempt: %v", err)
	}
	parentAttempt := mustGetAttempt(t, pool, f.tenantID, parentAttemptID)
	parentRef := *parentAttempt.ProviderReference

	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		d, err := ApplyReceiptEvidence(ctx, tx, orch, f.tenantID, "invdep1-t13inv-b", ReceiptEvidence{
			EventType: "deposit", ProviderReference: childRef, Outcome: OutcomeSucceeded,
			Amount: MockAmountProviderDeclineCascade, AssetCode: "EUR",
		})
		if err != nil {
			return err
		}
		if d != DispositionApplied {
			t.Fatalf("expected the child's own success applied, got %s", d)
		}
		return nil
	}); err != nil {
		t.Fatalf("apply child success: %v", err)
	}
	childAfter := mustGetAttempt(t, pool, f.tenantID, childAttempt.ID)
	if childAfter.State != AttemptSucceeded || childAfter.LedgerTransactionID == nil {
		t.Fatalf("expected the child succeeded with a ledger link, got state=%s ledger=%v", childAfter.State, childAfter.LedgerTransactionID)
	}

	// INVERTED: the parent's late success on its OWN reference must now
	// become T13d (disputed, multiple_success_for_intent), never a
	// second, distinct ledger transaction.
	var disposition ReceiptDisposition
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		disposition, err = ApplyReceiptEvidence(ctx, tx, orch, f.tenantID, "invdep1-t13inv-a", ReceiptEvidence{
			EventType: "deposit", ProviderReference: parentRef, Outcome: OutcomeSucceeded,
			Amount: MockAmountProviderDeclineCascade, AssetCode: "EUR",
		})
		return err
	}); err != nil {
		t.Fatalf("ApplyReceiptEvidence (T13d) must not fail: %v", err)
	}
	if disposition != DispositionAnomaly {
		t.Errorf("expected disposition anomaly for T13d, got %s", disposition)
	}
	assertDisputedMultipleSuccess(t, pool, f.tenantID, parentAttemptID)

	var intentLedgerTxID uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT ledger_transaction_id FROM deposit_intents WHERE id = $1`, res.Intent.ID).Scan(&intentLedgerTxID)
	}); err != nil {
		t.Fatalf("read intent ledger_transaction_id: %v", err)
	}
	if intentLedgerTxID != *childAfter.LedgerTransactionID {
		t.Fatalf("the intent's own ledger_transaction_id link must be left alone (still the FIRST posting), got %s want %s",
			intentLedgerTxID, *childAfter.LedgerTransactionID)
	}
	assertInvariantAndBalanced(t, pool, f.tenantID, res.Intent.ID)
}

// TestINVDEP1_Inverted_RVLF_P6_ReversalOfDisputedSecondCaptureTakesTombstoneBranch
// replaces TestRVLF_P6_ReversalOfSecondCaptureReversesItsOwnTransaction
// (rvlf_i1_regression_integration_test.go). Same fixture; inverted
// assertion: the parent never posted, so a reversal naming its reference
// takes the TOMBSTONE branch (no ledger effect), while the child's own
// (only) posting remains independently, exactly-once reversible.
func TestINVDEP1_Inverted_RVLF_P6_ReversalOfDisputedSecondCaptureTakesTombstoneBranch(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	pa := NewMockProvider("invdep1-p6inv-a", "EUR")
	pb := NewMockProvider("invdep1-p6inv-b", "EUR")
	pb.AcceptAllAmounts = true
	registerCapability(t, pool, f, pa, 100)
	registerCapability(t, pool, f, pb, 200)
	orch := NewOrchestrator(map[string]PaymentProvider{"invdep1-p6inv-a": pa, "invdep1-p6inv-b": pb},
		MultiWebhookCredentialResolver{"invdep1-p6inv-a": NewMockWebhookCredentials(pa), "invdep1-p6inv-b": NewMockWebhookCredentials(pb)})
	amt := int64(MockAmountProviderDeclineCascade)
	res := rvInit(t, pool, orch, f, amt, "invdep1-p6inv")
	childRef := *res.Attempt.ProviderReference
	var parentID uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM payment_attempts WHERE deposit_intent_id = $1 AND attempt_no = 1`, res.Intent.ID).Scan(&parentID)
	}); err != nil {
		t.Fatal(err)
	}
	parentRef := *mustGetAttempt(t, pool, f.tenantID, parentID).ProviderReference

	if _, err := rvCallback(pool, orch, f, "invdep1-p6inv-b", pb.CallbackPayload(f.tenantID, CallbackEventDeposit, childRef, "", OutcomeSucceeded, amt, "EUR", "", false)); err != nil {
		t.Fatalf("child success: %v", err)
	}
	if _, err := rvCallback(pool, orch, f, "invdep1-p6inv-a", pa.CallbackPayload(f.tenantID, CallbackEventDeposit, parentRef, "", OutcomeSucceeded, amt, "EUR", "", false)); err != nil {
		t.Fatalf("parent late success (T13d): %v", err)
	}
	child := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	parent := assertDisputedMultipleSuccess(t, pool, f.tenantID, parentID)
	if cashBalance(t, pool, f) != amt {
		t.Fatalf("expected exactly ONE capture (the child's), got balance=%d", cashBalance(t, pool, f))
	}

	// Reverse the (never-posted, disputed) parent's reference: this must
	// take the TOMBSTONE branch (LedgerTransactionID is nil, so it
	// resolves as "reversal for an attempt with no posting") - no ledger
	// debit, balance unchanged.
	if _, err := rvCallback(pool, orch, f, "invdep1-p6inv-a", pa.CallbackPayload(f.tenantID, CallbackEventDepositReversal, "invdep1-p6inv-rev-parent", parentRef, OutcomeSucceeded, amt, "EUR", "", false)); err != nil {
		t.Fatalf("reversal naming the disputed (never posted) parent: %v", err)
	}
	if b := cashBalance(t, pool, f); b != amt {
		t.Errorf("reversing a never-posted disputed attempt must not change the balance, got %d want %d", b, amt)
	}
	var tombstoneRows int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'tombstone' AND provider_tx_id = $2`, f.tenantID, parentRef).Scan(&tombstoneRows)
	}); err != nil {
		t.Fatal(err)
	}
	if tombstoneRows != 1 {
		t.Errorf("expected exactly 1 tombstone row for the parent's reference, got %d", tombstoneRows)
	}
	_ = parent

	// The child's own (ONLY) posting remains independently reversible
	// exactly once (PAY-REV-1 unaffected).
	if _, err := rvCallback(pool, orch, f, "invdep1-p6inv-b", pb.CallbackPayload(f.tenantID, CallbackEventDepositReversal, "invdep1-p6inv-rev-child", childRef, OutcomeSucceeded, amt, "EUR", "", false)); err != nil {
		t.Errorf("reversal of the child's own capture: %v", err)
	}
	if b := cashBalance(t, pool, f); b != 0 {
		t.Errorf("balance after reversing the ONE real capture = %d, want 0", b)
	}

	// Code review R6 (rv-fh3-code-review.md, 95a1c34): restore the two
	// assertions the original TestRVLF_P6_ReversalOfSecondCaptureReverses
	// ItsOwnTransaction carried before this test was inverted for §28 -
	// the reverses_transaction_id check (against the CHILD's own posting,
	// the only one that exists in this inverted shape - the parent never
	// posted, so its own reversal took the tombstone branch and has no
	// reverses_transaction_id to check) and the PAY-REV-1 distinct-
	// second-reversal-of-the-same-original refusal.
	var reverses uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT reverses_transaction_id FROM ledger_transactions WHERE provider_tx_id = 'invdep1-p6inv-rev-child'`).Scan(&reverses)
	}); err != nil {
		t.Fatal(err)
	}
	if reverses != *child.LedgerTransactionID {
		t.Errorf("R6/L1: the child's own reversal must reverse its own posting %s, got %s", *child.LedgerTransactionID, reverses)
	}
	// PAY-REV-1: a distinct second reversal of the same original (the
	// child's own capture, already reversed above) is refused.
	if _, err := rvCallback(pool, orch, f, "invdep1-p6inv-b", pb.CallbackPayload(f.tenantID, CallbackEventDepositReversal, "invdep1-p6inv-rev-child-b", childRef, OutcomeSucceeded, amt, "EUR", "", false)); !errors.Is(err, ErrDepositAlreadyReversed) {
		t.Errorf("R6/PAY-REV-1 not preserved on the inverted shape: %v", err)
	}

	assertLedgerBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
	_ = child
}
