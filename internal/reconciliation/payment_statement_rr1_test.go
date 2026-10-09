package reconciliation

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Ledger-finance ruling RR-1 (review of 2026-10-09; ADR 0111 §19): after an
// executed m4_evidence_not_paid, STANDING-1 (pay_captured_unposted) and R-1
// (pay_declared_not_paid_but_paid) stop raising ONLY when (a) the M2 (d)
// recovery is complete, (b) the late succeeded payout is exactly ONE payout of
// the attempt's amount and asset, and (c) tenant/provider scoping holds. Pure
// matcher-level cases (no database): every rule and its negation. The
// integration tests (internal/payments m4_resolve1_rr1_integration_test.go)
// run the same rule through the real stream and the real K2 path.

type rr1Fix struct {
	m *payMatcher
	a *payAttempt
	r *m4Resolution
}

func rr1Line(ref, merchant string, amount int64, asset string, at time.Time) *persistedLine {
	return &persistedLine{importID: uuid.New(), lineNo: 1, kind: paymentStatementKindPayout, ref: ref, merchant: merchant,
		status: paymentStatementStatusSucceeded, amount: big.NewInt(amount), asset: asset, occurredAt: at}
}

// rr1New builds an unbound payout park (invalid reference, no provider
// reference) whose withdrawal an executed m4_evidence_not_paid failed, with
// `recovered` already recovered under the resolution's causation.
func rr1New(recovered int64) *rr1Fix {
	a := &payAttempt{id: uuid.New(), operation: paymentStatementKindPayout, state: "disputed", amount: big.NewInt(100), asset: "EUR",
		merchantRef: "M", terminalReason: "invalid_provider_reference:control_char"}
	e := &k3Evidence{byRef: map[string][]*persistedLine{}, byMerchant: map[string][]*persistedLine{}, byMatchedRef: map[string][]*persistedLine{},
		yRef: map[uuid.UUID]string{}, m4NotPaid: map[uuid.UUID]*m4Resolution{}, capturedSeen: map[string]bool{}}
	e.m4 = []m4Resolution{{id: uuid.New(), attemptID: a.id, kind: m4KindNotPaid, ledgerTx: uuid.New(), amount: big.NewInt(100), asset: "EUR",
		attemptMerchnt: "M", evidenceRef: "D", walletID: uuid.New(), recovered: big.NewInt(recovered)}}
	e.m4NotPaid[a.id] = &e.m4[0]
	a.m4NotPaid = true
	m := &payMatcher{tenantID: uuid.New(), provider: "p", r: &sbRecorder{}, attempts: []*payAttempt{a}, k3: e,
		byRef: map[string]*payAttempt{}, bySettlement: map[string]*payAttempt{}, byMerchant: map[string]*payAttempt{a.merchantRef: a},
		ledgerByRef: map[string]*payLedgerTx{}}
	return &rr1Fix{m: m, a: a, r: &e.m4[0]}
}

// add files l under its reference (and merchant), as loadK3Evidence does.
func (f *rr1Fix) add(l *persistedLine) *rr1Fix {
	f.m.k3.byRef[l.ref] = append(f.m.k3.byRef[l.ref], l)
	if l.merchant != "" {
		f.m.k3.byMerchant[l.merchant] = append(f.m.k3.byMerchant[l.merchant], l)
	}
	return f
}

// addMatched files l as a line read only through a matched reference.
func (f *rr1Fix) addMatched(l *persistedLine) *rr1Fix {
	f.m.k3.byMatchedRef[l.ref] = append(f.m.k3.byMatchedRef[l.ref], l)
	f.r.matchedRefs = append(f.r.matchedRefs, l.ref)
	return f
}

// run evaluates STANDING-1 and R-1 and reports what each raised.
func (f *rr1Fix) run(t *testing.T) (standing, r1 int) {
	t.Helper()
	f.m.checkStandingUnbound()
	if err := f.m.checkM4Standing(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for _, x := range f.m.r.mismatches {
		switch x.MismatchKind {
		case MismatchKindPayCapturedUnposted:
			standing++
		case MismatchKindPayDeclaredNotPaidButPaid:
			r1++
		}
	}
	return standing, r1
}

func TestRR1_Pure_StopRuleAndEveryNegation(t *testing.T) {
	now := time.Now().UTC()
	line := func() *persistedLine { return rr1Line("L", "M", 100, "EUR", now) }
	cases := []struct {
		name    string
		f       *rr1Fix
		stopped bool
	}{
		{"full recovery, one equal line: stops", rr1New(100).add(line()), true},
		{"over-recovery cannot occur (INV-ADJ-6) but >= stops", rr1New(150).add(line()), true},
		{"no recovery: raises", rr1New(0).add(line()), false},
		{"partial recovery 99 of 100: raises", rr1New(99).add(line()), false},
		{"unequal line amount: raises", rr1New(100).add(rr1Line("L", "M", 99, "EUR", now)), false},
		{"other-asset line: raises", rr1New(100).add(rr1Line("L", "M", 100, "USD", now)), false},
		{"a second NEW succeeded line (other reference): raises", rr1New(100).add(line()).add(rr1Line("L2", "M", 100, "EUR", now)), false},
		{"a second NEW succeeded line (same reference, other time): raises", rr1New(100).add(line()).add(rr1Line("L", "M", 100, "EUR", now.Add(time.Second))), false},
		{"a re-delivery without the merchant reference on a matched reference: still one payout, stops",
			rr1New(100).add(line()).addMatched(rr1Line("L", "", 100, "EUR", now)), true},
		{"a NEW succeeded line on a matched reference (no merchant): raises",
			rr1New(100).add(line()).addMatched(rr1Line("D2", "", 100, "EUR", now)), false},
		{"a NEW succeeded line on the evidence line's own reference D: raises",
			rr1New(100).add(line()).add(rr1Line("D", "", 100, "EUR", now)), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			standing, r1 := c.f.run(t)
			if c.stopped && (standing != 0 || r1 != 0) {
				t.Fatalf("want no raise, got standing=%d r1=%d: %v", standing, r1, c.f.m.r.mismatches)
			}
			if !c.stopped && (standing == 0 || r1 != 1) {
				t.Fatalf("want STANDING-1 and R-1 raised, got standing=%d r1=%d", standing, r1)
			}
		})
	}
}

// (c) scoping and shape: a resolution whose amount or asset is not the
// attempt's, a resolution of another attempt, a deposit attempt, and a bound
// site (no evidencing line) never stop through the line-keyed rule. (The BOUND
// destination_mismatch rule of owner decision 1 is m4NotPaidRecoveredBound:
// TestRR30_Pure_BoundDestinationMismatch_StopRuleAndEveryNegation.)
func TestRR1_Pure_ScopingAndBoundSitesNeverStop(t *testing.T) {
	now := time.Now().UTC()
	mk := func() *rr1Fix { return rr1New(100).add(rr1Line("L", "M", 100, "EUR", now)) }
	ev := &evidencingLine{amount: big.NewInt(100), asset: "EUR"}

	f := mk()
	if !f.m.m4NotPaidRecoveredLine(f.a, "L", ev) {
		t.Fatal("control: the rule must hold")
	}
	if f.m.m4NotPaidRecoveredLine(f.a, "L", nil) || f.m.clearedRefFor(f.a, "L", nil) {
		t.Fatal("a bound site (no evidencing line) must never stop")
	}
	if f.m.m4NotPaidRecoveredLine(f.a, "OTHER", ev) {
		t.Fatal("a finding keyed on another reference must not stop")
	}
	if f.m.m4NotPaidRecoveredLine(f.a, "L", &evidencingLine{amount: big.NewInt(99), asset: "EUR"}) ||
		f.m.m4NotPaidRecoveredLine(f.a, "L", &evidencingLine{amount: big.NewInt(100), asset: "USD"}) {
		t.Fatal("an evidencing line of another amount or asset must not stop")
	}

	f = mk()
	f.r.amount = big.NewInt(101)
	if _, ok := f.m.m4NotPaidRecovered(f.a); ok {
		t.Fatal("a resolution amount other than the attempt's must not stop")
	}
	f = mk()
	f.r.asset = "USD"
	if _, ok := f.m.m4NotPaidRecovered(f.a); ok {
		t.Fatal("a resolution asset other than the attempt's must not stop")
	}
	f = mk()
	f.r.attemptID = uuid.New() // the map entry no longer belongs to this attempt
	if _, ok := f.m.m4NotPaidRecovered(f.a); ok {
		t.Fatal("another attempt's resolution must not stop")
	}
	f = mk()
	f.r.kind = m4KindPaid
	if _, ok := f.m.m4NotPaidRecovered(f.a); ok {
		t.Fatal("a paid resolution is not the not-paid rule")
	}
	f = mk()
	f.r.recovered = nil
	if _, ok := f.m.m4NotPaidRecovered(f.a); ok {
		t.Fatal("an unread recovery must not stop")
	}
	f = mk()
	f.a.operation = paymentStatementKindDeposit
	if _, ok := f.m.m4NotPaidRecovered(f.a); ok {
		t.Fatal("a deposit attempt is never in scope")
	}
	f = mk()
	delete(f.m.k3.m4NotPaid, f.a.id)
	if _, ok := f.m.m4NotPaidRecovered(f.a); ok {
		t.Fatal("no executed M4 not-paid: nothing stops")
	}
	// Repeated evaluation is identical (no state is consumed).
	f = mk()
	for i := 0; i < 3; i++ {
		if s, r1 := f.run(t); s != 0 || r1 != 0 {
			t.Fatalf("run %d: raised standing=%d r1=%d", i, s, r1)
		}
		f.m.r = &sbRecorder{}
		f.m.k3.capturedSeen = map[string]bool{}
	}
}

// --- r30 (owner decisions 1 and 2 of 2026-10-09, ADR 0095 §48; ADR 0111 §21) ---

// RR30-3: a reversed payout line on the references R-1 reads keeps both
// findings raised even after a full recovery of exactly one equal payout (a
// recovery from the player after the PSP reported the payout reversed is a
// possible over-recovery, never a silent stop).
func TestRR30_Pure_ReversedPayoutLineKeepsRaising(t *testing.T) {
	now := time.Now().UTC()
	f := rr1New(100).add(rr1Line("L", "M", 100, "EUR", now))
	rev := rr1Line("L", "M", 100, "EUR", now.Add(time.Second))
	rev.status = "reversed"
	f.add(rev)
	if s, r1 := f.run(t); s == 0 || r1 != 1 {
		t.Fatalf("want STANDING-1 and R-1 raised with a reversed line, got standing=%d r1=%d", s, r1)
	}
	// Control: a declined line (the M4 evidence shape) does not block the stop.
	g := rr1New(100).add(rr1Line("L", "M", 100, "EUR", now))
	dec := rr1Line("D", "M", 100, "EUR", now)
	dec.status = "declined"
	g.add(dec)
	if s, r1 := g.run(t); s != 0 || r1 != 0 {
		t.Fatalf("control: a declined line must not block the stop, got standing=%d r1=%d", s, r1)
	}
}

// matchedRef records ref as a matched reference of the resolution, as
// loadK3Evidence derives it from the lines matched by the merchant reference.
func (f *rr1Fix) matchedRef(ref string) *rr1Fix {
	f.r.matchedRefs = append(f.r.matchedRefs, ref)
	return f
}

// rr30Bound turns the fixture into a BOUND destination_mismatch park holding X
// (pinned at submission), as B13-B parks it.
func rr30Bound(recovered int64) *rr1Fix {
	f := rr1New(recovered)
	f.a.terminalReason = "destination_mismatch"
	f.a.providerRef = "X"
	f.r.pinnedRef = "X"
	f.r.attemptRef = "X"
	f.m.byRef[paymentStatementKindPayout+"\x00X"] = f.a
	return f
}

// Owner decision 1 (Q-R21-1 DECIDED): the BOUND site of a recovered
// destination_mismatch park stops raising ONLY on the complete predicate:
// NET recovery, exactly one equal succeeded payout, positively this park's (on
// X, or carrying the attempt's merchant reference, and naming no other
// merchant), no reversed line, still holding the pinned X. Every negation
// keeps raising.
func TestRR30_Pure_BoundDestinationMismatch_StopRuleAndEveryNegation(t *testing.T) {
	now := time.Now().UTC()
	on := func(ref, merchant string) *persistedLine { return rr1Line(ref, merchant, 100, "EUR", now) }
	cases := []struct {
		name    string
		f       *rr1Fix
		stopped bool
	}{
		{"line on X with the merchant reference: stops", rr30Bound(100).add(on("X", "M")), true},
		{"line on X without a merchant reference: stops (on X)", rr30Bound(100).add(on("X", "")), true},
		{"line on another reference carrying the attempt's merchant reference: stops", rr30Bound(100).add(on("Z", "M")), true},
		{"a re-delivery without the merchant reference joins the group: stops", rr30Bound(100).add(on("Z", "M")).addMatched(on("Z", "")), true},
		{"unrelated payout: another reference, no merchant reference: raises", rr30Bound(100).addMatched(on("Z", "")), false},
		{"line on X naming another merchant reference: raises", rr30Bound(100).add(on("X", "OTHER")), false},
		{"the group also names another merchant reference: raises", rr30Bound(100).add(on("Z", "M")).add(on("Z", "OTHER")).matchedRef("Z"), false},
		{"debit alone, no succeeded payout: raises", rr30Bound(100), false},
		{"partial NET recovery 99 of 100: raises", rr30Bound(99).add(on("X", "M")), false},
		{"no recovery: raises", rr30Bound(0).add(on("X", "M")), false},
		{"two distinct payouts: raises", rr30Bound(100).add(on("X", "M")).add(rr1Line("X", "M", 100, "EUR", now.Add(time.Second))), false},
		{"unequal amount: raises", rr30Bound(100).add(rr1Line("X", "M", 99, "EUR", now)), false},
		{"other asset: raises", rr30Bound(100).add(rr1Line("X", "M", 100, "USD", now)), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raises := c.f.m.capturedUnposted(c.f.a)
			if raises == c.stopped {
				t.Fatalf("stopped=%v want %v", !raises, c.stopped)
			}
			// The whole bound path (no line this run): checkUnmatchedAttempts.
			c.f.m.checkUnmatchedAttempts()
			n := 0
			for _, x := range c.f.m.r.mismatches {
				if x.MismatchKind == MismatchKindPayCapturedUnposted {
					n++
					if x.ExpectedValue != m4NotPaidCapturedUnpostedResolutionHint {
						t.Fatalf("a raised bound finding keeps the post-M4 hint, got %q", x.ExpectedValue)
					}
				}
			}
			if (n == 0) != c.stopped {
				t.Fatalf("checkUnmatchedAttempts raised %d, stopped want %v", n, c.stopped)
			}
		})
	}

	// Reversed line on X after the recovery: raises.
	f := rr30Bound(100).add(on("X", "M"))
	rev := on("X", "M")
	rev.status = "reversed"
	rev.occurredAt = now.Add(time.Second)
	f.add(rev)
	if !f.m.capturedUnposted(f.a) {
		t.Fatal("a reversed payout line must keep the bound finding raised")
	}
	// Scope: destination_integrity_failure is NOT M4-eligible; any other
	// reason, a park no longer holding the pinned X, an empty X, and a run
	// without K3 evidence never stop.
	for _, mut := range []struct {
		name string
		fn   func(f *rr1Fix)
	}{
		{"destination_integrity_failure", func(f *rr1Fix) { f.a.terminalReason = "destination_integrity_failure" }},
		{"another bound reason", func(f *rr1Fix) { f.a.terminalReason = "callback_amount_asset_mismatch" }},
		{"pinned reference differs from the held X", func(f *rr1Fix) { f.r.pinnedRef = "X-old" }},
		{"no held reference at all", func(f *rr1Fix) { f.a.providerRef = ""; f.r.pinnedRef = "" }},
		{"no executed M4 not-paid", func(f *rr1Fix) { delete(f.m.k3.m4NotPaid, f.a.id) }},
		{"no K3 evidence", func(f *rr1Fix) { f.m.k3 = nil }},
	} {
		f := rr30Bound(100).add(on("Z", "M"))
		if !f.m.m4NotPaidRecoveredBound(f.a) {
			t.Fatalf("%s: control must stop", mut.name)
		}
		mut.fn(f)
		if f.m.m4NotPaidRecoveredBound(f.a) {
			t.Fatalf("%s: must never stop", mut.name)
		}
	}
	// R-1 and the bound site are one rule over one recovery: an unrelated
	// payout lets R-1 stop (the recovery covers the one payout) but the bound
	// finding keeps raising (that payout is not positively this park's).
	g := rr30Bound(100).addMatched(on("Z", ""))
	if _, ok := g.m.m4NotPaidRecovered(g.a); !ok {
		t.Fatal("control: the shared RR-1 predicate holds")
	}
	if !g.m.capturedUnposted(g.a) {
		t.Fatal("the bound finding must keep raising on an unattributed payout")
	}
}
