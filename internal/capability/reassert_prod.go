//go:build !integration

package capability

// reassertFallbackOn is false in every production binary: a K1 approval or revocation without an authenticated
// ProofActor in the context is refused by the re-assertion (ErrActorNotLive) instead of being skipped (ADR 0119 S5 + S6
// review fix F7; the same split as the withdrawal-submit re-assertion, payments/submit_reassert_prod.go). The
// integration-test build (reassert_integration.go) lets fixtures that open their own sessions without a token skip it.
// TestStatic_K1ReassertionPins pins this file.
func reassertFallbackOn() bool { return false }
