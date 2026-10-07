package reconciliation

// PRH-2 R3 / H-W1: the structural half of the guarantee that the reconciliation
// path - including the non-active-tenant observation sweep - is read plus
// findings/alerts only. No database is needed; these run in every `go test`.
//
//   - Imports: this package imports no payment orchestrator, adapter, wallet,
//     withdrawal or staff-resolution package, so it CANNOT dispatch, query a
//     provider, move an attempt, or create/execute a manual resolution. The
//     only provider-facing surface is statement.PaymentStatementSource.Fetch.
//   - Ledger: of package ledger it uses only read functions and constants; it
//     never calls ledger.Post.
//   - SQL: the only tables it writes are its own findings/run/import stores.
//   - The observation entry point calls nothing but the payment_statement
//     stream and the non-active tenant list.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func r3ProductionFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	fset := token.NewFileSet()
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*ast.File{}
	for _, m := range matches {
		if strings.HasSuffix(m, "_test.go") {
			continue
		}
		src, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, m, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", m, err)
		}
		out[m] = f
	}
	if len(out) == 0 {
		t.Fatal("no production files found")
	}
	return out
}

func TestR3_Static_ReconciliationImportsNoProviderOrMoneyMovingPackage(t *testing.T) {
	allowed := map[string]bool{
		"github.com/Diansalas/igaming-platform/internal/alerting":                 true,
		"github.com/Diansalas/igaming-platform/internal/audit":                    true,
		"github.com/Diansalas/igaming-platform/internal/db":                       true,
		"github.com/Diansalas/igaming-platform/internal/ledger":                   true, // reads only, see the next test
		"github.com/Diansalas/igaming-platform/internal/providerref":              true,
		"github.com/Diansalas/igaming-platform/internal/reconciliation/statement": true,
		"github.com/Diansalas/igaming-platform/internal/txscope":                  true,
	}
	for name, f := range r3ProductionFiles(t) {
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if strings.HasPrefix(p, "github.com/Diansalas/igaming-platform/") && !allowed[p] {
				t.Errorf("%s imports %s: the reconciliation package may not depend on it (R3 read-only guarantee; add it only with ledger-finance and security review)", name, p)
			}
		}
	}
}

const r3LedgerPath = "github.com/Diansalas/igaming-platform/internal/ledger"

func TestR3_Static_LedgerUsedReadOnly(t *testing.T) {
	// Exact allow-list (security F-6): two read functions, the direction
	// constants, and the account/transaction type constants the sportsbook
	// stream compares. A new ledger symbol, including a future function whose
	// name starts with Tx or Account, fails here until reviewed.
	allowed := map[string]bool{
		"GetProjectedBalance": true, "RebuildBalance": true, "Credit": true, "Debit": true,
		"AccountHouseGaming": true, "AccountPlayerCash": true, "AccountPlayerLockedCash": true,
		"TxSportsbookBet": true, "TxSportsbookRollback": true, "TxSportsbookSettlement": true,
		"TxSportsbookVoid": true, "TxTombstone": true,
	}
	seen := 0
	for name, f := range r3ProductionFiles(t) {
		// Resolve the LOCAL name of the ledger import by PATH (LF N-2: an aliased
		// import must not evade the check); a dot import is refused outright.
		local := ""
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p != r3LedgerPath {
				continue
			}
			switch {
			case imp.Name == nil:
				local = "ledger"
			case imp.Name.Name == ".":
				t.Errorf("%s dot-imports the ledger package", name)
			default:
				local = imp.Name.Name
			}
		}
		if local == "" {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || id.Name != local {
				return true
			}
			seen++
			if !allowed[sel.Sel.Name] {
				t.Errorf("%s uses ledger.%s: only the reviewed read functions and constants are allowed here (no ledger.Post, no account creation)", name, sel.Sel.Name)
			}
			return true
		})
	}
	if seen < 4 {
		t.Fatalf("static scan is vacuous: it saw only %d ledger references", seen)
	}
}

// r3ThirdPartyAllowed is the reviewed allow-list of third-party (non-std,
// non-module) code the reconciliation package may pull in transitively. It is
// derived from `go list -deps .` and matched on module-root boundaries. Every
// entry is a read-path / plumbing dependency of the DB driver or tracing API;
// none performs money movement or provider I/O of its own.
var r3ThirdPartyAllowed = map[string]string{
	"github.com/google/uuid":         "identifier generation (pure)",
	"github.com/jackc/pgx/v5":        "PostgreSQL driver used by internal/db",
	"github.com/jackc/pgpassfile":    "pgx dependency: .pgpass parsing",
	"github.com/jackc/pgservicefile": "pgx dependency: service file parsing",
	"github.com/jackc/puddle/v2":     "pgxpool connection pooling",
	"golang.org/x/text":              "pgx dependency: SASLprep / unicode normalisation",
	"golang.org/x/sync":              "puddle dependency: semaphore",
	"github.com/go-logr/logr":        "otel dependency: logging facade",
	"github.com/go-logr/stdr":        "otel dependency: logr stdlib adapter",
	"github.com/cespare/xxhash/v2":   "otel attribute hashing (pure)",
	"go.opentelemetry.io/otel":       "tracing/metric API used by internal/db (no exporter)",
	"go.opentelemetry.io/auto/sdk":   "otel no-op/auto SDK shim (no exporter)",
}

// r3VendoredStdAllowed are the golang.org/x copies vendored inside the Go
// standard library (reached via crypto/tls and net/http).
var r3VendoredStdAllowed = []string{
	"vendor/golang.org/x/crypto/", "vendor/golang.org/x/net/",
	"vendor/golang.org/x/sys/", "vendor/golang.org/x/text/",
}

// r3StdDenied are std packages that must never appear in the closure: process
// execution, dynamic code loading and extra network protocol clients. They are
// all ABSENT today (verified from go list -deps); net/http, net, os, syscall
// and crypto/tls are legitimately present via the DB driver and so cannot be
// denied by package name (the import pin above covers direct use).
var r3StdDenied = map[string]bool{
	"os/exec": true, "plugin": true, "net/rpc": true, "net/smtp": true,
	"net/http/cgi": true, "net/http/fcgi": true, "net/http/pprof": true,
	"net/http/httputil": true, "os/signal": true,
}

const r3ModulePrefix = "github.com/Diansalas/igaming-platform/"

// r3ClassifyDep returns "" when the package is acceptable, else a reason.
func r3ClassifyDep(p string, internalAllowed map[string]bool) string {
	first := p
	if i := strings.Index(p, "/"); i >= 0 {
		first = p[:i]
	}
	switch {
	case strings.HasPrefix(p, r3ModulePrefix):
		if !internalAllowed[p] {
			return "not in the reviewed read-only internal set"
		}
	case strings.HasPrefix(p, "vendor/"):
		for _, v := range r3VendoredStdAllowed {
			if strings.HasPrefix(p, v) {
				return ""
			}
		}
		return "vendored std package not in the reviewed list"
	case !strings.Contains(first, "."):
		if r3StdDenied[p] {
			return "denied std package (exec / plugin / extra network client)"
		}
	default:
		for root := range r3ThirdPartyAllowed {
			if p == root || strings.HasPrefix(p, root+"/") {
				return ""
			}
		}
		return "third-party package not in the reviewed allow-list"
	}
	return ""
}

// LF N-2 / security: the import pin above looks at direct imports; this one
// asks the toolchain for the TRANSITIVE closure, so a forbidden package pulled
// in through an allowed one is caught as well - internal, third-party and std.
func TestR3_Static_TransitiveDependenciesAreReadOnlyPackages(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps failed (the transitive read-only pin must not silently switch off): %v", err)
	}
	internalAllowed := map[string]bool{}
	for _, p := range []string{"txscope", "db", "alerting", "audit", "ledger", "providerref", "reconciliation", "reconciliation/statement"} {
		internalAllowed[r3ModulePrefix+"internal/"+p] = true
	}
	internal, third, std := 0, 0, 0
	usedRoots := map[string]bool{}
	for _, p := range strings.Fields(string(out)) {
		switch first, _, _ := strings.Cut(p, "/"); {
		case strings.HasPrefix(p, r3ModulePrefix):
			internal++
		case strings.HasPrefix(p, "vendor/"):
			std++
		case strings.Contains(first, "."):
			third++
			for root := range r3ThirdPartyAllowed {
				if p == root || strings.HasPrefix(p, root+"/") {
					usedRoots[root] = true
				}
			}
		default:
			std++
		}
		if why := r3ClassifyDep(p, internalAllowed); why != "" {
			t.Errorf("transitive dependency %s: %s", p, why)
		}
	}
	if internal < 5 || third < 10 || std < 50 {
		t.Fatalf("go list -deps looks vacuous: internal=%d third-party=%d std=%d", internal, third, std)
	}
	for root := range r3ThirdPartyAllowed {
		if !usedRoots[root] {
			t.Errorf("allow-list entry %s is stale (no longer in the closure): remove it so the list stays minimal", root)
		}
	}
}

// Negative control for the classifier: each class of violation is flagged and
// the accepted shapes are not.
func TestR3_Static_DependencyClassifierHasTeeth(t *testing.T) {
	internal := map[string]bool{r3ModulePrefix + "internal/db": true}
	bad := []string{
		r3ModulePrefix + "internal/payment",
		"os/exec", "plugin", "net/rpc",
		"github.com/evil/exfil", "github.com/jackc/pgx/v50", "github.com/google/uuidx",
		"vendor/golang.org/x/evil/pkg",
	}
	for _, p := range bad {
		if r3ClassifyDep(p, internal) == "" {
			t.Errorf("classifier accepted forbidden dependency %s", p)
		}
	}
	good := []string{
		r3ModulePrefix + "internal/db", "fmt", "net/http", "github.com/jackc/pgx/v5/pgxpool",
		"github.com/google/uuid", "vendor/golang.org/x/net/idna",
	}
	for _, p := range good {
		if why := r3ClassifyDep(p, internal); why != "" {
			t.Errorf("classifier rejected acceptable dependency %s: %s", p, why)
		}
	}
}

// r3WriteRE finds SQL write statements. Besides INSERT/UPDATE/DELETE it sees
// MERGE INTO and TRUNCATE [TABLE] [ONLY] (both write without any of the three
// classic verbs).
var r3WriteRE = regexp.MustCompile(`(?is)\b(insert\s+into|update|delete\s+from|merge\s+into|truncate(?:\s+table)?(?:\s+only)?)\s+([a-z_][a-z0-9_]*)`)

// r3WriteFuncRE is an ADDITIVE deny-list of well-known SQL functions that
// persist data or reach outside the transaction: large-object writers
// (lo_import/lo_create/lo_creat/lo_unlink/lo_put/lowrite) and dblink_exec
// (runs arbitrary DML on a remote database). Deliberately NOT included:
// nextval/setval/pg_advisory_* (sequence and lock state, not data writes) and
// set_config (session setting). No reconciliation SQL uses any of these.
var r3WriteFuncRE = regexp.MustCompile(`(?i)\b(lo_import|lo_create|lo_creat|lo_unlink|lo_put|lowrite|dblink_exec)\s*\(`)

type r3SQLWrite struct{ verb, target string }

// r3SQLWrites returns every write found in one SQL string. Function calls are
// reported with verb "call".
func r3SQLWrites(s string) []r3SQLWrite {
	var out []r3SQLWrite
	for _, m := range r3WriteRE.FindAllStringSubmatch(s, -1) {
		out = append(out, r3SQLWrite{strings.ToLower(strings.Join(strings.Fields(m[1]), " ")), strings.ToLower(m[2])})
	}
	for _, m := range r3WriteFuncRE.FindAllStringSubmatch(s, -1) {
		out = append(out, r3SQLWrite{"call", strings.ToLower(m[1])})
	}
	return out
}

// Negative control: a synthetic string with every extended shape is flagged,
// and benign SQL (including FOR UPDATE OF and sequence/lock helpers) is not
// flagged beyond the known lock-clause token.
func TestR3_Static_WriteScanHasTeeth(t *testing.T) {
	flagged := map[string]string{
		"MERGE INTO payments USING x ON true WHEN MATCHED THEN DELETE": "merge into",
		"merge   into\n payments using x":                              "merge into",
		"TRUNCATE ledger_entries":                                      "truncate",
		"truncate table ledger_entries":                                "truncate table",
		"TRUNCATE TABLE ONLY ledger_entries":                           "truncate table only",
		"SELECT lo_import('/etc/passwd')":                              "call",
		"SELECT lo_create(0)":                                          "call",
		"SELECT LO_CREAT(0)":                                           "call",
		"SELECT lo_unlink(1234)":                                       "call",
		"SELECT lo_put(1, 0, 'x')":                                     "call",
		"SELECT lowrite(1, 'x')":                                       "call",
		"SELECT dblink_exec('c', 'delete from t')":                     "call",
	}
	for sql, wantVerb := range flagged {
		got := r3SQLWrites(sql)
		found := false
		for _, w := range got {
			if w.verb == wantVerb {
				found = true
			}
		}
		if !found {
			t.Errorf("write scan did not flag %q (want verb %q, got %v)", sql, wantVerb, got)
		}
	}
	for _, sql := range []string{
		"SELECT nextval('s'), setval('s', 1), pg_advisory_xact_lock(1), set_config('a','b',true)",
		"SELECT id FROM reconciliation_runs WHERE x = $1",
	} {
		if got := r3SQLWrites(sql); len(got) != 0 {
			t.Errorf("write scan false-positive on %q: %v", sql, got)
		}
	}
	// The production SQL must still be exactly the allowed shapes: nothing
	// from the extended verb/function set.
	for name, f := range r3ProductionFiles(t) {
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, w := range r3SQLWrites(s) {
				if w.verb != "insert into" && w.verb != "update" {
					t.Errorf("%s production SQL contains %s %s", name, w.verb, w.target)
				}
			}
			return true
		})
	}
}

func TestR3_Static_OnlyOwnStoresAreWritten(t *testing.T) {
	insertable := map[string]bool{
		"reconciliation_runs": true, "reconciliation_mismatches": true,
		"payment_statement_imports": true, "payment_statement_lines": true,
	}
	updatable := map[string]bool{"reconciliation_mismatches": true} // ResolveMismatch only (not on any sweep path)
	seenInsert := map[string]bool{}
	for name, f := range r3ProductionFiles(t) {
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, m := range r3SQLWrites(s) {
				verb, table := m.verb, m.target
				switch verb {
				case "insert into":
					seenInsert[table] = true
					if !insertable[table] {
						t.Errorf("%s writes (INSERT) to %q: the reconciliation package may write only its own stores", name, table)
					}
				case "update":
					if table == "of" || table == "set" {
						continue // "FOR UPDATE OF ..." is a lock clause, not a write
					}
					if !updatable[table] {
						t.Errorf("%s writes (UPDATE) to %q: the reconciliation package may write only its own stores", name, table)
					}
				case "delete from":
					t.Errorf("%s DELETEs from %q: the reconciliation package never deletes", name, table)
				default:
					t.Errorf("%s uses forbidden SQL write %s %q: the reconciliation package may write only via INSERT/UPDATE on its own stores", name, verb, table)
				}
			}
			return true
		})
	}
	// Vacuity control: the scan really sees the stores the stream writes.
	var got []string
	for tb := range seenInsert {
		got = append(got, tb)
	}
	sort.Strings(got)
	if len(got) < 4 {
		t.Fatalf("static scan is vacuous: it saw INSERTs into only %v", got)
	}
}

func TestR3_Static_ObservationEntryPointCallsOnlyThePaymentStatementStream(t *testing.T) {
	allowed := map[string]bool{
		"len": true, "make": true, "append": true,
		"listNonActiveTenants": true, "runLedgerStreamForTenant": true, "ReconcilePaymentStatementForTenant": true,
	}
	var fn *ast.FuncDecl
	for _, f := range r3ProductionFiles(t) {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "observeNonActiveTenants" {
				fn = fd
			}
		}
	}
	if fn == nil {
		t.Fatal("observeNonActiveTenants not found")
	}
	calls := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := ce.Fun.(type) {
		case *ast.Ident:
			calls[fun.Name] = true
			if !allowed[fun.Name] {
				t.Errorf("observeNonActiveTenants calls %s: it may call only the ledger_vs_projection stream, the payment_statement stream and the non-active tenant list", fun.Name)
			}
		case *ast.SelectorExpr:
			t.Errorf("observeNonActiveTenants calls %s.%s: only package-local stream functions are allowed", exprName(fun.X), fun.Sel.Name)
		case *ast.ArrayType, *ast.MapType:
			// conversions / composite constructors are not calls to behaviour
		}
		return true
	})
	for _, must := range []string{"listNonActiveTenants", "runLedgerStreamForTenant", "ReconcilePaymentStatementForTenant"} {
		if !calls[must] {
			t.Errorf("observeNonActiveTenants no longer calls %s (the pin would be vacuous)", must)
		}
	}
}

func exprName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return "?"
}
