package main

import (
	"context"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// A11 (PAY-K3-STATEMENT-SOURCE-WIRING-1): the payment statement source REGISTRY the scheduler resolves per
// (tenant, provider). The sweep-level behaviour (missing source = failed run + alert, truncation, cursor,
// tenant isolation) is pinned in internal/reconciliation; these tests pin the wiring table and the
// production refusals.

// realStmt is a NON-Synthetic statement source (a stand-in for a future real PSP source; it never fetches).
type realStmt struct {
	id string
}

func (r realStmt) Label() string      { return "real test source" }
func (r realStmt) ProviderID() string { return r.id }
func (r realStmt) Fetch(context.Context, statement.PaymentFetchRequest) (statement.PaymentStatement, error) {
	return statement.PaymentStatement{}, nil
}

// attestedReal / unattestedReal / prodReal compose the optional markers by embedding.
type attestedReal struct{ realStmt }

func (attestedReal) ReadOnlyStatementCredential() bool { return true }

type falseAttestedReal struct{ realStmt }

func (falseAttestedReal) ReadOnlyStatementCredential() bool { return false }

type prodReal struct{ attestedReal }

func (prodReal) MarkProductionEligible() {}

func TestRegistry_WiringTable_MockAllowedOutsideProductionOnly(t *testing.T) {
	b := devBundle(t)
	srcs := b.paymentStatementSources()
	if len(srcs) != 1 {
		t.Fatalf("want the one MOCK source scheduled, got %d", len(srcs))
	}
	for _, env := range []string{"development", "test", "staging"} {
		r, err := newPaymentStatementRegistry(env, srcs)
		if err != nil {
			t.Fatalf("%s: a MOCK source is allowed where mock components are allowed: %v", env, err)
		}
		src, ok := r.SourceFor(srcs[0].ProviderID())
		if !ok || src != srcs[0] {
			t.Fatalf("%s: the registry must resolve the mock provider's own source", env)
		}
		if got := r.Providers(); len(got) != 1 || got[0] != srcs[0].ProviderID() {
			t.Fatalf("%s: providers = %v", env, got)
		}
		if cfg := r.SweepConfig(123); !cfg.AllowSynthetic || cfg.Resolver == nil || cfg.RunLineBound != 123 {
			t.Fatalf("%s: sweep config must admit Synthetic and carry the resolver and bound: %+v", env, cfg)
		}
	}
	// Production refuses it (an unset APP_ENV is folded to "production" by Config.GuardEnvironment before it gets here).
	r, err := newPaymentStatementRegistry("production", srcs)
	if err == nil || r != nil {
		t.Fatal("a Synthetic (MOCK) statement source must be refused in production")
	}
	if !strings.Contains(err.Error(), "Synthetic") {
		t.Fatalf("the refusal must say why: %v", err)
	}
}

func TestRegistry_ProductionRefusesSyntheticDirectlyAndThroughWrappers(t *testing.T) {
	mock := fakeMockStmtSource{fakeStmtSource{id: "psp-x"}}
	if _, err := newPaymentStatementRegistry("production", []statement.PaymentStatementSource{mock}); err == nil {
		t.Fatal("production must refuse a Synthetic source whatever its provider id")
	}
	// A real-looking provider id does not make a MOCK eligible.
	if _, err := newPaymentStatementRegistry("production", []statement.PaymentStatementSource{
		prodReal{attestedReal{realStmt{id: "psp-a"}}}, mock}); err == nil {
		t.Fatal("one Synthetic source among eligible ones must still refuse the whole list (all-or-nothing)")
	}
}

func TestRegistry_ProductionRequiresProductionEligibleAndReadOnlyAttestation(t *testing.T) {
	// eligible + attested: admitted in production
	r, err := newPaymentStatementRegistry("production", []statement.PaymentStatementSource{prodReal{attestedReal{realStmt{id: "psp-a"}}}})
	if err != nil {
		t.Fatalf("a production-eligible, read-only-attested source must be admitted: %v", err)
	}
	if cfg := r.SweepConfig(10); cfg.AllowSynthetic {
		t.Fatal("production must never admit Synthetic sources in the sweep")
	}
	if _, ok := r.SourceFor("psp-a"); !ok {
		t.Fatal("psp-a must resolve")
	}
	// attested but NOT production-eligible: refused in production, admitted in development
	notEligible := attestedReal{realStmt{id: "psp-b"}}
	if _, err := newPaymentStatementRegistry("production", []statement.PaymentStatementSource{notEligible}); err == nil || !strings.Contains(err.Error(), "production-eligible") {
		t.Fatalf("an unmarked source must be refused in production: %v", err)
	}
	if _, err := newPaymentStatementRegistry("development", []statement.PaymentStatementSource{notEligible}); err != nil {
		t.Fatalf("development admits an attested source: %v", err)
	}
	// no attestation / a false attestation: refused EVERYWHERE (a statement source never holds the payment credential)
	for _, env := range []string{"development", "production"} {
		if _, err := newPaymentStatementRegistry(env, []statement.PaymentStatementSource{realStmt{id: "psp-c"}}); err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Fatalf("%s: a source without the read-only attestation must be refused: %v", env, err)
		}
		if _, err := newPaymentStatementRegistry(env, []statement.PaymentStatementSource{falseAttestedReal{realStmt{id: "psp-d"}}}); err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Fatalf("%s: a false read-only attestation must be refused: %v", env, err)
		}
	}
}

func TestRegistry_RefusesNilBlankMalformedAndDuplicate(t *testing.T) {
	good := attestedReal{realStmt{id: "psp-a"}}
	var typedNil *fakeStmtSource
	cases := map[string][]statement.PaymentStatementSource{
		"nil interface":   {nil},
		"typed nil":       {typedNil},
		"blank id":        {attestedReal{realStmt{id: ""}}},
		"padded id":       {attestedReal{realStmt{id: " psp-a"}}},
		"duplicate":       {good, good},
		"nil among good":  {good, nil},
		"duplicate (mix)": {good, attestedReal{realStmt{id: "psp-a"}}},
	}
	for name, srcs := range cases {
		r, err := newPaymentStatementRegistry("development", srcs)
		if err == nil || r != nil {
			t.Errorf("%s: want a startup error and no registry, got %v / %v", name, r, err)
		}
	}
}

func TestRegistry_NilAndEmptyRegistriesResolveNothing(t *testing.T) {
	var nilReg *paymentStatementRegistry
	if src, ok := nilReg.SourceFor("anything"); ok || src != nil {
		t.Fatal("a nil registry resolves nothing")
	}
	if nilReg.Providers() != nil {
		t.Fatal("a nil registry has no providers")
	}
	if cfg := nilReg.SweepConfig(5); cfg.Resolver != nil || cfg.AllowSynthetic || cfg.RunLineBound != 5 {
		t.Fatalf("a nil registry must give a fail-closed sweep config: %+v", cfg)
	}
	empty, err := newPaymentStatementRegistry("production", nil)
	if err != nil {
		t.Fatalf("production with no sources is a valid (fail-closed) registry: %v", err)
	}
	if _, ok := empty.SourceFor("psp-a"); ok {
		t.Fatal("an empty registry resolves nothing; the sweep then reports every configured provider as not reconciled")
	}
	// An unknown provider is not resolved by a populated registry either (no fallback to 'the' source).
	r, _ := newPaymentStatementRegistry("development", []statement.PaymentStatementSource{attestedReal{realStmt{id: "psp-a"}}})
	if _, ok := r.SourceFor("psp-b"); ok {
		t.Fatal("a source must resolve only for its own provider id")
	}
	if _, ok := r.SourceFor(""); ok {
		t.Fatal("the empty provider id must not resolve")
	}
}

// The registry satisfies the interface the sweep consumes, and the production loop is the scoped one.
var _ reconciliation.PaymentSourceResolver = (*paymentStatementRegistry)(nil)
