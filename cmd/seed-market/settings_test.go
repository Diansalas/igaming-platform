package main

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

var okArgs = []string{"-tenant-slug", "demo", "-brand-slug", "demo-casino", "-country", "SV", "-authorization-reference", "TICKET-1", "-operator", "ops", "-reason", "staging-bootstrap"}

func okEnv() map[string]string {
	return map[string]string{"APP_ENV": "staging", "DATABASE_URL": "postgres://igaming@db:5432/x?sslmode=require", "MOCK_GEOLOCATION_COUNTRY": "SV"}
}

func TestParseSettings_AcceptsAPlainStagingInvocation(t *testing.T) {
	s, err := parseSettings(okArgs, env(okEnv()))
	if err != nil {
		t.Fatalf("a plain staging invocation must pass: %v", err)
	}
	if s.country != "SV" || s.tenantSlug != "demo" || s.brandSlug != "demo-casino" || s.authRef != "TICKET-1" {
		t.Fatalf("unexpected settings: %+v", s)
	}
}

func TestParseSettings_RefusesProductionAndUnknownEnvironments(t *testing.T) {
	for _, appEnv := range []string{"production", "", "prod", "test", "Staging"} {
		e := okEnv()
		e["APP_ENV"] = appEnv
		if _, err := parseSettings(okArgs, env(e)); err == nil {
			t.Errorf("APP_ENV=%q must be refused", appEnv)
		}
	}
	e := okEnv()
	e["APP_ENV"] = "production"
	_, err := parseSettings(okArgs, env(e))
	if err == nil || !strings.Contains(err.Error(), "production") {
		t.Fatalf("production must be named in the refusal, got %v", err)
	}
	e["APP_ENV"] = "development"
	if _, err := parseSettings(okArgs, env(e)); err != nil {
		t.Fatalf("development must be accepted: %v", err)
	}
}

func TestParseSettings_EveryMandatoryFlagIsMandatory(t *testing.T) {
	for _, drop := range []string{"-tenant-slug", "-brand-slug", "-authorization-reference", "-operator", "-reason", "-country"} {
		var args []string
		for i := 0; i < len(okArgs); i += 2 {
			if okArgs[i] != drop {
				args = append(args, okArgs[i], okArgs[i+1])
			}
		}
		if _, err := parseSettings(args, env(okEnv())); err == nil {
			t.Errorf("%s must be required", drop)
		}
	}
}

func TestParseSettings_CountryRules(t *testing.T) {
	for _, bad := range []string{"sv", "SVX", "ZZ1", "1S", "", "XX"} {
		args := append(append([]string{}, okArgs...), "-country", bad)
		e := okEnv()
		delete(e, "MOCK_GEOLOCATION_COUNTRY")
		if _, err := parseSettings(args, env(e)); err == nil {
			t.Errorf("country %q must be refused", bad)
		}
	}
	args := append(append([]string{}, okArgs...), "-country", "DE")
	if _, err := parseSettings(args, env(okEnv())); err == nil {
		t.Fatal("a country different from MOCK_GEOLOCATION_COUNTRY must be refused")
	}
	e := okEnv()
	delete(e, "MOCK_GEOLOCATION_COUNTRY")
	if _, err := parseSettings(args, env(e)); err != nil {
		t.Fatalf("without MOCK_GEOLOCATION_COUNTRY any valid code is accepted: %v", err)
	}
}

func TestParseSettings_AuditFieldsRefuseUnsafeText(t *testing.T) {
	for _, bad := range []string{"a\nb", "a‮b", " lead", "trail ", "a​b", strings.Repeat("x", maxField+1)} {
		for _, flagName := range []string{"-operator", "-reason", "-authorization-reference"} {
			args := append(append([]string{}, okArgs...), flagName, bad)
			if _, err := parseSettings(args, env(okEnv())); err == nil {
				t.Errorf("%s=%q must be refused", flagName, bad)
			}
		}
	}
}

func TestParseSettings_ActorIDAndDatabaseURL(t *testing.T) {
	if _, err := parseSettings(append(append([]string{}, okArgs...), "-actor-id", "not-a-uuid"), env(okEnv())); err == nil {
		t.Error("a malformed -actor-id must be refused")
	}
	if _, err := parseSettings(append(append([]string{}, okArgs...), "-actor-id", "00000000-0000-0000-0000-000000000000"), env(okEnv())); err == nil {
		t.Error("the nil -actor-id must be refused")
	}
	e := okEnv()
	delete(e, "DATABASE_URL")
	if _, err := parseSettings(okArgs, env(e)); err == nil {
		t.Error("DATABASE_URL is required")
	}
	if _, err := parseSettings(append(append([]string{}, okArgs...), "extra"), env(okEnv())); err == nil {
		t.Error("positional arguments must be refused")
	}
}
