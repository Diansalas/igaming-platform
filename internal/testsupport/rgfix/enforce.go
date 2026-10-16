//go:build integration

package rgfix

// ADR 0115 S4-min test provisioning: put a TEST tenant under RG limit enforcement.
//
// The enforcement is OFF by default (migration 0158). A money-path test that wants it ON needs three
// things this file creates through the REAL paths, nothing disabled:
//
//  1. a jurisdiction and a licence, linked to the tenant (the jurisdiction code is derived server-side
//     from the tenant's licence; the fixture tenants of the older money-path tests have none);
//  2. a jurisdiction FLOOR per control kind through the complete four-eyes flow (a change request, an
//     approval by a different Person, the apply), with a NON-synthetic attestation marker so the rows
//     are in force whatever app.environment the test connection carries;
//  3. the activation row, written as an active staff principal of the tenant.
//
// The values are SYNTHETIC test values, not legal values. The package carries the integration build tag
// and is import-pinned like launchfix (internal/tenant/launch_fixture_pin_test.go).

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// TestAttestation marks fixture floors that must be IN FORCE in any test connection (it is not the
// reserved word the synthetic-config guard looks for, so the rows are not treated as absent outside
// dev/test GUCs). It is a test marker, not a legal attestation.
const TestAttestation = "rgfix-test-floor-not-legal-values"

// EnforcedFloor is a test floor of one kind for one asset: enabled, ceiling none, a 2-second increase
// cooldown (so a test can wait it out), every period allowed, and the supplied overrides on top.
func EnforcedFloor(jur, kind, asset string, over Spec) Spec {
	s := PermissiveFloor(jur, kind).Clone(Spec{"attestation_ref": TestAttestation, "reason_code": "rgfix_enforcement", "enabled": true})
	switch kind {
	case "deposit", "loss", "wager":
		s = s.Clone(Spec{"asset_code": asset, "increase_cooldown": "00:00:02"})
	case "session_duration":
		s = s.Clone(Spec{"increase_cooldown": "00:00:02"})
	}
	return s.Clone(over)
}

// Enforcement is what Provision created.
type Enforcement struct {
	Jurisdiction string
	Staff        uuid.UUID // the tenant staff principal that wrote the activation
}

// Provision gives the tenant a licence in a fresh jurisdiction and one floor per amount kind (deposit,
// loss, wager) for asset, applying over[kind] on top of EnforcedFloor, then switches enforcement ON for
// the tenant (brandID nil) or one brand. owner must be a TABLE-OWNER pool of the test database.
func Provision(tb testing.TB, owner *db.Pool, tenantID uuid.UUID, brandID *uuid.UUID, asset string, over map[string]Spec) Enforcement {
	tb.Helper()
	ctx := context.Background()
	launchfix.SeedOperatorsAt(tb, ownerURL(tb))
	ops := launchfix.Operators()
	jur := "RGE-" + strings.ToUpper(uuid.NewString()[:8])
	if err := owner.WithPlatformAdmin(ctx, ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		var jid, lic uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO jurisdictions (code, name) VALUES ($1, 'rgfix enforcement fixture') RETURNING id`, jur).Scan(&jid); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO licences (jurisdiction_id, licensee, licence_number) VALUES ($1, 'platform', $2) RETURNING id`, jid, "RGE-"+jur).Scan(&lic); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $1 WHERE id = $2`, lic, tenantID)
		return err
	}); err != nil {
		tb.Fatalf("rgfix: provision jurisdiction and licence: %v", err)
	}
	for _, kind := range []string{"deposit", "loss", "wager"} {
		spec := EnforcedFloor(jur, kind, asset, over[kind])
		if err := owner.WithPlatformAdmin(ctx, ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			if err := SetEnvironmentGUC(ctx, tx, "test"); err != nil {
				return err
			}
			_, err := InsertFloor(ctx, tx, spec, DefaultSteps())
			return err
		}); err != nil {
			tb.Fatalf("rgfix: provision %s floor: %v", kind, err)
		}
	}
	staff := Staff(tb, owner, tenantID)
	Activate(tb, owner, tenantID, brandID, staff)
	return Enforcement{Jurisdiction: jur, Staff: staff}
}

// Staff creates an active tenant_admin staff user with its own Person in the tenant and returns its id.
func Staff(tb testing.TB, owner *db.Pool, tenantID uuid.UUID) uuid.UUID {
	tb.Helper()
	ctx := context.Background()
	id, person := uuid.New(), uuid.New()
	if err := owner.WithPlatformAdmin(ctx, launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person)
		return err
	}); err != nil {
		tb.Fatalf("rgfix: seed staff person: %v", err)
	}
	if err := owner.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id)
			VALUES ($1, $2, $3, '!x', 'tenant_admin', $4)`, id, tenantID, "rgfix-"+id.String()+"@fixture.invalid", person)
		return err
	}); err != nil {
		tb.Fatalf("rgfix: seed tenant staff: %v", err)
	}
	return id
}

func ownerURL(tb testing.TB) string {
	tb.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		tb.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	return u
}

// Activate writes the enforcement activation row as the tenant staff principal (the database refuses any
// other writer). An already-active scope is not an error (idempotent).
func Activate(tb testing.TB, owner *db.Pool, tenantID uuid.UUID, brandID *uuid.UUID, staff uuid.UUID) {
	tb.Helper()
	if err := owner.WithPrincipalScope(context.Background(), tenantID, staff, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO rg_enforcement_activations (tenant_id, brand_id, reason_code, attestation_ref)
			VALUES ($1, $2, 'rgfix_enforcement', $3) ON CONFLICT DO NOTHING`, tenantID, brandID, TestAttestation)
		return err
	}); err != nil {
		tb.Fatalf("rgfix: activate enforcement: %v", err)
	}
}
