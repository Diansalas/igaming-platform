package reconciliation

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Casino internal consistency reconciliation stream (Stage 10.3 W2b,
// CAS-RECON-1; docs/plans/stage-10.3-planning/02-casino-financial-
// analysis.md §2.3-§2.4, §2.9, §2.12-§2.14; ADR 0092).
//
// Platform-internal, zero tolerance, no counterparty. It proves that the
// platform's own casino records agree with each other and with the ledger,
// and it turns durable evidence of a provider-asserted event the ledger
// does not hold (the casino_callback_rejections record, migration 0097)
// into findings. Seven checks, all read-only:
//
//	C1 round binding     every casino_bet has its casino_provider_rounds
//	                     row (same tenant, provider, correlation id), the
//	                     round's correlation id is the one BindProviderRound
//	                     derives, every round has >= 1 casino_bet, and the
//	                     player owning the bet's debited wallet is the
//	                     round's player.
//	C2 posting shape     every casino_bet/_win/_rollback carries
//	                     provider_id + provider_tx_id, has entries, balances
//	                     (debits == credits), uses one asset and touches
//	                     exactly one player wallet; a bet debits a player
//	                     spendable account, never debits house_gaming and
//	                     never credits a player spendable account; a win
//	                     credits a player account, never credits
//	                     house_gaming, and credits a wallet that one of its
//	                     round's bets debited.
//	C3 orphan win        every casino_win's round (tenant, provider,
//	                     correlation id) has a casino_bet.
//	C4 rollback linkage  every casino_rollback reverses a casino_bet or
//	                     casino_win of the same provider; nothing else
//	                     reverses a casino original; at most one reversal
//	                     per original; a cash rollback's caller legs are the
//	                     exact inverse multiset of the original's.
//	C5 tombstone conflict no non-tombstone ledger transaction shares
//	                     (provider_id, provider_tx_id) with a casino
//	                     tombstone (a backstop: structurally impossible
//	                     under the unique index).
//	C6 unposted provider event  a rejection-record row whose
//	                     (provider_id, provider_tx_id) the ledger holds no
//	                     transaction for (not even a tombstone): a provider
//	                     asserted a financial event the ledger lacks.
//	                     Evidence-type: recorded once per key.
//	C7 tombstone late original  a rejection-record row of class
//	                     original_tombstoned: an original arrived after its
//	                     tombstone. Platform net is zero (correct); only a
//	                     statement can tell whether the provider still
//	                     counts it. Evidence-type: recorded once per
//	                     tombstone.
//
// Detection only. The stream never writes ledger, projection, round,
// session, capability or rejection data; it inserts one immutable
// reconciliation_runs row plus zero or more reconciliation_mismatches rows
// (persistRun) and nothing else. Compensation is a human, four-eyes action
// through LEDGER-MANUAL-ADJ-4EYES-1, which is NOT IMPLEMENTED (paper 02
// §2.9): until it exists, the only corrections are provider redelivery or a
// provider-issued rollback through the normal idempotent callback path.
//
// Severity rule (paper 02 §2.3): every mismatch row is a P1, so only
// conditions that are ALWAYS wrong become mismatches. Ageing cash rounds
// (loss-by-silence is normal, F10) are a metric in the run's audit
// metadata, never a mismatch.
//
// Snapshot: each check is a single SQL statement, so each sees one
// consistent snapshot. A bet and its round row, and a posting and its
// entries, commit atomically, so live traffic cannot produce a
// half-visible pair inside one check.
//
// Deviation from paper 02 §2.4 (recorded by its author, ledger-finance):
//   - C3's "no casino_win posted after the round's only bet(s) were
//     reversed" order sub-check is NOT IMPLEMENTED. The ledger carries no
//     commit-order evidence: posted_at is the transaction START time, so a
//     legitimate interleaving (a rollback's transaction starting first but
//     acquiring the round's L2 row lock after the win) shows
//     rollback.posted_at < win.posted_at. A zero-tolerance P1 on that would
//     break the severity rule. The guarantee is enforced at write time
//     (postWin's L2 FOR UPDATE + resolveWinOrigin); orphan wins are still
//     detected.
//   - C4's exact-inverse comparison applies to cash rollbacks only. A
//     rollback whose original or itself touches BONUS_SET
//     (player_bonus/player_locked_bonus/player_bonus_held) follows the
//     §16.15 held-disposition shapes, which are deliberately not inverses;
//     bonus-funded casino stakes are not implemented (G-6). Linkage, the
//     one-per-original rule and C2's balance/asset/wallet checks still apply
//     to them.
//   - C6 does not include an "internal_error" class: a generic 500 is not a
//     rejection decision, is retryable, and is not recorded.

// StreamCasinoConsistency is the stream name on reconciliation_runs.
const StreamCasinoConsistency Stream = "casino_consistency"

// Mismatch kinds added by migration 0097.
const (
	MismatchKindCasRoundBinding        MismatchKind = "cas_round_binding_mismatch"
	MismatchKindCasPostingShape        MismatchKind = "cas_posting_shape_mismatch"
	MismatchKindCasOrphanWin           MismatchKind = "cas_orphan_win"
	MismatchKindCasRollbackLinkage     MismatchKind = "cas_rollback_linkage_mismatch"
	MismatchKindCasTombstoneConflict   MismatchKind = "cas_tombstone_conflict"
	MismatchKindCasUnpostedEvent       MismatchKind = "cas_unposted_provider_event"
	MismatchKindCasTombstoneLateOrigin MismatchKind = "cas_tombstone_late_original"
)

// CasinoAgeingMetricWindow is the age past which an unresolved cash round
// is counted in the unresolved_cash_rounds_older_than_window METRIC. It is
// a reporting parameter only - never a tolerance and never a mismatch
// trigger (loss-by-silence is normal, paper 02 §2.4). The real
// per-jurisdiction settlement window W is an open human decision (08
// §16.5a) and only matters once bonus-funded casino stakes ship.
const CasinoAgeingMetricWindow = 24 * time.Hour

// CasinoMetrics are recorded in the run's audit_log metadata, never as
// mismatches.
type CasinoMetrics struct {
	UnresolvedCashRoundsOlderThanWindow int64
	TombstonesTotal                     int64
	RejectionsTotal                     int64
	RejectionsUnposted                  int64
}

// AuditMetadata renders m for the run's audit record.
func (m CasinoMetrics) AuditMetadata() map[string]any {
	return map[string]any{
		"unresolved_cash_rounds_older_than_window": m.UnresolvedCashRoundsOlderThanWindow,
		"ageing_metric_window_seconds":             int64(CasinoAgeingMetricWindow / time.Second),
		"tombstones_total":                         m.TombstonesTotal,
		"rejections_total":                         m.RejectionsTotal,
		"rejections_unposted":                      m.RejectionsUnposted,
	}
}

// casinoTxTypes are the casino posting types.
var casinoTxTypes = []string{"casino_bet", "casino_win", "casino_rollback"}

// casinoProvidersCTE is the tenant's casino provider-id set (paper 02
// §2.4): the key format alone cannot tell a casino tombstone from a
// payments one (G-4), so casino tombstones are those whose provider_id is
// a casino provider of this tenant.
const casinoProvidersCTE = `casino_providers AS (
	    SELECT provider_id FROM casino_provider_capabilities WHERE tenant_id = $1
	    UNION SELECT provider_id FROM casino_provider_rounds WHERE tenant_id = $1
	    UNION SELECT provider_id FROM ledger_transactions
	           WHERE tenant_id = $1 AND provider_id IS NOT NULL
	             AND transaction_type IN ('casino_bet', 'casino_win', 'casino_rollback')
	    UNION SELECT provider_id FROM casino_callback_rejections WHERE tenant_id = $1)`

// RunCasinoConsistency runs the casino_consistency stream for tenantID
// inside tx (db.Pool.WithTenant(tenantID)), recording exactly one Run and
// its mismatches, atomically - the RunLedgerVsProjection contract. It
// writes nothing else.
func RunCasinoConsistency(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, periodStart, periodEnd time.Time) (Run, []Mismatch, CasinoMetrics, error) {
	run := Run{
		ID: uuid.New(), TenantID: tenantID, Stream: StreamCasinoConsistency,
		PeriodStart: periodStart, PeriodEnd: periodEnd, RunAt: time.Now().UTC(),
	}
	r := &sbRecorder{tenantID: tenantID}
	var metrics CasinoMetrics

	footprint, err := casHasFootprint(ctx, tx, tenantID)
	if err != nil {
		return Run{}, nil, CasinoMetrics{}, fmt.Errorf("reconciliation: casino footprint: %w", err)
	}
	if footprint {
		checks := []struct {
			name string
			fn   func(context.Context, pgx.Tx, uuid.UUID, *sbRecorder) error
		}{
			{"C1 round binding", casCheckRoundBinding},
			{"C2 posting shape", casCheckPostingShape},
			{"C3 orphan win", casCheckOrphanWin},
			{"C4 rollback linkage", casCheckRollbackLinkage},
			{"C5 tombstone conflict", casCheckTombstoneConflict},
			{"C6/C7 rejection record", casCheckRejectionRecord},
		}
		for _, c := range checks {
			if err := c.fn(ctx, tx, tenantID, r); err != nil {
				return Run{}, nil, CasinoMetrics{}, fmt.Errorf("reconciliation: casino %s: %w", c.name, err)
			}
		}
		if metrics, err = casLoadMetrics(ctx, tx, tenantID); err != nil {
			return Run{}, nil, CasinoMetrics{}, fmt.Errorf("reconciliation: casino metrics: %w", err)
		}
	}

	mismatches := r.mismatches
	if len(mismatches) > 0 {
		run.Status = StatusMismatchesFound
	} else {
		run.Status = StatusClean
	}
	if err := persistRun(ctx, tx, run, mismatches); err != nil {
		return Run{}, nil, CasinoMetrics{}, err
	}
	return run, mismatches, metrics, nil
}

// casHasFootprint: when the tenant holds no input of any check, every
// check is over empty sets and provably yields nothing - a cost guard for
// tenants with no casino activity, not a tolerance (the sbHasFootprint
// precedent).
func casHasFootprint(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (bool, error) {
	var found bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = ANY($2))
		    OR EXISTS (SELECT 1 FROM casino_provider_rounds WHERE tenant_id = $1)
		    OR EXISTS (SELECT 1 FROM casino_callback_rejections WHERE tenant_id = $1)
		    OR EXISTS (SELECT 1 FROM casino_provider_capabilities WHERE tenant_id = $1)`,
		tenantID, casinoTxTypes).Scan(&found)
	return found, err
}

// ---------------------------------------------------------------------
// C1 round binding.

// casRoundCorrelationID mirrors internal/casino.roundCorrelationID exactly
// (uuid v5, OID namespace, "<tenant>:<provider>:<round>"). Duplicated
// rather than imported: internal/reconciliation must not import
// internal/casino (the statement package's import-cycle rule). A unit test
// pins the two together.
func casRoundCorrelationID(tenantID uuid.UUID, providerID, roundID string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(tenantID.String()+":"+providerID+":"+roundID))
}

func casCheckRoundBinding(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, r *sbRecorder) error {
	// (a) casino_bet without its round row.
	rows, err := tx.Query(ctx, `
		SELECT t.id, COALESCE(t.provider_id, '<null>'), COALESCE(t.provider_tx_id, '<null>'), t.correlation_id
		  FROM ledger_transactions t
		 WHERE t.tenant_id = $1 AND t.transaction_type = 'casino_bet'
		   AND NOT EXISTS (SELECT 1 FROM casino_provider_rounds pr
		                    WHERE pr.tenant_id = t.tenant_id AND pr.provider_id = t.provider_id
		                      AND pr.correlation_id = t.correlation_id)
		 ORDER BY t.id`, tenantID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, corr uuid.UUID
		var provider, ref string
		if err := rows.Scan(&id, &provider, &ref, &corr); err != nil {
			rows.Close()
			return err
		}
		r.add(MismatchKindCasRoundBinding, "ledger_transaction="+id.String()+" check=bet_has_round",
			"casino_provider_rounds row for provider="+provider+" correlation="+corr.String(),
			"no round row (bet provider_tx_id="+ref+")")
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// (b) round rows: derived correlation, and >= 1 casino_bet.
	rows, err = tx.Query(ctx, `
		SELECT pr.id, pr.provider_id, pr.provider_round_id, pr.correlation_id,
		       EXISTS (SELECT 1 FROM ledger_transactions t
		                WHERE t.tenant_id = pr.tenant_id AND t.transaction_type = 'casino_bet'
		                  AND t.provider_id = pr.provider_id AND t.correlation_id = pr.correlation_id)
		  FROM casino_provider_rounds pr
		 WHERE pr.tenant_id = $1
		 ORDER BY pr.id`, tenantID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, corr uuid.UUID
		var provider, round string
		var hasBet bool
		if err := rows.Scan(&id, &provider, &round, &corr, &hasBet); err != nil {
			rows.Close()
			return err
		}
		key := "round=" + id.String()
		if want := casRoundCorrelationID(tenantID, provider, round); want != corr {
			r.add(MismatchKindCasRoundBinding, key+" check=round_correlation",
				"correlation="+want.String(), "correlation="+corr.String())
		}
		if !hasBet {
			r.add(MismatchKindCasRoundBinding, key+" check=round_has_bet",
				">= 1 casino_bet under correlation="+corr.String()+" provider="+provider, "no casino_bet")
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// (c) the player owning every wallet account the bet debited is the
	// round's player.
	rows, err = tx.Query(ctx, `
		SELECT DISTINCT t.id, pr.id, COALESCE(la.player_account_id::text, '<null>'), pr.player_account_id
		  FROM ledger_transactions t
		  JOIN casino_provider_rounds pr
		    ON pr.tenant_id = t.tenant_id AND pr.provider_id = t.provider_id AND pr.correlation_id = t.correlation_id
		  JOIN ledger_entries e ON e.ledger_transaction_id = t.id AND e.direction = 'debit'
		  JOIN ledger_accounts la ON la.id = e.ledger_account_id AND la.wallet_id IS NOT NULL
		 WHERE t.tenant_id = $1 AND t.transaction_type = 'casino_bet'
		   AND la.player_account_id IS DISTINCT FROM pr.player_account_id
		 ORDER BY 1, 2`, tenantID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var txID, roundID, roundPlayer uuid.UUID
		var betPlayer string
		if err := rows.Scan(&txID, &roundID, &betPlayer, &roundPlayer); err != nil {
			return err
		}
		r.add(MismatchKindCasRoundBinding, "ledger_transaction="+txID.String()+" round="+roundID.String()+" check=round_player",
			"debited wallet player="+roundPlayer.String(), "debited wallet player="+betPlayer)
	}
	return rows.Err()
}

// ---------------------------------------------------------------------
// C2 posting shape.

func casCheckPostingShape(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, r *sbRecorder) error {
	rows, err := tx.Query(ctx, `
		SELECT t.id, t.transaction_type,
		       (t.provider_id IS NULL OR t.provider_tx_id IS NULL),
		       count(e.id),
		       COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'debit'), 0)::text,
		       COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'credit'), 0)::text,
		       count(DISTINCT la.asset_code),
		       count(DISTINCT la.wallet_id),
		       COALESCE(bool_or(e.direction = 'debit' AND la.wallet_id IS NOT NULL
		                        AND la.account_type IN ('player_cash', 'player_bonus')), false),
		       COALESCE(bool_or(e.direction = 'debit' AND la.account_type = 'house_gaming'), false),
		       COALESCE(bool_or(e.direction = 'credit' AND la.wallet_id IS NOT NULL
		                        AND la.account_type IN ('player_cash', 'player_bonus')), false),
		       COALESCE(bool_or(e.direction = 'credit' AND la.wallet_id IS NOT NULL), false),
		       COALESCE(bool_or(e.direction = 'credit' AND la.account_type = 'house_gaming'), false)
		  FROM ledger_transactions t
		  LEFT JOIN ledger_entries e ON e.ledger_transaction_id = t.id
		  LEFT JOIN ledger_accounts la ON la.id = e.ledger_account_id
		 WHERE t.tenant_id = $1 AND t.transaction_type = ANY($2)
		 GROUP BY t.id, t.transaction_type
		 ORDER BY t.id`, tenantID, casinoTxTypes)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id uuid.UUID
		var txType, dr, cr string
		var missingProvider, playerSpendDebit, houseDebit, playerSpendCredit, walletCredit, houseCredit bool
		var entries, assets, wallets int64
		if err := rows.Scan(&id, &txType, &missingProvider, &entries, &dr, &cr, &assets, &wallets,
			&playerSpendDebit, &houseDebit, &playerSpendCredit, &walletCredit, &houseCredit); err != nil {
			rows.Close()
			return err
		}
		key := "ledger_transaction=" + id.String() + " type=" + txType
		add := func(check, expected, actual string) {
			r.add(MismatchKindCasPostingShape, key+" check="+check, expected, actual)
		}
		if missingProvider {
			add("provider_reference", "provider_id and provider_tx_id present", "missing")
		}
		if entries == 0 {
			add("entries", ">= 1 ledger entry", "0 entries")
			continue
		}
		debits, err := parseBig(dr)
		if err != nil {
			rows.Close()
			return err
		}
		credits, err := parseBig(cr)
		if err != nil {
			rows.Close()
			return err
		}
		if debits.Cmp(credits) != 0 {
			add("balance", "debits == credits", fmt.Sprintf("debits=%s credits=%s", debits, credits))
		}
		if assets != 1 {
			add("single_asset", "1 asset", fmt.Sprintf("%d assets", assets))
		}
		if wallets != 1 {
			add("single_wallet", "exactly 1 player wallet", fmt.Sprintf("%d wallets", wallets))
		}
		switch txType {
		case "casino_bet":
			if !playerSpendDebit {
				add("bet_debits_player", "a debit on player_cash/player_bonus", "none")
			}
			if houseDebit {
				add("bet_house_side", "house_gaming never debited by a bet", "house_gaming debited")
			}
			if playerSpendCredit {
				add("bet_player_side", "player_cash/player_bonus never credited by a bet", "credited")
			}
		case "casino_win":
			if !walletCredit {
				add("win_credits_player", "a credit on a player wallet account", "none")
			}
			if houseCredit {
				add("win_house_side", "house_gaming never credited by a win", "house_gaming credited")
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// A win credits a wallet one of its round's bets debited. Wins with no
	// bet at all are C3's, not this check's.
	rows, err = tx.Query(ctx, `
		WITH win_wallet AS (
		    SELECT t.id, t.provider_id, t.correlation_id, la.wallet_id
		      FROM ledger_transactions t
		      JOIN ledger_entries e ON e.ledger_transaction_id = t.id AND e.direction = 'credit'
		      JOIN ledger_accounts la ON la.id = e.ledger_account_id AND la.wallet_id IS NOT NULL
		     WHERE t.tenant_id = $1 AND t.transaction_type = 'casino_win'
		     GROUP BY t.id, t.provider_id, t.correlation_id, la.wallet_id),
		bet_wallet AS (
		    SELECT DISTINCT b.provider_id, b.correlation_id, la.wallet_id
		      FROM ledger_transactions b
		      JOIN ledger_entries e ON e.ledger_transaction_id = b.id AND e.direction = 'debit'
		      JOIN ledger_accounts la ON la.id = e.ledger_account_id AND la.wallet_id IS NOT NULL
		     WHERE b.tenant_id = $1 AND b.transaction_type = 'casino_bet')
		SELECT w.id, w.wallet_id::text,
		       (SELECT string_agg(bw.wallet_id::text, ',' ORDER BY bw.wallet_id::text) FROM bet_wallet bw
		         WHERE bw.provider_id = w.provider_id AND bw.correlation_id = w.correlation_id)
		  FROM win_wallet w
		 WHERE EXISTS (SELECT 1 FROM bet_wallet bw
		                WHERE bw.provider_id = w.provider_id AND bw.correlation_id = w.correlation_id)
		   AND NOT EXISTS (SELECT 1 FROM bet_wallet bw
		                    WHERE bw.provider_id = w.provider_id AND bw.correlation_id = w.correlation_id
		                      AND bw.wallet_id = w.wallet_id)
		 ORDER BY 1, 2`, tenantID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var winWallet, betWallets string
		if err := rows.Scan(&id, &winWallet, &betWallets); err != nil {
			return err
		}
		r.add(MismatchKindCasPostingShape, "ledger_transaction="+id.String()+" type=casino_win check=win_wallet",
			"credited wallet in round bet wallets ["+betWallets+"]", "credited wallet="+winWallet)
	}
	return rows.Err()
}

// ---------------------------------------------------------------------
// C3 orphan win.

func casCheckOrphanWin(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, r *sbRecorder) error {
	rows, err := tx.Query(ctx, `
		SELECT t.id, COALESCE(t.provider_id, '<null>'), COALESCE(t.provider_tx_id, '<null>'), t.correlation_id
		  FROM ledger_transactions t
		 WHERE t.tenant_id = $1 AND t.transaction_type = 'casino_win'
		   AND NOT EXISTS (SELECT 1 FROM ledger_transactions b
		                    WHERE b.tenant_id = t.tenant_id AND b.transaction_type = 'casino_bet'
		                      AND b.provider_id IS NOT DISTINCT FROM t.provider_id
		                      AND b.correlation_id = t.correlation_id)
		 ORDER BY t.id`, tenantID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, corr uuid.UUID
		var provider, ref string
		if err := rows.Scan(&id, &provider, &ref, &corr); err != nil {
			return err
		}
		r.add(MismatchKindCasOrphanWin, "ledger_transaction="+id.String(),
			"a casino_bet under provider="+provider+" correlation="+corr.String(),
			"no casino_bet (win provider_tx_id="+ref+")")
	}
	return rows.Err()
}

// ---------------------------------------------------------------------
// C4 rollback linkage.

func casCheckRollbackLinkage(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, r *sbRecorder) error {
	// (a) every casino_rollback names a casino_bet/_win of the same
	// provider.
	rows, err := tx.Query(ctx, `
		SELECT rb.id, rb.reverses_transaction_id IS NOT NULL, o.id IS NOT NULL,
		       COALESCE(o.transaction_type, '<none>'),
		       rb.provider_id IS NOT DISTINCT FROM o.provider_id
		  FROM ledger_transactions rb
		  LEFT JOIN ledger_transactions o ON o.id = rb.reverses_transaction_id AND o.tenant_id = rb.tenant_id
		 WHERE rb.tenant_id = $1 AND rb.transaction_type = 'casino_rollback'
		   AND (rb.reverses_transaction_id IS NULL OR o.id IS NULL
		        OR o.transaction_type NOT IN ('casino_bet', 'casino_win')
		        OR rb.provider_id IS DISTINCT FROM o.provider_id)
		 ORDER BY rb.id`, tenantID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id uuid.UUID
		var hasLink, found, sameProvider bool
		var origType string
		if err := rows.Scan(&id, &hasLink, &found, &origType, &sameProvider); err != nil {
			rows.Close()
			return err
		}
		r.add(MismatchKindCasRollbackLinkage, "ledger_transaction="+id.String()+" check=reverses",
			"reverses a casino_bet/casino_win of the same provider",
			fmt.Sprintf("linked=%t original_found=%t original_type=%s same_provider=%t", hasLink, found, origType, sameProvider))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// (b) nothing but a casino_rollback reverses a casino original, and at
	// most one reversal per original.
	rows, err = tx.Query(ctx, `
		SELECT o.id, count(x.id),
		       string_agg(x.transaction_type || ':' || x.id::text, ',' ORDER BY x.id::text),
		       bool_and(x.transaction_type = 'casino_rollback')
		  FROM ledger_transactions o
		  JOIN ledger_transactions x ON x.reverses_transaction_id = o.id
		 WHERE o.tenant_id = $1 AND o.transaction_type IN ('casino_bet', 'casino_win')
		 GROUP BY o.id
		HAVING count(x.id) > 1 OR NOT bool_and(x.transaction_type = 'casino_rollback')
		 ORDER BY o.id`, tenantID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id uuid.UUID
		var n int64
		var list string
		var allRollback bool
		if err := rows.Scan(&id, &n, &list, &allRollback); err != nil {
			rows.Close()
			return err
		}
		r.add(MismatchKindCasRollbackLinkage, "original="+id.String()+" check=one_reversal",
			"at most one reversal, of type casino_rollback", fmt.Sprintf("%d reversals [%s]", n, list))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// (c) cash rollback caller legs == exact inverse multiset of the
	// original's (mirror/recognition legs excluded; BONUS_SET rollbacks
	// exempt - see the file comment).
	rows, err = tx.Query(ctx, `
		WITH rb AS (
		    SELECT r.id AS rid, r.reverses_transaction_id AS oid
		      FROM ledger_transactions r
		      JOIN ledger_transactions o ON o.id = r.reverses_transaction_id AND o.tenant_id = r.tenant_id
		     WHERE r.tenant_id = $1 AND r.transaction_type = 'casino_rollback'
		       AND o.transaction_type IN ('casino_bet', 'casino_win')
		       AND NOT EXISTS (SELECT 1 FROM ledger_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
		                        WHERE e.ledger_transaction_id IN (r.id, o.id)
		                          AND la.account_type IN ('player_bonus', 'player_locked_bonus', 'player_bonus_held'))),
		legs AS (
		    SELECT rb.rid, e.ledger_account_id AS acct,
		           CASE e.direction WHEN 'debit' THEN 'credit' ELSE 'debit' END AS dir, e.amount, 'original' AS side
		      FROM rb JOIN ledger_entries e ON e.ledger_transaction_id = rb.oid
		      JOIN ledger_accounts la ON la.id = e.ledger_account_id
		     WHERE la.account_type NOT IN ('promo_liability', 'bonus_expense', 'provider_payable')
		    UNION ALL
		    SELECT rb.rid, e.ledger_account_id, e.direction, e.amount, 'rollback'
		      FROM rb JOIN ledger_entries e ON e.ledger_transaction_id = rb.rid
		      JOIN ledger_accounts la ON la.id = e.ledger_account_id
		     WHERE la.account_type NOT IN ('promo_liability', 'bonus_expense', 'provider_payable'))
		SELECT rid, acct, dir, amount::text,
		       count(*) FILTER (WHERE side = 'original'), count(*) FILTER (WHERE side = 'rollback')
		  FROM legs
		 GROUP BY rid, acct, dir, amount
		HAVING count(*) FILTER (WHERE side = 'original') <> count(*) FILTER (WHERE side = 'rollback')
		 ORDER BY 1, 2, 3, 4`, tenantID)
	if err != nil {
		return err
	}
	defer rows.Close()
	diffs := map[uuid.UUID][]string{}
	var order []uuid.UUID
	for rows.Next() {
		var rid, acct uuid.UUID
		var dir, amount string
		var nOrig, nRb int64
		if err := rows.Scan(&rid, &acct, &dir, &amount, &nOrig, &nRb); err != nil {
			return err
		}
		if _, seen := diffs[rid]; !seen {
			order = append(order, rid)
		}
		diffs[rid] = append(diffs[rid], fmt.Sprintf("%s %s %s: expected=%d actual=%d", acct, dir, amount, nOrig, nRb))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, rid := range order {
		d := diffs[rid]
		sort.Strings(d)
		r.add(MismatchKindCasRollbackLinkage, "ledger_transaction="+rid.String()+" check=exact_inverse",
			"caller legs are the exact inverse of the original's", fmt.Sprint(d))
	}
	return nil
}

// ---------------------------------------------------------------------
// C5 tombstone conflict.

func casCheckTombstoneConflict(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, r *sbRecorder) error {
	rows, err := tx.Query(ctx, `
		WITH `+casinoProvidersCTE+`
		SELECT ts.id, ts.provider_id, ts.provider_tx_id, o.id, o.transaction_type
		  FROM ledger_transactions ts
		  JOIN ledger_transactions o
		    ON o.tenant_id = ts.tenant_id AND o.provider_id = ts.provider_id
		   AND o.provider_tx_id = ts.provider_tx_id AND o.id <> ts.id AND o.transaction_type <> 'tombstone'
		 WHERE ts.tenant_id = $1 AND ts.transaction_type = 'tombstone'
		   AND ts.provider_id IN (SELECT provider_id FROM casino_providers)
		 ORDER BY ts.id, o.id`, tenantID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var tsID, oID uuid.UUID
		var provider, ref, oType string
		if err := rows.Scan(&tsID, &provider, &ref, &oID, &oType); err != nil {
			return err
		}
		r.add(MismatchKindCasTombstoneConflict,
			"tombstone="+tsID.String()+" provider="+provider+" provider_tx_id="+ref,
			"no non-tombstone transaction under the tombstoned reference",
			"ledger_transaction="+oID.String()+" type="+oType)
	}
	return rows.Err()
}

// ---------------------------------------------------------------------
// C6/C7 rejection record (evidence-type, deduplicated per key).

func casExistingEvidenceKeys(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (map[string]bool, error) {
	rows, err := tx.Query(ctx, `
		SELECT mismatch_kind, reconciliation_key FROM reconciliation_mismatches
		 WHERE tenant_id = $1 AND mismatch_kind IN ($2, $3)`,
		tenantID, string(MismatchKindCasUnpostedEvent), string(MismatchKindCasTombstoneLateOrigin))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var kind, key string
		if err := rows.Scan(&kind, &key); err != nil {
			return nil, err
		}
		out[kind+"\x00"+key] = true
	}
	return out, rows.Err()
}

func casCheckRejectionRecord(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, r *sbRecorder) error {
	existing, err := casExistingEvidenceKeys(ctx, tx, tenantID)
	if err != nil {
		return err
	}
	addOnce := func(kind MismatchKind, key, expected, actual string) {
		k := string(kind) + "\x00" + key
		if existing[k] {
			return
		}
		existing[k] = true
		r.add(kind, key, expected, actual)
	}

	// C6: provider asserted an event under a reference the ledger holds
	// nothing for - not a posting, not a tombstone.
	rows, err := tx.Query(ctx, `
		SELECT cr.provider_id, cr.event_type, cr.provider_tx_id, cr.reason_class,
		       COALESCE(cr.original_provider_tx_id, ''), COALESCE(cr.round_id, ''),
		       COALESCE(cr.asset_code, ''), COALESCE(cr.amount::text, '')
		  FROM casino_callback_rejections cr
		 WHERE cr.tenant_id = $1
		   AND NOT EXISTS (SELECT 1 FROM ledger_transactions t
		                    WHERE t.tenant_id = cr.tenant_id AND t.provider_id = cr.provider_id
		                      AND t.provider_tx_id = cr.provider_tx_id)
		 ORDER BY cr.first_seen_at, cr.id`, tenantID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var provider, event, ref, class, original, round, asset, amount string
		if err := rows.Scan(&provider, &event, &ref, &class, &original, &round, &asset, &amount); err != nil {
			rows.Close()
			return err
		}
		key := fmt.Sprintf("provider=%s event=%s provider_tx_id=%s reason=%s", provider, event, ref, class)
		actual := fmt.Sprintf("rejected (%s); ledger holds nothing under this reference; round=%s asset=%s amount=%s", class, round, asset, amount)
		if original != "" {
			actual += " original_provider_tx_id=" + original
		}
		addOnce(MismatchKindCasUnpostedEvent, key, "ledger holds the provider-asserted "+event, actual)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// C7: an original arrived after its tombstone.
	rows, err = tx.Query(ctx, `
		SELECT cr.provider_id, cr.provider_tx_id,
		       string_agg(DISTINCT cr.event_type, ',' ORDER BY cr.event_type),
		       (SELECT ts.id::text FROM ledger_transactions ts
		         WHERE ts.tenant_id = cr.tenant_id AND ts.provider_id = cr.provider_id
		           AND ts.provider_tx_id = cr.provider_tx_id AND ts.transaction_type = 'tombstone'
		         LIMIT 1)
		  FROM casino_callback_rejections cr
		 WHERE cr.tenant_id = $1 AND cr.reason_class = 'original_tombstoned'
		 GROUP BY cr.tenant_id, cr.provider_id, cr.provider_tx_id
		 ORDER BY 1, 2`, tenantID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var provider, ref, events string
		var tombstone *string
		if err := rows.Scan(&provider, &ref, &events, &tombstone); err != nil {
			return err
		}
		key := fmt.Sprintf("tombstone provider=%s provider_tx_id=%s", provider, ref)
		actual := "late original (" + events + ") rejected after tombstone=" + deref(tombstone) +
			"; platform net zero; provider-side count needs the statement"
		addOnce(MismatchKindCasTombstoneLateOrigin, key, "no original after its tombstone", actual)
	}
	return rows.Err()
}

// ---------------------------------------------------------------------
// Metrics (never mismatches).

func casLoadMetrics(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (CasinoMetrics, error) {
	var m CasinoMetrics
	cutoff := time.Now().UTC().Add(-CasinoAgeingMetricWindow)
	err := tx.QueryRow(ctx, `
		WITH `+casinoProvidersCTE+`
		SELECT
		  (SELECT count(*) FROM ledger_transactions b
		    WHERE b.tenant_id = $1 AND b.transaction_type = 'casino_bet' AND b.posted_at < $2
		      AND EXISTS (SELECT 1 FROM ledger_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
		                   WHERE e.ledger_transaction_id = b.id AND e.direction = 'debit' AND la.account_type = 'player_cash')
		      AND NOT EXISTS (SELECT 1 FROM ledger_transactions x WHERE x.reverses_transaction_id = b.id)
		      AND NOT EXISTS (SELECT 1 FROM ledger_transactions w
		                       WHERE w.tenant_id = b.tenant_id AND w.transaction_type = 'casino_win'
		                         AND w.provider_id = b.provider_id AND w.correlation_id = b.correlation_id)),
		  (SELECT count(*) FROM ledger_transactions ts
		    WHERE ts.tenant_id = $1 AND ts.transaction_type = 'tombstone'
		      AND ts.provider_id IN (SELECT provider_id FROM casino_providers)),
		  (SELECT count(*) FROM casino_callback_rejections WHERE tenant_id = $1),
		  (SELECT count(*) FROM casino_callback_rejections cr
		    WHERE cr.tenant_id = $1
		      AND NOT EXISTS (SELECT 1 FROM ledger_transactions t
		                       WHERE t.tenant_id = cr.tenant_id AND t.provider_id = cr.provider_id
		                         AND t.provider_tx_id = cr.provider_tx_id))`,
		tenantID, cutoff).Scan(&m.UnresolvedCashRoundsOlderThanWindow, &m.TombstonesTotal, &m.RejectionsTotal, &m.RejectionsUnposted)
	return m, err
}
