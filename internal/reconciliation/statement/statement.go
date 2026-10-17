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

// CasinoFetchRequest is what the casino_statement stream hands a source.
// TenantID comes from the server-side sweep (the tenant being reconciled),
// never from a statement.
type CasinoFetchRequest struct {
	TenantID               uuid.UUID
	PeriodStart, PeriodEnd time.Time
}

// CasinoStatement is one fetched casino statement: a SNAPSHOT value, fetched
// before any database transaction opens and compared later inside the run's
// own REPEATABLE READ transaction (CAS-STMT-IO-1).
//
// CoverageEnd, when non-zero, is the instant the statement vouches for:
// the stream compares only ledger transactions created at or before it, so
// a posting that commits between the fetch and the match is out of scope
// instead of surfacing as a false "no statement line" P1 (the fetch and the
// match no longer share one snapshot). Zero means the source gives no bound
// and the whole ledger is compared (the pre-CAS-STMT-IO-1 behaviour; a test
// source that wants it leaves it zero). A real source MUST set it to the
// end of the window its provider vouches for (PROVIDER DEPENDENT).
//
// Totals may be empty: the totals match is then skipped and the run's
// audit records that.
type CasinoStatement struct {
	CoverageEnd time.Time
	Lines       []CasinoStatementLine
	Totals      []CasinoStatementTotal
}

// CasinoStatementSource supplies a tenant's casino statement to the
// casino_statement reconciliation stream (Stage 10.3 W3a,
// CAS-RECON-STMT-1). It is an interface so the source is injectable: a
// test feeds a divergent statement to prove detection, and a future real
// provider adapter replaces the MOCK without touching the stream. Real
// statement ingestion and matching are PROVIDER DEPENDENT and NOT
// IMPLEMENTED; the only source wired today is casino.MockStatementSource
// (MOCK, tautological by construction).
//
// CAS-STMT-IO-1: a source is one of exactly two shapes, and the stream
// refuses anything else:
//
//   - CasinoStatementFetcher - the provider shape, and the ONLY shape a real
//     source may have. Fetch takes NO transaction: it is provider I/O and the
//     stream calls it with no database transaction held (ADR 0095 INV-IO-1;
//     the stream refuses to call it under txscope.Held, and a source must
//     also refuse). The fetched value is then matched inside the run's
//     REPEATABLE READ transaction, which performs no I/O.
//   - CasinoLedgerRenderer - the synthetic shape: a source that derives its
//     statement from the platform's OWN ledger tables (the MOCK in-house
//     source). It reads the database, not a provider, so it renders inside
//     the match transaction (the same snapshot as the ledger side, which is
//     what makes it tautological by construction and race-free). The stream
//     accepts it only from a component that marks itself
//     providerkind.Synthetic - a real source can never take this shape, and
//     the production startup guard refuses a binary that wires a Synthetic
//     component.
type CasinoStatementSource interface {
	// Label names the source in reconciliation records, audit metadata and
	// logs. A mock source's label must contain "MOCK".
	Label() string
}

// CasinoStatementFetcher is the provider shape of CasinoStatementSource.
type CasinoStatementFetcher interface {
	CasinoStatementSource
	// Fetch returns the tenant's statement for [req.PeriodStart,
	// req.PeriodEnd). It must not write, and must refuse while a database
	// transaction is held.
	Fetch(ctx context.Context, req CasinoFetchRequest) (CasinoStatement, error)
}

// SyntheticSource is the structural twin of providerkind.Synthetic (same
// marker method, so a component satisfies both without this leaf package, or
// internal/reconciliation, importing internal/providerkind - the R3 read-only
// import guard forbids that). The casino_statement stream accepts a
// CasinoLedgerRenderer only from a SyntheticSource.
type SyntheticSource interface {
	SyntheticComponent()
}

// CasinoLedgerRenderer is the synthetic (in-house, database-derived) shape of
// CasinoStatementSource. It must not write.
type CasinoLedgerRenderer interface {
	CasinoStatementSource
	// RenderFromLedger renders the tenant's statement from the platform's own
	// casino ledger rows, inside tx (the run's REPEATABLE READ transaction).
	RenderFromLedger(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]CasinoStatementLine, []CasinoStatementTotal, error)
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
//
// Truncated (R3-OBS-BOUND-1, A11): true when the source stopped at the
// request's MaxLines bound. A truncated statement vouches for
// [CoverageStart, CoverageEnd) only, where CoverageEnd is the exclusive
// instant of the first line NOT returned: every returned line has
// OccurredAt < CoverageEnd and no line of that window is missing (a source
// never splits a group of lines sharing one instant; PagePaymentLines does
// this). The next run continues from CoverageEnd, so no line is skipped and
// none is counted twice. A truncated statement is never a "clean" claim for
// the requested window: the stream records, counts and alerts it.
type PaymentStatement struct {
	CoverageStart, CoverageEnd time.Time
	Lines                      []PaymentStatementLine
	Truncated                  bool
}

// PaymentFetchRequest is what the payment_statement stream hands a source.
// TenantID and ProviderID come from the server-side sweep (the tenant
// being reconciled and the source's own provider), never from a statement.
//
// MaxLines (R3-OBS-BOUND-1) is the per-run line bound: when positive, a source
// must return at most that many lines and, if the window holds more, a
// Truncated statement (see PaymentStatement). Zero means no per-run bound
// (the import cap MaxPaymentStatementLines still applies). ContinueFrom, when
// non-zero, is the persisted cursor: the exclusive end of the last committed
// run for this tenant and provider. The source's statement must start EXACTLY
// at it (CoverageStart == ContinueFrom: a later start is a gap that skips a
// window, an earlier start an overlap that counts lines twice, and the stream
// refuses both) and must contain no line before it (OccurredAt >=
// ContinueFrom); PeriodStart equals ContinueFrom in that case.
//
// F2 / LF-A11-2a: in continuation mode PeriodEnd is the completeness horizon,
// min(sweep end, now - the provider's settle lag); the statement's CoverageEnd
// must not exceed it (the stream refuses a statement that does). A truncated
// statement must also list its lines in non-decreasing OccurredAt order (a page
// cut by time, LF-A11-6); a page in any other order is refused.
type PaymentFetchRequest struct {
	TenantID               uuid.UUID
	ProviderID             string
	PeriodStart, PeriodEnd time.Time
	MaxLines               int
	ContinueFrom           time.Time
	// Continuation is set by the continuation (scoped) stream on every request, including the first one of a
	// pair (zero ContinueFrom): PeriodEnd is then the completeness horizon (see above) and lines are dated by
	// their last status change.
	Continuation bool
}

// ReadOnlyAttested is implemented by every NON-Synthetic payment statement source and must return true: the
// source's credential at the provider is read-only (statements / reports scope only; it cannot initiate,
// capture, refund or pay out) and the source's code only reads. It is an attestation in code (like
// PayoutLinesCarryMerchantReference), refused by the cmd/platform-api registry when absent or false, and it is
// the only thing the platform can check about the real credential: PROVIDER DEPENDENT. A real PSP source MUST
// be provisioned with a separate read-only credential at the vendor (never the payment/payout credential), and
// that provisioning is a launch checklist item (docs/plans/a11-payments-wiring-record.md). The interface
// itself gives a source no database transaction and no payment-provider handle: see
// TestPaymentStatementSource_TypeLevelReadOnly.
type ReadOnlyAttested interface {
	ReadOnlyStatementCredential() bool
}

// SettleLagDeclarer (F2 / LF-A11-2, optional) declares the provider's own reporting / completeness lag: a
// line the provider records at time T may only become visible in its statements up to that long after T. The
// continuation cursor turns a statement's CoverageEnd into a permanent completeness claim, so the stream
// requests PeriodEnd = now - lag and refuses a CoverageEnd beyond it. A declared value must lie in
// [0, MaxStatementSettleLag]; the cmd/platform-api registry refuses any other value at startup. A
// non-Synthetic source that declares nothing gets DefaultStatementSettleLag; a Synthetic (MOCK) source that
// declares nothing gets 0 (the MOCK records synchronously).
type SettleLagDeclarer interface {
	StatementSettleLag() time.Duration
}

const (
	// DefaultStatementSettleLag is the conservative lag of a real source that declares none (PROVIDER
	// DEPENDENT: the real value comes from the vendor's reporting documentation).
	DefaultStatementSettleLag = 15 * time.Minute
	// MaxStatementSettleLag bounds a declared lag (a larger value is a configuration error).
	MaxStatementSettleLag = 7 * 24 * time.Hour
)

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
