package httpserver

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ADR 0114 S0 (security BLOCKER 2): the M4 evidence reference text R is never exposed through the API
// or tenant-visible audit metadata while evidence_ref_hash is OPEN. The server still reads R from the
// database (internal/payments/manual_resolution_m4.go).
//
// This is a heuristic AST guard, not a proof: it catches the literal key in any spelling a variable
// alias cannot hide (the string constant "evidence_reference" anywhere outside SQL text), a JSON tag
// of that name, and any dereference of a `.EvidenceReference` field outside the verification file.
// The REAL guard is the integration check that R does not appear anywhere in the K3 response body
// (payment_force_resolution_m4_api_integration_test.go) nor in the audit row's metadata
// (m4_resolve1_integration_test.go, position() over the whole metadata text). Those tests must stay in
// the required suite.
const evidenceVerificationFile = "manual_resolution_m4.go"

func scanEvidenceReference(t *testing.T, srcs map[string]string) []string {
	t.Helper()
	var out []string
	fset := token.NewFileSet()
	for name, src := range srcs {
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		base := filepath.Base(name)
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BasicLit:
				if x.Kind != token.STRING {
					return true
				}
				v, _ := strconv.Unquote(x.Value)
				if v == "evidence_reference" || strings.Contains(v, `json:"evidence_reference"`) || strings.Contains(v, `json:"evidence_reference,`) {
					out = append(out, fset.Position(x.Pos()).String()+": literal "+strconv.Quote(v))
				}
			case *ast.StarExpr:
				if sel, ok := x.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "EvidenceReference" && base != evidenceVerificationFile {
					out = append(out, fset.Position(x.Pos()).String()+": dereference of .EvidenceReference outside the verification file")
				}
			}
			return true
		})
	}
	return out
}

func TestEvidenceReferenceText_NotExposedByAPIOrAuditMetadata(t *testing.T) {
	srcs := map[string]string{}
	for p, s := range readInternalSources(t) {
		d := filepath.Dir(p)
		if d == filepath.Join("..", "httpserver") || d == filepath.Join("..", "payments") {
			srcs[p] = s
		}
	}
	if len(srcs) < 30 {
		t.Fatalf("read only %d files", len(srcs))
	}
	for _, f := range scanEvidenceReference(t, srcs) {
		t.Errorf("evidence reference text exposure: %s", f)
	}
}

func TestEvidenceReferenceText_PlantedEvasionsAreCaught(t *testing.T) {
	for name, src := range map[string]string{
		"json tag":       "package x\ntype d struct { R *string `json:\"evidence_reference,omitempty\"` }",
		"literal key":    "package x\nfunc f(md map[string]any) { md[\"evidence_reference\"] = 1 }",
		"alias variable": "package x\nfunc f(md map[string]any, r struct{ EvidenceReference *string }) { k := \"evidence_reference\"; md[k] = *r.EvidenceReference }",
		"deref only":     "package x\nfunc f(md map[string]any, r struct{ EvidenceReference *string }, k string) { md[k] = *r.EvidenceReference }",
	} {
		if len(scanEvidenceReference(t, map[string]string{"planted.go": src})) == 0 {
			t.Errorf("planted evasion %q was not flagged", name)
		}
	}
	if got := scanEvidenceReference(t, map[string]string{evidenceVerificationFile: "package x\nfunc f(r struct{ EvidenceReference *string }) string { return *r.EvidenceReference }"}); len(got) != 0 {
		t.Errorf("the verification file must be exempt: %v", got)
	}
}
