package payments

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// D-7 (ADR 0111 section 25): the Go-side eligibility refusal (m4EligibilityRefusal) is the only
// enforcement of three properties (non-MOCK block, R is not a platform-issued reference, the line
// lies inside the attempt's window) that the SQL verdict does not carry. It must therefore run on
// EVERY M4 path, after the seals, before anything can commit or post. A source-level pin, because
// the call order is not observable through database state alone.
func TestD7_EligibilityRefusalRunsOnEveryM4Path_AfterTheSeals(t *testing.T) {
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
	first := func(fn, callee string) token.Pos {
		d := funcs[fn]
		if d == nil {
			t.Fatalf("%s not found", fn)
		}
		var pos token.Pos
		ast.Inspect(d, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch f := c.Fun.(type) {
			case *ast.Ident:
				name = f.Name
			case *ast.SelectorExpr:
				name = f.Sel.Name
			}
			if name == callee && (pos == token.NoPos || c.Pos() < pos) {
				pos = c.Pos()
			}
			return true
		})
		if pos == token.NoPos {
			t.Fatalf("%s does not call %s", fn, callee)
		}
		return pos
	}
	// The request: seals, then eligibility, then the requested audit row.
	if s, e, a := first("requestInTx", "verifyImportSeals"), first("requestInTx", "m4EligibilityRefusal"), first("requestInTx", "recordResolutionAudit"); s >= e || e >= a {
		t.Error("requestInTx must verify the seals, then run m4EligibilityRefusal, then write its audit row")
	}
	// The execution: seals, then eligibility (inside m4EvidenceRefusal, which decideInTx runs before postM4).
	if s, e := first("m4EvidenceRefusal", "verifyImportSeals"), first("m4EvidenceRefusal", "m4EligibilityRefusal"); s > e {
		t.Error("m4EvidenceRefusal must verify the seals before m4EligibilityRefusal")
	}
}
