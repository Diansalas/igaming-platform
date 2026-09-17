package idempotency

import "fmt"

// This file implements ADR 0038 §14.6's in-house-vs-external-provider
// routing rule generically (the directive's explicit "in-house vs.
// external-provider routing" requirement) - which database-enforced
// uniqueness mechanism a composed occurrence key must be submitted
// through, given the two shapes the platform's existing schema already
// supports (migration 0021):
//
//   - UNIQUE (tenant_id, provider_id, provider_tx_id)
//     WHERE provider_id IS NOT NULL   (external-provider mode)
//   - UNIQUE (tenant_id, idempotency_key)                      (always
//     evaluated - the standard path for in-house-mode postings, ADR 0038
//     §14.6's Option 2)
//
// No new column or constraint is introduced here - this is a routing
// helper over the existing two mechanisms, not a third one.

// Mode is which uniqueness mechanism a composed occurrence key routes
// through.
type Mode int

const (
	// ModeExternalProvider is a real external vendor integration - the
	// composed key is submitted as provider_tx_id, alongside a non-nil,
	// non-empty provider_id (ADR 0033 §2: never a reserved sentinel for
	// the OTHER mode - see ModeInHouse).
	ModeExternalProvider Mode = iota
	// ModeInHouse is the platform's own in-house engine originating the
	// event with no external counterpart - provider_id/provider_tx_id
	// stay NULL (ADR 0033 §2, ADR 0038 §14.6 Option 2: "NULL... never a
	// reserved sentinel"), and the composed key is submitted as
	// idempotency_key instead, under the unconditional
	// UNIQUE (tenant_id, idempotency_key) constraint.
	ModeInHouse
)

// String implements fmt.Stringer for readable audit/log output.
func (m Mode) String() string {
	switch m {
	case ModeExternalProvider:
		return "external_provider"
	case ModeInHouse:
		return "in_house"
	default:
		return fmt.Sprintf("idempotency.Mode(%d)", int(m))
	}
}

// Assignment is where a composed occurrence key lands on the resulting
// posting call's identity fields (internal/ledger.TransactionInput's own
// ProviderID/ProviderTxID/IdempotencyKey fields, named generically here
// since this package must not import internal/ledger - Integrations owns
// the shared pattern, not the ledger schema). Exactly one of
// (ProviderID+ProviderTxID) or IdempotencyKey is the OPERATIVE uniqueness
// check for a given Assignment - see Mode's own doc comment - but every
// field required NOT NULL by the existing schema (IdempotencyKey is
// NOT NULL on every row, per migration 0021) is always populated, never
// left as a zero value that would fail an insert.
type Assignment struct {
	Mode Mode
	// ProviderID is set (non-nil, non-empty) only for ModeExternalProvider;
	// always nil for ModeInHouse (ADR 0038 §14.6: never a reserved
	// sentinel string).
	ProviderID *string
	// ProviderTxID is the composed occurrence key for ModeExternalProvider;
	// always nil for ModeInHouse (never half-populated while ProviderID is
	// nil - ADR 0038 §14.6's own "a row with provider_id IS NULL and
	// provider_tx_id populated would be meaningless" rule).
	ProviderTxID *string
	// IdempotencyKey is the composed occurrence key for ModeInHouse. For
	// ModeExternalProvider it is populated (the column is NOT NULL) but
	// is NOT the operative uniqueness check for that row - callers must
	// supply a value here that is never reused as an idempotency input
	// elsewhere (ADR 0038 §14.6's routing table), e.g. by namespacing it
	// with the provider id the same way internal/casino's own postBet
	// already does (`providerID + ":" + event.ProviderTxID`).
	IdempotencyKey string
}

// Assign builds an Assignment for a composed occurrence key
// (ComposeOccurrenceKey/ResolveOccurrence's output), given mode and the
// two mode-specific inputs:
//
//   - providerID is required for ModeExternalProvider (the adapter's
//     configured provider identifier) and ignored for ModeInHouse.
//   - externalIdempotencyKey is the ModeExternalProvider-only value used
//     to satisfy the schema's NOT NULL idempotency_key column without
//     making it the operative check (see IdempotencyKey's own doc
//     comment) - ignored for ModeInHouse, where composed itself becomes
//     IdempotencyKey directly.
func Assign(mode Mode, composed string, providerID string, externalIdempotencyKey string) (Assignment, error) {
	if composed == "" {
		return Assignment{}, fmt.Errorf("idempotency: composed occurrence key is required")
	}
	switch mode {
	case ModeExternalProvider:
		if providerID == "" {
			return Assignment{}, fmt.Errorf("idempotency: provider id is required for %s", ModeExternalProvider)
		}
		if externalIdempotencyKey == "" {
			return Assignment{}, fmt.Errorf("idempotency: an external-mode idempotency_key value is required (NOT NULL column, must not be reused as an idempotency input elsewhere - see Assignment.IdempotencyKey)")
		}
		pid, ptx := providerID, composed
		return Assignment{Mode: mode, ProviderID: &pid, ProviderTxID: &ptx, IdempotencyKey: externalIdempotencyKey}, nil
	case ModeInHouse:
		return Assignment{Mode: mode, ProviderID: nil, ProviderTxID: nil, IdempotencyKey: composed}, nil
	default:
		return Assignment{}, fmt.Errorf("idempotency: unknown mode %v", mode)
	}
}
