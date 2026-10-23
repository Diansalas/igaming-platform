# c2 - tenant-scoped, read-only operating bases API (record)

Branch `c2-tenant-basis-read` (from 4974b0d). No migration (head stays 0189). Providers: n/a. This record is a slice record, not a
legal position: an operating basis is a recorded business decision, not an authorisation.

## Labels

| Deliverable | Label |
|---|---|
| `GET /v1/admin/tenant/operating-bases` (list) and `GET /v1/admin/tenant/operating-bases/{basisID}` (detail), handler, service reads | IMPLEMENTED |
| New permission `operating_basis:tenant_read` (code-defined role map, no migration) | IMPLEMENTED |
| OpenAPI entries and schemas (`TenantOperatingBasis`, `TenantOperatingBasisPage`) | IMPLEMENTED |
| Golden route table + static gating pins + OpenAPI contract test | IMPLEMENTED |
| Back Office view | NOT IMPLEMENTED (deliberately; see below) |
| Security review (`security`) and code review (`code-reviewer`) | NOT DONE - required before this is called complete (auth/permission change) |

## Design decisions

1. **Route shape.** `/v1/admin/tenant/operating-bases[/{basisID}]`. There is no tenant path or query parameter. The tenant id is
   `tenant.FromContext` (token claim) only; a supplied `tenant_id`/`tenantID` query parameter is ignored (tested). Chain:
   `staffRoute(stepUpNone, RequireTenantScope(RequirePermission(PermOperatingBasisTenantRead)(...)))`. A platform_admin token has no
   tenant and gets 403 (also with a `tenant_id` parameter). Step-up none because it is a read (golden rule: GET = none).
2. **Permission.** NEW `PermOperatingBasisTenantRead = "operating_basis:tenant_read"`, granted to `tenant_admin` and `compliance`
   (the two roles that already read the tenant's own withdrawal-restriction policy / licensing changes). `jurisdiction_registry:read`
   was NOT reused: it is platform-wide and pinned platform_admin-only; widening it would change the meaning of every registry
   route. Roles are code-defined (`rolePermissions` in `internal/auth/permission.go`), so seeding needs no migration.
3. **Isolation, three independent layers.** (a) the transaction is `db.Pool.WithTenant(authenticated tenant)`; the FORCE-RLS policy
   `operating_bases_tenant_read` (0180) admits only that tenant's rows and none for a player-scoped session; (b) the service functions
   (`internal/operatingmarket/tenant_basis_reads.go`) call `assertTenantScope` and refuse a tenant id that differs from the
   transaction's (`ErrTransactionScope`), then force `SET TRANSACTION READ ONLY`; (c) every query also carries an explicit
   `tenant_id = $1` predicate. Brand filter: `SELECT EXISTS (... brands WHERE id AND tenant_id)`; unknown or foreign brand =
   `ErrNotFound` = the same 404. Unknown vs foreign basis id = the same 404 (`ErrNotFound`, body "not found", no id echoed).
4. **Pagination.** `parsePageParams` (default 50, max 200, silently clamped), the service rejects limit outside 1..200 and negative
   offset. `total` counts only the caller's rows.

## Projection, field by field (tenant view, an allow-list)

| Field | Tenant sees | Why |
|---|---|---|
| id | yes | row handle for detail |
| brand_id, country_code, operation_code, product_code | yes | the scope the tenant configured |
| basis_kind | yes | what the basis is |
| restricted_product_codes | yes | closed registry codes, needed to interpret coverage |
| has_restrictions_note | boolean only | the note is staff free text (<=1000 chars); the platform marks nothing tenant-visible, so the text is withheld |
| review_due_at, needs_rereview | yes | tenant must know a review is due |
| status (active/withdrawn), is_current, effective_from, effective_to, created_at | yes | state and history |
| policy_version | yes | rulebook version stamp, opaque |
| disclaimer | yes | same non-legal-position text as the platform route |
| tenant_id | no | implicit and known to the caller; never echoed |
| licence_id, jurisdiction_id | no | platform registry ids a tenant cannot dereference (no tenant registry read) |
| determination_reference, basis_evidence_reference, last_review_reference | no | opaque references into compliance evidence; not marked tenant-visible |
| determined_by_role | no | internal governance function of the platform decision-maker |
| restrictions_note (text) | no | free text |
| reason_code | no | staff-entered change reason |
| change_request_id, created_by, `decision` object (requester, executor, approver ids) | no | staff identity and internal workflow; the lic4/lic8 tenant readiness precedent drops internals (R-OPERATING-BASIS lists basis ids only, "never a reference, name or note") |

"Expired" rows: the table has statuses `active`/`withdrawn` only. A superseded/closed version is shown with `is_current=false` and an
`effective_to`; a withdrawn row with `status=withdrawn`. Review overdue is derivable from `review_due_at`/`needs_rereview` (no new
computed claim is made).

## Back Office

`backoffice/src/features/licensing` exists but is the PLATFORM licensing console (`LicensingLayout`, gated by the `licensingRead`
nav permission = platform_admin; `TenantOperatingBasesPage` is a platform view of a chosen tenant). There is no tenant-console
licensing section, so none was built (scope rule). Follow-up decision for the orchestrator: whether a tenant "Markets" page should
consume this API.

## Files

- `internal/operatingmarket/tenant_basis_reads.go` (new): `ListOwnOperatingBasesPage`, `GetOwnOperatingBasis`.
- `internal/httpserver/tenant_operating_basis_read_handlers.go` (new): handlers + `tenantOperatingBasisResponse` projection.
- `internal/httpserver/jurisdiction_admin_routes.go`: two route registrations.
- `internal/auth/permission.go`: constant + tenant_admin and compliance grants.
- `docs/api/openapi/platform-api.yaml`: two paths, two schemas; the platform list description's "A tenant has no read API of its own" sentence updated.
- Tests (new): `internal/auth/operating_basis_tenant_read_permission_test.go`, `internal/httpserver/tenant_operating_basis_projection_test.go`,
  `internal/httpserver/tenant_operating_basis_http_integration_test.go`. Edited pin: `internal/httpserver/staff_route_golden_test.go`.

## Intentional pin changes

1. `staffRouteGolden` (`staff_route_golden_test.go`): +`GET /v1/admin/tenant/operating-bases` = none, +`GET /v1/admin/tenant/operating-bases/{basisID}` = none.
2. Role-permission matrix: tenant_admin and compliance gain `operating_basis:tenant_read` (new constant; pinned by the new
   `TestOperatingBasisTenantReadPermission_RoleMatrix`, which also re-asserts `jurisdiction_registry:read` is platform_admin-only).
3. `staff_route_static_test.go`: golden route count pin 266 -> 268 (mutating stays 158). `staff_stepup_integration_test.go`: coverage `read` 108 -> 110 (critical, financial, none-mutating and classifier counts unchanged).
4. No change to the platform registry static pin (`licence_registry_read_routes_static_test.go`): its regex does not match the new
   `/v1/admin/tenant/...` paths, and the platform routes are unchanged. No step-up coverage count changed (both routes are "none"; the
   golden table is the coverage control and both-ways comparison passes). No existing assertion was weakened or removed.

## Tests added

Unit/static (no DB): role matrix; projection golden field set + forbidden-value scan (every withheld value and id is searched in the
JSON) + struct has no staff/tenant field; static gating of the two routes (RequireTenantScope, dedicated permission, stepUpNone, no
platform permission, handlers use only `WithTenant`, never read a client tenant id); OpenAPI documents both paths, permission,
401/403/404, no `tenant_id` parameter, and the schema properties equal the golden list.

Integration (real DB as `igaming_runtime`, real HTTP stack), `TestC2_*`:
- own rows only: tenant_admin and compliance see exactly A's four rows (active, withdrawn, closed, brand-scoped); B sees only B's;
  status/is_current/effective_to correct for withdrawn and closed versions; `current=true` drops only the closed one; wire field
  set of list items and of detail equals the golden list; no secret value/ids/`"decision"`/approver in either body.
- cross-tenant: B by A's id vs B by random id: same status+code+message, no id echoed; A by B's id 404; foreign brand vs unknown
  brand filter identical 404; own brand filter works; pagination walk (limit=1, offsets) as B yields only B's rows and `total` = B's count;
  limit clamped to 200; `tenant_id`/`tenantID` params ignored; country filter stays in-tenant; malformed inputs 400.
- authorization: no token / garbage token 401; support, finance, risk_manager 403 on list and detail; platform_admin 403 (with and
  without `tenant_id`); tenant roles 403 on the platform routes; POST/PUT/PATCH/DELETE not routed (405/404).
- DB layer: tenant B session sees only B's rows, 0 of A's by tenant predicate and by id; service functions given A's id inside B's
  session = `ErrTransactionScope`; B + A's brand / A's row = `ErrNotFound`; tenant session UPDATE changes nothing; session with no
  tenant sees 0 rows; player-scoped session on A sees 0; `relforcerowsecurity` and `relrowsecurity` true.

## Adversarial test list (covered unless marked)

1. Foreign id vs unknown id (status/body identical) - covered. 2. Foreign brand vs unknown brand - covered. 3. `tenant_id` query param
as another tenant, by tenant staff and by platform_admin - covered. 4. Path-tenant style variant (`tenantID`) - covered (ignored).
5. Pagination walk / huge limit / offset beyond total (offset 4,5 on B, empty page) - covered. 6. Total leaking cross-tenant count -
covered. 7. Manipulated context: service called with another tenant's id in a tenant session - covered; platform-scoped tx passed
to the tenant function - covered by `assertTenantScope` (no platform tx test through the function: NOT separately tested). 8. Player-scoped
session - covered at DB. 9. No-tenant session - covered. 10. Write verbs - covered. 11. Role downgrade: support/finance/risk_manager -
covered; promotions_manager/bonus_operations/player roles - covered only by the unit role matrix, not over HTTP. 12. Tenant role on
the platform route - covered. 13. Free-text/evidence leakage via sentinel strings in every withheld column - covered. 14. SQL
injection through `country_code`/`brand_id`/`current` - inputs are validated (ISO alpha-2, UUID parse, bool parse) and parameterised;
not separately fuzzed. 15. Timing/size side channel between foreign and unknown ids - NOT tested (both are a single indexed
lookup; latency equivalence not measured). 16. Suspended/disabled staff, revoked session - inherited from `staffRoute`
(`requireLiveStaffSession`); not re-tested here. 17. Rows written by the licensing change executor (change_request_id set) versus owner
fixtures: the read is identical; the HTTP test uses owner-path rows (NULL change_request_id), so the `decision` absence is proven
by field-set and not by a non-null change request fixture.

## Commands and results

All run in the worktree against private DB `c2_tenantbasis` (built with priv_db.sh; dropped afterwards). Observed results:

- `go build ./...` : ok. `go vet ./internal/httpserver/ ./internal/auth/` and `go vet -tags integration ./internal/httpserver/`: clean.
- `go test ./internal/auth/ -run "OperatingBasisTenant|Permission" -count=1` : ok. `go test ./internal/httpserver/ -run "TenantOperatingBasis|GoldenClassification|RegistryReadRoutes|OpenAPI" -count=1` : ok (DB-free).
- `priv_test.sh -tags integration -count=1 -p 1 ./internal/httpserver/ -run TestC2_ -v` : 4/4 PASS (after fixing a test artifact: my first forbidden-substring `decision` matched the disclaimer text "the runtime gate decides"; narrowed to the quoted JSON key).
- Full `priv_test.sh -tags integration -count=1 -p 1 -timeout 50m ./internal/httpserver/ ./internal/operatingmarket/` : operatingmarket ok (46s); httpserver FAILED only `TestStaffRoutes_EveryMutatingRouteClassified` and `TestStepUp_GoldenTable_EveryClassifiedRoute` (the two intentional count pins, 266->268 and read 108->110, then updated). A first run with the default 10m timeout panicked on timeout (package is ~23 min); not a test failure.
- After the pin edits: `priv_test.sh ... ./internal/httpserver/ -run "TestStaffRoutes_|TestStepUp_GoldenTable|TestC2_|TestRegistryRead|TestOpenAPI|TestTenantOperatingBasis"` : ok. The full httpserver package was NOT re-run end to end after the two pin edits (only the targeted set above).
- `./internal/auth/` with integration tag : ok (1.99s).
- YAML: `python3 -c "import yaml; yaml.safe_load(open('docs/api/openapi/platform-api.yaml'))"` : ok. `cd backoffice && npx vitest run src/api/openapiContract.test.ts src/api/operatingBases.test.ts src/api/licensing.test.ts` : 55 passed. `cd b2c && npx vitest run src/api/openapiContract.test.ts` : 21 passed. (node_modules were symlinked from the main checkout for the run and removed again.)
- `gofmt -l` flags `internal/auth/permission.go` and three other untouched files: pre-existing doc-comment formatting (gofmt -d shows only comment re-wrapping); no new gofmt delta from this slice's lines.

## Proposed text for governance docs (NOT applied)

**ADR addendum (to the lic9 / operating-basis decision):** "Tenant read of operating bases (c2). A tenant reads only its own
operating-basis versions through `GET /v1/admin/tenant/operating-bases[/{id}]`, permission `operating_basis:tenant_read`
(tenant_admin, compliance), tenant from the authenticated context only, FORCE-RLS `operating_bases_tenant_read` as the independent
control. The response is an allow-list that withholds staff identities, the decision record, change request id, reason code,
determination / evidence / review references, the responsible role, free-text notes (boolean only), licence and jurisdiction ids and
the tenant id, because the platform marks nothing in a basis row tenant-visible. Foreign and unknown ids and brands are the same
404. Widening the projection (for example exposing `determined_by_role` or the evidence reference) is a reviewed change with
`security` sign-off. Platform staff continue to use `/v1/admin/operating-bases`."

**Runbook (`docs/runbooks/operating-basis.md`) addition:** "Tenant visibility: tenant_admin and compliance of a tenant can list and
read that tenant's own basis versions at `/v1/admin/tenant/operating-bases`. What they see is redacted: no staff, no evidence
references, no note text (only that a note exists), no licence ids. If a tenant asks why a basis was set, platform staff read the
decision record on `/v1/admin/operating-bases/{id}`. A 404 for a basis id means unknown OR not yours; support cannot tell which and
must not try to."

**Registry row:** "c2 tenant operating-bases read API: IMPLEMENTED (read-only, no migration, tenant from token, FORCE RLS,
redacted allow-list projection, new permission operating_basis:tenant_read for tenant_admin and compliance). Back Office tenant view:
NOT IMPLEMENTED (no tenant console licensing section exists). Security review: pending."
