package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/alerting/channels"
	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/observability"
)

// A9 wiring helpers (kept out of run() so the AST pins on main.go stay narrow).

// startMetricsServer binds cfg.MetricsAddr SYNCHRONOUSLY (a bind failure fails startup, visibly) and serves the
// Prometheus handler on its own http.Server: never on the public API mux, never any other route. The returned
// function shuts it down. With no address configured it returns a no-op: the feature is off by default.
func startMetricsServer(cfg config.Config, handler http.Handler, logger *slog.Logger) (func(context.Context) error, error) {
	if cfg.MetricsAddr == "" {
		return func(context.Context) error { return nil }, nil
	}
	if handler == nil {
		return nil, errors.New("metrics: METRICS_ADDR is set but no scrape handler was built")
	}
	ln, err := net.Listen("tcp", cfg.MetricsAddr)
	if err != nil {
		return nil, fmt.Errorf("metrics listener: %w", err)
	}
	srv := observability.NewMetricsServer(cfg.MetricsAddr, handler)
	go func() {
		logger.Info("metrics server listening (separate from the public API; restrict at the network layer)", "addr", ln.Addr().String())
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server stopped", "error_type", fmt.Sprintf("%T", err))
		}
	}()
	return srv.Shutdown, nil
}

// buildAlertSinks builds the configured human-notification channels (A9): email over the internal/email
// provider interface and one HMAC-signed webhook. Both are OFF unless configured; nothing here names a
// recipient or URL (they come from ALERT_EMAIL_RECIPIENTS / ALERT_WEBHOOK_URL) and the webhook secret is a
// secret-store reference fetched at use. A configured channel that cannot be built is a startup error:
// a half-configured alert channel must not silently become "unrouted".
func buildAlertSinks(cfg config.Config, providers providerBundle, logger *slog.Logger) ([]alerting.Sink, error) {
	var sinks []alerting.Sink
	if len(cfg.AlertEmailRecipients) > 0 {
		s, err := channels.NewEmailSink(providers.Email, cfg.AlertEmailRecipients, cfg.GuardEnvironment(), logger)
		if err != nil {
			return nil, fmt.Errorf("alert email channel: %w", err)
		}
		sinks = append(sinks, s)
		logger.Info("alert email channel configured", "recipient_count", len(cfg.AlertEmailRecipients), "human_notification", s.HumanNotification())
	}
	if cfg.AlertWebhookURL != "" {
		secret, err := alertWebhookSecretFunc(cfg, providers.SecretBackends)
		if err != nil {
			return nil, fmt.Errorf("alert webhook channel: %w", err)
		}
		s, err := channels.NewWebhookSink(channels.WebhookConfig{
			URL: cfg.AlertWebhookURL, Secret: secret, Logger: logger,
			AllowLoopbackDev: cfg.AlertWebhookURLOptions().AllowLoopbackDev, // one rule, shared with config validation
		})
		if err != nil {
			return nil, fmt.Errorf("alert webhook channel: %w", err)
		}
		sinks = append(sinks, s)
		logger.Info("alert webhook channel configured", "human_notification", s.HumanNotification())
	}
	return sinks, nil
}

// startAlertRetention starts the ALERT-RETENTION-1 purge loop when ALERT_RETENTION_DAYS is set; unset, no
// purge runs and a startup warning says the alert history is unbounded. The returned WaitGroup is waited on
// at shutdown.
func startAlertRetention(ctx context.Context, cfg config.Config, d *alerting.Dispatcher, logger *slog.Logger) (*sync.WaitGroup, error) {
	var wg sync.WaitGroup
	if cfg.AlertRetention == 0 {
		logger.Warn("alert retention is not configured (ALERT_RETENTION_DAYS unset): alert history grows without bound until it is (ALERT-RETENTION-1)")
		return &wg, nil
	}
	rc := alerting.RetentionConfig{OlderThan: cfg.AlertRetention, Interval: cfg.AlertRetentionInterval, Logger: logger}
	if err := alerting.ValidateRetentionConfig(rc); err != nil {
		return nil, err
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		alerting.RunRetentionLoop(ctx, d, rc)
	}()
	logger.Info("alert retention enabled", "retention_days", int(cfg.AlertRetention/(24*time.Hour)), "interval", cfg.AlertRetentionInterval.String())
	return &wg, nil
}

// registerDBPoolMetrics exports the pool saturation signals.
func registerDBPoolMetrics(pool *db.Pool) error {
	if err := db.RegisterPoolMetrics(pool); err != nil {
		return fmt.Errorf("db pool metrics: %w", err)
	}
	return nil
}
