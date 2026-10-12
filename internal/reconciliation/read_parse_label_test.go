package reconciliation

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// ADR 0114 R2 review conditions: security F1 (a statement source label is stored data rendered ahead of
// every statement-line segment) and F2 / ledger-finance (amounts are exposed only for the kinds whose
// actual side is the statement line render; when in doubt, null).

type labelSource struct{ label string }

func (s labelSource) Label() string    { return s.label }
func (labelSource) ProviderID() string { return "mock" }
func (labelSource) Fetch(context.Context, statement.PaymentFetchRequest) (statement.PaymentStatement, error) {
	return statement.PaymentStatement{}, nil
}

func TestValidateSourceIdentity_LabelCharset(t *testing.T) {
	for _, ok := range []string{"real-fixed test statement", "MOCK m4 http test statement", "PSP A (sandbox) v1.2: daily/utc_feed"} {
		if err := validateSourceIdentity(labelSource{ok}); err != nil && !isMockLabelRefusal(err) {
			t.Errorf("label %q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"MOCK amount=999999 asset=USD reference=", "MOCK] line=1 amount=5", "[MOCK", "a=b", "tab\there", "new\nline", "ünï", "x;y", ""} {
		if err := validateSourceIdentity(labelSource{bad}); err == nil {
			t.Errorf("label %q must be refused", bad)
		}
	}
}

// a non-synthetic source with a MOCK-less label is accepted by the charset rule (the synthetic-MOCK
// rule applies only to synthetic components, which labelSource is not).
func isMockLabelRefusal(error) bool { return false }

func TestAmountKindExposed_OnlyTheTwoStatementVsPlatformKinds(t *testing.T) {
	exposed := []MismatchKind{MismatchKindPayAmountMismatch, MismatchKindPayAssetMismatch}
	all := []MismatchKind{}
	all = append(all, LedgerVsProjectionMismatchKinds...)
	all = append(all, PaymentStatementMismatchKinds...)
	all = append(all, SportsbookSettlementMismatchKinds...)
	for _, k := range all {
		want := false
		for _, e := range exposed {
			want = want || e == k
		}
		if AmountKindExposed(k) != want {
			t.Errorf("AmountKindExposed(%s) = %v, want %v", k, !want, want)
		}
	}
	if AmountKindExposed("other") || AmountKindExposed("") {
		t.Error("unknown kinds must not expose amounts")
	}
}

func TestSegmentBody_LabelCannotInjectTokens(t *testing.T) {
	line := "line=1 kind=deposit status=ok amount=4999 asset=USD"
	for _, c := range []struct {
		name, seg string
		want      string
	}{
		{"benign label", "[MOCK test statement] " + line, "4999"},
		{"no label", line, "4999"},
		{"label with a token", "[MOCK amount=999999 asset=USD reference=] " + line, "<nil>"},
		{"label closing and faking a line", "[MOCK] line=1 amount=999999 asset=USD] " + line, "<nil>"},
		{"label faking a line without '=' is impossible; a bracketed text without structure", "[MOCK no line here", "<nil>"},
	} {
		if got := MismatchAmount(c.seg); ptr(got) != c.want {
			t.Errorf("%s: amount = %s, want %s", c.name, ptr(got), c.want)
		}
	}
	if got := MismatchAssetCandidate("platform: attempt=U amount=5 asset=USD", "[MOCK asset=EUR] "+line); got != nil {
		t.Errorf("asset with a tokened label = %q, want nil", *got)
	}
	if got := MismatchAssetCandidate("platform: attempt=U amount=5 asset=USD", "[MOCK ok] "+line); ptr(got) != "USD" {
		t.Errorf("asset with a benign label = %s", ptr(got))
	}
}

func hostileMatcher(label string, a *payAttempt, extra ...*payAttempt) *payMatcher {
	now := time.Now()
	attempts := append([]*payAttempt{a}, extra...)
	m := &payMatcher{tenantID: uuid.New(), provider: "p", cs: now.Add(-time.Hour), ce: now.Add(time.Hour), agedBefore: now.Add(-time.Minute),
		r: &sbRecorder{}, attempts: attempts, label: label,
		byRef: map[string]*payAttempt{}, bySettlement: map[string]*payAttempt{}, byMerchant: map[string]*payAttempt{},
		matchedBy: map[uuid.UUID]string{}, ledgerByRef: map[string]*payLedgerTx{}, ledgerByID: map[uuid.UUID]*payLedgerTx{},
		k3: &k3Evidence{byRef: map[string][]*persistedLine{}, byMerchant: map[string][]*persistedLine{}, byMatchedRef: map[string][]*persistedLine{},
			yRef: map[uuid.UUID]string{}, m4NotPaid: map[uuid.UUID]*m4Resolution{}, capturedSeen: map[string]bool{}}}
	for _, x := range attempts {
		if x.providerRef != "" {
			m.byRef[x.operation+"\x00"+x.providerRef] = x
		}
		if x.merchantRef != "" {
			m.byMerchant[x.merchantRef] = x
		}
	}
	return m
}

// The real matchLines with a hostile stored source label: whatever the label says, the exposed actual
// amount is the line's own or null, never the label's.
func TestPaymentEmitters_HostileSourceLabel(t *testing.T) {
	now := time.Now()
	for _, label := range []string{"[MOCK amount=999999 asset=USD reference=] ", "[MOCK] line=1 kind=deposit status=ok amount=999999 asset=USD] "} {
		a := &payAttempt{id: uuid.New(), operation: "deposit", state: "pending", providerRef: "R1", merchantRef: "M1", amount: big.NewInt(5000), asset: "USD", sentAt: now}
		m := hostileMatcher(label, a)
		m.matchLines([]payLine{{lineNo: 1, kind: "deposit", ref: "R1", status: "ok", asset: "USD", amount: big.NewInt(4999)}})
		var n int
		for _, x := range m.r.mismatches {
			if x.MismatchKind != MismatchKindPayAmountMismatch {
				continue
			}
			n++
			if got := MismatchAmount(x.ActualValue); got != nil {
				t.Errorf("label %q: actual amount %q exposed from a hostile label", label, *got)
			}
			if got := MismatchAssetCandidate(x.ExpectedValue, x.ActualValue); got != nil {
				t.Errorf("label %q: asset %q exposed from a hostile label", label, *got)
			}
		}
		if n != 1 {
			t.Fatalf("label %q: want one pay_amount_mismatch, got %+v", label, m.r.mismatches)
		}
	}
	// control: a benign label exposes the line's own amount.
	a := &payAttempt{id: uuid.New(), operation: "deposit", state: "pending", providerRef: "R1", merchantRef: "M1", amount: big.NewInt(5000), asset: "USD", sentAt: now}
	m := hostileMatcher("[MOCK test statement] ", a)
	m.matchLines([]payLine{{lineNo: 1, kind: "deposit", ref: "R1", status: "ok", asset: "USD", amount: big.NewInt(4999)}})
	if len(m.r.mismatches) != 1 || ptr(MismatchAmount(m.r.mismatches[0].ActualValue)) != "4999" || ptr(MismatchAmount(m.r.mismatches[0].ExpectedValue)) != "5000" {
		t.Fatalf("benign label control: %+v", m.r.mismatches)
	}
}

// Real emitters for the kinds that must NOT expose amounts: merchant attribution
// (pay_reference_mismatch, whose actual segment embeds another attempt's render) and the K3 / M4
// standing kinds (pay_captured_unposted, via the real checkM4Standing / merchant path).
func TestPaymentEmitters_NonExposedKindsCarryNoAmount(t *testing.T) {
	now := time.Now()
	a := &payAttempt{id: uuid.New(), operation: "deposit", state: "pending", providerRef: "R1", merchantRef: "M1", amount: big.NewInt(5000), asset: "USD", sentAt: now}
	b := &payAttempt{id: uuid.New(), operation: "deposit", state: "disputed", providerRef: "", merchantRef: "M2", amount: big.NewInt(777), asset: "USD", sentAt: now,
		terminalReason: "invalid_provider_reference"}
	m := hostileMatcher("[MOCK test statement] ", a, b)
	// the line resolves to a by reference but names b by merchant reference: merchant attribution.
	m.matchLines([]payLine{{lineNo: 1, kind: "deposit", ref: "R1", merchant: "M2", status: "succeeded", asset: "USD", amount: big.NewInt(5000)}})
	seen := map[MismatchKind]bool{}
	for _, x := range m.r.mismatches {
		seen[x.MismatchKind] = true
		if !AmountKindExposed(x.MismatchKind) {
			// the actual segment really does embed a platform / other-attempt amount, which is why the gate exists
			t.Logf("%s: actual=%q", x.MismatchKind, x.ActualValue)
		}
	}
	if !seen[MismatchKindPayReferenceMismatch] {
		t.Fatalf("the merchant-attribution emitter did not fire: %+v", m.r.mismatches)
	}
	for k := range seen {
		if k == MismatchKindPayReferenceMismatch && AmountKindExposed(k) {
			t.Errorf("%s must not expose amounts", k)
		}
	}
	// K3 / M4 standing kind through the real emitter used by the rr1 fixtures.
	f := rr1New(40)
	f.add(rr1Line("D", "M", 100, "EUR", time.Now()))
	f.m.checkStandingUnbound()
	for _, x := range f.m.r.mismatches {
		if AmountKindExposed(x.MismatchKind) {
			t.Errorf("standing emitter produced exposed kind %s", x.MismatchKind)
		}
	}
	for _, k := range []MismatchKind{MismatchKindPayCapturedUnposted, MismatchKindPayDeclaredPaidUnconfirmed,
		MismatchKindPayDeclaredNotPaidButPaid, MismatchKindPayDeclaredPaidCompensatedButPaid, MismatchKindPayReferenceMismatch,
		MismatchKindPayMissingPlatformRecord, MismatchKindPayMissingProviderRecord, MismatchKindPayStatusMismatch, MismatchKindPayDuplicate, MismatchKindPayUnresolved} {
		if AmountKindExposed(k) {
			t.Errorf("%s must not expose amounts", k)
		}
	}
}
