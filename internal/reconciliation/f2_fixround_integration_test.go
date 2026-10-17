//go:build integration

package reconciliation

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

// F2 fix round (security SRF2 / ledger-finance LFF2 reviews): the run seal (migration 0168) and its probes.

// f2HoldStreamLock holds the payment_statement stream's per-tenant advisory lock on a dedicated RUNTIME-role
// connection (any role may take advisory locks) until the returned release is called.
func f2HoldStreamLock(t *testing.T, tenantID uuid.UUID) (release func()) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), os.Getenv("TEST_RUNTIME_DATABASE_URL"))
	if err != nil {
		t.Fatalf("connect as runtime: %v", err)
	}
	if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_lock(hashtextextended('reconciliation:payment_statement:' || $1::text, 0))`, tenantID); err != nil {
		t.Fatalf("hold the stream lock: %v", err)
	}
	done := false
	release = func() {
		if !done {
			done = true
			_ = conn.Close(context.Background())
		}
	}
	t.Cleanup(release)
	return release
}

type f2Import struct {
	id                   uuid.UUID
	coverageStart, endAt time.Time
}

// f2NewestImport is the newest stored import of the pair.
func f2NewestImport(t *testing.T, pool *db.Pool, tenantID uuid.UUID, provider string) f2Import {
	t.Helper()
	var im f2Import
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id, coverage_start, coverage_end FROM payment_statement_imports WHERE tenant_id = $1 AND provider_id = $2
			ORDER BY fetched_at DESC, id DESC LIMIT 1`, tenantID, provider).Scan(&im.id, &im.coverageStart, &im.endAt)
	}); err != nil {
		t.Fatal(err)
	}
	return im
}

func f2AlertWithTail(t *testing.T, pool *db.Pool, tenantID uuid.UUID, provider, tail string) bool {
	t.Helper()
	for _, r := range alertinject.Find(alertinject.WaitForKind(t, pool, tenantID, "reconciliation.run_failed", 1), "reconciliation.run_failed") {
		if strings.HasPrefix(r.Discriminator, "stream:payment_statement:provider:"+provider+":"+tail) && r.Severity == "p1" {
			return true
		}
	}
	return false
}

// THE SRF2-1 / LFF2-1 PROBE: as the runtime role, hold the stream's advisory lock during a sweep so the genuine
// match is SKIPPED and leaves a sealed import without a run, release it, then INSERT one run row at that orphan
// import's coverage_end (unsealed, or with a made-up seal naming the orphan import). Before 0168 the sealed cursor
// trusted it and silently skipped the window. Now: the skip is audited, the forged row never becomes the cursor (its
// run seal does not verify), the anomaly is audited and alerted, and the next sweep re-covers the window from the
// genuine cursor.
func TestF2_SRF2_1_AdvisoryLockOrphanPlusForgedRun_RefusedAndLoud(t *testing.T) {
	for _, variant := range []string{"unsealed_run_row", "made_up_run_seal_naming_the_orphan"} {
		t.Run(variant, func(t *testing.T) {
			w := newPayWorld(t)
			rt := f2RuntimePool(t)
			keys := f2Keys(t)
			src := &recSource{provider: payProvA}
			src.fn = func(_ int, req statement.PaymentFetchRequest) (statement.PaymentStatement, error) {
				return w.srcA.Fetch(context.Background(), req)
			}
			cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: src, payProvB: w.srcB}, AllowSynthetic: true, RunLineBound: 100, ImportKeys: keys}
			scopedSweep(t, w.pool, w.f, cfg)
			genuine := f2SealedCursor(t, w, keys, src)
			if genuine.cursor.IsZero() {
				t.Fatal("setup: sealed cursor")
			}
			dep := w.deposit(t, 777) // its line is dated inside the window the attack targets

			release := f2HoldStreamLock(t, w.f.tenantID)
			if a := outcomeFor(t, scopedSweep(t, w.pool, w.f, cfg), payProvA); !a.Skipped || a.Run.ID != uuid.Nil {
				t.Fatalf("with the lock held the match must be skipped: %+v", a)
			}
			release()
			if n := countAudit(t, w.pool, w.f.tenantID, "reconciliation.payment_match_skipped", payProvA); n != 1 {
				t.Fatalf("a skipped match leaving an import behind must be audited, got %d", n)
			}
			orphan := f2NewestImport(t, w.pool, w.f.tenantID, payProvA)
			if !orphan.endAt.After(genuine.cursor) {
				t.Fatal("setup: the orphan import lies above the cursor")
			}
			if err := rt.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				if variant == "unsealed_run_row" {
					_, err := tx.Exec(ctx, `INSERT INTO reconciliation_runs (tenant_id, stream, period_start, period_end, status, coverage_end, provider_id, window_truncated)
						VALUES ($1, 'payment_statement', $2, $3, 'clean', $3, $4, false)`, w.f.tenantID, orphan.coverageStart, orphan.endAt, payProvA)
					return err
				}
				_, err := tx.Exec(ctx, `INSERT INTO reconciliation_runs (tenant_id, stream, period_start, period_end, status, coverage_end, provider_id, window_truncated,
					run_import_id, run_seal, run_seal_kid)
					VALUES ($1, 'payment_statement', $2, $3, 'clean', $3, $4, false, $5, $6, 'f2-test')`,
					w.f.tenantID, orphan.coverageStart, orphan.endAt, payProvA, orphan.id, strings.Repeat("ab", 32))
				return err
			}); err != nil {
				t.Fatalf("forge as runtime: %v", err)
			}
			// Control: the pre-0168 rule (any backing import) would trust the forged row.
			if c := mustCursor(t, w.pool, w.f.tenantID, payProvA); !c.Equal(orphan.endAt.UTC().Truncate(time.Microsecond)) {
				t.Fatalf("control: the backing-import rule moves to the orphan's end, got %s", c)
			}
			if st := f2SealedCursor(t, w, keys, src); !st.cursor.Equal(genuine.cursor) || st.unbacked < 1 {
				t.Fatalf("the sealed cursor must ignore the forged run and report it: %+v (genuine %s)", st, genuine.cursor)
			}
			a := outcomeFor(t, scopedSweep(t, w.pool, w.f, cfg), payProvA)
			if a.Err != nil || a.Run.ID == uuid.Nil {
				t.Fatalf("the next sweep runs: %+v", a)
			}
			if reqs := src.requests(); !reqs[len(reqs)-1].ContinueFrom.Equal(genuine.cursor) {
				t.Fatalf("the window is re-covered from the genuine cursor %s, got %s", genuine.cursor, reqs[len(reqs)-1].ContinueFrom)
			}
			if a.Run.PeriodStart.UTC().Truncate(time.Microsecond) != genuine.cursor {
				t.Fatalf("the committed run covers the attacked window: starts %s", a.Run.PeriodStart)
			}
			var matched int
			if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM payment_statement_lines l JOIN reconciliation_runs r ON r.run_import_id = l.import_id
					WHERE l.tenant_id = $1 AND l.provider_reference = $2 AND r.id = $3`, w.f.tenantID, *dep.ProviderReference, a.Run.ID).Scan(&matched)
			}); err != nil || matched != 1 {
				t.Fatalf("the deposit's line must be matched by the genuine run of the attacked window: %d %v", matched, err)
			}
			if !f2AlertWithTail(t, w.pool, w.f.tenantID, payProvA, "cursor_anomaly:") {
				t.Fatal("the forged run row must raise the durable cursor_anomaly P1")
			}
			if n := countAudit(t, w.pool, w.f.tenantID, "reconciliation.payment_cursor_anomaly", payProvA); n < 1 {
				t.Fatal("the forged run row must be audited")
			}
		})
	}
}

// SRF2-1 (c): holding the lock across sweeps is a visible stall: an import of the source above the trusted cursor,
// older than the stall threshold, raises the durable match_stalled P1 and an audit row.
func TestF2_SRF2_1_HeldLockIsAVisibleStall(t *testing.T) {
	prev, prevWait := matchStallAfter, matchStallLockWait
	matchStallAfter = time.Millisecond
	matchStallLockWait = 100 * time.Millisecond // SRF2R-2: fixture only, the bounded wait for a held lock
	t.Cleanup(func() { matchStallAfter, matchStallLockWait = prev, prevWait })
	w := newPayWorld(t)
	keys := f2Keys(t)
	src := &recSource{provider: payProvA, fn: contiguousEmpty}
	cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: src, payProvB: w.srcB}, AllowSynthetic: true, ImportKeys: keys}
	scopedSweep(t, w.pool, w.f, cfg)
	release := f2HoldStreamLock(t, w.f.tenantID)
	time.Sleep(5 * time.Millisecond)
	scopedSweep(t, w.pool, w.f, cfg) // skipped: leaves an import above the cursor
	time.Sleep(5 * time.Millisecond)
	scopedSweep(t, w.pool, w.f, cfg) // phase 0 sees the stalled import
	release()
	if !f2AlertWithTail(t, w.pool, w.f.tenantID, payProvA, "match_stalled") {
		t.Fatal("a held lock must surface as the durable match_stalled P1")
	}
	if n := countAudit(t, w.pool, w.f.tenantID, "reconciliation.payment_match_stalled", payProvA); n < 1 {
		t.Fatal("the stall must be audited")
	}
	// Recovery: the next unblocked sweep matches from the genuine cursor; once the cursor passes the orphan imports
	// nothing is stalled any more.
	before := countAudit(t, w.pool, w.f.tenantID, "reconciliation.payment_match_stalled", payProvA)
	scopedSweep(t, w.pool, w.f, cfg)
	scopedSweep(t, w.pool, w.f, cfg)
	if n := countAudit(t, w.pool, w.f.tenantID, "reconciliation.payment_match_stalled", payProvA); n > before+1 {
		t.Fatalf("after recovery the stall stops being reported: %d -> %d", before, n)
	}
}

// SRF2-2 (stream side): a genuine sealed run row COPIED as the runtime role (new id, run_at = now(), the original's
// seal, kid and import) is not a cursor and is reported (run_at after the cursor run's).
func TestF2_SRF2_2_CopiedRunRowIsNeverTrustedAndIsReported(t *testing.T) {
	w := newPayWorld(t)
	rt := f2RuntimePool(t)
	keys := f2Keys(t)
	src := &recSource{provider: payProvA, fn: contiguousEmpty}
	cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: src, payProvB: w.srcB}, AllowSynthetic: true, ImportKeys: keys}
	scopedSweep(t, w.pool, w.f, cfg)
	first := f2SealedCursor(t, w, keys, src)
	scopedSweep(t, w.pool, w.f, cfg)
	second := f2SealedCursor(t, w, keys, src)
	if err := rt.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO reconciliation_runs (tenant_id, stream, period_start, period_end, status, coverage_end, provider_id, window_truncated,
			run_import_id, run_seal, run_seal_kid)
			SELECT tenant_id, stream, period_start, period_end, status, coverage_end, provider_id, window_truncated, run_import_id, run_seal, run_seal_kid
			  FROM reconciliation_runs WHERE id = $1`, first.cursorRunID)
		return err
	}); err != nil {
		t.Fatalf("copy as runtime: %v", err)
	}
	st := f2SealedCursor(t, w, keys, src)
	if !st.cursor.Equal(second.cursor) || st.cursorRunID != second.cursorRunID || st.unbacked != 1 {
		t.Fatalf("a copied run row must not be trusted and must be reported: %+v", st)
	}
}

// LFF2-5 (a): a forged, UNSEALED import (as runtime) holding a `succeeded` line on the attempt's reference no longer
// exempts a platform-succeeded deposit the provider never recorded (keys on): still pay_missing_provider_record.
// Without keys the same forged line (amount 1) is never an exemption either: it is reported as an amount mismatch.
func TestF2_LFF2_5_ForgedUnsealedLineDoesNotExempt(t *testing.T) {
	for _, keyed := range []bool{true, false} {
		w := newPayWorld(t)
		rt := f2RuntimePool(t)
		a := w.deposit(t, 410)
		ref := *a.ProviderReference
		hiding := payDivergentSource{inner: w.srcA, mutate: func(s statement.PaymentStatement) statement.PaymentStatement {
			out := s.Lines[:0]
			for _, l := range s.Lines {
				if l.ProviderReference != ref {
					out = append(out, l)
				}
			}
			s.Lines = out
			return s
		}}
		cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: hiding, payProvB: w.srcB}, AllowSynthetic: true, RunLineBound: 100}
		if keyed {
			cfg.ImportKeys = f2Keys(t)
		}
		scopedSweep(t, w.pool, w.f, cfg)
		w.succeed(t, w.mockA, payProvA, a)
		if err := rt.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			imp := uuid.New()
			digest := make([]byte, 32)
			if _, err := tx.Exec(ctx, `INSERT INTO payment_statement_imports
				(id, tenant_id, provider_id, source_label, is_mock, coverage_start, coverage_end, line_count, content_digest, fetched_at)
				VALUES ($1, $2, $3, $4, true, now() - interval '1 hour', now(), 1, $5, now())`, imp, w.f.tenantID, payProvA, hiding.Label(), digest); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO payment_statement_lines (id, tenant_id, import_id, line_no, provider_id, kind, provider_reference, status, amount, asset_code, occurred_at)
				VALUES ($1, $2, $3, 0, $4, 'deposit', $5, 'succeeded', 1, 'EUR', now() - interval '1 minute')`, uuid.New(), w.f.tenantID, imp, payProvA, ref)
			return err
		}); err != nil {
			t.Fatalf("forge a line as runtime: %v", err)
		}
		o := outcomeFor(t, scopedSweep(t, w.pool, w.f, cfg), payProvA)
		if o.Err != nil || o.Run.ID == uuid.Nil {
			t.Fatalf("keyed=%t: sweep: %+v", keyed, o)
		}
		missing := f2MissingRecords(t, w, o.Run.ID)
		var amount int
		if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_mismatches WHERE reconciliation_run_id = $1 AND mismatch_kind = 'pay_amount_mismatch'
				AND reconciliation_key LIKE '%check=persisted_amount%'`, o.Run.ID).Scan(&amount)
		}); err != nil {
			t.Fatal(err)
		}
		if keyed && (len(missing) != 1 || amount != 0) {
			t.Fatalf("keyed: an unsealed forged line must neither exempt nor count: missing=%v amount=%d", missing, amount)
		}
		if !keyed && (len(missing) != 0 || amount != 1) {
			t.Fatalf("no keys: a line with another amount must be reported as an amount mismatch, never exempt: missing=%v amount=%d", missing, amount)
		}
	}
}

// LFF2-5 (coverage): without keys, a forged line with the EXACT amount and asset, dated outside its own import's
// coverage, is not usable: the attempt is still pay_missing_provider_record (never exempted).
func TestF2_LFF2_5_OutOfCoverageLineDoesNotExempt(t *testing.T) {
	w := newPayWorld(t)
	rt := f2RuntimePool(t)
	a := w.deposit(t, 430)
	ref := *a.ProviderReference
	hiding := payDivergentSource{inner: w.srcA, mutate: func(s statement.PaymentStatement) statement.PaymentStatement {
		out := s.Lines[:0]
		for _, l := range s.Lines {
			if l.ProviderReference != ref {
				out = append(out, l)
			}
		}
		s.Lines = out
		return s
	}}
	cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: hiding, payProvB: w.srcB}, AllowSynthetic: true, RunLineBound: 100}
	scopedSweep(t, w.pool, w.f, cfg)
	w.succeed(t, w.mockA, payProvA, a)
	if err := rt.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		imp := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO payment_statement_imports
			(id, tenant_id, provider_id, source_label, is_mock, coverage_start, coverage_end, line_count, content_digest, fetched_at)
			VALUES ($1, $2, $3, $4, true, now() - interval '3 hours', now() - interval '2 hours', 1, $5, now())`, imp, w.f.tenantID, payProvA, hiding.Label(), make([]byte, 32)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO payment_statement_lines (id, tenant_id, import_id, line_no, provider_id, kind, provider_reference, status, amount, asset_code, occurred_at)
			VALUES ($1, $2, $3, 0, $4, 'deposit', $5, 'succeeded', $6, $7, now() - interval '1 minute')`, uuid.New(), w.f.tenantID, imp, payProvA, ref, a.Amount, a.AssetCode)
		return err
	}); err != nil {
		t.Fatalf("forge a line as runtime: %v", err)
	}
	o := outcomeFor(t, scopedSweep(t, w.pool, w.f, cfg), payProvA)
	if o.Err != nil || o.Run.ID == uuid.Nil {
		t.Fatalf("sweep: %+v", o)
	}
	if missing := f2MissingRecords(t, w, o.Run.ID); len(missing) != 1 {
		t.Fatalf("a line outside its import's coverage must not exempt: missing=%v", missing)
	}
}

// LFF2-6: the provider reversed what the platform later recorded as succeeded: pay_status_mismatch, never an
// exemption.
func TestF2_LFF2_6_ReversedPersistedLineIsAStatusMismatch(t *testing.T) {
	w := newPayWorld(t)
	a := w.deposit(t, 420)
	ref := *a.ProviderReference
	reversing := payDivergentSource{inner: w.srcA, mutate: mutateLine(ref, func(l *statement.PaymentStatementLine) { l.Status = statement.PaymentStatusReversed })}
	cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: reversing, payProvB: w.srcB}, AllowSynthetic: true, RunLineBound: 100}
	scopedSweep(t, w.pool, w.f, cfg)
	// The platform records the success (the provider's record stays as it was: no new line after the cursor).
	w.applyReceipt(t, payProvA, payments.ReceiptEvidence{EventType: "deposit", ProviderReference: ref, Outcome: payments.OutcomeSucceeded, Amount: a.Amount, AssetCode: a.AssetCode})
	o := outcomeFor(t, scopedSweep(t, w.pool, w.f, cfg), payProvA)
	if o.Err != nil || o.Run.ID == uuid.Nil {
		t.Fatalf("sweep: %+v", o)
	}
	var n int
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_mismatches WHERE reconciliation_run_id = $1 AND mismatch_kind = 'pay_status_mismatch'
			AND reconciliation_key LIKE '%check=persisted_reversed%' AND reconciliation_key LIKE '%' || $2 || '%'`, o.Run.ID, a.ID.String()).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(f2MissingRecords(t, w, o.Run.ID)) != 0 {
		t.Fatalf("want exactly one persisted_reversed status mismatch and no missing record, got %d", n)
	}
}
