// ADR 0082 §6 tests 9 and 10 - the two STATIC regression guards, and the
// only tests in this dispatch that prevent the ADR from decaying.
//
// Deliberately NOT behind the `integration` build tag and deliberately
// requiring no database: a rule that only gets checked when someone
// remembers to run the integration suite against a live Postgres is not a
// standing invariant, it is a suggestion. These run on every `go test
// ./...`.
//
// Both guards were WIDENED after independent `code-reviewer` and
// `security` review of the first implementation. The original projection
// guard matched exactly one construction (a literal containing both
// `wallet_balance_projection` and `FOR UPDATE`) and security's
// adversarial probe defeated it six different ways; the original
// INV-LOCK-E1 guard walked only `internal/bonus`, while the §3.3 folding
// of L0.2 into `AdvisoryLockGrant` moved the invariant's practical blast
// radius into every package that takes a grant/player-scope advisory lock.
// See TestLockOrder_ProjectionGuardDetectsKnownEvasions for the executable
// record of what the widened guard now catches.
package ledger

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// loInternalDir and loCmdDir are the trees both guards walk, relative to
// this package's own source directory. `cmd/` is included because a
// main-package helper is production code in exactly the way that matters
// here: it runs against the real database with the real schema.
const (
	loInternalDir = ".."
	loCmdDir      = "../../cmd"
)

// loProjectionTable is the table rule R4 protects.
const loProjectionTable = "wallet_balance_projection"

// loDynamicOperand is what loFoldConcat substitutes for a non-literal
// operand of a string concatenation (a variable, a call, a constant
// reference). It exists so that `"UPDATE " + table + " SET ..."` folds to
// a string whose shape is still recognisable as an UPDATE of a
// dynamically-named table. NUL never appears in a real SQL literal here.
const loDynamicOperand = "\x00"

// The dangerous-clause vocabulary. Deliberately permissive about casing
// and whitespace: a guard that can be defeated by writing `for   update`
// is not a guard.
var (
	// loForUpdate covers FOR UPDATE, including FOR UPDATE NOWAIT and FOR
	// UPDATE SKIP LOCKED (the \b after `update` matches either).
	loForUpdate = regexp.MustCompile(`(?is)\bfor\s+update\b`)
	// loForNoKeyUpdate and loForShare are the three WEAKER row-lock modes.
	// They are still row locks, they still participate in deadlock cycles,
	// and FOR NO KEY UPDATE conflicts with FOR UPDATE - so "I only took a
	// FOR SHARE" is not an escape from R4.
	loForNoKeyUpdate = regexp.MustCompile(`(?is)\bfor\s+no\s+key\s+update\b`)
	loForShare       = regexp.MustCompile(`(?is)\bfor\s+(key\s+)?share\b`)
	// loOnConflictDoUpdate is the IMPLICIT projection lock: migration
	// 0023's trigger takes its exclusive row lock exactly this way, and so
	// would any hand-written upsert. DO NOTHING is not matched - it takes
	// no row lock on an existing row and is what ledger's own R6
	// materialisation step (and wallet.EnsureWallet, on a different table)
	// legitimately uses.
	loOnConflictDoUpdate = regexp.MustCompile(`(?is)\bon\s+conflict\b[\s\S]*?\bdo\s+update\b`)
	// loProjectionDirectWrite is a statement that writes the projection
	// table by name - "never UPDATE a balance" (CLAUDE.md) made mechanical.
	loProjectionDirectWrite = regexp.MustCompile(`(?is)\b(update|delete\s+from|insert\s+into)\s+(only\s+)?` + loProjectionTable + `\b`)
	// loDynamicTableWrite is the same statement with the table name
	// supplied at runtime (fmt.Sprintf's %s, or a concatenated variable).
	// Only ever consulted for a file that names the projection table
	// somewhere in its own SQL.
	loDynamicTableWrite = regexp.MustCompile(`(?is)\b(update|delete\s+from|insert\s+into)\s+(only\s+)?(%[a-zA-Z]|` + loDynamicOperand + `)`)

	// loLedgerTxTable matches the two append-only ledger tables INV-LOCK-E1
	// protects.
	loLedgerTxTable = regexp.MustCompile(`\bledger_(transactions|entries)\b`)
)

// loAdvisoryLockFuncs are the two entry points that take an ADR 0082 class
// L0.2/L0.3 advisory lock. Anything holding either of them is inside
// INV-LOCK-E1's scope for the remainder of its transaction.
var loAdvisoryLockFuncs = []string{"AdvisoryLockGrant", "AdvisoryLockPlayerBonusScope"}

// loWalkGoFiles visits every non-test .go file under each root, in a
// deterministic order.
//
// Non-test files only, and the reason is worth stating because it is a
// real scoping decision rather than convenience: rule R4 constrains the
// PRODUCTION lock graph. Test code legitimately holds a
// wallet_balance_projection row from outside internal/ledger - that is
// precisely how internal/casino/stage9_concurrency_integration_test.go's
// s9Blocker and this package's own loHoldProjectionRow force a production
// caller to queue at a known point, and ADR 0082 §6 test 14 requires
// those existing tests keep passing unchanged. A blocker transaction in a
// test is not a code path that ships.
func loWalkGoFiles(t *testing.T, roots []string, visit func(path string, src string)) {
	t.Helper()
	for _, root := range roots {
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
}

// loLit is one SQL string found in the source, with the byte offset it
// starts at (used by the INV-LOCK-E1 guard to tell "before the advisory
// lock was taken" from "after").
type loLit struct {
	offset int
	value  string
}

// loCall is one function call, by callee name (the selector's final
// identifier for `pkg.Fn()` / `o.fn()`, the plain identifier otherwise).
type loCall struct {
	offset int
	name   string
}

// loFunc is one top-level function or method, delimited by its body's byte
// range so that "what happens after the advisory lock" is answerable
// positionally.
type loFunc struct {
	name       string
	start, end int
}

// loSource is one parsed production file: its SQL strings, its calls and
// its function ranges.
//
// Parsed with go/parser rather than scanned with a regex for one specific
// reason: COMMENTS ARE EXCLUDED. The previous version of these guards
// matched raw string-literal regexes over the whole file, comments
// included, which made every prose mention of `wallet_balance_projection`
// (internal/withdrawal, internal/casino, internal/bonus and
// internal/reconciliation all have one) indistinguishable from a real
// query - and therefore forced the checks to stay narrow enough to be
// evaded. Excluding comments is what makes the aggressive file-level rules
// below affordable.
type loSource struct {
	path  string
	lits  []loLit
	calls []loCall
	funcs []loFunc
}

func loParse(t *testing.T, path, src string) loSource {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := loSource{path: path}
	off := func(p token.Pos) int { return fset.Position(p).Offset }

	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		out.funcs = append(out.funcs, loFunc{name: fd.Name.Name, start: off(fd.Body.Pos()), end: off(fd.Body.End())})
	}

	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				if s, uerr := strconv.Unquote(v.Value); uerr == nil {
					out.lits = append(out.lits, loLit{offset: off(v.Pos()), value: s})
				}
			}
		case *ast.BinaryExpr:
			// A concatenation is ALSO recorded as one folded string, so
			// `"... FROM wallet_balance_" + "projection ... FOR UPDATE"`
			// and `"SELECT ... FROM " + tbl + " ... FOR UPDATE"` are both
			// visible as single statements. The individual operands stay in
			// the list too (ast.Inspect visits them); duplicates are
			// harmless.
			if v.Op == token.ADD {
				out.lits = append(out.lits, loLit{offset: off(v.Pos()), value: loFoldConcat(v)})
			}
		case *ast.CallExpr:
			if name := loCalleeName(v.Fun); name != "" {
				out.calls = append(out.calls, loCall{offset: off(v.Pos()), name: name})
			}
		}
		return true
	})
	return out
}

// loFoldConcat renders a `+` expression as the SQL it will actually
// produce, with every non-literal operand replaced by loDynamicOperand.
func loFoldConcat(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			if s, err := strconv.Unquote(v.Value); err == nil {
				return s
			}
		}
		return loDynamicOperand
	case *ast.BinaryExpr:
		if v.Op == token.ADD {
			return loFoldConcat(v.X) + loFoldConcat(v.Y)
		}
		return loDynamicOperand
	case *ast.ParenExpr:
		return loFoldConcat(v.X)
	default:
		return loDynamicOperand
	}
}

func loCalleeName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	case *ast.IndexExpr: // generic instantiation
		return loCalleeName(v.X)
	}
	return ""
}

func loFlatten(s string) string { return strings.Join(strings.Fields(s), " ") }

// loProjectionLockOffenders is rule R4's actual detector, factored out of
// the walk so TestLockOrder_ProjectionGuardDetectsKnownEvasions can prove
// - executably - which constructions it catches.
//
// Two layers:
//
//  1. STATEMENT-LEVEL. A single (possibly concatenation-folded) SQL string
//     that both names the projection table and carries a row-lock clause,
//     or that writes the projection table by name at all.
//  2. FILE-LEVEL. If ANY SQL string in the file names the projection
//     table, then a row-lock clause or an upsert-with-DO-UPDATE ANYWHERE
//     in that file's SQL is an offender, even in a different string. This
//     is what closes the `fmt.Sprintf("... FROM %s ... FOR UPDATE", tbl)`
//     and `const projTable = "wallet_balance_projection"` evasions, where
//     the table name and the lock clause never appear in the same literal.
//
// Layer 2 is deliberately over-eager: a file that talks to
// wallet_balance_projection at all and ALSO row-locks something else is
// flagged. That is the correct trade for this rule - the fix (move the
// lock into internal/ledger, or split the file) is cheap, and a false
// negative is a reopened production deadlock. It is not a theoretical
// risk taken lightly: the one file in the tree that legitimately reads
// the projection from outside internal/ledger (wallet.GetSummary) takes no
// row lock at all, which is exactly the property the rule asserts.
func loProjectionLockOffenders(path string, lits []loLit) []string {
	namesProjection := false
	for _, l := range lits {
		if strings.Contains(l.value, loProjectionTable) {
			namesProjection = true
			break
		}
	}

	var offenders []string
	add := func(why, sql string) {
		offenders = append(offenders, path+": "+why+": "+loFlatten(sql))
	}
	for _, l := range lits {
		v := l.value
		if loProjectionDirectWrite.MatchString(v) {
			add("direct write to "+loProjectionTable, v)
			continue
		}
		if !namesProjection {
			continue
		}
		switch {
		case loForUpdate.MatchString(v):
			add("FOR UPDATE in a file whose SQL names "+loProjectionTable, v)
		case loForNoKeyUpdate.MatchString(v):
			add("FOR NO KEY UPDATE in a file whose SQL names "+loProjectionTable, v)
		case loForShare.MatchString(v):
			add("FOR SHARE in a file whose SQL names "+loProjectionTable, v)
		case loOnConflictDoUpdate.MatchString(v):
			add("ON CONFLICT ... DO UPDATE in a file whose SQL names "+loProjectionTable, v)
		case loDynamicTableWrite.MatchString(v):
			add("write to a dynamically-named table in a file whose SQL names "+loProjectionTable, v)
		}
	}
	return offenders
}

// TestLockOrder_NoProjectionForUpdateOutsideLedgerPackage is ADR 0082's
// rule R4, made permanent: `wallet_balance_projection` is locked ONLY by
// internal/ledger.
//
// This is the single test that stops LOCK-1 from being reintroduced. The
// original defect was a three-line private helper
// (casino.lockCashBalance, sportsbook.lockCashBalance,
// withdrawal.lockCashBalanceForUpdate - all three deleted by this
// dispatch) that locked ONE projection row before calling ledger.Post,
// which then locked the rest in a different order. Nothing about that
// helper looked wrong in review; it read as a careful, correct
// "check the balance under a lock, in the same transaction" and it was
// copied into three packages. Only a mechanical rule keeps it out.
//
// Unlocked reads of the projection (wallet.GetSummary,
// ledger.GetProjectedBalance, internal/reconciliation) are unaffected and
// intentionally not flagged - the rule is about acquiring a lock on the
// table, not about reading it.
func TestLockOrder_NoProjectionForUpdateOutsideLedgerPackage(t *testing.T) {
	var offenders []string
	seen := 0
	loWalkGoFiles(t, []string{loInternalDir, loCmdDir}, func(path, src string) {
		if strings.Contains(path, "/ledger/") {
			return
		}
		parsed := loParse(t, path, src)
		for _, l := range parsed.lits {
			if strings.Contains(l.value, loProjectionTable) {
				seen++
			}
		}
		offenders = append(offenders, loProjectionLockOffenders(path, parsed.lits)...)
	})

	if seen == 0 {
		t.Fatal("this guard found no wallet_balance_projection SQL outside internal/ledger at all - " +
			"either the tree moved or the walk is broken, and a guard that inspects nothing proves nothing " +
			"(wallet.GetSummary's unlocked read is the expected sighting)")
	}
	if len(offenders) > 0 {
		t.Fatalf("ADR 0082 R4 violated - wallet_balance_projection may only be locked or written by internal/ledger "+
			"(use ledger.LockProjectionsForPosting, which locks the COMPLETE account set in canonical order; a "+
			"partial pre-lock is finding LOCK-1 itself):\n  %s", strings.Join(offenders, "\n  "))
	}
}

// TestLockOrder_ProjectionGuardDetectsKnownEvasions is the guard's own
// regression test: the six constructions `security`'s adversarial probe
// used to defeat the first (single-pattern) version of R4's guard, plus
// the legitimate shapes that must NOT be flagged.
//
// It exists because a static guard silently failing to match is
// indistinguishable, in a green test run, from a clean tree. Each case
// below is the evasion in the form the probe wrote it.
func TestLockOrder_ProjectionGuardDetectsKnownEvasions(t *testing.T) {
	mustFlag := []struct {
		name string
		src  string
	}{
		{
			name: "concatenated table name",
			src:  "package x\nfunc f(tx T) { tx.Query(ctx, `SELECT debit_total FROM wallet_balance_` + `projection WHERE ledger_account_id = $1 FOR UPDATE`) }\n",
		},
		{
			name: "fmt.Sprintf-built table name",
			src:  "package x\nconst tbl = \"wallet_balance_projection\"\nfunc f(tx T) { tx.Query(ctx, fmt.Sprintf(\"SELECT debit_total FROM %s WHERE ledger_account_id = $1 FOR UPDATE\", tbl)) }\n",
		},
		{
			name: "FOR NO KEY UPDATE",
			src:  "package x\nfunc f(tx T) { tx.Query(ctx, `SELECT debit_total FROM wallet_balance_projection WHERE ledger_account_id = $1 FOR NO KEY UPDATE`) }\n",
		},
		{
			name: "FOR SHARE",
			src:  "package x\nfunc f(tx T) { tx.Query(ctx, `SELECT debit_total FROM wallet_balance_projection WHERE ledger_account_id = $1 FOR SHARE`) }\n",
		},
		{
			name: "direct UPDATE statement",
			src:  "package x\nfunc f(tx T) { tx.Exec(ctx, `UPDATE wallet_balance_projection SET debit_total = debit_total + $1 WHERE ledger_account_id = $2`) }\n",
		},
		{
			name: "ON CONFLICT DO UPDATE upsert",
			src:  "package x\nfunc f(tx T) { tx.Exec(ctx, `INSERT INTO wallet_balance_projection (ledger_account_id, debit_total) VALUES ($1, $2) ON CONFLICT (ledger_account_id) DO UPDATE SET debit_total = EXCLUDED.debit_total`) }\n",
		},
		{
			name: "concatenated dynamic UPDATE",
			src:  "package x\nvar tbl = \"wallet_balance_projection\"\nfunc f(tx T) { tx.Exec(ctx, \"UPDATE \" + tbl + \" SET debit_total = 0 WHERE ledger_account_id = $1\") }\n",
		},
	}
	for _, c := range mustFlag {
		parsed := loParse(t, c.name+".go", c.src)
		if got := loProjectionLockOffenders("probe.go", parsed.lits); len(got) == 0 {
			t.Errorf("R4 guard did NOT flag the %q evasion - the guard is defeatable again:\n%s", c.name, c.src)
		}
	}

	mustNotFlag := []struct {
		name string
		src  string
	}{
		{
			name: "unlocked projection read (wallet.GetSummary)",
			src:  "package x\nfunc f(tx T) { tx.Query(ctx, `SELECT p.account_type, p.debit_total FROM wallet_balance_projection p JOIN ledger_accounts la ON la.id = p.ledger_account_id WHERE la.wallet_id = $1`) }\n",
		},
		{
			name: "projection read alongside an unrelated ON CONFLICT DO NOTHING",
			src:  "package x\nfunc f(tx T) {\n tx.Query(ctx, `SELECT debit_total FROM wallet_balance_projection WHERE ledger_account_id = $1`)\n tx.Exec(ctx, `INSERT INTO wallets (id) VALUES ($1) ON CONFLICT (player_account_id, asset_code) DO NOTHING`)\n}\n",
		},
		{
			name: "FOR UPDATE on an unrelated table, no projection SQL in the file",
			src:  "package x\nfunc f(tx T) { tx.Query(ctx, `SELECT id FROM withdrawal_requests WHERE id = $1 FOR UPDATE`) }\n",
		},
		{
			name: "projection named only in a comment",
			src:  "package x\n// takes its wallet_balance_projection locks via ledger.LockProjectionsForPosting\nfunc f(tx T) { tx.Query(ctx, `SELECT id FROM withdrawal_requests WHERE id = $1 FOR UPDATE`) }\n",
		},
	}
	for _, c := range mustNotFlag {
		parsed := loParse(t, c.name+".go", c.src)
		if got := loProjectionLockOffenders("probe.go", parsed.lits); len(got) > 0 {
			t.Errorf("R4 guard false-positived on %q (legitimate code must stay legal):\n  %s", c.name, strings.Join(got, "\n  "))
		}
	}
}

// loLedgerRowLockLit reports whether one SQL string takes a row lock on
// ledger_transactions/ledger_entries.
func loLedgerRowLockLit(v string) bool {
	if !loLedgerTxTable.MatchString(v) {
		return false
	}
	return loForUpdate.MatchString(v) || loForNoKeyUpdate.MatchString(v) || loForShare.MatchString(v)
}

// TestLockOrder_NoLedgerTransactionForUpdateUnderGrantAdvisoryLock is
// invariant INV-LOCK-E1 (ADR 0082 §5.1), the guard that keeps named
// exception E-1 safe.
//
// E-1 is a genuine inversion of rule R8: casino.postRollback locks the
// original `ledger_transactions` row (class L2) and only THEN resolves the
// grant and takes its advisory lock (class L0.3); postWinLockedBonus
// reaches the grant advisory lock after postWin's L2 lock. It is not
// reordered this stage because the advisory key IS DERIVED FROM the row
// being locked - the grant id is looked up from grant_ledger_attributions
// keyed on the original transaction id, which the locked read itself
// produces - so reordering means an unlocked pre-read, an advisory lock
// on its result, then a re-read and re-validation under the row lock: a
// restructuring of two of the most safety-critical settlement paths in
// the platform, for a cycle that does not currently exist.
//
// It does not currently exist because of exactly one property: no path
// that HOLDS a bonus_grant advisory lock ever waits on a
// ledger_transactions/ledger_entries row lock. Every internal/bonus read
// of those tables is unlocked (RemainingBonusBalance, ComputeAOE,
// checkReversalFundingMatches). This test pins that property:
//
//	INV-LOCK-E1: no code path that holds a `bonus_grant:` advisory lock
//	may take a FOR UPDATE on ledger_transactions or ledger_entries.
//
// SCOPE, widened after `code-reviewer`'s review of the first
// implementation. The original guard enforced this only as "no production
// file in internal/bonus contains such a lock", on the reasoning that
// internal/bonus is the package that takes the advisory lock. §3.3's
// folding changed that: AdvisoryLockGrant now also acquires L0.2, and
// casino.postBet takes L0.2 directly (orchestrator.go, around the delivery
// advisory lock) and holds it through commit - so internal/casino is
// inside the invariant's practical blast radius for most of postBet's
// execution, even though postBet takes no such row lock today. The guard
// therefore now covers:
//
//   - internal/bonus: PACKAGE-WIDE (strictest form - this package must
//     never row-lock those tables at all);
//   - every OTHER package that calls AdvisoryLockGrant /
//     AdvisoryLockPlayerBonusScope, discovered by walking the tree rather
//     than hardcoded: FUNCTION-SCOPED AND POSITIONAL - a row lock on those
//     tables is flagged only if it appears AFTER the advisory lock is
//     taken in the same function, which is precisely "while the lock could
//     be held". A row lock taken BEFORE is E-1 itself and is permitted
//     (casino.postRollback's :1165 lock, casino.postWin's ORDER BY id FOR
//     UPDATE). One level of same-package callee expansion is included, so
//     calling a known row-locking helper after the advisory lock is
//     flagged too.
//
// Known limit, stated rather than papered over: the callee expansion is
// one level deep and same-package only. A cross-package helper called
// after the advisory lock could still hide a row lock. Closing that needs
// a call-graph analysis; INV-LOCK-E1 remains owned by `code-reviewer` for
// that residue.
//
// If this test ever fails, E-1 must be RESOLVED FIRST and ADR 0082
// amended - not silenced.
func TestLockOrder_NoLedgerTransactionForUpdateUnderGrantAdvisoryLock(t *testing.T) {
	var offenders []string

	// --- Layer 1: internal/bonus, package-wide. ---
	inspected := 0
	loWalkGoFiles(t, []string{filepath.Join(loInternalDir, "bonus")}, func(path, src string) {
		inspected++
		for _, l := range loParse(t, path, src).lits {
			if loLedgerRowLockLit(l.value) {
				offenders = append(offenders, path+" (internal/bonus, package-wide): "+loFlatten(l.value))
			}
		}
	})
	if inspected == 0 {
		t.Fatal("this guard inspected no internal/bonus source files at all - the walk is broken")
	}

	// --- Layer 2: every other package that takes one of the advisory
	// locks, function-scoped and positional. ---
	byDir := map[string][]loSource{}
	loWalkGoFiles(t, []string{loInternalDir, loCmdDir}, func(path, src string) {
		dir := filepath.ToSlash(filepath.Dir(path))
		if strings.HasSuffix(dir, "/bonus") {
			return // covered, more strictly, by layer 1
		}
		// Cheap pre-filter before paying for a parse.
		takesAdvisory := false
		for _, fn := range loAdvisoryLockFuncs {
			if strings.Contains(src, fn+"(") {
				takesAdvisory = true
				break
			}
		}
		if !takesAdvisory {
			return
		}
		byDir[dir] = append(byDir[dir], loParse(t, path, src))
	})

	if len(byDir) == 0 {
		t.Fatal("INV-LOCK-E1 guard discovered NO package outside internal/bonus that calls AdvisoryLockGrant/" +
			"AdvisoryLockPlayerBonusScope - internal/casino does (orchestrator.go, bonus_settlement.go), so the " +
			"discovery walk is broken and this guard is proving nothing")
	}

	dirs := make([]string, 0, len(byDir))
	for dir := range byDir {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	for _, dir := range dirs {
		files := byDir[dir]
		// Same-package functions that row-lock the ledger tables anywhere in
		// their body - the one-level callee expansion below.
		rowLockers := map[string]bool{}
		for _, f := range files {
			for _, fn := range f.funcs {
				for _, l := range f.lits {
					if l.offset >= fn.start && l.offset < fn.end && loLedgerRowLockLit(l.value) {
						rowLockers[fn.name] = true
					}
				}
			}
		}

		for _, f := range files {
			for _, fn := range f.funcs {
				advisoryAt := -1
				for _, c := range f.calls {
					if c.offset < fn.start || c.offset >= fn.end {
						continue
					}
					isAdvisory := false
					for _, name := range loAdvisoryLockFuncs {
						if c.name == name {
							isAdvisory = true
							break
						}
					}
					if isAdvisory && (advisoryAt < 0 || c.offset < advisoryAt) {
						advisoryAt = c.offset
					}
				}
				if advisoryAt < 0 {
					continue
				}
				for _, l := range f.lits {
					if l.offset > advisoryAt && l.offset < fn.end && loLedgerRowLockLit(l.value) {
						offenders = append(offenders, f.path+": "+fn.name+" row-locks after taking the advisory lock: "+loFlatten(l.value))
					}
				}
				for _, c := range f.calls {
					if c.offset > advisoryAt && c.offset < fn.end && c.name != fn.name && rowLockers[c.name] {
						offenders = append(offenders, f.path+": "+fn.name+" calls row-locking "+c.name+" after taking the advisory lock")
					}
				}
			}
		}
	}

	if len(offenders) > 0 {
		t.Fatalf("INV-LOCK-E1 violated (ADR 0082 §5.1) - a FOR UPDATE on ledger_transactions/ledger_entries taken "+
			"while a bonus_grant/bonus_player advisory lock is held closes the L2-before-L0 cycle that exception "+
			"E-1 depends on NOT existing. Resolve E-1 and amend ADR 0082 before adding this:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// TestLockOrder_GrantAdvisoryLockAlwaysTakesPlayerScopeFirst pins the
// §3.3 folding decision itself: AdvisoryLockGrant acquires class L0.2
// (the player bonus-scope advisory lock) as an internal precondition
// before class L0.3. That is what makes every existing AdvisoryLockGrant
// caller correct with no call-site change, and it is a property that
// would be silently lost by an innocent-looking refactor of
// AdvisoryLockGrant's body.
//
// Static because the alternative - proving a lock was taken - requires
// racing a second transaction on a lock that is reentrant within the
// caller's own transaction, which proves nothing about ordering.
// TestLockOrder_ConcurrentBetAndGrantConversion_NoDeadlock (internal/
// casino) is the behavioural proof; this is the cheap guard that fails
// fast and names the reason.
func TestLockOrder_GrantAdvisoryLockAlwaysTakesPlayerScopeFirst(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(loInternalDir, "bonus", "lifecycle.go"))
	if err != nil {
		t.Fatalf("read internal/bonus/lifecycle.go: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "func AdvisoryLockGrant(")
	if start < 0 {
		t.Fatal("AdvisoryLockGrant not found in internal/bonus/lifecycle.go")
	}
	end := strings.Index(body[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not delimit AdvisoryLockGrant's body")
	}
	fn := body[start : start+end]

	scopeAt := strings.Index(fn, "AdvisoryLockPlayerBonusScope")
	grantAt := strings.Index(fn, "'bonus_grant:'")
	if scopeAt < 0 {
		t.Fatal("ADR 0082 §3.3: AdvisoryLockGrant must acquire the L0.2 player bonus-scope advisory lock " +
			"(AdvisoryLockPlayerBonusScope) as an internal precondition - without it, finding LOCK-1d reopens " +
			"for every caller at once")
	}
	if grantAt < 0 {
		t.Fatal("AdvisoryLockGrant no longer takes a 'bonus_grant:' advisory lock")
	}
	if scopeAt > grantAt {
		t.Fatal("ADR 0082 §2.1: class L0.2 (bonus_player) must be acquired BEFORE class L0.3 (bonus_grant), " +
			"not after")
	}
}
