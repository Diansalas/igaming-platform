package tenant

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ADR 0112 slice 2, security condition C3. The 0129 new-stake backstop
// (ledger_wager_brand_active_guard) is a DEFERRED constraint trigger: it resolves the
// brand from the posting's ledger_entries at COMMIT. Forced IMMEDIATE (`SET CONSTRAINTS
// ALL IMMEDIATE`, or naming the trigger), it would fire at the end of the
// ledger_transactions INSERT, before any entry exists, find no player wallet and pass:
// the backstop would fail OPEN. No non-test source may therefore issue `SET CONSTRAINTS
// ALL` or name ledger_wager_brand_active_guard in a SET CONSTRAINTS statement. Naming
// another constraint (ledger.Post's `SET CONSTRAINTS ledger_entries_balanced ...`) is
// unaffected. Comments are ignored. Heuristic over source text (the same family as
// status_readers_static_test.go): it does not catch SQL assembled by concatenation.

var setConstraintsBanned = regexp.MustCompile(`(?is)\bSET\s+CONSTRAINTS\s+(ALL\b|[^;` + "`" + `]*\bledger_wager_brand_active_guard\b)`)

// setConstraintsViolations returns the code lines of src (Go or SQL) that issue a banned
// SET CONSTRAINTS. Line comments (`//`, `--`) and block-comment continuation lines are skipped.
func setConstraintsViolations(src string) []string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "//") || strings.HasPrefix(trim, "--") || strings.HasPrefix(trim, "*") {
			continue
		}
		if i := strings.Index(trim, "//"); i >= 0 && !strings.Contains(trim[:i], "`") && !strings.Contains(trim[:i], `"`) {
			trim = trim[:i]
		}
		if i := strings.Index(trim, "--"); i >= 0 {
			trim = trim[:i]
		}
		if setConstraintsBanned.MatchString(trim) {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

func TestBrandBackstop_NoSetConstraintsAllOrNamedOutsideTests(t *testing.T) {
	root := repoRoot(t)
	scanned := 0
	for _, dir := range []string{"internal", "cmd", "migrations", "deploy"} {
		base := filepath.Join(root, dir)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			isGo := strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go")
			isSQL := strings.HasSuffix(path, ".sql")
			if !isGo && !isSQL {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scanned++
			rel, _ := filepath.Rel(root, path)
			for _, v := range setConstraintsViolations(string(b)) {
				t.Errorf("%s: SET CONSTRAINTS ALL / naming ledger_wager_brand_active_guard is banned outside tests (ADR 0112 slice 2, C3: it would make the 0129 brand backstop fail open): %s", filepath.ToSlash(rel), v)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d files; the pin would be vacuous", scanned)
	}
}

// Negative test: violating snippets are detected; the allowed forms are not.
func TestBrandBackstop_SetConstraintsPin_NegativeCases(t *testing.T) {
	for name, src := range map[string]string{
		"go ALL IMMEDIATE":      "_, err := tx.Exec(ctx, `SET CONSTRAINTS ALL IMMEDIATE`)",
		"go ALL DEFERRED":       "q := `set constraints all deferred`",
		"named immediate":       "_, err := tx.Exec(ctx, `SET CONSTRAINTS ledger_wager_brand_active_guard IMMEDIATE`)",
		"named in a list":       "_, err := tx.Exec(ctx, `SET CONSTRAINTS ledger_entries_balanced, ledger_wager_brand_active_guard IMMEDIATE`)",
		"sql statement":         "SET CONSTRAINTS ALL IMMEDIATE;",
		"sql schema-qualified":  "SET CONSTRAINTS public.ledger_wager_brand_active_guard IMMEDIATE;",
		"double-quoted go text": `q := "SET CONSTRAINTS ALL IMMEDIATE"`,
	} {
		if len(setConstraintsViolations(src)) == 0 {
			t.Errorf("%s: not detected", name)
		}
	}
	for name, src := range map[string]string{
		"ledger.Post balance check": "if _, err := tx.Exec(ctx, `SET CONSTRAINTS ledger_entries_balanced IMMEDIATE`); err != nil {",
		"go comment":                "// Do NOT issue `SET CONSTRAINTS ALL IMMEDIATE` here",
		"sql comment":               "-- trigger, checked once at commit (or at an explicit SET CONSTRAINTS ALL",
	} {
		if v := setConstraintsViolations(src); len(v) != 0 {
			t.Errorf("%s: false positive %v", name, v)
		}
	}
}
