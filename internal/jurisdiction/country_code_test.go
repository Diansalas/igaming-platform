// This file implements INV-M-4 (ADR 0045 §1.1): no Go code anywhere may
// select a jurisdictions row BY country_code, join country_code to
// jurisdictions.code, or pass country_code into internal/jurisdiction's
// own player-resolution path. It is a plain unit test (no database
// needed) so it runs on every `go test ./...` invocation, not only the
// integration suite.
package jurisdiction

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestJurisdictionCountryCode_IsNotAJurisdictionResolver statically
// confirms the player-resolution path files carry ZERO reference to the
// jurisdictions.country_code COLUMN (the new administrative field, ADR
// 0045 §1.1) - resolver.go, precedence.go, types.go, evidence.go,
// player_result.go, and every other file in playerResolutionFiles below
// must never mention it.
//
// Deliberately NOT a search for the bare identifier "CountryCode": the
// pre-existing, LEGITIMATE evidence types (VerifiedResidenceEvidence.
// CountryCode, DeclaredResidenceEvidence.CountryCode,
// LocationSignalEvidence.CountryCode) already use that exact Go field
// name for an unrelated concept - a player's declared/verified/observed
// residence country - and precedence.go legitimately reads them via
// (e.g.) `ev.VerifiedResidence.CountryCode`. Searching for that substring
// would flag correct, pre-existing code. The database COLUMN this
// invariant fences is always written/read as the snake_case SQL
// identifier "country_code" (with an underscore) - Go field/method access
// on the evidence types never produces that substring - so searching for
// it precisely targets the one thing INV-M-4 forbids: a
// jurisdictions.country_code SELECT/JOIN reaching this path.
func TestJurisdictionCountryCode_IsNotAJurisdictionResolver(t *testing.T) {
	playerResolutionFiles := []string{
		"resolver.go", "precedence.go", "types.go", "evidence.go",
		"player_result.go", "persist.go", "restriction.go",
		"resolution_active.go", "evidence_collection_active.go",
		"operation_purpose.go", "purpose.go",
		// Added in the Stage 4I Phase E fix round (P3 item): these two
		// files were omitted from the original guarded list even though
		// they are part of the same player-resolution surface family -
		// both are currently clean; this closes a future-regression gap
		// rather than fixing a live defect.
		"evaluation_policy.go", "evaluation_policy_admin.go",
	}
	for _, name := range playerResolutionFiles {
		path := filepath.Join(".", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(data), "country_code") {
			t.Fatalf("%s must never reference the jurisdictions.country_code column (INV-M-4, ADR 0045 §1.1) - the player-resolution path must remain structurally incapable of using it", path)
		}
	}
}
