package capability

import (
	"context"
	"errors"
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
	// Role and StaffVersion are the verified token's role and `sv` claim (ADR 0119 S5 + S6, the S4b-R3 hard
	// gate): a grant approval or revocation re-asserts the actor's own staff row against them, FOR SHARE, at the
	// A8 position (after the request / grant row FOR UPDATE). Empty Role is refused.
	Role         string
	StaffVersion int64
}

// reassertActor is the ADR 0119 section 9.5 K1 re-assertion (approve and revoke; ADR 0082 A9): the acting staff
// member's own row FOR SHARE, compared with the verified token's role and staff_version. It runs only for an
// authenticated principal in the context (every HTTP K1 call). Without one it refuses in production (review fix F7);
// the integration-only TestSessionProofHook path (fixtures that open their own sessions without a token) skips it
// through the build-tag split (reassert_integration.go). The row is visible in both K1 session shapes: a tenant
// caller's own row through the dual-scope policy (db.WithPrincipalScope), a platform caller's own platform row
// because db.WithPlatformAdmin leaves app.tenant_id empty - so the section 9.5(b) app.actor_staff_id pair is not
// needed here (verified by TestK1Reassertion_*).
func reassertActor(ctx context.Context, tx pgx.Tx) error {
	a, ok := ctx.Value(proofActorKey{}).(ProofActor)
	if !ok {
		// Review fix F7: no authenticated principal is a refusal in production (reassert_prod.go); only the
		// integration build may skip it for token-less fixtures (reassert_integration.go).
		if reassertFallbackOn() {
			return nil
		}
		return ErrActorNotLive
	}
	if a.Actor == uuid.Nil || a.Role == "" || a.StaffVersion < 1 {
		return ErrActorNotLive
	}
	var role, status string
	var version int64
	err := tx.QueryRow(ctx, `SELECT role, status, staff_version FROM staff_users WHERE id = $1 FOR SHARE`, a.Actor).Scan(&role, &status, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return actorLiveDecision(a, nil)
	}
	if err != nil {
		return err
	}
	return actorLiveDecision(a, &actorRow{Role: role, Status: status, StaffVersion: version})
}

// actorRow is the actor's own staff row as the re-assertion read it (nil: not visible).
type actorRow struct {
	Role         string
	Status       string
	StaffVersion int64
}

// actorLiveDecision is the pure rule of reassertActor - the SAME rule as db.CheckActorStaffLive (this package cannot
// import internal/db). Ledger-finance review LF-3: TestReassertDecision_EquivalentToCheckActorStaffLive pins that the
// two agree on a shared table of cases, so a drift in either fails a unit test.
func actorLiveDecision(a ProofActor, row *actorRow) error {
	if a.Actor == uuid.Nil || a.Role == "" || a.StaffVersion < 1 || row == nil {
		return ErrActorNotLive
	}
	if row.Status != "active" || row.Role != a.Role || row.StaffVersion != a.StaffVersion {
		return ErrActorNotLive
	}
	return nil
}

// ErrActorNotLive is the K1 re-assertion refusal (the same rule as db.CheckActorStaffLive; internal/capability does
// not import internal/db because the db integration tests import this package). The HTTP layer answers 401
// session_revoked; nothing was written.
var ErrActorNotLive = errors.New("capability: the acting staff member is no longer live")

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
