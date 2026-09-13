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
	"syscall"
	"time"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/httpserver"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/payments"
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
	// ProviderCapability row (PUT /v1/admin/providers/mock/capability)
	// before any deposit can route to it - registering the adapter here
	// does not itself enable it for any tenant.
	orchestrator := payments.NewOrchestrator(map[string]payments.PaymentProvider{
		"mock": payments.NewMockProvider("mock", "EUR", "USD", "GBP", "BRL", "MXN"),
	})

	handler := httpserver.New(httpserver.Deps{
		Logger:              logger,
		DB:                  pool,
		AuthIssuer:          issuer,
		ServiceName:         cfg.OTelServiceName,
		AccessTokenTTL:      cfg.AccessTokenTTL,
		RefreshTokenTTL:     cfg.RefreshTokenTTL,
		PaymentOrchestrator: orchestrator,
	})

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
	logger.Info("shutdown complete")
	return nil
}
