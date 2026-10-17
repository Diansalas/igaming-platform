package reconciliation

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// F3 (platform-freeze work list) / A5 R9 / LF-A5-1 (b): the standing finding of the A5 reversal TOMBSTONE BRANCH.
//
// The tombstone branch (payments.DecidePayoutReversal on a SUBMITTED payout) releases the hold to the player on a
// four-eyes staff attestation that the PSP returned the payout, and writes a ledger tombstone on (provider, original
// reference) correlated to the withdrawal request. If the attestation was wrong (the PSP paid and never got the
// money back) the player is paid twice. The late-success cell (payments, LF-A5-1 (a)) covers success CALLBACKS and
// polls; this finding covers the STATEMENT, the provider's own record:
//
//	raise   ANY persisted import of (tenant, provider) - MOCK included, any amount - holds a payout line with status
//	        succeeded on the original reference or naming the merchant reference of one of the request's payout
//	        attempts (the M2 (d) raising predicate);
//	clear   (1) PSP return evidence: an ELIGIBLE (D-4) payout line with status reversed on exactly the original
//	            reference, amount and asset equal to the request's, dated no earlier than the newest succeeded line
//	            (a return the PSP itself reports; a later "succeeded" again re-raises); or
//	        (2) a K2 NET recovery of the withdrawn amount: compensating_entry debits with causation = the request's
//	            withdrawal_failed transaction (the E10 release of the branch), on the request's wallet and asset,
//	            none reversed, minus every credit with that causation (netRecovery, the M2 (d) / RR-1 rule).
//	        Nothing else clears it. Off-platform recovery has no clearing path (as for M2 (d)): the row's
//	        investigation status tracks it.
//
// It is reported on EVERY run (unwindowed) until it clears, so it is never a permanent false P1 for a legitimately
// resolved case: a genuine return is visible as (1), a recovery from the player as (2); and a branch whose payout the
// PSP never paid never raises at all (no succeeded line).

// MismatchKindPayPayoutReversalTombstoneButPaid is the R9 kind (migration 0167).
const MismatchKindPayPayoutReversalTombstoneButPaid MismatchKind = "pay_payout_reversal_tombstone_but_paid"

const payoutReversalTombstoneButPaidResolutionHint = "possible double payout after the payout reversal tombstone branch: PSP return evidence (a reversed payout line on the original reference) or recovery via compensating debit (K2, causation = the withdrawal_failed transaction); off-platform recovery is tracked through the investigation status; never allocation"

// payoutTombstone is one payout the A5 tombstone branch failed.
type payoutTombstone struct {
	tombstoneID uuid.UUID
	requestID   uuid.UUID
	ref         string // the original reference the tombstone holds
	merchants   []string
	state       string
	failedTx    *uuid.UUID // withdrawal_requests.release_ledger_transaction_id (the E10 withdrawal_failed)
	walletID    uuid.UUID
	amount      *big.Int
	asset       string
	recovered   *big.Int // nil when not read (no withdrawal_failed release)
}

// loadPayoutTombstones reads the tombstones of this provider whose correlation is a withdrawal request of the tenant
// (only the A5 branch writes such a tombstone; WD027 binds it to the proof-bound quorum), with the request and the
// merchant references of its payout attempts at this provider, and the K2 net recovery under the request's release.
func (m *payMatcher) loadPayoutTombstones(ctx context.Context, tx pgx.Tx, e *k3Evidence) error {
	rows, err := tx.Query(ctx, `
		SELECT t.id, wr.id, t.provider_tx_id, wr.state, wr.release_ledger_transaction_id, wr.wallet_id, wr.amount::text, wr.asset_code,
		       COALESCE(array_agg(a.merchant_reference ORDER BY a.id) FILTER (WHERE a.id IS NOT NULL), '{}')
		  FROM ledger_transactions t
		  JOIN withdrawal_requests wr ON wr.tenant_id = t.tenant_id AND wr.id = t.correlation_id
		  LEFT JOIN payment_attempts a ON a.tenant_id = wr.tenant_id AND a.withdrawal_request_id = wr.id
		                              AND a.operation = 'payout' AND a.provider_id = t.provider_id
		 WHERE t.tenant_id = $1 AND t.provider_id = $2 AND t.transaction_type = 'tombstone' AND t.provider_tx_id IS NOT NULL
		 GROUP BY t.id, wr.id, t.provider_tx_id, wr.state, wr.release_ledger_transaction_id, wr.wallet_id, wr.amount, wr.asset_code
		 ORDER BY t.id`, m.tenantID, m.provider)
	if err != nil {
		return err
	}
	for rows.Next() {
		var pt payoutTombstone
		var amount string
		if err := rows.Scan(&pt.tombstoneID, &pt.requestID, &pt.ref, &pt.state, &pt.failedTx, &pt.walletID, &amount, &pt.asset, &pt.merchants); err != nil {
			rows.Close()
			return err
		}
		if pt.amount, err = parseBig(amount); err != nil {
			rows.Close()
			return err
		}
		e.payoutTombstones = append(e.payoutTombstones, pt)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range e.payoutTombstones {
		pt := &e.payoutTombstones[i]
		if pt.failedTx == nil {
			continue
		}
		if pt.recovered, err = netRecovery(ctx, tx, m.tenantID, *pt.failedTx, pt.walletID, pt.asset); err != nil {
			return fmt.Errorf("payout tombstone recovery probe: %w", err)
		}
	}
	return nil
}

// payoutTombstoneLines returns the persisted PAYOUT lines on pt's original reference or naming one of its merchant
// references (deduplicated).
func (e *k3Evidence) payoutTombstoneLines(pt *payoutTombstone) []*persistedLine {
	var out []*persistedLine
	seen := map[*persistedLine]bool{}
	add := func(ls []*persistedLine) {
		for _, l := range ls {
			if l.kind == paymentStatementKindPayout && !seen[l] {
				seen[l] = true
				out = append(out, l)
			}
		}
	}
	add(e.byRef[pt.ref])
	for _, mr := range pt.merchants {
		add(e.byMerchant[mr])
	}
	return out
}

// checkPayoutReversalTombstones raises R9 (see the file comment), unwindowed, every run, one finding per tombstone.
func (m *payMatcher) checkPayoutReversalTombstones() {
	for i := range m.k3.payoutTombstones {
		pt := &m.k3.payoutTombstones[i]
		var paid, returned *persistedLine
		for _, l := range m.k3.payoutTombstoneLines(pt) {
			switch {
			case l.status == paymentStatementStatusSucceeded:
				if paid == nil || l.occurredAt.After(paid.occurredAt) {
					paid = l
				}
			case l.status == "reversed" && l.eligible && l.ref == pt.ref && l.asset == pt.asset && l.amount.Cmp(pt.amount) == 0:
				if returned == nil || l.occurredAt.After(returned.occurredAt) {
					returned = l
				}
			}
		}
		if paid == nil {
			continue // the PSP never said it paid: nothing to report
		}
		if returned != nil && !returned.occurredAt.Before(paid.occurredAt) {
			continue // (1) the PSP reports the return
		}
		if pt.recovered != nil && pt.recovered.Cmp(pt.amount) >= 0 {
			continue // (2) recovered from the player (K2 net)
		}
		recovered := "<none>"
		if pt.recovered != nil {
			recovered = pt.recovered.String()
		}
		m.r.add(MismatchKindPayPayoutReversalTombstoneButPaid,
			m.key("withdrawal_request="+pt.requestID.String(), "provider_reference="+pt.ref, "check=payout_reversal_tombstone_but_paid"),
			payoutReversalTombstoneButPaidResolutionHint,
			fmt.Sprintf("platform: withdrawal_request=%s state=%s tombstone=%s amount=%s asset=%s recovered=%s; %spaid line import=%s line_no=%d is_mock=%t status=%s reference=%s amount=%s asset=%s occurred_at=%s",
				pt.requestID, pt.state, pt.tombstoneID, pt.amount, pt.asset, recovered, m.label, paid.importID, paid.lineNo, paid.isMock, paid.status, paid.ref, paid.amount, paid.asset,
				paid.occurredAt.UTC().Format(time.RFC3339Nano)))
	}
}
