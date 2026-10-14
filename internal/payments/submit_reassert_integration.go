//go:build integration

package payments

import "sync/atomic"

// testSubmitReassertionFallback (integration builds ONLY; the production build has no such variable,
// submit_reassert_prod.go) lets a domain test that calls ClaimForDispatch directly, without a verified token, leave
// SubmitActor.Role empty: the re-assertion is then skipped. It never applies to a SubmitActor that carries a Role
// (every HTTP submit does), so the HTTP tests and the dedicated re-assertion tests exercise the real check. ADR 0119
// S5 + S6, the same split as the actor-proof signer backstop's integration-only fallback (ADR 0119 S4b D10).
var testSubmitReassertionFallback atomic.Bool

func init() { testSubmitReassertionFallback.Store(true) }

// SetTestSubmitReassertionFallback switches the fallback (integration builds only). A test that wants the strict
// production behaviour for an empty Role turns it off and restores it.
func SetTestSubmitReassertionFallback(on bool) { testSubmitReassertionFallback.Store(on) }

func submitReassertionFallbackOn() bool { return testSubmitReassertionFallback.Load() }
