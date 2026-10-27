//go:build loadtest

package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/db"
)

// productionPoolConns is internal/config DefaultDatabaseMaxConns (the production default pool size).
const productionPoolConns = 10

// passInterval is cmd/platform-api/main.go's RunScreeningWorkerLoop interval.
var passInterval = 10 * time.Second

func openRuntimePool(ctx context.Context, dbName string, conns int) (*db.Pool, error) {
	u, err := runtimeURL(dbName)
	if err != nil {
		return nil, err
	}
	if conns <= 0 {
		conns = productionPoolConns
	}
	// the production pool sends idle_in_transaction_session_timeout (30 s) on every connection (config.DatabaseRuntimeParams)
	return db.ConnectWithRuntimeParams(ctx, u, int32(conns), 5*time.Second, map[string]string{"idle_in_transaction_session_timeout": "30000"})
}

func newWorker(pool *db.Pool, prov aml.ScreeningProvider, sc *Scenario) (*aml.ScreeningWorker, error) {
	w := aml.NewScreeningWorker(pool, prov, aml.MockSubjectKeys{})
	if sc.ClaimBatch > 0 {
		w.Config.ClaimBatch = sc.ClaimBatch
	}
	if v := intExtra(sc, "enqueue_batch", 0); v > 0 {
		w.Config.EnqueueBatch = v
	}
	if v := intExtra(sc, "lease_secs", 0); v > 0 {
		w.Config.Lease = time.Duration(v) * time.Second
	}
	if v := intExtra(sc, "backoff_base_secs", 0); v > 0 {
		w.Config.BackoffBase = time.Duration(v) * time.Second
	}
	return w, w.ValidateForLoop()
}

type passRec struct {
	Index          int           `json:"index"`
	StartOffsetS   float64       `json:"start_offset_s"`
	DurationS      float64       `json:"duration_s"`
	Truncated      bool          `json:"truncated_by_cap"`
	Stats          aml.PassStats `json:"stats"`
	ProviderCalls  int64         `json:"provider_calls"`
	OverInterval   bool          `json:"longer_than_10s_interval"`
	TenantsStarted int           `json:"tenants_started"`
}

// runLoop mimics aml.RunScreeningWorkerLoop exactly (a time.Ticker of passInterval; a pass longer than the interval is followed by an
// immediate pass because the ticker has a buffered tick), for `passes` passes or until `window` elapses. Each pass is capped at `cap`.
func runLoop(ctx context.Context, w *aml.ScreeningWorker, prov *LatencyProvider, passes int, window, passCap time.Duration) []passRec {
	var recs []passRec
	t0 := time.Now()
	tk := time.NewTicker(passInterval)
	defer tk.Stop()
	one := func(i int) {
		pctx, cancel := context.WithTimeout(ctx, passCap)
		defer cancel()
		c0, _, _ := prov.Counts()
		s := time.Now()
		st := w.RunPass(pctx)
		d := time.Since(s)
		c1, _, _ := prov.Counts()
		recs = append(recs, passRec{Index: i, StartOffsetS: s.Sub(t0).Seconds(), DurationS: d.Seconds(), Truncated: pctx.Err() != nil, Stats: st,
			ProviderCalls: c1 - c0, OverInterval: d > passInterval, TenantsStarted: st.Tenants})
	}
	one(1)
	for i := 2; (passes == 0 || i <= passes) && time.Since(t0) < window; i++ {
		select {
		case <-ctx.Done():
			return recs
		case <-tk.C:
		}
		if time.Since(t0) >= window {
			break
		}
		one(i)
	}
	return recs
}

func poolSampler(ctx context.Context, p *db.Pool) (stop func() map[string]any) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	maxAcq, maxTotal := int32(0), int32(0)
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				s := p.Raw().Stat()
				if s.AcquiredConns() > maxAcq {
					maxAcq = s.AcquiredConns()
				}
				if s.TotalConns() > maxTotal {
					maxTotal = s.TotalConns()
				}
			}
		}
	}()
	return func() map[string]any {
		close(done)
		wg.Wait()
		s := p.Raw().Stat()
		return map[string]any{"max_conns_config": s.MaxConns(), "max_acquired_sampled_100ms": maxAcq, "max_total_conns": maxTotal,
			"acquire_count": s.AcquireCount(), "empty_acquire_count(waited for a connection)": s.EmptyAcquireCount(),
			"canceled_acquire_count": s.CanceledAcquireCount(), "total_acquire_wait_seconds": s.AcquireDuration().Seconds()}
	}
}

// benchRunPass is measurement (c)/(i): the real ScreeningWorker.RunPass with the MOCK LatencyProvider.
func benchRunPass(ctx context.Context, dbName string, sc *Scenario, run int) (any, error) {
	prov := NewLatencyProvider(sc.Provider, sc.Seed+int64(run))
	pool, err := openRuntimePool(ctx, dbName, sc.PoolConns)
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
	tenants, err := listTenants(ctx, oc)
	if err != nil {
		return nil, err
	}
	var bg *bgLoad
	if sc.BgGate+sc.BgSweeper > 0 {
		big := tenants[0]
		for _, t := range tenants {
			if t.Persons > big.Persons {
				big = t
			}
		}
		ps, err := samplePersons(ctx, oc, big.ID, 40, sc.Seed)
		if err != nil {
			return nil, err
		}
		if intExtra(sc, "shared_pool", 0) == 1 {
			// production shape: gate callers share the worker's 10-connection pool
			bg = startBgWith(ctx, func(c context.Context, tid uuid.UUID, fn func(pgx.Tx) error) error {
				return pool.WithTenant(c, tid, func(c2 context.Context, tx pgx.Tx) error { return fn(tx) })
			}, big.ID, ps, sc.BgGate, sc.BgSweeper)
		} else if bg, err = startBg(ctx, dbName, big.ID, ps, sc.BgGate, sc.BgSweeper); err != nil {
			return nil, err
		}
	}
	pr, err := StartProbe(ctx, dbName, 500*time.Millisecond)
	if err != nil {
		return nil, err
	}
	stopPool := poolSampler(ctx, pool)
	window := time.Duration(sc.WindowSecs) * time.Second
	if window == 0 {
		window = 24 * time.Hour
	}
	passCap := time.Duration(intExtra(sc, "pass_cap_secs", 900)) * time.Second
	start := time.Now()
	var all [][]passRec
	var mu sync.Mutex
	var wg sync.WaitGroup
	nw := sc.Workers
	if nw < 1 {
		nw = 1
	}
	for i := 0; i < nw; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			wp, wk := pool, w
			if i > 0 { // further workers = further replicas, each with its own pool
				p2, err := openRuntimePool(ctx, dbName, sc.PoolConns)
				if err != nil {
					return
				}
				defer p2.Close()
				wk, err = newWorker(p2, prov, sc)
				if err != nil {
					return
				}
				wp = p2
			}
			_ = wp
			r := runLoop(ctx, wk, prov, sc.Passes, window, passCap)
			mu.Lock()
			all = append(all, r)
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)
	poolStats := stopPool()
	probe, perr := pr.Stop(ctx)
	if perr != nil {
		return nil, perr
	}
	var bgst any
	if bg != nil {
		bgst = bg.stop()
	}
	// aggregate
	var claimed, done, errs, retried, term, lost, trapped, enq, parked int
	var durs []time.Duration
	complete, truncated := 0, 0
	for _, rs := range all {
		for _, r := range rs {
			claimed += r.Stats.Claimed
			done += r.Stats.Done
			errs += r.Stats.Errors
			retried += r.Stats.Retried
			term += r.Stats.Terminal
			lost += r.Stats.Lost
			trapped += r.Stats.Trapped
			enq += r.Stats.Enqueued
			parked += r.Stats.ParkedScreening
			if r.Truncated {
				truncated++
			} else {
				complete++
				durs = append(durs, time.Duration(r.DurationS*float64(time.Second)))
			}
		}
	}
	nTen := len(tenants)
	// Rate window: from the first pass start to the END of the last pass's slot, i.e. last start + max(last duration, 10 s interval)
	// (a loop of N passes spans N slots; using the wall time of the last pass alone would overstate the rate).
	slot := 0.0
	for _, rs := range all {
		if n := len(rs); n > 0 {
			end := rs[n-1].StartOffsetS + maxF(rs[n-1].DurationS, passInterval.Seconds())
			if end > slot {
				slot = end
			}
		}
	}
	perHourPerTenant := 0.0
	if slot > 0 && nTen > 0 {
		perHourPerTenant = float64(claimed) / (slot / 3600) / float64(nTen)
	}
	return map[string]any{
		"tenants": nTen, "tenant_persons": tenantPersons(tenants), "workers": nw, "pool_conns": productionOrCfg(sc.PoolConns),
		"claim_batch": claimBatch(w), "elapsed_s": elapsed.Seconds(), "passes_complete": complete, "passes_truncated_by_cap": truncated,
		"pass_duration_complete": summarize(durs), "claims_total": claimed, "done": done, "retried": retried, "terminal": term, "lost": lost,
		"errors": errs, "enqueued": enq, "trapped_sum": trapped, "parked_sum": parked,
		"rate_window_s": slot, "claims_per_hour_per_tenant_measured": perHourPerTenant, "claims_per_hour_per_tenant_upper_bound_config": 3600.0 / 10 * float64(claimBatch(w)),
		"provider_latency": summarize(prov.Latencies()), "passes": all, "pool": poolStats, "probe": probe, "background": bgst,
		"provider_calls_fails_hangs": func() []int64 { a, b, c := prov.Counts(); return []int64{a, b, c} }(),
	}, nil
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func claimBatch(w *aml.ScreeningWorker) int { return w.Config.ClaimBatch }
func productionOrCfg(n int) int {
	if n <= 0 {
		return productionPoolConns
	}
	return n
}
func tenantPersons(ts []tenantInfo) any {
	m := map[int]int{}
	for _, t := range ts {
		m[t.Persons]++
	}
	return m
}

var _ = fmt.Sprint
