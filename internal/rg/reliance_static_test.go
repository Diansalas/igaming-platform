package rg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lic25 static pins: the jurisdiction selection lives in ONE file; every RG read goes through it; the migration keeps the set-aware
// classification. Behaviour is proven by the integration tests (internal/licensingchange/lic25_rg_scope_integration_test.go).

func nonTestSources(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[f] = string(b)
	}
	return out
}

func TestLic25_Static_TheLicenceOnlySelectionIsGone(t *testing.T) {
	for f, src := range nonTestSources(t) {
		if strings.Contains(src, "tenantJurisdiction(") {
			t.Errorf("%s: the licence-only tenantJurisdiction selection was replaced by tenantReliance / scopedSelection (lic25)", f)
		}
		if f != "reliance.go" && strings.Contains(src, "ResolveLicensingChain") {
			t.Errorf("%s: the tenant licence is read in reliance.go only, so every RG read selects the jurisdiction the same way", f)
		}
	}
}

func TestLic25_Static_EnforcementUsesTheScopedSelection_AndAccountLevelReadsTheReliedSet(t *testing.T) {
	src := nonTestSources(t)
	if !strings.Contains(src["enforcement.go"], "scopedSelection(ctx, tx, p)") {
		t.Error("EvaluateAction's limit evaluation must select its jurisdiction by the player's scope")
	}
	for _, f := range []string{"limits_service.go", "limits_timeout.go", "options.go", "limits_usage_inputs.go", "limits_config.go"} {
		if !strings.Contains(src[f], "tenantReliance(ctx, tx,") {
			t.Errorf("%s: account-level reads must use the relied-on jurisdiction set", f)
		}
	}
	rel := src["reliance.go"]
	for _, need := range []string{"withPlayerScopeCleared(ctx, tx, func() error {", "kyc.ReadMarketResidenceEvidence", "operatingmarket.ResolveRulebookJurisdiction",
		"set_config('app.player_account_id', $1, true)", "ErrRulebookUnavailable"} {
		if !strings.Contains(rel, need) {
			t.Errorf("reliance.go lost %q", need)
		}
	}
	// Both selections run inside the clear/restore wrapper.
	if strings.Count(rel, "withPlayerScopeCleared(ctx, tx, func() error {") < 2 {
		t.Error("tenantReliance and scopedSelection must each clear and restore the player scope")
	}
}

func TestLic25_Static_Migration0187KeepsTheSetAwareClassification(t *testing.T) {
	b, err := os.ReadFile("../../migrations/0187_lic25_rg_scope_aware_classification.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	m := string(b)
	for _, need := range []string{
		"CREATE FUNCTION rg_basis_jurisdictions(",
		"CREATE FUNCTION rg_limit_change_classify_set(",
		"GREATEST(COALESCE(v_cool, v_cfg.increase_cooldown), v_cfg.increase_cooldown)",
		"OR p_incomplete OR v_missing OR v_cool IS NULL",
		"USING ERRCODE = 'RL023'",
		"app.player_account_id', true), '') IS NOT NULL",
		"b.status = 'active' AND b.change_request_id IS NOT NULL",
		"FROM public.rg_basis_jurisdictions(NEW.tenant_id, NEW.requested_at)",
		"public.rg_limit_change_classify_set(NEW.tenant_id",
		"CREATE OR REPLACE FUNCTION rg_limit_change_classify(",
	} {
		if !strings.Contains(m, need) {
			t.Errorf("migration 0187 lost %q", need)
		}
	}
	if strings.Contains(m, "SECURITY DEFINER") {
		t.Error("migration 0187 adds no SECURITY DEFINER function")
	}
}
