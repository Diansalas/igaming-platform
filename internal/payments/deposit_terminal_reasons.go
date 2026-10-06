package payments

import "strings"

// Terminal reasons written outside the TerminalReason* constants of attempt.go and
// drive.go. They are named here so DepositDisputeTerminalReasons is the one place
// that enumerates every reason a deposit dispute can carry.
const (
	// TerminalReasonCallbackAmountAssetMismatch: a verified callback's success
	// echoed an amount or asset different from the live attempt's record (T10).
	TerminalReasonCallbackAmountAssetMismatch = "callback_amount_asset_mismatch"
	// TerminalReasonSuccessForNeverSentAttempt: success evidence for an attempt
	// the platform never sent (T15).
	TerminalReasonSuccessForNeverSentAttempt = "success_for_never_sent_attempt"
)

// invalidProviderReferencePrefix: the invalid-reference park writes
// "invalid_provider_reference:<reason>" with <reason> from the closed
// providerref.Reason set.
const invalidProviderReferencePrefix = TerminalReasonInvalidProviderReference + ":"

// DepositDisputeTerminalReasons enumerates EVERY terminal_reason the payments
// package can write on a deposit attempt that moves to 'disputed'. Entries ending
// in ":" are prefixes (the suffix is a closed providerref reason). Consumers
// (reconciliation's classification of parked captures) pin themselves against
// this list; a unit test fails if a write site uses a reason that is not in it.
//
// The strings are a contract: reconciliation matches them exactly.
func DepositDisputeTerminalReasons() []string {
	return []string{
		TerminalReasonMultipleSuccessForIntent,
		TerminalReasonTombstonePrecedesSuccess,
		TerminalReasonSyncAmountMismatch,
		TerminalReasonProviderReferenceConflict,
		TerminalReasonInvalidProviderReference, // B5 (D1-CR-4): the bare form drive.go writes when AsError fails
		invalidProviderReferencePrefix,
		TerminalReasonPollAmountMismatch,
		TerminalReasonPollReferenceMismatch,
		TerminalReasonCallbackAmountAssetMismatch,
		TerminalReasonSuccessForNeverSentAttempt,
	}
}

// IsDepositDisputeTerminalReason reports whether reason is one a deposit dispute
// may carry (exact match, or the invalid_provider_reference: prefix).
func IsDepositDisputeTerminalReason(reason string) bool {
	for _, r := range DepositDisputeTerminalReasons() {
		if strings.HasSuffix(r, ":") {
			if strings.HasPrefix(reason, r) {
				return true
			}
			continue
		}
		if reason == r {
			return true
		}
	}
	return false
}
