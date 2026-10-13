//go:build integration

package prooftest

import "github.com/Diansalas/igaming-platform/internal/actorproof"

// ADR 0119 S4b (section 9.1 "Test support"): in every integration test process
// that imports prooftest, a governed domain service called directly by a test
// (no HTTP chain) signs as if the HTTP step-up gate had run. An HTTP staff
// request is marked by auth.Middleware and always meets the real signer
// backstop (actorproof.SetTestStepUpFallback, integration builds only).
func init() { actorproof.SetTestStepUpFallback(true) }
