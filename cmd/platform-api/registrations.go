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
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
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

	// The MOCK webhook credential resolvers (Stage 10.3 gate-W1 fix round,
	// security S-4 / code review #2). They are built HERE - not in
	// wiring.go or main.go - so buildRegistrations hands them to the
	// synthetic guard like every other component. Each is a true nil
	// interface unless mockProviderWiring (GuardEnvironment-aware) enables
	// it; the orchestrator-facing composition is the *OrchestratorResolver
	// methods below.
	PaymentsWebhookResolver payments.WebhookCredentialResolver
	CasinoWebhookResolver   webhookauth.Resolver
	KYCWebhookResolver      webhookauth.Resolver
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
	if wiring.PaymentsWebhookResolver {
		b.PaymentsWebhookResolver = payments.NewMockWebhookCredentials(b.Payments)
	}
	if wiring.CasinoWebhookResolver {
		b.CasinoWebhookResolver = casino.NewMockWebhookCredentials(b.Casino)
	}
	if wiring.KYCWebhookEnabled {
		b.KYC = kyc.NewMockKYCProvider()
		b.KYCWebhookResolver = kyc.NewMockWebhookCredentials(b.KYC)
	}
	return b
}

// paymentsAdapters, casinoAdapters and kycAdapters are the adapter
// registries main() hands to each domain orchestrator, keyed by each
// adapter's own provider id. They hold ONLY bundle instances.
func (b providerBundle) paymentsAdapters() map[string]payments.PaymentProvider {
	return map[string]payments.PaymentProvider{b.Payments.Capabilities().ProviderID: b.Payments}
}

func (b providerBundle) casinoAdapters() map[string]casino.CasinoProvider {
	return map[string]casino.CasinoProvider{b.Casino.Capabilities().ProviderID: b.Casino}
}

// kycAdapters is empty when KYC is not wired (b.KYC == nil).
func (b providerBundle) kycAdapters() map[string]kyc.KYCProvider {
	if b.KYC == nil {
		return map[string]kyc.KYCProvider{}
	}
	return map[string]kyc.KYCProvider{b.KYC.ID(): b.KYC}
}

// paymentsOrchestratorResolver is the payments Orchestrator's single
// injected webhook credential resolver: the bundle's MOCK resolver bound
// to the bundle's mock adapter id, or a TRUE nil interface (never a typed
// nil or an empty map) when wiring left it unset, so the Orchestrator's
// own nil-resolver branch fails every callback closed as ReasonNoResolver.
func (b providerBundle) paymentsOrchestratorResolver() payments.WebhookCredentialResolver {
	if b.PaymentsWebhookResolver == nil || b.Payments == nil {
		return nil
	}
	return payments.MultiWebhookCredentialResolver{b.Payments.Capabilities().ProviderID: b.PaymentsWebhookResolver}
}

// casinoOrchestratorResolver is paymentsOrchestratorResolver's casino twin
// (Stage 10.2, CAS-WH-TENANT-1, ADR 0091, design §C7).
func (b providerBundle) casinoOrchestratorResolver() webhookauth.Resolver {
	if b.CasinoWebhookResolver == nil || b.Casino == nil {
		return nil
	}
	return webhookauth.MultiResolver{b.Casino.Capabilities().ProviderID: b.CasinoWebhookResolver}
}

// kycOrchestratorResolver is the KYC Orchestrator's resolver: the bundle's
// MOCK resolver directly (KYC wires one provider, design §B2), or a TRUE
// nil interface when wiring left it unset.
func (b providerBundle) kycOrchestratorResolver() webhookauth.Resolver {
	if b.KYCWebhookResolver == nil || b.KYC == nil {
		return nil
	}
	return b.KYCWebhookResolver
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
	// The MOCK webhook credential resolvers (security S-4 point 1). A nil
	// one (wiring off) is skipped by the guard.
	regs = append(regs,
		providerkind.Registration{Domain: "payments", Name: "webhook_resolver", Component: b.PaymentsWebhookResolver},
		providerkind.Registration{Domain: "casino", Name: "webhook_resolver", Component: b.CasinoWebhookResolver},
		providerkind.Registration{Domain: "kyc", Name: "webhook_resolver", Component: b.KYCWebhookResolver},
	)
	// Every adapter's WebhookScheme() (security S-1 / code review #6): the
	// platform MOCK scheme is itself a synthetic component, so the guard
	// refuses it in production even on an adapter that claims
	// ProductionEligible.
	for id, a := range b.paymentsAdapters() {
		regs = append(regs, providerkind.Registration{Domain: "payments", Name: "webhook_scheme:" + id, Component: a.WebhookScheme()})
	}
	for id, a := range b.casinoAdapters() {
		regs = append(regs, providerkind.Registration{Domain: "casino", Name: "webhook_scheme:" + id, Component: a.WebhookScheme()})
	}
	for id, a := range b.kycAdapters() {
		regs = append(regs, providerkind.Registration{Domain: "kyc", Name: "webhook_scheme:" + id, Component: a.WebhookScheme()})
	}
	return regs
}

// validateWebhookSchemes runs every domain's registration-time scheme
// validation (webhookauth.NewAdapterSchemeSet: a permitted declaration;
// Synthetic only for the domain's own canonical MOCK, and only from an
// adapter that is itself a synthetic component - security S-1) over the
// bundle's adapters, BEFORE db.Connect, returning an error rather than
// panicking (security I-1). The orchestrator constructors re-run the same
// validation; this pre-DB pass is what covers every domain from one place.
func validateWebhookSchemes(b providerBundle) error {
	if _, err := webhookauth.NewAdapterSchemeSet("payments", b.paymentsAdapters()); err != nil {
		return err
	}
	if _, err := webhookauth.NewAdapterSchemeSet("casino", b.casinoAdapters()); err != nil {
		return err
	}
	if _, err := webhookauth.NewAdapterSchemeSet("kyc", b.kycAdapters()); err != nil {
		return err
	}
	return nil
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
