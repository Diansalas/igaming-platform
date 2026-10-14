//go:build integration

package actorproof

import "sync/atomic"

// testStepUpFallback is the integration-build switch behind testStepUpFallbackOn
// (stepup.go). Production builds compile stepup_fallback_prod.go instead, where
// the answer is the constant false and this variable does not exist.
var testStepUpFallback atomic.Bool

func testStepUpFallbackOn() bool { return testStepUpFallback.Load() }

// SetTestStepUpFallback switches the signer backstop's integration-test
// fallback (see testStepUpFallbackOn in stepup.go). INTEGRATION BUILDS ONLY: this
// file carries the integration build tag, so no production binary contains the
// setter. internal/actorproof/prooftest.Install turns it on for integration
// test processes, so a governed domain service called directly by a test (no
// HTTP chain) signs as if the HTTP step-up gate had run; an HTTP staff request
// (marked by auth.Middleware) always meets the real backstop.
func SetTestStepUpFallback(on bool) { testStepUpFallback.Store(on) }
