package tenant

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrNotActiveForGameplay is returned by RequireActiveForGameplay when the
// tenant is suspended, closed, any other non-'active' status, or its row is
// not visible. Callers translate it into their own deterministic, non-
// retryable refusal; nothing may be posted after it.
var ErrNotActiveForGameplay = errors.New("tenant: tenant is not active; new gameplay postings are refused")

// RequireActiveForGameplay is the ONE shared check behind the owner decision
// R3-GAME-POSTINGS-NONACTIVE-1 (2026-10-05, ADR 0095 section 40.5): a NEW
// casino or sportsbook financial posting for a suspended or closed tenant is
// refused. It must be called inside the SAME transaction that posts, before
// the first ledger write, and only on a path that would create a new
// movement (a replay of an already-posted fact must not call it: replays
// are read-only and return the original outcome).
//
// Race freedom (migration 0118): it takes the per-tenant status advisory lock
// SHARED, then reads tenants.status in a fresh statement. A tenant status
// change takes the same lock EXCLUSIVE (trigger tenants_status_change_gate)
// and holds it to the end of its transaction, so a status change either
// committed before this read (and is seen) or waits until this transaction
// ends. A plain `FOR SHARE` on the tenants row is not used: under the
// runtime role RLS filters the row out of a locking read.
//
// A missing or unreadable row fails closed. The lock key comes from the
// database function tenant_status_gate_key so the Go and trigger sides can
// never drift.
func RequireActiveForGameplay(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	status, err := GameplayStatus(ctx, tx, tenantID)
	if err != nil {
		return err
	}
	if status != "active" {
		return fmt.Errorf("%w: status=%s", ErrNotActiveForGameplay, status)
	}
	return nil
}

// GameplayStatus takes the per-tenant status advisory lock SHARED and then
// reads tenants.status in a fresh statement, returning it WITHOUT refusing a
// non-active tenant. It is the primitive under RequireActiveForGameplay and
// the entry point for the one class of posting that stays allowed on a
// suspended or closed tenant: a TERMINAL STAKE RETURN (owner decision
// Q-GP-5, 2026-10-06, ADR 0095 section 40.5): a casino rollback of a posted
// bet, a sportsbook void of an open bet, a sportsbook void after settlement.
// Such a caller must still hold the shared lock (so a status change cannot
// race it), must verify the return corresponds to an existing accepted stake
// of the same tenant, and must refuse every other kind of posting itself.
//
// A missing or unreadable tenant row returns ErrNotActiveForGameplay (fail
// closed): a missing status is never a licence to post.
func GameplayStatus(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (string, error) {
	if tenantID == uuid.Nil {
		return "", fmt.Errorf("%w: no tenant id", ErrNotActiveForGameplay)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(public.tenant_status_gate_key($1))`, tenantID); err != nil {
		return "", fmt.Errorf("tenant: take status gate lock: %w", err)
	}
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM public.tenants WHERE id = $1`, tenantID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: tenant row not visible", ErrNotActiveForGameplay)
	}
	if err != nil {
		return "", fmt.Errorf("tenant: read status: %w", err)
	}
	return status, nil
}
