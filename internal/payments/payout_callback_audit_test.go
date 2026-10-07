package payments

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/google/uuid"
)

// The audit action is a stable, queryable name (dashboards and the HD-0095-1 review queue key on
// it): pin the literal, never only the constant.
func TestAuditPayoutCallbackDispute_ActionNameIsStable(t *testing.T) {
	if auditActionPayoutCallbackDispute != "payments.payout_callback_dispute" {
		t.Fatalf("audit action changed to %q: update ADR 0095 section 42.7 and every consumer", auditActionPayoutCallbackDispute)
	}
}

// PAY-PAYOUT-SUCCEEDED-REF-MISMATCH-1: the same pin for the foreign-reference audit action.
func TestAuditPayoutSucceededForeignRef_ActionNameIsStable(t *testing.T) {
	if auditActionPayoutSucceededForeignRef != "payments.payout_succeeded_foreign_reference" {
		t.Fatalf("audit action changed to %q: update ADR 0095 section 42.8 (M-1) and every consumer", auditActionPayoutSucceededForeignRef)
	}
}

// PAY-PAYOUT-CALLBACK-AUDIT-1: a deposit attempt writes no payout audit row, and a failed dispute
// CAS is returned untouched without recording anything. The nil transaction proves no statement is
// attempted in either case.
func TestAuditPayoutCallbackDispute_DepositNoOp_And_CASErrorPassthrough(t *testing.T) {
	dep := PaymentAttempt{ID: uuid.New(), TenantID: uuid.New(), Operation: AttemptOperationDeposit}
	if err := auditPayoutCallbackDispute(t.Context(), nil, dep, ReceiptEvidence{}, "x", nil); err != nil {
		t.Fatalf("a deposit attempt must be a no-op, got %v", err)
	}
	pay := PaymentAttempt{ID: uuid.New(), TenantID: uuid.New(), Operation: AttemptOperationPayout}
	if err := auditPayoutCallbackDispute(t.Context(), nil, pay, ReceiptEvidence{}, "x", ErrAttemptStateConflict); err != ErrAttemptStateConflict {
		t.Fatalf("a failed dispute CAS must be returned untouched, got %v", err)
	}
}

// Source guard (the B12 order pattern): in the four payout receipt cells the audit call sits INSIDE
// the payoutAlertAfterDispute call's arguments, so it is evaluated after the dispute CAS and before
// the raise (which is the returned, last statement).
func TestPayoutCallbackAudit_AuditPrecedesRaise_InEveryReceiptCell(t *testing.T) {
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, "receipt.go", nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cells := 0
	ast.Inspect(af, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := c.Fun.(*ast.Ident); !ok || id.Name != "payoutAlertAfterDispute" {
			return true
		}
		cells++
		last := c.Args[len(c.Args)-1] // the dispute result handed to the raise
		inner, ok := last.(*ast.CallExpr)
		if !ok {
			t.Errorf("line %d: the raise must be fed by the audit call", fset.Position(c.Pos()).Line)
			return true
		}
		if id, ok := inner.Fun.(*ast.Ident); !ok || id.Name != "auditPayoutCallbackDispute" {
			t.Errorf("line %d: payoutAlertAfterDispute is not fed by auditPayoutCallbackDispute", fset.Position(c.Pos()).Line)
			return true
		}
		// ...and the audit call itself is fed by the dispute CAS (never the other way round).
		dispute := inner.Args[len(inner.Args)-1]
		if _, ok := dispute.(*ast.CallExpr); !ok {
			t.Errorf("line %d: the audit call must wrap the dispute result", fset.Position(c.Pos()).Line)
		}
		return true
	})
	if cells != 4 {
		t.Fatalf("expected 4 payout receipt cells (T14, T15, amount/asset mismatch, tombstone), found %d", cells)
	}
}
