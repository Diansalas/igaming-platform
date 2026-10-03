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
	"internal/reconciliation/alerts.go:raiseLedgerRunAlerts":  true,
	"internal/reconciliation/alerts.go:raiseInTxMismatch":     true,
}

// evidence/run transaction owners that MUST contain an alerting.InTx call.
var staticInTxOwners = []string{
	"internal/payments/drive.go:driveCreatedAttempt",
	"internal/payments/deposit_v2.go:InitiateDepositAttempt",
	"internal/payments/sweeper.go:processViaQueryStatus",
	"internal/reconciliation/scheduler.go:sweepTenants",
	"internal/reconciliation/scheduler.go:runSportsbookStreamForTenant",
	"internal/reconciliation/scheduler.go:runCasinoStreamForTenant",
	"internal/httpserver/deposit_handlers.go:newPaymentWebhookHandler",
	"internal/httpserver/payment_deposit_simulation_handlers.go:newSimulateDepositCallbackHandler",
}

type staticCall struct {
	file, fn, name string
	line           int
	inTx, inSnap   bool
	recv           string
}

type staticVisitor struct {
	fset         *token.FileSet
	file, fn     string
	inTx, inSnap bool
	out          *[]staticCall
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
	case *ast.CallExpr:
		name, recv := staticCalleeName(x)
		*v.out = append(*v.out, staticCall{
			file: v.file, fn: v.fn, name: name, recv: recv, line: v.fset.Position(x.Pos()).Line,
			inTx: v.inTx, inSnap: v.inSnap,
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
			ast.Walk(staticVisitor{fset: fset, file: rel, out: &out}, f)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func TestStaticWiring_RaiseGuardedOnlyInsideInTxOrSanctionedHelpers_B1(t *testing.T) {
	var seenHelper = map[string]bool{}
	for _, c := range staticCollectCalls(t) {
		if c.name != "alerting.RaiseGuarded" {
			continue
		}
		key := c.file + ":" + c.fn
		if staticRaiseGuardedHelpers[key] {
			seenHelper[key] = true
			continue
		}
		if !c.inTx {
			t.Errorf("B-1: %s:%d (%s) calls alerting.RaiseGuarded outside an alerting.InTx closure and outside the sanctioned helpers", c.file, c.line, c.fn)
		}
	}
	for k := range staticRaiseGuardedHelpers {
		if !seenHelper[k] {
			t.Errorf("sanctioned helper %s no longer calls alerting.RaiseGuarded: remove it from the allowlist", k)
		}
	}
}

func TestStaticWiring_EvidenceTransactionOwnersOpenThroughInTx(t *testing.T) {
	has := map[string]bool{}
	for _, c := range staticCollectCalls(t) {
		if c.name == "alerting.InTx" {
			has[c.file+":"+c.fn] = true
		}
	}
	for _, owner := range staticInTxOwners {
		if !has[owner] {
			t.Errorf("transaction owner %s must open its evidence transaction through alerting.InTx (no Pending, no post-commit retry otherwise)", owner)
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
	for _, c := range staticCollectCalls(t) {
		if c.inSnap && strings.HasPrefix(c.name, "alerting.") {
			t.Errorf("C-102-5/SR-5: %s:%d (%s) calls %s inside a WithTenantSnapshot (REPEATABLE READ) closure", c.file, c.line, c.fn, c.name)
		}
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
