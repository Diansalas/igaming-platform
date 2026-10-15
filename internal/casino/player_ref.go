package casino

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CAS-PLAYER-REF-1 (ADR 0103 section 3.5; A3): the provider-neutral, opaque
// player reference.
//
// The platform never hands a vendor the player's account id. It hands the
// vendor a per-(tenant, provider) random reference instead
// (casino_provider_player_refs, migration 0111: tenant-scoped, FORCE RLS,
// append-only, UNIQUE (tenant_id, provider_id, player_account_id) and
// UNIQUE (tenant_id, provider_id, player_ref), player_ref a
// gen_random_uuid() default - random, derived from no secret and from no
// identifier, so it is not guessable and not linkable across tenants or
// providers). The vendor echoes it back on every callback; the platform
// resolves it, scoped to the callback's own (tenant, provider), and a value
// that does not resolve - including a raw player_account_id - is refused.
//
// The reference is authentication-neutral: it identifies, it does not
// authorise. Which wallet a bet debits is still derived from the platform's
// own launch session, and which wallet a win credits from the ledger's own
// round (postBet/postWin), exactly as before; the resolved player is
// cross-checked against that, never used to select the wallet.

// ErrPlayerRefUnresolved is returned when a callback's player reference does
// not resolve to a player of the callback's own (tenant, provider) - unknown,
// another tenant's or provider's, or a raw account id presented in its place.
// It wraps ErrInvalidInput so every HTTP surface already maps it to a
// deterministic 400; nothing is posted.
var ErrPlayerRefUnresolved = fmt.Errorf("%w: player_ref does not resolve for this tenant and provider", ErrInvalidInput)

// playerRefMismatchError is the sentinel type of ErrPlayerRefMismatch. Its text
// is IDENTICAL to ErrPlayerRefUnresolved's so the HTTP response is no oracle
// distinguishing "valid reference of another player" from "unknown reference";
// the distinction exists only internally (rejection class, audit row).
type playerRefMismatchError struct{}

func (playerRefMismatchError) Error() string { return ErrPlayerRefUnresolved.Error() }
func (playerRefMismatchError) Unwrap() error { return ErrPlayerRefUnresolved }

// ErrPlayerRefMismatch (A3 SR3-1, owner decision: fail closed): the callback's
// player reference resolves, for this tenant and provider, to a player OTHER
// than the one the platform's own state binds the money movement to (the launch
// session's owner for a bet, the round's bettor for a win, the original's owner
// for a rollback). It is an ErrPlayerRefUnresolved (same 400, same text) and is
// additionally a recorded rejection (casino_callback.player_ref_mismatch).
var ErrPlayerRefMismatch error = playerRefMismatchError{}

// postedTxPlayers returns the distinct player accounts that own the wallets on
// a posted ledger transaction's player-side legs. A well-formed casino bet/win
// has exactly one; the caller compares every returned player.
func postedTxPlayers(ctx context.Context, tx pgx.Tx, tenantID, ledgerTxID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx,
		`SELECT DISTINCT w.player_account_id
		   FROM ledger_entries le
		   JOIN ledger_accounts la ON la.id = le.ledger_account_id
		   JOIN wallets w ON w.id = la.wallet_id
		  WHERE le.ledger_transaction_id = $1 AND le.tenant_id = $2`, ledgerTxID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("casino: load posted transaction's player: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var p uuid.UUID
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("casino: scan posted transaction's player: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// postedTxOwnedBy reports whether every player-side leg of the posted
// transaction belongs to player (and at least one exists).
func postedTxOwnedBy(ctx context.Context, tx pgx.Tx, tenantID, ledgerTxID, player uuid.UUID) (bool, error) {
	players, err := postedTxPlayers(ctx, tx, tenantID, ledgerTxID)
	if err != nil {
		return false, err
	}
	if len(players) == 0 {
		return false, nil
	}
	for _, p := range players {
		if p != player {
			return false, nil
		}
	}
	return true, nil
}

// GetOrCreateProviderPlayerRef returns the opaque player reference for
// (tenantID, providerID, playerAccountID), creating it on first use. Safe
// under concurrency: the INSERT is ON CONFLICT DO NOTHING on the natural
// key, so racing creators converge on one row (the loser waits for the
// winner's commit, inserts nothing, and the SELECT that follows - a fresh
// statement snapshot at READ COMMITTED - reads the winner's row). Never an
// UPDATE (the table is append-only; an ON CONFLICT DO UPDATE arm would fire
// its deny-mutation trigger). tx must be tenant-scoped (RLS is the primary
// isolation; tenant_id is also an explicit predicate on every statement).
func GetOrCreateProviderPlayerRef(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, playerAccountID uuid.UUID) (uuid.UUID, error) {
	if tenantID == uuid.Nil || providerID == "" || playerAccountID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("%w: tenant, provider and player account are required for a player reference", ErrInvalidInput)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO casino_provider_player_refs (tenant_id, provider_id, player_account_id)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (tenant_id, provider_id, player_account_id) DO NOTHING`,
		tenantID, providerID, playerAccountID,
	); err != nil {
		return uuid.Nil, fmt.Errorf("casino: upsert provider player ref: %w", err)
	}
	var ref uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT player_ref FROM casino_provider_player_refs WHERE tenant_id = $1 AND provider_id = $2 AND player_account_id = $3`,
		tenantID, providerID, playerAccountID,
	).Scan(&ref); err != nil {
		return uuid.Nil, fmt.Errorf("casino: read provider player ref: %w", err)
	}
	return ref, nil
}

// LookupProviderPlayerRef is the read-only form of GetOrCreateProviderPlayerRef:
// it never writes (usable inside a READ ONLY transaction). found is false when
// no reference was ever issued for the player at this provider.
func LookupProviderPlayerRef(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, playerAccountID uuid.UUID) (ref uuid.UUID, found bool, err error) {
	err = tx.QueryRow(ctx,
		`SELECT player_ref FROM casino_provider_player_refs WHERE tenant_id = $1 AND provider_id = $2 AND player_account_id = $3`,
		tenantID, providerID, playerAccountID,
	).Scan(&ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("casino: lookup provider player ref: %w", err)
	}
	return ref, true, nil
}

// ResolveProviderPlayerRef maps an opaque reference echoed on an inbound
// callback back to the platform's player account id, scoped to tenantID and
// providerID taken from the callback's own authenticated route - never from
// the payload. A reference of another tenant or another provider, an unknown
// reference, and a raw account id all return ErrPlayerRefUnresolved
// (indistinguishably); nothing is created.
func ResolveProviderPlayerRef(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, ref uuid.UUID) (uuid.UUID, error) {
	if ref == uuid.Nil {
		return uuid.Nil, ErrPlayerRefUnresolved
	}
	var playerAccountID uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT player_account_id FROM casino_provider_player_refs WHERE tenant_id = $1 AND provider_id = $2 AND player_ref = $3`,
		tenantID, providerID, ref,
	).Scan(&playerAccountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrPlayerRefUnresolved
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("casino: resolve provider player ref: %w", err)
	}
	return playerAccountID, nil
}
