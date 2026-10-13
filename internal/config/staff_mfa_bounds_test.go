package config

import (
	"testing"
	"time"
)

// validateStaffMFA itself enforces the ADR 0119 section 7.4 bounds,
// independently of the integer pre-check in loadStaffMFA, so a Config
// assembled elsewhere is still refused.
func TestStaffMFA_ValidateBoundsDirectly(t *testing.T) {
	good := Config{StaffMFAIssuer: "Ops", StaffAccessTokenTTL: 15 * time.Minute, StaffRefreshIdleTTL: 30 * time.Minute,
		StaffSessionAbsoluteTTL: 12 * time.Hour, StaffStepUpCriticalMaxAge: 5 * time.Minute, StaffStepUpFinancialMaxAge: 10 * time.Minute}
	if err := good.validateStaffMFA(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(c *Config){
		"access":    func(c *Config) { c.StaffAccessTokenTTL = 16 * time.Minute },
		"idle":      func(c *Config) { c.StaffRefreshIdleTTL = 0 },
		"absolute":  func(c *Config) { c.StaffSessionAbsoluteTTL = 13 * time.Hour },
		"critical":  func(c *Config) { c.StaffStepUpCriticalMaxAge = 6 * time.Minute },
		"financial": func(c *Config) { c.StaffStepUpFinancialMaxAge = -time.Minute },
	} {
		c := good
		mutate(&c)
		if err := c.validateStaffMFA(); err == nil {
			t.Fatalf("%s out of bounds must be refused", name)
		}
	}
}
