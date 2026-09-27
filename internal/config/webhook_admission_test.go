package config

import "testing"

// TestWebhookAdmissionDefaultsValid: the ADR 0097 §9.1 technical
// defaults, at a representative pool size, pass §9.3 validation as-is.
func TestWebhookAdmissionDefaultsValid(t *testing.T) {
	cfg := defaultWebhookAdmissionConfig(10)
	if err := cfg.Validate(false, 10); err != nil {
		t.Fatalf("defaults must validate cleanly: %v", err)
	}
	if err := cfg.Validate(true, 10); err != nil {
		t.Fatalf("defaults must validate cleanly in production too: %v", err)
	}
}

// TestWebhookAdmissionProductionCannotDisable is T12/§9.3/§7.
func TestWebhookAdmissionProductionCannotDisable(t *testing.T) {
	cfg := defaultWebhookAdmissionConfig(10)
	cfg.Enabled = false
	if err := cfg.Validate(true, 10); err == nil {
		t.Fatal("production must refuse WEBHOOK_ADMISSION_ENABLED=false")
	}
	if err := cfg.Validate(false, 10); err != nil {
		t.Fatalf("non-production may disable: %v", err)
	}
}

// TestWebhookAdmissionValidationEachViolation walks every §9.3 rule and
// confirms Validate fails for it (T12).
func TestWebhookAdmissionValidationEachViolation(t *testing.T) {
	base := func() WebhookAdmissionConfig { return defaultWebhookAdmissionConfig(10) }

	cases := map[string]func(c *WebhookAdmissionConfig){
		"pre-auth rate zero":        func(c *WebhookAdmissionConfig) { c.PreAuthRate["payments"] = 0 },
		"pre-auth burst zero":       func(c *WebhookAdmissionConfig) { c.PreAuthBurst["payments"] = 0 },
		"pre-auth below verified":   func(c *WebhookAdmissionConfig) { c.PreAuthRate["payments"] = 1 },
		"verified rate zero":        func(c *WebhookAdmissionConfig) { c.VerifiedRate["payments"] = 0 },
		"per-IP negative":           func(c *WebhookAdmissionConfig) { c.PerIPRPS = -1 },
		"per-IP burst zero on":      func(c *WebhookAdmissionConfig) { c.PerIPRPS = 5; c.PerIPBurst = 0 },
		"inflight key > global":     func(c *WebhookAdmissionConfig) { c.InFlightPerKey = c.InFlightGlobal + 1 },
		"db gate >= N":              func(c *WebhookAdmissionConfig) { c.DBGateGlobal = 10 },
		"B2 >= N":                   func(c *WebhookAdmissionConfig) { c.DomainTxPerTenant = 10 },
		"db gate wait non-positive": func(c *WebhookAdmissionConfig) { c.DBGateWait = 0 },
		"directory cap zero":        func(c *WebhookAdmissionConfig) { c.DirectoryCap = 0 },
		"verified max keys zero":    func(c *WebhookAdmissionConfig) { c.VerifiedMaxKeys = 0 },
		"override unknown domain": func(c *WebhookAdmissionConfig) {
			c.Overrides = []WebhookAdmissionOverride{{Domain: "sportsbook", ProviderID: "x", Rate: 1, Burst: 1}}
		},
		"override bad provider id": func(c *WebhookAdmissionConfig) {
			c.Overrides = []WebhookAdmissionOverride{{Domain: "payments", ProviderID: "Not Valid!", Rate: 1, Burst: 1}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := base()
			mutate(&cfg)
			if err := cfg.Validate(false, 10); err == nil {
				t.Fatalf("%s: expected Validate to fail", name)
			}
		})
	}
}

func TestLoadWebhookAdmissionConfigFromEnv(t *testing.T) {
	t.Setenv("WEBHOOK_RL_PER_IP_RPS", "5")
	t.Setenv("WEBHOOK_RL_PER_IP_BURST", "20")
	t.Setenv("WEBHOOK_ADMISSION_OVERRIDES", `[{"domain":"payments","provider_id":"acme","rate":40,"burst":150}]`)
	cfg, err := loadWebhookAdmissionConfig(10)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PerIPRPS != 5 || cfg.PerIPBurst != 20 {
		t.Fatalf("per-IP env not applied: %+v", cfg)
	}
	if len(cfg.Overrides) != 1 || cfg.Overrides[0].ProviderID != "acme" {
		t.Fatalf("overrides not applied: %+v", cfg.Overrides)
	}
}

func TestLoadWebhookAdmissionConfigInvalidOverridesJSON(t *testing.T) {
	t.Setenv("WEBHOOK_ADMISSION_OVERRIDES", "not json")
	if _, err := loadWebhookAdmissionConfig(10); err == nil {
		t.Fatal("invalid JSON must fail to load")
	}
}
