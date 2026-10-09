package payoutinstrument

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/config"
)

// realAdapter / syntheticAdapter model payment adapters by their Go type
// marker only (the tiering predicate never looks at provider ids).
type realAdapter struct{}
type syntheticAdapter struct{}

func (syntheticAdapter) SyntheticComponent() {}

type realVerifier struct{ src VerificationSource }

func (realVerifier) ID() string                           { return "real-verifier" }
func (v realVerifier) DeclaredSource() VerificationSource { return v.src }
func (realVerifier) Supports(string, string) bool         { return true }
func (realVerifier) Verify(context.Context, VerifyRequest) (VerifyResult, error) {
	return VerifyResult{}, nil
}

func TestTieringMatrix(t *testing.T) {
	good := TierInput{Bound: true, Source: SourcePSPAccount, KindNonSyntheticEnabled: true, SealsValid: true, VerifierRegisteredNonSynthetic: true}
	mut := func(f func(*TierInput)) TierInput { c := good; f(&c); return c }
	// Non-Synthetic adapter: every condition is required.
	if err := CheckTier(realAdapter{}, good); err != nil {
		t.Fatalf("all conditions met: %v", err)
	}
	for name, in := range map[string]TierInput{
		"unbound (legacy NULL binding)": mut(func(i *TierInput) { i.Bound = false }),
		"synthetic source":              mut(func(i *TierInput) { i.Source = SourceSynthetic }),
		"unknown source":                mut(func(i *TierInput) { i.Source = "bogus" }),
		"kind not enabled":              mut(func(i *TierInput) { i.KindNonSyntheticEnabled = false }),
		"seals invalid":                 mut(func(i *TierInput) { i.SealsValid = false }),
		"verifier not registered":       mut(func(i *TierInput) { i.VerifierRegisteredNonSynthetic = false }),
	} {
		if err := CheckTier(realAdapter{}, in); !errors.Is(err, ErrTierRefused) {
			t.Errorf("non-Synthetic adapter + %s must be refused", name)
		}
	}
	if err := CheckTier(nil, good); !errors.Is(err, ErrTierRefused) {
		t.Error("a nil adapter is not Synthetic: refused")
	}
	// A Synthetic adapter accepts any source and a legacy NULL binding.
	for _, in := range []TierInput{good, mut(func(i *TierInput) { i.Source = SourceSynthetic }), mut(func(i *TierInput) { i.Bound = false })} {
		if err := CheckTier(syntheticAdapter{}, in); err != nil {
			t.Errorf("Synthetic adapter must accept: %v", err)
		}
	}
}

func TestForcedSourceFromMarker(t *testing.T) {
	// A Synthetic verifier can only produce `synthetic`, whatever it declares.
	got, err := ForcedSource(NewMockVerifier())
	if err != nil || got != SourceSynthetic {
		t.Fatalf("mock verifier source = %v %v", got, err)
	}
	// A non-Synthetic verifier declaring synthetic (or junk) is refused.
	for _, s := range []VerificationSource{SourceSynthetic, "", "bogus"} {
		if _, err := ForcedSource(realVerifier{src: s}); !errors.Is(err, ErrVerifierSourceRefused) {
			t.Errorf("real verifier declaring %q must be refused", s)
		}
	}
	for _, s := range []VerificationSource{SourcePSPAccount, SourceKYCVendor, SourceCustodian} {
		if got, err := ForcedSource(realVerifier{src: s}); err != nil || got != s {
			t.Errorf("real verifier %q: %v %v", s, got, err)
		}
	}
	if OwnershipFor(SourceSynthetic) != OwnershipSynthetic || OwnershipFor(SourcePSPAccount) != OwnershipVerified {
		t.Error("ownership assertion must follow the source")
	}
}

func TestStartupGateMatrix(t *testing.T) {
	keys := newTestKeys(t)
	mock := NewMockVerifier()
	cases := []struct {
		name    string
		env     string
		keys    *Keys
		regs    Registrations
		wantErr bool
	}{
		{"production, no keys, synthetic only", "production", nil, Registrations{PaymentAdapters: []any{syntheticAdapter{}}, Verifiers: []PayoutInstrumentVerifier{mock}}, true},
		{"production, keys", "production", keys, Registrations{Verifiers: []PayoutInstrumentVerifier{mock}}, false},
		{"development, no keys, synthetic only", "development", nil, Registrations{PaymentAdapters: []any{syntheticAdapter{}}, Verifiers: []PayoutInstrumentVerifier{mock}}, false},
		{"development, no keys, real payout adapter", "development", nil, Registrations{PaymentAdapters: []any{realAdapter{}}}, true},
		{"L-8: real payout adapter refused even WITH keys (B13-B not landed)", "staging", keys, Registrations{PaymentAdapters: []any{realAdapter{}}}, true},
		{"L-8: real payout adapter refused in production with keys", "production", keys, Registrations{PaymentAdapters: []any{syntheticAdapter{}, realAdapter{}}}, true},
		{"staging, no keys, real verifier", "staging", nil, Registrations{Verifiers: []PayoutInstrumentVerifier{realVerifier{src: SourcePSPAccount}}}, true},
		{"development, keys, real VERIFIER only", "development", keys, Registrations{Verifiers: []PayoutInstrumentVerifier{realVerifier{src: SourcePSPAccount}}}, false},
		{"staging, no keys, nothing registered", "staging", nil, Registrations{}, false},
		{"nil adapters ignored", "development", nil, Registrations{PaymentAdapters: []any{nil}}, false},
	}
	for _, c := range cases {
		err := VerifyStartup(c.env, c.keys, c.regs)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", c.name, err, c.wantErr)
		}
		if err != nil && !errors.Is(err, ErrStartupGate) {
			t.Errorf("%s: not an ErrStartupGate: %v", c.name, err)
		}
	}
}

// A missing APP_ENV counts as production: the gate keys on GuardEnvironment().
func TestStartupGateUsesGuardEnvironment(t *testing.T) {
	missing := config.Config{Environment: "development", EnvironmentExplicit: false}
	if err := VerifyStartup(missing.GuardEnvironment(), nil, Registrations{}); err == nil {
		t.Fatal("a missing APP_ENV (guard environment production) with no keys must refuse")
	}
	explicit := config.Config{Environment: "development", EnvironmentExplicit: true}
	if err := VerifyStartup(explicit.GuardEnvironment(), nil, Registrations{}); err != nil {
		t.Fatalf("explicit development with nothing registered must pass: %v", err)
	}
}

func TestLegacyBindingStartupCheck(t *testing.T) {
	ok := func(context.Context, []string) (int64, error) { return 0, nil }
	bad := func(context.Context, []string) (int64, error) { return 2, nil }
	boom := func(context.Context, []string) (int64, error) { return 0, errors.New("db down") }
	if err := VerifyLegacyBindings(context.Background(), ok); err != nil {
		t.Fatal(err)
	}
	if err := VerifyLegacyBindings(context.Background(), bad); !errors.Is(err, ErrStartupGate) {
		t.Fatal("legacy unbound non-MOCK withdrawals must refuse")
	}
	if err := VerifyLegacyBindings(context.Background(), boom); !errors.Is(err, ErrStartupGate) {
		t.Fatal("a check failure must fail closed")
	}
	var seen []string
	_ = VerifyLegacyBindings(context.Background(), func(_ context.Context, ids []string) (int64, error) { seen = ids; return 0, nil })
	if len(seen) != 1 || seen[0] != "mock-payments" {
		t.Fatalf("mock ids = %v", seen)
	}
}

func TestMockVerifier(t *testing.T) {
	m := NewMockVerifier()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return now }
	res, err := m.Verify(context.Background(), VerifyRequest{Kind: KindBankAccount, Detail: NewSecretDetail([]byte(`{}`))})
	if err != nil || !res.Verified || !res.SourceExpiry.After(now) {
		t.Fatalf("mock verify: %+v %v", res, err)
	}
	rej, _ := m.Verify(context.Background(), VerifyRequest{Kind: KindSyntheticTest, Detail: NewSecretDetail([]byte(`{"label":"reject"}`))})
	if rej.Verified {
		t.Fatal("label reject must be rejected")
	}
	if !IsSyntheticComponent(m) || IsSyntheticComponent(realVerifier{}) || IsSyntheticComponent(realAdapter{}) {
		t.Fatal("marker detection")
	}
}

func TestPayoutDestinationRedaction(t *testing.T) {
	iban := "GB82WEST12345698765432"
	d := PayoutDestination{InstrumentID: uuid.New(), Kind: KindBankAccount, Detail: json.RawMessage(`{"iban":"` + iban + `"}`), FingerprintKid: "f1"}
	rendered := []string{
		d.String(), d.GoString(), fmt.Sprintf("%v", d), fmt.Sprintf("%+v", d), fmt.Sprintf("%#v", d), fmt.Sprintf("%s", d),
		fmt.Sprintf("%q", d), fmt.Sprintf("%x", d), fmt.Sprintf("%v", &d), fmt.Sprint(d), fmt.Sprintf("%d", d),
		fmt.Sprintf("%v", []PayoutDestination{d}), fmt.Sprintf("%+v", map[string]PayoutDestination{"k": d}),
		fmt.Sprintf("%v", struct{ D PayoutDestination }{d}),
	}
	jb, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	rendered = append(rendered, string(jb))
	for _, s := range rendered {
		if strings.Contains(s, iban) || strings.Contains(s, "GB82") || strings.Contains(s, "WEST") {
			t.Errorf("PayoutDestination leaked Detail: %q", s)
		}
	}
	// slog (text and JSON handlers).
	for _, mk := range []func(*bytes.Buffer) slog.Handler{
		func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
		func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
	} {
		var buf bytes.Buffer
		lg := slog.New(mk(&buf))
		lg.Info("payout", "dest", d, slog.Any("d2", &d), slog.Group("g", slog.Any("x", d)))
		if strings.Contains(buf.String(), iban) || strings.Contains(buf.String(), "GB82") {
			t.Errorf("slog leaked Detail: %s", buf.String())
		}
		if !strings.Contains(buf.String(), "REDACTED") {
			t.Errorf("slog output should show the redaction marker: %s", buf.String())
		}
	}
	// Other secret-bearing types.
	sd := NewSecretDetail([]byte(`{"iban":"` + iban + `"}`))
	inst := Instrument{DetailCiphertext: []byte("ciphertextbytes"), Fingerprint: strings.Repeat("a", 64), Kind: KindBankAccount}
	snap := Snapshot{Fingerprint: strings.Repeat("a", 64)}
	for _, s := range []string{fmt.Sprintf("%v", sd), fmt.Sprintf("%+v", sd), fmt.Sprintf("%#v", sd), fmt.Sprintf("%v", inst),
		fmt.Sprintf("%+v", inst), fmt.Sprintf("%#v", inst), fmt.Sprintf("%+v", snap), fmt.Sprintf("%#v", snap)} {
		if strings.Contains(s, iban) || strings.Contains(s, strings.Repeat("a", 64)) || strings.Contains(s, "ciphertextbytes") {
			t.Errorf("leak: %q", s)
		}
	}
	if b, _ := json.Marshal(sd); strings.Contains(string(b), "iban") {
		t.Error("SecretDetail json leak")
	}
}

// The destination type has NO Fingerprint and NO VerificationSource field, and
// the fingerprinter exposes only Echo.
func TestDestinationTypeShape(t *testing.T) {
	var d PayoutDestination
	b, _ := json.Marshal(struct{ P PayoutDestination }{d})
	if strings.Contains(strings.ToLower(string(b)), "fingerprint") || strings.Contains(strings.ToLower(string(b)), "source") {
		t.Fatal("PayoutDestination must not carry a fingerprint or a verification source")
	}
	keys := newTestKeys(t, "f1", "f2")
	tid := uuid.New()
	fp := keys.FingerprinterFor(tid, DefaultKinds())
	detail := json.RawMessage(`{"iban":"GB82 WEST 1234 5698 7654 32"}`)
	e1, err := fp.Echo("f1", KindBankAccount, detail)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := DefaultKinds().Spec(KindBankAccount)
	n, _ := spec.Normalize(detail)
	want, _ := keys.Fingerprint("f1", tid, KindBankAccount, n.FingerprintInput)
	if e1 != want {
		t.Fatal("echo must equal the registration fingerprint under the same kid")
	}
	e2, _ := fp.Echo("f2", KindBankAccount, detail)
	if e1 == e2 {
		t.Fatal("echo under a different kid differs")
	}
	if _, err := fp.Echo("nope", KindBankAccount, detail); err == nil {
		t.Fatal("unknown kid must error (counts as mismatch)")
	}
	if _, err := fp.Echo("f1", KindBankAccount, json.RawMessage(`{"iban":"bad"}`)); err == nil {
		t.Fatal("an invalid vendor destination must error")
	}
	if s := fmt.Sprintf("%v %+v %#v", fp, fp, fp); strings.Contains(s, "keys:") {
		t.Fatalf("fingerprinter rendering: %s", s)
	}
	// A different tenant gives a different echo.
	other, _ := keys.FingerprinterFor(uuid.New(), DefaultKinds()).Echo("f1", KindBankAccount, detail)
	if other == e1 {
		t.Fatal("echo is tenant-bound")
	}
}
