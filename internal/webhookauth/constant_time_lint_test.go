package webhookauth

// Constant-time comparison rule (Stage 10.3 W1a; security C9 "Constant
// time", ruling R6). No black-box test can prove a MAC comparison is
// constant-time, so this AST rule enforces it: in internal/webhookauth and
// in EVERY non-test file of every PACKAGE (directory) under internal/ or
// cmd/ in which any non-test file implements a VerificationScheme
// (declares an Extract method returning AuthMaterial), signature/MAC
// comparison must use hmac.Equal. Package scope, not file scope (gate
// 10.3-W1 security S-3): a scheme split into scheme.go (Extract) and
// verify.go (Verify plus a comparison helper) has verify.go scanned too.
// It fails on:
//
//   - == / != where an operand names signature/MAC material (sig, mac,
//     digest, expected...) and neither side is a literal, nil or len(...);
//   - bytes.Equal / bytes.Compare / strings.Compare / strings.EqualFold /
//     reflect.DeepEqual / {strings,bytes}.HasPrefix / HasSuffix over such
//     operands (HasPrefix is the "accepts a prefix" bug);
//   - any import of crypto/subtle (misuse-prone - compare the result the
//     wrong way, or on unequal lengths - hmac.Equal is the one sanctioned
//     primitive).
//
// Test files are out of scope (the conformance self-test deliberately
// contains a prefix-comparing broken scheme). This rule is also a named
// code-reviewer checklist item.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	// identWords splits a camelCase/snake_case identifier into words.
	identWords = regexp.MustCompile(`[A-Z]?[a-z0-9]+|[A-Z]+(?:[^a-z]|$)`)
	// sensitiveWords are the words that name signature/MAC material.
	sensitiveWords = map[string]bool{"sig": true, "sigs": true, "signature": true, "signatures": true, "mac": true, "macs": true, "hmac": true, "digest": true, "expected": true, "computed": true}
	// exemptIdent: enum/sentinel/header-name identifiers that merely
	// contain "Signature" (ReasonSignatureMissing, ErrSignatureInvalid,
	// SignatureHeader) are not signature MATERIAL.
	exemptIdent = regexp.MustCompile(`^(Reason|Err)|Header$`)
)

func sensitiveIdent(name string) bool {
	if exemptIdent.MatchString(name) {
		return false
	}
	for _, w := range identWords.FindAllString(name, -1) {
		if sensitiveWords[strings.ToLower(strings.Trim(w, "_"))] {
			return true
		}
	}
	return false
}

type ctViolation struct {
	pos  token.Position
	what string
}

func exprText(fset *token.FileSet, src []byte, e ast.Expr) string {
	return string(src[fset.Position(e.Pos()).Offset:fset.Position(e.End()).Offset])
}

// sensitiveOperand reports whether e names signature/MAC material.
func sensitiveOperand(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && sensitiveIdent(id.Name) {
			found = true
		}
		return !found
	})
	return found
}

// trivialOperand: a literal, nil, or len(...) - comparing material against
// these is a presence/length check, not a MAC comparison.
func trivialOperand(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.BasicLit:
		return true
	case *ast.Ident:
		return v.Name == "nil"
	case *ast.CallExpr:
		if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "len" {
			return true
		}
	}
	return false
}

var forbiddenCompareCalls = map[string]bool{
	"bytes.Equal": true, "bytes.Compare": true, "strings.Compare": true, "strings.EqualFold": true,
	"reflect.DeepEqual": true, "strings.HasPrefix": true, "strings.HasSuffix": true,
	"bytes.HasPrefix": true, "bytes.HasSuffix": true,
}

func checkConstantTime(fset *token.FileSet, file *ast.File, src []byte) []ctViolation {
	var out []ctViolation
	for _, imp := range file.Imports {
		if strings.Trim(imp.Path.Value, `"`) == "crypto/subtle" {
			out = append(out, ctViolation{fset.Position(imp.Pos()), "imports crypto/subtle; use hmac.Equal for signature comparison"})
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.BinaryExpr:
			if (v.Op == token.EQL || v.Op == token.NEQ) && !trivialOperand(v.X) && !trivialOperand(v.Y) &&
				(sensitiveOperand(v.X) || sensitiveOperand(v.Y)) {
				out = append(out, ctViolation{fset.Position(v.Pos()), "non-constant-time comparison: " + exprText(fset, src, v)})
			}
		case *ast.CallExpr:
			if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok && forbiddenCompareCalls[pkg.Name+"."+sel.Sel.Name] {
					for _, a := range v.Args {
						if sensitiveOperand(a) {
							out = append(out, ctViolation{fset.Position(v.Pos()), "non-constant-time comparison: " + exprText(fset, src, v)})
							break
						}
					}
				}
			}
		}
		return true
	})
	return out
}

// declaresScheme reports whether file declares an Extract method whose first
// result is (webhookauth.)AuthMaterial - i.e. implements VerificationScheme.
func declaresScheme(file *ast.File) bool {
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Name.Name != "Extract" || fn.Type.Results == nil || len(fn.Type.Results.List) == 0 {
			continue
		}
		switch t := fn.Type.Results.List[0].Type.(type) {
		case *ast.Ident:
			if t.Name == "AuthMaterial" {
				return true
			}
		case *ast.SelectorExpr:
			if t.Sel.Name == "AuthMaterial" {
				return true
			}
		}
	}
	return false
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

type parsedGoFile struct {
	rel  string
	fset *token.FileSet
	file *ast.File
	src  []byte
}

// constantTimeScan applies checkConstantTime to every non-test .go file of
// every package directory under root/tops that is in scope: any directory
// in alwaysDirs (and below), or any directory containing a non-test file
// that declares a VerificationScheme. It returns the scanned files (slash-
// separated, relative to root) and the violations.
func constantTimeScan(root string, tops, alwaysDirs []string) (map[string]bool, []ctViolation, error) {
	byDir := map[string][]parsedGoFile{}
	schemeDirs := map[string]bool{}
	for _, top := range tops {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			dir := filepath.Dir(path)
			byDir[dir] = append(byDir[dir], parsedGoFile{rel: filepath.ToSlash(rel), fset: fset, file: file, src: src})
			if declaresScheme(file) {
				schemeDirs[dir] = true
			}
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	inAlways := func(dir string) bool {
		for _, a := range alwaysDirs {
			if dir == a || strings.HasPrefix(dir, a+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
	scanned := map[string]bool{}
	var violations []ctViolation
	for dir, files := range byDir {
		if !schemeDirs[dir] && !inAlways(dir) {
			continue
		}
		for _, f := range files {
			scanned[f.rel] = true
			violations = append(violations, checkConstantTime(f.fset, f.file, f.src)...)
		}
	}
	return scanned, violations, nil
}

func TestConstantTimeCompare_SchemePackages(t *testing.T) {
	root := repoRoot(t)
	webhookauthDir := filepath.Join(root, "internal", "webhookauth")
	scanned, violations, err := constantTimeScan(root, []string{"internal", "cmd"}, []string{webhookauthDir})
	if err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{"internal/webhookauth/webhookauth.go", "internal/webhookauth/scheme.go", "internal/webhookauth/mock_scheme.go"} {
		if !scanned[must] {
			t.Fatalf("constant-time rule did not scan %s (scope discovery broken); scanned %v", must, scanned)
		}
	}
	for _, v := range violations {
		t.Errorf("%s: %s", v.pos, v.what)
	}
}

// TestConstantTimeCompare_ScopeIsWholePackage is security S-3's self-test:
// in a package where only scheme.go declares Extract, a bytes.Equal MAC
// comparison in a sibling verify.go is still found; a package with no
// scheme is not scanned at all.
func TestConstantTimeCompare_ScopeIsWholePackage(t *testing.T) {
	root := t.TempDir()
	write := func(rel, src string) {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/vendorx/scheme.go", `package vendorx
func (s S) Extract(in webhookauth.Inbound) (webhookauth.AuthMaterial, webhookauth.Reason, bool) { return webhookauth.AuthMaterial{}, "", false }`)
	write("internal/vendorx/verify.go", `package vendorx
import "bytes"
func macMatches(expectedMAC, got []byte) bool { return bytes.Equal(expectedMAC, got) }`)
	write("internal/vendorx/verify_test.go", `package vendorx
import "bytes"
func testOnly(mac, got []byte) bool { return bytes.Equal(mac, got) }`)
	write("internal/unrelated/util.go", `package unrelated
import "bytes"
func same(mac, got []byte) bool { return bytes.Equal(mac, got) }`)

	scanned, violations, err := constantTimeScan(root, []string{"internal"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !scanned["internal/vendorx/verify.go"] || !scanned["internal/vendorx/scheme.go"] {
		t.Fatalf("every non-test file of a scheme package must be scanned, scanned %v", scanned)
	}
	if scanned["internal/unrelated/util.go"] || scanned["internal/vendorx/verify_test.go"] {
		t.Fatalf("a package without a scheme, and test files, must not be scanned, scanned %v", scanned)
	}
	if len(violations) != 1 || !strings.HasSuffix(violations[0].pos.Filename, filepath.Join("vendorx", "verify.go")) {
		t.Fatalf("want exactly one violation, in vendorx/verify.go, got %v", violations)
	}
}

// TestConstantTimeCompare_RuleSelfTest proves the rule flags each forbidden
// shape and accepts the sanctioned ones.
func TestConstantTimeCompare_RuleSelfTest(t *testing.T) {
	bad := map[string]string{
		"== on hex":         `package x; func f(expectedHex, sigHex string) bool { return expectedHex == sigHex }`,
		"!= on bytes str":   `package x; func f(mac, got []byte) bool { return string(mac) != string(got) }`,
		"bytes.Equal":       `package x; import "bytes"; func f(mac, sig []byte) bool { return bytes.Equal(mac, sig) }`,
		"subtle import":     `package x; import "crypto/subtle"; func f(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }`,
		"HasPrefix":         `package x; import "strings"; func f(expected, got string) bool { return strings.HasPrefix(expected, got) }`,
		"EqualFold":         `package x; import "strings"; func f(sig, got string) bool { return strings.EqualFold(sig, got) }`,
		"reflect.DeepEqual": `package x; import "reflect"; func f(digest, got []byte) bool { return reflect.DeepEqual(digest, got) }`,
	}
	for name, src := range bad {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "bad.go", src, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(checkConstantTime(fset, file, []byte(src))) == 0 {
			t.Errorf("rule did not flag %s", name)
		}
	}
	good := `package x
import "crypto/hmac"
func f(expectedHex, sigHex, sigHeader string, reason, want string, err, target error) bool {
	if sigHeader == "" || len(sigHex) != 64 || reason != ReasonSignatureMissing || err == ErrSignatureInvalid {
		return false
	}
	return hmac.Equal([]byte(expectedHex), []byte(sigHex))
}`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "good.go", good, 0)
	if err != nil {
		t.Fatal(err)
	}
	if v := checkConstantTime(fset, file, []byte(good)); len(v) != 0 {
		t.Fatalf("rule flagged sanctioned code: %v", v)
	}
	for _, n := range []string{"sigHex", "expectedHex", "mac", "sig_hex", "MAC", "computedDigest", "gotSignature"} {
		if !sensitiveIdent(n) {
			t.Errorf("identifier %q must count as signature material", n)
		}
	}
	for _, n := range []string{"BindingSignedTenant", "SignedTimestamp", "ReasonSignatureInvalid", "ErrSignatureInvalid", "SignatureHeader", "macro", "design", "keyID"} {
		if sensitiveIdent(n) {
			t.Errorf("identifier %q must not count as signature material", n)
		}
	}
	scheme := `package x
func (s S) Extract(in webhookauth.Inbound) (webhookauth.AuthMaterial, webhookauth.Reason, bool) { return webhookauth.AuthMaterial{}, "", false }`
	if file, err = parser.ParseFile(token.NewFileSet(), "scheme.go", scheme, 0); err != nil || !declaresScheme(file) {
		t.Fatalf("scope discovery must recognise a VerificationScheme implementation (err=%v)", err)
	}
}
