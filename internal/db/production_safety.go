package db

import (
	"context"
	"fmt"
)

// tableOwnershipChecker is satisfied by *Pool; kept as an interface so
// VerifyRuntimeRoleInProduction is unit-testable without a real
// database connection (see production_safety_test.go).
type tableOwnershipChecker interface {
	ConnectingRoleOwnsNoTables(ctx context.Context) (bool, error)
	// ConnectingRoleHoldsTemp reports whether the connecting role can create
	// temporary objects in the current database (PRH-2 R2, ADR 0108).
	ConnectingRoleHoldsTemp(ctx context.Context) (bool, error)
	// ConnectingRoleHasMemberships reports whether the connecting role is a
	// member of any other role (pg_auth_members). A membership with INHERIT
	// FALSE, SET TRUE (PostgreSQL 16) would let it SET ROLE into a role that
	// holds TEMP without has_database_privilege seeing it (ADR 0108, I2).
	ConnectingRoleHasMemberships(ctx context.Context) (bool, error)
}

// ConnectingRoleHasMemberships implements the interface method of the same name.
func (p *Pool) ConnectingRoleHasMemberships(ctx context.Context) (bool, error) {
	var has bool
	if err := p.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_auth_members WHERE member = (SELECT oid FROM pg_roles WHERE rolname = current_user))`,
	).Scan(&has); err != nil {
		return false, fmt.Errorf("db: check connecting role's role memberships: %w", err)
	}
	return has, nil
}

// ConnectingRoleHoldsTemp reports whether the role this Pool is connected as
// holds the TEMPORARY privilege on the current database, directly, through
// PUBLIC or through a role membership (has_database_privilege covers all three;
// it is also true for a superuser). Migration 0116 revokes it from PUBLIC and
// the runtime role, but the database ACL is NOT carried by a restore or a
// CREATE DATABASE, and `migrate up` will not re-run an already-recorded 0116,
// so the end state is also verified at startup (ADR 0108).
func (p *Pool) ConnectingRoleHoldsTemp(ctx context.Context) (bool, error) {
	var holds bool
	if err := p.pool.QueryRow(ctx,
		`SELECT has_database_privilege(current_user, current_database(), 'TEMP')`,
	).Scan(&holds); err != nil {
		return false, fmt.Errorf("db: check connecting role's TEMP privilege: %w", err)
	}
	return holds, nil
}

// ConnectingRoleOwnsNoTables reports whether the role this Pool is
// connected as owns zero tables in the "public" schema, directly OR through
// membership of an owning role (pg_has_role MEMBER; ADR 0112 security S-7: a
// role that merely inherits the migration-owner role can switch off triggers and
// RLS exactly like the owner). It exists purely
// as a PLAT-ROLESPLIT-1 (docs/security/runtime-role-separation.md)
// production safety signal: table ownership is what lets a Postgres role
// bypass every row-level-security policy regardless of GRANTs (RLS never
// applies to a table's owner, and FORCE ROW LEVEL SECURITY only changes
// that for the owner's own DML - it does not stop the owner from turning
// FORCE, or RLS itself, back off, since altering either is itself an
// owner-only operation). A role that owns nothing structurally cannot do
// any of that, no matter what it is granted.
func (p *Pool) ConnectingRoleOwnsNoTables(ctx context.Context) (bool, error) {
	var ownsNothing bool
	err := p.pool.QueryRow(ctx,
		`SELECT NOT EXISTS (
			SELECT 1 FROM pg_tables
			WHERE schemaname = 'public' AND pg_has_role(current_user, tableowner::name, 'MEMBER')
		)`,
	).Scan(&ownsNothing)
	if err != nil {
		return false, fmt.Errorf("db: check connecting role's table ownership: %w", err)
	}
	return ownsNothing, nil
}

// VerifyRuntimeRoleInProduction is a fail-closed production startup
// check for PLAT-ROLESPLIT-1. It refuses to let the application start in
// production if the connecting database role owns any table it is about
// to serve traffic against - i.e. it is the migration-owner role
// ("igaming" in this repo's dev/CI convention) rather than a genuinely
// non-owning runtime role ("igaming_runtime") - see
// docs/security/runtime-role-separation.md §1 for why that specific
// distinction is the actual security boundary (not the role's name, not
// its GRANTs, only whether it owns anything). This converts "an operator
// forgot to switch the credential in production" from a silent, severe
// vulnerability (every RLS policy platform-wide silently inert for that
// connection) into a startup crash with a clear, named cause.
//
// Deliberately gated on environment being the exact string "production":
// development/CI/staging legitimately connect as the owning role today,
// and that is intentional, not a gap this check should flag. A large
// share of this repository's own integration suite (every
// internal/*/migration_*_test.go file, e.g.
// internal/jurisdiction/migration_*_test.go,
// internal/operatingmarket/migration_*_test.go,
// internal/bonus/wave3_phase2_migrations_integration_test.go) calls
// Pool.MigrateUp/Pool.MigrateDown directly and needs owner (DDL)
// privileges to run at all - see
// internal/db/runtime_role_separation_test.go's own doc comment. Config's
// own doc comment (internal/config/config.go) states environment "must
// never gate a security control" as a general rule, written for the
// Stage 1 finding this codebase learned from (a case where every
// environment DID need identical security posture, and didn't have it).
// This check is the deliberate, reviewed exception to that general rule,
// not a silent violation of it: dev/CI's own ability to run
// migration-mechanics tests structurally requires owner privileges no
// production deployment should ever grant its runtime credential, so
// "identically configured" is not achievable (or desirable) here without
// either breaking that test suite or leaving production unchecked. This
// tension is a security-review item, not something this session
// resolved unilaterally - see docs/security/runtime-role-separation.md's
// "IMPLEMENTED" section for the record of it.
func VerifyRuntimeRoleInProduction(ctx context.Context, environment string, checker tableOwnershipChecker) error {
	if environment != "production" {
		return nil
	}

	ownsNothing, err := checker.ConnectingRoleOwnsNoTables(ctx)
	if err != nil {
		return fmt.Errorf("db: production role-ownership safety check failed (fail-closed - refusing to start): %w", err)
	}
	if !ownsNothing {
		return fmt.Errorf(
			"db: refusing to start in production - the connecting database role owns tables in schema " +
				"\"public\", which means it is the migration-owner role, not a non-owning runtime role; " +
				"row-level security is silently inert for every request this process would serve (see " +
				"docs/security/runtime-role-separation.md, PLAT-ROLESPLIT-1) - point DATABASE_URL at the " +
				"runtime role's credential instead",
		)
	}

	// PRH-2 R2 (ADR 0108): the runtime role must not be able to create temporary
	// objects - a TEMP table shadows an unqualified table name used by an
	// unpinned guard function (TRIGGER-SEARCH-PATH-1). A restored or recreated
	// database silently loses migration 0116's ACL change, so fail closed here.
	holdsTemp, err := checker.ConnectingRoleHoldsTemp(ctx)
	if err != nil {
		return fmt.Errorf("db: production TEMP-privilege safety check failed (fail-closed - refusing to start): %w", err)
	}
	if holdsTemp {
		return fmt.Errorf(
			"db: refusing to start in production - the connecting database role holds the TEMPORARY privilege on " +
				"the current database, so it can shadow tables used by unpinned trigger functions (ADR 0108, " +
				"TRIGGER-SEARCH-PATH-1); as the database OWNER run the REVOKE statements of migration 0116 (see " +
				"docs/runbooks/operational-runbooks.md section 7 step 5) and recycle the runtime sessions",
		)
	}

	// ADR 0108 I2: the runtime role must be a member of nothing. Memberships are
	// how a role could reach TEMP (or any other privilege) through SET ROLE
	// without holding it directly. NOTE: like the checks above this runs only for
	// APP_ENV=production (an unset APP_ENV counts as production); staging is NOT
	// gated. Extending the gate to staging is optional and was left out so the
	// staging posture stays identical to the existing ownership gate.
	member, err := checker.ConnectingRoleHasMemberships(ctx)
	if err != nil {
		return fmt.Errorf("db: production role-membership safety check failed (fail-closed - refusing to start): %w", err)
	}
	if member {
		return fmt.Errorf(
			"db: refusing to start in production - the connecting database role is a member of another role " +
				"(pg_auth_members); the runtime role must have no memberships (ADR 0108, I2)",
		)
	}
	return nil
}
