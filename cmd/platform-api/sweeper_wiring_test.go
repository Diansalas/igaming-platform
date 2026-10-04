package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payments"
)

// PRH-2 H (code-reviewer H3): NewSweeper leaves PayoutKYCGate nil, and a nil gate
// makes the whole payout half of the sweeper a silent no-op. The production
// constructor must set it explicitly.
func TestBuildPaymentsSweeper_PayoutKYCGateNonNil(t *testing.T) {
	cfg := baseConfig(t, "development", true)
	cfg.TestSupportEndpointsEnabled = true // the MOCK outbound resolver is wired only with test support on
	bundle := buildProviderBundle(mockProviderWiring(cfg))
	orch := payments.NewOrchestrator(bundle.paymentsAdapters(), bundle.paymentsOrchestratorResolver())

	s := buildPaymentsSweeper(new(db.Pool), orch, bundle)
	if s.PayoutKYCGate == nil {
		t.Fatal("constructed production sweeper has a nil PayoutKYCGate: the payout half would be inert")
	}
	if _, ok := s.PayoutKYCGate.(payments.KYCEnforcementPayoutGate); !ok {
		t.Fatalf("PayoutKYCGate is %T, want the real KYCEnforcementPayoutGate", s.PayoutKYCGate)
	}
	if _, ok := s.KYCGate.(payments.KYCEnforcementDepositGate); !ok {
		t.Fatalf("KYCGate is %T, want the real KYCEnforcementDepositGate", s.KYCGate)
	}
	if s.Orchestrator != orch {
		t.Fatal("the sweeper must use the SAME orchestrator (and so the same guarded adapter registry) as the HTTP path")
	}
	if s.CredResolver == nil {
		t.Fatal("the sweeper needs the bundle's per-tenant outbound credential resolver")
	}
	if !s.TenantAdvisoryHint {
		t.Fatal("expected the efficiency hint to be enabled in production wiring")
	}
	if err := s.ValidateForLoop(); err != nil {
		t.Fatalf("production wiring must pass ValidateForLoop: %v", err)
	}
}

// With test support off there is no outbound resolver (every provider call is
// refused fail-closed, as on the HTTP path); the sweeper must still be
// constructible and start, never turn that into a startup failure.
func TestBuildPaymentsSweeper_NoResolverStillValid(t *testing.T) {
	cfg := baseConfig(t, "development", true)
	bundle := buildProviderBundle(mockProviderWiring(cfg))
	orch := payments.NewOrchestrator(bundle.paymentsAdapters(), bundle.paymentsOrchestratorResolver())
	s := buildPaymentsSweeper(new(db.Pool), orch, bundle)
	if s.PayoutKYCGate == nil {
		t.Fatal("PayoutKYCGate must be set regardless of credential wiring")
	}
	if err := s.ValidateForLoop(); err != nil {
		t.Fatalf("ValidateForLoop: %v", err)
	}
}

// Negative control: the stock constructor alone is refused by ValidateForLoop,
// so the guard bites exactly the H3 shape.
func TestSweeperValidateForLoop_RefusesNilPayoutKYCGate(t *testing.T) {
	cfg := baseConfig(t, "development", true)
	bundle := buildProviderBundle(mockProviderWiring(cfg))
	orch := payments.NewOrchestrator(bundle.paymentsAdapters(), bundle.paymentsOrchestratorResolver())
	s := payments.NewSweeper(new(db.Pool), orch, payments.KYCEnforcementDepositGate{}, bundle.paymentsOutboundCredentials())
	if s.PayoutKYCGate != nil {
		t.Fatal("test premise: NewSweeper is expected to leave PayoutKYCGate nil")
	}
	if err := s.ValidateForLoop(); err == nil {
		t.Fatal("expected a refusal")
	}
}

// main.go must start the loop through buildPaymentsSweeper + ValidateForLoop (never
// a hand-built Sweeper) and must start it after the pre-DB guards.
func TestRun_StartsSweeperThroughValidatedConstructor(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var run *ast.FuncDecl
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "run" {
			run = fn
		}
	}
	if run == nil {
		t.Fatal("main.go has no run()")
	}
	first := map[string]token.Pos{}
	insideGo := map[string]bool{}
	ast.Inspect(run.Body, func(n ast.Node) bool {
		if g, ok := n.(*ast.GoStmt); ok {
			ast.Inspect(g, func(m ast.Node) bool {
				if c, ok := m.(*ast.CallExpr); ok {
					if sel, ok := c.Fun.(*ast.SelectorExpr); ok {
						if id, ok := sel.X.(*ast.Ident); ok {
							insideGo[id.Name+"."+sel.Sel.Name] = true
						}
					}
				}
				return true
			})
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch f := call.Fun.(type) {
		case *ast.Ident:
			name = f.Name
		case *ast.SelectorExpr:
			if id, ok := f.X.(*ast.Ident); ok {
				name = id.Name + "." + f.Sel.Name
			}
		}
		if _, seen := first[name]; name != "" && !seen {
			first[name] = call.Pos()
		}
		return true
	})
	for _, want := range []string{"buildPaymentsSweeper", "paymentsSweeper.ValidateForLoop", "payments.RunSweeperLoop"} {
		if _, ok := first[want]; !ok {
			t.Fatalf("run() does not call %s", want)
		}
	}
	if _, bad := first["payments.NewSweeper"]; bad {
		t.Fatal("run() must not construct a Sweeper by hand: use buildPaymentsSweeper (sets PayoutKYCGate)")
	}
	if first["buildPaymentsSweeper"] >= first["paymentsSweeper.ValidateForLoop"] || first["paymentsSweeper.ValidateForLoop"] >= first["payments.RunSweeperLoop"] {
		t.Fatal("expected build, then validate, then start")
	}
	if !insideGo["payments.RunSweeperLoop"] {
		t.Fatal("payments.RunSweeperLoop must run in its own goroutine")
	}
	if first["payments.RunSweeperLoop"] < first["db.Connect"] || first["buildPaymentsSweeper"] < first["refuseSyntheticInProduction"] {
		t.Fatal("the sweeper must start after the synthetic guard and the database connection")
	}
}
