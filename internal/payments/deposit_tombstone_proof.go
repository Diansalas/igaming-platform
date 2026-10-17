package payments

// Closure security review F-1 (migration 0175; ADR 0110, reused exactly as F4 / migration 0171 did): the tombstone a
// verified deposit_reversal callback writes for a never-posted original is a SIGNED attestation of the application
// server. A runtime credential that can INSERT into ledger_transactions can no longer forge one: the database refuses a
// deposit tombstone (AP001..AP005, DT001) unless a proof signed with the actor-proof key (which the runtime role cannot
// read) is attached to the transaction, bound to the tenant, the provider, the original reference, the ledger key, the
// correlation and the stored reversal receipt, and unused (nonce consumed in the same transaction). payment_ref_cleared
// (MA020) counts only such proof-bound tombstones.
//
// THIS FILE IS THE ONLY SIGNER OF OpDepositReversalTombstone (pinned by actorproof.TestStatic_DepositTombstoneSignedOnlyHere).
// It signs only from postDepositReversalTombstone, whose single production caller is applyReversalReceiptEvidence (the
// tombstone branch of a verified deposit_reversal receipt), right after that receipt was stored.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
)

// ErrDepositTombstoneProofUnavailable: the deposit reversal tombstone could not be signed (no issuer configured, or the
// claim set is not signable). The caller's transaction rolls back (receipt included); the webhook answers 503 +
// Retry-After so the PSP redelivers. Never a silent skip.
var ErrDepositTombstoneProofUnavailable = errors.New("payments: the deposit reversal tombstone could not be signed (retry later)")

// depositTombstoneDigest is the payload hash of a deposit_reversal_tombstone:write proof. It MUST equal the SQL
// k2_sha256_hex(k2_canonical(tenant, provider_id, provider_tx_id, idempotency_key, correlation_id, receipt)) of migration
// 0175's ledger_transactions_deposit_tombstone_proof_guard, computed from the row INSERTED.
func depositTombstoneDigest(tenantID uuid.UUID, providerID, originalRef, idempotencyKey string, correlationID, receiptID uuid.UUID) string {
	return actorproof.Digest(
		actorproof.S(tenantID.String()), actorproof.S(providerID), actorproof.S(originalRef), actorproof.S(idempotencyKey),
		actorproof.S(correlationID.String()), actorproof.S(receiptID.String()))
}

// attachDepositTombstoneProof signs and attaches the proof a deposit reversal tombstone needs (before its INSERT).
func attachDepositTombstoneProof(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID, originalRef, idempotencyKey string, correlationID, receiptID uuid.UUID) error {
	if prepare := actorproof.TestEnsureKeyHook; prepare != nil { // nil in production (integration test support only)
		if err := prepare(ctx, tx); err != nil {
			return fmt.Errorf("%w: %w", ErrDepositTombstoneProofUnavailable, err)
		}
	}
	err := actorproof.Default().Attach(ctx, tx, actorproof.Claims{
		Actor: actorproof.SystemActorDepositReversal(), Scope: actorproof.ScopeSystem, Tenant: tenantID,
		Operation: actorproof.OpDepositReversalTombstone, Target: receiptID.String(),
		PayloadHash: depositTombstoneDigest(tenantID, providerID, originalRef, idempotencyKey, correlationID, receiptID),
	})
	if err != nil {
		if errors.Is(err, actorproof.ErrNoIssuer) || errors.Is(err, actorproof.ErrInvalidClaims) {
			return fmt.Errorf("%w: %w", ErrDepositTombstoneProofUnavailable, err)
		}
		return err
	}
	return nil
}
