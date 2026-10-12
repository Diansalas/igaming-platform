package ledger

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

// ADR 0113 slice 1 (ledger-finance condition): WD043 (migration 0132, trigger
// ledger_transactions_withdrawal_hold_leg_at_commit) requires a player_withdrawal_hold leg on the
// withdrawal_* transaction types it LISTS EXPLICITLY. A new withdrawal_* TransactionType is not caught
// automatically, so adding one must fail this test until a WD043 decision is recorded here and the
// latest definition of the trigger function names it (or the decision says why it is exempt).
//
// wd043Decided maps every withdrawal_* TransactionType to its WD043 decision.
var wd043Decided = map[string]string{
	"withdrawal_requested": "hold leg required (Cr player_withdrawal_hold)",
	"withdrawal_rejected":  "hold leg required (Dr player_withdrawal_hold, release)",
	"withdrawal_failed":    "hold leg required (Dr player_withdrawal_hold, release)",
	"withdrawal_completed": "hold leg required (Dr player_withdrawal_hold, release)",
	"withdrawal_reversed":  "hold leg required; no writer, refused end to end by B-i (WITHDRAWAL-REVERSAL-1 needs a new migration)",
}

// withdrawalTxTypeConstants returns the values of the TransactionType constants of this package
// (non-test files) that start with "withdrawal_".
func withdrawalTxTypeConstants(t *testing.T, dir string) []string {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range af.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, s := range gd.Specs {
				vs := s.(*ast.ValueSpec)
				id, ok := vs.Type.(*ast.Ident)
				if !ok || id.Name != "TransactionType" {
					continue
				}
				for _, v := range vs.Values {
					lit, ok := v.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					s, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					if strings.HasPrefix(s, "withdrawal_") {
						out = append(out, s)
					}
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// latestWD043Body returns the body of the LAST migration that (re)defines the WD043 trigger function.
func latestWD043Body(t *testing.T, migrations string) (string, string) {
	t.Helper()
	ups, err := filepath.Glob(filepath.Join(migrations, "*.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(ups)
	def := regexp.MustCompile(`(?is)CREATE\s+(OR\s+REPLACE\s+)?FUNCTION\s+(public\.)?ledger_transactions_withdrawal_hold_leg_at_commit\s*\(\s*\).*?\$\$(.*?)\$\$`)
	var body, file string
	for _, f := range ups {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range def.FindAllStringSubmatch(string(b), -1) {
			body, file = m[3], filepath.Base(f)
		}
	}
	if body == "" {
		t.Fatal("no migration defines ledger_transactions_withdrawal_hold_leg_at_commit (WD043)")
	}
	return body, file
}

func TestWD043_EveryWithdrawalTxTypeHasAHoldLegDecision(t *testing.T) {
	got := withdrawalTxTypeConstants(t, ".")
	if len(got) == 0 {
		t.Fatal("no withdrawal_* TransactionType constant found: the pin would be vacuous")
	}
	for _, typ := range got {
		if _, ok := wd043Decided[typ]; !ok {
			t.Errorf("new withdrawal TransactionType %q has no WD043 decision: a withdrawal_* posting without a "+
				"player_withdrawal_hold leg is a double-pay shape (ADR 0113 residual 9.2). Add it to the WD043 trigger in a NEW "+
				"migration (or record why it is exempt) with ledger-finance sign-off, then add it to wd043Decided", typ)
		}
	}
	for typ := range wd043Decided {
		if !contains(got, typ) {
			t.Errorf("wd043Decided lists %q, which is no longer a TransactionType constant", typ)
		}
	}
	body, file := latestWD043Body(t, filepath.Join("..", "..", "migrations"))
	for _, typ := range got {
		if !strings.Contains(body, "'"+typ+"'") {
			t.Errorf("%s: the WD043 trigger function does not list %q", file, typ)
		}
	}
}

// Non-vacuity: a new constant in a scanned file is reported.
func TestWD043_Pin_IsNotVacuous(t *testing.T) {
	dir := t.TempDir()
	src := "package ledger\n\ntype TransactionType string\n\nconst (\n\tTxA TransactionType = \"withdrawal_requested\"\n\tTxB TransactionType = \"withdrawal_clawback\"\n\tTxC TransactionType = \"deposit\"\n)\n"
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	got := withdrawalTxTypeConstants(t, dir)
	if strings.Join(got, ",") != "withdrawal_clawback,withdrawal_requested" {
		t.Fatalf("scan: %v", got)
	}
	if _, ok := wd043Decided["withdrawal_clawback"]; ok {
		t.Fatal("fixture type must be undecided")
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
