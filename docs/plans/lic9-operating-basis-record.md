# lic9: explicit operating bases (platform licence / partner licence / documented no-licence basis)

Branch `lic9-operating-basis` (from `claude/licensing-jurisdiction-completion` e2ade1e, migration head 0179). **Migration 0180** (up + down).
Software capability only: nothing here is a legal or regulatory position, no value is shipped (zero `operating_bases` rows, no country,
no jurisdiction declared licence-exempt in code or data), and no universal licence requirement is added.
Operator procedure: `docs/runbooks/operating-basis.md`.

## 1. Status labels

| Deliverable | Label |
|---|---|
| `operating_bases` table: FORCE RLS, append-only, versioned, platform writes, tenant read-own (migration 0180, up + byte-for-byte down) | IMPLEMENTED |
| Governance: three kinds `operating_basis_set` / `_withdraw` / `_review_record` (classifier, row matcher, LC040 guard, actor proof through the existing `licensing_change:*` family) | IMPLEMENTED |
| Resolver STEP 0 (basis selection, three bases, conflict / review / withdrawn / not-approved denials), `not_permitted_by_basis` outcome, Explain fields, `PolicyVersion` v4 | IMPLEMENTED |
| Read APIs `GET /v1/admin/operating-bases`, `GET /v1/admin/operating-bases/{id}`; OpenAPI; golden route table | IMPLEMENTED |
| Readiness item `R-OPERATING-BASIS` (snapshot version 7), Back Office labels / notes / ids list, runbooks | IMPLEMENTED |
| Consumer audit (marketgate pre-filter fixed; every other consumer branches on `Permitted()`) | IMPLEMENTED |
| Integration tests (written, vetted with `-tags integration`, RUN by the author on a private database; results in the report) | IMPLEMENTED |
| Tenant-scoped read API of its own bases | NOT IMPLEMENTED (residual R2) |
| Back Office screen to file / browse bases | NOT IMPLEMENTED (labels only; filing goes through the generic licensing-change route) |
| KYC / RG / reporting rulebook selection by the basis' jurisdiction | NOT IMPLEMENTED (residual R1, decision needed) |

## 2. Design as built (and where it deviates from the brief)

`operating_bases` columns: `id, tenant_id, brand_id (NULL = tenant-wide; composite FK to brands), country_code, operation_code (NOT NULL, FK
platform_operations), product_code (NULL = all), basis_kind, licence_id, jurisdiction_id, determination_reference, determined_by_role,
evidence_reference, restrictions_note, restricted_product_codes, review_due_at (DATE), needs_rereview, last_review_reference, status
(active | withdrawn), reason_code, policy_version, effective_from / effective_to, change_request_id, created_by_actor_id, created_at`.

- Shape CHECK: `no_licence_documented` = no licence, jurisdiction set, review date set; the two licence kinds = licence set, no jurisdiction
  (the jurisdiction is the licence's). References are opaque (CHECK: trimmed, 1..200 bytes, no control characters, no `@`); the role is a
  function code `^[a-z][a-z0-9_.:-]{1,99}$` (a person's name does not match). The Go decode also applies `launchgov.LintFreeText`
  (e-mail, phone, IBAN, wallet, card shapes, long digit runs).
- One open version per scope (two partial unique indexes: tenant-wide / brand). Versions follow the ceiling / policy convention:
  `effective_from` DB-stamped for any non-owner, close = `UPDATE effective_to` (the only change ever allowed, once), TRUNCATE denied, no
  DELETE policy or grant (the `tenants` ON DELETE CASCADE is the only remover, the 0076 precedent).
- **Decision-makers are not copied into the row.** `change_request_id` and `created_by_actor_id` are stamped by the guard trigger from the
  EXECUTED request of the same transaction (the writer cannot supply them); requester, executor and approvers are read from
  `licensing_change_requests` / `licensing_change_approvals` by `GET /v1/admin/operating-bases/{id}`.
- **Deviation, deliberate (1): `needs_rereview` + `last_review_reference` columns.** "Marking needs_rereview" had to be stored somewhere; a
  boolean is clearer than a back-dated review date and keeps the review record kind a plain version.
- **Deviation, deliberate (2): the owner path.** As for every other governed table the table owner keeps a provisioning path
  (fixtures / seeding), but the guard forces `change_request_id = NULL` on it and the resolver treats a row without a change request as
  `not_permitted_by_basis / basis_not_approved`. An owner can therefore never *create a permission*, only a record.
- **Deviation, deliberate (3): RLS of two existing tables.** The resolver runs under the tenant's session, so `licences_read` and
  `licence_country_ceilings_read` were extended with "OR a licence named by the reading tenant's own `operating_bases` row" (no other
  predicate changed; down restores 0076 / 0077 text). Without this, a basis on a licence not bound through `tenants.licence_id` could never
  be read and would always resolve `licensing_unknown`.
- **Deviation, deliberate (4): the 0076 policy-write trigger.** `operating_country_policies_enforce_ceiling` step (1) required the tenant's
  single licence to enable a country; for a country with an approved, in-force, active basis for the tenant it is skipped (the resolver
  stays the decider at read time; steps 2-4 are byte-for-byte unchanged). A pinned `search_path` was added. Without this change, no policy
  could ever be enabled for a no-licence tenant or for a per-brand partner licence, and the whole feature would be unusable.
- `operating_basis_set` is **always widening** (even if it narrows a prior version): the safe reading, as in the brief.

## 3. Governance

| Kind | Class | Approvals | Row written |
|---|---|---|---|
| `operating_basis_set` | widening (always) | 2 distinct platform_admin Persons, requester excluded, approvals <= 24 h old | close the open version, insert an `active` version |
| `operating_basis_withdraw` | narrowing (always) | single actor, executed in the requester's transaction | close, insert a `withdrawn` copy |
| `operating_basis_review_record` | widening iff `review_due_at` moves later or `needs_rereview` goes true -> false; else narrowing | as above | close, insert an `active` copy with the new review fields |

All three are `platform_only`; platform permission `operating_market_ceiling:manage` (reused, no new permission); actor proof is the
existing `licensing_change:request/approve/execute` family (**no new proof family**: the executed request IS the proof-bound authorisation,
exactly as for the 0178 lifecycle kinds). Defence in depth in `operating_bases_governed_guard`: an INSERT needs a request whose
payload matches the row (`licensing_change_row_matches`), a withdrawal / review row must equal its predecessor except the columns of the
kind, and a row that widens by content needs a request classified widening that holds its approvals.

SQLSTATEs: `LC040` governed write without an executed matching request; `LC041` append-only violation; `LC043` content invalid at write
(licensee / kind mismatch, licence bound to another tenant, predecessor drift, unknown restricted product); `LC010` payload invalid at
filing; `LC095` down refused while rows or requests exist. No SECURITY DEFINER function is added (every function pins `search_path`).
Hardening mirrored from 0178 / 0179: `zz_licensing_change_governed` last-firing trigger name, conditional ACL (REVOKE ALL then
`SELECT, INSERT, UPDATE (effective_to)` only if the role exists; also in `deploy/init-app-role.sql`), NO FORCE / FORCE around the
registry insert, byte-for-byte down (snapshot test), refusal LC095.

## 4. Resolution

STEP 0 selects the in-force `operating_bases` row for `(tenant, brand?, country, operation, product?)`, most specific first (brand-specific
before tenant-wide, then product-specific before all products). Then:

- no row for the scope at all -> the pre-lic9 STEP 1 (tenant licence chain + ceilings), unchanged;
- rows exist but none in force at AsOf -> `not_permitted_by_basis` (`basis_not_yet_effective` / `basis_not_in_force`), never the legacy path;
- same-specificity duplicates -> `configuration_conflict`;
- most specific row: withdrawn -> `basis_withdrawn` (**it masks the broader tenant-wide row; no fall back**); no change request ->
  `basis_not_approved`; product in `restricted_product_codes` -> `basis_product_restricted`; `needs_rereview` -> `basis_needs_rereview`;
  AsOf date after `review_due_at` -> `basis_review_overdue` (**the due date itself still stands**, the day after it does not);
- `no_licence_documented` (shape-valid): conflict with a ceiling row of the tenant's own licence -> `configuration_conflict` (reason
  `basis_licence_ceiling_conflict`); otherwise the basis is accepted and STEPS 2-4 (the policy rungs) still must permit;
- licence kinds: licensee re-checked (`invalid_configuration` on mismatch, `licensing_unknown` if unreadable), a partner licence also bound
  to another tenant -> `invalid_configuration`, then `applyLicenceCeiling` = the old STEP 1 body, verbatim, for the row's licence.

New outcome `not_permitted_by_basis` (existing codes unchanged); `Result` keeps its closed method set (oracle rule), `Explanation` gains
`BasisKind, BasisID, BasisReason, BasisReviewDueAt` and a `basis` chain step. `PolicyVersion` `stage-4i-e.v3` -> `v4`.

Consumer audit: marketgate (`Check`, `AnyCountryPermits`, `BrandOpen`, `CountryServedForAny`), launchgov `R-MARKET-CONFIG`, registration,
deposit, casino, sportsbook, `stagingseed` all branch on `Result.Permitted()` or pass the outcome as an opaque string into audit
metadata, so the new value fails closed everywhere (unit test: only `permitted` permits, an unknown value does not). No OpenAPI enum or
Back Office label enumerates outcomes. **One real consumer bug found and fixed:** `marketgate.prefilterByCeiling` dropped every country
without an enabled ceiling on the tenant's own licence before the resolver ran, so a basis-governed country could never make `BrandOpen`
true; it now keeps countries with an active, approved, in-force basis (mutation-tested: the new test fails without the fix).

## 5. Existing assertions changed (intentional; nothing weakened)

| File | Change | Reason |
|---|---|---|
| `internal/operatingmarket/no_db_test.go` | `PolicyVersion` pin v3 -> v4 (test renamed `..V4`) | resolve() gained STEP 0 |
| `internal/licensingchange/kinds_test.go` | registry pin ten -> thirteen kinds, seeds now read from 0177 + 0178 + 0180 | three new kinds |
| `internal/httpserver/staff_route_static_test.go` | golden pin 239 -> 241 routes (143 mutating unchanged) | two new reads |
| `internal/launchgov/readiness_aml_test.go`, `readiness_legacy_integration_test.go` | `SnapshotVersion` 6 -> 7 | new check |
| `licence_registry_read_routes_static_test.go`, `openapi_licence_reads_contract_test.go`, `staff_route_golden_test.go` | entries ADDED for the new reads / kinds | strengthened, none removed |
| `backoffice .../LaunchBits.tsx` | the "Legacy risk rule ids" list now filters `refs` keys by prefix | refs now also carry basis ids; unchanged output for the old item |

## 6. Tests (all written first or alongside; integration tests RUN by the author)

`internal/licensingchange/operating_basis_integration_test.go`: legacy behaviour unchanged (NULL licence = `licensing_unknown`, enabling still
refused without licence and basis); documented no-licence matrix (pending, one approval, approved, no policy -> `not_configured`, permitted,
other country, not yet effective, review due date vs day after, needs_rereview, review extension widening, shortening narrowing, withdrawn);
conflict with a ceiling (early refusal and resolver conflict); partner licence per brand differing from the tenant default; brand row masks
tenant-wide row; platform-licence basis with a NULL tenant licence and licence suspension; maker-checker per kind (requester and the
requester's Person cannot approve, same approver / same Person twice, widening needs two, narrowing single, tenant staff forbidden);
refusals (licensee / kind mismatch, cross-tenant licence, free partner licence goes to one tenant only, foreign brand at execution, eleven
payload / lint refusals with a control payload); direct INSERT / close by the runtime role `LC040`, UPDATE / DELETE / TRUNCATE `42501`,
tenant-scoped INSERT refused, owner row not an approved basis, owner UPDATE `LC041`, owner TRUNCATE `LC041`, owner DELETE removes nothing;
tenant isolation (rows, licences, mis-scoped resolver `ErrTransactionScope`); read failure fails closed; audit rows with reason codes,
decision record (requester + two approvers, executor = created_by); restricted products; marketgate `BrandOpen`.
`migration_0180_integration_test.go`: up / down / up byte-for-byte snapshot, ACL, zero rows / three kinds, down refused (LC095) with a
request and with a row. `internal/httpserver/operating_basis_http_integration_test.go`: file -> two approvals -> read list / get with decision;
400 / 404 refusals; non-platform principals 403 / 401. `internal/launchgov/readiness_basis*_test.go`: pure arms and the integration scenario.
`internal/licensingchange/operating_basis_kinds_test.go`, `internal/operatingmarket/basis_unit_test.go`: DB-free decode / closed vocabulary /
fail-closed / no-clock invariants.

## 7. Residuals and risks

- **R1 (decision needed, material).** KYC thresholds, RG floors, reporting formats and payout freshness are still selected from the tenant's
  LICENCE jurisdiction (`tenants.licence_id`). A tenant that relies on a no-licence basis has none: `kyc` resolves `not_required` (dormant,
  default-allow) and `R-KYC-POLICY` reports `no_licence_jurisdiction`. The basis records the relied-on `jurisdiction_id` but nothing consumes
  it yet. Selecting rulebooks by the basis' jurisdiction is a cross-domain change (KYC / RG / reporting) that needs an ADR; until then a
  no-licence basis opens the market gate only, and the readiness item `R-KYC-POLICY` is the visible warning. Interim safe behaviour: nothing
  is auto-relaxed; the approver must acknowledge `R-KYC-POLICY`.
- R2. No tenant-facing read API of its own bases (the DB read policy exists because the resolver needs it). Platform-only reads are the
  safe minimum; a tenant read needs a permission and a redaction decision (determination references are internal).
- R3. Reverse exclusivity: `tenant_licence_bind` of a `licensee = tenant` licence to tenant Y is not refused when tenant X holds a partner
  basis on it. The resolver fails closed for X (`invalid_configuration`, partner licence bound to another tenant) but the bind itself is not
  blocked. A one-line addition to `precheckBind` / a trigger on `tenants` closes it; not done to keep this slice small.
- R4. The "review row widens by content" branch of `operating_bases_governed_guard` is defence in depth for a request/row mismatch the
  Go executor cannot produce; it is exercised only by reading, not by a dedicated test.
- R5. `effective_from` is DB-stamped (convention): no scheduled future-dated activation; "not yet effective" arises for AsOf before the row
  existed and for owner-dated rows.
- R6. Readiness coverage counts casino wagering only (product `casino`), mirroring `R-MARKET-CONFIG`; sportsbook-specific bases are not
  summarised.
- R7. Back Office: labels, notes and ids list only; no screen. The generic licensing-change filing dialog is the UI path.
- R8. `review_due_at` boundary (due date inclusive) and "withdrawn masks the tenant-wide row" are my interim decisions; both are one-line changes.
- R9. The policy-write trigger relaxation is per (tenant, country), not per operation / brand; the resolver (read time) is the decider.

## 8. Proposed ADR text (for the orchestrator to place; ADRs untouched)

### ADR 0045 amendment (new AMENDMENT-4: explicit operating basis, section 3.5 STEP 0)

> **AMENDMENT-4 (lic9, migration 0180). The ceiling may rest on an explicit operating basis.** `licence_country_ceilings` remains the only
> expression of what a *licence* permits. A new governed table `operating_bases` records, per (tenant, brand?, country, operation,
> product?), which basis the permission relies on: `platform_licence` (a licence with licensee `platform`), `partner_licence` (a licence
> with licensee `tenant`, exclusive to one tenant) or `no_licence_documented` (an explicit, reviewed determination naming the jurisdiction
> relied on, with a mandatory review date). The resolver gains STEP 0: a scope with no `operating_bases` row resolves exactly as before; a
> scope with one is decided by that row (the row's licence replaces `tenants.licence_id` for the scope and its validity and ceiling logic
> applies unchanged; a documented no-licence basis replaces the licence step and is a `configuration_conflict` if the tenant's own licence
> also has a ceiling row for the country). Any row that is withdrawn, not approved (no executed change request), not in force, past its
> review date, flagged for re-review or product-restricted yields the new outcome `not_permitted_by_basis`; the tenant, brand and operation
> rungs (STEPS 2-4) must still permit. Country configuration is never proof of legality; no basis ships; no jurisdiction is declared
> licence-exempt in code or data. `PolicyVersion` stage-4i-e.v4. `operating_country_policies_enforce_ceiling` step (1) is skipped for a
> country governed by an approved in-force basis (the resolver stays authoritative).

### ADR 0123 addendum (licensing change governance: operating-basis kinds)

> **Addendum (lic9).** The closed kind registry gains `operating_basis_set` (always widening, two approvals), `operating_basis_withdraw`
> (always narrowing, single actor) and `operating_basis_review_record` (widening iff the review date moves later or the re-review flag is
> cleared). All are platform-only, use permission `operating_market_ceiling:manage` and the existing `licensing_change:*` actor-proof
> family (no new family: the executed request is the proof-bound authorisation). `operating_bases` is governed like the other registry
> tables: INSERT and the `effective_to` close only inside an executed request of the matching kind and payload in the same transaction
> (LC040), `change_request_id` / `created_by_actor_id` stamped by the guard, never supplied; no UPDATE, DELETE or TRUNCATE for any other
> role. The decision-makers of a basis are the approvers of its change request; no personal data is copied into the row. The owner
> provisioning path is retained for fixtures but produces rows that no resolver accepts as approved.

## 9. For the orchestrator

1. Merge the branch; run `go test ./internal/db` (migration pins), the integration packages listed in the report, and the Back Office tests.
2. Place section 8 in ADR 0045 and ADR 0123; add registry rows (lic9 IMPLEMENTED; residual R1 as a decision request: "rulebook selection by
   the basis' jurisdiction"; R3 as a small follow-up).
3. Owner decisions pending: R1 (above), who approves a no-licence basis (interim: two platform_admin Persons, HDR-M-1), the review-date
   boundary (R8).
