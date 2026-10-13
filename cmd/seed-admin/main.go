// Command seed-admin creates the platform's first platform_admin staff
// user. This is a one-time bootstrap operation, deliberately NOT exposed
// through the HTTP API (there is no "create the first admin" endpoint,
// since any such endpoint would either need to be open to the internet
// pre-auth - a standing privilege-escalation risk - or itself require an
// existing admin, which is exactly the chicken-and-egg problem this tool
// exists to break). It connects to the database directly, the same way
// cmd/migrate does - see docs/decisions/0014-service-identity-pattern.md
// for why that's the platform's standard shape for a platform-level job,
// not a special case invented here.
//
// The password is read from an environment variable, never a CLI flag
// (flags are visible in process listings/shell history; an env var, set
// via a secrets manager or a one-off `SEED_ADMIN_PASSWORD=... go run
// ./cmd/seed-admin`, is not).
//
// ADR 0119 S3 (staff MFA): every staff login needs a second factor, so a
// platform admin created here cannot log in until it enrols one. seed-admin
// therefore also issues a single-use `bootstrap` enrolment token (24 h),
// printed ONCE on standard output - the protected output channel of the
// deployment job it runs as (ADR 0119 section 11); it is never logged and
// never stored (the database holds only its keyed MAC). The admin redeems it
// at POST /v1/staff/auth/enrol/redeem together with the password, then
// enrols a TOTP authenticator. The tool needs the STAFF_MFA_* key families
// (the same secret-store injection as platform-api) and refuses to run
// without them (staffmfa.LoadKeys, S3 entry gate 21.7 (a)).
//
// Modes:
//   - default: create a new platform_admin (-email, SEED_ADMIN_PASSWORD) and
//     issue its bootstrap token;
//   - -issue-bootstrap: issue a fresh bootstrap token for an EXISTING active
//     staff member (-email; plus -tenant-slug for a tenant staff member, omitted
//     for platform staff) that has NO active second factor - the S3 cutover
//     re-seed of accounts created before S3 (there is no invite or reset path
//     before S6), and the break-glass path between S3 and S6
//     (docs/runbooks/staff-mfa-s3-cutover.md). An account WITH an active factor
//     (including a hard-locked one) is refused: that is an MFA reset (S6), and
//     until S6 the platform break-glass is creating an ADDITIONAL admin. The
//     token is still useless without the account's password.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/staffmfa"
	"github.com/Diansalas/igaming-platform/internal/staffmfa/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "seed-admin: fatal:", err)
		os.Exit(1)
	}
}

// errHasActiveFactor: -issue-bootstrap for an account that already has an
// active second factor (an MFA reset, ADR 0119 S6).
var errHasActiveFactor = errors.New("the platform_admin already has an active second factor; a bootstrap token is not a reset (ADR 0119 S6) - for break-glass, create an additional platform_admin instead (docs/runbooks/staff-mfa-s3-cutover.md)")

func run() error {
	email := flag.String("email", "", "platform admin email (required)")
	personID := flag.String("person-id", "", "optional: link this platform_admin to an existing person id (mutually exclusive with -create-person)")
	createPerson := flag.Bool("create-person", false, "optional: create a new person cluster and link this platform_admin to it (mutually exclusive with -person-id)")
	issueBootstrap := flag.Bool("issue-bootstrap", false, "issue a bootstrap enrolment token for an EXISTING active staff member with no active second factor (no account is created)")
	tenantSlug := flag.String("tenant-slug", "", "with -issue-bootstrap: the tenant of a tenant staff member (omit for platform staff)")
	operator := flag.String("operator", "", "who runs this job (name or ticket handle); recorded in the audit rows; required with -issue-bootstrap")
	reason := flag.String("reason", "", "why (reason code or ticket); recorded in the audit rows; required with -issue-bootstrap")
	flag.Parse()

	if *email == "" {
		return fmt.Errorf("usage: seed-admin -email=admin@example.com (password read from SEED_ADMIN_PASSWORD) [-issue-bootstrap]")
	}
	if *personID != "" && *createPerson {
		return fmt.Errorf("-person-id and -create-person are mutually exclusive")
	}
	if *issueBootstrap && (*personID != "" || *createPerson) {
		return fmt.Errorf("-issue-bootstrap creates no account: -person-id / -create-person do not apply")
	}
	if *tenantSlug != "" && !*issueBootstrap {
		return fmt.Errorf("-tenant-slug applies only to -issue-bootstrap (seed-admin creates platform admins only)")
	}
	// ADR 0119 S3 security review R4: the break-glass re-issue is a mutating
	// administrative action, so it records who and why (CLAUDE.md audit rule).
	op := operatorContext{Operator: *operator, Reason: *reason}
	if err := op.validate(*issueBootstrap); err != nil {
		return err
	}

	// ADR 0119 S3 security review C2: the same staff MFA configuration rules
	// as platform-api's config.Load (published placeholders refused unless
	// APP_ENV is explicitly "development", key families pairwise distinct),
	// then entry gate 21.7 (a): the one key constructor, which runs
	// staffmfa.VerifyStartup, before anything reads the keys or the database.
	if err := config.ValidateStaffMFAEnv(); err != nil {
		return fmt.Errorf("staff MFA configuration: %w", err)
	}
	keys, err := staffmfa.LoadKeys(os.Getenv(staffmfa.EnvFactorActiveKID), os.Getenv(staffmfa.EnvFactorKeys),
		os.Getenv(staffmfa.EnvMACActiveKID), os.Getenv(staffmfa.EnvMACKeys))
	if err != nil {
		return fmt.Errorf("staff MFA keys: %w", err)
	}
	mfaStore, err := store.New(keys)
	if err != nil {
		return err
	}

	var explicitPersonID uuid.UUID
	if *personID != "" {
		explicitPersonID, err = uuid.Parse(*personID)
		if err != nil {
			return fmt.Errorf("-person-id must be a valid UUID: %w", err)
		}
	}
	var passwordHash string
	if !*issueBootstrap {
		password := os.Getenv("SEED_ADMIN_PASSWORD")
		if password == "" {
			return fmt.Errorf("SEED_ADMIN_PASSWORD environment variable is required")
		}
		if len(password) < 12 {
			return fmt.Errorf("SEED_ADMIN_PASSWORD must be at least 12 characters for a platform_admin account")
		}
		if passwordHash, err = auth.HashPassword(password); err != nil {
			return fmt.Errorf("hash password: %w", err)
		}
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, databaseURL, 2, 5*time.Second)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()

	if *issueBootstrap {
		return reissueBootstrap(ctx, pool, mfaStore, keys, *tenantSlug, *email, op)
	}

	var staff identity.StaffUser
	var tok staffmfa.Token
	err = pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var resolvedPersonID *uuid.UUID
		switch {
		case *createPerson:
			// Stage 4H-B0-R6 fix: born person-linked, rather than always
			// nil. A fresh person cluster in the same transaction as the
			// staff row, so the bootstrap never leaves an orphaned
			// person if staff creation later fails (e.g. email taken).
			linkedPerson, err := identity.CreatePerson(ctx, tx)
			if err != nil {
				return err
			}
			resolvedPersonID = &linkedPerson.ID
		case *personID != "":
			resolvedPersonID = &explicitPersonID
		}

		var err error
		staff, err = identity.CreateStaffUser(ctx, tx, uuid.Nil, *email, passwordHash, identity.StaffRolePlatformAdmin, resolvedPersonID)
		if err != nil {
			return err
		}
		metadata := op.metadata(map[string]any{"tool": "cmd/seed-admin"})
		if resolvedPersonID != nil {
			metadata["person_id"] = resolvedPersonID.String()
			metadata["person_created"] = *createPerson
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			ActorType:  audit.ActorSystem,
			Action:     "staff.platform_admin_bootstrapped",
			TargetType: "staff_user", TargetID: staff.ID.String(),
			Outcome:  audit.OutcomeSuccess,
			Metadata: metadata,
		}); err != nil {
			return err
		}
		tok, err = issueBootstrapToken(ctx, tx, mfaStore, staffmfa.StaffRef{StaffID: staff.ID}, "create", op)
		return err
	})
	if err != nil {
		return fmt.Errorf("create platform admin: %w", err)
	}

	if staff.PersonID != nil {
		fmt.Printf("seed-admin: created platform_admin %s (id: %s), linked to person %s\n", staff.Email, staff.ID, staff.PersonID)
	} else {
		fmt.Printf("seed-admin: created platform_admin %s (id: %s) - WARNING: no person_id set; this account cannot act as a four-eyes requester/approver on any control requiring resolved person linkage (e.g. Asset Registry) until remediated via POST /v1/admin/platform-staff/{staffID}/person-link\n", staff.Email, staff.ID)
	}
	printBootstrapToken(tok)
	return nil
}

// issueBootstrapToken issues the keyed bootstrap enrolment token in tx (which
// is scoped to the staff member's tenant, or platform-scoped) and audits the
// issue in that same scope. The token is returned for the single print; the
// audit row never carries it.
func issueBootstrapToken(ctx context.Context, tx pgx.Tx, st *store.Store, staff staffmfa.StaffRef, mode string, op operatorContext) (staffmfa.Token, error) {
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		return staffmfa.Token{}, err
	}
	tok, err := st.IssueEnrolmentToken(ctx, tx, staff, store.EnrolBootstrap, nil, nil, now)
	if err != nil {
		return staffmfa.Token{}, fmt.Errorf("issue bootstrap enrolment token: %w", err)
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: staff.TenantID, ActorType: audit.ActorSystem, Action: "staff.bootstrap_token_issued",
		TargetType: "staff_user", TargetID: staff.StaffID.String(), Outcome: audit.OutcomeSuccess,
		Metadata: op.metadata(map[string]any{"tool": "cmd/seed-admin", "mode": mode, "ttl_hours": int(staffmfa.BootstrapTokenTTL.Hours())}),
	}); err != nil {
		return staffmfa.Token{}, err
	}
	return tok, nil
}

// reissueBootstrap is -issue-bootstrap: an existing active staff member with no
// active second factor gets a fresh bootstrap token (any earlier live one is
// revoked by the store). The tenant comes from -tenant-slug (none = platform
// staff); the staff member is resolved by email inside that scope only.
func reissueBootstrap(ctx context.Context, pool *db.Pool, st *store.Store, keys *staffmfa.Keys, tenantSlug, email string, op operatorContext) error {
	factor, err := store.NewTOTPFactor(keys, staffmfa.DefaultIssuer)
	if err != nil {
		return err
	}
	tenantID := uuid.Nil
	if tenantSlug != "" {
		t, err := identity.GetTenantBySlug(ctx, pool, tenantSlug)
		if err != nil {
			return fmt.Errorf("-tenant-slug: %w", err)
		}
		tenantID = t.ID
	}
	var staff identity.StaffUser
	var tok staffmfa.Token
	scope := pool.WithoutTenant
	if tenantID != uuid.Nil {
		scope = func(ctx context.Context, fn db.TxFunc) error { return pool.WithTenant(ctx, tenantID, fn) }
	}
	err = scope(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		staff, err = identity.GetStaffUserByEmail(ctx, tx, email)
		if err != nil {
			return err
		}
		if staff.TenantID != tenantID || staff.Status != "active" {
			return fmt.Errorf("-issue-bootstrap applies only to an active staff member of the given scope")
		}
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
			return err
		}
		state, err := factor.ActiveFactorState(ctx, tx, staffmfa.StaffRef{StaffID: staff.ID, TenantID: tenantID}, now)
		if err != nil {
			return err
		}
		if state.Exists {
			return errHasActiveFactor
		}
		tok, err = issueBootstrapToken(ctx, tx, st, staffmfa.StaffRef{StaffID: staff.ID, TenantID: tenantID}, "reissue", op)
		return err
	})
	if err != nil {
		return fmt.Errorf("issue bootstrap token: %w", err)
	}
	fmt.Printf("seed-admin: issued a bootstrap enrolment token for %s %s (id: %s)\n", staff.Role, staff.Email, staff.ID)
	printBootstrapToken(tok)
	return nil
}

// operatorContext is the -operator / -reason pair recorded in every audit row
// this job writes (ADR 0119 S3 security review R4). Neither is a credential;
// the token itself is never recorded.
type operatorContext struct {
	Operator, Reason string
}

const maxOperatorField = 200

func (o operatorContext) validate(required bool) error {
	for name, v := range map[string]string{"-operator": o.Operator, "-reason": o.Reason} {
		if v == "" {
			if required {
				return fmt.Errorf("%s is required with -issue-bootstrap (recorded in the audit row)", name)
			}
			continue
		}
		if len(v) > maxOperatorField || strings.TrimSpace(v) != v || strings.IndexFunc(v, unicode.IsControl) >= 0 || !utf8.ValidString(v) {
			return fmt.Errorf("%s must be 1-%d bytes of UTF-8 without control characters or surrounding whitespace", name, maxOperatorField)
		}
	}
	return nil
}

func (o operatorContext) metadata(m map[string]any) map[string]any {
	if o.Operator != "" {
		m["operator"] = o.Operator
	}
	if o.Reason != "" {
		m["reason"] = o.Reason
	}
	return m
}

// printBootstrapToken is the single delivery of the bootstrap token: standard
// output of the deployment job (its protected output channel, ADR 0119
// section 11). It is the only RevealForDelivery call outside internal/httpserver
// (S3 entry gate G5 allow-list).
func printBootstrapToken(tok staffmfa.Token) {
	fmt.Printf("seed-admin: bootstrap enrolment token (single use, valid %s; shown ONCE, never logged - redeem at POST /v1/staff/auth/enrol/redeem with the account's password):\n%s\n",
		staffmfa.BootstrapTokenTTL, tok.RevealForDelivery())
}
