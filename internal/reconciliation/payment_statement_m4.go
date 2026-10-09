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
//     reversed payout line on R or on the merchant reference, on a second
//     DISTINCT succeeded line (after cross-import dedupe), or on any payout
//     line on R naming another merchant reference (review amendment H-2);
//   - m4_evidence_not_paid: pay_declared_not_paid_but_paid on any succeeded
//     payout line on the merchant reference, the bound reference, a Y, the
//     evidence line's own reference D (review amendment H-1) or any other
//     matched reference (security LOW condition 1 / LF RR-4: notPaidLines),
//     until the ledger-finance RR-1 rule holds (m4NotPaidRecovered: the M2 (d)
//     recovery - executed compensating_entry debits with causation = the
//     withdrawal_failed transaction, same wallet and asset, totalling at least
//     the amount - over exactly one succeeded payout of the attempt's amount
//     and asset). STANDING-1 for the same attempt uses the same rule
//     (clearedRefFor -> m4NotPaidRecoveredLine).
//
// Raising only: nothing here clears, posts or changes state. ctx and tx are
// kept for the stream's call shape; every input was read by loadK3Evidence.
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
			var contra, other *persistedLine
			distinct := map[string]bool{}
			for _, l := range m.k3.payoutLinesOn([]string{r.reference}, r.attemptMerchnt) {
				// Review amendment H-2/sec, C-1/LF: a payout line on R naming
				// ANOTHER merchant reference (any status, any import) makes the
				// attribution of R ambiguous.
				if other == nil && l.ref == r.reference && l.merchant != "" && l.merchant != r.attemptMerchnt {
					other = l
				}
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
			case other != nil:
				why = fmt.Sprintf("payout line on R names another merchant reference: import=%s line_no=%d is_mock=%t status=%s reference=%s merchant=%s",
					other.importID, other.lineNo, other.isMock, other.status, other.ref, other.merchant)
			case len(distinct) > 1:
				why = fmt.Sprintf("%d distinct succeeded payout lines on R or the merchant reference", len(distinct))
			default:
				continue
			}
			m.r.add(MismatchKindPayDeclaredPaidUnconfirmed, m.key("attempt="+a.id.String(), "resolution="+r.id.String(), "check=m4_paid_contradicted"),
				declaredPaidUnconfirmedResolutionHint,
				"platform: "+a.render()+"; M4 evidence paid, completion transaction="+r.ledgerTx.String()+" reference="+r.reference+"; "+why)
		case m4KindNotPaid:
			// GOV-R32 (security C-1 / LF Q-R32-2; mirrors migration 0127's not-paid refusal): a payout line
			// on any reference R-1 reads (bound, Y, the evidence line's D, every matched reference) that names
			// ANOTHER merchant reference makes the attribution behind the executed not-paid ambiguous. Raise
			// only; it is not subject to the RR-1 stop rule (it is not a recovered payout).
			for _, l := range m.k3.notPaidLines(&r) {
				if l.merchant != "" && l.merchant != r.attemptMerchnt {
					m.r.add(MismatchKindPayDeclaredNotPaidButPaid, m.key("attempt="+a.id.String(), "resolution="+r.id.String(), "check=m4_not_paid_attribution_ambiguous"),
						m4NotPaidCapturedUnpostedResolutionHint,
						fmt.Sprintf("platform: %s; M4 evidence not paid, withdrawal_failed transaction=%s; payout line names another merchant reference: import=%s line_no=%d is_mock=%t status=%s reference=%s merchant=%s",
							a.render(), r.ledgerTx, l.importID, l.lineNo, l.isMock, l.status, l.ref, l.merchant))
					break
				}
			}
			var evidence *persistedLine
			for _, l := range m.k3.notPaidLines(&r) {
				if l.status == paymentStatementStatusSucceeded {
					evidence = l
					break
				}
			}
			if evidence == nil {
				continue
			}
			// The ONE stop rule, shared with STANDING-1 (m4NotPaidRecoveredLine):
			// ledger-finance ruling RR-1 (ADR 0111 §19).
			if _, ok := m.m4NotPaidRecovered(a); ok {
				continue
			}
			recovered := "<none>"
			if r.recovered != nil {
				recovered = r.recovered.String()
			}
			m.r.add(MismatchKindPayDeclaredNotPaidButPaid, m.key("attempt="+a.id.String(), "resolution="+r.id.String(), "check=m4_not_paid_but_paid"),
				m4NotPaidCapturedUnpostedResolutionHint,
				fmt.Sprintf("platform: %s; M4 evidence not paid, withdrawal_failed transaction=%s; recovered=%s of %s; distinct succeeded lines=%d; succeeded line import=%s line_no=%d is_mock=%t reference=%s amount=%s asset=%s",
					a.render(), r.ledgerTx, recovered, r.amount, len(m.k3.succeededGroups(&r)), evidence.importID, evidence.lineNo, evidence.isMock, evidence.ref, evidence.amount, evidence.asset))
		}
	}
	return nil
}

// notPaidLines is every persisted payout line R-1 reads for an executed M4
// not-paid: the merchant reference, the bound reference (pinned and current),
// Y, the evidence line's own reference D and - security LOW condition 1 /
// LF RR-4 - every matched reference (matchedRefs, the verdict's v_rs).
func (e *k3Evidence) notPaidLines(r *m4Resolution) []*persistedLine {
	refs := append([]string{r.pinnedRef, r.attemptRef, e.yRef[r.attemptID], r.evidenceRef}, r.matchedRefs...)
	return e.payoutLinesOn(refs, r.attemptMerchnt)
}

// succeededGroups groups the succeeded lines of notPaidLines(r) after the
// cross-import dedupe of R19-5 (provider_reference, amount, asset_code,
// occurred_at): one group is one PSP payout, however often it is re-delivered.
func (e *k3Evidence) succeededGroups(r *m4Resolution) map[string]*persistedLine {
	groups := map[string]*persistedLine{}
	for _, l := range e.notPaidLines(r) {
		if l.status != paymentStatementStatusSucceeded {
			continue
		}
		k := strings.Join([]string{l.ref, l.amount.String(), l.asset, l.occurredAt.UTC().Format(time.RFC3339Nano)}, "\x00")
		if groups[k] == nil {
			groups[k] = l
		}
	}
	return groups
}

// m4NotPaidRecovered is the ledger-finance ruling RR-1 (review of 2026-10-09,
// ADR 0111 §19). For an attempt whose withdrawal an EXECUTED
// m4_evidence_not_paid failed, the post-M4 findings may stop raising ONLY if
// ALL of these hold; anything else keeps raising:
//
//	(a) the M2 (d) recovery rule: executed compensating_entry debit_player
//	    requests whose causation is THIS resolution's withdrawal_failed
//	    transaction, on the withdrawal's wallet and the resolution's asset,
//	    total at least the resolution's amount (r.recovered, loadK3Evidence);
//	(b) the late succeeded payout is ONE payout - exactly one distinct
//	    succeeded line group across every reference R-1 reads, re-deliveries
//	    collapsing into it - and its amount and asset equal the attempt's
//	    (the recovery covers one payout of the attempt's amount: a second,
//	    NEW succeeded line, or an unequal one, keeps raising);
//	(c) tenant and provider scoping: the resolution, the attempt and every
//	    line come from this run's tenant- and provider-bound reads, the
//	    resolution is this attempt's, and its amount and asset are the
//	    attempt's (DB-forced, R-4; re-checked, fail closed).
//
// It returns the single succeeded line on success. It reads no state the
// run did not already load and changes nothing: no row is written, cleared
// or deleted (earlier runs' mismatch rows stay as history).
func (m *payMatcher) m4NotPaidRecovered(a *payAttempt) (*persistedLine, bool) {
	if m.k3 == nil || a == nil || a.operation != paymentStatementKindPayout {
		return nil, false
	}
	r := m.k3.m4NotPaid[a.id]
	if r == nil || r.attemptID != a.id || r.kind != m4KindNotPaid {
		return nil, false
	}
	// (c) the resolution's amount and asset are the attempt's.
	if r.amount == nil || a.amount == nil || r.asset != a.asset || r.amount.Cmp(a.amount) != 0 {
		return nil, false
	}
	// (a) full recovery with the causation, wallet and asset.
	if r.recovered == nil || r.recovered.Cmp(r.amount) < 0 {
		return nil, false
	}
	// (b) exactly one payout, of the attempt's amount and asset.
	groups := m.k3.succeededGroups(r)
	if len(groups) != 1 {
		return nil, false
	}
	var single *persistedLine
	for _, l := range groups {
		single = l
	}
	if single.asset != a.asset || single.amount == nil || single.amount.Cmp(a.amount) != 0 {
		return nil, false
	}
	return single, true
}

// m4NotPaidRecoveredLine is the STANDING-1 (pay_captured_unposted) side of
// RR-1: a finding keyed on the evidencing line (ref, line) stops raising only
// when m4NotPaidRecovered holds AND that line IS the single recovered payout
// (same reference, amount and asset). The bound sites pass no line (nil) and
// never stop here: BOUND-CLEAR-1 is outside the ruling.
func (m *payMatcher) m4NotPaidRecoveredLine(a *payAttempt, ref string, line *evidencingLine) bool {
	if line == nil || line.amount == nil || ref == "" {
		return false
	}
	single, ok := m.m4NotPaidRecovered(a)
	if !ok {
		return false
	}
	return single.ref == ref && single.asset == line.asset && single.amount.Cmp(line.amount) == 0
}

// payoutLinesOn returns the persisted PAYOUT lines named by any of refs (as
// provider reference, including the lines read only as a matched reference)
// or by merchant, deduplicated. Exact equality only.
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
			add(e.byMatchedRef[k])
		}
	}
	if merchant != "" {
		add(e.byMerchant[merchant])
	}
	return out
}
