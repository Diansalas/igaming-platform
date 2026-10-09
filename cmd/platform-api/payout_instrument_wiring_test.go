package main

import (
	"context"
	"encoding/base64"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
)

// Migration 0123's literal MOCK provider-id set (L-6 / A-11) equals the ids of
// the providerkind.Synthetic payment adapters this binary registers.
func TestPayoutInstrument_MockProviderIDsPinnedToSyntheticAdapters(t *testing.T) {
	b := buildProviderBundle(mockWiring{})
	var ids []string
	for id, a := range b.paymentsAdapters() {
		if payoutinstrument.IsSyntheticComponent(a) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	want := append([]string(nil), payoutinstrument.MockProviderIDs...)
	sort.Strings(want)
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("Synthetic payment adapter ids %v != payoutinstrument.MockProviderIDs %v (and migration 0123's literal)", ids, want)
	}
}

// The MOCK verifier is registered with the synthetic guard, so production
// refuses it.
func TestPayoutInstrument_MockVerifierRegisteredWithSyntheticGuard(t *testing.T) {
	b := buildProviderBundle(mockWiring{})
	cfg := config.Config{Environment: "production", EnvironmentExplicit: true}
	err := refuseSyntheticInProduction(cfg, buildRegistrations(cfg, b))
	if err == nil || !strings.Contains(err.Error(), "payout_instrument/verifier") {
		t.Fatalf("production must refuse the MOCK payout verifier by name, got %v", err)
	}
}

func TestPayoutInstrument_ServiceWiringGate(t *testing.T) {
	b := buildProviderBundle(mockWiring{})
	// Development, synthetic only, no keys: the feature is simply unavailable.
	dev := config.Config{Environment: "development", EnvironmentExplicit: true}
	svc, err := buildPayoutInstrumentService(dev, b)
	if err != nil || svc != nil {
		t.Fatalf("dev without keys: svc=%v err=%v", svc, err)
	}
	// A missing APP_ENV counts as production: keys are required.
	missing := config.Config{Environment: "development", EnvironmentExplicit: false}
	if _, err := buildPayoutInstrumentService(missing, b); err == nil {
		t.Fatal("a missing APP_ENV with no keys must refuse")
	}
	// With keys the service is built.
	withKeys := dev
	withKeys.PayoutInstrumentKeys = config.NewSecretValue("m1:" + b64(32, 1))
	withKeys.PayoutInstrumentActiveKID = "m1"
	withKeys.PayoutInstrumentFPKeys = config.NewSecretValue("f1:" + b64(32, 2))
	withKeys.PayoutInstrumentFPActiveKID = "f1"
	svc, err = buildPayoutInstrumentService(withKeys, b)
	if err != nil || svc == nil {
		t.Fatalf("with keys: svc=%v err=%v", svc, err)
	}
	_ = context.Background()
}

func b64(n, seed int) string {
	raw := make([]byte, n)
	for i := range raw {
		raw[i] = byte(i*7 + seed*31)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// echoWrapped is a NON-Synthetic payment adapter (it embeds the interface, so the MOCK's Synthetic marker is not promoted)
// whose manifest destination-echo declaration the test controls.
type echoWrapped struct {
	payments.PaymentProvider
	sem payoutinstrument.DestinationEchoSemantics
}

func (e echoWrapped) Capabilities() payments.AdapterCapability {
	c := e.PaymentProvider.Capabilities()
	c.Manifest.DestinationEchoSemantics = e.sem
	return c
}

// ADR 0111 24: the binary reads each adapter's declaration verbatim and the startup gate refuses an unset one for a
// non-Synthetic payout adapter; an explicit Supported or Unsupported starts (Unsupported with the visible marker).
func TestPayoutInstrument_StartupRefusesUnsetEchoDeclaration(t *testing.T) {
	cfg := config.Config{Environment: "development", EnvironmentExplicit: true}
	cfg.PayoutInstrumentKeys = config.NewSecretValue("m1:" + b64(32, 1))
	cfg.PayoutInstrumentActiveKID = "m1"
	cfg.PayoutInstrumentFPKeys = config.NewSecretValue("f1:" + b64(32, 2))
	cfg.PayoutInstrumentFPActiveKID = "f1"

	// The shipped MOCK declares explicitly.
	shipped := buildProviderBundle(mockWiring{})
	_, decls := payoutAdapterRegistrations(shipped)
	if len(decls) != 1 || !decls[0].PayoutCapable || !decls[0].Semantics.Valid() {
		t.Fatalf("shipped declarations = %+v", decls)
	}

	for _, c := range []struct {
		sem     payoutinstrument.DestinationEchoSemantics
		wantErr bool
	}{
		{payoutinstrument.DestinationEchoUnset, true},
		{payoutinstrument.DestinationEchoSemantics(42), true},
		{payoutinstrument.DestinationEchoSupported, false},
		{payoutinstrument.DestinationEchoUnsupported, false},
	} {
		w := echoWrapped{PaymentProvider: shipped.Payments, sem: c.sem}
		adapters, decls := payoutAdapterDeclarations(map[string]payments.PaymentProvider{"x": w})
		keys, kerr := payoutInstrumentKeys(cfg)
		if kerr != nil {
			t.Fatal(kerr)
		}
		err := payoutinstrument.VerifyStartup(cfg.GuardEnvironment(), keys, payoutinstrument.Registrations{PaymentAdapters: adapters, PayoutEchoDeclarations: decls})
		if (err != nil) != c.wantErr {
			t.Fatalf("non-Synthetic adapter declaring %s: err = %v, wantErr %v", c.sem, err, c.wantErr)
		}
		if c.wantErr && !errors.Is(err, payoutinstrument.ErrStartupGate) {
			t.Fatalf("declaring %s: not a startup-gate refusal: %v", c.sem, err)
		}
	}
}
