package casino

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// ErrStakeReturnMismatch marks a rollback of a posted casino bet that is
// refused on a NON-ACTIVE tenant because the callback does not match the
// bet it names (another player, another asset, another amount, another
// round). It is always returned wrapped in ErrTenantNotActive, so the HTTP
// layer answers it exactly like any other refusal on a non-active tenant.
var ErrStakeReturnMismatch = errors.New("casino: rollback does not match the posted bet it names")

// ErrStakeReturnWinOutstanding marks a rollback of a posted casino bet that is
// refused on a NON-ACTIVE tenant because the bet's round still has an
// unreversed casino_win: the rollback of that win stays refused there, so
// returning the stake as well would leave stake AND win both paid
// (ledger-finance C1). Always wrapped in ErrTenantNotActive; the callback is
// kept as durable evidence for staff resolution.
var ErrStakeReturnWinOutstanding = errors.New("casino: the bet's round has an unreversed win")

// requireStakeReturnAllowed is the casino gate for a rollback of an
// already-posted casino_bet, the TERMINAL STAKE RETURN that stays allowed on
// a suspended or closed tenant (owner decision Q-GP-5, 2026-10-06, ADR 0095
// section 40.5). A terminal stake return is not a new wager: it returns the
// stake of a round the tenant already accepted, bounded by the original
// bet's own entries (postRollback inverts exactly those, once, under the
// FOR UPDATE it already holds).
//
// It runs after postRollback proved originalType == casino_bet for
// (tenant, provider, original ref) under the original's FOR UPDATE, and
// before the first ledger write. Behaviour:
//   - ACTIVE tenant: unchanged, nothing further is checked.
//   - NON-ACTIVE tenant: the shared status-gate lock is held (tenant.
//     GameplayStatus, so a status change cannot race this posting) and the
//     callback must agree with the bet it names: player, asset, amount and
//     round, each compared WHEN the callback carries it (a provider
//     rollback often carries none of them). A disagreement is refused as
//     ErrTenantNotActive wrapping ErrStakeReturnMismatch; nothing posts.
//
// Wins are NOT routed here: a rollback of a casino_win stays refused on a
// non-active tenant (requireActiveTenantForNewPosting).
func requireStakeReturnAllowed(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, originalID uuid.UUID, event CallbackEvent) error {
	status, err := tenant.GameplayStatus(ctx, tx, tenantID)
	if err != nil {
		if errors.Is(err, tenant.ErrNotActiveForGameplay) {
			return fmt.Errorf("%w: %w", ErrTenantNotActive, err)
		}
		return fmt.Errorf("casino: check tenant status: %w", err)
	}
	if status == "active" {
		return nil
	}
	if err := verifyStakeReturnMatchesBet(ctx, tx, tenantID, providerID, originalID, event); err != nil {
		return fmt.Errorf("%w: status=%s: %w", ErrTenantNotActive, status, err)
	}
	return nil
}

func verifyStakeReturnMatchesBet(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, originalID uuid.UUID, event CallbackEvent) error {
	var correlationID, playerID uuid.UUID
	var assetCode string
	var amount int64
	// The bet's stake is its debit on the player's own account (postBet:
	// debit player_cash, credit house_gaming). Everything is read under the
	// tenant's RLS scope and filtered by tenant_id again.
	err := tx.QueryRow(ctx, `
		SELECT t.correlation_id, la.player_account_id, la.asset_code, e.amount
		  FROM ledger_transactions t
		  JOIN ledger_entries  e  ON e.ledger_transaction_id = t.id AND e.tenant_id = t.tenant_id
		  JOIN ledger_accounts la ON la.id = e.ledger_account_id AND la.tenant_id = t.tenant_id
		 WHERE t.id = $1 AND t.tenant_id = $2 AND t.transaction_type = $3
		   AND e.direction = 'debit' AND la.player_account_id IS NOT NULL`,
		originalID, tenantID, ledger.TxCasinoBet,
	).Scan(&correlationID, &playerID, &assetCode, &amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: no player stake debit found on the bet", ErrStakeReturnMismatch)
	}
	if err != nil {
		return fmt.Errorf("casino: load bet for stake-return check: %w", err)
	}
	// Overpay guard: use the ORIGINAL bet's own correlation_id (never the
	// callback's RoundID) to find unreversed wins of that round.
	var winOutstanding bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM ledger_transactions w
		   WHERE w.tenant_id = $1 AND w.correlation_id = $2 AND w.transaction_type = $3
		     AND NOT EXISTS (SELECT 1 FROM ledger_transactions r WHERE r.reverses_transaction_id = w.id))`,
		tenantID, correlationID, ledger.TxCasinoWin,
	).Scan(&winOutstanding); err != nil {
		return fmt.Errorf("casino: check round wins for stake-return: %w", err)
	}
	if winOutstanding {
		return ErrStakeReturnWinOutstanding
	}
	if event.PlayerAccountID != uuid.Nil && event.PlayerAccountID != playerID {
		return fmt.Errorf("%w: player", ErrStakeReturnMismatch)
	}
	if event.AssetCode != "" && event.AssetCode != assetCode {
		return fmt.Errorf("%w: asset", ErrStakeReturnMismatch)
	}
	if event.Amount != 0 && event.Amount != amount {
		return fmt.Errorf("%w: amount", ErrStakeReturnMismatch)
	}
	if event.RoundID != "" && roundCorrelationID(tenantID, providerID, event.RoundID) != correlationID {
		return fmt.Errorf("%w: round", ErrStakeReturnMismatch)
	}
	return nil
}
