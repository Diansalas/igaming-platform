//go:build integration

package operatingmarket

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
)

// Licensing slice 1 (audit G-6): the resolver's licence arm for a SUSPENDED licence, an EXPIRED status, the expires_at
// boundary and an UNBOUND tenant, each driven through the new lifecycle writes (jurisdiction.SetLicence*), so the
// write path and the runtime refusal are proved together. The boundary is EvaluateLicenceValidity's, unchanged: a
// licence is NOT valid on its own expires_at date (half-open; confirming the boundary with compliance is a human item).

func lifecycleActor() jurisdiction.ActorContext {
	return jurisdiction.ActorContext{ActorID: uuid.New(), IPAddress: "127.0.0.1", UserAgent: "test", RequestID: "t-" + uuid.NewString()[:8], ReasonCode: "g6_test"}
}

func lifecyclePlatform(t *testing.T, pool *db.Pool, admin uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) {
	t.Helper()
	if err := pool.WithPlatformAdmin(context.Background(), admin, fn); err != nil {
		t.Fatal(err)
	}
}

func explainCeiling(t *testing.T, pool *db.Pool, tenantID uuid.UUID, cc string) LicenceCeilingReason {
	t.Helper()
	var exp Explanation
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		exp, err = ExplainOperatingCountryPolicy(ctx, tx, Query{TenantID: tenantID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC()})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return exp.CeilingReason
}

func TestResolve_LicenceLifecycle_SuspendedExpiredBoundaryAndRestore(t *testing.T) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	cases := []struct {
		name   string
		apply  func(t *testing.T, ctx context.Context, tx pgx.Tx, id uuid.UUID)
		reason LicenceCeilingReason
	}{
		{"suspended", func(t *testing.T, ctx context.Context, tx pgx.Tx, id uuid.UUID) {
			if _, err := jurisdiction.SetLicenceStatus(ctx, tx, id, "suspended", lifecycleActor()); err != nil {
				t.Fatal(err)
			}
		}, CeilingReasonLicenceSuspended},
		{"status expired", func(t *testing.T, ctx context.Context, tx pgx.Tx, id uuid.UUID) {
			if _, err := jurisdiction.SetLicenceStatus(ctx, tx, id, "expired", lifecycleActor()); err != nil {
				t.Fatal(err)
			}
		}, CeilingReasonLicenceExpired},
		{"expires_at is today (boundary: not valid on the expiry date)", func(t *testing.T, ctx context.Context, tx pgx.Tx, id uuid.UUID) {
			if _, err := jurisdiction.SetLicenceValidityDates(ctx, tx, id, nil, &today, lifecycleActor()); err != nil {
				t.Fatal(err)
			}
		}, CeilingReasonLicenceExpired},
		{"expires_at was yesterday", func(t *testing.T, ctx context.Context, tx pgx.Tx, id uuid.UUID) {
			y := today.AddDate(0, 0, -1)
			if _, err := jurisdiction.SetLicenceValidityDates(ctx, tx, id, nil, &y, lifecycleActor()); err != nil {
				t.Fatal(err)
			}
		}, CeilingReasonLicenceExpired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := testPool(t)
			f := seedFixture(t, pool)
			cc := "MS"
			enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
			enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)
			q := Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC()}
			if got := resolveNow(t, pool, f.tenantID, q); got.Outcome() != OutcomePermitted {
				t.Fatalf("sanity: %s", got.Outcome())
			}

			lifecyclePlatform(t, pool, f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
				c.apply(t, ctx, tx, f.licenceID)
				return nil
			})
			got := resolveNow(t, pool, f.tenantID, q)
			if got.Outcome() != OutcomeNotPermittedByLicence || got.Permitted() {
				t.Fatalf("outcome %s, want not_permitted_by_licence", got.Outcome())
			}
			if r := explainCeiling(t, pool, f.tenantID, cc); r != c.reason {
				t.Fatalf("ceiling reason %q, want %q", r, c.reason)
			}

			// Restoring through the lifecycle (a widening change) re-opens the gate.
			lifecyclePlatform(t, pool, f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
				if _, err := jurisdiction.SetLicenceStatus(ctx, tx, f.licenceID, "active", lifecycleActor()); err != nil {
					// a date-only case is already 'active'
					_ = err
				}
				future := today.AddDate(1, 0, 0)
				_, err := jurisdiction.SetLicenceValidityDates(ctx, tx, f.licenceID, nil, &future, lifecycleActor())
				return err
			})
			if got := resolveNow(t, pool, f.tenantID, q); got.Outcome() != OutcomePermitted {
				t.Fatalf("after restore: %s", got.Outcome())
			}
		})
	}
}

func TestResolve_LicenceLifecycle_ExpiresTomorrowIsStillValid(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "MS"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)
	tomorrow := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	lifecyclePlatform(t, pool, f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := jurisdiction.SetLicenceValidityDates(ctx, tx, f.licenceID, nil, &tomorrow, lifecycleActor())
		return err
	})
	got := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC()})
	if got.Outcome() != OutcomePermitted {
		t.Fatalf("a licence expiring tomorrow is still valid today, got %s", got.Outcome())
	}
}

func TestResolve_LicenceLifecycle_UnboundTenantIsLicensingUnknown(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "MS"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)
	unbound := uuid.New()
	lifecyclePlatform(t, pool, f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'No licence', 'under_platform_licence')`, unbound, "nl-"+unbound.String()[:8])
		return err
	})
	got := resolveNow(t, pool, unbound, Query{TenantID: unbound, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC()})
	if got.Outcome() != OutcomeLicensingUnknown || got.Permitted() {
		t.Fatalf("licence_id NULL: outcome %s, want licensing_unknown", got.Outcome())
	}
	// Unbinding an existing tenant (licence_id set to NULL) behaves the same.
	lifecyclePlatform(t, pool, f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = NULL WHERE id = $1`, f.tenantID)
		return err
	})
	got = resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC()})
	if got.Outcome() != OutcomeLicensingUnknown {
		t.Fatalf("unbound: %s", got.Outcome())
	}
}
