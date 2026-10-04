# PAY-CLOSED-TENANT-FUNDS-RESOLUTION-1 — the HD-PRH2-9 staff resolution path for player funds held by a closed tenant

- **Status: PROPOSED DESIGN (`architect`, 2026-10-04, base `3517980`). NOT IMPLEMENTED.** It becomes
  an ADR when the orchestrator allocates a number. No number is taken here, to avoid colliding with
  E1's ADR for HD-PRH2-11. It needs `security`, `ledger-finance` and `product-owner-proxy` review
  before any code. Legal and regulatory outcomes are **not** decided here (§11).
- **Source:** ADR 0105 §1 (HD-PRH2-9, human decision "controlled STAFF RESOLUTION PATH"); ADR 0095
  §37.5 (H LF F3 residual); ADR 0101 rev 3 §19 (why this is not folded into K3).
- **Registry:** a new workstream PAY-CLOSED-TENANT-FUNDS-RESOLUTION-1 (orchestrator registers it). It
  resolves the *mechanism* half of HD-TENANT-CLOSURE-PLAYER-FUNDS-1. The legal half remains the
  separate human decisions HD-CTF-1..9 (§11).
- **Labels used:** "Requirement" = stated by the human (ADR 0105 §1) or an existing binding ruling.
  "RECOMMENDATION" = architect proposal, which needs review. "Example only" = an outcome the
  mechanism must be able to *express*; whether it is *permitted* is a human/legal decision.

## 1. Requirements (from ADR 0105 §1, verbatim in substance)

| # | Requirement |
|---|---|
| R1 | No automatic payout dispatch for a closed tenant |
| R2 | No automatic cancel or release without a controlled resolution |
| R3 | Flow: closed tenant → outstanding player payout/hold → authorized financial-staff resolution queue → required authorization / four-eyes per the applicable financial policy → permitted resolution → deterministic ledger operation → complete audit trail |
| R4 | Configurable and jurisdiction-aware; no single universal legal outcome is hard-coded |
| R5 | Preserve tenant isolation, financial invariants, idempotency, actor/subject separation, auditability and four-eyes; no direct balance mutation; no unrestricted bypass |
| R6 | A legally required outcome or threshold that the architecture cannot determine is a separate human decision |

## 2. What is held, and what this path covers

Holds are created at `requested` (`withdrawal_requested`: `player_cash` → `player_withdrawal_hold`,
`withdrawal.go:496`) and released by exactly one terminal posting. For a tenant with
`tenants.status = 'closed'`, every withdrawal in a hold-bearing state falls into one item class:

| Class | Withdrawal state | Attempt (one per withdrawal, `0101:140`) | Today (post-H) | Covered here? |
|---|---|---|---|---|
| **CT-PRE** | `requested`, `pending_review`, `approved` | none | No sweeper involvement. `Cancel` is player-initiated (`:1640`, from `requested` only). `Reject` needs `pending_review` and tenant-staff eligibility (`:882`). `approved` has **no** non-KYC release function. HTTP `ClaimForDispatch` reads no tenant status (H-SEC-5). | **Yes** |
| **CT-NEVER-SENT** | `submitted` | `created` (`NOT ever_possibly_sent`, INV-IO-9 CHECK `0101:103`), including a KYC-deny escalated one | T2 re-claim withheld forever (`payout_sweep.go:271`); M3 has no caller | **Yes** (a governed M3) |
| CT-INFLIGHT | `submitted` | `submitting` / `pending` / `ambiguous` | polls continue (resolution-only); T12 withheld | **No.** Shown in the queue. Resolves by evidence, or by **K3 M2** (available for non-active tenants, ADR 0101 §5.1) |
| CT-M2 | `submitted` | `disputed` with an M2-allow-listed reason | — | **No.** Shown. Resolves by **K3 M2** |
| CT-BLOCKED | `submitted` | `disputed`, not M2-admitted (`amount_asset_mismatch`, `callback_amount_asset_mismatch`, `invalid_provider_reference*`, …) | — | **Only `retain`**. Release waits for PAYOUT-AMOUNT-DISPUTE-1 / PAY-PAYOUT-UNBOUND-HOLD-1 |

**Not covered (separate human decision HD-CTF-4):** `player_cash` balances (and bonus or locked
balances) of a closed tenant that are **not** under a withdrawal. The human's answer names
"outstanding player payout/hold". Extending this path to unrequested balances is a different
question (for example who initiates on the player's behalf), and is not assumed.

## 3. The queue

- **The queue is a read model, not a table.** It is a Go query in the new service, run under the
  caller's session (so RLS applies), over `withdrawal_requests`, `payment_attempts`, the latest
  resolution per withdrawal, and `tenants.status`. A DB view is avoided: a plain view runs with
  owner rights, and a `security_invoker` view would add a new pattern to review.
- **Item content:** class; withdrawal id, amount, asset and age; attempt state/reason; the
  **permitted outcomes as of now** (§4); the latest resolution and its state; the M2 link for
  CT-INFLIGHT / CT-M2.
- **Who may see it (RECOMMENDATION; security decides):**
  - **Platform financial staff acting in tenant X** (ADR 0099/0100 acting family,
    `WithPlatformActingInTenant`), valid only with an **explicit in-force grant for X** of a
    `closed_tenant_hold_resolution:*` capability (HD-PRH2-6: platform employment confers nothing).
  - **Tenant staff of the closed tenant: read-only at most, never request or approve** (HD-CTF-2).
    A closed tenant's staff accounts may no longer be under any contractual control, so the default
    is fail closed.
  - **Discovery across tenants:** a platform principal can list `closed` tenants (`tenants_read`).
    Per-tenant item counts need the acting grant. No new platform-wide read of tenant money rows is
    created. The tenant-closure runbook step is "grant designated platform finance staff the
    capability for X, then work the queue". An optional dedicated alert Kind is HD-CTF-8 / a
    security option, and is not part of this design.

## 4. Configurable, jurisdiction-aware permitted outcomes (R4)

### 4.1 The outcome catalogue (mechanics only; family R, migration-seeded)

Table `closed_tenant_resolution_outcomes(code PK, applies_to_classes text[], posting_shape text,
description)`. **A row describes what the platform can execute, not what is legal.** The
mechanism must be able to express at least these. All are **examples only; legal permission per
jurisdiction is HD-CTF-1**:

| Code | Classes | Effect | Ledger operation |
|---|---|---|---|
| `release_hold_to_player_cash` | CT-PRE, CT-NEVER-SENT | the hold returns to the player's `player_cash` in the same wallet and asset | CT-PRE: `withdrawal_rejected` (hold → cash); CT-NEVER-SENT: M3 `RejectCreated` + `withdrawal.Fail` → `withdrawal_failed` (hold → cash). §5 |
| `dispatch_via_governed_permit` | CT-NEVER-SENT | a single-use permit that lets H's T2 re-claim dispatch **this one** never-sent payout despite resolution-only. Kill switch, KYC gate and credential resolution still apply. | none by the resolution. A later provider success posts the normal Step B by evidence |
| `retain_pending_determination` | all classes | records a decision to keep the hold; no state change | none |

**Deliberately NOT expressible** without a new posting shape and a separate human decision
(HD-CTF-5): transfer to a third party (an unclaimed-funds authority, a regulator, escrow, a
successor operator); partial release; conversion to another asset; a fee or charge; dispatch of a
CT-PRE withdrawal (that would need the full approve/submit path on behalf of a closed tenant).

### 4.2 The rule rows (configuration, not code)

Table `closed_tenant_resolution_rules` (append-only, effective-dated; family P for writes, R-style
read):

| Column | Meaning |
|---|---|
| `id`, `created_at` | |
| `level` | `CHECK IN ('platform','jurisdiction','licence')` |
| `jurisdiction_id` | NOT NULL iff `level = 'jurisdiction'` (FK `jurisdictions`) |
| `licence_id` | NOT NULL iff `level = 'licence'` (FK `licences`). This is how an **own-licence operator's** applicable rule is expressed (ADR 0006) |
| `item_class` | `CHECK IN ('CT-PRE','CT-NEVER-SENT','CT-INFLIGHT','CT-M2','CT-BLOCKED')` |
| `outcome_code` | FK `closed_tenant_resolution_outcomes`; must be in its `applies_to_classes` (trigger) |
| `permitted` | `BOOLEAN NOT NULL` |
| `effective_from` | |
| `change_id` | FK to the approved change that created it |

**Evaluation** `closed_tenant_outcome_permitted(p_tenant, p_class, p_outcome, p_as_of) RETURNS
boolean` (STABLE; one implementation, used by the trigger and the executor; the K2
`financial_policy_required_approvals` style):
1. Resolve `tenants.licence_id` and then `licences.jurisdiction_id`, in-tx.
2. Take the in-force row (latest `effective_from <= p_as_of`) for `(p_class, p_outcome)` at the
   **most specific** level that has one: `licence` > `jurisdiction` > `platform`. Return its
   `permitted`.
3. **No row at any level ⇒ false (fail closed).** **Nothing is seeded.** Until a human-approved rule
   exists, every outcome is disabled and the queue is display-only.
4. If the tenant's jurisdiction is unresolvable while any `jurisdiction`-level row exists for
   `(class, outcome)` ⇒ false (the K2 precedent, `0113:682-687`).
5. Tenant- and brand-level rows **do not exist**: a closed tenant cannot configure its own exit
   (security ruling 5 / `0113:655` precedent).

RECOMMENDATION: "most specific wins" lets a jurisdiction or licence row both permit and forbid. That
follows the human's "the applicable operator/jurisdiction determines the permitted resolution".
Whether the platform row should instead be a **floor** that a jurisdiction cannot loosen is
HD-CTF-1(b).

### 4.3 Rule change governance

The HD-PRH2-7 / K2 pattern:
- `closed_tenant_resolution_rule_changes` (proposal: payload, author staff, author Person, status)
  plus `…_change_approvals`;
- an apply-on-approval trigger inserts the rule row (the `0113:591-617` pattern);
- authoring and approval are **platform principals only**, under a new static governance permission
  `closed_tenant_resolution_rule:author` (platform_admin), with an independent second platform
  Person approver (LF-11 floor, non-configurable);
- every change is audited, effective-dated and append-only, so historical evaluation is
  reproducible;
- the Persons who authored or approved the rule row that permits the chosen outcome may not request
  or approve a resolution relying on it (S-2(iii) precedent; RECOMMENDATION, security confirms).

## 5. Resolution requests, four-eyes and execution

### 5.1 Tables

**`closed_tenant_hold_resolutions`** (families **A** for writes; T SELECT-only per HD-CTF-2; no P):

| Column | Type / constraint |
|---|---|
| `id` | `UUID PK` |
| `tenant_id` | `UUID NOT NULL` |
| `withdrawal_request_id` | composite FK `(tenant_id, …)` → `withdrawal_requests` |
| `attempt_id` | `UUID NULL`, composite FK; NOT NULL iff class = CT-NEVER-SENT (trigger-derived) |
| `item_class` | derived by trigger at insert, re-derived at execution |
| `outcome_code` | FK outcomes |
| `rule_id_at_submission`, `rule_id_at_execution` | the rule row that permitted it |
| `jurisdiction_id_at_submission`, `licence_id_at_submission` | recorded |
| `amount`, `asset_code`, `wallet_id`, `player_account_id` | copied from the withdrawal |
| `reason_code` | FK `closed_tenant_resolution_reason_codes` (closed catalogue; vocabulary in §5.5) |
| `evidence_ref_hash` | `TEXT NULL CHECK (~ '^[0-9a-f]{64}$')`, required for `release_*` and `dispatch_*` (RECOMMENDATION) |
| `payload_hash` | DB-computed over every field above |
| `requested_by`, `requested_by_scope`, `requested_by_person_id` | forced from the session; scope must be `platform` (HD-CTF-2) |
| `tenant_status_at_submission`, `tenant_status_at_execution` | must be `closed` at both |
| `required_at_submission`, `contributing_policy_ids` | K2 policy evaluation |
| `state` | `pending`, `executing`, `executed`, `rejected`, `cancelled`, `expired`, `refused_at_execution` |
| `expires_at` | from the policy, as K2 does (no invented value) |
| `executed_txid` | |
| `ledger_transaction_id` | `UUID NULL UNIQUE`; NOT NULL iff executed release |
| `permit_id` | NOT NULL iff executed dispatch permit |

Partial UNIQUE indexes:
- `(withdrawal_request_id) WHERE state = 'pending'`;
- `(withdrawal_request_id) WHERE state = 'executed' AND outcome_code <> 'retain_pending_determination'`
  (at most one money-effective resolution per withdrawal; `retain` may repeat).

**`closed_tenant_hold_resolution_approvals`:** the same shape as K2/K3 approvals (payload-hash
pinned, forced approver, `decided_txid`, UNIQUE `(resolution_id, decided_by)`, immutable).

**`closed_tenant_dispatch_permits`:** `id`, `tenant_id`, `attempt_id`, `resolution_id`,
`created_txid`, `consumed_at NULL`, `consumed_txid NULL`, `voided_by_resolution_id NULL`. Partial
UNIQUE `(attempt_id) WHERE consumed_at IS NULL AND voided_by_resolution_id IS NULL`. Only
`consumed_*` and `voided_*` are updatable, and only once (trigger).

### 5.2 Governance

| Aspect | Rule |
|---|---|
| Operation kind | new `financial_control_classifications` row `closed_tenant_hold_resolution`. **RECOMMENDATION:** `mandatory_four_eyes` (HD-PRH2-1 requires an explicit classification; security/LF confirm) |
| Capabilities | new enum rows `closed_tenant_hold_resolution:request` / `:approve` (ADR 0099 §3: extended by migration + ADR). `eligible_tenant_roles = '{}'` (no tenant grantee, HD-CTF-2); `platform_grantee_allowed = true`. Grants follow HD-PRH2-2 (c): a platform grant needs an independent second platform approver. |
| Policy | `financial_policy_required_approvals('closed_tenant_hold_resolution', tenant, brand, asset, amount, now)`, `GREATEST(1, …)`. No platform policy row ⇒ disabled. K2's non-active special case (`0113:655`) is widened to this operation (tenant and brand rows ignored), by CREATE OR REPLACE on the latest body. **No threshold is seeded** (HD-PRH2-3). |
| Independence floor | **LF-11, non-configurable:** distinct, non-NULL `person_id` for the requester and each approver |
| Beneficiary (S-12) | trigger `closed_tenant_hold_resolutions_beneficiary_guard`: requester and approver Persons ≠ `withdrawal_requests.player_account_id → player_accounts.person_id`. A NULL staff Person or a missing lookup row fails closed. Re-checked at execution. |
| Counting | ADR 0100 §6.2 exactly: grants and staff `FOR SHARE`, in force at execution, S-2(iii), S-4 live re-read (suspended or demoted actors refused despite a valid JWT), S-11 voiding |
| HD-PRH2-8 interim | at least one independent approver |
| Sock-puppet | not reachable: tenant grantees are excluded entirely; platform grants need platform co-approval |

### 5.3 Execution (in the final approval's transaction; ADR 0082 A8 lock order)

Common prefix, steps 1–8:
1. **L1 parent:** `withdrawal_requests` `FOR UPDATE`. CT-NEVER-SENT uses
   `withdrawal.LockSubmittedForResolution` (`:1184`); CT-PRE uses the new function's own lock.
2. **L1 attempt** `FOR UPDATE` (CT-NEVER-SENT only).
3. **L1 resolution** `FOR UPDATE`. It must be `pending` and unexpired.
4. Insert the approval. 5. Evaluate; if short of the requirement, commit.
6. **L1 staff, then grants**, `FOR SHARE` ascending id. Recount.
7. **Re-check at execution:**
   - `tenants.status = 'closed'`, read in-tx and recorded (no lock, see §6 R-CT-3);
   - `closed_tenant_outcome_permitted(...)` is still true as of now, with the rule id recorded;
   - the item class is re-derived and unchanged;
   - the withdrawal state is still in the class.
   - For CT-NEVER-SENT: `attempt.state = 'created' AND NOT ever_possibly_sent`.
8. Resolution → `executing`, `executed_txid = txid_current()`.

Then, by outcome:

| Outcome × class | Steps 9–10 | Posting and key |
|---|---|---|
| release × CT-NEVER-SENT (governed **M3**) | `payments.RejectCreated(tx, attempt, EvidenceOperator, "closed_tenant_release")` (CAS `state='created'`; guard and CHECK enforce `NOT ever_possibly_sent`) → `withdrawal.Fail(tx, wr, "closed_tenant_release")` | `withdrawal_failed`, key `wr.id:failed` (`withdrawal.go:1584`), hold → `player_cash`, `ReversesTransactionID` = hold tx |
| release × CT-PRE | **new** `withdrawal.ReleaseForGovernedResolution(tx, wr, resolutionID, reasonCode)`: legal from `requested`/`pending_review`/`approved`; → `rejected` | `withdrawal_rejected`, hold → `player_cash`. **Key: RECOMMENDATION `wr.id:rejected`** (shared with `Reject`, so the two are mutually exclusive by key as well as by state); LF rules (Q-CT-LF-1) |
| dispatch permit × CT-NEVER-SENT | insert `closed_tenant_dispatch_permits`; void any earlier unconsumed permit | none |
| retain × any | none; void any unconsumed permit (an explicit retain withdraws a dispatch permission) | none |

Then: → `executed` (+ `ledger_transaction_id` / `permit_id`), audit, commit.

- **Deferred constraint trigger:** no `executing` row at commit. An executed release links a
  same-tenant `withdrawal_failed`/`withdrawal_rejected` with `correlation_id = wr.id` and the
  expected key, and the withdrawal is `failed`/`rejected`.
- **Ledger rules:** postings go only through `ledger.Post`, which takes `LockProjectionsForPosting`
  (L3/L4, `lockorder.go:113`). There is never a balance UPDATE. No negative balance: the debit is
  on `player_withdrawal_hold` for exactly `wr.Amount`, which the hold posting created, and
  `ledger.Post`'s projection check refuses otherwise. SUM(D) = SUM(C) holds by construction. A
  `withdrawal_*` posting cannot carry `ledger_transactions.reason_code` (`lockorder.go:358-365`),
  so the closed-catalogue reason code is recorded on the resolution and in the audit, with
  `terminal_reason = 'closed_tenant_release'` on the attempt.
- **Idempotency:** the resolution id; the partial UNIQUE indexes; the deterministic ledger keys
  derived from `wr.id` (a second release cannot post); the permit UNIQUE.

### 5.4 Acting-session fences (extend K3's 0115 objects in this workstream's migration)

- `ledger_governed_fence_allows` gains:
  - **(d)** `withdrawal_failed` keyed `wr.id:failed` with an executing `release` resolution of class
    CT-NEVER-SENT, `executed_txid = txid_current()`, `correlation_id = wr.id`;
  - **(e)** `withdrawal_rejected` keyed per Q-CT-LF-1 with an executing CT-PRE `release`.
- K3's acting UPDATE WITH CHECK on `payment_attempts` gains one OR arm: an executing CT-NEVER-SENT
  release for this attempt, `NEW.state = 'rejected'`, `last_evidence_kind = 'operator'`. The arm on
  `withdrawal_requests` admits an executing closed-tenant resolution for this withdrawal.
- **`payment_attempts_guard()` is not edited** unless `payments` finds that 0107 refuses payout
  `created → rejected` with `operator` evidence. In that case the change is an exact, reviewed diff
  (the F12 discipline), with a deposit matrix test.

### 5.5 Reason-code catalogue (proposed vocabulary; LF and compliance confirm; HD-CTF-7)

`closed_tenant_resolution_reason_codes`: `player_request_after_closure`,
`operator_instruction_documented`, `regulator_instruction_documented`,
`legal_determination_documented`, `retained_pending_determination`. These are labels for *why staff
acted*, each requiring `evidence_ref_hash` except `retained_pending_determination`. They grant no
power. The outcome permission comes only from §4.

## 6. Interaction with H, K3 and the rest of the platform

| Topic | Interaction |
|---|---|
| **H resolution-only sweeper** | Unchanged for every attempt without a permit: T2 and T12 stay withheld for a non-active tenant. **One change, for the dispatch outcome only:** in `reclaimPayoutCreated`'s claim transaction, `checkPayoutResolutionOnly` (`sweeper_resolution_only.go:75`, `payout_sweep.go:271`) treats a closed tenant as not blocked **iff** an unconsumed, unvoided permit for this attempt exists. It consumes it (`consumed_txid = txid_current()`) in the same transaction, **at the same position** (after the withdrawal lock, before the kill-switch check and the KYC gate; ADR 0095 §37.3 safeguard 1). Kill switch, KYC gate and per-tenant credentials apply unchanged. A NotSent result returns the attempt to `created` with the permit consumed, so a new resolution is needed (no silent retry loop). |
| **T12** | never permitted by this path (an `ambiguous` attempt may have been sent: CT-INFLIGHT goes to M2) |
| **`ever_possibly_sent`** | release requires `NOT ever_possibly_sent` (CAS, INV-IO-9 CHECK, guard). Any attempt ever possibly sent is excluded from release here, permanently (INV-IO-7) |
| **Race: release vs T2/permit** | both lock the withdrawal row first (L1), so they serialize. The release CAS needs `created`; the T2 CAS needs `created`. Exactly one wins; the loser fails its state check |
| **Race: CT-PRE release vs HTTP `ClaimForDispatch` / `Approve` / `Reject` / `Cancel`** | all take the withdrawal L1 lock; state checks make exactly one terminal. H-SEC-5 (no tenant-status gate on HTTP initiation or dispatch) remains a separate launch item; this design does not close it |
| **K3 M2** | CT-INFLIGHT and CT-M2 route to M2 (available for closed tenants). The two resolution tables are disjoint (attempt-keyed vs withdrawal-keyed). A trigger refuses a closed-tenant resolution while an M2 resolution is `pending` for the same withdrawal's attempt, and K3's executor refuses M2 while a closed-tenant resolution is `pending` (cross-check; RECOMMENDATION) |
| **Reconciliation** | a released CT-NEVER-SENT attempt is `rejected`. A later statement success line for it gives `pay_status_mismatch` (`payment_statement.go:1084-1086`), and a later callback gives T15 (`receipt.go:756`). Both stay loud. No new kind is needed |
| **MA020 / K2** | not involved (no `manual_adjustment`) |
| **Tenant reopening** | if a `closed` tenant is set back to `active` between submission and execution, execution refuses (`tenant_status_at_execution` must be `closed`); the normal sweeper resumes |

## 7. Audit and tenant isolation

- Every submission, approval, rejection, cancellation, expiry, refusal, execution, rule change and
  permit consumption writes `audit_log` in the same transaction. Each record carries actor, scope,
  acting tenant, Persons, item class, outcome, rule id, jurisdiction and licence, reason code,
  evidence hash, before/after of the withdrawal and attempt, the ledger transaction or permit, IP,
  UA and request id.
- Acting rows follow ADR 0099 §10.6 (`audit_log_acting_actor`). The tenant-visible projection (ADR
  0104 / HD-PRH2-5) applies; for a closed tenant it matters for the record and for any successor or
  regulator access.
- RLS: every new tenant-owned table is ENABLE + FORCE RLS, with families A (write) and T (SELECT
  only, HD-CTF-2) on resolutions, approvals and permits, and P (write) plus a read policy on the
  rules and changes tables. Delete and truncate are denied; there is no `FOR ALL` and no
  `SECURITY DEFINER`. **No policy is added on `tenants`, `licences` or `jurisdictions`**, so the 0077
  exact whitelist (11 tuples) is unaffected. The FK references to `jurisdictions`/`licences` need no
  policy.
- The sweeper's permit read runs in the tenant's own `WithTenant` transaction, the H pattern.

## 8. Schema (migration: next free number after K3, allocated at merge)

New: `closed_tenant_resolution_outcomes` (R, 3 rows), `closed_tenant_resolution_reason_codes` (R),
`closed_tenant_resolution_rules`, `closed_tenant_resolution_rule_changes`,
`…_rule_change_approvals`, `closed_tenant_hold_resolutions`, `closed_tenant_hold_resolution_approvals`,
`closed_tenant_dispatch_permits`; the function `closed_tenant_outcome_permitted`; the triggers
(payload immutability, forced actor, beneficiary guard, class derivation, state machine, deferred
check, permit one-shot); the classification row, two capability rows and one governance permission
row; and CREATE OR REPLACE of `financial_policy_required_approvals` (non-active special case),
`ledger_governed_fence_allows` (+(d),(e)) and K3's acting WITH CHECKs.

Grants (`init-app-role.sql` append, the K2 loop pattern): R tables SELECT; resolutions and rule
changes SELECT/INSERT/UPDATE; approvals and rules SELECT/INSERT; permits SELECT/INSERT/UPDATE.

Down: refuses while any resolution, permit, rule or rule-change row exists; otherwise restores
K3's (0115's) function bodies byte-for-byte. It must succeed on an empty scratch DB (the 0075/0077
full-chain rollbacks).

## 9. Tests (DoD)

Every test asserts SUM(D) = SUM(C) and projection = recomputed.

| Class | Tests |
|---|---|
| R | release CT-NEVER-SENT → `rejected` + `withdrawal_failed` hold→cash, keys exact; release CT-PRE from each of `requested`/`pending_review`/`approved`; retain posts nothing; a dispatch permit lets exactly one T2 re-claim through for a closed tenant, and no other attempt of that tenant |
| Config | nothing seeded ⇒ every outcome refused; platform row only; a jurisdiction row overrides; a licence row overrides; unresolvable jurisdiction + a jurisdiction row ⇒ refused; a rule changed between submission and execution ⇒ re-evaluated (refused if now forbidden); tenant/brand rows impossible |
| AZ | S-12 beneficiary refusal (requester and approver); LF-11 same Person refused; self-approval; no grant; revoked/expired at execution; suspended/demoted actor with a valid JWT (S-4); tenant-scoped staff refused (HD-CTF-2 default); a rule author as requester/approver refused; sock-puppet (a tenant-minted grantee) refused |
| CON (`-race -count=50`) | release vs sweeper T2 with a permit; release vs HTTP `ClaimForDispatch`; two final approvals; release vs K3 M2 pending; permit consumption by two sweeper instances (one consumes) |
| ADV | release of an `ever_possibly_sent` attempt (CT-INFLIGHT, T14, ambiguous) refused by executor, guard and CHECK; release for an active or suspended tenant refused; dispatch permit for CT-PRE refused; second release refused by key and index |
| FL/RB | fault injection in `Fail`/`ReleaseForGovernedResolution` (the ADR 0095 §36.7 lock-timeout technique) → full rollback; a committed `executing` row is impossible |
| RLS | tenant isolation; plain platform refused; acting only with the grant for X; T family read-only; probe list (the C-40 pattern) |
| AU | the audit rows above, including the acting actor and the tenant projection |
| MIG | up/down/up on a HEAD-migrated scratch DB; down refusal; byte-for-byte restoration of K3's functions; the 0077 whitelist unchanged; 0075/0077 chain |
| H regression | every H resolution-only test passes unchanged without a permit; the H-CR-4 ordering mutant (resolution-only after KYC) is still killed with the permit hook present |
| Mutants | drop `NOT ever_possibly_sent`; drop the tenant-status re-check; drop the rule re-evaluation; let a permit be reused; drop the beneficiary guard; let a tenant-scope actor request; drop `executed_txid = txid_current()` from the fence (d)/(e) |

## 10. Threats and residuals

| ID | Threat / residual | Control |
|---|---|---|
| TM-CT-1 | A platform insider releases or dispatches a closed tenant's funds to themselves | S-12, LF-11, grants with platform co-approval, rule/executor separation, audit, tenant projection |
| TM-CT-2 | Rule tampering to permit an outcome | four-eyes rule changes by platform principals only; effective-dated, append-only; rule id recorded on each resolution |
| TM-CT-3 | A dispatch permit used for another attempt, or twice | permit bound to `attempt_id`; one-shot UNIQUE; consumed in the claim transaction |
| TM-CT-4 | Release of a payout that may have been sent (double payout) | `NOT ever_possibly_sent` at three layers; CT-INFLIGHT excluded |
| R-CT-1 | Released funds sit in a closed tenant's `player_cash` with no player access path | HD-CTF-3 |
| R-CT-2 | Unrequested balances are not covered | HD-CTF-4 |
| R-CT-3 | The tenant status is read without a lock (FOR SHARE on `tenants` would need UPDATE privilege): a reopen racing an execution | money-safe: the outcome is permitted only for `closed`; the status is recorded; a reopened tenant's normal flows resume. Disclosed |
| R-CT-4 | CT-BLOCKED payouts cannot be released | PAYOUT-AMOUNT-DISPUTE-1, PAY-PAYOUT-UNBOUND-HOLD-1 |
| R-CT-5 | Nothing alerts on "a closed tenant holds funds" | ALERT-DELIVERY-1 OPEN; metric only; HD-CTF-8 |
| R-CT-6 | HTTP initiation and dispatch paths do not read tenant status (H-SEC-5) | PAY-H-FOLLOWUPS-1 item 4, a separate launch item |

## 11. Separate human decisions (not decidable by the architecture; nothing is invented)

| ID | Question |
|---|---|
| **HD-CTF-1** | (a) Which outcomes (§4.1) are permitted, per jurisdiction (Anjouan first) and per own-licence operator, for each item class: the content of the first rule rows. (b) Is the platform row a floor that a jurisdiction may not loosen, or does the most specific level win? |
| HD-CTF-2 | May staff of the closed tenant itself see, request or approve? (Default: see at most; never act.) |
| HD-CTF-3 | After `release_hold_to_player_cash`, how does the player actually obtain the money from a closed tenant (player access after closure, a withdrawal path, notification)? |
| HD-CTF-4 | What happens to closed-tenant player balances **not** under a withdrawal? |
| HD-CTF-5 | Is any outcome outside §4.1 legally required (a third-party or unclaimed-funds transfer, escrow, a successor operator)? Each needs a new posting shape and its own design |
| HD-CTF-6 | May a tenant be set to `closed` while hold-bearing withdrawals exist? (Architect recommends an interim operational rule, optionally a DB guard, refusing it.) |
| HD-CTF-7 | The reason-code vocabulary (§5.5) and the evidence obligations |
| HD-CTF-8 | Is a dedicated alert, a notification to the player or regulator, or a deadline required? No timelines, recipients or retention periods are set here |
| HD-CTF-9 | May a `closed` tenant ever be reopened? (This affects R-CT-3 and the queue lifecycle) |

Thresholds and approval counts are configuration under HD-PRH2-3 (none seeded).

## 12. Ownership, files, reviewers, DoD

- **Owner:** `payments` implements; `ledger-finance` owns the invariants (keys, postings, Q-CT-LF-1);
  `architect` owns the ADR; `identity-compliance` reviews the jurisdiction semantics.
  **Reviewers:** security (new privileged power, threat model), LF, QA, code-reviewer, POP (scope),
  architect.
- **Sequencing:** after K3 merges (it builds on 0115's fence, acting policies and the governance
  pattern). Its migration is the next free number at merge (Rule 3).
- **Files (indicative; the Touches list is recorded by the orchestrator):** new migration; new
  `internal/payments/closed_tenant_resolution.go` (service, queue, executor);
  `internal/withdrawal/withdrawal.go` (**one new function**, `ReleaseForGovernedResolution`; a
  Rule 1 Touches item, payments F3 lifted explicitly for this workstream);
  `internal/payments/sweeper_resolution_only.go` + `payout_sweep.go` (the permit hook only; H
  files, modified for this directly related residual, ADR 0105 §4); a new
  `internal/httpserver/closed_tenant_resolution_routes.go` + one line in `routes.go`;
  `internal/auth/permission.go`; `deploy/init-app-role.sql` (append);
  `backoffice/src/auth/permissions.ts`; tests.
- **DoD:** the ADR (numbered); ADR 0095 §37.5 updated (residual → mechanism IMPLEMENTED (MOCK),
  outcomes per HD-CTF-1); a runbook entry "Closed tenant: held player funds"; the
  withdrawal-state-machine doc (the new release edge); security-architecture (the new capability and
  family use); HANDOVER. **Label until HD-CTF-1 rule rows exist: mechanism IMPLEMENTED, every
  outcome disabled by configuration.** Never "player funds resolved".
- **Launch status:** a **launch blocker for any tenant-closure flow** (ADR 0105 §1), not for
  operating active tenants.

## 13. Open questions for reviewers

| ID | To | Question |
|---|---|---|
| Q-CT-LF-1 | LF | The CT-PRE release key (`wr.id:rejected` shared with `Reject`, or a distinct one) and the transaction type (`withdrawal_rejected`) |
| Q-CT-LF-2 | LF | Confirm that a governed M3 uses `withdrawal_failed` (as ADR 0095 §4.8 M3 does) |
| Q-CT-SEC-1 | security | Platform-only requesters/approvers for closed tenants; T family read-only |
| Q-CT-SEC-2 | security | The new capability pair and the classification row vs reusing `payment_force_resolve` |
| Q-CT-SEC-3 | security | The permit hook inside H's resolution-only gate (position, one-shot) |
| Q-CT-SEC-4 | security | Rule-author / executor Person separation |
| Q-CT-PAY-1 | payments | Does 0107 admit payout `created → rejected` with `operator` evidence? |
| Q-CT-POP-1 | product-owner-proxy | The three-outcome catalogue and the five item classes are the minimum, not more |
