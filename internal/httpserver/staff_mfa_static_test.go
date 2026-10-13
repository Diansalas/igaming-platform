package httpserver

// ADR 0119 S3 static pins (DB-free): the RevealFor* caller allow-list (S3 entry gate G5), the single staff MFA key
// constructor (gate 21.7 (b)), authtest imported only by tests (section 4.1), and the audit metadata key rule
// (section 5.4). Each scanner is proven non-vacuous by a planted offender.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// repoGoSources returns every non-test .go file under internal/ and cmd/ (path -> source), relative to the repo root.
func repoGoSources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, root := range []string{filepath.Join("..", "..", "internal"), filepath.Join("..", "..", "cmd")} {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(filepath.Join("..", ".."), p)
			out[filepath.ToSlash(rel)] = string(b)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(out) < 300 {
		t.Fatalf("read only %d sources: wrong working directory?", len(out))
	}
	return out
}

// callSites returns "file:func:method" for every call of a selector whose name is in names, in srcs.
func callSites(t *testing.T, srcs map[string]string, names map[string]bool) []string {
	t.Helper()
	var out []string
	for path, src := range srcs {
		f, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var name string
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					name = fun.Sel.Name
				case *ast.Ident:
					name = fun.Name
				}
				if names[name] {
					out = append(out, path+":"+fn.Name.Name+":"+name)
				}
				return true
			})
		}
	}
	sort.Strings(out)
	return out
}

// S3 entry gate G5: the raw value of a staffmfa secret type leaves it only at these production call sites.
var revealAllowlist = map[string]bool{
	"internal/httpserver/staff_auth_secrets.go:newStaffChallenge:RevealForDelivery":      true,
	"internal/httpserver/staff_auth_secrets.go:newEnrolStartResponse:RevealForEnrolment": true,
	"internal/httpserver/staff_auth_secrets.go:newEnrolConfirmResponse:RevealForDisplay": true,
	"cmd/seed-admin/main.go:printBootstrapToken:RevealForDelivery":                       true,
}

var revealNames = map[string]bool{"RevealForEnrolment": true, "RevealForDisplay": true, "RevealForDelivery": true}

func TestStatic_RevealForCallersAllowlist(t *testing.T) {
	sites := callSites(t, repoGoSources(t), revealNames)
	seen := map[string]bool{}
	for _, s := range sites {
		if strings.HasPrefix(s, "internal/staffmfa/") { // the definitions' own package (no call sites today)
			continue
		}
		if !revealAllowlist[s] {
			t.Errorf("RevealFor* called outside the allow-list (S3 gate G5): %s", s)
		}
		seen[s] = true
	}
	for s := range revealAllowlist {
		if !seen[s] {
			t.Errorf("allow-listed reveal site %s no longer exists: shrink the allow-list", s)
		}
	}
	// Non-vacuity: a planted offender is caught.
	planted := callSites(t, map[string]string{"internal/x/x.go": "package x\nfunc leak(m interface{ RevealForEnrolment() (string, string) }) { m.RevealForEnrolment() }\n"}, revealNames)
	if len(planted) != 1 || revealAllowlist[planted[0]] {
		t.Fatalf("the scanner must catch a planted reveal: %v", planted)
	}
}

// Gate 21.7 (b): *staffmfa.Keys has exactly one production constructor, LoadKeys (which runs VerifyStartup); NewKeys
// and NewKeysFromConfig are called only inside it (and CheckKeyConfig, which returns no keys); LoadKeys is called only
// by the two binaries.
func TestStatic_StaffMFAKeysSingleConstructor(t *testing.T) {
	srcs := repoGoSources(t)
	mine := staffmfaCallSites(t, srcs, map[string]bool{"NewKeys": true, "NewKeysFromConfig": true, "LoadKeys": true, "VerifyStartup": true})
	want := []string{
		"cmd/platform-api/main.go:run:LoadKeys",
		"cmd/seed-admin/main.go:run:LoadKeys",
		"internal/staffmfa/startup.go:CheckKeyConfig:NewKeysFromConfig",
		"internal/staffmfa/startup.go:LoadKeys:NewKeysFromConfig",
		"internal/staffmfa/startup.go:LoadKeys:VerifyStartup",
	}
	// NewKeysFromConfig itself calls NewKeys (keys.go); that is the constructor chain, not a second entry point.
	want = append(want, "internal/staffmfa/keys.go:NewKeysFromConfig:NewKeys")
	sort.Strings(want)
	if strings.Join(mine, "\n") != strings.Join(want, "\n") {
		t.Fatalf("staff MFA key construction sites:\n got  %v\n want %v", mine, want)
	}
	if !strings.Contains(srcs["cmd/platform-api/main.go"], "staffmfa.LoadKeys(") || !strings.Contains(srcs["cmd/seed-admin/main.go"], "staffmfa.LoadKeys(") {
		t.Fatal("both binaries must build the keys through staffmfa.LoadKeys")
	}
}

// staffmfaCallSites is callSites restricted to the staffmfa package: a call qualified `staffmfa.Name`, or an
// unqualified call inside internal/staffmfa itself (other packages have functions of the same names).
func staffmfaCallSites(t *testing.T, srcs map[string]string, names map[string]bool) []string {
	t.Helper()
	var out []string
	for path, src := range srcs {
		inPkg := strings.HasPrefix(path, "internal/staffmfa/") && !strings.HasPrefix(path, "internal/staffmfa/store/") && !strings.HasPrefix(path, "internal/staffmfa/totp/")
		f, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					if id, ok := fun.X.(*ast.Ident); ok && id.Name == "staffmfa" && names[fun.Sel.Name] {
						out = append(out, path+":"+fn.Name.Name+":"+fun.Sel.Name)
					}
				case *ast.Ident:
					if inPkg && names[fun.Name] {
						out = append(out, path+":"+fn.Name.Name+":"+fun.Name)
					}
				}
				return true
			})
		}
	}
	sort.Strings(out)
	return out
}

// Section 4.1: internal/auth/authtest (integration-tagged) is imported only by test files.
func TestStatic_AuthtestImportedOnlyByTests(t *testing.T) {
	for path, src := range repoGoSources(t) {
		f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range f.Imports {
			if p, _ := strconv.Unquote(im.Path.Value); strings.HasSuffix(p, "/internal/auth/authtest") && !strings.HasPrefix(path, "internal/auth/authtest/") {
				t.Errorf("%s imports authtest: only _test.go files may", path)
			}
		}
	}
	b, err := os.ReadFile(filepath.Join("..", "auth", "authtest", "authtest.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "//go:build integration\n") {
		t.Fatal("authtest must be behind the integration build tag")
	}
}

// Section 5.4: audit metadata in the S3 staff authentication code never carries a secret-shaped key.
func TestStatic_StaffAuthAuditMetadataKeys(t *testing.T) {
	files := []string{"internal/httpserver/staff_auth_handlers.go", "internal/httpserver/staff_routes.go", "internal/httpserver/staff_mfa.go",
		"internal/auth/staff_session.go", "cmd/seed-admin/main.go"}
	srcs := repoGoSources(t)
	scan := func(path, src string) []string {
		f, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		var bad []string
		ast.Inspect(f, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if _, isMap := cl.Type.(*ast.MapType); !isMap {
				return true
			}
			for _, el := range cl.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				bl, ok := kv.Key.(*ast.BasicLit)
				if !ok || bl.Kind != token.STRING {
					continue
				}
				k, _ := strconv.Unquote(bl.Value)
				if forbiddenMetadataKey(k) {
					bad = append(bad, path+": "+k)
				}
			}
			return true
		})
		return bad
	}
	scanned := 0
	for _, p := range files {
		src, ok := srcs[p]
		if !ok {
			t.Fatalf("%s not found", p)
		}
		scanned++
		for _, b := range scan(p, src) {
			t.Errorf("a secret-shaped audit / alert metadata key: %s", b)
		}
	}
	if scanned != len(files) {
		t.Fatal("vacuous scan")
	}
	if len(scan("x.go", "package x\nvar m = map[string]any{\"recovery\": 1, \"challenge_token\": 2, \"otpauth_uri\": 3}\n")) != 3 {
		t.Fatal("the scanner must catch planted secret-shaped keys")
	}
}

// forbiddenMetadataKey: the ADR 0119 section 5.4 set {secret, code, token, uri, recovery} as whole keys, and any key
// with a secret, token, uri, otpauth, password or recovery segment.
func forbiddenMetadataKey(k string) bool {
	switch k {
	case "secret", "code", "token", "uri", "recovery":
		return true
	}
	for _, seg := range strings.Split(k, "_") {
		switch seg {
		case "secret", "token", "uri", "otpauth", "password", "recovery":
			return true
		}
	}
	return false
}
