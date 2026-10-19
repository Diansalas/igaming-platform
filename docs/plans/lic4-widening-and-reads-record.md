# Slice 4a record: licence lifecycle widening through the licensing change primitive, platform read APIs, security-review fixes (migration 0178)

- **Worktree / branch:** `.claude/worktrees/lic4`, branch `lic4-widening-and-reads`, from `claude/licensing-jurisdiction-completion` at `7d414cc`.
  The scope extension (security review `docs/plans/licensing-security-review-record.md`, F-1..F-5) is in the same migration.
- **Migration:** `0178_licence_lifecycle_governed_changes` (up and down) only.
- **Not a legal opinion.** Software capability only; no licence, jurisdiction or country was created, seeded or approved.
- **Test status:** the author's shell could not run the integration harness (the permission system refused `priv_db.sh` / `priv_test.sh`).
  Everything below labelled IMPLEMENTED is **code-complete, vetted (plain and `-tags integration`) and unit-tested; the SQL and the
  integration tests have NOT been run by the author** (no database was reachable). The orchestrator runs them (section 7) and
  returns failures. Until they pass, treat the SQL as UNVERIFIED.

## 1. Deliverables and labels

| Deliverable | Label | Where |
|---|---|---|
| Four licensing change kinds (`licence_status_change`, `licence_validity_dates_change`, `licence_review_due_change`, `licence_evidence_reference_change`): registry rows, `licensing_change_classify` branches (DB computes widening against the locked licence row), `licensing_change_row_matches` branch for `licences` UPDATE, governed BEFORE UPDATE trigger, Go `register()` | IMPLEMENTED (SQL unrun) | `migrations/0178*.up.sql`, `internal/licensingchange/licence_lifecycle_kinds.go` |
| The four lifecycle PUT routes file widening changes (202) and execute narrowing / neutral ones (200); bodies unchanged | IMPLEMENTED (integration unrun) | `internal/httpserver/licence_lifecycle_handlers.go` |
| F-1 (HIGH) `licences_update_guard`: identity columns immutable for every role (`LC042`); non-owner UPDATE needs an executed request of the matching kind in this txid (`LC040`); a widening row change (classified again from OLD to NEW) needs a widening request that holds its approvals | IMPLEMENTED (SQL unrun) | 0178 section F-1 |
| F-2 `licence_versions`: `version_no` bigint; INSERT policy needs `pg_trigger_depth() > 0`; INSERT revoked from `igaming_runtime` (migration and `deploy/init-app-role.sql`); SECURITY DEFINER writer with pinned `search_path` recording `changed_by` only for an ACTIVE platform staff row (else NULL); per-licence row lock before `max+1` | IMPLEMENTED (SQL unrun) | 0178 section F-2 |
| F-3 sweep self-monitoring: kinds `licence.sweep_failed` (p1; phases `list_tenants`, `tenant_read`, `raise`) and `licence.unmonitored` (p2; bound licence with no `expires_at` and no more specific alert) | PARTIALLY IMPLEMENTED | `internal/jurisdiction/licence_sweep.go`, `internal/alerting/*`; **unbound licences are NOT reported** (section 5, D-2) |
| F-4 brand ownership at filing for tenant-filed brand-rung policies (Go precheck as not found + SQL classify `LC010` for tenant sessions) | IMPLEMENTED (SQL unrun) | `kinds.go`, 0178 |
| F-5 open-request uniqueness per requester class (`payload_hash, requested_scope`), same index name | IMPLEMENTED (SQL unrun) | 0178 |
| Read APIs: `GET /v1/admin/jurisdictions/{id}`, `/licences/{id}` (+ `tenant_ids`; `tenant_ids` also on the list), `/licence-country-ceilings?licence_id=`, `/operating-country-policies?tenant_id=`, `/tenants/{id}/jurisdiction-configs`; permission `jurisdiction_registry:read` (platform_admin only), step-up none, `?limit&offset` envelope | IMPLEMENTED (integration unrun) | `internal/httpserver/licence_registry_read_handlers.go`, `internal/jurisdiction/registry_reads.go`, `internal/operatingmarket/admin_reads.go` |
| OpenAPI, golden route table, static pins, runbooks | IMPLEMENTED | `docs/api/openapi/platform-api.yaml`; pins: 239 routes / 143 mutating; read 96 (critical 70, none-mutating 43 unchanged) |
| Tenant-scope read of the tenant's own licence assignment history | NOT IMPLEMENTED (G-11 stays a security call, D-3) | - |
| G-14 server-side default | IMPLEMENTED (verified: none exists) + regression test | `TestG14_NoServerSideLicensingModelDefault` |
| Back Office pages | NOT IMPLEMENTED | out of scope |

## 2. Design decisions (local and reversible; flagged where security must confirm)

- **D-1 No new proof operation family.** The brief asked for a new proof-bound operation for licence UPDATE. It was not added because
  every non-owner UPDATE of `licences` now requires an EXECUTED licensing change request of this transaction, and each such request
  already carries a verified signed proof for `licensing_change:request` and `licensing_change:execute` (and a proof per approval),
  checked by the 0177 request guard. A made-up platform GUC therefore authorises nothing (tests `TestSecurityF1_*`). Adding a
  duplicate family would have meant replacing the shared `actor_proof_require` a third time for no extra property. `security`
  should confirm or ask for the family.
- **D-2 Unbound licences are not reported by the sweep.** Listing licences with no tenant needs a read path the sweep does not have
  (the `licences_read` policy admits a platform principal or the tenant's own licence). A service-identity SELECT policy on `licences`
  (or a SECURITY DEFINER lister) is a security decision. Interim: bound licences only; an unbound licence affects nobody.
- **D-3 G-11 (tenant sees its licence assignment history) left as a security call.** `licence_versions` is platform-only by RLS and
  its `changed_by` is a platform staff id; exposing history to tenants needs a new RLS path plus redaction. The tenant already reads
  its own bound licence row through `licences_read`. Not built.
- **D-4 Owner path.** The table owner keeps writing the lifecycle columns (seeding, fixtures) without a request, exactly like the other
  governed tables (lic2 R1). The owner can NOT change the identity columns (a reviewed migration must disable the trigger).
- **D-5 Governing every licence UPDATE, narrowing included.** All four lifecycle writes (also the evidence reference and suspend) go through
  the primitive. Narrowing and neutral changes requested by a platform admin execute in the requester's own transaction (0 approvals),
  as for `tenant_licence_unbind`.
- **D-6 `reason_code`** of the lifecycle PUTs must now match the licensing change code format (1..64 of `[A-Za-z0-9_.:-]`); free text is
  refused 400. Intended behaviour change.
- **D-7 The Go precheck and the SQL classifier are two implementations of one rule.** A no-op is refused early in Go (400, stores nothing)
  and by the database (`LC010`). The evidence payload key is `licence_evidence_reference` (the ADR 0114 S0 guard reserves the other literal in `internal/httpserver`).
- **D-8 Policies read by a platform admin** bind the TARGET tenant (`app.tenant_id`) inside the platform session for the read only and clear it
  again, as the licensing change executor already does for writes. No platform read policy was added to `operating_country_policies`.
  The target tenant id is the platform operator's explicit query parameter (same trust as the path id of `PUT /v1/admin/tenants/{tenantID}/licence`).
- **D-9 Existing list routes keep their shape** (bare arrays for `GET /v1/admin/jurisdictions` and `/licences`); the new lists use the shared
  `{items, limit, offset, total}` envelope. Changing the old two would break clients; paginate them later if a Back Office needs it.

## 3. Intended behaviour changes (existing tests adapted, no assertion weakened except where stated)

| Change | Tests |
|---|---|
| Widening lifecycle PUTs answer 202 (not 200) and need two approvals; narrowing keeps 200 | `licence_lifecycle_http_integration_test.go`: the reinstatement in the round trip is now filed and executed through `executeFiledChange`; the round trip's other steps (set dates / review / evidence from empty) are narrowing / neutral and unchanged |
| **Fixture repair (merged-tree failure of lic1+lic2):** `lcSetup` created the licence with `POST /v1/admin/licences`, which since 0177 answers 202 with no licence id (every later call was a 404). The licence is now created through the owner pool (`launchfix.OwnerPool`, `WithPlatformAdmin`), and `ensureLicensingProofs` is installed in the HTTP lifecycle tests. This repairs `..._PlatformAdminRoundTrip_AuditedAndVersioned`, `..._ValidationRefusals`, `..._NonPlatformPrincipalsRefused` (and the stale / fresh step-up test needs the proof issuer) | same file |
| `licence_versions.changed_by` is NULL when the session's principal is not an active platform staff account | `TestSecurityF2_*` |
| A non-owner UPDATE of `licences` outside an executed change is refused (`LC040`); identity columns are immutable (`LC042`) | `TestLifecycle_GovernedTrigger_*`, `TestSecurityF1_*` |
| `licence.unmonitored` is raised for a bound licence with no `expires_at` and no other alert; the pre-existing sweep tests are unchanged because none of their licences is in that state | `internal/jurisdiction/licence_lifecycle_integration_test.go` |
| Pins: `staff_route_static_test.go` 234 -> 239 routes (143 mutating), `staff_stepup_integration_test.go` read 91 -> 96, golden table +5 reads, `kinds_test.go` six -> ten kinds, `alert_test.go` subjectless allowlist +2 | summed honestly |
| `ErrNotFound` of `operatingmarket` is no longer reserved | comment only |

## 4. Verification

- Author-run (no database): `gofmt -l` clean; `go vet ./...` and `go vet -tags integration` on every touched package clean; `go test ./internal/... ./cmd/...` all green (includes
  `internal/db` migration pins, `internal/httpserver` route / OpenAPI / golden tests, `internal/licensingchange` unit tests, `internal/auth`).
- NOT run by the author: every integration test and the migration SQL (section 7 lists what the orchestrator should run).

## 5. Residuals

- D-2, D-3 above. Owner credential bypass of the lifecycle columns (lic2 R1). Alert delivery to a person (ALERT-DELIVERY-1). Dispatcher GUC forgery of `licence.*` alerts (F-6, platform-wide).
- The sweep reports `licence.sweep_failed` only through the same dispatcher write path; if that write fails too it is logged only.
- A widening request can no longer execute if the licence state already equals the target at the last approval (the approval rolls back, the request stays open: cancel it).
- With fewer than three platform_admin Persons no widening licence change can be completed (HDR-M-1 interim).
- `security` and `code-reviewer` review are required (this touches auth, RLS, grants, triggers and admin permissions).

## 6. Proposed text for the orchestrator (not applied)

- Registry MKT-DUAL-1: "IMPLEMENTED for licences INSERT and UPDATE as of migration 0178 once the integration tests pass: lifecycle widening is maker-checker, the identity columns are immutable, a platform GUC alone authorises no licence write."
- Close G-1 / G-2 residuals of lic1 section 4 (single-actor widening) when 0178 is verified. G-11 stays open (D-3). G-14 closed (server has no default; the Back Office form preselection is a UI item).
- Security review record F-1, F-2, F-4, F-5 fixed; F-3 partly (D-2); F-6..F-9 unchanged.

## 7. For the orchestrator: rebuild the database (REBUILD DB), then run

`-tags integration -count=1 -p 1`:

1. `./internal/licensingchange` (all; new: `TestLifecycleKinds_*`, `TestLifecycleWidening_*`, `TestLifecycle_*`, `TestSecurityF1_*`, `TestSecurityF2_*`, `TestSecurityF4_*`, `TestSecurityF5_*`, `TestSecurity_TenantA_*`, `TestMigration0178_*`; existing `TestMigration0177_*`)
2. `./internal/jurisdiction` (`-run 'Licence|Lifecycle|Sweep'` then the package)
3. `./internal/alerting -run 'Migration0176|Migration0178|Licence|KindParity'`
4. `./internal/operatingmarket`, `./internal/launchgov -run 'Licence|Validity'`, `./internal/db`
5. `./internal/httpserver -timeout 50m -run 'Licence|Jurisdiction|LicensingChange|ReadAPIs|G14|Staff|OpenAPI|AssignTenantLicence|Market'`
6. `./internal/stagingseed ./cmd/seed-market ./cmd/seed-staging-world` (owner paths touch `licences`).

Likely failure points to look at first: the SQL of 0178 (never executed by the author), `TestMigration0178_UpDownUp_SnapshotAndDownRefusal` (byte-for-byte down restore, including the ACL of `licence_versions` after the REVOKE / re-GRANT, the 0176 writer text and the policies), `TestSecurityF5_*` (the executing brand/tenant-rung narrowing fixture), `newReadsWorld` (fixture writers called through the owner pool), and the stale / fresh step-up test (needs staff with a Person).
