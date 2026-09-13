// Package wallet implements Wallet: PlayerAccount -> Wallet -> Asset
// (docs/architecture/financial-domain-model.md). Wallet belongs to
// exactly one PlayerAccount, never to Person - a Person with accounts at
// two brands gets independent wallet sets per brand (ADR 0012's Brand/
// Tenant distinctness, extended to Wallet by that document's own
// decision). This package never posts ledger entries itself - it owns
// wallet identity and read-side balance composition; internal/ledger
// owns posting.
package wallet

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// ErrNotFound is returned when a wallet does not exist.
var ErrNotFound = errors.New("wallet: not found")

// Status mirrors the wallets.status CHECK constraint.
type Status string

const (
	StatusActive Status = "active"
	StatusFrozen Status = "frozen"
	StatusClosed Status = "closed"
)

// Wallet is a player's holding of one asset, scoped to a specific
// PlayerAccount (financial-domain-model.md "Wallet identity").
type Wallet struct {
	ID              uuid.UUID
	TenantID        uuid.UUID
	BrandID         uuid.UUID
	PlayerAccountID uuid.UUID
	AssetCode       string
	Status          Status
}

// GetOrCreate returns the player's wallet for assetCode, creating it if
// this is the first time the player is using that asset. Race-free under
// concurrent first use via INSERT ... ON CONFLICT DO NOTHING against the
// UNIQUE(player_account_id, asset_code) constraint (migration 0019),
// never check-then-insert.
func GetOrCreate(ctx context.Context, tx pgx.Tx, tenantID, brandID, playerAccountID uuid.UUID, assetCode string) (Wallet, error) {
	newID := uuid.New()
	if _, err := tx.Exec(ctx,
		`INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (player_account_id, asset_code) DO NOTHING`,
		newID, tenantID, brandID, playerAccountID, assetCode,
	); err != nil {
		return Wallet{}, fmt.Errorf("wallet: get or create: %w", err)
	}
	return GetByPlayerAndAsset(ctx, tx, playerAccountID, assetCode)
}

// GetByPlayerAndAsset looks up a player's wallet for a specific asset.
func GetByPlayerAndAsset(ctx context.Context, tx pgx.Tx, playerAccountID uuid.UUID, assetCode string) (Wallet, error) {
	var w Wallet
	err := tx.QueryRow(ctx,
		`SELECT id, tenant_id, brand_id, player_account_id, asset_code, status
		 FROM wallets WHERE player_account_id = $1 AND asset_code = $2`,
		playerAccountID, assetCode,
	).Scan(&w.ID, &w.TenantID, &w.BrandID, &w.PlayerAccountID, &w.AssetCode, &w.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Wallet{}, ErrNotFound
	}
	if err != nil {
		return Wallet{}, fmt.Errorf("wallet: get by player and asset: %w", err)
	}
	return w, nil
}

// GetByID looks up a wallet by id within the current tenant/player scope.
func GetByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Wallet, error) {
	var w Wallet
	err := tx.QueryRow(ctx,
		`SELECT id, tenant_id, brand_id, player_account_id, asset_code, status FROM wallets WHERE id = $1`,
		id,
	).Scan(&w.ID, &w.TenantID, &w.BrandID, &w.PlayerAccountID, &w.AssetCode, &w.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Wallet{}, ErrNotFound
	}
	if err != nil {
		return Wallet{}, fmt.Errorf("wallet: get by id: %w", err)
	}
	return w, nil
}

// List returns every wallet the current player_account (from RLS scope)
// holds - used by the "list my wallets" self-service endpoint.
func List(ctx context.Context, tx pgx.Tx, playerAccountID uuid.UUID) ([]Wallet, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, tenant_id, brand_id, player_account_id, asset_code, status
		 FROM wallets WHERE player_account_id = $1 ORDER BY asset_code`,
		playerAccountID,
	)
	if err != nil {
		return nil, fmt.Errorf("wallet: list: %w", err)
	}
	defer rows.Close()

	var wallets []Wallet
	for rows.Next() {
		var w Wallet
		if err := rows.Scan(&w.ID, &w.TenantID, &w.BrandID, &w.PlayerAccountID, &w.AssetCode, &w.Status); err != nil {
			return nil, fmt.Errorf("wallet: scan: %w", err)
		}
		wallets = append(wallets, w)
	}
	return wallets, rows.Err()
}

// Summary composes the player-facing balance view for one wallet from
// its player-owned ledger accounts (financial-domain-model.md,
// reconciliation-model.md §3): current cash, funds held against an open
// withdrawal request, and bonus balance. Available balance is simply the
// current player_cash balance - Flow 3 Step A already debits player_cash
// the moment a withdrawal is requested (ledger-accounting-model.md §2),
// so subtracting the hold again would double-count it
// (reconciliation-model.md §3's own corrected formula).
type Summary struct {
	Wallet            Wallet
	CashBalance       int64
	AvailableBalance  int64
	HeldForWithdrawal int64
	LockedBalance     int64
	BonusBalance      int64
}

// GetSummary reads every player-owned ledger account for w, creating none
// that don't already exist (a wallet with no activity yet simply reports
// zero balances - GetOrCreateAccount is only ever called from the
// posting path, when there is an actual fact to post).
func GetSummary(ctx context.Context, tx pgx.Tx, w Wallet) (Summary, error) {
	s := Summary{Wallet: w}

	rows, err := tx.Query(ctx,
		`SELECT p.account_type, p.debit_total, p.credit_total
		 FROM wallet_balance_projection p
		 JOIN ledger_accounts la ON la.id = p.ledger_account_id
		 WHERE la.wallet_id = $1`,
		w.ID,
	)
	if err != nil {
		return Summary{}, fmt.Errorf("wallet: get summary: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var accountType ledger.AccountType
		var debit, credit int64
		if err := rows.Scan(&accountType, &debit, &credit); err != nil {
			return Summary{}, fmt.Errorf("wallet: scan summary row: %w", err)
		}
		signed := credit - debit
		switch accountType {
		case ledger.AccountPlayerCash:
			s.CashBalance = signed
		case ledger.AccountPlayerWithdrawalHold:
			s.HeldForWithdrawal = signed
		case ledger.AccountPlayerLocked:
			s.LockedBalance = signed
		case ledger.AccountPlayerBonus:
			s.BonusBalance = signed
		}
	}
	if err := rows.Err(); err != nil {
		return Summary{}, fmt.Errorf("wallet: read summary rows: %w", err)
	}

	s.AvailableBalance = s.CashBalance
	return s, nil
}
