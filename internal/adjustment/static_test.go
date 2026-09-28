package adjustment

// Static guards for PRH-2 K2 (ADR 0100 §11; ADR 0099 §6.1/§6.8; security
// K2-P3). Each walks the real repository source (skipping ".", "_" and
// testdata directories, exactly like the go tool) and each has a planted
// negative control, so a silently broken scan can never pass by scanning
// nothing.

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

func walkGo(t *testing.T, root string, visit func(path string, src []byte)) int {
	t.Helper()
	n := 0
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		base := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") || base == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		n++
		visit(path, src)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("scanned zero .go files - the walk is broken")
	}
	return n
}

var txManualAdjustmentRef = regexp.MustCompile(`\bTxManualAdjustment\b`)

// B-21 (C-100-4): in non-test Go, ledger.TxManualAdjustment appears only in
// internal/ledger (the constant and its validation) and internal/adjustment
// (the single governed caller).
func TestB21_TxManualAdjustmentOnlyInLedgerAndAdjustment(t *testing.T) {
	root := repoRoot(t)
	allowed := []string{
		filepath.Join(root, "internal", "ledger") + string(filepath.Separator),
		filepath.Join(root, "internal", "adjustment") + string(filepath.Separator),
	}
	var bad []string
	walkGo(t, root, func(path string, src []byte) {
		if strings.HasSuffix(path, "_test.go") || !txManualAdjustmentRef.Match(src) {
			return
		}
		for _, a := range allowed {
			if strings.HasPrefix(path, a) {
				return
			}
		}
		bad = append(bad, path)
	})
	if len(bad) > 0 {
		t.Fatalf("B-21: ledger.TxManualAdjustment used outside internal/ledger and internal/adjustment:\n  %s", strings.Join(bad, "\n  "))
	}
}

func TestB21_GuardCatchesPlant(t *testing.T) {
	if !txManualAdjustmentRef.MatchString(`in := ledger.TransactionInput{TransactionType: ledger.TxManualAdjustment}`) {
		t.Fatal("planted use not detected")
	}
	if txManualAdjustmentRef.MatchString(`ledger.TxManualAdjustmentX`) {
		t.Fatal("a different identifier was flagged")
	}
}

var (
	fromStaffOrPlayers = regexp.MustCompile(`(?i)\bFROM\s+(staff_users|player_accounts)\b`)
	selectKeyword      = regexp.MustCompile(`(?i)\bSELECT\b`)
	staffAllowedCols   = map[string]bool{"id": true, "tenant_id": true, "role": true, "status": true, "person_id": true}
	playerAllowedCols  = map[string]bool{"id": true, "tenant_id": true, "person_id": true, "status": true}
)

// a19Violations applies ADR 0099 §6.8 / A-19 column discipline to a
// source text: for every "FROM staff_users|player_accounts", the SELECT
// list is the text between the NEAREST preceding SELECT keyword and that
// FROM; staff_users may select only id, tenant_id, role, status,
// person_id; player_accounts only id, tenant_id, person_id, status; never
// "*".
func a19Violations(src string) []string {
	var out []string
	for _, loc := range fromStaffOrPlayers.FindAllStringSubmatchIndex(src, -1) {
		table := src[loc[2]:loc[3]]
		before := src[:loc[0]]
		sels := selectKeyword.FindAllStringIndex(before, -1)
		if len(sels) == 0 {
			continue
		}
		cols := before[sels[len(sels)-1][1]:]
		allowed := staffAllowedCols
		if table == "player_accounts" {
			allowed = playerAllowedCols
		}
		for _, c := range strings.Split(cols, ",") {
			c = strings.TrimSpace(c)
			if i := strings.LastIndex(c, "."); i >= 0 {
				c = c[i+1:]
			}
			if c == "count(*)" || c == "1" {
				continue
			}
			if !allowed[c] {
				out = append(out, table+": "+c)
			}
		}
	}
	return out
}

// B-22 (security confirmation C-2; A-19 via B-22): column discipline for
// every SQL literal in internal/adjustment's non-test Go.
func TestB22_A19ColumnDisciplineInAdjustment(t *testing.T) {
	root := filepath.Join(repoRoot(t), "internal", "adjustment")
	var bad []string
	scanned := walkGo(t, root, func(path string, src []byte) {
		if strings.HasSuffix(path, "_test.go") {
			return
		}
		for _, v := range a19Violations(string(src)) {
			bad = append(bad, path+": "+v)
		}
	})
	if scanned < 3 {
		t.Fatalf("expected to scan the package's sources, scanned %d", scanned)
	}
	if len(bad) > 0 {
		t.Fatalf("B-22/A-19 column discipline violated:\n  %s", strings.Join(bad, "\n  "))
	}
}

func TestB22_GuardCatchesPlants(t *testing.T) {
	for _, plant := range []string{
		"SELECT id, password_hash FROM staff_users WHERE id = $1",
		"SELECT * FROM staff_users",
		"select email, id from player_accounts",
	} {
		if len(a19Violations(plant)) == 0 {
			t.Fatalf("planted violation not detected: %s", plant)
		}
	}
	if v := a19Violations("SELECT id, tenant_id, role, status, person_id FROM staff_users WHERE id = ANY($1)"); len(v) != 0 {
		t.Fatalf("a compliant select was flagged: %v", v)
	}
}

// K2-G4 / security K2-P3 / ADR 0099 §6.1: the call-site pin for the first
// WithPlatformActingInTenant caller. In internal/adjustment there is
// exactly ONE call; its principal argument is an identifier assigned from
// uuid.Parse(tc.Subject) where tc comes from tenant.FromContext(ctx) in
// the SAME function; its target argument is target.tenantID, a field of a
// Target that only NewTarget (which applies the canActOnTenant rule)
// constructs; and the HTTP layer builds Targets only from the route's
// {tenantID} path value.
func TestK2P3_ActingSetterCallSitePinned(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "internal", "adjustment")
	fset := token.NewFileSet()
	var calls []*ast.CallExpr
	var enclosing []*ast.FuncDecl
	targetLiterals := 0
	walkGo(t, dir, func(path string, src []byte) {
		if strings.HasSuffix(path, "_test.go") {
			return
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "WithPlatformActingInTenant" {
						calls = append(calls, x)
						enclosing = append(enclosing, fd)
					}
				case *ast.CompositeLit:
					if id, ok := x.Type.(*ast.Ident); ok && id.Name == "Target" && fd.Name.Name != "NewTarget" {
						targetLiterals++
					}
				}
				return true
			})
		}
	})
	if len(calls) != 1 {
		t.Fatalf("expected exactly one WithPlatformActingInTenant call in internal/adjustment, found %d", len(calls))
	}
	if targetLiterals != 0 {
		t.Fatalf("a Target is constructed outside NewTarget (%d literal(s))", targetLiterals)
	}
	call, fd := calls[0], enclosing[0]
	if len(call.Args) < 3 {
		t.Fatalf("unexpected setter call shape")
	}
	principal, ok := call.Args[1].(*ast.Ident)
	if !ok {
		t.Fatalf("principal argument is not a plain identifier")
	}
	tgt, ok := call.Args[2].(*ast.SelectorExpr)
	if !ok || tgt.Sel.Name != "tenantID" {
		t.Fatalf("target argument must be <Target>.tenantID")
	}
	if xid, ok := tgt.X.(*ast.Ident); !ok || !paramHasType(fd, xid.Name, "Target") {
		t.Fatalf("target argument's receiver must be a Target parameter of the enclosing function")
	}
	// principal := uuid.Parse(<tc>.Subject), <tc> := tenant.FromContext(ctx)
	var tcName string
	var subjectFromTC bool
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, _ := sel.X.(*ast.Ident)
		if pkg != nil && pkg.Name == "tenant" && sel.Sel.Name == "FromContext" {
			if id, ok := as.Lhs[0].(*ast.Ident); ok {
				tcName = id.Name
			}
		}
		if pkg != nil && pkg.Name == "uuid" && sel.Sel.Name == "Parse" && len(call.Args) == 1 {
			if id, ok := as.Lhs[0].(*ast.Ident); ok && id.Name == principal.Name {
				if arg, ok := call.Args[0].(*ast.SelectorExpr); ok && arg.Sel.Name == "Subject" {
					if x, ok := arg.X.(*ast.Ident); ok && x.Name == tcName && tcName != "" {
						subjectFromTC = true
					}
				}
			}
		}
		return true
	})
	if !subjectFromTC {
		t.Fatalf("the setter's principal (%s) is not parsed from tenant.FromContext(ctx).Subject in %s", principal.Name, fd.Name.Name)
	}

	// NewTarget applies the canActOnTenant rule.
	src, err := os.ReadFile(filepath.Join(dir, "adjustment.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "if tc.TenantID != uuid.Nil && tc.TenantID != pathTenantID {") {
		t.Fatal("NewTarget no longer applies the canActOnTenant rule")
	}
	// The HTTP layer builds Targets only from the {tenantID} path value.
	routes, err := os.ReadFile(filepath.Join(root, "internal", "httpserver", "manual_adjustment_routes.go"))
	if err != nil {
		t.Fatal(err)
	}
	newTarget := regexp.MustCompile(`adjustment\.NewTarget\(([^)]*)\)`).FindAllStringSubmatch(string(routes), -1)
	if len(newTarget) == 0 {
		t.Fatal("manual_adjustment_routes.go never calls adjustment.NewTarget")
	}
	for _, m := range newTarget {
		if strings.TrimSpace(m[1]) != "c.tc, c.target" {
			t.Fatalf("adjustment.NewTarget called with %q; must be (c.tc, c.target)", m[1])
		}
	}
	if !strings.Contains(string(routes), `uuid.Parse(r.PathValue("tenantID"))`) || !strings.Contains(string(routes), "canActOnTenant(tc, target)") {
		t.Fatal("the route target is not the canActOnTenant-validated {tenantID} path value")
	}
	if strings.Contains(string(routes), "WithPlatformActingInTenant") {
		t.Fatal("the HTTP layer must not call the acting setter directly")
	}
}

func paramHasType(fd *ast.FuncDecl, name, typ string) bool {
	for _, f := range fd.Type.Params.List {
		id, ok := f.Type.(*ast.Ident)
		if !ok || id.Name != typ {
			continue
		}
		for _, n := range f.Names {
			if n.Name == name {
				return true
			}
		}
	}
	return false
}
