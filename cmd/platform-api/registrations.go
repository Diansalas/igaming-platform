package main

import (
	"context"
	"fmt"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/email"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/providercred"
	"github.com/Diansalas/igaming-platform/internal/providerkind"
	"github.com/Diansalas/igaming-platform/internal/secretstore"
	"github.com/Diansalas/igaming-platform/internal/secretstore/awssm"
	"github.com/Diansalas/igaming-platform/internal/secretstore/devfile"
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
	CasinoStmt      casino.MockStatementSource // Stage 10.3 W3a, CAS-RECON-STMT-1 (MOCK)
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

	// Credentials is the REAL provider-credential subsystem (Stage 10.3
	// W2a, ADR 0093): the handle-table resolver, outbound resolution and
	// the four-eyes lifecycle. Nil unless BOTH a fingerprint key and a
	// permitted secret-store backend are configured (fail closed: real
	// callbacks get no_resolver, the request/approve/apply routes are not
	// mounted). Set by withCredentialSubsystem, never by buildProviderBundle,
	// so the bundle stays a pure function of the mock wiring.
	Credentials *providercred.Subsystem
	// SecretBackends are the constructed secret-store backends, registered
	// with the synthetic guard individually: awssm is ProductionEligible;
	// devfile carries no production-eligibility marker, so the guard
	// refuses it in production even if the environment allow-list were ever
	// bypassed.
	SecretBackends []secretstore.Store
}

// awssmConstructor builds the awssm:// backend. Production uses awssm.New
// (withCredentialSubsystem); the wiring tests pass awssm.NewWithSDKFake,
// which runs the same preflight refusals over an in-process SDK fake (no
// network). Both return the concrete *awssm.Store, which is
// ProductionEligible.
type awssmConstructor func(ctx context.Context, cfg config.Config, region string) (*awssm.Store, error)

// withCredentialSubsystem builds the real credential subsystem from cfg
// and attaches it to b (ADR 0093 §4 wiring; security review §3/§4.1):
//
//   - every scheme in cfg.SecretStoreBackends must pass
//     cfg.ValidateSecretBackendScheme (config.Load already refused
//     otherwise; checked again by secretstore.NewRouter);
//   - devfile:// is constructed by devfile.New, which re-checks the
//     environment itself and refuses a bad root directory;
//   - awssm:// (Stage 10.3 W3b) is constructed by awssm.New ONLY when the
//     operator configured it AND the environment permits it (explicit
//     staging/production). It needs cfg.SecretStoreAWSRegion, and awssm.New
//     re-checks the environment and refuses static credentials and
//     endpoint/CA overrides. If awssm was configured and cannot be built,
//     startup is refused - never silently continued without it. Not
//     configured, it is never constructed;
//   - memory:// can never be configured (test-only, import-restricted).
//
// With no fingerprint key or no backend, b.Credentials stays nil and
// startup still succeeds. It makes no database call. awssm.New makes no
// network call either: it resolves configuration only, and the SDK fetches
// task-role credentials lazily on the first secret read.
func withCredentialSubsystem(ctx context.Context, cfg config.Config, b providerBundle) (providerBundle, error) {
	return withCredentialSubsystemUsing(ctx, cfg, b, awssm.New)
}

// withCredentialSubsystemUsing is withCredentialSubsystem with the awssm
// constructor injected (tests pass awssm.NewWithSDKFake).
func withCredentialSubsystemUsing(ctx context.Context, cfg config.Config, b providerBundle, newAWSSM awssmConstructor) (providerBundle, error) {
	var stores []secretstore.Store
	for _, scheme := range cfg.SecretStoreBackends {
		if err := cfg.ValidateSecretBackendScheme(scheme); err != nil {
			return b, fmt.Errorf("secret store: %w", err)
		}
		switch scheme {
		case config.SecretBackendDevFile:
			st, err := devfile.New(cfg, cfg.SecretStoreDevFileRoot)
			if err != nil {
				return b, fmt.Errorf("secret store: %w", err)
			}
			stores = append(stores, st)
		case config.SecretBackendAWSSecretsManager:
			if err := cfg.ValidateSecretStoreAWSRegion(); err != nil {
				return b, fmt.Errorf("secret store: %w", err)
			}
			if newAWSSM == nil {
				return b, fmt.Errorf("secret store: backend %q has no constructor", scheme)
			}
			st, err := newAWSSM(ctx, cfg, cfg.SecretStoreAWSRegion)
			if err != nil {
				return b, fmt.Errorf("secret store: %w", err)
			}
			stores = append(stores, st)
		default:
			return b, fmt.Errorf("secret store: backend %q cannot be configured", scheme)
		}
	}
	router, err := secretstore.NewRouter(cfg, stores...)
	if err != nil {
		return b, fmt.Errorf("secret store: %w", err)
	}
	sub, err := providercred.New(cfg, router)
	if err != nil {
		return b, fmt.Errorf("provider credentials: %w", err)
	}
	b.Credentials = sub
	b.SecretBackends = stores
	return b, nil
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
		CasinoStmt:      casino.MockStatementSource{},
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
//
// Stage 10.3 W2a (ADR 0093 §4 wiring): the result is the two-way split by
// ADAPTER KIND - synthetic adapters to the MOCK resolver, every other
// adapter to the real handle-table resolver (b.Credentials) - never a
// per-vendor map. Both nil still yields a TRUE nil interface.
func (b providerBundle) paymentsOrchestratorResolver() payments.WebhookCredentialResolver {
	var mock webhookauth.Resolver
	if b.PaymentsWebhookResolver != nil && b.Payments != nil {
		mock = payments.MultiWebhookCredentialResolver{b.Payments.Capabilities().ProviderID: b.PaymentsWebhookResolver}
	}
	return webhookauth.NewKindSplitResolver(b.paymentsAdapters(), mock, b.Credentials.Resolver("payments"))
}

// casinoOrchestratorResolver is paymentsOrchestratorResolver's casino twin
// (Stage 10.2, CAS-WH-TENANT-1, ADR 0091, design §C7).
// Stage 10.3 W2a: the same kind split; the real casino resolver is wired
// only now that revocation is built and its immediacy tested (R9/C14).
func (b providerBundle) casinoOrchestratorResolver() webhookauth.Resolver {
	var mock webhookauth.Resolver
	if b.CasinoWebhookResolver != nil && b.Casino != nil {
		mock = webhookauth.MultiResolver{b.Casino.Capabilities().ProviderID: b.CasinoWebhookResolver}
	}
	return webhookauth.NewKindSplitResolver(b.casinoAdapters(), mock, b.Credentials.Resolver("casino"))
}

// casinoOutboundCredentials is LaunchGame's phase B credential resolver
// (ADR 0095 §15.1/§9.1, PROV-OUTBOUND-CRED-1) - casinoOrchestratorResolver's
// OUTBOUND twin. Stage 4A ships a MOCK casino adapter only (CLAUDE.md's
// scope gate), so this returns the synthetic in-memory resolver whenever
// the mock adapter is wired; a real casino adapter would need the same
// kind-split-by-adapter-identity pattern casinoOrchestratorResolver already
// uses for INBOUND credentials, added when one is actually registered (no
// commercial relationship exists today - CLAUDE.md's provider-abstraction
// rule). b.Credentials.Outbound("casino") is nil-receiver-safe and returns
// nil itself when the real subsystem is not constructed, so a deployment
// with the mock disabled and no real subsystem configured fails every
// launch closed via LaunchGame's own nil-resolver check - never a silent
// fallback.
func (b providerBundle) casinoOutboundCredentials() casino.OutboundCredentialResolver {
	if b.Casino != nil {
		return casino.NewMockOutboundResolver()
	}
	return b.Credentials.Outbound("casino")
}

// kycOrchestratorResolver is the KYC Orchestrator's resolver: the bundle's
// MOCK resolver directly (KYC wires one provider, design §B2), or a TRUE
// nil interface when wiring left it unset.
// Stage 10.3 W2a: the same kind split.
func (b providerBundle) kycOrchestratorResolver() webhookauth.Resolver {
	var mock webhookauth.Resolver
	if b.KYCWebhookResolver != nil && b.KYC != nil {
		mock = b.KYCWebhookResolver
	}
	return webhookauth.NewKindSplitResolver(b.kycAdapters(), mock, b.Credentials.Resolver("kyc"))
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
		{Domain: "casino", Name: "statement_source", Component: b.CasinoStmt},
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
	// The real credential subsystem (production-eligible) and each
	// secret-store backend: awssm is ProductionEligible (W3b); devfile has
	// no eligibility marker, so the guard refuses it in production; memory
	// is synthetic and never configured.
	if b.Credentials != nil {
		regs = append(regs, providerkind.Registration{Domain: "provider_credentials", Name: "subsystem", Component: b.Credentials})
	}
	for _, st := range b.SecretBackends {
		regs = append(regs, providerkind.Registration{Domain: "provider_credentials", Name: "secret_backend:" + st.Scheme(), Component: st})
	}
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
//
// It takes the adapter registries (not the bundle) so a test can hand it a
// non-conforming adapter; run() passes the bundle's own registries.
func validateWebhookSchemes(paymentsAdapters map[string]payments.PaymentProvider, casinoAdapters map[string]casino.CasinoProvider, kycAdapters map[string]kyc.KYCProvider) error {
	if _, err := webhookauth.NewAdapterSchemeSet("payments", paymentsAdapters); err != nil {
		return err
	}
	if _, err := webhookauth.NewAdapterSchemeSet("casino", casinoAdapters); err != nil {
		return err
	}
	if _, err := webhookauth.NewAdapterSchemeSet("kyc", kycAdapters); err != nil {
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
