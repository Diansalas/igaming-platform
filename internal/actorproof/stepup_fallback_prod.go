//go:build !integration

package actorproof

// testStepUpFallbackOn: in every non-integration build (production included) the
// signer backstop's test fallback is the constant false - there is no variable
// to set (ADR 0119 S4b, security review finding 5). The integration-build
// counterpart is in testsupport_integration.go.
func testStepUpFallbackOn() bool { return false }
