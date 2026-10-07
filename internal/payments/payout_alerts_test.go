package payments

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/alerting"
)

// B12 (PAY-PAYOUT-DISPUTE-ALERT-1): the closed-set mapping. Every payout dispute reason (the keys
// of PayoutDisputeReasons(), which include the B10 provider_reference_conflict park) and every
// payout escalation reason maps to itself; the invalid_provider_reference:<detail> family
// collapses to its prefix; anything else, including a raw provider string, a deposit-only reason
// and the empty string, becomes "unclassified" and is never echoed.
func TestPayoutAlertReasonFor_ClosedSet(t *testing.T) {
	for reason := range PayoutDisputeReasons() {
		if got := payoutAlertReasonFor(reason); got != reason {
			t.Errorf("payout dispute reason %q maps to %q, want itself", reason, got)
		}
	}
	for reason := range payoutEscalationReasons {
		if got := payoutAlertReasonFor(reason); got != reason {
			t.Errorf("payout escalation reason %q maps to %q, want itself", reason, got)
		}
	}
	if len(payoutEscalationReasons) != 3 {
		t.Fatalf("the payout escalation set changed (%d): review the sweeper call sites", len(payoutEscalationReasons))
	}
	for _, in := range []string{
		"invalid_provider_reference:too_long", "invalid_provider_reference:control_char", "invalid_provider_reference:",
	} {
		if got := payoutAlertReasonFor(in); got != TerminalReasonInvalidProviderReference {
			t.Errorf("%q maps to %q, want the family prefix", in, got)
		}
	}
	for _, in := range []string{
		"", "unclassified_x", "PROVIDER SAID: account 123 closed", "provider_declined",
		TerminalReasonSyncAmountMismatch, TerminalReasonMultipleSuccessForIntent, // deposit-only reasons
		alertReasonDepositSettlementWindowExceeded,
		"invalid_provider_referenceX", "late_success_after_terminal ", "success_after_payout_declined;DROP",
	} {
		if got := payoutAlertReasonFor(in); got != alertReasonUnclassified {
			t.Errorf("%q maps to %q, want %q", in, got, alertReasonUnclassified)
		}
	}
}

// The alert carries only the server-side ids and the closed reason: the discriminator shape, the
// platform-owned Kind (no new Kind, ADR 0102 17.1), the subject tenant, and provider_id as the
// only attribute (and none when the attempt has no provider yet).
func TestPayoutDisputeAlert_Shape(t *testing.T) {
	pid := "mock-b12"
	a := PaymentAttempt{ID: uuid.New(), TenantID: uuid.New(), Operation: AttemptOperationPayout, ProviderID: &pid}
	got := payoutDisputeAlert(a, "success_after_payout_declined")
	if got.Kind != alerting.KindPaymentWebhookIntegrity || got.SubjectTenantID != a.TenantID {
		t.Fatalf("kind/subject: %+v", got)
	}
	if want := "payout_attempt:" + a.ID.String() + ":reason:success_after_payout_declined"; got.Discriminator != want {
		t.Fatalf("discriminator %q, want %q", got.Discriminator, want)
	}
	if len(got.Attributes) != 1 || got.Attributes["provider_id"] != pid {
		t.Fatalf("attributes %+v, want only provider_id", got.Attributes)
	}
	a.ProviderID = nil
	if got := payoutDisputeAlert(a, "x"); len(got.Attributes) != 0 {
		t.Fatalf("an attempt with no provider carries no attributes, got %+v", got.Attributes)
	}
}

// A deposit attempt is a no-op for the payout helper (deposits have their own helpers); the
// nil transaction proves no statement is attempted.
func TestRaisePayoutDisputeAlert_DepositIsNoOp(t *testing.T) {
	a := PaymentAttempt{ID: uuid.New(), TenantID: uuid.New(), Operation: AttemptOperationDeposit}
	if err := raisePayoutDisputeAlert(t.Context(), nil, a, "success_after_payout_declined"); err != nil {
		t.Fatalf("deposit must be a no-op, got %v", err)
	}
	if err := payoutAlertAfterDispute(t.Context(), nil, a, "x", nil); err != nil {
		t.Fatalf("deposit must be a no-op, got %v", err)
	}
}

// payoutAlertAfterDispute never raises when the dispute transition itself failed, and returns
// that error untouched.
func TestPayoutAlertAfterDispute_PropagatesDisputeError(t *testing.T) {
	a := PaymentAttempt{ID: uuid.New(), TenantID: uuid.New(), Operation: AttemptOperationPayout}
	if err := payoutAlertAfterDispute(t.Context(), nil, a, "x", ErrAttemptStateConflict); err != ErrAttemptStateConflict {
		t.Fatalf("want the dispute error untouched, got %v", err)
	}
}

// ADR 0102 7.7 source-order guard (the B6 pattern): the alert tables are the terminal lock level,
// so every payout dispute raise must be the LAST statement of its site, i.e. the call is itself
// a result expression of a `return` statement (nothing in the function runs after it; at most the
// transaction owner's audit/commit tail does). Nested use is allowed only for the chained receipt
// cells, where payoutAlertAfterDispute is the return value and alertAfterDispute sits inside it.
func TestPayoutDisputeRaiseIsReturnedNeverFollowedByWork_ADR0102_7_7(t *testing.T) {
	files := []string{"payout.go", "payout_refbind.go", "payout_sweep.go", "receipt.go"}
	sites := 0
	var problems []string
	for _, f := range files {
		problems = append(problems, b12CheckReturned(t, f, nil, &sites)...)
	}
	if len(problems) != 0 {
		sort.Strings(problems)
		t.Fatalf("payout dispute raise placement violations:\n  %s", strings.Join(problems, "\n  "))
	}
	// 1 sync invalid ref, 1 sync ref mismatch, 1 late evidence, 1 status invalid ref, 2 status
	// mismatches, 1 refbind park, 1 sweeper escalation, 1+4 receipt cells = 13.
	if sites != 13 {
		t.Fatalf("expected 13 payout dispute raise call sites in the reviewed files, found %d", sites)
	}
}

func TestPayoutDisputeRaiseOrderGuard_NegativeControl(t *testing.T) {
	bad := `package payments
func a(ctx, tx any) error {
	raisePayoutDisputeAlert(ctx, tx, at, "r")
	return nil
}
func b(ctx, tx any) error {
	if err := raisePayoutDisputeAlert(ctx, tx, at, "r"); err != nil {
		return err
	}
	return audit(ctx)
}
func c(ctx, tx any) error {
	return raisePayoutDisputeAlert(ctx, tx, at, "r")
}
func d(ctx, tx any) (bool, error) {
	return true, payoutAlertAfterDispute(ctx, tx, at, "r", alertAfterDispute(ctx, tx, at, "r", nil))
}`
	sites := 0
	problems := b12CheckReturned(t, "fixture.go", []byte(bad), &sites)
	if sites != 4 || len(problems) != 2 {
		t.Fatalf("guard must flag exactly the two non-returned raises (a, b) of 4 sites, got sites=%d problems=%v", sites, problems)
	}
}

func b12CheckReturned(t *testing.T, file string, src []byte, sites *int) []string {
	t.Helper()
	fset := token.NewFileSet()
	var af *ast.File
	var err error
	if src != nil {
		af, err = parser.ParseFile(fset, file, src, 0)
	} else {
		af, err = parser.ParseFile(fset, file, nil, 0)
	}
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var problems []string
	for _, d := range af.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		returned := map[*ast.CallExpr]bool{}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if rs, ok := n.(*ast.ReturnStmt); ok {
				for _, r := range rs.Results {
					if c, ok := r.(*ast.CallExpr); ok {
						returned[c] = true
					}
				}
			}
			return true
		})
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := c.Fun.(*ast.Ident)
			if !ok || (id.Name != "raisePayoutDisputeAlert" && id.Name != "payoutAlertAfterDispute") {
				return true
			}
			*sites++
			if !returned[c] {
				problems = append(problems, file+":"+fd.Name.Name+": "+id.Name+" at line "+strconv.Itoa(fset.Position(c.Pos()).Line)+" is not a returned result (work could follow it)")
			}
			return true
		})
	}
	return problems
}
