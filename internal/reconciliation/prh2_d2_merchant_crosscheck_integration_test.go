//go:build integration

// PRH-2 D2-1: merchant-reference cross-check on lines resolved by provider
// or settlement reference (ledger-finance ruling
// docs/plans/prh2-hardening-round/reviews/d2-1-ledger-finance-ruling.md (a)
// and (c) 1-8; ADR 0095 §35.2). The (c) 9 mutants are recorded in
// docs/plans/payment-readiness/evidence/prh2-d2-mutation-kill.txt.
//
// Before this check, ONE statement line (reference R, merchant reference
// naming the conflict-parked attempt B) resolved by reference to R's holder
// A and the run was silent (code review D2-1 probe: 0 mismatches) - a
// misattributed capture the balanced ledger cannot show.
package reconciliation

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/providerref"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// parkDepositConflict: holder A binds R through the real path (and, when
// succeedHolder, succeeds and posts through a verified callback); B's
// adapter then returns R, and phase C parks B provider_reference_conflict.
func (w *d2World) parkDepositConflict(t *testing.T, succeedHolder bool) (holder, parked payments.PaymentAttempt, ref string) {
	t.Helper()
	holder = w.deposit(t, d2Amount)
	if holder.ProviderReference == nil {
		t.Fatalf("setup: holder has no reference (state %s)", holder.State)
	}
	ref = *holder.ProviderReference
	if succeedHolder {
		w.succeed(t, w.mockA, payProvA, holder)
	}
	w.p.setScript(d2Pending(ref))
	defer w.p.setScript(nil)
	parked = w.mustParked(t, w.deposit(t, d2Amount).ID, payments.TerminalReasonProviderReferenceConflict, false)
	if parked.ProviderReference != nil {
		t.Fatalf("setup: the conflict park must not bind the reference")
	}
	return w.attempt(t, holder.ID), parked, ref
}

func d2Merchant(ms []Mismatch) []Mismatch {
	var out []Mismatch
	for _, m := range ms {
		if m.MismatchKind == MismatchKindPayReferenceMismatch && strings.Contains(m.ReconciliationKey, "check=merchant") {
			out = append(out, m)
		}
	}
	return out
}

// d2OneMerchant asserts exactly one check=merchant mismatch, attributed to
// attempt a, whose detail contains every fragment.
func d2OneMerchant(t *testing.T, ms []Mismatch, a uuid.UUID, detail ...string) {
	t.Helper()
	got := d2Merchant(ms)
	if len(got) != 1 {
		t.Fatalf("want exactly one pay_reference_mismatch check=merchant, got %d:\n%s", len(got), renderMismatches(ms))
	}
	if !strings.Contains(got[0].ReconciliationKey, "attempt="+a.String()) {
		t.Fatalf("check=merchant must be attributed to attempt=%s, key=%s", a, got[0].ReconciliationKey)
	}
	for _, d := range detail {
		if !strings.Contains(got[0].ActualValue, d) {
			t.Fatalf("check=merchant detail lacks %q: %s", d, got[0].ActualValue)
		}
	}
}

// d2AssertBalanced is (c) 8: SUM(D)=SUM(C) and RunLedgerVsProjection reports 0.
func (w *d2World) d2AssertBalanced(t *testing.T) {
	t.Helper()
	var debits, credits int64
	var drift []Mismatch
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'), 0)::bigint,
			       COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0)::bigint
			  FROM ledger_entries WHERE tenant_id = $1`, w.f.tenantID).Scan(&debits, &credits); err != nil {
			return err
		}
		var err error
		_, drift, err = RunLedgerVsProjection(ctx, tx, w.f.tenantID, time.Now().Add(-time.Hour), time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if debits != credits || len(drift) != 0 {
		t.Fatalf("ledger invariants: debits=%d credits=%d drift mismatches=%d", debits, credits, len(drift))
	}
}

// TestD2_7_MerchantCrossCheck_DepositConflict is the ruling's (c) 1-4, 7, 8.
func TestD2_7_MerchantCrossCheck_DepositConflict(t *testing.T) {
	// (c) 1: A succeeded on R; B parked conflict on R; ONE line (R,
	// merchant B, succeeded).
	t.Run("c1_core_single_line_names_parked_attempt", func(t *testing.T) {
		w := newD2World(t)
		a, b, r := w.parkDepositConflict(t, true)
		ms := w.d2Run(t, d2Src(d2Line(r, b.MerchantReference, statement.PaymentStatusSucceeded, d2Amount)))
		d2Expect(t, ms, map[MismatchKind]int{MismatchKindPayReferenceMismatch: 1, d2KindCU: 1})
		d2OneMerchant(t, ms, a.ID, "names attempt="+b.ID.String())
		cu := d2CUFor(t, ms, b.ID)
		if !strings.Contains(cu.ActualValue, "resolved by reference to attempt="+a.ID.String()) {
			t.Errorf("B's finding must name the attempt the line resolved to: %s", cu.ActualValue)
		}
		w.d2AssertNoMoney(t, d2Parked{attempt: b, pspRef: "d2-none"})
		w.d2AssertBalanced(t)
	})
	// (c) 2: B's captured_unposted clears on a reversal line naming R in
	// the run, or a tombstone on R; the check=merchant finding stays.
	t.Run("c2_reversal_line_on_R_clears_B_keeps_merchant_check", func(t *testing.T) {
		w := newD2World(t)
		a, b, r := w.parkDepositConflict(t, true)
		ms := w.d2Run(t, d2Src(d2Line(r, b.MerchantReference, statement.PaymentStatusSucceeded, d2Amount),
			d2ReversalLine("d2-rev-"+uuid.NewString()[:8], r, d2Amount)))
		d2OneMerchant(t, ms, a.ID, "names attempt="+b.ID.String())
		d2NoCU(t, ms, "a reversal line naming the line's reference")
		w.d2AssertBalanced(t)
	})
	t.Run("c2_tombstone_on_R_clears_B_keeps_merchant_check", func(t *testing.T) {
		w := newD2World(t)
		a, b, r := w.parkDepositConflict(t, false) // holder pending: R never posted, so the reversal tombstones it
		w.deliverReversal(t, "d2-tomb-"+uuid.NewString()[:8], r, d2Amount)
		var tombs int64
		if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'tombstone' AND provider_id = $2 AND provider_tx_id = $3`,
				w.f.tenantID, payProvA, r).Scan(&tombs)
		}); err != nil || tombs != 1 {
			t.Fatalf("setup: want one tombstone on R, got %d err=%v", tombs, err)
		}
		ms := w.d2Run(t, d2Src(d2Line(r, b.MerchantReference, statement.PaymentStatusSucceeded, d2Amount)))
		d2OneMerchant(t, ms, a.ID, "names attempt="+b.ID.String())
		d2NoCU(t, ms, "a tombstone on the line's reference")
		w.d2AssertNoMoney(t, d2Parked{attempt: b, pspRef: "d2-none"})
		w.d2AssertBalanced(t)
	})
	// (c) 3: holder pending - loud on A (status) and names B.
	t.Run("c3_holder_pending", func(t *testing.T) {
		w := newD2World(t)
		a, b, r := w.parkDepositConflict(t, false)
		ms := w.d2Run(t, d2Src(d2Line(r, b.MerchantReference, statement.PaymentStatusSucceeded, d2Amount)))
		d2Expect(t, ms, map[MismatchKind]int{MismatchKindPayStatusMismatch: 1, MismatchKindPayReferenceMismatch: 1, d2KindCU: 1})
		mustKeyed(t, ms, MismatchKindPayStatusMismatch, "attempt="+a.ID.String())
		d2OneMerchant(t, ms, a.ID, "names attempt="+b.ID.String())
		d2CUFor(t, ms, b.ID)
		w.d2AssertNoMoney(t, d2Parked{attempt: b, pspRef: "d2-none"})
		w.d2AssertBalanced(t)
	})
	// (c) 4: a merchant reference naming A, or an empty one: no finding.
	// (The other half of (c) 4 - the full existing suite, whose MOCK
	// statement lines carry each attempt's own merchant reference, passing
	// unchanged - is the package run itself.)
	t.Run("c4_merchant_names_A_or_empty_is_clean", func(t *testing.T) {
		w := newD2World(t)
		a, _, r := w.parkDepositConflict(t, true)
		for _, merchant := range []string{a.MerchantReference, ""} {
			d2Expect(t, w.d2Run(t, d2Src(d2Line(r, merchant, statement.PaymentStatusSucceeded, d2Amount))), map[MismatchKind]int{})
		}
		w.d2AssertBalanced(t)
	})
	// (c) 7: B is not consumed by the first line: a second line (R2,
	// merchant B) matches B through the normal merchant path, with no
	// false pay_duplicate.
	// R-1 (code review): the B step is for UNBOUND parks only. B disputed with
	// a bound reason (it holds its own reference RB) or an excluded reason,
	// and a line (R, merchant B, succeeded) resolved by reference to A: only
	// the check=merchant mismatch against A, and NO pay_captured_unposted for
	// B from this line. (A bound B still gets its own STANDING finding, keyed
	// by RB - that is the bound rule, not this line.) Kills Y-B-ANYDISPUTED.
	t.Run("r1_B_bound_or_excluded_reason_gets_no_finding_from_the_line", func(t *testing.T) {
		for _, c := range []struct {
			reason       string
			wantStanding int
		}{
			{payments.TerminalReasonPollAmountMismatch, 1},
			{payments.TerminalReasonSyncAmountMismatch, 1},
			{payments.TerminalReasonTombstonePrecedesSuccess, 0},
		} {
			t.Run(c.reason, func(t *testing.T) {
				w := newD2World(t)
				a := w.deposit(t, d2Amount)
				w.succeed(t, w.mockA, payProvA, a)
				a = w.attempt(t, a.ID)
				r := *a.ProviderReference
				b := w.parkPoll(t, c.reason) // a real pending deposit B (own reference RB), T10 by fixture
				ms := w.d2Run(t, d2Src(d2Line(r, b.attempt.MerchantReference, statement.PaymentStatusSucceeded, d2Amount)))
				d2OneMerchant(t, ms, a.ID, "names attempt="+b.attempt.ID.String())
				for _, m := range ms {
					if m.MismatchKind == d2KindCU && strings.Contains(m.ReconciliationKey, "provider_reference="+r+" ") {
						t.Fatalf("R-1: B (%s) must get no pay_captured_unposted from the line naming it:\n%s", c.reason, renderMismatches(ms))
					}
				}
				want := map[MismatchKind]int{MismatchKindPayReferenceMismatch: 1}
				if c.wantStanding > 0 {
					want[d2KindCU] = c.wantStanding
					cu := d2CUFor(t, ms, b.attempt.ID)
					if !strings.Contains(cu.ReconciliationKey, "provider_reference="+b.pspRef) || !strings.Contains(cu.ActualValue, "no statement line") {
						t.Fatalf("R-1: B's only finding must be its standing one keyed by its own reference: %s", cu.ReconciliationKey)
					}
				}
				d2Expect(t, ms, want)
				w.d2AssertBalanced(t)
			})
		}
	})
	t.Run("c7_B_not_consumed", func(t *testing.T) {
		w := newD2World(t)
		a, b, r := w.parkDepositConflict(t, true)
		// Lines are matched in provider_reference order: R2 must sort AFTER
		// R (the MOCK's "mock-psp-recon-a-..."), so the R line's cross-check
		// runs first and a consumed B would surface on the R2 line.
		r2 := "zz-d2-r2-" + uuid.NewString()
		if r2 <= r {
			t.Fatalf("setup: R2 %q must sort after R %q", r2, r)
		}
		ms := w.d2Run(t, d2Src(
			d2Line(r, b.MerchantReference, statement.PaymentStatusSucceeded, d2Amount),
			d2Line(r2, b.MerchantReference, statement.PaymentStatusSucceeded, d2Amount)))
		d2Expect(t, ms, map[MismatchKind]int{MismatchKindPayReferenceMismatch: 1, d2KindCU: 2})
		d2OneMerchant(t, ms, a.ID, "names attempt="+b.ID.String())
		var viaR2 bool
		for _, m := range ms {
			if m.MismatchKind != d2KindCU {
				continue
			}
			if !strings.Contains(m.ReconciliationKey, "attempt="+b.ID.String()) {
				t.Fatalf("captured_unposted must name B: %s", m.ReconciliationKey)
			}
			viaR2 = viaR2 || strings.Contains(m.ReconciliationKey, "provider_reference="+r2)
		}
		if !viaR2 {
			t.Fatalf("the second line must match B through the merchant path:\n%s", renderMismatches(ms))
		}
		w.d2AssertBalanced(t)
	})
}

// TestD2_8_MerchantCrossCheck_UnknownAndOtherOperation is the ruling's (c) 5.
func TestD2_8_MerchantCrossCheck_UnknownAndOtherOperation(t *testing.T) {
	t.Run("names_no_platform_attempt", func(t *testing.T) {
		w := newD2World(t)
		a := w.deposit(t, d2Amount)
		w.succeed(t, w.mockA, payProvA, a)
		ms := w.d2Run(t, d2Src(d2Line(*a.ProviderReference, "d2-never-issued", statement.PaymentStatusSucceeded, d2Amount)))
		d2Expect(t, ms, map[MismatchKind]int{MismatchKindPayReferenceMismatch: 1})
		d2OneMerchant(t, ms, a.ID, "names no platform attempt")
		w.d2AssertBalanced(t)
	})
	t.Run("names_an_attempt_of_the_other_operation", func(t *testing.T) {
		w := newD2World(t)
		a := w.deposit(t, d2Amount)
		w.succeed(t, w.mockA, payProvA, a)
		payout := w.attempt(t, w.payoutFixture(t, payProvA, "d2-instr-"+uuid.NewString()[:8], "", 3000, false))
		ms := w.d2Run(t, d2Src(d2Line(*a.ProviderReference, payout.MerchantReference, statement.PaymentStatusSucceeded, d2Amount)))
		d2Expect(t, ms, map[MismatchKind]int{MismatchKindPayReferenceMismatch: 1})
		d2OneMerchant(t, ms, a.ID, "names attempt="+payout.ID.String(), "other operation (payout)")
		w.d2AssertBalanced(t)
	})
}

// TestD2_9_MerchantCrossCheck_PayoutBySettlement is the ruling's (c) 6.
func TestD2_9_MerchantCrossCheck_PayoutBySettlement(t *testing.T) {
	w := newD2World(t)
	s1, s2 := "d2-settle-1-"+uuid.NewString()[:8], "d2-settle-2-"+uuid.NewString()[:8]
	p1 := w.attempt(t, w.payoutFixture(t, payProvA, "d2-instr-1-"+uuid.NewString()[:8], s1, 3000, true))
	p2 := w.attempt(t, w.payoutFixture(t, payProvA, "d2-instr-2-"+uuid.NewString()[:8], s2, 2000, true))
	payoutLine := func(ref, merchant string, amount int64) statement.PaymentStatementLine {
		return payLineFor(payProvA, ref, merchant, statement.PaymentLinePayout, statement.PaymentStatusSucceeded, amount)
	}
	// Control: each settlement line names its own attempt - clean.
	d2Expect(t, w.d2Run(t, d2Src(payoutLine(s1, p1.MerchantReference, 3000), payoutLine(s2, p2.MerchantReference, 2000))), map[MismatchKind]int{})
	// The line found by settlement reference s1 names P2.
	ms := w.d2Run(t, d2Src(payoutLine(s1, p2.MerchantReference, 3000), payoutLine(s2, p2.MerchantReference, 2000)))
	d2Expect(t, ms, map[MismatchKind]int{MismatchKindPayReferenceMismatch: 1})
	d2OneMerchant(t, ms, p1.ID, "names attempt="+p2.ID.String())
	w.d2AssertBalanced(t)
}

// TestD2_9b_MerchantCrossCheck_SettlementLine_NamedUnboundPark_ClearsOnLineRef:
// a payout line found by SETTLEMENT reference S1 (so the line's reference
// differs from the resolved attempt's own provider reference) whose merchant
// reference names an unbound-park attempt B. B's captured_unposted is raised,
// and clears on a reversal naming the LINE's reference S1 - never on the
// resolved attempt's reference. This is the only shape where the two differ
// (for a deposit resolved by reference they are equal by construction), so it
// is what pins the ruling's mutant "clear B on a.providerRef instead of l.ref".
// B is a fixture: a pending payout moved to T10 with an unbound reason.
//
// FLIPPED under PAY-PAYOUT-UNBOUND-STANDING-1 (ADR 0095 §35.2 PO-1 / §35.6): B
// is a PAYOUT park, so a deposit_reversal naming S1 no longer clears it (the
// D2 pin "clears on a reversal naming the line's reference" was the
// fail-open-across-operations defect). B's finding uses the payout wording,
// stands on later runs, and is not cleared by P1's own withdrawal_completed on
// S1 either (S1 is P1's settlement reference: not attributable to B). The D2
// mutant "clear B on a.providerRef instead of l.ref" has no distinguishing
// shape for payouts any more (both references belong to P1, neither is
// attributable to B); it stays pinned for the clearing it can still change by
// TestB11_Recon_UnboundPayoutPark_WithdrawalCompletedOnLineRefClears and the
// STANDING-1 mutation evidence.
func TestD2_9b_MerchantCrossCheck_SettlementLine_NamedUnboundPark_ClearsOnLineRef(t *testing.T) {
	w := newD2World(t)
	s1, i1 := "d2-settle-"+uuid.NewString()[:8], "d2-instr-1-"+uuid.NewString()[:8]
	p1 := w.attempt(t, w.payoutFixture(t, payProvA, i1, s1, 3000, true))
	bID := w.payoutFixture(t, payProvA, "d2-instr-b-"+uuid.NewString()[:8], "", 3000, false)
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return payments.ApplyDisputeFromNonTerminal(ctx, tx, bID, payments.EvidenceSync, payments.TerminalReasonInvalidProviderReference+":"+string(providerref.ReasonControlChar))
	}); err != nil {
		t.Fatalf("setup: T10: %v", err)
	}
	b := w.attempt(t, bID)
	line := payLineFor(payProvA, s1, b.MerchantReference, statement.PaymentLinePayout, statement.PaymentStatusSucceeded, 3000)

	ms := w.d2Run(t, d2Src(line))
	d2Expect(t, ms, map[MismatchKind]int{MismatchKindPayReferenceMismatch: 1, d2KindCU: 1})
	d2OneMerchant(t, ms, p1.ID, "names attempt="+b.ID.String())
	s1PayoutCUFor(t, ms, b.ID, s1)

	ms = w.d2Run(t, d2Src(line, d2ReversalLine("d2-rev-"+uuid.NewString()[:8], s1, 3000)))
	d2OneMerchant(t, ms, p1.ID, "names attempt="+b.ID.String())
	// STANDING-1: a deposit reversal never clears a payout park - not even in
	// this run: the finding is still the IN-RUN merchant cross-check one, not a
	// standing re-raise of a finding the in-run step wrongly cleared.
	if m := s1PayoutCUFor(t, ms, b.ID, s1); strings.Contains(m.ActualValue, "standing: persisted line") ||
		!strings.Contains(m.ActualValue, "line resolved by reference to attempt="+p1.ID.String()) {
		t.Fatalf("want the in-run cross-check finding for B, got %s", m.ActualValue)
	}
	s1PayoutCUFor(t, w.d2Run(t, d2PastSrc()), b.ID, s1)
	w.d2AssertBalanced(t)
}
