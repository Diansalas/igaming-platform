// Package statement is the contract between a reconciliation stream and
// the provider-statement source it matches against (ADR 0088 §8.4, §14
// Q3(a); Stage 10.3 W3a for the casino statement). It is a dependency-free
// leaf, so a domain package can implement a source (internal/sportsbook's
// and internal/casino's MOCK sources) and internal/reconciliation can
// consume it without either importing the other (reconciliation importing
// internal/sportsbook or internal/casino would close an import cycle
// through internal/rg -> internal/wallet, whose tests import
// reconciliation).
package statement

import (
	"context"
	"math/big"
	"time"

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

// Casino statement line kinds (CasinoStatementLine.Kind).
const (
	CasinoLineBet      = "bet"
	CasinoLineWin      = "win"
	CasinoLineRollback = "rollback"
)

// CasinoStatementLine is one line of a casino provider statement: the
// statement issuer's view of one bet, win or rollback, keyed by
// (ProviderID, ProviderTxID) - Stage 10.3 W3a, CAS-RECON-STMT-1
// (docs/plans/stage-10.3-planning/02-casino-financial-analysis.md §2.5).
//
// A tombstone is never a statement line: a provider lists its rollback
// (ProviderTxID = the rollback's own reference, OriginalProviderTxID = the
// reference it names), and when the platform never saw that original the
// ledger holds a tombstone under OriginalProviderTxID instead of a
// casino_rollback. The casino_statement stream treats that pairing as a
// match.
type CasinoStatementLine struct {
	ProviderID   string
	ProviderTxID string
	// Kind is CasinoLineBet, CasinoLineWin or CasinoLineRollback.
	Kind string
	// OriginalProviderTxID is set for a rollback only.
	OriginalProviderTxID string
	RoundID              string
	AssetCode            string
	// Amount is minor units: the stake of a bet, the payout of a win, and
	// for a rollback the amount of the original it reverses. The stream
	// compares and sums amounts as big.Int, never as int64 sums.
	Amount int64
}

// CasinoStatementTotal is an optional per-(provider, asset, period)
// aggregate: the provider's reported GGR (stakes minus payouts, net of
// rollbacks) in minor units. The stream matches it against the net
// house_gaming movement of the casino transactions carrying that
// provider_id.
type CasinoStatementTotal struct {
	ProviderID string
	AssetCode  string
	GGR        *big.Int
}

// CasinoStatementSource supplies a tenant's casino statement to the
// casino_statement reconciliation stream (Stage 10.3 W3a,
// CAS-RECON-STMT-1). It is an interface so the source is injectable: a
// test feeds a divergent statement to prove detection, and a future real
// provider adapter replaces the MOCK without touching the stream. Real
// statement ingestion and matching are PROVIDER DEPENDENT and NOT
// IMPLEMENTED; the only source wired today is casino.MockStatementSource
// (MOCK, tautological by construction).
type CasinoStatementSource interface {
	// Label names the source in reconciliation records, audit metadata and
	// logs. A mock source's label must contain "MOCK".
	Label() string
	// Statement returns the tenant's statement lines and, optionally,
	// per-(provider, asset) totals for [periodStart, periodEnd), read
	// inside tx (the run's own tenant-scoped REPEATABLE READ transaction).
	// A nil or empty totals slice means "this source reports no
	// aggregate": the totals match is then skipped and the run's audit
	// records that. It must not write.
	Statement(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, periodStart, periodEnd time.Time) ([]CasinoStatementLine, []CasinoStatementTotal, error)
}
