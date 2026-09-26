package awssm

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/secretstore"
)

// TestNewWithSDKFake_RunsPreflightAndFailsClosed: the test constructor
// applies New's refusals and its store serves nothing.
func TestNewWithSDKFake_RunsPreflightAndFailsClosed(t *testing.T) {
	staging := permittedEnv(t)
	if _, err := NewWithSDKFake(context.Background(), config.Config{Environment: "development", EnvironmentExplicit: true}, "eu-west-1"); err == nil {
		t.Fatal("the environment allow-list must apply")
	}
	if _, err := NewWithSDKFake(context.Background(), staging, ""); err == nil {
		t.Fatal("an explicit region must be required")
	}
	t.Setenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "")
	if _, err := NewWithSDKFake(context.Background(), staging, "eu-west-1"); err == nil {
		t.Fatal("the container-credential endpoint must be required")
	}
	t.Setenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", testRelativeURI)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE-not-a-real-key")
	if _, err := NewWithSDKFake(context.Background(), staging, "eu-west-1"); err == nil || !strings.Contains(err.Error(), "AWS_ACCESS_KEY_ID") {
		t.Fatalf("static credentials must be refused, got %v", err)
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	s, err := NewWithSDKFake(context.Background(), staging, "eu-west-1")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := secretstore.ParseRef("awssm://" + testARNPrefix + "/provider-creds/" + uuid.NewString() + "/casino/acme/n?versionId=" + strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(context.Background(), ref); secretstore.ClassOf(err) != secretstore.ClassNotFound {
		t.Fatalf("the SDK fake must serve nothing (not_found), got %v", err)
	}
}

// TestNewWithSDKFake_OnlyFromTests: no non-test .go file outside this
// package may call NewWithSDKFake.
func TestNewWithSDKFake_OnlyFromTests(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			n := d.Name()
			if n == "vendor" || n == "testdata" || n == "node_modules" || (strings.HasPrefix(n, ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if rel == filepath.Join("internal", "secretstore", "awssm", "sdkfake.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", rel, perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if x.Sel.Name == "NewWithSDKFake" {
					t.Errorf("%s calls awssm.NewWithSDKFake outside a _test.go file", rel)
				}
			case *ast.Ident:
				if x.Name == "NewWithSDKFake" && f.Name.Name == "awssm" {
					t.Errorf("%s calls NewWithSDKFake outside a _test.go file", rel)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
