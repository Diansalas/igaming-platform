package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// ALERT-DELIVERY-1 (ADR 0102 section 18): the dispatcher is constructed before
// the HTTP server so the status endpoint reports its readiness, the production
// guard is derived from GuardEnvironment() (a missing APP_ENV counts as
// production), the recording/test channel is never wired, and /readyz stays
// report-only (no readiness coupling).
func TestMain_AlertRoutingWiring(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if !regexp.MustCompile(`AlertRouting:\s+alertDispatcher,`).MatchString(src) {
		t.Error("Deps.AlertRouting must be the dispatcher built in run()")
	}
	if !regexp.MustCompile(`AlertRoutingProduction:\s+cfg\.GuardEnvironment\(\) == "production"`).MatchString(src) {
		t.Error("Deps.AlertRoutingProduction must come from cfg.GuardEnvironment(), never cfg.Environment")
	}
	dispIdx := strings.Index(src, "alertDispatcher := alerting.NewDispatcher(")
	depsIdx := strings.Index(src, "httpserver.NewWithAdmission(")
	if dispIdx < 0 || depsIdx < 0 || dispIdx > depsIdx {
		t.Error("the dispatcher must be constructed before the HTTP server Deps that reference it")
	}
	for _, banned := range []string{"alertingtest", "RecordingChannel", "ALERT_MOCK_CHANNEL"} {
		if strings.Contains(src, banned) {
			t.Errorf("main.go must not reference %s: the test channel is tests-only (security S1 declined dev wiring)", banned)
		}
	}
}
