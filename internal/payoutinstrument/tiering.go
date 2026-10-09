package payoutinstrument

import (
	"errors"

	"github.com/Diansalas/igaming-platform/internal/providerkind"
)

// ErrTierRefused is the tiering predicate's refusal.
var ErrTierRefused = errors.New("payoutinstrument: tiering predicate refused")

// IsSyntheticComponent reports whether c carries the providerkind.Synthetic
// Go type marker. The tiering predicate is keyed ONLY on this marker of the
// live object - never on a provider_id or on configuration (ADR 0111 2.5).
func IsSyntheticComponent(c any) bool {
	_, ok := c.(providerkind.Synthetic)
	return ok
}

// TierInput is what the predicate needs about the destination.
type TierInput struct {
	// Bound: the withdrawal has a non-NULL payout instrument binding.
	Bound bool
	// Source is the in-force verification's source (meaningful when Bound).
	Source VerificationSource
	// KindNonSyntheticEnabled: payout_instrument_kinds.non_synthetic_enabled.
	KindNonSyntheticEnabled bool
	// SealsValid: every seal on the instrument and verification verified.
	SealsValid bool
	// VerifierRegisteredNonSynthetic: the verification's verifier_provider_id
	// names a currently registered non-Synthetic verifier.
	VerifierRegisteredNonSynthetic bool
}

// CheckTier is the tiering predicate (ADR 0111 2.5). It must run at T1p, in
// phase B on the EXACT adapter value about to be invoked, and at T2/T12.
//
//   - A non-Synthetic payment adapter (sandbox or real) requires a non-NULL
//     binding, a non-synthetic source, a non_synthetic_enabled kind, valid
//     seals and a registered non-Synthetic verifier.
//   - A Synthetic adapter accepts any source and, for legacy rows only, a NULL
//     binding.
func CheckTier(adapter any, in TierInput) error {
	if IsSyntheticComponent(adapter) {
		return nil
	}
	if adapter == nil || !in.Bound || in.Source.IsSynthetic() || !in.Source.Valid() ||
		!in.KindNonSyntheticEnabled || !in.SealsValid || !in.VerifierRegisteredNonSynthetic {
		return ErrTierRefused
	}
	return nil
}
