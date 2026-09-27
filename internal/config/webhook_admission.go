package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"
)

// WebhookDomains are the three provider-facing webhook domains ADR 0097
// governs. Fixed, closed set - never derived from request input.
var WebhookDomains = []string{"payments", "casino", "kyc"}

// RateBurst is one GCRA (rate, burst) pair (ADR 0097 §5.1).
type RateBurst struct {
	Rate  float64
	Burst int
}

// WebhookAdmissionOverride is one entry of WEBHOOK_ADMISSION_OVERRIDES
// (ADR 0097 §9.2): a platform-operator override of the A3 or B1 rate/
// burst for one domain + provider_id, optionally narrowed to one tenant.
// Tenant is a slug for the A3 (pre-auth) tier and a tenant id string for
// the B1 (verified) tier - httpserver resolves which applies. This is
// NOT tenant/brand configuration (CLAUDE.md's "brand differences are
// config rows" rule does not apply here, per ADR 0097 §9.2) - it is
// platform-operator configuration delivered via env, never partner-
// console editable.
type WebhookAdmissionOverride struct {
	Domain     string  `json:"domain"`
	ProviderID string  `json:"provider_id"`
	Tenant     string  `json:"tenant,omitempty"`
	Rate       float64 `json:"rate"`
	Burst      int     `json:"burst"`
}

// WebhookAdmissionConfig is ADR 0097's fully-resolved, validated
// (§9.3) configuration. cmd/platform-api/main.go maps this into
// httpserver.Deps; internal/httpserver builds the actual
// admission.GCRALimiter/admission.Bulkhead instances from it once, at
// startup.
type WebhookAdmissionConfig struct {
	// Enabled gates the whole admission layer. §7's fail-safe table:
	// disabling is accepted only outside production (Validate refuses a
	// production Config with Enabled=false).
	Enabled bool

	// PerIPRPS/PerIPBurst are A2, off by default (rate 0) per §4.4.
	PerIPRPS   float64
	PerIPBurst int

	// PreAuthRate/PreAuthBurst are A3's per-domain known-key rate/burst;
	// PreAuthUnknownRate/PreAuthUnknownBurst are the shared "_unknown"
	// bucket per domain (§9.1).
	PreAuthRate         map[string]float64
	PreAuthBurst        map[string]int
	PreAuthUnknownRate  map[string]float64
	PreAuthUnknownBurst map[string]int

	// VerifiedRate/VerifiedBurst are B1's per-domain rate/burst for a
	// verified (tenant_id, provider_id) key (§9.1).
	VerifiedRate  map[string]float64
	VerifiedBurst map[string]int

	// InFlightGlobal/PerKey/Unknown are A4a (§9.1: 64/16/4).
	InFlightGlobal  int
	InFlightPerKey  int
	InFlightUnknown int

	// DBGateGlobal/PerKey/Unknown/Wait are A4b (§9.1: W_db=max(1,floor(0.3N))
	// / min(2,W_db) / 1 / 100ms).
	DBGateGlobal  int
	DBGatePerKey  int
	DBGateUnknown int
	DBGateWait    time.Duration

	// DomainTxPerTenant/Wait are B2 (§9.1: max(1,floor(0.3N)) / 2s).
	DomainTxPerTenant int
	DomainWait        time.Duration

	// BodyReadTimeout is A5's per-request read deadline (§9.1: 10s).
	BodyReadTimeout time.Duration

	// DirectoryRefresh/DirectoryCap govern the webhook tenant directory
	// (§4.2: 30s / 10000).
	DirectoryRefresh time.Duration
	DirectoryCap     int

	// IdleEvict/VerifiedMaxKeys/PerIPMaxKeys bound limiter memory (§9.1:
	// 10min / 30000 / 50000).
	IdleEvict       time.Duration
	VerifiedMaxKeys int
	PerIPMaxKeys    int

	Overrides []WebhookAdmissionOverride
}

// defaultWebhookAdmissionConfig computes ADR 0097 §9.1's technical
// defaults from the pool size N (DatabaseMaxConns). These are
// deliberately NOT independently exposed as one env var per knob - §9.1
// ties W_db/B2's cap to N by formula, and PRH-I4 is explicit that these
// numbers are a non-gating benchmark to be revisited before any real-
// provider gate, not a fixed operational contract. The operator-facing
// knobs are Enabled, the A2 per-IP tier, and Overrides (WEBHOOK_RL_*/
// WEBHOOK_ADMISSION_OVERRIDES below) - the ones §9.2 actually calls out
// as platform-operator configuration.
func defaultWebhookAdmissionConfig(n int32) WebhookAdmissionConfig {
	wdb := int(0.3 * float64(n))
	if wdb < 1 {
		wdb = 1
	}
	wdbPerKey := 2
	if wdb < wdbPerKey {
		wdbPerKey = wdb
	}
	domainTx := wdb // same formula, same value, per §9.1's table

	return WebhookAdmissionConfig{
		Enabled:    true,
		PerIPRPS:   0, // off by default, §4.4
		PerIPBurst: 1,

		PreAuthRate:         map[string]float64{"payments": 50, "casino": 300, "kyc": 10},
		PreAuthBurst:        map[string]int{"payments": 200, "casino": 1000, "kyc": 50},
		PreAuthUnknownRate:  map[string]float64{"payments": 2, "casino": 2, "kyc": 2},
		PreAuthUnknownBurst: map[string]int{"payments": 10, "casino": 10, "kyc": 10},

		VerifiedRate:  map[string]float64{"payments": 25, "casino": 200, "kyc": 5},
		VerifiedBurst: map[string]int{"payments": 100, "casino": 800, "kyc": 50},

		InFlightGlobal:  64,
		InFlightPerKey:  16,
		InFlightUnknown: 4,

		DBGateGlobal:  wdb,
		DBGatePerKey:  wdbPerKey,
		DBGateUnknown: 1,
		DBGateWait:    100 * time.Millisecond,

		DomainTxPerTenant: domainTx,
		DomainWait:        2 * time.Second,

		BodyReadTimeout: 10 * time.Second,

		DirectoryRefresh: 30 * time.Second,
		DirectoryCap:     10000,

		IdleEvict:       10 * time.Minute,
		VerifiedMaxKeys: 30000,
		PerIPMaxKeys:    50000,
	}
}

// loadWebhookAdmissionConfig reads the operator-facing env vars on top of
// defaultWebhookAdmissionConfig(databaseMaxConns).
func loadWebhookAdmissionConfig(databaseMaxConns int32) (WebhookAdmissionConfig, error) {
	cfg := defaultWebhookAdmissionConfig(databaseMaxConns)

	if v := os.Getenv("WEBHOOK_ADMISSION_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return WebhookAdmissionConfig{}, fmt.Errorf("config: invalid WEBHOOK_ADMISSION_ENABLED: %w", err)
		}
		cfg.Enabled = b
	}
	if v := os.Getenv("WEBHOOK_RL_PER_IP_RPS"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return WebhookAdmissionConfig{}, fmt.Errorf("config: invalid WEBHOOK_RL_PER_IP_RPS: %w", err)
		}
		cfg.PerIPRPS = f
	}
	if v := os.Getenv("WEBHOOK_RL_PER_IP_BURST"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return WebhookAdmissionConfig{}, fmt.Errorf("config: invalid WEBHOOK_RL_PER_IP_BURST: %w", err)
		}
		cfg.PerIPBurst = n
	}
	if v := os.Getenv("WEBHOOK_ADMISSION_OVERRIDES"); v != "" {
		var overrides []WebhookAdmissionOverride
		if err := json.Unmarshal([]byte(v), &overrides); err != nil {
			return WebhookAdmissionConfig{}, fmt.Errorf("config: invalid WEBHOOK_ADMISSION_OVERRIDES JSON: %w", err)
		}
		cfg.Overrides = overrides
	}
	return cfg, nil
}

func isKnownWebhookDomain(d string) bool {
	for _, wd := range WebhookDomains {
		if wd == d {
			return true
		}
	}
	return false
}

// validProviderIDCharset mirrors webhookauth.ValidProviderID's charset
// rule (lowercase alnum + underscore/hyphen, bounded length) without
// importing internal/webhookauth from internal/config (config stays a
// leaf that produces plain values; see ADR 0097 §20 AC1's "internal/
// config produces plain values, httpserver maps them to admission
// types").
func validProviderIDCharset(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// Validate applies ADR 0097 §9.3's fail-closed startup checks. production
// is the caller's already-resolved GuardEnvironment() value (see
// providerkind.RefuseSyntheticInProduction's identical pattern for why
// the caller, not this function, resolves ambiguity around a missing
// APP_ENV).
func (c WebhookAdmissionConfig) Validate(production bool, databaseMaxConns int32) error {
	if production && !c.Enabled {
		return fmt.Errorf("config: WEBHOOK_ADMISSION_ENABLED=false is not permitted in production (ADR 0097 §7/§9.3)")
	}
	if !c.Enabled {
		return nil
	}
	for _, d := range WebhookDomains {
		rate, ok := c.PreAuthRate[d]
		if !ok || rate <= 0 {
			return fmt.Errorf("config: webhook admission: %s pre-auth rate must be > 0", d)
		}
		burst := c.PreAuthBurst[d]
		if burst < 1 {
			return fmt.Errorf("config: webhook admission: %s pre-auth burst must be >= 1", d)
		}
		uRate, ok := c.PreAuthUnknownRate[d]
		if !ok || uRate <= 0 {
			return fmt.Errorf("config: webhook admission: %s pre-auth unknown rate must be > 0", d)
		}
		if c.PreAuthUnknownBurst[d] < 1 {
			return fmt.Errorf("config: webhook admission: %s pre-auth unknown burst must be >= 1", d)
		}
		vRate, ok := c.VerifiedRate[d]
		if !ok || vRate <= 0 {
			return fmt.Errorf("config: webhook admission: %s verified rate must be > 0", d)
		}
		if c.VerifiedBurst[d] < 1 {
			return fmt.Errorf("config: webhook admission: %s verified burst must be >= 1", d)
		}
		// §9.3: A3 rate >= B1 rate for the same domain, otherwise
		// legitimate traffic competes with attackers at A3.
		if rate < vRate {
			return fmt.Errorf("config: webhook admission: %s pre-auth rate (%.2f) must be >= verified rate (%.2f)", d, rate, vRate)
		}
	}
	if c.PerIPRPS < 0 {
		return fmt.Errorf("config: webhook admission: per-IP rate must not be negative")
	}
	if c.PerIPRPS > 0 && c.PerIPBurst < 1 {
		return fmt.Errorf("config: webhook admission: per-IP burst must be >= 1 when the per-IP tier is enabled")
	}
	checkBulkhead := func(name string, global, key, unknown int) error {
		if global < 1 {
			return fmt.Errorf("config: webhook admission: %s global cap must be >= 1", name)
		}
		if key < 1 || key > global {
			return fmt.Errorf("config: webhook admission: %s per-key cap must be in (0, global]", name)
		}
		if unknown < 1 || unknown > global {
			return fmt.Errorf("config: webhook admission: %s unknown-bucket cap must be in (0, global]", name)
		}
		return nil
	}
	if err := checkBulkhead("A4a in-flight", c.InFlightGlobal, c.InFlightPerKey, c.InFlightUnknown); err != nil {
		return err
	}
	if err := checkBulkhead("A4b db gate", c.DBGateGlobal, c.DBGatePerKey, c.DBGateUnknown); err != nil {
		return err
	}
	if c.DomainTxPerTenant < 1 {
		return fmt.Errorf("config: webhook admission: B2 per-tenant domain-tx cap must be >= 1")
	}
	// §9.3: W_db < N and B2 < N - unauthenticated/webhook work must never
	// be able to claim the WHOLE pool.
	if databaseMaxConns > 0 {
		if c.DBGateGlobal >= int(databaseMaxConns) {
			return fmt.Errorf("config: webhook admission: A4b db gate global cap (%d) must be < DatabaseMaxConns (%d)", c.DBGateGlobal, databaseMaxConns)
		}
		if c.DomainTxPerTenant >= int(databaseMaxConns) {
			return fmt.Errorf("config: webhook admission: B2 per-tenant domain-tx cap (%d) must be < DatabaseMaxConns (%d)", c.DomainTxPerTenant, databaseMaxConns)
		}
	}
	if c.DBGateWait <= 0 || c.DomainWait <= 0 || c.BodyReadTimeout <= 0 {
		return fmt.Errorf("config: webhook admission: DBGateWait, DomainWait and BodyReadTimeout must all be positive")
	}
	if c.DirectoryRefresh <= 0 || c.DirectoryCap < 1 {
		return fmt.Errorf("config: webhook admission: directory refresh interval and cap must be positive")
	}
	if c.VerifiedMaxKeys < 1 || c.PerIPMaxKeys < 1 {
		return fmt.Errorf("config: webhook admission: VerifiedMaxKeys and PerIPMaxKeys must be positive")
	}
	for _, o := range c.Overrides {
		if !isKnownWebhookDomain(o.Domain) {
			return fmt.Errorf("config: webhook admission override references unknown domain %q", o.Domain)
		}
		if !validProviderIDCharset(o.ProviderID) {
			return fmt.Errorf("config: webhook admission override references a charset-invalid provider_id %q", o.ProviderID)
		}
		if o.Rate <= 0 || o.Burst < 1 {
			return fmt.Errorf("config: webhook admission override for %s/%s must have rate > 0 and burst >= 1", o.Domain, o.ProviderID)
		}
	}
	return nil
}
