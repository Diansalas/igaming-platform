package payments

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ADR 0101 C-9b (static pin): every payments ingress validates a provider-
// supplied reference with the payments validators (which add the reserved-
// namespace refusal); casino and sportsbook keep the shared validators and never
// call the payments ones (their postings are covered by the all-sessions ledger
// trigger instead, O-1).

type refCall struct {
	file, fn, field string
}

// providerrefCalls returns every call of a providerref.<name> function in the
// non-test files under dir (relative to the payments package directory), with the
// first string-literal argument (the field name) when there is one.
func providerrefCalls(t *testing.T, dir string) []refCall {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	var out []refCall
	for _, pkg := range pkgs {
		for fname, f := range pkg.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok || id.Name != "providerref" || !strings.HasPrefix(sel.Sel.Name, "Validate") {
					return true
				}
				rc := refCall{file: filepath.Base(fname), fn: sel.Sel.Name}
				if len(call.Args) > 0 {
					if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						rc.field, _ = strconv.Unquote(lit.Value)
					}
				}
				out = append(out, rc)
				return true
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].file+out[i].fn+out[i].field < out[j].file+out[j].fn+out[j].field })
	return out
}

func TestK3_C9b_PaymentsIngressUsesThePaymentsValidatorsOnly(t *testing.T) {
	calls := providerrefCalls(t, ".")
	payments := 0
	for _, c := range calls {
		if !strings.HasPrefix(c.fn, "ValidatePayment") {
			t.Errorf("payments/%s calls the shared providerref.%s(%q): a payments ingress must use ValidatePayment*", c.file, c.fn, c.field)
			continue
		}
		payments++
	}
	if payments < 10 {
		t.Fatalf("only %d payments validator calls found; the scan is not seeing the code", payments)
	}
	// The statement fetch is a payments ingress too: only the provider id and the
	// platform-generated merchant reference may use the shared validators there.
	for _, c := range providerrefCalls(t, filepath.Join("..", "reconciliation")) {
		if c.file != "payment_statement.go" {
			continue
		}
		if !strings.HasPrefix(c.fn, "ValidatePayment") && c.field != "provider_id" && c.field != "merchant_reference" {
			t.Errorf("reconciliation/%s: shared providerref.%s(%q) on a provider-supplied reference", c.file, c.fn, c.field)
		}
	}
}

func TestK3_C9b_CasinoAndSportsbookKeepTheSharedValidator(t *testing.T) {
	for _, dir := range []string{"casino", "sportsbook"} {
		calls := providerrefCalls(t, filepath.Join("..", dir))
		if len(calls) == 0 {
			t.Fatalf("%s: no providerref calls found; the pin is vacuous", dir)
		}
		for _, c := range calls {
			if strings.HasPrefix(c.fn, "ValidatePayment") {
				t.Errorf("%s/%s calls providerref.%s: casino and sportsbook must keep the shared validator", dir, c.file, c.fn)
			}
		}
	}
}
