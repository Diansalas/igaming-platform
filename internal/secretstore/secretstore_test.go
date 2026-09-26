package secretstore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/secretstore"
	"github.com/Diansalas/igaming-platform/internal/secretstore/memstore"
)

func TestStoreRef_ParseMirrorsMigrationChecks(t *testing.T) {
	tenant := uuid.New()
	ns := fmt.Sprintf("/provider-creds/%s/casino/acme/name", tenant)
	const arn = "arn:aws:secretsmanager:eu-west-1:123456789012:secret:igaming"
	good := []string{
		"memory://v" + ns + "?version=v1",
		"memory://v" + ns,
		"devfile:/" + ns + "?version=2026-09",
		"awssm://" + arn + ns + "?versionId=" + uuid.NewString(),
		"awssm://" + arn + ns + "-AbCdEf?versionId=" + uuid.NewString() + "#apiKey",
		"awssm://arn:aws-us-gov:secretsmanager:us-gov-west-1:123456789012:secret:igaming" + ns + "?versionId=" + uuid.NewString(),
	}
	for _, raw := range good {
		ref, err := secretstore.ParseRef(raw)
		if err != nil {
			t.Fatalf("%q must parse: %v", raw, err)
		}
		if !ref.InNamespace(tenant, "casino", "acme") {
			t.Fatalf("%q must be in the tenant namespace", raw)
		}
		if ref.InNamespace(uuid.New(), "casino", "acme") || ref.InNamespace(tenant, "kyc", "acme") || ref.InNamespace(tenant, "casino", "other") {
			t.Fatalf("%q must not be in a foreign namespace", raw)
		}
	}
	bad := []string{
		"", "file:/" + ns, "awssm://" + arn + ns, "awssm://" + arn + ns + "?versionId=AWSCURRENT",
		"awssm://" + arn + ns + "?versionStage=AWSCURRENT",
		// S-2 (gate W2/W3): a bare secret NAME is refused - the ref must be
		// a full ARN pinning partition, region and account.
		"awssm://prefix" + ns + "?versionId=" + uuid.NewString(),
		"awssm://arn:aws:secretsmanager:eu-west-1:12345678901:secret:igaming" + ns + "?versionId=" + uuid.NewString(),
		"awssm://arn:aws:secretsmanager:eu-west-1:123456789012:igaming" + ns + "?versionId=" + uuid.NewString(),
		"awssm://arn:aws:s3:eu-west-1:123456789012:secret:igaming" + ns + "?versionId=" + uuid.NewString(),
		"awssm://arn:evil:secretsmanager:eu-west-1:123456789012:secret:igaming" + ns + "?versionId=" + uuid.NewString(),
		"awssm://arn:aws:secretsmanager::123456789012:secret:igaming" + ns + "?versionId=" + uuid.NewString(),
		"awssm://arn:aws:secretsmanager:eu-west-1:123456789012:secret:a:b" + ns + "?versionId=" + uuid.NewString(),
		"devfile:/" + ns, "devfile:/" + ns + "?version=1#x",
		"memory://v" + ns + "?a=b", "memory://v" + ns + " ", "memory://v" + ns + "\n",
		"memory://v" + ns + "?version=1?version=2", "memory://" + strings.Repeat("a", 600),
	}
	for _, raw := range bad {
		if _, err := secretstore.ParseRef(raw); secretstore.ClassOf(err) != secretstore.ClassInvalidRef {
			t.Fatalf("%q must be refused, got %v", raw, err)
		}
	}
	for name, raw := range map[string]string{
		"two markers":   fmt.Sprintf("memory://x/provider-creds/a/provider-creds/%s/casino/acme/name", tenant),
		"overlap":       fmt.Sprintf("memory://x/provider-creds/provider-creds/%s/casino/acme/name", tenant),
		"dot":           fmt.Sprintf("memory://x/provider-creds/%s/casino/acme/.", tenant),
		"dotdot":        fmt.Sprintf("memory://x/provider-creds/%s/casino/acme/..", tenant),
		"five segments": fmt.Sprintf("memory://x/provider-creds/%s/casino/acme/a/b", tenant),
		"in query":      fmt.Sprintf("memory://x/y?version=/provider-creds/%s/casino/acme/name", tenant),
		"no namespace":  "memory://x/y",
	} {
		ref, err := secretstore.ParseRef(raw)
		if err == nil && ref.InNamespace(tenant, "casino", "acme") {
			t.Fatalf("%s: %q must not be in the namespace", name, raw)
		}
	}
}

// TestStoreRouter_OnlyAllowListedSchemes: NewRouter admits a backend only
// if config.ValidateSecretBackendScheme permits it (security review §4.1
// point 1); memory:// is refused in every environment.
func TestStoreRouter_OnlyAllowListedSchemes(t *testing.T) {
	dev := config.Config{Environment: "development", EnvironmentExplicit: true}
	prod := config.Config{Environment: "production", EnvironmentExplicit: true}
	missing := config.Config{Environment: "development", EnvironmentExplicit: false}
	mem := memstore.New()
	for name, cfg := range map[string]config.Config{"development": dev, "production": prod, "missing APP_ENV": missing} {
		if _, err := secretstore.NewRouter(cfg, mem); err == nil {
			t.Fatalf("%s: memory:// must never be routable through NewRouter", name)
		}
	}
	devfileLike := schemeOnly("devfile")
	if _, err := secretstore.NewRouter(dev, devfileLike); err != nil {
		t.Fatalf("devfile must be routable in explicit development: %v", err)
	}
	for name, cfg := range map[string]config.Config{"production": prod, "missing APP_ENV": missing,
		"staging": {Environment: "staging", EnvironmentExplicit: true}} {
		if _, err := secretstore.NewRouter(cfg, devfileLike); err == nil {
			t.Fatalf("%s: devfile must be refused", name)
		}
	}
	if _, err := secretstore.NewRouter(prod, schemeOnly("awssm")); err != nil {
		t.Fatalf("awssm must be routable in explicit production: %v", err)
	}
	if _, err := secretstore.NewRouter(dev, schemeOnly("awssm")); err == nil {
		t.Fatal("awssm is refused in development by the W1b rule")
	}
	if _, err := secretstore.NewRouter(dev, schemeOnly("file")); err == nil {
		t.Fatal("an unknown scheme must be refused")
	}
	r, err := secretstore.NewRouter(dev, devfileLike)
	if err != nil || !r.Validated() {
		t.Fatal("a NewRouter router is validated")
	}
	if r, _ := memstore.NewRouter(mem); r.Validated() {
		t.Fatal("the test-only router is not validated")
	}
}

type schemeOnly string

func (s schemeOnly) Scheme() string { return string(s) }
func (schemeOnly) Get(_ context.Context, _ secretstore.Ref) (secretstore.Secret, error) {
	return secretstore.Secret{}, secretstore.NewError(secretstore.ClassNotFound)
}

// TestSecretStore_Redaction (part of TestSecretTypes_Redaction): a store
// result never renders its bytes.
func TestSecretStore_Redaction(t *testing.T) {
	raw := randBytes(t, 32)
	s := secretstore.NewSecret(raw)
	assertRedacted(t, s, raw)
}

func assertRedacted(t *testing.T, v any, raw []byte) {
	t.Helper()
	forms := []string{string(raw), fmt.Sprintf("%x", raw), fmt.Sprintf("%X", raw)}
	var outs []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%q"} {
		outs = append(outs, fmt.Sprintf(verb, v))
	}
	var sb strings.Builder
	slog.New(slog.NewTextHandler(&sb, nil)).Info("m", "v", v)
	slog.New(slog.NewJSONHandler(&sb, nil)).Info("m", "v", v)
	outs = append(outs, sb.String())
	j, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	outs = append(outs, string(j))
	for _, out := range outs {
		for _, form := range forms {
			if strings.Contains(out, form) {
				t.Fatalf("%T rendered its secret: %q", v, out)
			}
		}
	}
}

// repoRoot walks up to the module root.
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

// nonTestGoFiles returns every non-test .go file in the module, skipping
// testdata, hidden directories and nested worktrees.
func nonTestGoFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

const memstoreImport = "github.com/Diansalas/igaming-platform/internal/secretstore/memstore"

// memstoreImportersIn returns every non-test file under root importing
// the memory backend.
func memstoreImportersIn(t *testing.T, root string) []string {
	t.Helper()
	fset := token.NewFileSet()
	var offenders []string
	for _, path := range nonTestGoFiles(t, root) {
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == memstoreImport {
				offenders = append(offenders, path)
			}
		}
	}
	return offenders
}

// TestSecretStore_MemstoreImportedOnlyByTests (CI; security review §4.1):
// the memory:// backend may be imported only from _test.go files.
func TestSecretStore_MemstoreImportedOnlyByTests(t *testing.T) {
	if offenders := memstoreImportersIn(t, repoRoot(t)); len(offenders) != 0 {
		t.Fatalf("non-test files import the test-only memory backend: %v", offenders)
	}
}

// TestSecretStore_MemstoreCheckCatchesOffender is the negative control: a
// non-test file importing memstore in a temp tree is found.
func TestSecretStore_MemstoreCheckCatchesOffender(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := "package x\n\nimport _ \"" + memstoreImport + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "bad.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ok_test.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	got := memstoreImportersIn(t, dir)
	if len(got) != 1 || !strings.HasSuffix(got[0], "bad.go") {
		t.Fatalf("expected exactly bad.go, got %v", got)
	}
}

// TestSecretStore_UnvalidatedRouterOnlyFromTestSupport: the allow-list
// bypass NewRouterUnvalidated may be called only from _test.go files and
// the (itself test-only) memstore package.
func TestSecretStore_UnvalidatedRouterOnlyFromTestSupport(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	for _, path := range nonTestGoFiles(t, root) {
		rel, _ := filepath.Rel(root, path)
		if strings.HasPrefix(rel, filepath.Join("internal", "secretstore", "memstore")+string(filepath.Separator)) ||
			rel == filepath.Join("internal", "secretstore", "router.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewRouterUnvalidated" {
				t.Errorf("%s uses secretstore.NewRouterUnvalidated outside test support", rel)
			}
			return true
		})
	}
}
