// ADR 0088 §5.3/§14 (INV-LOCK-E4, Q1): sportsbook_bet_settlements and
// sportsbook_bets.status have exactly the writers the contract names.
// Deliberately NOT behind the `integration` build tag and requiring no
// database - like internal/ledger/lockorder_static_test.go, a rule that
// only gets checked when someone remembers `-tags=integration` is not a
// standing invariant.
package sportsbook

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// swInternalDir is the tree this guard walks, relative to this package's
// own source directory (mirrors internal/ledger/lockorder_static_test.go's
// loInternalDir).
const swInternalDir = ".."

// swInsertSettlements matches an INSERT into sportsbook_bet_settlements.
var swInsertSettlements = regexp.MustCompile(`(?is)\binsert\s+into\s+sportsbook_bet_settlements\b`)

// swUpdateBetStatus matches an UPDATE of sportsbook_bets.status - written
// permissively (SET status = ... anywhere in the SET list, not
// necessarily first) since insertBet's own INSERT never matches this
// (it is an INSERT, not an UPDATE).
var swUpdateBetStatus = regexp.MustCompile(`(?is)\bupdate\s+(only\s+)?sportsbook_bets\b[\s\S]*?\bset\b[\s\S]*?\bstatus\s*=`)

// swAllowedFile/Funcs: the ONLY file and functions permitted to contain
// either statement (ADR 0088 §5.3's sole-writer statement, §14 Q1).
const swAllowedFile = "settlement.go"

var swAllowedFuncs = map[string]bool{
	"insertSettlementRecord": true, // sportsbook_bet_settlements INSERT
	"updateBetStatus":        true, // sportsbook_bets.status UPDATE
}

// swCatalogueTables are the tables INV-SB-SETTLE-6 forbids settlement.go
// from ever naming (ADR 0047 §5(c): settlement never reads or locks the
// catalogue).
var swCatalogueTables = []string{"sb_selections", "sb_markets", "sb_events"}

type swSource struct {
	path  string
	lits  []swLit
	funcs []swFunc
}

type swLit struct {
	offset int
	value  string
}

type swFunc struct {
	name       string
	start, end int
}

func swParse(t *testing.T, path, src string) swSource {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := swSource{path: path}
	off := func(p token.Pos) int { return fset.Position(p).Offset }

	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		out.funcs = append(out.funcs, swFunc{name: fd.Name.Name, start: off(fd.Body.Pos()), end: off(fd.Body.End())})
	}

	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				if s, uerr := strconv.Unquote(v.Value); uerr == nil {
					out.lits = append(out.lits, swLit{offset: off(v.Pos()), value: s})
				}
			}
		case *ast.BinaryExpr:
			if v.Op == token.ADD {
				out.lits = append(out.lits, swLit{offset: off(v.Pos()), value: swFoldConcat(v)})
			}
		}
		return true
	})
	return out
}

// swDynamicOperand mirrors internal/ledger's loDynamicOperand.
const swDynamicOperand = "\x00"

func swFoldConcat(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			if s, err := strconv.Unquote(v.Value); err == nil {
				return s
			}
		}
		return swDynamicOperand
	case *ast.BinaryExpr:
		if v.Op == token.ADD {
			return swFoldConcat(v.X) + swFoldConcat(v.Y)
		}
		return swDynamicOperand
	case *ast.ParenExpr:
		return swFoldConcat(v.X)
	default:
		return swDynamicOperand
	}
}

func swWalkGoFiles(t *testing.T, root string, visit func(path, src string)) {
	t.Helper()
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("guard root %s is not walkable: %v", root, err)
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		visit(filepath.ToSlash(path), string(b))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// funcContainingOffset returns the innermost top-level function whose body
// contains offset, or "" (package scope / var initializer) if none does.
func funcContainingOffset(funcs []swFunc, offset int) string {
	for _, f := range funcs {
		if offset >= f.start && offset < f.end {
			return f.name
		}
	}
	return ""
}

// TestSoleWriter_SportsbookBetSettlementsAndBetStatus is INV-LOCK-E4's
// static guard (ADR 0088 §5.3, §14 Q1): only internal/sportsbook/
// settlement.go may contain `INSERT INTO sportsbook_bet_settlements` or
// `UPDATE sportsbook_bets SET status`, and within that file only
// insertSettlementRecord/updateBetStatus may.
func TestSoleWriter_SportsbookBetSettlementsAndBetStatus(t *testing.T) {
	var offenders []string
	sawInsert, sawUpdate := false, false

	swWalkGoFiles(t, swInternalDir, func(path, src string) {
		parsed := swParse(t, path, src)
		base := filepath.Base(path)
		inSportsbookPkg := strings.Contains(path, "/sportsbook/")

		for _, l := range parsed.lits {
			isInsert := swInsertSettlements.MatchString(l.value)
			isUpdate := swUpdateBetStatus.MatchString(l.value)
			if !isInsert && !isUpdate {
				continue
			}
			if isInsert {
				sawInsert = true
			}
			if isUpdate {
				sawUpdate = true
			}
			if !inSportsbookPkg || base != swAllowedFile {
				offenders = append(offenders, path+": contains a statement only "+swAllowedFile+" may contain: "+strings.TrimSpace(l.value))
				continue
			}
			fn := funcContainingOffset(parsed.funcs, l.offset)
			if !swAllowedFuncs[fn] {
				offenders = append(offenders, path+": function "+fn+" contains a statement only insertSettlementRecord/updateBetStatus may contain: "+strings.TrimSpace(l.value))
			}
		}
	})

	if !sawInsert {
		t.Fatal("this guard found NO `INSERT INTO sportsbook_bet_settlements` anywhere in the tree - " +
			"either the tree moved or the walk is broken, and a guard that inspects nothing proves nothing")
	}
	if !sawUpdate {
		t.Fatal("this guard found NO `UPDATE sportsbook_bets ... SET status` anywhere in the tree - " +
			"either the tree moved or the walk is broken")
	}
	if len(offenders) > 0 {
		t.Fatalf("INV-LOCK-E4 violated (ADR 0088 §5.3) - sportsbook_bet_settlements and sportsbook_bets.status "+
			"may be written ONLY by settlement.go's insertSettlementRecord/updateBetStatus:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// TestSoleWriter_SettlementNeverReferencesCatalogueTables is
// INV-SB-SETTLE-6 (ADR 0088 §7, §5.4): settlement.go must never read or
// lock sb_selections/sb_markets/sb_events.
func TestSoleWriter_SettlementNeverReferencesCatalogueTables(t *testing.T) {
	path := filepath.Join(swInternalDir, "sportsbook", "settlement.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	parsed := swParse(t, filepath.ToSlash(path), string(src))
	for _, l := range parsed.lits {
		for _, table := range swCatalogueTables {
			if strings.Contains(l.value, table) {
				t.Fatalf("INV-SB-SETTLE-6 violated: settlement.go's SQL references %q: %s", table, strings.TrimSpace(l.value))
			}
		}
	}
}
