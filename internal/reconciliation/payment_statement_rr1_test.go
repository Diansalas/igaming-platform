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
// site (no evidencing line) never stop.
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
