//go:build integration

package payments

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SRF2R-1 (M2 gate side): the evidence read streams the provider's sealed-looking run rows through ONE server-side
// cursor and scans at most evidenceScanBudget of them; past the budget the gate REFUSES
// (evidence_scan_budget_exceeded), it never admits on partial evidence. Request pre-reads only the attempt's provider
// (SRF2R-1 (c)); a provider not read is never current.

// srf2rGateFlood plants n run rows copied from the provider's newest sealed run with run_at = now() and a made-up
// seal (a new id breaks the seal in any case): all newer than the genuine run, none verifying.
func srf2rGateFlood(w *k3World, n int) {
	w.t.Helper()
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO reconciliation_runs (tenant_id, stream, period_start, period_end, status, coverage_end, provider_id, window_truncated,
			run_import_id, run_seal, run_seal_kid)
			SELECT r.tenant_id, r.stream, r.period_start, r.period_end, r.status, r.coverage_end, r.provider_id, false,
			       r.run_import_id, md5(g::text) || md5(g::text || 'x'), r.run_seal_kid
			  FROM (SELECT * FROM reconciliation_runs WHERE tenant_id = $1 AND provider_id = $2 AND run_seal IS NOT NULL ORDER BY run_at DESC LIMIT 1) r,
			       generate_series(1, $3) g`, w.f.tenantID, w.provider, n)
		return err
	})
}

func srf2rGateBudget(t *testing.T, n int) {
	t.Helper()
	prev := evidenceScanBudget
	evidenceScanBudget = n
	t.Cleanup(func() { evidenceScanBudget = prev })
}

func TestSRF2R1_M2GateStreamsTheFloodAndRefusesPastTheBudget(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1, noStatementRun: true})
	keys := f2eKeys(t)
	w.svc = w.svc.WithImportSealKeys(keys)
	_, a := w.ambiguousPayout(500)
	k3StatementRun(t, w.pool, w.f.tenantID, w.mock, keys)
	srf2rGateFlood(w, 3*evidenceRunsPerPage+11)

	// Inside the budget: the genuine sealed run is found behind the flood (several fetch batches), M2 admitted.
	ev := w.svc.preReadStatementEvidence(context.Background(), w.f.tenantID, a.ID)
	if ev.err != nil || len(ev.reasons) != 1 || ev.reasons[w.provider] != "" {
		t.Fatalf("narrowed pre-read inside the budget: %+v", ev)
	}
	// Concurrent pre-reads agree (each its own transaction and server-side cursor).
	var wg sync.WaitGroup
	results := make([]*reconEvidence, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = w.svc.preReadStatementEvidence(context.Background(), w.f.tenantID, a.ID)
		}(i)
	}
	wg.Wait()
	for i, r := range results {
		if r.err != nil || r.reasons[w.provider] != "" {
			t.Fatalf("concurrent pre-read %d: %+v", i, r)
		}
	}
	if _, err := f2eRequestM2(w, a.ID, ResolutionM2DeclareNotPaid); err != nil {
		t.Fatalf("M2 inside the budget must be admitted: %v", err)
	}

	// Past the budget: refused with the closed reason, at the pre-read and at the request (retry: same answer).
	srf2rGateBudget(t, 600)
	ev = w.svc.preReadStatementEvidence(context.Background(), w.f.tenantID, a.ID)
	if ev.err != nil || ev.reasons[w.provider] != reconEvidenceScanBudget {
		t.Fatalf("past the budget the reason is %q: %+v", reconEvidenceScanBudget, ev)
	}
	_, b := w.ambiguousPayout(500)
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := f2eRequestM2(w, b.ID, ResolutionM2DeclarePaid); !errors.Is(err, ErrResolutionReconciliationNotCurrent) {
			t.Fatalf("attempt %d: past the budget M2 must be refused (fail closed), got %v", attempt, err)
		}
	}
	evidenceScanBudget = EvidenceScanBudget
	if _, err := f2eRequestM2(w, b.ID, ResolutionM2DeclarePaid); err != nil {
		t.Fatalf("with the budget restored the same request is admitted: %v", err)
	}
}

// SRF2R-1 (c): Request reads only the attempt's provider; Decide (uuid.Nil: the pending resolution is invisible to
// the system session) reads every provider of the tenant. An unknown attempt reads nothing and is never current.
func TestSRF2R1_M2PreReadIsNarrowedToTheAttemptsProvider(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(500)
	w.ensureStatementRun()
	all := w.svc.preReadStatementEvidence(context.Background(), w.f.tenantID, uuid.Nil)
	one := w.svc.preReadStatementEvidence(context.Background(), w.f.tenantID, a.ID)
	if all.err != nil || one.err != nil || len(one.reasons) != 1 || one.reasons[w.provider] != "" || len(all.reasons) < 1 || all.reasons[w.provider] != "" {
		t.Fatalf("narrowed %+v, all %+v", one, all)
	}
	none := w.svc.preReadStatementEvidence(context.Background(), w.f.tenantID, uuid.New())
	if none.err != nil || len(none.reasons) != 0 {
		t.Fatalf("an unknown attempt reads nothing: %+v", none)
	}
	p := w.provider
	if reason, err := statementEvidenceRefusal(withReconEvidence(context.Background(), none), &p); err != nil || reason != reconEvidenceNoRun {
		t.Fatalf("a provider not read is never current: %q %v", reason, err)
	}
}
