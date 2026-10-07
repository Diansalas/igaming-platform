package alerting

// ADR 0102 I-wire static wiring guards (pure go/ast, no build, like
// static_dispatcher_identity_test.go). They pin, across the whole module:
//
//  1. B-1 (LF I-core re-review): every alerting.RaiseGuarded call is either
//     lexically inside an alerting.InTx closure, or inside one of the named
//     helper functions below (which take the caller's tx and are only reached
//     from InTx owners - rule 2). A RaiseGuarded outside both can lose a
//     swallowed alert: there is no Pending to flush.
//  2. Every evidence-transaction OWNER that reaches those helpers opens its
//     transaction through alerting.InTx (so the post-commit Flush exists).
//  3. Every PaymentOrchestrator.ReceiveVerifiedCallback call (the T10/T13d
//     receipt path) is inside an InTx closure.
//  4. C-102-5 / SR-5: no alerting call at all is lexically inside a
//     WithTenantSnapshot (REPEATABLE READ) closure.
//  5. The kill-switch and reconciliation REPEATABLE READ raises are
//     post-commit only: payments_kill_switch_handlers.go never calls
//     RaiseGuarded.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// helpers that call RaiseGuarded with the caller's tx; reached only from InTx owners.
var staticRaiseGuardedHelpers = map[string]bool{
	"internal/payments/alerts.go:raiseDepositParkAlert":       true,
	"internal/payments/alerts.go:raisePollContradictionAlert": true,
	"internal/payments/alerts.go:raiseMultipleSuccessAlert":   true,
	"internal/payments/alerts.go:raiseDepositEscalationAlert": true,
	// B12 (PAY-PAYOUT-DISPUTE-ALERT-1): the payout dispute / T14 / T16 raise.
	"internal/payments/payout_alerts.go:raisePayoutDisputeAlert": true,
	"internal/reconciliation/alerts.go:raiseLedgerRunAlerts":     true,
	"internal/reconciliation/alerts.go:raiseInTxMismatch":        true,
	// PRH-2 E1 (ADR 0106 section 4.3): the KYC outbox terminal alert, raised in-tx
	// from the tenant phase-C session by runPhaseC / recordApplyConflict.
	"internal/kyc/outbox_worker.go:raiseSubmissionFailedTerminal": true,
}

// evidence/run transaction owners that MUST contain an alerting.InTx call.
var staticInTxOwners = []string{
	"internal/payments/drive.go:driveCreatedAttempt",
	"internal/payments/deposit_v2.go:InitiateDepositAttempt",
	"internal/payments/sweeper.go:processViaQueryStatus",
	// B7: the reference-less submitting deposit's escalation transaction.
	"internal/payments/sweeper.go:processUnreferenced",
	// B12: the payout evidence transactions (sync phase C, QueryStatus poll/resolve) and the
	// payout T16 escalation transaction. Payout callbacks run inside the webhook handler's InTx.
	"internal/payments/payout.go:ApplyPayoutResult",
	"internal/payments/payout.go:applyPayoutStatusEvidence",
	"internal/payments/payout_sweep.go:escalateAmbiguousPayout",
	// H-W1 moved the ledger_vs_projection evidence transaction (and its InTx/Flush) out of
	// sweepTenants into runLedgerStreamForTenant, shared by the ordinary and observation sweeps.
	"internal/reconciliation/scheduler.go:runLedgerStreamForTenant",
	"internal/reconciliation/scheduler.go:runSportsbookStreamForTenant",
	"internal/reconciliation/scheduler.go:runCasinoStreamForTenant",
	"internal/httpserver/deposit_handlers.go:newPaymentWebhookHandler",
	"internal/httpserver/payment_deposit_simulation_handlers.go:newSimulateDepositCallbackHandler",
	// PRH-2 E1 (ADR 0106 section 4.3): the KYC outbox phase-C owners.
	"internal/kyc/outbox_worker.go:runPhaseC",
	"internal/kyc/outbox_worker.go:recordApplyConflict",
}

type staticCall struct {
	file, fn, name string
	line           int
	inTx, inSnap   bool
	recv           string
	discarded      bool // the call's result is dropped (expression statement or assigned to _)
}

type staticVisitor struct {
	fset         *token.FileSet
	file, fn     string
	inTx, inSnap bool
	out          *[]staticCall
	discarded    map[*ast.CallExpr]bool
}

func staticCalleeName(c *ast.CallExpr) (name, recv string) {
	switch f := c.Fun.(type) {
	case *ast.SelectorExpr:
		if id, ok := f.X.(*ast.Ident); ok {
			return id.Name + "." + f.Sel.Name, id.Name
		}
		return "." + f.Sel.Name, staticExprText(f.X)
	case *ast.Ident:
		return f.Name, ""
	}
	return "", ""
}

func staticExprText(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return staticExprText(x.X) + "." + x.Sel.Name
	}
	return ""
}

func (v staticVisitor) Visit(n ast.Node) ast.Visitor {
	switch x := n.(type) {
	case *ast.FuncDecl:
		v.fn = x.Name.Name
		return v
	case *ast.ExprStmt:
		if c, ok := x.X.(*ast.CallExpr); ok {
			v.discarded[c] = true
		}
		return v
	case *ast.AssignStmt:
		allBlank := len(x.Lhs) > 0
		for _, l := range x.Lhs {
			if id, ok := l.(*ast.Ident); !ok || id.Name != "_" {
				allBlank = false
			}
		}
		if allBlank {
			for _, r := range x.Rhs {
				if c, ok := r.(*ast.CallExpr); ok {
					v.discarded[c] = true
				}
			}
		}
		return v
	case *ast.CallExpr:
		name, recv := staticCalleeName(x)
		*v.out = append(*v.out, staticCall{
			file: v.file, fn: v.fn, name: name, recv: recv, line: v.fset.Position(x.Pos()).Line,
			inTx: v.inTx, inSnap: v.inSnap, discarded: v.discarded[x],
		})
		isInTx := name == "alerting.InTx"
		isSnap := strings.HasSuffix(name, ".WithTenantSnapshot")
		ast.Walk(v, x.Fun)
		for _, a := range x.Args {
			lit, ok := a.(*ast.FuncLit)
			if ok && (isInTx || isSnap) {
				inner := v
				if isInTx {
					inner.inTx = true
				}
				if isSnap {
					inner.inSnap = true
				}
				ast.Walk(inner, lit.Body)
				continue
			}
			ast.Walk(v, a)
		}
		return nil
	}
	return v
}

// staticCollectFromSource runs the same visitor over one in-memory source file
// (negative-control fixtures: each guard must fire on a known-bad snippet).
func staticCollectFromSource(t *testing.T, rel, src string) []staticCall {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, 0)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", rel, err)
	}
	var out []staticCall
	ast.Walk(staticVisitor{fset: fset, file: rel, out: &out, discarded: map[*ast.CallExpr]bool{}}, f)
	return out
}

func staticCollectCalls(t *testing.T) []staticCall {
	t.Helper()
	root := staticRepoRoot(t)
	var out []staticCall
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.Walk(filepath.Join(root, top), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if strings.HasPrefix(rel, "internal/alerting/") || strings.HasPrefix(rel, "internal/testsupport/") {
				return nil
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				t.Fatalf("parse %s: %v", rel, perr)
			}
			ast.Walk(staticVisitor{fset: fset, file: rel, out: &out, discarded: map[*ast.CallExpr]bool{}}, f)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// staticB1Violations returns the RaiseGuarded calls that are neither inside an
// InTx closure nor in a sanctioned helper, and the sanctioned helpers seen.
func staticB1Violations(calls []staticCall) (violations []string, seen map[string]bool) {
	seen = map[string]bool{}
	for _, c := range calls {
		if c.name != "alerting.RaiseGuarded" {
			continue
		}
		key := c.file + ":" + c.fn
		if staticRaiseGuardedHelpers[key] {
			seen[key] = true
			continue
		}
		if !c.inTx {
			violations = append(violations, fmt.Sprintf("%s:%d (%s)", c.file, c.line, c.fn))
		}
	}
	return violations, seen
}

func TestStaticWiring_RaiseGuardedOnlyInsideInTxOrSanctionedHelpers_B1(t *testing.T) {
	violations, seenHelper := staticB1Violations(staticCollectCalls(t))
	for _, v := range violations {
		t.Errorf("B-1: %s calls alerting.RaiseGuarded outside an alerting.InTx closure and outside the sanctioned helpers", v)
	}
	for k := range staticRaiseGuardedHelpers {
		if !seenHelper[k] {
			t.Errorf("sanctioned helper %s no longer calls alerting.RaiseGuarded: remove it from the allowlist", k)
		}
	}
}

// staticDiscardedRaiseResults returns calls to a raise helper whose returned
// error is dropped. A propagated alert-statement error (25P02, deadlock, a
// cancelled context) must reach the transaction owner; dropping it would lose
// the alert silently (LF F4). The post-commit detached raises (RaiseDetached,
// RaisePostCommit) are deliberately excluded: their result is logged inside.
func staticDiscardedRaiseResults(calls []staticCall) []string {
	guarded := map[string]bool{
		"alerting.RaiseGuarded": true, "raiseDepositParkAlert": true, "raisePollContradictionAlert": true,
		"raiseMultipleSuccessAlert": true, "raiseLedgerRunAlerts": true, "raiseInTxMismatch": true, "alertAfterDispute": true,
		"raiseSubmissionFailedTerminal": true, "raiseDepositEscalationAlert": true,
		"raisePayoutDisputeAlert": true, "payoutAlertAfterDispute": true,
	}
	var out []string
	for _, c := range calls {
		if guarded[c.name] && c.discarded {
			out = append(out, fmt.Sprintf("%s:%d (%s) drops the result of %s", c.file, c.line, c.fn, c.name))
		}
	}
	return out
}

func TestStaticWiring_InTxRaiseResultsAreNeverDiscarded_LFF4(t *testing.T) {
	for _, v := range staticDiscardedRaiseResults(staticCollectCalls(t)) {
		t.Errorf("LF F4: %s", v)
	}
}

// Negative controls (code review CR-4): each guard must FIRE on a known-bad
// fixture, otherwise a guard that went vacuous (a renamed call, a changed AST
// shape) would pass silently.
func TestStaticWiring_NegativeControls(t *testing.T) {
	bad := `package p
func f(ctx, tx any) {
	_ = alerting.RaiseGuarded(ctx, tx, a)
	alerting.RaiseGuarded(ctx, tx, a)
	pool.WithTenantSnapshot(ctx, id, func(ctx, tx any) error { return alerting.RaiseGuarded(ctx, tx, a) })
}
func g(ctx, tx any) {
	alerting.InTx(ctx, r, func(ctx, tx any) error { return alerting.RaiseGuarded(ctx, tx, a) })
}`
	calls := staticCollectFromSource(t, "internal/x/bad.go", bad)
	if v, _ := staticB1Violations(calls); len(v) != 3 {
		t.Fatalf("B-1 guard must flag the three RaiseGuarded calls outside InTx (two bare, one in a snapshot closure; g's is inside InTx): got %v", v)
	}
	if d := staticDiscardedRaiseResults(calls); len(d) != 2 {
		t.Fatalf("discarded-result guard must flag exactly the two dropped calls, got %v", d)
	}
	var snap, inTx int
	for _, c := range calls {
		if c.inSnap && strings.HasPrefix(c.name, "alerting.") {
			snap++
		}
		if c.inTx && c.name == "alerting.RaiseGuarded" {
			inTx++
		}
	}
	if snap != 1 || inTx != 1 {
		t.Fatalf("snapshot/InTx detection: snap=%d inTx=%d, want 1/1", snap, inTx)
	}
}

// B6/B7: the deposit escalation raise is a sanctioned RaiseGuarded helper and its result must
// never be dropped (negative control: a dropped call is flagged), and the one real call site
// is the sweeper's single T16 helper.
func TestStaticWiring_DepositEscalationRaiseResultNeverDiscarded_B6B7(t *testing.T) {
	bad := `package p
func f(ctx, tx any) {
	raiseDepositEscalationAlert(ctx, tx, a, r)
	_ = raiseDepositEscalationAlert(ctx, tx, a, r)
}
func g(ctx, tx any) error { return raiseDepositEscalationAlert(ctx, tx, a, r) }`
	calls := staticCollectFromSource(t, "internal/x/bad.go", bad)
	if d := staticDiscardedRaiseResults(calls); len(d) != 2 {
		t.Fatalf("discarded-result guard must flag the two dropped escalation raises, got %v", d)
	}
	var real int
	for _, c := range staticCollectCalls(t) {
		if c.name == "raiseDepositEscalationAlert" && c.file == "internal/payments/sweeper.go" && c.fn == "escalateDepositIfDue" {
			real++
		}
	}
	if real != 1 {
		t.Fatalf("expected exactly one raiseDepositEscalationAlert call in sweeper.go:escalateDepositIfDue, saw %d", real)
	}
}

// B12 (PAY-PAYOUT-DISPUTE-ALERT-1): the payout dispute raise is a sanctioned RaiseGuarded helper,
// its result is never dropped (negative control), and its call sites are exactly the reviewed
// set: one pinned count per owner function. A new payout dispute write site that raises (or one
// that stops raising) changes this table and must be reviewed.
func TestStaticWiring_PayoutDisputeRaiseSitesArePinned_B12(t *testing.T) {
	bad := `package p
func f(ctx, tx any) {
	raisePayoutDisputeAlert(ctx, tx, a, r)
	_ = payoutAlertAfterDispute(ctx, tx, a, r, nil)
}
func g(ctx, tx any) error { return raisePayoutDisputeAlert(ctx, tx, a, r) }`
	if d := staticDiscardedRaiseResults(staticCollectFromSource(t, "internal/x/bad.go", bad)); len(d) != 2 {
		t.Fatalf("discarded-result guard must flag the two dropped payout raises, got %v", d)
	}
	want := map[string]int{
		"internal/payments/payout.go:ApplyPayoutResult:raisePayoutDisputeAlert":                   1, // invalid-reference park (sync)
		"internal/payments/payout.go:applyPayoutSuccess:raisePayoutDisputeAlert":                  1, // provider_reference_mismatch park
		"internal/payments/payout.go:applyPayoutLateEvidence:raisePayoutDisputeAlert":             1, // T14 and late T10
		"internal/payments/payout.go:applyPayoutStatusEvidenceInTx:raisePayoutDisputeAlert":       1, // invalid-reference park (poll)
		"internal/payments/payout.go:applyPayoutSuccessCheckedFromStatus:raisePayoutDisputeAlert": 2, // amount/asset + reference mismatch (poll)
		"internal/payments/payout_refbind.go:payoutGuardReferenceBinding:raisePayoutDisputeAlert": 1, // B10 provider_reference_conflict park
		"internal/payments/payout_sweep.go:escalateAmbiguousPayout:raisePayoutDisputeAlert":       1, // T16 payout escalation
		"internal/payments/receipt.go:applyResolvedReceiptEvidence:raisePayoutDisputeAlert":       4, // callback reference-mismatch park + R-5 (declined), R-6 (succeeded) mismatched success and M-1 foreign-reference success on succeeded (raise only)
		"internal/payments/receipt.go:applyResolvedReceiptEvidence:payoutAlertAfterDispute":       4, // T15, T14, callback amount mismatch, tombstone
		"internal/payments/payout_alerts.go:payoutAlertAfterDispute:raisePayoutDisputeAlert":      1,
	}
	got := map[string]int{}
	for _, c := range staticCollectCalls(t) {
		if c.name == "raisePayoutDisputeAlert" || c.name == "payoutAlertAfterDispute" {
			got[c.file+":"+c.fn+":"+c.name]++
		}
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("payout dispute raise site %s: want %d call(s), found %d", k, n, got[k])
		}
	}
	for k, n := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unreviewed payout dispute raise site %s (%d call(s)): review it against ADR 0102 7.7 and ADR 0095 section 42, then pin it", k, n)
		}
	}
}

func TestStaticWiring_EvidenceTransactionOwnersOpenThroughInTxAndFlush(t *testing.T) {
	hasInTx, hasFlush := map[string]bool{}, map[string]bool{}
	for _, c := range staticCollectCalls(t) {
		if c.name == "alerting.InTx" {
			hasInTx[c.file+":"+c.fn] = true
		}
		// The receiver must be a Pending (named pending / pendingAlerts): an
		// unrelated .Flush (a bufio or http.Flusher) must not satisfy the guard.
		if strings.HasSuffix(c.name, ".Flush") && strings.HasPrefix(c.recv, "pending") {
			hasFlush[c.file+":"+c.fn] = true
		}
	}
	for _, owner := range staticInTxOwners {
		if !hasInTx[owner] {
			t.Errorf("transaction owner %s must open its evidence transaction through alerting.InTx (no Pending, no post-commit retry otherwise)", owner)
		}
		if !hasFlush[owner] {
			t.Errorf("transaction owner %s must call Pending.Flush after the commit (mandatory post-commit detached retry, condition (b))", owner)
		}
	}
}

func TestStaticWiring_PaymentCallbackReceiptPathIsInsideInTx(t *testing.T) {
	var seen int
	for _, c := range staticCollectCalls(t) {
		if c.name == ".ReceiveVerifiedCallback" && strings.Contains(c.recv, "PaymentOrchestrator") {
			seen++
			if !c.inTx {
				t.Errorf("%s:%d: PaymentOrchestrator.ReceiveVerifiedCallback must run inside an alerting.InTx closure", c.file, c.line)
			}
		}
	}
	if seen < 2 {
		t.Fatalf("expected the webhook and simulation handlers to call PaymentOrchestrator.ReceiveVerifiedCallback, saw %d (guard is vacuous)", seen)
	}
}

func TestStaticWiring_NoAlertingCallInsideSnapshotTransaction_C1025(t *testing.T) {
	var snapClosuresSeen int
	for _, c := range staticCollectCalls(t) {
		if strings.HasSuffix(c.name, ".WithTenantSnapshot") && c.file == "internal/reconciliation/scheduler.go" {
			snapClosuresSeen++
		}
		if c.inSnap && strings.HasPrefix(c.name, "alerting.") {
			t.Errorf("C-102-5/SR-5: %s:%d (%s) calls %s inside a WithTenantSnapshot (REPEATABLE READ) closure", c.file, c.line, c.fn, c.name)
		}
	}
	// The scheduler's casino_statement and payment_statement match each open a
	// snapshot; a count below two means the rule is checking nothing.
	if snapClosuresSeen < 2 {
		t.Fatalf("expected at least 2 WithTenantSnapshot closures in scheduler.go, saw %d (the snapshot guard would be vacuous)", snapClosuresSeen)
	}
}

func TestStaticWiring_KillSwitchRaisesPostCommitOnly(t *testing.T) {
	var postCommit int
	for _, c := range staticCollectCalls(t) {
		if c.file != "internal/httpserver/payments_kill_switch_handlers.go" {
			continue
		}
		switch c.name {
		case "alerting.RaiseGuarded", "alerting.InTx":
			t.Errorf("%s:%d: the kill-switch engage alert must be post-commit only (LF F10), found %s", c.file, c.line, c.name)
		case "alerting.RaisePostCommit":
			postCommit++
		}
	}
	if postCommit != 1 {
		t.Fatalf("expected exactly one alerting.RaisePostCommit in the kill-switch handlers, got %d", postCommit)
	}
}

// staticEvidenceFuncs pins every non-test call site of the evidence-application
// functions to either an alerting.InTx closure (the transaction owners) or a
// named function that is itself only reached from those owners. A NEW caller of
// one of these functions through a plain WithTenant would otherwise reach
// RaiseGuarded with no Pending (the swallowed alert would be lost, logged as
// alert_raise_no_pending): it fails here and must be reviewed.
var staticEvidenceFuncs = map[string]bool{
	".applyDepositCallResult": true, "o.applyDepositCallResult": true,
	"ApplyReceiptEvidence": true, "ApplyDeferredReceiptsForAttempt": true,
	".applyStatusEvidence": true, "s.applyStatusEvidence": true,
	".postDepositSuccessOrDispute": true, "o.postDepositSuccessOrDispute": true,
	"s.Orchestrator.postDepositSuccessOrDispute": true,
	// B12: the payout evidence chain (each reaches raisePayoutDisputeAlert).
	"applyPayoutSuccess": true, "applyPayoutDecline": true, "applyPayoutLateEvidence": true,
	"payoutHandleContradiction": true, "payoutGuardReferenceBinding": true,
	"applyPayoutSuccessCheckedFromStatus": true, "applyPayoutStatusEvidenceInTx": true,
}

// Reviewed callers: each is only reachable from a transaction owner that opens
// through alerting.InTx (applyDepositCallResult from driveCreatedAttempt and
// InitiateDepositAttempt phase C; receiveCallbackViaReceiptPath from
// ReceiveVerifiedCallback inside the webhook/simulation InTx; the receipt
// helpers from ApplyReceiptEvidence; applyStatusEvidence from the sweeper's
// InTx).
var staticEvidenceCallerAllowlist = map[string]bool{
	"internal/payments/drive.go:applyDepositCallResult":               true,
	"internal/payments/orchestrator.go:receiveCallbackViaReceiptPath": true,
	"internal/payments/receipt.go:ApplyReceiptEvidence":               true,
	"internal/payments/receipt.go:applyDepositSuccessAndPost":         true,
	"internal/payments/sweeper.go:applyStatusEvidence":                true,
	"internal/payments/sweeper.go:applyPollPending":                   true,
	// B12: the payout chain. Entered only from ApplyPayoutResult / applyPayoutStatusEvidence
	// (alerting.InTx owners) or from the webhook handler's InTx via applyResolvedReceiptEvidence.
	"internal/payments/payout.go:applyPayoutSuccess":                  true,
	"internal/payments/payout.go:applyPayoutDecline":                  true,
	"internal/payments/payout.go:applyPayoutLateEvidence":             true,
	"internal/payments/payout.go:payoutHandleContradiction":           true,
	"internal/payments/payout.go:applyPayoutSuccessCheckedFromStatus": true,
	"internal/payments/payout.go:applyPayoutStatusEvidenceInTx":       true,
	"internal/payments/payout_refbind.go:payoutGuardReferenceBinding": true,
	"internal/payments/receipt.go:applyResolvedReceiptEvidence":       true,
}

func TestStaticWiring_EvidenceFunctionCallersAreInTxOrReviewed(t *testing.T) {
	for _, c := range staticCollectCalls(t) {
		if !staticEvidenceFuncs[c.name] || strings.HasPrefix(c.file, "internal/alerting/") {
			continue
		}
		if c.inTx || staticEvidenceCallerAllowlist[c.file+":"+c.fn] {
			continue
		}
		t.Errorf("reachability: %s:%d (%s) calls %s outside an alerting.InTx closure and outside the reviewed caller allowlist", c.file, c.line, c.fn, c.name)
	}
}
