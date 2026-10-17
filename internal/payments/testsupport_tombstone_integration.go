//go:build integration

package payments

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PlantVerifiedDepositReversalTombstone is INTEGRATION TEST SUPPORT ONLY (this file carries the integration build tag, so no
// production binary contains it; pinned by actorproof.TestStatic_DepositTombstoneSignedOnlyHere). Since migration 0175 a
// deposit reversal tombstone is a signed system attestation bound to an applied deposit_reversal receipt, so a fixture can
// no longer post one raw. This stores a synthetic, applied and resolved deposit_reversal receipt for (providerID,
// originalRef) and posts the tombstone through the production writer (postDepositReversalTombstone, which signs): the same
// two rows the tombstone branch of applyReversalReceiptEvidence writes, without its attempt / intent side effects or audit.
// The caller's transaction must be a tenant transaction of tenantID, and the test binary must install the key hook (import
// internal/actorproof/prooftest, directly or through wdpath).
func PlantVerifiedDepositReversalTombstone(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID, originalRef string) (uuid.UUID, error) {
	ev := ReceiptEvidence{
		EventType: string(CallbackEventDepositReversal), ProviderReference: "fixture-rev-" + uuid.NewString(),
		OriginalProviderReference: originalRef, Outcome: OutcomeSucceeded, Amount: 1, AssetCode: "EUR",
	}
	receiptID, duplicate, err := insertReceiptDeduped(ctx, tx, tenantID, providerID, ev, DispositionApplied)
	if err != nil {
		return uuid.Nil, err
	}
	if duplicate {
		return uuid.Nil, fmt.Errorf("payments: fixture reversal receipt collided")
	}
	txID, err := postDepositReversalTombstone(ctx, tx, tenantID, providerID, originalRef, receiptID)
	if err != nil {
		return uuid.Nil, err
	}
	if err := ResolveReceipt(ctx, tx, receiptID, nil, string(ResolutionApplied)); err != nil {
		return uuid.Nil, err
	}
	return txID, nil
}
