package webhookauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// spyScheme is a configurable VerificationScheme for the platform-helper
// tests. It records what the platform handed it.
type spyScheme struct {
	name       string
	props      SchemeProperties
	extract    func(Inbound) (AuthMaterial, Reason, bool)
	verify     func(CredentialSet, Inbound, AuthMaterial, time.Time) (string, error)
	verifyRuns *int
	lastCreds  *CredentialSet
}

func (s spyScheme) Name() string                                    { return s.name }
func (s spyScheme) Properties() SchemeProperties                    { return s.props }
func (s spyScheme) Extract(in Inbound) (AuthMaterial, Reason, bool) { return s.extract(in) }
func (s spyScheme) Verify(c CredentialSet, in Inbound, m AuthMaterial, now time.Time) (string, error) {
	if s.verifyRuns != nil {
		*s.verifyRuns++
	}
	if s.lastCreds != nil {
		*s.lastCreds = c
	}
	return s.verify(c, in, m, now)
}

func realProps() SchemeProperties {
	return SchemeProperties{Binding: BindingPerMerchantKey, KeySelection: KeyFromHeader, SignedTimestamp: true, MaxSkew: 5 * time.Minute, Replay: ReplayTimestampWindow}
}

func TestValidateProperties_Matrix(t *testing.T) {
	mock := PaymentsScheme().VerificationScheme().Properties()
	cases := []struct {
		name  string
		p     func() SchemeProperties
		valid bool
	}{
		{"mock declaration", func() SchemeProperties { return mock }, true},
		{"real per-merchant key", realProps, true},
		{"real signed account, implicit key", func() SchemeProperties {
			p := realProps()
			p.Binding, p.KeySelection = BindingPerMerchantKeySignedAccount, KeyImplicit
			return p
		}, true},
		{"MaxSkew exactly at the 10-minute cap", func() SchemeProperties { p := realProps(); p.MaxSkew = MaxSkewCap; return p }, true},
		{"MaxSkew above the cap", func() SchemeProperties { p := realProps(); p.MaxSkew = MaxSkewCap + time.Nanosecond; return p }, false},
		{"MaxSkew zero with signed timestamp", func() SchemeProperties { p := realProps(); p.MaxSkew = 0; return p }, false},
		{"MaxSkew negative", func() SchemeProperties { p := realProps(); p.MaxSkew = -time.Second; return p }, false},
		{"real scheme without signed timestamp", func() SchemeProperties {
			p := realProps()
			p.SignedTimestamp, p.MaxSkew, p.Replay = false, 0, ReplayIdempotencyOnly
			return p
		}, false},
		{"real scheme claiming signed tenant", func() SchemeProperties { p := realProps(); p.Binding = BindingSignedTenant; return p }, false},
		{"unknown binding", func() SchemeProperties { p := realProps(); p.Binding = TenantBinding(99); return p }, false},
		{"zero binding", func() SchemeProperties { p := realProps(); p.Binding = 0; return p }, false},
		{"unknown key selection", func() SchemeProperties { p := realProps(); p.KeySelection = KeySelection(7); return p }, false},
		{"zero key selection", func() SchemeProperties { p := realProps(); p.KeySelection = 0; return p }, false},
		{"unknown replay defence", func() SchemeProperties { p := realProps(); p.Replay = ReplayDefence(9); return p }, false},
		{"timestamp but idempotency-only replay", func() SchemeProperties { p := realProps(); p.Replay = ReplayIdempotencyOnly; return p }, false},
		{"MaxSkew without timestamp (synthetic)", func() SchemeProperties { p := mock; p.MaxSkew = time.Minute; return p }, false},
		{"window replay without timestamp (synthetic)", func() SchemeProperties { p := mock; p.Replay = ReplayTimestampWindow; return p }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateProperties(tc.p())
			if tc.valid && err != nil {
				t.Fatalf("want valid, got %v", err)
			}
			if !tc.valid && err == nil {
				t.Fatal("want the declaration refused, got nil")
			}
		})
	}
}

func TestValidateScheme_RegistrationRules(t *testing.T) {
	extractOK := func(Inbound) (AuthMaterial, Reason, bool) { return NewAuthMaterial("k", nil), "", true }
	verifyOK := func(c CredentialSet, _ Inbound, _ AuthMaterial, _ time.Time) (string, error) {
		return c.Active.KeyID, nil
	}

	for name, v := range map[string]VerificationScheme{
		"payments": PaymentsScheme().VerificationScheme(), "kyc": KYCScheme().VerificationScheme(), "casino": CasinoScheme().VerificationScheme(),
	} {
		if err := ValidateScheme(v); err != nil {
			t.Errorf("%s MOCK scheme refused: %v", name, err)
		}
	}

	fakeSynthetic := spyScheme{name: "fake-mock", props: PaymentsScheme().VerificationScheme().Properties(), extract: extractOK, verify: verifyOK}
	if err := ValidateScheme(fakeSynthetic); err == nil || !strings.Contains(err.Error(), "Synthetic") {
		t.Fatalf("a non-platform scheme declaring Synthetic must be refused, got %v", err)
	}

	vendorScheme := spyScheme{name: "vendor-x-v1", props: realProps(), extract: extractOK, verify: verifyOK}
	if err := ValidateScheme(vendorScheme); err == nil || !strings.Contains(err.Error(), "conformance manifest") {
		t.Fatalf("a non-synthetic scheme outside the conformance manifest must be refused, got %v", err)
	}
	conformanceManifest["vendor-x-v1"] = struct{}{}
	t.Cleanup(func() { delete(conformanceManifest, "vendor-x-v1") })
	if err := ValidateScheme(vendorScheme); err != nil {
		t.Fatalf("a manifest-listed, well-declared scheme must be accepted, got %v", err)
	}
	badSkew := vendorScheme
	badSkew.props.MaxSkew = 11 * time.Minute
	if err := ValidateScheme(badSkew); err == nil {
		t.Fatal("a manifest-listed scheme with MaxSkew above the cap must still be refused")
	}

	if err := ValidateScheme(nil); err == nil {
		t.Fatal("nil scheme must be refused")
	}
	badName := vendorScheme
	badName.name = "Bad Name!"
	if err := ValidateScheme(badName); err == nil {
		t.Fatal("an invalid scheme name must be refused")
	}
}

func TestNewSchemeSet_BadDeclarationFailsStartup(t *testing.T) {
	good := PaymentsScheme().VerificationScheme()
	if _, err := NewSchemeSet("payments", map[string]VerificationScheme{"mock-psp": good}); err != nil {
		t.Fatalf("valid set refused: %v", err)
	}
	if _, err := NewSchemeSet("payments", map[string]VerificationScheme{"Bad_ID": good}); err == nil {
		t.Fatal("a provider id failing the webhook charset must be refused")
	}
	if _, err := NewSchemeSet("payments", map[string]VerificationScheme{"mock-psp": nil}); err == nil {
		t.Fatal("a nil scheme must be refused")
	}
	bad := spyScheme{name: "vendor-y-v1", props: SchemeProperties{Binding: BindingPerMerchantKey, KeySelection: KeyFromHeader}}
	defer func() {
		if recover() == nil {
			t.Fatal("MustSchemeSet must panic (fail startup) on a bad declaration")
		}
	}()
	MustSchemeSet("casino", map[string]VerificationScheme{"vendor-y": bad})
}

func TestSchemeSet_Lookup(t *testing.T) {
	var nilSet *SchemeSet
	if _, ok := nilSet.Lookup("x"); ok {
		t.Fatal("nil set must have no schemes")
	}
	set := MustSchemeSet("kyc", map[string]VerificationScheme{"mock": KYCScheme().VerificationScheme()})
	if v, ok := set.Lookup("mock"); !ok || v.Name() != "platform-mock:"+KYCSigningPrefix {
		t.Fatalf("lookup failed: %v %v", v, ok)
	}
	if _, ok := set.Lookup("other"); ok {
		t.Fatal("unregistered provider must not resolve")
	}
}

// TestCheckInboundPreamble_Order pins the Stage 10.3 order: charset, body
// bound, scheme lookup (provider_unregistered - before any tenant work and
// before Extract), then Extract.
func TestCheckInboundPreamble_Order(t *testing.T) {
	s := PaymentsScheme()
	v := s.VerificationScheme()
	lookup := func(id string) (VerificationScheme, bool) {
		if id == "mock-psp" {
			return v, true
		}
		return nil, false
	}
	good := http.Header{}
	s.SetHeaders(good, MockKeyID, strings.Repeat("a", 64))
	const maxBody = 16

	res, ok := CheckInboundPreamble("BAD", http.Header{}, strings.NewReader(strings.Repeat("x", 99)), maxBody, lookup)
	if ok || res.Reason != ReasonProviderInvalid {
		t.Fatalf("charset first: got %+v", res)
	}
	res, ok = CheckInboundPreamble("not-registered", http.Header{}, strings.NewReader(strings.Repeat("x", 99)), maxBody, lookup)
	if ok || res.Reason != ReasonBodyTooLarge {
		t.Fatalf("body bound before scheme lookup: got %+v", res)
	}
	res, ok = CheckInboundPreamble("not-registered", http.Header{}, strings.NewReader("{}"), maxBody, lookup)
	if ok || res.Reason != ReasonProviderUnregistered || !res.ProviderIDValid || res.BodyLen != 2 {
		t.Fatalf("an unregistered provider is provider_unregistered BEFORE header checks: got %+v", res)
	}
	res, ok = CheckInboundPreamble("mock-psp", http.Header{}, strings.NewReader("{}"), maxBody, lookup)
	if ok || res.Reason != ReasonSignatureMissing {
		t.Fatalf("registered provider, no headers: got %+v", res)
	}
	res, ok = CheckInboundPreamble("mock-psp", good, strings.NewReader("{}"), maxBody, lookup)
	if !ok || string(res.Body) != "{}" {
		t.Fatalf("well-formed request: got %+v ok=%v", res, ok)
	}
	if res, ok = CheckInboundPreamble("mock-psp", good, strings.NewReader("{}"), maxBody, nil); ok || res.Reason != ReasonProviderUnregistered {
		t.Fatalf("a nil lookup must fail closed as provider_unregistered, got %+v", res)
	}
}

// TestCheckInboundPreamble_UsesTheProvidersOwnScheme proves the preamble
// no longer hard-codes the MOCK headers: a provider whose scheme expects
// other headers passes with ITS headers and fails with the MOCK's.
func TestCheckInboundPreamble_UsesTheProvidersOwnScheme(t *testing.T) {
	vendor := spyScheme{name: "vendor-z-v1", props: realProps(),
		extract: func(in Inbound) (AuthMaterial, Reason, bool) {
			if in.Header.Get("X-Vendor-Sig") == "" {
				return AuthMaterial{}, ReasonSignatureMissing, false
			}
			return NewAuthMaterial("vk-1", nil), "", true
		}}
	lookup := func(string) (VerificationScheme, bool) { return vendor, true }
	vh := http.Header{}
	vh.Set("X-Vendor-Sig", "anything")
	if res, ok := CheckInboundPreamble("vendor-z", vh, strings.NewReader("{}"), 100, lookup); !ok {
		t.Fatalf("vendor headers rejected by the preamble: %+v", res)
	}
	mh := http.Header{}
	PaymentsScheme().SetHeaders(mh, MockKeyID, strings.Repeat("a", 64))
	if res, ok := CheckInboundPreamble("vendor-z", mh, strings.NewReader("{}"), 100, lookup); ok || res.Reason != ReasonSignatureMissing {
		t.Fatalf("MOCK headers must not satisfy a vendor scheme: %+v", res)
	}
}

func TestExtractInbound_PlatformGuarantees(t *testing.T) {
	mk := func(props SchemeProperties, f func(Inbound) (AuthMaterial, Reason, bool)) VerificationScheme {
		return spyScheme{name: "x", props: props, extract: f}
	}
	implicit := realProps()
	implicit.KeySelection = KeyImplicit
	cases := []struct {
		name   string
		scheme VerificationScheme
		want   Reason
		ok     bool
	}{
		{"panic is signature_invalid", mk(realProps(), func(Inbound) (AuthMaterial, Reason, bool) { panic("boom") }), ReasonSignatureInvalid, false},
		{"out-of-contract reason folded", mk(realProps(), func(Inbound) (AuthMaterial, Reason, bool) { return AuthMaterial{}, ReasonTenantUnknown, false }), ReasonSignatureInvalid, false},
		{"missing kept", mk(realProps(), func(Inbound) (AuthMaterial, Reason, bool) { return AuthMaterial{}, ReasonSignatureMissing, false }), ReasonSignatureMissing, false},
		{"C3: KeyFromHeader ok with empty key id is signature_missing", mk(realProps(), func(Inbound) (AuthMaterial, Reason, bool) { return NewAuthMaterial("", nil), "", true }), ReasonSignatureMissing, false},
		{"KeyFromHeader key id outside charset", mk(realProps(), func(Inbound) (AuthMaterial, Reason, bool) { return NewAuthMaterial("Bad Key", nil), "", true }), ReasonSignatureInvalid, false},
		{"KeyImplicit with a key id", mk(implicit, func(Inbound) (AuthMaterial, Reason, bool) { return NewAuthMaterial("k", nil), "", true }), ReasonSignatureInvalid, false},
		{"KeyImplicit without a key id", mk(implicit, func(Inbound) (AuthMaterial, Reason, bool) { return NewAuthMaterial("", nil), "", true }), "", true},
		{"KeyFromHeader good", mk(realProps(), func(Inbound) (AuthMaterial, Reason, bool) { return NewAuthMaterial("k-1", nil), "", true }), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, authErr := ExtractInbound(tc.scheme, Inbound{})
			if tc.ok != (authErr == nil) {
				t.Fatalf("ok=%v, authErr=%v", tc.ok, authErr)
			}
			if authErr != nil && authErr.Reason != tc.want {
				t.Fatalf("reason = %s, want %s", authErr.Reason, tc.want)
			}
		})
	}
}

type countingResolver struct {
	calls int
	cred  Credential
	err   error
}

func (r *countingResolver) Resolve(_ context.Context, _ uuid.UUID, _, _ string) (Credential, error) {
	r.calls++
	return r.cred, r.err
}

// TestResolveCredentials_KeySelectionFromPropertiesOnly is security C3: the
// mode comes from Properties().KeySelection, never from an empty key id.
func TestResolveCredentials_KeySelectionFromPropertiesOnly(t *testing.T) {
	tenant := uuid.New()
	in := Inbound{TenantID: tenant, ProviderID: "vendor-a"}
	good := Credential{TenantID: tenant, ProviderID: "vendor-a", KeyID: "k-1", Secret: make([]byte, 32)}
	fromHeader := spyScheme{name: "x", props: realProps()}
	implicitProps := realProps()
	implicitProps.KeySelection = KeyImplicit
	implicit := spyScheme{name: "x", props: implicitProps}

	r := &countingResolver{cred: good}
	if _, e := ResolveCredentials(context.Background(), fromHeader, r, in, NewAuthMaterial("", nil)); e == nil || e.Reason != ReasonSignatureMissing || r.calls != 0 {
		t.Fatalf("KeyFromHeader + empty key id must be signature_missing with NO resolver call (no multi-key trial), got %v calls=%d", e, r.calls)
	}
	if _, e := ResolveCredentials(context.Background(), implicit, r, in, NewAuthMaterial("", nil)); e == nil || e.Reason != ReasonCredentialUnavailable || r.calls != 0 {
		t.Fatalf("KeyImplicit must fail closed until the W2a resolver exists, got %v calls=%d", e, r.calls)
	}
	if _, e := ResolveCredentials(context.Background(), fromHeader, nil, in, NewAuthMaterial("k-1", nil)); e == nil || e.Reason != ReasonNoResolver || e.KeyID != "k-1" {
		t.Fatalf("nil resolver: got %v", e)
	}
	if _, e := ResolveCredentials(context.Background(), fromHeader, &countingResolver{err: ErrCredentialUnavailable}, in, NewAuthMaterial("k-1", nil)); e == nil || e.Reason != ReasonCredentialUnavailable {
		t.Fatalf("resolver error: got %v", e)
	}
	for name, mutate := range map[string]func(*Credential){
		"other tenant":   func(c *Credential) { c.TenantID = uuid.New() },
		"other provider": func(c *Credential) { c.ProviderID = "vendor-b" },
		"other key id":   func(c *Credential) { c.KeyID = "k-2" },
	} {
		c := good
		mutate(&c)
		if _, e := ResolveCredentials(context.Background(), fromHeader, &countingResolver{cred: c}, in, NewAuthMaterial("k-1", nil)); e == nil || e.Reason != ReasonCredentialUnavailable {
			t.Fatalf("%s: a mis-bound credential must fail closed, got %v", name, e)
		}
	}
	set, e := ResolveCredentials(context.Background(), fromHeader, &countingResolver{cred: good}, in, NewAuthMaterial("k-1", nil))
	if e != nil || set.Active.KeyID != "k-1" || set.Previous != nil {
		t.Fatalf("good resolve: %+v %v", set, e)
	}
}

func TestVerifyInbound_PlatformGuarantees(t *testing.T) {
	tenant := uuid.New()
	in := Inbound{TenantID: tenant, ProviderID: "vendor-a"}
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	active := Credential{TenantID: tenant, ProviderID: "vendor-a", KeyID: "k-new", Secret: make([]byte, 32), Fingerprint: "fp-active"}
	prev := Credential{TenantID: tenant, ProviderID: "vendor-a", KeyID: "k-old", Secret: make([]byte, 32), NotAfter: now.Add(time.Hour)}
	m := NewAuthMaterial("k-new", nil)

	scheme := func(props SchemeProperties, f func(CredentialSet, Inbound, AuthMaterial, time.Time) (string, error)) (spyScheme, *int, *CredentialSet) {
		runs, seen := new(int), new(CredentialSet)
		return spyScheme{name: "x", props: props, verify: f, verifyRuns: runs, lastCreds: seen}, runs, seen
	}
	okActive := func(c CredentialSet, _ Inbound, _ AuthMaterial, _ time.Time) (string, error) {
		return c.Active.KeyID, nil
	}

	t.Run("success returns the active credential", func(t *testing.T) {
		s, _, _ := scheme(realProps(), okActive)
		got, e := VerifyInbound(s, CredentialSet{Active: active}, in, m, now)
		if e != nil || got.KeyID != "k-new" {
			t.Fatalf("got %+v %v", got, e)
		}
	})
	t.Run("KeyFromHeader never receives Previous", func(t *testing.T) {
		s, _, seen := scheme(realProps(), okActive)
		p := prev
		_, _ = VerifyInbound(s, CredentialSet{Active: active, Previous: &p}, in, m, now)
		if seen.Previous != nil {
			t.Fatal("a KeyFromHeader scheme was handed a Previous credential (C4)")
		}
	})
	t.Run("KeyFromHeader header key id must equal the active key id", func(t *testing.T) {
		s, runs, _ := scheme(realProps(), okActive)
		if _, e := VerifyInbound(s, CredentialSet{Active: active}, in, NewAuthMaterial("k-other", nil), now); e == nil || e.Reason != ReasonSignatureInvalid || *runs != 0 {
			t.Fatalf("got %v runs=%d", e, *runs)
		}
	})
	implicitProps := realProps()
	implicitProps.KeySelection = KeyImplicit
	t.Run("KeyImplicit Previous kept only before not_after", func(t *testing.T) {
		for _, tc := range []struct {
			notAfter time.Time
			kept     bool
		}{{now.Add(time.Second), true}, {now, false}, {now.Add(-time.Second), false}, {time.Time{}, false}} {
			s, _, seen := scheme(implicitProps, okActive)
			p := prev
			p.NotAfter = tc.notAfter
			_, _ = VerifyInbound(s, CredentialSet{Active: active, Previous: &p}, in, NewAuthMaterial("", nil), now)
			if (seen.Previous != nil) != tc.kept {
				t.Fatalf("not_after %v: Previous kept=%v, want %v", tc.notAfter, seen.Previous != nil, tc.kept)
			}
		}
	})
	t.Run("KeyImplicit Previous match returns the previous credential", func(t *testing.T) {
		s, _, _ := scheme(implicitProps, func(c CredentialSet, _ Inbound, _ AuthMaterial, _ time.Time) (string, error) {
			return c.Previous.KeyID, nil
		})
		p := prev
		got, e := VerifyInbound(s, CredentialSet{Active: active, Previous: &p}, in, NewAuthMaterial("", nil), now)
		if e != nil || got.KeyID != "k-old" {
			t.Fatalf("got %+v %v", got, e)
		}
	})
	for name, c := range map[string]Credential{
		"short secret":   {TenantID: tenant, ProviderID: "vendor-a", KeyID: "k-new", Secret: make([]byte, MinSecretBytes-1)},
		"empty secret":   {TenantID: tenant, ProviderID: "vendor-a", KeyID: "k-new"},
		"other tenant":   {TenantID: uuid.New(), ProviderID: "vendor-a", KeyID: "k-new", Secret: make([]byte, 32)},
		"other provider": {TenantID: tenant, ProviderID: "vendor-b", KeyID: "k-new", Secret: make([]byte, 32)},
		"empty set":      {},
	} {
		t.Run(name+" never reaches the scheme", func(t *testing.T) {
			s, runs, _ := scheme(realProps(), okActive)
			if _, e := VerifyInbound(s, CredentialSet{Active: c}, in, m, now); e == nil || e.Reason != ReasonSignatureInvalid || *runs != 0 {
				t.Fatalf("got %v runs=%d", e, *runs)
			}
		})
	}
	for name, tc := range map[string]struct {
		err    error
		reason Reason
	}{
		"timestamp window":           {ErrTimestampOutOfWindow, ReasonTimestampOutOfWindow},
		"wrapped timestamp folded":   {fmt.Errorf("x: %w", ErrTimestampOutOfWindow), ReasonSignatureInvalid},
		"signature invalid":          {ErrSignatureInvalid, ReasonSignatureInvalid},
		"any other error folded":     {errors.New("vendor parse error"), ReasonSignatureInvalid},
		"wrapped signature (folded)": {fmt.Errorf("%w: detail", ErrSignatureInvalid), ReasonSignatureInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _ := scheme(realProps(), func(CredentialSet, Inbound, AuthMaterial, time.Time) (string, error) { return "", tc.err })
			_, e := VerifyInbound(s, CredentialSet{Active: active}, in, m, now)
			if e == nil || e.Reason != tc.reason || !errors.Is(e, ErrAuthFailed) {
				t.Fatalf("got %v, want reason %s", e, tc.reason)
			}
			if (e.CredentialFingerprint != "") != (tc.reason == ReasonSignatureInvalid) {
				t.Fatalf("fingerprint must be logged only for signature_invalid, got %q", e.CredentialFingerprint)
			}
		})
	}
	t.Run("panic is signature_invalid", func(t *testing.T) {
		s, _, _ := scheme(realProps(), func(CredentialSet, Inbound, AuthMaterial, time.Time) (string, error) { panic("boom") })
		if _, e := VerifyInbound(s, CredentialSet{Active: active}, in, m, now); e == nil || e.Reason != ReasonSignatureInvalid {
			t.Fatalf("got %v", e)
		}
	})
	t.Run("matched key id outside the set is rejected", func(t *testing.T) {
		for _, matched := range []string{"", "k-unknown", "k-old"} {
			s, _, _ := scheme(realProps(), func(CredentialSet, Inbound, AuthMaterial, time.Time) (string, error) { return matched, nil })
			if _, e := VerifyInbound(s, CredentialSet{Active: active}, in, m, now); e == nil || e.Reason != ReasonSignatureInvalid {
				t.Fatalf("matched %q: got %v", matched, e)
			}
		}
	})
}

// TestReason_ClosedEnum pins the closed Reason enum (security C10: the new
// reason is added to the enum and to the allow-list log tests).
func TestReason_ClosedEnum(t *testing.T) {
	want := []string{"tenant_unknown", "tenant_inactive", "provider_invalid", "provider_unregistered", "provider_not_configured",
		"no_resolver", "credential_unavailable", "signature_missing", "signature_invalid", "key_material", "body_too_large", "timestamp_out_of_window"}
	got := AllReasons()
	if len(got) != len(want) {
		t.Fatalf("AllReasons has %d members, want %d", len(got), len(want))
	}
	seen := map[Reason]bool{}
	for i, r := range got {
		if string(r) != want[i] {
			t.Fatalf("AllReasons[%d] = %q, want %q", i, r, want[i])
		}
		if seen[r] {
			t.Fatalf("duplicate reason %q", r)
		}
		seen[r] = true
	}
}

func TestErrTimestampOutOfWindow_Contract(t *testing.T) {
	if !errors.Is(ErrTimestampOutOfWindow, ErrSignatureInvalid) {
		t.Fatal("ErrTimestampOutOfWindow must satisfy errors.Is(_, ErrSignatureInvalid)")
	}
	if errors.Is(ErrSignatureInvalid, ErrTimestampOutOfWindow) {
		t.Fatal("ErrSignatureInvalid must not be a timestamp-window error")
	}
	if ErrTimestampOutOfWindow == ErrSignatureInvalid {
		t.Fatal("the two sentinels must be distinct values")
	}
}

// TestMockVerificationScheme_WrapsSchemeUnchanged: the MOCK adapter to the
// interface reuses Scheme's bytes and adds only the MinSecretBytes floor.
func TestMockVerificationScheme_WrapsSchemeUnchanged(t *testing.T) {
	for name, s := range allSchemes {
		t.Run(name, func(t *testing.T) {
			v := s.VerificationScheme()
			if got, ok := MockScheme(v); !ok || got != s {
				t.Fatal("MockScheme must return the wrapped Scheme")
			}
			if v.Name() != "platform-mock:"+s.Prefix {
				t.Fatalf("name = %q", v.Name())
			}
			p := v.Properties()
			if !p.Synthetic || p.SignedTimestamp || p.KeySelection != KeyFromHeader || p.Binding != BindingSignedTenant || p.Replay != ReplayIdempotencyOnly {
				t.Fatalf("unexpected MOCK properties %+v", p)
			}
			key := testKey(t)
			tenant := uuid.New()
			in := signedInbound(s, key, tenant, "mock-x", MockKeyID, []byte(`{"a":1}`))
			m, reason, ok := v.Extract(in)
			if !ok || m.KeyID != MockKeyID || m.Private() != in.Header.Get(s.SignatureHeader)[3:] {
				t.Fatalf("extract: %+v %s %v", m, reason, ok)
			}
			matched, err := v.Verify(CredentialSet{Active: credFor(tenant, "mock-x", key)}, in, m, time.Time{})
			if err != nil || matched != MockKeyID {
				t.Fatalf("verify: %q %v", matched, err)
			}
			if _, ok := MockScheme(spyScheme{}); ok {
				t.Fatal("MockScheme must refuse a non-MOCK scheme")
			}
		})
	}
}
