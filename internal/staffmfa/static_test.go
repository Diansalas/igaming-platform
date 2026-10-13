package staffmfa

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Secret comparisons are constant time (ADR 0119 section 4.3): the TOTP code
// comparison uses subtle.ConstantTimeCompare and keyed hashes use hmac.Equal;
// no non-test source compares a secret-derived value with ==, != or
// bytes.Equal. A functional test cannot see timing, so this pin is the
// control (mutation T11 in the S1 evidence file).
func TestStatic_SecretComparisonsAreConstantTime(t *testing.T) {
	secretIdents := map[string]bool{"want": true, "got": true, "stored": true, "mac": true, "expected": true, "code": true}
	files, _ := filepath.Glob("*.go")
	more, _ := filepath.Glob("totp/*.go")
	files = append(files, more...)
	required := map[string]string{filepath.Join("totp", "totp.go"): "subtle.ConstantTimeCompare(", "keys.go": "hmac.Equal("}
	seen := map[string]bool{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "bytes.Equal") {
			t.Fatalf("%s uses bytes.Equal", path)
		}
		if need, ok := required[path]; ok {
			seen[path] = true
			if !strings.Contains(string(src), need) {
				t.Fatalf("%s no longer uses %s", path, need)
			}
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			be, ok := n.(*ast.BinaryExpr)
			if !ok || (be.Op != token.EQL && be.Op != token.NEQ) {
				return true
			}
			for _, side := range []ast.Expr{be.X, be.Y} {
				if call, ok := side.(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "len" {
						continue
					}
				}
				ast.Inspect(side, func(m ast.Node) bool {
					if id, ok := m.(*ast.Ident); ok && secretIdents[id.Name] {
						t.Errorf("%s: secret-derived value %q compared with %s", path, id.Name, be.Op)
					}
					return true
				})
			}
			return true
		})
	}
	for path := range required {
		if !seen[path] {
			t.Fatalf("required file %s was not scanned", path)
		}
	}
}
