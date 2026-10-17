package reconciliation

// Ledger-finance review condition C3 / F4-R2-NEGBAL-SQL (detection half; migration 0174): the standing, READ-ONLY
// detective control for a negative player wallet and for a payout dispatched while the wallet was negative.
//
// Why it exists: the WD-RG-1 gate (owner decision 13, internal/rg walletBalanceNegative, WD050) refuses withdrawals,
// payout dispatch (T1p / T2 / T12), wagers, casino launch and bonus eligibility while a wallet is negative, but it is a
// PREVENTIVE check read before the claim's locks (ADR 0082 R8): a chargeback / deposit reversal that commits between
// the gate's read and the claim's commit, or between the claim's commit and the provider send, is not seen by it
// (closure-ledger-finance-review-record.md 1.3, residual R1). Before this check nothing reported a negative wallet or
// such a payout at all.
//
// It is appended to RunLedgerVsProjection exactly like check D (withdrawal_hold.go): standing and unwindowed, run
// hourly through the ledger_vs_projection sweep for EVERY tenant (active or observed), ONE SQL statement (one READ
// COMMITTED snapshot), tenant-scoped (explicit tenant_id predicates on top of RLS), FAIL CLOSED when the read is blind.
// It writes NOTHING but the run's own reconciliation_mismatches rows and the alert: never a ledger entry, never a
// projection row, never a withdrawal or attempt. The staff response is a governed compensating path (runbook
// payout-reversal-and-amount-dispute.md section 4.1), never a balance edit.
//
// Predicate (EXACTLY the gate's, so the detector and the gate agree): an account of type player_cash or
// player_locked_cash of a wallet whose balance is below zero, credit_total < debit_total (both credit-normal). It is
// evaluated on the ledger rebuilt from ledger_entries (authoritative) AND on wallet_balance_projection (what the gate
// reads); either being negative is a finding (the two can only differ under projection drift, which the same run
// reports as balance_mismatch). Holds are NOT netted: an open player_withdrawal_hold does not cure a negative
// player_cash. Netting would hide precisely the chargeback-with-open-withdrawal case (deposit 100, request 100 -> cash
// 0 / hold 100, chargeback 100 -> cash -100 / hold 100: net 0, yet paying that withdrawal pays out money the platform
// no longer has), which is the case C3 exists for. No production path can create an UNFUNDED hold (RequestWithdrawal
// checks player_cash >= amount under the L3 projection lock, withdrawal.go), so a funded hold never makes player_cash
// negative and produces no finding; an unfunded hold (only a test fixture, wdpath.Plant, can build one) is a state the
// gate itself refuses, and so does this check.
//
// Findings (append-only reconciliation_mismatches rows of the run; the alert is the standing signal):
//   - player_wallet_negative_balance: keyed by the negative ledger account id. Clears (no new row) on the first run
//     after the balance is back to >= 0 (a deposit or a governed K2 credit).
//   - payout_dispatched_while_wallet_negative: keyed by the withdrawal request id. A request in state submitted or
//     completed (money sent or in flight; failed / reversed / rejected / cancelled requests did not, or no longer,
//     pay out) on a wallet that is negative NOW (the first finding) and whose negative account's balance rebuilt from
//     the entries posted at or before the request's DISPATCH INSTANT was already negative. The dispatch instant is the
//     latest dispatch evidence of the request: the newest allowing WD-RG-1 dispatch decision (decided_at,
//     clock_timestamp() inside the claim, after the gate's read) and the payout attempt's first_submitted_at /
//     last_sent_at; the request's updated_at only when neither exists (legacy rows before migration 0169 / 0101).
//     It clears with the first finding (recovery of the receivable). A chargeback whose transaction STARTED after the
//     dispatch instant (ledger created_at is the posting transaction's start) is not attributed to that payout; the
//     wallet is still reported by the first finding (residual, recorded in docs/plans/c3-record.md).
//
// Alerts: KindReconciliationLedgerProjectionDrift (P1, an existing Kind, no alert_kinds change), one stable
// discriminator per finding kind and tenant (discLedgerWalletNegative, discLedgerPayoutWhileNegative), run id as an
// attribute only: a persisting condition is ONE open alert with growing occurrences, never one alert per run.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	// MismatchKindPlayerWalletNegativeBalance: a player_cash / player_locked_cash balance below zero (C3 (a)).
	MismatchKindPlayerWalletNegativeBalance MismatchKind = "player_wallet_negative_balance"
	// MismatchKindPayoutDispatchedWhileWalletNegative: a payout dispatched while its wallet was negative (C3 (b)).
	MismatchKindPayoutDispatchedWhileWalletNegative MismatchKind = "payout_dispatched_while_wallet_negative"
)

// ErrNegativeBalanceCheckBlind is the C3 check's fail-closed error: the read could not see what it must.
var ErrNegativeBalanceCheckBlind = errors.New("reconciliation: negative balance check (C3) read blind: fail closed, the run is not recorded")

// negativeBalanceGateDecisionTerm is the dispatch-decision evidence term, used only when withdrawal_gate_decisions
// exists (migration 0169; migration tests drive reconciliation on scratch schemas pinned below it).
const negativeBalanceGateDecisionTerm = `,
               (SELECT max(d.decided_at) FROM withdrawal_gate_decisions d
                 WHERE d.tenant_id = r.tenant_id AND d.withdrawal_request_id = r.id AND d.allowed)`

// gateDecisionTermToken marks where negativeBalanceSQL places negativeBalanceGateDecisionTerm (or nothing).
const gateDecisionTermToken = "/*GATE_DECISION_TERM*/"

// negativeBalanceCheckSQL is the C3 check as ONE statement. $1 = the tenant. The first three columns are the visibility probe (identical on every row); the remaining four are one finding
// (NULL when none).
const negativeBalanceCheckSQL = `
WITH
acct AS (
    SELECT la.id, la.wallet_id, la.asset_code, la.account_type, count(e.id) AS n_entries,
           COALESCE(sum(e.amount) FILTER (WHERE e.direction = 'debit'), 0) AS dr,
           COALESCE(sum(e.amount) FILTER (WHERE e.direction = 'credit'), 0) AS cr
      FROM ledger_accounts la
      LEFT JOIN ledger_entries e ON e.ledger_account_id = la.id AND e.tenant_id = la.tenant_id
     WHERE la.tenant_id = $1 AND la.account_type IN ('player_cash', 'player_locked_cash') AND la.wallet_id IS NOT NULL
     GROUP BY la.id, la.wallet_id, la.asset_code, la.account_type
),
neg AS (
    SELECT a.id, a.wallet_id, a.asset_code, a.account_type, a.dr, a.cr, p.debit_total AS p_dr, p.credit_total AS p_cr
      FROM acct a
      LEFT JOIN wallet_balance_projection p ON p.ledger_account_id = a.id AND p.tenant_id = $1
     WHERE a.cr < a.dr OR p.credit_total < p.debit_total
),
vis AS (
    SELECT (NULLIF(current_setting('app.tenant_id', true), '') IS NOT DISTINCT FROM $1::text
            AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL) AS sess_ok,
           (SELECT COALESCE(sum(n_entries), 0) FROM acct) AS n_entries,
           (SELECT count(*) FROM wallet_balance_projection p
             WHERE p.tenant_id = $1 AND p.account_type IN ('player_cash', 'player_locked_cash')
               AND (p.credit_total > 0 OR p.debit_total > 0)) AS n_proj_active
),
neg_mm AS (
    SELECT 'player_wallet_negative_balance'::text AS kind, n.id::text AS key,
           'player_cash / player_locked_cash balance >= 0 (owner decision 13, the WD050 gate predicate)'::text AS expected,
           format('balance=%s (ledger credit=%s debit=%s; projection credit=%s debit=%s) wallet=%s account_type=%s asset=%s',
                  (n.cr - n.dr)::text, n.cr::text, n.dr::text, COALESCE(n.p_cr::text, 'none'), COALESCE(n.p_dr::text, 'none'),
                  n.wallet_id::text, n.account_type, n.asset_code) AS actual
      FROM neg n
),
disp AS (
    SELECT r.id, r.wallet_id, r.state, r.amount, r.asset_code,
           COALESCE(GREATEST(
               (SELECT max(GREATEST(a.first_submitted_at, a.last_sent_at)) FROM payment_attempts a
                 WHERE a.tenant_id = r.tenant_id AND a.withdrawal_request_id = r.id AND a.operation = 'payout')/*GATE_DECISION_TERM*/
           ), r.updated_at) AS dispatched_at
      FROM withdrawal_requests r
     WHERE r.tenant_id = $1 AND r.state IN ('submitted', 'completed')
       AND r.wallet_id IN (SELECT wallet_id FROM neg)
),
after_mm AS (
    SELECT DISTINCT ON (r.id)
           'payout_dispatched_while_wallet_negative'::text AS kind, r.id::text AS key,
           'no payout is dispatched while its wallet is negative (WD050 at T1p / T2 / T12)'::text AS expected,
           format('request state=%s amount=%s asset=%s dispatched_at=%s; %s balance at dispatch=%s, now=%s (account=%s wallet=%s)',
                  r.state, r.amount::text, r.asset_code, to_char(r.dispatched_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
                  n.account_type, b.bal_at::text, (n.cr - n.dr)::text, n.id::text, r.wallet_id::text) AS actual
      FROM disp r
      JOIN neg n ON n.wallet_id = r.wallet_id
      CROSS JOIN LATERAL (
          SELECT COALESCE(sum(CASE e.direction WHEN 'credit' THEN e.amount ELSE -e.amount END), 0) AS bal_at
            FROM ledger_entries e
           WHERE e.ledger_account_id = n.id AND e.tenant_id = $1 AND e.created_at <= r.dispatched_at) b
     WHERE b.bal_at < 0
     ORDER BY r.id, b.bal_at
)
SELECT v.sess_ok, v.n_entries, v.n_proj_active, f.kind, f.key, f.expected, f.actual
  FROM vis v
  LEFT JOIN (SELECT * FROM neg_mm UNION ALL SELECT * FROM after_mm) f ON true
 ORDER BY f.kind, f.key`

// negativeBalanceSQL returns the statement for the schema tx sees.
func negativeBalanceSQL(ctx context.Context, tx pgx.Tx) (string, error) {
	var hasDecisions bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.withdrawal_gate_decisions') IS NOT NULL`).Scan(&hasDecisions); err != nil {
		return "", fmt.Errorf("reconciliation: negative balance check: %w", err)
	}
	term := ""
	if hasDecisions {
		term = negativeBalanceGateDecisionTerm
	}
	return strings.Replace(negativeBalanceCheckSQL, gateDecisionTermToken, term, 1), nil
}

// negativeBalanceVisibility is the C3 check's visibility probe, read in the same statement as the findings.
type negativeBalanceVisibility struct {
	SessionOK                  bool
	PlayerEntries              int64
	PlayerProjectionsWithPosts int64
}

// blind returns the fail-closed reason, or "" when the read can be trusted.
func (v negativeBalanceVisibility) blind() string {
	switch {
	case !v.SessionOK:
		return "the session is not exactly the tenant scope of the run (app.tenant_id / app.player_account_id)"
	case v.PlayerEntries == 0 && v.PlayerProjectionsWithPosts > 0:
		return fmt.Sprintf("no player_cash / player_locked_cash ledger entry is visible while %d projection rows show postings", v.PlayerProjectionsWithPosts)
	}
	return ""
}

// negativeBalanceFindings runs the C3 check for tenantID in tx (the tenant's own scope) and returns its findings, or
// an error wrapping ErrNegativeBalanceCheckBlind when the read is blind. It only reads.
func negativeBalanceFindings(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]Mismatch, error) {
	q, err := negativeBalanceSQL(ctx, tx)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("reconciliation: negative balance check: %w", err)
	}
	defer rows.Close()
	var vis negativeBalanceVisibility
	seen := false
	var out []Mismatch
	for rows.Next() {
		var kind, key, expected, actual *string
		if err := rows.Scan(&vis.SessionOK, &vis.PlayerEntries, &vis.PlayerProjectionsWithPosts, &kind, &key, &expected, &actual); err != nil {
			return nil, fmt.Errorf("reconciliation: negative balance check: %w", err)
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
		return nil, fmt.Errorf("reconciliation: negative balance check: %w", err)
	}
	if !seen {
		return nil, fmt.Errorf("%w: the probe returned no row", ErrNegativeBalanceCheckBlind)
	}
	if reason := vis.blind(); reason != "" {
		return nil, fmt.Errorf("%w: %s", ErrNegativeBalanceCheckBlind, reason)
	}
	return out, nil
}
