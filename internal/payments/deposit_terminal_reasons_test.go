package payments

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// payoutOnlyDisputeReasons are written by payout code that shares a function with
// the deposit matrix (receipt.go's applyResolvedReceiptEvidence); they are never a
// deposit dispute reason.
var payoutOnlyDisputeReasons = map[string]bool{
	"provider_reference_mismatch":   true,
	"success_after_payout_declined": true,
	"amount_asset_mismatch":         true,
}

// functions that build the reason argument from the TerminalReason* constants at
// runtime (a variable, not a literal), so the static check cannot resolve it.
var reasonBuildingFuncs = map[string]bool{
	"applyDepositCallResult":   true, // invalid_provider_reference:<reason>
	"parkDepositAttempt":       true, // forwards its caller's reason
	"checkPollSuccessEvidence": true, // contradict(reason, ...) closure
}

func evalStringExpr(e ast.Expr, consts map[string]string) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			s, err := strconv.Unquote(x.Value)
			return s, err == nil
		}
	case *ast.Ident:
		s, ok := consts[x.Name]
		return s, ok
	case *ast.ParenExpr:
		return evalStringExpr(x.X, consts)
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			l, ok1 := evalStringExpr(x.X, consts)
			r, ok2 := evalStringExpr(x.Y, consts)
			return l + r, ok1 && ok2
		}
	}
	return "", false
}

// TestDepositDisputeTerminalReasons_CoverEveryWriteSite parses the package's
// non-test, non-payout sources and checks the reason argument of every dispute
// write (ApplyDisputeFromNonTerminal, ApplyDisputeFromNeverSent,
// parkDepositAttempt): a literal or constant must be in
// DepositDisputeTerminalReasons; a non-constant argument is accepted only in the
// functions known to build it from those constants. It also requires every
// TerminalReason* constant to be listed.
func TestDepositDisputeTerminalReasons_CoverEveryWriteSite(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		n := fi.Name()
		return !strings.HasSuffix(n, "_test.go") && n != "payout.go" && n != "payout_sweep.go"
	}, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pkg, ok := pkgs["payments"]
	if !ok {
		t.Fatalf("package payments not found")
	}

	consts := map[string]string{}
	for pass := 0; pass < 4; pass++ {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
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

	argIndex := map[string]int{"ApplyDisputeFromNonTerminal": 4, "ApplyDisputeFromNeverSent": 4, "parkDepositAttempt": 6}
	sites := 0
	var problems []string
	for fname, f := range pkg.Files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
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
				idx, watched := argIndex[name]
				if !watched || idx >= len(call.Args) {
					return true
				}
				pos := fset.Position(call.Pos())
				reason, resolved := evalStringExpr(call.Args[idx], consts)
				switch {
				case resolved && payoutOnlyDisputeReasons[reason]:
					return true
				case resolved:
					sites++
					if !IsDepositDisputeTerminalReason(reason) {
						problems = append(problems, pos.String()+": reason "+strconv.Quote(reason)+" is not in DepositDisputeTerminalReasons")
					}
				case reasonBuildingFuncs[fd.Name.Name]:
					sites++
				default:
					problems = append(problems, fname+" "+pos.String()+": non-constant reason argument in "+fd.Name.Name+" (add it to reasonBuildingFuncs only if it is built from the TerminalReason* constants)")
				}
				return true
			})
		}
	}
	if sites < 8 {
		t.Fatalf("only %d dispute write sites found; the scan is not seeing the code", sites)
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}

	for name, v := range consts {
		if strings.HasPrefix(name, "TerminalReason") {
			if !IsDepositDisputeTerminalReason(v) && !IsDepositDisputeTerminalReason(v+":x") {
				t.Errorf("constant %s = %q is not in DepositDisputeTerminalReasons", name, v)
			}
		}
	}
}

func TestDepositDisputeTerminalReasons_ExactStringsAreAContract(t *testing.T) {
	for _, want := range []string{
		"multiple_success_for_intent", "reversal_tombstone_precedes_success", "sync_amount_mismatch",
		"provider_reference_conflict", "invalid_provider_reference:", "poll_amount_mismatch", "poll_reference_mismatch",
	} {
		found := false
		for _, r := range DepositDisputeTerminalReasons() {
			if r == want {
				found = true
			}
		}
		if !found {
			t.Errorf("DepositDisputeTerminalReasons lacks %q", want)
		}
	}
	if !IsDepositDisputeTerminalReason("invalid_provider_reference:too_long") || IsDepositDisputeTerminalReason("poll_amount_mismatch_x") {
		t.Errorf("prefix/exact matching is wrong")
	}
}
