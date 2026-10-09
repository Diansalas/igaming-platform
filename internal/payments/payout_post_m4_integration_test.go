//go:build integration

// ADR 0111 section 4.8 / section 20 (F-1): the post-resolution signal cells, against MOCK only.
//
//	succeeded callback / poll on a disputed attempt whose withdrawal was `failed` by an executed m4_evidence_not_paid
//	    -> success_after_m4_not_paid
//	declined callback / poll after an executed m4_evidence_paid
//	    -> contradiction_after_m4_paid
//
// Oracle of every test: one audit row (payments.payout_post_m4_contradiction) per new receipt, then ONE open B12 P1
// (existing Kind, discriminator payout_attempt:<id>:reason:<reason>, attribute provider_id only) as the last statement;
// nothing changes state, posts, releases or schedules. The cells fire only after an EXECUTED M4 of the matching kind.
package payments

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/capability"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// pm4NewWorldOn is newM4World on an existing pool: a second tenant in the same database (isolation tests).
func pm4NewWorldOn(t *testing.T, pool *db.Pool) *m4World {
	t.Helper()
	w := newK3WorldOn(t, pool, k3Opts{base: 1})
	keys := m4NewKeys(t)
	w.svc.WithImportSealKeys(keys)
	m := &m4World{k3World: w, keys: keys}
	m.acting3 = w.staffMember(uuid.Nil, "platform_admin")
	w.grantActing(m.acting3, capability.CapabilityPaymentForceResolveApprove)
	return m
}

// pm4Callback delivers a payout receipt for the attempt (resolved through its merchant reference) inside the webhook-shaped
// alerting.InTx owner and flushes its Pending after the commit.
func (m *m4World) pm4Callback(att PaymentAttempt, outcome Outcome, ref string, amount int64, echo *payoutinstrument.DestinationEcho) (ReceiptDisposition, error) {
	var disp ReceiptDisposition
	pending, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(m.pool, m.f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		disp, err = ApplyReceiptEvidence(ctx, tx, m.orch, m.f.tenantID, m.provider, ReceiptEvidence{
			EventType: "payout", ProviderReference: ref, MerchantReference: att.MerchantReference,
			Outcome: outcome, Amount: amount, AssetCode: "EUR", DeclineReason: "insufficient_funds", DestinationEcho: echo,
		})
		return err
	})
	if err == nil {
		pending.Flush(context.Background())
	}
	return disp, err
}

func (m *m4World) pm4MustCallback(att PaymentAttempt, outcome Outcome, ref string, amount int64) ReceiptDisposition {
	m.t.Helper()
	d, err := m.pm4Callback(att, outcome, ref, amount, nil)
	if err != nil {
		m.t.Fatalf("callback %s: %v", outcome, err)
	}
	return d
}

// pm4Poll applies a QueryStatus-shaped result through the production evidence transaction (applyPayoutStatusEvidence).
func (m *m4World) pm4Poll(att PaymentAttempt, class ErrorClass, res StatusResult) error {
	return applyPayoutStatusEvidence(context.Background(), m.pool, m.f.tenantID, *att.WithdrawalRequestID, att,
		GateResult[StatusResult]{Class: class, Value: res}, EvidenceQueryStatus, time.Now().Add(time.Minute), nil, m.orch.PayoutOptions()...)
}

func (m *m4World) pm4Audits(attemptID uuid.UUID) int {
	return m.countRows(`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3`,
		m.f.tenantID, auditActionPayoutPostM4Contradiction, attemptID.String())
}

func (m *m4World) pm4Alerts() []alertinject.Row {
	return alertinject.ForSubject(m.t, m.pool, m.f.tenantID)
}

// pm4PostM4Alerts are the alerts of the two new reasons.
func (m *m4World) pm4PostM4Alerts(attemptID uuid.UUID) []alertinject.Row {
	var out []alertinject.Row
	for _, r := range m.pm4Alerts() {
		for _, reason := range []string{alertReasonPayoutSuccessAfterM4NotPaid, alertReasonPayoutContradictionAfterM4Paid} {
			if r.Discriminator == "payout_attempt:"+attemptID.String()+":reason:"+reason {
				out = append(out, r)
			}
		}
	}
	return out
}

// pm4WantNoSignal: no audit row, no alert of the two reasons.
func (m *m4World) pm4WantNoSignal(att PaymentAttempt, what string) {
	m.t.Helper()
	if n := m.pm4Audits(att.ID); n != 0 {
		m.t.Fatalf("%s: %d post-M4 audit rows, want 0", what, n)
	}
	if rows := m.pm4PostM4Alerts(att.ID); len(rows) != 0 {
		m.t.Fatalf("%s: post-M4 alerts raised: %+v", what, rows)
	}
}

// pm4WantSignal asserts exactly wantAudits audit rows with the exact closed context, and ONE open P1 with wantOcc
// occurrences, carrying only provider_id and none of the probes.
func (m *m4World) pm4WantSignal(att PaymentAttempt, reason, m4Kind, observed, evidence string, wantAudits, wantOcc int, probes ...string) {
	m.t.Helper()
	if n := m.pm4Audits(att.ID); n != wantAudits {
		m.t.Fatalf("%s: audit rows = %d, want %d", reason, n, wantAudits)
	}
	rows := m.sysQuery(`SELECT actor_type, outcome, target_type, metadata FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3 ORDER BY created_at, id`,
		m.f.tenantID, auditActionPayoutPostM4Contradiction, att.ID.String())
	for _, r := range rows {
		if r["actor_type"] != "system" || r["outcome"] != "denied" || r["target_type"] != "payment_attempt" {
			m.t.Fatalf("audit shape: %v", r)
		}
		md, _ := r["metadata"].(map[string]any)
		if md["reason"] != reason || md["m4_kind"] != m4Kind || md["observed"] != observed || (evidence != "" && md["evidence"] != evidence) ||
			md["provider_id"] != m.provider || md["withdrawal_request_id"] != att.WithdrawalRequestID.String() || md["attempt_state"] != "disputed" ||
			md["m4_resolution_id"] == nil {
			m.t.Fatalf("audit metadata: %v", md)
		}
		blob := fmt.Sprint(md)
		for _, p := range probes {
			if p != "" && strings.Contains(strings.ReplaceAll(blob, att.ID.String(), ""), p) && p != md["echoed_provider_reference"] {
				m.t.Fatalf("audit metadata leaks %q: %s", p, blob)
			}
		}
	}
	al := m.pm4PostM4Alerts(att.ID)
	if len(al) != 1 {
		m.t.Fatalf("%s: want exactly one alert, got %+v", reason, al)
	}
	a := al[0]
	if a.Kind != string(alerting.KindPaymentWebhookIntegrity) || a.Severity != "p1" || a.State != "open" || a.Occurrences != wantOcc ||
		a.Discriminator != "payout_attempt:"+att.ID.String()+":reason:"+reason {
		m.t.Fatalf("alert: %+v (want %d occurrences)", a, wantOcc)
	}
	if len(a.Attributes) != 1 || a.Attributes["provider_id"] != m.provider {
		m.t.Fatalf("alert attributes %v, want provider_id only", a.Attributes)
	}
	for _, p := range probes {
		if p != "" && strings.Contains(strings.ReplaceAll(fmt.Sprint(a.Attributes, a.Discriminator), att.ID.String(), ""), p) {
			m.t.Fatalf("alert leaks %q", p)
		}
	}
}

// pm4Unchanged: the cell changed nothing (state, balances, ledger, schedule) against the snapshot taken after the M4.
func (m *m4World) pm4Unchanged(p *b11Parked, base b11Snap, what string) {
	m.t.Helper()
	if now := m.b11Snap(p.wr.ID, p.fresh.ID); now != base {
		m.t.Fatalf("%s: state moved\n before: %+v\n  after: %+v", what, base, now)
	}
	m.assertInvariants()
}

// ---- scenarios -----------------------------------------------------------------------------------------------------

type pm4Scenario struct {
	name     string
	m4Kind   ResolutionKind
	observed Outcome
	reason   string
	amount   int64
}

var pm4Signals = []pm4Scenario{
	{"not_paid_then_success", ResolutionM4EvidenceNotPaid, OutcomeSucceeded, alertReasonPayoutSuccessAfterM4NotPaid, 650},
	{"paid_then_decline", ResolutionM4EvidencePaid, OutcomeDeclined, alertReasonPayoutContradictionAfterM4Paid, 700},
}

// pm4Executed returns a parked attempt with the M4 EXECUTED, and the snapshot taken right after.
func (m *m4World) pm4Executed(kind ResolutionKind, amount int64) (*b11Parked, b11Snap) {
	m.t.Helper()
	var p *b11Parked
	if kind == ResolutionM4EvidencePaid {
		var ev M4Evidence
		p, _, ev = m.paidPark(amount)
		m.execute(p, kind, ev)
	} else {
		var ev M4Evidence
		p, ev = m.notPaidPark(amount)
		m.execute(p, kind, ev)
	}
	return p, m.b11Snap(p.wr.ID, p.fresh.ID)
}

// Both cells in the CALLBACK path: audit row with exact context, one open P1, nothing moved; the attempt stays disputed.
func TestPostM4_Callback_BothCells_OneAuditRowOneAlert_NothingMoves(t *testing.T) {
	for _, sc := range pm4Scenarios(t) {
		t.Run(sc.name, func(t *testing.T) {
			m := newM4World(t)
			p, base := m.pm4Executed(sc.m4Kind, sc.amount)
			ref := "pm4-ev-" + uuid.NewString()[:12]
			disp := m.pm4MustCallback(p.fresh, sc.observed, ref, sc.amount)
			if disp != DispositionDuplicateEffect {
				t.Fatalf("disposition = %s, want %s (no state change)", disp, DispositionDuplicateEffect)
			}
			m.pm4WantSignal(p.fresh, sc.reason, string(sc.m4Kind), string(sc.observed), string(EvidenceCallback), 1, 1, "insufficient_funds")
			m.pm4Unchanged(p, base, "callback cell")
			a := m.attempt(p.fresh.ID)
			if a.State != AttemptDisputed || !equalOptString(a.TerminalReason, p.fresh.TerminalReason) || a.ProviderReference != nil {
				t.Fatalf("attempt changed: %s %v %v", a.State, a.TerminalReason, a.ProviderReference)
			}
			// The receipt was stored once and resolved to the attempt.
			if n := m.countRows(`SELECT count(*) FROM payment_provider_events WHERE tenant_id = $1 AND attempt_id = $2 AND resolved_at IS NOT NULL`, m.f.tenantID, p.fresh.ID); n != 1 {
				t.Fatalf("resolved receipts for the attempt = %d, want 1", n)
			}
		})
	}
}

func pm4Scenarios(t *testing.T) []pm4Scenario {
	t.Helper()
	return pm4Signals
}

// Both cells in the POLL path (the production evidence transaction applyPayoutStatusEvidence).
func TestPostM4_Poll_BothCells_OneAuditRowOneAlert_NothingMoves(t *testing.T) {
	for _, sc := range pm4Scenarios(t) {
		t.Run(sc.name, func(t *testing.T) {
			m := newM4World(t)
			p, base := m.pm4Executed(sc.m4Kind, sc.amount)
			class := ErrorClassSucceeded
			if sc.observed == OutcomeDeclined {
				class = ErrorClassDefiniteDecline
			}
			ref := "pm4-poll-" + uuid.NewString()[:12]
			if err := m.pm4Poll(m.attempt(p.fresh.ID), class, StatusResult{Outcome: sc.observed, ProviderReference: ref, Amount: sc.amount, AssetCode: "EUR", DeclineReason: "insufficient_funds"}); err != nil {
				t.Fatalf("poll: %v", err)
			}
			m.pm4WantSignal(p.fresh, sc.reason, string(sc.m4Kind), string(sc.observed), string(EvidenceQueryStatus), 1, 1, "insufficient_funds")
			m.pm4Unchanged(p, base, "poll cell")
		})
	}
}

// The destination_mismatch park composes: M4 not-paid leaves the attempt disputed (destination_mismatch); a real
// PollPayoutStatus then finds the payout PAID under the bound reference -> success_after_m4_not_paid. The destination
// echo cells add nothing on a disputed attempt (the §4.8 cell is the one signal).
func TestPostM4_DestinationMismatchPark_NotPaid_ThenPoll_Succeeded_Signals(t *testing.T) {
	m := newM4World(t)
	wr, a := m.payout(640)
	m.tx(func(ctx context.Context, tx pgx.Tx) error {
		return ApplyDisputeFromNonTerminal(ctx, tx, a.ID, EvidenceCallback, "destination_mismatch")
	})
	a = m.attempt(a.ID)
	x := *a.ProviderReference
	m.ingest(m4Imp{start: a.CreatedAt.Add(-time.Minute), end: a.LastSentAt.Add(25 * time.Hour)},
		m.line(x, a.MerchantReference, statement.PaymentStatusDeclined, 640, time.Now()))
	ev := m.mustEvidence(a.ID, M4VerdictNotPaid)
	// M4 PAID is refused for a destination_mismatch park (the DB scope), so contradiction_after_m4_paid cannot occur here.
	if _, err := m.request(m.acting, m.m4In(a.ID, ResolutionM4EvidencePaid, ev.LineID)); err == nil {
		t.Fatal("an M4 paid request for a destination_mismatch park must be refused")
	}
	// A poll BEFORE the M4 (no executed M4): the disputed park stays a no-op (replay idempotence).
	m.prov.setStatus(x, StatusResult{Outcome: OutcomeSucceeded, ProviderReference: x, Amount: 640, AssetCode: "EUR"})
	if err := PollPayoutStatus(context.Background(), m.pool, m.orch, MockCredentialResolver{}, m.f.tenantID, m.attempt(a.ID), time.Now().Add(time.Minute), nil); err != nil {
		t.Fatalf("poll before M4: %v", err)
	}
	m.pm4WantNoSignal(a, "disputed destination park, no executed M4")

	r, err := m.request(m.acting, m.m4In(a.ID, ResolutionM4EvidenceNotPaid, ev.LineID))
	k3RequireNoErr(t, err, "request")
	out, err := m.decide(m.acting2, r, ResolutionApprove)
	k3RequireNoErr(t, err, "approve")
	if !out.Executed || m.wd(wr.ID).State != withdrawal.StateFailed {
		t.Fatalf("M4 not executed: %+v", out)
	}
	pp := &b11Parked{wr: m.wd(wr.ID), fresh: m.attempt(a.ID)}
	base := m.b11Snap(wr.ID, a.ID)
	if pp.fresh.State != AttemptDisputed || pp.fresh.TerminalReason == nil || *pp.fresh.TerminalReason != "destination_mismatch" {
		t.Fatalf("M4 not-paid must leave the destination park disputed: %s %v", pp.fresh.State, pp.fresh.TerminalReason)
	}
	for i := 0; i < 3; i++ {
		if err := PollPayoutStatus(context.Background(), m.pool, m.orch, MockCredentialResolver{}, m.f.tenantID, m.attempt(a.ID), time.Now().Add(time.Minute), nil); err != nil {
			t.Fatalf("poll after M4 #%d: %v", i, err)
		}
	}
	// One audit row for the poll source however often it is polled; one open alert with the occurrences growing.
	m.pm4WantSignal(a, alertReasonPayoutSuccessAfterM4NotPaid, string(ResolutionM4EvidenceNotPaid), "succeeded", string(EvidenceQueryStatus), 1, 3)
	// The recorded context names the destination park.
	rows := m.sysQuery(`SELECT metadata->>'attempt_terminal_reason' AS r FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3`,
		m.f.tenantID, auditActionPayoutPostM4Contradiction, a.ID.String())
	if len(rows) != 1 || rows[0]["r"] != "destination_mismatch" {
		t.Fatalf("audit terminal reason: %v", rows)
	}
	// A callback success carrying a BAD destination echo after the M4: still only the §4.8 cell (no destination re-park,
	// no terminal destination signal, the attempt stays as it is).
	d, err := m.pm4Callback(m.attempt(a.ID), OutcomeSucceeded, "pm4-echo-"+uuid.NewString()[:8], 640,
		&payoutinstrument.DestinationEcho{Kid: "nope", Fingerprint: strings.Repeat("ab", 32)})
	if err != nil || d != DispositionDuplicateEffect {
		t.Fatalf("callback with a bad echo: %v %v", d, err)
	}
	m.pm4WantSignal(a, alertReasonPayoutSuccessAfterM4NotPaid, string(ResolutionM4EvidenceNotPaid), "succeeded", "", 2, 4) // "" = any evidence kind: one poll row and one callback row
	if n := m.countRows(`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND target_id = $2 AND action IN ($3, $4)`,
		m.f.tenantID, a.ID.String(), terminalSignalAuditAction, "payments.payout_parked_destination"); n != 0 {
		t.Fatalf("destination cells fired on a disputed attempt: %d rows", n)
	}
	if now := m.b11Snap(wr.ID, a.ID); now != base {
		t.Fatalf("state moved:\n%+v\n%+v", base, now)
	}
	m.assertInvariants()
}

// No-M4 control: a disputed park (any shape) that receives a success or a decline, by callback or poll, signals nothing:
// the same silent no-op as before this change.
func TestPostM4_NoM4_Control_NoSignal(t *testing.T) {
	m := newM4World(t)
	p := m.park(300)
	base := m.b11Snap(p.wr.ID, p.fresh.ID)
	for i, oc := range []Outcome{OutcomeSucceeded, OutcomeDeclined, OutcomePending, OutcomeAmbiguous} {
		m.pm4MustCallback(p.fresh, oc, fmt.Sprintf("pm4-ctl-%d-%s", i, uuid.NewString()[:8]), 300)
	}
	for _, c := range []ErrorClass{ErrorClassSucceeded, ErrorClassDefiniteDecline, ErrorClassPending, ErrorClassAmbiguous} {
		if err := m.pm4Poll(m.attempt(p.fresh.ID), c, StatusResult{Outcome: OutcomeSucceeded, ProviderReference: "pm4-ctl-poll", Amount: 300, AssetCode: "EUR"}); err != nil {
			t.Fatalf("poll %s: %v", c, err)
		}
	}
	m.pm4WantNoSignal(p.fresh, "no executed M4")
	// (the poll reschedule of the pre-existing code path is not the cell's concern: compare the money and the state only)
	now := m.b11Snap(p.wr.ID, p.fresh.ID)
	base.aNextSet, now.aNextSet = false, false
	if now != base {
		t.Fatalf("state moved:\n%+v\n%+v", base, now)
	}
	m.assertInvariants()
}

// The cells fire only after an EXECUTED M4: a pending, rejected, cancelled or refused M4 gives no signal.
func TestPostM4_OnlyAfterExecutedM4_PendingRefusedRejectedCancelled_NoSignal(t *testing.T) {
	type tc struct {
		name string
		kind ResolutionKind
		arm  func(m *m4World, p *b11Parked, ev M4Evidence)
	}
	for _, c := range []tc{
		{"pending", ResolutionM4EvidenceNotPaid, func(m *m4World, p *b11Parked, ev M4Evidence) {
			_, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidenceNotPaid, ev.LineID))
			k3RequireNoErr(t, err, "request")
		}},
		{"refused_at_execution", ResolutionM4EvidenceNotPaid, func(m *m4World, p *b11Parked, ev M4Evidence) {
			r, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidenceNotPaid, ev.LineID))
			k3RequireNoErr(t, err, "request")
			m.ingest(m4Imp{unsealed: true}, m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", 650, time.Now()))
			out, err := m.decide(m.acting2, r, ResolutionApprove)
			k3RequireNoErr(t, err, "approve")
			if out.Executed || !out.Refused {
				t.Fatalf("want refused, got %+v", out)
			}
		}},
		{"rejected", ResolutionM4EvidenceNotPaid, func(m *m4World, p *b11Parked, ev M4Evidence) {
			r, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidenceNotPaid, ev.LineID))
			k3RequireNoErr(t, err, "request")
			if _, err := m.decide(m.acting2, r, ResolutionReject); err != nil {
				t.Fatalf("reject: %v", err)
			}
		}},
		{"cancelled", ResolutionM4EvidenceNotPaid, func(m *m4World, p *b11Parked, ev M4Evidence) {
			r, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidenceNotPaid, ev.LineID))
			k3RequireNoErr(t, err, "request")
			if _, err := m.svc.Cancel(k3Ctx(m.acting), m.target(m.acting), r.ID, ResolutionMeta{RequestID: "pm4"}); err != nil {
				t.Fatalf("cancel: %v", err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newM4World(t)
			p, ev := m.notPaidPark(650)
			c.arm(m, p, ev)
			if wr := m.wd(p.wr.ID); wr.State != withdrawal.StateSubmitted {
				t.Fatalf("setup: the hold must be kept, got %s", wr.State)
			}
			m.pm4MustCallback(p.fresh, OutcomeSucceeded, "pm4-np-"+uuid.NewString()[:8], 650)
			if err := m.pm4Poll(m.attempt(p.fresh.ID), ErrorClassSucceeded, StatusResult{Outcome: OutcomeSucceeded, ProviderReference: "pm4-np-poll", Amount: 650, AssetCode: "EUR"}); err != nil {
				t.Fatalf("poll: %v", err)
			}
			m.pm4WantNoSignal(p.fresh, c.name)
			if wr := m.wd(p.wr.ID); wr.State != withdrawal.StateSubmitted || wr.ReleaseLedgerTransactionID != nil {
				t.Fatalf("withdrawal moved: %+v", wr)
			}
			m.assertInvariants()
		})
	}
}

// Evidence that AGREES with the executed M4 is not a contradiction: a decline after not-paid, a success after paid, and a
// non-definite outcome after either.
func TestPostM4_AgreeingEvidence_NoSignal(t *testing.T) {
	for _, sc := range pm4Scenarios(t) {
		t.Run(sc.name, func(t *testing.T) {
			m := newM4World(t)
			p, base := m.pm4Executed(sc.m4Kind, sc.amount)
			agree := OutcomeDeclined
			agreeClass := ErrorClassDefiniteDecline
			if sc.observed == OutcomeDeclined {
				agree, agreeClass = OutcomeSucceeded, ErrorClassSucceeded
			}
			m.pm4MustCallback(p.fresh, agree, "pm4-ag-"+uuid.NewString()[:8], sc.amount)
			m.pm4MustCallback(p.fresh, OutcomePending, "pm4-ag-p-"+uuid.NewString()[:8], sc.amount)
			m.pm4MustCallback(p.fresh, OutcomeAmbiguous, "pm4-ag-a-"+uuid.NewString()[:8], sc.amount)
			for _, c := range []ErrorClass{agreeClass, ErrorClassPending, ErrorClassAmbiguous} {
				if err := m.pm4Poll(m.attempt(p.fresh.ID), c, StatusResult{Outcome: agree, ProviderReference: "pm4-ag-poll", Amount: sc.amount, AssetCode: "EUR"}); err != nil {
					t.Fatalf("poll %s: %v", c, err)
				}
			}
			m.pm4WantNoSignal(p.fresh, "agreeing evidence")
			base.aNextSet = false
			if now := m.b11Snap(p.wr.ID, p.fresh.ID); now != base {
				now.aNextSet = false
				if now != base {
					t.Fatalf("state moved:\n%+v\n%+v", base, now)
				}
			}
			m.assertInvariants()
		})
	}
}

// Replay and redelivery: the byte-identical event again and again gives one audit row (once per NEW receipt) and ONE open
// alert whose occurrences grow; a DIFFERENT event (new receipt) adds exactly one row and one occurrence.
func TestPostM4_Callback_Replay_OnceAuditPerReceipt_OneAlertGrowingOccurrences(t *testing.T) {
	for _, sc := range pm4Scenarios(t) {
		t.Run(sc.name, func(t *testing.T) {
			m := newM4World(t)
			p, base := m.pm4Executed(sc.m4Kind, sc.amount)
			ref := "pm4-rp-" + uuid.NewString()[:10]
			for i := 0; i < 4; i++ {
				m.pm4MustCallback(p.fresh, sc.observed, ref, sc.amount)
			}
			m.pm4WantSignal(p.fresh, sc.reason, string(sc.m4Kind), string(sc.observed), string(EvidenceCallback), 1, 4)
			ref2 := "pm4-rp2-" + uuid.NewString()[:10]
			m.pm4MustCallback(p.fresh, sc.observed, ref2, sc.amount)
			m.pm4MustCallback(p.fresh, sc.observed, ref2, sc.amount)
			m.pm4WantSignal(p.fresh, sc.reason, string(sc.m4Kind), string(sc.observed), string(EvidenceCallback), 2, 6)
			m.pm4Unchanged(p, base, "replays")
			if n := m.countRows(`SELECT count(*) FROM payment_provider_events WHERE tenant_id = $1 AND attempt_id = $2`, m.f.tenantID, p.fresh.ID); n != 2 {
				t.Fatalf("receipts = %d, want 2 (one per distinct event)", n)
			}
		})
	}
}

// An ORPHANED unresolved receipt (stored by a transaction whose commit the binding transaction could not see) redelivered
// after the M4: the delivery is the first application, so it writes the audit row and closes the receipt; further
// redeliveries add nothing.
func TestPostM4_Callback_OrphanedReceipt_FirstApplicationAudits_Once(t *testing.T) {
	m := newM4World(t)
	p, base := m.pm4Executed(ResolutionM4EvidenceNotPaid, 650)
	ref := "pm4-orph-" + uuid.NewString()[:8]
	ev := ReceiptEvidence{EventType: "payout", ProviderReference: ref, MerchantReference: p.fresh.MerchantReference,
		Outcome: OutcomeSucceeded, Amount: 650, AssetCode: "EUR", DeclineReason: "insufficient_funds"}
	if err := m.pool.WithTenant(context.Background(), m.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, dup, err := insertReceiptDeduped(ctx, tx, m.f.tenantID, m.provider, ev, DispositionDeferredUnresolved)
		if err == nil && dup {
			err = fmt.Errorf("setup: unexpectedly a duplicate")
		}
		return err
	}); err != nil {
		t.Fatalf("plant orphan: %v", err)
	}
	for i := 0; i < 3; i++ {
		m.pm4MustCallback(p.fresh, OutcomeSucceeded, ref, 650)
		if n := m.pm4Audits(p.fresh.ID); n != 1 {
			t.Fatalf("after delivery #%d audit rows = %d, want 1", i, n)
		}
	}
	m.pm4WantSignal(p.fresh, alertReasonPayoutSuccessAfterM4NotPaid, string(ResolutionM4EvidenceNotPaid), "succeeded", string(EvidenceCallback), 1, 3)
	if n := m.countRows(`SELECT count(*) FROM payment_provider_events WHERE tenant_id = $1 AND provider_reference = $2 AND resolved_at IS NULL`, m.f.tenantID, ref); n != 0 {
		t.Fatalf("the orphaned receipt must be resolved, %d unresolved", n)
	}
	m.pm4Unchanged(p, base, "orphan")
}

// Concurrency: racing duplicate deliveries of the same event give exactly one audit row, one open alert with one
// occurrence per delivery, one receipt, and no state or money change; the same for racing polls.
func TestPostM4_Concurrency_RacingDuplicateDeliveries_OneAuditRow(t *testing.T) {
	const n = 8
	for _, sc := range pm4Scenarios(t) {
		t.Run(sc.name+"/callback", func(t *testing.T) {
			m := newM4World(t)
			p, base := m.pm4Executed(sc.m4Kind, sc.amount)
			ref := "pm4-cc-" + uuid.NewString()[:10]
			var wg sync.WaitGroup
			errs := make(chan error, n)
			start := make(chan struct{})
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, err := m.pm4Callback(p.fresh, sc.observed, ref, sc.amount, nil)
					errs <- err
				}()
			}
			close(start)
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatalf("racing delivery: %v", err)
				}
			}
			m.pm4WantSignal(p.fresh, sc.reason, string(sc.m4Kind), string(sc.observed), string(EvidenceCallback), 1, n)
			if got := m.countRows(`SELECT count(*) FROM payment_provider_events WHERE tenant_id = $1 AND attempt_id = $2`, m.f.tenantID, p.fresh.ID); got != 1 {
				t.Fatalf("receipts = %d, want 1", got)
			}
			m.pm4Unchanged(p, base, "racing deliveries")
		})
		t.Run(sc.name+"/poll", func(t *testing.T) {
			m := newM4World(t)
			p, base := m.pm4Executed(sc.m4Kind, sc.amount)
			class := ErrorClassSucceeded
			if sc.observed == OutcomeDeclined {
				class = ErrorClassDefiniteDecline
			}
			var wg sync.WaitGroup
			errs := make(chan error, n)
			start := make(chan struct{})
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					errs <- m.pm4Poll(m.attempt(p.fresh.ID), class, StatusResult{Outcome: sc.observed, ProviderReference: "pm4-ccp", Amount: sc.amount, AssetCode: "EUR", DeclineReason: "insufficient_funds"})
				}()
			}
			close(start)
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatalf("racing poll: %v", err)
				}
			}
			m.pm4WantSignal(p.fresh, sc.reason, string(sc.m4Kind), string(sc.observed), string(EvidenceQueryStatus), 1, n)
			m.pm4Unchanged(p, base, "racing polls")
		})
	}
}

// Tenant isolation, both directions, with two tenants in one database each holding its own executed M4:
//   - tenant B's executed M4 not-paid cannot TRIGGER a signal for tenant A's disputed attempt that has none;
//   - tenant A's own executed M4 still signals (B's M4 cannot SUPPRESS it), and B's attempt signals independently;
//   - a delivery in tenant B's session naming tenant A's merchant reference resolves nothing and touches nothing of A's.
func TestPostM4_TenantIsolation_AnotherTenantsM4_CannotTriggerOrSuppress(t *testing.T) {
	a := newM4World(t)
	b := pm4NewWorldOn(t, a.pool)

	// A: an UNRESOLVED park (no M4). B: an EXECUTED not-paid M4 on its own park.
	pa := a.park(300)
	pb, baseB := b.pm4Executed(ResolutionM4EvidenceNotPaid, 650)

	a.pm4MustCallback(pa.fresh, OutcomeSucceeded, "pm4-iso-a-"+uuid.NewString()[:8], 300)
	if err := a.pm4Poll(a.attempt(pa.fresh.ID), ErrorClassSucceeded, StatusResult{Outcome: OutcomeSucceeded, ProviderReference: "pm4-iso-ap", Amount: 300, AssetCode: "EUR"}); err != nil {
		t.Fatalf("poll: %v", err)
	}
	a.pm4WantNoSignal(pa.fresh, "tenant A has no executed M4 (B has one)")
	if rows := a.pm4Alerts(); len(rows) != 0 {
		// the park itself raised its own B12 alert; only the two new reasons are excluded by pm4WantNoSignal
		for _, r := range rows {
			if strings.Contains(r.Discriminator, ":reason:success_after_m4_not_paid") || strings.Contains(r.Discriminator, ":reason:contradiction_after_m4_paid") {
				t.Fatalf("unexpected post-M4 alert in tenant A: %+v", r)
			}
		}
	}

	// B's session delivering an event that names A's merchant reference resolves nothing in B (RLS) and writes nothing in A.
	pending, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(b.pool, b.f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
		_, err := ApplyReceiptEvidence(ctx, tx, b.orch, b.f.tenantID, b.provider, ReceiptEvidence{
			EventType: "payout", ProviderReference: "pm4-iso-x-" + uuid.NewString()[:8], MerchantReference: pa.fresh.MerchantReference,
			Outcome: OutcomeSucceeded, Amount: 300, AssetCode: "EUR"})
		return err
	})
	if err != nil {
		t.Fatalf("cross-tenant delivery: %v", err)
	}
	pending.Flush(context.Background())
	a.pm4WantNoSignal(pa.fresh, "B's session naming A's reference")
	if b.pm4Audits(pa.fresh.ID) != 0 {
		t.Fatal("tenant B read or wrote a post-M4 row for A's attempt")
	}

	// A now executes its OWN M4; B's M4 neither suppresses nor duplicates A's signal.
	pa2, baseA := a.pm4Executed(ResolutionM4EvidencePaid, 700)
	a.pm4MustCallback(pa2.fresh, OutcomeDeclined, "pm4-iso-a2-"+uuid.NewString()[:8], 700)
	a.pm4WantSignal(pa2.fresh, alertReasonPayoutContradictionAfterM4Paid, string(ResolutionM4EvidencePaid), "declined", string(EvidenceCallback), 1, 1)
	a.pm4Unchanged(pa2, baseA, "tenant A")
	// B's own signal is independent and untouched by A's activity.
	b.pm4MustCallback(pb.fresh, OutcomeSucceeded, "pm4-iso-b-"+uuid.NewString()[:8], 650)
	b.pm4WantSignal(pb.fresh, alertReasonPayoutSuccessAfterM4NotPaid, string(ResolutionM4EvidenceNotPaid), "succeeded", string(EvidenceCallback), 1, 1)
	b.pm4Unchanged(pb, baseB, "tenant B")
	// Each tenant sees only its own rows.
	if n := len(a.pm4PostM4Alerts(pb.fresh.ID)); n != 0 {
		t.Fatalf("tenant A sees B's alert: %d", n)
	}
	if n := len(b.pm4PostM4Alerts(pa2.fresh.ID)); n != 0 {
		t.Fatalf("tenant B sees A's alert: %d", n)
	}
}

// Partial failure. A TRANSIENT alert failure fails the delivery and rolls the whole transaction back (audit row, receipt
// and alert all absent: the raise is the last statement, so nothing after it can be lost and nothing before it commits
// alone); a DETERMINISTIC alert failure (P0001) never rolls the cell back: the audit row and the receipt commit, the
// swallowed raise is retried detached by the Pending, and the state is untouched either way.
func TestPostM4_PartialFailure_TransientRollsBackAll_DeterministicKeepsAudit(t *testing.T) {
	t.Run("transient_rolls_back_everything", func(t *testing.T) {
		m := newM4World(t)
		p, base := m.pm4Executed(ResolutionM4EvidenceNotPaid, 650)
		ref := "pm4-pf-" + uuid.NewString()[:8]
		alertinject.Install(t, m.pool, m.f.tenantID, alertinject.Persistent, "40P01")
		if _, err := m.pm4Callback(p.fresh, OutcomeSucceeded, ref, 650, nil); err == nil {
			t.Fatal("a transient alert failure must fail the delivery")
		}
		if n := m.pm4Audits(p.fresh.ID); n != 0 {
			t.Fatalf("audit row survived the rollback: %d", n)
		}
		if n := m.countRows(`SELECT count(*) FROM payment_provider_events WHERE tenant_id = $1 AND provider_reference = $2`, m.f.tenantID, ref); n != 0 {
			t.Fatalf("receipt survived the rollback: %d", n)
		}
		m.pm4Unchanged(p, base, "transient failure")
	})
	t.Run("deterministic_keeps_audit_and_state", func(t *testing.T) {
		m := newM4World(t)
		p, base := m.pm4Executed(ResolutionM4EvidenceNotPaid, 650)
		alertinject.Install(t, m.pool, m.f.tenantID, alertinject.Persistent, "P0001")
		if _, err := m.pm4Callback(p.fresh, OutcomeSucceeded, "pm4-pfd-"+uuid.NewString()[:8], 650, nil); err != nil {
			t.Fatalf("a deterministic alert failure must not fail the delivery: %v", err)
		}
		if n := m.pm4Audits(p.fresh.ID); n != 1 {
			t.Fatalf("the audit row (written before the raise) must commit, got %d", n)
		}
		m.pm4Unchanged(p, base, "deterministic failure")
	})
}

// Deposits are untouched: a disputed DEPOSIT attempt receiving a success or a decline keeps its no-op.
func TestPostM4_Deposit_Untouched(t *testing.T) {
	m := newM4World(t)
	dep := m.disputedDeposit(500)
	for _, oc := range []Outcome{OutcomeSucceeded, OutcomeDeclined} {
		ev := ReceiptEvidence{EventType: "deposit", MerchantReference: dep.MerchantReference, ProviderReference: "pm4-dep-" + uuid.NewString()[:8],
			Outcome: oc, Amount: 500, AssetCode: "EUR", DeclineReason: "insufficient_funds"}
		pending, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(m.pool, m.f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
			_, err := ApplyReceiptEvidence(ctx, tx, m.orch, m.f.tenantID, m.provider, ev)
			return err
		})
		if err != nil {
			t.Fatalf("deposit %s: %v", oc, err)
		}
		pending.Flush(context.Background())
	}
	m.pm4WantNoSignal(dep, "deposit")
}

// The signal is derived from an EXECUTED M4 row, not from the withdrawal's state alone: a withdrawal failed (or completed)
// through some OTHER route on a disputed attempt, with no M4 or with an M4 request that never executed, gives no signal.
func TestPostM4_WithdrawalClosedByAnotherRoute_NoExecutedM4_NoSignal(t *testing.T) {
	t.Run("failed_no_m4", func(t *testing.T) {
		m := newM4World(t)
		p := m.park(300)
		m.tx(func(ctx context.Context, tx pgx.Tx) error {
			return withdrawal.Fail(ctx, tx, p.wr.ID, "pm4_other_route")
		})
		if wr := m.wd(p.wr.ID); wr.State != withdrawal.StateFailed {
			t.Fatalf("setup: want failed, got %s", wr.State)
		}
		m.pm4MustCallback(p.fresh, OutcomeSucceeded, "pm4-or-"+uuid.NewString()[:8], 300)
		if err := m.pm4Poll(m.attempt(p.fresh.ID), ErrorClassSucceeded, StatusResult{Outcome: OutcomeSucceeded, ProviderReference: "pm4-or-p", Amount: 300, AssetCode: "EUR"}); err != nil {
			t.Fatal(err)
		}
		m.pm4WantNoSignal(p.fresh, "failed by another route, no M4")
	})
	t.Run("failed_pending_m4_request", func(t *testing.T) {
		m := newM4World(t)
		p, ev := m.notPaidPark(300)
		_, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidenceNotPaid, ev.LineID))
		k3RequireNoErr(t, err, "request")
		m.tx(func(ctx context.Context, tx pgx.Tx) error {
			return withdrawal.Fail(ctx, tx, p.wr.ID, "pm4_other_route")
		})
		m.pm4MustCallback(p.fresh, OutcomeSucceeded, "pm4-or2-"+uuid.NewString()[:8], 300)
		m.pm4WantNoSignal(p.fresh, "failed by another route, M4 only requested")
	})
	t.Run("completed_no_m4", func(t *testing.T) {
		m := newM4World(t)
		p := m.park(300)
		m.tx(func(ctx context.Context, tx pgx.Tx) error {
			return withdrawal.Complete(ctx, tx, p.wr.ID, m.provider, "pm4-other-"+uuid.NewString()[:8])
		})
		if wr := m.wd(p.wr.ID); wr.State != withdrawal.StateCompleted {
			t.Fatalf("setup: want completed, got %s", wr.State)
		}
		m.pm4MustCallback(p.fresh, OutcomeDeclined, "pm4-or3-"+uuid.NewString()[:8], 300)
		if err := m.pm4Poll(m.attempt(p.fresh.ID), ErrorClassDefiniteDecline, StatusResult{Outcome: OutcomeDeclined, ProviderReference: "pm4-or3-p", Amount: 300, AssetCode: "EUR"}); err != nil {
			t.Fatal(err)
		}
		m.pm4WantNoSignal(p.fresh, "completed by another route, no M4")
	})
	t.Run("wrong_kind_executed", func(t *testing.T) {
		// An executed M4 NOT-PAID followed by a (contradicting-looking) SUCCESS is the not-paid cell; an executed not-paid
		// must never answer the PAID cell, and an executed paid never the NOT-PAID cell: a decline after not-paid and a
		// success after paid were covered by TestPostM4_AgreeingEvidence_NoSignal; here the withdrawal state is the other one.
		m := newM4World(t)
		p, _ := m.pm4Executed(ResolutionM4EvidenceNotPaid, 650)
		m.pm4MustCallback(p.fresh, OutcomeDeclined, "pm4-wk-"+uuid.NewString()[:8], 650)
		m.pm4WantNoSignal(p.fresh, "decline after an executed not-paid")
	})
}

// The late-evidence path: a worker holding a STALE snapshot of the attempt (submitting / pending) applies a definite result
// that loses the CAS to the already-disputed attempt; after an executed M4 that is the same post-resolution signal.
func TestPostM4_LateEvidence_StaleSnapshot_Sync_And_Poll(t *testing.T) {
	for _, sc := range pm4Scenarios(t) {
		for _, shape := range []string{"poll_stale_pending", "sync_stale_submitting"} {
			t.Run(sc.name+"/"+shape, func(t *testing.T) {
				m := newM4World(t)
				p, base := m.pm4Executed(sc.m4Kind, sc.amount)
				ref := "pm4-late-" + uuid.NewString()[:10]
				class := ErrorClassSucceeded
				if sc.observed == OutcomeDeclined {
					class = ErrorClassDefiniteDecline
				}
				switch shape {
				case "poll_stale_pending":
					if err := m.pm4Poll(p.staleAs(AttemptPending), class, StatusResult{Outcome: sc.observed, ProviderReference: ref, Amount: sc.amount, AssetCode: "EUR", DeclineReason: "insufficient_funds"}); err != nil {
						t.Fatalf("poll with a stale snapshot: %v", err)
					}
				default:
					gr := GateResult[WithdrawResult]{Class: class, Value: WithdrawResult{Outcome: sc.observed, ProviderReference: ref, DeclineReason: "insufficient_funds"}}
					if err := ApplyPayoutResult(context.Background(), m.pool, m.f.tenantID, p.wr.ID, p.staleAs(AttemptSubmitting), gr, EvidenceSync, m.orch.PayoutOptions()...); err != nil {
						b11OKOrConflict(t, err, "sync with a stale snapshot")
					}
				}
				ev := string(EvidenceQueryStatus)
				if shape != "poll_stale_pending" {
					ev = string(EvidenceSync)
				}
				m.pm4WantSignal(p.fresh, sc.reason, string(sc.m4Kind), string(sc.observed), ev, 1, 1)
				m.pm4Unchanged(p, base, shape)
			})
		}
	}
}

// Closed vocabulary: the audit row carries only the reviewed keys (no provider text beyond the validated echoed reference).
func TestPostM4_AuditRow_ClosedVocabulary(t *testing.T) {
	allowed := map[string]bool{"reason": true, "observed": true, "m4_kind": true, "m4_resolution_id": true, "evidence": true,
		"provider_id": true, "withdrawal_request_id": true, "withdrawal_state": true, "attempt_state": true,
		"attempt_terminal_reason": true, "echoed_provider_reference": true, "echo_ref_reason": true, "echo_ref_len": true, "echo_ref_sha256_prefix": true}
	m := newM4World(t)
	p, _ := m.pm4Executed(ResolutionM4EvidenceNotPaid, 650)
	// A hostile-shaped reference cannot reach the cells through ingress; the helper is exercised directly with one.
	hostile := "pm4\x00<script>" + strings.Repeat("Z", 300)
	for _, ref := range []string{"pm4-ok-" + uuid.NewString()[:8], hostile} {
		if err := m.pool.WithTenant(context.Background(), m.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			newR := true
			return payoutPostM4Cell(ctx, tx, p.fresh, OutcomeSucceeded, EvidenceCallback, &newR, ref)
		}); err != nil {
			t.Fatalf("cell: %v", err)
		}
	}
	rows := m.sysQuery(`SELECT metadata FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3`,
		m.f.tenantID, auditActionPayoutPostM4Contradiction, p.fresh.ID.String())
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		md := r["metadata"].(map[string]any)
		for k := range md {
			if !allowed[k] {
				t.Fatalf("unreviewed audit key %q: %v", k, md)
			}
		}
		if blob := fmt.Sprint(md); strings.Contains(blob, "<script>") || strings.Contains(blob, "ZZZZZZZZZZ") {
			t.Fatalf("hostile text stored raw: %s", blob)
		}
	}
}
