// This file implements docs/decisions/0096-kyc-enforcement-boundary.md
// (PRH-I3, KYC-ENFORCE-1): the deterministic "may this money-boundary
// operation proceed given the player's current KYC state" boundary.
//
// EvaluateEnforcement is a pure database read plus in-process comparison.
// It NEVER calls a KYCProvider/vendor - that is a completely separate
// concern (this package's own verification/document orchestration,
// already isolated behind KYCProvider, ADR 0028 §4). It never accepts a
// pre-computed Outcome from a caller (§2.5): the identity fields on
// EnforcementParams MUST be resolved server-side from authenticated/
// tenant context, exactly like rg.EligibilityParams/risk.RiskRequest.
//
// LABEL: IMPLEMENTED for the mechanism described in ADR 0096 §2/§3.2/§3.5
// as wired into internal/withdrawal, internal/casino, internal/sportsbook
// by this same change. The deposit call site (internal/payments.
// InitiateDeposit, at the time this file was written) was NOT wired by
// this change - PRH-I1 rewrote that call site (task split, task-
// registry.md PRH-I3 row). [Status note] InitiateDeposit itself is now
// deleted (PROV-OUTBOUND-CRED-1-LEGACY-PATH, E2); the deposit call site
// is InitiateDepositAttempt (internal/payments/deposit_v2.go), which
// calls this exported service via KYCEnforcementDepositGate
// (internal/payments/kycgate.go). No production threshold value exists anywhere in this
// file; every numeric value this package ever compares against comes
// from a kyc_enforcement_policies row an operator authors later (ADR
// 0096 §3.7) - zero rows are seeded by migration 0100.
//
// KNOWN GAP (disclosed, not hidden): the edd_amount and registration_tier
// trigger_types are fully modelled in the schema and in the admin write
// path (CreateEnforcementPolicy/ActivateEnforcementPolicy/
// WithdrawEnforcementPolicy below) but EvaluateEnforcement itself does
// not yet CONSULT them - only cumulative_deposit (for EnforcementDeposit)
// and play (for EnforcementCasinoPlay/EnforcementSportsbookPlay) are
// wired into the outcome computation, per the withdrawal structural rule
// (§3.2 point 1) needing no policy lookup at all. Wiring edd_amount and
// registration_tier is deferred, PARTIALLY IMPLEMENTED, pending HD-KYC-2/
// HD-KYC-3's actual content - the mechanism (table, CHECK constraints,
// admin write path) exists so that work is a policy-authoring exercise,
// not a schema/code change, matching ADR 0096's own "mechanism now,
// values later" contract.
package kyc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

// PolicyVersion is EvaluateEnforcement's own compiled-in logic version,
// recorded on every kyc_enforcement_decisions row (mirroring jurisdiction.
// PolicyVersion / risk's own decision-code discipline) so a later change
// to this file's evaluation logic is distinguishable, after the fact,
// from a change to the underlying policy DATA.
const PolicyVersion = "kyc-enforcement-v1"

// EnforcementOperation is the closed set of money-boundary operations
// this ADR's mechanism gates (ADR 0096 §2.2).
type EnforcementOperation string

const (
	EnforcementDeposit          EnforcementOperation = "deposit"
	EnforcementWithdrawalHold   EnforcementOperation = "withdrawal_hold"
	EnforcementWithdrawalPayout EnforcementOperation = "withdrawal_payout"
	EnforcementCasinoPlay       EnforcementOperation = "casino_play"
	EnforcementSportsbookPlay   EnforcementOperation = "sportsbook_play"
)

func validEnforcementOperation(op EnforcementOperation) bool {
	switch op {
	case EnforcementDeposit, EnforcementWithdrawalHold, EnforcementWithdrawalPayout,
		EnforcementCasinoPlay, EnforcementSportsbookPlay:
		return true
	default:
		return false
	}
}

// isWithdrawalOperation reports whether op is one of the two structural,
// always-on withdrawal enforcement points (ADR 0096 §3.2 point 1).
func isWithdrawalOperation(op EnforcementOperation) bool {
	return op == EnforcementWithdrawalHold || op == EnforcementWithdrawalPayout
}

func isPlayOperation(op EnforcementOperation) bool {
	return op == EnforcementCasinoPlay || op == EnforcementSportsbookPlay
}

// EnforcementOutcome is one of exactly five normalized values (ADR 0096
// §2.2) - never a raw kyc_verifications.status string, never a boolean.
type EnforcementOutcome string

const (
	OutcomeNotRequired EnforcementOutcome = "not_required"
	OutcomePassed      EnforcementOutcome = "passed"
	OutcomePending     EnforcementOutcome = "pending"
	OutcomeFailed      EnforcementOutcome = "failed"
	OutcomeUnavailable EnforcementOutcome = "unavailable"

	// OutcomeHeld is the INTERNAL sixth outcome of ADR 0116 s5.1 (an extension of ADR 0096 s2.2; s2.3 gains
	// held -> deny): an active AML hold gates the operation. It is only ever the value of EnforcementDecision.
	// TrueOutcome. It is NEVER the value of EnforcementDecision.Outcome, which is the PUBLIC outcome every
	// player-facing and tenant-visible surface renders: a held operation reports Outcome == OutcomePending, code
	// AccountReviewCode, exactly like a KYC `pending` (no tipping-off, ADR 0116 s5.1 C4). A static test pins that no
	// production file renders TrueOutcome.
	OutcomeHeld EnforcementOutcome = "held"
)

// AccountReviewCode is the ONE player-facing code of every deny that means "your account is under review": a KYC
// `pending`, an AML `held` and an AML hold-evaluator error (ADR 0116 s5.1 C4). It replaces the earlier
// "kyc_<operation>:pending" code at all five gates, and the deposit gate's "kyc_required:" decline prefix for it.
const AccountReviewCode = "account_review"

// PublicOutcomeReview is the outcome recorded in the tenant-visible kyc_enforcement_decisions log for both a KYC `pending`
// and an AML `held` (migration 0141). Historical rows keep `pending`; readers map it to `review`.
const PublicOutcomeReview = "review"

// EnforcementParams is EvaluateEnforcement's input. Every identity field
// MUST be resolved server-side from the authenticated/tenant context -
// never accepted from a request body.
//
// DEVIATION FROM THE ADR 0096 §2.2 SKETCH (disclosed): the sketch listed
// LicensingJurisdictionID as a caller-supplied field ("resolved... exactly
// as jurisdiction.ResolveEvaluationPolicy already does internally"). This
// implementation instead resolves it INSIDE EvaluateEnforcement, from
// TenantID, using the identical tenants.licence_id -> licences.
// jurisdiction_id join jurisdiction.ResolveEvaluationPolicy already
// performs - so every call site (deposit, withdrawal, casino, sportsbook)
// shares one resolution instead of four independent copies, and no call
// site can supply a wrong/stale jurisdiction id even by accident. This is
// a strictly narrower trust surface than the sketch, not a weaker one,
// and is recorded in this ADR's implementation record (§ "Implementation
// record") rather than left as a silent divergence.
type EnforcementParams struct {
	TenantID        uuid.UUID
	BrandID         uuid.UUID
	PlayerAccountID uuid.UUID
	// PersonID is REQUIRED for every operation, but is NOT the primary
	// read key (corrected per security re-verification N1, 2026-09-27,
	// which found the original draft self-contradictory - see
	// readLatestVerificationByPlayerAccount's own doc comment for the
	// full reasoning). The primary key for every operation, including
	// withdrawal, is the gated account's own latest row for
	// (tenant_id, brand_id, player_account_id), ordered (created_at DESC,
	// id DESC). PersonID is used ONLY as an additional, deny-only
	// cross-account overlay on the withdrawal structural rule
	// (crossAccountRejectedOverlay): a rejection recorded against a
	// DIFFERENT PlayerAccount of the same Person, in the same tenant,
	// denies a withdrawal from THIS account - never the reverse (an
	// approval on one account can never authorize a withdrawal from
	// another). HD-KYC-6 (cross-tenant/cross-brand reuse) is unaffected:
	// the overlay never widens visibility past the tenant boundary.
	PersonID  uuid.UUID
	Operation EnforcementOperation
	AssetCode string
	// Amount is minor units, player-chosen; never trusted as the sole
	// basis of a cumulative comparison (§2.6(e)) - only as the amount of
	// the transaction being gated right now.
	Amount        int64
	CorrelationID uuid.UUID
}

// EnforcementDecision is EvaluateEnforcement's output.
type EnforcementDecision struct {
	// Outcome is the PUBLIC outcome (never OutcomeHeld): what a player, a gate caller's response and a tenant-visible log
	// may show.
	Outcome        EnforcementOutcome
	Allowed        bool
	Code           string
	Message        string
	MatchedTrigger string
	PolicyVersion  string

	// Hold is the INTERNAL AML verdict (ADR 0116 s5.1). It is written only to the AML-scoped aml_enforcement_decisions
	// (by RecordDecision) and must never be rendered. TrueOutcome is OutcomeHeld when the hold changed the result.
	Hold        aml.HoldVerdict
	TrueOutcome EnforcementOutcome
	// KYCOutcome is the KYC side's own outcome before the hold was combined (internal; recorded in the AML-scoped log).
	KYCOutcome EnforcementOutcome
}

// PublicOutcome is the outcome string a tenant-visible log may record: `review` for pending (and therefore for held),
// otherwise the outcome unchanged.
func (d EnforcementDecision) PublicOutcome() string {
	if d.Outcome == OutcomePending {
		return PublicOutcomeReview
	}
	return string(d.Outcome)
}

// ErrEnforcementUnavailable is returned alongside a non-nil error from
// EvaluateEnforcement itself is never expected on the happy/deny path -
// deny is signalled through EnforcementDecision.Allowed == false with
// Outcome == OutcomeUnavailable, matching ADR 0096 §2.2's contract that
// "any non-nil error is a DENY at the call site". A non-nil error return
// additionally happens only for a structurally invalid call (missing
// required id), caught before any query runs.
var ErrEnforcementUnavailable = errors.New("kyc: enforcement evaluation unavailable")

// EvaluateEnforcement is the single authoritative "may this money-
// boundary operation proceed given the player's current KYC state"
// policy boundary (ADR 0096 §2). It never calls a KYCProvider and takes
// no row lock (plain SELECTs only, §5) - it composes safely anywhere
// RG/Risk already compose today, at the position ADR 0096 §2.4 states.
//
// Any non-nil error return is itself a caller bug (missing required
// input) and must be treated as a DENY - but the ordinary "the evaluator
// itself could not determine an outcome" case (a DB error, a malformed
// policy row) is represented as a NIL error with
// EnforcementDecision{Outcome: OutcomeUnavailable, Allowed: false}, per
// §2.6(c): "no rows found" and "the query failed" are different states,
// and a query failure must never be interpreted as not_required.
//
// KYC-ENF-OUTAGE-1 (2026-09-28, code review N1/rv-prh-i3-code-review.md
// "Re-review (FH-7)"): every read below runs inside a SAVEPOINT nested
// within tx (db.RunReadOnlyInSavepoint), not directly against tx. Before
// this fix, a genuine Postgres error inside e.g.
// readLatestVerificationByPlayerAccount aborted the CALLER's transaction
// (tx) - the branch functions below already swallow that error into a
// typed unavailableDecision with a nil Go error (§2.6(c)'s own contract),
// so EvaluateEnforcement itself returned successfully while silently
// leaving tx unusable. The caller (e.g. withdrawal.RequestWithdrawal)
// would then try to write that very "unavailable" decision INTO the now-
// aborted tx and hit SQLSTATE 25P02, which the withdrawal handler could
// only map to a generic non-retryable 500 - never the fail-closed,
// retryable 503 ADR 0096 §7.6 and the handler's own outcome-mapping
// (withdrawal_handlers.go:138-164) intend for an `unavailable` outcome.
// Running every read against a savepoint, and unconditionally rolling it
// back afterwards (safe: this function is read-only, §5, "it takes no row
// lock"), means a failed read is fully contained: tx itself is never
// touched, so the caller's own RecordDecision call - and every other
// domain write already in tx - keeps working exactly as if the failed
// read had never happened, and the resulting `unavailable` decision
// commits cleanly.
func EvaluateEnforcement(ctx context.Context, tx pgx.Tx, params EnforcementParams) (EnforcementDecision, error) {
	if params.TenantID == uuid.Nil || params.BrandID == uuid.Nil || params.PlayerAccountID == uuid.Nil {
		return EnforcementDecision{}, fmt.Errorf("kyc: tenant/brand/player account ids are required")
	}
	if !validEnforcementOperation(params.Operation) {
		return EnforcementDecision{}, fmt.Errorf("kyc: unknown enforcement operation %q", params.Operation)
	}
	if params.PersonID == uuid.Nil {
		return EnforcementDecision{}, fmt.Errorf("kyc: person id is required")
	}

	var decision EnforcementDecision
	if err := db.RunReadOnlyInSavepoint(ctx, tx, func(spTx pgx.Tx) error {
		var evalErr error
		decision, evalErr = evaluateEnforcementReads(ctx, spTx, params)
		return evalErr
	}); err != nil {
		// evaluateEnforcementReads itself never returns a non-nil error for
		// an ordinary DB read failure (those are folded into
		// unavailableDecision, per §2.6(c)) - a non-nil error here means
		// RunReadOnlyInSavepoint's OWN housekeeping failed (opening or
		// rolling back the savepoint), which is itself a sign the
		// connection/outer transaction is unusable. Fail closed the same
		// way as any other evaluator failure, rather than propagating a Go
		// error a caller might mistake for a structural caller bug.
		decision = unavailableDecision("savepoint_failed")
	}
	// ADR 0116 s5.1 (L1 / C6): the AML hold check runs UNCONDITIONALLY after the full KYC path, and the two are combined
	// at the end. It is never skipped by the no-licence `not_required` early return, and the KYC path still runs when a
	// hold is active, so there is no early-return timing signal.
	return composeHold(ctx, tx, params, decision)
}

// evaluateHold is the AML enforcement read. It is a package variable ONLY so a pure unit test with a fake pgx.Tx (which
// cannot answer a real database round trip) can stand in for it; production code never assigns it (a static test pins
// that no non-test file does).
var evaluateHold = aml.EvaluateHold

// amlOperationOf maps a KYC enforcement operation to the AML hold operation it corresponds to.
func amlOperationOf(op EnforcementOperation) aml.Operation {
	switch op {
	case EnforcementDeposit:
		return aml.OpDeposit
	case EnforcementWithdrawalHold:
		return aml.OpWithdrawal
	case EnforcementWithdrawalPayout:
		return aml.OpPayout
	case EnforcementCasinoPlay:
		return aml.OpCasinoStake
	case EnforcementSportsbookPlay:
		return aml.OpSportsbookStake
	}
	return ""
}

// accountReviewMessage is the one internal message of every "account under review" deny.
const accountReviewMessage = "account under review"

// composeHold combines the KYC decision with the AML hold verdict (ADR 0116 s5.1): deny if either side denies.
//
// Public equivalence (C4): a KYC `pending`, an AML `held` and a hold-evaluator error all produce Outcome pending, Allowed
// false and Code AccountReviewCode - byte-identical at every gate. A KYC `failed` / `unavailable` keeps its own public
// result (it already denies and the player is told nothing new); the hold is still recorded in the AML-scoped log.
func composeHold(ctx context.Context, tx pgx.Tx, params EnforcementParams, kycDecision EnforcementDecision) (EnforcementDecision, error) {
	verdict, hErr := evaluateHold(ctx, tx, params.TenantID, params.PersonID, params.PlayerAccountID, amlOperationOf(params.Operation))
	if hErr != nil {
		// Only a HARD error of the evaluator's alert raise (the transaction is unusable): every caller treats a non-nil
		// error from EvaluateEnforcement as a deny (ADR 0096 s2.2).
		return EnforcementDecision{}, fmt.Errorf("%w: %w", ErrEnforcementUnavailable, hErr)
	}
	kycDecision.Hold = verdict
	kycDecision.KYCOutcome = kycDecision.Outcome
	kycDecision.TrueOutcome = kycDecision.Outcome
	if kycDecision.Outcome == OutcomePending {
		kycDecision.Code = AccountReviewCode
		kycDecision.Message = accountReviewMessage
	}
	if !verdict.Denied() {
		return kycDecision, nil
	}
	if kycDecision.Outcome == OutcomeFailed || kycDecision.Outcome == OutcomeUnavailable {
		if verdict.Held {
			kycDecision.TrueOutcome = OutcomeHeld
		}
		return kycDecision, nil
	}
	out := kycDecision
	out.Outcome = OutcomePending
	out.Allowed = false
	out.Code = AccountReviewCode
	out.Message = accountReviewMessage
	out.MatchedTrigger = ""
	if verdict.Held {
		out.TrueOutcome = OutcomeHeld
	}
	return out, nil
}

// RetryableOnHoldEvalFailure re-expresses a decision that was denied ONLY because the AML hold evaluator failed (the KYC side
// itself passed or was not required, and no definite hold matched) as the existing `unavailable` outcome (ADR 0116 s5.1, ledger-
// finance condition C1; ADR 0096 N5 / PAY-KYC-UNAVAIL-1). Used at the payout CLAIM and at the sweeper's T2/T12 re-checks only:
// there a staff-approved withdrawal must stay approved and be retried, never be rejected or escalated on a transient evaluator
// fault. Every other decision, including a definite hold, is returned unchanged. The hold verdict is kept so the AML-scoped
// decision log still records the evaluator error.
func (d EnforcementDecision) RetryableOnHoldEvalFailure() EnforcementDecision {
	if !d.Hold.EvalFailed || d.Hold.Held {
		return d
	}
	if d.KYCOutcome != OutcomePassed && d.KYCOutcome != OutcomeNotRequired {
		return d
	}
	u := unavailableDecision("evaluator")
	u.Hold = d.Hold
	u.KYCOutcome = d.KYCOutcome
	u.TrueOutcome = OutcomeUnavailable
	return u
}

// evaluateEnforcementReads is EvaluateEnforcement's dispatch, unchanged in
// behavior from before KYC-ENF-OUTAGE-1 - the only difference is that its
// caller now always passes a SAVEPOINT-scoped tx, never the caller's own
// transaction directly.
//
// Withdrawal's structural rule (§3.2 point 1) needs NO policy-table
// lookup and therefore no jurisdiction resolution at all (§5:
// "withdrawal_hold/withdrawal_payout need no policy-table lookup and
// no withdrawal_requests history query") - resolved lazily below,
// only for the two operations that actually consult a policy row, so
// a tenant with no licence bound (a perfectly ordinary fixture/test
// state, or an as-yet-unlicensed tenant) never turns every withdrawal
// into a spurious `unavailable`.
func evaluateEnforcementReads(ctx context.Context, tx pgx.Tx, params EnforcementParams) (EnforcementDecision, error) {
	switch {
	case isWithdrawalOperation(params.Operation):
		return evaluateWithdrawalStructuralRule(ctx, tx, params)
	case params.Operation == EnforcementDeposit:
		licensingJurisdictionID, hasLicence, unavailable, err := resolveEnforcementJurisdiction(ctx, tx, params)
		if err != nil {
			// A genuine query error is a policy-lookup failure, not "no
			// policy configured" - unavailable, never not_required
			// (§2.6(c)).
			return unavailableDecision("jurisdiction_unresolved"), nil
		}
		if unavailable != "" {
			return unavailableDecision(unavailable), nil
		}
		if !hasLicence {
			// No licence bound means no jurisdiction-keyed policy could
			// possibly have been activated for this tenant (activation is
			// inherently jurisdiction-keyed) - not_required, not
			// unavailable. Distinct from a genuine query error above.
			signalKYCDormant(params.TenantID, params.Operation, "no_licence_bound")
			return notRequiredDecision(params.Operation, "tenant has no licence bound"), nil
		}
		return evaluateDepositThreshold(ctx, tx, params, licensingJurisdictionID)
	case isPlayOperation(params.Operation):
		licensingJurisdictionID, hasLicence, unavailable, err := resolveEnforcementJurisdiction(ctx, tx, params)
		if err != nil {
			return unavailableDecision("jurisdiction_unresolved"), nil
		}
		if unavailable != "" {
			return unavailableDecision(unavailable), nil
		}
		if !hasLicence {
			signalKYCDormant(params.TenantID, params.Operation, "no_licence_bound")
			return notRequiredDecision(params.Operation, "tenant has no licence bound"), nil
		}
		return evaluatePlayTrigger(ctx, tx, params, licensingJurisdictionID)
	default:
		return unavailableDecision("unhandled_operation"), nil
	}
}

func unavailableDecision(code string) EnforcementDecision {
	return EnforcementDecision{
		Outcome:       OutcomeUnavailable,
		Allowed:       false,
		Code:          "kyc_unavailable:" + code,
		Message:       "kyc enforcement evaluation could not complete",
		PolicyVersion: PolicyVersion,
	}
}

// kycDormantSignalled remembers which (tenant, operation, reason) was already
// reported, so the signal is once per process per key and never a per-request
// log flood.
var kycDormantSignalled sync.Map

// signalKYCDormant is the G9 operator signal (licensing enforcement slice 3):
// a KYC decision of not_required because the tenant has NO licence bound or NO
// active policy is a default-allow, and a live operator must be able to see
// it. It changes no decision. Logged at WARN once per process per key as
// kyc_enforcement_dormant (see docs/runbooks/observability-and-alerting.md);
// ListDormantJurisdictionTriggers remains the platform-admin read.
func signalKYCDormant(tenantID uuid.UUID, op EnforcementOperation, reason string) {
	key := tenantID.String() + "|" + string(op) + "|" + reason
	if _, loaded := kycDormantSignalled.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	slog.Default().Warn("kyc_enforcement_dormant", "tenant_id", tenantID.String(), "operation", string(op), "reason", reason)
}

func notRequiredDecision(op EnforcementOperation, message string) EnforcementDecision {
	return EnforcementDecision{
		Outcome:       OutcomeNotRequired,
		Allowed:       true,
		Code:          "kyc_" + string(op) + ":not_required",
		Message:       message,
		PolicyVersion: PolicyVersion,
	}
}

// resolveEnforcementJurisdiction is the lic14 (H-1) rulebook selection for a deposit or play decision. A tenant with NO approved,
// active, in-force operating basis resolves exactly as before (resolveLicensingJurisdictionID: a tenant with no licence is
// not_required). A tenant WITH bases selects the rulebook by the governing basis of the player's scope (tenant, brand, verified
// residence country, operation): the basis row's jurisdiction, or its licence's. If the scope is unavailable (no verified
// residence country, a basis that does not permit, no row and no licence) the result is a non-empty `unavailable` code: the
// money / wager path is never default-allowed because a basis, not a licence, opened it.
func resolveEnforcementJurisdiction(ctx context.Context, tx pgx.Tx, params EnforcementParams) (jid uuid.UUID, hasLicence bool, unavailable string, err error) {
	var asOf time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&asOf); err != nil {
		return uuid.Nil, false, "", fmt.Errorf("kyc: read clock: %w", err)
	}
	has, err := operatingmarket.TenantHasApprovedBasis(ctx, tx, params.TenantID, asOf)
	if err != nil {
		return uuid.Nil, false, "", fmt.Errorf("kyc: %w", err)
	}
	if !has {
		jid, hasLicence, err = resolveLicensingJurisdictionID(ctx, tx, params.TenantID)
		return jid, hasLicence, "", err
	}
	country, ok, err := ReadMarketResidenceEvidence(ctx, tx, params.TenantID, params.PlayerAccountID)
	if err != nil {
		return uuid.Nil, false, "", fmt.Errorf("kyc: %w", err)
	}
	if !ok {
		return uuid.Nil, false, "basis_scope_unavailable", nil
	}
	q := operatingmarket.RulebookQuery{TenantID: params.TenantID, CountryCode: country, AsOf: asOf}
	if params.BrandID != uuid.Nil {
		b := params.BrandID
		q.BrandID = &b
	}
	switch params.Operation {
	case EnforcementDeposit:
		q.OperationCode = "deposit"
	case EnforcementCasinoPlay:
		p := "casino"
		q.OperationCode, q.ProductCode = "wagering", &p
	case EnforcementSportsbookPlay:
		p := "sportsbook"
		q.OperationCode, q.ProductCode = "wagering", &p
	default:
		return uuid.Nil, false, "unhandled_operation", nil
	}
	rb, err := operatingmarket.ResolveRulebookJurisdiction(ctx, tx, q)
	if err != nil {
		return uuid.Nil, false, "", fmt.Errorf("kyc: %w", err)
	}
	switch rb.State {
	case operatingmarket.RulebookBasis:
		return rb.JurisdictionID, true, "", nil
	case operatingmarket.RulebookLegacy:
		// The scope has no row: the tenant's own licence governs, and a tenant with bases but no licence is never default-allow.
		jid, hasLicence, err = resolveLicensingJurisdictionID(ctx, tx, params.TenantID)
		if err == nil && !hasLicence {
			return uuid.Nil, false, "basis_scope_unavailable", nil
		}
		return jid, hasLicence, "", err
	default:
		return uuid.Nil, false, rb.Reason, nil
	}
}

// resolveLicensingJurisdictionID returns (jurisdictionID, true, nil) when
// the tenant has a bound licence resolving to a jurisdiction; (uuid.Nil,
// false, nil) when the tenant simply has no licence bound (a valid,
// non-error state - a not-yet-licensed tenant, or an ordinary test
// fixture); and a non-nil error only for a genuine query failure. Callers
// MUST treat these three cases differently (§2.6(c)): "no licence" is
// not_required, a query failure is unavailable - never conflated.
func resolveLicensingJurisdictionID(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (uuid.UUID, bool, error) {
	chain, err := jurisdiction.ResolveLicensingChain(ctx, tx, tenantID)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("kyc: %w", err)
	}
	switch chain.Outcome {
	case jurisdiction.LicensingResolved:
		return chain.JurisdictionID, true, nil
	case jurisdiction.LicensingNoLicence:
		// KNOWN GAP G9 (licensing enforcement slice 3): "no licence" is still
		// not_required for the caller (behaviour unchanged, dormant by design);
		// it is now an explicit, separately detectable state, surfaced by the
		// launch-readiness item R-KYC-ENFORCEMENT-DORMANT.
		return uuid.Nil, false, nil
	default:
		// licence_invalid: the chain cannot be relied on. Fail closed (an
		// error becomes unavailable), never not_required.
		return uuid.Nil, false, fmt.Errorf("kyc: tenant licensing chain unusable: %s", chain.InvalidReason)
	}
}

// latestVerificationState is the §2.6(a)/(b) read: the single latest row,
// deterministically ordered, with expiry folded into the effective
// status independently of the stored column.
type latestVerificationState struct {
	found  bool
	status VerificationStatus
}

// effectiveOutcome maps a latestVerificationState to one of
// passed/pending/failed, per ADR 0096 §2.3 - never itself returning
// not_required or unavailable (those are the caller's own decision,
// depending on whether a check is required at all, and whether the
// lookup itself failed).
func (s latestVerificationState) effectiveOutcome(expired bool) EnforcementOutcome {
	if !s.found {
		return OutcomeFailed
	}
	if expired {
		return OutcomeFailed
	}
	switch s.status {
	case StatusApproved:
		return OutcomePassed
	case StatusPending, StatusReviewRequired:
		return OutcomePending
	case StatusUnverified, StatusRejected, StatusExpired:
		return OutcomeFailed
	default:
		return OutcomeFailed
	}
}

// orphanRowExclusionSQL is ADR 0096 §2.6(g)'s "decided row" predicate
// (RV-PRH-I2 KYC review F1, code review + security): a kyc_verifications
// row with status='unverified' and provider_reference IS NULL is
// CreateVerification's own phase-A intent row (ADR 0095 §15.2) that never
// received ANY decision - phase B/C never completed a provider round-trip
// for it (a vendor outage, a resolver failure, or a process crash between
// phase A and phase C, ADR 0095 §15.2 "Failure recovery"). Such a row must
// NEVER be treated as this account's "latest" state for enforcement
// purposes: doing so would let a transient vendor outage alone silently
// supersede an already-DECIDED approval with a manufactured deny (F1's own
// reproduced scenario - an approved player starting a routine
// re-verification while the vendor happens to be down must not lose
// withdrawal access on that basis alone), and, via the cross-account
// overlay below, would let an undecided orphan on one account mask an
// already-decided REJECTION on that SAME account behind a newer-but-
// meaningless row. A row that DID receive a decision - CreateVerification's
// own phase C succeeded (provider_reference is set, whatever the resulting
// status), a callback applied, or a staff review applied - is never
// excluded by this predicate, however non-terminal that decision is
// (e.g. 'pending' still counts as decided, per the pre-existing, unchanged
// "success path already supersedes an approval with a pending row"
// behaviour F1 also names, which is deliberate and NOT this predicate's
// concern).
const orphanRowExclusionSQL = `NOT (status = 'unverified' AND provider_reference IS NULL)`

// readLatestVerificationByPlayerAccount is the PRIMARY read key every
// enforcement point uses (security re-verification N1, 2026-09-27,
// resolving the original draft's self-contradiction the same way §2.6(a)/
// HD-KYC-6 already state it): the gated account's own latest DECIDED row
// (orphanRowExclusionSQL) for (tenant_id, brand_id, player_account_id),
// ordered (created_at DESC, id DESC). PersonID is NOT used as the primary
// key anywhere - it is used ONLY as an additional, deny-only cross-account
// overlay for the withdrawal structural rule (crossAccountRejectedOverlay
// below), so a newer approval on one brand/account can never mask a
// rejection on a DIFFERENT brand/account of the same Person, without a
// stale approval on account A ever being able to authorize a withdrawal
// FROM account B. A never-decided orphan is treated identically to no row
// at all (found=false, OutcomeFailed either way) - this predicate changes
// nothing when every row for the account happens to be such an orphan.
func readLatestVerificationByPlayerAccount(ctx context.Context, tx pgx.Tx, tenantID, brandID, playerAccountID uuid.UUID) (latestVerificationState, bool, error) {
	var status string
	var expiresAt *time.Time
	err := tx.QueryRow(ctx, `
		SELECT status, expires_at FROM kyc_verifications
		 WHERE tenant_id = $1 AND brand_id = $2 AND player_account_id = $3
		   AND `+orphanRowExclusionSQL+`
		 ORDER BY created_at DESC, id DESC LIMIT 1`,
		tenantID, brandID, playerAccountID,
	).Scan(&status, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return latestVerificationState{}, false, nil
	}
	if err != nil {
		return latestVerificationState{}, false, err
	}
	expired := expiresAt != nil && !expiresAt.After(time.Now())
	return latestVerificationState{found: true, status: VerificationStatus(status)}, expired, nil
}

// crossAccountRejectedOverlay implements security re-verification N1's
// exact prescription: "PersonID is used, if at all, only as an
// additional deny-only check: failed if any other PlayerAccount of the
// same Person in the same tenant has a latest row that is rejected."
// Applied ONLY to the withdrawal structural rule (ADR 0096 §3.2 point 1)
// - never to deposit/play, which stay purely per-PlayerAccount per
// §2.6(a)/HD-KYC-6. A "latest row" here means each OTHER account's own
// most-recent DECIDED verification (orphanRowExclusionSQL, ADR 0096
// §2.6(g), RV-PRH-I2 KYC review F1) - a never-submitted orphan committed
// AFTER a genuine rejection must never mask that rejection just by being
// newer; the inner subquery's own predicate is what enforces this per
// OTHER account, independent of the outer query's tenant/person scope.
// finalStatusesSQL is N-1's own predicate (RV-PRH-I2 KYC security
// re-verification, HIGH, pre-existing), CORRECTED by N-1b (security
// re-verification 2, `rv-prh-i2-kyc-security.md`, and identity-compliance's
// own domain ruling, `rv-prh-i2-kyc-identity-compliance.md` Ruling 1, both
// 2026-09-27 - the two independently confirmed the same gap): this is NOT
// simply "every terminal/final status" (statusRank's own terminal tier,
// unverified(0) < pending(1) < review_required(2) < terminal(3),
// provider.go, includes `expired` there). It is deliberately NARROWER -
// only the two statuses that can ever count as evidence for THIS overlay's
// own question ("has this other account's rejection been superseded by a
// later genuine clearance?"). `expired` was wrongly included in an earlier
// revision of this constant: `expired` is a distinct terminal PROVIDER
// decision (ADR 0028 §2, `ProviderExpired`), never a status the platform
// derives from `approved.expires_at` lapsing (§2.6(b) - an approved row
// stays `status='approved'` forever unless a provider delivers an actual
// `expired` outcome), and ADR 0096 §2.3 folds `expired` into the SAME deny
// bucket as `rejected` ("Covers 'never verified', rejected, and expired
// uniformly"). Including it here let a fresh `expired` row on the rejected
// account silently become that account's "latest final row" in place of
// the earlier `rejected` row, which cleared the overlay's deny exactly
// like N-1's original `pending` exploit did - reproduced by security as
// N-1b. Excluding `expired` from this set entirely (rather than adding a
// third branch to reason about) means `expired` is simply invisible to
// this subquery, so the latest row among ONLY {approved, rejected} is
// exactly "has this account's most recent REAL decision between those two
// been a clearance or a rejection" - unaffected by however many `expired`
// attempts land at any point in between. Used ONLY by
// crossAccountRejectedOverlay's own "latest row per OTHER account"
// subquery - never by the primary read (readLatestVerificationByPlayerAccount),
// whose own "latest row" semantics (§2.6(a)) are unchanged, non-final rows
// (pending, review_required) included, per the primary-account rule.
const finalStatusesSQL = `('approved', 'rejected')`

// crossAccountRejectedOverlay implements security re-verification N1's
// original prescription, NARROWED by N-1 (security re-verification of fix
// round 492cb20, HIGH, pre-existing - not introduced by that round) and
// CORRECTED by N-1b (security re-verification 2 + identity-compliance
// Ruling 1, both 2026-09-27): "a rejection on another account of the same
// Person denies a withdrawal" held only until the FIRST reproduction of a
// fresh, merely-pending CreateVerification on the rejected account - a
// `pending` row counted as "decided" under the F1 fix, so a player could
// neutralise the overlay with one ordinary API call (start a new
// verification; it need not ever be approved). N-1's fix: the overlay is
// now computed from each OTHER account's latest row among ONLY
// finalStatusesSQL's two statuses (approved, rejected) - unverified,
// pending, review_required, `expired`, and any never-decided orphan are
// ALL ignored when deciding whether a rejection has been superseded. A
// rejection on another account is lifted ONLY by a LATER genuine
// `approved` decision on that SAME account - NEVER by a merely-in-progress
// re-verification attempt (pending/review_required), and NEVER by a later
// `expired` outcome either (N-1b's own correction - `expired` is not
// evidence of a clearance; ADR 0096 §2.3 treats it as a deny, identically
// to `rejected` - so it must not be able to supersede anything here, and
// is excluded from the set outright rather than reasoned about as a
// third case). The primary-account rule (§2.6(a), the gated account's own
// latest row, non-final rows included) is UNCHANGED - this predicate
// applies only inside this function's own subquery, scoped to OTHER
// accounts.
func crossAccountRejectedOverlay(ctx context.Context, tx pgx.Tx, tenantID, personID, excludePlayerAccountID uuid.UUID) (bool, error) {
	var rejected bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM kyc_verifications v1
			 WHERE v1.tenant_id = $1 AND v1.person_id = $2 AND v1.player_account_id <> $3
			   AND v1.status = 'rejected'
			   AND v1.id = (
			       SELECT v2.id FROM kyc_verifications v2
			        WHERE v2.tenant_id = v1.tenant_id AND v2.player_account_id = v1.player_account_id
			          AND v2.status IN `+finalStatusesSQL+`
			        ORDER BY v2.created_at DESC, v2.id DESC LIMIT 1
			   )
		)`,
		tenantID, personID, excludePlayerAccountID,
	).Scan(&rejected)
	if err != nil {
		return false, err
	}
	return rejected, nil
}

// evaluateWithdrawalStructuralRule implements ADR 0096 §3.2 point 1: every
// withdrawal request/payout dispatch requires the gated account's CURRENT
// latest verification to be passed and unexpired - full stop, no policy
// row, no "already withdrawn once" exemption, no jurisdiction override -
// PLUS the deny-only cross-account overlay (crossAccountRejectedOverlay)
// so a rejection recorded against a DIFFERENT PlayerAccount of the same
// Person cannot be sidestepped by withdrawing from this one.
func evaluateWithdrawalStructuralRule(ctx context.Context, tx pgx.Tx, params EnforcementParams) (EnforcementDecision, error) {
	state, expired, err := readLatestVerificationByPlayerAccount(ctx, tx, params.TenantID, params.BrandID, params.PlayerAccountID)
	if err != nil {
		return unavailableDecision("verification_lookup_failed"), nil
	}
	outcome := state.effectiveOutcome(expired)
	if outcome == OutcomePassed {
		crossRejected, err := crossAccountRejectedOverlay(ctx, tx, params.TenantID, params.PersonID, params.PlayerAccountID)
		if err != nil {
			return unavailableDecision("cross_account_overlay_failed"), nil
		}
		if crossRejected {
			outcome = OutcomeFailed
		}
	}
	return EnforcementDecision{
		Outcome:        outcome,
		Allowed:        outcome == OutcomePassed,
		Code:           "kyc_" + string(params.Operation) + ":" + string(outcome),
		Message:        "withdrawal requires a passed, unexpired verification (ADR 0096 §3.2)",
		MatchedTrigger: "structural:withdrawal_requires_passed",
		PolicyVersion:  PolicyVersion,
	}, nil
}

// activePolicyRow is the subset of a kyc_enforcement_policies row
// EvaluateEnforcement needs.
type activePolicyRow struct {
	id                  uuid.UUID
	thresholdMinorUnits *string // NUMERIC read as text to preserve exact precision
	assetCode           *string
}

// readActivePolicies returns EVERY active row for
// (licensingJurisdictionID, triggerType[, playOperation]) - one per
// asset_code, per the asset-aware unique index (security re-verification
// N2, 2026-09-27: kyc_enforcement_policies_one_active now includes
// asset_code, so more than one active row for the same trigger_type can
// legitimately coexist, one per asset).
func readActivePolicies(ctx context.Context, tx pgx.Tx, licensingJurisdictionID uuid.UUID, triggerType string, playOperation *string) ([]activePolicyRow, error) {
	var rows pgx.Rows
	var err error
	if playOperation != nil {
		rows, err = tx.Query(ctx, `SELECT id, threshold_minor_units::text, asset_code FROM kyc_enforcement_policies
		           WHERE licensing_jurisdiction_id = $1 AND trigger_type = $2 AND status = 'active' AND play_operation = $3`,
			licensingJurisdictionID, triggerType, *playOperation)
	} else {
		rows, err = tx.Query(ctx, `SELECT id, threshold_minor_units::text, asset_code FROM kyc_enforcement_policies
		           WHERE licensing_jurisdiction_id = $1 AND trigger_type = $2 AND status = 'active'`,
			licensingJurisdictionID, triggerType)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []activePolicyRow
	for rows.Next() {
		var row activePolicyRow
		if err := rows.Scan(&row.id, &row.thresholdMinorUnits, &row.assetCode); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// readActivePolicy is readActivePolicies' single-row convenience wrapper,
// used only where asset scoping does not apply (the 'play' trigger,
// which carries no asset_code at all).
func readActivePolicy(ctx context.Context, tx pgx.Tx, licensingJurisdictionID uuid.UUID, triggerType string, playOperation *string) (activePolicyRow, bool, error) {
	rows, err := readActivePolicies(ctx, tx, licensingJurisdictionID, triggerType, playOperation)
	if err != nil {
		return activePolicyRow{}, false, err
	}
	if len(rows) == 0 {
		return activePolicyRow{}, false, nil
	}
	return rows[0], true, nil
}

// evaluateDepositThreshold implements ADR 0096 §3.2 point 2's deposit
// leg: dormant (no active cumulative_deposit row for this jurisdiction,
// any asset) resolves to not_required/allow. If at least one active row
// exists for this jurisdiction but NONE matches the requested asset, the
// outcome is `unavailable` (fail closed) - security re-verification N2:
// no FX/cross-asset aggregation exists (that is KYC-FX-AGG-1, a separate,
// registered, human-decision-gated follow-up: it needs a rate source
// this platform does not have), so a jurisdiction that has decided
// cumulative-deposit KYC matters at all must not silently exempt an
// asset it has no configured threshold for. An active row matching the
// requested asset governs the comparison exactly as before.
func evaluateDepositThreshold(ctx context.Context, tx pgx.Tx, params EnforcementParams, licensingJurisdictionID uuid.UUID) (EnforcementDecision, error) {
	policies, err := readActivePolicies(ctx, tx, licensingJurisdictionID, "cumulative_deposit", nil)
	if err != nil {
		return unavailableDecision("policy_lookup_failed"), nil
	}
	if len(policies) == 0 {
		signalKYCDormant(params.TenantID, params.Operation, "no_active_policy")
		return EnforcementDecision{
			Outcome:       OutcomeNotRequired,
			Allowed:       true,
			Code:          "kyc_deposit:not_required",
			Message:       "no active cumulative_deposit policy for this jurisdiction",
			PolicyVersion: PolicyVersion,
		}, nil
	}
	var policy activePolicyRow
	var matched bool
	for _, p := range policies {
		if p.assetCode != nil && *p.assetCode == params.AssetCode {
			policy = p
			matched = true
			break
		}
	}
	if !matched {
		// N2: at least one active cumulative_deposit policy exists for
		// this jurisdiction, but none for the requested asset - fail
		// closed rather than silently exempting an unconfigured asset.
		return unavailableDecision("asset_not_covered_by_active_policy"), nil
	}

	cumulative, err := sumSettledDeposits(ctx, tx, params.TenantID, params.PlayerAccountID, params.AssetCode)
	if err != nil {
		return unavailableDecision("cumulative_lookup_failed"), nil
	}

	threshold, ok := new(big.Int).SetString(strings.TrimSpace(derefStr(policy.thresholdMinorUnits)), 10)
	if !ok {
		return unavailableDecision("malformed_threshold"), nil
	}
	total := new(big.Int).Add(cumulative, big.NewInt(params.Amount))
	if total.Cmp(threshold) < 0 {
		return EnforcementDecision{
			Outcome:        OutcomeNotRequired,
			Allowed:        true,
			Code:           "kyc_deposit:not_required",
			Message:        "cumulative deposit total below the active threshold",
			MatchedTrigger: policy.id.String(),
			PolicyVersion:  PolicyVersion,
		}, nil
	}

	state, expired, err := readLatestVerificationByPlayerAccount(ctx, tx, params.TenantID, params.BrandID, params.PlayerAccountID)
	if err != nil {
		return unavailableDecision("verification_lookup_failed"), nil
	}
	outcome := state.effectiveOutcome(expired)
	return EnforcementDecision{
		Outcome:        outcome,
		Allowed:        outcome == OutcomePassed,
		Code:           "kyc_deposit:" + string(outcome),
		Message:        "cumulative deposit threshold reached; verification required",
		MatchedTrigger: policy.id.String(),
		PolicyVersion:  PolicyVersion,
	}, nil
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// sumSettledDeposits sums SETTLED deposit credits (ledger TxDeposit) to
// the player's player_cash account for assetCode - never
// deposit_intents.amount, which includes declined/pending intents (ADR
// 0096 §2.6(e)/ledger-finance C6). KNOWN GAP: in-flight (initiated, not
// yet settled) deposits are NOT included here - the exact in-flight
// handling rule is HD-KYC-1's own content, not decided by this
// mechanism (ADR 0096 §2.6(e)).
func sumSettledDeposits(ctx context.Context, tx pgx.Tx, tenantID, playerAccountID uuid.UUID, assetCode string) (*big.Int, error) {
	var totalText *string
	err := tx.QueryRow(ctx, `
		SELECT SUM(e.amount)::text
		  FROM ledger_entries e
		  JOIN ledger_transactions t ON t.id = e.ledger_transaction_id
		  JOIN ledger_accounts a ON a.id = e.ledger_account_id
		  JOIN wallets w ON w.id = a.wallet_id
		 WHERE t.tenant_id = $1
		   AND w.player_account_id = $2
		   AND a.asset_code = $3
		   AND a.account_type = 'player_cash'
		   AND t.transaction_type = 'deposit'
		   AND e.direction = 'credit'`,
		tenantID, playerAccountID, assetCode,
	).Scan(&totalText)
	if err != nil {
		return nil, fmt.Errorf("kyc: sum settled deposits: %w", err)
	}
	if totalText == nil {
		return big.NewInt(0), nil
	}
	total, ok := new(big.Int).SetString(strings.TrimSpace(*totalText), 10)
	if !ok {
		return nil, fmt.Errorf("kyc: malformed settled-deposit sum %q", *totalText)
	}
	return total, nil
}

// evaluatePlayTrigger implements ADR 0096 §3.5: dormant (no active 'play'
// row for this jurisdiction/operation) resolves to not_required/allow. An
// active row requires the player's current verification to be passed,
// with the ordinary §2.3 mapping applied with no exception.
func evaluatePlayTrigger(ctx context.Context, tx pgx.Tx, params EnforcementParams, licensingJurisdictionID uuid.UUID) (EnforcementDecision, error) {
	op := string(params.Operation)
	policy, found, err := readActivePolicy(ctx, tx, licensingJurisdictionID, "play", &op)
	if err != nil {
		return unavailableDecision("policy_lookup_failed"), nil
	}
	if !found {
		signalKYCDormant(params.TenantID, params.Operation, "no_active_policy")
		return EnforcementDecision{
			Outcome:       OutcomeNotRequired,
			Allowed:       true,
			Code:          "kyc_" + op + ":not_required",
			Message:       "no active play policy for this jurisdiction/operation",
			PolicyVersion: PolicyVersion,
		}, nil
	}

	state, expired, err := readLatestVerificationByPlayerAccount(ctx, tx, params.TenantID, params.BrandID, params.PlayerAccountID)
	if err != nil {
		return unavailableDecision("verification_lookup_failed"), nil
	}
	outcome := state.effectiveOutcome(expired)
	return EnforcementDecision{
		Outcome:        outcome,
		Allowed:        outcome == OutcomePassed,
		Code:           "kyc_" + op + ":" + string(outcome),
		Message:        "jurisdiction requires a passed verification before play",
		MatchedTrigger: policy.id.String(),
		PolicyVersion:  PolicyVersion,
	}, nil
}

// RecordDecision writes one kyc_enforcement_decisions row plus one
// audit.Record call (ADR 0096 §3.6/§7.6) - PII-free, no document
// content, no raw provider reason. Callers are responsible for §3.6's
// commit discipline (allow: same transaction as the domain effect; deny:
// a transaction containing only this row and no domain effect) - this
// function itself only performs the write, inside whatever transaction
// the caller passes.
func RecordDecision(ctx context.Context, tx pgx.Tx, params EnforcementParams, decision EnforcementDecision) error {
	// ADR 0116 s5.1 C4: the tenant-visible log records the PUBLIC outcome. A KYC `pending` and an AML `held` (and a hold
	// evaluator error) are both `review`, and a `review` row carries no matched_trigger, so the two are indistinguishable
	// to a tenant reader. The true outcome and the matched hold go to the AML-scoped log below.
	publicOutcome := decision.PublicOutcome()
	var matchedTrigger *string
	if decision.MatchedTrigger != "" && publicOutcome != PublicOutcomeReview {
		matchedTrigger = &decision.MatchedTrigger
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO kyc_enforcement_decisions
			(tenant_id, brand_id, player_account_id, operation, outcome, allowed, matched_trigger, policy_version, correlation_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		params.TenantID, params.BrandID, params.PlayerAccountID, string(params.Operation),
		publicOutcome, decision.Allowed, matchedTrigger, decision.PolicyVersion, params.CorrelationID,
	); err != nil {
		return fmt.Errorf("kyc: record enforcement decision: %w", err)
	}
	if decision.Hold.Denied() {
		holdOutcome := aml.HoldOutcomeEvalError
		if decision.Hold.Held {
			holdOutcome = aml.HoldOutcomeHeld
		}
		kycOutcome := string(decision.KYCOutcome)
		if kycOutcome == "" {
			kycOutcome = string(decision.Outcome)
		}
		if err := aml.RecordEnforcementDecision(ctx, tx, aml.EnforcementRecord{
			TenantID: params.TenantID, PlayerAccountID: params.PlayerAccountID, PersonID: params.PersonID,
			Operation: string(params.Operation), KYCOutcome: kycOutcome, HoldOutcome: holdOutcome,
			PublicOutcome: publicOutcome, Allowed: decision.Allowed, HoldID: decision.Hold.HoldID, CorrelationID: params.CorrelationID,
		}); err != nil {
			return err
		}
	}

	action := "kyc.enforcement_allowed"
	outcome := audit.OutcomeSuccess
	if !decision.Allowed {
		action = "kyc.enforcement_denied"
		outcome = audit.OutcomeFailure
	}
	return audit.Record(ctx, tx, audit.Entry{
		TenantID:   params.TenantID,
		ActorType:  audit.ActorSystem,
		Action:     action,
		TargetType: "player_account",
		TargetID:   params.PlayerAccountID.String(),
		Outcome:    outcome,
		Metadata: map[string]any{
			"operation":      string(params.Operation),
			"kyc_outcome":    publicOutcome,
			"policy_version": decision.PolicyVersion,
			"correlation_id": params.CorrelationID.String(),
		},
	})
}
