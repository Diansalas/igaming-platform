package payments

import (
	"context"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/providerref"
)

// PAY-FPAY-HARDENING-1 F-L2: a KYC outage is not a KYC requirement.
func TestDepositKYCDeclineReason(t *testing.T) {
	cases := []struct{ deny, want string }{
		{"kyc_unavailable:verification_lookup_failed", "kyc_unavailable:verification_lookup_failed"},
		{"kyc_unavailable:", "kyc_unavailable:"},
		{"kyc_test_deny", "kyc_required:kyc_test_deny"},
		{"kyc_required_by_policy", "kyc_required:kyc_required_by_policy"},
		{"", "kyc_required:"},
		// The prefix must be at the START: a denial that merely mentions it is a deny.
		{"x_kyc_unavailable:y", "kyc_required:x_kyc_unavailable:y"},
	}
	for _, tc := range cases {
		if got := depositKYCDeclineReason(tc.deny); got != tc.want {
			t.Errorf("depositKYCDeclineReason(%q) = %q, want %q", tc.deny, got, tc.want)
		}
	}
}

// hostileStatusProvider returns a fully populated status with a hostile reference.
type hostileStatusProvider struct {
	*MockProvider
	res StatusResult
}

func (p hostileStatusProvider) QueryStatus(context.Context, string) (StatusResult, error) {
	return p.res, nil
}

// F-L3: the invalid-reference branch of payoutStatusQuery returns only the outcome.
func TestPayoutStatusQuery_InvalidReferenceIsScrubbed(t *testing.T) {
	hostile := "bad\x00ref-" + strings.Repeat("z", 8)
	p := hostileStatusProvider{MockProvider: NewMockProvider("m", "EUR"), res: StatusResult{
		ProviderReference: hostile, Outcome: OutcomeSucceeded, Amount: 777, AssetCode: "EUR",
		DeclineReason: "raw-vendor-text", Cascadable: true,
	}}
	res, class, err := payoutStatusQuery(p, "ref")(context.Background(), CallContext{})
	if class != ErrorClassProviderRefInvalid {
		t.Fatalf("class = %s, want ProviderRefInvalid", class)
	}
	if _, ok := providerref.AsError(err); !ok {
		t.Fatalf("expected a *providerref.Error, got %v", err)
	}
	if res != (StatusResult{Outcome: OutcomeSucceeded}) {
		t.Fatalf("the invalid branch must return only the outcome, got %+v", res)
	}
	if strings.Contains(err.Error(), "ref-zzzz") {
		t.Fatalf("raw value leaked into the error text: %v", err)
	}
}

// A valid reference is not scrubbed (the scrub is only for the invalid branch).
func TestPayoutStatusQuery_ValidReferenceKept(t *testing.T) {
	p := hostileStatusProvider{MockProvider: NewMockProvider("m", "EUR"), res: StatusResult{ProviderReference: "psp-ok-1", Outcome: OutcomePending, Amount: 5, AssetCode: "EUR"}}
	res, class, err := payoutStatusQuery(p, "ref")(context.Background(), CallContext{})
	if err != nil || class != ErrorClassPending || res.ProviderReference != "psp-ok-1" || res.Amount != 5 {
		t.Fatalf("valid result altered: res=%+v class=%s err=%v", res, class, err)
	}
}
