package db

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/txscope"
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
	return p.withTenantTx(ctx, tenantID, pgx.TxOptions{}, fn)
}

// withTenantTx is the single place that opens a tenant-scoped transaction
// and sets "app.tenant_id" on it. WithTenant and WithTenantSnapshot
// (tenant_snapshot.go) are both thin wrappers around this with different
// pgx.TxOptions, so any future change to how tenant session state is
// established (another GUC, a statement_timeout, a role switch) is made
// exactly once and applies identically to both. Callers must validate
// tenantID themselves so the error message names the public function the
// caller actually invoked.
func (p *Pool) withTenantTx(ctx context.Context, tenantID uuid.UUID, txOpts pgx.TxOptions, fn TxFunc) error {
	tx, err := p.pool.BeginTx(ctx, txOpts)
	if err != nil {
		return fmt.Errorf("db: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op if already committed

	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); err != nil {
		return fmt.Errorf("db: set tenant context: %w", err)
	}

	if err := fn(txscope.Mark(ctx), tx); err != nil {
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

	if err := fn(txscope.Mark(ctx), tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// WithPlatformAdmin runs fn in a genuinely PLATFORM-scoped transaction
// (app.tenant_id deliberately never set) with the Postgres session
// variable "app.platform_admin_principal_id" set, for the lifetime of the
// transaction, to principalID. This is the ONLY sanctioned way to perform
// an ADR 0037 layer-1-3 asset-registry mutation (create/activate/suspend/
// platform-authorize an asset, or write a platform-wide operation-
// eligibility default).
//
// It exists because `assets` is platform-wide by design - no tenant_id,
// nothing for a tenant-match RLS policy to key on - which Stage
// 4H-B0-R5's security review (finding S-3) identified as leaving layers
// 1-3 protected by an application permission check with no database
// backstop at all. Migration 0044's policies on `assets`,
// `asset_change_requests` and `asset_change_approvals` (and migration
// 0045's on `platform_products` and platform-wide
// `asset_operation_eligibility` rows) require exactly this GUC to be set
// AND app.tenant_id/app.player_account_id to be UNSET, so a tenant-scoped
// or player-scoped connection is structurally unable to satisfy them.
//
// principalID must be the platform-scoped staff principal resolved from
// the verified token's own subject - never anything a client supplied.
// Migration 0044 independently requires that principal to resolve to a
// platform-scoped (tenant_id IS NULL) staff_users row before any
// four-eyes request or approval it files is accepted, so passing an
// arbitrary uuid here grants nothing.
func (p *Pool) WithPlatformAdmin(ctx context.Context, principalID uuid.UUID, fn TxFunc) error {
	if principalID == uuid.Nil {
		return fmt.Errorf("db: WithPlatformAdmin called with nil principal id")
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, principalID.String()); err != nil {
		return fmt.Errorf("db: set platform admin context: %w", err)
	}

	if err := fn(txscope.Mark(ctx), tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit tx: %w", err)
	}
	return nil
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

	if err := fn(txscope.Mark(ctx), tx); err != nil {
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

	if err := fn(txscope.Mark(ctx), tx); err != nil {
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

	if err := fn(txscope.Mark(ctx), tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit tx: %w", err)
	}
	return nil
}

// WithCredentialTokenLookup runs fn in a platform-scoped transaction (no
// app.tenant_id) with "app.credential_token_lookup_hash" set to hash, for
// the lifetime of the transaction. This is the ONLY sanctioned way to
// read a player_credential_tokens row (Stage 4F: email verification /
// password reset) before any tenant scope is known - the presented token
// carries no tenant hint, exactly like a refresh token, so this mirrors
// WithSessionLookup's identical rationale and mechanism. hash must be the
// SHA-256 hex digest of the actual, unguessable raw token presented by
// the caller. Migration 0040's token_lookup policy grants visibility to
// at most the one row whose token_hash exactly equals this value.
func (p *Pool) WithCredentialTokenLookup(ctx context.Context, hash string, fn TxFunc) error {
	if hash == "" {
		return fmt.Errorf("db: WithCredentialTokenLookup called with empty hash")
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT set_config('app.credential_token_lookup_hash', $1, true)`, hash); err != nil {
		return fmt.Errorf("db: set credential token lookup context: %w", err)
	}

	if err := fn(txscope.Mark(ctx), tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit tx: %w", err)
	}
	return nil
}

// SetPrincipalIDForCurrentTx sets the Postgres session variable
// "app.principal_id" for the lifetime of the CURRENT transaction (tx must
// already be open - this does not begin or commit one), for code that
// needs to satisfy the sessions table's own session_select_own_principal
// RLS policy (migration 0018) from WITHIN an already-open, differently-
// scoped transaction (e.g. WithTenant) rather than opening a fresh one
// via WithPrincipalScope - used by internal/auth.
// RevokeAllSessionsForPrincipal (Stage 4F's password-reset-confirm flow),
// which must revoke sessions ATOMICALLY alongside a password-hash update
// and an audit record already running inside one WithTenant transaction;
// WithPrincipalScope's own separate transaction would break that
// atomicity. Like SetSessionInternalOpID, this grants no authorization of
// its own - the caller must already be legitimately acting as this exact
// principal (e.g. it was just resolved from a validated credential token
// or an authenticated session) before calling it.
func SetPrincipalIDForCurrentTx(ctx context.Context, tx pgx.Tx, principalID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT set_config('app.principal_id', $1, true)`, principalID.String()); err != nil {
		return fmt.Errorf("db: set principal id context: %w", err)
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
