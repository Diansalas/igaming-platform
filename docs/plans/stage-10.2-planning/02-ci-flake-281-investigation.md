# CI-FLAKE-281 investigation (devops + qa)

Status: **investigation complete, root cause not conclusively proven — classified by strength of evidence.** No test skipped or quarantined; no CI quality gate weakened. One diagnostic-only `ci.yml` change applied (§4).

## 1. What is known going in

- CI run **#281** (run id `36212456896`, job id `108321699935`, commit `a2a0981`, documentation-only) failed at step 18, "Integration tests (RLS, HTTP foundation, race detector)" (`go test -race -tags=integration -v ./...`, 32 packages, `postgres:16` service container).
- Runs **#280** (`516bc0b`) and **#282**/**#283** — identical or near-identical application code — passed. Six local fresh-database runs at the time passed.
- The failing test was never identified: job logs were retrievable only as their last 5,000 lines, which were filled entirely by the Postgres service container's own shutdown log (a long checkpoint/WAL sequence printed when Docker sends the container SIGTERM), and direct log-URL download was blocked.
- `docs/governance/stage-10.1-completion-report.md` §8 already records the Postgres log tail as showing "no deadlock, no fatal error, and no errors other than the ones tests produce on purpose."
- Registered in `docs/governance/task-registry.md` as `CI-FLAKE-281`, owner devops+qa, "In progress (investigation)" under Stage 10.2.

## 2. Attempt to identify the failing test from GitHub's API (this session)

**(a) Check-run annotations for job 108321699935** (`GET /repos/Diansalas/igaming-platform/check-runs/108321699935/annotations`, unauthenticated, worked):

```
1. warning  — Node 20 deprecation notice (unrelated)
2. failure  — path ".github", line 5229, message "Process completed with exit code 1."
3. notice   — ubuntu-latest → Ubuntu 26 migration notice (unrelated)
```

This confirms the step failed but carries **no test name or location** — expected, because the "Summarize integration failures" annotation step (commit `444e6e1`) that emits per-test `::error` annotations was added *after* run #281 (`a2a0981` → `444e6e1` is a later commit), so #281 predates that instrumentation entirely. This is consistent with the completion report's claim that #281's failing test was never named.

**(b) `mcp__github__get_job_logs` tool.** Not present in this session's tool set (no MCP GitHub tools were exposed; no `ToolSearch` tool was available either), so it could not be invoked directly. As a substitute, the equivalent raw REST calls were made directly:

- `GET /repos/.../actions/jobs/108321699935/logs` returns an HTTP **302/303** redirect to a SAS-signed Azure Blob Storage URL (`productionresultssa19.blob.core.windows.net/...`) — this is exactly how GitHub Actions serves full job logs; there is no "get the last N lines only" server-side option in the public API, which corroborates the report's description of the *client-side* 5,000-line tail truncation as the actual constraint, not a GitHub-side limit.
- Following that redirect (`curl -sSL`) failed with `CONNECT tunnel failed, response 403` against `productionresultssa19.blob.core.windows.net:443`. The proxy status endpoint (`/__agentproxy/status`) confirms this in `recentRelayFailures` as `connect_rejected — gateway answered 403 to CONNECT (policy denial or upstream failure)`. Per `/root/.ccr/README.md`, a 403 from the proxy is an **organization egress-policy denial, not to be retried or routed around** — this independently reproduces and confirms the "direct log-URL download is blocked by this environment's proxy" constraint named in the task, and shows it is systemic (blob storage host is simply not on the egress allow-list) rather than transient.
- No other GitHub API exposes step-level log slicing. The **Checks API annotations** (used above) are the only structured, non-full-log surface, and only contain what a workflow step explicitly emits via `::error`/`::warning`/`::notice` — which is exactly why `444e6e1` was the correct fix direction (emit the needed detail as annotations instead of relying on log retrieval).
- Conclusion: **for run #281 specifically, no additional identifying information is retrievable today.** Any future occurrence is expected to name the failing test via `444e6e1`'s annotations (or, after this session's change, via the uploaded `integration-test-log` artifact — see §4).

**(c) Postgres log tail analysis.** The actual job-108321699935 log tail could not be re-fetched in this session for the reasons in (b). As a substitute, this session's own local Postgres log (`/var/log/postgresql/postgresql-16-main.log`) was inspected after running the full integration suite twice (§3) against a fresh, disposable database (`igaming_ci_flake`, not the shared `igaming_platform_ci_local` other agents may be using). The only `ERROR`/`DETAIL` lines produced are recognizable, intentional negative-test cases (e.g. repeated `update or delete on table "tenants" violates foreign key constraint "casino_game_availability_tenant_id_fkey"` from a test that deliberately attempts a blocked tenant delete). No deadlocks, no `FATAL`, no `too many connections`, no statement-timeout or lock-timeout errors, no OOM-killer lines were observed. This is consistent with — though does not independently prove for run #281 itself — the completion report's characterization of the tail as containing only intentional test-generated errors plus the container shutdown sequence.

## 3. Local reproduction attempts

All runs used a **dedicated database** (`igaming_ci_flake`), never `igaming_platform_ci_local`, to avoid disrupting other agents; Postgres itself was never stopped. Script: `scratchpad/ci-flake-281-run.sh` (copy of `scratchpad/ci-local.sh`'s DB/role setup, targeting the new database, running only the integration-test step repeatedly with configurable `GOMAXPROCS`/`-shuffle`).

Environment: 4 vCPUs, PostgreSQL 16.13 local, `max_connections=100`, `shared_buffers=128MB`.

| Run | Settings | Result |
|---|---|---|
| `baseline1` | default `GOMAXPROCS` (4), `-count=1`, no shuffle, no artificial load | 32/32 packages `ok`, 0 `--- FAIL`, 0 panics. `internal/httpserver` took 225.8s (all other packages ≤24s). |
| `stress1` | `GOMAXPROCS=2`, `-shuffle=on` (per-package seeds recorded, e.g. `-test.shuffle 1790412463852451489` for the first package), plus 4 CPU-burn busy-loop processes running concurrently | 32/32 packages `ok`, 0 `--- FAIL`, 0 panics. `internal/httpserver` took 274.1s. |
| `httpserver-stress` (targeted) | `GOMAXPROCS=1`, 8 CPU-burn processes on the same 4-core box, `-run 'TestStage9'` only (the 6 Stage 9 concurrency tests in `internal/httpserver`) | All 6 `--- PASS`. Critically, `TestStage9_ConcurrentLogins_DifferentAccounts_NoPoolExhaustionOrHang` took **22.12s** against its fixed **30s** timeout (`stage9AwaitAll`, `internal/httpserver/stage9_concurrency_integration_test.go`) — a 74% margin consumed under this session's heaviest available artificial contention. |

No failure was reproduced in ~3 focused attempts (budget as instructed). However, the third run identifies a **specific, load-bearing timing margin** that is the most plausible mechanism found:

- `internal/httpserver`'s Stage 9 concurrency tests (`TestStage9_ConcurrentLogins_SameAccount_...`, `..._DifferentAccounts_...`, etc.) spin `n = configuredMaxConns × (2..6)` concurrent goroutines, most of which register or log in a player — i.e. call `auth.HashPassword`/`VerifyPassword`, which use **Argon2id with 64 MiB memory and parallelism=4** (`internal/auth/password.go`, deliberately expensive per Stage 2 security review — OWASP baseline, raised to resist GPU cracking). This is by far the most CPU/memory-intensive workload in the whole integration suite, which is exactly why `internal/httpserver` (225–274s in this session) dwarfs every other package (≤44s even under stress).
- Each of these tests is bounded by `stage9AwaitAll`'s **fixed 30-second wall-clock timeout** (`internal/httpserver/stage9_concurrency_integration_test.go:190`, `t.Fatalf("timed out after %s ... possible deadlock/pool exhaustion")`), independent of `go test`'s own timeout. This is deliberate and correct in intent (an explicit deadlock/pool-exhaustion detector, not a `t.Fatal` on a real assertion), but it is a **fixed real-time budget for a CPU-bound workload**, on a runner whose CPU availability is not something this codebase controls.
- GitHub-hosted `ubuntu-latest` runners are also nominally 4 vCPUs, shared with the host's other tenants (documented "noisy neighbor" variance is a known characteristic of hosted runners), and in the actual CI job this package's tests run while 31 *other* packages are also executing (the default `go test ./...` package-level parallelism is `GOMAXPROCS`), unlike the targeted, isolated run above. A run-#281-specific combination of (a) scheduler/host variance on that particular runner instance, (b) whatever other packages happened to be mid-execution at the same moment, and (c) the already-tight ~25–30% margin this session measured even artificially, is sufficient to cross 30 seconds without any code defect.

A secondary, weaker candidate: the two lock-wait tests that poll `pg_stat_activity` by statement text (`internal/jurisdiction/tenant_licence_admin_integration_test.go`, `internal/operatingmarket/concurrency_integration_test.go`) use a **3-second** polling deadline (`waitForBlockedStatement`) to observe a deliberately-blocked backend. This is the "known W0 deferral" the task named. It is deterministic in mechanism (no `time.Sleep`-only barrier — it polls a real lock wait) but still has a fixed real-time ceiling; under sufficiently severe scheduler contention the *blocking* goroutine's own `INSERT` could plausibly be delayed past 3 seconds before ever reaching the lock. No failure of this kind was reproduced in this session's runs (all `operatingmarket`/`jurisdiction` runs passed), and 3 seconds is a much larger margin relative to the sub-millisecond work involved than the httpserver case, so this is assessed as materially less likely than the Argon2/Stage-9 mechanism.

## 4. CI diagnostics assessment and change applied

**Is the existing `444e6e1` annotation step sufficient?** Yes, for its stated purpose (surviving the log-tail truncation) — it will correctly extract and annotate a `--- FAIL`/`panic`/data-race line and its `_test.go` location the next time this occurs, including a `stage9AwaitAll` timeout (which produces a normal `--- FAIL: TestName (30.0Xs)` line via `t.Fatalf`). It does **not** give full context (e.g. the exact wall-clock/goroutine-count `t.Logf` line just before it, or output from the other 5 Stage 9 tests in the same package for corroboration), and it is capped at 50/30 lines.

**Change applied to `.github/workflows/ci.yml`** (diagnostics only, does not touch the pass/fail gate, added after "Summarize integration failures", before the existing evidence-assertion gate):

```yaml
- name: Upload integration test log (CI-FLAKE-281)
  if: failure()
  uses: actions/upload-artifact@v4
  with:
    name: integration-test-log
    path: integration-test.log
    retention-days: 14
    if-no-files-found: warn
```

Rationale/safety:
- `actions/upload-artifact@v4` is the current major version and, like the `checkout@v4`/`setup-go@v5` already pinned in this workflow, targets a Node runtime that GitHub Actions is already transparently upgrading to Node 24 on this runner generation (visible in run #281's own check-run annotations: "actions/checkout@v4 ... are being forced to run on Node.js 24"). No compatibility concern.
- `if: failure()` only — never runs on a green step, so it can never mask or weaken anything; it is additive.
- Gives a complete, directly downloadable file bypassing both the log-tail truncation and the proxy-blocked blob-redirect path this session hit, for any future failure in this environment or any other.

**Considered, not applied: reducing Postgres container log verbosity.** The service-container `options:` field maps to `docker create` flags, not `postgres.conf` settings, so there is no clean way to lower `log_min_messages` there without either (a) baking a custom postgres image/config just for CI, which is more infrastructure than this flake currently justifies, or (b) suppressing Docker's own log capture (e.g. `--log-driver=none`), which would also hide any **genuine** Postgres-side error (crash, OOM, corruption) that happens to occur near shutdown — exactly the class of evidence the completion report already leans on to say "no deadlock, no fatal error." Given CLAUDE.md's instruction not to weaken what a check can catch, and that the artifact upload above already solves the underlying "we can't get the real log" problem more directly, this was evaluated and **not** applied.

## 5. Classification

| Class | Likelihood | Basis |
|---|---|---|
| **Test concurrency / fixed-timeout margin under CPU contention** (Stage 9 `httpserver` Argon2-heavy concurrent-auth tests, `stage9AwaitAll`'s 30s ceiling) | **Most likely** | Directly measured: this session's own targeted stress run consumed 74% of the 30s budget without any artificial slowdown beyond CPU contention; `httpserver` is measurably the most CPU/memory-expensive package in the suite by a wide margin (225–274s vs ≤44s) purely because of intentional, security-motivated Argon2id cost; CI runners are shared/variable-performance infrastructure this codebase does not control. |
| **DB lock-wait polling under contention** (`operatingmarket`/`jurisdiction` 3s `pg_stat_activity` polls) | Possible, less likely | Same class of fixed-real-time-ceiling mechanism, but far larger relative margin and not reproduced in any run this session. |
| **Resource contention (Postgres connections/locks, `too many connections`, OOM)** | Unlikely | No such evidence in this session's own Postgres log across 3 stressed runs; report's own #281 tail excerpt already says the same. |
| **Migration timing / DB lifecycle** | Unlikely | Migrations run in earlier, unrelated steps that succeeded (steps 13–16); reversibility step never ran (skipped after step 18 failed), so it is not implicated. |
| **Infrastructure (Postgres service container itself, Docker networking)** | Unlikely | Health check passed (step 2 succeeded before any test ran); no infra-level error signature found. |
| **Genuine application defect** | Unlikely | Same commit's code (`516bc0b`→`a2a0981`, documentation-only diff) passed immediately before (#280) and after (#282/#283), and in every local replay including this session's 3 stressed runs — a real, deterministic defect would not be this selectively load-dependent while also surviving deliberate stress attempts to reproduce it. |

**Overall assessment: infrastructure/CI-runner timing contention interacting with a fixed-timeout integration test, not a product defect.** No test's *correctness* logic is in question — `stage9AwaitAll`'s own comment explicitly frames its timeout as a deadlock/exhaustion tripwire, and the underlying operations (Argon2 hashing, DB pool acquisition) are doing real, currently-necessary work.

## 6. Does this block Stage 10.2?

**No.** Reasoning:
- Stage 10.2's scope (KYC-WH-1, CAS-WH-TENANT-1, per `docs/governance/task-registry.md`) does not touch `internal/httpserver`'s Stage 9 concurrency tests, `internal/auth` password hashing, or the lock-wait test helpers in `internal/operatingmarket`/`internal/jurisdiction`.
- The one CI run that failed did so on a documentation-only commit, and the identical code passed both immediately before and after — there is no evidence this is a systemic gate failure that would block legitimate Stage 10.2 changes from merging; it would, at worst, cost an occasional re-run.
- Per CLAUDE.md and this specialist's authority: a CI quality gate must never be weakened to unblock a merge, and nothing here was skipped, quarantined, or loosened — the diagnostics were strengthened instead (§4), and CI-FLAKE-281 stays open, tracked, and unroot-caused, pending its next occurrence with the improved instrumentation.

## 7. If it recurs

On the next occurrence: pull the `integration-test-log` artifact (§4) directly (no tail limit, no blob-redirect dependency), or read the `444e6e1` check-run annotations if the artifact step itself did not run. If the failing test is one of the six `internal/httpserver` Stage 9 tests with a `stage9AwaitAll` timeout message, this investigation's hypothesis is confirmed, and the remedy is a `devops`+`qa`-reviewed increase to that fixed timeout (or making it scale with `configuredMaxConns`/observed runner CPU count) — not a weakening of what the test checks for, and not something this investigation applies unilaterally since it touches test code.
