//go:build integration

// PAY-RECEIPT-ANOMALY-APPLIED-1 Option A (owner decisions 24-30, ADR 0095 section 45): the one-time
// locked re-attribution plus missing audit-row repair of an anomaly-closed receipt. Runtime-role
// integration tests: happy path (deposit and payout), idempotency, every refusal (and its durable
// signal), no money/state change, concurrency, and the abuse surface (no general reassignment).
package payments

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

type rrCase struct {
	pool      *db.Pool
	tenantID  uuid.UUID
	provider  string
	attempt   PaymentAttempt
	eventType string
	ref       string
}

// rrPayoutSucceeded: a payout settled by a matching success callback (500 EUR, ref).
func rrPayoutSucceeded(t *testing.T, pool *db.Pool, pid, key, ref string) (rrCase, rbEnv) {
	t.Helper()
	f, orch, _ := fpOrch(t, pool, pid)
	e := rbEnv{pool: pool, f: f, orch: orch, pid: pid}
	a, _ := e.r6Succeeded(t, key, ref)
	return rrCase{pool: pool, tenantID: f.tenantID, provider: pid, attempt: mustGetAttempt(t, pool, f.tenantID, a.ID), eventType: "payout", ref: ref}, e
}

// rrDepositSucceeded: a deposit settled by a matching success receipt that binds ref (T4).
func rrDepositSucceeded(t *testing.T, ref string) (rrCase, t4DrainEnv) {
	t.Helper()
	e := t4DrainSetup(t, "mock-rr-dep-"+uuid.NewString()[:8], "rr-dep-"+uuid.NewString()[:8])
	if _, err := e.apply(ReceiptEvidence{EventType: "deposit", ProviderReference: ref, MerchantReference: e.attempt.MerchantReference,
		Outcome: OutcomeSucceeded, Amount: MockAmountAmbiguous, AssetCode: "EUR"}); err != nil {
		t.Fatalf("setup deposit success: %v", err)
	}
	a := mustGetAttempt(t, e.pool, e.f.tenantID, e.attempt.ID)
	if a.State != AttemptSucceeded {
		t.Fatalf("setup: deposit state = %s, want succeeded", a.State)
	}
	return rrCase{pool: e.pool, tenantID: e.f.tenantID, provider: e.provider, attempt: a, eventType: "deposit", ref: ref}, e
}

func (c rrCase) ev(amountDelta int64) ReceiptEvidence {
	return ReceiptEvidence{EventType: c.eventType, ProviderReference: c.ref, MerchantReference: c.attempt.MerchantReference,
		Outcome: OutcomeSucceeded, Amount: c.attempt.Amount + amountDelta, AssetCode: c.attempt.AssetCode}
}

// plant stores an anomaly receipt through the production insert and closes it through the production
// close, exactly as the lock-free anomaly branches do.
func (c rrCase) plant(t *testing.T, provider string, ev ReceiptEvidence, reason ReceiptResolution) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := c.pool.WithTenant(context.Background(), c.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var dup bool
		var err error
		id, dup, err = insertReceiptDeduped(ctx, tx, c.tenantID, provider, ev, DispositionAnomaly)
		if err != nil || dup {
			t.Fatalf("plant: dup=%v err=%v", dup, err)
		}
		return closeAnomalyReceipt(ctx, tx, c.tenantID, provider, id, false, reason)
	}); err != nil {
		t.Fatalf("plant receipt: %v", err)
	}
	return id
}

func (c rrCase) repair(t *testing.T, receiptID uuid.UUID) ReceiptRepairResult {
	t.Helper()
	res, err := c.repairErr(receiptID)
	if err != nil {
		t.Fatalf("RepairReceiptAttribution: %v", err)
	}
	return res
}

func (c rrCase) repairErr(receiptID uuid.UUID) (ReceiptRepairResult, error) {
	return RepairReceiptAttribution(context.Background(), c.pool, ReceiptRepairRequest{
		TenantID: c.tenantID, ReceiptID: receiptID, Reason: RepairReasonAnomalyAppliedGap, Caller: "test.repair"})
}

func (c rrCase) q(t *testing.T, sql string, args ...any) string {
	t.Helper()
	var s string
	if err := c.pool.WithTenant(context.Background(), c.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&s)
	}); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return s
}

func (c rrCase) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	return int(rvQueryIntTenant(t, c.pool, c.tenantID, sql, args...))
}

func rvQueryIntTenant(t *testing.T, pool *db.Pool, tenantID uuid.UUID, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	}); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

// rrSnap is everything the repair must NOT change: the attempt row, its parent row, the ledger.
type rrSnap struct {
	attempt, parent             string
	ledgerTx, entries, entrySum int64
}

func (c rrCase) snap(t *testing.T) rrSnap {
	t.Helper()
	s := rrSnap{attempt: c.q(t, `SELECT row_to_json(a)::text FROM payment_attempts a WHERE id = $1`, c.attempt.ID)}
	if c.attempt.DepositIntentID != nil {
		s.parent = c.q(t, `SELECT row_to_json(p)::text FROM deposit_intents p WHERE id = $1`, *c.attempt.DepositIntentID)
	} else {
		s.parent = c.q(t, `SELECT row_to_json(p)::text FROM withdrawal_requests p WHERE id = $1`, *c.attempt.WithdrawalRequestID)
	}
	s.ledgerTx = int64(c.count(t, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, c.tenantID))
	s.entries = int64(c.count(t, `SELECT count(*) FROM ledger_entries WHERE tenant_id = $1`, c.tenantID))
	s.entrySum = int64(c.count(t, `SELECT COALESCE(sum(amount), 0) FROM ledger_entries WHERE tenant_id = $1`, c.tenantID))
	return s
}

func (c rrCase) assertNoFinancialChange(t *testing.T, before rrSnap) {
	t.Helper()
	if after := c.snap(t); after != before {
		t.Fatalf("the repair changed attempt/parent/ledger state:\nbefore %+v\nafter  %+v", before, after)
	}
	assertLedgerBalanced(t, c.pool, c.tenantID)
}

// receiptNoAttr is the whole receipt row except attempt_id.
func (c rrCase) receiptNoAttr(t *testing.T, id uuid.UUID) string {
	return c.q(t, `SELECT (row_to_json(r)::jsonb - 'attempt_id')::text FROM payment_provider_events r WHERE id = $1`, id)
}

func (c rrCase) receiptAttempt(t *testing.T, id uuid.UUID) *uuid.UUID {
	t.Helper()
	var a *uuid.UUID
	if err := c.pool.WithTenant(context.Background(), c.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT attempt_id FROM payment_provider_events WHERE id = $1`, id).Scan(&a)
	}); err != nil {
		t.Fatal(err)
	}
	return a
}

func (c rrCase) auditN(t *testing.T, action string) int {
	return c.count(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2`, c.tenantID, action)
}

func (c rrCase) repairAlerts(t *testing.T) []alertinject.Row {
	t.Helper()
	var out []alertinject.Row
	for _, r := range alertinject.ForSubject(t, c.pool, c.tenantID) {
		if strings.HasPrefix(r.Discriminator, "receipt:") {
			out = append(out, r)
		}
	}
	return out
}

func (c rrCase) meta(t *testing.T, action, targetID string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := c.pool.WithTenant(context.Background(), c.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT metadata FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3`, c.tenantID, action, targetID).Scan(&m)
	}); err != nil {
		t.Fatalf("read audit metadata %s: %v", action, err)
	}
	return m
}

func (c rrCase) assertRefused(t *testing.T, res ReceiptRepairResult, receiptID uuid.UUID, reason string, signalled bool, before rrSnap, receiptBefore string) {
	t.Helper()
	if res.Outcome != RepairRefused || res.RefusalReason != reason || res.Signalled != signalled {
		t.Fatalf("result = %+v, want refused/%s/signalled=%v", res, reason, signalled)
	}
	if c.receiptAttempt(t, receiptID) != nil {
		t.Fatal("a refused repair must leave attempt_id NULL")
	}
	if got := c.receiptNoAttr(t, receiptID); got != receiptBefore {
		t.Fatalf("a refused repair changed the receipt:\n%s\n%s", receiptBefore, got)
	}
	if n := c.auditN(t, auditActionReceiptAttributionRepaired); n != 0 {
		t.Fatalf("a refusal must write no repaired row, got %d", n)
	}
	c.assertNoFinancialChange(t, before)
	rows := c.repairAlerts(t)
	if signalled {
		want := "receipt:" + receiptID.String() + ":reason:" + reason
		if len(rows) != 1 || rows[0].Discriminator != want || rows[0].Kind != string(alerting.KindPaymentWebhookIntegrity) ||
			rows[0].Severity != "p1" || rows[0].State != "open" {
			t.Fatalf("want ONE open P1 %s, got %+v", want, rows)
		}
		if len(rows[0].Attributes) != 1 || rows[0].Attributes["provider_id"] != c.provider {
			t.Fatalf("alert attributes = %v, want only provider_id", rows[0].Attributes)
		}
	} else if len(rows) != 0 {
		t.Fatalf("an ineligible receipt must raise nothing, got %+v", rows)
	}
}

// --- happy path ---------------------------------------------------------------------------------

func TestRR_HappyPath_OrphanAnomalyReceipt_AttributedOnce_AuditedOnce_NoFinancialChange(t *testing.T) {
	pool := depositV2ScratchPool(t)
	mk := map[string]func(t *testing.T) rrCase{
		"payout": func(t *testing.T) rrCase {
			c, _ := rrPayoutSucceeded(t, pool, "mock-rr-pay-hp", "rr-hp-pay", "rr-hp-pay-ref")
			return c
		},
		"deposit": func(t *testing.T) rrCase {
			c, _ := rrDepositSucceeded(t, "rr-hp-dep-ref")
			return c
		},
	}
	for name, build := range mk {
		t.Run(name, func(t *testing.T) {
			c := build(t)
			// The anomaly close the race leaves: a mismatched success receipt, anomaly_cross_provider, NULL attempt.
			rid := c.plant(t, c.provider, c.ev(7), ResolutionAnomalyCrossProvider)
			resolvedAt := c.q(t, `SELECT resolved_at::text FROM payment_provider_events WHERE id = $1`, rid)
			beforeReceipt := c.receiptNoAttr(t, rid)
			before := c.snap(t)
			mismatchBefore := c.auditN(t, "payments.callback_amount_asset_mismatch_terminal")

			res := c.repair(t, rid)
			if res.Outcome != RepairRepaired || res.AttemptID != c.attempt.ID || res.ReceiptID != rid {
				t.Fatalf("result = %+v, want repaired onto %s", res, c.attempt.ID)
			}
			if a := c.receiptAttempt(t, rid); a == nil || *a != c.attempt.ID {
				t.Fatalf("receipt attempt_id = %v, want %s", a, c.attempt.ID)
			}
			// The original anomaly history and the evidence are intact (only attempt_id moved).
			if got := c.receiptNoAttr(t, rid); got != beforeReceipt {
				t.Fatalf("receipt changed beyond attempt_id:\n%s\n%s", beforeReceipt, got)
			}
			if got := c.q(t, `SELECT resolution FROM payment_provider_events WHERE id = $1`, rid); got != string(ResolutionAnomalyCrossProvider) {
				t.Fatalf("resolution = %s: the anomaly label must be preserved", got)
			}
			if got := c.q(t, `SELECT resolved_at::text FROM payment_provider_events WHERE id = $1`, rid); got != resolvedAt {
				t.Fatal("resolved_at changed")
			}
			// Audit rows written exactly once: the replayed terminal-cell row and the repair row.
			if n := c.auditN(t, "payments.callback_amount_asset_mismatch_terminal") - mismatchBefore; n != 1 {
				t.Fatalf("replayed mismatch audit rows = %d, want 1", n)
			}
			if n := c.auditN(t, auditActionReceiptAttributionRepaired); n != 1 {
				t.Fatalf("repair audit rows = %d, want 1", n)
			}
			m := c.meta(t, auditActionReceiptAttributionRepaired, rid.String())
			for k, want := range map[string]any{
				"reason": RepairReasonAnomalyAppliedGap, "caller": "test.repair", "attempt_id": c.attempt.ID.String(),
				"resolution_preserved": true, "reconstructed": true, "financial_effect": "none",
				"derivation_basis": "provider_reference+merchant_reference", "provider_id": c.provider,
			} {
				if m[k] != want {
					t.Fatalf("repair audit %s = %v, want %v (%v)", k, m[k], want, m)
				}
			}
			bf, _ := m["before"].(map[string]any)
			af, _ := m["after"].(map[string]any)
			if bf["attempt_id"] != nil || bf["resolution"] != string(ResolutionAnomalyCrossProvider) || af["attempt_id"] != c.attempt.ID.String() {
				t.Fatalf("before/after = %v / %v", bf, af)
			}
			if u, _ := m["unknowable"].([]any); len(u) == 0 || u[0] != "attempt_state_at_event" {
				t.Fatalf("unknowable = %v, must name the attempt state at the event", m["unknowable"])
			}
			var actorType string
			_ = c.pool.WithTenant(context.Background(), c.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT actor_type FROM audit_log WHERE action = $1 AND target_id = $2`, auditActionReceiptAttributionRepaired, rid.String()).Scan(&actorType)
			})
			if actorType != "system" {
				t.Fatalf("actor_type = %s, want system", actorType)
			}
			rm := c.replayMeta(t, rid)
			if rm["reconstructed"] != true || rm["receipt_id"] != rid.String() || rm["attempt_state"] != "succeeded_inferred" {
				t.Fatalf("replayed audit metadata = %v", rm)
			}
			if len(c.repairAlerts(t)) != 0 {
				t.Fatal("a successful repair raises nothing")
			}
			c.assertNoFinancialChange(t, before)
			if c.count(t, `SELECT count(*) FROM payment_provider_events WHERE tenant_id = $1`, c.tenantID) < 1 {
				t.Fatal("receipt rows must never be deleted")
			}
		})
	}
}

func (c rrCase) replayMeta(t *testing.T, rid uuid.UUID) map[string]any {
	t.Helper()
	var m map[string]any
	if err := c.pool.WithTenant(context.Background(), c.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT metadata FROM audit_log WHERE tenant_id = $1 AND action = 'payments.callback_amount_asset_mismatch_terminal'
			AND metadata->>'receipt_id' = $2`, c.tenantID, rid.String()).Scan(&m)
	}); err != nil {
		t.Fatalf("read replayed audit: %v", err)
	}
	return m
}

// A matched (non-mismatched) success writes no terminal-cell audit row: nothing to replay.
func TestRR_MatchedSuccess_NoReplayRow_OnlyRepairRow(t *testing.T) {
	pool := depositV2ScratchPool(t)
	c, _ := rrPayoutSucceeded(t, pool, "mock-rr-matched", "rr-matched", "rr-matched-ref")
	ev := c.ev(0)
	ev.SettlementReference = "rr-matched-settlement" // a distinct fingerprint from the settled receipt
	rid := c.plant(t, c.provider, ev, ResolutionAnomalyCrossProvider)
	base := c.auditN(t, "payments.callback_amount_asset_mismatch_terminal")
	before := c.snap(t)
	if res := c.repair(t, rid); res.Outcome != RepairRepaired || len(res.ReplayedAudit) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if got := c.auditN(t, "payments.callback_amount_asset_mismatch_terminal"); got != base {
		t.Fatalf("a matched success must not replay a mismatch row (%d -> %d)", base, got)
	}
	if c.auditN(t, auditActionReceiptAttributionRepaired) != 1 {
		t.Fatal("want one repair row")
	}
	c.assertNoFinancialChange(t, before)
}

// The attempt state at the event is unknowable for a disputed attempt: no replay, the gap is recorded.
func TestRR_DisputedAttempt_NoReplay_UnknowableRecorded(t *testing.T) {
	e := t4DrainSetup(t, "mock-rr-disputed", "rr-disputed")
	ref := "rr-disputed-ref"
	// A genuine mismatched success on the ref-less ambiguous deposit parks it (disputed) and binds the reference.
	if _, err := e.apply(ReceiptEvidence{EventType: "deposit", ProviderReference: ref, MerchantReference: e.attempt.MerchantReference,
		Outcome: OutcomeSucceeded, Amount: MockAmountAmbiguous + 3, AssetCode: "EUR"}); err != nil {
		t.Fatal(err)
	}
	a := mustGetAttempt(t, e.pool, e.f.tenantID, e.attempt.ID)
	if a.State != AttemptDisputed || a.ProviderReference == nil || *a.ProviderReference != ref {
		t.Fatalf("setup: state=%s ref=%v, want disputed with the bound reference", a.State, a.ProviderReference)
	}
	c := rrCase{pool: e.pool, tenantID: e.f.tenantID, provider: e.provider, attempt: a, eventType: "deposit", ref: ref}
	rid := c.plant(t, c.provider, c.ev(9), ResolutionAnomalyReferenceConflict)
	base := c.auditN(t, "payments.callback_amount_asset_mismatch_terminal")
	before := c.snap(t)
	res := c.repair(t, rid)
	if res.Outcome != RepairRepaired || len(res.ReplayedAudit) != 0 {
		t.Fatalf("result = %+v", res)
	}
	found := false
	for _, u := range res.Unknowable {
		found = found || strings.HasPrefix(u, "terminal_mismatch_audit_not_replayed")
	}
	if !found {
		t.Fatalf("unknowable = %v, must record the missing replay", res.Unknowable)
	}
	if got := c.auditN(t, "payments.callback_amount_asset_mismatch_terminal"); got != base {
		t.Fatal("nothing may be replayed for a disputed attempt")
	}
	c.assertNoFinancialChange(t, before)
}

// --- idempotency ----------------------------------------------------------------------------------

func TestRR_SecondCall_NoOp_SameResult(t *testing.T) {
	pool := depositV2ScratchPool(t)
	c, _ := rrPayoutSucceeded(t, pool, "mock-rr-idem", "rr-idem", "rr-idem-ref")
	rid := c.plant(t, c.provider, c.ev(5), ResolutionAnomalyCrossProvider)
	first := c.repair(t, rid)
	if first.Outcome != RepairRepaired {
		t.Fatalf("first = %+v", first)
	}
	snap := c.snap(t)
	audits := c.count(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1`, c.tenantID)
	for i := 0; i < 3; i++ {
		again := c.repair(t, rid)
		if again.Outcome != RepairAlreadyRepaired || again.AttemptID != first.AttemptID || again.ReceiptID != rid {
			t.Fatalf("call %d = %+v, want already_repaired onto %s", i+2, again, first.AttemptID)
		}
	}
	if got := c.count(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1`, c.tenantID); got != audits {
		t.Fatalf("a repeated call wrote audit rows (%d -> %d)", audits, got)
	}
	c.assertNoFinancialChange(t, snap)
}

// --- refusals -------------------------------------------------------------------------------------

func TestRR_Ambiguous_TwoCandidates_Refused_Signalled_Repeatable(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f, orch, _ := fpOrch(t, pool, "mock-rr-amb")
	e := rbEnv{pool: pool, f: f, orch: orch, pid: "mock-rr-amb"}
	_, x := e.claim(t, "rr-amb-x")
	y, _ := e.r6Succeeded(t, "rr-amb-y", "rr-amb-y-ref")
	// The reference-binding race shape: the provider reference names Y, the merchant reference names X.
	c := rrCase{pool: pool, tenantID: f.tenantID, provider: "mock-rr-amb", attempt: mustGetAttempt(t, pool, f.tenantID, x.ID), eventType: "payout", ref: "rr-amb-y-ref"}
	_ = y
	rid := c.plant(t, c.provider, c.ev(0), ResolutionAnomalyReferenceConflict)
	before, rb := c.snap(t), c.receiptNoAttr(t, rid)
	res := c.repair(t, rid)
	c.assertRefused(t, res, rid, RefusalAmbiguousCandidates, true, before, rb)
	if n := c.auditN(t, auditActionReceiptAttributionRepairRefused); n != 1 {
		t.Fatalf("refusal audit rows = %d, want 1", n)
	}
	// A repeat is another refusal (audited) onto the SAME open alert whose occurrences grow.
	res = c.repair(t, rid)
	rows := c.repairAlerts(t)
	if res.Outcome != RepairRefused || len(rows) != 1 || rows[0].Occurrences != 2 {
		t.Fatalf("repeat: %+v alerts=%+v, want one alert with 2 occurrences", res, rows)
	}
}

func TestRR_NoCandidate_Refused_Signalled(t *testing.T) {
	pool := depositV2ScratchPool(t)
	c, _ := rrPayoutSucceeded(t, pool, "mock-rr-nocand", "rr-nocand", "rr-nocand-ref")
	ev := ReceiptEvidence{EventType: "payout", ProviderReference: "rr-unknown-ref", MerchantReference: "rr-unknown-merchant",
		Outcome: OutcomeSucceeded, Amount: 500, AssetCode: "EUR"}
	rid := c.plant(t, c.provider, ev, ResolutionAnomalyOther)
	before, rb := c.snap(t), c.receiptNoAttr(t, rid)
	c.assertRefused(t, c.repair(t, rid), rid, RefusalNoCandidate, true, before, rb)
}

func TestRR_ContradictoryEvidence_Refused_Signalled(t *testing.T) {
	pool := depositV2ScratchPool(t)
	c, _ := rrPayoutSucceeded(t, pool, "mock-rr-contra", "rr-contra", "rr-contra-ref")
	d, _ := rrDepositSucceeded(t, "rr-contra-dep-ref")
	cases := map[string]struct {
		c        rrCase
		provider string
		ev       ReceiptEvidence
	}{
		"merchant_reference_names_no_attempt": {c, c.provider, ReceiptEvidence{EventType: "payout", ProviderReference: c.ref,
			MerchantReference: "rr-no-such-merchant", Outcome: OutcomeSucceeded, Amount: 500, AssetCode: "EUR"}},
		"other_provider_never_used": {c, c.provider + "-x", c.ev(1)},
		"event_type_vs_operation": {d, d.provider, ReceiptEvidence{EventType: "payout", ProviderReference: d.ref,
			MerchantReference: d.attempt.MerchantReference, Outcome: OutcomeSucceeded, Amount: d.attempt.Amount, AssetCode: "EUR"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rid := tc.c.plant(t, tc.provider, tc.ev, ResolutionAnomalyOther)
			before, rb := tc.c.snap(t), tc.c.receiptNoAttr(t, rid)
			res := tc.c.repair(t, rid)
			if res.Outcome == RepairRefused && res.RefusalReason == RefusalContradictoryEvidence {
				// Other receipts of this test pool also raise: assert on this receipt's alert only.
				want := "receipt:" + rid.String() + ":reason:" + RefusalContradictoryEvidence
				found := false
				for _, r := range tc.c.repairAlerts(t) {
					found = found || r.Discriminator == want
				}
				if !found {
					t.Fatalf("no alert %s in %+v", want, tc.c.repairAlerts(t))
				}
				if tc.c.receiptAttempt(t, rid) != nil || tc.c.receiptNoAttr(t, rid) != rb {
					t.Fatal("receipt changed")
				}
				tc.c.assertNoFinancialChange(t, before)
				return
			}
			t.Fatalf("result = %+v, want refused/%s", res, RefusalContradictoryEvidence)
		})
	}
}

func TestRR_InsufficientEvidence_Refused_Signalled(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f, orch, _ := fpOrch(t, pool, "mock-rr-insuf")
	e := rbEnv{pool: pool, f: f, orch: orch, pid: "mock-rr-insuf"}
	// (a) the merchant reference names an attempt that never bound the receipt's provider reference.
	_, x := e.claim(t, "rr-insuf-x")
	c := rrCase{pool: pool, tenantID: f.tenantID, provider: "mock-rr-insuf", attempt: mustGetAttempt(t, pool, f.tenantID, x.ID), eventType: "payout", ref: "rr-insuf-ref"}
	rid := c.plant(t, c.provider, c.ev(0), ResolutionAnomalyCrossProvider)
	before, rb := c.snap(t), c.receiptNoAttr(t, rid)
	c.assertRefused(t, c.repair(t, rid), rid, RefusalInsufficientEvidence, true, before, rb)

	// (b) a SUCCESS receipt on an attempt still pending cannot have been applied to it.
	_, p := e.claim(t, "rr-insuf-p")
	e.b12Callback(t, p, OutcomePending, "rr-insuf-p-ref", 500)
	pa := mustGetAttempt(t, pool, f.tenantID, p.ID)
	if pa.State != AttemptPending {
		t.Fatalf("setup: state = %s, want pending", pa.State)
	}
	c2 := rrCase{pool: pool, tenantID: f.tenantID, provider: "mock-rr-insuf", attempt: pa, eventType: "payout", ref: "rr-insuf-p-ref"}
	rid2 := c2.plant(t, c2.provider, c2.ev(0), ResolutionAnomalyCrossProvider)
	if res := c2.repair(t, rid2); res.Outcome != RepairRefused || res.RefusalReason != RefusalInsufficientEvidence || !res.Signalled {
		t.Fatalf("pending attempt + success receipt: %+v", res)
	}
	if c2.receiptAttempt(t, rid2) != nil {
		t.Fatal("must stay unattributed")
	}
}

func TestRR_NotEligible_AppliedUnresolvedAndNonAnomaly_Refused_NoSignal(t *testing.T) {
	pool := depositV2ScratchPool(t)
	c, _ := rrPayoutSucceeded(t, pool, "mock-rr-inel", "rr-inel", "rr-inel-ref")
	// (a) the genuinely applied receipt of the settlement.
	var applied uuid.UUID
	if err := pool.WithTenant(context.Background(), c.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM payment_provider_events WHERE tenant_id = $1 AND provider_reference = $2 AND resolution = 'applied'`, c.tenantID, c.ref).Scan(&applied)
	}); err != nil {
		t.Fatal(err)
	}
	before, rb := c.snap(t), c.receiptNoAttr(t, applied)
	res := c.repair(t, applied)
	if res.Outcome != RepairRefused || res.RefusalReason != RefusalNotEligible || res.Signalled {
		t.Fatalf("applied receipt: %+v", res)
	}
	if c.receiptNoAttr(t, applied) != rb || c.receiptAttempt(t, applied) == nil || *c.receiptAttempt(t, applied) != c.attempt.ID {
		t.Fatal("an applied receipt must be untouched")
	}
	if c.auditN(t, auditActionReceiptAttributionRepaired) != 0 {
		t.Fatal("no repair row for an applied receipt")
	}
	c.assertNoFinancialChange(t, before)
	// (b) an unresolved (deferred) receipt is the drain's domain.
	var deferred uuid.UUID
	if err := pool.WithTenant(context.Background(), c.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var dup bool
		var err error
		deferred, dup, err = insertReceiptDeduped(ctx, tx, c.tenantID, c.provider, c.ev(11), DispositionDeferredUnresolved)
		if dup {
			t.Fatal("dup")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if res := c.repair(t, deferred); res.Outcome != RepairRefused || res.RefusalReason != RefusalNotEligible || res.Signalled || c.receiptAttempt(t, deferred) != nil {
		t.Fatalf("unresolved receipt: %+v", res)
	}
	// (c) resolved applied with a NULL attempt is not an anomaly closure.
	var odd uuid.UUID
	if err := pool.WithTenant(context.Background(), c.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var dup bool
		var err error
		odd, dup, err = insertReceiptDeduped(ctx, tx, c.tenantID, c.provider, c.ev(12), DispositionAnomaly)
		if err != nil || dup {
			t.Fatalf("dup=%v err=%v", dup, err)
		}
		return ResolveReceipt(ctx, tx, odd, nil, string(ResolutionApplied))
	}); err != nil {
		t.Fatal(err)
	}
	if res := c.repair(t, odd); res.Outcome != RepairRefused || res.RefusalReason != RefusalNotEligible || res.Signalled || c.receiptAttempt(t, odd) != nil {
		t.Fatalf("applied+NULL receipt: %+v", res)
	}
	if len(c.repairAlerts(t)) != 0 {
		t.Fatal("ineligible receipts raise nothing")
	}
}

// --- tenancy ---------------------------------------------------------------------------------------

func TestRR_CrossTenant(t *testing.T) {
	pool := depositV2ScratchPool(t)
	a, _ := rrPayoutSucceeded(t, pool, "mock-rr-ten", "rr-ten-a", "rr-ten-shared-ref")
	b, _ := rrPayoutSucceeded(t, pool, "mock-rr-ten", "rr-ten-b", "rr-ten-b-ref")
	// (1) tenant B cannot repair tenant A's receipt: indistinguishable from a missing one.
	rid := a.plant(t, a.provider, a.ev(4), ResolutionAnomalyCrossProvider)
	if _, err := RepairReceiptAttribution(context.Background(), pool, ReceiptRepairRequest{TenantID: b.tenantID, ReceiptID: rid,
		Reason: RepairReasonAnomalyAppliedGap, Caller: "test.repair"}); err != ErrReceiptRepairNotFound {
		t.Fatalf("cross-tenant repair err = %v, want ErrReceiptRepairNotFound", err)
	}
	if a.receiptAttempt(t, rid) != nil {
		t.Fatal("must stay unattributed")
	}
	// (2) a receipt in tenant A naming tenant B's merchant and provider reference never uses B's attempt.
	ev := ReceiptEvidence{EventType: "payout", ProviderReference: b.ref, MerchantReference: b.attempt.MerchantReference,
		Outcome: OutcomeSucceeded, Amount: 500, AssetCode: "EUR"}
	rid2 := a.plant(t, a.provider, ev, ResolutionAnomalyCrossProvider)
	res := a.repair(t, rid2)
	if res.Outcome != RepairRefused || res.RefusalReason != RefusalNoCandidate {
		t.Fatalf("cross-tenant evidence: %+v, want no_candidate", res)
	}
	if got := b.q(t, `SELECT count(*) FROM payment_provider_events WHERE attempt_id = $1 AND resolution LIKE 'anomaly_%'`, b.attempt.ID); got != "0" {
		t.Fatalf("tenant B's attempt was attributed %s anomaly receipt(s)", got)
	}
}

// --- request shape ----------------------------------------------------------------------------------

func TestRR_Request_NoTargetParameter_AndValidation(t *testing.T) {
	rt := reflect.TypeOf(ReceiptRepairRequest{})
	var fields []string
	for i := 0; i < rt.NumField(); i++ {
		fields = append(fields, rt.Field(i).Name)
	}
	if !reflect.DeepEqual(fields, []string{"TenantID", "ReceiptID", "Reason", "Caller"}) {
		t.Fatalf("ReceiptRepairRequest fields = %v: there must be no caller-chosen target", fields)
	}
	ft := reflect.TypeOf(RepairReceiptAttribution)
	if ft.NumIn() != 3 || ft.In(2) != rt {
		t.Fatalf("RepairReceiptAttribution must take only (ctx, pool, request), has %d params", ft.NumIn())
	}
	pool := depositV2ScratchPool(t)
	good := ReceiptRepairRequest{TenantID: uuid.New(), ReceiptID: uuid.New(), Reason: RepairReasonAnomalyAppliedGap, Caller: "ok.caller"}
	for name, mut := range map[string]func(r *ReceiptRepairRequest){
		"nil_tenant":   func(r *ReceiptRepairRequest) { r.TenantID = uuid.Nil },
		"nil_receipt":  func(r *ReceiptRepairRequest) { r.ReceiptID = uuid.Nil },
		"bad_reason":   func(r *ReceiptRepairRequest) { r.Reason = "because" },
		"empty_caller": func(r *ReceiptRepairRequest) { r.Caller = "" },
		"caller_upper": func(r *ReceiptRepairRequest) { r.Caller = "Ops" },
		"caller_long":  func(r *ReceiptRepairRequest) { r.Caller = strings.Repeat("a", 65) },
	} {
		r := good
		mut(&r)
		if _, err := RepairReceiptAttribution(context.Background(), pool, r); err != ErrReceiptRepairInvalidRequest {
			t.Errorf("%s: err = %v, want ErrReceiptRepairInvalidRequest", name, err)
		}
	}
	if _, err := RepairReceiptAttribution(context.Background(), pool, good); err != ErrReceiptRepairNotFound {
		t.Errorf("unknown receipt: err = %v, want ErrReceiptRepairNotFound", err)
	}
}

// --- concurrency ------------------------------------------------------------------------------------

func TestRR_Concurrent_TwoRepairsOfOneReceipt_OneEffect(t *testing.T) {
	pool := depositV2ScratchPool(t)
	c, _ := rrPayoutSucceeded(t, pool, "mock-rr-conc", "rr-conc", "rr-conc-ref")
	before := c.snap(t)
	for rep := 0; rep < 8; rep++ {
		rid := c.plant(t, c.provider, c.ev(int64(100+rep)), ResolutionAnomalyCrossProvider)
		var wg sync.WaitGroup
		start := make(chan struct{})
		res := make([]ReceiptRepairResult, 2)
		errs := make([]error, 2)
		for i := range res {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				res[i], errs[i] = c.repairErr(rid)
			}(i)
		}
		close(start)
		wg.Wait()
		repaired, already := 0, 0
		for i := range res {
			if errs[i] != nil {
				t.Fatalf("rep %d goroutine %d: %v", rep, i, errs[i])
			}
			switch res[i].Outcome {
			case RepairRepaired:
				repaired++
			case RepairAlreadyRepaired:
				already++
			}
		}
		if repaired != 1 || already != 1 {
			t.Fatalf("rep %d: repaired=%d already=%d (%+v), want exactly one effect", rep, repaired, already, res)
		}
		if n := c.count(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3`, c.tenantID, auditActionReceiptAttributionRepaired, rid.String()); n != 1 {
			t.Fatalf("rep %d: repair audit rows for the receipt = %d, want 1", rep, n)
		}
		if n := c.count(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND metadata->>'receipt_id' = $2`, c.tenantID, rid.String()); n != 1 {
			t.Fatalf("rep %d: replayed rows for the receipt = %d, want 1", rep, n)
		}
	}
	c.assertNoFinancialChange(t, before)
}

// A repair racing the callback path applying on the same attempt (parent lock shared): neither fails, the
// attempt's own audit/alert rows are not duplicated, and no conflict leaks.
func TestRR_Concurrent_RepairRacesCallbackOnSameAttempt(t *testing.T) {
	pool := depositV2ScratchPool(t)
	c, e := rrPayoutSucceeded(t, pool, "mock-rr-race", "rr-race", "rr-race-ref")
	before := c.snap(t)
	for rep := 0; rep < 6; rep++ {
		rid := c.plant(t, c.provider, c.ev(int64(200+rep)), ResolutionAnomalyCrossProvider)
		var wg sync.WaitGroup
		start := make(chan struct{})
		var repErr, cbErr error
		var res ReceiptRepairResult
		wg.Add(2)
		go func() { defer wg.Done(); <-start; res, repErr = c.repairErr(rid) }()
		go func() {
			defer wg.Done()
			<-start
			// A redelivery of the genuine settlement success (duplicate effect) on the same attempt.
			cbErr = e.r5Callback(c.attempt, c.ref, c.attempt.Amount, "EUR")
		}()
		close(start)
		wg.Wait()
		if repErr != nil || cbErr != nil {
			t.Fatalf("rep %d: repair err=%v callback err=%v (no conflict may leak)", rep, repErr, cbErr)
		}
		if res.Outcome != RepairRepaired {
			t.Fatalf("rep %d: %+v", rep, res)
		}
	}
	if n := c.auditN(t, auditActionReceiptAttributionRepaired); n != 6 {
		t.Fatalf("repair rows = %d, want 6", n)
	}
	if n := c.count(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'payments.callback_amount_asset_mismatch_terminal'`, c.tenantID); n != 6 {
		t.Fatalf("mismatch rows = %d, want exactly the 6 replays (the matched redeliveries write none)", n)
	}
	c.assertNoFinancialChange(t, before)
}

// --- abuse: no general-purpose reassignment ------------------------------------------------------------

func TestRR_Abuse_DirectUpdatesRefusedByTheGuard(t *testing.T) {
	pool := depositV2ScratchPool(t)
	c, e := rrPayoutSucceeded(t, pool, "mock-rr-abuse", "rr-abuse", "rr-abuse-ref")
	_, other := e.claim(t, "rr-abuse-other") // a second attempt of the same tenant and provider, not named by the receipt
	rid := c.plant(t, c.provider, c.ev(6), ResolutionAnomalyCrossProvider)
	var appliedID uuid.UUID
	if err := pool.WithTenant(context.Background(), c.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM payment_provider_events WHERE tenant_id = $1 AND resolution = 'applied'`, c.tenantID).Scan(&appliedID)
	}); err != nil {
		t.Fatal(err)
	}
	// An applied-resolution receipt with a NULL attempt, and an applied one with an attempt.
	odd := c.plant(t, c.provider, c.ev(13), ResolutionAnomalyOther)
	oddApplied := func() uuid.UUID {
		var id uuid.UUID
		if err := pool.WithTenant(context.Background(), c.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var dup bool
			var err error
			id, dup, err = insertReceiptDeduped(ctx, tx, c.tenantID, c.provider, c.ev(14), DispositionAnomaly)
			if err != nil || dup {
				t.Fatalf("dup=%v err=%v", dup, err)
			}
			return ResolveReceipt(ctx, tx, id, nil, string(ResolutionApplied))
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}()
	exec := func(sql string, args ...any) error {
		return pool.WithTenant(context.Background(), c.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, args...)
			return err
		})
	}
	refused := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: the guard must refuse", name)
		}
	}
	// A caller-chosen attempt that does not match the receipt's evidence.
	refused("NULL -> unrelated attempt", exec(`UPDATE payment_provider_events SET attempt_id = $2 WHERE id = $1`, rid, other.ID))
	refused("NULL -> unrelated attempt (odd)", exec(`UPDATE payment_provider_events SET attempt_id = $2 WHERE id = $1`, odd, other.ID))
	// An applied-resolution receipt is never attributed after resolution, even to the matching attempt.
	refused("applied+NULL -> matching attempt", exec(`UPDATE payment_provider_events SET attempt_id = $2 WHERE id = $1`, oddApplied, c.attempt.ID))
	// Already attributed: one-shot.
	refused("applied receipt re-attributed", exec(`UPDATE payment_provider_events SET attempt_id = $2 WHERE id = $1`, appliedID, other.ID))
	refused("applied receipt attempt cleared", exec(`UPDATE payment_provider_events SET attempt_id = NULL WHERE id = $1`, appliedID))
	// Every other column stays immutable.
	refused("resolution relabelled", exec(`UPDATE payment_provider_events SET resolution = 'applied' WHERE id = $1`, rid))
	refused("resolved_at changed", exec(`UPDATE payment_provider_events SET resolved_at = now() + interval '1 day' WHERE id = $1`, rid))
	refused("amount changed", exec(`UPDATE payment_provider_events SET amount = amount + 1 WHERE id = $1`, rid))
	refused("provider_reference changed", exec(`UPDATE payment_provider_events SET provider_reference = 'x' WHERE id = $1`, rid))
	refused("merchant_reference changed", exec(`UPDATE payment_provider_events SET merchant_reference = $2 WHERE id = $1`, rid, other.MerchantReference))
	refused("provider_id changed", exec(`UPDATE payment_provider_events SET provider_id = 'other' WHERE id = $1`, rid))
	refused("tenant_id changed", exec(`UPDATE payment_provider_events SET tenant_id = $2 WHERE id = $1`, rid, uuid.New()))
	refused("disposition changed", exec(`UPDATE payment_provider_events SET disposition_at_receipt = 'applied' WHERE id = $1`, rid))
	refused("delete", exec(`DELETE FROM payment_provider_events WHERE id = $1`, rid))
	// The legitimate repair then succeeds, and afterwards the attribution is one-shot.
	if res := c.repair(t, rid); res.Outcome != RepairRepaired {
		t.Fatalf("repair after the refused abuse: %+v", res)
	}
	refused("repaired receipt re-attributed", exec(`UPDATE payment_provider_events SET attempt_id = $2 WHERE id = $1`, rid, other.ID))
	refused("repaired receipt attempt cleared", exec(`UPDATE payment_provider_events SET attempt_id = NULL WHERE id = $1`, rid))
	if a := c.receiptAttempt(t, rid); a == nil || *a != c.attempt.ID {
		t.Fatalf("receipt attempt_id = %v after the abuse attempts", a)
	}
}

// The DB-level target check: a direct UPDATE that names the evidence-matching attempt is the ONLY
// attribution the guard admits (and nothing else), so the target can never be caller-chosen even by SQL.
func TestRR_Guard_AdmitsOnlyEvidenceMatchingTarget_SameTenantProviderOperation(t *testing.T) {
	pool := depositV2ScratchPool(t)
	c, e := rrPayoutSucceeded(t, pool, "mock-rr-guard", "rr-guard", "rr-guard-ref")
	d, _ := rrDepositSucceeded(t, "rr-guard-dep-ref")
	_ = d
	exec := func(rid, attempt uuid.UUID) error {
		return pool.WithTenant(context.Background(), c.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE payment_provider_events SET attempt_id = $2 WHERE id = $1`, rid, attempt)
			return err
		})
	}
	// matching target but a receipt whose event_type disagrees with the attempt's operation
	wrongOp := c.plant(t, c.provider, ReceiptEvidence{EventType: "deposit", ProviderReference: c.ref, MerchantReference: c.attempt.MerchantReference,
		Outcome: OutcomeSucceeded, Amount: 500, AssetCode: "EUR"}, ResolutionAnomalyOther)
	if err := exec(wrongOp, c.attempt.ID); err == nil {
		t.Error("a deposit receipt must not be attributed to a payout attempt")
	}
	// matching target but a receipt of ANOTHER provider id
	wrongProv := c.plant(t, c.provider+"-z", c.ev(21), ResolutionAnomalyCrossProvider)
	if err := exec(wrongProv, c.attempt.ID); err == nil {
		t.Error("a receipt of another provider must not be attributed")
	}
	// matching merchant reference but a provider reference the attempt never bound
	unboundRef := c.plant(t, c.provider, ReceiptEvidence{EventType: "payout", ProviderReference: "rr-guard-unbound-ref", MerchantReference: c.attempt.MerchantReference,
		Outcome: OutcomeSucceeded, Amount: 500, AssetCode: "EUR"}, ResolutionAnomalyCrossProvider)
	if err := exec(unboundRef, c.attempt.ID); err == nil {
		t.Error("a receipt whose provider reference the attempt never bound must not be attributed")
	}
	// matching provider reference but a merchant reference naming a different attempt
	_, third := e.claim(t, "rr-guard-third")
	otherMerchant := c.plant(t, c.provider, ReceiptEvidence{EventType: "payout", ProviderReference: c.ref, MerchantReference: third.MerchantReference,
		Outcome: OutcomeSucceeded, Amount: 501, AssetCode: "EUR"}, ResolutionAnomalyReferenceConflict)
	if err := exec(otherMerchant, c.attempt.ID); err == nil {
		t.Error("a receipt whose merchant reference names a different attempt must not be attributed")
	}
	// a nonexistent attempt id (FK) and an attempt of another tenant (invisible under RLS)
	good := c.plant(t, c.provider, c.ev(22), ResolutionAnomalyCrossProvider)
	if err := exec(good, uuid.New()); err == nil {
		t.Error("an unknown attempt id must be refused")
	}
	if err := exec(good, d.attempt.ID); err == nil {
		t.Error("another tenant's attempt must be refused")
	}
	if err := exec(good, c.attempt.ID); err != nil {
		t.Errorf("the evidence-matching attempt is admitted by the guard: %v", err)
	}
}

// The closed refusal-reason set (a new reason must be reviewed against the static wiring pins and ADR 0095 s45).
func TestRR_SignalReasons_ClosedSet(t *testing.T) {
	want := map[string]bool{
		"receipt_repair_no_candidate": true, "receipt_repair_ambiguous_candidates": true,
		"receipt_repair_contradictory_evidence": true, "receipt_repair_insufficient_evidence": true,
	}
	if len(receiptRepairSignalReasons) != len(want) {
		t.Fatalf("receiptRepairSignalReasons = %v", receiptRepairSignalReasons)
	}
	for r := range receiptRepairSignalReasons {
		if !want[r] {
			t.Errorf("unexpected signal reason %q", r)
		}
	}
	if _, ok := receiptRepairSignalReasons[RefusalNotEligible]; ok {
		t.Error("an ineligible receipt must not raise")
	}
}

// The three row locks of the documented order (parent, attempt, receipt) are really taken: with each held by
// another transaction the repair blocks (its context expires) and writes nothing; once released it succeeds.
func TestRR_Locks_ParentAttemptAndReceiptAreTaken(t *testing.T) {
	pool := depositV2ScratchPool(t)
	c, _ := rrPayoutSucceeded(t, pool, "mock-rr-locks", "rr-locks", "rr-locks-ref")
	targets := []struct {
		name, sql string
		arg       func(rid uuid.UUID) any
	}{
		{"parent_withdrawal_request", `SELECT id FROM withdrawal_requests WHERE id = $1 FOR UPDATE`, func(uuid.UUID) any { return *c.attempt.WithdrawalRequestID }},
		{"attempt", `SELECT id FROM payment_attempts WHERE id = $1 FOR UPDATE`, func(uuid.UUID) any { return c.attempt.ID }},
		{"receipt", `SELECT id FROM payment_provider_events WHERE id = $1 FOR UPDATE`, func(rid uuid.UUID) any { return rid }},
	}
	for i, tg := range targets {
		rid := c.plant(t, c.provider, c.ev(int64(300+i)), ResolutionAnomalyCrossProvider)
		var blockedErr error
		if err := pool.WithTenant(context.Background(), c.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, tg.sql, tg.arg(rid)); err != nil {
				return err
			}
			tctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			_, blockedErr = RepairReceiptAttribution(tctx, pool, ReceiptRepairRequest{TenantID: c.tenantID, ReceiptID: rid,
				Reason: RepairReasonAnomalyAppliedGap, Caller: "test.repair"})
			return nil
		}); err != nil {
			t.Fatalf("%s: %v", tg.name, err)
		}
		if blockedErr == nil {
			t.Errorf("%s: the repair completed while the %s lock was held elsewhere: that lock is not taken", tg.name, tg.name)
		}
		if c.receiptAttempt(t, rid) != nil || c.count(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND target_id = $2`, c.tenantID, rid.String()) != 0 {
			t.Errorf("%s: a blocked repair must write nothing", tg.name)
		}
		if res := c.repair(t, rid); res.Outcome != RepairRepaired {
			t.Errorf("%s: after release: %+v", tg.name, res)
		}
	}
}
