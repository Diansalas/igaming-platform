package config

import (
	"fmt"
	"net"
	"net/mail"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Diansalas/igaming-platform/internal/egressguard"
)

// Observability and alert-delivery configuration (A9). Everything defaults to DISABLED / empty: no listener,
// no recipient, no webhook, no retention purge. Credentials are never configured here: the webhook signing
// secret is a secret-store REFERENCE (internal/secretstore) and the value is fetched at use.
//
//	METRICS_ADDR                      host:port of the dedicated Prometheus listener; unset = no listener
//	METRICS_ALLOW_NON_LOOPBACK        true acknowledges a non-loopback bind in production (network-protected)
//	ALERT_EMAIL_RECIPIENTS            comma-separated bare addresses for the email alert channel
//	ALERT_WEBHOOK_URL                 https URL of the single webhook alert channel
//	ALERT_WEBHOOK_SECRET_REF          secret-store reference of the HMAC signing secret (required with the URL)
//	ALERT_RETENTION_DAYS              resolved-alert retention window; unset/0 = no purge job (floor 7 days)
//	ALERT_RETENTION_INTERVAL_HOURS    purge job cadence (default 24)

const (
	// MaxAlertEmailRecipients bounds the email fan-out of one alert.
	MaxAlertEmailRecipients = 10
	// MinAlertRetention is the shortest resolved-alert retention the platform will configure; the database
	// function enforces the same floor independently.
	MinAlertRetention = 7 * 24 * time.Hour
	// DefaultAlertRetentionInterval is the purge job cadence.
	DefaultAlertRetentionInterval = 24 * time.Hour
	minAlertRetentionInterval     = time.Minute
	maxSecretRefBytes             = 512
)

func loadObservability(cfg *Config) error {
	cfg.MetricsAddr = os.Getenv("METRICS_ADDR")
	if v := os.Getenv("METRICS_ALLOW_NON_LOOPBACK"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("config: invalid METRICS_ALLOW_NON_LOOPBACK: %w", err)
		}
		cfg.MetricsAllowNonLoopback = b
	}
	if v := os.Getenv("ALERT_EMAIL_RECIPIENTS"); strings.TrimSpace(v) != "" {
		for _, r := range strings.Split(v, ",") {
			cfg.AlertEmailRecipients = append(cfg.AlertEmailRecipients, strings.TrimSpace(r))
		}
	}
	cfg.AlertWebhookURL = os.Getenv("ALERT_WEBHOOK_URL")
	cfg.AlertWebhookSecretRef = os.Getenv("ALERT_WEBHOOK_SECRET_REF")
	cfg.AlertRetentionInterval = DefaultAlertRetentionInterval
	if v := os.Getenv("ALERT_RETENTION_DAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return fmt.Errorf("config: invalid ALERT_RETENTION_DAYS %q: must be a non-negative integer", v)
		}
		cfg.AlertRetention = time.Duration(n) * 24 * time.Hour
	}
	if v := os.Getenv("ALERT_RETENTION_INTERVAL_HOURS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return fmt.Errorf("config: invalid ALERT_RETENTION_INTERVAL_HOURS %q: must be a positive integer", v)
		}
		cfg.AlertRetentionInterval = time.Duration(n) * time.Hour
	}
	return nil
}

// validateObservability is the pure validation of the fields loadObservability sets.
func (c Config) validateObservability() error {
	if err := c.validateMetricsAddr(); err != nil {
		return err
	}
	if err := c.validateAlertEmailRecipients(); err != nil {
		return err
	}
	if err := c.validateAlertWebhook(); err != nil {
		return err
	}
	if c.AlertRetention != 0 && c.AlertRetention < MinAlertRetention {
		return fmt.Errorf("config: ALERT_RETENTION_DAYS must be 0 (no purge) or at least %d", int(MinAlertRetention/(24*time.Hour)))
	}
	if c.AlertRetention != 0 && c.AlertRetentionInterval < minAlertRetentionInterval {
		return fmt.Errorf("config: ALERT_RETENTION_INTERVAL_HOURS is below the minimum interval")
	}
	return nil
}

func (c Config) validateMetricsAddr() error {
	if c.MetricsAddr == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(c.MetricsAddr)
	if err != nil || strings.ContainsAny(c.MetricsAddr, " \t\r\n/") {
		return fmt.Errorf("config: METRICS_ADDR must be host:port (for example 127.0.0.1:9464)")
	}
	pn, err := strconv.Atoi(port)
	if err != nil || pn < 1 || pn > 65535 {
		return fmt.Errorf("config: METRICS_ADDR has an invalid port")
	}
	if _, apiPort, err := net.SplitHostPort(c.HTTPAddr); err == nil && apiPort == port {
		return fmt.Errorf("config: METRICS_ADDR must not share a port with HTTP_ADDR: the metrics listener is separate from the public API")
	}
	if c.GuardEnvironment() == "production" && !c.MetricsAllowNonLoopback && !isLoopbackHost(host) {
		return fmt.Errorf("config: METRICS_ADDR binds a non-loopback address in production; bind 127.0.0.1 (scraped by a sidecar/agent) or set METRICS_ALLOW_NON_LOOPBACK=true after restricting the port to the metrics collector at the network layer (docs/runbooks/observability-and-alerting.md)")
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().IsLoopback()
}

func (c Config) validateAlertEmailRecipients() error {
	if len(c.AlertEmailRecipients) > MaxAlertEmailRecipients {
		return fmt.Errorf("config: ALERT_EMAIL_RECIPIENTS lists more than %d recipients", MaxAlertEmailRecipients)
	}
	seen := map[string]bool{}
	for _, r := range c.AlertEmailRecipients {
		if r == "" || strings.ContainsAny(r, ",;<>\r\n\t ") {
			return fmt.Errorf("config: ALERT_EMAIL_RECIPIENTS entries must be bare addresses (no display name, separators or whitespace)")
		}
		a, err := mail.ParseAddress(r)
		if err != nil || a.Address != r {
			return fmt.Errorf("config: ALERT_EMAIL_RECIPIENTS contains an entry that is not a bare email address")
		}
		key := strings.ToLower(r)
		if seen[key] {
			return fmt.Errorf("config: ALERT_EMAIL_RECIPIENTS contains a duplicate entry")
		}
		seen[key] = true
	}
	return nil
}

func (c Config) validateAlertWebhook() error {
	if c.AlertWebhookURL == "" {
		if c.AlertWebhookSecretRef != "" {
			return fmt.Errorf("config: ALERT_WEBHOOK_SECRET_REF is set without ALERT_WEBHOOK_URL")
		}
		return nil
	}
	if _, err := egressguard.ValidateURL(c.AlertWebhookURL, c.AlertWebhookURLOptions()); err != nil {
		return fmt.Errorf("config: ALERT_WEBHOOK_URL refused: %w", err)
	}
	ref := c.AlertWebhookSecretRef
	if ref == "" {
		return fmt.Errorf("config: ALERT_WEBHOOK_SECRET_REF is required with ALERT_WEBHOOK_URL (a secret-store reference, never the secret itself)")
	}
	knownScheme := strings.HasPrefix(ref, "awssm://") || strings.HasPrefix(ref, "devfile://")
	if len(ref) > maxSecretRefBytes || !knownScheme || strings.ContainsAny(ref, " \t\r\n") {
		return fmt.Errorf("config: ALERT_WEBHOOK_SECRET_REF must be an awssm:// or devfile:// secret-store reference")
	}
	return nil
}

// AlertWebhookURLOptions is the SSRF-guard option set for the webhook URL: loopback (and plain http to it)
// is admitted ONLY under an explicit "development" GuardEnvironment; every other environment, including an
// unset APP_ENV, is the strict production posture.
func (c Config) AlertWebhookURLOptions() egressguard.URLOptions {
	return egressguard.URLOptions{AllowLoopbackDev: c.GuardEnvironment() == "development"}
}
