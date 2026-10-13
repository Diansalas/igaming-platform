package adjustment

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
)

// walletHoldVerdict evaluates the AML hold for the player that owns walletID, for the operation manual_adjustment (ANY
// active hold refuses: ADR 0116 s5.1 "Held balances"). It runs inside the caller's transaction (a tenant or acting session).
// A wallet that does not resolve is left to the existing payload rules (wallet_not_found): no verdict is invented for it.
func walletHoldVerdict(ctx context.Context, tx pgx.Tx, tenantID, walletID uuid.UUID) (aml.HoldVerdict, error) {
	var accountID, personID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT w.player_account_id, pa.person_id
		FROM wallets w JOIN player_accounts pa ON pa.id = w.player_account_id AND pa.tenant_id = w.tenant_id
		WHERE w.id = $1 AND w.tenant_id = $2`, walletID, tenantID).Scan(&accountID, &personID)
	if errors.Is(err, pgx.ErrNoRows) {
		return aml.HoldVerdict{}, nil
	}
	if err != nil {
		return aml.HoldVerdict{EvalFailed: true}, fmt.Errorf("adjustment: resolve the wallet's player for the AML hold check: %w", err)
	}
	return aml.EvaluateHold(ctx, tx, tenantID, personID, accountID, aml.OpManualAdjustment)
}
