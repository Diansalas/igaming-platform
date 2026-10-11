// Package launchgov is the governed launch authorisation service of ADR 0112 (slice 3):
// the ONLY non-test writer of tenants.status and brands.status (a static test pins it).
//
// What it does. A tenant or a brand is created pending_launch; its status moves only through a
// recorded, manual, four-eyes, signed decision: request (activate / reactivate / close /
// ratify) -> approvals by distinct platform staff AND distinct Persons -> execution, or an
// immediate suspension by one authorised person (the fail-closed direction). Every write
// carries an ADR 0110 signed actor proof for the authenticated principal (migration 0130), so a
// runtime session that merely sets session GUCs can no longer impersonate the requester or the
// approvers. The executor absorbs the former tenant.ChangeStatus semantics (the GP020 closure
// refusal and its separate-transaction refusal audit, lock_timeout, the tenant-then-brand lock
// order) and the S6 brand path (app.tenant_id bound transaction-locally to the subject's own
// tenant, the migration-0121 technique).
//
// What it is NOT. A record produced here is evidence that named people took a decision on
// stated inputs. It is NOT a legal, regulatory or licensing authorisation, and the software
// gives no legal advice: readiness checks prove configuration presence only; licensing is
// recorded, never judged ("no licence" never blocks by itself; four recorded bases exist).
//
// Policies decided in slice 3 (ADR 0112 section 13): a recorded reject carries over to a later
// request with the identical subject, action and payload hash within RequestTTL (migration 0130,
// LA014 / ErrPriorReject; any change of content is a different request); a tenant's own-brand
// suspension supersedes the pending requests of that brand and records each superseded request
// id in the audit trail; approvals older than Config.ApprovalMaxAge, or decided on a readiness
// whose hash is no longer current, are refused at execution (refused_at_execution, durable);
// operator-fixable refusals (GP020 open bets, LA023 hold-bearing withdrawals, a lock timeout)
// roll the execution back as a unit and leave the request pending.
//
// Status of the implementation (CLAUDE.md labels): the service, readiness evaluator, executor
// and audit are IMPLEMENTED against the local database; no HTTP route or console exists yet
// (ADR 0112 slices 4-5), so nothing here is reachable by an operator. The readiness checks
// R-PROVIDER-TIER and R-ALERTS describe the process and the alert vocabulary; R-CATALOGUE
// covers casino availability only (the sportsbook catalogue is not brand-scoped).
package launchgov

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/auth"
)

// Disclaimer is the fixed text every user-facing surface of this feature carries.
const Disclaimer = "A recorded launch decision is evidence that named people took a decision on stated inputs. It is not a legal or regulatory authorisation, and it does not determine whether a licence is required or whether any jurisdiction permits operation."

// Closed vocabularies (CHECK plus Go constants; a new value needs a migration and an ADR).
const (
	KindTenant = "tenant"
	KindBrand  = "brand"

	ActionActivate   = "activate"
	ActionSuspend    = "suspend"
	ActionReactivate = "reactivate"
	ActionClose      = "close"
	ActionRatify     = "ratify"

	StatusPendingLaunch = "pending_launch"
	StatusActive        = "active"
	StatusSuspended     = "suspended"
	StatusClosed        = "closed"

	ReqPending           = "pending"
	ReqExecuting         = "executing"
	ReqExecuted          = "executed"
	ReqRefusedAtExec     = "refused_at_execution"
	ReqRejected          = "rejected"
	ReqCancelled         = "cancelled"
	ReqExpired           = "expired"
	ReqSuperseded        = "superseded"
	DecisionApprove      = "approve"
	DecisionReject       = "reject"
	ScopePlatform        = actorproof.ScopePlatform
	ScopeTenant          = actorproof.ScopeTenant
	LicModelPlatform     = "platform_licence"
	LicModelTenant       = "tenant_licence"
	LicModelOther        = "other_manually_approved"
	LicModelNotApplic    = "not_applicable_recorded_determination"
	LicStatusInForce     = "in_force"
	LicStatusAppliedPend = "applied_pending"
	LicStatusConditional = "conditional"
	LicStatusSuspended   = "suspended"
	LicStatusExpired     = "expired"
	LicStatusNotRequired = "not_required_per_determination"
)

// RequestTTL is the technical lifetime of a pending request (ADR 0112 3.2 S3: kept only so the
// one-pending-per-subject UNIQUE can never deadlock a new request; NOT a policy value). It is also
// the reject carry-over window (migration 0130).
const RequestTTL = 72 * time.Hour

// DefaultApprovalMaxAge bounds how old an approval may be when the request executes (ADR 0112
// 3.2 S1, slice-3 obligation). An engineering default, configurable per Service; not a policy value.
const DefaultApprovalMaxAge = 24 * time.Hour

// Actor is the authenticated principal of one call, built by the caller (the HTTP layer, slice 4)
// from the verified token. Nothing in it comes from a request body. The service re-checks the
// static permission, the scope rules and the actor's existence (the database does, by session).
type Actor struct {
	StaffID  uuid.UUID
	Scope    string    // ScopePlatform or ScopeTenant; platform_acting is refused
	TenantID uuid.UUID // the actor's own tenant for ScopeTenant; uuid.Nil for ScopePlatform
	Role     auth.Role

	IPAddress string
	RequestID string
}

// Subject names the thing whose status moves. BrandID nil = the tenant itself. Both ids come
// from a route-validated target, never from a body.
type Subject struct {
	TenantID uuid.UUID
	BrandID  *uuid.UUID
}

// Kind is KindBrand when a brand id is present.
func (s Subject) Kind() string {
	if s.BrandID != nil {
		return KindBrand
	}
	return KindTenant
}

// Sentinel errors. Callers branch with errors.Is / errors.As; the HTTP layer (slice 4) maps them
// to the closed LAUNCH_* codes of ADR 0112 section 9.
var (
	ErrForbidden            = errors.New("launchgov: the actor's role or scope does not permit this operation")
	ErrActingSession        = errors.New("launchgov: an acting session may not act here")
	ErrInvalidInput         = errors.New("launchgov: invalid input")
	ErrNotFound             = errors.New("launchgov: not found")
	ErrIllegalTransition    = errors.New("launchgov: LAUNCH_ILLEGAL_TRANSITION")
	ErrSubjectClosed        = errors.New("launchgov: LAUNCH_SUBJECT_CLOSED")
	ErrRequestPending       = errors.New("launchgov: LAUNCH_REQUEST_PENDING")
	ErrSelfApproval         = errors.New("launchgov: LAUNCH_SELF_APPROVAL")
	ErrNotPending           = errors.New("launchgov: the request is not pending (decided, executed, expired or superseded)")
	ErrUnacknowledged       = errors.New("launchgov: LAUNCH_UNACKNOWLEDGED_CHECKS")
	ErrPreconditionFailed   = errors.New("launchgov: LAUNCH_PRECONDITION_FAILED")
	ErrStaleReadiness       = errors.New("launchgov: the readiness the decider saw is stale; re-read it and decide again")
	ErrPriorReject          = errors.New("launchgov: an identical request was rejected within the request TTL")
	ErrWithdrawalsHolding   = errors.New("launchgov: LAUNCH_WITHDRAWALS_HOLDING_FUNDS")
	ErrRetryableLockTimeout = errors.New("launchgov: LAUNCH_RETRYABLE_LOCK_TIMEOUT")
	ErrIsolationLevel       = errors.New("launchgov: an approval-based execution requires a READ COMMITTED transaction")
	ErrProofUnavailable     = errors.New("launchgov: no actor-proof issuer is configured (governed writes fail closed)")
)

// PreconditionError carries the machine-readable codes of a refused hard precondition (H-1..H-3)
// or readiness acknowledgement (H-5). errors.Is matches ErrPreconditionFailed or ErrUnacknowledged.
type PreconditionError struct {
	Base  error    // ErrPreconditionFailed or ErrUnacknowledged
	Codes []string // e.g. H-2:licence_id_required, R-LIC-VALID
}

func (e *PreconditionError) Error() string   { return fmt.Sprintf("%v: %v", e.Base, e.Codes) }
func (e *PreconditionError) Is(t error) bool { return t == e.Base }
func (e *PreconditionError) Unwrap() error   { return e.Base }

// PriorRejectError names the earlier rejected request an identical new request collides with.
type PriorRejectError struct {
	RequestID  uuid.UUID
	DecidedAt  time.Time
	RetryAfter time.Time
}

func (e *PriorRejectError) Error() string {
	return fmt.Sprintf("%v (request %s, rejected %s; retry unchanged only after %s)", ErrPriorReject, e.RequestID, e.DecidedAt.UTC().Format(time.RFC3339), e.RetryAfter.UTC().Format(time.RFC3339))
}
func (e *PriorRejectError) Is(t error) bool { return t == ErrPriorReject }
