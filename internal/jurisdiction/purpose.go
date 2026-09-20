package jurisdiction

// Purpose identifies WHICH EVIDENCE HIERARCHY governs a player-jurisdiction
// determination (Stage 4I Phase C, docs/decisions/0042-human-decision-
// response.md HDR-J-2). It is DELIBERATELY SEPARATE from the existing
// OperationClass (OperationPlay/OperationCatalogueAvailability/
// OperationBonusIssuance/OperationBonusConversion, types.go):
// OperationClass answers "which call site, is resolution switched on";
// Purpose answers "which evidence hierarchy governs this determination."
//
// The mapping from OperationClass to Purpose is owned by exactly one
// place: RequiredPurposes (operation_purpose.go), added by Stage 4I
// Phase D. That function deliberately contains NO mapping content - all
// four operation classes return ErrPurposeMappingUndetermined, because
// which Purpose an operation requires is undecided HDR-J-2/PC-GAP-3
// legal content owned by identity-compliance + legal (human decision
// item HDR-J-7), not engineering. No other code may branch on an
// OperationClass to select a Purpose. Purpose itself remains a
// compile-time constant at each future call site: there is no wire
// representation, no HTTP surface, and no database column for it.
type Purpose string

const (
	// PurposeIdentityDetermination covers identity/KYC and residence-based
	// regulatory determinations (HDR-J-2).
	PurposeIdentityDetermination Purpose = "identity_determination"
	// PurposeMarketAccessControl covers real-time market-access /
	// access-control decisions (HDR-J-2).
	PurposeMarketAccessControl Purpose = "market_access_control"
	// PurposeHistoricalReporting covers an event-time record - it is
	// NEVER recomputed from current evidence (HDR-J-2/HDR-J-4).
	PurposeHistoricalReporting Purpose = "historical_reporting"
)

// validPurpose reports whether p is one of the three declared Purpose
// constants. Exhaustive switch, fail closed on anything else.
func validPurpose(p Purpose) bool {
	switch p {
	case PurposeIdentityDetermination, PurposeMarketAccessControl, PurposeHistoricalReporting:
		return true
	default:
		return false
	}
}
