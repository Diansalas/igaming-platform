package main

import (
	"context"
	"fmt"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/db"
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
	Payments       *payments.MockProvider
	Casino         *casino.MockCasinoProvider
	KYC            *kyc.MockKYCProvider // nil unless wiring.KYCWebhookEnabled (ADR 0085 "absent", not merely unwired)
	Sportsbook     *sportsbook.MockSportsbookProvider
	SettlementStmt sportsbook.MockSettlementStatementSource
	CasinoStmt     casino.MockStatementSource // Stage 10.3 W3a, CAS-RECON-STMT-1 (MOCK)
	// PaymentsStmt is the payment_statement stream's source (PRH-I5, ADR
	// 0095 §12.4; MOCK): the Payments MockProvider's own records, fetched
	// through the provider-call gate with the MOCK outbound credential.
	PaymentsStmt    *payments.MockStatementSource
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

	// CasinoOutboundResolver is casino's own OUTBOUND-credential MOCK
	// (ADR 0095 §15.1, PROV-OUTBOUND-CRED-1; security review RV-PRH-I2
	// C3) - built HERE, exactly like the inbound MOCK resolvers above, so
	// buildRegistrations can hand it to the synthetic guard as its own
	// component rather than it existing only as an anonymous value inside
	// casinoOutboundCredentials(). A true nil interface unless
	// wiring.CasinoOutboundResolver enables it.
	CasinoOutboundResolver casino.OutboundCredentialResolver

	// KYCOutboundResolver is KYC's own OUTBOUND-credential MOCK (ADR 0095
	// §15.2/§15.3, PROV-OUTBOUND-CRED-1) - CasinoOutboundResolver's KYC
	// twin, built HERE for the same reason. A true nil interface unless
	// wiring.KYCOutboundResolver enables it.
	KYCOutboundResolver kyc.OutboundCredentialResolver

	// PaymentsOutboundResolver is payments' own OUTBOUND-credential MOCK
	// (ADR 0095 §9.1/§11, PROV-OUTBOUND-CRED-1, phase 2 orchestrator
	// wiring) - CasinoOutboundResolver's payments twin, built HERE for the
	// same reason. A true nil interface unless
	// wiring.PaymentsOutboundResolver enables it.
	PaymentsOutboundResolver payments.OutboundCredentialResolver

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
	// PROV-OUTBOUND-CRED-1 phase 2 code review C2: this is the one
	// DELIBERATE hard-wired MockCredentialResolver{} in this file, never
	// the kind-split resolver - the reconciliation MOCK statement source
	// only ever fetches the MockProvider's OWN in-process records
	// (payments.MockStatementSourceLabel's own doc comment), so it must
	// never resolve a real credential regardless of what real adapters are
	// registered alongside it. If this is ever rewired to
	// b.paymentsOutboundCredentials() (the kind split) for a real payments
	// adapter, that adapter's provider id is not synthetic, so the split
	// would route it to the REAL resolver - reaching an actual vendor
	// credential-store call, and a real HTTP fetch, from what is
	// documented and audited as a MOCK-only source. Pinned, not merely a
	// comment: internal/payments/mock_statement_source.go's own Fetch
	// passes gate.go's callProvider a nil pool for this exact reason
	// (s.resolver is always MockCredentialResolver, which ignores it).
	b.PaymentsStmt = payments.NewMockStatementSource(b.Payments, payments.MockCredentialResolver{})
	if wiring.PaymentsWebhookResolver {
		b.PaymentsWebhookResolver = payments.NewMockWebhookCredentials(b.Payments)
	}
	if wiring.CasinoWebhookResolver {
		b.CasinoWebhookResolver = casino.NewMockWebhookCredentials(b.Casino)
	}
	if wiring.CasinoOutboundResolver {
		b.CasinoOutboundResolver = casino.NewMockOutboundResolver()
	}
	if wiring.KYCWebhookEnabled {
		b.KYC = kyc.NewMockKYCProvider()
		b.KYCWebhookResolver = kyc.NewMockWebhookCredentials(b.KYC)
	}
	if wiring.KYCOutboundResolver {
		b.KYCOutboundResolver = kyc.NewMockOutboundResolver()
	}
	if wiring.PaymentsOutboundResolver {
		b.PaymentsOutboundResolver = payments.MockCredentialResolver{}
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
// OUTBOUND twin. Security review RV-PRH-I2 C3: the choice is keyed on the
// ADAPTER's own kind (casino.NewOutboundKindSplitResolver), exactly like
// casinoOrchestratorResolver already does for INBOUND credentials - never
// on "is any mock wired anywhere in this process". A real casino adapter
// registered alongside the mock (e.g. in staging) therefore reaches the
// real b.Credentials.Outbound("casino") resolver, not the synthetic one;
// in production the mock adapter itself is refused at boot regardless
// (MOCK-ADAPTER-PROD-1), so this is defense in depth, not the only guard.
// b.Credentials.Outbound("casino") is nil-receiver-safe, so a deployment
// with the mock disabled and no real subsystem configured still fails
// every launch closed via LaunchGame's own nil-resolver check.
func (b providerBundle) casinoOutboundCredentials() casino.OutboundCredentialResolver {
	var mock casino.OutboundCredentialResolver
	if b.CasinoOutboundResolver != nil && b.Casino != nil {
		mock = b.CasinoOutboundResolver
	}
	// b.Credentials.Outbound returns *providercred.OutboundResolver, a
	// CONCRETE type, unlike Resolver's identical-looking method (which
	// already returns the webhookauth.Resolver INTERFACE, so a nil there
	// is already a true nil interface). Converting a nil *OutboundResolver
	// straight into the OutboundCredentialResolver interface parameter
	// below would produce the classic non-nil-interface-wrapping-a-nil-
	// pointer value, which NewOutboundKindSplitResolver's own `mock == nil
	// && real == nil` check would then never see as nil. This explicit
	// concrete-pointer nil check is what keeps that check meaningful.
	var real casino.OutboundCredentialResolver
	if or := b.Credentials.Outbound("casino"); or != nil {
		real = or
	}
	return casino.NewOutboundKindSplitResolver(b.casinoAdapters(), mock, real)
}

// paymentsOutboundCredentials is InitiateDepositAttempt/DispatchWithdraw's
// phase B credential resolver (ADR 0095 §9.1/§11, PROV-OUTBOUND-CRED-1,
// phase 2 orchestrator wiring) - casinoOutboundCredentials' payments twin,
// mirroring it exactly (including its kind-split discipline and its
// nil-concrete-pointer-to-true-nil-interface conversion). The choice is
// keyed on the ADAPTER's own kind (payments.NewOutboundKindSplitResolver),
// never on "is any mock wired anywhere in this process" - a real payments
// adapter registered alongside the mock would reach the real
// b.Credentials.Outbound("payments") resolver, not the synthetic one; in
// production the mock adapter itself is refused at boot regardless
// (MOCK-ADAPTER-PROD-1), so this is defense in depth, not the only guard.
// No commercial PSP relationship exists today (CLAUDE.md's provider-
// abstraction rule), so only the MOCK path is reachable in this
// deployment - the real path is wired, not fabricated.
func (b providerBundle) paymentsOutboundCredentials() payments.OutboundCredentialResolver {
	var mock payments.OutboundCredentialResolver
	if b.PaymentsOutboundResolver != nil && b.Payments != nil {
		mock = b.PaymentsOutboundResolver
	}
	var real payments.OutboundCredentialResolver
	if or := b.Credentials.Outbound("payments"); or != nil {
		real = or
	}
	return payments.NewOutboundKindSplitResolver(b.paymentsAdapters(), mock, real)
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

// kycOutboundCredentials is CreateVerification/SubmitVerification's phase B
// credential resolver (ADR 0095 §15.2/§15.3/§9.1, PROV-OUTBOUND-CRED-1) -
// kycOrchestratorResolver's OUTBOUND twin, mirroring
// casinoOutboundCredentials exactly (including its kind-split discipline
// and its nil-concrete-pointer-to-true-nil-interface conversion).
func (b providerBundle) kycOutboundCredentials() kyc.OutboundCredentialResolver {
	var mock kyc.OutboundCredentialResolver
	if b.KYCOutboundResolver != nil && b.KYC != nil {
		mock = b.KYCOutboundResolver
	}
	var real kyc.OutboundCredentialResolver
	if or := b.Credentials.Outbound("kyc"); or != nil {
		real = or
	}
	return kyc.NewOutboundKindSplitResolver(b.kycAdapters(), mock, real)
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
		{Domain: "payments", Name: "statement_source", Component: b.PaymentsStmt},
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
		// The casino OUTBOUND-credential MOCK (security review RV-PRH-I2
		// C3) - a nil one (wiring off) is skipped by the guard, same as
		// every other MOCK resolver above.
		providerkind.Registration{Domain: "casino", Name: "outbound_resolver", Component: b.CasinoOutboundResolver},
		// The KYC OUTBOUND-credential MOCK (ADR 0095 §15.2/§15.3, PRH-I2) -
		// a nil one (wiring off) is skipped by the guard, same as every
		// other MOCK resolver above.
		providerkind.Registration{Domain: "kyc", Name: "outbound_resolver", Component: b.KYCOutboundResolver},
		// The payments OUTBOUND-credential MOCK (ADR 0095 §9.1/§11, PRH-I1
		// phase 2 orchestrator wiring) - a nil one (wiring off) is skipped
		// by the guard, same as every other MOCK resolver above.
		providerkind.Registration{Domain: "payments", Name: "outbound_resolver", Component: b.PaymentsOutboundResolver},
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

// buildPaymentsSweeper constructs the production payments Sweeper (PRH-2 H,
// CP-W1) from the SAME orchestrator and the SAME bundle-derived outbound
// credential resolver the HTTP path uses, so it reaches only MOCK adapters that
// already passed the synthetic guard (the real path stays PROVIDER DEPENDENT).
//
// PayoutKYCGate is set EXPLICITLY (code-reviewer H3): NewSweeper leaves it nil,
// and a nil gate makes the whole payout half (T2 re-claim, T12 resend, payout
// resolve) a silent no-op. TestBuildPaymentsSweeper_PayoutKYCGateNonNil pins it,
// and Sweeper.ValidateForLoop refuses a nil one at startup. It lives here, not in
// main.go, because the provider-construction AST guard confines payments-package
// constructors to this file.
func buildPaymentsSweeper(pool *db.Pool, orch *payments.Orchestrator, b providerBundle) *payments.Sweeper {
	s := payments.NewSweeper(pool, orch, payments.KYCEnforcementDepositGate{}, b.paymentsOutboundCredentials())
	s.PayoutKYCGate = payments.KYCEnforcementPayoutGate{}
	// Efficiency hint only (LF-8): correctness rests on the row lease, the claim
	// token CAS and ledger idempotency, never on this.
	s.TenantAdvisoryHint = true
	return s
}

// buildKYCOutboxWorker builds the PRH-2 E1 KYC submission outbox worker from the
// SAME orchestrator and outbound credential resolver as the HTTP path (ADR 0106
// section 7.4, security D1). It returns nil, never an error, when either is
// absent: that is the production / test-support-off shape (kycOrchestrator
// returns a true nil), where the create handler answers 503 before writing any
// row and main.go logs once and does not start the worker.
func buildKYCOutboxWorker(pool *db.Pool, orch *kyc.Orchestrator, outbound kyc.OutboundCredentialResolver) *kyc.OutboxWorker {
	if orch == nil || outbound == nil {
		return nil
	}
	return kyc.NewOutboxWorker(pool, orch, outbound)
}
