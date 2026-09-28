# ADR 0101 — Payment force-resolution M1/M2 (PRH-2 K3; amends ADR 0095 §4.8)

- **Status:** PROPOSED — **revision 2** (`architect`, 2026-09-28). NOT IMPLEMENTED. Revision 1
  (`d83a71c`) was reviewed ACCEPT (`product-owner-proxy`) and ACCEPT WITH CONDITIONS (`security`,
  `ledger-finance`). This revision applies every condition (§16). **`security` and `ledger-finance`
  confirm it before any K3 code.** K3 also waits for K2 to merge.
- **Decision type:** cross-domain architecture and financial control:
  - `payments`;
  - `withdrawal`, called but **not edited**;
  - `internal/providerref`;
  - `internal/reconciliation` (payment statement);
  - migration 0114.
- **Owner:** `architect`. **`ledger-finance` owns the financial invariants, and its rulings are
  binding.** **`payments` implements.** **Reviewers:** `security`, `ledger-finance`, `payments`,
  `qa`, `code-reviewer`.
- **Registry:**
  - HD-0095-1 (decided);
  - ADR 0095 M1/M2;
  - follow-ups PAYOUT-AMOUNT-DISPUTE-1 and WITHDRAWAL-REVERSAL-1.

  Workstream K3; migration 0114.
- **Binding inputs:**
  - ADR 0098 §1 and §5.
  - Plan §11 (including the `pending_suspense_allocation_b` deferral).
  - LF-1, LF-2, LF-3, LF-15, LF-18; S-12.
  - `reviews/adr-0099-0101-security.md` C-101-1..4 and ruling 5.
  - `reviews/adr-0099-0101-ledger-finance.md` F2(d), F9–F14, F16, F17 and rulings 1, 5 and 6.
  - `reviews/adr-0099-0101-product-owner-proxy.md`.
  - ADRs 0099 and 0100, revision 2.
- **This ADR amends ADR 0095 §4.8** (and §4.3, §28.9 and INV-IO-7). §10 holds the amendment text.
  The orchestrator writes it into ADR 0095.

| Rev | Base | Change |
|---|---|---|
| 1 | `d83a71c` | Initial draft |
| 2 | `6864efa` | Acting UPDATE policies gated on an executing M2 resolution and `operator` evidence (C-101-1); `evidence_ref_hash` NOT NULL for M2 (C-101-2); tenant status recorded (C-101-4); the M2 terminal-reason allow-list refusing `amount_asset_mismatch` (F9); reconciliation matching plus two standing kinds (F10, ruling 5); the `psp_clearing` residual (F11); the exact permitted guard diff and the deposit matrix test (F12); detail text (F13); ingress validation (F14); the reserved-prefix trigger for all sessions (F2(d)); binding code catalogues (ruling 1); lock order (ruling 6); full 0114 content; review disposition |

---

## 1. Context

ADR 0095 §4.8 defines three manual interventions: M1 (a disputed deposit), M2 (an ambiguous or
disputed payout) and M3 (a never-sent payout, already built). ADR 0098 decided M1 and M2 are
allowed as governed capabilities. ADRs 0099 and 0100 provide grants, policy and four-eyes.

**Facts (at `6864efa`; code unchanged since `cabca27`):**

| Fact | Where |
|---|---|
| No `failed` attempt state. The 0107 whitelist has no `disputed → *` pair. | `0101:94-97`; `0107:172-192` |
| The evidence gates: `→ succeeded` requires `sync`, `callback` or `query_status`; the same for payout `→ declined`; deposit `→ declined` never `operator` | `0107:259-268` |
| The 0107 indexes `payment_attempts_one_succeeded_deposit_per_intent` and `ledger_transactions_one_deposit_per_intent` | `0107:78-80,87-89` |
| `Complete` key = `providerID + ":" + providerTxID`; `Fail` key = `requestID + ":failed"`; both require `submitted` | `withdrawal.go:1442,1539,1417,1508` |
| `withdrawal.LockSubmittedForResolution` | `withdrawal.go:1139` |
| Payout dispute reasons written today: `amount_asset_mismatch`, `provider_reference_mismatch`, `success_for_never_sent_attempt` | `payout.go:1118,1151`; `receipt.go:750,873` |
| Reconciliation skips `disputed` attempts except `multiple_success_for_intent` | `payment_statement.go:950` |
| `capturedUnposted` clears only on a reversal or tombstone. Its detail text says "or M1/allocation (BLOCKED)". | `payment_statement.go:949,977-979,1002` |
| `providerref.Validate` (shared with casino and sportsbook) reserves no prefix. Payout sync and poll call it; **deposit sync and poll do not** (F14). | `providerref.go:88-146`; `payout.go:392-395,1178-1181` |

## 2. Decision summary

1. **Deposits never leave `disputed`.** M1 is evidence-only, with no posting and no ledger link
   (LF-1, LF-2).
2. **M1 never clears or suppresses `pay_captured_unposted`** (LF-3).
3. **M2 is payouts only.**
   - Transitions: `{ambiguous, disputed}` → `succeeded` ("declare paid") or → `declined` ("declare
     not paid").
   - Only with `operator` evidence, an executed four-eyes resolution in the same transaction, a
     withdrawal in `submitted`, and an **allow-listed** dispute reason. `amount_asset_mismatch` is
     refused.
   - "Declare paid" uses the reserved provider-tx namespace **`platform-operator-declared:`**.
4. **Acting sessions** (ADR 0099) can update attempts and withdrawals only with an executing M2
   resolution, and for attempts only with `operator` evidence (C-101-1).
5. **Two new standing reconciliation kinds** keep every M2 risk visible:
   `pay_declared_paid_unconfirmed` and `pay_declared_not_paid_but_paid`.
6. **0114** changes the 0107 guard by exactly the diff in §8.3. The down migration restores the
   0107 guard verbatim and refuses while resolutions exist.

## 3. M1: disputed deposit, evidence only (LF-1, LF-2)

- **Subject.** A deposit attempt in `disputed`, with any `terminal_reason`.
- **What it records.**
  - `kind = 'm1_deposit_evidence'`.
  - A **`finding_code`** (LF ruling 1, binding): `awaiting_psp_refund`,
    `refund_requested_from_psp` or `investigated_no_platform_action`.
    **`pending_suspense_allocation_b` is not seeded** (`product-owner-proxy`, plan §11). It is added
    with LEDGER-SUSPENSE-B-1 when that is authorized.
  - `evidence_ref_hash` (optional for M1). No PII.
  - Reason code and approvals.
- **What it never does.**
  - It never changes `payment_attempts` or `deposit_intents` (the acting UPDATE policy on
    `deposit_intents` is WITH CHECK `false`).
  - It never posts. CHECK `kind = 'm1_deposit_evidence' ⇒ ledger_transaction_id IS NULL AND
    target_state IS NULL`.
  - The executor refuses any attachment of a K2 adjustment.
- **Where funds go.** Funds for a financially resolved intent leave only by a PSP refund
  (reversal → tombstone), or by LEDGER-SUSPENSE-B-1 later.
- **Out of scope.** An M1 credit for an unresolved intent. If it is ever built, it goes through
  `postDepositSuccess`.
- **Governance.** As §6. With no amount, only the policy base applies (ADR 0100 §3.2).

## 4. LF-3: M1 and reconciliation (F13)

- `pay_captured_unposted` keeps being emitted every run, unwindowed, until a reversal or tombstone
  exists (or a future (B) allocation). M1 writes nothing to reconciliation tables and never sets
  `investigation_status`.
- "Acknowledged" is a read-time join to the executed M1 resolution. The emission predicate does not
  reference resolutions.
- **F13:** the detail strings at `payment_statement.go:949` and `:1002`, and the matching file comment at `:146`, change to:

  > `resolution: a PSP-initiated reversal/tombstone, or allocation (LEDGER-SUSPENSE-B-1); M1 only acknowledges`

  Test C-19 asserts that no detail string says M1 clears.

## 5. M2: payout force-resolution

### 5.1 Preconditions (executor **and** guard/trigger)

| Precondition | Detail |
|---|---|
| Operation and state | `operation = 'payout'`; `state ∈ {ambiguous, disputed}` |
| **Allow-list (F9)** | `state = 'ambiguous'`, **or** `state = 'disputed' AND terminal_reason IN ('provider_reference_mismatch', 'success_for_never_sent_attempt')`. **`amount_asset_mismatch` is refused for both M2 kinds** (`MR010`), as is any other reason. Provider evidence contradicts the amount, so M2 would post or release the wrong value. The amount-dispute path is **PAYOUT-AMOUNT-DISPUTE-1**. |
| Withdrawal | `submitted`. `LockSubmittedForResolution` refuses otherwise, and the guard re-checks it. This excludes T14 disputes (LF-15). |
| Provider | `attempt.provider_id IS NOT NULL` |
| **Basis (LF ruling 1)** | `basis_code IN ('provider_confirmed_out_of_band', 'reconciliation_exhausted')` for both M2 kinds (CHECK). Meaning: for `m2_declare_paid`, `reconciliation_exhausted` means a statement success line exists; for `m2_declare_not_paid`, it means statement coverage extends past the attempt with no line. `context_code` is NULL or in `('provider_unqueryable', 'past_resubmission_horizon')`, and is **secondary only**. The vocabulary is `payments`' to confirm. |
| **Evidence (C-101-2)** | `evidence_ref_hash` NOT NULL for M2 (CHECK). The runbook requires an operator T17 re-verify first, and its outcome is referenced. |
| Kind | `kind ∈ {m2_declare_paid, m2_declare_not_paid}`, with `target_state` `succeeded` / `declined` (CHECK) |
| **Tenant status (C-101-4; security ruling 5)** | M1 and M2 are **available for non-active tenants**. Tenant status is read in-tx and recorded at submission and at execution. Policy evaluation for a non-active tenant ignores tenant and brand rows (ADR 0100 §6.7). |

### 5.2 Transitions admitted (payout only)

All transitions: `ambiguous` or `disputed` → `succeeded` or `declined`. Each is admitted only if
`payment_m2_admits(...)` (§8.3) is true, which requires all of:
- `NEW.last_evidence_kind = 'operator'`;
- the §5.1 allow-list;
- a resolution for `attempt_id = OLD.id` in `state = 'executing'` with
  `executed_txid = txid_current()`, `target_state = NEW.state` and the matching kind;
- the withdrawal is `submitted`.

**UNIQUE per attempt:** partial UNIQUE `(attempt_id) WHERE state = 'executed'`, and
`(attempt_id) WHERE state = 'pending'`.

### 5.3 "Declare paid" and "declare not paid"

| | Declare paid | Declare not paid |
|---|---|---|
| Attempt | → `succeeded` (operator) | → `declined` (operator) |
| Withdrawal | `withdrawal.Complete(ctx, tx, wr.ID, attempt.provider_id, reservedTxID)` | `withdrawal.Fail(ctx, tx, wr.ID, "operator_declared_not_paid")` |
| Posting | `withdrawal_completed`, key `provider_id:reservedTxID`, where `reservedTxID = 'platform-operator-declared:' || resolution_id` | `withdrawal_failed`, key `wr.id:failed`; the hold is released to `player_cash` |
| Late evidence | A real success: the already-succeeded branch records it; `Complete` refuses (`state ≠ submitted`); **no second Step B**. A real decline: recorded, no state change; K3 confirms the §4.4 matrix raises a P1 (C-20). | A real success: **T14** (`declined → disputed`, P1). This is a **double payout**. |
| Residual | Provider never paid: the player's hold went to `psp_clearing`. A `compensating_entry` credit restores the player (subject to ADR 0100 §5.4's open confirmation), **but `psp_clearing` stays misstated** (F11). It is tracked by `pay_declared_paid_unconfirmed`. The proper fix, a withdrawal reversal (`TxWithdrawalReversed` exists; the transition is not implemented), is **WITHDRAWAL-REVERSAL-1**. | Recovery is a `compensating_entry` debit with causation = the `withdrawal_failed` transaction, which ADR 0100 §6.4 may refuse for insufficient funds, or an off-platform process. It is tracked by `pay_declared_not_paid_but_paid`. |

**INV-IO-7 is amended** (§10): "declare not paid" is the single governed exception.

### 5.4 The reserved namespace (LF-15(1), F2(d), F14, C-101-3)

- **The prefix:** `payment_reserved_ref_prefix()` is an IMMUTABLE SQL function returning
  `'platform-operator-declared:'` (27 bytes). The Go constant `providerref.ReservedOperatorPrefix`
  matches, and a test pins the two equal.
- **A Go validator for payments only (F14).**
  - `providerref.ValidatePaymentReference(field, value)` = `Validate` plus refusal of the prefix
    (new reason `ReasonReservedNamespace`).
  - The shared `Validate` is unchanged, so casino and sportsbook semantics do not change.
  - It is called at **every payments ingress**:
    - deposit sync (`drive.go`, the site C adds);
    - deposit poll (`sweeper.go`, the site D adds);
    - payout sync and poll (`payout.go:392-395,1178-1181`);
    - deposit and payout callbacks (`receipt.go`, the deposit and withdrawal handlers);
    - **statement import**.
  - C and D land their reference validation before K3 (nominal order W2/W3 < W4). K3 switches those
    sites to the payments validator. Those files join K3's Touches (§7).
- **DB backstops.**
  - A CHECK `left(col, 27) <> 'platform-operator-declared:'` (via the function) on:
    - `payment_attempts.provider_reference`;
    - `payment_provider_events.provider_reference`, `.original_provider_reference`,
      `.settlement_reference`;
    - `payment_statement_lines.provider_reference`, `.original_provider_reference`,
      `.settlement_reference`.
  - A trigger **`ledger_transactions_reserved_prefix_guard`** (BEFORE INSERT, **all sessions**,
    `left()` not `LIKE`). If `left(NEW.provider_tx_id, 27) = prefix`, it requires
    `NEW.transaction_type = 'withdrawal_completed'` and ADR 0099 §6.6 predicate (b) (an executing
    `m2_declare_paid` resolution binding correlation, provider and key). Otherwise it raises
    `MR020`.
  - **0114 up refuses** if any existing value in those columns, or any `ledger_transactions.provider_tx_id`,
    already has the prefix.

## 6. Governance

### 6.1 Capabilities, policy and counting

| Aspect | Rule |
|---|---|
| Grants | `payment_force_resolve:request` (requester) and `payment_force_resolve:approve` (approvers), for the attempt's tenant (ADR 0099) |
| Classification | `mandatory_four_eyes` |
| Policy | `financial_policy_required_approvals('payment_force_resolve', …)`: M2 uses the attempt's amount and asset; M1 the base only; `GREATEST(1, …)`; no platform baseline ⇒ disabled |
| Counting | exactly ADR 0100 §6.2, including `FOR SHARE` and S-2(iii) for approvers |
| Payload hash | covers `attempt_id`, `kind`, `target_state`, `finding_code`/`basis_code`/`context_code`, `evidence_ref_hash`, amount, asset, reason code |

### 6.2 Beneficiary exclusion (S-12): trigger `payment_manual_resolutions_beneficiary_guard`

- BEFORE INSERT on `payment_manual_resolutions` **and** `payment_manual_resolution_approvals`, and
  re-checked at execution.
- The owner's Person:
  - **M1:** `deposit_intents.player_account_id → player_accounts.person_id`;
  - **M2:** `withdrawal_requests.player_account_id → player_accounts.person_id`.
- It refuses when the requester's or approver's `staff_users.person_id` equals it. A NULL staff
  Person is refused, and a missing lookup row fails closed.
- For M2 it is in addition to the withdrawal guards.

### 6.3 Execution in the final approval's transaction (ADR 0082 A8, LF ruling 6)

1. **L1 parent.** M2: `withdrawal.LockSubmittedForResolution`. M1: `deposit_intents` `FOR UPDATE`.
2. **L1 attempt:** `payment_attempts` `FOR UPDATE`.
3. **L1 resolution** `FOR UPDATE`. It must be `pending` and unexpired.
4. Insert the approval (no lock class).
5. Evaluate. If short of the requirement, commit (stays `pending`).
6. **L1 staff, then grants,** `FOR SHARE` ascending id. Recount.
7. Re-check the §5.1 preconditions and the tenant status (record it).
8. Resolution → `executing`, `executed_txid = txid_current()`.
9. **M2 only:** attempt UPDATE (§5.2) → `Complete` or `Fail` (L3/L4 inside `ledger.Post`) →
   `ledger_transaction_id`.
10. → `executed`. Audit. Commit.

- **Deferred constraint trigger.** No `executing` row at commit. For an executed M2, also:
  - attempt = `target_state`;
  - withdrawal `completed` or `failed`;
  - `ledger_transaction_id` = a same-tenant `withdrawal_completed` keyed
    `provider_id:reservedTxID`, or a `withdrawal_failed` keyed `wr.id:failed`, with
    `correlation_id = wr.id`.
- **Race with a sweeper or callback.** The same L1 parent → attempt order applies. Whoever commits
  first wins. The loser sees a state outside the preconditions (M2 stays `pending`, later
  cancelled or expired) or takes the existing already-terminal branch.

### 6.4 Acting-session UPDATE policies (C-101-1) — 0114

| Table | USING (row visibility; allows `FOR UPDATE` locking) | WITH CHECK (what an actual UPDATE may write) |
|---|---|---|
| `payment_attempts` | `tenant_id = acting_tenant AND financial_acting_session_valid()` | the same **AND** `last_evidence_kind = 'operator'` **AND** `EXISTS (SELECT 1 FROM payment_manual_resolutions m WHERE m.attempt_id = payment_attempts.id AND m.tenant_id = payment_attempts.tenant_id AND m.state = 'executing' AND m.executed_txid = txid_current() AND m.kind IN ('m2_declare_paid','m2_declare_not_paid'))` |
| `withdrawal_requests` | as above | the same **AND** `EXISTS (… m.withdrawal_request_id = withdrawal_requests.id … state = 'executing' AND executed_txid = txid_current() AND m.kind IN (M2 kinds))` |
| `deposit_intents` | as above | `false` (M1 never updates an intent) |

- Every other table `Complete`/`Fail` touch is a ledger table (ADR 0099 §6.5–§6.7 fences) or
  `ledger_accounts` (INSERT limited by account type).
- **Consequence:** an acting session **cannot** write `callback`, `sync` or `query_status` evidence,
  and cannot move an attempt or withdrawal without an executing M2 resolution. This closes K3-1.

## 7. What K3 touches

| File | Why |
|---|---|
| `migrations/0114_*` | §8 |
| `internal/payments/attempt.go`, new `internal/payments/manual_resolution.go` | §5, §6 |
| **`internal/providerref/providerref.go`** | §5.4 |
| **`internal/payments/drive.go`, `sweeper.go`, `payout.go`, `receipt.go`, the deposit and withdrawal callback handlers, and the statement import** | switch to `ValidatePaymentReference`, after C and D merge |
| **`internal/reconciliation/payment_statement.go`** | §4 F13; §9 ruling 5 |
| a new route file | |

The **bolded** rows are Touches additions the orchestrator records (Rule 1). There are **no edits
to `withdrawal.go`.**

## 8. Migration 0114 (K3) — content

Common rules: FORCE RLS on new tables; no `FOR ALL`; `BEFORE TRUNCATE` deny; DELETE refused; no
`SECURITY DEFINER`; SQLSTATE class **`MR`**. The new tables use **families T and A only; no P**
(HD-PRH2-6).

### 8.1 Reference and functions

- **`payment_manual_resolution_codes`** (family R): `code TEXT PK`, `code_type TEXT CHECK
  (code_type IN ('finding','basis','context'))`. Rows:
  - finding: `awaiting_psp_refund`, `refund_requested_from_psp`, `investigated_no_platform_action`;
  - basis: `provider_confirmed_out_of_band`, `reconciliation_exhausted`;
  - context: `provider_unqueryable`, `past_resubmission_horizon`.
- **`payment_reserved_ref_prefix()`** (IMMUTABLE).
- **`payment_m2_admits(p_attempt_id uuid, p_old_state text, p_old_reason text,
  p_withdrawal_request_id uuid, p_new_state text, p_new_evidence text) RETURNS boolean`** (STABLE):
  the §5.2 conjunction.

### 8.2 Tables

**`payment_manual_resolutions`:**

| Column | Type / constraint |
|---|---|
| `id` | `UUID PK` |
| `tenant_id` | `UUID NOT NULL` |
| `attempt_id` | composite FK `(tenant_id, attempt_id)` → `payment_attempts` |
| `operation` | derived; `CHECK IN ('deposit','payout')` |
| `kind` | `CHECK IN ('m1_deposit_evidence','m2_declare_paid','m2_declare_not_paid')` |
| `target_state` | `TEXT NULL` |
| `finding_code`, `basis_code`, `context_code` | FKs to the codes table, with type checks by trigger |
| `evidence_ref_hash` | `TEXT NULL CHECK (~ '^[0-9a-f]{64}$')` |
| `amount`, `asset_code` | copied from the attempt |
| `deposit_intent_id`, `withdrawal_request_id` | derived |
| `provider_id` | derived; M2 |
| `reserved_provider_tx_id` | `TEXT NULL`; for `m2_declare_paid` forced to `prefix || id` |
| `reason_code` | |
| `payload_hash` | DB-computed |
| `requested_by`, `requested_by_scope`, `requested_by_person_id` | forced |
| `tenant_status_at_submission`, `tenant_status_at_execution` | |
| `required_at_submission`, `contributing_policy_ids` | |
| `state` | `CHECK IN ('pending','executing','executed','rejected','cancelled','expired','refused_at_execution')` |
| `expires_at` | |
| `executed_txid` | `BIGINT NULL` |
| `ledger_transaction_id` | `UUID NULL UNIQUE`, composite FK → `ledger_transactions_id_tenant_key` |

CHECKs:
- `kind = 'm1_deposit_evidence' ⇔ operation = 'deposit'`;
- M1 ⇒ `target_state`, `ledger_transaction_id`, `basis_code` and `reserved_provider_tx_id` are
  NULL, and `finding_code` is NOT NULL;
- M2 ⇒ `target_state = CASE kind …`, `basis_code` NOT NULL, `evidence_ref_hash` NOT NULL
  (C-101-2), `finding_code` NULL;
- `(kind = 'm2_declare_paid') = (reserved_provider_tx_id IS NOT NULL)`;
- `(state = 'executed' AND operation = 'payout') = (ledger_transaction_id IS NOT NULL)` among
  executed rows.

Indexes: partial UNIQUE `(attempt_id) WHERE state = 'executed'` and `(attempt_id) WHERE state =
'pending'`.

Triggers:
- payload immutability; forced actor;
- the §5.1 allow-list (M2) and the attempt-state checks at insert;
- `payment_manual_resolutions_beneficiary_guard`;
- state machine;
- the deferred check (§6.3).

**`payment_manual_resolution_approvals`:**
- Columns: `resolution_id` (composite FK); `decision`; `payload_hash`; approver, scope and Person
  (forced); `decided_txid`; `reason_code`.
- UNIQUE `(resolution_id, decided_by)`.
- Triggers: the beneficiary guard (the same function), distinct Person, S-2(iii), grant present.
- Immutable.

### 8.3 The `payment_attempts_guard()` change: the exact permitted diff (F12)

The 0114 body equals the **0107 body verbatim**, except for exactly these edits:

**(i)** Two lines are **inserted** into the state-pair whitelist, before its closing `)`:

```sql
OR (OLD.state = 'disputed' AND NEW.state = 'succeeded' AND OLD.operation = 'payout' AND payment_m2_admits(OLD.id, OLD.state, OLD.terminal_reason, OLD.withdrawal_request_id, NEW.state, NEW.last_evidence_kind))
OR (OLD.state = 'disputed' AND NEW.state = 'declined'  AND OLD.operation = 'payout' AND payment_m2_admits(OLD.id, OLD.state, OLD.terminal_reason, OLD.withdrawal_request_id, NEW.state, NEW.last_evidence_kind))
```

The existing `(OLD.state = 'declined' AND NEW.state = 'disputed')` line keeps its trailing
comment. The inserted lines follow it with a leading `OR`.

**(ii)** The two evidence gates are **rewritten** as `<0107 predicate> AND NOT (payout AND M2)`:

```sql
IF NEW.state = 'succeeded' AND NEW.last_evidence_kind NOT IN ('sync', 'callback', 'query_status')
   AND NOT (OLD.operation = 'payout' AND payment_m2_admits(OLD.id, OLD.state, OLD.terminal_reason, OLD.withdrawal_request_id, NEW.state, NEW.last_evidence_kind)) THEN …
IF NEW.state = 'declined' AND OLD.operation = 'payout' AND NEW.last_evidence_kind NOT IN ('sync', 'callback', 'query_status')
   AND NOT (OLD.operation = 'payout' AND payment_m2_admits(OLD.id, OLD.state, OLD.terminal_reason, OLD.withdrawal_request_id, NEW.state, NEW.last_evidence_kind)) THEN …
```

**Nothing else changes:**
- the INSERT branch, the immutability checks, T5/M3/T12/N3, and T13t/T13d;
- the deposit `→ declined ≠ operator` gate;
- both 0107 indexes;
- the state CHECK.

The deposit **outcomes** are identical because every edit is conjoined with `OLD.operation =
'payout'`. The evidence gates were shared text in 0107, so "byte-identical deposit branches" is
stated as this exact diff, not as literal identity (F12).

### 8.4 Other objects

| Object | Change |
|---|---|
| The §5.4 CHECKs | on `payment_attempts`, `payment_provider_events` and `payment_statement_lines` |
| `ledger_transactions_reserved_prefix_guard` | all sessions |
| `ledger_transactions_governed_fence` | replaced with ADR 0099 §6.6 (a) + (b) + (c) |
| Acting policies | §6.4. Acting SELECT on `withdrawal_requests`; acting SELECT on `payment_statement_lines` is **not** added (not needed) |
| **Reconciliation kinds** | the `reconciliation_mismatches_mismatch_kind_check` is dropped and re-added as a strict superset of the latest definition at merge (0112's), plus **`'pay_declared_paid_unconfirmed'`** and **`'pay_declared_not_paid_but_paid'`** |
| **Up-time refusal** | if any reserved-prefix value exists (§5.4) |

### 8.5 Down

- **Refuse** (`MR099`) while any `payment_manual_resolutions` row exists, or any mismatch row of the
  two new kinds exists.
- Otherwise:
  - restore **the 0107 `payment_attempts_guard()` body verbatim**;
  - restore 0112's fence function (branch (a) only);
  - drop the reserved-prefix trigger and CHECKs, the acting policies, the new tables and the
    functions;
  - restore 0112's kind CHECK exactly.
- The `providerref` and reconciliation Go changes revert with the code.
- **Guard tests run on a HEAD-migrated scratch DB** (LF-18).

## 9. Reconciliation for M2 (F10, LF ruling 5): implemented in `payment_statement.go` by K3

| Rule | Behaviour |
|---|---|
| (a) | A reserved-prefix `withdrawal_completed` **skips the settlement-reference comparison**. Amount and asset are still compared. |
| (b) | Statement lines for an M2-declared payout resolve **by reference, then by merchant reference**. A matching `succeeded` line is a **confirmation**, counted by the metric `payment_m2_declared_paid_confirmed_total` (no tenant label); it is not a mismatch. A `declined` line → `pay_status_mismatch`. |
| (c) | **`pay_declared_paid_unconfirmed`**, standing and **unwindowed**, is raised for every executed `m2_declare_paid` until either a confirming `succeeded` line exists in any persisted import, **or** a `compensating_entry` with causation = that Step B exists. **The second clearing path is disabled** until ledger-finance resolves the ruling 1 vs 5(c) conflict (ADR 0100 §5.4); fail closed. |
| (d) | **`pay_declared_not_paid_but_paid`**, standing and **unwindowed**, is raised for every executed `m2_declare_not_paid` whose attempt reached T14 `disputed` **or** for which any persisted import has a `succeeded` line. It clears only when executed `compensating_entry` debits with causation = the `withdrawal_failed` transaction **total at least the withdrawn amount**. That is an architect tightening of "a recovery debit": a partial recovery must not clear it. Ledger-finance confirms. It **overrides the `:950` disputed exclusion** for these attempts. |
| (e) | The reserved id is never expected on a statement. A line carrying the prefix is refused at import (§5.4). |

`docs/architecture/reconciliation-model.md` gains both kinds (edited by ledger-finance).

## 10. Proposed amendment text for ADR 0095 (the orchestrator applies it)

**§4.8, replace the M1 and M2 rows:**

| M | What | Governance | Status |
|---|---|---|---|
| M1 | An evidence-only resolution on a `disputed` **deposit**. The attempt stays `disputed`. No posting, no ledger link. Funds leave only via a PSP refund, or LEDGER-SUSPENSE-B-1 later. It never clears `pay_captured_unposted`. | ADR 0101 §3, §6 (ADR 0099 grants; ADR 0100 four-eyes; beneficiary guard) | DESIGNED; NOT IMPLEMENTED until K3 |
| M2 | For an `ambiguous`, or `disputed` with `provider_reference_mismatch` / `success_for_never_sent_attempt`, **payout** whose withdrawal is `submitted`: declare paid → `succeeded` + `Complete` under `platform-operator-declared:`; declare not paid → `declined` + `Fail`. `operator` evidence; an executed resolution in the same tx. `amount_asset_mismatch` is excluded (PAYOUT-AMOUNT-DISPUTE-1). | As M1 | DESIGNED; NOT IMPLEMENTED until K3 |

**§4.3:** add **M2p** (`{ambiguous, disputed}` → `succeeded`, payout, operator) and **M2n** (→
`declined`), pointing to ADR 0101 §5.2 and §8.3.

**§28.9:** replace "or when M1/allocation occurs (BLOCKED)" with:

> "or, under HD-LEDGER-UNALLOC-1 (B), when an allocation posting exists. An M1 resolution never
> clears or suppresses the finding; it only annotates it as acknowledged (ADR 0101 §4, LF-3)."

**§2 INV-IO-7, append:**

> "Exception: M2 'declare not paid' (ADR 0101 §5.3), admitted only with `operator` evidence and an
> executed four-eyes resolution in the same transaction. Its late-success double-payout risk
> surfaces as T14 (P1) and as `pay_declared_not_paid_but_paid`."

**§12 kinds table:** add `pay_declared_paid_unconfirmed` and `pay_declared_not_paid_but_paid` (§9).

## 11. Audit

Every M1/M2 submission, approval, rejection, cancellation, expiry, refusal and execution writes
`audit_log` in the same transaction. Each record carries:
- actor, scope, acting tenant;
- ids, kind, target, and the finding, basis and context codes;
- `evidence_ref_hash`;
- tenant status;
- attempt and withdrawal before and after;
- the ledger transaction;
- IP, user agent, request id.

Events: `payment.manual_resolution_requested` / `_approved` / `_executed` / `_refused`, plus the
existing attempt and withdrawal events with `evidence_kind = operator`. Acting rows follow ADR 0099
§10.6.

## 12. Tests and mutants (K3 DoD; LF K3 list, LF rev-2 tests 9–14, QA W4)

Notes:
- **Guard tests run on a HEAD-migrated scratch DB** (LF-18).
- T-1 clock; no wall-clock assertion.
- **Every test asserts** SUM(D) = SUM(C), projection = recomputed, **M1 produces zero ledger
  transactions**, and **the 0107 index definitions are unchanged** (LF test 14).
- Results are reported PASS / FAIL / FLAKE / NOT RUN / BLOCKED.

| ID | Class | Test |
|---|---|---|
| C-1 | ADV | A deposit `disputed` → any state is refused, even with an executed resolution, for every reason |
| C-2 | R | M1 on `multiple_success_for_intent`: no posting; `pay_captured_unposted` reported next run with unchanged ageing; acknowledged in the read model |
| C-3 | ADV | K2 link to a deposit resolution, or a ledger link on M1: refused |
| C-4 | R | M2 paid and not-paid, from `ambiguous` and from allow-listed `disputed`, withdrawal `submitted`: correct postings and keys |
| C-5 | ADV | M2 on T14 (withdrawal `failed`), on a deposit, and **on `amount_asset_mismatch`** (executor and guard; LF test 9): refused. `provider_reference_mismatch`, `success_for_never_sent_attempt` and `ambiguous` are admitted. |
| C-6 | ADV | A resolution for X used on Y; a target mismatch; reuse; `executed_txid ≠ txid_current()`; operator evidence without a resolution. All refused. |
| C-7 | R | A late success after "not paid" → T14 P1, **and `pay_declared_not_paid_but_paid` standing across N runs, including runs whose coverage excludes the payout**. It clears only when recovery debits total the amount (LF test 10). |
| C-8 | R | A late success after "paid" → no second Step B. **A real matching line gives no `pay_reference_mismatch` and no missing-record finding; the confirmation metric increments** (LF test 10). |
| C-9 | ADV | **Reserved prefix at every ingress (LF test 13; C-101-3):** deposit sync, deposit poll, payout sync, payout poll, callback, **statement import**. Also the DB CHECKs, and the all-sessions ledger trigger (the prefix on any other transaction type, or without an executing resolution, in a tenant session: refused). 0114 up refuses when a prefix exists. |
| C-10 | AZ | S-12 for M1 and M2 owners; unlinked staff Person: refused |
| C-11 | AZ | Self-approval, same Person, no grant, revoked or expired at execution (`FOR SHARE`), suspended actor, policy author as approver: refused or not counted |
| C-12 | CON | M2 vs sweeper or callback success → exactly one outcome and at most one Step B. Concurrent final approvals → one execution. |
| C-13 | FL/RB | A failure in `Complete`/`Fail` rolls back everything; a committed `executing` is refused |
| C-14 | RLS | Tenant isolation; plain platform refused; acting only with the specific grant for that tenant |
| C-14b | RLS | **C-101-1:** an acting session UPDATEs `payment_attempts` with `callback` evidence → refused; an attempt or withdrawal UPDATE without an executing resolution → refused; a `deposit_intents` UPDATE → refused; the governed M2 path → succeeds |
| C-15 | AU | Audit rows, including tenant status |
| C-16 | MIG | 0114 up/down/up (HEAD-migrated). Down refuses with resolutions or new-kind rows. After down, `pg_get_functiondef(payment_attempts_guard)` equals 0107's, and the index definitions are unchanged. |
| C-17 | R | **F12:** a text diff of 0114's guard against 0107's equals exactly §8.3's (i) and (ii) |
| C-17b | R | **F12 (LF test 11): an exhaustive deposit matrix**, every `(OLD.state, NEW.state, last_evidence_kind, terminal_reason ∈ {NULL, each named reason, other})` for `operation = 'deposit'`, gives **identical accept/refuse** on a 0107 DB and a 0114 DB |
| C-18 | R | No platform policy → M1 and M2 disabled |
| C-19 | R | **F13 (LF test 12):** no reconciliation detail string says M1 clears |
| C-20 | R | A late provider decline after "declare paid": recorded, no state change, P1 raised |
| C-21 | R | Non-active tenant: M1 and M2 available; tenant status recorded; tenant-level tightening ignored for policy |
| C-22 | R | `pay_declared_paid_unconfirmed` stands until a confirming line (the compensation clearing path stays disabled pending ADR 0100 §5.4) |

**Mutants:**

| Mutant | Killed by |
|---|---|
| drop `executed_txid = txid_current()` | C-6 |
| drop the attempt binding | C-6 |
| allow deposit `disputed →` | C-1, C-17b |
| drop the target match | C-6 |
| drop the withdrawal `submitted` re-check | C-5 |
| drop the allow-list, or admit `amount_asset_mismatch` | C-5 |
| drop the beneficiary guard for M1 or M2 | C-10 |
| let M1 clear `pay_captured_unposted` | C-2 |
| accept the prefix at any one ingress | C-9 |
| use `LIKE` instead of `left()` with a `_`/`%`-bearing value | C-9 |
| drop the deferred check | C-13 |
| drop the `operator` term from the acting WITH CHECK | C-14b |
| let `pay_declared_not_paid_but_paid` clear on a partial recovery | C-7 |
| skip the amount comparison for a reserved-prefix Step B | C-8 |

## 13. Invariants

**Preserved:**
- **INV-DEP-1:** preserved. Deposit outcomes are identical (C-17b); indexes unchanged; M1 posts
  nothing.
- **Append-only double-entry:** only `Complete`/`Fail` → `ledger.Post`.
- **DB idempotency:**
  - Step B key `provider_id:platform-operator-declared:<id>`, which cannot collide with a real
    reference (Go validator, DB CHECKs, all-sessions ledger trigger);
  - `Fail` key unchanged;
  - one executed and one pending resolution per attempt;
  - UNIQUE `ledger_transaction_id`.
- **RLS:** T and A families only; acting updates gated by an executing resolution.
- **No direct balance mutation:** only withdrawal postings.
- **HD-LEDGER-UNALLOC-1:** A unchanged; B deferred.
- **INV-IO-7:** a single governed exception (§10). **INV-IO-9 / M3:** unchanged.

**Introduced:**

| ID | Invariant |
|---|---|
| INV-M-1 | No deposit attempt leaves `disputed` |
| INV-M-2 | An operator-evidence payout terminal exists only with an executed resolution from the same transaction, an allow-listed reason, and a `submitted` withdrawal |
| INV-M-3 | The reserved prefix appears only on M2 Step B postings |
| INV-M-4 | Every executed M2 is either confirmed or standing in reconciliation |

## 14. Alternatives rejected

| Alternative | Why rejected |
|---|---|
| A `resolved_no_action` deposit state | LF-1 |
| An M1 credit via K2 | LF-2 |
| M1 sets `investigation_status` | LF-3 |
| Declare paid under the real or an empty reference | Collision, or empty-key refusal |
| M2 on T14 | LF-15 |
| **M2 on `amount_asset_mismatch`** | F9. It goes to PAYOUT-AMOUNT-DISPUTE-1. |
| **The prefix refusal inside the shared `providerref.Validate`** | It would change casino and sportsbook semantics (F14). A payments-only validator plus DB backstops is used instead. |
| **`LIKE` for the prefix** | `_` is a wildcard (F2(d)). `left()` is used. |
| Approve now, execute later | LF-13 pattern |
| Edit `withdrawal.go` | Payments F3 |

## 15. Open items

1. **Ledger-finance confirms:**
   - the ruling 1 vs 5(c) resolution (ADR 0100 §5.4). Until then, the (c) compensation clearing is
     disabled;
   - the full-amount clearing tightening in §9(d).
2. **`payments` confirms the basis and context vocabulary** (§5.1). Automated verification of
   `reconciliation_exhausted` against persisted statement lines is a candidate follow-up, not K3
   scope.
3. **Follow-ups:** PAYOUT-AMOUNT-DISPUTE-1 (amount disputes); WITHDRAWAL-REVERSAL-1 (the
   `psp_clearing` correction, F11).
4. **Touches additions (§7):** the orchestrator records them.
5. **ADR 0095 amendments (§10) and ADR 0082 A8** (ADR 0100 §8): the orchestrator applies them.

**HUMAN DECISION REQUIRED:** none new. HD-PRH2-8 (ADR 0100 §3.4) also governs
`payment_force_resolve`; the interim is at least one independent approver.

## 16. Review disposition

| Finding | Where addressed |
|---|---|
| **Product-owner-proxy** | |
| `pending_suspense_allocation_b` deferral | §3, §8.1 (not seeded) |
| M1 four-eyes not overbuilt | §6 unchanged |
| **Security** | |
| K3-1 / C-101-1 | §6.4; C-14b |
| K3-2 / C-101-2 | §5.1; §8.2 CHECK |
| K3-3 / C-101-3 | §5.4; C-9 (including statement import) |
| C-101-4 / ruling 5 | §5.1; §8.2 columns; C-21 |
| **Ledger-finance** | |
| F2(d) | §5.4 `ledger_transactions_reserved_prefix_guard` (all sessions, `left()`); C-9 |
| F9 (HIGH) | §5.1 allow-list; `payment_m2_admits`; C-5; PAYOUT-AMOUNT-DISPUTE-1 |
| F10 (HIGH) / ruling 5 | §9 (a)–(e); §8.4 kind CHECK widened in 0114; C-7, C-8, C-22 |
| F11 | §5.3 residual row; WITHDRAWAL-REVERSAL-1 |
| F12 | §8.3 exact diff; C-17, C-17b |
| F13 | §4; C-19 |
| F14 | §5.4 (payments validator at deposit sync and poll, and every other ingress); C-9 |
| F16 | §3 (deferral concurred) |
| F17 | §5.3 (keys and releases as verified) |
| Ruling 1 (finding and basis codes) | §3; §5.1; §8.1 |
| Ruling 6 | §6.3; ADR 0100 §8 |
| LF rev-2 tests 9–14 | C-5, C-7/C-8, C-17b, C-19, C-9, every test |
| **Orchestrator** | Registry ids used: PAYOUT-AMOUNT-DISPUTE-1, WITHDRAWAL-REVERSAL-1 |

## 17. Handover / DoD

K3 updates:
- `docs/architecture/withdrawal-state-machine.md` (M2; the reserved namespace; the risks and
  residuals);
- `payment-orchestration.md` (M1/M2; operator evidence; the allow-list);
- `reconciliation-model.md` (LF-3 acknowledgement; the two new kinds; the M2 matching rules);
- `docs/security/security-architecture.md` (S-12; C-101-1);
- `backoffice/src/auth/permissions.ts`;
- `docs/runbooks/operational-runbooks.md`, entry "Payment force-resolution (M1/M2)":
  - T17 first; basis and evidence requirements;
  - `amount_asset_mismatch` is not M2-resolvable;
  - declared-paid-unconfirmed and declared-not-paid-but-paid recovery, and the `psp_clearing`
    residual;
  - M1 never clears `pay_captured_unposted`;
  - non-active tenants;
- `docs/runbooks/observability-and-alerting.md` (T14-after-M2 and the two kinds as P1s, with ADR
  0102).

The orchestrator updates: ADR 0095 (§10); the registry (HD-0095-1, M1/M2, PAYOUT-AMOUNT-DISPUTE-1,
WITHDRAWAL-REVERSAL-1); the HANDOVER index and mock-vs-real matrix; plan §3 Touches; the review
records.
