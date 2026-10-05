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
// so "no non-MOCK source" is evaluated PER PROVIDER (owner ruling H-W2): the MOCK
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
	if !(assignPos < coveragePos && coveragePos < registerPos && registerPos < connectPos && registerPos < newServerPos) {
		t.Fatalf("order must be: assign < coverage gate < register < db.Connect and < HTTP server construction (%v %v %v %v %v)",
			assignPos, coveragePos, registerPos, connectPos, newServerPos)
	}
}
