# RV-PRH — Architect review of the Payment Readiness & Provider-Independent Hardening block

- Reviewer: `architect` (mandatory cross-domain review, PRH-REV)
- Branch / HEAD read: `claude/focused-wright-jw88w9` at `ee490a3`. The task named `f4d7dce`; HEAD
  had moved on by one docs-only commit. Other agents are still committing, so every code
  observation below is pinned to `ee490a3`.
- Inputs read: `CLAUDE.md`; `docs/governance/task-registry.md` PRH section; ADR 0094, 0095
  (§0–§3, §6.1, §10.3–§10.8, §14, §15), 0096 (§20–§22.6), 0097 (§3), ADR 0082 Amendment A7, ADR
  0013; `rv-prh-i1-killswitch-security.md`, `rv-prh-i2-kyc-identity-compliance.md`,
  `rv-prh-i1-callback-ledger.md`, `rv-prh-i1-callback-code-review.md`,
  `rv-prh-i1-payout-code-review.md`; migrations 0011, 0014, 0099–0106; the code listed per item.
- Scope: architecture rulings only. I did not edit any ADR, code or registry row. Amendment
  text is supplied for the orchestrator to apply.
- Labels follow CLAUDE.md. "Required" means required before the item is marked complete, unless
  a different gate is named.

## Summary of rulings

| # | Item | Ruling |
|---|---|---|
| 1 | Kill-switch single route family (deviation from ADR 0095 §10.4/§10.5) | **Approved with changes.** Amendment AM-1 text is below. One small code change is recommended, not blocking. |
| 2 | KS-AUDIT-TENANT-1 | **Structural fix chosen: option A+.** Add a platform INSERT-only `audit_log` policy family with a provenance trigger and a typed `actor_scope` column, so a platform action lands under the target tenant. This amends ADR 0013 platform-wide; it is not kill-switch-specific. **Not needed for the PRH gate. Required before production launch or the first B2B tenant** (security's bar, unchanged). |
| 3 | N-1/N-1b overlay + sticky `review_required` (ADR 0096 §20–§22) | **Approved with changes.** The shape is confirmed. One gap: "later" is measured by `created_at` (attempt creation), not by decision time. New item KYC-OVERLAY-DECISION-ORDER-1, blocking before the first real KYC adapter. The L-2 de-dup reads `audit_log` as a control input; tolerated, but registered. |
| 4 | Cross-domain consistency | **Two non-conformances and three gaps found.** (a) The reversal tombstone branch inserts R0 **after** an L1 lock, which violates A7 and creates a real deadlock cycle. (b) Casino/KYC phase B has no in-gate `txscope.Held` refusal, and their MOCK resolvers do not refuse either (INV-IO-1(b)). (c) I could not find the INV-IO-1(c) static scan. (d) Some A7 (7) harness tests may be missing. (e) KYC callback races are handled by 5xx redelivery, not a durable receipt (accepted divergence). The ADR 0097 ordering is consistent with ADR 0095 §6.1 and ADR 0094. |
| 5 | Migration hygiene / HEAD-repoint rule | **Allocation 0099–0106 is gap-free on disk, but the registry allocation line omits 0106** and ADR 0095's header still carries the old numbers. The HEAD-repoint rule is adopted as project policy. **Yes to a CI check** (MIG-PIN-HEAD-1). It does not block the PRH gate; a manual gate check stands in until it lands. |
| 6 | ADR statuses | 0094: IMPLEMENTED, closure pending CI. 0095: **PARTIALLY IMPLEMENTED** (its header "NOT IMPLEMENTED" is wrong). 0096: **PARTIALLY IMPLEMENTED** (its header "IMPLEMENTED" over-claims). 0097: **PARTIALLY IMPLEMENTED**, security-approved. Also ADR 0082 A7: PARTIALLY IMPLEMENTED with one non-conformance. Deferred items go into one consolidated decision record (list in §6). |
| 7 | CLAUDE.md / Blueprint / stage-gate | **No scope creep beyond the human's PRH authorisation, and no human decision taken by an agent.** Violations to fix before PRH-GATE: `docs/active-stage.md` and `docs/progress.md` do not record PRH at all; the ADR status lines over- or under-claim (no-fake-completion); the registry migration allocation is incomplete. |

---

## 1. Kill-switch route deviation — approved with changes

### Verdict

**Approved with changes.** The single `/v1/admin/tenants/{tenantID}/payments/...` family using
`canActOnTenant` is architecturally sound, and it is more consistent with the codebase than the
ADR's two-tree design:

- `provider_credential_handlers.go` and `admin_routes.go` already use the same dual-scope
  pattern.
- The database is the actual enforcement point. `payment_kill_switch_session()` (migration 0105,
  redefined by 0106) derives scope from GUCs and re-validates it against `staff_users`. The
  platform-lock, four-eyes and KS-L6 rules all live in triggers.
- `staff_users` CHECK (migration 0011:23–25) makes "nil tenant" equivalent to
  `role = 'platform_admin'`.

I verified the scope derivation in `runKillSwitchTx`
(`internal/httpserver/payments_kill_switch_handlers.go:179`): a nil-tenant token opens
`WithPlatformAdmin(subject)`. Otherwise `WithPrincipalScope(c.target, subject)` runs, where
`c.target` is the **path** value, already checked to equal `tc.TenantID` by `canActOnTenant`.

**Change (recommended, non-blocking, defence in depth):** for a tenant-scoped principal, open the
transaction with `tc.TenantID`, not the path value. Under CLAUDE.md ("tenant_id is authoritative
from server-side authenticated context only") the path is an assertion to compare, never a source
of scope. Behaviour is identical today because K16 (the `canActOnTenant` mutant) is killed. The
change removes the dependency on that one comparison.

### Amendment text for ADR 0095 §10.4/§10.5 (for the orchestrator to apply verbatim)

> #### 10.4a/10.5a Amendment AM-1 (`architect`, 2026-09-27) — one dual-scope route family for the payment kill switch
>
> **Status:** ACCEPTED. Recorded at `security`'s request (`rv-prh-i1-killswitch-security.md`,
> "Ruling on the route deviation", condition 1).
>
> **Supersedes:**
> - §10.4 bullet 1's "tenant scope … and the platform admin API (platform scope, §10.5)";
> - §10.4 last bullet ("A platform principal cannot act through the tenant API, and a tenant
>   principal cannot act through the platform API");
> - §10.5's "Platform admin API" table and its `platform_payments_kill_switch:*` permission
>   family;
> - §10.5's tenant-route rule "A platform-scoped principal is refused with 403";
> - §10.5's "no tenant id is taken from the path".
>
> Everything else in §10.3–§10.5 stands: engage is single-actor with a reason code; release is
> four-eyes with approve-and-release in one tx; a platform-engaged row is read-only to tenants;
> the switch is never exposed on a player route; OpenAPI is pinned by a conformance test. Read
> every "0102/0103 kill switch" reference as migration **0105**, with its three guard functions as
> **redefined by migration 0106**.
>
> **Routes (the only kill-switch surface).** All are under `/v1/admin/tenants/{tenantID}/payments/`:
> - `GET kill-switches` and `GET kill-switches/{killSwitchID}` — `payments_kill_switch:read`;
> - `POST kill-switches` (engage) — `payments_kill_switch:engage`;
> - `POST kill-switches/{killSwitchID}/release-requests` — `payments_kill_switch:release`;
> - `GET kill-switch-release-requests/{requestID}` — `payments_kill_switch:read`;
> - `POST kill-switch-release-requests/{requestID}/approve` — `payments_kill_switch:release`;
> - `POST kill-switch-release-requests/{requestID}/cancel` — `payments_kill_switch:release`.
>
> The three permissions are granted only to `platform_admin` and `tenant_admin`.
>
> **Scope derivation (normative).**
> 1. The acting scope comes only from the authenticated token. `TenantID == nil` means platform
>    scope. Anything else means tenant scope. The path `{tenantID}` is never a source of scope.
> 2. *Tenant principal.* `{tenantID}` must equal the token's tenant, or the call gets 403 plus a
>    denied audit row in the caller's own scope. The transaction runs under
>    `WithPrincipalScope(<token tenant>, principal)`.
> 3. *Platform principal.* `{tenantID}` names the target tenant.
>    - The handler verifies the tenant exists, or returns 404.
>    - The transaction runs under `WithPlatformAdmin(principal)`.
>    - Every statement is predicated on `tenant_id = {tenantID}` under the §10.2.1 platform RLS
>      family.
> 4. An object id belonging to another tenant, under the caller's own path, returns 404, never
>    data.
> 5. Player, service and unauthenticated callers get 403/401 on all seven operations. The
>    route-table test pins this.
>
> **Safety invariants (binding; changing any of them requires `security` re-review before
> merge).**
> - **SI-1.** Only `platform_admin` may ever hold a nil-tenant staff token. Today this rests on
>   the `staff_users` CHECK (`role = 'platform_admin'` ⇔ `tenant_id IS NULL`, migration 0011) and
>   on token issuance. Any change that lets another role hold a nil tenant invalidates this
>   amendment.
> - **SI-2.** Platform lock, four-eyes, `engaged_by_scope`/`changed_by_scope` derivation and
>   KS-L6 are enforced by the database (`payment_kill_switch_session()` and both guard triggers).
>   They are never enforced by the URL or the permission name. A change that moves any of them
>   into handler code is a violation.
> - **SI-3.** Granting any `payments_kill_switch:*` permission to a role other than
>   `platform_admin` or `tenant_admin`, or introducing a second platform-scoped role, requires
>   `security` re-review. A second platform role is also the trigger to reconsider a separate
>   platform permission family, for example read-only platform operations versus engage.
>
> **Audit.** Every mutation writes its audit row in the same transaction. Every considered refusal
> (`cas_conflict`, `trigger_refusal`) writes a denied row in a separate transaction. Both carry
> actor, before/after, IP, UA, reason code and `target_tenant_id`. Until KS-AUDIT-TENANT-1 lands,
> a platform-scoped row is written with `tenant_id NULL` and the target tenant in metadata only.
> §10.5's audit rule is therefore met for the platform actor but **not yet for the target
> tenant's own audit view**. This is launch-blocking, as registered.
>
> **Rationale.** This is functionally equivalent to the two-tree design. Both permission families
> would map to exactly one role each today. It is consistent with the existing `canActOnTenant`
> precedent, and it keeps one OpenAPI surface. **Reversibility:** splitting later is additive
> (new paths and permissions, no data change).

---

## 2. KS-AUDIT-TENANT-1 — structural fix (design only, not implemented)

### Findings that widen the scope

1. **It is not kill-switch-specific.** `internal/providercred/service.go:834–867` uses the same
   `tenant_id NULL` + `metadata.target_tenant_id` shape. By construction, every platform-on-
   tenant mutation under `WithPlatformAdmin` is invisible to that tenant's audit read.
   `audit_log.dual_scope_isolation` (migration 0014) admits `tenant_id IS NULL` rows only when
   `app.tenant_id` is unset.
2. **Even the platform read cannot see the target tenant today.** `queryAuditLog`
   (`admin_routes.go:~1166`) does not select `metadata`, so `GET /v1/admin/platform/audit-log`
   cannot show `target_tenant_id` to anyone. The interim measure is only reachable by direct SQL.
3. `audit_log` is not partitioned, so adding a column is a cheap metadata-only change.

### Options

| Option | Where a platform action on tenant X lives | Assessment |
|---|---|---|
| B: typed indexed `target_tenant_id` column plus a tenant SELECT policy `target_tenant_id = app.tenant_id` | Platform scope (`tenant_id NULL`) | Simple. However, the tenant's audit trail is then split across two ownership scopes. When isolation tightens (schema-, database- or cluster-per-tenant, per CLAUDE.md), a per-licence regulator export must union the tenant store with the platform store. It also adds a second, cross-scope read path to RLS. |
| A: platform INSERT-only policy family (mirrors 0105) | Tenant X's own scope | The tenant's trail is self-contained and survives isolation tightening unchanged. A regulator export reads one scope. It reuses a pattern `security` has already reviewed. |
| **A+ (chosen)**: A, plus a provenance trigger and a typed `actor_scope` column | Tenant X's own scope, with provenance forced by the DB | See below. |

### Chosen design (A+), to be written as its own ADR amending ADR 0013

- **Column.** `audit_log.actor_scope TEXT NULL CHECK (actor_scope IN ('tenant','platform','service','system','player'))`.
  It is typed, never JSONB, and is forced by a trigger (below). Historical rows stay NULL
  (append-only; synthetic data only, no backfill).
- **Policy.** Add `platform_insert_into_tenant ON audit_log FOR INSERT WITH CHECK
  (tenant_id IS NOT NULL AND app.platform_admin_principal_id IS NOT NULL AND app.tenant_id IS
  NULL AND app.player_account_id IS NULL)`.
  - It is INSERT only. There is no platform SELECT of tenant rows through this family.
  - The existing `dual_scope_isolation` policy is unchanged.
- **Trigger** (`BEFORE INSERT`, the `payment_kill_switch_session()` pattern). When a row is
  inserted with `tenant_id NOT NULL` under a platform session, the trigger:
  - requires `actor_type = 'staff'` and `actor_id = app.platform_admin_principal_id`;
  - re-validates that principal against `staff_users` (`tenant_id IS NULL`, `status = 'active'`);
  - requires that the target tenant exists;
  - forces `actor_scope = 'platform'`.

  On every other insert it forces `actor_scope` from the session: `'tenant'` when `app.tenant_id`
  is set, and so on. The application never supplies `actor_scope`.
- **Platform oversight read.** "Everything platform staff did, across tenants" needs a platform
  SELECT of `actor_scope = 'platform'` rows in any tenant. That is a separate policy, and it is a
  **`security` decision**. I propose it but do not rule on it. Without it, platform staff read
  tenant X's trail by scoping to X, which already works. Index: `(actor_scope, created_at DESC)`
  partial on `actor_scope = 'platform'`, only if that policy is adopted.
- **Application.** `audit.Record` callers acting under `WithPlatformAdmin` on a named tenant pass
  that tenant's id. `auditTenantID()` and the providercred equivalents are deleted. The platform
  and tenant audit read APIs return `actor_scope`.
- **Tests (for `qa`).**
  - A platform engage on X is visible in X's tenant audit read and invisible to tenant Y.
  - A tenant session cannot insert with `actor_scope = 'platform'` (the trigger forces it).
  - A platform session with a non-staff or suspended UUID cannot insert into X.
  - A platform session cannot SELECT X's rows through the new policy.
  - Mutants: drop the trigger's staff re-validation; widen the policy to FOR ALL.

### Timing

- **Not required for the PRH gate.** PRH is not a launch gate, and the interim measure was
  accepted by `security`.
- **Required before production (real-money) launch or the first B2B tenant, whichever is
  first.** This is security's bar and I do not relax it. For the own-brand B2C launch the
  operator and the platform are the same organisation, which reduces but does not remove the
  need: the Anjouan regulator's question "who stopped payments on this licence" must be
  answerable from a supported read path, and today it is not (finding 2).
- It needs its own ADR number (orchestrator allocates), migration and security review. It
  belongs in the next authorised hardening stage, not in PRH.

---

## 3. ADR 0096 N-1/N-1b and sticky `review_required` — approved with changes

**Confirmed:**
- **The cross-account overlay's final-row rule** (`finalStatusesSQL = ('approved','rejected')`):
  - `expired`, `pending`, `review_required` and orphans are invisible to the other-account
    subquery;
  - a rejection is lifted only by a later genuine `approved` on the same account;
  - the primary-account rule (§2.6(a)) is unchanged;
  - the overlay is tenant-bounded (`v1.tenant_id = $1`), so nothing presumes HD-KYC-6
    cross-tenant reuse.
- **Sticky `review_required`** (the guard is `status = 'review_required' AND reviewed_by IS NOT
  NULL AND newStatus IN ('approved','expired')`):
  - `rejected` always applies (N-3);
  - a held result is audited;
  - a provider-set `review_required` is unaffected.

  `reviewed_by` is a sufficient signal. It is written only by `ReviewVerification`, and
  migration 0040's `CHECK ((reviewed_at IS NULL) = (reviewed_by IS NULL))` keeps it paired.

  I concur with identity-compliance (Rulings 2 and 5) that this is an internal case-management
  control that tightens enforcement, **not** a human/legal decision.

**Change 1 — KYC-OVERLAY-DECISION-ORDER-1 (new; blocking before the first real KYC adapter, not
the PRH gate).**
- **The problem.** Both the overlay (`enforcement.go:446`) and the primary read (`:352`) order by
  `created_at DESC, id DESC`, which is attempt-creation order. Identity-compliance's Ruling 1
  wording ("a later `approved`") is really about decision order. The two diverge when an older
  attempt is decided after a newer one:
  1. V1 is created on account B;
  2. V2 is created on B;
  3. V2 is approved;
  4. V1 is later rejected, for example because the vendor detects a forged document.
- **The effect.** The overlay selects V2 (newer by `created_at`) and the rejection is masked.
  This is a deny being lifted by evidence that predates it.
- **Reachability.** It needs asynchronous vendor decisions and more than one open verification
  per account. The N-1 probe shows creation on an account is not restricted. It is not reachable
  with today's synchronous MOCK path, which is why it does not block PRH.
- **Required fix direction** (the mechanism is identity-compliance's to choose; the invariant is
  mine):
  - *Invariant:* the overlay compares **decision times**. A rejection is lifted only by an
    `approved` decided after that rejection was decided.
  - *Implementation:* a trigger-stamped, immutable `decided_at`, set on the first transition into
    a terminal status. Alternatively, `updated_at`, but only if it is proven immutable after a
    terminal transition.
  - *Scope:* apply the same review to the primary-account read.
  - *Tests:* one out-of-order-decision regression test plus a mutant.

**Change 2 — KYC-HELD-AUDIT-DEDUP-1 (register, non-blocking).**
- **The problem.** L-2's de-dup (`internal/kyc/provider.go:660`) reads `audit_log` to decide
  whether to write. This is a check-then-insert against the audit sink. The cost of a race is
  only a duplicate audit row, and enforcement is not affected, so I tolerate it.
- **Architectural invariant (for `code-reviewer`):** `audit_log` is a write-only sink as far as
  domain logic is concerned. **No enforcement, financial or state-machine decision may depend on
  its contents.** This site is the only permitted exception and must stay confined to
  suppressing duplicate audit rows.
- **Before audit moves to an external append-only store:** move the key into a KYC-owned table
  or a unique partial index.

**Note (non-blocking).** A held vendor `approved`/`expired` is recoverable only by an officer
reading the audit row, because the vendor does not redeliver. A case-management surface ("held
results awaiting review") is a deferred consideration (KYC-HELD-QUEUE-1). It is needed before
real KYC volume, not now.

---

## 4. Cross-domain consistency

### 4.1 ADR 0082 A7 lock order (withdrawal/intent → attempt → posting)

| Path | As built (`ee490a3`) | Conformance |
|---|---|---|
| Deposit/payout callback evidence (`receipt.go` `ApplyReceiptEvidence`) | R0 insert (`:436`) → parent `FOR UPDATE` (`:443`/`:447`) → attempt re-read + CAS → L3/L4 | Conforms. The attempt lock is taken by the CAS `UPDATE`, after the parent, which is equivalent. |
| Deposit reversal, posting branch (`:838`→`:855`→`:872`) | R0 → intent `FOR UPDATE` → L2 original tx `FOR UPDATE` → `Post` | Conforms (M3 fixed). |
| **Deposit reversal, tombstone branch (`:795`→`:806`)** | **intent `FOR UPDATE` (L1) → R0 insert** | **Non-conformant: violates A7(2), "nothing that already holds an L1 … ever inserts a receipt."** |
| Payout claim T1p (`payout.go:241`) | `withdrawal_requests` `FOR UPDATE` → KYC gate (plain reads) → kill-switch predicate in-statement → `DenyForCompliance` or attempt INSERT | Conforms to A7(6). |
| Payout phase C / status evidence | `LockForPayoutEvidence` (withdrawal) → attempt CAS → L3/L4 | Conforms. |
| Deposit T2 per-item (sweeper) | RG/KYC gate → intent `FOR UPDATE` → attempt | Conforms to rule N1. |

**Finding A7-TOMB-1 (Medium; payments + ledger-finance; blocks PRH-I1 callback cutover
completion, which is already NOT READY).**
- **How it arose.** Ledger-finance L5 asked for a parent lock on the tombstone branch. The fix
  placed that lock **before** the receipt insert.
- **Why the ledger reviewer found no deadlock.** Duplicates of one fingerprint always serialise
  on the parent first. That holds only while both deliveries take the same branch.
- **The branch choice is made on an unlocked read** (`original.LedgerTransactionID == nil`,
  `:786`). If the original deposit posts between two concurrent deliveries of the same reversal
  event, they diverge:
  - Tx1 (tombstone branch) holds the intent and waits on the receipt key;
  - Tx2 (posting branch) holds the receipt key and waits on the intent.

  That is a real cycle. PostgreSQL aborts one side, the provider gets a 5xx and redelivers, so no
  money is lost, but A7's deadlock-freedom argument no longer holds.
- **Required:**
  1. Move `insertReceiptDeduped` ahead of the L5 parent lock on the tombstone branch. R0 stays the
     first write, and the lock remains.
  2. After taking the parent lock, re-read the original attempt and re-decide the branch, so the
     tombstone-vs-post decision is made under the lock.
  3. Add a race test for this divergence (two concurrent reversal deliveries racing the original's
     posting) plus the A7(7) mutant "receipt insert after an L1 lock".

  Ledger-finance must re-verify.

**Finding A7-TESTS-1 (verify).** I could not locate these A7(7)-mandated harness tests:
- a sweeper deposit T2 re-claim racing an RG self-exclusion write (rule N1);
- a deferred-receipt backstop racing a callback.

Present: `TestLockOrder_ConcurrentDepositAndDepositReversal_NoDeadlock` and
`TestReceiveCallback_ConcurrentDuplicateCallbacksOnlyOnePosts`. Ledger-finance must confirm or
add the missing tests before A7 is labelled IMPLEMENTED. The §1.6/§1.7 inventory update that A7
requires "when PRH-I1 lands" is also still owed.

**Forward note (record in WD-RG-1).** T1p runs the KYC gate after the `withdrawal_requests` lock.
That is legal only because `EvaluateEnforcement` takes no advisory lock. When WD-RG-1 adds an RG
gate to withdrawal or payout, `rg.EvaluateEligibility` takes L0.4 and **must** move before the
withdrawal lock (rule N1).

### 4.2 Two-phase, no-tx-across-network-call (ADR 0095 D1 / INV-IO-1)

| Domain | Phase A commits before call | (a) API shape | (b) runtime refusal at the gate | (c) static scan | (d) capture test |
|---|---|---|---|---|---|
| Payments | Yes (T2/T1p) | Yes | Yes: `gate.go:101` | **Not found** | Yes |
| Casino launch | Yes | Yes (`LaunchGame` takes the pool) | **No in-gate check.** Only indirect: the real `providercred` resolver refuses. The casino `MockOutboundResolver` (`mock.go:124`) ignores ctx and does not refuse. | **Not found** | Yes (`launch_two_phase_integration_test.go`) |
| KYC create/submit | Yes | Yes | **Same as casino** (`kyc/callcontext.go:107`) | **Not found** | Yes |
| Payment reconciliation | Fetch outside tx | Yes | Yes (`payment_statement.go:223`) | — | Yes |

**Finding IO-1B (Low; casino + identity-compliance; required before PRH-GATE, since it is
cheap).** ADR 0095 §3.2 says "Casino and KYC use the same gate shape (steps 1, 3, 4, 5, 6 and
8)", and §11 says "a MOCK uses a Synthetic credential source with the **same** refusal". Neither
holds for casino or KYC. The primary control (API shape) holds, so this is defence in depth, but
the ADR text over-claims. Either of these is acceptable:
- (a) add a one-line `txscope.Held(ctx)` refusal immediately before each adapter outbound call in
  `LaunchGame`, `CreateVerification` and `SubmitVerification`, and make both MOCK resolvers
  refuse under `txscope.Held`, with one test each; or
- (b) relabel INV-IO-1(b) for casino/KYC as PARTIALLY IMPLEMENTED in ADR 0095 §15.

I prefer (a).

**Finding IO-1C (Medium for labelling; required before PRH-GATE).** INV-IO-1(c) ("a source scan
… fails on an adapter-method call lexically inside a `With*` closure") and §16.2 item 14 are not
present that I can find. I searched every AST-using test file. It must either be implemented,
following the `lockorder_static_test.go` pattern, or be explicitly labelled NOT IMPLEMENTED and
registered. It must not be implied by the INV-IO-1 row.

**Accepted divergence (record, no change now).** Payments durably receipts and defers a callback
that arrives before the attempt knows its reference (INV-IO-10). KYC answers
`ErrVerificationReferenceUnknown` with a retryable 5xx and stores nothing, so it relies on vendor
redelivery. That is acceptable under the MOCK and under KYC-SUBMIT-OUTBOX-1's acceptance. Add to
KYC-SUBMIT-OUTBOX-1's hard precondition: "KYC callback receipt/deferral, or a vendor-contract
guarantee of redelivery on 5xx (the KYC analogue of LF-C1), is decided at KYC vendor intake."

**INV-IO-4 audit per attempt transition (carried from code review B8).** Attempt transitions are
still audited only at some sites (`payout_sweep.go` has 2 `audit.Record` calls; `attempt.go` has
none). Keep this on the PRH-GATE checklist. The ADR invariant says "in the same tx as its audit
record", so either every CAS site audits or the invariant text is narrowed with `security`'s
agreement.

### 4.3 Admission ordering (ADR 0097)

The ADR 0097 order is admission (A1–A5, A4b) → verification (V) → binding (`VerifiedCallback`) →
verified admission (B1/B2) → parsing → domain. It is **consistent** with:
- ADR 0095 §6.1: redeem → `HandleCallback` → PROVIDER-REF-BOUND validation → read-only
  resolution → cap probe → R0 as first write → locks;
- ADR 0094's split verification (no connection held on the store).

The ADR 0097 order is: admission → verification → binding → parsing → domain. The step order
above adds a second, post-binding admission tier; that refines the ADR order and does not
contradict it. No inconsistency found. One note: the cap probe (§6.1 step 5) is a DB read
inside the domain tx and is correctly after B2, so ORD-3 holds.

---

## 5. Migration hygiene

**Allocation.** Migrations 0099–0106 are gap-free on disk (`ls migrations`). Map:
- 0099 provider-reference bound;
- 0100 KYC policy and decision audit;
- 0101 payment attempts;
- 0102 statement reconciliation;
- 0103 KYC supersession;
- 0104 statement CHECKs;
- 0105 kill switch;
- 0106 platform-GUC hardening.

Required (orchestrator, before PRH-GATE):
1. **The registry PRH allocation line stops at 0105.** 0106 was allocated by the orchestrator
   (ADR 0095 §10.7) but is not recorded there. ADR 0095 §10.7 also says "the callback agent's
   migration is 0107", which is not on disk and not allocated in the registry. Record 0106, and
   either reserve 0107 explicitly or strike the statement.
2. **ADR 0095's header** still says "0101 payment attempts…, 0102 kill switch, 0103 payment
   statement reconciliation", and §13.2/§13.3 carry the old numbers. Replace these with one
   authoritative number map in the header (the list above). A reader should not have to
   reconstruct it from three "read as swapped" notes.

**HEAD-repoint rule.** Adopted as project policy, and it should be written into a general
document, not only ADR 0095 §10.8:
- A migration that `CREATE OR REPLACE`s a guard, trigger or session function must, in the same
  change, repoint every behaviour test of that function to a HEAD-migrated database.
- Only tests explicitly about the older migration's own history (up/down round-trip, down-guard)
  may stay pinned, and each must say so.

State at `ee490a3`:
- **0106**: the payments harness was repointed in round 1c.
- **0103**: `internal/kyc/migration_0100_integration_test.go` pins only the round-trip and
  down-guard tests (`stagedMigrations0100`). The lifecycle and supersession behaviour tests use
  `testPool` (HEAD). This conforms.

**CI check: yes (MIG-PIN-HEAD-1; owners `qa` + `devops`).**
- **Why.** The defect already produced invalid mutation evidence once (§10.7 → §10.8), and the
  failure is silent: tests pass against dead code.
- **Proposed mechanism** (cheap, pure Go, no DB):
  1. All version-pinned scratch harnesses go through one shared helper that takes a mandatory
     `HistoryOnly("<reason>")` marker. A static test forbids ad-hoc hold-back loops elsewhere.
     Today these are `stagedMigrations0048/0075/0092/0095/0100` and the bonus staging helper.
  2. A test parses `migrations/*.up.sql`, builds `function → latest defining version`, and fails
     when a test file pinned at version P references (by name, or by the table its trigger
     guards) a function redefined at a version above P. The only escape is an explicit
     `//migpin:allow <fn> <reason>`.
- **Introducing it includes a one-time sweep** of the existing staged harnesses against the
  redefinitions in 0047, 0067/0070, 0089, 0093 and 0103/0106.
- **Timing.** It does not block the PRH gate. It must land before the next migration that
  redefines a guarded function. Until then, the PRH-GATE checklist carries a manual line: "for
  0103 and 0106, confirm that every behaviour test runs at HEAD". I verified this above as of
  `ee490a3`.

---

## 6. ADR statuses and deferred considerations

| ADR | Header today | Should carry now |
|---|---|---|
| 0094 | "ACCEPTED — IMPLEMENTED 2026-09-26 … F-POOL-1 stays OPEN until security reviews" | **ACCEPTED — IMPLEMENTED; F-POOL-1 CLOSED WITH CONDITIONS; final closure BLOCKED on CI-BILLING-1** (PRH-FPOOL1: isolated timing lane of 8 named tests plus full CI green, no reruns). The "stays OPEN until security reviews" sentence is stale and should cite the security closure. |
| 0095 | "ACCEPTED (design) — NOT IMPLEMENTED" | **ACCEPTED — PARTIALLY IMPLEMENTED.** Add a per-part table: see the breakdown below this table. |
| 0096 | "ACCEPTED — IMPLEMENTED, pending … review" | **ACCEPTED — PARTIALLY IMPLEMENTED** (matches the registry's PRH-I3 label; the header over-claims). Per part: the enforcement mechanism and gates are IMPLEMENTED, including deposit/payout call sites now wired by PRH-I1 (`kycgate.go`, `payout.go:103`), which the header still says are unwired. Still open: §16.2 open items (LF-I3-4/5, security F4/F5, B7); threshold values NOT IMPLEMENTED (HD-KYC-1..7, human); KYC-FX-AGG-1 NOT IMPLEMENTED; KYC-OVERLAY-DECISION-ORDER-1 open (this review). §20.2's "pending architect review" line is closed by §3 of this review. |
| 0097 | "ACCEPTED — IMPLEMENTED (pending security code review)" | **ACCEPTED — PARTIALLY IMPLEMENTED.** Admission control is IMPLEMENTED and security-APPROVED (`rv-prh-i4-security.md` §8). PRH-I4-METRICS-1 is NOT IMPLEMENTED. WEBHOOK-EDGE-1 is open (STAGING/INFRA REQUIRED). The "pending security code review" wording is stale. |
| 0082 A7 | "Design ACCEPTED. NOT IMPLEMENTED" | **ACCEPTED — PARTIALLY IMPLEMENTED**: as-built conforms except A7-TOMB-1; the (7) tests and the §1.6/§1.7 inventory are pending (A7-TESTS-1). Only `ledger-finance` may change this status. |

ADR 0095 per-part breakdown:
- attempts/state machine (0101) and deposit path: IMPLEMENTED; the callback cutover is NOT READY
  per the latest code review;
- payout T1p/phase C: PARTIALLY IMPLEMENTED (APPROVE WITH CONDITIONS);
- kill switch: PARTIALLY IMPLEMENTED (Phase 2 T3/T1p labelled-reason wiring; alert delivery NOT
  IMPLEMENTED);
- PROV-OUTBOUND-CRED-1: payments kind-split NOT IMPLEMENTED; casino/KYC IMPLEMENTED;
- §15 casino/KYC: IMPLEMENTED, pending re-reviews, with IO-1B;
- §12 reconciliation: IMPLEMENTED against MOCK; real source PROVIDER DEPENDENT;
- §10.1 manifest: PARTIALLY IMPLEMENTED (MANIFEST-1..4);
- INV-IO-1(c): NOT IMPLEMENTED (IO-1C);
- M1/M2: BLOCKED (HD-0095-1);
- AM-1 (this review).

**Deferred considerations.** Recommend **one consolidated decision record** ("PRH deferred
considerations", number allocated by the orchestrator) rather than one ADR per item. Each entry
gives the trigger that re-opens it. The registry rows point to it.
- PROV-REVOKE-ALL-1: platform-wide, cross-tenant kill switch.
- Kill-switch permission-family split. Trigger: a second platform role (AM-1 SI-3).
- PAY-PAYOUT-CASCADE-1, PAY-ATTEMPT-RETENTION-1, and LF95-R1 automatic re-drive.
- PRH-I1-MANIFEST-1..4, each with the tripwire "before the first real payments adapter".
- KYC-SUBMIT-OUTBOX-1, extended per §4.2 (callback receipt/redelivery). A hard precondition on
  the first real KYC adapter.
- KYC-FX-AGG-1. Needs a human decision on the rate source.
- KYC-HELD-QUEUE-1 and KYC-HELD-AUDIT-DEDUP-1 (this review).
- CAS-STMT-IO-1, CAS-HEALTH-FAILOPEN-1, CAS-REVOKE-BET-RACE-1: "before first real casino
  adapter".
- WEBHOOK-RL-SHARED-1.
- **Consolidate the three overlapping rows** WEBHOOK-RL-F2-AUTHROUTES-1, API-POOL-PIN-1 and
  WEBHOOK-RL-ADMIN-1. They describe the same authenticated-route pool-pinning/rate-limit class
  under three IDs; keep one ID.

Items that are **not** deferred considerations but decisions or work items with a gate:
- KS-AUDIT-TENANT-1: its own ADR, launch-blocking;
- KYC-OVERLAY-DECISION-ORDER-1: blocks the first real KYC adapter;
- MIG-PIN-HEAD-1;
- A7-TOMB-1, IO-1B, IO-1C: PRH work items.

---

## 7. CLAUDE.md, Blueprint and stage-gate compliance

**Scope: no creep found.** Everything built maps to the human's PRH list (F-POOL-1/2, KYC-
ENFORCE-1, PAYWH-RL-1, PROVIDER-REF-BOUND-1, PROV-OUTBOUND-CRED-1, payment contract, MOCK
reconciliation, payment-scope kill switch) or to a review condition on those items. Items that
look like extras but are justified:
- the `credentialscan` reflection test is ADR 0095 §16.2 item 15;
- casino `postBet` session status/expiry enforcement makes ADR 0095 §15.1's own claim true
  (security I3);
- migrations 0103, 0104 and 0106 close security conditions;
- the KYC "play" gates are ADR 0096 §3.5 within KYC-ENFORCE-1. They ship dormant with no seeded
  policy values (§3.7).

I did not re-verify §3.5 against the Blueprint PDF text in this review. ADR 0096 records it as
its own design, so `product-owner-proxy` should confirm it is labelled as a design choice
(RECOMMENDATION), not a Blueprint requirement.

**Also confirmed:**
- There is no real vendor, no AWS/Terraform/IAM, no Bonus Wave 4 and no AI-agent work.
- No human decision was taken by an agent:
  - HD-0095-1 leaves M1/M2 BLOCKED;
  - HD-KYC-1..7 have no values;
  - HD-PRH-1 is open;
  - identity-compliance Rulings 2 and 5 are internal controls, and I concur (§3).

**Violations to fix before PRH-GATE (orchestrator, PRH-0):**
1. **Stage state documents.**
   - `docs/active-stage.md`'s current-stage pointer still reads "Stage 10.3 ACCEPTED … awaiting
     human authorization".
   - Neither `docs/active-stage.md` nor `docs/progress.md` mentions PRH at all.
   - CLAUDE.md makes these the per-session source of stage state. The human authorised PRH on
     2026-09-27, and that must be recorded there, not only in the registry.
2. **No fake completion.** The ADR 0095 and 0096 status lines are wrong in opposite directions
   (§6), and ADR 0095 §3.2/§11 over-claim the casino/KYC gate (IO-1B). Correct them.
3. **Registry allocation line** (§5 item 1).
4. **Security-sensitive completion.** Per CLAUDE.md, none of PRH-I1 (callback cutover NOT READY;
   payout APPROVE WITH CONDITIONS), PRH-I2 casino (code review NOT READY), PRH-I3 (code review
   NOT READY) or PRH-I5 (code review NOT READY) may be labelled IMPLEMENTED until the named
   reviewers clear them. The registry currently respects this. Keep it that way at synthesis.
5. **Stage gate.** PRH-GATE requires green GitHub CI, and CI-BILLING-1 is external. No local
   replay compensates for it, as the registry already states. The block must stop at PRH-GATE
   for explicit human authorisation. Nothing in this review authorises the next stage or any
   launch.

## Architectural invariants for `qa` / `code-reviewer` (added by this review)

- **AR-PRH-1.** For a tenant-scoped principal, the transaction's tenant scope is always the
  token's tenant. A path or body tenant id is compared, never used as scope (AM-1).
- **AR-PRH-2.** No enforcement, financial or state-machine decision reads `audit_log`. The single
  sanctioned exception is the KYC held-for-review duplicate suppression (§3).
- **AR-PRH-3.** A receipt (R0) insert is never preceded in the same transaction by any L1+ lock,
  on every branch (A7-TOMB-1).
- **AR-PRH-4.** Every adapter outbound call site in payments, casino and KYC refuses under
  `txscope.Held`, in the gate itself, independent of the credential resolver (IO-1B).
- **AR-PRH-5.** The cross-account KYC overlay orders by decision time, not creation time (from
  KYC-OVERLAY-DECISION-ORDER-1 onward).
