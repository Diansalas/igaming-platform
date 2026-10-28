//go:build loadtest

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// benchGrowth is measurement (b): cost of aml_screening_since / aml_screening_covers / the gate / enqueue_due against the number of
// screening policy versions per tenant and jurisdiction (the scenario's policy_versions, or -versions).
func benchGrowth(ctx context.Context, db string, sc *Scenario, run int) (any, error) {
	c, err := openOwner(ctx, db)
	if err != nil {
		return nil, err
	}
	defer c.Close(ctx)
	tenants, err := listTenants(ctx, c)
	if err != nil {
		return nil, err
	}
	tid := tenants[0].ID
	if err := setTenant(ctx, c, tid); err != nil {
		return nil, err
	}
	var jur uuid.UUID
	if err := c.QueryRow(ctx, `SELECT licensing_jurisdiction_id FROM aml_screening_policies WHERE tenant_id=$1 LIMIT 1`, tid).Scan(&jur); err != nil {
		return nil, err
	}
	var nv int
	_ = c.QueryRow(ctx, `SELECT count(*) FROM aml_screening_policies WHERE tenant_id=$1 AND licensing_jurisdiction_id=$2`, tid, jur).Scan(&nv)
	persons, err := samplePersons(ctx, c, tid, 25, sc.Seed)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"policy_versions": nv, "persons": tenants[0].Persons}
	reps := sc.Reps
	sinceAt := []struct{ name, expr string }{
		{"since_at_oldest(all_versions_later)", fmt.Sprintf(`now() - make_interval(days => %d)`, sc.GateFirstDaysAgo+2)},
		{"since_at_mid", fmt.Sprintf(`now() - make_interval(days => %d)`, (sc.GateFirstDaysAgo+2)/2)},
		{"since_at_recent_3d", `now() - interval '3 days'`},
	}
	for _, s := range sinceAt {
		s := s
		t, err := withProbe(ctx, db, func() ([]time.Duration, map[string]any, error) {
			var ds []time.Duration
			var last any
			for r := 0; r < reps; r++ {
				d, err := timeIn(ctx, c, tid, true, func(tx pgx.Tx) error {
					var v *int64
					e := tx.QueryRow(ctx, `SELECT aml_screening_since($1,$2,`+s.expr+`, ARRAY['sanctions','pep'], NULL)`, tid, jur).Scan(&v)
					last = v
					return e
				})
				if err != nil {
					return nil, nil, err
				}
				ds = append(ds, d)
			}
			return ds, map[string]any{"result_is_null": last == (*int64)(nil)}, nil
		})
		if err != nil {
			return nil, err
		}
		res[s.name] = t
	}
	// Production call shape (review M1): the gate calls since(.., LEAST(claimed_at, completed_at, screened_at), lists, r.claim_policy_id) with the
	// claim-time version; the cells above pass p_after_policy = NULL. Here p_at is recent (3 days) and p_after is a version from mid-history / the latest.
	var midID, lastID uuid.UUID
	if err := c.QueryRow(ctx, `SELECT id FROM aml_screening_policies WHERE tenant_id=$1 AND licensing_jurisdiction_id=$2 ORDER BY created_at, id OFFSET $3 LIMIT 1`, tid, jur, nv/2).Scan(&midID); err != nil {
		return nil, err
	}
	if err := c.QueryRow(ctx, `SELECT id FROM aml_screening_policies WHERE tenant_id=$1 AND licensing_jurisdiction_id=$2 ORDER BY created_at DESC, id DESC LIMIT 1`, tid, jur).Scan(&lastID); err != nil {
		return nil, err
	}
	for _, shape := range []struct {
		name  string
		after uuid.UUID
	}{{"since_prod_shape_after=mid_history_version", midID}, {"since_prod_shape_after=latest_version", lastID}} {
		shape := shape
		t, err := withProbe(ctx, db, func() ([]time.Duration, map[string]any, error) {
			var ds []time.Duration
			for r := 0; r < reps; r++ {
				d, err := timeIn(ctx, c, tid, true, func(tx pgx.Tx) error {
					var v *int64
					return tx.QueryRow(ctx, `SELECT aml_screening_since($1,$2, now() - interval '3 days', ARRAY['sanctions','pep'], $3)`, tid, jur, shape.after).Scan(&v)
				})
				if err != nil {
					return nil, nil, err
				}
				ds = append(ds, d)
			}
			return ds, nil, nil
		})
		if err != nil {
			return nil, err
		}
		res[shape.name] = t
	}
	// covers is a constant-time SQL function (no table access); one timing call per rep
	t, err := withProbe(ctx, db, func() ([]time.Duration, map[string]any, error) {
		var ds []time.Duration
		for r := 0; r < reps*20; r++ {
			d, err := timeIn(ctx, c, tid, true, func(tx pgx.Tx) error {
				var b bool
				return tx.QueryRow(ctx, `SELECT aml_screening_covers(now()-interval '1 day', 1209600, ARRAY['sanctions','pep'], 1209600, ARRAY['sanctions','pep'], 1209600)`).Scan(&b)
			})
			if err != nil {
				return nil, nil, err
			}
			ds = append(ds, d)
		}
		return ds, nil, nil
	})
	if err != nil {
		return nil, err
	}
	res["covers"] = t
	byClass := map[string][]person{}
	for _, p := range persons {
		byClass[p.Class] = append(byClass[p.Class], p)
	}
	for _, class := range []string{"clear", "stale", "none"} {
		ps := byClass[class]
		if len(ps) == 0 {
			continue
		}
		t, err := withProbe(ctx, db, func() ([]time.Duration, map[string]any, error) {
			var ds []time.Duration
			for r := 0; r < reps; r++ {
				for _, p := range ps {
					d, err := timeIn(ctx, c, tid, true, func(tx pgx.Tx) error {
						var b bool
						return tx.QueryRow(ctx, `SELECT aml_screening_gate_denies($1,$2,'withdrawal')`, tid, p.ID).Scan(&b)
					})
					if err != nil {
						return nil, nil, err
					}
					ds = append(ds, d)
				}
			}
			return ds, map[string]any{"persons": len(ps)}, nil
		})
		if err != nil {
			return nil, err
		}
		res["gate_denies_"+class] = t
	}
	for _, q := range []struct {
		name, sql string
	}{{"enqueue_due_100", `SELECT aml_screening_enqueue_due($1,100)`}, {"gate_overdue_500", `SELECT aml_screening_gate_overdue($1,500)`}} {
		q := q
		t, err := withProbe(ctx, db, func() ([]time.Duration, map[string]any, error) {
			var ds []time.Duration
			for r := 0; r < reps; r++ {
				d, err := timeIn(ctx, c, tid, true, func(tx pgx.Tx) error {
					var n int
					return tx.QueryRow(ctx, q.sql, tid).Scan(&n)
				})
				if err != nil {
					return nil, nil, err
				}
				ds = append(ds, d)
			}
			return ds, nil, nil
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", q.name, err)
		}
		res[q.name] = t
	}
	return res, nil
}
