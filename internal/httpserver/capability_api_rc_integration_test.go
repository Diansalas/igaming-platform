//go:build integration

// C-1 (code review of 5a27be6, mutant MC-1): the F-3 error-class mapping
// (internal/capability.ClassifyError -> writeCapabilityError) had no HTTP
// test exercising the 23505/23514 branches specifically - only the CG0xx
// guard-refusal branch was covered by the rest of this package's tests.
// Reuses newCGAPI/cgAPI from capability_api_integration_test.go (same
// package); a new file, per this project's own convention of adding new
// tests in new files rather than editing another agent's test file.
package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// latestDeniedAuditMetadata reads the most recent DENIED audit row's
// metadata for action, scoped to the tenant session (mirrors
// cgAPI.latestAuditRow, which only reads outcome='success' rows - a
// refused request writes outcome='denied' via
// recordCapabilityRefusalAudit, so a separate reader is needed here).
func (a *cgAPI) latestDeniedAuditMetadata(tenantID uuid.UUID, action string) map[string]any {
	a.t.Helper()
	var metaRaw []byte
	if err := a.pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT metadata FROM audit_log
			 WHERE action = $1 AND outcome = 'denied'
			 ORDER BY created_at DESC, id DESC
			 LIMIT 1`, action).Scan(&metaRaw)
	}); err != nil {
		a.t.Fatalf("read latest denied audit row for %s: %v", action, err)
	}
	var meta map[string]any
	if len(metaRaw) > 0 {
		if err := json.Unmarshal(metaRaw, &meta); err != nil {
			a.t.Fatalf("decode denied audit metadata for %s: %v", action, err)
		}
	}
	return meta
}

// TestCapabilityAPI_C1_DuplicateRequestUniqueViolation: the SAME G-T
// request submitted twice in a row. R-11 (one pending request per
// (tenant, grantee, capability)) has no explicit trigger pre-check in
// migration 0112 - it is enforced purely by the partial unique index - so
// the second, identical submission hits Postgres 23505 directly,
// classified by internal/capability.ClassifyError as
// ErrClassUniqueViolation, which writeCapabilityError maps to 409 with a
// "capability_grant.request" DENIED audit row whose metadata carries
// denied_class = "unique_violation" (never the CG0xx guard-refusal class,
// which this same audit path also writes with a different denied_class
// value - the two are distinguished by that field, not by a different
// action name).
func TestCapabilityAPI_C1_DuplicateRequestUniqueViolation(t *testing.T) {
	a := newCGAPI(t)
	tenantID := a.tenant()
	tenantAdmin := a.staff(tenantID, "tenant_admin")
	financeGrantee := a.staff(tenantID, "finance")
	tok := a.token(tenantAdmin, tenantID, "tenant_admin")

	body := map[string]any{
		"grantee_staff_id": financeGrantee.String(), "capability": "ledger_adjustment:initiate", "reason_code": "test",
	}

	first := a.do(http.MethodPost, "/v1/admin/tenants/"+tenantID.String()+"/capability-grants/requests", tok, body)
	if first.status != http.StatusCreated {
		t.Fatalf("first request: expected 201, got %d: %s", first.status, first.body)
	}

	second := a.do(http.MethodPost, "/v1/admin/tenants/"+tenantID.String()+"/capability-grants/requests", tok, body)
	if second.status != http.StatusConflict {
		t.Fatalf("duplicate request: expected 409 (unique_violation, MC-1's own target), got %d: %s", second.status, second.body)
	}

	meta := a.latestDeniedAuditMetadata(tenantID, "capability_grant.request")
	if meta == nil {
		t.Fatal("expected a capability_grant.request denied audit row for the refused duplicate")
	}
	if got, _ := meta["denied_class"].(string); got != "unique_violation" {
		t.Fatalf("expected denied_class=unique_violation, got %q (full metadata: %+v)", got, meta)
	}
}

// TestCapabilityAPI_C1_CheckViolationGives400: a reason_code longer than
// the table's own CHECK (octet_length BETWEEN 1 AND 64) passes this
// handler's own non-empty validation (so it reaches the database) but
// trips ONLY the table CHECK constraint - no trigger in migration 0112
// validates reason_code length - so ClassifyError's 23514 branch
// (ErrClassCheckViolation) is what's actually exercised here, mapped to
// 400 (a client-data-validation failure, not a 409 conflict - there is no
// concurrent second actor to retry against).
func TestCapabilityAPI_C1_CheckViolationGives400(t *testing.T) {
	a := newCGAPI(t)
	tenantID := a.tenant()
	tenantAdmin := a.staff(tenantID, "tenant_admin")
	financeGrantee := a.staff(tenantID, "finance")
	tok := a.token(tenantAdmin, tenantID, "tenant_admin")

	tooLong := ""
	for i := 0; i < 65; i++ {
		tooLong += "x"
	}

	resp := a.do(http.MethodPost, "/v1/admin/tenants/"+tenantID.String()+"/capability-grants/requests", tok, map[string]any{
		"grantee_staff_id": financeGrantee.String(), "capability": "ledger_adjustment:initiate", "reason_code": tooLong,
	})
	if resp.status != http.StatusBadRequest {
		t.Fatalf("over-length reason_code (CHECK violation): expected 400, got %d: %s", resp.status, resp.body)
	}

	meta := a.latestDeniedAuditMetadata(tenantID, "capability_grant.request")
	if meta == nil {
		t.Fatal("expected a capability_grant.request denied audit row for the refused CHECK violation")
	}
	if got, _ := meta["denied_class"].(string); got != "check_violation" {
		t.Fatalf("expected denied_class=check_violation, got %q (full metadata: %+v)", got, meta)
	}
}
