// Package licensingchange is the GENERIC two-person approval primitive for licensing and jurisdiction configuration writes
// (MKT-DUAL-1; licensing audit gaps G-1, G-2, G-9; triage L-10, L-24, L-25; migration 0177). It reuses the ADR 0112
// (internal/launchgov) four-eyes pattern without re-inventing it: distinct staff AND distinct Person, the requester's
// Person never decides, approvals at most 24 h old at execution, a signed actor proof (ADR 0110) on every request,
// approval, reject, cancel and execution, row locks, READ COMMITTED with a bounded lock_timeout, and an audit row per event.
//
// What it governs (the closed kind registry, licensing_change_kinds; this package's kinds map mirrors it):
//
//	licence_create                       create a licences row                               always widening
//	tenant_licence_bind                  bind / rebind tenants.licence_id                    always widening
//	tenant_licence_unbind                clear tenants.licence_id                            always narrowing
//	licence_ceiling_version              append a licence_country_ceilings version           widening iff enabled + active
//	operating_policy_version             append an operating_country_policies version       widening iff enabled + active, or a
//	                                                                                         withdrawal at an inheriting rung
//	licence_status_change                set licences.status                                  widening iff toward active (0178)
//	licence_validity_dates_change        set issued_at / expires_at                           widening iff later / cleared expiry or earlier / cleared issue (0178)
//	licence_review_due_change            set review_due_at                                    widening iff postponed or cleared (0178)
//	licence_evidence_reference_change    set evidence_reference                               never widening (0178)
//	tenant_jurisdiction_config_version   set / close a tenant_jurisdiction_configs version   narrowing iff it only removes
//	                                                                                         currencies / methods, adds geo blocks,
//	                                                                                         keeps the ruleset ids, or closes
//
// The database classifies (licensing_change_classify), never the caller. A WIDENING change needs the kind's approvals (2)
// by distinct eligible Persons; a NARROWING change requested by a platform admin executes in the same transaction as its
// request (single actor with a reason code: fail-closed asymmetry, the same as a launch suspension); a narrowing change
// requested by tenant staff needs one platform approval (tenant staff may only request and read).
//
// INTERIM SAFE BEHAVIOUR (HDR-M-1 pending: which roles approve and how many): approvers are ACTIVE platform_admin staff
// accounts with a Person; widening needs 2 approvals. Both values live in the kind registry (a reviewed migration changes
// them), never in a code path.
//
// The governed tables are writable ONLY through the executor: a write by any role but the table owner needs an executed
// request of the matching kind and payload in the same transaction (LC040). The domain writers (jurisdiction.CreateLicence,
// jurisdiction.AssignTenantLicence, operatingmarket.Create*Version, jurisdiction.SetTenantJurisdictionConfigVersion) are
// unchanged and still own their validation, append-only versioning and before/after audit; this package only decides
// whether and when they run.
//
// EXTENSION HOOK (the licence-lifecycle slice): a new kind is (1) a licensing_change_kinds row, (2) a
// licensing_change_classify branch and a licensing_change_row_matches branch plus a governed-write trigger on the table it
// writes (one migration), and (3) a kindSpec registered here with register() from its own file. Until all three exist the
// kind fails closed (unknown kind in Go, LC010 / LC040 in SQL).
//
// Nothing here is a legal or regulatory authorisation: an executed change is a recorded decision by named people.
package licensingchange

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Kind is a registered change kind.
type Kind string

// The kinds of this slice (migration 0177).
const (
	KindLicenceCreate                   Kind = "licence_create"
	KindTenantLicenceBind               Kind = "tenant_licence_bind"
	KindTenantLicenceUnbind             Kind = "tenant_licence_unbind"
	KindLicenceCeilingVersion           Kind = "licence_ceiling_version"
	KindOperatingPolicyVersion          Kind = "operating_policy_version"
	KindTenantJurisdictionConfigVersion Kind = "tenant_jurisdiction_config_version"
)

// The licence lifecycle kinds (migration 0178, lic4 slice 4a). Widening (reinstate to active, extend or clear expires_at, issue
// date earlier or cleared, postponed or cleared review date) needs the kind's approvals; narrowing and the evidence reference
// execute in the requester's own transaction.
const (
	KindLicenceStatusChange            Kind = "licence_status_change"
	KindLicenceValidityDatesChange     Kind = "licence_validity_dates_change"
	KindLicenceReviewDueChange         Kind = "licence_review_due_change"
	KindLicenceEvidenceReferenceChange Kind = "licence_evidence_reference_change"
)

// Request statuses (migration 0177).
const (
	StatusRequested = "requested"
	StatusApproved  = "approved"
	StatusExecuted  = "executed"
	StatusRejected  = "rejected"
	StatusCancelled = "cancelled"
	StatusExpired   = "expired"
)

// Actor scopes.
const (
	ScopePlatform = "platform"
	ScopeTenant   = "tenant"
)

// DefaultApprovalMaxAge bounds how old an approval may be when the request executes (the database caps it at 24 h too).
const DefaultApprovalMaxAge = 24 * time.Hour

// RequestTTL is the lifetime of an open request (the database sets expires_at = created_at + 72 h).
const RequestTTL = 72 * time.Hour

// Disclaimer is carried by every 2xx response of the HTTP layer.
const Disclaimer = "A licensing change record is a decision recorded by named staff. It is not a legal or regulatory authorisation and does not determine whether any licence or jurisdiction permits operation."

var (
	// ErrForbidden: the actor may not perform this call.
	ErrForbidden = errors.New("licensingchange: forbidden")
	// ErrNotFound: unknown request, or one the actor may not see.
	ErrNotFound = errors.New("licensingchange: not found")
	// ErrInvalidInput: the input is malformed.
	ErrInvalidInput = errors.New("licensingchange: invalid input")
	// ErrNotPending: the request is no longer open (decided, executed, cancelled or expired).
	ErrNotPending = errors.New("licensingchange: the request is not open")
	// ErrSelfApproval: the requester (or an account of the requester's Person) may not decide the request.
	ErrSelfApproval = errors.New("licensingchange: the requester's Person may not decide the request")
	// ErrAlreadyDecided: this Person already approved the request.
	ErrAlreadyDecided = errors.New("licensingchange: this Person already approved the request")
	// ErrNoPerson: a staff account has no readable Person, so four-eyes cannot be evaluated (fail closed).
	ErrNoPerson = errors.New("licensingchange: a staff account has no readable Person; four-eyes cannot be evaluated")
	// ErrApprovalsStale: enough distinct approvals exist but some are older than the approval max age.
	ErrApprovalsStale = errors.New("licensingchange: approvals are older than the approval window; file a new request")
	// ErrDuplicatePending: an identical request (same kind, payload and reason) is already open.
	ErrDuplicatePending = errors.New("licensingchange: an identical request is already open")
	// ErrProofUnavailable: no actor-proof issuer is configured; governed writes fail closed.
	ErrProofUnavailable = errors.New("licensingchange: no actor-proof issuer configured (governed writes fail closed)")
	// ErrRetryableLockTimeout: a lock timeout or deadlock; retry.
	ErrRetryableLockTimeout = errors.New("licensingchange: lock timeout; retry")
	// ErrActingSession: an acting session never acts on licensing changes.
	ErrActingSession = errors.New("licensingchange: an acting session may not act on licensing changes")
	// ErrIsolationLevel: the transaction is not READ COMMITTED.
	ErrIsolationLevel = errors.New("licensingchange: governed transactions must be READ COMMITTED")
)

// GuardError is a database guard refusal (SQLSTATE class LC or AP) with no more specific sentinel.
type GuardError struct {
	Code    string
	Message string
}

func (e *GuardError) Error() string {
	return fmt.Sprintf("licensingchange: database guard %s: %s", e.Code, e.Message)
}

// GuardCode returns the SQLSTATE of a GuardError, or "".
func GuardCode(err error) string {
	var ge *GuardError
	if errors.As(err, &ge) {
		return ge.Code
	}
	return ""
}

// DomainError wraps a refusal of the domain writer (jurisdiction / operatingmarket sentinels stay reachable with errors.Is).
type DomainError struct{ Err error }

func (e *DomainError) Error() string {
	return "licensingchange: refused by the domain writer: " + e.Err.Error()
}
func (e *DomainError) Unwrap() error { return e.Err }

// Actor is the authenticated principal, built by the caller from the verified token (never from a body).
type Actor struct {
	StaffID   uuid.UUID
	Scope     string // ScopePlatform | ScopeTenant
	TenantID  uuid.UUID
	Role      string // auth.Role value
	RequestID string
	IPAddress string
	UserAgent string
}

// Request is a licensing_change_requests row (plus its approvals on Get).
type Request struct {
	ID                  uuid.UUID
	Kind                Kind
	Payload             json.RawMessage
	PayloadHash         string
	ReasonCode          string
	Note                *string
	Widening            bool
	RequiredApprovals   int
	SubjectTenantID     *uuid.UUID
	SubjectLicenceID    *uuid.UUID
	RequestedBy         uuid.UUID
	RequestedByPersonID uuid.UUID
	RequestedScope      string
	RequestedTenantID   *uuid.UUID
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

// Approval is a licensing_change_approvals row.
type Approval struct {
	ID               uuid.UUID
	RequestID        uuid.UUID
	ApproverID       uuid.UUID
	ApproverPersonID uuid.UUID
	PayloadHash      string
	ReasonCode       string
	DecidedAt        time.Time
}

// Result is the outcome of a request or an approval.
type Result struct {
	Request  Request
	Executed bool
	// Outcome is what the domain writer produced (ids of the written rows), set only when Executed.
	Outcome map[string]any
}

// RequestInput is a change request as filed.
type RequestInput struct {
	Kind       Kind
	Payload    json.RawMessage
	ReasonCode string
	Note       string
}

// ListFilter narrows List.
type ListFilter struct {
	Status string
	Kind   Kind
	Limit  int
}
