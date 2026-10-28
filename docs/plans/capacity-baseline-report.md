# Screening capacity baseline (CAP-A) - measured report

Revision 2 (after three independent reviews; section 12 maps every review item; sections 4.9, 7, 13 and appendix A are new or rewritten). Status: test-tooling and measurement only. **No migration, no production code, no behaviour change.** Every number below was produced by
`deploy/loadtest/screening/` (build tag `loadtest`) against **private `capa_*` PostgreSQL databases with synthetic data and a MOCK provider**.
Nothing here is a production capacity figure: it is one 4 vCPU / 16 GB machine with default PostgreSQL settings, one harness process, no network.
Labels: `MEASURED` = produced by a run listed here (3 runs unless stated); `HYPOTHESIS` = not established by a run; `EXTRAPOLATION` = arithmetic beyond measured sizes.

## 1. Summary

1. **MEASURED. The reviewer's function timings are reproduced, and they do not need concurrent load.** On an otherwise idle machine,
   `aml_screening_gate_overdue(tenant, 5000)` takes 5.3 s at 10,000 persons and `aml_screening_parked_overdue(..., 5000)` 5.4 s (production
   `ScreeningWorkerConfig.PhaseTimeout` is 5 s). `aml_screening_enqueue_due` is linear in the tenant's persons (0.17 ms/person): 1.7 s at 10k, 8.7 s at 50k, 33 s at 200k.
2. **MEASURED. Consequence in the real worker.** A tenant of about 5,000 persons or more sits AT the 5 s phase timeout (borderline, not a cliff): `gate_overdue(5000)` measured 4.8-5.5 s, and the worker's overdue check failed in all of our 10k-person passes (`Errors=1`, `Trapped=0`),
   while the DB-performance reviewer's idle machine timed out in 3 of 9 passes (`Trapped` 1,118-1,318 in the others). When the check times out the worker raises no trapped alert (it only counts an error and writes an untagged log line, section 4.9.4). With a 200k-person tenant (fleet 1 x 200k + 99 x 1k) the scheduled enqueue and the overdue check both time out
   in every pass: **the big tenant is enqueued 0 times and claims 0 requests** while still costing the pass about 10 s of timeouts.
3. **MEASURED. "About 1,440 claims/h/tenant" is an upper bound reached only by a single small tenant** (and only while the pass fits the 10 s interval). Measured per-tenant claims/hour:
   1,440 (1 tenant x 1k persons, provider <= 1 s), 1,105 (10 tenants), 110 (100 tenants x 1k persons, 0 ms provider), 24 (500 tenants; 246 tenants started per 300 s window). The cause is the
   per-tenant fixed cost of `gate_overdue` + `enqueue_due` (about 1.3 s per 1k-person tenant), because `RunPass` walks tenants serially.
4. **MEASURED. A mass-stale event drains at exactly the claim cap**: 24 persons/minute for a tenant (4 claims per 10 s pass), independent of the number of concurrent withdrawal callers.
   100 stale persons: 250 s to clear (3 of 3 runs). 2,000 stale persons: 72 cleared in 180 s; clearing all would take about 1.3 h (`EXTRAPOLATION`, linear). Gate callers keep getting 'deny' until their own turn in the drain.
5. **MEASURED. Safety properties held in every run**: a SIGKILL'd worker's claims were reclaimed under a new token after the lease (137 s after the kill), no double finish (a late finish with the old token applied 0 times),
   (duplicate idempotency keys: 0, but that is guaranteed by the unique constraint, so it is not evidence about the worker), and under a policy change every `done` request recorded the policy version of its claim (0 violations); the cost of a policy change between claim and finish is a failed finish (the request waits for its lease) not a wrong clear.
6. **MEASURED (growth) / validated candidates.** Policy versions make `aml_screening_since` super-linear (74.7 ms at 1,000 versions vs 0.8 ms at 1; `gate_denies` of a stale person 1.5 -> 31.6 ms).
   Candidate rewrites validated only as throwaway experiments on private databases with output-equivalence checks: window-function `since` (O(V log V)), guarded `since` in the enqueue candidate
   (3.3-3.8x faster), keyset-sliced enqueue evaluation (cost independent of tenant size, but a BEHAVIOUR CHANGE), set-based gate evaluation for the batch readers (14-21x faster, identical denied set). Section 7 lists exactly what the equivalence evidence covers and does not cover.
7. **MEASURED (review round) - new findings.** (a) A pass longer than the 10-minute heartbeat bound makes the tail finishes fail: mechanism confirmed, and a real 800 s pass (100 tenants, 5 s provider) completed 112 of 151 claims (4.9.1). (b) `TenantCap` 500: with 501 tenants, tenant #501 (by id) is never served (4.9.2). (c) The payments sweeper's own pass takes about 40 s for 100 tenants against its 15 s tick (4.9.3). (d) Several worker loops did not duplicate or lose anything and scaled the claim rate almost linearly (4.9.5). (e) A lost finish plus an idempotent-replay provider makes every reclaim fail (4.9.6). (f) A graceful SIGTERM leaves unprocessed claims to expire (4.9.7). (g) With a short backoff the retry storm reached `failed_terminal` for the 20 always-failing subjects and none was re-queued (4.9.8).

**Data-realism limits (read before using any number).** One done request per person (plus 4 extra old ones in one cell, which changed nothing: 4.9.9); `claim_policy_id` is always the version in force at claim time; one jurisdiction per tenant; a cache-resident database (the 200k tenant is 287 MB against 128 MB shared_buffers and 16 GB RAM); no multi-account persons; no writers other than the harness; a mock provider that always answers `clear`. Absolute timings in production will differ; ratios and growth shapes are the evidence.

## 2. Environment and method

| item | value |
|---|---|
| machine | 4 vCPU Intel Xeon @ 2.10 GHz, 16 GB RAM, kernel Linux 6.18.44-fc (v165 before the restart, v180 after), disk 252 GB |
| PostgreSQL | 16.15 (Ubuntu 16.15-0ubuntu0.24.04.1), **default settings**: shared_buffers 128MB, work_mem 4MB, maintenance_work_mem 64MB, effective_cache_size 4GB, max_connections 100, random_page_cost 4, jit on, max_parallel_workers_per_gather 2, track_io_timing off, fsync/synchronous_commit on, autovacuum on. Full list: `docs/plans/capacity-baseline-data/env-info.txt` |
| commit | harness (review-round additions in the later commits) at `HEAD` of branch `claude/capacity-baseline-gate-design` (from integration tip `01ea296`); report data produced with harness commits `c5b6546`, `866a82f`, `b794c4a` and the later commit that adds this report |
| date | 2026-10-27 (runs 16:50-23:50 UTC); review round 2026-10-28 (runs 00:26-01:50 UTC, same machine, same Postgres, nothing else running) |
| load / other work | load average at the start of each run is in the run log lines (0.1-1.0); only the harness, PostgreSQL and the agent process (3-4 % CPU) ran; no two benchmarks ran concurrently except where the scenario is concurrency |
| restart | **The container was restarted at about 21:18 UTC in the middle of the campaign** (processes stopped; disk and PostgreSQL data persisted). Results produced BEFORE the restart: sections 4.1-4.3 (function latency, growth, EXPLAIN, experiments), RunPass cells `run-t1-*`, `run-t10-l0..hang`, `run-t100-*`, `run-t500-l100`, `run-skew-l0`. Produced AFTER: `run-skew-l100`, `run-p10k-l0`, `run-t500-l0` (re-run), `run-t10-shared-pool`, `run-t10-ownerpool-gate`, mass-stale, retry storm, kill/recovery, policy-change races. The one run (`run-skew-l100` run 1) that was complete before the restart was discarded and all three runs repeated afterwards. No partial run is reported. |
| statistics | every measurement is run 3 times, each on a fresh clone of the seeded database. Tables show the median over runs of each run's own median, with the min-max across runs in brackets |

Data: synthetic (`md5(seed:kind:tenant:index)`), seed 42. A person's state is drawn from the scenario mix. "Steady" mix = 94 % clear, 3 % stale, 0.5 % legacy (a result without provenance), 0.5 % unresolved match, 0.5 % held, rest never screened;
"backlog" mix = 70 / 15 / 3 / 1 / 1 %. Policies: 3 screening-policy versions (both lists, 14-day re-screen interval) and 1 gating trigger-policy version (first effective 60 days ago, alert threshold 1 h) per tenant and jurisdiction.
Seeding is an owner-path bulk load on the scratch database (guard triggers and FORCE RLS switched off only during the load, restored and verified), see `deploy/loadtest/screening/README.md`.
Worker scenarios run the unmodified `aml.ScreeningWorker` as the runtime role with the production pool size (10) and `DefaultScreeningWorkerConfig` (EnqueueBatch 100, ClaimBatch 4, Lease 2 min, CallTimeout 10 s, PhaseTimeout 5 s, MaxAttempts 5, backoff 30 s..30 min), pass loop = `RunScreeningWorkerLoop` semantics (10 s ticker).
Function scenarios call the production SQL functions on an owner connection (mutating ones inside a rolled-back transaction).

Limits of the method: wait events of other roles' sessions are not visible to a non-superuser, so wait events are sampled only for harness (owner-role) sessions; `pg_locks` waits are visible for all.
`pg_stat_statements` / `auto_explain` are unavailable, so statement plans are manual reconstructions (section 5).

## 3. The reviewer's claims, reproduced

| claim | measured here | verdict |
|---|---|---|
| `aml_screening_gate_overdue(tenant,5000)` 3.9-5.8 s at 10k persons/tenant under load | 5.27 s idle (5.05-5.45), 5.15 s with 4 paced gate callers + 1 keyset reader (4.89-5.31); 1.04 s at 1k persons; 5.4-5.7 s at 50k/200k (saturates: only the first 5000 persons are scanned) | MEASURED, reproduced; does not need concurrency |
| `aml_screening_parked_overdue` batch of 5000 ~4.2-5.3 s | 5.41 s (5.23-5.84) at 10k, 5.4-5.6 s at 50k/200k. **The sweeper uses batches of 500: 0.49-0.58 s at every size** | reproduced for 5000; not the production batch |
| `aml_screening_enqueue_due(1000)` ~0.9-3.0 s | 1.86 s (1.73-1.95) at 10k; 0.18 s at 1k; 8.7 s at 50k; 33.2 s at 200k (linear, 0.17 ms/person; `enqueue_due(100)` costs the same) | reproduced at 10k, and worse above it |
| vs `PhaseTimeout` 5 s | reached by gate_overdue at ~4.8k-5k persons (borderline: 4.8-5.5 s measured against 5 s; failing in all our 10k passes, 3 of 9 on the reviewer's machine) and exceeded by enqueue_due at >= ~29k persons (EXTRAPOLATION of the linear fit between measured 10k and 50k, confirmed by the 200k RunPass below) | MEASURED (see 4.4) |
| claim drain 4 per 10 s pass => "about 1,440/h/tenant", only an upper bound because passes walk tenants serially | exactly 24 claims/min = 1,440/h when the pass fits 10 s; see 4.4: 110/h/tenant at 100 tenants | confirmed as an upper bound |

## 4. Measurements

### 4.1 (a) Function latency versus persons per tenant (1 tenant, steady mix, 3 policy versions)

| function (median ms; min-max across runs) | fn-1k | fn-10k | fn-10k-backlog | fn-10k-loaded | fn-50k | fn-200k |
|---|---|---|---|---|---|---|
| gate_denies, one clear person | 1.5 (1.4-1.7) | 1.5 (1.4-1.9) | 1.5 (1.2-1.9) | 1.3 (1.2-1.3) | 1.5 (1.3-1.6) | 1.2 (1.2-1.9) |
| gate_denies, one stale person | 1.6 (1.4-2.1) | 1.7 (1.4-2.7) | 1.5 (1.5-2.0) | 1.5 (1.4-1.9) | 1.6 (1.5-2.3) | 1.8 (1.5-2.3) |
| gate_denies, one none person | 1.0 (0.9-1.1) | 1.4 (1.1-1.4) | 1.0 (0.9-1.3) | 1.0 (0.8-1.2) | 0.8 (0.8-0.9) | 0.9 (0.9-1.1) |
| gate_denies, one legacy person | 1.8 (1.6-1.9) | 2.1 (2.0-2.4) | 1.4 (1.4-1.5) | 1.5 (1.3-1.8) | 2.2 (1.7-2.5) | 1.4 (1.4-1.7) |
| gate_denies, one needs_officer person | 0.9 (0.6-0.9) | 0.9 (0.8-1.0) | 0.9 (0.7-1.1) | 0.9 (0.6-1.1) | 0.8 (0.6-0.9) | 1.1 (0.7-1.1) |
| gate_denies, one held person | 1.7 (1.4-1.8) | 1.5 (1.3-1.8) | 1.3 (1.2-1.6) | 1.4 (1.4-1.7) | 1.6 (1.2-1.9) | 1.5 (1.4-2.0) |
| enqueue_due_100 | 175 (173-178) | 1,703 (1,698-1,704) | 1,723 (1,631-1,819) | 1,592 (1,567-1,647) | 8,688 (8,434-9,148) | 32,992 (32,517-35,201) |
| enqueue_due_1000 | 179 (161-199) | 1,858 (1,728-1,945) | 1,748 (1,688-1,944) | 1,877 (1,657-1,903) | 8,720 (8,449-9,123) | 33,179 (32,673-36,743) |
| claim_4_after_enqueue_100 | 3.2 (2.5-3.4) | 3.1 (2.4-3.4) | 2.7 (2.2-3.1) | 2.1 (2.1-2.3) | 2.5 (2.4-3.5) | 3.4 (3.2-3.4) |
| claim_4_after_enqueue_1000 | 2.3 (2.2-2.4) | 2.6 (2.5-2.6) | 2.4 (2.4-2.5) | 2.3 (2.1-2.7) | 2.9 (2.5-3.1) | 2.7 (2.6-3.5) |
| finish_clear | 2.3 (2.0-3.9) | 1.8 (1.6-2.6) | 1.8 (1.6-2.1) | 2.2 (1.4-2.3) | 2.0 (1.6-2.4) | 2.4 (2.3-2.4) |
| gate_overdue_500 | 530 (513-557) | 500 (493-548) | 555 (484-566) | 560 (488-562) | 525 (460-631) | 570 (556-607) |
| gate_overdue_5000 | 1,037 (945-1,040) | 5,265 (5,048-5,453) | 5,314 (5,215-5,327) | 5,153 (4,891-5,307) | 5,436 (5,303-5,642) | 5,677 (5,266-5,776) |
| parked_summary_7d | 1.0 (0.7-1.4) | 1.2 (1.1-1.9) | 3.1 (2.9-4.0) | 3.8 (3.0-4.4) | 2.8 (2.7-2.9) | 8.8 (8.0-12.2) |
| parked_overdue_500_first | 535 (500-642) | 537 (534-549) | 515 (505-531) | 520 (519-534) | 494 (488-604) | 579 (527-731) |
| parked_overdue_5000_first | 966 (965-1,041) | 5,410 (5,229-5,838) | 5,529 (5,421-5,612) | 5,124 (5,088-5,142) | 5,365 (4,855-5,877) | 5,578 (5,432-5,806) |
| parked_keyset_cycle_500 | 554 (483-555) /batch; cycle 1.17 s, 3 batches, complete=True | 564 (534-608) /batch; cycle 11.24 s, 21 batches, complete=True | 515 (502-541) /batch; cycle 11.16 s, 21 batches, complete=True | 532 (501-533) /batch; cycle 10.91 s, 21 batches, complete=True | 516 (509-537) /batch; cycle 53.28 s, 101 batches, complete=True | 528 (524-535) /batch; cycle 217.05 s, 401 batches, complete=True |

Per-call cost of the gate does **not** grow with tenant size (index lookups by person): 0.8-2.2 ms for every class at 1k...200k. Everything that scans persons grows linearly: `enqueue_due` 0.17 ms/person, `gate_overdue`
and `parked_overdue` about 1.05 ms per person scanned up to their limit (5000 / 500). The claim (2-3 ms) and finish (about 2 ms) are independent of tenant size and queue depth (100 vs 1000 queued).
`parked_keyset_cycle_500`: a batch is 0.5 s at any size; a full cycle over a tenant is P/500 batches (3, 21, 101, 401): back-to-back 1.2 s / 11 s / 53 s / 217 s, but the payments sweeper reads **one batch per pass**: with the 15 s tick alone that is 401 x 15 s = 100 minutes at 200k persons, which is only a LOWER BOUND - the real sweeper pass is serial over tenants and takes about 40 s for 100 tenants (4.9.3), so at fleet size the cycle is longer.
`parked_summary_7d` grows with the decision table (1 ms at 10k persons / 10k decisions, 8.8 ms at 200k persons); see 4.7 for decision volume.

DB/OS during these runs (probe objects in the result JSON, `docs/plans/capacity-baseline-data/results-json.tar.gz`): a single function call keeps one backend CPU-bound (OS CPU busy 25-27 % of 4 vCPU at 200k, i.e. one core), iowait 0,
shared-buffer hit ratio 0.992-1.000, no `pg_locks` wait in any probe window (0 of 490 windows over all runs). `enqueue_due` at 200k wrote 34.7 MB of temporary files in total across its repetitions (sort/hash spill at work_mem 4 MB); the
cost is CPU, not I/O. With 4 gate callers and a keyset reader running concurrently (`fn-10k-loaded`, 7 database sessions, CPU busy about 52 %) the function latencies are unchanged within noise (table, column `fn-10k-loaded`),
gate callers see a median 2.4 ms / p95 5.0 ms / max up to 307 ms per `aml_hold_active`, 19-20k calls per 3-run window, 77 % of calls denied (the backlog mix). Wait-event samples of the harness sessions: 100 % on CPU.

### 4.2 (b) Policy-version growth (1 tenant x 1000 persons)

| statement (median ms; min-max across runs) | growth-v1 | growth-v10 | growth-v100 | growth-v1000 |
|---|---|---|---|---|
| since_at_oldest(all_versions_later) | 0.7 (0.6-0.9) | 0.7 (0.6-0.7) | 2.2 (2.1-2.3) | 75.0 (72.0-75.2) |
| since_at_mid | 0.8 (0.7-0.9) | 1.0 (0.7-1.0) | 1.6 (1.2-1.7) | 27.5 (27.3-28.8) |
| since_at_recent_3d | 0.8 (0.7-0.9) | 0.7 (0.7-0.9) | 0.9 (0.8-1.0) | 2.5 (2.4-3.3) |
| since_prod_shape_after=mid_history_version | 0.7 (0.6-0.8) | 0.8 (0.8-1.2) | 1.4 (1.4-2.0) | 27.5 (26.2-43.2) |
| since_prod_shape_after=latest_version | 0.7 (0.6-0.8) | 0.8 (0.7-1.0) | 0.9 (0.9-1.3) | 3.6 (2.5-3.9) |
| covers | 0.1 (0.1-0.1) | 0.1 (0.1-0.1) | 0.1 (0.1-0.1) | 0.1 (0.1-0.2) |
| gate_denies_clear | 1.1 (1.1-1.2) | 1.2 (1.2-1.5) | 1.4 (1.4-1.4) | 3.7 (3.7-4.7) |
| gate_denies_stale | 1.8 (1.3-2.0) | 1.4 (1.4-1.4) | 2.2 (2.1-2.2) | 30.6 (29.0-30.8) |
| gate_denies_none | 0.9 (0.8-1.1) | 0.8 (0.8-0.9) | 0.8 (0.8-1.0) | 1.1 (1.1-1.7) |
| enqueue_due_100 | 154 (153-160) | 170 (165-188) | 361 (342-363) | 5,880 (5,849-5,922) |
| gate_overdue_500 | 498 (484-498) | 497 (451-516) | 610 (600-640) | 3,658 (3,636-3,717) |

MEASURED (table re-run in the review round, 3 runs, after the restart; the first campaign gave the same shape): the cost is a fixed overhead of about 0.7 ms plus a super-linear term. With every version later than `p_at` (`since_at_oldest`) it goes 0.7 ms (V=10) -> 2.2 (V=100) -> 75.0 ms (V=1000): about x34 net of the fixed part for x10 versions, i.e. between linear and quadratic (a self-correlated NOT EXISTS over an index scan). 'Quadratic plus a fixed overhead' is the accurate description, not pure O(V^2).
**Call shapes.** The first three `since_*` rows pass `p_after_policy = NULL` (and `explain/20_since.sql` replaces the OR branch of migration 0194 with `OR FALSE`). The production gate calls `since(.., LEAST(claimed_at, completed_at, screened_at), lists, claim_policy_id)` with the claim-time version, so the two `since_prod_shape` rows were ADDED in the review round: with a claim version from the middle of the history 27.5 ms at V=1000 (1.4 ms at V=100), with the latest version 3.6 ms (0.9 ms at V=100). The reviewer's figures (27 ms against 2.8 ms; about 1.7 ms at V=100) are reproduced. Fresh clears (`p_at` 3 days ago, claim version recent) stay cheap. Effect: `gate_denies` of a stale person 31.6 ms at V=1000,
`enqueue_due(100)` on 1000 persons 6.3 s (x40 vs V=1), `gate_overdue(500)` 3.8 s. At V <= 100 the effect is under 3x. `covers` is constant (0.1-0.2 ms). The actual number of versions per tenant/jurisdiction in production is unknown (HYPOTHESIS: tens, not hundreds).

### 4.3 (h) Locks, CPU, I/O, buffers

See 4.1 (function level) and the `probe` object in each result JSON for every scenario (pg_stat_database / pg_stat_user_tables / pg_stat_user_indexes deltas, `/proc/stat` shares, load average, sampled `pg_locks` waits, connections by role).
Across all scenarios: **no lock wait was ever sampled** (`samples_with_lock_wait` = 0 in all 490 probe windows, including 20 closed-loop gate callers sharing a pool with the worker), no deadlock, `deadlocks` delta 0.
Seq scans seen are on tiny tables (`aml_screening_policies`, `aml_screening_trigger_policies`: 3-row tables, millions of scans, irrelevant) and on `aml_enforcement_decisions` (see 4.7) and `player_accounts` (the DISTINCT person list).

### 4.4 (c) Full `RunPass` (real worker, runtime role, pool 10, MOCK provider)

Fleet cells use 1k persons per tenant (backlog mix) unless stated. "capped": a pass was cut at the stated cap (60-300 s) and the table shows how many tenants it had started. Claims/h/tenant = claims / rate window / number of tenants,
rate window = start of the first pass to the end of the last pass slot (last start + max(duration, 10 s)). Provider latency columns: l0 = 0 ms, l100 = 100 ms, l1s = 1 s, l5s = 5 s, hang = never answers (the 10 s `CallTimeout` decides).

| scenario | passes complete / capped (per run) | pass duration s, complete passes (median; min-max across runs) | tenants started per pass (median) | claims | claims/h/tenant measured (min-max) | errors | max pool conns acquired | OS cpu busy % | buffer hit |
|---|---|---|---|---|---|---|---|---|---|
| run-t1-l0 | 3 / 0 | 1.2 (1.2-1.3) | 1 of 1 | 12 | 1440 (1440-1440) | 0 | 1 | 5 | 0.9935 |
| run-t1-l100 | 3 / 0 | 1.7 (1.7-1.9) | 1 of 1 | 12 | 1440 (1440-1440) | 0 | 1 | 5 | 0.9935 |
| run-t1-l1s | 3 / 0 | 5.3 (5.3-5.3) | 1 of 1 | 12 | 1440 (1440-1440) | 0 | 1 | 4 | 0.9935 |
| run-t1-l5s | 3 / 0 | 21.3 (21.2-21.3) | 1 of 1 | 12 | 677 (677-678) | 0 | 1 | 2 | 0.9936 |
| run-t1-hang | 3 / 0 | 41.4 (41.4-41.4) | 1 of 1 | 12 | 348 (348-348) | 0 | 1 | 2 | 0.9938 |
| run-t10-l0 | 3 / 0 | 13.3 (13.1-13.3) | 10 of 10 | 120 | 1105 (1067-1107) | 0 | 1 | 25 | 0.9986 |
| run-t10-l100 | 3 / 0 | 16.7 (16.3-16.9) | 10 of 10 | 120 | 859 (850-882) | 0 | 1 | 19 | 0.9986 |
| run-t10-l1s | 2 / 0 | 52.9 (52.2-53.7) | 10 of 10 | 80 | 269 (268-273) | 0 | 1 | 7 | 0.9979 |
| run-t10-l5s | 0 / 2 | none completed (capped) | 3 of 10 | 24 | 72 (72-72) | 4 | 1 | 2 | 0.9922 |
| run-t10-hang | 0 / 2 | none completed (capped) | 2 of 10 | 12 | 36 (36-36) | 0 | 1 | 1 | 0.9876 |
| run-t100-l0 | 2 / 0 | 130.2 (126.9-130.3) | 100 of 100 | 800 | 110 (109-110) | 0 | 1 | 26 | 0.9989 |
| run-t100-l100 | 0 / 2 | none completed (capped) | 85 of 100 | 684 | 82 (80-83) | 2 | 1 | 20 | 0.9988 |
| run-t100-l1s | 0 / 2 | none completed (capped) | 12 of 100 | 89 | 27 (27-27) | 1 | 1 | 7 | 0.9955 |
| run-t100-l5s | 0 / 2 | none completed (capped) | 3 of 100 | 24 | 7 (7-7) | 4 | 1 | 2 | 0.9864 |
| run-t100-hang | 0 / 2 | none completed (capped) | 2 of 100 | 12 | 4 (4-4) | 0 | 1 | 2 | 0.9799 |
| run-t500-l0 | 0 / 2 | none completed (capped) | 246 of 500 | 1964 | 24 (24-24) | 2 | 1 | 26 | 0.9956 |
| run-t500-l100 | 0 / 2 | none completed (capped) | 68 of 500 | 529 | 16 (16-16) | 1 | 1 | 20 | 0.9938 |
| run-skew-l0 | 2 / 0 | 136.0 (126.0-144.3) | 100 of 100 | 792 | 101 (95-113) | 4 | 2 | 26 | 0.9942 |
| run-skew-l100 | 2 / 0 | 172.8 (172.1-173.4) | 100 of 100 | 792 | 82 (82-82) | 4 | 2 | 20 | 0.9942 |
| run-p10k-l0 | 3 / 0 | 6.6 (6.6-6.7) | 1 of 1 | 12 | 1440 (1440-1440) | 3 | 1 | 19 | 0.9974 |
| run-t10-shared-pool | 3 / 0 | 24.7 (24.0-27.0) | 10 of 10 | 120 | 588 (527-600) | 0 | 10 | 94 | 0.9996 |
| run-t10-ownerpool-gate | 3 / 0 | 16.3 (16.2-16.4) | 10 of 10 | 120 | 881 (880-889) | 0 | 1 | 53 | 0.9993 |

Reading (MEASURED unless noted):
* Pass time is dominated by the per-tenant fixed cost, not by the provider, until the provider is slow: 1.3 s per 1k-person tenant at 0 ms (t10: 13.3 s, t100: 130 s). That is `enqueue_due` (0.18 s) + `gate_overdue` (1.0 s) + 4 claims.
  A pass therefore exceeds the 10 s interval from 8-10 tenants of 1k persons on; the loop then runs passes back to back (no idle time) and each tenant is served once per pass.
* Provider latency adds 4 x latency per tenant serially: at 100 ms the t100 pass was capped (85 of 100 tenants in 150 s, i.e. about 176 s per pass `EXTRAPOLATION` from tenants/s); at 1 s a pass of 100 tenants would take about 8-9 min (12 tenants started in 60 s); at 5 s per call a single tenant's pass takes 21.3 s and delivers 677 claims/h; a hung provider (10 s `CallTimeout`) gives 41.4 s per tenant pass = 348 claims/h for ONE tenant and 4/h/tenant at 100 tenants.
* One 10k-person tenant (`run-p10k-l0`): pass 6.6 s, `Errors=1` with `Trapped=0` in all 9 passes (3 runs x 3) - the 5 s overdue check is borderline (4.8-5.5 s) and lost every time here; the reviewer's machine passed it in 6 of 9. When it times out no trapped alert is raised.
* Skewed fleet (1 x 200k + 99 x 1k): pass 136 s (0 ms) / 173 s (100 ms); `Enqueued=9900` = 99 x 100: the 200k tenant is **never enqueued and claims nothing** (enqueue times out at 5 s, overdue check times out at 5 s; 2 errors per pass). Its persons only get screened through the registration / KYC-approval triggers, never by the scheduled re-screen or the backfill.
* Connections: one worker uses 1 pool connection at a time (max acquired 1-2 of 10). **MEASURED (i), the production-like topology:** `cmd/platform-api/main.go` starts the worker in every API replica on that replica's shared pool (default 10 connections), so request traffic and the worker compete for one pool. When 20 closed-loop gate callers share the worker's 10-connection pool (`run-t10-shared-pool`; its gate p95 of 15 ms is above the proposed T5 of 10 ms), all 10 connections are in use, 151k-173k of the acquires waited for a connection (792-903 s of cumulative wait in a 75 s window), gate calls rose from 2.2 ms to a 9.1 ms median (p95 15 ms, max 100-182 ms) and the worker pass went from 16.3 s to 24.7 s. With the callers on a separate pool (`run-t10-ownerpool-gate`) the worker is unaffected (pass 16.3 s) while the database saw 11 + 1 sessions (limit `max_connections` 100).

### 4.5 (d) Mass-stale event (list added) and drain

1 tenant, a policy version adding the `pep` list makes every clear stale. Worker with default config, 100 ms MOCK provider; backlog = persons the gate denies right after the event.

| scenario | backlog denied after event | worker cfg | done in window | drain rate /min (min-max) | pass duration s (median; max) | denied at end | time to clear | gate caller median / p95 / max ms | gate denied share | errors |
|---|---|---|---|---|---|---|---|---|---|---|
| stale-100-drain | 100 | batch 4, lease 120s | 100 | 24.0 (24.0-24.0) | 0.6; 0.7 | 0 | 250 s measured, 250 s measured, 250 s measured | - | - | 3 (3 runs) |
| stale-100-drain-gate | 100 | batch 4, lease 120s | 100 | 24.0 (24.0-24.0) | 0.6; 0.6 | 0 | 250 s measured, 250 s measured, 250 s measured | 2.2 / 3.7 / 93 | 0.48 | 3 (3 runs) |
| stale-2k-default | 2000 | batch 4, lease 120s | 72 | 24.0 (24.0-24.0) | 3.4; 3.7 | 1928 | 1.3 h EXTRAPOLATED, 1.3 h EXTRAPOLATED, 1.3 h EXTRAPOLATED | - | - | 0 (3 runs) |

* Drain rate is 24.0 persons/min in every run (min = max): `ClaimBatch` 4 x 6 passes/min. Provider latency 100 ms and the gate callers do not change it.
* 100 stale persons cleared in 250 s in 3 of 3 runs, with and without 4 paced (20 ms) gate callers. With callers, 48 % of their calls were denied overall; the denied share of calls fell from 93 % (first 30 s) to 0 % (last bucket): **a person's gate evaluation stays 'deny' until its own turn in the queue**, so the time a person stays gated is spread uniformly over 0..backlog/24 min. What the denial means differs by operation (ledger review L1): a withdrawal REQUEST is refused (the funds stay in the wallet and can still be wagered), whereas an already approved PAYOUT is parked and re-evaluated later.
* 2,000 stale persons: 72 cleared in the 180 s window; 1,928 still denied. Full drain about 83 min (`EXTRAPOLATION`: 2000 / 24 per min). For a 10,000-person event 6.9 h; for 200k persons (where the scheduled enqueue does not even run, see above) not applicable.
* `ValidateForLoop` couples the lease to the batch: Lease >= ClaimBatch x (3 x PhaseTimeout + CallTimeout) + 5 s = 25 s x batch + 5 s, so the default 120 s lease supports at most batch 4, and the largest batch under the 1 h lease limit is 143. Raising the drain rate by batch therefore needs a longer lease (HYPOTHESIS: batch 20 / lease 600 s gives about 5x the rate; scenario `stale-2k-batch20` exists but was NOT run, see section 9).
* Measured while draining, before the event 40 of 2,000 persons were denied; evaluating the whole tenant takes 2.4 s (about 1.2 ms per person).

### 4.6 (e) Retry storm and (f) worker failure / recovery

**Retry storm** (`retry-storm`, 3 runs, 300 persons backlog mix, provider failing 100 % for 240 s then recovering; additionally 30 % of subjects always fail; default retry config, 600 s window):
* All runs identical: 78 requests pending at start; during the outage the worker gave each request its first attempt at the cap of 4 claims per pass (19 passes = 190 s before the first retry backoff expired), 150 of 208 provider calls failed, 0 starvation: failing requests do not block others (claims are ordered by `next_attempt_at`, a retried request leaves the due set for its backoff).
* After recovery 58 requests were cleared (251 -> 309 done) over the next 240 s and 20 requests (the always-failing subjects) remained pending with `last_error_code=unavailable`, attempts 3-4 at the end of the window.
* **NOT observed in this window**: the `failed_terminal` transition (needs attempt 5, backoff 30+60+120+240 s plus queue turns > 600 s), the terminal alert (`persons_with_failed_terminal=0`, no alert rows), and whether a terminal person is re-queued (`enqueue_due` would enqueue 0 now because the 20 still hold open requests). From the code (HYPOTHESIS, not measured): the scheduled idempotency key is per re-screen-interval bucket and policy version, so a person whose request ended `failed_terminal` is not re-enqueued until the next bucket (up to one re-screen interval) or a new policy version; the gate keeps denying meanwhile. This needs a longer run or a compressed-backoff run to confirm.

**Kill and recovery** (`crash-kill`, 3 runs; the worker child process is killed with SIGKILL by pid while 4 claims are held and a provider call (8 s latency) is in flight):
* The 4 claimed requests stayed `claimed` after the kill; they were reclaimed by the restarted worker (restarted 5 s after the kill) **137.2-137.3 s after the kill** (lease 120 s + pass tick), each under a NEW claim token with `attempts` 1 -> 2, and finished one per call (8 s each): done at 145, 153, 161, 169 s.
* No double finish: a `finish` with the OLD token applied 0 times (3 of 3 runs); 0 duplicate idempotency keys; 0 requests above `MaxAttempts`. Backlog denied persons 135 -> 110 over the 210 s observation window.
* Time-to-recover for persons whose requests were in flight is therefore lease + one pass (about 2.3 min) + their serial processing; nothing else in the queue is blocked meanwhile (other pending requests are claimed normally by the restarted worker).

### 4.7 (g) Concurrent policy change during claim / finish

`race-control` (no policy inserts) vs `race-policy-churn` (an extreme setting: a new policy version every 400 ms, 224 inserted in 90 s), 3 runs each, plus a deterministic interleaving:
* Deterministic (3 of 3 runs): after a claim, a stricter version (list added) is inserted; a finish naming the NEW version is **refused** by the database ("not the one in force when claimed", `AT001`); a finish naming the claim-time version is applied and records the claim-time provenance; the gate then denies the person (the new version requires the added list) and `enqueue_due` re-queues them under the new version.
* Stress: control 36 claims -> 36 done, 0 errors. Churn: 36 claims -> 28-33 done and 11-17 phase errors per run (the worker reads the newest version in phase A, the claim recorded the older one, so finish raises; the request stays `claimed` until its lease expires and is reclaimed under the new version). Invariants checked in the database after every churn run: done requests whose policy differs from the claim policy: 0; done requests whose lists do not cover the claim policy's lists: 0. So the race is fail-closed and self-healing but costs a lease (2 min) per hit and one wasted provider call; at a realistic change rate (a few per year) it is negligible, at the extreme rate above 11-17 of 36 claims per run hit it.

### 4.8 Decision-volume sensitivity (EXPERIMENT, `experiments/08_decisions_scale.sql`, 1 run)

`aml_screening_parked_summary` and `parked_overdue` read `aml_enforcement_decisions` of the last 7 days (`DISTINCT ON (person_id)` sort). With the fn-10k tenant and N synthetic payout decisions: 1M rows -> summary 1.5 s (sort spilled 64 MB to disk), 4M rows -> 6.0-6.7 s (spill 258 MB), i.e. **over the 5 s phase timeout at about 3.3M decisions in the window** (single run, labelled exploratory). How fast decisions accumulate in production is a HYPOTHESIS (code writes one decision per hold-involved enforcement evaluation; a parked payout re-evaluated every sweeper pass would write about 5,760 rows/day per parked payout). `SET work_mem='256MB'` did not help at 4M rows (still `external merge`, 258 MB; 6.7 s -> 8.3 s).

### 4.9 Review-round measurements (all after the restart; 3 runs unless stated)

**4.9.1 Heartbeat bound (ops C1, code fact CONFIRMED).** `RunPass` writes `screening_worker_heartbeat` once at its start (`screening_worker.go`); `aml_screening_request_finish` for a clear/match calls `aml_screening_assert_worker_provider`, which needs a heartbeat younger than 10 minutes.
* Mechanism (`experiments/11_heartbeat_expiry.sql`, table owner on a private DB): heartbeat aged to 11 minutes -> `finish(clear)` raises ("the provider is not the one a running screening worker reported", AT001); `finish(retry)` is still applied; with a fresh heartbeat the clear is applied. (Output: `explain/heartbeat-expiry.txt`.)
* Real pass (`run-t100-l5s-long`, 1 run, 100 tenants x 1k persons, 5 s provider, pass capped at 800 s): 38 tenants started, 151 claims, only 112 done (28 tenants x 4 = the tenants finished before about 600 s), 38 errors, `Trapped` 7,099. The tail tenants' finishes failed and their requests stay claimed until the lease ends; each failed finish leaves an attempt burnt. A fleet whose pass exceeds ~600 s (the extrapolated 500-tenant pass is ~610 s) therefore stops clearing people in the tail of the pass.
* Interaction: the pass time is the bound that matters for the heartbeat, not the 10 s tick.

**4.9.2 `TenantCap` (ops H1).** `activeTenants` is `ORDER BY id LIMIT TenantCap (500)` with no rotation, no log and no metric. 501 active tenants x 3 stale persons (`tenantcap-501`, 2 runs): the pass served 500 tenants (1,500 claims, 1,500 done, 9.8 s) and **tenant #501 by id received no enqueue and no claim in either run**; every pass restarts at tenant 1 so it is never served. The payments sweeper, by contrast, rotates by offset. (Tenants without screening work are not affected; the cap would first hit the highest ids.)

**4.9.3 Payments sweeper pass at fleet scale (ops H2).** The REAL `payments.Sweeper.RunPass` (nil orchestrator: the scratch database has no payment attempts, so only the per-tenant overhead is measured, which includes the screening-parked keyset read of up to 500 persons per tenant and the claim query):

| fleet | pass duration s (median; min-max over 3 runs x passes) | scanned persons per tenant per pass |
|---|---|---|
| 1 tenant x 10k persons | 0.5 (0.2-0.6) | 500 |
| 10 tenants x 1k | 4.4 (3.0-5.2) | 410 |
| 100 tenants x 1k | 39.6 (35.0-41.5) | 383 |
| skewed 1 x 200k + 99 x 1k | 39.2 (34.7-40.2) | 377 |

About 0.4 s per tenant, so a pass over 100 tenants is ~40 s against the 15 s tick, and 10 tenants already exceed it. The sweeper pass runs before each tenant's payment claim (`sweeper_loop.go`), so this time is added in front of every tenant's payout work (it is serial and shared with payments). A full per-tenant observation cycle is therefore (persons/500) sweeper passes - 'lower bound 100 min' for 200k persons in earlier text was the tick-only figure; with 100 tenants it is at least 401 x 40 s = 4.5 h (EXTRAPOLATION). CPU busy about 25 % (one core).

**4.9.4 Is the worker's trapped alert silently never raised? (ops H4, precise answer).** Worker path (`alertTrapped`): if `aml_screening_gate_overdue` errors (timeout included) it does `st.Errors++` and `w.logErr("overdue check failed")` - an `slog` error line with no tenant id (`aml screening worker: overdue check failed`), and the pass summary line carries `errors=N`; no alert, no metric. If it succeeds with n > 0 it raises `aml.screening_gate_trapped` (source `worker`). So: not raised, but not entirely silent (counter + untagged log line) - and at ~5k+ persons that is the state a tenant is in much of the time. The sweeper path raises the same alert kind (source `sweeper`) from its independent keyset read when overdue persons are found; alert DELIVERY to owners is the open item ALERT-DELIVERY-1 (docs/plans/c3-record.md, a11 record), which I did not re-verify here.

**4.9.5 Several workers (ops H3; the production topology is a worker per API replica).** `run-t10-w2/w3`: 2 and 3 worker loops, each with its own 10-connection pool, on 10 tenants, 100 ms provider, 3 passes each:

| workers | pass s (median) | claims/h/tenant | OS cpu busy % | `pg_locks` waits | requests created | provider calls vs sum of attempts | persons with >1 scheduled request per bucket | lost / errors |
|---|---|---|---|---|---|---|---|---|
| 1 (earlier `run-t10-l100`) | 16.7 | 859 | 19 | 0 | - | - | - | 0 / 0 |
| 2 | 16.4 | 1,753 (1,747-1,760) | 38 | 0 | 2,805 | 240 = 240 | 0 | 0 / 0 |
| 3 | 16.2 | 2,635 (2,621-2,641) | 56 | 0 | 2,805 | 360 = 360 | 0 | 0 / 0 |

Assertions that held in all 6 runs: one provider call per claim (calls <= sum of attempts, equal here), every claimed request ended `done` exactly once (done = claims), no `lost` finish, no duplicate scheduled request for a person and bucket across keys (0), no lock waits. The claim rate scaled almost linearly with workers because `FOR UPDATE SKIP LOCKED` lets the workers take different requests; the per-pass fixed cost (enqueue/overdue per tenant) is repeated by every worker (CPU 19 -> 56 %), i.e. N workers do N times the scan work. 'One terminal transition per request' is enforced by the transition guard and the claim-token CAS, so the run proves no violation was observed, not that violations are impossible.

**4.9.6 Idempotent provider replay after a lost finish (ledger M4; PROVIDER DEPENDENT).** A mock provider that replays the first answer (including `ScreenedAt`) per idempotency key; 4 claims whose finish is lost (the worker died after the answer), lease 125 s, reclaim by a running worker (`replay-4`, 2 runs): at the first reclaim (131 s after the loss) 4 of 4 finishes failed (pass `Errors=4`, `Done=0`), attempts 2, requests still `claimed`; at the second reclaim (262 s) again 4 of 4, attempts 3. The finish function refuses a screening time more than 120 s before the claim, and the replayed time is older than the lease. Consequence (fail-closed): such requests never clear and burn attempts until `failed_terminal`. The error code itself was not captured by the harness (the worker logs no code); the cause is inferred from the function body and from the fact that fresh answers succeed in the same pass loop. A real vendor's replay semantics are unknown.

**4.9.7 Rolling restart (ops M4): SIGTERM with 4 claims in flight** (`crash-term`, 3 runs, 8 s provider latency, restart 5 s later): the worker stops after the request in progress; that one request is ended with a retry (its attempt is spent, backoff 30 s); **the other 3 claims are left `claimed`** and were reclaimed 137.3 s after the signal (lease 120 s + a pass), each with `attempts` 1 -> 2 although they never reached the provider. One of them finished at 145 s; the others were still being processed (8 s each, serially) at the end of the 150 s window. Graceful shutdown does not release unprocessed claims. (SIGKILL results were in 4.6.)

**4.9.8 Retry storm to `failed_terminal` (ops M2; backoff base 3 s instead of 30 s, STATED).** `retry-fast` (2 runs, 300 s) and `retry-fast-long` (1 run, 600 s): provider failing 100 % for 60 s, then recovering; 30 % of subjects always fail. In the 300 s window the always-failing 20 requests were still pending at attempts 3-4. In the 600 s run they reached attempt 5 and **20 requests ended `failed_terminal` at about 410-450 s**; the 58 recoverable requests had cleared by about 290 s; afterwards `enqueue_due` would enqueue 0 for them (the scheduled key is per bucket and policy version) and the 20 persons stayed denied (`denied_after` 25 = 20 terminal + 5 unresolved matches). The pace of retries is set by queue position (FIFO by `next_attempt_at`), not by the backoff, while a backlog exists. With the DEFAULT backoff (30+60+120+240 s before attempt 5, no jitter, no circuit breaker) an outage of about 8-10 minutes while requests are in flight ends every in-flight request `failed_terminal` (arithmetic from the code; the default-backoff run to terminal was not performed). Alert rows were not visible to the harness's read (NOT verified).

**4.9.9 Richer history (data-realism M2).** `fn-10k-hist`: 4 extra old done requests per clear/stale person (10k persons, 3 runs): `gate_overdue(5000)` 4,975 ms (4,951-5,009), `enqueue_due(1000)` 1,865 ms, `gate_denies` stale 2.1 ms (1.7 before), clear 1.2 ms: no material change from the extra history at this size.

## 5. EXPLAIN (ANALYZE, BUFFERS) - manual reconstructions

Files: `deploy/loadtest/screening/explain/*.sql` (exact SQL), outputs `docs/plans/capacity-baseline-data/explain/*.txt`. **These are reconstructions of the statements inside the plpgsql functions with literals from the data, not captured plans of the functions.**
Representativeness (reconstruction vs real function, same data): `enqueue_due` candidate INSERT 1,969 ms vs function 1,612-1,695 ms at 10k (EXPLAIN instrumentation overhead); 32,887 ms vs 33,038-34,090 ms at 200k.
Excerpts (10k persons, 4 MB work_mem):

* `enqueue_due` (E3): `Nested Loop Anti Join / Hash Anti Join / Subquery Scan c ... rows=10000 loops=1` with **SubPlan 1** (`max(completed_at)` union, 0.030 ms x 10,000 loops, Index Only Scan on `aml_screening_requests_person`) and **SubPlan 2** (the covering EXISTS: `Index Scan using aml_screening_requests_person ... Filter: ... aml_screening_covers(... aml_screening_since(...))`, **0.151 ms x 10,000 loops = 1.5 s of 1.9 s**). Nothing is a seq scan of the big tables; the cost is one function call to `since()` + `covers()` per person, executed for every person before `LIMIT`.
* `gate_denies` inner statements (CORRECTED after the DB-performance review; my first text called the cost 'plpgsql overhead, not one heavy statement', which the plan file `fn-10k.10_gate_denies.txt` contradicts): for a stale person the covering `NOT EXISTS` (first branch, with `aml_screening_covers(... aml_screening_since(...))` in its filter) is the single largest statement, 0.88 ms of a 0.9-1.4 ms call; the FIRST `since()` call of a `gate_denies` costs 0.25-0.34 ms (one-shot) against 0.06-0.11 ms amortised in set-based loops (reviewer's measurement; consistent with the plan); the other statements are 0.02-0.15 ms index probes. So guarding or replacing `since()` helps the per-person gate path as well as `enqueue_due`; per-call SPI overhead of ~10 statements is the remainder, not the whole. Seq scans only on the 1-3 row policy tables.
* `since` body at V=1000: `Index Scan on aml_screening_policies ... SubPlan NOT EXISTS ... loops=1000` = 72.6 ms (O(V^2)); the same at p_at = 3 days ago 2.2 ms.
* `parked_overdue`: batch read `Unique / Sort / Index Scan player_accounts` 1-4 ms; decisions read for the denied persons and open requests 1-11 ms; the 0.5 s is the 500 `gate_denies` calls.
* Buffers: warm (`shared hit`), 0 reads after warm-up; JIT compiles 60 functions for the enqueue statement (45 ms of 1.97 s, 2 %). Temporary files: only the decision-volume sorts (4.8) and the enqueue sort at 200k.
* Larger `work_mem` (hypothesis only, session `SET`): not useful - the CPU-bound statements have no spill at 10k; the one real spill (decisions) did not go away at 256 MB.

## 6. Bottlenecks ranked by measured contribution

| # | bottleneck | measured contribution | label (rates and shapes MEASURED; a duration marked EXTRAPOLATED is arithmetic beyond measured sizes) |
|---|---|---|---|
| 1 | **Per-person plpgsql gate evaluation in the batch readers** (`gate_overdue` limit 5000, `parked_overdue`): 1.0-1.1 ms/person, 5000 persons = 5.2-5.4 s > PhaseTimeout | 80 % of the per-tenant cost of a 1k-tenant pass (1.04 of 1.3 s); sits at the 5 s timeout (borderline) from ~4.8k persons and failed in all our 10k passes; the worker's trapped count is partial and the alert can be missed | MEASURED |
| 2 | **`enqueue_due` is O(persons) per call**, per pass, evaluating `since()`+`covers()` for every person before the LIMIT | 0.17 ms/person: 1.7 / 8.7 / 33 s at 10k / 50k / 200k; times out (and never enqueues) above ~29k persons; 0.18 of 1.3 s per 1k tenant | MEASURED (29k: EXTRAPOLATION, confirmed qualitatively by the 200k tenant) |
| 3 | **Serial tenant walk with a fixed per-tenant cost** | pass = sum over tenants; 100 tenants x 1k: 130 s, 110 claims/h/tenant (13x below nominal); 500 tenants: ~610 s (EXTRAPOLATION from 246 tenants / 300 s) | MEASURED |
| 4 | **Claim throughput cap** 4 per pass per tenant, lease-coupled | 24 persons/min/tenant; 2,000-person event = ~83 min (EXTRAPOLATION), 10k = 6.9 h | MEASURED rate / EXTRAPOLATED durations |
| 5 | **Provider latency is serial** (4 calls x latency per tenant per pass) | +0.4 s (100 ms) ... +40 s (hung) per tenant per pass; single tenant at 5 s: 677 claims/h | MEASURED |
| 6 | **Policy-version growth** in `since()` | negligible at V <= 100, 20x on `gate_denies` of stale persons and 40x on `enqueue_due` at V = 1000 | MEASURED |
| 7 | **Sweeper observation cycle** = P/500 batches x one sweeper PASS (not the 15 s tick: a 100-tenant pass takes ~40 s, 4.9.3) | LOWER BOUND using only the tick: 5 min at 10k, 25 min at 50k, ~100 min at 200k; longer at fleet size | MEASURED batch counts and pass time; cycle durations EXTRAPOLATED |
| 8 | `aml_enforcement_decisions` volume in `parked_summary` | exploratory: 6 s at 4M rows | HYPOTHESIS about production volume; one run |
| 9 | Shared 10-connection pool under gate-caller load | worker pass +50 %, gate calls 4x slower in the test | MEASURED (test shape: 20 closed-loop callers) |
| - | locks, I/O, buffer misses, work_mem | none observed | MEASURED (negative) |

Other MEASURED observation: `gate_overdue(5000)` evaluates `DISTINCT person_id ... LIMIT 5000` with **no ORDER BY**: on a 10,000-person tenant it saw 1,419 overdue persons where limit 20,000 sees 2,894 (49 %; `experiments/10_overdue_coverage.sql`). The worker's trapped count is therefore a partial, arbitrary sample on tenants above 5,000 persons (the sweeper's keyset read is the complete one).

## 7. Smallest safe changes - validated only as throwaway experiments

None of this is implemented, and nothing here is a design decision. SQL is in `deploy/loadtest/screening/experiments/`; each ran on a private database. Equivalence = identical output on the same data. **Ledger-finance veto (accepted):** the bodies of `aml_screening_since`, `aml_screening_gate_denies` and `aml_hold_active` are on the money path (withdrawal/payout gate); no candidate may replace them in place. Every candidate below is a NEW function name, used by MONITORS ONLY first (`gate_overdue`, `parked_overdue`, the enqueue candidate); the live gate keeps the existing bodies until a separately reviewed migration says otherwise, and claim batch / lease are not to be raised in the next slice.

| change | experiment | before -> after | equivalence |
|---|---|---|---|
| E1 a NEW function (not a replacement of the live `aml_screening_since`; monitors only first) without the correlated NOT EXISTS (window minimum over peer groups of equal created_at, `GROUPS` frame) | `01`,`02` | removes the V^2 term (74.7 ms at V=1000; after: not timed separately inside the sets below) | 6 random policy histories (4 tenants x 3 jurisdictions, ties in created_at, future/backdated effective_from, foreign after-policy ids, NULL/empty lists) x 6,000 random inputs each: 0 mismatches in 36,000 inputs |
| E2 guard before `since()` in the `enqueue_due` candidate: skip the call when `p_at` is not before the latest started version and the claim used that version (result is provably NULL) | `06` | 1.83 -> 0.55 s (10k), 8.4 -> 2.5 s (50k), 32.2 -> 8.4 s (200k): 3.3-3.8x | full candidate relation (person, last_any, covered) identical at 10k / 50k / 200k persons (0 rows differ either way) |
| E3 evaluate the enqueue candidates per keyset slice (2,000 persons per call). **BEHAVIOUR CHANGE, not an optimisation:** the production statement orders ALL candidates of the tenant by `last_any NULLS FIRST, person_id` before its LIMIT (oldest-screened first across the tenant); a slice orders only within the slice, so that global priority is lost unless a persisted cursor and a new ordering rule are decided by the owner | `07` | 2,000-person slice 76 ms, 500-person 25 ms at a 50k tenant (full: 2.5 s guarded / 8.7 s original); cycle of 25 calls | the union of slices equals the full relation, which is true by construction (each person's row is independent) and is NOT evidence about ordering or selection |
| E4 set-based gate evaluation for a person set (it returns the denied SET; a monitor would have to produce the same outputs as `gate_overdue`/`parked_overdue`, which I did not compare) (one SQL instead of one plpgsql call per person), with E1 + the guard | `03`,`04`,`05` | 500 persons 575 -> 32 ms; 5,000 persons 4,589 -> 225 ms; all 10,000 persons 10.4 s -> 0.49 s (18-21x) | fn-10k: denied sets equal for 500 / 5,000 / 10,000 persons (543 denied, 0 differences); randomised fixture (3 tenants x 3 jurisdictions, 1,200 persons, matches, holds, lifts, resolutions, auto-reviewed re-flags, pending/failed requests) with 6 seeds: 0 mismatches, denied counts 332-934 of 1,200 so both outcomes are covered |
| E5 `parked_summary`: partial index `(tenant_id, person_id, decided_at DESC, id DESC) INCLUDE (...) WHERE operation='withdrawal_payout'` + per-person LATERAL for the persons count. **Trade-off, not a free win:** it moves the cost from decision VOLUME to PERSON COUNT (one index probe per distinct person: ~200k probes at 200k persons, UNMEASURED here) and adds an index to an append-only audit table whose size and per-insert write overhead I did NOT measure; a plain CREATE INDEX would block the gate's decision INSERTs, so it would have to be `CREATE INDEX CONCURRENTLY` or run in a write-quiet window | `08`,`09` | 4M decisions: 6.0 -> 1.1-1.5 s (10k persons; 1 run) | identical (persons, evaluations) for 7 windows on one synthetic table |

**Exactly what the equivalence evidence covers (review round; the first version of this table said more than the tests prove).**
* E1 (`02_since_equiv.sql`, 6,000 inputs x 3 seeds re-run now = 18,000, previously 6 seeds): random histories with ties in `created_at`, effective_from before and after creation, not-yet-started versions, foreign/unknown after-policy ids, NULL/empty/partial lists. **Mutation check:** removing the 'never in force' rule makes the test fail (1,636 of 6,000 mismatches), so the test can fail.
* E4 (`04_gate_equiv.sql`, now 4 tenants x 3 jurisdictions, 1,720 persons per seed, 6 seeds, 0 mismatches, denied counts 213-1,231): exercises open matches, auto-reviewed re-flags, officer resolutions by a clear AND by a sanctions-class match, holds and lifts, pending/failed requests, results with and without provenance, multiple jurisdictions (gating on/off, 0-3 trigger versions), screening-policy histories of 4-14 versions including never-in-force versions, `created_at` ties and future-effective versions, a tenant whose gate took effect 5 days ago with a 30-day backfill grace, and 30 persons per tenant with no account (unknown persons). **Mutation check on 04:** with the 'never in force' rule dropped, seed 0.2718 shows 35 mismatches (1,171 vs 1,206 denied) but seed 0.11 shows 0 - one seed is not enough, which is why 6 seeds run; before this round the mutation survived 04 (it did not contain such versions).
* NOT covered: ordering/selection effects (E3), the monitors' own outputs (`gate_overdue` anchors, `parked_overdue` buckets: E4 compares only the inner denied set), live concurrency with policy writes, any real data. A single clock instant is now passed (`p_now`) so the candidates no longer read separate `clock_timestamp()` values (ledger M3); production-vs-candidate comparisons still call the production function at a slightly different instant, which random fixtures make immaterial.
* E2 (`06`): identical on the uniform 3-version seeds at 10k/50k/200k AND now on the random multi-version fixture (`12_fixture_then_enqueue.sql`: 12 jurisdiction/tenant combinations, 4,836 candidate rows, 0 differences; speed-up there only 1.5x because that fixture is small and mostly stale).
* Re-run of 05 after the `p_now`/GROUPS change on the 10k backlog tenant: all 10,000 persons 9.4 s (production path) -> 0.66 s (candidate), denied sets equal (2,894, 0 differences) - 14x; the 500/5,000-person cells were not re-timed (previous run: 575 -> 32 ms and 4,589 -> 225 ms).
* Security notes (ledger L3): a set-returning 'who is denied' function is a screening-status oracle; a real version must be owner-only (EXECUTE revoked from the runtime role) and bound to the session tenant like every definer function here; the experiments are INVOKER functions in scratch databases.

Not turned into an experiment (HYPOTHESES, with the reason the evidence points there):
* H-a Run `enqueue_due` / `gate_overdue` / `parked_summary` on a slower cadence than the 10 s claim loop, or skip `enqueue_due` while the tenant already has at least `ClaimBatch` x N due requests. Evidence: claims need ~3 ms, the other two phases need 1-33 s; most passes find nothing new in steady state.
* H-b Per-tenant concurrency (a bounded set of tenants in parallel) or a priority lane for claims. Evidence: the pass is the sum of tenant costs; the machine used 25 % CPU during the 100-tenant pass.
* H-c More claims per tenant per pass (needs the lease change from 4.5). Evidence: drain rate is exactly the cap.
* H-d `gate_overdue` with `ORDER BY person_id` and a persisted cursor, or moving the trapped signal to the sweeper's keyset read.

## 8. Proposed targets (PROPOSED, to be decided by the owner; not facts)

Derived from the existing timeouts and the observed headroom. Each assumes the measured shapes (one 4 vCPU database, default settings, warm cache, persons per tenant as stated).

| target | value proposed | derived from / assumes |
|---|---|---|
| T1 every database phase of the worker p95 | <= 1 s at the largest supported tenant (5x headroom on `PhaseTimeout` 5 s) | today: enqueue 33 s, overdue 5.3 s, claim/finish 3 ms |
| T2 worker pass time for the whole fleet | <= the pass interval x k (k to be chosen) or per-tenant scheduling so that a tenant's turn does not depend on fleet size | today 1.3 s per 1k-person tenant |
| T3 claims per tenant per hour at 1 s provider latency | >= the value implied by the drain objective below | today 1,440 (1 tenant) ... 24 (500 tenants) |
| T4 sweeper full observation cycle | <= the gate alert threshold of the policy in force (that threshold is a POLICY value; the 1 h used in my fixtures is a fixture) | today at least P/500 x one sweeper pass; a pass is the SUM over tenants (about 40 s for 100 tenants), so the fleet multiplier must be included, not only the 15 s tick: lower bound 100 min at 200k for one tenant |
| T5 withdrawal gate call p95 | <= 10 ms with the worker running | today median 2.4 ms and p95 5 ms with 4 callers on their own pool, median 9 ms / p95 15 ms on the shared 10-connection pool |
| T6 recovery after a worker crash | lease + the time until the owning tenant's turn in the next pass | measured 137 s for ONE tenant (lease + one pass); at fleet size the reclaim happens when the pass reaches the owning tenant: with the measured pass times (130 s at 100 tenants, ~610 s extrapolated at 500) that is 4-12 minutes, and a crash loop burns one attempt per reclaim |

Owner decisions these depend on (not chosen here; the first six are the original list, the rest were added by the reviews): (1) the acceptable time a payout may stay parked for screening after a policy change (mass-stale drain objective: minutes? hours?); (2) the largest tenant (persons) and the number of tenants the platform commits to serve on one worker; (3) whether a `failed_terminal` screening may keep a person gated until the next re-screen bucket or must be re-queued automatically; (4) the cadence of scheduled enqueue relative to the claim loop; (5) whether lease length (and so crash-recovery time) may grow to allow larger claim batches; (6) the real number of policy versions expected per tenant; (7) the paging/escalation route for monitor failures (today a failed monitor is a counter and an untagged log line); (8) deployment topology: a dedicated screening worker process versus a worker inside every API replica (today: in every replica, sharing the request pool), and the intended worker count; (9) what withdrawals do while screening is behind (refuse request, accept and park, or accept-and-hold funds); (10) the provider-outage budget (today ~8-10 minutes of outage mass-terminates in-flight requests: backoff 30+60+120+240 s, no jitter, no breaker) and whether a circuit breaker is wanted; (11) the database sizing assumption behind any target (one 4 vCPU box here); (12) the heartbeat bound (10 minutes, fixed in SQL) versus the longest allowed pass; (13) a priority lane in the claim order for persons with a parked payout or a pending withdrawal; (14) the maximum parked time before escalation; (15) cancelling a long-parked payout - only through a governed compensating reversal, never an edit; (16) a policy-change blast-radius preview (how many persons a change makes stale) before activation; (17) incident handling when a mass-stale event happens during a drain; (18) whether `TenantCap` (500) and silent truncation are acceptable.

**Determinations (legal/regulatory, NOT engineering decisions - flagged, not decided):** the maximum payout delay allowed per jurisdiction; whether wagering may continue while a withdrawal request is refused; what the player may be told about a screening hold (tipping-off).

## 9. Scenarios NOT run, and why

* `stale-2k-gate`, `stale-2k-batch20` (scenario files exist), `run-p50k-l0`: dropped to bound the time after the container restart; the batch/lease lever in 4.5 is therefore a HYPOTHESIS. Gate-caller effects on the drain were measured on the 100-person backlog instead.
* Retry storm with the DEFAULT 30 s backoff to `failed_terminal` (needs > 20 min; the 3 s-backoff variant reached it, 4.9.8, 1 run); alert ROWS for the terminal state (the harness's read of `alerts` is empty, platform-routed rows are not visible to it: NOT verified); multiple jurisdictions per tenant (`jurisdictions` parameter exists, not run; note the gate cost scales with the number of gating jurisdictions, HYPOTHESIS); lognormal / bimodal provider latency and partial failure rates (the provider supports them, only fixed latency, hang and 100 % failure were run); RunPass with the 50k tenant; a fleet of 500 tenants at latencies above 100 ms and of 100 tenants over 10 min windows; alert delivery (the alert rows are created in the same transaction, delivery is not exercised).
* Cells that were capped (listed "capped" in 4.4) measure the throughput of a time window, not a complete pass; complete-pass durations for them are EXTRAPOLATIONS and are labelled.
* Production data volumes, real vendor latency distributions and a multi-node database are out of scope (CLAUDE.md: synthetic data, no production access).

* Review round, not run: a real 500-tenant pass above 600 s (the 800 s / 100-tenant pass with a 5 s provider was run instead, 1 run); sweeper pass for the 200k tenant alone (the skewed fleet includes it; 3 runs); worker count above 3; `stale-2k-batch20`; measuring E5's write overhead and index size; the monitors' outputs under E4; a rolling restart of several workers at once; the withdrawal request path itself (only `aml_hold_active` callers were emulated).

## 10. Residual uncertainty

Single machine and single PostgreSQL configuration; the first 17 h of results predate a container restart (section 2) and the kernel build changed minor version, but the repeated cell (`run-skew-l100` after, `run-skew-l0` before) is consistent with the pre-restart cells; per-person gate cost depends on the data shape (here one done request per person); the synthetic decision volume is not a production estimate; fixture timing (`now()`) is relative so results do not depend on the date; harness cost is small (the sampler adds 3-4 short queries per 500 ms) but not zero.

## 11. How to rerun

```
# environment (existing dev/CI values; nothing is printed): TEST_ADMIN_DATABASE_URL, DATABASE_URL (owner), TEST_RUNTIME_DATABASE_URL
cd deploy/loadtest/screening
./env-info.sh                                  # hardware / PostgreSQL / load, goes into the report
./run.sh base                                  # migrate capa_base from this checkout
./run.sh seed fn-10k && ./run.sh measure fn fn-10k 3
./run.sh seed growth-v1000 && ./run.sh measure growth growth-v1000 3
./run.sh seed pass-t100 && ./run.sh measure runpass run-t100-l0 3        # seed_scenario is read from the scenario file
./run.sh seed stale-100 && ./run.sh measure stale stale-100-drain 3
./run.sh seed retry-300 && ./run.sh measure retry retry-storm 3
./run.sh seed crash-500 && ./run.sh measure crash crash-kill 3
./run.sh seed race-2k && ./run.sh measure race race-policy-churn 3
./run.sh explain fn-10k explain/30_enqueue_due.sql                       # EXPLAIN reconstructions
./run.sh explain-src capa_base experiments/04_gate_equiv.sql             # randomised equivalence (psql -v seed=0.37 via CAPA_PSQL_ARGS)
python3 -I summarize.py results fn fn-1k fn-10k fn-50k fn-200k
./run.sh seed tenantcap-501 && ./run.sh measure tenantcap tenantcap-501 2        # review round
./run.sh seed pass-t100 && ./run.sh measure sweep sweep-t100 3                    # real payments.Sweeper.RunPass
./run.sh seed replay-4 && ./run.sh measure replay replay-4 2                      # idempotent-replay provider, lost finish
./run.sh seed pass-t10 && ./run.sh measure runpass run-t10-w2 3                  # several worker loops + invariants
./run.sh explain race-2k experiments/11_heartbeat_expiry.sql                      # heartbeat bound mechanism
CAPA_PSQL_ARGS="-v seed=0.37 -A -t" ./run.sh explain-src capa_base experiments/04_gate_equiv.sql   # add -v mutation=1 to prove the test can fail
./run.sh drop-all
```
Scenario files: `deploy/loadtest/screening/scenarios/`. Raw results of the review round: `results-json-review-round.tar.gz`, `review-round-experiments.txt`, `review-round-exp05.txt`, `run-logs-review-round.txt` in the same directory. Raw results of the first round: `docs/plans/capacity-baseline-data/` (`results-json.tar.gz` holds every `<scenario>.<mode>.runN.json` and `<scenario>.seed.json`; plain `explain/`, `env-info.txt`, `run-logs.txt`; unpack with `tar xzf` into a results directory and point `summarize.py` at it).
Known harness caveat: when a run's `DROP DATABASE ... WITH (FORCE)` is refused ("permission denied to terminate process", seen once for `run-t500-l0`) the script stops; clean up with `./run.sh drop-all`.


## 12. Review dispositions

Three independent reviews (DB-performance, operations, ledger-finance; all ACCEPTABLE AFTER CHANGES; ledger-finance with a veto). Every item below: ACCEPTED (changed here), MEASURED (new data), or NOTED (with the reason it is not changed).

| item | disposition |
|---|---|
| DB-perf H1 knife-edge timeout wording | ACCEPTED: reworded everywhere to 'borderline at ~5 s; a tenant of ~5k persons or more sits at the timeout' (summary 2, 4.4, 6 row 1, section 3). Our 10k passes all lost it; the reviewer's machine lost 3 of 9: both are reported |
| DB-perf H2 attribution of gate_denies cost | ACCEPTED: section 5 rewritten (covering NOT EXISTS 0.88 ms of the stale call; first `since()` 0.25-0.34 ms vs 0.06-0.11 amortised); guarding `since()` helps the per-person gate too |
| DB-perf M1 growth called with `p_after_policy = NULL` | ACCEPTED and MEASURED: stated; production shape added x3 (27.5 ms mid-history / 3.6 ms latest at V=1000; 1.4 / 0.9 at V=100) |
| M2 data realism | ACCEPTED: limits stated in the summary; extra-history cell run (no change) |
| M3 E3 is a behaviour change | ACCEPTED: relabelled (selection order across the tenant is lost; needs a persisted cursor and an owner decision) |
| M4 E5 trade-off | ACCEPTED: relabelled (cost moves to person count, ~200k probes UNMEASURED; index size/write overhead UNMEASURED; CONCURRENTLY or write-quiet window) |
| M5 T4 ignores the serial multiplier, 1 h is a fixture; 'MEASURED' on extrapolated rows; 'quadratic' wording | ACCEPTED: T4 rewritten, section 6 label column fixed, since-growth wording changed to 'quadratic plus a fixed overhead' |
| Ledger veto: no change to bodies of `since` / `gate_denies` / `aml_hold_active`, no claim batch / lease increase in the next slice | ACCEPTED: section 7 says new names, monitors only first; section 4.5 lease lever stays a HYPOTHESIS |
| Ledger: equivalence evidence narrower than stated | ACCEPTED: exact coverage listed in section 7; never-in-force versions, ties, future-effective versions, backfill grace, unknown persons, sanctions-class resolution by a match added to `04`; mutation check shown (detected by `02`; by `04` on 1 of 2 seeds, hence 6 seeds); E2 now also on a random multi-version fixture; E3 union called a tautology; E4 compares only the inner denied set |
| Ledger L1 'callers denied' wording | ACCEPTED: withdrawal REQUESTS are refused (funds stay in the wallet and can still be wagered), approved PAYOUTS are parked (4.5) |
| Ledger M3 separate clock instants | ACCEPTED: candidates take one `p_now` |
| Ledger M4 idempotent replay | MEASURED (4.9.6): every reclaim fails; PROVIDER DEPENDENT |
| Ledger M5 CREATE INDEX blocks decision inserts | ACCEPTED (E5 row) |
| Ledger L3 / L4 oracle / frame | ACCEPTED: owner-only and session-tenant-bound note; `GROUPS` frame instead of a microsecond RANGE |
| Ledger E1 changes the money path | ACCEPTED: new name, monitors only first |
| Ops C1 heartbeat once per pass | MEASURED (4.9.1): confirmed by the mechanism test and a real 800 s pass; added as a finding |
| Ops H1 TenantCap | MEASURED (4.9.2): tenant 501 never served |
| Ops H2 sweeper pass | MEASURED (4.9.3); '100 min at 200k' now a lower bound |
| Ops H3 production topology, several workers | MEASURED (4.9.5) with the assertions asked for; `run-t10-shared-pool` relabelled production-like (gate p95 15 ms vs T5 10 ms) |
| Ops H4 trapped alert silently never raised | answered precisely (4.9.4): errors counter + untagged log line, no alert/metric; sweeper path raises it; delivery is ALERT-DELIVERY-1 (not re-verified) |
| Ops H5 operator view for the skewed fleet | answered below (Appendix A.1) |
| Ops M1 T6 only true for one tenant | ACCEPTED: T6 rewritten (4-12 min at fleet size, arithmetic from measured pass times) |
| Ops M2 outage > ~8-10 min mass-terminates | MEASURED with a 3 s backoff (4.9.8); the default-backoff run was not performed (arithmetic only) |
| Ops M4 rolling restart | MEASURED (4.9.7) |
| Ops M6 rate-limit sentence | NOTED from the code, not measured: `rate_limited` provider errors are mapped to an error code and retried with the same exponential backoff; there is no Retry-After handling and no per-provider throttle (the serial worker is the only limiter: one call in flight per worker) |
| Ops L1 duplicate keys are not evidence | ACCEPTED: sentence corrected in the summary |
| Ops appendix (observability table, SLOs, acceptance template), missing owner decisions, legal items | ADDED: Appendix A, section 8 (items 7-18), legal 'determinations' paragraph |
| Ordering conflict (PART 2) | Recorded, not decided: section 13 |

Where I disagree or qualify (with evidence): (1) the reviews ask to state the p10k timeout as knife-edge; I agree, but 9 of 9 of our own passes failed (`run-p10k-l0`), so 'borderline' should not be read as 'usually passes' on this hardware; (2) E4's 14-21x is for the denied SET only; I do not claim monitor output equivalence; (3) for the retry storm, queue position, not backoff, paced the retries in our runs because a backlog existed (4.9.8) - the ops concern about outage > 8-10 minutes remains correct for requests already attempted several times, and is arithmetic until a default-backoff run is made. I found no review item I consider wrong.

## 13. Ordering conflict recorded for the owner (not decided)

Operations recommends the SQL capacity slice BEFORE the gate migration 0198 (which md5-pins the gate predicate and monitor bodies). Ledger-finance notes that the candidates touch bodies the gate design pins identical through 0198, so they would have to land as 0199 or later, or the pin would have to be reopened as a named reviewed exception.

| option | what it means | consequences |
|---|---|---|
| A. Capacity slice first (changes to the monitor bodies and indexes before 0198) | the gate design pins the NEW bodies/hashes in 0198 | + the monitors are fast before the gate rollout, which makes the rollout observable; + one set of pins; - the gate design's 'identical through 0198' assumption is reopened as a named reviewed exception; - the capacity slice is on the critical path of 0198; - the ledger veto applies: only new function names for monitors, the live gate bodies stay untouched, so the pins of the live bodies are not actually affected - what changes is which MONITOR bodies are pinned |
| B. Gate migration 0198 first, capacity slice as 0199+ | the pin holds as designed; capacity changes follow as new migrations with their own hashes | + the money-path pins are untouched and reviewed once; - the gate ships with monitors that time out at ~5k persons and with the issues in 4.9 (heartbeat bound, TenantCap) for at least one release; - a second round of pin updates later |
| Facts both options share | measured in this report | the monitors, not the gate call, are what exceed the timeout (gate call 1-2 ms); the heartbeat/TenantCap/sweeper findings do not depend on SQL migrations (worker or config changes) |

## Appendix A. Operations view

**A.1 What an operator sees today for the skewed fleet (1 x 200k + 99 x 1k).** Worker pass log line (info): `tenants=100 enqueued=9900 claimed=396 done=396 ... errors=2 trapped=...`; two error log lines per pass without tenant id (`scheduled enqueue failed`, `overdue check failed`); no metric; no alert for the failed phases; the 200k tenant's persons are simply never scheduled for re-screening. The sweeper reports `screening_scanned/partial` counts only when parked or errors exist. Nothing tells the operator that tenant X is being starved.

**A.2 Observability that does not exist and would be needed (PROPOSED).**

| signal | why | exists today |
|---|---|---|
| pass duration, per phase and per tenant (histogram) | pass > heartbeat bound, tenant starvation | no (only a count line) |
| tenants served / tenants skipped (TenantCap) per pass | silent truncation | no |
| phase timeout counts per phase and tenant | borderline phases | log line without tenant, `errors` counter |
| queue depth and oldest due `next_attempt_at` per tenant | drain progress, starvation | no |
| claimed-with-expired-lease count | crashed/stalled workers | no |
| heartbeat age (max) | finish refusals | no |
| attempts distribution, `failed_terminal` count per tenant | outage damage | no |
| count of persons denied by the gate (per tenant), age of the oldest | parked time | partial (sweeper keyset) |
| sweeper pass duration vs tick | payout work delayed by the observation read | no |
| connection-pool wait time | shared-pool contention | pool metrics exist (`RegisterPoolMetrics`), not tied to this path |

**A.3 SLO candidates (PROPOSED, thresholds are owner decisions).** (1) oldest unserved due request age <= X minutes per tenant; (2) pass duration <= heartbeat bound / 2; (3) 0 tenants skipped per pass; (4) monitor phase p95 <= 1 s at the largest tenant; (5) gate call p95 <= 10 ms; (6) failed_terminal rate per outage minute; (7) time from policy change to last affected person cleared.

**A.4 Capacity acceptance template (to be filled per release candidate).** Environment (hardware, PostgreSQL version and settings, commit); scenarios run with `deploy/loadtest/screening` (names and number of runs); table: fleet shape (tenants, persons per tenant, versions per tenant, jurisdictions) x {pass duration median/max, claims/h/tenant, phases over 1 s, TenantCap skipped, heartbeat headroom}; drain: backlog, measured rate, time to clear; failure drills: SIGKILL, SIGTERM, provider outage (duration vs backoff), lost finish; invariants asserted (no duplicate request per person and bucket, calls <= attempts, no finish with an old token applied, no done request whose provenance differs from its claim policy); verdict per target (PASS / FAIL / NOT RUN) with the report commit.
