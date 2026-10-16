// Command seed-market writes the two operating-market rows a SYNTHETIC staging (or development) brand needs before the
// A8 market gate admits registration, deposit, casino launch and sportsbook placement (ADR 0045,
// docs/runbooks/market-blocking-configuration.md sections 2 and 3):
//
//  1. a licence-country CEILING version (licence_country_ceilings): "this licence permits country CC", written with
//     operatingmarket.CreateLicenceCountryCeilingVersion inside a platform-admin transaction;
//  2. a tenant-scope operating-country POLICY version (operating_country_policies): "this tenant opts country CC in",
//     written with operatingmarket.CreateOperatingCountryPolicyVersion inside a tenant transaction.
//
// Both writes go through the reviewed library calls (close-open-then-insert, the mandatory audit_log row in the same
// transaction, the migration 0076 constraints); this tool adds no SQL write of its own. It is the one-off Go call the
// runbook (section 2) describes, not a route, and it is NOT part of any HTTP surface.
//
// Safety properties:
//   - -operator and -reason are required and recorded in the audit rows' reason code (the library writes the audit rows);
//   - an enabling write needs a mandatory -authorization-reference (the library refuses an empty one);
//   - it refuses APP_ENV=production, and refuses unless APP_ENV is development or staging (synthetic environments only);
//   - IDEMPOTENT: an identical open version (enabled + active) is reported and left alone; a DIFFERENT open version
//     (disabled / withdrawn) is REFUSED, because turning a closed country back on is a governed decision this tool does
//     not make (use the runbook's reviewed path) - it never writes a duplicate version;
//   - the country must equal MOCK_GEOLOCATION_COUNTRY when that is set (a different value would leave the gate refusing
//     every request); MKT-DUAL-1 (four-eyes on market enablement) is still OPEN: two humans review the parameters by process;
//   - the database password never appears here: DATABASE_URL carries none (PGPASSFILE / PGPASSWORD supply it).
//
// After the writes it re-resolves the policy for registration, deposit, casino wagering and sportsbook wagering for the
// brand and exits non-zero unless every one is permitted (it cannot claim success on rows that do not open the gate).
//
// Output is line oriented ("SEEDMARKET|key|value"); it prints no secret.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "seed-market: fatal:", err)
		os.Exit(1)
	}
}

const maxField = 200

// settings is the validated, DB-independent input.
type settings struct {
	tenantSlug, brandSlug, country, authRef, operator, reason string
	actorID                                                   uuid.UUID // uuid.Nil = look up the sole active platform admin
	dryRun                                                    bool
	databaseURL                                               string
}

func parseSettings(args []string, getenv func(string) string) (settings, error) {
	var s settings
	fs := flag.NewFlagSet("seed-market", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	actor := fs.String("actor-id", "", "optional: staff id of the platform admin recorded as the actor (default: the sole active platform admin)")
	fs.StringVar(&s.tenantSlug, "tenant-slug", "", "slug of the (synthetic) tenant (required)")
	fs.StringVar(&s.brandSlug, "brand-slug", "", "slug of the (synthetic) brand of that tenant (required)")
	fs.StringVar(&s.country, "country", "", "ISO-3166-1 alpha-2 country code (required; must equal MOCK_GEOLOCATION_COUNTRY when that is set)")
	fs.StringVar(&s.authRef, "authorization-reference", "", "the change ticket / authorisation reference recorded on both rows (required)")
	fs.StringVar(&s.operator, "operator", "", "who runs this job (name or ticket handle); required")
	fs.StringVar(&s.reason, "reason", "", "why (reason code or ticket); required; recorded as the audit reason code")
	fs.BoolVar(&s.dryRun, "dry-run", false, "validate and report what would be written; write nothing")
	if err := fs.Parse(args); err != nil {
		return s, fmt.Errorf("usage: seed-market -tenant-slug S -brand-slug B -country CC -authorization-reference REF -operator OP -reason R [-actor-id UUID] [-dry-run]: %w", err)
	}
	if fs.NArg() != 0 {
		return s, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	env := strings.TrimSpace(getenv("APP_ENV"))
	switch env {
	case "development", "staging":
	case "production":
		return s, errors.New("refused: APP_ENV=production. This tool writes market rows for synthetic development/staging data only; production market configuration is a reviewed, human-authorised change (market-blocking-configuration.md)")
	default:
		return s, fmt.Errorf("refused: APP_ENV must be development or staging (got %q)", env)
	}
	for name, v := range map[string]string{"-tenant-slug": s.tenantSlug, "-brand-slug": s.brandSlug, "-authorization-reference": s.authRef, "-operator": s.operator, "-reason": s.reason} {
		if err := checkField(name, v); err != nil {
			return s, err
		}
	}
	if !validation.IsISO3166Alpha2(s.country) {
		return s, errors.New("-country must be a valid, uppercase ISO-3166-1 alpha-2 code")
	}
	if mock := strings.TrimSpace(getenv("MOCK_GEOLOCATION_COUNTRY")); mock != "" && mock != s.country {
		return s, fmt.Errorf("-country %s differs from MOCK_GEOLOCATION_COUNTRY %s: the gate would refuse every request", s.country, mock)
	}
	if *actor != "" {
		id, err := uuid.Parse(*actor)
		if err != nil || id == uuid.Nil {
			return s, errors.New("-actor-id must be a valid non-nil UUID")
		}
		s.actorID = id
	}
	s.databaseURL = getenv("DATABASE_URL")
	if s.databaseURL == "" {
		return s, errors.New("DATABASE_URL is required (no password in it: PGPASSFILE / PGPASSWORD supply the credential)")
	}
	return s, nil
}

// checkField mirrors cmd/seed-admin's operator/reason hygiene so an audit row cannot visually misrepresent who ran the
// job or why (control, format and line-separator characters are refused), and bounds every free-text input.
func checkField(name, v string) error {
	if v == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(v) > maxField || strings.TrimSpace(v) != v || !utf8.ValidString(v) || strings.IndexFunc(v, unsafeRune) >= 0 {
		return fmt.Errorf("%s must be 1-%d bytes of UTF-8 without control, format or line-separator characters or surrounding whitespace", name, maxField)
	}
	return nil
}

func unsafeRune(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.In(r, unicode.Zl, unicode.Zp)
}

var errConflict = errors.New("refused: a different open version exists")

func run(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	s, err := parseSettings(args, getenv)
	if err != nil {
		return err
	}
	pool, err := db.Connect(ctx, s.databaseURL, 2, 10*time.Second)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()
	return apply(ctx, pool, s, out)
}

// apply is the DB part, separate from run so the integration tests can drive it with their own pool.
func apply(ctx context.Context, pool *db.Pool, s settings, out io.Writer) error {
	say := func(k, v string) { fmt.Fprintf(out, "SEEDMARKET|%s|%s\n", k, v) }
	say("environment", "synthetic-only")

	var tenantID, licenceID, brandID uuid.UUID
	var tenantStatus string
	err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var lic *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id, licence_id, status FROM tenants WHERE slug = $1`, s.tenantSlug).Scan(&tenantID, &lic, &tenantStatus); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("tenant %q does not exist", s.tenantSlug)
			}
			return err
		}
		if lic == nil {
			return fmt.Errorf("tenant %q has no licence_id: the licence ceiling needs one (a platform admin attaches the licence first)", s.tenantSlug)
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
		if err := tx.QueryRow(ctx, `SELECT id FROM brands WHERE slug = $1 AND tenant_id = $2`, s.brandSlug, tenantID).Scan(&brandID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("brand %q does not exist under tenant %q", s.brandSlug, s.tenantSlug)
			}
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	say("brand_id", brandID.String())

	actorID := s.actorID
	if actorID == uuid.Nil {
		actorID, err = soleActivePlatformAdmin(ctx, pool)
		if err != nil {
			return err
		}
	}
	say("actor_id", actorID.String())

	actor := operatingmarket.ActorContext{ActorID: actorID, RequestID: "seed-market", UserAgent: "cmd/seed-market operator=" + s.operator, ReasonCode: s.reason}

	// ---- 1. licence ceiling (platform scope) ------------------------------------------------------------------
	ceilingAction, err := ensureCeiling(ctx, pool, s, licenceID, actor)
	if err != nil {
		return err
	}
	say("ceiling", ceilingAction)

	// ---- 2. tenant opt-in (tenant scope) ----------------------------------------------------------------------
	policyAction, err := ensureTenantPolicy(ctx, pool, s, tenantID, actor)
	if err != nil {
		return err
	}
	say("tenant_policy", policyAction)
	if s.dryRun {
		say("result", "dry-run: nothing was written")
		return nil
	}

	// ---- 3. prove the gate would open ------------------------------------------------------------------------
	var asOf time.Time
	bad := []string{}
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
				TenantID: tenantID, BrandID: &brandID, CountryCode: s.country, OperationCode: op.code, ProductCode: op.product, AsOf: asOf,
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
	say("result", "OK: registration, deposit, casino and sportsbook wagering are permitted for the brand in "+s.country)
	return nil
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
		return uuid.Nil, fmt.Errorf("found %d active platform admins: pass -actor-id explicitly (the actor recorded in the audit rows must be one named staff member)", len(ids))
	}
	return ids[0], nil
}

// ensureCeiling returns "created", "already present" or an error (errConflict-wrapped when a different version is open).
func ensureCeiling(ctx context.Context, pool *db.Pool, s settings, licenceID uuid.UUID, actor operatingmarket.ActorContext) (string, error) {
	var action string
	err := pool.WithPlatformAdmin(ctx, actor.ActorID, func(ctx context.Context, tx pgx.Tx) error {
		var state, status string
		err := tx.QueryRow(ctx, `SELECT state, status FROM licence_country_ceilings WHERE licence_id = $1 AND country_code = $2 AND effective_to IS NULL`, licenceID, s.country).Scan(&state, &status)
		switch {
		case err == nil:
			if state == "enabled" && status == "active" {
				action = "already present (enabled, active); nothing written"
				return nil
			}
			return fmt.Errorf("%w: ceiling for %s is %s/%s; re-enabling a closed country is a governed decision, not this tool's", errConflict, s.country, state, status)
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if s.dryRun {
			action = "would be created"
			return nil
		}
		rec, err := operatingmarket.CreateLicenceCountryCeilingVersion(ctx, tx, operatingmarket.CreateLicenceCountryCeilingVersionParams{
			LicenceID: licenceID, CountryCode: s.country, State: operatingmarket.StateEnabled, Status: operatingmarket.StatusActive,
			AuthorizationReference: s.authRef, Actor: actor,
		})
		if err != nil {
			return err
		}
		action = "created version " + rec.ID.String()
		return nil
	})
	return action, err
}

func ensureTenantPolicy(ctx context.Context, pool *db.Pool, s settings, tenantID uuid.UUID, actor operatingmarket.ActorContext) (string, error) {
	var action string
	err := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var state, status string
		err := tx.QueryRow(ctx, `SELECT state, status FROM operating_country_policies
			WHERE tenant_id = $1 AND scope_kind = 'tenant' AND country_code = $2 AND effective_to IS NULL`, tenantID, s.country).Scan(&state, &status)
		switch {
		case err == nil:
			if state == "enabled" && status == "active" {
				action = "already present (enabled, active); nothing written"
				return nil
			}
			return fmt.Errorf("%w: tenant policy for %s is %s/%s; re-enabling a closed country is a governed decision, not this tool's", errConflict, s.country, state, status)
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if s.dryRun {
			action = "would be created"
			return nil
		}
		rec, err := operatingmarket.CreateOperatingCountryPolicyVersion(ctx, tx, operatingmarket.CreateOperatingCountryPolicyVersionParams{
			Scope: operatingmarket.ScopeTenant, TenantID: tenantID, CountryCode: s.country,
			State: operatingmarket.StateEnabled, Status: operatingmarket.StatusActive, AuthorizationReference: s.authRef, Actor: actor,
		})
		if err != nil {
			return err
		}
		action = "created version " + rec.ID.String()
		return nil
	})
	return action, err
}
