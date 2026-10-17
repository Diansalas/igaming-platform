//go:build integration

package reconciliation

import (
	"context"
	"crypto/rand"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

// F2 (platform-freeze work list): SR11-1 via SR11-10 (sealed cursor), SR11-11 (anomaly discriminator).

func f2Keys(t *testing.T) *payoutinstrument.Keys {
	t.Helper()
	m, fp := make([]byte, 32), make([]byte, 32)
	if _, err := rand.Read(m); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(fp); err != nil {
		t.Fatal(err)
	}
	k, err := payoutinstrument.NewKeys("f2-test", map[string][]byte{"f2-test": m}, "f2-fp", map[string][]byte{"f2-fp": fp})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// f2RuntimePool connects as the RUNTIME role (igaming_runtime): the attacker of SR11-1 holds exactly its SQL.
func f2RuntimePool(t *testing.T) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_RUNTIME_DATABASE_URL must be set: the SR11-1 probe runs as the runtime role")
	}
	pool, err := db.Connect(context.Background(), url, 4, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect as runtime: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// f2Forge writes, as the runtime role in an ordinary tenant session, a forged import (shaped like the stream's own:
// same source label, mock flag and importing service, 0 lines) and a run row with the same coverage_end. seal and
// kid are written verbatim (nil = unsealed).
func f2Forge(t *testing.T, rt *db.Pool, tenantID uuid.UUID, provider, label string, isMock bool, coverageEnd time.Time, seal, kid *string) {
	t.Helper()
	if err := rt.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var who string
		if err := tx.QueryRow(ctx, `SELECT current_user`).Scan(&who); err != nil {
			return err
		}
		if who != "igaming_runtime" {
			t.Fatalf("the probe must run as igaming_runtime, got %q", who)
		}
		digest := make([]byte, 32)
		_, _ = rand.Read(digest)
		if _, err := tx.Exec(ctx, `INSERT INTO payment_statement_imports
			    (id, tenant_id, provider_id, source_label, is_mock, coverage_start, coverage_end, line_count, content_digest, fetched_at,
			     payout_lines_carry_merchant_reference, imported_by_service, import_seal, seal_kid)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 0, $8, now(), false, $9, $10, $11)`,
			uuid.New(), tenantID, provider, label, isMock, coverageEnd.Add(-time.Minute), coverageEnd, digest,
			ImportedByServicePaymentStatement, seal, kid); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO reconciliation_runs (tenant_id, stream, period_start, period_end, status, coverage_end, provider_id, window_truncated)
			VALUES ($1, 'payment_statement', $2, $3, 'clean', $3, $4, false)`, tenantID, coverageEnd.Add(-time.Minute), coverageEnd, provider)
		return err
	}); err != nil {
		t.Fatalf("forge as runtime: %v", err)
	}
}

func f2AnomalyAlerts(t *testing.T, pool *db.Pool, tenantID uuid.UUID, provider string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, r := range alertinject.Find(alertinject.WaitForKind(t, pool, tenantID, "reconciliation.run_failed", 1), "reconciliation.run_failed") {
		if strings.HasPrefix(r.Discriminator, "stream:payment_statement:provider:"+provider+":cursor_anomaly:") && r.Severity == "p1" {
			out[r.Discriminator] = r.Occurrences
		}
	}
	return out
}

func f2SealedCursor(t *testing.T, w *payWorld, keys statement.StreamSealKeys, src statement.PaymentStatementSource) cursorState {
	t.Helper()
	st, err := readPaymentCursorState(context.Background(), w.pool, w.f.tenantID, src.ProviderID(), cursorBasisFor(src, PaymentStatementOptions{ImportKeys: keys}))
	if err != nil {
		t.Fatalf("sealed cursor: %v", err)
	}
	return st
}

// THE SR11-1 PROBE (security review 10.4, re-run with SR11-10): an attacker holding only the runtime role's SQL
// forges BOTH rows - an import shaped like the stream's own (label, mock flag, importing service; unsealed, or with a
// made-up seal, or with a seal copied from a genuine import) and a run row with the same coverage_end - before each
// sweep. With the sealed cursor the stream is NOT blinded (the deposit made after the genuine cursor is imported and
// matched) and the durable P1 cursor_anomaly fires. The same forgery DOES move the A11-mode cursor (control).
func TestF2_SR11_1_ForgedImportAndRunAsRuntime_StreamNotBlinded_AnomalyFires(t *testing.T) {
	for _, variant := range []string{"unsealed", "made_up_seal", "copied_seal"} {
		t.Run(variant, func(t *testing.T) {
			w := newPayWorld(t)
			rt := f2RuntimePool(t)
			keys := f2Keys(t)
			cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: w.srcA, payProvB: w.srcB}, AllowSynthetic: true, RunLineBound: 100, ImportKeys: keys}
			if a := outcomeFor(t, scopedSweep(t, w.pool, w.f, cfg), payProvA); a.Err != nil {
				t.Fatalf("genuine first sweep: %+v", a)
			}
			genuine := f2SealedCursor(t, w, keys, w.srcA)
			if genuine.cursor.IsZero() || genuine.unbacked != 0 || !genuine.sealed {
				t.Fatalf("the genuine run must set a sealed cursor with no anomaly: %+v", genuine)
			}
			var seal, kid *string
			switch variant {
			case "made_up_seal":
				s, k := strings.Repeat("ab", 32), "f2-test"
				seal, kid = &s, &k
			case "copied_seal":
				if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
					return tx.QueryRow(ctx, `SELECT import_seal, seal_kid FROM payment_statement_imports
						WHERE tenant_id = $1 AND provider_id = $2 AND import_seal IS NOT NULL ORDER BY fetched_at DESC LIMIT 1`,
						w.f.tenantID, payProvA).Scan(&seal, &kid)
				}); err != nil || seal == nil {
					t.Fatalf("read a genuine seal to copy: %v", err)
				}
			}

			dep := w.deposit(t, 900) // created AFTER the genuine cursor and BEFORE the forged coverage_end
			forgedEnd := time.Now().UTC().Truncate(time.Microsecond)
			f2Forge(t, rt, w.f.tenantID, payProvA, w.srcA.Label(), true, forgedEnd, seal, kid)

			// Control: the A11 rule (any backing import) WOULD trust the forgery.
			if c := mustCursor(t, w.pool, w.f.tenantID, payProvA); !c.Equal(forgedEnd) {
				t.Fatalf("control: the A11-mode cursor should have moved to the forged %s, got %s", forgedEnd, c)
			}
			// The sealed cursor does not.
			st := f2SealedCursor(t, w, keys, w.srcA)
			if !st.cursor.Equal(genuine.cursor) || st.unbacked != 1 {
				t.Fatalf("the sealed cursor must ignore the forgery: %+v (genuine %s)", st, genuine.cursor)
			}

			for i := 0; i < 2; i++ { // "before each sweep": the forgery is repeated
				if i == 1 {
					f2Forge(t, rt, w.f.tenantID, payProvA, w.srcA.Label(), true, time.Now().UTC().Truncate(time.Microsecond), seal, kid)
				}
				a := outcomeFor(t, scopedSweep(t, w.pool, w.f, cfg), payProvA)
				if a.Err != nil || a.Run.ID == uuid.Nil {
					t.Fatalf("sweep %d still runs: %+v", i, a)
				}
			}
			if refs := importedRefs(t, w.pool, w.f.tenantID, payProvA); refs[*dep.ProviderReference] == 0 {
				t.Fatalf("the deposit made after the genuine cursor must still be imported (stream not blinded), got %v", refs)
			}
			alerts := f2AnomalyAlerts(t, w.pool, w.f.tenantID, payProvA)
			if len(alerts) < 2 {
				t.Fatalf("each new forgery must raise its own durable P1 cursor_anomaly (SR11-11), got %v", alerts)
			}
			if n := countAudit(t, w.pool, w.f.tenantID, "reconciliation.payment_cursor_anomaly", payProvA); n < 2 {
				t.Fatalf("the anomaly must be audited on each sweep that sees it, got %d", n)
			}
		})
	}
}

// SR11-11: the anomaly discriminator carries the unbacked-row count and digest: an UNCHANGED set of unbacked rows
// keeps one alert with growing occurrences; a NEW forgery opens a new alert (pages again) instead of being absorbed.
func TestF2_SR11_11_AnomalyDiscriminatorChangesOnANewForgery(t *testing.T) {
	w := newPayWorld(t)
	rt := f2RuntimePool(t)
	keys := f2Keys(t)
	cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: &recSource{provider: payProvA, fn: contiguousEmpty}, payProvB: w.srcB}, AllowSynthetic: true, ImportKeys: keys}
	scopedSweep(t, w.pool, w.f, cfg)
	src := cfg.Resolver.(mapResolver)[payProvA]
	// A far-future forged row stays "above the cursor" forever (the case SR11-11 describes).
	f2Forge(t, rt, w.f.tenantID, payProvA, src.Label(), true, time.Now().Add(24*time.Hour).UTC(), nil, nil)
	scopedSweep(t, w.pool, w.f, cfg)
	scopedSweep(t, w.pool, w.f, cfg)
	first := f2AnomalyAlerts(t, w.pool, w.f.tenantID, payProvA)
	if len(first) != 1 {
		t.Fatalf("one forgery = one alert, got %v", first)
	}
	var d1 string
	for d, occ := range first {
		d1 = d
		if occ != 2 || !strings.Contains(d, ":cursor_anomaly:n1:") {
			t.Fatalf("an unchanged set re-occurs on ONE alert carrying its count: %s x%d", d, occ)
		}
	}
	f2Forge(t, rt, w.f.tenantID, payProvA, src.Label(), true, time.Now().Add(48*time.Hour).UTC(), nil, nil)
	scopedSweep(t, w.pool, w.f, cfg)
	second := f2AnomalyAlerts(t, w.pool, w.f.tenantID, payProvA)
	if len(second) != 2 || second[d1] != 2 {
		t.Fatalf("a later forgery must open a NEW alert (the old one unchanged), got %v", second)
	}
	for d := range second {
		if d != d1 && !strings.Contains(d, ":cursor_anomaly:n2:") {
			t.Fatalf("the new alert carries the new count: %s", d)
		}
	}
	// The discriminator always fits the alerts CHECK, even for a 100-character provider id.
	long := strings.Repeat("p", 100)
	if d := "stream:payment_statement:provider:" + long + ":" + cursorAnomalyPhase(long, cursorState{unbacked: 123456789, unbackedDigest: "0123456789abcdef"}); len(d) > cursorAnomalyDiscMax {
		t.Fatalf("discriminator too long (%d): %s", len(d), d)
	}
}

// SR11-10: the backing import must be THIS source's (label and mock flag) and written by the stream; a genuine sealed
// import of another source does not back a cursor for this one.
func TestF2_SR11_10_SealedCursorIsPerSourceLabel(t *testing.T) {
	w := newPayWorld(t)
	keys := f2Keys(t)
	src := &recSource{provider: payProvA, fn: contiguousEmpty}
	scopedSweep(t, w.pool, w.f, PaymentSweepConfig{Resolver: mapResolver{payProvA: src, payProvB: w.srcB}, AllowSynthetic: true, ImportKeys: keys})
	if st := f2SealedCursor(t, w, keys, src); st.cursor.IsZero() {
		t.Fatal("the genuine sealed run must set the cursor")
	}
	other := relabelled{recSource: src, label: "MOCK another payment statement (test-only)"}
	if st := f2SealedCursor(t, w, keys, other); !st.cursor.IsZero() || st.unbacked != 0 { // 0168: the genuine run seal verifies, so it is no anomaly; it only cannot back ANOTHER source
		t.Fatalf("another source's sealed import must not back this source's cursor: %+v", st)
	}
	// The same label from a source that is NOT Synthetic (is_mock differs): not this source's import.
	if st := f2SealedCursor(t, w, keys, unmarkedSource{src}); !st.cursor.IsZero() {
		t.Fatalf("an import with another is_mock must not back the cursor: %+v", st)
	}
	// Other keys: nothing verifies.
	if st := f2SealedCursor(t, w, f2Keys(t), src); !st.cursor.IsZero() {
		t.Fatalf("a seal under other keys must not verify: %+v", st)
	}
}

type relabelled struct {
	*recSource
	label string
}

func (r relabelled) Label() string { return r.label }

// unmarkedSource delegates to a recSource WITHOUT its Synthetic marker (same label, is_mock = false).
type unmarkedSource struct{ inner *recSource }

func (u unmarkedSource) Label() string      { return u.inner.Label() }
func (u unmarkedSource) ProviderID() string { return u.inner.ProviderID() }
func (u unmarkedSource) Fetch(ctx context.Context, req statement.PaymentFetchRequest) (statement.PaymentStatement, error) {
	return u.inner.Fetch(ctx, req)
}

// SRF2-4 / LFF2-4: a flood of forged run rows (unsealed, and with made-up seals naming the GENUINE import) can
// neither move the cursor nor stop the stream (no fail-closed state any more: recoverable by design). The sweep keeps
// continuing from the genuine sealed cursor, the anomaly is loud and carries the full count, and a later genuine run
// moves the cursor as usual.
func TestF2_SRF2_4_ForgedRunFloodNeitherMovesNorStopsTheStream(t *testing.T) {
	w := newPayWorld(t)
	rt := f2RuntimePool(t)
	keys := f2Keys(t)
	src := &recSource{provider: payProvA, fn: contiguousEmpty}
	cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: src, payProvB: w.srcB}, AllowSynthetic: true, ImportKeys: keys}
	scopedSweep(t, w.pool, w.f, cfg)
	base := f2SealedCursor(t, w, keys, src)
	if base.cursor.IsZero() || base.cursorRunID == uuid.Nil {
		t.Fatalf("setup: a sealed cursor: %+v", base)
	}
	var genuineImport uuid.UUID
	var genuineStart time.Time
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT run_import_id, period_start FROM reconciliation_runs WHERE id = $1`, base.cursorRunID).Scan(&genuineImport, &genuineStart)
	}); err != nil {
		t.Fatal(err)
	}
	const flood = 2000
	if err := rt.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Half unsealed above the cursor, half with a made-up seal naming the genuine import (at its coverage_end and above).
		if _, err := tx.Exec(ctx, `INSERT INTO reconciliation_runs (tenant_id, stream, period_start, period_end, status, coverage_end, provider_id, window_truncated)
			SELECT $1, 'payment_statement', $2::timestamptz, $2::timestamptz + g * interval '1 microsecond', 'clean', $2::timestamptz + g * interval '1 microsecond', $3, false
			  FROM generate_series(1, $4) g`, w.f.tenantID, base.cursor, payProvA, flood/2); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO reconciliation_runs (tenant_id, stream, period_start, period_end, status, coverage_end, provider_id, window_truncated,
			                                                        run_import_id, run_seal, run_seal_kid)
			SELECT $1, 'payment_statement', $5, $2::timestamptz, 'clean', $2::timestamptz, $3, false, $6, md5(g::text) || md5(g::text), 'f2-test'
			  FROM generate_series(1, $4) g`, w.f.tenantID, base.cursor, payProvA, flood/2, genuineStart, genuineImport)
		return err
	}); err != nil {
		t.Fatalf("flood as runtime: %v", err)
	}
	st := f2SealedCursor(t, w, keys, src)
	if !st.cursor.Equal(base.cursor) || st.cursorRunID != base.cursorRunID {
		t.Fatalf("the flood must not move the cursor: %+v vs %+v", st, base)
	}
	if st.unbacked != flood {
		t.Fatalf("every forged row is counted (count(*), not the bounded read): %d, want %d", st.unbacked, flood)
	}
	a := outcomeFor(t, scopedSweep(t, w.pool, w.f, cfg), payProvA)
	if a.Err != nil || a.Run.ID == uuid.Nil {
		t.Fatalf("the stream keeps working under the flood: %+v", a)
	}
	reqs := src.requests()
	if last := reqs[len(reqs)-1]; !last.ContinueFrom.Equal(base.cursor) {
		t.Fatalf("the sweep continues from the genuine cursor %s, got %s", base.cursor, last.ContinueFrom)
	}
	found := false
	for d := range f2AnomalyAlerts(t, w.pool, w.f.tenantID, payProvA) {
		found = found || strings.Contains(d, ":cursor_anomaly:n"+strconv.Itoa(flood)+":")
	}
	if !found {
		t.Fatal("the flood must be reported loudly with its full count")
	}
	if after := f2SealedCursor(t, w, keys, src); !after.cursor.After(base.cursor) {
		t.Fatalf("the genuine run made under the flood moves the cursor: %s -> %s", base.cursor, after.cursor)
	}
}

// Parity: the reconciliation-side verification accepts and refuses exactly what payoutinstrument's (the M4
// executor's) does, on a sealed import and under other keys.
func TestF2_VerifyImportSealParityWithPayoutInstrument(t *testing.T) {
	w := newPayWorld(t)
	keys, other := f2Keys(t), f2Keys(t)
	src := &recSource{provider: payProvA, fn: func(_ int, req statement.PaymentFetchRequest) (statement.PaymentStatement, error) {
		st, _ := contiguousEmpty(0, req)
		st.Lines = []statement.PaymentStatementLine{payLineFor(payProvA, "f2-par-"+uuid.NewString()[:8], "", statement.PaymentLineDeposit, statement.PaymentStatusPending, 7)}
		st.Lines[0].OccurredAt = st.CoverageStart
		return st, nil
	}}
	scopedSweep(t, w.pool, w.f, PaymentSweepConfig{Resolver: mapResolver{payProvA: src, payProvB: w.srcB}, AllowSynthetic: true, ImportKeys: keys})
	var id uuid.UUID
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM payment_statement_imports WHERE tenant_id = $1 AND provider_id = $2 AND import_seal IS NOT NULL LIMIT 1`, w.f.tenantID, payProvA).Scan(&id)
	}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []*payoutinstrument.Keys{keys, other} {
		if err := w.pool.WithTenantReadOnly(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			mine := verifyImportSealInTx(ctx, tx, k, w.f.tenantID, id)
			theirs := payoutinstrument.VerifyStatementImportInTx(ctx, tx, k, w.f.tenantID, id)
			if (mine == nil) != (theirs == nil) {
				t.Errorf("parity broken: reconciliation=%v payoutinstrument=%v", mine, theirs)
			}
			if (k == keys) != (mine == nil) {
				t.Errorf("keys %p: verification result %v", k, mine)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// SR11-10 through the PROCESS-WIDE keys (production's wiring: cmd/platform-api installs payoutinstrument.Keys with
// SetDefaultImportSealer and passes no ImportKeys): the stream seals with them AND reads a verified cursor with them.
func TestF2_SR11_10_ProcessWideKeysSealAndVerify(t *testing.T) {
	prev := DefaultImportSealer()
	keys := f2Keys(t)
	SetDefaultImportSealer(keys)
	t.Cleanup(func() { SetDefaultImportSealer(prev) })
	w := newPayWorld(t)
	rt := f2RuntimePool(t)
	src := &recSource{provider: payProvA, fn: contiguousEmpty}
	cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: src, payProvB: w.srcB}, AllowSynthetic: true}
	scopedSweep(t, w.pool, w.f, cfg)
	genuine, err := readPaymentCursorState(context.Background(), w.pool, w.f.tenantID, payProvA, cursorBasisFor(src, PaymentStatementOptions{}))
	if err != nil || genuine.cursor.IsZero() || !genuine.sealed {
		t.Fatalf("the process-wide keys must seal the import and verify the cursor: %+v %v", genuine, err)
	}
	f2Forge(t, rt, w.f.tenantID, payProvA, src.Label(), true, time.Now().UTC().Truncate(time.Microsecond), nil, nil)
	scopedSweep(t, w.pool, w.f, cfg)
	reqs := src.requests()
	if last := reqs[len(reqs)-1]; !last.ContinueFrom.Equal(genuine.cursor) {
		t.Fatalf("the sweep must continue from the sealed cursor %s, not the forgery: %s", genuine.cursor, last.ContinueFrom)
	}
}
