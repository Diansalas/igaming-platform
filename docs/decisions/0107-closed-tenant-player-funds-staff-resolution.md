# ADR 0107 — Closed-tenant player funds: the governed staff resolution path (HD-PRH2-9)

- **Status: PROPOSED — DESIGN ONLY.** This ADR is **NOT part of the PRH-2 implementation** and
  authorizes no code. Before any code:
  - the HD-CTF-* human decisions (§12) are answered;
  - `security` re-reviews this ADR (its design-record verdict requires that);
  - `ledger-finance` confirms it;
  - `product-owner-proxy` concurs on scope;
  - the human authorizes a workstream.

  **Until HD-CTF-1 is answered, the mechanism stays "every outcome disabled by configuration".**
- **Pointer (2026-10-05, PRH-2 R3 / H-W1):** reconciliation OBSERVATION of non-active tenants now exists
  (evidence only, `payment_statement` stream, ADR 0095 §40.4 / ADR 0101 §28.3). It implements nothing of
  this ADR: this stays DESIGN ONLY, and no resolution, dispatch, release or posting is added for a closed tenant.
- **Pointer (2026-10-05, R3-GAME-POSTINGS-NONACTIVE-1, ADR 0095 §40.5):** NEW casino and sportsbook gameplay postings are now REFUSED for a suspended or closed tenant (owner decision, fail closed; migration 0118). This ADR is still DESIGN ONLY and adds no resolution path. Consequence relevant here: rounds and bets open at closure strand (no casino or sportsbook staff resolution path exists), so HD-CTF-6's interim rule should be read as "refuse closing a tenant while hold-bearing withdrawals OR open casino rounds / sportsbook bets exist". Closed-tenant staff access is still not refused by auth; the refusal is in the posting transaction.
- **Pointer (2026-10-06, PRH-2 R5, ADR 0095 §40.6, migration 0121):** terminal stake returns (casino rollback of a posted bet, sportsbook void of an open bet and void after settlement) are now ALLOWED on a non-active tenant (Q-GP-5), and closing a tenant is REFUSED while it has open sportsbook bets (Q-GP-1, the sportsbook half of HD-CTF-6's interim rule; casino rounds are not representable, Q-GP-6). The hold-bearing-withdrawal half of HD-CTF-6 is not implemented by this change. This ADR is still DESIGN ONLY.
- **Source:** ADR 0105 §1 (HD-PRH2-9, human decision: a controlled STAFF RESOLUTION PATH); ADR
  0095 §37.5 (H LF F3 residual); ADR 0101 rev 3/4 §19 (why this is separate from K3).
- **Reviews of the design record** (`3158cf0`):
  - `reviews/k3-design-security.md`: ACCEPT WITH CONDITIONS as a design record, CT-R1..CT-R7 and
    Q-CT-SEC-1..3;
  - `reviews/k3-design-ledger-finance.md`: ACCEPT WITH CONDITIONS, CT-1..CT-6, Q-CT-LF-1/2.

  Every condition is written in below. §15 maps each condition to the section that addresses it.
- **Owner:** `architect` (ADR). `payments` implements; `ledger-finance` owns the financial
  invariants; `identity-compliance` reviews the jurisdiction semantics. **Reviewers:** security,
  LF, QA, code-reviewer, POP, architect.
- **Registry:** workstream PAY-CLOSED-TENANT-FUNDS-RESOLUTION-1 (to be registered). It implements
  the mechanism half of HD-TENANT-CLOSURE-PLAYER-FUNDS-1. The legal half is HD-CTF-1..9, which is
  OPEN.
- **Labels:**
  - "Requirement" means the human (ADR 0105 §1) or a binding review ruling.
  - "RECOMMENDATION" means an architect proposal.
  - "Example only" means an outcome the mechanism must be able to *express*. Whether it is
    *permitted* is a human or legal decision.

## 1. Requirements (ADR 0105 §1)

| # | Requirement |
|---|---|
| R1 | No automatic payout dispatch for a closed tenant |
| R2 | No automatic cancel or release without a controlled resolution |
| R3 | Flow: closed tenant → outstanding player payout/hold → authorized financial-staff queue → authorization / four-eyes under the applicable policy → permitted resolution → deterministic ledger operation → complete audit trail |
| R4 | Configurable and jurisdiction-aware; no universal legal outcome hard-coded |
| R5 | Tenant isolation, financial invariants, idempotency, actor/subject separation, auditability, four-eyes; no direct balance mutation; no unrestricted bypass |
| R6 | A legally required outcome or threshold the architecture cannot determine is a separate human decision |

## 2. Item classes

Holds are posted at `requested` (`withdrawal_requested`, `withdrawal.go:496`). Each is released by
exactly one terminal posting. For a tenant with `tenants.status = 'closed'`:

| Class | Withdrawal state | Attempt (one per withdrawal, `0101:140`) | Path |
|---|---|---|---|
| **CT-PRE** | `requested`, `pending_review`, `approved` | **none** (an attempt is inserted only at `submitting`, together with `submitted`, `payout.go:240-250`) | this ADR |
| **CT-NEVER-SENT** | `submitted` | `created`, `NOT ever_possibly_sent` (INV-IO-9) | this ADR (governed M3) |
| CT-INFLIGHT | `submitted` | `submitting` / `pending` / `ambiguous` | evidence, or **K3 M2** (ADR 0101; closed tenants are platform-acting only, ADR 0101 §24.5) |
| CT-M2 | `submitted` | `disputed`, M2-admitted | **K3 M2** |
| CT-BLOCKED | `submitted` | `disputed`, not M2-admitted | **`retain` only**, until PAYOUT-AMOUNT-DISPUTE-1, PAY-PAYOUT-UNBOUND-HOLD-1 or PAY-PAYOUT-CONTRADICTION-HOLD-1 |

- **LF CT-6:** a payout that a permit dispatches and that becomes `ambiguous` is CT-INFLIGHT. It
  routes to M2, never back to this path.
- **Not covered:** closed-tenant balances not under a withdrawal (HD-CTF-4).

## 3. Queue and session shapes (security CT-R6)

The queue is a read model (a Go query, not a view). Every read and write has an explicit session
shape:

| Use | Session shape | Tables read / written, and the policy that admits each |
|---|---|---|
| Queue read, resolution request/approve/execute | **acting** (`WithPlatformActingInTenant`; an explicit in-force `closed_tenant_hold_resolution:*` grant for X) | read: `withdrawal_requests` (acting_read, ADR 0101 0115), `payment_attempts` (acting_read, 0113), `player_accounts` and `wallets` (acting_read, 0113), `tenants` (`tenants_read`, available to every non-player session, `0077:55`; no new policy), `licences` (`acting_read_own_licence`, 0113), `jurisdictions` (existing read), the rules table (reference read), and the resolutions, approvals and permits (acting policies, this ADR). Write: the resolutions, approvals and permits; the governed postings through the fences (§7.3). **Every table the queue joins has an acting read policy, or the queue fails closed.** |
| Permit consumption | **system** (`WithTenant`: `app.tenant_id` only; principal, player, platform-admin, platform-service and acting all NULL), from H's sweeper T2 re-claim transaction | `closed_tenant_dispatch_permits`: a system-shape SELECT and a system-shape UPDATE of `consumed_*` only (§6.3) |
| Reconciliation | system snapshot | none of these tables is needed by reconciliation |
| Rule changes | **platform** (`WithPlatformAdmin`; `closed_tenant_resolution_rule:author`) | the rule-change tables and approvals (family P) |
| Tenant staff of the closed tenant | — | **no T-family read** until HD-CTF-2 is answered (security Q-CT-SEC-1). The ADR 0104 projection remains the record |

Discovery: a platform principal lists `closed` tenants through `tenants_read`. Per-tenant items need
the acting grant. No platform-wide read of tenant money rows is created. The runbook step is: grant
designated platform finance staff the capability for X, then work the queue.

## 4. Configurable, jurisdiction-aware permitted outcomes (R4)

### 4.1 The outcome catalogue (mechanics only)

Table `closed_tenant_resolution_outcomes(code PK, applies_to_classes text[] NOT NULL, posting_shape
text NOT NULL CHECK (posting_shape IN ('none', 'withdrawal_failed_hold_to_cash',
'withdrawal_rejected_hold_to_cash')))`. It is migration-seeded. **LF CT-4:** every `posting_shape`
maps one-to-one to a code path, and a pin test enforces that.

| Code | Classes | `posting_shape` | Effect |
|---|---|---|---|
| `release_hold_to_player_cash` | CT-PRE | `withdrawal_rejected_hold_to_cash` | §7.2 |
| `release_hold_to_player_cash` | CT-NEVER-SENT | `withdrawal_failed_hold_to_cash` | governed M3, §7.1 |
| `dispatch_via_governed_permit` | CT-NEVER-SENT | `none` | a single-use permit for one T2 re-claim, §6 |
| `retain_pending_determination` | all | `none` | records the decision; voids any unconsumed permit |

Because `posting_shape` differs by class, the catalogue key is `(code, item_class)`. **Every row is
example only. Legal permission is HD-CTF-1.**

**Not expressible** without a new posting shape and a human decision (HD-CTF-5):
- a third-party, unclaimed-funds, regulator, escrow or successor transfer;
- a partial release;
- a conversion;
- a fee;
- dispatch of a CT-PRE withdrawal.

### 4.2 Rule rows

Table `closed_tenant_resolution_rules`: append-only and effective-dated.

| Column | Meaning |
|---|---|
| `level` | `platform`, `jurisdiction` or `licence` (`licence` expresses an own-licence operator, ADR 0006) |
| `jurisdiction_id` / `licence_id` | set iff the level requires it |
| `item_class`, `outcome_code` | FK to the catalogue |
| `permitted` | boolean |
| `effective_from` | effective date |
| `change_id` | the approved change that created the row |
| `author_person_id`, `approver_person_id` | copied from the change |

`closed_tenant_outcome_permitted(p_tenant, p_class, p_outcome, p_as_of) → (permitted boolean,
rule_id uuid)` (STABLE, one implementation):
1. Read `tenants.licence_id` and then `licences.jurisdiction_id`, in-tx.
2. Take the latest in-force row for `(class, outcome)` at the most specific level present:
   `licence` > `jurisdiction` > `platform`.
3. **No row ⇒ false.** Nothing is seeded.
4. An unresolvable jurisdiction while a `jurisdiction`-level row exists for `(class, outcome)` ⇒
   false (the K2 precedent, `0113:682-687`).
5. There are no tenant or brand rows.

Whether the platform row is a **floor** or the most specific level wins is **HD-CTF-1(b)**. The
function's precedence is configured by that answer before any rule row is accepted. Until then,
rule changes are refused.

### 4.3 Rule change governance

- `closed_tenant_resolution_rule_changes` plus `…_change_approvals`. An apply-on-approval trigger
  writes the rule row (the `0113:591-617` pattern).
- Platform principals only, with the static `closed_tenant_resolution_rule:author` permission
  (platform_admin).
- **Author Person ≠ approver Person** (LF-11, non-configurable), enforced in the DB.
- **`effective_from` is forced to be ≥ the approval time**: no backdating (security CT-R5).
- Every change is audited and append-only.

## 5. Resolutions, governance and DB guards

### 5.1 Tables

**`closed_tenant_hold_resolutions`** (FORCE RLS; family **A** for read and write; no T until
HD-CTF-2; no P):
- `id`: server-forced.
- `tenant_id`, `withdrawal_request_id`: composite FK.
- `attempt_id`: NOT NULL iff CT-NEVER-SENT. Composite FK through `payment_attempts_id_tenant_key`
  (ADR 0101 0115, D-8).
- `item_class`: derived at insert and re-derived at execution.
- `outcome_code`.
- `rule_id_at_submission`, `rule_id_at_execution`.
- `jurisdiction_id_at_submission`, `licence_id_at_submission`.
- `amount`, `asset_code`, `wallet_id`, `player_account_id`: copied from the withdrawal.
- **Pinned (security CT-R7 / R-6):** `withdrawal_state_at_submission`,
  `attempt_state_at_submission` (NULL for CT-PRE) and `item_class_at_submission`, all in
  `payload_hash`.
- `reason_code`: FK to the closed catalogue (§5.4).
- `evidence_ref_hash`: required for `release_*` and `dispatch_*`.
- `payload_hash`: DB-computed.
- `requested_by`, `requested_by_scope`, `requested_by_person_id`: forced. The scope must be
  `platform_acting`.
- `tenant_status_at_submission`, `tenant_status_at_execution`: both must be `closed`.
- `required_at_submission`, `contributing_policy_ids`.
- `state`: `pending`, `executing`, `executed`, `rejected`, `cancelled`, `expired` or
  `refused_at_execution`.
- `expires_at`: DB-forced from the policy.
- `executed_txid`.
- `ledger_transaction_id`: `UUID NULL UNIQUE`.
- `permit_id`.

Partial UNIQUE indexes:
- `(withdrawal_request_id) WHERE state = 'pending'`;
- `(withdrawal_request_id) WHERE state = 'executed' AND outcome_code <> 'retain_pending_determination'`.

**`closed_tenant_hold_resolution_approvals`:**
- the same shape as K2/K3 approvals: payload-pinned, forced approver, `decided_txid`, UNIQUE
  `(resolution_id, decided_by)`, immutable;
- family A only.

### 5.2 Governance

| Aspect | Rule |
|---|---|
| Classification | a new `financial_control_classifications` row `closed_tenant_hold_resolution` = `mandatory_four_eyes` (**security Q-CT-SEC-2 CONFIRMED**). The `0113:85` CHECK is widened by migration |
| Capabilities | a new pair `closed_tenant_hold_resolution:request` / `:approve` (**CONFIRMED; do not reuse `payment_force_resolve`**). The `0112:221` operation-kind CHECK is widened. `eligible_tenant_roles = '{}'`; `platform_grantee_allowed = true`. **G-P2 grants only with a NOT NULL `valid_until`.** No static permission for `tenant_admin` |
| Requester and approvers | **platform_acting only** (**Q-CT-SEC-1 CONFIRMED**) |
| Policy | `financial_policy_required_approvals('closed_tenant_hold_resolution', …)`, `GREATEST(1, …)`. No platform row ⇒ disabled. The `0113:655` non-active special case is extended to this operation. **No threshold is seeded** (HD-PRH2-3) |
| LF-11 floor | distinct, non-NULL Persons; non-configurable |
| S-12 | requester and approver Persons ≠ the withdrawal's player Person (`closed_tenant_hold_resolutions_beneficiary_guard`). A NULL or invisible lookup is refused. Re-checked in the DB recount |
| **Rule author vs executor (security CT-R5, REQUIREMENT)** | Persons who authored or approved the in-force rule row that permits the outcome, **at submission and at execution**, or who authored or approved a contributing financial policy (S-2(iii)), may not request, approve or be counted. DB-enforced at insert and in the recount |
| Optional grant-chain separation (security TM-7 note) | the resolution approver may not be the Person who approved the requester's G-P2 grant, and vice versa (this raises the collusion floor from 2 to 3 platform admins). **RECOMMENDATION; security and the human decide (HD-CTF-2 context)** |
| HD-PRH2-8 interim | at least one independent approver |

### 5.3 DB-guard parity (security CT-R7 applying ADR 0101 §24.3)

ADR 0101 §24.3 (i)–(viii) applies verbatim, with the capability strings
`closed_tenant_hold_resolution:request` / `:approve`. Specifically:
- (i) insert scope is `platform_acting` only;
- (ii) the capability-specific grant at insert and in counting;
- (iii) S-2(iii) at insert;
- (iv) the DB recount at `pending → executing`;
- (v) a non-executed exit after a governed posting is refused (keys §7);
- (vi) `expires_at` is DB-forced;
- (vii) cancel by the requester only, reject by a same-tx decision, execution only in the final
  approval's transaction;
- (viii) server-forced ids.

R-6-style pinning is §5.1. R-9-style routes are §10.

### 5.4 Reason codes (vocabulary proposed; HD-CTF-7)

`closed_tenant_resolution_reason_codes`:
- `player_request_after_closure`;
- `operator_instruction_documented`;
- `regulator_instruction_documented`;
- `legal_determination_documented`;
- `retained_pending_determination`.

These are labels, not powers. Each needs `evidence_ref_hash` except the last. A `withdrawal_*`
posting cannot carry `ledger_transactions.reason_code` (`lockorder.go:358-365`). The code is
therefore recorded on the resolution and in the audit.

## 6. The dispatch permit (security CT-R1, CT-R2; Q-CT-SEC-3)

### 6.1 Table

`closed_tenant_dispatch_permits` has these columns: `id`, `tenant_id`, `attempt_id` (composite FK),
`resolution_id`, `rule_id`, `created_txid`, `expires_at` (DB-forced, §6.2), `consumed_at`,
`consumed_txid`, `voided_by_resolution_id`.

- Partial UNIQUE `(attempt_id) WHERE consumed_at IS NULL AND voided_by_resolution_id IS NULL`.
- A trigger allows exactly these mutations, each **once**:
  - `consumed_at` / `consumed_txid`, from NULL;
  - `voided_by_resolution_id`, from NULL, and only by an executing resolution.

  Every other column is immutable. DELETE and TRUNCATE are denied.
- Policies:
  - acting INSERT, SELECT and UPDATE (void only), for the executor;
  - **a system-shape SELECT and a system-shape UPDATE** (tenant GUC only; every other identity GUC
    NULL; no NULL arm), for H's sweeper consumption.

### 6.2 Expiry

- `expires_at = created_at + <permit max lifetime>`. The lifetime comes from a settings row
  (`closed_tenant_resolution_settings`, key `dispatch_permit_max_lifetime`).
- **With no row, permits cannot be created** (fail closed).
- The implementing workstream proposes a technical security default, which security approves and
  records, following the K1 `acting_grant_max_lifetime` precedent. **No value is set by this ADR.**
- An expired permit is never consumed.

### 6.3 Consumption (H's T2 re-claim, `payout_sweep.go:253`, through `checkPayoutResolutionOnly`, `sweeper_resolution_only.go:75`)

The steps run in the per-item claim transaction (system shape), in this order:

1. The withdrawal L1 lock, then the attempt lock (H's existing order).
2. **The permit hook, at the resolution-only check position: after the L1 locks, before the kill
   switch and the KYC gate** (ADR 0095 §37.3 safeguard 1; H-CR-4's ordering mutant stays killed).
   For a `closed` tenant, it does `SELECT … FOR UPDATE` on the unconsumed, unvoided, unexpired permit
   for this attempt. If there is none, the item is resolution-only blocked as today.
3. **Re-evaluation at consumption (CT-R2).** All must hold, or there is no consumption and the item
   is rescheduled with an audit:
   - the tenant is still `closed`;
   - `closed_tenant_outcome_permitted(..., 'dispatch_via_governed_permit', now())` still holds (its
     rule id is recorded on the permit);
   - the resolution is `executed` and not voided;
   - the attempt is `created AND NOT ever_possibly_sent`;
   - `now() < expires_at`.
4. The kill switch and then the KYC gate, unchanged.
   - **Kill switch blocks:** the permit stays unconsumed (it was only row-locked), the item is
     rescheduled, and the permit remains usable until it expires.
   - **KYC deny:** the H escalation (T16), and in the same transaction the permit is voided
     (`voided_by_resolution_id` = a system void marker recorded in the audit). A new resolution is
     needed. The outcome is deterministic: no silent retry with a stale authorization.
   - **KYC unavailable:** reschedule; the permit stays unconsumed.
5. Immediately before the T2 CAS (`created → submitting`): the permit UPDATE is a CAS
   (`WHERE consumed_at IS NULL AND voided_by_resolution_id IS NULL AND expires_at > now()`,
   **RowsAffected = 1**, or the item aborts).
6. The provider call is outside the transaction (unchanged). **NotSent** returns the attempt to
   `created`, with the permit consumed. A new resolution is required.

### 6.4 DB enforcement of R1/R5 in every session (CT-R1)

Two new triggers, **not** edits of `payment_attempts_guard()`:
- **`payment_attempts_closed_tenant_dispatch_gate`** (BEFORE INSERT OR UPDATE on
  `payment_attempts`, `operation = 'payout'`): when the tenant is `closed` (read in-tx),
  - an UPDATE `created → submitting` requires a permit for this attempt with
    `consumed_txid = txid_current()`;
  - an INSERT of a payout attempt (T1p) is refused.
- **`withdrawal_requests_closed_tenant_submit_gate`** (BEFORE UPDATE on `withdrawal_requests`):
  when the tenant is `closed`, `→ submitted` is refused. CT-PRE dispatch is not expressible (§4.1).

Together these close **H-SEC-5 for payouts of closed tenants**: no code path, including HTTP
`ClaimForDispatch` driven by uncontrolled closed-tenant staff, can dispatch without a consumed
permit. **H-SEC-5 for deposits** and for suspended tenants was open here (PAY-H-FOLLOWUPS-1 item 4); *update 2026-10-08: closed for HTTP initiation by ADR 0095 section 43 (`tenant.RequireActiveForPaymentInitiation`, tenant and brand, in the creating transaction).*
**The tenant-closure flow must not launch until H-SEC-5 is closed for `closed` tenants.** This ADR
closes it for payouts, and the deposit half remains a prerequisite.

## 7. Deterministic ledger operations

**Lock order** (ADR 0082 A8 amendment, **LF CT-3**): `closed_tenant_hold_resolutions` takes its L1
position **after `payment_manual_resolutions` and before `ledger_adjustment_requests`**. The
permit row is locked after the withdrawal and the attempt and before L3/L4, in both the executor and
the sweeper. Postings go only through `ledger.Post` (L3/L4 via `LockProjectionsForPosting`). There
is never a balance UPDATE.

### 7.1 Release × CT-NEVER-SENT (governed M3; **LF Q-CT-LF-2 CONFIRMED**)

1. Lock the withdrawal (`LockSubmittedForResolution`), then the attempt, then the resolution.
2. Run approvals and the DB recount (§5.3).
3. Re-check: the tenant is `closed`; the rule is still permitted (its id is recorded); the class,
   withdrawal state and attempt state equal the pinned values; the attempt is
   `created AND NOT ever_possibly_sent`.
4. Move the resolution to `executing`.
5. `payments.RejectCreated(tx, attempt, EvidenceOperator, "closed_tenant_release")`. 0107 already
   admits payout `created → rejected` with any evidence (`0107:173` plus the M3 guard), so **no
   guard edit is needed**. ADR 0101 §24.7's column-discipline trigger gains the one named case
   `terminal_reason = 'closed_tenant_release'` for `created → rejected`.
6. `withdrawal.Fail(tx, wr, "closed_tenant_release")` posts `withdrawal_failed` with key
   `wr.id:failed` (shared by construction: `Fail` is the only writer, and the withdrawal reaches
   `failed` once by CAS). The posting moves hold → `player_cash`, with `ReversesTransactionID` =
   the hold transaction.
7. Mark the resolution `executed` and link `ledger_transaction_id`. Attribution is through that
   link, the audit and the attempt's `terminal_reason`.

### 7.2 Release × CT-PRE (**LF Q-CT-LF-1 ruling**)

New function `withdrawal.ReleaseForGovernedResolution(ctx, tx, requestID, resolutionID)`:
- It takes the **L1 lock** on the withdrawal.
- It accepts only `requested`, `pending_review` or `approved`.
- **It asserts that no `payment_attempts` row exists for the withdrawal** (LF CT-5).
- It posts `withdrawal_rejected` (hold → `player_cash`, same wallet, `wr.amount`, `CorrelationID
  = wr.id`, `ReversesTransactionID` = the hold transaction) with the **distinct key
  `<wr.id>:closed_tenant_released`**. A shared `:rejected` key is refused because a governed
  release could otherwise adopt a tenant `Reject` posting through `ledger.Post`'s already-posted
  replay and be recorded as executed without moving money. The other keys stay as they are:
  `:rejected`, `:cancelled`, `:kyc_denied` and `:failed` (`withdrawal.go:951`, `:1669`, `:1118`,
  `:1584`).
- It moves the state to `rejected` with a **conditional UPDATE `WHERE state = <the locked
  state>`** that sets `release_ledger_transaction_id` in the same statement (the N3 pattern).
- It sets **no** `reason_code` on the ledger row; the reason goes in the audit.

**DB gating in ALL sessions (security CT-R3):**
- a BEFORE UPDATE trigger on `withdrawal_requests` refuses a `requested`/`pending_review`/`approved`
  → `rejected` move whose `release_ledger_transaction_id` points at a `:closed_tenant_released`
  posting, unless an **executing** CT-PRE release resolution for this withdrawal has
  `executed_txid = txid_current()`;
- an **all-sessions** BEFORE INSERT trigger on `ledger_transactions` refuses any `idempotency_key`
  ending in `:closed_tenant_released` without that executing resolution (the
  `ledger_transactions_reserved_prefix_guard` pattern).

### 7.3 Acting fences (security CT-R4; LF CT-2)

- `ledger_governed_fence_allows` gains:
  - **(d)** `withdrawal_failed`, key `wr.id:failed`, `correlation_id = wr.id`, with an executing
    CT-NEVER-SENT release in this txid;
  - **(e)** `withdrawal_rejected`, key `<wr.id>:closed_tenant_released`, `correlation_id = wr.id`,
    with an executing CT-PRE release in this txid.
- `ledger_entries_governed_fence()` (as rewritten by ADR 0101 §24.1) gains **per-entry shapes**:
  - (d): debit `player_withdrawal_hold` (wallet = `wr.wallet_id`) and credit `player_cash` (the
    same wallet);
  - (e): the same shape;
  - both: asset `wr.asset_code`, amount `wr.amount` = the resolution's amount, at most two entries,
    one per direction.
- The acting `ledger_accounts` INSERT (ADR 0101 §24.2) gains `player_withdrawal_hold` for the
  executing closed-tenant resolution's wallet and asset. `player_cash` is already allowed.
- Tests and mutants follow the ADR 0101 T-2 pattern.

### 7.4 Permit and retain

Neither posts. The permit outcome inserts a permit and voids any earlier one. Retain voids any
unconsumed permit.

### 7.5 Exactly-once and no-negative (**LF CT-1: corrected**)

Revision 3's claim that "`ledger.Post`'s projection check refuses otherwise" was **wrong**: there is
no generic non-negative check. Exactly-once comes from:
- the withdrawal L1 lock;
- the state-conditional UPDATE (or the `Fail` CAS);
- the deterministic, distinct ledger keys;
- the partial UNIQUE index on executed resolutions;
- the deferred check (no `executing` at commit; an executed release links a same-tenant posting
  with the expected key and `correlation_id`, and the withdrawal is `failed` or `rejected`).

The hold account nets to zero because exactly one release can post (test: a second release is
refused, and the hold for that withdrawal sums to zero).

## 8. Interactions

| Topic | Interaction |
|---|---|
| H resolution-only | unchanged without a permit; the §6.3 hook only; H regression suite green |
| T12 | never via this path |
| `ever_possibly_sent` | release requires NOT, at three layers (CAS, CHECK, guard) |
| Races | release vs T2 and the permit, and CT-PRE release vs HTTP `Approve`/`Reject`/`Cancel`/`ClaimForDispatch`, are all serialized on the withdrawal L1 lock; state CAS; exactly one wins |
| K3 M2 | CT-INFLIGHT and CT-M2 go to M2. A cross-check refuses a closed-tenant resolution while an M2 resolution is pending on the same attempt, and the reverse |
| Reconciliation | a released never-sent attempt is `rejected`. A later success line → `pay_status_mismatch`; a later callback → T15 |
| Reopening | execution requires `closed`; a reopen voids nothing automatically, but every pending resolution then refuses at execution (HD-CTF-9) |

## 9. Audit and tenant isolation

- Every submission, approval, rejection, cancellation, expiry, refusal, execution, rule change,
  permit creation, consumption and void writes `audit_log` in the same transaction. Each record
  carries actor and scope, the acting tenant, Persons, class, outcome, rule id, jurisdiction and
  licence, reason, evidence hash, withdrawal and attempt before/after, the ledger transaction or
  permit, IP, UA and request id.
- Acting rows follow ADR 0099 §10.6. The ADR 0104 projection applies.
- FORCE RLS on every new table; no `FOR ALL`; no `SECURITY DEFINER`; DELETE and TRUNCATE denied.
- **No policy on `tenants`, `licences` or `jurisdictions`**, so the 0077 whitelist stays at 11
  tuples.

## 10. Routes and permissions (security CT-R7 applying R-9)

- The static `closed_tenant_hold_resolution:request` / `:approve` / `:read` permissions go to
  `platform_admin` and platform `finance` only, never `tenant_admin`.
- The tenant comes from the path, never the body.
- Errors use a closed token set.
- Every refusal writes a denial audit.
- The executed audit links the ledger transaction and the permit.

## 11. Schema (migration: next free number at merge, after K3)

New:
- `closed_tenant_resolution_outcomes` (R, seeded mechanics rows);
- `closed_tenant_resolution_reason_codes` (R);
- `closed_tenant_resolution_settings` (**no row seeded by this ADR**);
- `closed_tenant_resolution_rules`, `closed_tenant_resolution_rule_changes`,
  `…_rule_change_approvals`;
- `closed_tenant_hold_resolutions`, `closed_tenant_hold_resolution_approvals`;
- `closed_tenant_dispatch_permits`;
- `closed_tenant_outcome_permitted()`;
- the guard and state-machine triggers, the beneficiary guard, the deferred check, and the
  `payment_attempts_closed_tenant_dispatch_gate` and `withdrawal_requests_closed_tenant_submit_gate`
  triggers;
- the CT-PRE edge trigger and the `:closed_tenant_released` ledger guard;
- the classification row, two capability rows and one governance permission;
- the widened `0112:221` / `0113:85` CHECKs.

Replaced:
- `financial_policy_required_approvals` (non-active special case);
- `ledger_governed_fence_allows` (+(d)(e));
- `ledger_entries_governed_fence` (+(d)(e) shapes);
- the acting `ledger_accounts` INSERT;
- ADR 0101's column-discipline trigger (+ the one named case).

Grants: an `init-app-role.sql` append using the K2 loop.

Down:
- refuses while any resolution, permit, rule or change row exists;
- restores the prior (0115-era) bodies byte-for-byte;
- is verified by the whole-schema snapshot;
- passes on an empty scratch DB (the 0075/0077 chains).

## 12. Separate human decisions: ALL OPEN (nothing invented)

| ID | Question |
|---|---|
| **HD-CTF-1** | (a) Which outcomes (§4.1) are permitted, per jurisdiction (Anjouan first) and per own-licence operator, for each item class: the content of the first rule rows. (b) Is the platform row a floor that a jurisdiction cannot loosen, or does the most specific level win? |
| HD-CTF-2 | May the closed tenant's own staff see, request or approve anything? (Default: none.) Is grant-chain separation (§5.2) required? |
| HD-CTF-3 | After a release to `player_cash`, how does the player obtain the money from a closed tenant (access, withdrawal path, notification)? |
| HD-CTF-4 | What happens to closed-tenant balances not under a withdrawal? |
| HD-CTF-5 | Is an outcome outside §4.1 legally required (a third-party or unclaimed-funds transfer, escrow, a successor operator)? |
| HD-CTF-6 | May a tenant be set to `closed` while hold-bearing withdrawals exist? (Architect recommends an interim rule refusing it.) |
| HD-CTF-7 | The reason-code vocabulary and evidence obligations (§5.4) |
| HD-CTF-8 | Are alerts, notifications to players or regulators, or deadlines required? (No timelines, recipients or retention are set.) |
| HD-CTF-9 | May a `closed` tenant ever be reopened? |
| **HD-CTF-10** (added 2026-10-05, PRH-2 R3 security F-1; **OPEN HUMAN DECISION**) | **Credential treatment and end condition of reconciliation observation of a non-active tenant.** Observing a closed tenant fetches its statement with that tenant's still-ACTIVE outbound PSP credential (`providercred.OutboundResolver` checks the handle's status and expiry, never `tenants.status`). Either (a) the credential stays active: the platform keeps authenticating to an offboarded operator's merchant account every sweep with no end (a legal, contract and licence question, especially for an `own_licence` operator); or (b) closure revokes it: every closed tenant then fails the fetch phase on every sweep, writing a P1 audit row and `reconciliation.run_failed` alert forever, which trains operators to ignore the control. Needed: the credential treatment on closure (read/reporting-scoped credential where the PSP supports one; retention vs revocation), and when observation ENDS (for example: all hold-bearing work resolved and N clean runs). Any tenant-closure flow must depend on this answer. See ADR 0095 §40.4. |

Note (security F-4, tracks Q-CT-SEC-1 / HD-CTF-2): observation of a closed tenant writes new post-closure `audit_log` rows with that `tenant_id` (`reconciliation.sweep_run`, `reconciliation.sweep_run_failed`; benign content: counts, provider id, import id, status, the observation flag). `internal/auth` does not refuse closed-tenant staff sessions today, so staff of the closed tenant with audit-read could see them. HD-CTF-2's answer must cover these rows.

Thresholds and approval counts are configuration under HD-PRH2-3 (none seeded). The permit lifetime
is a technical security default (§6.2), not a legal value.

## 13. Tests (DoD for the future workstream)

Every test runs as the runtime role (`NOT rolsuper AND NOT rolbypassrls`) in the real session
shapes (§3), and asserts SUM(D) = SUM(C) and projection = recomputed.

**R**
- A governed M3 posts the exact key and shape.
- A CT-PRE release from each of the three states posts `:closed_tenant_released`.
- A second release is refused, and the hold nets to zero.
- Retain posts nothing and voids the permit.
- A permit lets exactly one T2 through.

**Config**
- Nothing seeded ⇒ everything refused.
- Each level of precedence.
- An unresolvable jurisdiction.
- A rule change between submission and execution.
- No backdated `effective_from`.

**AZ**
- S-12 and LF-11.
- Self-approval.
- No grant, or a revoked or expired one.
- S-4: a suspended or demoted actor with a valid JWT.
- A tenant-scope actor refused.
- A rule author or approver refused (both at submission and at execution).
- A policy author refused.
- The sock-puppet case.
- A principal with only `payment_force_resolve:*` or `ledger_adjustment:*` refused (capability
  specificity).

**CT-R1**
- HTTP `ClaimForDispatch` / `→ submitted` for a closed tenant is refused in a tenant session.
- `created → submitting` without a consumed permit is refused in every session.
- A payout attempt INSERT is refused.

**CT-R2**
- An expired permit is not consumed.
- A rule revoked after the permit was created means no consumption.
- Kill-switch block: the permit stays usable.
- KYC deny: the permit is voided.
- NotSent: the permit is consumed and a new resolution is needed.

**CT-R3 / CT-R4**
- A CT-PRE edge, or a `:closed_tenant_released` key, without an executing resolution is refused in
  tenant, acting and system sessions.
- Fence leg shapes: wrong wallet, wrong amount, a third entry, an entry after `executed`.
- The first `player_withdrawal_hold` account creation under acting.

**CON** (`-race -count=50`)
- Release vs T2 with a permit.
- Release vs HTTP dispatch, approve or reject.
- Two final approvals.
- Two sweepers consuming one permit (exactly one does).
- Release vs a pending K3 M2.

**ADV**
- Release of an `ever_possibly_sent` attempt.
- Release for an active or suspended tenant.
- A permit for CT-PRE.

**Other classes**
- FL/RB: fault injection, with absence verified from a second session.
- RLS: the probe list.
- AU: audit and linkage.
- MIG: whole-schema up/down/up; the 0077 whitelist unchanged; the 0075/0077 chain.
- **H regression:** the H suite is green; the H-CR-4 mutant is still killed with the hook present.
- **CT-4 pin:** each `posting_shape` maps one-to-one to a code path.

**Mutants**
- Drop `NOT ever_possibly_sent`.
- Drop the tenant re-check.
- Drop rule re-evaluation, at execution or at consumption.
- Permit reuse.
- Drop permit expiry.
- Drop the dispatch-gate trigger.
- Drop the CT-PRE edge trigger.
- A shared `:rejected` key.
- Drop the fence leg shapes.
- A tenant-scope actor admitted.
- Drop author/executor separation.

## 14. Threats and residuals

| ID | Threat / residual | Control |
|---|---|---|
| TM-CT-1 | A platform insider releases or dispatches funds to themselves | S-12, LF-11, platform-co-approved grants, author/executor separation, audit, tenant projection, optional grant-chain separation |
| TM-CT-2 | Rule tampering | four-eyes platform-only rule changes; no backdating; rule id recorded at submission, execution and consumption |
| TM-CT-3 | Permit misuse | bound to one attempt; one-shot CAS; expiry; re-evaluation at consumption; DB dispatch gate |
| TM-CT-4 | Double payout | `NOT ever_possibly_sent` at three layers; CT-INFLIGHT excluded |
| TM-CT-5 | A closed tenant's staff dispatch through HTTP (H-SEC-5) | the §6.4 DB gates (payouts) |
| R-CT-1..R-CT-4 | Funds sit in a closed tenant's `player_cash` (HD-CTF-3); unrequested balances (HD-CTF-4); status read without a lock (money-safe; recorded); CT-BLOCKED holds | as noted |
| R-CT-5 | Nothing alerts on "a closed tenant holds funds" | ALERT-DELIVERY-1 OPEN; HD-CTF-8 |
| R-CT-6 | H-SEC-5 for deposits and suspended tenants | PAY-H-FOLLOWUPS-1 item 4; a prerequisite for any tenant-closure flow. **Closed for HTTP initiation 2026-10-08 (ADR 0095 section 43).** |

**Launch status:** a launch blocker for any tenant-closure flow (ADR 0105 §1). It does not block
operating active tenants.

## 15. Review conditions → section

| Condition | Section |
|---|---|
| CT-R1 (permit consumption session; DB-enforced R1/R5; H-SEC-5 for closed tenants) | §6.1, §6.3, §6.4, §3 |
| CT-R2 (expiry; re-evaluation at consumption; kill-switch/KYC path) | §6.2, §6.3 |
| CT-R3 (new withdrawal edges DB-gated in all sessions) | §7.2 |
| CT-R4 (fence (d)/(e) shapes; `ledger_accounts`) | §7.3 |
| CT-R5 (author vs executor separation as a requirement; no backdating) | §5.2, §4.3 |
| CT-R6 (explicit session shapes) | §3 |
| CT-R7 (R-3 / R-6 / R-9 applied) | §5.1, §5.3, §10 |
| LF CT-1 (exactly-once claim corrected) | §7.5 |
| LF CT-2 (fence shapes, `ledger_accounts`) | §7.3 |
| LF CT-3 (ADR 0082 A8 lock positions) | §7 |
| LF CT-4 (`posting_shape` closed set and pin) | §4.1, §13 |
| LF CT-5 (CT-PRE asserts no attempt) | §7.2 |
| LF CT-6 (a permit-dispatched payout that becomes ambiguous goes to M2) | §2 |
| Q-CT-LF-1 (key `<wr.id>:closed_tenant_released`; function requirements) | §7.2 |
| Q-CT-LF-2 (governed M3 = `withdrawal_failed`, `wr.id:failed`; no guard edit) | §7.1 |
| Q-CT-SEC-1 (platform-only; no T read) | §3, §5.2 |
| Q-CT-SEC-2 (new capability pair, classification, CHECK widening, `valid_until`, no `tenant_admin`) | §5.2 |
| Q-CT-SEC-3 (hook position; conditional on CT-R1/CT-R2) | §6.3 |
| HD-CTF-1..9 OPEN | §12 |
