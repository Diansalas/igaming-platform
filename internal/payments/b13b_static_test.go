package payments

// B13-B static guards (ADR 0111 section 18). Lexical (go/ast), no database. Each guard has a negative
// control proving it is not vacuous.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

func readSrc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// funcBody returns the source text of the named top-level function or method.
func funcBody(t *testing.T, file, name string) string {
	t.Helper()
	src := readSrc(t, file)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name {
			return src[fset.Position(fd.Pos()).Offset:fset.Position(fd.End()).Offset]
		}
	}
	t.Fatalf("%s not found in %s", name, file)
	return ""
}

// T1p order (ADR 0111 2.4): lock -> H-SEC-11 -> KYC gate -> route error -> DESTINATION GATE -> allow decision row
// -> approved->submitted -> attempt insert -> SNAPSHOT insert -> audit.
func TestB13B_Static_ClaimForDispatchOrder(t *testing.T) {
	body := funcBody(t, "payout.go", "ClaimForDispatch")
	order := []string{
		"withdrawal.LockApprovedForSubmission(", "tenant.RequireActiveForPaymentInitiation(", "evaluatePayoutGate(",
		"return fmt.Errorf(\"payments: route payout: %w\", routeErr)", "gatePayoutDestination(",
	}
	last := -1
	for _, s := range order {
		i := strings.Index(body, s)
		if i < 0 {
			t.Fatalf("ClaimForDispatch no longer contains %q", s)
		}
		if i < last {
			t.Fatalf("ClaimForDispatch order broken at %q", s)
		}
		last = i
	}
	gate := strings.Index(body, "gatePayoutDestination(")
	mark := strings.Index(body, "withdrawal.MarkSubmittedPending(")
	allowDecision := strings.LastIndex(body[:mark], "kyc.RecordDecision(")
	insert := strings.Index(body, "InsertSubmittingAttempt(")
	snap := strings.Index(body, "WriteSnapshot(")
	if gate >= allowDecision || allowDecision >= mark || mark >= insert || insert >= snap {
		t.Fatalf("T1p order must be gate(%d) < allow decision(%d) < submitted(%d) < attempt insert(%d) < snapshot(%d)", gate, allowDecision, mark, insert, snap)
	}
	if !strings.Contains(body, "payoutinstrument.IsGateRefusal(") || !strings.Contains(body, "destRefusal = gerr") {
		t.Fatal("a destination refusal must be committed as the denial audit only and returned after the commit")
	}
}

// T2 and T12: the destination gate runs before the KYC gate and before the claim / resend CAS.
func TestB13B_Static_SweeperRechecksTheDestination(t *testing.T) {
	for fn, claim := range map[string]string{"reclaimPayoutCreated": "ClaimCreatedForSubmission(", "resubmitPayoutAmbiguous": "ResubmitAmbiguous("} {
		body := funcBody(t, "payout_sweep.go", fn)
		d := strings.Index(body, "s.destinationGateAndEscalate(")
		k := strings.Index(body, "s.gateAndEscalateOnDeny(")
		c := strings.Index(body, claim)
		if d < 0 || k < 0 || c < 0 || d >= k || k >= c {
			t.Fatalf("%s: destination gate(%d) must precede the KYC gate(%d) and the claim CAS(%d)", fn, d, k, c)
		}
		ks := strings.Index(body, "checkPayoutKillSwitch(")
		if ks < 0 || ks >= d {
			t.Fatalf("%s: the kill-switch check must stay ahead of the destination gate", fn)
		}
	}
}

func goFilesUnder(t *testing.T, roots ...string) []string {
	t.Helper()
	var out []string
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) && p != root {
				return filepath.SkipDir
			}
			if !d.IsDir() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
				out = append(out, p)
			}
			return nil
		})
	}
	return out
}

type callSite struct {
	file     string
	spread   bool
	callName string
}

func callsNamed(t *testing.T, files []string, names ...string) []callSite {
	t.Helper()
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var out []callSite
	for _, f := range files {
		fset := token.NewFileSet()
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				name := calleeName(c)
				if want[name] {
					out = append(out, callSite{file: f, spread: c.Ellipsis.IsValid(), callName: name})
				}
			}
			return true
		})
	}
	return out
}

// Every production call of the free payout phase functions passes the destination collaborators as a spread
// (opts...). Omitting them would drop the phase-B gate / the evidence check for a bound withdrawal (the functions
// fail closed without them, but a Synthetic adapter on a legacy row would otherwise skip the gate silently).
func TestB13B_Static_ProductionCallSitesPassPayoutOptions(t *testing.T) {
	root := filepath.Join("..", "..")
	files := goFilesUnder(t, filepath.Join(root, "internal"), filepath.Join(root, "cmd"))
	sites := callsNamed(t, files, "DispatchWithdraw", "ApplyPayoutResult", "applyPayoutStatusEvidence")
	// DispatchWithdraw: handler + sweeper; ApplyPayoutResult: handler + sweeper; applyPayoutStatusEvidence: PollPayoutStatus.
	counts := map[string]int{}
	for _, s := range sites {
		if !s.spread {
			t.Errorf("%s: %s is called without the payout options spread (opts...)", s.file, s.callName)
		}
		counts[s.callName]++
	}
	if counts["DispatchWithdraw"] != 2 || counts["ApplyPayoutResult"] != 2 || counts["applyPayoutStatusEvidence"] != 1 {
		t.Fatalf("production call sites = %v, want DispatchWithdraw 2, ApplyPayoutResult 2, applyPayoutStatusEvidence 1 (a new caller must be reviewed)", counts)
	}
}

func TestB13B_Static_NegativeControl_Spread(t *testing.T) {
	src := `package x
func f() { DispatchWithdraw(a, b, c, d, e); ApplyPayoutResult(a, b, c, d, e, g, h, opts...) }`
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var bare, spread int
	ast.Inspect(af, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			switch calleeName(c) {
			case "DispatchWithdraw", "ApplyPayoutResult":
				if c.Ellipsis.IsValid() {
					spread++
				} else {
					bare++
				}
			}
		}
		return true
	})
	if bare != 1 || spread != 1 {
		t.Fatalf("the spread detector is vacuous: bare=%d spread=%d", bare, spread)
	}
}

// The destination check wraps all three evidence paths: sync phase C, the QueryStatus poll, the callback cell.
func TestB13B_Static_EveryEvidencePathChecksTheDestination(t *testing.T) {
	files := goFilesUnder(t, ".")
	sites := callsNamed(t, files, "payoutDestinationEvidence")
	got := map[string]int{}
	for _, s := range sites {
		got[filepath.Base(s.file)]++
	}
	if got["payout.go"] != 2 || got["receipt.go"] != 1 || len(sites) != 3 {
		t.Fatalf("payoutDestinationEvidence call sites = %v, want payout.go x2 (ApplyPayoutResult, applyPayoutStatusEvidenceInTx) and receipt.go x1", got)
	}
	// ... and ahead of any cell that can settle: in ApplyPayoutResult before the class switch; in the poll before the
	// state switch; in the receipt before the outcome switch.
	if b := funcBody(t, "payout.go", "ApplyPayoutResult"); strings.Index(b, "payoutDestinationEvidence(") > strings.Index(b, "switch gr.Class {") {
		t.Fatal("ApplyPayoutResult: the destination check must precede the class switch")
	}
	if b := funcBody(t, "payout.go", "applyPayoutStatusEvidenceInTx"); strings.Index(b, "payoutDestinationEvidence(") > strings.Index(b, "switch attempt.State {") {
		t.Fatal("the poll: the destination check must precede the state switch")
	}
	if b := funcBody(t, "receipt.go", "applyResolvedReceiptEvidence"); strings.Index(b, "payoutDestinationEvidence(") > strings.Index(b, "switch ev.Outcome {") {
		t.Fatal("the callback cell: the destination check must precede the outcome switch")
	}
}

// Decision 3: callbacks / polls / phase C never write a destination. The only snapshot writer call is T1p's.
func TestB13B_Static_OnlyT1pWritesTheSnapshot(t *testing.T) {
	root := filepath.Join("..", "..")
	files := goFilesUnder(t, filepath.Join(root, "internal"), filepath.Join(root, "cmd"))
	for _, s := range callsNamed(t, files, "WriteSnapshot") {
		base := filepath.ToSlash(s.file)
		if !strings.HasSuffix(base, "internal/payments/payout.go") && !strings.Contains(base, "internal/payoutinstrument/") {
			t.Errorf("%s calls WriteSnapshot: only ClaimForDispatch (T1p) may write a destination snapshot", s.file)
		}
	}
	if n := len(callsNamed(t, []string{"payout.go"}, "WriteSnapshot")); n != 1 {
		t.Fatalf("payout.go WriteSnapshot calls = %d, want exactly 1 (ClaimForDispatch)", n)
	}
	if b := funcBody(t, "payout.go", "ClaimForDispatch"); !strings.Contains(b, "WriteSnapshot(") {
		t.Fatal("WriteSnapshot must be called from ClaimForDispatch")
	}
	// No evidence-side function may reference the instrument write API at all.
	for _, fn := range []string{"ApplyPayoutResult", "applyPayoutStatusEvidenceInTx", "payoutDestinationEvidence"} {
		file := "payout.go"
		if fn == "payoutDestinationEvidence" {
			file = "payout_destination.go"
		}
		b := funcBody(t, file, fn)
		for _, forbidden := range []string{"WriteSnapshot(", "Register(", "Suspend(", "Revoke(", "ApplyProviderBlock(", "INSERT INTO payout_", "UPDATE payout_"} {
			if strings.Contains(b, forbidden) {
				t.Errorf("%s must not contain %q (callbacks, polls and phase C can never determine, replace or change a destination)", fn, forbidden)
			}
		}
	}
}

// Closed reason sets and the alert mapping for the new reasons.
func TestB13B_ClosedReasons(t *testing.T) {
	disp := PayoutDisputeReasons()
	for _, r := range []string{TerminalReasonDestinationMismatch, TerminalReasonDestinationIntegrityFailure} {
		m2, ok := disp[r]
		if !ok || m2 {
			t.Errorf("%s must be a payout dispute reason that is NOT M2-admitted (hold kept), got present=%v m2=%v", r, ok, m2)
		}
		if got := payoutAlertReasonFor(r); got != r {
			t.Errorf("payoutAlertReasonFor(%q) = %q", r, got)
		}
	}
	for _, r := range []string{alertReasonPayoutDestinationNotUsable, alertReasonPayoutDestinationMismatchOnTerminal} {
		if got := payoutAlertReasonFor(r); got != r {
			t.Errorf("payoutAlertReasonFor(%q) = %q, want it kept as a closed reason", r, got)
		}
	}
	if got := payoutAlertReasonFor("destination_something_else"); got != alertReasonUnclassified {
		t.Errorf("an unknown destination reason must not leak into a discriminator, got %q", got)
	}
}

// Without the collaborators nothing is configured: fail closed.
func TestB13B_OptionsFailClosedByDefault(t *testing.T) {
	e := buildPayoutEnv(nil)
	if e.destinations != nil || e.providers != nil || e.echoDeclared(nil) {
		t.Fatal("no options must mean no destination service")
	}
	var o *Orchestrator
	if opts := o.PayoutOptions(); opts != nil {
		t.Fatal("a nil orchestrator yields no options")
	}
	id := uuid.New()
	fp := "00"
	bound := withdrawal.WithdrawalRequest{PayoutInstrumentID: &id, PayoutInstrumentFingerprint: &fp}
	_, err := gatePayoutDestination(context.Background(), nil, nil, bound, NewMockProvider("m"), true, false)
	if g, ok := payoutinstrument.IsGateRefusal(err); !ok || g.Reason != payoutinstrument.ReasonNoGate || g.Integrity() {
		t.Fatalf("a bound withdrawal with no service must be refused (gate unavailable), got %v", err)
	}
	// Legacy (unbound): the tiering predicate with Bound=false - Synthetic passes, anything else is refused.
	if _, err := gatePayoutDestination(context.Background(), nil, nil, withdrawal.WithdrawalRequest{}, NewMockProvider("m"), true, false); err != nil {
		t.Fatalf("legacy + Synthetic adapter must pass: %v", err)
	}
	// Security L-1: a BOUND withdrawal with a nil adapter is a tier refusal at a dispatch gate (EvaluateGate would read nil as
	// "request creation" and skip the tiering predicate).
	if _, err := gatePayoutDestination(context.Background(), nil, nil, bound, nil, true, false); func() bool {
		g, ok := payoutinstrument.IsGateRefusal(err)
		return !ok || g.Reason != payoutinstrument.ReasonTierRefused
	}() {
		t.Fatalf("bound + nil adapter must be tier_refused, got %v", err)
	}
	for name, adapter := range map[string]any{"unmarked adapter": unmarkedPaymentsAdapter{NewMockProvider("m")}, "nil adapter": nil} {
		_, err := gatePayoutDestination(context.Background(), nil, nil, withdrawal.WithdrawalRequest{}, adapter, true, false)
		if g, ok := payoutinstrument.IsGateRefusal(err); !ok || g.Reason != payoutinstrument.ReasonTierRefused {
			t.Fatalf("legacy + %s must be refused by the tiering predicate, got %v", name, err)
		}
	}
}
