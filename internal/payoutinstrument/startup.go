package payoutinstrument

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// ErrStartupGate is the startup refusal.
var ErrStartupGate = errors.New("payoutinstrument: startup gate refused")

// Registrations are the payout-capable components a binary registers.
type Registrations struct {
	// PaymentAdapters are the payment adapter values capable of paying out.
	PaymentAdapters []any
	// Verifiers are the registered instrument verifiers.
	Verifiers []PayoutInstrumentVerifier
}

// NonSyntheticRegistered reports whether any payout adapter or instrument
// verifier is not Synthetic.
func (r Registrations) NonSyntheticRegistered() bool {
	for _, a := range r.PaymentAdapters {
		if a != nil && !IsSyntheticComponent(a) {
			return true
		}
	}
	for _, v := range r.Verifiers {
		if v != nil && !IsSyntheticComponent(v) {
			return true
		}
	}
	return false
}

// VerifyStartup is the ADR 0111 2.2 startup gate. guardEnvironment is
// cfg.GuardEnvironment() (a missing APP_ENV counts as production). The key
// families are REQUIRED when the guard environment is "production" OR when
// any non-Synthetic payout adapter or instrument verifier is registered, in
// ANY environment. There is no in-binary random-key fallback outside the
// integration build tag, so outside both conditions the feature is simply
// unavailable (keys == nil) rather than silently keyed.
func VerifyStartup(guardEnvironment string, keys *Keys, regs Registrations) error {
	// L-8 (security review): until B13-B wires the binding into the withdrawal and
	// payments paths, NO non-Synthetic payout adapter may be registered - it would
	// dispatch NULL-binding withdrawals (the 0123 insert guard tolerates NULL/NULL
	// until B13-B replaces it). Remove this clause in the B13-B change.
	for _, a := range regs.PaymentAdapters {
		if a != nil && !IsSyntheticComponent(a) {
			return fmt.Errorf("%w: a non-Synthetic payout adapter is registered but B13-B (destination binding on the withdrawal and payments paths) has not landed; refusing to start", ErrStartupGate)
		}
	}
	required := guardEnvironment == "production" || regs.NonSyntheticRegistered()
	if required && keys == nil {
		return fmt.Errorf("%w: PAYOUT_INSTRUMENT_KEYS / PAYOUT_INSTRUMENT_ACTIVE_KID and PAYOUT_INSTRUMENT_FP_KEYS / PAYOUT_INSTRUMENT_FP_ACTIVE_KID are required (production, or a non-Synthetic payout adapter or verifier is registered); refusing to start", ErrStartupGate)
	}
	return nil
}

// MockProviderIDs is the literal MOCK payment provider-id set of migration
// 0123's up-time assertion. A test pins it equal to the ids of the
// providerkind.Synthetic payment adapters the binary registers.
var MockProviderIDs = []string{"mock-payments"}

// LegacyBindingChecker counts non-terminal pre-0123 withdrawals that have a
// provider id outside the MOCK set (the Go repeat of the L-6 assertion). It is
// satisfied by a function in cmd/platform-api that iterates tenants under RLS.
type LegacyBindingChecker func(ctx context.Context, mockProviderIDs []string) (int64, error)

// VerifyLegacyBindings is the Go startup check repeating migration 0123's
// assertion (A-11, three stacked controls with the tiering predicate): it
// fails when any non-terminal withdrawal carries a non-MOCK provider id and no
// binding. Run after the database is connected.
func VerifyLegacyBindings(ctx context.Context, check LegacyBindingChecker) error {
	ids := append([]string(nil), MockProviderIDs...)
	sort.Strings(ids)
	n, err := check(ctx, ids)
	if err != nil {
		return fmt.Errorf("%w: legacy binding check: %v", ErrStartupGate, err)
	}
	if n > 0 {
		return fmt.Errorf("%w: %d non-terminal withdrawal(s) with a non-MOCK provider_id have no payout destination binding (ADR 0111 A-11)", ErrStartupGate, n)
	}
	return nil
}
