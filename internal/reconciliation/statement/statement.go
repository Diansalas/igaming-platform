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

// Payment statement line kinds (PaymentStatementLine.Kind) and statuses
// (PaymentStatementLine.Status) - ADR 0095 §9.5, PRH-I5.
const (
	PaymentLineDeposit         = "deposit"
	PaymentLineDepositReversal = "deposit_reversal"
	PaymentLinePayout          = "payout"

	PaymentStatusPending   = "pending"
	PaymentStatusSucceeded = "succeeded"
	PaymentStatusDeclined  = "declined"
	PaymentStatusReversed  = "reversed"
)

// Size caps a payment statement is held to (ADR 0095 §12.1 step 1,
// S95-C11). MaxPaymentStatementLines is enforced by the payment_statement
// stream after Fetch (above it the import is refused, nothing is stored,
// and the failure is audited and logged as a P1) and by migration 0102's
// line_count CHECK. MaxPaymentStatementBodyBytes binds a REAL source that
// reads a statement over the wire: it must stop reading and fail at this
// many bytes rather than buffer an unbounded body (the MOCK source has no
// wire body). Both are enforced while streaming by the helpers in
// payment_limits.go (security condition C1), which every source must use.
const (
	MaxPaymentStatementLines     = 1_000_000
	MaxPaymentStatementBodyBytes = 256 << 20
)

// PaymentStatementLine is one line of a payment provider statement: the
// provider's view of one deposit, deposit reversal or payout (ADR 0095
// §9.5). References are the provider's own; MerchantReference is the
// platform's attempt merchant reference as the provider echoes it;
// SettlementReference is a payout's Step B settlement (send-confirmation)
// reference when the provider reports one (LF95-C13).
type PaymentStatementLine struct {
	ProviderID                string
	ProviderReference         string
	MerchantReference         string
	OriginalProviderReference string
	SettlementReference       string
	// Kind is PaymentLineDeposit, PaymentLineDepositReversal or
	// PaymentLinePayout.
	Kind string
	// Status is PaymentStatusPending, _Succeeded, _Declined or _Reversed.
	Status string
	// Amount is minor units; the stream compares it as big.Int.
	Amount     int64
	AssetCode  string
	OccurredAt time.Time
}

// PaymentStatement is one fetched statement. [CoverageStart, CoverageEnd)
// is the window the provider vouches for: platform records outside it are
// never flagged as missing (ADR 0095 §12.3 "Coverage window").
type PaymentStatement struct {
	CoverageStart, CoverageEnd time.Time
	Lines                      []PaymentStatementLine
}

// PaymentFetchRequest is what the payment_statement stream hands a source.
// TenantID and ProviderID come from the server-side sweep (the tenant
// being reconciled and the source's own provider), never from a statement.
type PaymentFetchRequest struct {
	TenantID               uuid.UUID
	ProviderID             string
	PeriodStart, PeriodEnd time.Time
}

// PaymentStatementSource supplies one provider's statement for one tenant
// to the payment_statement reconciliation stream (ADR 0095 §9.5, §12).
//
// Unlike CasinoStatementSource it takes NO transaction: Fetch is provider
// I/O and the stream calls it with no transaction held (ADR 0095
// INV-IO-1; the stream refuses to call it under txscope.Held, and a
// source must also refuse). The fetched statement is persisted by the
// stream in its own short ingest transaction and matched later from the
// stored copy only (§12.1). This is the CAS-STMT-IO-1 fix applied to
// payments.
//
// Deviation from the ADR 0095 §9.5 sketch, recorded in the ADR's
// implementation record: the sketch passes a payments.CallContext. This
// leaf cannot import internal/payments (import cycle, see the package
// comment), so the request carries only the server-side tenant and
// provider; an implementation in internal/payments builds the real
// CallContext itself, resolving the tenant's outbound credential through
// the provider-call gate (§3.2, §11). payments.MockStatementSource does.
type PaymentStatementSource interface {
	// Label names the source in import rows, reconciliation records, audit
	// metadata and logs. A synthetic source's label must contain "MOCK"
	// (the stream and migration 0102 both refuse otherwise).
	Label() string
	// ProviderID is the one provider whose statement this source fetches.
	// Every line must carry it; a line of any other provider refuses the
	// whole import (ADR 0095 INV-IO-14, S95-C1).
	ProviderID() string
	// Fetch returns the statement for req. It must not be called, and
	// must refuse, while a database transaction is held. A source that
	// reads a wire body MUST read it through LimitPaymentStatementBody and
	// collect lines with a PaymentLineCollector (or use
	// DecodePaymentStatementJSONLines), returning their sentinels
	// unwrapped-compatible (errors.Is) so the stream refuses the import
	// (security condition C1).
	Fetch(ctx context.Context, req PaymentFetchRequest) (PaymentStatement, error)
}
