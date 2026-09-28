// CAS-PLAY-BOOTSTRAP-1 (ADR 0103) - code review C-8: F-9 marked
// ResolveLaunchToken's own doc comment DO-NOT-USE for a new caller (S-5's
// own reason - no binding check, no gate re-evaluation, no idempotency
// record), but a doc comment alone does not stop a future PR from adding
// one. This is the permanent static guard: no non-test .go file anywhere
// in this module may call ResolveLaunchToken.
//
// Deliberately lexical (an *ast.CallExpr whose callee resolves to the
// identifier "ResolveLaunchToken", by selector or bare name), matching
// this codebase's established static-guard style
// (internal/txscope/no_provider_call_in_tx_closure_static_test.go,
// internal/ledger/lockorder_static_test.go) - not a full type-checked
// call-graph analysis.
//
// Walks the WHOLE repository tree from the module root, not just
// internal/casino - a caller could live in any package (an httpserver
// handler, cmd/platform-api, another provider integration). This repo's
// own worktree layout is a real hazard for exactly that kind of walk (the
// coordinator's own report: "I just hit that trap merging I-core"): every
// other in-flight agent's worktree lives under .claude/worktrees/ as a
// FULL nested copy of this same module, so a naive filepath.WalkDir from
// the root would also re-scan every other worktree's own internal/casino,
// multiplying the work arbitrarily and risking false positives from
// another branch's WIP state. Skipped exactly the way `go` itself skips
// directories when it discovers packages (`go help packages`): any
// directory named "testdata", or whose name starts with "." or "_".
package casino

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rlt8SkipDir reports whether a directory entry name should never be
// descended into while walking for Go source - the same convention the go
// tool itself uses when discovering packages, extended with nothing else:
// no casino- or worktree-specific special-casing, so this helper stays
// correct as new worktrees come and go.
func rlt8SkipDir(name string) bool {
	if name == "testdata" {
		return true
	}
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
		return true
	}
	return false
}

type rlt8Violation struct {
	pos token.Position
}

func (v rlt8Violation) String() string {
	return v.pos.String() + ": calls casino.ResolveLaunchToken, which is DO-NOT-USE for any new caller (ADR 0103 §11 item 5, F-9)"
}

func rlt8FindViolations(root string) ([]rlt8Violation, error) {
	var violations []rlt8Violation
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && rlt8SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		file, parseErr := parser.ParseFile(fset, path, src, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				name = fn.Name
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			}
			if name == "ResolveLaunchToken" {
				violations = append(violations, rlt8Violation{pos: fset.Position(call.Pos())})
			}
			return true
		})
		return nil
	})
	return violations, err
}

// rlt8RepoRoot mirrors this codebase's own established convention
// (internal/txscope's ioc1RepoRoot, internal/webhookauth's
// constant_time_lint_test.go): walk up from the working directory to the
// module root (the directory containing go.mod), so this test works
// regardless of which directory `go test` is invoked from.
func rlt8RepoRoot(t *testing.T) string {
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

// TestResolveLaunchToken_NoNonTestCallersAnywhereInTheModule is C-8's own
// permanent regression guard.
func TestResolveLaunchToken_NoNonTestCallersAnywhereInTheModule(t *testing.T) {
	root := rlt8RepoRoot(t)
	violations, err := rlt8FindViolations(root)
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(violations) != 0 {
		msgs := make([]string, len(violations))
		for i, v := range violations {
			msgs[i] = v.String()
		}
		t.Fatalf("found %d non-test caller(s) of ResolveLaunchToken (DO-NOT-USE, ADR 0103 §11 item 5):\n%s",
			len(violations), strings.Join(msgs, "\n"))
	}
}
