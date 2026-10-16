# A13a - test-infrastructure hygiene record

Scope: TEST-HYGIENE-1 (scratch database leaks), `migrate verify` end-to-end check, TEST-ISOLATION-WATCH-1 write-up.
ENV-GO-CACHE-1 context: the repeated `go clean -cache` runs and the killed long runs were driven by disk pressure from
leaked scratch databases; this change attacks the cause (leaks), not the cache.

## 1. TEST-HYGIENE-1 - what changed

- `internal/testsupport/scratchname` (untagged, pure): `BuildName`, `ParseName`, `SelectReapable`. Name pattern
  `<prefix><yyyymmddhhmmss UTC>p<pid>x<8 hex>`. Kept in its own untagged package because the CI guard requires every file
  in `internal/testsupport/scratchdb` to carry `//go:build integration`; the pure logic is therefore unit-tested in the
  default `go test ./...`.
- `internal/testsupport/scratchdb`: `New` uses `BuildName` (fails the test on a bad prefix or a name over 63 bytes; all
  existing call-site prefixes were scanned and conform). New `Reap(ctx, adminURL, ownerURL, maxAge)` and the opt-in
  `TestReapLeakedScratchDatabases` (needs `SCRATCHDB_REAP=1`).
- Reaper safety: exact pattern only, age from the name timestamp (future timestamps never reapable), owner must equal
  `TEST_DATABASE_URL`'s role, not a template, zero sessions, plain `DROP DATABASE` (no `FORCE`), `maxAge <= 0` refused,
  never the connected database. Uses the existing `igaming_test_admin` credentials; no role, password or grant was created
  or changed. The admin credentials worked.
- Not covered (by design): legacy-format leftovers (`prefix` + 16 hex) cannot be aged and are not reaped; databases
  created by `priv_db.sh` with arbitrary names are not reaped unless named with the pattern (documented in
  `docs/testing/testing-strategy.md`, "Scratch-database names and the leak reaper").
- Not done (proposed in the registry row, out of this scope): clone-from-template to avoid one fully migrated database per
  test (the m0101v2_ pattern), and retry/fail on failed cleanup beyond the existing bounded retry.
- Found while testing: my first integration-test version leaked its databases because `t.Cleanup` ran after a deferred
  `Close` of the admin connection; fixed (fresh connection, errors reported). The leaked `reaptest_*` databases (all created
  by this work) were dropped explicitly.

## 2. `migrate verify` end to end (fresh private database)

Private database `a13priv_20261016072634p1xa13a13a1` (pattern-named, owner `igaming`, built by `priv_db.sh`, dropped at the end).

| Step | Result |
|---|---|
| `migrate up` from zero (priv_db.sh) | 153 migrations applied |
| `migrate verify` | all 153 OK, no version gaps |
| `migrate -steps=1 down` x153 (one at a time, `APP_ENV=development`) down to zero, then `up`, then `verify` | 153 downs succeeded, re-up clean, verify clean, no gaps |
| `go test -tags integration ./internal/db/... ./cmd/migrate/...` on the private DB | pass |

No defects found in the migration chain. One tidy: `reservedUnusedVersions` in `internal/db/migrate.go` already listed only
143-150, 159, 161 (158 was not in the map; only the comment still said 158 was reserved). The comment was corrected and
`TestReservedUnusedVersions_ExactSet` pins the exact set and that a missing 158 is reported.

## 3. TEST-ISOLATION-WATCH-1 - known suspects and current mitigation

Evidence (from the registry / HANDOVER, class B): in one full local core run under CI grants, 3 tests failed that did not
reproduce alone, in two further parallel runs, or on the hosted core shard:

1. `internal/aml` `TestEligibility_PlatformOfficers_*` (two quorum tests, e.g.
   `TestEligibility_PlatformOfficers_SuspendedOrDemotedAreNoOfficersAndNoQuorum`). They assert an exact platform-officer
   quorum (two live of three) and toggle staff status, so they are sensitive to any other package changing platform
   officer / staff / capability-grant rows in the same database at the same time. Hypothesis, not proven.
2. `internal/reconciliation` `TestCasinoStatement_NilOrErroringSourceFailsClosed`. It asserts "no partial rows" via a run
   count; sensitive to another package writing reconciliation runs for the same fixture scope concurrently. Hypothesis,
   not proven.
3. `internal/rg` timing-sensitive tests (statement/lock timeout fail-closed, `limits_s3_perf`, `limits_s4_heavy`): assert on
   elapsed time or on fail-closed behaviour under a short timeout, so they are sensitive to CPU and lock pressure from
   parallel packages. The A6 fix (deterministic lock-held timeout test, `7347eba`) removed the wall-clock dependence for
   the statement-timeout case; the perf/heavy tests remain load-sensitive.

Shared mechanisms that can interfere (all in one shared database per run): table-level locks and trigger-disable fixtures
(`ALTER TABLE ... DISABLE TRIGGER`, TRUNCATE-class probes), global counts of sessions or rows, wall-clock bounds.

What CI does now (`.github/workflows/ci.yml`, A1 hosted run #828 and the follow-ups; unchanged by A13a):

- Integration is 16 shards, each with its OWN postgres service (no cross-shard state).
- Core shards (`core:*` and `core-rest`) run packages ONE AT A TIME (`-p 1`); the `core-rg-recon-aml` shard puts rg,
  reconciliation, aml, launchgov and tenant in one serial shard. httpserver and payments shards are test-name partitions of
  a single package (so no inter-package parallelism within them).
- Wall-clock security bounds are excluded from the shards (`-skip`) and run alone in the `timing-lane` job.
- Probes that counted whole-database state were scoped (kyc `TestOutboxState_59` by `application_name`; lock-wait polling
  scoped to the test's own blocker, see testing-strategy.md).

Residual risk: a local `go test -tags integration ./...` (parallel packages, one database) can still show these 3 failures;
use `-p 1` (or the per-package runner) for gate runs. The watch item stays open until a final-verification full run is
clean; if a failure recurs, capture the full log and the concurrently running packages before re-running.
