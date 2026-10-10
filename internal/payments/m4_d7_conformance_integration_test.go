//go:build integration

// D-7 CONFORMANCE SUITE (owner decision 6 of ADR 0095 section 48; ADR 0111 section 25).
//
// One test per D-7 property. Each proves, against real PostgreSQL in the same runtime /
// acting session shapes as the M4 world fixtures (newM4World), that a wrong value in THAT
// property alone makes the verdict neither paid nor not_paid (insufficient / contradictory)
// - or, where the database's verdict is positive by design, that the Go-side eligibility
// refusal ends it - and that the M4 is refused at the request and at the execution, with
// the park (hold) kept and no posting. The meta tests then prove that no single
// provider-controlled field is sufficient and that ambiguous evidence parks.
//
// The matrix (property -> enforcement -> re-check at execution -> test) is ADR 0111
// section 25. MOCK statement sources only; no real provider is connected.
package payments

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// d7NonPositive asserts the DB verdict of the park is one of want (never paid / not_paid)
// and carries no line or reference.
func (m *m4World) d7NonPositive(p *b11Parked, want ...string) M4Evidence {
	m.t.Helper()
	ev, err := m.evidence(p.fresh.ID)
	if err != nil {
		m.t.Fatalf("evidence: %v", err)
	}
	ok := false
	for _, w := range want {
		ok = ok || ev.Verdict == w
	}
	if !ok || ev.LineID != nil || ev.Reference != nil {
		m.t.Fatalf("want a non-positive verdict in %v without a line or reference, got %+v", want, ev)
	}
	return ev
}

// d7BothKindsRefusedAtRequest: neither M4 kind can be requested (MR062: the verdict is not positive),
// and the park keeps its hold with nothing posted and no resolution row.
func (m *m4World) d7BothKindsRefusedAtRequest(p *b11Parked) {
	m.t.Helper()
	anyID := uuid.New()
	for _, k := range []ResolutionKind{ResolutionM4EvidencePaid, ResolutionM4EvidenceNotPaid} {
		_, err := m.request(m.acting, m.m4In(p.fresh.ID, k, &anyID))
		k3RequireCode(t0(m), err, "MR062")
		if ResolutionToken(ClassifyResolutionError(err)) != TokenForceResolveEvidenceInsufficient {
			m.t.Fatalf("%s: token %s", k, ResolutionToken(ClassifyResolutionError(err)))
		}
	}
	if n := m.countRows(`SELECT count(*) FROM payment_manual_resolutions WHERE tenant_id = $1 AND attempt_id = $2`, m.f.tenantID, p.fresh.ID); n != 0 {
		m.t.Fatalf("a refused request left %d resolution row(s)", n)
	}
	m.d7Held(p, "refused M4 request")
}

func t0(m *m4World) *testing.T { return m.t }

// d7Held is b11Held for a world that holds several parks: the b11 snapshot is wallet-wide, so it
// cannot be compared once a second park exists. It asserts the park's own hold: the withdrawal
// is still submitted with no release, the attempt is still disputed, no withdrawal posting names
// the withdrawal, and the ledger invariants hold.
func (m *m4World) d7Held(p *b11Parked, what string) {
	m.t.Helper()
	wr := m.wd(p.wr.ID)
	if wr.State != withdrawal.StateSubmitted || wr.ReleaseLedgerTransactionID != nil {
		m.t.Fatalf("%s: the withdrawal was released: %+v", what, wr)
	}
	if a := m.attempt(p.fresh.ID); a.State != AttemptDisputed {
		m.t.Fatalf("%s: the attempt left disputed: %s", what, a.State)
	}
	if n := m.countRows(`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND correlation_id = $2
		AND transaction_type IN ('withdrawal_completed', 'withdrawal_failed', 'withdrawal_rejected', 'withdrawal_reversed')`, m.f.tenantID, p.wr.ID); n != 0 {
		m.t.Fatalf("%s: %d withdrawal posting(s) name the withdrawal", what, n)
	}
	m.assertInvariants()
}

// d7RefusedAtExecution requests a positive M4 (it must be accepted), applies corrupt() (a change in ONE
// property that lands after the request), then makes the final approval. The execution must not
// post: either Go ends it refused_at_execution, or the database's `-> executing` re-check raises
// (MR061/MR010/MR012). Returns the refusal (Go code or SQLSTATE).
func (m *m4World) d7RefusedAtExecution(p *b11Parked, kind ResolutionKind, ev M4Evidence, corrupt func()) string {
	m.t.Helper()
	postedBefore := m.ledgerTxCount("withdrawal_completed") + m.ledgerTxCount("withdrawal_failed")
	r, err := m.request(m.acting, m.m4In(p.fresh.ID, kind, ev.LineID))
	k3RequireNoErr(m.t, err, "request (the positive control)")
	corrupt()
	out, err := m.decide(m.acting2, r, ResolutionApprove)
	got := ""
	switch {
	case err != nil:
		got = k3Code(err)
		if got != "MR061" && got != "MR010" && got != "MR012" {
			m.t.Fatalf("want a refusal at execution, got error %v", err)
		}
	case out.Executed || !out.Refused || out.Resolution.RefusalCode == nil:
		m.t.Fatalf("want refused_at_execution, got %+v", out)
	default:
		got = *out.Resolution.RefusalCode
	}
	if n := m.ledgerTxCount("withdrawal_completed") + m.ledgerTxCount("withdrawal_failed"); n != postedBefore {
		m.t.Fatalf("a refused execution posted %d withdrawal transaction(s)", n-postedBefore)
	}
	if wr := m.wd(p.wr.ID); wr.State != withdrawal.StateSubmitted || wr.ReleaseLedgerTransactionID != nil {
		m.t.Fatalf("a refused execution released the park: %+v", wr)
	}
	m.assertInvariants()
	return got
}

// d7ReqNotEligible asserts the request is refused by the Go-side eligibility check although the
// database's verdict is positive, with the reason, and that nothing persists.
func (m *m4World) d7ReqNotEligible(p *b11Parked, kind ResolutionKind, ev M4Evidence, reason string) {
	m.t.Helper()
	_, err := m.request(m.acting, m.m4In(p.fresh.ID, kind, ev.LineID))
	if !errors.Is(err, ErrResolutionEvidenceNotEligible) || !strings.Contains(err.Error(), reason) {
		m.t.Fatalf("want ErrResolutionEvidenceNotEligible/%s, got %v", reason, err)
	}
	if ResolutionToken(ClassifyResolutionError(err)) != TokenForceResolveEvidenceInsufficient {
		m.t.Fatalf("token: %s", ResolutionToken(ClassifyResolutionError(err)))
	}
	if n := m.countRows(`SELECT count(*) FROM payment_manual_resolutions WHERE tenant_id = $1 AND attempt_id = $2`, m.f.tenantID, p.fresh.ID); n != 0 {
		m.t.Fatalf("a refused request left %d resolution row(s)", n)
	}
	m.d7Held(p, "ineligible M4 request")
}

// --- Property 1: correct withdrawal ---------------------------------------------------------------

func TestD7_P1_WrongWithdrawal(t *testing.T) {
	m := newM4World(t)

	t.Run("a paid line of ANOTHER withdrawal's attempt (same amount) does not resolve this one", func(t *testing.T) {
		m.t = t
		a, b := m.park(610), m.park(610)
		m.ingest(m4Imp{}, m.line(m4Ref(), b.fresh.MerchantReference, "succeeded", 610, time.Now()))
		m.d7NonPositive(a, M4VerdictInsufficient)
		m.d7BothKindsRefusedAtRequest(a)
		evB := m.mustEvidence(b.fresh.ID, M4VerdictPaid)
		// A's own paid line exists, but B's line id is requested for A: not the deterministic line.
		m.ingest(m4Imp{}, m.line(m4Ref(), a.fresh.MerchantReference, "succeeded", 610, time.Now()))
		m.mustEvidence(a.fresh.ID, M4VerdictPaid)
		_, err := m.request(m.acting, m.m4In(a.fresh.ID, ResolutionM4EvidencePaid, evB.LineID))
		k3RequireCode(t, err, "MR061")
		m.d7Held(a, "B's line requested for A")
	})

	t.Run("the resolution's withdrawal is the attempt's, DB-forced", func(t *testing.T) {
		m.t = t
		p, _, ev := m.paidPark(611)
		r, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID))
		k3RequireNoErr(t, err, "request")
		if r.WithdrawalRequestID == nil || *r.WithdrawalRequestID != p.wr.ID || p.fresh.WithdrawalRequestID == nil || *p.fresh.WithdrawalRequestID != p.wr.ID {
			t.Fatalf("resolution withdrawal %v, attempt withdrawal %v, want %s", r.WithdrawalRequestID, p.fresh.WithdrawalRequestID, p.wr.ID)
		}
	})

	t.Run("the withdrawal is no longer submitted: refused at request and at execution", func(t *testing.T) {
		m.t = t
		// at request
		p, _, ev := m.paidPark(612)
		m.tx(func(ctx context.Context, tx pgx.Tx) error { return withdrawal.Fail(ctx, tx, p.wr.ID, "d7_other_route") })
		_, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID))
		k3RequireCode(t, err, "MR010")
		// at execution
		completedBefore := m.ledgerTxCount("withdrawal_completed")
		q, _, evq := m.paidPark(613)
		r, err := m.request(m.acting, m.m4In(q.fresh.ID, ResolutionM4EvidencePaid, evq.LineID))
		k3RequireNoErr(t, err, "request")
		m.tx(func(ctx context.Context, tx pgx.Tx) error { return withdrawal.Fail(ctx, tx, q.wr.ID, "d7_other_route") })
		out, err := m.decide(m.acting2, r, ResolutionApprove)
		if err == nil && (out.Executed || !out.Refused) {
			t.Fatalf("executed on a withdrawal that is no longer submitted: %+v", out)
		}
		if err != nil {
			k3RequireCode(t, err, "MR010")
		}
		if n := m.ledgerTxCount("withdrawal_completed"); n != completedBefore {
			t.Fatalf("a completion was posted for a failed withdrawal")
		}
	})
}

// --- Property 2: correct payout attempt -----------------------------------------------------------

func TestD7_P2_WrongAttempt(t *testing.T) {
	m := newM4World(t)

	t.Run("an attempt outside the M4 scope (pending, ambiguous, bound-reference park) is refused", func(t *testing.T) {
		m.t = t
		_, pend := m.payout(620) // pending, holds a reference
		_, amb := m.ambiguousPayout(621)
		bound := m.b11ParkPoll(622, OutcomePending) // unbound reason WITH a reference
		anyID := uuid.New()
		for _, id := range []uuid.UUID{pend.ID, amb.ID, bound.stale.ID} {
			for _, k := range []ResolutionKind{ResolutionM4EvidencePaid, ResolutionM4EvidenceNotPaid} {
				_, err := m.request(m.acting, m.m4In(id, k, &anyID))
				k3RequireCode(t, err, "MR012")
			}
		}
		m.d7Held(bound, "scope")
	})

	t.Run("a line naming ANOTHER attempt's merchant reference never attributes to this attempt", func(t *testing.T) {
		m.t = t
		a, b := m.park(623), m.park(623)
		m.ingest(m4Imp{}, m.line(m4Ref(), b.fresh.MerchantReference, "succeeded", 623, time.Now()))
		m.d7NonPositive(a, M4VerdictInsufficient)
		m.d7BothKindsRefusedAtRequest(a)
	})

	t.Run("one reference naming two attempts' merchant references is ambiguous for BOTH (H-2)", func(t *testing.T) {
		m.t = t
		a, b := m.park(624), m.park(624)
		r := m4Ref()
		m.ingest(m4Imp{}, m.line(r, a.fresh.MerchantReference, "succeeded", 624, time.Now()), m.line(r, b.fresh.MerchantReference, "succeeded", 624, time.Now()))
		m.d7NonPositive(a, M4VerdictContradictory)
		m.d7NonPositive(b, M4VerdictContradictory)
		m.d7BothKindsRefusedAtRequest(a)
		m.d7BothKindsRefusedAtRequest(b)
	})

	t.Run("a second attempt appears on R after the request: refused at execution", func(t *testing.T) {
		m.t = t
		a, r, ev := m.paidPark(625)
		b := m.park(625)
		got := m.d7RefusedAtExecution(a, ResolutionM4EvidencePaid, ev, func() {
			m.ingest(m4Imp{}, m.line(r, b.fresh.MerchantReference, "succeeded", 625, time.Now()))
		})
		if got != resolutionRefusedEvidence {
			t.Fatalf("refusal %q", got)
		}
		m.d7Held(b, "the other park")
	})
}

// --- Property 3: correct provider transaction / reference (R vs merchant ref vs Y) ---------------

func TestD7_P3_WrongProviderReference(t *testing.T) {
	m := newM4World(t)

	t.Run("R equals the attempt's OWN merchant reference: the DB reads paid, Go refuses (known SQL gap, see ADR 0111 s25.4)", func(t *testing.T) {
		m.t = t
		p := m.park(630)
		mr := p.fresh.MerchantReference
		m.ingest(m4Imp{}, m.line(mr, mr, "succeeded", 630, time.Now()))
		// PIN OF THE SQL GAP: payout_m4_evidence (migration 0125) has no "R is not a platform-issued
		// reference" clause. When it gains one this assertion flips to contradictory/insufficient.
		ev := m.mustEvidence(p.fresh.ID, M4VerdictPaid)
		if *ev.Reference != mr {
			t.Fatalf("setup: R=%q", *ev.Reference)
		}
		m.d7ReqNotEligible(p, ResolutionM4EvidencePaid, ev, m4EligPlatformRef)
	})

	t.Run("R equals ANOTHER attempt's merchant reference: refused in Go", func(t *testing.T) {
		m.t = t
		a, b := m.park(631), m.park(632)
		m.ingest(m4Imp{}, m.line(b.fresh.MerchantReference, a.fresh.MerchantReference, "succeeded", 631, time.Now()))
		ev := m.mustEvidence(a.fresh.ID, M4VerdictPaid)
		m.d7ReqNotEligible(a, ResolutionM4EvidencePaid, ev, m4EligPlatformRef)
	})

	t.Run("R is another attempt's provider_reference (X)", func(t *testing.T) {
		m.t = t
		p := m.park(633)
		_, other := m.payout(1234)
		m.ingest(m4Imp{}, m.line(*other.ProviderReference, p.fresh.MerchantReference, "succeeded", 633, time.Now()))
		m.d7NonPositive(p, M4VerdictContradictory)
		m.d7BothKindsRefusedAtRequest(p)
	})

	t.Run("R is the bound reference of a DEPOSIT attempt of the same provider", func(t *testing.T) {
		m.t = t
		p := m.park(6330)
		_, depositRef := m.depositAmbiguousBound()
		m.ingest(m4Imp{}, m.line(depositRef, p.fresh.MerchantReference, "succeeded", 6330, time.Now()))
		m.d7NonPositive(p, M4VerdictContradictory)
		m.d7BothKindsRefusedAtRequest(p)
	})

	t.Run("R is another attempt's typed Y", func(t *testing.T) {
		m.t = t
		p := m.park(634)
		_, _, y := m.parkPollMismatch()
		m.ingest(m4Imp{}, m.line(y, p.fresh.MerchantReference, "succeeded", 634, time.Now()))
		m.d7NonPositive(p, M4VerdictContradictory)
		m.d7BothKindsRefusedAtRequest(p)
	})

	t.Run("R already keys a ledger row, a tombstone, or provider:R an idempotency key", func(t *testing.T) {
		m.t = t
		for i, setup := range []func(r string){m.b11PostDepositKey, m.tombstone, m.idemKeyOnly} {
			p := m.park(int64(635 + i))
			r := m4Ref()
			setup(r)
			m.ingest(m4Imp{}, m.line(r, p.fresh.MerchantReference, "succeeded", int64(635+i), time.Now()))
			m.d7NonPositive(p, M4VerdictContradictory)
			m.d7BothKindsRefusedAtRequest(p)
		}
	})

	t.Run("another PROVIDER's statement in the same tenant, naming this merchant reference, is not evidence", func(t *testing.T) {
		m.t = t
		p := m.park(639)
		l := m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", 639, time.Now())
		l.ProviderID = "d7-other-psp"
		m.ingest(m4Imp{provider: "d7-other-psp"}, l)
		m.d7NonPositive(p, M4VerdictInsufficient)
		m.d7BothKindsRefusedAtRequest(p)
		// ... and a declined one with a covering window.
		d := m.line("m4-D-"+uuid.NewString()[:12], p.fresh.MerchantReference, "declined", 639, time.Now())
		d.ProviderID = "d7-other-psp"
		m.ingest(m4Imp{provider: "d7-other-psp", start: p.fresh.CreatedAt.Add(-time.Minute), end: p.fresh.LastSentAt.Add(25 * time.Hour)}, d)
		m.d7NonPositive(p, M4VerdictInsufficient)
		m.d7BothKindsRefusedAtRequest(p)
	})
	t.Run("two different references on one merchant reference", func(t *testing.T) {
		m.t = t
		p := m.park(640)
		m.ingest(m4Imp{}, m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", 640, time.Now()), m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", 640, time.Now()))
		m.d7NonPositive(p, M4VerdictContradictory)
		m.d7BothKindsRefusedAtRequest(p)
	})

	t.Run("a reserved-prefix R cannot even be stored as a statement line (CHECK ..._no_reserved_prefix)", func(t *testing.T) {
		m.t = t
		p := m.park(641)
		before := m.countRows(`SELECT count(*) FROM payment_statement_lines WHERE tenant_id = $1`, m.f.tenantID)
		_, err := m.ingestErr(m4Imp{}, m.line(m.rowString(`SELECT payment_reserved_ref_prefix()`)+uuid.NewString(), p.fresh.MerchantReference, "succeeded", 641, time.Now()))
		if k3Code(err) != "23514" {
			t.Fatalf("want the CHECK violation, got %v", err)
		}
		if after := m.countRows(`SELECT count(*) FROM payment_statement_lines WHERE tenant_id = $1`, m.f.tenantID); after != before {
			t.Fatalf("a refused import stored %d line(s)", after-before)
		}
		m.d7NonPositive(p, M4VerdictInsufficient)
	})

	t.Run("R later also names another merchant reference: refused at execution", func(t *testing.T) {
		m.t = t
		p, r, ev := m.paidPark(642)
		got := m.d7RefusedAtExecution(p, ResolutionM4EvidencePaid, ev, func() {
			m.ingest(m4Imp{unsealed: true}, m.line(r, "d7-someone-else", "pending", 642, time.Now()))
		})
		if got != resolutionRefusedEvidence {
			t.Fatalf("refusal %q", got)
		}
	})
}

// --- Property 4: correct player / tenant context -------------------------------------------------

func TestD7_P4_WrongPlayerOrTenant(t *testing.T) {
	pool := depositV2ScratchPool(t)
	a, b := newM4WorldOn(t, pool), newM4WorldOn(t, pool)

	t.Run("tenant B's lines carrying tenant A's merchant reference and amount give A no verdict", func(t *testing.T) {
		a.t, b.t = t, t
		pa := a.park(650)
		// B's source (B's provider id, B's tenant) names A's merchant reference.
		b.ingest(m4Imp{}, b.line(m4Ref(), pa.fresh.MerchantReference, "succeeded", 650, time.Now()))
		a.d7NonPositive(pa, M4VerdictInsufficient)
		a.d7BothKindsRefusedAtRequest(pa)
		// ... and a declined line with a covering window.
		b.ingest(m4Imp{start: pa.fresh.CreatedAt.Add(-time.Minute), end: pa.fresh.LastSentAt.Add(25 * time.Hour)},
			b.line(m4Ref(), pa.fresh.MerchantReference, "declined", 650, time.Now()))
		a.d7NonPositive(pa, M4VerdictInsufficient)
		a.d7BothKindsRefusedAtRequest(pa)
	})

	t.Run("tenant A's lines under A's own provider do not become B's evidence", func(t *testing.T) {
		a.t, b.t = t, t
		pb := b.park(651)
		a.ingest(m4Imp{}, a.line(m4Ref(), pb.fresh.MerchantReference, "succeeded", 651, time.Now()))
		b.d7NonPositive(pb, M4VerdictInsufficient)
	})

	t.Run("a platform principal acting in tenant B cannot request for tenant A's attempt, nor for B's with A's id", func(t *testing.T) {
		a.t, b.t = t, t
		pa, _, evA := a.paidPark(652)
		// B's acting principal targeted at A: no grant in A.
		tg, err := NewResolutionTarget(tenant.Context{TenantID: uuid.Nil}, a.f.tenantID)
		k3RequireNoErr(t, err, "target")
		_, err = a.svc.Request(k3Ctx(b.acting), tg, a.m4In(pa.fresh.ID, ResolutionM4EvidencePaid, evA.LineID), ResolutionMeta{RequestID: "d7"})
		if err == nil {
			t.Fatal("tenant B's acting principal requested an M4 in tenant A")
		}
		// A's acting principal, targeting A, with B's attempt id.
		pb, _, evB := b.paidPark(653)
		_, err = a.request(a.acting, a.m4In(pb.fresh.ID, ResolutionM4EvidencePaid, evB.LineID))
		if err == nil {
			t.Fatal("tenant A requested an M4 for tenant B's attempt")
		}
		for _, n := range []struct {
			w *m4World
			p *b11Parked
		}{{a, pa}, {b, pb}} {
			if c := n.w.countRows(`SELECT count(*) FROM payment_manual_resolutions WHERE tenant_id = $1`, n.w.f.tenantID); c != 0 {
				t.Fatalf("a cross-tenant request left %d row(s) in tenant %s", c, n.w.f.tenantID)
			}
			n.w.d7Held(n.p, "cross-tenant")
		}
	})

	t.Run("a tenant-scoped principal of B cannot even form a target for A", func(t *testing.T) {
		a.t, b.t = t, t
		if _, err := NewResolutionTarget(tenant.Context{TenantID: b.f.tenantID}, a.f.tenantID); err == nil {
			t.Fatal("a tenant-B principal formed a tenant-A target")
		}
	})

	t.Run("another tenant's contradicting lines arriving after the request do not stop an honest execution", func(t *testing.T) {
		a.t, b.t = t, t
		pa, _, ev := a.paidPark(654)
		r, err := a.request(a.acting, a.m4In(pa.fresh.ID, ResolutionM4EvidencePaid, ev.LineID))
		k3RequireNoErr(t, err, "request")
		b.ingest(m4Imp{unsealed: true}, b.line(m4Ref(), pa.fresh.MerchantReference, "declined", 654, time.Now()), b.line(m4Ref(), pa.fresh.MerchantReference, "reversed", 654, time.Now()))
		out, err := a.decide(a.acting2, r, ResolutionApprove)
		k3RequireNoErr(t, err, "approve")
		if !out.Executed {
			t.Fatalf("tenant B's lines blocked tenant A (RLS/tenant predicate leak): %+v", out)
		}
		a.assertInvariants()
	})

	t.Run("the posting lands on the withdrawal's own wallet (player context)", func(t *testing.T) {
		a.t = t
		pa, _, ev := a.paidPark(655)
		out := a.execute(pa, ResolutionM4EvidencePaid, ev)
		rows := a.sysQuery(`SELECT a.wallet_id, a.account_type, e.direction FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.ledger_account_id
			WHERE e.ledger_transaction_id = $1 ORDER BY a.account_type`, *out.Resolution.LedgerTransactionID)
		if len(rows) != 2 {
			t.Fatalf("want 2 entries, got %d", len(rows))
		}
		for _, r := range rows {
			if r["account_type"] == "player_withdrawal_hold" {
				if w, ok := r["wallet_id"].([16]byte); !ok || uuid.UUID(w) != a.f.walletID {
					t.Fatalf("hold debit on wallet %v, want the withdrawal's %s", r["wallet_id"], a.f.walletID)
				}
			}
		}
	})
}

// --- Property 5 and 6: correct asset, correct amount ----------------------------------------------

func TestD7_P5_P6_WrongAssetOrAmount(t *testing.T) {
	m := newM4World(t)
	type wrong struct {
		name   string
		mutate func(l *statement.PaymentStatementLine, amount int64)
	}
	wrongs := []wrong{
		{"amount +1", func(l *statement.PaymentStatementLine, a int64) { l.Amount = a + 1 }},
		{"amount -1", func(l *statement.PaymentStatementLine, a int64) { l.Amount = a - 1 }},
		{"amount scaled x100 (an exponent mix-up)", func(l *statement.PaymentStatementLine, a int64) { l.Amount = a * 100 }},
		{"asset USD", func(l *statement.PaymentStatementLine, a int64) { l.AssetCode = "USD" }},
		{"asset GBP", func(l *statement.PaymentStatementLine, a int64) { l.AssetCode = "GBP" }},
	}
	t.Run("paid: one wrong value in the amount or the asset", func(t *testing.T) {
		for i, w := range wrongs {
			m.t = t
			amount := int64(660 + i)
			p := m.park(amount)
			l := m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", amount, time.Now())
			w.mutate(&l, amount)
			m.ingest(m4Imp{}, l)
			m.d7NonPositive(p, M4VerdictContradictory)
			m.d7BothKindsRefusedAtRequest(p)
			m.t.Logf("ok: %s", w.name)
		}
	})
	t.Run("not paid: a decline of another amount or asset", func(t *testing.T) {
		for i, w := range wrongs {
			m.t = t
			amount := int64(670 + i)
			p := m.park(amount)
			l := m.line("m4-D-"+uuid.NewString()[:12], p.fresh.MerchantReference, "declined", amount, time.Now())
			w.mutate(&l, amount)
			m.ingest(m4Imp{start: p.fresh.CreatedAt.Add(-time.Minute), end: p.fresh.LastSentAt.Add(25 * time.Hour)}, l)
			m.d7NonPositive(p, M4VerdictInsufficient)
			m.d7BothKindsRefusedAtRequest(p)
		}
	})
	t.Run("a late line of another amount or asset after the request: refused at execution (paid)", func(t *testing.T) {
		for i, w := range wrongs {
			m.t = t
			amount := int64(680 + i)
			p, r, ev := m.paidPark(amount)
			got := m.d7RefusedAtExecution(p, ResolutionM4EvidencePaid, ev, func() {
				l := m.line(r, "", "succeeded", amount, time.Now().Add(time.Second))
				w.mutate(&l, amount)
				m.ingest(m4Imp{unsealed: true}, l)
			})
			if got != resolutionRefusedEvidence {
				t.Fatalf("%s: refusal %q", w.name, got)
			}
		}
	})
	t.Run("a malformed asset code cannot even be stored as evidence (CHECK payment_statement_lines_asset_code_shape)", func(t *testing.T) {
		m.t = t
		l := m.line(m4Ref(), "d7-x", "succeeded", 1, time.Now())
		l.AssetCode = "eur"
		if _, err := m.ingestErr(m4Imp{}, l); k3Code(err) != "23514" {
			t.Fatalf("want a CHECK violation, got %v", err)
		}
	})
	t.Run("the withdrawal, attempt and resolution agree on amount and asset (R-4, DB-forced)", func(t *testing.T) {
		m.t = t
		p, _, ev := m.paidPark(690)
		r, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID))
		k3RequireNoErr(t, err, "request")
		if r.Amount != p.wr.Amount || r.AssetCode != p.wr.AssetCode || r.Amount != p.fresh.Amount || r.AssetCode != p.fresh.AssetCode {
			t.Fatalf("resolution %d %s, withdrawal %d %s, attempt %d %s", r.Amount, r.AssetCode, p.wr.Amount, p.wr.AssetCode, p.fresh.Amount, p.fresh.AssetCode)
		}
	})
}

// --- Property 7: correct destination / instrument -------------------------------------------------

func TestD7_P7_WrongDestination(t *testing.T) {
	m := newM4World(t)

	t.Run("a destination_mismatch park cannot be M4-paid, even with a perfect succeeded line; the SCOPE refuses it, not the verdict", func(t *testing.T) {
		m.t = t
		wr, a := m.payout(700)
		m.tx(func(ctx context.Context, tx pgx.Tx) error {
			return ApplyDisputeFromNonTerminal(ctx, tx, a.ID, EvidenceCallback, "destination_mismatch")
		})
		// A perfect, sealed, attributed, matching succeeded line on the merchant reference and on the
		// bound reference: the VERDICT would be paid. Only the scope keeps this from paying.
		fresh := m.attempt(a.ID)
		m.ingest(m4Imp{}, m.line(m4Ref(), fresh.MerchantReference, "succeeded", 700, time.Now()))
		m.mustEvidence(a.ID, M4VerdictPaid)
		anyID := uuid.New()
		_, err := m.request(m.acting, m.m4In(a.ID, ResolutionM4EvidencePaid, &anyID))
		k3RequireCode(t, err, "MR012")
		if ResolutionToken(ClassifyResolutionError(err)) != TokenForceResolveReasonNotResolved {
			t.Fatalf("token %s", ResolutionToken(ClassifyResolutionError(err)))
		}
		if M4ResolvableDispute(ResolutionM4EvidencePaid, AttemptDisputed, k3StrPtr("destination_mismatch"), fresh.ProviderReference) {
			t.Fatal("the Go restatement of the scope admits destination_mismatch paid")
		}
		if w := m.wd(wr.ID); w.State != withdrawal.StateSubmitted {
			t.Fatalf("withdrawal %s", w.State)
		}
		m.assertInvariants()
	})

	t.Run("a destination_mismatch park is not M4-not-paid on a succeeded line either (verdict, not scope)", func(t *testing.T) {
		m.t = t
		_, a := m.payout(701)
		m.tx(func(ctx context.Context, tx pgx.Tx) error {
			return ApplyDisputeFromNonTerminal(ctx, tx, a.ID, EvidenceCallback, "destination_mismatch")
		})
		fresh := m.attempt(a.ID)
		m.ingest(m4Imp{}, m.line(*fresh.ProviderReference, fresh.MerchantReference, "succeeded", 701, time.Now()))
		anyID := uuid.New()
		_, err := m.request(m.acting, m.m4In(a.ID, ResolutionM4EvidenceNotPaid, &anyID))
		k3RequireCode(t, err, "MR062")
	})

	t.Run("every bound payout carries a write-once destination snapshot; the statement path reads none of it (the documented gap)", func(t *testing.T) {
		m.t = t
		// KNOWN GAP, pinned (ADR 0111 s25.4 G-DEST): the statement-line model has no destination field,
		// so the paid verdict can never positively evidence the destination. For MOCK that is by design;
		// for any non-MOCK source it is closed by the Go gate (TestD7_NonMockM4IsBlocked...), because
		// the checklist cannot admit "paid" while m4StatementLineCarriesDestinationEcho is false.
		p, _, _ := m.paidPark(702)
		if n := m.countRows(`SELECT count(*) FROM payout_attempt_destination_snapshots WHERE tenant_id = $1 AND withdrawal_request_id = $2`, m.f.tenantID, p.wr.ID); n < 1 {
			t.Fatalf("setup: a bound payout must carry a destination snapshot, got %d", n)
		}
	})
}

// --- Property 8: final provider status ------------------------------------------------------------

func TestD7_P8_NonFinalProviderStatus(t *testing.T) {
	m := newM4World(t)
	t.Run("pending only", func(t *testing.T) {
		m.t = t
		p := m.park(710)
		m.ingest(m4Imp{start: p.fresh.CreatedAt.Add(-time.Minute), end: p.fresh.LastSentAt.Add(25 * time.Hour)}, m.line(m4Ref(), p.fresh.MerchantReference, "pending", 710, time.Now()))
		m.d7NonPositive(p, M4VerdictInsufficient)
		m.d7BothKindsRefusedAtRequest(p)
	})
	t.Run("reversed only", func(t *testing.T) {
		m.t = t
		p := m.park(711)
		m.ingest(m4Imp{start: p.fresh.CreatedAt.Add(-time.Minute), end: p.fresh.LastSentAt.Add(25 * time.Hour)}, m.line(m4Ref(), p.fresh.MerchantReference, "reversed", 711, time.Now()))
		m.d7NonPositive(p, M4VerdictInsufficient)
		m.d7BothKindsRefusedAtRequest(p)
	})
	t.Run("succeeded then reversed (a return/chargeback of a payout), any import", func(t *testing.T) {
		m.t = t
		p := m.park(712)
		r := m4Ref()
		m.ingest(m4Imp{}, m.line(r, p.fresh.MerchantReference, "succeeded", 712, time.Now()))
		m.ingest(m4Imp{unsealed: true}, m.line(r, "", "reversed", 712, time.Now().Add(time.Minute)))
		m.d7NonPositive(p, M4VerdictContradictory)
		m.d7BothKindsRefusedAtRequest(p)
	})
	t.Run("succeeded then pending on the merchant reference", func(t *testing.T) {
		m.t = t
		p := m.park(713)
		m.ingest(m4Imp{}, m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", 713, time.Now()), m.line(m4Ref(), p.fresh.MerchantReference, "pending", 713, time.Now()))
		m.d7NonPositive(p, M4VerdictContradictory)
	})
	t.Run("a declined line and a pending / reversed line on the SAME reference in one sealed import: not paid is not established", func(t *testing.T) {
		for i, st := range []string{"pending", "reversed"} {
			m.t = t
			p := m.park(int64(7140 + i))
			m.ingest(m4Imp{start: p.fresh.CreatedAt.Add(-time.Minute), end: p.fresh.LastSentAt.Add(25 * time.Hour)},
				m.line("m4-D-"+uuid.NewString()[:12], p.fresh.MerchantReference, "declined", int64(7140+i), time.Now()),
				m.line("m4-D-"+uuid.NewString()[:12], p.fresh.MerchantReference, st, int64(7140+i), time.Now()))
			m.d7NonPositive(p, M4VerdictInsufficient)
			m.d7BothKindsRefusedAtRequest(p)
		}
	})
	t.Run("a late pending / reversed / declined after the request: refused at execution (paid)", func(t *testing.T) {
		for i, st := range []string{"pending", "reversed", "declined"} {
			m.t = t
			p, r, ev := m.paidPark(int64(714 + i))
			got := m.d7RefusedAtExecution(p, ResolutionM4EvidencePaid, ev, func() {
				m.ingest(m4Imp{unsealed: true}, m.line(r, "", st, int64(714+i), time.Now()))
			})
			if got != resolutionRefusedEvidence {
				t.Fatalf("%s: refusal %q", st, got)
			}
		}
	})
	t.Run("a late succeeded / pending / reversed after a not-paid request: refused at execution", func(t *testing.T) {
		for i, st := range []string{"succeeded", "pending", "reversed"} {
			m.t = t
			p, ev := m.notPaidPark(int64(720 + i))
			got := m.d7RefusedAtExecution(p, ResolutionM4EvidenceNotPaid, ev, func() {
				m.ingest(m4Imp{unsealed: true}, m.line(m4Ref(), p.fresh.MerchantReference, st, int64(720+i), time.Now()))
			})
			if got != resolutionRefusedEvidence {
				t.Fatalf("%s: refusal %q", st, got)
			}
		}
	})
}

// --- Property 9: causal relationship between the provider evidence and the attempt ---------------

func TestD7_P9_CausalLink(t *testing.T) {
	m := newM4World(t)
	win := func(p *b11Parked, o m4Imp) m4Imp {
		o.start, o.end = p.fresh.CreatedAt.Add(-time.Minute), p.fresh.LastSentAt.Add(25*time.Hour)
		return o
	}
	t.Run("a succeeded line naming no merchant reference (R only) attributes to no attempt", func(t *testing.T) {
		m.t = t
		a, b := m.park(730), m.park(730)
		m.ingest(m4Imp{}, m.line(m4Ref(), "", "succeeded", 730, time.Now()))
		m.d7NonPositive(a, M4VerdictInsufficient)
		m.d7NonPositive(b, M4VerdictInsufficient)
		m.d7BothKindsRefusedAtRequest(a)
	})
	t.Run("a declared source whose declined line names no merchant reference", func(t *testing.T) {
		m.t = t
		p := m.park(731)
		m.ingest(win(p, m4Imp{}), m.line("m4-D-"+uuid.NewString()[:12], "", "declined", 731, time.Now()))
		m.d7NonPositive(p, M4VerdictInsufficient)
		m.d7BothKindsRefusedAtRequest(p)
	})
	t.Run("an UNDECLARED source: a perfect declined line is not enough (S-3 declaration, (iv))", func(t *testing.T) {
		m.t = t
		p := m.park(732)
		m.ingest(win(p, m4Imp{noDecl: true}), m.line("m4-D-"+uuid.NewString()[:12], p.fresh.MerchantReference, "declined", 732, time.Now()))
		m.d7NonPositive(p, M4VerdictInsufficient)
		m.d7BothKindsRefusedAtRequest(p)
	})
	t.Run("a declining source holding a payout line with a NULL merchant reference contradicts its declaration; late arrival refused at execution", func(t *testing.T) {
		m.t = t
		p, ev := m.notPaidPark(733)
		got := m.d7RefusedAtExecution(p, ResolutionM4EvidenceNotPaid, ev, func() {
			// A new declaring import with a NULL-merchant payout line on the declined line's own reference.
			d := m.rowString(`SELECT provider_reference FROM payment_statement_lines WHERE id = $1`, *ev.LineID)
			m.ingest(m4Imp{}, m.line(d, "", "declined", 733, time.Now()))
		})
		if got != resolutionRefusedEvidence {
			t.Fatalf("refusal %q", got)
		}
	})
	t.Run("a perfect declined line in a declaring import that also holds ANY payout line without a merchant reference", func(t *testing.T) {
		m.t = t
		p := m.park(7310)
		m.ingest(win(p, m4Imp{}),
			m.line("m4-D-"+uuid.NewString()[:12], p.fresh.MerchantReference, "declined", 7310, time.Now()),
			m.line(m4Ref(), "", "succeeded", 1, time.Now())) // unrelated to this attempt, but the declaration is contradicted
		m.d7NonPositive(p, M4VerdictInsufficient)
		m.d7BothKindsRefusedAtRequest(p)
	})
	t.Run("a window that opens AFTER the attempt was created does not cover its settlement", func(t *testing.T) {
		m.t = t
		p := m.park(7311)
		m.ingest(m4Imp{start: p.fresh.CreatedAt.Add(time.Hour), end: p.fresh.LastSentAt.Add(30 * time.Hour)},
			m.line("m4-D-"+uuid.NewString()[:12], p.fresh.MerchantReference, "declined", 7311, time.Now()))
		m.d7NonPositive(p, M4VerdictInsufficient)
		m.d7BothKindsRefusedAtRequest(p)
	})
	t.Run("a declined line dated before the last send is not the answer to it", func(t *testing.T) {
		m.t = t
		p := m.park(734)
		m.ingest(win(p, m4Imp{}), m.line("m4-D-"+uuid.NewString()[:12], p.fresh.MerchantReference, "declined", 734, p.fresh.LastSentAt.Add(-time.Hour)))
		m.d7NonPositive(p, M4VerdictInsufficient)
	})
	t.Run("a window that does not cover the settlement horizon", func(t *testing.T) {
		m.t = t
		p := m.park(735)
		m.ingest(m4Imp{start: p.fresh.CreatedAt.Add(-time.Minute), end: p.fresh.LastSentAt.Add(2 * time.Hour)},
			m.line("m4-D-"+uuid.NewString()[:12], p.fresh.MerchantReference, "declined", 735, time.Now()))
		m.d7NonPositive(p, M4VerdictInsufficient)
	})
	t.Run("a succeeded line dated BEFORE the attempt existed is not its payout: DB reads paid (SQL gap), Go refuses", func(t *testing.T) {
		m.t = t
		p := m.park(736)
		m.ingest(m4Imp{}, m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", 736, p.fresh.CreatedAt.Add(-48*time.Hour)))
		// PIN OF THE SQL GAP: the paid branch of payout_m4_evidence reads no timestamp.
		ev := m.mustEvidence(p.fresh.ID, M4VerdictPaid)
		m.d7ReqNotEligible(p, ResolutionM4EvidencePaid, ev, m4EligPredates)
	})
	t.Run("a succeeded line dated AFTER the coverage its import vouches for: DB reads paid (SQL gap), Go refuses", func(t *testing.T) {
		m.t = t
		p := m.park(737)
		m.ingest(m4Imp{}, m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", 737, time.Now().Add(10*24*time.Hour)))
		ev := m.mustEvidence(p.fresh.ID, M4VerdictPaid)
		m.d7ReqNotEligible(p, ResolutionM4EvidencePaid, ev, m4EligAfterCoverage)
	})
	t.Run("the lower bound is the FIRST SEND, zero tolerance: one microsecond before is refused, the instant itself executes", func(t *testing.T) {
		m.t = t
		p := m.park(738)
		if p.fresh.FirstSubmittedAt == nil {
			t.Fatal("setup: the park has no first_submitted_at")
		}
		first := *p.fresh.FirstSubmittedAt
		if first.Before(p.fresh.CreatedAt) {
			t.Fatalf("setup: first send %s before creation %s", first, p.fresh.CreatedAt)
		}
		// A line between creation and the first send is refused (the bound is the first send, not creation).
		if first.After(p.fresh.CreatedAt) {
			q := m.park(739)
			m.ingest(m4Imp{}, m.line(m4Ref(), q.fresh.MerchantReference, "succeeded", 739, q.fresh.FirstSubmittedAt.Add(-time.Microsecond)))
			evq := m.mustEvidence(q.fresh.ID, M4VerdictPaid)
			m.d7ReqNotEligible(q, ResolutionM4EvidencePaid, evq, m4EligPredates)
		}
		m.ingest(m4Imp{}, m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", 738, first))
		ev := m.mustEvidence(p.fresh.ID, M4VerdictPaid)
		m.execute(p, ResolutionM4EvidencePaid, ev)
	})
	t.Run("a succeeded line dated before last_sent_at but after the first send is NOT refused (an earlier send may have paid)", func(t *testing.T) {
		m.t = t
		p := m.park(7381)
		if p.fresh.LastSentAt.After(*p.fresh.FirstSubmittedAt) {
			m.ingest(m4Imp{}, m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", 7381, p.fresh.FirstSubmittedAt.Add(time.Microsecond)))
			ev := m.mustEvidence(p.fresh.ID, M4VerdictPaid)
			m.execute(p, ResolutionM4EvidencePaid, ev)
			return
		}
		// Single send: last_sent_at == first_submitted_at; the resend ordering is probed below.
		t.Log("single-send park: the resend ordering is probed in 'an attempt with no recorded send'")
	})
	t.Run("an attempt with no recorded send has no eligible paid line", func(t *testing.T) {
		m.t = t
		p := m.park(7382)
		res := ManualResolution{TenantID: m.f.tenantID, AttemptID: p.fresh.ID, Kind: ResolutionM4EvidencePaid}
		m.ingest(m4Imp{}, m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", 7382, time.Now()))
		ev := m.mustEvidence(p.fresh.ID, M4VerdictPaid)
		res.EvidenceLineID, res.EvidenceReference, res.EvidenceImportIDs = ev.LineID, ev.Reference, ev.ImportIDs
		att := p.fresh
		att.FirstSubmittedAt, att.LastSentAt = nil, nil
		var got string
		if err := m.pool.WithPlatformActingInTenant(context.Background(), m.acting.ID, m.f.tenantID, uuid.Nil, OperationKindForceResolve, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			got, err = m4EligibilityRefusal(ctx, tx, res, att)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if got != m4EligNeverSent {
			t.Fatalf("want %s, got %q", m4EligNeverSent, got)
		}
		// first_submitted_at absent, last_sent_at present: the fallback bound is last_sent_at.
		att = p.fresh
		att.FirstSubmittedAt = nil
		later := time.Now().Add(time.Hour)
		att.LastSentAt = &later
		if err := m.pool.WithPlatformActingInTenant(context.Background(), m.acting.ID, m.f.tenantID, uuid.Nil, OperationKindForceResolve, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			got, err = m4EligibilityRefusal(ctx, tx, res, att)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if got != m4EligPredates {
			t.Fatalf("fallback to last_sent_at: want %s, got %q", m4EligPredates, got)
		}
		// A resent attempt: first send an hour ago, last send an hour ahead; the line (now) lies between
		// them. The bound is the FIRST send, so it is eligible.
		early := time.Now().Add(-time.Hour)
		att.FirstSubmittedAt = &early
		if err := m.pool.WithPlatformActingInTenant(context.Background(), m.acting.ID, m.f.tenantID, uuid.Nil, OperationKindForceResolve, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			got, err = m4EligibilityRefusal(ctx, tx, res, att)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if got != "" {
			t.Fatalf("a line before last_sent_at but after the first send must be eligible, got %q", got)
		}
	})
	t.Run("DOCUMENTING (no assertion on the r32 outcome): success-triggered parks", func(t *testing.T) {
		// ADR 0111 25.4 G-SUCCESS-PARK. provider_reference_conflict, destination_mismatch and
		// destination_integrity_failure parks are raised on a provider SUCCESS that is not stored
		// durably; a later declined statement line can therefore satisfy the not-paid verdict. The
		// fix (a durable success-reported record in every park writer, not-paid contradictory for
		// ALL M4 not-paid reasons) is carried by gov-r32-integrity (migration 0127). This subtest
		// only records, with the present scope, which not-paid reasons are exposed; it asserts
		// nothing that r32 would change.
		for _, r := range []string{"provider_reference_conflict", "destination_mismatch", "destination_integrity_failure"} {
			t.Logf("not-paid scope admits %-32s today: %v (exposed to G-SUCCESS-PARK until 0127)", r, M4ResolvableDispute(ResolutionM4EvidenceNotPaid, AttemptDisputed, k3StrPtr(r), nil))
		}
	})
}

// rowString reads one text value.
func (m *m4World) rowString(sql string, args ...any) string {
	m.t.Helper()
	var s string
	m.tx(func(ctx context.Context, tx pgx.Tx) error { return tx.QueryRow(ctx, sql, args...).Scan(&s) })
	return s
}

// --- The Go-side eligibility check is the SAME function at request and at execution ---------------

// m4EligibilityRefusal runs at the request (requestInTx) and at the execution (m4EvidenceRefusal) on
// the pinned evidence; lines are immutable, so the execution-time branch is driven here directly on
// the pinned resolution fields.
func TestD7_EligibilityRefusal_ReasonsAtTheExecutionPoint(t *testing.T) {
	m := newM4World(t)
	probe := func(res ManualResolution, att PaymentAttempt) string {
		var got string
		if err := m.pool.WithPlatformActingInTenant(context.Background(), m.acting.ID, m.f.tenantID, uuid.Nil, OperationKindForceResolve, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			got, err = m4EligibilityRefusal(ctx, tx, res, att)
			return err
		}); err != nil {
			t.Fatalf("m4EligibilityRefusal: %v", err)
		}
		return got
	}
	// control: an eligible paid evidence.
	p, r, ev := m.paidPark(740)
	mk := func(p *b11Parked, ev M4Evidence, kind ResolutionKind) ManualResolution {
		return ManualResolution{TenantID: m.f.tenantID, AttemptID: p.fresh.ID, Kind: kind, EvidenceLineID: ev.LineID, EvidenceReference: ev.Reference, EvidenceImportIDs: ev.ImportIDs}
	}
	if got := probe(mk(p, ev, ResolutionM4EvidencePaid), p.fresh); got != "" {
		t.Fatalf("control refused: %q", got)
	}
	_ = r
	// R = merchant reference.
	q := m.park(741)
	m.ingest(m4Imp{}, m.line(q.fresh.MerchantReference, q.fresh.MerchantReference, "succeeded", 741, time.Now()))
	evq := m.mustEvidence(q.fresh.ID, M4VerdictPaid)
	if got := probe(mk(q, evq, ResolutionM4EvidencePaid), q.fresh); got != m4EligPlatformRef {
		t.Fatalf("R=merchant ref: %q", got)
	}
	// predates / after coverage.
	s := m.park(742)
	m.ingest(m4Imp{}, m.line(m4Ref(), s.fresh.MerchantReference, "succeeded", 742, s.fresh.CreatedAt.Add(-time.Hour)))
	evs := m.mustEvidence(s.fresh.ID, M4VerdictPaid)
	if got := probe(mk(s, evs, ResolutionM4EvidencePaid), s.fresh); got != m4EligPredates {
		t.Fatalf("predates: %q", got)
	}
	u := m.park(743)
	m.ingest(m4Imp{}, m.line(m4Ref(), u.fresh.MerchantReference, "succeeded", 743, time.Now().Add(72*time.Hour)))
	evu := m.mustEvidence(u.fresh.ID, M4VerdictPaid)
	if got := probe(mk(u, evu, ResolutionM4EvidencePaid), u.fresh); got != m4EligAfterCoverage {
		t.Fatalf("after coverage: %q", got)
	}
	// An incomplete paid resolution.
	if got := probe(ManualResolution{TenantID: m.f.tenantID, AttemptID: p.fresh.ID, Kind: ResolutionM4EvidencePaid, EvidenceImportIDs: ev.ImportIDs}, p.fresh); got != m4EligIncomplete {
		t.Fatalf("incomplete: %q", got)
	}
	// The non-MOCK gate: a real-shaped import in the set blocks both kinds.
	n := m.park(744)
	m.ingest(m4Imp{real: true}, m.line(m4Ref(), n.fresh.MerchantReference, "succeeded", 744, time.Now()))
	evn := m.mustEvidence(n.fresh.ID, M4VerdictPaid)
	for _, k := range []ResolutionKind{ResolutionM4EvidencePaid, ResolutionM4EvidenceNotPaid} {
		if got := probe(mk(n, evn, k), n.fresh); got != m4EligNonMock {
			t.Fatalf("non-MOCK %s: %q", k, got)
		}
	}
	// Not-paid ignores the paid-only checks (the SQL not-paid verdict reads occurred_at itself).
	if got := probe(mk(s, evs, ResolutionM4EvidenceNotPaid), s.fresh); got != "" {
		t.Fatalf("not-paid must not run the paid-only checks: %q", got)
	}
}

// Non-MOCK M4 is BLOCKED (ADR 0111 10.3): a positive database verdict resting on a real-shaped
// (non-MOCK) import is refused by the Go gate at the request, for BOTH kinds, with nothing persisted.
func TestD7_NonMockM4IsBlocked_PositiveDatabaseVerdictOnARealImportIsRefused(t *testing.T) {
	m := newM4World(t)
	t.Run("paid", func(t *testing.T) {
		m.t = t
		p := m.park(750)
		m.ingest(m4Imp{real: true}, m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", 750, time.Now()))
		ev := m.mustEvidence(p.fresh.ID, M4VerdictPaid)
		m.d7ReqNotEligible(p, ResolutionM4EvidencePaid, ev, m4EligNonMock)
	})
	t.Run("not paid", func(t *testing.T) {
		m.t = t
		p := m.park(751)
		m.ingest(m4Imp{real: true, start: p.fresh.CreatedAt.Add(-time.Minute), end: p.fresh.LastSentAt.Add(25 * time.Hour)},
			m.line("m4-D-"+uuid.NewString()[:12], p.fresh.MerchantReference, "declined", 751, time.Now()))
		ev := m.mustEvidence(p.fresh.ID, M4VerdictNotPaid)
		m.d7ReqNotEligible(p, ResolutionM4EvidenceNotPaid, ev, m4EligNonMock)
	})
}

// --- Meta property: NO SINGLE untrusted provider field is sufficient ------------------------------

// Part 1: a statement in which exactly ONE provider-controlled field is right (here: the one named) and
// everything else is wrong or absent never yields a positive verdict, for either kind.
func TestD7_Meta_NoSingleProviderFieldIsSufficient_OnlyThisFieldIsRight(t *testing.T) {
	m := newM4World(t)
	other := func(p *b11Parked) string { return m.park(p.fresh.Amount).fresh.MerchantReference } // another attempt's merchant ref
	type tc struct {
		name  string
		build func(p *b11Parked) (imp m4Imp, lines []statement.PaymentStatementLine)
	}
	cover := func(p *b11Parked) m4Imp {
		return m4Imp{start: p.fresh.CreatedAt.Add(-time.Minute), end: p.fresh.LastSentAt.Add(25 * time.Hour)}
	}
	cases := []tc{
		{"only the merchant reference is right (pending, wrong amount, wrong asset)", func(p *b11Parked) (m4Imp, []statement.PaymentStatementLine) {
			l := m.line(m4Ref(), p.fresh.MerchantReference, "pending", p.fresh.Amount+9, time.Now())
			l.AssetCode = "USD"
			return cover(p), []statement.PaymentStatementLine{l}
		}},
		{"only the merchant reference is right (succeeded, wrong amount and asset)", func(p *b11Parked) (m4Imp, []statement.PaymentStatementLine) {
			l := m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", p.fresh.Amount+9, time.Now())
			l.AssetCode = "USD"
			return m4Imp{}, []statement.PaymentStatementLine{l}
		}},
		{"only amount and asset are right (another merchant reference, succeeded)", func(p *b11Parked) (m4Imp, []statement.PaymentStatementLine) {
			return m4Imp{}, []statement.PaymentStatementLine{m.line(m4Ref(), other(p), "succeeded", p.fresh.Amount, time.Now())}
		}},
		{"only amount and asset are right (no merchant reference, declined, covering window)", func(p *b11Parked) (m4Imp, []statement.PaymentStatementLine) {
			return cover(p), []statement.PaymentStatementLine{m.line(m4Ref(), "", "declined", p.fresh.Amount, time.Now())}
		}},
		{"only the status is right (succeeded, nothing else matches)", func(p *b11Parked) (m4Imp, []statement.PaymentStatementLine) {
			l := m.line(m4Ref(), "d7-stranger", "succeeded", p.fresh.Amount+1, time.Now())
			l.AssetCode = "USD"
			return m4Imp{}, []statement.PaymentStatementLine{l}
		}},
		{"only the status is right (declined, nothing else matches)", func(p *b11Parked) (m4Imp, []statement.PaymentStatementLine) {
			l := m.line(m4Ref(), "d7-stranger", "declined", p.fresh.Amount+1, time.Now())
			l.AssetCode = "USD"
			return cover(p), []statement.PaymentStatementLine{l}
		}},
		{"only the reference R is 'plausible' (a fresh PSP-looking id, nothing attributes it)", func(p *b11Parked) (m4Imp, []statement.PaymentStatementLine) {
			return m4Imp{}, []statement.PaymentStatementLine{m.line("psp-"+uuid.NewString(), "", "succeeded", p.fresh.Amount, time.Now())}
		}},
		{"only occurred_at is right (inside the window, otherwise a stranger's line)", func(p *b11Parked) (m4Imp, []statement.PaymentStatementLine) {
			return cover(p), []statement.PaymentStatementLine{m.line(m4Ref(), "d7-stranger", "declined", p.fresh.Amount+3, p.fresh.LastSentAt.Add(time.Minute))}
		}},
		{"only the seal is valid (a sealed import of unrelated lines)", func(p *b11Parked) (m4Imp, []statement.PaymentStatementLine) {
			return m4Imp{}, []statement.PaymentStatementLine{m.line(m4Ref(), "d7-stranger", "succeeded", 1, time.Now())}
		}},
		{"only the coverage window and the source declaration are right (no line of this attempt)", func(p *b11Parked) (m4Imp, []statement.PaymentStatementLine) {
			return cover(p), []statement.PaymentStatementLine{m.line(m4Ref(), "d7-stranger", "declined", 1, time.Now())}
		}},
		{"everything is right except the seal (unsealed import, perfect succeeded line)", func(p *b11Parked) (m4Imp, []statement.PaymentStatementLine) {
			return m4Imp{unsealed: true}, []statement.PaymentStatementLine{m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", p.fresh.Amount, time.Now())}
		}},
		{"everything is right except the seal (unsealed import, perfect declined line, covering window)", func(p *b11Parked) (m4Imp, []statement.PaymentStatementLine) {
			o := cover(p)
			o.unsealed = true
			return o, []statement.PaymentStatementLine{m.line("m4-D-"+uuid.NewString()[:12], p.fresh.MerchantReference, "declined", p.fresh.Amount, time.Now())}
		}},
		{"everything is right except the source declaration (perfect declined line)", func(p *b11Parked) (m4Imp, []statement.PaymentStatementLine) {
			o := cover(p)
			o.noDecl = true
			return o, []statement.PaymentStatementLine{m.line(m4Ref(), p.fresh.MerchantReference, "declined", p.fresh.Amount, time.Now())}
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m.t = t
			p := m.park(int64(760 + i))
			imp, lines := c.build(p)
			m.ingest(imp, lines...)
			m.d7NonPositive(p, M4VerdictInsufficient, M4VerdictContradictory)
			m.d7BothKindsRefusedAtRequest(p)
		})
	}
}

// Part 2: start from a fully valid positive baseline and corrupt exactly ONE provider-controlled field.
// Each row states the outcome; "positive_by_design" rows are fields the verdict OUTPUTS rather than
// checks (R) and are explained in ADR 0111 s25; every other row must end with NO M4 possible.
func TestD7_Meta_NoSingleProviderFieldIsSufficient_OneFieldCorrupted(t *testing.T) {
	m := newM4World(t)
	type outcome int
	const (
		dbNonPositive outcome = iota // the database verdict is insufficient / contradictory
		goRefuses                    // the database verdict is positive; the Go eligibility refusal ends it
		sealRefuses                  // the database verdict is positive; the Go seal verification ends it
		positiveByDesign
		importRefused // the importer's CHECK refuses to store the line at all
	)
	type tc struct {
		name   string
		mutate func(p *b11Parked, l *statement.PaymentStatementLine, o *m4Imp)
		want   outcome
	}
	cases := []tc{
		{"provider_reference -> a fresh PSP reference (R is the verdict's OUTPUT)", func(p *b11Parked, l *statement.PaymentStatementLine, o *m4Imp) {
			l.ProviderReference = "psp-" + uuid.NewString()
		}, positiveByDesign},
		{"provider_reference -> the platform merchant reference", func(p *b11Parked, l *statement.PaymentStatementLine, o *m4Imp) {
			l.ProviderReference = p.fresh.MerchantReference
		}, goRefuses},
		{"provider_reference -> a reserved-prefix value", func(p *b11Parked, l *statement.PaymentStatementLine, o *m4Imp) {
			l.ProviderReference = m.rowString(`SELECT payment_reserved_ref_prefix()`) + "x"
		}, importRefused},
		{"merchant_reference -> empty", func(p *b11Parked, l *statement.PaymentStatementLine, o *m4Imp) { l.MerchantReference = "" }, dbNonPositive},
		{"merchant_reference -> a stranger's", func(p *b11Parked, l *statement.PaymentStatementLine, o *m4Imp) { l.MerchantReference = "d7-stranger" }, dbNonPositive},
		{"merchant_reference -> another park's", func(p *b11Parked, l *statement.PaymentStatementLine, o *m4Imp) {
			l.MerchantReference = m.park(p.fresh.Amount).fresh.MerchantReference
		}, dbNonPositive},
		{"amount + 1", func(p *b11Parked, l *statement.PaymentStatementLine, o *m4Imp) { l.Amount++ }, dbNonPositive},
		{"asset -> USD", func(p *b11Parked, l *statement.PaymentStatementLine, o *m4Imp) { l.AssetCode = "USD" }, dbNonPositive},
		{"status -> pending", func(p *b11Parked, l *statement.PaymentStatementLine, o *m4Imp) { l.Status = "pending" }, dbNonPositive},
		{"status -> reversed", func(p *b11Parked, l *statement.PaymentStatementLine, o *m4Imp) { l.Status = "reversed" }, dbNonPositive},
		{"status -> declined (window too short for not-paid)", func(p *b11Parked, l *statement.PaymentStatementLine, o *m4Imp) { l.Status = "declined" }, dbNonPositive},
		{"occurred_at -> before the attempt existed", func(p *b11Parked, l *statement.PaymentStatementLine, o *m4Imp) {
			l.OccurredAt = p.fresh.CreatedAt.Add(-24 * time.Hour)
		}, goRefuses},
		{"occurred_at -> after the import's coverage", func(p *b11Parked, l *statement.PaymentStatementLine, o *m4Imp) {
			l.OccurredAt = time.Now().Add(96 * time.Hour)
		}, goRefuses},
		{"import seal -> absent", func(p *b11Parked, l *statement.PaymentStatementLine, o *m4Imp) { o.unsealed = true }, dbNonPositive},
		{"import seal -> made with a key this process does not hold", func(p *b11Parked, l *statement.PaymentStatementLine, o *m4Imp) { o.keys = m4NewKeys(t) }, sealRefuses},
		{"source -> a non-MOCK (real-shaped) source", func(p *b11Parked, l *statement.PaymentStatementLine, o *m4Imp) { o.real = true }, goRefuses},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m.t = t
			amount := int64(780 + i)
			p := m.park(amount)
			l := m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", amount, time.Now())
			imp := m4Imp{}
			c.mutate(p, &l, &imp)
			if c.want == importRefused {
				if _, err := m.ingestErr(imp, l); k3Code(err) != "23514" {
					t.Fatalf("want the CHECK violation, got %v", err)
				}
				m.d7NonPositive(p, M4VerdictInsufficient)
				return
			}
			m.ingest(imp, l)
			switch c.want {
			case dbNonPositive:
				m.d7NonPositive(p, M4VerdictInsufficient, M4VerdictContradictory)
				m.d7BothKindsRefusedAtRequest(p)
			case goRefuses:
				ev := m.mustEvidence(p.fresh.ID, M4VerdictPaid)
				_, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID))
				if !errors.Is(err, ErrResolutionEvidenceNotEligible) {
					t.Fatalf("want the Go eligibility refusal, got %v", err)
				}
				m.d7Held(p, "go refusal")
			case sealRefuses:
				ev := m.mustEvidence(p.fresh.ID, M4VerdictPaid)
				_, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID))
				if !errors.Is(err, ErrResolutionEvidenceUnsealed) {
					t.Fatalf("want the seal refusal, got %v", err)
				}
				m.d7Held(p, "seal refusal")
			case positiveByDesign:
				// The only thing that changed is the PSP's own reference: the verdict is still positive and
				// pins THAT reference (exclusive: held by no other attempt, ledger row, tombstone or Y), and
				// it is the operator's four-eyes portal confirmation - not a machine check - that ties R to
				// the PSP. The ADR names this the residual of D-7 property 3 for an unbound attempt.
				ev := m.mustEvidence(p.fresh.ID, M4VerdictPaid)
				if ev.Reference == nil || *ev.Reference != l.ProviderReference {
					t.Fatalf("the verdict must pin the evidenced reference, got %+v", ev)
				}
			}
		})
	}
	t.Run("provider_id -> another provider's: the fetch path refuses the whole statement (INV-IO-14), nothing is stored", func(t *testing.T) {
		m.t = t
		p := m.park(799)
		before := m.countRows(`SELECT count(*) FROM payment_statement_lines WHERE tenant_id = $1`, m.f.tenantID)
		l := m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", 799, time.Now())
		l.ProviderID = "another-provider"
		src := m4MockSource{m4Source{k3Source: k3Source{provider: m.provider, start: time.Now().Add(-time.Hour), end: time.Now().Add(time.Minute), lines: []statement.PaymentStatementLine{l}}, carries: true}}
		_, err := reconciliation.FetchPaymentStatement(context.Background(), src, m.f.tenantID, time.Now().Add(-time.Hour), time.Now(), reconciliation.PaymentStatementOptions{})
		if !errors.Is(err, reconciliation.ErrPaymentStatementInvalid) {
			t.Fatalf("a statement carrying a line of another provider was accepted: %v", err)
		}
		if after := m.countRows(`SELECT count(*) FROM payment_statement_lines WHERE tenant_id = $1`, m.f.tenantID); after != before {
			t.Fatalf("a refused statement stored %d line(s)", after-before)
		}
		m.d7NonPositive(p, M4VerdictInsufficient)
	})
}

// --- Ambiguity parks: no automatic pay, no automatic resolve --------------------------------------

func TestD7_Ambiguity_ParksWithoutAutomaticPayOrResolve(t *testing.T) {
	m := newM4World(t)
	type tc struct {
		name  string
		setup func(p *b11Parked, q *b11Parked)
		want  []string
	}
	now := time.Now()
	cases := []tc{
		{"two plausible attempts for one reference-only succeeded line", func(p, q *b11Parked) {
			m.ingest(m4Imp{}, m.line(m4Ref(), "", "succeeded", p.fresh.Amount, now))
		}, []string{M4VerdictInsufficient}},
		{"one reference naming both merchant references", func(p, q *b11Parked) {
			r := m4Ref()
			m.ingest(m4Imp{}, m.line(r, p.fresh.MerchantReference, "succeeded", p.fresh.Amount, now), m.line(r, q.fresh.MerchantReference, "succeeded", p.fresh.Amount, now))
		}, []string{M4VerdictContradictory}},
		{"duplicate succeeded lines of DIFFERENT timestamps on one reference", func(p, q *b11Parked) {
			r := m4Ref()
			m.ingest(m4Imp{}, m.line(r, p.fresh.MerchantReference, "succeeded", p.fresh.Amount, now), m.line(r, p.fresh.MerchantReference, "succeeded", p.fresh.Amount, now.Add(time.Second)))
		}, []string{M4VerdictContradictory}},
		{"succeeded and declined together", func(p, q *b11Parked) {
			m.ingest(m4Imp{}, m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", p.fresh.Amount, now), m.line(m4Ref(), p.fresh.MerchantReference, "declined", p.fresh.Amount, now))
		}, []string{M4VerdictContradictory}},
		{"partial coverage: a decline whose window ends before the settlement horizon", func(p, q *b11Parked) {
			m.ingest(m4Imp{start: p.fresh.CreatedAt.Add(-time.Minute), end: p.fresh.LastSentAt.Add(time.Hour)}, m.line(m4Ref(), p.fresh.MerchantReference, "declined", p.fresh.Amount, now))
		}, []string{M4VerdictInsufficient}},
		{"partial coverage: a decline whose window starts after the attempt", func(p, q *b11Parked) {
			m.ingest(m4Imp{start: p.fresh.CreatedAt.Add(time.Hour), end: p.fresh.LastSentAt.Add(30 * time.Hour)}, m.line(m4Ref(), p.fresh.MerchantReference, "declined", p.fresh.Amount, now))
		}, []string{M4VerdictInsufficient}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m.t = t
			amount := int64(800 + i)
			p, q := m.park(amount), m.park(amount)
			c.setup(p, q)
			m.d7NonPositive(p, c.want...)
			m.d7BothKindsRefusedAtRequest(p)
			// No automatic resolve: the sweeper pass and a reconciliation run change nothing; the
			// reconciliation run RAISES the existing standing finding when a succeeded line exists.
			m.sweep()
			m.d7Held(p, "after the sweeper")
			m.d7Held(q, "after the sweeper (the other park)")
		})
	}
	t.Run("an ambiguous succeeded line raises the existing standing finding and pays nothing", func(t *testing.T) {
		m.t = t
		p := m.park(820)
		r := m4Ref()
		l1 := m.line(r, p.fresh.MerchantReference, "succeeded", 820, time.Now().Add(-time.Minute))
		l2 := m.line(m4Ref(), p.fresh.MerchantReference, "declined", 820, time.Now().Add(-time.Minute))
		ms := m.b11Recon(m.source(false, l1, l2))
		if len(m.b11CU(ms, p.fresh.ID)) == 0 {
			t.Fatalf("an ambiguous park with a succeeded line raised no pay_captured_unposted:\n%s", render(ms))
		}
		m.d7Held(p, "after reconciliation")
		m.assertInvariants()
	})
}
