package payments

import (
	"strings"

	"github.com/Diansalas/igaming-platform/internal/kyc"
)

// kycUnavailablePrefix is the prefix internal/kyc/enforcement.go gives every
// fail-closed "enforcement could not be evaluated" decision code
// (OutcomeUnavailable). Such a decision is a DENY, but it is not a KYC
// requirement: the player has nothing to complete, and the deposit is
// retryable once enforcement is reachable again.
const kycUnavailablePrefix = "kyc_unavailable:"

// depositKYCDeclineReason maps a DepositKYCGate deny reason onto the declined
// deposit's terminal reason (PAY-FPAY-HARDENING-1 F-L2). A KYC outage is
// reported as itself (the gate's own "kyc_unavailable:<code>" reason), never as
// "kyc_required:", which would tell the player to complete KYC they may already
// hold.
//
// ADR 0116 s5.1 C4: the shared "account under review" code (kyc.AccountReviewCode:
// a KYC pending, an AML hold, or a hold-evaluator error) is reported as itself and
// NEVER with the "kyc_required:" prefix, so a held deposit is byte-identical to a
// KYC-pending one and tells the player nothing about why. Every other deny keeps
// the "kyc_required:" prefix. All are fail-closed: the deposit is declined and no
// money moves.
func depositKYCDeclineReason(denyReason string) string {
	if strings.HasPrefix(denyReason, kycUnavailablePrefix) || denyReason == kyc.AccountReviewCode {
		return denyReason
	}
	return "kyc_required:" + denyReason
}
