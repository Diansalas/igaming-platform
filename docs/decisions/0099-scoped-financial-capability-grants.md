# ADR 0099 — Scoped financial capability grants on the existing RBAC (PRH-2 K1)

- **Status:** ACCEPTED (2026-09-28). `security` CONFIRMED WITH CONDITIONS (C-1, written into §6.3, §10.1 and A-4)
  and `ledger-finance` CONFIRMED (no K1 condition), both on revision 2
  (`docs/plans/prh2-hardening-round/reviews/adr-0099-0101-{security,ledger-finance}-confirmation.md`). NOT IMPLEMENTED.
- **Revision history:** PROPOSED — **revision 2** (`architect`, 2026-09-28). Revision 1
  (`d83a71c`) was reviewed ACCEPT WITH CONDITIONS by `product-owner-proxy` (ACCEPT), `security` and
  `ledger-finance`. This revision applies every condition (§18). **`security` and `ledger-finance`
  confirm this revision before any K1 code.**
- **Decision type:** cross-domain architecture and security control (`auth`, `identity`, `db`,
  `httpserver`, the new `internal/capability`, and the consumers `internal/adjustment` (ADR 0100) and
  `payments` (ADR 0101)).
- **Owner:** `architect`. **Reviewers:** `security` (all of it), `ledger-finance` (financial
  invariants, the acting-family ledger and projection fences), `qa` (§14), `code-reviewer`,
  `product-owner-proxy` (the closed enum — **confirmed**).
- **Registry:** CAP-GRANT-1; consumers HD-0095-1 and LEDGER-MANUAL-ADJ-4EYES-1; follow-up
  STAFF-LIFECYCLE-1. Workstream K1 of `docs/plans/prh2-hardening-round/plan.md`; migration 0112.
- **NOTE ON NUMBERING (implementation record, backend-engineer K1 build):** this ADR's text, as
  originally drafted, numbered K1's own migration 0111 and referred to K2/K3's future migrations as
  0112/0114. The orchestrator's allocation changed during K1's build (B, the casino bootstrap,
  merges first and takes 0111): the final allocation is **B = 0111, K1 = 0112, K2 = 0113, E1 = 0114,
  K3 = 0115**. Every migration-number reference in this ADR has been updated to that final
  allocation (K1's own migration is `0112`; where the text discusses K2/K3's future tables it now
  says `0113`/`0115`) — this is the same renumbering-disclosure pattern migration 0105's own header
  comment uses. The actual K1 migration file on disk is
  `migrations/0112_scoped_financial_capability_grants.{up,down}.sql`.
- **Binding inputs:**
  - ADR 0098 §1–§3 and §5: HD-PRH2-2 = (c); HD-PRH2-5; HD-PRH2-6.
  - Plan §11.
  - `reviews/security.md` S-1, S-3, S-4 and S-11.
  - `reviews/adr-0099-0101-security.md` Part 2: C-99-1..C-99-8 and rulings 1–7.
  - `reviews/adr-0099-0101-ledger-finance.md`: F1, F2 and F3, rulings 2 and 6.
  - `reviews/adr-0099-0101-product-owner-proxy.md` Q1/Q2.
  - `reviews/qa.md` (W1 K1).
- **Related:** ADR 0011 (platform tokens), ADR 0013 (dual-scope RLS), ADR 0024 (Stage 3D in-tx
  eligibility re-read; platform-minted `finance`), ADR 0082 (lock order; Amendment A8 in ADR 0100
  §8), ADR 0095 §10.2 (session resolver, migration 0105), migration 0096 (Person-distinct four-eyes;
  no SECURITY DEFINER), ADR 0104 / migration 0109 (`subject_tenant_id`).
- **No regulatory or licensing claim.** Software capability is not legal approval.

| Rev | Base | Change |
|---|---|---|
| 1 | `cabca27` → `d83a71c` | Initial draft |
| 2 | `6864efa` | All review conditions applied. Main changes: the enumerated `AS RESTRICTIVE` acting fence on every NULL-arm table (C-99-1); grantees limited to `finance` and G-P2 `platform_admin` (C-99-2); the projection write fence (LF F1); exact ledger fence predicates (LF F2); `FOR SHARE` at execution (LF F3); acting SELECT on `payment_attempts`/`deposit_intents` (LF ruling 2); the audit actor trigger (C-99-3); INV-CAP-6 reworded (C-99-4); column-discipline test (C-99-5); G-P2 lifetime (C-99-6); re-attestation reduced to an audit action (PO Q2, security ruling 6); the setter contract (C-99-8); full 0112 content; review disposition |

---

## 1. Context

ADR 0098 decided that force-resolve and manual adjustment are configurable, user-level capabilities
at platform and tenant scope, and that the existing RBAC is extended rather than duplicated
(0098 §3).

Verified facts (at `6864efa`; the code is unchanged since `cabca27`):

| Fact | Where |
|---|---|
| A static role → permission map | `internal/auth/permission.go:583-880` |
| One role per staff user; `tenant_id IS NULL` ⇔ `platform_admin` | `0011:12,23-26` |
| `RequirePermission` reads the role from the JWT. Tokens last 15 minutes and are not revocable. | `permission.go:899-914`; `jwt.go:98-102`; `config.go:448` |
| Persons are optional on staff, unverified, and minted per NO_MATCH registration | `0029:12-18`; `0009:13-19`; `player_account.go:108-113` |
| A tenant caller creates `tenant_admin`, `support` and `compliance` staff with a chosen password and `person_id`. `finance`, `risk_manager`, `promotions_manager` and `bonus_operations` are platform-only. **`platform_admin` is not in the HTTP allowlist.** | `admin_routes.go:491-535,550,656` |
| `platform_admin` accounts come only from `cmd/seed-admin` | `cmd/seed-admin/main.go:1` (security review) |
| **No staff lifecycle API.** The only staff UPDATE is the person link; no API suspends staff, changes a role or resets a password. | `identity/staff_user.go:164` (security review) |
| `WithPlatformAdmin` sets only `app.platform_admin_principal_id` | `db/tenant_rls.go:112-133` |
| The 0105 resolver raises on any unresolvable or mixed session | `0105:25-50` |
| Ledger tables are tenant-RLS on `app.tenant_id` only | `0021:65-68`, `0022:116-131`, `0023:72-87`, `0019`, `0020` |
| Every table has RLS enabled | checked by `comm` over `CREATE TABLE` vs `ENABLE ROW LEVEL SECURITY` across `migrations/*.up.sql` |

## 2. Decision summary

1. **Two layers, both required.** A governed financial action needs the static permission (route,
   then re-checked in-tx) **and** an in-force grant row read in the action's own transaction and,
   at execution, locked `FOR SHARE`. A JWT is never trusted for a grant.
2. **A closed, extensible enum** (§3). It is confirmed by `product-owner-proxy`.
3. **The grant model is HD-PRH2-2 (c)** (§4): a tenant admin requests; an independent platform
   principal co-approves. A platform-originated grant needs a second, independent platform approver.
4. **The "platform principal acting in tenant X" session family** (§6) uses its own GUC names. It
   is valid only with an in-force grant for X. Four things close it:
   - **`AS RESTRICTIVE` deny policies** on every existing table whose policies have a
     `tenant_id IS NULL` arm;
   - acting policies only on an enumerated table list;
   - a **ledger fence** and a **projection fence**;
   - an audit actor trigger.
5. **The actor is derived from the DB session** (§7). Staff and grant rows are re-read, and at
   execution locked `FOR SHARE` (S-4, LF F3).
6. **Lifecycle** (§8): one-way revoke (the emergency stop); a mandatory expiry for G-P2 grants; a
   `grant.reattested` audit action. The re-attestation table and view move to STAFF-LIFECYCLE-1.
7. **The S-1 caveat is stated plainly** (§9). Distinct-Person is defence in depth. The structural
   control is that the platform co-approver is not tenant-mintable over HTTP.

## 3. The capability enum (closed, extensible) — CONFIRMED by product-owner-proxy

### 3.1 Financial capabilities (grantable, always tenant-scoped)

| Capability | Operation kind (ADR 0100 §2) | Used by |
|---|---|---|
| `ledger_adjustment:initiate` | `ledger_adjustment` | ADR 0100 |
| `ledger_adjustment:approve` | `ledger_adjustment` | ADR 0100 |
| `payment_force_resolve:request` | `payment_force_resolve` | ADR 0101 |
| `payment_force_resolve:approve` | `payment_force_resolve` | ADR 0101 |

Every grant names exactly one tenant (`tenant_id NOT NULL`). There is no all-tenants grant and no
platform-wide financial grant.

### 3.2 Governance permissions (static RBAC, not grantable)

| Permission | Roles |
|---|---|
| `capability_grant:request` | `tenant_admin` (own tenant), `platform_admin` |
| `capability_grant:approve` | `platform_admin` only |
| `capability_grant:revoke` | `tenant_admin` (own tenant), `platform_admin` |
| `capability_grant:read` | `tenant_admin`, `compliance`, `platform_admin` |
| `financial_policy:author` | `platform_admin` only |
| `financial_policy:tighten` | `tenant_admin` |
| `financial_policy:read` | `tenant_admin`, `finance`, `compliance`, `platform_admin` |

These are static so that there is a bootstrap. A grantable approver capability would need a seeded
person or a bypass for its first holder.

### 3.3 Eligible grantees (C-99-2; security ruling 1)

| Grantee | Eligible for | How |
|---|---|---|
| `finance` staff of tenant X | all four financial capabilities | G-T or G-P1 |
| `platform_admin` | all four, for **one named tenant X** | G-P2 only |

- `finance` is platform-minted only (`admin_routes.go:504-533`).
- **`compliance` is not eligible in PRH-2.** Tenant admins mint `compliance` accounts with
  passwords they choose, and there is no reset or first-login-change flow. That is S-1 one role over.
- **`tenant_admin`, `support`, `risk_manager`, `promotions_manager` and `bonus_operations` are
  never eligible.**
- Residual: `finance` accounts get their initial password from their (platform) creator. This is
  tracked in STAFF-LIFECYCLE-1.
- The static permission of each financial capability is held by `finance` and `platform_admin`
  only. The catalogue copy of these role sets (§10.1) is what the triggers re-check.

### 3.4 Extensibility

A new capability needs a migration (catalogue row, classification row if financial), an ADR, and
the static permission. Nothing is added at runtime. The app role has `SELECT` only on the catalogue
tables.

## 4. The grant model (HD-PRH2-2 = (c))

| Flow | Requester (session) | Required approval (session) |
|---|---|---|
| **G-T:** tenant-originated grant for `finance` staff of X | `tenant_admin` of X, `capability_grant:request` (tenant) | one `platform_admin` with `capability_grant:approve` (platform) |
| **G-P1:** platform-originated grant for `finance` staff of X | `platform_admin` (platform) | a **different** `platform_admin` with a **different** Person (platform) |
| **G-P2:** platform-originated grant for a `platform_admin` to act in X (HD-PRH2-6) | `platform_admin` (platform) | as G-P1 |

- A request goes `pending → approved | rejected | cancelled | expired`.
- The approval row and the grant row are inserted in **one transaction**
  (`decided_txid = txid_current()`; the 0105 precedent at `0105:162,324`).
- Request TTL: a technical default, forced by trigger (0105 uses 24 hours). It is not a money value.

**Always refused (trigger; mirrored in Go for legible errors):**

| Rule | Check |
|---|---|
| R-1 | grantee ≠ requester |
| R-2 | approver ≠ requester |
| R-3 | approver ∉ {requester, grantee} |
| R-4 | requester, approver and grantee each have a non-NULL `person_id`; the three are pairwise distinct |
| R-5 | a tenant requester's tenant = the request's tenant = the grantee's tenant |
| R-6 | a tenant requester may not name a platform grantee; governance permissions are not in the catalogue at all |
| R-7 | the approval's derived scope is `platform` |
| R-8 | for G-P1/G-P2, the approver is a different platform principal and a different Person from the requester |
| R-9 | the grantee's role is eligible (§3.3); the grantee is `active`; a platform grantee only via G-P2 |
| R-10 | an acting session never requests, approves or revokes grants |
| R-11 | one pending request per `(tenant, grantee, capability)` (partial UNIQUE) |
| R-12 | no second unrevoked, unexpired grant per `(tenant, grantee, capability)`. The approval trigger checks this against `now()`, plus a partial UNIQUE on unrevoked rows. |
| R-13 | a G-P2 grant has NOT NULL `valid_until`, `<= valid_from + acting_grant_max_lifetime` (§8.2) |

## 5. Session families

| Family | GUCs | Opened by | May |
|---|---|---|---|
| **tenant** | `app.tenant_id = X`, `app.principal_id = S` | `db.WithPrincipalScope(X, S)` (plain `WithTenant` sets no principal and is refused by the resolver) | G-T requests, revoke, read in X; K2/K3 actions in X with its own grants |
| **platform** | only `app.platform_admin_principal_id = P` | `db.WithPlatformAdmin` | G-P1/G-P2 requests, approvals, revokes, reads in any tenant. **No K2/K3 or ledger access.** |
| **platform acting in X** | only `app.acting_tenant_id = X`, `app.acting_platform_principal_id = P` | `db.WithPlatformActingInTenant` (§6.1) | K2/K3 actions in X with P's grants for X; nothing else (§6) |
| player, service, mixed | anything else | — | nothing on any 0112/0113/0115 table |

HD-PRH2-6's interim "refuse platform scope on tenant ledgers" becomes **"refuse unless an explicit
in-force grant for that tenant exists"**.

## 6. The "platform principal acting in tenant X" family (HD-PRH2-6, S-3)

### 6.1 The setter (C-99-8)

```go
func (p *Pool) WithPlatformActingInTenant(ctx context.Context, principalID, targetTenantID uuid.UUID, fn TxFunc) error
```

- It lives in `internal/db/tenant_rls.go` (a K1 Touches addition).
- `principalID` comes **only** from the verified token subject (`tenant.FromContext(ctx).Subject`).
  The caller passes the value it read from the context; a static test pins that every call site's
  argument is that expression.
- `targetTenantID` comes **only** from the route-validated target (`canActOnTenant`). It is never
  read from a request body or query.
- Inside one transaction the setter:
  1. sets exactly `app.acting_tenant_id` and `app.acting_platform_principal_id` via `set_config(…,
     true)`, and nothing else;
  2. runs `SELECT financial_acting_session_open()`, which raises SQLSTATE `CG020` unless the §6.3
     validity holds;
  3. writes the audit row `financial.acting_session_opened` (actor P, tenant X, request id, the
     calling operation) in the same transaction, then calls `fn`.

  A refused open rolls the transaction back. It is logged at Warn and counted by
  `financial_acting_session_refused_total{reason}`. There is no tenant label.
- **Static tests:**
  - raw `set_config('app.acting_…')` anywhere outside this function is forbidden;
  - only `internal/adjustment` and `internal/payments/manual_resolution.go` call the setter.

### 6.2 What an acting session can reach (corrected, C-99-1)

Revision 1 claimed "no existing policy matches an acting session". **That was false.** An acting
session leaves `app.tenant_id` unset, so it satisfies every policy arm of the form
`tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL` (K1-1).

The effective policy set at HEAD was recomputed by replaying every `CREATE POLICY`/`DROP POLICY` in
`migrations/*.up.sql` in order. The results:

**Exposed: a NULL arm with no other required positive GUC. All are closed by 0112's restrictive
policies (§6.4).**

| Table | Exposed policies (effective at HEAD) | What an acting session could do without the fence |
|---|---|---|
| `staff_users` | `dual_scope_isolation` FOR ALL (0011) | read every platform staff row including `password_hash`; **insert a `platform_admin`**; update or delete platform staff |
| `sessions` | `session_scoped_insert`, `session_scoped_update` (0012) | insert or update a platform session row |
| `login_attempts` | `dual_scope_isolation` FOR ALL (0013) | read or write platform login attempts |
| `audit_log` | `dual_scope_isolation` FOR ALL (0014) | read platform audit rows; insert platform audit rows |
| `persons` | `persons_platform_scope_read_write` (SELECT), `_update`, `_delete` (0015); `persons_insert_any_scope` INSERT `WITH CHECK (true)` | read, update, delete or insert Persons, including forging a "distinct" Person |
| `player_restrictions` | `staff_insert` (0037; **not narrowed** by 0038 or later) | insert a platform-level (`tenant_id IS NULL`) staff restriction |
| `risk_rules` | `tenant_and_platform_write`, `_update`, `_delete_visibility` (0041; **not narrowed** later) | write platform-level risk rules |

**Checked and not exposed:**
- **`open_bet_self_exclusion_policies`:** 0043's NULL arm was **narrowed by 0049** to require
  `app.platform_admin_principal_id`.
- **`asset_operation_eligibility` (0045), and every `platform_admin_scope` / `*_platform_admin_*`
  policy** (assets, casino catalogue, jurisdictions, licences, tenants, kyc enforcement policies,
  provider credentials, kill switch, sb restrictions): these require the platform GUC.
- **The sb catalogue sync policies:** these require `app.platform_service_id`.
- **`player_credential_tokens.token_lookup`:** requires `app.credential_token_lookup_hash`.
- **The `sessions` SELECT policies:** require `app.principal_id`, `app.session_internal_op_id` or
  `app.session_lookup_hash`.

**Read-only reference data an acting session can still read (accepted):** `USING (true)` or
player-unset-only SELECT policies on:
- `assets`, `brands`, `casino_games`, `jurisdictions`, `jurisdiction_precedence_configs`,
  `kyc_enforcement_policies`;
- `platform_operations`, `platform_products`;
- `sb_*` catalogue tables, `sb_jurisdiction_restrictions`;
- `tenants` (`tenants_read`), `licences` and `licence_country_ceilings` (their read policies need
  `app.tenant_id` or the platform GUC, so these return nothing to an acting session).

These tables are platform reference data that every staff session already reads. They hold no PII
and no credentials. The acting executor needs `assets` and `tenants` (asset status, licence →
jurisdiction).

**Future-proofing:** static test **A-18** replays the migrations the same way and fails if any
effective permissive policy with a NULL `app.tenant_id` arm and no other required positive GUC has
no restrictive acting policy on its table.

### 6.3 Validity, and the policy predicates

`financial_acting_session_valid()` (`STABLE`, **not** `SECURITY DEFINER`) is true only if all of
the following hold:
- exactly the two acting GUCs are set, and `app.tenant_id`, `app.principal_id`,
  `app.platform_admin_principal_id`, `app.player_account_id`, `app.platform_service_id` are unset;
- `staff_users` has row P with `tenant_id IS NULL`, `role = 'platform_admin'`, `status = 'active'`
  (visible through the own-row exception, §6.4);
- `staff_capability_grant_in_force(X, P, NULL, now())`: any unrevoked financial grant for (X, P)
  with `valid_from <= now() < valid_until`.

**Acting permissive-policy predicate** (every table in §6.5, except the two below):

```sql
tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid
AND financial_acting_session_valid()
```

**Recursion exception.** `staff_users` and `staff_capability_grants` are read *by* the validator.
Their acting policies are therefore **GUC-only**:

```sql
financial_acting_gucs_exact()
AND tenant_id = acting tenant
```

Here `financial_acting_gucs_exact()` checks the GUC shape only. This avoids policy recursion. It is
safe because the setter raises before `fn` runs unless the session is valid, and the setter is the
only code that can create the shape (§6.1 static tests).

**Exact GUC shape (security confirmation C-1, binding on K1).** `financial_acting_gucs_exact()` is
true only when **both** `app.acting_tenant_id` and `app.acting_platform_principal_id` are set **and**
all of `app.tenant_id`, `app.principal_id`, `app.platform_admin_principal_id`,
`app.player_account_id` and `app.platform_service_id` are unset (NULL or empty). A mixed session
therefore never matches the recursion-exempt policies. A-4's mixed-session cases cover these two
tables.

### 6.4 The restrictive fence (C-99-1) — added by 0112

`financial_acting_gucs_present()` is `STABLE` and true if either acting GUC is non-empty. Here
`acting` abbreviates that function. For each table below, 0112 adds `AS RESTRICTIVE` policies per
command. They are ANDed with every permissive policy, so they bind every existing and future
permissive arm.

| Table | SELECT (USING) | INSERT (WITH CHECK) | UPDATE (USING / WITH CHECK) | DELETE (USING) |
|---|---|---|---|---|
| `staff_users` | `NOT acting OR id = acting_principal OR tenant_id = acting_tenant` | `NOT acting` | USING as SELECT / `NOT acting` | `NOT acting` |
| `audit_log` | `NOT acting` | `NOT acting OR (tenant_id IS NOT NULL AND tenant_id = acting_tenant)` | `NOT acting` / `NOT acting` | `NOT acting` |
| `sessions`, `login_attempts`, `persons`, `player_restrictions`, `risk_rules` | `NOT acting` | `NOT acting` | `NOT acting` / `NOT acting` | `NOT acting` |

Notes:
- `staff_users` UPDATE USING admits the own row and X rows **only so that `FOR SHARE` can lock
  them** (PostgreSQL checks UPDATE USING for `SELECT … FOR SHARE`). WITH CHECK `NOT acting` refuses
  every actual update.
- `persons` is fully denied. No acting code reads `persons`; the beneficiary and distinct-Person
  checks read `staff_users.person_id` and `player_accounts.person_id`.

### 6.5 Acting permissive policies (closed list)

| Table | Acting access | Migration | Notes |
|---|---|---|---|
| `staff_users` | SELECT X rows (GUC-only); UPDATE USING X rows, WITH CHECK `false` (for `FOR SHARE`) | 0112 | own row via 0011's NULL arm, cut to the own row by §6.4 |
| `staff_capability_grants` | SELECT X rows (GUC-only); UPDATE USING X rows, WITH CHECK `false` (for `FOR SHARE`) | 0112 | |
| `audit_log` | INSERT `tenant_id = acting tenant` | 0112 | the actor trigger (§11) forces actor and tenant |
| `player_accounts` | SELECT X | 0113 | column discipline (§6.8) |
| `wallets`, `assets` (already readable) | SELECT X | 0113 | |
| `ledger_accounts` | SELECT X; INSERT X with `account_type IN ('player_cash','manual_adjustment','player_withdrawal_hold','psp_clearing')` | 0113 | the account types the governed postings resolve |
| `ledger_transactions`, `ledger_entries` | SELECT X; INSERT X, **fenced** (§6.6). `ledger_transactions` also has UPDATE USING X WITH CHECK `false`, only for the ADR 0100 §6.5 L2 causation lock; 0082's immutability triggers refuse real updates anyway. | 0113 | |
| `wallet_balance_projection` | SELECT X; INSERT and UPDATE X, **fenced** (§6.7) | 0113 | UPDATE also needed for L3 `FOR UPDATE` |
| `payment_attempts`, `deposit_intents` | SELECT X; UPDATE USING X (for `FOR UPDATE`) with the WITH CHECK in ADR 0101 §6.4 | 0113 SELECT (LF ruling 2: the `open_payment_exposure` check); 0115 UPDATE | `deposit_intents` UPDATE WITH CHECK is `false` (M1 never updates an intent) |
| `withdrawal_requests` | SELECT X; UPDATE USING X, WITH CHECK per ADR 0101 §6.4 | 0115 | |
| ADR 0100 §9 tables; ADR 0101 §8 tables | per those ADRs | 0113 / 0115 | |

A table that `ledger.Post`, `LockProjectionsForPosting`, `GetOrCreateAccounts` or
`withdrawal.Complete`/`Fail` touch and that is missing from this list makes the governed path fail
closed. The positive tests (A-3, B-3, C-4) must find any such gap before merge. Adding a table needs
an amendment to this section.

### 6.6 The ledger fence (LF F2) — exact predicates

The trigger `ledger_transactions_governed_fence` (BEFORE INSERT) works as follows. When
`financial_acting_gucs_present()`, NEW must satisfy **exactly one** of these, otherwise it raises
`CG030`:

```sql
-- (a) manual adjustment (ADR 0100)
NEW.transaction_type = 'manual_adjustment' AND EXISTS (
  SELECT 1 FROM ledger_adjustment_requests r
   WHERE r.tenant_id = NEW.tenant_id
     AND NEW.idempotency_key = 'manual_adjustment:' || r.id::text
     AND NEW.correlation_id = r.id
     AND r.state = 'executing' AND r.executed_txid = txid_current())
-- (b) M2 declare paid (ADR 0101)
OR NEW.transaction_type = 'withdrawal_completed' AND EXISTS (
  SELECT 1 FROM payment_manual_resolutions m
   WHERE m.tenant_id = NEW.tenant_id AND m.kind = 'm2_declare_paid'
     AND m.state = 'executing' AND m.executed_txid = txid_current()
     AND NEW.correlation_id = m.withdrawal_request_id
     AND NEW.provider_id = m.provider_id
     AND NEW.provider_tx_id = m.reserved_provider_tx_id
     AND NEW.idempotency_key = m.provider_id || ':' || m.reserved_provider_tx_id)
-- (c) M2 declare not paid (ADR 0101)
OR NEW.transaction_type = 'withdrawal_failed' AND EXISTS (
  SELECT 1 FROM payment_manual_resolutions m
   WHERE m.tenant_id = NEW.tenant_id AND m.kind = 'm2_declare_not_paid'
     AND m.state = 'executing' AND m.executed_txid = txid_current()
     AND NEW.correlation_id = m.withdrawal_request_id
     AND NEW.idempotency_key = m.withdrawal_request_id::text || ':failed')
```

- The keys match the code: `withdrawal.go:1442` (`providerID + ":" + providerTxID`) and `:1539`
  (`requestID + ":failed"`).
- 0113 creates the trigger with branch (a) only. 0115 replaces the function with (a) + (b) + (c).
- The same predicate, evaluated for the parent transaction row, gates acting INSERTs into
  `ledger_entries` (trigger `ledger_entries_governed_fence`). So entries can never be appended to a
  pre-existing transaction.
- **(d) The reserved-prefix guard applies to all sessions.** It is created in 0115; see ADR 0101
  §5.4.
- Outside acting sessions the trigger is a no-op, so existing flows are unchanged. `txid_current()`
  inside `ledger.Post`'s savepoint is the top-level xid (*not executed*; test A-4c proves it).

### 6.7 The projection fence (LF F1)

The trigger `wallet_balance_projection_acting_fence` (BEFORE INSERT OR UPDATE OR DELETE on
`wallet_balance_projection`) applies only when `financial_acting_gucs_present()`:

| Command | Rule |
|---|---|
| INSERT | allowed only if `NEW.debit_total = 0 AND NEW.credit_total = 0` (the `ensureProjectionRowSQL` shape, `lockorder.go:214-219`), **or** `pg_trigger_depth() >= 2`, meaning it is issued from inside 0023's `ledger_entries_update_projection` trigger |
| UPDATE | allowed only if `pg_trigger_depth() >= 2` (the 0023 trigger's upsert). A direct UPDATE, or `RebuildProjectionRow`'s `ON CONFLICT DO UPDATE` issued by the session, runs at depth 1 and is refused (`CG031`). |
| DELETE | always refused (`CG031`). There is also no acting DELETE policy. |

`SELECT … FOR UPDATE` (L3) needs the acting UPDATE policy's USING clause and fires no trigger, so
locking works. Test A-4b proves the depth semantics (*not executed*).

### 6.8 Column discipline (C-99-5; security ruling 2)

Acting-session reads are row-narrowed by RLS (tenant X) and column-narrowed by static test **A-19**.
Every SQL literal in `internal/adjustment` and `internal/payments/manual_resolution.go`, and every
helper they call, that reads:
- `staff_users` selects only `id, tenant_id, role, status, person_id`;
- `player_accounts` selects only `id, tenant_id, person_id, status`;
- never `password_hash`, email or other PII.

No `SELECT *` on either table. No further row narrowing now (security ruling 2).

### 6.9 Threat model (corrected)

| # | Threat | Control | Residual |
|---|---|---|---|
| TM-1 | A platform principal reaches a tenant ledger without a grant | The setter raises; every acting permissive policy (except the two recursion-exempt tables) calls `financial_acting_session_valid()`; the fences require an executing, approved request or resolution | None known |
| TM-2 | A grant for X is used on Y | Every acting predicate compares with `app.acting_tenant_id`; grants are validated per (X, P) | None known |
| TM-3 | Lateral use within X, or of platform-scope NULL arms (rev 1 was wrong here, K1-1) | The §6.4 restrictive fence on all seven exposed tables; the closed §6.5 list; the kill-switch resolver raises for this shape; A-18 future-proofing; A-19 column discipline | Reads of X's `staff_users` and `grants` rows are GUC-only (recursion exception), so a session with the acting GUC shape but no valid grant could read them. It cannot be created outside the setter, which raises first. Reference-data reads (§6.2) are accepted. |
| TM-4 | A non-governed money write | The ledger fence (§6.6); the projection fence (§6.7); `ledger_accounts` INSERT limited by account type | None known |
| TM-5 | GUC injection | The sole setter; the static tests (§6.1); P from the token, X from the route; the validator re-reads staff | A future raw `set_config`, pinned by static test |
| TM-6 | A mixed session (corrected) | The setters are mutually exclusive by construction (no setter sets both families' GUCs); `financial_actor_session()`, the acting validator and every 0112/0113/0115 policy require the other family's GUCs unset | Existing tenant policies elsewhere do not check the acting GUCs. A hypothetical acting + `app.tenant_id` session would have exactly its tenant half's power, and no K-table or ledger-fence power. Pinned by the static tests. |
| TM-7 | A single compromised platform account | It needs its own G-P2 (a second platform human) and an independent approver per operation (a distinct Person) | **Two colluding platform admins**, plus the `seed-admin` trust root (§9). **LAUNCH FLAG:** needs human risk acceptance before real-money K operations (security review). |
| TM-8 | A stale JWT | In-tx re-read and `FOR SHARE` at execution (§7) | There is no suspend API (STAFF-LIFECYCLE-1). **Grant revoke is the emergency stop** (§8.1). |
| TM-9 | Self-approval of an acting grant | R-2, R-3, R-4, R-8 | None known |
| TM-10 | Own-licence tenants | Not an engineering control | **LAUNCH FLAG: LEGAL / COMPLIANCE REVIEW** before any own-licence tenant receives a G-P2 grant |
| TM-11 | A forged Person under an acting session | `persons` is fully denied to acting sessions (§6.4) | None known |

## 7. Actor derivation and re-checks (S-4, LF F3)

1. **`financial_actor_session(OUT actor uuid, OUT scope text, OUT tenant uuid, OUT person_id uuid,
   OUT role text)`** resolves one of:
   - **tenant:** `app.tenant_id` and `app.principal_id` set, everything else unset; the actor is a
     staff row of that tenant;
   - **platform:** only `app.platform_admin_principal_id` set; the actor has `tenant_id IS NULL`;
   - **platform_acting:** §6.3.

   Anything else raises `CG001`. The actor's `status` must be `active`. A NULL `person_id` raises
   `CG002` for any K write.
2. **Forced actor columns.** Every `*_by`, `*_by_scope` and `*_by_person_id` column on a
   0112/0113/0115 table is overwritten by the trigger from the resolver (the `0105:88-89`
   precedent). A test asserts that a mismatching supplied value never persists.
3. **In-tx role re-check.** The trigger checks the actor's current `role` against the catalogue's
   role sets (§10.1). Test A-15 pins that the Go map and the catalogue agree.
4. **Execution re-check with `FOR SHARE` (LF F3; ADR 0082 A8 per LF ruling 6).**
   - At execution, after the request or resolution row lock (L1), the executor runs:
     - `SELECT id, tenant_id, role, status, person_id FROM staff_users WHERE id = ANY($counted)
       ORDER BY id FOR SHARE`;
     - then `SELECT … FROM staff_capability_grants WHERE id = ANY($grant_ids) ORDER BY id FOR
       SHARE`.
   - Here `$counted` is the initiator plus every approval being counted.
   - An approval counts only if its approver is `active`, has an eligible role, and holds an
     in-force, unrevoked grant at `now()`.
   - A concurrent revoke either commits first, and `FOR SHARE` then re-reads the revoked version,
     so the approval is not counted; or it waits for the execution to commit.
5. **A cross-family visibility limit (stated).**
   - A tenant session cannot see a platform approver's `staff_users` row (0011's policy), and an
     acting session sees only its own platform row.
   - So when an execution runs in a tenant session and an earlier approver was a platform acting
     principal, that approver's **grant** (tenant X row, visible and locked `FOR SHARE`) is
     re-checked, but their staff `status` is re-checked only at their own approval time.
   - This is acceptable because platform staff status cannot change through any API
     (STAFF-LIFECYCLE-1), and grant revoke is the emergency stop.
   - Once STAFF-LIFECYCLE-1 adds a suspend path, it must also revoke the principal's grants in the
     same transaction. This is recorded as a STAFF-LIFECYCLE-1 requirement.

## 8. Lifecycle (S-11)

### 8.1 Revoke: the emergency stop (C-99-7)

- A single actor can revoke: a `tenant_admin` of X, for any grant in X (including G-P2 grants held
  by platform principals), or a `platform_admin`, for any tenant.
- UPDATE may set only `revoked_at`, `revoked_by`, `revoked_by_scope` and `revoke_reason_code`, each
  from NULL. Every other column must be unchanged (whole-row equality). There is no un-revoke.
  DELETE and TRUNCATE are refused.
- Effective at the next in-tx read. A concurrent execution is handled by §7.4.
- **The runbook names grant revoke as the emergency stop.** It is the only immediate way to remove a
  staff member's financial authority, because there is no suspend or role-change API
  (STAFF-LIFECYCLE-1).

### 8.2 Expiry (C-99-6; security ruling 3)

- **G-P2:** `valid_until` is NOT NULL and `<= valid_from + acting_grant_max_lifetime` (R-13,
  checked by trigger against `financial_capability_settings`).
- **G-T and G-P1:** `valid_until` is optional.
- An expiry between approval and execution means the approval does not count.
- **Orchestrator decision:** `acting_grant_max_lifetime` is a **technical security default**. It is
  configurable (a `financial_capability_settings` row, changed by migration) and its value is set
  and recorded in K1's implementation record. It is not a human policy value.

### 8.3 Demoted or suspended grantor: re-attestation (PO Q2, security ruling 6)

- A grant stays in force if its requester or approver later changes.
- **K1 provides the audit action `grant.reattested`.** A platform principal (or a tenant admin of
  X) records that an in-force grant was reviewed. It is written through `audit.Record`, with the
  grant id, the attesting actor and a note code.
- **The typed re-attestation table, the "needing re-attestation" view, and the p2 alert for
  entries older than a configurable technical default all move to STAFF-LIFECYCLE-1.** This is
  adopted from `product-owner-proxy`, and `security` accepts it.
- Rationale: with no staff suspend or role-change API, "grantor demoted or suspended" cannot arise
  through the product today.
- The re-attestation alert age is, like §8.2, a technical security default (orchestrator decision)
  fixed with STAFF-LIFECYCLE-1.

### 8.4 Grantee changes

A suspended or demoted grantee fails the §7.4 check at use. The grant row is not changed.

## 9. Identity caveat (S-1) and trust roots

**Under the unverified identity model, distinct-principal and distinct-Person checks do not prove
two humans.** A tenant admin can mint `tenant_admin`, `support` and `compliance` accounts with
chosen passwords and Persons, and Persons are self-registered and unverified.

**The structural control is HD-PRH2-2 (c):**
- every financial grant needs a platform approver (R-7);
- `compliance` is not grantable (§3.3);
- `finance` is platform-minted;
- no HTTP API creates a platform principal.

A person link proves a human only once verified staff identity exists. Every K actor must have a
non-NULL `person_id`. The platform co-approver verifies out of band that a requested grantee is a
real, distinct person; that is a runbook control, not a DB property.

**INV-CAP-6 (reworded, C-99-4; security ruling 7): no HTTP API creates a `platform_admin`.**
- `cmd/seed-admin` is an out-of-band trust root.
- Static test **A-14b** fails if any non-test Go package other than `cmd/seed-admin` inserts a
  `staff_users` row with role `platform_admin`. The test-support packages allow-listed are
  `internal/testsupport/*` and `internal/providercred/providercredtest`.
- The acting-session path to such an insert is closed by §6.4.
- The runbook documents `seed-admin` as a **two-person, audited procedure**: two named operators,
  a recorded ticket, and the audit row it writes.
- **Security's recommended DB bootstrap guard** (a trigger refusing a `platform_admin` INSERT or
  UPDATE unless a bootstrap GUC set only by `cmd/seed-admin` is present) is **adopted as a
  STAFF-LIFECYCLE-1 deliverable, not K1.** 23 test files insert `platform_admin` fixtures directly,
  and they need a shared helper first. The static test above is mandatory in K1.

## 10. Migration 0112 (K1) — content

Common rules:
- **FORCE ROW LEVEL SECURITY** on every new table.
- No `FOR ALL` permissive policy.
- `BEFORE TRUNCATE` → `ledger_deny_mutation()`; DELETE refused by trigger.
- No `SECURITY DEFINER`.
- Every RAISE uses SQLSTATE class **`CG`** (codes listed in the migration header, as 0096 does). Go
  classifies by code only.

**Family predicates used below:**

| Family | Predicate |
|---|---|
| T (tenant) | `tenant_id = NULLIF(current_setting('app.tenant_id',true),'')::uuid AND NULLIF(current_setting('app.principal_id',true),'') IS NOT NULL AND` player, platform, platform-service and both acting GUCs unset |
| P (platform) | `NULLIF(current_setting('app.platform_admin_principal_id',true),'') IS NOT NULL AND` tenant, player, platform-service and acting GUCs unset |
| A (acting) | §6.3 |
| R (reference) | `NULLIF(current_setting('app.player_account_id',true),'') IS NULL`; SELECT only; no write policy; no app-role write grant |

### 10.1 Functions

- `financial_acting_gucs_present()` and `financial_acting_gucs_exact()`: GUC shape only, `STABLE`. `_exact()` has the exact shape pinned in §6.3 (C-1).
- `financial_acting_session_valid()` and `financial_acting_session_open()` (the latter raises
  `CG020`).
- `staff_capability_grant_in_force(p_tenant uuid, p_staff uuid, p_capability text, p_at
  timestamptz) RETURNS boolean`. A NULL capability means "any financial capability".
- `financial_actor_session()` (§7.1).

### 10.2 Reference tables (family R; rows written by 0112 itself)

| Table | Columns | Rows |
|---|---|---|
| `financial_capability_catalogue` | `capability TEXT PK`, `operation_kind TEXT NOT NULL CHECK (operation_kind IN ('ledger_adjustment','payment_force_resolve'))` (0113 adds the FK to the classification), `action TEXT CHECK (action IN ('initiate','request','approve'))`, `eligible_tenant_roles TEXT[] NOT NULL`, `platform_grantee_allowed BOOLEAN NOT NULL` | the four §3.1 rows, `eligible_tenant_roles = '{finance}'`, `platform_grantee_allowed = true` |
| `financial_governance_permissions` | `permission TEXT PK`, `roles TEXT[] NOT NULL` | the seven §3.2 rows |
| `financial_capability_settings` | `key TEXT PK CHECK (key IN ('acting_grant_max_lifetime'))`, `value_interval INTERVAL NOT NULL CHECK (value_interval > interval '0')` | **No row is inserted by the ADR.** K1 inserts the technical security default in 0112 and records it in its implementation record (§8.2). With no row, G-P2 requests are refused (fail closed). |

### 10.3 `staff_capability_grant_requests` (families T, P; no A)

| Column | Type / constraint |
|---|---|
| `id` | `UUID PK DEFAULT gen_random_uuid()` |
| `tenant_id` | `UUID NOT NULL REFERENCES tenants(id)`; `UNIQUE (tenant_id, id)` |
| `grantee_staff_id` | `UUID NOT NULL REFERENCES staff_users(id)` |
| `grantee_scope` | `TEXT NOT NULL CHECK (grantee_scope IN ('tenant','platform'))`, derived by trigger |
| `capability` | `TEXT NOT NULL REFERENCES financial_capability_catalogue` |
| `valid_from` | `TIMESTAMPTZ NOT NULL` |
| `valid_until` | `TIMESTAMPTZ NULL`, `CHECK (valid_until IS NULL OR valid_until > valid_from)` |
| `reason_code` | `TEXT NOT NULL CHECK (octet_length(reason_code) BETWEEN 1 AND 64)` |
| `requested_by`, `requested_by_scope`, `requested_by_person_id` | forced (§7.2) |
| `status` | `TEXT NOT NULL CHECK (status IN ('pending','approved','rejected','cancelled','expired'))` |
| `created_at`, `expires_at` | `TIMESTAMPTZ NOT NULL`, forced |

- Index: partial UNIQUE `(tenant_id, grantee_staff_id, capability) WHERE status = 'pending'`
  (R-11).
- BEFORE INSERT/UPDATE trigger `staff_capability_grant_requests_guard`: R-1, R-4 (requester vs
  grantee), R-5, R-6, R-9, R-10, R-13 (G-P2); forced actor and scope; identity columns immutable;
  `pending → approved|rejected|cancelled|expired` only; terminal rows immutable.
- Policies:
  - T: SELECT, INSERT, UPDATE (cancel only, by trigger);
  - P: SELECT, INSERT, UPDATE (all tenants).

### 10.4 `staff_capability_grant_approvals` (families T read, P write; no A)

- **Columns:** `id`; `tenant_id`; `request_id` with FK `(tenant_id, request_id)` → requests; UNIQUE
  `request_id`; `decision TEXT CHECK (decision IN ('approve','reject'))`; `decided_by`,
  `decided_by_scope`, `decided_by_person_id` (forced); `decided_at`;
  `decided_txid BIGINT NOT NULL` (forced to `txid_current()`); `reason_code`.
- **Trigger `…_guard`:** R-2, R-3, R-4, R-7, R-8, R-12; the request is `pending` and unexpired; it
  moves the request to `approved` or `rejected` in the same statement's transaction.
- **Deferred constraint trigger:** an `approve` row commits only if a grant row with `approval_id`
  equal to it exists.
- Immutable.
- **Policies:** T: SELECT; P: SELECT, INSERT.

### 10.5 `staff_capability_grants` (families T, P, A-read)

- **Columns:** `id`; `tenant_id` with `UNIQUE (tenant_id, id)`; `grantee_staff_id`;
  `grantee_scope`; `capability`; `request_id UNIQUE`; `approval_id UNIQUE`; `valid_from`;
  `valid_until`; `granted_at`; `revoked_at`, `revoked_by`, `revoked_by_scope`,
  `revoke_reason_code`, all NULL until a revoke.
- **CHECK:** `grantee_scope = 'platform' ⇒ valid_until IS NOT NULL`.
- **Partial UNIQUE** `(tenant_id, grantee_staff_id, capability) WHERE revoked_at IS NULL`.
- **Triggers:**
  - INSERT only with an `approve` approval for `request_id` whose `decided_txid = txid_current()`;
    the columns are copied from and checked against the request;
  - one-way revoke under whole-row equality (§8.1); forced revoker; R-10;
  - DELETE and TRUNCATE refused.
- **Policies:**
  - T: SELECT, UPDATE (revoke);
  - P: SELECT, INSERT, UPDATE (revoke);
  - **A:** SELECT X rows (GUC-only, §6.3); UPDATE USING X rows WITH CHECK `false` (for `FOR SHARE`).

### 10.6 Policies and triggers on existing tables

- **The restrictive fence** of §6.4 on `staff_users`, `audit_log`, `sessions`, `login_attempts`,
  `persons`, `player_restrictions` and `risk_rules`.
- **Acting permissive policies** on `staff_users` and `audit_log` per §6.5.
- **Trigger `audit_log_acting_actor`** (BEFORE INSERT on `audit_log`; C-99-3). When
  `financial_acting_gucs_present()`, it forces:
  - `actor_type = 'staff'`;
  - `actor_id = acting principal`;
  - `tenant_id = acting tenant`;
  - `metadata = metadata || '{"actor_scope":"platform_acting"}'`.

  It refuses if `financial_acting_session_valid()` is false.

### 10.7 Down

Refuse (`CG099`) while any row exists in 0112's request, approval or grant tables. Otherwise drop, in
reverse order: the triggers and policies on existing tables (restoring their pre-0112 effective
policy sets exactly), then the new tables, then the functions.

## 11. Audit

Every grant request, approval, rejection, cancellation, expiry and revoke, every `grant.reattested`
action, and every acting session open writes `audit_log` in the same transaction, with:
- actor, actor scope, and acting tenant;
- target, grantee, capability and tenant;
- before and after state;
- reason code, IP, user agent and request id.

Scope rules:
- **Platform-session actions on X's grants:** `tenant_id NULL` + `subject_tenant_id = X` (0109; G1
  merges first).
- **Acting-session actions:** `tenant_id = X`, with the actor forced by trigger (§10.6) and marked
  `platform_acting` in the tenant presentation (HD-PRH2-5: identifiable actor and approval chain).
  IP, user agent and free-form metadata stay out of the tenant presentation by default.

## 12. Launch flags (not K1 blockers)

- **TM-7:** human risk acceptance of the two-colluding-platform-admins and `seed-admin` trust-root
  residual before real-money K operations.
- **TM-10:** LEGAL / COMPLIANCE REVIEW before any own-licence tenant receives a G-P2 grant.
- HD-PRH2-8 (ADR 0100 §3.4) must be answered.

## 13. Invariants

**Preserved:**
- **INV-DEP-1:** K1 adds no deposit path. The acting ledger fence refuses `deposit`.
- **Append-only double-entry:** K1 writes no ledger rows. Later writes go only through `ledger.Post`
  under the §6.6 fence.
- **DB idempotency:** R-11 and R-12 unique indexes; UNIQUE `request_id` and `approval_id`.
- **RLS:** FORCE RLS on every new table. The acting family is deny-by-default and restrictively
  fenced.
- **No direct balance mutation:** the §6.7 projection fence for acting sessions.
- **HD-LEDGER-UNALLOC-1 "A now, B later":** untouched.

**Introduced:**

| ID | Invariant |
|---|---|
| INV-CAP-1 | The static permission and an in-force grant are both required, read in-tx and locked `FOR SHARE` at execution |
| INV-CAP-2 | Every financial grant has a platform approver who is neither requester nor grantee; requester, approver and grantee have three distinct non-NULL Persons |
| INV-CAP-3 | Actor columns always come from the DB session |
| INV-CAP-4 | An acting session is fenced by §6.4–§6.7 and valid only with a grant for its tenant |
| INV-CAP-5 | Grants are append-only except for a one-way revoke |
| INV-CAP-6 | No HTTP API creates a `platform_admin`; `seed-admin` is the only non-test inserter |
| INV-CAP-7 | Grantees are `finance` staff (G-T/G-P1) or `platform_admin` (G-P2, time-bounded) only |

## 14. Tests and mutants (K1 DoD; QA W1)

Notes:
- T-1 clock; no new wall-clock assertion.
- Results are reported PASS / FAIL / FLAKE / NOT RUN / BLOCKED. Local runs are never labelled CI.

| ID | Class | Test |
|---|---|---|
| A-1 | AZ | Role × family × capability × action matrix, at HTTP and DB level |
| A-2 | ADV | **Sock-puppet refused:** tenant-minted accounts (any role) never hold a grant; a `compliance` grantee is refused (C-99-2); a tenant-scope approval is refused |
| A-3 | RLS | An acting session with a grant opens, audits, and (in K2/K3) completes a governed post |
| A-4 | RLS | **Acting negatives:** <br>• no grant, revoked or expired grant, or a grant for Y used on X; <br>• mixed sessions, including on `staff_users` and `staff_capability_grants` (C-1: every non-acting GUC must be unset); <br>• **K1-1 cases**: insert a `platform_admin`; read another platform staff row or `password_hash` of anyone but self; update or delete staff; insert or update a platform `sessions` row; read or write `login_attempts`; read any audit row, or insert a platform (`tenant_id NULL`) audit row; any `persons` access; insert a NULL-tenant `player_restrictions` row; write `risk_rules`; <br>• writes outside §6.5 (kill switch, bonus tables, a `player_accounts` UPDATE). <br>All refused. |
| A-4b | RLS | **Projection fence (LF F1):** an acting direct UPDATE, a `RebuildProjectionRow`, and a non-zero INSERT are refused; a zero-row ensure and a governed post succeed; drift = 0. This also proves `pg_trigger_depth()` semantics. |
| A-4c | RLS | **Ledger fence (LF F2):** a W′ correlation, a mismatched `provider_tx_id` or `idempotency_key`, a `casino_bet`, a `manual_adjustment` with no executing request, and entries appended to an old transaction are all refused under acting. This proves `txid_current()` under `ledger.Post`'s savepoint. |
| A-5 | AZ | Self-grant; self-approval; approver = grantee; one Person under two principals; a NULL Person. All refused. |
| A-6 | AZ | A tenant names a platform grantee or another tenant. Refused. |
| A-7 | AZ | G-P1/G-P2 with the same platform principal or Person. Refused. |
| A-8 | AZ | S-4: a suspended actor (status set by DB fixture) with a live token; a demoted grantee. Both refused. |
| A-9 | CON | **`FOR SHARE` (LF F3):** revoke commits first → not counted; execution first → the revoke waits, then applies. Expiry between approve and execute → not counted. |
| A-10 | R | G-P2 without `valid_until`, beyond the max lifetime, or with no settings row: refused. G-T without `valid_until`: allowed. |
| A-11 | IDM | Duplicate pending request and duplicate in-force grant: refused |
| A-12 | AU | One audit row per lifecycle action and per acting open. The acting audit actor is forced (a supplied different actor is overwritten). The tenant view marks `platform_acting`; IP and user agent are hidden. `grant.reattested` is recorded. |
| A-13 | R | Append-only; un-revoke, DELETE, TRUNCATE: refused |
| A-14 | R | `POST /staff` with `role = platform_admin` is refused (HTTP allowlist) |
| A-14b | R | Static: only `cmd/seed-admin` (plus allow-listed test support) inserts `platform_admin` |
| A-15 | R | The Go role map equals the catalogue role sets |
| A-16 | R | Static: no raw `set_config('app.acting_…')`; only the two packages call the setter; its P and X arguments come from the token subject and the route target |
| A-17 | MIG | 0112 up/down/up; down refuses with rows; after down, the effective policy sets on the seven fenced tables equal pre-0112 |
| A-18 | R | Static: migration replay finds no NULL-arm table without the restrictive acting fence |
| A-19 | R | Static column discipline (§6.8) |

**Mutants:**

| Mutant | Killed by |
|---|---|
| trust the supplied actor | §7.2 test |
| drop distinct-Person | A-5 |
| allow a tenant-scope approval | A-2 |
| allow `compliance` | A-2 |
| drop the in-tx status re-read | A-8 |
| drop `FOR SHARE` | A-9 |
| drop the grant function from one acting policy | A-4 |
| widen acting to any tenant | A-4 |
| drop one restrictive policy | A-4 (K1-1 cases) and A-18 |
| drop the ledger fence | A-4c |
| drop the projection fence or the depth check | A-4b |
| allow un-revoke | A-13 |
| drop R-13 | A-10 |
| drop the audit actor trigger | A-12 |

## 15. Alternatives rejected

| Alternative | Why rejected |
|---|---|
| Custom DB roles | Out of scope; a second authorization system |
| Grants in the JWT | Stale for up to 15 minutes; not revocable |
| Tenant-local grant approval (HD-PRH2-2 (b)) | Not chosen, and defeated by sock puppets |
| Grantable governance capabilities | No bootstrap |
| An acting session reusing `app.tenant_id` | Full tenant power |
| Plain platform family on ledger or K tables | Implicit access to every tenant ledger |
| `SECURITY DEFINER` helpers | The 0096 B7 precedent |
| Amending each exposed NULL-arm policy in place, instead of adding restrictive policies | Seven migrations' policies would be rewritten, and every future NULL arm would have to remember the acting case. `AS RESTRICTIVE` binds existing and future permissive arms, and A-18 guards new tables. |
| Keeping `compliance` eligible | K1-2 / security ruling 1 |
| Building the re-attestation table and view now | No lifecycle API can create the condition; PO Q2, security ruling 6 |
| A DB bootstrap guard for `platform_admin` in K1 | Adopted, but in STAFF-LIFECYCLE-1, after the 23 fixture files move to a helper (§9) |

## 16. Open items

1. **STAFF-LIFECYCLE-1** carries:
   - the suspend and role-change API, which must revoke grants in the same transaction (§7.5);
   - the re-attestation table, view and alert (§8.3);
   - the `platform_admin` bootstrap DB guard (§9);
   - the `finance` initial-password residual (§3.3).
2. **K1 Touches addition:** `internal/db/tenant_rls.go` (the setter). The orchestrator records it
   (Rule 1).
3. **Technical defaults to record in K1's implementation record:** the request TTL and
   `acting_grant_max_lifetime` (orchestrator decision: technical security defaults, listed for the
   human in the final report).
4. **Security "not covered" items that K1 verifies in code:** the `permission.go` role sets
   (A-15); login refusal for suspended tenants (out of K1 scope, noted); `persons` RLS (now fully
   fenced for acting, §6.4).

**HUMAN DECISION REQUIRED:** none new in this ADR. The launch flags in §12 need human risk
acceptance or legal review before real money; they are not new design decisions.

## 17. Handover / DoD

K1 updates:
- `docs/architecture/05-identity-architecture.md` (grants, eligible grantees, the S-1 caveat, the
  trust roots);
- `docs/architecture/03-database-architecture.md` (the acting family, the §6.2 table, the
  restrictive fence, A-18);
- `docs/security/security-architecture.md` (the §6.9 threat model, INV-CAP-1..7, the launch flags);
- `docs/architecture/36-backoffice-and-partner-console-architecture.md` (the grant screens);
- `backoffice/src/auth/permissions.ts`;
- `docs/runbooks/operational-runbooks.md`:
  - "Capability grants": the co-approval checklist including the out-of-band identity check;
    **revoke as the emergency stop**; `grant.reattested`;
  - "seed-admin: two-person audited procedure";
- `deploy/init-app-role.sql` (append-only grants; no write grant on the reference tables).

The orchestrator updates: the registry (CAP-GRANT-1, STAFF-LIFECYCLE-1 scope from §16.1), the
HANDOVER index, plan §3 Touches, and the review records.

## 18. Review disposition

| Finding | Where addressed |
|---|---|
| **Product-owner-proxy** | |
| Q1 (enum confirmed) | §3 header; §2.2 |
| Q2 re-attestation simplification | §8.3 (audit action; the table and view go to STAFF-LIFECYCLE-1) |
| Q2 `pending_suspense_allocation_b` | ADR 0101 §3 (not seeded) |
| Q3 HD-PRH2-8 | ADR 0100 §3.4; §12 |
| **Security** | |
| K1-1 / C-99-1 | §6.2 (corrected, enumerated by replay), §6.4, §6.9 TM-3/TM-6, §10.6, A-4, A-17, A-18 |
| K1-2 / C-99-2 / ruling 1 | §3.3, R-9, INV-CAP-7, A-2 |
| K1-3 / C-99-7 | §7.5, §8.1, §16.1, §17 runbook |
| K1-4 / C-99-3 | §10.6 `audit_log_acting_actor`, §11, A-12 |
| K1-5 / C-99-4 / ruling 7 | §9 INV-CAP-6, A-14b, the runbook; the DB guard goes to STAFF-LIFECYCLE-1 |
| K1-6 / C-99-5 / ruling 2 | §6.8, A-19 |
| C-99-6 / ruling 3 | §8.2, R-13, §10.2 settings, §10.5 CHECK, A-10 |
| C-99-8 | §6.1, A-16 |
| Ruling 4 (S-2(iii) approvers) | ADR 0100 §3.3 |
| Ruling 5 (non-active tenants) | ADR 0100 §6.7; ADR 0101 §6.5 |
| Ruling 6 (re-attestation) | §8.3 |
| Launch flags (TM-7, TM-10, HD-PRH2-8) | §12 |
| "Not covered" list | §16.4; §6.2 (0037/0041/0043 narrowing checked: 0043 narrowed by 0049; 0037 and 0041 not narrowed) |
| K2-*, K3-*, C-100-*, C-101-* | ADR 0100 §17, ADR 0101 §16 |
| **Ledger-finance** | |
| F1 (HIGH) | §6.7, A-4b |
| F2 | §6.6, A-4c; (d) in ADR 0101 §5.4 |
| F3 | §7.4, A-9; ADR 0100 §8 (A8) |
| Ruling 2 (acting SELECT for exposure) | §6.5 (`payment_attempts`, `deposit_intents` SELECT in 0113) |
| Ruling 6 | §7.4; ADR 0100 §8 |
| F4–F17, rulings 1, 3–5, 7 | ADR 0100 §17, ADR 0101 §16 |
| **Orchestrator decisions** | G-P2 lifetime and re-attestation defaults are technical security defaults: §8.2, §8.3, §16.3. Registry ids used: STAFF-LIFECYCLE-1. |

## 19. K1 implementation status (backend-engineer, PRH-2 K1 build)

**Status: PARTIALLY IMPLEMENTED.** Migration 0112 is applied, the DB-level
invariants are tested and mutation-tested, and a working (but not fully
audited by `security`) HTTP API exists for the G-T flow end to end. Several
items are explicitly deferred; see below.

### 19.1 What is IMPLEMENTED

- **Migration 0112** (`migrations/0112_scoped_financial_capability_grants.{up,down}.sql`):
  every §10 object - the functions (§10.1), the three reference tables with
  their seed rows (§10.2, including `acting_grant_max_lifetime = 4 hours`,
  the K1 technical security default), `staff_capability_grant_requests`
  (§10.3), `staff_capability_grant_approvals` (§10.4),
  `staff_capability_grants` (§10.5), the §6.4 restrictive fence and §6.5
  acting policies on the seven exposed tables plus `audit_log_acting_actor`
  (§10.6), and the §10.7 down migration (refuses with rows; reversible on an
  empty database; `go run ./cmd/migrate verify` is clean).
- **The sole setter**, `WithPlatformActingInTenant` (`internal/db/tenant_rls.go`).
  Static test A-16 (`internal/db/acting_setter_static_test.go`) pins it as the
  only place that sets the two acting GUCs, with a required negative control.
- **`internal/capability`**: the Go service layer (CreateRequest,
  CancelRequest, DecideAndGrant, RevokeGrant, ListRequests, ListGrants, error
  classification by `CG` SQLSTATE class). Deliberately thin - every real
  invariant lives in the migration's own triggers.
- **`internal/httpserver/capability_routes.go`**: the HTTP API (request,
  approve/reject, cancel, revoke, list requests, list grants), following the
  dual-scope `/v1/admin/tenants/{tenantID}/capability-grants/...` convention
  this codebase already uses for the payment kill switch and provider
  credentials. One line added to `routes.go` (`registerCapabilityRoutes`).
  **This HTTP layer has NOT been reviewed by `security` or `code-reviewer`**
  - see §19.3.
- **`internal/auth/permission.go`**: the seven §3.2 governance permissions
  added and wired into `RolePlatformAdmin`/`RoleTenantAdmin`/`RoleCompliance`/
  `RoleFinance` exactly matching the seeded `financial_governance_permissions`
  catalogue (test A-15 pins the two in agreement).
- **Tests** (`internal/db/capability_grant_integration_test.go`,
  `migration_0112_integration_test.go`, `acting_setter_static_test.go`,
  `acting_grant_fixture_integration_test.go`, plus
  `internal/identity/platform_admin_insert_static_test.go`): A-2 (both
  halves), A-4 (the K1-1 restrictive-fence cases, DB-level only - not the
  HTTP-level A-1 matrix), A-5, A-6, A-7, A-8, A-10, A-11, A-13, A-15, A-16,
  A-17 (both halves, using the "migrate only through 0112" scratch pattern),
  A-14b, the C-1 exact-GUC-shape predicate test, the setter's own
  no-valid-grant refusal test, and the required sock-puppet test. All pass
  under `-race -tags integration`.
- **Mutants**: `docs/plans/payment-readiness/evidence/prh2-k1-mutation-kill.txt`
  - 6 independently killed, 4 honestly disclosed as masked by an
  independent, equally-real control (not a gap), 0 undetected gaps.

### 19.2 A real design gap found and fixed during K1 (disclosed, not silently patched)

While building the G-T flow end to end, K1 found that migration 0011's
`dual_scope_isolation` policy leaves a **plain platform session
structurally blind to any tenant-scoped `staff_users` row** (independently
already documented by `newLinkPlatformStaffPersonHandler`'s own doc comment
in `admin_routes.go`). This breaks the platform approver's own R-4
distinct-Person check for a G-T grant (finance grantee, tenant-scoped) the
moment the approver is a plain platform session - there was no way for it
to read the grantee's `person_id` at all.

**Fix applied (no new tenant-isolation mechanism, no RLS widening):**
`staff_capability_grant_requests` gained a forced column,
`grantee_person_id`, captured from the grantee's own `staff_users` row at
**request-creation time** (when the actor - a tenant session for G-T, or a
platform session for G-P2 - can legitimately see it). The approval trigger
reads `v_req.grantee_person_id` instead of re-querying `staff_users`. This
fully unblocks G-T (tenant admin requests a `finance` grantee; a platform
principal approves) and G-P2 (platform grantee, itself visible to a plain
platform session via the pre-existing NULL-tenant arm).

**What remains BLOCKED / NOT IMPLEMENTED by this fix: G-P1** (a platform
session *requesting* a grant naming a **tenant-scoped** grantee, e.g. a
`finance` staff member, per §4's G-P1 row). At **request-creation** time a
platform session still cannot see the tenant-scoped grantee row at all, so
`staff_capability_grant_requests_guard`'s own grantee lookup fails
("grantee ... does not exist") before any of R-1/R-4/R-9 can even run. **This
needs an `architect` decision** - options include (a) a new,
column-restricted platform-wide SELECT view onto `staff_users` (id,
tenant_id, role, status, person_id only, mirroring the acting-session
column-discipline precedent in §6.8), (b) requiring G-P1 to be filed as a
tenant-side co-request instead, or (c) declaring G-P1 out of scope for PRH-2
and relying on G-T alone for tenant-scoped grantees. **G-T and G-P2 are
fully functional and tested end to end; G-P1 is NOT IMPLEMENTED.** No
tenant-isolation mechanism was invented or RLS policy widened to work
around this - per the backend-engineer role's own limits, this is escalated
rather than decided unilaterally.

### 19.3 Explicitly NOT DONE / deferred (honest labels)

- **`security` and `code-reviewer` review of this K1 HTTP/DB
  implementation**: NOT DONE. This is a security-critical financial-grant
  control surface and CLAUDE.md requires explicit `security` review before
  it is "complete" - this build is a candidate for that review, not a
  substitute for it.
- **A-1** (the full role × family × capability × action HTTP+DB matrix),
  **A-3** (acting session opens/audits, in the context of an actual K2/K3
  governed post - K1 has no governed post to complete), **A-4b/A-4c** (the
  projection and ledger fences - explicitly K2's migration 0113 content per
  ADR §6.6/§6.7, "not executed" here on purpose), **A-9** (the `FOR SHARE`
  concurrent-revoke-vs-execution race - there is no K1 execution path to
  race against yet; only file-level revoke-is-effective-next-read is
  implicitly exercised), **A-12** (the full audit-content/`platform_acting`
  tenant-presentation/`grant.reattested` test), **A-18** (the static
  migration-replay tool that proves no future NULL-arm table ships
  unfenced - a real, nontrivial SQL-parsing static analysis tool, not built
  in this session for time reasons; A-17's DB-level baseline check is a
  partial substitute, not a replacement), and **A-19** (column discipline -
  moot for K1 alone, since K1 code never reads `player_accounts` or ledger
  tables; applies to K2/K3) are **NOT IMPLEMENTED** in this K1 build.
- **`deploy/init-app-role.sql`**: UPDATED - the `igaming_runtime` grants for
  all six new K1 tables (SELECT-only on the three reference tables;
  SELECT/INSERT/UPDATE on `staff_capability_grant_requests`/
  `staff_capability_grants`; SELECT/INSERT on
  `staff_capability_grant_approvals`), mirroring the alerts-table pattern
  already in that file.
- **`docs/architecture/05-identity-architecture.md`,
  `03-database-architecture.md`, `docs/security/security-architecture.md`,
  `36-backoffice-and-partner-console-architecture.md`,
  `backoffice/src/auth/permissions.ts`, `docs/runbooks/operational-
  runbooks.md`**: **NOT UPDATED** in this session (time) - a real, disclosed
  documentation gap.
- The registry (CAP-GRANT-1), `docs/progress.md`, `docs/active-stage.md`,
  `docs/governance/task-registry.md` and the HANDOVER index are the
  orchestrator's to update, per this ADR's own §17 - proposed row text is in
  the K1 build's final report, not applied here.
