# Screening capacity harness (CAP-A)

Test tooling that measures the automatic AML screening path (queue, worker, withdrawal gate, parked/overdue reads) against a
PostgreSQL database holding synthetic data. It is **not** part of any normal build or CI run (`//go:build loadtest`), it changes **no**
production behaviour, and it uses **only private databases named `capa_*`** (created from a migrated template, dropped afterwards).
The provider is a **MOCK** with injected latency and failures; there is no real provider.

Report with the measured numbers: `docs/plans/capacity-baseline-report.md`. Raw JSON: `docs/plans/capacity-baseline-data/`.

## What is in here

| path | purpose |
|---|---|
| `scenarios/*.json` | versioned scenario definitions (seed, tenants, persons, skew, policy versions, state mix, provider behaviour, concurrency) |
| `run.sh` | one command per step: `base`, `seed <scenario>`, `measure <mode> <scenario> [runs]`, `explain <scenario> <file>`, `drop-all` |
| `env-info.sh` | records hardware, OS, PostgreSQL version and non-default settings, load average, commit |
| `summarize.py` | aggregates result JSON into markdown tables (median and min-max across runs) |
| `explain/*.sql` | manual reconstructions of the inner statements of the plpgsql functions, for `EXPLAIN (ANALYZE, BUFFERS)` |
| `experiments/*.sql` | throwaway candidate rewrites / indexes, validated on a private database only (nothing here is a migration) |
| `*.go` | the harness (`seed`, `fn`, `growth`, `runpass`, `stale`, `retry`, `crash`/`worker`, `race`) |

## Isolation and safety

* Databases: `capa_base` (migrated), `capa_seed_<scenario>` (seeded), `capa_<mode>_<scenario>` (one clone per measured run). The Go
  code refuses any database name that does not start with `capa_`. `./run.sh drop-all` drops them all.
* Credentials come from the existing harness environment variables (`TEST_ADMIN_DATABASE_URL` for `CREATE/DROP DATABASE` only,
  `DATABASE_URL` the migration owner, `TEST_RUNTIME_DATABASE_URL` the `igaming_runtime` login). Nothing is printed or stored; no role,
  password or privilege is changed; no `sudo`; processes are stopped by pid (`syscall.Kill`), never `pkill`.
* **Seeding is an owner-path bulk load on the scratch database.** For the load only, user triggers (the actor-proof guards, the launch
  guards, the registration enqueue trigger) and FORCE ROW LEVEL SECURITY are switched off on the tables the seed writes, then switched
  back on and **verified against the catalog** before the seed returns (`seed.go`). Table CHECK constraints and foreign keys stay in
  force, so the data has the shape the production functions leave. Two later steps do the same around a single INSERT of a policy
  version (`insertPolicyVersion`, and the churn goroutine in `race`), also only on the scratch database.
* Worker runs (`runpass`, `stale`, `retry`, `crash`, `race`) call the **unmodified** `aml.ScreeningWorker` as the **runtime role** with
  the production pool size (10) and the production config (`DefaultScreeningWorkerConfig`) unless a scenario says otherwise.
* Function-level measurements (`fn`, `growth`) call the production SQL functions on an owner connection (`aml_screening_gate_denies` is
  not executable by the runtime role; it is reached in production through `aml_hold_active`, which is SECURITY DEFINER). Mutating
  functions are timed inside a transaction that is rolled back, so every repetition sees identical state.

## Reproduce

```
export TEST_ADMIN_DATABASE_URL=... DATABASE_URL=... TEST_RUNTIME_DATABASE_URL=...   # existing dev/CI harness values
cd deploy/loadtest/screening
./run.sh base                      # migrate capa_base from this checkout
./run.sh seed fn-10k               # clone + seed
./run.sh measure fn fn-10k 3       # 3 runs, each on a fresh clone; JSON to ./results (or $CAPA_OUT)
python3 -I summarize.py results fn fn-10k
./run.sh explain fn-10k explain/10_gate_denies.sql
./run.sh drop-all
```

Reproducibility: all data is derived from `seed` through `md5(seed:kind:tenant:index)`; the provider RNG is seeded from
`scenario.seed + run`. Timing noise remains (single machine, shared kernel); every measurement is run three times and reported as the
median of the runs with the min-max spread. Record the machine load (`env-info.sh`) and make sure nothing else uses the database.
