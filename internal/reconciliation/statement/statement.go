// Package statement is the contract between a reconciliation stream and
// the provider-statement source it matches against (ADR 0088 §8.4, §14
// Q3(a)). It is a dependency-free leaf, so a domain package can implement
// a source (internal/sportsbook's MOCK source) and internal/reconciliation
// can consume it without either importing the other (reconciliation
// importing internal/sportsbook would close an import cycle through
// internal/rg -> internal/wallet, whose tests import reconciliation).
package statement

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SportsbookSettlementLine is one line of a sportsbook settlement
// statement: the statement issuer's view of one bet's current settlement
// state. A line is either a settlement (Generation, Outcome, PayoutAmount
// set; Void false) or a void (Void true, Generation nil, Outcome empty,
// PayoutAmount 0). A bet with no current state (open, including open
// again after a rollback or holding only a tombstone) has no line.
type SportsbookSettlementLine struct {
	BetID        uuid.UUID
	Generation   *int
	Outcome      string
	PayoutAmount int64
	AssetCode    string
	Void         bool
}

// SportsbookSettlementSource supplies a tenant's sportsbook settlement
// statement to the sportsbook_settlement reconciliation stream. It is an
// interface so the source is injectable: a test feeds a divergent
// statement to prove detection, and a future real provider adapter
// replaces the mock without touching the stream.
type SportsbookSettlementSource interface {
	// Label names the source in reconciliation records, audit metadata and
	// logs. A mock source's label must contain "MOCK".
	Label() string
	// StatementLines returns the statement for tenantID, read inside tx
	// (the reconciliation run's own tenant-scoped transaction). It must
	// not write.
	StatementLines(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]SportsbookSettlementLine, error)
}
