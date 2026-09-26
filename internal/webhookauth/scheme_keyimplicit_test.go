package webhookauth

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestValidateScheme_KeyImplicitRegistersSinceW2a replaces gate 10.3-W1's
// TestValidateScheme_KeyImplicitRefusedUntilW2a (code review #10): Stage
// 10.3 W2a implements KeyImplicit resolution (ADR 0093 §4 - the active
// handle plus at most one verify_only predecessor, from one handle read),
// so the registration-time refusal is lifted. A manifest-listed, otherwise
// permitted KeyImplicit scheme now registers exactly like the identical
// KeyFromHeader declaration; every OTHER registration rule still applies
// (an unlisted KeyImplicit scheme is still refused).
func TestValidateScheme_KeyImplicitRegistersSinceW2a(t *testing.T) {
	extractOK := func(Inbound) (AuthMaterial, Reason, bool) { return NewAuthMaterial("", nil), "", true }
	verifyOK := func(c CredentialSet, _ Inbound, _ AuthMaterial, _ time.Time) (string, error) {
		return c.Active.KeyID, nil
	}
	implicit := spyScheme{name: "vendor-implicit-v1", props: realProps(), extract: extractOK, verify: verifyOK}
	implicit.props.KeySelection = KeyImplicit

	if err := ValidateScheme(implicit); err == nil {
		t.Fatal("an unlisted KeyImplicit scheme must still be refused (conformance manifest rule)")
	}
	conformanceManifest["vendor-implicit-v1"] = struct{}{}
	t.Cleanup(func() { delete(conformanceManifest, "vendor-implicit-v1") })

	if err := ValidateScheme(implicit); err != nil {
		t.Fatalf("a listed, permitted KeyImplicit scheme must register since W2a, got %v", err)
	}
	if _, err := NewAdapterSchemeSet("payments", map[string]SchemeSource{"vendor-implicit": eligibleAdapter{implicit}}); err != nil {
		t.Fatalf("NewAdapterSchemeSet must accept a listed KeyImplicit scheme since W2a, got %v", err)
	}
}

// TestResolveCredentials_KeyImplicitPairRules pins the platform checks on a
// KeyImplicit set (ADR 0022 §3 point 2 as amended; ADR 0093 §4): the
// resolver is asked once with an empty key id and KeyImplicit; the active
// credential and an optional predecessor must both be bound to the
// callback's tenant and provider; the predecessor must have its own key id
// and a non-zero not_after. Anything else fails closed as
// credential_unavailable, and resolver errors fold into their closed
// reasons.
func TestResolveCredentials_KeyImplicitPairRules(t *testing.T) {
	tenant := uuid.New()
	in := Inbound{TenantID: tenant, ProviderID: "vendor-a"}
	props := realProps()
	props.KeySelection = KeyImplicit
	scheme := spyScheme{name: "x", props: props}
	now := time.Now()
	active := Credential{TenantID: tenant, ProviderID: "vendor-a", KeyID: "k-new", Secret: make([]byte, 32)}
	prev := Credential{TenantID: tenant, ProviderID: "vendor-a", KeyID: "k-old", Secret: make([]byte, 32), NotAfter: now.Add(time.Hour)}

	t.Run("active only", func(t *testing.T) {
		r := &countingResolver{cred: active}
		set, e := ResolveCredentials(context.Background(), nil, scheme, r, in, NewAuthMaterial("", nil))
		if e != nil || set.Active.KeyID != "k-new" || set.Previous != nil || r.calls != 1 || r.lastSel != KeyImplicit || r.lastKey != "" {
			t.Fatalf("got %v %v calls=%d sel=%v key=%q", set, e, r.calls, r.lastSel, r.lastKey)
		}
	})
	t.Run("active plus predecessor", func(t *testing.T) {
		p := prev
		set, e := ResolveCredentials(context.Background(), nil, scheme, &countingResolver{cred: active, prev: &p}, in, NewAuthMaterial("", nil))
		if e != nil || set.Previous == nil || set.Previous.KeyID != "k-old" {
			t.Fatalf("got %v %v", set, e)
		}
	})
	for name, mutate := range map[string]func(a, p *Credential){
		"active other tenant":        func(a, _ *Credential) { a.TenantID = uuid.New() },
		"active other provider":      func(a, _ *Credential) { a.ProviderID = "vendor-b" },
		"active empty key id":        func(a, _ *Credential) { a.KeyID = "" },
		"predecessor other tenant":   func(_, p *Credential) { p.TenantID = uuid.New() },
		"predecessor other provider": func(_, p *Credential) { p.ProviderID = "vendor-b" },
		"predecessor same key id":    func(_, p *Credential) { p.KeyID = "k-new" },
		"predecessor zero not_after": func(_, p *Credential) { p.NotAfter = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			a, p := active, prev
			mutate(&a, &p)
			if _, e := ResolveCredentials(context.Background(), nil, scheme, &countingResolver{cred: a, prev: &p}, in, NewAuthMaterial("", nil)); e == nil || e.Reason != ReasonCredentialUnavailable {
				t.Fatalf("must fail closed as credential_unavailable, got %v", e)
			}
		})
	}
	for err, want := range map[error]Reason{
		ErrCredentialUnavailable:      ReasonCredentialUnavailable,
		ErrCredentialStoreUnavailable: ReasonCredentialStoreUnavailable,
		ErrCredentialIntegrity:        ReasonCredentialIntegrity,
		ErrNoResolver:                 ReasonNoResolver,
	} {
		if _, e := ResolveCredentials(context.Background(), nil, scheme, &countingResolver{err: err}, in, NewAuthMaterial("", nil)); e == nil || e.Reason != want {
			t.Fatalf("resolver error %v: got %v, want %s", err, e, want)
		}
	}
	if _, e := ResolveCredentials(context.Background(), nil, scheme, nil, in, NewAuthMaterial("", nil)); e == nil || e.Reason != ReasonNoResolver {
		t.Fatalf("nil resolver: got %v", e)
	}
}

// TestResolveCredentials_KeyFromHeaderRefusesPrevious: a KeyFromHeader set
// that carries a Previous credential is a resolver bug and fails closed; a
// KeyFromHeader scheme never receives a predecessor (security C3/C4).
func TestResolveCredentials_KeyFromHeaderRefusesPrevious(t *testing.T) {
	tenant := uuid.New()
	in := Inbound{TenantID: tenant, ProviderID: "vendor-a"}
	good := Credential{TenantID: tenant, ProviderID: "vendor-a", KeyID: "k-1", Secret: make([]byte, 32)}
	prev := good
	prev.KeyID = "k-0"
	r := &countingResolver{cred: good, prev: &prev}
	if _, e := ResolveCredentials(context.Background(), nil, spyScheme{name: "x", props: realProps()}, r, in, NewAuthMaterial("k-1", nil)); e == nil || e.Reason != ReasonCredentialUnavailable {
		t.Fatalf("got %v", e)
	}
	if r.lastSel != KeyFromHeader || r.lastKey != "k-1" {
		t.Fatalf("KeyFromHeader must resolve by the header key id with KeyFromHeader, got sel=%v key=%q", r.lastSel, r.lastKey)
	}
}

// TestKindSplitResolver routes synthetic adapters to the MOCK resolver and
// every other adapter to the real resolver (ADR 0093 §4 wiring); an
// unregistered provider id fails closed; a missing side is no_resolver.
func TestKindSplitResolver(t *testing.T) {
	tenant := uuid.New()
	mock := &countingResolver{cred: Credential{TenantID: tenant, ProviderID: "mock", KeyID: "m"}}
	real := &countingResolver{cred: Credential{TenantID: tenant, ProviderID: "vendor", KeyID: "r"}}
	adapters := map[string]any{"mock": syntheticThing{}, "vendor": struct{}{}}

	if NewKindSplitResolver(adapters, nil, nil) != nil {
		t.Fatal("both sides nil must yield a TRUE nil resolver")
	}
	split := NewKindSplitResolver(adapters, mock, real)
	if set, err := split.Resolve(context.Background(), nil, tenant, "mock", "m", KeyFromHeader); err != nil || set.Active.KeyID != "m" || mock.calls != 1 || real.calls != 0 {
		t.Fatalf("synthetic adapter must go to the mock: %v %v", set, err)
	}
	if set, err := split.Resolve(context.Background(), nil, tenant, "vendor", "r", KeyFromHeader); err != nil || set.Active.KeyID != "r" || real.calls != 1 {
		t.Fatalf("non-synthetic adapter must go to the real resolver: %v %v", set, err)
	}
	if _, err := split.Resolve(context.Background(), nil, tenant, "unknown", "r", KeyFromHeader); err != ErrCredentialUnavailable {
		t.Fatalf("unregistered provider must fail closed, got %v", err)
	}
	mockOnly := NewKindSplitResolver(adapters, mock, nil)
	if _, err := mockOnly.Resolve(context.Background(), nil, tenant, "vendor", "r", KeyFromHeader); err != ErrNoResolver {
		t.Fatalf("a real adapter with no real resolver must be no_resolver, got %v", err)
	}
	realOnly := NewKindSplitResolver(adapters, nil, real)
	if _, err := realOnly.Resolve(context.Background(), nil, tenant, "mock", "m", KeyFromHeader); err != ErrNoResolver {
		t.Fatalf("a synthetic adapter with no mock resolver must be no_resolver, got %v", err)
	}
}

type syntheticThing struct{}

func (syntheticThing) SyntheticComponent() {}
