# ADR 0104 — Tenant-Visible Audit of Platform Actions (KS-AUDIT-TENANT-1)

> **Status 2026-09-28: ACCEPTED** (revision 2). Ledger-finance and security accepted it: see `docs/plans/prh2-hardening-round/reviews/` (`adr-0102-0104-security-confirmation.md`, and for 0102 also the confirmation section of `adr-0102-ledger-finance.md`); product-owner-proxy accepted it in `adr-0102-0104-product-owner-proxy.md`. Implementation is NOT IMPLEMENTED until the PRH-2 workstream merges. The earlier PROPOSED status below is historical.

- **Status:** PROPOSED, **revision 2**, 2026-09-28. Drafted by `architect` for PRH-2 W0 (W0-X).
  Nothing here is implemented. The implementation is PRH-2 workstream **G1** (W1), migration **0109**.
- **Revisions:**

  | Rev | Base | Change |
  |---|---|---|
  | 1 | `f06818b` | Initial draft |
  | 2 | `10e0471` | Applies the three changes below. Mapped in §11 |

  Revision 2 applies:
  - `reviews/adr-0102-0104-security.md` (ACCEPT WITH CONDITIONS: SA-1..SA-6, Q7, C-104-1..6);
  - `reviews/adr-0102-0104-product-owner-proxy.md` (ACCEPT WITH CONDITIONS: resolver-only, with
    the table deferred as AUDIT-PRESENTATION-POLICY-1);
  - the orchestrator's decision on display name (option (a), recorded at `0939c5a`).

- **Decision type:** cross-domain architecture. It changes `audit_log`'s RLS (ADR 0013) and adds
  `staff_users.display_name`.
- **Owner:** `architect` + `security`. **Reviewers:** `security` (hard gate; diff review,
  C-104-6), `payments`, `code-reviewer`, `qa`.
- **Registry:** KS-AUDIT-TENANT-1 (launch-blocking before production or the first B2B tenant).
  Related rows, already registered: PLAT-AUDIT-SUBJECT-1 and AUDIT-PRESENTATION-POLICY-1.
- **Binding inputs:** plan §4 row 0109, §5-G1, §11 (HD-PRH2-5); ADR 0098 §5; security S-10 and the
  revision-2 review; QA W1 "G1".
- **Related:** ADR 0013; migrations 0011, 0014, 0016, 0105, 0106; ADR 0099 §6 (`app.acting_*`);
  ADR 0102 (reuses `subject_tenant_id`).

---

## 1. Context (verified at `cabca27`)

- `audit_log` (0014) has one `FOR ALL` dual-scope policy, `dual_scope_isolation` (0014:32-41).
  Append-only is enforced by `audit_log_immutable` (0014:43-53) and `audit_log_deny_truncate`
  (0016:14-16).
- **Platform-scope kill-switch mutations** are audited with `TenantID: uuid.Nil`, and the target is
  kept only in `metadata->>'target_tenant_id'`.
  - `auditTenantID()`: `payments_kill_switch_handlers.go:246-251`, used at `:326`, `:643`, `:703`,
    `:766` and `:813`.
  - The target comes from the path, and passes `canActOnTenant` (`:149`) and an existence check
    (`:161-174`).
- **The tenant-context readers filter explicitly:**
  - `admin_routes.go:1165-1172` (`queryAuditLog`; it selects no IP, user agent or metadata);
  - `kyc/provider.go:672`;
  - `testsupport/noeffect/noeffect.go:81,197`;
  - **`casino/rejections.go:257-266`**: its `JOIN audit_log a ON a.tenant_id = t.tenant_id`, with
    `t.tenant_id = $1` (SA-1).
- **Surveyed divergence (registered as PLAT-AUDIT-SUBJECT-1):** `provider_credential_handlers.go`
  uses the `uuid.Nil` shape. `admin_routes.go:277-286,555,660` write platform actions **into**
  tenant scope.
- `staff_users` (0011) has no display-name column, and its policy hides platform staff from tenant
  sessions.
- **SA-3 (owned by ADR 0099/0112, not this ADR).** ADR 0099's "platform acting in X" sessions leave
  `app.tenant_id` unset. They therefore satisfy `dual_scope_isolation`'s `tenant_id IS NULL` arm,
  and could read every platform audit row and insert platform rows. The fix belongs in 0099/0112.
  This ADR's own new policy and trigger exclude `app.acting_*` (§3).

## 2. Decision

Option (c): a first-class `audit_log.subject_tenant_id`. The platform action remains a
platform-level row. The subject tenant gets **read-only** visibility through an additional
`FOR SELECT` policy.

- There is **no new write power into tenant scope**, and the append-only triggers are unchanged.
- The tenant presentation is **resolver-only with compiled-in defaults** (product-owner-proxy): the
  actor is identified, and network and free-form metadata are hidden. The configurable table and its
  write API are deferred to **AUDIT-PRESENTATION-POLICY-1**.

## 3. Migration 0109: content

```sql
ALTER TABLE audit_log ADD COLUMN subject_tenant_id UUID NULL REFERENCES tenants (id);
ALTER TABLE audit_log ADD CONSTRAINT audit_log_subject_tenant_platform_only
    CHECK (subject_tenant_id IS NULL OR tenant_id IS NULL);
CREATE INDEX idx_audit_log_subject_tenant_time
    ON audit_log (subject_tenant_id, created_at DESC) WHERE subject_tenant_id IS NOT NULL;

CREATE POLICY subject_tenant_read ON audit_log
    FOR SELECT
    USING (
        tenant_id IS NULL
        AND subject_tenant_id IS NOT NULL
        AND subject_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
    );

-- C-104-4 / orchestrator decision (option a)
ALTER TABLE staff_users ADD COLUMN display_name TEXT NULL
    CONSTRAINT staff_users_display_name_hygiene
    CHECK (display_name IS NULL OR (char_length(display_name) BETWEEN 1 AND 100
                                    AND display_name !~ '[[:cntrl:]]'));
```

**BEFORE INSERT trigger `audit_log_subject_actor_guard`** (S-10(4), C-104-1, SA-2). When
`NEW.subject_tenant_id IS NOT NULL`, all of the following are required:
- `v := NULLIF(current_setting('app.platform_admin_principal_id', true), '')` is NOT NULL;
- **all of these are unset:**
  - `app.tenant_id`;
  - `app.player_account_id`;
  - `app.principal_id`;
  - `app.platform_service_id`;
  - `app.acting_tenant_id`;
  - `app.acting_platform_principal_id`;
- `NEW.actor_type = 'staff'` and `NEW.actor_id = v::uuid`;
- `EXISTS (SELECT 1 FROM staff_users WHERE id = v::uuid AND tenant_id IS NULL)` (the 0105:32-38
  validation).
- **No `status` check (Q7).** The audit records what happened. An account's status governs
  authorization, and authorization is enforced elsewhere.

Otherwise it raises P0001. When `subject_tenant_id` is NULL, the trigger is a no-op.

**Other properties of 0109:**
- **FK:** `NO ACTION`. A tenant that has subject rows cannot be deleted; this is intended.
- **Unchanged:** `dual_scope_isolation`, both append-only triggers, and every existing column and
  index.
- **Down:** refuse if any `subject_tenant_id` is set, or if any `staff_users.display_name` is
  non-NULL. Otherwise drop, in reverse order: the trigger, the policy, the index, the constraint,
  the column, and `display_name`.
  - The existence check runs with `app.tenant_id` unset, so it can see platform rows. The MIG test
    pins this.
- **Not in 0109 (product-owner-proxy):** there is no `audit_presentation_policies` table
  (AUDIT-PRESENTATION-POLICY-1).

## 4. Write path (G1)

- **`audit.Entry.SubjectTenantID`.** `audit.Record` refuses an entry with both `TenantID` and
  `SubjectTenantID` set, in Go, before any SQL.
- **The target comes from the route (S-10(5)).**
  - The kill-switch handlers set `SubjectTenantID = c.target` **only** when
    `c.tc.TenantID == uuid.Nil`, that is, in a platform session, after `canActOnTenant` and the
    existence check.
  - **The value never comes from a body.**
  - Tenant-scoped callers are unchanged.
- **In scope:** every platform-scope kill-switch audit write:
  - engage;
  - request, approve, cancel and release;
  - `recordKillSwitchRefusalAudit` (`:324-341`).

  `metadata.target_tenant_id` stays. `recordKillSwitchDenied` (`:178-199`, which writes in the
  caller's own scope) is unchanged.
- **`display_name` write path (C-104-4).**
  - It is written **only** by the existing audited staff-management handlers in `admin_routes.go`:
    create and update of the staff user, plus a self-rename by the staff member.
  - Every change, **self-rename included**, is audited as `staff.display_name_changed` with
    before and after.
  - There is no other writer.
  - `admin_routes.go` joins G1's Touches, subject to plan §3 Rule 1; the orchestrator sequences it.
- **Out of scope:** the `provider_credential_handlers.go` and `admin_routes.go` tenant-scope writes
  (PLAT-AUDIT-SUBJECT-1).
- **Reuse:** ADR 0102 §4.2 ack and resolve audits reuse `SubjectTenantID`.

## 5. Read path: the tenant projection (HD-PRH2-5)

### 5.1 Endpoint

- **Route:** `GET /v1/admin/audit-log/platform-actions`, guarded by `RequireTenantScope` and
  `RequirePermission(PermAuditRead)`. The existing `GET /v1/admin/audit-log` is unchanged.
- **Session:** `WithTenant(tc.TenantID)`.
- **Query:** `WHERE tenant_id IS NULL AND subject_tenant_id = $1`. The explicit filter is defence
  in depth; RLS is the enforcement.

### 5.2 What the tenant sees

| Field | Shown by default | Source |
|---|---|---|
| `id`, `created_at`, `action`, `target_type`, `target_id`, `outcome` | yes | columns |
| `actor.scope` = `"platform"`, **`actor.staff_id`** | **yes: always shown**, identified | `actor_id` |
| `actor.display_name` | yes when set; otherwise `null` | §5.4 |
| `reason_code` | yes | per-action metadata allowlist |
| `approval_chain` | yes, where recorded | §5.5 |
| `before` / `after` | yes, where recorded | per-action metadata allowlist |
| `ip_address`, `user_agent`, `request_id`, non-allowlisted metadata | **no** | compiled-in default (§5.3) |

- **The allowlist** is `audit.TenantPresentation`, in Go, keyed by `action`. An action without an
  entry projects only the columns: fail closed on disclosure.
- **The underlying record is never altered.**

### 5.3 Presentation setting: resolver-only in PRH-2 (product-owner-proxy; Q7)

- **Resolver:** `audit.ResolvePresentation(ctx, tenantID) (Presentation, error)` returns compiled-in
  defaults:
  - `actor_presentation = identified`;
  - `show_network_metadata = false`;
  - `show_free_form_metadata = false`.
- **The seam is shaped for the deferred work.** The resolver's signature and its callers are what
  AUDIT-PRESENTATION-POLICY-1 will back with a platform-authored, append-only, effective-dated table
  (platform, jurisdiction or tenant scope; platform-admin writes only), with no application
  rewrite.
- **Fail closed:** a resolver error returns the most restrictive presentation (identified, all
  metadata hidden). It never returns a wider one.
- **SA-6: LEGAL/PRIVACY REVIEW REQUIRED** before any future policy enables
  `show_network_metadata`. This is carried into AUDIT-PRESENTATION-POLICY-1.

### 5.4 Actor display name (the orchestrator chose option (a); hygiene per C-104-3/4, SA-4)

- **Lookup (C-104-3).** A second short server-side read:
  - it runs under `WithoutTenant`, which sees platform staff;
  - it selects **only `id, display_name`**;
  - it is restricted to the `actor_id`s present in the **RLS-filtered** tenant result;
  - it **never selects `email`**, and never widens the tenant session.
- **Hygiene (C-104-4):**
  - 1–100 characters, with no control characters (the DB CHECK);
  - written only by the audited path in §4;
  - `staff_id` is always shown next to the name, because names are mutable and are resolved at read
    time;
  - the value is **output-encoded** by the JSON encoder, and is never interpolated into HTML or log
    format strings.
- An unset name gives `display_name: null`.

### 5.5 Approval chain (SA-5, C-104-5)

- The chain is built **only from rows that the tenant session's own RLS returns**:
  - the tenant's own rows;
  - subject rows for that tenant.

  They are linked by `target_id` and allowlisted metadata ids, such as the kill-switch release
  request → approve. **No second query in a wider scope is used for the chain**; only the §5.4 name
  lookup widens, and that returns names only.
- **Pseudonyms.** When a future presentation policy (AUDIT-PRESENTATION-POLICY-1) selects
  `pseudonymous`, each staff id maps to a pseudonym through a **stored random mapping** (never a
  hash of the id). The mapping is applied to the actor **and** every chain entry.
- **Not reachable in PRH-2:** the compiled-in default is `identified`.
- **Fail closed:** if chain assembly or name resolution fails, the entry shows `staff_id` only, or
  the request fails. It never falls back to showing more.

## 6. Invariants

| ID | Invariant |
|---|---|
| AT-1 | A tenant session sees a platform row only when its `subject_tenant_id` equals the session's `app.tenant_id`. Player, platform-admin, platform-service, `app.acting_*` and mixed sessions never do. |
| AT-2 | Only a validated platform principal, as the row's own staff actor, can write `subject_tenant_id`. |
| AT-3 | `subject_tenant_id` and `tenant_id` are never both set. |
| AT-4 | No new write power. The append-only triggers are byte-identical. |
| AT-5 | `subject_tenant_id` comes from the route-validated target, never a body. |
| AT-6 | By default, the tenant presentation never includes IP, user agent, `request_id` or non-allowlisted metadata. The record keeps them. |
| AT-7 | The existing tenant audit readers keep their explicit tenant filters. |
| AT-8 | The name lookup returns only `id, display_name` for ids in the RLS-filtered result, never email. `staff_id` is always shown. |
| AT-9 | The approval chain is built from RLS-filtered rows only. Every failure fails closed. |

## 7. Tests (QA W1 "G1"; T-1 to T-3)

**TI (the headline test).** A platform admin engages, and then releases through four-eyes, a switch
on tenant A, and does the same on tenant B.
- A's `platform-actions` read returns exactly A's rows, with actor id, display name, reason,
  before/after and the chain, and **never B's**. The same holds for B.
- A raw `SELECT` under `WithTenant(A)` never returns B's rows.

**Excluded sessions** (zero subject rows each):
- a player session;
- a mixed platform + tenant session;
- a tenant session with `app.platform_service_id` set;
- a tenant session with `app.acting_*` set (SA-2).

**AZ/RLS write side:**
- a tenant insert of a subject → refused;
- a platform actor ≠ the GUC principal → P0001;
- a tenant staff actor → P0001;
- a `system` actor → P0001;
- a session with `app.acting_*` or `app.platform_service_id` set → P0001;
- both tenant columns set → CHECK violation;
- an unknown subject → FK violation;
- **a suspended platform admin can still write** (Q7: no status check).

**Append-only:** UPDATE, DELETE and TRUNCATE are refused.

**Route target:** a body carrying a tenant id is refused (unknown field), and the success path
records the path target.

**Readers unchanged (R):**
- `GET /v1/admin/audit-log`;
- `kyc/provider.go:672`;
- `noeffect` counts;
- **`casino/rejections.go:257-266` (SA-1)** returns the same result with subject rows present;
- the `TestPaymentsKillSwitchAPI_AuditRecordsCarryBeforeAfterAndTargetTenant` pin still passes.

**Presentation and name:**
- by default, no IP, user agent, `request_id` or non-allowlisted key appears;
- a forced resolver error → the restrictive presentation;
- the name lookup query selects no `email` (an assertion on SQL text or a query log);
- a `display_name` with a control character is refused, and one of 101 characters is refused;
- a rename, **including a self-rename**, produces a `staff.display_name_changed` audit row;
- a name containing `<script>` is returned JSON-escaped.

**Chain:** a chain row outside the tenant's RLS view never appears; an injected chain-assembly
failure fails closed.

**MIG:**
- `up`, then a subject row, then `down` → refuses;
- a non-NULL `display_name`, then `down` → refuses;
- neither present → clean `down`.

**MUT (must be killed):**
- the predicate without `subject_tenant_id = app.tenant_id`;
- no player exclusion;
- **no `app.acting_*` exclusion**;
- the trigger without the actor equality;
- the policy changed to `FOR ALL`;
- the subject taken from `tc` instead of `c.target`;
- the projection including `ip_address`;
- **the name lookup selecting `email`**;
- **the chain assembled from a platform-scope query**.

## 8. KS-AUDIT-TENANT-1 closure path

1. This ADR ACCEPTED by security.
2. G1 merged: 0109, `SubjectTenantID`, the kill-switch writes, the `display_name` write path, the
   resolver, the projection, and the §7 tests passing locally (labelled local).
3. **Security diff review** of the implementation (C-104-6).
4. The orchestrator marks KS-AUDIT-TENANT-1 IMPLEMENTED. PLAT-AUDIT-SUBJECT-1 and
   AUDIT-PRESENTATION-POLICY-1 stay open. SA-3 is tracked in ADR 0099/0112.

## 9. Alternatives rejected

| Alternative | Why rejected |
|---|---|
| (a) An INSERT-into-tenant-scope family | New write power; indistinguishable from the tenant's own actions |
| (b) A dual write | Not atomic, and drifts |
| A JSONB-indexed `target_tenant_id` policy | Untyped, with no FK and no actor guard |
| Pseudonymous by default | Superseded by HD-PRH2-5 |
| Extending the existing endpoint | Changes existing consumers |
| **The full presentation table in G1** | Product-owner-proxy: no consumer yet; deferred as AUDIT-PRESENTATION-POLICY-1 |
| **Email as the display identity** | Discloses contact data (C-104-3) |
| **A snapshot of the name into `audit_log`** | Needs a new audit column. The mutable name is mitigated by always showing `staff_id`, and by renames being audited |
| **A status check in the trigger** | Q7: the audit records what happened |

## 10. Open items

1. **SA-3** (owned by ADR 0099/0112): `app.acting_*` sessions satisfy `dual_scope_isolation`'s
   platform arm. It must be closed there, before K1/K2 code.
2. **AUDIT-PRESENTATION-POLICY-1** (registered): the table and write API, with **SA-6 LEGAL/PRIVACY
   REVIEW REQUIRED** before `show_network_metadata` is ever enabled.
3. **PLAT-AUDIT-SUBJECT-1** (registered): converge the other platform-on-tenant audit writes.
4. **`admin_routes.go` Touches:** the orchestrator sequences G1's `display_name` write-path edit
   under Rule 1.
5. **The platform audit reader** may add `subject_tenant_id` to its output (optional, additive).

## 11. Review disposition (revision 2)

| Finding | Resolution |
|---|---|
| SA-1 / C-104-2 | §1 and §7 readers test include `rejections.go:257-266` |
| SA-2 / C-104-1 | §3 policy and trigger exclude `app.platform_service_id` and `app.acting_*`; tests; mutant |
| SA-3 | Noted as owned by ADR 0099/0112 (§1, §10 item 1) |
| SA-4 / C-104-4 | §3 CHECK; §4 audited write path; §5.4 hygiene; tests |
| SA-5 / C-104-5 | §5.5; AT-9; tests; mutant |
| SA-6 | §5.3; §10 item 2 |
| C-104-3 | §5.4 lookup; AT-8; mutant |
| C-104-6 | §8 step 3; SA-3 tracked; SA-6 flagged |
| Q7 | §3 (no status check); §5.3 defaults identified and network hidden; the resolver fails closed |
| Product-owner-proxy (resolver-only) | §2, §3, §5.3; AUDIT-PRESENTATION-POLICY-1 |
| Orchestrator decision on display name (option (a)) | §3, §4, §5.4. Revision 1's former open item 4 (status) is closed by Q7; former item 1 (display name) is closed by the decision |

**Handover/DoD:**
- **Artefacts:**
  - this ADR ACCEPTED;
  - G1 merged per §8;
  - `docs/architecture/12-audit-reporting-architecture.md` and the security-architecture doc
    updated;
  - HANDOVER row: "tenant-visible platform audit: IMPLEMENTED (kill switch only; others per
    PLAT-AUDIT-SUBJECT-1; presentation compiled-in defaults, configurable table per
    AUDIT-PRESENTATION-POLICY-1)".
- **Registry (orchestrator):** KS-AUDIT-TENANT-1 → IMPLEMENTED after the security diff review.
