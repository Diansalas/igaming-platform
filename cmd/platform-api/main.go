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

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/bonus"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/email"
	"github.com/Diansalas/igaming-platform/internal/httpserver"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/sportsbook"
)

// kycMockWebhookSecret is MockKYCProvider's dev/test-only HMAC signing
// key - never a real credential, since no real KYC vendor is integrated
// (docs/decisions/0028 §1). A real vendor's own webhook secret would come
// from cfg (internal/config), provisioned per environment, never
// hardcoded like this.
const kycMockWebhookSecret = "dev-mock-kyc-webhook-secret-not-for-production"

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
	if err := db.VerifyRuntimeRoleInProduction(ctx, cfg.Environment, pool); err != nil {
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
	orchestrator := payments.NewOrchestrator(map[string]payments.PaymentProvider{
		"mock-payments": payments.NewMockProvider("mock-payments", "EUR", "USD", "GBP", "BRL", "MXN"),
	})

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
	casinoOrchestrator := casino.NewOrchestrator(map[string]casino.CasinoProvider{
		"mock-casino": casino.NewMockCasinoProvider("mock-casino", "EUR", "USD", "GBP", "BRL", "MXN"),
	})

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
	if err := pool.WithPlatformService(ctx, db.ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
		return sportsbook.SyncCatalogue(ctx, tx, sportsbook.NewMockSportsbookProvider())
	}); err != nil {
		return fmt.Errorf("sync sportsbook catalogue: %w", err)
	}
	logger.Info("sportsbook catalogue synced")

	handler := httpserver.New(httpserver.Deps{
		Logger:              logger,
		DB:                  pool,
		AuthIssuer:          issuer,
		ServiceName:         cfg.OTelServiceName,
		AccessTokenTTL:      cfg.AccessTokenTTL,
		RefreshTokenTTL:     cfg.RefreshTokenTTL,
		PaymentOrchestrator: orchestrator,
		CasinoOrchestrator:  casinoOrchestrator,
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
		// Stage 4E: no real identity-resolution vendor is contracted yet
		// (docs/decisions/0027 §3) - MockPersonResolver's honest default
		// (NoMatch for every registration, since none carries verified
		// identity evidence today) preserves Stage 2's original
		// registration behavior while ensuring every registration now
		// goes through the resolution boundary, never bypassing it.
		PersonResolver: identityresolution.NewMockPersonResolver(),

		// Stage 4F: no real KYC/identity-verification vendor is contracted
		// yet (docs/decisions/0028 §1) - MockKYCProvider is the only
		// implementation registered, exactly mirroring the mock casino/
		// payment adapters' identical role. KYC verification is a
		// genuinely optional/deferred flow (unlike PersonResolver above),
		// so nil-means-disabled would also be a legitimate choice for a
		// deployment that doesn't want it exposed yet - this deployment
		// enables it.
		KYCOrchestrator: kyc.NewOrchestrator(map[string]kyc.KYCProvider{
			"mock": kyc.NewMockKYCProvider(kycMockWebhookSecret),
		}),
		DocumentStorage: kyc.NewMockDocumentStorageProvider(),
		MalwareScanner:  kyc.NewMockMalwareScanner(),

		// Stage 4F: no real email-delivery vendor is contracted yet
		// (docs/decisions/0030 §4) - email.MockProvider records what would
		// have been sent without any production SMTP/API credentials.
		EmailProvider: email.NewMockProvider(),
	})

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
		reconciliation.RunSchedulerLoop(ctx, pool, logger, cfg.ReconciliationInterval)
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

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
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

	logger.Info("shutdown complete")
	return nil
}
