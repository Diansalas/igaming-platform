// Stage 9.2 (ADR 0083 §12.1 item 16, INV-SB-JUR-1's own regression guard):
// a static, source-level check that jurisdiction.Resolve appears exactly
// ONCE in internal/sportsbook - inside ResolvePlayerJurisdiction
// (jurisdiction.go). PlaceBet (orchestrator.go) and both catalogue
// annotators (catalogue.go) all call ResolvePlayerJurisdiction rather
// than jurisdiction.Resolve directly, so this is the test that keeps this
// ADR from decaying into two divergent implementations of "may this
// player bet on this selection". No build tag - this test needs no
// database and should run under a plain `go test ./...` too, mirroring
// the existing Stage 9.1 static tests
// (TestLockOrder_NoProjectionForUpdateOutsideLedgerPackage-class) this
// ADR's own test plan cites as the style to follow.
package sportsbook

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var jurisdictionResolveCallPattern = regexp.MustCompile(`\bjurisdiction\.Resolve\s*\(`)

// TestSportsbookCatalogue_AndPlaceBetShareOneEvaluation is INV-SB-JUR-1's
// own regression guard: grep every non-test .go file in this package for
// the literal call `jurisdiction.Resolve(` and assert it appears in
// EXACTLY one file, at EXACTLY one call site - jurisdiction.go's own
// ResolvePlayerJurisdiction. Test files are excluded deliberately:
// TestSportsbookJurisdiction_RestrictedSelectionDeniesMatchingJurisdiction
// (jurisdiction_integration_test.go) legitimately calls jurisdiction.Resolve
// directly to obtain a genuinely Resolved value for its own table-driven
// unit test, exactly mirroring internal/casino's identical precedent -
// that is test-fixture setup, not a second PRODUCTION implementation of
// the shared evaluation, and INV-SB-JUR-1 is a production-code invariant.
func TestSportsbookCatalogue_AndPlaceBetShareOneEvaluation(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}

	type match struct {
		file string
		line int
	}
	var matches []match
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		for i, line := range strings.Split(string(content), "\n") {
			if jurisdictionResolveCallPattern.MatchString(line) {
				matches = append(matches, match{file: entry.Name(), line: i + 1})
			}
		}
	}

	if len(matches) != 1 {
		t.Fatalf("expected exactly one jurisdiction.Resolve( call site in internal/sportsbook's production code (INV-SB-JUR-1), found %d: %v", len(matches), matches)
	}
	if matches[0].file != "jurisdiction.go" {
		t.Fatalf("expected the single jurisdiction.Resolve( call site to be in jurisdiction.go (ResolvePlayerJurisdiction), found it in %s:%d instead", matches[0].file, matches[0].line)
	}
}
