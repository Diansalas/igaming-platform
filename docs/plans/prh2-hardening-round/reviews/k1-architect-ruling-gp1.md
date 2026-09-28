# Architect ruling — K1 G-P1 and `grantee_person_id` (2026-09-28)

**Reviewer:** `architect`. The orchestrator recorded this ruling.

**Scope:** ADR 0099 §19.2, on K1 branch head `0f34d36`. Read-only.

## 1. G-P1: DEFERRED out of PRH-2, refused fail-closed

G-P1 (a platform-originated grant for tenant X's `finance` staff) always names a tenant-scoped grantee. `tenant_id IS NULL` holds exactly when the role is `platform_admin`, and migration 0011's `dual_scope_isolation` hides every tenant-scoped `staff_users` row from platform sessions.

**Options considered:**
- **A column-restricted view: rejected.**
  - A view cannot bypass FORCE RLS without `SECURITY DEFINER`, which 0112 forbids, and column grants cannot vary per session family.
  - So it would need a platform-wide read policy on `staff_users`, whose rows include `password_hash`, email and other PII. That widens tenant isolation.
  - Any future proposal to do it is a **human + security decision**.
- **A tenant co-request:** functionally the same as G-T, which already exists. A tenant-countersigned proposal is the preferred shape if G-P1 is ever needed.
- **Defer: CHOSEN.**
  - HD-PRH2-2 (c) is fully served by G-T: the tenant admin requests and a platform approver co-approves.
  - HD-PRH2-6 is served by G-P2.
  - No RLS policy is widened. The change is stricter and reversible.
  - `product-owner-proxy` is informed, because a flow is removed from an ACCEPTED ADR.

**Conditions:**
- **I-1:** keep the fail-closed `CG010` refusal (no request row, no success audit) and test it. Map it to a legible HTTP error. Expose no G-P1 route or UI.
- **I-2:** label G-P1 NOT IMPLEMENTED (DEFERRED).

## 2. The `grantee_person_id` snapshot: SOUND, with conditions

The snapshot is forced from the live row and immutable. It is sound for R-4 because migration 0034's `staff_users_person_id_append_only` makes a non-NULL `person_id` immutable, so 0034 is load-bearing and must be pinned. A grantee with a request or grant cannot be deleted (NO ACTION FKs), and no staff mutation API exists.

| Point | Session | Must enforce |
|---|---|---|
| Request | tenant (G-T) or platform (G-P2) | R-1, R-4, R-5, R-6 and R-9 against the live row; force the snapshot (present today) |
| Approval, tenant grantee | platform | R-4 against the snapshot only. Status and role cannot be verified here: a **stated residual** |
| Approval, platform grantee (G-P2) | platform | Re-read the live row: `active`, `platform_admin`, and `person_id` = snapshot; otherwise `CG011` |
| Use / execution (K2/K3, §7.4) | tenant or acting | Re-read the grantee `FOR SHARE`: `active`, eligible role, tenant = the grant's tenant, **live `person_id` = `grantee_person_id`**. A mismatch means the grant does not count |

**Implementer conditions:**
- **I-3:** a test that a caller-supplied `grantee_person_id` that differs from the live value never persists.
- **I-4:** a test pinning the 0034 dependency, which must fail if that trigger is missing.
- **I-5:** the G-P2 live re-read in the approval trigger, plus a mutant and the test that kills it.
- **I-6 (a K2/K3 condition):** the §7.4 re-check includes live `person_id` = `grantee_person_id`.

**Security conditions:**
- **S-a:** confirm the residual that a tenant grantee's status and role cannot be verified at approval time.
- **S-b:** extend STAFF-LIFECYCLE-1. Any suspend, role-change, tenant-change or Person-unlink path must, in the same transaction, cancel that staff member's pending grant requests and revoke their grants.
- **S-c:** the K1 security review covers the snapshot column and the G-P1 refusal path.

## 3. ADR 0099 edits

Exact text is given in the ruling for §3.3, §4, the new §4.1, §7.4, §7.5, §8.4, §13 (INV-CAP-11 and INV-CAP-12), §16 and §19.2. The K1 implementer applies them on the K1 branch, because that branch owns ADR 0099's current text.
