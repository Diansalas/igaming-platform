//go:build loadtest

// Package main is the screening-capacity harness (CAP-A). TEST TOOLING ONLY: it is behind the `loadtest` build tag so it is in no
// normal build and no CI run; it works ONLY on a PRIVATE scratch database (capa_*), seeded with SYNTHETIC data through owner-path
// SQL, and never touches the shared base database. The provider is a MOCK (latency / failure injection); no real provider exists.
//
// Nothing here changes screening behaviour: it calls the production functions and the production ScreeningWorker unmodified.
package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// TenantGroup is `Count` tenants with `Persons` persons each (several groups express skew).
type TenantGroup struct {
	Count   int `json:"count"`
	Persons int `json:"persons"`
}

// ProviderCfg describes the MOCK provider's injected behaviour.
type ProviderCfg struct {
	Dist        string  `json:"dist"`         // fixed | lognormal | bimodal
	LatencyMs   float64 `json:"latency_ms"`   // fixed value / median
	Sigma       float64 `json:"sigma"`        // lognormal sigma
	SlowMs      float64 `json:"slow_ms"`      // bimodal: the slow mode
	SlowFrac    float64 `json:"slow_frac"`    // bimodal: fraction slow
	FailRate    float64 `json:"fail_rate"`    // fraction returning an error (unavailable)
	HangRate    float64 `json:"hang_rate"`    // fraction that never answers (waits for the call deadline)
	FailPersist float64 `json:"fail_persist"` // fraction of SUBJECTS that fail on every attempt (hash of the subject ref)
}

// Scenario is a versioned scenario definition (a file under scenarios/).
type Scenario struct {
	Name                  string        `json:"name"`
	Description           string        `json:"description"`
	Version               int           `json:"version"`
	Seed                  int64         `json:"seed"`
	Jurisdictions         int           `json:"jurisdictions"`
	Groups                []TenantGroup `json:"tenant_groups"`
	PolicyVersions        int           `json:"policy_versions"`  // screening policy versions per tenant and jurisdiction
	TriggerVersions       int           `json:"trigger_versions"` // trigger policy versions per tenant and jurisdiction
	IntervalSeconds       int64         `json:"rescreen_interval_seconds"`
	GateAlertAfterSeconds int64         `json:"gate_alert_after_seconds"`
	GateFirstDaysAgo      int           `json:"gate_first_days_ago"` // the first gating trigger version took effect this long ago
	// Mix is the fraction of persons per state: clear, stale, legacy, needs_officer, held, pending; the remainder is `none`.
	Mix              map[string]float64 `json:"mix"`
	DecisionFraction float64            `json:"parked_decision_fraction"` // fraction of denied persons with a parked payout decision in the last 7 days
	Provider         ProviderCfg        `json:"provider"`
	// Run parameters (the harness mode reads what it needs).
	Reps       int            `json:"reps"`
	Passes     int            `json:"passes"`
	WindowSecs int            `json:"window_secs"`
	BgGate     int            `json:"bg_gate"`     // parallel gate callers (withdrawal / payout claims)
	BgSweeper  int            `json:"bg_sweeper"`  // parallel keyset parked readers
	Workers    int            `json:"workers"`     // parallel worker loops
	ClaimBatch int            `json:"claim_batch"` // 0 = production default
	PoolConns  int            `json:"pool_conns"`  // 0 = production default (10)
	Extra      map[string]any `json:"extra"`
}

func (s *Scenario) defaults() {
	if s.Jurisdictions == 0 {
		s.Jurisdictions = 1
	}
	if s.PolicyVersions == 0 {
		s.PolicyVersions = 3
	}
	if s.TriggerVersions == 0 {
		s.TriggerVersions = 1
	}
	if s.IntervalSeconds == 0 {
		s.IntervalSeconds = 14 * 86400
	}
	if s.GateAlertAfterSeconds == 0 {
		s.GateAlertAfterSeconds = 3600
	}
	if s.GateFirstDaysAgo == 0 {
		s.GateFirstDaysAgo = 60
	}
	if s.Mix == nil {
		s.Mix = map[string]float64{"clear": 0.70, "stale": 0.15, "legacy": 0.03, "needs_officer": 0.01, "held": 0.01, "pending": 0}
	}
	if s.DecisionFraction == 0 {
		s.DecisionFraction = 0.5
	}
	if s.Reps == 0 {
		s.Reps = 5
	}
	if s.Provider.Dist == "" {
		s.Provider.Dist = "fixed"
	}
}

func loadScenario(path string) (*Scenario, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Scenario
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("scenario %s: %w", path, err)
	}
	s.defaults()
	return &s, nil
}

func (s *Scenario) totalPersons() (n int) {
	for _, g := range s.Groups {
		n += g.Count * g.Persons
	}
	return
}

// dbURL returns base (an env-supplied URL; never printed) with the database name replaced. Only the harness's own private database
// names are accepted (capa_*).
func dbURL(envName, db string) (string, error) {
	if !strings.HasPrefix(db, "capa_") {
		return "", fmt.Errorf("refusing database %q: the harness works only on private capa_* databases", db)
	}
	base := os.Getenv(envName)
	if base == "" {
		return "", fmt.Errorf("%s is not set", envName)
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parse %s: invalid URL", envName)
	}
	u.Path = "/" + db
	q := u.Query()
	q.Set("sslmode", "disable")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func ownerURL(db string) (string, error)   { return dbURL("DATABASE_URL", db) }
func runtimeURL(db string) (string, error) { return dbURL("TEST_RUNTIME_DATABASE_URL", db) }

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }
