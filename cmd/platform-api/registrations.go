package main

import (
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/email"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/providerkind"
	"github.com/Diansalas/igaming-platform/internal/sportsbook"
)

// providerBundle is every provider/scanner/storage/resolver/statement-
// source component this binary ever constructs, in ONE place, built ONCE,
// before anything else - db.Connect included - happens. main() (run())
// uses these same instances for every wiring decision below; nothing is
// constructed a second time under a different name. This is what makes
// buildRegistrations(cfg) a genuine completeness statement rather than a
// second, independently-maintained list that could silently drift from
// what main() actually wires (Stage 10.3, MOCK-ADAPTER-PROD-1; security
// condition C13 point 3, "coverage must be all-of-main").
type providerBundle struct {
	Payments        *payments.MockProvider
	Casino          *casino.MockCasinoProvider
	KYC             *kyc.MockKYCProvider // nil unless wiring.KYCWebhookEnabled (ADR 0085 "absent", not merely unwired)
	Sportsbook      *sportsbook.MockSportsbookProvider
	SettlementStmt  sportsbook.MockSettlementStatementSource
	PersonResolver  *identityresolution.MockPersonResolver
	DocumentStorage *kyc.MockDocumentStorageProvider
	MalwareScanner  kyc.MockMalwareScanner
	Email           *email.MockProvider
}

// buildProviderBundle constructs every mock component this binary wires,
// exactly mirroring run()'s own construction (payments/casino provider ids
// and asset lists match main()'s literals) - it does no I/O and touches no
// database, so it is safe to call before db.Connect.
func buildProviderBundle(wiring mockWiring) providerBundle {
	b := providerBundle{
		Payments:        payments.NewMockProvider("mock-payments", "EUR", "USD", "GBP", "BRL", "MXN"),
		Casino:          casino.NewMockCasinoProvider("mock-casino", "EUR", "USD", "GBP", "BRL", "MXN"),
		Sportsbook:      sportsbook.NewMockSportsbookProvider(),
		SettlementStmt:  sportsbook.MockSettlementStatementSource{},
		PersonResolver:  identityresolution.NewMockPersonResolver(),
		DocumentStorage: kyc.NewMockDocumentStorageProvider(),
		MalwareScanner:  kyc.NewMockMalwareScanner(),
		Email:           email.NewMockProvider(),
	}
	if wiring.KYCWebhookEnabled {
		b.KYC = kyc.NewMockKYCProvider()
	}
	return b
}

// buildRegistrations enumerates every component buildProviderBundle
// constructed, as the input to RefuseSyntheticInProduction
// (internal/providerkind). cfg is accepted for symmetry with the design
// (§3's "buildRegistrations(cfg)") and so a future registration that
// legitimately depends on cfg (e.g. only registering a component under a
// specific flag) has somewhere to read it from; today only KYC depends on
// wiring, which is itself derived purely from cfg.
func buildRegistrations(_ config.Config, b providerBundle) []providerkind.Registration {
	regs := []providerkind.Registration{
		{Domain: "payments", Name: "provider", Component: b.Payments},
		{Domain: "casino", Name: "provider", Component: b.Casino},
		{Domain: "sportsbook", Name: "catalogue_provider", Component: b.Sportsbook},
		{Domain: "sportsbook", Name: "settlement_statement_source", Component: b.SettlementStmt},
		{Domain: "identity_resolution", Name: "person_resolver", Component: b.PersonResolver},
		{Domain: "kyc", Name: "document_storage", Component: b.DocumentStorage},
		{Domain: "kyc", Name: "malware_scanner", Component: b.MalwareScanner},
		{Domain: "email", Name: "provider", Component: b.Email},
	}
	if b.KYC != nil {
		regs = append(regs, providerkind.Registration{Domain: "kyc", Name: "provider", Component: b.KYC})
	}
	return regs
}

// refuseSyntheticInProduction is the cmd/platform-api-level wrapper the
// W1b design names directly ("a pure function refuseSyntheticInProduction
// (cfg, wiring) runs right after config load"). It is pure: no I/O, no
// globals. It resolves cfg.GuardEnvironment() - NOT cfg.Environment - so a
// missing APP_ENV is treated identically to APP_ENV=production (security
// condition C13, ruling R8; see config.Config.GuardEnvironment's own doc
// comment), then delegates the actual marker-based decision to
// providerkind.RefuseSyntheticInProduction, which has no knowledge of
// Config at all (kept a dependency-free leaf package deliberately).
func refuseSyntheticInProduction(cfg config.Config, regs []providerkind.Registration) error {
	return providerkind.RefuseSyntheticInProduction(cfg.GuardEnvironment(), regs)
}
