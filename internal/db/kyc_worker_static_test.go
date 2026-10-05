package db

// PRH-2 E1 (ADR 0106 section 10.4, security F8) static guards for the KYC worker
// identity, pure go/ast:
//
//   - 32(a): no non-test Go file outside internal/db contains the STRING LITERAL
//     `app.platform_service_id` (a raw set_config could spoof any identity; comments
//     are excluded, only literals count);
//   - the closed vocabulary lists the worker, and WithPlatformService refuses an
//     unknown string before opening any transaction (test 33, unit level).

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func dbRepoRoot(t *testing.T) string {
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

// guc literal scan: returns "file:line" for each string literal in src that
// contains needle.
func literalsContaining(t *testing.T, rel, src, needle string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	var hits []string
	ast.Inspect(f, func(n ast.Node) bool {
		bl, ok := n.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			return true
		}
		s, uerr := strconv.Unquote(bl.Value)
		if uerr == nil && strings.Contains(s, needle) {
			hits = append(hits, rel+":"+strconv.Itoa(fset.Position(bl.Pos()).Line))
		}
		return true
	})
	return hits
}

func TestStatic_PlatformServiceGUCLiteralOnlyInsideDBPackage_F8a(t *testing.T) {
	root := dbRepoRoot(t)
	var seenInDB int
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.Walk(filepath.Join(root, top), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel := filepath.ToSlash(mustRel(root, path))
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			hits := literalsContaining(t, rel, string(b), "app.platform_service_id")
			if strings.HasPrefix(rel, "internal/db/") {
				seenInDB += len(hits)
				return nil
			}
			for _, h := range hits {
				t.Errorf("%s: string literal app.platform_service_id outside internal/db (a raw set_config could spoof a service identity)", h)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if seenInDB == 0 {
		t.Fatal("expected internal/db/platform_service.go to carry the literal (guard is vacuous)")
	}
}

func mustRel(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}

// Negative control: a literal outside internal/db is flagged, a comment is not.
func TestStatic_PlatformServiceGUCLiteral_NegativeControl(t *testing.T) {
	bad := "package x\nfunc f(){ _ = `SELECT set_config('app.platform_service_id', 'kyc_submission_worker', true)` }\n"
	if got := literalsContaining(t, "internal/x/bad.go", bad, "app.platform_service_id"); len(got) != 1 {
		t.Fatalf("a string literal must be flagged, got %v", got)
	}
	ok := "package x\n// app.platform_service_id is only a comment here\nfunc f(){}\n"
	if got := literalsContaining(t, "internal/x/ok.go", ok, "app.platform_service_id"); len(got) != 0 {
		t.Fatalf("a comment must not be flagged, got %v", got)
	}
}

func TestPlatformServiceAllowlistContainsKYCWorkerAndRefusesUnknown_33(t *testing.T) {
	if _, ok := platformServiceAllowlist[ServiceKYCSubmissionWorker]; !ok {
		t.Fatal("ServiceKYCSubmissionWorker must be in the closed allowlist")
	}
	if string(ServiceKYCSubmissionWorker) != "kyc_submission_worker" {
		t.Fatalf("worker identity string = %q", ServiceKYCSubmissionWorker)
	}
	// WithPlatformService validates BEFORE touching the pool: a zero Pool
	// (nil inner pool) must return the refusal, not panic on Begin.
	var p Pool
	for _, bad := range []PlatformService{"", "kyc_submission_worker ", "KYC_SUBMISSION_WORKER", "nope", "kyc_submission_worker'; --"} {
		if err := p.WithPlatformService(context.Background(), bad, func(context.Context, pgx.Tx) error { return nil }); err == nil {
			t.Errorf("WithPlatformService(%q) must be refused before any transaction", bad)
		}
	}
}
