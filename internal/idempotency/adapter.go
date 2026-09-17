package idempotency

import (
	"context"
	"errors"
	"fmt"
)

// This file closes Security finding S-5 (docs/security/security-
// architecture.md, Stage 4H-B0-R5 section): ADR 0038 §14.1's fallback
// occurrence discriminator - for a provider whose own protocol carries
// no dedicated per-occurrence field - derived from "the adapter's own
// inbound-delivery deduplication record," a transport-level observation
// the provider never signed. A validly-signed body replayed as a
// distinct transport delivery would get a NEW dedup record, compose to a
// NEW key, and post as a second, "legitimate-looking" occurrence - a
// real double-post that the database's own uniqueness constraint cannot
// catch, because the constraint is only as good as the string handed to
// it.
//
// The fix is a contract, enforced by this file's types and by
// ResolveOccurrence's own logic, not merely documented as a rule for an
// adapter author to remember: an occurrence discriminator is ONLY ever
// accepted from a field that was verified as part of the specific
// event's own signature/authentication check (OccurrenceSource). When a
// provider's protocol genuinely has no such field at all, the fail-closed
// answer is ErrOccurrenceOrdinalRequiredButUnavailable - the transaction
// type is rejected for that provider - never a silent transport-derived
// substitute. ADR 0038 §14.1 separately describes a platform-minted
// canonical-id round-trip as a possible future fallback tier for such a
// provider; this package does not yet implement that mechanism (no
// caller needs it today - see doc.go), so it is not declared here ahead
// of a real use.

// ErrNoAuthenticatedOccurrenceField is returned by
// OccurrenceSource.AuthenticatedOccurrenceReference when the provider's
// protocol supplies no field, for this specific event, that was part of
// what the provider's own signature/authentication check covered. An
// implementation MUST return this error rather than synthesizing a
// value from a transport-level signal (a delivery id, a receipt
// timestamp, an inbound-deduplication record, or any other observation
// the provider never signed) - doing so is precisely the S-5 defect this
// package exists to close.
var ErrNoAuthenticatedOccurrenceField = errors.New("idempotency: provider protocol supplies no authenticated per-occurrence field for this event")

// ErrOccurrenceOrdinalRequiredButUnavailable is ResolveOccurrence's
// fail-closed result when transactionType requires an occurrence
// discriminator (per RequiresOrdinalFunc) and no authenticated
// per-occurrence field (via OccurrenceSource) exists for this event. Per
// the directive's explicit instruction, this is the correct behavior for
// "this provider cannot supply one" - the transaction type is REJECTED
// for this provider until it can supply an authenticated discriminator
// (whether that is a native protocol field or a future round-trip
// mechanism the owning adapter builds - see ADR 0038 §14.1), never
// silently downgraded to a weaker, transport-derived discriminator.
var ErrOccurrenceOrdinalRequiredButUnavailable = errors.New("idempotency: transaction type requires an occurrence discriminator and none can be safely obtained for this event (fail closed)")

// OccurrenceSource is the adapter-contract requirement every future
// provider adapter must implement for any transaction type
// RequiresOrdinalFunc reports true for (ADR 0038 §14.1's named examples:
// partial settlement, cashout, rollback/re-settlement). It is the single
// place S-5's fix is enforced: the returned ProviderOccurrenceID must be
// sourced exclusively from a field inside the already-verified event
// that was itself part of what the provider's signature/authentication
// check covered.
//
// verifiedEvent is deliberately typed `any`: this package has no
// knowledge of, and must never depend on, any specific provider's or
// domain's own parsed-callback type (internal/casino.CallbackEvent, a
// future internal/sportsbook equivalent, etc.) - exactly the provider-
// neutrality this specialist's charter requires. An implementation type-
// asserts verifiedEvent to its own domain's callback type internally.
type OccurrenceSource interface {
	// AuthenticatedOccurrenceReference returns the discriminator for one
	// already-signature-verified inbound event (verifiedEvent must never
	// be handed to this method before its signature has been checked -
	// mirroring internal/casino/mock.go's HandleCallback, which verifies
	// the HMAC over the full payload before returning a CallbackEvent at
	// all).
	//
	// Returns ErrNoAuthenticatedOccurrenceField, wrapped or bare, when
	// this provider's protocol has no such field for this event - never
	// a fallback value computed from anything outside the signed
	// payload.
	AuthenticatedOccurrenceReference(ctx context.Context, verifiedEvent any) (ProviderOccurrenceID, error)
}

// OccurrenceSourceFunc is an OccurrenceSource adapter for a plain
// function, mirroring the standard library's http.HandlerFunc pattern -
// convenient for a small/inline adapter implementation or a test double,
// without requiring a dedicated named type for every case.
type OccurrenceSourceFunc func(ctx context.Context, verifiedEvent any) (ProviderOccurrenceID, error)

// AuthenticatedOccurrenceReference implements OccurrenceSource.
func (f OccurrenceSourceFunc) AuthenticatedOccurrenceReference(ctx context.Context, verifiedEvent any) (ProviderOccurrenceID, error) {
	return f(ctx, verifiedEvent)
}

// RequiresOrdinalFunc reports whether transactionType is one of the
// caller's own multi-occurrence transaction types requiring an
// occurrence discriminator before an event may be composed/posted (ADR
// 0038 §14.1's named examples for sportsbook: partial settlement,
// cashout, rollback/re-settlement). This package makes no assumption
// about which types apply for a domain it does not own - the caller
// (the domain specialist building a specific adapter) supplies this,
// exactly mirroring how internal/risk.Evaluate's own LimitKind extension
// model requires each domain to declare its own mapping rather than
// Risk inventing one (ADR 0031 §12).
type RequiresOrdinalFunc func(transactionType string) bool

// ResolveOccurrence is the one call every provider adapter makes, for
// every lifecycle event, before composing the value it will hand to
// internal/ledger.Post (or an equivalent posting call) as
// provider_tx_id/idempotency_key (see Mode/Assign in routing.go). It
// implements S-5 and S-6's fixes together:
//
//   - S-5: if transactionType requires a discriminator, the
//     discriminator is sourced EXCLUSIVELY from src (an authenticated
//     payload field) - never inferred from a transport-level signal. If
//     src reports ErrNoAuthenticatedOccurrenceField (or src is nil),
//     ResolveOccurrence fails closed with
//     ErrOccurrenceOrdinalRequiredButUnavailable rather than inventing a
//     fallback.
//   - S-6: whatever discriminator is obtained (or none, for a single-
//     occurrence transaction type) is folded into the final opaque
//     string via ComposeOccurrenceKey's length-prefixed, losslessly-
//     decomposable encoding - never raw concatenation.
//
// reference is the adapter's own stable, pre-composition reference for
// this event (the provider's settlement reference, or - in-house mode -
// the engine's own internally-generated reference in the identical role,
// ADR 0038 §14.6).
func ResolveOccurrence(ctx context.Context, requiresOrdinal RequiresOrdinalFunc, src OccurrenceSource, transactionType string, reference string, verifiedEvent any) (composed string, err error) {
	if requiresOrdinal == nil {
		return "", fmt.Errorf("idempotency: RequiresOrdinalFunc is required")
	}
	if !requiresOrdinal(transactionType) {
		return ComposeOccurrenceKey(reference, nil)
	}
	if src == nil {
		return "", fmt.Errorf("%w: transaction type %q requires an occurrence discriminator but no OccurrenceSource was supplied",
			ErrOccurrenceOrdinalRequiredButUnavailable, transactionType)
	}
	discriminator, err := src.AuthenticatedOccurrenceReference(ctx, verifiedEvent)
	if err != nil {
		if errors.Is(err, ErrNoAuthenticatedOccurrenceField) {
			return "", fmt.Errorf("%w: %v", ErrOccurrenceOrdinalRequiredButUnavailable, err)
		}
		return "", err
	}
	if discriminator == "" {
		return "", fmt.Errorf("idempotency: authenticated occurrence reference must not be empty")
	}
	d := string(discriminator)
	return ComposeOccurrenceKey(reference, &d)
}
