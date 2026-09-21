package sportsbook

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

const betColumns = `id, tenant_id, brand_id, player_account_id, wallet_id, selection_id, asset_code,
	stake_amount, odds_numerator, odds_denominator, potential_return, status, idempotency_key,
	ledger_transaction_id, placed_at, provider_id, provider_bet_reference, jurisdiction_code`

func scanBet(row pgx.Row) (Bet, error) {
	var b Bet
	var jurisdictionCode *string
	err := row.Scan(
		&b.ID, &b.TenantID, &b.BrandID, &b.PlayerAccountID, &b.WalletID, &b.SelectionID, &b.AssetCode,
		&b.StakeAmount, &b.OddsNumerator, &b.OddsDenominator, &b.PotentialReturn, &b.Status, &b.IdempotencyKey,
		&b.LedgerTransactionID, &b.PlacedAt, &b.ProviderID, &b.ProviderBetReference, &jurisdictionCode,
	)
	if err != nil {
		return Bet{}, err
	}
	if jurisdictionCode != nil {
		b.JurisdictionCode = *jurisdictionCode
	}
	return b, nil
}

// ErrBetIdempotencyKeyReused is returned when a retried PlaceBet call's
// (tenant_id, player_account_id, idempotency_key) matches an existing bet
// whose selection/stake/asset/odds differ from the current request - the
// sportsbook analogue of internal/withdrawal.ErrIdempotencyKeyReused and
// internal/payments.ErrIdempotencyKeyReused: a caller must never silently
// receive a DIFFERENT bet's data back just because it reused a key.
var ErrBetIdempotencyKeyReused = errors.New("sportsbook: idempotency key reused with different bet parameters")

// findBetByIdempotencyKey looks up an existing sportsbook_bets row for
// (tenantID, playerAccountID, idempotencyKey) - PlaceBet's idempotency
// short-circuit, mirroring internal/casino's findPostedBetTransaction:
// checked BEFORE RG/risk evaluation so a retried request never
// re-evaluates policy against whatever state happens to be live at
// redelivery time (the same rationale casino's own doc comment gives in
// full). Scoped by player_account_id (not just tenant_id) so that two
// different players in the same tenant can never collide on the same
// client-chosen key - matching deposit_intents/withdrawal_requests'
// identical three-column scoping precedent.
func findBetByIdempotencyKey(ctx context.Context, tx pgx.Tx, tenantID, playerAccountID uuid.UUID, idempotencyKey string) (Bet, bool, error) {
	row := tx.QueryRow(ctx,
		`SELECT `+betColumns+` FROM sportsbook_bets WHERE tenant_id = $1 AND player_account_id = $2 AND idempotency_key = $3`,
		tenantID, playerAccountID, idempotencyKey)
	b, err := scanBet(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Bet{}, false, nil
	}
	if err != nil {
		return Bet{}, false, fmt.Errorf("sportsbook: find bet by idempotency key: %w", err)
	}
	return b, true, nil
}

// insertBetParams is the row insertBet writes - always status 'open',
// per this stage's explicit scope boundary (no settlement is ever
// written this stage).
type insertBetParams struct {
	ID                  uuid.UUID
	TenantID            uuid.UUID
	BrandID             uuid.UUID
	PlayerAccountID     uuid.UUID
	WalletID            uuid.UUID
	SelectionID         uuid.UUID
	AssetCode           string
	StakeAmount         int64
	OddsNumerator       int64
	OddsDenominator     int64
	PotentialReturn     int64
	IdempotencyKey      string
	LedgerTransactionID uuid.UUID
	// JurisdictionCode - see Bet.JurisdictionCode's own doc comment.
	// Empty string is written as NULL (matches casino.CreateLaunchSession's
	// identical NULLIF(..., '') convention below).
	JurisdictionCode string
}

// insertBet writes the new bet row via db.IdempotentInsert - the real,
// DB-enforced backstop against two concurrent requests racing past
// findBetByIdempotencyKey's own pre-check with the SAME (tenant, player,
// idempotency_key) before either commits (CLAUDE.md: idempotency is a DB
// unique constraint, never "check then insert" alone). On a genuine
// unique-violation race, the loser re-reads the winner's row and, if its
// parameters differ, surfaces ErrBetIdempotencyKeyReused rather than
// either a raw constraint-violation error or silently returning the
// wrong bet.
func insertBet(ctx context.Context, tx pgx.Tx, p insertBetParams) (Bet, error) {
	conflict, err := db.IdempotentInsert(ctx, tx, func(spTx pgx.Tx) error {
		_, err := spTx.Exec(ctx,
			`INSERT INTO sportsbook_bets
				(id, tenant_id, brand_id, player_account_id, wallet_id, selection_id, asset_code,
				 stake_amount, odds_numerator, odds_denominator, potential_return, status, idempotency_key, ledger_transaction_id,
				 jurisdiction_code)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, NULLIF($15, ''))`,
			p.ID, p.TenantID, p.BrandID, p.PlayerAccountID, p.WalletID, p.SelectionID, p.AssetCode,
			p.StakeAmount, p.OddsNumerator, p.OddsDenominator, p.PotentialReturn, BetStatusOpen, p.IdempotencyKey, p.LedgerTransactionID,
			p.JurisdictionCode,
		)
		return err
	})
	if err != nil {
		return Bet{}, fmt.Errorf("sportsbook: insert bet: %w", err)
	}
	if !conflict {
		return GetBetByID(ctx, tx, p.ID)
	}

	existing, found, err := findBetByIdempotencyKey(ctx, tx, p.TenantID, p.PlayerAccountID, p.IdempotencyKey)
	if err != nil {
		return Bet{}, fmt.Errorf("sportsbook: look up existing bet after idempotency conflict: %w", err)
	}
	if !found {
		return Bet{}, fmt.Errorf("sportsbook: idempotency conflict on insert but no existing bet found for key %q", p.IdempotencyKey)
	}
	if existing.SelectionID != p.SelectionID || existing.StakeAmount != p.StakeAmount || existing.AssetCode != p.AssetCode ||
		existing.OddsNumerator != p.OddsNumerator || existing.OddsDenominator != p.OddsDenominator {
		return Bet{}, fmt.Errorf("%w: existing bet %s", ErrBetIdempotencyKeyReused, existing.ID)
	}
	return existing, nil
}

// GetBetByID looks up a bet within the current tenant/player RLS scope.
func GetBetByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Bet, error) {
	row := tx.QueryRow(ctx, `SELECT `+betColumns+` FROM sportsbook_bets WHERE id = $1`, id)
	b, err := scanBet(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Bet{}, ErrBetNotFound
	}
	if err != nil {
		return Bet{}, fmt.Errorf("sportsbook: get bet by id: %w", err)
	}
	return b, nil
}

// ListBetsForPlayer returns a page of playerAccountID's own bets, newest
// first, using the shared Stage 5 pagination convention (limit/offset,
// total count). tx should be scoped via db.Pool.WithPlayerScope.
func ListBetsForPlayer(ctx context.Context, tx pgx.Tx, playerAccountID uuid.UUID, limit, offset int) ([]Bet, int, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+betColumns+` FROM sportsbook_bets WHERE player_account_id = $1
		 ORDER BY placed_at DESC LIMIT $2 OFFSET $3`,
		playerAccountID, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("sportsbook: list bets for player: %w", err)
	}
	bets, err := scanBetsWithTotal(rows)
	if err != nil {
		return nil, 0, err
	}
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM sportsbook_bets WHERE player_account_id = $1`, playerAccountID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("sportsbook: count bets for player: %w", err)
	}
	return bets, total, nil
}

// ListBetsForTenant is the Back Office view: every bet in the caller's
// own tenant, regardless of player - tx should be scoped via
// db.Pool.WithTenant (staff/system scope, never WithPlayerScope).
func ListBetsForTenant(ctx context.Context, tx pgx.Tx, limit, offset int) ([]Bet, int, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+betColumns+` FROM sportsbook_bets ORDER BY placed_at DESC LIMIT $1 OFFSET $2`,
		limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("sportsbook: list bets for tenant: %w", err)
	}
	bets, err := scanBetsWithTotal(rows)
	if err != nil {
		return nil, 0, err
	}
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM sportsbook_bets`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("sportsbook: count bets for tenant: %w", err)
	}
	return bets, total, nil
}

func scanBetsWithTotal(rows pgx.Rows) ([]Bet, error) {
	defer rows.Close()
	var out []Bet
	for rows.Next() {
		b, err := scanBet(rows)
		if err != nil {
			return nil, fmt.Errorf("sportsbook: scan bet: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
