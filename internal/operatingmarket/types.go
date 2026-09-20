// Package operatingmarket is the platform-core OPERATING-MARKET /
// COUNTRY-POLICY mechanism (Stage 4I Phase E, architect design ruling
// "Stage 4I Phase E: Operating Market & Country Policy Foundation";
// recorded in full at docs/decisions/0045-operating-market-and-country-
// policy-foundation.md). It answers exactly one question: "for this
// tenant/brand, this operation (and optionally this product), this
// country, at this AsOf - is this platform actually permitted to operate,
// given both the licence's ceiling and every narrower policy decision
// beneath it?"
//
// It does NOT answer "which jurisdiction governs this player" - that is
// internal/jurisdiction's own, structurally separate question. This
// package is a NEW package, not an addition to internal/jurisdiction, so
// that separation is a compile-time fact rather than a review-time
// promise: a caller that imports operatingmarket cannot accidentally
// reach PlayerJurisdictionResult, EvidenceSet, or Resolution - they are
// not in scope. INV-M-1 (mechanically enforced by
// TestOperatingMarket_ImportGraphInvariant via `go list -deps`): this
// package may import internal/jurisdiction (for EvaluateLicenceValidity
// only), internal/validation, and internal/audit; it must NEVER import
// internal/identity, internal/kyc, internal/geolocation, or internal/rg.
// internal/jurisdiction must never import this package, now or later.
//
// LABELLED STATUS (CLAUDE.md's no-fake-completion rule): IMPLEMENTED as a
// MECHANISM ONLY. No HTTP route, no OpenAPI change, and no production
// caller exist anywhere in this codebase - ResolveOperatingCountryPolicy
// and IsRegistrationPermitted are wired into no consuming domain
// (casino/bonus/risk/payments/sportsbook/registration/withdrawal). Every
// table this package reads/writes holds ZERO country/market content as of
// this phase (only the four seeded platform_operations vocabulary rows
// exist). Dual control on enabling a country is NOT built in this phase -
// see docs/governance/task-registry.md item MKT-DUAL-1 - the fail-closed
// control is the absence of any production-reachable write path at all,
// plus the NOT-NULL-on-enable authorization_reference CHECK constraint.
package operatingmarket

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PolicyVersion identifies the resolution ALGORITHM version that produced
// a Result (mirrors jurisdiction.PolicyVersion's own rationale - "without
// it a decision made under an older logic version is not reproducible").
// Stamped on every row and every Result from this compiled-in constant,
// never from a caller. Bump whenever resolve()'s own logic changes in a
// way that could change the answer for identical inputs.
//
// v1 -> v2: AMENDMENT-1 (§16), STEP 4's operation-rung rewrite.
// v2 -> v3: ADR 0045 §18, finding F4 ONLY (jurisdiction.EvaluateLicenceValidity
// gained the issued_at/"not yet issued" check, which resolve() surfaces as a
// new Outcome/LicenceCeilingReason for an identical row set that previously
// resolved differently). AMENDMENT-3 (§18, SEC-E-REV-2, the bare-close fix)
// does NOT bump this constant: it adds a write-time/commit-time constraint
// on which row sets the sanctioned write path can construct, and changes
// neither resolve() nor operating_country_policies_enforce_ceiling()'s
// executable body - resolve() computes the identical function of a fixed
// stored row set before and after AMENDMENT-3, exactly as AMENDMENT-2 did
// not bump v2 -> v3 for the identical reason (§17).
const PolicyVersion = "stage-4i-e.v3"

// --- Errors ---

var (
	ErrInvalidInput = errors.New("operatingmarket: invalid input")
	// ErrNotFound is RESERVED: no function in this package currently
	// returns it (flagged in the Phase E fix round's P3 review). Left
	// declared rather than removed - a future read-by-id admin surface
	// (e.g. reading one specific CeilingRecord/PolicyRecord version by its
	// id, as opposed to today's list-by-key surfaces) is the obvious,
	// likely-imminent caller, and removing a currently-inert exported
	// sentinel is itself a public-API change some future caller could
	// already be depending on having reserved.
	ErrNotFound = errors.New("operatingmarket: not found")
	// ErrTransactionScope is a SERVER-side caller bug, not a client input
	// error - deliberately NOT wrapping ErrInvalidInput (PHASE-B-ARCH-1-
	// SEC-5's precedent) so the two failure classes can be alerted on
	// differently.
	ErrTransactionScope      = errors.New("operatingmarket: wrong transaction scope")
	ErrConcurrentPolicyWrite = errors.New("operatingmarket: a concurrent policy write for this key was detected; nothing was written")
	// ErrCeilingExceeded is returned when a write would exceed the licence
	// ceiling, a higher scope's policy (tenant/brand absence-or-disable
	// above the row being written), OR - per ADR 0045 §3.5-A AMENDMENT-1 -
	// a broader in-force ACTIVE DISABLED operation policy at the SAME
	// scope (operating_country_policies_enforce_ceiling's step (4)): a
	// more-specific operation/product row may only narrow, never widen
	// past a broader in-force disable at its own rung.
	ErrCeilingExceeded = errors.New("operatingmarket: the requested policy would exceed the licence ceiling, a higher scope's policy, or a broader in-force policy at the same scope")
	// ErrPolicyCloseRequiresSuccessor reports that a transaction closed an
	// in-force active+disabled brand- or operation-scope policy WITHOUT
	// inserting an open successor version at the same key (ADR 0045 §3.5-A
	// AMENDMENT-3, SEC-E-REV-2). Raised by the DEFERRED constraint trigger
	// ocp_inherit_rung_close_requires_successor at COMMIT, not at statement
	// time - so it surfaces from the caller's tx.Commit(), never from a call
	// in this package. Interpret commit errors with ClassifyCommitError.
	ErrPolicyCloseRequiresSuccessor = errors.New("operatingmarket: closing an in-force disabled policy at an inheriting rung requires an open successor version in the same transaction")
	// ErrLicenceNotDeterminable is RESERVED: resolve() classifies every
	// licence-determinability failure it can reach today via the eleven-
	// valued Outcome (licensing_unknown) rather than this sentinel, since
	// resolve() must never return a bare Go error for a data condition -
	// only for a genuine caller/environment bug. This sentinel is reserved
	// for a future ADMIN-SURFACE helper that needs to fail hard (a Go
	// error, not an Outcome) when asked to act on a tenant whose licence
	// cannot be determined at all - no such helper exists yet.
	ErrLicenceNotDeterminable = errors.New("operatingmarket: this tenant's licence is not determinable")
)

// --- Scope: which RUNG a stored policy row is, or ScopeLicence for the ceiling ---

type Scope string

const (
	// ScopeLicence identifies the ceiling in diagnostics (Explanation/
	// ChainStep) - it is NEVER a stored operating_country_policies.
	// scope_kind value and is rejected by validWritableScope.
	ScopeLicence   Scope = "licence"
	ScopeTenant    Scope = "tenant"
	ScopeBrand     Scope = "brand"
	ScopeOperation Scope = "operation"
)

// validWritableScope reports whether s may be written to
// operating_country_policies.scope_kind. ScopeLicence is deliberately
// excluded - the ceiling is a different table with a different write API
// (CreateLicenceCountryCeilingVersion).
func validWritableScope(s Scope) bool {
	switch s {
	case ScopeTenant, ScopeBrand, ScopeOperation:
		return true
	default:
		return false
	}
}

func parseScope(s string) (Scope, error) {
	switch s {
	case string(ScopeTenant):
		return ScopeTenant, nil
	case string(ScopeBrand):
		return ScopeBrand, nil
	case string(ScopeOperation):
		return ScopeOperation, nil
	default:
		return "", fmt.Errorf("%w: unrecognized scope_kind %q", ErrInvalidInput, s)
	}
}

// --- State / Status: the two stored, closed enums (Phase D.1 rulings 8/9,
// strengthened by ADR 0045 INC-5's revision of `status` to two values) ---

type State string

const (
	StateEnabled  State = "enabled"
	StateDisabled State = "disabled"
)

// Every stored-enum parse uses an exhaustive switch with a REJECTING
// default, per evaluation_policy.go's parseLocationRequirement precedent:
// a raw conversion would turn an unknown DB value into a typed value that
// is neither valid nor recognised, misreporting a data defect as a caller
// bug.
func parseState(s string) (State, error) {
	switch s {
	case string(StateEnabled):
		return StateEnabled, nil
	case string(StateDisabled):
		return StateDisabled, nil
	default:
		return "", fmt.Errorf("%w: unrecognized state %q", ErrInvalidInput, s)
	}
}

type Status string

const (
	StatusActive    Status = "active"
	StatusWithdrawn Status = "withdrawn"
)

func parseStatus(s string) (Status, error) {
	switch s {
	case string(StatusActive):
		return StatusActive, nil
	case string(StatusWithdrawn):
		return StatusWithdrawn, nil
	default:
		return "", fmt.Errorf("%w: unrecognized status %q", ErrInvalidInput, s)
	}
}

// --- LicenceCeilingReason: diagnostic detail for why the ceiling denied ---

type LicenceCeilingReason string

const (
	CeilingReasonNone             LicenceCeilingReason = ""
	CeilingReasonNoCeilingRow     LicenceCeilingReason = "no_ceiling_row"
	CeilingReasonCountryDisabled  LicenceCeilingReason = "country_disabled_in_ceiling"
	CeilingReasonCeilingWithdrawn LicenceCeilingReason = "ceiling_withdrawn"
	CeilingReasonLicenceSuspended LicenceCeilingReason = "licence_suspended"
	CeilingReasonLicenceExpired   LicenceCeilingReason = "licence_expired"
	// CeilingReasonLicenceNotYetIssued (ADR 0045 §18, finding F4): the
	// licence's issued_at is in the future at AsOf.
	CeilingReasonLicenceNotYetIssued LicenceCeilingReason = "licence_not_yet_issued"
)

// --- Outcome: the eleven-valued, closed resolution outcome ---

type Outcome string

const (
	OutcomePermitted             Outcome = "permitted"
	OutcomeNotConfigured         Outcome = "not_configured"
	OutcomeDisabledByTenant      Outcome = "disabled_by_tenant"
	OutcomeDisabledByBrand       Outcome = "disabled_by_brand"
	OutcomeDisabledByOperation   Outcome = "disabled_by_operation"
	OutcomeNotPermittedByLicence Outcome = "not_permitted_by_licence"
	OutcomeLicensingUnknown      Outcome = "licensing_unknown"
	OutcomeConfigurationConflict Outcome = "configuration_conflict"
	OutcomeInvalidConfiguration  Outcome = "invalid_configuration"
	OutcomePolicyNotYetEffective Outcome = "policy_not_yet_effective"
	OutcomePolicyExpired         Outcome = "policy_expired"
)

// --- ReadOnlyQuerier: resolve()'s only allowed handle ---

// ReadOnlyQuerier exposes only Query/QueryRow - deliberately NOT the full
// pgx.Tx interface, and in particular NOT Exec, so a write added inside
// resolve()'s own body DOES NOT COMPILE (RISK H-2 / canonical-model §6.4's
// structural-enforcement requirement, mirrored here for this domain).
// Declared LOCALLY in this package with the IDENTICAL shape to
// jurisdiction.ReadOnlyQuerier - deliberately, not a shared type alias:
// coupling the two packages' read-only handle would couple the two
// domains this phase exists to separate. Any pgx.Tx or *pgxpool.Pool
// satisfies this interface structurally, and a value of this interface
// type is itself assignable wherever a jurisdiction.ReadOnlyQuerier is
// expected (their method sets are identical), which is what lets
// EvaluateLicenceValidity be called directly with a q of this type.
type ReadOnlyQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// --- ActorContext: audit-trail context on every mutation ---

// ActorContext mirrors jurisdiction.ActorContext exactly (deliberately
// duplicated, not shared, for the same package-separation reason as
// ReadOnlyQuerier).
type ActorContext struct {
	ActorID    uuid.UUID
	IPAddress  string
	UserAgent  string
	RequestID  string
	ReasonCode string
}

func (a ActorContext) validate() error {
	if a.ActorID == uuid.Nil {
		return fmt.Errorf("%w: actor id is required", ErrInvalidInput)
	}
	if strings.TrimSpace(a.ReasonCode) == "" {
		return fmt.Errorf("%w: reason_code is required on every mutating operating-market operation", ErrInvalidInput)
	}
	return nil
}

// --- Query / RegistrationQuery / HistoryQuery: read-path inputs ---

// Query is ResolveOperatingCountryPolicy's input. Every field must be
// resolved SERVER-SIDE from authenticated context or a compile-time
// constant - never from a request body, header, query, or path segment.
type Query struct {
	TenantID uuid.UUID
	BrandID  *uuid.UUID
	// CountryCode must be server-resolved, ISO-3166-1 alpha-2.
	CountryCode string
	// OperationCode must be a compile-time constant at the call site,
	// never a wire string.
	OperationCode string
	// ProductCode is nil == "not product-specific".
	ProductCode *string
	// AsOf is REQUIRED and non-zero. resolve() never calls time.Now() and
	// no SQL on this path calls now()/clock_timestamp()/CURRENT_TIMESTAMP
	// (INV-M-3).
	AsOf time.Time
}

// RegistrationQuery is IsRegistrationPermitted's input - see that
// function's own doc comment (registration.go) for the full narrow-
// projection contract it exists to enforce.
type RegistrationQuery struct {
	TenantID    uuid.UUID
	BrandID     *uuid.UUID
	CountryCode string
	AsOf        time.Time
}

// HistoryQuery is ListOperatingCountryPolicyVersions' input.
type HistoryQuery struct {
	TenantID      uuid.UUID
	Scope         Scope
	BrandID       *uuid.UUID
	OperationCode string
	ProductCode   *string
	CountryCode   string
}

// --- CeilingRecord / PolicyRecord: row projections for the admin read surface ---

type CeilingRecord struct {
	ID                     uuid.UUID
	LicenceID              uuid.UUID
	CountryCode            string
	State                  State
	Status                 Status
	AuthorizationReference string
	ReasonCode             string
	PolicyVersion          string
	EffectiveFrom          time.Time
	EffectiveTo            *time.Time
	CreatedByActorType     string
	CreatedByActorID       uuid.UUID
	CreatedAt              time.Time
}

type PolicyRecord struct {
	ID                     uuid.UUID
	TenantID               uuid.UUID
	Scope                  Scope
	BrandID                *uuid.UUID
	OperationCode          *string
	ProductCode            *string
	CountryCode            string
	State                  State
	Status                 Status
	AuthorizationReference string
	ReasonCode             string
	PolicyVersion          string
	EffectiveFrom          time.Time
	EffectiveTo            *time.Time
	CreatedByActorType     string
	CreatedByActorID       uuid.UUID
	CreatedAt              time.Time
}

// --- Result: the non-forgeable resolution result (ADR 0045 §4) ---

// Result is produced ONLY by ResolveOperatingCountryPolicy. Every field is
// unexported, so a struct literal of this type does not compile outside
// this package - there is no exported constructor, no setter, and no
// function anywhere in this package that accepts an Outcome from a caller
// and returns a Result carrying it (jurisdiction.Resolution's own device).
//
// THREE STRUCTURAL PROPERTIES, each load-bearing:
//  1. Permitted() is the only boolean, and it is DERIVED, never
//     collapsed-into: the eleven-valued Outcome() is always available, so
//     a gate cannot accidentally treat e.g. configuration_conflict as
//     permission by comparing against the wrong sentinel.
//  2. There is NO accessor for blockingScope/blockingVersionID/
//     sourceScope/licenceID/licenceCeilingReason - the oracle rule made
//     structural: a player-facing handler holding a Result CANNOT leak
//     which scope blocked, because the language will not let it read the
//     field. The only way to that information is ExplainOperatingCountryPolicy
//     (explain.go), a separate, permission-gated, staff-only call.
//  3. There is NO Code(), ID(), or any jurisdiction accessor. A Result can
//     never be mistaken for, or substituted for, a jurisdiction.Resolution
//     or a PlayerJurisdictionResult - it exposes no jurisdiction identity
//     at all.
type Result struct {
	outcome       Outcome
	asOf          time.Time
	policyVersion string

	tenantID      uuid.UUID
	brandID       *uuid.UUID
	countryCode   string
	operationCode string
	productCode   *string

	// Diagnostic provenance. Deliberately UNEXPORTED WITH NO ACCESSOR.
	licenceID             uuid.UUID
	licenceCeilingReason  LicenceCeilingReason
	sourceScope           Scope
	sourceVersionID       uuid.UUID
	sourceEffectiveFrom   time.Time
	blockingScope         Scope
	blockingVersionID     uuid.UUID
	blockingEffectiveFrom time.Time
}

func (r Result) Outcome() Outcome      { return r.outcome }
func (r Result) Permitted() bool       { return r.outcome == OutcomePermitted }
func (r Result) AsOf() time.Time       { return r.asOf }
func (r Result) PolicyVersion() string { return r.policyVersion }
func (r Result) CountryCode() string   { return r.countryCode }
func (r Result) OperationCode() string { return r.operationCode }
func (r Result) TenantID() uuid.UUID   { return r.tenantID }
func (r Result) BrandID() *uuid.UUID   { return r.brandID }

// AssertScope re-asserts this result's binding against the caller's OWN
// authenticated context, exactly as jurisdiction.Resolution.AssertScope
// does. Every consuming gate must call it and refuse on a non-nil return.
func (r Result) AssertScope(tenantID uuid.UUID, brandID *uuid.UUID) error {
	if r.tenantID != tenantID {
		return fmt.Errorf("%w: result tenant %s does not match caller tenant %s", ErrTransactionScope, r.tenantID, tenantID)
	}
	if r.brandID != nil && !uuidPtrEqual(r.brandID, brandID) {
		return fmt.Errorf("%w: result brand does not match the caller's", ErrTransactionScope)
	}
	return nil
}

func uuidPtrEqual(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
