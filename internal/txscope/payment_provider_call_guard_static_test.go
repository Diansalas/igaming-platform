// PROV-OUTBOUND-CRED-1-LEGACY-PATH (E2), security review finding E2-C1
// (`docs/plans/prh2-hardening-round/reviews/e2-security.md`): IO-1C
// (`no_provider_call_in_tx_closure_static_test.go`) inspects only
// `*ast.FuncLit` closures with a `pgx.Tx` parameter. A planted NAMED
// method with a `pgx.Tx` parameter that calls `provider.Deposit` directly
// - exactly the deleted `attemptDeposit`'s own shape - compiles and
// passes IO-1C without being flagged, because it is not a closure
// literal at all.
//
// This guard closes that blind spot with a DIFFERENT, narrower and
// stronger rule than IO-1C's own tx-closure-shaped one: it does not ask
// "is this call lexically inside something shaped like a pgx.Tx
// closure?" at all. Instead it asks "is this call to one of
// payments.PaymentProvider's own outbound methods (Deposit, Withdraw,
// QueryStatus, and any future Refund) lexically inside one of the two
// shapes ADR 0095 §3.2 recognizes as the gate's own call site?":
//
//  1. inside the body of a FuncDecl NAMED depositAdapterCall,
//     payoutAdapterCall or payoutStatusQuery (the three named builders
//     that construct and return the AdapterCall closure `callProvider`
//     is later handed - drive.go, payout.go); or
//  2. inside a `*ast.FuncLit` passed DIRECTLY as one of the arguments of
//     a call to `callProvider` (sweeper.go's own inline AdapterCall
//     literal, which is never factored into a named builder).
//
// Every other call to one of these four method names, anywhere in a
// non-test .go file in this repository, in ANY package - not just
// internal/payments/casino/kyc the way IO-1C's own walk is scoped, since
// nothing stops a future caller in a different package from holding a
// payments.PaymentProvider value and calling it directly - is a
// violation, REGARDLESS of whether the enclosing function takes a
// pgx.Tx parameter at all. This is deliberately broader than "inside a
// tx closure": a named, non-tx-taking function that calls
// provider.Deposit() directly outside the gate is equally a
// PROV-OUTBOUND-CRED-1 bypass (no credential resolution, no txscope
// refusal, no manifest deadline, no panic recovery, no redaction), even
// though INV-IO-1(a)-(d) alone would not flag it. This guard therefore
// SUBSUMES the "extend IO-1C to ast.FuncDecl with a pgx.Tx parameter"
// alternative the security review offered for these four method names
// specifically: any named FuncDecl - with or without a pgx.Tx parameter -
// that calls one of them outside the two allowed shapes above is caught
// here, unconditionally. IO-1C itself is left unchanged and still covers
// every OTHER adapter method (casino, KYC, and payments' own
// HandleCallback/Capabilities/HealthStatus, none of which are outbound
// calls) exactly as before.
//
// Deliberately lexical/name-based only (go/ast + go/parser, no
// go/types), matching this codebase's own established convention for
// this class of guard (IO-1C's own doc comment states the identical
// trade-off and its identical coverage limit: a same-package helper that
// itself makes the call, one level removed from the literal call site,
// is not caught). In this codebase's actual call sites today, every
// outbound Deposit/Withdraw/QueryStatus call is spelled out directly at
// one of the two allowed shapes (never through an intermediate helper),
// so the lexical check is sufficient; a future change introducing such
// an intermediate helper should update this guard's own allow-list.
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

// pcg1FlaggedMethods is payments.PaymentProvider's own outbound method
// set: Deposit, Withdraw, QueryStatus (types.go), plus Refund (not yet
// defined on the interface, allow-listed here so it is caught from day
// one the moment a future change adds it, per the security review's own
// "any future Refund" instruction - never a silent gap until someone
// remembers to update this guard).
var pcg1FlaggedMethods = map[string]bool{
	"Deposit": true, "Withdraw": true, "QueryStatus": true, "Refund": true,
}

// pcg1AllowedBuilderNames is the closed set of named FuncDecls whose
// ENTIRE body is allowed to call a flagged method - the three adapter-
// call builders ADR 0095 §3.2's own ordering names as the gate's actual
// call sites (drive.go's depositAdapterCall, payout.go's
// payoutAdapterCall and payoutStatusQuery). A flagged-method call
// anywhere lexically inside one of these FuncDecls - including inside a
// FuncLit the FuncDecl constructs and returns - is allowed.
var pcg1AllowedBuilderNames = map[string]bool{
	"depositAdapterCall": true, "payoutAdapterCall": true, "payoutStatusQuery": true,
}

// pcg1Violation is one disallowed outbound-method call.
type pcg1Violation struct {
	pos    token.Position
	method string
}

func (v pcg1Violation) String() string {
	return v.pos.String() + ": ." + v.method + "(...) called outside the payments outbound-call gate (callProvider) or its named adapter-call builders"
}

// pcg1CalleeName returns a call's own callee name for either shape this
// guard needs to recognize: a bare, unqualified call (`callProvider(...)`,
// an *ast.Ident) or a method/selector call (`provider.Deposit(...)`, an
// *ast.SelectorExpr) - unlike IO-1C's own ioc1CalleeName (SelectorExpr
// only), this guard also needs to recognize the bare-identifier shape,
// since callProvider is called unqualified within its own package.
func pcg1CalleeName(e ast.Expr) string {
	switch n := e.(type) {
	case *ast.Ident:
		return n.Name
	case *ast.SelectorExpr:
		return n.Sel.Name
	default:
		return ""
	}
}

// pcg1Scanner walks one file's AST, threading "allowed" state down
// through FuncDecl/FuncLit boundaries per this guard's own two allowed
// shapes (see the package doc comment above). It implements ast.Visitor
// directly (rather than a single ast.Inspect closure) because the
// "allowed" state must flip only at specific node boundaries and must NOT
// leak sideways to a sibling closure that happens to run at the same
// syntactic depth - a plain unconditional recursive descent, exactly
// like IO-1C's own use of ast.Inspect per closure, but needing per-branch
// state here since two DIFFERENT node shapes (not just one) can flip it.
type pcg1Scanner struct {
	fset       *token.FileSet
	violations *[]pcg1Violation
	allowed    bool
}

func (s *pcg1Scanner) Visit(n ast.Node) ast.Visitor {
	switch node := n.(type) {
	case *ast.FuncDecl:
		allowed := s.allowed
		if node.Name != nil && pcg1AllowedBuilderNames[node.Name.Name] {
			allowed = true
		}
		return &pcg1Scanner{fset: s.fset, violations: s.violations, allowed: allowed}

	case *ast.CallExpr:
		name := pcg1CalleeName(node.Fun)
		if name == "callProvider" {
			// Every FuncLit argument is allowed for its own entire body
			// (shape 2); every OTHER argument keeps the CURRENT allowed
			// state (a flagged call could in principle appear in a
			// non-FuncLit argument expression, however unlikely - never
			// silently skipped).
			for _, arg := range node.Args {
				if lit, ok := arg.(*ast.FuncLit); ok {
					ast.Walk(&pcg1Scanner{fset: s.fset, violations: s.violations, allowed: true}, lit)
				} else {
					ast.Walk(s, arg)
				}
			}
			ast.Walk(s, node.Fun)
			return nil
		}
		if pcg1FlaggedMethods[name] && !s.allowed {
			*s.violations = append(*s.violations, pcg1Violation{pos: s.fset.Position(node.Pos()), method: name})
		}
		return s
	}
	return s
}

// pcg1ScanFile parses one non-test .go file and returns every flagged
// outbound-method call found outside this guard's two allowed shapes.
func pcg1ScanFile(path string, src []byte) ([]pcg1Violation, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var violations []pcg1Violation
	ast.Walk(&pcg1Scanner{fset: fset, violations: &violations, allowed: false}, file)
	return violations, nil
}

// pcg1SkipDirNames are NAMED directories this guard's whole-repository
// walk never descends into, on top of the dot-/underscore-prefix rule
// below: the two frontend (non-Go) app trees, which contain no .go files
// but are large enough to slow the walk down for nothing, and the
// standard Go-tool-ignored names (vendor, node_modules, testdata).
//
// Coordinator finding (2026-09-28, I-core integration): a raw
// filepath.WalkDir from the repository root also walks into
// .claude/worktrees/, where OTHER AGENTS' own full checkouts of this same
// repository live - their copies of this repo's own allowed call sites
// were flagged as violations when this guard ran from the main checkout,
// where that directory is populated. Fixed by doing what the go tool
// itself does (see `go help packages`: "Directory and file names that
// begin with '.' or '_' are ignored by the go tool, as are directories
// named 'testdata'") plus the two conventional dependency-vendoring names
// (vendor, node_modules) - not by naming .claude/worktrees specifically,
// which would only fix this one symptom and not the general problem of a
// nested checkout/build-output tree appearing anywhere under root.
var pcg1SkipDirNames = map[string]bool{
	"vendor": true, "node_modules": true, "testdata": true,
	"b2c": true, "backoffice": true,
}

// pcg1SkipDir reports whether a directory named name (not the walk's own
// root, which is never skipped even if it happens to start with '.' or
// '_') must never be descended into - go-tool convention (dot-/
// underscore-prefixed, per `go help packages`) plus pcg1SkipDirNames.
func pcg1SkipDir(name string) bool {
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
		return true
	}
	return pcg1SkipDirNames[name]
}

// pcg1WalkNonTestGoFiles visits every non-test .go file in the entire
// repository rooted at root - deliberately NOT scoped to
// internal/payments/casino/kyc the way IO-1C's own walk is, per the
// security review's own "scan all non-test packages" instruction: nothing
// stops a future caller anywhere in the tree from holding a
// payments.PaymentProvider value. Skips dot-/underscore-prefixed
// directories and the names in pcg1SkipDirNames (see their own doc
// comments) - in particular this keeps the walk out of any nested git
// worktree checkout of this same repository (e.g. .claude/worktrees/ on
// the main checkout), never assuming root itself is free of such trees.
func pcg1WalkNonTestGoFiles(root string, visit func(path string, src []byte) error) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && pcg1SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		return visit(path, src)
	})
}

// TestPCG1_NoOutboundProviderCallOutsideTheGate is E2-C1 itself, made
// permanent: no non-test .go file anywhere in this repository may call
// payments.PaymentProvider's Deposit/Withdraw/QueryStatus/Refund except
// from inside callProvider's own named adapter-call builders or a
// FuncLit passed directly to callProvider.
func TestPCG1_NoOutboundProviderCallOutsideTheGate(t *testing.T) {
	root := ioc1RepoRoot(t)

	var violations []pcg1Violation
	filesScanned := 0
	if err := pcg1WalkNonTestGoFiles(root, func(path string, src []byte) error {
		v, err := pcg1ScanFile(path, src)
		if err != nil {
			return err
		}
		violations = append(violations, v...)
		filesScanned++
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	if filesScanned < 100 {
		t.Fatalf("this guard scanned only %d non-test .go files across the whole repository - "+
			"either the walk is broken or the tree moved, and a guard that scans almost nothing "+
			"proves almost nothing", filesScanned)
	}
	if len(violations) > 0 {
		var lines []string
		for _, v := range violations {
			lines = append(lines, v.String())
		}
		t.Fatalf("PROV-OUTBOUND-CRED-1 bypass(es) found - an outbound payments provider method was "+
			"called outside callProvider's own gate:\n  %s", strings.Join(lines, "\n  "))
	}
}

// TestPCG1_GuardCatchesPlantedNamedMethodWithTxParam is the guard's own
// required negative control for the EXACT shape the security review
// demonstrated defeats IO-1C: a named method taking a pgx.Tx parameter
// (never a closure literal at all) that calls provider.Deposit directly.
// IO-1C's own guard does not flag this (it inspects FuncLit closures
// only); this guard must.
func TestPCG1_GuardCatchesPlantedNamedMethodWithTxParam(t *testing.T) {
	planted := `package payments

func (o *Orchestrator) attemptDeposit(ctx context.Context, tx pgx.Tx, provider PaymentProvider) error {
	_, err := provider.Deposit(ctx, DepositRequest{Amount: 100})
	return err
}
`
	v, err := pcg1ScanFile("planted_named_method.go", []byte(planted))
	if err != nil {
		t.Fatalf("parse planted violation: %v", err)
	}
	if len(v) != 1 || v[0].method != "Deposit" {
		t.Fatalf("expected exactly one Deposit violation for a named method with a pgx.Tx param, got %v", v)
	}
}

// TestPCG1_GuardCatchesPlantedCallFromAnotherPackage proves the guard is
// package-independent (purely structural, per its own doc comment): a
// call from a DIFFERENT package's own function - not internal/payments,
// not a method on *Orchestrator at all - that calls .Deposit(...) on a
// provider it holds, outside any allowed shape, is flagged identically.
func TestPCG1_GuardCatchesPlantedCallFromAnotherPackage(t *testing.T) {
	planted := `package sportsbook

func settleViaPaymentsProvider(ctx context.Context, provider payments.PaymentProvider) error {
	_, err := provider.Deposit(ctx, payments.DepositRequest{Amount: 100})
	return err
}
`
	v, err := pcg1ScanFile("planted_other_package.go", []byte(planted))
	if err != nil {
		t.Fatalf("parse planted violation: %v", err)
	}
	if len(v) != 1 || v[0].method != "Deposit" {
		t.Fatalf("expected exactly one Deposit violation for a call from another package, got %v", v)
	}
}

// TestPCG1_AllowsTheThreeNamedBuilders proves the positive control for
// shape 1: a flagged call lexically inside depositAdapterCall,
// payoutAdapterCall or payoutStatusQuery - including inside a FuncLit
// those builders construct and return - is never flagged.
func TestPCG1_AllowsTheThreeNamedBuilders(t *testing.T) {
	planted := `package payments

func depositAdapterCall(provider PaymentProvider, attempt PaymentAttempt, manifest OperationManifest) AdapterCall[DepositResult] {
	return func(callCtx context.Context, cc CallContext) (DepositResult, ErrorClass, error) {
		res, err := provider.Deposit(callCtx, DepositRequest{Amount: attempt.Amount})
		return res, ErrorClassPending, err
	}
}

func payoutAdapterCall(provider PaymentProvider, attempt PaymentAttempt) AdapterCall[WithdrawResult] {
	return func(callCtx context.Context, cc CallContext) (WithdrawResult, ErrorClass, error) {
		res, err := provider.Withdraw(callCtx, WithdrawRequest{Amount: attempt.Amount})
		return res, ErrorClassPending, err
	}
}

func payoutStatusQuery(provider PaymentProvider, providerReference string) AdapterCall[StatusResult] {
	return func(callCtx context.Context, cc CallContext) (StatusResult, ErrorClass, error) {
		return provider.QueryStatus(callCtx, providerReference)
	}
}
`
	v, err := pcg1ScanFile("allowed_builders.go", []byte(planted))
	if err != nil {
		t.Fatalf("parse allowed-builders case: %v", err)
	}
	if len(v) != 0 {
		t.Fatalf("expected no violations for calls inside the three named builders, got %v", v)
	}
}

// TestPCG1_AllowsAFuncLitPassedDirectlyToCallProvider proves the positive
// control for shape 2: sweeper.go's own inline AdapterCall literal,
// passed directly as an argument to callProvider, is never flagged, while
// a flagged call OUTSIDE that literal (in the same file, at the same
// syntactic depth as the callProvider call itself) still is.
func TestPCG1_AllowsAFuncLitPassedDirectlyToCallProvider(t *testing.T) {
	planted := `package payments

func (s *Sweeper) pollOne(ctx context.Context, provider PaymentProvider, attempt PaymentAttempt) GateResult[StatusResult] {
	return callProvider(ctx, s.Pool, s.CredResolver, in, func(callCtx context.Context, cc CallContext) (StatusResult, ErrorClass, error) {
		res, err := provider.QueryStatus(callCtx, *attempt.ProviderReference)
		return res, ErrorClassPending, err
	})
}

func (s *Sweeper) bypassed(ctx context.Context, provider PaymentProvider) error {
	_, err := provider.QueryStatus(ctx, "ref")
	return err
}
`
	v, err := pcg1ScanFile("allowed_funclit_arg.go", []byte(planted))
	if err != nil {
		t.Fatalf("parse funclit-arg case: %v", err)
	}
	if len(v) != 1 || v[0].method != "QueryStatus" {
		t.Fatalf("expected exactly one QueryStatus violation (the bypassed function, never the callProvider argument), got %v", v)
	}
}

// TestPCG1_DoesNotFlagNonOutboundMethods proves this guard's own name set
// is exactly the four outbound methods - metadata/inbound methods sharing
// no name with them (Capabilities, HealthStatus, HandleCallback) are
// never flagged, mirroring IO-1C's own "metadata calls are fine" design
// note (its ioc1FlaggedMethods deliberately excludes them for the
// identical reason).
func TestPCG1_DoesNotFlagNonOutboundMethods(t *testing.T) {
	planted := `package payments

func (o *Orchestrator) readHealth(ctx context.Context, provider PaymentProvider) error {
	_, err := provider.HealthStatus(ctx)
	return err
}
`
	v, err := pcg1ScanFile("safe_metadata.go", []byte(planted))
	if err != nil {
		t.Fatalf("parse safe-metadata case: %v", err)
	}
	if len(v) != 0 {
		t.Fatalf("expected no violations for a non-outbound method call, got %v", v)
	}
}
