package payments

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// These pins document and freeze the single-jurisdiction state of payments
// routing (see the "Dimension 2" design note on RoutingRequest). Routing has
// no jurisdiction-dependent policy store, so nothing can be evaluated "as if
// it did not exist"; if someone adds a jurisdiction/country dimension, these
// tests fail and force them to read the contract (fail closed when a
// jurisdiction-dependent policy exists and the jurisdiction is unresolved;
// never reuse AdapterCapability.SupportedCountries as the regulatory gate).

func TestRoutingRequest_HasNoJurisdictionOrCountryDimension(t *testing.T) {
	typ := reflect.TypeOf(RoutingRequest{})
	var got []string
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	sort.Strings(got)
	want := []string{"Amount", "AssetCode", "BrandID", "ExcludeProviderIDs", "Operation", "PaymentMethod", "TenantID"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RoutingRequest fields changed: got %v want %v. If a jurisdiction/country dimension was added, "+
			"implement the fail-closed contract in the RoutingRequest design note (unresolved jurisdiction + "+
			"jurisdiction-dependent policy => route nothing) and update this pin deliberately", got, want)
	}
}

// SupportedCountries must stay a capability fact consumed only by
// capability configuration (narrow-only write check) and declarations, never
// by routing (which would turn an "empty = unrestricted" capability default
// into a regulatory gate's absent-value contract).
func TestPaymentsRouting_NeverReadsSupportedCountries(t *testing.T) {
	allowed := map[string]bool{"capability.go": true, "types.go": true, "mock.go": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || allowed[f] {
			continue
		}
		parsed, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "SupportedCountries" {
				t.Errorf("%s: %s reads SupportedCountries outside capability/types/mock; routing must not use it as a jurisdiction gate (see RoutingRequest design note)",
					f, fset.Position(sel.Pos()))
			}
			return true
		})
	}
}
