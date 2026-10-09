package payments

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// Review amendment L-2/sec (ADR 0111 §17.7): the import seals are verifiable in
// Go only, so every M4 path must call verifyImportSeals before it commits.
// A source-level pin (the call order is not observable through database state
// alone): the request verifies before its audit row; the execution re-evaluates
// the evidence, then verifies, and only then posts; postM4 has exactly one
// caller, and that caller runs m4EvidenceRefusal first.
func TestL2_EveryM4PathVerifiesImportSeals(t *testing.T) {
	fset := token.NewFileSet()
	funcs := map[string]*ast.FuncDecl{}
	for _, f := range []string{"manual_resolution.go", "manual_resolution_m4.go"} {
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				funcs[fd.Name.Name] = fd
			}
		}
	}
	// calls returns the source positions of every call to name inside fn.
	calls := func(fn *ast.FuncDecl, name string) []token.Pos {
		var out []token.Pos
		ast.Inspect(fn, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch f := c.Fun.(type) {
			case *ast.Ident:
				if f.Name == name {
					out = append(out, c.Pos())
				}
			case *ast.SelectorExpr:
				if f.Sel.Name == name {
					out = append(out, c.Pos())
				}
			}
			return true
		})
		return out
	}
	need := func(fn, callee string) []token.Pos {
		d := funcs[fn]
		if d == nil {
			t.Fatalf("%s not found", fn)
		}
		ps := calls(d, callee)
		if len(ps) == 0 {
			t.Fatalf("%s does not call %s", fn, callee)
		}
		return ps
	}
	// The request: seals verified before the requested audit row.
	v := need("requestInTx", "verifyImportSeals")
	a := need("requestInTx", "recordResolutionAudit")
	if v[0] > a[0] {
		t.Error("requestInTx writes its audit row before verifying the import seals")
	}
	// The execution: evaluate, then verify.
	e := need("m4EvidenceRefusal", "EvaluateM4Evidence")
	v = need("m4EvidenceRefusal", "verifyImportSeals")
	if e[0] > v[0] {
		t.Error("m4EvidenceRefusal verifies before re-evaluating the evidence")
	}
	// postM4 has exactly one caller, which runs m4EvidenceRefusal first.
	var callers []string
	for name, fd := range funcs {
		if len(calls(fd, "postM4")) > 0 {
			callers = append(callers, name)
		}
	}
	if len(callers) != 1 || callers[0] != "decideInTx" {
		t.Fatalf("postM4 callers: %v (want exactly decideInTx)", callers)
	}
	r := need("decideInTx", "m4EvidenceRefusal")
	p := need("decideInTx", "postM4")
	if r[0] > p[0] {
		t.Error("decideInTx posts an M4 before m4EvidenceRefusal")
	}
	// verifyImportSeals keeps its fail-closed guard (no keys / no imports -> refused).
	if len(calls(funcs["verifyImportSeals"], "VerifyStatementImportInTx")) == 0 {
		t.Error("verifyImportSeals no longer calls payoutinstrument.VerifyStatementImportInTx")
	}
}
