// Stage 10.3 W1b, MOCK-ADAPTER-PROD-1 coverage requirement (security
// condition C13 point 3 / ADR 0085's Stage 10.3 amendment §6): "cmd/seed-
// admin and cmd/migrate wire no mock or provider component, and a test
// keeps it that way." Both binaries connect to the database directly and
// never construct a provider adapter, scanner, storage, resolver, or
// statement-source component, so RefuseSyntheticInProduction has nothing
// to check there - this test proves that structurally (by import list),
// the same convention this package's own
// kyc_prefix_e2_const_secret_test.go already uses for a different
// structural guarantee, so a future change that starts importing
// internal/payments, internal/casino, internal/kyc, internal/sportsbook,
// internal/email, internal/identityresolution, internal/geolocation, or
// internal/providerkind into either binary is caught immediately, before
// anyone has to notice its absence from the synthetic guard.
package main

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// forbiddenProviderImports are the packages that hold every provider/
// scanner/storage/resolver/statement-source mock in this codebase (see
// internal/providerkind's own doc comment for the full inventory).
// cmd/seed-admin and cmd/migrate must import none of them.
var forbiddenProviderImports = []string{
	"github.com/Diansalas/igaming-platform/internal/payments",
	"github.com/Diansalas/igaming-platform/internal/casino",
	"github.com/Diansalas/igaming-platform/internal/kyc",
	"github.com/Diansalas/igaming-platform/internal/sportsbook",
	"github.com/Diansalas/igaming-platform/internal/email",
	"github.com/Diansalas/igaming-platform/internal/identityresolution",
	"github.com/Diansalas/igaming-platform/internal/geolocation",
	"github.com/Diansalas/igaming-platform/internal/providerkind",
	"github.com/Diansalas/igaming-platform/internal/webhookauth",
}

func assertNoProviderImports(t *testing.T, mainGoPath string) {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, mainGoPath, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", mainGoPath, err)
	}

	forbidden := make(map[string]bool, len(forbiddenProviderImports))
	for _, p := range forbiddenProviderImports {
		forbidden[p] = true
	}

	for _, imp := range file.Imports {
		importPath := imp.Path.Value
		// imp.Path.Value includes the surrounding quotes.
		unquoted := importPath[1 : len(importPath)-1]
		if forbidden[unquoted] {
			t.Errorf("%s imports %q, a provider/mock package - MOCK-ADAPTER-PROD-1 requires this binary to wire no mock or provider component", mainGoPath, unquoted)
		}
	}
}

func TestSeedAdmin_WiresNoProviderComponent(t *testing.T) {
	assertNoProviderImports(t, filepath.Join("..", "seed-admin", "main.go"))
}

func TestMigrate_WiresNoProviderComponent(t *testing.T) {
	assertNoProviderImports(t, filepath.Join("..", "migrate", "main.go"))
}
