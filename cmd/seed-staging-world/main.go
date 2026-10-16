// Command seed-staging-world provisions the SYNTHETIC world the staging acceptance suite (deploy/aws/acceptance) needs and the
// platform offers no supported staging path for (finding SA-1 of docs/plans/staging-acceptance-suite-record.md): an ACTIVE
// synthetic tenant and brand with operating-market rows, an optional second ACTIVE tenant for the isolation checks, and the tenant
// staff accounts finance x2, compliance, tenant_admin and risk_manager with a bootstrap MFA enrolment token each.
//
// It runs as a one-off job of the runner image (runner mode seed-world), connected as the table owner/master like cmd/seed-admin,
// and uses the platform's own services: identity.CreatePerson / CreateStaffUser, the staff MFA store's bootstrap enrolment token,
// the operating-market library calls (via internal/stagingseed), and the SANCTIONED owner-provisioning path for ACTIVE rows (ADR 0112,
// migration 0128: the table owner may insert an active tenant/brand and the database records an owner_provisioned launch
// transition). It never activates anything that already exists in another state (activation of a pending subject is a governed launch
// decision).
//
// TWO PHASES, so a credential can never be lost:
//
//	-phase plan   READ-ONLY. Gates, then reports which staff accounts are missing and writes generated one-time passwords for exactly those
//	              to -credentials-out (0600). The runner uploads that file to the Secrets Manager secret igaming-staging/seed-world-passwords.
//	-phase apply  Creates everything from -credentials-in and writes passwords AND bootstrap tokens to -credentials-out (0600); the
//	              runner uploads it again (merged). Idempotent: existing tenants, brands, market rows and staff are left alone.
//
// Gates (before anything else, both phases): APP_ENV development or staging AND the database_environment anchor staging, development or
// test (no anchor row = production = refused; SRP3-1); mandatory -operator, -reason and -authorization-reference; the actor is an ACTIVE
// platform_admin. Credentials are NEVER printed or logged: standard output carries "SEEDWORLD|key|value" status lines only. The staff MFA
// key families are required exactly as for cmd/seed-admin (staffmfa.LoadKeys).
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

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/staffmfa"
	"github.com/Diansalas/igaming-platform/internal/staffmfa/store"
	"github.com/Diansalas/igaming-platform/internal/stagingseed"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "seed-staging-world: fatal:", err)
		os.Exit(1)
	}
}

type settings struct {
	stagingseed.WorldParams
	phase, credsIn, credsOut, databaseURL, appEnv string
}

func parse(args []string, getenv func(string) string) (settings, error) {
	var s settings
	fs := flag.NewFlagSet("seed-staging-world", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	actor := fs.String("actor-id", "", "optional: staff id of the platform admin recorded as the actor (default: the sole active platform admin)")
	fs.StringVar(&s.phase, "phase", "", "plan | apply (required)")
	fs.StringVar(&s.TenantSlug, "tenant-slug", "", "slug of the synthetic tenant (required)")
	fs.StringVar(&s.BrandSlug, "brand-slug", "", "slug of its brand (required)")
	fs.StringVar(&s.SecondTenantSlug, "second-tenant-slug", "", "optional: slug of a second ACTIVE synthetic tenant (cross-tenant isolation checks)")
	fs.StringVar(&s.SecondBrandSlug, "second-brand-slug", "", "brand slug of the second tenant (required with -second-tenant-slug)")
	fs.StringVar(&s.Country, "country", "", "ISO-3166-1 alpha-2 country (must equal MOCK_GEOLOCATION_COUNTRY when that is set)")
	fs.StringVar(&s.AuthRef, "authorization-reference", "", "the change ticket / authorisation reference (required)")
	fs.StringVar(&s.Operator, "operator", "", "who runs this job; required; recorded in the audit rows")
	fs.StringVar(&s.Reason, "reason", "", "why; required; recorded in the audit rows")
	fs.StringVar(&s.credsIn, "credentials-in", "", "apply: the plan's credentials file (0600)")
	fs.StringVar(&s.credsOut, "credentials-out", "", "the credentials file to write (0600, a tmpfs path); never printed")
	if err := fs.Parse(args); err != nil {
		return s, fmt.Errorf("usage: seed-staging-world -phase plan|apply -tenant-slug S -brand-slug B -country CC -authorization-reference REF -operator OP -reason R -credentials-out FILE [-credentials-in FILE] [-second-tenant-slug S2 -second-brand-slug B2] [-actor-id UUID]: %w", err)
	}
	if fs.NArg() != 0 {
		return s, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	s.appEnv = getenv("APP_ENV")
	if err := stagingseed.CheckAppEnv(s.appEnv); err != nil {
		return s, err
	}
	if err := s.Validate(); err != nil {
		return s, err
	}
	switch s.phase {
	case "plan":
		if s.credsIn != "" {
			return s, errors.New("-credentials-in belongs to -phase apply")
		}
	case "apply":
		if s.credsIn == "" {
			return s, errors.New("-phase apply needs -credentials-in (the file written by -phase plan)")
		}
	default:
		return s, errors.New("-phase must be plan or apply")
	}
	if s.credsOut == "" {
		return s, errors.New("-credentials-out is required")
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
		return s, errors.New("DATABASE_URL is required (no password in it: PGPASSFILE supplies the credential)")
	}
	return s, nil
}

func run(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	s, err := parse(args, getenv)
	if err != nil {
		return err
	}
	var mfa *store.Store
	if s.phase == "apply" {
		// the same staff MFA configuration rules as platform-api and cmd/seed-admin (staffmfa.LoadKeys runs VerifyStartup)
		if err := config.ValidateStaffMFAEnv(); err != nil {
			return fmt.Errorf("staff MFA configuration: %w", err)
		}
		keys, err := staffmfa.LoadKeys(getenv(staffmfa.EnvFactorActiveKID), getenv(staffmfa.EnvFactorKeys), getenv(staffmfa.EnvMACActiveKID), getenv(staffmfa.EnvMACKeys))
		if err != nil {
			return fmt.Errorf("staff MFA keys: %w", err)
		}
		if mfa, err = store.New(keys); err != nil {
			return err
		}
	}
	pool, err := db.Connect(ctx, s.databaseURL, 2, 10*time.Second)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()

	switch s.phase {
	case "plan":
		c, err := stagingseed.PlanWorld(ctx, pool, s.WorldParams, s.appEnv, out)
		if err != nil {
			return err
		}
		return writeCreds(s.credsOut, c, out)
	default:
		b, err := readSecretFile(s.credsIn)
		if err != nil {
			return err
		}
		in, err := stagingseed.ReadCreds(b)
		if err != nil {
			return err
		}
		res, err := stagingseed.ApplyWorld(ctx, pool, mfa, s.WorldParams, s.appEnv, in, out)
		if err != nil {
			// credentials of accounts created before the failure are still delivered, so none is lost
			if len(res.Actors) > 0 {
				_ = writeCreds(s.credsOut, res, io.Discard)
			}
			return err
		}
		return writeCreds(s.credsOut, res, out)
	}
}
