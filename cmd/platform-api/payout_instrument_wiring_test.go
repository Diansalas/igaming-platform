package main

import (
	"context"
	"encoding/base64"
	"sort"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/config"
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
