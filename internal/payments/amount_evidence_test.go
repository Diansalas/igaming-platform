package payments

import (
	"context"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/providerref"
)

// PRH-2 C unit tests: no database. The helper is shared with PRH-2 D, so its
// table is pinned here independently of any caller.
func TestCompareProviderAmount_Table(t *testing.T) {
	cases := []struct {
		name     string
		expAmt   int64
		expAsset string
		gotAmt   int64
		gotAsset string
		want     AmountEvidence
	}{
		{"exact match", 5000, "EUR", 5000, "EUR", AmountEvidenceMatch},
		{"zero amount is missing", 5000, "EUR", 0, "EUR", AmountEvidenceMissing},
		{"empty asset is missing", 5000, "EUR", 5000, "", AmountEvidenceMissing},
		{"both absent is missing", 5000, "EUR", 0, "", AmountEvidenceMissing},
		{"amount one under", 5000, "EUR", 4999, "EUR", AmountEvidenceMismatch},
		{"amount one over", 5000, "EUR", 5001, "EUR", AmountEvidenceMismatch},
		{"asset differs", 5000, "EUR", 5000, "USD", AmountEvidenceMismatch},
		{"asset case differs (never case-folded)", 5000, "EUR", 5000, "eur", AmountEvidenceMismatch},
		{"negative amount is a mismatch, not missing", 5000, "EUR", -5000, "EUR", AmountEvidenceMismatch},
		{"both differ", 5000, "EUR", 1, "USD", AmountEvidenceMismatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CompareProviderAmount(c.expAmt, c.expAsset, c.gotAmt, c.gotAsset); got != c.want {
				t.Fatalf("CompareProviderAmount(%d,%q,%d,%q) = %s, want %s", c.expAmt, c.expAsset, c.gotAmt, c.gotAsset, got, c.want)
			}
		})
	}
}

// scriptedDeposit is a PaymentProvider whose Deposit returns a fixed result;
// everything else is the embedded MOCK.
type scriptedDeposit struct {
	*MockProvider
	res DepositResult
}

func (p *scriptedDeposit) Deposit(context.Context, DepositRequest) (DepositResult, error) {
	return p.res, nil
}

// TestDepositAdapterCall_ClassifiesReferenceAndAmountEvidence pins the phase-B
// classification (PAY-DEP-REF-VALIDATE-1 / LF-5 sync half) at the unit level.
func TestDepositAdapterCall_ClassifiesReferenceAndAmountEvidence(t *testing.T) {
	attempt := PaymentAttempt{Amount: 5000, AssetCode: "EUR", MerchantReference: "m-1", PaymentMethod: "card"}
	syncOK := OperationManifest{SupportsDeposit: true, SyncSuccessPossible: true}
	syncNo := OperationManifest{SupportsDeposit: true}
	good := strings.Repeat("r", providerref.MaxBytes)
	bad := strings.Repeat("r", providerref.MaxBytes+1)

	cases := []struct {
		name      string
		manifest  OperationManifest
		res       DepositResult
		wantClass ErrorClass
		wantRef   providerref.Reason // non-empty => the error must be a providerref.Error with this reason
	}{
		{"pending valid", syncOK, DepositResult{Outcome: OutcomePending, ProviderReference: "p-1"}, ErrorClassPending, ""},
		{"pending exactly max bytes", syncOK, DepositResult{Outcome: OutcomePending, ProviderReference: good}, ErrorClassPending, ""},
		{"pending empty parks (S-9)", syncOK, DepositResult{Outcome: OutcomePending}, ErrorClassProviderRefInvalid, providerref.ReasonEmpty},
		{"pending oversize", syncOK, DepositResult{Outcome: OutcomePending, ProviderReference: bad}, ErrorClassProviderRefInvalid, providerref.ReasonTooLong},
		{"pending control char", syncOK, DepositResult{Outcome: OutcomePending, ProviderReference: "a\x07b"}, ErrorClassProviderRefInvalid, providerref.ReasonControlChar},
		{"pending invalid utf8", syncOK, DepositResult{Outcome: OutcomePending, ProviderReference: "\xff\xfe"}, ErrorClassProviderRefInvalid, providerref.ReasonInvalidUTF8},
		{"declined oversize parks", syncOK, DepositResult{Outcome: OutcomeDeclined, ProviderReference: bad}, ErrorClassProviderRefInvalid, providerref.ReasonTooLong},
		{"declined empty ref is still a decline", syncOK, DepositResult{Outcome: OutcomeDeclined}, ErrorClassDefiniteDecline, ""},
		{"ambiguous oversize parks", syncOK, DepositResult{Outcome: OutcomeAmbiguous, ProviderReference: bad}, ErrorClassProviderRefInvalid, providerref.ReasonTooLong},
		{"ambiguous empty ref stays ambiguous", syncOK, DepositResult{Outcome: OutcomeAmbiguous}, ErrorClassAmbiguous, ""},
		{"sync success oversize parks despite correct amount", syncOK,
			DepositResult{Outcome: OutcomeSucceeded, ProviderReference: bad, Amount: 5000, AssetCode: "EUR"}, ErrorClassProviderRefInvalid, providerref.ReasonTooLong},
		{"sync success valid + exact echo", syncOK,
			DepositResult{Outcome: OutcomeSucceeded, ProviderReference: "s-1", Amount: 5000, AssetCode: "EUR"}, ErrorClassSucceeded, ""},
		{"sync success empty ref keeps the ambiguous path", syncOK,
			DepositResult{Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR"}, ErrorClassAmbiguous, ""},
		{"sync success without SyncSuccessPossible is ambiguous", syncNo,
			DepositResult{Outcome: OutcomeSucceeded, ProviderReference: "s-2", Amount: 5000, AssetCode: "EUR"}, ErrorClassAmbiguous, ""},
		{"sync success missing amount is ambiguous", syncOK,
			DepositResult{Outcome: OutcomeSucceeded, ProviderReference: "s-3", AssetCode: "EUR"}, ErrorClassAmbiguous, ""},
		{"sync success missing asset is ambiguous", syncOK,
			DepositResult{Outcome: OutcomeSucceeded, ProviderReference: "s-4", Amount: 5000}, ErrorClassAmbiguous, ""},
		{"sync success mismatched echo is classified Succeeded (phase C disputes it, it needs the tx)", syncOK,
			DepositResult{Outcome: OutcomeSucceeded, ProviderReference: "s-5", Amount: 4999, AssetCode: "EUR"}, ErrorClassSucceeded, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &scriptedDeposit{MockProvider: NewMockProvider("mock-unit", "EUR"), res: c.res}
			got, class, err := depositAdapterCall(p, attempt, c.manifest)(context.Background(), CallContext{})
			if class != c.wantClass {
				t.Fatalf("class = %s, want %s (err=%v)", class, c.wantClass, err)
			}
			if c.wantRef != "" {
				perr, ok := providerref.AsError(err)
				if !ok || perr.Reason != c.wantRef {
					t.Fatalf("want a providerref.Error with reason %q, got %v", c.wantRef, err)
				}
				// The parked result is scrubbed: nothing from an unvalidated
				// response may reach persistence or the player.
				if got.ProviderReference != "" || got.RedirectURL != "" || got.HostedFieldToken != "" {
					t.Fatalf("a ProviderRefInvalid result must be scrubbed, got %+v", got)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// The MOCK echoes the request's own amount and asset on every outcome.
func TestMockDeposit_EchoesAmountAndAsset(t *testing.T) {
	m := NewMockProvider("mock-echo", "EUR")
	for _, amt := range []int64{5000, MockAmountPlayerDeclineNoCascade, MockAmountProviderDeclineCascade, MockAmountAmbiguous, MockAmountSyncSuccess} {
		res, err := m.Deposit(context.Background(), DepositRequest{MerchantReference: "m", Amount: amt, AssetCode: "EUR", PaymentMethod: "card"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Amount != amt || res.AssetCode != "EUR" {
			t.Fatalf("amount %d: mock echoed %d %q", amt, res.Amount, res.AssetCode)
		}
	}
	m.AcceptAllAmounts = true
	res, _ := m.Deposit(context.Background(), DepositRequest{MerchantReference: "m", Amount: 222, AssetCode: "EUR", PaymentMethod: "card"})
	if res.Amount != 222 || res.AssetCode != "EUR" {
		t.Fatalf("AcceptAllAmounts: mock echoed %d %q", res.Amount, res.AssetCode)
	}
}
