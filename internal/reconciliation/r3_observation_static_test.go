package reconciliation

// PRH-2 R3 / H-W1: the structural half of the guarantee that the reconciliation
// path - including the non-active-tenant observation sweep - is read plus
// findings/alerts only. No database is needed; these run in every `go test`.
//
//   - Imports: this package imports no payment orchestrator, adapter, wallet,
//     withdrawal or staff-resolution package, so it CANNOT dispatch, query a
//     provider, move an attempt, or create/execute a manual resolution. The
//     only provider-facing surface is statement.PaymentStatementSource.Fetch.
//   - Ledger: of package ledger it uses only read functions and constants; it
//     never calls ledger.Post.
//   - SQL: the only tables it writes are its own findings/run/import stores.
//   - The observation entry point calls nothing but the payment_statement
//     stream and the non-active tenant list.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func r3ProductionFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	fset := token.NewFileSet()
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*ast.File{}
	for _, m := range matches {
		if strings.HasSuffix(m, "_test.go") {
			continue
		}
		src, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, m, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", m, err)
		}
		out[m] = f
	}
	if len(out) == 0 {
		t.Fatal("no production files found")
	}
	return out
}

func TestR3_Static_ReconciliationImportsNoProviderOrMoneyMovingPackage(t *testing.T) {
	allowed := map[string]bool{
		"github.com/Diansalas/igaming-platform/internal/alerting":                 true,
		"github.com/Diansalas/igaming-platform/internal/audit":                    true,
		"github.com/Diansalas/igaming-platform/internal/db":                       true,
		"github.com/Diansalas/igaming-platform/internal/ledger":                   true, // reads only, see the next test
		"github.com/Diansalas/igaming-platform/internal/providerref":              true,
		"github.com/Diansalas/igaming-platform/internal/reconciliation/statement": true,
		"github.com/Diansalas/igaming-platform/internal/txscope":                  true,
	}
	for name, f := range r3ProductionFiles(t) {
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if strings.HasPrefix(p, "github.com/Diansalas/igaming-platform/") && !allowed[p] {
				t.Errorf("%s imports %s: the reconciliation package may not depend on it (R3 read-only guarantee; add it only with ledger-finance and security review)", name, p)
			}
		}
	}
}

func TestR3_Static_LedgerUsedReadOnly(t *testing.T) {
	allowed := map[string]bool{"GetProjectedBalance": true, "RebuildBalance": true, "Credit": true, "Debit": true}
	for name, f := range r3ProductionFiles(t) {
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || id.Name != "ledger" {
				return true
			}
			sym := sel.Sel.Name
			if allowed[sym] || strings.HasPrefix(sym, "Tx") || strings.HasPrefix(sym, "Account") {
				return true
			}
			t.Errorf("%s uses ledger.%s: only read functions and constants are allowed here (no ledger.Post, no account creation)", name, sym)
			return true
		})
	}
}

var r3WriteRE = regexp.MustCompile(`(?is)\b(insert\s+into|update|delete\s+from)\s+([a-z_][a-z0-9_]*)`)

func TestR3_Static_OnlyOwnStoresAreWritten(t *testing.T) {
	insertable := map[string]bool{
		"reconciliation_runs": true, "reconciliation_mismatches": true,
		"payment_statement_imports": true, "payment_statement_lines": true,
	}
	updatable := map[string]bool{"reconciliation_mismatches": true} // ResolveMismatch only (not on any sweep path)
	seenInsert := map[string]bool{}
	for name, f := range r3ProductionFiles(t) {
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, m := range r3WriteRE.FindAllStringSubmatch(s, -1) {
				verb, table := strings.ToLower(strings.Join(strings.Fields(m[1]), " ")), strings.ToLower(m[2])
				switch verb {
				case "insert into":
					seenInsert[table] = true
					if !insertable[table] {
						t.Errorf("%s writes (INSERT) to %q: the reconciliation package may write only its own stores", name, table)
					}
				case "update":
					if table == "of" || table == "set" {
						continue // "FOR UPDATE OF ..." is a lock clause, not a write
					}
					if !updatable[table] {
						t.Errorf("%s writes (UPDATE) to %q: the reconciliation package may write only its own stores", name, table)
					}
				default:
					t.Errorf("%s DELETEs from %q: the reconciliation package never deletes", name, table)
				}
			}
			return true
		})
	}
	// Vacuity control: the scan really sees the stores the stream writes.
	var got []string
	for tb := range seenInsert {
		got = append(got, tb)
	}
	sort.Strings(got)
	if len(got) < 4 {
		t.Fatalf("static scan is vacuous: it saw INSERTs into only %v", got)
	}
}

func TestR3_Static_ObservationEntryPointCallsOnlyThePaymentStatementStream(t *testing.T) {
	allowed := map[string]bool{
		"len": true, "make": true, "append": true,
		"nonActiveTenants": true, "ReconcilePaymentStatementForTenant": true,
	}
	var fn *ast.FuncDecl
	for _, f := range r3ProductionFiles(t) {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "observeNonActiveTenants" {
				fn = fd
			}
		}
	}
	if fn == nil {
		t.Fatal("observeNonActiveTenants not found")
	}
	calls := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := ce.Fun.(type) {
		case *ast.Ident:
			calls[fun.Name] = true
			if !allowed[fun.Name] {
				t.Errorf("observeNonActiveTenants calls %s: it may call only the payment_statement stream and the non-active tenant list", fun.Name)
			}
		case *ast.SelectorExpr:
			t.Errorf("observeNonActiveTenants calls %s.%s: only package-local stream functions are allowed", exprName(fun.X), fun.Sel.Name)
		case *ast.ArrayType, *ast.MapType:
			// conversions / composite constructors are not calls to behaviour
		}
		return true
	})
	for _, must := range []string{"nonActiveTenants", "ReconcilePaymentStatementForTenant"} {
		if !calls[must] {
			t.Errorf("observeNonActiveTenants no longer calls %s (the pin would be vacuous)", must)
		}
	}
}

func exprName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return "?"
}
