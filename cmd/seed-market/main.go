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
// transaction, the migration 0076 constraints); this tool adds no SQL write of its own. The logic lives in
// internal/stagingseed (shared with cmd/seed-staging-world). It is the one-off Go call the runbook (section 2) describes, not
// a route, and it is NOT part of any HTTP surface.
//
// Safety properties:
//   - FIRST, before it reads or writes anything else (also with -dry-run): APP_ENV must be development or staging AND the
//     database_environment anchor must be staging, development or test. No anchor row means PRODUCTION and is refused
//     (security review SRP3-1);
//   - -operator and -reason are required; an enabling write needs a mandatory -authorization-reference;
//   - the tenant must be active or pending_launch (never suspended or closed); the actor (-actor-id, or the sole active platform admin) must be an ACTIVE platform_admin,
//     checked and locked in the same transaction as the ceiling write (SRP3-3);
//   - IDEMPOTENT: an identical open version (enabled + active) is reported and left alone; a DIFFERENT open version
//     (disabled / withdrawn) is REFUSED, because turning a closed country back on is a governed decision this tool does
//     not make; it never writes a duplicate version;
//   - the country must equal MOCK_GEOLOCATION_COUNTRY when that is set; MKT-DUAL-1 (four-eyes on market enablement) is still
//     OPEN: two humans review the parameters by process;
//   - the database password never appears here: DATABASE_URL carries none (PGPASSFILE / PGPASSWORD supply it).
//
// After the writes it re-resolves the policy for registration, deposit, casino wagering and sportsbook wagering for the
// brand and exits non-zero unless every one is permitted. Output is line oriented ("SEEDMARKET|key|value"); no secret.
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

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/stagingseed"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "seed-market: fatal:", err)
		os.Exit(1)
	}
}

// settings is the validated, DB-independent input.
type settings struct {
	stagingseed.MarketParams
	databaseURL string
}

func parseSettings(args []string, getenv func(string) string) (settings, error) {
	var s settings
	fs := flag.NewFlagSet("seed-market", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	actor := fs.String("actor-id", "", "optional: staff id of the platform admin recorded as the actor (default: the sole active platform admin)")
	fs.StringVar(&s.TenantSlug, "tenant-slug", "", "slug of the (synthetic) tenant (required)")
	fs.StringVar(&s.BrandSlug, "brand-slug", "", "slug of the (synthetic) brand of that tenant (required)")
	fs.StringVar(&s.Country, "country", "", "ISO-3166-1 alpha-2 country code (required; must equal MOCK_GEOLOCATION_COUNTRY when that is set)")
	fs.StringVar(&s.AuthRef, "authorization-reference", "", "the change ticket / authorisation reference recorded on both rows (required)")
	fs.StringVar(&s.Operator, "operator", "", "who runs this job (name or ticket handle); required")
	fs.StringVar(&s.Reason, "reason", "", "why (reason code or ticket); required; recorded as the audit reason code")
	fs.BoolVar(&s.DryRun, "dry-run", false, "validate and report what would be written; write nothing")
	if err := fs.Parse(args); err != nil {
		return s, fmt.Errorf("usage: seed-market -tenant-slug S -brand-slug B -country CC -authorization-reference REF -operator OP -reason R [-actor-id UUID] [-dry-run]: %w", err)
	}
	if fs.NArg() != 0 {
		return s, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if err := stagingseed.CheckAppEnv(getenv("APP_ENV")); err != nil {
		return s, err
	}
	if err := s.MarketParams.Validate(); err != nil {
		return s, err
	}
	if mock := strings.TrimSpace(getenv("MOCK_GEOLOCATION_COUNTRY")); mock != "" && mock != s.Country {
		return s, fmt.Errorf("-country %s differs from MOCK_GEOLOCATION_COUNTRY %s: the gate would refuse every request", s.Country, mock)
	}
	if *actor != "" {
		id, err := uuid.Parse(*actor)
		if err != nil || id == uuid.Nil {
			return s, errors.New("-actor-id must be a valid non-nil UUID")
		}
		s.ActorID = id
	}
	s.databaseURL = getenv("DATABASE_URL")
	if s.databaseURL == "" {
		return s, errors.New("DATABASE_URL is required (no password in it: PGPASSFILE / PGPASSWORD supply the credential)")
	}
	return s, nil
}

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
	return stagingseed.EnsureMarket(ctx, pool, s.MarketParams, out)
}
