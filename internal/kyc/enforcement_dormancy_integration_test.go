//go:build integration

// ListDormantJurisdictionTriggers tests (ADR 0096 §6, security condition
// 9). code review rv-prh-i3-code-review.md B4: the original version had
// no test at all (dormancy per surface was added, but never proven), and
// separately omitted a `tenants.status` filter entirely, so a jurisdiction
// whose only tenant had gone suspended/closed was still reported as
// needing a policy decision. Both are pinned here (KYC-ENF-TESTPINS-1).
package kyc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// dormantKeysFor collects every dormancy key ("trigger_type" or
// "trigger_type:play_operation") reported for jurisdictionID.
func dormantKeysFor(triggers []DormantJurisdictionTrigger, jurisdictionID uuid.UUID) map[string]bool {
	found := map[string]bool{}
	for _, tr := range triggers {
		if tr.LicensingJurisdictionID != jurisdictionID {
			continue
		}
		key := tr.TriggerType
		if tr.PlayOperation != "" {
			key += ":" + tr.PlayOperation
		}
		found[key] = true
	}
	return found
}

func listDormantTriggers(t *testing.T, pool *db.Pool) []DormantJurisdictionTrigger {
	t.Helper()
	var triggers []DormantJurisdictionTrigger
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		triggers, err = ListDormantJurisdictionTriggers(ctx, tx)
		return err
	})
	if err != nil {
		t.Fatalf("ListDormantJurisdictionTriggers: %v", err)
	}
	return triggers
}

// TestListDormantJurisdictionTriggers_ReportsEveryTriggerType proves a
// jurisdiction with a live tenant and NO active policy at all is reported
// dormant for cumulative_deposit AND both play surfaces independently
// (code review B4's own original defect: 'play' was omitted entirely).
func TestListDormantJurisdictionTriggers_ReportsEveryTriggerType(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	jurisdictionID := mustLicenseTenant(t, pool, f)

	found := dormantKeysFor(listDormantTriggers(t, pool), jurisdictionID)
	for _, want := range []string{"cumulative_deposit", "play:casino_play", "play:sportsbook_play"} {
		if !found[want] {
			t.Fatalf("expected dormancy key %q to be reported for jurisdiction %s, got %+v", want, jurisdictionID, found)
		}
	}
}

// TestListDormantJurisdictionTriggers_ActivePolicyExcludesOnlyThatTrigger
// proves activating a cumulative_deposit policy removes ONLY that key
// from the report, not the jurisdiction's still-dormant play triggers.
func TestListDormantJurisdictionTriggers_ActivePolicyExcludesOnlyThatTrigger(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	jurisdictionID := mustLicenseTenant(t, pool, f)

	var policyID uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		policyID, err = CreateEnforcementPolicy(ctx, tx, CreateEnforcementPolicyParams{
			LicensingJurisdictionID: jurisdictionID, TriggerType: "cumulative_deposit",
			ThresholdMinorUnits: strPtr("100000"), AssetCode: strPtr("EUR"),
			LegalReviewReference: "test-legal-ref", ReasonCode: "test",
		})
		return err
	})
	if err != nil {
		t.Fatalf("create cumulative_deposit policy: %v", err)
	}
	if err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		return ActivateEnforcementPolicy(ctx, tx, policyID)
	}); err != nil {
		t.Fatalf("activate cumulative_deposit policy: %v", err)
	}

	found := dormantKeysFor(listDormantTriggers(t, pool), jurisdictionID)
	if found["cumulative_deposit"] {
		t.Fatalf("expected cumulative_deposit to be EXCLUDED once an active policy exists, got %+v", found)
	}
	for _, want := range []string{"play:casino_play", "play:sportsbook_play"} {
		if !found[want] {
			t.Fatalf("expected dormancy key %q to still be reported (unaffected by the cumulative_deposit policy), got %+v", want, found)
		}
	}
}

// TestListDormantJurisdictionTriggers_ExcludesNonActiveTenants is B4's
// tenant-status filter pin: a jurisdiction whose only tenant is
// suspended/closed must not appear in the dormancy report at all - it has
// no LIVE tenant for whom a dormant policy decision is actually pending.
func TestListDormantJurisdictionTriggers_ExcludesNonActiveTenants(t *testing.T) {
	pool := testPool(t)

	cases := []string{"suspended", "closed"}
	for _, status := range cases {
		t.Run(status, func(t *testing.T) {
			f := seedFixture(t, pool)
			jurisdictionID := mustLicenseTenant(t, pool, f)

			// Sanity: before the status change, the jurisdiction DOES appear
			// (proves the test fixture itself would otherwise be reported).
			before := dormantKeysFor(listDormantTriggers(t, pool), jurisdictionID)
			if len(before) == 0 {
				t.Fatalf("expected the jurisdiction to appear as dormant while the tenant is active (fixture sanity check failed)")
			}

			err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE tenants SET status = $1 WHERE id = $2`, status, f.tenantID)
				return err
			})
			if err != nil {
				t.Fatalf("set tenant status=%s: %v", status, err)
			}

			after := dormantKeysFor(listDormantTriggers(t, pool), jurisdictionID)
			if len(after) != 0 {
				t.Fatalf("expected 0 dormancy rows for a jurisdiction whose only tenant is %q, got %+v", status, after)
			}
		})
	}
}
