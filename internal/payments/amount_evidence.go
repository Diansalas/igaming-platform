package payments

// PRH-2 C (LF-5): the ONE amount-and-asset comparison helper shared by every
// path that holds provider-echoed money evidence. C uses it for a synchronous
// deposit success (DepositResult.Amount/AssetCode); PRH-2 D reuses it,
// unchanged, for the status-poll path (StatusResult.Amount/AssetCode,
// PAY-POLL-AMOUNT-1) so the two never drift into two subtly different rules.
//
// The platform's own recorded figures (the attempt's amount and asset) are
// the EXPECTED side; the provider's echo is the EVIDENCE side. The helper is
// pure: no I/O, no state, and it never decides what to do with the answer -
// the caller routes Missing to "ambiguous, the poll decides" and Mismatch to
// a T10 dispute with no posting.

// AmountEvidence is the three-way outcome of comparing a provider's echoed
// amount and asset against what the platform recorded.
type AmountEvidence int

const (
	// AmountEvidenceMissing: the provider did not echo a usable amount or
	// asset (zero amount or empty asset - the zero value of the struct field
	// is indistinguishable from "not supplied", so it is treated as absent).
	// This is NOT a mismatch: absent evidence means "cannot confirm", which a
	// caller resolves by polling, never by posting and never by disputing.
	AmountEvidenceMissing AmountEvidence = iota
	// AmountEvidenceMatch: both the amount and the asset code equal the
	// platform's recorded values exactly.
	AmountEvidenceMatch
	// AmountEvidenceMismatch: the provider echoed a concrete amount and asset
	// and at least one differs from the platform's record (including a
	// negative amount, which no honest provider echoes).
	AmountEvidenceMismatch
)

// String makes AmountEvidence loggable and test-assertable without leaking
// any amount.
func (e AmountEvidence) String() string {
	switch e {
	case AmountEvidenceMissing:
		return "missing"
	case AmountEvidenceMatch:
		return "match"
	case AmountEvidenceMismatch:
		return "mismatch"
	default:
		return "unknown"
	}
}

// CompareProviderAmount compares the provider's echoed amount and asset code
// against the platform's expected values. The asset comparison is an exact
// string comparison: asset codes are opaque registry keys here, never
// case-folded or trimmed (providerref's "never normalise" rule).
func CompareProviderAmount(expectedAmount int64, expectedAssetCode string, gotAmount int64, gotAssetCode string) AmountEvidence {
	if gotAmount == 0 || gotAssetCode == "" {
		return AmountEvidenceMissing
	}
	if gotAmount != expectedAmount || gotAssetCode != expectedAssetCode {
		return AmountEvidenceMismatch
	}
	return AmountEvidenceMatch
}
