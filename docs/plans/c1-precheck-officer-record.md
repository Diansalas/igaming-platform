# c1 record: closed precheck refusal codes, and the authorisation paths of platform_officer_required

Branch `c1-precheck-officer` from `4974b0d` (migration head 0189). **No migration added.** Software capability only; providers are MOCK; nothing here is a legal or regulatory claim.

## Task 1: closed refusal codes for licensing-change / tenant-policy refusals

| Item | Label |
|---|---|
| Closed table `licensingchange.ClosedRefusals` (31 codes), `refuse()` on every precheck branch, `ClosedRefusal(err)`, SQLSTATE-class-LC guard map, `ClassifyDBError` | IMPLEMENTED |
| Code in `check_codes` of the licensing-change routes (file, approve, cancel...) and of POST /v1/admin/jurisdictions (`duplicate_jurisdiction_code`); status and generic API code unchanged | IMPLEMENTED |
| OpenAPI schema `LicensingRefusalCode` (enum + one description per code), Error.check_codes and LicensingChangeConflict text | IMPLEMENTED |
| Back Office `licensingRefusalWording` (backoffice/src/features/licensing/licensingRefusal.ts), used by `describeLicensingError` (every filing dialog and the change detail page go through it) | IMPLEMENTED |
| Drift pin: Go table = OpenAPI enum = UI map (vitest reads the Go source; a Go test reads the OpenAPI) | IMPLEMENTED |
| Wire test of every PRECHECK code (16 of 31 provoked over HTTP, real DB, nothing written) plus LC024 on the wire | IMPLEMENTED |
| Execution-time codes (`policy_exceeds_permitted_scope`, `policy_close_needs_successor`) and the other 12 guard codes on the wire | PARTIALLY IMPLEMENTED: unit-tested through `classify` and the HTTP writer; not provoked on a real database (see Residuals) |

### Design

- Source of the set: every `DomainError` raised by a `spec.precheck` (kinds.go, operating_basis_kinds.go, licence_lifecycle_kinds.go), the domain sentinels an executor can return that a precheck cannot see (`operatingmarket.ErrCeilingExceeded`, `ErrPolicyCloseRequiresSuccessor`, `jurisdiction.ErrInactiveAsset`), the runtime SQLSTATEs of class LC that reach `classify` (LC001 LC002 LC010 LC012 LC020 LC021 LC023 LC024 LC030 LC040 LC041 LC042 LC043 LC044 LC099), and the duplicate jurisdiction code. LC013/LC031 (not pending) and LC022/LC032 (self approval) keep their own API codes and carry no refusal code; AP* keeps `LICENSING_CHANGE_PROOF_REFUSED`; LC095/LC098 are migration pre-flight and unreachable by a request.
- Input SHAPE errors (decode step: malformed payload, missing key, bad date, "reference required to enable/withdraw") stay the generic 400 `validation_error` with no code. They are refused before any database access and the Back Office form rules block them; they are not precheck refusals.
- Mechanism: `DomainError` gained `Code`; `refuse(code, err)` panics on a code outside the table. `ClosedRefusal(err)` order: explicit precheck code, SQLSTATE guard map, domain sentinel. `classify` is untouched (sentinels kept). The writer `writeLicensingChangeError` adds the code to `check_codes` unless the answer is an internal error. `jurisdiction.ErrDuplicateJurisdictionCode` and `ErrInactiveAsset` are empty-text markers next to `ErrInvalidInput` (message text unchanged).
- Status is unchanged for every case, including the inconsistent ones: an unknown tenant is 404 on bind/unbind but 400 on a policy or configuration filing; an unknown licence is 400 on bind/ceiling but 404 on the lifecycle kinds; a missing ceiling is 409. These were not "wrong" in the sense of the brief (each was already the case), so they are documented, not changed.
- Naming: the identifiers are `ClosedRefusals` / `ClosedRefusal(` and not `RefusalCodes` / `RefusalCode(` because the standing pin `TestLic30_RefusalCodesAreNotUsedOutsideTheAMLRoutes` forbids those substrings outside the AML layer. The pin is NOT changed.

### The closed set

| Code | Meaning (OpenAPI description) |
|---|---|
| `jurisdiction_unknown` | the jurisdiction does not exist |
| `tenant_unknown` | the tenant does not exist |
| `licence_unknown` | the licence does not exist |
| `licence_not_active` | a licence that is not active cannot be bound to a tenant |
| `licence_licensee_mismatch` | the licence's licensee does not match the tenant's licensing model |
| `licence_bound_to_other_tenant` | a licence held by a tenant is exclusively bound to one tenant, and it is bound to another |
| `brand_not_owned` | the brand is not a brand of the tenant (a foreign brand is indistinguishable from an unknown one) |
| `tenant_without_licence_or_basis` | the tenant has no licence bound and no operating basis for the country, so no country may be enabled for it |
| `ceiling_not_enabled_for_country` | the tenant's licence has no enabled ceiling for the country (a missing ceiling and a disabled one are the same answer) |
| `basis_clashes_with_licence_ceiling` | the tenant's own licence already has a ceiling row for the country, so a documented no-licence basis would be ambiguous |
| `currency_not_active_asset` | every allowed currency must be an active asset of the platform asset registry |
| `licence_status_unchanged` | the licence already has this status |
| `licence_validity_dates_unchanged` | the validity dates are unchanged |
| `licence_review_due_unchanged` | the review date is unchanged |
| `licence_evidence_reference_unchanged` | the evidence reference is unchanged |
| `duplicate_jurisdiction_code` | a jurisdiction with this code already exists |
| `policy_exceeds_permitted_scope` | the policy would exceed the licence ceiling, a higher scope's policy, or a broader in-force policy at the same scope |
| `policy_close_needs_successor` | closing an in-force disabled policy at an inheriting rung requires an open successor version |
| `session_not_permitted` | this session may not request or act on licensing changes |
| `request_invalid` | the database refused the request as invalid or naming an unknown record |
| `request_immutable` | a filed request cannot be edited |
| `approvals_insufficient` | the request does not hold the required number of fresh approvals by distinct eligible people |
| `single_actor_execution_only` | a single-actor change is executed only by its requester, in its own transaction |
| `only_requester_cancels` | only the requester may cancel a request |
| `change_now_widens` | the change now widens what is permitted; a new request is needed |
| `approver_not_eligible` | only an eligible platform administrator decides a request |
| `governed_write_needs_executed_request` | governed data changes only through an executed licensing change request |
| `licence_identity_immutable` | licence identity fields can never change |
| `operating_basis_write_refused` | the operating basis, licence or tenant records do not allow this write |
| `closed_basis_needs_successor` | a closed operating basis version needs a successor version that starts when it ended |
| `records_append_only` | licensing change records are append-only |

### Wire results (real database, runtime role, exact command below)

16 precheck codes were provoked through POST /v1/admin/licensing-changes (platform requester, tenant compliance, tenant admin) and POST /v1/admin/jurisdictions: `basis_clashes_with_licence_ceiling`, `brand_not_owned`, `ceiling_not_enabled_for_country`, `currency_not_active_asset`, `duplicate_jurisdiction_code`, `jurisdiction_unknown`, `licence_bound_to_other_tenant`, `licence_evidence_reference_unchanged`, `licence_licensee_mismatch`, `licence_not_active`, `licence_review_due_unchanged`, `licence_status_unchanged`, `licence_unknown`, `licence_validity_dates_unchanged`, `tenant_unknown`, `tenant_without_licence_or_basis`. Each assertion: exact status, unchanged generic code (400 validation_error, 404 not_found, 409 LICENSING_CHANGE_REFUSED), `check_codes` equal to exactly that one code, the fixed sentence contains no id, table name or LC code, and the row counts of licensing_change_requests, licensing_change_approvals, licences, licence_country_ceilings, operating_country_policies, tenant_jurisdiction_configs, operating_bases, jurisdictions, audit_log and bound tenants are identical before and after. A positive control (an accepted request) proves the counters move. `LC024` (a tenant narrowing that now widens) is provoked on the wire as 409 `LICENSING_CHANGE_GUARD_REFUSED` + `change_now_widens`, the request stays `requested` with no approval.

Tenant isolation of the refusals: a tenant requester filing against another tenant's id is 404 with no code and no foreign id (unchanged, before the precheck); a foreign brand and an unknown brand give byte-identical answers (`brand_not_owned`, request id removed); every refusal of a tenant session concerns that tenant's own rows only. A tenant requester can learn from `ceiling_not_enabled_for_country` / `tenant_without_licence_or_basis` a boolean about ITS OWN tenant's licence ceiling; this is the same fact the 409 already implied and is not another tenant's data.

## Task 2: platform_officer_required and the authorisation path

### Verdict on lic32 finding 5

Finding 5 said the code "was NOT observed on the wire" and implied it was unreachable. **It is reachable**, by exactly one actor class: a tenant `compliance` staff member holding an in-force TENANT-scope `aml_officer` grant. The grant is creatable by the real tenant-originated G-T flow over HTTP (tenant_admin POST /capability-grants/requests with capability `aml_officer`, a distinct platform_admin POST .../approve); the lic32 e2e only tried the unsupported platform-originated variant. The test below does exactly that and observes **403 + check_codes ["platform_officer_required"]** on both POST /screening/policy and POST /screening/trigger-policy, for a tenant-scope officer in a platform-licensed tenant and for every member of a quorate (2) officer set in an own-licence tenant.

### Layers (in the order a request meets them)

| Layer | Decides | Evidence |
|---|---|---|
| L0 token | 401 | no token on both routes: 401 |
| L1 `staffRoute` + `RequirePermission(aml_hold:manage)` | 403, no code | only `platform_admin` and `compliance` hold the permission (static test over every role incl. unknown/empty). `tenant_admin` is refused here even with a malformed body |
| L2 `amlHandler` / `canActOnTenant` | 404 for a foreign tenant | tenant staff of tenant Q posting to tenant P's path: 404, no code |
| L3 session open | 403 `CG020` (no code) | a platform admin WITHOUT the officer grant, and an officer of ANOTHER tenant, cannot open `WithPlatformActingInTenant` for P: SQLSTATE CG020 asserted directly |
| L4 `aml_authorize` | AM001 (no in-force grant of the actor's scope) then proof then AM002 + HINT | tenant compliance without a grant: SQLSTATE AM001 asserted by raw SQL as that principal; with a tenant-scope grant the grant check passes (no AM001) and the service answer is `ErrForbidden` + `platform_officer_required` |

### Actor matrix, both configuration routes (platform-licensed tenant, quorum of 2 platform officers)

| Actor | Status | check_codes | Layer |
|---|---|---|---|
| platform admin acting as AML officer (grant, quorum, proof) | 201 | - | intended; exactly one new version row |
| tenant admin | 403 | none | L1 (lacks aml_hold:manage) |
| tenant compliance, no grant | 403 | none | L4 AM001 (passes L1/L2: a malformed body is a 400) |
| tenant compliance WITH tenant-scope officer grant (real G-T flow) | 403 | `platform_officer_required` | L4 AM002 + HINT |
| platform admin without the officer grant | 403 | none | L3 CG020 |
| officer of another tenant (platform acting) | 403 | none | L3 CG020 |
| staff of another tenant | 404 | none | L2 |
| platform staff without the aml permission | cannot exist | - | the only platform role is `platform_admin` (staff_users CHECK, migration 0011); no other role holds the permission outside compliance |
| no token | 401 | none | L0 |

For every refusal the version tables are unchanged and the grants table is unchanged. The other AM002 sites (the uncoded "actor does not own this tenant's AML scope", hold routes): a tenant-scope officer on a platform-OWNED tenant gets 403 on place and 404 on lift of a platform-owned hold (invisible to it), no hold lifted, no hold added; in an own-licence tenant the same tenant officers place a hold (201), so the refusal is the configuration authority only.

No gap found: no actor that should be refused was accepted. Authorisation behaviour was not changed.

### Defence in depth, stated precisely

`platform_officer_required` is not "unreachable behind an earlier gate": L1 admits `compliance`, and L4 is the only layer that refuses a compliance officer with a tenant-scope grant. The UI wording therefore is reachable and matters. Conversely the Go service has NO scope check of its own before the database for these two functions (AuthorScreeningPolicy / SetScreeningTriggerPolicy), so the database is the single enforcement point for this rule; the lic29 service test and this HTTP test both pin it.

## Commands and results

```
PRIV_DB=c1_priv PRIV_SRC=<worktree> bash .../priv_db.sh                         -> rebuilt c1_priv
go build ./... ; go vet ./internal/... ; go vet -tags integration ./internal/httpserver ./internal/licensingchange      -> clean
gofmt -l internal                                                               -> empty
go test -count=1 ./internal/httpserver ./internal/licensingchange ./internal/jurisdiction ./internal/operatingmarket ./internal/apierror ./internal/aml  -> ok (all)
priv_test.sh -tags integration -count=1 -p 1 ./internal/licensingchange ./internal/jurisdiction ./internal/operatingmarket  -> ok (53s / 52s / 46s)
priv_test.sh -tags integration -count=1 -p 1 ./internal/httpserver -run 'Licens|Jurisdiction|Lic[0-9]|Launch|LH|Market|OpenAPI'  -> ok (42s)
priv_test.sh -tags integration -count=1 -p 1 ./internal/httpserver -run 'AML|Capab|C1|Licens|Jurisdiction'               -> ok (75s)
priv_test.sh -tags integration -count=1 -p 1 ./internal/httpserver -run 'TestLicensingRefusalCodes|TestC1_' -v           -> PASS (all subtests)
cd backoffice && npx vitest run                                                 -> 81 files, 1065 tests passed
cd backoffice && npx tsc --noEmit -p .                                          -> clean
cd b2c && npx vitest run src/api/openapiContract.test.ts                        -> 21 passed
python3 -c "yaml.safe_load(docs/api/openapi/platform-api.yaml)"                 -> ok
```

(The backoffice and b2c checks ran with `node_modules` symlinked read-only from the main checkout; the symlinks are untracked and git-ignored.)

Mutation check: replacing one `refuse("licence_not_active", ...)` by a plain `DomainError` was killed by `TestRefusalCodes_ClosedAndEveryCodeIsRaised` (the code is no longer raised) and by the wire test; restored.

## Files

New: internal/licensingchange/refusal.go, refusal_test.go; internal/httpserver/licensing_refusal_codes_test.go, licensing_refusal_codes_integration_test.go, aml_officer_authz_integration_test.go; backoffice/src/features/licensing/licensingRefusal.ts, licensingRefusal.test.ts. Edited: internal/licensingchange/{kinds,licence_lifecycle_kinds,operating_basis_kinds,licensingchange}.go; internal/jurisdiction/{registry_admin,tenant_jurisdiction_config_admin}.go; internal/httpserver/{licensing_change_routes,jurisdiction_admin_handlers}.go; backoffice/src/features/licensing/{errors.ts,marketConfig.test.tsx}; docs/api/openapi/platform-api.yaml.

## Intentional pin changes

None. (Identifiers were named to stay clear of the lic30 substring pin instead of editing it.)

## Residuals and findings

1. **Execution-time sentinel on a non-DomainError path.** `execute()` returns `operatingmarket.ClassifyCommitError(err)` unwrapped when the executor's database call fails with a trigger error. The HTTP mapper only maps a `DomainError` to 409 `LICENSING_CHANGE_REFUSED`, so an `ErrCeilingExceeded` / `ErrPolicyCloseRequiresSuccessor` that arrives this way would answer 500 `internal_error`, and no refusal code is attached (the writer skips internal errors). Not provoked here (the prechecks catch the ceiling case first; the successor case needs a deferred-constraint race), so it is a suspected pre-existing gap, not a verified one. Not changed: it is a status change.
2. `backoffice/src/features/licensing/errors.ts` `LC_CODE_TEXT` describes LC codes the server never sends as check codes (the server sends none) and several texts differ from the migrations (LC021, LC023). It is dead, partly inaccurate UI text; left alone.
3. The wire evidence for the 12 other guard codes and the two execution sentinels is classify-level and writer-level only.
4. Tenant-staff filing of configuration and brand-rung kinds was characterised only for the refusals above (finding 7 of lic32 stands).
5. `platform_officer_required` is enforced only in the database for these two functions (no Go pre-check); this is by design (single source) but means a Go change cannot weaken it, nor can it add a friendlier early answer.

## Proposed text for governance documents (not applied)

**Refusal-code registry (extends the lic29 proposal).** "The licensing change routes and POST /v1/admin/jurisdictions answer a refusal with the generic code and fixed sentence of the licensing error table and, when the refusal is one the platform names, one stable string from `licensingchange.ClosedRefusals` in `check_codes`. A code is never renamed or reused; adding one needs the Go branch (or SQLSTATE mapping) that raises it, the table entry, the OpenAPI enum `LicensingRefusalCode` and the Back Office wording; a test pins the three sets together. Statuses and generic codes are those that existed before the closed codes."

**lic32 finding 5, correction.** "`platform_officer_required` is observable on the wire: a tenant `compliance` officer holding a tenant-scope aml_officer grant (tenant-originated request, platform approval) receives 403 with that code on both screening configuration routes. Pinned by TestC1_PlatformOfficerRequired_*."
