package db

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TxFunc is application code that runs inside a tenant-scoped
// transaction. It receives the transaction, not the pool, so it cannot
// accidentally issue a query outside the tenant-scoped connection.
type TxFunc func(ctx context.Context, tx pgx.Tx) error

// WithTenant is the ONLY sanctioned way to run a tenant-scoped query.
// It opens a transaction, sets the Postgres session variable
// "app.tenant_id" for the lifetime of that transaction via
// set_config(..., true) (the "true" argument scopes it to the current
// transaction, equivalent to SET LOCAL, and - unlike SET LOCAL itself -
// set_config is a normal function call that accepts a bound parameter,
// so the tenant id is never interpolated into SQL text), runs fn, and
// commits or rolls back.
//
// Every tenant-owned table's row-level security policy is expected to
// check current_setting('app.tenant_id', true)::uuid = tenant_id. This
// function is the single place that sets that value - no other code path
// in the platform may set app.tenant_id, and callers never pass a
// tenant id that didn't come from the verified JWT (see internal/tenant).
func (p *Pool) WithTenant(ctx context.Context, tenantID uuid.UUID, fn TxFunc) error {
	if tenantID == uuid.Nil {
		return fmt.Errorf("db: WithTenant called with nil tenant id")
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op if already committed

	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); err != nil {
		return fmt.Errorf("db: set tenant context: %w", err)
	}

	if err := fn(ctx, tx); err != nil {
		return err // deferred Rollback cleans up
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit tx: %w", err)
	}
	return nil
}

// WithoutTenant runs fn in a plain transaction with no tenant context
// set. Row-level-security policies on tenant-owned tables will therefore
// see current_setting('app.tenant_id', true) as empty and deny access -
// this is intentional. It exists only for platform-level operations that
// are genuinely not tenant-scoped (e.g. reading the jurisdiction/asset
// registries, which are platform-wide reference data, not per-tenant).
func (p *Pool) WithoutTenant(ctx context.Context, fn TxFunc) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// WithSessionLookup runs fn in a platform-scoped transaction (no
// app.tenant_id) with the Postgres session variable
// "app.session_lookup_hash" set, for the lifetime of the transaction, to
// hash. This is the ONLY sanctioned way to read a `sessions` row before
// any tenant/principal context is known - a refresh token carries no
// tenant hint, so the sessions table's SELECT policy for this path
// (migration 0018) grants visibility to at most the one row whose
// refresh_token_hash exactly equals this value, never a blanket read.
// hash must be the SHA-256 hex digest of the actual, unguessable refresh
// token presented by the caller - never anything derived from
// unauthenticated free-form input beyond the token itself. See
// docs/decisions/0016-sessions-rls-hardening.md.
func (p *Pool) WithSessionLookup(ctx context.Context, hash string, fn TxFunc) error {
	if hash == "" {
		return fmt.Errorf("db: WithSessionLookup called with empty hash")
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT set_config('app.session_lookup_hash', $1, true)`, hash); err != nil {
		return fmt.Errorf("db: set session lookup context: %w", err)
	}

	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit tx: %w", err)
	}
	return nil
}

// WithPrincipalScope runs fn in a transaction scoped to tenantID (or
// platform scope if uuid.Nil, via the same rules as WithTenant/
// WithoutTenant) AND to principalID via the Postgres session variable
// "app.principal_id". This is the ONLY sanctioned way to read a caller's
// own `sessions` rows (e.g. "list my active sessions") - migration
// 0018's SELECT policy requires both the tenant/platform scope AND the
// exact principal_id to match, so no principal - even one legitimately
// scoped to the right tenant - can read another principal's session
// metadata through this table. See
// docs/decisions/0016-sessions-rls-hardening.md.
func (p *Pool) WithPrincipalScope(ctx context.Context, tenantID, principalID uuid.UUID, fn TxFunc) error {
	if principalID == uuid.Nil {
		return fmt.Errorf("db: WithPrincipalScope called with nil principal id")
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if tenantID != uuid.Nil {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); err != nil {
			return fmt.Errorf("db: set tenant context: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.principal_id', $1, true)`, principalID.String()); err != nil {
		return fmt.Errorf("db: set principal context: %w", err)
	}

	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit tx: %w", err)
	}
	return nil
}

// WithPlayerScope runs fn in a transaction scoped to BOTH tenantID (via
// "app.tenant_id") AND playerAccountID (via "app.player_account_id").
// This is the ONLY sanctioned way to run a player-self-service financial
// read/write - the wallet/ledger RLS policies (migrations 0019-0026) are
// deliberately split into two permissive policies per table: a
// tenant_staff_scope policy that requires app.player_account_id to be
// UNSET (used by WithTenant for provider callbacks, staff/admin
// handlers, and the posting engine itself), and a player_self_scope
// policy that requires app.player_account_id to match exactly (used only
// here). This mirrors WithPrincipalScope's role for `sessions`
// (docs/decisions/0016) but uses a distinct GUC name, per
// docs/decisions/0019 ("The player scope GUC is app.player_account_id,
// set only by trusted server code from the authenticated player
// principal's own resolved account - never from a path, query, or body
// parameter").
//
// Splitting tenant_staff_scope on "is app.player_account_id unset" is
// what actually makes the isolation hold: without it, a player-scoped
// connection would ALSO satisfy a plain tenant-match policy (Postgres
// OR's every applicable permissive policy together), defeating the
// per-player isolation this function exists to provide. See the Stage 3B
// migrations' own comments for the same reasoning repeated per table.
func (p *Pool) WithPlayerScope(ctx context.Context, tenantID, playerAccountID uuid.UUID, fn TxFunc) error {
	if tenantID == uuid.Nil {
		return fmt.Errorf("db: WithPlayerScope called with nil tenant id")
	}
	if playerAccountID == uuid.Nil {
		return fmt.Errorf("db: WithPlayerScope called with nil player account id")
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); err != nil {
		return fmt.Errorf("db: set tenant context: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.player_account_id', $1, true)`, playerAccountID.String()); err != nil {
		return fmt.Errorf("db: set player account context: %w", err)
	}

	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit tx: %w", err)
	}
	return nil
}

// SetSessionInternalOpID sets the Postgres session variable
// "app.session_internal_op_id" for the lifetime of the CURRENT
// transaction (tx must already be open - this does not begin or commit
// one) to sessionID. This is the only sanctioned way internal system
// code (e.g. refresh-token rotation, reuse-chain revocation in
// internal/auth) grants itself visibility into a specific, already-
// resolved `sessions` row when neither a token hash nor a caller-
// asserted principal_id is available in the current scope - see
// migration 0018 and docs/decisions/0016-sessions-rls-hardening.md.
// Callers must have already established, through some other trusted
// mechanism (a prior token-hash lookup, in every current caller), that
// they are entitled to act on this exact row - this function grants no
// authorization of its own, it only makes the row visible to Postgres's
// row-level security policies once that authorization already holds.
func SetSessionInternalOpID(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT set_config('app.session_internal_op_id', $1, true)`, sessionID.String()); err != nil {
		return fmt.Errorf("db: set session internal-op context: %w", err)
	}
	return nil
}
