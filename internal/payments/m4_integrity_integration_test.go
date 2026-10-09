//go:build integration

// GOV-R32 (owner decision 4, ADR 0095 section 48; ADR 0111 section 23; migration
// 0127): the governed exit of a `destination_integrity_failure` park.
//
// Oracle (owner decision 4, verbatim intent): NO automatic exit; the park stays
// held until a controlled FOUR-EYES resolution; no unilateral staff override; no
// provider callback or poll changes the destination or releases the hold; no
// automatic release, settlement or beneficiary reassignment. The controlled
// cancellation/release outcome is the existing M4 NOT-PAID resolution only
// (positive decline evidence over sealed imports, platform_acting requester and
// final approver, the DB platform floor), which returns the hold to the player's
// OWN cash. M4 PAID stays refused. "Resume" is DESIGN ONLY (ADR 0111 23.3).
//
// Every park here is made by the REAL B13-B writer (payoutDestinationEvidence ->
// parkPayoutDestination) on a BOUND withdrawal whose write-once snapshot was
// damaged by an owner-level tamper (the attack the seals exist for), through the
// real QueryStatus poll. Every provider is MOCK; every statement source is MOCK.
// The world runs as the runtime-shaped, non-superuser, non-BYPASSRLS role.
package payments

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

const r32Reason = TerminalReasonDestinationIntegrityFailure

// migrationsAbove counts the migration versions in the real directory that are
// newer than v, so a "roll back everything above v" step count never silently
// targets the wrong version when a later head migration lands.
func migrationsAbove(t *testing.T, v int64) int {
	t.Helper()
	entries, err := os.ReadDir(realMigrationsDir(t))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".up.sql") || len(n) < 4 {
			continue
		}
		ver, err := strconv.Atoi(n[:4])
		if err != nil {
			t.Fatalf("migration filename %q has no numeric version prefix", n)
		}
		if int64(ver) > v {
			seen[ver] = true
		}
	}
	return len(seen)
}

// r32Tamper damages the write-once destination snapshot as the table owner would
// (triggers off, FORCE RLS lifted for the one statement), exactly b13bW.tamper.
func (m *m4World) r32Tamper(stmt string, args ...any) {
	m.t.Helper()
	const table = "payout_attempt_destination_snapshots"
	err := m.pool.WithTenant(context.Background(), m.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for _, q := range []string{`ALTER TABLE ` + table + ` DISABLE TRIGGER USER`, `ALTER TABLE ` + table + ` NO FORCE ROW LEVEL SECURITY`} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx, stmt, args...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() < 1 {
			return errors.New("tamper statement affected no row")
		}
		for _, q := range []string{`ALTER TABLE ` + table + ` FORCE ROW LEVEL SECURITY`, `ALTER TABLE ` + table + ` ENABLE TRIGGER USER`} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		m.t.Fatalf("tamper snapshot: %v", err)
	}
}

// r32Snapshot is a digest of the attempt's snapshot rows (count + row hashes):
// "unchanged" means byte-identical, including "still missing".
func (m *m4World) r32Snapshot(attemptID uuid.UUID) string {
	m.t.Helper()
	var n int
	var h string
	m.tx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*), COALESCE(string_agg(md5(row_to_json(s)::text), ',' ORDER BY s.attempt_id), '')
			FROM payout_attempt_destination_snapshots s WHERE s.tenant_id = $1 AND s.attempt_id = $2`, m.f.tenantID, attemptID).Scan(&n, &h)
	})
	return strconv.Itoa(n) + "|" + h
}

// r32Shape is how the snapshot is damaged before the evidence arrives.
type r32Shape string

const (
	r32Missing  r32Shape = "missing"
	r32Tampered r32Shape = "tampered"
)

// r32Park makes a BOUND payout (real claim + snapshot + dispatch; the MOCK PSP
// answers pending with a reference X), damages its snapshot, then lets the REAL
// QueryStatus poll deliver evidence: a DECLINE carrying a destination echo (the
// echo forces the destination check), or a SUCCESS. Either parks the attempt
// `disputed` / destination_integrity_failure with X bound, hold kept.
func (m *m4World) r32Park(amount int64, shape r32Shape, success bool) *b11Parked {
	m.t.Helper()
	wr, pend := m.payout(amount)
	x := *pend.ProviderReference
	switch shape {
	case r32Missing:
		m.r32Tamper(`DELETE FROM payout_attempt_destination_snapshots WHERE attempt_id = $1`, pend.ID)
	case r32Tampered:
		m.r32Tamper(`UPDATE payout_attempt_destination_snapshots SET display_mask = 'ZZ****9999' WHERE attempt_id = $1`, pend.ID)
	}
	st := StatusResult{Outcome: OutcomeDeclined, ProviderReference: x, Amount: amount, AssetCode: "EUR", DeclineReason: "insufficient_funds",
		DestinationEcho: &payoutinstrument.DestinationEcho{Fingerprint: strings.Repeat("cd", 32), Kid: "r32-any"}}
	if success {
		st = StatusResult{Outcome: OutcomeSucceeded, ProviderReference: x, Amount: amount, AssetCode: "EUR"}
	}
	m.prov.setStatus(x, st)
	if err := PollPayoutStatus(context.Background(), m.pool, m.orch, MockCredentialResolver{}, m.f.tenantID, pend, time.Now().Add(time.Minute), nil); err != nil {
		m.t.Fatalf("park (poll): %v", err)
	}
	fresh := m.attempt(pend.ID)
	if fresh.State != AttemptDisputed || fresh.TerminalReason == nil || *fresh.TerminalReason != r32Reason ||
		fresh.ProviderReference == nil || *fresh.ProviderReference != x || fresh.LastSentAt == nil {
		m.t.Fatalf("setup: want disputed/%s holding %s, got %s %v %v", r32Reason, x, fresh.State, fresh.TerminalReason, fresh.ProviderReference)
	}
	if got := m.withdrawalOf(wr.ID); got.State != withdrawal.StateSubmitted || got.ReleaseLedgerTransactionID != nil {
		m.t.Fatalf("setup: the park must keep the hold, got %s release=%v", got.State, got.ReleaseLedgerTransactionID)
	}
	if n := m.countRows(`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'payments.payout_parked_destination' AND target_id = $2
		AND metadata->>'reason' = $3`, m.f.tenantID, pend.ID.String(), r32Reason); n != 1 {
		m.t.Fatalf("setup: park audit rows %d", n)
	}
	m.r32RequireAlert(pend.ID)
	return &b11Parked{shape: string(shape), wr: wr, stale: pend, fresh: fresh, ref: x, base: m.b11Snap(wr.ID, pend.ID)}
}

func (m *m4World) r32RequireAlert(attemptID uuid.UUID) {
	m.t.Helper()
	want := "payout_attempt:" + attemptID.String() + ":reason:" + r32Reason
	for _, r := range alertinject.ForSubject(m.t, m.pool, m.f.tenantID) {
		if r.Discriminator == want && r.Severity == "p1" {
			return
		}
	}
	m.t.Fatalf("the integrity park raised no P1 %q", want)
}

// r32Held asserts every "remain parked" property against the post-park baseline:
// nothing posted or released, the attempt untouched (disputed / integrity, same
// reference), no resend, the snapshot untouched, the ledger balanced.
func (m *m4World) r32Held(p *b11Parked, snap, what string) {
	m.t.Helper()
	now := m.b11Snap(p.wr.ID, p.stale.ID)
	if now != p.base {
		m.t.Fatalf("%s: state moved after the park\n before: %+v\n  after: %+v", what, p.base, now)
	}
	if now.wrState != withdrawal.StateSubmitted || now.wrReleased || now.aState != AttemptDisputed || now.aReason != r32Reason || now.aRef != p.ref {
		m.t.Fatalf("%s: not held: %+v", what, now)
	}
	if got := m.r32Snapshot(p.fresh.ID); got != snap {
		m.t.Fatalf("%s: the write-once snapshot changed: %q -> %q", what, snap, got)
	}
	m.assertInvariants()
}

// r32Decline ingests ONE sealed MOCK declaring import covering [created_at,
// last_sent_at + 25h] with a declined line of the attempt's amount on its
// merchant reference (under reference ref), and returns the not-paid evidence.
func (m *m4World) r32Decline(p *b11Parked, ref string) M4Evidence {
	m.t.Helper()
	m.ingest(m4Imp{start: p.fresh.CreatedAt.Add(-time.Minute), end: p.fresh.LastSentAt.Add(25 * time.Hour)},
		m.line(ref, p.fresh.MerchantReference, statement.PaymentStatusDeclined, p.fresh.Amount, time.Now()))
	return m.mustEvidence(p.fresh.ID, M4VerdictNotPaid)
}

func (m *m4World) r32FailedFor(wrID uuid.UUID) int {
	return m.countRows(`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'withdrawal_failed' AND correlation_id = $2`,
		m.f.tenantID, wrID)
}

// --- (a) the controlled cancellation/release outcome: M4 not-paid ------------------------------

func TestR32_IntegrityPark_NotPaid_EndToEnd(t *testing.T) {
	for _, c := range []struct {
		shape r32Shape
		onX   bool // the declined line is under the bound reference X (else another reference D)
	}{{r32Missing, false}, {r32Tampered, true}} {
		t.Run(string(c.shape), func(t *testing.T) {
			m := newM4World(t)
			p := m.r32Park(910, c.shape, false)
			snap := m.r32Snapshot(p.fresh.ID)
			ref := "r32-D-" + uuid.NewString()[:12]
			if c.onX {
				ref = p.ref
			}
			ev := m.r32Decline(p, ref)
			holdBefore, cashBefore, clearingBefore := m.walletBalance("player_withdrawal_hold"), m.walletBalance("player_cash"), m.pspClearing()

			r, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidenceNotPaid, ev.LineID))
			k3RequireNoErr(t, err, "request")
			if r.State != ResolutionPending || r.ProviderReferenceAtSubmission == nil || *r.ProviderReferenceAtSubmission != p.ref ||
				r.TerminalReasonAtSubmission == nil || *r.TerminalReasonAtSubmission != r32Reason || r.Amount != 910 || r.AssetCode != "EUR" ||
				r.EvidenceVerdict == nil || *r.EvidenceVerdict != M4VerdictNotPaid || r.EvidenceReference != nil {
				t.Fatalf("pending row wrong: %+v", r)
			}
			// Nothing moved at request time.
			m.r32Held(p, snap, "after the request")

			out, err := m.decide(m.acting2, r, ResolutionApprove)
			k3RequireNoErr(t, err, "final approval")
			if !out.Executed || out.Resolution.State != ResolutionExecuted || out.Resolution.LedgerTransactionID == nil {
				t.Fatalf("not executed: %+v", out)
			}
			res := out.Resolution
			wr := m.wd(p.wr.ID)
			if wr.State != withdrawal.StateFailed || wr.ReleaseLedgerTransactionID == nil || *wr.ReleaseLedgerTransactionID != *res.LedgerTransactionID {
				t.Fatalf("withdrawal not failed by the M4 posting: %+v", wr)
			}
			// The ledger legs, exactly once: withdrawal_failed keyed wr.id:failed; the hold debit and
			// the player_cash credit, both on the withdrawal's OWN wallet; no psp_clearing movement.
			rows := m.sysQuery(`SELECT transaction_type, idempotency_key, provider_id, provider_tx_id, correlation_id::text AS corr
				FROM ledger_transactions WHERE id = $1`, *res.LedgerTransactionID)
			if len(rows) != 1 || rows[0]["transaction_type"] != "withdrawal_failed" || rows[0]["idempotency_key"] != p.wr.ID.String()+":failed" ||
				rows[0]["provider_id"] != nil || rows[0]["provider_tx_id"] != nil || rows[0]["corr"] != p.wr.ID.String() {
				t.Fatalf("failure posting wrong: %v", rows)
			}
			legs := m.sysQuery(`SELECT a.account_type, a.wallet_id::text AS w, e.direction, e.amount::text AS amt, e.asset_code
				FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.ledger_account_id
				WHERE e.ledger_transaction_id = $1 ORDER BY e.direction`, *res.LedgerTransactionID)
			if len(legs) != 2 ||
				legs[0]["account_type"] != "player_cash" || legs[0]["direction"] != "credit" || legs[0]["w"] != m.f.walletID.String() || legs[0]["amt"] != "910" ||
				legs[1]["account_type"] != "player_withdrawal_hold" || legs[1]["direction"] != "debit" || legs[1]["w"] != m.f.walletID.String() || legs[1]["amt"] != "910" {
				t.Fatalf("legs wrong: %v", legs)
			}
			if m.walletBalance("player_withdrawal_hold") != holdBefore-910 || m.walletBalance("player_cash") != cashBefore+910 || m.pspClearing() != clearingBefore {
				t.Fatal("the hold did not move to the player's own cash (or psp_clearing moved)")
			}
			if n := m.r32FailedFor(p.wr.ID); n != 1 {
				t.Fatalf("withdrawal_failed postings for the withdrawal: %d", n)
			}
			// A-15: the attempt stays exactly as parked; the write-once snapshot is never touched.
			if a := m.attempt(p.fresh.ID); a.State != AttemptDisputed || a.TerminalReason == nil || *a.TerminalReason != r32Reason ||
				a.ProviderReference == nil || *a.ProviderReference != p.ref {
				t.Fatalf("attempt changed by M4: %s %v %v", a.State, a.TerminalReason, a.ProviderReference)
			}
			if got := m.r32Snapshot(p.fresh.ID); got != snap {
				t.Fatalf("the snapshot changed: %q -> %q", snap, got)
			}
			// Audit: requested then executed on the resolution; withdrawal.failed with the M4 reason.
			if acts := m.auditActions("payment_manual_resolution", res.ID.String()); strings.Join(acts, ",") != "payment.manual_resolution_requested,payment.manual_resolution_executed" {
				t.Fatalf("audit trail: %v", acts)
			}
			ar := m.sysQuery(`SELECT metadata->>'evidence_verdict' AS v, metadata->>'evidence_line_id' AS l, metadata->>'import_seals_verified' AS s
				FROM audit_log WHERE tenant_id = $1 AND target_id = $2 AND action = 'payment.manual_resolution_requested'`, m.f.tenantID, res.ID.String())
			if len(ar) != 1 || ar[0]["v"] != "not_paid" || ar[0]["l"] != ev.LineID.String() || ar[0]["s"] != "1" {
				t.Fatalf("request audit row lacks the evidence: %v", ar)
			}
			if n := m.countRows(`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'withdrawal.failed' AND metadata->>'reason_code' = $2 AND target_id = $3`,
				m.f.tenantID, M4EvidenceNotPaidReason, p.wr.ID.String()); n != 1 {
				t.Fatalf("withdrawal.failed audit rows with the M4 reason: %d", n)
			}
			// Idempotent replay: a further decision is refused and posts nothing; a new request
			// (either kind) is refused; still exactly one posting.
			if _, err := m.decide(m.acting3, res, ResolutionApprove); !errors.Is(err, ErrResolutionNotPending) {
				t.Fatalf("decide after executed: %v", err)
			}
			if _, err := m.decide(m.acting2, res, ResolutionApprove); !errors.Is(err, ErrResolutionNotPending) {
				t.Fatalf("re-decide by the same approver after executed: %v", err)
			}
			for _, k := range []ResolutionKind{ResolutionM4EvidenceNotPaid, ResolutionM4EvidencePaid} {
				if _, err := m.request(m.acting, m.m4In(p.fresh.ID, k, ev.LineID)); err == nil {
					t.Fatalf("a second %s on an executed integrity park was accepted", k)
				}
			}
			if n := m.r32FailedFor(p.wr.ID); n != 1 {
				t.Fatalf("withdrawal_failed postings after replay: %d", n)
			}
			m.assertInvariants()
		})
	}
}

// M4 PAID stays refused for this reason, in the database (MR012 at request, whatever the
// evidence) and in Go (the scope restatement and the execution precondition). M2 is refused too.
func TestR32_IntegrityPark_M4PaidRefused_DBAndGo(t *testing.T) {
	m := newM4World(t)
	// A SUCCESS reported against a damaged snapshot: the most tempting case for "paid".
	p := m.r32Park(920, r32Missing, true)
	snap := m.r32Snapshot(p.fresh.ID)
	m.ingest(m4Imp{}, m.line(m4Ref(), p.fresh.MerchantReference, statement.PaymentStatusSucceeded, 920, time.Now()))
	_, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, &m4AnyLine))
	k3RequireCode(t, err, "MR012")
	for _, k := range []ResolutionKind{ResolutionM2DeclarePaid, ResolutionM2DeclareNotPaid} {
		_, err := m.request(m.acting, m.m2In(p.fresh.ID, k))
		k3RequireCode(t, err, "MR012")
	}
	m.r32Held(p, snap, "paid / M2 refused")
	// The DB function and its Go restatement, both reference shapes.
	m.tx(func(ctx context.Context, tx pgx.Tx) error {
		for _, ref := range []*string{nil, k3StrPtr("x-ref")} {
			var paid, notPaid bool
			if err := tx.QueryRow(ctx, `SELECT payment_m4_in_scope('m4_evidence_paid', 'disputed', $1, $2), payment_m4_in_scope('m4_evidence_not_paid', 'disputed', $1, $2)`,
				r32Reason, ref).Scan(&paid, &notPaid); err != nil {
				return err
			}
			r := r32Reason
			if paid || M4ResolvableDispute(ResolutionM4EvidencePaid, AttemptDisputed, &r, ref) {
				t.Errorf("M4 paid admitted for %s (ref=%v)", r32Reason, ref != nil)
			}
			if !notPaid || !M4ResolvableDispute(ResolutionM4EvidenceNotPaid, AttemptDisputed, &r, ref) {
				t.Errorf("M4 not-paid not admitted for %s (ref=%v)", r32Reason, ref != nil)
			}
		}
		return nil
	})
	// Go execution precondition: a (hypothetical, DB-impossible) paid row on this attempt is refused.
	pid := m.provider
	res := ManualResolution{Kind: ResolutionM4EvidencePaid, ProviderID: &pid, Amount: 920, AssetCode: "EUR", ProviderReferenceAtSubmission: k3StrPtr(p.ref)}
	if got := m.svc.m4ExecutionRefusal(res, m.attempt(p.fresh.ID), m.wd(p.wr.ID)); got != resolutionRefusedNotAllowed {
		t.Fatalf("Go execution precondition for a paid M4 on an integrity park: %q", got)
	}
	res.Kind = ResolutionM4EvidenceNotPaid
	if got := m.svc.m4ExecutionRefusal(res, m.attempt(p.fresh.ID), m.wd(p.wr.ID)); got != "" {
		t.Fatalf("Go execution precondition for not-paid: %q", got)
	}
}

// R19-1: tenant staff can neither request nor finally approve an M4 on an integrity park
// (MR060: the tenant session cannot see the whole evidence scope); a tenant approval
// counts but never meets the platform floor; the requester cannot approve its own request.
func TestR32_IntegrityPark_TenantStaffCannotRequestOrApprove(t *testing.T) {
	m := newM4World(t)
	p := m.r32Park(930, r32Tampered, false)
	snap := m.r32Snapshot(p.fresh.ID)
	ev := m.r32Decline(p, p.ref)
	_, err := m.request(m.f1, m.m4In(p.fresh.ID, ResolutionM4EvidenceNotPaid, ev.LineID))
	k3RequireCode(t, err, "MR060")
	if ClassifyResolutionError(err) != ResolutionErrForbidden {
		t.Fatalf("class: %s", ClassifyResolutionError(err))
	}
	m.r32Held(p, snap, "tenant request refused")
	r, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidenceNotPaid, ev.LineID))
	k3RequireNoErr(t, err, "platform request")
	if _, err := m.decide(m.acting, r, ResolutionApprove); err == nil {
		t.Fatal("the requester approved its own M4")
	}
	for _, s := range []k3Staff{m.f1, m.f2} {
		out, err := m.decide(s, r, ResolutionApprove)
		if err == nil && out.Executed {
			t.Fatalf("a tenant-staff approval executed an M4 on an integrity park: %+v", out)
		}
	}
	if got := m.resolution(r.ID); got.State != ResolutionPending {
		t.Fatalf("state after tenant approvals: %s", got.State)
	}
	// A tenant session cannot move it to executing directly either.
	err = m.inExecuting(r, m.f3, func(context.Context, pgx.Tx) error { return nil })
	if c := k3Code(err); c != "MR030" && c != "MR060" {
		t.Fatalf("tenant executing: %v", err)
	}
	m.r32Held(p, snap, "tenant approvals only")
	out, err := m.decide(m.acting2, r, ResolutionApprove)
	k3RequireNoErr(t, err, "platform final approval")
	if !out.Executed || m.wd(p.wr.ID).State != withdrawal.StateFailed {
		t.Fatalf("not executed after the platform approval: %+v", out)
	}
	m.assertInvariants()
}

// Four-eyes: with a policy requiring two approvals, one platform approver is not enough
// (no posting, park held); the second distinct platform approver executes.
func TestR32_IntegrityPark_SingleApproverCannotExecute(t *testing.T) {
	m := newM4WorldBase(t, 2)
	p := m.r32Park(940, r32Missing, false)
	snap := m.r32Snapshot(p.fresh.ID)
	ev := m.r32Decline(p, "r32-D-"+uuid.NewString()[:12])
	r, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidenceNotPaid, ev.LineID))
	k3RequireNoErr(t, err, "request")
	out, err := m.decide(m.acting2, r, ResolutionApprove)
	k3RequireNoErr(t, err, "first approval")
	if out.Executed || out.Counted != 1 || out.Required != 2 {
		t.Fatalf("one approver executed (or counted wrong): %+v", out)
	}
	if _, err := m.decide(m.acting2, r, ResolutionApprove); err == nil {
		if got := m.resolution(r.ID); got.State != ResolutionPending {
			t.Fatalf("a repeated approval by the same person executed: %s", got.State)
		}
	}
	m.r32Held(p, snap, "single approver")
	out, err = m.decide(m.acting3, r, ResolutionApprove)
	k3RequireNoErr(t, err, "second approval")
	if !out.Executed || m.r32FailedFor(p.wr.ID) != 1 {
		t.Fatalf("not executed by the second distinct approver: %+v", out)
	}
	m.assertInvariants()
}

// Ambiguous, insufficient or contradictory evidence keeps the park: every request is refused
// (MR062) and nothing moves. Includes evidence that turns contradictory AFTER the request
// (refused at execution by Go first, the DB re-check behind it).
func TestR32_IntegrityPark_InsufficientEvidenceKeepsThePark(t *testing.T) {
	m := newM4World(t)
	cov := func(p *b11Parked, end time.Duration) m4Imp {
		return m4Imp{start: p.fresh.CreatedAt.Add(-time.Minute), end: p.fresh.LastSentAt.Add(end)}
	}
	now := time.Now()
	cases := []struct {
		name  string
		setup func(p *b11Parked)
	}{
		{"no evidence at all", func(p *b11Parked) {}},
		{"pending line on the bound reference", func(p *b11Parked) {
			m.ingest(cov(p, 25*time.Hour), m.line(m4Ref(), p.fresh.MerchantReference, "declined", p.fresh.Amount, now),
				m.line(p.ref, p.fresh.MerchantReference, "pending", p.fresh.Amount, now))
		}},
		{"reversed line on the merchant reference", func(p *b11Parked) {
			m.ingest(cov(p, 25*time.Hour), m.line(m4Ref(), p.fresh.MerchantReference, "declined", p.fresh.Amount, now),
				m.line(m4Ref(), p.fresh.MerchantReference, "reversed", p.fresh.Amount, now))
		}},
		{"succeeded line on the bound reference (contradictory)", func(p *b11Parked) {
			m.ingest(cov(p, 25*time.Hour), m.line(m4Ref(), p.fresh.MerchantReference, "declined", p.fresh.Amount, now),
				m.line(p.ref, p.fresh.MerchantReference, "succeeded", p.fresh.Amount, now))
		}},
		{"succeeded line in an UNSEALED import", func(p *b11Parked) {
			m.ingest(cov(p, 25*time.Hour), m.line(m4Ref(), p.fresh.MerchantReference, "declined", p.fresh.Amount, now))
			m.ingest(m4Imp{unsealed: true}, m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", p.fresh.Amount, now))
		}},
		{"decline only in an unsealed import", func(p *b11Parked) {
			o := cov(p, 25*time.Hour)
			o.unsealed = true
			m.ingest(o, m.line(m4Ref(), p.fresh.MerchantReference, "declined", p.fresh.Amount, now))
		}},
		{"coverage too short", func(p *b11Parked) {
			m.ingest(cov(p, 23*time.Hour), m.line(m4Ref(), p.fresh.MerchantReference, "declined", p.fresh.Amount, now))
		}},
		{"decline of another amount", func(p *b11Parked) {
			m.ingest(cov(p, 25*time.Hour), m.line(m4Ref(), p.fresh.MerchantReference, "declined", p.fresh.Amount-1, now))
		}},
		{"source does not declare merchant references", func(p *b11Parked) {
			o := cov(p, 25*time.Hour)
			o.noDecl = true
			m.ingest(o, m.line(m4Ref(), p.fresh.MerchantReference, "declined", p.fresh.Amount, now))
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m.t = t
			p := m.r32Park(int64(950+i), r32Missing, false)
			snap := m.r32Snapshot(p.fresh.ID)
			c.setup(p)
			ev, err := m.evidence(p.fresh.ID)
			if err != nil || ev.Verdict == M4VerdictNotPaid || ev.LineID != nil {
				t.Fatalf("verdict must not be not_paid: %+v %v", ev, err)
			}
			_, err = m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidenceNotPaid, &m4AnyLine))
			k3RequireCode(t, err, "MR062")
			m.r32Held(p, snap, c.name)
		})
	}
	t.Run("contradicted after the request", func(t *testing.T) {
		m.t = t
		p := m.r32Park(990, r32Tampered, false)
		snap := m.r32Snapshot(p.fresh.ID)
		ev := m.r32Decline(p, p.ref)
		r, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidenceNotPaid, ev.LineID))
		k3RequireNoErr(t, err, "request")
		m.ingest(m4Imp{unsealed: true}, m.line(p.ref, "", "succeeded", p.fresh.Amount, time.Now()))
		out, err := m.decide(m.acting2, r, ResolutionApprove)
		if err == nil && out.Executed {
			t.Fatalf("executed on contradicted evidence: %+v", out)
		}
		if err == nil && (!out.Refused || m.resolution(r.ID).State != ResolutionRefusedAtExecute) {
			t.Fatalf("want refused_at_execution: %+v", out)
		}
		m.r32Held(p, snap, "contradicted after the request")
	})
}

// "Remain parked" is the default and nothing automatic leaves it: the sweeper, the poll
// (success or decline, matching or differing echo) and provider callbacks (success,
// decline, with and without an echo) change nothing - no destination write, no release,
// no settlement, no resend. Then the governed exit still works.
func TestR32_IntegrityPark_NoAutomaticExit_ProviderCannotChangeDestinationOrRelease(t *testing.T) {
	m := newM4World(t)
	p := m.r32Park(1010, r32Tampered, false)
	snap := m.r32Snapshot(p.fresh.ID)
	echo := &payoutinstrument.DestinationEcho{Fingerprint: strings.Repeat("ef", 32), Kid: "r32-other"}
	for i := 0; i < 2; i++ {
		_ = m.b11Sweeper().RunOnce(context.Background(), []uuid.UUID{m.f.tenantID})
		_ = m.b11Sweeper().processPayoutAttempt(context.Background(), m.f.tenantID, m.attempt(p.fresh.ID))
		for _, st := range []StatusResult{
			{Outcome: OutcomeSucceeded, ProviderReference: p.ref, Amount: 1010, AssetCode: "EUR"},
			{Outcome: OutcomeSucceeded, ProviderReference: p.ref, Amount: 1010, AssetCode: "EUR", DestinationEcho: echo},
			{Outcome: OutcomeDeclined, ProviderReference: p.ref, Amount: 1010, AssetCode: "EUR", DeclineReason: "insufficient_funds"},
		} {
			m.prov.setStatus(p.ref, st)
			// Both the fresh (disputed) copy and a stale pending copy a worker might still hold.
			for _, a := range []PaymentAttempt{m.attempt(p.fresh.ID), p.staleAs(AttemptPending)} {
				err := PollPayoutStatus(context.Background(), m.pool, m.orch, MockCredentialResolver{}, m.f.tenantID, a, time.Now().Add(time.Minute), nil)
				b11OKOrConflict(t, err, "poll on the integrity park")
			}
		}
		for _, cb := range []struct {
			o Outcome
			e *payoutinstrument.DestinationEcho
		}{{OutcomeSucceeded, nil}, {OutcomeSucceeded, echo}, {OutcomeDeclined, nil}, {OutcomeDeclined, echo}} {
			if _, err := m.pm4Callback(p.fresh, cb.o, p.ref, 1010, cb.e); err != nil && !errors.Is(err, ErrAttemptStateConflict) {
				t.Fatalf("callback %s echo=%v: %v", cb.o, cb.e != nil, err)
			}
		}
		m.r32Held(p, snap, "automatic paths on the integrity park")
	}
	if c := m.prov.calls[k3CallWithdraw].Load(); c != p.base.withdrawCalls {
		t.Fatalf("a provider Withdraw call was made: %d -> %d", p.base.withdrawCalls, c)
	}
	// GOV-R32 review (LF HIGH): the provider SUCCESSES above were recorded durably on the park, so
	// even a sealed declaring decline can no longer release the hold: not-paid is contradictory and
	// the park remains (the governed exit is refused, never forced).
	if ev := m.r32ParkEvidence(p.fresh.ID); !ev["succeeded/query_status"] || !ev["succeeded/callback"] {
		t.Fatalf("the reported successes were not recorded: %v", ev)
	}
	m.declineOn(p, "r32-D-"+uuid.NewString()[:12])
	m.mustEvidence(p.fresh.ID, M4VerdictContradictory)
	_, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidenceNotPaid, &m4AnyLine))
	k3RequireCode(t, err, "MR062")
	m.r32Held(p, snap, "not-paid after recorded provider successes")

	// A park that only ever saw non-success noise keeps its governed exit.
	q := m.r32Park(1011, r32Missing, false)
	qsnap := m.r32Snapshot(q.fresh.ID)
	m.prov.setStatus(q.ref, StatusResult{Outcome: OutcomeDeclined, ProviderReference: q.ref, Amount: 1011, AssetCode: "EUR", DeclineReason: "insufficient_funds"})
	for i := 0; i < 2; i++ {
		_ = m.b11Sweeper().RunOnce(context.Background(), []uuid.UUID{m.f.tenantID})
		b11OKOrConflict(t, PollPayoutStatus(context.Background(), m.pool, m.orch, MockCredentialResolver{}, m.f.tenantID, m.attempt(q.fresh.ID), time.Now().Add(time.Minute), nil), "decline poll")
		if _, err := m.pm4Callback(q.fresh, OutcomeDeclined, q.ref, 1011, nil); err != nil && !errors.Is(err, ErrAttemptStateConflict) {
			t.Fatalf("decline callback: %v", err)
		}
		m.r32Held(q, qsnap, "decline noise")
	}
	ev := m.r32Decline(q, "r32-D-"+uuid.NewString()[:12])
	m.execute(q, ResolutionM4EvidenceNotPaid, ev)
	if m.wd(q.wr.ID).State != withdrawal.StateFailed || m.r32FailedFor(q.wr.ID) != 1 {
		t.Fatal("the governed exit did not execute exactly once")
	}
	if got := m.r32Snapshot(q.fresh.ID); got != qsnap {
		t.Fatalf("the snapshot changed: %q -> %q", qsnap, got)
	}
	m.assertInvariants()
}

// Concurrency: two final approvals race on an integrity park; exactly one executes and
// exactly one withdrawal_failed is posted for the withdrawal.
func TestR32_IntegrityPark_Concurrency_TwoFinalApprovals(t *testing.T) {
	m := newM4World(t)
	for i := 0; i < 4; i++ {
		p := m.r32Park(int64(1020+i), r32Missing, false)
		ev := m.r32Decline(p, "r32-D-"+uuid.NewString()[:12])
		res, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidenceNotPaid, ev.LineID))
		k3RequireNoErr(t, err, "request")
		var wg sync.WaitGroup
		outs := make([]ResolutionOutcome, 2)
		errs := make([]error, 2)
		for j, s := range []k3Staff{m.acting2, m.acting3} {
			wg.Add(1)
			go func(j int, s k3Staff) {
				defer wg.Done()
				outs[j], errs[j] = m.decide(s, res, ResolutionApprove)
			}(j, s)
		}
		wg.Wait()
		executed := 0
		for j := range outs {
			if errs[j] == nil && outs[j].Executed {
				executed++
			}
		}
		if executed != 1 {
			t.Fatalf("iteration %d: executed %d times (errs %v)", i, executed, errs)
		}
		if n := m.r32FailedFor(p.wr.ID); n != 1 {
			t.Fatalf("iteration %d: withdrawal_failed postings %d", i, n)
		}
		if a := m.attempt(p.fresh.ID); a.State != AttemptDisputed || *a.TerminalReason != r32Reason {
			t.Fatalf("iteration %d: attempt %s %v", i, a.State, a.TerminalReason)
		}
	}
	m.assertInvariants()
}

// Cross-tenant: tenant B's platform principals (granted in B) cannot request on, or
// evaluate, tenant A's integrity park; B's statement lines naming A's merchant reference
// and A's bound reference never make A's verdict positive.
func TestR32_IntegrityPark_CrossTenant(t *testing.T) {
	a := newM4World(t)
	b := newM4WorldOn(t, a.pool)
	p := a.r32Park(1030, r32Missing, false)
	snap := a.r32Snapshot(p.fresh.ID)
	b.ingest(m4Imp{start: p.fresh.CreatedAt.Add(-time.Minute), end: p.fresh.LastSentAt.Add(25 * time.Hour)},
		b.line(p.ref, p.fresh.MerchantReference, statement.PaymentStatusDeclined, 1030, time.Now()))
	a.mustEvidence(p.fresh.ID, M4VerdictInsufficient)
	_, err := b.request(b.acting, b.m4In(p.fresh.ID, ResolutionM4EvidenceNotPaid, &m4AnyLine))
	if err == nil {
		t.Fatal("tenant B requested an M4 on tenant A's attempt")
	}
	// Refused fail-closed by whichever guard runs first in B's session: S-12 cannot
	// resolve the beneficiary Person (MR032), or the attempt is not in the session
	// tenant (MR010), or the evidence scope is invisible (MR060).
	if c := k3Code(err); c != "MR032" && c != "MR010" && c != "MR060" {
		t.Fatalf("cross-tenant request: %v", err)
	}
	if _, err := b.evidence(p.fresh.ID); k3Code(err) != "MR060" {
		t.Fatalf("cross-tenant evaluation must be MR060, got %v", err)
	}
	a.r32Held(p, snap, "cross-tenant attempts")
	// A's own governed path still works and B is untouched.
	ev := a.r32Decline(p, "r32-D-"+uuid.NewString()[:12])
	a.execute(p, ResolutionM4EvidenceNotPaid, ev)
	if n := b.countRows(`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'withdrawal_failed'`, b.f.tenantID); n != 0 {
		t.Fatalf("tenant B got a posting: %d", n)
	}
	a.assertInvariants()
	b.assertInvariants()
}

// destination_mismatch cells are unchanged next to the integrity park: both reasons are
// not-paid only, paid is refused for both, and the two parks resolve independently.
func TestR32_DestinationMismatchCellsUnchanged_BesideIntegrityPark(t *testing.T) {
	m := newM4World(t)
	ip := m.r32Park(1040, r32Missing, false)
	wr, a := m.payout(1041)
	m.tx(func(ctx context.Context, tx pgx.Tx) error {
		return ApplyDisputeFromNonTerminal(ctx, tx, a.ID, EvidenceCallback, TerminalReasonDestinationMismatch)
	})
	a = m.attempt(a.ID)
	for _, id := range []uuid.UUID{ip.fresh.ID, a.ID} {
		_, err := m.request(m.acting, m.m4In(id, ResolutionM4EvidencePaid, &m4AnyLine))
		k3RequireCode(t, err, "MR012")
	}
	evI := m.r32Decline(ip, "r32-D-"+uuid.NewString()[:12])
	m.ingest(m4Imp{start: a.CreatedAt.Add(-time.Minute), end: a.LastSentAt.Add(25 * time.Hour)},
		m.line(*a.ProviderReference, a.MerchantReference, statement.PaymentStatusDeclined, 1041, time.Now()))
	evM := m.mustEvidence(a.ID, M4VerdictNotPaid)
	m.execute(ip, ResolutionM4EvidenceNotPaid, evI)
	if m.wd(wr.ID).State != withdrawal.StateSubmitted {
		t.Fatal("resolving the integrity park touched the mismatch park")
	}
	r, err := m.request(m.acting, m.m4In(a.ID, ResolutionM4EvidenceNotPaid, evM.LineID))
	k3RequireNoErr(t, err, "mismatch request")
	out, err := m.decide(m.acting2, r, ResolutionApprove)
	k3RequireNoErr(t, err, "mismatch approve")
	if !out.Executed || m.wd(wr.ID).State != withdrawal.StateFailed || m.r32FailedFor(wr.ID) != 1 || m.r32FailedFor(ip.wr.ID) != 1 {
		t.Fatalf("independent resolution failed: %+v", out)
	}
	m.assertInvariants()
}

// Reconciliation: before M4 the integrity park's captured-unposted finding carries the
// integrity hint (M4 not-paid named, paid refused); after an executed M4 not-paid a
// succeeded line on the bound reference raises R-1 with the post-M4 hint (the
// destination_mismatch treatment, reason-agnostic code); a full K2 recovery stops R-1
// while the BOUND finding keeps raising (RR1-2, exactly as for destination_mismatch).
func TestR32_IntegrityPark_Recon_HintR1AndRR1(t *testing.T) {
	m := newM4World(t)
	m.k2Setup()
	p := m.r32Park(1050, r32Missing, false)
	pre := m.stmtRun(m.source(false, m.payoutLine(p.ref, "", statement.PaymentStatusSucceeded, 1050)))
	cu := m.requireOne(pre, reconciliation.MismatchKindPayCapturedUnposted, p.fresh.ID, "integrity park with a succeeded line on X")
	if cu.ExpectedValue != "payout parked on a destination integrity failure: no completion against the player's hold and no M4 paid; M4 not-paid only on positive decline evidence; PSP recall/return or off-platform recovery; never allocation" {
		t.Fatalf("integrity hint: %q", cu.ExpectedValue)
	}
	// That succeeded line now blocks not-paid (contradictory evidence keeps the park).
	if ev, _ := m.evidence(p.fresh.ID); ev.Verdict == M4VerdictNotPaid {
		t.Fatal("a succeeded line on X must block not-paid")
	}

	q := m.r32Park(1051, r32Tampered, false)
	ev := m.r32Decline(q, q.ref)
	out := m.execute(q, ResolutionM4EvidenceNotPaid, ev)
	l := m.rr1Line(q.ref, q.fresh.MerchantReference, 1051, "EUR")
	m.rr1RequireRaised(m.stmtRun(m.source(false, l)), q.fresh.ID, "R-1 after integrity not-paid")
	m.rr1Debit(1051, *out.Resolution.LedgerTransactionID, "full recovery")
	ms := m.stmtRun(m.source(false))
	cuN, r1 := rr1Raised(ms, q.fresh.ID)
	if r1 != 0 || cuN != 1 {
		t.Fatalf("want R-1 stopped and the bound finding kept (as destination_mismatch), got cu=%d r1=%d:\n%s", cuN, r1, render(ms))
	}
	m.assertInvariants()
}

// --- migration 0127 ------------------------------------------------------------------------------

const r32MigrationVersion = 127

func TestR32_Migration0127UpDownUp_WholeSchema(t *testing.T) {
	pool, _ := scratchThrough(t, "r32_", r32MigrationVersion-1)
	scope := func() (bool, bool) {
		var np, p bool
		if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT payment_m4_in_scope('m4_evidence_not_paid', 'disputed', 'destination_integrity_failure', 'x'),
				payment_m4_in_scope('m4_evidence_paid', 'disputed', 'destination_integrity_failure', NULL)`).Scan(&np, &p)
		}); err != nil {
			t.Fatal(err)
		}
		return np, p
	}
	preSnap := schemaSnapshot15(t, pool)
	if np, _ := scope(); np {
		t.Fatal("0126 schema already admits the integrity reason")
	}
	dir := migration0101Dir(t, r32MigrationVersion)
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("up: %v", err)
	}
	upSnap := schemaSnapshot15(t, pool)
	// Set differences computed here (snapDiff truncates its rendering).
	setOf := func(snap string) map[string]bool {
		out := map[string]bool{}
		for _, l := range strings.Split(snap, "\n") {
			out[l] = true
		}
		return out
	}
	pre, up := setOf(preSnap), setOf(upSnap)
	var removed, added []string
	for l := range pre {
		if !up[l] {
			removed = append(removed, l)
		}
	}
	for l := range up {
		if !pre[l] {
			added = append(added, l)
		}
	}
	for _, l := range added {
		if !strings.Contains(l, "payment_m4_in_scope(") && !strings.Contains(l, "payout_m4_evidence(") &&
			!strings.Contains(l, "payout_destination_park_evidence") {
			t.Errorf("0127 added something outside its declared objects: %s", l)
		}
	}
	if len(removed) != 2 {
		t.Errorf("want exactly the two replaced function bodies removed, got %d: %v", len(removed), removed)
	}
	for _, l := range removed {
		if !strings.HasPrefix(l, "function:payment_m4_in_scope(") && !strings.HasPrefix(l, "function:payout_m4_evidence(") {
			t.Errorf("0127 removed/replaced something other than the two functions: %s", l)
		}
	}
	if np, p := scope(); !np || p {
		t.Fatalf("0127 up: not-paid=%v paid=%v", np, p)
	}
	if _, err := pool.MigrateDown(context.Background(), dir, 1); err != nil {
		t.Fatalf("down on an empty 0127: %v", err)
	}
	if got := schemaSnapshot15(t, pool); got != preSnap {
		t.Fatalf("0127 down did not restore the 0126 schema exactly:\n%s", snapDiff(preSnap, got))
	}
	if np, _ := scope(); np {
		t.Fatal("0127 down: still admits the integrity reason")
	}
	// The restored bodies are the 0125 text byte for byte.
	b, err := os.ReadFile(realMigrationsDir(t) + "/0125_payout_unbound_resolution_m4.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct{ name, header string }{
		{"payment_m4_in_scope", "CREATE FUNCTION payment_m4_in_scope(p_kind text, p_state text, p_reason text, p_ref text) RETURNS boolean AS $$"},
		{"payout_m4_evidence", "CREATE FUNCTION payout_m4_evidence(p_tenant uuid, p_attempt uuid)\n    RETURNS TABLE (verdict text, line_id uuid, reference text, import_ids uuid[]) AS $$"},
	} {
		var src string
		if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT prosrc FROM pg_proc WHERE proname = $1 AND pronamespace = 'public'::regnamespace`, f.name).Scan(&src)
		}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), f.header+src+"$$") {
			t.Fatalf("0127 down did not restore the 0125 body of %s byte for byte:\n%s", f.name, src)
		}
	}
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("re-up: %v", err)
	}
	if got := schemaSnapshot15(t, pool); got != upSnap {
		t.Fatalf("0127 re-up differs from the first up:\n%s", snapDiff(upSnap, got))
	}
}

// r32DownTo0126 rolls back every migration above 0127 (none today), then 0127 itself.
func r32DownTo0126(t *testing.T, m *m4World) error {
	t.Helper()
	if n := migrationsAbove(t, r32MigrationVersion); n > 0 {
		if _, err := m.pool.MigrateDown(context.Background(), realMigrationsDir(t), n); err != nil {
			t.Fatalf("down above 0127 (empty): %v", err)
		}
	}
	_, err := m.pool.MigrateDown(context.Background(), migration0101Dir(t, r32MigrationVersion), 1)
	return err
}

func TestR32_Migration0127DownRefusals(t *testing.T) {
	t.Run("pending integrity M4", func(t *testing.T) {
		m := newM4World(t)
		p := m.r32Park(1060, r32Missing, false)
		ev := m.r32Decline(p, p.ref)
		_, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidenceNotPaid, ev.LineID))
		k3RequireNoErr(t, err, "request")
		k3RequireCode(t, r32DownTo0126(t, m), "MR099")
	})
	t.Run("executed integrity M4", func(t *testing.T) {
		m := newM4World(t)
		p := m.r32Park(1061, r32Tampered, false)
		m.execute(p, ResolutionM4EvidenceNotPaid, m.r32Decline(p, p.ref))
		k3RequireCode(t, r32DownTo0126(t, m), "MR099")
		// The refused down left 0127 in force.
		var ok bool
		m.tx(func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT payment_m4_in_scope('m4_evidence_not_paid', 'disputed', 'destination_integrity_failure', NULL)`).Scan(&ok)
		})
		if !ok {
			t.Fatal("a refused down changed the scope")
		}
	})
	t.Run("only a destination_mismatch M4: down allowed", func(t *testing.T) {
		m := newM4World(t)
		_, a := m.payout(1062)
		m.tx(func(ctx context.Context, tx pgx.Tx) error {
			return ApplyDisputeFromNonTerminal(ctx, tx, a.ID, EvidenceCallback, TerminalReasonDestinationMismatch)
		})
		a = m.attempt(a.ID)
		m.ingest(m4Imp{start: a.CreatedAt.Add(-time.Minute), end: a.LastSentAt.Add(25 * time.Hour)},
			m.line(*a.ProviderReference, a.MerchantReference, statement.PaymentStatusDeclined, 1062, time.Now()))
		ev := m.mustEvidence(a.ID, M4VerdictNotPaid)
		_, err := m.request(m.acting, m.m4In(a.ID, ResolutionM4EvidenceNotPaid, ev.LineID))
		k3RequireNoErr(t, err, "request")
		if err := r32DownTo0126(t, m); err != nil {
			t.Fatalf("down with only an in-0125-scope M4 row: %v", err)
		}
	})
}
