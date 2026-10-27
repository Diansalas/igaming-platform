//go:build loadtest

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type timed struct {
	Stat  Stat           `json:"latency"`
	Probe ProbeResult    `json:"probe"`
	Notes map[string]any `json:"notes,omitempty"`
}

// withProbe runs fn under a probe on a private database.
func withProbe(ctx context.Context, db string, fn func() ([]time.Duration, map[string]any, error)) (timed, error) {
	p, err := StartProbe(ctx, db, 500*time.Millisecond)
	if err != nil {
		return timed{}, err
	}
	ds, notes, ferr := fn()
	pr, perr := p.Stop(ctx)
	if ferr != nil {
		return timed{}, ferr
	}
	if perr != nil {
		return timed{}, perr
	}
	return timed{Stat: summarize(ds), Probe: pr, Notes: notes}, nil
}

func claimOne(ctx context.Context, tx pgx.Tx, tid uuid.UUID, n int) (ids, tokens, policies []uuid.UUID, d time.Duration, err error) {
	t0 := time.Now()
	rows, err := tx.Query(ctx, `SELECT o_id, o_claim_token FROM aml_screening_request_claim($1,$2,120)`, tid, n)
	if err != nil {
		return
	}
	for rows.Next() {
		var id, tok uuid.UUID
		if err = rows.Scan(&id, &tok); err != nil {
			rows.Close()
			return
		}
		ids, tokens = append(ids, id), append(tokens, tok)
	}
	rows.Close()
	d = time.Since(t0)
	if err = rows.Err(); err != nil {
		return
	}
	for _, id := range ids {
		var pol uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT claim_policy_id FROM aml_screening_requests WHERE id=$1`, id).Scan(&pol); err != nil {
			return
		}
		policies = append(policies, pol)
	}
	return
}

// benchFn is measurement (a): per-function latency on the scenario's largest tenant, optionally under concurrent gate callers and
// keyset readers (scenario bg_gate / bg_sweeper). Mutating functions run in a transaction that is ROLLED BACK, so every repetition sees
// the same state.
func benchFn(ctx context.Context, db string, sc *Scenario, run int) (any, error) {
	c, err := openOwner(ctx, db)
	if err != nil {
		return nil, err
	}
	defer c.Close(ctx)
	tenants, err := listTenants(ctx, c)
	if err != nil {
		return nil, err
	}
	if len(tenants) == 0 {
		return nil, fmt.Errorf("no tenants")
	}
	big := tenants[0]
	for _, t := range tenants {
		if t.Persons > big.Persons {
			big = t
		}
	}
	persons, err := samplePersons(ctx, c, big.ID, 25, sc.Seed)
	if err != nil {
		return nil, err
	}
	// warm: touch the tenant's pages once so every run starts from the same (warm) cache state; the cache state is reported.
	if err := setTenant(ctx, c, big.ID); err != nil {
		return nil, err
	}
	if _, err := timeIn(ctx, c, big.ID, true, func(tx pgx.Tx) error {
		var n int
		return tx.QueryRow(ctx, `SELECT aml_screening_gate_overdue($1, 5000)`, big.ID).Scan(&n)
	}); err != nil {
		return nil, fmt.Errorf("warm-up: %w", err)
	}
	res := map[string]any{"tenant_persons": big.Persons, "tenants_in_db": len(tenants), "sampled_persons": len(persons)}
	byClass := map[string][]person{}
	for _, p := range persons {
		byClass[p.Class] = append(byClass[p.Class], p)
	}
	counts := map[string]int{}
	for k, v := range byClass {
		counts[k] = len(v)
	}
	res["sample_classes"] = counts

	var bg *bgLoad
	if sc.BgGate+sc.BgSweeper > 0 {
		if bg, err = startBg(ctx, db, big.ID, persons, sc.BgGate, sc.BgSweeper); err != nil {
			return nil, err
		}
		time.Sleep(2 * time.Second)
		res["background"] = map[string]int{"gate_callers": sc.BgGate, "sweeper_readers": sc.BgSweeper}
	}

	reps := sc.Reps
	// 1. gate_denies per person, per class
	gd := map[string]any{}
	for class, ps := range byClass {
		t, err := withProbe(ctx, db, func() ([]time.Duration, map[string]any, error) {
			var ds []time.Duration
			denied := 0
			for r := 0; r < reps; r++ {
				for _, p := range ps {
					d, err := timeIn(ctx, c, big.ID, true, func(tx pgx.Tx) error {
						var b bool
						if err := tx.QueryRow(ctx, `SELECT aml_screening_gate_denies($1,$2,'withdrawal')`, big.ID, p.ID).Scan(&b); err != nil {
							return err
						}
						if b {
							denied++
						}
						return nil
					})
					if err != nil {
						return nil, nil, err
					}
					ds = append(ds, d)
				}
			}
			return ds, map[string]any{"denied_calls": denied, "persons": len(ps)}, nil
		})
		if err != nil {
			return nil, fmt.Errorf("gate_denies %s: %w", class, err)
		}
		gd[class] = t
	}
	res["gate_denies_by_class"] = gd

	one := func(name string, reps int, f func(tx pgx.Tx) (time.Duration, map[string]any, error)) error {
		var last map[string]any
		t, err := withProbe(ctx, db, func() ([]time.Duration, map[string]any, error) {
			var ds []time.Duration
			for r := 0; r < reps; r++ {
				var d time.Duration
				_, err := timeIn(ctx, c, big.ID, true, func(tx pgx.Tx) error {
					var e error
					d, last, e = f(tx)
					return e
				})
				if err != nil {
					return nil, nil, err
				}
				ds = append(ds, d)
			}
			return ds, last, nil
		})
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		res[name] = t
		return nil
	}
	timeSQL := func(sql string, args ...any) func(tx pgx.Tx) (time.Duration, map[string]any, error) {
		return func(tx pgx.Tx) (time.Duration, map[string]any, error) {
			t0 := time.Now()
			var n int
			err := tx.QueryRow(ctx, sql, args...).Scan(&n)
			return time.Since(t0), map[string]any{"result": n}, err
		}
	}
	steps := []struct {
		name string
		f    func(tx pgx.Tx) (time.Duration, map[string]any, error)
	}{
		{"enqueue_due_100", timeSQL(`SELECT aml_screening_enqueue_due($1,100)`, big.ID)},
		{"enqueue_due_1000", timeSQL(`SELECT aml_screening_enqueue_due($1,1000)`, big.ID)},
		{"gate_overdue_5000", timeSQL(`SELECT aml_screening_gate_overdue($1,5000)`, big.ID)},
		{"gate_overdue_500", timeSQL(`SELECT aml_screening_gate_overdue($1,500)`, big.ID)},
		{"parked_summary_7d", func(tx pgx.Tx) (time.Duration, map[string]any, error) {
			t0 := time.Now()
			var a, b int
			err := tx.QueryRow(ctx, `SELECT o_persons,o_evaluations FROM aml_screening_parked_summary($1, clock_timestamp()-interval '7 days')`, big.ID).Scan(&a, &b)
			return time.Since(t0), map[string]any{"persons": a, "evaluations": b}, err
		}},
		{"parked_overdue_500_first", parkedBatch(ctx, big.ID, nil, 500)},
		{"parked_overdue_5000_first", parkedBatch(ctx, big.ID, nil, 5000)},
		{"claim_4_after_enqueue_100", func(tx pgx.Tx) (time.Duration, map[string]any, error) {
			if _, err := tx.Exec(ctx, `SELECT aml_screening_enqueue_due($1,100)`, big.ID); err != nil {
				return 0, nil, err
			}
			_, _, _, d, err := claimOne(ctx, tx, big.ID, 4)
			return d, map[string]any{"queue_depth": 100}, err
		}},
		{"claim_4_after_enqueue_1000", func(tx pgx.Tx) (time.Duration, map[string]any, error) {
			if _, err := tx.Exec(ctx, `SELECT aml_screening_enqueue_due($1,1000)`, big.ID); err != nil {
				return 0, nil, err
			}
			_, _, _, d, err := claimOne(ctx, tx, big.ID, 4)
			return d, map[string]any{"queue_depth": 1000}, err
		}},
		{"finish_clear", func(tx pgx.Tx) (time.Duration, map[string]any, error) {
			if _, err := tx.Exec(ctx, `SELECT screening_worker_heartbeat('mock-screening', true)`); err != nil {
				return 0, nil, err
			}
			if _, err := tx.Exec(ctx, `SELECT aml_screening_enqueue_due($1,10)`, big.ID); err != nil {
				return 0, nil, err
			}
			ids, toks, pols, _, err := claimOne(ctx, tx, big.ID, 1)
			if err != nil || len(ids) == 0 {
				return 0, nil, fmt.Errorf("claim for finish: %v (n=%d)", err, len(ids))
			}
			t0 := time.Now()
			var ap bool
			var st string
			var rv bool
			err = tx.QueryRow(ctx, `SELECT o_applied,o_status,o_reviewed FROM aml_screening_request_finish($1,$2,$3,'clear',NULL,NULL,'mock-screening','lt-x',$4,now(),0,NULL,$5)`,
				big.ID, ids[0], toks[0], "00000000000000000000000000000000000000000000000000000000000000ab", pols[0]).Scan(&ap, &st, &rv)
			return time.Since(t0), map[string]any{"applied": ap, "status": st}, err
		}},
	}
	for _, s := range steps {
		r := reps
		if err := one(s.name, r, s.f); err != nil {
			return nil, err
		}
	}

	// keyset parked read: the whole cycle of 500-person batches (what the payments sweeper walks, one batch per pass)
	budget := time.Duration(intExtra(sc, "cycle_budget_secs", 300)) * time.Second
	cyc, err := withProbe(ctx, db, func() ([]time.Duration, map[string]any, error) {
		var ds []time.Duration
		var after *uuid.UUID
		scanned, batches := 0, 0
		t0 := time.Now()
		complete := false
		for time.Since(t0) < budget {
			var d time.Duration
			var next *uuid.UUID
			var sc2 int
			_, err := timeIn(ctx, c, big.ID, true, func(tx pgx.Tx) error {
				var a, b, cc, dd int
				t1 := time.Now()
				e := tx.QueryRow(ctx, `SELECT o_parked,o_overdue,o_officer,o_officer_overdue,o_scanned,o_next FROM aml_screening_parked_overdue($1, clock_timestamp()-interval '7 days', $2, 500)`,
					big.ID, after).Scan(&a, &b, &cc, &dd, &sc2, &next)
				d = time.Since(t1)
				return e
			})
			if err != nil {
				return nil, nil, err
			}
			ds = append(ds, d)
			scanned += sc2
			batches++
			after = next
			if next == nil {
				complete = true
				break
			}
		}
		return ds, map[string]any{"batches": batches, "scanned": scanned, "cycle_complete": complete, "cycle_seconds": time.Since(t0).Seconds()}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("keyset cycle: %w", err)
	}
	res["parked_keyset_cycle_500"] = cyc
	if bg != nil {
		res["background_stats"] = bg.stop()
	}
	return res, nil
}

func parkedBatch(ctx context.Context, tid uuid.UUID, after *uuid.UUID, limit int) func(tx pgx.Tx) (time.Duration, map[string]any, error) {
	return func(tx pgx.Tx) (time.Duration, map[string]any, error) {
		t0 := time.Now()
		var a, b, c, d, e int
		var next *uuid.UUID
		err := tx.QueryRow(ctx, `SELECT o_parked,o_overdue,o_officer,o_officer_overdue,o_scanned,o_next FROM aml_screening_parked_overdue($1, clock_timestamp()-interval '7 days', $2, $3)`,
			tid, after, limit).Scan(&a, &b, &c, &d, &e, &next)
		return time.Since(t0), map[string]any{"parked": a, "overdue": b, "officer": c, "officer_overdue": d, "scanned": e}, err
	}
}

func intExtra(sc *Scenario, k string, def int) int {
	if v, ok := sc.Extra[k]; ok {
		if f, ok := v.(float64); ok {
			return int(f)
		}
	}
	return def
}
