package main

import (
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/providerkind"
)

// outboundPreconditionRow is the task-registry row this tripwire enforces.
const outboundPreconditionRow = "docs/governance/task-registry.md, Stage 10.3 section, row PROV-OUTBOUND-CRED-1 " +
	"(security gate W2 condition W2A-SEC-1; docs/plans/stage-10.3-planning/09-gate-w2-review-security-w2a.md §3)"

// TestOutboundPrecondition_EveryWiredAdapterIsSynthetic is the W2A-SEC-1
// TRIPWIRE. PROV-OUTBOUND-CRED-1 is PARTIALLY IMPLEMENTED: every
// payments, casino and KYC adapter call (Deposit, Withdraw, QueryStatus,
// Launch, CreateVerification) still runs INSIDE a domain DB transaction, so
// a real adapter would hold DB connections and row locks across vendor
// HTTP latency. The launch-blocking precondition is: no non-synthetic
// payments, casino or KYC adapter may be registered or make an outbound
// call until outbound provider calls run outside any domain DB
// transaction (owners: architect + ledger-finance + the domain
// specialists).
//
// This test fails the moment buildProviderBundle wires any payments,
// casino or KYC adapter that is not a synthetic MOCK. That failure is
// DELIBERATE: do not "fix" it by editing this test. Resolve the
// precondition row first, record that in the registry, and only then
// change this test in the same reviewed change.
func TestOutboundPrecondition_EveryWiredAdapterIsSynthetic(t *testing.T) {
	b := buildProviderBundle(allOnWiring)
	adapters := map[string]any{}
	for id, a := range b.paymentsAdapters() {
		adapters["payments/"+id] = a
	}
	for id, a := range b.casinoAdapters() {
		adapters["casino/"+id] = a
	}
	for id, a := range b.kycAdapters() {
		adapters["kyc/"+id] = a
	}
	// Non-vacuity: every domain contributes at least one adapter.
	for _, domain := range []string{"payments", "casino", "kyc"} {
		found := false
		for name := range adapters {
			if strings.HasPrefix(name, domain+"/") {
				found = true
			}
		}
		if !found {
			t.Fatalf("no %s adapter in buildProviderBundle(allOnWiring); the tripwire would be vacuous", domain)
		}
	}
	for name, a := range adapters {
		if _, ok := a.(providerkind.Synthetic); !ok {
			t.Errorf("adapter %s (%T) is not a synthetic MOCK. A non-synthetic payments/casino/KYC adapter may not be "+
				"registered, or make an outbound call, until outbound provider calls run outside any domain DB "+
				"transaction - LAUNCH-BLOCKING precondition, see %s", name, a, outboundPreconditionRow)
		}
		if _, ok := a.(providerkind.ProductionEligible); ok {
			t.Errorf("adapter %s (%T) is marked ProductionEligible - blocked by the precondition in %s", name, a, outboundPreconditionRow)
		}
	}
}
