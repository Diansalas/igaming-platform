# Slice 2 record: licensing / jurisdiction maker-checker (migration 0177)

- **Worktree / branch:** `.claude/worktrees/lic2`, branch `lic2-maker-checker`, based on `claude/licensing-jurisdiction-completion` at `7ebba1a`.
- **Owner of this record:** `security` specialist (implementation and self-review; an independent `code-reviewer` / `architect`
  pass is still required before merge, section 10).
- **Closes (software side):** audit gaps G-1, G-2, G-9 (`docs/plans/licensing-audit-core.md`); triage L-24, L-25 and the
  engineering half of L-10 (`docs/plans/licensing-decision-triage.md`); MKT-DUAL-1 trigger (b) (the market gate already reads
  these rows) now has a software control. HDR-M-1 stays OPEN (interim behaviour in section 8).
- **Not a legal opinion.** Nothing here decides whether any licence or market is legally permitted. No licence, country,
  ruleset or legal value was created, seeded or invented.

## 1. Deliverables and labels

| Deliverable | Label | Evidence |
|---|---|---|
| Generic append-only change-request + approvals tables, closed kind registry, states requested / approved / executed / rejected / cancelled / expired, DB-computed payload hash and widening classification, reason codes, idempotency (one open identical request; one approval per Person) | IMPLEMENTED | `migrations/0177_licensing_change_approvals.up.sql` sections 2-5, 8; `internal/licensingchange/*` |
| Signed actor proof (ADR 0110) with a new proof-bound operation family `licensing_change:{request,approve,reject,cancel,execute}`; nonce / freshness / key rules are the existing verifier's | IMPLEMENTED | 0177 section 1 (verifier = 0175 body + one block); `internal/actorproof/actorproof.go`, `stepup.go`; `licensing_change_scope_test.go`; `TestSQL_MissingOrForgedProof_Refused` (AP001 / AP002 / AP004) |
| Execution re-checks everything in ONE transaction (approvals: count, <= 24 h, distinct Persons, not the requester's Person or account, ACTIVE eligible role; expiry; single-actor rule; re-classification `LC024`) | IMPLEMENTED | 0177 `licensing_change_requests_guard`; `TestTenantJurisdictionConfig_Versions` (LC024), `TestSQL_SingleActorExecution_*` (LC021), `TestApprovalOlderThanWindow_Refused` |
| DB trigger making the governed tables writable ONLY by the executor path (`LC040`): `licences` INSERT, `tenants.licence_id`, `licence_country_ceilings`, `operating_country_policies`, `tenant_jurisdiction_configs` | IMPLEMENTED | 0177 section 6; `TestSQL_GovernedTables_DirectRuntimeWriteRefused` |
| Operating-market ceiling and policy versions governed; widening two-person, narrowing single actor with reason (fail-closed asymmetry) | IMPLEMENTED | `kinds.go`; `TestNarrowing_SingleActor_WideningTwoPerson`; `TestPolicyEnableBeyondCeiling_RefusedWhenFiled` |
| `tenant_jurisdiction_configs` governed writer (append-only with `effective_to` close, one open version, audit before / after in the same transaction projected to the tenant), RLS narrowed (tenant SELECT only; platform write via the executor) | IMPLEMENTED | `internal/jurisdiction/tenant_jurisdiction_config_admin.go`; 0177 section 7; `TestTenantJurisdictionConfig_Versions`; adapted `TestRuntimeRole_OrdinaryDMLIsGenuinelyRLSScoped` |
| Licence CREATE and licence-to-tenant BIND / REBIND through the primitive, request bodies unchanged; UNBIND single actor | IMPLEMENTED | `jurisdiction_admin_handlers.go` (`POST /v1/admin/licences` -> 202), `tenant_licence_admin_handlers.go` (`PUT .../licence`: UUID -> 202, null -> 200 executed) |
| Extension hook for slice 1's widening kinds (reinstate / extend expiry) | IMPLEMENTED (hook only; no slice-1 kind added) | `register()` in `kinds.go`; registry row + SQL branches; runbook section 6; section 6 below |
| API (6 routes) + OpenAPI + golden step-up table + signer backstop + permissions (`licensing_change:{request,approve,read}`) | IMPLEMENTED | `licensing_change_routes.go`; `docs/api/openapi/platform-api.yaml`; `staff_route_golden_test.go`; `openapi_licensing_change_contract_test.go`; `licensing_change_permission_test.go` |
| `cmd/seed-market` production refusal unchanged; dev / staging seeding keeps working (owner path) | IMPLEMENTED | `cmd/seed-market` doc comment; `go test -tags integration ./cmd/seed-market ./internal/stagingseed` ok |
| Migration up / down (byte-for-byte snapshot) and down refusal while requests exist (`LC095`) | IMPLEMENTED | `TestMigration0177_UpDownUp_SnapshotAndDownRefusal` |
| Runbook | IMPLEMENTED | `docs/runbooks/licensing-change-approvals.md`; pointers in `market-blocking-configuration.md` s2, `launch-and-suspension.md` s3 |
| Back Office pages (G-10) | NOT IMPLEMENTED (out of scope; build on this API) | - |
| Licence status / dates / evidence (G-4) | NOT IMPLEMENTED here (slice 1, migration 0176) | - |

## 2. Design (what the database enforces)

- **Request INSERT** (`licensing_change_requests_guard`): the session actor must be exactly a tenant principal (active staff of the
  tenant, role `tenant_admin` or `compliance`, with a Person) or a platform principal (active `platform_admin` with a Person); acting,
  player and service sessions are refused (LC001). The kind must be in the registry; tenant sessions may not file `platform_only` kinds
  or a foreign subject (LC002). The payload is validated strictly per kind (exact key set) and CLASSIFIED by
  `licensing_change_classify`; `required_approvals = widening ? kind.widening_approvals (2) : (tenant ? 1 : 0)`. The payload hash,
  actor, Person, scope, times and txid are derived here; the requester's proof (`licensing_change:request`, target `new`) is consumed.
- **Request UPDATE** is the closed state machine: content immutable (LC012); `requested -> expired` (no proof, only after
  `expires_at`); `-> cancelled` (requester only, proof); `-> rejected` (eligible platform approver, not the requester's Person, proof,
  reason code); `-> approved` (fresh approvals >= required); `-> executed` (platform_admin; a 0-approval change only by its
  requester in its own transaction (LC021); otherwise from `approved` with the approvals re-counted; re-classified (LC024); proof
  `licensing_change:execute`). `executed_txid = txid_current()`.
- **Approval INSERT**: platform session, role in `kind.approver_roles`, open unexpired request (locked), not the requester's account
  or Person (LC032), hash copied from the request, proof `licensing_change:approve`; one per Person and per account (unique).
- **Fresh approvals** (`licensing_change_fresh_approvals`): distinct Persons, payload hash equal, `decided_at > now() - 24 h`, not the
  requester's Person or account, approver still ACTIVE platform staff with an eligible role and the same Person.
- **Governed-write guard** (`licensing_change_governed_write_guard`, `zz_` BEFORE ROW): any role but the table owner needs an
  executed request of the matching kind and payload with `executed_txid = txid_current()` (`licensing_change_row_matches`); unknown
  table or kind = false (fail closed). The table owner path is kept on purpose (ADR 0112 LF2 precedent; an owner can disable
  triggers anyway): synthetic seeding and fixtures.
- **`tenant_jurisdiction_configs`**: 0005's `FOR ALL` tenant policy replaced by `tjc_tenant_read` (the same predicate, SELECT only),
  `tjc_platform_read` / `_insert` / `_close` (platform shape) and `tjc_owner_tenant_insert` / `_close` (strict owner equality);
  append-only trigger (only `effective_to`, once; `now()` for a non-owner); TRUNCATE denied; unique open version.
- **RLS of the new tables**: platform shape (launch `p_pred`) for SELECT / INSERT / UPDATE; tenant principal shape bound to
  `subject_tenant_id` for SELECT / INSERT / UPDATE (cancel) of requests and SELECT of their approvals (ADR 0104 projection); ADR
  0099 acting fences (restrictive) on all three; kinds readable by platform or tenant principals; grants: kinds SELECT; requests
  SELECT, INSERT, UPDATE of the 8 state columns; approvals SELECT, INSERT (mirrored in `deploy/init-app-role.sql`).

## 3. Go and HTTP

- `internal/licensingchange`: `Service.Request / Approve / Reject / Cancel / Get / List`; READ COMMITTED asserted, `lock_timeout`;
  the final approval executes in the same transaction (order: `approved`, proof, `executed`, event audit, then the domain writer
  LAST, because the policy kind binds `app.tenant_id` for `operating_country_policies` and keeps it bound through COMMIT for the
  deferred successor check). Approvals unreadable -> error, nothing recorded (`TestApprovalsUnreadable_FailsClosed`).
- Domain writers unchanged and still authoritative for their own rules and audit: `jurisdiction.CreateLicence`,
  `jurisdiction.AssignTenantLicence`, `operatingmarket.Create*Version`; new `jurisdiction.SetTenantJurisdictionConfigVersion` /
  `CloseTenantJurisdictionConfig` (currencies must be ACTIVE assets; geo entries ISO alpha-2; ruleset ids shape only).
- Routes: `POST/GET /v1/admin/licensing-changes`, `GET .../{changeID}`, `POST .../{changeID}/approve|reject|cancel`. Step-up:
  request and approve `critical`, reject / cancel / reads `none` (golden table 229 routes / 139 mutating). Staff ids rendered
  through `staffViewer` (a tenant never sees a platform actor's id). Codes: closed `LICENSING_CHANGE_*` set (11).
- `Deps.LicensingChanges` is wired by `cmd/platform-api`; when nil and `Deps.DB` is set, `NewWithAdmission` builds one over DB
  (signing with `actorproof.Default()`; no issuer = 500, nothing written). Deliberate deviation from the launch "nil = 503"
  convention: the two pre-existing licence routes must keep working in every server built over a DB.

## 4. Intentional behaviour changes (tests adapted, no assertion weakened)

| Test | Change | Why |
|---|---|---|
| `internal/db/runtime_role_separation_test.go` `TestRuntimeRole_OrdinaryDMLIsGenuinelyRLSScoped` | the runtime role's tenant-scope INSERT into `tenant_jurisdiction_configs` is now asserted REFUSED (LC040 / 42501); the two rows are seeded by the owner fixture path; every read-isolation assertion unchanged | G-9: the write was the defect |
| `internal/launchgov/market_config_readiness_integration_test.go` | policy fixture via the owner pool | runtime policy writes are governed (LC040); the test is about the reader |
| `internal/launchgov/rules_integration_test.go` `setRuleset`, `rg_config_readiness_integration_test.go` | in-place `UPDATE` of a configuration replaced by close + append (`newConfigVersion`); the rg test's former fallback would now SKIP (not evidence), so it was replaced by the real append | the table is append-only (LC041) |
| `internal/httpserver/jurisdiction_admin_integration_test.go`, `tenant_licence_admin_integration_test.go` | `POST /v1/admin/licences` and a bind now answer 202 and are executed by two other Persons over HTTP (`executeFiledChange`); final state asserted (licence listed / tenant bound); every refusal case (400 / 404 / 403 / 401) keeps its status | G-2 |

## 5. Verification run (private database `lic2...`, harness scripts)

- Unit: `go test ./internal/actorproof ./internal/auth ./internal/apierror ./internal/licensingchange ./internal/httpserver ./internal/db` ok;
  `gofmt -l` clean; `go vet` (plain and `-tags integration`) clean on touched packages.
- Integration (`-tags integration -count=1 -p 1`): `internal/licensingchange` (13 tests incl. migration up/down), `operatingmarket`,
  `jurisdiction`, `db`, `stagingseed`, `cmd/seed-market`, `cmd/seed-staging-world`, `launchgov`, `rg`, `tenant`, `adjustment`, `aml`,
  `bonus`, `identity`, `kyc`, `marketgate`, `payoutinstrument`, `casino`, `sportsbook`: ok. `payments` and `httpserver`: see the final
  report (section 11).
- NOT run: Back Office vitest (no frontend change); the full repository integration suite.

## 6. Extension hook for slice 1 (licence lifecycle, migration 0176)

To govern a widening licence change (reinstate, extend `expires_at`, set `issued_at` earlier): in ONE migration add a
`licensing_change_kinds` row (for example `licence_status_change`, `platform_only = true`), a `licensing_change_classify` branch
(widening iff it reinstates or extends; narrowing iff it suspends / expires / shortens), a `licensing_change_row_matches` branch
for `licences` UPDATE, and `CREATE TRIGGER zz_licensing_change_governed BEFORE UPDATE ON licences` (the 0177 trigger on licences is
INSERT only, so slice 1's own UPDATE path is untouched today). In Go add a file in `internal/licensingchange` that calls
`register(...)` with decode / precheck / execute (execute calls slice 1's writer). Until all exist the kind fails closed.

## 7. Merge instructions for the orchestrator

1. **Merge 0176 (slice 1) first, then this branch.** Both migrations may `CREATE OR REPLACE actor_proof_require`. This branch's
   verifier is the 0175 body plus ONE marked block (`-- 0177 (licensing / jurisdiction maker-checker...`) and one extra
   `OR p_operation LIKE 'licensing\_change:%'` in the platform-scope list. If 0176 also replaces the verifier, rebase 0177's
   section 1 onto 0176's body (keep both blocks), and make 0177's DOWN restore 0176's body (not 0175's); then re-run
   `TestMigration0177_UpDownUp_SnapshotAndDownRefusal` (it computes its baseline as the highest version below 177, so it covers
   0176 automatically) and `go test ./internal/actorproof`.
2. On this branch alone there is a version gap at 0176: `migrate verify` reports it and the two tests that assert no gap
   (`internal/reconciliation/ma020_parity_integration_test.go:673`, `internal/alerting/migration_0117_integration_test.go:515`)
   fail until 0176 lands. Not run here; expected to pass after step 1.
3. If slice 1 adds a licences UPDATE trigger or new NOT NULL columns, check `licensing_change_row_matches` for `licences` INSERT
   (it pins `status = 'active'`, `issued_at` / `expires_at` NULL at creation). A licence created with dates by slice 1 would need
   that branch extended (fails closed with LC040 meanwhile).
4. Registry / governance updates are the orchestrator's (I did not edit them): proposed text in section 9.

## 8. Human decisions pending (exact one-question requests; interim safe behaviour in force)

1. **HDR-M-1 (approver roles):** "Which roles may approve a licensing or market-widening change (licence creation, licence binding,
   country enablement, jurisdiction configuration): platform administrators only, a platform compliance role, or both?" Interim:
   ACTIVE `platform_admin` only, platform scope.
2. **HDR-M-1 (count):** "How many approvals, by distinct people other than the requester, must a widening licensing or market change
   have: one or two?" Interim: two (three humans including the requester). Note: a platform team with fewer than three
   `platform_admin` Persons cannot open a market under the interim rule.
3. **Unbind / suspend:** "May one platform administrator alone unbind a licence from a tenant (which immediately closes every market
   of that tenant), or must a second person approve it?" Interim: single actor with a reason code (fail-closed direction).
4. **Tenant-requested narrowing:** "When tenant compliance staff ask to disable one of their own countries, may that take effect
   immediately, or must platform staff approve it first?" Interim: one platform approval (tenant staff never execute).
5. **Approval window:** "Is 24 hours the right maximum age of an approval at execution, and 72 hours the right lifetime of an open
   request?" Interim: 24 h / 72 h.

## 9. Proposed text for the orchestrator (registry / ADR; not edited by me)

- Task registry MKT-DUAL-1: "IMPLEMENTED in software by migration 0177 (`internal/licensingchange`): every runtime write of
  `licence_country_ceilings`, `operating_country_policies`, `tenant_jurisdiction_configs`, `licences` (INSERT) and
  `tenants.licence_id` needs an executed licensing change of the same transaction; widening needs two approvals by distinct
  Persons (interim HDR-M-1: platform_admin only). HDR-M-1 remains OPEN for the role and count."
- Proposed ADR (number assigned by the orchestrator), "Licensing and jurisdiction change approvals": context = G-1 / G-2 / G-9;
  decision = sections 2 and 3 above; consequences = the table owner path stays the only non-governed writer (synthetic seeding,
  fixtures, migrations); the HDR-M-1 values live in `licensing_change_kinds`; Back Office (G-10) builds on these routes.

## 10. Security review scope (what was and was not reviewed)

Reviewed by me (security specialist, code-level): the new SQL guards, RLS, grants, verifier change, proof claims and step-up
classes, the Go service and the HTTP mapping / redaction, and the adapted tests. NOT reviewed / out of scope: an independent second
review (required: `code-reviewer`, `architect` for the cross-cutting primitive), penetration testing, production key management,
the slice-1 interaction after merge, and Back Office UX. Residual risks:

- **R1 (accepted, documented):** the table OWNER role bypasses the governed-write guard (as it bypasses every trigger it could
  disable). Production must keep the owner credential out of application runtime (already the case: platform-api runs as
  `igaming_runtime`; the staging runner's owner login is synthetic-only by anchor and APP_ENV).
- **R2:** an `approved` row is never persisted (approval and execution are one transaction); a domain refusal at execution rolls
  the final approval back, and the request stays `requested`. A request whose first approval aged past 24 h can no longer reach
  execution (`APPROVALS_STALE`): cancel and refile.
- **R3:** tenant visibility of licensing-change audit rows is the base columns only (no `TenantPresentation` entry: fail closed);
  G-11 (tenant-visible licence-assignment history) is therefore only partly served (action names, not before / after).
- **R4:** expiry is lazy (materialised when a decision meets an expired request); no sweeper. Expired requests cannot be decided.
- **R5:** the licence-create match pins the creation shape; any future licence column set at creation needs the SQL branch updated.
