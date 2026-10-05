package kyc

// PRH-2 E1 static guards (ADR 0106 sections 7.1 and 10.4; pure go/ast, no
// database, no build tag):
//
//   - tests 32(c) and 32(d): every UPDATE of kyc_submission_outbox in non-test Go
//     lives in internal/kyc/outbox*.go and every tenant-side one carries
//     `claim_token = $n` in its WHERE (the claim statement and the
//     verification_not_submitted cascade are the two named exceptions); no KYC
//     vendor outbound call outside outbox_worker.go, and no exported
//     internal/kyc function (other than the worker's own entry points) reaches one;
//   - test 34 (IC T-C / INV-KYC-OB-5): enforcement, payments and withdrawal code
//     never reference the outbox, and never call the staff-only derived
//     submission state (IC R1);
//   - the worker identity is confined to claimNext, whose closure runs exactly
//     one statement over the outbox table only (the db package statics check the
//     identity side; this checks the kyc side);
//   - the worker never reads document content or storage references (content
//     read in phase B is NOT BUILT; the rule is pinned here).

import (
	"fmt"
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

func kycRepoRoot(t *testing.T) string {
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

type srcFile struct {
	rel  string
	fset *token.FileSet
	ast  *ast.File
	text string
}

// nonTestGoFiles parses every non-test Go file under the given top-level dirs.
func nonTestGoFiles(t *testing.T, tops ...string) []srcFile {
	t.Helper()
	root := kycRepoRoot(t)
	var out []srcFile
	for _, top := range tops {
		err := filepath.Walk(filepath.Join(root, top), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, b, parser.ParseComments)
			if perr != nil {
				t.Fatalf("parse %s: %v", path, perr)
			}
			rel, _ := filepath.Rel(root, path)
			out = append(out, srcFile{rel: filepath.ToSlash(rel), fset: fset, ast: f, text: string(b)})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// stringLiterals returns every string literal in f (comments excluded),
// unquoted.
func stringLiterals(f *ast.File) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING {
			if s, err := strconv.Unquote(bl.Value); err == nil {
				out = append(out, s)
			}
		}
		return true
	})
	return out
}

var outboxUpdateRe = regexp.MustCompile(`(?is)\bUPDATE\s+kyc_submission_outbox\b`)
var claimTokenCASRe = regexp.MustCompile(`(?is)\bWHERE\b.*\bclaim_token\s*=\s*\$\d+`)

// outboxUpdateViolations returns, for the given parsed files, the UPDATE
// statements that break the F8(c) rules.
func outboxUpdateViolations(files []srcFile) (violations []string, seen int) {
	for _, f := range files {
		for _, lit := range stringLiterals(f.ast) {
			if !outboxUpdateRe.MatchString(lit) {
				continue
			}
			seen++
			base := filepath.Base(f.rel)
			inKYCOutbox := strings.HasPrefix(f.rel, "internal/kyc/") && strings.HasPrefix(base, "outbox") && strings.HasSuffix(base, ".go")
			if !inKYCOutbox {
				violations = append(violations, f.rel+": UPDATE kyc_submission_outbox outside internal/kyc/outbox*.go")
				continue
			}
			claimStmt := strings.Contains(lit, "SKIP LOCKED")
			cascade := strings.Contains(lit, "'verification_not_submitted'") && strings.Contains(lit, "state = 'pending'")
			if !claimStmt && !cascade && !claimTokenCASRe.MatchString(lit) {
				violations = append(violations, f.rel+": tenant-side UPDATE kyc_submission_outbox without claim_token = $n in its WHERE")
			}
		}
	}
	return violations, seen
}

func TestStatic_OutboxUpdatesConfinedAndClaimTokenCAS_F8c(t *testing.T) {
	v, seen := outboxUpdateViolations(nonTestGoFiles(t, "internal", "cmd"))
	for _, m := range v {
		t.Error(m)
	}
	if seen < 6 {
		t.Fatalf("expected the claim, sent, cancel, retry, terminal and cascade statements, saw %d (guard is vacuous)", seen)
	}
}

// Negative controls: the guard must FIRE on known-bad fixtures.
func TestStatic_OutboxUpdateGuard_NegativeControls(t *testing.T) {
	mk := func(rel, lit string) srcFile {
		src := "package p\nconst q = `" + lit + "`\n"
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, rel, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		return srcFile{rel: rel, fset: fset, ast: f, text: src}
	}
	cases := []struct {
		name string
		file srcFile
		want int
	}{
		{"outside outbox*.go", mk("internal/httpserver/x.go", "UPDATE kyc_submission_outbox SET state = 'sent' WHERE id = $1 AND claim_token = $2"), 1},
		{"in kyc but not outbox*.go", mk("internal/kyc/x.go", "UPDATE kyc_submission_outbox SET state = 'sent' WHERE id = $1 AND claim_token = $2"), 1},
		{"tenant-side without claim_token", mk("internal/kyc/outbox_x.go", "UPDATE kyc_submission_outbox SET state = 'sent' WHERE id = $1 AND state = 'claimed'"), 1},
		{"claim_token only in SET, not WHERE", mk("internal/kyc/outbox_x.go", "UPDATE kyc_submission_outbox SET claim_token = $2 WHERE id = $1"), 1},
		{"a compliant CAS", mk("internal/kyc/outbox_x.go", "UPDATE kyc_submission_outbox SET state = 'sent' WHERE id = $1 AND state = 'claimed' AND claim_token = $2"), 0},
	}
	for _, c := range cases {
		v, _ := outboxUpdateViolations([]srcFile{c.file})
		if len(v) != c.want {
			t.Errorf("%s: violations = %d (%v), want %d", c.name, len(v), v, c.want)
		}
	}
}

// vendorCallers returns, over files, the set of functions (by name) whose body
// contains a call to a selector named CreateVerification or SubmitVerification
// (the KYCProvider vendor methods), keyed "rel:Name".
func funcsReachingVendor(files []srcFile) (direct map[string]bool, exported map[string]bool) {
	type fn struct {
		key, name string
		exported  bool
		callees   map[string]bool
		vendor    bool
	}
	var fns []*fn
	byName := map[string][]*fn{}
	for _, f := range files {
		for _, d := range f.ast.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			recvExported := true
			if fd.Recv != nil && len(fd.Recv.List) > 0 {
				recvExported = ast.IsExported(recvTypeName(fd.Recv.List[0].Type))
			}
			x := &fn{key: f.rel + ":" + fd.Name.Name, name: fd.Name.Name, exported: ast.IsExported(fd.Name.Name) && recvExported, callees: map[string]bool{}}
			// EVERY selector and identifier reference counts, not only call
			// expressions: a method value (f := p.CreateVerification) or a
			// function value (g := helper) reaches the vendor just as a call does
			// (code review N10).
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch e := n.(type) {
				case *ast.SelectorExpr:
					x.callees[e.Sel.Name] = true
					if e.Sel.Name == "CreateVerification" || e.Sel.Name == "SubmitVerification" {
						x.vendor = true
					}
				case *ast.Ident:
					x.callees[e.Name] = true
				}
				return true
			})
			fns = append(fns, x)
			byName[x.name] = append(byName[x.name], x)
		}
	}
	reach := map[*fn]bool{}
	direct = map[string]bool{}
	for _, x := range fns {
		if x.vendor {
			reach[x] = true
			direct[x.key] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for _, x := range fns {
			if reach[x] {
				continue
			}
			for c := range x.callees {
				for _, target := range byName[c] {
					if reach[target] {
						reach[x] = true
						changed = true
					}
				}
				if reach[x] {
					break
				}
			}
		}
	}
	exported = map[string]bool{}
	for x := range reach {
		if x.exported {
			exported[x.key] = true
		}
	}
	return direct, exported
}

func recvTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return recvTypeName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return recvTypeName(t.X)
	}
	return ""
}

func TestStatic_NoVendorCallOutsideWorkerAndNoExportedPathToIt_Q_R3(t *testing.T) {
	files := nonTestGoFiles(t, "internal", "cmd")

	// (d1) no non-test file outside outbox_worker.go calls the vendor methods.
	var calls int
	for _, f := range files {
		ast.Inspect(f.ast, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr) // calls AND method values
			if !ok || (sel.Sel.Name != "CreateVerification" && sel.Sel.Name != "SubmitVerification") {
				return true
			}
			calls++
			if f.rel != "internal/kyc/outbox_worker.go" {
				t.Errorf("%s:%d: KYC vendor reference %s outside internal/kyc/outbox_worker.go", f.rel, f.fset.Position(sel.Pos()).Line, sel.Sel.Name)
			}
			return true
		})
	}
	if calls < 2 {
		t.Fatalf("expected the worker's create and submit vendor calls, saw %d (guard is vacuous)", calls)
	}

	// (d2) within internal/kyc: no exported function other than the worker's own
	// entry points transitively reaches a vendor call.
	var kycFiles []srcFile
	for _, f := range files {
		if strings.HasPrefix(f.rel, "internal/kyc/") && !strings.Contains(strings.TrimPrefix(f.rel, "internal/kyc/"), "/") {
			kycFiles = append(kycFiles, f)
		}
	}
	direct, exported := funcsReachingVendor(kycFiles)
	if !direct["internal/kyc/outbox_worker.go:callVendor"] {
		t.Fatalf("expected callVendor to be the direct vendor caller, got %v (guard is vacuous)", direct)
	}
	allowed := map[string]bool{
		"internal/kyc/outbox_worker.go:RunPass":             true,
		"internal/kyc/outbox_worker.go:RunOutboxWorkerLoop": true,
	}
	var bad []string
	for k := range exported {
		if !allowed[k] {
			bad = append(bad, k)
		}
	}
	sort.Strings(bad)
	if len(bad) != 0 {
		t.Errorf("exported internal/kyc function(s) reach a KYC vendor call: %v", bad)
	}
	if len(exported) != len(allowed) {
		t.Errorf("expected exactly the worker entry points %v to be exported and reach the vendor, got %v", allowed, exported)
	}
}

// Negative control for (d2): a fixture with an exported function reaching the
// vendor through an unexported helper is flagged.
func TestStatic_ExportedVendorPathGuard_NegativeControl(t *testing.T) {
	src := `package kyc
func helper(p KYCProvider) { p.CreateVerification(nil, CreateVerificationInput{}) }
func Exported(p KYCProvider) { helper(p) }
func Harmless() {}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "internal/kyc/x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, exported := funcsReachingVendor([]srcFile{{rel: "internal/kyc/x.go", fset: fset, ast: f, text: src}})
	if !exported["internal/kyc/x.go:Exported"] || exported["internal/kyc/x.go:Harmless"] {
		t.Fatalf("the guard must flag Exported and only Exported, got %v", exported)
	}
}

// Negative control for N10: an exported function that takes the vendor method
// as a VALUE (never calling it itself) and one that stores a helper as a value
// are both flagged.
func TestStatic_ExportedVendorPathGuard_MethodValueNegativeControl(t *testing.T) {
	src := `package kyc
func helper(p KYCProvider) func() { return func() { p.CreateVerification(nil, CreateVerificationInput{}) } }
func ViaMethodValue(p KYCProvider) { f := p.SubmitVerification; _ = f }
func ViaFunctionValue(p KYCProvider) { g := helper; _ = g }
func Harmless() {}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "internal/kyc/x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, exported := funcsReachingVendor([]srcFile{{rel: "internal/kyc/x.go", fset: fset, ast: f, text: src}})
	if !exported["internal/kyc/x.go:ViaMethodValue"] || !exported["internal/kyc/x.go:ViaFunctionValue"] || exported["internal/kyc/x.go:Harmless"] {
		t.Fatalf("the guard must flag the method-value and function-value paths and not Harmless, got %v", exported)
	}
}

// INV-KYC-OB-5 / IC T-C: enforcement, payments and withdrawal code never
// reference the outbox table, and (IC R1) never call the staff-only derived
// submission state.
// outboxReaderReferences returns every selector or identifier reference (not
// only calls) to a function that reads or writes the outbox or the derived
// staff-only state.
func outboxReaderReferences(f srcFile) []string {
	banned := map[string]bool{
		"StaffSubmissionState": true, "StaffSubmissionStates": true, "deriveSubmissionState": true,
		"RequestVerification": true, "enqueueSubmitForCurrentSet": true, "enqueueSubmitRow": true,
	}
	var out []string
	ast.Inspect(f.ast, func(n ast.Node) bool {
		// A selector's Sel is itself an Ident, so identifiers cover both forms.
		if e, ok := n.(*ast.Ident); ok && banned[e.Name] {
			out = append(out, fmt.Sprintf("%s (line %d)", e.Name, f.fset.Position(e.Pos()).Line))
		}
		return true
	})
	return out
}

func TestStatic_OutboxReaderReferenceGuard_NegativeControl(t *testing.T) {
	src := `package payments
func gate() { _ = kyc.StaffSubmissionStates; kyc.RequestVerification(nil, nil, kyc.CreateVerificationParams{}, "x") }
func fine() { kyc.EvaluateEnforcement() }
`
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, "internal/payments/x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	refs := outboxReaderReferences(srcFile{rel: "internal/payments/x.go", fset: fset, ast: af, text: src})
	if len(refs) != 2 {
		t.Fatalf("expected the value reference and the call to be flagged (2), got %v", refs)
	}
}

func TestStatic_EnforcementPaymentsWithdrawalNeverReadOutbox_INVKYCOB5(t *testing.T) {
	var scanned int
	for _, f := range nonTestGoFiles(t, "internal") {
		base := filepath.Base(f.rel)
		inScope := (strings.HasPrefix(f.rel, "internal/kyc/") && strings.HasPrefix(base, "enforcement")) ||
			strings.HasPrefix(f.rel, "internal/payments/") || strings.HasPrefix(f.rel, "internal/withdrawal/")
		if !inScope {
			continue
		}
		scanned++
		if strings.Contains(f.text, "kyc_submission_outbox") {
			t.Errorf("%s references kyc_submission_outbox: enforcement must be independent of outbox state (INV-KYC-OB-5)", f.rel)
		}
		for _, ref := range outboxReaderReferences(f) {
			t.Errorf("%s references %s: an exported function that reads or writes the outbox must never be used by enforcement, payments or withdrawal code (IC R1, IC F-6)", f.rel, ref)
		}
	}
	if scanned < 5 {
		t.Fatalf("expected enforcement*.go plus the payments and withdrawal packages, scanned %d files (guard is vacuous)", scanned)
	}
}

// The derived state is called ONLY by staff handlers: the only callers outside
// internal/kyc are in internal/httpserver/kyc_admin_handlers.go.
func TestStatic_SubmissionStateCalledOnlyByStaffHandlers_ICR1(t *testing.T) {
	var callers []string
	for _, f := range nonTestGoFiles(t, "internal", "cmd") {
		if f.rel == "internal/kyc/submission_state.go" {
			continue
		}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "StaffSubmissionStates" || sel.Sel.Name == "StaffSubmissionState") {
				callers = append(callers, f.rel)
			}
			return true
		})
	}
	if len(callers) == 0 {
		t.Fatal("expected the staff handlers to call the derived state (guard is vacuous)")
	}
	for _, c := range callers {
		if c != "internal/httpserver/kyc_admin_handlers.go" {
			t.Errorf("%s calls the staff-only derived submission state", c)
		}
	}
}

// claimNext is the only user of db.ServiceKYCSubmissionWorker outside the db
// package's own definition, and its closure runs exactly one statement, over
// kyc_submission_outbox only.
func TestStatic_WorkerIdentityConfinedToClaimNextOneStatement(t *testing.T) {
	var uses int
	for _, f := range nonTestGoFiles(t, "internal", "cmd") {
		if f.rel == "internal/db/platform_service.go" {
			continue
		}
		for _, d := range f.ast.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "ServiceKYCSubmissionWorker" {
					return true
				}
				uses++
				if f.rel != "internal/kyc/outbox_worker.go" || fd.Name.Name != "claimNext" {
					t.Errorf("%s:%s references db.ServiceKYCSubmissionWorker: only internal/kyc/outbox_worker.go:claimNext may", f.rel, fd.Name.Name)
				}
				return true
			})
		}
	}
	if uses < 2 { // WithPlatformService and AssertPlatformServiceScope
		t.Fatalf("expected claimNext to use the identity twice, saw %d (guard is vacuous)", uses)
	}

	// claimNext's WithPlatformService closure: exactly one tx.* statement call.
	for _, f := range nonTestGoFiles(t, "internal/kyc") {
		if f.rel != "internal/kyc/outbox_worker.go" {
			continue
		}
		for _, d := range f.ast.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Name.Name != "claimNext" {
				continue
			}
			var stmts int
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "tx" {
						switch sel.Sel.Name {
						case "Exec", "Query", "QueryRow", "SendBatch", "CopyFrom", "Begin":
							stmts++
						}
					}
				}
				return true
			})
			if stmts != 1 {
				t.Errorf("claimNext's transaction must run exactly one statement, found %d", stmts)
			}
		}
	}
	// The claim statement references only kyc_submission_outbox.
	for _, other := range []string{"kyc_verifications", "kyc_documents", "audit_log", "tenants", "alerts", "staff_users"} {
		if strings.Contains(claimSQL, other) {
			t.Errorf("the claim statement must reference only kyc_submission_outbox, found %s", other)
		}
	}
}

// Document content and storage references are never read by the worker: phase
// B has no content seam today (NOT BUILT), and the rule that a future one reads
// in phase B only is pinned by keeping the worker away from both.
func TestStatic_WorkerNeverReadsDocumentContentOrStorageReferences(t *testing.T) {
	for _, f := range nonTestGoFiles(t, "internal/kyc") {
		base := filepath.Base(f.rel)
		if base != "outbox_worker.go" && base != "outbox.go" && base != "submission_state.go" {
			continue
		}
		for _, lit := range stringLiterals(f.ast) {
			if strings.Contains(lit, "storage_reference") {
				t.Errorf("%s selects storage_reference: the outbox worker never reads storage references", f.rel)
			}
		}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Retrieve" || sel.Sel.Name == "GetDocumentContent") {
				t.Errorf("%s:%d reads document content: not allowed in the outbox worker", f.rel, f.fset.Position(call.Pos()).Line)
			}
			return true
		})
	}
}

// ClaimScope is a TEST SEAM (nil in production): no non-test file may assign or
// read it other than the worker itself, and cmd/platform-api must not set it.
func TestStatic_ClaimScopeSeamNeverSetOutsideTests(t *testing.T) {
	var mentions int
	for _, f := range nonTestGoFiles(t, "internal", "cmd") {
		ast.Inspect(f.ast, func(n ast.Node) bool {
			var pos token.Pos
			switch e := n.(type) {
			case *ast.SelectorExpr: // w.ClaimScope = ... / reads
				if e.Sel.Name != "ClaimScope" {
					return true
				}
				pos = e.Pos()
			case *ast.KeyValueExpr: // OutboxWorker{ClaimScope: ...} (a keyed composite literal)
				id, ok := e.Key.(*ast.Ident)
				if !ok || id.Name != "ClaimScope" {
					return true
				}
				pos = e.Pos()
			default:
				return true
			}
			mentions++
			if f.rel != "internal/kyc/outbox_worker.go" {
				t.Errorf("%s:%d references OutboxWorker.ClaimScope: it is a test seam, nil in production", f.rel, f.fset.Position(pos).Line)
			}
			return true
		})
	}
	if mentions == 0 {
		t.Fatal("expected claimNext to read ClaimScope (guard is vacuous)")
	}
}

// Every worker log call carries only allowlisted argument forms (security L-2 and
// C-1b, code review R4): string literals, RedactedProviderErrorDetail(...), a
// conversion string(row.X) or string(ClassX), or row.X.String(). Anything else -
// an error variable (whatever its name), .Error(), fmt.Sprint(err), an aliased
// error - is rejected by DETECTION, never by guessing names. Every logging
// receiver in the file is covered: slog.*, logger.*, w.Logger.* and
// w.logger().*. The one exemption is the loop's start-refusal line, whose error
// is a ValidateForLoop message made of constant configuration strings.
func TestStatic_WorkerLogCallsNeverCarryRawErrorText_L2(t *testing.T) {
	var f srcFile
	for _, x := range nonTestGoFiles(t, "internal") {
		if x.rel == "internal/kyc/outbox_worker.go" {
			f = x
		}
	}
	if f.ast == nil {
		t.Fatal("outbox_worker.go not found")
	}
	var errs []string
	logCalls, detailArgs := workerLogViolations(f, &errs)
	for _, e := range errs {
		t.Error(e)
	}
	if logCalls < 10 || detailArgs < 4 {
		t.Fatalf("expected the worker's log calls (>=10) and detail attributes (>=4), saw %d and %d (guard is vacuous)", logCalls, detailArgs)
	}
}

var logMethodNames = map[string]bool{"Error": true, "Warn": true, "Info": true, "Debug": true, "Log": true,
	"ErrorContext": true, "WarnContext": true, "InfoContext": true, "DebugContext": true}

func isLogReceiver(x ast.Expr) bool {
	switch e := x.(type) {
	case *ast.Ident:
		return e.Name == "slog" || e.Name == "logger"
	case *ast.SelectorExpr:
		return e.Sel.Name == "Logger"
	case *ast.CallExpr:
		if s, ok := e.Fun.(*ast.SelectorExpr); ok {
			return s.Sel.Name == "logger"
		}
	}
	return false
}

func rootIdent(x ast.Expr) string {
	for {
		switch e := x.(type) {
		case *ast.SelectorExpr:
			x = e.X
		case *ast.Ident:
			return e.Name
		default:
			return ""
		}
	}
}

// allowedLogArg reports whether a (non-message) log argument is an allowlisted form.
func allowedLogArg(a ast.Expr) bool {
	e, isCall := a.(*ast.CallExpr)
	if _, isLit := a.(*ast.BasicLit); isLit {
		return true
	}
	if !isCall {
		return false
	}
	switch fn := e.Fun.(type) {
	case *ast.Ident:
		if fn.Name == "RedactedProviderErrorDetail" {
			return true
		}
		if fn.Name == "string" && len(e.Args) == 1 {
			switch in := e.Args[0].(type) {
			case *ast.SelectorExpr:
				return rootIdent(in) == "row"
			case *ast.Ident:
				return strings.HasPrefix(in.Name, "Class")
			}
		}
	case *ast.SelectorExpr:
		return fn.Sel.Name == "String" && len(e.Args) == 0 && rootIdent(fn.X) == "row"
	}
	return false
}

// workerLogViolations scans f and appends one message per violation.
func workerLogViolations(f srcFile, out *[]string) (logCalls, detailArgs int) {
	ast.Inspect(f.ast, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !logMethodNames[sel.Sel.Name] || !isLogReceiver(sel.X) {
			return true
		}
		logCalls++
		line := f.fset.Position(call.Pos()).Line
		if len(call.Args) > 0 {
			if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Value == `"kyc outbox worker: refusing to start"` {
				return true // constant configuration strings only
			}
		}
		for i, a := range call.Args {
			if i == 0 {
				continue // the message
			}
			if lit, ok := a.(*ast.BasicLit); ok && lit.Value == `"detail"` && i+1 < len(call.Args) {
				detailArgs++
				c, isCall := call.Args[i+1].(*ast.CallExpr)
				if !isCall {
					*out = append(*out, fmt.Sprintf("line %d: a log detail must be RedactedProviderErrorDetail(err)", line))
				} else if id, isID := c.Fun.(*ast.Ident); !isID || id.Name != "RedactedProviderErrorDetail" {
					*out = append(*out, fmt.Sprintf("line %d: a log detail must be RedactedProviderErrorDetail(err)", line))
				}
			}
			if !allowedLogArg(a) {
				*out = append(*out, fmt.Sprintf("line %d: log argument %d is not an allowlisted form (literal, RedactedProviderErrorDetail(...), string(row.X), row.X.String()): an error value or raw text may reach the log", line, i))
			}
		}
		return true
	})
	return logCalls, detailArgs
}

// Negative controls: every leak shape the reviewers proved is detected, the
// compliant call is not, and a non-call "detail" value does not panic.
func TestStatic_WorkerLogGuard_NegativeControls_L2(t *testing.T) {
	src := `package kyc
func f(w *OutboxWorker, row claimedRow, err error, callErr error) {
	cause := callErr
	w.logger().Warn("a", "cause", cause)
	slog.Warn("b", "e", err)
	w.Logger.Warn("c", "e", err)
	logger.Error("d", "e", err.Error())
	w.logger().Error("e", "detail", err)
	w.logger().Error("f", "x", fmt.Sprint(err))
	w.logger().Error("ok", "detail", RedactedProviderErrorDetail(err), "outbox_id", row.ID.String(), "class", string(ClassAmbiguous), "n", "lit")
}
`
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, "internal/kyc/outbox_worker.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var errs []string
	calls, _ := workerLogViolations(srcFile{rel: "internal/kyc/outbox_worker.go", fset: fset, ast: af, text: src}, &errs)
	flagged := map[string]bool{}
	for _, e := range errs {
		flagged[strings.SplitN(e, ":", 2)[0]] = true
	}
	if calls != 7 || len(flagged) != 6 {
		t.Fatalf("expected 7 log calls and 6 flagged lines (all but the compliant one), got %d calls, flagged %v", calls, flagged)
	}
}
