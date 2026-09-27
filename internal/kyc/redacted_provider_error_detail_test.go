// N-2/N3 (RV-PRH-I2 KYC security re-verification, code re-review): before
// this test, changing RedactedProviderErrorDetail's default branch back to
// `return err.Error()` survived the entire `internal/kyc` and
// `internal/httpserver` suites - the function was implemented but had no
// test of its own. This closes that gap directly (no database needed).
package kyc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const redactedErrorDetailSentinel = "SUPER-SECRET-VENDOR-RESPONSE-BODY-abc123"

func TestRedactedProviderErrorDetail_Nil(t *testing.T) {
	if got := RedactedProviderErrorDetail(nil); got != "" {
		t.Fatalf("expected empty string for a nil error, got %q", got)
	}
}

func TestRedactedProviderErrorDetail_ClosedClasses(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"deadline exceeded, bare", context.DeadlineExceeded, "timeout"},
		{"deadline exceeded, wrapped", fmt.Errorf("call failed: %w", context.DeadlineExceeded), "timeout"},
		{"canceled, bare", context.Canceled, "canceled"},
		{"canceled, wrapped", fmt.Errorf("call failed: %w", context.Canceled), "canceled"},
		{"ErrProviderUnavailable, bare", ErrProviderUnavailable, "provider unavailable"},
		{
			"ErrProviderUnavailable, wrapped with raw adapter text",
			fmt.Errorf("%w: create verification with provider: %v", ErrProviderUnavailable, errors.New(redactedErrorDetailSentinel)),
			"provider unavailable",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := RedactedProviderErrorDetail(c.err)
			if got != c.want {
				t.Fatalf("RedactedProviderErrorDetail(%v) = %q, want %q", c.err, got, c.want)
			}
			if strings.Contains(got, redactedErrorDetailSentinel) {
				t.Fatalf("output leaked the sentinel: %q", got)
			}
		})
	}
}

// TestRedactedProviderErrorDetail_UnclassifiedErrorNeverEchoesRawText is the
// required mutant-killing test (RV-PRH-I2 KYC security re-verification
// N-2's own C5-raw mutant: the default branch reverted to `return
// err.Error()`): an arbitrary, unclassified error carrying a sentinel
// string (standing in for a real adapter's transport error embedding a
// vendor response body, header, or a credential-bearing URL) must NEVER
// have that text appear anywhere in the output, and the output itself must
// stay within a small, fixed bound.
func TestRedactedProviderErrorDetail_UnclassifiedErrorNeverEchoesRawText(t *testing.T) {
	rawErr := fmt.Errorf("POST https://vendor.example/verify?api_key=%s: unexpected response body %q",
		redactedErrorDetailSentinel, strings.Repeat("X", 4096))

	got := RedactedProviderErrorDetail(rawErr)
	if strings.Contains(got, redactedErrorDetailSentinel) {
		t.Fatalf("RedactedProviderErrorDetail leaked the sentinel: %q", got)
	}
	if strings.Contains(got, "vendor.example") {
		t.Fatalf("RedactedProviderErrorDetail leaked the raw URL: %q", got)
	}
	if len(got) > 64 {
		t.Fatalf("expected a short, bounded classification, got %d bytes: %q", len(got), got)
	}
	if got != "internal error (redacted)" {
		t.Fatalf("expected the fixed unclassified-error label, got %q", got)
	}
}
