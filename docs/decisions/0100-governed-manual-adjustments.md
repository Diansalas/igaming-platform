# ADR 0100 — Governed manual adjustments and financial approval policies (PRH-2 K2)

- **Status:** ACCEPTED (2026-09-28). `security` CONFIRMED WITH CONDITIONS and `ledger-finance`
  CONFIRMED WITH CONDITIONS, both on revision 2
  (`docs/plans/prh2-hardening-round/reviews/adr-0099-0101-{security,ledger-finance}-confirmation.md`).
  The orchestrator wrote their conditions (LF K2-a, K2-b and K3-a; security C-2 and C-4) into this text.
  **Implementation (2026-09-28): see §20 - IMPLEMENTED on the unmerged K2 branch
  (`prh2-k2-manual-adjustments`, migration 0113), NOT YET REVIEWED by `security` /
  `code-reviewer` / `qa`.**
  **Amendment 2026-10-15 (A4): §21 - MANUAL-ADJ-LINK-1, the session-independent link fence
  (migration 0153). IMPLEMENTED on the A4 branch; reviews pending.**
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
launch-blocking for the first real-money tenant). *Amended 2026-10-15: implemented as §21 (migration 0153),
with AFTER INSERT constraint triggers instead of the BEFORE trigger sketched below - see §21.2 for why.
The detector stays, as the backstop for the owner-DDL residual (§21.5). **Accepted deviation (security A4
C-1):** the fixture files did NOT move to request-backed helpers as the second bullet below says; they use an
owner-only trigger-disable fixture exemption (§21.4). No test-bypass GUC exists in the production schema.*
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
   real-money tenant. *2026-10-15: implemented, §21; reviews pending.*
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

## 19. Conditions carried from K1's reviews (orchestrator, 2026-09-28)

These bind K2. The sources are `reviews/k1-security.md`, `reviews/k1-ledger-finance.md` and `reviews/k1-architect-ruling-gp1.md`. **Migration number:** K2 is **0113** (re-allocation in the plan); references to "0112" in this ADR mean K2's migration.

- **Security K2-P1:** A-18 (the static NULL-arm migration replay) exists and passes before K2 merges.
- **Security K2-P2:** A-12 and A-1 (including the cross-tenant path cases) before K2.
- **Security K2-P3:** a §6.1 call-site static test for the first `WithPlatformActingInTenant` caller. `principalID` comes from `tenant.FromContext(ctx).Subject`, and `targetTenantID` is the `canActOnTenant`-validated path value.
- **Security K2-P4 / architect I-6:** use-time re-checks in the K2 transaction: the grantee is `active`, its role is eligible, its tenant is unchanged, and its live `person_id` = the request's `grantee_person_id`. Each check has its own mutant.
- **LF C-K1-2:** the migration that adds any acting permissive policy on `ledger_transactions`, `ledger_entries`, `ledger_accounts` or `wallet_balance_projection` must first create, in the same migration, the ADR 0099 §6.6 and §6.7 fences (branch (a) here).
- **LF C-K1-3:**
  - (a) the execution-time LF-11 floor reads live `staff_users.person_id` `FOR SHARE`, never the snapshot;
  - (b) B-7 proves `FOR SHARE` on grants and staff rows from tenant **and** acting sessions inside the real executor, and that a revoke committed first is re-read as revoked;
  - (c) in-force is evaluated at `now()`; any "as of" evaluation uses `(revoked_at IS NULL OR at < revoked_at)`.
- **LF N-2:** after K2's acting SELECT on `ledger_accounts` lands, A-4/B-16 must re-assert that a non-governed `ledger_entries` insert is refused.
- **Migration tests:** migrate a scratch DB only through 0113 (the `scratchPoolThrough0110` pattern).
- **Architect R-12 ruling, I-5:** executors select and lock `FOR SHARE` the grant **in force at `now()`** (`revoked_at IS NULL AND valid_from <= now() AND (valid_until IS NULL OR now() < valid_until)`), never "the unrevoked grant". Several unrevoked rows may exist per key once renewals are queued or grants have expired.
- **Security K1 re-check hard gates (before K2 merges; `reviews/k1-security-recheck.md`):**
  - **K2-G1:** A-18 false negatives fixed. Guards must be positive, `true` arms are flagged, SELECT NULL arms are allowed only for the §6.2 allowlist, plus a dynamic acting-session row-visibility check.
  - **K2-G2:** an A-4 case where an acting session reads another tenant's grants and sees 0 (kills M9), plus a rolled-back-DDL layered test of the setter's tenant arm.
  - **K2-G3:** an A-8 acting case: suspend the grantee after the grant; `WithPlatformActingInTenant` must give CG020 (kills M10).
  - **K2-G4:** K2-P2..P4.
  - The "drop the grant function from one acting policy" mutant is mandatory for every acting policy K2 adds.

## 20. Implementation status (PRH-2 K2, `ledger-finance` owner, branch `prh2-k2-manual-adjustments`)

**Label: IMPLEMENTED (engineering, on the unmerged K2 branch) — reviews
ACCEPT WITH CONDITIONS; condition fixes (§20.5) awaiting re-review.** Every
K2 deliverable in §3–§12 and the K1 follow-up gates of §19 are built and
tested. `security` (18c357a) and `code-reviewer` returned ACCEPT WITH
CONDITIONS; the pre-merge conditions are fixed on the branch (§20.5) but
their closure is for those reviewers to confirm, not for this author.
The items in §20.4 are explicitly NOT IMPLEMENTED or deferred.

### 20.1 What exists

- **§19 hard gates (committed first, `1f45882`):** K2-G1 (A-18 hardened:
  positive guards only, `true` arms flagged, SELECT exposures accepted only
  on the ADR 0099 §6.2 allowlist; planted cases for all three of security's
  false negatives; a dynamic acting-session visibility probe over every
  public table); K2-G2 (acting X sees 0 of Y's grants - M9 killed; a
  rolled-back-DDL layered test AND a committed scratch variant through the
  real setter - "widen acting to any tenant" killed); K2-G3 (suspended
  grantee → CG020 - M10 killed). K2-G4: A-1/A-12 over HTTP for K2
  (`internal/httpserver/manual_adjustment_api_integration_test.go`), the
  §6.1 call-site AST pin (`TestK2P3_ActingSetterCallSitePinned`), use-time
  re-checks each with a killed mutant (active, eligible role, tenant
  unchanged - layered, live person = grant snapshot), and "lock the grant in
  force at `now()`" (queued-future and expired unrevoked grants never count).
- **Migration 0113** (`migrations/0113_governed_manual_adjustments.{up,down}.sql`):
  §10.1–§10.9 in full, with the §20.3 decisions; SQLSTATE class `MA`.
- **`internal/adjustment`**: `SubmitInTx`, `DecideInTx` (the final approval
  executes in its own transaction, §6.5 steps 1–10, ADR 0082 A8 order),
  `CancelInTx`, reads, policy-change propose/decide/cancel, the ONE
  evaluator call (`EvaluateInTx`); the single `WithPlatformActingInTenant`
  call site (`runSession`), principal from `tenant.FromContext(ctx).Subject`,
  target from `NewTarget` (the canActOnTenant rule on the path value).
- **HTTP** (`internal/httpserver/manual_adjustment_routes.go`, one line in
  `routes.go`): submit/list/get/approve/reject/cancel under
  `/v1/admin/tenants/{tenantID}/manual-adjustments`, policy changes under
  `/v1/admin/financial-policy-changes` (platform-family rows) and
  `/v1/admin/tenants/{tenantID}/financial-policy-changes` (tenant/brand rows,
  profile assignments), `GET /v1/admin/financial-policies`. Static
  permissions `ledger_adjustment:{initiate,approve,read}` (finance,
  platform_admin; read also compliance) in `internal/auth/permission.go`
  and `backoffice/src/auth/permissions.ts` (flags only - no backoffice page).
- **Detector** `ledger_unlinked_manual_adjustment` in
  `reconciliation.RunLedgerVsProjection` (already scheduled; `scheduler.go`
  unchanged).
- **Docs**: ADR 0082 Amendment A8 (applied; manual-adjustment order
  IMPLEMENTED), ADR 0099 §20 (amendments pending security), the runbook §11,
  `reconciliation-model.md` §2.1a, `ledger-accounting-model.md` §7.K2,
  `financial-transaction-flows.md` §16a, `06-wallet-ledger-architecture.md`,
  `security-architecture.md`, `deploy/init-app-role.sql`.

### 20.2 Tests (all under `-race -tags integration`, local private DB - never labelled CI)

B-1 … B-26 (B-18 inside B-25), plus: the acting-policy grant-function probes
(24 policies), the layered execution-count tests (a bad approval planted past
the insert guards is never counted: stale hash, initiator's Person,
beneficiary, policy author, no grant, one Person twice), txid-not-xmin,
K2-G4 scratch tests, B-19 whole-schema up/down/up equality, and every test
asserts SUM(D) = SUM(C) and projection = recomputed (`assertInvariants`).
Mutation results: `docs/plans/payment-readiness/evidence/prh2-k2-mutation-kill.txt`.
The round-1 "68/68 killed" claim is WITHDRAWN: the security review found
two survivors that round 1 never ran (F2, F7). The fix round (§20.5)
re-ran every mutant, including F1/F2/F7 and the new ones; the evidence
file is the only authoritative count.

### 20.3 Implementation decisions and deviations (disclosed)

1. **Proposed policy rows are typed columns**, not a JSONB blob (the same
   CHECKs then apply at proposal time); `content_hash` is DB-computed over a
   length-prefixed canonical encoding.
2. **A policy row is never back-dated:** `effective_from :=
   GREATEST(proposed, now())` at approval, so past evaluations stay
   reproducible. Unique per (key, effective_from).
3. **Acting `ledger_accounts` INSERT** is limited to `player_cash` /
   `manual_adjustment` (stricter than ADR 0099 §6.5's four types).
4. **Three acting SELECT policies beyond ADR 0099 §6.5** (`licences` own,
   `asset_authorizations`, `staff_capability_grant_requests`) and one
   system-tenant read (`tenant_system_read_executed`, executed requests only,
   for the §12 detector) - see ADR 0099 §20. **Pending security review.**
5. **K2-b pin:** suspended = `NOT (assets.active AND
   assets.platform_authorized)` OR no `asset_authorizations` row with
   `scope_kind = 'tenant' AND product IS NULL AND eligible`. 0045 rows carry
   no effective dating, so "not in force" = absent or `eligible = false`.
6. **Compensation cap pin:** the causation's cap is the SUM of its
   `player_cash` legs on this wallet in this asset (any direction).
7. **The note** is validated in Go and by `ledger_adjustment_note_hash()`
   (the INSERT computes `note_hash` through it). The DB cannot prove that the
   note written to `audit_log` hashes to `note_hash` (acting sessions cannot
   read `audit_log`); the executor writes both in one transaction.
8. **Counting is ONE SQL function** (`ledger_adjustment_execution_status`),
   used by the executor after its `FOR SHARE` locks and re-run by the
   `→ executing` guard as a backstop; there is no Go copy of any rule.
9. **§7.6 residual as built:** see ADR 0099 §20 item 6.
10. **The detector lives inside `RunLedgerVsProjection`** (standing,
    unwindowed); no `scheduler.go` change was needed. Four pre-K2 test
    assertions that required a clean ledger run on fixtures funded by raw
    `manual_adjustment` postings now accept exactly this kind and nothing
    else (casino consistency, two sportsbook settlement tests, wallet locked
    split); moving those fixtures is LEDGER-MANUAL-ADJ-LINK-1.
11. **Real defect found and fixed in 0113's own down:** FORCE RLS binds the
    migration role, so the MA099 row checks saw zero rows and the down would
    have silently dropped live governed data. The down now lifts FORCE on
    the checked tables first (the 0110/0112 precedent); B-19 and mutant D01
    pin it.
12. **HTTP mapping:** CG020 (a platform caller without a grant for the
    tenant) → 403; MA0xx refusals → 409 (MA001/MA003 → 403, MA014 → 409
    "not enabled", MA020 → 409 `open_payment_exposure`). Refusals write a
    denied audit row (tenant row for tenant callers; platform row with
    `subject_tenant_id` for platform callers).
13. **`evidence_ref_hash`** is accepted from the client as a hex SHA-256;
    the evidence reference itself never reaches the platform.
14. **Technical defaults** (not money values): request and policy-change
    TTL 24 hours.

### 20.4 NOT IMPLEMENTED / deferred / open

- **K3's Step-B causation arm** (§5.4 RESOLVED, ADOPTED): NOT IMPLEMENTED
  here by design - ruling 1 ships literally; K3 (0115) adds it.
- **LEDGER-MANUAL-ADJ-LINK-1** (preventive link trigger for all sessions,
  19 fixture files): NOT IMPLEMENTED (deferred; launch-blocking). *Superseded 2026-10-15
  by §21 (migration 0153): IMPLEMENTED on the A4 branch, reviews pending.*
- **HD-PRH2-8**: OPEN (human). Interim (b) enforced (`base >= 1` trigger +
  `GREATEST(1, …)`).
- **P1 routing** of `ledger_unlinked_manual_adjustment`: I-wire (ADR 0102).
- **Backoffice UI pages** for requests/policies: NOT IMPLEMENTED (flags only).
- **Reviews:** `security` and `code-reviewer`: ACCEPT WITH CONDITIONS
  (conditions fixed in §20.5, closure pending their re-check);
  `ledger-finance` independent review: ACCEPT WITH CONDITIONS (C-K2-1 bound
  to I-wire); `qa`: not done.
- **Launch flags unchanged:** TM-7, TM-10, HD-PRH2-8, LEDGER-MANUAL-ADJ-LINK-1.
- **STAFF-LIFECYCLE-1 (launch prerequisite, recorded per the security
  review §5):** the §7.6 residual - a platform approver's own staff row is
  invisible to the tenant-scope execution, so only its grant and Person
  snapshot are re-checked at execution, not its status - is accepted for
  merge ONLY. Before any real-money use it depends on STAFF-LIFECYCLE-1
  revoking a principal's grants in the same transaction that suspends it,
  DB-enforced (security S-b).

### 20.5 Fix round after the K2 security and code reviews

Security review (`docs/plans/prh2-hardening-round/reviews/k2-security.md`)
and code review (`k2-code-review.md`), both ACCEPT WITH CONDITIONS.

- **K2-C1 (High) - FIXED.** (i) Every non-executed exit of a request
  (`refused_insufficient_funds`, `refused_at_execution`, `rejected`,
  `cancelled`, `expired`) raises MA040 while a `ledger_transactions` row
  carries `manual_adjustment:`||id in the tenant - in every session family,
  not only acting (this is also code review R-1). (ii) The acting entries
  fence validates each entry against the executing request: the account
  is the request wallet's `player_cash` (player direction) or the tenant's
  `manual_adjustment` house account with `wallet_id IS NULL` (opposite
  direction); asset and amount equal the payload; at most two entries and
  one per direction. Tests: `TestK2C1_ActingFenceEnforcesClosedShape`
  (security's probe, the exact shape followed by the refused exit,
  wrong wallet, wrong direction, split legs, duplicated balanced legs) and
  `TestK2R1_TenantSessionNonExecutedExitRefusedAfterGovernedPosting`.
- **K2-C2 - FIXED (test).** `TestK2C2_FenceStateArmAfterExit`: the
  required extra-entry-after-executed case, plus a governed posting after
  a legitimate refused exit, which is what isolates the state arm now
  that (ii)'s count arm also refuses the first case.
- **K2-C3 - FIXED (test).** `TestK2C3_DBRecountOnExecuting`: pending ->
  executing driven directly with 1 of 2 approvals, and with an invalid
  initiator (initiate grant revoked); the UPDATE itself must raise MA030.
- **R-2 - FIXED.** A decision on an expired request commits the expiry
  transition and its audit row and returns `ErrRequestExpired` -> 409
  "request expired"; no approval row is written.
- **R-3 - FIXED.** MA021/MA022/MA025 -> 400 with a token from a CLOSED set
  (`adjustment.RefusalToken`; any other text falls back to a per-code
  generic token); 40001/40P01 -> retryable 409; a denied audit row on each
  path.
- **R-4 - FIXED.** The reconciliation fixture helper decides in memory from
  the returned mismatches; the sweep variant (which only holds the Run)
  requires at least one visible row for a non-clean run.
- **LOW - FIXED.** The A-18 guard check is structural over each policy
  clause (an OR-ed guard no longer counts; planted cases); the 0113 down
  file states it must run in a single transaction.
- **F1** (drop the fence's `executed_txid` arm) is EQUIVALENT, as security
  accepted; it is listed in the evidence file.

## 21. MANUAL-ADJ-LINK-1: the session-independent link fence (migration 0153)

- **Status:** IMPLEMENTED on the A4 worktree branch (not merged, not CI-run), `ledger-finance`, 2026-10-15.
  **Pending:** `security` review, independent `ledger-finance` / `code-reviewer` review, hosted CI.
- **Registry:** MANUAL-ADJ-LINK-1 (= LEDGER-MANUAL-ADJ-LINK-1). Proposed registry text:
  `docs/plans/a4-adjustment-fence-record.md` (the orchestrator is the single registry writer).
- **Numbering note.** The A4 task text says "ADR 0113"; the acting-GUC fences it names
  (`ledger_transactions_governed_fence`, `ledger_entries_governed_fence`) are migration 0113's and are
  specified by this ADR (ADR 0113 is the withdrawal state machine). This section is therefore the amendment.

### 21.1 The gap

Migration 0113's ledger fences (§10.7, ADR 0099 §6.6) fire only when an acting GUC is present
(`financial_acting_gucs_present()`). A session without acting GUCs - the runtime role with no GUCs, the
system-tenant shape, a tenant-principal shape, or the table owner - could insert a `manual_adjustment`
transaction and its entries with no governed request behind it. B-21 (a Go static test) and the §12
detective kind were the only guards. Under ADR 0110's accepted R1 class (arbitrary SQL as the runtime
role), that is a direct, ungoverned money path.

### 21.2 The mechanism (migration 0153)

Two `NOT DEFERRABLE` AFTER INSERT row **constraint triggers**, for **every** session (no GUC, role or
environment condition anywhere in them), no SECURITY DEFINER, `search_path` pinned:

1. `ledger_transactions_manual_adjustment_link`, `WHEN (NEW.transaction_type = 'manual_adjustment')`,
   function `ledger_manual_adjustment_link_check()`. The row must be the posting of a request that is
   (a) visible to the posting session in the row's tenant, with `id = correlation_id` and
   `idempotency_key = 'manual_adjustment:' || id`; (b) `state = 'executing'` with
   `executed_txid = txid_current()`; (c) `payload_hash = ledger_adjustment_payload_hash(request)`;
   (d) still approved: `ledger_adjustment_execution_status()` - the single 0113 implementation of the
   four-eyes / threshold count - reports `enabled`, `initiator_valid` and `counted >= required`,
   re-evaluated at the posting; (e) keyed exactly: `reason_code` and `causation_id` equal the request's,
   `provider_id`, `provider_tx_id`, `reverses_transaction_id` NULL. Refusal `MA050`.
2. `ledger_entries_manual_adjustment_link`, function `ledger_manual_adjustment_entry_check()`. For an entry
   whose parent (looked up in the entry's own tenant; an invisible parent fails closed) is a
   `manual_adjustment`: the parent's request must be executing in this transaction (so nothing can ever be
   appended to a committed adjustment), and the entry must be one leg of the closed §4 shape - the request
   wallet's `player_cash` in the player direction, or the tenant's `manual_adjustment` house account
   (`wallet_id IS NULL`) in the opposite direction - in the request's asset and amount, at most one leg per
   direction. Refusal `MA051`.
3. In the same entry function (security A4 F1): an entry whose parent is **not** a `manual_adjustment` may not
   post to any account of type `manual_adjustment`. Refusal `MA052`. Evidence that no legitimate type uses that
   account: no production code or migration other than the governed executor posts to it (the only
   `AccountManualAdjustment` reference outside `internal/ledger` is `internal/adjustment/execute.go`; the
   migrations mention `manual_adjustment` only in type/CHECK lists), and with MA052 in force the full
   integration suites hit it in exactly three places, all ADVERSARIAL test injections that picked the account as
   an arbitrary "wrong" account: the casino C2 posting-shape detector tests (stake/payout on a non-house account,
   4 subtests) and the RG S3 "undeclared player-side leg" tests (2). Those injections now use `psp_clearing`
   (house-level, resp. wallet-scoped); every assertion is unchanged. No legitimate posting path uses the
   account for another type. The account is visible whenever an entry may be
   inserted on it (the 0048 populate trigger refuses an invisible account first), so the check cannot be
   evaded by visibility.

**Why "executing in this transaction" is "approved and not yet executed".** 0113's request guard admits
`pending -> executing` only in the transaction of a same-transaction final APPROVE decision (itself bound to
a signed actor proof by 0120), after re-verifying the count and the payload rules; `executing` can never
commit (MA041); every non-executed exit refuses once a posting with the key exists (MA040); `executed`
runs `verify_link`. The fence adds the binding at the ledger itself, independent of the session's GUCs,
and re-checks (c) and (d) rather than trusting the state flag. **The recount (d) is as of the deciding
transaction's `now()`** - the same time-versioned evaluation as the 0113 guard (policies with
`effective_from <= now()`, grants in force at `now()`; the executor also holds the staff and grant rows
`FOR SHARE`). A policy tightening or grant revocation committed by another session while the decision is in
flight is therefore NOT caught - the same semantics as 0113 and its "tightened after submission" tests
(ledger-finance A4 review L2, reproduced by a probe). Only non-time-versioned inputs (tenant/brand status,
licence-derived jurisdiction, the player's Person link) can differ between the guard's and the fence's
evaluation; any divergence fails closed (MA050, the whole decision rolls back, the request stays pending).

**Why AFTER (constraint) triggers, not the BEFORE trigger §12 sketched.** An AFTER ROW trigger fires only
for a row actually inserted. A replay of an already-posted key fails on the `(tenant_id, idempotency_key)`
index first, so ADR 0020's idempotency contract (AlreadyPosted, ErrIdempotencyKeyReused,
ErrIdempotencyPayloadMismatch) is unchanged - a BEFORE trigger would turn every replay of a committed
adjustment into a fence error (mutant T19 below pins this). `NOT DEFERRABLE`: `SET CONSTRAINTS` cannot
postpone the check to a commit at which the request is no longer executing (mutants T18/E21).

**Why no SECURITY DEFINER.** The functions run with the caller's rights and RLS. A session that cannot
see the request (no GUCs, the system-tenant shape, a foreign tenant) is refused. `FORCE ROW LEVEL SECURITY`
binds the owner too, so a definer would gain nothing and would widen the attack surface. The pinned
`search_path` also means `ledger_adjustment_execution_status()` is evaluated against the real `public`
tables: the 0116 TEMP-shadow forgery is refused by the fence even with the proof triggers disabled
(`TestTempRevoke_K2ShadowAttack_SucceedsWhenTempIsGrantedBack`, new assertion).

**Lock order (ADR 0082 A8) unchanged.** The fence only reads: the request row the executor already holds
(L1, FOR UPDATE) and immutable ledger rows. No new lock, no new lock class.

**Cost.** The entry trigger runs for every ledger entry of every type: one primary-key lookup of the parent
transaction. Everything else (the request, the count, the shape) runs only for `manual_adjustment`.

**Unchanged.** The acting fences (CG030) still fire first for acting sessions with their K3/HSEC/M4 branches;
the MA020 rule and its 0115 Step B exemption stay in `ledger_adjustment_payload_refusal()`, run by the
`-> executing` guard in the same transaction; B-21 and the §12 detector stay (the detector is the
backstop for what the fence cannot see, below).

### 21.3 Owner-only exemptions: inventory (none added)

| Existing exemption | Where defined | Used by 0153's tests |
|---|---|---|
| The table owner (and a superuser) can `ALTER TABLE ... DISABLE TRIGGER` | ADR 0110 §9 item 4 ("the migration role is already outside the threat model (it owns every table and can disable any trigger)"); the 0131/0136 owner-residual notes | Yes: `internal/testsupport/adjlinkfix` disables exactly the two 0153 triggers inside the caller's own owner transaction and re-enables them in the same transaction |
| Seed rows inserted before FORCE RLS in a migration | 0112/0113 reference tables | No |
| `launch_is_table_owner()` owner paths (0128) | launch governance | No (not ledger) |
| `database_environment` owner-only write (0135) | RG synthetic config | No (not ledger) |

0153 adds **no** owner, GUC or environment bypass (a static test greps the up SQL for `current_user`,
`session_user`, `pg_has_role`, `current_setting`, `SECURITY DEFINER`, `rg_database_environment`). There is
no test-bypass GUC in the production schema (LF ruling 4).

### 21.4 The fixtures (the "19 files")

**Accepted deviation (security A4 C-1; ledger-finance, owner of ruling 4, accepts it; the orchestrator to
confirm).** §12 and the registry row said the fixture files move to *request-backed helpers*. They did not:
they use an owner-only fixture exemption (`adjlinkfix`: the two 0153 triggers disabled by the table owner inside
the fixture's own transaction, re-enabled in it). The cost is that those fixtures never exercise the fence; the
fence's own tests (§21.6) cover it. A request-backed helper would be the stronger design (each fixture would also
exercise the governed path), and remains a possible later improvement; it was not built because most fixtures
need shapes §4 cannot express (bonus legs, cash locks, arbitrary counter accounts) and the governed path needs
policies, grants, Persons and actor-proof keys in every package's world. The exemption is not a production-schema
bypass (no GUC, role or environment switch), is integration-build only, is invisible to other sessions (ALTER
TABLE is transactional and blocks their writers until the fixture commits), and every caller - including any raw
`DISABLE TRIGGER` of a fence trigger in a test (L3) - is pinned by `TestLinkFence0153_ExemptionCallersInventoried`.

The pre-0153 tests that posted an ungoverned `manual_adjustment` were inventoried at 32 `TxManualAdjustment`
references in 20 files outside `internal/adjustment` (plus 3 adjustment test files that post it deliberately,
and `migration_0113_integration_test`, whose scratch schema stops below 0153). None of them tests a manual adjustment:
they fund a wallet or post an arbitrary balanced shape. Disposition (every caller is pinned by
`TestLinkFence0153_ExemptionCallersInventoried`, which fails on any new or stale caller):

- **Owner-only fixture exemption** (`adjlinkfix.Without` / `adjlinkpost.Post` / per-package
  `mustPostFixture`): casino (2 files), idempotency (first posts only; every replay now runs with the
  fence enabled), launchgov, ledger (bonus_mirror, locked_origin_split, replay L7), reconciliation (2),
  risk (2), sportsbook (orchestrator, settlement_assertions), wallet. Integration-build only; listed in
  `TestNoTriggerDisableOutsideTests`'s allowlist with launchfix / staffsv / pitest.
- **Different non-governed type** where a trigger disable is impossible: the two lock-order racers
  (`TestLockOrder_ConcurrentFirstPostingsCreateSameAccounts_NoDeadlock`) now post `bonus_forfeiture`
  (the ALTER TABLE lock would serialize the racers and destroy the ABBA interleaving); the runtime-role
  closed-tenant probe (`TestTenantNotActive_DatabaseBackstopRefusesGameplayTypesOnly`) now proves
  "non-gameplay postings are unaffected" with a `deposit`, and additionally asserts an ungoverned
  `manual_adjustment` there is refused by MA050, never by GP010.
- **MA052 (final round):** the casino C2 detector tests and the RG S3 undeclared-leg tests used a
  `manual_adjustment` account as an arbitrary wrong account inside deliberately malformed casino / deposit
  postings; the injected account is now `psp_clearing` (house-level in C2, wallet-scoped in RG). Assertions
  unchanged.
- **Unaffected:** `internal/tenant/migration_0136` (scratch schema below 0153), `ledger_integration_test`
  L247 and the `lockorder_multipost` / `canonical_order` cases (conflict or pre-insert paths).
- **Deliberate adversarial tests in `internal/adjustment`** keep every original assertion and gain the
  fence assertion: B-12 (each forged shape now MA051/MA050; the original MA040 still proven with the
  fence absent), B-25 (the rogue posting is now MA050; the detector still raises the owner-planted one),
  K2-R1 (the pending-state postings are now MA050; the MA040 exits still proven with the posting planted
  through the owner residual), TempRevoke (the fence refuses the TEMP forgery; 0116's own non-vacuity
  still proven with the fence disabled on the scratch DB).

### 21.5 Threat model and residuals (what remains OUT of scope)

The fence holds against: a runtime-role session with no GUCs; any non-acting session shape; the table owner
without trigger DDL; forged tenant or acting GUCs without a matching approved request (no actor proof ->
no same-transaction approval -> no `executing` request); wrong amount / asset / direction / wallet /
counter account / extra leg / keys; an already-executed request; another tenant.

Out of scope, stated honestly:

1. **Owner / superuser DDL** (`DISABLE TRIGGER`, `DROP TRIGGER`, editing the function): ADR 0110 §9 item 4.
   `session_replication_role = replica` is a **superuser** residual only (security A4 C-2): on PostgreSQL 16 the
   non-superuser owner and the runtime role both get 42501, and `pg_parameter_acl` grants SET on it to no
   application role; a superuser with `replica` would skip both (`tgenabled = 'O'`) triggers. The §12 detector
   (P1 via I-wire) is the backstop and is kept; after 0153 any new finding means one of these happened
   (runbook: `docs/runbooks/operational-runbooks.md` §11, "Applying migration 0153").
2. **A genuine proof-backed final approval in the attacker's own transaction** is the governed path itself;
   compromised approver credentials / identity-store integrity are ADR 0110 §9 item 2 (T3/T4).
3. **Non-governed posting types** (deposits, withdrawals, casino, sportsbook, bonus) are ADR 0110 T5:
   0153 fences `manual_adjustment` transactions, their entries, and (MA052, security A4 F1) any other type's
   use of a `manual_adjustment` account. What stays open under T5: a runtime session can still mint with a
   non-governed type against any OTHER house account (e.g. a `deposit` against `psp_clearing`), and can append
   balanced entries to an already committed non-adjustment transaction (security A4 F5; a generic "entries only
   in the inserting transaction" seal is a ledger-finance follow-up under T5, not built here).
4. **Rows posted before 0153** are not re-validated (the §12 detector covers them from `governed_since`).
   Before 0153 is applied to an environment with real ledger data, the runbook step (ledger-finance A4 I2,
   `docs/runbooks/operational-runbooks.md` §11) requires zero or individually classified findings.
5. **Down** removes the fence (no data change); it must ship with a code rollback and the detector live.

### 21.6 Tests and mutation pass

Tests (`internal/adjustment`, `-tags integration`): `link_fence_0153_integration_test.go` (governed credit
and debit post, posting keys, audit, clean reconciliation; idempotent replay and payload-mismatch replay of
the governed key; concurrent final approvals post once; partial failure before the posting rolls back and
the retry posts once; compensating entry reverses an executed credit, over-compensation MA022; unlinked
insert refused for the runtime role AND the owner in every non-acting shape, via `ledger.Post` and raw SQL;
forged tenant / acting GUCs without an approved request; every wrong shape inside the executing
transaction; the fence's state / payload / count re-checks with the 0113 guard disabled; an executed request
cannot post again; tenant isolation; MA052 - the security review's `bonus_forfeiture` repro and a `deposit`
on the `manual_adjustment` account refused for runtime and owner, governed path unaffected; catalog pins),
`migration_0153_integration_test.go` (up adds exactly
6 objects; down restores the schema byte-for-byte and changes no ledger row; up/down/up twice; the fence
is enforced after each up and absent after each down - MA050 and MA052 both), `link_fence_static_test.go`
(exemption inventory including raw `DISABLE TRIGGER` of a fence trigger,
trigger-name agreement with the migration, no bypass tokens in the SQL). SUM(debits) = SUM(credits) and
projection = recomputation are asserted at the end of every money test (`assertInvariants`).

Mutation pass (kill-or-justify; private DB; each mutant applied to the live
schema as the owner, the behavioural fence tests run - catalog/static pin tests excluded so every kill is
behavioural - then 0153 re-applied). Driver: `docs/plans/prh2-hardening-round/a4-mutation-driver.py`.

- **Final pass, on the 0153 that will merge (with MA052): 47 mutants, 41 killed, 6 survived, all 6
  equivalent** (T06, T07, E01, E08, E09, E18). Complete log: `docs/plans/prh2-hardening-round/a4-mutation-kill.txt`.
- First pass (before MA052): 44 mutants, 38 killed, 6 survived (the same six). *Correction (ledger-finance A4 L1):
  this section first said "39 killed, 5 survived"; the table always listed six survivors.* Log:
  `docs/plans/prh2-hardening-round/a4-mutation-log-round1.txt` (T06b, run separately, appended).

| Id | Mutant | Result |
|---|---|---|
| T01 | tx: visible-request (not found) arm off | killed (UnlinkedInsert, message pin) |
| T02 | tx: key arm off | killed (B-12, ExecutingTx) |
| T03 | tx: state arm off | killed (ExecutingTx: posting after a refused exit in the same tx) |
| T04 | tx: txid arm off | killed (ChecksHoldWithoutTheRequestGuard) |
| T05 | tx: payload_hash arm off | killed (ChecksHold...: tampered payload) |
| T06 | tx: provider_id arm off | **survived - equivalent**: `ledger_transactions_check` forces `provider_id` and `provider_tx_id` NULL together, so the provider_tx_id arm refuses the same rows |
| T07 | tx: provider_tx_id arm off | **survived - equivalent** (same CHECK, mirror of T06) |
| T06b | tx: both provider arms off | killed (ExecutingTx: provider reference set) |
| T08 / T09 / T10 | tx: reverses / reason_code / causation arm off | killed (ExecutingTx) |
| T11 / T12 / T13 | tx: enabled / initiator_valid / count arm off | killed (ChecksHold...: policy disabled after submission; initiator's grant revoked; no approvals) |
| T14 / T15 | tx: count `<` -> `<=`; `< required - 1` | killed (every governed execution; ChecksHold...) |
| T16 / T17 | tx trigger: WHEN removed; WHEN on another type | killed |
| T18 | tx trigger: DEFERRABLE INITIALLY DEFERRED | killed (every governed execution: at commit the request is no longer executing) |
| T19 | tx trigger: BEFORE INSERT | killed (idempotent replay of the governed key; ExecutedRequestCannotPostAgain) |
| T20 | tx trigger: not created | killed |
| E01 | entry: parent not-found arm off | **survived - equivalent and unreachable**: the composite FK `(ledger_transaction_id, tenant_id)` forces the parent into the entry's tenant, and the INSERT and SELECT policies of both ledger tables are identical, so a session that may insert the entry sees the parent; were it ever invisible, the NULL-filled record skips the type gate and the request lookup fails closed (MA051) anyway |
| E02 / E03 | entry: type gate always / never returns | killed |
| E04 / E05 / E06 / E07 | entry: request key / state / txid condition off; request not-found arm off | killed (ExecutingTx entry-fence-alone probes; ChecksHold...) |
| E08 | entry: request tenant condition off | **survived - equivalent**: request ids are unique and RLS only shows a session requests of the tenant whose entries it may insert |
| E09 | entry: account not-found arm off | **survived - equivalent**: a NULL account makes the shape test refuse anyway; unreachable (composite account FK + the 0048 populate trigger) |
| E10..E17 | entry: asset / amount / player wallet / player direction / house wallet NULL / house direction / house type / player type arm off | killed (ExecutingTx, one probe each) |
| E18 | entry: COALESCE(..., false) removed | **survived - equivalent**: the shape expression is never NULL (account type NOT NULL, `player_cash` has a wallet by CHECK, `IS NULL` is never NULL, every request field NOT NULL) |
| E19 / E20 | entry: duplicate-leg arm off; counts both directions | killed |
| E21 / E22 / E23 | entry trigger deferred; direction mapping swapped; trigger not created | killed |
| E24 / E25 / E26 | MA052 arm off; MA052 on another account type; MA052 without the account-id condition | killed (OtherTypesCannotUseTheManualAdjustmentAccount; every governed and casino posting) |

The six survivors are kept as fail-closed defence in depth (they cost one comparison each).
