# Screening capacity baseline (CAP-A) - measured report

Status: test-tooling and measurement only. **No migration, no production code, no behaviour change.** Every number below was produced by
`deploy/loadtest/screening/` (build tag `loadtest`) against **private `capa_*` PostgreSQL databases with synthetic data and a MOCK provider**.
Nothing here is a production capacity figure: it is one 4 vCPU / 16 GB machine with default PostgreSQL settings, one harness process, no network.
Labels: `MEASURED` = produced by a run listed here (3 runs unless stated); `HYPOTHESIS` = not established by a run; `EXTRAPOLATION` = arithmetic beyond measured sizes.

## 1. Summary

1. **MEASURED. The reviewer's function timings are reproduced, and they do not need concurrent load.** On an otherwise idle machine,
   `aml_screening_gate_overdue(tenant, 5000)` takes 5.3 s at 10,000 persons and `aml_screening_parked_overdue(..., 5000)` 5.4 s (production
   `ScreeningWorkerConfig.PhaseTimeout` is 5 s). `aml_screening_enqueue_due` is linear in the tenant's persons (0.17 ms/person): 1.7 s at 10k, 8.7 s at 50k, 33 s at 200k.
2. **MEASURED. Consequence in the real worker.** With a 10k-person tenant the worker's overdue check times out in every pass (`Errors=1`, `Trapped=0`: the
   `aml.screening_gate_trapped` alert from the worker path cannot be raised). With a 200k-person tenant (fleet 1 x 200k + 99 x 1k) the scheduled enqueue and the overdue check both time out
   in every pass: **the big tenant is enqueued 0 times and claims 0 requests** while still costing the pass about 10 s of timeouts.
3. **MEASURED. "About 1,440 claims/h/tenant" is an upper bound reached only by a single small tenant** (and only while the pass fits the 10 s interval). Measured per-tenant claims/hour:
   1,440 (1 tenant x 1k persons, provider <= 1 s), 1,105 (10 tenants), 110 (100 tenants x 1k persons, 0 ms provider), 24 (500 tenants; 246 tenants started per 300 s window). The cause is the
   per-tenant fixed cost of `gate_overdue` + `enqueue_due` (about 1.3 s per 1k-person tenant), because `RunPass` walks tenants serially.
4. **MEASURED. A mass-stale event drains at exactly the claim cap**: 24 persons/minute for a tenant (4 claims per 10 s pass), independent of the number of concurrent withdrawal callers.
   100 stale persons: 250 s to clear (3 of 3 runs). 2,000 stale persons: 72 cleared in 180 s; clearing all would take about 1.3 h (`EXTRAPOLATION`, linear). Withdrawal callers are denied for the whole drain.
5. **MEASURED. Safety properties held in every run**: a SIGKILL'd worker's claims were reclaimed under a new token after the lease (137 s after the kill), no double finish (a late finish with the old token applied 0 times),
   0 duplicate idempotency keys, and under a policy change every `done` request recorded the policy version of its claim (0 violations); the cost of a policy change between claim and finish is a failed finish (the request waits for its lease) not a wrong clear.
6. **MEASURED (growth) / validated candidates.** Policy versions make `aml_screening_since` super-linear (74.7 ms at 1,000 versions vs 0.8 ms at 1; `gate_denies` of a stale person 1.5 -> 31.6 ms).
   Candidate rewrites validated only as throwaway experiments on private databases with output-equivalence checks: window-function `since` (O(V log V)), guarded `since` in the enqueue candidate
   (3.3-3.8x faster), keyset-sliced enqueue evaluation (cost independent of tenant size), set-based gate evaluation for the batch readers (18-21x faster, identical denied set). Section 7.

## 2. Environment and method

| item | value |
|---|---|
| machine | 4 vCPU Intel Xeon @ 2.10 GHz, 16 GB RAM, kernel Linux 6.18.44-fc (v165 before the restart, v180 after), disk 252 GB |
| PostgreSQL | 16.15 (Ubuntu 16.15-0ubuntu0.24.04.1), **default settings**: shared_buffers 128MB, work_mem 4MB, maintenance_work_mem 64MB, effective_cache_size 4GB, max_connections 100, random_page_cost 4, jit on, max_parallel_workers_per_gather 2, track_io_timing off, fsync/synchronous_commit on, autovacuum on. Full list: `docs/plans/capacity-baseline-data/env-info.txt` |
| commit | harness at `HEAD` of branch `claude/capacity-baseline-gate-design` (from integration tip `01ea296`); report data produced with harness commits `c5b6546`, `866a82f`, `b794c4a` and the later commit that adds this report |
| date | 2026-10-27 (runs 16:50-23:50 UTC) |
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
| vs `PhaseTimeout` 5 s | exceeded by gate_overdue at >= ~4.8k persons and by enqueue_due at >= ~29k persons (the latter: EXTRAPOLATION of the linear fit between measured 10k and 50k, confirmed by the 200k RunPass below) | MEASURED (see 4.4) |
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
`parked_keyset_cycle_500`: a batch is 0.5 s at any size; a full cycle over a tenant is P/500 batches (3, 21, 101, 401): back-to-back 1.2 s / 11 s / 53 s / 217 s, but the payments sweeper reads **one batch per 15 s pass**,
so a full observation cycle takes 401 x 15 s = **about 100 minutes at 200k persons** (arithmetic on the measured batch count and `SweeperLoopDefaultInterval` 15 s).
`parked_summary_7d` grows with the decision table (1 ms at 10k persons / 10k decisions, 8.8 ms at 200k persons); see 4.7 for decision volume.

DB/OS during these runs (probe objects in the result JSON, `docs/plans/capacity-baseline-data/results-json.tar.gz`): a single function call keeps one backend CPU-bound (OS CPU busy 25-27 % of 4 vCPU at 200k, i.e. one core), iowait 0,
shared-buffer hit ratio 0.992-1.000, no `pg_locks` wait in any probe window (0 of 490 windows over all runs). `enqueue_due` at 200k wrote 34.7 MB of temporary files in total across its repetitions (sort/hash spill at work_mem 4 MB); the
cost is CPU, not I/O. With 4 gate callers and a keyset reader running concurrently (`fn-10k-loaded`, 7 database sessions, CPU busy about 52 %) the function latencies are unchanged within noise (table, column `fn-10k-loaded`),
gate callers see a median 2.4 ms / p95 5.0 ms / max up to 307 ms per `aml_hold_active`, 19-20k calls per 3-run window, 77 % of calls denied (the backlog mix). Wait-event samples of the harness sessions: 100 % on CPU.

### 4.2 (b) Policy-version growth (1 tenant x 1000 persons)

| statement (median ms; min-max across runs) | growth-v1 | growth-v10 | growth-v100 | growth-v1000 |
|---|---|---|---|---|
| since_at_oldest(all_versions_later) | 1.0 (0.6-1.0) | 0.8 (0.8-1.2) | 2.6 (2.3-2.7) | 74.7 (74.4-88.5) |
| since_at_mid | 0.8 (0.7-1.0) | 1.1 (1.1-1.2) | 1.4 (1.3-2.0) | 27.8 (27.2-46.9) |
| since_at_recent_3d | 0.8 (0.6-0.8) | 0.8 (0.8-0.9) | 0.9 (0.9-1.0) | 2.8 (2.7-4.1) |
| covers | 0.1 (0.1-0.1) | 0.2 (0.1-0.2) | 0.2 (0.1-0.2) | 0.1 (0.1-0.2) |
| gate_denies_clear | 1.4 (1.3-1.8) | 1.3 (1.2-1.3) | 1.5 (1.4-1.7) | 4.6 (4.1-4.7) |
| gate_denies_stale | 1.5 (1.4-1.9) | 1.6 (1.4-2.0) | 2.3 (2.3-2.4) | 31.6 (31.0-32.4) |
| gate_denies_none | 1.2 (1.0-1.3) | 1.0 (1.0-1.2) | 0.9 (0.9-1.0) | 1.5 (1.4-1.7) |
| enqueue_due_100 | 155 (151-170) | 205 (179-218) | 391 (338-403) | 6,307 (6,160-6,480) |
| gate_overdue_500 | 580 (576-588) | 533 (514-557) | 634 (633-655) | 3,824 (3,794-4,054) |

MEASURED: the hypothesis O(V^2) is supported: `aml_screening_since` with every version later than `p_at` (the stale-person case) goes 0.8 ms (V=10) -> 2.6 (V=100) -> 74.7 ms (V=1000); about x41 net for x10 versions
(between linear and quadratic; a self-correlated NOT EXISTS over an index scan). Fresh clears (`p_at` 3 days ago) stay cheap (2.8 ms at 1000). Effect: `gate_denies` of a stale person 31.6 ms at V=1000,
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
* One 10k-person tenant (`run-p10k-l0`): pass 6.6 s, but `Errors=1` in every pass (the 5 s overdue check times out) and `Trapped=0` - the worker cannot raise the trapped-persons alert for it.
* Skewed fleet (1 x 200k + 99 x 1k): pass 136 s (0 ms) / 173 s (100 ms); `Enqueued=9900` = 99 x 100: the 200k tenant is **never enqueued and claims nothing** (enqueue times out at 5 s, overdue check times out at 5 s; 2 errors per pass). Its persons only get screened through the registration / KYC-approval triggers, never by the scheduled re-screen or the backfill.
* Connections: one worker uses 1 pool connection at a time (max acquired 1-2 of 10). **MEASURED (i):** when 20 closed-loop gate callers share the worker's 10-connection pool (`run-t10-shared-pool`), all 10 connections are in use, 151k-173k of the acquires waited for a connection (792-903 s of cumulative wait in a 75 s window), gate calls rose from 2.2 ms to a 9.1 ms median (p95 15 ms, max 100-182 ms) and the worker pass went from 16.3 s to 24.7 s. With the callers on a separate pool (`run-t10-ownerpool-gate`) the worker is unaffected (pass 16.3 s) while the database saw 11 + 1 sessions (limit `max_connections` 100).

### 4.5 (d) Mass-stale event (list added) and drain

1 tenant, a policy version adding the `pep` list makes every clear stale. Worker with default config, 100 ms MOCK provider; backlog = persons the gate denies right after the event.

| scenario | backlog denied after event | worker cfg | done in window | drain rate /min (min-max) | pass duration s (median; max) | denied at end | time to clear | gate caller median / p95 / max ms | gate denied share | errors |
|---|---|---|---|---|---|---|---|---|---|---|
| stale-100-drain | 100 | batch 4, lease 120s | 100 | 24.0 (24.0-24.0) | 0.6; 0.7 | 0 | 250 s measured, 250 s measured, 250 s measured | - | - | 3 (3 runs) |
| stale-100-drain-gate | 100 | batch 4, lease 120s | 100 | 24.0 (24.0-24.0) | 0.6; 0.6 | 0 | 250 s measured, 250 s measured, 250 s measured | 2.2 / 3.7 / 93 | 0.48 | 3 (3 runs) |
| stale-2k-default | 2000 | batch 4, lease 120s | 72 | 24.0 (24.0-24.0) | 3.4; 3.7 | 1928 | 1.3 h EXTRAPOLATED, 1.3 h EXTRAPOLATED, 1.3 h EXTRAPOLATED | - | - | 0 (3 runs) |

* Drain rate is 24.0 persons/min in every run (min = max): `ClaimBatch` 4 x 6 passes/min. Provider latency 100 ms and the gate callers do not change it.
* 100 stale persons cleared in 250 s in 3 of 3 runs, with and without 4 paced (20 ms) gate callers. With callers, 48 % of their calls were denied overall; the denied share of calls fell from 93 % (first 30 s) to 0 % (last bucket): **a person is refused at the withdrawal gate until its own turn in the queue**, so parked time is uniformly distributed over 0..backlog/24 min.
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

## 5. EXPLAIN (ANALYZE, BUFFERS) - manual reconstructions

Files: `deploy/loadtest/screening/explain/*.sql` (exact SQL), outputs `docs/plans/capacity-baseline-data/explain/*.txt`. **These are reconstructions of the statements inside the plpgsql functions with literals from the data, not captured plans of the functions.**
Representativeness (reconstruction vs real function, same data): `enqueue_due` candidate INSERT 1,969 ms vs function 1,612-1,695 ms at 10k (EXPLAIN instrumentation overhead); 32,887 ms vs 33,038-34,090 ms at 200k.
Excerpts (10k persons, 4 MB work_mem):

* `enqueue_due` (E3): `Nested Loop Anti Join / Hash Anti Join / Subquery Scan c ... rows=10000 loops=1` with **SubPlan 1** (`max(completed_at)` union, 0.030 ms x 10,000 loops, Index Only Scan on `aml_screening_requests_person`) and **SubPlan 2** (the covering EXISTS: `Index Scan using aml_screening_requests_person ... Filter: ... aml_screening_covers(... aml_screening_since(...))`, **0.151 ms x 10,000 loops = 1.5 s of 1.9 s**). Nothing is a seq scan of the big tables; the cost is one function call to `since()` + `covers()` per person, executed for every person before `LIMIT`.
* `gate_denies` inner statements: every statement is a 0.02-0.15 ms index probe (`aml_screening_requests_person`, `aml_screening_results_person`); the 1.5 ms per person is ~10 statements of per-call plpgsql/SPI overhead, not one heavy statement. Seq scans only on the 1-3 row policy tables.
* `since` body at V=1000: `Index Scan on aml_screening_policies ... SubPlan NOT EXISTS ... loops=1000` = 72.6 ms (O(V^2)); the same at p_at = 3 days ago 2.2 ms.
* `parked_overdue`: batch read `Unique / Sort / Index Scan player_accounts` 1-4 ms; decisions read for the denied persons and open requests 1-11 ms; the 0.5 s is the 500 `gate_denies` calls.
* Buffers: warm (`shared hit`), 0 reads after warm-up; JIT compiles 60 functions for the enqueue statement (45 ms of 1.97 s, 2 %). Temporary files: only the decision-volume sorts (4.8) and the enqueue sort at 200k.
* Larger `work_mem` (hypothesis only, session `SET`): not useful - the CPU-bound statements have no spill at 10k; the one real spill (decisions) did not go away at 256 MB.

## 6. Bottlenecks ranked by measured contribution

| # | bottleneck | measured contribution | label |
|---|---|---|---|
| 1 | **Per-person plpgsql gate evaluation in the batch readers** (`gate_overdue` limit 5000, `parked_overdue`): 1.0-1.1 ms/person, 5000 persons = 5.2-5.4 s > PhaseTimeout | 80 % of the per-tenant cost of a 1k-tenant pass (1.04 of 1.3 s); times out every pass from ~4.8k persons; also makes the worker's trapped alert incomplete | MEASURED |
| 2 | **`enqueue_due` is O(persons) per call**, per pass, evaluating `since()`+`covers()` for every person before the LIMIT | 0.17 ms/person: 1.7 / 8.7 / 33 s at 10k / 50k / 200k; times out (and never enqueues) above ~29k persons; 0.18 of 1.3 s per 1k tenant | MEASURED (29k: EXTRAPOLATION, confirmed qualitatively by the 200k tenant) |
| 3 | **Serial tenant walk with a fixed per-tenant cost** | pass = sum over tenants; 100 tenants x 1k: 130 s, 110 claims/h/tenant (13x below nominal); 500 tenants: ~610 s (EXTRAPOLATION from 246 tenants / 300 s) | MEASURED |
| 4 | **Claim throughput cap** 4 per pass per tenant, lease-coupled | 24 persons/min/tenant; 2,000-person event = ~83 min (EXTRAPOLATION), 10k = 6.9 h | MEASURED rate / EXTRAPOLATED durations |
| 5 | **Provider latency is serial** (4 calls x latency per tenant per pass) | +0.4 s (100 ms) ... +40 s (hung) per tenant per pass; single tenant at 5 s: 677 claims/h | MEASURED |
| 6 | **Policy-version growth** in `since()` | negligible at V <= 100, 20x on `gate_denies` of stale persons and 40x on `enqueue_due` at V = 1000 | MEASURED |
| 7 | **Sweeper observation cycle** = P/500 batches x 15 s | 5 min at 10k, 25 min at 50k, ~100 min at 200k (arithmetic on measured batch counts) | MEASURED batches / computed cycle |
| 8 | `aml_enforcement_decisions` volume in `parked_summary` | exploratory: 6 s at 4M rows | HYPOTHESIS about production volume; one run |
| 9 | Shared 10-connection pool under gate-caller load | worker pass +50 %, gate calls 4x slower in the test | MEASURED (test shape: 20 closed-loop callers) |
| - | locks, I/O, buffer misses, work_mem | none observed | MEASURED (negative) |

Other MEASURED observation: `gate_overdue(5000)` evaluates `DISTINCT person_id ... LIMIT 5000` with **no ORDER BY**: on a 10,000-person tenant it saw 1,419 overdue persons where limit 20,000 sees 2,894 (49 %; `experiments/10_overdue_coverage.sql`). The worker's trapped count is therefore a partial, arbitrary sample on tenants above 5,000 persons (the sweeper's keyset read is the complete one).

## 7. Smallest safe changes - validated only as throwaway experiments

None of this is implemented. SQL is in `deploy/loadtest/screening/experiments/`; each ran on a private database. Equivalence = identical output on the same data.

| change | experiment | before -> after | equivalence |
|---|---|---|---|
| E1 `aml_screening_since` without the correlated NOT EXISTS (window minimum over later-created versions) | `01`,`02` | removes the V^2 term (74.7 ms at V=1000; after: not timed separately inside the sets below) | 6 random policy histories (4 tenants x 3 jurisdictions, ties in created_at, future/backdated effective_from, foreign after-policy ids, NULL/empty lists) x 6,000 random inputs each: 0 mismatches in 36,000 inputs |
| E2 guard before `since()` in the `enqueue_due` candidate: skip the call when `p_at` is not before the latest started version and the claim used that version (result is provably NULL) | `06` | 1.83 -> 0.55 s (10k), 8.4 -> 2.5 s (50k), 32.2 -> 8.4 s (200k): 3.3-3.8x | full candidate relation (person, last_any, covered) identical at 10k / 50k / 200k persons (0 rows differ either way) |
| E3 evaluate the enqueue candidates per keyset slice (2,000 persons per call) | `07` | 2,000-person slice 76 ms, 500-person 25 ms at a 50k tenant (full: 2.5 s guarded / 8.7 s original); cycle of 25 calls | union of the 25 slices = full relation, 0 differences |
| E4 set-based gate evaluation for a person set (one SQL instead of one plpgsql call per person), with E1 + the guard | `03`,`04`,`05` | 500 persons 575 -> 32 ms; 5,000 persons 4,589 -> 225 ms; all 10,000 persons 10.4 s -> 0.49 s (18-21x) | fn-10k: denied sets equal for 500 / 5,000 / 10,000 persons (543 denied, 0 differences); randomised fixture (3 tenants x 3 jurisdictions, 1,200 persons, matches, holds, lifts, resolutions, auto-reviewed re-flags, pending/failed requests) with 6 seeds: 0 mismatches, denied counts 332-934 of 1,200 so both outcomes are covered |
| E5 `parked_summary`: partial index `(tenant_id, person_id, decided_at DESC, id DESC) INCLUDE (...) WHERE operation='withdrawal_payout'` + per-person LATERAL for the persons count | `08`,`09` | 4M decisions: 6.0 -> 1.1-1.5 s | identical (persons, evaluations) for 7 windows |

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
| T4 sweeper full observation cycle | <= the gate alert threshold (policy value, 1 h in the fixtures) | today P/500 x 15 s = 100 min at 200k |
| T5 withdrawal gate call p95 | <= 10 ms with the worker running | today median 2.4 ms and p95 5 ms with 4 callers on their own pool, median 9 ms / p95 15 ms on the shared 10-connection pool |
| T6 recovery after a worker crash | lease + 1 pass (about 2.3 min today) | measured 137 s |

Owner decisions these depend on (not chosen here): (1) the acceptable time a payout may stay parked for screening after a policy change (mass-stale drain objective: minutes? hours?); (2) the largest tenant (persons) and the number of tenants the platform commits to serve on one worker; (3) whether a `failed_terminal` screening may keep a person gated until the next re-screen bucket or must be re-queued automatically; (4) the cadence of scheduled enqueue relative to the claim loop; (5) whether lease length (and so crash-recovery time) may grow to allow larger claim batches; (6) the real number of policy versions expected per tenant.

## 9. Scenarios NOT run, and why

* `stale-2k-gate`, `stale-2k-batch20` (scenario files exist), `run-p50k-l0`: dropped to bound the time after the container restart; the batch/lease lever in 4.5 is therefore a HYPOTHESIS. Gate-caller effects on the drain were measured on the 100-person backlog instead.
* Retry storm to `failed_terminal` / alert / re-queue (needs a window > 600 s or a compressed backoff); worker count > 1 (the `workers` parameter exists, not run); multiple jurisdictions per tenant (`jurisdictions` parameter exists, not run; note the gate cost scales with the number of gating jurisdictions, HYPOTHESIS); lognormal / bimodal provider latency and partial failure rates (the provider supports them, only fixed latency, hang and 100 % failure were run); RunPass with the 50k tenant; a fleet of 500 tenants at latencies above 100 ms and of 100 tenants over 10 min windows; alert delivery (the alert rows are created in the same transaction, delivery is not exercised).
* Cells that were capped (listed "capped" in 4.4) measure the throughput of a time window, not a complete pass; complete-pass durations for them are EXTRAPOLATIONS and are labelled.
* Production data volumes, real vendor latency distributions and a multi-node database are out of scope (CLAUDE.md: synthetic data, no production access).

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
./run.sh drop-all
```
Scenario files: `deploy/loadtest/screening/scenarios/`. Raw results of this report: `docs/plans/capacity-baseline-data/` (`results-json.tar.gz` holds every `<scenario>.<mode>.runN.json` and `<scenario>.seed.json`; plain `explain/`, `env-info.txt`, `run-logs.txt`; unpack with `tar xzf` into a results directory and point `summarize.py` at it).
Known harness caveat: when a run's `DROP DATABASE ... WITH (FORCE)` is refused ("permission denied to terminate process", seen once for `run-t500-l0`) the script stops; clean up with `./run.sh drop-all`.
