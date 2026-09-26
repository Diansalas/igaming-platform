# Stage 10.3 gate W2 — W2a design review (`security`)

- **Reviewed:** ADR 0093 (as of HEAD `bc72fe4` plus the uncommitted W1 "Secret length" / "`KeyImplicit`"
  additions in §4); `05-gate-log.md` gate W0 items 1, 2, 7, 8; my planning review `04-review-security.md`
  (C1, C2, C5–C8, C12); `01-provider-trust-analysis.md` §2; migrations 0011, 0044, 0047, 0086, 0089.
- **Code facts checked:** `internal/db/tenant_rls.go` (`WithTenant`, `WithPlatformAdmin`),
  `internal/config/config.go` (`GuardEnvironment`, `ValidateSecretBackendScheme`, `JWT_SIGNING_SECRET`
  loading), `internal/audit/audit.go`, `migrations/0014` (audit_log dual-scope RLS),
  `internal/httpserver/admin_routes.go` (`canActOnTenant`), `internal/identity` (`GetStaffUserByID`),
  `internal/webhookauth` (`ValidProviderID` = `^[a-z0-9][a-z0-9-]{0,62}$`), `.gitignore`, `.env.example`,
  `.github/workflows/ci.yml`.
- **Type:** design review only. No code, ADR or `deploy/` file was edited. Nothing committed.

## Verdict: **CONCUR WITH THE ADR 0093 BRIDGE, WITH BINDING REFINEMENTS**

The scope bridge is sound, provided it is built exactly as in §1 below. The refinements tighten it; none
weakens an ADR 0093 rule. Three of them change the ADR 0093 §1 table additively and must be recorded by
`architect` in the ADR before W2a merges (§7). No human decision is required.

**Binding:** W2a code must not merge until it meets §1–§6. Every test named here is required, and must
run as the NOBYPASSRLS runtime role unless marked "owner".

---

## 1. The scope bridge

### 1.1 Ruling: sound, with seven conditions

Filing and approval stay in platform scope, so the hardened 0047/0089 principal checks against
`staff_users` work unchanged. The consume step runs inside the tenant-scoped insert and reads the
governance tables through a narrow tenant policy. `staff_users` and the handle table get no new policy,
and nothing uses `SECURITY DEFINER`. That keeps the handle table tenant-only and leaves `staff_users`
exactly as migration 0011 defines it.

Conditions:

| # | Condition | Why |
|---|---|---|
| B1 | The tenant bridge policies are `FOR SELECT` on both governance tables, plus `FOR UPDATE` on requests only. They are never `FOR ALL`, `INSERT` or `DELETE`. | A tenant-scoped connection must never be able to file or approve. |
| B2 | Every bridge policy also requires `app.player_account_id` to be unset. | Same split as the wallet/ledger policies. A player-scoped connection sees nothing. |
| B3 | The consume step selects the request by an **explicit id carried on the handle row** (`activation_request_id`), not by a content search. | This is deterministic and avoids the SEC-S92-5 "oldest matching request" class. It also gives the handle permanent provenance. |
| B4 | A request can move to `applied` only if a handle row exists that points back to it. This is checked in the request's own trigger. | Otherwise any tenant-scoped code, or platform-scope code under the `platform_admin_scope` policy, could burn an approval without creating a handle. |
| B5 | `requested_at`, `decided_at`, `content_hash`, `state`, `created_at` and `status_changed_at` are **forced by triggers**. Client-supplied values are ignored. | A client-supplied future `decided_at` would defeat the 24 h expiry. The precedents only `DEFAULT now()`, which a client can override. |
| B6 | The consume function never reads `staff_users`. | It cannot see platform rows from tenant scope under 0011, and it must not need to. |
| B7 | No function created by the migration is `SECURITY DEFINER`. This is tested by introspection. | ADR 0093 §3 and the 0029/0076 precedent. |

**What the DB checks at consume time, and what it does not.**
- **DB-enforced when the request is filed and when it is decided** (platform scope, verbatim 0047/0089
  shape): the requester and approver each resolve to a staff account, are platform-scoped,
  person-linked and active; they are distinct principals and distinct Persons.
- **DB-enforced at consume** (tenant scope): the request is pending; the target tenant matches; the
  content is equal column by column and by hash; there is at least one `approve` from a principal other
  than the requester, with a matching hash and `decided_at` within 24 h; there is no `reject`; the
  predecessor disposition matches; the request is used once.
- **Not re-checked at consume by the DB: whether the requester and approver are still active.**
  - The precedents (0047/0089 consume functions) do not re-check this either.
  - The 24 h expiry bounds the window.
  - Binding defence in depth in the apply handler: before opening `WithTenant(target)`, it runs one
    `WithPlatformAdmin` transaction that re-resolves the applier, the requester and every approving
    principal with `identity.GetStaffUserByID`. It refuses unless all three are `active`, platform-scoped
    and person-linked.
  - Accepted residual: **Low.** A suspension that commits between that check and the insert is not
    seen.
- **Who applies is authorized by the application, not the DB.** Any tenant-T-scoped connection could
  consume an approved request for T. This does not widen anything: the content applied is exactly what
  two Persons approved, and only its timing is in the applier's hands. The route that applies is
  platform-only (§6).
- **What the bridge exposes.** Tenant-T-scoped code can read T's requests and approvals: `secret_ref`,
  fingerprint, `vendor_account_id`, and the platform principal ids. This is the same exposure as T's
  handle rows. It is accepted.

### 1.2 Table shapes (migration 0096)

Shared column rules. The handle table and the request table both carry these, with identical CHECKs:

- `domain`, `provider_id`, `purpose`, `key_id` and `fingerprint` follow ADR 0093 §1.
- `secret_ref TEXT NOT NULL`. It must satisfy all of the following:
  - `length(secret_ref) <= 512`;
  - `secret_ref !~ '[[:cntrl:][:space:]]'`;
  - `secret_ref ~ '^(awssm|devfile|memory)://'`;
  - the **namespace CHECK** below.
- `vendor_account_id TEXT NULL`. When present: `CHECK (vendor_account_id <> '' AND
  length(vendor_account_id) <= 128 AND vendor_account_id !~ '[[:cntrl:]]')`.
- `not_before TIMESTAMPTZ NOT NULL`, `not_after TIMESTAMPTZ NULL`, with
  `CHECK (not_after IS NULL OR not_after > not_before)`.

**Namespace CHECK (new; defence in depth for C1, alongside the API and resolver checks).**

Let `path` be the ref with its query string and fragment removed:
`split_part(split_part(ref, '#', 1), '?', 1)`.

Then:
- `path` contains `'/provider-creds/'` **exactly once**;
- `tail = substring(path from '/provider-creds/(.*)$')` splits on `/` into exactly four segments:
  - segment 1 equals `tenant_id::text` (`target_tenant_id` on the request table);
  - segment 2 equals `domain`;
  - segment 3 equals `provider_id`;
  - segment 4 matches `^[A-Za-z0-9_-][A-Za-z0-9._-]{0,127}$`. This refuses `.` and `..`.

Write this as an `IMMUTABLE` SQL function, `provider_credential_ref_in_namespace(ref, tenant, domain,
provider) RETURNS boolean`, used by both tables' CHECKs. Split on segments. **Do not** build a regex
from column values.

**Pinned-version CHECK:**
- `awssm` refs: `CHECK (secret_ref !~ '^awssm://' OR secret_ref ~ '\?versionId=[A-Za-z0-9-]{32,64}(#[A-Za-z0-9._-]{1,64})?$')`.
  This is required in every environment. It is stricter than the ADR's "staging and production" rule,
  and it also refuses stage labels.
- `devfile` refs: `?version=[A-Za-z0-9._-]{1,64}` is required (see §4).
- `memory` refs: unconstrained.

#### `provider_credential_handles` (ADR 0093 §1 plus one column)

All ADR 0093 §1 columns and constraints, plus:

| Column | Rule |
|---|---|
| `activation_request_id` | `UUID NOT NULL UNIQUE REFERENCES provider_credential_change_requests(id)`. Immutable. **New: records the approval that activated this key (B3).** |
| `revoke_reason` | `CHECK (revoke_reason IS NULL OR revoke_reason IN (<revoke enum, §1.5>))`. `CHECK ((status = 'revoked') = (revoked_at IS NOT NULL AND revoked_by IS NOT NULL AND revoke_reason IS NOT NULL))`. |
| `created_by` | `UUID NOT NULL`. |

Other rules:
- `tenant_id REFERENCES tenants(id)`, with **no** `ON DELETE CASCADE`. The history is evidence, so a
  tenant that has handles cannot be deleted.
- RLS: `ENABLE` and `FORCE`. There are three policies, `tenant_isolation_read`,
  `tenant_isolation_insert` and `tenant_isolation_update`. Each uses the predicate
  `tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid AND
  NULLIF(current_setting('app.player_account_id', true), '') IS NULL`. There is no DELETE policy and no
  platform policy.
- Runtime grants are as in ADR 0093. `activation_request_id` is **not** in the `UPDATE` column grant.

#### `provider_credential_change_requests` (platform-scoped, bridged)

| Column | Rule |
|---|---|
| `id` | UUID PK. |
| `target_tenant_id` | `UUID NOT NULL REFERENCES tenants(id)`. |
| `domain`, `provider_id`, `purpose`, `key_id`, `secret_ref`, `fingerprint`, `vendor_account_id`, `not_before`, `not_after` | The shared rules above. `fingerprint` is **computed by the server** at filing (§3). |
| `predecessor_handle_id` | `UUID NULL REFERENCES provider_credential_handles(id)`. |
| `predecessor_disposition` | `TEXT NOT NULL CHECK IN ('none','verify_only','revoked')`. |
| `predecessor_not_after` | `TIMESTAMPTZ NULL`. |
| `content_hash` | `TEXT NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$')`. Set by the trigger. |
| `reason_code` | `TEXT NOT NULL CHECK IN ('initial_registration','scheduled_rotation','compromise_replacement','vendor_migration')`. |
| `requested_by_principal_id` | `UUID NOT NULL`. |
| `requested_at` | `TIMESTAMPTZ NOT NULL`. Forced to `now()`. |
| `state` | `TEXT NOT NULL CHECK IN ('pending','applied')`. Forced to `'pending'` on insert. |
| `applied_at`, `applied_by_principal_id` | Nullable. |
| `applied_handle_id` | `UUID NULL REFERENCES provider_credential_handles(id)`. |

Table-level CHECKs:
- `CHECK ((predecessor_disposition = 'none') = (predecessor_handle_id IS NULL))`
- `CHECK ((predecessor_disposition = 'verify_only') = (predecessor_not_after IS NOT NULL))`
- `CHECK (purpose = 'webhook_verify' OR predecessor_disposition <> 'verify_only')`. An outbound
  predecessor is revoked, never overlapped.
- `CHECK ((state = 'applied') = (applied_at IS NOT NULL AND applied_by_principal_id IS NOT NULL AND
  applied_handle_id IS NOT NULL))`

The foreign keys between the two tables are circular. Create both tables, then `ALTER TABLE … ADD
CONSTRAINT`. Referential-integrity checks bypass RLS, which is intended here.

#### `provider_credential_change_approvals` (platform-scoped, bridged read-only)

| Column | Rule |
|---|---|
| `id` | UUID PK. |
| `request_id` | `UUID NOT NULL REFERENCES provider_credential_change_requests(id)`. |
| `approver_principal_id` | `UUID NOT NULL`. |
| `decision` | `CHECK IN ('approve','reject')`. |
| `content_hash` | `TEXT NOT NULL`. The hash the approver saw and echoed back. |
| `reason_code` | Required when `decision = 'reject'` (0044 rule). |
| `decided_at` | Forced to `now()`. |
| constraint | `UNIQUE (request_id, approver_principal_id)`. |

#### Content hash

It is computed only in the DB, by one `STABLE` SQL function:
`provider_credential_content_hash(tenant, domain, provider_id, purpose, key_id, secret_ref,
fingerprint, vendor_account_id, not_before, not_after, predecessor_handle_id, predecessor_disposition,
predecessor_not_after) RETURNS text`.

- The input is the label `'pcr-v1|'` followed by each field **length-prefixed**:
  `length(x)::text || ':' || x`, with NULL encoded as `'-'`.
- Timestamps are rendered as `to_char(ts AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')`. This
  makes the hash independent of the session `TimeZone`.
- UUIDs use their canonical text form.
- The result is `encode(sha256(convert_to(input, 'UTF8')), 'hex')`.
- Go never computes it. The API returns it to the approver, and the approver echoes it back.

### 1.3 Policies

For both governance tables:
- `platform_admin_scope FOR ALL` uses the **verbatim** 0044/0086 predicate: platform GUC set, tenant
  and player GUCs unset.

Bridge policies:

```sql
CREATE POLICY tenant_consume_read ON provider_credential_change_requests
  FOR SELECT USING (
    target_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL);

CREATE POLICY tenant_consume_apply ON provider_credential_change_requests
  FOR UPDATE
  USING (state = 'pending'
    AND target_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL)
  WITH CHECK (state = 'applied'
    AND target_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL);

CREATE POLICY tenant_consume_read ON provider_credential_change_approvals
  FOR SELECT USING (
    NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    AND EXISTS (SELECT 1 FROM provider_credential_change_requests r
                 WHERE r.id = request_id
                   AND r.target_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid));
```

- There is no recursion: the requests policies never reference approvals.
- `staff_users` and `provider_credential_handles` receive **no** new or changed policy. A test compares
  `pg_policies` for `staff_users` against 0011's exact text.

### 1.4 Triggers

**Requests.**
- `BEFORE INSERT`:
  - the verbatim 0089 `require_platform_principal` body: resolve the principal; `NOT FOUND` raises;
    it must be platform-scoped, person-linked and `active`;
  - then force `requested_at := now()`, `state := 'pending'` and every `applied_*` field to NULL;
  - then set `content_hash := provider_credential_content_hash(NEW…)`.
- `BEFORE UPDATE`: the 0044 immutability body, extended to every content column, `target_tenant_id`,
  every `predecessor_*` field, `content_hash`, `reason_code`, the requester and `requested_at`. Only
  `pending→applied` is allowed. When moving to `applied`:
  - force `applied_at := now()`;
  - require `EXISTS (SELECT 1 FROM provider_credential_handles h WHERE h.id = NEW.applied_handle_id
    AND h.activation_request_id = NEW.id AND h.tenant_id = NEW.target_tenant_id)`, which is B4. In
    platform scope the handle is invisible, so this is refused there.
- `BEFORE DELETE` and `BEFORE TRUNCATE` refuse.

**Approvals.**
- `BEFORE INSERT`:
  - the verbatim 0089 `deny_self_approval` body: distinct principal; the approver resolved, platform,
    person-linked and active; the requester person-linked; distinct Person;
  - plus: the request's `state = 'pending'`, else raise;
  - plus: `NEW.content_hash = request.content_hash`, else raise;
  - plus: force `decided_at := now()`.
- `BEFORE UPDATE OR DELETE`: `ledger_deny_mutation`. `BEFORE TRUNCATE`: refuse.

**Handles, `BEFORE INSERT`.**
- `NEW.status = 'active'`, else raise. **No row is ever inserted as `verify_only` or `revoked`.**
- `NEW.activation_request_id IS NOT NULL`, and `NEW.created_by IS NOT NULL`.
- Force `created_at := now()` and `status_changed_at := now()`.
- `revoked_*` must be NULL.
- `NEW.not_after IS NULL OR NEW.not_after > now()`.

**Handles, `AFTER INSERT` (the consume step).**
- It calls `provider_credential_consume(NEW)`. Handle inserts are plain `INSERT`, and `ON CONFLICT` is
  forbidden. `AFTER` is used for the 0047 reason and because the new row must be visible to B4.
- In order, raising on any failure:
  1. `SELECT … FROM provider_credential_change_requests WHERE id = NEW.activation_request_id FOR
     UPDATE`. `NOT FOUND` raises. This also covers another tenant's request, which is invisible.
  2. `state = 'pending'`, and `target_tenant_id = NEW.tenant_id`.
  3. Each of `domain`, `provider_id`, `purpose`, `key_id`, `secret_ref`, `fingerprint`,
     `vendor_account_id`, `not_before` and `not_after` is `IS NOT DISTINCT FROM` the request.
  4. `provider_credential_content_hash(NEW columns + the request's predecessor fields) =
     r.content_hash`.
  5. `EXISTS` an approval with:
     - `decision = 'approve'`;
     - `approver_principal_id <> r.requested_by_principal_id`;
     - `content_hash = r.content_hash`;
     - `decided_at <= now()`;
     - `decided_at > now() - interval '24 hours'`.

     The 24 hours are a platform constant, and changing them needs an ADR amendment.
  6. `NOT EXISTS` an approval with `decision = 'reject'`.
  7. The predecessor:
     - `'none'`: no other `active` row exists for the same (tenant, domain, provider, purpose). The
       partial unique index also enforces this.
     - `'verify_only'`: the row `predecessor_handle_id` has the same tenant, domain, provider and
       purpose, `status = 'verify_only'`, `not_after = r.predecessor_not_after` and
       `status_changed_at = now()`, meaning it was demoted in this transaction.
     - `'revoked'`: that row has `status = 'revoked'`, whenever it was revoked.
  8. `UPDATE provider_credential_change_requests SET state = 'applied', applied_handle_id = NEW.id,
     applied_by_principal_id = NEW.created_by WHERE id = r.id`.
- Every `RAISE` uses a dedicated `ERRCODE` (`PC001`–`PC0nn`), so the application can classify the
  error for logs without parsing messages.

**The apply transaction.** It is one `WithTenant(target)` transaction, in this order:
1. the predecessor transition (a single-actor transition, §1.5, with reason `rotation`);
2. the handle `INSERT` carrying `activation_request_id`;
3. one audit row per changed handle row.

### 1.5 Handle transition trigger (single actor)

This is `BEFORE UPDATE`, together with `BEFORE DELETE` and `BEFORE TRUNCATE`, which refuse.

- If `OLD.status = 'revoked'`, any update is refused, because `revoked` is terminal.
- Every column **except** `status`, `status_changed_at`, `not_after` and `revoked_*` is compared
  `IS DISTINCT FROM OLD`. Any difference is refused. This includes `activation_request_id`, `secret_ref`,
  `fingerprint` and `tenant_id`.
- Allowed status pairs:

  | From | To | Rule |
  |---|---|---|
  | `active` | `active` | `not_after` shrink only |
  | `verify_only` | `verify_only` | `not_after` shrink only |
  | `active` | `verify_only` | `purpose = 'webhook_verify'` only; `NEW.not_after` must be non-NULL |
  | `active` | `revoked` | — |
  | `verify_only` | `revoked` | — |

  Every other pair is refused. **No transition ever leads to `active`.**
- On a status change, force `NEW.status_changed_at := now()`. Otherwise force it to equal
  `OLD.status_changed_at`.
- `not_after`: if it differs from the old value, require all of:
  - `NEW.not_after IS NOT NULL`;
  - `OLD.not_after IS NULL OR NEW.not_after <= OLD.not_after`;
  - `NEW.not_after >= now()`. To stop a key immediately, revoke it.
- The cap: `NEW.status = 'verify_only'` implies `NEW.not_after <= NEW.status_changed_at + interval '7 days'`.
- To `revoked`: force `revoked_at := now()`; require `revoked_by` to be non-NULL and `revoke_reason` to
  be in the enum. For any other status, `revoked_*` must stay NULL.

**Reason codes.** These are closed enums in Go, and also a DB CHECK where a column exists.

| Action | Codes |
|---|---|
| `verify_only` | `rotation` |
| `not_after` shrink | `overlap_shortened`, `suspected_compromise`, `vendor_instruction` |
| `revoke` (the `revoke_reason` column) | `rotation_complete`, `suspected_compromise`, `confirmed_compromise`, `vendor_offboarded`, `misregistration`, `tenant_request` |

The DB enforces the reason only for `revoke`, because only `revoke` has a column (ADR 0093's grant
list). For `verify_only` and shrink, the reason is required by the single service function
`providercred.TransitionHandle(ctx, tx, handleID, action, notAfter, reason, actor)`. That function is the
only UPDATE path, and it writes the audit row in the same transaction. **Accepted residual: Low.** Adding
`status_change_reason` and `status_changed_by` columns is an optional later tightening; it is not
required.

### 1.6 Tests (DB; runtime role unless marked "owner")

**RLS, bridge and structure.**
- `TestPCHandles_RLS_TenantIsolation`: `WithTenant(A)` sees 0 of B's rows; `WithoutTenant` and
  `WithPlayerScope` see 0; an insert with `tenant_id = B` under A fails.
- `TestPCHandles_NoPlatformOrDualScopePolicy`
- `TestPCHandles_SchemaHasNoSecretColumns`
- `TestPCGovernance_BridgePoliciesSelectUpdateOnly`: introspects `pg_policies`.
- `TestPCGovernance_StaffUsersPoliciesUnchanged`
- `TestPCGovernance_NoSecurityDefiner`: `pg_proc.prosecdef` is false for every function in the migration.
- `TestPCGovernance_TenantScopeCannotInsertRequestOrApproval`
- `TestPCGovernance_PlayerScopeSeesNothing`
- `TestPCGovernance_TenantBCannotSeeOrConsumeTenantARequest`
- `TestPCGovernance_PlatformScopeCannotMarkApplied`
- `TestPCGovernance_TenantScopeCannotMarkAppliedWithoutHandle`

**Filing and approval.**
- `TestPCRequest_RequesterEligibility`, one subtest each: unresolvable, tenant-scoped, unlinked,
  suspended.
- `TestPCRequest_ServerFieldsForced`: the client's `requested_at`, `state` and `content_hash` are
  ignored.
- `TestPCRequest_ImmutableAfterInsert`: each column, table-driven; DELETE and TRUNCATE refused.
- `TestPCApproval_SelfApprovalRefused`
- `TestPCApproval_SamePersonSecondAccountRefused`
- `TestPCApproval_ApproverEligibility`: unresolvable, tenant-scoped, unlinked, suspended.
- `TestPCApproval_RequesterUnlinkedRefused`
- `TestPCApproval_ContentHashMismatchRefused`
- `TestPCApproval_NotPendingRefused`
- `TestPCApproval_DuplicateApproverRefused`
- `TestPCApproval_DecidedAtClientValueIgnored`: a future `decided_at` is stored as `now()`.
- `TestPCApproval_Immutable`: UPDATE, DELETE and TRUNCATE refused.

**Consume.**
- `TestPCConsume_ApprovedInsertSucceedsAndMarksApplied`
- `TestPCConsume_NoApprovalRefused`
- `TestPCConsume_AnyRejectBlocks`
- `TestPCConsume_ExpiryBoundary`: backdate `decided_at` through the owner connection with the trigger
  disabled; 23h59m passes and 24h00m01s is refused.
- `TestPCConsume_SingleUse`: a sequential second insert is refused.
- `TestPCConsume_ConcurrentApplyExactlyOneWins`
- `TestPCConsume_ContentBinding`: one subtest per bound column, including NULL↔value for
  `vendor_account_id` and `not_after`, and the tenant.
- `TestPCConsume_PredecessorDisposition`: `verify_only` with the wrong `not_after`; predecessor not
  demoted in this transaction; `revoked` passes; `none` with an existing active row is refused.
- `TestPCConsume_HashIndependentOfSessionTimeZone`

**Handle rules.**
- `TestPCHandles_InsertRequiresActivationRequest`
- `TestPCHandles_InsertNonActiveRefused`
- `TestPCHandleTransition_*` (T7):
  - backward transitions refused;
  - any transition to `active` refused;
  - anything out of `revoked` refused;
  - `verify_only` on `outbound_api` refused;
  - 7-day cap: exactly 7d passes, 7d+1s is refused;
  - `not_after` extend, clear or set in the past refused, shrink passes;
  - each immutable column refused;
  - DELETE and TRUNCATE refused;
  - revoke without a reason, or with a reason outside the enum, refused;
  - a client-supplied `status_changed_at` is overridden.
- `TestPCHandles_RevokedFingerprintCannotBeReRegistered` and
  `TestPCHandles_SameFingerprintSecondTenantRefused` (T6).
- `TestPCRef_NamespaceCheck`, on both tables: foreign tenant, domain or provider; two `/provider-creds/`
  segments; `.` and `..` names; a namespace smuggled in the query or fragment; `awssm` without
  `versionId` or with `AWSCURRENT`.
- `TestPCMigration_DownRefusesWithRows`

**Mutation kills (`qa`).** Each of these mutations must make a named test fail:
- dropping the expiry predicate;
- dropping the Person check;
- dropping the reject check;
- widening `tenant_consume_apply` to `FOR ALL`;
- dropping the player-unset clause;
- dropping B4's `EXISTS`;
- removing the `decided_at` forcing.

---

## 2. Outbound credential caching — interpretation **CONFIRMED, with four precisions**

The gate W0 item 8 wording is correct: "handle row read on every call; nothing cached in the adapter,
the Authenticator, the HTTP client or the SDK session; the resolver's secret-by-pinned-version cache is
allowed."

Precisions (binding):

1. **Where the read happens.** The row is read in a transaction that ends **before** the network call.
   An outbound HTTP call is never made while a DB transaction is held open. The in-flight exposure is
   one call: a revoke that commits between the read and the send.
2. **Retries.** Transport-level retries inside one call's timeout budget may reuse the credential.
   Anything scheduled later (a job, a worker, or a retry after a backoff of more than 1 s) re-reads the
   handle row.
3. **Derived credentials count as credentials.** An OAuth or bearer token, a session cookie or a signed
   session obtained with the handle's secret may be cached only under all of these rules:
   - the cache is keyed by (tenant_id, handle id, fingerprint);
   - it is served only after this call's handle read returned that same handle `active` and within its
     window;
   - its TTL does not exceed the vendor's own expiry.

   Otherwise it is forbidden. This makes revocation immediate for derived tokens too.
4. **Connection reuse is allowed for bearer-style auth.** Keep-alive TLS connections carry no
   credential. **mTLS client certificates are excluded.** A client certificate is bound to the
   connection, so an mTLS purpose would need per-handle transports that are closed on revocation. That
   is `PROVIDER DEPENDENT` and out of 10.3 scope.

Tests:
- `TestOutbound_RevokeThenNextCallFailsClosed`: no request reaches the fake vendor.
- `TestOutbound_TenantACallNeverCarriesBCredential` (T11, request capture).
- `TestOutbound_RotationRevokesOldAtomically`
- `TestOutbound_NoCredentialOnLongLivedTypes`: a reflection or AST check that adapter structs, the HTTP
  client and the orchestrator hold no `OutboundCredential`, `Authenticator` or `[]byte` secret field.
- `TestOutbound_APIKeyEnvVarRemoved`: an AST check that no code references `APIKeyEnvVar` and that
  `internal/providers` makes no `os.Getenv`.
- `TestOutbound_NoTxHeldAcrossHTTPCall`: the fake vendor handler asserts that the pool has no
  transaction open from this call.

---

## 3. Fingerprint HMAC key delivery (local)

**Ruling.** The key comes from the **existing environment-variable config path**, the same one that
loads `JWT_SIGNING_SECRET`: `internal/config.Load`.

- New setting: `PROVIDER_CREDENTIAL_FINGERPRINT_KEY` becomes the field
  `Config.ProviderCredentialFingerprintKey`.
- In development it comes from `.env.example` with a clearly dev-only value. In CI it comes from the
  `ci.yml` job `env`. Unit tests set the `Config` field directly.
- In AWS it would be an ECS `secrets` entry, like `JWT_SIGNING_SECRET`. That is DEPLOY-FPKEY-1, which is
  out of 10.3.

Validation in `Load()`:
- It must be at least 32 bytes.
- It must not equal `JWT_SIGNING_SECRET` or `JWT_PREVIOUS_SECRET`.
- When `GuardEnvironment() != "development"`, it must not equal the `.env.example` or CI literal.

**If the key is absent,** the real credential subsystem is **not constructed**:
- the real resolver is nil, so real-provider callbacks return 401 `no_resolver`;
- outbound calls through non-synthetic adapters fail closed;
- the credential-request, approve and apply routes are not mounted.

Absence does **not** refuse startup. The MOCK paths and the staging topology are unaffected. This fails
closed without coupling 10.3 to DEPLOY-FPKEY-1.

Other rules:
- The value is never logged. It must be redacted wherever `Config` is logged or printed, the same as
  `JWT_SIGNING_SECRET`.
- **The key must be stable per environment.** Changing it makes every stored `fp1:` fingerprint
  mismatch. Every resolve then fails closed with `credential_integrity`, which is the safe direction.
  Rotating the key needs `fp2:` plus an ADR amendment. Record this in the ADR 0093 §2 note.
- **The operator's confirmation value** is `sha256:<hex>`, an unkeyed hash of the secret bytes computed
  by the operator at provisioning.
  - The server fetches the secret by ref, compares the unkeyed hash in constant time, and then computes
    the `fp1:` fingerprint itself.
  - The confirmation value is never persisted, never logged, never audited and never echoed.
  - Request-body logging stays off for these routes.
- The HMAC input is exactly `label || 0x00 || secret`. It contains no tenant id, per ADR 0093 §2.

Tests:
- `TestFingerprint_KnownAnswerVector`
- `TestFingerprint_InputHasNoTenant`
- `TestConfig_FingerprintKey_TooShortRefused`
- `TestConfig_FingerprintKey_EqualsJWTSecretRefused`
- `TestConfig_FingerprintKey_DevLiteralRefusedOutsideDevelopment`
- `TestWiring_NoFingerprintKey_RealCredentialSubsystemAbsent`
- `TestConfig_FingerprintKeyRedacted`
- `TestRegistration_ConfirmationValueNeverPersistedOrLogged`

---

## 4. Memory and dev-file backends

### 4.1 Allow-list: consistent with W1b's `ValidateSecretBackendScheme`, unchanged

`ValidateSecretBackendScheme` stays the single rule, and W2a does not change it:

| Scheme | Rule |
|---|---|
| `awssm` | Allowed everywhere. |
| `devfile` | Only when `GuardEnvironment() == "development"`. An absent `APP_ENV` counts as production. |
| `memory` | Always refused. |

W2a must call it at three points:
1. **Store construction at startup.** The store router is built only from backends that pass. A
   configured backend that fails refuses startup.
2. **Registration.** The ref's scheme is parsed and validated. A failure is the generic 409 (§6).
3. **Every resolve.** The row's scheme must have a backend in the router. Otherwise the resolve fails
   closed with `credential_unavailable`. A `memory://` row in a non-test process therefore never
   resolves.

`memory` is constructed only from test code:
- The constructor lives in its own package, `internal/secretstore/memstore`.
- A CI check fails if any non-`_test.go` file imports it, in the same form as the existing "only test
  support reads X" checks.

`awssm` in development is allowed by W1b's rule. **Recommendation to W3b (not W2a):** apply the
static-credential refusal (C12.4) in every `GuardEnvironment`, not only staging and production. That
makes `awssm` unusable with a personal AWS profile locally, which matches CLAUDE.md's environment
rules.

### 4.2 `devfile://` rules

- **Constructor.** `devfile.New(cfg config.Config, root string)` itself calls
  `cfg.ValidateSecretBackendScheme("devfile")`, as defence in depth. It takes the `Config` value, never
  a bool.
- **Root.** It comes from `SECRETSTORE_DEVFILE_ROOT`, default `./.secrets/dev`. W2a adds `.secrets/` to
  **both** `.gitignore` and `.dockerignore`.
- **Ref and file mapping.** `devfile://provider-creds/<tenant>/<domain>/<provider>/<name>?version=<v>`
  maps to `<root>/provider-creds/<tenant>/<domain>/<provider>/<name>.v<v>`.
- **Opening files.** Files are opened only through `os.OpenRoot(root)` (Go 1.25), so traversal and
  symlink escape fail.
- **Permission checks.** These are Unix-only; the backend refuses to construct on other operating
  systems.
  - At construction, the root must be a directory owned by the process euid with
    `mode & 0o077 == 0`. Otherwise startup is refused.
  - On **every** fetch, the file is checked with `Lstat` through the root. It must be a regular file,
    not a symlink, owned by the euid, with `mode & 0o077 == 0` (0600 or 0400) and a size of 1 B to
    64 KiB.
  - A failure is a closed error class, `store_config`. It maps to reason
    `credential_store_unavailable`, does not count toward the circuit breaker (§5) and is not
    negative-cached for longer than 30 s.
  - The owner check uses an injectable `stat` for testing.
- **Content.** The secret is the exact file bytes, with no trimming. The fingerprint check catches a
  file that was edited in place.
- **Never in staging or production.** This is guaranteed by `GuardEnvironment` in three places: the
  constructor, the router build and registration.

Tests:
- `TestSecretStore_MemstoreImportedOnlyByTests` (CI)
- `TestStoreRouter_OnlyAllowListedSchemes`
- `TestResolver_MemoryRefRowFailsClosedOutsideTests`
- `TestDevFile_RefusedUnlessExplicitDevelopment`: absent, staging and production.
- `TestDevFile_RootPermissions`
- `TestDevFile_FilePermissions`: group-readable, world-readable, wrong owner, symlink, directory,
  empty, oversize.
- `TestDevFile_TraversalRefused`
- `TestDevFile_RootIgnoredByGitAndDocker`

---

## 5. Circuit breaker and fail-closed behaviour (C7)

All values are platform constants in `internal/secretstore`. Changing them needs a `security` review.

| Parameter | Value |
|---|---|
| Store call timeout | **2 s** total, including SDK retries. At most 1 retry, with backoff capped so the total stays within 2 s. The call runs under `singleflight` per (tenant, ref, fingerprint), with a context detached from the first caller's cancellation but bounded by 2 s. |
| Concurrent store calls per process | **4** (a semaphore). A caller waits at most **250 ms** for a slot, then fails fast. This bounds how many pooled DB connections can be held waiting on the store. **Superseded (F-POOL-1, `15-ci-342-security-ruling.md` §2; ADR 0094):** that bound did not hold, because callers waiting for a slot or a flight held their transaction. After ADR 0094 no pooled connection is held across any store call or wait (INV-POOL). The 4-slot bound now protects only the store and the process. It is complemented by per-(backend, tenant) caps: P = 2 per tenant, D = 2 for degraded tenants together. |
| Breaker scope | One breaker per backend instance, per process. **Amended by ADR 0094 §4.2:** one breaker per (backend, tenant), with the same thresholds, so one tenant's outage cannot open another tenant's breaker. |
| Failures that count | Timeout or deadline, network or transport errors, 5xx, throttling. |
| Failures that do not count | Not found, access denied, invalid version, `store_config`, fingerprint mismatch. These are per-ref problems and use the negative cache instead. |
| Trip threshold | **3 consecutive** counting failures. |
| Cooldown (open) | **15 s**, doubling on each failed probe, capped at **60 s**. It resets to 15 s after a successful probe. |
| Half-open | Exactly **1** probe call. Every other caller fails fast while the probe is in flight. |
| Per-ref negative cache | **5 s** after a counting failure; **30 s** after a non-counting per-ref error. A fingerprint mismatch is negative-cached for 30 s, and its P1 alert is rate-limited to 1 per ref per 5 min. |
| Positive cache | Keyed on (tenant_id, secret_ref, fingerprint). Bounded LRU of **1024** entries. TTL **10 min**, after which the entry is **refreshed, not evicted**. **Max-stale 60 min**. The fingerprint is compared on every hit. |

**Fail-closed behaviour:**
- When the breaker is open, or a slot wait times out, a cached entry within max-stale is served (its
  fingerprint is still compared). Otherwise:
  - inbound: uniform 401, log reason `credential_store_unavailable`;
  - outbound: the operation fails with a retryable internal error, and **no request is sent to the
    vendor**. There is never a fallback to another credential or to an unauthenticated call.
- A mismatch between a fetched or cached fingerprint and the row gives `credential_integrity`, a P1
  alert, and the value is never served. This applies whether the entry is stale or not.
- A handle-row DB error gives the uniform 401. The log reason must not depend on whether a handle
  exists (C5).
- The breaker never lets a request through without the per-call handle read. Revocation stays
  immediate in every breaker state.

Tests:
- `TestStoreBreaker_OpensAfterThreeConsecutiveFailures`
- `TestStoreBreaker_FailsFastWhileOpen`: under 5 ms, zero store calls.
- `TestStoreBreaker_HalfOpenSingleProbe`
- `TestStoreBreaker_CooldownBackoffCapped`
- `TestStoreBreaker_PerRefErrorsDoNotTrip`
- `TestStoreCache_ServesStaleWithinMaxStale`
- `TestStoreCache_NoStaleBeyondMaxStale`
- `TestStoreCache_RefreshNotEvict`
- `TestStoreCache_FingerprintComparedOnHit` (T12)
- `TestStoreCache_NoCrossTenantHit`
- `TestStoreOutage_DoesNotPinPool`:
  - setup: a blocking fake store and 50 concurrent callbacks over 8 refs;
  - at most 4 store calls are in flight;
  - at most 4 connections are held for more than 250 ms;
  - an unrelated tenant query completes in under 500 ms.
  - ADR 0094 adds two stricter criteria: no pre-verification transaction is held for more than
    250 ms (400 ms measurement slack), and no store call runs with a transaction held.
  - ADR 0094 also adds a production-pool-size variant,
    `TestStoreOutage_DoesNotPinPool_ProductionPoolSize`, at 10 connections.
- `TestResolver_UnknownKeyIDNoStoreCall`
- `TestResolver_RevokeImmediateWhileBreakerOpen`

---

## 6. Admin handle API

**Routes.** The target tenant always comes from the path, through `canActOnTenant` (the ADR 0011
precedent). A tenant-scoped caller can only name its own tenant. The body never carries a tenant, and
`DisallowUnknownFields` is on.

| Route | Permission | Scope used |
|---|---|---|
| `GET /v1/admin/tenants/{tenantID}/provider-credentials` | `provider_credential:read` | `WithTenant(target)` |
| `POST /v1/admin/tenants/{tenantID}/provider-credential-requests` | `provider_credential:request` | `WithPlatformAdmin` |
| `GET /v1/admin/tenants/{tenantID}/provider-credential-requests[/{id}]` | `provider_credential:request` **or** `:approve` | `WithPlatformAdmin` |
| `POST …/provider-credential-requests/{id}/decisions` | `provider_credential:approve` | `WithPlatformAdmin` |
| `POST …/provider-credential-requests/{id}/apply` | `provider_credential:request` | recheck with `WithPlatformAdmin` (§1.1), then `WithTenant(target)` |
| `POST /v1/admin/tenants/{tenantID}/provider-credentials/{handleID}/transitions` | `provider_credential:revoke` | `WithTenant(target)` |

Notes on the routes:
- A decision carries the `content_hash` the approver saw.
- The apply step is a separate call, not a side effect of approval. Filing and approval are
  platform-scope transactions; the insert is a tenant-scope transaction.
- A transition body is `action ∈ {verify_only, shorten, revoke}`, `not_after`, `reason_code`.
- There is deliberately no cancel route (0089 precedent). A stale request becomes unusable when its
  approvals expire.

**Permissions** follow ADR 0093 exactly: `provider_credential:read`, `:request`, `:approve`, `:revoke`.

| Role | Permissions |
|---|---|
| `RolePlatformAdmin` | all four |
| `RoleTenantAdmin` | `read` and `revoke` |
| every other role | none |

None of them is implied by `PermTenantWrite`, `PermCasinoConfigWrite` or `PermProviderConfigWrite`.
Holding `:approve` never bypasses the DB's check for a distinct Person.

**Audit.** Use `audit.Record` in the same transaction as the write.
- Scope: the file and decision rows are platform-scope, so `tenant_id` is NULL and
  `metadata.target_tenant_id` is set. The apply and transition rows are `tenant_id = target`.
- Actions:
  - `provider_credential.request_filed`
  - `provider_credential.request_decided`
  - `provider_credential.activated`
  - `provider_credential.predecessor_transitioned`
  - `provider_credential.transitioned`
  - plus `failure` and `denied` outcomes for rejected attempts.
- Fields: actor, IP, user agent, `request_id`, `target_type = 'provider_credential_handle'` or
  `'provider_credential_request'`, `target_id`.
- Metadata:
  - `target_tenant_id`, `domain`, `provider_id`, `purpose`, `key_id`;
  - `handle_id`, `change_request_id`, `content_hash`, `fingerprint`, `secret_ref`;
  - `status_before` / `status_after`, `not_before`, `not_after_before` / `not_after_after`;
  - `reason_code`, `decision`, and the requester and approver principal ids.
- **Never recorded:** the secret value, the confirmation value, store error text or any SDK output.
- A failure row carries the generic class only (`registration_rejected`, `activation_rejected`,
  `transition_rejected`). The specific class is written to the application log only.

**Error mapping.** Trigger `RAISE` text is never returned. Errors are classified by `ERRCODE`.

| Situation | Response |
|---|---|
| Unauthenticated | 401 |
| Missing permission, or a tenant caller naming another tenant | 403, no data, no row changed |
| Handle or request not visible in scope | 404 |
| Syntactic body errors only (malformed JSON, unknown field, missing field, value outside an enum, `key_id` charset) | 400 `invalid_request` |
| **Registration, semantic:** ref not found, access denied, confirmation mismatch, namespace mismatch, backend not allowed, missing `versionId`, global fingerprint duplicate, duplicate `key_id`, store unavailable | one **byte-identical** 409 `credential_registration_rejected` |
| Decision: self-approval, same Person, ineligible principal, hash mismatch, not pending | 409 `approval_rejected` |
| Apply: expired, unapproved, rejected, content, hash or predecessor mismatch, principal recheck failed, fingerprint recheck failed, unique violation | 409 `credential_activation_rejected` |
| Transition: illegal pair, extension, cap exceeded, revoked row | 409 `credential_transition_rejected` |
| Transition: missing or unknown reason | 400 `invalid_request` |
| Any unexpected DB error | 500 with no detail |

**No secret material in any response.**
- Response DTOs are dedicated structs with an allow-list of fields: `id`, `domain`, `provider_id`,
  `purpose`, `key_id`, `status`, `not_before`, `not_after`, `status_changed_at`, `fingerprint`,
  `secret_ref`, `vendor_account_id`, `activation_request_id`, and for revoked rows `revoked_*`.
- Store results never flow into a DTO.
- Every secret-bearing type carries C15 redaction: `String`, `GoString`, `LogValue` and `MarshalJSON`.
- OpenAPI examples are synthetic.

API tests:
- `TestProviderCredentialAPI_PermissionMatrix`: every route × {platform_admin, tenant_admin, support,
  compliance, finance, risk_manager}.
- `TestProviderCredentialAPI_TenantAdminCannotTargetOtherTenant` (T1): no row changes in either tenant.
- `TestProviderCredentialAPI_TenantAdminCannotRequestApproveApply`
- `TestProviderCredentialAPI_BodyTenantFieldRejected`
- `TestProviderCredentialAPI_RegistrationErrorsUniform`: every semantic class gives an identical body
  apart from the request id, and the log carries the class.
- `TestProviderCredentialAPI_ApplyRechecksPrincipals`: the approver is suspended after approving, and
  the apply gives 409.
- `TestProviderCredentialAPI_RevokeSingleActorTenantAndPlatform`
- `TestProviderCredentialAPI_AuditRowPerWrite`: fields asserted, including the predecessor row.
- `TestProviderCredentialAPI_ResponsesAuditsLogsNeverContainSecret`:
  - a sentinel secret and a sentinel confirmation value are set up;
  - every response, audit row and captured log line is scanned for the raw, hex, base64 and base64url
    forms.
- `TestSecretTypes_Redaction`: `%v`, `%+v`, `%#v`, slog and `json.Marshal`, for `CredentialSet`,
  `OutboundCredential` and store results.

---

## 7. Items for `architect` to record in ADR 0093 before W2a merges (engineering, not human)

1. Add `activation_request_id` to the §1 handle table (B3), together with the request and approval table
   shapes in §1.2.
2. Add the DB namespace CHECK and the rule that `awssm` refs pin `versionId` in every environment. Both
   are stricter than the current ADR text.
3. Add the §3 notes on fingerprint-key stability and the confirmation value.

All three tighten ADR 0093; none of them weakens it. If `architect` declines any of them, W2a still
merges only with an equivalent that `security` concurs with.

## 8. Human decisions

**None.** DEPLOY-FPKEY-1 (delivering the fingerprint key on AWS) is already registered as a future human
decision and is not affected. Launch-blocking flags are unchanged from `04-review-security.md` §8 and
`06-gate-w1-review-security.md`.

## 9. Scope of this review

**Covered:** the W2a design, meaning the tables, policies, triggers, resolver, cache and breaker,
outbound handling, backends, fingerprint key and admin API, as specified in ADR 0093 and here.

**Not covered:**
- any W2a implementation (none has been reviewed);
- the W3b `awssm` code;
- `deploy/`;
- real vendor schemes;
- volumetric DoS beyond §5.

The W2a code still needs its own `security` review before it is marked complete. This design ruling does
not make the implementation secure.
