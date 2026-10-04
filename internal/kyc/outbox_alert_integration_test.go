//go:build integration

package kyc

// PRH-2 E1 (ADR 0106 section 10.6): redaction, alert dedup, the raise path.
// The KYC alert is durable but UNROUTED: alert NOTIFICATION is NOT IMPLEMENTED
// (no route, channel or recipient; ALERT-DELIVERY-1 OPEN) - these tests prove
// only that the alert row exists with the right shape and scope.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

// failTerminalNow drives r's player's create row to failed_terminal with class
// ambiguous (MaxFailedAttempts 1).
func (r *rig) failTerminalNow(f fixture) uuid.UUID {
	r.t.Helper()
	r.w.Config.MaxFailedAttempts = 1
	r.createImpl = func(context.Context, CreateVerificationInput) (ProviderResult, error) {
		return ProviderResult{}, errors.New("vendor down")
	}
	v, _ := requestCreate(r.t, r.pool, f, "mock")
	passUntilQuiet(r.t, r.w)
	if row := onlyRow(r.t, r.pool, f.tenantID, v.ID, OpCreate); row.State != OutboxFailedTerminal {
		r.t.Fatalf("create row = %s, want failed_terminal", row.State)
	}
	return v.ID
}

// 46. Redaction: a vendor error carrying a secret-shaped string, and a vendor
// reference, appear in no audit row, no alert attribute, no last_error_class,
// no outbox column and no worker log line.
func TestOutboxAlert_46_Redaction(t *testing.T) {
	r := newRig(t)
	var logs bytes.Buffer
	r.w.Logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	const secret = "VENDOR-SECRET-xyz-4242"
	r.w.Config.MaxFailedAttempts = 2

	// A create whose error carries the secret, ending failed_terminal (alert).
	r.createImpl = func(context.Context, CreateVerificationInput) (ProviderResult, error) {
		return ProviderResult{}, errors.New("POST https://vendor.example/verify?token=" + secret)
	}
	v := r.create()
	row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
	r.pass()
	makeDueNow(t, row.ID)
	r.pass()
	if got := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate); got.State != OutboxFailedTerminal {
		t.Fatalf("setup: create row = %s", got.State)
	}

	// A second player whose create is accepted (a vendor REFERENCE exists) and
	// whose submit then fails ambiguously.
	other := seedSecondAccount(t, r.pool, r.f)
	const vendorRef = "mock-ref-REDACTION-REFERENCE-9191"
	r.createImpl = func(context.Context, CreateVerificationInput) (ProviderResult, error) {
		return ProviderResult{ProviderReference: vendorRef, Outcome: ProviderPending, Reason: "created"}, nil
	}
	v2, _ := requestCreate(t, r.pool, other, "mock")
	passUntilQuiet(t, r.w)
	seedDocument(t, r.pool, other, v2.ID, DocumentPassport, "p.png")
	r.submitImpl = func(context.Context, string, []SubmittedDocument, CallContext) (ProviderResult, error) {
		return ProviderResult{}, errors.New("submit failed for " + vendorRef + " token=" + secret)
	}
	passUntilQuiet(t, r.w)

	// Everything durable the worker produced, as text.
	var corpus strings.Builder
	corpus.WriteString(logs.String())
	if err := r.pool.WithTenant(context.Background(), r.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for _, q := range []string{
			`SELECT coalesce(string_agg(metadata::text, ' '), '') FROM audit_log WHERE tenant_id = $1`,
			`SELECT coalesce(string_agg(to_jsonb(o)::text, ' '), '') FROM kyc_submission_outbox o WHERE tenant_id = $1`,
			`SELECT coalesce(string_agg(attributes::text || discriminator, ' '), '') FROM alerts WHERE subject_tenant_id = $1`,
		} {
			var s string
			if err := tx.QueryRow(ctx, q, r.f.tenantID).Scan(&s); err != nil {
				return err
			}
			corpus.WriteString(s)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{secret, vendorRef, "vendor.example", "token="} {
		if strings.Contains(corpus.String(), leak) {
			t.Errorf("%q leaked into an audit row, alert, outbox column or worker log line", leak)
		}
	}
	if !strings.Contains(corpus.String(), "kyc_outbox_provider_call_failed") {
		t.Error("vacuity: the worker log must have recorded the failures (closed classes only)")
	}
}

// 47. Alert dedup: one open alert per (subject tenant, discriminator), later
// failures add occurrences; separate per tenant and per suffix; the subject
// tenant reads but cannot ack; other tenants cannot see it.
func TestOutboxAlert_47_DedupAndScope(t *testing.T) {
	r := newRig(t)
	second := seedSecondAccount(t, r.pool, r.f)
	r.failTerminalNow(r.f)
	r.failTerminalNow(second)
	al := r.alerts()
	if len(al) != 1 || al[0].Discriminator != "create:mock" || al[0].Occurrences != 2 || al[0].State != "open" {
		t.Fatalf("expected ONE open alert with two occurrences, got %+v", al)
	}

	// A different suffix is a separate alert.
	third := seedSecondAccount(t, r.pool, r.f)
	r.w.Outbound = mismatchedKYCOutboundResolver{}
	v3, _ := requestCreate(t, r.pool, third, "mock")
	passUntilQuiet(t, r.w)
	if row := onlyRow(t, r.pool, r.f.tenantID, v3.ID, OpCreate); row.LastErrorClass != string(ClassCredentialBindingMismatch) {
		t.Fatalf("setup: %+v", row)
	}
	discs := map[string]int{}
	for _, a := range r.alerts() {
		discs[a.Discriminator] += a.Occurrences
	}
	if len(discs) != 2 || discs["create:mock"] != 2 || discs["create:mock:binding_mismatch"] != 1 {
		t.Fatalf("expected separate alerts per suffix, got %v", discs)
	}

	// Tenant B: own alert only, never A's.
	b := newRigFor(t, r)
	b.failTerminalNow(b.f)
	if got := b.alerts(); len(got) != 1 || got[0].Occurrences != 1 {
		t.Fatalf("tenant B must have exactly its own alert, got %+v", got)
	}
	if got := alertinject.Find(alertinject.ForSubject(t, r.pool, b.f.tenantID), "kyc.submission_failed_terminal"); len(got) != 1 {
		t.Fatalf("tenant B sees %d alerts", len(got))
	}
	// The subject tenant cannot ack its alert (no UPDATE path for a tenant session).
	probeTx(t, r.pool, r.f.tenantID, nil, func(ctx context.Context, tx pgx.Tx) {
		n, err := spExec(ctx, tx, `UPDATE alerts SET state = 'acked' WHERE subject_tenant_id = $1`, r.f.tenantID)
		if err == nil && n != 0 {
			t.Errorf("the subject tenant must not ack, affected %d", n)
		}
	})
}

// newRigFor builds a second rig (its own tenant) sharing r's spy-less defaults,
// WITHOUT purging r's rows.
func newRigFor(t *testing.T, base *rig) *rig {
	t.Helper()
	r := &rig{t: t, pool: base.pool, owner: base.owner, f: seedFixture(t, base.pool)}
	b := NewMockKYCProvider()
	r.createImpl = func(ctx context.Context, in CreateVerificationInput) (ProviderResult, error) {
		return b.CreateVerification(ctx, in)
	}
	r.submitImpl = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		b.mu.Lock()
		b.created[ref] = true
		b.mu.Unlock()
		return b.SubmitVerification(ctx, ref, docs, call)
	}
	r.spy = &spyKYCProvider{MockKYCProvider: b}
	r.spy.onCreateVerification = func(ctx context.Context, in CreateVerificationInput) (ProviderResult, error) {
		r.mu.Lock()
		r.createCalls++
		impl := r.createImpl
		r.mu.Unlock()
		return impl(ctx, in)
	}
	r.spy.onSubmitVerification = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		r.mu.Lock()
		r.submitCalls++
		impl := r.submitImpl
		r.mu.Unlock()
		return impl(ctx, ref, docs, call)
	}
	r.w = workerFor(base.pool, NewMockOutboundResolver(), r.spy)
	return r
}

// 48. Tenant B cannot raise the KYC Kind with tenant A as the subject (the RLS
// policy forces subject_tenant_id = the session tenant).
func TestOutboxAlert_48_TenantBCannotRaiseWithTenantAAsSubject(t *testing.T) {
	r := newRig(t)
	b := seedFixture(t, r.pool)
	probeTx(t, r.pool, b.tenantID, nil, func(ctx context.Context, tx pgx.Tx) {
		_, err := tx.Exec(ctx, `INSERT INTO alerts (kind, severity, discriminator, subject_tenant_id) VALUES ('kyc.submission_failed_terminal', 'p2', 'create:mock', $1)`, r.f.tenantID)
		if pgCode(err) != "42501" {
			t.Errorf("tenant B raising with tenant A as subject must be refused (42501), got %v", err)
		}
	})
	// And through the sanctioned path: the swallowed refusal never produces an alert for A.
	pending, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(r.pool, b.tenantID), func(ctx context.Context, tx pgx.Tx) error {
		return alerting.RaiseGuarded(ctx, tx, alerting.Alert{
			Kind: alerting.KindKYCSubmissionFailedTerminal, SubjectTenantID: r.f.tenantID, Discriminator: "create:mock",
			Attributes: map[string]alerting.AttrValue{"operation": "create", "provider_id": "mock", "last_error_class": "ambiguous", "outbox_id": uuid.NewString()},
		})
	})
	if err != nil {
		t.Fatalf("the swallowable refusal must not fail the business transaction: %v", err)
	}
	pending.Flush(context.Background())
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("tenant A must have no alert raised by tenant B, got %+v", got)
	}
}

// 49a. A swallowable SQLSTATE in the in-tx raise: failed_terminal STILL commits;
// Flush's detached retry (in the tenant scope) lands the alert.
func TestOutboxAlert_49a_SwallowedInTxRaise_TerminalStillCommits_FlushRecovers(t *testing.T) {
	r := newRig(t)
	alertinject.Install(t, r.owner, r.f.tenantID, alertinject.InTxOnly, "P0001")
	v := r.failTerminalNow(r.f)
	if row := onlyRow(t, r.pool, r.f.tenantID, v, OpCreate); row.State != OutboxFailedTerminal {
		t.Fatalf("a swallowed in-tx raise must not abort the terminal transition, got %s", row.State)
	}
	if al := r.alerts(); len(al) != 1 || al[0].Discriminator != "create:mock" {
		t.Fatalf("Flush's detached retry must land the alert, got %+v", al)
	}
	if n := auditCount(r.auditFor(v), auditActionSubmissionFailedTerminal); n != 1 {
		t.Fatalf("the terminal audit row must have committed, got %d", n)
	}
}

// 49b. 40P01 is NOT in the swallow allowlist: it propagates out of RaiseGuarded
// into the bounded phase-C retry; the terminal transition does NOT commit (the
// row stays claimed) and no alert exists.
func TestOutboxAlert_49b_DeadlockClassPropagates_NotSwallowed(t *testing.T) {
	r := newRig(t)
	alertinject.Install(t, r.owner, r.f.tenantID, alertinject.InTxOnly, "40P01")
	r.w.Config.MaxFailedAttempts = 1
	r.createImpl = func(context.Context, CreateVerificationInput) (ProviderResult, error) {
		return ProviderResult{}, errors.New("vendor down")
	}
	v := r.create()
	st := r.pass()
	if st.Results[resultPhaseCFailed] != 1 {
		t.Fatalf("expected a phase_c_failed item (40P01 propagated through the bounded retry), got %+v", st.Results)
	}
	row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
	if row.State != OutboxClaimed {
		t.Fatalf("40P01 must not be swallowed: the row must stay claimed (lease path), got %s", row.State)
	}
	if al := r.alerts(); len(al) != 0 {
		t.Fatalf("no alert may exist, got %+v", al)
	}
	if n := auditCount(r.auditFor(v.ID), auditActionSubmissionFailedTerminal); n != 0 {
		t.Fatalf("the terminal audit row must have rolled back with the transaction, got %d", n)
	}
}
