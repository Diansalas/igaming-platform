package kyc

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// DormantJurisdictionTrigger names a (licensing jurisdiction, trigger
// type[, play operation]) tuple with at least one live tenant and no
// active kyc_enforcement_policies row - ADR 0096 §6 dormancy observability
// (security condition 9), implemented in its simplest form: a
// platform-admin-only read with no HTTP surface yet (deferred - the
// query itself is the mechanism this ADR asks for; wiring an HTTP route
// on top is a small, later addition once an operator actually needs it,
// per CLAUDE.md's "no uncontrolled scope expansion").
type DormantJurisdictionTrigger struct {
	LicensingJurisdictionID uuid.UUID
	TriggerType             string
	// PlayOperation is non-empty only when TriggerType == "play"
	// ("casino_play" or "sportsbook_play") - play is authored per surface
	// (ADR 0096 §3.6), so dormancy is reported per surface too, never
	// collapsed into one "play" row that could hide one surface's own
	// gap behind the other's configured policy.
	PlayOperation string
}

// ListDormantJurisdictionTriggers implements ADR 0096 §6's illustrative
// query: every licensing jurisdiction with at least one active tenant and
// no active kyc_enforcement_policies row for a given trigger_type (and,
// for 'play', per play_operation - code review B4: the original version
// of this query omitted 'play' entirely, contradicting its own "reports
// every trigger_type the evaluator consults" comment). Must be called
// under a platform-admin-scoped transaction (db.Pool.WithPlatformAdmin) -
// it reads platform-wide reference data, mirroring every other
// platform-admin-only read in this codebase.
func ListDormantJurisdictionTriggers(ctx context.Context, tx pgx.Tx) ([]DormantJurisdictionTrigger, error) {
	rows, err := tx.Query(ctx, `
		-- cumulative_deposit: one row per jurisdiction with a live tenant
		-- and no active row for that trigger_type (asset-scoped policies
		-- are collapsed to "dormant" only when NO asset has an active row -
		-- KYC-FX-AGG-1's own per-asset reporting is a registered follow-up,
		-- not built here).
		SELECT DISTINCT l.jurisdiction_id, 'cumulative_deposit', ''
		  FROM tenants t
		  JOIN licences l ON l.id = t.licence_id
		 WHERE NOT EXISTS (
		       SELECT 1 FROM kyc_enforcement_policies p
		        WHERE p.licensing_jurisdiction_id = l.jurisdiction_id
		          AND p.trigger_type = 'cumulative_deposit'
		          AND p.status = 'active'
		 )
		UNION ALL
		-- play, reported separately per surface (casino_play/sportsbook_play)
		-- - security condition 9 / code review B4.
		SELECT DISTINCT l.jurisdiction_id, 'play', tt.play_operation
		  FROM tenants t
		  JOIN licences l ON l.id = t.licence_id
		 CROSS JOIN (VALUES ('casino_play'), ('sportsbook_play')) AS tt(play_operation)
		 WHERE NOT EXISTS (
		       SELECT 1 FROM kyc_enforcement_policies p
		        WHERE p.licensing_jurisdiction_id = l.jurisdiction_id
		          AND p.trigger_type = 'play'
		          AND p.play_operation = tt.play_operation
		          AND p.status = 'active'
		 )
		-- Deliberately excludes edd_amount/registration_tier (security
		-- re-verification N3): only trigger_types the evaluator actually
		-- consults are reported here. Reporting an evaluator-unwired
		-- trigger_type as "dormant" (or "configured") would be the exact
		-- false-assurance N3 identifies, since activating either is
		-- refused at the database
		-- (kyc_enforcement_policies_enforce_lifecycle).
		`)
	if err != nil {
		return nil, fmt.Errorf("kyc: list dormant jurisdiction triggers: %w", err)
	}
	defer rows.Close()

	var out []DormantJurisdictionTrigger
	for rows.Next() {
		var d DormantJurisdictionTrigger
		if err := rows.Scan(&d.LicensingJurisdictionID, &d.TriggerType, &d.PlayOperation); err != nil {
			return nil, fmt.Errorf("kyc: scan dormant jurisdiction trigger: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
