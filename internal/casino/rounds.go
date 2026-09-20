// Stage 8, docs/decisions/0080-provider-integration-readiness-without-
// external-contracts.md, Decision 1 - the casino_provider_rounds binding
// (migration 0080). This is additive read/write access to a NEW table
// only: it never touches casino_launch_sessions, ledger_transactions, or
// any other existing table, and postBet/postWin/postRollback's own
// account-resolution logic is unchanged (BindProviderRound is consulted
// ALONGSIDE that logic, never in place of it).
package casino

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ProviderRound mirrors one casino_provider_rounds row (migration 0080) -
// the durable, provider-neutral binding of a provider-declared round id to
// the platform's own session/player/brand/game/correlation identity.
type ProviderRound struct {
	ID              uuid.UUID
	TenantID        uuid.UUID
	BrandID         uuid.UUID
	PlayerAccountID uuid.UUID
	LaunchSessionID uuid.UUID
	GameID          uuid.UUID
	ProviderID      string
	ProviderRoundID string
	// ProviderSessionID is nullable - a pointer, matching this codebase's
	// nullable-optional-field convention (internal/sportsbook's own doc
	// comment cites this exact convention as "internal/casino's *string").
	ProviderSessionID *string
	CorrelationID     uuid.UUID
}

// BindProviderRound idempotently records the first observation of
// (tenantID, providerID, providerRoundID), binding it to the resolved
// session's own brandID/playerAccountID/gameID identity (ADR 0080
// Decision 1). correlationID is never a caller-supplied parameter - it is
// always deterministically derived here from (tenantID, providerID,
// providerRoundID) via roundCorrelationID, the same function every other
// caller in this package uses, so it can never silently diverge from what
// the round's own ledger transactions use. Safe to call on every bet
// delivery for a round, not just the first - a redelivery/re-bet naming
// the SAME round simply advances last_seen_at.
//
// A provider round belongs to exactly one (player_account_id, brand_id) -
// NOT to exactly one launch_session_id. Ownership of an EXISTING binding's
// player_account_id and brand_id is never silently changed: a call naming
// the same (tenantID, providerID, providerRoundID) but a DIFFERENT
// player/brand is the cross-player/cross-brand round-id collision Stage 8
// requires be rejected, not overwritten - it returns
// ErrProviderRoundOwnershipConflict, and the caller (postBet) must reject
// the whole callback rather than proceed with a partial/misattributed
// posting. launch_session_id, by contrast, IS allowed to change on an
// otherwise-matching (same player, same brand) row: a round can
// legitimately span more than one launch session for the SAME player (a
// free-spins round continuing across a session timeout, for example), and
// session identity is not part of the property this table exists to
// enforce - only cross-player/cross-brand misattribution is. A legitimate
// same-player/same-brand continuation under a new session simply updates
// launch_session_id (and last_seen_at) to the newest session.
//
// game_id is not part of the ownership-conflict guard on its own (game_id
// is implied by launch_session_id, which is itself immutable once a
// session is created - casino_launch_sessions_enforce_immutable_fields).
func BindProviderRound(ctx context.Context, tx pgx.Tx, tenantID, brandID, playerAccountID, launchSessionID, gameID uuid.UUID, providerID, providerRoundID string, providerSessionID *string) error {
	if tenantID == uuid.Nil || brandID == uuid.Nil || playerAccountID == uuid.Nil || launchSessionID == uuid.Nil || gameID == uuid.Nil {
		return fmt.Errorf("%w: binding a provider round requires fully-populated, server-resolved identity fields", ErrInvalidInput)
	}
	if providerID == "" || providerRoundID == "" {
		return fmt.Errorf("%w: provider_id and provider_round_id are required to bind a provider round", ErrInvalidInput)
	}
	correlationID := roundCorrelationID(tenantID, providerID, providerRoundID)

	var providerSessionIDValue string
	if providerSessionID != nil {
		providerSessionIDValue = *providerSessionID
	}

	// A single INSERT ... ON CONFLICT ... DO UPDATE ... WHERE ... RETURNING
	// statement, rather than a separate SELECT-then-decide: the WHERE
	// clause on the DO UPDATE arm means a conflicting row whose own
	// player_account_id/brand_id does NOT match this call's values is
	// neither updated nor returned - Postgres locks the conflicting row but
	// performs no write, so RETURNING yields zero rows (surfaced to Go as
	// pgx.ErrNoRows via QueryRow) - never a race between a separate check
	// and a separate write. launch_session_id and last_seen_at are the only
	// columns ever mutated on an existing row (a legitimate same-player/
	// same-brand continuation under a new session); every OTHER identity
	// column is set once, at first insert, and is otherwise immutable via
	// this function (and via the DB-level trigger - migration 0080).
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO casino_provider_rounds
			(tenant_id, brand_id, player_account_id, launch_session_id, game_id,
			 provider_id, provider_round_id, provider_session_id, correlation_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), $9)
		ON CONFLICT (tenant_id, provider_id, provider_round_id) DO UPDATE
			SET last_seen_at = now(), launch_session_id = EXCLUDED.launch_session_id
			WHERE casino_provider_rounds.player_account_id = EXCLUDED.player_account_id
			  AND casino_provider_rounds.brand_id = EXCLUDED.brand_id
		RETURNING id`,
		tenantID, brandID, playerAccountID, launchSessionID, gameID,
		providerID, providerRoundID, providerSessionIDValue, correlationID,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: provider_id=%s provider_round_id=%s", ErrProviderRoundOwnershipConflict, providerID, providerRoundID)
	}
	if err != nil {
		return fmt.Errorf("casino: bind provider round: %w", err)
	}
	return nil
}

// LookupProviderRound reads the casino_provider_rounds row for
// (tenantID, providerID, providerRoundID), read-only - the durable,
// provider-identifier-keyed counterpart to resolving a round by its
// platform-own session id. Returns ErrProviderRoundNotFound when no bet
// has ever bound this round (e.g. a win/rollback delivered for a round
// this binding was never asked to observe, or a tenant/provider that has
// never posted a bet at all).
func LookupProviderRound(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID, providerRoundID string) (ProviderRound, error) {
	var r ProviderRound
	err := tx.QueryRow(ctx, `
		SELECT id, tenant_id, brand_id, player_account_id, launch_session_id, game_id,
		       provider_id, provider_round_id, provider_session_id, correlation_id
		  FROM casino_provider_rounds
		 WHERE tenant_id = $1 AND provider_id = $2 AND provider_round_id = $3`,
		tenantID, providerID, providerRoundID,
	).Scan(&r.ID, &r.TenantID, &r.BrandID, &r.PlayerAccountID, &r.LaunchSessionID, &r.GameID,
		&r.ProviderID, &r.ProviderRoundID, &r.ProviderSessionID, &r.CorrelationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProviderRound{}, ErrProviderRoundNotFound
	}
	if err != nil {
		return ProviderRound{}, fmt.Errorf("casino: lookup provider round: %w", err)
	}
	return r, nil
}

// LookupProviderRoundIDsBySession batches the Back Office's admin round
// list's provider_round_id lookup (ADR 0080 Decision 5) into ONE query per
// page, mirroring newListAdminCasinoRoundsHandler's/newListAdminBetsHandler's
// own established per-page asset-decimal-exponent caching pattern rather
// than querying casino_provider_rounds once per row. Returns a map keyed by
// launch_session_id -> provider_round_id; a session with no binding simply
// has no entry (callers treat a missing key as "" per that field's own
// documented empty-when-unbound contract).
//
// A launch_session_id is NOT part of casino_provider_rounds' own UNIQUE
// constraint (that is scoped to tenant_id+provider_id+provider_round_id
// only - see migration 0080's own comment), so a session could in principle
// have more than one bound provider round id (e.g. a future provider whose
// "round" concept is narrower than the platform's own session). This
// function picks the most-recently-seen binding per session
// (DISTINCT ON ... ORDER BY last_seen_at DESC) as the single value a
// summary-row display can show - a deliberate, documented display choice,
// not a claim that a session can only ever have one true binding.
func LookupProviderRoundIDsBySession(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, sessionIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	out := make(map[uuid.UUID]string, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT ON (launch_session_id) launch_session_id, provider_round_id
		  FROM casino_provider_rounds
		 WHERE tenant_id = $1 AND launch_session_id = ANY($2)
		 ORDER BY launch_session_id, last_seen_at DESC`,
		tenantID, sessionIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("casino: batch lookup provider round ids by session: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sessionID uuid.UUID
		var providerRoundID string
		if err := rows.Scan(&sessionID, &providerRoundID); err != nil {
			return nil, fmt.Errorf("casino: scan provider round id by session: %w", err)
		}
		out[sessionID] = providerRoundID
	}
	return out, rows.Err()
}
