//go:build loadtest

package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

// Probe records DB and OS counters around a measured region (item h): pg_stat_database / pg_stat_user_tables / pg_stat_user_indexes
// deltas, the shared-buffer hit ratio, /proc/stat CPU shares, the load average, and a sampler of pg_locks waits and (for sessions of the
// SAME role as the probe; PostgreSQL hides other roles' wait events from a non-superuser and this harness does not escalate)
// pg_stat_activity wait events and connection counts by role.
type Probe struct {
	db                             string
	conn                           *pgx.Conn
	start                          statSnap
	stop                           chan struct{}
	wg                             sync.WaitGroup
	mu                             sync.Mutex
	waitHist                       map[string]int
	lockWaits, samples, maxWaiting int
	lockKinds                      map[string]int
	maxConns                       map[string]int
	t0                             time.Time
	ru0                            syscall.Rusage
	// Interval of the sampler.
	Every time.Duration
}

type statSnap struct {
	db     map[string]int64
	tables map[string]map[string]int64
	idx    map[string]map[string]int64
	cpu    []float64
	load   string
}

var screeningRels = []string{"aml_screening_requests", "aml_screening_results", "aml_screening_policies", "aml_screening_trigger_policies",
	"aml_enforcement_decisions", "aml_holds", "player_accounts", "alerts", "alert_occurrences", "screening_worker_heartbeats", "kyc_verifications"}

func readCPU() []float64 {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return nil
	}
	line := strings.SplitN(string(b), "\n", 2)[0]
	f := strings.Fields(line)[1:]
	out := make([]float64, 0, 8)
	for i := 0; i < len(f) && i < 8; i++ {
		v, _ := strconv.ParseFloat(f[i], 64)
		out = append(out, v)
	}
	return out
}

func readLoad() string {
	b, _ := os.ReadFile("/proc/loadavg")
	return strings.TrimSpace(string(b))
}

func snapshot(ctx context.Context, c *pgx.Conn) (statSnap, error) {
	s := statSnap{db: map[string]int64{}, tables: map[string]map[string]int64{}, idx: map[string]map[string]int64{}, cpu: readCPU(), load: readLoad()}
	cols := []string{"xact_commit", "xact_rollback", "blks_read", "blks_hit", "tup_returned", "tup_fetched", "tup_inserted", "tup_updated", "tup_deleted",
		"temp_files", "temp_bytes", "deadlocks"}
	q := "SELECT " + strings.Join(cols, "::bigint, ") + "::bigint FROM pg_stat_database WHERE datname = current_database()"
	vals := make([]int64, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := c.QueryRow(ctx, q).Scan(ptrs...); err != nil {
		return s, err
	}
	for i, k := range cols {
		s.db[k] = vals[i]
	}
	rows, err := c.Query(ctx, `SELECT relname, COALESCE(seq_scan,0), COALESCE(seq_tup_read,0), COALESCE(idx_scan,0), COALESCE(idx_tup_fetch,0),
		n_tup_ins, n_tup_upd, n_tup_del, n_dead_tup FROM pg_stat_user_tables WHERE relname = ANY($1)`, screeningRels)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var n string
		var a, b, cc, d, e, f, g, h int64
		if err := rows.Scan(&n, &a, &b, &cc, &d, &e, &f, &g, &h); err != nil {
			rows.Close()
			return s, err
		}
		s.tables[n] = map[string]int64{"seq_scan": a, "seq_tup_read": b, "idx_scan": cc, "idx_tup_fetch": d, "n_tup_ins": e, "n_tup_upd": f, "n_tup_del": g, "n_dead_tup": h}
	}
	rows.Close()
	rows, err = c.Query(ctx, `SELECT indexrelname, COALESCE(idx_scan,0), COALESCE(idx_tup_read,0), COALESCE(idx_tup_fetch,0) FROM pg_stat_user_indexes WHERE relname = ANY($1)`, screeningRels)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var n string
		var a, b, cc int64
		if err := rows.Scan(&n, &a, &b, &cc); err != nil {
			rows.Close()
			return s, err
		}
		s.idx[n] = map[string]int64{"idx_scan": a, "idx_tup_read": b, "idx_tup_fetch": cc}
	}
	rows.Close()
	return s, nil
}

// StartProbe begins a probe on the private database.
func StartProbe(ctx context.Context, db string, every time.Duration) (*Probe, error) {
	c, err := openOwner(ctx, db)
	if err != nil {
		return nil, err
	}
	p := &Probe{db: db, conn: c, stop: make(chan struct{}), waitHist: map[string]int{}, lockKinds: map[string]int{}, maxConns: map[string]int{}, Every: every}
	if p.Every == 0 {
		p.Every = 500 * time.Millisecond
	}
	time.Sleep(1200 * time.Millisecond) // let earlier activity's statistics flush so the baseline is clean
	if p.start, err = snapshot(ctx, c); err != nil {
		c.Close(ctx)
		return nil, err
	}
	p.t0 = time.Now()
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &p.ru0)
	p.wg.Add(1)
	go p.loop(ctx)
	return p, nil
}

func (p *Probe) loop(ctx context.Context) {
	defer p.wg.Done()
	c2, err := openOwner(ctx, p.db) // sampler uses its own connection so it never waits on the snapshot connection
	if err != nil {
		return
	}
	defer c2.Close(context.Background())
	t := time.NewTicker(p.Every)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
		}
		qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		p.sample(qctx, c2)
		cancel()
	}
}

func (p *Probe) sample(ctx context.Context, c *pgx.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.samples++
	// lock waits: pg_locks is visible to every role
	rows, err := c.Query(ctx, `SELECT l.locktype||':'||l.mode, count(*) FROM pg_locks l JOIN pg_stat_database d ON d.datid = l.database WHERE NOT l.granted AND d.datname = current_database() GROUP BY 1`)
	if err == nil {
		tot := 0
		for rows.Next() {
			var k string
			var n int
			if rows.Scan(&k, &n) == nil {
				p.lockKinds[k] += n
				tot += n
			}
		}
		rows.Close()
		if tot > 0 {
			p.lockWaits++
		}
		if tot > p.maxWaiting {
			p.maxWaiting = tot
		}
	}
	rows, err = c.Query(ctx, `SELECT COALESCE(wait_event_type,'cpu_or_none')||':'||COALESCE(wait_event,'-'), count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND state = 'active' AND pid <> pg_backend_pid() GROUP BY 1`)
	if err == nil {
		for rows.Next() {
			var k string
			var n int
			if rows.Scan(&k, &n) == nil {
				p.waitHist[k] += n
			}
		}
		rows.Close()
	}
	rows, err = c.Query(ctx, `SELECT usename, count(*) FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid() GROUP BY 1`)
	if err == nil {
		for rows.Next() {
			var k string
			var n int
			if rows.Scan(&k, &n) == nil && n > p.maxConns[k] {
				p.maxConns[k] = n
			}
		}
		rows.Close()
	}
}

// ProbeResult is the JSON form of a probe.
type ProbeResult struct {
	WallSeconds         float64                     `json:"wall_seconds"`
	DB                  map[string]int64            `json:"db_delta"`
	BufferHitRatio      float64                     `json:"shared_buffers_hit_ratio"`
	Tables              map[string]map[string]int64 `json:"tables_delta"`
	Indexes             map[string]map[string]int64 `json:"indexes_delta_nonzero"`
	CPUPct              map[string]float64          `json:"os_cpu_pct"`
	LoadAvgBefore       string                      `json:"loadavg_before"`
	LoadAvgAfter        string                      `json:"loadavg_after"`
	HarnessCPUSecs      float64                     `json:"harness_process_cpu_seconds"`
	Samples             int                         `json:"lock_wait_samples"`
	SamplesWithLockWait int                         `json:"samples_with_lock_wait"`
	MaxWaitingLocks     int                         `json:"max_waiting_locks"`
	LockKinds           map[string]int              `json:"lock_wait_kinds"`
	WaitEvents          map[string]int              `json:"wait_events_same_role_sessions"`
	MaxConnsByRole      map[string]int              `json:"max_connections_by_role"`
}

// Stop ends the probe and returns the deltas.
func (p *Probe) Stop(ctx context.Context) (ProbeResult, error) {
	close(p.stop)
	p.wg.Wait()
	time.Sleep(1200 * time.Millisecond) // stats flush
	end, err := snapshot(ctx, p.conn)
	p.conn.Close(ctx)
	var r ProbeResult
	if err != nil {
		return r, err
	}
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	r.WallSeconds = time.Since(p.t0).Seconds() - 1.2
	r.DB = map[string]int64{}
	for k, v := range end.db {
		r.DB[k] = v - p.start.db[k]
	}
	if tot := r.DB["blks_hit"] + r.DB["blks_read"]; tot > 0 {
		r.BufferHitRatio = float64(r.DB["blks_hit"]) / float64(tot)
	}
	r.Tables = map[string]map[string]int64{}
	for n, m := range end.tables {
		d := map[string]int64{}
		nz := false
		for k, v := range m {
			dv := v - p.start.tables[n][k]
			if k == "n_dead_tup" {
				dv = v
			}
			d[k] = dv
			if dv != 0 && k != "n_dead_tup" {
				nz = true
			}
		}
		if nz {
			r.Tables[n] = d
		}
	}
	r.Indexes = map[string]map[string]int64{}
	for n, m := range end.idx {
		d := map[string]int64{}
		nz := false
		for k, v := range m {
			dv := v - p.start.idx[n][k]
			d[k] = dv
			if dv != 0 {
				nz = true
			}
		}
		if nz {
			r.Indexes[n] = d
		}
	}
	r.CPUPct = map[string]float64{}
	if len(end.cpu) >= 8 && len(p.start.cpu) >= 8 {
		names := []string{"user", "nice", "system", "idle", "iowait", "irq", "softirq", "steal"}
		tot := 0.0
		d := make([]float64, 8)
		for i := range d {
			d[i] = end.cpu[i] - p.start.cpu[i]
			tot += d[i]
		}
		for i, n := range names {
			if tot > 0 {
				r.CPUPct[n] = round1(100 * d[i] / tot)
			}
		}
	}
	r.LoadAvgBefore, r.LoadAvgAfter = p.start.load, end.load
	r.HarnessCPUSecs = float64(ru.Utime.Sec-p.ru0.Utime.Sec+ru.Stime.Sec-p.ru0.Stime.Sec) + float64(ru.Utime.Usec-p.ru0.Utime.Usec+ru.Stime.Usec-p.ru0.Stime.Usec)/1e6
	p.mu.Lock()
	defer p.mu.Unlock()
	r.Samples, r.SamplesWithLockWait, r.MaxWaitingLocks = p.samples, p.lockWaits, p.maxWaiting
	r.LockKinds, r.WaitEvents, r.MaxConnsByRole = p.lockKinds, p.waitHist, p.maxConns
	return r, nil
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

// ---- small statistics helpers

type Stat struct {
	N      int     `json:"n"`
	MinMs  float64 `json:"min_ms"`
	MedMs  float64 `json:"median_ms"`
	P95Ms  float64 `json:"p95_ms"`
	MaxMs  float64 `json:"max_ms"`
	MeanMs float64 `json:"mean_ms"`
	Over5s int     `json:"over_phase_timeout_5s"`
}

func summarize(ds []time.Duration) Stat {
	if len(ds) == 0 {
		return Stat{}
	}
	ms := make([]float64, len(ds))
	var sum float64
	over := 0
	for i, d := range ds {
		ms[i] = float64(d.Microseconds()) / 1000
		sum += ms[i]
		if d > 5*time.Second {
			over++
		}
	}
	sort.Float64s(ms)
	pct := func(p float64) float64 {
		i := int(p * float64(len(ms)-1))
		return ms[i]
	}
	return Stat{N: len(ms), MinMs: ms[0], MedMs: pct(0.5), P95Ms: pct(0.95), MaxMs: ms[len(ms)-1], MeanMs: sum / float64(len(ms)), Over5s: over}
}

func must(err error) {
	if err != nil {
		panic(fmt.Sprint(err))
	}
}
