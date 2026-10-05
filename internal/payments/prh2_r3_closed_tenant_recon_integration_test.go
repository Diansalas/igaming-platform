//go:build integration

// PRH-2 R3 / H-W1 (OWNER DECISION 2026-10-05): the payment_statement stream
// OBSERVES tenants whose status is not 'active', so provider-side money
// movements cannot disappear merely because a tenant was suspended or closed,
// while financial resolution of a closed tenant's player funds stays behind the
// staff process (ADR 0101 R-5, platform_acting, four-eyes).
//
// Every test here runs in a k3World: a fully migrated PRIVATE scratch database
// whose session role is asserted neither rolsuper nor rolbypassrls (T-1,
// k3AssertUnprivilegedRole, called by newK3World), so RLS, FORCE RLS, guards
// and grants are what the application actually experiences. Providers and
// statement sources are MOCK/test-only; nothing here is a real PSP.
package payments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
	"github.com/Diansalas/igaming-platform/internal/sportsbook"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// r3Source is a tenant-aware fixed MOCK payment statement source with a fetch
// counter, an optional per-tenant failure and an optional fetch hook.
type r3Source struct {
	provider   string
	start, end time.Time

	mu      sync.Mutex
	fetches map[uuid.UUID]int
	failFor map[uuid.UUID]bool
	lines   map[uuid.UUID][]statement.PaymentStatementLine
	onFetch func(tenant uuid.UUID)
}

func newR3Source(provider string) *r3Source {
	now := time.Now().UTC()
	return &r3Source{provider: provider, start: now.Add(-time.Hour), end: now.Add(time.Minute),
		fetches: map[uuid.UUID]int{}, failFor: map[uuid.UUID]bool{}, lines: map[uuid.UUID][]statement.PaymentStatementLine{}}
}

func (s *r3Source) SyntheticComponent() {}
func (s *r3Source) Label() string       { return "MOCK r3 fixed payment statement (test-only)" }
func (s *r3Source) ProviderID() string  { return s.provider }
func (s *r3Source) Fetch(_ context.Context, req statement.PaymentFetchRequest) (statement.PaymentStatement, error) {
	s.mu.Lock()
	s.fetches[req.TenantID]++
	fail, hook := s.failFor[req.TenantID], s.onFetch
	lines := append([]statement.PaymentStatementLine(nil), s.lines[req.TenantID]...)
	s.mu.Unlock()
	if hook != nil {
		hook(req.TenantID)
	}
	if fail {
		return statement.PaymentStatement{}, errors.New("r3: scripted statement fetch failure")
	}
	return statement.PaymentStatement{CoverageStart: s.start, CoverageEnd: s.end, Lines: lines}, nil
}

func (s *r3Source) fetchCount(t uuid.UUID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetches[t]
}

// r3Sweep runs the real scoped scheduler entry point (RunSweepTenants) for ids.
func (w *k3World) r3Sweep(ids []uuid.UUID, srcs ...statement.PaymentStatementSource) []reconciliation.SweepOutcome {
	w.t.Helper()
	now := time.Now()
	outs, err := reconciliation.RunSweepTenants(context.Background(), w.pool, nil, ids, now.Add(-time.Hour), now,
		sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{}, srcs...)
	if err != nil {
		w.t.Fatalf("RunSweepTenants: %v", err)
	}
	return outs
}

func r3Outcome(t *testing.T, outs []reconciliation.SweepOutcome, tenantID uuid.UUID) reconciliation.SweepOutcome {
	t.Helper()
	var found []reconciliation.SweepOutcome
	for _, o := range outs {
		if o.TenantID == tenantID {
			found = append(found, o)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one SweepOutcome for tenant %s, got %d", tenantID, len(found))
	}
	return found[0]
}

type r3Finding struct{ Kind, Key string }

// r3Findings reads the persisted findings of one run in the tenant's own scope.
func (w *k3World) r3Findings(runID uuid.UUID) []r3Finding {
	w.t.Helper()
	var out []r3Finding
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT mismatch_kind, reconciliation_key FROM reconciliation_mismatches
			WHERE tenant_id = $1 AND reconciliation_run_id = $2 ORDER BY mismatch_kind, reconciliation_key`, w.f.tenantID, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f r3Finding
			if err := rows.Scan(&f.Kind, &f.Key); err != nil {
				return err
			}
			out = append(out, f)
		}
		return rows.Err()
	})
	return out
}

func r3Has(fs []r3Finding, kind reconciliation.MismatchKind, attempt uuid.UUID) bool {
	for _, f := range fs {
		if f.Kind == string(kind) && strings.Contains(f.Key, "attempt="+attempt.String()) {
			return true
		}
	}
	return false
}

func r3Render(fs []r3Finding) string {
	var b strings.Builder
	for _, f := range fs {
		fmt.Fprintf(&b, "  %s | %s\n", f.Kind, f.Key)
	}
	return b.String()
}

// r3PaymentOutcome returns the (single) payment_statement outcome and fails the
// test if it errored.
func r3PaymentOutcome(t *testing.T, o reconciliation.SweepOutcome) reconciliation.StreamOutcome {
	t.Helper()
	if len(o.PaymentStatement) != 1 {
		t.Fatalf("want one payment_statement outcome, got %d", len(o.PaymentStatement))
	}
	ps := o.PaymentStatement[0]
	if ps.Err != nil {
		t.Fatalf("payment_statement run failed: %v", ps.Err)
	}
	return ps
}

// r3Snap is everything a money-affecting path could change, per tenant.
type r3Snap struct {
	ledgerTx, ledgerEntries, resolutions, resolutionApprovals, events int
	debits, credits, projDebit, projCredit                            int64
	attempts, intents, withdrawals, tenantStatus                      string
	providerCalls                                                     int64
}

func (w *k3World) r3Snapshot() r3Snap {
	w.t.Helper()
	var s r3Snap
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
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
			{`SELECT COALESCE(string_agg(id::text || state || COALESCE(terminal_reason, '') || updated_at::text || COALESCE(next_action_at::text, '') || poll_count::text, ',' ORDER BY id), '') FROM payment_attempts WHERE tenant_id = $1`, &s.attempts},
			{`SELECT COALESCE(string_agg(id::text || status || updated_at::text, ',' ORDER BY id), '') FROM deposit_intents WHERE tenant_id = $1`, &s.intents},
			{`SELECT COALESCE(string_agg(id::text || state || updated_at::text, ',' ORDER BY id), '') FROM withdrawal_requests WHERE tenant_id = $1`, &s.withdrawals},
			{`SELECT count(*) FROM payment_provider_events WHERE tenant_id = $1`, &s.events},
		}
		for _, q := range qs {
			if err := tx.QueryRow(ctx, q.sql, w.f.tenantID).Scan(q.dst); err != nil {
				return fmt.Errorf("%s: %w", q.sql, err)
			}
		}
		return nil
	})
	// Manual resolutions are invisible to the system session shape (RLS), so they
	// are counted two ways: every resolution mutation writes a
	// payment.manual_resolution_* audit record (tenant-visible, covers
	// platform_acting rows too), and the tenant-scope rows are counted as a
	// tenant principal. TestR3_Controls proves both counters move.
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action LIKE 'payment.manual_resolution%'`, w.f.tenantID).Scan(&s.resolutions)
	})
	if err := w.pool.WithPrincipalScope(context.Background(), w.f.tenantID, w.f1.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_manual_resolutions WHERE tenant_id = $1`, w.f.tenantID).Scan(&s.resolutionApprovals)
	}); err != nil {
		w.t.Fatalf("count resolutions: %v", err)
	}
	if err := w.pool.WithPlatformAdmin(context.Background(), w.adminA.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1`, w.f.tenantID).Scan(&s.tenantStatus)
	}); err != nil {
		w.t.Fatalf("read tenant status: %v", err)
	}
	s.providerCalls = w.prov.callTotal() + int64(w.mock.AttemptCount())
	return s
}

// r3RequireNoMoneyEffect asserts nothing but findings/runs/imports/audit/alerts
// changed: the ledger, projections, attempts, intents, withdrawals, receipts,
// manual resolutions, the tenant's status and the provider call counters.
func r3RequireNoMoneyEffect(t *testing.T, before, after r3Snap) {
	t.Helper()
	if before != after {
		t.Fatalf("the observation sweep changed money-affecting state:\nbefore=%+v\nafter =%+v", before, after)
	}
	if after.debits != after.credits {
		t.Fatalf("SUM(debits) != SUM(credits): %d vs %d", after.debits, after.credits)
	}
}

func (w *k3World) r3Runs() int {
	return w.countRows(`SELECT count(*) FROM reconciliation_runs WHERE tenant_id = $1 AND stream = 'payment_statement'`, w.f.tenantID)
}

func (w *k3World) r3Imports() int {
	return w.countRows(`SELECT count(*) FROM payment_statement_imports WHERE tenant_id = $1`, w.f.tenantID)
}

// r3AuditSweepRun returns the audit metadata of the sweep_run record of runID.
func (w *k3World) r3AuditSweepRun(runID uuid.UUID) map[string]any {
	w.t.Helper()
	var meta map[string]any
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT metadata FROM audit_log WHERE tenant_id = $1 AND action = 'reconciliation.sweep_run' AND target_id = $2`,
			w.f.tenantID, runID.String()).Scan(&meta)
	})
	return meta
}

func (w *k3World) r3Alerts(kind alerting.Kind) int {
	w.t.Helper()
	var n int
	if err := w.pool.WithPlatformAdmin(context.Background(), w.adminA.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = $1 AND (tenant_id = $2 OR subject_tenant_id = $2)`, string(kind), w.f.tenantID).Scan(&n)
	}); err != nil {
		w.t.Fatalf("count alerts: %v", err)
	}
	return n
}

// ---------------------------------------------------------------------------

// R3-0 vacuity controls: the role is unprivileged, and the resolution counter
// really sees a resolution (otherwise "no resolution created" would be vacuous).
func TestR3_Controls_RoleAndCounters(t *testing.T) {
	w := newK3World(t, k3Opts{base: 2})
	k3AssertUnprivilegedRole(t, w.pool, w.f.tenantID)
	_, a := w.ambiguousPayout(100)
	before := w.r3Snapshot()
	w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
	after := w.r3Snapshot()
	if after.resolutions != before.resolutions+1 || after.resolutionApprovals != before.resolutionApprovals+1 {
		t.Fatalf("control: the resolution counters did not see a new request (%+v -> %+v)", before, after)
	}
	// And the call counter sees a provider call.
	calls := w.r3Snapshot().providerCalls
	if _, err := w.prov.QueryStatus(context.Background(), *a.ProviderReference); err != nil {
		t.Fatal(err)
	}
	if got := w.r3Snapshot().providerCalls; got != calls+1 {
		t.Fatalf("control: the provider call counter did not move (%d -> %d)", calls, got)
	}
}

// R3-1: a parked deposit capture gets its standing pay_captured_unposted in the
// observation sweep of a suspended and of a closed tenant; an active tenant is
// swept by the full stream set exactly as before and carries no observation flag.
func TestR3_ParkedDepositCapture_StandingFindingForNonActiveTenants(t *testing.T) {
	for _, status := range []string{"active", "suspended", "closed"} {
		t.Run(status, func(t *testing.T) {
			w := newK3World(t, k3Opts{base: 1})
			a := w.disputedDeposit(500)
			if status != "active" {
				w.setTenantStatus(status)
			}
			src := newR3Source(w.provider)
			before := w.r3Snapshot()
			outs := w.r3Sweep([]uuid.UUID{w.f.tenantID}, src)
			o := r3Outcome(t, outs, w.f.tenantID)
			ps := r3PaymentOutcome(t, o)
			fs := w.r3Findings(ps.Run.ID)
			if !r3Has(fs, reconciliation.MismatchKindPayCapturedUnposted, a.ID) {
				t.Fatalf("%s tenant: no standing pay_captured_unposted for the parked capture:\n%s", status, r3Render(fs))
			}
			if ps.Run.Status != reconciliation.StatusMismatchesFound {
				t.Fatalf("run status %s", ps.Run.Status)
			}
			meta := w.r3AuditSweepRun(ps.Run.ID)
			if status == "active" {
				if o.ObservationOnly || o.TenantStatus != "" {
					t.Fatalf("an active tenant must not be flagged observation-only: %+v", o)
				}
				if _, flagged := meta["non_active_tenant_observation"]; flagged {
					t.Fatalf("an active tenant's run audit carries the observation flag: %v", meta)
				}
				if o.Run.ID == uuid.Nil || o.Sportsbook.Run.ID == uuid.Nil {
					t.Fatal("an active tenant must still get the full stream set")
				}
			} else {
				if !o.ObservationOnly || o.TenantStatus != status {
					t.Fatalf("outcome not marked observation-only/%s: %+v", status, o)
				}
				if meta["non_active_tenant_observation"] != true || meta["tenant_status"] != status {
					t.Fatalf("run audit lacks the observation flag/status: %v", meta)
				}
				// ONLY payment_statement ran: no other stream's run exists.
				if n := w.countRows(`SELECT count(*) FROM reconciliation_runs WHERE tenant_id = $1 AND stream <> 'payment_statement'`, w.f.tenantID); n != 0 {
					t.Fatalf("the observation sweep ran %d non-payment_statement run(s)", n)
				}
				if o.Run.ID != uuid.Nil || o.Sportsbook.Run.ID != uuid.Nil || o.Casino.Run.ID != uuid.Nil || o.CasinoStatement.Run.ID != uuid.Nil {
					t.Fatalf("a non-payment stream ran for a non-active tenant: %+v", o)
				}
				r3RequireNoMoneyEffect(t, before, w.r3Snapshot())
				// The finding raised a durable alert for the (non-active) tenant.
				if n := w.r3Alerts(alerting.KindReconciliationPaymentStatement); n == 0 {
					t.Fatal("no payment_statement mismatch alert was raised for the non-active tenant")
				}
			}
			w.assertInvariants()
		})
	}
}

// R3-2: a closed tenant with a declared-paid payout gets
// pay_declared_paid_unconfirmed (standing, no confirming line), then clears
// when a confirming statement line arrives - all via observation, no resolution.
func TestR3_ClosedTenant_DeclaredPaidPayout_Unconfirmed(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(500)
	w.executeM2(a.ID, ResolutionM2DeclarePaid) // while the tenant is still open
	w.setTenantStatus("closed")
	src := newR3Source(w.provider)

	before := w.r3Snapshot()
	ps := r3PaymentOutcome(t, r3Outcome(t, w.r3Sweep([]uuid.UUID{w.f.tenantID}, src), w.f.tenantID))
	fs := w.r3Findings(ps.Run.ID)
	if !r3Has(fs, reconciliation.MismatchKindPayDeclaredPaidUnconfirmed, a.ID) {
		t.Fatalf("no pay_declared_paid_unconfirmed for the closed tenant's declared-paid payout:\n%s", r3Render(fs))
	}
	r3RequireNoMoneyEffect(t, before, w.r3Snapshot())

	// A confirming statement line clears it, again with no money effect.
	src.mu.Lock()
	src.lines[w.f.tenantID] = []statement.PaymentStatementLine{
		w.payoutLine(*a.ProviderReference, a.MerchantReference, statement.PaymentStatusSucceeded, 500)}
	src.mu.Unlock()
	before = w.r3Snapshot()
	ps = r3PaymentOutcome(t, r3Outcome(t, w.r3Sweep([]uuid.UUID{w.f.tenantID}, src), w.f.tenantID))
	if fs := w.r3Findings(ps.Run.ID); r3Has(fs, reconciliation.MismatchKindPayDeclaredPaidUnconfirmed, a.ID) {
		t.Fatalf("a confirming line did not clear the finding:\n%s", r3Render(fs))
	}
	r3RequireNoMoneyEffect(t, before, w.r3Snapshot())
	w.assertInvariants()
}

// R3-3: the observation sweep of a closed tenant with in-flight, ambiguous and
// disputed work writes NO ledger transaction/entry, changes NO attempt,
// withdrawal, intent or receipt, makes NO provider call, creates NO manual
// resolution - including with the REAL mock statement source (which reads the
// MockProvider's own records and must not dispatch).
func TestR3_ClosedTenantSweep_NoMoneyEffect_NoProviderCall_NoResolution(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, pending := w.payout(100)
	_, ambiguous := w.ambiguousPayout(200)
	_, disputed := w.disputedPayout(300, "provider_reference_mismatch")
	dep := w.disputedDeposit(400)
	w.setTenantStatus("closed")

	realMock := NewMockStatementSource(w.mock, MockCredentialResolver{})
	for _, src := range []statement.PaymentStatementSource{realMock, newR3Source(w.provider)} {
		before := w.r3Snapshot()
		outs := w.r3Sweep([]uuid.UUID{w.f.tenantID}, src)
		ps := r3PaymentOutcome(t, r3Outcome(t, outs, w.f.tenantID))
		r3RequireNoMoneyEffect(t, before, w.r3Snapshot())
		if ps.Run.ID == uuid.Nil {
			t.Fatal("no run recorded")
		}
	}
	// The states are exactly as the writers left them.
	for id, want := range map[uuid.UUID]AttemptState{pending.ID: AttemptPending, ambiguous.ID: AttemptAmbiguous, disputed.ID: AttemptDisputed, dep.ID: AttemptDisputed} {
		if got := w.attempt(id); got.State != want {
			t.Fatalf("attempt %s state %s, want %s", id, got.State, want)
		}
	}
	w.assertInvariants()
}

// R3-4: M2 on the closed tenant is gated exactly as before the observation
// existed: a tenant-scope requester is still refused (MR030), a platform_acting
// resolution still needs its full four-eyes count, and a sweep creates or
// executes none. The same outcomes hold with and without a prior sweep.
func TestR3_ClosedTenant_M2StaysGatedByTheStaffPath(t *testing.T) {
	for _, swept := range []bool{false, true} {
		t.Run(fmt.Sprintf("swept=%v", swept), func(t *testing.T) {
			w := newK3World(t, k3Opts{base: 2})
			wr, a := w.ambiguousPayout(100)
			w.setTenantStatus("closed")
			if swept {
				src := newR3Source(w.provider)
				w.r3Sweep([]uuid.UUID{w.f.tenantID}, src)
				if got := w.r3Snapshot().resolutions; got != 0 {
					t.Fatalf("the sweep created %d manual resolution(s)", got)
				}
			}
			// Tenant-scope requester: refused at insert.
			_, err := w.request(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
			k3RequireCode(t, err, "MR030")
			// platform_acting requester + ONE acting approval of a required TWO:
			// not executed (four-eyes holds), nothing released.
			r := w.mustRequest(w.acting, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
			out, err := w.decide(w.acting2, r, ResolutionApprove)
			if err != nil || out.Executed || out.Resolution.State != ResolutionPending {
				t.Fatalf("a single approval executed a base-2 closed-tenant M2: %v %+v", err, out)
			}
			if got := w.attempt(a.ID); got.State != AttemptAmbiguous {
				t.Fatalf("attempt moved to %s", got.State)
			}
			if w.withdrawalOf(wr.ID).State != withdrawal.StateSubmitted {
				t.Fatal("withdrawal moved")
			}
			// A sweep AFTER the pending staff resolution neither executes nor
			// approves it.
			w.r3Sweep([]uuid.UUID{w.f.tenantID}, newR3Source(w.provider))
			if got := w.resolution(r.ID); got.State != ResolutionPending {
				t.Fatalf("a sweep changed the pending resolution to %s", got.State)
			}
			w.assertInvariants()
		})
	}
}

// R3-5: cross-tenant isolation. Closed tenant A's runs/findings are never
// visible to tenant B, and A's persisted statement lines cannot clear B's
// standing finding even when A's import (same provider id) names B's captured
// reference as a reversal original.
func TestR3_CrossTenantIsolation(t *testing.T) {
	a := newK3World(t, k3Opts{base: 1})
	b := newK3WorldOn(t, a.pool, k3Opts{base: 1})
	depB := b.disputedDeposit(500)
	depA := a.disputedDeposit(500)
	a.setTenantStatus("closed")

	// Tenant A (closed) ingests a reversal line under B's provider id naming B's
	// captured reference; tenant B (active) carries no line. One source per
	// provider; every tenant is swept against both.
	srcA, srcB := newR3Source(a.provider), newR3Source(b.provider)
	srcB.lines[a.f.tenantID] = []statement.PaymentStatementLine{{
		ProviderID: b.provider, ProviderReference: "r3-rev-" + uuid.NewString(), Kind: statement.PaymentLineDepositReversal,
		OriginalProviderReference: *depB.ProviderReference, Status: statement.PaymentStatusSucceeded, Amount: 500, AssetCode: "EUR",
		OccurredAt: time.Now().UTC()}}
	outs := a.r3Sweep([]uuid.UUID{a.f.tenantID, b.f.tenantID}, srcA, srcB)
	oa, ob := r3Outcome(t, outs, a.f.tenantID), r3Outcome(t, outs, b.f.tenantID)
	if !oa.ObservationOnly || ob.ObservationOnly {
		t.Fatalf("observation flags wrong: A=%v B=%v", oa.ObservationOnly, ob.ObservationOnly)
	}
	runA := r3PaymentOutcome(t, singleSource(t, oa, 0)).Run // A's own provider
	runB := r3PaymentOutcome(t, singleSource(t, ob, 1)).Run // B's provider, B's view

	// B still reports its own standing finding (A's line did not clear it).
	if fs := b.r3Findings(runB.ID); !r3Has(fs, reconciliation.MismatchKindPayCapturedUnposted, depB.ID) {
		t.Fatalf("tenant A's line cleared tenant B's finding:\n%s", r3Render(fs))
	}
	// A's findings are A's own and invisible from B's scope (RLS), and vice versa.
	if fs := a.r3Findings(runA.ID); !r3Has(fs, reconciliation.MismatchKindPayCapturedUnposted, depA.ID) {
		t.Fatalf("closed tenant A lacks its own finding:\n%s", r3Render(fs))
	}
	for _, c := range []struct {
		name   string
		viewer *k3World
		run    uuid.UUID
	}{{"B sees A's run", b, runA.ID}, {"A sees B's run", a, runB.ID}} {
		var runs, mms, audit int
		if err := c.viewer.pool.WithTenant(context.Background(), c.viewer.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_runs WHERE id = $1`, c.run).Scan(&runs); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_mismatches WHERE reconciliation_run_id = $1`, c.run).Scan(&mms); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE target_id = $1`, c.run.String()).Scan(&audit)
		}); err != nil {
			t.Fatal(err)
		}
		if runs+mms+audit != 0 {
			t.Fatalf("%s: runs=%d mismatches=%d audit=%d", c.name, runs, mms, audit)
		}
	}
	// A's statement lines and imports are invisible to B.
	var aLinesSeenByB int
	if err := b.pool.WithTenant(context.Background(), b.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_statement_lines WHERE tenant_id = $1`, a.f.tenantID).Scan(&aLinesSeenByB)
	}); err != nil {
		t.Fatal(err)
	}
	if aLinesSeenByB != 0 {
		t.Fatalf("tenant B read %d of closed tenant A's statement lines", aLinesSeenByB)
	}
}

// R3-6: re-running the observation of a closed tenant is idempotent: the same
// statement is stored once (import reused), every run reports the same
// findings, and nothing money-affecting changes.
func TestR3_ClosedTenantSweep_IdempotentRerun(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	w.disputedDeposit(500)
	w.setTenantStatus("closed")
	src := newR3Source(w.provider)
	before := w.r3Snapshot()
	var keys [][]r3Finding
	for i := 0; i < 3; i++ {
		ps := r3PaymentOutcome(t, r3Outcome(t, w.r3Sweep([]uuid.UUID{w.f.tenantID}, src), w.f.tenantID))
		keys = append(keys, w.r3Findings(ps.Run.ID))
	}
	for i := 1; i < len(keys); i++ {
		if fmt.Sprint(keys[i]) != fmt.Sprint(keys[0]) {
			t.Fatalf("run %d findings differ from run 0:\n%s\nvs\n%s", i, r3Render(keys[i]), r3Render(keys[0]))
		}
	}
	if len(keys[0]) == 0 {
		t.Fatal("setup: no findings to compare")
	}
	if n := w.r3Imports(); n != 1 {
		t.Fatalf("the identical statement was stored %d times", n)
	}
	if n := w.r3Runs(); n != 3 {
		t.Fatalf("want 3 payment_statement runs, got %d", n)
	}
	r3RequireNoMoneyEffect(t, before, w.r3Snapshot())
}

// R3-7: failure isolation. One non-active tenant's failing statement fetch is a
// recorded, audited, alerted P1 for that tenant only; the other tenants
// (non-active AND active) are still observed.
func TestR3_FailureOfOneTenantDoesNotStopOthers(t *testing.T) {
	a := newK3World(t, k3Opts{base: 1})
	b := newK3WorldOn(t, a.pool, k3Opts{base: 1})
	c := newK3WorldOn(t, a.pool, k3Opts{base: 1})
	// The observation order is by tenant id: make the failing tenant A the FIRST
	// closed tenant, so "a failure stops the loop" is detectable deterministically.
	if b.f.tenantID.String() < a.f.tenantID.String() {
		a, b = b, a
	}
	depB := b.disputedDeposit(500)
	depC := c.disputedDeposit(500)
	a.setTenantStatus("closed")
	b.setTenantStatus("closed")
	// c stays active.
	src := newR3Source(b.provider)
	src.failFor[a.f.tenantID] = true
	// c's parked capture is under c's provider; use a source per provider so each
	// tenant's provider has a source (a source is per provider).
	srcC := newR3Source(c.provider)
	ids := []uuid.UUID{a.f.tenantID, b.f.tenantID, c.f.tenantID}
	// Order the sources so the failing tenant A is not first or last by chance.
	outs := a.r3Sweep(ids, src, srcC)

	oa := r3Outcome(t, outs, a.f.tenantID)
	if !oa.ObservationOnly || len(oa.PaymentStatement) != 2 || oa.PaymentStatement[0].Err == nil {
		t.Fatalf("closed tenant A's scripted fetch failure was not recorded on its outcome: %+v", oa)
	}
	if got := a.countRows(`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'reconciliation.sweep_run_failed'`, a.f.tenantID); got == 0 {
		t.Fatal("no sweep_run_failed audit for the failed closed tenant")
	}
	var failedMeta map[string]any
	a.tx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT metadata FROM audit_log WHERE tenant_id = $1 AND action = 'reconciliation.sweep_run_failed' ORDER BY created_at DESC LIMIT 1`, a.f.tenantID).Scan(&failedMeta)
	})
	if failedMeta["non_active_tenant_observation"] != true || failedMeta["tenant_status"] != "closed" {
		t.Fatalf("the failure audit lacks the observation flag: %v", failedMeta)
	}
	if n := a.r3Alerts(alerting.KindReconciliationRunFailed); n == 0 {
		t.Fatal("no reconciliation.run_failed alert for the failed closed tenant (platform scope)")
	}
	if src.fetchCount(a.f.tenantID) != 1 || src.fetchCount(b.f.tenantID) != 1 {
		t.Fatalf("each closed tenant must be fetched exactly once per sweep, got A=%d B=%d", src.fetchCount(a.f.tenantID), src.fetchCount(b.f.tenantID))
	}
	// B (closed) and C (active) were still processed.
	psB := r3PaymentOutcome(t, singleSource(t, r3Outcome(t, outs, b.f.tenantID), 0))
	if !r3Has(b.r3Findings(psB.Run.ID), reconciliation.MismatchKindPayCapturedUnposted, depB.ID) {
		t.Fatal("closed tenant B was not observed after A failed")
	}
	psC := r3PaymentOutcome(t, singleSource(t, r3Outcome(t, outs, c.f.tenantID), 1))
	if !r3Has(c.r3Findings(psC.Run.ID), reconciliation.MismatchKindPayCapturedUnposted, depC.ID) {
		t.Fatal("active tenant C was not reconciled after A failed")
	}
}

// singleSource narrows an outcome with several sources to the idx-th source's
// result, as a one-element outcome.
func singleSource(t *testing.T, o reconciliation.SweepOutcome, idx int) reconciliation.SweepOutcome {
	t.Helper()
	if idx >= len(o.PaymentStatement) {
		t.Fatalf("outcome has %d payment results, want index %d", len(o.PaymentStatement), idx)
	}
	o.PaymentStatement = []reconciliation.StreamOutcome{o.PaymentStatement[idx]}
	return o
}

// R3-8: concurrency. (a) a held per-tenant payment_statement advisory lock makes
// the observation run skip (recorded, no run row, not an error); (b) two
// scheduler instances sweeping the same closed tenant concurrently both
// complete without error and without any money effect.
func TestR3_Concurrency_TwoSchedulerInstances(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	w.disputedDeposit(500)
	w.setTenantStatus("closed")
	src := newR3Source(w.provider)
	ctx := context.Background()

	// (a) another instance holds the tenant's lock while this one matches.
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- w.pool.WithTenant(ctx, w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('reconciliation:payment_statement:' || $1::text, 0))`, w.f.tenantID).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return errors.New("holder could not take the lock")
			}
			close(held)
			<-release
			return nil
		})
	}()
	select {
	case <-held:
	case err := <-done:
		t.Fatalf("lock holder: %v", err)
	}
	before := w.r3Snapshot()
	o := r3Outcome(t, w.r3Sweep([]uuid.UUID{w.f.tenantID}, src), w.f.tenantID)
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("lock holder: %v", err)
	}
	if len(o.PaymentStatement) != 1 || o.PaymentStatement[0].Err != nil || !o.PaymentStatement[0].Skipped {
		t.Fatalf("a lock-contended observation must be a clean skip, got %+v", o.PaymentStatement)
	}
	if n := w.r3Runs(); n != 0 {
		t.Fatalf("a skipped observation recorded %d run(s)", n)
	}
	r3RequireNoMoneyEffect(t, before, w.r3Snapshot())

	// (b) two instances at once.
	var wg sync.WaitGroup
	results := make([]reconciliation.SweepOutcome, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			now := time.Now()
			outs, err := reconciliation.RunSweepTenants(ctx, w.pool, nil, []uuid.UUID{w.f.tenantID}, now.Add(-time.Hour), now,
				sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{}, src)
			if err != nil || len(outs) != 1 {
				t.Errorf("instance %d: %v %d", i, err, len(outs))
				return
			}
			results[i] = outs[0]
		}(i)
	}
	wg.Wait()
	ran := 0
	for i, r := range results {
		if len(r.PaymentStatement) != 1 || r.PaymentStatement[0].Err != nil {
			t.Fatalf("instance %d failed: %+v", i, r.PaymentStatement)
		}
		if !r.PaymentStatement[0].Skipped {
			ran++
		}
	}
	if ran < 1 || w.r3Runs() != ran {
		t.Fatalf("ran=%d, runs recorded=%d", ran, w.r3Runs())
	}
	r3RequireNoMoneyEffect(t, before, w.r3Snapshot())
}

// R3-9: the tenant status changes between runs and mid-run. active -> closed ->
// active again: the finding continues without a gap and the audit flag follows
// the status at selection; a tenant closed DURING the fetch phase of an
// ordinary run still completes it and nothing breaks.
func TestR3_TenantStatusChangesBetweenAndDuringRuns(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	a := w.disputedDeposit(500)
	src := newR3Source(w.provider)
	ids := []uuid.UUID{w.f.tenantID}

	check := func(label string, wantObs bool, wantStatus string) {
		t.Helper()
		o := r3Outcome(t, w.r3Sweep(ids, src), w.f.tenantID)
		ps := r3PaymentOutcome(t, o)
		if !r3Has(w.r3Findings(ps.Run.ID), reconciliation.MismatchKindPayCapturedUnposted, a.ID) {
			t.Fatalf("%s: the standing finding is missing", label)
		}
		if o.ObservationOnly != wantObs || o.TenantStatus != wantStatus {
			t.Fatalf("%s: observation=%v status=%q, want %v/%q", label, o.ObservationOnly, o.TenantStatus, wantObs, wantStatus)
		}
		meta := w.r3AuditSweepRun(ps.Run.ID)
		if (meta["non_active_tenant_observation"] == true) != wantObs {
			t.Fatalf("%s: audit flag %v, want %v", label, meta["non_active_tenant_observation"], wantObs)
		}
	}
	check("active", false, "")
	w.setTenantStatus("closed")
	check("closed", true, "closed")
	w.setTenantStatus("suspended")
	check("suspended", true, "suspended")
	w.setTenantStatus("active")
	check("reactivated", false, "")

	// Closed DURING the fetch phase of an ordinary (active-listed) run.
	src2 := newR3Source(w.provider)
	src2.onFetch = func(uuid.UUID) { w.setTenantStatus("closed") }
	o := r3Outcome(t, w.r3Sweep(ids, src2), w.f.tenantID)
	if o.ObservationOnly {
		t.Fatal("selected active, must not be flagged observation")
	}
	ps := r3PaymentOutcome(t, o)
	if !r3Has(w.r3Findings(ps.Run.ID), reconciliation.MismatchKindPayCapturedUnposted, a.ID) {
		t.Fatal("a tenant closed mid-run lost its finding")
	}
	w.assertInvariants()
}

// R3-11: the UNSCOPED entry point (RunSweep, what RunSchedulerLoop runs) observes
// every non-active tenant of the database, and a SCOPED sweep observes only the
// non-active tenants it names (an unnamed closed tenant is untouched).
func TestR3_UnscopedAndScopedSelection(t *testing.T) {
	a := newK3World(t, k3Opts{base: 1})
	b := newK3WorldOn(t, a.pool, k3Opts{base: 1})
	a.disputedDeposit(500)
	b.disputedDeposit(500)
	a.setTenantStatus("closed")
	b.setTenantStatus("suspended")
	srcA, srcB := newR3Source(a.provider), newR3Source(b.provider)

	// Scoped to A only: B is not observed at all.
	a.r3Sweep([]uuid.UUID{a.f.tenantID}, srcA, srcB)
	if n := b.countRows(`SELECT count(*) FROM reconciliation_runs WHERE tenant_id = $1`, b.f.tenantID); n != 0 {
		t.Fatalf("a scoped sweep observed the unnamed tenant B (%d runs)", n)
	}
	if srcA.fetchCount(b.f.tenantID)+srcB.fetchCount(b.f.tenantID) != 0 {
		t.Fatal("a scoped sweep fetched a statement for the unnamed tenant B")
	}
	if n := a.r3Runs(); n != 2 {
		t.Fatalf("scoped tenant A: want 2 payment_statement runs (one per source), got %d", n)
	}

	// Unscoped: both are observed, with their own statuses.
	now := time.Now()
	outs, err := reconciliation.RunSweep(context.Background(), a.pool, nil, now.Add(-time.Hour), now,
		sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{}, srcA, srcB)
	if err != nil {
		t.Fatalf("RunSweep: %v", err)
	}
	oa, ob := r3Outcome(t, outs, a.f.tenantID), r3Outcome(t, outs, b.f.tenantID)
	if !oa.ObservationOnly || oa.TenantStatus != "closed" || !ob.ObservationOnly || ob.TenantStatus != "suspended" {
		t.Fatalf("unscoped observation outcomes wrong: A=%+v B=%+v", oa, ob)
	}
	if b.countRows(`SELECT count(*) FROM reconciliation_runs WHERE tenant_id = $1 AND stream = 'payment_statement'`, b.f.tenantID) != 2 {
		t.Fatal("the unscoped sweep did not observe suspended tenant B")
	}
}

// R3-10: no source registered means nothing to observe; a non-active tenant is
// then not swept at all (the pre-existing behaviour for the other streams is
// unchanged).
func TestR3_NoSource_NonActiveTenantIsNotSwept(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	w.setTenantStatus("closed")
	if outs := w.r3Sweep([]uuid.UUID{w.f.tenantID}); len(outs) != 0 {
		t.Fatalf("a closed tenant with no payment source produced %d outcome(s)", len(outs))
	}
	if n := w.countRows(`SELECT count(*) FROM reconciliation_runs WHERE tenant_id = $1`, w.f.tenantID); n != 0 {
		t.Fatalf("%d run(s) for a closed tenant with no source", n)
	}
}
