package rg

// ADR 0115 SLICE S3: the daily deposit credit check (section 4, "Daily check").
//
// Status: PARTIALLY IMPLEMENTED. The comparison is implemented and tested as a read-only function:
// every deposit intent in a time range is compared with the ledger posting it points at. Scheduling
// it daily and raising the rg_deposit_credit_mismatch alert (ADR 0102; delivery is
// ALERT-DELIVERY-1, NOT IMPLEMENTED) is not wired in S3.

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CreditMismatchKind names a way an intent and its ledger posting disagree.
type CreditMismatchKind string

const (
	// MismatchSucceededWithoutPosting: a succeeded intent that points at no ledger transaction.
	MismatchSucceededWithoutPosting CreditMismatchKind = "succeeded_without_posting"
	// MismatchPostingOnUnsettledIntent: an intent that points at a ledger transaction but is not succeeded.
	MismatchPostingOnUnsettledIntent CreditMismatchKind = "posting_on_unsettled_intent"
	// MismatchWrongPostingType: the linked transaction is not a deposit.
	MismatchWrongPostingType CreditMismatchKind = "wrong_posting_type"
	// MismatchAmount: the credited amount differs from the intent amount.
	MismatchAmount CreditMismatchKind = "amount_mismatch"
	// MismatchAccount: the credit went to another account than the intent's player.
	MismatchAccount CreditMismatchKind = "account_mismatch"
	// MismatchAsset: the credit is in another asset than the intent's.
	MismatchAsset CreditMismatchKind = "asset_mismatch"
)

// CreditMismatch is one disagreement. Amounts are NUMERIC(38,0) text.
type CreditMismatch struct {
	IntentID        uuid.UUID
	Kind            CreditMismatchKind
	IntentStatus    string
	IntentAmount    string
	CreditedAmount  string
	LedgerTxID      *uuid.UUID
	PlayerAccountID uuid.UUID
}

// CheckDepositCredits compares the credited amount of every intent created in [from, to) with its
// ledger posting. limit bounds the result (default 500, max 5000). Read only.
func CheckDepositCredits(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, from, to time.Time, limit int) ([]CreditMismatch, error) {
	if tenantID == uuid.Nil || !from.Before(to) {
		return nil, fmt.Errorf("rg: credit check needs a tenant and a non-empty range")
	}
	if limit <= 0 {
		limit = 500
	}
	if limit > 5000 {
		limit = 5000
	}
	rows, err := tx.Query(ctx, `
		SELECT di.id, di.status, di.amount::text, di.asset_code, di.player_account_id, di.ledger_transaction_id,
		       lt.transaction_type, cr.credited::text, cr.accounts, cr.assets
		  FROM deposit_intents di
		  LEFT JOIN ledger_transactions lt ON lt.id = di.ledger_transaction_id AND lt.tenant_id = di.tenant_id
		  LEFT JOIN LATERAL (
		        SELECT SUM(e.amount) AS credited, array_agg(DISTINCT e.player_account_id) AS accounts, array_agg(DISTINCT e.asset_code) AS assets
		          FROM ledger_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id AND la.tenant_id = e.tenant_id
		         WHERE e.tenant_id = di.tenant_id AND e.ledger_transaction_id = di.ledger_transaction_id
		           AND la.account_type = 'player_cash' AND e.direction = 'credit') cr ON true
		 WHERE di.tenant_id = $1 AND di.created_at >= $2 AND di.created_at < $3
		   AND (di.status = 'succeeded' OR di.ledger_transaction_id IS NOT NULL)
		 ORDER BY di.created_at, di.id`, tenantID, from, to)
	if err != nil {
		return nil, fmt.Errorf("rg: credit check: %w", err)
	}
	defer rows.Close()
	var out []CreditMismatch
	for rows.Next() {
		var id, player uuid.UUID
		var status, amount, asset string
		var txID *uuid.UUID
		var txType, credited *string
		var accounts []uuid.UUID
		var assets []string
		if err := rows.Scan(&id, &status, &amount, &asset, &player, &txID, &txType, &credited, &accounts, &assets); err != nil {
			return nil, fmt.Errorf("rg: scan credit check: %w", err)
		}
		kind := creditKind(status, amount, asset, player, txID, txType, credited, accounts, assets)
		if kind == "" {
			continue
		}
		m := CreditMismatch{IntentID: id, Kind: kind, IntentStatus: status, IntentAmount: amount, LedgerTxID: txID, PlayerAccountID: player, CreditedAmount: "0"}
		if credited != nil {
			m.CreditedAmount = *credited
		}
		out = append(out, m)
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

func creditKind(status, amount, asset string, player uuid.UUID, txID *uuid.UUID, txType, credited *string, accounts []uuid.UUID, assets []string) CreditMismatchKind {
	switch {
	case txID == nil:
		if status == "succeeded" {
			return MismatchSucceededWithoutPosting
		}
		return ""
	case status != "succeeded":
		return MismatchPostingOnUnsettledIntent
	case txType == nil || *txType != "deposit":
		return MismatchWrongPostingType
	}
	want, ok := new(big.Int).SetString(amount, 10)
	got := new(big.Int)
	if credited != nil {
		v, okc := new(big.Int).SetString(*credited, 10)
		if !okc {
			return MismatchAmount
		}
		got = v
	}
	if !ok || want.Cmp(got) != 0 {
		return MismatchAmount
	}
	if len(accounts) != 1 || accounts[0] != player {
		return MismatchAccount
	}
	if len(assets) != 1 || assets[0] != asset {
		return MismatchAsset
	}
	return ""
}
