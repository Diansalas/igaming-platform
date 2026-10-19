# lic6 record: licensing hardening after the security re-review (migration 0179)

- **Worktree / branch:** `.claude/worktrees/lic6`, `lic6-hardening`, from `claude/licensing-jurisdiction-completion` at `ba9b594`.
- **Migration:** `0179_licensing_hardening` (up and down).
- **Test status:** the author could not run the integration harness. The SQL and the integration tests are **unrun**; the orchestrator builds the database and runs them (section 4). Unit tests, `go vet` (plain and `-tags integration`), `gofmt` and `go test ./internal/db` were run.

## 1. Deliverables

| Item | Label | Notes |
|---|---|---|
| L-1 `licence_versions.changed_by` from the authorising request | IMPLEMENTED (SQL unrun) | `licences_write_version` (SECURITY DEFINER) selects the executed request of this txid that matches the row write (same `licensing_change_row_matches` predicate as the guards) and records its `executed_by` and id; new nullable `licence_versions.change_request_id` (FK to `licensing_change_requests`). No read of `app.platform_admin_principal_id`. Owner path: NULL / NULL. API `GET .../versions` and OpenAPI carry `change_request_id`. |
| L-2 tests (a) wrong-order narrowing pair, (b) `INSERT ... ON CONFLICT DO UPDATE` | IMPLEMENTED (tests unrun) | (a) A: 2031 to 2029, B: 2031 to 2030 (both narrowing against the original row, filed and executed in one txn, raw SQL with real proofs); applying A then B turns B into a widening row change carried by a narrowing request: `LC040` from `licences_update_guard`. (b) the BEFORE INSERT guard fires before conflict detection: `LC040`. No SQL change was needed. |
| I-1 guard trigger order | IMPLEMENTED (test unrun) | `TestHardeningI1_GovernanceGuardsFireLast`: on licences, tenants, tenant_jurisdiction_configs, licence_country_ceilings, operating_country_policies the last BEFORE row trigger (by name, `COLLATE "C"`) is a `zz_licensing_change_governed*` guard; on licences both guards are the last two; no other `zz_` trigger sorts among them; licence_versions has no `zz_` trigger. |
| I-2 no DELETE on licences | IMPLEMENTED (SQL unrun) | conditional `REVOKE DELETE ON licences FROM igaming_runtime` (only when the role holds it); down re-grants only where the role holds SELECT (the blanket-grant case); `deploy/init-app-role.sql` re-asserts the revoke after the blanket grant. No deny trigger added (the privilege is the control; the table also has no RLS DELETE policy). |
| I-3 licence_create match pins `review_due_at` and `evidence_reference` NULL | IMPLEMENTED (SQL unrun) | `CreateLicence` inserts neither column, so no existing creation flow changes; test on the match function. |
| I-4 read permission pin | IMPLEMENTED | the role matrix test already existed (`internal/auth`); added a static route test (`licence_registry_read_routes_static_test.go`: every registry GET uses `PermJurisdictionRegistryRead`, step-up none, no tenant scope, platform transactions only). The behavioural refusal test (`TestReadAPIs_NonPlatformPrincipalsRefused`, tenant_admin and compliance 403, anonymous 401) already existed. |
| I-5 read-only read views | IMPLEMENTED (test unrun) | `SET TRANSACTION READ ONLY` (switching to read-only is allowed after earlier statements) in `ListLicenceCeilingVersionsPage`, `ListTenantPolicyVersionsPage`, `jurisdiction.ListTenantJurisdictionConfigs`; the single-row getters are unchanged. Test: a write after the call fails 25006. |
| I-6 break-glass note | IMPLEMENTED | `docs/runbooks/licence-lifecycle.md`: operator-only, owner credential, no new access. |
| I-7 | NOT IMPLEMENTED | skipped as instructed. |

## 2. Behaviour changes

- `licence_versions.changed_by` is no longer set for owner-path writes (it was set from a session setting when that matched an active platform staff row). Any test that read `changed_by` after an owner-path change now sees NULL; governed changes (service, HTTP, executor) are unaffected.
- The version row of a licence created or changed by an executed request records the EXECUTOR (the final approver for a widening change), as before in practice.

## 3. Residuals

- The 0179 down restores DELETE only where the runtime role holds SELECT on `licences`; a database where the role had SELECT but no DELETE would gain DELETE on a down (not a case in this repository).
- L-2a depends on the existing guard recomputing widening from OLD to NEW; the request-side classification is still done at filing/execution time (a pair of narrowing requests cannot be made to widen, they are refused at apply time).

## 4. For the orchestrator

Build the database, then `-tags integration -count=1 -p 1`: `./internal/licensingchange` (new: `TestHardeningL1_*`, `TestHardeningL2a_*`, `TestHardeningL2b_*`, `TestHardeningI1_*`, `TestHardeningI2_*`, `TestHardeningI3_*`, `TestHardeningI5_*`, `TestMigration0179_*`; existing 0177 / 0178 / lifecycle tests), `./internal/jurisdiction`, `./internal/httpserver -timeout 50m -run 'Licence|ReadAPIs|Staff|OpenAPI'`, `./internal/db`, `./internal/stagingseed ./cmd/seed-market ./cmd/seed-staging-world`, and the init-role pin tests (`./internal/payoutinstrument ./internal/rg ./internal/tenant`).
Likely first failures: the snapshot test (byte-for-byte down: licences ACL after the conditional re-grant, function text), the I-1 expectation if another BEFORE trigger sorts after `zz_`, and the version attribution test if the definer cannot read the requests table under the session GUC.
