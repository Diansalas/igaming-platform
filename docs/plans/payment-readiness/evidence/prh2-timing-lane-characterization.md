# TestResolutionIsolation_NormalOperation - timing characterization (2026-10-05)

Raw data: scratchpad/tl/keep/results{,2,3}.jsonl (one JSON per rep, all callback latencies), an.py (analysis), drive.py (driver/sampler), patch.py (note() patch), stmts_*.txt (SQL per callback).
No repo file changed, nothing committed. Throwaway copies (git archive) and private DBs tl_char_* removed/dropped.

## Environment
- 4 vCPU Intel Xeon @ 2.80GHz (1 thread/core, 4 cores), 16 GB RAM (~14.8 GB free), no swap. Go 1.26.8. Linux 6.18 (Firecracker VM).
- PostgreSQL 16.13 (Ubuntu), same cluster as CI-local: max_connections=100, shared_buffers=16384 (8kB pages = 128MB), fsync=on, synchronous_commit=on, full_page_writes=on, wal_level=replica, work_mem=4MB, log_statement=none, no pg_stat_statements preloaded (extension available but needs cluster restart: skipped). No test-only overrides.
- Test DB: FRESH private DB per commit (priv_db.sh, migrations only), reused across that commit's reps (each rep adds ~3 tenants + 60 players + ~90 ledger txs; a few thousand rows max, tables tiny; exact counts not captured before drop).
- Pool: phasecapture.Pool10Named -> db.Connect(maxConns=10, connect timeout 5s), owner role via TEST_DATABASE_URL. HTTP client: DisableKeepAlives, 30s timeout. httptest server in-process. isoBound=500ms (applied per callback, i.e. a p100 check, failure if >= 500ms). In flight: 10 per tenant x 3 tenants = 30 goroutine chains.
- Workload is 90 timed callbacks per run, not 60: 3 tenants x (10 bets + 10 wins + 10 deposits). All 90 latencies collected.
- Only this test ran (`-run '^TestResolutionIsolation_NormalOperation$' -count=1 -tags=integration`, precompiled `go test -c` binaries, -race unless stated). No other Go builds/tests ran during reps. Before every rep: a 2s /proc/stat sample required >97% idle (all reps started at >=97.12% idle), loadavg and `ps` top-5 recorded in the JSON ("pre"). loadavg-1min before reps ranged 0.54-4.61 but that is the lagging average of my own previous rep (2.5-5s CPU-saturating bursts); the instantaneous idle was >97%. The only non-self hog in the ps top-5 over 135 reps was one stale postgres lifetime-%CPU line (S5_4ff1999_9); `ps` itself and my python driver show as 100% (lifetime ratio of short processes). No competing CPU consumer observed.

## CPU during the timed phase (race build, HEAD; similar for all commits)
Window = first callback start .. last callback end (~0.8-1.0 s), /proc/stat sampled at 100ms:
user 67-71%, sys 13-15%, idle 10-13%, iowait 0.2-0.9%, steal 1.1-2.4%. The 4 cores are ~88% saturated for the whole window: the test is CPU-bound, not IO-bound.
Process CPU over whole run (setup+timed, ~3.8s wall): test binary ~5.8 cpu-s (race) / 2.8 (no race); postgres backends ~4-7 cpu-s (summed from 0.5s samples of backends still alive: approximate and noisy, it varied 3.7-7.3 across batches for identical code, so only "comparable to the test binary" is claimed).
Race removed: wall 2.9s vs 3.8s, test-binary CPU halves.

## Latency distribution, 10 reps x 90 callbacks pooled (ms). Pass = rep with max < 500
Batch 1 (interleaved A,B,C,N,P):
| group | pass | p50 | p90 | p95 | p99 | max | calls>=500 (of 900) | by kind |
|---|---|---|---|---|---|---|---|---|
| HEAD 95b17c5 -race | 0/10 | 279 | 458 | 531 | 619 | 707 | 61 | deposit 57, bet 4 |
| 94b4ae5 -race (FH-7 baseline) | 2/10 | 237 | 414 | 473 | 546 | 631 | 24 | deposit 24 |
| ecd2b74 -race | 1/10 | 250 | 414 | 478 | 552 | 610 | 32 | deposit 30, bet 2 |
| HEAD no -race | 5/10 | 211 | 355 | 412 | 505 | 611 | 11 | deposit 11 |
| HEAD -race taskset -c 0-1 (5 reps, 450 calls) | 0/5 | 364 | 634 | 726 | 867 | 940 | 96 (of 450) | deposit 65, bet 27, win 4 |

Per-run max (ms): HEAD [555,574,570,650,707,622,565,696,617,580]; 94b4ae5 [612,631,573,564,458,543,553,573,496,529]; ecd2b74 [577,562,610,536,541,591,597,461,539,556]; HEAD no-race [535,401,458,427,513,611,517,467,505,420]; HEAD pinned [700,940,931,733,768].
Per-kind p50/p95/max: HEAD bet 275/447/593, win 232/337/410, deposit 350/597/707; 94b4ae5 bet 234/390/494, win 193/294/365, deposit 306/531/631.

Batch 2 (interleaved, all -race, 10 reps each):
| commit | pass | p50 | p95 | p99 | max | >=500 | mean of per-run max |
|---|---|---|---|---|---|---|---|
| ecd2b74 | 1/10 | 257 | 495 | 567 | 674 | 44 | 571 |
| 24ef501 (K3 branch, mig 0115 + code) | 0/10 | 282 | 519 | 614 | 686 | 60 | 616 |
| ca8a68f (E1 branch, mig 0114 + worker) | 0/10 | 253 | 483 | 555 | 630 | 37 | 571 |
| 5d6b746 (merge of both) | 0/10 | 277 | 533 | 625 | 720 | 68 | 629 |
| 4ff1999 | 0/10 | 290 | 526 | 611 | 728 | 68 | 621 |
| HEAD | 0/10 | 275 | 531 | 634 | 777 | 64 | 638 |

Batch 3 (interleaved): ecd2b74 5/10 pass (mean per-run max 500), 082aa82 (= ecd2b74 + migration 0115 only, no Go code change) 0/10 (540), 24ef501 1/10 (557).

Totals: HEAD never passed with -race in 25 reps (0/25: batch 1 10, batch 2 10, pinned 5). 94b4ae5 and ecd2b74 are not green either today: 94b4ae5 2/10, ecd2b74 1/10 + 1/10 + 5/10 = 7/30 across batches.

## Drift between batches
Same ecd2b74 binary: mean of per-run max 559 (batch 1, C), 571 (batch 2), 500 (batch 3). Batch-to-batch environmental drift is ~+-7-13%, the same order as the commit effect, so only within-batch (interleaved) comparisons are valid; pass-rates are not comparable across batches.

## Is HEAD slower than baseline? (Mann-Whitney on per-run values, n=10 vs 10)
- Batch 1: HEAD vs 94b4ae5: per-run max mean 614 vs 553 (p=0.019), per-run mean latency 299 vs 259 (p=0.003), deposit mean 382 vs 335 (p=0.007). HEAD vs ecd2b74: 614 vs 557 (p=0.028). 94b4ae5 vs ecd2b74: no difference (p=0.88 max, p=0.26 mean). So no shift across the 342 commits 94b4ae5..ecd2b74.
- Batch 2: relative to ecd2b74, ca8a68f (E1) no shift (571 vs 571, p=0.82); 24ef501 +8% (p=0.028), 5d6b746 +10% (p=0.034), 4ff1999 +9% (p=0.096), HEAD +12% (p=0.041). 24ef501 vs ca8a68f p=0.041.
- Batch 3: 082aa82 vs ecd2b74: per-run max 540 vs 500 (p=0.041), mean 253 vs 238 (p=0.041), deposit mean 327 vs 302 (p=0.010).
- Pooled ecd2b74-family (30 reps) vs HEAD (20 reps): mean per-run max 560 vs 626, p=0.0002.
Caveats: marginal p-values with several comparisons; effect ~+8-12% on mean latency, deposits (+10-12%) most; bet p95 also up. Not an E1 effect (ca8a68f unchanged). First commit where the distribution shifts: 082aa82 (migration 0115_payment_force_resolution, migration-only commit, no Go change), with 24ef501 (K3 code) adding a little more; E1 (0114 outbox worker) not implicated. Confidence: moderate (statistically suggestive, effect smaller than batch drift, single tree sampled for 082aa82). The earlier 13 commits ecd2b74..082aa82 not separately tested (docs/test only per `git diff --stat`: 082aa82 diff vs ecd2b74 has 3 non-doc files: the 0115 up/down and init-app-role.sql).

## Structural check: SQL per callback (client-side pgx tracer in throwaway copies; pg_stat_statements/log_statement not usable without cluster change/superuser)
| callback | 94b4ae5 | ecd2b74 | HEAD |
|---|---|---|---|
| deposit | 46 stmts, 4 tx | 46, 4 | 46, 4 |
| bet | 42 stmts, 3 tx | 44, 3 | 44, 3 |
| win | 30, 3 | 30, 3 | 30, 3 |
Deposit statement list is identical in 94b4ae5 and HEAD, so deposits are NOT structurally heavier in statements/transactions. Bet has +2 statements (an extra savepoint pair) between 94b4ae5 and ecd2b74, none since. (Statements include BEGIN/COMMIT/SAVEPOINT; counted after a warm-up so credential cache is warm; one callback, sequential.)
Per-statement DB cost did grow with migration 0115: new triggers payment_attempts_operator_column_discipline (plpgsql payment_attempts_guard, ~200 lines, fires on payment_attempts writes) and ledger_transactions_reserved_prefix_guard (fires on every ledger_transactions insert), plus extra RLS policies (pg_policy 344 -> 403, triggers 266 -> 288, functions 191 -> 207 from ecd2b74 to HEAD schema). This is a plausible mechanism for the deposit-heavy +10% (hypothesis, not isolated by a profile).

## Is the 500 ms bound still technically justified?
- The test's own comment: with all chains in flight, -race p100 was 400-590 ms at pool 10, "pure pool throughput", so 500 ms measures CPU saturation. Today's data agree: window CPU is ~88% saturated; per-run max sits at 450-780 ms on all three commits, i.e. the bound is not a knee but inside the body of the p100 distribution (per-run max median 599 HEAD / 558 baseline / 559 ecd2b74 under -race; 486 without -race).
- Removing -race drops HEAD to 5/10 pass; pinning to 2 cores makes it 0/5 with max 940. The result is a function of CPU available to the run, not of resolution/admission behaviour (the stated purpose). In ADR terms the 500 ms bound was never widened but the metric (single worst of 90 under CPU saturation, with race detector) is not a stable discriminator: on today's idle machine even the FH-7 baseline fails 8/10 -race reps. The earlier 40/40 pass cannot be reproduced on this machine/under the same lane; either the original runs were on a faster/less-loaded runner or the margin was always thin. Not determined which.
- What the data do NOT show: a resolution/pool-admission defect (no bet/win slowdown beyond +10%, no store-call-held-in-tx issue measured here), nor an E1/K3 regression attributable to a single commit with high confidence (K3 migration 0115 shows a ~8-12% shift; baseline itself already fails).

## ADR 0094 review PROPOSAL (text only; not applied)
Options with evidence:
1. Keep 500 ms p100 -race as is. Evidence against: 0/25 HEAD, 2/10 baseline, 7/30 ecd2b74 pass; the test is flaky-by-construction on 4 vCPU. A permanently red or "quarantined" lane would violate the no-silent-skip rule; keeping it means the lane fails at the knee regardless of code quality.
2. Change the metric from p100 to a percentile over N samples (e.g. p95 over the 90 callbacks < 500 ms, plus an absolute hard ceiling e.g. 2 s for p100). Data: p95 pooled -race HEAD 531 / baseline 473 / ecd2b74 478 (batch 1) and per-run p95 mean 524 vs 470; so p95<500 would still fail HEAD. A p90<500 (HEAD 458, baseline 414, no-race 355) would pass all groups but the pinned one. Statistically better but still has the CPU-dependence problem.
3. Raise the bound (e.g. 750-800 ms). Evidence: max across 135 -race reps on non-pinned runs was 777 ms (HEAD); pinned 940. 800 ms would pass every unpinned run seen but is only headroom ~3% above the max observed; ADR says the bound is "never widened" because it is a reviewed security criterion (admission), so this needs a recorded decision by security/architect and it weakens the isolation signal.
4. Make the test environment-calibrated: measure a CPU/pool baseline in the same process (e.g. run the same 90 callbacks without resolver contention or against an already-warm single tenant, or measure a calibration loop first) and assert the bad-tenant/outage latency relative to that baseline (ratio) rather than absolute ms. This directly tests what ADR 0094 cares about (admission isolation, no pool pinning) and is robust to -race and runner speed. Highest engineering cost, best signal.
5. Move the [lane] to a dedicated runner with pinned resources (>=8 vCPU, no concurrent jobs) and keep 500 ms. Cheap, keeps the reviewed bound, but the data show even 4 idle cores do not suffice with -race, so the runner spec must be validated (e.g. 8 vCPU run) before it is claimed fixed; I did not measure 8 cores.

Recommendation: combine 4 (primary: relative-to-calibration assertion for admission) with 2 as the interim (p95 over 90 calls < the bound, hard p100 ceiling), and record the decision (ADR 0094 amendment) via architect+security because the 500 ms is a reviewed criterion. Do not silently disable or skip. Until then the lane must be reported as FAILING at HEAD on this 4-vCPU box (0/25), and the 094b4ae5 "40/40" is not reproducible here (2/10). Independently, track the ~10% deposit-path cost increase from migration 0115 as an observation for ledger-finance/K3 owners (hypothesis: new triggers/policies).

## Method notes / limitations
- Latency is measured by the test's own post() (client-side wall, including race-detector and httptest overhead); LAT lines emitted from note() in the patched copies; no timing logic changed.
- Process-CPU split postgres vs test binary is approximate (sampled /proc ticks of surviving postgres backends).
- 082aa82/24ef501 etc. built from `git archive` trees; the test code is byte-identical between ecd2b74 and HEAD; 94b4ae5 differs only in the deposit setup helper (not in the timed phase).
- Bisection covered: ecd2b74, 082aa82, 24ef501, ca8a68f, 5d6b746, 4ff1999, 95b17c5 (not every commit).
