// Plain unit tests (no database, no build tag) for the structural
// invariants that don't need PostgreSQL: the import-graph boundary
// (INV-M-1), the write-API shape (no ceiling/licence field is acceptable
// as caller input), the Result oracle discipline (no blocking/source
// accessor, no jurisdiction accessor), and the AsOf-determinism source
// check (INV-M-3: no time.Now()/now() on the resolve path).
package operatingmarket

import (
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// TestPolicyVersion_IsPinnedToV3 guards PolicyVersion's literal value
// against silent drift - nothing else in this package fails a build or a
// test if this constant's value changes, so this is the sole regression
// guard. Currently "stage-4i-e.v3" (ADR 0045 §18, finding F4 ONLY -
// AMENDMENT-3 itself does not bump this constant, see types.go's own
// comment).
func TestPolicyVersion_IsPinnedToV3(t *testing.T) {
	if PolicyVersion != "stage-4i-e.v3" {
		t.Fatalf("expected PolicyVersion to be pinned to %q, got %q", "stage-4i-e.v3", PolicyVersion)
	}
}

// TestOperatingMarket_ImportGraphInvariant mechanically enforces INV-M-1
// via `go list -deps`: internal/operatingmarket may depend on
// internal/jurisdiction, internal/validation, internal/audit (plus
// standard library and third-party packages) but must NEVER depend on
// internal/identity, internal/kyc, internal/geolocation, or internal/rg.
func TestOperatingMarket_ImportGraphInvariant(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps .: %v\n%s", err, out)
	}
	deps := strings.Fields(string(out))

	forbidden := []string{
		"github.com/Diansalas/igaming-platform/internal/identity",
		"github.com/Diansalas/igaming-platform/internal/kyc",
		"github.com/Diansalas/igaming-platform/internal/geolocation",
		"github.com/Diansalas/igaming-platform/internal/rg",
	}
	for _, dep := range deps {
		for _, f := range forbidden {
			if dep == f {
				t.Fatalf("internal/operatingmarket must NEVER depend on %s (INV-M-1); go list -deps reports it does", f)
			}
		}
	}

	// jurisdiction must never depend on operatingmarket either (no cycle,
	// now or later).
	out2, err := exec.Command("go", "list", "-deps", "github.com/Diansalas/igaming-platform/internal/jurisdiction").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps internal/jurisdiction: %v\n%s", err, out2)
	}
	for _, dep := range strings.Fields(string(out2)) {
		if dep == "github.com/Diansalas/igaming-platform/internal/operatingmarket" {
			t.Fatal("internal/jurisdiction must NEVER depend on internal/operatingmarket - a cycle would defeat the entire package-separation ruling")
		}
	}
}

// TestOperatingCountryPolicy_NoWriteAPIAcceptsACeilingOrLicenceID asserts,
// by reflection over the exported write-parameter structs, that no field
// anywhere in the package's write API accepts a ceiling value or a raw
// licence id from a tenant-scoped caller - CreateOperatingCountryPolicyVersion's
// own params must have NO field that could carry the ceiling's own
// judgment (only CreateLicenceCountryCeilingVersionParams, itself
// PLATFORM-scoped, carries LicenceID at all).
func TestOperatingCountryPolicy_NoWriteAPIAcceptsACeilingOrLicenceID(t *testing.T) {
	typ := reflect.TypeOf(CreateOperatingCountryPolicyVersionParams{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		lower := strings.ToLower(name)
		if strings.Contains(lower, "licence") || strings.Contains(lower, "license") || strings.Contains(lower, "ceiling") {
			t.Fatalf("CreateOperatingCountryPolicyVersionParams must have NO field naming a licence or ceiling (found %q) - the ceiling is read from the database on every write, never accepted from a caller", name)
		}
	}
}

// TestResult_ExposesNoBlockingScopeAccessor confirms, by reflection over
// Result's exported method set, that only the documented eight accessors
// plus AssertScope exist - in particular, NO method exposes
// blockingScope/blockingVersionID/sourceScope/licenceID/
// licenceCeilingReason, and NO Code()/ID()/jurisdiction accessor exists.
func TestResult_ExposesNoBlockingScopeAccessor(t *testing.T) {
	typ := reflect.TypeOf(Result{})
	allowed := map[string]bool{
		"Outcome": true, "Permitted": true, "AsOf": true, "PolicyVersion": true,
		"CountryCode": true, "OperationCode": true, "TenantID": true, "BrandID": true,
		"AssertScope": true,
	}
	if typ.NumMethod() != len(allowed) {
		t.Fatalf("Result exposes %d exported methods, expected exactly %d (the oracle rule, ADR 0045 Section 4, requires EXACTLY the eight accessors plus AssertScope)", typ.NumMethod(), len(allowed))
	}
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		if !allowed[name] {
			t.Fatalf("Result exposes an unexpected exported method %q - nothing that could leak blocking/source/licence provenance is permitted", name)
		}
	}
}

// TestOperatingMarket_NeverProducesAJurisdictionResolution confirms Result
// cannot be mistaken for, or substituted for, a jurisdiction.Resolution or
// PlayerJurisdictionResult: it has no Code()/ID() method and no field of
// any jurisdiction type. Reflection over both the method set and the
// (unexported, but reflectable) struct field types.
func TestOperatingMarket_NeverProducesAJurisdictionResolution(t *testing.T) {
	typ := reflect.TypeOf(Result{})
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		if name == "Code" || name == "ID" {
			t.Fatalf("Result must not expose a %s() method - that is jurisdiction.Resolution's own exclusive accessor shape, and Result must never be substitutable for it", name)
		}
	}
	for i := 0; i < typ.NumField(); i++ {
		fieldType := typ.Field(i).Type.String()
		if strings.Contains(fieldType, "jurisdiction.") {
			t.Fatalf("Result must carry NO field of any internal/jurisdiction type (found field %q of type %q)", typ.Field(i).Name, fieldType)
		}
	}
}

// TestResolveOperatingCountryPolicy_SourceHasNoTimeNow is the source-level
// half of INV-M-3: resolve()'s own source file must never call
// time.Now(), and the SQL string literals in that file must never contain
// now()/clock_timestamp()/CURRENT_TIMESTAMP.
func TestResolveOperatingCountryPolicy_SourceHasNoTimeNow(t *testing.T) {
	data, err := os.ReadFile("resolve.go")
	if err != nil {
		t.Fatalf("read resolve.go: %v", err)
	}
	// Strip full-line "//" comments (this file's own doc comments
	// legitimately DISCUSS time.Now()/now() in prose, e.g. "resolve()
	// NEVER calls time.Now()") - only actual code/SQL-string content is
	// checked below.
	var codeOnly strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		codeOnly.WriteString(line)
		codeOnly.WriteString("\n")
	}
	content := codeOnly.String()
	if strings.Contains(content, "time.Now()") {
		t.Fatal("resolve.go must never call time.Now() in actual code - AsOf is a parameter, and resolve() must be reproducible for a given AsOf (INV-M-3)")
	}
	lower := strings.ToLower(content)
	for _, forbidden := range []string{"now()", "clock_timestamp()", "current_timestamp"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("resolve.go must never issue SQL containing %q on the resolve path (INV-M-3)", forbidden)
		}
	}
}
