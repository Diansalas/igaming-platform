package reconciliation

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ADR 0114 R2 strict parsers (security C2, ledger-finance F1-F3): unit tests, including the REAL
// payment_statement matcher emitters (payMatcher.matchLines, the same key/render code production
// runs) fed references that contain spaces and injected attempt=/bet=/asset=/amount= tokens.

func subj(s *SubjectRef) string {
	if s == nil {
		return ""
	}
	return s.Type + ":" + s.ID
}

func ptr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func TestTrustedPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"provider=p provider_reference=x attempt=U": "provider=p",
		"a=1 b=2": "a=1 b=2",
		"attempt=U merchant_reference=m asset=EUR":  "attempt=U",
		"line=1 original=o":                         "line=1",
		"x settlement_reference=s":                  "x",
		"provider_tx_id=zzz":                        "",
		"xprovider_reference=notatoken attempt=U":   "xprovider_reference=notatoken attempt=U",
		"a=1 reference=r b=2":                       "a=1",
		"a=1 provider_reference=r reference=r2 b=2": "a=1",
	} {
		if got := TrustedPrefix(in); got != want {
			t.Errorf("TrustedPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMismatchSubject_Hardened(t *testing.T) {
	a, b := uuid.NewString(), uuid.NewString()
	cases := []struct {
		name     string
		stream   Stream
		kind     MismatchKind
		key, exp string
		want     string
	}{
		{"ledger account", StreamLedgerVsProjection, MismatchKindBalanceMismatch, a, "", "ledger_account:" + a},
		{"ledger tx", StreamLedgerVsProjection, MismatchKindLedgerUnlinkedManualAdjustment, a, "", "ledger_transaction:" + a},
		{"ledger key not a uuid", StreamLedgerVsProjection, MismatchKindBalanceMismatch, "attempt=" + a, "", ""},
		{"payment: attempt in key prefix", StreamPaymentStatement, MismatchKindPayUnresolved, "provider=p attempt=" + a + " check=unresolved", "", "payment_attempt:" + a},
		{"payment: attempt after the reference is NOT trusted in the key", StreamPaymentStatement, MismatchKindPayAmountMismatch,
			"provider=p provider_reference=R kind=deposit attempt=" + a + " check=amount", "", ""},
		{"payment: attempt from the expected segment", StreamPaymentStatement, MismatchKindPayAmountMismatch,
			"provider=p provider_reference=R kind=deposit attempt=" + a + " check=amount", "platform: attempt=" + a + " op=deposit amount=5 asset=USD provider_reference=R", "payment_attempt:" + a},
		{"payment: reference with spaces and an injected attempt", StreamPaymentStatement, MismatchKindPayMissingPlatformRecord,
			"provider=p provider_reference=my ref attempt=" + b + " kind=deposit check=resolve", "platform: an attempt of this provider with this reference", ""},
		{"payment: duplicate attempt token in the prefix", StreamPaymentStatement, MismatchKindPayUnresolved,
			"provider=p attempt=" + a + " attempt=" + b + " check=x", "", ""},
		{"payment: injected attempt in the expected prefix twice", StreamPaymentStatement, MismatchKindPayUnresolved,
			"provider=p check=x", "platform: attempt=" + a + " attempt=" + b, ""},
		{"payment: uppercase uuid refused", StreamPaymentStatement, MismatchKindPayUnresolved, "provider=p attempt=" + strings.ToUpper(a), "", ""},
		{"payment: receipt", StreamPaymentStatement, MismatchKindPayUnresolved, "provider=p receipt=" + a + " provider_reference=R check=deferred_receipt", "", "payment_receipt:" + a},
		{"payment: intent", StreamPaymentStatement, MismatchKindPayDuplicate, "provider=p deposit_intent=" + a + " check=duplicate_success", "", "deposit_intent:" + a},
		{"payment: bet token is not a payment subject", StreamPaymentStatement, MismatchKindPayUnresolved, "provider=p bet=" + a, "", ""},
		{"sportsbook: bet", StreamSportsbookSettlement, MismatchKindSBBetNet, "bet=" + a + " check=end_state status=won", "", "bet:" + a},
		{"sportsbook: wallet", StreamSportsbookSettlement, MismatchKindSBLocked, "wallet=" + a + " asset=EUR account_type=player_locked_cash", "", "wallet:" + a},
		{"sportsbook: history then bet", StreamSportsbookSettlement, MismatchKindSBOrphanHistory, "history=" + a + " bet=" + b + " check=reverses", "", "bet:" + b},
		{"sportsbook: statement key embeds free text", StreamSportsbookSettlement, MismatchKindSBMockStatement, "statement: x bet=" + a, "", ""},
		{"sportsbook: ledger key embeds free text", StreamSportsbookSettlement, MismatchKindSBMockStatement, "ledger: x bet=" + a, "", ""},
		{"sportsbook: duplicate bet", StreamSportsbookSettlement, MismatchKindSBBetNet, "bet=" + a + " bet=" + b, "", ""},
	}
	for _, c := range cases {
		if got := subj(MismatchSubject(c.stream, c.kind, c.key, c.exp)); got != c.want {
			t.Errorf("%s: subject = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestMismatchAmount_AllowListAndUniqueness(t *testing.T) {
	for in, want := range map[string]string{
		"open_bet_stakes=500":           "500",
		"ledger_locked_net=-30":         "-30",
		"sportsbook_locked_bonus_net=0": "0",
		"platform: attempt=U op=payout state=pending amount=1250 asset=USD provider_reference=R amount=9": "1250",
		"line=1 kind=deposit status=ok amount=77 asset=USD merchant_reference=M amount=5":                 "77",
	} {
		if got := MismatchAmount(in); ptr(got) != want {
			t.Errorf("MismatchAmount(%q) = %s, want %s", in, ptr(got), want)
		}
	}
	for _, in := range []string{
		"", "cash=1 locked=2 house=3", "debit=5 credit=7", "balance=5", "stakes=5", // names outside the allow-list
		"amount=1 amount=2", "status=ok amount=1 amount=4999", // duplicate (a provider status injecting amount=)
		"amount=1.5", "amount=abc", "amount=" + strings.Repeat("9", 41), "open_bet_stakes=1 ledger_locked_net=2", // ambiguous
		"provider_reference=x amount=5", "merchant_reference=m amount=5", // only free text precedes
	} {
		if got := MismatchAmount(in); got != nil {
			t.Errorf("MismatchAmount(%q) = %q, want nil", in, *got)
		}
	}
}

func TestMismatchAssetCandidate_RenderedSegmentsOnly(t *testing.T) {
	for _, c := range []struct{ exp, act, want string }{
		{"asset=USD type=player_cash debit=5 credit=7", "asset=USD type=player_cash debit=5 credit=6", "USD"},
		{"platform: attempt=U amount=5 asset=USD provider_reference=R", "line=1 amount=4 asset=usd", "USD"},
		{"open_bet_stakes=500", "ledger_locked_net=300 asset=EUR", "EUR"},
		{"x", "y", "<nil>"},
		{"asset=USD", "asset=EUR", "<nil>"},
		{"asset=USD asset=EUR", "asset=USD", "<nil>"},
		{"asset=bad!", "asset=USD", "<nil>"},
		{"provider_reference=r asset=USD", "none", "<nil>"},
		{"asset=" + strings.Repeat("A", 17), "", "<nil>"},
	} {
		if got := MismatchAssetCandidate(c.exp, c.act); ptr(got) != c.want {
			t.Errorf("asset(%q, %q) = %s, want %s", c.exp, c.act, ptr(got), c.want)
		}
	}
}

func TestIsCanonicalUUID(t *testing.T) {
	id := uuid.NewString()
	if !IsCanonicalUUID(id) || IsCanonicalUUID(strings.ToUpper(id)) || IsCanonicalUUID("{"+id+"}") || IsCanonicalUUID(id+" ") || IsCanonicalUUID("") {
		t.Error("IsCanonicalUUID accepts a non-canonical spelling")
	}
}

// The real payment_statement emitters, fed hostile references: a provider reference with spaces and
// injected platform-looking tokens, a merchant reference likewise, and a statement status that
// injects amount=. Whatever the stored texts look like, the parsers never return the injected
// subject, asset or amount.
func TestPaymentEmitters_HostileReferencesThroughTheParsers(t *testing.T) {
	now := time.Now()
	real, injected, injectedBet := uuid.New(), uuid.New(), uuid.New()
	hostileRef := "REF 1 attempt=" + injected.String() + " bet=" + injectedBet.String() + " asset=XTS amount=1"
	a := &payAttempt{id: real, operation: "deposit", state: "pending", providerRef: hostileRef, merchantRef: "M-1",
		amount: big.NewInt(5000), asset: "USD", sentAt: now}
	m := &payMatcher{tenantID: uuid.New(), provider: "p", cs: now.Add(-time.Hour), ce: now.Add(time.Hour), agedBefore: now.Add(-time.Minute),
		r: &sbRecorder{}, attempts: []*payAttempt{a}, label: "",
		byRef:        map[string]*payAttempt{"deposit\x00" + hostileRef: a},
		bySettlement: map[string]*payAttempt{}, byMerchant: map[string]*payAttempt{"M-1": a},
		matchedBy: map[uuid.UUID]string{}, ledgerByRef: map[string]*payLedgerTx{}, ledgerByID: map[uuid.UUID]*payLedgerTx{},
		k3: &k3Evidence{byRef: map[string][]*persistedLine{}, byMerchant: map[string][]*persistedLine{}, byMatchedRef: map[string][]*persistedLine{},
			yRef: map[uuid.UUID]string{}, m4NotPaid: map[uuid.UUID]*m4Resolution{}, capturedSeen: map[string]bool{}}}
	lines := []payLine{
		// amount differs; the line's status tries to inject a second amount= and an attempt=.
		{lineNo: 1, kind: "deposit", ref: hostileRef, merchant: "M-1 attempt=" + injected.String(), status: "ok attempt=" + injected.String() + " amount=1",
			asset: "USD", amount: big.NewInt(4999)},
		// no platform record: the reference carries injected subjects.
		{lineNo: 2, kind: "deposit", ref: "ghost wallet=" + injected.String() + " attempt=" + injectedBet.String(), status: "ok", asset: "USD", amount: big.NewInt(1)},
	}
	m.matchLines(lines)
	if len(m.r.mismatches) < 2 {
		t.Fatalf("emitters produced %d mismatches, want at least 2: %+v", len(m.r.mismatches), m.r.mismatches)
	}
	sawAmount, sawMissing := false, false
	for _, x := range m.r.mismatches {
		s := MismatchSubject(StreamPaymentStatement, x.MismatchKind, x.ReconciliationKey, x.ExpectedValue)
		if s != nil && s.ID != real.String() {
			t.Errorf("%s: injected subject %v surfaced (key %q)", x.MismatchKind, s, x.ReconciliationKey)
		}
		switch x.MismatchKind {
		case MismatchKindPayAmountMismatch:
			sawAmount = true
			if got := subj(s); got != "payment_attempt:"+real.String() {
				t.Errorf("amount mismatch subject = %q, want the real attempt", got)
			}
			if got := MismatchAmount(x.ExpectedValue); ptr(got) != "5000" {
				t.Errorf("expected amount = %s, want 5000", ptr(got))
			}
			// the line's status injected a second amount= before the real one: ambiguous -> nil
			if got := MismatchAmount(x.ActualValue); got != nil {
				t.Errorf("actual amount = %q, want nil (injected duplicate amount=)", *got)
			}
			if got := MismatchAssetCandidate(x.ExpectedValue, x.ActualValue); ptr(got) != "USD" {
				t.Errorf("asset = %s, want USD (and never the injected XTS)", ptr(got))
			}
		case MismatchKindPayMissingPlatformRecord:
			sawMissing = true
			if s != nil {
				t.Errorf("a no-platform-record mismatch must carry no subject, got %v", s)
			}
		}
	}
	if !sawAmount || !sawMissing {
		t.Fatalf("expected an amount mismatch and a missing-platform-record mismatch; got %+v", m.r.mismatches)
	}
}

// The subject_id filter re-validates its id inside the reader: a non-canonical id is refused before any
// query runs (a nil transaction proves no statement is issued).
func TestListMismatchesForStream_RejectsNonCanonicalSubjectBeforeAnyQuery(t *testing.T) {
	for _, bad := range []string{"x", strings.ToUpper(uuid.NewString()), uuid.NewString() + " ", "attempt=" + uuid.NewString(), "' OR 1=1 --"} {
		_, _, err := ListMismatchesForStream(context.Background(), nil, StaffMismatchFilter{Stream: StreamPaymentStatement, SubjectID: bad}, 10, 0)
		if !errors.Is(err, ErrBadSubjectID) {
			t.Errorf("subject %q: err = %v, want ErrBadSubjectID", bad, err)
		}
	}
}
