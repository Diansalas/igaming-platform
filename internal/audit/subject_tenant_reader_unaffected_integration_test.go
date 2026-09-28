//go:build integration

// PRH-2 G1 code review F-4 (ADR 0104 §7 "Readers unchanged (R)"):
// testsupport/noeffect's own `SELECT count(*) FROM audit_log WHERE
// tenant_id = $1` (both call sites, Capture and CaptureCasino) can never
// count a subject row - its tenant_id is always NULL by migration 0109's
// platform-only CHECK, regardless of what its subject_tenant_id is.
package audit

import (
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/testsupport/noeffect"
)

func TestNoEffectCapture_AuditLogCountIgnoresSubjectRows(t *testing.T) {
	pool := testPool(t)
	tenantA := createTestTenant(t, pool)
	admin := stPlatformAdmin(t, pool)
	recordSubjectRow(t, pool, admin, tenantA, "test.f4_noeffect_"+uuid.NewString())

	snap := noeffect.Capture(t, pool, []uuid.UUID{tenantA}, nil)
	if got := snap.AuditLogCount[tenantA]; got != 0 {
		t.Fatalf("F-4: expected noeffect.Capture's AuditLogCount to ignore the subject row (tenant has no tenant-owned audit rows), got %d", got)
	}
}
