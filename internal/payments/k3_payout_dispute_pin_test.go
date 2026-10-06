package payments

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ADR 0101 C-5b (hardened, LF D-9): every payout dispute write site in the
// payout-reachable files uses a reason that PayoutDisputeReasons classifies as
// M2-admitted or M2-refused. A new payout reason fails this pin until ADR 0101
// classifies it.
//
// It parses receipt.go, payout.go, payout_sweep.go, attempt.go and
// orchestrator.go (payout_sweep.go may hold no site; it is parsed if present)
// and watches the five dispute writers. A reason written in the shared
// applyResolvedReceiptEvidence counts as payout-reachable. A variable reason is
// RESOLVED to the literals assigned to it in the same function (never
// whitelisted); "invalid_provider_reference:"+x is expanded to the closed
// "invalid_provider_reference" family. A parameter that only forwards its
// caller's reason (applyPayoutLateEvidence) is covered by its own call sites.
func TestK3_C5b_PayoutDisputeWriteSitesAreClassified(t *testing.T) {
	files := []string{"receipt.go", "payout.go", "payout_refbind.go", "payout_sweep.go", "attempt.go", "orchestrator.go"}
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	for _, f := range files {
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			if f == "payout_sweep.go" {
				continue // optional
			}
			t.Fatalf("parse %s: %v", f, err)
		}
		parsed[f] = af
	}
	if len(parsed) < 4 {
		t.Fatalf("parsed only %d files", len(parsed))
	}

	// Constants of the package (all non-test files).
	consts := map[string]string{}
	pkgFiles := []string{"attempt.go", "drive.go", "deposit_terminal_reasons.go", "receipt.go", "payout.go", "payout_refbind.go", "orchestrator.go"}
	for pass := 0; pass < 4; pass++ {
		for _, f := range pkgFiles {
			af, perr := parser.ParseFile(fset, f, nil, 0)
			if perr != nil {
				continue
			}
			for _, d := range af.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					continue
				}
				for _, sp := range gd.Specs {
					vs := sp.(*ast.ValueSpec)
					for i, n := range vs.Names {
						if i < len(vs.Values) {
							if v, ok := evalStringExpr(vs.Values[i], consts); ok {
								consts[n.Name] = v
							}
						}
					}
				}
			}
		}
	}

	// writer -> index of the reason argument; -1: the writer fixes its own reason.
	watch := map[string]int{
		"ApplyDisputeFromNonTerminal":    4,
		"ApplyDisputeFromNeverSent":      4,
		"ApplyDisputeFromDeclinedPayout": 4,
		"ApplyTombstonePrecedesSuccess":  -1,
		"applyPayoutLateEvidence":        4,
	}
	// Deposit-only reasons written in orchestrator.go (never a payout reason).
	depositOnly := map[string]bool{"multiple_success_for_intent": true}
	table := PayoutDisputeReasons()

	family := func(r string) string {
		if i := strings.Index(r, ":"); i >= 0 {
			return r[:i]
		}
		return r
	}

	sites := 0
	seenWriters := map[string]bool{}
	var problems []string
	for fname, af := range parsed {
		for _, d := range af.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			// Assignments to each local identifier, with their positions: a variable
			// reason is resolved ONLY from the assignments inside the innermost block
			// that contains the call and before it (a function may reuse the name).
			type asg struct {
				pos token.Pos
				rhs ast.Expr
			}
			assigned := map[string][]asg{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for i, l := range as.Lhs {
					if id, ok := l.(*ast.Ident); ok && i < len(as.Rhs) {
						assigned[id.Name] = append(assigned[id.Name], asg{as.Pos(), as.Rhs[i]})
					}
				}
				return true
			})
			params := map[string]bool{}
			for _, p := range fd.Type.Params.List {
				for _, n := range p.Names {
					params[n.Name] = true
				}
			}
			var blocks []*ast.BlockStmt
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if bs, ok := n.(*ast.BlockStmt); ok {
					blocks = append(blocks, bs)
				}
				return true
			})
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var name string
				switch fn := call.Fun.(type) {
				case *ast.Ident:
					name = fn.Name
				case *ast.SelectorExpr:
					name = fn.Sel.Name
				}
				idx, watched := watch[name]
				if !watched {
					return true
				}
				pos := fset.Position(call.Pos()).String()
				seenWriters[name] = true
				if idx < 0 {
					// The writer fixes the reason (reversal_tombstone_precedes_success).
					sites++
					if _, ok := table[TerminalReasonTombstonePrecedesSuccess]; !ok {
						problems = append(problems, pos+": tombstone reason unclassified")
					}
					return true
				}
				if idx >= len(call.Args) {
					return true
				}
				arg := call.Args[idx]
				var reasons []string
				if v, ok := evalStringExpr(arg, consts); ok {
					reasons = []string{v}
				} else if id, ok := arg.(*ast.Ident); ok {
					if params[id.Name] && len(assigned[id.Name]) == 0 {
						return true // forwarder: covered by the callers of fd
					}
					var inner *ast.BlockStmt
					for _, b := range blocks {
						if b.Pos() <= call.Pos() && call.End() <= b.End() && (inner == nil || b.Pos() > inner.Pos()) {
							inner = b
						}
					}
					var rhss []ast.Expr
					for _, a := range assigned[id.Name] {
						if a.pos >= inner.Pos() && a.pos < call.Pos() {
							rhss = append(rhss, a.rhs)
						}
					}
					if len(rhss) == 0 {
						problems = append(problems, pos+": variable reason "+id.Name+" in "+fd.Name.Name+" has no assignment in its block")
					}
					for _, rhs := range rhss {
						if v, ok := evalStringExpr(rhs, consts); ok {
							reasons = append(reasons, v)
							continue
						}
						// "<literal>:" + x
						if be, ok := rhs.(*ast.BinaryExpr); ok && be.Op == token.ADD {
							if l, ok := evalStringExpr(be.X, consts); ok && strings.HasSuffix(l, ":") {
								reasons = append(reasons, l+"x")
								continue
							}
						}
						// A value read from the provider/DB is not a dispute reason
						// this pin may whitelist: fail loudly.
						problems = append(problems, pos+": variable reason "+id.Name+" in "+fd.Name.Name+" has an unresolvable assignment")
					}
				} else {
					problems = append(problems, fname+" "+pos+": unresolvable reason argument in "+fd.Name.Name)
					return true
				}
				for _, r := range reasons {
					sites++
					if fname == "orchestrator.go" && depositOnly[r] {
						continue
					}
					if _, ok := table[family(r)]; !ok {
						problems = append(problems, pos+": payout dispute reason "+strconv.Quote(r)+" is not classified in PayoutDisputeReasons")
					}
				}
				return true
			})
		}
	}
	for w := range watch {
		if !seenWriters[w] {
			t.Errorf("the pin never saw a call of %s: the scan is not seeing the code", w)
		}
	}
	if sites < 15 {
		t.Fatalf("only %d dispute write sites resolved; the scan is not seeing the code", sites)
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}

	// The table lists every reason once and the admitted set equals the
	// allow-list function.
	for r, admitted := range table {
		for _, state := range []AttemptState{AttemptDisputed} {
			rr := r
			if got := M2ResolvableDispute(state, &rr); got != admitted {
				t.Errorf("PayoutDisputeReasons[%q]=%v but M2ResolvableDispute=%v", r, admitted, got)
			}
		}
	}
	unknown := "some_future_payout_reason"
	if M2ResolvableDispute(AttemptDisputed, &unknown) || M2ResolvableDispute(AttemptDisputed, nil) {
		t.Error("an unknown or NULL payout reason must be refused")
	}
	if !M2ResolvableDispute(AttemptAmbiguous, nil) {
		t.Error("an ambiguous payout is admitted")
	}
}
