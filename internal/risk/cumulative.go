package risk

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ErrUnsupportedCumulativeOperation is returned when a LimitCumulativeAmount
// rule matches a request whose Operation has no cumulativeSpec - a
// configuration the evaluator cannot compute, surfaced as an error rather
// than silently treated as "no usage yet" (fail-closed, ADR 0031 §33).
var ErrUnsupportedCumulativeOperation = errors.New("risk: cumulative_amount is not supported for this operation")

// ErrInvalidCumulativeSpec is returned when an operation's own
// cumulativeSpec is incomplete (no transaction type, no measured account
// type, or no/unknown consuming direction). A spec that omits any of the
// facts needed to measure usage must never fall back to a default - the
// default is exactly what produced ADR 0031 §32(a)'s fail-open.
var ErrInvalidCumulativeSpec = errors.New("risk: incomplete cumulative usage specification for this operation")

// ErrUnrecognizedCumulativeLeg is returned when the cumulative-usage
// query observes a player-side ledger_accounts.account_type that the
// operation's spec declares neither as measured nor as deliberately
// ignored. This is the self-defending half of ADR 0031 §33: if the
// ledger's posting shape for an operation changes underneath Risk (a
// bonus-funded stake leg, a new hold account, a widened transaction-type
// set with a different leg shape), the evaluation stops instead of
// silently under-counting usage against the limit.
var ErrUnrecognizedCumulativeLeg = errors.New("risk: cumulative usage query observed an undeclared player-side ledger leg")

// ledgerDirection is a ledger_entries.direction value. Declared here (not
// imported from internal/ledger) because internal/risk deliberately has
// no dependency on the ledger package - it reads ledger_entries through
// the same transaction the guarded operation posts in, and must not be
// able to post anything itself.
type ledgerDirection string

const (
	directionDebit  ledgerDirection = "debit"
	directionCredit ledgerDirection = "credit"
)

// cumulativeSpec declares everything the evaluator needs in order to
// measure one operation's cumulative usage from ledger_entries without
// assuming anything about that operation's posting shape (ADR 0031 §33,
// closing §32(a)).
//
// The defect this replaces: the previous implementation summed
// (debit - credit) over ledger_entries filtered only by tenant/player/
// asset/transaction_type, with no join to ledger_accounts and therefore
// no awareness of account_type. That is correct ONLY for a posting shape
// whose single player-owned leg is the one being measured (casino_bet,
// whose house_gaming counterparty is wallet-less). For any transaction
// posting two player-owned legs - sportsbook_bet's
// Dr player_cash / Cr player_locked, or withdrawal step A's
// Dr player_cash / Cr player_withdrawal_hold - player_account_id is
// denormalized identically onto BOTH legs and they cancel, so usage
// computed as zero no matter how much was staked: a fail-OPEN in the one
// limit kind that most needs to fail closed.
//
// Adding an entry to operationCumulativeSpecs therefore requires stating
// the posting shape explicitly; there is no shape the evaluator assumes.
type cumulativeSpec struct {
	// TransactionTypes are the ledger_transactions.transaction_type
	// value(s) this operation itself posts.
	TransactionTypes []string
	// ReversalTypes are the type(s) that RETRACT the operation and so
	// un-consume the player's capacity (casino_rollback for casino_bet):
	// a voided round must not permanently consume a rolling-window cap.
	// They are summed together with TransactionTypes, with their own
	// (opposite) direction providing the negative sign - no special
	// casing.
	ReversalTypes []string
	// MeasuredAccountTypes are the player-side ledger_accounts.
	// account_type value(s) whose entries ARE the usage being limited.
	// Required and non-empty.
	MeasuredAccountTypes []string
	// IgnoredAccountTypes are player-side legs this posting shape is
	// KNOWN to also write and which must NOT be counted (a future
	// sportsbook_bet's player_locked counterparty, a withdrawal's
	// player_withdrawal_hold counterparty). Declaring them is what makes
	// ErrUnrecognizedCumulativeLeg a meaningful signal rather than noise.
	IgnoredAccountTypes []string
	// ConsumingDirection is the ledger_entries.direction that CONSUMES
	// capacity: debit for a stake or a withdrawal, credit for a deposit
	// or a payout. Entries in the opposite direction on a measured
	// account subtract, which is what nets a reversal back out.
	ConsumingDirection ledgerDirection
}

// operationCumulativeSpecs maps a risk Operation to its cumulative-usage
// measurement spec. ONLY operations an enforcement point actually calls
// today have an entry (ADR 0031 §7/§31): a cumulative rule configured for
// an operation with no entry fails closed with
// ErrUnsupportedCumulativeOperation, never "no usage yet".
//
// Wiring a NEW operation needs three facts about its posting shape, all
// of them ledger-finance's to supply (ADR 0031 §33 restates §16 step 5
// accordingly) - the ledger transaction type ALONE is no longer enough:
// which player-side leg measures the usage, which player-side legs exist
// but must not be counted, and which direction consumes capacity. For
// reference, the two shapes already specified by other ADRs would be:
//
//	sportsbook_bet (ADR 0038 §13, NOT wired here - no internal/sportsbook
//	exists): {[sportsbook_bet], [sportsbook_void],
//	measured=[player_cash], ignored=[player_locked], debit}
//	withdrawal step A (Flow 3, NOT wired here - no withdrawal Risk call
//	site exists): {[withdrawal_requested], [...], measured=[player_cash],
//	ignored=[player_withdrawal_hold], debit}
//
// Neither is added by this stage; they are written down only so the next
// author does not have to re-derive the shape from the flows document.
var operationCumulativeSpecs = map[Operation]cumulativeSpec{
	OperationCasinoBet: {
		TransactionTypes: []string{"casino_bet"},
		// A rolled-back bet must not permanently consume the player's
		// cumulative capacity (financial correctness review finding,
		// preserved exactly): postRollback posts an inverted (credit)
		// entry against the SAME player_cash account, so a voided round
		// nets to zero.
		ReversalTypes: []string{"casino_rollback"},
		// casino's postBet debits player_cash only. If a future Bonus
		// Engine ever funds a stake from player_bonus, that leg must be
		// added here DELIBERATELY - until it is,
		// ErrUnrecognizedCumulativeLeg stops the evaluation rather than
		// letting the cap under-count (ADR 0031 §33).
		MeasuredAccountTypes: []string{"player_cash"},
		// house_gaming is wallet-less, so it never appears in this query
		// at all (ledger_entries.player_account_id is NULL on that leg) -
		// nothing to ignore for casino_bet's shape.
		IgnoredAccountTypes: nil,
		ConsumingDirection:  directionDebit,
	},
	// OperationBonusConversion's posting shape, per ledger-finance's own
	// authoritative specification (ledger-accounting-model.md §7.6, ADR
	// 0032 §4): "Caller supplies Dr player_bonus X . Cr player_cash X;
	// the generator supplies Cr promo_liability X . Dr bonus_expense X"
	// (or Dr provider_payable X for a provider-funded Grant) - a single
	// transaction_type=bonus_conversion posting with exactly two
	// PLAYER-OWNED legs (player_bonus debited, player_cash credited) and
	// two house-level, wallet-less mirror legs (promo_liability,
	// bonus_expense/provider_payable) that carry no player_account_id at
	// all and therefore - identically to casino_bet's house_gaming
	// counterparty above - never appear in this query's result set; no
	// IgnoredAccountTypes entry is needed for them.
	//
	// Measured on the CREDIT to player_cash, consistent with this file's
	// own documented convention ("credit for a deposit or a payout") -
	// bonus_conversion is a payout-shaped release of previously
	// non-withdrawable value into cash, not a stake. player_bonus (the
	// same posting's DEBIT leg, same amount, same asset) IS a
	// player-owned leg this shape touches and MUST therefore be declared
	// - as Ignored, not Measured - or every conversion would trip
	// ErrUnrecognizedCumulativeLeg.
	//
	// ReversalTypes deliberately OMITTED, not overlooked: ADR 0032 §7's
	// `bonus_reversal` is a single generic "this posting should never
	// have existed" transaction_type shared across bonus_grant/
	// bonus_conversion/bonus_forfeiture (ledger-accounting-model.md
	// §6.6.15, §7.7's own table), not an exclusive per-operation reversal
	// the way casino_rollback exclusively reverses casino_bet. Netting
	// EVERY bonus_reversal transaction_type row into THIS operation's
	// cumulative usage would silently fold in reversals of an unrelated
	// bonus_grant or bonus_forfeiture (whose ledger legs do not even
	// match this spec's MeasuredAccountTypes/IgnoredAccountTypes
	// declaration), which is exactly the kind of undeclared-shape
	// mis-measurement ADR 0031 §33 exists to prevent. Omitting it is the
	// conservative, fail-closed direction: a genuinely-reversed
	// conversion still counts toward the player's cumulative cap until a
	// future change gives `bonus_reversal` a per-original-transaction-
	// type-aware query shape (it never causes an UNDER-count, which is
	// the direction that would actually be unsafe here).
	OperationBonusConversion: {
		TransactionTypes:     []string{"bonus_conversion"},
		ReversalTypes:        nil,
		MeasuredAccountTypes: []string{"player_cash"},
		IgnoredAccountTypes:  []string{"player_bonus"},
		ConsumingDirection:   directionCredit,
	},
}

// validate fails closed on an incomplete spec. Called on every evaluation
// (it is three length checks) rather than only at init, so a spec injected
// by a test - or added by a future author who omitted a field - is
// rejected at the exact point it would otherwise mis-measure.
func (s cumulativeSpec) validate() error {
	if len(s.TransactionTypes) == 0 {
		return fmt.Errorf("%w: no ledger transaction type declared", ErrInvalidCumulativeSpec)
	}
	if len(s.MeasuredAccountTypes) == 0 {
		return fmt.Errorf("%w: no measured player-side account_type declared", ErrInvalidCumulativeSpec)
	}
	if s.ConsumingDirection != directionDebit && s.ConsumingDirection != directionCredit {
		return fmt.Errorf("%w: consuming direction %q is not 'debit' or 'credit'", ErrInvalidCumulativeSpec, s.ConsumingDirection)
	}
	return nil
}

// allTransactionTypes is the full set of ledger transaction types a
// cumulative check nets together: the operation's own plus its reversals.
func (s cumulativeSpec) allTransactionTypes() []string {
	out := make([]string, 0, len(s.TransactionTypes)+len(s.ReversalTypes))
	out = append(out, s.TransactionTypes...)
	out = append(out, s.ReversalTypes...)
	return out
}

func containsAccountType(list []string, accountType string) bool {
	for _, a := range list {
		if a == accountType {
			return true
		}
	}
	return false
}

// numericToBigInt converts a scanned NUMERIC(38,0) column to *big.Int,
// never through int64 or float64 - a SUM of many ledger entries for an
// 18-exponent asset can legitimately exceed int64's range (CLAUDE.md's
// own crypto-precision rule), and a silent overflow here would fail OPEN
// exactly where a cumulative limit most needs to fail closed.
// A positive Exp is normal and must be handled, NOT rejected: PostgreSQL
// returns a NUMERIC sum in whatever scale it likes, so an exact integer
// total of 1000 arrives as Int=1, Exp=3. The pre-Stage-4H-B0-R6 version
// of this function rejected any non-zero Exp outright, which made a
// cumulative usage total of exactly 1000 (or any other value PostgreSQL
// chose to normalize) fail the whole evaluation - fail-CLOSED, so not a
// correctness hole, but an availability defect that surfaced the moment
// this workstream's own regression test posted a round number. A NEGATIVE
// Exp is still refused: a fractional count of minor units cannot exist in
// this schema, and truncating or rounding one would be exactly the silent
// money-mangling CLAUDE.md's no-floating-point rule exists to prevent.
func numericToBigInt(n pgtype.Numeric) (*big.Int, error) {
	if !n.Valid || n.Int == nil {
		return big.NewInt(0), nil
	}
	if n.Exp < 0 {
		return nil, fmt.Errorf("risk: refusing a fractional NUMERIC minor-unit value (exponent %d)", n.Exp)
	}
	if n.Exp == 0 {
		return n.Int, nil
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n.Exp)), nil)
	return new(big.Int).Mul(n.Int, scale), nil
}

// cumulativeUsage returns the player's already-consumed usage for spec
// inside the window starting at windowStart, measured PER LEDGER LEG.
//
// tx must already be tenant-scoped (Evaluate verifies this before any
// rule is evaluated). The query joins ledger_accounts - the join whose
// absence was ADR 0031 §32(a)'s latent fail-open - and groups by
// account_type so the evaluator can both (a) count only the legs the spec
// measures and (b) refuse to proceed when it observes a player-side leg
// the spec never declared.
func cumulativeUsage(ctx context.Context, tx pgx.Tx, spec cumulativeSpec, req RiskRequest, windowStart time.Time) (*big.Int, error) {
	rows, err := tx.Query(ctx,
		`SELECT la.account_type,
		        COALESCE(SUM(CASE WHEN le.direction = $6 THEN le.amount ELSE -le.amount END), 0)
		 FROM ledger_entries le
		 JOIN ledger_transactions lt ON lt.id = le.ledger_transaction_id
		 JOIN ledger_accounts la ON la.id = le.ledger_account_id AND la.tenant_id = le.tenant_id
		 WHERE le.tenant_id = $1
		   AND le.player_account_id = $2
		   AND le.asset_code = $3
		   AND lt.transaction_type = ANY($4)
		   AND le.created_at >= $5
		 GROUP BY la.account_type`,
		req.TenantID, req.PlayerAccountID, req.AssetCode, spec.allTransactionTypes(), windowStart, string(spec.ConsumingDirection),
	)
	if err != nil {
		return nil, fmt.Errorf("risk: query cumulative usage: %w", err)
	}
	defer rows.Close()

	total := big.NewInt(0)
	for rows.Next() {
		var accountType string
		// Scanned as NUMERIC via pgtype.Numeric, never int64 - the SUM of
		// many NUMERIC(38,0) entries can legitimately exceed int64's range
		// for an 18-exponent asset, and a naive int64 SUM would silently
		// wrap and fail OPEN exactly where this rule most needs to fail
		// closed (financial + security specialist review finding).
		var netNumeric pgtype.Numeric
		if err := rows.Scan(&accountType, &netNumeric); err != nil {
			return nil, fmt.Errorf("risk: scan cumulative usage: %w", err)
		}
		switch {
		case containsAccountType(spec.MeasuredAccountTypes, accountType):
			net, err := numericToBigInt(netNumeric)
			if err != nil {
				return nil, fmt.Errorf("risk: convert cumulative usage: %w", err)
			}
			total.Add(total, net)
		case containsAccountType(spec.IgnoredAccountTypes, accountType):
			// A known counterparty leg of this posting shape - excluded
			// by declaration, not by accident.
		default:
			return nil, fmt.Errorf("%w: operation=%s account_type=%s", ErrUnrecognizedCumulativeLeg, req.Operation, accountType)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("risk: read cumulative usage: %w", err)
	}
	return total, nil
}
