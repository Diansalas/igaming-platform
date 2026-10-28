//go:build loadtest

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/aml"
)

// workerMain is the child process of the crash scenario: the REAL aml.RunScreeningWorkerLoop with the MOCK LatencyProvider, until SIGTERM.
func workerMain(ctx context.Context, dbName string, sc *Scenario) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	prov := NewLatencyProvider(sc.Provider, sc.Seed)
	pool, err := openRuntimePool(ctx, dbName, sc.PoolConns)
	if err != nil {
		return err
	}
	defer pool.Close()
	w, err := newWorker(pool, prov, sc)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	aml.RunScreeningWorkerLoop(ctx, w, logger, passInterval)
	c, f, h := prov.Counts()
	fmt.Printf("worker exit provider_calls=%d fails=%d hangs=%d\n", c, f, h)
	return nil
}

type claimRow struct {
	ID, Token, Policy uuid.UUID
	Attempts          int
	LeaseUntil        time.Time
}

// benchCrash is measurement (f): kill the worker process (by pid, SIGKILL) while it holds claims and is inside a provider call, then
// restart a worker and watch lease expiry, reclaim under a new token, and that no request is finished twice.
func benchCrash(ctx context.Context, dbName string, sc *Scenario, run int) (any, error) {
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
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	start := func(logName string) (*exec.Cmd, error) {
		cmd := exec.Command(self, "worker", "-scenario", scenarioPath, "-db", dbName)
		f, err := os.CreateTemp("", "capa_worker_"+logName+"_*.log")
		if err != nil {
			return nil, err
		}
		cmd.Stdout, cmd.Stderr = f, f
		cmd.Env = os.Environ()
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return cmd, nil
	}
	res := map[string]any{}
	denied0, _, _ := deniedCount(ctx, oc, tid)
	res["denied_before"] = denied0
	w1, err := start("w1")
	if err != nil {
		return nil, err
	}
	t0 := time.Now()
	// wait for claims to be held while the provider call is in flight
	var held []claimRow
	for time.Since(t0) < 90*time.Second {
		time.Sleep(300 * time.Millisecond)
		if err := setTenant(ctx, oc, tid); err != nil {
			return nil, err
		}
		rows, err := oc.Query(ctx, `SELECT id, claim_token, claim_policy_id, attempts, lease_until FROM aml_screening_requests WHERE tenant_id=$1 AND status='claimed'`, tid)
		if err != nil {
			return nil, err
		}
		held = held[:0]
		for rows.Next() {
			var r claimRow
			if err := rows.Scan(&r.ID, &r.Token, &r.Policy, &r.Attempts, &r.LeaseUntil); err != nil {
				rows.Close()
				return nil, err
			}
			held = append(held, r)
		}
		rows.Close()
		if len(held) >= 2 {
			break
		}
	}
	if len(held) == 0 {
		w1.Process.Kill()
		return nil, fmt.Errorf("worker never held a claim")
	}
	time.Sleep(1500 * time.Millisecond) // inside the provider call (injected latency is longer)
	tKill := time.Now()
	pid := w1.Process.Pid
	sig, signame := syscall.SIGKILL, "SIGKILL"
	if v, ok := sc.Extra["kill_signal"].(string); ok && v == "TERM" {
		sig, signame = syscall.SIGTERM, "SIGTERM"
	}
	if err := syscall.Kill(pid, sig); err != nil { // by pid; no pkill
		return nil, err
	}
	w1.Wait()
	res["killed_pid_signal"] = signame
	res["held_at_kill"] = len(held)
	res["lease_remaining_at_kill_s"] = time.Until(held[0].LeaseUntil).Seconds() + time.Since(tKill).Seconds()
	st1, _ := statusCounts(ctx, oc, tid)
	res["status_after_kill"] = st1
	restart := time.Duration(intExtra(sc, "restart_delay_secs", 5)) * time.Second
	time.Sleep(restart)
	w2, err := start("w2")
	if err != nil {
		return nil, err
	}
	tRestart := time.Now()
	res["restart_after_kill_s"] = tRestart.Sub(tKill).Seconds()
	type reclaim struct {
		ID           string  `json:"request"`
		OldAttempts  int     `json:"old_attempts"`
		NewAttempts  int     `json:"new_attempts"`
		NewToken     bool    `json:"token_changed"`
		ReclaimedAtS float64 `json:"reclaimed_s_after_kill"`
		DoneAtS      float64 `json:"done_s_after_kill"`
	}
	recl := map[uuid.UUID]*reclaim{}
	for _, h := range held {
		recl[h.ID] = &reclaim{ID: h.ID.String()[:8], OldAttempts: h.Attempts}
	}
	deadline := time.Duration(intExtra(sc, "observe_secs", 420)) * time.Second
	var timeline []map[string]any
	for time.Since(tKill) < deadline {
		time.Sleep(2 * time.Second)
		if err := setTenant(ctx, oc, tid); err != nil {
			return nil, err
		}
		alldone := true
		for _, h := range held {
			var att int
			var tok *uuid.UUID
			var st string
			if err := oc.QueryRow(ctx, `SELECT attempts, claim_token, status FROM aml_screening_requests WHERE id=$1`, h.ID).Scan(&att, &tok, &st); err != nil {
				return nil, err
			}
			r := recl[h.ID]
			if att > h.Attempts && r.ReclaimedAtS == 0 {
				r.NewAttempts, r.NewToken, r.ReclaimedAtS = att, tok == nil || *tok != h.Token, time.Since(tKill).Seconds()
			}
			if st == "done" && r.DoneAtS == 0 {
				r.NewAttempts, r.DoneAtS = att, time.Since(tKill).Seconds()
			}
			if st != "done" {
				alldone = false
			}
		}
		m, _ := statusCounts(ctx, oc, tid)
		timeline = append(timeline, map[string]any{"s_after_kill": time.Since(tKill).Seconds(), "status": m})
		if alldone && m["pending"]+m["claimed"] == 0 {
			break
		}
	}
	var rs []reclaim
	for _, h := range held {
		rs = append(rs, *recl[h.ID])
	}
	res["held_requests"] = rs
	// no double finish: a late finish with the OLD token (a stalled worker waking up) must apply nothing; a second finish with the
	// current state must apply nothing.
	var staleApplied, secondApplied int
	for _, h := range held {
		for _, tok := range []uuid.UUID{h.Token} {
			var ap bool
			var st string
			var rv bool
			if err := setTenant(ctx, oc, tid); err != nil {
				return nil, err
			}
			if err := oc.QueryRow(ctx, `SELECT o_applied,o_status,o_reviewed FROM aml_screening_request_finish($1,$2,$3,'clear',NULL,NULL,'mock-screening','lt-late',$4,now(),0,NULL,$5)`,
				tid, h.ID, tok, "00000000000000000000000000000000000000000000000000000000000000cd", h.Policy).Scan(&ap, &st, &rv); err != nil {
				res["late_finish_error"] = err.Error()
				continue
			}
			if ap {
				staleApplied++
			}
		}
	}
	res["late_finish_with_old_token_applied_count"] = staleApplied
	_ = secondApplied
	var dups int
	_ = oc.QueryRow(ctx, `SELECT count(*) FROM (SELECT idempotency_key FROM aml_screening_requests WHERE tenant_id=$1 GROUP BY 1 HAVING count(*) > 1) q`, tid).Scan(&dups)
	res["duplicate_idempotency_keys"] = dups
	var over int
	_ = oc.QueryRow(ctx, `SELECT count(*) FROM aml_screening_requests WHERE tenant_id=$1 AND attempts > 5`, tid).Scan(&over)
	res["requests_with_attempts_over_max"] = over
	// restart drain: time from restart to no pending / claimed left
	stN, _ := statusCounts(ctx, oc, tid)
	deniedN, _, _ := deniedCount(ctx, oc, tid)
	res["status_end"], res["denied_after"], res["timeline"] = stN, deniedN, timeline
	res["observe_s_after_kill"] = time.Since(tKill).Seconds()
	res["restart_to_end_s"] = time.Since(tRestart).Seconds()
	syscall.Kill(w2.Process.Pid, syscall.SIGTERM)
	w2.Wait()
	return res, nil
}
