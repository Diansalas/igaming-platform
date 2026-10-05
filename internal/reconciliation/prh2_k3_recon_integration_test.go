//go:build integration

package reconciliation

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// PRH-2 K3 (ADR 0101 9.2, 9.3; C-34..C-36, C-38, C-43, C-44, C-14d, C-19+): the
// persisted statement-line lookup across imports (S1-S6) and the typed Y
// evidence, in the real WithTenantSnapshot shape, against fixtures written by
// the real sweeper/callback paths. MOCK evidence only.

func k3Cov(offsetMin int, lines ...statement.PaymentStatementLine) payFixedSource {
	return payFixedSource{provider: payProvA, stmt: statement.PaymentStatement{
		CoverageStart: time.Now().Add(-time.Hour - time.Duration(offsetMin)*time.Minute).UTC(),
		CoverageEnd:   time.Now().Add(time.Minute).UTC(), Lines: lines}}
}

// C-34 (S1 + S2): an unbound park with a merchant-resolved succeeded line in
// import 1 and NO line in imports 2..N is pay_captured_unposted on EVERY run
// (the persisted lookup, not the current import); a reversal in import k clears
// it from run k on, and it stays cleared when later imports carry nothing.
func TestK3_C34_S1S2_UnboundParkStandingThenClearedByAPersistedReversal(t *testing.T) {
	w := newD2World(t)
	pk, _ := w.parkInvalidRef(t)
	line := d2Line(pk.pspRef, pk.attempt.MerchantReference, statement.PaymentStatusSucceeded, d2Amount)

	d2CUFor(t, w.d2Run(t, d2Src(line)), pk.attempt.ID) // import 1
	for i := 0; i < 3; i++ {                           // imports 2..4: no line, coverage excludes the payout
		d2CUFor(t, w.d2Run(t, d2PastSrc()), pk.attempt.ID)
	}
	// A reversal naming the line's reference, in import k.
	d2NoCU(t, w.d2Run(t, k3Cov(7, d2ReversalLine("k3-rev-"+uuid.NewString(), pk.pspRef, d2Amount))), "a persisted reversal on the line's reference clears from run k")
	d2NoCU(t, w.d2Run(t, d2PastSrc()), "and it stays cleared in later runs")
	w.d2AssertNoMoney(t, pk)
}

// C-34 (unrelated reversal): a reversal naming a different reference does not clear.
func TestK3_C34_S2_UnrelatedReversalDoesNotClear(t *testing.T) {
	w := newD2World(t)
	pk, _ := w.parkInvalidRef(t)
	line := d2Line(pk.pspRef, pk.attempt.MerchantReference, statement.PaymentStatusSucceeded, d2Amount)
	d2CUFor(t, w.d2Run(t, d2Src(line)), pk.attempt.ID)
	ms := w.d2Run(t, k3Cov(3, d2ReversalLine("k3-rev-"+uuid.NewString(), "k3-unrelated-"+uuid.NewString(), d2Amount)))
	d2CUFor(t, ms, pk.attempt.ID)
	d2CUFor(t, w.d2Run(t, d2PastSrc()), pk.attempt.ID)
}

// C-35 (S3, TestD2_14 shape): a reversal line naming the bound reference in
// import 1 clears the bound standing finding in runs 2..N.
func TestK3_C35_S3_BoundParkClearedByAPersistedReversalInAnEarlierImport(t *testing.T) {
	w := newD2World(t)
	pk := w.parkSyncMismatch(t)
	// import 1: the matched succeeded line AND a reversal naming it.
	ms := w.d2Run(t, d2Src(
		d2Line(pk.pspRef, "", statement.PaymentStatusSucceeded, d2PSPAmount),
		d2ReversalLine("k3-rev-"+uuid.NewString(), pk.pspRef, d2PSPAmount)))
	if got := d2Kinds(ms)[d2KindCU]; got != 0 {
		t.Fatalf("a reversal in the same import must clear in-run, got %d CU:\n%s", got, renderMismatches(ms))
	}
	for i := 0; i < 3; i++ {
		d2NoCU(t, w.d2Run(t, d2PastSrc()), "a persisted reversal keeps the standing finding cleared")
	}
	// Control: a bound park with NO reversal anywhere keeps standing.
	pk2 := w.parkSyncMismatch(t)
	d2CUFor(t, w.d2Run(t, d2Src(d2Line(pk2.pspRef, "", statement.PaymentStatusSucceeded, d2PSPAmount))), pk2.attempt.ID)
	d2CUFor(t, w.d2Run(t, d2PastSrc()), pk2.attempt.ID)
}

// C-36 (S4): a poll_reference_mismatch park with Y evidence: a reversal on Y
// clears; on an unrelated reference it does not.
func TestK3_C36_S4_PollReferenceMismatchClearsOnTheReturnedReference(t *testing.T) {
	w := newD2World(t)
	a := w.deposit(t, d2Amount)
	x := *a.ProviderReference
	y := "k3-echo-" + uuid.NewString()
	w.p.setStatus(x, payments.StatusResult{ProviderReference: y, Outcome: payments.OutcomeSucceeded, Amount: d2Amount, AssetCode: "EUR"})
	w.d2PollOnce(t, a.ID)
	pk := d2Parked{attempt: w.mustParked(t, a.ID, payments.TerminalReasonPollReferenceMismatch, false), pspRef: x}

	var rows int
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_attempt_reference_evidence WHERE attempt_id = $1 AND reference = $2`, a.ID, y).Scan(&rows)
	}); err != nil || rows != 1 {
		t.Fatalf("setup: want one typed Y row, got %d (%v)", rows, err)
	}

	d2CUFor(t, w.d2Run(t, d2Src(d2Line(x, "", statement.PaymentStatusSucceeded, d2Amount))), a.ID)
	d2CUFor(t, w.d2Run(t, k3Cov(3, d2ReversalLine("k3-rev-"+uuid.NewString(), "k3-unrelated-"+uuid.NewString(), d2Amount))), a.ID)
	d2NoCU(t, w.d2Run(t, k3Cov(5, d2ReversalLine("k3-rev-"+uuid.NewString(), y, d2Amount))), "a reversal on Y clears")
	d2NoCU(t, w.d2Run(t, d2PastSrc()), "and stays cleared")
	w.d2AssertNoMoney(t, pk)
}

// C-36 (no Y row; mutant: read echoed_provider_reference from audit JSON): a
// poll_reference_mismatch park WITHOUT a typed Y row is cleared only on X, even
// when an audit row carries a Y.
func TestK3_C36_S4_YComesOnlyFromTheTypedTable_NeverFromAuditJSON(t *testing.T) {
	w := newD2World(t)
	pk := w.parkPoll(t, payments.TerminalReasonPollReferenceMismatch) // fixture park: no Y row
	y := "k3-audit-only-" + uuid.NewString()
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, audit.Entry{TenantID: w.f.tenantID, ActorType: audit.ActorSystem, Action: "payment.attempt_disputed",
			TargetType: "payment_attempt", TargetID: pk.attempt.ID.String(), Outcome: audit.OutcomeSuccess,
			Metadata: map[string]any{"terminal_reason": "poll_reference_mismatch", "echoed_provider_reference": y}})
	}); err != nil {
		t.Fatalf("plant the audit-only Y: %v", err)
	}
	d2CUFor(t, w.d2Run(t, d2Src(d2Line(pk.pspRef, "", statement.PaymentStatusSucceeded, d2Amount))), pk.attempt.ID)
	d2CUFor(t, w.d2Run(t, k3Cov(3, d2ReversalLine("k3-rev-"+uuid.NewString(), y, d2Amount))), pk.attempt.ID)
	d2NoCU(t, w.d2Run(t, k3Cov(5, d2ReversalLine("k3-rev-"+uuid.NewString(), pk.pspRef, d2Amount))), "only X clears without a typed Y")
}

// C-38 (T15i): success_for_never_sent_attempt is bound-if-referenced (S5): an
// attempt never sent, resolved by a merchant-reference callback, is reported
// from the persisted line and cleared by a reversal on the line's reference.
func TestK3_C38_T15i_NeverSentSuccessIsBoundIfReferenced(t *testing.T) {
	w := newD2World(t)
	w.p.setScript(func(req payments.DepositRequest) payments.DepositResult {
		return payments.DepositResult{Outcome: payments.OutcomeDeclined, DeclineReason: "do_not_honor", Amount: req.Amount, AssetCode: req.AssetCode}
	})
	first := w.deposit(t, d2Amount)
	w.p.setScript(nil)
	var created payments.PaymentAttempt
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		created, err = payments.InsertCreatedAttempt(ctx, tx, payments.NewCreatedAttempt{
			ID: uuid.New(), TenantID: w.f.tenantID, Operation: payments.AttemptOperationDeposit, DepositIntentID: first.DepositIntentID,
			AttemptNo: 2, ExcludedProviderIDs: []string{}, PaymentMethod: "card", AssetCode: "EUR", Amount: d2Amount,
		})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE payment_attempts SET provider_id = $2 WHERE id = $1`, created.ID, payProvA)
		return err
	}); err != nil {
		t.Fatalf("setup: never-sent attempt: %v", err)
	}
	ref := "k3-ns-" + uuid.NewString()
	w.applyReceipt(t, payProvA, payments.ReceiptEvidence{EventType: "deposit", ProviderReference: ref, MerchantReference: created.MerchantReference,
		Outcome: payments.OutcomeSucceeded, Amount: d2Amount, AssetCode: "EUR"})
	got := w.mustParked(t, created.ID, payments.TerminalReasonSuccessForNeverSentAttempt, false)
	pk := d2Parked{attempt: got, pspRef: ref}

	d2CUFor(t, w.d2Run(t, d2Src(d2Line(ref, created.MerchantReference, statement.PaymentStatusSucceeded, d2Amount))), created.ID)
	d2CUFor(t, w.d2Run(t, d2PastSrc()), created.ID)
	d2NoCU(t, w.d2Run(t, k3Cov(4, d2ReversalLine("k3-rev-"+uuid.NewString(), ref, d2Amount))), "a reversal on the line's reference clears it")
	w.d2AssertNoMoney(t, pk)
}

// C-43 / C-44: one finding per exposure per run, and a line repeated in
// overlapping imports is deduplicated and never gives pay_duplicate.
func TestK3_C43_C44_OneFindingPerExposure_OverlappingImportsDeduplicate(t *testing.T) {
	w := newD2World(t)
	pk, _ := w.parkInvalidRef(t)
	line := d2Line(pk.pspRef, pk.attempt.MerchantReference, statement.PaymentStatusSucceeded, d2Amount)
	// The SAME line (same occurred_at) in three overlapping imports.
	for i := 0; i < 3; i++ {
		ms := w.d2Run(t, k3Cov(i*2, line))
		d2CUFor(t, ms, pk.attempt.ID)
		if n := d2Kinds(ms)[MismatchKindPayDuplicate]; n != 0 {
			t.Fatalf("overlapping imports must not give pay_duplicate (import %d):\n%s", i+1, renderMismatches(ms))
		}
	}
	// Two DIFFERENT lines both resolving to the one parked attempt are two exposures
	// (one finding per attempt and evidencing reference), never more than one each.
	line2 := d2Line("k3-second-"+uuid.NewString(), pk.attempt.MerchantReference, statement.PaymentStatusSucceeded, d2Amount)
	ms := w.d2Run(t, k3Cov(9, line, line2))
	count := 0
	for _, m := range ms {
		if m.MismatchKind == d2KindCU && strings.Contains(m.ReconciliationKey, "attempt="+pk.attempt.ID.String()) {
			count++
		}
	}
	if count < 1 || count > 2 {
		t.Fatalf("want one finding per (attempt, reference), got %d:\n%s", count, renderMismatches(ms))
	}
	seen := map[string]bool{}
	for _, m := range ms {
		if seen[m.ReconciliationKey] {
			t.Fatalf("duplicate reconciliation key %q in one run:\n%s", m.ReconciliationKey, renderMismatches(ms))
		}
		seen[m.ReconciliationKey] = true
	}
}

// C-14d: two tenants with the same provider and the same merchant and provider
// references never see each other's lines or evidence in the persisted lookup.
func TestK3_C14d_PersistedLookupNeverCrossesTenants(t *testing.T) {
	w := newD2World(t)
	pk, _ := w.parkInvalidRef(t)

	// A second tenant, same provider, imports a succeeded line carrying the first
	// tenant's merchant and provider references, plus a reversal naming that ref.
	f2 := seedFixture(t, w.pool)
	w2 := &payWorld{pool: w.pool, f: f2, mockA: w.mockA, orch: w.orch, srcA: w.srcA}
	w2.registerCapability(t, w.p, 10)
	l := d2Line(pk.pspRef, pk.attempt.MerchantReference, statement.PaymentStatusSucceeded, d2Amount)
	rev := d2ReversalLine("k3-rev-"+uuid.NewString(), pk.pspRef, d2Amount)
	src := d2Src(l, rev)
	if stmt, err := FetchPaymentStatement(context.Background(), src, f2.tenantID, time.Now().Add(-time.Hour), time.Now(), PaymentStatementOptions{}); err == nil {
		_ = w.pool.WithTenant(context.Background(), f2.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, _, err := IngestPaymentStatement(ctx, tx, f2.tenantID, src, stmt, time.Now())
			return err
		})
	} else {
		t.Fatalf("fetch for tenant 2: %v", err)
	}

	// Tenant 1's finding is untouched: it has no evidence of its own yet, so with
	// a line-less import it must NOT report (no foreign line) and, once its own
	// line arrives, a foreign reversal must NOT clear it.
	d2NoCU(t, w.d2Run(t, d2PastSrc()), "tenant 2's lines are invisible to tenant 1")
	d2CUFor(t, w.d2Run(t, k3Cov(2, d2Line(pk.pspRef, pk.attempt.MerchantReference, statement.PaymentStatusSucceeded, d2Amount))), pk.attempt.ID)
	d2CUFor(t, w.d2Run(t, d2PastSrc()), pk.attempt.ID) // tenant 2's reversal never clears tenant 1's finding
}

// C-19+ (LF): the detail text STORED by real runs (not a source grep) never says
// M1 clears, and names allocation / acknowledgement.
func TestK3_C19Plus_StoredDetailTextNeverSaysM1Clears(t *testing.T) {
	w := newD2World(t)
	pk, _ := w.parkInvalidRef(t)
	d2CUFor(t, w.d2Run(t, d2Src(d2Line(pk.pspRef, pk.attempt.MerchantReference, statement.PaymentStatusSucceeded, d2Amount))), pk.attempt.ID)
	pkB := w.parkSyncMismatch(t)
	d2CUFor(t, w.d2Run(t, d2Src(d2Line(pkB.pspRef, "", statement.PaymentStatusSucceeded, d2PSPAmount))), pkB.attempt.ID)

	var details []string
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT expected_value || ' | ' || actual_value FROM reconciliation_mismatches WHERE tenant_id = $1`, w.f.tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			details = append(details, s)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read stored mismatch details: %v", err)
	}
	if len(details) < 2 {
		t.Fatalf("setup: only %d stored mismatch rows", len(details))
	}
	for _, d := range details {
		if strings.Contains(d, "M1/allocation") || strings.Contains(strings.ToLower(d), "m1 clears") || strings.Contains(strings.ToLower(d), "cleared by m1") {
			t.Errorf("a stored detail says M1 clears: %q", d)
		}
		if strings.Contains(d, "pay_captured_unposted") || strings.Contains(d, "captured_unposted") {
			if !strings.Contains(d, "M1 only acknowledges") {
				t.Errorf("a stored captured-unposted detail lacks the acknowledgement wording: %q", d)
			}
		}
	}
}
