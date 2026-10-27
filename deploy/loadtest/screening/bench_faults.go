//go:build loadtest

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func statusCounts(ctx context.Context, c *pgx.Conn, tid uuid.UUID) (map[string]int, error) {
	if err := setTenant(ctx, c, tid); err != nil {
		return nil, err
	}
	rows, err := c.Query(ctx, `SELECT status, count(*) FROM aml_screening_requests WHERE tenant_id=$1 GROUP BY 1`, tid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}

// deniedCount evaluates the gate for every person of the tenant (a full pass of aml_screening_gate_denies; not a production call).
func deniedCount(ctx context.Context, c *pgx.Conn, tid uuid.UUID) (int, time.Duration, error) {
	if err := setTenant(ctx, c, tid); err != nil {
		return 0, 0, err
	}
	t0 := time.Now()
	var n int
	err := c.QueryRow(ctx, `SELECT count(*) FROM (SELECT DISTINCT person_id FROM player_accounts WHERE tenant_id=$1) p WHERE aml_screening_gate_denies($1, p.person_id, 'withdrawal')`, tid).Scan(&n)
	return n, time.Since(t0), err
}

// insertPolicyVersion adds a new screening policy version for every jurisdiction of the tenant through the OWNER path (the actor-proof
// guard trigger is switched off around the insert on the private scratch database, then restored). This is how the harness emulates
// "a list was added" / "the interval was shortened" without the officer ceremony; the version is otherwise an ordinary row.
func insertPolicyVersion(ctx context.Context, c *pgx.Conn, tid uuid.UUID, lists []string, interval int64) (int64, error) {
	if err := setTenant(ctx, c, tid); err != nil {
		return 0, err
	}
	if _, err := c.Exec(ctx, `ALTER TABLE aml_screening_policies DISABLE TRIGGER aml_screening_policies_10_guard`); err != nil {
		return 0, err
	}
	var n int64
	tag, err := c.Exec(ctx, `INSERT INTO aml_screening_policies (tenant_id,licensing_jurisdiction_id,lists,rescreen_interval_seconds,backfill_grace_seconds,effective_from,
		legal_review_reference,reason_code,created_by,created_by_person_id,created_by_scope,created_at)
		SELECT DISTINCT ON (licensing_jurisdiction_id) tenant_id, licensing_jurisdiction_id, $2::text[], $3, backfill_grace_seconds, clock_timestamp(),
		       'CAPA SYNTHETIC - NOT LEGAL','capa_event',created_by,created_by_person_id,created_by_scope, clock_timestamp()
		  FROM aml_screening_policies WHERE tenant_id=$1 ORDER BY licensing_jurisdiction_id, created_at DESC, id DESC`, tid, lists, interval)
	if err == nil {
		n = tag.RowsAffected()
	}
	if _, e2 := c.Exec(ctx, `ALTER TABLE aml_screening_policies ENABLE TRIGGER aml_screening_policies_10_guard`); e2 != nil && err == nil {
		err = e2
	}
	return n, err
}

func pollWait(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// benchStale is measurement (d): a mass-stale event on a mostly-clear tenant (extra.event = list_added | interval_shortened), then the
// real worker drains the backlog; optional concurrent withdrawal gate callers (bg_gate).
func benchStale(ctx context.Context, dbName string, sc *Scenario, run int) (any, error) {
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
	res := map[string]any{"tenant_persons": tenants[0].Persons}
	before, dur0, err := deniedCount(ctx, oc, tid)
	if err != nil {
		return nil, err
	}
	res["denied_before_event"] = before
	res["denied_eval_seconds_full_tenant"] = dur0.Seconds()
	persons, err := samplePersons(ctx, oc, tid, 100, sc.Seed)
	if err != nil {
		return nil, err
	}
	// the event
	evt := "interval_shortened"
	if v, ok := sc.Extra["event"].(string); ok {
		evt = v
	}
	if err := setTenant(ctx, oc, tid); err != nil {
		return nil, err
	}
	var lists []string
	var interval int64
	if err := oc.QueryRow(ctx, `SELECT lists, rescreen_interval_seconds FROM aml_screening_policies WHERE tenant_id=$1 ORDER BY created_at DESC, id DESC LIMIT 1`, tid).Scan(&lists, &interval); err != nil {
		return nil, err
	}
	switch evt {
	case "list_added":
		lists = []string{"sanctions", "pep"}
	default:
		f := int64(intExtra(sc, "shorten_factor", 20))
		interval = interval / f
	}
	tEvent := time.Now()
	nv, err := insertPolicyVersion(ctx, oc, tid, lists, interval)
	if err != nil {
		return nil, err
	}
	res["event"] = map[string]any{"kind": evt, "versions_inserted": nv, "new_interval_s": interval, "new_lists": lists}
	after, dur1, err := deniedCount(ctx, oc, tid)
	if err != nil {
		return nil, err
	}
	res["backlog_denied_after_event"] = after
	res["denied_eval_seconds_after_event"] = dur1.Seconds()
	st0, _ := statusCounts(ctx, oc, tid)

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
	var bg *bgLoad
	tBg := time.Now()
	if sc.BgGate > 0 {
		if bg, err = startBg(ctx, dbName, tid, persons, sc.BgGate, 0); err != nil {
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
		window = 5 * time.Minute
	}
	wctx, cancel := context.WithTimeout(ctx, window)
	type sample struct {
		T          float64        `json:"t_s"`
		Status     map[string]int `json:"requests_by_status"`
		PassesDone int            `json:"passes"`
	}
	var timeline []sample
	recsCh := make(chan []passRec, 1)
	passCap := time.Duration(intExtra(sc, "pass_cap_secs", 900)) * time.Second
	tStart := time.Now()
	go func() { recsCh <- runLoop(wctx, w, prov, 0, window, passCap) }()
	sc2, _ := openOwner(ctx, dbName)
	defer sc2.Close(ctx)
	cleared := -1.0
	var recs []passRec
loop:
	for {
		select {
		case recs = <-recsCh:
			break loop
		case <-time.After(10 * time.Second):
		}
		m, err := statusCounts(ctx, sc2, tid)
		if err == nil {
			timeline = append(timeline, sample{T: time.Since(tStart).Seconds(), Status: m})
			// the backlog is cleared when every denied person has been served: done(new) >= backlog (approximate; verified exactly below)
			if cleared < 0 && m["done"]-st0["done"] >= after {
				cleared = time.Since(tStart).Seconds()
				if sc.Extra["stop_when_cleared"] == true {
					cancel()
				}
			}
		}
	}
	cancel()
	elapsed := time.Since(tStart)
	poolStats := stopPool()
	probe, perr := pr.Stop(ctx)
	if perr != nil {
		return nil, perr
	}
	var bgst any
	var tl any
	if bg != nil {
		bgst = bg.stop()
		tl = bg.timeline(tBg, 30*time.Second)
	}
	final, dur2, err := deniedCount(ctx, oc, tid)
	if err != nil {
		return nil, err
	}
	stN, _ := statusCounts(ctx, oc, tid)
	doneNew := stN["done"] - st0["done"]
	res["window_s"] = elapsed.Seconds()
	res["worker_cfg"] = map[string]any{"claim_batch": w.Config.ClaimBatch, "lease_s": w.Config.Lease.Seconds(), "enqueue_batch": w.Config.EnqueueBatch, "workers": 1}
	res["done_new"] = doneNew
	res["drain_rate_per_minute_measured"] = float64(doneNew) / elapsed.Minutes()
	res["denied_final_exact"] = final
	res["denied_eval_seconds_final"] = dur2.Seconds()
	res["event_to_end_s"] = time.Since(tEvent).Seconds()
	if final == 0 {
		res["time_to_clear_s_measured"] = cleared
	} else if doneNew > 0 {
		res["time_to_clear_s_EXTRAPOLATED_linear"] = float64(final) / (float64(doneNew) / elapsed.Seconds())
	}
	res["status_before"], res["status_after"], res["timeline"] = st0, stN, timeline
	pd := []time.Duration{}
	trunc := 0
	for _, r := range recs {
		pd = append(pd, time.Duration(r.DurationS*float64(time.Second)))
		if r.Truncated {
			trunc++
		}
	}
	res["passes"], res["pass_duration"], res["passes_truncated"] = recs, summarize(pd), trunc
	res["pool"], res["probe"], res["gate_callers"], res["gate_timeline_30s"] = poolStats, probe, bgst, tl
	res["provider_latency"] = summarize(prov.Latencies())
	return res, nil
}

// benchRetry is measurement (e): provider failing 100% for `outage_secs`, then recovering; a fraction of subjects (fail_persist)
// fails on every attempt. Watches attempts, backoff, terminal failures, queue starvation and the alerts raised.
func benchRetry(ctx context.Context, dbName string, sc *Scenario, run int) (any, error) {
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
	outage := time.Duration(intExtra(sc, "outage_secs", 240)) * time.Second
	pcfg := sc.Provider
	storm := pcfg
	storm.FailRate = 1.0
	prov := NewLatencyProvider(storm, sc.Seed+int64(run))
	pool, err := openRuntimePool(ctx, dbName, sc.PoolConns)
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	w, err := newWorker(pool, prov, sc)
	if err != nil {
		return nil, err
	}
	window := time.Duration(sc.WindowSecs) * time.Second
	wctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	st0, _ := statusCounts(ctx, oc, tid)
	denied0, _, _ := deniedCount(ctx, oc, tid)
	pr, err := StartProbe(ctx, dbName, 500*time.Millisecond)
	if err != nil {
		return nil, err
	}
	recsCh := make(chan []passRec, 1)
	passCap := 15 * time.Minute
	tStart := time.Now()
	go func() { recsCh <- runLoop(wctx, w, prov, 0, window, passCap) }()
	sc2, _ := openOwner(ctx, dbName)
	defer sc2.Close(ctx)
	type sample struct {
		T        float64        `json:"t_s"`
		Phase    string         `json:"phase"`
		Status   map[string]int `json:"requests_by_status"`
		MaxAtt   int            `json:"max_attempts_pending_or_claimed"`
		Attempts map[int]int    `json:"requests_by_attempts"`
		Alerts   map[string]int `json:"alerts_by_kind"`
	}
	var tl []sample
	recovered := false
	var recs []passRec
loop:
	for {
		select {
		case recs = <-recsCh:
			break loop
		case <-time.After(10 * time.Second):
		}
		el := time.Since(tStart)
		if !recovered && el >= outage {
			c := pcfg
			c.FailRate = 0
			prov.SetConfig(c)
			recovered = true
		}
		m, err := statusCounts(ctx, sc2, tid)
		if err != nil {
			continue
		}
		s := sample{T: el.Seconds(), Phase: map[bool]string{false: "outage", true: "recovered"}[recovered], Status: m, Attempts: map[int]int{}, Alerts: map[string]int{}}
		rows, e := sc2.Query(ctx, `SELECT attempts, status, count(*) FROM aml_screening_requests WHERE tenant_id=$1 GROUP BY 1,2`, tid)
		if e == nil {
			for rows.Next() {
				var a, n int
				var st string
				if rows.Scan(&a, &st, &n) == nil {
					s.Attempts[a] += n
					if (st == "pending" || st == "claimed") && a > s.MaxAtt {
						s.MaxAtt = a
					}
				}
			}
			rows.Close()
		}
		if _, e := sc2.Exec(ctx, `SELECT set_config('app.tenant_id','',false)`); e == nil {
			if rows, e := sc2.Query(ctx, `SELECT kind, count(*) FROM alerts WHERE subject_tenant_id=$1 GROUP BY 1`, tid); e == nil {
				for rows.Next() {
					var k string
					var n int
					if rows.Scan(&k, &n) == nil {
						s.Alerts[k] = n
					}
				}
				rows.Close()
			}
		}
		tl = append(tl, s)
	}
	cancel()
	probe, perr := pr.Stop(ctx)
	if perr != nil {
		return nil, perr
	}
	stN, _ := statusCounts(ctx, oc, tid)
	deniedN, _, _ := deniedCount(ctx, oc, tid)
	// after the run: are the persons whose request ended failed_terminal re-queued by the scheduled enqueue? (the key is per bucket)
	var terminalPersons, terminalWithNewRequest, reEnq int
	_ = setTenant(ctx, oc, tid)
	_ = oc.QueryRow(ctx, `SELECT count(DISTINCT person_id) FROM aml_screening_requests WHERE tenant_id=$1 AND status='failed_terminal'`, tid).Scan(&terminalPersons)
	_ = oc.QueryRow(ctx, `SELECT count(*) FROM (SELECT person_id FROM aml_screening_requests WHERE tenant_id=$1 GROUP BY person_id HAVING bool_or(status='failed_terminal') AND count(*) FILTER (WHERE status IN ('pending','claimed')) > 0) q`, tid).Scan(&terminalWithNewRequest)
	_, _ = timeIn(ctx, oc, tid, true, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT aml_screening_enqueue_due($1,1000)`, tid).Scan(&reEnq)
	})
	var lastErr []string
	rows, e := oc.Query(ctx, `SELECT last_error_code, count(*) FROM aml_screening_requests WHERE tenant_id=$1 AND last_error_code IS NOT NULL GROUP BY 1`, tid)
	if e == nil {
		for rows.Next() {
			var k string
			var n int
			if rows.Scan(&k, &n) == nil {
				lastErr = append(lastErr, fmt.Sprintf("%s=%d", k, n))
			}
		}
		rows.Close()
	}
	c, f, h := prov.Counts()
	var pd []time.Duration
	for _, r := range recs {
		pd = append(pd, time.Duration(r.DurationS*float64(time.Second)))
	}
	return map[string]any{"outage_secs": outage.Seconds(), "worker_cfg": map[string]any{"max_attempts": w.Config.MaxAttempts, "backoff_base_s": w.Config.BackoffBase.Seconds(),
		"backoff_cap_s": w.Config.BackoffCap.Seconds()}, "status_before": st0, "status_after": stN, "denied_before": denied0, "denied_after": deniedN,
		"persons_with_failed_terminal": terminalPersons, "terminal_persons_with_a_new_open_request": terminalWithNewRequest,
		"enqueue_due_would_enqueue_now(rolled back)": reEnq, "last_error_codes": lastErr, "timeline": tl, "passes": recs, "pass_duration": summarize(pd),
		"provider_calls_fails_hangs": []int64{c, f, h}, "probe": probe}, nil
}
