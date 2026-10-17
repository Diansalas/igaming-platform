# F5 and F6 record (platform freeze work list, 2026-10-16)

Branch `f5-kyc-residence-ops`, from `origin/claude/focused-wright-jw88w9` at `1f833ec`. Source items:
`docs/plans/platform-freeze-work-list-2026-10-16.md` rows F5 and F6; `docs/plans/market-evidence-kyc-record.md` section 12 (SRME-R2-1);
`docs/plans/staging-acceptance-suite-record.md` (SA-3, SA-4, SA-5). Everything here is provider independent. Migration used: **0170** only
(0166-0169 belong to other agents; 0159 and 0161 stay reserved).

Not done, deliberately: a maker-checker for residence determinations (OWNER decision D, not in scope; SRME-1's step-up class is unchanged),
registry / HANDOVER / progress / active-stage / decision-register / ADR edits.

## 1. Labels (CLAUDE.md)

| Item | Label | Note |
|---|---|---|
| F5(a) SRME-R2-1 carry-forward breakers | `IMPLEMENTED` | SQL in `internal/kyc/market_evidence.go`; tests + mutation pass below |
| F5(b) durable alert `kyc.residence_into_open_market` (Kind, migration 0170, raise site, tests) | `IMPLEMENTED` | alert ROW is durable and atomic with the review |
| F5(b) delivery of that alert to a person | `NOT IMPLEMENTED` (owner configuration) | no route / recipient exists; ALERT-DELIVERY-1 stays OPEN; routing is by severity (p2), see 3.4 |
| F5(b) runbook + routing notes | `IMPLEMENTED` | `observability-and-alerting.md`, `alert-delivery-channels.md` |
| F6 SA-3 `GET /v1/admin/platform/status` | `IMPLEMENTED` | build commit, migration head, `database_environment` anchor |
| F6 SA-3 acceptance-suite use of the route | `NOT IMPLEMENTED` | the suite's version/migration/anchor checks stay `STUB`/SKIP; the route now exists to call |
| F6 SA-4 duplicate log keys on `payments_kill_switch_engaged_alert` | `IMPLEMENTED` | that line only; see residual R6 |
| F6 SA-5 capability PUT | `PARTIALLY IMPLEMENTED` | the reported silent narrowing is NOT reproducible at this commit (see 4.3); the response now states the effective stored result |

## 2. F5(a): SRME-R2-1

### Design
`kyc.ReadMarketResidenceEvidence` picks the most recent approved, unexpired row carrying a reviewer-recorded residence, provided no
"breaker" row is newer than it. The breaker set was `rejected`, `expired`, `review_required`. It now also contains:

* a **decided `unverified`** row (`status = 'unverified' AND provider_reference IS NOT NULL`): enforcement treats it like rejected/expired
  (`enforcement.go`, `orphanRowExclusionSQL`). An orphan intent row (no provider reference) is NOT a decision and still does not break;
* an **`approved` row whose `expires_at <= clock_timestamp()`**: its status stays `approved`, so the status list alone missed it.

"Newer" is unchanged: `(created_at, id) > (determination row created_at, id)`. A lapsed or decided-unverified row OLDER than the determination
does not break it. The latest-decided-row gate (must be approved and unexpired) is untouched.

### Tests (`internal/marketgate/kycresidence_integration_test.go`, runtime role, RLS)
* the two exact section-12 probes, added to `TestKYCResidence_NoVerifiedResidenceMeansNoEvidenceAndRefusal` (every case also asserts the
  three gated operations refuse with the generic evidence-unavailable reason and the locator reports inconclusive):
  (1) approved MT, then a decided `unverified` with provider reference, then approved without a determination -> no evidence;
  (2) approved MT, then an approved row with a lapsed `expires_at`, then a fresh approved without a determination -> no evidence.
* three guards against over-breaking in `TestKYCResidence_SelectionRules`: an orphan intent row between two approvals keeps the carry-forward;
  an approved row still within its expiry between two approvals keeps it; a lapsed approved row and a decided unverified row OLDER than the
  determination do not break it.

### Mutation pass (kill-or-justify), file `internal/kyc/market_evidence.go`, run `TestKYCResidence*` in `internal/marketgate`
| Mutant | Result | Killed by |
|---|---|---|
| M1 drop the decided-unverified breaker | KILLED | section-12 probe 1 |
| M2 drop the lapsed-approved breaker | KILLED | section-12 probe 2 |
| M3 `unverified` breaks even orphans | KILLED | both orphan tests |
| M4 lapsed comparison flipped (`<=` -> `>`) | KILLED | probe 2 + the within-expiry guard |
| M5 any approved row between breaks (expiry condition removed) | KILLED | pending / orphan / within-expiry guards |
| M6 breaker direction flipped (`>` -> `<`) | KILLED | the SRME-2 rejected / expired / review_required probes |
| M7 decided-unverified needs a NULL reference | KILLED | probe 1 + both orphan tests |

7/7 killed, 0 survivors. Script: `scratchpad/f5_mut.sh` (restores the file after each mutant; `git diff` clean afterwards).

## 3. F5(b): alert for `market.residence_into_open_market`

### 3.1 Design
The audit action stays exactly as it was (SRME-1). In the **same transaction** as the review and that audit row, the handler now also raises the
platform alert Kind **`kyc.residence_into_open_market`**:

* Kind: severity **p2** (engineering default), scope platform, `requires_subject = true`, `in_tx_raisable_by_tenant = true`, raise mode `in_tx`,
  `simulation = false`. Closed attribute vocabulary: `verification_id`, `brand_id`, `request_id`. **No country, no player id, no reviewer id,
  no status** (those are in the audit row: `actor_id`, `metadata.player_account_id`, `brand_id`, `new_status`).
* Subject tenant = the server-derived `tc.TenantID`; nothing from the client. The `request_id` attribute is dropped (not failed) by the
  existing charset rule if non-conforming.
* **Dedupe key**: `(subject tenant, kind, discriminator)` with discriminator `verification:<kyc_verification_id>`: one open alert per
  determination; re-reviewing the same verification adds an occurrence to the open alert; once resolved, a later determination opens a fresh
  one. Per-player dedupe was rejected: a second determination for a player (for example a changed country) would hide behind an acknowledged
  or still-open first alert.
* Raise mechanics (ADR 0102): `alerting.InTx` + `RaiseGuarded` in the review transaction; `Pending.Flush` runs deferred after the response.
  A swallowable alert failure (SQLSTATE class 22/23, `42501`, `P0001`) never aborts the review and the mandatory detached retry still records
  the alert. Any other failure class (for example a deadlock) rolls the **whole review back** (HTTP 500, nothing written, reviewer retries):
  a residence determination cannot commit without its signal, the same fail-closed stance as the audit row.
* Migration **0170** only seeds the `alert_kinds` row (0114/0141 pattern: temporary single-Kind insert policy created and dropped in one DO
  block; pre-flight refuses if the Kind exists). Down removes it and **refuses** (foreign key) while any alert of the Kind exists.

### 3.2 Tests
* `internal/alerting/migration_0170_integration_test.go`: seed row (p2, allowed keys exactly the three, no country key), down round trip,
  down refusal while an alert exists, Kind left in place after the refusal. `TestKindParity_FullColumn_BothDirections_50` (existing) now also
  covers the new Kind in both directions Go <-> database.
* `internal/alerting/residence_kind_test.go` (unit): definition, closed vocabulary, `country` / `verified_residence_country` /
  `player_account_id` / `reviewer` / `country_code` refused by Go validation, subject required.
* `internal/httpserver/kyc_flow_integration_test.go`: `TestKYC_ReviewResidenceIntoAnOpenMarketWritesAnAlertableAuditRow` extended: exactly one
  open p2 alert keyed `verification:<id>`, one occurrence, attributes ids only and no country anywhere; a residence into a closed country and a
  review without a residence raise none. New `TestKYC_ReviewResidenceAlert_FailureModes`: injected `P0001` -> review 200 and the detached retry
  records the alert; injected `40P01` -> review 500, no audit row, verification status unchanged.

### 3.3 Runbook
`docs/runbooks/observability-and-alerting.md`: row in "Which events now raise durable alerts" and a section "Runbook:
`kyc.residence_into_open_market`" (who reviews: a second person, never the recording reviewer, a different compliance officer or tenant admin /
platform admin; what to check: the audit row, the documents behind the decision, that the country is in the intended market set, patterns per
reviewer, correction by a new review, never by editing `kyc_verifications`). Honest limit stated there: no API returns the country, so
comparing it with the documents needs an authorised support query.

### 3.4 Routing
`alert_routes` is keyed by severity and escalation step, not by Kind. No per-Kind rule exists in the platform, and none was invented. The Kind
is covered by whatever p2 route an operator configures (`alert-delivery-channels.md` section 3, now with a note) and by the existing generic
`AlertUndeliveredP2Old` rule. No recipient was seeded (HD-PRH2-4). Until an operator routes p2 to a person, the alert is a durable row nobody is
paged about.

## 4. F6

### 4.1 SA-3: `GET /v1/admin/platform/status`
* Registered `mux.Handle("GET /v1/admin/platform/status", staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermPlatformStatusRead)(...)))`
  in `internal/httpserver/platform_status_routes.go`; golden table row `none` (a read never steps up); static pin 222 -> 223 routes, 135 mutating
  unchanged; OpenAPI path + `PlatformStatus` schema; DB-free OpenAPI both-ways pin.
* **Permission**: new `platform_status:read` (`auth.PermPlatformStatusRead`), granted **only** to `RolePlatformAdmin` (pinned over the keys of
  `rolePermissions`). It is deliberately not `alert:manage`: this describes the platform, not alerts. Three layers: permission, explicit
  `tc.TenantID == uuid.Nil` (`platformAlertCaller`), staff chain (live aal2 session). Not added to `backoffice/src/auth/permissions.ts` (no UI).
* **Response** (exactly three objects): `build.commit` (validated 7-64 lower-case hex or `unknown`), `migrations.head_version` and
  `migrations.applied_count`, `database_environment.{anchor, recorded}` with `anchor` in `production|staging|development|test|unrecorded|unrecognised`.
  No connection string, host, role, secret reference, checksum or tenant data.
* **Build commit**: new `internal/buildinfo` (`-X .../buildinfo.Commit=<sha>`; falls back to the toolchain VCS stamp, then `unknown`; anything
  not matching `^[0-9a-f]{7,64}$` is ignored, so a hostile build argument cannot reach a response). `deploy/docker/platform-api.Dockerfile` takes
  `ARG GIT_COMMIT=unknown` and passes it for `platform-api` only; `deploy/aws/scripts/deploy.sh` passes `--build-arg GIT_COMMIT=${IMAGE_TAG}`
  (the commit sha). Not exercised here (no docker build in this environment).
* **Migration head**: `db.Pool.ReadMigrationHead` = `SELECT COALESCE(max(version),0), count(*) FROM schema_migrations` through the runtime pool.
  Grants checked: the runtime role keeps SELECT on `schema_migrations` (blanket grant; only INSERT/UPDATE/DELETE are revoked,
  `deploy/init-app-role.sql`, `init-runtime-role.rds.sql`) and the table has no RLS, so no owner connection and no SECURITY DEFINER function.
  The anchor uses the existing `db.Pool.ReadEnvironmentAnchor` (runtime SELECT on `database_environment`).
* **Audit**: like the other platform reads, a platform-scope row `platform.status_read` (tenant_id NULL, no subject tenant: the read concerns
  the platform, not a tenant; empty metadata) is committed BEFORE anything is read; if it cannot be written the response is 500 and carries
  nothing (injected-failure test).
* Runbooks: `operational-runbooks.md` section 1 step 4 and `production-configuration-checklist.md` (anchor section).

### 4.2 SA-4: duplicate `tenant_id` on `payments_kill_switch_engaged_alert`
Cause: the handler passed the request-context logger (`observability.LoggerFromContext`), which already carries `request_id` and `tenant_id`
(the caller's own tenant; the nil UUID for a platform admin), and the alert line then added its own `tenant_id` (the affected tenant) and
`request_id`: both keys twice. Fix: the call site writes the line through the base logger (`deps.Logger`); the line states the affected tenant
once. Not renamed: `log-event-alert-rules.yml` and the runbooks name `tenant_id` as the affected tenant. Tests: unit
`TestLogKillSwitchEngagedAlert_NoDuplicateKeys` (every key once; the nil UUID never appears; plus a premise check that the context logger does
duplicate) and, over the whole HTTP stack, `TestPaymentsKillSwitchAPI_AuditRecordsPlatformTakeover` now asserts both alert lines (tenant
engage, platform takeover) have every key exactly once and the real tenant id.

### 4.3 SA-5: capability PUT
**Finding not reproduced.** At this commit `PUT /v1/admin/providers/{id}/capability` does not silently narrow: a payment method outside the
adapter's manifest (`sepa` on the mock, alone or with `card`) is **400** (`payments.ErrCapabilityWidensAdapter`, message names the field) and
writes nothing; an unknown JSON field is 400 (`DisallowUnknownFields`); the stored row equals the request exactly. The existing OpenAPI text and
the backoffice widening test already said "a widening attempt is 400". I could not find a path that returns 200 with a dropped value; the
acceptance run's `{card}` is not explained by the code (candidates: an earlier build, or a different request body). Nothing was "fixed" for the
reported symptom, because there is none to fix; I did not invent one.

What was done instead, as the task allows ("return the effective result explicitly"): the 200 response is now
`{"id", "effective": {...stored configuration, every list present...}, "narrowed": bool}`. `effective` is read back with
`payments.LoadCapability` inside the SAME transaction as the write; `narrowed` is true if it differs from the request (lists as sets, amount
limits by asset). Today it is false on every 200 (a wider request is a 400), so the field makes any future divergence explicit; a divergence also
logs `provider_capability_effective_differs_from_request`. **Additive and non-breaking**: `id` is unchanged and the backoffice type keeps
`effective` / `narrowed` optional. OpenAPI: new `WriteProviderCapabilityResponse`, the operation text now states "never silently reduced".
Backoffice: `PaymentCapabilityResponse` type, the success alert shows the stored payment methods and currencies and a warning when `narrowed`,
new `ConfigurationPage.test.tsx` case (existing cases untouched; they return `{id}` only and still pass).
Tests: unit `capabilityNarrowed` (13 divergences, order and nil-vs-empty ignored), response builder, OpenAPI schema vs DTO; integration
`TestProviderCapabilityPut_ReportsTheEffectiveStoredConfigurationAndRefusesWidening` (response == stored row; `sepa` alone and with `card` -> 400
with the stored row, limit rows and audit count unchanged; limit below the adapter range -> 400; unknown field -> 400; a narrowing replacement
reports the new smaller set).

### 4.4 Mutation pass (kill-or-justify), F6 and F5(b) code
| Mutant | Result |
|---|---|
| SA-4 call site back to the context logger | KILLED (`...AuditRecordsPlatformTakeover`) |
| SA-3 audit write skipped | KILLED (audited-read test, fail-closed test) |
| SA-3 migration head `max` -> `min` | KILLED |
| SA-3 handler tenant-scope check dropped | first run SURVIVED (the permission and the staff session still refused every tenant caller); added the DB-free `TestPlatformAlertCaller_RefusesATenantScopedPrincipal`; re-run KILLED |
| SA-3 `RequirePermission` removed | first run did not compile (unused import, not a kill); replaced by "wrong permission `alert:manage`": KILLED by the source pin `TestPlatformStatusRoute_RegisteredWithTheDedicatedPermissionAndClassNone`. Note: the layered tenant check hides a missing permission at HTTP level for tenant callers, so the pin on the registration is the control |
| SA-3 permission granted to `tenant_admin` | KILLED (`TestRoleHasPermission_PlatformStatusRead_ExactlyPlatformAdmin`) |
| SA-3 commit validation removed | KILLED (`TestResolveCommit`) |
| SA-5 `narrowed` always false | first run SURVIVED (field set outside the unit-tested builder); moved the comparison into `toWriteCapabilityResponse`; re-run KILLED |
| SA-5 `narrowed` ignores payment methods | KILLED |
| SA-5 widening check on payment methods disabled | KILLED (`...RefusesWidening`) |
| F5(b) alert raised without its subject tenant | KILLED (open-market review test) |

## 5. Changed existing assertions (none weakened)
| File | Change | Why |
|---|---|---|
| `internal/httpserver/staff_route_static_test.go` | pinned route count 222 -> 223 (mutating 135 unchanged) | new read route, golden table updated together |
| `internal/httpserver/staff_route_golden_test.go` | + `"GET /v1/admin/platform/status": "none"` | same |
| `internal/httpserver/staff_stepup_integration_test.go` | coverage pin `read` 87 -> 88 | same (found by the first full run, fixed, re-run green) |
| `internal/httpserver/kyc_flow_integration_test.go` | `TestKYC_ReviewResidenceIntoAnOpenMarketWritesAnAlertableAuditRow` gained alert assertions | strengthened, none removed |
| `internal/httpserver/payments_kill_switch_api_integration_test.go` | takeover test gained duplicate-key assertions | strengthened |
| `internal/marketgate/kycresidence_integration_test.go` | two probe cases added to the no-evidence table, three guard sub-tests added | strengthened |
| `docs/api/openapi/platform-api.yaml` | review description (alert), capability PUT 200 schema, new path + 2 schemas | honest contract |

## 6. Verification (private scratch database, runtime role, `-tags integration -count=1 -p 1`)
Setup: `PRIV_DB=f520261016215505p7x01 PRIV_SRC=<worktree> bash priv_db.sh` (rebuilt once after adding 0170), tests through `priv_test.sh`. Scratch
database dropped at the end. Counts are `--- PASS/FAIL/SKIP` lines of `go test -v` (subtests included).

| Run | PASS | FAIL | SKIP |
|---|---|---|---|
| `go test -count=1 ./...` (unit, no tags) | 75 packages ok | 0 | - |
| `./internal/kyc/` | 460 | 0 | 0 |
| `./internal/marketgate/` | 107 | 0 | 0 |
| `./internal/tenant/` | 64 | 0 | 0 |
| `./internal/launchgov/` | 129 | 0 | 0 |
| `./cmd/...` | 135 | 0 | 0 |
| `./internal/alerting/...` (re-run with `-timeout 90m`; the first run hit the 10 minute default under machine load, no assertion failed) | 248 | 0 | 1 |
| `./internal/aml/` | 174 | 0 | 0 |
| `./internal/auth/ ./internal/db/ ./internal/buildinfo/ ./deploy/observability/` | 499 | 0 | 1 |
| `./internal/payments/ -run 'KillSwitch\|Capability'` | 37 | 0 | 0 |
| `./internal/httpserver/ -run 'MarketGate\|Register\|StepUp\|Classif\|StaffRoute\|Staff\|KYC\|OpenAPI\|Capability\|KillSwitch\|PlatformStatus\|PlatformAlertCaller\|ProviderCapability\|Alert\|Narrowed\|ToWriteCapability\|TestLogKillSwitch'` (`-timeout 90m`; the first run under load timed out at 10 minutes, the second found the `read 87 -> 88` pin, fixed in `50feed9`; this is the clean third run) | 548 | 0 | 0 |

`gofmt -l .` clean; `go vet ./...` and `go vet -tags integration ./...` clean; golangci-lint 2.9.0 0 issues. Backoffice: `npm run build` OK;
`npm test` (vitest): 56 files, 644 tests, 0 failed (an earlier run overlapping the integration suites failed 3 unrelated alerts/launch tests on
timing; they passed alone and in the clean full run). `node_modules` was symlinked from the main checkout for the run and removed afterwards.
`git diff origin/claude/focused-wright-jw88w9 --stat -- docs/governance` is empty (verified at the end; origin had not moved from `1f833ec`).

## 7. Residuals and honest limits
* **R1** Alert volume: it fires on EVERY residence determination into a served market, p2. Whether that should be p3, batched, or sampled is an
  owner/compliance call; nothing pages anyone until a p2 route exists.
* **R2** The reviewer of the alert cannot read the country through any API (by design). The runbook says so and points at a support query.
* **R3** Migration 0170 leaves gaps 0166-0169 on this branch alone: `cmd/migrate verify` tolerates only the reserved set, so it will report the
  gap until the other agents' migrations merge. `migrate up` is unaffected (the scratch database was built with it).
* **R4** SA-5's reported behaviour is not reproduced (4.3). If the acceptance run can be repeated against a current build and the symptom
  persists, capture the exact request body and the stored row.
* **R5** The Dockerfile / `deploy.sh` build argument is untested here. Until an image is built with it the route reports `unknown` (or the
  toolchain VCS stamp if the build tree is a git checkout).
* **R6** SA-4 is fixed for the one named line. 26 other `logger.Error/Warn(..., "tenant_id", ...)` call sites in `internal/httpserver` pass a
  context logger the same way; on authenticated staff/player routes they duplicate `tenant_id` identically (webhook routes carry no context
  tenant, so they do not). A central fix (de-duplicating context attributes in the logging layer) is a separate observability decision.
* **R7** The acceptance suite still reports version / migration head / anchor as `STUB`; wiring it to the new route is follow-up work.
* **R8** The residence determination still has no maker-checker (owner decision D). The alert is detective, after the fact.

## 8. Probe lists for security review
F5(a)
1. For each of: decided `unverified` (with reference), orphan `unverified` (no reference), lapsed `approved`, unexpired `approved`, `pending`,
   `review_required`: place it (a) between the determination row and the latest approval, (b) older than the determination, (c) as the latest
   row; confirm evidence / no evidence per sections 2 and the table of enforcement.
2. Equal `created_at` timestamps with ids ordering the rows (tuple comparison) around the determination.
3. A determination row that is itself `unverified`/lapsed/unapproved never counts (unchanged), and a second account of the same person gets no
   carry-forward.
4. Tenant B's rows never influence tenant A's evidence (RLS + the explicit scope assertion).

F5(b)
5. Review with `verified_residence_country` in a served country: one alert, ids only; grep the alert row, occurrences, audit metadata and logs
   for the country; closed country and no-residence reviews raise none.
6. Force each failure class on the alert insert (`P0001`, `23xxx`, `40P01`, `55P03`, `57014`): swallowable -> review commits and the detached
   retry records it; others -> review rolls back, no audit row, status unchanged.
7. Same verification reviewed twice: one open alert, two occurrences; resolved then re-reviewed: a fresh alert.
8. `X-Request-Id` with hostile characters: attribute dropped, alert still raised (existing N-1 rule).
9. Tenant staff of tenant B cannot see or act on tenant A's alert; the subject tenant sees it read-only; no tenant ack endpoint exists.
10. Migration 0170 up/down on a database holding alerts of the Kind (refused), and on one without (clean).

F6
11. `GET /v1/admin/platform/status`: no token, player token, service token, every tenant role, a tenant-scoped token claiming `platform_admin`,
    a revoked/stale staff session, a platform admin without a fresh second factor (must still work: class none), HTTP methods other than GET.
12. Response contents: only the three objects; no host/role/secret/checksum; `commit` is hex or `unknown` even with a hostile `GIT_COMMIT`
    build argument (quotes, newline, shell metacharacters, 200 characters).
13. Audit: exactly one `platform.status_read` per successful call, tenant_id and subject_tenant_id NULL, no metadata; none for refusals;
    the read returns nothing when the audit insert fails; the row is visible to no tenant.
14. The migration head is read with the runtime role only (confirm no owner URL is available to the handler).
15. Capability PUT: widen each list / flag / limit by one element and check 400 + unchanged row + unchanged audit count; an unknown field;
    a body with duplicate keys; compare `effective` with a direct row read for the tenant (and that a platform caller / other tenant's token is
    refused, unchanged `RequireTenantScope`).
16. Kill-switch engage by tenant admin and by platform admin: the alert line carries each key once and the affected tenant id.

## 9. Independent security review (security specialist, 2026-10-17)

Scope: the six commits `1446bba..128397b` on `f5-kyc-residence-ops` (base `1f833ec`). Code-level and design-level review plus test runs;
not a penetration test. Not in scope: the other agents' migrations 0166-0169, a docker build of the image (R5), alert delivery to a person
(owner configuration), the maker-checker for residence determinations (owner decision D).

**Verdict: APPROVE.** No blocking or launch-blocking finding. SRF5-1 is a Low detective-control gap that should be closed (code or
runbook) before the residence alert is relied on as the compensating control for the missing maker-checker.

### 9.1 Verification run (private scratch database `f5s20261017002042p9038x01`, runtime role, dropped afterwards)
`-tags integration -count=1 -p 1`: `./internal/marketgate -run TestKYCResidence`, `./internal/alerting -run 'Migration0170|KindParity|Residence'`,
`./internal/auth ./internal/buildinfo ./internal/db -run 'PlatformStatus|ResolveCommit|MigrationHead|Anchor'`, `./internal/httpserver -run
'PlatformStatus|PlatformAlertCaller|ProviderCapability|Narrowed|ToWriteCapability|KillSwitch|ReviewResidence|StaffRoute|StepUp_GoldenTable|OpenAPI'`:
all `ok`, 185 PASS, 0 FAIL, 1 SKIP (`TestVerifyEnvironmentAnchor_RealDatabase_FourStates`, pre-existing, unrelated). The fail-closed audit test
(`TestPlatformStatus_FailsClosedWhenTheAuditRowCannotBeWritten`) ran (not skipped). One temporary probe test (not committed, file removed)
reproduced SRF5-1.

### 9.2 What was checked and holds
* **F5(a)** The breaker predicate is now the exact complement of "decided and still good" for rows newer than the determination: rejected,
  expired, review_required, decided unverified (`provider_reference IS NOT NULL`, identical to `orphanRowExclusionSQL`'s notion of a decision),
  approved with lapsed `expires_at`. Non-breakers: pending (SRME-2 design), orphan intent rows, unexpired approved rows. Breakers are only rows
  `(created_at, id) >` the determination row, and because "newer than B" implies "newer than any older A", an older determination can never
  survive a breaker that removed a newer one (monotone; no fallback past a breaker). The provider-callback path cannot approve a staff-reviewed
  `review_required` row (`applyForwardOnlyStatus`), so no provider-only path makes a determination effective. The latest-decided-row gate is
  unchanged; DB error still fails closed; tenant scope assertion + RLS unchanged (`TestKYCResidence_TenantIsolation`, `..._DatabaseErrorFailsClosed`
  pass). No over-breaking (orphan / within-expiry / older-row guards pass).
* **F5(b)** Attributes are server-derived UUIDs plus `request_id`; the closed vocabulary is enforced in Go (unknown key is a validation failure)
  and in the seeded `allowed_keys`; a hostile `X-Request-Id` is dropped, not failed (N-1), so request content cannot turn the raise into a
  failure. Subject tenant is `tc.TenantID`. Swallowed failures retry through `RaiseDetached` on a detached context (a client disconnect cannot
  cancel the retry) and end in the terminal `alerting.raise_failed` fallback; non-swallowable classes roll the review back (tested with 40P01).
  A reviewer has no request-level lever to choose between those paths. Migration 0170 is seed-only; the down block's EXCEPTION handler rolls back
  the trigger disable and the temporary policy with the refused DELETE.
* **F6 SA-3** Three layers hold: `platform_status:read` only on `RolePlatformAdmin`, `platformAlertCaller` (`tc.TenantID == uuid.Nil`),
  `staffRoute` session chain; class `none` matches the other platform reads; golden/static/step-up pins and OpenAPI agree. The audit row is
  committed through `WithPlatformAdmin` before any read; failure returns a body-free 500. The response is a validated hex commit or `unknown`,
  two integers and a closed anchor token: useful only for fingerprinting, and only to the most privileged role, audited. `ReadMigrationHead` runs
  through the runtime pool (`WithoutTenant`), aggregates only; SELECT on `schema_migrations` is the pre-existing blanket grant
  (`deploy/init-app-role.sql`, INSERT/UPDATE/DELETE revoked); this branch changes no grant. `deploy.sh` passes `IMAGE_TAG`, which it already
  forces to 40-hex from a clean tree; `buildinfo` re-validates at runtime, so nothing hostile reaches a response.
* **F6 SA-4** The context logger adds only `request_id` and `tenant_id` (`observability.LoggerFromContext`); the alert line carries both itself
  (affected tenant) plus `changed_by` / `changed_by_scope`, so no correlation is lost; `reason_code` is a validated closed code. No new data.
* **F6 SA-5** `effective` is the caller's own tenant-wide row (RLS transaction, `tc.TenantID`, brand NULL), the same data the caller just wrote;
  no `settlement_behavior` / `callback_capabilities` / other-tenant data. Route keeps `RequireTenantScope` + critical step-up. Additive contract
  (`id` unchanged; backoffice fields optional).
* **Changed assertions** 222->223 routes (mutating 135 unchanged), read 87->88, golden row `none`: each matches exactly one new read route; the
  KYC, kill-switch and marketgate test changes only add assertions. None weakened.

### 9.3 Findings
| ID | Severity | Finding | Concrete failure scenario | Recommendation | Blocks? |
|---|---|---|---|---|---|
| SRF5-1 | Low | The alert fires when the residence is RECORDED, not when it becomes EFFECTIVE. A residence can be recorded on a `review_required` review (non-terminal); a later `approved` review without `verified_residence_country` keeps it (`COALESCE`) and makes it evidence with no audit row, no alert and no occurrence. Likewise a second determination on the same still-`review_required` verification only adds an occurrence to an open or ACKNOWLEDGED alert (`findOpenAlert`: `state <> 'resolved'`). | Reproduced: review `review_required` + MT -> 1 audit row (`new_status` review_required), 1 alert, evidence none; then review `approved` (no residence) -> evidence `MT`, still 1 audit row, 1 alert, 1 occurrence. If the alert reviewer resolved or acknowledged the first alert as "not yet effective (review_required)", the admitting approval, possibly by the same reviewer, is unsignalled. | Either (code) also raise the Kind (same discriminator, or `verification:<id>:approved`) and write the audit action when an approval makes a row carrying a served-market residence effective, or (runbook, minimum) state that `new_status = review_required` is NOT "no effect": keep the alert unresolved until the verification is terminal and re-check on approval. Add a test for the review_required -> approved path. | No (non-blocking; fix before relying on the alert as the compensating control for owner decision D) |
| SRF5-2 | Info | Swallowable-path durability window: the detached retry runs after commit in-process; a process crash between commit and `Flush` loses the alert (the audit row remains). | Requires a swallowable DB failure on the alert insert AND a crash in the next ~1 s; not request-inducible. | Accepted ADR 0102 residual; the audit action remains the record of truth. | No |
| SRF5-3 | Info | `served` is evaluated at review time. A residence recorded into a closed country raises nothing; if the brand later opens that country, those players become admitted with no per-player alert. | Determination into a closed country, then a market-open policy change. | Pre-existing SRME-1 design; the market opening is itself an audited governed action. Record as a known limit in the runbook. | No |
| SRF5-4 | Info | The Dockerfile expands `${GIT_COMMIT}` inside the `-ldflags` string; a value with spaces could add linker flags (e.g. another `-X`). | Only whoever controls the build arguments, i.e. already controls the build; `deploy.sh` forces 40-hex; runtime regex blocks response injection. | Optional hardening: validate `GIT_COMMIT` against `^[0-9a-f]{7,64}$|^unknown$` in the RUN step before `go build`. | No |

Not re-verified here (accepted from the record, out of this review's run): the full-package suites in section 6, golangci-lint, backoffice vitest.
