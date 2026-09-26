// N3 (docs/governance/stage-10.1-code-review.md, "Re-verification
// (2026-09-26, 3f67ac5)"): mapReceiveCallbackError is the single place
// both the public webhook route and the simulate-callback route derive
// their HTTP response from (payment_callback_errors.go's own doc
// comment). This is a plain unit test (no DB, no integration tag) of that
// pure function, covering both callbackRouteKind values for
// ErrCallbackMalformedBody and ErrCallbackProviderMismatch - the two
// sentinels the code review flagged as untested for the public route, and
// which the plan separately calls out for "simulate route mapping ... per
// internal/httpserver/payment_callback_errors.go". The simulate route
// itself can never actually MANUFACTURE either sentinel via a real HTTP
// request (newSimulateDepositCallbackHandler always signs the intent's
// OWN amount/asset and always sends a well-formed body - see that
// handler's own doc comment), so the mapping is proven directly against
// the shared function rather than through an unreachable HTTP path.
package httpserver

import (
	"fmt"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/payments"
)

func TestMapReceiveCallbackError_PostVerificationMappings(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		kind        callbackRouteKind
		wantCode    apierror.Code
		wantMessage string
	}{
		{
			name:        "malformed_body/public_webhook",
			err:         fmt.Errorf("%w: parse callback: unexpected EOF", payments.ErrCallbackMalformedBody),
			kind:        callbackRoutePublicWebhook,
			wantCode:    apierror.CodeValidation,
			wantMessage: "callback rejected",
		},
		{
			name:        "malformed_body/simulate",
			err:         fmt.Errorf("%w: parse callback: unexpected EOF", payments.ErrCallbackMalformedBody),
			kind:        callbackRouteSimulate,
			wantCode:    apierror.CodeInternal,
			wantMessage: "failed to simulate deposit callback",
		},
		{
			name:        "provider_mismatch/public_webhook",
			err:         fmt.Errorf("%w: intent expected 5000 EUR, provider confirmed 9999 EUR", payments.ErrCallbackProviderMismatch),
			kind:        callbackRoutePublicWebhook,
			wantCode:    apierror.CodeValidation,
			wantMessage: "callback rejected",
		},
		{
			name:        "provider_mismatch/simulate",
			err:         fmt.Errorf("%w: intent expected 5000 EUR, provider confirmed 9999 EUR", payments.ErrCallbackProviderMismatch),
			kind:        callbackRouteSimulate,
			wantCode:    apierror.CodeConflict,
			wantMessage: "deposit could not be settled",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, msg := mapReceiveCallbackError(tc.err, tc.kind)
			if code != tc.wantCode || msg != tc.wantMessage {
				t.Fatalf("mapReceiveCallbackError(%v, %v) = (%q, %q), want (%q, %q)", tc.err, tc.kind, code, msg, tc.wantCode, tc.wantMessage)
			}
			// The mapped message must never echo the wrapped error's own
			// text (which, for ErrCallbackProviderMismatch, embeds the
			// mismatched amounts - S-5/security review) - only the fixed,
			// generic strings above.
			if msg == tc.err.Error() {
				t.Fatalf("mapped message must not equal the raw error text")
			}
		})
	}
}
