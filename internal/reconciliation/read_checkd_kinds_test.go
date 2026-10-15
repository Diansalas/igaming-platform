package reconciliation

import "testing"

// A10b G5 (minimal): the ADR 0113 s4.4 check D kinds are part of the ledger_vs_projection staff-view vocabulary, so a
// withdrawal-hold finding is reported under its own kind and not as `other`. They must stay in step with the
// constants check D emits and with the OpenAPI StaffReconciliationKind enum (pinned in the httpserver drift test).
func TestStaffViewVocabulary_IncludesCheckDKinds(t *testing.T) {
	kinds, ok := StaffViewMismatchKinds(StreamLedgerVsProjection)
	if !ok {
		t.Fatal("ledger_vs_projection must be a staff-view stream")
	}
	have := map[MismatchKind]bool{}
	for _, k := range kinds {
		have[k] = true
	}
	for _, want := range []MismatchKind{MismatchKindWithdrawalHoldOpenMismatch, MismatchKindWithdrawalRequestLedgerLinkInvalid} {
		if !have[want] {
			t.Errorf("check D kind %q missing from the ledger_vs_projection vocabulary", want)
		}
	}
	// check D kinds belong to no other stream
	for _, s := range []Stream{StreamPaymentStatement, StreamSportsbookSettlement} {
		other, _ := StaffViewMismatchKinds(s)
		for _, k := range other {
			if k == MismatchKindWithdrawalHoldOpenMismatch || k == MismatchKindWithdrawalRequestLedgerLinkInvalid {
				t.Errorf("check D kind %q must not appear in stream %s", k, s)
			}
		}
	}
}
