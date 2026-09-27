// RV-PRH-I1 kill-switch security review, finding M5 (§16.2 item 15 does
// not scan what production wires): this test scans the objects
// buildProviderBundle ACTUALLY constructs - the same providerBundle,
// paymentsAdapters()/casinoAdapters()/kycAdapters() registries, and outbound
// resolvers main() hands to each domain orchestrator - not a skeletal mock
// each domain package's own test builds separately. A real adapter
// registered in registrations.go without a corresponding edit here is
// scanned automatically, because this walks whatever buildProviderBundle
// returns for allOnWiring (registrations_test.go), not a hand-maintained
// list of adapter ids.
package main

import (
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/testsupport/credentialscan"
)

func TestCredentialReflection_ProductionWiringHoldsNoCredential(t *testing.T) {
	b := buildProviderBundle(allOnWiring)

	// Scoped to payments/casino/KYC (this ADR's own scope, per the security
	// review's "Out of scope" note) rather than the whole providerBundle -
	// b also carries sportsbook/email/etc. fields with their own,
	// unrelated legitimate func fields (e.g. a test-clock hook) that would
	// otherwise need allow-listing here for a domain this review never
	// covered.
	targets := map[string]any{
		"paymentsAdapters":        b.paymentsAdapters(),
		"casinoAdapters":          b.casinoAdapters(),
		"kycAdapters":             b.kycAdapters(),
		"PaymentsWebhookResolver": b.PaymentsWebhookResolver,
		"CasinoWebhookResolver":   b.CasinoWebhookResolver,
		"CasinoOutboundResolver":  b.CasinoOutboundResolver,
		"KYCWebhookResolver":      b.KYCWebhookResolver,
		"KYCOutboundResolver":     b.KYCOutboundResolver,
		"PaymentsStmt":            b.PaymentsStmt,
		// PaymentsOutboundResolver (PROV-OUTBOUND-CRED-1, phase 2
		// orchestrator wiring) - payments' own OUTBOUND-credential MOCK,
		// CasinoOutboundResolver's twin.
		"PaymentsOutboundResolver": b.PaymentsOutboundResolver,
		// The REAL kind-split resolver InitiateDepositAttempt/
		// DispatchWithdraw actually receive - not merely the bundle field
		// above, so a future change to paymentsOutboundCredentials()'s own
		// composition (e.g. wrapping the real b.Credentials.Outbound
		// resolver in something that accidentally captures a secret) is
		// scanned too, not just the MOCK half.
		"paymentsOutboundCredentials()": b.paymentsOutboundCredentials(),
	}
	for name, v := range targets {
		if v == nil {
			continue
		}
		if violations := credentialscan.Scan(v); len(violations) > 0 {
			t.Errorf("%s (production wiring, buildProviderBundle(allOnWiring)) holds a credential/secret-shaped field:\n%s",
				name, strings.Join(violations, "\n"))
		}
	}
}
