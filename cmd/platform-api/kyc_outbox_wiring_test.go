package main

// PRH-2 E1 (ADR 0106 section 7.4, security D1; test 58): the KYC submission
// outbox worker is built from the SAME orchestrator and outbound credential
// resolver as the HTTP path, started through a validated builder in its own
// goroutine on the run context with the configured interval, drained on
// shutdown, and NOT started (never failing startup) when no orchestrator is
// wired (production / test-support off). The two main.go blocks are delimited
// and placed after the I-wire blocks.

import (
	"go/ast"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/kyc"
)

func runBody(t *testing.T, src string) *ast.FuncDecl {
	t.Helper()
	f := parseSrc(t, "main.go", src)
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "run" {
			return fn
		}
	}
	t.Fatal("main.go has no run()")
	return nil
}

func readMain(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(".", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exprText(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprText(x.X) + "." + x.Sel.Name
	}
	return ""
}

// checkKYCOutboxWiring is the whole structural check (so negative controls can
// prove each rule bites).
func checkKYCOutboxWiring(t *testing.T, src string) []string {
	t.Helper()
	var v []string
	run := runBody(t, src)

	var builder, loop, validate, newWorker, kycOrchCalls, credCalls int
	var loopCall *ast.CallExpr
	insideGo := map[string]bool{}
	ast.Inspect(run.Body, func(n ast.Node) bool {
		if g, ok := n.(*ast.GoStmt); ok {
			ast.Inspect(g, func(m ast.Node) bool {
				if c, ok := m.(*ast.CallExpr); ok {
					insideGo[exprText(c.Fun)] = true
				}
				return true
			})
		}
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch exprText(c.Fun) {
		case "buildKYCOutboxWorker":
			builder++
			if len(c.Args) != 3 || exprText(c.Args[1]) != "kycOrch" || exprText(c.Args[2]) != "kycOutboundCreds" {
				v = append(v, "buildKYCOutboxWorker must be given the SAME kycOrch / kycOutboundCreds locals as the HTTP Deps (D1)")
			}
		case "kyc.RunOutboxWorkerLoop":
			loop++
			loopCall = c
		case "kycOutboxWorker.ValidateForLoop":
			validate++
		case "kyc.NewOutboxWorker":
			newWorker++
		case "kycOrchestrator":
			kycOrchCalls++
		case "providers.kycOutboundCredentials":
			credCalls++
		}
		return true
	})
	if builder != 1 {
		v = append(v, "run() must call buildKYCOutboxWorker exactly once")
	}
	if loop != 1 {
		v = append(v, "run() must start exactly one kyc.RunOutboxWorkerLoop")
	}
	if validate != 1 {
		v = append(v, "run() must call kycOutboxWorker.ValidateForLoop exactly once")
	}
	if newWorker != 0 {
		v = append(v, "run() must not construct an OutboxWorker by hand: use buildKYCOutboxWorker")
	}
	if kycOrchCalls != 1 || credCalls != 1 {
		v = append(v, "the KYC orchestrator and the outbound credential resolver must each be built ONCE (hoisted) so the HTTP path and the worker share them (D1)")
	}
	if loopCall != nil {
		if len(loopCall.Args) != 4 || exprText(loopCall.Args[0]) != "ctx" || exprText(loopCall.Args[1]) != "kycOutboxWorker" ||
			exprText(loopCall.Args[3]) != "cfg.KYCOutboxInterval" {
			v = append(v, "the loop must run on the run ctx, with the built worker and cfg.KYCOutboxInterval")
		}
	}
	if !insideGo["kyc.RunOutboxWorkerLoop"] {
		v = append(v, "kyc.RunOutboxWorkerLoop must run in its own goroutine")
	}

	// The nil branch logs and never returns an error (startup never fails).
	nilBranchOK := false
	ast.Inspect(run.Body, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		b, ok := ifs.Cond.(*ast.BinaryExpr)
		if !ok || exprText(b.X) != "kycOutboxWorker" || exprText(b.Y) != "nil" {
			return true
		}
		returns := false
		logs := false
		ast.Inspect(ifs.Body, func(m ast.Node) bool {
			switch x := m.(type) {
			case *ast.ReturnStmt:
				returns = true
			case *ast.CallExpr:
				if exprText(x.Fun) == "logger.Info" {
					logs = true
				}
			}
			return true
		})
		nilBranchOK = logs && !returns
		return true
	})
	if !nilBranchOK {
		v = append(v, "a nil worker must log once and must NOT return an error from run()")
	}

	// Drain: a goroutine waits on the WaitGroup and a select races it against time.After.
	if !strings.Contains(src, "kycOutboxWG.Wait()") || !strings.Contains(src, "close(kycOutboxDone)") || !strings.Contains(src, "case <-kycOutboxDone:") {
		v = append(v, "the shutdown drain (kycOutboxWG.Wait + kycOutboxDone) is missing")
	}

	// Delimited blocks, placed after the I-wire blocks, drain before "shutdown complete".
	idx := func(s string) int { return strings.Index(src, s) }
	order := []string{
		"PRH-2 I-wire: durable alert dispatcher (end)",
		"PRH-2 E1 (KYC-SUBMIT-OUTBOX-1): KYC submission outbox worker (begin)",
		"PRH-2 E1: KYC submission outbox worker (end)",
		"PRH-2 I-wire: alert dispatcher shutdown drain (end)",
		"PRH-2 E1: KYC outbox worker shutdown drain (begin)",
		"PRH-2 E1: KYC outbox worker shutdown drain (end)",
		`logger.Info("shutdown complete")`,
	}
	last := -1
	for _, marker := range order {
		i := idx(marker)
		if i < 0 {
			v = append(v, "marker missing: "+marker)
			continue
		}
		if i < last {
			v = append(v, "marker out of order: "+marker)
		}
		last = i
	}
	return v
}

func TestMain_KYCOutboxWorkerWiring_58(t *testing.T) {
	if v := checkKYCOutboxWiring(t, readMain(t)); len(v) != 0 {
		t.Fatalf("main.go KYC outbox wiring violations:\n  %s", strings.Join(v, "\n  "))
	}
}

// Negative controls: each rule must be able to fail.
func TestMain_KYCOutboxWorkerWiring_NegativeControls(t *testing.T) {
	good := readMain(t)
	mutants := map[string]func(string) string{
		"loop without the validated builder": func(s string) string {
			return strings.Replace(s, "kycOutboxWorker := buildKYCOutboxWorker(pool, kycOrch, kycOutboundCreds)", "kycOutboxWorker := kyc.NewOutboxWorker(pool, kycOrch, kycOutboundCreds)", 1)
		},
		"a second orchestrator for the worker": func(s string) string {
			return strings.Replace(s, "buildKYCOutboxWorker(pool, kycOrch, kycOutboundCreds)", "buildKYCOutboxWorker(pool, kycOrchestrator(wiring, providers, logger), kycOutboundCreds)", 1)
		},
		"a different credential resolver": func(s string) string {
			return strings.Replace(s, "buildKYCOutboxWorker(pool, kycOrch, kycOutboundCreds)", "buildKYCOutboxWorker(pool, kycOrch, providers.kycOutboundCredentials())", 1)
		},
		"loop on context.Background": func(s string) string {
			return strings.Replace(s, "kyc.RunOutboxWorkerLoop(ctx, kycOutboxWorker", "kyc.RunOutboxWorkerLoop(context.Background(), kycOutboxWorker", 1)
		},
		"loop with a literal interval": func(s string) string { return strings.Replace(s, "cfg.KYCOutboxInterval", "5*time.Second", 1) },
		"nil worker fails startup": func(s string) string {
			return strings.Replace(s, `logger.Info("kyc outbox worker not started: no KYC orchestrator or outbound credential resolver is wired")`, `return fmt.Errorf("no kyc orchestrator")`, 1)
		},
		"no shutdown drain": func(s string) string { return strings.Replace(s, "kycOutboxWG.Wait()", "", 1) },
		"E1 start block before the I-wire end": func(s string) string {
			return strings.Replace(s, "PRH-2 E1 (KYC-SUBMIT-OUTBOX-1): KYC submission outbox worker (begin)", "PRH-2 E1 begin moved", 1)
		},
	}
	for name, mutate := range mutants {
		mutated := mutate(good)
		if mutated == good {
			t.Fatalf("%s: the mutation did not apply (the check would be vacuous)", name)
		}
		if v := checkKYCOutboxWiring(t, mutated); len(v) == 0 {
			t.Errorf("%s: the wiring check must flag this mutation", name)
		}
	}
}

// D1: the builder uses the SAME orchestrator and resolver it is given; nil
// orchestrator or nil resolver => nil worker (not started, never an error); both
// branches.
func TestBuildKYCOutboxWorker_D1_BothBranches(t *testing.T) {
	// Production / test-support off: the orchestrator is a true nil.
	off := mockWiring{}
	bOff := buildProviderBundle(off)
	if orch := kycOrchestrator(off, bOff, nil); orch != nil {
		t.Fatal("test premise: no orchestrator with test support off")
	}
	if w := buildKYCOutboxWorker(new(db.Pool), nil, bOff.kycOutboundCredentials()); w != nil {
		t.Fatal("a nil orchestrator must yield no worker (the create handler 503s before any row)")
	}
	if w := buildKYCOutboxWorker(new(db.Pool), kyc.NewOrchestrator(map[string]kyc.KYCProvider{"mock": kyc.NewMockKYCProvider()}, nil), nil); w != nil {
		t.Fatal("a nil outbound resolver must yield no worker")
	}

	// Test support on: the worker holds the very orchestrator and resolver it was given.
	cfg := baseConfig(t, "development", true)
	cfg.TestSupportEndpointsEnabled = true
	wiring := mockProviderWiring(cfg)
	bundle := buildProviderBundle(wiring)
	orch := kycOrchestrator(wiring, bundle, nil)
	creds := bundle.kycOutboundCredentials()
	if orch == nil || creds == nil {
		t.Fatal("test premise: orchestrator and resolver present with test support on")
	}
	w := buildKYCOutboxWorker(new(db.Pool), orch, creds)
	if w == nil {
		t.Fatal("expected a worker")
	}
	if w.Orchestrator != orch {
		t.Fatal("D1: the worker must use the SAME orchestrator as the HTTP path")
	}
	if w.Outbound != creds {
		t.Fatal("D1: the worker must use the SAME outbound credential resolver as the HTTP path")
	}
	if err := w.ValidateForLoop(); err != nil {
		t.Fatalf("the production wiring must pass ValidateForLoop: %v", err)
	}
}
