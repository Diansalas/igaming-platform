package launchgov

import (
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// LicensingRecord is the RECORDED basis of a launch decision (ADR 0112 4.1/5.2 H-2). It is
// separate from tenants.licensing_model (which keeps driving licence binding and credential
// ownership). Recording a basis is never judging it: every model is always available.
type LicensingRecord struct {
	Model  string // LicModel*
	Status string // LicStatus*
	// LicenceID is required for platform_licence and tenant_licence, optional for
	// other_manually_approved, and must be nil for not_applicable_recorded_determination.
	LicenceID *uuid.UUID
	// DeterminationReference is an external document reference (never the document);
	// DeterminationByRole the responsible function ("external counsel"), never a person's name.
	DeterminationReference string
	DeterminationByRole    string
}

// RequestInput is everything a requester supplies. Every actor, derived, forced and hash column
// is the database's: there is no field for any of them.
type RequestInput struct {
	Subject    Subject
	Action     string // activate | reactivate | close | ratify (suspension: Suspend)
	ReasonCode string

	// Required for activate / reactivate / ratify; must be empty for close.
	Licensing                       *LicensingRecord
	ResponsibleOperatorName         string
	ResponsibleOperatorRegistration string
	JurisdictionIDs                 []uuid.UUID

	// ConditionsNote is a bounded record (<= 2000 bytes, PII-linted). Nothing in it is machine
	// enforced; the console says so.
	ConditionsNote string
	// AcknowledgedCheckCodes must equal exactly the set of readiness checks whose result is not
	// pass (H-5): an explicit human acknowledgement, no silent regression.
	AcknowledgedCheckCodes []string
}

// Request is one launch_authorisation_requests row.
type Request struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	SubjectKind string
	BrandID     *uuid.UUID
	Action      string
	FromStatus  string
	ToStatus    string

	LicensingModel                  *string
	LicensingStatus                 *string
	LicenceID                       *uuid.UUID
	DeterminationReference          *string
	DeterminationByRole             *string
	ResponsibleOperatorName         *string
	ResponsibleOperatorRegistration string
	JurisdictionIDs                 []uuid.UUID
	ConditionsNote                  string

	ReadinessSnapshotHash  string
	AcknowledgedCheckCodes []string
	ReasonCode             string

	RequestedBy         uuid.UUID
	RequestedByScope    string
	RequestedByPersonID uuid.UUID
	RequiredApprovals   int
	PayloadHash         string

	Status      string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	DecidedAt   *time.Time
	RefusalCode *string
	ExecutingTx *int64

	// PriorRejects is filled by GetRequest only (never persisted): every reject decision of a
	// request of the same subject and action within the request TTL, whatever its payload, so
	// an approver sees that a variant of this request was already turned down (security C2).
	PriorRejects []PriorReject
}

// PriorReject is a reject decision on an earlier request of the same subject and action within
// the request TTL. Codes and ids only.
type PriorReject struct {
	RequestID         uuid.UUID
	DecidedAt         time.Time
	DecidedBy         uuid.UUID
	DecidedByPersonID uuid.UUID
	ReasonCode        string
	SamePayload       bool // the rejected request had this request's payload_hash
}

// Subject returns the request's subject.
func (r Request) Subject() Subject { return Subject{TenantID: r.TenantID, BrandID: r.BrandID} }

const requestColumns = `id, tenant_id, subject_kind, brand_id, action, from_status, to_status,
	licensing_model, licensing_status, licence_id, determination_reference, determination_by_role,
	responsible_operator_name, responsible_operator_registration, jurisdiction_ids, conditions_note,
	readiness_snapshot_hash, acknowledged_check_codes, reason_code,
	requested_by, requested_by_scope, requested_by_person_id, required_approvals, payload_hash,
	status, created_at, expires_at, decided_at, refusal_code, executing_txid`

func scanRequest(row pgx.Row, r *Request) error {
	return row.Scan(&r.ID, &r.TenantID, &r.SubjectKind, &r.BrandID, &r.Action, &r.FromStatus, &r.ToStatus,
		&r.LicensingModel, &r.LicensingStatus, &r.LicenceID, &r.DeterminationReference, &r.DeterminationByRole,
		&r.ResponsibleOperatorName, &r.ResponsibleOperatorRegistration, &r.JurisdictionIDs, &r.ConditionsNote,
		&r.ReadinessSnapshotHash, &r.AcknowledgedCheckCodes, &r.ReasonCode,
		&r.RequestedBy, &r.RequestedByScope, &r.RequestedByPersonID, &r.RequiredApprovals, &r.PayloadHash,
		&r.Status, &r.CreatedAt, &r.ExpiresAt, &r.DecidedAt, &r.RefusalCode, &r.ExecutingTx)
}

// Approval is one launch_authorisation_approvals row.
type Approval struct {
	ID                 uuid.UUID
	RequestID          uuid.UUID
	Decision           string
	DecidedBy          uuid.UUID
	DecidedByPersonID  uuid.UUID
	PayloadHash        string
	ReadinessHashAtDec string
	ReasonCode         string
	DecidedAt          time.Time
	DecidedTxID        int64
}

const approvalColumns = `id, request_id, decision, decided_by, decided_by_person_id, payload_hash,
	readiness_snapshot_hash_at_decision, reason_code, decided_at, decided_txid`

func scanApproval(row pgx.Row, a *Approval) error {
	return row.Scan(&a.ID, &a.RequestID, &a.Decision, &a.DecidedBy, &a.DecidedByPersonID, &a.PayloadHash,
		&a.ReadinessHashAtDec, &a.ReasonCode, &a.DecidedAt, &a.DecidedTxID)
}

// RequestResult is returned by Request and Suspend. Readiness is the evaluation the server bound.
type RequestResult struct {
	Request   Request
	Readiness Readiness
	// Executed is true for a suspension (requested and executed in one transaction).
	Executed bool
	// Superseded lists the pending requests a suspension superseded.
	Superseded []uuid.UUID
}

// DecisionResult is returned by Decide.
type DecisionResult struct {
	Request  Request
	Decision string
	// Executed is true when this approval was the final one and the status change committed.
	Executed bool
	// Refused is non-empty when the final approval reached execution and the executor refused it
	// (refused_at_execution; the code names the failed precondition). The approval is recorded.
	Refused   string
	Readiness Readiness
	// PriorRejects: see Request.PriorRejects (shown to the approver at decision time).
	PriorRejects []PriorReject
}

// RefusedAtExecutionError is returned by Decide when the request reached execution and the
// executor refused it. The refusal is durable (request status refused_at_execution).
type RefusedAtExecutionError struct {
	RequestID uuid.UUID
	Code      string
}

func (e *RefusedAtExecutionError) Error() string {
	return "launchgov: request " + e.RequestID.String() + " refused at execution: " + e.Code
}
