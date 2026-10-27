//go:build loadtest

package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type tenantInfo struct {
	ID      uuid.UUID
	Persons int
}

func listTenants(ctx context.Context, c *pgx.Conn) ([]tenantInfo, error) {
	// player_accounts is tenant-RLS protected (FORCE); the harness lists tenants through the tenants table and counts per tenant
	// under each tenant's own setting.
	rows, err := c.Query(ctx, `SELECT id FROM tenants WHERE status='active' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := make([]tenantInfo, 0, len(ids))
	for _, id := range ids {
		if err := setTenant(ctx, c, id); err != nil {
			return nil, err
		}
		var n int
		if err := c.QueryRow(ctx, `SELECT count(DISTINCT person_id) FROM player_accounts WHERE tenant_id=$1`, id).Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, tenantInfo{id, n})
	}
	return out, nil
}

// setTenant sets the SESSION-level tenant setting on an owner connection (FORCE RLS applies to the owner role as well).
func setTenant(ctx context.Context, c *pgx.Conn, id uuid.UUID) error {
	_, err := c.Exec(ctx, `SELECT set_config('app.tenant_id', $1, false)`, id.String())
	return err
}

type person struct {
	ID, Account uuid.UUID
	Class       string
}

// samplePersons returns up to n persons per class (class derived from the data, see the SQL), reproducible for a seed.
func samplePersons(ctx context.Context, c *pgx.Conn, tid uuid.UUID, perClass int, seed int64) ([]person, error) {
	if err := setTenant(ctx, c, tid); err != nil {
		return nil, err
	}
	rows, err := c.Query(ctx, `
WITH cand AS (
  SELECT pa.person_id, min(pa.id::text)::uuid AS account FROM player_accounts pa WHERE pa.tenant_id=$1
   GROUP BY pa.person_id ORDER BY md5(pa.person_id::text||$2::text) LIMIT $3
), cls AS (
  SELECT c.person_id, c.account, CASE
    WHEN EXISTS (SELECT 1 FROM aml_holds h WHERE h.tenant_id=$1 AND h.person_id=c.person_id) THEN 'held'
    WHEN EXISTS (SELECT 1 FROM aml_screening_requests r WHERE r.tenant_id=$1 AND r.person_id=c.person_id AND r.status='needs_officer') THEN 'needs_officer'
    WHEN EXISTS (SELECT 1 FROM aml_screening_requests r WHERE r.tenant_id=$1 AND r.person_id=c.person_id AND r.status IN ('pending','claimed')) THEN 'pending'
    WHEN EXISTS (SELECT 1 FROM aml_screening_requests r WHERE r.tenant_id=$1 AND r.person_id=c.person_id AND r.status='done' AND r.completed_at > now()-interval '7 days') THEN 'clear'
    WHEN EXISTS (SELECT 1 FROM aml_screening_requests r WHERE r.tenant_id=$1 AND r.person_id=c.person_id AND r.status='done') THEN 'stale'
    WHEN EXISTS (SELECT 1 FROM aml_screening_results x WHERE x.tenant_id=$1 AND x.person_id=c.person_id) THEN 'legacy'
    ELSE 'none' END AS class FROM cand c
), ranked AS (SELECT *, row_number() OVER (PARTITION BY class ORDER BY person_id) rn FROM cls)
SELECT person_id, account, class FROM ranked WHERE rn <= $4`, tid, fmt.Sprint(seed), perClass*40, perClass)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []person
	for rows.Next() {
		var p person
		if err := rows.Scan(&p.ID, &p.Account, &p.Class); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// timeIn runs fn inside a transaction with the tenant set (transaction-local) and ALWAYS rolls back, so a mutating function can be timed
// repeatedly on identical state. It returns the time of the call itself.
func timeIn(ctx context.Context, c *pgx.Conn, tid uuid.UUID, rollback bool, fn func(tx pgx.Tx) error) (time.Duration, error) {
	tx, err := c.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tid.String()); err != nil {
		return 0, err
	}
	t0 := time.Now()
	if err := fn(tx); err != nil {
		return time.Since(t0), err
	}
	d := time.Since(t0)
	if !rollback {
		return d, tx.Commit(ctx)
	}
	return d, nil
}

// ---- background load: parallel gate callers (withdrawal / payout claims) and keyset parked readers

type bgStats struct {
	GateCalls, GateErrors, GateDenied int64
	Gate                              Stat
	SweeperBatches, SweeperErrors     int64
	Sweeper                           Stat
}

// bgGatePace, when > 0, is the pause between one gate caller's calls (0 = closed loop: a saturation stress, not a traffic model).
var bgGatePace time.Duration

type bgEv struct {
	T      time.Time
	D      time.Duration
	Denied bool
	Err    bool
}

type bgLoad struct {
	evs    []bgEv
	cancel context.CancelFunc
	wg     sync.WaitGroup
	gate   []time.Duration
	sweep  []time.Duration
	mu     sync.Mutex
	st     bgStats
	closer func()
}

// startBg starts nGate gate callers and nSweep keyset readers on an owner-login pool (the functions are SECURITY DEFINER, so the
// executing role is the owner either way; the owner login makes the sessions' wait events visible to the probe).
func startBg(ctx context.Context, db string, tid uuid.UUID, persons []person, nGate, nSweep int) (*bgLoad, error) {
	if nGate+nSweep == 0 {
		return &bgLoad{}, nil
	}
	u, err := ownerURL(db)
	if err != nil {
		return nil, err
	}
	pcfg, err := pgxpool.ParseConfig(u)
	if err != nil {
		return nil, fmt.Errorf("pool config")
	}
	pcfg.MaxConns = int32(nGate + nSweep + 1)
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, err
	}
	run := func(cctx context.Context, tid uuid.UUID, fn func(tx pgx.Tx) error) error {
		tx, err := pool.Begin(cctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(context.Background())
		if _, err := tx.Exec(cctx, `SELECT set_config('app.tenant_id', $1, true)`, tid.String()); err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit(cctx)
	}
	b := startBgWith(ctx, run, tid, persons, nGate, nSweep)
	go func() { <-ctx.Done() }()
	b.closer = pool.Close
	return b, nil
}

type txRunner func(ctx context.Context, tid uuid.UUID, fn func(tx pgx.Tx) error) error

// startBgWith runs the background callers over any tenant-scoped transaction runner (an owner pool, or the production db.Pool when
// the scenario shares the worker's pool).
func startBgWith(ctx context.Context, run txRunner, tid uuid.UUID, persons []person, nGate, nSweep int) *bgLoad {
	b := &bgLoad{}
	if nGate+nSweep == 0 {
		return b
	}
	cctx, cancel := context.WithCancel(ctx)
	b.cancel = cancel
	for i := 0; i < nGate; i++ {
		b.wg.Add(1)
		go func(i int) {
			defer b.wg.Done()
			rng := rand.New(rand.NewPCG(uint64(i+1), 77))
			for cctx.Err() == nil {
				p := persons[rng.IntN(len(persons))]
				t0 := time.Now()
				var denied bool
				err := run(cctx, tid, func(tx pgx.Tx) error {
					return tx.QueryRow(cctx, `SELECT aml_hold_active($1,$2,$3,'withdrawal')`, tid, p.ID, p.Account).Scan(&denied)
				})
				if cctx.Err() != nil {
					return
				}
				d := time.Since(t0)
				b.mu.Lock()
				b.st.GateCalls++
				b.evs = append(b.evs, bgEv{T: t0, D: d, Denied: denied, Err: err != nil})
				if err != nil {
					b.st.GateErrors++
				} else {
					b.gate = append(b.gate, d)
					if denied {
						b.st.GateDenied++
					}
				}
				b.mu.Unlock()
				if bgGatePace > 0 {
					time.Sleep(bgGatePace)
				}
			}
		}(i)
	}
	for i := 0; i < nSweep; i++ {
		b.wg.Add(1)
		go func(i int) {
			defer b.wg.Done()
			var after *uuid.UUID
			for cctx.Err() == nil {
				t0 := time.Now()
				var next *uuid.UUID
				err := run(cctx, tid, func(tx pgx.Tx) error {
					var a, bb, c, d, e int
					return tx.QueryRow(cctx, `SELECT o_parked,o_overdue,o_officer,o_officer_overdue,o_scanned,o_next FROM aml_screening_parked_overdue($1, clock_timestamp()-interval '7 days', $2, 500)`,
						tid, after).Scan(&a, &bb, &c, &d, &e, &next)
				})
				if cctx.Err() != nil {
					return
				}
				d := time.Since(t0)
				b.mu.Lock()
				b.st.SweeperBatches++
				if err != nil {
					b.st.SweeperErrors++
				} else {
					b.sweep = append(b.sweep, d)
				}
				b.mu.Unlock()
				after = next
				time.Sleep(200 * time.Millisecond) // sweeper pass interval is far longer in production; this is a stress setting
			}
		}(i)
	}
	return b
}

func (b *bgLoad) stop() bgStats {
	if b.cancel == nil {
		return b.st
	}
	b.cancel()
	b.wg.Wait()
	if b.closer != nil {
		b.closer()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.st.Gate = summarize(b.gate)
	b.st.Sweeper = summarize(b.sweep)
	return b.st
}

var _ atomic.Int64

// timeline buckets the gate callers' calls into `width` buckets since t0: calls, errors, denied, median and max latency.
func (b *bgLoad) timeline(t0 time.Time, width time.Duration) []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	by := map[int][]bgEv{}
	max := 0
	for _, e := range b.evs {
		i := int(e.T.Sub(t0) / width)
		if i < 0 {
			continue
		}
		by[i] = append(by[i], e)
		if i > max {
			max = i
		}
	}
	var out []map[string]any
	for i := 0; i <= max; i++ {
		es := by[i]
		var ds []time.Duration
		den, errs := 0, 0
		for _, e := range es {
			ds = append(ds, e.D)
			if e.Denied {
				den++
			}
			if e.Err {
				errs++
			}
		}
		st := summarize(ds)
		out = append(out, map[string]any{"t_start_s": (time.Duration(i) * width).Seconds(), "calls": len(es), "denied": den, "errors": errs, "median_ms": st.MedMs, "max_ms": st.MaxMs})
	}
	return out
}

// deniedPersons is the set of distinct persons any gate caller saw denied.
func (b *bgLoad) calls() int { b.mu.Lock(); defer b.mu.Unlock(); return len(b.evs) }
