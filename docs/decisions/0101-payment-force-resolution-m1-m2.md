# ADR 0101 — Payment force-resolution M1/M2 (PRH-2 K3; amends ADR 0095 §4.8)

- **Status:** PROPOSED (W0-K draft, `architect`, 2026-09-28). NOT IMPLEMENTED. K3 may not start
  until K2 (ADR 0100) is merged and this ADR is reviewed by `security` and `ledger-finance` and
  recorded as accepted (plan §2 W4, §11).
- **Decision type:** cross-domain architecture plus financial control (`payments`, `withdrawal`
  (called, **not edited**), `internal/providerref`, migration 0114).
- **Owner:** `architect`. **`ledger-finance` owns the financial invariants** (INV-DEP-1, the M2
  postings, the LF-3 reconciliation rule). **`payments` implements.** **Reviewers:** `security`,
  `ledger-finance`, `payments`, `qa`, `code-reviewer`.
- **Registry:** HD-0095-1 (decided, ADR 0098 §1), ADR 0095 M1/M2. Workstream K3; migration 0114.
- **Binding inputs:**
  - ADR 0098 §1 and §5.
  - Plan §11.
  - `reviews/ledger-finance.md` LF-1, LF-2, LF-3, LF-15 and LF-18, plus "Required tests and
    mutants", K3.
  - `reviews/security.md` S-12, and §3 "K3".
  - `reviews/payments.md` F3.
  - `reviews/qa.md` W4 K3 items.
  - ADR 0099 (grants, the acting family, actor derivation) and ADR 0100 (the classification, the
    policy evaluation, approval counting, the lock order and Amendment A8).
- **This ADR amends ADR 0095 §4.8** (and, consequentially, §28.9 and INV-IO-7). Proposed amendment
  text is in §9. Plan Rule 2 makes the orchestrator the writer of amendment sections, so ADR 0095's
  own file is not edited here.

---

## 1. Context

ADR 0095 §4.8 defines three manual interventions:
- **M3** (abandon a never-sent payout) is built.
- **M1** (resolve a `disputed` deposit) was BLOCKED on LEDGER-MANUAL-ADJ-4EYES-1.
- **M2** (force-resolve an `ambiguous` or `disputed` payout) was BLOCKED on HD-0095-1.

Both are now decided (ADR 0098), and the design pieces exist in ADRs 0099/0100.

**Facts at `cabca27`:**

| Fact | Where |
|---|---|
| There is no `failed` attempt state | `0101:94-97` |
| There is no transition out of `disputed` | `0107` guard whitelist, `payment_attempts_guard()` |
| `→ succeeded` requires `last_evidence_kind ∈ {sync, callback, query_status}` | `0107` guard |
| A payout `→ declined` requires the same evidence kinds; a deposit `→ declined` never `operator` | `0107` guard |
| `operator` already exists as an evidence-kind value | `0101:98`, ADR 0095 §13 |
| The 0107 ledger backstop covers only `transaction_type = 'deposit'` | `0107:84-86` |
| `withdrawal.Complete` needs a non-empty `providerTxID`, which is Step B's idempotency key; `Complete` and `Fail` both require `submitted` | `withdrawal.go:1408-1419`, `:1498-1510` |
| `withdrawal.LockSubmittedForResolution` locks the request and requires `submitted` | `withdrawal.go:1139` |
| `providerref.Validate` refuses only empty, over-long, invalid-UTF-8 and control-character values. It reserves no prefix today. | `providerref.go:88-146` |
| `pay_captured_unposted` "clears … when M1/allocation occurs (BLOCKED)" | ADR 0095 §28.9 |

## 2. Decision summary

1. **Deposits never leave `disputed`** (LF-1). M1 is an evidence-only resolution record on a
   still-disputed deposit attempt. There is no state change, no posting of any kind, and no link to
   a ledger transaction (LF-2).
2. **M1 never clears or suppresses `pay_captured_unposted`** (LF-3). It only annotates the finding as
   acknowledged.
3. **M2, payouts only.** `{ambiguous, disputed} → succeeded` ("declare paid") or `→ declined`
   ("declare not paid") are allowed only:
   - with `last_evidence_kind = 'operator'`;
   - with an executed, four-eyes-approved resolution for that attempt and that target state **in
     the same transaction**;
   - when the withdrawal is `submitted`.

   "Declare paid" completes the withdrawal under a **reserved provider-tx namespace** (LF-15).
4. Both are governed by the `payment_force_resolve` capability (ADR 0099), the mandatory four-eyes
   class and policy (ADR 0100 §2–§3), and the S-12 beneficiary guard
   `payment_manual_resolutions_beneficiary_guard`.
5. Migration 0114 amends `payment_attempts_guard()` for **payout attempts only**. The deposit
   branches and both 0107 indexes stay byte-identical, with no state-CHECK change. The down migration
   restores the 0107 guard verbatim and refuses while resolutions exist.

## 3. M1 — disputed deposit: evidence only (LF-1, LF-2)

- **Subject.** A deposit attempt in `disputed`, with any `terminal_reason`
  (`multiple_success_for_intent`, `reversal_tombstone_precedes_success`, a mismatch reason from
  T10, …).
- **What it records** (`kind = 'm1_deposit_evidence'`):
  - a closed `finding_code`. Proposed codes, a **RECOMMENDATION** for `ledger-finance` and
    `payments` to confirm:
    - `awaiting_psp_refund`
    - `refund_requested_from_psp`
    - `investigated_no_platform_action`
    - `pending_suspense_allocation_b` (a placeholder for LEDGER-SUSPENSE-B-1; it posts nothing)
  - an `evidence_ref_hash`. Documents live outside the DB and only their hash is pinned. There is no
    PII.
  - the reason code and approvals.
- **What it never does:**
  - It never changes `payment_attempts` or `deposit_intents`. The attempt stays `disputed`.
  - It never posts, and never links a ledger transaction: CHECK `kind = 'm1_deposit_evidence' ⇒
    ledger_transaction_id IS NULL AND target_state IS NULL`.
  - The K3 executor refuses any request to attach a K2 adjustment to a deposit resolution. ADR 0100's
    request has no column that could reference an attempt or intent.
- **Where funds go.** Funds held at the PSP for a financially resolved intent leave **only** by:
  - a PSP refund (reversal → tombstone; the existing path);
  - **or**, later, LEDGER-SUSPENSE-B-1 (HD-LEDGER-UNALLOC-1 "B later"; deferred and separately
    authorized).
- **Out of scope.** An M1 *credit* for an **unresolved** intent. If it is ever built, it goes through
  `postDepositSuccess` as a `deposit` posting under INV-DEP-1, never as a manual adjustment.
- **Governance.** It follows the same request/approval/execution path as M2 (§6). "Execution" of an
  M1 is only `pending → executed` of the resolution row, plus the audit record. M1 has no amount, so
  policy evaluation uses `base_required_approvals` only (ADR 0100 §3.2).

## 4. LF-3 — M1 and reconciliation

- `pay_captured_unposted` keeps being emitted on **every** run for a
  `disputed` / `multiple_success_for_intent` attempt until a reversal or tombstone appears (the PSP
  refunded), or until a future (B) allocation. Ageing, amount and asset reporting are unchanged.
- The M1 executor **writes nothing to any reconciliation table**. It never sets
  `investigation_status = 'resolved'` on a `pay_captured_unposted` mismatch.
- "Acknowledged" is derived at read time. The reconciliation read model and back office join the
  finding to the executed M1 resolution for that attempt and display
  `acknowledged_by_resolution_id`, the finding code and the approvers. The emission predicate
  (ADR 0095 §28.9) does not reference resolutions at all.
- **Test:** an M1 on a `multiple_success_for_intent` attempt leaves `pay_captured_unposted` reported
  on the next run, with the same ageing, and posts nothing.

## 5. M2 — payout force-resolution (LF-1(b), LF-15)

### 5.1 Preconditions (checked by the executor and by trigger)

- `payment_attempts.operation = 'payout'` and `state ∈ {ambiguous, disputed}`.
- The parent withdrawal is `submitted`. `withdrawal.LockSubmittedForResolution` refuses otherwise,
  and the 0114 guard re-checks it. **This excludes T14 disputes** (`declined → disputed`), whose
  withdrawal has already failed (LF-15(2)).
- `attempt.provider_id IS NOT NULL`.
- A closed `basis_code` records why provider evidence cannot resolve it (ADR 0095 §4.8 "when it is
  the only option"). Proposed codes, a **RECOMMENDATION** for `payments`:
  - `provider_unqueryable`
  - `past_resubmission_horizon`
  - `provider_confirmed_out_of_band`
  - `reconciliation_exhausted`

  The runbook requires an operator T17 re-verify (ADR 0095 §12.5) to be attempted first. Its
  outcome goes into the evidence reference.
- `kind ∈ {m2_declare_paid, m2_declare_not_paid}`, with `target_state` `succeeded` or `declined`
  respectively (CHECK).

### 5.2 Transitions admitted by the 0114 guard (payout only)

| From | To | Extra conditions (all in the trigger) |
|---|---|---|
| `ambiguous` | `succeeded` | `OLD.operation = 'payout'` |
| `disputed` | `succeeded` | `OLD.operation = 'payout'` |
| `ambiguous` | `declined` | `OLD.operation = 'payout'` |
| `disputed` | `declined` | `OLD.operation = 'payout'` |

For all four rows, additionally:
- `NEW.last_evidence_kind = 'operator'`;
- a `payment_manual_resolutions` row exists with `attempt_id = OLD.id`, `state = 'executing'`,
  `executed_txid = txid_current()`, `target_state = NEW.state`, and the M2 kind matching the target;
- the parent `withdrawal_requests.state = 'submitted'`.

How this fits the existing guard:
- `ambiguous → succeeded` and `ambiguous → declined` already exist (T7/T8) for provider evidence and
  are unchanged. The `operator` evidence exception is new.
- `disputed → *` is new, and **payout-only**.
- **Every other operator-evidence terminal stays refused**, including every deposit transition. The
  existing deposit `→ declined ≠ operator` line and every deposit branch are unchanged.
- **UNIQUE per attempt:** a partial unique index on `payment_manual_resolutions (attempt_id) WHERE
  state = 'executed'`, and a second one `WHERE state = 'pending'` (one open request at a time).

### 5.3 "Declare paid" (`m2_declare_paid`)

In the execution transaction (§6.3):
1. Move the attempt to `succeeded` (`last_evidence_kind = 'operator'`).
2. Call `withdrawal.Complete(ctx, tx, wr.ID, attempt.provider_id, reservedTxID)`, where:
   - `reservedTxID = "platform-operator-declared:" || resolution_id`;
   - the exact prefix is fixed by K3 and recorded in `providerref`.
3. Step B posts `withdrawal_completed` with `(provider_id, reservedTxID)` as its idempotency key.

**Why a reserved namespace (LF-15(1)):**
- Step B's key must not collide with any real provider reference, now or later.
- K3 therefore extends `providerref.Validate` to **refuse** any real reference beginning with the
  reserved prefix (a new reason, `ReasonReservedNamespace`). Every ingress of a real reference (sync
  results, callbacks, poll results, statement lines) is validated, so a provider can never present
  one.
- A DB CHECK on `payment_attempts.provider_reference` and `payment_provider_events`' reference
  column refuses the prefix too. A trigger on `ledger_transactions` admits a `provider_tx_id` with
  the prefix only for `withdrawal_completed` under an executing M2 resolution.
- **0114 up refuses** if any existing `provider_reference` or `provider_tx_id` already carries the
  prefix.

**The late real callback or poll success** after "declare paid":
- The attempt is already `succeeded` and the withdrawal `completed`.
- The existing already-succeeded evidence branch records it, and `Complete` refuses
  (`state ≠ submitted`).
- There is **no second Step B**.

Reconciliation must match the real statement line to the M2-declared Step B (§14 open item).

**Risk: declared paid, but the provider never paid.** The player's held funds went to
`psp_clearing`. This is detected by payment statement reconciliation (a missing provider record),
and is corrected by an ADR 0100 `compensating_entry` credit (four-eyes). The runbook covers it.

### 5.4 "Declare not paid" (`m2_declare_not_paid`)

In the execution transaction:
1. Move the attempt to `declined` (`last_evidence_kind = 'operator'`).
2. Call `withdrawal.Fail(ctx, tx, wr.ID, "operator_declared_not_paid")`, which releases the hold
   back to `player_cash`.

**Recorded risk, the double payout (LF-15(3)).** If the provider did pay and a real success arrives
later, the platform has both released the hold and seen the payout go out. That is a **double
payout**:
- It surfaces as **T14** (`declined → disputed`, payout, P1; ADR 0095 §4.3) through the existing
  evidence path, and in statement reconciliation.
- Recovery (a debit adjustment, which ADR 0100 §6.4 may refuse on insufficient funds, or an
  off-platform recovery) is an operational and legal process. It is not automated.
- This risk is why M2 is four-eyes, needs a `basis_code`, and why the runbook requires T17 first.

**INV-IO-7 is amended accordingly** (§9): "declare not paid" is the single governed exception to
"no payout failure without definite decline evidence", admitted only with an executed four-eyes M2
resolution in the same transaction.

## 6. Governance (reusing ADRs 0099 and 0100)

### 6.1 Capabilities and policy

- **Capabilities:** the requester holds `payment_force_resolve:request`; each approver holds
  `payment_force_resolve:approve`. Both are grants for the attempt's tenant, re-checked in the
  transaction and at execution (ADR 0099 §7).
- **Classification:** `payment_force_resolve` is `mandatory_four_eyes` (ADR 0100 §2).
- **Policy evaluation:** `financial_policy_required_approvals('payment_force_resolve', tenant,
  brand, asset, amount, as_of)`. M2 uses the attempt's amount and asset. M1 uses no amount (base
  only). Required = MAX(at submission, at execution). **No in-force platform baseline ⇒ disabled.**
- **Approval counting:** exactly ADR 0100 §6.2. The payload hash covers `attempt_id`, `kind`,
  `target_state`, `finding_code` or `basis_code`, `evidence_ref_hash`, amount, asset and reason
  code. The payload is immutable after submission.
- **Distinct-Person floor:** non-configurable (LF-11), as in ADR 0100 §6.3. The ADR 0099 §9 S-1
  caveat applies.

### 6.2 Beneficiary exclusion (S-12): trigger `payment_manual_resolutions_beneficiary_guard`

- It runs `BEFORE INSERT` on `payment_manual_resolutions` **and** on
  `payment_manual_resolution_approvals`, and is re-checked at execution.
- It resolves the **attempt owner's Person**:
  - **M1:** `deposit_intents.player_account_id` → `player_accounts.person_id`;
  - **M2:** `withdrawal_requests.player_account_id` → `player_accounts.person_id`.
- It refuses if the requester's or the approver's `staff_users.person_id` equals it.
- A staff actor with a NULL `person_id` is refused (not skipped; ADR 0100 §6.3).
- The attempt owner's Person is NOT NULL (`0010:15`). If the lookup returns no row (for example, not
  visible under the session), the trigger refuses: fail closed.
- For M2 this is **on top of** the withdrawal state machine's own guards (security §3 K3).

### 6.3 Execution in the final approval's transaction

The order follows ADR 0082 A7 plus A8 (ADR 0100 §7):
1. Lock the parent: for M2, `withdrawal.LockSubmittedForResolution` (L1, which refuses if not
   `submitted`); for M1, the `deposit_intents` row `FOR UPDATE`.
2. Lock the `payment_attempts` row (L1, after its parent).
3. Lock the `payment_manual_resolutions` row (L1, after its attempt). Refuse unless `pending` and
   unexpired.
4. Insert the approval (triggers: actor, grants, distinct Person, beneficiary, payload hash).
5. Evaluate `required` and `counted`. If `counted < required`, commit and stay `pending`.
6. Set the resolution to `executing` with `executed_txid = txid_current()`.
7. M2 only: update the attempt (§5.2) → `withdrawal.Complete` or `Fail` (L3/L4 inside
   `ledger.Post`) → record `ledger_transaction_id` on the resolution.
8. Set the resolution to `executed`, audit, and commit.

Rules:
- **"Same transaction" is `executed_txid` / `decided_txid = txid_current()`, never `xmin`** (LF-10).
- A DEFERRABLE INITIALLY DEFERRED constraint trigger refuses commit if any resolution is still
  `executing`. For an executed M2, it also refuses unless:
  - the attempt is in `target_state`;
  - the withdrawal is `completed` (declare paid) or `failed` (declare not paid);
  - `ledger_transaction_id` references a same-tenant `withdrawal_completed` transaction keyed
    `(provider_id, reservedTxID)`, or a `withdrawal_failed` transaction with
    `correlation_id = wr.id`.
- **Concurrency with the sweeper or a callback:**
  - Both take the same L1 parent → attempt locks. Whoever commits first wins.
  - If the provider's success lands first, the attempt is `succeeded`, and the M2 precondition
    (`state ∈ {ambiguous, disputed}`) fails. The resolution stays `pending` and is later cancelled
    or expired.
  - If M2 lands first, the later evidence takes the existing already-terminal branch (§5.3 and
    §5.4).
- **Platform principals** act only through an ADR 0099 §6 acting session with the specific grant.
  0114 adds the acting-family policies on `payment_attempts`, `deposit_intents`,
  `withdrawal_requests` and every further table `withdrawal.Complete`/`Fail` touch, plus the
  `withdrawal_*` branch of the ADR 0099 §6.5 ledger fence (`correlation_id = wr.id` and an executing
  M2 resolution for that withdrawal).

## 7. What K3 touches

- `migrations/0114_*`.
- `internal/payments/attempt.go` and a new `internal/payments/manual_resolution.go`.
- **`internal/providerref/providerref.go`** for the reserved-namespace refusal. It is **not** in plan
  §3's K3 row; the orchestrator adds it (Rule 1).
- A new route file.

**No edits to `withdrawal.go`** (payments F3). K3 uses only `LockSubmittedForResolution`, `Complete`
and `Fail`, which already exist.

## 8. Migration 0114 (K3) — content

Common rules:
- Every new table: `ENABLE` **and `FORCE ROW LEVEL SECURITY`**; no `FOR ALL`; `BEFORE TRUNCATE`
  deny; DELETE refused; no `SECURITY DEFINER`; SQLSTATE class **`MR`**.
- **RLS family for the new tables: tenant plus acting** (ADR 0099 §5, §6.3). **No plain platform
  family** (HD-PRH2-6).

| Object | Content |
|---|---|
| `payment_manual_resolutions` | `id`, `tenant_id`, `attempt_id` (composite FK to `payment_attempts (tenant_id, id)`), `operation` (derived), `kind`, `target_state`, `finding_code`, `basis_code`, `evidence_ref_hash`, `amount` and `asset_code` (copied from the attempt), `withdrawal_request_id` / `deposit_intent_id` (derived), `reason_code`, `payload_hash` (DB-computed), `requested_by`/`_scope`/`_person_id` (forced), `required_at_submission`, `contributing_policy_ids`, `state`, `expires_at`, `executed_txid`, `ledger_transaction_id` (M2 only, UNIQUE, composite FK `ledger_transactions_id_tenant_key`), `reserved_provider_tx_id` (M2 paid only). CHECKs: kind ↔ operation ↔ target_state; M1 ⇒ no ledger link. Immutable payload; state-transition triggers; the two partial UNIQUE indexes (§5.2); the deferred executed/executing check (§6.3); **`payment_manual_resolutions_beneficiary_guard`** (§6.2). |
| `payment_manual_resolution_approvals` | `payload_hash`, `decision`, `decided_by`/`_scope`/`_person_id` (forced), `decided_txid`, `reason_code`. Immutable. Distinct-Person, grant and beneficiary triggers (the same guard function is attached here). |
| `payment_manual_resolution_codes` | Reference table for `finding_code` and `basis_code` (§3, §5.1). Written by migration only. **Reference RLS family.** |
| **`payment_attempts_guard()`** | `CREATE OR REPLACE`. **The 0107 body verbatim**, plus only the payout-only additions in §5.2: the two new `disputed → {succeeded, declined}` whitelist pairs, and the `operator`-evidence exception on the two evidence-kind checks, each gated on `OLD.operation = 'payout'` plus the resolution/`txid`/withdrawal predicate. **The deposit branches and the `ledger_transactions_one_deposit_per_intent` and `payment_attempts_one_succeeded_deposit_per_intent` indexes stay byte-identical. No state-CHECK change** (LF-1). |
| Reserved namespace | A CHECK on the provider-reference columns (§5.3); a `ledger_transactions` insert trigger branch; an up-time refusal if the prefix already exists. |
| Acting-family policies | As listed in §6.3, and the `withdrawal_*` branch of the ADR 0099 §6.5 fence. |
| **Down** | **Refuse while any `payment_manual_resolutions` row exists.** Otherwise restore **the 0107 `payment_attempts_guard()` body verbatim**, drop the added policies, CHECKs, triggers and tables, and remove the fence branch. The `providerref` Go change is reverted with the code. |

**Guard tests run on a HEAD-migrated scratch database** (LF-18; the 0107 project rule, ADR 0095
§28.8(3)). They never run on a database migrated only to 0114's predecessor.

## 9. Proposed amendment text for ADR 0095 (for the orchestrator to apply)

**§4.8 table: replace the M1 and M2 rows.**

| M | What | Governance | Status |
|---|---|---|---|
| M1 | Record an evidence-only resolution on a `disputed` **deposit** attempt. The attempt stays `disputed`. There is no posting and no ledger link (LF-1/LF-2). Funds leave only via a PSP refund or LEDGER-SUSPENSE-B-1 later. | ADR 0101 §3, §6: `payment_force_resolve` grants (ADR 0099), mandatory four-eyes (ADR 0100), beneficiary guard | DESIGNED (ADR 0101); NOT IMPLEMENTED until K3 |
| M2 | Force-resolve an `ambiguous`/`disputed` **payout** whose withdrawal is `submitted`: declare paid → `succeeded` + `Complete` under the reserved provider-tx namespace; declare not paid → `declined` + `Fail`. `last_evidence_kind = 'operator'`, with an executed resolution in the same tx. | As M1 | DESIGNED (ADR 0101); NOT IMPLEMENTED until K3 |

**§28.9: amend the clearing sentence.** Replace "or when M1/allocation occurs (BLOCKED)" with: "or,
under HD-LEDGER-UNALLOC-1 (B), when an allocation posting exists. **An M1 resolution never clears
or suppresses the finding. It only annotates it as acknowledged** (ADR 0101 §4, LF-3)."

**§2 INV-IO-7: append.** "Exception: an M2 'declare not paid' (ADR 0101 §5.4), admitted by the
guard only with `last_evidence_kind = 'operator'` and an executed four-eyes resolution in the same
transaction. Its late-success double-payout risk surfaces as T14 (P1)."

**§4.3:** add rows **M2p** (`{ambiguous, disputed}` → `succeeded`, payout, operator) and **M2n**
(`{ambiguous, disputed}` → `declined`, payout, operator), each pointing to ADR 0101 §5.2.

## 10. Audit

Every M1/M2 submission, approval, rejection, cancellation, expiry and execution writes `audit_log`
in the same transaction. Each record carries:
- actor, actor scope, and acting tenant;
- attempt, withdrawal or intent ids; kind; target state; finding or basis code;
  `evidence_ref_hash`;
- before and after state of the attempt and withdrawal;
- `ledger_transaction_id` for M2;
- IP, user agent and request id.

Events: `payment.manual_resolution_requested` / `_approved` / `_executed`, plus the existing
`payment.attempt_succeeded` / `_declined` and `withdrawal.completed` / `.failed` with
`evidence_kind = operator`. ADR 0099 §11 applies to platform actors and tenant presentation.

## 11. Tests and mutants (K3 DoD; the LF K3 list and QA W4 are incorporated by reference)

Notes:
- T-1 clock for expiry.
- No new wall-clock assertion.
- Guard tests on a HEAD-migrated scratch DB (LF-18).
- Results are reported PASS / FAIL / FLAKE / NOT RUN / BLOCKED. Local runs are never labelled CI.

| ID | Class | Test |
|---|---|---|
| C-1 | ADV | A deposit `disputed` → any state is refused **even with an executed resolution in the same tx**, for every `terminal_reason` |
| C-2 | R | M1 on `multiple_success_for_intent`: no posting; the attempt is still `disputed`; `pay_captured_unposted` is reported on the next run with unchanged ageing; the read model shows it acknowledged (LF-3) |
| C-3 | ADV | Linking a K2 adjustment to a deposit resolution, or setting `ledger_transaction_id` on an M1: refused |
| C-4 | R | M2 declare paid and declare not paid, from `ambiguous` and from `disputed`, only when the withdrawal is `submitted`. The postings are correct. `SUM(D) = SUM(C)`; the projection equals the recomputed value. |
| C-5 | ADV | M2 on a T14 dispute (withdrawal `failed`): refused. M2 on a deposit: refused. |
| C-6 | ADV | A resolution for attempt X used on Y; a target-state mismatch; a reused executed resolution; a resolution from another tx (`executed_txid ≠ txid_current()`); `operator` evidence with no resolution. All refused by the guard. |
| C-7 | R | A late real success after "declare not paid" → T14 (`declined → disputed`), P1 (the double-payout record) |
| C-8 | R | A late callback or poll success after "declare paid" → no second Step B; the withdrawal stays `completed`; ledger idempotency holds |
| C-9 | ADV | Reserved namespace: `providerref.Validate` refuses the prefix; a real callback carrying it is refused at ingress; the DB CHECK refuses it on `provider_reference`; 0114 up refuses while a prefixed value exists |
| C-10 | AZ | **S-12:** a requester or approver who is the attempt owner's Person is refused for **M1 (deposit owner) and M2 (payout owner)**; an unlinked staff Person is refused |
| C-11 | AZ | Self-approval; the same Person under two principals; no grant; a revoked or expired grant at execution; a suspended actor with a live token. All refused or not counted. |
| C-12 | CON | M2 racing a sweeper or callback success on the same attempt: exactly one terminal outcome and at most one Step B. Two concurrent final approvals give one execution. `-race`, with order-based assertions. |
| C-13 | FL/RB | A failure inside `Complete`/`Fail` rolls back the attempt update, the resolution state and the approval. No partial rows (QA W4). The deferred check refuses a committed `executing`. |
| C-14 | RLS | Tenant B cannot see or act on A's resolutions. A plain platform session is refused. An acting session works only with the specific grant for that tenant. |
| C-15 | AU | One audit row per transition, with the correct actor and scope |
| C-16 | MIG | 0114 up/down/up on a HEAD-migrated DB. Down refuses while resolutions exist. After down, `pg_get_functiondef(payment_attempts_guard)` equals 0107's, and both 0107 indexes are unchanged. |
| C-17 | R | A text diff between 0114's guard body and 0107's shows only the §5.2 payout additions (the byte-identical deposit branches) |
| C-18 | R | No in-force platform `payment_force_resolve` policy → M1 and M2 are disabled |

**Mutants (each must be killed):**

| Mutant | Killed by |
|---|---|
| drop the `executed_txid = txid_current()` predicate | C-6 |
| drop the attempt binding (`attempt_id = OLD.id`) | C-6 |
| allow deposit `disputed →` any state | C-1 |
| drop the `target_state = NEW.state` match | C-6 |
| drop the withdrawal `submitted` re-check | C-5 |
| drop the beneficiary guard for M1, or for M2 | C-10 |
| make M1 clear or suppress `pay_captured_unposted` | C-2 |
| accept a reserved-prefix real reference | C-9 |
| drop the deferred `executing` check | C-13 |

## 12. Invariants preserved

- **INV-DEP-1:** preserved. There is no deposit transition out of `disputed` and no deposit posting
  from K3. Both 0107 indexes and every deposit guard branch are byte-identical (C-1, C-16, C-17).
- **Append-only double-entry:** M2 posts only through `withdrawal.Complete`/`Fail` → `ledger.Post`.
  M1 posts nothing. There are no edits or deletes.
- **DB idempotency:**
  - Step B is keyed `(provider_id, reservedTxID)`, and the prefix cannot collide with a real
    reference;
  - the `Fail` posting keeps its existing key;
  - the partial UNIQUE indexes give one executed and one pending resolution per attempt;
  - `ledger_transaction_id` is UNIQUE.
- **RLS:** FORCE RLS; tenant plus acting families only; no plain platform family.
- **No direct balance mutation:** the hold is released or consumed only by the withdrawal postings.
- **HD-LEDGER-UNALLOC-1 "A now, B later":** A is unchanged. A second capture stays `disputed`,
  uncredited and reported. M1 adds an annotation only. B stays deferred (LEDGER-SUSPENSE-B-1).
- **INV-IO-7:** amended with a single, governed, same-transaction exception (§9).
- **INV-IO-9 / M3:** unchanged.

## 13. Alternatives rejected

| Alternative | Why rejected |
|---|---|
| A `resolved_no_action` deposit state, or `disputed → declined` for deposits | LF-1: a state-CHECK change and a deposit guard change, and it weakens INV-DEP-1's structure |
| An M1 credit through a K2 adjustment | LF-2: the 0107 backstop covers only `deposit`, so a double credit would pass every backstop |
| M1 setting `investigation_status = 'resolved'` | LF-3: it suppresses ageing |
| "Declare paid" using the attempt's real `provider_reference`, or an empty key | May be absent (ambiguous). Could collide with a later real reference. An empty key is refused (plan D, LF-4). |
| M2 on T14 disputes | The withdrawal has already failed. Resolving would need `withdrawal.Reverse` semantics that do not exist (LF-15). |
| Approve now, execute later | An approved-but-unexecuted window (LF-13 pattern) |
| Editing `withdrawal.go` to add an M2-specific transition | Payments F3. The existing `LockSubmittedForResolution`/`Complete`/`Fail` suffice. |

## 14. Open items

1. **Code catalogues:** the `finding_code` and `basis_code` content (§3, §5.1) is a
   RECOMMENDATION. `payments` and `ledger-finance` confirm.
2. **Reconciliation matching for M2-declared payouts.** The statement import must map a real
   provider line to a Step B keyed in the reserved namespace (by `withdrawal_request_id` /
   `merchant_reference`), so a genuine later line is not mis-reported as `pay_missing_platform_record`
   or `pay_reference_mismatch`. `payments` and `ledger-finance` specify this in K3.
3. **Late provider *decline* after "declare paid".** Confirm that the ADR 0095 §4.4 matrix records
   it without a state change and raises a P1. `payments` checks it in K3 and adds a test.
4. **Non-active tenants.** In-flight payouts of a suspended tenant must still be resolvable (the H
   ruling, security addendum §2). M1/M2 are resolution actions and are **not** blocked by tenant
   status. `security` confirms.
5. **The exact reserved prefix** (§5.3) is fixed by K3 and recorded in `providerref` and in ADR 0095
   §9.
6. **K3 Touches addition:** `internal/providerref/providerref.go` (§7). The orchestrator records it.
7. **ADR 0095 amendments** (§9) and ADR 0082 A8 (ADR 0100 §7) are applied by the orchestrator.

**HUMAN DECISION REQUIRED:** none new here. ADR 0100 §3.4's below-threshold question also governs
`payment_force_resolve`. Until the human answers, the interim (always at least one independent
approver) applies to M1 and M2.

## 15. Handover / DoD

K3 is not done until it updates:
- `docs/architecture/withdrawal-state-machine.md` (M2 declare paid / not paid; the reserved
  namespace; the double-payout risk);
- `docs/architecture/payment-orchestration.md` (M1/M2 flows; `operator` evidence);
- `docs/architecture/reconciliation-model.md` (LF-3: M1 acknowledgement is a read-time
  annotation; `pay_captured_unposted` is never cleared by M1; matching for M2-declared payouts);
- `docs/security/security-architecture.md` (the S-12 beneficiary guard for M1/M2);
- `backoffice/src/auth/permissions.ts`;
- `docs/runbooks/operational-runbooks.md` (new entry "Payment force-resolution (M1/M2)": T17 first;
  the basis codes; the declared-paid-but-unpaid and declared-not-paid-but-paid (T14) recovery paths;
  "M1 never clears `pay_captured_unposted`"; how an M1 relates to a PSP refund);
- `docs/runbooks/observability-and-alerting.md` (T14 after M2 as a P1 Kind, with ADR 0102).

The orchestrator updates:
- ADR 0095 §4.8, §4.3, §28.9 and INV-IO-7 (§9);
- registry HD-0095-1 and the M1/M2 rows;
- the HANDOVER decisions index and the mock-vs-real matrix (M1/M2 is MOCK-provider only);
- the review records under `docs/plans/prh2-hardening-round/`.
