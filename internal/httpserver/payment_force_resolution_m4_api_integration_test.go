//go:build integration

// PAY-PAYOUT-UNBOUND-RESOLVE-1 over real HTTP (ADR 0111 4.5, 4.7): the M4 kinds
// on the existing force-resolution route, against a MOCK statement source whose
// import is sealed with the server's B13 keys. Closed tokens and denied audit
// rows on every refusal.
package httpserver

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/capability"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

type m4APISource struct {
	lines []statement.PaymentStatementLine
}

func (m4APISource) SyntheticComponent()                     {}
func (m4APISource) Label() string                           { return "MOCK m4 http test statement" }
func (m4APISource) ProviderID() string                      { return "mock" }
func (m4APISource) PayoutLinesCarryMerchantReference() bool { return true }
func (s m4APISource) Fetch(context.Context, statement.PaymentFetchRequest) (statement.PaymentStatement, error) {
	return statement.PaymentStatement{}, errors.New("not used")
}

func m4APIKeys(t *testing.T) *payoutinstrument.Service {
	t.Helper()
	m, fp := make([]byte, 32), make([]byte, 32)
	_, _ = rand.Read(m)
	_, _ = rand.Read(fp)
	k, err := payoutinstrument.NewKeys("m4api", map[string][]byte{"m4api": m}, "m4fp", map[string][]byte{"m4fp": fp})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := payoutinstrument.NewService(k, payoutinstrument.DefaultKinds())
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// unboundPayout claims a real USD MOCK payout and parks it as an UNBOUND
// dispute (invalid_provider_reference, no reference); returns (withdrawal, attempt, merchant reference).
func (w *frWorld) unboundPayout(amount int64) (uuid.UUID, uuid.UUID, string) {
	t := w.a.t
	t.Helper()
	ctx := context.Background()
	pool := w.a.pool
	fundWallet(t, pool, w.tenant, w.brand, w.player, "USD", 100000)
	if err := pool.WithTenant(ctx, w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO withdrawal_policies (tenant_id, asset_code, approval_threshold_minor_units, required_approvals, effective_from)
			VALUES ($1, 'USD', 1000000, 2, now() - interval '1 hour') ON CONFLICT DO NOTHING`, w.tenant)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	wr := mustCreateWithdrawalRequest(t, pool, w.tenant, w.brand, w.player, walletIDFor(t, pool, w.tenant, w.player, "USD"), "USD", amount)
	do := func(fn func(ctx context.Context, tx pgx.Tx) error) {
		t.Helper()
		if err := pool.WithTenant(ctx, w.tenant, fn); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	do(func(ctx context.Context, tx pgx.Tx) error { return withdrawal.MoveToPendingReview(ctx, tx, wr.ID) })
	do(func(ctx context.Context, tx pgx.Tx) error {
		ok, err := withdrawal.Approve(ctx, tx, wr.ID, uuid.New(), true, nil, nil)
		if err == nil && !ok {
			err = errors.New("automated approval did not approve")
		}
		return err
	})
	claim, err := w.orch.ClaimForDispatch(ctx, pool, payments.KYCEnforcementPayoutGate{}, w.tenant, wr.ID, "bank_transfer",
		payments.SubmitActor{StaffID: uuid.New(), IPAddress: "127.0.0.1", UserAgent: "t", RequestID: uuid.NewString()})
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	var merchant string
	do(func(ctx context.Context, tx pgx.Tx) error {
		if err := payments.ApplyDisputeFromNonTerminal(ctx, tx, claim.Attempt.ID, payments.EvidenceSync, "invalid_provider_reference"); err != nil {
			return err
		}
		a, err := payments.GetAttemptByID(ctx, tx, claim.Attempt.ID)
		merchant = a.MerchantReference
		if err == nil && (a.State != payments.AttemptDisputed || a.ProviderReference != nil) {
			err = errors.New("setup: not an unbound park")
		}
		return err
	})
	return wr.ID, claim.Attempt.ID, merchant
}

func TestForceResolutionAPI_M4_Paid_OverHTTP(t *testing.T) {
	reg := &payments.StatementSourceRegistry{}
	reg.Register("mock")
	pi := m4APIKeys(t)
	cgAPIPayoutInstruments = pi
	t.Cleanup(func() { cgAPIPayoutInstruments = nil })
	w := newFRWorldWithSources(t, reg)
	approver := w.a.staff(uuid.Nil, "platform_admin")
	w.grant(w.tenant, w.pa, approver, true, capability.CapabilityPaymentForceResolveApprove)

	wrID, attempt, merchant := w.unboundPayout(5000)
	r := "m4-http-" + uuid.NewString()[:10]
	if err := w.a.pool.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		now := time.Now().UTC().Truncate(time.Microsecond)
		stmt := statement.PaymentStatement{CoverageStart: now.Add(-time.Hour), CoverageEnd: now.Add(time.Minute),
			Lines: []statement.PaymentStatementLine{{ProviderID: "mock", ProviderReference: r, MerchantReference: merchant,
				Kind: statement.PaymentLinePayout, Status: statement.PaymentStatusSucceeded, Amount: 5000, AssetCode: "USD", OccurredAt: now}}}
		_, _, err := reconciliation.IngestPaymentStatementSealed(ctx, tx, w.tenant, m4APISource{}, stmt, now, pi.Keys())
		return err
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	var line uuid.UUID
	if err := w.a.pool.WithPlatformActingInTenant(context.Background(), w.acting, w.tenant, uuid.Nil, payments.OperationKindForceResolve, func(ctx context.Context, tx pgx.Tx) error {
		ev, err := payments.EvaluateM4Evidence(ctx, tx, w.tenant, attempt)
		if err == nil && (ev.Verdict != payments.M4VerdictPaid || ev.LineID == nil) {
			err = errors.New("setup: verdict " + ev.Verdict)
		}
		if err == nil {
			line = *ev.LineID
		}
		return err
	}); err != nil {
		t.Fatalf("evidence: %v", err)
	}
	body := func(l uuid.UUID) map[string]any {
		return map[string]any{"attempt_id": attempt.String(), "kind": "m4_evidence_paid", "basis_code": payments.BasisProviderConfirmedOutOfBand,
			"evidence_ref_hash": strings.Repeat("cd", 32), "reason_code": "http-m4", "note": "http m4 (MOCK)", "evidence_line_id": l.String()}
	}
	// A tenant-scoped requester cannot evaluate the evidence scope (MR060): audited 403.
	before := w.deniedAudits()
	res := w.a.do("POST", w.base(), w.tok(w.f1), body(line))
	if res.status != http.StatusForbidden || !strings.Contains(string(res.body), `"`+payments.TokenForceResolveNotPermitted+`"`) || w.deniedAudits() != before+1 {
		t.Fatalf("tenant-scoped M4: want audited 403, got %d %s", res.status, res.body)
	}
	// A line other than the deterministic one: audited 409 force_resolve_evidence_mismatch.
	before = w.deniedAudits()
	res = w.a.do("POST", w.base(), w.tok(w.acting), body(uuid.New()))
	if res.status != http.StatusConflict || !strings.Contains(string(res.body), `"`+payments.TokenForceResolveEvidenceMismatch+`"`) || w.deniedAudits() != before+1 {
		t.Fatalf("wrong line: want audited 409 %s, got %d %s", payments.TokenForceResolveEvidenceMismatch, res.status, res.body)
	}
	// The real request.
	res = w.a.do("POST", w.base(), w.tok(w.acting), body(line))
	if res.status != http.StatusCreated {
		t.Fatalf("M4 request: %d %s", res.status, res.body)
	}
	var d struct {
		ID, PayloadHash, EvidenceReference, EvidenceVerdict, EvidenceLineID string
		EvidenceImportIDs                                                   []string
	}
	var raw map[string]any
	res.decode(t, &raw)
	d.ID, _ = raw["id"].(string)
	d.PayloadHash, _ = raw["payload_hash"].(string)
	d.EvidenceReference, _ = raw["evidence_reference"].(string)
	d.EvidenceVerdict, _ = raw["evidence_verdict"].(string)
	d.EvidenceLineID, _ = raw["evidence_line_id"].(string)
	if d.EvidenceReference != r || d.EvidenceVerdict != "paid" || d.EvidenceLineID != line.String() {
		t.Fatalf("M4 DTO: %v", raw)
	}
	dec := w.a.do("POST", w.base()+"/"+d.ID+"/approve", w.a.token(approver, uuid.Nil, "platform_admin"),
		map[string]any{"payload_hash": d.PayloadHash, "reason_code": "http-m4"})
	if dec.status != http.StatusOK || !strings.Contains(string(dec.body), `"executed":true`) {
		t.Fatalf("approve: %d %s", dec.status, dec.body)
	}
	var state string
	if err := w.a.pool.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM withdrawal_requests WHERE id = $1`, wrID).Scan(&state)
	}); err != nil || state != "completed" {
		t.Fatalf("withdrawal: %s %v", state, err)
	}
}

// Without the B13 key module every M4 is refused as unsealed (fail closed).
func TestForceResolutionAPI_M4_NoKeys_Unsealed(t *testing.T) {
	reg := &payments.StatementSourceRegistry{}
	reg.Register("mock")
	w := newFRWorldWithSources(t, reg)
	_, attempt, merchant := w.unboundPayout(5000)
	other := m4APIKeys(t) // seals with a key the server does not hold
	r := "m4-http-" + uuid.NewString()[:10]
	if err := w.a.pool.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		now := time.Now().UTC().Truncate(time.Microsecond)
		stmt := statement.PaymentStatement{CoverageStart: now.Add(-time.Hour), CoverageEnd: now.Add(time.Minute),
			Lines: []statement.PaymentStatementLine{{ProviderID: "mock", ProviderReference: r, MerchantReference: merchant,
				Kind: statement.PaymentLinePayout, Status: statement.PaymentStatusSucceeded, Amount: 5000, AssetCode: "USD", OccurredAt: now}}}
		_, _, err := reconciliation.IngestPaymentStatementSealed(ctx, tx, w.tenant, m4APISource{}, stmt, now, other.Keys())
		return err
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	var line uuid.UUID
	if err := w.a.pool.WithPlatformActingInTenant(context.Background(), w.acting, w.tenant, uuid.Nil, payments.OperationKindForceResolve, func(ctx context.Context, tx pgx.Tx) error {
		ev, err := payments.EvaluateM4Evidence(ctx, tx, w.tenant, attempt)
		if err == nil && ev.LineID != nil {
			line = *ev.LineID
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := w.deniedAudits()
	res := w.a.do("POST", w.base(), w.tok(w.acting), map[string]any{"attempt_id": attempt.String(), "kind": "m4_evidence_paid",
		"basis_code": payments.BasisProviderConfirmedOutOfBand, "evidence_ref_hash": strings.Repeat("cd", 32), "reason_code": "http-m4",
		"evidence_line_id": line.String()})
	if res.status != http.StatusConflict || !strings.Contains(string(res.body), `"`+payments.TokenForceResolveEvidenceUnsealed+`"`) || w.deniedAudits() != before+1 {
		t.Fatalf("no keys: want audited 409 %s, got %d %s", payments.TokenForceResolveEvidenceUnsealed, res.status, res.body)
	}
	var n int
	if err := w.a.pool.WithPrincipalScope(context.Background(), w.tenant, w.f1, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_manual_resolutions WHERE tenant_id = $1`, w.tenant).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("a refused M4 left %d rows (%v)", n, err)
	}
}
