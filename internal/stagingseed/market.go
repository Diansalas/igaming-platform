package stagingseed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

// ErrConflict: a different open version exists (a closed country is never silently re-enabled).
var ErrConflict = errors.New("refused: a different open version exists")

// MarketParams is the validated input of EnsureMarket.
type MarketParams struct {
	TenantSlug, BrandSlug, Country, AuthRef, Operator, Reason string
	ActorID                                                   uuid.UUID // uuid.Nil = the sole active platform admin
	DryRun                                                    bool
}

// Validate checks the free-text fields and the country (no database access).
func (p MarketParams) Validate() error {
	for name, v := range map[string]string{"tenant slug": p.TenantSlug, "brand slug": p.BrandSlug, "authorization reference": p.AuthRef, "operator": p.Operator, "reason": p.Reason} {
		if err := CheckField(name, v); err != nil {
			return err
		}
	}
	if !validation.IsISO3166Alpha2(p.Country) {
		return errors.New("country must be a valid, uppercase ISO-3166-1 alpha-2 code")
	}
	return nil
}

// EnsureMarket writes the licence-country ceiling (platform scope) and the tenant opt-in (tenant scope) for the tenant's brand
// through the reviewed library calls, then proves the gate opens. The anchor gate runs FIRST, also for a dry run.
func EnsureMarket(ctx context.Context, pool *db.Pool, p MarketParams, out io.Writer) error {
	say := func(k, v string) { _, _ = fmt.Fprintf(out, "SEEDMARKET|%s|%s\n", k, v) }
	anchor, err := RequireSyntheticAnchor(ctx, pool)
	if err != nil {
		return err
	}
	say("environment", "synthetic-only; database_environment anchor="+anchor)

	var tenantID, licenceID, brandID uuid.UUID
	err = pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var lic *uuid.UUID
		var status string
		if err := tx.QueryRow(ctx, `SELECT id, licence_id, status FROM tenants WHERE slug = $1`, p.TenantSlug).Scan(&tenantID, &lic, &status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("tenant %q does not exist", p.TenantSlug)
			}
			return err
		}
		if !TenantStatusAllowsSeeding(status) {
			return fmt.Errorf("refused: tenant %q is %s (only active or pending_launch tenants are seeded; market rows are a launch-readiness prerequisite, so pending_launch is allowed)", p.TenantSlug, status)
		}
		if lic == nil {
			return fmt.Errorf("tenant %q has no licence_id: the licence ceiling needs one (a platform admin attaches the licence first)", p.TenantSlug)
		}
		licenceID = *lic
		return nil
	})
	if err != nil {
		return err
	}
	say("tenant_id", tenantID.String())
	say("licence_id", licenceID.String())

	err = pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT id FROM brands WHERE slug = $1 AND tenant_id = $2`, p.BrandSlug, tenantID).Scan(&brandID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("brand %q does not exist under tenant %q", p.BrandSlug, p.TenantSlug)
			}
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	say("brand_id", brandID.String())

	actorID := p.ActorID
	if actorID == uuid.Nil {
		if actorID, err = soleActivePlatformAdmin(ctx, pool); err != nil {
			return err
		}
	}
	say("actor_id", actorID.String())
	actor := operatingmarket.ActorContext{ActorID: actorID, RequestID: "seed-market", UserAgent: "cmd/seed-market operator=" + p.Operator, ReasonCode: p.Reason}

	ceilingAction, err := ensureCeiling(ctx, pool, p, tenantID, licenceID, actor)
	if err != nil {
		return err
	}
	say("ceiling", ceilingAction)
	policyAction, err := ensureTenantPolicy(ctx, pool, p, tenantID, actor)
	if err != nil {
		return err
	}
	say("tenant_policy", policyAction)
	if p.DryRun {
		say("result", "dry-run: nothing was written")
		return nil
	}

	var asOf time.Time
	var bad []string
	err = pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&asOf); err != nil {
			return err
		}
		casino, sportsbook := "casino", "sportsbook"
		for _, op := range []struct {
			name, code string
			product    *string
		}{
			{"registration", "registration", nil},
			{"deposit", "deposit", nil},
			{"casino_wagering", "wagering", &casino},
			{"sportsbook_wagering", "wagering", &sportsbook},
		} {
			res, err := operatingmarket.ResolveOperatingCountryPolicy(ctx, tx, operatingmarket.Query{
				TenantID: tenantID, BrandID: &brandID, CountryCode: p.Country, OperationCode: op.code, ProductCode: op.product, AsOf: asOf,
			})
			if err != nil {
				return fmt.Errorf("resolve %s: %w", op.name, err)
			}
			say("resolves_"+op.name, string(res.Outcome()))
			if !res.Permitted() {
				bad = append(bad, op.name+"="+string(res.Outcome()))
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(bad) > 0 {
		return fmt.Errorf("the rows are written but the gate is NOT open for: %s (a narrowing brand/operation row, an expired or suspended licence, or a ceiling the licence does not have)", strings.Join(bad, ", "))
	}
	say("result", "OK: registration, deposit, casino and sportsbook wagering are permitted for the brand in "+p.Country)
	return nil
}

// TenantStatusAllowsSeeding: a suspended or closed tenant is never seeded. pending_launch is allowed because the operating-market rows
// are a prerequisite of the launch readiness check (R-MARKET-CONFIG) that precedes activation.
func TenantStatusAllowsSeeding(status string) bool { return status == "active" || status == "pending_launch" }

// RequireActivePlatformAdmin checks, inside tx (a platform-admin scoped transaction), that id is an ACTIVE platform_admin
// (tenant_id IS NULL) and takes a share lock on the row so it cannot be offboarded between the check and the write.
func RequireActivePlatformAdmin(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM staff_users WHERE id = $1 AND tenant_id IS NULL AND role = 'platform_admin' AND status = 'active' FOR SHARE`, id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("refused: actor %s is not an active platform_admin (the actor recorded in the audit rows must be one)", id)
	}
	return err
}

func soleActivePlatformAdmin(ctx context.Context, pool *db.Pool) (uuid.UUID, error) {
	var ids []uuid.UUID
	err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM staff_users WHERE tenant_id IS NULL AND role = 'platform_admin' AND status = 'active' ORDER BY created_at, id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("look up the platform admin: %w", err)
	}
	if len(ids) != 1 {
		return uuid.Nil, fmt.Errorf("found %d active platform admins: pass the actor id explicitly (the actor recorded in the audit rows must be one named staff member)", len(ids))
	}
	return ids[0], nil
}

// SoleActivePlatformAdmin is exported for cmd/seed-staging-world.
func SoleActivePlatformAdmin(ctx context.Context, pool *db.Pool) (uuid.UUID, error) {
	return soleActivePlatformAdmin(ctx, pool)
}

func ensureCeiling(ctx context.Context, pool *db.Pool, p MarketParams, tenantID, licenceID uuid.UUID, actor operatingmarket.ActorContext) (string, error) {
	var action string
	err := pool.WithPlatformAdmin(ctx, actor.ActorID, func(ctx context.Context, tx pgx.Tx) error {
		// check and write in ONE transaction, with the actor and the tenant locked (SRP3-3)
		if err := RequireActivePlatformAdmin(ctx, tx, actor.ActorID); err != nil {
			return err
		}
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1 FOR SHARE`, tenantID).Scan(&status); err != nil {
			return err
		}
		if !TenantStatusAllowsSeeding(status) {
			return fmt.Errorf("refused: tenant is %s", status)
		}
		var state, st string
		err := tx.QueryRow(ctx, `SELECT state, status FROM licence_country_ceilings WHERE licence_id = $1 AND country_code = $2 AND effective_to IS NULL FOR UPDATE`, licenceID, p.Country).Scan(&state, &st)
		switch {
		case err == nil:
			if state == "enabled" && st == "active" {
				action = "already present (enabled, active); nothing written"
				return nil
			}
			return fmt.Errorf("%w: ceiling for %s is %s/%s; re-enabling a closed country is a governed decision, not this tool's", ErrConflict, p.Country, state, st)
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if p.DryRun {
			action = "would be created"
			return nil
		}
		rec, err := operatingmarket.CreateLicenceCountryCeilingVersion(ctx, tx, operatingmarket.CreateLicenceCountryCeilingVersionParams{
			LicenceID: licenceID, CountryCode: p.Country, State: operatingmarket.StateEnabled, Status: operatingmarket.StatusActive,
			AuthorizationReference: p.AuthRef, Actor: actor,
		})
		if err != nil {
			return err
		}
		action = "created version " + rec.ID.String()
		return nil
	})
	return action, err
}

func ensureTenantPolicy(ctx context.Context, pool *db.Pool, p MarketParams, tenantID uuid.UUID, actor operatingmarket.ActorContext) (string, error) {
	var action string
	err := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var tstatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1`, tenantID).Scan(&tstatus); err != nil {
			return err
		}
		if !TenantStatusAllowsSeeding(tstatus) {
			return fmt.Errorf("refused: tenant is %s", tstatus)
		}
		var state, status string
		err := tx.QueryRow(ctx, `SELECT state, status FROM operating_country_policies
			WHERE tenant_id = $1 AND scope_kind = 'tenant' AND country_code = $2 AND effective_to IS NULL FOR UPDATE`, tenantID, p.Country).Scan(&state, &status)
		switch {
		case err == nil:
			if state == "enabled" && status == "active" {
				action = "already present (enabled, active); nothing written"
				return nil
			}
			return fmt.Errorf("%w: tenant policy for %s is %s/%s; re-enabling a closed country is a governed decision, not this tool's", ErrConflict, p.Country, state, status)
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if p.DryRun {
			action = "would be created"
			return nil
		}
		rec, err := operatingmarket.CreateOperatingCountryPolicyVersion(ctx, tx, operatingmarket.CreateOperatingCountryPolicyVersionParams{
			Scope: operatingmarket.ScopeTenant, TenantID: tenantID, CountryCode: p.Country,
			State: operatingmarket.StateEnabled, Status: operatingmarket.StatusActive, AuthorizationReference: p.AuthRef, Actor: actor,
		})
		if err != nil {
			return err
		}
		action = "created version " + rec.ID.String()
		return nil
	})
	return action, err
}
