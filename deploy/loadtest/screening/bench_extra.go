//go:build loadtest

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/payments"
)

// benchSweep (review H2) runs the REAL payments.Sweeper.RunPass (nil orchestrator / gates: there are no payment attempts in the scratch
// database, so only the per-tenant overhead is measured, including the screening-parked keyset read of 500 persons per tenant) and
// records the pass duration against the 15 s sweeper tick.
func benchSweep(ctx context.Context, dbName string, sc *Scenario, run int) (any, error) {
	pool, err := openRuntimePool(ctx, dbName, sc.PoolConns)
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	sw := payments.NewSweeper(pool, nil, nil, nil)
	pr, err := StartProbe(ctx, dbName, 500*time.Millisecond)
	if err != nil {
		return nil, err
	}
	n := sc.Passes
	if n == 0 {
		n = 3
	}
	capD := time.Duration(intExtra(sc, "pass_cap_secs", 600)) * time.Second
	type rec struct {
		Pass                                       int
		DurationS                                  float64
		Tenants, Scanned, Parked, Errors, Failures int
		Partial, Capped                            bool
	}
	var recs []rec
	var ds []time.Duration
	for i := 0; i < n; i++ {
		pctx, cancel := context.WithTimeout(ctx, capD)
		t0 := time.Now()
		st := sw.RunPass(pctx, nil, i)
		d := time.Since(t0)
		capped := pctx.Err() != nil
		cancel()
		recs = append(recs, rec{i + 1, d.Seconds(), st.Tenants, st.ScreeningParkedScanned, st.ScreeningParked, st.Errors, st.ScreeningParkedReadFailures, st.ScreeningParkedPartial, capped})
		if !capped {
			ds = append(ds, d)
		}
	}
	probe, err := pr.Stop(ctx)
	if err != nil {
		return nil, err
	}
	oc, _ := openOwner(ctx, dbName)
	defer oc.Close(ctx)
	ts, _ := listTenants(ctx, oc)
	return map[string]any{"tenants": len(ts), "tenant_persons": tenantPersons(ts), "passes": recs, "pass_duration_complete": summarize(ds), "sweeper_tick_s": 15, "probe": probe}, nil
}

// benchTenantCap (review H1): 501+ tenants, one real RunPass loop; which tenants (in id order) were served?
func benchTenantCap(ctx context.Context, dbName string, sc *Scenario, run int) (any, error) {
	prov := NewLatencyProvider(sc.Provider, sc.Seed)
	pool, err := openRuntimePool(ctx, dbName, 0)
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	w, err := newWorker(pool, prov, sc)
	if err != nil {
		return nil, err
	}
	oc, err := openOwner(ctx, dbName)
	if err != nil {
		return nil, err
	}
	defer oc.Close(ctx)
	start := time.Now()
	recs := runLoop(ctx, w, prov, sc.Passes, 24*time.Hour, 900*time.Second)
	rows, err := oc.Query(ctx, `SELECT id FROM tenants WHERE status='active' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	served, firstUnserved := 0, -1
	var unservedPending int
	perPos := []int{}
	for i, id := range ids {
		if err := setTenant(ctx, oc, id); err != nil {
			return nil, err
		}
		var n, pend int
		oc.QueryRow(ctx, `SELECT count(*) FILTER (WHERE updated_at > $2 AND attempts >= 1 AND status IN ('claimed','done','needs_officer','pending')), count(*) FILTER (WHERE status='pending')
			FROM aml_screening_requests WHERE tenant_id=$1`, id, start).Scan(&n, &pend)
		if n > 0 {
			served++
			perPos = append(perPos, i+1)
		} else {
			if firstUnserved < 0 {
				firstUnserved = i + 1
			}
			unservedPending += pend
		}
	}
	lastServed := 0
	if len(perPos) > 0 {
		lastServed = perPos[len(perPos)-1]
	}
	var ds []time.Duration
	for _, r := range recs {
		ds = append(ds, time.Duration(r.DurationS*float64(time.Second)))
	}
	return map[string]any{"tenants_active": len(ids), "tenant_cap": w.Config.TenantCap, "tenants_with_work_done": served, "last_served_position_by_id_order": lastServed,
		"first_unserved_position": firstUnserved, "pending_requests_of_unserved_tenants": unservedPending, "pass_stats": recs, "pass_duration": summarize(ds)}, nil
}

// benchReplay (ledger M4): a provider that replays the FIRST answer (incl. ScreenedAt) for an idempotency key; a claim whose finish is
// lost (worker died after the provider answered) is reclaimed after the lease; the replayed screened_at is older than the finish
// function's 120 s tolerance, so the finish is refused on every reclaim.
func benchReplay(ctx context.Context, dbName string, sc *Scenario, run int) (any, error) {
	oc, err := openOwner(ctx, dbName)
	if err != nil {
		return nil, err
	}
	defer oc.Close(ctx)
	tenants, err := listTenants(ctx, oc)
	if err != nil {
		return nil, err
	}
	tid := tenants[0].ID
	cfg := sc.Provider
	cfg.Cache = true
	prov := NewLatencyProvider(cfg, sc.Seed)
	if _, err := oc.Exec(ctx, `SELECT screening_worker_heartbeat('mock-screening', true)`); err != nil {
		return nil, err
	}
	if err := setTenant(ctx, oc, tid); err != nil {
		return nil, err
	}
	var n int
	if err := oc.QueryRow(ctx, `SELECT aml_screening_enqueue_due($1, 100)`, tid).Scan(&n); err != nil {
		return nil, err
	}
	lease := intExtra(sc, "lease_secs", 125)
	rows, err := oc.Query(ctx, `SELECT o_id, o_idempotency_key FROM aml_screening_request_claim($1, 4, $2)`, tid, lease)
	if err != nil {
		return nil, err
	}
	type cr struct {
		id  uuid.UUID
		key string
	}
	var held []cr
	for rows.Next() {
		var c cr
		rows.Scan(&c.id, &c.key)
		held = append(held, c)
	}
	rows.Close()
	for _, h := range held { // the provider answers (and caches), the finish is lost
		_, err := prov.Screen(ctx, aml.ScreeningRequest{IdempotencyKey: h.key, SubjectRef: "00000000000000000000000000000000000000000000000000000000000000aa",
			Lists: []aml.ListType{aml.ListSanctions, aml.ListPEP}})
		if err != nil {
			return nil, fmt.Errorf("provider: %v", err)
		}
	}
	tLost := time.Now()
	pool, err := openRuntimePool(ctx, dbName, 0)
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	w, err := newWorker(pool, prov, sc)
	if err != nil {
		return nil, err
	}
	type cyc struct {
		Cycle            int
		SecondsSinceLoss float64
		PassErrors       int
		Claimed          int
		Done             int
		ReqAttempts      []int
		ReqStatus        []string
	}
	var cycles []cyc
	for c := 1; c <= 2; c++ {
		time.Sleep(time.Until(tLost.Add(time.Duration(c*(lease+6)) * time.Second)))
		st := w.RunPass(ctx)
		cy := cyc{Cycle: c, SecondsSinceLoss: time.Since(tLost).Seconds(), PassErrors: st.Errors, Claimed: st.Claimed, Done: st.Done}
		for _, h := range held {
			var a int
			var s string
			oc.QueryRow(ctx, `SELECT attempts, status FROM aml_screening_requests WHERE id=$1`, h.id).Scan(&a, &s)
			cy.ReqAttempts = append(cy.ReqAttempts, a)
			cy.ReqStatus = append(cy.ReqStatus, s)
		}
		cycles = append(cycles, cy)
	}
	return map[string]any{"lease_s": lease, "held_requests": len(held), "cycles": cycles, "note": "Done=0 and Errors>0 in each cycle means the replayed screened_at (older than the 120 s tolerance) was refused by aml_screening_request_finish"}, nil
}

var _ pgx.Tx
