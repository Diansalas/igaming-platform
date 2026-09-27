//go:build integration

// PRH-I5 (ADR 0095 §12, §16.3): the payment_statement reconciliation
// stream.
//
// Two kinds of source are exercised:
//
//   - payments.MockStatementSource, the only wired source (MOCK): it renders
//     the MockProvider's OWN per-tenant records, not the platform DB, so
//     several divergences here are genuine MOCK-native ones (a dropped
//     success callback, a provider-confirmed amount that differs, a restart
//     that starts a new coverage window);
//   - payDivergentSource / payFixedSource, TEST-ONLY sources that inject
//     exactly one divergence per mismatch kind, or state a fixed statement
//     (payouts, reversals, cross-provider lines, idempotent re-fetch).
//
// Every test runs against TEST_DATABASE_URL (migrated through 0102); the
// migration round trip runs on scratch databases.
package reconciliation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/providerkind"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
	"github.com/Diansalas/igaming-platform/internal/txscope"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

const (
	payProvA = "mock-psp-recon-a"
	payProvB = "mock-psp-recon-b"
)

// allowAllKYCGate is this test file's own explicit, clearly-named
// always-allow payments.DepositKYCGate stand-in. PRH-I1's deposit cutover
// moved payments.AllowAllDepositKYCGate into a payments-package-internal
// _test.go file (it must never be reachable from non-test/production
// code), so a cross-package test that needs the same "KYC deposit
// enforcement is NOT ACTIVE" stand-in defines its own local one instead -
// the interface (payments.DepositKYCGate) is the only thing that needs to
// match.
type allowAllKYCGate struct{}

func (allowAllKYCGate) EvaluateDeposit(context.Context, pgx.Tx, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, int64, string) (bool, string, error) {
	return true, "", nil
}

// ---------------------------------------------------------------------
// World.

type payWorld struct {
	pool  *db.Pool
	f     fixture
	mockA *payments.MockProvider
	mockB *payments.MockProvider
	orch  *payments.Orchestrator
	srcA  *payments.MockStatementSource
	srcB  *payments.MockStatementSource
}

func newPayWorld(t *testing.T) *payWorld {
	t.Helper()
	pool := testPool(t)
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
	// Lower Priority wins the tie-break: A is primary, B the cascade target.
	w.registerCapability(t, w.mockA, 10)
	w.registerCapability(t, w.mockB, 50)
	w.srcA = payments.NewMockStatementSource(w.mockA, payments.MockCredentialResolver{})
	w.srcB = payments.NewMockStatementSource(w.mockB, payments.MockCredentialResolver{})
	return w
}

func (w *payWorld) registerCapability(t *testing.T, p payments.PaymentProvider, priority int) {
	t.Helper()
	declared := p.Capabilities()
	err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := payments.WriteCapability(ctx, tx, p, w.f.tenantID, nil, payments.CapabilityConfig{
			SupportedFiatCurrencies: declared.SupportedFiatCurrencies,
			SupportedPaymentMethods: declared.SupportedPaymentMethods,
			SupportsDeposit:         declared.SupportsDeposit,
			SupportsWithdrawal:      declared.SupportsWithdrawal,
			SupportsRefundReversal:  declared.SupportsRefundReversal,
			AmountLimits:            declared.AmountLimits,
			Priority:                priority,
			Status:                  payments.CapabilityActive,
		})
		return err
	})
	if err != nil {
		t.Fatalf("register capability %s: %v", declared.ProviderID, err)
	}
}

// deposit runs the real ADR 0095 deposit path (phase A / gate / phase C).
func (w *payWorld) deposit(t *testing.T, amount int64) payments.PaymentAttempt {
	t.Helper()
	res, err := w.orch.InitiateDepositAttempt(context.Background(), w.pool, allowAllKYCGate{}, payments.MockCredentialResolver{}, payments.InitiateDepositParams{
		Scope:     payments.DepositScope{TenantID: w.f.tenantID, BrandID: w.f.brandID, PlayerAccountID: w.f.playerAccountID, WalletID: w.f.walletID},
		AssetCode: "EUR", Amount: amount, PaymentMethod: "card", IdempotencyKey: "pay-recon-" + uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt(%d): %v", amount, err)
	}
	if !res.AttemptCreated {
		t.Fatalf("InitiateDepositAttempt(%d): no attempt created", amount)
	}
	return res.Attempt
}

// succeed makes the provider (mock) and the platform (a verified callback
// receipt applied through the payments state machine) agree on success.
func (w *payWorld) succeed(t *testing.T, mock *payments.MockProvider, providerID string, a payments.PaymentAttempt) {
	t.Helper()
	mock.Resolve(*a.ProviderReference, payments.OutcomeSucceeded, "", false)
	w.applyReceipt(t, providerID, payments.ReceiptEvidence{
		EventType: "deposit", ProviderReference: *a.ProviderReference, Outcome: payments.OutcomeSucceeded,
		Amount: a.Amount, AssetCode: a.AssetCode,
	})
}

func (w *payWorld) applyReceipt(t *testing.T, providerID string, ev payments.ReceiptEvidence) payments.ReceiptDisposition {
	t.Helper()
	var d payments.ReceiptDisposition
	err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, err = payments.ApplyReceiptEvidence(ctx, tx, w.orch, w.f.tenantID, providerID, ev)
		return err
	})
	if err != nil {
		t.Fatalf("ApplyReceiptEvidence: %v", err)
	}
	return d
}

func (w *payWorld) attempt(t *testing.T, id uuid.UUID) payments.PaymentAttempt {
	t.Helper()
	var a payments.PaymentAttempt
	err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		a, err = payments.GetAttemptByID(ctx, tx, id)
		return err
	})
	if err != nil {
		t.Fatalf("GetAttemptByID: %v", err)
	}
	return a
}

// fetchIngest runs phases 1 and 2 and returns the import id.
func (w *payWorld) fetchIngest(t *testing.T, src statement.PaymentStatementSource, opts PaymentStatementOptions) (uuid.UUID, bool) {
	t.Helper()
	stmt, err := FetchPaymentStatement(context.Background(), src, w.f.tenantID, time.Now().Add(-time.Hour), time.Now(), opts)
	if err != nil {
		t.Fatalf("FetchPaymentStatement: %v", err)
	}
	var id uuid.UUID
	var reused bool
	err = w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, reused, err = IngestPaymentStatement(ctx, tx, w.f.tenantID, src, stmt, time.Now())
		return err
	})
	if err != nil {
		t.Fatalf("IngestPaymentStatement: %v", err)
	}
	return id, reused
}

func (w *payWorld) matchErr(t *testing.T, importID uuid.UUID, opts PaymentStatementOptions) (Run, []Mismatch, PaymentStatementInfo, error) {
	t.Helper()
	var run Run
	var ms []Mismatch
	var info PaymentStatementInfo
	err := w.pool.WithTenantSnapshot(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		run, ms, info, err = RunPaymentStatement(ctx, tx, w.f.tenantID, importID, opts)
		return err
	})
	return run, ms, info, err
}

func (w *payWorld) run(t *testing.T, src statement.PaymentStatementSource, opts PaymentStatementOptions) (Run, []Mismatch) {
	t.Helper()
	id, _ := w.fetchIngest(t, src, opts)
	run, ms, _, err := w.matchErr(t, id, opts)
	if err != nil {
		t.Fatalf("RunPaymentStatement: %v", err)
	}
	return run, ms
}

func (w *payWorld) mustClean(t *testing.T, src statement.PaymentStatementSource, opts PaymentStatementOptions) {
	t.Helper()
	run, ms := w.run(t, src, opts)
	if run.Status != StatusClean || len(ms) != 0 {
		t.Fatalf("expected a clean payment_statement run for %s, got %s with %d mismatches:\n%s", src.ProviderID(), run.Status, len(ms), renderMismatches(ms))
	}
}

func renderMismatches(ms []Mismatch) string {
	var b strings.Builder
	for _, m := range ms {
		fmt.Fprintf(&b, "  %s | %s | expected=%s | actual=%s\n", m.MismatchKind, m.ReconciliationKey, m.ExpectedValue, m.ActualValue)
	}
	return b.String()
}

// mustOnePay asserts exactly one mismatch, of kind, whose key contains
// every fragment.
func mustOnePay(t *testing.T, ms []Mismatch, kind MismatchKind, keyContains ...string) Mismatch {
	t.Helper()
	if len(ms) != 1 {
		t.Fatalf("expected exactly one %s mismatch, got %d:\n%s", kind, len(ms), renderMismatches(ms))
	}
	m := ms[0]
	if m.MismatchKind != kind {
		t.Fatalf("expected kind %s, got %s:\n%s", kind, m.MismatchKind, renderMismatches(ms))
	}
	for _, k := range keyContains {
		if !strings.Contains(m.ReconciliationKey, k) {
			t.Fatalf("mismatch key %q does not contain %q", m.ReconciliationKey, k)
		}
	}
	return m
}

// ---------------------------------------------------------------------
// Test-only sources.

const payDivergentLabel = "MOCK divergent test payment statement (test-only; injected divergence over the MOCK payment statement)"

// payDivergentSource renders the MOCK statement and then applies mutate.
type payDivergentSource struct {
	inner  *payments.MockStatementSource
	mutate func(s statement.PaymentStatement) statement.PaymentStatement
}

func (payDivergentSource) SyntheticComponent()  {}
func (payDivergentSource) Label() string        { return payDivergentLabel }
func (d payDivergentSource) ProviderID() string { return d.inner.ProviderID() }
func (d payDivergentSource) Fetch(ctx context.Context, req statement.PaymentFetchRequest) (statement.PaymentStatement, error) {
	s, err := d.inner.Fetch(ctx, req)
	if err != nil {
		return s, err
	}
	if d.mutate != nil {
		s = d.mutate(s)
	}
	return s, nil
}

func mutateLine(ref string, f func(l *statement.PaymentStatementLine)) func(statement.PaymentStatement) statement.PaymentStatement {
	return func(s statement.PaymentStatement) statement.PaymentStatement {
		for i := range s.Lines {
			if s.Lines[i].ProviderReference == ref {
				f(&s.Lines[i])
			}
		}
		return s
	}
}

const payFixedLabel = "MOCK fixed test payment statement (test-only)"

// payFixedSource states a fixed statement. heldSeen records whether Fetch
// ran with a transaction held.
type payFixedSource struct {
	provider string
	stmt     statement.PaymentStatement
	err      error
	heldSeen *[]bool
}

func (payFixedSource) SyntheticComponent()  {}
func (payFixedSource) Label() string        { return payFixedLabel }
func (s payFixedSource) ProviderID() string { return s.provider }
func (s payFixedSource) Fetch(ctx context.Context, _ statement.PaymentFetchRequest) (statement.PaymentStatement, error) {
	if s.heldSeen != nil {
		*s.heldSeen = append(*s.heldSeen, txscope.Held(ctx))
	}
	out := s.stmt
	out.Lines = append([]statement.PaymentStatementLine(nil), s.stmt.Lines...)
	return out, s.err
}

func payLineFor(provider, ref, merchant, kind, status string, amount int64) statement.PaymentStatementLine {
	return statement.PaymentStatementLine{
		ProviderID: provider, ProviderReference: ref, MerchantReference: merchant, Kind: kind, Status: status,
		Amount: amount, AssetCode: "EUR", OccurredAt: time.Now().UTC(),
	}
}

// wideCoverage is a window covering everything this test creates.
func wideCoverage(lines ...statement.PaymentStatementLine) statement.PaymentStatement {
	return statement.PaymentStatement{
		CoverageStart: time.Now().Add(-time.Hour).UTC(), CoverageEnd: time.Now().Add(time.Minute).UTC(), Lines: lines,
	}
}

// ---------------------------------------------------------------------
// No-effect capture (INV-IO-12, MX9).

type paySnapshot struct {
	ledgerTx, ledgerEntries, runs, mismatches, imports, lines int
	debits, credits                                           int64
	projDebit, projCredit                                     int64
	attempts, intents, receipts, withdrawals                  string
}

func capturePay(t *testing.T, pool *db.Pool, tenantID uuid.UUID) paySnapshot {
	t.Helper()
	var s paySnapshot
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		qs := []struct {
			sql string
			dst any
		}{
			{`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, &s.ledgerTx},
			{`SELECT count(*) FROM ledger_entries WHERE tenant_id = $1`, &s.ledgerEntries},
			{`SELECT COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'), 0)::bigint FROM ledger_entries WHERE tenant_id = $1`, &s.debits},
			{`SELECT COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0)::bigint FROM ledger_entries WHERE tenant_id = $1`, &s.credits},
			{`SELECT COALESCE(SUM(debit_total), 0)::bigint FROM wallet_balance_projection WHERE tenant_id = $1`, &s.projDebit},
			{`SELECT COALESCE(SUM(credit_total), 0)::bigint FROM wallet_balance_projection WHERE tenant_id = $1`, &s.projCredit},
			{`SELECT count(*) FROM reconciliation_runs WHERE tenant_id = $1`, &s.runs},
			{`SELECT count(*) FROM reconciliation_mismatches WHERE tenant_id = $1`, &s.mismatches},
			{`SELECT count(*) FROM payment_statement_imports WHERE tenant_id = $1`, &s.imports},
			{`SELECT count(*) FROM payment_statement_lines WHERE tenant_id = $1`, &s.lines},
			{`SELECT COALESCE(string_agg(id::text || state || updated_at::text || COALESCE(next_action_at::text, ''), ',' ORDER BY id), '') FROM payment_attempts WHERE tenant_id = $1`, &s.attempts},
			{`SELECT COALESCE(string_agg(id::text || status || updated_at::text, ',' ORDER BY id), '') FROM deposit_intents WHERE tenant_id = $1`, &s.intents},
			{`SELECT COALESCE(string_agg(id::text || COALESCE(resolved_at::text, ''), ',' ORDER BY id), '') FROM payment_provider_events WHERE tenant_id = $1`, &s.receipts},
			{`SELECT COALESCE(string_agg(id::text || state || updated_at::text, ',' ORDER BY id), '') FROM withdrawal_requests WHERE tenant_id = $1`, &s.withdrawals},
		}
		for _, q := range qs {
			if err := tx.QueryRow(ctx, q.sql, tenantID).Scan(q.dst); err != nil {
				return fmt.Errorf("%s: %w", q.sql, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	return s
}

// assertOnlyRunWritten: between before and after, the only writes are one
// reconciliation run and wantMismatches mismatch rows; the ledger, SUM
// equality, projections, attempts, intents, receipts and withdrawals are
// untouched.
func assertOnlyRunWritten(t *testing.T, before, after paySnapshot, wantMismatches int) {
	t.Helper()
	if after.runs != before.runs+1 || after.mismatches != before.mismatches+wantMismatches {
		t.Fatalf("expected +1 run and +%d mismatches, got runs %d->%d mismatches %d->%d",
			wantMismatches, before.runs, after.runs, before.mismatches, after.mismatches)
	}
	if after.ledgerTx != before.ledgerTx || after.ledgerEntries != before.ledgerEntries {
		t.Fatalf("INV-IO-12: ledger written by the match: tx %d->%d entries %d->%d", before.ledgerTx, after.ledgerTx, before.ledgerEntries, after.ledgerEntries)
	}
	if after.debits != after.credits || after.debits != before.debits || after.credits != before.credits {
		t.Fatalf("SUM(debits)==SUM(credits) or totals changed: %d/%d -> %d/%d", before.debits, before.credits, after.debits, after.credits)
	}
	if after.projDebit != before.projDebit || after.projCredit != before.projCredit {
		t.Fatalf("projection changed: %d/%d -> %d/%d", before.projDebit, before.projCredit, after.projDebit, after.projCredit)
	}
	if after.attempts != before.attempts || after.intents != before.intents || after.receipts != before.receipts || after.withdrawals != before.withdrawals {
		t.Fatal("INV-IO-12: an attempt, intent, receipt or withdrawal row changed during the match")
	}
	if after.imports != before.imports || after.lines != before.lines {
		t.Fatal("the match phase must not write the statement store")
	}
}

// ---------------------------------------------------------------------
// Clean runs.

// buildMixedHistory: deposits pending, succeeded, declined, cascaded to a
// second provider and succeeded there, a reversal of a succeeded deposit,
// and a tombstone (reversal of a never-seen original).
func (w *payWorld) buildMixedHistory(t *testing.T) (succeeded payments.PaymentAttempt) {
	t.Helper()
	w.deposit(t, 5000) // stays pending (inside the horizon)
	succeeded = w.deposit(t, 7000)
	w.succeed(t, w.mockA, payProvA, succeeded)
	if a := w.deposit(t, payments.MockAmountPlayerDeclineNoCascade); a.State != payments.AttemptDeclined {
		t.Fatalf("expected a declined attempt, got %s", a.State)
	}
	// Cascade: A declines cascadable; the child attempt goes to B.
	_, child := w.cascade(t)
	w.succeed(t, w.mockB, payProvB, child)

	// A reversal of the succeeded deposit, and a tombstone.
	w.deliverReversal(t, "rev-"+uuid.NewString()[:8], *succeeded.ProviderReference, succeeded.Amount)
	w.deliverReversal(t, "rev-"+uuid.NewString()[:8], "never-seen-"+uuid.NewString()[:8], 1234)
	return succeeded
}

// cascade makes a deposit that A declines cascadable and returns A's
// declined attempt and B's submitted child (driven inline or by the
// payments sweeper).
func (w *payWorld) cascade(t *testing.T) (declined, child payments.PaymentAttempt) {
	t.Helper()
	w.deposit(t, payments.MockAmountProviderDeclineCascade)
	find := func(provider string) payments.PaymentAttempt {
		var a payments.PaymentAttempt
		err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var id uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM payment_attempts WHERE tenant_id = $1 AND provider_id = $2 AND amount = $3 ORDER BY attempt_no DESC LIMIT 1`,
				w.f.tenantID, provider, payments.MockAmountProviderDeclineCascade).Scan(&id); err != nil {
				return err
			}
			var err error
			a, err = payments.GetAttemptByID(ctx, tx, id)
			return err
		})
		if err != nil {
			t.Fatalf("find cascade attempt on %s: %v", provider, err)
		}
		return a
	}
	declined = find(payProvA)
	if declined.State != payments.AttemptDeclined {
		t.Fatalf("expected A's attempt declined, got %s", declined.State)
	}
	child = find(payProvB)
	if child.ProviderReference == nil {
		sw := payments.NewSweeper(w.pool, w.orch, allowAllKYCGate{}, payments.MockCredentialResolver{})
		if st := sw.RunOnce(context.Background(), []uuid.UUID{w.f.tenantID}); len(st.Errors) > 0 {
			t.Fatalf("sweeper: %v", st.Errors)
		}
		child = find(payProvB)
	}
	if child.ProviderReference == nil {
		t.Fatalf("cascade child was not submitted (state %s)", child.State)
	}
	return declined, child
}

func (w *payWorld) deliverReversal(t *testing.T, reversalRef, originalRef string, amount int64) {
	t.Helper()
	payload := w.mockA.CallbackPayload(w.f.tenantID, payments.CallbackEventDepositReversal, reversalRef, originalRef, payments.OutcomeDeclined, amount, "EUR", "chargeback", false)
	v, err := w.orch.VerifyCallback(context.Background(), w.pool, w.f.tenantID, payProvA, payload)
	if err != nil {
		t.Fatalf("VerifyCallback (reversal): %v", err)
	}
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.orch.ReceiveVerifiedCallback(ctx, tx, w.f.tenantID, payProvA, v)
		return err
	}); err != nil {
		t.Fatalf("ReceiveVerifiedCallback (reversal): %v", err)
	}
}

func TestPaymentStatement_CleanMockRunOverMixedHistory(t *testing.T) {
	w := newPayWorld(t)
	w.buildMixedHistory(t)
	w.mustClean(t, w.srcA, PaymentStatementOptions{})
	w.mustClean(t, w.srcB, PaymentStatementOptions{})
}

// TestPaymentStatement_NoFinancialEffect is the §16.3 statement-capture
// test (INV-IO-12, MX9): the match writes one run plus its mismatches and
// nothing else; ledger row counts, SUM(debits)==SUM(credits), projections,
// attempts, intents, receipts and withdrawals are unchanged - for a clean
// run and for a run with mismatches.
func TestPaymentStatement_NoFinancialEffect(t *testing.T) {
	w := newPayWorld(t)
	a := w.buildMixedHistory(t)

	id, _ := w.fetchIngest(t, w.srcA, PaymentStatementOptions{})
	before := capturePay(t, w.pool, w.f.tenantID)
	if _, ms, _, err := w.matchErr(t, id, PaymentStatementOptions{}); err != nil || len(ms) != 0 {
		t.Fatalf("clean match: err=%v mismatches=%d", err, len(ms))
	}
	assertOnlyRunWritten(t, before, capturePay(t, w.pool, w.f.tenantID), 0)

	w.mockA.SetConfirmedAmount(*a.ProviderReference, a.Amount+1, "EUR")
	id, _ = w.fetchIngest(t, w.srcA, PaymentStatementOptions{})
	before = capturePay(t, w.pool, w.f.tenantID)
	_, ms, _, err := w.matchErr(t, id, PaymentStatementOptions{})
	if err != nil || len(ms) != 1 {
		t.Fatalf("divergent match: err=%v mismatches=%d", err, len(ms))
	}
	assertOnlyRunWritten(t, before, capturePay(t, w.pool, w.f.tenantID), 1)
}

// ---------------------------------------------------------------------
// One divergence per kind (8), each detected exactly once.

func TestPaymentStatement_Kind_Duplicate(t *testing.T) {
	w := newPayWorld(t)
	a := w.deposit(t, 7000)
	w.succeed(t, w.mockA, payProvA, a)
	w.mustClean(t, w.srcA, PaymentStatementOptions{})
	src := payDivergentSource{inner: w.srcA, mutate: func(s statement.PaymentStatement) statement.PaymentStatement {
		s.Lines = append(s.Lines, s.Lines[0], s.Lines[0]) // three copies: reported ONCE
		return s
	}}
	_, ms := w.run(t, src, PaymentStatementOptions{})
	mustOnePay(t, ms, MismatchKindPayDuplicate, "provider_reference="+*a.ProviderReference, "check=duplicate_line")
}

// TestPaymentStatement_Kind_DuplicatePlatformSuccess was removed (ADR
// 0095 §28, ledger-finance ruling §5 item 3, INV-DEP-1 / PAY-DOUBLE-
// CREDIT-1 Financial Hardening FH-3): after migration 0107's
// ledger_transactions_one_deposit_per_intent index, the direct
// ledger.Post this test used to build its "second capture" fixture now
// itself refuses (ledger.ErrDepositAlreadyPostedForIntent) - the
// scenario it tested is structurally unreachable on current schema.
// Split into two, both in inv_dep1_recon_integration_test.go:
// TestINVDEP1_Recon_M_CapturedUnposted_ReplacesDuplicate (the real
// receipt path, which now reports pay_captured_unposted instead of
// pay_duplicate) and TestINVDEP1_Recon_M_DuplicateDetector_LegacyDataShape
// (the detector itself, built on a scratch DB migrated to just BEFORE
// 0107 - the legacy-data shape it guards against).

func TestPaymentStatement_Kind_MissingPlatformRecord(t *testing.T) {
	w := newPayWorld(t)
	w.mustClean(t, w.srcA, PaymentStatementOptions{})
	src := payDivergentSource{inner: w.srcA, mutate: func(s statement.PaymentStatement) statement.PaymentStatement {
		s.Lines = append(s.Lines, payLineFor(payProvA, "unknown-ref-1", "", statement.PaymentLineDeposit, statement.PaymentStatusSucceeded, 900))
		return s
	}}
	_, ms := w.run(t, src, PaymentStatementOptions{})
	mustOnePay(t, ms, MismatchKindPayMissingPlatformRecord, "provider_reference=unknown-ref-1", "check=resolve")
}

func TestPaymentStatement_Kind_MissingProviderRecord(t *testing.T) {
	w := newPayWorld(t)
	a := w.deposit(t, 7000)
	w.succeed(t, w.mockA, payProvA, a)
	src := payDivergentSource{inner: w.srcA, mutate: func(s statement.PaymentStatement) statement.PaymentStatement {
		s.Lines = nil
		return s
	}}
	_, ms := w.run(t, src, PaymentStatementOptions{})
	mustOnePay(t, ms, MismatchKindPayMissingProviderRecord, "attempt="+a.ID.String())
}

func TestPaymentStatement_Kind_AmountMismatch_MockNative(t *testing.T) {
	// A genuine MOCK divergence: the provider's own confirmed amount
	// differs from the platform's.
	w := newPayWorld(t)
	a := w.deposit(t, 7000)
	w.succeed(t, w.mockA, payProvA, a)
	w.mockA.SetConfirmedAmount(*a.ProviderReference, 7001, "EUR")
	_, ms := w.run(t, w.srcA, PaymentStatementOptions{})
	mustOnePay(t, ms, MismatchKindPayAmountMismatch, "attempt="+a.ID.String(), "check=amount")
}

func TestPaymentStatement_Kind_AssetMismatch_CheckedBeforeAmount(t *testing.T) {
	w := newPayWorld(t)
	a := w.deposit(t, 7000)
	w.succeed(t, w.mockA, payProvA, a)
	// Asset AND amount differ: only the asset mismatch is reported.
	w.mockA.SetConfirmedAmount(*a.ProviderReference, 9999, "USD")
	_, ms := w.run(t, w.srcA, PaymentStatementOptions{})
	mustOnePay(t, ms, MismatchKindPayAssetMismatch, "attempt="+a.ID.String(), "check=asset")
}

func TestPaymentStatement_Kind_ReferenceMismatch(t *testing.T) {
	w := newPayWorld(t)
	a := w.deposit(t, 7000)
	w.succeed(t, w.mockA, payProvA, a)
	src := payDivergentSource{inner: w.srcA, mutate: mutateLine(*a.ProviderReference, func(l *statement.PaymentStatementLine) {
		l.ProviderReference = "other-ref-for-" + a.ID.String()[:8]
	})}
	_, ms := w.run(t, src, PaymentStatementOptions{})
	mustOnePay(t, ms, MismatchKindPayReferenceMismatch, "attempt="+a.ID.String(), "check=reference")
}

func TestPaymentStatement_Kind_StatusMismatch_DroppedSuccessCallback(t *testing.T) {
	// LF-C1(b): the provider settled, the success callback was lost.
	w := newPayWorld(t)
	a := w.deposit(t, 7000)
	w.mockA.Resolve(*a.ProviderReference, payments.OutcomeSucceeded, "", false)
	_, ms := w.run(t, w.srcA, PaymentStatementOptions{})
	mustOnePay(t, ms, MismatchKindPayStatusMismatch, "attempt="+a.ID.String(), "check=status")
}

func TestPaymentStatement_Kind_StatusMismatch_ProviderDeclinedPlatformSucceeded(t *testing.T) {
	w := newPayWorld(t)
	a := w.deposit(t, 7000)
	w.succeed(t, w.mockA, payProvA, a)
	w.mockA.Resolve(*a.ProviderReference, payments.OutcomeDeclined, "reversed_by_issuer", false)
	_, ms := w.run(t, w.srcA, PaymentStatementOptions{})
	mustOnePay(t, ms, MismatchKindPayStatusMismatch, "attempt="+a.ID.String(), "check=status")
}

func TestPaymentStatement_Kind_Unresolved_OnlyPastHorizon(t *testing.T) {
	w := newPayWorld(t)
	a := w.deposit(t, 5000) // pending, line pending
	w.mustClean(t, w.srcA, PaymentStatementOptions{})
	time.Sleep(5 * time.Millisecond)
	_, ms := w.run(t, w.srcA, PaymentStatementOptions{UnresolvedHorizon: time.Millisecond})
	mustOnePay(t, ms, MismatchKindPayUnresolved, "attempt="+a.ID.String(), "check=unresolved")

	// Line absent too: still pay_unresolved (never missing_provider).
	src := payDivergentSource{inner: w.srcA, mutate: func(s statement.PaymentStatement) statement.PaymentStatement { s.Lines = nil; return s }}
	_, ms = w.run(t, src, PaymentStatementOptions{})
	if len(ms) != 0 {
		t.Fatalf("inside the horizon an absent line for an in-flight attempt is not a finding:\n%s", renderMismatches(ms))
	}
	_, ms = w.run(t, src, PaymentStatementOptions{UnresolvedHorizon: time.Millisecond})
	mustOnePay(t, ms, MismatchKindPayUnresolved, "attempt="+a.ID.String(), "check=unresolved")
}

func TestPaymentStatement_UnresolvedDeferredReceipt(t *testing.T) {
	// LF95-C5: a verified receipt that resolves to no attempt stays
	// deferred; past the horizon it is pay_unresolved.
	w := newPayWorld(t)
	if d := w.applyReceipt(t, payProvA, payments.ReceiptEvidence{
		EventType: "deposit", ProviderReference: "lost-success-ref", Outcome: payments.OutcomeSucceeded, Amount: 100, AssetCode: "EUR",
	}); d != payments.DispositionDeferredUnresolved {
		t.Fatalf("expected a deferred receipt, got %s", d)
	}
	w.mustClean(t, w.srcA, PaymentStatementOptions{})
	time.Sleep(5 * time.Millisecond)
	_, ms := w.run(t, w.srcA, PaymentStatementOptions{UnresolvedHorizon: time.Millisecond})
	mustOnePay(t, ms, MismatchKindPayUnresolved, "provider_reference=lost-success-ref", "check=deferred_receipt")
}

// TestPaymentStatement_DeterministicOrdering: several divergences, two
// runs over the same import, identical (kind, key) sequences.
func TestPaymentStatement_DeterministicOrdering(t *testing.T) {
	w := newPayWorld(t)
	a := w.deposit(t, 7000)
	w.succeed(t, w.mockA, payProvA, a)
	b := w.deposit(t, 7100)
	w.mockA.Resolve(*b.ProviderReference, payments.OutcomeSucceeded, "", false)
	src := payDivergentSource{inner: w.srcA, mutate: func(s statement.PaymentStatement) statement.PaymentStatement {
		s.Lines = append(s.Lines,
			payLineFor(payProvA, "zz-unknown", "", statement.PaymentLineDeposit, statement.PaymentStatusSucceeded, 1),
			payLineFor(payProvA, "aa-unknown", "", statement.PaymentLineDeposit, statement.PaymentStatusSucceeded, 1))
		return s
	}}
	id, _ := w.fetchIngest(t, src, PaymentStatementOptions{})
	render := func() []string {
		_, ms, _, err := w.matchErr(t, id, PaymentStatementOptions{})
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(ms))
		for i, m := range ms {
			out[i] = string(m.MismatchKind) + "|" + m.ReconciliationKey
		}
		return out
	}
	first, second := render(), render()
	if len(first) != 3 || strings.Join(first, "\n") != strings.Join(second, "\n") {
		t.Fatalf("expected 3 identical, ordered mismatches, got\n%v\n%v", first, second)
	}
	if !strings.Contains(first[0], "aa-unknown") {
		t.Fatalf("lines must be matched in sorted reference order, got %v", first)
	}
}

// ---------------------------------------------------------------------
// Ledger join (LF95-C13), payouts, reversals, cross-provider.

// payoutFixture creates a payout attempt for provider on a submitted
// withdrawal and, when settlement != "", completes it (withdrawal.Complete
// posts withdrawal_completed under the settlement reference) and applies
// success to the attempt.
func (w *payWorld) payoutFixture(t *testing.T, provider, instruction, settlement string, amount int64, succeed bool) uuid.UUID {
	t.Helper()
	wrID, attemptID := uuid.New(), uuid.New()
	err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Fund the hold so the completion posting has something to move.
		hold, err := ledger.GetOrCreateAccount(ctx, tx, w.f.tenantID, &w.f.walletID, ledger.AccountPlayerWithdrawalHold, "EUR")
		if err != nil {
			return err
		}
		reqRef := "wreq-" + wrID.String()
		if _, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: w.f.tenantID, TransactionType: ledger.TxWithdrawalRequested, IdempotencyKey: reqRef, CorrelationID: wrID,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: w.f.cashAccountID, Direction: ledger.Debit, Amount: amount},
				{LedgerAccountID: hold, Direction: ledger.Credit, Amount: amount},
			},
		}); err != nil {
			return fmt.Errorf("post hold: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO withdrawal_requests (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, state, idempotency_key, provider_id, provider_reference)
			 VALUES ($1,$2,$3,$4,$5,'EUR',$6,'submitted',$7,$8,$9)`,
			wrID, w.f.tenantID, w.f.brandID, w.f.playerAccountID, w.f.walletID, amount, "wd-"+wrID.String(), provider, instruction); err != nil {
			return fmt.Errorf("insert withdrawal: %w", err)
		}
		if _, err := payments.InsertSubmittingAttempt(ctx, tx, payments.NewSubmittingAttempt{
			ID: attemptID, TenantID: w.f.tenantID, Operation: payments.AttemptOperationPayout, WithdrawalRequestID: &wrID,
			ProviderID: provider, PaymentMethod: "bank_transfer", AssetCode: "EUR", Amount: amount, Interactive: false,
			ClaimToken: uuid.New(), LeaseOwner: "test", LeaseUntil: time.Now().Add(time.Minute),
		}); err != nil {
			return err
		}
		if err := payments.MarkAccepted(ctx, tx, attemptID, payments.EvidenceSync, instruction, time.Now().Add(time.Hour)); err != nil {
			return fmt.Errorf("T4: %w", err)
		}
		if settlement != "" {
			if err := withdrawal.Complete(ctx, tx, wrID, provider, settlement); err != nil {
				return fmt.Errorf("complete: %w", err)
			}
		}
		if succeed {
			if err := payments.ApplySuccess(ctx, tx, attemptID, payments.SuccessEvidence{Evidence: payments.EvidenceCallback, ProviderReference: instruction}); err != nil {
				return fmt.Errorf("T7: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("payout fixture: %v", err)
	}
	return attemptID
}

func TestPaymentStatement_Payout_SettlementReferenceOnlyIsAMatch(t *testing.T) {
	w := newPayWorld(t)
	w.payoutFixture(t, payProvA, "instr-"+uuid.NewString()[:8], "settle-1-"+w.f.tenantID.String()[:8], 3000, true)
	src := payFixedSource{provider: payProvA, stmt: wideCoverage(
		payLineFor(payProvA, "settle-1-"+w.f.tenantID.String()[:8], "", statement.PaymentLinePayout, statement.PaymentStatusSucceeded, 3000))}
	w.mustClean(t, src, PaymentStatementOptions{})
}

func TestPaymentStatement_LedgerJoin_SucceededPayoutWithoutPosting(t *testing.T) {
	w := newPayWorld(t)
	instr := "instr-" + uuid.NewString()[:8]
	id := w.payoutFixture(t, payProvA, instr, "", 3000, true) // succeeded, never completed
	src := payFixedSource{provider: payProvA, stmt: wideCoverage(
		payLineFor(payProvA, instr, "", statement.PaymentLinePayout, statement.PaymentStatusSucceeded, 3000))}
	_, ms := w.run(t, src, PaymentStatementOptions{})
	mustOnePay(t, ms, MismatchKindPayStatusMismatch, "attempt="+id.String(), "check=ledger_join")
}

func TestPaymentStatement_LedgerJoin_PostingWithNoAttempt(t *testing.T) {
	// A psp_clearing deposit posting of this provider with no attempt
	// behind it (the dual-write orphan class).
	w := newPayWorld(t)
	err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		provider, ref := payProvA, "orphan-"+uuid.NewString()[:8]
		_, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: w.f.tenantID, TransactionType: ledger.TxDeposit, IdempotencyKey: provider + ":" + ref,
			ProviderID: &provider, ProviderTxID: &ref, CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: w.f.clearingID, Direction: ledger.Debit, Amount: 400},
				{LedgerAccountID: w.f.cashAccountID, Direction: ledger.Credit, Amount: 400},
			},
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	src := payFixedSource{provider: payProvA, stmt: wideCoverage()}
	_, ms := w.run(t, src, PaymentStatementOptions{})
	mustOnePay(t, ms, MismatchKindPayMissingPlatformRecord, "type=deposit", "check=ledger_join")
}

func TestPaymentStatement_Reversal(t *testing.T) {
	w := newPayWorld(t)
	a := w.deposit(t, 7000)
	w.succeed(t, w.mockA, payProvA, a)
	revRef := "rev-" + uuid.NewString()[:8]
	w.deliverReversal(t, revRef, *a.ProviderReference, a.Amount)
	neverRef := "never-" + uuid.NewString()[:8]
	w.deliverReversal(t, "rev-t-"+uuid.NewString()[:8], neverRef, 1234)

	depositLine := payLineFor(payProvA, *a.ProviderReference, a.MerchantReference, statement.PaymentLineDeposit, statement.PaymentStatusReversed, 7000)
	revLine := payLineFor(payProvA, revRef, "", statement.PaymentLineDepositReversal, statement.PaymentStatusSucceeded, 7000)
	revLine.OriginalProviderReference = *a.ProviderReference
	tombLine := payLineFor(payProvA, "rev-of-never", "", statement.PaymentLineDepositReversal, statement.PaymentStatusSucceeded, 1234)
	tombLine.OriginalProviderReference = neverRef
	w.mustClean(t, payFixedSource{provider: payProvA, stmt: wideCoverage(depositLine, revLine, tombLine)}, PaymentStatementOptions{})

	orphan := payLineFor(payProvA, "rev-unknown", "", statement.PaymentLineDepositReversal, statement.PaymentStatusSucceeded, 10)
	orphan.OriginalProviderReference = "not-a-deposit"
	_, ms := w.run(t, payFixedSource{provider: payProvA, stmt: wideCoverage(depositLine, revLine, tombLine, orphan)}, PaymentStatementOptions{})
	mustOnePay(t, ms, MismatchKindPayMissingPlatformRecord, "provider_reference=rev-unknown", "check=reversal")

	bad := revLine
	bad.Amount = 6999
	_, ms = w.run(t, payFixedSource{provider: payProvA, stmt: wideCoverage(depositLine, bad, tombLine)}, PaymentStatementOptions{})
	mustOnePay(t, ms, MismatchKindPayAmountMismatch, "provider_reference="+revRef, "check=amount")
}

func TestPaymentStatement_CrossProviderLineNeverMatches(t *testing.T) {
	// S95-C1 / INV-IO-14: a line of provider A carrying provider B's
	// reference and merchant reference never resolves B's attempt.
	w := newPayWorld(t)
	_, child := w.cascade(t) // A declines; B gets the child
	line := payLineFor(payProvA, *child.ProviderReference, child.MerchantReference, statement.PaymentLineDeposit, statement.PaymentStatusPending, child.Amount)
	src := payDivergentSource{inner: w.srcA, mutate: func(s statement.PaymentStatement) statement.PaymentStatement {
		s.Lines = append(s.Lines, line)
		return s
	}}
	_, ms := w.run(t, src, PaymentStatementOptions{})
	mustOnePay(t, ms, MismatchKindPayMissingPlatformRecord, "provider_reference="+*child.ProviderReference, "check=resolve")

	// And a line naming provider B inside provider A's statement refuses
	// the whole import.
	wrong := payLineFor(payProvB, "x", "", statement.PaymentLineDeposit, statement.PaymentStatusPending, 1)
	_, err := FetchPaymentStatement(context.Background(), payFixedSource{provider: payProvA, stmt: wideCoverage(wrong)}, w.f.tenantID, time.Now(), time.Now(), PaymentStatementOptions{})
	if !errors.Is(err, ErrPaymentStatementInvalid) {
		t.Fatalf("expected a cross-provider line to refuse the import, got %v", err)
	}
}

// ---------------------------------------------------------------------
// Coverage window, restart.

func TestPaymentStatement_CoverageWindow_RestartIsNotAFalseAlarm(t *testing.T) {
	w := newPayWorld(t)
	a := w.deposit(t, 7000)
	w.succeed(t, w.mockA, payProvA, a)
	time.Sleep(2 * time.Millisecond)
	// "Restart": a fresh MockProvider instance under the same id has no
	// records and a later coverage start. The succeeded attempt and its
	// deposit posting predate the window: neither is flagged.
	restarted := payments.NewMockProvider(payProvA, "EUR")
	src := payments.NewMockStatementSource(restarted, payments.MockCredentialResolver{})
	w.mustClean(t, src, PaymentStatementOptions{UnresolvedHorizon: time.Millisecond})

	// Control: the same empty statement with a window covering the attempt
	// flags it.
	_, ms := w.run(t, payFixedSource{provider: payProvA, stmt: wideCoverage()}, PaymentStatementOptions{})
	mustOnePay(t, ms, MismatchKindPayMissingProviderRecord, "attempt="+a.ID.String())
}

// ---------------------------------------------------------------------
// Isolation, snapshot, fetch outside tx.

func TestPaymentStatement_RefusesOutsideRepeatableRead(t *testing.T) {
	w := newPayWorld(t)
	id, _ := w.fetchIngest(t, w.srcA, PaymentStatementOptions{})
	err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, _, err := RunPaymentStatement(ctx, tx, w.f.tenantID, id, PaymentStatementOptions{})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "REPEATABLE READ") {
		t.Fatalf("expected a READ COMMITTED run to be refused, got %v", err)
	}
}

func TestPaymentStatement_FetchRunsWithNoTransactionHeld(t *testing.T) {
	w := newPayWorld(t)
	var held []bool
	src := payFixedSource{provider: payProvA, stmt: wideCoverage(), heldSeen: &held}
	out := ReconcilePaymentStatementForTenant(context.Background(), w.pool, nil, w.f.tenantID, time.Now().Add(-time.Hour), time.Now(), src, PaymentStatementOptions{})
	if out.Err != nil || out.Run.Status != StatusClean {
		t.Fatalf("expected a clean run, got %+v", out)
	}
	if len(held) != 1 || held[0] {
		t.Fatalf("INV-IO-1: Fetch must run exactly once with no transaction held, saw %v", held)
	}
	// Under a held transaction both the stream and the MOCK refuse.
	marked := txscope.Mark(context.Background())
	if _, err := FetchPaymentStatement(marked, src, w.f.tenantID, time.Now(), time.Now(), PaymentStatementOptions{}); !errors.Is(err, ErrPaymentFetchUnderTx) {
		t.Fatalf("stream fetch under tx: expected ErrPaymentFetchUnderTx, got %v", err)
	}
	if _, err := w.srcA.Fetch(marked, statement.PaymentFetchRequest{TenantID: w.f.tenantID, ProviderID: payProvA}); !errors.Is(err, payments.ErrStatementFetchUnderTx) {
		t.Fatalf("MOCK fetch under tx: expected ErrStatementFetchUnderTx, got %v", err)
	}
}

// TestPaymentStatement_SnapshotConsistencyUnderConcurrentPosting: a deposit
// success commits between the match's attempt read and its ledger read.
// Under REPEATABLE READ the match sees neither (clean); the READ COMMITTED
// kill control (the same matcher, isolation check bypassed) sees the
// posting without its succeeded attempt and raises a false P1.
func TestPaymentStatement_SnapshotConsistencyUnderConcurrentPosting(t *testing.T) {
	for _, rc := range []bool{false, true} {
		t.Run(fmt.Sprintf("read_committed=%v", rc), func(t *testing.T) {
			w := newPayWorld(t)
			a := w.deposit(t, 7000)
			line := payLineFor(payProvA, *a.ProviderReference, a.MerchantReference, statement.PaymentLineDeposit, statement.PaymentStatusPending, 7000)
			id, _ := w.fetchIngest(t, payFixedSource{provider: payProvA, stmt: wideCoverage(line)}, PaymentStatementOptions{})

			payMatchAfterAttemptsHook = func() { w.succeed(t, w.mockA, payProvA, a) }
			defer func() { payMatchAfterAttemptsHook = nil }()

			var ms []Mismatch
			var err error
			if rc {
				err = w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
					var e error
					_, ms, _, e = runPaymentStatementUnchecked(ctx, tx, w.f.tenantID, id, PaymentStatementOptions{})
					return e
				})
			} else {
				_, ms, _, err = w.matchErr(t, id, PaymentStatementOptions{})
			}
			if err != nil {
				t.Fatal(err)
			}
			if rc {
				mustOnePay(t, ms, MismatchKindPayMissingPlatformRecord, "check=ledger_join")
			} else if len(ms) != 0 {
				t.Fatalf("REPEATABLE READ must see one consistent snapshot, got:\n%s", renderMismatches(ms))
			}
		})
	}
}

func TestPaymentStatement_RLS_CrossTenant(t *testing.T) {
	wA := newPayWorld(t)
	a := wA.deposit(t, 7000)
	wA.succeed(t, wA.mockA, payProvA, a)
	idA, _ := wA.fetchIngest(t, wA.srcA, PaymentStatementOptions{})

	wB := newPayWorld(t)
	// Tenant B sees none of A's statement rows.
	err := wB.pool.WithTenant(context.Background(), wB.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM payment_statement_imports WHERE id = $1`, idA).Scan(&n); err != nil || n != 0 {
			return fmt.Errorf("import visible cross-tenant: n=%d err=%v", n, err)
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM payment_statement_lines WHERE import_id = $1`, idA).Scan(&n); err != nil || n != 0 {
			return fmt.Errorf("lines visible cross-tenant: n=%d err=%v", n, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Tenant B cannot write a row for tenant A.
	err = wB.pool.WithTenant(context.Background(), wB.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO payment_statement_imports (id, tenant_id, provider_id, source_label, is_mock, coverage_start, coverage_end, line_count, content_digest, fetched_at)
			VALUES ($1, $2, 'p', 'MOCK x', true, now() - interval '1 hour', now(), 0, decode(repeat('00', 32), 'hex'), now())`, uuid.New(), wA.f.tenantID)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("expected an RLS refusal writing another tenant's import, got %v", err)
	}
	// Tenant B cannot match tenant A's import.
	if _, _, _, err := wB.matchErr(t, idA, PaymentStatementOptions{}); err == nil {
		t.Fatal("tenant B must not be able to run tenant A's import")
	}
	// Tenant B's own MOCK statement (same MockProvider id, own instance
	// here) never lists A's records: B's run over A's mock is clean.
	srcBOverA := payments.NewMockStatementSource(wA.mockA, payments.MockCredentialResolver{})
	wB.mustClean(t, srcBOverA, PaymentStatementOptions{})
}

// ---------------------------------------------------------------------
// Statement store: idempotency, append-only, caps.

func TestPaymentStatement_IngestIsIdempotent(t *testing.T) {
	w := newPayWorld(t)
	src := payFixedSource{provider: payProvA, stmt: wideCoverage(
		payLineFor(payProvA, "r1", "", statement.PaymentLineDeposit, statement.PaymentStatusPending, 1),
		payLineFor(payProvA, "r2", "", statement.PaymentLineDeposit, statement.PaymentStatusPending, 2))}
	id1, reused1 := w.fetchIngest(t, src, PaymentStatementOptions{})
	before := capturePay(t, w.pool, w.f.tenantID)
	id2, reused2 := w.fetchIngest(t, src, PaymentStatementOptions{})
	after := capturePay(t, w.pool, w.f.tenantID)
	if reused1 || !reused2 || id1 != id2 {
		t.Fatalf("expected the second ingest to reuse the first import: %s/%v %s/%v", id1, reused1, id2, reused2)
	}
	if after.imports != before.imports || after.lines != before.lines {
		t.Fatalf("a re-fetch of identical content stored rows: imports %d->%d lines %d->%d", before.imports, after.imports, before.lines, after.lines)
	}
}

func TestPaymentStatement_StoreIsAppendOnly(t *testing.T) {
	w := newPayWorld(t)
	id, _ := w.fetchIngest(t, payFixedSource{provider: payProvA, stmt: wideCoverage(
		payLineFor(payProvA, "r1", "", statement.PaymentLineDeposit, statement.PaymentStatusPending, 1))}, PaymentStatementOptions{})
	for _, sql := range []string{
		`UPDATE payment_statement_imports SET source_label = 'MOCK x' WHERE id = $1`,
		`DELETE FROM payment_statement_imports WHERE id = $1`,
		`UPDATE payment_statement_lines SET amount = 2 WHERE import_id = $1`,
		`DELETE FROM payment_statement_lines WHERE import_id = $1`,
		`TRUNCATE payment_statement_lines`,
		`TRUNCATE payment_statement_imports CASCADE`,
		// Appending a line to a stored import breaks its declared count.
		`INSERT INTO payment_statement_lines (tenant_id, import_id, line_no, provider_id, kind, provider_reference, status, amount, asset_code, occurred_at)
		 SELECT tenant_id, id, 99, provider_id, 'deposit', 'late', 'pending', 1, 'EUR', now() FROM payment_statement_imports WHERE id = $1`,
	} {
		err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if strings.HasPrefix(sql, "TRUNCATE") {
				_, err := tx.Exec(ctx, sql)
				return err
			}
			_, err := tx.Exec(ctx, sql, id)
			return err
		})
		if err == nil {
			t.Errorf("expected %q to be refused", sql)
		}
	}
}

func TestPaymentStatement_LineCapRefusesAndStoresNothing(t *testing.T) {
	w := newPayWorld(t)
	src := payFixedSource{provider: payProvA, stmt: wideCoverage(
		payLineFor(payProvA, "r1", "", statement.PaymentLineDeposit, statement.PaymentStatusPending, 1),
		payLineFor(payProvA, "r2", "", statement.PaymentLineDeposit, statement.PaymentStatusPending, 1),
		payLineFor(payProvA, "r3", "", statement.PaymentLineDeposit, statement.PaymentStatusPending, 1))}
	before := capturePay(t, w.pool, w.f.tenantID)
	var logBuf strings.Builder
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	out := ReconcilePaymentStatementForTenant(context.Background(), w.pool, logger, w.f.tenantID, time.Now(), time.Now(), src, PaymentStatementOptions{MaxLines: 2})
	if !errors.Is(out.Err, ErrPaymentStatementTooLarge) {
		t.Fatalf("expected ErrPaymentStatementTooLarge, got %v", out.Err)
	}
	after := capturePay(t, w.pool, w.f.tenantID)
	if after.imports != before.imports || after.lines != before.lines || after.runs != before.runs {
		t.Fatal("an over-cap statement must store nothing and record no run")
	}
	if !strings.Contains(logBuf.String(), "P1") || !strings.Contains(logBuf.String(), "phase=fetch") {
		t.Fatalf("expected a P1 error log naming the fetch phase, got %s", logBuf.String())
	}
	var n int
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'reconciliation.sweep_run_failed'
		  AND metadata->>'stream' = 'payment_statement' AND metadata->>'phase' = 'fetch' AND metadata->>'severity' = 'P1'`, w.f.tenantID).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("expected one audited P1 failure, got n=%d err=%v", n, err)
	}
}

func TestPaymentStatement_OversizedFieldsRefused(t *testing.T) {
	w := newPayWorld(t)
	long := strings.Repeat("x", 256)
	cases := map[string]func(l *statement.PaymentStatementLine){
		"provider_reference": func(l *statement.PaymentStatementLine) { l.ProviderReference = long },
		"merchant_reference": func(l *statement.PaymentStatementLine) { l.MerchantReference = strings.Repeat("m", 65) },
		"original":           func(l *statement.PaymentStatementLine) { l.OriginalProviderReference = long },
		"settlement":         func(l *statement.PaymentStatementLine) { l.SettlementReference = long },
		"asset":              func(l *statement.PaymentStatementLine) { l.AssetCode = strings.Repeat("A", 17) },
		"control_char":       func(l *statement.PaymentStatementLine) { l.ProviderReference = "a\x01b" },
		"kind":               func(l *statement.PaymentStatementLine) { l.Kind = "refund" },
		"negative":           func(l *statement.PaymentStatementLine) { l.Amount = -1 },
	}
	for name, mut := range cases {
		l := payLineFor(payProvA, "r1", "", statement.PaymentLineDeposit, statement.PaymentStatusPending, 1)
		mut(&l)
		_, err := FetchPaymentStatement(context.Background(), payFixedSource{provider: payProvA, stmt: wideCoverage(l)}, w.f.tenantID, time.Now(), time.Now(), PaymentStatementOptions{})
		if !errors.Is(err, ErrPaymentStatementInvalid) {
			t.Errorf("%s: expected refusal at fetch, got %v", name, err)
		}
	}
	// The database CHECKs refuse the same shapes independently.
	for name, sql := range map[string]string{
		"line_count":       `INSERT INTO payment_statement_imports (id, tenant_id, provider_id, source_label, is_mock, coverage_start, coverage_end, line_count, content_digest, fetched_at) VALUES ($1, $2, 'p', 'MOCK x', true, now() - interval '1 hour', now(), 1000001, decode(repeat('00', 32), 'hex'), now())`,
		"label_not_mock":   `INSERT INTO payment_statement_imports (id, tenant_id, provider_id, source_label, is_mock, coverage_start, coverage_end, line_count, content_digest, fetched_at) VALUES ($1, $2, 'p', 'real-looking', true, now() - interval '1 hour', now(), 0, decode(repeat('00', 32), 'hex'), now())`,
		"label_too_long":   `INSERT INTO payment_statement_imports (id, tenant_id, provider_id, source_label, is_mock, coverage_start, coverage_end, line_count, content_digest, fetched_at) VALUES ($1, $2, 'p', 'MOCK ' || repeat('x', 300), true, now() - interval '1 hour', now(), 0, decode(repeat('00', 32), 'hex'), now())`,
		"empty_coverage":   `INSERT INTO payment_statement_imports (id, tenant_id, provider_id, source_label, is_mock, coverage_start, coverage_end, line_count, content_digest, fetched_at) VALUES ($1, $2, 'p', 'MOCK x', true, now(), now(), 0, decode(repeat('00', 32), 'hex'), now())`,
		"short_line_count": `INSERT INTO payment_statement_imports (id, tenant_id, provider_id, source_label, is_mock, coverage_start, coverage_end, line_count, content_digest, fetched_at) VALUES ($1, $2, 'p', 'MOCK x', true, now() - interval '1 hour', now(), 5, decode(repeat('00', 32), 'hex'), now())`,
		"merchant_reference": `WITH i AS (INSERT INTO payment_statement_imports (id, tenant_id, provider_id, source_label, is_mock, coverage_start, coverage_end, line_count, content_digest, fetched_at) VALUES ($1, $2, 'p', 'MOCK x', true, now() - interval '1 hour', now(), 1, decode(repeat('00', 32), 'hex'), now()) RETURNING id, tenant_id)
			INSERT INTO payment_statement_lines (tenant_id, import_id, line_no, provider_id, kind, provider_reference, merchant_reference, status, amount, asset_code, occurred_at) SELECT tenant_id, id, 0, 'p', 'deposit', 'r', repeat('m', 65), 'pending', 1, 'EUR', now() FROM i`,
	} {
		err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, uuid.New(), w.f.tenantID)
			return err
		})
		if err == nil {
			t.Errorf("%s: expected the database to refuse", name)
		}
	}
}

// ---------------------------------------------------------------------
// Production guard, horizon, LF-C1(b) end to end, sweep wiring.

func TestPaymentStatement_MockSourceRefusedInProduction(t *testing.T) {
	w := newPayWorld(t)
	regs := []providerkind.Registration{{Domain: "payments", Name: "statement_source", Component: w.srcA}}
	if err := providerkind.RefuseSyntheticInProduction("production", regs); err == nil || !strings.Contains(err.Error(), "payments/statement_source") {
		t.Fatalf("expected the MOCK payment statement source to be refused in production, got %v", err)
	}
	if !strings.Contains(w.srcA.Label(), "MOCK") {
		t.Fatal("the MOCK source's label must say MOCK")
	}
	// A synthetic source whose label hides MOCK is refused before any I/O.
	_, err := FetchPaymentStatement(context.Background(), sneakyLabel{payFixedSource{provider: payProvA, stmt: wideCoverage()}}, w.f.tenantID, time.Now(), time.Now(), PaymentStatementOptions{})
	if !errors.Is(err, ErrPaymentStatementInvalid) {
		t.Fatalf("expected a synthetic source without MOCK in its label to be refused, got %v", err)
	}
}

type sneakyLabel struct{ payFixedSource }

func (sneakyLabel) Label() string { return "totally real PSP" }

func TestPaymentStatement_HorizonEqualsSettlementWindow(t *testing.T) {
	if DefaultPaymentUnresolvedHorizon != payments.DefaultSettlementWindow {
		t.Fatalf("DefaultPaymentUnresolvedHorizon %s != payments.DefaultSettlementWindow %s", DefaultPaymentUnresolvedHorizon, payments.DefaultSettlementWindow)
	}
}

// TestPaymentStatement_LFC1b_DroppedSuccessConvergesThroughT17: the dropped
// success is flagged; T17 (an operator re-verify request - the automatic
// LF95-R1 re-drive job is NOT IMPLEMENTED) lets the payments sweeper
// QueryStatus and post through the normal idempotent path; the next run
// is clean. The stream itself writes nothing along the way.
func TestPaymentStatement_LFC1b_DroppedSuccessConvergesThroughT17(t *testing.T) {
	w := newPayWorld(t)
	a := w.deposit(t, 7000)
	w.mockA.Resolve(*a.ProviderReference, payments.OutcomeSucceeded, "", false)
	_, ms := w.run(t, w.srcA, PaymentStatementOptions{})
	mustOnePay(t, ms, MismatchKindPayStatusMismatch, "attempt="+a.ID.String())

	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return payments.Touch(ctx, tx, a.ID)
	}); err != nil {
		t.Fatalf("T17: %v", err)
	}
	sw := payments.NewSweeper(w.pool, w.orch, allowAllKYCGate{}, payments.MockCredentialResolver{})
	if st := sw.RunOnce(context.Background(), []uuid.UUID{w.f.tenantID}); len(st.Errors) > 0 {
		t.Fatalf("sweeper: %v", st.Errors)
	}
	if st := w.attempt(t, a.ID).State; st != payments.AttemptSucceeded {
		t.Fatalf("expected T17 + QueryStatus to post the success, attempt is %s", st)
	}
	w.mustClean(t, w.srcA, PaymentStatementOptions{})
}

func TestPaymentStatement_SweepWiringAndAudit(t *testing.T) {
	w := newPayWorld(t)
	a := w.deposit(t, 7000)
	w.succeed(t, w.mockA, payProvA, a)
	outs, err := RunSweepTenants(context.Background(), w.pool, nil, []uuid.UUID{w.f.tenantID}, time.Now().Add(-time.Hour), time.Now(),
		payNopSportsbookSource{}, payNopCasinoSource{}, w.srcA, nil)
	if err != nil || len(outs) != 1 {
		t.Fatalf("sweep: %v %d", err, len(outs))
	}
	ps := outs[0].PaymentStatement
	if len(ps) != 2 || ps[0].Err != nil || ps[0].Run.Status != StatusClean {
		t.Fatalf("expected the MOCK payment stream to run clean, got %+v", ps)
	}
	if ps[1].Err == nil {
		t.Fatal("a nil payment statement source must fail closed")
	}
	var md string
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT metadata::text FROM audit_log WHERE tenant_id = $1 AND action = 'reconciliation.sweep_run'
		  AND metadata->>'stream' = 'payment_statement' ORDER BY created_at DESC LIMIT 1`, w.f.tenantID).Scan(&md)
	}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"is_mock": true`, "MOCK in-process payment provider statement", `"statement_lines": 1`, "import_id", "coverage_start"} {
		if !strings.Contains(md, want) {
			t.Errorf("run audit metadata lacks %q: %s", want, md)
		}
	}
}

type payNopSportsbookSource struct{}

func (payNopSportsbookSource) Label() string { return "MOCK nop sportsbook (test-only)" }
func (payNopSportsbookSource) StatementLines(context.Context, pgx.Tx, uuid.UUID) ([]statement.SportsbookSettlementLine, error) {
	return nil, nil
}

type payNopCasinoSource struct{}

func (payNopCasinoSource) Label() string { return "MOCK nop casino (test-only)" }
func (payNopCasinoSource) Statement(context.Context, pgx.Tx, uuid.UUID, time.Time, time.Time) ([]statement.CasinoStatementLine, []statement.CasinoStatementTotal, error) {
	return nil, nil, nil
}

// ---------------------------------------------------------------------
// Migration 0102 up/down (scratch databases).

func payMigrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// migration0102Version derives 0102's version from its filename.
func migration0102Version(t *testing.T) int64 {
	t.Helper()
	entries, err := os.ReadDir(payMigrationsDir(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "_payment_statement_reconciliation.up.sql") {
			v, err := strconv.ParseInt(e.Name()[:4], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	t.Fatal("migration payment_statement_reconciliation not found")
	return 0
}

func payScratchThrough(t *testing.T, through int64) (*db.Pool, string) {
	t.Helper()
	src := payMigrationsDir(t)
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".sql") || len(n) < 4 {
			continue
		}
		v, err := strconv.ParseInt(n[:4], 10, 64)
		if err != nil || v > through {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, n))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, n), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pool, err := db.Connect(context.Background(), scratchdb.New(t, "m0102_"), 5, 5_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	applied, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if len(applied) == 0 || applied[len(applied)-1] != through {
		t.Fatalf("expected %d applied last, got %v", through, applied)
	}
	return pool, dir
}

func TestMigration0102_CleanDatabaseDownThenUpRoundTrip(t *testing.T) {
	v := migration0102Version(t)
	pool, dir := payScratchThrough(t, v)
	rolled, err := pool.MigrateDown(context.Background(), dir, 1)
	if err != nil || len(rolled) != 1 || rolled[0] != v {
		t.Fatalf("down: %v %v", rolled, err)
	}
	var n int
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name IN ('payment_statement_imports', 'payment_statement_lines')`).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("down must drop both tables: n=%d err=%v", n, err)
	}
	if applied, err := pool.MigrateUp(context.Background(), dir); err != nil || len(applied) != 1 {
		t.Fatalf("up again: %v %v", applied, err)
	}
}

func TestMigration0102_DownRefusesOnceAStatementExists(t *testing.T) {
	v := migration0102Version(t)
	pool, dir := payScratchThrough(t, v)
	f := seedFixture(t, pool)
	src := payFixedSource{provider: payProvA, stmt: wideCoverage(
		payLineFor(payProvA, "r1", "", statement.PaymentLineDeposit, statement.PaymentStatusPending, 1))}
	stmt, err := FetchPaymentStatement(context.Background(), src, f.tenantID, time.Now(), time.Now(), PaymentStatementOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := IngestPaymentStatement(ctx, tx, f.tenantID, src, stmt, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.MigrateDown(context.Background(), dir, 1); err == nil || !strings.Contains(err.Error(), "irreversible") {
		t.Fatalf("expected the down migration to refuse once a statement exists, got %v", err)
	}
}

func TestMigration0102_RuntimeGrantsMinimal(t *testing.T) {
	pool := testPool(t)
	var privs []string
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime')`).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return errSkipNoRuntime
		}
		rows, err := tx.Query(ctx, `SELECT table_name || ':' || privilege_type FROM information_schema.role_table_grants
			WHERE grantee = 'igaming_runtime' AND table_name IN ('payment_statement_imports', 'payment_statement_lines')`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				return err
			}
			privs = append(privs, p)
		}
		return rows.Err()
	})
	if errors.Is(err, errSkipNoRuntime) {
		t.Skip("igaming_runtime role not present")
	}
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(privs)
	want := "payment_statement_imports:INSERT,payment_statement_imports:SELECT,payment_statement_lines:INSERT,payment_statement_lines:SELECT"
	if strings.Join(privs, ",") != want {
		t.Fatalf("igaming_runtime privileges on the statement tables: got %v, want %s", privs, want)
	}
}

var errSkipNoRuntime = errors.New("no runtime role")
