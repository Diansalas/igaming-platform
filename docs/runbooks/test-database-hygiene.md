# Runbook: test and scratch database hygiene (TEST-HYGIENE-1 policy)

Status: policy and procedure (A13). The leak is a local/dev problem only: CI uses an ephemeral service container. **The automatic reaper proposed
in section 5 is `NOT IMPLEMENTED`**; until it exists this policy is manual. Evidence of the problem: roughly 740 leaked scratch databases
(about 13 GB, 150 to 300 new on busy days at about 17 MB each) found on 2026-10-05, from
`docs/plans/prh2-hardening-round/analysis/cleanup-deletion-plan-NOT-EXECUTED.md` (which also holds the one-off deletion plan; nothing in it has been executed).

## 1. How databases are created, and why they leak

- Tests make their own databases through `internal/testsupport/scratchdb` (`scratchdb.New(t, prefix)`: name = prefix + 16 hex, owner = the
  unprivileged application role, created and dropped through `TEST_ADMIN_DATABASE_URL`, the `igaming_test_admin` role, which exists for that helper
  only). `t.Cleanup` runs `DROP DATABASE IF EXISTS ... WITH (FORCE)` on normal test-binary exit.
- Cleanup is skipped when the test binary dies: `go test -timeout` panic, SIGKILL or Ctrl-C, an OOM kill, a tool-call timeout, a database restart. A
  failed drop (for example `max_connections` exhausted by parallel packages) is logged with `t.Logf` only, so it is silent without `-v`.
- Hand-made private databases (`PRIV_DB=...` evidence runs, `igaming_reversibility`) have no teardown at all.

## 2. Rules (apply today)

1. Run long suites with an explicit `-timeout` large enough for the package (the full `internal/httpserver` integration package needs about 33 minutes
   under `-p 1`; hosted CI needs an explicit timeout there too) so a timeout is not the way the run ends.
2. Run one heavy lane at a time against the shared local PostgreSQL (`-p 1`); parallel packages exhaust `max_connections` and cause drop failures.
3. Name any hand-made database with an owner prefix and a date, record it in the task note, and drop it yourself when the evidence is captured:
   plain `DROP DATABASE <name>;` (no `FORCE`). A database you did not create is not yours to drop.
4. After an abnormal end of a test run, check for leftovers (section 3) before starting the next run.
5. If the disk is nearly full, STOP and report (`developer-handover.md` troubleshooting). Never `sudo`, change roles or passwords, or delete unknown
   databases or worktrees to make room (CLAUDE.md environment safety).

## 3. Read-only inspection (any developer with the test admin URL)

```sql
-- candidates created by scratchdb.New: a 16-hex suffix, not a template, not in use right now
SELECT d.datname, pg_size_pretty(pg_database_size(d.datname)) AS size
  FROM pg_database d
 WHERE d.datname ~ '[0-9a-f]{16}$' AND NOT d.datistemplate
   AND NOT EXISTS (SELECT 1 FROM pg_stat_activity a WHERE a.datname = d.datname)
 ORDER BY pg_database_size(d.datname) DESC;
SELECT count(*), pg_size_pretty(sum(pg_database_size(datname))) FROM pg_database WHERE NOT datistemplate;
```

Never-delete list: `postgres`, `template0`, `template1`, the shared migrated test database (`TEST_DATABASE_URL`), the orchestrator's database, and
anything with a live backend. Hand-named databases hold evidence run state: drop only on the owner's word that the evidence record is sufficient.

## 4. Cleanup procedure (human-authorised, not an agent action on a shared host)

Preconditions: the final technical gate is complete and the owner has authorised cleanup in writing; no test, mutation or agent run is active
(check running `go test`/`.test` processes and `pg_stat_activity`); `git status` clean and the branch pushed (a removed worktree must not be the only
copy of work). Use only `TEST_ADMIN_DATABASE_URL`. Generate the list with the query of section 3, review head and tail, then drop one at a time with plain
`DROP DATABASE "<name>";` and report failures. If a drop is refused, STOP AND REPORT (do not escalate privileges). The ordered one-off plan (worktrees,
branches, helper databases, hand-named databases, verification) is the deletion plan referenced above.

## 5. Proposed durable fix (NOT IMPLEMENTED; owner to schedule, qa + devops)

- Put the creation time and pid in the scratch database name (or a registry row) so liveness is decidable.
- `scratchdb.Reap(ctx, maxAge)` (or `make test-db-reap`): drop databases matching known prefixes that are older than N hours and have no backend,
  using only the existing test admin role.
- Make a failed cleanup loud (`t.Errorf` after a retry) instead of `t.Logf`.
- Create the migrated scratch database once per package and clone it with `CREATE DATABASE ... TEMPLATE` (the `m0101v2_` family alone was 331 databases).
- Replace hand-named `PRIV_DB` use with a script that records and tears down its databases.
