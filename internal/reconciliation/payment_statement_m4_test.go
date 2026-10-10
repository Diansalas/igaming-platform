package reconciliation

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// PAY-PAYOUT-UNBOUND-RESOLVE-1 R-2 (ADR 0111 §4.6, ledger-finance ruling): the
// ledger_join attributes a withdrawal_completed posting t to an executed
// m4_evidence_paid ONLY when the resolution links t AND the same attempt's
// withdrawal releases with t. Pure matcher-level cases (no database): every
// direction of a mismatched link, a non-paid M4 kind, and double attribution.
func r2Matcher(attemptRelease *uuid.UUID, m4 []m4Resolution, extra ...*payAttempt) (*payMatcher, *payLedgerTx, *payAttempt) {
	now := time.Now()
	t1 := &payLedgerTx{id: uuid.New(), txType: "withdrawal_completed", ref: "R-1", postedAt: now, amount: big.NewInt(100), asset: "EUR"}
	a := &payAttempt{id: uuid.New(), operation: "payout", state: "disputed", amount: big.NewInt(100), asset: "EUR",
		terminalReason: "invalid_provider_reference:control_char", sentAt: now, releaseTx: attemptRelease}
	for i := range m4 {
		if m4[i].attemptID == uuid.Nil {
			m4[i].attemptID = a.id
		}
		if m4[i].ledgerTx == uuid.Nil {
			m4[i].ledgerTx = t1.id
		}
	}
	m := &payMatcher{tenantID: uuid.New(), provider: "p", cs: now.Add(-time.Hour), ce: now.Add(time.Hour),
		r: &sbRecorder{}, attempts: append([]*payAttempt{a}, extra...), ledger: []*payLedgerTx{t1},
		k3: &k3Evidence{m4: m4}}
	return m, t1, a
}

func r2Raised(m *payMatcher, t1 *payLedgerTx) bool {
	for _, x := range m.r.mismatches {
		if x.MismatchKind == MismatchKindPayMissingPlatformRecord && strings.Contains(x.ReconciliationKey, "provider_tx_id="+t1.ref) {
			return true
		}
	}
	return false
}

func TestR2_M4PaidCompletion_AttributionCases(t *testing.T) {
	// Attributed: both links point at t1.
	{
		m, t1, a := r2Matcher(nil, []m4Resolution{{kind: m4KindPaid}})
		a.releaseTx = &t1.id
		m.checkLedgerJoin()
		if r2Raised(m, t1) {
			t.Fatal("an executed M4-paid completion linked both ways must be attributed (n = 1)")
		}
	}
	// The resolution links t1 but the withdrawal releases another transaction.
	{
		other := uuid.New()
		m, t1, _ := r2Matcher(&other, []m4Resolution{{kind: m4KindPaid}})
		m.checkLedgerJoin()
		if !r2Raised(m, t1) {
			t.Fatal("resolution -> t1 but withdrawal -> other must still raise")
		}
	}
	// The withdrawal releases t1 but the resolution links another transaction.
	{
		m, t1, a := r2Matcher(nil, []m4Resolution{{kind: m4KindPaid, ledgerTx: uuid.New()}})
		a.releaseTx = &t1.id
		m.checkLedgerJoin()
		if !r2Raised(m, t1) {
			t.Fatal("withdrawal -> t1 but resolution -> other must still raise")
		}
	}
	// An M4 NOT-paid never attributes a completion.
	{
		m, t1, a := r2Matcher(nil, []m4Resolution{{kind: m4KindNotPaid}})
		a.releaseTx = &t1.id
		m.checkLedgerJoin()
		if !r2Raised(m, t1) {
			t.Fatal("a not-paid M4 must never attribute a completion")
		}
	}
	// No M4 at all (e.g. a pending, refused or M2 resolution, none of which is loaded as an executed M4).
	{
		m, t1, a := r2Matcher(nil, nil)
		a.releaseTx = &t1.id
		m.checkLedgerJoin()
		if !r2Raised(m, t1) {
			t.Fatal("without an executed M4-paid the completion of a disputed attempt raises")
		}
	}
	// Double attribution: a succeeded attempt AND the M4 both claim t1: n = 2 raises.
	{
		b := &payAttempt{id: uuid.New(), operation: "payout", state: "succeeded", amount: big.NewInt(100), asset: "EUR",
			sentAt: time.Now(), releaseIsCompletion: true}
		m, t1, a := r2Matcher(nil, []m4Resolution{{kind: m4KindPaid}}, b)
		a.releaseTx = &t1.id
		b.releaseTx = &t1.id
		m.checkLedgerJoin()
		if !r2Raised(m, t1) {
			t.Fatal("a posting attributed both ways must raise (n = 2)")
		}
	}
	_ = context.Background()
}

// S-1 / R-1 pure cases: the predicates read every import, any occurred_at.
func TestR1_M4Predicates_Pure(t *testing.T) {
	now := time.Now()
	mk := func(kind string, lines ...*persistedLine) *payMatcher {
		a := &payAttempt{id: uuid.New(), operation: "payout", state: "disputed", amount: big.NewInt(100), asset: "EUR", merchantRef: "M"}
		e := &k3Evidence{byRef: map[string][]*persistedLine{}, byMerchant: map[string][]*persistedLine{}, yRef: map[uuid.UUID]string{},
			m4: []m4Resolution{{id: uuid.New(), attemptID: a.id, kind: kind, ledgerTx: uuid.New(), amount: big.NewInt(100), reference: "R", attemptMerchnt: "M", pinnedRef: "B"}}}
		for _, l := range lines {
			e.byRef[l.ref] = append(e.byRef[l.ref], l)
			if l.merchant != "" {
				e.byMerchant[l.merchant] = append(e.byMerchant[l.merchant], l)
			}
		}
		return &payMatcher{provider: "p", r: &sbRecorder{}, attempts: []*payAttempt{a}, k3: e}
	}
	line := func(ref, merchant, status string, at time.Time) *persistedLine {
		return &persistedLine{kind: "payout", ref: ref, merchant: merchant, status: status, amount: big.NewInt(100), asset: "EUR", occurredAt: at}
	}
	cases := []struct {
		name  string
		m     *payMatcher
		raise bool
	}{
		{"paid: clean (the evidenced line only)", mk(m4KindPaid, line("R", "M", "succeeded", now)), false},
		{"paid: declined only on R, back-dated", mk(m4KindPaid, line("R", "M", "succeeded", now), line("R", "", "declined", now.Add(-500*time.Hour))), true},
		{"paid: reversed on the merchant reference", mk(m4KindPaid, line("R", "M", "succeeded", now), line("Z", "M", "reversed", now)), true},
		{"paid: a second distinct succeeded line", mk(m4KindPaid, line("R", "M", "succeeded", now), line("Q", "M", "succeeded", now)), true},
		{"paid: a pending line is not an R-1 trigger", mk(m4KindPaid, line("R", "M", "succeeded", now), line("R", "", "pending", now)), false},
		{"not paid: succeeded on the bound reference only", mk(m4KindNotPaid, line("B", "", "succeeded", now)), true},
		{"not paid: succeeded on the merchant reference", mk(m4KindNotPaid, line("Z", "M", "succeeded", now)), true},
		{"not paid: only declined lines", mk(m4KindNotPaid, line("Z", "M", "declined", now)), false},
	}
	for _, c := range cases {
		// Since r21 (RR-1) the not-paid recovery is read by loadK3Evidence, so
		// checkM4Standing needs no database for either kind (no recovery here:
		// a not-paid with a succeeded line raises).
		if err := c.m.checkM4Standing(context.Background(), nil); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := len(c.m.r.mismatches) > 0
		if got != c.raise {
			t.Errorf("%s: raised=%v, want %v (%v)", c.name, got, c.raise, c.m.r.mismatches)
		}
	}
}

// Review amendment C-3/C-4/RR-2 (LF): the destination park wording. B13-B has
// landed and classifies both reasons (bound-if-referenced), so the hint is now
// LIVE: this pin flipped deliberately at the B13-B merge. Pin 1 - both reasons
// are classified (never reasonUnclassified). Pin 2 - selection by reason, never
// the generic payout text or the M4-scope text; integrity failure gets its own
// wording with no M4 route (RR-2). Pin 3 - an executed M4 not-paid keeps
// precedence. Pin 4 - a REFERENCED destination park raises a finding.
func TestC3_DestinationHint_LiveAfterClassification_SelectedByReason(t *testing.T) {
	const want = "payout reported to a destination other than the bound one: no completion against the player's hold; PSP recall/return or off-platform recovery; M4 not-paid only on positive decline evidence; never allocation"
	if destinationPayoutCapturedUnpostedResolutionHint != want {
		t.Fatalf("destination hint text drifted: %q", destinationPayoutCapturedUnpostedResolutionHint)
	}
	for _, r := range []string{"destination_mismatch", "destination_integrity_failure"} {
		if c, ok := payoutDisputeReasonClasses[r]; !ok || c != reasonBoundIfReferenced {
			t.Fatalf("%s: payout classification %v (present=%v), want bound-if-referenced", r, c, ok)
		}
		wantHint := want
		if r == "destination_integrity_failure" {
			wantHint = destinationIntegrityPayoutCapturedUnpostedResolutionHint
			if strings.Contains(wantHint, "M4 not-paid only") {
				t.Fatalf("integrity-failure hint must not advertise an M4 route: %q", wantHint)
			}
		}
		for _, ref := range []string{"X-bound", ""} {
			a := &payAttempt{operation: "payout", state: "disputed", terminalReason: r, providerRef: ref}
			if ref != "" && !a.boundCapture() {
				t.Fatalf("%s/%q: a referenced destination park must raise a captured-unposted finding", r, ref)
			}
			if got := capturedUnpostedHintFor(a, capturedUnpostedResolutionHint); got != wantHint {
				t.Fatalf("%s/%q: hint %q", r, ref, got)
			}
			a.m4NotPaid = true
			if got := capturedUnpostedHintFor(a, capturedUnpostedResolutionHint); got != m4NotPaidCapturedUnpostedResolutionHint {
				t.Fatalf("%s/%q after M4 not-paid: hint %q", r, ref, got)
			}
		}
	}
	// Other payout reasons keep their wording.
	if got := capturedUnpostedHintFor(&payAttempt{operation: "payout", state: "disputed", terminalReason: "provider_reference_conflict"}, ""); got != m4ScopePayoutCapturedUnpostedResolutionHint {
		t.Fatalf("unbound in-scope park: %q", got)
	}
	if got := capturedUnpostedHintFor(&payAttempt{operation: "payout", state: "disputed", terminalReason: "callback_amount_asset_mismatch", providerRef: "X"}, ""); got != payoutCapturedUnpostedResolutionHint {
		t.Fatalf("bound park: %q", got)
	}
}

// ADR 0111 24.4 (ledger-finance MEDIUM-1): a destination_mismatch park caused by an unexpected echo from an adapter that
// declared it cannot echo must not be described as a destination mismatch; a real mismatch keeps its wording.
func TestC3_UnexpectedEchoPark_HasItsOwnHint(t *testing.T) {
	a := &payAttempt{operation: "payout", state: "disputed", terminalReason: "destination_mismatch", providerRef: "X", unexpectedEcho: true}
	got := capturedUnpostedHintFor(a, "")
	if got != unexpectedEchoPayoutCapturedUnpostedResolutionHint || strings.Contains(got, "reported to a destination other than the bound one") {
		t.Fatalf("hint = %q", got)
	}
	if !strings.Contains(got, "no governed completion route") {
		t.Fatal("the hint must state there is no governed completion route")
	}
	a.unexpectedEcho = false
	if got := capturedUnpostedHintFor(a, ""); got != destinationPayoutCapturedUnpostedResolutionHint {
		t.Fatalf("a real mismatch keeps its wording: %q", got)
	}
	a.unexpectedEcho = true
	a.m4NotPaid = true
	if got := capturedUnpostedHintFor(a, ""); got != m4NotPaidCapturedUnpostedResolutionHint {
		t.Fatalf("an executed M4 not-paid keeps precedence: %q", got)
	}
}
