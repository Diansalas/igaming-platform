//go:build !integration

package payments

// submitReassertionFallbackOn is false in every production binary: a SubmitActor without a verified role is refused
// by the submit re-assertion (ADR 0119 S5 + S6). The integration-test build (submit_reassert_integration.go) lets a
// domain test that calls ClaimForDispatch directly, with no token, leave Role empty. TestStatic_SubmitReassertionPins
// pins this file.
func submitReassertionFallbackOn() bool { return false }
