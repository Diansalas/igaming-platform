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
| `06-operating-basis` | (a) no-licence basis through nav + tenant picker + explicit dialog, "Pending approval", DB shows no row and API total 0 before approval, requester refused (hint + server 409), two distinct approvers activate it, DB row has `change_request_id`, detail shows 2 approvers; (b) platform-licence basis likewise; (c) withdraw is single actor and applied at once; (d) review: flag re-review applies at once, extend + clear flag is widening (pending, then two approvals via API); (e) tenant staff: nav hidden, UI denial, server 403 on `/v1/admin/operating-bases`, tenant filing refused with 4xx and nothing written; (f) empty review date keeps Submit disabled, unknown operation refused by the server and rendered; (g) past review date accepted as a narrowing review and shown "Review overdue: no longer permitting"; (h) step-up prompt forced (see below) |
| `07-kyc-policy` | (lic21) KYC enforcement policy via the UI as platform admins: author a draft with explicit fields; activation by the author refused (rendered 403, nothing changes); by a second account of the author's Person refused (KP003, no UI hint); by an unlinked-Person admin refused (KP002) for authoring and activation; BELOW the API with the runtime role: KP003, KP002, KP004, AP001-AP004 and the older principal rule; Person-distinct admin activates (DB active); an active policy has exactly one action (Supersede), the API and the database refuse a switch-off and a TRUNCATE, a DELETE removes nothing; real deposit path: a draft governs nothing, over the threshold an unverified player is declined (boundary 4999 / 5000, pending vendor verification also declined), allowed after the compliance reviewer approves the verification; supersede (authors refused, third admin succeeds, enforcement follows the successor); withdraw a draft (author refused, other admin succeeds, no resurrection); tenant staff denied (nav, UI, server 403, 401 anonymous) |
| `08-residence-maker-checker` | (lic21) residence maker-checker via the UI as tenant compliance staff: default None; None -> All applies at once; a determination under All is pending, shown "Not market evidence" and NOT returned by the real market-evidence read (Go `kyc.ReadMarketResidenceEvidence`, runtime role); approval refused for the determiner, for another account of the determiner's Person and for an account of the player's own Person (UI + server + DB); 33 BELOW-API attempts with the runtime role, each with its SQLSTATE and an unchanged-database fingerprint (RD005 incl. future-dated, KV001/KV002 verification guard, 23514, 42501, AP001-AP004, RD004, RD001, RD040) plus a rolled-back control and two characterisation probes; a distinct Person approves and the evidence read returns it; All -> Into open market filed pending, requester and same-Person refused, two distinct Persons activate it; fail-closed open-market declaration (RD010); served market pending, unserved effective at once; the default mode is restored at the end |
| `11-market-config` | (lic32) the lic31 market-configuration filing flows via the UI as platform admins linked to distinct Persons: (a) enable a licence country ceiling (widening, pending, requester and same-Person refused, two distinct Persons approve, row exists, the REAL resolver outcome changes `not_permitted_by_licence` -> `not_configured`), (b) disable a ceiling (narrowing, applied at once), (c) operating-country policy refused at filing while no ceiling/basis exists, accepted after the ceiling through the governed flow, operation-rung disable narrows the real resolver, brand-rung withdraw needs a reference, (d) tenant jurisdiction configuration set/close with the UI class hint compared with `licensing_change_requests.widening` for six steps, (e) jurisdiction creation (single actor, duplicate refused, grants no permission), (f) tenant staff (nav hidden, UI denial, server 403, nothing stored; own-tenant filing by compliance characterised), (g) change detail renders the three kinds in words with the database's approver counts, (h) deny by default |
| `12-screening-policy` | (lic32) the AML screening policy tab as a platform officer: first version with explicit fields, UI client blocks (floor, loosening vs the version in force), the closed refusal codes on the wire by direct calls with the officer session (`rescreen_interval_below_floor`, `policy_loosening_refused`, `policy_effective_from_out_of_range`), refusals reached through the UI with exact wording (`policy_effective_from_out_of_range`, `policy_loosening_refused` against a future-dated latest version, `rescreen_interval_below_alert_threshold` under a gate in force, `screening_worker_heartbeat_missing` on the existing trigger screen), who is refused (non-officer platform admin, tenant staff, other tenant) |

Step-up: `run.sh` starts the API with `STAFF_STEP_UP_CRITICAL_MAX_AGE_SECONDS=60` (`E2E_STEPUP_SECONDS`; 60 s is the lowest value the server accepts, configuration can only tighten). Spec 06(h) ages the session past it (waits 70 s, `E2E_STEPUP_WAIT_MS`) and asserts the "Confirm it is you" prompt appears and the filing then succeeds. Setup helpers (`lib/api.ts`) create prerequisite bases through the real filing + two-approver API route, not by database shortcut.

Each login uses one TOTP step; the replay rule (a step is usable once per factor) is handled by `lib/totp.ts`/`lib/world.ts`,
which wait for the next 30 s step when a factor's window is used up, so runs can include waits.

lic21 additions: `run.sh` starts the API with `TEST_SUPPORT_ENDPOINTS_ENABLED=true` and `MOCK_GEOLOCATION_COUNTRY=MT` (non-production seams: player e-mail verification token in the response, mock KYC vendor, mock PSP; the geolocation mock is a static country for the market gate), builds `tools/dbprobe` into the run directory as `e2e-dbprobe` and exports `E2E_RUNTIME_DATABASE_URL`. Helper files with the `kyc_` prefix under `lib/`: `kyc_db.ts` (owner reads and the runtime-role probe runner, always rolled back), `kyc_proof.ts` (a harness-key signer for the below-API probes), `kyc_world.ts` (extra accounts: platform admins via `seed-admin -break-glass`, an UNLINKED platform admin and extra tenant staff via the documented owner fixture insert with the SF014 trigger disabled for that statement, enrolled over the real HTTP flow; players, KYC verification, deposits through the real API), `kyc_policy.ts`, `kyc_residence.ts`. Specs 07 and 08 restore the shared state they change (the active KYC policy is superseded by a non-binding successor; the residence mode is returned to None through the two-approver route), because neither can be switched off or deleted.

## Not covered

Operating-basis: the market RESOLVER itself (no HTTP endpoint exposes it; "does not permit before approval" is evidenced by the absence of any operating_bases row and API total 0), partner_licence kind, brand/product-scoped bases, restricted products, 72 h expiry. Filing of ceiling / policy / jurisdiction-config / tenant-bind kinds, validity/review/evidence lifecycle kinds, expiry (72 h) and
stale-approval (24 h) paths, step-up *prompt* on a stale session (the helper answers it if it appears; the freshly logged-in
session did not need one in these runs), recovery codes, other Back Office sections, B2C, real providers, browsers other than
Chromium, load. See `docs/plans/lic13-e2e-record.md`.

lic21 not covered: Back Office KYC review screens, the Back Office page for pending vendor results, play-trigger enforcement (casino/sportsbook) with an active policy (the `play` policy is authored and withdrawn but no play is attempted), cross-asset aggregation, a real KYC vendor, a determination made through the Back Office UI (the reviewer determination is made through the real review API as set-up), the 72 h expiry of a pending residence-mode request, the 24 h stale-approval rule, and any signing-key rotation. See `docs/plans/lic21-e2e-record.md`.

lic32 additions: `tools/mcprobe` (built by `run.sh` as `e2e-mcprobe`) runs the REAL `operatingmarket.ResolveOperatingCountryPolicy` over the runtime role in a rolled-back tenant-scoped transaction and prints `RESOLVE|<outcome>`; helper files `lib/mc_world.ts`, `lib/mc_ui.ts` (spec 11) and `lib/sp_ui.ts` (spec 12). Spec 11 files governed changes only through the real routes and the Back Office; the only owner-role write is the documented synthetic licence fixture (`createLicence`). Not covered: see `docs/plans/lic32-e2e-record.md`.
