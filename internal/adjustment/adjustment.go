// Package adjustment is PRH-2 K2's governed manual adjustment service (ADR
// 0100): a four-eyes, payload-hash-pinned request/approval flow whose final
// approval executes the ONE allowed posting shape (the tenant's
// manual_adjustment house account <-> one player's player_cash, same
// asset) through ledger.Post, in the final approval's own transaction.
//
// The authority decisions live in migration 0113's triggers and SQL
// functions (the forced actor, the in-force capability grant, the
// distinct-Person floor, the S-12 beneficiary exclusion, S-2(iii), the
// reason-code catalogue, the MA020 open-payment-exposure refusal, the
// suspended-asset rule, the policy evaluation, the state machine and the
// link trigger). There is no second implementation of any of them here:
// this package builds the statements, takes the ADR 0082 Amendment A8
// locks in order, runs the ledger posting, classifies errors by SQLSTATE
// and writes the audit rows.
//
// It is also the ONLY non-payments caller of db.WithPlatformActingInTenant
// (ADR 0099 §6.1; A-16), from exactly one call site (runSession), whose
// arguments are pinned by a static test: the principal is parsed from
// tenant.FromContext(ctx).Subject in the same function, and the target
// tenant is a Target, constructible only through NewTarget, which applies
// the canActOnTenant rule to the route's path value.
package adjustment

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// OperationKind is the ADR 0100 §2 classification key for this package.
const OperationKind = "ledger_adjustment"

// Scope values forced by migration 0113 onto every actor column.
const (
	ScopeTenant         = "tenant"
	ScopePlatformActing = "platform_acting"
)

// Direction is the request direction (ADR 0100 §4).
type Direction string

const (
	DirectionCreditPlayer Direction = "credit_player"
	DirectionDebitPlayer  Direction = "debit_player"
)

// ReasonCode is one of the closed catalogue codes (ADR 0100 §5.4, LF
// ruling 1). The catalogue itself is migration-written reference data.
type ReasonCode string

const (
	ReasonOperationalErrorCorrection ReasonCode = "operational_error_correction"
	ReasonCompensatingEntry          ReasonCode = "compensating_entry"
	ReasonGoodwillCredit             ReasonCode = "goodwill_credit"
	ReasonExternalInstruction        ReasonCode = "external_instruction"
)

// State mirrors ledger_adjustment_requests.state.
type State string

const (
	StatePending                  State = "pending"
	StateExecuting                State = "executing"
	StateExecuted                 State = "executed"
	StateRefusedInsufficientFunds State = "refused_insufficient_funds"
	StateRefusedAtExecution       State = "refused_at_execution"
	StateRejected                 State = "rejected"
	StateCancelled                State = "cancelled"
	StateExpired                  State = "expired"
)

// Sentinel errors for conditions this package detects itself.
var (
	// ErrForeignTenant: a tenant-scoped caller named a tenant other than
	// its own (the canActOnTenant rule).
	ErrForeignTenant = errors.New("adjustment: caller may not act on this tenant")
	// ErrNotFound: no such request visible in this tenant.
	ErrNotFound = errors.New("adjustment: not found")
	// ErrNotPending: the request is no longer pending.
	ErrNotPending = errors.New("adjustment: request is not pending")
	// ErrRequestExpired: the request was past expires_at when a decision
	// arrived. The pending -> expired transition and its audit row ARE
	// committed; the decision is not recorded (code review R-2).
	ErrRequestExpired = errors.New("adjustment: request expired")
	// ErrInvalidInput: a request failed Go-side validation before any SQL.
	ErrInvalidInput = errors.New("adjustment: invalid input")
	// ErrNoAuthContext: no verified token subject in ctx.
	ErrNoAuthContext = errors.New("adjustment: no authenticated context")
	// ErrIntegrity: a condition the design makes unreachable occurred
	// (e.g. an idempotent replay of a never-executed request's key).
	ErrIntegrity = errors.New("adjustment: integrity violation")
)

// Target is a route-validated target tenant. Its only constructor applies
// the canActOnTenant rule (a tenant caller may name only its own tenant; a
// platform caller may name any tenant, and then acts ONLY through an ADR
// 0099 §6 acting session that the database refuses without an in-force
// grant for exactly this tenant).
type Target struct {
	tenantID uuid.UUID
}

// TenantID returns the validated target tenant.
func (t Target) TenantID() uuid.UUID { return t.tenantID }

// NewTarget validates pathTenantID (the route's {tenantID} path value,
// never a body or query field) against the caller's authenticated
// context.
func NewTarget(tc tenant.Context, pathTenantID uuid.UUID) (Target, error) {
	if pathTenantID == uuid.Nil {
		return Target{}, fmt.Errorf("%w: nil target tenant", ErrInvalidInput)
	}
	if tc.TenantID != uuid.Nil && tc.TenantID != pathTenantID {
		return Target{}, ErrForeignTenant
	}
	return Target{tenantID: pathTenantID}, nil
}

// Meta is the non-authoritative request metadata recorded in audit rows.
type Meta struct {
	IPAddress string
	UserAgent string
	RequestID string
}

// Call identifies the actor of one governed call, derived from the
// verified token and the session actually opened - never from a body.
type Call struct {
	ActorID  uuid.UUID
	TenantID uuid.UUID
	Scope    string
	Meta     Meta
	// Proofs signs the SIGNED-ACTOR-PROOF (ADR 0110, migration 0120) for this
	// call's governed writes. nil means the process-wide actorproof.Default().
	Proofs *actorproof.Issuer
}

// attachProof signs and attaches the actor proof for ONE governed write. It is
// called only after runSession has authenticated the principal (verified token
// subject) and the caller's own server-side checks have run; the database
// verifies it (migration 0120) and refuses the write without it. Any signing
// failure - a missing issuer or a claim set the database could never admit - is
// returned: it is a server-side defect (HTTP 500), never silently skipped.
func (c Call) attachProof(ctx context.Context, tx pgx.Tx, operation, target, payloadHash string) error {
	iss := c.Proofs
	if iss == nil {
		iss = actorproof.Default()
	}
	return iss.Attach(ctx, tx, actorproof.Claims{
		Actor: c.ActorID, Scope: c.Scope, Tenant: c.TenantID,
		Operation: operation, Target: target, PayloadHash: payloadHash,
	})
}

// Service runs governed calls against a pool.
type Service struct {
	pool   *db.Pool
	proofs *actorproof.Issuer
}

// NewService returns a Service over pool. Proofs are signed with the
// process-wide actorproof.Default() issuer unless WithProofIssuer overrides it.
func NewService(pool *db.Pool) *Service { return &Service{pool: pool} }

// WithProofIssuer sets the issuer this Service signs actor proofs with.
func (s *Service) WithProofIssuer(i *actorproof.Issuer) *Service {
	s.proofs = i
	return s
}

// runSession opens the ONE session shape migration 0113 admits for the
// caller: a tenant caller gets db.WithPrincipalScope (tenant family); a
// platform caller gets db.WithPlatformActingInTenant (acting family, ADR
// 0099 §6.1) for the Target's tenant. The plain platform session has no
// policy on any K2 request table (ADR 0100 §6.6), so it is never used.
//
// A-16 / K2-G4 (§6.1 call-site pin): principalID is `subject`, parsed from
// tenant.FromContext(ctx).Subject in THIS function; targetTenantID is
// `target.tenantID`, set only by NewTarget.
func (s *Service) runSession(ctx context.Context, target Target, requestID uuid.UUID, meta Meta, fn func(ctx context.Context, tx pgx.Tx, call Call) error) error {
	tc, err := tenant.FromContext(ctx)
	if err != nil {
		return ErrNoAuthContext
	}
	subject, err := uuid.Parse(tc.Subject)
	if err != nil || subject == uuid.Nil {
		return ErrNoAuthContext
	}
	if target.tenantID == uuid.Nil {
		return fmt.Errorf("%w: unvalidated target", ErrInvalidInput)
	}
	if tc.TenantID == uuid.Nil {
		return s.pool.WithPlatformActingInTenant(ctx, subject, target.tenantID, requestID, OperationKind, func(ctx context.Context, tx pgx.Tx) error {
			return fn(ctx, tx, Call{ActorID: subject, TenantID: target.tenantID, Scope: ScopePlatformActing, Meta: meta, Proofs: s.proofs})
		})
	}
	if tc.TenantID != target.tenantID {
		return ErrForeignTenant
	}
	return s.pool.WithPrincipalScope(ctx, tc.TenantID, subject, func(ctx context.Context, tx pgx.Tx) error {
		return fn(ctx, tx, Call{ActorID: subject, TenantID: tc.TenantID, Scope: ScopeTenant, Meta: meta, Proofs: s.proofs})
	})
}

// ErrClass classifies a K2 error for the HTTP layer.
type ErrClass string

const (
	ErrClassNone           ErrClass = ""
	ErrClassNotFound       ErrClass = "not_found"
	ErrClassForbidden      ErrClass = "forbidden"
	ErrClassConflict       ErrClass = "conflict"
	ErrClassInvalid        ErrClass = "invalid"
	ErrClassDisabled       ErrClass = "disabled"
	ErrClassExposure       ErrClass = "open_payment_exposure"
	ErrClassSessionInvalid ErrClass = "session_invalid"
	// ErrClassRefusedInvalid: a DB payload-rule refusal (MA021 asset rule,
	// MA022 reason-code rule, MA025 note) - a 400 with the closed token
	// from RefusalToken (code review R-3).
	ErrClassRefusedInvalid ErrClass = "validation_refused"
	// ErrClassExpired: ErrRequestExpired (code review R-2) - a 409.
	ErrClassExpired ErrClass = "expired"
	// ErrClassRetryable: a serialization failure or deadlock (40001,
	// 40P01) - a retryable 409, never a 500 (code review R-3).
	ErrClassRetryable ErrClass = "retryable"
	ErrClassOther     ErrClass = "other"
)

// SQLState returns err's SQLSTATE, or "".
func SQLState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// ClassifyError maps an error to an ErrClass, by sentinel or SQLSTATE
// only - never by message text.
func ClassifyError(err error) ErrClass {
	switch {
	case err == nil:
		return ErrClassNone
	case errors.Is(err, ErrNotFound), errors.Is(err, pgx.ErrNoRows):
		return ErrClassNotFound
	case errors.Is(err, ErrForeignTenant), errors.Is(err, ErrNoAuthContext):
		return ErrClassForbidden
	case errors.Is(err, ErrNotPending):
		return ErrClassConflict
	case errors.Is(err, ErrRequestExpired):
		return ErrClassExpired
	case errors.Is(err, ErrInvalidInput):
		return ErrClassInvalid
	}
	code := SQLState(err)
	switch {
	case code == "MA014":
		return ErrClassDisabled
	case code == "MA021", code == "MA022", code == "MA025":
		return ErrClassRefusedInvalid
	case code == "40001", code == "40P01":
		return ErrClassRetryable
	case code == "MA020":
		return ErrClassExposure
	case code == "MA001", code == "MA003", code == "42501":
		return ErrClassForbidden
	case len(code) == 5 && code[:2] == "AP":
		// SIGNED-ACTOR-PROOF refusal (migration 0120): fail closed.
		return ErrClassForbidden
	case code == "CG001", code == "CG002", code == "CG020", code == "MA002":
		return ErrClassSessionInvalid
	case len(code) == 5 && code[:2] == "MA", code == "CG030", code == "CG031", code == "23505":
		return ErrClassConflict
	case code == "23514", code == "22003", code == "23503":
		return ErrClassInvalid
	}
	return ErrClassOther
}

// refusalTokens is the CLOSED set of payload-rule refusal tokens migration
// 0113 emits (ledger_adjustment_payload_refusal's 'SQLSTATE:token' values,
// raised as "...: refused: <token>"). Only a token in this set is ever
// exposed to a client; anything else maps to its code's generic token.
var refusalTokens = map[string]bool{
	"unknown_reason_code": true, "direction_not_allowed_for_reason": true,
	"wallet_not_found": true, "asset_mismatch": true, "asset_suspended": true,
	"evidence_required": true, "causation_forbidden": true, "causation_required": true,
	"causation_not_found": true, "causation_type_refused": true, "causation_not_on_wallet": true,
	"compensation_cap_exceeded": true,
}

// RefusalToken returns the closed, client-safe refusal token for an
// ErrClassRefusedInvalid error: the split_part token when it is in the
// closed set, else a generic per-code token. Never message text.
func RefusalToken(err error) string {
	code := SQLState(err)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if i := strings.LastIndex(pgErr.Message, "refused: "); i >= 0 {
			if tok := pgErr.Message[i+len("refused: "):]; refusalTokens[tok] {
				return tok
			}
		}
	}
	switch code {
	case "MA021":
		return "asset_rule"
	case "MA022":
		return "reason_code_rule"
	case "MA025":
		return "note_invalid"
	}
	return "invalid"
}
