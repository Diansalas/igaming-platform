# ADR 0101 — Payment force-resolution M1/M2 (PRH-2 K3; amends ADR 0095 §4.8)

- **Status:** revision 2 ACCEPTED (2026-09-28): `security` CONFIRMED WITH CONDITIONS (C-3, C-4) and
  `ledger-finance` CONFIRMED WITH CONDITIONS (K3-a). **Revision 3 PROPOSED (2026-10-04, `architect`,
  K3 design phase, base `3517980`).** Revision 3 is a refresh plus a scope decision. It does not
  reopen any revision-2 ruling. Its **deltas** (§18 folds, §8.6 schema, §9.2 rules, §12.2 tests and
  the open questions in §22) need `security` and `ledger-finance` confirmation **before any K3
  code**. **NOT IMPLEMENTED.**
- **Revision history:** revision 1 (`d83a71c`) was reviewed ACCEPT (`product-owner-proxy`) and ACCEPT
  WITH CONDITIONS (`security`, `ledger-finance`). Revision 2 (`6864efa`) applied every condition
  (§16). Revision 3 is described in the table below and in §17a.
- **Decision type:** cross-domain architecture and financial control:
  - `payments`;
  - `withdrawal`, called but **not edited**;
  - `internal/providerref`;
  - `internal/reconciliation` (payment statement);
  - **migration 0115** (renumbered from 0114 by plan §11; see §17a).
- **Owner:** `architect`. **`ledger-finance` owns the financial invariants, and its rulings are
  binding.** **`payments` implements.** **Reviewers:** `security`, `ledger-finance`, `payments`,
  `qa`, `code-reviewer`, `architect` (cross-domain), and `product-owner-proxy` (scope folds, §18).
- **Registry:**
  - HD-0095-1 (decided);
  - ADR 0095 M1/M2;
  - follow-ups PAYOUT-AMOUNT-DISPUTE-1 and WITHDRAWAL-REVERSAL-1;
  - **revision 3:** PAY-RECON-PARKED-CAPTURE-STANDING-1 and PAY-RECON-POLL-REF-CLEAR-1 are **folded
    into K3** (§18). MA020-SYNC-MISMATCH-1, PAY-RECON-D2-HARDENING-1, PAY-PAYOUT-UNBOUND-HOLD-1 and
    PAY-PAYOUT-REFBIND-1 stay separate. HD-PRH2-9 goes to the **separate** workstream
    PAY-CLOSED-TENANT-FUNDS-RESOLUTION-1 (§19).

  Workstream K3; **migration 0115**.
- **Binding inputs:**
  - ADR 0098 §1 and §5; **ADR 0105 §1 and §4** (HD-PRH2-9, K3 scope instructions).
  - Plan §11 (including the `pending_suspense_allocation_b` deferral), §12, and the K3 scope note.
  - LF-1, LF-2, LF-3, LF-15, LF-18; S-12.
  - `reviews/adr-0099-0101-security.md` C-101-1..4 and ruling 5.
  - `reviews/adr-0099-0101-ledger-finance.md` F2(d), F9–F14, F16, F17 and rulings 1, 5 and 6.
  - `reviews/adr-0099-0101-product-owner-proxy.md`.
  - ADRs 0099 and 0100, revision 2, as implemented in 0112 and 0113.
  - **Revision 3:** `reviews/e1-k3-preflight.md` §2; ADR 0095 §35 (D2), §36 (D), §37 (H); the LF D2
    rulings B1, B3, B4, D2F-1 and PM-1..3; `reviews/h-ledger-finance*.md` F3.
- **This ADR amends ADR 0095 §4.8** (and §4.3, §28.9, §35.4 and INV-IO-7). §10 holds the amendment
  text. The orchestrator writes it into ADR 0095.

| Rev | Base | Change |
|---|---|---|
| 1 | `d83a71c` | Initial draft |
| 2 | `6864efa` | Acting UPDATE policies gated on an executing M2 resolution and `operator` evidence (C-101-1); `evidence_ref_hash` NOT NULL for M2 (C-101-2); tenant status recorded (C-101-4); the M2 terminal-reason allow-list refusing `amount_asset_mismatch` (F9); reconciliation matching plus two standing kinds (F10, ruling 5); the `psp_clearing` residual (F11); the exact permitted guard diff and the deposit matrix test (F12); detail text (F13); ingress validation (F14); the reserved-prefix trigger for all sessions (F2(d)); binding code catalogues (ruling 1); lock order (ruling 6); full migration content; review disposition |
| **3** | **`3517980`** | **Refresh:** migration 0114→**0115**; K2's fence and kind CHECK live in **0113** (not 0112); facts and line references re-verified against post-D2/F-pay/H/I-wire code (§1); F13 found **already landed by D2** (§4); the payout dispute reason set at HEAD, including `callback_amount_asset_mismatch` and `invalid_provider_reference:*` on payouts (§5.1); the authoritative file list (§7), including `poll_evidence.go`, the deposit poll binding sites and `httpserver/deposit_handlers.go`; what the P1 tests can assert today (§12.3). **Scope:** STANDING-1 and POLL-REF-CLEAR-1 folded under one LF-signed schema change (§18, §8.6, §9.2); the other four items stay separate (§18); HD-PRH2-9 is a separate workstream (§19); the 0115 plan and E1 coordination (§20); the open questions (§22) |

---

## 1. Context

ADR 0095 §4.8 defines three manual interventions: M1 (a disputed deposit), M2 (an ambiguous or
disputed payout) and M3 (a never-sent payout; the transition is built, but **no non-test caller
exists**, see §19). ADR 0098 decided M1 and M2 are allowed as governed capabilities. ADRs 0099 and
0100 provide grants, policy and four-eyes. Both are implemented (0112, 0113).

**Facts (re-verified at `3517980`; revision 3).** "R2" marks a revision-2 fact whose reference
moved.

| Fact | Where (at `3517980`) |
|---|---|
| No `failed` attempt state. The 0107 guard's state-pair whitelist has no `disputed → *` pair. No migration after 0107 redefines `payment_attempts_guard()`. | `0101` state CHECK; `0107:98` (guard), `:252` (the `declined → disputed` pair, T13t/T14) |
| Evidence gates: `→ succeeded` requires `sync`, `callback` or `query_status`; the same for payout `→ declined`; deposit `→ declined` is never `operator` | `0107:259-264` |
| The 0107 indexes `payment_attempts_one_succeeded_deposit_per_intent` and `ledger_transactions_one_deposit_per_intent` | `0107:78`, `:87` |
| Exactly one payout attempt per withdrawal | `0101:140` `payment_attempts_one_per_withdrawal` |
| `Complete` key = `providerID + ":" + providerTxID`; `Fail` key = `requestID + ":failed"`; both require `submitted` (R2) | `withdrawal.go:1487`, `:1584`; state checks `:1463`, `:1553` |
| `withdrawal.LockSubmittedForResolution` (R2) | `withdrawal.go:1184` |
| `ledger_transactions.reason_code` is allowed only on `manual_adjustment` and `bonus_forfeiture`. A `withdrawal_*` posting carries its reason on the audit row. | `lockorder.go:358-365`; `withdrawal.go:1575-1580` |
| **Payout dispute reasons written at HEAD** (R2, widened) | `amount_asset_mismatch`: `payout.go:1181` (poll). `provider_reference_mismatch`: `payout.go:698` (sync), `:1214` (poll), `receipt.go:883` (callback). `callback_amount_asset_mismatch`: `receipt.go:825-826` (**also reached by payouts**: the mismatch check runs before the deposit/payout split). `reversal_tombstone_precedes_success`: `receipt.go:836-837`. `success_for_never_sent_attempt` (T15): `receipt.go:756-757`. `invalid_provider_reference[:<reason>]`: `payout.go:558-560`, `:1001-1003`. `late_success_after_terminal`, `late_decline_after_terminal`, `late_contradicting_evidence`: `payout.go:715`, `:747`, `:840` via `applyPayoutLateEvidence` `:767`. T14 `success_after_payout_declined`: `receipt.go:791` |
| **Payout disputes raise no durable alert.** `raiseDepositParkAlert` is a no-op for payouts, including T14. | `alerts.go:95-96`; ADR 0102 §17.3 and §17.8 |
| Reconciliation skips `disputed` attempts except the captured-unposted classes (R2) | `payment_statement.go:1084` |
| `pay_captured_unposted`: bound in-run `:1063-1071`; unbound in-run `:1072-1083`; bound standing `:1191-1194`; clearing `:1164`; classes `disputeReasonClasses` `:211`; runtime rule `captureClass` `:247` (D2) | `payment_statement.go` |
| **F13 is already in the code.** D2 landed the exact detail string "…M1 only acknowledges" at all four sites and in the type comment. | `payment_statement.go:154-163`, `:1071`, `:1083`, `:1135`, `:1194` |
| Settlement-reference comparison for payouts | `payment_statement.go:1040-1045` |
| Statement fetch validation (`validatePaymentLine`) uses the shared `Validate` | `payment_statement.go:390-410` (`:395`, `:398`, `:401`, `:408`) |
| Persisted statement lines exist per import: append-only, FORCE RLS, no cross-import index | `0102:40-98` (`payment_statement_lines_import` index only, `:98`); append-only and RLS `:147-170` |
| `providerref.Validate` / `ValidateOptional` / `ValidateAll` reserve no prefix (R2) | `providerref.go:100`, `:111`, `:126` |
| K2 objects K3 builds on | `financial_policy_required_approvals` `0113:631` (the `payment_force_resolve` non-active special case at `:655`); `player_open_payment_exposure` `0113:706`; `ledger_adjustment_payload_refusal` `0113:808` (MA020 at `:886`); `ledger_governed_fence_allows` `0113:1426` (branch (a) only); fence triggers `:1437`/`:1449`, `:1463`/`:1507`; acting reads on `payment_attempts`/`deposit_intents` `:1584-1586`; kind CHECK `:1850-1863`; capability-parametric grant helper `ledger_adjustment_eligible_grant` `:898` |
| The capability enum already holds `payment_force_resolve:{request,approve}` (no migration needed for the enum) | `0112` catalogue; `internal/capability/capability.go:40-41` |
| H: non-active tenants are resolution-only. Payout T2 re-claim and T12 resend are withheld. | `sweeper_resolution_only.go:38`, `:75`; `payout_sweep.go:271`, `:364`; ADR 0095 §37.3 |

## 2. Decision summary

1. **Deposits never leave `disputed`.** M1 is evidence-only, with no posting and no ledger link
   (LF-1, LF-2).
2. **M1 never clears or suppresses `pay_captured_unposted`** (LF-3), whether bound, unbound,
   in-run or standing (now including the folded STANDING-1 standing findings, §9.2).
3. **M2 is payouts only.**
   - Transitions: `{ambiguous, disputed}` → `succeeded` ("declare paid") or → `declined` ("declare
     not paid").
   - Only with `operator` evidence, an executed four-eyes resolution in the same transaction, a
     withdrawal in `submitted`, and an **allow-listed** dispute reason. `amount_asset_mismatch` and
     `callback_amount_asset_mismatch` are refused.
   - "Declare paid" uses the reserved provider-tx namespace **`platform-operator-declared:`**.
4. **Acting sessions** (ADR 0099) can update attempts and withdrawals only with an executing M2
   resolution, and for attempts only with `operator` evidence (C-101-1).
5. **Three standing reconciliation kinds** keep every M2 risk visible:
   `pay_declared_paid_unconfirmed`, `pay_declared_not_paid_but_paid` and
   `pay_declared_paid_compensated_but_paid` (LF K3-a).
6. **0115** changes the 0107 guard by exactly the diff in §8.3. The down migration restores the
   0107 guard verbatim and refuses while resolutions or evidence exist.
7. **(Revision 3) Persisted-evidence substrate.** K3 builds one cross-import lookup over
   `payment_statement_lines` (§9.2). It serves M2 rules (c)/(c2)/(d) and the folded
   STANDING-1. The poll's returned reference Y is persisted as structured evidence
   (POLL-REF-CLEAR-1), both under 0115, signed off once by ledger-finance.

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
  - It never writes `payment_attempt_reference_evidence` (§8.6) or any reconciliation table.
- **Where funds go.** Funds for a financially resolved intent leave only by a PSP refund
  (reversal → tombstone), or by LEDGER-SUSPENSE-B-1 later.
- **Out of scope.** An M1 credit for an unresolved intent. If it is ever built, it goes through
  `postDepositSuccess`.
- **Governance.** As §6. With no amount, only the policy base applies (ADR 0100 §3.2).

## 4. LF-3: M1 and reconciliation (F13)

- `pay_captured_unposted` keeps being emitted every run until it clears by its own rule: a reversal
  or tombstone on the clearing reference (§9.2 for the folded clearing references), or a future (B)
  allocation. That holds for the bound standing rule (unwindowed), the unbound in-run rule, and the
  standing unbound rule folded in from STANDING-1. M1 writes nothing to reconciliation tables and
  never sets `investigation_status`.
- "Acknowledged" is a read-time join to the executed M1 resolution. The emission predicate does not
  reference resolutions. **This includes the standing `poll_reference_mismatch` finding** (ADR 0095
  §35.4 operator rule; registry note on POLL-REF-CLEAR-1).
- **F13: ALREADY IMPLEMENTED by D2** (revision 3 finding). The detail strings at
  `payment_statement.go:1071`, `:1083`, `:1135` and `:1194`, and the comment at `:154-163`, already
  carry:

  > `resolution: a PSP-initiated reversal/tombstone, or allocation (LEDGER-SUSPENSE-B-1); M1 only acknowledges`

  (or the "…on this line's reference…" variant for unbound parks). K3 makes **no text change** here.
  Test C-19 stays as a regression pin: no detail string says M1 clears, including the strings K3
  adds for its three new kinds and for the folded standing rule.

## 5. M2: payout force-resolution

### 5.1 Preconditions (executor **and** guard/trigger)

| Precondition | Detail |
|---|---|
| Operation and state | `operation = 'payout'`; `state ∈ {ambiguous, disputed}` |
| **Allow-list (F9; revision 3 restated against HEAD)** | `state = 'ambiguous'`, **or** `state = 'disputed' AND terminal_reason IN ('provider_reference_mismatch', 'success_for_never_sent_attempt')`. **Every other payout reason is refused (`MR010`) for both M2 kinds.** That covers the reasons §1 lists at HEAD: `amount_asset_mismatch` and **`callback_amount_asset_mismatch`** (the provider evidence contradicts the amount, so they go to PAYOUT-AMOUNT-DISPUTE-1); `invalid_provider_reference[:*]` (an unbound payout park, so its hold is kept, per PAY-PAYOUT-UNBOUND-HOLD-1); `reversal_tombstone_precedes_success`; the three `late_*` reasons; and T14 `success_after_payout_declined`, which the withdrawal-state precondition already excludes. Any future reason is refused until this ADR names it. The allow-list is a closed literal set in `payment_m2_admits` and in the executor, and a pin test (C-5b) enumerates every payout dispute write site. |
| Withdrawal | `submitted`. `LockSubmittedForResolution` refuses otherwise, and the guard re-checks it. This excludes T14 disputes (LF-15). |
| Provider | `attempt.provider_id IS NOT NULL` |
| **Basis (LF ruling 1)** | `basis_code IN ('provider_confirmed_out_of_band', 'reconciliation_exhausted')` for both M2 kinds (CHECK). Meaning: for `m2_declare_paid`, `reconciliation_exhausted` means a statement success line exists; for `m2_declare_not_paid`, it means statement coverage extends past the attempt with no line. `context_code` is NULL or in `('provider_unqueryable', 'past_resubmission_horizon')`, and is **secondary only**. The vocabulary is `payments`' to confirm. Automated verification of `reconciliation_exhausted` against the §9.2 persisted lines stays a candidate follow-up, not K3 scope. |
| **Evidence (C-101-2)** | `evidence_ref_hash` NOT NULL for M2 (CHECK). The runbook requires an operator T17 re-verify first, and its outcome is referenced. |
| Kind | `kind ∈ {m2_declare_paid, m2_declare_not_paid}`, with `target_state` `succeeded` / `declined` (CHECK) |
| **Tenant status (C-101-4; security ruling 5)** | M1 and M2 are **available for non-active tenants**, which includes `closed` (§19). Tenant status is read in-tx and recorded at submission and at execution. Policy evaluation for a non-active tenant ignores tenant and brand rows (`0113:655`). |
| **Payout KYC gate (revision 3, open question Q-IC-1)** | Architect position: the ADR 0096 payout KYC gate does **not** apply to M2. M2 records a fact about a payout already dispatched (declare paid), or releases a hold the provider never paid out (declare not paid). It is not a dispatch. ADR 0095 §5 already rules that no KYC outcome triggers or blocks a post-dispatch resolution. `identity-compliance` and LF confirm. |

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
| Posting | `withdrawal_completed`, key `provider_id:reservedTxID`, where `reservedTxID = 'platform-operator-declared:' || resolution_id` (`withdrawal.go:1487`) | `withdrawal_failed`, key `wr.id:failed` (`withdrawal.go:1584`); the hold is released to `player_cash` |
| Late evidence | A real success: the already-succeeded branch records it; `Complete` refuses (`state ≠ submitted`); **no second Step B**. A real decline: recorded, no state change. The P1 surface is §12.3 (C-20). | A real success: **T14** (`declined → disputed`, `receipt.go:791`, or `payout.go:775` via late evidence). This is a **double payout**. The P1 surface is §12.3 (C-7). |
| Residual | Provider never paid: the player's hold went to `psp_clearing`. A `compensating_entry` credit restores the player (ADR 0100 §5.4, adopted), **but `psp_clearing` stays misstated** (F11). It is tracked by `pay_declared_paid_unconfirmed`. The proper fix, a withdrawal reversal (`TxWithdrawalReversed` `ledger.go:114` exists; the transition is not implemented), is **WITHDRAWAL-REVERSAL-1**. | Recovery is a `compensating_entry` debit with causation = the `withdrawal_failed` transaction, which ADR 0100 §6.4 may refuse for insufficient funds, or an off-platform process. It is tracked by `pay_declared_not_paid_but_paid`. |

**INV-IO-7 is amended** (§10): "declare not paid" is the single governed exception.

### 5.4 The reserved namespace (LF-15(1), F2(d), F14, C-101-3)

- **The prefix:** `payment_reserved_ref_prefix()` is an IMMUTABLE SQL function returning
  `'platform-operator-declared:'` (27 bytes). The Go constant `providerref.ReservedOperatorPrefix`
  matches, and a test pins the two equal.
- **A Go validator for payments only (F14).**
  - `providerref.ValidatePaymentReference(field, value)` = `Validate` plus refusal of the prefix
    (new reason `ReasonReservedNamespace`). `ValidatePaymentReferenceOptional` and a payments
    `ValidatePaymentReferences(fields...)` (the `ValidateAll` shape) follow the same rule. They
    return the same `*providerref.Error` type, so every caller's existing `AsError` branch and 4xx
    mapping applies unchanged.
  - The shared `Validate` is unchanged, so casino and sportsbook semantics do not change.
  - **Every payments ingress at HEAD (revision 3, authoritative):**

    | Ingress | Site at `3517980` | Change |
    |---|---|---|
    | Deposit sync (phase C) | `drive.go:311` / `:313` | switch to the payments validator |
    | Deposit poll, a pending branch binding the echo of an unbound attempt | `sweeper.go:499-503` | **add** validation before binding. It is reachable only by a direct caller: `processViaQueryStatus` never polls a reference-less attempt (`:366`). A refused echo is not bound: no state change, plus an audit. |
    | Deposit poll, a decline branch adopting the echo of an unbound attempt | `sweeper.go:646-647` | **add** validation; a refused echo is not adopted |
    | Deposit poll echo audit and the Y evidence write | `poll_evidence.go:116-119`, `:175-183` (`echoAuditMeta`) | switch `echoAuditMeta` to the payments validator, so a prefixed echo is recorded as reason, length and hash only. Y is persisted only if it passes (§8.6). |
    | Payout sync | `payout.go:453` | switch |
    | Payout poll | `payout.go:1242` | switch |
    | Verified callbacks, deposit and payout (`ReceiveVerifiedCallback`) | `receipt.go:1631` (`validateReceiptReferences`) | switch all four fields |
    | Legacy callback (`HandleCallback`) | `orchestrator.go:1003` | switch `provider_reference` and `original_provider_reference` |
    | Webhook HTTP mapping | `httpserver/deposit_handlers.go:316` (`newPaymentWebhookHandler`) | **no new branch expected**; a test asserts that a reserved-prefix reference maps to the same deterministic 4xx class as any other invalid reference, and never to a retryable status |
    | Statement fetch | `payment_statement.go:395`, `:398`, `:401` (`validatePaymentLine`) | switch `provider_reference`, `original_provider_reference` and `settlement_reference` (the run fails and nothing is stored, ADR 0095 §35.2) |
- **DB backstops.**
  - A CHECK `left(col, 27) <> payment_reserved_ref_prefix()` on:
    - `payment_attempts.provider_reference`;
    - `payment_provider_events.provider_reference`, `.original_provider_reference`,
      `.settlement_reference`;
    - `payment_statement_lines.provider_reference`, `.original_provider_reference`,
      `.settlement_reference`;
    - **(revision 3)** `payment_attempt_reference_evidence.reference` (§8.6);
    - **(revision 3, to confirm in implementation)** `deposit_intents.provider_reference` and
      `withdrawal_requests.provider_reference`, if those columns carry provider-supplied values.
      `payments` lists every provider-reference column by a catalogue query
      (`information_schema.columns` filtered by name), and the list is pinned by test C-9c.
  - A trigger **`ledger_transactions_reserved_prefix_guard`** (BEFORE INSERT, **all sessions**,
    `left()` not `LIKE`). If `left(NEW.provider_tx_id, 27) = prefix`, it requires
    `NEW.transaction_type = 'withdrawal_completed'` and ADR 0099 §6.6 predicate (b) (an executing
    `m2_declare_paid` resolution binding correlation, provider and key). Otherwise it raises
    `MR020`.
  - **0115 up refuses** if any existing value in those columns, or any
    `ledger_transactions.provider_tx_id`, already has the prefix.

## 6. Governance

### 6.1 Capabilities, policy and counting

| Aspect | Rule |
|---|---|
| Grants | `payment_force_resolve:request` (requester) and `payment_force_resolve:approve` (approvers), for the attempt's tenant (ADR 0099; already in the 0112 enum). Eligibility reuses `ledger_adjustment_eligible_grant(p_tenant, p_staff, p_capability)` (`0113:898`), which is capability-parametric. K3 may rename it to a neutral alias only by `CREATE FUNCTION` of a wrapper, never by editing K2's body. |
| Classification | `mandatory_four_eyes` (already seeded, `0113:89-91`) |
| Policy | `financial_policy_required_approvals('payment_force_resolve', …)`: M2 uses the attempt's amount and asset; M1 the base only; `GREATEST(1, …)`; no platform baseline ⇒ disabled |
| Independence floor | LF-11: a distinct, non-NULL `person_id` for the requester and each approver. **Non-configurable.** |
| Counting | exactly ADR 0100 §6.2, including `FOR SHARE` and S-2(iii) for approvers |
| Payload hash | covers `attempt_id`, `kind`, `target_state`, `finding_code`/`basis_code`/`context_code`, `evidence_ref_hash`, amount, asset, reason code |
| HD-PRH2-8 interim | at least one independent approver (ADR 0100 §3.4) |

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
9. **M2 only:** attempt UPDATE (§5.2) → `Complete` or `Fail` (L3/L4 via
   `LockProjectionsForPosting` inside `ledger.Post`, `lockorder.go:113`) → `ledger_transaction_id`.
10. → `executed`. Audit. Commit.

- **Deferred constraint trigger.** No `executing` row at commit. For an executed M2, also:
  - attempt = `target_state`;
  - withdrawal `completed` or `failed`;
  - `ledger_transaction_id` = a same-tenant `withdrawal_completed` keyed
    `provider_id:reservedTxID`, or a `withdrawal_failed` keyed `wr.id:failed`, with
    `correlation_id = wr.id`.
- **Race with a sweeper or callback.** The same L1 parent → attempt order applies. Whoever commits
  first wins. The loser sees a state outside the preconditions (M2 stays `pending`, later
  cancelled or expired) or takes the existing already-terminal branch. **(Revision 3)** H's payout
  sweeper locks the withdrawal first in T2/T12 (`payout_sweep.go:253`, `:321`) and in poll-apply,
  so the race is serialized on the same L1 row. For a non-active tenant, T2 and T12 are withheld
  anyway (§37.3), but polls still run and can race M2. C-12 covers both.
- **I-wire constraint (revision 3).** Any alert raise K3 adds must sit inside an `alerting.InTx`
  closure (I-wire `static_wiring_test.go`). **K3 adds no in-tx raise** (§12.3); the reconciliation
  P1 is raised post-commit by the existing scheduler site.

### 6.4 Acting-session UPDATE policies (C-101-1), in 0115

| Table | USING (row visibility; allows `FOR UPDATE` locking) | WITH CHECK (what an actual UPDATE may write) |
|---|---|---|
| `payment_attempts` | `tenant_id = acting_tenant AND financial_acting_session_valid()` | the same **AND** `last_evidence_kind = 'operator'` **AND** `EXISTS (SELECT 1 FROM payment_manual_resolutions m WHERE m.attempt_id = payment_attempts.id AND m.tenant_id = payment_attempts.tenant_id AND m.state = 'executing' AND m.executed_txid = txid_current() AND m.kind IN ('m2_declare_paid','m2_declare_not_paid'))` |
| `withdrawal_requests` | as above | the same **AND** `EXISTS (… m.withdrawal_request_id = withdrawal_requests.id … state = 'executing' AND executed_txid = txid_current() AND m.kind IN (M2 kinds))` |
| `deposit_intents` | as above | `false` (M1 never updates an intent) |

- Every other table `Complete`/`Fail` touch is a ledger table (ADR 0099 §6.5–§6.7 fences) or
  `ledger_accounts` (INSERT limited by account type).
- **Consequence:** an acting session **cannot** write `callback`, `sync` or `query_status` evidence,
  and cannot move an attempt or withdrawal without an executing M2 resolution. This closes K3-1.
- **LF C-K1-2:** 0115 replaces the fence function with (a)+(b)+(c) **in the same migration** as
  these acting policies.
- **Revision 3:** acting sessions get **no** policy on `payment_attempt_reference_evidence` or
  `payment_statement_lines`. Neither M1 nor M2 needs them, and the executor reads no statement
  evidence.

## 7. What K3 touches (authoritative, revision 3)

Rule 1: this list is the K3 Touches record. The orchestrator copies it into plan §3/§12.
"Folded" marks a §18 fold.

| File | Why |
|---|---|
| `migrations/0115_payment_force_resolution.{up,down}.sql` (name indicative) | §8 |
| `internal/payments/attempt.go`; **new** `internal/payments/manual_resolution.go` | §5, §6 (executor, M2 attempt transitions with `EvidenceOperator`) |
| `internal/providerref/providerref.go` (+ its tests) | §5.4 the payments validator and prefix constant |
| `internal/payments/drive.go` | §5.4 deposit sync ingress (`:311`, `:313`) |
| `internal/payments/sweeper.go` | §5.4 the deposit poll binding sites (`:499-503`, `:646-647`) |
| **`internal/payments/poll_evidence.go`** | §5.4 echo audit (`:175-183`); **folded** POLL-REF-CLEAR-1 Y evidence write at the `poll_reference_mismatch` park (`:116-119`) |
| `internal/payments/payout.go` | §5.4 payout sync (`:453`) and poll (`:1242`) ingress |
| `internal/payments/receipt.go` | §5.4 callback ingress (`:1631`). **No change to the dispute sites** (`:756`, `:825`, `:883`): the allow-list lives in the executor and the DB, not at the write sites |
| `internal/payments/orchestrator.go` | §5.4 legacy callback ingress (`:1003`) only |
| **`internal/httpserver/deposit_handlers.go`** | §5.4: test-backed confirmation that the reserved-namespace refusal maps to the existing deterministic 4xx. A code edit only if that test fails. |
| `internal/reconciliation/payment_statement.go` | §5.4 statement fetch validation; §9.1 M2 rules (a)–(e); **folded** §9.2 (standing unbound coverage, persisted-reversal clearing, Y clearing). **The F13 text is already done** (§4). |
| `internal/reconciliation/` (new test files) | C-7/C-8/C-22/C-29, the folded STANDING-1 tests, T15i |
| **new** `internal/httpserver/payment_force_resolution_routes.go`; **one registration line** in `internal/httpserver/routes.go` (next to `registerManualAdjustmentRoutes`, `:178`) | the staff API (request, approve, reject, cancel, list, get), mirroring `manual_adjustment_routes.go` |
| `internal/auth/permission.go` | static permissions `payment_force_resolve:request` / `:approve` / `:read` and their role map, mirroring `PermLedgerAdjustment*` (`:654-656`). This is a two-layer gate: the static permission plus the in-tx grant (ADR 0099 §2). |
| `backoffice/src/auth/permissions.ts` | the matching UI permission constants (DoD) |
| `deploy/init-app-role.sql` | **append** the K3 block **after** E1's block (§20.3) |
| Tests: new `internal/payments/*_k3_*_integration_test.go`, `internal/payments/migration_0115_integration_test.go`, `internal/providerref/*_test.go`; **plus** updates to `migration_0101_integration_test.go` and `migration_0107_integration_test.go` HEAD pins only if they assert the guard body text | §12 |
| Docs (DoD, §17) | as listed |

**Called, NOT edited:** `withdrawal.Complete`/`Fail`/`LockSubmittedForResolution`;
`internal/capability`; K2's `internal/adjustment/*` Go code; `internal/payments/payout_sweep.go`,
`sweeper_resolution_only.go` and `alerts.go` (H/I-wire); `internal/alerting/*`;
`reconciliation/scheduler.go`; `cmd/platform-api/main.go`; `internal/config/config.go`.
**If** implementation finds an edit to any of these unavoidable, it is a Rule 1 Touches addition
recorded by the orchestrator first. For `main.go` and `config.go`, it also waits for E1's merge
(§20.3).

**K2 SQL objects K3 replaces in 0115 (CREATE OR REPLACE, built on the 0113 body):**
`ledger_governed_fence_allows` (adds branches (b) and (c)); `ledger_adjustment_payload_refusal`
(adds the M2 Step B compensating-credit causation arm, ADR 0100 §5.4 adopted). Each down migration
restores the 0113 body byte-for-byte (C-16).

## 8. Migration 0115 (K3): content

Common rules: FORCE RLS on new tables; no `FOR ALL`; `BEFORE TRUNCATE` deny; DELETE refused; no
`SECURITY DEFINER`; SQLSTATE class **`MR`**. The force-resolution tables use **families T and A
only; no P** (HD-PRH2-6). Reference rows are inserted before FORCE RLS (the 0113 pattern).

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

**`payment_manual_resolutions`** (unchanged from revision 2):

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
'pending'`; `(tenant_id, state)`; `(tenant_id, ledger_transaction_id)`.

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

**RLS (QA F4, stated explicitly):** both tables ENABLE + FORCE RLS. Family **T**:
`tenant_scope_select` / `_insert` (and `_update` on resolutions only, for the state machine), all
`tenant_id = current tenant GUC` with no platform GUC present. Family **A**: `acting_read`,
`acting_insert` and `acting_update` (resolutions only), each requiring `financial_acting_session_valid()`
and `tenant_id = acting tenant`, mirroring K2's `ledger_adjustment_requests` policies. **No P
family.** The codes table: `reference_read FOR SELECT USING (true)`, no write policy.

### 8.3 The `payment_attempts_guard()` change: the exact permitted diff (F12)

The 0115 body equals the **0107 body verbatim**, except for exactly these edits:

**(i)** Two lines are **inserted** into the state-pair whitelist, before its closing `)`:

```sql
OR (OLD.state = 'disputed' AND NEW.state = 'succeeded' AND OLD.operation = 'payout' AND payment_m2_admits(OLD.id, OLD.state, OLD.terminal_reason, OLD.withdrawal_request_id, NEW.state, NEW.last_evidence_kind))
OR (OLD.state = 'disputed' AND NEW.state = 'declined'  AND OLD.operation = 'payout' AND payment_m2_admits(OLD.id, OLD.state, OLD.terminal_reason, OLD.withdrawal_request_id, NEW.state, NEW.last_evidence_kind))
```

The existing `(OLD.state = 'declined' AND NEW.state = 'disputed')` line (`0107:252`) keeps its
trailing comment. The inserted lines follow it with a leading `OR`.

**(ii)** The two evidence gates (`0107:259-264`) are **rewritten** as `<0107 predicate> AND NOT
(payout AND M2)`:

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
stated as this exact diff, not as literal identity (F12). The ambiguous → succeeded/declined pairs
already exist in 0107's whitelist (as evidence transitions). Only the evidence gates admit them
with `operator` evidence.

### 8.4 Other objects

| Object | Change |
|---|---|
| The §5.4 CHECKs | on `payment_attempts`, `payment_provider_events`, `payment_statement_lines`, `payment_attempt_reference_evidence`, plus any column C-9c lists |
| `ledger_transactions_reserved_prefix_guard` | all sessions |
| `ledger_governed_fence_allows` | replaced: ADR 0099 §6.6 (a) + (b) + (c). Both fence triggers (`ledger_transactions_governed_fence`, `ledger_entries_governed_fence`) call it unchanged |
| `ledger_adjustment_payload_refusal` | replaced: 0113 body plus the M2 Step B causation arm (ADR 0100 §5.4 adopted; security C-4 (a)–(e)). **The MA020 tail is unchanged** pending Q-LF-2 (§18.3) |
| Acting policies | §6.4. Acting SELECT on `withdrawal_requests`. **Not** on `payment_statement_lines` or the evidence table |
| **Reconciliation kinds** | `reconciliation_mismatches_mismatch_kind_check` (the constraint **name is kept**; `migration_0097/0098` tests match it) is dropped and re-added as a strict superset of **0113's** list (`0113:1850-1863`), or of the latest definition at merge if one lands earlier, plus **`pay_declared_paid_unconfirmed`**, **`pay_declared_not_paid_but_paid`** and **`pay_declared_paid_compensated_but_paid`** (LF K3-a). **The folded items add no kind** (§9.2) |
| **(Folded) `payment_attempt_reference_evidence`** | §8.6 |
| **(Folded) indexes on `payment_statement_lines`** | §8.6 |
| **Up-time refusal** | if any reserved-prefix value exists (§5.4) |

### 8.5 Down

- **Refuse** (`MR099`) while any `payment_manual_resolutions` row exists, any mismatch row of the
  three new kinds exists, **or any `payment_attempt_reference_evidence` row exists** (revision 3:
  dropping it would destroy money evidence).
- Otherwise:
  - restore **the 0107 `payment_attempts_guard()` body verbatim**;
  - restore **0113's** `ledger_governed_fence_allows` (branch (a) only) and **0113's**
    `ledger_adjustment_payload_refusal`, byte-for-byte;
  - drop the reserved-prefix trigger and CHECKs, the acting policies, the new tables, the folded
    evidence table and indexes, and the functions;
  - restore **0113's** kind CHECK exactly (or, if another kind-widening migration merged between
    0113 and 0115, that migration's definition; §20.2).
- The `providerref`, payments and reconciliation Go changes revert with the code.
- **Must succeed on an empty scratch DB**: the full-chain rollback tests
  (`jurisdiction/migration_0075_*`, `migration_0077_*`) roll back every migration above 0099.
- **Guard tests run on a HEAD-migrated scratch DB** (LF-18).

### 8.6 (Revision 3, folded) Persisted evidence substrate: STANDING-1 + POLL-REF-CLEAR-1

**One schema change, ledger-finance sign-off required** (LF D2 final review B3; registry rows).

**(a) `payment_attempt_reference_evidence`.** This is the structured home of the poll's returned
reference Y (POLL-REF-CLEAR-1). It is never audit JSON (LF ruling).

| Column | Type / constraint |
|---|---|
| `id` | `UUID PK DEFAULT gen_random_uuid()` |
| `tenant_id` | `UUID NOT NULL` |
| `attempt_id` | `UUID NOT NULL`; composite FK `(attempt_id, tenant_id)` → `payment_attempts (id, tenant_id)` (add a UNIQUE `(id, tenant_id)` on `payment_attempts` if none exists; `payments` to check) |
| `provider_id` | `TEXT NOT NULL`, the 0099 bound; must equal the attempt's `provider_id` (trigger) |
| `evidence_kind` | `TEXT NOT NULL CHECK (evidence_kind IN ('poll_returned_reference'))`. The set is closed; widening needs a migration and LF |
| `reference` | `TEXT NOT NULL`, the 0099 bound (length, no control characters), plus the §5.4 reserved-prefix CHECK, plus `CHECK (reference <> '')` |
| `recorded_at` | `TIMESTAMPTZ NOT NULL DEFAULT now()` |

- **Constraints and triggers:** UNIQUE `(tenant_id, attempt_id, evidence_kind)` (a park happens once;
  a replay is a no-op through `ON CONFLICT DO NOTHING` only when the stored reference is equal,
  otherwise it raises). A BEFORE INSERT trigger requires the attempt to be
  `operation = 'deposit'`, the reference to differ from the attempt's bound `provider_reference`,
  and the provider to match. Writing in the **same transaction** as the T10 park is required: the
  executor inserts the evidence row **before** `parkDepositAttempt`, so the alert raise stays the
  last alert-table statement (ADR 0102 §7.7), and a failed park CAS rolls the row back.
  `ledger_deny_mutation` on UPDATE, DELETE and TRUNCATE, binding on the owner too (0102 pattern).
- **RLS:** ENABLE + FORCE. Family **T** only: `tenant_scope_select`, `tenant_scope_insert`
  (`tenant_id = current tenant GUC`, no platform GUC). No A family, no P family. The
  reconciliation stream reads it under `WithTenantSnapshot` like every other payment table.
- **Grants:** `SELECT, INSERT` to `igaming_runtime` (migration block and `init-app-role.sql`).
- **Writer:** only `poll_evidence.go`'s `poll_reference_mismatch` park, and only when
  `ValidatePaymentReference(Y)` passes. An invalid Y stays audit-only (length and hash prefix),
  exactly as today.

**(b) Indexes on `payment_statement_lines`** (the cross-import lookup; non-concurrent is
acceptable because the migration runs in a deploy window, and the table is append-only and small
today; LF/devops confirm):
- `(tenant_id, provider_id, provider_reference)`;
- `(tenant_id, provider_id, merchant_reference) WHERE merchant_reference IS NOT NULL`;
- `(tenant_id, provider_id, original_provider_reference) WHERE kind = 'deposit_reversal'`.

No column is added to `payment_attempts`, so `payment_attempts_guard()` keeps exactly the §8.3 diff
(F12). A column alternative was rejected for that reason (§14).

## 9. Reconciliation (implemented in `payment_statement.go` by K3)

### 9.1 M2 rules (F10, LF ruling 5): unchanged from revision 2

| Rule | Behaviour |
|---|---|
| (a) | A reserved-prefix `withdrawal_completed` **skips the settlement-reference comparison** (`payment_statement.go:1040-1045`). Amount and asset are still compared. |
| (b) | Statement lines for an M2-declared payout resolve **by reference, then by merchant reference**. A matching `succeeded` line is a **confirmation**, counted by the metric `payment_m2_declared_paid_confirmed_total` (no tenant label); it is not a mismatch. A `declined` line → `pay_status_mismatch`. |
| (c) | **`pay_declared_paid_unconfirmed`**, standing and **unwindowed**, is raised for every executed `m2_declare_paid`. It clears **only** when (i) a confirming `succeeded` line exists in any persisted import (§9.3 lookup), or (ii) a future WITHDRAWAL-REVERSAL-1 posting reverses that Step B. **A `compensating_entry` credit with causation = that Step B does not clear it** (LF K3-a). The compensation appears only as a read-time annotation. |
| (c2) | **`pay_declared_paid_compensated_but_paid`** (P1, standing, **unwindowed**; security C-4(b), LF K3-a) is raised when an executed `compensating_entry` credit whose causation is an M2 Step B is followed by a confirming `succeeded` line. It clears only when executed `compensating_entry` **debits**, whose causation is that credit's own `manual_adjustment` transaction, total **at least** the credited amount. |
| (d) | **`pay_declared_not_paid_but_paid`**, standing and **unwindowed**, is raised for every executed `m2_declare_not_paid` whose attempt reached T14 `disputed` **or** for which any persisted import has a `succeeded` line (§9.3 lookup). It clears only when executed `compensating_entry` debits with causation = the `withdrawal_failed` transaction **total at least the withdrawn amount** (LF confirmed). It **overrides the `:1084` disputed exclusion** for these attempts. |
| (e) | The reserved id is never expected on a statement. A line carrying the prefix is refused at fetch (§5.4). |

**The compensating-credit causation arm** (ADR 0100 §5.4, adopted; security C-4 (a)–(e), LF K3-a)
is added to `ledger_adjustment_payload_refusal` in 0115. Its conditions:
- the reserved prefix via `left()` **and** `transaction_type = 'withdrawal_completed'`;
- an executed `m2_declare_paid` whose `ledger_transaction_id` is that causation;
- the `player_withdrawal_hold` leg on this wallet and asset (replacing the `player_cash` leg test
  for this arm only; today's code returns `MA022:causation_not_on_wallet` for any Step B);
- credit only; the cap is the hold-leg amount, under L2; evidence required;
- Person separation from the M2 resolution.

### 9.2 (Revision 3, folded) STANDING-1 and POLL-REF-CLEAR-1

| Rule | Behaviour (ledger-finance owns the final wording and signs off) |
|---|---|
| **S1: standing coverage for unbound parks** (STANDING-1) | For every `disputed` deposit attempt that the D2F-1 runtime rule (`captureClass`) treats as **unbound**: `invalid_provider_reference*`; a phase C `provider_reference_conflict` park with no reference; and the reference-less bound-reason parks (`callback_amount_asset_mismatch`, `multiple_success_for_intent` resolved by merchant reference). If **any persisted import** of this tenant and provider has a `deposit` line with status `succeeded` that resolves to the attempt **by merchant reference** (or by reference, should the attempt hold one), `pay_captured_unposted` is raised **on every run, unwindowed**, keyed by the attempt and the line's reference. This closes the ADR 0095 §35.4 "drops out silently" hole. |
| **S2: clearing for S1** | The finding clears only on a `deposit_reversal` line in **any persisted import** naming the evidencing line's reference as original, or a `tombstone` ledger row on `(provider_id, that reference)`. M1 never clears it (LF-3). |
| **S3: persisted-reversal clearing for bound standing findings** (STANDING-1, LF D2 confirmation, the TestD2_14 shape) | The bound standing rule (`:1191-1194`) also clears on a `deposit_reversal` line in **any persisted import** naming the bound reference X, not only in this run. A tombstone on X still clears it. |
| **S4: Y clearing** (POLL-REF-CLEAR-1, LF B3) | For a `poll_reference_mismatch` park with a `payment_attempt_reference_evidence` row Y, the bound standing finding clears on a reversal line (any persisted import) or a tombstone on **X or Y**. Without a Y row, X only, as today. Y is read only from the evidence table, **never from audit JSON**. |
| **S5: T15i** | An integration test of `success_for_never_sent_attempt` bound-if-referenced and unreferenced, built through the payout `created → disputed` path or a provider-bearing deposit fixture (`payments` provides a test helper; no production change). |
| **N1 residual** (ADR 0095 §35.4) | Clearing for a conflict park is keyed on the evidencing line's reference, which another attempt may hold. S2 makes the evidence a persisted line, not this run's line, but attribution remains approximate for `provider_reference_conflict`. **Ledger-finance rules whether this is acceptable or whether a per-park evidence row (a second `evidence_kind`) is required** (Q-LF-3). Default: accept and document; no extra table. |
| **Flip** | `d2NoCU(... "an unbound park with no line this run")` (`prh2_d2_parked_capture_integration_test.go:305`) is flipped to assert the standing finding. The ADR 0095 §35.4 "NOT IMPLEMENTED" wording and the `payment_statement.go:126-129` disclosed limit are rewritten by K3 (ledger-finance edits ADR 0095 §35). |
| **Gate effect** | The ADR 0095 §35.4 B1 gate item 1 (STANDING-1 + POLL-REF-CLEAR-1) is satisfied **against MOCK** once K3 merges. Item 2 (a **delivered** I-wire P1) still fails: ALERT-DELIVERY-1 is OPEN. The real-PSP gate therefore stays closed. |

### 9.3 The one persisted-line lookup

A single matcher helper (`loadPersistedEvidence`) queries `payment_statement_lines` across **all**
imports of `(tenant_id, provider_id)`. It uses the §8.6(b) indexes, for exactly the attempts and
references the run needs:
- the M2-resolved payouts (rules (c) and (d));
- the unbound and bound parks (S1–S4).

It runs in the stream's `WithTenantSnapshot` REPEATABLE READ transaction (`requireSnapshotIsolationFor`,
`payment_statement.go:588`), with an explicit `tenant_id = $1` predicate and RLS. It never reads
another tenant's lines (C-14d). Run cost grows with history, as every stream does
(CAS-RECON-SCALE-1).

`docs/architecture/reconciliation-model.md` gains the three kinds, the widened standing rule and
the Y clearing (edited by ledger-finance).

## 10. Proposed amendment text for ADR 0095 (the orchestrator applies it)

**§4.8, replace the M1 and M2 rows:**

| M | What | Governance | Status |
|---|---|---|---|
| M1 | An evidence-only resolution on a `disputed` **deposit**. The attempt stays `disputed`. No posting, no ledger link. Funds leave only via a PSP refund, or LEDGER-SUSPENSE-B-1 later. It never clears `pay_captured_unposted`. | ADR 0101 §3, §6 (ADR 0099 grants; ADR 0100 four-eyes; beneficiary guard) | DESIGNED; NOT IMPLEMENTED until K3 |
| M2 | For an `ambiguous`, or `disputed` with `provider_reference_mismatch` / `success_for_never_sent_attempt`, **payout** whose withdrawal is `submitted`: declare paid → `succeeded` + `Complete` under `platform-operator-declared:`; declare not paid → `declined` + `Fail`. `operator` evidence; an executed resolution in the same tx. Every other reason, including `amount_asset_mismatch` and `callback_amount_asset_mismatch` (PAYOUT-AMOUNT-DISPUTE-1), is excluded. | As M1 | DESIGNED; NOT IMPLEMENTED until K3 |

**§4.8 M3 row, append to Status (revision 3):** "No non-test caller exists. A governed caller for
held payouts of **closed** tenants is designed in PAY-CLOSED-TENANT-FUNDS-RESOLUTION-1 (ADR 0101
§19). Active-tenant M3 (KYC deny at T2, credential permanently gone) has no caller either; see
PAY-M3-STAFF-PATH-1."

**§4.3:** add **M2p** (`{ambiguous, disputed}` → `succeeded`, payout, operator) and **M2n** (→
`declined`), pointing to ADR 0101 §5.2 and §8.3.

**§28.9:** replace "or when M1/allocation occurs (BLOCKED)" with:

> "or, under HD-LEDGER-UNALLOC-1 (B), when an allocation posting exists. An M1 resolution never
> clears or suppresses the finding; it only annotates it as acknowledged (ADR 0101 §4, LF-3)."

**§35.4 (revision 3; ledger-finance edits the final text when K3 merges):** "Standing coverage for
unbound parks" and "Clearing on the poll's returned reference Y" move from NOT IMPLEMENTED to
IMPLEMENTED (MOCK) by ADR 0101 §9.2. The B1 gate's item 2 (delivered P1) is unchanged and still open.

**§2 INV-IO-7, append:**

> "Exception: M2 'declare not paid' (ADR 0101 §5.3), admitted only with `operator` evidence and an
> executed four-eyes resolution in the same transaction. Its late-success double-payout risk
> surfaces as T14 and as the standing reconciliation kind `pay_declared_not_paid_but_paid`."

**§12 kinds table:** add `pay_declared_paid_unconfirmed`, `pay_declared_not_paid_but_paid` and
`pay_declared_paid_compensated_but_paid` (§9.1).

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

Events: `payment.manual_resolution_requested` / `_approved` / `_rejected` / `_cancelled` /
`_expired` / `_executed` / `_refused`, plus the existing attempt and withdrawal events with
`evidence_kind = operator`. Acting rows follow ADR 0099 §10.6 (`audit_log_acting_actor`), and ADR
0104's tenant-visible projection shows platform-actor M1/M2 actions to the tenant (HD-PRH2-5).
**(Revision 3)** The Y evidence write adds `"evidence_recorded": true` to the existing
`payment.attempt_disputed` park audit. There is no new action and no reference value beyond what
`echoAuditMeta` already writes.

## 12. Tests and mutants (K3 DoD; LF K3 list, LF rev-2 tests 9–14, QA W1/W2/W4)

Notes:
- **Guard tests run on a HEAD-migrated scratch DB** (LF-18).
- T-1 clock; no wall-clock assertion; no sleeps (plan §5.0).
- **Every test asserts** SUM(D) = SUM(C), projection = recomputed, **M1 produces zero ledger
  transactions**, and **the 0107 index definitions are unchanged** (LF test 14).
- Concurrency tests run `-race`; the C-12 class runs `-count=50` (QA W3 precedent).
- Results are reported PASS / FAIL / FLAKE / NOT RUN / BLOCKED.

### 12.1 Revision-2 tests (IDs kept; references refreshed)

| ID | Class | Test |
|---|---|---|
| C-1 | ADV | A deposit `disputed` → any state is refused, even with an executed resolution, for every reason in `DepositDisputeTerminalReasons()` |
| C-2 | R | M1 on `multiple_success_for_intent`: no posting; `pay_captured_unposted` reported next run with unchanged ageing; acknowledged in the read model |
| C-3 | ADV | K2 link to a deposit resolution, or a ledger link on M1: refused |
| C-4 | R | M2 paid and not-paid, from `ambiguous` and from allow-listed `disputed`, withdrawal `submitted`: correct postings and keys |
| C-5 | ADV | M2 on T14 (withdrawal `failed`), on a deposit, on **`amount_asset_mismatch`** and on **`callback_amount_asset_mismatch`** (executor and guard; LF test 9): refused. `provider_reference_mismatch`, `success_for_never_sent_attempt` and `ambiguous` are admitted. |
| C-6 | ADV | A resolution for X used on Y; a target mismatch; reuse; `executed_txid ≠ txid_current()`; operator evidence without a resolution. All refused. |
| C-7 | R | A late success after "not paid" → T14, **and `pay_declared_not_paid_but_paid` standing across N runs, including runs whose coverage excludes the payout**. It clears only when recovery debits total the amount (LF test 10). P1 assertion per §12.3. |
| C-8 | R | A late success after "paid" → no second Step B. **A real matching line gives no `pay_reference_mismatch` and no missing-record finding; the confirmation metric increments** (LF test 10). |
| C-9 | ADV | **Reserved prefix at every ingress in the §5.4 table** (LF test 13; C-101-3): deposit sync; both deposit poll binding sites; the poll echo audit; payout sync; payout poll; verified callback (deposit and payout); legacy callback; webhook HTTP mapping; **statement fetch**. Also the DB CHECKs, and the all-sessions ledger trigger (the prefix on any other transaction type, or without an executing resolution, in a tenant session: refused). 0115 up refuses when a prefix exists. |
| C-10 | AZ | S-12 for M1 and M2 owners; unlinked staff Person: refused |
| C-11 | AZ | Self-approval, same Person, no grant, revoked or expired at execution (`FOR SHARE`), suspended actor, policy author as approver: refused or not counted |
| C-12 | CON | M2 vs sweeper (H's loop: payout poll-apply; T2/T12 for an active tenant) or callback success → exactly one outcome and at most one Step B. Concurrent final approvals → one execution. |
| C-13 | FL/RB | A failure in `Complete`/`Fail` rolls back everything; a committed `executing` is refused |
| C-14 | RLS | Tenant isolation; plain platform refused; acting only with the specific grant for that tenant |
| C-14b | RLS | **C-101-1:** an acting session UPDATEs `payment_attempts` with `callback` evidence → refused; an attempt or withdrawal UPDATE without an executing resolution → refused; a `deposit_intents` UPDATE → refused; the governed M2 path → succeeds |
| C-15 | AU | Audit rows, including tenant status |
| C-16 | MIG | 0115 up/down/up (HEAD-migrated). Down refuses with resolutions, new-kind rows or evidence rows. After down, `pg_get_functiondef(payment_attempts_guard)` equals 0107's, the fence and payload-refusal functions equal 0113's, the kind CHECK equals the previous definition, and the index definitions are unchanged. |
| C-17 | R | **F12:** a text diff of 0115's guard against 0107's equals exactly §8.3's (i) and (ii) |
| C-17b | R | **F12 (LF test 11): an exhaustive deposit matrix**, every `(OLD.state, NEW.state, last_evidence_kind, terminal_reason ∈ {NULL, each named reason, other})` for `operation = 'deposit'`, gives **identical accept/refuse** on a 0107-only DB and a 0115 DB |
| C-18 | R | No platform policy → M1 and M2 disabled |
| C-19 | R | **F13 (LF test 12; regression pin since D2 landed the text):** no reconciliation detail string says M1 clears, including the new kinds' and S1's strings |
| C-20 | R | A late provider decline after "declare paid": recorded, no state change; P1 assertion per §12.3 |
| C-21 | R | Non-active tenant (`suspended` and `closed`): M1 and M2 available; tenant status recorded; tenant-level tightening ignored for policy |
| C-22 | R | Compensation **annotates but does not clear** `pay_declared_paid_unconfirmed` across N runs. It clears on a confirming line. |
| C-23 | R | **C-4(a):** a non-prefixed `withdrawal_completed`, or a prefixed transaction of another type, is refused as causation (with a mutant) |
| C-24 | R | A causation pointing at another wallet's Step B is refused |
| C-25 | R | A debit with a Step B causation is refused |
| C-26 | CC | Two concurrent credits on one Step B: the cap holds under L2 |
| C-27 | R | A compensating credit without an evidence hash is refused |
| C-28 | R/AZ | **C-4(c):** a Person counted on the M2 resolution is refused as initiator or approver of its compensation, both at insert and at execution |
| C-29 | R | Compensation, then a confirming line: `pay_declared_paid_unconfirmed` clears; `pay_declared_paid_compensated_but_paid` is raised; a partial recovery does not clear it; a full recovery does |
| C-30 | INV | SUM(D) = SUM(C), and projection = recomputed, throughout C-22..C-29 |
| C-31 | RLS | **Security C-3:** `withdrawal.Complete`/`Fail` under an acting session succeed **only** inside the governed M2 transaction. Every table they touch is covered by an acting policy or a fence. |

### 12.2 Revision-3 additions

| ID | Class | Test |
|---|---|---|
| C-5b | PIN | Every payout dispute write site in `internal/payments` (static scan, as `DepositDisputeTerminalReasons` does for deposits) uses a reason that is classified as M2-admitted or M2-refused in one table. A new payout reason fails the pin until this ADR classifies it. |
| C-9b | R | `ValidatePaymentReference` wraps `Validate` exactly. Every `providerref` reason still fires, plus `ReasonReservedNamespace`. Casino and sportsbook still call the shared `Validate` (static pin). |
| C-9c | MIG | Catalogue-driven pin: every `provider_reference`-like column in `public` carries the reserved-prefix CHECK, or is listed as exempt with a reason |
| C-12b | CON | **S-11 races:** grant revoked, or the approver suspended, between approval and execution (refused/not counted); the resolution expires between the last approval and execution (refused); a K2 compensation and an M2 execution on the same wallet concurrently (the lock order holds, no deadlock under `-race -count=50`) |
| C-14c | RLS/AZ | **S-4:** a suspended actor with an unexpired JWT is refused; an actor demoted mid-token is refused (the DB re-read, not the JWT) |
| C-14d | RLS | Two tenants with the same provider and the same merchant and provider references: the §9.3 lookup, S1–S4 and rules (c)/(d) never see the other tenant's lines or evidence (the PAY-RECON-D2-HARDENING-1 (2) shape for K3's own code) |
| C-32 | AZ | **Sock-puppet (QA W1, HD-PRH2-2 (c)):** a grant to a tenant-minted account without platform co-approval is refused; the same Person under two principals cannot be requester and approver; under (c) a tenant admin cannot approve a `payment_force_resolve` grant |
| C-33 | ADV | M2 refused for `invalid_provider_reference:*`, `reversal_tombstone_precedes_success` and each `late_*` reason; the hold is unchanged (part of PAY-PAYOUT-UNBOUND-HOLD-1's "never automatically released" assertion, for the manual path only) |
| C-34 | R | **S1:** an unbound park with a merchant-resolved succeeded line in import 1, and no line in imports 2..N → `pay_captured_unposted` on every run (the flipped `d2NoCU`). **S2:** a reversal in import k clears it from run k on. M1 executed: still reported (LF-3). |
| C-35 | R | **S3 (TestD2_14 shape):** the reversal line naming X in import 1 clears the bound standing finding in runs 2..N |
| C-36 | R | **S4:** a `poll_reference_mismatch` park with Y evidence: reversal or tombstone on Y clears; on an unrelated reference it does not; without a Y row only X clears; a Y never comes from audit JSON (mutant: read `echoed_provider_reference` → killed) |
| C-37 | R/FL | Y evidence write: written only for a valid Y; in the park transaction (fault injection after the evidence insert → nothing persists, as ADR 0095 §36.7 QA C3); a prefixed Y is refused (CHECK and validator); UPDATE/DELETE refused; a duplicate is idempotent only if equal |
| C-38 | R | **T15i:** `success_for_never_sent_attempt` bound-if-referenced integration test (S5) |
| C-39 | MIG | The 0077 exact whitelist stays at 11 tuples; the `jurisdiction/migration_0075`/`0077` full-chain rollbacks pass with 0115 present; `migration_0097/0098` constraint-name tests pass; I-wire `static_wiring_test.go` passes |
| C-40 | RLS | K3's own acting-policy probe list (the `internal/adjustment/acting_policies_integration_test.go` pattern, in a new payments test file): each new acting policy is allowed with a valid grant and denied without one |

**Mutants:**

| Mutant | Killed by |
|---|---|
| drop `executed_txid = txid_current()` | C-6 |
| drop the attempt binding | C-6 |
| allow deposit `disputed →` | C-1, C-17b |
| drop the target match | C-6 |
| drop the withdrawal `submitted` re-check | C-5 |
| drop the allow-list, or admit `amount_asset_mismatch` / `callback_amount_asset_mismatch` | C-5, C-5b |
| drop the beneficiary guard for M1 or M2 | C-10 |
| let M1 clear `pay_captured_unposted` (bound, unbound or S1) | C-2, C-34 |
| accept the prefix at any one ingress | C-9 |
| use `LIKE` instead of `left()` with a `_`/`%`-bearing value | C-9 |
| drop the deferred check | C-13 |
| drop the `operator` term from the acting WITH CHECK | C-14b |
| let `pay_declared_not_paid_but_paid` clear on a partial recovery | C-7 |
| skip the amount comparison for a reserved-prefix Step B | C-8 |
| **(rev 3)** window S1 to the current import | C-34 |
| **(rev 3)** S4 reads Y from audit JSON, or accepts any reference | C-36 |
| **(rev 3)** drop `tenant_id` from the persisted lookup | C-14d |
| **(rev 3)** grant-function removed from a K3 acting policy | C-40 |
| **(rev 3)** use the shared `Validate` at one payments ingress | C-9, C-9b |

### 12.3 What the P1 tests can assert today (revision 3; I-wire merged, ALERT-DELIVERY-1 OPEN)

I-wire has merged (`dcaa2c6`). The dispatcher runs with the **log sink only**. No route or
recipient exists (HD-PRH2-4-OPS), so every alert is `unrouted`. **Payout disputes, including T14,
raise no alert** (§1). K3's P1 tests can therefore assert exactly this, and must not claim more:

| Test | Asserted today | Not assertable (ALERT-DELIVERY-1 OPEN) |
|---|---|---|
| C-7 (T14 after "not paid") | (1) the attempt is `disputed` / `success_after_payout_declined`, with its audit; (2) `pay_declared_not_paid_but_paid` rows on every run; (3) **one open durable alert** of kind `reconciliation.payment_statement_mismatch`, discriminator `stream:payment_statement:provider:<id>`, raised post-commit by `scheduler.go:751`. It is in delivery state `unrouted` and visible to `alert:manage`. The `alerting.unrouted` warning is raised. | delivery to a human or channel; escalation to a recipient |
| C-20 (decline after "paid") | the audit row and no state change; `pay_declared_paid_unconfirmed` keeps standing; with a declined statement line, `pay_status_mismatch` and the same durable `unrouted` reconciliation alert | as above |
| §9.1 (c2) | `pay_declared_paid_compensated_but_paid` rows and the same durable `unrouted` alert | as above |
| C-34/C-35/C-36 | `pay_captured_unposted` rows and the same alert | as above |

- **The standing kinds surface only where the payment_statement stream runs**, that is, for a
  provider with a registered statement source (MOCK today). An M2 on a provider with no statement
  source produces resolutions and audit, but no standing mismatch and no alert. This is a disclosed
  residual (§21 R-K3-5). The runbook makes "statement source registered" a precondition of using M2.
- **K3 adds no new alert Kind and no in-tx raise.** A dedicated payout-dispute/T14 alert is not
  in K3. It is the existing gap "payout disputes not alerted" (ADR 0102 §17.8), proposed for
  registration as PAY-PAYOUT-DISPUTE-ALERT-1 (§22, Q-SEC-3). This keeps K3 off `internal/alerting/kind.go`, an
  E1 file, and away from the FORCE-lift seeding pattern.
- No K3 document or test may say an alert is "delivered" or that someone is "paged".

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
  - UNIQUE `ledger_transaction_id`;
  - one Y evidence row per attempt and kind.
- **RLS:** T and A families only on the resolution tables; T only on the evidence table; acting
  updates gated by an executing resolution.
- **No direct balance mutation:** only withdrawal postings.
- **HD-LEDGER-UNALLOC-1:** A unchanged; B deferred.
- **INV-IO-7:** a single governed exception (§10). **INV-IO-9 / M3:** unchanged.
- **INV-IO-12:** reconciliation still writes only its run and mismatch rows. The Y evidence row is
  written by payments, not by reconciliation.

**Introduced:**

| ID | Invariant |
|---|---|
| INV-M-1 | No deposit attempt leaves `disputed` |
| INV-M-2 | An operator-evidence payout terminal exists only with an executed resolution from the same transaction, an allow-listed reason, and a `submitted` withdrawal |
| INV-M-3 | The reserved prefix appears only on M2 Step B postings |
| INV-M-4 | Every executed M2 is either confirmed or standing in reconciliation (where the stream runs, §12.3) |
| INV-M-5 (rev 3) | A captured-unposted exposure, once evidenced by any persisted statement line, stays reported until its clearing signal exists (no silent drop-out) |
| INV-M-6 (rev 3) | Clearing evidence is read only from ledger rows, persisted statement lines and the typed evidence table, never from audit JSON |

## 14. Alternatives rejected

| Alternative | Why rejected |
|---|---|
| A `resolved_no_action` deposit state | LF-1 |
| An M1 credit via K2 | LF-2 |
| M1 sets `investigation_status` | LF-3 |
| Declare paid under the real or an empty reference | Collision, or empty-key refusal |
| M2 on T14 | LF-15 |
| **M2 on `amount_asset_mismatch` or `callback_amount_asset_mismatch`** | F9. They go to PAYOUT-AMOUNT-DISPUTE-1. |
| **The prefix refusal inside the shared `providerref.Validate`** | It would change casino and sportsbook semantics (F14). A payments-only validator plus DB backstops is used instead. |
| **`LIKE` for the prefix** | `_` is a wildcard (F2(d)). `left()` is used. |
| Approve now, execute later | LF-13 pattern |
| Edit `withdrawal.go` | Payments F3 |
| **(rev 3)** Persist Y as a `payment_attempts` column | Changes the row the guard governs. Immutability of the new column would need a guard edit outside the F12 diff. A separate append-only table keeps F12 exact. |
| **(rev 3)** Read Y from `echoed_provider_reference` in audit JSON | LF ruling: reconciliation never parses audit JSON as money evidence |
| **(rev 3)** Fold the closed-tenant funds path into K3 | §19 |
| **(rev 3)** A new alert Kind for payout T14 in K3 | §12.3: scope, the E1 file overlap, and the FORCE-lift seeding pattern |

## 15. Open items

1. **Ledger-finance: CONFIRMED (revision 2).**
   - The ruling 1 vs 5(c) resolution is ADOPTED with C-4, and the compensation does not clear (§9.1(c), (c2)).
   - The full-amount clearing tightening in §9.1(d) is confirmed.
2. **`payments` confirms the basis and context vocabulary** (§5.1).
3. **Follow-ups:** PAYOUT-AMOUNT-DISPUTE-1 (amount disputes); WITHDRAWAL-REVERSAL-1 (the
   `psp_clearing` correction, F11).
4. **Touches (§7):** the orchestrator records the revision-3 list.
5. **ADR 0095 amendments (§10) and ADR 0082 A8** (ADR 0100 §8): the orchestrator applies them.
6. **Revision 3:** every question in §22 must be answered before K3 code.

**HUMAN DECISION REQUIRED:** none new **for K3 itself**. HD-PRH2-8 (ADR 0100 §3.4) also governs
`payment_force_resolve`; the interim is at least one independent approver. HD-PRH2-9 creates
separate human decisions, listed in the §19 design (HD-CTF-*), which do not block K3.

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
| F9 (HIGH) | §5.1 allow-list; `payment_m2_admits`; C-5, C-5b; PAYOUT-AMOUNT-DISPUTE-1 |
| F10 (HIGH) / ruling 5 | §9.1 (a)–(e); §8.4 kind CHECK widened in 0115; C-7, C-8, C-22 |
| F11 | §5.3 residual row; WITHDRAWAL-REVERSAL-1 |
| F12 | §8.3 exact diff; C-17, C-17b |
| F13 | §4 (landed by D2); C-19 |
| F14 | §5.4 (payments validator at every ingress); C-9, C-9b |
| F16 | §3 (deferral concurred) |
| F17 | §5.3 (keys and releases as verified) |
| Ruling 1 (finding and basis codes) | §3; §5.1; §8.1 |
| Ruling 6 | §6.3; ADR 0100 §8 |
| LF rev-2 tests 9–14 | C-5, C-7/C-8, C-17b, C-19, C-9, every test |
| **QA** F4 | §8.2 RLS paragraph; §8.6(a) |
| **Preflight** (`e1-k3-preflight.md` §2) | §1 facts; §7 (poll_evidence.go, deposit_handlers.go); §12.3 (I-wire); §18; §20 |
| **Orchestrator** | Registry ids used: PAYOUT-AMOUNT-DISPUTE-1, WITHDRAWAL-REVERSAL-1, plus the revision-3 ids in §18–§22 |

## 17. Handover / DoD

K3 updates:
- `docs/architecture/withdrawal-state-machine.md` (M2; the reserved namespace; the risks and
  residuals);
- `payment-orchestration.md` (M1/M2; operator evidence; the allow-list; the payout dispute reason
  table of §5.1);
- `reconciliation-model.md` (LF-3 acknowledgement; the three new kinds; the M2 matching rules; the
  folded S1–S4; edited by ledger-finance);
- `docs/security/security-architecture.md` (S-12; C-101-1; the reserved namespace);
- `backoffice/src/auth/permissions.ts`;
- `docs/runbooks/operational-runbooks.md`, entry "Payment force-resolution (M1/M2)":
  - T17 first; basis and evidence requirements;
  - which reasons are not M2-resolvable (`amount_asset_mismatch`, `callback_amount_asset_mismatch`,
    `invalid_provider_reference*`, `late_*`, tombstone) and where they go;
  - declared-paid-unconfirmed and declared-not-paid-but-paid recovery, and the `psp_clearing`
    residual;
  - M1 never clears `pay_captured_unposted`, including a standing `poll_reference_mismatch`;
  - non-active tenants (suspended, closed) and the pointer to the closed-tenant queue (§19), once
    that exists;
  - precondition: a statement source registered for the provider (§12.3);
- `docs/runbooks/observability-and-alerting.md` (the three kinds are surfaced through
  `reconciliation.payment_statement_mismatch`; T14 has no direct alert; ALERT-DELIVERY-1 OPEN;
  nothing is delivered);
- ADR 0095 §35 (ledger-finance, §10);
- an implementation record in this ADR (commit, test results PASS/FAIL/FLAKE/NOT RUN/BLOCKED,
  surviving mutants classified).

The orchestrator updates: ADR 0095 (§10); the registry (§23); the HANDOVER index and mock-vs-real
matrix; plan §3/§12 Touches; the review records.

### 17a. Revision 3 change log

| Item | Revision 2 | Revision 3 |
|---|---|---|
| Migration number | 0114 | **0115** (plan §11) |
| The K2 fence and kind CHECK base | "0112" | **0113** (`0113:1426`, `:1850`) |
| F13 detail strings | to change | **already changed by D2**; regression pin only |
| Line references | `cabca27` / `6864efa` | re-verified at `3517980` (§1) |
| Payout reasons refused by M2 | `amount_asset_mismatch` + "any other" | the HEAD list made explicit, including `callback_amount_asset_mismatch` (§5.1); pin C-5b |
| Ingress list | 6 sites | 10 sites, including `poll_evidence.go`, the two `sweeper.go` binding sites, `orchestrator.go:1003` and `deposit_handlers.go` (§5.4) |
| Persisted evidence | "any persisted import" (unspecified) | one lookup and three indexes (§8.6(b), §9.3) |
| STANDING-1 / POLL-REF-CLEAR-1 | follow-ups | **folded** (§18, §8.6, §9.2) |
| P1 claims | "P1 raised" | restated as durable, `unrouted`, undelivered (§12.3) |
| HD-PRH2-9 | — | separate workstream (§19) |

## 18. Scope decisions (revision 3)

The rule (CLAUDE.md "No uncontrolled scope expansion"; ADR 0105 §4): **fold an item into K3 only if
it is directly within K3's reconciliation/force-resolution scope and shares K3's substrate** (the
`payment_statement_lines` persisted-evidence lookup, the mismatch-kind CHECK, the 0115 schema).
Otherwise it stays separately registered with an owner, a dependency and its launch-prerequisite
status. Nothing is silently closed.

| Item | Decision | Reasoning |
|---|---|---|
| **PAY-RECON-PARKED-CAPTURE-STANDING-1** (standing detection of parked captures: unbound coverage, persisted-reversal clearing, T15i) | **FOLD** | (1) It is reconciliation of `pay_captured_unposted`, the finding K3's LF-3 rule governs. K3 must already annotate it as M1-acknowledged. (2) The registry and ADR 0095 §35.4 both say its route is "persisted `payment_statement_lines` evidence, the same approach as LF ruling 5(c)(d)", which is exactly K3's §9.1(c)/(d) "any persisted import" rule. Building the lookup once (§9.3) avoids two implementations of one money-evidence query. (3) Same file (`payment_statement.go`), same indexes (§8.6(b)), and the same `d2NoCU` gap test. Landing it as 0116 would rebase K3's matcher a second time. (4) It is a HARD prerequisite before any real PSP (ADR 0105 §4). Folding does not relax it. **Cost:** adds S1–S3, S5 and the tests C-34, C-35, C-38. **No new kind, no new table.** |
| **PAY-RECON-POLL-REF-CLEAR-1** (persist Y, clear on X or Y) | **FOLD** | (1) The LF ruling B3 binds it to ship **with STANDING-1 under ONE LF-signed schema change**. Folding STANDING-1 without it would break that ruling. (2) Its write site is `poll_evidence.go`, which K3 already touches for the reserved-namespace ingress (§5.4). Its read site is `payment_statement.go`'s standing clearing, which K3 already touches. (3) The new table is K3's 0115 schema and gets the §5.4 reserved-prefix CHECK. **Cost:** one table (§8.6(a)), one write, rule S4, the tests C-36 and C-37. Binding before the first real PSP or non-MOCK source, whichever comes first: satisfied against MOCK when K3 merges. |
| **PAY-RECON-D2-HARDENING-1** ((1) loud failure for an unclassified disputed reason; (2) two-tenant matcher pin) | **DO NOT FOLD** (one test overlaps) | (1) is a classification-policy change on `disputeReasonClasses` (default bound-if-referenced, or a dedicated check). It changes existing assertions (`TestD2_6`), needs LF's own ruling, and uses no K3 substrate. (2) is a test on D2's existing queries. K3 adds **C-14d** for its *own* new lookup only, which does not close D2's item. **Stays:** owner ledger-finance (review security); Low; optional before the first real PSP; it lands **after K3** because K3 owns `payment_statement.go` until merge. |
| **MA020-SYNC-MISMATCH-1** (widen `player_open_payment_exposure`) | **DO NOT FOLD**; sequence immediately after K3 | (1) MA020 is K2's **preventive manual-adjustment** check (ADR 0100 §5.2), not reconciliation and not force-resolution. (2) It depends on K3's substrate: its `poll_reference_mismatch` clause "clears on a tombstone on the bound or the returned reference" needs Y, which exists only after 0115 (§8.6(a)). So it must follow K3, not precede it. (3) No body conflict: MA020 edits `player_open_payment_exposure` (`0113:706`), while K3 edits `ledger_adjustment_payload_refusal` and the fence. They are different functions, but both are K2-family CREATE OR REPLACE work, so the one that lands second builds on the other's body. **Stays:** owner ledger-finance; **before the first real-money tenant**; migration allocated at merge (nominally 0116 or later, after K3, Rule 3). **Open ruling Q-LF-2 / Q-SEC-2 (§22): should MA020 block an M2 compensating credit?** K3's interim: unchanged (fail closed; MA020 applies to every credit, including the Step B arm). |
| **PAY-PAYOUT-UNBOUND-HOLD-1** (a payout parked unbound keeps its hold; the payout meaning of the finding; payout-side F-C4) | **DO NOT FOLD**; K3 contributes C-33 | (1) Its core is the payments dispatch and park path (`payout.go` phase C, a payout F-C4 pre-check) and an ADR 0095 §35.2 definition. That is not force-resolution. (2) K3's only overlap is the manual path. M2 refuses every unbound payout reason (§5.1), and **C-33** pins that the hold is untouched by M2. That is a partial contribution to "never released automatically", not closure. **Stays:** owner payments (review LF); binding before the first real PSP and the D2 B1 gate; lands **after K3** (shared `payout.go`). |
| **PAY-PAYOUT-REFBIND-1** (payout phase C LF-6 binding pre-check; the scrub at `payout.go` ~1243) | **DO NOT FOLD** | Payout dispatch-time reference binding is outside force-resolution. Its only K3 contact is that the reserved-prefix key can never be a valid payout reference (§5.4), so the two cannot collide. The `payout.go:1242` validator switch in K3 touches the same function as the F-L3 scrub, so REFBIND rebases on K3. **Stays:** owner payments (review security); Low; PROVIDER DEPENDENT; binding before any real payout provider; lands **after K3**. |

### 18.1 Ordering versus MA020 and the K2 widening

```
E1 (0114) ─► K3 (0115: force-resolution + STANDING-1 + POLL-REF-CLEAR-1) ─► MA020-SYNC-MISMATCH-1 (next free number)
                                                                         ─► PAY-PAYOUT-UNBOUND-HOLD-1, PAY-PAYOUT-REFBIND-1, PAY-RECON-D2-HARDENING-1 (no migration expected)
                                                                         ─► PAY-CLOSED-TENANT-FUNDS-RESOLUTION-1 (next free number; §19)
```

- MA020 after K3, because it needs Y (§8.6(a)), and because K3's 0115 down restores 0113's
  `ledger_adjustment_payload_refusal`. If MA020 also replaced that function first, K3's down
  target would move.
- If the orchestrator instead lands MA020 **before** K3 (for example to unblock a real-money tenant
  earlier), MA020 ships **without** the Y clause (bound reference only, fail-closed noisy), K3
  renumbers (Rule 3), and 0115 widens `player_open_payment_exposure` with the Y clause. That is
  the orchestrator's call; architect recommends K3 first.

### 18.2 What K3 does not change in K2

K3 does not touch `player_open_payment_exposure`, K2's Go executor, K2's reason catalogue rows, or
the K2 policy evaluator. The `payment_force_resolve` special case at `0113:655` is already there.

### 18.3 MA020 versus the M2 compensating credit (flagged; not decided here)

- **Today (0113:886):** every `credit_player` adjustment is refused while
  `player_open_payment_exposure` is true, **with no override**. That includes a K3 Step B
  compensating credit restoring a player whose payout was declared paid but never paid.
- **Arguments for keeping MA020 on that arm (fail closed):** MA020 exists so that no credit can
  substitute for an unposted capture. An exposure open on the same player means the platform's
  view of that player's money is already uncertain.
- **Arguments for exempting it:** the Step B arm is capped at the hold leg and bound to one executed
  M2 Step B with evidence. It is not a hand-pay of a deposit capture, and blocking it strands a
  player the platform itself declared paid.
- **K3 interim:** keep MA020 as is (no exemption). **Ruling required from ledger-finance
  (financial invariant) and security before K3 code** (Q-LF-2, Q-SEC-2). If they rule an
  exemption, it is one `AND NOT <step-b-arm>` term in 0115's `ledger_adjustment_payload_refusal`,
  plus a test and a mutant.

## 19. HD-PRH2-9 (closed-tenant player funds): disposition

**Decision: a SEPARATE registered workstream, PAY-CLOSED-TENANT-FUNDS-RESOLUTION-1. It is NOT
folded into K3.** The design is in
`docs/plans/prh2-hardening-round/designs/pay-closed-tenant-funds-resolution-1.md`. It becomes an ADR
when the orchestrator allocates a number; it is not given one here, to avoid colliding with E1's ADR
(HD-PRH2-11).

Reasons (no-scope-expansion rule):
1. **Different subject.** K3 resolves a *payment attempt* (`payment_manual_resolutions.attempt_id`
   NOT NULL). The closed-tenant path resolves a *withdrawal hold*, including `requested`,
   `pending_review` and `approved` withdrawals that have **no attempt**, and the never-sent
   `created` attempt (M3), which is not M1 or M2.
2. **New configuration substrate.** HD-PRH2-9 requires a **configurable, jurisdiction-aware** set
   of permitted outcomes. That is a new rule table and change governance, with no counterpart in
   K3.
3. **New privileged power, new threat model.** A platform principal releasing or dispatching funds
   in a closed tenant needs its own security review (the requester scope for a closed tenant, a new
   capability pair, a new operation-kind classification).
4. **Touches K3 must not.** It needs a governed release from `approved`/`requested` (a new
   `withdrawal.go` function; K3 has "no edits to `withdrawal.go`", payments F3), and an optional
   dispatch permit consumed by H's T2 gate (`payout_sweep.go`, an H file).
5. **Legal inputs pending.** The permitted outcomes per jurisdiction are human/legal decisions
   (HD-CTF-*). K3 must not wait on them.
6. **K3 is already large** (M1/M2 plus two folds).

**What K3 already gives a closed tenant** (unchanged): M2 is available for non-active tenants
(§5.1, C-21). So a closed tenant's `ambiguous` or allow-listed `disputed` payout already has a
governed path after K3. The **new** workstream covers what K3 does not: never-sent `created`
payouts (M3), pre-dispatch holds, and the configurable outcome set.

**Until it is implemented:** the H residual stands (ADR 0095 §37.5). Held funds of a closed tenant
have no automated or governed release, and this **is a launch blocker for any tenant-closure flow**
(ADR 0105 §1). Architect recommends an interim operational control for the orchestrator/human:
no tenant is set to `closed` while it has withdrawals in a hold-bearing state (runbook; optionally
a DB guard in the new workstream). This is a recommendation, not a requirement.

## 20. Migration 0115 plan, file ownership and E1 coordination

### 20.1 0115 object list (summary of §8)

| Group | Objects | RLS / grants |
|---|---|---|
| Reference | `payment_manual_resolution_codes` (7 rows) | FORCE RLS; `reference_read` SELECT; grant SELECT |
| Force-resolution | `payment_manual_resolutions`, `payment_manual_resolution_approvals`; functions `payment_reserved_ref_prefix`, `payment_m2_admits`, the beneficiary guard, the state machine, the deferred check | FORCE RLS; families T + A; no P; grants SELECT/INSERT/UPDATE (resolutions) and SELECT/INSERT (approvals); no DELETE; TRUNCATE denied |
| Folded evidence | `payment_attempt_reference_evidence`; three `payment_statement_lines` indexes | FORCE RLS; family T only; grant SELECT/INSERT; append-only triggers |
| Altered | `payment_attempts_guard()` (§8.3 diff); reserved-prefix CHECKs; `ledger_transactions_reserved_prefix_guard`; `ledger_governed_fence_allows` (a)+(b)+(c); `ledger_adjustment_payload_refusal` (+ Step B arm); acting UPDATE policies on `payment_attempts`, `withdrawal_requests`, `deposit_intents`; acting SELECT on `withdrawal_requests`; the kind CHECK (+3) | as §6.4 |
| Up-time refusal | any reserved-prefix value present | — |
| Down | refuses with resolutions, new-kind rows or evidence rows; restores 0107 guard, 0113 functions and CHECK; must pass on an empty scratch DB | — |

**`deploy/init-app-role.sql`:** one new block, **appended after E1's block** (§20.3), in K2's
`DO $$ … FOR t IN SELECT * FROM (VALUES …)` loop pattern (`init-app-role.sql:398-418`):

```
('payment_manual_resolution_codes', 'SELECT'),
('payment_manual_resolutions', 'SELECT, INSERT, UPDATE'),
('payment_manual_resolution_approvals', 'SELECT, INSERT'),
('payment_attempt_reference_evidence', 'SELECT, INSERT')
```

Rule 4 applies: append-only, no role or attribute change, and the migration's in-file grant block
matches exactly. CLAUDE.md environment safety applies: no `ALTER ROLE`, no password or superuser
action; if DB access fails, the implementer stops and reports.

**Exact-policy and whole-schema tests 0115 must keep green (or update deliberately):**

| Test | Effect |
|---|---|
| `internal/jurisdiction/migration_0077_integration_test.go:167-192` (11 exact tuples on `tenants`/`licences`/`jurisdictions`) | **No change**: K3 adds no policy on those tables. The tenant status is read through K2's existing path (`financial_policy_required_approvals` OUT `tenant_status`). Should any K3 need arise, the new tuple is added to the whitelist **with security sign-off** in the same commit. C-39. |
| `jurisdiction/migration_0075_*` (`:508`, `:688`, `:760`) and `0077` full-chain rollbacks (`:256-277`, count derived) | 0115 down must succeed on an empty scratch DB |
| `internal/reconciliation/migration_0097/0098_integration_test.go` (`:125`, `:117`) | match the constraint **name** `reconciliation_mismatches_mismatch_kind_check`, so it is kept |
| `internal/adjustment/migration_0113_integration_test.go` (scratch through 0113, `schemaSnapshot`) | unaffected (runs at 0113) |
| `internal/adjustment/acting_policies_integration_test.go` (K2's probe list) | unchanged; K3 adds its own probe list (C-40), not edits to K2's file |
| HEAD-migrated K2 tests that exercise the fence (`b11_b17_*`, `layered_*`, `internal/db/k2_gates_integration_test.go`) | branch (a) must behave identically; any test asserting the 0113 fence's exact refusal set is updated **only with LF agreement** |
| `internal/payments/migration_0101_integration_test.go` (`:293`, `:389`, `:554`) and `migration_0107_integration_test.go:406` (HEAD) | must still pass unchanged (no executing resolution in them) |
| `payout_dispatch_integration_test.go:342`, `poll_amount_integration_test.go:955`, `payout_dispatch_fixround_test.go:622` | Validate-shaped expectations: the payments validator keeps the field names and error type, so no change is expected. If one changes, it is a deliberate, reviewed update. |
| `prh2_d2_parked_capture_integration_test.go:305` (`d2NoCU`) | **flipped by the fold** (§9.2) |
| I-wire `internal/alerting/static_wiring_test.go` | K3 adds no `RaiseGuarded` |
| `internal/alerting/migration_0110_integration_test.go` (`TestMigration0110_KindsSeeded`) | K3 adds no Go Kind, so unaffected |

### 20.2 Numbering and merge order

- **Merge order: E1 (0114), then K3 (0115).** Gap-free (Rule 3). The objects are disjoint (E1:
  `kyc_submission_outbox`, an alert-kind seed, platform-service policies; K3: §20.1), so there is
  no technical ordering constraint, only numbering.
- If E1 slips and K3 is ready first, the orchestrator either holds K3 or swaps the numbers
  (K3 → 0114, E1 → 0115). K3 never merges with a gap.
- 0115 down restores "the definition immediately preceding at merge" of the kind CHECK and the two
  K2 functions. C-16 verifies this against a snapshot captured on a scratch DB migrated to N-1, not
  against a hard-coded 0113 text, so an intervening migration cannot make the down silently wrong.

### 20.3 Shared files with E1 (E1 designs in parallel)

| File | E1 | K3 | Conflict avoidance |
|---|---|---|---|
| `deploy/init-app-role.sql` | appends its 0114 block | appends its 0115 block | K3's block goes **at end of file, after E1's**. On rebase after E1 merges, K3 re-appends below E1. Neither edits an existing block. |
| `cmd/platform-api/main.go` | 1 wiring line (KYC worker) | **no edit** (K3 has no loop; routes register in `httpserver/routes.go`) | none |
| `internal/config/config.go` | possible interval setting | **no edit** (K3 has no config) | none |
| `internal/alerting/kind.go` | new KYC Kind (HD-PRH2-10) | **no edit** (§12.3) | none |
| `internal/db/platform_service.go` | new KYC worker identity (HD-PRH2-11) | **no edit**: K3 uses the acting family, not a platform service | none |
| `internal/kyc/**` | owner | **no edit**: M2 applies no KYC gate (§5.1, Q-IC-1) | none |
| `internal/auth/permission.go` | not expected (E1 uses a platform-service identity) | adds the `payment_force_resolve` permissions | If E1's design adds a permission, E1 merges first and K3 rebases. Each appends its own `Perm*` constants and role-map lines; neither reorders the other's. |
| `migrations/` | 0114 | 0115 | numbering only |

E1 and K3 run in parallel **in design** only. K3 implementation starts after this revision is
confirmed, and K3 merges after E1.

## 21. Threats and residuals (K3)

| ID | Threat / residual | Control / status |
|---|---|---|
| T-K3-1 | A governed actor forges a payout terminal without a provider | four-eyes with LF-11 and S-12; the executing-resolution binding in the guard and the acting WITH CHECK; the reserved namespace; standing reconciliation kinds |
| T-K3-2 | A real reference collides with a declared-paid key | the payments validator at 10 ingresses, DB CHECKs, the all-sessions ledger trigger, the up-time refusal |
| T-K3-3 | M1 used to hide a captured-unposted exposure | LF-3: the emission predicate never reads resolutions (C-2, C-34) |
| T-K3-4 | Cross-tenant evidence bleed through the new cross-import lookup | `tenant_id` predicate plus RLS plus the snapshot transaction (C-14d) |
| T-K3-5 | Y evidence spoofed through a hostile echo | written only after `ValidatePaymentReference`; reserved-prefix CHECK; append-only; per-attempt UNIQUE; used only to *clear* when a PSP reversal or tombstone on Y exists (a ledger or statement fact) |
| R-K3-1 | Double payout after "declare not paid" + late success | detected (T14 state plus `pay_declared_not_paid_but_paid`); **no direct alert**; recovery is a K2 debit or off-platform |
| R-K3-2 | `psp_clearing` misstated after an unconfirmed "declare paid" | WITHDRAWAL-REVERSAL-1 |
| R-K3-3 | Amount-disputed payouts have no path | PAYOUT-AMOUNT-DISPUTE-1 |
| R-K3-4 | Nothing is delivered to a human | ALERT-DELIVERY-1 OPEN; HD-PRH2-4-OPS |
| R-K3-5 | Standing kinds need a running payment_statement stream for the provider | runbook precondition; registry note |
| R-K3-6 | N1 conflict-park attribution stays approximate | Q-LF-3 |
| R-K3-7 | S-1: distinct Person does not prove two humans under the unverified identity model | HD-PRH2-2 (c) platform co-approval; disclosed in ADR 0099 |
| R-K3-8 | Unbound payout parks (`invalid_provider_reference*`) keep their hold with no resolution path | PAY-PAYOUT-UNBOUND-HOLD-1; for a closed tenant, also the §19 workstream (it may list them as "retain") |

## 22. Open questions (must be answered before K3 code)

| ID | To | Question |
|---|---|---|
| Q-LF-1 | ledger-finance | Confirm the fold of STANDING-1 + POLL-REF-CLEAR-1 into 0115 as **the** single LF-signed schema change (§8.6, §9.2), including S1–S5 wording and the Y-table shape (a separate table, not a column) |
| Q-LF-2 | ledger-finance | Should MA020 refuse a K3 Step B compensating credit (§18.3)? Interim: yes (unchanged) |
| Q-LF-3 | ledger-finance | N1: accept persisted-line attribution for conflict parks, or require a per-park evidence row (§9.2)? |
| Q-LF-4 | ledger-finance | Confirm `callback_amount_asset_mismatch` on a payout is refused by M2 and belongs to PAYOUT-AMOUNT-DISPUTE-1 (§5.1) |
| Q-LF-5 | ledger-finance + devops | The non-concurrent index build on `payment_statement_lines` in 0115 (§8.6(b)) |
| Q-SEC-1 | security | Confirm the revision-3 ingress list (§5.4) and the C-9c column-catalogue pin as complete |
| Q-SEC-2 | security | The MA020 versus Step B arm question, jointly with Q-LF-2 |
| Q-SEC-3 | security | Accept that K3 adds no payout T14 alert, with PAY-PAYOUT-DISPUTE-ALERT-1 registered (§12.3), or require it inside K3 (it would then touch `alerts.go`/`receipt.go` alert sites and possibly `kind.go`, an E1 file, so it would sequence after E1) |
| Q-SEC-4 | security | The Y evidence table: is family T only (no acting read) sufficient (§8.6(a))? |
| Q-IC-1 | identity-compliance + LF | The payout KYC gate does not apply to M2 (§5.1) |
| Q-PAY-1 | payments | The basis/context vocabulary (§5.1); whether `deposit_intents`/`withdrawal_requests.provider_reference` need the prefix CHECK (§5.4); a composite key `(id, tenant_id)` on `payment_attempts` for the evidence FK (§8.6(a)) |
| Q-POP-1 | product-owner-proxy | Concur with the two folds and four non-folds (§18) |

No threshold, recipient, retention period or legal rule is set by this ADR.

## 23. Registry and HANDOVER wording (proposed; orchestrator writes)

See the K3 design hand-off. The orchestrator is the single writer of `task-registry.md`,
`HANDOVER.md`, `progress.md` and `active-stage.md`.
