package kyc

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// DormantJurisdictionTrigger names a (licensing jurisdiction, trigger
// type) pair with at least one live tenant and no active
// kyc_enforcement_policies row - ADR 0096 §6 dormancy observability
// (security condition 9), implemented in its simplest form: a
// platform-admin-only read with no HTTP surface yet (deferred - the
// query itself is the mechanism this ADR asks for; wiring an HTTP route
// on top is a small, later addition once an operator actually needs it,
// per CLAUDE.md's "no uncontrolled scope expansion").
type DormantJurisdictionTrigger struct {
	LicensingJurisdictionID uuid.UUID
	TriggerType             string
}

// ListDormantJurisdictionTriggers implements ADR 0096 §6's illustrative
// query: every licensing jurisdiction with at least one active tenant and
// no active kyc_enforcement_policies row for a given trigger_type. Must
// be called under a platform-admin-scoped transaction
// (db.Pool.WithPlatformAdmin) - it reads platform-wide reference data,
// mirroring every other platform-admin-only read in this codebase.
func ListDormantJurisdictionTriggers(ctx context.Context, tx pgx.Tx) ([]DormantJurisdictionTrigger, error) {
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT l.jurisdiction_id, tt.trigger_type
		  FROM tenants t
		  JOIN licences l ON l.id = t.licence_id
		 CROSS JOIN (VALUES ('cumulative_deposit')) AS tt(trigger_type)
		 -- Deliberately narrower than migration 0100's full CHECK
		 -- vocabulary (security re-verification N3): only trigger_types the
		 -- evaluator actually consults are reported here. Reporting
		 -- edd_amount/registration_tier as "dormant" (or "configured")
		 -- would be the exact false-assurance N3 identifies, since
		 -- activating either is refused at the database
		 -- (kyc_enforcement_policies_enforce_lifecycle) - reporting on them
		 -- here would imply activation is possible.
		 WHERE NOT EXISTS (
		       SELECT 1 FROM kyc_enforcement_policies p
		        WHERE p.licensing_jurisdiction_id = l.jurisdiction_id
		          AND p.trigger_type = tt.trigger_type
		          AND p.status = 'active'
		 )`)
	if err != nil {
		return nil, fmt.Errorf("kyc: list dormant jurisdiction triggers: %w", err)
	}
	defer rows.Close()

	var out []DormantJurisdictionTrigger
	for rows.Next() {
		var d DormantJurisdictionTrigger
		if err := rows.Scan(&d.LicensingJurisdictionID, &d.TriggerType); err != nil {
			return nil, fmt.Errorf("kyc: scan dormant jurisdiction trigger: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
