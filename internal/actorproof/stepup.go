package actorproof

// ADR 0119 slice S4b, section 9.1: the signer backstop. The proof format and
// the database verifier do not change. The issuer refuses to sign a
// step-up-classified operation unless the request context carries a fresh
// auth.StepUpAssertion for the SAME actor (placed by the HTTP step-up gate
// after it enforced the route's class). A route that forgot its step-up class
// therefore fails loudly - the handler answers 500 (a signing failure is a
// defect, ADR 0110 section 7), nothing is written - instead of silently
// signing a governed write for a session that never stepped up.

import (
	"context"
	"errors"
	"time"

	"github.com/Diansalas/igaming-platform/internal/staffmfa"
	"github.com/Diansalas/igaming-platform/internal/stepup"
)

// ErrStepUpMissing: a step-up-classified operation was to be signed without a
// fresh step-up assertion for the proof's actor. Never a 403: a defect (500).
var ErrStepUpMissing = errors.New("actorproof: step-up-classified operation without a fresh step-up for the actor")

// StepUpInFlightGrace is added to a class's max-age: the HTTP gate accepted the
// assertion at the start of the request; the handler signs a moment later.
const StepUpInFlightGrace = 30 * time.Second

// StepUpOperations is the registry of step-up-classified proof operations
// (ADR 0119 section 9.1), each with the class whose max-age the backstop
// applies. The reject / cancel operations and the launch suspend are NOT here
// (protective, section 8.1). Every key is a proof operation constant; a static
// test pins the set. staff_lifecycle:request|approve joined with S5 + S6
// (section 10.4: a platform-staff grant, reactivation or MFA reset).
//
// rg_limit_config:apply is the platform staff member's INSERT of the approved
// floor row: a staff HTTP action executing a four-eyes decision, so it is
// critical. If ADR 0115 ever moves it to a system worker, it must leave this
// registry in the same change (otherwise the backstop turns it into a 500).
var StepUpOperations = map[string]stepup.Class{
	OpAdjustmentInitiate:    stepup.Financial,
	OpAdjustmentApprove:     stepup.Financial,
	OpResolutionRequest:     stepup.Financial,
	OpResolutionApprove:     stepup.Financial,
	OpHoldResolutionRequest: stepup.Financial,
	OpHoldResolutionApprove: stepup.Financial,
	OpGrantRequest:          stepup.Critical,
	OpGrantApprove:          stepup.Critical,
	OpGrantRevoke:           stepup.Critical,
	OpPolicyPropose:         stepup.Critical,
	OpPolicyApprove:         stepup.Critical,
	OpLaunchRequest:         stepup.Critical,
	OpLaunchApprove:         stepup.Critical,
	OpRGConfigRequest:       stepup.Critical,
	OpRGConfigApprove:       stepup.Critical,
	OpRGConfigApply:         stepup.Critical,
	OpStaffLifecycleRequest: stepup.Critical,
	OpStaffLifecycleApprove: stepup.Critical,
	// ADR 0116 A7: AML hold lift and the two halves of an AML four-eyes action (rule 8.1 (d)/(e)). Placing a hold, opening a
	// case, a transition, a note and a SAR flag only tighten or record, so they are protective and absent.
	OpAMLHoldLift:    stepup.Critical,
	OpAMLCasePropose: stepup.Critical,
	OpAMLCaseApprove: stepup.Critical,
	// ADR 0110 T8 (migration 0165): each half of a four-eyes payout reversal (credits house money to a player,
	// or releases a hold on the tombstone branch); the route POST /v1/admin/withdrawals/{id}/payout-reversal is
	// critical in the golden table.
	OpPayoutReversalDecide: stepup.Critical,
	// Migration 0177 (MKT-DUAL-1): filing a licensing / jurisdiction change, approving one and executing one (the final
	// approver, or the requester of a single-actor narrowing change, executes it) are critical; the reject and cancel
	// operations are protective and absent. The routes POST /v1/admin/licensing-changes and .../{changeID}/approve, and the
	// two legacy routes that now file a change (POST /v1/admin/licences, PUT /v1/admin/tenants/{tenantID}/licence), are
	// critical in the golden table.
	OpLicensingChangeRequest: stepup.Critical,
	OpLicensingChangeApprove: stepup.Critical,
	OpLicensingChangeExecute: stepup.Critical,
}

// stepUpBackstopMaxAge is the backstop's max-age per class: the section 7.4
// UPPER bound (configuration only ever tightens it, and the route gate has
// already applied the configured value) plus the in-flight grace.
func stepUpBackstopMaxAge(c stepup.Class) (time.Duration, bool) {
	switch c {
	case stepup.Critical:
		return staffmfa.StepUpCriticalMaxAgeBound.Max + StepUpInFlightGrace, true
	case stepup.Financial:
		return staffmfa.StepUpFinancialMaxAgeBound.Max + StepUpInFlightGrace, true
	}
	return 0, false
}

// stepUpExempt is the one scoped exemption: a financial_policy_change:propose
// from a TENANT session. Only financial_policy:tighten holders propose from a
// tenant session (financial_policy:author is platform_admin only), and
// migration 0113 refuses anything but a tightening there: a protective action
// (rule 8.1), classified none by financialPolicyChangeClass (section 8.2).
func stepUpExempt(c Claims) bool {
	return c.Operation == OpPolicyPropose && c.Scope == ScopeTenant
}

// RequiresStepUp reports whether signing c needs a fresh step-up assertion.
func RequiresStepUp(c Claims) bool {
	_, ok := StepUpOperations[c.Operation]
	return ok && !stepUpExempt(c)
}

// testStepUpFallbackOn is integration-test support ONLY (ADR 0119 section 9.1
// "Test support"): when it reports true, a governed domain call made directly
// by an integration test - a context that never passed the HTTP staff chain
// (!stepup.IsStaffRequest) - is treated as carrying a fresh synthetic step-up
// for its actor. It is split by build tag (security review of S4b, finding 5):
// stepup_fallback_prod.go (!integration) returns the constant false, so a
// production binary has no fallback variable at all; testsupport_integration.go
// (integration) holds the switch and its only setter, SetTestStepUpFallback.
// Static tests pin both files. Every HTTP staff request is marked by
// auth.Middleware and always meets the real backstop.

// checkStepUp is the backstop: nil when c is not step-up-classified, or when
// ctx carries a present assertion whose Subject is c.Actor and whose MFAAt is
// within the class's backstop max-age at now.
func checkStepUp(ctx context.Context, c Claims, now time.Time) error {
	if !RequiresStepUp(c) {
		return nil
	}
	if _, present := stepup.From(ctx); !present && testStepUpFallbackOn() && !stepup.IsStaffRequest(ctx) {
		return nil
	}
	maxAge, ok := stepUpBackstopMaxAge(StepUpOperations[c.Operation])
	if !ok {
		return ErrStepUpMissing
	}
	a, ok := stepup.From(ctx)
	if !ok || !a.FreshFor(c.Actor, now, maxAge) {
		return ErrStepUpMissing
	}
	return nil
}
