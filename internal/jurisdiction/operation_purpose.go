package jurisdiction

import (
	"errors"
	"fmt"
)

// This file implements PC-GAP-3 (canonical-model §14.6): the mapping from
// each OperationClass to the Purpose(s) it requires. It is the SINGLE
// canonical owner of that mapping - see purpose.go's own doc comment
// (amended by this same phase) for the "no other code may branch on an
// OperationClass to select a Purpose" rule this file exists to make true.
//
// This file contains ZERO mapping CONTENT. Every one of the four
// operation classes is undetermined and RequiredPurposes fails closed for
// all of them - see PC-GAP-3's own architect ruling for why guessing this
// mapping from consumer behaviour (every existing consumer makes an
// availability/restriction decision) would be exactly the legal guess
// CLAUDE.md forbids: Purpose does not classify what kind of decision a
// consumer makes, it selects WHICH EVIDENCE HIERARCHY governs the
// determination (purpose.go), and the two hierarchies differ in whether a
// real-time geolocation signal may ever restrict the operation at all
// (HDR-J-3a). That is a licensing judgment, not a fact recoverable from
// this repository - human decision item HDR-J-7.

// PurposeRequirement is the set of determinations one OperationClass
// requires. Its two-bool shape is deliberate and structural:
//   - "both" and "neither" are representable (canonical-model §14.6's own
//     enumeration: "identity determination, market-access control, both,
//     or neither");
//   - PurposeHistoricalReporting is NOT representable at all, because
//     DeterminePlayerJurisdiction refuses that purpose unconditionally
//     (precedence.go:101-103, HDR-J-2/HDR-J-4) - a mapping that could
//     name it would be a mapping to an unreachable evaluation;
//   - there is no slice, so there is no aliasing surface (the Phase C
//     slice-aliasing defect class, canonical-model §14.7 item 1).
type PurposeRequirement struct {
	IdentityDetermination bool
	MarketAccessControl   bool
}

// ErrPurposeMappingUndetermined is returned for a VALID OperationClass
// whose required Purpose(s) are an open legal/policy question
// (PC-GAP-3, canonical-model §14.6; human decision item HDR-J-7). It is
// deliberately distinct from ErrInvalidInput: "you asked about a real
// operation class and the answer is not ours to invent" is a different
// condition from "that is not an operation class", and an enforcement
// path must be able to alert on the two differently.
var ErrPurposeMappingUndetermined = errors.New("jurisdiction: the Purpose(s) required by this operation class is an undecided legal/policy question (PC-GAP-3 / HDR-J-7) and must never be guessed")

// purposeMapping is RequiredPurposes' internal representation. A ZERO
// VALUE entry means UNDETERMINED - never "neither" (that would itself be
// a decided answer). determined must be explicitly set true before
// requirement is meaningful.
type purposeMapping struct {
	determined  bool
	requirement PurposeRequirement
}

// operationClassPurposeMapping is the SINGLE canonical owner of the
// OperationClass -> Purpose mapping. No other code in this repository may
// branch on an OperationClass to select a Purpose; a future reviewer
// finding `if oc == OperationPlay` next to a Purpose selection must treat
// it as a defect against the Stage 4I Phase D architect ruling.
//
// It contains ZERO mapping content today: every one of the four classes
// is undetermined, and RequiredPurposes fails closed for all of them.
// When HDR-J-7 is answered, exactly one table literal below changes and
// nothing else in the codebase does.
var operationClassPurposeMapping = map[OperationClass]purposeMapping{
	OperationPlay:                  {}, // UNDETERMINED - HDR-J-7
	OperationCatalogueAvailability: {}, // UNDETERMINED - HDR-J-7
	OperationBonusIssuance:         {}, // UNDETERMINED - HDR-J-7
	OperationBonusConversion:       {}, // UNDETERMINED - HDR-J-7
}

// RequiredPurposes is the SINGLE canonical owner of the
// OperationClass -> Purpose mapping. See this file's own package-level
// doc comment for why no mapping content exists yet.
func RequiredPurposes(oc OperationClass) (PurposeRequirement, error) {
	if !validOperationClass(oc) {
		return PurposeRequirement{}, fmt.Errorf("%w: unknown operation_class %q", ErrInvalidInput, oc)
	}
	entry, ok := operationClassPurposeMapping[oc]
	if !ok {
		// Defence against a future enum value added to validOperationClass
		// without a corresponding map entry here - the invariant test in
		// operation_purpose_test.go makes this unreachable today.
		return PurposeRequirement{}, fmt.Errorf("%w: unknown operation_class %q", ErrInvalidInput, oc)
	}
	if !entry.determined {
		return PurposeRequirement{}, ErrPurposeMappingUndetermined
	}
	return entry.requirement, nil
}
