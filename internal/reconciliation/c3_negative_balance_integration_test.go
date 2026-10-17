//go:build integration

// C3 (closure ledger-finance review) / F4-R2-NEGBAL-SQL detection (negative_balance.go, migration 0174): the standing,
// read-only ledger_vs_projection findings player_wallet_negative_balance and payout_dispatched_while_wallet_negative.
// The sweeps run on the RUNTIME role (igaming_runtime), as the production scheduler does; the owner pool only seeds.
// Every negative state is driven the way production reaches it: a deposit_reversal (chargeback) posted against funds
// already spent or held (internal/testsupport/negbal). The fixture wallet starts with a 1000 deposit, so every hold
// planted here is FUNDED (the only hold shape withdrawal.RequestWithdrawal can produce).
package reconciliation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
	"github.com/Diansalas/igaming-platform/internal/testsupport/negbal"
	"github.com/Diansalas/igaming-platform/internal/testsupport/wdpath"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

type c3Row struct {
	kind, key, actual string
	run               uuid.UUID
}

// c3Mismatches returns the tenant's C3 rows (both kinds) in creation order.
func c3Mismatches(t *testing.T, pool *db.Pool, tenantID uuid.UUID) []c3Row {
	t.Helper()
	var out []c3Row
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT mismatch_kind, reconciliation_key, actual_value, reconciliation_run_id
			FROM reconciliation_mismatches
			WHERE tenant_id = $1 AND mismatch_kind IN ('player_wallet_negative_balance', 'payout_dispatched_while_wallet_negative')
			ORDER BY created_at, mismatch_kind, reconciliation_key`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r c3Row
			if err := rows.Scan(&r.kind, &r.key, &r.actual, &r.run); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func c3OfRun(rows []c3Row, run uuid.UUID) []c3Row {
	var out []c3Row
	for _, r := range rows {
		if r.run == run {
			out = append(out, r)
		}
	}
	return out
}

func c3Alert(rows []alertinject.Row, disc string) []alertinject.Row {
	var out []alertinject.Row
	for _, r := range rows {
		if r.Discriminator == disc {
			out = append(out, r)
		}
	}
	return out
}

// c3Run runs ONE ledger_vs_projection stream for the tenant on the runtime role, exactly as the sweep does.
func c3Run(t *testing.T, rt *db.Pool, tenantID uuid.UUID) StreamOutcome {
	t.Helper()
	o := runLedgerStreamForTenant(context.Background(), rt, nil, tenantID, time.Now().Add(-time.Hour), time.Now(), "")
	if o.Err != nil || o.Skipped {
		t.Fatalf("ledger_vs_projection run: err=%v skipped=%v", o.Err, o.Skipped)
	}
	return o
}

func c3Chargeback(t *testing.T, pool *db.Pool, f fixture, amount int64) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = negbal.Chargeback(ctx, tx, f.tenantID, f.walletID, "EUR", amount)
		return err
	}); err != nil {
		t.Fatalf("chargeback: %v", err)
	}
	return id
}

func c3Deposit(t *testing.T, pool *db.Pool, f fixture, amount int64) {
	t.Helper()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return negbal.Deposit(ctx, tx, f.tenantID, f.walletID, "EUR", amount)
	}); err != nil {
		t.Fatalf("deposit: %v", err)
	}
}

// c3LedgerFootprint is every ledger / projection / withdrawal / attempt figure of the tenant: the check must leave it
// unchanged (read-only detective control).
func c3LedgerFootprint(t *testing.T, pool *db.Pool, tenantID uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT format('tx=%s entries=%s sum=%s proj=%s wr=%s pa=%s',
			(SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1),
			(SELECT count(*) FROM ledger_entries WHERE tenant_id = $1),
			(SELECT COALESCE(sum(amount), 0) FROM ledger_entries WHERE tenant_id = $1),
			(SELECT string_agg(ledger_account_id::text || ':' || debit_total::text || ':' || credit_total::text, ',' ORDER BY ledger_account_id)
			   FROM wallet_balance_projection WHERE tenant_id = $1),
			(SELECT string_agg(id::text || ':' || state || ':' || updated_at::text, ',' ORDER BY id) FROM withdrawal_requests WHERE tenant_id = $1),
			(SELECT string_agg(id::text || ':' || state || ':' || updated_at::text, ',' ORDER BY id) FROM payment_attempts WHERE tenant_id = $1))`,
			tenantID).Scan(&s)
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

// (a) A chargeback after the player spent the deposit: exactly ONE standing finding (keyed by the player_cash
// account), ONE open P1 under its own discriminator; repeat sweeps add one row per run (the run's evidence) and one
// occurrence on the SAME alert, never a second alert; the ledger is untouched.
func TestC3_ChargebackNegative_ExactlyOneStandingFinding_RepeatsIdempotent(t *testing.T) {
	pool := testPool(t)
	rt := wdpath.RuntimeFor(t, pool)
	f := seedFixture(t, pool)
	c3Chargeback(t, pool, f, 1_300) // 1000 deposited, 1300 charged back: -300

	before := c3LedgerFootprint(t, pool, f.tenantID)
	o := c3Run(t, rt, f.tenantID)
	if o.Run.Status != StatusMismatchesFound {
		t.Fatalf("a negative wallet must be a finding: %s", o.Run.Status)
	}
	rows := c3OfRun(c3Mismatches(t, pool, f.tenantID), o.Run.ID)
	if len(rows) != 1 || rows[0].kind != string(MismatchKindPlayerWalletNegativeBalance) || rows[0].key != f.cashAccountID.String() ||
		!strings.Contains(rows[0].actual, "balance=-300 ") || !strings.Contains(rows[0].actual, "account_type=player_cash") {
		t.Fatalf("want exactly one player_wallet_negative_balance finding on the cash account (-300): %+v", rows)
	}
	if o.Mismatches != 1 {
		t.Fatalf("no other finding expected (no drift, no check D): %d", o.Mismatches)
	}
	al := alertinject.ForSubject(t, pool, f.tenantID)
	neg := c3Alert(al, discLedgerWalletNegative)
	if len(al) != 1 || len(neg) != 1 || neg[0].Kind != string(alerting.KindReconciliationLedgerProjectionDrift) ||
		neg[0].Severity != "p1" || neg[0].Occurrences != 1 {
		t.Fatalf("want ONE P1 under %s: %+v", discLedgerWalletNegative, al)
	}

	for i := 0; i < 2; i++ {
		c3Run(t, rt, f.tenantID)
	}
	all := c3Mismatches(t, pool, f.tenantID)
	keys := map[string]int{}
	for _, r := range all {
		keys[r.kind+"/"+r.key]++
	}
	if len(all) != 3 || len(keys) != 1 {
		t.Fatalf("three runs: one row per run, all the same finding: %+v", all)
	}
	al = alertinject.ForSubject(t, pool, f.tenantID)
	if neg = c3Alert(al, discLedgerWalletNegative); len(al) != 1 || len(neg) != 1 || neg[0].Occurrences != 3 || neg[0].State == "resolved" {
		t.Fatalf("a persisting negative wallet is ONE open alert with growing occurrences: %+v", al)
	}
	if after := c3LedgerFootprint(t, pool, f.tenantID); after != before {
		t.Fatalf("the detector must not write the ledger, projection, requests or attempts:\n%s\n%s", before, after)
	}
}

// A funded hold is the legitimate hold state: player_cash goes to exactly zero (never below; RequestWithdrawal refuses
// an unfunded hold under the L3 lock) and the request moves through every state. No finding, no alert.
func TestC3_FundedHold_NoFinding(t *testing.T) {
	pool := testPool(t)
	rt := wdpath.RuntimeFor(t, pool)
	f := seedFixture(t, pool)
	spec := wdpath.Spec{TenantID: f.tenantID}
	full := dWithdrawal(t, pool, f, 600, withdrawal.StateApproved) // cash 400
	dWithdrawal(t, pool, f, 400, withdrawal.StateRequested)        // cash 0, two open holds
	if o := c3Run(t, rt, f.tenantID); o.Run.Status != StatusClean {
		t.Fatalf("player_cash at zero with funded holds is clean: %s %+v", o.Run.Status, c3Mismatches(t, pool, f.tenantID))
	}
	wdpath.AdvanceAsRuntime(context.Background(), t, pool, f.tenantID, full, withdrawal.StateSubmitted, spec)
	if o := c3Run(t, rt, f.tenantID); o.Run.Status != StatusClean {
		t.Fatalf("a dispatched funded payout is clean: %s %+v", o.Run.Status, c3Mismatches(t, pool, f.tenantID))
	}
	wdpath.AdvanceAsRuntime(context.Background(), t, pool, f.tenantID, full, withdrawal.StateCompleted, spec)
	if o := c3Run(t, rt, f.tenantID); o.Run.Status != StatusClean {
		t.Fatalf("a completed funded payout is clean: %s %+v", o.Run.Status, c3Mismatches(t, pool, f.tenantID))
	}
	if rows := c3Mismatches(t, pool, f.tenantID); len(rows) != 0 {
		t.Fatalf("no C3 finding: %+v", rows)
	}
	if al := alertinject.ForSubject(t, pool, f.tenantID); len(al) != 0 {
		t.Fatalf("no alert: %+v", al)
	}
}

// The predicate is the gate's: an open hold does NOT net a chargeback away. Deposit 1000, hold 1000 (cash 0), chargeback
// 1000: cash -1000 while the hold still carries 1000. Netting would report 0 and hide the exact case C3 exists for.
// A funded hold that the chargeback overtakes is reported; an UNFUNDED hold (a state only a test fixture can build) is
// reported too, exactly as the WD050 gate refuses it.
func TestC3_OpenHoldDoesNotNetAChargebackAway(t *testing.T) {
	pool := testPool(t)
	rt := wdpath.RuntimeFor(t, pool)
	f := seedFixture(t, pool)
	dWithdrawal(t, pool, f, 1_000, withdrawal.StatePendingReview)
	c3Chargeback(t, pool, f, 1_000)
	o := c3Run(t, rt, f.tenantID)
	rows := c3OfRun(c3Mismatches(t, pool, f.tenantID), o.Run.ID)
	if len(rows) != 1 || rows[0].kind != string(MismatchKindPlayerWalletNegativeBalance) || !strings.Contains(rows[0].actual, "balance=-1000 ") {
		t.Fatalf("cash -1000 with a 1000 hold open is a finding: %+v", rows)
	}

	g := seedFixture(t, pool)
	dWithdrawal(t, pool, g, 1_500, withdrawal.StateRequested) // 1000 deposited: an unfunded 500 (fixture-only shape)
	o = c3Run(t, rt, g.tenantID)
	if rows := c3OfRun(c3Mismatches(t, pool, g.tenantID), o.Run.ID); len(rows) != 1 || !strings.Contains(rows[0].actual, "balance=-500 ") {
		t.Fatalf("an unfunded hold is negative to the gate and to the detector alike: %+v", rows)
	}
}

// (b) The R1 window: the chargeback commits while the request is approved and the payout is then dispatched (the
// fixture records an ALLOW, standing in for a gate read that did not see the chargeback). Both findings, each under
// its own discriminator; the payout finding is keyed by the request and persists after completion.
func TestC3_PayoutDispatchedAfterNegativeMakingPosting_Finding(t *testing.T) {
	pool := testPool(t)
	rt := wdpath.RuntimeFor(t, pool)
	f := seedFixture(t, pool)
	spec := wdpath.Spec{TenantID: f.tenantID}
	id := dWithdrawal(t, pool, f, 1_000, withdrawal.StateApproved) // cash 0, hold 1000
	c3Chargeback(t, pool, f, 1_000)                                // cash -1000
	wdpath.AdvanceAsRuntime(context.Background(), t, pool, f.tenantID, id, withdrawal.StateSubmitted, spec)

	o := c3Run(t, rt, f.tenantID)
	rows := c3OfRun(c3Mismatches(t, pool, f.tenantID), o.Run.ID)
	if len(rows) != 2 || rows[0].kind != string(MismatchKindPayoutDispatchedWhileWalletNegative) || rows[0].key != id.String() ||
		!strings.Contains(rows[0].actual, "state=submitted") || !strings.Contains(rows[0].actual, "balance at dispatch=-1000") ||
		rows[1].kind != string(MismatchKindPlayerWalletNegativeBalance) {
		t.Fatalf("want the payout finding (keyed by the request) and the wallet finding: %+v", rows)
	}
	al := alertinject.ForSubject(t, pool, f.tenantID)
	if len(al) != 2 || len(c3Alert(al, discLedgerPayoutWhileNegative)) != 1 || len(c3Alert(al, discLedgerWalletNegative)) != 1 {
		t.Fatalf("two P1s, one per discriminator: %+v", al)
	}

	wdpath.AdvanceAsRuntime(context.Background(), t, pool, f.tenantID, id, withdrawal.StateCompleted, spec)
	o = c3Run(t, rt, f.tenantID)
	rows = c3OfRun(c3Mismatches(t, pool, f.tenantID), o.Run.ID)
	if len(rows) != 2 || rows[0].key != id.String() || !strings.Contains(rows[0].actual, "state=completed") {
		t.Fatalf("the payout finding persists once the payout completed: %+v", rows)
	}
}

// The converse: a payout dispatched while the wallet was funded, and charged back afterwards, is NOT a payout
// finding (the gate could not have refused it); the wallet itself is still reported.
func TestC3_PayoutDispatchedBeforeTheChargeback_OnlyTheWalletFinding(t *testing.T) {
	pool := testPool(t)
	rt := wdpath.RuntimeFor(t, pool)
	f := seedFixture(t, pool)
	spec := wdpath.Spec{TenantID: f.tenantID}
	id := dWithdrawal(t, pool, f, 1_000, withdrawal.StateSubmitted)
	c3Chargeback(t, pool, f, 1_000)
	o := c3Run(t, rt, f.tenantID)
	rows := c3OfRun(c3Mismatches(t, pool, f.tenantID), o.Run.ID)
	if len(rows) != 1 || rows[0].kind != string(MismatchKindPlayerWalletNegativeBalance) {
		t.Fatalf("only the wallet finding: %+v", rows)
	}
	wdpath.AdvanceAsRuntime(context.Background(), t, pool, f.tenantID, id, withdrawal.StateCompleted, spec)
	o = c3Run(t, rt, f.tenantID)
	if rows := c3OfRun(c3Mismatches(t, pool, f.tenantID), o.Run.ID); len(rows) != 1 || rows[0].kind != string(MismatchKindPlayerWalletNegativeBalance) {
		t.Fatalf("still only the wallet finding after completion: %+v", rows)
	}
	if len(c3Alert(alertinject.ForSubject(t, pool, f.tenantID), discLedgerPayoutWhileNegative)) != 0 {
		t.Fatal("no payout alert")
	}
}

// Recovery: the receivable is collected through a compensating credit (a deposit here; a governed K2 credit in
// production). The next run records no C3 row and is clean; the open alert gets no new occurrence (staff resolve it).
func TestC3_RecoveryClears(t *testing.T) {
	pool := testPool(t)
	rt := wdpath.RuntimeFor(t, pool)
	f := seedFixture(t, pool)
	spec := wdpath.Spec{TenantID: f.tenantID}
	id := dWithdrawal(t, pool, f, 1_000, withdrawal.StateApproved)
	c3Chargeback(t, pool, f, 1_000)
	wdpath.AdvanceAsRuntime(context.Background(), t, pool, f.tenantID, id, withdrawal.StateSubmitted, spec)
	if o := c3Run(t, rt, f.tenantID); len(c3OfRun(c3Mismatches(t, pool, f.tenantID), o.Run.ID)) != 2 {
		t.Fatal("precondition: both findings")
	}
	c3Deposit(t, pool, f, 999) // still -1: both findings stand
	if o := c3Run(t, rt, f.tenantID); len(c3OfRun(c3Mismatches(t, pool, f.tenantID), o.Run.ID)) != 2 {
		t.Fatal("a partial recovery clears nothing")
	}
	occ := map[string]int{}
	for _, a := range alertinject.ForSubject(t, pool, f.tenantID) {
		occ[a.Discriminator] = a.Occurrences
	}
	c3Deposit(t, pool, f, 1) // exactly 0
	o := c3Run(t, rt, f.tenantID)
	if o.Run.Status != StatusClean || len(c3OfRun(c3Mismatches(t, pool, f.tenantID), o.Run.ID)) != 0 {
		t.Fatalf("a recovered wallet is clean: %s", o.Run.Status)
	}
	for _, a := range alertinject.ForSubject(t, pool, f.tenantID) {
		if a.Occurrences != occ[a.Discriminator] {
			t.Fatalf("a clean run adds no occurrence: %+v (was %v)", a, occ)
		}
	}
}

// Tenant isolation: one tenant's negative wallet never appears in another tenant's run, and the check fails closed
// (no run recorded) when the session is not exactly the run tenant's scope.
func TestC3_TenantIsolation_AndFailClosedOnForeignScope(t *testing.T) {
	pool := testPool(t)
	rt := wdpath.RuntimeFor(t, pool)
	a := seedFixture(t, pool)
	b := seedFixture(t, pool)
	c3Chargeback(t, pool, b, 1_001)
	if o := c3Run(t, rt, a.tenantID); o.Run.Status != StatusClean {
		t.Fatalf("tenant A is clean: %s", o.Run.Status)
	}
	if rows := c3Mismatches(t, pool, a.tenantID); len(rows) != 0 {
		t.Fatalf("B's wallet leaked into A: %+v", rows)
	}
	if len(alertinject.ForSubject(t, pool, a.tenantID)) != 0 {
		t.Fatal("no alert for A")
	}
	if o := c3Run(t, rt, b.tenantID); len(c3OfRun(c3Mismatches(t, pool, b.tenantID), o.Run.ID)) != 1 {
		t.Fatal("B's run reports B's wallet")
	}
	// A's tenant id read inside B's scope: blind (RLS hides A's rows), so fail closed, never a "clean" A.
	err := rt.WithTenant(context.Background(), b.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := negativeBalanceFindings(ctx, tx, a.tenantID)
		return err
	})
	if !errors.Is(err, ErrNegativeBalanceCheckBlind) {
		t.Fatalf("a foreign-scope read must fail closed, got %v", err)
	}
	// And B's own findings are invisible from A's scope.
	if err := rt.WithTenant(context.Background(), a.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_mismatches WHERE reconciliation_key = $1`, b.cashAccountID.String()).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("B's finding visible from A: %d", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Concurrency (1): sweeps racing on the SAME negative tenant. Each either records a run with exactly the one finding
// or skips on the tenant's advisory lock; the alert stays ONE open row whose occurrences equal the recorded runs.
func TestC3_ConcurrentSweeps_OneAlertNoDuplicates(t *testing.T) {
	pool := testPool(t)
	rt := wdpath.RuntimeFor(t, pool)
	f := seedFixture(t, pool)
	c3Chargeback(t, pool, f, 1_200)
	var wg sync.WaitGroup
	var recorded atomic.Int64
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o := runLedgerStreamForTenant(context.Background(), rt, nil, f.tenantID, time.Now().Add(-time.Hour), time.Now(), "")
			if o.Err != nil {
				errs <- o.Err
				return
			}
			if !o.Skipped {
				recorded.Add(1)
				if o.Mismatches != 1 {
					errs <- fmt.Errorf("run %s: %d findings", o.Run.ID, o.Mismatches)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if recorded.Load() < 1 || int64(len(c3Mismatches(t, pool, f.tenantID))) != recorded.Load() {
		t.Fatalf("one row per recorded run: runs=%d rows=%d", recorded.Load(), len(c3Mismatches(t, pool, f.tenantID)))
	}
	al := alertinject.ForSubject(t, pool, f.tenantID)
	if neg := c3Alert(al, discLedgerWalletNegative); len(al) != 1 || len(neg) != 1 || int64(neg[0].Occurrences) != recorded.Load() {
		t.Fatalf("ONE alert, one occurrence per recorded run (%d): %+v", recorded.Load(), al)
	}
}

// Concurrency (2): live money movement that never takes the wallet below zero (each writer deposits before it charges
// back or holds the same amount) races the sweeps. One statement, one snapshot: no run may report a false finding.
func TestC3_ConcurrentLivePostings_NoFalseFinding(t *testing.T) {
	pool := testPool(t)
	rt := wdpath.RuntimeFor(t, pool)
	f := seedFixture(t, pool)
	c3Chargeback(t, pool, f, 1_000) // start from exactly zero: any interleaving that went negative would be reported
	ctx := context.Background()
	// A real verified binding, so every writer runs as igaming_runtime with no owner DDL (as check D's race test).
	inst := pitest.Bind(t, pool, f.tenantID, f.playerAccountID, "EUR")
	var fp string
	if err := pool.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT fingerprint FROM payout_instruments WHERE id = $1`, inst).Scan(&fp)
	}); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var made atomic.Int64
	errs := make(chan error, 8)
	for g := 0; g < 3; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				amount := int64(1 + i%5)
				if err := rt.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
					return negbal.Deposit(ctx, tx, f.tenantID, f.walletID, "EUR", amount)
				}); err != nil {
					errs <- err
					return
				}
				var err error
				if (g+i)%2 == 0 {
					err = rt.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
						_, err := negbal.Chargeback(ctx, tx, f.tenantID, f.walletID, "EUR", amount)
						return err
					})
				} else {
					spec := wdpath.Spec{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
						AssetCode: "EUR", Amount: amount, IdempotencyKey: fmt.Sprintf("c3conc-%d-%d-%s", g, i, uuid.NewString()),
						InstrumentID: &inst, Fingerprint: &fp}
					err = rt.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
						_, err := wdpath.Plant(ctx, tx, spec)
						return err
					})
				}
				if err != nil {
					errs <- err
					return
				}
				made.Add(1)
			}
		}()
	}
	runs := 0
	deadline := time.Now().Add(4 * time.Second)
	var failure string
	for time.Now().Before(deadline) && failure == "" {
		o := runLedgerStreamForTenant(ctx, rt, nil, f.tenantID, time.Now().Add(-time.Hour), time.Now(), "")
		switch {
		case o.Err != nil:
			failure = fmt.Sprintf("run %d failed: %v", runs, o.Err)
		case !o.Skipped && o.Run.Status != StatusClean:
			failure = fmt.Sprintf("FALSE finding during live postings (run %d): %+v", runs, c3Mismatches(t, pool, f.tenantID))
		case !o.Skipped:
			runs++
		}
	}
	close(stop)
	wg.Wait()
	close(errs)
	if failure != "" {
		t.Fatal(failure)
	}
	for err := range errs {
		t.Fatalf("writer: %v", err)
	}
	if runs < 5 || made.Load() < 20 {
		t.Fatalf("the race must be exercised: %d runs, %d postings", runs, made.Load())
	}
}

// withoutUnfundedHoldFixtureNegatives drops C3 findings (either kind) ONLY for a player account whose every debit is a
// withdrawal_requested hold leg: such an account can be negative only because a hold was planted UNFUNDED, a state
// only the test fixture wdpath.Plant can build (withdrawal.RequestWithdrawal checks funds under the L3 lock). The
// payout suites using it pre-date C3, plant payouts larger than the fixture's 1000 deposit and assert "no OTHER
// finding" (the check-D precedent: withoutPlantedUnlinkedHoldLegs). A C3 finding on an account with ANY other debit
// (a chargeback, a wager, a completion) is kept, and every other kind is kept, so the callers' len(drift) == 0
// assertion still proves every dropped finding was a fixture artifact (TestC3_FixtureFilter_KeepsRealNegatives).
func withoutUnfundedHoldFixtureNegatives(ctx context.Context, tx pgx.Tx, ms []Mismatch) ([]Mismatch, error) {
	accountOf := func(m Mismatch) (uuid.UUID, bool) {
		switch m.MismatchKind {
		case MismatchKindPlayerWalletNegativeBalance:
			id, err := uuid.Parse(m.ReconciliationKey)
			return id, err == nil
		case MismatchKindPayoutDispatchedWhileWalletNegative:
			i := strings.Index(m.ActualValue, "(account=")
			if i < 0 || len(m.ActualValue) < i+len("(account=")+36 {
				return uuid.Nil, false
			}
			id, err := uuid.Parse(m.ActualValue[i+len("(account=") : i+len("(account=")+36])
			return id, err == nil
		}
		return uuid.Nil, false
	}
	var ids []uuid.UUID
	for _, m := range ms {
		if id, ok := accountOf(m); ok {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return ms, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT a.id FROM unnest($1::uuid[]) a(id)
		 WHERE NOT EXISTS (SELECT 1 FROM ledger_entries e JOIN ledger_transactions t ON t.id = e.ledger_transaction_id
		                    WHERE e.ledger_account_id = a.id AND e.direction = 'debit' AND t.transaction_type <> 'withdrawal_requested')`, ids)
	if err != nil {
		return nil, err
	}
	fixtureOnly := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		fixtureOnly[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []Mismatch
	for _, m := range ms {
		if id, ok := accountOf(m); ok && fixtureOnly[id] {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// The fixture filter is exact: it drops the findings of an unfunded-hold-only account and keeps a chargeback-driven
// negative (wallet and payout finding) and every other kind.
func TestC3_FixtureFilter_KeepsRealNegatives(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	spec := wdpath.Spec{TenantID: f.tenantID}
	id := dWithdrawal(t, pool, f, 1_000, withdrawal.StateApproved)
	c3Chargeback(t, pool, f, 1_000)
	wdpath.AdvanceAsRuntime(context.Background(), t, pool, f.tenantID, id, withdrawal.StateSubmitted, spec)
	g := seedFixture(t, pool)
	gid := dWithdrawal(t, pool, g, 1_500, withdrawal.StateSubmitted) // unfunded 500: fixture-only
	for _, c := range []struct {
		fx   fixture
		want int
		name string
	}{{f, 2, "chargeback: both kept"}, {g, 0, "unfunded fixture hold: both dropped"}} {
		if err := pool.WithTenant(context.Background(), c.fx.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, ms, err := RunLedgerVsProjection(ctx, tx, c.fx.tenantID, time.Now().Add(-time.Hour), time.Now())
			if err != nil {
				return err
			}
			if c.fx.tenantID == g.tenantID && (len(ms) != 2 || ms[0].ReconciliationKey != gid.String()) {
				return fmt.Errorf("precondition: the unfunded fixture yields both findings: %+v", ms)
			}
			ms = append(ms, Mismatch{MismatchKind: MismatchKindBalanceMismatch, ReconciliationKey: c.fx.cashAccountID.String()})
			kept, err := withoutUnfundedHoldFixtureNegatives(ctx, tx, ms)
			if err != nil {
				return err
			}
			if len(kept) != c.want+1 || kept[len(kept)-1].MismatchKind != MismatchKindBalanceMismatch {
				return fmt.Errorf("%s: kept %+v", c.name, kept)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
