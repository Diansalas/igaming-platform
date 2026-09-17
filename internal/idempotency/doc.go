// Package idempotency is the platform's provider-neutral idempotency
// primitive/contract - shared scaffolding any future provider adapter
// (a second casino provider, an external or in-house sportsbook engine,
// a new PSP, etc.) must build on, per CLAUDE.md's "each integration is
// treated as a subsystem... the platform still owns the adapter,
// idempotency/retry semantics" rule and this specialist's Integrations
// charter ("owns the shared pattern; does not own individual vendor
// business logic").
//
// This package does not replace or duplicate the existing, shipped
// idempotency mechanism (ADR 0020's SAVEPOINT-based exact-retry path in
// internal/db.IdempotentInsert, consumed today by internal/ledger.Post,
// internal/withdrawal.RequestWithdrawal, and internal/payments.
// InitiateDeposit). It sits ONE LAYER BELOW that mechanism: it is how an
// adapter derives the exact string value it hands to that mechanism as
// provider_tx_id/idempotency_key, so the mechanism's own uniqueness
// constraint means what everyone assumes it means.
//
// # Why this package exists (Stage 4H-B0-R5 -> Stage 4H-B0-R6)
//
// docs/decisions/0038-sportsbook-accounting-and-ledger-integration.md
// §14/§14.6 designed a canonical idempotency contract for any product
// whose financial events are more numerous than a single bet/win pair
// (a sportsbook bet's placement, settlement, partial settlement per
// bet-builder leg, cashout, and market-correction rollback/re-settlement
// can all occur against the SAME underlying bet). That design was
// reviewed by `security` (docs/security/security-architecture.md,
// Stage 4H-B0-R5 section) and two P1-severity vulnerabilities were found
// in the architecture as documented - both closed by this package, not
// re-proposed in the flawed original shape:
//
//   - S-5: the architecture's fallback occurrence discriminator (for a
//     provider whose protocol carries no dedicated per-occurrence field)
//     derived from "the adapter's own inbound-delivery deduplication
//     record - assigned once, the first time a specific raw delivery is
//     processed." That value is NOT part of what the provider's
//     signature covers; a validly-signed body replayed as a distinct
//     transport-level delivery gets a NEW dedup record, composes to a
//     NEW key, misses the uniqueness constraint entirely, and posts as a
//     "legitimate new occurrence" - a real double-post. Closed here by
//     OccurrenceSource/ResolveOccurrence: the discriminator MUST come
//     from a field that was verified as part of the event's own
//     signature check, and ErrOccurrenceOrdinalRequiredButUnavailable is
//     returned (fail closed, never a silent transport-derived
//     substitute) when no such field exists, until
//     CanonicalOccurrenceIssuer's explicit round-trip mechanism has run.
//   - S-6: the architecture's composed key
//     ("{provider reference}#{occurrence_ordinal}") is unescaped string
//     concatenation - ref="A#1" with no ordinal and ref="A" with ordinal
//     1 both compose to "A#1", silently colliding two distinct events.
//     Closed here by ComposeOccurrenceKey/DecomposeOccurrenceKey: a
//     length-prefixed encoding that is losslessly decomposable for ANY
//     byte content in the reference, proved by
//     TestComposeOccurrenceKey_NoTwoDistinctPairsCollide.
//
// # What this package is not
//
// It does not implement a real sportsbook adapter, a real vendor
// integration, or a ledger posting call - those remain the owning
// domain specialist's work (per this specialist's "does not own
// individual vendor business logic" limitation) built on top of the
// primitives here. It does not change internal/casino's already-shipped
// behavior - internal/casino/mock.go's HandleCallback already verifies
// an HMAC signature over the full payload (including provider_tx_id)
// BEFORE returning a CallbackEvent, which is exactly the "authenticated
// per-occurrence field" property OccurrenceSource requires; casino has
// no multi-occurrence transaction type today (bet and win each occur at
// most once per round), so it has no present need to compose an
// occurrence discriminator at all, and is left unchanged.
package idempotency
