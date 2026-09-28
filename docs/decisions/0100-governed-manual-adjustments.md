# ADR 0100 — Governed manual adjustments and financial approval policies (PRH-2 K2)

- **Status:** ACCEPTED (2026-09-28). `security` CONFIRMED WITH CONDITIONS and `ledger-finance`
  CONFIRMED WITH CONDITIONS, both on revision 2
  (`docs/plans/prh2-hardening-round/reviews/adr-0099-0101-{security,ledger-finance}-confirmation.md`).
  The orchestrator wrote their conditions (LF K2-a, K2-b and K3-a; security C-2 and C-4) into this text. NOT IMPLEMENTED.
- **Revision history:** PROPOSED — **revision 2** (`architect`, 2026-09-28). Revision 1
  (`d83a71c`) was reviewed ACCEPT WITH CONDITIONS by `product-owner-proxy`, `security` and
  `ledger-finance`. This revision applies every condition (§17). **`security` and `ledger-finance`
  confirm it before any K2 code.** K2 also waits for K1 to merge.
- **Decision type:** cross-domain architecture and financial control:
  - `internal/adjustment` (new);
  - `internal/ledger`, called but **not edited**;
  - `internal/reconciliation` (a new detective check);
  - `auth`, `httpserver`;
  - migration 0112.
- **Owner:** `architect` for the cross-domain shape. **`ledger-finance` owns every financial
  invariant here, and its rulings in `reviews/adr-0099-0101-ledger-finance.md` are binding.**
  **Reviewers:** `security`, `ledger-finance`, `qa`, `code-reviewer`.
- **Registry:**
  - LEDGER-MANUAL-ADJ-4EYES-1 (this ADR);
  - HD-PRH2-8 (§3.4);
  - LEDGER-MANUAL-ADJ-LINK-1 (= MANUAL-ADJ-LINK-1; the preventive follow-up, launch-blocking for
    the first real-money tenant).

  Workstream K2; migration 0112.
- **Binding inputs:**
  - ADR 0098 §2, §4 and §5 (HD-PRH2-1, -3, -6, -7).
  - Plan §11.
  - S-2, S-12 and S-13.
  - LF-9..LF-14.
  - `reviews/adr-0099-0101-security.md` C-100-1..6 and rulings 4 and 5.
  - `reviews/adr-0099-0101-ledger-finance.md` F4–F8, F15 and rulings 1–4, 6 and 7.
  - `reviews/adr-0099-0101-product-owner-proxy.md`.
  - ADR 0099 revision 2.
- **No threshold value is set anywhere in this ADR or in 0112** (HD-PRH2-3). No legal claim is made.

| Rev | Base | Change |
|---|---|---|
| 1 | `d83a71c` | Initial draft |
| 2 | `6864efa` | LF rulings 1–4, 6 and 7 written in; the preventive `open_payment_exposure` refusal; exact catalogues; the suspended-asset and non-active-tenant rules; S-2(iii) extended to approvers; K2-1 resolved (§6.7); the note bound; the detective kind `ledger_unlinked_manual_adjustment`; the fixture count corrected to 19; ADR 0082 A8 corrected; full 0112 content; review disposition |

---

## 1. Context

**What exists today:**
- `TxManualAdjustment` and `AccountManualAdjustment` (`ledger.go:76,115`) are used only by tests.
- `ledger.Post` requires a reason code for `manual_adjustment` (`lockorder.go:355-357`; 0051:72).
  It constrains neither the accounts nor the approval, and has no generic sufficiency check.
- The sanctioned balance read is `LockProjectionsForPosting` (`lockorder.go:94-124`).

**Constraints that shape this design:**
- **HR-9 is removed** (`ledger.go:26-29`). A BONUS_SET posting auto-generates mirror legs, and HR-17
  forbids hand-built ones. So the posting shape must be closed.
- The 0107 INV-DEP-1 backstop covers only `transaction_type = 'deposit'`. A manual credit is
  therefore able to hand-pay a captured-but-unposted deposit unless it is prevented (LF F4).

## 2. The financial-control classification (HD-PRH2-1)

| `operation_kind` | Class | `governed_since` |
|---|---|---|
| `ledger_adjustment` | `mandatory_four_eyes` | set by 0112 to the migration time (the cutover for §12's detector) |
| `payment_force_resolve` | `mandatory_four_eyes` | set by 0112 |

- This is migration-written reference data. The app role has `SELECT` only.
- A CHECK pins both kinds to `mandatory_four_eyes`. Removing one needs a human decision, an ADR and
  a migration. Policy data can never do it.
- **There is no `never` mode**, and the policy model has no mode column.
- An operation outside the class is represented by an explicit `outside_mandatory_class` row, added
  by migration and ADR. None exists.
- No CLAUDE.md amendment.

## 3. Approval policies (HD-PRH2-7, S-2, HD-PRH2-3)

### 3.1 Levels and authorship

| Level | Scope key | Requester | Approver |
|---|---|---|---|
| `platform`, `jurisdiction`, `profile` (and the tenant → profile assignment) | none / `jurisdiction_id` / `profile_code` | `platform_admin`, `financial_policy:author` | a **different** `platform_admin` with `financial_policy:author`, different Person |
| `tenant`, `brand` | `tenant_id` (+ `brand_id`) | `tenant_admin` (`financial_policy:tighten`, **tightening only**) or `platform_admin` | a different principal and Person: tenant (tightening only) or platform. **A non-tightening tenant or brand row needs both requester and approver to be platform principals.** |

### 3.2 Evaluation (S-2 ii/iv; LF ruling 7)

- **In-force row per key.** For each `(level, scope key, operation_kind, asset_code)` the in-force
  row is the one with the latest `effective_from <= as_of`. Rows are append-only, so any past
  evaluation can be reproduced.
- **Applicable rows.** Rows at every level that match: the operation; the tenant; the tenant's
  jurisdiction (`tenants.licence_id → licences.jurisdiction_id`); the tenant's profile; the brand;
  and `asset_code` equal to the request's asset or NULL.
- **Row requirement.**
  - `base_required_approvals` if the row has no threshold, or if the amount is `<=` the threshold.
  - Otherwise `required_approvals_above_threshold`.
  - An operation without an amount (M1) uses the base only.
- **`financial_policy_required_approvals(operation_kind, tenant_id, brand_id, asset_code, amount,
  as_of)`** returns `(enabled boolean, required int, contributing_policy_ids uuid[],
  tenant_status text)`, where for the mandatory class:
  - **`required = GREATEST(1, MAX(row requirement over applicable rows))`** (LF ruling 7);
  - **`enabled`** only if an in-force `platform`-level row exists for the operation (asset NULL or
    equal). **No platform baseline means disabled** (fail closed). No row is seeded.
  - If a jurisdiction row exists for the operation and the tenant's jurisdiction is unresolvable,
    the result is `enabled = false`.
  - **K2-1 rule (§6.7):** for `operation_kind = 'payment_force_resolve'`, if the tenant's `status`
    is not `active`, tenant- and brand-level rows are **not** applicable. Platform, jurisdiction and
    profile rows still are.
- The executor and the triggers call this one function. There is no second implementation in Go.
- MAX evaluation makes "tighten-only" structural. The insert trigger enforces S-2 (iv) as well
  (§3.3).
- A trigger on `financial_approval_policies` refuses `base_required_approvals < 1` for a
  mandatory-class operation at **every** level. The CHECK `required_approvals_above_threshold >=
  base_required_approvals` also stays.

### 3.3 Policy changes: four-eyes, and never usable by their authors (S-2 iii; C-100-1)

- **Every change is a request plus an approval.** Tables: `financial_approval_policy_changes` and
  `financial_approval_policy_change_approvals`.
  - The request pins `content_hash`, a DB function over the proposed row.
  - The approval re-checks the hash and inserts the policy row in the same transaction
    (`decided_txid = txid_current()`).
  - Requester ≠ approver, with distinct non-NULL Persons.
- **Tightening, defined** against the in-force row of the same key. With no predecessor, any row is
  a tightening. Otherwise all of the following must hold:
  - `new.base >= old.base`;
  - if `old.threshold` is not NULL: `new.threshold` is not NULL, `new.threshold <= old.threshold`,
    and `new.above >= old.above`.

  A tenant or brand row that is not a tightening is refused unless both requester and approver are
  platform principals (`MA010`).
- **Authors may not use their own policy (C-100-1; security ruling 4).** The Persons who requested
  or approved a policy row are stored on it (`author_person_id`, `approver_person_id`). If the
  **initiator**, or the Person of **any approval being counted**, equals either Person on a row in
  `contributing_policy_ids` (at submission or at execution), then:
  - an initiation is refused (`MA011`);
  - such an approval is refused at insert and not counted at execution.
- `legal_review_reference` is informational text only. It is never evaluated.

### 3.4 HUMAN DECISION REQUIRED: HD-PRH2-8, below-threshold semantics for the mandatory class

The question is whether a mandatory-class operation at or below a threshold may execute with **no
independent approver**.

| Option | Consequence |
|---|---|
| **(a)** The threshold gates four-eyes (`base` may be 0) | Single-person small corrections. There is a structuring risk from repeated small amounts. |
| **(b)** Always at least one independent approver; thresholds only add approvers | Two distinct Persons on every correction. |

**Interim (enforced, and confirmed by ledger-finance ruling 7): (b).**
- The trigger refuses `base < 1` at every level, and the evaluator returns `GREATEST(1, …)`.
- **Moving to (a) is not "a one-line change"** (LF F7). It needs all three of:
  1. the human's answer;
  2. `ledger-finance` sign-off;
  3. an **implemented cumulative-window (structuring) control, per initiator and per player,
     before the trigger is relaxed.**

  The window values would themselves be configuration, under legal review.

`product-owner-proxy` asks that this reach the human promptly, so the interim does not become
permanent by default.

### 3.5 Per-asset thresholds (LF-12, HD-PRH2-3)

- A threshold requires `asset_code` (CHECK). The threshold is `NUMERIC(38,0) >= 0`.
- No migration seeds a policy row. Fixtures use synthetic, test-only values.
- Real values: LEGAL / COMPLIANCE REVIEW REQUIRED.

### 3.6 Pinning

- At submission: `required_at_submission` and `contributing_policy_ids` are stored.
- At execution: `required = MAX(required_at_submission, the evaluation at execution)`.

## 4. The posting-shape catalogue (LF-9, S-13)

| Direction | Debit | Credit |
|---|---|---|
| `credit_player` | the tenant's `manual_adjustment` (house, same asset) | the player's `player_cash` (the wallet's) |
| `debit_player` | the player's `player_cash` | the tenant's `manual_adjustment` |

- Exactly two entries, one amount, one asset, one tenant, with `transaction_type =
  'manual_adjustment'`.
- The request's `account_type` column has CHECK `= 'player_cash'`.
- Every other account type is unrepresentable: BONUS_SET (so no mirror legs are generated), hold,
  locked, PSP, provider, promo, `bonus_expense`, jackpot and house gaming.
- Bonus corrections go through the bonus engine. Cross-asset movement is a `ConversionOperation`.

## 5. The request

### 5.1 Payload (LF-10, F6): immutable after submission

- **Payload columns:** `tenant_id`, `wallet_id`, `player_account_id`, `brand_id`, `account_type`,
  `asset_code`, `direction`, `amount`, `reason_code`, `causation_transaction_id`,
  `evidence_ref_hash`, `note_hash`.
  - `player_account_id` and `brand_id` are derived by trigger from the wallet.
- **`payload_hash`** is computed by a DB function over a canonical encoding of **all** payload
  columns, including `evidence_ref_hash` (F6). The Go code never supplies it.
- **The note (C-100-5).** Free text, `octet_length` 1–1000 and no C0/C1 controls other than `\n`,
  validated in Go and in the trigger. It is stored in the audit metadata. Only `note_hash` is on
  the request.
- **Evidence.** `evidence_ref_hash` is `TEXT NULL`: a hex SHA-256 of an external evidence
  reference, `CHECK (evidence_ref_hash ~ '^[0-9a-f]{64}$')`. It is required by the reason codes in
  §5.4. No PII.

### 5.2 Checks at submission, repeated at execution

| Check | Rule |
|---|---|
| Amount | `NUMERIC(38,0)`, CHECK `amount > 0 AND amount <= 9223372036854775807` (LF-12) |
| Asset (LF ruling 3) | `asset_code` equals the wallet's asset and exists in `assets`. **Suspended asset** (`assets.active = false OR assets.platform_authorized = false`, **or** the 0045 tenant-authorization layer: the tenant's `asset_authorizations` row for the asset is absent or not in force. LF confirmation K2-b; K2 pins the exact 0045 column predicate): `goodwill_credit` is refused; `compensating_entry`, `operational_error_correction` and `external_instruction` are allowed. The rule is re-evaluated at execution. |
| Reason code | in the catalogue (§5.4); direction allowed; causation and evidence rules satisfied |
| **Open payment exposure (LF F4, ruling 2; PREVENTIVE)** | Every `credit_player` request, whatever its reason code, is refused with `MA020 open_payment_exposure` while `player_open_payment_exposure(tenant_id, player_account_id)` is true. It is checked at submission **and** at execution. **No override.** Debits are unaffected. |
| Non-active tenant (C-100-3; security ruling 5) | if `tenants.status <> 'active'` (read in-tx), `goodwill_credit` is refused. The status is recorded on the request. |
| Policy | `enabled` (§3.2) |
| Initiator | an in-force `ledger_adjustment:initiate` grant, S-2(iii) (§3.3) and S-12 (§6.3) |

**`player_open_payment_exposure(p_tenant uuid, p_player uuid) RETURNS boolean`** (`STABLE`; one
function used by the trigger and the executor; no statement source needed):

```sql
SELECT EXISTS (
  SELECT 1
    FROM payment_attempts a
    JOIN deposit_intents i ON i.id = a.deposit_intent_id AND i.tenant_id = a.tenant_id
   WHERE a.tenant_id = p_tenant AND i.player_account_id = p_player
     AND a.operation = 'deposit' AND a.state = 'disputed'
     AND a.terminal_reason = 'multiple_success_for_intent'
     AND NOT EXISTS (
       SELECT 1 FROM ledger_transactions t
        WHERE t.tenant_id = a.tenant_id
          AND t.transaction_type = 'tombstone'
          AND t.provider_id = a.provider_id
          AND t.provider_tx_id = a.provider_reference))
```

This is the ledger-side form of the reconciliation `capturedUnposted` predicate
(`payment_statement.go:977-979`). **Key column pinned by ledger-finance (K2-a):**
- The tombstone writer keys on the **original** reference: `ProviderID = providerID`, `ProviderTxID = originalRef` (`orchestrator.go:1526-1528`).
- The former `deposit_reversal` arm is removed. A reversal is keyed on its own reference and needs a posted original, which a `multiple_success_for_intent` attempt never has.
- A NULL `provider_reference` keeps the exposure true (fail-closed).
- A refund visible only on a statement does not clear it (fail-closed, accepted).
- B-23 includes a case where a refund produces a tombstone and the exposure clears. An acting session
reads `payment_attempts` and `deposit_intents` through ADR 0099 §6.5.

### 5.3 Keys (LF-14)

| Key | Value |
|---|---|
| `idempotency_key` | `'manual_adjustment:' || request_id` (UNIQUE `(tenant_id, idempotency_key)`, 0021:43) |
| `correlation_id` | the request id |
| `causation_id` | `causation_transaction_id` |
| `reason_code` | copied to `ledger_transactions.reason_code` |
| `ProviderID` / `ProviderTxID` | nil |

### 5.4 The reason-code catalogue (LF ruling 1: binding)

Table `ledger_adjustment_reason_codes`, written by 0112 only:

| `reason_code` | Directions | Causation rule | `evidence_ref_hash` | Suspended asset | Non-active tenant |
|---|---|---|---|---|---|
| `operational_error_correction` | both | optional. If present: a same-tenant transaction with a `player_cash` leg on **this wallet** | optional | allowed | allowed |
| `compensating_entry` | both | **required**. It must be a same-tenant transaction, **not** of type `deposit`, `deposit_reversal` or `tombstone`, with a `player_cash` leg on **this wallet in the same asset**. **Cumulative cap:** the sum of executed `compensating_entry` amounts for this `(causation, direction)`, plus this amount, must be `<=` that leg's amount. The cap is checked at execution under the L2 lock of the causation row (§8). | **required** | allowed | allowed |
| `goodwill_credit` | credit only (withdrawable `player_cash`; wagered goodwill goes through the bonus engine) | forbidden | optional | **refused** | **refused** |
| `external_instruction` | both | optional (same rule as `operational_error_correction`) | **required, pinned** | allowed | allowed |

- **There is no deposit-allocation code** (LF-2).
- The request has no column that references an attempt or intent.
- Causation to a `deposit` transaction is refused for every code: `compensating_entry` via the rule
  above; the others via the "player_cash leg on this wallet" rule plus an explicit type check.

**RESOLVED (ledger-finance confirmation K3-a; security C-4): the proposal below is ADOPTED.** The
record of the conflict follows.
- Ruling 5(c) clears `pay_declared_paid_unconfirmed` with "a `compensating_entry` with causation =
  that Step B".
- But Step B (`withdrawal_completed`: hold → `psp_clearing`, `withdrawal.go:1446-1449`) has **no
  `player_cash` leg**, so ruling 1 refuses that causation.
- **Proposed resolution:** for `compensating_entry` with `direction = 'credit_player'`, also accept
  a causation of type `withdrawal_completed` whose `provider_tx_id` carries the reserved M2 prefix
  (ADR 0101 §5.4) and which has a `player_withdrawal_hold` leg on this wallet in the same asset. The
  same cumulative cap applies against that leg.
- **Adopted with security C-4 (a)–(e) and the ledger notes.** The causation must:
  - satisfy `left(provider_tx_id, 27) = payment_reserved_ref_prefix() AND transaction_type = 'withdrawal_completed'`;
  - be the `ledger_transaction_id` of an **executed `m2_declare_paid`** resolution;
  - have its `player_withdrawal_hold` leg on **this wallet** in this asset.

  Further constraints:
  - `direction = 'credit_player'` only; a debit with this causation is refused.
  - The cumulative cap is the hold-leg amount, taken under the L2 lock.
  - `evidence_ref_hash` is required.
  - The Persons counted on the M2 resolution (requester or approvers) may not initiate or count as approver. This is enforced by trigger and at execution.
  - No other causation widens, and INV-ADJ-5 still applies.
- **Why this is tighter than rejecting it:** `operational_error_correction` and `external_instruction` could otherwise restore the player with an uncapped, unlinked credit.
- **Implementation:**
  - 0112 (K2) ships ruling 1 literally, because K2 merges before 0114 exists.
  - K3 (0114) adds this causation arm, with its tests (ADR 0101 §9, §12).
- **The compensation does not clear** `pay_declared_paid_unconfirmed`. See ADR 0101 §9(c).

## 6. Approval and execution

### 6.1 States

`pending → executed | refused_insufficient_funds | refused_at_execution | rejected | cancelled |
expired`, plus the transaction-local state `executing`.

- `refused_at_execution` records any other execution-time re-check failure that should end the
  request rather than roll back: `open_payment_exposure`, an asset that became suspended for
  goodwill, a compensation cap now exceeded.
- One `reject` approval ends the request.
- Cancel is by the initiator only.
- The TTL is a technical default forced by trigger.

### 6.2 Counting (LF-14, S-4, LF F3)

An approval counts at execution only if all of these hold:
- `decision = 'approve'`;
- `payload_hash` equals the request's;
- the approver's staff row and grant, **locked `FOR SHARE`** (ADR 0099 §7.4), show `active`, an
  eligible role, and an in-force, unrevoked `ledger_adjustment:approve` grant at `now()`;
- the approver's Person is non-NULL, distinct from the initiator's and from every other counted
  approver's, and is not the beneficiary;
- S-2(iii) (§3.3) is satisfied.

The initiator is re-checked in the same way.

### 6.3 Independence floors (non-configurable)

- **Distinct Person (LF-11).** There is no `distinct_principal` option. The ADR 0099 §9 caveat
  applies.
- **Beneficiary exclusion (S-12).** The triggers `ledger_adjustment_requests_beneficiary_guard` and
  `ledger_adjustment_approvals_beneficiary_guard` refuse an initiator or approver whose Person equals
  `player_accounts.person_id` (NOT NULL, `0010:15`). An unlinked staff Person is refused. The check
  is repeated at execution.

### 6.4 No negative balance (LF-13)

- For a debit: build the exact `TransactionInput`, call `LockProjectionsForPosting` with **the same
  input**, and read `player_cash`.
- If `balance < amount`: move to `refused_insufficient_funds`, post nothing, and commit.

### 6.5 Execution in the final approval's transaction (LF-13, LF-10; ADR 0082 A8)

1. **L1:** `SELECT … FROM ledger_adjustment_requests WHERE id = $1 FOR UPDATE`. It must be
   `pending` and unexpired.
2. Insert the approval. Triggers apply. This is **no lock class**; it happens while the L1 request
   row is held (LF ruling 6).
3. Evaluate `required` (§3.6) and the candidate set. If fewer than `required` approvals can count,
   commit (stays `pending`).
4. **L1 (continued):** `staff_users` rows of the initiator and candidate approvers, `ORDER BY id FOR
   SHARE`; then their `staff_capability_grants` rows, `ORDER BY id FOR SHARE`. Recount under these
   locks. If the recount falls short, commit (stays `pending`).
5. **L2 (only `compensating_entry`, and the causation of any code that has one):**
   `SELECT … FROM ledger_transactions WHERE id = causation FOR UPDATE`. This serializes the §5.4
   cumulative cap across concurrent requests.
6. Re-run the §5.2 execution checks (exposure, asset, tenant status, cap). On failure: move to
   `refused_at_execution` and commit.
7. Set `state = 'executing'` and `executed_txid = txid_current()`.
8. **L3:** `LockProjectionsForPosting(in)`, then the §6.4 check.
9. **L4:** `ledger.Post(in)`.
10. Set `state = 'executed'` and `ledger_transaction_id`. Audit. Commit.

**The link trigger (LF-10).** On `→ executed`, `ledger_transaction_id` must be:
- a same-tenant `manual_adjustment` row (composite FK `(ledger_transaction_id, tenant_id)` →
  `ledger_transactions_id_tenant_key`, 0021:51), UNIQUE;
- with the §5.3 keys;
- with **exactly the two §4 entries equal to the payload**.

**A DEFERRABLE INITIALLY DEFERRED constraint trigger** refuses to commit any row in `executing`.

**Concurrency.** Final approvals serialize on step 1. The idempotency key is the backstop.

**"Same transaction"** means `executed_txid` / `decided_txid`, never `xmin`.

### 6.6 Platform principals (HD-PRH2-6)

- Only through an ADR 0099 §6 acting session holding the specific grant.
- The plain platform session has no policy on any 0112 request or approval table.
- The acting ledger post passes the ADR 0099 §6.6 fence, branch (a), because step 7 precedes step 9.

### 6.7 K2-1: stranding through tenant tightening (C-100-2), **choice (b)**

The problem: unbounded tenant tightening could make `payment_force_resolve` unsatisfiable. That
would strand in-flight payouts, most acutely for a suspended tenant whose staff cannot act.

**Chosen (b):** for `payment_force_resolve` on a non-active tenant, only platform, jurisdiction and
profile rows are evaluated (§3.2). Tenant status is read in-tx and recorded on the request or
resolution (C-101-4).

**Why (b), not (a) (a technical CHECK bound on approval counts):**
1. **(a) does not solve the problem.** Any bound (say N) can still exceed the number of eligible,
   independent Persons a tenant or the platform actually has, so the payout stays stranded. (b)
   removes the tenant's own rows exactly when the tenant can no longer act, which is the case the
   security addendum §2 ruling ("in-flight money must resolve") cares about.
2. **(a) would invent a number** that constrains legitimate tightening for every active tenant.
   HD-PRH2-3 asks us not to invent values, and HD-PRH2-7 lets tenants tighten.
3. **(b) never goes below a platform-mandated control.** The platform, jurisdiction and profile
   floors, `GREATEST(1, …)` and distinct-Person all still apply, so HD-PRH2-7's "never weaken a
   platform-mandated control" holds. Suspending a tenant is itself a platform-only act
   (`tenants_platform_admin_update`, 0077).
4. **Residual for active tenants:** a tenant can make its own M2 slow. The remedy is on the platform
   side: a non-tightening change to the tenant row with a platform requester and approver (§3.3).
   The runbook states this.

For `ledger_adjustment`, the evaluation is unchanged for non-active tenants, and `goodwill_credit`
is refused (C-100-3).

## 7. Relation to CLAUDE.md

- The rule "Manual balance adjustments require a reason code and four-eyes approval above a
  configurable threshold" is satisfied:
  - the reason code comes from a closed catalogue;
  - under the interim, four-eyes applies at every amount (§3.4);
  - the threshold is configuration.
- Audit: §9.
- No value is invented (HD-PRH2-3).

(Revision 1's §7 held the A8 text. It is now §8, corrected.)

## 8. ADR 0082 Amendment A8 (LF-13, LF ruling 6) — corrected text for the orchestrator

> **Amendment A8 — PRH-2 K2/K3 (ADRs 0100, 0101).**
>
> **Class L1** is extended, in this order:
> - … `withdrawal_requests` … `deposit_intents`, `payment_attempts` (per A7);
> - then **`payment_manual_resolutions`**, then **`ledger_adjustment_requests`** (each
>   `FOR UPDATE`);
> - then **`staff_users`**, then **`staff_capability_grants`**, both `FOR SHARE`, ascending `id`
>   within each table.
>
> All of these come before L2/L3.
>
> **M2 order:** withdrawal → attempt → resolution → staff → grants.
>
> **M1 order:** intent → attempt → resolution → staff → grants.
>
> **Manual adjustment order:**
> - request → staff → grants;
> - then, for a request with a causation transaction, **L2** `ledger_transactions` `FOR UPDATE` on
>   that one causation row (this serializes ADR 0100 §5.4's cumulative cap);
> - then L3 (`LockProjectionsForPosting`) and L4 (`Post`).
>
> **Approval-row inserts** happen while the L1 request or resolution row is held. They belong to no
> lock class; they are not L4.
>
> There is no new class and no new exception.

The L2 step for causation rows is an architect addition to LF's ruling 6. It uses the existing L2
class exactly as §2.1 defines it, and **ledger-finance confirms it** in its revision-2 check.

## 9. Audit

Every submission, approval, rejection, cancellation, expiry, refusal, execution, policy change and
profile assignment writes `audit_log` in the same transaction. Each record carries:
- actor, actor scope, acting tenant;
- ids; asset; direction; amount; reason code;
- before and after state;
- `payload_hash`, `contributing_policy_ids`, tenant status;
- the ledger transaction id;
- the note (bounded);
- IP, user agent, request id.

Acting rows follow ADR 0099 §10.6/§11.

## 10. Migration 0112 (K2) — content

Common rules: FORCE RLS; no `FOR ALL` permissive policy; `BEFORE TRUNCATE` deny; DELETE refused;
no `SECURITY DEFINER`; SQLSTATE class **`MA`**. Families T, P, A and R are as in ADR 0099 §10.

### 10.1 Reference tables (family R)

| Table | Columns / constraints | Rows |
|---|---|---|
| `financial_control_classifications` | `operation_kind TEXT PK`, `class TEXT NOT NULL CHECK (class IN ('mandatory_four_eyes','outside_mandatory_class'))`, `governed_since TIMESTAMPTZ NOT NULL`, `CHECK (operation_kind NOT IN ('ledger_adjustment','payment_force_resolve') OR class = 'mandatory_four_eyes')` | the two §2 rows. Also `ALTER TABLE financial_capability_catalogue ADD FOREIGN KEY (operation_kind) REFERENCES financial_control_classifications`. |
| `ledger_adjustment_reason_codes` | `reason_code TEXT PK`, `allowed_directions TEXT[] NOT NULL`, `causation_rule TEXT NOT NULL CHECK (causation_rule IN ('forbidden','optional_same_wallet','required_compensation'))`, `evidence_required BOOLEAN NOT NULL`, `allowed_in_suspended_asset BOOLEAN NOT NULL`, `allowed_for_non_active_tenant BOOLEAN NOT NULL` | the four §5.4 rows |

### 10.2 `financial_approval_policies` (append-only)

**Columns:**

| Column | Type / constraint |
|---|---|
| `id` | `UUID PK` |
| `operation_kind` | FK to the classifications |
| `level` | `CHECK (level IN ('platform','jurisdiction','profile','tenant','brand'))` |
| `tenant_id` | `UUID NULL` FK |
| `brand_id` | `UUID NULL`, composite FK `(brand_id, tenant_id)` → brands |
| `jurisdiction_id` | `UUID NULL` FK |
| `profile_code` | `TEXT NULL` |
| `asset_code` | `TEXT NULL` FK assets |
| `base_required_approvals` | `INT NOT NULL CHECK (>= 0)` (the `>= 1` mandatory-class rule is by trigger, so the interim is a single switch) |
| `threshold_minor_units` | `NUMERIC(38,0) NULL CHECK (>= 0)` |
| `required_approvals_above_threshold` | `INT NULL` |
| `effective_from` | `TIMESTAMPTZ NOT NULL` |
| `supersedes_id` | `UUID NULL` |
| `change_id` | `UUID NOT NULL UNIQUE` FK to changes |
| `author_person_id`, `approver_person_id` | `UUID NOT NULL` |
| `legal_review_reference` | `TEXT NULL CHECK (octet_length <= 256)` |
| `created_at` | |

**CHECKs:**
- level ↔ scope columns: `platform`: all NULL; `jurisdiction`: only `jurisdiction_id`; `profile`:
  only `profile_code`; `tenant`: only `tenant_id`; `brand`: `tenant_id` + `brand_id`;
- `(threshold_minor_units IS NULL) = (required_approvals_above_threshold IS NULL)`;
- `threshold_minor_units IS NULL OR asset_code IS NOT NULL` (LF-12);
- `required_approvals_above_threshold IS NULL OR required_approvals_above_threshold >=
  base_required_approvals`.

**Triggers:** INSERT only from the change-approval trigger (`decided_txid = txid_current()` of the
change approval); the `base >= 1` rule for the mandatory class; the tightening rule (§3.3); UPDATE
and DELETE refused.

**Policies:**
- SELECT:
  - rows with `tenant_id IS NULL`: T, P and A;
  - tenant and brand rows: T (own), A (X), P.
- INSERT: P (any level); T (own tenant and brand rows).

### 10.3 `tenant_financial_policy_profiles`

`(tenant_id, profile_code, effective_from, change_id)`, append-only. SELECT: T (own), A (X), P.
INSERT: P only, through the change path.

### 10.4 `financial_approval_policy_changes`, `financial_approval_policy_change_approvals`

- **Changes:** proposed-row JSONB, `content_hash`, requester, scope and Person (forced), `status`,
  `expires_at`.
- **Change approvals:** `content_hash`, approver, scope and Person (forced), `decided_txid`.
- **Triggers:** distinct principal and Person; the §3.1 authorship matrix; the tightening rule.
- **Policies:** T (own tenant and brand changes) and P (all). **No A.**

### 10.5 `ledger_adjustment_requests` (families T, A; **no P**)

**Columns:**
- the §5.1 payload;
- `payload_hash` (DB-computed);
- `initiated_by`, `initiated_by_scope`, `initiated_by_person_id` (forced);
- `tenant_status_at_submission`, `required_at_submission`, `contributing_policy_ids UUID[]`;
- `state`, `expires_at`, `executed_txid BIGINT NULL`;
- `ledger_transaction_id UUID NULL UNIQUE`, composite FK → `ledger_transactions_id_tenant_key`;
- `refusal_code TEXT NULL`;
- `idempotency_key TEXT GENERATED ALWAYS AS ('manual_adjustment:' || id::text) STORED`, with
  `UNIQUE (tenant_id, idempotency_key)`.

**CHECKs:**
- `account_type = 'player_cash'`;
- `direction IN ('credit_player','debit_player')`;
- the amount bound;
- `(state = 'executed') = (ledger_transaction_id IS NOT NULL)`;
- `state IN ('refused_insufficient_funds','refused_at_execution') ⇒ ledger_transaction_id IS NULL`.

**Triggers:**
- payload immutability;
- forced actor;
- reason-code rules (§5.4), the asset rule, the exposure refusal and the tenant-status rule
  (§5.2);
- S-2(iii); the beneficiary guard;
- state machine;
- the link trigger (§6.5);
- the deferred `executing` check.

### 10.6 `ledger_adjustment_approvals` (T, A; no P)

- **Columns:** `request_id` with composite FK; `decision`; `payload_hash`; approver, scope and
  Person (forced); `decided_txid`; `reason_code`.
- **UNIQUE** `(request_id, decided_by)`.
- **Triggers:** distinct Person, beneficiary, S-2(iii), grant present at insert, request `pending`.
- Immutable.

### 10.7 Acting policies and fences on existing tables (ADR 0099 §6.5–§6.7)

| Table | Acting access |
|---|---|
| `player_accounts` | SELECT |
| `wallets` | SELECT |
| `ledger_accounts` | SELECT; INSERT, restricted by account type |
| `ledger_transactions` | SELECT; INSERT (fenced); UPDATE USING X WITH CHECK `false` (for the L2 causation lock only; 0082's immutability triggers refuse real updates anyway) |
| `ledger_entries` | SELECT; INSERT (fenced) |
| `wallet_balance_projection` | SELECT; INSERT and UPDATE, **projection fence** |
| `payment_attempts`, `deposit_intents` | SELECT (LF ruling 2) |

0112 also creates:
- `ledger_transactions_governed_fence` with branch (a) only;
- `ledger_entries_governed_fence`;
- `wallet_balance_projection_acting_fence`.

### 10.8 Reconciliation kind (LF ruling 4)

- `ALTER TABLE reconciliation_mismatches DROP CONSTRAINT reconciliation_mismatches_mismatch_kind_check`,
  then `ADD` it back as a **strict superset of the latest definition at merge time** (0107's list
  today) plus **`'ledger_unlinked_manual_adjustment'`**.
- 0114 widens it again (ADR 0101 §8).

### 10.9 Down

- Refuse (`MA099`) while any row exists in the requests, approvals, policies, changes or profile
  tables, **or while any `ledger_unlinked_manual_adjustment` mismatch row exists**.
- Otherwise:
  - restore the previous kind CHECK exactly;
  - drop the fences, the acting policies, the tables and the functions;
  - drop the FK added to the 0111 catalogue.

The existing ledger policies are left byte-identical.

## 11. Static tests (C-100-4)

- **B-21:** `ledger.TxManualAdjustment` appears in non-test Go only in `internal/ledger` (constant
  and validation) and `internal/adjustment` (the single caller). This test is mandatory in K2.
- **B-22:** the ADR 0099 A-19 column discipline, applied to `internal/adjustment`.

## 12. The detective control: `ledger_unlinked_manual_adjustment` (LF ruling 4)

**What it checks.** A new check in the ledger reconciliation run (`internal/reconciliation`; a K2
Touches addition). It raises a **P1** mismatch for every `ledger_transactions` row where:
- `transaction_type = 'manual_adjustment'`;
- `created_at >= financial_control_classifications.governed_since` for `ledger_adjustment`;
- no `ledger_adjustment_requests` row has `state = 'executed'` and `ledger_transaction_id` equal to
  it.

It is standing and unwindowed. It routes as a P1 via ADR 0102 once I-wire lands.

**The preventive rule is deferred to LEDGER-MANUAL-ADJ-LINK-1** (= MANUAL-ADJ-LINK-1;
launch-blocking for the first real-money tenant):
- a BEFORE INSERT trigger for **all** sessions, requiring an executing request;
- the **19** test files that post `TxManualAdjustment` directly move to request-backed helpers
  first. Revision 1 said 24; worktree copies inflated that count (LF F8);
- **no test-bypass GUC in the production schema.**

## 13. Tests and mutants (K2 DoD; LF K2 list, LF rev-2 tests 4–8 and 14, QA W2)

Notes:
- T-1 clock; no new wall-clock assertion.
- Results are reported PASS / FAIL / FLAKE / NOT RUN / BLOCKED. Local runs are never labelled CI.
- **Every test also asserts** SUM(D) = SUM(C) and projection = recomputed (LF test 14).

| ID | Class | Test |
|---|---|---|
| B-1 | R | Credit and debit executed in the final approval's tx |
| B-2 | PROP | Random sequences, including refusals: the invariants hold |
| B-3 | RLS | Tenant and acting flows. Plain platform: refused. Acting Y on X: refused. **K1-1 negative (C-100-6):** the acting session used for K2 cannot insert a `platform_admin`, read `password_hash`, or write platform audit, sessions or `persons` rows. |
| B-4 | ADV | Payload change after submission; a stale or foreign `payload_hash`; an `evidence_ref_hash` differing from the pinned one. All refused. |
| B-5 | AZ | Self-approval; same Person; unlinked Person; the beneficiary as initiator or approver. All refused. |
| B-6 | AZ | Suspended or demoted approver; grant revoked or expired before execution: not counted |
| B-7 | CON | **`FOR SHARE`** revoke vs execute (LF test 3); two concurrent final approvals → one ledger transaction; two concurrent `compensating_entry` requests on one causation → the cap holds (L2) |
| B-8 | R | Tightened between submission and execution → an extra approval needed; loosened → the pinned value holds |
| B-9 | R | No platform baseline → disabled; tenant row only → disabled; unresolvable jurisdiction with a jurisdiction row → disabled |
| B-10 | ADV | Tenant loosening refused; single-Person platform change refused; **the policy author as initiator or as approver** refused (C-100-1) |
| B-11 | R | Debit beyond balance → `refused_insufficient_funds`, committed; debit equal to balance posts |
| B-12 | ADV | Any non-§4 shape is unrepresentable; a forged link is refused |
| B-13 | R | **Ruling 1 (LF test 5):** `compensating_entry` with another wallet, another asset, a `deposit`/`deposit_reversal`/`tombstone` causation, a cumulative excess, or a missing hash → refused. `goodwill_credit` debit → refused. `external_instruction` without a hash → refused. |
| B-14 | R | `above_threshold` without an asset → CHECK; `2^63` → refused |
| B-15 | FL/RB | A crash between steps 7 and 10 → nothing committed; a committed `executing` is refused |
| B-16 | ADV | Acting fence: an unexecuting `manual_adjustment` or a `casino_bet` → refused |
| B-17 | AU | One audit row per transition; the note is bounded (`> 1000` bytes or control characters → refused) |
| B-18 | R | Reconciliation drift = 0 |
| B-19 | MIG | 0112 up/down/up; down refuses with rows; the kind CHECK is restored exactly |
| B-20 | R | Classification pinned; **a fixture policy row with `base = 0` still evaluates to 1** (LF test 8); a `base = 0` insert for the mandatory class is refused by trigger |
| B-23 | R | **Ruling 2 (LF test 4):** a credit (any code) under an open exposure is refused at submission and at execution, with **no statement source configured**; admitted after a tombstone or reversal; debits unaffected |
| B-24 | R | **Ruling 3 (LF test 6):** suspended asset → goodwill refused, compensation succeeds; suspension between submission and execution → `refused_at_execution` for goodwill. **K2-b:** a tenant-disabled asset (0045 layer) behaves the same. |
| B-25 | R | **Ruling 4 (LF test 7):** a post-cutover `manual_adjustment` posted by a test fixture outside a request raises `ledger_unlinked_manual_adjustment` on the next run; a request-backed one does not |
| B-26 | R | **C-100-3:** non-active tenant → goodwill refused; other codes allowed. **K2-1:** a non-active tenant's tenant-level tightening is ignored for `payment_force_resolve` and applied for `ledger_adjustment`. |

**Mutants:** drop the payload hash; drop distinct-Person; drop `FOR SHARE`; drop the in-tx grant
read; drop the link match; drop the sufficiency check; drop the beneficiary guard; use `xmin`;
evaluate most-specific-wins; drop the platform-baseline requirement; drop `GREATEST(1, …)`; drop
the exposure refusal (at submission, or at execution); drop the compensation cap or the L2 lock;
drop the deferred check; drop S-2(iii) for approvers.

## 14. Invariants

**Preserved:**
- **INV-DEP-1:** preserved preventively. No credit can execute while the player has an open
  captured-unposted exposure (§5.2). There is no deposit reference and no deposit-allocation code,
  and causation to deposits, reversals and tombstones is refused.
- **Append-only double-entry:** `ledger.Post` only. Corrections are compensating entries, capped by
  causation.
- **DB idempotency:** UNIQUE `(tenant_id, idempotency_key)`; UNIQUE `ledger_transaction_id`; the L1
  lock.
- **RLS:** FORCE RLS; no plain-platform access to tenant money rows; acting sessions fenced.
- **No direct balance mutation:** the projection fence; sufficiency read under L3.
- **HD-LEDGER-UNALLOC-1 "A now, B later":** unchanged. LEDGER-SUSPENSE-B-1 stays deferred.

**Introduced:**

| ID | Invariant |
|---|---|
| INV-ADJ-1 | Only the §4 shape, exactly the approved payload, posted in the final approval's tx |
| INV-ADJ-2 | Required approvals = `GREATEST(1, MAX(applicable rows))`, and never below the pinned value |
| INV-ADJ-3 | A counted approval has an in-force grant (locked `FOR SHARE`), a distinct non-NULL Person, is not the beneficiary, and is not an author of a contributing policy |
| INV-ADJ-4 | A debit never makes `player_cash` negative |
| INV-ADJ-5 | No credit executes under an open captured-unposted payment exposure |
| INV-ADJ-6 | Cumulative compensations per `(causation, direction)` never exceed the causation leg |

## 15. Alternatives rejected

| Alternative | Why rejected |
|---|---|
| A `never` mode | HD-PRH2-1 |
| Most-specific-wins evaluation | S-2 |
| Tenant authoring with tenant-local four-eyes | Not chosen |
| An open posting shape | LF-9, S-13 |
| Approve now, execute later | LF-13 |
| `xmin` as the same-transaction marker | LF-10 |
| A sufficiency check inside `ledger.Post` | A `ledger` edit |
| A `distinct_principal` option | LF-11 |
| **Detection only for unposted captures** | LF F4: refunds would pay twice. Preventive refusal adopted. |
| **K2-1 option (a), a numeric bound** | §6.7 |
| **A test-bypass GUC for the preventive link rule** | LF ruling 4 |

## 16. Open items

1. **HD-PRH2-8:** HUMAN DECISION REQUIRED. The interim is (b).
2. **RESOLVED:** the ruling 1 vs ruling 5(c) causation conflict (§5.4). It is ADOPTED with C-4 and implemented in K3.
3. **Ledger-finance pins: DONE.**
   - The tombstone key, with the `deposit_reversal` arm removed (K2-a, §5.2).
   - The suspended asset now includes the 0045 tenant layer (K2-b, §5.2). K2 pins the column predicate.
   - The A8 L2 addition is CONFIRMED (§8).
   - Security C-2 binds K2: B-3's K1-1 negatives, and A-19 via B-22, run in K2's real acting executor transaction.
4. **LEDGER-MANUAL-ADJ-LINK-1** (preventive link, 19 fixtures): launch-blocking for the first
   real-money tenant.
5. **Touches additions (orchestrator, Rule 1):** `internal/reconciliation` (the §12 check). Its
   scheduler registration conflicts with I-wire's `scheduler.go` ownership, so the orchestrator
   sequences them.
6. **ADR 0082 A8** (§8) is written by the orchestrator.
7. **Player-jurisdiction vs licence-jurisdiction** as the policy key: LEGAL / COMPLIANCE REVIEW
   REQUIRED. The licence jurisdiction is used until ruled otherwise.

## 17. Review disposition

| Finding | Where addressed |
|---|---|
| **Product-owner-proxy** | |
| ACCEPT WITH CONDITIONS (minor) | §3.4 routes HD-PRH2-8 promptly; the other items are in ADR 0099 and 0101 |
| **Security** | |
| K2-1 / C-100-2 | §6.7 (choice (b), justified); §3.2; B-26 |
| K2-2 / C-100-4 | §11 B-21; §12 (LEDGER-MANUAL-ADJ-LINK-1) |
| K2-3 / C-100-5 | §5.1 note bound; B-17 |
| C-100-1 / ruling 4 | §3.3; §6.2; B-10 |
| C-100-3 / ruling 5 | §5.2; §5.4 table; B-26 |
| C-100-6 | B-3 |
| **Ledger-finance** | |
| F4 (HIGH) / ruling 2 | §5.2 (`player_open_payment_exposure`, MA020, no override); INV-ADJ-5; B-23; acting SELECT in ADR 0099 §6.5 |
| F5 / ruling 1 | §5.4; §10.1; B-13; the conflict with ruling 5(c) is flagged in §5.4 and §16.2 |
| F6 | §5.1 (`evidence_ref_hash` in the payload hash); B-4 |
| F7 / ruling 7 | §3.2 `GREATEST(1, …)`; §3.4 preconditions for (a); B-20 |
| F8 / ruling 4 | §12 (19 fixtures; detective kind; no bypass GUC); §10.8; B-25 |
| F15 / ruling 6 | §6.5; §8 (A8 corrected; approval inserts belong to no class); the L2 causation addition is flagged for confirmation |
| Ruling 3 | §5.2; §5.4; B-24 |
| F3 | §6.2; §6.5 step 4; B-7 |
| LF rev-2 tests 3–8, 14 | B-7, B-23, B-13, B-24, B-25, B-20, every test |
| F1, F2 | ADR 0099 §6.6–§6.7; §10.7 |
| F9–F14, F16, F17, ruling 5 | ADR 0101 |
| **Orchestrator** | Registry ids used: HD-PRH2-8, LEDGER-MANUAL-ADJ-LINK-1 (= MANUAL-ADJ-LINK-1) |

## 18. Handover / DoD

K2 updates:
- `docs/architecture/ledger-accounting-model.md` (the shape, the catalogue, the compensation cap,
  the exposure refusal);
- `06-wallet-ledger-architecture.md` and `financial-transaction-flows.md` (the "manual adjustment"
  flow);
- `reconciliation-model.md` (`ledger_unlinked_manual_adjustment`);
- `docs/security/security-architecture.md`;
- `backoffice/src/auth/permissions.ts`;
- `docs/runbooks/operational-runbooks.md`, entry "Manual adjustment":
  - submitting and approving;
  - insufficient funds and `open_payment_exposure` refusals, and why there is no override;
  - the policy-change procedure;
  - that nothing is enabled until the platform authors a baseline;
  - how the platform unblocks an over-tightened tenant (§6.7);
- `deploy/init-app-role.sql`.

The orchestrator updates: ADR 0082 A8; the registry (LEDGER-MANUAL-ADJ-4EYES-1, HD-PRH2-8,
LEDGER-MANUAL-ADJ-LINK-1); the HANDOVER index; the review records.
