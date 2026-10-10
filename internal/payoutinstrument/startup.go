package payoutinstrument

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
)

// ErrStartupGate is the startup refusal.
var ErrStartupGate = errors.New("payoutinstrument: startup gate refused")

// Registrations are the payout-capable components a binary registers.
type Registrations struct {
	// PaymentAdapters are the payment adapter values capable of paying out.
	PaymentAdapters []any
	// Verifiers are the registered instrument verifiers.
	Verifiers []PayoutInstrumentVerifier
	// StatementSources are the registered payment statement sources (ADR 0111
	// 4.3, S-5: the key families are also required whenever any non-MOCK
	// statement source is registered, because its imports must be sealed to be
	// M4 evidence at all).
	StatementSources []any

	// PayoutEchoDeclarations carries each registered payment adapter's destination-echo declaration (ADR 0111 24),
	// read by the binary from the adapter's own manifest (never defaulted here). Every non-Synthetic entry of
	// PaymentAdapters MUST have exactly one entry, and every payout-capable entry (Synthetic or not) must declare a
	// valid state: an unset declaration refuses startup.
	PayoutEchoDeclarations []PayoutEchoDeclaration
}

// PayoutEchoDeclaration is one adapter's declaration as the startup gate sees it.
type PayoutEchoDeclaration struct {
	// Adapter is the registered adapter value (the same value placed in PaymentAdapters).
	Adapter any
	// ProviderID names the provider in refusals and in the startup marker.
	ProviderID string
	// PayoutCapable is the adapter's own SupportsWithdrawal capability. A deposit-only adapter makes no payout and
	// needs no echo declaration.
	PayoutCapable bool
	// Semantics is the manifest's declaration, verbatim.
	Semantics DestinationEchoSemantics
}

// EchoUnsupportedStartupEvent is the structured log message of the visible, auditable marker written at startup for each
// non-Synthetic payout adapter that declares DestinationEchoUnsupported (ADR 0111 24). Tests and log-based alerting key on it.
const EchoUnsupportedStartupEvent = "payout_destination_echo_unsupported_adapter_registered"

func sameComponent(a, b any) bool {
	if a == nil || b == nil {
		return false
	}
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	if ta != tb || !ta.Comparable() {
		return false
	}
	return a == b
}

// verifyEchoDeclarations is the ADR 0111 24 startup rule: no default, fail closed.
func verifyEchoDeclarations(regs Registrations) error {
	for _, a := range regs.PaymentAdapters {
		if a == nil || IsSyntheticComponent(a) {
			continue
		}
		found := false
		for _, d := range regs.PayoutEchoDeclarations {
			if sameComponent(d.Adapter, a) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: a non-Synthetic payment adapter (%T) is registered without a destination-echo declaration (ADR 0111 24); refusing to start", ErrStartupGate, a)
		}
	}
	for _, d := range regs.PayoutEchoDeclarations {
		if !d.PayoutCapable {
			continue
		}
		if !d.Semantics.Valid() {
			return fmt.Errorf("%w: payout adapter %q has no valid destination-echo declaration (%s): it must declare supported or unsupported explicitly, there is no default (ADR 0111 24); refusing to start", ErrStartupGate, d.ProviderID, d.Semantics)
		}
	}
	for _, d := range regs.PayoutEchoDeclarations {
		if d.PayoutCapable && d.Semantics == DestinationEchoUnsupported && d.Adapter != nil && !IsSyntheticComponent(d.Adapter) {
			slog.Warn(EchoUnsupportedStartupEvent, "provider_id", d.ProviderID,
				"destination_echo", d.Semantics.String(),
				"compensating_control", "statement-level evidence (M4/reconciliation)",
				"owner_acknowledgement", "per-provider, not yet defined (ADR 0111 24)")
		}
	}
	return nil
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
	for _, src := range r.StatementSources {
		if src != nil && !IsSyntheticComponent(src) {
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
	// B13-B (ADR 0111 section 18): the former L-8 clause (refuse every non-Synthetic payout
	// adapter until the binding lands) is REMOVED in the same change that wires the binding
	// into the withdrawal and payments paths and closes the NULL arm (migration 0126). A
	// non-Synthetic adapter now needs the keys below and the per-claim tiering predicate.
	if err := verifyEchoDeclarations(regs); err != nil {
		return err
	}
	required := guardEnvironment == "production" || regs.NonSyntheticRegistered()
	if required && keys == nil {
		return fmt.Errorf("%w: PAYOUT_INSTRUMENT_KEYS / PAYOUT_INSTRUMENT_ACTIVE_KID and PAYOUT_INSTRUMENT_FP_KEYS / PAYOUT_INSTRUMENT_FP_ACTIVE_KID are required (production, or a non-Synthetic payout adapter, verifier or payment statement source is registered); refusing to start", ErrStartupGate)
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

// BindingGuardChecker returns the source (pg_proc.prosrc) of withdrawal_requests_payout_binding_guard().
type BindingGuardChecker func(ctx context.Context) (string, error)

// bindingGuardMarker is the SQLSTATE the 0126 guard raises for NULL/NULL; its presence in the function source is
// how the binary recognises that the NULL-arm-closing migration is applied.
const bindingGuardMarker = "PI046"

// HasNonSyntheticPaymentAdapter reports whether any registered payout adapter is not Synthetic.
func (r Registrations) HasNonSyntheticPaymentAdapter() bool {
	for _, a := range r.PaymentAdapters {
		if a != nil && !IsSyntheticComponent(a) {
			return true
		}
	}
	return false
}

// VerifyBindingGuardApplied (B13-B security L-4) refuses to start when a non-Synthetic payout adapter is registered
// but the database guard still tolerates a NULL/NULL withdrawal (migration 0126, which closes the 0123 arm, is not
// applied). With only Synthetic adapters nothing is checked: the tiering predicate already confines a NULL binding
// to them.
func VerifyBindingGuardApplied(ctx context.Context, regs Registrations, check BindingGuardChecker) error {
	if !regs.HasNonSyntheticPaymentAdapter() {
		return nil
	}
	src, err := check(ctx)
	if err != nil {
		return fmt.Errorf("%w: binding guard check: %v", ErrStartupGate, err)
	}
	if !strings.Contains(src, bindingGuardMarker) {
		return fmt.Errorf("%w: a non-Synthetic payout adapter is registered but the withdrawal binding guard still tolerates NULL/NULL (migration 0126 not applied)", ErrStartupGate)
	}
	return nil
}
