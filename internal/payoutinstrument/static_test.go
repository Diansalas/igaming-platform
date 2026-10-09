package payoutinstrument

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

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

func nonTestGoFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "worktrees", "b2c", "backoffice", "docs":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

var payoutTableWrite = regexp.MustCompile(`(?is)\b(INSERT\s+INTO|UPDATE|DELETE\s+FROM|TRUNCATE(\s+TABLE)?)\s+(payout_instrument\w*|payout_attempt_destination_snapshots)\b`)

// Only internal/payoutinstrument writes any B13 table (L-7: provider
// revocation is blocking-only and written only inside this package; the only
// snapshot writer is Service.WriteSnapshot, called by T1p in B13-B; no
// receipt, phase-C, poll or reconciliation code writes any section 2.1 table).
func TestStatic_OnlyThisPackageWritesPayoutTables(t *testing.T) {
	root := repoRoot(t)
	for _, f := range nonTestGoFiles(t, root) {
		rel, _ := filepath.Rel(root, f)
		if strings.HasPrefix(rel, filepath.Join("internal", "payoutinstrument")+string(filepath.Separator)) {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if loc := payoutTableWrite.FindIndex(b); loc != nil {
			t.Errorf("%s writes a B13 table outside internal/payoutinstrument: %q", rel, string(b[loc[0]:loc[1]]))
		}
	}
}

// The snapshot insert has exactly one call site in this package.
func TestStatic_SingleSnapshotWriter(t *testing.T) {
	re := regexp.MustCompile(`INSERT INTO payout_attempt_destination_snapshots`)
	n := 0
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		n += len(re.FindAll(b, -1))
	}
	if n != 1 {
		t.Fatalf("expected exactly one snapshot INSERT in the package, found %d", n)
	}
}

// Synthetic-marked types perform no external I/O and never wrap a real
// adapter (ADR 0111 2.5): the file declaring a SyntheticComponent method may
// not import network, process, SQL or PSP packages, and no type with that
// method has a field of the verifier interface type.
func TestStatic_SyntheticTypesPerformNoIONorWrapAnAdapter(t *testing.T) {
	fset := token.NewFileSet()
	files, _ := filepath.Glob("*.go")
	banned := []string{"net", "net/http", "os/exec", "database/sql", "github.com/jackc/pgx", "github.com/aws", "crypto/tls", "os"}
	found := false
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		syntheticTypes := map[string]bool{}
		for _, d := range af.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv != nil && fd.Name.Name == "SyntheticComponent" {
				switch rt := fd.Recv.List[0].Type.(type) {
				case *ast.StarExpr:
					if id, ok := rt.X.(*ast.Ident); ok {
						syntheticTypes[id.Name] = true
					}
				case *ast.Ident:
					syntheticTypes[rt.Name] = true
				}
			}
		}
		if len(syntheticTypes) == 0 {
			continue
		}
		found = true
		for _, imp := range af.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			for _, b := range banned {
				if p == b || strings.HasPrefix(p, b+"/") && b != "os" && b != "net" {
					t.Errorf("%s: a Synthetic-marked type's file imports %q", f, p)
				}
			}
		}
		if f != "verifier.go" && f != "tiering.go" {
			t.Errorf("unexpected file with a Synthetic type: %s", f)
		}
		for _, d := range af.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, sp := range gd.Specs {
				ts, ok := sp.(*ast.TypeSpec)
				if !ok || !syntheticTypes[ts.Name.Name] {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				for _, fld := range st.Fields.List {
					if id, ok := fld.Type.(*ast.Ident); ok && (id.Name == "PayoutInstrumentVerifier" || id.Name == "Service") {
						t.Errorf("Synthetic type %s wraps %s", ts.Name.Name, id.Name)
					}
				}
			}
		}
	}
	if !found {
		t.Fatal("expected at least one Synthetic type (the MOCK verifier)")
	}
}

// No in-binary random-key fallback: key material comes only from NewKeys'
// arguments. crypto/rand is used only for the AEAD nonce.
func TestStatic_NoRandomKeyFallback(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		if strings.Contains(string(b), "crypto/rand") && f != "keys.go" {
			t.Errorf("%s imports crypto/rand: random key generation belongs only to the integration-tag test helper", f)
		}
	}
	b, _ := os.ReadFile("keys.go")
	if n := strings.Count(string(b), "rand.Read("); n != 1 {
		t.Errorf("keys.go must use rand.Read exactly once (the nonce), found %d", n)
	}
}

// Migration 0123 has no SECURITY DEFINER and no FOR ALL policy, and the
// literal MOCK provider-id set equals MockProviderIDs.
func TestStatic_MigrationShape(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "migrations", "0123_payout_instruments.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	up := string(b)
	// Strip comments before matching.
	var sb strings.Builder
	for _, line := range strings.Split(up, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		sb.WriteString(line + "\n")
	}
	code := strings.ToUpper(sb.String())
	if strings.Contains(code, "SECURITY DEFINER") {
		t.Error("0123 must not use SECURITY DEFINER")
	}
	if strings.Contains(code, "FOR ALL") {
		t.Error("0123 must not create a FOR ALL policy")
	}
	m := regexp.MustCompile(`mock_ids CONSTANT TEXT\[\] := ARRAY\[([^\]]*)\]`).FindStringSubmatch(up)
	if m == nil {
		t.Fatal("literal MOCK provider-id set not found in 0123")
	}
	var ids []string
	for _, s := range strings.Split(m[1], ",") {
		ids = append(ids, strings.Trim(strings.TrimSpace(s), "'"))
	}
	if strings.Join(ids, ",") != strings.Join(MockProviderIDs, ",") {
		t.Errorf("SQL MOCK ids %v != Go MockProviderIDs %v", ids, MockProviderIDs)
	}
	// Every function 0123 creates pins its search_path.
	fns := regexp.MustCompile(`(?is)CREATE (?:OR REPLACE )?FUNCTION (\w+)\(\).*?\$\$ LANGUAGE plpgsql(.*?);`).FindAllStringSubmatch(up, -1)
	for _, f := range fns {
		if f[1] == "withdrawal_requests_enforce_immutable_fields" {
			continue // byte-for-byte the 0026 shape plus two columns (no search_path in 0026)
		}
		if !strings.Contains(f[2], "search_path") {
			t.Errorf("function %s does not pin search_path", f[1])
		}
	}
}
