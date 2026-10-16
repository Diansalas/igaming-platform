//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

type fixture struct {
	tenantSlug, brandSlug string
	tenantID, brandID     uuid.UUID
	licenceID, admin      uuid.UUID
}

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := db.Connect(context.Background(), url, 5, 5*time.Second)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func mustActor(f fixture) operatingmarket.ActorContext {
	return operatingmarket.ActorContext{ActorID: f.admin, ReasonCode: "seed-market-test"}
}

// seed creates a synthetic tenant with a platform licence (or none), one brand and one platform admin.
func seed(t *testing.T, pool *db.Pool, withLicence bool) fixture {
	t.Helper()
	f := fixture{tenantID: uuid.New(), brandID: uuid.New(), licenceID: uuid.New(), admin: uuid.New()}
	f.tenantSlug = "sm-" + f.tenantID.String()[:8]
	f.brandSlug = "smb-" + f.brandID.String()[:8]
	jur := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), f.admin, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'Seed Market Tenant', 'under_platform_licence')`, f.tenantID, f.tenantSlug); err != nil {
			return err
		}
		if withLicence {
			if _, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'J')`, jur, "SM-"+jur.String()[:8]); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1, $2, 'platform', $3)`, f.licenceID, jur, "L-"+f.licenceID.String()[:8]); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $2 WHERE id = $1`, f.tenantID, f.licenceID); err != nil {
				return err
			}
		}
		personID := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id, status) VALUES ($1, NULL, $2, 'x', 'platform_admin', $3, 'active')`,
			f.admin, f.admin.String()+"@example.com", personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed platform rows: %v", err)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name, status) VALUES ($1, $2, $3, 'B', 'pending_launch')`, f.brandID, f.tenantID, f.brandSlug)
		return err
	})
	if err != nil {
		t.Fatalf("seed brand: %v", err)
	}
	return f
}

func settingsFor(f fixture, country string) settings {
	return settings{tenantSlug: f.tenantSlug, brandSlug: f.brandSlug, country: country, authRef: "TICKET-SM-1", operator: "ops-test", reason: "seed-market-test", actorID: f.admin}
}

func count(t *testing.T, pool *db.Pool, tenant uuid.UUID, platform bool, q string, args ...any) int {
	t.Helper()
	var n int
	var err error
	if platform {
		err = pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error { return tx.QueryRow(ctx, q, args...).Scan(&n) })
	} else {
		err = pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error { return tx.QueryRow(ctx, q, args...).Scan(&n) })
	}
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestSeedMarket_WritesBothRowsAuditsThemAndOpensTheGate(t *testing.T) {
	pool := testPool(t)
	f := seed(t, pool, true)
	var out bytes.Buffer
	if err := apply(context.Background(), pool, settingsFor(f, "SV"), &out); err != nil {
		t.Fatalf("apply: %v\n%s", err, out.String())
	}
	for _, want := range []string{"SEEDMARKET|ceiling|created version ", "SEEDMARKET|tenant_policy|created version ", "SEEDMARKET|resolves_registration|permitted", "SEEDMARKET|resolves_deposit|permitted", "SEEDMARKET|resolves_casino_wagering|permitted", "SEEDMARKET|resolves_sportsbook_wagering|permitted", "SEEDMARKET|result|OK"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if n := count(t, pool, uuid.Nil, true, `SELECT count(*) FROM licence_country_ceilings WHERE licence_id=$1 AND country_code='SV' AND effective_to IS NULL AND state='enabled' AND authorization_reference='TICKET-SM-1' AND reason_code='seed-market-test'`, f.licenceID); n != 1 {
		t.Fatalf("expected exactly one open enabled ceiling row, got %d", n)
	}
	if n := count(t, pool, f.tenantID, false, `SELECT count(*) FROM operating_country_policies WHERE tenant_id=$1 AND scope_kind='tenant' AND country_code='SV' AND effective_to IS NULL AND state='enabled' AND authorization_reference='TICKET-SM-1'`, f.tenantID); n != 1 {
		t.Fatalf("expected exactly one open enabled tenant policy row, got %d", n)
	}
	// the audit rows the library writes: one platform-scope ceiling row (actor = the named admin) and one tenant-scope policy row
	if n := count(t, pool, uuid.Nil, true, `SELECT count(*) FROM audit_log WHERE action='operating_market.licence_ceiling_version_created' AND actor_id=$1 AND metadata->>'licence_id'=$2 AND metadata->>'reason_code'='seed-market-test' AND user_agent LIKE '%operator=ops-test'`, f.admin, f.licenceID.String()); n != 1 {
		t.Fatalf("expected one ceiling audit row naming the actor and operator, got %d", n)
	}
	if n := count(t, pool, f.tenantID, false, `SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action='operating_market.policy_version_created' AND actor_id=$2 AND metadata->>'country_code'='SV'`, f.tenantID, f.admin); n != 1 {
		t.Fatalf("expected one policy audit row, got %d", n)
	}
}

func TestSeedMarket_IsIdempotent_NoDuplicateVersionsNoExtraAudit(t *testing.T) {
	pool := testPool(t)
	f := seed(t, pool, true)
	for i := 0; i < 3; i++ {
		var out bytes.Buffer
		if err := apply(context.Background(), pool, settingsFor(f, "SV"), &out); err != nil {
			t.Fatalf("run %d: %v\n%s", i, err, out.String())
		}
		if i > 0 && (!strings.Contains(out.String(), "SEEDMARKET|ceiling|already present") || !strings.Contains(out.String(), "SEEDMARKET|tenant_policy|already present")) {
			t.Fatalf("run %d must report already present:\n%s", i, out.String())
		}
	}
	if n := count(t, pool, uuid.Nil, true, `SELECT count(*) FROM licence_country_ceilings WHERE licence_id=$1 AND country_code='SV'`, f.licenceID); n != 1 {
		t.Fatalf("a re-run must not add ceiling versions, got %d rows", n)
	}
	if n := count(t, pool, f.tenantID, false, `SELECT count(*) FROM operating_country_policies WHERE tenant_id=$1 AND country_code='SV'`, f.tenantID); n != 1 {
		t.Fatalf("a re-run must not add policy versions, got %d rows", n)
	}
	if n := count(t, pool, f.tenantID, false, `SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action='operating_market.policy_version_created'`, f.tenantID); n != 1 {
		t.Fatalf("a re-run must not add audit rows, got %d", n)
	}
}

func TestSeedMarket_ResumesAfterAPartialRun(t *testing.T) {
	pool := testPool(t)
	f := seed(t, pool, true)
	ctx := context.Background()
	if _, err := ensureCeiling(ctx, pool, settingsFor(f, "SV"), f.licenceID, mustActor(f)); err != nil {
		t.Fatalf("ceiling only: %v", err)
	}
	var out bytes.Buffer
	if err := apply(ctx, pool, settingsFor(f, "SV"), &out); err != nil {
		t.Fatalf("apply after a partial run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "SEEDMARKET|ceiling|already present") || !strings.Contains(out.String(), "SEEDMARKET|tenant_policy|created") {
		t.Fatalf("expected ceiling skipped and policy created:\n%s", out.String())
	}
}

func TestSeedMarket_RefusesToReEnableAClosedCountry(t *testing.T) {
	pool := testPool(t)
	f := seed(t, pool, true)
	ctx := context.Background()
	if err := apply(ctx, pool, settingsFor(f, "SV"), &bytes.Buffer{}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	// an operator disables the tenant opt-in (the emergency stop needs no authorisation reference)
	err := pool.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := operatingmarket.CreateOperatingCountryPolicyVersion(ctx, tx, operatingmarket.CreateOperatingCountryPolicyVersionParams{
			Scope: operatingmarket.ScopeTenant, TenantID: f.tenantID, CountryCode: "SV",
			State: operatingmarket.StateDisabled, Status: operatingmarket.StatusActive, Actor: mustActor(f),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	err = apply(ctx, pool, settingsFor(f, "SV"), &bytes.Buffer{})
	if !errors.Is(err, errConflict) {
		t.Fatalf("a closed country must not be silently re-enabled, got %v", err)
	}
	if n := count(t, pool, f.tenantID, false, `SELECT count(*) FROM operating_country_policies WHERE tenant_id=$1 AND country_code='SV' AND effective_to IS NULL AND state='disabled'`, f.tenantID); n != 1 {
		t.Fatalf("the disable must still be the open version, got %d", n)
	}
}

func TestSeedMarket_RefusesAMissingOrLicencelessTenantAndForeignBrand(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	f := seed(t, pool, true)
	s := settingsFor(f, "SV")
	s.tenantSlug = "no-such-tenant"
	if err := apply(ctx, pool, s, &bytes.Buffer{}); err == nil {
		t.Error("an unknown tenant must be refused")
	}
	s = settingsFor(f, "SV")
	s.brandSlug = "no-such-brand"
	if err := apply(ctx, pool, s, &bytes.Buffer{}); err == nil {
		t.Error("an unknown brand must be refused")
	}
	other := seed(t, pool, true)
	s = settingsFor(f, "SV")
	s.brandSlug = other.brandSlug // a brand of ANOTHER tenant
	if err := apply(ctx, pool, s, &bytes.Buffer{}); err == nil {
		t.Error("a brand of another tenant must be refused")
	}
	g := seed(t, pool, false)
	err := apply(ctx, pool, settingsFor(g, "SV"), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no licence_id") {
		t.Errorf("a tenant without a licence must be refused, got %v", err)
	}
	if n := count(t, pool, g.tenantID, false, `SELECT count(*) FROM operating_country_policies WHERE tenant_id=$1`, g.tenantID); n != 0 {
		t.Errorf("nothing may be written for a refused tenant, got %d policy rows", n)
	}
}

func TestSeedMarket_DryRunWritesNothing(t *testing.T) {
	pool := testPool(t)
	f := seed(t, pool, true)
	s := settingsFor(f, "SV")
	s.dryRun = true
	var out bytes.Buffer
	if err := apply(context.Background(), pool, s, &out); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(out.String(), "would be created") {
		t.Fatalf("expected a would-be report:\n%s", out.String())
	}
	if n := count(t, pool, uuid.Nil, true, `SELECT count(*) FROM licence_country_ceilings WHERE licence_id=$1`, f.licenceID); n != 0 {
		t.Fatalf("dry-run wrote %d ceiling rows", n)
	}
}

func TestSeedMarket_AmbiguousPlatformAdminRequiresAnExplicitActor(t *testing.T) {
	pool := testPool(t)
	seed(t, pool, true)
	seed(t, pool, true) // at least two active platform admins now exist in this database
	_, err := soleActivePlatformAdmin(context.Background(), pool)
	if err == nil || !strings.Contains(err.Error(), "pass -actor-id") {
		t.Fatalf("expected the ambiguity to be refused, got %v", err)
	}
}

func TestSeedMarket_ARunThatCannotOpenTheGateFails(t *testing.T) {
	pool := testPool(t)
	f := seed(t, pool, true)
	ctx := context.Background()
	// a brand-scope disable exists before seed-market runs: the tenant opt-in is written, but the gate stays closed for the brand.
	err := pool.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := operatingmarket.CreateOperatingCountryPolicyVersion(ctx, tx, operatingmarket.CreateOperatingCountryPolicyVersionParams{
			Scope: operatingmarket.ScopeBrand, TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: "SV",
			State: operatingmarket.StateDisabled, Status: operatingmarket.StatusActive, Actor: mustActor(f),
		})
		return err
	})
	if err != nil {
		t.Fatalf("brand disable: %v", err)
	}
	var out bytes.Buffer
	err = apply(ctx, pool, settingsFor(f, "SV"), &out)
	if err == nil || !strings.Contains(err.Error(), "gate is NOT open") {
		t.Fatalf("expected the post-write verification to fail, got %v\n%s", err, out.String())
	}
}
