// Stage 9.2 (ADR 0083 §12.1 items 5, 7-10): rung 2
// (evaluateOperatingMarket, §5.3.3) is SPECIFIED, NOT IMPLEMENTED -
// BLOCKED on HDR-J-7. Per the ADR's own instruction, these tests are
// written against the documented contract rather than silently omitted,
// and are marked BLOCKED via t.Skip so the suite records the gap instead
// of hiding it. No build tag - these are pure Go.Skip stubs, they touch
// no database and must compile/run (and skip) under a plain `go test
// ./...` too.
package sportsbook

import "testing"

// TestSportsbookJurisdiction_ExpiredEvidenceFailsClosed (item 5) - the
// fail-closed posture for stale/expired jurisdiction evidence is reachable
// only through jurisdiction.DeterminePlayerJurisdiction's
// MaxLocationSignalAge path, which has no production caller anywhere in
// this codebase (ADR 0083 §5.3.1's own explicit "DeterminePlayerJurisdiction
// is deliberately NOT called" ruling).
func TestSportsbookJurisdiction_ExpiredEvidenceFailsClosed(t *testing.T) {
	t.Skip("BLOCKED on HDR-J-7 — see ADR 0083 §5.3.3")
}

// TestSportsbookOperatingMarket_LicenceCeilingDenialBlocksPlacement
// (item 7) - rung 2's licence-ceiling denial path.
func TestSportsbookOperatingMarket_LicenceCeilingDenialBlocksPlacement(t *testing.T) {
	t.Skip("BLOCKED on HDR-J-7 — see ADR 0083 §5.3.3")
}

// TestSportsbookOperatingMarket_TenantScopeDisableBlocksPlacement (item 8)
// - rung 2's tenant-scope disable path.
func TestSportsbookOperatingMarket_TenantScopeDisableBlocksPlacement(t *testing.T) {
	t.Skip("BLOCKED on HDR-J-7 — see ADR 0083 §5.3.3")
}

// TestSportsbookOperatingMarket_BrandScopeDisableBlocksPlacement (item 9)
// - rung 2's brand-scope disable path.
func TestSportsbookOperatingMarket_BrandScopeDisableBlocksPlacement(t *testing.T) {
	t.Skip("BLOCKED on HDR-J-7 — see ADR 0083 §5.3.3")
}

// TestSportsbookOperatingMarket_OperationScopeDisableBlocksPlacement
// (item 10) - rung 2's wagering/sportsbook operation+product scope
// disable path, and the invariant that a tenant/brand row can never widen
// past a denial above it (INV-SB-JUR-5).
func TestSportsbookOperatingMarket_OperationScopeDisableBlocksPlacement(t *testing.T) {
	t.Skip("BLOCKED on HDR-J-7 — see ADR 0083 §5.3.3")
}
