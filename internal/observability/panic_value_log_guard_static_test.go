package observability

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// SAFECALL-LOG-GUARD-1 / PANIC-LOG-VALUE-1 static guard.
//
// A recovered panic value can carry a credential, a token, a URL with a
// query-string secret, a vendor body or player PII. In production code
// (every non-_test.go file under cmd/, internal/ and deploy/) a value
// obtained from recover() may ONLY be:
//
//   - compared with nil (`r != nil`, `recover() != nil`);
//   - discarded (`_ = recover()`);
//   - re-raised (`panic(r)`);
//   - rendered as its TYPE by a fmt formatting call whose matching verb is
//     %T (`fmt.Sprintf("%T", r)`, `fmt.Errorf("... (%T)", r)`).
//
// Every other use - an argument to a logger, slog attribute or other fmt
// call, an alias (`x := r`), a composite literal, a type assertion, a
// return, an assignment of recover() to a pre-existing variable, or
// recover() passed straight into a call - is a violation unless it is in
// panicValueUseAllowList below, which is EXACT: an entry whose count does
// not match the scan (including a stale entry that no longer matches
// anything) fails the test, so the list cannot silently rot.
//
// Limits (stated, not hidden): the analysis is syntactic and per function;
// it tracks the variable bound by `x := recover()` through Go's object
// resolution within the file, not through a value once it has escaped via
// an allow-listed use. That is why every escape is itself a violation
// unless reviewed here.

// panicValueUseAllowList: key -> expected count. Key format:
// "<repo-relative path>|<enclosing func decl>|<use kind>". Each entry
// carries the reviewed reason.
var panicValueUseAllowList = map[string]int{
	// Test-support conformance harness (package webhookauthtest, imported
	// only by _test.go files): a panicking scheme under test is reported
	// through testing.T failure text, never through a runtime logger. The
	// panic value is the harness's diagnostic for the developer.
	"internal/webhookauth/webhookauthtest/webhookauthtest.go|runCase|call:r.errorf":  1,
	"internal/webhookauth/webhookauthtest/webhookauthtest.go|env.run|composite":      1,
	"internal/webhookauth/webhookauthtest/webhookauthtest.go|caseSC5|recover-escape": 1,
}

// panicValueUseViolations scans one parsed file and returns one key per
// disallowed use (keys may repeat).
func panicValueUseViolations(fset *token.FileSet, rel string, file *ast.File) []string {
	parents := map[ast.Node]ast.Node{}
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if len(stack) > 0 {
			parents[n] = stack[len(stack)-1]
		}
		stack = append(stack, n)
		return true
	})

	enclosingDecl := func(n ast.Node) string {
		for p := parents[n]; p != nil; p = parents[p] {
			if fd, ok := p.(*ast.FuncDecl); ok {
				if fd.Recv != nil && len(fd.Recv.List) == 1 {
					return recvTypeName(fd.Recv.List[0].Type) + "." + fd.Name.Name
				}
				return fd.Name.Name
			}
		}
		return "<file scope>"
	}

	var out []string
	add := func(n ast.Node, kind string) {
		out = append(out, rel+"|"+enclosingDecl(n)+"|"+kind)
	}

	// Pass 1: every recover() call and the variables it binds.
	// Identifiers are tracked by (enclosing declaration, name) instead of the deprecated ast.Object resolution; the
	// scan is syntax-only, so a same-named identifier in the same declaration is treated as the recovered value (conservative).
	type scopeVar struct {
		block ast.Node
		name  string
		after token.Pos
	}
	var tracked []scopeVar
	defining := map[*ast.Ident]bool{}
	enclosingBlock := func(n ast.Node) ast.Node {
		for p := parents[n]; p != nil; p = parents[p] {
			if _, ok := p.(*ast.BlockStmt); ok {
				return p
			}
		}
		return file
	}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isRecoverCall(call) {
			return true
		}
		switch p := parents[call].(type) {
		case *ast.BinaryExpr:
			if (p.Op == token.EQL || p.Op == token.NEQ) && (isNilIdent(p.X) || isNilIdent(p.Y)) {
				return true
			}
		case *ast.AssignStmt:
			if len(p.Lhs) == 1 && len(p.Rhs) == 1 {
				if id, ok := p.Lhs[0].(*ast.Ident); ok {
					if id.Name == "_" {
						return true
					}
					if p.Tok == token.DEFINE {
						tracked = append(tracked, scopeVar{enclosingBlock(p), id.Name, p.End()})
						defining[id] = true
						return true
					}
				}
			}
			add(call, "recover-escape")
			return true
		case *ast.ValueSpec:
			if len(p.Names) == 1 {
				tracked = append(tracked, scopeVar{enclosingBlock(p), p.Names[0].Name, p.End()})
				defining[p.Names[0]] = true
				return true
			}
		}
		add(call, "recover-use:"+nodeKind(parents[call]))
		return true
	})
	if len(tracked) == 0 {
		return out
	}

	// Pass 2: every use of a tracked variable.
	ast.Inspect(file, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || defining[id] {
			return true
		}
		inScope := false
		for _, t := range tracked {
			if t.name == id.Name && id.Pos() >= t.after && id.Pos() >= t.block.Pos() && id.End() <= t.block.End() {
				inScope = true
				break
			}
		}
		if !inScope {
			return true
		}
		if sel, ok := parents[id].(*ast.SelectorExpr); ok && sel.Sel == id {
			return true
		}
		switch p := parents[id].(type) {
		case *ast.BinaryExpr:
			if (p.Op == token.EQL || p.Op == token.NEQ) && (isNilIdent(p.X) || isNilIdent(p.Y)) {
				return true
			}
			add(id, "binary")
		case *ast.CallExpr:
			if fn, ok := p.Fun.(*ast.Ident); ok && fn.Name == "panic" {
				return true
			}
			if argIsTypeVerbOnly(p, id) {
				return true
			}
			add(id, "call:"+exprString(p.Fun))
		default:
			add(id, nodeKind(p))
		}
		return true
	})
	return out
}

func isRecoverCall(c *ast.CallExpr) bool {
	id, ok := c.Fun.(*ast.Ident)
	return ok && id.Name == "recover" && len(c.Args) == 0
}

func isNilIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}

func isDefiningIdent(id *ast.Ident, decl ast.Node) bool {
	switch d := decl.(type) {
	case *ast.AssignStmt:
		for _, l := range d.Lhs {
			if l == id {
				return true
			}
		}
	case *ast.ValueSpec:
		for _, nm := range d.Names {
			if nm == id {
				return true
			}
		}
	}
	return false
}

func recvTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return recvTypeName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return recvTypeName(t.X)
	case *ast.IndexListExpr:
		return recvTypeName(t.X)
	}
	return "?"
}

func exprString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return exprString(t.X) + "." + t.Sel.Name
	case *ast.CallExpr:
		return exprString(t.Fun) + "()"
	case *ast.IndexExpr:
		return exprString(t.X)
	case *ast.FuncLit:
		return "func-literal"
	}
	return fmt.Sprintf("%T", e)
}

func nodeKind(n ast.Node) string {
	switch n.(type) {
	case *ast.CompositeLit, *ast.KeyValueExpr:
		return "composite"
	case *ast.AssignStmt, *ast.ValueSpec:
		return "alias"
	case *ast.ReturnStmt:
		return "return"
	case *ast.TypeAssertExpr:
		return "type-assert"
	case *ast.SendStmt:
		return "send"
	case nil:
		return "nil-parent"
	}
	return strings.TrimPrefix(fmt.Sprintf("%T", n), "*ast.")
}

// fmtFormatArgStart maps a fmt formatting function to the index of its
// format-string argument.
var fmtFormatArgStart = map[string]int{
	"Sprintf": 0, "Errorf": 0, "Printf": 0, "Fprintf": 1, "Appendf": 1,
}

// argIsTypeVerbOnly reports whether id is an argument of a fmt.*f call
// with a literal format string whose verb for that argument is %T.
func argIsTypeVerbOnly(call *ast.CallExpr, id *ast.Ident) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "fmt" {
		return false
	}
	fi, ok := fmtFormatArgStart[sel.Sel.Name]
	if !ok || len(call.Args) <= fi {
		return false
	}
	lit, ok := call.Args[fi].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	format, err := strconv.Unquote(lit.Value)
	if err != nil {
		return false
	}
	verbs, ok := formatVerbs(format)
	if !ok {
		return false
	}
	for i, a := range call.Args[fi+1:] {
		if a == ast.Expr(id) {
			return i < len(verbs) && verbs[i] == 'T'
		}
	}
	return false
}

// formatVerbs returns the verb consuming each successive argument. It
// refuses (ok=false) formats it does not fully understand: explicit
// argument indexes and '*' width/precision.
func formatVerbs(f string) (verbs []rune, ok bool) {
	for i := 0; i < len(f); i++ {
		if f[i] != '%' {
			continue
		}
		i++
		if i >= len(f) {
			return nil, false
		}
		if f[i] == '%' {
			continue
		}
		for i < len(f) && strings.IndexByte("+-# 0123456789.", f[i]) >= 0 {
			i++
		}
		if i >= len(f) || f[i] == '*' || f[i] == '[' {
			return nil, false
		}
		verbs = append(verbs, rune(f[i]))
	}
	return verbs, true
}

func panicGuardRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}
	return root
}

func TestStatic_RecoveredPanicValueNeverLogged(t *testing.T) {
	root := panicGuardRepoRoot(t)
	got := map[string]int{}
	scanned, recoverFiles := 0, 0
	for _, top := range []string{"cmd", "internal", "deploy"} {
		err := filepath.Walk(filepath.Join(root, top), func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				switch info.Name() {
				case "node_modules", "testdata", ".git":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			scanned++
			if !strings.Contains(string(src), "recover()") {
				return nil
			}
			recoverFiles++
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, p, src, parser.ParseComments)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			for _, k := range panicValueUseViolations(fset, filepath.ToSlash(rel), f) {
				got[k]++
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if scanned < 500 || recoverFiles < 15 {
		t.Fatalf("scan looks wrong: %d files, %d with recover() (expected >= 500 and >= 15)", scanned, recoverFiles)
	}
	var problems []string
	for k, n := range got {
		if want, ok := panicValueUseAllowList[k]; !ok {
			problems = append(problems, fmt.Sprintf("NOT ALLOWED: %s (x%d) - log only fmt.Sprintf(\"%%T\", r) and a stable code", k, n))
		} else if want != n {
			problems = append(problems, fmt.Sprintf("COUNT MISMATCH: %s found %d, allow-list says %d", k, n, want))
		}
	}
	for k := range panicValueUseAllowList {
		if _, ok := got[k]; !ok {
			problems = append(problems, "STALE allow-list entry (matches nothing): "+k)
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("recovered panic value use outside the reviewed allow-list:\n%s", strings.Join(problems, "\n"))
	}
}

// The analyzer itself must catch every known-bad shape and accept every
// known-good one (guards the guard).
func TestStatic_RecoveredPanicValueGuard_SelfTest(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string // violation kinds, in order
	}{
		{"slog_attr", `defer func() { if r := recover(); r != nil { logger.Error("x", "panic", r) } }()`, []string{"call:logger.Error"}},
		{"slog_any", `defer func() { if r := recover(); r != nil { logger.Error("x", slog.Any("p", r)) } }()`, []string{"call:slog.Any"}},
		{"fmt_sprint", `defer func() { if r := recover(); r != nil { logger.Error("x", "p", fmt.Sprint(r)) } }()`, []string{"call:fmt.Sprint"}},
		{"fmt_v", `defer func() { if r := recover(); r != nil { _ = fmt.Sprintf("%v", r) } }()`, []string{"call:fmt.Sprintf"}},
		{"fmt_T_then_v", `defer func() { if r := recover(); r != nil { _ = fmt.Sprintf("%T %v", r, r) } }()`, []string{"call:fmt.Sprintf"}},
		{"fmt_w", `defer func() { if r := recover(); r != nil { err = fmt.Errorf("%w", r) } }()`, []string{"call:fmt.Errorf"}},
		{"fmt_star", `defer func() { if r := recover(); r != nil { _ = fmt.Sprintf("%*T", 1, r) } }()`, []string{"call:fmt.Sprintf"}},
		{"log_printf", `defer func() { if r := recover(); r != nil { log.Printf("%s", r) } }()`, []string{"call:log.Printf"}},
		{"alias", `defer func() { if r := recover(); r != nil { x := r; _ = x } }()`, []string{"alias"}},
		{"composite", `defer func() { if r := recover(); r != nil { _ = T{v: r} } }()`, []string{"composite"}},
		{"type_assert", `defer func() { if r := recover(); r != nil { if e, ok := r.(error); ok { _ = e } } }()`, []string{"type-assert"}},
		{"helper_call", `defer func() { if r := recover(); r != nil { logPanic(r) } }()`, []string{"call:logPanic"}},
		{"recover_into_call", `defer func() { logger.Error("x", "p", recover()) }()`, []string{"recover-use:CallExpr"}},
		{"recover_escape", `defer func() { v = recover() }()`, []string{"recover-escape"}},
		{"ok_type_only", `defer func() { if r := recover(); r != nil { logger.Error("x", "panic_type", fmt.Sprintf("%T", r)) } }()`, nil},
		{"ok_errorf_T", `defer func() { if r := recover(); r != nil { err = fmt.Errorf("adapter panic (%T) %d%%", r, 3) } }()`, nil},
		{"ok_compare", `defer func() { if recover() != nil { err = e } }()`, nil},
		{"ok_discard", `defer func() { _ = recover() }()`, nil},
		{"ok_repanic", `defer func() { if r := recover(); r != nil { cleanup(); panic(r) } }()`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "package p\nfunc f() {\n" + tc.body + "\n}\n"
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "x.go", src, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got := panicValueUseViolations(fset, "x.go", f)
			var kinds []string
			for _, k := range got {
				parts := strings.SplitN(k, "|", 3)
				if parts[1] != "f" {
					t.Fatalf("enclosing decl = %q, want f", parts[1])
				}
				kinds = append(kinds, parts[2])
			}
			if strings.Join(kinds, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("violations = %v, want %v", kinds, tc.want)
			}
		})
	}
}
