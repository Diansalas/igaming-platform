package reconciliation

// ADR 0113 s4.4 (slice 3): reconciliation check D, the standing DETECTIVE control of invariant #12
// ("every open player_withdrawal_hold credit matches exactly one non-terminal request of matching
// amount"; ledger-accounting-model.md row 12, withdrawal-state-machine.md s3). Slice 1's triggers (A,
// B-i, B-ii, WD043) are PREVENTIVE; D covers what triggers cannot: the table owner switching a trigger off (ADR 0110
// s9.4), history, and the residual forged-leg shapes of ADR 0113 s9.2.
//
// It is appended to RunLedgerVsProjection as a standing, unwindowed check (like the K2 unlinked
// manual adjustments), so it runs hourly through the ledger_vs_projection sweep for EVERY tenant,
// active or not (runLedgerStreamForTenant serves both sweepTenants and observeNonActiveTenants), and a
// finding raises the P1 through raiseLedgerRunAlerts (inside the stream's alerting.InTx closure, after
// the run's audit row; stable discriminator, run id as an attribute only, so a persisting condition is
// ONE open alert with growing occurrences).
//
// C7 (ledger-finance):
//   - ONE SQL statement: every count, balance and link rule is read from a single snapshot (a READ
//     COMMITTED statement snapshot), so a withdrawal committing during the run (request row and its
//     ledger leg commit atomically) is seen whole or not at all: no false P1. The ledger_vs_projection
//     transaction stays READ COMMITTED (its in-tx alert raise is not allowed under REPEATABLE READ,
//     SR-5).
//   - Rebuilt FROM ledger_entries, never from wallet_balance_projection.
//   - FAIL CLOSED (ErrWithdrawalHoldCheckBlind: a run error, the transaction rolls back, the run is
//     never recorded `clean`, and reconciliation.run_failed is raised) when the read is blind: the
//     session is not exactly the tenant scope (app.tenant_id = the run tenant, no player scope), or a
//     count read under the tenant's set_config is zero while an independently policied count is not:
//       (a) no withdrawal request visible while an E0b-shaped withdrawal_requested transaction
//           (idempotency_key = correlation_id, the shape B-i pins) is (an EXISTS probe);
//       (b) no E0b-shaped transaction visible while withdrawal requests are (every request carries one:
//           check A, WD031/WD032);
//       (c) no hold-account ledger entry visible while the hold projection rows show postings.
//     A TRUE owner-side count is not available to this credential: the runtime role has no BYPASSRLS
//     and FORCE RLS binds the table owner too, so the cross-table counts are the runtime equivalent.
//     A policy change that blinds every one of these tables at once is not detectable here (residual;
//     pinned instead by the whole-schema snapshot tests of every migration).
//
// Findings (append-only reconciliation_mismatches rows, P1):
//   - withdrawal_hold_open_mismatch: per (wallet, asset), the player_withdrawal_hold balance rebuilt
//     from ledger_entries (credit - debit) differs from the sum of the non-terminal requests
//     (requested, pending_review, approved, submitted) on that wallet and asset. Keyed by the hold
//     account id ("wallet:<id>:<asset>" when no hold account exists).
//   - withdrawal_request_ledger_link_invalid: the request fails check A's link rules (ADR 0113 s4.2,
//     the same rules as withdrawal_request_ledger_link_violation in migration 0132, set-wise and
//     read-only: WD030 not visible, WD031 no hold link, WD032 hold shape, WD033 release shape, WD034
//     correlated hold legs do not net to amount / 0), keyed by the request id; and (ledger-finance
//     review C1, the read-only B-ii) WD042: a player_withdrawal_hold leg whose transaction NO request
//     links as its hold or release (an anti-join of the tenant's hold-leg transactions against the
//     requests' two link columns), keyed "ledger_transaction:<id>". This also reports an unattributed
//     hold-leg pair that nets to zero on its wallet (no balance left), which the aggregate cannot see.
//     A5 (WITHDRAWAL-REVERSAL-1, migration 0162): the 'reversed' arm of check A (WD033 release = the
//     completion, WD035 the reversal link's exact withdrawal_reversed shape, parity-tested like the
//     rest), and the read-only WD044: a withdrawal_reversed transaction that no reversed request links
//     (keyed "ledger_transaction:<id>"), the detective twin of the 0162 commit-time trigger for an owner
//     who switched that trigger off (ADR 0110 s9.4).

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	// MismatchKindWithdrawalHoldOpenMismatch: invariant #12 aggregate drift (ADR 0113 s4.4).
	MismatchKindWithdrawalHoldOpenMismatch MismatchKind = "withdrawal_hold_open_mismatch"
	// MismatchKindWithdrawalRequestLedgerLinkInvalid: check A's link rules fail (read-only).
	MismatchKindWithdrawalRequestLedgerLinkInvalid MismatchKind = "withdrawal_request_ledger_link_invalid"
)

// ErrWithdrawalHoldCheckBlind is check D's fail-closed error (C7): the read could not see what it must.
var ErrWithdrawalHoldCheckBlind = errors.New("reconciliation: withdrawal hold check (ADR 0113 D) read blind: fail closed, the run is not recorded")

// withdrawalHoldCheckSQL is check D as ONE statement. $1 = the tenant. The first five columns are the
// visibility probe (identical on every row); the remaining four are one finding (NULL when none).
const withdrawalHoldCheckSQL = `
WITH
req AS (
    SELECT w.id, w.tenant_id, w.wallet_id, w.asset_code, w.amount, w.state, w.provider_id,
           w.hold_ledger_transaction_id, w.release_ledger_transaction_id, w.reversal_ledger_transaction_id,
           w.state IN ('rejected', 'cancelled', 'failed', 'completed', 'reversed') AS term
      FROM withdrawal_requests w
     WHERE w.tenant_id = $1
),
hold_bal AS (
    SELECT la.id, la.wallet_id, la.asset_code, count(e.id) AS n_entries,
           COALESCE(sum(CASE e.direction WHEN 'credit' THEN e.amount ELSE -e.amount END), 0) AS open_amount
      FROM ledger_accounts la
      LEFT JOIN ledger_entries e ON e.ledger_account_id = la.id AND e.tenant_id = la.tenant_id
     WHERE la.tenant_id = $1 AND la.account_type = 'player_withdrawal_hold'
     GROUP BY la.id, la.wallet_id, la.asset_code
),
open_req AS (
    SELECT r.wallet_id, r.asset_code, sum(r.amount) AS open_sum, count(*) AS n
      FROM req r WHERE NOT r.term
     GROUP BY r.wallet_id, r.asset_code
),
vis AS (
    SELECT (NULLIF(current_setting('app.tenant_id', true), '') IS NOT DISTINCT FROM $1::text
            AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL) AS sess_ok,
           (SELECT count(*) FROM req) AS n_req,
           (EXISTS (SELECT 1 FROM ledger_transactions t
                     WHERE t.tenant_id = $1 AND t.transaction_type = 'withdrawal_requested'
                       AND t.idempotency_key = t.correlation_id::text))::int::bigint AS n_e0b,
           (SELECT COALESCE(sum(n_entries), 0) FROM hold_bal) AS n_hold_entries,
           (SELECT count(*) FROM wallet_balance_projection p
             WHERE p.tenant_id = $1 AND p.account_type = 'player_withdrawal_hold'
               AND (p.credit_total > 0 OR p.debit_total > 0)) AS n_proj_active
),
open_mm AS (
    SELECT 'withdrawal_hold_open_mismatch'::text AS kind,
           COALESCE(b.id::text, 'wallet:' || o.wallet_id::text || ':' || o.asset_code) AS key,
           format('hold balance = sum of non-terminal requests = %s (requests=%s)',
                  COALESCE(o.open_sum, 0)::text, COALESCE(o.n, 0)::text) AS expected,
           format('hold balance rebuilt from ledger_entries = %s (wallet=%s asset=%s)',
                  COALESCE(b.open_amount, 0)::text, COALESCE(b.wallet_id, o.wallet_id)::text, COALESCE(b.asset_code, o.asset_code)) AS actual
      FROM hold_bal b
      FULL JOIN open_req o ON o.wallet_id = b.wallet_id AND o.asset_code = b.asset_code
     WHERE COALESCE(b.open_amount, 0) <> COALESCE(o.open_sum, 0)
),
linked AS (
    SELECT r.hold_ledger_transaction_id AS tx FROM req r WHERE r.hold_ledger_transaction_id IS NOT NULL
    UNION
    SELECT r.release_ledger_transaction_id FROM req r WHERE r.release_ledger_transaction_id IS NOT NULL
),
unlinked_mm AS (
    SELECT 'withdrawal_request_ledger_link_invalid'::text AS kind, 'ledger_transaction:' || u.tx::text AS key,
           'every player_withdrawal_hold leg belongs to a transaction a request links as its hold or release'::text AS expected,
           format('violation=WD042 unlinked hold leg transaction=%s', u.tx) AS actual
      FROM (SELECT DISTINCT e.ledger_transaction_id AS tx
              FROM ledger_entries e
              JOIN ledger_accounts la ON la.id = e.ledger_account_id
             WHERE la.tenant_id = $1 AND la.account_type = 'player_withdrawal_hold' AND e.tenant_id = $1) u
      LEFT JOIN linked l ON l.tx = u.tx
     WHERE l.tx IS NULL
),
unlinked_rev_mm AS (
    SELECT 'withdrawal_request_ledger_link_invalid'::text AS kind, 'ledger_transaction:' || t.id::text AS key,
           'every withdrawal_reversed transaction is the reversal link of a reversed request'::text AS expected,
           format('violation=WD044 unlinked withdrawal_reversed transaction=%s', t.id) AS actual
      FROM ledger_transactions t
     WHERE t.tenant_id = $1 AND t.transaction_type = 'withdrawal_reversed'
       AND NOT EXISTS (SELECT 1 FROM req r WHERE r.state = 'reversed' AND r.reversal_ledger_transaction_id = t.id)
),
link_mm AS (
    SELECT 'withdrawal_request_ledger_link_invalid'::text AS kind, r.id::text AS key,
           'check A link rules (ADR 0113 s4.2) hold for this request'::text AS expected,
           format('violation=%s state=%s', c.code, r.state) AS actual
      FROM req r
      LEFT JOIN ledger_transactions ht ON ht.id = r.hold_ledger_transaction_id AND ht.tenant_id = r.tenant_id
      LEFT JOIN LATERAL (
          SELECT count(*) AS n,
                 count(*) FILTER (WHERE e.direction = 'debit' AND la.account_type = 'player_cash'
                                    AND e.wallet_id = r.wallet_id AND e.asset_code = r.asset_code AND e.amount = r.amount) AS dr,
                 count(*) FILTER (WHERE e.direction = 'credit' AND la.account_type = 'player_withdrawal_hold'
                                    AND e.wallet_id = r.wallet_id AND e.asset_code = r.asset_code AND e.amount = r.amount) AS cr
            FROM ledger_entries e LEFT JOIN ledger_accounts la ON la.id = e.ledger_account_id
           WHERE e.ledger_transaction_id = ht.id) he ON true
      LEFT JOIN ledger_transactions rt ON rt.id = r.release_ledger_transaction_id AND rt.tenant_id = r.tenant_id
      LEFT JOIN LATERAL (
          SELECT count(*) AS n,
                 count(*) FILTER (WHERE e.direction = 'debit' AND la.account_type = 'player_withdrawal_hold'
                                    AND e.wallet_id = r.wallet_id AND e.asset_code = r.asset_code AND e.amount = r.amount) AS dr,
                 count(*) FILTER (WHERE e.direction = 'credit' AND e.asset_code = r.asset_code AND e.amount = r.amount
                                    AND CASE WHEN r.state IN ('completed', 'reversed')
                                             THEN la.account_type = 'psp_clearing' AND e.wallet_id IS NULL
                                             ELSE la.account_type = 'player_cash' AND e.wallet_id = r.wallet_id END) AS cr
            FROM ledger_entries e LEFT JOIN ledger_accounts la ON la.id = e.ledger_account_id
           WHERE e.ledger_transaction_id = rt.id) re ON true
      LEFT JOIN ledger_transactions vt ON vt.id = r.reversal_ledger_transaction_id AND vt.tenant_id = r.tenant_id
      LEFT JOIN LATERAL (
          SELECT count(*) AS n,
                 count(*) FILTER (WHERE e.direction = 'debit' AND la.account_type = 'psp_clearing' AND e.wallet_id IS NULL
                                    AND e.asset_code = r.asset_code AND e.amount = r.amount) AS dr,
                 count(*) FILTER (WHERE e.direction = 'credit' AND la.account_type = 'player_cash'
                                    AND e.wallet_id = r.wallet_id AND e.asset_code = r.asset_code AND e.amount = r.amount) AS cr
            FROM ledger_entries e LEFT JOIN ledger_accounts la ON la.id = e.ledger_account_id
           WHERE e.ledger_transaction_id = vt.id) ve ON true
      LEFT JOIN LATERAL (
          SELECT COALESCE(sum(CASE WHEN e.direction = 'credit' THEN e.amount ELSE -e.amount END), 0) AS s
            FROM ledger_transactions x
            JOIN ledger_entries e ON e.ledger_transaction_id = x.id
            JOIN ledger_accounts la ON la.id = e.ledger_account_id
           WHERE x.correlation_id = r.id AND x.tenant_id = r.tenant_id
             AND la.account_type = 'player_withdrawal_hold' AND e.wallet_id = r.wallet_id AND e.asset_code = r.asset_code) cs ON true
      CROSS JOIN LATERAL (SELECT CASE
          WHEN r.hold_ledger_transaction_id IS NULL THEN 'WD031'
          WHEN ht.id IS NULL THEN 'WD030'
          WHEN ht.transaction_type IS DISTINCT FROM 'withdrawal_requested' OR ht.idempotency_key IS DISTINCT FROM r.id::text
               OR ht.correlation_id IS DISTINCT FROM r.id OR ht.reverses_transaction_id IS NOT NULL THEN 'WD032'
          WHEN he.n = 0 THEN 'WD030'
          WHEN he.n <> 2 OR he.dr <> 1 OR he.cr <> 1 THEN 'WD032'
          WHEN NOT r.term AND r.release_ledger_transaction_id IS NOT NULL THEN 'WD033'
          WHEN r.term AND r.release_ledger_transaction_id IS NULL THEN 'WD033'
          WHEN r.term AND rt.id IS NULL THEN 'WD030'
          WHEN r.term AND NOT COALESCE(rt.correlation_id = r.id AND CASE r.state
              WHEN 'cancelled' THEN rt.transaction_type = 'withdrawal_rejected' AND rt.idempotency_key = r.id::text || ':cancelled'
                                    AND rt.reverses_transaction_id IS NOT DISTINCT FROM r.hold_ledger_transaction_id
              WHEN 'rejected'  THEN rt.transaction_type = 'withdrawal_rejected'
                                    AND rt.idempotency_key IN (r.id::text || ':rejected', r.id::text || ':kyc_denied', r.id::text || ':governed_hold_released')
                                    AND rt.reverses_transaction_id IS NOT DISTINCT FROM r.hold_ledger_transaction_id
              WHEN 'failed'    THEN rt.transaction_type = 'withdrawal_failed' AND rt.idempotency_key = r.id::text || ':failed'
                                    AND rt.reverses_transaction_id IS NOT DISTINCT FROM r.hold_ledger_transaction_id
              WHEN 'completed' THEN rt.transaction_type = 'withdrawal_completed' AND r.provider_id IS NOT NULL
                                    AND rt.provider_id = r.provider_id AND rt.idempotency_key = r.provider_id || ':' || rt.provider_tx_id
                                    AND rt.reverses_transaction_id IS NULL
              WHEN 'reversed'  THEN rt.transaction_type = 'withdrawal_completed' AND r.provider_id IS NOT NULL
                                    AND rt.provider_id = r.provider_id AND rt.idempotency_key = r.provider_id || ':' || rt.provider_tx_id
                                    AND rt.reverses_transaction_id IS NULL
              ELSE false END, false) THEN 'WD033'
          WHEN r.term AND re.n = 0 THEN 'WD030'
          WHEN r.term AND (re.n <> 2 OR re.dr <> 1 OR re.cr <> 1) THEN 'WD033'
          WHEN r.state <> 'reversed' AND r.reversal_ledger_transaction_id IS NOT NULL THEN 'WD035'
          WHEN r.state = 'reversed' AND r.reversal_ledger_transaction_id IS NULL THEN 'WD035'
          WHEN r.state = 'reversed' AND vt.id IS NULL THEN 'WD030'
          WHEN r.state = 'reversed' AND (vt.transaction_type IS DISTINCT FROM 'withdrawal_reversed'
               OR vt.idempotency_key IS DISTINCT FROM r.id::text || ':reversed' OR vt.correlation_id IS DISTINCT FROM r.id
               OR vt.reverses_transaction_id IS DISTINCT FROM r.release_ledger_transaction_id
               OR vt.provider_id IS DISTINCT FROM r.provider_id OR vt.provider_tx_id IS NULL) THEN 'WD035'
          WHEN r.state = 'reversed' AND ve.n = 0 THEN 'WD030'
          WHEN r.state = 'reversed' AND (ve.n <> 2 OR ve.dr <> 1 OR ve.cr <> 1) THEN 'WD035'
          WHEN cs.s <> CASE WHEN r.term THEN 0 ELSE r.amount END THEN 'WD034'
      END AS code) c
     WHERE c.code IS NOT NULL
)
SELECT v.sess_ok, v.n_req, v.n_e0b, v.n_hold_entries, v.n_proj_active, f.kind, f.key, f.expected, f.actual
  FROM vis v
  LEFT JOIN (SELECT * FROM open_mm UNION ALL SELECT * FROM link_mm UNION ALL SELECT * FROM unlinked_mm
             UNION ALL SELECT * FROM unlinked_rev_mm) f ON true
 ORDER BY f.kind, f.key`

// withdrawalHoldVisibility is check D's visibility probe, read in the same statement as the findings.
type withdrawalHoldVisibility struct {
	SessionOK                      bool
	Requests, E0bTxs, HoldEntries  int64
	HoldProjectionRowsWithPostings int64
}

// blind returns the fail-closed reason, or "" when the read can be trusted (C7).
func (v withdrawalHoldVisibility) blind() string {
	switch {
	case !v.SessionOK:
		return "the session is not exactly the tenant scope of the run (app.tenant_id / app.player_account_id)"
	case v.Requests == 0 && v.E0bTxs > 0:
		return "no withdrawal request is visible while withdrawal_requested hold transactions are"
	case v.Requests > 0 && v.E0bTxs == 0:
		return fmt.Sprintf("no withdrawal_requested hold transaction is visible while %d withdrawal requests are", v.Requests)
	case v.HoldEntries == 0 && v.HoldProjectionRowsWithPostings > 0:
		return fmt.Sprintf("no player_withdrawal_hold ledger entry is visible while %d hold projection rows show postings", v.HoldProjectionRowsWithPostings)
	}
	return ""
}

// withdrawalHoldFindings runs check D for tenantID in tx (the tenant's own scope) and returns its
// findings, or an error wrapping ErrWithdrawalHoldCheckBlind when the read is blind.
func withdrawalHoldFindings(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]Mismatch, error) {
	rows, err := tx.Query(ctx, withdrawalHoldCheckSQL, tenantID)
	if err != nil {
		return nil, fmt.Errorf("reconciliation: withdrawal hold check: %w", err)
	}
	defer rows.Close()
	var vis withdrawalHoldVisibility
	seen := false
	var out []Mismatch
	for rows.Next() {
		var kind, key, expected, actual *string
		if err := rows.Scan(&vis.SessionOK, &vis.Requests, &vis.E0bTxs, &vis.HoldEntries, &vis.HoldProjectionRowsWithPostings,
			&kind, &key, &expected, &actual); err != nil {
			return nil, fmt.Errorf("reconciliation: withdrawal hold check: %w", err)
		}
		seen = true
		if kind == nil {
			continue
		}
		out = append(out, Mismatch{
			ID:                  uuid.New(),
			TenantID:            tenantID,
			ReconciliationKey:   *key,
			ExpectedValue:       *expected,
			ActualValue:         *actual,
			MismatchKind:        MismatchKind(*kind),
			InvestigationStatus: investigationStatusOpen,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reconciliation: withdrawal hold check: %w", err)
	}
	if !seen {
		return nil, fmt.Errorf("%w: the probe returned no row", ErrWithdrawalHoldCheckBlind)
	}
	if reason := vis.blind(); reason != "" {
		return nil, fmt.Errorf("%w: %s", ErrWithdrawalHoldCheckBlind, reason)
	}
	return out, nil
}
