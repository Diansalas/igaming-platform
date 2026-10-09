//go:build integration

// PAY-PAYOUT-UNBOUND-RESOLVE-1 reconciliation of EXECUTED M4 resolutions (ADR
// 0111 §4.6: S-1 keys, R-1 predicates, R-2 ledger_join attribution, the
// post-M4-not-paid hint), through the REAL three-phase payment_statement
// stream in the REAL WithTenantSnapshot session shape.
package payments

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

const m4NotPaidHint = "resolution: possible double payout after M4 not-paid: recovery via compensating debit (K2) or off-platform recovery; never allocation"

// R-2: the M4-paid completion (attempt still disputed) is attributed in the
// ledger join; with the confirming line in the run nothing is raised for it.
func TestM4Recon_PaidCompletion_AttributedInLedgerJoin(t *testing.T) {
	m := newM4World(t)
	p := m.park(600)
	r := "m4-R-" + uuid.NewString()[:12]
	// The PSP re-delivers the SAME line (same content, same occurred_at) in a later statement.
	l := m.line(r, p.fresh.MerchantReference, statement.PaymentStatusSucceeded, 600, time.Now().UTC().Truncate(time.Microsecond))
	m.ingest(m4Imp{}, l)
	ev := m.mustEvidence(p.fresh.ID, M4VerdictPaid)
	m.execute(p, ResolutionM4EvidencePaid, ev)
	ms := m.stmtRun(m.source(false, l))
	if got := mismatchesOf(ms, reconciliation.MismatchKindPayMissingPlatformRecord, "provider_tx_id="+r); len(got) != 0 {
		t.Fatalf("the M4-paid completion is not attributed in ledger_join (R-2):\n%s", render(ms))
	}
	m.requireNone(ms, reconciliation.MismatchKindPayCapturedUnposted, p.fresh.ID, "own completion keyed by R clears")
	m.requireNone(ms, reconciliation.MismatchKindPayDeclaredPaidUnconfirmed, p.fresh.ID, "no contradiction")
	// And on a later run with no line at all (standing): still attributed, still clean for this attempt.
	ms = m.stmtRun(m.source(false))
	if got := mismatchesOf(ms, reconciliation.MismatchKindPayMissingPlatformRecord, "provider_tx_id="+r); len(got) != 0 {
		t.Fatalf("standing run raised the attributed completion:\n%s", render(ms))
	}
}

// R-1 + S-1: a contradicting line that names ONLY R (no merchant reference),
// back-dated, in any import, raises pay_declared_paid_unconfirmed on every run.
func TestM4Recon_PaidContradicted_ByLineNamingOnlyR_BackDated(t *testing.T) {
	m := newM4World(t)
	p, r, ev := m.paidPark(610)
	m.execute(p, ResolutionM4EvidencePaid, ev)
	l := m.payoutLine(r, "", statement.PaymentStatusDeclined, 610)
	l.OccurredAt = time.Now().Add(-96 * time.Hour).UTC()
	ms := m.stmtRun(m.source(false, l))
	f := m.requireOne(ms, reconciliation.MismatchKindPayDeclaredPaidUnconfirmed, p.fresh.ID, "declined line on R")
	if !strings.Contains(f.ReconciliationKey, "check=m4_paid_contradicted") || !strings.Contains(f.ActualValue, "status=declined") {
		t.Fatalf("finding: %+v", f)
	}
	// Standing: the next run with an empty statement still raises (persisted line).
	ms = m.stmtRun(m.source(false))
	m.requireOne(ms, reconciliation.MismatchKindPayDeclaredPaidUnconfirmed, p.fresh.ID, "standing")
}

// R-1: a second DISTINCT succeeded line on the merchant reference raises.
func TestM4Recon_PaidSecondDistinctSucceeded(t *testing.T) {
	m := newM4World(t)
	p, r, ev := m.paidPark(620)
	m.execute(p, ResolutionM4EvidencePaid, ev)
	_ = r
	ms := m.stmtRun(m.source(false, m.payoutLine("m4-second-"+uuid.NewString()[:8], p.fresh.MerchantReference, statement.PaymentStatusSucceeded, 620)))
	f := m.requireOne(ms, reconciliation.MismatchKindPayDeclaredPaidUnconfirmed, p.fresh.ID, "second succeeded line")
	if !strings.Contains(f.ActualValue, "distinct succeeded") {
		t.Fatalf("finding: %+v", f)
	}
}

// R-1 + the L-4 hint: after an executed M4 not-paid, a succeeded line raises
// pay_declared_not_paid_but_paid and the captured-unposted finding reads the
// post-M4 text.
func TestM4Recon_NotPaidThenSucceeded_RaisesWithPostM4Hint(t *testing.T) {
	m := newM4World(t)
	p, ev := m.notPaidPark(630)
	m.execute(p, ResolutionM4EvidenceNotPaid, ev)
	ms := m.stmtRun(m.source(false, m.payoutLine("m4-late-"+uuid.NewString()[:8], p.fresh.MerchantReference, statement.PaymentStatusSucceeded, 630)))
	f := m.requireOne(ms, reconciliation.MismatchKindPayDeclaredNotPaidButPaid, p.fresh.ID, "succeeded after M4 not-paid")
	if !strings.Contains(f.ReconciliationKey, "check=m4_not_paid_but_paid") || f.ExpectedValue != m4NotPaidHint {
		t.Fatalf("finding: %+v", f)
	}
	cu := m.requireOne(ms, reconciliation.MismatchKindPayCapturedUnposted, p.fresh.ID, "captured unposted")
	if cu.ExpectedValue != m4NotPaidHint {
		t.Fatalf("post-M4 hint: %q", cu.ExpectedValue)
	}
}

// S-1: after a destination_mismatch not-paid, a succeeded line on the BOUND
// reference (no merchant reference) raises. destination_mismatch is not a
// reason reconciliation classifies, so only the M4 key additions load the line.
func TestM4Recon_DestinationMismatchNotPaid_LineOnBoundReferenceRaises(t *testing.T) {
	m := newM4World(t)
	wr, a := m.payout(640)
	m.tx(func(ctx context.Context, tx pgx.Tx) error {
		return ApplyDisputeFromNonTerminal(ctx, tx, a.ID, EvidenceCallback, "destination_mismatch")
	})
	a = m.attempt(a.ID)
	x := *a.ProviderReference
	m.ingest(m4Imp{start: a.CreatedAt.Add(-time.Minute), end: a.LastSentAt.Add(25 * time.Hour)},
		m.line(x, "", statement.PaymentStatusDeclined, 640, time.Now()))
	ev := m.mustEvidence(a.ID, M4VerdictNotPaid)
	r, err := m.request(m.acting, m.m4In(a.ID, ResolutionM4EvidenceNotPaid, ev.LineID))
	k3RequireNoErr(t, err, "request")
	if r.ProviderReferenceAtSubmission == nil || *r.ProviderReferenceAtSubmission != x {
		t.Fatalf("C-6 pin: %v", r.ProviderReferenceAtSubmission)
	}
	out, err := m.decide(m.acting2, r, ResolutionApprove)
	k3RequireNoErr(t, err, "approve")
	if !out.Executed || m.wd(wr.ID).State != "failed" {
		t.Fatalf("not executed: %+v", out)
	}
	ms := m.stmtRun(m.source(false, m.payoutLine(x, "", statement.PaymentStatusSucceeded, 640)))
	m.requireOne(ms, reconciliation.MismatchKindPayDeclaredNotPaidButPaid, a.ID, "succeeded line on the bound reference")
}

// The importer writes the seal IN the INSERT with the in-process declaration;
// the Go verifier accepts it and refuses another key, a missing seal, and no keys.
func TestM4_ImportSeal_WrittenInTheInsert_AndVerified(t *testing.T) {
	m := newM4World(t)
	p := m.park(650)
	sealed := m.ingest(m4Imp{}, m.line(m4Ref(), p.fresh.MerchantReference, statement.PaymentStatusSucceeded, 650, time.Now()))
	undeclared := m.ingest(m4Imp{noDecl: true}, m.line(m4Ref(), "x", statement.PaymentStatusSucceeded, 1, time.Now()))
	unsealed := m.ingest(m4Imp{unsealed: true}, m.line(m4Ref(), "y", statement.PaymentStatusSucceeded, 1, time.Now()))
	rows := m.sysQuery(`SELECT id::text AS id, import_seal IS NOT NULL AS sealed, seal_kid, imported_by_service, payout_lines_carry_merchant_reference AS decl, is_mock
		FROM payment_statement_imports WHERE tenant_id = $1 ORDER BY fetched_at`, m.f.tenantID)
	if len(rows) != 3 {
		t.Fatalf("imports: %v", rows)
	}
	if rows[0]["sealed"] != true || rows[0]["seal_kid"] != "m4-test" || rows[0]["imported_by_service"] != reconciliation.ImportedByServicePaymentStatement ||
		rows[0]["decl"] != true || rows[0]["is_mock"] != true {
		t.Fatalf("sealed import row: %v", rows[0])
	}
	// The unsealed import uses the pre-0125 column list (no declaration, no service).
	if rows[1]["decl"] != false || rows[2]["sealed"] != false || rows[2]["imported_by_service"] != nil {
		t.Fatalf("declaration / unsealed rows: %v %v", rows[1], rows[2])
	}
	acting := func(fn func(ctx context.Context, tx pgx.Tx) error) error {
		return m.pool.WithPlatformActingInTenant(context.Background(), m.acting.ID, m.f.tenantID, uuid.Nil, OperationKindForceResolve, fn)
	}
	if err := acting(func(ctx context.Context, tx pgx.Tx) error {
		return payoutinstrument.VerifyStatementImportInTx(ctx, tx, m.keys, m.f.tenantID, sealed)
	}); err != nil {
		t.Fatalf("a sealed import must verify: %v", err)
	}
	if err := acting(func(ctx context.Context, tx pgx.Tx) error {
		return payoutinstrument.VerifyStatementImportInTx(ctx, tx, m4NewKeys(t), m.f.tenantID, sealed)
	}); !errors.Is(err, payoutinstrument.ErrImportSealInvalid) {
		t.Fatalf("another key must not verify: %v", err)
	}
	if err := acting(func(ctx context.Context, tx pgx.Tx) error {
		return payoutinstrument.VerifyStatementImportInTx(ctx, tx, m.keys, m.f.tenantID, unsealed)
	}); !errors.Is(err, payoutinstrument.ErrStatementImportUnsealed) {
		t.Fatalf("an unsealed import must refuse: %v", err)
	}
	if err := acting(func(ctx context.Context, tx pgx.Tx) error {
		return payoutinstrument.VerifyStatementImportInTx(ctx, tx, nil, m.f.tenantID, sealed)
	}); !errors.Is(err, payoutinstrument.ErrImportSealInvalid) {
		t.Fatalf("no keys must refuse: %v", err)
	}
	_ = undeclared
	// Re-ingesting identical content reuses the import (idempotent) and keeps its seal.
	again := m.ingest(m4Imp{noDecl: true}, m.line(m4Ref(), "x2", statement.PaymentStatusSucceeded, 1, time.Now()))
	if again == undeclared {
		t.Fatal("distinct content must be a distinct import")
	}
	// The acting session now READS imports, lines and the typed reference
	// evidence of its own tenant only (C-3), and never writes them.
	if err := acting(func(ctx context.Context, tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM payment_statement_lines WHERE tenant_id <> $1`, m.f.tenantID).Scan(&n); err != nil || n != 0 {
			return errors.New("acting session sees foreign lines")
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_statement_lines WHERE tenant_id = $1`, m.f.tenantID).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
}
