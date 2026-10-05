package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// PAY-K3-STATEMENT-SOURCE-WIRING-1 tests. The registry is keyed by provider id,
// so "no non-MOCK source" is evaluated PER PROVIDER (orchestrator engineering ruling H-W2, reversible; PRH-2-ROUND2-ENGINEERING-RULINGS): the MOCK
// source unlocks only the mock provider's attempts.

// fakeRealAdapter is a payments adapter WITHOUT the Synthetic marker (embedding
// the interface promotes only interface methods, never SyntheticComponent).
type fakeRealAdapter struct {
	payments.PaymentProvider
	id string
}

func (f fakeRealAdapter) Capabilities() payments.AdapterCapability {
	c := f.PaymentProvider.Capabilities()
	c.ProviderID = f.id
	return c
}

// fakeStmtSource is a statement source with a settable id; embedding the
// interface hides the Synthetic marker of any wrapped MOCK.
type fakeStmtSource struct {
	statement.PaymentStatementSource
	id string
}

func (f fakeStmtSource) ProviderID() string { return f.id }

// fakeMockStmtSource is fakeStmtSource that DOES carry the Synthetic marker.
type fakeMockStmtSource struct{ fakeStmtSource }

func (fakeMockStmtSource) SyntheticComponent() {}

func devBundle(t *testing.T) providerBundle {
	t.Helper()
	return buildProviderBundle(mockProviderWiring(baseConfig(t, "development", true)))
}

// The registered set equals the scheduled set: both come from
// paymentStatementSources(), and the registry holds exactly those ids.
func TestWiring_RegisteredSetEqualsScheduledSet(t *testing.T) {
	b := devBundle(t)
	sched := b.paymentStatementSources()
	if len(sched) != 1 {
		t.Fatalf("want exactly the MOCK payments statement source scheduled, got %d", len(sched))
	}
	reg := new(payments.StatementSourceRegistry)
	if err := registerPaymentStatementSources(reg, sched); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, s := range sched {
		want[s.ProviderID()] = true
		if !reg.Registered(s.ProviderID()) {
			t.Fatalf("scheduled provider %q not registered", s.ProviderID())
		}
	}
	if want["mock-payments"] != true || len(want) != 1 {
		t.Fatalf("unexpected scheduled set %v", want)
	}
	// Every other provider id (any real PSP) stays unregistered => M2 refused.
	for _, id := range []string{"", "real-psp", "mock-payments-2", "MOCK-PAYMENTS", "mock-casino"} {
		if reg.Registered(id) {
			t.Fatalf("provider %q must NOT be registered", id)
		}
	}
	// And the dev bundle passes the coverage gate (MOCK adapter + MOCK source).
	if err := checkPaymentStatementCoverage(b.paymentsAdapters(), sched); err != nil {
		t.Fatalf("dev bundle must pass the coverage gate: %v", err)
	}
}

func TestWiring_RegistrationFailsStartupOnBadSource(t *testing.T) {
	b := devBundle(t)
	good := b.PaymentsStmt
	var typedNil *payments.MockStatementSource
	cases := map[string][]statement.PaymentStatementSource{
		"nil interface":    {nil},
		"typed nil":        {typedNil},
		"empty id":         {fakeStmtSource{id: ""}},
		"blank id":         {fakeStmtSource{id: "   "}},
		"padded id":        {fakeStmtSource{id: " x "}},
		"duplicate id":     {good, good},
		"good then nil":    {good, nil},
		"mock w/o adapter": {payments.NewMockStatementSource(nil, payments.MockCredentialResolver{})},
	}
	for name, srcs := range cases {
		reg := new(payments.StatementSourceRegistry)
		if err := registerPaymentStatementSources(reg, srcs); err == nil {
			t.Fatalf("%s: want a startup error", name)
		}
		// All-or-nothing: a refused list registers nothing.
		if reg.Registered("mock-payments") {
			t.Fatalf("%s: a refused list must register nothing", name)
		}
	}
	if err := registerPaymentStatementSources(nil, []statement.PaymentStatementSource{good}); err == nil {
		t.Fatal("nil registry must fail startup")
	}
	// An empty list is valid (no sources: every M2 refused) and registers nothing.
	reg := new(payments.StatementSourceRegistry)
	if err := registerPaymentStatementSources(reg, nil); err != nil {
		t.Fatal(err)
	}
	if reg.Registered("mock-payments") {
		t.Fatal("empty list registered something")
	}
}

func TestWiring_CoverageGate(t *testing.T) {
	b := devBundle(t)
	mockAdapter := b.Payments
	real := func(id string) payments.PaymentProvider { return fakeRealAdapter{PaymentProvider: mockAdapter, id: id} }
	realSrc := func(id string) statement.PaymentStatementSource { return fakeStmtSource{id: id} }
	mockSrc := func(id string) statement.PaymentStatementSource { return fakeMockStmtSource{fakeStmtSource{id: id}} }

	// (i) a real adapter with no source at all is refused.
	if err := checkPaymentStatementCoverage(map[string]payments.PaymentProvider{"psp-a": real("psp-a")}, nil); err == nil {
		t.Fatal("real adapter without a source must be refused")
	}
	// (i-b) a real adapter with only the MOCK source of another id is refused.
	if err := checkPaymentStatementCoverage(map[string]payments.PaymentProvider{"psp-a": real("psp-a")}, b.paymentStatementSources()); err == nil {
		t.Fatal("real adapter with only the mock-payments MOCK source must be refused")
	}
	// (ii) a MOCK source carrying the real adapter's id never vouches for it.
	err := checkPaymentStatementCoverage(map[string]payments.PaymentProvider{"psp-a": real("psp-a")}, []statement.PaymentStatementSource{mockSrc("psp-a")})
	if err == nil || !strings.Contains(err.Error(), "MOCK") {
		t.Fatalf("MOCK source with a real adapter's id must be refused, got %v", err)
	}
	// MOCK source carrying the id AND a real source for it: still refused (ambiguity).
	if err := checkPaymentStatementCoverage(map[string]payments.PaymentProvider{"psp-a": real("psp-a")},
		[]statement.PaymentStatementSource{realSrc("psp-a"), mockSrc("psp-a")}); err == nil {
		t.Fatal("a MOCK source sharing a real adapter's id must be refused even beside a real source")
	}
	// A real source under a different id does not cover the adapter.
	if err := checkPaymentStatementCoverage(map[string]payments.PaymentProvider{"psp-a": real("psp-a")}, []statement.PaymentStatementSource{realSrc("psp-b")}); err == nil {
		t.Fatal("source id mismatch must be refused")
	}
	// (iii) real adapter + real source with the same id boots; MOCK + MOCK boots.
	if err := checkPaymentStatementCoverage(map[string]payments.PaymentProvider{"psp-a": real("psp-a")}, []statement.PaymentStatementSource{realSrc("psp-a")}); err != nil {
		t.Fatalf("real+real same id: %v", err)
	}
	if err := checkPaymentStatementCoverage(b.paymentsAdapters(), b.paymentStatementSources()); err != nil {
		t.Fatalf("mock+mock: %v", err)
	}
	// Mixed: the MOCK pair is fine alongside a covered real pair.
	adapters := b.paymentsAdapters()
	adapters["psp-a"] = real("psp-a")
	if err := checkPaymentStatementCoverage(adapters, append(b.paymentStatementSources(), realSrc("psp-a"))); err != nil {
		t.Fatalf("mixed covered: %v", err)
	}
	// The adapter's own Capabilities().ProviderID decides, not the map key.
	if err := checkPaymentStatementCoverage(map[string]payments.PaymentProvider{"alias": real("psp-a")}, []statement.PaymentStatementSource{realSrc("alias")}); err == nil {
		t.Fatal("the adapter's own provider id, not the map key, must be matched")
	}
}

// Static pin: in run(), ONE variable (paySources, assigned from
// providers.paymentStatementSources()) is (1) passed to the coverage gate,
// (2) registered, and (3) spread as the scheduler's trailing argument - and all
// of it happens before db.Connect and before the HTTP server is built, and
// Deps.StatementSources receives the registry that was filled.
func TestRun_StatementSourcesRegisteredAndScheduledFromOneList(t *testing.T) {
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
		t.Fatal("no run()")
	}
	paySourcesWrites := 0
	var (
		assignedFrom       string
		assignPos          token.Pos
		coverageArg        string
		registerArgs       []string
		registerPos        token.Pos
		coveragePos        token.Pos
		schedulerLast      string
		schedulerEllipsis  bool
		connectPos         token.Pos
		newServerPos       token.Pos
		depsStmtSourcesVal string
	)
	ast.Inspect(run.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			// LF C-1/F-3: paySources is written EXACTLY once (its := from the
			// bundle); any later write (reassignment, element or slice write) would
			// let the scheduled slice differ from the registered one.
			for _, lhs := range x.Lhs {
				if paySourcesWrite(lhs) {
					paySourcesWrites++
				}
			}
			if len(x.Lhs) == 1 && len(x.Rhs) == 1 {
				if id, ok := x.Lhs[0].(*ast.Ident); ok && id.Name == "paySources" {
					if call, ok := x.Rhs[0].(*ast.CallExpr); ok {
						if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
							if base, ok := sel.X.(*ast.Ident); ok {
								assignedFrom = base.Name + "." + sel.Sel.Name
								assignPos = x.Pos()
							}
						}
					}
				}
			}
		case *ast.KeyValueExpr:
			if k, ok := x.Key.(*ast.Ident); ok && k.Name == "StatementSources" {
				if v, ok := x.Value.(*ast.Ident); ok {
					depsStmtSourcesVal = v.Name
				}
			}
		case *ast.CallExpr:
			name := ""
			switch f := x.Fun.(type) {
			case *ast.Ident:
				name = f.Name
			case *ast.SelectorExpr:
				if id, ok := f.X.(*ast.Ident); ok {
					name = id.Name + "." + f.Sel.Name
				}
			}
			switch name {
			case "checkPaymentStatementCoverage":
				coveragePos = x.Pos()
				if id, ok := x.Args[len(x.Args)-1].(*ast.Ident); ok {
					coverageArg = id.Name
				}
			case "registerPaymentStatementSources":
				registerPos = x.Pos()
				for _, a := range x.Args {
					if id, ok := a.(*ast.Ident); ok {
						registerArgs = append(registerArgs, id.Name)
					}
				}
			case "reconciliation.RunSchedulerLoop":
				schedulerEllipsis = x.Ellipsis.IsValid()
				if id, ok := x.Args[len(x.Args)-1].(*ast.Ident); ok {
					schedulerLast = id.Name
				}
			case "db.Connect":
				if connectPos == 0 {
					connectPos = x.Pos()
				}
			case "httpserver.NewWithAdmission":
				newServerPos = x.Pos()
			}
		}
		return true
	})
	if paySourcesWrites != 1 {
		t.Fatalf("paySources must be written exactly once in run() (its := from the bundle), got %d writes", paySourcesWrites)
	}
	if assignedFrom != "providers.paymentStatementSources" {
		t.Fatalf("paySources must be assigned from providers.paymentStatementSources(), got %q", assignedFrom)
	}
	if coverageArg != "paySources" {
		t.Fatalf("the coverage gate must take paySources, got %q", coverageArg)
	}
	if len(registerArgs) != 2 || registerArgs[1] != "paySources" {
		t.Fatalf("registerPaymentStatementSources must take (registry, paySources), got %v", registerArgs)
	}
	if schedulerLast != "paySources" || !schedulerEllipsis {
		t.Fatalf("RunSchedulerLoop's trailing argument must be paySources..., got %q ellipsis=%v", schedulerLast, schedulerEllipsis)
	}
	if depsStmtSourcesVal != registerArgs[0] {
		t.Fatalf("Deps.StatementSources must receive the registry that was filled (%q), got %q", registerArgs[0], depsStmtSourcesVal)
	}
	ordered := assignPos < coveragePos && coveragePos < registerPos && registerPos < connectPos && registerPos < newServerPos
	if !ordered {
		t.Fatalf("order must be: assign < coverage gate < register < db.Connect and < HTTP server construction (%v %v %v %v %v)",
			assignPos, coveragePos, registerPos, connectPos, newServerPos)
	}
}

// paySourcesWrite reports whether lhs writes to paySources: the identifier
// itself, an index or slice expression over it, or a dereference of it.
func paySourcesWrite(lhs ast.Expr) bool {
	switch e := lhs.(type) {
	case *ast.Ident:
		return e.Name == "paySources"
	case *ast.IndexExpr:
		return paySourcesWrite(e.X)
	case *ast.SliceExpr:
		return paySourcesWrite(e.X)
	case *ast.StarExpr:
		return paySourcesWrite(e.X)
	case *ast.ParenExpr:
		return paySourcesWrite(e.X)
	}
	return false
}

// Security F-5: every scheduled payment statement source is ALSO in the
// production-guard registration list (buildRegistrations), so a future real
// source appended to paymentStatementSources() cannot skip
// RefuseSyntheticInProduction.
func TestWiring_EveryScheduledSourceIsInTheProductionGuardRegistrations(t *testing.T) {
	cfg := baseConfig(t, "development", true)
	b := buildProviderBundle(mockProviderWiring(cfg))
	regs := buildRegistrations(cfg, b)
	inGuard := func(c any) bool {
		for _, r := range regs {
			if r.Component == c {
				return true
			}
		}
		return false
	}
	srcs := b.paymentStatementSources()
	if len(srcs) == 0 {
		t.Fatal("setup: no scheduled sources")
	}
	for _, src := range srcs {
		if !inGuard(src) {
			t.Fatalf("scheduled payment statement source %T is not in buildRegistrations: it would skip RefuseSyntheticInProduction", src)
		}
	}
	// And the guard really refuses the MOCK source in production.
	if err := refuseSyntheticInProduction(baseConfig(t, "production", true), buildRegistrations(baseConfig(t, "production", true), b)); err == nil ||
		!strings.Contains(err.Error(), "payments/statement_source") {
		t.Fatalf("production must refuse the MOCK payments statement source, got %v", err)
	}
}

// Code review F-5: startup must FAIL on a bad list. In run(), the coverage gate
// and the registration are each `if err := f(...); err != nil { return err }`
// with the condition EXACTLY `err != nil` (a mutant `err != nil && false` or a
// dropped return would let a bad list start).
func TestRun_StatementSourceGatesReturnTheirError(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		as, ok := ifs.Init.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || (id.Name != "checkPaymentStatementCoverage" && id.Name != "registerPaymentStatementSources") {
			return true
		}
		cond, ok := ifs.Cond.(*ast.BinaryExpr)
		if !ok || cond.Op != token.NEQ {
			t.Errorf("%s: the guard condition must be exactly err != nil", id.Name)
			return true
		}
		if x, ok := cond.X.(*ast.Ident); !ok || x.Name != "err" {
			t.Errorf("%s: condition left side must be err", id.Name)
		}
		if y, ok := cond.Y.(*ast.Ident); !ok || y.Name != "nil" {
			t.Errorf("%s: condition right side must be nil", id.Name)
		}
		if len(ifs.Body.List) != 1 {
			t.Errorf("%s: the body must be a single return err", id.Name)
			return true
		}
		ret, ok := ifs.Body.List[0].(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			t.Errorf("%s: the body must be return err", id.Name)
			return true
		}
		if r, ok := ret.Results[0].(*ast.Ident); !ok || r.Name != "err" {
			t.Errorf("%s: must return err unchanged", id.Name)
			return true
		}
		found[id.Name] = true
		return true
	})
	for _, name := range []string{"checkPaymentStatementCoverage", "registerPaymentStatementSources"} {
		if !found[name] {
			t.Errorf("main.go has no pinned fail-startup guard for %s", name)
		}
	}
}
