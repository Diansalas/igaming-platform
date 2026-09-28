# ADR 0099 — Scoped financial capability grants on the existing RBAC (PRH-2 K1)

- **Status:** PROPOSED (W0-K draft, `architect`, 2026-09-28). NOT IMPLEMENTED. K1 code may not start
  until `security` and `ledger-finance` have reviewed this ADR and the orchestrator records it as
  accepted (plan §11: "K may start only once the decisions below are incorporated into ADRs
  0099–0101 and verified by security and ledger-finance").
- **Decision type:** cross-domain architecture and security control (`auth`, `identity`, `db`,
  `httpserver`, the new `internal/capability`, and the consumers `internal/adjustment` (ADR 0100) and
  `payments` (ADR 0101)).
- **Owner:** `architect`. **Reviewers:** `security` (all of it), `ledger-finance` (financial
  invariants, the acting-family ledger fence), `qa` (§12), `code-reviewer`, `product-owner-proxy`
  (the closed enum, §2).
- **Registry:** CAP-GRANT-1 (the grant framework), HD-0095-1 and LEDGER-MANUAL-ADJ-4EYES-1
  (consumers). Workstream K1 of `docs/plans/prh2-hardening-round/plan.md`; migration 0111.
- **Binding inputs:**
  - ADR 0098 §1–§3 and §5: HD-PRH2-2 = (c) platform co-approval; HD-PRH2-6 = yes, explicit
    tenant-scoped grants only; HD-PRH2-5 (identifiable actor).
  - Plan §11 (overrides §5-K and §7 where they differ).
  - `reviews/security.md` S-1, S-3, S-4 and S-11; `reviews/ledger-finance.md` LF-11 and LF-14;
    `reviews/qa.md` (W1 K1 items); `reviews/product-owner-proxy.md` F4.
- **Related:** ADR 0011 (platform-scoped tokens), ADR 0013 (dual-scope RLS), ADR 0024 (Stage 3D
  withdrawal governance: in-tx eligibility re-read, platform-minted `finance`), ADR 0095 §10.2
  (kill-switch session resolver, migration 0105), migration 0096 (provider-credential four-eyes with
  distinct Person, no SECURITY DEFINER), ADR 0104 / migration 0109 (`subject_tenant_id` audit).
- **Nothing here is a regulatory or licensing claim.** The software capability described here is not
  legal approval of any operating model.

---

## 1. Context

ADR 0098 decided that force-resolve and manual adjustment are **configurable capabilities** assigned
to individual users, at platform and tenant scope, with full audit, actor/subject separation and
independent approval. It said to extend the existing RBAC rather than build a second authorization
system (0098 §3).

The existing RBAC (verified at `cabca27`):

| Fact | Where |
|---|---|
| A static, in-code role → permission map | `internal/auth/permission.go:583-880` |
| One role per staff user. `tenant_id IS NULL` ⇔ `platform_admin` | `migrations/0011:12,23-26` |
| Dual-scope RLS on `staff_users`: a session with `app.tenant_id` unset sees only platform staff | `0011:39-48` |
| `RequirePermission` reads the role from the JWT | `permission.go:899-914` |
| Access tokens live 15 minutes and cannot be revoked; role and status are re-checked only on refresh | `jwt.go:98-102`, `config.go:448`, `auth_routes.go:346-349` |
| Staff ↔ Person link is optional; Persons are platform-wide, unverified, and one is minted per NO_MATCH registration | `0029:12-18`, `0009:13-19`, `player_account.go:108-113` |
| A tenant caller can create `tenant_admin`, `support` and `compliance` staff, choosing the password and `person_id`. Only `finance`, `risk_manager`, `promotions_manager` and `bonus_operations` are platform-only. `platform_admin` is not in the creation allowlist at all | `admin_routes.go:491-535,550,656` |
| Platform session: `WithPlatformAdmin` sets only `app.platform_admin_principal_id`, never `app.tenant_id` | `db/tenant_rls.go:112-133` |
| The 0105 resolver raises on any mixed platform + tenant session | `0105:25-50` |
| Ledger tables are tenant-RLS on `app.tenant_id` only | `0021:65-68`, `0022:116-131`, `0023:72-87`, `0019`, `0020` |

A static role map cannot express "this user, in this tenant, until revoked". HD-PRH2-2 also requires
a grant to be co-approved by the platform, and HD-PRH2-6 requires that a platform principal can act
in a tenant's ledger only with an explicit grant for that tenant. Neither exists today.

## 2. Decision summary

1. **Two layers, both required.** A governed financial action needs (a) the **static permission** from
   the existing role map, checked at the route and re-checked in the transaction, **and** (b) an
   **in-force capability grant row**, read inside the action's own transaction. A JWT is never
   trusted for a grant.
2. **A closed, extensible capability enum** (§3). New capabilities are added only by migration plus
   ADR, never per tenant, and never as custom roles.
3. **Grant model per HD-PRH2-2 (c)** (§4). A tenant admin may *request* a grant for a user in its own
   tenant; the grant takes effect only after co-approval by an independent platform principal. A
   grant requested by a platform principal needs an independent second platform approver.
4. **A new, deny-by-default session family, "platform principal acting in tenant X"** (§6), valid
   only while the principal holds an in-force grant for X. It uses its own GUC names, so no existing
   tenant or platform RLS policy matches it. Policies for it exist only on the tables the governed
   executors need, and money-table writes under it are fenced by trigger.
5. **Actor derived from the DB session** (§7). Triggers derive the actor from the GUCs, re-read the
   actor's `staff_users` row in the transaction, and re-check every grant at execution (S-4).
6. **Lifecycle** (§8): one-way revoke; expiry; demoted or suspended grantors surfaced for
   re-attestation (S-11).
7. **The S-1 identity caveat is stated plainly** (§9). The distinct-Person checks are defence in
   depth. The structural control is that the co-approver is a platform principal, which no tenant
   caller can create.

## 3. The capability enum (closed, extensible)

### 3.1 Financial capabilities (grantable, always tenant-scoped)

| Capability | Operation kind (ADR 0100 §2 classification) | Used by |
|---|---|---|
| `ledger_adjustment:initiate` | `ledger_adjustment` | ADR 0100: submit a manual adjustment request |
| `ledger_adjustment:approve` | `ledger_adjustment` | ADR 0100: approve one |
| `payment_force_resolve:request` | `payment_force_resolve` | ADR 0101: request an M1 or M2 resolution |
| `payment_force_resolve:approve` | `payment_force_resolve` | ADR 0101: approve one |

These are the four capabilities from plan §5-K1. `product-owner-proxy` confirms the closed enum
(plan §2 W1 condition).

Every grant of a financial capability names exactly one tenant (`tenant_id NOT NULL`). **There is no
"all tenants" grant and no platform-wide financial grant.** The ledger is per tenant, so the only
meaning platform scope can have for a financial capability is "a platform principal holding a grant
for tenant X" (§6), which HD-PRH2-6 requires to be explicit.

### 3.2 Governance permissions (static RBAC, not grantable)

These administer the grants and policies. They are ordinary static permissions in the existing role
map, like `PermProviderCredentialApprove`:

| Permission | Roles | Meaning |
|---|---|---|
| `capability_grant:request` | `tenant_admin` (own tenant only), `platform_admin` | File a grant request |
| `capability_grant:approve` | `platform_admin` only | Co-approve a grant request (the HD-PRH2-2 platform co-approval) |
| `capability_grant:revoke` | `tenant_admin` (own tenant only), `platform_admin` | Revoke a grant (§8.1) |
| `capability_grant:read` | `tenant_admin`, `compliance`, `platform_admin` | List grants and requests |
| `financial_policy:author` | `platform_admin` only | ADR 0100 §3 platform policy authoring |
| `financial_policy:tighten` | `tenant_admin` | ADR 0100 §3 tenant/brand tightening |
| `financial_policy:read` | `tenant_admin`, `finance`, `compliance`, `platform_admin` | Read policies |

**Why the governance capabilities are static, not grantable (engineering decision, reversible).**
A grantable `capability_grant:approve` has no bootstrap: the first holder would need a grant that
nobody can yet approve, and the only ways out are a seeded person (forbidden) or a bypass. The
human's "platform principal with the grant-approval capability" is met by a static RBAC permission
held only by `platform_admin`, which is itself the Stage 3D "platform-minted" boundary: no API can
create a `platform_admin` (`admin_routes.go:491` allowlist). That boundary is load-bearing here, so
it becomes invariant **INV-CAP-6** with a pinning test (§12).

### 3.3 The static half of each financial capability

Each financial capability also has a static permission of the same name. It is held by the roles
**eligible** to be granted it. The grant is the authority; the role is only eligibility:

| Capability | Eligible grantee roles (proposed; `security` and `product-owner-proxy` confirm) |
|---|---|
| all four | `finance`, `compliance` (tenant staff of tenant X); `platform_admin` (only via a grant for tenant X, §6) |

`tenant_admin` is **never** an eligible grantee. This carries forward the Stage 3D rule (ADR 0024,
`permission.go:651-674`): one account must not hold both staff administration and money authority.
The eligible-role set is a column of the catalogue (§10.1), so a trigger re-checks it at execution
(a demoted grantee stops qualifying mid-token).

### 3.4 Extensibility rule

A new capability is added by (1) a migration that adds a catalogue row and, if it is financial, its
operation kind to ADR 0100's classification, (2) an ADR that names it, and (3) the static
permission. Nothing is added at runtime. The application role has `SELECT` only on the catalogue.

## 4. The grant model (HD-PRH2-2 = (c))

### 4.1 Flows

| Flow | Requester | Required approval | Effect |
|---|---|---|---|
| **G-T:** tenant-originated grant for a tenant user | a `tenant_admin` of tenant X with `capability_grant:request`, in a tenant session for X | **one** platform principal with `capability_grant:approve`, in a platform session | The grant row is inserted in the approval transaction and is in force from then |
| **G-P1:** platform-originated grant for a tenant user of X | a `platform_admin` with `capability_grant:request` | **a second, independent** `platform_admin` with `capability_grant:approve` | As above |
| **G-P2:** platform-originated grant for a platform principal in tenant X (HD-PRH2-6) | a `platform_admin` with `capability_grant:request` | **a second, independent** `platform_admin` with `capability_grant:approve` | As above; the grantee may then open an acting session for X (§6) |

A request is `pending → approved | rejected | cancelled | expired`. It has a bounded TTL, forced by
the trigger. The value is a technical default recorded in K1's implementation record; 0105 uses
24 hours. It is not a monetary threshold. Approval is a single row in the approvals table, and the
grant row is inserted in the **same transaction** (`decided_txid = txid_current()`, the 0105
pattern at `0105:162,324`).

### 4.2 Always refused (enforced by trigger, and mirrored in Go for legible errors)

| Rule | Check |
|---|---|
| **R-1:** no self-grant | `grantee_staff_id <> requested_by` |
| **R-2:** no self-approval | `approved_by <> requested_by` |
| **R-3:** the approver is neither the requester nor the grantee | `approved_by NOT IN (requested_by, grantee_staff_id)` |
| **R-4:** distinct Persons | requester, approver and grantee each have a non-NULL `person_id`, and the three are pairwise distinct (§9) |
| **R-5:** a tenant requester stays in its tenant | a tenant-session requester's `tenant_id` = the request's `tenant_id` = the grantee's `tenant_id`. RLS enforces the first equality; the trigger enforces the second |
| **R-6:** a tenant admin never grants platform scope | a tenant-session requester may not name a grantee with `staff_users.tenant_id IS NULL` (G-P2 is platform-only) and may not name a governance permission (§3.2 is not in the grantable catalogue at all) |
| **R-7:** the approval is platform, always | the approval row's derived scope must be `platform`. A tenant-session approval is refused |
| **R-8:** G-P1/G-P2 need two platform principals | requester scope `platform` ⇒ the approver is a *different* platform principal with a *different* Person |
| **R-9:** eligibility | the grantee's role is in the capability's eligible set (§3.3); the grantee is `active`; a platform grantee is only allowed for a G-P2 request |
| **R-10:** no acting session administers grants | a session of the §6 family can neither request, approve nor revoke a grant |
| **R-11:** one open request per `(tenant, grantee, capability)` | partial UNIQUE index on `status = 'pending'` |
| **R-12:** no duplicate in-force grant | partial UNIQUE on `(tenant_id, grantee_staff_id, capability)` where not revoked. Expiry is time-based, so the approval trigger also refuses a request while an unexpired grant exists |

## 5. Scopes and what each session may do

| Session family | GUCs set | May |
|---|---|---|
| **tenant** | `app.tenant_id = X`, `app.principal_id = S` (opened with `db.WithPrincipalScope`, as the kill switch does; plain `WithTenant` sets no principal and is refused by the resolver) | request (G-T), revoke and read grants in X; act on K2/K3 rows in X with its own grants |
| **platform** | `app.platform_admin_principal_id = P` only | request (G-P1/G-P2), approve, revoke and read grants in any tenant. **It has no K2/K3 financial access.** |
| **platform acting in X** (§6) | `app.acting_tenant_id = X`, `app.acting_platform_principal_id = P` only | act on K2/K3 rows in X with P's grants for X; nothing else |
| player / service / mixed | anything else | nothing on any 0111/0112/0114 table |

HD-PRH2-6's interim rule, "refuse platform scope on tenant ledgers", is replaced by **"refuse unless
an explicit in-force grant for that tenant exists"**. That is implemented as: the plain platform
family has no policy on any tenant ledger or K2/K3 table, and the acting family requires the grant.

## 6. The "platform principal acting in tenant X" session family (HD-PRH2-6, S-3)

### 6.1 Shape

- **GUCs:** `app.acting_tenant_id = X` and `app.acting_platform_principal_id = P`. `app.tenant_id`,
  `app.principal_id`, `app.platform_admin_principal_id` and `app.player_account_id` must all be
  unset.
- **Sole setter:** a new `db.WithPlatformActingInTenant(ctx, principalID, tenantID, fn)` in
  `internal/db/tenant_rls.go`. As its first statement it calls
  `financial_acting_session_open()`, which raises unless P is an active platform staff row (visible
  through `staff_users`' platform branch, `0011:43,47`, because `app.tenant_id` is unset) holding at
  least one in-force financial grant for X. P comes from the verified token subject; X comes from the
  route-validated target (`canActOnTenant`), never a body field.
- **Callers:** only `internal/adjustment` (ADR 0100) and `payments/manual_resolution.go` (ADR 0101).
  A static/AST test pins both the caller set and the rule that no other code sets any `app.acting_*`
  GUC.

### 6.2 Why new GUC names (deny by default)

Every existing tenant policy keys on `app.tenant_id`, and every existing platform policy requires
`app.tenant_id` unset **and** `app.platform_admin_principal_id` set. An acting session sets neither,
so **no existing policy on any table matches it**. It sees and writes nothing unless a migration
adds an explicit acting-family policy. The 0105 resolver also raises for it
(`app.tenant_id` unset and `app.platform_admin_principal_id` unset ⇒ "unresolvable"), so the kill
switch is untouchable from it.

### 6.3 The acting-family policy predicate

Every acting-family policy is:

```sql
tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid
AND financial_acting_grant_in_force()
```

`financial_acting_grant_in_force()` is `STABLE`, **not** `SECURITY DEFINER` (the 0096 B7 rule). It
checks that:
- exactly the two acting GUCs are set and the four others are unset;
- the acting principal is an active platform staff row;
- an in-force (§8), unrevoked grant of any financial capability exists for `(acting tenant, acting
  principal)`.

It reads `staff_capability_grants` through that table's own acting read policy, which is the only
acting policy without the function call: `tenant_id = acting tenant AND grantee_staff_id = acting
principal`. That avoids policy recursion. The **specific** capability (for example
`ledger_adjustment:approve`) is checked by the operation's own trigger (ADR 0100 §6, ADR 0101 §5).

### 6.4 The tables that get an acting-family policy (closed list)

| Table | Acting access | Added by | Why |
|---|---|---|---|
| `staff_capability_grants` | SELECT own grants (§6.3) | 0111 | grant validation |
| `staff_users` | SELECT, rows `tenant_id = X` only | 0112 | S-4 re-read of tenant initiators and approvers; distinct-Person |
| `player_accounts` | SELECT, tenant X | 0112 | S-12 beneficiary Person |
| `financial_approval_policies` and ADR 0100's other 0112 tables | per ADR 0100 §9 | 0112 | K2 |
| `wallets`, `ledger_accounts` | SELECT; `ledger_accounts` also INSERT (`GetOrCreateAccount`) | 0112 | the governed post |
| `ledger_transactions`, `ledger_entries` | SELECT, INSERT, **fenced (§6.5)** | 0112 | the governed post |
| `wallet_balance_projection` | SELECT, INSERT, UPDATE (the 0023 trigger's upsert, `LockProjectionsForPosting`) | 0112 | the governed post |
| `audit_log` | INSERT with `tenant_id = X` | 0112 | audit of acting actions (§11) |
| `payment_attempts`, `deposit_intents`, `withdrawal_requests`, and every further table `withdrawal.Complete`/`Fail` touch | SELECT; UPDATE only where ADR 0101 §5 allows; fenced | 0114 | K3 |
| ADR 0101's 0114 tables | per ADR 0101 §8 | 0114 | K3 |

**K2 and K3 must each enumerate, from the code, every table and trigger that `ledger.Post`,
`LockProjectionsForPosting` and `withdrawal.Complete`/`Fail` touch.** Any table missing from this
list makes the governed post fail closed under an acting session. That is safe, but it must be found
by the positive integration test (§12, A-3) before merge, not in operation. Adding a table beyond
this list needs an amendment to this section.

### 6.5 The money-write fence

A `BEFORE INSERT` trigger on `ledger_transactions`, and the same predicate evaluated for the parent
transaction of each `ledger_entries` insert, does the following **when the session is an acting
session**. It refuses unless the posting is governed by a request or resolution **executing in this
transaction**:
- `transaction_type = 'manual_adjustment'` ⇒ an ADR 0100 request whose `idempotency_key` matches and
  whose `executed_txid = txid_current()`;
- `transaction_type IN ('withdrawal_completed', 'withdrawal_failed')` ⇒ an ADR 0101 M2 resolution
  for that withdrawal whose `executed_txid = txid_current()`;
- anything else is refused.

The trigger is a no-op for any other session family, so existing flows are byte-identical in
behaviour. `ledger-finance` owns the exact predicate, which is written in 0112 and extended in 0114.

### 6.6 Threat model (required before K1 code, S-3)

| # | Threat | Control | Residual |
|---|---|---|---|
| TM-1 | A platform principal reaches a tenant ledger **without** a grant ("employment is authority") | No existing policy matches (§6.2). Every acting policy calls `financial_acting_grant_in_force()`. The session opener raises. | None known |
| TM-2 | A grant for X is used on tenant Y | Every acting predicate compares `tenant_id` with `app.acting_tenant_id`; grant validation is per `(X, P)` | None known |
| TM-3 | Lateral use of an acting session for non-financial work in X (read PII, flip a kill switch, write bonus rows) | Deny by default; the closed table list (§6.4); the kill-switch resolver raises; no acting policy on any other table | **Reads** on the listed identity tables (`staff_users`, `player_accounts`) are broader than a single request needs. Mitigated by the caller allowlist test. `security` rules on whether per-row narrowing is required |
| TM-4 | A non-governed money write under an acting session (e.g. a `casino_bet` posting) | The §6.5 fence | Direct `wallet_balance_projection` UPDATE by acting code is not fenced. That is the same exposure every tenant session has today, and the hourly drift check detects it |
| TM-5 | GUC injection or forgery | The sole setter (§6.1) and a static test; P from the verified token, X from the route; the validator re-reads `staff_users` | A future code path that runs raw `set_config`. Pinned by the static test |
| TM-6 | A mixed session (acting plus tenant or platform) | The validator raises if any other scope GUC is set. Existing policies require their own GUC set and others unset (0105/0106 pattern) | None known |
| TM-7 | A compromised single platform account | It needs its own grant for X (G-P2, which needs a second platform human), and every operation needs an independent approver with a distinct Person (ADR 0100/0101) | Two colluding platform admins can grant each other and then initiate and approve. Detective controls: tenant-visible grants and audit with identifiable actor (HD-PRH2-5); `pay_captured_unposted`, drift and reconciliation |
| TM-8 | A stale JWT after suspension or demotion | The in-tx `staff_users` re-read and the execution-time re-check (§7) | None known |
| TM-9 | A platform principal approves its own acting grant | R-2, R-3, R-4 | None known |
| TM-10 | Own-licence tenants: regulatory effect of platform staff acting in the operator's ledger | This is not an engineering control | **LEGAL / COMPLIANCE REVIEW REQUIRED** per operator and jurisdiction before a real B2B own-licence tenant enables a G-P2 grant. The software capability is not legal approval |

## 7. Actor derivation and in-transaction re-checks (S-4)

1. **One resolver.** 0111 adds `financial_actor_session()`, returning `(actor, scope, tenant,
   person_id)`. It generalizes `payment_kill_switch_session()` (`0105:25-50`) with the acting shape:
   - **tenant:** `app.tenant_id` and `app.principal_id` set, others unset; the actor is a staff row
     of that tenant.
   - **platform:** only `app.platform_admin_principal_id` set; a staff row with `tenant_id IS NULL`.
   - **platform acting:** §6.1.
   - Anything else raises.

   In every case the staff row must be `status = 'active'`.
2. **Actor columns are forced, never accepted.** Every `requested_by`, `approved_by`,
   `initiated_by`, `revoked_by` and `*_scope` column on a 0111/0112/0114 table is overwritten by the
   trigger from the resolver, as `0105:88-89` does. A client- or Go-supplied value is ignored. A
   test asserts that a mismatching supplied value never persists.
3. **The static permission is re-checked in the transaction.** The route checks the JWT role, as
   today. The trigger re-reads the actor's current `role` and checks it against the catalogue's
   eligible roles or the governance permission's role set. The DB copy of the role sets for §3.2 and
   §3.3 lives in the catalogue (§10.1). K1 adds a Go test asserting the Go map and the catalogue
   agree.
4. **Execution-time re-check.** At execution (ADR 0100 §6, ADR 0101 §5), for the initiator and
   **every approval being counted**: the staff row is active; the role is still eligible; the grant
   is in force at `now()`; the grant is unrevoked. A failing approval **does not count**. It is not
   deleted and stays in the record.
5. **Precedent:** `withdrawal_handlers.go:537-552` (ADR 0024 §1) and `0105:25-50`.

## 8. Lifecycle (S-11)

### 8.1 Revoke

- A single authorized actor can revoke, because revoking only reduces power (the 0096 "disabling
  takes one" precedent). The actor is a `tenant_admin` of X for any grant in X, including a G-P2
  grant held by a platform principal, or a `platform_admin` for any tenant. The platform can
  re-grant through G-P2, so a tenant revoke is never a veto.
- Revoke is one-way. `UPDATE` is allowed only to set `revoked_at`, `revoked_by`, `revoked_by_scope`
  and `revoke_reason_code` from NULL, with **whole-row equality** on every other column
  (`(to_jsonb(NEW) - <revoke columns>) = (to_jsonb(OLD) - <revoke columns>)`, the 0108 pattern). No
  un-revoke. `DELETE` and `TRUNCATE` are refused.
- It takes effect at the next in-transaction read. No token revocation is needed.
- Pending requests or approvals that relied on the grant are voided by the execution-time re-check
  (§7.4). No sweeper is involved.

### 8.2 Expiry

- `valid_until` is optional and fixed at request time. A grant is in force while
  `valid_from <= now() < COALESCE(valid_until, 'infinity')` and it is unrevoked.
- If a grant expires between an approval and the execution, that approval does not count and the
  execution is refused or waits (§7.4). The QA CON test covers this, using the T-1 injectable clock
  or fixture timestamps.
- A mandatory maximum lifetime for G-P2 (acting) grants is an open item for `security` (§13). No
  value is set here.

### 8.3 Demoted or suspended grantor

- A grant **stays in force** when its requester or approver is later suspended, demoted or removed.
  Voiding silently would strip unrelated users' authority.
- It is **surfaced for re-attestation**:
  - the view `staff_capability_grants_needing_reattestation` lists in-force grants whose requester
    or approver is no longer `active` or no longer holds the governance permission's role;
  - a platform principal (or a tenant admin of X) either revokes the grant or records an attestation
    in the append-only `staff_capability_grant_attestations` table, which removes the grant from the
    view.
- No deadline is set here (§13).

### 8.4 Grantee changes

- A suspended grantee cannot use a grant: §7.4 checks the grantee's status at use.
- A demoted grantee fails the eligible-role check. The grant row is not changed.

## 9. Identity caveat (S-1), stated plainly

**Under the current, unverified identity model, distinct-principal and distinct-Person checks do not
prove two humans.**
- A `tenant_admin` can create `tenant_admin`, `support` and `compliance` staff with a password and
  `person_id` of its choosing (`admin_routes.go:491-550`), or link a Person later (`:656`).
- Persons are unverified and self-registered (`0009:13-19`), and one is minted per NO_MATCH player
  registration (`player_account.go:108-113`).

So one human can hold any number of "distinct" Persons.

**What structurally prevents one tenant human from moving money alone** is HD-PRH2-2 (c):
- every financial grant needs approval by a platform principal (R-7);
- no tenant caller can create a platform principal (INV-CAP-6);
- so a tenant admin cannot make an account that holds a grant without a platform human agreeing.

The ADR 0100/0101 operations then require an independent approver, which is itself a granted,
platform-co-approved user.

**What the distinct-Person checks add:**
- defence in depth against accidental self-dealing;
- the 0029 beneficiary exclusion;
- the "same Person, two principals" case, refused under every configuration.

A person link counts as proof of a distinct human only once verified staff identity exists.
`persons.status = 'verified'` means something only with real KYC. **Every K actor must have a
non-NULL `person_id`** (the 0024 §1 / 0096 precedent), and an unlinked actor is refused.

Residual: **the platform co-approver is trusted to verify, out of band, that a tenant's requested
grantee is a real, distinct person.** That is an operational control. The runbook (Handover/DoD)
states it. It is not a DB property.

## 10. Migration 0111 (K1) — tables and RLS

All tables: `tenant_id` where tenant-owned; `ENABLE` **and `FORCE ROW LEVEL SECURITY`**; no `FOR ALL`
policy; `BEFORE TRUNCATE` deny (`ledger_deny_mutation()`); no `SECURITY DEFINER`; dedicated SQLSTATE
class **`CG`** for every RAISE (the 0096 convention), so Go classifies by code, never by message
text.

### 10.1 `financial_capability_catalogue` (platform reference data)

- **Columns:** `capability` (PK), `operation_kind` (`ledger_adjustment` | `payment_force_resolve`;
  0112 adds the FK to ADR 0100's classification), `action` (`initiate` | `approve` | `request`),
  `eligible_tenant_roles TEXT[]`, `platform_grantee_allowed BOOLEAN`, `created_by_migration TEXT`.
- **Content:** the four §3.1 rows, written by 0111 itself. They are enum members, not people and not
  thresholds.
- **RLS family: reference/read-all.** SELECT for any non-player session. **No write policy**, and
  the app role has no INSERT/UPDATE/DELETE grant (`deploy/init-app-role.sql`, append-only grants per
  plan Rule 4).
- The §3.2 governance-permission role sets live in a sibling reference table
  `financial_governance_permissions` of the same shape and RLS family.

### 10.2 `staff_capability_grant_requests`

- **Columns:** `id`, `tenant_id NOT NULL`, `grantee_staff_id` (FK `staff_users`), `capability` (FK
  catalogue), `valid_from`, `valid_until NULL`, `reason_code` (closed), `requested_by`,
  `requested_by_scope`, `requested_by_person_id`, `status`, `created_at`, `expires_at`.
- **Triggers:** force the actor (§7.1-2); R-1, R-4 (requester vs grantee), R-5, R-6, R-9, R-10, R-11;
  identity columns immutable; `pending → approved | rejected | cancelled | expired` only; terminal
  rows fully immutable.
- **RLS families:**
  - **tenant:** `tenant_id = app.tenant_id`, with principal set and no player/platform/acting GUC;
    SELECT, INSERT, UPDATE (cancel only, trigger-restricted);
  - **platform:** validated platform GUC, `app.tenant_id` unset; SELECT, INSERT, UPDATE across
    tenants (the 0105 `platform_scope` shape, since grant approval is a platform function);
  - **no acting family.**

### 10.3 `staff_capability_grant_approvals`

- **Columns:** `id`, `tenant_id`, `request_id` (composite FK `(tenant_id, request_id)`), `decision`
  (`approve` | `reject`), `decided_by`, `decided_by_scope`, `decided_by_person_id`, `decided_at`,
  `decided_txid`, `reason_code`.
- **Triggers:** force the actor; R-2, R-3, R-4, R-7, R-8, R-12; the request must be `pending` and
  unexpired; on `approve`, the same statement's transaction must insert the grant (checked by a
  constraint trigger on the grant, §10.4). Rows are immutable.
- **RLS families:** tenant (SELECT only); platform (SELECT, INSERT). No acting family.

### 10.4 `staff_capability_grants`

- **Columns:** `id`, `tenant_id`, `grantee_staff_id`, `grantee_scope` (`tenant` | `platform`),
  `capability`, `request_id` UNIQUE, `approval_id` UNIQUE, `valid_from`, `valid_until`, `granted_at`,
  `revoked_at`, `revoked_by`, `revoked_by_scope`, `revoke_reason_code`.
- **Triggers:**
  - INSERT only with an `approve` approval for this request whose `decided_txid = txid_current()`;
    the columns are copied from the request, and the trigger checks this;
  - one-way revoke under whole-row equality (§8.1);
  - DELETE and TRUNCATE refused.
- **RLS families:**
  - tenant: SELECT; UPDATE (revoke only);
  - platform: SELECT, INSERT, UPDATE (revoke only);
  - **acting: SELECT, own grants only** (§6.3).

### 10.5 `staff_capability_grant_attestations`

- **Columns:** `id`, `tenant_id`, `grant_id`, `attested_by`, `attested_by_scope`, `attested_at`,
  `note_code`.
- Append-only. **RLS families:** tenant and platform (SELECT, INSERT). No acting family.

### 10.6 Functions and view

- `financial_actor_session()` (§7.1)
- `financial_acting_session_open()` (§6.1)
- `financial_acting_grant_in_force()` (§6.3)
- the view `staff_capability_grants_needing_reattestation` (§8.3), `security_invoker = true`, so it
  inherits the caller's RLS

### 10.7 Down migration

Refuse while any row exists in any 0111 table. Otherwise drop in reverse order.

## 11. Audit

- Every grant request, approval, rejection, cancellation, expiry, revoke and attestation writes an
  `audit_log` record **in the same transaction** (`audit.Record`), with:
  - actor, actor scope, and acting tenant where relevant;
  - target (`staff_capability_grant*` id), grantee, capability, tenant;
  - before and after state;
  - reason code, IP, user agent and request id.
- **Platform-session actions on tenant X's grants:** `tenant_id NULL` plus `subject_tenant_id = X`
  (the ADR 0104 / 0109 pattern). This depends on G1 merging first; that holds in the nominal merge
  order (plan §3).
- **Acting-session actions:** `tenant_id = X`, with `actor_id = P` and `actor_scope =
  platform_acting` in metadata.
- Per HD-PRH2-5, the tenant sees the identifiable platform actor (staff id and display name) and
  the approval chain. IP, user agent and free-form metadata stay out of the tenant **presentation**
  by default (the 0098 §5 interpretation), and stay in the record.
- Grant state is also reconstructable from the append-only grant tables themselves. The audit log
  is not the only record.

## 12. Tests and mutants (K1 DoD; QA W1 checklist)

Notes:
- Clock-dependent cases use the T-1 injectable clock or fixture timestamps; there are no new
  wall-clock assertions (T-2).
- Results are reported PASS / FAIL / FLAKE / NOT RUN / BLOCKED. Local runs are never labelled CI.

| ID | Class | Test |
|---|---|---|
| A-1 | AZ | The full matrix role × session family × capability × action, both through HTTP and at the DB layer |
| A-2 | ADV | **The sock-puppet case is refused.** A tenant admin mints two `compliance` accounts linked to two self-registered Persons and requests `initiate` for one and `approve` for the other. Neither grant is ever in force without a platform approval. A tenant-session approval is refused (R-7). |
| A-3 | RLS | **Acting family, positive:** with a grant for X, a full governed post and the audit row succeed (run in K2/K3 against the §6.4 list) |
| A-4 | RLS | **Acting family, negative:** without a grant; with a revoked or expired grant; with a grant for Y used on X; with a mixed session (acting + `app.tenant_id`, acting + platform GUC); a write to a table outside §6.4 (`payment_kill_switches`, a bonus table, a `player_accounts` UPDATE); a non-governed posting (`casino_bet`, a `manual_adjustment` with no executing request). All refused. |
| A-5 | AZ | Self-grant; self-approval; approver = grantee; same Person under two principals; an unlinked Person for requester, approver or grantee. All refused. |
| A-6 | AZ | A tenant admin names a platform grantee, names a governance permission, or targets another tenant (via RLS and via trigger). All refused. |
| A-7 | AZ | G-P1/G-P2 with the same platform principal, or the same Person, as requester and approver. Refused. |
| A-8 | AZ | S-4: a suspended actor with an unexpired token is refused at request, approval and use. A grantee demoted mid-token is refused. |
| A-9 | CON | Revoke racing an execution: exactly one outcome, and a revoked grant's approval is never counted after the revoke commits. Expiry between approve and execute: refused. |
| A-10 | R | A demoted or suspended grantor: the grant stays in force and appears in the re-attestation view; an attestation removes it from the view |
| A-11 | IDM | A duplicate pending request and a duplicate in-force grant are refused by the unique indexes |
| A-12 | AU | Every lifecycle action writes exactly one audit row with the right `tenant_id` / `subject_tenant_id`. The tenant projection shows the platform actor and never IP or user agent. |
| A-13 | R | Append-only: UPDATE other than revoke, un-revoke, DELETE and TRUNCATE on every 0111 table are refused |
| A-14 | R | **INV-CAP-6 pin:** `POST /staff` with `role = platform_admin` from any caller is refused (the allowlist test) |
| A-15 | R | The Go role map and the catalogue's role sets agree |
| A-16 | R | Static: only `db.WithPlatformActingInTenant` sets `app.acting_*`; only the two named packages call it |
| A-17 | MIG | 0111 up/down/up; down refuses while rows exist |

**Mutants (each must be killed by a named test):**

| Mutant | Killed by |
|---|---|
| M-1: trust the supplied `requested_by` / `approved_by` instead of the GUC | §7.2 test |
| M-2: drop the distinct-Person check | A-5 |
| M-3: allow a tenant-scope approval | A-2 |
| M-4: drop the in-tx `staff_users` status re-read | A-8 |
| M-5: drop `financial_acting_grant_in_force()` from one acting policy | A-4 |
| M-6: widen an acting policy to any tenant | A-4 (Y on X) |
| M-7: drop the §6.5 fence | A-4 non-governed posting |
| M-8: allow un-revoke | A-13 |
| M-9: count an approval whose grant has expired | A-9 |
| M-10: allow a platform grantee on G-T | A-6 |

## 13. Invariants preserved and introduced

**Preserved:**
- **INV-DEP-1:** untouched. 0111 adds no path to a deposit posting, and the §6.5 fence refuses
  `deposit` under an acting session.
- **Append-only double-entry:** no ledger writes in K1. Later ledger writes go only through
  `ledger.Post`.
- **DB idempotency:** unique indexes R-11 and R-12; `request_id` and `approval_id` UNIQUE on grants.
- **RLS:** FORCE RLS on every new table. The new family is deny-by-default and grant-gated. Tenant
  id comes only from server context.
- **No direct balance mutation:** K1 writes no balance.
- **HD-LEDGER-UNALLOC-1 "A now, B later":** untouched. No capability here posts for a disputed
  deposit (ADR 0101 §3).

**Introduced (for `qa` and `code-reviewer`):**

| ID | Invariant |
|---|---|
| INV-CAP-1 | A governed financial action needs the static permission and an in-force grant read in-tx |
| INV-CAP-2 | Every financial grant has one approval by a platform principal who is neither the requester nor the grantee, and all three have distinct non-NULL Persons |
| INV-CAP-3 | Actor columns are always derived from the DB session |
| INV-CAP-4 | An acting session sees and writes only the §6.4 tables, only for its tenant, only while it holds a grant; its ledger writes are fenced (§6.5) |
| INV-CAP-5 | Grant rows are append-only apart from a one-way revoke |
| INV-CAP-6 | No API creates a `platform_admin` |

## 14. Alternatives rejected

| Alternative | Why rejected |
|---|---|
| Custom DB-driven roles | Out of scope (plan §8); a second authorization system, contrary to 0098 §3 |
| Grants in the JWT | Stale for up to 15 minutes and not revocable (S-4) |
| Tenant-local second approver for grants (HD-PRH2-2 (b)) | Not chosen by the human, and defeated by sock puppets (S-1) |
| Grantable governance capabilities | No bootstrap without seeded people or a bypass (§3.2) |
| An acting session that reuses `app.tenant_id = X` plus a marker GUC | Every existing tenant policy on every table would match it. That is full tenant power, where S-3 asked for the named tables only. |
| Give the plain platform family (0105 `platform_scope` shape) access to K2/K3 and ledger tables | Implicit access to every tenant ledger, which HD-PRH2-6 forbids |
| `SECURITY DEFINER` lookup helpers | 0096 B7 precedent. The needed reads are expressible as narrow policies. |
| Void grants when the grantor is suspended | Silently strips unrelated users. S-11 recommends re-attestation. |

## 15. Open items

1. **Eligible grantee roles** (§3.3: `finance` and `compliance`, plus `platform_admin` via G-P2).
   `security` and `product-owner-proxy` confirm.
2. **The breadth of acting reads** on `staff_users` and `player_accounts` (TM-3). `security` rules
   whether per-row narrowing is required.
3. **Maximum lifetime for G-P2 acting grants**, and a **re-attestation deadline** (§8.2, §8.3).
   `security` recommends; no value is set here. If a fixed value is required, it goes to the human.
4. **Tenant notification** when a G-P2 grant for its tenant is approved. The row and audit are
   tenant-visible already; push notification depends on ADR 0102 routing (HD-PRH2-4-OPS).
5. **K1 Touches addition:** `internal/db/tenant_rls.go` (the sole setter, §6.1) is not in plan §3's
   K1 row. The orchestrator adds it (Rule 1).
6. **TM-10:** LEGAL / COMPLIANCE REVIEW REQUIRED before any own-licence B2B tenant has a G-P2 grant.
   This is a review flag, not a new decision.

**HUMAN DECISION REQUIRED:** none in this ADR. HD-PRH2-2, -5 and -6 are applied as decided. The
threshold-semantics question is raised in ADR 0100 §3.4.

## 16. Handover / DoD

K1 is not done until it updates:
- `docs/architecture/05-identity-architecture.md` (the capability grants section, and the S-1
  caveat);
- `docs/architecture/03-database-architecture.md` (the acting session family and its closed table
  list);
- `docs/security/security-architecture.md` (the threat model in §6.6, INV-CAP-1..6);
- `docs/architecture/36-backoffice-and-partner-console-architecture.md` (grant request, approval and
  revoke screens);
- `backoffice/src/auth/permissions.ts`;
- `docs/runbooks/operational-runbooks.md` (new entry "Capability grants": co-approval checklist,
  including the out-of-band identity check in §9; revoke; re-attestation);
- `deploy/init-app-role.sql` (append-only least-privilege grants).

The orchestrator updates: the HANDOVER decisions-index row, registry CAP-GRANT-1, and the review
records filed under `docs/plans/prh2-hardening-round/`.
