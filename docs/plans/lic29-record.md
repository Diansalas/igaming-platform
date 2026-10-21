# lic29 record: AML operability (trapped-funds monitor, screening policy route, closed refusal codes, RG wording)

Branch `lic29-aml-operability` from integ-c `ffb88d0` (migration head 0188). Software capability only; the only screening provider is the MOCK; no regulatory approval is claimed.

## 0. Which migration

- **Migration 0181 was edited IN PLACE** (unreleased) for everything AML that lives in it: `aml_screening_parked_overdue` (new signature and one-pass body), and a `HINT` closed code on the screening refusals. Up, down, the grant loop and `deploy/init-app-role.sql` are consistent (the down drops the function by its new signature). **Any database that applied 0181 before this change must be REBUILT**, never rolled back and never patched by hand.
- **Migration 0189 is new** (`0189_lic29_screening_policy_tighten_only_author`): the screening policy writer `aml_screening_policy_set` lives in the RELEASED 0154, which is not edited. 0189 adds `aml_screening_policy_author`; its down drops it.

## 1. Items

| Item | Change | Label |
|---|---|---|
| (1) F-2 / N-1: quadratic rescans, LIMIT 5000 with no ORDER BY | `aml_screening_parked_overdue(p_tenant, p_since, p_after, p_limit)` reads ONE keyset batch (persons in `person_id` order after the cursor, 1..5000, the sweeper uses 500) and computes the payout-decision run with window functions in a single pass (no per-person rescans of a materialised set). It returns `o_next`: NULL when the batch reached the end of the person list. The sweeper keeps a per-tenant cursor, so every person is observed once per cycle whatever the size of the tenant; a stats flag (`ScreeningParkedPartial`) and the scanned count are exported and logged. The decision look-back stays `LEAST(p_since, now - GREATEST(7 days, 2 x largest threshold))`. | IMPLEMENTED |
| (1) N-2 / F-3: officer queue vs dead worker | Persons with an unresolved `needs_officer` request are counted apart (`o_officer`, `o_officer_overdue`) from persons stuck for want of a screening result (`o_parked`, `o_overdue`). The sweeper alerts them apart: `aml.screening_gate_trapped` with discriminator `sweeper:tenant:<id>` and `sweeper_officer:tenant:<id>`; `persons` is the only attribute (counts only, platform-routed). | IMPLEMENTED |
| (1) read failure | N = 3 consecutive failed passes for a tenant raise `aml.screening_attention` (platform-routed, p2) with reason `monitor_failing`, discriminator `monitor:tenant:<id>`, no other attribute; a success resets the counter. No new alert kind was added (the existing kind and its closed keys are used; its description is widened). The counter is in the sweeper process. | IMPLEMENTED |
| (1) candidate narrowing | NOT done as a separate pre-filter: every person of the batch is evaluated by the gate itself (the authoritative test). Narrowing without the gate would have to re-implement it. The cost is bounded by the batch size instead. | PARTIALLY IMPLEMENTED (by design) |
| (2) screening policy routes | `GET` and `POST /v1/admin/tenants/{tenantID}/screening/policy`. Read: `aml:case:read`, step-up none, `configured=false` when none exists. Write: `aml:hold:manage`, step-up critical, platform_acting officer, quorum and a signed proof checked by the DATABASE, legal review reference required; `rescreen_interval_seconds` and `backfill_grace_seconds` are REQUIRED (a missing field is a 400; an explicit 0 grace is a stated value); no default is shipped. Golden table (+2 routes: 266 / 158 mutating), step-up class pins, mutation audit allow-list, OpenAPI paths and schemas, runbook updated. Back Office fields are a separate follow-up. | IMPLEMENTED |
| (2) SR7-3 pre-route condition | The standing pin (`TestSetScreeningPolicy_IsNotRoutedYet_SR7_3`) said a route may be wired only with a four-eyes path for LOOSENING. That path is not built, so the route calls a new `AuthorScreeningPolicy`, which goes through `aml_screening_policy_author` (0189): the same authorisation and row as `aml_screening_policy_set`, plus **tighten-only** (a version less strict than the latest of that tenant and jurisdiction is refused: a list removed, a longer interval, a longer grace, an earlier effective time; code `policy_loosening_refused`, 409). The pin is UNCHANGED and still passes (the HTTP layer names neither `SetScreeningPolicy` nor exposes it in `AMLOfficerAPI`; the type is reachable as `aml.ScreeningPolicyInput`, an alias). The first version of a jurisdiction is not "loosening" anything. | IMPLEMENTED (loosening over HTTP: NOT IMPLEMENTED, owner decision) |
| (3) closed refusal codes | The database raises each screening refusal with a `HINT` from a closed table (`aml.RefusalCodes`, 21 codes); `classify` keeps the sentinel (status unchanged) and carries the code; `amlErrorCode` appends it to `check_codes`. Go-side validation refusals use the same codes. A static test pins both directions (every HINT is in the table; every code is raised by a migration), a unit test pins classification (status class unchanged, database text never copied, unknown hint ignored), a real-database test pins 12 codes end to end (plus `policy_loosening_refused`, `tenant_not_in_jurisdiction` and `platform_officer_required` in the author tests). OpenAPI: schema `AMLScreeningRefusalCode` (enum with descriptions), the screening route descriptions and the `Error.check_codes` description. **No HTTP status of an existing answer changed.** | IMPLEMENTED |
| (4) RG wording | `internal/rg/options.go`, `internal/httpserver/rg_options_handlers.go` and the OpenAPI `unavailable_reason` / `protective_only` text now say what the code does: for `missing_floor` the server refuses increases, removals and confirms (`RefusedConfigMissing`); for `calendar_conflict` none is offered, play and the account usage read fail closed, and the write path does NOT refuse them (it composes the rulebook without reading the calendar settings). Whether it should is an owner decision (section 4). | IMPLEMENTED (documentation) |

## 2. Pin changes (declared; nothing weakened)

- Golden route tables: `staff_route_golden_test.go` and `aml_screening_routes_test.go` gain the two policy routes; the route-count pin in `staff_route_static_test.go` moves 264/157 to 266/158; the registration counts in `TestAMLScreeningRoutes_GoldenRegistrationAndPermissionsAreExact` move 3 to 4 (critical) and 2 to 3 (read). `staff_mutation_audit_static_test.go` gains the allow-list entry for the new write route.
- `staff_stepup_integration_test.go`: the coverage pin moves critical 81 to 82 and read 107 to 108 (the two policy routes).
- `static_test.go`: `TestStatic_AMClassOnlyInAMLMigrations` now also accepts migration 0189 (an AML migration; it reuses the AM codes of the 0154 policy function).
- `catalog_integration_test.go`: `aml_screening_policy_author` is a runtime-executable officer function (it authorises first, like the rest); `aml_screening_parked_overdue` keeps its proof-less entry (new signature).
- `TestSetScreeningPolicy_IsNotRoutedYet_SR7_3`, `TestStepUpOperations_RegistryPinned` and all existing assertions are untouched. The operation `aml_screening_policy:set` is deliberately NOT added to the signer's step-up registry (the route is critical; the registry pin is ADR 0119 section 9.1).

## 3. Tests and mutation checks

New: `internal/aml/screening_lic29_integration_test.go` (keyset cycle, officer queue apart, no policy and bad arguments, tighten-only author, authority, 12 refusal codes end to end), `internal/aml/refusal_test.go`, `internal/httpserver/aml_screening_policy_routes_test.go`, `internal/payments/sweeper_lic29_integration_test.go`.

Each mutation was applied to the live function or the source, killed by the named test, then restored:

| Mutation | Killed by |
|---|---|
| cursor ignored in the batch | `TestLic29_ParkedScan_KeysetCycleObservesEveryPersonOnce` |
| officer persons counted in the dead-worker overdue | `TestLic29_ParkedScan_OfficerQueueIsCountedApartFromTheDeadWorkerSignal` |
| author without the interval / grace / effective-time / list rule (four mutations) | `TestLic29_PolicyAuthor_TightensButNeverLoosens` |
| author without the quorum | `TestLic29_PolicyAuthor_FirstVersionAuthorityAndProof` |
| a `HINT` removed from a refusal | `TestLic29_RefusalCodes_ThroughTheRealService` |
| sweeper does not save the cursor | `TestLic29_SweeperWalksTheKeysetCycleAndAlertsApart` |
| sweeper does not reset the failure counter | `TestLic29_SweeperAlertsWhenItsOwnReadFailsConsecutively` |
| failure alert after 1 instead of 3 | same test (the threshold is written out literally) |
| officer alert under the screening discriminator | `TestLic29_SweeperWalksTheKeysetCycleAndAlertsApart` |

## 4. Proposed ADR / registry text (for the owner; nothing here is decided)

**ADR 0116 s6 (AML routes), proposed addition.** "The screening policy is authored over HTTP by `POST /v1/admin/tenants/{tenantID}/screening/policy` (step-up critical, platform_acting officer, quorum, signed proof, legal review reference). The route is tighten-only (`aml_screening_policy_author`): a version less strict than the latest of the same tenant and jurisdiction is refused. Loosening a screening policy (removing a list, lengthening the re-screen interval or the backfill grace, or an earlier effective time) is NOT available over HTTP; it requires a four-eyes propose / approve path (SR7-3) that is not built. DECISION NEEDED: build that path, or keep loosening a database-operator action."

**ADR 0119 s9.1 (step-up registry), proposed.** "`aml_screening_policy:set` stays out of the signer backstop registry while the only routed caller is tighten-only (protective). If loosening is ever routed, the operation moves into the registry as `critical` in the same change."

**Refusal-code registry, proposed.** "The screening routes answer a refusal with the generic code and fixed sentence of the AML error table and one stable string from `aml.RefusalCodes` in `check_codes`. A code is never renamed or reused; adding one needs the migration (or the Go validation) that raises it, the table entry, the OpenAPI enum and a test. The Back Office owns the wording."

**RG options, owner question (lic28 note).** "Under `calendar_conflict` (relied-on jurisdictions disagree on `calendar_tz` / `week_start`) the options offer no increase, removal or confirm and play fails closed, but the limit write path does not refuse them. DECISION NEEDED: should the write path refuse a loosening (increase, removal, confirm) of a limit kind under `calendar_conflict`, as it does under `missing_floor`? Until decided, the documentation states the current behaviour."

## 5. Residuals

1. The sweeper's cursor, failure counter and cycle state are per process and in memory; a restart begins a new cycle and re-counts (repeat work only). Two sweeper instances each walk their own cycle.
2. The monitor still depends on the sweeper running; a dead worker AND a dead sweeper remains invisible, and `ALERT-DELIVERY-1` (nobody is paged unless a route exists) stays open.
3. A batch of 500 persons costs one gate evaluation per person; a very large tenant needs many passes per cycle (exported as `ScreeningParkedPartial`). The batch size is an engineering value.
4. The sweeper alert threshold is exercised in `internal/payments` through the injected read; the SQL is tested against a real policy in `internal/aml`.
5. Loosening a screening policy over HTTP: NOT IMPLEMENTED (owner decision). The unrouted service setter remains.
6. The parked / overdue wording `o_parked` counts persons whose latest payout decision is a screening-only hold AND whom the gate still denies; a person who never tried to withdraw appears only in `o_overdue`/`o_officer_overdue` (gated since the oldest unresolved request or the first gating version), not in `o_parked`.
