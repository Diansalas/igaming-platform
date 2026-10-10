//go:build integration

// GOV-R32 review round (ADR 0111 23.6; migration 0127): refusal-direction fixes.
//
//   - LF HIGH: a destination park (destination_mismatch / destination_integrity_failure)
//     records durably what the provider reported (payout_park_evidence), on every
//     path: sync phase C, QueryStatus poll, callback/receipt, and a success that reaches an
//     ALREADY parked attempt (fresh poll, callback, stale/late result). A recorded success
//     makes M4 not-paid `contradictory`, at request and at execution.
//   - security C-1 / LF Q-R32-2: a payout line on the bound or a matched reference naming
//     ANOTHER merchant reference makes not-paid `insufficient`; reconciliation R-1 mirrors it.
//   - security C-3: raw writes without an actor proof are refused in every session shape.
//
// Every provider and statement source is MOCK; the world runs as the non-superuser,
// non-BYPASSRLS owner-shaped role (k3AssertUnprivilegedRole).
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
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// r32ParkEvidence is the set "outcome/kind" of the attempt's park-evidence rows.
func (m *m4World) r32ParkEvidence(attemptID uuid.UUID) map[string]bool {
	m.t.Helper()
	out := map[string]bool{}
	for _, r := range m.sysQuery(`SELECT reported_outcome || '/' || evidence_kind AS k FROM payout_park_evidence
		WHERE tenant_id = $1 AND attempt_id = $2`, m.f.tenantID, attemptID) {
		out[r["k"].(string)] = true
	}
	return out
}

func (m *m4World) r32RequireEvidence(attemptID uuid.UUID, want ...string) {
	m.t.Helper()
	got := m.r32ParkEvidence(attemptID)
	for _, w := range want {
		if !got[w] {
			m.t.Fatalf("park evidence of %s lacks %q (got %v)", attemptID, w, got)
		}
	}
}

func r32Echo() *payoutinstrument.DestinationEcho {
	return &payoutinstrument.DestinationEcho{Fingerprint: strings.Repeat("ab", 32), Kid: "r32-other"}
}

// r32MismatchPark parks a pending bound payout as destination_mismatch through the REAL writer
// (intact snapshot, a poll carrying a differing echo) with the given outcome.
func (m *m4World) r32MismatchPark(amount int64, outcome Outcome) *b11Parked {
	m.t.Helper()
	wr, pend := m.payout(amount)
	x := *pend.ProviderReference
	st := StatusResult{Outcome: outcome, ProviderReference: x, Amount: amount, AssetCode: "EUR", DestinationEcho: r32Echo()}
	if outcome == OutcomeDeclined {
		st.DeclineReason = "insufficient_funds"
	}
	m.prov.setStatus(x, st)
	if err := PollPayoutStatus(context.Background(), m.pool, m.orch, MockCredentialResolver{}, m.f.tenantID, pend, time.Now().Add(time.Minute), nil); err != nil {
		m.t.Fatalf("park (poll): %v", err)
	}
	fresh := m.attempt(pend.ID)
	if fresh.State != AttemptDisputed || fresh.TerminalReason == nil || *fresh.TerminalReason != TerminalReasonDestinationMismatch {
		m.t.Fatalf("setup: want disputed/destination_mismatch, got %s %v", fresh.State, fresh.TerminalReason)
	}
	return &b11Parked{shape: "mismatch", wr: wr, stale: pend, fresh: fresh, ref: x, base: m.b11Snap(wr.ID, pend.ID)}
}

// r32RequireNotPaidRefused: with a declaring sealed decline on the merchant reference (another
// PSP reference), the verdict is not not_paid and an M4 not-paid request is refused (MR062).
func (m *m4World) r32RequireNotPaidRefused(p *b11Parked, want string) {
	m.t.Helper()
	m.declineOn(p, "r32-D-"+uuid.NewString()[:12])
	ev := m4Verdict(m.t, m, p.fresh.ID)
	if ev.Verdict != want || ev.LineID != nil {
		m.t.Fatalf("verdict: want %s, got %+v", want, ev)
	}
	_, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidenceNotPaid, &m4AnyLine))
	k3RequireCode(m.t, err, "MR062")
	if w := m.wd(p.wr.ID); w.State != "submitted" || w.ReleaseLedgerTransactionID != nil {
		m.t.Fatalf("the hold moved: %+v", w)
	}
	m.assertInvariants()
}

// --- LF HIGH -------------------------------------------------------------------------------------

// The ledger-finance reproduction, both destination reasons: the PSP reports SUCCESS, the
// platform parks, a sealed declaring import then carries a decline on ANOTHER reference with the
// merchant reference. Before the fix the verdict was not_paid; now it is contradictory.
func TestR32_HIGH_SuccessParked_RefusesNotPaid_BothReasons(t *testing.T) {
	m := newM4World(t)
	t.Run("integrity, poll success", func(t *testing.T) {
		m.t = t
		p := m.r32Park(1100, r32Missing, true)
		m.r32RequireEvidence(p.fresh.ID, "succeeded/query_status")
		m.r32RequireNotPaidRefused(p, M4VerdictContradictory)
	})
	t.Run("mismatch, poll success with a differing echo", func(t *testing.T) {
		m.t = t
		p := m.r32MismatchPark(1101, OutcomeSucceeded)
		m.r32RequireEvidence(p.fresh.ID, "succeeded/query_status")
		m.r32RequireNotPaidRefused(p, M4VerdictContradictory)
	})
}

// Decline-parked parks keep working (both reasons, real writer): the record says declined and
// M4 not-paid executes once.
func TestR32_HIGH_DeclineParked_NotPaidStillWorks_BothReasons(t *testing.T) {
	m := newM4World(t)
	for _, park := range []func() *b11Parked{
		func() *b11Parked { return m.r32Park(1110, r32Tampered, false) },
		func() *b11Parked { return m.r32MismatchPark(1111, OutcomeDeclined) },
	} {
		p := park()
		m.r32RequireEvidence(p.fresh.ID, "declined/query_status")
		if m.r32ParkEvidence(p.fresh.ID)["succeeded/query_status"] {
			t.Fatal("a decline park recorded a success")
		}
		m.declineOn(p, "r32-D-"+uuid.NewString()[:12])
		ev := m.mustEvidence(p.fresh.ID, M4VerdictNotPaid)
		m.execute(p, ResolutionM4EvidenceNotPaid, ev)
		if m.r32FailedFor(p.wr.ID) != 1 {
			t.Fatal("not executed exactly once")
		}
	}
	m.assertInvariants()
}

// The record is written on every path that can park or that can report a success on a parked
// attempt; each then refuses not-paid.
func TestR32_HIGH_RecordWrittenOnEveryPath(t *testing.T) {
	m := newM4World(t)
	t.Run("sync phase C success parks (integrity)", func(t *testing.T) {
		m.t = t
		wr, att := m.b11Claim(1120)
		gr := m.b11Dispatch(att, func(WithdrawRequest) WithdrawResult {
			return WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: "r32-sync-" + uuid.NewString()[:10]}
		})
		m.r32Tamper(`DELETE FROM payout_attempt_destination_snapshots WHERE attempt_id = $1`, att.ID)
		if err := ApplyPayoutResult(context.Background(), m.pool, m.f.tenantID, wr.ID, att, gr, EvidenceSync, WithDestinations(pitest.Shared())); err != nil {
			t.Fatalf("apply: %v", err)
		}
		a := m.attempt(att.ID)
		if a.State != AttemptDisputed || *a.TerminalReason != r32Reason {
			t.Fatalf("not parked: %s %v", a.State, a.TerminalReason)
		}
		m.r32RequireEvidence(att.ID, "succeeded/sync")
		m.r32RequireNotPaidRefused(&b11Parked{wr: wr, stale: att, fresh: a, ref: *a.ProviderReference}, M4VerdictContradictory)
	})
	t.Run("callback success parks (integrity)", func(t *testing.T) {
		m.t = t
		wr, pend := m.payout(1121)
		m.r32Tamper(`DELETE FROM payout_attempt_destination_snapshots WHERE attempt_id = $1`, pend.ID)
		if _, err := m.pm4Callback(pend, OutcomeSucceeded, *pend.ProviderReference, 1121, nil); err != nil {
			t.Fatalf("callback: %v", err)
		}
		a := m.attempt(pend.ID)
		if a.State != AttemptDisputed || *a.TerminalReason != r32Reason {
			t.Fatalf("not parked: %s %v", a.State, a.TerminalReason)
		}
		m.r32RequireEvidence(pend.ID, "succeeded/callback")
		m.r32RequireNotPaidRefused(&b11Parked{wr: wr, stale: pend, fresh: a, ref: *a.ProviderReference}, M4VerdictContradictory)
	})
	t.Run("callback success on an already parked attempt", func(t *testing.T) {
		m.t = t
		p := m.r32Park(1122, r32Missing, false)
		if _, err := m.pm4Callback(p.fresh, OutcomeSucceeded, p.ref, 1122, nil); err != nil {
			t.Fatalf("callback: %v", err)
		}
		m.r32RequireEvidence(p.fresh.ID, "declined/query_status", "succeeded/callback")
		m.r32RequireNotPaidRefused(p, M4VerdictContradictory)
	})
	t.Run("poll success on an already parked attempt", func(t *testing.T) {
		m.t = t
		p := m.r32MismatchPark(1123, OutcomeDeclined)
		m.prov.setStatus(p.ref, StatusResult{Outcome: OutcomeSucceeded, ProviderReference: p.ref, Amount: 1123, AssetCode: "EUR"})
		if err := PollPayoutStatus(context.Background(), m.pool, m.orch, MockCredentialResolver{}, m.f.tenantID, m.attempt(p.fresh.ID), time.Now().Add(time.Minute), nil); err != nil {
			t.Fatalf("poll: %v", err)
		}
		m.r32RequireEvidence(p.fresh.ID, "declined/query_status", "succeeded/query_status")
		m.r32RequireNotPaidRefused(p, M4VerdictContradictory)
	})
	t.Run("late sync success that lost the CAS to the park", func(t *testing.T) {
		m.t = t
		p := m.r32Park(1124, r32Missing, false)
		// A sync phase C result computed from the pre-park (pending) snapshot of the attempt.
		gr := GateResult[WithdrawResult]{Class: ErrorClassSucceeded, Value: WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: p.ref}}
		err := ApplyPayoutResult(context.Background(), m.pool, m.f.tenantID, p.wr.ID, p.staleAs(AttemptPending), gr, EvidenceSync, WithDestinations(pitest.Shared()))
		b11OKOrConflict(t, err, "late sync")
		m.r32RequireEvidence(p.fresh.ID, "declined/query_status", "succeeded/sync")
		m.r32RequireNotPaidRefused(p, M4VerdictContradictory)
	})
	t.Run("stale poll success on a parked attempt", func(t *testing.T) {
		m.t = t
		p := m.r32Park(1125, r32Missing, false)
		m.prov.setStatus(p.ref, StatusResult{Outcome: OutcomeSucceeded, ProviderReference: p.ref, Amount: 1125, AssetCode: "EUR"})
		err := PollPayoutStatus(context.Background(), m.pool, m.orch, MockCredentialResolver{}, m.f.tenantID, p.staleAs(AttemptPending), time.Now().Add(time.Minute), nil)
		b11OKOrConflict(t, err, "stale poll")
		m.r32RequireEvidence(p.fresh.ID, "declined/query_status", "succeeded/query_status")
		m.r32RequireNotPaidRefused(p, M4VerdictContradictory)
	})
}

// A success that arrives AFTER the request is refused at execution (Go first, the DB re-check
// behind it); nothing moves.
func TestR32_HIGH_SuccessAfterRequest_RefusedAtExecution(t *testing.T) {
	m := newM4World(t)
	p := m.r32Park(1130, r32Missing, false)
	snap := m.r32Snapshot(p.fresh.ID)
	ev := m.r32Decline(p, "r32-D-"+uuid.NewString()[:12])
	r, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidenceNotPaid, ev.LineID))
	k3RequireNoErr(t, err, "request")
	if _, err := m.pm4Callback(p.fresh, OutcomeSucceeded, p.ref, 1130, nil); err != nil {
		t.Fatalf("callback: %v", err)
	}
	p.base = m.b11Snap(p.wr.ID, p.fresh.ID) // the callback itself moves nothing financial
	out, err := m.decide(m.acting2, r, ResolutionApprove)
	if err == nil && (out.Executed || !out.Refused) {
		t.Fatalf("want refused at execution, got %+v", out)
	}
	m.r32Held(p, snap, "success after the request")
	// A fresh request is refused by the recorded success (MR062).
	_, err = m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidenceNotPaid, &m4AnyLine))
	k3RequireCode(t, err, "MR062")
}

// The record is append-only and system-written only.
func TestR32_ParkEvidence_AppendOnlySystemWritten(t *testing.T) {
	m := newM4World(t)
	p := m.r32Park(1140, r32Missing, false)
	other, pend := m.payout(1141)
	_ = other
	ins := func(attempt uuid.UUID, reason, outcome, kind string) func(ctx context.Context, tx pgx.Tx) error {
		return func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO payout_park_evidence (tenant_id, attempt_id, terminal_reason, reported_outcome, evidence_kind)
				VALUES ($1, $2, $3, $4, $5)`, m.f.tenantID, attempt, reason, outcome, kind)
			return err
		}
	}
	// Acting and tenant-staff sessions cannot write (no policy): RLS refusal.
	err := m.pool.WithPlatformActingInTenant(context.Background(), m.acting.ID, m.f.tenantID, uuid.Nil, OperationKindForceResolve, ins(p.fresh.ID, r32Reason, "succeeded", "callback"))
	k3RequireCode(t, err, "42501")
	err = m.pool.WithPrincipalScope(context.Background(), m.f.tenantID, m.f1.ID, ins(p.fresh.ID, r32Reason, "succeeded", "callback"))
	k3RequireCode(t, err, "42501")
	// The system shape may describe only a payout parked on that reason, never with the backfill marker.
	err = m.pool.WithTenant(context.Background(), m.f.tenantID, ins(pend.ID, r32Reason, "succeeded", "callback"))
	k3RequireCode(t, err, "MR064")
	err = m.pool.WithTenant(context.Background(), m.f.tenantID, ins(p.fresh.ID, TerminalReasonDestinationMismatch, "succeeded", "callback"))
	k3RequireCode(t, err, "MR064")
	err = m.pool.WithTenant(context.Background(), m.f.tenantID, ins(p.fresh.ID, r32Reason, "unknown_pre_0127", "migration_0127"))
	k3RequireCode(t, err, "MR064")
	// Append-only: no UPDATE/DELETE reaches a row (no policy), and with RLS lifted the trigger refuses.
	before := m.r32ParkEvidence(p.fresh.ID)
	m.tx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE payout_park_evidence SET reported_outcome = 'declined' WHERE attempt_id = $1`, p.fresh.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM payout_park_evidence WHERE attempt_id = $1`, p.fresh.ID)
		return err
	})
	err = m.pool.WithTenant(context.Background(), m.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE payout_park_evidence NO FORCE ROW LEVEL SECURITY`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM payout_park_evidence WHERE attempt_id = $1`, p.fresh.ID)
		return err
	})
	if err == nil {
		t.Fatal("a DELETE with RLS lifted was not refused by the immutability trigger")
	}
	if got := m.r32ParkEvidence(p.fresh.ID); len(got) != len(before) || !got["declined/query_status"] {
		t.Fatalf("the park evidence changed: %v -> %v", before, got)
	}
}

// --- security C-1 / LF Q-R32-2 (flips the former current-behaviour pin) ----------------------------

func TestR32_C1_NotPaid_LineNamingAnotherMerchant_Refused(t *testing.T) {
	m := newM4World(t)
	t.Run("integrity park: decline on the bound reference naming another merchant", func(t *testing.T) {
		m.t = t
		p := m.r32Park(1150, r32Missing, false)
		m.ingest(m4Imp{start: p.fresh.CreatedAt.Add(-time.Minute), end: p.fresh.LastSentAt.Add(25 * time.Hour)},
			m.line(p.ref, "someone-elses-merchant-ref", statement.PaymentStatusDeclined, 1150, time.Now()))
		m.mustEvidence(p.fresh.ID, M4VerdictInsufficient)
		_, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidenceNotPaid, &m4AnyLine))
		k3RequireCode(t, err, "MR062")
	})
	t.Run("unbound park: a matched reference D also names another merchant", func(t *testing.T) {
		m.t = t
		p := m.park(1151)
		d := "r32-D-" + uuid.NewString()[:12]
		m.declineOn(p, d, m.line(d, "someone-elses-merchant-ref", statement.PaymentStatusDeclined, 1151, time.Now()))
		m.mustEvidence(p.fresh.ID, M4VerdictInsufficient)
	})
	t.Run("destination_mismatch park: same rule", func(t *testing.T) {
		m.t = t
		p := m.r32MismatchPark(1152, OutcomeDeclined)
		m.ingest(m4Imp{start: p.fresh.CreatedAt.Add(-time.Minute), end: p.fresh.LastSentAt.Add(25 * time.Hour)},
			m.line(p.ref, p.fresh.MerchantReference, statement.PaymentStatusDeclined, 1152, time.Now()),
			m.line(p.ref, "someone-elses-merchant-ref", statement.PaymentStatusDeclined, 1152, time.Now()))
		m.mustEvidence(p.fresh.ID, M4VerdictInsufficient)
	})
	t.Run("arrives after the request: refused at execution", func(t *testing.T) {
		m.t = t
		p := m.r32Park(1153, r32Tampered, false)
		snap := m.r32Snapshot(p.fresh.ID)
		ev := m.r32Decline(p, p.ref)
		r, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidenceNotPaid, ev.LineID))
		k3RequireNoErr(t, err, "request")
		m.ingest(m4Imp{unsealed: true}, m.line(p.ref, "someone-elses-merchant-ref", statement.PaymentStatusDeclined, 1153, time.Now()))
		out, err := m.decide(m.acting2, r, ResolutionApprove)
		if err == nil && (out.Executed || !out.Refused) {
			t.Fatalf("want refused at execution, got %+v", out)
		}
		m.r32Held(p, snap, "C-1 after the request")
	})
	t.Run("control: lines naming only this merchant (or none) still allow not-paid", func(t *testing.T) {
		m.t = t
		p := m.r32Park(1154, r32Missing, false)
		m.r32Decline(p, p.ref)
	})
}

// R-1 mirrors C-1: after an executed not-paid, a payout line on a reference R-1 reads that names
// another merchant reference raises (check=m4_not_paid_attribution_ambiguous); a control without
// it raises nothing for the attempt.
func TestR32_C1_R1Mirror_RaisesAfterExecutedNotPaid(t *testing.T) {
	m := newM4World(t)
	p := m.park(1160)
	d := "r32-D-" + uuid.NewString()[:12]
	m.declineOn(p, d)
	ev := m.mustEvidence(p.fresh.ID, M4VerdictNotPaid)
	m.execute(p, ResolutionM4EvidenceNotPaid, ev)
	ms := m.stmtRun(m.source(false))
	if got := mismatchesOf(ms, reconciliation.MismatchKindPayDeclaredNotPaidButPaid, "attempt="+p.fresh.ID.String()); len(got) != 0 {
		t.Fatalf("control raised:\n%s", render(ms))
	}
	ms = m.stmtRun(m.source(false, m.payoutLine(d, "someone-elses-merchant-ref", statement.PaymentStatusDeclined, 1160)))
	if got := mismatchesOf(ms, reconciliation.MismatchKindPayDeclaredNotPaidButPaid, "check=m4_not_paid_attribution_ambiguous"); len(got) != 1 ||
		!strings.Contains(got[0].ReconciliationKey, p.fresh.ID.String()) {
		t.Fatalf("want one attribution finding for the attempt:\n%s", render(ms))
	}
}

// --- security C-3: raw writes without an actor proof, per session shape --------------------------

func TestR32_C3_RawWritesWithoutProof_EverySessionShape(t *testing.T) {
	m := newM4World(t)
	p := m.r32Park(1170, r32Missing, false)
	ev := m.r32Decline(p, p.ref)
	insert := func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO payment_manual_resolutions
			(tenant_id, attempt_id, operation, kind, basis_code, evidence_ref_hash, amount, asset_code, brand_id, reason_code,
			 attempt_state_at_submission, ever_possibly_sent_at_submission, payload_hash, requested_by, requested_by_scope,
			 requested_by_person_id, tenant_status_at_submission, required_at_submission, contributing_policy_ids, expires_at, evidence_line_id)
			VALUES ($1, $2, 'payout', 'm4_evidence_not_paid', 'provider_confirmed_out_of_band', $3, NULL, NULL, $4, 'r32', '-', false, '-', $4, 'tenant', $4, '-', 1, '{}', now(), $5)`,
			m.f.tenantID, p.fresh.ID, k3EvidenceHash(), uuid.Nil, *ev.LineID)
		return err
	}
	ctx := context.Background()
	shapes := []struct {
		name string
		run  func(fn func(ctx context.Context, tx pgx.Tx) error) error
		want []string
	}{
		{"platform_acting", func(fn func(ctx context.Context, tx pgx.Tx) error) error {
			return m.pool.WithPlatformActingInTenant(ctx, m.acting.ID, m.f.tenantID, uuid.Nil, OperationKindForceResolve, fn)
		}, []string{"AP001"}},
		{"tenant staff", func(fn func(ctx context.Context, tx pgx.Tx) error) error {
			return m.pool.WithPrincipalScope(ctx, m.f.tenantID, m.f1.ID, fn)
		}, []string{"AP001", "MR060"}},
		{"system", func(fn func(ctx context.Context, tx pgx.Tx) error) error {
			return m.pool.WithTenant(ctx, m.f.tenantID, fn)
		}, []string{"AP001", "MR001", "CG001"}},
		{"platform admin", func(fn func(ctx context.Context, tx pgx.Tx) error) error {
			return m.pool.WithPlatformAdmin(ctx, m.adminA.ID, fn)
		}, []string{"AP001", "MR001", "CG001", "42501"}},
	}
	for _, s := range shapes {
		err := s.run(insert)
		ok := false
		for _, w := range s.want {
			if k3Code(err) == w {
				ok = true
			}
		}
		if !ok {
			t.Fatalf("%s: raw M4 INSERT without a proof: want one of %v, got %v", s.name, s.want, err)
		}
	}
	// A real pending request, then raw UPDATE / approval INSERT without a proof.
	r, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidenceNotPaid, ev.LineID))
	k3RequireNoErr(t, err, "request")
	cancel := func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE payment_manual_resolutions SET state = 'cancelled' WHERE id = $1`, r.ID)
		if err == nil && tag.RowsAffected() == 0 {
			return errors.New("0 rows")
		}
		return err
	}
	approve := func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO payment_manual_resolution_approvals
			(tenant_id, resolution_id, decision, payload_hash, decided_by, decided_by_scope, decided_by_person_id, decided_txid, reason_code)
			VALUES ($1, $2, 'approve', $3, $4, 'tenant', $4, 0, 'r32')`, m.f.tenantID, r.ID, r.PayloadHash, uuid.Nil)
		return err
	}
	for _, s := range shapes {
		for name, fn := range map[string]func(ctx context.Context, tx pgx.Tx) error{"cancel": cancel, "approve": approve} {
			if err := s.run(fn); err == nil {
				t.Fatalf("%s: raw %s without a proof was accepted", s.name, name)
			}
		}
	}
	if got := m.resolution(r.ID); got.State != ResolutionPending {
		t.Fatalf("state after the refused raw writes: %s", got.State)
	}
	if n := m.r32FailedFor(p.wr.ID); n != 0 {
		t.Fatalf("a posting happened: %d", n)
	}
}

// --- migration 0127 backfill (fail closed for parks that predate the record) ----------------------

func TestR32_Migration0127_BackfillsExistingDestinationParks(t *testing.T) {
	m := newM4World(t)
	if err := r32DownTo0126(t, m); err != nil {
		t.Fatalf("down to 0126 (empty): %v", err)
	}
	var parks []*b11Parked
	for i, reason := range []string{r32Reason, TerminalReasonDestinationMismatch} {
		wr, a := m.payout(int64(1180 + i))
		m.tx(func(ctx context.Context, tx pgx.Tx) error {
			return ApplyDisputeFromNonTerminal(ctx, tx, a.ID, EvidenceCallback, reason)
		})
		fresh := m.attempt(a.ID)
		parks = append(parks, &b11Parked{wr: wr, stale: a, fresh: fresh, ref: *fresh.ProviderReference})
	}
	if _, err := m.pool.MigrateUp(context.Background(), realMigrationsDir(t)); err != nil {
		t.Fatalf("up: %v", err)
	}
	for _, p := range parks {
		m.r32RequireEvidence(p.fresh.ID, "unknown_pre_0127/migration_0127")
		m.r32RequireNotPaidRefused(p, M4VerdictInsufficient)
	}
	k3RequireCode(t, r32DownTo0126(t, m), "MR099")
}

// --- D-7 review (refinement 1): EVERY park writer that a provider success can trigger -----------------

// A success-triggered park of ANY M4-scope reason records the success and refuses not-paid; a
// pending-triggered park records pending and keeps its not-paid exit; a success reaching an already
// parked unbound attempt (callback, late sync) is recorded too.
func TestR32_AllParkWriters_SuccessRecorded_NotPaidRefused(t *testing.T) {
	m := newM4World(t)
	t.Run("invalid_provider_reference, sync SUCCESS", func(t *testing.T) {
		m.t = t
		p := m.b11ParkSync(1190, OutcomeSucceeded)
		m.r32RequireEvidence(p.fresh.ID, "succeeded/sync")
		m.r32RequireNotPaidRefused(p, M4VerdictContradictory)
	})
	t.Run("invalid_provider_reference, sync PENDING (control: not-paid still works)", func(t *testing.T) {
		m.t = t
		p := m.park(1191)
		m.r32RequireEvidence(p.fresh.ID, "pending/sync")
		m.declineOn(p, "r32-D-"+uuid.NewString()[:12])
		m.execute(p, ResolutionM4EvidenceNotPaid, m.mustEvidence(p.fresh.ID, M4VerdictNotPaid))
	})
	t.Run("invalid_provider_reference, poll SUCCESS (park holding X)", func(t *testing.T) {
		m.t = t
		pp := m.b11ParkPoll(1192, OutcomeSucceeded)
		m.r32RequireEvidence(pp.fresh.ID, "succeeded/query_status")
	})
	t.Run("provider_reference_conflict, sync SUCCESS", func(t *testing.T) {
		m.t = t
		p, _, _ := m.b11ParkReverse(1193)
		m.r32RequireEvidence(p.fresh.ID, "succeeded/sync")
		m.r32RequireNotPaidRefused(p, M4VerdictContradictory)
	})
	t.Run("callback SUCCESS on an already parked unbound attempt", func(t *testing.T) {
		m.t = t
		p := m.park(1194)
		if _, err := m.pm4Callback(p.fresh, OutcomeSucceeded, "r32-cb-"+uuid.NewString()[:10], 1194, nil); err != nil {
			t.Fatalf("callback: %v", err)
		}
		m.r32RequireEvidence(p.fresh.ID, "pending/sync", "succeeded/callback")
		m.r32RequireNotPaidRefused(p, M4VerdictContradictory)
	})
	t.Run("late sync SUCCESS on an already parked unbound attempt", func(t *testing.T) {
		m.t = t
		p := m.park(1195)
		gr := GateResult[WithdrawResult]{Class: ErrorClassSucceeded, Value: WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: "r32-late-" + uuid.NewString()[:10]}}
		err := ApplyPayoutResult(context.Background(), m.pool, m.f.tenantID, p.wr.ID, p.staleAs(AttemptPending), gr, EvidenceSync, WithDestinations(pitest.Shared()))
		b11OKOrConflict(t, err, "late sync")
		m.r32RequireEvidence(p.fresh.ID, "pending/sync", "succeeded/sync")
		m.r32RequireNotPaidRefused(p, M4VerdictContradictory)
	})
}

// --- D-7 review (G-TIME upper bound in the DB, zero tolerance) ----------------------------------------

func TestR32_GTIME_CoverageEndBoundary_InTheDB(t *testing.T) {
	m := newM4World(t)
	end := time.Now().Add(2 * time.Minute).UTC().Truncate(time.Microsecond)
	t.Run("a line exactly at its import's coverage end is paid", func(t *testing.T) {
		m.t = t
		p := m.park(1196)
		m.ingest(m4Imp{end: end}, m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", 1196, end))
		m.mustEvidence(p.fresh.ID, M4VerdictPaid)
	})
	t.Run("one microsecond after its import's coverage end is insufficient (DB, MR062)", func(t *testing.T) {
		m.t = t
		p := m.park(1197)
		m.ingest(m4Imp{end: end}, m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", 1197, end.Add(time.Microsecond)))
		m.d7DBRefusesPaid(p, M4VerdictInsufficient)
	})
}
