//go:build integration

package reconciliation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// SRF2R closure (security re-review of the F2 fix round): SRF2R-1 (streaming cursor read + per-read scan budget,
// keyset indexes in 0172), SRF2R-2 (match_stalled counts an import only when no verifying run committed after its
// fetch; a held lock still trips), SRF2R-3 (0172 down/up round trip; the guard itself is pinned by the MA020 tests).

// srf2rBudget shortens the cursor scan budget for one test.
func srf2rBudget(t *testing.T, n int) {
	t.Helper()
	prev := cursorScanBudget
	cursorScanBudget = n
	t.Cleanup(func() { cursorScanBudget = prev })
}

// srf2rGenuine returns the genuine cursor run's import id and coverage start.
func srf2rGenuine(t *testing.T, w *payWorld, runID uuid.UUID) (uuid.UUID, time.Time) {
	t.Helper()
	var imp uuid.UUID
	var start time.Time
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT run_import_id, period_start FROM reconciliation_runs WHERE id = $1`, runID).Scan(&imp, &start)
	}); err != nil {
		t.Fatal(err)
	}
	return imp, start
}

// srf2rFlood plants n sealed-looking forged run rows AS THE RUNTIME ROLE (random 64-hex seals, a real kid, the genuine
// import id; all accepted by the 0168 CHECKs and FK), with coverage_end in (cursor, cursor + n us] and run_at = now():
// every one of them sorts before the genuine cursor run in both keyset orders.
func srf2rFlood(t *testing.T, rt *db.Pool, w *payWorld, cursor time.Time, imp uuid.UUID, start time.Time, n int) {
	t.Helper()
	if err := rt.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO reconciliation_runs (tenant_id, stream, period_start, period_end, status, coverage_end, provider_id, window_truncated,
			                                                        run_import_id, run_seal, run_seal_kid)
			SELECT $1, 'payment_statement', $5, $2::timestamptz + g * interval '1 microsecond', 'clean', $2::timestamptz + g * interval '1 microsecond', $3, false,
			       $6, md5(g::text || $7) || md5($7 || g::text), 'f2-test'
			  FROM generate_series(1, $4) g`, w.f.tenantID, cursor, payProvA, n, start, imp, uuid.NewString())
		return err
	}); err != nil {
		t.Fatalf("flood as runtime: %v", err)
	}
}

func srf2rAuditPhases(t *testing.T, pool *db.Pool, tenantID uuid.UUID, provider string) []string {
	t.Helper()
	var out []string
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT metadata->>'phase' FROM audit_log WHERE tenant_id = $1 AND action = 'reconciliation.sweep_run_failed'
			AND metadata->>'provider_id' = $2 ORDER BY created_at, id`, tenantID, provider)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func srf2rImports(t *testing.T, w *payWorld) int {
	t.Helper()
	var n int
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_statement_imports WHERE tenant_id = $1 AND provider_id = $2`, w.f.tenantID, payProvA).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// SRF2R-1 (normal): a flood spanning several fetch batches of the ONE server-side cursor (3 x 512 rows, inside the
// budget) neither moves nor stops the stream; every forged row is scanned once and reported.
func TestSRF2R1_StreamingCursorReadCrossesBatchesUnderAFlood(t *testing.T) {
	w := newPayWorld(t)
	rt := f2RuntimePool(t)
	keys := f2Keys(t)
	src := &recSource{provider: payProvA, fn: contiguousEmpty}
	cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: src, payProvB: w.srcB}, AllowSynthetic: true, ImportKeys: keys}
	scopedSweep(t, w.pool, w.f, cfg)
	base := f2SealedCursor(t, w, keys, src)
	imp, start := srf2rGenuine(t, w, base.cursorRunID)
	const flood = 3*cursorRunsPerPage + 7
	srf2rFlood(t, rt, w, base.cursor, imp, start, flood)
	st := f2SealedCursor(t, w, keys, src)
	if !st.cursor.Equal(base.cursor) || st.cursorRunID != base.cursorRunID {
		t.Fatalf("the flood must not move the cursor: %+v vs %+v", st, base)
	}
	if st.scanned != flood+1 || st.rejected != flood || st.unbacked != flood {
		t.Fatalf("every forged row scanned once and reported: scanned=%d rejected=%d unbacked=%d (flood %d)", st.scanned, st.rejected, st.unbacked, flood)
	}
	a := outcomeFor(t, scopedSweep(t, w.pool, w.f, cfg), payProvA)
	if a.Err != nil || a.Run.ID == uuid.Nil {
		t.Fatalf("the stream keeps working inside the budget: %+v", a)
	}
	if after := f2SealedCursor(t, w, keys, src); !after.cursor.After(base.cursor) || after.cursorRunID != a.Run.ID || after.scanned != 1 || after.rejected != 0 {
		t.Fatalf("the genuine run moves the cursor and is the first row scanned: %+v (run %s)", after, a.Run.ID)
	}
}

// SRF2R-1 (budget; retry; recovery): a flood past the per-read budget stops the cursor read with the typed error;
// the sweep fails LOUD as cursor_flood (audit + durable P1), fetches nothing, trusts nothing, and leaves the cursor
// where it was. A retry fails the same way (no fetch, still one alert). Never fail open: no zero or partial cursor
// is ever used. Recovery (here: a larger budget, in production the owner-authorised removal procedure) resumes from
// the genuine cursor.
func TestSRF2R1_FloodPastTheBudgetFailsLoudAsCursorFloodAndFetchesNothing(t *testing.T) {
	w := newPayWorld(t)
	rt := f2RuntimePool(t)
	keys := f2Keys(t)
	src := &recSource{provider: payProvA, fn: contiguousEmpty}
	cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: src, payProvB: w.srcB}, AllowSynthetic: true, ImportKeys: keys}
	scopedSweep(t, w.pool, w.f, cfg)
	base := f2SealedCursor(t, w, keys, src)
	imp, start := srf2rGenuine(t, w, base.cursorRunID)
	srf2rFlood(t, rt, w, base.cursor, imp, start, 1300)
	srf2rBudget(t, 600)

	if _, err := readPaymentCursorState(context.Background(), w.pool, w.f.tenantID, payProvA, cursorBasisFor(src, PaymentStatementOptions{ImportKeys: keys})); !errors.Is(err, ErrPaymentCursorScanBudgetExceeded) {
		t.Fatalf("the read must stop at the budget with the typed error, got %v", err)
	}
	reqs, imports := len(src.requests()), srf2rImports(t, w)
	for attempt := 1; attempt <= 2; attempt++ {
		a := outcomeFor(t, scopedSweep(t, w.pool, w.f, cfg), payProvA)
		if !errors.Is(a.Err, ErrPaymentCursorScanBudgetExceeded) || a.Run.ID != uuid.Nil {
			t.Fatalf("attempt %d: the run must fail with the budget error and commit nothing: %+v", attempt, a)
		}
		if n := len(src.requests()); n != reqs {
			t.Fatalf("attempt %d: nothing may be fetched without a trusted cursor: %d -> %d requests", attempt, reqs, n)
		}
		if n := srf2rImports(t, w); n != imports {
			t.Fatalf("attempt %d: nothing may be stored: %d -> %d imports", attempt, imports, n)
		}
		phases := srf2rAuditPhases(t, w.pool, w.f.tenantID, payProvA)
		if len(phases) != attempt || phases[attempt-1] != "cursor_flood" {
			t.Fatalf("attempt %d: one P1 failure audit per run, phase cursor_flood: %v", attempt, phases)
		}
	}
	var alerts []alertinject.Row
	for _, r := range alertinject.Find(alertinject.WaitForKind(t, w.pool, w.f.tenantID, "reconciliation.run_failed", 1), "reconciliation.run_failed") {
		if r.Discriminator == "stream:payment_statement:provider:"+payProvA && r.Severity == "p1" {
			alerts = append(alerts, r)
		}
	}
	if len(alerts) != 1 || alerts[0].Attributes["phase"] != "cursor_flood" {
		t.Fatalf("one durable P1 run_failed alert (phase cursor_flood) for the pair: %+v", alerts)
	}
	cursorScanBudget = CursorScanBudget
	if st := f2SealedCursor(t, w, keys, src); !st.cursor.Equal(base.cursor) || st.cursorRunID != base.cursorRunID {
		t.Fatalf("the failed runs must not have moved the cursor: %+v", st)
	}
	a := outcomeFor(t, scopedSweep(t, w.pool, w.f, cfg), payProvA)
	if a.Err != nil || a.Run.ID == uuid.Nil {
		t.Fatalf("recovery: %+v", a)
	}
	if r := src.requests(); !r[len(r)-1].ContinueFrom.Equal(base.cursor) {
		t.Fatalf("recovery continues from the genuine cursor %s, got %s", base.cursor, r[len(r)-1].ContinueFrom)
	}
}

// SRF2R-1 (partial failure): the flood appears AFTER the pre-fetch cursor read (planted during the fetch), so the
// re-read under the advisory lock in the match transaction is the one that hits the budget. The match rolls back
// (no run, the import stays an orphan), the run fails as cursor_flood, the cursor does not move, and the next sweep
// with a sufficient budget re-covers the window from the genuine cursor.
func TestSRF2R1_FloodDuringTheSweepFailsTheMatchAndCommitsNoRun(t *testing.T) {
	w := newPayWorld(t)
	rt := f2RuntimePool(t)
	keys := f2Keys(t)
	var plant func()
	src := &recSource{provider: payProvA}
	src.fn = func(n int, req statement.PaymentFetchRequest) (statement.PaymentStatement, error) {
		if n == 2 && plant != nil {
			plant()
		}
		return contiguousEmpty(n, req)
	}
	cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: src, payProvB: w.srcB}, AllowSynthetic: true, ImportKeys: keys}
	scopedSweep(t, w.pool, w.f, cfg)
	base := f2SealedCursor(t, w, keys, src)
	imp, start := srf2rGenuine(t, w, base.cursorRunID)
	plant = func() { srf2rFlood(t, rt, w, base.cursor, imp, start, 1300) }
	srf2rBudget(t, 600)
	a := outcomeFor(t, scopedSweep(t, w.pool, w.f, cfg), payProvA)
	if !errors.Is(a.Err, ErrPaymentCursorScanBudgetExceeded) || a.Run.ID != uuid.Nil {
		t.Fatalf("the match re-read must hit the budget and commit no run: %+v", a)
	}
	if phases := srf2rAuditPhases(t, w.pool, w.f.tenantID, payProvA); len(phases) != 1 || phases[0] != "cursor_flood" {
		t.Fatalf("the match failure is audited as cursor_flood: %v", phases)
	}
	cursorScanBudget = CursorScanBudget
	if st := f2SealedCursor(t, w, keys, src); st.cursorRunID != base.cursorRunID {
		t.Fatalf("no run committed, the cursor stays: %+v", st)
	}
	b := outcomeFor(t, scopedSweep(t, w.pool, w.f, cfg), payProvA)
	if b.Err != nil || b.Run.ID == uuid.Nil || !b.Run.PeriodStart.UTC().Truncate(time.Microsecond).Equal(base.cursor) {
		t.Fatalf("the next sweep re-covers the window from the genuine cursor %s: %+v", base.cursor, b)
	}
}

// SRF2R-1 (concurrency): concurrent cursor reads over the same flood are independent (each its own transaction and
// server-side cursor): inside the budget all agree on the genuine cursor; past it all fail with the typed error.
func TestSRF2R1_ConcurrentCursorReadsUnderAFlood(t *testing.T) {
	w := newPayWorld(t)
	rt := f2RuntimePool(t)
	keys := f2Keys(t)
	src := &recSource{provider: payProvA, fn: contiguousEmpty}
	cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: src, payProvB: w.srcB}, AllowSynthetic: true, ImportKeys: keys}
	scopedSweep(t, w.pool, w.f, cfg)
	base := f2SealedCursor(t, w, keys, src)
	imp, start := srf2rGenuine(t, w, base.cursorRunID)
	srf2rFlood(t, rt, w, base.cursor, imp, start, 1100)
	basis := cursorBasisFor(src, PaymentStatementOptions{ImportKeys: keys})
	run := func() ([]cursorState, []error) {
		const n = 6
		sts, errs := make([]cursorState, n), make([]error, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				sts[i], errs[i] = readPaymentCursorState(context.Background(), w.pool, w.f.tenantID, payProvA, basis)
			}(i)
		}
		wg.Wait()
		return sts, errs
	}
	sts, errs := run()
	for i := range sts {
		if errs[i] != nil || sts[i].cursorRunID != base.cursorRunID || sts[i].unbacked != 1100 {
			t.Fatalf("reader %d: %+v %v", i, sts[i], errs[i])
		}
	}
	srf2rBudget(t, 600)
	_, errs = run()
	for i, err := range errs {
		if !errors.Is(err, ErrPaymentCursorScanBudgetExceeded) {
			t.Fatalf("reader %d past the budget: %v", i, err)
		}
	}
}

// srf2rStall sets the stall threshold and the lock wait for one test.
func srf2rStall(t *testing.T, after, lockWait time.Duration) {
	t.Helper()
	prevA, prevW := matchStallAfter, matchStallLockWait
	matchStallAfter, matchStallLockWait = after, lockWait
	t.Cleanup(func() { matchStallAfter, matchStallLockWait = prevA, prevW })
}

// srf2rOrphan makes one sealed orphan import of the source above the cursor: a sweep whose match is skipped because
// the test holds the stream's advisory lock. The stall threshold must be longer than this takes.
func srf2rOrphan(t *testing.T, w *payWorld, cfg PaymentSweepConfig) {
	t.Helper()
	release := f2HoldStreamLock(t, w.f.tenantID)
	if a := outcomeFor(t, scopedSweep(t, w.pool, w.f, cfg), payProvA); !a.Skipped || a.Run.ID != uuid.Nil {
		t.Fatalf("setup: the match must be skipped: %+v", a)
	}
	release()
}

// SRF2R-2 (the reviewer's probe): a benign contention loser's import is NOT a stall. Its import sits above the trusted
// cursor and is older than the threshold, but the next sweep's own run commits after the import's fetch, so the
// stream progressed: no match_stalled audit, no match_stalled alert. (Before SRF2R-2 the next sweep's phase-0 check
// raised the durable P1: payment_match_stalled audits = 1.)
func TestSRF2R2_BenignContentionLoserIsNotAStall(t *testing.T) {
	srf2rStall(t, time.Hour, 200*time.Millisecond)
	w := newPayWorld(t)
	keys := f2Keys(t)
	src := &recSource{provider: payProvA, fn: contiguousEmpty}
	cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: src, payProvB: w.srcB}, AllowSynthetic: true, ImportKeys: keys}
	scopedSweep(t, w.pool, w.f, cfg)
	srf2rOrphan(t, w, cfg)
	time.Sleep(20 * time.Millisecond)
	matchStallAfter = 10 * time.Millisecond // the orphan is now "old"
	a := outcomeFor(t, scopedSweep(t, w.pool, w.f, cfg), payProvA)
	if a.Err != nil || a.Run.ID == uuid.Nil {
		t.Fatalf("the next sweep runs: %+v", a)
	}
	if n := countAudit(t, w.pool, w.f.tenantID, "reconciliation.payment_match_stalled", payProvA); n != 0 {
		t.Fatalf("benign contention must not raise match_stalled, got %d audits", n)
	}
	for _, r := range alertinject.Find(alertinject.ForSubject(t, w.pool, w.f.tenantID), "reconciliation.run_failed") {
		if strings.HasSuffix(r.Discriminator, ":match_stalled") {
			t.Fatalf("no match_stalled alert for benign contention: %+v", r)
		}
	}
	// Duplicate sweep: still nothing.
	scopedSweep(t, w.pool, w.f, cfg)
	if n := countAudit(t, w.pool, w.f.tenantID, "reconciliation.payment_match_stalled", payProvA); n != 0 {
		t.Fatalf("a repeated sweep must not raise match_stalled either, got %d", n)
	}
}

// SRF2R-2 (in-flight winner): the loser's end-of-sweep check runs while the WINNER is still inside its match
// transaction (holding the stream lock, paused by the test hook). The rule cannot see the winner's run yet, so the
// check waits for the lock to be released, re-evaluates, sees the committed run after the orphan's fetch, and raises
// nothing.
func TestSRF2R2_LoserWaitsForTheWinnersCommitInsteadOfRaising(t *testing.T) {
	srf2rStall(t, time.Hour, 20*time.Second)
	w := newPayWorld(t)
	keys := f2Keys(t)
	src := &recSource{provider: payProvA, fn: contiguousEmpty}
	cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: src, payProvB: w.srcB}, AllowSynthetic: true, ImportKeys: keys}
	scopedSweep(t, w.pool, w.f, cfg)
	srf2rOrphan(t, w, cfg)
	time.Sleep(1100 * time.Millisecond)
	matchStallAfter = time.Second // the orphan qualifies; the imports of the two sweeps below do not

	paused, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	payMatchAfterAttemptsHook = func() {
		once.Do(func() {
			close(paused)
			<-resume
		})
	}
	t.Cleanup(func() { payMatchAfterAttemptsHook = nil })
	winner := make(chan StreamOutcome, 1)
	go func() {
		winner <- ReconcilePaymentStatementForTenant(context.Background(), w.pool, nil, w.f.tenantID, time.Now().Add(-time.Hour), time.Now(), src,
			PaymentStatementOptions{Continuation: true, ImportKeys: keys})
	}()
	<-paused
	loser := make(chan StreamOutcome, 1)
	go func() {
		loser <- ReconcilePaymentStatementForTenant(context.Background(), w.pool, nil, w.f.tenantID, time.Now().Add(-time.Hour), time.Now(), src,
			PaymentStatementOptions{Continuation: true, ImportKeys: keys})
	}()
	// The loser skips (lock held by the paused winner) and its check is now waiting for the lock.
	deadline := time.Now().Add(15 * time.Second)
	for countAudit(t, w.pool, w.f.tenantID, "reconciliation.payment_match_skipped", payProvA) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the loser never skipped")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	close(resume)
	if wo := <-winner; wo.Err != nil || wo.Run.ID == uuid.Nil {
		t.Fatalf("winner: %+v", wo)
	}
	if lo := <-loser; lo.Err != nil || !lo.Skipped {
		t.Fatalf("loser: %+v", lo)
	}
	if n := countAudit(t, w.pool, w.f.tenantID, "reconciliation.payment_match_stalled", payProvA); n != 0 {
		t.Fatalf("the loser must wait for the winner's commit, not raise: %d match_stalled audits", n)
	}
}

// SRF2R-2 (held lock): a CONTINUOUSLY held lock commits no run, so the stall still trips: after the bounded wait the
// durable P1 match_stalled is raised and audited with lock_still_held = true. Releasing the lock, the next sweep
// re-covers the window and nothing new is raised.
func TestSRF2R2_ContinuouslyHeldLockStillTrips(t *testing.T) {
	srf2rStall(t, time.Hour, 150*time.Millisecond)
	w := newPayWorld(t)
	keys := f2Keys(t)
	src := &recSource{provider: payProvA, fn: contiguousEmpty}
	cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: src, payProvB: w.srcB}, AllowSynthetic: true, ImportKeys: keys}
	scopedSweep(t, w.pool, w.f, cfg)
	release := f2HoldStreamLock(t, w.f.tenantID)
	scopedSweep(t, w.pool, w.f, cfg) // skipped: an orphan import
	time.Sleep(20 * time.Millisecond)
	matchStallAfter = 10 * time.Millisecond
	a := outcomeFor(t, scopedSweep(t, w.pool, w.f, cfg), payProvA) // still held: skipped again
	if !a.Skipped || a.Run.ID != uuid.Nil {
		t.Fatalf("setup: skipped under the held lock: %+v", a)
	}
	if n := countAudit(t, w.pool, w.f.tenantID, "reconciliation.payment_match_stalled", payProvA); n != 1 {
		t.Fatalf("a held lock must raise match_stalled once per sweep: %d", n)
	}
	var held string
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT metadata->>'lock_still_held' FROM audit_log WHERE tenant_id = $1 AND action = 'reconciliation.payment_match_stalled'`, w.f.tenantID).Scan(&held)
	}); err != nil || held != "true" {
		t.Fatalf("the audit says the lock was still held: %q %v", held, err)
	}
	if !f2AlertWithTail(t, w.pool, w.f.tenantID, payProvA, "match_stalled") {
		t.Fatal("the durable match_stalled P1 must be raised")
	}
	release()
	b := outcomeFor(t, scopedSweep(t, w.pool, w.f, cfg), payProvA)
	if b.Err != nil || b.Run.ID == uuid.Nil {
		t.Fatalf("recovery sweep: %+v", b)
	}
	if n := countAudit(t, w.pool, w.f.tenantID, "reconciliation.payment_match_stalled", payProvA); n != 1 {
		t.Fatalf("after recovery nothing new is raised: %d", n)
	}
}

// SRF2R-2 (forgery / budget): run rows forged after the orphan's fetch are never progress (their run seals do not
// verify), so they cannot suppress the stall; a flood past the budget makes the progress read stop and the stall
// is raised (fail closed). Only a genuine committed run clears it.
func TestSRF2R2_ForgedRunsAreNotProgressAndTheBudgetFailsClosed(t *testing.T) {
	srf2rStall(t, time.Hour, 50*time.Millisecond)
	w := newPayWorld(t)
	rt := f2RuntimePool(t)
	keys := f2Keys(t)
	src := &recSource{provider: payProvA, fn: contiguousEmpty}
	cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: src, payProvB: w.srcB}, AllowSynthetic: true, ImportKeys: keys}
	scopedSweep(t, w.pool, w.f, cfg)
	base := f2SealedCursor(t, w, keys, src)
	imp, start := srf2rGenuine(t, w, base.cursorRunID)
	srf2rOrphan(t, w, cfg)
	time.Sleep(20 * time.Millisecond)
	srf2rFlood(t, rt, w, base.cursor, imp, start, 1100) // run_at = now(): after the orphan's fetch
	matchStallAfter = 10 * time.Millisecond
	basis := cursorBasisFor(src, PaymentStatementOptions{ImportKeys: keys})
	st := f2SealedCursor(t, w, keys, src)
	ev, err := evaluateMatchStall(context.Background(), w.pool, w.f.tenantID, payProvA, basis, st)
	if err != nil || ev.n != 1 || ev.budgetHit || !ev.progressAt.IsZero() {
		t.Fatalf("forged runs are not progress: %+v %v", ev, err)
	}
	srf2rBudget(t, 600)
	ev, err = evaluateMatchStall(context.Background(), w.pool, w.f.tenantID, payProvA, basis, st)
	if err != nil || ev.n != 1 || !ev.budgetHit {
		t.Fatalf("past the budget the stall is raised (fail closed): %+v %v", ev, err)
	}
	cursorScanBudget = CursorScanBudget
	if a := outcomeFor(t, scopedSweep(t, w.pool, w.f, cfg), payProvA); a.Err != nil || a.Run.ID == uuid.Nil {
		t.Fatalf("genuine sweep: %+v", a)
	}
	ev, err = evaluateMatchStall(context.Background(), w.pool, w.f.tenantID, payProvA, basis, st)
	if err != nil || ev.n != 0 || ev.progressAt.IsZero() {
		t.Fatalf("a genuine run after the fetch is progress: %+v %v", ev, err)
	}
}

// 0172 (SRF2R-1 / SRF2R-3) on a throwaway database: up installs the tombstone-only payment_ref_cleared (shape kept:
// STABLE, pinned search_path, not SECURITY DEFINER) and the two keyset indexes; down restores the exact 0119 body and
// drops the indexes; re-up is clean.
func TestMigration0172_UpDownRoundTrip(t *testing.T) {
	src := "../../migrations"
	dir := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	var body0119 string
	for _, e := range entries {
		n, perr := strconv.ParseInt(e.Name()[:4], 10, 64)
		if perr != nil || n > 172 {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(src, e.Name()))
		if e.Name() == "0119_ma020_sync_mismatch.up.sql" {
			s := string(b)
			i := strings.Index(s, "CREATE FUNCTION payment_ref_cleared")
			i += strings.Index(s[i:], "AS $$") + len("AS $$")
			j := strings.Index(s[i:], "$$ LANGUAGE sql STABLE")
			body0119 = s[i : i+j]
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pool, err := db.Connect(context.Background(), scratchdb.New(t, "srf2r_0172"), 4, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ctx := context.Background()
	if applied, err := pool.MigrateUp(ctx, dir); err != nil || applied[len(applied)-1] != 172 {
		t.Fatalf("migrate through 0172: %v", err)
	}
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	read := func() (string, []string, bool, string, int) {
		var src, vol string
		var cfg []string
		var secdef bool
		var idx int
		if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT prosrc, COALESCE(proconfig, '{}'), prosecdef, provolatile::text FROM pg_proc
				WHERE oid = 'payment_ref_cleared(uuid, text, text)'::regprocedure`).Scan(&src, &cfg, &secdef, &vol); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE tablename = 'reconciliation_runs'
				AND indexname IN ('reconciliation_runs_payment_sealed_keyset', 'reconciliation_runs_payment_sealed_by_run_at')`).Scan(&idx)
		}); err != nil {
			t.Fatal(err)
		}
		return src, cfg, secdef, vol, idx
	}
	body, cfg, secdef, vol, idx := read()
	if strings.Contains(body, "payment_statement_lines") || !strings.Contains(body, "'tombstone'") || idx != 2 ||
		secdef || vol != "s" || len(cfg) != 1 || cfg[0] != "search_path=pg_catalog, public, pg_temp" {
		t.Fatalf("0172 up: body=%q idx=%d secdef=%v vol=%s cfg=%v", body, idx, secdef, vol, cfg)
	}
	if _, err := pool.MigrateDown(ctx, dir, 1); err != nil {
		t.Fatalf("down: %v", err)
	}
	body, cfg, _, _, idx = read()
	if body0119 == "" || norm(body) != norm(body0119) || idx != 0 || len(cfg) != 1 {
		t.Fatalf("0172 down must restore the 0119 body and drop the indexes: idx=%d body=%q", idx, body)
	}
	if applied, err := pool.MigrateUp(ctx, dir); err != nil || len(applied) != 1 || applied[0] != 172 {
		t.Fatalf("re-up: %v %v", applied, err)
	}
	if body, _, _, _, idx = read(); strings.Contains(body, "payment_statement_lines") || idx != 2 {
		t.Fatalf("re-up: idx=%d body=%q", idx, body)
	}
}
