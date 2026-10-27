# lic13: real-stack Back Office end-to-end harness (record)

Status: harness IMPLEMENTED; coverage PARTIALLY IMPLEMENTED (see "Not covered"). Operating-basis specs: NOT IMPLEMENTED (`test.fixme`).
Branch `lic13-e2e-harness` (from da0af90). Files: `backoffice/e2e/` (README there is the operator guide), one `e2e` script in
`backoffice/package.json`. No Go, migration, API or Back Office feature code was changed.

## Why
Back Office tests so far use MSW mocks. This harness verifies the licensing section against the ACTUAL API and database: the real
server decides (database-classified widening/narrowing, two-Person rule, FORCE RLS, step-up, actor proof), the browser only renders.

## Design
* Input: owner and `igaming_runtime` URLs of an already built, fresh PRIVATE database. The harness creates no database, role or
  password and refuses shared database names. Seed tools run with `APP_ENV=development` and the `test` environment anchor.
* Secrets: JWT, staff-MFA factor/MAC keys and the actor-proof key are generated at run time into a 0700 temp dir (0600 files),
  deleted at exit; account passwords and TOTP secrets live in the same dir (`world.json`). No trace/video recording (they would
  capture typed passwords and bearer tokens).
* Seeding through sanctioned tools only: `seed-admin` (first admin; further admins by `-break-glass -force-additional`, only in
  this throwaway DB), `seed-staging-world` (synthetic active tenant/brand/jurisdiction/licence/tenant staff). Only the
  environment anchor, the actor-proof verification key and per-spec licence fixtures are written directly, as the table owner.
* MFA: every account is enrolled over the real HTTP flow (bootstrap token, redeem, start, confirm); specs log in through the UI
  with computed TOTP codes honouring the server's replay rule.
* Static-pin note: a Go helper naming the platform_admin role would violate `TestA14b_OnlySeedAdminReferencesPlatformAdminRole`,
  so the harness has no Go code and drives `cmd/seed-admin` instead.

## Covered (real stack)
Staff login with password + TOTP; unknown account refused; tenant staff: Licensing nav hidden, section denied in UI, server 403 on
`/v1/admin/licences`, `/jurisdictions`, `/tenants`, 401 anonymous, platform admin 200 control; read pages (jurisdictions, licences,
detail, version history, changes); widening licence status change filed -> "Pending approval" and licence unchanged; requester
cannot approve (hint disabled AND server 409 SELF_APPROVAL via the API with the requester token); a second account of the requester's Person
refused; first approval recorded (1 of 2); second account of the first approver's Person refused; a third distinct admin's
approval executes the change and the licence row and version history change (DB-verified); reject by another person; requester
cancel; narrowing change applied at once.

## Findings
* F1 (by design, not a bug): `GET /v1/admin/licensing-changes` answers 200 to tenant staff (tenant-scoped, empty when nothing is
  filed for their tenant); the denial is on licences, jurisdictions and tenants. The UI hides the whole section for tenant staff.
  The spec asserts exactly this.
* F2 (test-infra): `vite preview` serves the app with `/v1` proxied only if `VITE_API_PROXY_TARGET` is set at preview time.
* F3 (UX observation, no change made): for a second account of the requester's own Person, the Approve button is enabled; only the
  server refuses (rendered correctly).

## Not covered
Other change kinds (ceilings, operating policy, jurisdiction config, tenant bind/unbind, validity/review/evidence), the 24 h
stale-approval and 72 h expiry paths, a stale-session step-up prompt (helper handles it if shown), recovery-code login, other Back
Office sections, B2C, real providers, non-Chromium browsers, performance, accessibility audit. Operating-basis screens:
`06-operating-basis.spec.ts` is `test.fixme` until a route exists in `src/app/AppRoutes.tsx`; its selectors are TODOs.

## Residual / decisions for the orchestrator
* Fresh DB per run is required (first-admin bootstrap is once-only).
* `-break-glass -force-additional` creates audit rows and P1 alerts in the private DB; acceptable for a throwaway synthetic DB.
* Adding the script to CI is not done (needs Chromium and a Postgres service); proposal only.
