package payments

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// SEC-8 (B1): a recovered adapter panic must map to Ambiguous with
// Attempted=true, and the panic VALUE must never reach GateResult.Err.
func TestCallProvider_PanicValueNeverReachesErr(t *testing.T) {
	const secret = "SENTINEL-s3cr3t-api-key-9f2c"
	cases := []struct {
		name     string
		val      any
		wantType string
	}{
		{"string", "Authorization: Bearer " + secret, "string"},
		{"error", errors.New("vendor said: " + secret), "*errors.errorString"},
		{"url_error", &url.Error{Op: "Post", URL: "https://psp.invalid/c?api_key=" + secret, Err: errors.New(secret)}, "*url.Error"},
		{"struct", struct{ Body string }{secret}, "struct { Body string }"},
		{"fmt_wrapped", fmt.Errorf("wrap: %w", errors.New(secret)), "*fmt.wrapError"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok := uuid.New()
			in := callProviderInput{
				TenantID: uuid.New(), ProviderID: "mock-psp-panic",
				AttemptState: AttemptSubmitting, ClaimToken: tok, ExpectedClaim: tok,
				Domain: "payments", Manifest: OperationManifest{CallTimeout: time.Second},
			}
			gr := callProvider(context.Background(), nil, MockCredentialResolver{}, in,
				func(context.Context, CallContext) (DepositResult, ErrorClass, error) { panic(tc.val) })
			if gr.Class != ErrorClassAmbiguous {
				t.Fatalf("class = %s, want Ambiguous", gr.Class)
			}
			if !gr.Attempted {
				t.Fatal("Attempted must be true for a recovered panic")
			}
			if gr.Err == nil {
				t.Fatal("expected non-nil Err")
			}
			msg := gr.Err.Error()
			if strings.Contains(msg, secret) || strings.Contains(msg, "api_key") || strings.Contains(msg, "psp.invalid") {
				t.Fatalf("panic value leaked into GateResult.Err: %q", msg)
			}
			if !strings.Contains(msg, "adapter panic recovered") || !strings.Contains(msg, "("+tc.wantType+")") {
				t.Fatalf("expected type-only description with %q, got %q", tc.wantType, msg)
			}
		})
	}
}
