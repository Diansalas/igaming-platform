// Package jurisdiction is the platform-core jurisdiction RESOLVER
// (Stage 4I, docs/governance/stage-4i-canonical-model.md - "the
// canonical model"). Its interface contract is architect-owned (the
// canonical model document); this implementation is backend-owned; its
// eventual source-precedence RULESET CONTENT is identity-compliance-
// owned (canonical-model §2.1). It is platform core - not owned by
// internal/risk, internal/casino, internal/bonus, or any other
// consuming domain.
//
// LABELLED STATUS (CLAUDE.md's no-fake-completion rule):
// PARTIALLY IMPLEMENTED. Resolve can genuinely produce exactly ONE
// basis - tenant_licence, and only for a tenant/brand-subject operation
// (never a player-scoped one) - and otherwise honestly returns
// unresolved(no_signal). This is the disclosed, correct Stage 4I outcome
// (canonical-model §11.3), not a partial implementation of "resolve a
// player's jurisdiction" - that capability does not exist anywhere in
// this codebase yet and is gated on HDR-J-3 (a human decision this
// package does not make and must never work around).
package jurisdiction

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PolicyVersion identifies the resolver LOGIC (and, once real precedence
// rows exist, the precedence configuration) that produced a Resolution
// (canonical-model §5.2's resolver_policy_version - "without it a
// decision made under an older precedence rule is not reproducible").
// Bump this whenever Resolve's own logic changes in a way that could
// change the answer for identical inputs.
const PolicyVersion = "stage-4i.v1"

// --- Errors ---

// ErrInvalidInput covers caller-side mistakes - mirrors risk.
// ErrInvalidInput/assetregistry.ErrInvalidInput's package-local sentinel
// convention.
var ErrInvalidInput = errors.New("jurisdiction: invalid input")

// ErrNotResolved is returned by Code()/ID() for any outcome other than
// Resolved. There is deliberately no other way to obtain a jurisdiction
// code or id from a Resolution (canonical-model §2.3 property 2) - this
// is what structurally prevents RECON C-3's "absent looks like unscoped"
// defect family from recurring: no caller can obtain "" or uuid.Nil from
// a Resolution and proceed as though it had a real value.
var ErrNotResolved = errors.New("jurisdiction: Code()/ID() are unreachable for a non-resolved outcome")

// ErrScopeMismatch is returned by Resolution.AssertScope when the
// resolution's own tenant/brand/player binding does not match the
// caller's authenticated context (canonical-model §4.4 Layer 2). A
// resolution produced for player X is structurally unusable in an
// operation for player Y; a resolution scoped to brand A is refused,
// never silently widened, for a brand-B operation (§1.1, RULING
// BI-4I-1).
var ErrScopeMismatch = errors.New("jurisdiction: resolution scope does not match the caller's authenticated context")

// --- Outcome: the ONLY axis any gate branches on (canonical-model §2.2 axis 1) ---

// Outcome is a closed, 3-valued enum. A gate that branches on more than
// these three values will eventually branch on one of them wrongly, and
// the wrong branch will be an ALLOW - see the canonical model's own
// argument for why this is deliberately not a richer enum.
type Outcome string

const (
	Resolved   Outcome = "resolved"
	Unresolved Outcome = "unresolved"
	Refused    Outcome = "refused"
)

// --- Reason: diagnostic only, recorded always, branched on by no gate (axis 2) ---

type Reason string

const (
	ReasonDetermined                Reason = "determined"
	ReasonInsufficientConfidence    Reason = "insufficient_confidence"
	ReasonNoSignal                  Reason = "no_signal"
	ReasonIrreconcilableBases       Reason = "irreconcilable_bases"
	ReasonDependencyUnavailable     Reason = "dependency_unavailable"
	ReasonUnsupportedOperationClass Reason = "unsupported_operation_class"
	ReasonScopeMismatch             Reason = "scope_mismatch"
	ReasonRegistryUnknownCode       Reason = "registry_unknown_code"
)

// --- Basis: the canonical, CLOSED source-basis enum (canonical-model §3.1) ---
//
// Adding a value is an ADR, not an implementation detail.

type Basis string

const (
	BasisPlayerVerifiedResidence Basis = "player_verified_residence"
	BasisPlayerDeclaredResidence Basis = "player_declared_residence"
	BasisKYCCorroboration        Basis = "kyc_corroboration"
	BasisGeoSignal               Basis = "geo_signal"
	BasisRetailNode              Basis = "retail_node"
	BasisTenantLicence           Basis = "tenant_licence"
	BasisTenantAsserted          Basis = "tenant_asserted"
	// BasisPlatformFallback is reserved so a future HDR-J-1 "yes" answer
	// adds a producer rather than a schema migration (§3.1). Resolve is
	// structurally incapable of emitting it: no code path in resolver.go
	// assigns this value to a Resolution's selectedBasis, and there is no
	// exported function anywhere in this package that accepts a basis
	// from a caller and returns a Resolution carrying it.
	BasisPlatformFallback Basis = "platform_fallback"
	// BasisStaffSupplied exists ONLY for SEC-4I-F2's interim audit label
	// (canonical-model §8.4). It is a value in THIS enum so a later
	// reader can distinguish records written before JV-2 lands (which
	// carry this label, drawn from a real request field) from records
	// written after (which never do), without a second, free-standing
	// vocabulary. Resolve NEVER produces it - the only sanctioned use is
	// an audit metadata literal at the ONE interim call site named in
	// canonical-model §8.4, never a Resolution value.
	BasisStaffSupplied Basis = "staff_supplied"
)

// --- ConsideredBasisStatus: per-basis STATUS only, never the value (§5.3 item 1) ---

type ConsideredBasisStatus string

const (
	StatusSelected                ConsideredBasisStatus = "selected"
	StatusRejectedLowerPrecedence ConsideredBasisStatus = "rejected_lower_precedence"
	StatusUnavailable             ConsideredBasisStatus = "unavailable"
	StatusDisagreed               ConsideredBasisStatus = "disagreed"
)

// ConsideredBasis records that a basis was considered and what happened
// to it - NEVER the evidentiary value it held (canonical-model §5.3's
// governing principle: "a resolution record persists the DECISION and
// REFERENCES to its evidence, never the evidence VALUES").
type ConsideredBasis struct {
	Basis  Basis                 `json:"basis"`
	Status ConsideredBasisStatus `json:"status"`
}

// --- ConfidenceClass: a bucket, never a raw score (§5.2) ---

type ConfidenceClass string

const (
	// ConfidenceAuthoritative is the bucket for a licensing/registry fact
	// with no player-evidence dimension at all (tenant_licence).
	ConfidenceAuthoritative ConfidenceClass = "authoritative"
	ConfidenceDeclared      ConfidenceClass = "declared"
	ConfidenceCorroborated  ConfidenceClass = "corroborated"
	ConfidenceVerified      ConfidenceClass = "verified"
)

// --- OperationClass: compile-time constant at each call site, never a wire string (§4.4 Layer 1) ---

type OperationClass string

const (
	OperationPlay                  OperationClass = "play"
	OperationCatalogueAvailability OperationClass = "catalogue_availability"
	OperationBonusIssuance         OperationClass = "bonus_issuance"
	OperationBonusConversion       OperationClass = "bonus_conversion"
)

func validOperationClass(oc OperationClass) bool {
	switch oc {
	case OperationPlay, OperationCatalogueAvailability, OperationBonusIssuance, OperationBonusConversion:
		return true
	default:
		return false
	}
}

// --- ActorType: matches audit_log's own CHECK vocabulary (migration 0014) ---

type ActorType string

const (
	ActorPlayer  ActorType = "player"
	ActorStaff   ActorType = "staff"
	ActorService ActorType = "service"
	ActorSystem  ActorType = "system"
)

func validActorType(a ActorType) bool {
	switch a {
	case ActorPlayer, ActorStaff, ActorService, ActorSystem:
		return true
	default:
		return false
	}
}

// --- ReadOnlyQuerier: Resolve's only allowed handle (canonical-model §6.4) ---

// ReadOnlyQuerier exposes only Query/QueryRow - deliberately NOT the full
// pgx.Tx interface, and in particular NOT Exec. Resolve accepts this
// narrow type rather than a pgx.Tx so that a write added inside Resolve's
// own body DOES NOT COMPILE - the single compile-time guarantee RISK H-2
// requires (canonical-model §6.4): "the jurisdiction resolver is
// READ-ONLY on the evaluation path... enforced structurally, not by
// comment." Any pgx.Tx or *pgxpool.Pool satisfies this interface
// structurally, so a caller MAY pass a writable handle - what this type
// prevents is THIS PACKAGE's own code from ever calling Exec, not a
// caller's ability to reuse an existing connection.
type ReadOnlyQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// --- Params: Resolve's input ---

// Params is Resolve's input. Every field must be resolved SERVER-SIDE
// from authenticated context or a compile-time constant - never from a
// request body, header, query, or path segment (canonical-model §4.4
// Layer 1, JV-1). Resolve does not itself enforce this (it cannot - it
// has no visibility into how its caller obtained these values); the
// enforcement is Layer 1's "no parse path" discipline at every call site,
// which is `qa`'s later-phase adversarial contract (canonical-model
// §6.2 scenarios 4-6).
type Params struct {
	TenantID uuid.UUID
	// BrandID is nil where an operation is not brand-scoped (ADR 0037
	// §C.2's own precedent, mirrored here).
	BrandID *uuid.UUID
	// PlayerAccountID is nil for a tenant/brand-subject operation (e.g.
	// "which jurisdiction governs this tenant's catalogue availability
	// question") and non-nil for a player-scoped operation. THIS is the
	// field that determines whether BasisTenantLicence may ever be
	// selected (canonical-model §3.2) - never a separate "subject kind"
	// flag a caller could set inconsistently.
	PlayerAccountID *uuid.UUID
	OperationClass  OperationClass

	// RequestedByActorType/RequestedByActorID are recorded on the
	// persisted resolution (Persist) exactly as audit_log records them -
	// they are NEVER branched on by Resolve itself.
	RequestedByActorType ActorType
	RequestedByActorID   *uuid.UUID
}

// --- Resolution: non-forgeable by construction (canonical-model §2.3) ---

// Resolution is produced ONLY by Resolve (and re-produced, with a
// RecordID, only by Persist). Its fields are unexported, so a struct
// literal of this type does not compile outside this package. There is
// deliberately no exported constructor, no setter, and no function
// anywhere in this package that accepts a jurisdiction code from a caller
// and returns a Resolution carrying it.
type Resolution struct {
	recordID uuid.UUID
	outcome  Outcome
	reason   Reason
	code     string
	id       uuid.UUID
	asOf     time.Time

	policyVersion       string
	registryVersion     string
	configEffectiveFrom *time.Time

	selectedBasis   Basis
	consideredBases []ConsideredBasis
	confidenceClass ConfidenceClass

	// Binding scope - re-asserted by every consuming gate (§4.4 Layer 2).
	tenantID       uuid.UUID
	brandID        *uuid.UUID
	playerAcctID   *uuid.UUID
	operationClass OperationClass

	requestedByActorType ActorType
	requestedByActorID   *uuid.UUID
}

func (r Resolution) Outcome() Outcome                 { return r.outcome }
func (r Resolution) Reason() Reason                   { return r.reason }
func (r Resolution) RecordID() uuid.UUID              { return r.recordID }
func (r Resolution) AsOf() time.Time                  { return r.asOf }
func (r Resolution) PolicyVersion() string            { return r.policyVersion }
func (r Resolution) TenantID() uuid.UUID              { return r.tenantID }
func (r Resolution) BrandID() *uuid.UUID              { return r.brandID }
func (r Resolution) PlayerAccountID() *uuid.UUID      { return r.playerAcctID }
func (r Resolution) OperationClass() OperationClass   { return r.operationClass }
func (r Resolution) SelectedBasis() Basis             { return r.selectedBasis }
func (r Resolution) ConfidenceClass() ConfidenceClass { return r.confidenceClass }

// Code returns the resolved jurisdiction's code. Unreachable (returns
// ErrNotResolved) for any outcome other than Resolved - see ErrNotResolved's
// own doc comment for why this is the single most important property
// this type has.
func (r Resolution) Code() (string, error) {
	if r.outcome != Resolved {
		return "", fmt.Errorf("%w: outcome is %q", ErrNotResolved, r.outcome)
	}
	return r.code, nil
}

// ID mirrors Code() for jurisdictions.id - the one internal
// representation a consumer (AssetAuthorization layer 6) needs
// (canonical-model §2.4). Both Code() and ID() come from the SAME
// resolution; no domain performs its own code->id translation.
func (r Resolution) ID() (uuid.UUID, error) {
	if r.outcome != Resolved {
		return uuid.Nil, fmt.Errorf("%w: outcome is %q", ErrNotResolved, r.outcome)
	}
	return r.id, nil
}

// AssertScope re-asserts this resolution's tenant/brand/player binding
// against the caller's OWN authenticated context (canonical-model §4.4
// Layer 2). Every consuming gate must call this - and refuse the
// operation on a non-nil return - before using Code()/ID(). A resolution
// for player X is structurally unusable in an operation for player Y;
// per RULING BI-4I-1 (§1.1), a resolution scoped to one brand is refused,
// never silently widened, for a different brand within the SAME tenant.
// A resolution with no brand (BrandID() == nil) is a tenant-level fact
// and may be used by any brand within that tenant.
func (r Resolution) AssertScope(tenantID uuid.UUID, brandID, playerAccountID *uuid.UUID) error {
	if r.tenantID != tenantID {
		return fmt.Errorf("%w: resolution tenant %s does not match caller tenant %s", ErrScopeMismatch, r.tenantID, tenantID)
	}
	if !uuidPtrEqual(r.playerAcctID, playerAccountID) {
		return fmt.Errorf("%w: resolution player account does not match the caller's", ErrScopeMismatch)
	}
	if r.brandID != nil && !uuidPtrEqual(r.brandID, brandID) {
		return fmt.Errorf("%w: resolution brand does not match the caller's", ErrScopeMismatch)
	}
	return nil
}

func uuidPtrEqual(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
