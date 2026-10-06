package reconciliation

// PRH-2 D2 review P1 (ledger-finance; code review D2-2): the cross-package
// classification pin. Every deposit dispute terminal reason the payments
// package can write (payments.DepositDisputeTerminalReasons, PRH-2 D1) must be
// classified EXPLICITLY by reconciliation as bound, unbound or excluded for
// pay_captured_unposted (ADR 0095 §35). A reason added on the payments side
// without a decision here fails this test, instead of being silently excluded
// at run time. Unit test: no database.
//
// The table says what a reason IS. The runtime rule (captureClass, D2 code
// final review D2F-1) decides by the attempt row: a bound or
// bound-if-referenced reason is bound ONLY when the attempt holds a provider
// reference, and unbound otherwise (in-run by merchant reference, cleared on
// the line's reference). Unbound stays unbound and excluded stays excluded
// whatever the row holds. TestD2_P1_RuntimeRule pins this for every reason.

import (
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/providerref"
)

// d2ExpectedClasses is the ledger-finance classification (D2 review P1),
// written against the payments constants.
var d2ExpectedClasses = map[string]disputeReasonClass{
	payments.TerminalReasonMultipleSuccessForIntent:    reasonBound,
	payments.TerminalReasonSyncAmountMismatch:          reasonBound,
	payments.TerminalReasonPollAmountMismatch:          reasonBound,
	payments.TerminalReasonPollReferenceMismatch:       reasonBound,
	payments.TerminalReasonCallbackAmountAssetMismatch: reasonBound,
	payments.TerminalReasonProviderReferenceConflict:   reasonBoundIfReferenced, // LF D2 final PM-1
	payments.TerminalReasonInvalidProviderReference:    reasonUnbound,           // bare form (PAY-POLL-ECHO-HARDENING-1)
	payments.TerminalReasonSuccessForNeverSentAttempt:  reasonBoundIfReferenced,
	payments.TerminalReasonTombstonePrecedesSuccess:    reasonExcluded,
}

// d2InvalidRefSuffixes: the closed providerref reasons, the suffixes of the
// invalid_provider_reference: prefix family.
var d2InvalidRefSuffixes = []string{
	string(providerref.ReasonEmpty), string(providerref.ReasonTooLong),
	string(providerref.ReasonInvalidUTF8), string(providerref.ReasonControlChar),
}

func className(c disputeReasonClass) string {
	switch c {
	case reasonExcluded:
		return "excluded"
	case reasonBound:
		return "bound"
	case reasonUnbound:
		return "unbound"
	case reasonBoundIfReferenced:
		return "bound-if-referenced"
	default:
		return "UNCLASSIFIED"
	}
}

// d2ReasonsToCheck expands payments' list: a prefix entry ("...:") is checked
// with representative suffixes AND in its bare form.
func d2ReasonsToCheck(t *testing.T) []string {
	t.Helper()
	list := payments.DepositDisputeTerminalReasons()
	if len(list) == 0 {
		t.Fatal("payments.DepositDisputeTerminalReasons() is empty")
	}
	var out []string
	for _, r := range list {
		if strings.HasSuffix(r, ":") {
			for _, s := range d2InvalidRefSuffixes {
				out = append(out, r+s)
			}
			out = append(out, strings.TrimSuffix(r, ":"))
			continue
		}
		out = append(out, r)
	}
	return out
}

// TestD2_P1_EveryPaymentsDepositDisputeReasonIsClassified: the pin. Fails on
// any reason payments can write that reconciliation does not classify, and on
// any classification that departs from the ledger-finance table.
func TestD2_P1_EveryPaymentsDepositDisputeReasonIsClassified(t *testing.T) {
	for _, r := range d2ReasonsToCheck(t) {
		got := classifyDisputeReason(r)
		if got == reasonUnclassified {
			t.Errorf("deposit dispute reason %q (payments.DepositDisputeTerminalReasons) is NOT classified by reconciliation: add it to disputeReasonClasses as bound, unbound or excluded (ADR 0095 §35)", r)
			continue
		}
		key := r
		if strings.HasPrefix(r, payments.TerminalReasonInvalidProviderReference+":") {
			key = payments.TerminalReasonInvalidProviderReference
		}
		want, ok := d2ExpectedClasses[key]
		if !ok {
			t.Errorf("reason %q is classified %s by reconciliation but has no ledger-finance expectation in this test: add one", r, className(got))
			continue
		}
		if got != want {
			t.Errorf("reason %q: reconciliation classifies it %s, ledger-finance requires %s", r, className(got), className(want))
		}
	}
}

// TestD2_P1_NoStaleClassification: every row of reconciliation's table is a
// reason payments can actually write (exactly, or via the prefix), so the
// table cannot drift into dead entries that hide a renamed reason.
func TestD2_P1_NoStaleClassification(t *testing.T) {
	for r := range disputeReasonClasses {
		if !payments.IsDepositDisputeTerminalReason(r) {
			t.Errorf("reconciliation classifies %q, which payments.DepositDisputeTerminalReasons() does not list (renamed or removed?)", r)
		}
	}
}

// TestD2_P1_UnknownReasonIsUnclassified proves the pin can fail: a reason
// payments has never written is reasonUnclassified, never silently mapped.
func TestD2_P1_UnknownReasonIsUnclassified(t *testing.T) {
	for _, r := range []string{"", "d2_some_future_reason", "invalid_provider_referenceX", "INVALID_PROVIDER_REFERENCE:control_char"} {
		if c := classifyDisputeReason(r); c != reasonUnclassified {
			t.Errorf("unknown reason %q classified %s, want UNCLASSIFIED", r, className(c))
		}
	}
}

// TestD2_P1_BoundIfReferenced pins T15's resolution against the attempt row:
// bound when it holds a reference, unbound otherwise, never excluded.
func TestD2_P1_BoundIfReferenced(t *testing.T) {
	r := payments.TerminalReasonSuccessForNeverSentAttempt
	withRef := &payAttempt{state: "disputed", terminalReason: r, providerRef: "psp-ref-1"}
	noRef := &payAttempt{state: "disputed", terminalReason: r}
	if !withRef.boundCapture() || withRef.unboundPark() {
		t.Errorf("%s with a reference must be bound, got %s", r, className(withRef.captureClass()))
	}
	if !noRef.unboundPark() || noRef.boundCapture() {
		t.Errorf("%s without a reference must be unbound, got %s", r, className(noRef.captureClass()))
	}
	notDisputed := &payAttempt{state: "declined", terminalReason: r, providerRef: "psp-ref-1"}
	if notDisputed.boundCapture() || notDisputed.unboundPark() {
		t.Error("only a disputed attempt is a captured-unposted candidate")
	}
}

// TestD2_P1_RuntimeRule pins captureClass for EVERY reason payments can write,
// with and without a stored reference (D2F-1): bound and bound-if-referenced
// resolve to bound only with a reference and to unbound without one; unbound
// and excluded are unaffected by the reference.
func TestD2_P1_RuntimeRule(t *testing.T) {
	for _, r := range d2ReasonsToCheck(t) {
		class := classifyDisputeReason(r)
		for _, ref := range []string{"psp-ref-1", ""} {
			a := &payAttempt{state: "disputed", terminalReason: r, providerRef: ref}
			got := a.captureClass()
			var want disputeReasonClass
			switch class {
			case reasonBound, reasonBoundIfReferenced:
				want = reasonUnbound
				if ref != "" {
					want = reasonBound
				}
			default:
				want = class
			}
			if got != want {
				t.Errorf("reason %q (table class %s) with reference %q: runtime class %s, want %s", r, className(class), ref, className(got), className(want))
			}
		}
	}
}
