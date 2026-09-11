// Package db provides the platform's PostgreSQL connection pool and the
// tenant-scoping helper that enforces row-level security. No other
// package opens its own database connection - everything goes through
// here so the tenant-context discipline in tenant_rls.go is applied
// uniformly.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool wraps a pgxpool.Pool. Kept as a thin wrapper (rather than exposing
// *pgxpool.Pool everywhere) so tenant-scoping helpers have one place to
// live and callers can't accidentally bypass them by holding a raw pool
// reference.
type Pool struct {
	pool *pgxpool.Pool
}

// Connect opens a connection pool against databaseURL. maxConns and
// connectTimeout come from config, not hardcoded, so dev/staging/prod can
// differ without a code change.
func Connect(ctx context.Context, databaseURL string, maxConns int32, connectTimeout time.Duration) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("db: parse database url: %w", err)
	}
	cfg.MaxConns = maxConns
	cfg.ConnConfig.ConnectTimeout = connectTimeout

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}

	if err := verifyNotPrivileged(pingCtx, pool); err != nil {
		pool.Close()
		return nil, err
	}

	return &Pool{pool: pool}, nil
}

// verifyNotPrivileged refuses to operate as a superuser or a role with
// the BYPASSRLS attribute. Both bypass row-level security entirely, even
// on a table with FORCE ROW LEVEL SECURITY - so if the connected role is
// either, every tenant-isolation guarantee this package provides (see
// tenant_rls.go) is silently inert, no matter how correct the RLS
// policies themselves are. This check makes that invariant enforced by
// the code rather than by remembering to configure the environment
// correctly - the exact "discipline fails exactly once" failure mode
// CLAUDE.md warns about, applied to infrastructure instead of queries.
// It was added after a Stage 1 specialist review found the CI and dev
// Postgres configs both connected as the initdb bootstrap superuser.
func verifyNotPrivileged(ctx context.Context, pool *pgxpool.Pool) error {
	var isSuperuser, bypassesRLS bool
	err := pool.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`,
	).Scan(&isSuperuser, &bypassesRLS)
	if err != nil {
		return fmt.Errorf("db: check role privileges: %w", err)
	}
	if isSuperuser || bypassesRLS {
		return fmt.Errorf(
			"db: refusing to connect as a role with rolsuper=%t rolbypassrls=%t - "+
				"such a role bypasses row-level security entirely, silently disabling tenant "+
				"isolation; connect as an ordinary application role instead (see "+
				"deploy/init-app-role.sql)",
			isSuperuser, bypassesRLS,
		)
	}
	return nil
}

func (p *Pool) Close() {
	p.pool.Close()
}

// Raw returns the underlying pgxpool.Pool for operations that
// legitimately need it (migrations, health checks). Tenant-scoped
// application queries should use WithTenant instead.
func (p *Pool) Raw() *pgxpool.Pool {
	return p.pool
}

// HealthCheck verifies the pool can reach the database, for readiness
// probes.
func (p *Pool) HealthCheck(ctx context.Context) error {
	return p.pool.Ping(ctx)
}
