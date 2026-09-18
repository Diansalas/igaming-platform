// Package economicop implements the storage layer for
// EconomicOperationIdentity (docs/architecture/34-economic-operation-
// identity.md, "doc 34") - a cross-cutting authorization-lineage
// mechanism. Bonus Engine is its first consumer (docs/architecture/10-
// bonus-engine-architecture.md N2.4a), but doc 34 §4.5 names it a shared
// primitive CRM and Affiliate are also expected to mint operation_type
// values under, which is why it is its own package and its own
// migration (0053), never Bonus's to own exclusively.
//
// Doc 34 §2.1's one binding sentence: "An authorization mints exactly
// one EconomicOperationIdentity. Every execution it causes - directly or
// transitively, first attempt or thousandth - inherits that identity as
// its parent_operation_id. No executor ever mints."
//
// STATUS: schema and plumbing only (Stage 4H-B1 Wave 2 Phase 2). This
// package provides types mirroring the economic_operations row and a
// thin CRUD repository layer - Create/Get/List, plus the two narrow
// lookups (by idempotency key, by root) every consumer needs. It
// deliberately implements NONE of doc 34 §5's enforcement: no fail-closed
// entry check (§5.1), no locking budget-consumption function (§5.4), no
// canonical lock ordering against a domain's own gate chain (§5.3), and
// no consumption-record-shape declaration for any operation_type (§3.4).
// That is Phase 3 / the architect's later integration work, built on
// this schema, per this dispatch's own scope.
package economicop

import (
	"math/big"
	"time"

	"github.com/google/uuid"
)

// OperationType is doc 34 §3.1's closed, compiled-in list of who may
// mint an EconomicOperationIdentity. Not operator-extensible; a new
// value requires the same additive-extension discipline ADR 0031 §12
// established for internal/risk.Operation, and per doc 34 §3.1's own
// binding rule this vocabulary is NEVER derived from, aliased to, or
// kept in lockstep with internal/risk.Operation - the two enums classify
// different things and must stay independently owned.
type OperationType string

const (
	OperationBonusBulkGrant                  OperationType = "bonus_bulk_grant"
	OperationBonusManualGrant                OperationType = "bonus_manual_grant"
	OperationBonusCampaignActivation         OperationType = "bonus_campaign_activation"
	OperationBonusHeldDispositionResolution  OperationType = "bonus_held_disposition_resolution"
	OperationCRMEngagementCampaignActivation OperationType = "crm_engagement_campaign_activation"
	OperationAffiliateCommissionSettlement   OperationType = "affiliate_commission_settlement"
	OperationAffiliateReattribution          OperationType = "affiliate_reattribution"
	OperationManualBalanceAdjustment         OperationType = "manual_balance_adjustment"
	OperationAPIInitiatedGrant               OperationType = "api_initiated_grant"
)

// SubjectScope is doc 34 §2.2's subject_scope field.
type SubjectScope string

const (
	SubjectScopeSingle          SubjectScope = "single_subject"
	SubjectScopeEnumeratedSet   SubjectScope = "enumerated_set"
	SubjectScopeCriteriaDefined SubjectScope = "criteria_defined"
	SubjectScopeNone            SubjectScope = "none"
)

// BeneficiaryClass is doc 34 §2.2's beneficiary_class field.
type BeneficiaryClass string

const (
	BeneficiaryClassPlayer    BeneficiaryClass = "player"
	BeneficiaryClassAffiliate BeneficiaryClass = "affiliate"
	BeneficiaryClassStaff     BeneficiaryClass = "staff"
	BeneficiaryClassPlatform  BeneficiaryClass = "platform"
)

// LineageKind is doc 34 §3.2's four ways a child comes to exist, plus
// 'root' for a freshly-minted authorization.
type LineageKind string

const (
	LineageRoot         LineageKind = "root"
	LineageRetry        LineageKind = "retry"
	LineageResume       LineageKind = "resume"
	LineagePage         LineageKind = "page"
	LineageItem         LineageKind = "item"
	LineageCompensation LineageKind = "compensation"
)

// ApprovalState is doc 34 §2.2's approval_state field. 'consumed' is
// reserved exclusively for single-consumption operation_types (§3.1's
// Consumption shape column) - a standing-authorization type never writes
// it, no matter how many effecting writes occur under it.
type ApprovalState string

const (
	ApprovalNotRequired ApprovalState = "not_required"
	ApprovalPending     ApprovalState = "pending"
	ApprovalApproved    ApprovalState = "approved"
	ApprovalRejected    ApprovalState = "rejected"
	ApprovalExpired     ApprovalState = "expired"
	ApprovalConsumed    ApprovalState = "consumed"
	ApprovalRevoked     ApprovalState = "revoked"
)

// Status is doc 34 §2.2's lifecycle status field.
type Status string

const (
	StatusOpen       Status = "open"
	StatusExhausted  Status = "exhausted"
	StatusCompleted  Status = "completed"
	StatusAborted    Status = "aborted"
	StatusSuperseded Status = "superseded"
)

// EconomicOperation mirrors one economic_operations row (doc 34 §2.2).
// Every monetary field is *big.Int (NUMERIC(38,0)), never int64/float64
// (CLAUDE.md; doc 34 §2.2's own explicit instruction).
type EconomicOperation struct {
	OperationID uuid.UUID
	TenantID    uuid.UUID
	BrandID     *uuid.UUID

	OperationType OperationType

	InitiatingActorType   string // internal/audit.ActorType verbatim (doc 10 N2.3)
	InitiatingActorID     uuid.UUID
	InitiatingPrincipalID *uuid.UUID

	SubjectScope          SubjectScope
	SubjectRef            *uuid.UUID
	SubjectSetHash        *string
	SubjectDefinitionHash *string
	SubjectSetCount       *int32
	BeneficiaryClass      *BeneficiaryClass

	EconomicOwner string // "tenant" | "platform" | "provider:<id>" | "affiliate:<node_id>"

	AssetCode              *string
	IntendedAggregateValue *big.Int // NUMERIC(38,0), nil iff not asserted
	RecipientCeiling       *int32
	PerWindowCeiling       *int32
	CeilingWindow          *time.Duration
	ValueMeasureBasis      []byte // JSONB, opaque to this package

	ParentOperationID *uuid.UUID
	RootOperationID   uuid.UUID
	LineageKind       LineageKind
	BatchOrdinal      *int32
	BatchTotal        *int32

	ApprovalState               ApprovalState
	RequiredApprovals           int32
	ApprovalsReceived           int32
	ThresholdAtDecision         *big.Int
	RequiredApprovalsAtDecision *int32
	ApprovalRefs                []uuid.UUID
	PinnedPayload               []byte // JSONB, opaque to this package

	IdempotencyKey string
	CorrelationID  uuid.UUID
	AuditRecordID  *uuid.UUID
	CreatedAt      time.Time
	CreatedBy      *uuid.UUID

	Status    Status
	ExpiresAt time.Time
}
