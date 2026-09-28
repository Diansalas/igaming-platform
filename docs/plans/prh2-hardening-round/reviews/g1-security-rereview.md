# Security re-review — PRH-2 G1 (2026-09-28)

**Reviewer:** `security`. The orchestrator recorded this review.

**Scope:** `aa2596f`, covering the G1 delta `ac073d8..23c642d` (fix commits `1fa5b77` and `23c642d`), excluding the merged-in main content. Tested on the merged tree (0107–0109), on a private DB that was dropped afterwards.

## Verdict: ACCEPT

- G1-C1, G1-C2 and G1-C3 are closed.
- F-5 (the second chain query) adds no disclosure beyond the tenant's own audit read.
- KS-AUDIT-TENANT-1 may be marked IMPLEMENTED once G1 merges.

## Checks

**G1-C1 (down migration): closed.** The down now runs three checks:
1. the `audit_log` subject check;
2. the platform staff check, with no GUC;
3. a per-tenant loop that sets `app.tenant_id` for each tenant and checks `staff_users.display_name`.

`tenants_read` (`0077:55`) admits every non-player session, so the loop reads every tenant.
- **Proof:** `TestMigration0109_Down_RefusesWhenTenantStaffDisplayNameSet` FAILS on the old `ac073d8` down and PASSES on the new one.
- **R-1 (Low):** `FOR … IN SELECT` fetches lazily and re-applies RLS with the current GUCs. Snapshot the ids first (`array_agg`, then `FOREACH`), or pin the dependency on `tenants_read` in a comment.

**G1-C2:** closed. The platform self-rename uses `WithPlatformAdmin`.

**G1-C3:** closed. The DB CHECK class and the Go ranges both include U+2060–2064, U+FEFF and U+061C.
- 0109 up was edited in place. That is acceptable only because 0109 has never been applied outside dev DBs. A dev DB that ran the old 0109 must be rebuilt.

**F-5 (chain query): accepted.**
- It runs in the same `WithTenant` transaction, with an explicit `$1 = tc.TenantID` filter on both arms, and covers kill-switch actions only.
- There is no new projection. Chain entries carry only `id`, `action`, `outcome`, `created_at`, `actor_staff_id` and `actor_display_name`, which is strictly less than a page entry, and a subset of what `/v1/admin/audit-log` already shows for the tenant's own rows.
- There is no new disclosure.

**F-4 (readers unchanged):**
- The noeffect and casino tests call the real functions.
- The kyc test copies the SQL text. That is acceptable as supplementary coverage, not as a pin.
- **R-2 (Low):** hoist the SQL into a shared `const`, or statically assert that `provider.go` contains the exact string.

**F-9:** accepted. A body `tenant_id` gets a 400, and the stored `subject_tenant_id` equals the path target.

**Mutant re-kill on the merged tree:** RLS tenant equality removed from `subject_tenant_read` is **KILLED** by `TestSubjectTenantRLS_TIHeadline`.

**Tests (private DB, not CI):**
- `-race -tags integration`: audit, identity, kyc and casino ok;
- httpserver focus: 47 PASS.

The full httpserver `-race` suite was not run.
