# Back Office real-stack end-to-end harness (lic13)

Drives the BUILT Back Office in Chromium (Playwright) against the REAL `cmd/platform-api` and a real, migrated, PRIVATE
PostgreSQL database. No MSW, no mocked API. Everything is synthetic.

## Run

```
# 1. a fresh private database, built with the repo's scratch tooling (creates the DB, never roles/passwords):
export PRIV_DB=<prefix>$(date -u +%H%M%S)p$$x01 PRIV_SRC=<this worktree>
bash <scratchpad>/priv_db.sh
# 2. point the harness at it (owner URL + igaming_runtime URL of THAT database):
export E2E_OWNER_DATABASE_URL=...      # default: $TEST_DATABASE_URL
export E2E_RUNTIME_DATABASE_URL=...    # default: $TEST_RUNTIME_DATABASE_URL
bash backoffice/e2e/run.sh             # or: cd backoffice && npm run e2e
# 3. drop the database.
```

The database must be FRESH (no staff rows): seeding uses `seed-admin`'s first-admin mode, which only works once. The script
refuses shared database names (`igaming_platform_ci_local`, `_dev`, `_ci`, anything containing `prod`/`staging`) and refuses
when owner and runtime URLs name different databases.

`run.sh` does, in order: generate run-time secrets (JWT, staff-MFA factor/MAC keys, actor-proof key; a 0600 file in a 0700 temp
dir, deleted at exit, never in git) -> `go build` platform-api, seed-admin, seed-staging-world -> `tsc -b` + `vite build` ->
seed -> start platform-api (APP_ENV=development, runtime role) -> `vite preview` with `/v1` proxied to it -> enrol every account's
TOTP over the real HTTP flow -> Playwright (Chromium from `PLAYWRIGHT_BROWSERS_PATH=/opt/pw-browsers`, global `playwright`
package, `--no-sandbox`, 1 worker) -> `results/summary.{md,json}` + `results/playwright.json`. Teardown kills only the pids it
started (no pkill). Options: `E2E_SPEC=<file filter>`, `E2E_KEEP_RUN_DIR=1` (keeps secrets: debugging only),
`E2E_STACK_ONLY=1` (start the stack and wait).

Disk: `backoffice/node_modules` is symlinked from `/home/user/igaming-platform/backoffice/node_modules` (override
`E2E_FRONTEND_NODE_MODULES`) and removed again at exit; `e2e/node_modules` is a temporary symlink to the global Playwright.

## Seeding (sanctioned tools only)

* `database_environment` anchor `test` and the actor-proof verification key: inserted as the table owner (psql).
* `cmd/seed-admin`: platform admins PA_A (Person P1), PA_A2 (also P1), PA_B (P2), PA_B2 (also P2), PA_C, PA_D (own Persons).
  PA_A is the first-admin bootstrap; the others use `-break-glass -force-additional` (audited, raises the P1 alert, in this
  throwaway database only; a real platform uses the four-eyes platform_invite).
* `cmd/seed-staging-world`: synthetic ACTIVE tenant `e2e-tenant`, brand, jurisdiction `STG-SYNTHETIC`, platform licence
  `STG-SYNTHETIC-1`, tenant staff (tenant_admin, finance x2, compliance, risk_manager). Country `MT` and authorisation reference
  `E2E-SYNTHETIC-NOT-A-REAL-AUTHORISATION` are test fixtures, not any approval of a jurisdiction.
* Per-spec licences (`E2E-<NAME>-<random>`) are inserted by the specs as the table owner (the documented fixture path of the
  governed-write guard) so every spec is independent and repeatable. A suspended licence is the starting point of the widening flow.

## Specs

| File | Covers |
|---|---|
| `01-auth-and-visibility` | login with password + TOTP; bad account refused; tenant staff: nav hidden, UI denial, server 403 on licences/jurisdictions/tenants, tenant-scoped (empty) licensing-change list, 401 anonymous; platform admin control 200s |
| `02-read-pages` | jurisdictions list/filter/detail, licences list/filter/detail, version history, licensing-changes page |
| `03-licence-lifecycle-approval` | widening status change filed -> "Pending approval"; requester cannot approve (UI hint AND server 409 LICENSING_CHANGE_SELF_APPROVAL via the API with the requester token); second account of requester's Person refused; first approval recorded (1 of 2, licence unchanged); second account of the first approver's Person refused; distinct third admin approves -> executed, licence row + history changed (verified in the DB) |
| `04-reject-and-cancel` | reject by another person; requester cancel; licence unchanged, request closed (DB-verified) |
| `05-narrowing-immediate` | suspend filed by a platform admin is applied at once |
| `06-operating-basis` | `test.fixme` until an operating-basis route exists in `src/app/AppRoutes.tsx`; TODO selectors only |

Each login uses one TOTP step; the replay rule (a step is usable once per factor) is handled by `lib/totp.ts`/`lib/world.ts`,
which wait for the next 30 s step when a factor's window is used up, so runs can include waits.

## Not covered

Filing of ceiling / policy / jurisdiction-config / tenant-bind kinds, validity/review/evidence lifecycle kinds, expiry (72 h) and
stale-approval (24 h) paths, step-up *prompt* on a stale session (the helper answers it if it appears; the freshly logged-in
session did not need one in these runs), recovery codes, other Back Office sections, B2C, real providers, browsers other than
Chromium, load. See `docs/plans/lic13-e2e-record.md`.
