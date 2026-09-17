package idempotency

// This file defines the identifier types ADR 0038 §14.1 names that are
// actually distinct as Go types with real compile-time value - i.e. they
// appear in a real, load-bearing function signature somewhere in this
// package (OccurrenceSource.AuthenticatedOccurrenceReference,
// ComposeOccurrenceKeyWithOrdinal), not merely named in prose.
//
// Stage 4H-B0-R6-R1's code-reviewer/QA findings removed several other
// identifier "types" this file originally declared: four were plain
// `type X = uuid.UUID` aliases (PlatformOperationID, CorrelationID,
// CorrectionID, ReversalID) that carried zero compile-time distinction
// from uuid.UUID or from each other, and two more (ProviderOperationID,
// ProviderReference) were distinct declared types that nonetheless never
// appeared in any function signature in this package - every real call
// site uses a plain `string`. None of the removed names had a caller;
// they described vocabulary from ADR 0038 §14.1's prose without doing
// any work in compiled code. That vocabulary still exists in ADR 0038
// itself for anyone who needs the conceptual mapping; it does not need a
// duplicate, unused Go declaration here. If a future adapter's
// implementation genuinely needs one of these as a distinct parameter or
// return type, it should be reintroduced there, at the call site that
// needs it - not spring back into being spun up ahead of any use.
//
// Neither of the two types kept below introduces a new database column -
// ProviderOccurrenceID is never persisted on its own (it is folded into
// the composed string ComposeOccurrenceKey produces), and OccurrenceOrdinal
// has no dedicated column either (ADR 0038 §14.1/§14.5: "no new
// LedgerTransaction or LedgerEntry column is introduced").

// ProviderOccurrenceID is the authenticated, per-occurrence discriminator
// for one specific occurrence of a multi-occurrence transaction type
// (ADR 0038 §14.1 item 3's "provider occurrence/event ID"; the
// directive's "provider event/occurrence ID (authenticated-payload-
// sourced only, never transport-derived)"). It is produced exclusively
// by OccurrenceSource.AuthenticatedOccurrenceReference, sourced from a
// field that was part of what the provider's own signature/
// authentication check covered for that specific event - NEVER from a
// transport-level delivery/receipt/dedup observation (the S-5 fix). It
// may be the string form of a numeric OccurrenceOrdinal (when the
// provider's protocol supplies one directly - a leg index, a settlement
// sequence number, a cashout attempt counter) or a provider-committed
// event/message id (the fallback tier the directive names for a
// provider with a signed field that is not itself a dedicated ordinal).
type ProviderOccurrenceID string

// OccurrenceOrdinal is a strictly increasing integer scoped to
// (tenant_id, correlation_id, transaction_type), required for every
// transaction type where more than one occurrence may legitimately exist
// against the same business operation (ADR 0038 §14.1 item 3:
// sportsbook_partial_settlement, sportsbook_cashout, and
// sportsbook_rollback/its paired re-settlement are the concrete named
// examples; this package makes no assumption about which types apply for
// a domain it does not own - see RequiresOrdinalFunc).
//
// It is NEVER computed as "count existing rows for this
// (correlation_id, transaction_type) and add one" (a check-then-insert
// race, and not reproducible by a legitimate retry - ADR 0038 §14.1).
// It is derived from something intrinsic to the specific occurrence: the
// provider's own per-occurrence signal (a leg index, a settlement
// sequence number, a cashout attempt counter), obtained via
// OccurrenceSource exactly like any other authenticated discriminator.
// ADR 0038 §14.1 also names a platform-minted canonical-id round trip as
// a fallback tier for a provider with no such field at all; this package
// does not yet ship a concrete mechanism for that tier - it is future
// work for whichever adapter first needs it, not a general-purpose
// interface built ahead of any caller.
//
// It has no dedicated database column (ADR 0038 §14.1/§14.5: "no new
// LedgerTransaction or LedgerEntry column is introduced"). Its value is
// carried entirely inside the composed string ComposeOccurrenceKeyWithOrdinal
// produces.
type OccurrenceOrdinal int64
