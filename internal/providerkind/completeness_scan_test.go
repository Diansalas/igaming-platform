package providerkind

import (
	"path/filepath"
	"testing"
)

// repoInternalDir locates this repository's "internal" directory relative
// to this test file's own source location, so the test works regardless of
// the working directory `go test` is invoked from.
func repoInternalDir(t *testing.T) string {
	t.Helper()
	// This file lives at <repo>/internal/providerkind/completeness_scan_test.go.
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve absolute path: %v", err)
	}
	return filepath.Dir(dir) // .../internal
}

// TestSyntheticGuard_ASTCompletenessScan is the required W1b case: every
// type across the real internal/** tree whose name matches the mock-name
// heuristic must carry SyntheticComponent(). This is a repo-wide,
// non-vacuous assertion - it fails loudly (naming every offender) if a
// future mock is added without the marker.
func TestSyntheticGuard_ASTCompletenessScan(t *testing.T) {
	root := repoInternalDir(t)
	findings, err := ScanForUnmarkedMocks(root)
	if err != nil {
		t.Fatalf("ScanForUnmarkedMocks(%s): %v", root, err)
	}
	if len(findings) > 0 {
		for _, f := range findings {
			t.Errorf("unmarked mock-like type: %s", f)
		}
	}
}

// TestSyntheticGuard_ASTCompletenessScan_GoesRedIfMarkerRemoved is the
// required negative control (W1b test plan): a build-tag-gated fixture
// type with no marker must be reported, and its marked sibling must not -
// proving the scan is a real, runnable check and not merely a described
// property.
func TestSyntheticGuard_ASTCompletenessScan_GoesRedIfMarkerRemoved(t *testing.T) {
	root := filepath.Join("testdata", "fixtures")
	findings, err := ScanForUnmarkedMocks(root)
	if err != nil {
		t.Fatalf("ScanForUnmarkedMocks(%s): %v", root, err)
	}

	var sawUnmarked, sawMarked bool
	for _, f := range findings {
		switch f.Type {
		case "FixtureUnmarkedMock":
			sawUnmarked = true
		case "FixtureMarkedMock":
			sawMarked = true
		}
	}
	if !sawUnmarked {
		t.Error("expected FixtureUnmarkedMock to be reported as unmarked, it was not")
	}
	if sawMarked {
		t.Error("expected FixtureMarkedMock (has the marker) NOT to be reported, but it was - scan is vacuously red")
	}
}

// TestSyntheticGuard_ScanSkipsTestdataInRepoWideWalk proves the repo-wide
// scan (as used by TestSyntheticGuard_ASTCompletenessScan above) does not
// itself trip over this package's own negative-control fixtures - i.e.
// that testdata-skipping actually works, not merely that the fixtures
// directory happens to also be excluded by go build's own tooling.
func TestSyntheticGuard_ScanSkipsTestdataInRepoWideWalk(t *testing.T) {
	findings, err := ScanForUnmarkedMocks(".")
	if err != nil {
		t.Fatalf("ScanForUnmarkedMocks(.): %v", err)
	}
	for _, f := range findings {
		if f.Type == "FixtureUnmarkedMock" {
			t.Fatalf("repo-wide scan from providerkind's own directory must skip testdata/, but found: %s", f)
		}
	}
}
