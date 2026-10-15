// IO-1C (architect review `rv-prh-architect.md`, INV-IO-1(c)): "a source
// scan ... fails on an adapter-method call lexically inside a `With*`
// closure". ADR 0095 D1's primary control is the API shape (no function
// that can reach a provider adapter's outbound method takes a pgx.Tx) plus
// the runtime txscope.Held(ctx) refusal at each phase-B call site (IO-1B,
// this package's own Held/Mark). This is the THIRD, static layer: a
// permanent regression guard that fails the build the moment a future
// change writes a provider adapter call directly inside a database
// transaction closure - the shape that would defeat both of the other two
// controls at once (the closure's own ctx, freshly derived inside
// pool.WithTenant/WithPrincipalScope/etc, is never txscope-marked from the
// call site's OWN perspective in the way a caller passing an already-held
// ctx into a lower-level function is; the runtime guard only catches the
// LATTER shape, not a hand-written call site that never goes through the
// pool at all - though in every real case in this codebase it does, since
// no adapter method is reachable except through CreateVerification/
// SubmitVerification/LaunchGame/the payments gate).
//
// Deliberately lexical only, matching the ADR's own wording and this
// codebase's existing static-guard style (internal/ledger's
// lockorder_static_test.go, internal/webhookauth's
// constant_time_lint_test.go): a call is a violation only if it appears
// textually inside the closure literal's OWN body, not inside some other
// function the closure happens to invoke (that would be a call-graph
// analysis, deliberately out of scope here, exactly as
// lockorder_static_test.go's own INV-LOCK-E1 guard states for its
// one-level callee expansion). This is a real coverage limit, stated
// rather than papered over: a closure that calls a same-package helper
// which itself calls a provider adapter method is not caught. In this
// codebase's actual production code, every provider adapter call reachable
// from a database transaction closure is spelled out directly at the call
// site (CreateVerification/SubmitVerification's own phase-B blocks,
// LaunchGame's own phase-B block, payments' gate.go) rather than through an
// intermediate helper, so the lexical check is sufficient today; a future
// change that introduces such an intermediate helper should also update
// this guard's own doc comment or add call-graph expansion (code-reviewer
// checklist item, same convention INV-LOCK-E1 uses).
package txscope

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ioc1FlaggedMethods is the outbound-network method set of the three named
// provider interfaces (payments.PaymentProvider, casino.CasinoProvider,
// kyc.KYCProvider) - the ADAPTER methods ADR 0095 D1's phase B exists to
// keep off the tx-held path. Deliberately EXCLUDES each interface's own
// metadata/in-memory methods (ID, Capabilities, WebhookScheme,
// HealthStatus, HandleCallback - each interface's own doc comment states
// these never perform real network I/O; HandleCallback in particular
// parses bytes ALREADY received, verified before this method ever runs),
// since those are legitimately called from many contexts, including
// inside a transaction, with no I/O-under-lock hazard at all - flagging
// them would be noise, not a real finding, in the way the R4 lock-order
// guard's own "layer 2 is deliberately over-eager" note explicitly says
// its OWN over-breadth is an acceptable trade: over-breadth is acceptable
// when the false positive is cheap to fix; it is not free when it would
// require a design discussion every time (see conformance_test.go's own
// Bet/Win/Rollback calls, which the mock provider's own conformance suite
// - test code, out of this guard's scope by construction - is the only
// PRODUCTION-tree caller of those three names at all today, confirming
// they are provider-INITIATED shapes this platform receives, not shapes
// it calls outward, except through the exact phase-B call sites this
// guard's own tree-wide test already proves are clean).
var ioc1FlaggedMethods = map[string]bool{
	// casino.CasinoProvider
	"Catalogue": true, "Launch": true, "Balance": true, "Bet": true, "Win": true, "Rollback": true,
	// kyc.KYCProvider
	"CreateVerification": true, "SubmitVerification": true, "GetVerification": true,
	// payments.PaymentProvider
	"Deposit": true, "Withdraw": true, "QueryStatus": true,
	// sportsbook.Provider (SB-CATALOGUE-IO-1, ADR 0095 §33): FetchCatalogue
	// is the sole sanctioned caller of Provider.Catalogue, so flagging
	// FetchCatalogue itself (a package-level function, not a method on an
	// adapter value - ioc1CalleeName matches either shape, since both are
	// `*ast.SelectorExpr`) is what actually enforces "no provider I/O
	// lexically inside a db transaction closure" for this call site, the
	// same way flagging Deposit/Launch/CreateVerification does for their
	// own packages.
	"FetchCatalogue": true,
	// CAS-STMT-IO-1 (A3): the statement sources of the reconciliation
	// streams. CasinoStatementSource.Fetch and PaymentStatementSource.Fetch
	// are provider I/O for a real source (a statement API or file read), and
	// FetchCasinoStatement is the stream's single sanctioned caller of the
	// casino one; flagging both names keeps a statement fetch out of the
	// reconciliation match transaction - the original CAS-STMT-IO-1 defect,
	// where the casino statement was read inside the REPEATABLE READ tx.
	"Fetch": true, "FetchCasinoStatement": true,
}

// ioc1FlaggedBareFuncs are package-level helpers that perform provider I/O
// and are called BARE (an *ast.Ident callee, from inside their own package),
// which ioc1CalleeName's selector-only matching would not see. Kept to the
// helpers that need it; a bare-call name list for every adapter method would
// flag unrelated local helpers.
var ioc1FlaggedBareFuncs = map[string]bool{
	"FetchCasinoStatement": true,
}

// ioc1TxLocalReceivers are receiver identifiers whose Balance/Rollback methods
// are NOT provider adapters: a pgx transaction or savepoint (tx, sp:
// Rollback/Commit) and the ledger's locked-projection set (locked: Balance).
// They are exempt because the NAMED-function scan (A3 SR3-6) walks functions
// that take a pgx.Tx and therefore legitimately handle exactly these values.
// Lexical, by variable name: a provider value must not be given one of these
// names (a code-reviewer checklist item, the same limit as the rest of this
// guard).
var ioc1TxLocalReceivers = map[string]bool{"tx": true, "sp": true, "locked": true}

// ioc1TxClosureViolation is one adapter-method call found lexically inside
// a database-transaction closure.
type ioc1TxClosureViolation struct {
	pos    token.Position
	method string
}

func (v ioc1TxClosureViolation) String() string {
	return v.pos.String() + ": adapter method ." + v.method + "(...) called lexically inside a db transaction closure"
}

// ioc1IsTxClosureParam reports whether f's own parameter list declares a
// parameter of type pgx.Tx - the shape every db.Pool.With* callback
// (TxFunc: func(ctx context.Context, tx pgx.Tx) error) and every other
// hand-written transaction closure in this codebase shares. Matched
// textually (a *ast.SelectorExpr `pgx.Tx`), not by type-checking - this
// guard is pure go/ast + go/parser, no go/types, no build, matching this
// codebase's own established convention for this class of test (see
// internal/providerkind/completeness_scan.go's identical "no go/types"
// note) and keeping it independent of module resolution/build tags.
func ioc1IsTxClosureParam(f *ast.FuncLit) bool {
	return ioc1HasPgxTxParam(f.Type)
}

// ioc1HasPgxTxParam reports whether a function type (a literal's or a named
// declaration's) declares a pgx.Tx parameter.
func ioc1HasPgxTxParam(ft *ast.FuncType) bool {
	if ft == nil || ft.Params == nil {
		return false
	}
	for _, field := range ft.Params.List {
		sel, ok := field.Type.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "pgx" && sel.Sel.Name == "Tx" {
			return true
		}
	}
	return false
}

// ioc1CalleeName returns the method name of a `recv.Method(...)` call
// (the selector's own final identifier), or "" for any other call shape
// (a bare function call, a package-qualified function call with no
// receiver value, etc - those are never an interface method call on an
// adapter value).
func ioc1CalleeName(e ast.Expr) string {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	return sel.Sel.Name
}

// ioc1ScanFile parses one non-test .go file and returns every flagged
// adapter-method call found lexically inside a pgx.Tx-shaped closure
// literal anywhere in the file (a closure nested inside another closure is
// still in scope - ast.Inspect naturally recurses).
func ioc1ScanFile(path string, src []byte) ([]ioc1TxClosureViolation, int, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, 0, err
	}
	var violations []ioc1TxClosureViolation
	closuresSeen := 0
	reported := map[token.Pos]bool{} // a call nested in a tx scope inside another is one finding
	scanBody := func(body *ast.BlockStmt) {
		if body == nil {
			return
		}
		ast.Inspect(body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ioc1CalleeName(call.Fun)
			if id, ok := call.Fun.(*ast.Ident); ok && ioc1FlaggedBareFuncs[id.Name] {
				name = id.Name
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if recv, ok := sel.X.(*ast.Ident); ok && ioc1TxLocalReceivers[recv.Name] {
					return true
				}
			}
			if name != "" && (ioc1FlaggedMethods[name] || ioc1FlaggedBareFuncs[name]) && !reported[call.Pos()] {
				reported[call.Pos()] = true
				violations = append(violations, ioc1TxClosureViolation{pos: fset.Position(call.Pos()), method: name})
			}
			return true
		})
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch fn := n.(type) {
		case *ast.FuncLit:
			if ioc1IsTxClosureParam(fn) {
				closuresSeen++
				scanBody(fn.Body)
			}
		case *ast.FuncDecl:
			// A3 SR3-6 (security review): a NAMED function (or method) that takes
			// a pgx.Tx is, by construction, called with a transaction held, so a
			// provider call in its body is the same defect as one inside a
			// closure - the closure-only scan missed it.
			if ioc1HasPgxTxParam(fn.Type) {
				closuresSeen++
				scanBody(fn.Body)
			}
		}
		return true
	})
	return violations, closuresSeen, nil
}

// ioc1WalkNonTestGoFiles visits every non-test .go file under root.
func ioc1WalkNonTestGoFiles(root string, visit func(path string, src []byte) error) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		return visit(path, src)
	})
}

// ioc1RepoRoot mirrors webhookauth's constant_time_lint_test.go's identical
// helper: walk up from the working directory to the module root (the
// directory containing go.mod), so this test works regardless of which
// directory `go test` is invoked from.
func ioc1RepoRoot(t *testing.T) string {
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

// TestINV_IO_1c_NoAdapterCallInsideTxClosure is INV-IO-1(c) itself, made
// permanent: internal/casino, internal/kyc, internal/payments,
// internal/sportsbook and cmd/platform-api's own non-test source must
// never call a payments/casino/KYC/sportsbook provider adapter's outbound
// method (or, for sportsbook, FetchCatalogue - its sole sanctioned caller)
// lexically inside a database transaction closure
// (db.Pool.WithTenant/WithPrincipalScope/WithPlatformAdmin/WithPlayerScope/
// WithTenantSnapshot/WithTenantReadOnly/WithPlatformService, or any other
// hand-written closure taking a pgx.Tx parameter - matched structurally,
// not by the caller's own name, so this does not need updating every time
// db.Pool grows a new With* method).
func TestINV_IO_1c_NoAdapterCallInsideTxClosure(t *testing.T) {
	root := ioc1RepoRoot(t)
	dirs := []string{
		filepath.Join(root, "internal", "casino"),
		filepath.Join(root, "internal", "kyc"),
		filepath.Join(root, "internal", "payments"),
		// SB-CATALOGUE-IO-1 (ADR 0095 §33): sportsbook's FetchCatalogue is
		// called from both internal/sportsbook itself (helper/test
		// composition) and cmd/platform-api/main.go (the actual startup
		// call site) - both are in scope so the guard covers the real
		// production caller, not just a copy of its call shape in a test
		// file.
		filepath.Join(root, "internal", "sportsbook"),
		filepath.Join(root, "cmd", "platform-api"),
		// CAS-STMT-IO-1 (A3): the reconciliation streams fetch provider
		// statements (casino and payment); their match transactions must
		// never contain the fetch.
		filepath.Join(root, "internal", "reconciliation"),
	}

	var violations []ioc1TxClosureViolation
	totalClosures := 0
	for _, dir := range dirs {
		if err := ioc1WalkNonTestGoFiles(dir, func(path string, src []byte) error {
			v, closures, err := ioc1ScanFile(path, src)
			if err != nil {
				return err
			}
			violations = append(violations, v...)
			totalClosures += closures
			return nil
		}); err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	if totalClosures == 0 {
		t.Fatal("this guard found NO pgx.Tx-shaped closures at all across internal/casino, internal/kyc, " +
			"internal/payments, internal/sportsbook and cmd/platform-api - either the tree moved or the " +
			"walk/detection is broken, and a guard that inspects nothing proves nothing")
	}
	if len(violations) > 0 {
		var lines []string
		for _, v := range violations {
			lines = append(lines, v.String())
		}
		t.Fatalf("INV-IO-1(c) violated - a provider adapter method was called lexically inside a database "+
			"transaction closure (ADR 0095 D1's phase-B call sites must run OUTSIDE any pooled transaction):\n  %s",
			strings.Join(lines, "\n  "))
	}
}

// TestINV_IO_1c_GuardCatchesPlantedViolation is the guard's own required
// negative control (per this task's own "prove it catches a planted
// violation"): a synthetic file with a payments-shaped adapter call
// lexically inside a WithTenant-shaped closure must be flagged; the same
// call OUTSIDE any closure, and an in-closure call to a NON-flagged method
// (Capabilities, matching the "metadata calls are fine inside a tx" design
// note above), must not be.
func TestINV_IO_1c_GuardCatchesPlantedViolation(t *testing.T) {
	planted := `package x

func (o *Orchestrator) attemptDeposit(ctx context.Context, pool *db.Pool, tenantID uuid.UUID) error {
	return pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		result, err := provider.Deposit(ctx, DepositRequest{Amount: 100})
		_ = result
		return err
	})
}
`
	v, closures, err := ioc1ScanFile("planted.go", []byte(planted))
	if err != nil {
		t.Fatalf("parse planted violation: %v", err)
	}
	if closures == 0 {
		t.Fatal("test setup: the planted source's own closure was not even recognized as a pgx.Tx closure")
	}
	if len(v) != 1 || v[0].method != "Deposit" {
		t.Fatalf("expected exactly one Deposit violation, got %v", v)
	}

	safeOutsideClosure := `package x

func (o *Orchestrator) attemptDeposit(ctx context.Context, provider PaymentProvider) error {
	result, err := provider.Deposit(ctx, DepositRequest{Amount: 100})
	_ = result
	return err
}
`
	v, _, err = ioc1ScanFile("safe_outside.go", []byte(safeOutsideClosure))
	if err != nil {
		t.Fatalf("parse safe-outside case: %v", err)
	}
	if len(v) != 0 {
		t.Fatalf("expected no violations for an adapter call OUTSIDE any tx closure, got %v", v)
	}

	safeMetadataInClosure := `package x

func (o *Orchestrator) readCapabilities(ctx context.Context, pool *db.Pool, tenantID uuid.UUID, provider PaymentProvider) error {
	return pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		declared := provider.Capabilities()
		_ = declared
		return nil
	})
}
`
	v, closures, err = ioc1ScanFile("safe_metadata.go", []byte(safeMetadataInClosure))
	if err != nil {
		t.Fatalf("parse safe-metadata case: %v", err)
	}
	if closures == 0 {
		t.Fatal("test setup: the safe-metadata source's own closure was not recognized as a pgx.Tx closure")
	}
	if len(v) != 0 {
		t.Fatalf("expected no violations for a non-flagged metadata call (Capabilities) inside a tx closure, got %v", v)
	}
}

// TestINV_IO_1c_GuardCatchesStatementFetchInsideTx is CAS-STMT-IO-1's
// negative control: the original defect shape - a casino statement source
// called from inside the reconciliation REPEATABLE READ closure - and the
// FetchCasinoStatement helper called from one are both flagged, while the
// production order (fetch first, then open the transaction) is not.
func TestINV_IO_1c_GuardCatchesStatementFetchInsideTx(t *testing.T) {
	planted := `package x

func run(ctx context.Context, pool *db.Pool, tenantID uuid.UUID, source CasinoStatementSource) error {
	return pool.WithTenantSnapshot(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		stmt, err := source.Fetch(ctx, CasinoFetchRequest{TenantID: tenantID})
		_, _ = stmt, err
		fetched, err := FetchCasinoStatement(ctx, source, tenantID, start, end)
		_, _ = fetched, err
		return nil
	})
}
`
	v, closures, err := ioc1ScanFile("planted_stmt.go", []byte(planted))
	if err != nil {
		t.Fatal(err)
	}
	if closures != 1 {
		t.Fatalf("expected the planted snapshot closure to be recognized, got %d", closures)
	}
	got := map[string]bool{}
	for _, x := range v {
		got[x.method] = true
	}
	if len(v) != 2 || !got["Fetch"] || !got["FetchCasinoStatement"] {
		t.Fatalf("expected a Fetch and a FetchCasinoStatement violation, got %v", v)
	}

	safe := `package x

func run(ctx context.Context, pool *db.Pool, tenantID uuid.UUID, source CasinoStatementSource) error {
	fetched, err := FetchCasinoStatement(ctx, source, tenantID, start, end)
	if err != nil {
		return err
	}
	return pool.WithTenantSnapshot(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, _, err := RunCasinoStatement(ctx, tx, tenantID, start, end, fetched)
		return err
	})
}
`
	v, closures, err = ioc1ScanFile("safe_stmt.go", []byte(safe))
	if err != nil {
		t.Fatal(err)
	}
	if closures != 1 || len(v) != 0 {
		t.Fatalf("fetch-then-match must not be flagged (closures=%d violations=%v)", closures, v)
	}
}

// TestINV_IO_1c_DetectsTxClosureRegardlessOfCallerName proves the
// structural (parameter-shape) detection: a hand-written closure literal
// with a pgx.Tx parameter is recognized as a transaction closure even when
// it is not passed to a function literally named WithTenant/
// WithPrincipalScope/etc - this guard does not need updating every time
// db.Pool grows a new With* method, or when a closure of this shape is
// passed to some other helper entirely.
func TestINV_IO_1c_DetectsTxClosureRegardlessOfCallerName(t *testing.T) {
	src := `package x

func f(pool *db.Pool, ctx context.Context, tenantID uuid.UUID, provider CasinoProvider) error {
	return pool.SomeFutureHelperNotYetInvented(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := provider.Launch(ctx, LaunchRequest{})
		return err
	})
}
`
	v, closures, err := ioc1ScanFile("future_helper.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if closures != 1 {
		t.Fatalf("expected exactly one recognized tx closure regardless of the caller's own name, got %d", closures)
	}
	if len(v) != 1 || v[0].method != "Launch" {
		t.Fatalf("expected exactly one Launch violation, got %v", v)
	}
}

// TestINV_IO_1c_GuardCatchesProviderCallInNamedTxFunction is SR3-6's negative
// control: a NAMED function or method taking a pgx.Tx that calls a provider
// adapter method is flagged (the closure-only scan missed this shape), while a
// named function WITHOUT a pgx.Tx parameter, and a tx-taking function that
// calls only non-flagged methods, are not.
func TestINV_IO_1c_GuardCatchesProviderCallInNamedTxFunction(t *testing.T) {
	planted := `package x

func (o *Orchestrator) settle(ctx context.Context, tx pgx.Tx, provider CasinoProvider) error {
	_, err := provider.Balance(ctx, BalanceRequest{})
	return err
}

func fetchInHelper(ctx context.Context, tx pgx.Tx, source CasinoStatementSource) error {
	_, err := FetchCasinoStatement(ctx, source, id, a, b)
	return err
}
`
	v, n, err := ioc1ScanFile("named.go", []byte(planted))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, x := range v {
		got[x.method] = true
	}
	if n != 2 || len(v) != 2 || !got["Balance"] || !got["FetchCasinoStatement"] {
		t.Fatalf("expected the Balance and FetchCasinoStatement calls in named tx functions flagged (named=%d), got %v", n, v)
	}

	safe := `package x

func (o *Orchestrator) outside(ctx context.Context, provider CasinoProvider) error {
	_, err := provider.Balance(ctx, BalanceRequest{})
	return err
}

func (o *Orchestrator) meta(ctx context.Context, tx pgx.Tx, provider CasinoProvider) {
	_ = provider.Capabilities()
}
`
	v, n, err = ioc1ScanFile("named_safe.go", []byte(safe))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(v) != 0 {
		t.Fatalf("a function without a pgx.Tx and a metadata-only tx function must not be flagged (txFuncs=%d violations=%v)", n, v)
	}

	// A pgx transaction/savepoint and the locked-projection set are not adapters.
	exempt := `package x

func (o *Orchestrator) lockAndRollback(ctx context.Context, tx pgx.Tx, sp pgx.Tx) error {
	locked, err := ledger.LockProjectionsForPosting(ctx, tx, in)
	_, _ = locked.Balance(id)
	if err != nil {
		return sp.Rollback(ctx)
	}
	return nil
}
`
	v, _, err = ioc1ScanFile("exempt.go", []byte(exempt))
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		t.Fatalf("tx/savepoint Rollback and locked.Balance must not be flagged, got %v", v)
	}

	// A closure nested inside a named tx function is one finding, not two.
	nested := `package x

func (o *Orchestrator) outer(ctx context.Context, tx pgx.Tx, provider CasinoProvider) error {
	return run(func(ctx context.Context, tx pgx.Tx) error {
		_, err := provider.Launch(ctx, LaunchRequest{})
		return err
	})
}
`
	v, _, err = ioc1ScanFile("nested.go", []byte(nested))
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 1 || v[0].method != "Launch" {
		t.Fatalf("a call nested in a closure inside a named tx function must be reported once, got %v", v)
	}
}
