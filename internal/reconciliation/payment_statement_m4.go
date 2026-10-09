package reconciliation

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// PAY-PAYOUT-UNBOUND-RESOLVE-1 (ADR 0111 §4.6, revision 3 R-1/R-2, revision 4
// S-1): reconciliation of EXECUTED M4 resolutions. Executed M4 rows are read
// through tenant_system_read_executed (widened by migration 0125) in
// loadK3Evidence, which also adds their keys (R, merchant reference, bound
// reference, Y) to the bounded persisted-lines read (S-1). This file holds the
// two standing predicates (R-1); the ledger_join attribution (R-2) is in
// checkLedgerJoin.

// checkM4Standing raises the R-1 predicates for executed M4 resolutions,
// unwindowed, every run, in ANY import (MOCK and unsealed included),
// irrespective of occurred_at or import time - a back-dated contradiction
// counts. No new mismatch kind (R-1):
//   - m4_evidence_paid: pay_declared_paid_unconfirmed on any declined or
//     reversed payout line on R or on the merchant reference, or on a second
//     DISTINCT succeeded line (after cross-import dedupe);
//   - m4_evidence_not_paid: pay_declared_not_paid_but_paid on any succeeded
//     payout line on the merchant reference, the bound reference or a Y, until
//     executed compensating_entry debits with causation = the withdrawal_failed
//     transaction total at least the amount (the M2 (d) recovery rule).
//
// Raising only: nothing here clears, posts or changes state.
func (m *payMatcher) checkM4Standing(ctx context.Context, tx pgx.Tx) error {
	if len(m.k3.m4) == 0 {
		return nil
	}
	for _, r := range m.k3.m4 {
		a := m.payAttemptByID(r.attemptID)
		if a == nil {
			continue
		}
		switch r.kind {
		case m4KindPaid:
			var contra *persistedLine
			distinct := map[string]bool{}
			for _, l := range m.k3.payoutLinesOn([]string{r.reference}, r.attemptMerchnt) {
				switch l.status {
				case "declined", "reversed":
					if contra == nil {
						contra = l
					}
				case paymentStatementStatusSucceeded:
					distinct[strings.Join([]string{l.ref, l.amount.String(), l.asset, l.occurredAt.UTC().Format(time.RFC3339Nano)}, "\x00")] = true
				}
			}
			var why string
			switch {
			case contra != nil:
				why = fmt.Sprintf("contradicting line import=%s line_no=%d is_mock=%t status=%s reference=%s amount=%s asset=%s",
					contra.importID, contra.lineNo, contra.isMock, contra.status, contra.ref, contra.amount, contra.asset)
			case len(distinct) > 1:
				why = fmt.Sprintf("%d distinct succeeded payout lines on R or the merchant reference", len(distinct))
			default:
				continue
			}
			m.r.add(MismatchKindPayDeclaredPaidUnconfirmed, m.key("attempt="+a.id.String(), "resolution="+r.id.String(), "check=m4_paid_contradicted"),
				declaredPaidUnconfirmedResolutionHint,
				"platform: "+a.render()+"; M4 evidence paid, completion transaction="+r.ledgerTx.String()+" reference="+r.reference+"; "+why)
		case m4KindNotPaid:
			var evidence *persistedLine
			for _, l := range m.k3.payoutLinesOn([]string{r.pinnedRef, r.attemptRef, m.k3.yRef[r.attemptID]}, r.attemptMerchnt) {
				if l.status == paymentStatementStatusSucceeded {
					evidence = l
					break
				}
			}
			if evidence == nil {
				continue
			}
			var recoveredS string
			if err := tx.QueryRow(ctx, `
				SELECT COALESCE(sum(q.amount), 0)::text FROM ledger_adjustment_requests q
				 WHERE q.tenant_id = $1 AND q.state = 'executed' AND q.reason_code = 'compensating_entry'
				   AND q.direction = 'debit_player' AND q.causation_transaction_id = $2`, m.tenantID, r.ledgerTx).Scan(&recoveredS); err != nil {
				return fmt.Errorf("M4 not-paid recovery probe: %w", err)
			}
			recovered, err := parseBig(recoveredS)
			if err != nil {
				return err
			}
			if recovered.Cmp(r.amount) >= 0 {
				continue
			}
			m.r.add(MismatchKindPayDeclaredNotPaidButPaid, m.key("attempt="+a.id.String(), "resolution="+r.id.String(), "check=m4_not_paid_but_paid"),
				m4NotPaidCapturedUnpostedResolutionHint,
				fmt.Sprintf("platform: %s; M4 evidence not paid, withdrawal_failed transaction=%s; recovered=%s of %s; succeeded line import=%s line_no=%d is_mock=%t reference=%s amount=%s asset=%s",
					a.render(), r.ledgerTx, recovered, r.amount, evidence.importID, evidence.lineNo, evidence.isMock, evidence.ref, evidence.amount, evidence.asset))
		}
	}
	return nil
}

// payoutLinesOn returns the persisted PAYOUT lines named by any of refs (as
// provider reference) or by merchant, deduplicated.
func (e *k3Evidence) payoutLinesOn(refs []string, merchant string) []*persistedLine {
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
	for _, k := range refs {
		if k != "" {
			add(e.byRef[k])
		}
	}
	if merchant != "" {
		add(e.byMerchant[merchant])
	}
	return out
}
