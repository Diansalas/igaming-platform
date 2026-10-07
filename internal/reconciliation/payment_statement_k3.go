package reconciliation

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// PRH-2 K3 (ADR 0101 revision 4 + section 26): the payment_statement stream's
// persisted-evidence substrate and the M2 standing kinds.
//
//   - ONE cross-import lookup over payment_statement_lines (loadK3Evidence,
//     ADR 0101 9.3), through migration 0115's indexes. It runs in the stream's
//     own REPEATABLE READ WithTenantSnapshot session with an explicit
//     tenant_id predicate (RLS is the second line), never truncates with a
//     LIMIT (a cap would have to fail the run loudly, never drop evidence:
//     INV-M-5), and never reads another tenant's lines (C-14d).
//   - Clearing and confirming evidence comes only from ledger rows, persisted
//     statement lines and the typed evidence table - never from audit JSON
//     (INV-M-6).
//   - D-4 (as amended by section 26 RC-3): a MOCK import's lines may clear or
//     confirm only when NO is_mock = false import exists for (tenant,
//     provider). Raising may use any import.
//   - Three standing kinds keep every M2 risk visible, each UNWINDOWED, every
//     run: pay_declared_paid_unconfirmed, pay_declared_not_paid_but_paid,
//     pay_declared_paid_compensated_but_paid.
//
// An M1 resolution never reaches this file: M1 writes nothing the stream
// reads for emission (LF-3). Resolutions are read only as EXECUTED M2 rows,
// through the tenant_system_read_executed policy (R-2).

// The three M2 standing kinds (migration 0115; LF K3-a).
const (
	MismatchKindPayDeclaredPaidUnconfirmed        MismatchKind = "pay_declared_paid_unconfirmed"
	MismatchKindPayDeclaredNotPaidButPaid         MismatchKind = "pay_declared_not_paid_but_paid"
	MismatchKindPayDeclaredPaidCompensatedButPaid MismatchKind = "pay_declared_paid_compensated_but_paid"
	reservedOperatorPrefix                                     = "platform-operator-declared:"
	disputeReasonSuccessAfterPayoutDeclined                    = "success_after_payout_declined"
	paymentStatementKindPayout                                 = "payout"
	paymentStatementKindDeposit                                = "deposit"
	paymentStatementStatusSucceeded                            = "succeeded"
	m2KindPaid                                                 = "m2_declare_paid"
	m2KindNotPaid                                              = "m2_declare_not_paid"
	capturedUnpostedResolutionHint                             = "resolution: a PSP-initiated reversal/tombstone on this line's reference, or allocation (LEDGER-SUSPENSE-B-1); M1 only acknowledges"
	boundCapturedUnpostedResolutionHint                        = "resolution: a PSP-initiated reversal/tombstone, or allocation (LEDGER-SUSPENSE-B-1); M1 only acknowledges"
	// payoutCapturedUnpostedResolutionHint is the PAYOUT reading of
	// pay_captured_unposted (ADR 0095 §35.2 PO-1, PAY-PAYOUT-UNBOUND-STANDING-1):
	// "the provider may have paid out and the platform posted no completion".
	// Allocation is NEVER a route for a payout (it would credit player cash for
	// money the PSP already paid out: a double payout); the resolution path
	// (PAY-PAYOUT-UNBOUND-RESOLVE-1) is NOT IMPLEMENTED (ADR 0101 R-K3-8).
	payoutCapturedUnpostedResolutionHint         = "resolution: PSP-side recall/return or governed completion against the hold (NOT IMPLEMENTED, R-K3-8); never allocation; M1 only acknowledges"
	declaredPaidUnconfirmedResolutionHint        = "resolution: a confirming statement line from an eligible import (payout, succeeded, resolved to this attempt, amount and asset equal) or a withdrawal reversal (WITHDRAWAL-REVERSAL-1); a compensating credit annotates but does not clear"
	declaredNotPaidButPaidResolutionHint         = "resolution: executed compensating_entry debits, causation = the withdrawal_failed transaction, totalling at least the withdrawn amount; an off-platform recovery has no clearing path and is tracked through the row's investigation status"
	declaredPaidCompensatedButPaidResolutionHint = "resolution: executed compensating_entry debits, causation = the compensating credit's own transaction, totalling at least the credited amount"
)

var reconciliationMeter = otel.Meter("github.com/Diansalas/igaming-platform/internal/reconciliation")

// paymentM2DeclaredPaidConfirmedTotal counts confirmations of an M2 "declare
// paid" by a matching statement line (ADR 0101 9.1 (b)). No tenant, provider or
// attempt label (bounded cardinality).
var paymentM2DeclaredPaidConfirmedTotal, _ = reconciliationMeter.Int64Counter("payment_m2_declared_paid_confirmed_total",
	metric.WithDescription("M2 declare-paid resolutions confirmed by a matching succeeded statement line (PRH-2 K3)."))

// persistedLine is one persisted statement line, deduplicated across
// overlapping imports.
type persistedLine struct {
	importID   uuid.UUID
	lineNo     int
	isMock     bool
	kind       string
	ref        string
	merchant   string
	original   string
	status     string
	asset      string
	amount     *big.Int
	occurredAt time.Time
	// eligible: the line may CLEAR or CONFIRM (D-4 / RC-3): its import is not
	// MOCK, or no non-MOCK import exists for (tenant, provider). When the same
	// line appears in several imports it is eligible if ANY copy is.
	eligible bool
}

// m2Resolution is one executed M2 resolution with the attempt it resolved.
type m2Resolution struct {
	id             uuid.UUID
	attemptID      uuid.UUID
	kind           string
	ledgerTx       uuid.UUID
	amount         *big.Int
	asset          string
	attemptState   string
	attemptReason  string
	attemptRef     string
	attemptMerchnt string
}

// k3Evidence is everything the K3 additions need beyond the run's own import.
type k3Evidence struct {
	importIsMock bool
	hasRealImp   bool
	// reversalOriginal: references named as the original of a deposit_reversal
	// line in an ELIGIBLE persisted import (S2/S3/S4 clearing).
	reversalOriginal map[string]bool
	// yRef: attempt id -> the poll's returned reference Y (typed evidence only).
	yRef map[uuid.UUID]string
	// lines: persisted lines by provider reference and by merchant reference.
	byRef      map[string][]*persistedLine
	byMerchant map[string][]*persistedLine
	m2         []m2Resolution
	// m2Paid: attempt ids of executed declare-paid resolutions (the
	// confirmation metric in matchPayment).
	m2Paid map[uuid.UUID]bool
	// capturedSeen: (attempt, evidencing reference) pairs already reported this
	// run (S6: one finding per exposure per run).
	capturedSeen map[string]bool
}

// clearEligible reports whether lines of an import with the given is_mock may
// clear or confirm.
func (e *k3Evidence) clearEligible(importIsMock bool) bool {
	return !importIsMock || !e.hasRealImp
}

// loadK3Evidence runs the ADR 0101 9.3 lookups inside the stream's snapshot
// transaction. importIsMock is the run's own import.
func (m *payMatcher) loadK3Evidence(ctx context.Context, tx pgx.Tx, importIsMock bool) error {
	e := &k3Evidence{
		importIsMock:     importIsMock,
		reversalOriginal: map[string]bool{}, yRef: map[uuid.UUID]string{},
		byRef: map[string][]*persistedLine{}, byMerchant: map[string][]*persistedLine{},
		m2Paid: map[uuid.UUID]bool{}, capturedSeen: map[string]bool{},
	}
	m.k3 = e

	// RC-3: MOCK evidence may clear/confirm only when no non-MOCK import exists.
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM payment_statement_imports WHERE tenant_id = $1 AND provider_id = $2 AND NOT is_mock)`,
		m.tenantID, m.provider).Scan(&e.hasRealImp); err != nil {
		return fmt.Errorf("real-import probe: %w", err)
	}

	// The typed Y evidence (never audit JSON).
	rows, err := tx.Query(ctx, `SELECT attempt_id, reference FROM payment_attempt_reference_evidence
		WHERE tenant_id = $1 AND provider_id = $2 AND evidence_kind = 'poll_returned_reference'`, m.tenantID, m.provider)
	if err != nil {
		return fmt.Errorf("reference evidence: %w", err)
	}
	for rows.Next() {
		var id uuid.UUID
		var y string
		if err := rows.Scan(&id, &y); err != nil {
			rows.Close()
			return err
		}
		e.yRef[id] = y
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// Executed M2 resolutions (the tenant_system_read_executed policy).
	rows, err = tx.Query(ctx, `
		SELECT r.id, r.attempt_id, r.kind, r.ledger_transaction_id, r.amount::text, r.asset_code,
		       a.state, COALESCE(a.terminal_reason, ''), COALESCE(a.provider_reference, ''), a.merchant_reference
		  FROM payment_manual_resolutions r
		  JOIN payment_attempts a ON a.id = r.attempt_id AND a.tenant_id = r.tenant_id
		 WHERE r.tenant_id = $1 AND a.provider_id = $2 AND r.state = 'executed'
		   AND r.kind IN ('m2_declare_paid', 'm2_declare_not_paid') AND r.ledger_transaction_id IS NOT NULL
		 ORDER BY r.id`, m.tenantID, m.provider)
	if err != nil {
		return fmt.Errorf("executed M2 resolutions: %w", err)
	}
	for rows.Next() {
		var r m2Resolution
		var amount string
		if err := rows.Scan(&r.id, &r.attemptID, &r.kind, &r.ledgerTx, &amount, &r.asset, &r.attemptState, &r.attemptReason, &r.attemptRef, &r.attemptMerchnt); err != nil {
			rows.Close()
			return err
		}
		if r.amount, err = parseBig(amount); err != nil {
			rows.Close()
			return err
		}
		e.m2 = append(e.m2, r)
		if r.kind == m2KindPaid {
			e.m2Paid[r.attemptID] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// The references and merchant references the run needs evidence for.
	refs, merchants := map[string]bool{}, map[string]bool{}
	for _, a := range m.attempts {
		if a.state != "disputed" {
			continue
		}
		c := a.captureClass()
		switch a.operation {
		case paymentStatementKindDeposit:
			if c == reasonBound && a.providerRef != "" {
				refs[a.providerRef] = true
			}
			if c == reasonUnbound {
				merchants[a.merchantRef] = true
				if a.providerRef != "" {
					refs[a.providerRef] = true
				}
			}
		case paymentStatementKindPayout:
			// PAY-PAYOUT-UNBOUND-STANDING-1 (ADR 0095 §35.2 PO-1): an unbound
			// payout park gets standing coverage from persisted PAYOUT lines
			// (checkStandingUnbound). Bound-class payout parks keep their
			// existing rule (checkUnmatchedAttempts) and need no lines here.
			if c == reasonUnbound {
				merchants[a.merchantRef] = true
				if a.providerRef != "" {
					refs[a.providerRef] = true
				}
			}
		}
	}
	for _, y := range e.yRef {
		refs[y] = true
	}
	for _, r := range e.m2 {
		if r.attemptRef != "" {
			refs[r.attemptRef] = true
		}
		merchants[r.attemptMerchnt] = true
	}

	// Persisted lines across ALL imports of (tenant, provider), no LIMIT.
	if len(refs) > 0 || len(merchants) > 0 {
		rows, err = tx.Query(ctx, `
			SELECT l.import_id, l.line_no, i.is_mock, l.kind, l.provider_reference, COALESCE(l.merchant_reference, ''),
			       COALESCE(l.original_provider_reference, ''), l.status, l.amount::text, l.asset_code, l.occurred_at
			  FROM payment_statement_lines l
			  JOIN payment_statement_imports i ON i.id = l.import_id AND i.tenant_id = l.tenant_id
			 WHERE l.tenant_id = $1 AND l.provider_id = $2 AND l.kind IN ('deposit', 'payout')
			   AND (l.provider_reference = ANY($3) OR l.merchant_reference = ANY($4))
			 ORDER BY l.provider_reference, l.kind, l.status, l.occurred_at, i.fetched_at, l.import_id, l.line_no`,
			m.tenantID, m.provider, keysOf(refs), keysOf(merchants))
		if err != nil {
			return fmt.Errorf("persisted statement lines: %w", err)
		}
		seen := map[string]*persistedLine{}
		for rows.Next() {
			l := &persistedLine{}
			var amount string
			if err := rows.Scan(&l.importID, &l.lineNo, &l.isMock, &l.kind, &l.ref, &l.merchant, &l.original, &l.status, &amount, &l.asset, &l.occurredAt); err != nil {
				rows.Close()
				return err
			}
			if l.amount, err = parseBig(amount); err != nil {
				rows.Close()
				return err
			}
			l.eligible = e.clearEligible(l.isMock)
			// Overlapping imports repeat the same statement line: dedupe on the
			// content key; keep the first copy, and make it eligible if any copy is.
			k := strings.Join([]string{l.kind, l.ref, l.merchant, l.status, l.amount.String(), l.asset, l.occurredAt.UTC().Format(time.RFC3339Nano)}, "\x00")
			if prev, ok := seen[k]; ok {
				prev.eligible = prev.eligible || l.eligible
				continue
			}
			seen[k] = l
			e.byRef[l.ref] = append(e.byRef[l.ref], l)
			if l.merchant != "" {
				e.byMerchant[l.merchant] = append(e.byMerchant[l.merchant], l)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}

	// Reversal lines naming a reference we may need to clear (S2-S4): the held
	// references, Y, and every evidencing line's own reference. ELIGIBLE imports
	// only (D-4), COMPLETED reversals only (reversalCompleted, B4 review C3); a
	// tombstone is read from the ledger (loadPlatform).
	need := map[string]bool{}
	for r := range refs {
		need[r] = true
	}
	for _, ls := range e.byMerchant {
		for _, l := range ls {
			need[l.ref] = true
		}
	}
	if len(need) > 0 {
		rows, err = tx.Query(ctx, `
			SELECT DISTINCT l.original_provider_reference
			  FROM payment_statement_lines l
			  JOIN payment_statement_imports i ON i.id = l.import_id AND i.tenant_id = l.tenant_id
			 WHERE l.tenant_id = $1 AND l.provider_id = $2 AND l.kind = 'deposit_reversal'
			   AND l.status IN ('succeeded', 'reversed')
			   AND l.original_provider_reference = ANY($3)
			   AND (NOT i.is_mock OR NOT $4)`,
			m.tenantID, m.provider, keysOf(need), e.hasRealImp)
		if err != nil {
			return fmt.Errorf("persisted reversal lines: %w", err)
		}
		for rows.Next() {
			var o string
			if err := rows.Scan(&o); err != nil {
				rows.Close()
				return err
			}
			e.reversalOriginal[o] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

func keysOf(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// linesFor returns the persisted lines of kind that resolve to attempt a: by
// provider reference first, then by merchant reference (ADR 0101 9.1 (b)).
func (e *k3Evidence) linesFor(a *payAttempt, kind string) []*persistedLine {
	var out []*persistedLine
	seen := map[*persistedLine]bool{}
	if a.providerRef != "" {
		for _, l := range e.byRef[a.providerRef] {
			if l.kind == kind && !seen[l] {
				seen[l] = true
				out = append(out, l)
			}
		}
	}
	for _, l := range e.byMerchant[a.merchantRef] {
		if l.kind == kind && !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	return out
}

// confirming is D-5's confirming-line definition (used to CLEAR (c) and to
// count the confirmation metric): kind payout, succeeded, same tenant and
// provider (the query), resolved to the attempt, amount AND asset equal to the
// attempt's, from an eligible import.
func confirmingLine(l *persistedLine, a *payAttempt) bool {
	return l.kind == paymentStatementKindPayout && l.status == paymentStatementStatusSucceeded && l.eligible &&
		l.asset == a.asset && l.amount.Cmp(a.amount) == 0
}

// resolvesTo is the CLEARING resolution order of D-5 / ADR 0101 9.1(b) (LF F-1,
// security PM-S2): a persisted line may confirm attempt a only when it resolves to
// a by provider reference, or by merchant reference when NO other attempt of the
// same kind holds the line's own reference. Another live payout's line (carrying
// that payout's reference) can therefore never clear a's finding by borrowing a's
// merchant reference. Raising predicates keep the broad linesFor union.
func (m *payMatcher) resolvesTo(l *persistedLine, a *payAttempt) bool {
	if a.providerRef != "" && l.ref == a.providerRef {
		return true
	}
	if l.merchant == "" || l.merchant != a.merchantRef {
		return false
	}
	holder := m.byRef[l.kind+"\x00"+l.ref]
	return holder == nil || holder.id == a.id
}

// payAttemptByID finds a loaded attempt.
func (m *payMatcher) payAttemptByID(id uuid.UUID) *payAttempt {
	for _, a := range m.attempts {
		if a.id == id {
			return a
		}
	}
	return nil
}

// clearedRef is the shared clearing rule for one reference (S2-S4): a
// deposit_reversal line naming it as original in this run (when this run's
// import may clear) or in any eligible persisted import, or a tombstone ledger
// row on (provider, reference). Nothing else clears it - never an M1 decision.
func (m *payMatcher) clearedRef(ref string) bool {
	if ref == "" {
		return false
	}
	return m.reversalOriginals[ref] || m.ledgerByRef["tombstone\x00"+ref] != nil
}

// capturedKey is the S6 dedupe key.
func capturedKey(a *payAttempt, ref string) string { return a.id.String() + "\x00" + ref }

// markCaptured records (attempt, reference) as reported this run; false when it
// already was (so the caller skips a second finding).
func (m *payMatcher) markCaptured(a *payAttempt, ref string) bool {
	k := capturedKey(a, ref)
	if m.k3.capturedSeen[k] {
		return false
	}
	m.k3.capturedSeen[k] = true
	return true
}

// clearedRefFor is the per-OPERATION clearing rule for an unbound park's
// finding keyed on the evidencing line's reference ref.
//
//   - deposit: clearedRef (a completed deposit_reversal line naming ref, or a
//     tombstone on ref) - unchanged.
//   - payout (PAY-PAYOUT-UNBOUND-STANDING-1, ADR 0095 §35.2 PO-1): a
//     deposit-shaped signal (a deposit_reversal line, a tombstone, even naming
//     the same reference) NEVER clears: in the reverse-collision case the PSP
//     refund of a real deposit R says nothing about whether a payout under R
//     left the platform. The only clearing is payoutCompletedRef.
//
// Any other operation never clears (fail closed).
func (m *payMatcher) clearedRefFor(a *payAttempt, ref string) bool {
	switch a.operation {
	case paymentStatementKindDeposit:
		return m.clearedRef(ref)
	case paymentStatementKindPayout:
		return m.payoutCompletedRef(a, ref)
	}
	return false
}

// payoutCompletedRef: a withdrawal_completed ledger posting of this provider
// keyed by ref (the governed completion against the hold, or the PSP's own
// settlement reference) exists, and it is ATTRIBUTABLE to a: no OTHER attempt
// holds ref as its payout settlement reference or as its payout provider
// reference. Borrowed attribution may raise, never clear (the resolvesTo /
// yAttributable precedent): another payout's completion under the same
// reference says nothing about a's hold. No new line kind or schema; a
// future payout_return line kind is a schema change out of scope here.
func (m *payMatcher) payoutCompletedRef(a *payAttempt, ref string) bool {
	if ref == "" || m.ledgerByRef["withdrawal_completed\x00"+ref] == nil {
		return false
	}
	if h := m.bySettlement[ref]; h != nil && h != a {
		return false
	}
	if h := m.byRef[paymentStatementKindPayout+"\x00"+ref]; h != nil && h != a {
		return false
	}
	return true
}

// capturedUnpostedHintFor returns the operator text for a pay_captured_unposted
// finding on attempt a: the deposit wording (ADR 0101 F13) for a deposit, and
// the payout reading (never allocation) for a payout.
func capturedUnpostedHintFor(a *payAttempt, depositHint string) string {
	if a.operation == paymentStatementKindPayout {
		return payoutCapturedUnpostedResolutionHint
	}
	return depositHint
}

// checkStandingUnbound is S1 (STANDING-1): every disputed attempt the D2F-1
// runtime rule treats as unbound is reported on EVERY run, unwindowed, while
// ANY persisted import of this tenant and provider has a succeeded line OF THE
// ATTEMPT'S OWN OPERATION resolving to it (by merchant reference, or by
// reference should the attempt hold one) that is not cleared under that
// operation's rule (clearedRefFor; S2 for deposits). The detail records the
// evidencing import, its line number, whether that import is MOCK and the
// attempt that holds the line's own reference (the N1 residual, C-34b).
//
// PAY-PAYOUT-UNBOUND-STANDING-1 (ADR 0095 §35.2 PO-1): payouts run the same
// rule per operation - linesFor(a, "payout"), the "payout\x00" holder
// namespace, the payout clearing rule and the payout wording. A deposit line
// never evidences a payout park and a payout line never evidences a deposit
// park.
func (m *payMatcher) checkStandingUnbound() {
	for _, a := range m.attempts {
		if !a.unboundPark() {
			continue
		}
		var kind string
		switch a.operation {
		case paymentStatementKindDeposit:
			kind = paymentStatementKindDeposit
		case paymentStatementKindPayout:
			kind = paymentStatementKindPayout
		default:
			continue
		}
		for _, l := range m.k3.linesFor(a, kind) {
			if l.status != paymentStatementStatusSucceeded || m.clearedRefFor(a, l.ref) {
				continue
			}
			if !m.markCaptured(a, l.ref) {
				continue
			}
			holder := "none"
			if h := m.byRef[kind+"\x00"+l.ref]; h != nil {
				holder = h.id.String()
			} else if h := m.bySettlement[l.ref]; kind == paymentStatementKindPayout && h != nil {
				holder = h.id.String()
			}
			m.r.add(MismatchKindPayCapturedUnposted, m.key("attempt="+a.id.String(), "provider_reference="+orNone(l.ref), "check=captured_unposted"),
				capturedUnpostedHintFor(a, capturedUnpostedResolutionHint),
				"platform: "+a.render()+" terminal_reason="+a.terminalReason+fmt.Sprintf("; standing: persisted line import=%s line_no=%d is_mock=%t holder_attempt=%s; ", l.importID, l.lineNo, l.isMock, holder)+
					m.label+fmt.Sprintf("kind=%s status=%s amount=%s asset=%s reference=%s", l.kind, l.status, l.amount, l.asset, l.ref))
		}
	}
}

// checkM2Standing raises the three M2 standing kinds (ADR 0101 9.1 (c), (c2),
// (d)), unwindowed, every run, one finding per (attempt, kind).
func (m *payMatcher) checkM2Standing(ctx context.Context, tx pgx.Tx) error {
	if len(m.k3.m2) == 0 {
		return nil
	}
	// Executed compensating_entry requests, read through K2's
	// tenant_system_read_executed policy: credits and debits by causation.
	type comp struct {
		direction string
		amount    *big.Int
		causation uuid.UUID
		ledgerTx  uuid.UUID
	}
	rows, err := tx.Query(ctx, `
		SELECT r.direction, r.amount::text, r.causation_transaction_id, r.ledger_transaction_id
		  FROM ledger_adjustment_requests r
		 WHERE r.tenant_id = $1 AND r.state = 'executed' AND r.reason_code = 'compensating_entry'
		   AND r.causation_transaction_id IS NOT NULL AND r.ledger_transaction_id IS NOT NULL
		 ORDER BY r.id`, m.tenantID)
	if err != nil {
		return fmt.Errorf("compensating entries: %w", err)
	}
	var comps []comp
	for rows.Next() {
		var c comp
		var amount string
		if err := rows.Scan(&c.direction, &amount, &c.causation, &c.ledgerTx); err != nil {
			rows.Close()
			return err
		}
		if c.amount, err = parseBig(amount); err != nil {
			rows.Close()
			return err
		}
		comps = append(comps, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	sumFor := func(direction string, causations map[uuid.UUID]bool) *big.Int {
		total := new(big.Int)
		for _, c := range comps {
			if c.direction == direction && causations[c.causation] {
				total.Add(total, c.amount)
			}
		}
		return total
	}

	for _, r := range m.k3.m2 {
		a := m.payAttemptByID(r.attemptID)
		if a == nil {
			continue
		}
		switch r.kind {
		case m2KindPaid:
			lines := m.k3.linesFor(a, paymentStatementKindPayout)
			confirmed := false
			var anySucceeded *persistedLine
			for _, l := range lines {
				if l.status != paymentStatementStatusSucceeded {
					continue
				}
				if anySucceeded == nil {
					anySucceeded = l
				}
				if m.resolvesTo(l, a) && confirmingLine(l, a) {
					confirmed = true
				}
			}
			// The credits compensating this Step B, and their recoveries.
			credited := sumFor("credit_player", map[uuid.UUID]bool{r.ledgerTx: true})
			creditTxs := map[uuid.UUID]bool{}
			for _, c := range comps {
				if c.direction == "credit_player" && c.causation == r.ledgerTx {
					creditTxs[c.ledgerTx] = true
				}
			}
			recovered := sumFor("debit_player", creditTxs)

			// (c) standing, clears only on a confirming line or a reversal of this
			// Step B. A compensating credit annotates, never clears.
			if !confirmed {
				var reversed bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ledger_transactions WHERE tenant_id = $1 AND reverses_transaction_id = $2)`,
					m.tenantID, r.ledgerTx).Scan(&reversed); err != nil {
					return fmt.Errorf("step B reversal probe: %w", err)
				}
				if !reversed {
					note := "no compensating credit"
					if credited.Sign() > 0 {
						note = fmt.Sprintf("compensating credits executed=%s recovered=%s (annotation only)", credited, recovered)
					}
					m.r.add(MismatchKindPayDeclaredPaidUnconfirmed, m.key("attempt="+a.id.String(), "resolution="+r.id.String(), "check=declared_paid_unconfirmed"),
						declaredPaidUnconfirmedResolutionHint,
						"platform: "+a.render()+"; M2 declare paid, step B transaction="+r.ledgerTx.String()+"; "+note)
				}
			}
			// (c2) a compensating credit AND a succeeded line from any import: the
			// player was restored and the PSP paid. Clears only on full recovery.
			if credited.Sign() > 0 && anySucceeded != nil && recovered.Cmp(credited) < 0 {
				m.r.add(MismatchKindPayDeclaredPaidCompensatedButPaid, m.key("attempt="+a.id.String(), "resolution="+r.id.String(), "check=declared_paid_compensated_but_paid"),
					declaredPaidCompensatedButPaidResolutionHint,
					fmt.Sprintf("platform: %s; compensated credits=%s recovered=%s; line: import=%s line_no=%d is_mock=%t status=%s amount=%s asset=%s",
						a.render(), credited, recovered, anySucceeded.importID, anySucceeded.lineNo, anySucceeded.isMock, anySucceeded.status, anySucceeded.amount, anySucceeded.asset))
			}
		case m2KindNotPaid:
			// (d) the attempt reached T14, or ANY import has a succeeded line for it
			// (the raising predicate: any amount, any asset, MOCK included).
			var evidence *persistedLine
			for _, l := range m.k3.linesFor(a, paymentStatementKindPayout) {
				if l.status == paymentStatementStatusSucceeded {
					evidence = l
					break
				}
			}
			t14 := a.state == "disputed" && a.terminalReason == disputeReasonSuccessAfterPayoutDeclined
			if !t14 && evidence == nil {
				continue
			}
			recovered := sumFor("debit_player", map[uuid.UUID]bool{r.ledgerTx: true})
			if recovered.Cmp(r.amount) >= 0 {
				continue
			}
			why := "T14 success_after_payout_declined"
			if evidence != nil {
				why = fmt.Sprintf("succeeded line import=%s line_no=%d is_mock=%t amount=%s asset=%s", evidence.importID, evidence.lineNo, evidence.isMock, evidence.amount, evidence.asset)
			}
			m.r.add(MismatchKindPayDeclaredNotPaidButPaid, m.key("attempt="+a.id.String(), "resolution="+r.id.String(), "check=declared_not_paid_but_paid"),
				declaredNotPaidButPaidResolutionHint,
				fmt.Sprintf("platform: %s; M2 declare not paid, withdrawal_failed transaction=%s; recovered=%s of %s; %s", a.render(), r.ledgerTx, recovered, r.amount, why))
		}
	}
	return nil
}

// countM2Confirmation is ADR 0101 9.1 (b): a matching succeeded payout line of
// THIS run for an M2-declared-paid attempt is a confirmation, counted by the
// metric; it is never a mismatch.
func (m *payMatcher) countM2Confirmation(ctx context.Context, a *payAttempt, l payLine) {
	if m.k3 == nil || !m.k3.m2Paid[a.id] || l.kind != paymentStatementKindPayout || l.status != paymentStatementStatusSucceeded {
		return
	}
	if l.asset != a.asset || l.amount.Cmp(a.amount) != 0 || !m.k3.clearEligible(m.k3.importIsMock) {
		return
	}
	if paymentM2DeclaredPaidConfirmedTotal != nil {
		paymentM2DeclaredPaidConfirmedTotal.Add(ctx, 1)
	}
}
