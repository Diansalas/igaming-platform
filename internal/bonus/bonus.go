// Package bonus implements the storage layer for the Bonus Engine domain
// model (docs/architecture/10-bonus-engine-architecture.md, "doc 10") -
// Campaign, Offer, Grant, WageringProgress, HeldDisposition, Suggestion,
// and BulkGrantJob/Item.
//
// STATUS: schema and plumbing only (Stage 4H-B1 Wave 2 Phase 2, following
// Phase 1's ledger-finance financial substrate - migrations 0050-0052,
// internal/ledger/bonus_mirror.go, internal/money). This package provides
// Go types mirroring each table and a thin repository layer
// (Create/Get/List/Update-by-allowed-transition). It deliberately
// implements NONE of the following, all explicitly Phase 3
// (bonus-engine)'s job:
//
//   - Grant lifecycle state-machine enforcement (which transitions are
//     legal from which state, doc 10 §1.3/T.1-T.13/N1.4).
//   - Eligibility evaluation (AssetAuthorization/RG/Risk calls, T.1/T.3).
//   - Wagering-progress computation (P_net/P_firm derivation, §6.6.5/
//     §6.6.6 of ledger-accounting-model.md) - this package stores the
//     append-only contribution record only, never derives progress from
//     it.
//   - bonus-type-specific business rules (the five bonus types' actual
//     calculation/completion logic).
//   - HTTP handlers.
//   - The EconomicOperationIdentity consume/lock-ordering logic itself
//     (internal/economicop provides the same schema-only scope for that
//     mechanism).
//
// Every monetary field is *big.Int (NUMERIC(38,0)), never int64/
// float64, matching Phase 1's internal/money conventions (CLAUDE.md).
//
// Segmentation (internal/segment) is explicitly out of this Wave's
// scope (known Kleene-logic polarity bug, unpinned member_of, per the
// Orchestrator's standing scope decision) - every segment reference in
// this package is a nullable (segment_id, segment_version_id) pair with
// no segment-evaluation logic anywhere in this package.
package bonus

import "github.com/google/uuid"

// ActorType mirrors internal/audit.ActorType verbatim (doc 10 N2.3:
// "there is no 'provider' value, and no domain invents a parallel
// enum"). Declared locally as a string type rather than importing
// internal/audit's own type alias, to avoid this storage package
// depending on the audit package for a type it only needs as a closed
// string vocabulary; the CHECK constraint on every *_actor_type column
// enforces the same four values at the database layer regardless.
type ActorType string

const (
	ActorPlayer  ActorType = "player"
	ActorStaff   ActorType = "staff"
	ActorService ActorType = "service"
	ActorSystem  ActorType = "system"
)

// SegmentReference is the nullable (segment_id, segment_version_id) pair
// used everywhere doc 10 §W7 requires "a segment reference, never an
// inlined criteria blob" - Campaign/Offer eligibility axes, Grant's own
// audit snapshot, BulkGrantJob targeting. Both fields are nil together
// (no segment reference) or set together; this package never evaluates
// segment membership.
type SegmentReference struct {
	SegmentID        *uuid.UUID
	SegmentVersionID *uuid.UUID
}
