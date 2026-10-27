package withdrawal

// lic11 (migration 0182): the governed per-tenant WITHDRAWAL RESTRICTION POLICY, evaluated at the request (R), payout
// dispatch (T1p) and re-dispatch (T2 / T12) points. Analysis: docs/plans/lic11-withdrawal-restriction-analysis.md.
//
// What it governs. Only the causes the registry marks configurable (withdrawal_restriction_causes): the tenant's licence
// state (suspended, expired, not yet issued, missing). Before 0182 a withdrawal never read the licence, so "withdrawals
// continue while the licence is invalid" was a universal rule by omission. Now the effective action per cause is
//
//   - the tenant's CONFIGURED policy row (append-only, versioned, written only through the governed maker-checker of
//     internal/withdrawalpolicy), or
//   - the registry's INTERIM ENGINEERING DEFAULT, which is exactly the behaviour before 0182 (allow at every stage),
//
// and every evaluation records which one decided (source configured / default_unconfigured / none) and the tenant's
// policy set version, so a decision can be replayed against the policy in force.
//
// What it does NOT govern (unchanged, never overridable by a policy row):
//   - tenant / brand status: ADR 0095 section 44 decisions 13-18 (H-SEC-11, migrations 0131 / 0124, resolution-only
//     deferral). The H-SEC-11 gate runs BEFORE this evaluation and its refusal is unchanged.
//   - the hard restrictions: account / wallet status (WD050 / WD052), KYC and AML / sanctions holds, risk DENY / REVIEW
//     (WD051), kill switch, provider capability, payout instrument. A policy can only ADD a refusal; "allow" here means
//     "this cause does not refuse", never "skip the other gates".
//
// Actions:
//
//	stage      allow     hold_for_review                                   block
//	request    proceed   accepted: hold placed, audit says "held for review"  refused: WD056 denial audit, nothing else
//	dispatch   proceed   park (request stays approved, hold kept): WD056     park: WD056
//	redispatch proceed   escalate (T16, sticky, no resend, no release): WD056 escalate: WD056
//
// "Review" is a human acting on a parked / escalated payout through the EXISTING paths (reject releases the hold to the
// player's cash; re-submit re-evaluates; a policy change goes through the maker-checker). There is no per-withdrawal
// override: none is invented here (record, D-4).
//
// Fail closed: a read error of the policy itself, an unknown licence status that could matter, or an unknown cause is an error
// wrapping ErrGateUnavailable (retryable; nothing is written, nothing is claimed, nothing is sent). A default never refuses.
// A licence state that cannot be READ is an error only when some configured cause could refuse at this stage (lic23 N-6): with no
// policy row at all, or with every configured action at this stage "allow", the decision is allow with the licence state recorded
// as unknown, exactly as before the policy existed.
//
// Lock order (ADR 0082): plain reads only. At R, T1p, T2 and T12 it runs after the tenant status key is taken SHARED (R and T1p:
// the H-SEC-11 status gate; T2 / T12: evaluateRedispatchGate, lic17 F-3) and before the WD-RG-1 gate (L0.4) and, at T2 / T12,
// before the L1 row lock. A policy change takes the same tenant status key EXCLUSIVE before it writes (0182 guards), so a
// decision either sees the change or the change waits for the decision's transaction: the set version it reads is one
// consistent version. The recorded set version says which.

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
	"github.com/Diansalas/igaming-platform/internal/withdrawalgate"
)

// RestrictionStage is where a restriction policy is evaluated (withdrawal_restriction_effective's p_stage).
type RestrictionStage string

const (
	RestrictionStageRequest    RestrictionStage = "request"
	RestrictionStageDispatch   RestrictionStage = "dispatch"
	RestrictionStageRedispatch RestrictionStage = "redispatch"
)

// RestrictionAction is a policy action, ordered allow < hold_for_review < block.
type RestrictionAction string

const (
	RestrictionAllow         RestrictionAction = "allow"
	RestrictionHoldForReview RestrictionAction = "hold_for_review"
	RestrictionBlock         RestrictionAction = "block"
)

// Rank orders actions by strictness (allow 0, hold_for_review 1, block 2); an unknown action is -1.
func (a RestrictionAction) Rank() int {
	switch a {
	case RestrictionAllow:
		return 0
	case RestrictionHoldForReview:
		return 1
	case RestrictionBlock:
		return 2
	}
	return -1
}

// Valid reports whether a is one of the three actions.
func (a RestrictionAction) Valid() bool { return a.Rank() >= 0 }

// RestrictionCause is a registry cause (withdrawal_restriction_causes.cause).
type RestrictionCause string

// The configurable causes (licence state). The registry also lists the non-configurable ones (tenant_suspended,
// tenant_closed, brand_suspended, brand_closed, market_closed) for the read API; they are never evaluated here.
const (
	CauseLicenceSuspended    RestrictionCause = "licence_suspended"
	CauseLicenceExpired      RestrictionCause = "licence_expired"
	CauseLicenceNotYetIssued RestrictionCause = "licence_not_yet_issued"
	CauseLicenceMissing      RestrictionCause = "licence_missing"
)

// ConfigurableCauses is the closed set this package evaluates (the registry rows with configurable = true).
var ConfigurableCauses = []RestrictionCause{CauseLicenceSuspended, CauseLicenceExpired, CauseLicenceNotYetIssued, CauseLicenceMissing}

// IsConfigurableCause reports whether c is in ConfigurableCauses.
func IsConfigurableCause(c RestrictionCause) bool {
	for _, x := range ConfigurableCauses {
		if x == c {
			return true
		}
	}
	return false
}

// Provenance sources.
const (
	RestrictionSourceConfigured = "configured"
	RestrictionSourceDefault    = "default_unconfigured"
	RestrictionSourceNone       = "none"
)

// GateCodeRestriction is withdrawalgate.CodeRestriction (WD056).
const GateCodeRestriction = withdrawalgate.CodeRestriction

// RestrictionDecision is one evaluation's outcome.
type RestrictionDecision struct {
	Stage RestrictionStage
	// Causes in force (at most one today: the licence state).
	Causes []RestrictionCause
	// Action is the strictest effective action over Causes (allow when none).
	Action RestrictionAction
	// DecidingCause is the cause whose action is Action ("" when none).
	DecidingCause RestrictionCause
	// Source is RestrictionSourceConfigured / Default / None, of the deciding cause.
	Source string
	// PolicyID / PolicyVersion identify the configured row (nil / 0 for a default).
	PolicyID      *uuid.UUID
	PolicyVersion int64
	// SetVersion is the tenant's policy set version at evaluation (0 = nothing configured for the tenant).
	SetVersion int64
	// LicenceState is the licence evaluation behind the deciding cause (a jurisdiction.LicenceValidity value), or
	// LicenceStateUnknown when the licences relied on could not be evaluated (lic17 F-4 / M-2).
	LicenceState string
	// LicenceUnknown: the licences relied on could not be evaluated, so the licence state is recorded as unknown. The decision is
	// allow when the tenant has NO policy row (the interim default) or when every configured cause is allow at this stage
	// (lic23 N-6); when a configured cause could refuse, an evaluation failure is an error instead.
	LicenceUnknown bool
	// SchemaAbsent: set ONLY under the test-only pre-0182 marker (a migration test's scratch database predates 0182). The
	// decision is the pre-0182 behaviour and carries no provenance (the 0182 columns do not exist there).
	SchemaAbsent bool
}

// LicenceStateUnknown is the recorded licence state when the reliance could not be evaluated (lic17 F-4).
const LicenceStateUnknown = "unknown"

// Refuses reports whether the decision holds or blocks.
func (d RestrictionDecision) Refuses() bool { return d.Action != RestrictionAllow }

// Provenance is the recordable form (withdrawal_gate_decisions columns, audit metadata).
func (d RestrictionDecision) Provenance() *withdrawalgate.RestrictionProvenance {
	if d.SchemaAbsent {
		return nil
	}
	return &withdrawalgate.RestrictionProvenance{Source: d.Source, SetVersion: d.SetVersion, Cause: string(d.DecidingCause), Action: string(d.Action)}
}

// AuditMetadata is the closed audit payload of the evaluation (codes and versions only).
func (d RestrictionDecision) AuditMetadata() map[string]any {
	md := map[string]any{
		"restriction_stage": string(d.Stage), "restriction_action": string(d.Action),
		"restriction_policy_source": d.Source, "restriction_policy_set_version": d.SetVersion,
	}
	if d.DecidingCause != "" || d.LicenceUnknown {
		if d.DecidingCause != "" {
			md["restriction_cause"] = string(d.DecidingCause)
		}
		md["restriction_licence_state"] = d.LicenceState
	}
	if d.PolicyID != nil {
		md["restriction_policy_id"] = d.PolicyID.String()
		md["restriction_policy_version"] = d.PolicyVersion
	}
	return md
}

// assertRecordable (lic17 L-2) checks that a REFUSING decision has the WD056 shape the database enforces
// (withdrawal_gate_decisions_wd056_shape, 0182): source configured, a configurable deciding cause (the reason code), action
// hold_for_review or block. Only a configured row may refuse; a refusing DEFAULT would mean the cause registry was changed to
// a non-allow default without the review this path needs (the 0182 registry defaults are all allow). The refusal then fails
// closed and diagnosable (ErrGateUnavailable) instead of writing a malformed decision row or blocking on an unreviewed default.
// A non-refusing decision is always recordable.
func (d RestrictionDecision) assertRecordable() error {
	if !d.Refuses() {
		return nil
	}
	if d.Source != RestrictionSourceConfigured || d.DecidingCause == "" || !IsConfigurableCause(d.DecidingCause) ||
		(d.Action != RestrictionHoldForReview && d.Action != RestrictionBlock) {
		return fmt.Errorf("%w: a restriction refusal (source %q, cause %q, action %q) has no recordable WD056 shape",
			ErrGateUnavailable, d.Source, d.DecidingCause, d.Action)
	}
	return nil
}

// DispatchDenial is the WD056 gate decision a refusing restriction decision becomes at a dispatch point (Reason = the
// cause, the closed alert vocabulary; the action is in Restriction).
func (d RestrictionDecision) DispatchDenial() withdrawalgate.Decision {
	return withdrawalgate.Decision{
		Allowed: false, GateCode: GateCodeRestriction, Reason: string(d.DecidingCause),
		Message:     "the withdrawal is held under the tenant's withdrawal restriction policy",
		Restriction: d.Provenance(),
	}
}

// ErrRestrictionUnknownLicenceState: the licence evaluation returned a state this package does not map (fail closed).
var ErrRestrictionUnknownLicenceState = errors.New("withdrawal: unknown licence state for the restriction policy")

// validityCause maps a licence validity to a cause ("" when the licence is valid). unknown is true for a validity that is not a
// statement about the licence but about its readability (LicenceNotFound: the row is not visible, lic17 M-2) or a missing
// binding passed in by mistake (LicenceNotBound): the caller treats it as "could not evaluate", never as licence_missing. Any
// value outside the closed set is an error. A static test pins that every jurisdiction.LicenceValidity constant is covered.
func validityCause(v jurisdiction.LicenceValidity) (cause RestrictionCause, unknown bool, err error) {
	switch v {
	case jurisdiction.LicenceValid:
		return "", false, nil
	case jurisdiction.LicenceSuspended:
		return CauseLicenceSuspended, false, nil
	case jurisdiction.LicenceStatusExpired, jurisdiction.LicenceDateExpired:
		return CauseLicenceExpired, false, nil
	case jurisdiction.LicenceNotYetIssued:
		return CauseLicenceNotYetIssued, false, nil
	case jurisdiction.LicenceNotFound, jurisdiction.LicenceNotBound:
		return "", true, nil
	}
	return "", false, fmt.Errorf("%w: %q", ErrRestrictionUnknownLicenceState, v)
}

// effective is one cause's resolution.
type effective struct {
	state    string // the licence validity behind the cause
	cause    RestrictionCause
	action   RestrictionAction
	source   string
	policyID *uuid.UUID
	version  int64
}

// combine picks the strictest action; on a tie a CONFIGURED source wins over a default (so a configured refusal is the one
// recorded), then the first cause in the given order. No cause: allow / none.
func combine(stage RestrictionStage, setVersion int64, licenceState string, effs []effective) RestrictionDecision {
	d := RestrictionDecision{Stage: stage, Action: RestrictionAllow, Source: RestrictionSourceNone, SetVersion: setVersion, LicenceState: licenceState}
	best := -1
	for i, e := range effs {
		d.Causes = append(d.Causes, e.cause)
		if best < 0 || e.action.Rank() > effs[best].action.Rank() ||
			(e.action.Rank() == effs[best].action.Rank() && e.source == RestrictionSourceConfigured && effs[best].source != RestrictionSourceConfigured) {
			best = i
		}
	}
	if best >= 0 {
		e := effs[best]
		d.Action, d.DecidingCause, d.Source, d.PolicyID, d.PolicyVersion = e.action, e.cause, e.source, e.policyID, e.version
		if e.state != "" {
			d.LicenceState = e.state
		}
	}
	return d
}

// restrictionClock is the evaluation instant of the licence dates (UTC calendar dates, EvaluateLicenceValidity's own
// half-open semantics). A package variable only so a unit test can pin it; production never assigns it.
var restrictionClock = time.Now

// EvaluateRestriction resolves the withdrawal restriction policy of tenantID (and brandID, whose operating bases count) at stage
// inside tx (a tenant-scoped session: the policy rows, the tenant row, its own licence and the licences of its bases are visible).
// It writes nothing and takes no lock. Any error wraps ErrGateUnavailable (fail closed, retryable).
func EvaluateRestriction(ctx context.Context, tx pgx.Tx, tenantID, brandID uuid.UUID, stage RestrictionStage) (RestrictionDecision, error) {
	d, err := evaluateRestriction(ctx, tx, tenantID, brandID, stage)
	if err != nil {
		return RestrictionDecision{}, fmt.Errorf("%w: restriction policy: %w", ErrGateUnavailable, err)
	}
	return d, nil
}

// reliedLicence is one licence the tenant relies on and its evaluated validity.
type reliedLicence struct {
	id       uuid.UUID
	validity jurisdiction.LicenceValidity
}

// licenceReliance is what the tenant (and brand) relies on (lic17 F-1).
type licenceReliance struct {
	licences []reliedLicence
	hasBasis bool
	unknown  bool // something relied on could not be evaluated (M-2 / F-4)
}

// evaluateReliance reads, in a SAVEPOINT (a failing read must not abort the caller's transaction, F-4), every licence the tenant
// relies on: tenants.licence_id and the licences named by its approved, active, in-force licence bases (tenant-wide or of the
// brand), each evaluated with jurisdiction.EvaluateLicenceValidity (the single implementation, INV-M-5). An unreadable licence
// (LicensingInvalid chain, LicenceNotFound) is unknown, never licence_missing (M-2).
func evaluateReliance(ctx context.Context, tx pgx.Tx, tenantID, brandID uuid.UUID, asOf time.Time) (rel licenceReliance, err error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return licenceReliance{}, fmt.Errorf("withdrawal: open the licence reliance savepoint: %w", err)
	}
	defer func() {
		if err != nil {
			_ = sp.Rollback(ctx)
			return
		}
		err = sp.Commit(ctx)
	}()
	chain, err := jurisdiction.ResolveLicensingChain(ctx, sp, tenantID)
	if err != nil {
		return licenceReliance{}, err
	}
	var ids []uuid.UUID
	switch chain.Outcome {
	case jurisdiction.LicensingResolved:
		ids = append(ids, chain.LicenceID)
	case jurisdiction.LicensingNoLicence:
	default:
		rel.unknown = true // the tenant row or its licence is not readable in this session
	}
	bases, err := operatingmarket.TenantBasisReliance(ctx, sp, tenantID, brandID, asOf)
	if err != nil {
		return licenceReliance{}, err
	}
	rel.hasBasis = bases.HasBasis
	for _, id := range bases.Licences {
		dup := false
		for _, have := range ids {
			dup = dup || have == id
		}
		if !dup {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		v, err := jurisdiction.EvaluateLicenceValidity(ctx, sp, id, asOf)
		if err != nil {
			return licenceReliance{}, err
		}
		rel.licences = append(rel.licences, reliedLicence{id: id, validity: v})
	}
	return rel, nil
}

// reliedCauses maps the reliance to causes in the fixed ConfigurableCauses order (strictest-wins happens in combine). A tenant
// with NO licence and NO basis keeps the pre-lic17 cause licence_missing; a tenant whose only reliance is a documented
// no-licence basis contributes no licence cause. unknown is true when any relied licence could not be evaluated.
func reliedCauses(rel licenceReliance) (causes map[RestrictionCause]string, unknown bool, err error) {
	causes = map[RestrictionCause]string{}
	unknown = rel.unknown
	for _, l := range rel.licences {
		c, unk, err := validityCause(l.validity)
		if err != nil {
			return nil, false, err
		}
		if unk {
			unknown = true
			continue
		}
		if c != "" {
			if _, have := causes[c]; !have {
				causes[c] = string(l.validity)
			}
		}
	}
	if len(rel.licences) == 0 && !rel.hasBasis && !rel.unknown {
		causes[CauseLicenceMissing] = string(jurisdiction.LicenceNotBound)
	}
	return causes, unknown, nil
}

// anyConfiguredCauseRefuses reports whether any configurable cause resolves to hold_for_review or block at the stage, and the
// provenance source to record when none does ("configured" when any cause resolved from a configured row, else the default).
func anyConfiguredCauseRefuses(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, stage RestrictionStage) (source string, refuses bool, err error) {
	source = RestrictionSourceDefault
	for _, cause := range ConfigurableCauses {
		var action, src string
		if err := tx.QueryRow(ctx, `SELECT action, source FROM withdrawal_restriction_effective($1, $2, $3)`, tenantID, string(cause), string(stage)).Scan(&action, &src); err != nil {
			return "", false, fmt.Errorf("withdrawal: resolve restriction policy: %w", err)
		}
		a := RestrictionAction(action)
		if !a.Valid() || (src != RestrictionSourceConfigured && src != RestrictionSourceDefault) {
			return "", false, fmt.Errorf("withdrawal: restriction policy resolved to an unknown action %q / source %q", action, src)
		}
		if a != RestrictionAllow {
			return "", true, nil
		}
		if src == RestrictionSourceConfigured {
			source = RestrictionSourceConfigured
		}
	}
	return source, false, nil
}

func evaluateRestriction(ctx context.Context, tx pgx.Tx, tenantID, brandID uuid.UUID, stage RestrictionStage) (RestrictionDecision, error) {
	switch stage {
	case RestrictionStageRequest, RestrictionStageDispatch, RestrictionStageRedispatch:
	default:
		return RestrictionDecision{}, fmt.Errorf("%w: unknown restriction stage %q", ErrInvalidInput, stage)
	}
	if tenantID == uuid.Nil {
		return RestrictionDecision{}, fmt.Errorf("%w: tenant id is required", ErrInvalidInput)
	}
	// TEST-ONLY: a migration up / down / up test drives a scratch database whose schema predates 0182 (no policy table, no
	// functions). Only test code of this package can set the marker (the key type is unexported and the setter lives in a
	// _test.go file; TestLic17_Static_Pre0182MarkerIsTestOnly pins it). Production never probes the schema: a missing function
	// is an error (fail closed).
	if pre0182SchemaForTest(ctx) {
		return RestrictionDecision{Stage: stage, Action: RestrictionAllow, Source: RestrictionSourceNone, SchemaAbsent: true}, nil
	}
	// The set version first, through the session-bound function (0184 M-3: a mis-scoped session is an error here, never
	// "nothing configured").
	var setVersion int64
	if err := tx.QueryRow(ctx, `SELECT withdrawal_restriction_set_version($1)`, tenantID).Scan(&setVersion); err != nil {
		return RestrictionDecision{}, fmt.Errorf("withdrawal: read restriction policy set version: %w", err)
	}
	rel, relErr := evaluateReliance(ctx, tx, tenantID, brandID, restrictionClock().UTC())
	var causes map[RestrictionCause]string
	unknown := relErr != nil
	if relErr == nil {
		var mapErr error
		causes, unknown, mapErr = reliedCauses(rel)
		if mapErr != nil {
			relErr, unknown = mapErr, true
		}
	}
	if unknown {
		if setVersion > 0 {
			// A configured policy exists. lic23 N-6: the licence state is unknown, so ANY configurable cause might apply; if none of
			// them could refuse at this stage (every effective action is allow) the unknown state cannot change the outcome and is
			// recorded as such. Otherwise never guess (fail closed, retryable).
			source, refuses, rerr := anyConfiguredCauseRefuses(ctx, tx, tenantID, stage)
			if rerr != nil {
				return RestrictionDecision{}, rerr
			}
			if !refuses {
				return RestrictionDecision{Stage: stage, Action: RestrictionAllow, Source: source, SetVersion: setVersion,
					LicenceState: LicenceStateUnknown, LicenceUnknown: true}, nil
			}
			if relErr == nil {
				relErr = errors.New("withdrawal: a licence the tenant relies on could not be evaluated")
			}
			return RestrictionDecision{}, relErr
		}
		// lic17 F-4 / M-2: no policy row at all, so the answer is the interim default whatever the licence state; record that
		// the licence state was unknown instead of turning a read problem into a 503 (the behaviour before 0182 never read it).
		return RestrictionDecision{Stage: stage, Action: RestrictionAllow, Source: RestrictionSourceDefault, SetVersion: 0,
			LicenceState: LicenceStateUnknown, LicenceUnknown: true}, nil
	}
	var effs []effective
	for _, cause := range ConfigurableCauses {
		state, ok := causes[cause]
		if !ok {
			continue
		}
		e := effective{cause: cause, state: state}
		var action string
		var version *int64
		if err := tx.QueryRow(ctx, `SELECT action, source, policy_id, set_version FROM withdrawal_restriction_effective($1, $2, $3)`,
			tenantID, string(cause), string(stage)).Scan(&action, &e.source, &e.policyID, &version); err != nil {
			return RestrictionDecision{}, fmt.Errorf("withdrawal: resolve restriction policy: %w", err)
		}
		e.action = RestrictionAction(action)
		if !e.action.Valid() || (e.source != RestrictionSourceConfigured && e.source != RestrictionSourceDefault) {
			return RestrictionDecision{}, fmt.Errorf("withdrawal: restriction policy resolved to an unknown action %q / source %q", action, e.source)
		}
		if version != nil {
			e.version = *version
		}
		effs = append(effs, e)
	}
	return combine(stage, setVersion, string(jurisdiction.LicenceValid), effs), nil
}

// evaluateRequestRestriction runs the policy for a NEW request (after H-SEC-11, before the WD-RG-1 gate). A configured
// block writes the denial audit in tx and returns a *GateDeniedError with GateCodeRestriction (WD056): the HTTP handler and
// every existing caller already commit a gate denial's audit and answer the player generically. Otherwise it returns the
// decision for the request's audit.
func evaluateRequestRestriction(ctx context.Context, tx pgx.Tx, p RequestParams) (RestrictionDecision, error) {
	rd, err := EvaluateRestriction(ctx, tx, p.TenantID, p.BrandID, RestrictionStageRequest)
	if err != nil {
		return RestrictionDecision{}, err
	}
	if err := rd.assertRecordable(); err != nil {
		return RestrictionDecision{}, err
	}
	if rd.Action != RestrictionBlock {
		return rd, nil
	}
	extra := rd.AuditMetadata()
	if aerr := auditGateDenial(ctx, tx, p, "withdrawal.denied_by_restriction_policy", GateCodeRestriction, string(rd.DecidingCause), extra); aerr != nil {
		return RestrictionDecision{}, aerr
	}
	return RestrictionDecision{}, &GateDeniedError{GateCode: GateCodeRestriction, Reason: string(rd.DecidingCause),
		Message: "withdrawals are restricted for this tenant under its withdrawal restriction policy"}
}

// auditRequestRestriction records, in the request's transaction, a cause in force under a CONFIGURED policy when the
// request was ACCEPTED (allow or hold_for_review). Under the default (or with no cause) nothing extra is written: the
// default path writes exactly the rows it wrote before 0182, and the request's own withdrawal.requested audit row carries
// the provenance (source, set version, cause and licence state) either way.
func auditRequestRestriction(ctx context.Context, tx pgx.Tx, p RequestParams, requestID uuid.UUID, rd RestrictionDecision) error {
	if rd.DecidingCause == "" || rd.Source != RestrictionSourceConfigured {
		return nil
	}
	action := "withdrawal.restriction_cause_in_force"
	if rd.Action == RestrictionHoldForReview {
		action = "withdrawal.held_for_restriction_review"
	}
	md := rd.AuditMetadata()
	md["withdrawal_request_id"] = requestID.String()
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: p.TenantID, ActorType: audit.ActorSystem, Action: action,
		TargetType: "withdrawal_request", TargetID: requestID.String(), Outcome: audit.OutcomeSuccess, Metadata: md,
		IPAddress: p.IPAddress, UserAgent: p.UserAgent, RequestID: p.RequestID,
	}); err != nil {
		return fmt.Errorf("withdrawal: audit restriction policy: %w", err)
	}
	return nil
}

// pre0182SchemaKey is the TEST-ONLY context marker of a scratch database older than migration 0182. Unexported: no other
// package can set it, and this package's production code only reads it (pinned by a static test).
type pre0182SchemaKey struct{}

// pre0182SchemaHook is the cross-package form of the same TEST-ONLY marker (lic24): a migration test of ANOTHER package (e.g.
// internal/payments) that drives the production request / dispatch path on a scratch schema older than 0182. Only
// the setter in restriction_schema_hook_integration.go writes it, and that file exists ONLY in integration builds
// (restriction_schema_hook_integration.go, `//go:build integration`, the internal/kyc hold_hook_integration.go precedent): in
// every production binary nothing can set it, so it is constantly false. TestLic24_Static_Pre0182HookIsIntegrationOnly pins it.
var pre0182SchemaHook atomic.Bool

func pre0182SchemaForTest(ctx context.Context) bool {
	if pre0182SchemaHook.Load() {
		return true
	}
	v, _ := ctx.Value(pre0182SchemaKey{}).(bool)
	return v
}
