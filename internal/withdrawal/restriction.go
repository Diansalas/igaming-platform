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
// Fail closed: a read error, an unknown licence status or an unknown cause is an error wrapping ErrGateUnavailable
// (retryable; nothing is written, nothing is claimed, nothing is sent). A default never refuses.
//
// Lock order (ADR 0082): plain reads only. At R and T1p it runs after the H-SEC-11 status gate (tenant status key SHARED)
// and before the WD-RG-1 gate (L0.4). A policy change takes the same tenant status key EXCLUSIVE before it writes (0182
// guards), so a decision either sees the change or the change waits for the decision's transaction. At T2 / T12 it runs
// before the L1 lock without the status key (the sweeper takes none today): one tick may still use the previous version;
// the recorded set version says which.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
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
	// LicenceState is the licence evaluation behind a licence cause (jurisdiction.LicenceValidity or a chain outcome).
	LicenceState string
}

// Refuses reports whether the decision holds or blocks.
func (d RestrictionDecision) Refuses() bool { return d.Action != RestrictionAllow }

// Provenance is the recordable form (withdrawal_gate_decisions columns, audit metadata).
func (d RestrictionDecision) Provenance() *withdrawalgate.RestrictionProvenance {
	return &withdrawalgate.RestrictionProvenance{Source: d.Source, SetVersion: d.SetVersion, Cause: string(d.DecidingCause), Action: string(d.Action)}
}

// AuditMetadata is the closed audit payload of the evaluation (codes and versions only).
func (d RestrictionDecision) AuditMetadata() map[string]any {
	md := map[string]any{
		"restriction_stage": string(d.Stage), "restriction_action": string(d.Action),
		"restriction_policy_source": d.Source, "restriction_policy_set_version": d.SetVersion,
	}
	if d.DecidingCause != "" {
		md["restriction_cause"] = string(d.DecidingCause)
		md["restriction_licence_state"] = d.LicenceState
	}
	if d.PolicyID != nil {
		md["restriction_policy_id"] = d.PolicyID.String()
		md["restriction_policy_version"] = d.PolicyVersion
	}
	return md
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

// licenceCause maps the shared licensing chain + validity to a cause ("" when the licence is valid).
func licenceCause(chain jurisdiction.LicensingOutcome, validity jurisdiction.LicenceValidity) (RestrictionCause, string, error) {
	switch chain {
	case jurisdiction.LicensingNoLicence:
		return CauseLicenceMissing, string(jurisdiction.LicenceNotBound), nil
	case jurisdiction.LicensingInvalid:
		return CauseLicenceMissing, string(jurisdiction.LicenceNotFound), nil
	case jurisdiction.LicensingResolved:
	default:
		return "", "", fmt.Errorf("%w: chain outcome %q", ErrRestrictionUnknownLicenceState, chain)
	}
	switch validity {
	case jurisdiction.LicenceValid:
		return "", string(validity), nil
	case jurisdiction.LicenceSuspended:
		return CauseLicenceSuspended, string(validity), nil
	case jurisdiction.LicenceStatusExpired, jurisdiction.LicenceDateExpired:
		return CauseLicenceExpired, string(validity), nil
	case jurisdiction.LicenceNotYetIssued:
		return CauseLicenceNotYetIssued, string(validity), nil
	case jurisdiction.LicenceNotBound, jurisdiction.LicenceNotFound:
		return CauseLicenceMissing, string(validity), nil
	}
	return "", "", fmt.Errorf("%w: %q", ErrRestrictionUnknownLicenceState, validity)
}

// effective is one cause's resolution.
type effective struct {
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
	}
	return d
}

// restrictionClock is the evaluation instant of the licence dates (UTC calendar dates, EvaluateLicenceValidity's own
// half-open semantics). A package variable only so a unit test can pin it; production never assigns it.
var restrictionClock = time.Now

// EvaluateRestriction resolves the withdrawal restriction policy of tenantID at stage inside tx (a tenant-scoped session:
// the policy rows, the tenant row and its own licence are visible). It writes nothing and takes no lock. Any error wraps
// ErrGateUnavailable (fail closed, retryable).
func EvaluateRestriction(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, stage RestrictionStage) (RestrictionDecision, error) {
	d, err := evaluateRestriction(ctx, tx, tenantID, stage)
	if err != nil {
		return RestrictionDecision{}, fmt.Errorf("%w: restriction policy: %w", ErrGateUnavailable, err)
	}
	return d, nil
}

func evaluateRestriction(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, stage RestrictionStage) (RestrictionDecision, error) {
	switch stage {
	case RestrictionStageRequest, RestrictionStageDispatch, RestrictionStageRedispatch:
	default:
		return RestrictionDecision{}, fmt.Errorf("%w: unknown restriction stage %q", ErrInvalidInput, stage)
	}
	if tenantID == uuid.Nil {
		return RestrictionDecision{}, fmt.Errorf("%w: tenant id is required", ErrInvalidInput)
	}
	chain, err := jurisdiction.ResolveLicensingChain(ctx, tx, tenantID)
	if err != nil {
		return RestrictionDecision{}, err
	}
	if chain.Outcome == jurisdiction.LicensingInvalid && chain.InvalidReason == jurisdiction.LicensingInvalidTenantNotVisible {
		// Every gate session sees its own tenant row: an invisible one is a mis-scoped session, never "no licence".
		return RestrictionDecision{}, errors.New("withdrawal: the tenant row is not visible to the restriction policy evaluation")
	}
	validity := jurisdiction.LicenceValid
	if chain.Outcome == jurisdiction.LicensingResolved {
		if validity, err = jurisdiction.EvaluateLicenceValidity(ctx, tx, chain.LicenceID, restrictionClock().UTC()); err != nil {
			return RestrictionDecision{}, err
		}
	}
	cause, licenceState, err := licenceCause(chain.Outcome, validity)
	if err != nil {
		return RestrictionDecision{}, err
	}
	var setVersion int64
	if err := tx.QueryRow(ctx, `SELECT withdrawal_restriction_set_version($1)`, tenantID).Scan(&setVersion); err != nil {
		return RestrictionDecision{}, fmt.Errorf("withdrawal: read restriction policy set version: %w", err)
	}
	var effs []effective
	if cause != "" {
		e := effective{cause: cause}
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
	return combine(stage, setVersion, licenceState, effs), nil
}

// evaluateRequestRestriction runs the policy for a NEW request (after H-SEC-11, before the WD-RG-1 gate). A configured
// block writes the denial audit in tx and returns a *GateDeniedError with GateCodeRestriction (WD056): the HTTP handler and
// every existing caller already commit a gate denial's audit and answer the player generically. Otherwise it returns the
// decision for the request's audit.
func evaluateRequestRestriction(ctx context.Context, tx pgx.Tx, p RequestParams) (RestrictionDecision, error) {
	rd, err := EvaluateRestriction(ctx, tx, p.TenantID, RestrictionStageRequest)
	if err != nil {
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

// auditRequestRestriction records, in the request's transaction, a restriction cause that was in force when the request
// was ACCEPTED (allow or hold_for_review). Nothing is written when no cause is in force (the request's own audit row
// carries the provenance either way).
func auditRequestRestriction(ctx context.Context, tx pgx.Tx, p RequestParams, requestID uuid.UUID, rd RestrictionDecision) error {
	if rd.DecidingCause == "" {
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
