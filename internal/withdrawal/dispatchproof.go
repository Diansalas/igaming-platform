package withdrawal

// F4 residual R1 (migration 0171; ADR 0110, reused): the ALLOW decision of the WD-RG-1 dispatch gate is a SIGNED
// attestation of the application server. A runtime credential that can INSERT into withdrawal_gate_decisions can no
// longer forge an allow: the database refuses an allowed row (AP001..AP005) unless a proof signed with the actor-proof
// key (which the runtime role cannot read) is attached to the transaction, bound to the tenant, the request, the attempt
// (re-dispatch), the stage, the evaluated outcome and the request's own figures, and unused (nonce consumed in the same
// transaction). The WD053 guards then count only proof-bound allows.
//
// THIS FILE IS THE ONLY SIGNER IN internal/withdrawal (pinned by actorproof.TestStatic_WithdrawalSignsOnlyInDispatchProof).
// It signs only from RecordDispatchGateDecision, which is itself pinned to the four production call sites that obtained
// the decision from EvaluateDispatchGate.

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
)

// ErrDispatchProofUnavailable: the allow decision could not be signed (no issuer configured, or the claim set is not
// signable). It wraps ErrGateUnavailable, so every caller treats it as a fail-closed, retryable refusal: nothing is
// recorded, nothing is claimed, nothing is sent.
var ErrDispatchProofUnavailable = fmt.Errorf("%w: the dispatch gate decision could not be signed", ErrGateUnavailable)

// dispatchAllowDigest is the payload hash of a withdrawal_dispatch_gate:allow proof. It MUST equal the SQL
// k2_sha256_hex(k2_canonical(tenant, request, attempt, stage, risk_outcome, restriction_in_force, account, wallet,
// asset, amount)) of migration 0171's withdrawal_gate_decisions_actor_proof_guard, computed from the row INSERTED.
func dispatchAllowDigest(wr WithdrawalRequest, stage DispatchStage, attemptID *uuid.UUID, riskOutcome, restriction *string) string {
	return actorproof.Digest(
		actorproof.S(wr.TenantID.String()), actorproof.S(wr.ID.String()), actorproof.UUIDP(attemptID),
		actorproof.S(string(stage)), riskOutcome, restriction,
		actorproof.S(wr.PlayerAccountID.String()), actorproof.S(wr.WalletID.String()), actorproof.S(wr.AssetCode),
		actorproof.S(strconv.FormatInt(wr.Amount, 10)))
}

// attachDispatchAllowProof signs and attaches the proof an ALLOWED decision row needs (before its INSERT).
func attachDispatchAllowProof(ctx context.Context, tx pgx.Tx, wr WithdrawalRequest, stage DispatchStage, attemptID *uuid.UUID, riskOutcome, restriction *string) error {
	if prepare := actorproof.TestEnsureKeyHook; prepare != nil { // nil in production (integration test support only)
		if err := prepare(ctx, tx); err != nil {
			return fmt.Errorf("%w: %w", ErrDispatchProofUnavailable, err)
		}
	}
	err := actorproof.Default().Attach(ctx, tx, actorproof.Claims{
		Actor: actorproof.SystemActorDispatchGate(), Scope: actorproof.ScopeSystem, Tenant: wr.TenantID,
		Operation: actorproof.OpDispatchGateAllow, Target: wr.ID.String(),
		PayloadHash: dispatchAllowDigest(wr, stage, attemptID, riskOutcome, restriction),
	})
	if err != nil {
		if errors.Is(err, actorproof.ErrNoIssuer) || errors.Is(err, actorproof.ErrInvalidClaims) {
			return fmt.Errorf("%w: %w", ErrDispatchProofUnavailable, err)
		}
		return err
	}
	return nil
}
