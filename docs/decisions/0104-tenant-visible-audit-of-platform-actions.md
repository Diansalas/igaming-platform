# ADR 0104 — Tenant-Visible Audit of Platform Actions (KS-AUDIT-TENANT-1)

- **Status:** PROPOSED, 2026-09-28. Drafted by `architect` for PRH-2 W0 (workstream W0-X). Nothing
  here is implemented. The implementation is PRH-2 workstream **G1** (W1), migration **0109**.
- **Decision type:** cross-domain architecture. It changes `audit_log`'s RLS (ADR 0013) and touches
  the `audit` package, the kill-switch handlers and a new tenant read projection.
- **Owner:** `architect` + `security`. **Reviewers:** `security` (hard gate, S-10), `payments`,
  `code-reviewer`, `qa`.
- **Registry:** KS-AUDIT-TENANT-1. It is launch-blocking before production launch or the first B2B
  tenant.
- **Binding inputs:**
  - plan §4 row 0109, §5-G1 and §11 (HD-PRH2-5);
  - ADR 0098 §5: HD-PRH2-5 and the orchestrator interpretation note;
  - `reviews/security.md` S-10 and §1 "G1";
  - `reviews/qa.md` W1 "G1".

  **Where plan §5-G1 says "pseudonymous until HD-PRH2-5 is answered", §11 supersedes it:
  HD-PRH2-5 is answered.**
- **Related:** ADR 0013 (audit immutability and dual-scope RLS), migrations 0014 and 0016, 0105
  (the validated platform-principal resolver), 0106 (the mixed-GUC exclusion), ADR 0102 (which
  reuses `subject_tenant_id` for alerts).

---

## 1. Context (verified at `cabca27`)

- `audit_log` (0014) has one `FOR ALL` dual-scope policy, `dual_scope_isolation` (0014:32-41). A row
  with `tenant_id` set is visible only to that tenant's session. A row with `tenant_id IS NULL` is
  visible only when `app.tenant_id` is unset.
- Append-only is enforced by triggers: `audit_log_immutable` (0014:43-53, UPDATE/DELETE) and
  `audit_log_deny_truncate` (0016:14-16).
- A platform-scope kill-switch mutation is audited with `TenantID: uuid.Nil`, and the real target
  appears only in `metadata->>'target_tenant_id'`: `auditTenantID()` at
  `payments_kill_switch_handlers.go:246-251`, used at `:326`, `:643`, `:703`, `:766` and `:813`. The target comes from the
  path and passes `canActOnTenant` (`:149`) plus a tenant-existence check (`:161-174`).
- **So a tenant never sees that the platform engaged, took over or released its own kill switch.**
- The existing tenant-context audit readers all filter explicitly on `tenant_id = $1`:
  - `admin_routes.go:1165-1172` (`queryAuditLog`; it selects no `ip_address`, `user_agent` or
    `metadata`);
  - `kyc/provider.go:672`;
  - `testsupport/noeffect/noeffect.go:81,197`.
- **Surveyed divergence (not changed here).** Some platform-admin actions already write audit rows
  **into** the target tenant's scope under `WithTenant(target)`: `admin_routes.go:277-286`
  (`brand.created`), and similarly `:555` and `:660`. `provider_credential_handlers.go` has the same
  `uuid.Nil` shape as the kill switch. See §10.
- `staff_users` (0011) has **no display-name column**: only `email`, `role` and `status`. Its
  `dual_scope_isolation` policy hides platform staff (`tenant_id IS NULL`) from tenant sessions.

## 2. Decision

Option (c) from plan §5-G1: a first-class `audit_log.subject_tenant_id`. The platform action stays
a **platform-level** row (`tenant_id IS NULL`), and the subject tenant gets **read-only**
visibility through an additional `FOR SELECT` policy.

- There is **no new write power into tenant scope**. The write side of `dual_scope_isolation` is
  unchanged.
- The append-only triggers are unchanged.

Rejected alternatives: (a), an INSERT into tenant scope, and (b), a non-atomic dual write (§9).

## 3. Migration 0109: content

```sql
ALTER TABLE audit_log ADD COLUMN subject_tenant_id UUID NULL REFERENCES tenants (id);
ALTER TABLE audit_log ADD CONSTRAINT audit_log_subject_tenant_platform_only
    CHECK (subject_tenant_id IS NULL OR tenant_id IS NULL);
CREATE INDEX idx_audit_log_subject_tenant_time
    ON audit_log (subject_tenant_id, created_at DESC) WHERE subject_tenant_id IS NOT NULL;

-- Permissive, FOR SELECT only: ORs into reads, adds nothing to INSERT/UPDATE/DELETE.
CREATE POLICY subject_tenant_read ON audit_log
    FOR SELECT
    USING (
        tenant_id IS NULL
        AND subject_tenant_id IS NOT NULL
        AND subject_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL          -- players excluded
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL -- 0106 mixed-GUC
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
    );
```

**BEFORE INSERT trigger `audit_log_subject_actor_guard`** (S-10 item 4). When
`NEW.subject_tenant_id IS NOT NULL`, all of the following are required:
- `v := NULLIF(current_setting('app.platform_admin_principal_id', true), '')` is not NULL;
- `app.tenant_id`, `app.player_account_id` and `app.principal_id` are unset;
- `NEW.actor_type = 'staff'` and `NEW.actor_id = v::uuid`;
- `EXISTS (SELECT 1 FROM staff_users WHERE id = v::uuid AND tenant_id IS NULL)`. This is the 0105
  `payment_kill_switch_session` validation (0105:32-38). A platform session can see platform staff
  rows under `dual_scope_isolation`.

Otherwise it raises `P0001`. When `subject_tenant_id` is NULL the trigger is a no-op, so every
existing writer is unaffected.

**Other properties of 0109:**
- **FK behaviour.** The default is `NO ACTION`, so a tenant with subject audit rows cannot be
  deleted. That is the intended consequence of an append-only audit trail; `ON DELETE CASCADE` would
  be refused by the immutability trigger anyway.
- **Unchanged:** `dual_scope_isolation`, `audit_log_immutable`, `audit_log_deny_truncate`, and
  every existing column and index.
- **Down:** `RAISE EXCEPTION` if `EXISTS (SELECT 1 FROM audit_log WHERE subject_tenant_id IS NOT
  NULL)`. Otherwise drop the trigger, policy, index, constraint and column in that order.
  **Caveat:** under FORCE RLS, the down's existence check must run in a scope that sees platform
  rows: `app.tenant_id` unset, which is the migration runner's normal state. The MIG test pins
  this.

The optional presentation-policy table is in §5.3.

## 4. Write path (G1)

- **`audit.Entry` gains `SubjectTenantID uuid.UUID`.** `audit.Record` refuses (in Go, before SQL) an
  entry with both `TenantID` and `SubjectTenantID` set, mirroring the CHECK.
- **The target comes from the route (S-10 item 5).** The kill-switch handlers set `SubjectTenantID
  = c.target` **only** when `c.tc.TenantID == uuid.Nil` (a platform session), where `c.target` has
  already passed `canActOnTenant` and the existence check. **The value never comes from a request
  body.** A tenant-scoped caller keeps writing `TenantID = c.target` (its own tenant), exactly as
  today.
- **In scope for G1:** every platform-scope kill-switch audit write:
  - engage;
  - request, approve, cancel and release;
  - the refusal audits (`recordKillSwitchRefusalAudit`, `:324-341`).

  `metadata.target_tenant_id` stays, for backward compatibility with
  `TestPaymentsKillSwitchAPI_AuditRecordsCarryBeforeAfterAndTargetTenant`.
  `recordKillSwitchDenied`'s foreign-tenant denial (`:178-199`) is written in the **caller's** scope
  and is unchanged.
- **Out of scope for G1:** `provider_credential_handlers.go` and the `admin_routes.go` tenant-scope
  writes. They are surveyed and registered, and are not extended without the orchestrator's go-ahead
  (plan §5-G1).
- **Downstream reuse:** ADR 0102 §4.2 ack/resolve audits of platform-owned alerts reuse
  `SubjectTenantID`.

## 5. Read path: the tenant projection (HD-PRH2-5)

### 5.1 Endpoint

- **Route:** `GET /v1/admin/audit-log/platform-actions`, with `RequireTenantScope` and
  `RequirePermission(PermAuditRead)`. This is the same gate as the existing tenant audit read
  (`routes.go:140-141`). It is a **separate** endpoint, so the existing `GET /v1/admin/audit-log`
  and every existing reader are unchanged.
- **Session:** `WithTenant(tc.TenantID)`.
- **Query:** `WHERE tenant_id IS NULL AND subject_tenant_id = $1` with `$1 = tc.TenantID`. The
  explicit filter is defence in depth; RLS is the enforcement.

### 5.2 What the tenant sees (HD-PRH2-5 as decided; orchestrator interpretation, 0098 §5)

| Field | Shown | Source |
|---|---|---|
| `id`, `created_at`, `action`, `target_type`, `target_id`, `outcome` | yes | columns |
| `actor.scope` = `"platform"`, `actor.staff_id` | **yes: identifiable, not pseudonymized** | `actor_id` |
| `actor.display_name` | **yes, when one exists (see §5.4)** | see §5.4 |
| `reason_code` | yes | per-action metadata allowlist |
| `approval_chain` (requester and approver staff ids and names, and times) | yes, **where recorded** | linked rows for the same `target_id` (e.g. the kill-switch release request → approve), plus allowlisted metadata ids |
| `before`, `after` | yes, **where recorded** | per-action metadata allowlist (for the kill switch, the existing `before`/`after` objects) |
| `ip_address`, `user_agent` | **no, by default** | privacy-presentation setting (§5.3) |
| Free-form `metadata` (any key not in the action's allowlist), `request_id` | **no, by default** | privacy-presentation setting (§5.3) |

- **The allowlist lives in Go** (`audit.TenantPresentation`), keyed by `action`, and lists the
  metadata keys that are projected. An action without an entry projects only the columns. It is
  fail closed on disclosure.
- **The underlying record is never altered or redacted.** IP, user agent and metadata stay in
  `audit_log`, readable by platform audit readers.

### 5.3 The privacy-presentation setting (configurable; hidden by default)

- **What it is.** A platform-authored, append-only, effective-dated policy. It is proposed as the
  table `audit_presentation_policies`, in 0109.
  - Scope: platform default, jurisdiction, or tenant.
  - Fields:
    - `show_network_metadata BOOL` (IP and user agent);
    - `show_free_form_metadata BOOL`;
    - `actor_presentation` ∈ {`identified`, `pseudonymous`}, supporting HD-PRH2-5's "a different
      presentation … required by a jurisdiction".
- **Resolution:** the most specific effective row. **With no row, the defaults apply:** network and
  free-form metadata hidden, actor `identified` (HD-PRH2-5).
- **Who writes it.** Only a validated platform admin, audited: a trigger forces the actor from the
  GUC, and the table has a FORCE RLS platform family. **A tenant cannot write it**, because the
  setting governs disclosure of platform staff data, not tenant data.
- **No seed row.**
- **Minimal alternative** (product-owner-proxy to choose; see §11 item 2): ship only the resolver
  with compiled-in defaults, and defer the table and write API to a registered follow-up. This
  meets "hidden by default" but not "configurable" until the follow-up lands.

### 5.4 Actor display name: ORCHESTRATOR DECISION REQUIRED (interpretation of HD-PRH2-5)

- **The problem.** `staff_users` has no display name (§1), and tenant sessions cannot read platform
  staff rows.
- **What this ADR fixes regardless of the choice:** the lookup happens server-side, in a second
  short platform-scope read (`WithoutTenant`, which sees `tenant_id IS NULL` staff), restricted to
  the `actor_id`s already returned by the RLS-filtered tenant query. It selects only `id` and the
  chosen display field, and never widens the tenant session's RLS.

| Option | What it means |
|---|---|
| (a) Recommended | 0109 adds a nullable `staff_users.display_name` (bounded length, no backfill). The platform staff-management path may set it. Until it is set, the projection shows `staff_id` with `display_name: null`. |
| (b) | Show the platform staff **email** as the display identity. It is identifiable today, but it discloses contact data. |
| (c) | Staff id only, until a display-name source exists. |

Options (a) and (c) disclose nothing beyond the id. (b) discloses more than the orchestrator's
interpretation ("staff id and display name") literally requires. **Until this is decided, the
implementation ships the id plus `display_name: null`**, which is a subset of every option.

## 6. Invariants (for `qa` and `code-reviewer`)

| ID | Invariant |
|---|---|
| AT-1 | A tenant session sees a platform row only when its `subject_tenant_id` equals the session's own `app.tenant_id`. A player session, or a mixed platform+tenant session, never sees one. RLS enforces this. |
| AT-2 | Only a validated platform principal, as the row's own staff actor, can write `subject_tenant_id`. No tenant or player session can write it (the trigger, plus `dual_scope_isolation`'s WITH CHECK on `tenant_id IS NULL` rows). |
| AT-3 | `subject_tenant_id` and `tenant_id` are never both set. |
| AT-4 | No new INSERT, UPDATE or DELETE power anywhere. The append-only triggers are byte-identical. |
| AT-5 | `subject_tenant_id` comes from the route-validated target, never a body. |
| AT-6 | By default, the tenant presentation never includes platform staff IP, user agent, `request_id` or non-allowlisted metadata. The record keeps them. |
| AT-7 | The existing tenant audit readers keep their explicit `tenant_id = $1` filter, so they do not start returning platform rows. |

## 7. Tests (QA W1 "G1"; T-1 to T-3 apply)

- **TI (the headline test).** A platform admin engages, and then releases through four-eyes, a
  switch on tenant A, and does the same on tenant B.
  - Tenant A's `platform-actions` read returns exactly A's rows, with actor id, reason, before/after
    and approval chain, and **never B's**. The same holds for B.
  - Tenant A's raw `SELECT` on `audit_log` under `WithTenant(A)` also never returns B's rows (the
    RLS level, not just the handler).
- **Player exclusion:** under `WithPlayerScope(A, p)`, zero subject rows.
- **Mixed GUC:** with `app.tenant_id=A` and `app.platform_admin_principal_id` both set, zero
  subject rows.
- **AZ/RLS write side:**
  - a tenant session inserting `subject_tenant_id` → refused;
  - a platform session whose `actor_id` ≠ the GUC principal → refused (P0001);
  - an actor that is a tenant staff id → refused;
  - `actor_type='system'` with a subject → refused;
  - `tenant_id` and `subject_tenant_id` both set → CHECK violation;
  - a nonexistent subject → FK violation.
- **Append-only:** UPDATE, DELETE and TRUNCATE of a subject row are refused (existing triggers).
- **Route-target test:** a platform request whose body carries a different `tenant_id` still records
  the path tenant. The kill-switch body refuses unknown fields, so the test asserts the refusal, and
  asserts the path target on the success path.
- **Readers unchanged (R):**
  - `GET /v1/admin/audit-log` for A returns no subject rows;
  - `kyc/provider.go:672` and `noeffect` counts are unchanged by the presence of subject rows;
  - the `TestPaymentsKillSwitchAPI_AuditRecordsCarryBeforeAfterAndTargetTenant` metadata pin still
    passes.
- **Presentation:**
  - by default, no IP, user agent, `request_id` or non-allowlisted key appears;
  - with a platform policy row enabling network metadata for a tenant, they appear for that tenant
    only;
  - with a jurisdiction row set to `pseudonymous`, the staff id is replaced;
  - a tenant cannot insert a policy row.
- **MIG:** `up`, then a subject row, then `down` → refuses; `down` with none → clean; `up` again →
  idempotent shape.
- **MUT (must be killed):**
  - the RLS predicate without `subject_tenant_id = app.tenant_id`, e.g. `IS NOT NULL` only (killed
    by the TI test);
  - the predicate without the player exclusion;
  - the trigger without the actor equality;
  - the policy changed to `FOR ALL`;
  - the handler setting the subject from `tc` instead of `c.target`;
  - the projection including `ip_address`.

## 8. KS-AUDIT-TENANT-1 closure path

1. This ADR ACCEPTED by security. §5.4 decided by the orchestrator; §5.3's form chosen.
2. G1 merged: 0109, `audit.Entry.SubjectTenantID`, the kill-switch handler writes (§4), the
   read projection (§5) and the §7 tests passing locally, labelled local.
3. Security reviews the diff (S-10 items 1–6, as superseded by HD-PRH2-5 for item 6's actor
   identity).
4. The orchestrator marks KS-AUDIT-TENANT-1 IMPLEMENTED and registers the §10 follow-ups.

## 9. Alternatives rejected

| Alternative | Why rejected |
|---|---|
| (a) An INSERT-only "platform writes into tenant scope" family | New write power into tenant scope. The platform action would stop being a platform-level row, and a regulator export could not distinguish it from a tenant's own action without trusting metadata. |
| (b) A dual write: one platform row plus one tenant row | Not atomic under separate scopes, drifts, and doubles the record. |
| Indexing `metadata->>'target_tenant_id'` and a policy over it | An untyped JSONB with no FK, no CHECK and no actor guard. The platform writer could put anything there. |
| Pseudonymous by default | Superseded by HD-PRH2-5 (identifiable). Pseudonymity remains available as a presentation-policy value. |
| Extending the existing `GET /v1/admin/audit-log` | It would change the semantics of every existing tenant audit consumer. A separate endpoint keeps AT-7 trivially true. |

## 10. Open items

1. **ORCHESTRATOR DECISION REQUIRED:** the §5.4 display-name source ((a) recommended, (b) or (c)).
2. **Product-owner-proxy:** the full §5.3 table plus write API in G1, or only the resolver plus
   defaults, with the table as a registered follow-up. Either way, the defaults are hidden and
   identified.
3. **Register** (recommended wording for the orchestrator), **PLAT-AUDIT-SUBJECT-1**: "platform
   actions on a tenant written with `tenant_id = NULL` outside the kill switch
   (`provider_credential_handlers.go`), and platform actions written directly into tenant scope
   (`admin_routes.go:277-286,555,660`), should converge on `subject_tenant_id` (ADR 0104). The
   latter currently appear in the tenant's ordinary audit read as if they were tenant actions, apart
   from the actor id."
4. **Whether a suspended platform staff principal may still write subject rows.** The trigger
   mirrors 0105, which does not check `status`. This is the same S-4 class as K1, and follows K1's
   answer.
5. **The platform audit reader** (`GET /v1/admin/platform/audit-log`) may add `subject_tenant_id`
   to its output. This is additive and optional.

**Handover/DoD:**
- **Artefacts:**
  - this ADR ACCEPTED;
  - G1 merged per §8;
  - `docs/architecture/12-audit-reporting-architecture.md` and the security-architecture doc
    updated (the subject-tenant pattern, the presentation policy);
  - a HANDOVER row: "tenant-visible platform audit: IMPLEMENTED (kill switch only; other platform
    actions per PLAT-AUDIT-SUBJECT-1)".
- **Registry (orchestrator):** KS-AUDIT-TENANT-1 → IMPLEMENTED after security sign-off.
