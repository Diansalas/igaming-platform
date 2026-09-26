//go:build providerkind_fixture

// Package fixtures holds ScanForUnmarkedMocks' own negative-control
// fixtures (internal/providerkind's own completeness_scan_test.go). It
// lives under a directory literally named "testdata" so the go tool's
// ordinary package discovery (go build/vet/test ./...) never compiles it,
// and carries the providerkind_fixture build tag as a second, explicit,
// belt-and-braces signal that it is fixture source text, never production
// code - the negative-control test still finds and parses it directly via
// go/parser (which does not evaluate build constraints), exactly per the
// W1b test-plan requirement: "via a build-tag-gated fixture type, not by
// editing production code".
package fixtures

// FixtureMarkedMock has the Synthetic marker and must NEVER appear in
// ScanForUnmarkedMocks' findings - proves the scan is not vacuously red
// for everything under this fixture directory.
type FixtureMarkedMock struct{}

func (FixtureMarkedMock) SyntheticComponent() {}
