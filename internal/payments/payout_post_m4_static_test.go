package payments

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// ADR 0111 4.8 (F-1) / ADR 0102 7.7 raise-last discipline, pinned on the source (no database needed):
//
//   - payoutPostM4Cell's FINAL statement is `return raisePayoutDisputeAlert(...)`, with exactly one raise and exactly one
//     audit.Record, and the audit.Record is textually BEFORE the raise;
//   - nothing else in the function writes (no Exec of an UPDATE/INSERT/DELETE, no ledger or withdrawal writer, no state
//     transition): the cell reads, audits and raises, nothing more;
//   - at every call site the cell's result is RETURNED (never dropped, never followed by another statement that could
//     take a business lock): receipt.go returns it as the cell value, payout.go returns it from the function.
func parsePayoutPostM4(t *testing.T, file string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	return fset, af
}

func findFunc(af *ast.File, name string) *ast.FuncDecl {
	for _, d := range af.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name && fd.Recv == nil {
			return fd
		}
	}
	return nil
}

func TestPayoutPostM4Cell_RaiseIsTheLastStatement_AuditBeforeRaise_NoWrites(t *testing.T) {
	fset, af := parsePayoutPostM4(t, "payout_post_m4.go")
	fd := findFunc(af, "payoutPostM4Cell")
	if fd == nil {
		t.Fatal("payoutPostM4Cell not found")
	}
	stmts := fd.Body.List
	last, ok := stmts[len(stmts)-1].(*ast.ReturnStmt)
	if !ok || len(last.Results) != 1 {
		t.Fatalf("the last statement must be `return raisePayoutDisputeAlert(...)`")
	}
	call, ok := last.Results[0].(*ast.CallExpr)
	if !ok {
		t.Fatalf("the last statement must return the raise call")
	}
	if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "raisePayoutDisputeAlert" {
		t.Fatalf("the last statement must return raisePayoutDisputeAlert(...)")
	}
	var raises, audits int
	var raisePos, auditPos token.Pos
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch f := c.Fun.(type) {
		case *ast.Ident:
			if f.Name == "raisePayoutDisputeAlert" {
				raises++
				raisePos = c.Pos()
			}
		case *ast.SelectorExpr:
			if x, ok := f.X.(*ast.Ident); ok && x.Name == "audit" && f.Sel.Name == "Record" {
				audits++
				auditPos = c.Pos()
			}
			// A write of any kind is a finding: the cell changes nothing.
			switch f.Sel.Name {
			case "Exec", "Post", "Complete", "Fail", "CopyFrom", "SendBatch":
				t.Errorf("line %d: payoutPostM4Cell must not write (%s)", fset.Position(c.Pos()).Line, f.Sel.Name)
			}
			if x, ok := f.X.(*ast.Ident); ok && (x.Name == "withdrawal" || x.Name == "ledger") {
				switch f.Sel.Name {
				case "LockForPayoutEvidence", "GetByID":
				default:
					t.Errorf("line %d: payoutPostM4Cell may only read the withdrawal (%s.%s)", fset.Position(c.Pos()).Line, x.Name, f.Sel.Name)
				}
			}
		}
		return true
	})
	if raises != 1 || audits != 1 {
		t.Fatalf("want exactly one raise and one audit.Record, found %d / %d", raises, audits)
	}
	if auditPos >= raisePos {
		t.Fatal("the audit row must be written BEFORE the raise")
	}
	// No statement follows the raise's return (it IS the final statement) and no deferred call runs after it.
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if _, ok := n.(*ast.DeferStmt); ok {
			t.Error("payoutPostM4Cell must not defer anything after the raise")
		}
		return true
	})
}

// Every call site hands the cell's result straight back: `return ..., payoutPostM4Cell(...)` or `return payoutPostM4Cell(...)`,
// never an expression statement or an assignment followed by more work.
func TestPayoutPostM4Cell_EveryCallSiteReturnsTheResult(t *testing.T) {
	sites := 0
	for _, file := range []string{"receipt.go", "payout.go"} {
		fset, af := parsePayoutPostM4(t, file)
		ast.Inspect(af, func(n ast.Node) bool {
			rs, ok := n.(*ast.ReturnStmt)
			if ok {
				for _, r := range rs.Results {
					if c, ok := r.(*ast.CallExpr); ok {
						if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "payoutPostM4Cell" {
							sites++
						}
					}
				}
				return true
			}
			if c, ok := n.(*ast.CallExpr); ok {
				if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "payoutPostM4Cell" {
					// counted above when it is a return operand; any other position is a finding
					if !isReturnOperand(af, c) {
						t.Errorf("%s line %d: payoutPostM4Cell must be returned directly", file, fset.Position(c.Pos()).Line)
					}
				}
			}
			return true
		})
	}
	if sites != 5 {
		t.Fatalf("expected 5 returned call sites (2 receipt cells, 1 late-evidence, 2 poll), found %d", sites)
	}
}

func isReturnOperand(af *ast.File, target *ast.CallExpr) bool {
	found := false
	ast.Inspect(af, func(n ast.Node) bool {
		if rs, ok := n.(*ast.ReturnStmt); ok {
			for _, r := range rs.Results {
				if r == ast.Expr(target) {
					found = true
				}
			}
		}
		return true
	})
	return found
}
