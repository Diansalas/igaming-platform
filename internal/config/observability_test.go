package config

import (
	"strings"
	"testing"
	"time"
)

func obsCfg(env string, explicit bool) Config {
	return Config{Environment: env, EnvironmentExplicit: explicit, HTTPAddr: ":8080"}
}

func TestValidateMetricsAddr(t *testing.T) {
	type tc struct {
		name     string
		env      string
		explicit bool
		addr     string
		allow    bool
		wantErr  string
	}
	for _, c := range []tc{
		{name: "disabled when empty", env: "production", explicit: true, addr: ""},
		{name: "dev wildcard ok", env: "development", explicit: true, addr: ":9464"},
		{name: "dev loopback ok", env: "development", explicit: true, addr: "127.0.0.1:9464"},
		// SR9-A9-9 (CHANGED ASSERTION): staging used to accept a non-loopback bind without acknowledgement.
		{name: "staging wildcard refused", env: "staging", explicit: true, addr: "0.0.0.0:9464", wantErr: "non-loopback"},
		{name: "staging private refused", env: "staging", explicit: true, addr: "10.0.0.5:9464", wantErr: "non-loopback"},
		{name: "staging wildcard acknowledged", env: "staging", explicit: true, addr: "0.0.0.0:9464", allow: true},
		{name: "staging loopback ok", env: "staging", explicit: true, addr: "127.0.0.1:9464"},
		{name: "unknown env wildcard refused", env: "qa", explicit: true, addr: ":9464", wantErr: "non-loopback"},
		{name: "prod loopback ok", env: "production", explicit: true, addr: "127.0.0.1:9464"},
		{name: "prod ipv6 loopback ok", env: "production", explicit: true, addr: "[::1]:9464"},
		{name: "prod localhost ok", env: "production", explicit: true, addr: "localhost:9464"},
		{name: "prod wildcard refused", env: "production", explicit: true, addr: ":9464", wantErr: "non-loopback"},
		{name: "prod 0.0.0.0 refused", env: "production", explicit: true, addr: "0.0.0.0:9464", wantErr: "non-loopback"},
		{name: "prod private refused", env: "production", explicit: true, addr: "10.0.0.5:9464", wantErr: "non-loopback"},
		{name: "prod wildcard acknowledged", env: "production", explicit: true, addr: ":9464", allow: true},
		{name: "unset APP_ENV is treated as production", env: "development", explicit: false, addr: ":9464", wantErr: "non-loopback"},
		{name: "same as API addr refused", env: "development", explicit: true, addr: ":8080", wantErr: "HTTP_ADDR"},
		{name: "same port other host refused", env: "development", explicit: true, addr: "127.0.0.1:8080", wantErr: "HTTP_ADDR"},
		{name: "no port", env: "development", explicit: true, addr: "127.0.0.1", wantErr: "host:port"},
		{name: "bad port", env: "development", explicit: true, addr: "127.0.0.1:99999", wantErr: "port"},
		{name: "port zero", env: "development", explicit: true, addr: "127.0.0.1:0", wantErr: "port"},
		{name: "whitespace", env: "development", explicit: true, addr: " 127.0.0.1:9464", wantErr: "host:port"},
		{name: "url form", env: "development", explicit: true, addr: "http://127.0.0.1:9464", wantErr: "host:port"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := obsCfg(c.env, c.explicit)
			cfg.MetricsAddr, cfg.MetricsAllowNonLoopback = c.addr, c.allow
			err := cfg.validateObservability()
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Fatalf("error = %v, want containing %q", err, c.wantErr)
			}
		})
	}
}

func TestValidateAlertEmailRecipients(t *testing.T) {
	good := [][]string{{"ops@example.com"}, {"a@example.com", "b@example.org"}}
	for _, g := range good {
		cfg := obsCfg("development", true)
		cfg.AlertEmailRecipients = g
		if err := cfg.validateObservability(); err != nil {
			t.Errorf("%v: %v", g, err)
		}
	}
	bad := map[string][]string{
		"display name":     {"Ops <ops@example.com>"},
		"not an address":   {"ops"},
		"header injection": {"ops@example.com\r\nBcc: x@y.z"},
		"empty":            {""},
		"duplicate":        {"ops@example.com", "OPS@example.com"},
		"too many":         {"a@e.com", "b@e.com", "c@e.com", "d@e.com", "e@e.com", "f@e.com", "g@e.com", "h@e.com", "i@e.com", "j@e.com", "k@e.com"},
		"comma inside":     {"a@e.com,b@e.com"},
		"rfc5322 comment":  {"ops@example.com(owner)"},
	}
	for name, b := range bad {
		cfg := obsCfg("development", true)
		cfg.AlertEmailRecipients = b
		if err := cfg.validateObservability(); err == nil {
			t.Errorf("%s: accepted %v", name, b)
		}
	}
}

func TestValidateAlertWebhook(t *testing.T) {
	prod := obsCfg("production", true)
	prod.AlertWebhookURL = "https://hooks.example.com/alerts"
	if err := prod.validateObservability(); err == nil || !strings.Contains(err.Error(), "ALERT_WEBHOOK_SECRET_REF is required") {
		t.Fatalf("a webhook URL without a secret reference must be refused as required, got %v", err)
	}
	prod.AlertWebhookSecretRef = "awssm://arn:aws:secretsmanager:eu-west-1:123456789012:secret:alert-webhook?versionId=11111111111111111111111111111111"
	if err := prod.validateObservability(); err != nil {
		t.Fatalf("valid production webhook: %v", err)
	}

	for _, u := range []string{"http://hooks.example.com/a", "https://127.0.0.1/a", "https://10.0.0.1/a", "https://user:pw@h.example.com/a", "https://localhost/a"} {
		cfg := prod
		cfg.AlertWebhookURL = u
		if err := cfg.validateObservability(); err == nil {
			t.Errorf("production accepted %q", u)
		}
	}

	dev := obsCfg("development", true)
	dev.AlertWebhookURL = "http://127.0.0.1:8099/hook"
	dev.AlertWebhookSecretRef = "devfile://alert-webhook?version=v1"
	if err := dev.validateObservability(); err != nil {
		t.Fatalf("development loopback webhook: %v", err)
	}
	// An unset APP_ENV is the production posture: the dev loopback allowance must not apply.
	unset := dev
	unset.EnvironmentExplicit = false
	if err := unset.validateObservability(); err == nil {
		t.Fatal("loopback webhook accepted under GuardEnvironment()==production")
	}

	// SR9-A9-9: a single-label host such as "metadata" is refused unless explicitly allow-listed.
	meta := prod
	meta.AlertWebhookURL = "https://metadata/computeMetadata/v1/"
	if err := meta.validateObservability(); err == nil {
		t.Error("a single-label webhook host was accepted without an allow-list")
	}
	meta.AlertWebhookAllowHosts = []string{"hooks"}
	if err := meta.validateObservability(); err == nil {
		t.Error("a single-label host not on the allow-list was accepted")
	}
	meta.AlertWebhookURL = "https://hooks/a"
	if err := meta.validateObservability(); err != nil {
		t.Errorf("an allow-listed single-label host was refused: %v", err)
	}
	opts := meta.AlertWebhookURLOptions()
	if len(opts.AllowSingleLabelHosts) != 1 || opts.AllowNAT64 || opts.AllowLoopbackDev {
		t.Errorf("url options = %+v", opts)
	}
	nat := prod
	nat.AlertWebhookURL = "https://[64:ff9b::808:808]/a"
	if err := nat.validateObservability(); err == nil {
		t.Error("a NAT64 literal was accepted by default")
	}
	nat.AlertWebhookAllowNAT64 = true
	if err := nat.validateObservability(); err != nil {
		t.Errorf("a NAT64 literal with a public embedded v4 was refused when allowed: %v", err)
	}

	// The secret reference must be a syntactically valid store reference, and never a literal secret.
	bad := prod
	bad.AlertWebhookSecretRef = "a-literal-shared-secret-value"
	if err := bad.validateObservability(); err == nil {
		t.Fatal("a literal secret in the reference field was accepted")
	}
	// A secret reference without a URL is a misconfiguration worth refusing (nothing would use it).
	orphan := obsCfg("development", true)
	orphan.AlertWebhookSecretRef = "devfile://alert-webhook?version=v1"
	if err := orphan.validateObservability(); err == nil {
		t.Fatal("secret reference without ALERT_WEBHOOK_URL accepted")
	}
}

func TestValidateAlertRetention(t *testing.T) {
	cfg := obsCfg("development", true)
	if err := cfg.validateObservability(); err != nil {
		t.Fatalf("retention disabled (zero) must be valid: %v", err)
	}
	cfg.AlertRetentionInterval = time.Hour // a valid interval, so ONLY the window can be the reason for the refusal
	cfg.AlertRetention = 24 * time.Hour
	if err := cfg.validateObservability(); err == nil || !strings.Contains(err.Error(), "ALERT_RETENTION_DAYS") {
		t.Fatalf("a retention window below the 7-day floor must be refused naming ALERT_RETENTION_DAYS, got %v", err)
	}
	cfg.AlertRetention = 7*24*time.Hour - time.Second
	if err := cfg.validateObservability(); err == nil {
		t.Fatal("a window one second under the floor was accepted")
	}
	cfg.AlertRetention = 7 * 24 * time.Hour
	cfg.AlertRetentionInterval = time.Hour
	if err := cfg.validateObservability(); err != nil {
		t.Fatal(err)
	}
	cfg.AlertRetentionInterval = time.Second
	if err := cfg.validateObservability(); err == nil || !strings.Contains(err.Error(), "ALERT_RETENTION_INTERVAL_HOURS") {
		t.Fatalf("a sub-minute retention interval must be refused naming ALERT_RETENTION_INTERVAL_HOURS, got %v", err)
	}
}

func TestLoadObservabilityFromEnv(t *testing.T) {
	t.Setenv("METRICS_ADDR", "127.0.0.1:9464")
	t.Setenv("METRICS_ALLOW_NON_LOOPBACK", "true")
	t.Setenv("ALERT_EMAIL_RECIPIENTS", " a@example.com , b@example.com ")
	t.Setenv("ALERT_WEBHOOK_URL", "https://hooks.example.com/a")
	t.Setenv("ALERT_WEBHOOK_SECRET_REF", "devfile://alert-webhook?version=v1")
	t.Setenv("ALERT_RETENTION_DAYS", "400")
	t.Setenv("ALERT_RETENTION_INTERVAL_HOURS", "12")
	t.Setenv("ALERT_WEBHOOK_ALLOW_HOSTS", " hooks , Pager ")
	t.Setenv("ALERT_WEBHOOK_ALLOW_NAT64", "true")
	var cfg Config
	if err := loadObservability(&cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.AlertWebhookAllowHosts) != 2 || cfg.AlertWebhookAllowHosts[0] != "hooks" || cfg.AlertWebhookAllowHosts[1] != "pager" || !cfg.AlertWebhookAllowNAT64 {
		t.Errorf("webhook egress options: %v %v", cfg.AlertWebhookAllowHosts, cfg.AlertWebhookAllowNAT64)
	}
	if cfg.MetricsAddr != "127.0.0.1:9464" || !cfg.MetricsAllowNonLoopback {
		t.Errorf("metrics: %+v", cfg)
	}
	if len(cfg.AlertEmailRecipients) != 2 || cfg.AlertEmailRecipients[0] != "a@example.com" {
		t.Errorf("recipients = %v", cfg.AlertEmailRecipients)
	}
	if cfg.AlertWebhookURL != "https://hooks.example.com/a" || cfg.AlertWebhookSecretRef == "" {
		t.Errorf("webhook: %q %q", cfg.AlertWebhookURL, cfg.AlertWebhookSecretRef)
	}
	if cfg.AlertRetention != 400*24*time.Hour || cfg.AlertRetentionInterval != 12*time.Hour {
		t.Errorf("retention = %v every %v", cfg.AlertRetention, cfg.AlertRetentionInterval)
	}

	for k, v := range map[string]string{"ALERT_RETENTION_DAYS": "x", "ALERT_RETENTION_INTERVAL_HOURS": "-1", "METRICS_ALLOW_NON_LOOPBACK": "maybe", "ALERT_WEBHOOK_ALLOW_NAT64": "maybe"} {
		t.Run("bad "+k, func(t *testing.T) {
			t.Setenv(k, v)
			var c Config
			if err := loadObservability(&c); err == nil {
				t.Fatalf("%s=%q accepted", k, v)
			}
		})
	}
}

// Nothing in the observability configuration may render a secret: the webhook secret is a REFERENCE and
// the Config String methods must not print recipients or the reference.
func TestObservabilityConfigDefaultsAreDisabled(t *testing.T) {
	t.Setenv("METRICS_ADDR", "")
	var cfg Config
	if err := loadObservability(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.MetricsAddr != "" || len(cfg.AlertEmailRecipients) != 0 || cfg.AlertWebhookURL != "" || cfg.AlertRetention != 0 {
		t.Fatalf("everything must default to disabled: %+v", cfg)
	}
}
