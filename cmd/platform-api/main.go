// Command platform-api is the Stage 1 foundation service: config
// loading, structured logging, OpenTelemetry tracing/metrics, database
// connectivity, JWT auth, tenant-context/RLS enforcement, health checks,
// and graceful shutdown. It intentionally does not implement wallet,
// payments, casino, sportsbook, bonus, or KYC business logic - see
// docs/active-stage.md for what Stage 1 covers.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/bonus"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/httpserver"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/sportsbook"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "platform-api: fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// MOCK-ADAPTER-PROD-1 (Stage 10.3, ADR 0085's Stage 10.3 amendment;
	// security condition C13, ruling R8): the synthetic-component startup
	// guard runs IMMEDIATELY after config.Load() - before the logger,
	// before tracing, before db.Connect, and before the sportsbook
	// catalogue sync below. buildProviderBundle constructs every mock
	// component this binary ever wires (no I/O, no database), so this
	// guard's placement, not merely its logic, is what proves refusal
	// happens before any side effect - see
	// TestSyntheticGuard_RunsBeforeDBConnect_Subprocess.
	//
	// wiring/bundle computed here are the SAME values used for every
	// wiring decision later in this function - nothing is constructed a
	// second time under a different name.
	wiring := mockProviderWiring(cfg)
	providers, err := withCredentialSubsystem(ctx, cfg, buildProviderBundle(wiring))
	if err != nil {
		return err
	}
	if err := refuseSyntheticInProduction(cfg, buildRegistrations(cfg, providers)); err != nil {
		return err
	}
	// Security S-1/I-1: every domain's webhook schemes are validated here,
	// pre-DB, as an error (the orchestrator constructors below re-check and
	// panic as the last line of defence).
	if err := validateWebhookSchemes(providers.paymentsAdapters(), providers.casinoAdapters(), providers.kycAdapters()); err != nil {
		return fmt.Errorf("webhook scheme registration: %w", err)
	}

	logger := observability.NewLogger(cfg.Environment)
	logger.Info("starting platform-api", "environment", cfg.Environment)

	_, shutdownTracing, err := observability.InitTracing(ctx, cfg.OTelServiceName, cfg.OTelExporter)
	if err != nil {
		return fmt.Errorf("init tracing: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(shutdownCtx); err != nil {
			logger.Error("tracing shutdown failed", "error", err)
		}
	}()

	_, shutdownMetrics, err := observability.InitMetrics(ctx, cfg.OTelServiceName, cfg.OTelExporter)
	if err != nil {
		return fmt.Errorf("init metrics: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownMetrics(shutdownCtx); err != nil {
			logger.Error("metrics shutdown failed", "error", err)
		}
	}()

	pool, err := db.Connect(ctx, cfg.DatabaseURL, cfg.DatabaseMaxConns, cfg.DatabaseConnTimeout)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()
	logger.Info("database connected")

	// PLAT-ROLESPLIT-1 (docs/security/runtime-role-separation.md):
	// fail-closed production safety check. Refuses to start if the
	// connecting role owns tables (i.e. it is the migration-owner role,
	// not a non-owning runtime role) - see db.VerifyRuntimeRoleInProduction's
	// own doc comment for exactly why this is scoped to production only
	// and does not apply to development/CI/staging.
	//
	// Stage 10.3 W1b (found by `architect`'s W0 code check, same class of
	// gap as security condition C13): this now passes cfg.GuardEnvironment()
	// rather than cfg.Environment directly, so a production task deployed
	// with APP_ENV missing entirely is no longer silently exempted from
	// this check either. VerifyRuntimeRoleInProduction ITSELF is
	// unchanged - it still gates on the literal string "production" and
	// has no knowledge of GuardEnvironment; only the value passed in here
	// changed. See config.Config.GuardEnvironment's own doc comment.
	if err := db.VerifyRuntimeRoleInProduction(ctx, cfg.GuardEnvironment(), pool); err != nil {
		return err
	}

	keys := map[string]string{cfg.JWTActiveKID: cfg.JWTSigningSecret}
	if cfg.JWTPreviousSecret != "" {
		keys[cfg.JWTPreviousKID] = cfg.JWTPreviousSecret
	}
	keyRegistry, err := auth.NewKeyRegistry(cfg.JWTActiveKID, keys)
	if err != nil {
		return fmt.Errorf("build JWT key registry: %w", err)
	}
	issuer := auth.NewIssuer(keyRegistry, cfg.JWTIssuer, cfg.JWTAudience)

	// Stage 3B ships a mock PSP only (CLAUDE.md's Stage 3B scope gate) -
	// registered exactly like a future real adapter would be, via the
	// same PaymentProvider interface and provider_id-keyed registry
	// (docs/decisions/0022 §2.1). A tenant must still write its own
	// ProviderCapability row (PUT /v1/admin/providers/mock-payments/
	// capability) before any deposit can route to it - registering the
	// adapter here does not itself enable it for any tenant.
	//
	// Stage 9.3 finding: this provider_id must be distinct from casino's
	// own mock adapter's id below. Both post to the SAME ledger, whose
	// idempotency key is (provider_id, provider_tx_id) (CLAUDE.md's
	// financial rules), and both mock adapters independently generate
	// provider_tx_id via an identical low-entropy per-instance sequential
	// counter starting at 1 ("<providerID>-1", "<providerID>-2", ...) -
	// see MockProvider/MockCasinoProvider's own tx-ref generation. Two
	// adapters sharing the literal id "mock" therefore alias their FIRST
	// transaction onto the same idempotency key ("mock:mock-1"), and
	// internal/ledger's own reused-key guard correctly refuses the second
	// one as a cross-type collision - found via a real Stage 9.3 staging
	// acceptance run (a funded deposit followed by the first-ever casino
	// rollback in that environment failed with exactly this error). No
	// real deployment would ever register two different vendors under the
	// identical provider_id in the first place; this is that same
	// uniqueness requirement, just not yet enforced by a doc convention
	// before this stage found it the hard way.
	// PAY-WH-TENANT-1 (ADR 0090; docs/decisions/0022 §3 amendment
	// 2026-09-26): bind the mock adapter to a variable so its own
	// MockWebhookCredentials resolver can be wired into the same
	// Orchestrator - the ONLY component that ever sees inbound-webhook
	// credential secret material. This is a MOCK resolver; the real
	// resolver (a FORCE-RLS handle table plus an external secret store) is
	// NOT IMPLEMENTED, and any provider without a resolver entry fails
	// closed on every callback.
	//
	// PAYWH-GATE-1 (Stage 10.2, ADR 0091, ruling J9): the MOCK resolver is
	// wired only when mockProviderWiring (derived solely from
	// cfg.TestSupportRoutesEnabled(), ADR 0085) says so. In production or
	// with test support off the Orchestrator gets a nil resolver, so every
	// payments webhook fails closed with the uniform 401 (no_resolver).
	// The mock adapter itself stays registered for initiation
	// (MOCK-ADAPTER-PROD-1, closed in W1b via the guard above).
	//
	// mockPaymentsProvider/mockCasinoProvider/mockKYCProvider below are the
	// SAME instances buildProviderBundle already constructed above (before
	// db.Connect) and passed through the synthetic guard - not a second,
	// independent construction (Stage 10.3 W1b, MOCK-ADAPTER-PROD-1
	// coverage requirement: "all-of-main").
	//
	// Stage 10.3 gate-W1 fix round (security S-4): the MOCK resolver is
	// also a bundle component now (registered with the guard above);
	// paymentsOrchestratorResolver only composes it. No file in this
	// package other than registrations.go constructs a provider component
	// (TestMain_ConstructsNoProviderComponentOutsideRegistrations).
	orchestrator := payments.NewOrchestrator(providers.paymentsAdapters(), providers.paymentsOrchestratorResolver())
	orchestrator.SetWebhookLogger(logger) // W2A-SEC-2 matched-key_id line

	// KYC-WH-1 (Stage 10.2, ADR 0091; final review K11): providers.KYC is
	// non-nil ONLY when wiring enabled it at bundle-construction time (ADR
	// 0085 amendment: "absent" in production/test-support-off, not merely
	// unwired). kycOrchestrator below guards against a nil provider.

	// Stage 4A ships a mock casino adapter only (CLAUDE.md's Stage 4A
	// scope gate) - registered exactly like a future real aggregator
	// would be, via the same CasinoProvider interface and provider_id-
	// keyed registry (ADR 0025 §1/§4). A tenant must still write its own
	// CasinoProviderCapability row (PUT
	// /v1/admin/casino/providers/mock-casino/capability) and opt a
	// platform-catalogue title into its own availability before any
	// player can launch it - registering the adapter here does not itself
	// enable it for any tenant. See the payments registration above for
	// why this provider_id must differ from it.
	// CAS-WH-TENANT-1 (Stage 10.2, ADR 0091): bind the mock adapter to a
	// variable so its own MockWebhookCredentials resolver can be wired
	// into the same Orchestrator - the ONLY component that ever sees
	// inbound-webhook credential secret material. This is a MOCK
	// resolver; the real resolver (a FORCE-RLS handle table plus an
	// external secret store) is NOT IMPLEMENTED. The resolver is wired
	// only when mockProviderWiring (derived solely from
	// cfg.TestSupportRoutesEnabled(), ADR 0085) says so - in production
	// or with test support off the Orchestrator gets a nil resolver, so
	// every casino webhook fails closed with the uniform 401
	// (no_resolver). The mock adapter itself stays registered for
	// catalogue and launch (MOCK-ADAPTER-PROD-1, a pre-launch checklist
	// item - not this stage's scope).
	casinoOrchestrator := casino.NewOrchestrator(providers.casinoAdapters(), providers.casinoOrchestratorResolver())
	casinoOrchestrator.SetWebhookLogger(logger) // W2A-SEC-2 matched-key_id line

	// Stage 6 ships a mock sportsbook provider only (CLAUDE.md's "does not
	// integrate a real provider without a confirmed commercial
	// relationship" limitation) - no adapter registry is needed for bet
	// placement itself (internal/sportsbook.PlaceBet has no external
	// provider round-trip, see that package's own doc comment), only for
	// catalogue sync. Synced once at startup, idempotently (SyncCatalogue
	// upserts keyed by external_ref, safe to re-run on every restart) -
	// event start times are computed relative to time.Now() at sync time
	// (MockSportsbookProvider's own doc comment), so they stay "near
	// future" regardless of how long this binary has existed. A real
	// provider adapter implementing sportsbook.Provider is a drop-in
	// replacement for this one call, with zero change to the domain model,
	// the orchestrator, or the HTTP layer.
	//
	// Migration 0084 (ADR 0081, ARCH-DB-2) gave sb_sports/sb_competitions/
	// sb_events/sb_markets/sb_selections ENABLE+FORCE row-level security
	// with a write policy scoped to a new, closed-vocabulary
	// app.platform_service_id = 'sportsbook_catalogue_sync' GUC, so a
	// plain WithoutTenant transaction (the scope every ordinary platform
	// read already uses) can no longer write these tables - this startup
	// sync is the sole legitimate writer and now authorizes itself as
	// such via db.Pool.WithPlatformService, rather than laundering as an
	// unscoped connection. If the sync cannot authorize itself, the
	// binary must not serve traffic with a half-synced catalogue, so the
	// existing fail-the-startup wrap below is unchanged.
	// SB-CATALOGUE-IO-1 (ADR 0095 amendment, 2026-09-28): the provider call
	// runs first, with no database transaction open at all (FetchCatalogue
	// refuses under txscope.Held and never opens one itself); only once it
	// has returned a validated result does the platform-service-scoped
	// transaction open, to upsert that already-fetched result.
	sbCatalogue, err := sportsbook.FetchCatalogue(ctx, providers.Sportsbook)
	if err != nil {
		return fmt.Errorf("sync sportsbook catalogue: %w", err)
	}
	if err := pool.WithPlatformService(ctx, db.ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
		return sportsbook.SyncCatalogue(ctx, tx, sbCatalogue)
	}); err != nil {
		return fmt.Errorf("sync sportsbook catalogue: %w", err)
	}
	logger.Info("sportsbook catalogue synced")

	// PRH-2 E1 (ADR 0106 section 8.2, security D1): ONE KYC orchestrator and
	// ONE outbound credential resolver, hoisted so the HTTP path and the KYC
	// outbox worker below are built from the very same values. kycOrch is a
	// true nil when test-support routes are off (production): then the create
	// handler answers 503 before writing any row and the worker is not started.
	kycOrch := kycOrchestrator(wiring, providers, logger)
	kycOutboundCreds := providers.kycOutboundCredentials()

	// ALERT-DELIVERY-1 (ADR 0102 section 18): the dispatcher is constructed
	// here so the platform status endpoint can report its routing readiness.
	// It is STARTED further below (PRH-2 I-wire). LogSink only: a log line is
	// not a notification to any person, so readiness stays NOT READY.
	alertDispatcher := alerting.NewDispatcher(pool, alerting.DefaultDispatcherConfig(), alerting.LogSink{Logger: logger})

	handler, webhookAdmission := httpserver.NewWithAdmission(httpserver.Deps{
		AlertRouting:                alertDispatcher,
		AlertRoutingProduction:      cfg.GuardEnvironment() == "production",
		Logger:                      logger,
		DB:                          pool,
		AuthIssuer:                  issuer,
		ServiceName:                 cfg.OTelServiceName,
		AccessTokenTTL:              cfg.AccessTokenTTL,
		RefreshTokenTTL:             cfg.RefreshTokenTTL,
		PaymentOrchestrator:         orchestrator,
		PaymentsOutboundCredentials: providers.paymentsOutboundCredentials(),
		CasinoOrchestrator:          casinoOrchestrator,
		CasinoOutboundCredentials:   providers.casinoOutboundCredentials(),
		// ADR 0097 PRH-I4 (PAYWH-RL-1): mapped field by field from
		// config.WebhookAdmissionConfig - internal/httpserver never imports
		// internal/config (architect review AC1).
		WebhookAdmission: toWebhookAdmissionSettings(cfg.WebhookAdmission),
		// S9.1-LAUNCH-1/S9.1-LAUNCH-2 (docs/security/security-
		// architecture.md): both plumbed straight from environment-sourced
		// config, with no hardcoded default overriding cfg's own (0/0 -
		// see internal/config.Config's doc comments on each field for the
		// exact semantics).
		AuthRateLimitPerMinute: cfg.AuthRateLimitPerMinute,
		TrustedProxyCount:      cfg.TrustedProxyCount,
		CORSAllowedOrigins:     cfg.CORSAllowedOrigins,
		// Stage 7: the mock-provider play-simulation routes let an
		// authenticated player mint self-signed casino callbacks on their
		// own behalf (casino_play_handlers.go's own doc comment) - a
		// deliberate, disclosed MOCK seam for a vertical slice with no
		// real hosted game client, never safe to expose once "production"
		// means real money. Specialist review requirement (architect/
		// security/ledger-finance, independently).
		//
		// Stage 9.4: two independent conditions are now both required -
		// see internal/config.Config's Environment/TestSupportEndpointsEnabled
		// doc comments, and Config.TestSupportRoutesEnabled's own doc
		// comment, for the full two-layer, fail-closed design this
		// replaced (a Stage 9.3 security-review finding: `cfg.Environment
		// != "production"` alone fails OPEN on a typo or an unset APP_ENV).
		// config.Load() itself already refuses to start if Environment is
		// an unrecognized value, or if TestSupportEndpointsEnabled is true
		// while Environment == "production" - so by the time this line
		// runs, both operands are already known-good; TestSupportRoutesEnabled
		// is the second, independent layer, not a redundant restatement of
		// the first.
		CasinoPlaySimulationEnabled: cfg.TestSupportRoutesEnabled(),
		// Stage 9.3: the payments-domain twin of the above - see
		// Deps.PaymentsMockSettlementEnabled's own doc comment
		// (internal/httpserver/server.go) and payment_deposit_simulation_
		// handlers.go's doc comment for the full rationale. Same gate,
		// same Stage 9.4 two-layer guarantee as above.
		PaymentsMockSettlementEnabled: cfg.TestSupportRoutesEnabled(),
		// Stage 9.3: closes the account-activation gap the two flags above
		// don't - see Deps.AccountActivationTestSupportEnabled's own doc
		// comment (internal/httpserver/server.go) for the full rationale.
		// Same gate, same Stage 9.4 two-layer guarantee as above.
		AccountActivationTestSupportEnabled: cfg.TestSupportRoutesEnabled(),
		SportsbookEnabled:                   true,
		// Stage 10 W1 (ADR 0088 §9): the sportsbook-domain twin of the
		// three flags above - see Deps.SportsbookSettlementSimulationEnabled's
		// own doc comment (internal/httpserver/server.go) for the full
		// rationale. Same gate, same Stage 9.4 two-layer guarantee as above.
		SportsbookSettlementSimulationEnabled: cfg.TestSupportRoutesEnabled(),
		// Stage 4E: no real identity-resolution vendor is contracted yet
		// (docs/decisions/0027 §3) - MockPersonResolver's honest default
		// (NoMatch for every registration, since none carries verified
		// identity evidence today) preserves Stage 2's original
		// registration behavior while ensuring every registration now
		// goes through the resolution boundary, never bypassing it.
		PersonResolver: providers.PersonResolver,

		// Stage 4F: no real KYC/identity-verification vendor is contracted
		// yet (docs/decisions/0028 §1) - MockKYCProvider is the only
		// implementation registered, exactly mirroring the mock casino/
		// payment adapters' identical role.
		//
		// KYC-WH-1 (Stage 10.2, ADR 0091, design §B3, ruling J7): the mock
		// provider, its webhook credential resolver, AND the Orchestrator
		// itself are now wired from the SAME mockWiring value that decided
		// the payments resolver above - kycOrchestrator returns a true nil
		// when cfg.TestSupportRoutesEnabled() is false, which makes player
		// self-service KYC (POST /v1/me/kyc/verifications) unavailable
		// (503) in production/test-support-off, a disclosed consequence
		// (design §B3/§D): there is no real KYC vendor to fall back to.
		// KYCWebhookEnabled below gates the ROUTE itself identically, so
		// route registration and orchestrator wiring cannot diverge (K11).
		KYCOrchestrator:        kycOrch,
		KYCWebhookEnabled:      wiring.KYCWebhookEnabled,
		DocumentStorage:        providers.DocumentStorage,
		MalwareScanner:         providers.MalwareScanner,
		KYCOutboundCredentials: kycOutboundCreds,

		// Stage 4F: no real email-delivery vendor is contracted yet
		// (docs/decisions/0030 §4) - email.MockProvider records what would
		// have been sent without any production SMTP/API credentials.
		EmailProvider: providers.Email,

		// Stage 10.3 W2a (ADR 0093): the real provider-credential
		// subsystem, nil unless a fingerprint key AND a permitted
		// secret-store backend are configured.
		ProviderCredentials: providers.Credentials,
	})

	// ADR 0097 §7 (PRH-I4): startup waits for the webhook tenant
	// directory's first successful load before serving any traffic - a
	// failure here does not fail startup (a down DB is already surfaced by
	// /readyz and by the earlier db.Connect/HealthCheck calls above); it
	// logs and leaves the directory not-Loaded(), so /readyz stays
	// not-ready and every webhook route answers 503 until a later
	// background refresh succeeds (never falls back to unbounded/raw-slug
	// keying).
	if err := webhookAdmission.LoadDirectory(ctx); err != nil {
		logger.Error("webhook_tenant_directory_initial_load_failed", "error", err)
	}
	// devops condition 2 (ADR 0097 §17): the refresher goroutine is wired
	// to the SAME ctx signal.NotifyContext cancels on shutdown, so it stops
	// cleanly and never logs after the deferred pool.Close()/tracing/
	// metrics shutdowns above run.
	go webhookAdmission.RunDirectoryRefresh(ctx)

	// Stage 3C directive item 4: operationalize the ledger-vs-projection
	// reconciliation stream, which Stage 3B built but never actually
	// scheduled - every tenant is swept on cfg.ReconciliationInterval
	// (default hourly, the Blueprint's target cadence) until shutdown.
	// See internal/reconciliation/scheduler.go for the per-tenant
	// isolation/idempotency/observability guarantees, and its own
	// panic-recovery (this goroutine has no other backstop - unlike the
	// HTTP path, there is no recoverMiddleware wrapping it).
	//
	// reconcilerDone lets graceful shutdown below actually wait
	// (bounded) for the current tick to finish, rather than relying on
	// pool.Close()'s incidental blocking behavior - specialist review
	// (backend): an unbounded wait on a stuck query would otherwise hang
	// shutdown indefinitely, unlike the HTTP server's own bounded
	// Shutdown call just below.
	var reconcilerWG sync.WaitGroup
	reconcilerWG.Add(1)
	go func() {
		defer reconcilerWG.Done()
		// The sportsbook_settlement stream's statement source is the
		// MOCK in-house source (ADR 0088 §8.4); real provider statement
		// matching is PROVIDER DEPENDENT (ADR 0038 §12). The
		// casino_statement stream's source is likewise the MOCK in-house
		// source (Stage 10.3 W3a, CAS-RECON-STMT-1): tautological against
		// the ledger it renders from; real casino statement matching is
		// PROVIDER DEPENDENT. The payment_statement stream's source is the
		// MOCK payments statement source (PRH-I5, ADR 0095 §12.4): the
		// MockProvider's own records, fetched with no transaction held;
		// real PSP statement matching is PROVIDER DEPENDENT.
		reconciliation.RunSchedulerLoop(ctx, pool, logger, cfg.ReconciliationInterval, providers.SettlementStmt, providers.CasinoStmt, providers.PaymentsStmt)
	}()

	// Stage 4H-B0-R7 directive item 4: operationalize the self-exclusion
	// enumeration-run reconciliation queries (FindMissingEnumerationRuns,
	// FindStalledEnumerationRuns), which Stage 4H-B0-R6 built but never
	// actually scheduled - the exact "index/query with zero running
	// callers" shape as the ledger-vs-projection sweep had at Stage 3B,
	// closed the identical way. See internal/rg/enumeration_sweep.go for
	// the per-tenant isolation/idempotency/observability guarantees
	// (mirrors reconciliation.RunSchedulerLoop's own).
	var rgSweepWG sync.WaitGroup
	rgSweepWG.Add(1)
	go func() {
		defer rgSweepWG.Done()
		rg.RunEnumerationReconciliationSchedulerLoop(ctx, pool, logger, cfg.RGEnumerationSweepInterval, cfg.RGEnumerationStalledThreshold)
	}()

	// Stage 4H-B1 Wave 3 Phase 3 (bonus-engine): the three Bonus Engine
	// scheduled jobs ledger-accounting-model.md §7.18.2/§7.18.4 and the
	// Grant-expiry dependency-map entry specify - each had zero live
	// caller before this dispatch (docs/governance/wave-3-reconnaissance.md
	// gap-list items 7, 10-12, 20). Identical per-tenant advisory-lock/
	// panic-recovery/graceful-shutdown posture as reconciliation's own
	// loop above and rg's own enumeration sweep - see
	// internal/bonus/schedulers.go for the shared implementation.
	var bonusSchedulersWG sync.WaitGroup
	bonusSchedulersWG.Add(3)
	go func() {
		defer bonusSchedulersWG.Done()
		bonus.RunDepositSweepSchedulerLoop(ctx, pool, logger, cfg.BonusDepositSweepInterval)
	}()
	go func() {
		defer bonusSchedulersWG.Done()
		bonus.RunCashbackSchedulerLoop(ctx, pool, logger, cfg.BonusCashbackSweepInterval)
	}()
	go func() {
		defer bonusSchedulersWG.Done()
		bonus.RunExpirySweepSchedulerLoop(ctx, pool, logger, cfg.BonusExpirySweepInterval)
	}()

	// ---- PRH-2 H (CP-W1): payments sweeper process (begin) -------------------
	// Activates the deposit poll AND payout dispatch/resend/resolve
	// (payments.RunSweeperLoop). The Sweeper is built in registrations.go from the
	// same orchestrator and bundle resolver as the HTTP path, with PayoutKYCGate set
	// explicitly; ValidateForLoop refuses to start one without it. Non-active
	// tenants are resolution-only (ADR 0095 §7.3). MOCK adapters only today.
	paymentsSweeper := buildPaymentsSweeper(pool, orchestrator, providers)
	if err := paymentsSweeper.ValidateForLoop(); err != nil {
		return fmt.Errorf("payments sweeper wiring: %w", err)
	}
	var paymentsSweeperWG sync.WaitGroup
	paymentsSweeperWG.Add(1)
	go func() {
		defer paymentsSweeperWG.Done()
		payments.RunSweeperLoop(ctx, paymentsSweeper, logger, cfg.PaymentsSweepInterval)
	}()
	// ---- PRH-2 H: payments sweeper process (end) -----------------------------

	// ---- PRH-2 I-wire (ALERT-DELIVERY-1): durable alert dispatcher (begin) ----
	// ADR 0102 6.1/17.6. LogSink only: real channels are PROVIDER DEPENDENT and
	// HD-PRH2-4-OPS is open, and the MOCK sink is never wired into a binary. No
	// alert_routes row is seeded, so after this starts every alert is 'unrouted'
	// until a human configures routes; nothing is delivered to a person. The loop
	// recovers a panic per pass and drains the pass in flight on shutdown.
	var alertDispatcherWG sync.WaitGroup
	alertDispatcherWG.Add(1)
	go func() {
		defer alertDispatcherWG.Done()
		alerting.RunDispatcherLoop(ctx, alertDispatcher, alerting.DefaultLoopInterval)
	}()
	// ---- PRH-2 I-wire: durable alert dispatcher (end) -------------------------

	// ---- PRH-2 E1 (KYC-SUBMIT-OUTBOX-1): KYC submission outbox worker (begin) ----
	// ADR 0106 7.4. The worker is built from the SAME orchestrator and outbound
	// credential resolver as the HTTP path (kycOrch / kycOutboundCreds above). With
	// no orchestrator (production / test-support off) the create handler answers
	// 503 before writing a row, so there is nothing to drain: the worker is NOT
	// started, this is logged once, and startup never fails. MOCK adapter only;
	// a real KYC vendor is PROVIDER DEPENDENT. Alert notification for a terminal
	// outbox row is NOT IMPLEMENTED (ALERT-DELIVERY-1 OPEN).
	kycOutboxWorker := buildKYCOutboxWorker(pool, kycOrch, kycOutboundCreds)
	var kycOutboxWG sync.WaitGroup
	if kycOutboxWorker == nil {
		logger.Info("kyc outbox worker not started: no KYC orchestrator or outbound credential resolver is wired")
	} else {
		if err := kycOutboxWorker.ValidateForLoop(); err != nil {
			return fmt.Errorf("kyc outbox worker wiring: %w", err)
		}
		kycOutboxWG.Add(1)
		go func() {
			defer kycOutboxWG.Done()
			kyc.RunOutboxWorkerLoop(ctx, kycOutboxWorker, logger, cfg.KYCOutboxInterval)
		}()
	}
	// ---- PRH-2 E1: KYC submission outbox worker (end) ----

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		// HTTP-TIMEOUTS-1 (RL-F3, registered by ADR 0097 §1/§13; devops
		// condition 1: a SEPARATE, platform-wide fix, not folded into the
		// webhook-admission change set). Before this, only
		// ReadHeaderTimeout was set, so a slow body sender (or a
		// never-responding client on any route, not only webhooks) held a
		// goroutine indefinitely.
		//
		// ReadTimeout/WriteTimeout apply to EVERY route, including the
		// admin/back-office/reconciliation/statement handlers devops's own
		// review flagged as the ones needing headroom analysis (none of
		// them declare an explicit per-request deadline today - see
		// internal/httpserver's own health.go's 2s readyz timeout for the
		// only existing precedent, which is far too short for a bulk
		// admin/report query). 60s is a deliberately generous, NON-GATING
		// estimate (CLAUDE.md "technical default, not measured"): no admin/
		// report handler in this codebase streams or holds a connection
		// open, they are all bounded, synchronous JSON responses, so this
		// bounds a genuinely stuck request without being tight enough to
		// abort a legitimate large reconciliation/statement query under
		// normal load. Revisit with real measurement before this is relied
		// on as a production SLA (registered as HTTP-TIMEOUTS-1's own
		// follow-up, not re-litigated by this ADR 0097 commit).
		//
		// IdleTimeout bounds a keep-alive connection sitting open between
		// requests - unrelated to any single handler's duration.
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", cfg.HTTPAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	// The OS signal that got us here already canceled ctx (that's what
	// unblocked the <-ctx.Done() case above), so RunSchedulerLoop's own
	// ctx.Done() case is already unblocking its loop too - this is a
	// bounded wait for whatever sweep tick was already in flight to
	// actually finish, matching the HTTP server's own bounded Shutdown
	// above rather than the previous, undocumented reliance on
	// pool.Close()'s incidental blocking-until-idle behavior.
	reconcilerDone := make(chan struct{})
	go func() {
		reconcilerWG.Wait()
		close(reconcilerDone)
	}()
	select {
	case <-reconcilerDone:
	case <-time.After(10 * time.Second):
		logger.Error("reconciliation scheduler did not stop within the shutdown timeout")
	}

	// Identical bounded-wait treatment for the enumeration reconciliation
	// sweep - same reasoning as reconcilerDone above.
	rgSweepDone := make(chan struct{})
	go func() {
		rgSweepWG.Wait()
		close(rgSweepDone)
	}()
	select {
	case <-rgSweepDone:
	case <-time.After(10 * time.Second):
		logger.Error("self-exclusion enumeration reconciliation scheduler did not stop within the shutdown timeout")
	}

	// Identical bounded-wait treatment for the three Bonus Engine
	// scheduler loops - same reasoning as reconcilerDone/rgSweepDone above.
	bonusSchedulersDone := make(chan struct{})
	go func() {
		bonusSchedulersWG.Wait()
		close(bonusSchedulersDone)
	}()
	select {
	case <-bonusSchedulersDone:
	case <-time.After(10 * time.Second):
		logger.Error("bonus engine scheduler loops did not stop within the shutdown timeout")
	}

	// ---- PRH-2 H: payments sweeper shutdown drain (begin) --------------------
	// RunSweeperLoop stops starting items on ctx cancel and drains the item in
	// flight (payments.SweeperItemTimeout); this is the same bounded wait.
	paymentsSweeperDone := make(chan struct{})
	go func() {
		paymentsSweeperWG.Wait()
		close(paymentsSweeperDone)
	}()
	select {
	case <-paymentsSweeperDone:
	case <-time.After(10 * time.Second):
		logger.Error("payments sweeper did not stop within the shutdown timeout")
	}
	// ---- PRH-2 H: payments sweeper shutdown drain (end) ----------------------

	// ---- PRH-2 I-wire: alert dispatcher shutdown drain (begin) ----------------
	// RunDispatcherLoop starts no new pass on ctx cancel and drains the pass in
	// flight within its DrainTimeout (10s); this bounded wait is longer than that.
	alertDispatcherDone := make(chan struct{})
	go func() {
		alertDispatcherWG.Wait()
		close(alertDispatcherDone)
	}()
	select {
	case <-alertDispatcherDone:
	case <-time.After(15 * time.Second):
		logger.Error("alert dispatcher did not stop within the shutdown timeout")
	}
	// ---- PRH-2 I-wire: alert dispatcher shutdown drain (end) ------------------

	// ---- PRH-2 E1: KYC outbox worker shutdown drain (begin) ----
	// RunOutboxWorkerLoop claims no new row on ctx cancel and lets the item in
	// flight finish on its own bounded contexts. Typical item: prepare + phase B
	// + phase C (about 25 s at the defaults); the worst case (prepare 5 s,
	// resolve+call 15 s, three phase-C attempts of 5 s and an apply-conflict
	// transaction of 5 s) is about 40 s, longer than this 30 s wait. Exiting
	// mid-item is benign: the row stays claimed, its lease expires, and the
	// re-claim re-sends with the SAME idempotency key (ADR 0106 section 2.6).
	kycOutboxDone := make(chan struct{})
	go func() {
		kycOutboxWG.Wait()
		close(kycOutboxDone)
	}()
	select {
	case <-kycOutboxDone:
	case <-time.After(30 * time.Second):
		logger.Error("kyc outbox worker did not stop within the shutdown timeout")
	}
	// ---- PRH-2 E1: KYC outbox worker shutdown drain (end) ----

	logger.Info("shutdown complete")
	return nil
}
