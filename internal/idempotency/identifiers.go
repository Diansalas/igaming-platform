package idempotency

import "github.com/google/uuid"

// This file defines the canonical identifier types ADR 0038 §14.1 names
// and Stage 4H-B0-R6's directive requires explicitly: platform operation
// id, provider operation id, provider event/occurrence id (authenticated-
// payload-sourced only, never transport-derived), occurrence ordinal/
// version, and correction/reversal identifiers. None of these introduce a
// new database column - every one already has a concrete home on
// ledger.TransactionInput/ledger_transactions (ADR 0038 §14.1's own
// "no new column" statement), or is deliberately not persisted at all
// (OccurrenceOrdinal - see its own doc comment).

// PlatformOperationID is the platform's own identity for one specific,
// already-posted financial operation - internal/ledger.LedgerTransaction.
// Id's role (ADR 0038 §14.1 item 1). It does not exist before the row is
// inserted, so it is never an input to that same insert's idempotency
// decision; its role is downstream - what CorrectionID/ReversalID point
// at, and what audit/reconciliation join on once a transaction exists.
type PlatformOperationID = uuid.UUID

// ProviderOperationID is the provider's (or, for an in-house engine, the
// engine's own internally-minted, identical-role) reference for ONE
// specific lifecycle event, opaque, never a provider type or enum
// (ledger-accounting-model.md §1.2). This is the pre-composition raw
// value - see ProviderReference for the type used at the composition
// boundary. It plays the role of ledger_transactions.provider_tx_id in
// external-provider mode, or the semantically-equivalent
// idempotency_key in in-house mode (ADR 0038 §14.6's routing rule - see
// Mode/Assignment in routing.go).
type ProviderOperationID string

// ProviderReference is the raw, pre-composition string an adapter has
// available for one lifecycle event before ComposeOccurrenceKey folds in
// an occurrence discriminator, if one is required. Kept as a distinct
// type from ProviderOperationID (rather than reusing it) so a reviewer
// can see, from the type alone, which value is "the thing about to be
// composed" versus "the final opaque value already handed to
// ledger.Post" - the exact distinction the S-6 vulnerability blurred by
// treating both as interchangeable raw strings.
type ProviderReference string

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
// provider's own per-occurrence signal, or - only via the explicit
// CanonicalOccurrenceIssuer round trip, never a silent transport-derived
// substitute - a platform-minted canonical occurrence id the provider
// echoes back and later signs.
//
// It has no dedicated database column (ADR 0038 §14.1/§14.5: "no new
// LedgerTransaction or LedgerEntry column is introduced"). Its value is
// carried entirely inside the composed ProviderOperationID string
// produced by ComposeOccurrenceKey.
type OccurrenceOrdinal int64

// CorrelationID ties every transaction one business operation (a bet, a
// deposit, a withdrawal) ever produces together - "find this operation's
// whole history," never part of any uniqueness constraint (ADR 0038
// §14.1 item 1, ledger-accounting-model.md §1.2). Two transactions may
// share a CorrelationID freely.
type CorrelationID = uuid.UUID

// CorrectionID identifies the PlatformOperationID of a transaction being
// corrected by a new, later transaction - the value a correction's
// ReversesTransactionID field is set to (ADR 0038 §10/§14.2 cases 4/5).
// A correction and a CorrectionID are never an edit or deletion of the
// original row (CLAUDE.md's "corrections are compensating entries"
// rule) - this identifier is purely the traceability pointer.
type CorrectionID = uuid.UUID

// ReversalID is the identical mechanism as CorrectionID (ADR 0038 §14.2
// case 5: "the one and only correction/reversal traceability
// mechanism... generalized, not reinvented"), named separately per this
// stage's explicit requirement to define both a correction and a
// reversal identifier, so a caller's own signature can say which of the
// two business meanings a given reference carries without inventing a
// second underlying mechanism.
type ReversalID = uuid.UUID
