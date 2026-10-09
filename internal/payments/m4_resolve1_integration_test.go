//go:build integration

// PAY-PAYOUT-UNBOUND-RESOLVE-1 (ADR 0111 section 4, migration 0125): the M4
// evidence-backed four-eyes resolution, against MOCK statement sources only.
// Owner decisions 9-12 (ADR 0095 section 44) are the oracle of every test: no
// automatic resolution, the park stays held until positive evidence, four-eyes
// with a complete audit, and nothing released or settled without the positive
// verdict.
package payments

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// --- normal paths ------------------------------------------------------------------

func TestM4_Paid_EndToEnd_MOCK(t *testing.T) {
	m := newM4World(t)
	p, r, ev := m.paidPark(700)
	holdBefore, clearingBefore := m.walletBalance("player_withdrawal_hold"), m.pspClearing()

	out := m.execute(p, ResolutionM4EvidencePaid, ev)
	res := out.Resolution
	if res.State != ResolutionExecuted || res.EvidenceReference == nil || *res.EvidenceReference != r ||
		res.EvidenceVerdict == nil || *res.EvidenceVerdict != M4VerdictPaid || res.EvidenceLineID == nil || *res.EvidenceLineID != *ev.LineID ||
		res.ProviderReferenceAtSubmission != nil || res.Amount != 700 || res.AssetCode != "EUR" || res.TargetState != nil {
		t.Fatalf("executed M4 row wrong: %+v", res)
	}
	wr := m.wd(p.wr.ID)
	if wr.State != withdrawal.StateCompleted || wr.ReleaseLedgerTransactionID == nil || *wr.ReleaseLedgerTransactionID != *res.LedgerTransactionID {
		t.Fatalf("withdrawal not completed by the M4 posting: %+v", wr)
	}
	rows := m.sysQuery(`SELECT transaction_type, idempotency_key, provider_id, provider_tx_id, correlation_id::text AS corr
		FROM ledger_transactions WHERE id = $1`, *res.LedgerTransactionID)
	if len(rows) != 1 || rows[0]["transaction_type"] != "withdrawal_completed" || rows[0]["idempotency_key"] != m.provider+":"+r ||
		rows[0]["provider_tx_id"] != r || rows[0]["provider_id"] != m.provider || rows[0]["corr"] != p.wr.ID.String() {
		t.Fatalf("completion keyed wrong: %v", rows)
	}
	if got := m.walletBalance("player_withdrawal_hold"); got != holdBefore-700 {
		t.Fatalf("hold: want %d, got %d", holdBefore-700, got)
	}
	if got := m.pspClearing(); got != clearingBefore+700 {
		t.Fatalf("psp_clearing: want %d, got %d", clearingBefore+700, got)
	}
	// A-15: the attempt stays exactly as parked.
	a := m.attempt(p.fresh.ID)
	if a.State != AttemptDisputed || !equalOptString(a.TerminalReason, p.fresh.TerminalReason) || a.ProviderReference != nil {
		t.Fatalf("attempt changed by M4: %s %v %v", a.State, a.TerminalReason, a.ProviderReference)
	}
	// Audit: requested, executed; the evidence is on the rows; the hash, never a reference text, of the portal confirmation.
	acts := m.auditActions("payment_manual_resolution", res.ID.String())
	if strings.Join(acts, ",") != "payment.manual_resolution_requested,payment.manual_resolution_executed" {
		t.Fatalf("audit trail: %v", acts)
	}
	ar := m.sysQuery(`SELECT metadata->>'evidence_reference' AS r, metadata->>'evidence_verdict' AS v, metadata->>'evidence_line_id' AS l,
		metadata->>'import_seals_verified' AS s, metadata->>'evidence_ref_hash' AS h
		FROM audit_log WHERE tenant_id = $1 AND target_id = $2 AND action = 'payment.manual_resolution_requested'`, m.f.tenantID, res.ID.String())
	if len(ar) != 1 || ar[0]["r"] != r || ar[0]["v"] != "paid" || ar[0]["l"] != ev.LineID.String() || ar[0]["s"] != "1" || ar[0]["h"] != k3EvidenceHash() {
		t.Fatalf("request audit row lacks the evidence: %v", ar)
	}
	if n := m.countRows(`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'withdrawal.completed' AND target_id = $2`, m.f.tenantID, p.wr.ID.String()); n != 1 {
		t.Fatalf("withdrawal.completed audit rows: %d", n)
	}
	m.assertInvariants()
}

func TestM4_NotPaid_EndToEnd_MOCK(t *testing.T) {
	m := newM4World(t)
	p, ev := m.notPaidPark(650)
	holdBefore, cashBefore := m.walletBalance("player_withdrawal_hold"), m.walletBalance("player_cash")
	out := m.execute(p, ResolutionM4EvidenceNotPaid, ev)
	res := out.Resolution
	if res.EvidenceVerdict == nil || *res.EvidenceVerdict != M4VerdictNotPaid || res.EvidenceReference != nil {
		t.Fatalf("executed not-paid row wrong: %+v", res)
	}
	wr := m.wd(p.wr.ID)
	if wr.State != withdrawal.StateFailed || wr.ReleaseLedgerTransactionID == nil || *wr.ReleaseLedgerTransactionID != *res.LedgerTransactionID {
		t.Fatalf("withdrawal not failed by the M4 posting: %+v", wr)
	}
	rows := m.sysQuery(`SELECT transaction_type, idempotency_key FROM ledger_transactions WHERE id = $1`, *res.LedgerTransactionID)
	if len(rows) != 1 || rows[0]["transaction_type"] != "withdrawal_failed" || rows[0]["idempotency_key"] != p.wr.ID.String()+":failed" {
		t.Fatalf("failure posting wrong: %v", rows)
	}
	if m.walletBalance("player_withdrawal_hold") != holdBefore-650 || m.walletBalance("player_cash") != cashBefore+650 {
		t.Fatal("hold was not returned to player_cash")
	}
	if a := m.attempt(p.fresh.ID); a.State != AttemptDisputed {
		t.Fatalf("attempt changed by M4: %s", a.State)
	}
	if n := m.countRows(`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'withdrawal.failed' AND metadata->>'reason_code' = $2 AND target_id = $3`,
		m.f.tenantID, M4EvidenceNotPaidReason, p.wr.ID.String()); n != 1 {
		t.Fatalf("withdrawal.failed audit rows with the M4 reason: %d", n)
	}
	m.assertInvariants()
}

// --- the deterministic verdict (section 4.4) ---------------------------------------------

func TestM4_Verdict_PaidConditions(t *testing.T) {
	m := newM4World(t)
	now := time.Now()
	type tc struct {
		name  string
		setup func(p *b11Parked) // lines and ledger state for the park
		want  string
	}
	cases := []tc{
		{"no evidence at all", func(p *b11Parked) {}, M4VerdictInsufficient},
		{"amount differs (I-1)", func(p *b11Parked) {
			m.ingest(m4Imp{}, m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", p.fresh.Amount+1, now))
		}, M4VerdictContradictory},
		{"asset differs (I-1)", func(p *b11Parked) {
			l := m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", p.fresh.Amount, now)
			l.AssetCode = "USD"
			m.ingest(m4Imp{}, l)
		}, M4VerdictContradictory},
		{"two references succeeded", func(p *b11Parked) {
			m.ingest(m4Imp{}, m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", p.fresh.Amount, now),
				m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", p.fresh.Amount, now))
		}, M4VerdictContradictory},
		{"declined on the merchant reference", func(p *b11Parked) {
			m.ingest(m4Imp{}, m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", p.fresh.Amount, now),
				m.line(m4Ref(), p.fresh.MerchantReference, "declined", p.fresh.Amount, now))
		}, M4VerdictContradictory},
		{"pending naming ONLY R (S-4 R lookup)", func(p *b11Parked) {
			r := m4Ref()
			m.ingest(m4Imp{}, m.line(r, p.fresh.MerchantReference, "succeeded", p.fresh.Amount, now), m.line(r, "", "pending", p.fresh.Amount, now))
		}, M4VerdictContradictory},
		{"reversed naming ONLY R, back-dated", func(p *b11Parked) {
			r := m4Ref()
			m.ingest(m4Imp{}, m.line(r, p.fresh.MerchantReference, "succeeded", p.fresh.Amount, now))
			m.ingest(m4Imp{unsealed: true}, m.line(r, "", "reversed", p.fresh.Amount, now.Add(-72*time.Hour)))
		}, M4VerdictContradictory},
		{"only an unsealed import", func(p *b11Parked) {
			m.ingest(m4Imp{unsealed: true}, m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", p.fresh.Amount, now))
		}, M4VerdictInsufficient},
		{"unsealed lines contradict", func(p *b11Parked) {
			m.ingest(m4Imp{}, m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", p.fresh.Amount, now))
			m.ingest(m4Imp{unsealed: true}, m.line(m4Ref(), p.fresh.MerchantReference, "declined", p.fresh.Amount, now))
		}, M4VerdictContradictory},
		{"R held by another attempt", func(p *b11Parked) {
			_, other := m.payout(1234)
			m.ingest(m4Imp{}, m.line(*other.ProviderReference, p.fresh.MerchantReference, "succeeded", p.fresh.Amount, now))
		}, M4VerdictContradictory},
		{"R is a deposit ledger key", func(p *b11Parked) {
			r := m4Ref()
			m.b11PostDepositKey(r)
			m.ingest(m4Imp{}, m.line(r, p.fresh.MerchantReference, "succeeded", p.fresh.Amount, now))
		}, M4VerdictContradictory},
		{"tombstone on R", func(p *b11Parked) {
			r := m4Ref()
			m.tombstone(r)
			m.ingest(m4Imp{}, m.line(r, p.fresh.MerchantReference, "succeeded", p.fresh.Amount, now))
		}, M4VerdictContradictory},
		{"provider:R is an idempotency key", func(p *b11Parked) {
			r := m4Ref()
			m.idemKeyOnly(r)
			m.ingest(m4Imp{}, m.line(r, p.fresh.MerchantReference, "succeeded", p.fresh.Amount, now))
		}, M4VerdictContradictory},
		{"control: one sealed succeeded line", func(p *b11Parked) {
			m.ingest(m4Imp{}, m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", p.fresh.Amount, now))
		}, M4VerdictPaid},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m.t = t
			p := m.park(int64(400 + i))
			c.setup(p)
			ev := m.mustEvidence(p.fresh.ID, c.want)
			if c.want != M4VerdictPaid && (ev.LineID != nil || ev.Reference != nil) {
				t.Fatalf("a non-positive verdict must carry no line or reference: %+v", ev)
			}
		})
	}
}

// The 'MOCK only' import is not eligible once ANY real import exists (RC-3);
// a MOCK line still CONTRADICTS a real one.
func TestM4_Verdict_MockIneligibleOnceRealExists(t *testing.T) {
	m := newM4World(t)
	p := m.park(410)
	m.ingest(m4Imp{}, m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", 410, time.Now()))
	m.mustEvidence(p.fresh.ID, M4VerdictPaid)
	m.ingest(m4Imp{real: true}, m.line(m4Ref(), "someone-else", "succeeded", 1, time.Now()))
	m.mustEvidence(p.fresh.ID, M4VerdictInsufficient)
	// Real (sealed) succeeded evidence is positive; a MOCK declined line contradicts it.
	p2 := m.park(411)
	m.ingest(m4Imp{real: true}, m.line(m4Ref(), p2.fresh.MerchantReference, "succeeded", 411, time.Now()))
	m.mustEvidence(p2.fresh.ID, M4VerdictPaid)
	m.ingest(m4Imp{}, m.line(m4Ref(), p2.fresh.MerchantReference, "declined", 411, time.Now()))
	m.mustEvidence(p2.fresh.ID, M4VerdictContradictory)
}

// L-1: the evidence line is the earliest qualifying copy by (fetched_at,
// import_id, line_no), stable across evaluations; import_ids are sorted.
func TestM4_Verdict_DeterministicLine(t *testing.T) {
	m := newM4World(t)
	p := m.park(420)
	l := m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", 420, time.Now())
	first := m.ingest(m4Imp{start: time.Now().Add(-2 * time.Hour)}, l)
	second := m.ingest(m4Imp{start: time.Now().Add(-3 * time.Hour)}, l)
	ev1 := m.mustEvidence(p.fresh.ID, M4VerdictPaid)
	ev2 := m.mustEvidence(p.fresh.ID, M4VerdictPaid)
	var lineImport uuid.UUID
	m.tx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT import_id FROM payment_statement_lines WHERE id = $1`, *ev1.LineID).Scan(&lineImport)
	})
	if lineImport != first || *ev1.LineID != *ev2.LineID || len(ev1.ImportIDs) != 2 {
		t.Fatalf("non-deterministic line: first=%s second=%s line-import=%s ev1=%+v ev2=%+v", first, second, lineImport, ev1, ev2)
	}
	if ev1.ImportIDs[0].String() > ev1.ImportIDs[1].String() {
		t.Fatalf("import_ids not ascending: %v", ev1.ImportIDs)
	}
}

func TestM4_Verdict_NotPaidConditions(t *testing.T) {
	m := newM4World(t)
	type tc struct {
		name  string
		setup func(p *b11Parked)
		want  string
	}
	cov := func(p *b11Parked, endAfterSend time.Duration) m4Imp {
		return m4Imp{start: p.fresh.CreatedAt.Add(-time.Minute), end: p.fresh.LastSentAt.Add(endAfterSend)}
	}
	cases := []tc{
		{"control", func(p *b11Parked) {
			m.ingest(cov(p, 25*time.Hour), m.line(m4Ref(), p.fresh.MerchantReference, "declined", p.fresh.Amount, time.Now()))
		}, M4VerdictNotPaid},
		{"(i) coverage ends before last_sent_at + 24h", func(p *b11Parked) {
			m.ingest(cov(p, 23*time.Hour), m.line(m4Ref(), p.fresh.MerchantReference, "declined", p.fresh.Amount, time.Now()))
		}, M4VerdictInsufficient},
		{"(i) coverage starts after created_at", func(p *b11Parked) {
			o := cov(p, 25*time.Hour)
			o.start = p.fresh.CreatedAt.Add(time.Second)
			m.ingest(o, m.line(m4Ref(), p.fresh.MerchantReference, "declined", p.fresh.Amount, time.Now()))
		}, M4VerdictInsufficient},
		{"(ii) decline before last_sent_at", func(p *b11Parked) {
			m.ingest(cov(p, 25*time.Hour), m.line(m4Ref(), p.fresh.MerchantReference, "declined", p.fresh.Amount, p.fresh.LastSentAt.Add(-time.Minute)))
		}, M4VerdictInsufficient},
		{"(ii) decline of another amount", func(p *b11Parked) {
			m.ingest(cov(p, 25*time.Hour), m.line(m4Ref(), p.fresh.MerchantReference, "declined", p.fresh.Amount-1, time.Now()))
		}, M4VerdictInsufficient},
		{"(iii) a pending line in ANY import", func(p *b11Parked) {
			m.ingest(cov(p, 25*time.Hour), m.line(m4Ref(), p.fresh.MerchantReference, "declined", p.fresh.Amount, time.Now()))
			m.ingest(m4Imp{unsealed: true}, m.line(m4Ref(), p.fresh.MerchantReference, "pending", p.fresh.Amount, time.Now()))
		}, M4VerdictInsufficient},
		{"(iii) a reversed line", func(p *b11Parked) {
			m.ingest(cov(p, 25*time.Hour), m.line(m4Ref(), p.fresh.MerchantReference, "declined", p.fresh.Amount, time.Now()),
				m.line(m4Ref(), p.fresh.MerchantReference, "reversed", p.fresh.Amount, time.Now()))
		}, M4VerdictInsufficient},
		{"(iv) the source does not declare merchant references", func(p *b11Parked) {
			o := cov(p, 25*time.Hour)
			o.noDecl = true
			m.ingest(o, m.line(m4Ref(), p.fresh.MerchantReference, "declined", p.fresh.Amount, time.Now()))
		}, M4VerdictInsufficient},
		{"unsealed import", func(p *b11Parked) {
			o := cov(p, 25*time.Hour)
			o.unsealed = true
			m.ingest(o, m.line(m4Ref(), p.fresh.MerchantReference, "declined", p.fresh.Amount, time.Now()))
		}, M4VerdictInsufficient},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m.t = t
			p := m.park(int64(500 + i))
			c.setup(p)
			m.mustEvidence(p.fresh.ID, c.want)
		})
	}
}

// I-2 / S-4: 64 lines is fine, the 65th is evidence_overflow (refused, never
// truncated), and the R lookups count against the same budget.
func TestM4_Verdict_OverflowAt65_IncludingR(t *testing.T) {
	m := newM4World(t)
	t.Run("merchant", func(t *testing.T) {
		m.t = t
		p := m.park(430)
		var ls []statement.PaymentStatementLine
		for i := 0; i < 64; i++ {
			ls = append(ls, m.line(m4Ref(), p.fresh.MerchantReference, "pending", 430, time.Now()))
		}
		m.ingest(m4Imp{}, ls...)
		if ev := m.mustEvidence(p.fresh.ID, M4VerdictInsufficient); len(ev.ImportIDs) != 1 {
			t.Fatalf("64 lines must be read in full: %+v", ev)
		}
		p2 := m.park(431)
		ls = nil
		for i := 0; i < 65; i++ {
			ls = append(ls, m.line(m4Ref(), p2.fresh.MerchantReference, "pending", 431, time.Now()))
		}
		m.ingest(m4Imp{}, ls...)
		m.mustEvidence(p2.fresh.ID, M4VerdictOverflow)
	})
	t.Run("R lookups", func(t *testing.T) {
		m.t = t
		p := m.park(432)
		r := m4Ref()
		ls := []statement.PaymentStatementLine{m.line(r, p.fresh.MerchantReference, "succeeded", 432, time.Now())}
		for i := 0; i < 60; i++ {
			ls = append(ls, m.line(m4Ref(), p.fresh.MerchantReference, "declined", 432, time.Now()))
		}
		for i := 0; i < 4; i++ { // 61 platform-keyed + 4 on R only = 65
			ls = append(ls, m.line(r, "", "pending", 432, time.Now().Add(time.Duration(i)*time.Second)))
		}
		m.ingest(m4Imp{}, ls...)
		m.mustEvidence(p.fresh.ID, M4VerdictOverflow)
		_, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, &m4AnyLine))
		k3RequireCode(t, err, "MR063")
		if tok := ResolutionToken(ClassifyResolutionError(err)); tok != TokenForceResolveEvidenceOverflow {
			t.Fatalf("token: %s", tok)
		}
	})
}

// --- scope, forced columns, visibility ----------------------------------------------------

func TestM4_Scope_DBAndGo(t *testing.T) {
	m := newM4World(t)
	// destination_mismatch: not-paid only (M-10); paid refused IN THE DATABASE.
	wr, a := m.payout(440)
	m.tx(func(ctx context.Context, tx pgx.Tx) error {
		return ApplyDisputeFromNonTerminal(ctx, tx, a.ID, EvidenceCallback, "destination_mismatch")
	})
	_, err := m.request(m.acting, m.m4In(a.ID, ResolutionM4EvidencePaid, &m4AnyLine))
	k3RequireCode(t, err, "MR012")
	// In scope for not-paid: with no evidence the refusal is the VERDICT (MR062), not the scope.
	_, err = m.request(m.acting, m.m4In(a.ID, ResolutionM4EvidenceNotPaid, &m4AnyLine))
	k3RequireCode(t, err, "MR062")
	if ResolutionToken(ClassifyResolutionError(err)) != TokenForceResolveEvidenceInsufficient {
		t.Fatalf("token for MR062")
	}
	_ = wr
	// An unbound reason WITH a reference (the poll park holds X): outside M4, both kinds (C-6).
	pp := m.b11ParkPoll(441, OutcomePending)
	for _, k := range []ResolutionKind{ResolutionM4EvidencePaid, ResolutionM4EvidenceNotPaid} {
		_, err := m.request(m.acting, m.m4In(pp.stale.ID, k, &m4AnyLine))
		k3RequireCode(t, err, "MR012")
	}
	m.b11Held(pp, "M4 refused on a referenced unbound park")
	// An ambiguous payout is not an M4 case.
	_, amb := m.ambiguousPayout(442)
	_, err = m.request(m.acting, m.m4In(amb.ID, ResolutionM4EvidencePaid, &m4AnyLine))
	k3RequireCode(t, err, "MR012")
}

// Go M4ResolvableDispute == DB payment_m4_in_scope over the reason matrix.
func TestM4_ScopeParity_GoAndDB(t *testing.T) {
	m := newM4World(t)
	reasons := []string{"invalid_provider_reference", "invalid_provider_reference:empty", "invalid_provider_reference:control_char",
		"provider_reference_conflict", "destination_mismatch", "provider_reference_mismatch", "success_for_never_sent_attempt",
		"amount_asset_mismatch", "late_success_after_terminal", "invalid_provider_referencex"}
	refs := []*string{nil, k3StrPtr("x-ref")}
	states := []AttemptState{AttemptDisputed, AttemptAmbiguous, AttemptPending}
	n := 0
	m.tx(func(ctx context.Context, tx pgx.Tx) error {
		for _, k := range []ResolutionKind{ResolutionM4EvidencePaid, ResolutionM4EvidenceNotPaid, ResolutionM2DeclarePaid} {
			for _, s := range states {
				for _, r := range reasons {
					for _, ref := range refs {
						r := r
						var db bool
						if err := tx.QueryRow(ctx, `SELECT payment_m4_in_scope($1, $2, $3, $4)`, string(k), string(s), r, ref).Scan(&db); err != nil {
							return err
						}
						if goV := M4ResolvableDispute(k, s, &r, ref); goV != db {
							t.Errorf("parity: kind=%s state=%s reason=%s ref=%v go=%v db=%v", k, s, r, ref != nil, goV, db)
						}
						if db {
							n++
						}
					}
				}
			}
		}
		return nil
	})
	// paid: the 4 unbound forms with no reference; not-paid: the same 4 + destination_mismatch with and without a reference.
	if n != 10 {
		t.Fatalf("admitted combinations: want 10, got %d", n)
	}
}

func TestM4_Request_ForcedColumns_And_EvidenceMismatch(t *testing.T) {
	m := newM4World(t)
	p, _, ev := m.paidPark(450)
	// A line other than the deterministic one: force_resolve_evidence_mismatch.
	other := uuid.New()
	_, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, &other))
	k3RequireCode(t, err, "MR061")
	if ResolutionToken(ClassifyResolutionError(err)) != TokenForceResolveEvidenceMismatch {
		t.Fatal("token for MR061")
	}
	// The verdict is paid, so a not-paid request is insufficient.
	_, err = m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidenceNotPaid, ev.LineID))
	k3RequireCode(t, err, "MR062")
	// R-4: a client amount / asset / evidence column is refused (raw insert, valid proof).
	for _, col := range []string{"amount", "asset_code", "evidence_reference", "evidence_verdict", "evidence_import_ids", "provider_reference_at_submission"} {
		col := col
		t.Run(col, func(t *testing.T) {
			err := m.pool.WithPlatformActingInTenant(context.Background(), m.acting.ID, m.f.tenantID, uuid.Nil, OperationKindForceResolve, func(ctx context.Context, tx pgx.Tx) error {
				in := m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID)
				if err := prooftest.AttachForSession(ctx, tx, actorproof.OpResolutionRequest, actorproof.TargetNew, m4Digest(m.f.tenantID, in)); err != nil {
					return err
				}
				val := map[string]any{"amount": "450", "asset_code": "EUR", "evidence_reference": "x", "evidence_verdict": "paid",
					"evidence_import_ids": "{" + uuid.NewString() + "}", "provider_reference_at_submission": "x"}[col]
				amount, asset := any(nil), any(nil)
				if col == "amount" {
					amount = val
				}
				if col == "asset_code" {
					asset = val
				}
				extraCol, extraVal := "", any(nil)
				if col != "amount" && col != "asset_code" {
					extraCol, extraVal = ", "+col, val
				}
				q := fmt.Sprintf(`INSERT INTO payment_manual_resolutions
					(tenant_id, attempt_id, operation, kind, basis_code, evidence_ref_hash, amount, asset_code, brand_id, reason_code,
					 attempt_state_at_submission, ever_possibly_sent_at_submission, payload_hash, requested_by, requested_by_scope,
					 requested_by_person_id, tenant_status_at_submission, required_at_submission, contributing_policy_ids, expires_at,
					 evidence_line_id%s)
					VALUES ($1, $2, 'payout', 'm4_evidence_paid', 'provider_confirmed_out_of_band', $3, $4::numeric, $5::text, $6, 'm4-test',
					 '-', false, '-', $6, 'tenant', $6, '-', 1, '{}', now(), $7%s)`, extraCol, map[bool]string{true: ", $8", false: ""}[extraCol != ""])
				args := []any{m.f.tenantID, p.fresh.ID, k3EvidenceHash(), amount, asset, uuid.Nil, *ev.LineID}
				if extraCol != "" {
					args = append(args, extraVal)
				}
				_, err := tx.Exec(ctx, q, args...)
				return err
			})
			k3RequireCode(t, err, "MR010")
		})
	}
	m.b11Held(p, "refused M4 requests change nothing")
}

func TestM4_Visibility_ErrorNeverVerdict(t *testing.T) {
	m := newM4World(t)
	p, _, ev := m.paidPark(460)
	// A tenant-staff session cannot see the typed reference evidence: MR060, never a verdict.
	err := m.pool.WithPrincipalScope(context.Background(), m.f.tenantID, m.f1.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := EvaluateM4Evidence(ctx, tx, m.f.tenantID, p.fresh.ID)
		return err
	})
	k3RequireCode(t, err, "MR060")
	// ... so a tenant-scoped M4 request is refused (403), audited by the route layer.
	_, err = m.request(m.f1, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID))
	k3RequireCode(t, err, "MR060")
	if ClassifyResolutionError(err) != ResolutionErrForbidden {
		t.Fatalf("class: %s", ClassifyResolutionError(err))
	}
	// An attempt the session cannot see (unknown id): an error.
	_, err = m.evidence(uuid.New())
	k3RequireCode(t, err, "MR060")
	// An acting session for this tenant evaluating ANOTHER tenant: an error.
	err = m.pool.WithPlatformActingInTenant(context.Background(), m.acting.ID, m.f.tenantID, uuid.Nil, OperationKindForceResolve, func(ctx context.Context, tx pgx.Tx) error {
		_, err := EvaluateM4Evidence(ctx, tx, uuid.New(), p.fresh.ID)
		return err
	})
	k3RequireCode(t, err, "MR060")
	m.b11Held(p, "visibility refusals change nothing")
}

// --- authorization: the platform_acting approver floor (S-6), four-eyes, proofs --------------

func TestM4_PlatformFloor_InTheDBRecount(t *testing.T) {
	m := newM4World(t)
	p, _, ev := m.paidPark(470)
	r, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID))
	k3RequireNoErr(t, err, "request")
	// Two TENANT approvers: counted 2 >= required 1, but no platform_acting approval: not executed.
	for _, s := range []k3Staff{m.f1, m.f2} {
		out, err := m.decide(s, r, ResolutionApprove)
		k3RequireNoErr(t, err, "tenant approve")
		if out.Executed {
			t.Fatal("an M4 executed with no platform_acting approver (S-6)")
		}
	}
	if got := m.resolution(r.ID); got.State != ResolutionPending {
		t.Fatalf("state: %s", got.State)
	}
	// The DB recount refuses -> executing on its own (a direct UPDATE by a third tenant approver).
	err = m.inExecuting(r, m.f3, func(context.Context, pgx.Tx) error { return nil })
	k3RequireCode(t, err, "MR030")
	// The requester cannot approve its own request (four-eyes).
	_, err = m.decide(m.acting, r, ResolutionApprove)
	if err == nil {
		t.Fatal("the requester approved its own M4")
	}
	// A platform_acting approval completes the floor and executes.
	out, err := m.decide(m.acting2, r, ResolutionApprove)
	k3RequireNoErr(t, err, "platform approve")
	if !out.Executed {
		t.Fatalf("not executed after the platform approval: %+v", out)
	}
	m.assertInvariants()
}

func m4Digest(tenantID uuid.UUID, in ResolutionRequestInput) string {
	return actorproof.Digest(actorproof.S(tenantID.String()), actorproof.S(in.AttemptID.String()), actorproof.S(string(in.Kind)),
		actorproof.SP(in.FindingCode), actorproof.SP(in.BasisCode), actorproof.SP(in.ContextCode), actorproof.SP(in.EvidenceRefHash),
		actorproof.S(in.ReasonCode), actorproof.SP(evidenceLineText(in.EvidenceLineID)))
}

// ADR 0110 L-1: the K3 INSERT digest carries evidence_line_id (SQL and Go in
// lockstep). A proof over the pre-0125 eight-field digest is refused (AP004),
// no proof is AP001, and a replayed proof is AP005.
func TestM4_ActorProof_DigestCarriesEvidenceLine(t *testing.T) {
	m := newM4World(t)
	p, _, ev := m.paidPark(480)
	in := m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID)
	insert := func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO payment_manual_resolutions
			(tenant_id, attempt_id, operation, kind, basis_code, evidence_ref_hash, amount, asset_code, brand_id, reason_code,
			 attempt_state_at_submission, ever_possibly_sent_at_submission, payload_hash, requested_by, requested_by_scope,
			 requested_by_person_id, tenant_status_at_submission, required_at_submission, contributing_policy_ids, expires_at, evidence_line_id)
			VALUES ($1, $2, 'payout', $3, $4, $5, NULL, NULL, $6, $7, '-', false, '-', $6, 'tenant', $6, '-', 1, '{}', now(), $8)`,
			m.f.tenantID, in.AttemptID, string(in.Kind), in.BasisCode, in.EvidenceRefHash, uuid.Nil, in.ReasonCode, in.EvidenceLineID)
		return err
	}
	acting := func(fn func(ctx context.Context, tx pgx.Tx) error) error {
		return m.pool.WithPlatformActingInTenant(context.Background(), m.acting.ID, m.f.tenantID, uuid.Nil, OperationKindForceResolve, fn)
	}
	k3RequireCode(t, acting(insert), "AP001")
	old := actorproof.Digest(actorproof.S(m.f.tenantID.String()), actorproof.S(in.AttemptID.String()), actorproof.S(string(in.Kind)),
		nil, actorproof.SP(in.BasisCode), nil, actorproof.SP(in.EvidenceRefHash), actorproof.S(in.ReasonCode))
	k3RequireCode(t, acting(func(ctx context.Context, tx pgx.Tx) error {
		if err := prooftest.AttachForSession(ctx, tx, actorproof.OpResolutionRequest, actorproof.TargetNew, old); err != nil {
			return err
		}
		return insert(ctx, tx)
	}), "AP004")
	// The new digest verifies (control), inside a rolled-back transaction.
	err := acting(func(ctx context.Context, tx pgx.Tx) error {
		if err := prooftest.AttachForSession(ctx, tx, actorproof.OpResolutionRequest, actorproof.TargetNew, m4Digest(m.f.tenantID, in)); err != nil {
			return err
		}
		if err := insert(ctx, tx); err != nil {
			return err
		}
		return errK3Rollback
	})
	if !errors.Is(err, errK3Rollback) {
		t.Fatalf("control: the 0125 digest must verify: %v", err)
	}
	// The digest of an M1/M2 row carries the trailing NULL ("~") field.
	var dbDigest string
	m.tx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT k2_sha256_hex(k2_canonical('a', 'b', 'c', NULL, NULL, NULL, NULL, 'r', NULL))`).Scan(&dbDigest)
	})
	if goDigest := actorproof.Digest(actorproof.S("a"), actorproof.S("b"), actorproof.S("c"), nil, nil, nil, nil, actorproof.S("r"), actorproof.SP(evidenceLineText(uuid.Nil))); goDigest != dbDigest {
		t.Fatalf("Go/SQL digest encodings differ: %s vs %s", goDigest, dbDigest)
	}
	m.b11Held(p, "proof refusals change nothing")
}

// --- seals (ADR 0110 T10) -----------------------------------------------------------

func TestM4_Seals_UnsealedTamperedAndPrincipalInsert(t *testing.T) {
	m := newM4World(t)
	// An import sealed under a key this process does not hold looks sealed to the
	// database (verdict paid) but never verifies in Go: refused, nothing persists.
	p := m.park(490)
	m.ingest(m4Imp{keys: m4NewKeys(t)}, m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", 490, time.Now()))
	ev := m.mustEvidence(p.fresh.ID, M4VerdictPaid)
	_, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID))
	if !errors.Is(err, ErrResolutionEvidenceUnsealed) || ResolutionToken(ClassifyResolutionError(err)) != TokenForceResolveEvidenceUnsealed {
		t.Fatalf("want the unsealed refusal, got %v", err)
	}
	if n := m.countRows(`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'payment.manual_resolution_requested'`, m.f.tenantID); n != 0 {
		t.Fatalf("a refused request left %d audit rows", n)
	}
	m.b11Held(p, "unverifiable seal")
	// A service with no import keys refuses every M4.
	p2, _, ev2 := m.paidPark(491)
	m.svc.WithImportSealKeys(nil)
	_, err = m.request(m.acting, m.m4In(p2.fresh.ID, ResolutionM4EvidencePaid, ev2.LineID))
	if !errors.Is(err, ErrResolutionEvidenceUnsealed) {
		t.Fatalf("no keys: %v", err)
	}
	m.svc.WithImportSealKeys(m.keys)
	// S-5: the import INSERT arms are the SYSTEM SHAPE only: a principal-shaped
	// (tenant staff) or acting session cannot write an import or a line.
	for name, run := range map[string]func(fn func(ctx context.Context, tx pgx.Tx) error) error{
		"tenant staff": func(fn func(ctx context.Context, tx pgx.Tx) error) error {
			return m.pool.WithPrincipalScope(context.Background(), m.f.tenantID, m.f1.ID, fn)
		},
		"acting": func(fn func(ctx context.Context, tx pgx.Tx) error) error {
			return m.pool.WithPlatformActingInTenant(context.Background(), m.acting.ID, m.f.tenantID, uuid.Nil, OperationKindForceResolve, fn)
		},
	} {
		err := run(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO payment_statement_imports (id, tenant_id, provider_id, source_label, is_mock, coverage_start,
				coverage_end, line_count, content_digest, fetched_at) VALUES ($1, $2, $3, 'MOCK forged', true, now() - interval '1 hour', now(), 0,
				decode(repeat('ab', 32), 'hex'), now())`, uuid.New(), m.f.tenantID, m.provider)
			return err
		})
		k3RequireCode(t, err, "42501")
		var importID uuid.UUID
		m.tx(func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT id FROM payment_statement_imports WHERE tenant_id = $1 LIMIT 1`, m.f.tenantID).Scan(&importID)
		})
		err = run(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO payment_statement_lines (tenant_id, import_id, line_no, provider_id, kind, provider_reference,
				merchant_reference, status, amount, asset_code, occurred_at) VALUES ($1, $2, 999999, $3, 'payout', 'forged', $4, 'succeeded', 1, 'EUR', now())`,
				m.f.tenantID, importID, m.provider, p.fresh.MerchantReference)
			return err
		})
		k3RequireCode(t, err, "42501")
		_ = name
	}
}

// --- execution: rollback, refusal, partial failure, retry, idempotency ---------------------

// S-2: the evidence is re-evaluated at execution; a contradiction that arrived
// after the request ends the resolution refused_at_execution (committed and
// audited); nothing is posted; the park stays held.
func TestM4_Execution_RefusedWhenEvidenceChanges(t *testing.T) {
	m := newM4World(t)
	p, _, ev := m.paidPark(510)
	r, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID))
	k3RequireNoErr(t, err, "request")
	m.ingest(m4Imp{unsealed: true}, m.line(m4Ref(), p.fresh.MerchantReference, "declined", 510, time.Now()))
	out, err := m.decide(m.acting2, r, ResolutionApprove)
	k3RequireNoErr(t, err, "approve")
	if out.Executed || !out.Refused || out.Resolution.RefusalCode == nil || *out.Resolution.RefusalCode != resolutionRefusedEvidence {
		t.Fatalf("want refused_at_execution/%s, got %+v", resolutionRefusedEvidence, out)
	}
	if wr := m.wd(p.wr.ID); wr.State != withdrawal.StateSubmitted || wr.ReleaseLedgerTransactionID != nil {
		t.Fatalf("the park was released: %+v", wr)
	}
	if acts := m.auditActions("payment_manual_resolution", r.ID.String()); !strings.Contains(strings.Join(acts, ","), "payment.manual_resolution_refused") {
		t.Fatalf("no refusal audit: %v", acts)
	}
	m.assertInvariants()
}

// The seal is re-verified at execution (S-2 step 7).
func TestM4_Execution_RefusedWhenSealNoLongerVerifies(t *testing.T) {
	m := newM4World(t)
	p, _, ev := m.paidPark(511)
	r, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID))
	k3RequireNoErr(t, err, "request")
	m.svc.WithImportSealKeys(m4NewKeys(t)) // the key that sealed the import is no longer held
	out, err := m.decide(m.acting2, r, ResolutionApprove)
	k3RequireNoErr(t, err, "approve")
	if !out.Refused || *out.Resolution.RefusalCode != resolutionRefusedUnsealed {
		t.Fatalf("want refused/%s, got %+v", resolutionRefusedUnsealed, out)
	}
	if wr := m.wd(p.wr.ID); wr.State != withdrawal.StateSubmitted {
		t.Fatalf("released: %s", wr.State)
	}
}

// Partial failure: a failure after `executing` and before the posting rolls the
// WHOLE final-approval transaction back (no approval, no posting, still
// pending); a retry then executes once.
func TestM4_PartialFailure_ThenRetry_ExactlyOnce(t *testing.T) {
	m := newM4World(t)
	p, r, ev := m.paidPark(520)
	res, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID))
	k3RequireNoErr(t, err, "request")
	testHookResolutionBeforePost = func(context.Context, uuid.UUID) error { return errors.New("injected failure before the posting") }
	_, err = m.decide(m.acting2, res, ResolutionApprove)
	testHookResolutionBeforePost = nil
	if err == nil {
		t.Fatal("the injected failure did not surface")
	}
	if got := m.resolution(res.ID); got.State != ResolutionPending {
		t.Fatalf("state after the failure: %s", got.State)
	}
	var approvals int
	if err := m.pool.WithPlatformActingInTenant(context.Background(), m.acting.ID, m.f.tenantID, res.ID, OperationKindForceResolve, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_manual_resolution_approvals WHERE resolution_id = $1`, res.ID).Scan(&approvals)
	}); err != nil || approvals != 0 {
		t.Fatalf("an approval survived the rollback: %d (%v)", approvals, err)
	}
	if n := m.countRows(`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND provider_tx_id = $2`, m.f.tenantID, r); n != 0 {
		t.Fatalf("a posting survived the rollback: %d", n)
	}
	m.b11Held(p, "partial failure")
	// Retry by the same approver: executes exactly once.
	out, err := m.decide(m.acting2, res, ResolutionApprove)
	k3RequireNoErr(t, err, "retry")
	if !out.Executed {
		t.Fatalf("retry did not execute: %+v", out)
	}
	// Idempotency / replay: a further decision is refused and posts nothing; a new request is refused.
	if _, err := m.decide(m.acting3, res, ResolutionApprove); !errors.Is(err, ErrResolutionNotPending) {
		t.Fatalf("decide after executed: %v", err)
	}
	if _, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID)); err == nil {
		t.Fatal("a second M4 on an executed attempt was accepted")
	}
	if n := m.countRows(`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND provider_tx_id = $2`, m.f.tenantID, r); n != 1 {
		t.Fatalf("completions keyed by R: %d", n)
	}
	m.assertInvariants()
}

// One pending resolution per attempt (the 0115 partial UNIQUE also makes M2 and
// M4 mutually exclusive); a cancelled one frees the slot.
func TestM4_OnePendingPerAttempt_AndCancel(t *testing.T) {
	m := newM4World(t)
	p, _, ev := m.paidPark(530)
	r, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID))
	k3RequireNoErr(t, err, "first")
	_, err = m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID))
	k3RequireCode(t, err, "23505")
	if _, err := m.svc.Cancel(k3Ctx(m.acting), m.target(m.acting), r.ID, ResolutionMeta{RequestID: "m4"}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID)); err != nil {
		t.Fatalf("request after cancel: %v", err)
	}
}

// Concurrency: two final approvals race; exactly one executes, one posting.
func TestM4_Concurrency_TwoFinalApprovals(t *testing.T) {
	m := newM4World(t)
	for i := 0; i < 4; i++ {
		p, r, ev := m.paidPark(int64(540 + i))
		res, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID))
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
		if n := m.countRows(`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND provider_tx_id = $2`, m.f.tenantID, r); n != 1 {
			t.Fatalf("iteration %d: completions %d", i, n)
		}
	}
	m.assertInvariants()
}

// Concurrency: the final approval races a contradicting import. Whatever the
// interleaving, the outcome is consistent: either executed (and the evidence it
// executed on was positive at execution) or refused/raised, never a release on
// a contradicted verdict that the DB re-check saw.
func TestM4_Concurrency_ApprovalVsContradictingImport(t *testing.T) {
	m := newM4World(t)
	for i := 0; i < 3; i++ {
		p, _, ev := m.paidPark(int64(550 + i))
		res, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID))
		k3RequireNoErr(t, err, "request")
		var wg sync.WaitGroup
		var out ResolutionOutcome
		var derr error
		wg.Add(2)
		go func() { defer wg.Done(); out, derr = m.decide(m.acting2, res, ResolutionApprove) }()
		go func() {
			defer wg.Done()
			m.ingest(m4Imp{unsealed: true}, m.line(m4Ref(), p.fresh.MerchantReference, "declined", p.fresh.Amount, time.Now()))
		}()
		wg.Wait()
		wr := m.wd(p.wr.ID)
		switch {
		case derr == nil && out.Executed:
			if wr.State != withdrawal.StateCompleted {
				t.Fatalf("executed but withdrawal %s", wr.State)
			}
		case derr == nil && out.Refused, derr != nil:
			if wr.State != withdrawal.StateSubmitted {
				t.Fatalf("refused but withdrawal %s", wr.State)
			}
		default:
			t.Fatalf("unexpected outcome %+v %v", out, derr)
		}
	}
	m.assertInvariants()
}

// --- the fences and MR041 (C-7) through DB-level forgeries -----------------------------------

func TestM4_Fences_And_MR041_Forgeries(t *testing.T) {
	m := newM4World(t)
	p, r, ev := m.paidPark(560)
	res, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID))
	k3RequireNoErr(t, err, "request")
	post := func(ctx context.Context, tx pgx.Tx, typ ledger.TransactionType, key string, corr uuid.UUID, ptx *string) error {
		accts, err := ledger.GetOrCreateAccounts(ctx, tx, m.f.tenantID,
			ledger.AccountSpec{WalletID: &m.f.walletID, AccountType: ledger.AccountPlayerWithdrawalHold, AssetCode: "EUR"},
			ledger.AccountSpec{AccountType: ledger.AccountPSPClearing, AssetCode: "EUR"})
		if err != nil {
			return err
		}
		pid := m.provider
		in := ledger.TransactionInput{TenantID: m.f.tenantID, TransactionType: typ, IdempotencyKey: key, CorrelationID: corr,
			Entries: []ledger.EntryInput{{LedgerAccountID: accts[0], Direction: ledger.Debit, Amount: 560},
				{LedgerAccountID: accts[1], Direction: ledger.Credit, Amount: 560}}}
		if ptx != nil {
			in.ProviderID, in.ProviderTxID = &pid, ptx
		}
		_, err = ledger.Post(ctx, tx, in)
		return err
	}
	other := "m4-other-" + uuid.NewString()[:8]
	for name, f := range map[string]func(ctx context.Context, tx pgx.Tx) error{
		"(f) correlation dropped": func(ctx context.Context, tx pgx.Tx) error {
			return post(ctx, tx, ledger.TxWithdrawalCompleted, m.provider+":"+r, uuid.New(), &r)
		},
		"(f) provider_tx_id not R": func(ctx context.Context, tx pgx.Tx) error {
			return post(ctx, tx, ledger.TxWithdrawalCompleted, m.provider+":"+r, p.wr.ID, &other)
		},
		"(f) any key": func(ctx context.Context, tx pgx.Tx) error {
			return post(ctx, tx, ledger.TxWithdrawalCompleted, m.provider+":"+other, p.wr.ID, &other)
		},
		"(g) failed under an executing PAID M4": func(ctx context.Context, tx pgx.Tx) error {
			return post(ctx, tx, ledger.TxWithdrawalFailed, p.wr.ID.String()+":failed", p.wr.ID, nil)
		},
	} {
		err := m.inExecutingActing(res, m.acting2, func(ctx context.Context, tx pgx.Tx) error { return k3Try(ctx, tx, f) })
		k3RequireCode(t, err, "CG030")
		_ = name
	}
	// MR041 at COMMIT: the right posting, but the resolution links another transaction.
	err = m.pool.WithPlatformActingInTenant(context.Background(), m.acting2.ID, m.f.tenantID, res.ID, OperationKindForceResolve, func(ctx context.Context, tx pgx.Tx) error {
		if err := prooftest.AttachForSession(ctx, tx, "payment_force_resolve:approve", res.ID.String(), res.PayloadHash); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO payment_manual_resolution_approvals
			(tenant_id, resolution_id, decision, payload_hash, decided_by, decided_by_scope, decided_by_person_id, decided_txid, reason_code)
			VALUES ($1, $2, 'approve', $3, $4, 'tenant', $4, 0, 'm4')`, m.f.tenantID, res.ID, res.PayloadHash, uuid.Nil); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_manual_resolutions SET state = 'executing' WHERE id = $1`, res.ID); err != nil {
			return err
		}
		if err := withdrawal.Complete(ctx, tx, p.wr.ID, m.provider, r); err != nil {
			return err
		}
		wr, err := withdrawal.GetByID(ctx, tx, p.wr.ID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE payment_manual_resolutions SET state = 'executed', ledger_transaction_id = $2 WHERE id = $1`, res.ID, *wr.HoldLedgerTransactionID)
		return err
	})
	k3RequireCode(t, err, "MR041")
	m.b11Held(p, "MR041 refusal at commit")
	// The tenant session cannot reach `executing` for an M4 at all: the floor
	// (no platform_acting approval) refuses first, and evaluation would be MR060.
	err = m.inExecuting(res, m.f2, func(context.Context, pgx.Tx) error { return nil })
	if c := k3Code(err); c != "MR030" && c != "MR060" {
		t.Fatalf("tenant executing: %v", err)
	}
}

// --- migration ---------------------------------------------------------------------------

const m4MigrationVersion = 125

func TestM4_Migration0125UpDownUp_WholeSchema(t *testing.T) {
	pool, _ := scratchThrough(t, "m4r19_", m4MigrationVersion-1)
	colsQ := `SELECT string_agg(table_name || '.' || column_name || ':' || data_type || ':' || is_nullable, ',' ORDER BY table_name, column_name)
		FROM information_schema.columns WHERE table_schema = 'public'
		  AND table_name IN ('payment_manual_resolutions', 'payment_statement_imports', 'payment_statement_lines')`
	cols := func() string {
		var s string
		if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error { return tx.QueryRow(ctx, colsQ).Scan(&s) }); err != nil {
			t.Fatal(err)
		}
		return s
	}
	preSnap, preCols := schemaSnapshot15(t, pool), cols()
	dir := migration0101Dir(t, m4MigrationVersion)
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("up: %v", err)
	}
	upSnap := schemaSnapshot15(t, pool)
	for _, must := range []string{"function:payout_m4_evidence(uuid,uuid)", "policy:payment_statement_lines:acting_read",
		"policy:payment_statement_imports:acting_read", "policy:payment_attempt_reference_evidence:acting_read",
		"constraint:payment_manual_resolutions:payment_manual_resolutions_m4_shape_check"} {
		if !strings.Contains(upSnap, must) {
			t.Errorf("the 0125 schema lacks %q", must)
		}
	}
	if _, err := pool.MigrateDown(context.Background(), dir, 1); err != nil {
		t.Fatalf("down on an empty 0125: %v", err)
	}
	if got := schemaSnapshot15(t, pool); got != preSnap {
		t.Fatalf("0125 down did not restore the 0124 schema exactly:\n%s", snapDiff(preSnap, got))
	}
	if got := cols(); got != preCols {
		t.Fatalf("0125 down did not restore the columns:\n%s\n%s", preCols, got)
	}
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("re-up: %v", err)
	}
	if got := schemaSnapshot15(t, pool); got != upSnap {
		t.Fatalf("0125 re-up differs from the first up:\n%s", snapDiff(upSnap, got))
	}
}

func TestM4_Migration0125DownRefusals(t *testing.T) {
	t.Run("sealed_import", func(t *testing.T) {
		m := newM4World(t)
		p := m.park(570)
		m.ingest(m4Imp{}, m.line(m4Ref(), p.fresh.MerchantReference, "succeeded", 570, time.Now()))
		_, err := m.pool.MigrateDown(context.Background(), migration0101Dir(t, m4MigrationVersion), 1)
		k3RequireCode(t, err, "MR099")
	})
	t.Run("m4_row", func(t *testing.T) {
		m := newM4World(t)
		p, _, ev := m.paidPark(571)
		_, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID))
		k3RequireNoErr(t, err, "request")
		_, err = m.pool.MigrateDown(context.Background(), migration0101Dir(t, m4MigrationVersion), 1)
		k3RequireCode(t, err, "MR099")
	})
}

// The not-paid window is payments.DefaultSettlementWindow (24 h), literally in
// the migration (a change on either side must change both).
func TestM4_SettlementWindowPinned(t *testing.T) {
	if DefaultSettlementWindow != 24*time.Hour || m4SettlementWindowLabel != "24 hours" {
		t.Fatalf("DefaultSettlementWindow %s / label %q", DefaultSettlementWindow, m4SettlementWindowLabel)
	}
	b, err := os.ReadFile(filepath.Join(realMigrationsDir(t), "0125_payout_unbound_resolution_m4.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "i.coverage_end >= v_a.last_sent_at + interval '"+m4SettlementWindowLabel+"'") {
		t.Fatal("the migration's not-paid window is not the pinned 24 hours")
	}
}

func m4Ref() string { return "m4-" + uuid.NewString()[:16] }

// m4AnyLine is a syntactically valid line id for requests the database refuses
// BEFORE comparing the line (scope, verdict).
var m4AnyLine = uuid.MustParse("00000000-0000-4000-8000-000000000001")

// Concurrency: the final approval races the sweeper (RunOnce and a direct
// processPayoutAttempt of the disputed attempt) and a stale-copy poll of the
// provider. The serialisation is L1 withdrawal -> attempt; whatever the
// interleaving, exactly one posting, the attempt stays disputed, no provider
// call, the ledger balances.
func TestM4_Concurrency_FinalApprovalVsSweeperAndPoll(t *testing.T) {
	m := newM4World(t)
	for i := 0; i < 3; i++ {
		p, r, ev := m.paidPark(int64(580 + i))
		res, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID))
		k3RequireNoErr(t, err, "request")
		calls := m.prov.calls[k3CallWithdraw].Load()
		var wg sync.WaitGroup
		var out ResolutionOutcome
		var derr error
		wg.Add(3)
		go func() { defer wg.Done(); out, derr = m.decide(m.acting2, res, ResolutionApprove) }()
		go func() {
			defer wg.Done()
			_ = m.b11Sweeper().RunOnce(context.Background(), []uuid.UUID{m.f.tenantID})
			_ = m.b11Sweeper().processPayoutAttempt(context.Background(), m.f.tenantID, p.fresh)
		}()
		go func() {
			defer wg.Done()
			_ = PollPayoutStatus(context.Background(), m.pool, m.orch, MockCredentialResolver{}, m.f.tenantID, p.staleAs(AttemptPending), time.Now().Add(time.Minute), nil)
		}()
		wg.Wait()
		if derr != nil || !out.Executed {
			t.Fatalf("iteration %d: approval %+v %v", i, out, derr)
		}
		if a := m.attempt(p.fresh.ID); a.State != AttemptDisputed {
			t.Fatalf("iteration %d: attempt %s", i, a.State)
		}
		if n := m.countRows(`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND provider_tx_id = $2`, m.f.tenantID, r); n != 1 {
			t.Fatalf("iteration %d: completions %d", i, n)
		}
		if m.prov.calls[k3CallWithdraw].Load() != calls {
			t.Fatalf("iteration %d: a provider Withdraw call was made", i)
		}
	}
	m.assertInvariants()
}

// S-2: the `pending -> executing` move re-runs payout_m4_evidence IN THE
// DATABASE (Go is the first check, not the only one): a direct move to
// executing after the evidence changed is refused (MR061) even when Go is
// bypassed, in the acting session that could otherwise post.
func TestM4_DBRecheck_AtExecuting_EvidenceChanged(t *testing.T) {
	m := newM4World(t)
	p, _, ev := m.paidPark(590)
	res, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID))
	k3RequireNoErr(t, err, "request")
	// Control: with the evidence unchanged the direct move succeeds (rolled back).
	if err := m.inExecutingActing(res, m.acting2, func(context.Context, pgx.Tx) error { return nil }); err != nil {
		t.Fatalf("control: executing with unchanged evidence: %v", err)
	}
	m.ingest(m4Imp{unsealed: true}, m.line(m4Ref(), "", "pending", 590, time.Now()), m.line(m4Ref(), p.fresh.MerchantReference, "reversed", 590, time.Now()))
	err = m.inExecutingActing(res, m.acting2, func(context.Context, pgx.Tx) error { return nil })
	k3RequireCode(t, err, "MR061")
	m.b11Held(p, "DB re-check")
}

// Fence (f) at the TRANSACTION level (ADR 0111 4.5, C-7). The entries fence is
// a second line that would also refuse a forged posting, so this probes the
// ledger_transactions fence on its own with bare INSERTs in an acting session
// that has an executing m4_evidence_paid: each forged shape is refused (CG030);
// the exact governed shape is admitted (control; every probe is rolled back).
func TestM4_Fence_TransactionLevel_EachBinding(t *testing.T) {
	m := newM4World(t)
	p, r, ev := m.paidPark(565)
	res, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID))
	k3RequireNoErr(t, err, "request")
	ins := func(typ, key string, corr uuid.UUID, ptx *string) func(ctx context.Context, tx pgx.Tx) error {
		return func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, provider_id, provider_tx_id, correlation_id)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`, uuid.New(), m.f.tenantID, typ, key, m.provider, ptx, corr)
			return err
		}
	}
	other := "m4-other-" + uuid.NewString()[:8]
	if err := m.inExecutingActing(res, m.acting2, func(ctx context.Context, tx pgx.Tx) error {
		return k3Try(ctx, tx, ins("withdrawal_completed", m.provider+":"+r, p.wr.ID, &r))
	}); err != nil {
		t.Fatalf("control: the exact governed completion must pass the transaction fence: %v", err)
	}
	for name, f := range map[string]func(ctx context.Context, tx pgx.Tx) error{
		"correlation":                       ins("withdrawal_completed", m.provider+":"+r, uuid.New(), &r),
		"provider_tx_id":                    ins("withdrawal_completed", m.provider+":"+r, p.wr.ID, &other),
		"idempotency key":                   ins("withdrawal_completed", m.provider+":"+other, p.wr.ID, &r),
		"failed under an executing M4 paid": ins("withdrawal_failed", p.wr.ID.String()+":failed", p.wr.ID, nil),
	} {
		err := m.inExecutingActing(res, m.acting2, func(ctx context.Context, tx pgx.Tx) error { return k3Try(ctx, tx, f) })
		if k3Code(err) != "CG030" {
			t.Errorf("%s: want CG030 from the transaction fence, got %v", name, err)
		}
	}
	m.b11Held(p, "transaction fence probes")
}

// MR041 at commit (C-7) closes what the fences cannot see: (a) the withdrawal's
// release link rewritten inside the executing transaction (S-8 is still open:
// the column is not frozen), and (b) a completion planted earlier by an ordinary
// (system-session) posting with the wrong key, then linked by the resolution.
// Both commit-time refusals roll the whole executing transaction back.
func TestM4_MR041_ForgedLinkAndPlantedPosting(t *testing.T) {
	m := newM4World(t)
	execForge := func(res ManualResolution, forge func(ctx context.Context, tx pgx.Tx, wr withdrawal.WithdrawalRequest) (uuid.UUID, error)) error {
		return m.pool.WithPlatformActingInTenant(context.Background(), m.acting2.ID, m.f.tenantID, res.ID, OperationKindForceResolve, func(ctx context.Context, tx pgx.Tx) error {
			if err := prooftest.AttachForSession(ctx, tx, "payment_force_resolve:approve", res.ID.String(), res.PayloadHash); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO payment_manual_resolution_approvals
				(tenant_id, resolution_id, decision, payload_hash, decided_by, decided_by_scope, decided_by_person_id, decided_txid, reason_code)
				VALUES ($1, $2, 'approve', $3, $4, 'tenant', $4, 0, 'm4')`, m.f.tenantID, res.ID, res.PayloadHash, uuid.Nil); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE payment_manual_resolutions SET state = 'executing' WHERE id = $1`, res.ID); err != nil {
				return err
			}
			wr, err := withdrawal.GetByID(ctx, tx, *res.WithdrawalRequestID)
			if err != nil {
				return err
			}
			link, err := forge(ctx, tx, wr)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE payment_manual_resolutions SET state = 'executed', ledger_transaction_id = $2 WHERE id = $1`, res.ID, link)
			return err
		})
	}
	t.Run("release link rewritten", func(t *testing.T) {
		m.t = t
		p, r, ev := m.paidPark(567)
		res, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID))
		k3RequireNoErr(t, err, "request")
		err = execForge(res, func(ctx context.Context, tx pgx.Tx, wr withdrawal.WithdrawalRequest) (uuid.UUID, error) {
			if err := withdrawal.Complete(ctx, tx, wr.ID, m.provider, r); err != nil {
				return uuid.Nil, err
			}
			after, err := withdrawal.GetByID(ctx, tx, wr.ID)
			if err != nil {
				return uuid.Nil, err
			}
			if _, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET release_ledger_transaction_id = $2 WHERE id = $1`, wr.ID, *wr.HoldLedgerTransactionID); err != nil {
				return uuid.Nil, err
			}
			return *after.ReleaseLedgerTransactionID, nil
		})
		k3RequireCode(t, err, "MR041")
		m.b11Held(p, "forged release link")
	})
	t.Run("planted wrong-key completion", func(t *testing.T) {
		m.t = t
		p, _, ev := m.paidPark(568)
		res, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID))
		k3RequireNoErr(t, err, "request")
		// An ordinary posting (system session; ADR 0110 T5) shaped like the governed
		// completion in every respect but its key.
		var planted uuid.UUID
		plantedRef := "m4-planted-" + uuid.NewString()[:8]
		m.tx(func(ctx context.Context, tx pgx.Tx) error {
			accts, err := ledger.GetOrCreateAccounts(ctx, tx, m.f.tenantID,
				ledger.AccountSpec{WalletID: &m.f.walletID, AccountType: ledger.AccountPlayerWithdrawalHold, AssetCode: "EUR"},
				ledger.AccountSpec{AccountType: ledger.AccountPSPClearing, AssetCode: "EUR"})
			if err != nil {
				return err
			}
			pid := m.provider
			out, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: m.f.tenantID, TransactionType: ledger.TxWithdrawalCompleted,
				IdempotencyKey: m.provider + ":" + plantedRef, ProviderID: &pid, ProviderTxID: &plantedRef, CorrelationID: p.wr.ID,
				Entries: []ledger.EntryInput{{LedgerAccountID: accts[0], Direction: ledger.Debit, Amount: 568},
					{LedgerAccountID: accts[1], Direction: ledger.Credit, Amount: 568}}})
			planted = out.TransactionID
			return err
		})
		err = execForge(res, func(ctx context.Context, tx pgx.Tx, wr withdrawal.WithdrawalRequest) (uuid.UUID, error) {
			_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'completed', release_ledger_transaction_id = $2, updated_at = now() WHERE id = $1`, wr.ID, planted)
			return planted, err
		})
		k3RequireCode(t, err, "MR041")
		if wr := m.wd(p.wr.ID); wr.State != withdrawal.StateSubmitted {
			t.Fatalf("withdrawal moved: %s", wr.State)
		}
	})
	t.Run("planted wrong-key failure (not paid)", func(t *testing.T) {
		m.t = t
		p, ev := m.notPaidPark(569)
		res, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidenceNotPaid, ev.LineID))
		k3RequireNoErr(t, err, "request")
		// An ordinary hold -> cash posting correlated to the withdrawal but NOT
		// under the governed wr.id:failed key.
		var planted uuid.UUID
		m.tx(func(ctx context.Context, tx pgx.Tx) error {
			accts, err := ledger.GetOrCreateAccounts(ctx, tx, m.f.tenantID,
				ledger.AccountSpec{WalletID: &m.f.walletID, AccountType: ledger.AccountPlayerWithdrawalHold, AssetCode: "EUR"},
				ledger.AccountSpec{WalletID: &m.f.walletID, AccountType: ledger.AccountPlayerCash, AssetCode: "EUR"})
			if err != nil {
				return err
			}
			out, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: m.f.tenantID, TransactionType: ledger.TxWithdrawalFailed,
				IdempotencyKey: p.wr.ID.String() + ":planted", CorrelationID: p.wr.ID,
				Entries: []ledger.EntryInput{{LedgerAccountID: accts[0], Direction: ledger.Debit, Amount: 569},
					{LedgerAccountID: accts[1], Direction: ledger.Credit, Amount: 569}}})
			planted = out.TransactionID
			return err
		})
		err = execForge(res, func(ctx context.Context, tx pgx.Tx, wr withdrawal.WithdrawalRequest) (uuid.UUID, error) {
			_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'failed', release_ledger_transaction_id = $2, updated_at = now() WHERE id = $1`, wr.ID, planted)
			return planted, err
		})
		k3RequireCode(t, err, "MR041")
		if wr := m.wd(p.wr.ID); wr.State != withdrawal.StateSubmitted {
			t.Fatalf("withdrawal moved: %s", wr.State)
		}
	})
}

// S-2 step 7: the import SET the re-evaluation reads must equal the pinned one.
// The PSP re-delivers the identical succeeded line in a later (sealed) import
// after the request: verdict, line and R are unchanged, but the import set is
// not, so the execution ends refused_at_execution (fail closed; a fresh request
// pins the new set) - Go refuses before the database would raise MR061.
func TestM4_Execution_RefusedWhenOnlyTheImportSetChanges(t *testing.T) {
	m := newM4World(t)
	p := m.park(595)
	r := "m4-R-" + uuid.NewString()[:12]
	l := m.line(r, p.fresh.MerchantReference, statement.PaymentStatusSucceeded, 595, time.Now().UTC().Truncate(time.Microsecond))
	m.ingest(m4Imp{start: time.Now().Add(-2 * time.Hour)}, l)
	ev := m.mustEvidence(p.fresh.ID, M4VerdictPaid)
	res, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID))
	k3RequireNoErr(t, err, "request")
	m.ingest(m4Imp{start: time.Now().Add(-3 * time.Hour)}, l)
	ev2 := m.mustEvidence(p.fresh.ID, M4VerdictPaid)
	if *ev2.LineID != *ev.LineID || *ev2.Reference != r || len(ev2.ImportIDs) != 2 {
		t.Fatalf("setup: want the same verdict/line/R over two imports, got %+v", ev2)
	}
	out, err := m.decide(m.acting2, res, ResolutionApprove)
	k3RequireNoErr(t, err, "approve")
	if !out.Refused || out.Resolution.RefusalCode == nil || *out.Resolution.RefusalCode != resolutionRefusedEvidence {
		t.Fatalf("want refused_at_execution/%s, got %+v", resolutionRefusedEvidence, out)
	}
	m.b11Held(p, "import set changed")
}
