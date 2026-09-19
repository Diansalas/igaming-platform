package jurisdiction

// Purpose identifies WHICH EVIDENCE HIERARCHY governs a player-jurisdiction
// determination (Stage 4I Phase C, docs/decisions/0042-human-decision-
// response.md HDR-J-2). It is DELIBERATELY SEPARATE from the existing
// OperationClass (OperationPlay/OperationCatalogueAvailability/
// OperationBonusIssuance/OperationBonusConversion, types.go):
// OperationClass answers "which call site, is resolution switched on";
// Purpose answers "which evidence hierarchy governs this determination."
//
// No mapping function between OperationClass and Purpose exists or should
// be added in this package. That mapping is undecided HDR-J-2 content -
// tracked as PC-GAP-3, owned by identity-compliance + legal, not
// engineering. A future caller supplies Purpose as a compile-time
// constant, exactly as OperationClass is supplied today; there is no wire
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
