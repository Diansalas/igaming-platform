//go:build integration

package capability

import "sync/atomic"

// testReassertFallback (integration builds ONLY; the production build has no such variable, reassert_prod.go) lets a
// fixture that calls DecideAndGrant / RevokeGrant directly, with no authenticated ProofActor (the TestSessionProofHook
// path), skip the K1 re-assertion. It never applies when a ProofActor is present (every HTTP K1 call), so the HTTP tests
// exercise the real check. ADR 0119 S5 + S6 review fix F7.
var testReassertFallback atomic.Bool

func init() { testReassertFallback.Store(true) }

// SetTestReassertFallback switches the fallback (integration builds only).
func SetTestReassertFallback(on bool) { testReassertFallback.Store(on) }

func reassertFallbackOn() bool { return testReassertFallback.Load() }
