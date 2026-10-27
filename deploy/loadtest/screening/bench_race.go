//go:build loadtest

package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// benchRace is measurement (g): a screening policy change that lands between claim and finish. Part A is a deterministic
// interleaving (step results are recorded); part B is a stress run: the real worker drains while a policy version is inserted every
// `policy_every_ms`, then invariants are checked in the database.
func benchRace(ctx context.Context, dbName string, sc *Scenario, run int) (any, error) {
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
	res := map[string]any{}
	persons, err := samplePersons(ctx, oc, tid, 5, sc.Seed)
	if err != nil {
		return nil, err
	}
	var x, y person
	for _, p := range persons {
		if p.Class == "stale" && x.ID == uuid.Nil {
			x = p
		} else if p.Class == "stale" && y.ID == uuid.Nil {
			y = p
		}
	}
	if y.ID == uuid.Nil {
		return nil, fmt.Errorf("need two stale persons")
	}
	step := map[string]any{}
	res["part_a"] = step
	if err := setTenant(ctx, oc, tid); err != nil {
		return nil, err
	}
	var jur uuid.UUID
	if err := oc.QueryRow(ctx, `SELECT licensing_jurisdiction_id FROM aml_screening_policies WHERE tenant_id=$1 LIMIT 1`, tid).Scan(&jur); err != nil {
		return nil, err
	}
	// enqueue one request per person (owner path; the guard trigger stays ON)
	for _, p := range []person{x, y} {
		if _, err := oc.Exec(ctx, `INSERT INTO aml_screening_requests (tenant_id, person_id, licensing_jurisdiction_id, trigger, idempotency_key) VALUES ($1,$2,$3,'kyc_approval',$4)`,
			tid, p.ID, jur, "race:"+p.ID.String()+":a"); err != nil {
			return nil, fmt.Errorf("enqueue: %w", err)
		}
	}
	if _, err := oc.Exec(ctx, `SELECT screening_worker_heartbeat('mock-screening', true)`); err != nil {
		return nil, err
	}
	// 1. claim both
	rows, err := oc.Query(ctx, `SELECT o_id, o_person_id, o_claim_token FROM aml_screening_request_claim($1, 200, 120)`, tid)
	if err != nil {
		return nil, err
	}
	type cl struct{ id, token uuid.UUID }
	got := map[uuid.UUID]cl{}
	n := 0
	for rows.Next() {
		var id, pid, tok uuid.UUID
		if err := rows.Scan(&id, &pid, &tok); err != nil {
			return nil, err
		}
		got[pid] = cl{id, tok}
		n++
	}
	rows.Close()
	step["1_claimed_total"] = n
	cx, cy := got[x.ID], got[y.ID]
	var polClaim uuid.UUID
	_ = oc.QueryRow(ctx, `SELECT claim_policy_id FROM aml_screening_requests WHERE id=$1`, cx.id).Scan(&polClaim)
	// 2. a stricter policy version (a list added) takes effect after the claim
	var lists []string
	var interval int64
	_ = oc.QueryRow(ctx, `SELECT lists, rescreen_interval_seconds FROM aml_screening_policies WHERE tenant_id=$1 ORDER BY created_at DESC, id DESC LIMIT 1`, tid).Scan(&lists, &interval)
	// the seed's policies carry both lists unless initial_lists_single was set; make the new version STRICTER either way by halving the interval
	if _, err := insertPolicyVersion(ctx, oc, tid, []string{"sanctions", "pep"}, interval/2); err != nil {
		return nil, err
	}
	var polNew uuid.UUID
	_ = oc.QueryRow(ctx, `SELECT id FROM aml_screening_policies WHERE tenant_id=$1 ORDER BY created_at DESC, id DESC LIMIT 1`, tid).Scan(&polNew)
	step["2_new_policy_version_inserted_after_claim"] = true
	// 3. phase-A race: the worker read the NEW version (aml_screening_policy_effective) but the claim recorded the OLD one
	var ap bool
	var st string
	var rv bool
	err = oc.QueryRow(ctx, `SELECT o_applied,o_status,o_reviewed FROM aml_screening_request_finish($1,$2,$3,'clear',NULL,NULL,'mock-screening','lt-y',$4,now(),0,NULL,$5)`,
		tid, cy.id, cy.token, "00000000000000000000000000000000000000000000000000000000000000ef", polNew).Scan(&ap, &st, &rv)
	step["3_finish_naming_the_NEW_version_(claim_recorded_old)"] = map[string]any{"applied": ap, "status": st, "error": errStr(err)}
	// 4. the worker read the version in force at the claim: finish applies, recording the OLD version's provenance
	err = oc.QueryRow(ctx, `SELECT o_applied,o_status,o_reviewed FROM aml_screening_request_finish($1,$2,$3,'clear',NULL,NULL,'mock-screening','lt-x',$4,now(),0,NULL,$5)`,
		tid, cx.id, cx.token, "00000000000000000000000000000000000000000000000000000000000000ab", polClaim).Scan(&ap, &st, &rv)
	step["4_finish_naming_the_claim_version"] = map[string]any{"applied": ap, "status": st, "error": errStr(err)}
	var scr uuid.UUID
	var sInt int64
	_ = oc.QueryRow(ctx, `SELECT screened_policy_id, screened_interval_seconds FROM aml_screening_requests WHERE id=$1`, cx.id).Scan(&scr, &sInt)
	step["4_recorded_provenance"] = map[string]any{"policy_is_claim_version": scr == polClaim, "interval_s": sInt}
	// 5. does the gate still deny X right after its clear under the old version (the new version is stricter)?
	var denied bool
	_ = oc.QueryRow(ctx, `SELECT aml_screening_gate_denies($1,$2,'withdrawal')`, tid, x.ID).Scan(&denied)
	step["5_gate_denies_X_after_clear_under_old_version"] = denied
	// 6. is X re-enqueued for the new version?
	var reEnq int
	d, _ := timeIn(ctx, oc, tid, true, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT aml_screening_enqueue_due($1,1000)`, tid).Scan(&reEnq)
	})
	step["6_enqueue_due_count(rolled back)"] = reEnq
	step["6_enqueue_due_ms"] = float64(d.Microseconds()) / 1000

	// ---- part B: stress
	every := time.Duration(intExtra(sc, "policy_every_ms", 700)) * time.Millisecond
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
	window := time.Duration(sc.WindowSecs) * time.Second
	if window == 0 {
		window = 90 * time.Second
	}
	wctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	pc, err := openOwner(ctx, dbName)
	if err != nil {
		return nil, err
	}
	defer pc.Close(ctx)
	if err := setTenant(ctx, pc, tid); err != nil {
		return nil, err
	}
	if _, err := pc.Exec(ctx, `ALTER TABLE aml_screening_policies DISABLE TRIGGER aml_screening_policies_10_guard`); err != nil {
		return nil, err
	}
	var wg sync.WaitGroup
	inserted := 0
	if every > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(every)
			defer t.Stop()
			for {
				select {
				case <-wctx.Done():
					return
				case <-t.C:
				}
				if _, err := pc.Exec(ctx, `INSERT INTO aml_screening_policies (tenant_id,licensing_jurisdiction_id,lists,rescreen_interval_seconds,backfill_grace_seconds,effective_from,
					legal_review_reference,reason_code,created_by,created_by_person_id,created_by_scope,created_at)
					SELECT tenant_id, licensing_jurisdiction_id, lists, rescreen_interval_seconds, backfill_grace_seconds, clock_timestamp(), 'CAPA SYNTHETIC - NOT LEGAL','capa_event',
					       created_by,created_by_person_id,created_by_scope, clock_timestamp()
					  FROM aml_screening_policies WHERE tenant_id=$1 ORDER BY created_at DESC, id DESC LIMIT 1`, tid); err == nil {
					inserted++
				}
			}
		}()
	}
	recs := runLoop(wctx, w, prov, 0, window, 5*time.Minute)
	cancel()
	wg.Wait()
	if _, err := pc.Exec(ctx, `ALTER TABLE aml_screening_policies ENABLE TRIGGER aml_screening_policies_10_guard`); err != nil {
		return nil, err
	}
	var errs, lost, claimed, done int
	for _, r := range recs {
		errs += r.Stats.Errors
		lost += r.Stats.Lost
		claimed += r.Stats.Claimed
		done += r.Stats.Done
	}
	b := map[string]any{"window_s": window.Seconds(), "policy_versions_inserted": inserted, "passes": len(recs), "claimed": claimed, "done": done, "errors": errs, "lost": lost}
	var mismatch, listGap, expiredClaims, reclaimed int
	_ = setTenant(ctx, oc, tid)
	_ = oc.QueryRow(ctx, `SELECT count(*) FROM aml_screening_requests WHERE tenant_id=$1 AND status IN ('done','needs_officer') AND screened_policy_id IS DISTINCT FROM claim_policy_id AND claim_policy_id IS NOT NULL`, tid).Scan(&mismatch)
	_ = oc.QueryRow(ctx, `SELECT count(*) FROM aml_screening_requests r WHERE r.tenant_id=$1 AND r.status='done' AND r.claim_policy_id IS NOT NULL
		AND NOT (r.screened_lists @> (SELECT p.lists FROM aml_screening_policies p WHERE p.id=r.claim_policy_id))`, tid).Scan(&listGap)
	_ = oc.QueryRow(ctx, `SELECT count(*) FROM aml_screening_requests WHERE tenant_id=$1 AND status='claimed' AND lease_until <= clock_timestamp()`, tid).Scan(&expiredClaims)
	_ = oc.QueryRow(ctx, `SELECT count(*) FROM aml_screening_requests WHERE tenant_id=$1 AND attempts > 1`, tid).Scan(&reclaimed)
	b["invariant_done_with_policy_different_from_claim_policy(want 0)"] = mismatch
	b["invariant_done_whose_lists_do_not_cover_claim_policy_lists(want 0)"] = listGap
	b["claimed_with_expired_lease_at_end"] = expiredClaims
	b["requests_with_attempts_gt_1"] = reclaimed
	res["part_b"] = b
	return res, nil
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
