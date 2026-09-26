package awssm

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// awsSDKImportPrefix is the module path condition C12.2 (04-review-
// security.md §3) and ADR 0093 §6 confine to this one package. Widened
// from "github.com/aws/aws-sdk-go-v2/" to every github.com/aws/ module
// (gate W2/W3 security finding S-5), so smithy-go and the v1 SDK
// (github.com/aws/aws-sdk-go) are covered too: no other runtime AWS path.
const awsSDKImportPrefix = "github.com/aws/"

// repoRoot locates the repository root relative to this test file's own
// source location (mirrors internal/secretstore/secretstore_test.go's
// repoRoot/nonTestGoFiles helpers, duplicated here so this package's CI
// check has no dependency on the secretstore test-only helpers).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve absolute path: %v", err)
	}
	// This file lives at <repo>/internal/secretstore/awssm/import_boundary_test.go.
	return filepath.Dir(filepath.Dir(filepath.Dir(dir)))
}

// nonAWSSMGoFiles walks root and returns every .go file NOT under
// internal/secretstore/awssm, skipping vendor and testdata directories.
func nonAWSSMGoFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	awssmDir := filepath.Join(root, "internal", "secretstore", "awssm")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "vendor" || name == "testdata" || (strings.HasPrefix(name, ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(awssmDir, path)
		if !strings.HasPrefix(rel, "..") {
			return nil // inside internal/secretstore/awssm itself
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func awsSDKImportersIn(t *testing.T, root string) []string {
	t.Helper()
	fset := token.NewFileSet()
	var offenders []string
	for _, path := range nonAWSSMGoFiles(t, root) {
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			if strings.HasPrefix(p, awsSDKImportPrefix) {
				offenders = append(offenders, path)
				break
			}
		}
	}
	return offenders
}

// TestImportBoundary_NoOtherPackageImportsAWSSDK (ADR 0093 §6; security
// review C12.2): internal/secretstore/awssm is the ONLY package in this
// module permitted to import the AWS SDK v2. This stops the dependency
// from quietly becoming the path for some other runtime AWS call.
func TestImportBoundary_NoOtherPackageImportsAWSSDK(t *testing.T) {
	if offenders := awsSDKImportersIn(t, repoRoot(t)); len(offenders) != 0 {
		t.Fatalf("non-awssm files import the AWS SDK: %v", offenders)
	}
}

// TestImportBoundary_CheckCatchesOffender is the negative control: a file
// outside internal/secretstore/awssm importing the AWS SDK in a temp tree
// is found, proving the scan is a real, runnable check.
func TestImportBoundary_CheckCatchesOffender(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "internal", "secretstore", "awssm"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "internal", "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	badSrc := "package other\n\nimport _ \"" + awsSDKImportPrefix + "service/secretsmanager\"\n"
	if err := os.WriteFile(filepath.Join(dir, "internal", "other", "bad.go"), []byte(badSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	okSrc := "package awssm\n\nimport _ \"" + awsSDKImportPrefix + "service/secretsmanager\"\n"
	if err := os.WriteFile(filepath.Join(dir, "internal", "secretstore", "awssm", "ok.go"), []byte(okSrc), 0o600); err != nil {
		t.Fatal(err)
	}

	got := awsSDKImportersIn(t, dir)
	if len(got) != 1 || !strings.HasSuffix(got[0], filepath.Join("internal", "other", "bad.go")) {
		t.Fatalf("expected exactly internal/other/bad.go, got %v", got)
	}
}

// TestImportBoundary_ASTNotStringMatch proves the scan parses imports
// rather than string-matching source text: a file that merely mentions the
// AWS SDK path in a comment or string literal must NOT be flagged.
func TestImportBoundary_ASTNotStringMatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "internal", "secretstore", "awssm"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "internal", "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := "package other\n\n// see " + awsSDKImportPrefix + "service/secretsmanager\nconst s = \"" + awsSDKImportPrefix + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "internal", "other", "mentions.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := awsSDKImportersIn(t, dir); len(got) != 0 {
		t.Fatalf("expected no offenders from a mere text mention, got %v", got)
	}
}

// TestImportBoundary_CoversSmithyAndV1SDK (S-5): smithy-go and the v1 SDK
// are caught by the widened prefix, and a non-AWS module is not.
func TestImportBoundary_CoversSmithyAndV1SDK(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{filepath.Join("internal", "secretstore", "awssm"), filepath.Join("internal", "a"), filepath.Join("internal", "b"), filepath.Join("internal", "c")} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join("internal", "a", "smithy.go"): "package a\n\nimport _ \"github.com/aws/smithy-go\"\n",
		filepath.Join("internal", "b", "v1.go"):     "package b\n\nimport _ \"github.com/aws/aws-sdk-go/aws\"\n",
		filepath.Join("internal", "c", "other.go"):  "package c\n\nimport _ \"github.com/awslabs/other\"\n",
	}
	for rel, src := range files {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := awsSDKImportersIn(t, dir)
	if len(got) != 2 {
		t.Fatalf("expected exactly the smithy-go and v1 SDK importers, got %v", got)
	}
	for _, g := range got {
		if strings.HasSuffix(g, "other.go") {
			t.Fatalf("a non-AWS module must not be flagged: %v", got)
		}
	}
}
