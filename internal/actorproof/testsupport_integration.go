//go:build integration

package actorproof

// SetTestStepUpFallback switches the signer backstop's integration-test
// fallback (see testStepUpFallback in stepup.go). INTEGRATION BUILDS ONLY: this
// file carries the integration build tag, so no production binary contains the
// setter. internal/actorproof/prooftest.Install turns it on for integration
// test processes, so a governed domain service called directly by a test (no
// HTTP chain) signs as if the HTTP step-up gate had run; an HTTP staff request
// (marked by auth.Middleware) always meets the real backstop.
func SetTestStepUpFallback(on bool) { testStepUpFallback.Store(on) }
