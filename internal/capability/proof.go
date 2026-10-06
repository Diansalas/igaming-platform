package capability

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
)

// SIGNED-ACTOR-PROOF for K1 (ADR 0110, migration 0120, review finding H2): every
// capability-grant request / cancel / approval / revoke is accepted by the
// database only with a proof signed by the application server for the
// authenticated principal. The HTTP layer (after authentication and
// authorisation) puts the principal into the context with WithProofActor; the
// functions in this package sign for exactly that actor.

// ProofActor is the authenticated principal of one K1 call. Scope is
// actorproof.ScopeTenant (Tenant is the caller's tenant) or
// actorproof.ScopePlatform (Tenant is uuid.Nil).
type ProofActor struct {
	Actor  uuid.UUID
	Scope  string
	Tenant uuid.UUID
	Proofs *actorproof.Issuer // nil means actorproof.Default()
}

// Proof scopes (re-exported so HTTP code need not import actorproof).
const (
	ProofScopeTenant   = actorproof.ScopeTenant
	ProofScopePlatform = actorproof.ScopePlatform
)

type proofActorKey struct{}

// WithProofActor returns ctx carrying the authenticated principal, for the
// capability functions below to sign for. It must be called only by code that
// has authenticated the principal (the HTTP handler, from the verified token).
func WithProofActor(ctx context.Context, a ProofActor) context.Context {
	return context.WithValue(ctx, proofActorKey{}, a)
}

// TestSessionProofHook is nil in every production binary. Integration tests set it
// (internal/actorproof/prooftest, integration builds only) so fixtures that open
// their own sessions and call this package without an authenticated HTTP
// principal get a proof for the session's own actor. A static test pins that
// nothing else assigns it.
var TestSessionProofHook func(ctx context.Context, tx pgx.Tx, operation, target, payloadHash string) error

func attachProof(ctx context.Context, tx pgx.Tx, operation, target, payloadHash string) error {
	if a, ok := ctx.Value(proofActorKey{}).(ProofActor); ok {
		iss := a.Proofs
		if iss == nil {
			iss = actorproof.Default()
		}
		return iss.Attach(ctx, tx, actorproof.Claims{
			Actor: a.Actor, Scope: a.Scope, Tenant: a.Tenant,
			Operation: operation, Target: target, PayloadHash: payloadHash,
		})
	}
	if TestSessionProofHook != nil {
		return TestSessionProofHook(ctx, tx, operation, target, payloadHash)
	}
	// No authenticated principal in the context and no test hook: attach nothing,
	// the database refuses the write (AP001) - fail closed.
	return nil
}

// createDigest is the digest the zz_actor_proof_guard trigger recomputes from
// the request row (migration 0120).
func createDigest(tenantID, grantee uuid.UUID, capability Capability, validFrom time.Time, validUntil *time.Time, reason string) string {
	vf := validFrom
	return actorproof.Digest(actorproof.S(tenantID.String()), actorproof.S(grantee.String()), actorproof.S(string(capability)),
		actorproof.TS(&vf), actorproof.TS(validUntil), actorproof.S(reason))
}

// requestDigest is the request's content digest as an approver or the requester
// reviews it: SHA-256(k2_canonical(id, tenant_id, grantee_staff_id, capability,
// valid_from, valid_until, reason_code)), identical to the SQL function
// actor_proof_k1_request_digest the zz_actor_proof_guard trigger evaluates. It is
// computed in Go from the row read inside the same transaction.
func requestDigest(ctx context.Context, tx pgx.Tx, requestID uuid.UUID) (string, error) {
	var (
		id, tenantID, grantee uuid.UUID
		capability, reason    string
		validFrom             time.Time
		validUntil            *time.Time
	)
	if err := tx.QueryRow(ctx, `SELECT id, tenant_id, grantee_staff_id, capability, valid_from, valid_until, reason_code
		FROM staff_capability_grant_requests WHERE id = $1`, requestID).
		Scan(&id, &tenantID, &grantee, &capability, &validFrom, &validUntil, &reason); err != nil {
		return "", err
	}
	return actorproof.Digest(actorproof.S(id.String()), actorproof.S(tenantID.String()), actorproof.S(grantee.String()), actorproof.S(capability),
		actorproof.TS(&validFrom), actorproof.TS(validUntil), actorproof.S(reason)), nil
}

func revokeDigest(grantID, tenantID uuid.UUID, reason string) string {
	return actorproof.Digest(actorproof.S(grantID.String()), actorproof.S(tenantID.String()), actorproof.S(reason))
}
