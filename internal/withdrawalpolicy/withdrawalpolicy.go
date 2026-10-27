// Package withdrawalpolicy is the GOVERNED write and read side of the per-tenant withdrawal restriction policy (lic11,
// migration 0182). The evaluation on the money paths lives in internal/withdrawal (restriction.go); this package decides
// whether and when a policy row is written, and shows operators what is in force.
//
// Governance (the 0177 licensing maker-checker semantics, mirrored on dedicated tables because the licensing kind
// registry and classifier may not be extended in this slice; record D-3):
//
//   - only a platform_admin in a PLATFORM session requests, approves, rejects, cancels or executes; a tenant (any role) can
//     only READ its own effective policy and versions (FORCE RLS), never write it;
//   - the DATABASE classifies a change: WIDENING when the requested action is less strict (allow < hold_for_review < block)
//     than the effective action at any stage; a widening change needs 2 approvals by distinct Persons that are neither the
//     requester's Person nor the requester's account, each at most 24 h old at execution, and executes with the final
//     approval; a NARROWING (or equal) change is single actor with a reason code and executes in the request's own
//     transaction (fail-closed asymmetry, as for licensing / launch suspension);
//   - every request, approval, rejection, cancellation and execution is proof-bound (ADR 0110, operation family
//     withdrawal_restriction_policy:*, platform scope); the database re-classifies at execution (WR024);
//   - every event writes an audit row (actor, subject tenant, IP, user agent, request id, reason code, before / after);
//   - the policy row itself is append-only and versioned (per-tenant set_version), written under the tenant status gate key
//     EXCLUSIVE so it serialises with every withdrawal decision that reads it.
//
// The package ships NO policy and no legal value. With no row, the interim engineering default (the registry's, equal to
// the behaviour before 0182) is in force, and the read API says so ("default_unconfigured").
//
// Not a legal or regulatory authorisation: an executed change is a decision recorded by named staff.
package withdrawalpolicy

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
)

// Causes, actions and stages (the registry's closed vocabularies; the database is authoritative).
const (
	ActionAllow         = "allow"
	ActionHoldForReview = "hold_for_review"
	ActionBlock         = "block"

	StageRequest    = "request"
	StageDispatch   = "dispatch"
	StageRedispatch = "redispatch"

	SourceConfigured = "configured"
	SourceDefault    = "default_unconfigured"
)

// Stages is the evaluation order of the read API.
var Stages = []string{StageRequest, StageDispatch, StageRedispatch}

// Change statuses.
const (
	StatusRequested = "requested"
	StatusApproved  = "approved"
	StatusExecuted  = "executed"
	StatusRejected  = "rejected"
	StatusCancelled = "cancelled"
	StatusExpired   = "expired"
)

// DefaultApprovalMaxAge bounds the age of a counted approval (the database caps it at 24 h too).
const DefaultApprovalMaxAge = 24 * time.Hour

// Disclaimer accompanies every read of the policy.
const Disclaimer = "A withdrawal restriction policy is an operator configuration recorded by named staff. It is not a legal or regulatory authorisation and does not determine what any licence or jurisdiction requires."

// UnconfiguredNotice is the operator-facing informational note when a tenant has NO configured row for a configurable cause
// (proposed for the launch readiness surface as an informational item; see the record, residual R-5).
const UnconfiguredNotice = "UNCONFIGURED: no withdrawal restriction policy is configured for this cause. The INTERIM ENGINEERING DEFAULT is in force: it is the behaviour before migration 0182 (withdrawal requests, payout claims and re-dispatch proceed whatever the licence state). It is not an approved policy (owner decision Q12 pending)."

var (
	// ErrForbidden: the actor may not perform this call.
	ErrForbidden = errors.New("withdrawalpolicy: forbidden")
	// ErrNotFound: unknown change or tenant, or one the actor may not see.
	ErrNotFound = errors.New("withdrawalpolicy: not found")
	// ErrInvalidInput: malformed input.
	ErrInvalidInput = errors.New("withdrawalpolicy: invalid input")
	// ErrNotConfigurable: the cause is governed elsewhere (owner decision) or not evaluable; no policy row may be written.
	ErrNotConfigurable = errors.New("withdrawalpolicy: the cause is not configurable")
	// ErrNotPending: the change is no longer open.
	ErrNotPending = errors.New("withdrawalpolicy: the change is not open")
	// ErrSelfApproval: the requester (or an account of the requester's Person) may not decide the change.
	ErrSelfApproval = errors.New("withdrawalpolicy: the requester's Person may not decide the change")
	// ErrAlreadyDecided: this Person already approved the change.
	ErrAlreadyDecided = errors.New("withdrawalpolicy: this Person already approved the change")
	// ErrNoPerson: a staff account has no readable Person; four-eyes cannot be evaluated (fail closed).
	ErrNoPerson = errors.New("withdrawalpolicy: a staff account has no readable Person; four-eyes cannot be evaluated")
	// ErrApprovalsStale: enough distinct approvals exist, but not within the approval window.
	ErrApprovalsStale = errors.New("withdrawalpolicy: approvals are older than the approval window; file a new change")
	// ErrDuplicatePending: an open change for the same tenant and cause exists.
	ErrDuplicatePending = errors.New("withdrawalpolicy: an open change for this tenant and cause already exists")
	// ErrProofUnavailable: no actor-proof issuer; governed writes fail closed.
	ErrProofUnavailable = errors.New("withdrawalpolicy: no actor-proof issuer configured (governed writes fail closed)")
	// ErrRetryableLockTimeout: lock timeout or deadlock; retry.
	ErrRetryableLockTimeout = errors.New("withdrawalpolicy: lock timeout; retry")
	// ErrIsolationLevel: the transaction is not READ COMMITTED.
	ErrIsolationLevel = errors.New("withdrawalpolicy: governed transactions must be READ COMMITTED")
)

// GuardError is a database guard refusal (SQLSTATE class WR, LC or AP) with no more specific sentinel.
type GuardError struct {
	Code    string
	Message string
}

func (e *GuardError) Error() string {
	return fmt.Sprintf("withdrawalpolicy: database guard %s: %s", e.Code, e.Message)
}

// GuardCode returns the SQLSTATE of a GuardError, or "".
func GuardCode(err error) string {
	var ge *GuardError
	if errors.As(err, &ge) {
		return ge.Code
	}
	return ""
}

// Actor scopes.
const (
	ScopePlatform = "platform"
	ScopeTenant   = "tenant"
)

// Actor is the authenticated principal, built by the caller from the verified token (never from a body). A platform actor
// carries no tenant; a tenant actor (reads only) carries its own.
type Actor struct {
	StaffID   uuid.UUID
	Scope     string
	TenantID  uuid.UUID
	Role      string
	RequestID string
	IPAddress string
	UserAgent string
}

// Change is a withdrawal_restriction_policy_changes row (plus its approvals on Get).
type Change struct {
	ID                  uuid.UUID
	TenantID            uuid.UUID
	Cause               string
	Action              string
	ReasonCode          string
	Note                *string
	PayloadHash         string
	Widening            bool
	RequiredApprovals   int
	BaseSetVersion      int64
	RequestedBy         uuid.UUID
	RequestedByPersonID uuid.UUID
	Status              string
	CreatedAt           time.Time
	ExpiresAt           time.Time
	DecidedBy           *uuid.UUID
	DecidedAt           *time.Time
	DecisionReasonCode  *string
	ExecutedBy          *uuid.UUID
	ExecutedAt          *time.Time
	Approvals           []Approval
}

// Approval is a withdrawal_restriction_policy_approvals row.
type Approval struct {
	ID               uuid.UUID
	ChangeID         uuid.UUID
	ApproverID       uuid.UUID
	ApproverPersonID uuid.UUID
	PayloadHash      string
	ReasonCode       string
	DecidedAt        time.Time
}

// Policy is a withdrawal_restriction_policies row (one version).
type Policy struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	Cause      string
	Action     string
	SetVersion int64
	ChangeID   *uuid.UUID
	ReasonCode string
	CreatedBy  *uuid.UUID
	CreatedAt  time.Time
}

// Result is the outcome of a request or an approval.
type Result struct {
	Change   Change
	Executed bool
	// Policy is the written version, set only when Executed.
	Policy *Policy
}

// RequestInput is a change as filed.
type RequestInput struct {
	TenantID   uuid.UUID
	Cause      string
	Action     string
	ReasonCode string
	Note       string
}

// ListFilter narrows ListChanges.
type ListFilter struct {
	TenantID uuid.UUID
	Status   string
	Limit    int
}

// StageEffect is the effective action of a cause at one stage.
type StageEffect struct {
	Stage      string
	Action     string
	Source     string
	PolicyID   *uuid.UUID
	SetVersion *int64
}

// CauseView is one registry cause with its effective actions for the tenant.
type CauseView struct {
	Cause              string
	Class              string
	Configurable       bool
	Evaluated          bool
	GoverningReference string
	CurrentBehaviour   string
	// Stages is set only for a configurable cause.
	Stages []StageEffect
	// Unconfigured is true for a configurable cause with no configured row (the interim default is in force).
	Unconfigured bool
	// Notice is UnconfiguredNotice when Unconfigured.
	Notice string
}

// EffectiveView is the read API's answer for one tenant.
type EffectiveView struct {
	TenantID   uuid.UUID
	SetVersion int64
	// Configured is true when at least one policy row exists for the tenant.
	Configured bool
	Causes     []CauseView
	// ActionSemantics states plainly what each action does (lic17 M-1); the same closed text on every read.
	ActionSemantics []ActionSemantic
	// Parked summarises the tenant's withdrawals currently parked by a WD056 refusal (lic17 M-1).
	Parked     ParkedSummary
	Disclaimer string
}

// ActionSemantic is the plain meaning of one action.
type ActionSemantic struct {
	Action  string
	Meaning string
}

// ActionSemantics is the closed, plain statement of the three actions (lic17 M-1, ledger-finance review). hold_for_review is a
// PARK, not a review queue: there is no per-withdrawal release in this policy (an owner decision; see docs/plans/lic17-record.md).
var ActionSemantics = []ActionSemantic{
	{ActionAllow, "allow: the withdrawal proceeds at this stage. Every other gate (tenant / brand status, KYC, RG, risk, market, kill switch) still applies."},
	{ActionHoldForReview, "hold_for_review: PARK, not a review queue. At the request stage the request is accepted and the amount moves to the player's withdrawal hold. At a payout dispatch point the payout is NOT sent (first dispatch: the request stays approved with its hold; sweeper re-dispatch: the attempt is escalated, no resend, no release) and the WD056 alert is raised. There is NO per-withdrawal release in this policy: the withdrawal moves again only when the cause clears (a re-submit, or the next sweeper tick, re-runs the gate) or the policy is widened by two distinct approvers. The amount stays on the player's withdrawal hold; it is not forfeited and not paid. For a non-active tenant or brand the governed release_hold_to_player (ADR 0111) returns an approved hold to the player; for an active tenant there is no such path today (residual, docs/plans/lic17-record.md)."},
	{ActionBlock, "block: at the request stage the request is refused and nothing is held. At a payout dispatch point the same park as hold_for_review."},
}

// ParkedSummary counts the tenant's withdrawal requests that are still open (approved or submitted) and whose LATEST dispatch
// gate decision is a WD056 refusal, with the earliest WD056 refusal among them (the park age). Visible is false for a platform
// session: the decision rows are tenant-scoped (RLS); a platform operator uses the withdrawal.payout_gate_denied alerts
// (attribute gate_code WD056; first_seen_at is the park start of each subject).
type ParkedSummary struct {
	Visible           bool
	Count             int64
	OldestParkedSince *time.Time
	// OldestAgeSeconds is the age of OldestParkedSince at the database clock.
	OldestAgeSeconds *int64
	Note             string
}

// ParkedPlatformNote is ParkedSummary.Note for a platform session.
const ParkedPlatformNote = "Not visible in a platform session (the gate decision rows are tenant-scoped). Use the withdrawal.payout_gate_denied alerts with gate_code WD056: one open alert per parked withdrawal or attempt, first_seen_at is when it was parked."

// ParkedTenantNote is ParkedSummary.Note for a tenant session.
const ParkedTenantNote = "Open withdrawals whose latest dispatch gate decision is a WD056 refusal. A parked withdrawal is NOT released by this policy; see the hold_for_review semantics."

var reasonCodeRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)

// ValidReasonCode reports whether s is a well-formed reason code (a code, never free text).
func ValidReasonCode(s string) bool { return reasonCodeRE.MatchString(s) }

// ValidAction reports whether s is one of the three actions.
func ValidAction(s string) bool { return ActionRank(s) >= 0 }

// ActionRank orders actions by strictness; -1 for an unknown action.
func ActionRank(s string) int {
	switch s {
	case ActionAllow:
		return 0
	case ActionHoldForReview:
		return 1
	case ActionBlock:
		return 2
	}
	return -1
}

// IsWidening mirrors withdrawal_restriction_is_widening (migration 0184; the database stays authoritative): the new action is
// less strict than the current effective action at any stage, OR (lic17 F-2) the new action is allow while the source at any
// stage is still default_unconfigured (the first configured allow is a decision to allow: four-eyes).
func IsWidening(newAction string, current []StageEffect) bool {
	for _, st := range current {
		if ActionRank(newAction) < ActionRank(st.Action) {
			return true
		}
		if newAction == ActionAllow && st.Source == SourceDefault {
			return true
		}
	}
	return false
}
