package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func nonTestGoFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") {
			out = append(out, f)
		}
	}
	if len(out) < 5 {
		t.Fatalf("scanned only %d source files", len(out))
	}
	return out
}

// Nothing in the store logs, prints, reads the environment or the clock, and no error message formats a value:
// secrets, codes and tokens can therefore not reach a log, an audit row or an error string from this package
// (ADR 0119 section 5.4). The clock is injected (`now` parameters), as in internal/staffmfa.
func TestStatic_StoreNeverLogsPrintsOrFormatsValues(t *testing.T) {
	badImports := map[string]bool{"log": true, "log/slog": true, "os": true, "testing": true}
	for _, path := range nonTestGoFiles(t) {
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if badImports[p] {
				t.Errorf("%s imports %q", path, p)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			x, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			switch {
			case x.Name == "time" && sel.Sel.Name == "Now":
				t.Errorf("%s: time.Now (the clock is injected)", path)
			case x.Name == "fmt" && sel.Sel.Name != "Errorf":
				t.Errorf("%s: fmt.%s (only fmt.Errorf with %%w is allowed)", path, sel.Sel.Name)
			case x.Name == "fmt" && sel.Sel.Name == "Errorf":
				lit, ok := call.Args[0].(*ast.BasicLit)
				if !ok {
					t.Errorf("%s: fmt.Errorf with a non-literal format", path)
					return true
				}
				format, _ := strconv.Unquote(lit.Value)
				for _, verb := range regexp.MustCompile(`%[^%]`).FindAllString(format, -1) {
					if verb != "%w" {
						t.Errorf("%s: fmt.Errorf(%q) formats a value with %s; errors here only wrap (%%w)", path, format, verb)
					}
				}
			}
			return true
		})
	}
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// stripSQLComments removes `-- ...` line comments (the migrations use no block comments).
func stripSQLComments(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if i := strings.Index(l, "--"); i >= 0 {
			l = l[:i]
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// runtimeGrants extracts the igaming_runtime grants for the four staff MFA tables from SQL text:
// table -> sorted "TABLE:SELECT,INSERT" plus "UPDATE(cols)".
func runtimeGrants(sql string) map[string]string {
	sql = stripSQLComments(sql)
	out := map[string][]string{}
	tbl := `(staff_mfa_factors|staff_mfa_recovery_codes|staff_auth_challenges|staff_enrolment_tokens)`
	for _, m := range regexp.MustCompile(`GRANT ([A-Z, ]+) ON `+tbl+` TO igaming_runtime`).FindAllStringSubmatch(sql, -1) {
		var ps []string
		for _, p := range strings.Split(m[1], ",") {
			ps = append(ps, strings.TrimSpace(p))
		}
		sort.Strings(ps)
		out[m[2]] = append(out[m[2]], "TABLE:"+strings.Join(ps, ","))
	}
	for _, m := range regexp.MustCompile(`GRANT UPDATE \(([a-z_0-9, ]+)\) ON `+tbl+` TO igaming_runtime`).FindAllStringSubmatch(sql, -1) {
		var cs []string
		for _, c := range strings.Split(m[1], ",") {
			cs = append(cs, strings.TrimSpace(c))
		}
		sort.Strings(cs)
		out[m[2]] = append(out[m[2]], "UPDATE("+strings.Join(cs, ",")+")")
	}
	res := map[string]string{}
	for k, v := range out {
		sort.Strings(v)
		res[k] = strings.Join(v, ";")
	}
	return res
}

// deploy/init-app-role.sql re-asserts exactly the runtime grants migration 0137 makes (the integration suite's
// grant matrix proves the migration's grants against a database).
func TestStatic_InitAppRoleSQLMatchesMigration0137Grants(t *testing.T) {
	want := map[string]string{
		"staff_mfa_factors":        "TABLE:INSERT,SELECT;UPDATE(activated_at,failed_24h_count,failed_24h_start,failed_count,failed_window_start,hard_locked_at,last_used_step,locked_until,pending_expires_at,revoked_at,revoked_reason,status)",
		"staff_mfa_recovery_codes": "TABLE:INSERT,SELECT;UPDATE(consumed_at,revoked_at)",
		"staff_auth_challenges":    "TABLE:INSERT,SELECT;UPDATE(attempts_left,consumed_at)",
		"staff_enrolment_tokens":   "TABLE:INSERT,SELECT;UPDATE(consumed_at,revoked_at)",
	}
	mig := runtimeGrants(readRepoFile(t, "migrations/0137_staff_mfa.up.sql"))
	dep := runtimeGrants(readRepoFile(t, "deploy/init-app-role.sql"))
	for tbl, w := range want {
		if mig[tbl] != w {
			t.Errorf("migration 0137 grants for %s = %q, want %q", tbl, mig[tbl], w)
		}
		if dep[tbl] != w {
			t.Errorf("deploy/init-app-role.sql grants for %s = %q, want %q", tbl, dep[tbl], w)
		}
	}
	if len(mig) != 4 || len(dep) != 4 {
		t.Errorf("unexpected table sets: migration=%v deploy=%v", mig, dep)
	}
	// No DELETE / TRUNCATE / REFERENCES / TRIGGER / ALL / table-level UPDATE grant on these tables, anywhere.
	for name, src := range map[string]string{"migration": readRepoFile(t, "migrations/0137_staff_mfa.up.sql"), "deploy": readRepoFile(t, "deploy/init-app-role.sql")} {
		if regexp.MustCompile(`(?i)GRANT[^;(]*\b(DELETE|TRUNCATE|REFERENCES|TRIGGER|ALL)\b[^;(]*ON\s+(staff_mfa_|staff_auth_challenges|staff_enrolment_tokens)`).MatchString(stripSQLComments(src)) {
			t.Errorf("%s grants a forbidden privilege on a staff MFA table", name)
		}
	}
}

// Migration 0137's scope (ADR 0119 sections 13.1, 20 C1/C5, 16 S2): only the four tables; no sessions column or
// SF040, no staff_users DDL or trigger (0138 / 0139), no SF007 / SF030 (S-H1), no SECURITY DEFINER, every function
// pins its search_path, every SF code is from the S2 set, and the down raises only SF097.
func TestStatic_Migration0137_Scope(t *testing.T) {
	up := stripSQLComments(readRepoFile(t, "migrations/0137_staff_mfa.up.sql"))
	down := stripSQLComments(readRepoFile(t, "migrations/0137_staff_mfa.down.sql"))
	for name, re := range map[string]string{
		"a sessions DDL":              `(?i)ALTER\s+TABLE\s+sessions`,
		"a staff_users DDL":           `(?i)ALTER\s+TABLE\s+staff_users`,
		"a trigger on staff_users":    `(?i)CREATE\s+TRIGGER[^;]*\bON\s+staff_users`,
		"a policy on staff_users":     `(?i)CREATE\s+POLICY[^;]*\bON\s+staff_users`,
		"a SECURITY DEFINER function": `(?i)SECURITY\s+DEFINER`,
		"seeded or backfilled rows":   `(?im)^\s*(INSERT\s+INTO|UPDATE\s+\w+\s+SET|DELETE\s+FROM)\b`,
		"a session cutover table":     `(?i)staff_session_cutover`,
		"a later-slice SQLSTATE":      `SF0(07|1\d|2\d|3\d|4\d)`,
		"a lifecycle table":           `(?i)staff_lifecycle_`,
	} {
		if regexp.MustCompile(re).MatchString(up) {
			t.Errorf("migration 0137 up contains %s (belongs to a later slice or is forbidden)", name)
		}
	}
	// Every function pins search_path; counts: 7 functions, 7 pins.
	if n, m := len(regexp.MustCompile(`(?i)CREATE\s+FUNCTION`).FindAllString(up, -1)), len(regexp.MustCompile(`SET search_path = pg_catalog, public, pg_temp`).FindAllString(up, -1)); n != 7 || m != 7 {
		t.Errorf("functions=%d search_path pins=%d, want 7 and 7", n, m)
	}
	if !strings.HasPrefix(strings.TrimSpace(up), "SET LOCAL lock_timeout = '5s';") {
		t.Error("the up must start with SET LOCAL lock_timeout (0133 / 0136 precedent)")
	}
	codes := map[string]bool{}
	for _, m := range regexp.MustCompile(`ERRCODE\s*=\s*'(SF\d{3})'`).FindAllStringSubmatch(up, -1) {
		codes[m[1]] = true
	}
	for _, c := range []string{"SF001", "SF002", "SF003", "SF004", "SF005", "SF006", "SF098", "SF099"} {
		if !codes[c] {
			t.Errorf("migration 0137 up never raises %s", c)
		}
	}
	if len(codes) != 8 {
		t.Errorf("migration 0137 up raises %v, want exactly SF001-SF006, SF098, SF099", codes)
	}
	if m := regexp.MustCompile(`ERRCODE\s*=\s*'(SF\d{3})'`).FindAllStringSubmatch(down, -1); len(m) != 1 || m[0][1] != "SF097" {
		t.Errorf("the down must raise exactly SF097, got %v", m)
	}
	// FORCE ROW LEVEL SECURITY on every new table; the down lifts it only to count (inside the transaction).
	if n := len(regexp.MustCompile(`FORCE ROW LEVEL SECURITY`).FindAllString(up, -1)); n != 4 {
		t.Errorf("FORCE ROW LEVEL SECURITY statements = %d, want 4", n)
	}
	// Exactly the four tables are created.
	var tables []string
	for _, m := range regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(\w+)`).FindAllStringSubmatch(up, -1) {
		tables = append(tables, m[1])
	}
	sort.Strings(tables)
	if strings.Join(tables, ",") != "staff_auth_challenges,staff_enrolment_tokens,staff_mfa_factors,staff_mfa_recovery_codes" {
		t.Errorf("tables created: %v", tables)
	}
}

// SQLSTATE class SF is used only where the ADR assigns it, in migrations and in Go (ADR 0119 section 13.6): any
// other migration using class SF, or an SF code outside the S2 set, is a collision or an early slice.
func TestStatic_SQLSTATEClassSF_OnlyS2Codes(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	allowed := map[string]bool{"SF001": true, "SF002": true, "SF003": true, "SF004": true, "SF005": true, "SF006": true, "SF097": true, "SF098": true, "SF099": true}
	// ADR 0119 S3 (migration 0138, extended deliberately): exactly these codes, in that migration only.
	allowed0138 := map[string]bool{"'SF008'": true, "'SF040'": true, "'SF041'": true, "'SF042'": true, "'SF099'": true}
	var offenders []string
	scan := func(dir, glob string) {
		files, _ := filepath.Glob(filepath.Join(root, dir, glob))
		for _, f := range files {
			if strings.Contains(f, "0137_staff_mfa") {
				continue
			}
			if strings.Contains(f, "0138_staff_session_cutover") {
				b, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				for _, m := range regexp.MustCompile(`'SF\d{3}'|"SF\d{3}"`).FindAllString(string(b), -1) {
					if !allowed0138[m] {
						offenders = append(offenders, f+": "+m)
					}
				}
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range regexp.MustCompile(`'SF\d{3}'|"SF\d{3}"`).FindAllString(string(b), -1) {
				offenders = append(offenders, f+": "+m)
			}
		}
	}
	scan("migrations", "*.sql")
	for _, d := range []string{"internal/*", "internal/*/*", "cmd/*"} {
		files, _ := filepath.Glob(filepath.Join(root, d, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") || strings.Contains(f, "internal/staffmfa/") {
				continue
			}
			b, _ := os.ReadFile(f)
			for _, m := range regexp.MustCompile(`"SF\d{3}"`).FindAllString(string(b), -1) {
				offenders = append(offenders, f+": "+m)
			}
		}
	}
	if len(offenders) != 0 {
		t.Errorf("SQLSTATE class SF is used outside migrations 0137 / 0138 (S3 codes SF008, SF040-SF042, SF099) / internal/staffmfa (extend this test deliberately when a later slice lands): %v", offenders)
	}
	// The allowed set itself is pinned by the migration static test; here only the closed set is declared.
	if len(allowed) != 9 {
		t.Fatal("allowed SF set changed")
	}
}
