# ADR 0101 — Payment force-resolution M1/M2 (PRH-2 K3; amends ADR 0095 §4.8)

- **Status:** revision 2 ACCEPTED (2026-09-28): `security` CONFIRMED WITH CONDITIONS (C-3, C-4) and
  `ledger-finance` CONFIRMED WITH CONDITIONS (K3-a). **Revision 3 PROPOSED (2026-10-04, `architect`,
  K3 design phase, base `3517980`).** Revision 3 is a refresh plus a scope decision. It does not
  reopen any revision-2 ruling. **Revision 3 reviews:** `security` ACCEPT WITH CONDITIONS (R-1..R-9,
  `reviews/k3-design-security.md`) and `ledger-finance` ACCEPT WITH CONDITIONS (D-1..D-10,
  `reviews/k3-design-ledger-finance.md`). **Revision 4 (2026-10-04, `architect`) writes every
  required condition into this ADR** (§24, with the in-place edits listed in the §25 checklist).
  **Before any K3 code:** a short `security` text-delta confirmation of revision 4, which also
  confirms the MA020 exemption (§18.3), and `ledger-finance` confirmation of the D-* text. **Implemented
  2026-10-04 against the MOCK provider (§27); real-PSP behaviour is PROVIDER DEPENDENT.**
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
| **4** | **`3158cf0`** | **Review conditions written in.** Security R-1..R-9 and LF D-1..D-10 (§24): the entries fence rewritten with per-entry shapes for (b)/(c), plus the widened acting `ledger_accounts` INSERT (R-1/D-1/D-2); a system-shape read of executed resolutions for reconciliation (R-2); a DB-guard parity table with K2 (R-3); the Y table's split system-shape policies and deferred park binding (R-4/D-6); the closed-tenant actor scope (R-5); payload pinning of the attempt state and reason (R-6); the column-discipline trigger (R-7); ingress conditions (R-8); routes and permissions (R-9); "declare paid" needs a reference (D-3); non-MOCK clearing (D-4); the confirming-line definition (D-5); one finding per exposure (D-7); `payment_attempts_id_tenant_key` (D-8); the hardened C-5b (D-9); the whole-schema C-16 (D-10). **MA020 exemption ADOPTED** with security (a)–(d) (§18.3). O-1..O-6 and L-1..L-4 adopted or recorded. LIKE-vs-`left()` mutant recorded as EQUIVALENT. New tests T-1..T-18 plus the LF additions (§12.4). The closed-tenant design becomes **ADR 0107** (PROPOSED, design only) |

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
| **Reference for "declare paid" (rev 4, LF D-3)** | `m2_declare_paid ⇒ attempt.provider_reference IS NOT NULL`. This is refused at submission by the insert trigger, again in `payment_m2_admits`, and in the executor (`MR010` family). The 0101 CHECK `state <> 'succeeded' OR provider_reference IS NOT NULL` (LF95-C4) is **not** relaxed, and the reserved id is **never** bound to the attempt. **Residual R-K3-9:** a reference-less payout (for example an ambiguous timeout before the provider acknowledged, or a T15 from `created`) that is confirmed paid out of band has no "declare paid" path; its hold is retained. "Declare not paid" stays available for it. |
| **Basis for "declare not paid" after possible dispatch (rev 4, LF L-3, adopted)** | `m2_declare_not_paid` on an attempt with `ever_possibly_sent = true` requires `basis_code = 'provider_confirmed_out_of_band'`. `reconciliation_exhausted` cannot be verified automatically, and a late statement line is exactly the double-payout case. Enforced by CHECK-by-trigger and the executor; test C-41. |
| **Basis (LF ruling 1)** | `basis_code IN ('provider_confirmed_out_of_band', 'reconciliation_exhausted')` for both M2 kinds (CHECK). Meaning: for `m2_declare_paid`, `reconciliation_exhausted` means a statement success line exists; for `m2_declare_not_paid`, it means statement coverage extends past the attempt with no line. `context_code` is NULL or in `('provider_unqueryable', 'past_resubmission_horizon')`, and is **secondary only**. The vocabulary is `payments`' to confirm. Automated verification of `reconciliation_exhausted` against the §9.2 persisted lines stays a candidate follow-up, not K3 scope. |
| **Evidence (C-101-2)** | `evidence_ref_hash` NOT NULL for M2 (CHECK). The runbook requires an operator T17 re-verify first, and its outcome is referenced. |
| Kind | `kind ∈ {m2_declare_paid, m2_declare_not_paid}`, with `target_state` `succeeded` / `declined` (CHECK) |
| **Tenant status (C-101-4; security ruling 5)** | M1 and M2 are **available for non-active tenants**, which includes `closed` (§19). Tenant status is read in-tx and recorded at submission and at execution. Policy evaluation for a non-active tenant ignores tenant and brand rows (`0113:655`). **Rev 4 (security R-5):** when `tenant_status = 'closed'`, read in-tx at insert, at each approval and at execution, a **tenant-scope** requester or approver is refused (and is not counted). Only `platform_acting` actors holding `payment_force_resolve` grants for that tenant may act (§24.5). |
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
    | **(rev 4, R-8(c))** Poll echo audits on the succeeded-terminal and decline paths | `sweeper.go:556`, `:636` (both through `echoAuditMeta`) | covered by the `echoAuditMeta` switch; each site is listed and tested separately (C-9) |
    | **(rev 4, R-8(d))** Deferred receipt replay | `receipt.go:1569-1607` (`ApplyDeferredReceiptsForAttempt`), which re-applies stored `payment_provider_events` without calling `validateReceiptReferences` | protected by the `payment_provider_events` CHECKs and the up-time refusal; C-9 adds a replay case proving a stored prefixed reference cannot exist |
    | (rev 4, Q-SEC-1 enumeration) Deposit sync via `deposit_v2.go:297` (`depositAdapterCall`); payout resubmit `payout.go:990-1085`; `payoutStatusQuery` callers (`/resolve`, `payout.go:1376`); deposit simulation via `ReceiveVerifiedCallback` | the same validator sites above | no extra site; listed so C-9 drives each caller |

    **Refused-echo handling (rev 4, LF L-2 / security O-2).** At `sweeper.go:499-503`, a refused echo
    writes one audit row and then `RescheduleNonTerminal` at the normal poll backoff. It **never**
    returns an error: an error return would re-drive the item at once, giving a hot loop and an
    audit-volume DoS by a hostile PSP. At `sweeper.go:646-647`, the decline proceeds with a nil
    reference (the echo is not adopted) and the same audit. Test T-13.
- **DB backstops.**
  - A CHECK `left(col, 27) <> payment_reserved_ref_prefix()` on:
    - `payment_attempts.provider_reference`;
    - `payment_provider_events.provider_reference`, `.original_provider_reference`,
      `.settlement_reference`;
    - `payment_statement_lines.provider_reference`, `.original_provider_reference`,
      `.settlement_reference`;
    - **(revision 3)** `payment_attempt_reference_evidence.reference` (§8.6);
    - **(revision 4, R-8(a), unconditional)** `deposit_intents.provider_reference` and
      `withdrawal_requests.provider_reference`. Both are provider-supplied (writers `drive.go:535`,
      `orchestrator.go:476`, `sweeper.go:507`, `:657`; `withdrawal.AttachProviderReference` from
      `payout.go:606`, `:636`, `:719`, `:752`, `:1032`, `:1046`, `:1079`). This is safe for M2:
      `withdrawal.Complete` does not write `withdrawal_requests.provider_reference`
      (`withdrawal.go:1501`).
    - **C-9c (R-8(b))** is a catalogue pin. It lists every column whose name matches
      `%provider_reference%`, `%provider_tx_id%` or `settlement_reference`, **plus
      `payment_attempt_reference_evidence.reference` by explicit name** (its name matches no
      filter). Each must carry the CHECK (or the all-sessions trigger, for
      `ledger_transactions.provider_tx_id`). `merchant_reference` columns are listed as
      **exempt** (the platform generates them).
  - A trigger **`ledger_transactions_reserved_prefix_guard`** (BEFORE INSERT, **all sessions**,
    `left()` not `LIKE`). If `left(NEW.provider_tx_id, 27) = prefix`, it requires
    `NEW.transaction_type = 'withdrawal_completed'` and ADR 0099 §6.6 predicate (b) (an executing
    `m2_declare_paid` resolution binding correlation, provider and key). Otherwise it raises
    `MR020`.
  - **0115 up refuses** if any existing value in those columns, or any
    `ledger_transactions.provider_tx_id`, already has the prefix. This is a full scan, run in the
    deploy window (security O-4).
  - **Casino and sportsbook sessions (security O-1, adopted).** The all-sessions trigger also binds
    casino and sportsbook postings. A provider-sent `provider_tx_id` that carries the prefix
    raises `MR020`. The casino and sportsbook callback paths map `MR020` to their existing
    deterministic invalid-reference (non-retryable 4xx) class, never to a retryable 5xx. Test T-13.
    If the mapping cannot be done without editing those packages' error tables, the residual is
    recorded (a retried 5xx is money-safe, because the trigger refuses every retry) and the mapping
    becomes a follow-up.

## 6. Governance

### 6.1 Capabilities, policy and counting

| Aspect | Rule |
|---|---|
| Grants | `payment_force_resolve:request` (requester) and `payment_force_resolve:approve` (approvers), for the attempt's tenant (ADR 0099; already in the 0112 enum). Eligibility reuses `ledger_adjustment_eligible_grant(p_tenant, p_staff, p_capability)` (`0113:898`), which is capability-parametric. K3 may rename it to a neutral alias only by `CREATE FUNCTION` of a wrapper, never by editing K2's body. |
| Classification | `mandatory_four_eyes` (already seeded, `0113:89-91`) |
| Policy | `financial_policy_required_approvals('payment_force_resolve', …)`: M2 uses the attempt's amount and asset; M1 the base only; `GREATEST(1, …)`; no platform baseline ⇒ disabled |
| Independence floor | LF-11: a distinct, non-NULL `person_id` for the requester and each approver. **Non-configurable.** |
| Counting | exactly ADR 0100 §6.2, including `FOR SHARE` and S-2(iii) for approvers |
| Payload hash | covers `attempt_id`, `kind`, `target_state`, `finding_code`/`basis_code`/`context_code`, `evidence_ref_hash`, amount, asset, reason code, **and (rev 4, R-6) `attempt_state_at_submission` and `terminal_reason_at_submission`** (DB-forced from the attempt row at insert). Execution ends `refused_at_execution` if either has changed. |
| Capability-specific grants (rev 4, R-3(ii)) | `financial_acting_session_valid()` is capability-agnostic. Every insert, approval and count therefore also requires `ledger_adjustment_eligible_grant(tenant, staff, 'payment_force_resolve:request' \| ':approve')`, with the `ledger_adjustment_invisible_platform_grant` fallback. A principal holding only `ledger_adjustment:*` for tenant X is refused (T-6). |
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
  `ledger_accounts` (INSERT limited by account type). **Rev 4:** these need the entries-fence
  rewrite (§24.1) and the widened `ledger_accounts` policy (§24.2). Without them every acting M2
  posting fails at its first entry (R-1/D-1/D-2).
- **Column discipline (rev 4, R-7):** neither the acting WITH CHECK nor `payment_m2_admits`
  constrains columns. The new trigger in §24.7 does.
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
| **(rev 4)** `internal/adjustment/execute.go` | one audit attribute, `open_payment_exposure_at_execution`, when the MA020 exemption applied (§18.3 (c)). No logic change |
| **(rev 4, conditional on O-1)** the casino and sportsbook callback error mapping | map `MR020` to the existing deterministic invalid-reference class. Only if it can be done without touching their domain logic; otherwise it is a recorded residual (§5.4) |
| Tests: new `internal/payments/*_k3_*_integration_test.go`, `internal/payments/migration_0115_integration_test.go`, `internal/providerref/*_test.go`; **plus** updates to `migration_0101_integration_test.go` and `migration_0107_integration_test.go` HEAD pins only if they assert the guard body text | §12 |
| Docs (DoD, §17) | as listed |

**Called, NOT edited:** `withdrawal.Complete`/`Fail`/`LockSubmittedForResolution`;
`internal/capability`; K2's `internal/adjustment/*` Go code; `internal/payments/payout_sweep.go`,
`sweeper_resolution_only.go` and `alerts.go` (H/I-wire); `internal/alerting/*`;
`reconciliation/scheduler.go`; `cmd/platform-api/main.go`; `internal/config/config.go`.
**If** implementation finds an edit to any of these unavoidable, it is a Rule 1 Touches addition
recorded by the orchestrator first. For `main.go` and `config.go`, it also waits for E1's merge
(§20.3).

**K2 SQL objects K3 replaces in 0115 (CREATE OR REPLACE, built on the 0113 body; §20.1 #13-#15 and §26 are the binding list, this paragraph is illustrative):**
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

**Rev 4 (security R-2): system-shape read of executed resolutions.** Without this policy the
reconciliation stream would see no `m2_*` rows, and rules (c), (c2) and (d) and INV-M-4 would be
silently dead. The stream runs as `WithTenantSnapshot`, which sets only `app.tenant_id`
(`tenant_rls.go:54`). One SELECT-only policy is added on `payment_manual_resolutions`, following K2's
`tenant_system_read_executed` (`0113:1785`):

```sql
CREATE POLICY tenant_system_read_executed ON payment_manual_resolutions FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND state = 'executed'
           AND NULLIF(current_setting('app.principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
```

No system-shape policy is added on approvals. C-7, C-22 and C-29 run in the real reconciliation
session shape **as the runtime role** (T-1). The mutant that drops this policy must be killed (T-3).

**Rev 4 (R-3): the DB-guard triggers** are listed in the parity table, §24.3. The trigger list
above is the minimum; §24.3 is binding.

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
| `ledger_governed_fence_allows` | replaced: ADR 0099 §6.6 (a) + (b) + (c). `ledger_transactions_governed_fence` calls it unchanged |
| **`ledger_entries_governed_fence()` (rev 4, D-1/R-1)** | **replaced** (CREATE OR REPLACE): branch (a) byte-identical to `0113:1463-1505`; new per-entry shapes for (b) and (c), §24.1. The revision-3 claim that this trigger is "unchanged" was wrong |
| **`ledger_accounts` `acting_insert` (rev 4, D-2/R-1)** | dropped and re-created, widened only as §24.2 says |
| **`payment_attempts_id_tenant_key` (rev 4, D-8/R-8(e))** | `ALTER TABLE payment_attempts ADD CONSTRAINT payment_attempts_id_tenant_key UNIQUE (id, tenant_id)`. Prerequisite for the composite FKs |
| **`payment_attempts_operator_column_discipline` (rev 4, R-7)** | a new BEFORE UPDATE trigger, §24.7. Not a guard edit |
| **`tenant_system_read_executed` on `payment_manual_resolutions` (rev 4, R-2)** | §8.2 |
| `ledger_adjustment_payload_refusal` | replaced: 0113 body plus the M2 Step B causation arm (ADR 0100 §5.4 adopted; security C-4 (a)–(e)), **plus (rev 4) the MA020 exemption term** (§18.3) |
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
  - restore **0113's** `ledger_governed_fence_allows` (branch (a) only), **0113's**
    `ledger_entries_governed_fence`, **0113's** `ledger_accounts` `acting_insert` policy, and **0113's**
    `ledger_adjustment_payload_refusal`, byte-for-byte (rev 4);
  - drop `payment_attempts_id_tenant_key`, the column-discipline trigger and the system-read policy
    (rev 4);
  - drop the reserved-prefix trigger and CHECKs, the acting policies, the new tables, the folded
    evidence table and indexes, and the functions;
  - restore **0113's** kind CHECK exactly (or, if another kind-widening migration merged between
    0113 and 0115, that migration's definition; §20.2).
- The `providerref`, payments and reconciliation Go changes revert with the code.
- **Must succeed on an empty scratch DB**: the full-chain rollback tests
  (`jurisdiction/migration_0075_*`, `migration_0077_*`) roll back every migration above 0099.
- **Guard tests run on a HEAD-migrated scratch DB** (LF-18).
- **C-16 uses K2's whole-schema snapshot (rev 4, D-10/T-18).** `schemaSnapshot`
  (`internal/adjustment/migration_0113_integration_test.go:20-47`, extended with indexes and grants)
  is taken on a scratch DB at N-1. Then up, down, and a second snapshot must be equal. That covers
  functions (including the entries fence), policies (including `ledger_accounts` and the R-2/R-4
  policies), constraints (including D-8), indexes, triggers and grants.
- **One transaction (security O-4).** Up and down each run in one transaction, so a FORCE-RLS
  lifted window for reference seeding is never visible outside the migration.

### 8.6 (Revision 3, folded) Persisted evidence substrate: STANDING-1 + POLL-REF-CLEAR-1

**One schema change, ledger-finance sign-off required** (LF D2 final review B3; registry rows).

**(a) `payment_attempt_reference_evidence`.** This is the structured home of the poll's returned
reference Y (POLL-REF-CLEAR-1). It is never audit JSON (LF ruling).

| Column | Type / constraint |
|---|---|
| `id` | `UUID PK DEFAULT gen_random_uuid()` |
| `tenant_id` | `UUID NOT NULL` |
| `attempt_id` | `UUID NOT NULL`; composite FK `(attempt_id, tenant_id)` → `payment_attempts (id, tenant_id)`, through the new `payment_attempts_id_tenant_key` that 0115 adds (rev 4, D-8: no such UNIQUE exists at HEAD) |
| `provider_id` | `TEXT NOT NULL`, the 0099 bound; must equal the attempt's `provider_id` (trigger) |
| `evidence_kind` | `TEXT NOT NULL CHECK (evidence_kind IN ('poll_returned_reference'))`. The set is closed; widening needs a migration and LF |
| `reference` | `TEXT NOT NULL`, the 0099 bound (length, no control characters), plus the §5.4 reserved-prefix CHECK, plus `CHECK (reference <> '')` |
| `recorded_at` | `TIMESTAMPTZ NOT NULL DEFAULT now()` |

- **Constraints and triggers (rev 4, D-6 / R-4; supersedes revision 3):**
  - UNIQUE `(tenant_id, attempt_id, evidence_kind)`. The writer uses a **plain INSERT; any
    duplicate raises**. There is no `ON CONFLICT DO NOTHING`, because a park happens exactly once.
  - **BEFORE INSERT trigger:** the attempt is `operation = 'deposit'` and in a **live** state
    (`submitting`, `pending` or `ambiguous`); its bound `provider_reference` is NOT NULL and differs
    from `reference`; `provider_id` equals the attempt's.
  - **DEFERRABLE INITIALLY DEFERRED constraint trigger** `payment_attempt_reference_evidence_bound_to_park`:
    at commit, the attempt is `disputed` with `terminal_reason = 'poll_reference_mismatch'`. Live
    at insert and parked at commit means the park happened **in this transaction**. A row without
    its park is refused at commit (T-7, mutant "drop deferred check").
  - `ledger_deny_mutation` on UPDATE, DELETE and TRUNCATE, binding on the owner too (0102 pattern).
  - The writer inserts the row **before** `parkDepositAttempt` in the same transaction, so the
    alert raise stays the last alert-table statement (ADR 0102 §7.7), and a failed park CAS rolls
    the row back.
- **RLS (rev 4, R-4; supersedes revision 3):** ENABLE + FORCE. **Split, system-shape policies
  only**, with no NULL arm (`NULLIF(...)::uuid` equality only):
  - `system_insert` FOR INSERT WITH CHECK: `tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid`,
    and `app.principal_id`, `app.player_account_id`, `app.platform_admin_principal_id` and
    `app.platform_service_id` all NULL, and `NOT financial_acting_gucs_present()`. This is the
    sweeper's `WithTenant` shape.
  - `system_select` FOR SELECT USING: the same predicate. This is the reconciliation
    `WithTenantSnapshot` shape.
  - **No** tenant-staff, player, acting or platform policy; **no** UPDATE or DELETE policy. Display
    to tenant staff is not needed by K3 and is not added.
  - Consequence (LF L-4): an acting MA020 evaluation cannot see Y and fails closed (safe).
    MA020-SYNC-MISMATCH-1 must add an acting read or accept that.
- **Grants:** `SELECT, INSERT` to `igaming_runtime` (migration block and `init-app-role.sql`).
- **Writer:** only `poll_evidence.go`'s `poll_reference_mismatch` park, and only when
  `ValidatePaymentReference(Y)` passes. An invalid Y stays audit-only (length and hash prefix),
  exactly as today.

**(b) Indexes on `payment_statement_lines`** (the cross-import lookup). **Non-concurrent build
ACCEPTED (LF Q-LF-5)**, inside the deploy window, together with the D-8 unique constraint (the
migration runs in a transaction). The runbook records the row counts of `payment_statement_lines`
and `payment_attempts` before the deploy. Index work at real-PSP volume goes through a later,
dedicated `CONCURRENTLY` migration. Exact names:
- `payment_statement_lines_ref` on `(tenant_id, provider_id, provider_reference)`;
- `payment_statement_lines_merchant` on `(tenant_id, provider_id, merchant_reference) WHERE merchant_reference IS NOT NULL`;
- `payment_statement_lines_reversal_original` on `(tenant_id, provider_id, original_provider_reference) WHERE kind = 'deposit_reversal'`.

No column is added to `payment_attempts`, so `payment_attempts_guard()` keeps exactly the §8.3 diff
(F12). A column alternative was rejected for that reason (§14).

## 9. Reconciliation (implemented in `payment_statement.go` by K3)

### 9.1 M2 rules (F10, LF ruling 5): unchanged from revision 2

| Rule | Behaviour |
|---|---|
| (a) | A reserved-prefix `withdrawal_completed` **skips the settlement-reference comparison** (`payment_statement.go:1040-1045`). Amount and asset are still compared. |
| (b) | Statement lines for an M2-declared payout resolve **by reference, then by merchant reference**. A matching `succeeded` line is a **confirmation**, counted by the metric `payment_m2_declared_paid_confirmed_total` (no tenant label); it is not a mismatch. A `declined` line → `pay_status_mismatch`. |
| (c) | **`pay_declared_paid_unconfirmed`**, standing and **unwindowed**, is raised for every executed `m2_declare_paid`. It clears **only** when (i) a **confirming line** (rev 4, D-5: defined below) exists in any eligible persisted import (§9.3 lookup, D-4), or (ii) a future WITHDRAWAL-REVERSAL-1 posting reverses that Step B. **A `compensating_entry` credit with causation = that Step B does not clear it** (LF K3-a). The compensation appears only as a read-time annotation. |
| (c2) | **`pay_declared_paid_compensated_but_paid`** (P1, standing, **unwindowed**; security C-4(b), LF K3-a) is raised when an executed `compensating_entry` credit whose causation is an M2 Step B is followed by a confirming `succeeded` line. It clears only when executed `compensating_entry` **debits**, whose causation is that credit's own `manual_adjustment` transaction, total **at least** the credited amount. |
| (d) | **`pay_declared_not_paid_but_paid`**, standing and **unwindowed**, is raised for every executed `m2_declare_not_paid` whose attempt reached T14 `disputed` **or** for which any persisted import has a `succeeded` line (§9.3 lookup). It clears only when executed `compensating_entry` debits with causation = the `withdrawal_failed` transaction **total at least the withdrawn amount** (LF confirmed). It **overrides the `:1084` disputed exclusion** for these attempts. |
| (e) | The reserved id is never expected on a statement. A line carrying the prefix is refused at fetch (§5.4). |

**Rev 4, D-5: "raise broad, clear narrow".**
- **Confirming line** (used to *clear* (c) and to count the confirmation metric): `kind = 'payout'`,
  `status = 'succeeded'`, same tenant and provider, resolved to the attempt by reference (then by
  merchant reference), `amount = attempt.amount` **and** `asset_code = attempt.asset_code`, from an
  eligible import (D-4).
- **Raising predicate** (used to *raise* (c2) and (d)): any `succeeded` payout line resolved to the
  attempt by reference or merchant reference, from **any** import (MOCK or not), whatever its
  amount and asset.
- A succeeded line with a different amount does **not** clear `pay_declared_paid_unconfirmed`, and
  it does raise `pay_amount_mismatch`. Test C-42.

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
| **N1 residual** (ADR 0095 §35.4) | Clearing for a conflict park is keyed on the evidencing line's reference, which another attempt may hold. **LF Q-LF-3 ruling (rev 4): approximate attribution ACCEPTED; no second `evidence_kind` in K3.** The S1 detail records the evidencing import id, its `line_no` and the holder attempt. Test **C-34b**: a conflict park is cleared by a reversal on R, and the holder is still reconciled independently. The N1 residual is disclosed in ADR 0095 §35 and `reconciliation-model.md` (LF edits). |
| **S6: one finding per exposure (rev 4, D-7)** | `checkMerchantAttribution` does not record the named attempt b in `matchedBy` (`payment_statement.go:1107-1113`), so S1 could otherwise emit twice. Findings are deduplicated on **(attempt, evidencing reference) per run**. The lookup deduplicates a line that appears in several overlapping imports (same provider, reference, kind, status, amount, asset and `occurred_at`). Persisted-lookup lines **never feed `pay_duplicate`**, which stays per-import. Tests C-43 (one finding) and C-44 (overlapping imports). |
| **Eligible evidence (rev 4, D-4)** | Evidence that **clears or confirms** ((c) confirmation, S2, S3, S4 reversal lines) comes only from imports with `is_mock = false`, **unless** NO `is_mock = false` import exists for (tenant, provider) (MOCK-only dev and test environments; §26 RC-3 supersedes "the current run's own import is MOCK"). Evidence that **raises** may come from any import. The finding detail names the import id and `is_mock`. Test C-45; mutant "MOCK clearing allowed". |
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

**Rev 4 (security O-3, adopted):**
- The lookup **never truncates with a LIMIT**. If a cap is ever needed it fails the run loudly
  (`reconciliation.run_failed`), and never drops evidence (INV-M-5).
- Each standing kind adds mismatch rows on every run, unwindowed. That growth is disclosed under
  CAS-RECON-SCALE-1.
- Session shape: as above, with the `system_select` policy on the evidence table (§8.6(a)) and the
  `tenant_system_read_executed` policy on resolutions (§8.2).

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
| C-37 | R/FL | Y evidence write: written only for a valid Y; in the park transaction (fault injection after the evidence insert → nothing persists, as ADR 0095 §36.7 QA C3); a prefixed Y is refused (CHECK and validator); UPDATE/DELETE refused; **a duplicate plain INSERT raises** (rev 4 / §26 RC-2; supersedes "idempotent only if equal") |
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
| use `LIKE` instead of `left()` | **EQUIVALENT (rev 4, security T-16)**: the prefix `platform-operator-declared:` contains no `_`, `%` or `\`, so `LIKE prefix || '%'` and `left()` agree on every input. It is recorded as equivalent, **never counted as killed**. `left()` stays the mandated form. |
| drop the deferred check | C-13 |
| drop the `operator` term from the acting WITH CHECK | C-14b |
| let `pay_declared_not_paid_but_paid` clear on a partial recovery | C-7 |
| skip the amount comparison for a reserved-prefix Step B | C-8 |
| **(rev 3)** window S1 to the current import | C-34 |
| **(rev 3)** S4 reads Y from audit JSON, or accepts any reference | C-36 |
| **(rev 3)** drop `tenant_id` from the persisted lookup | C-14d |
| **(rev 3)** grant-function removed from a K3 acting policy | C-40 |
| **(rev 3)** use the shared `Validate` at one payments ingress | C-9, C-9b |

### 12.4 Revision-4 additions (security T-1..T-18; LF test additions)

**Binding test rules:**
- **T-1 (vacuity):** every RLS, guard and fence test runs as the runtime role and asserts
  `NOT rolsuper AND NOT rolbypassrls`.
- The reconciliation tests (C-7, C-22, C-29, C-34..C-36, C-42..C-45) run in the real
  `WithTenantSnapshot` shape, with fixtures written through the real writers (sweeper park,
  executor, ingest), never by direct INSERT under the owner.

| ID | Source | Test |
|---|---|---|
| T-2 | R-1, D-1, D-2 | Acting M2 "paid" and "not paid" post successfully, **including the first-ever `psp_clearing` creation for a tenant and asset**. Refused (K2-C1/C2 analogs): wrong wallet, wrong amount, wrong asset, wrong account type, a third entry, a second leg in one direction, an entry appended after `executed` in the same transaction, `ledger_accounts` INSERT of another type or another wallet, or any of these with no executing M2 in this txid |
| T-3 | R-2 | The reconciliation session (app.tenant_id only, runtime role) sees executed `m2_*` rows, and sees no pending row and no approvals. Mutant: drop the policy → C-7 and C-22 fail |
| T-4 | R-3(iv) | Driving `pending → executing` with too few counted approvals is refused **by the DB** (a direct UPDATE in a test session). Mutant: delete the DB recount |
| T-5 | R-3(v) | After a governed posting in this transaction, any exit to `refused_at_execution`, `rejected`, `cancelled` or `expired` is refused. Mutant |
| T-6 | R-3(ii) | An acting principal with only `ledger_adjustment:*` for X cannot request, approve or be counted; the tenant-scope analog too. Mutant: capability argument set to NULL |
| T-7 | R-4, D-6 | Tenant staff, player, acting and platform sessions cannot INSERT or SELECT evidence. Evidence for a non-parked attempt, or for a parked attempt with another reason, is refused at commit. A duplicate plain INSERT raises. Mutants: drop the deferred check; widen the INSERT policy |
| T-8 | R-5 | `closed` tenant: a tenant-scope requester is refused at insert, a tenant-scope approver at approval, and a tenant-scope approval is not counted at execution (including a tenant closed **after** submission). Mutant |
| T-9 | R-6 | The attempt state or terminal reason changes between submission and execution → `refused_at_execution`. Mutant: drop the pinned fields from the hash |
| T-10 | R-7 | An operator-evidence UPDATE that also sets `provider_reference`, `provider_id`, `ledger_transaction_id`, `amount` or any other column is refused, in tenant **and** acting sessions. Mutant: drop the trigger |
| T-11 | C-17c | An exhaustive **payout** matrix, every non-`operator` evidence transition, is identical on 0107 and 0115 |
| T-12 | — | `payment_m2_admits` returns **false, never an error**, in sessions that cannot see resolutions (the sweeper's `WithTenant` shape). The H payout sweeper's behaviour is unchanged (H suite green) |
| T-13 | R-8, O-1, O-2, L-2 | C-9 extensions: `sweeper.go:556` and `:636` echo audits; deferred receipt replay; the `deposit_intents` and `withdrawal_requests` CHECKs; casino and sportsbook `MR020` mapping; a refused echo at `:499` reschedules with backoff (one audit per poll, no hot loop: assert `next_action_at` advanced and no error) |
| T-14 | R-9 | HTTP: a missing static permission gives 403; `tenant_admin` has no K3 permission; a path tenant different from the token tenant is refused; a body `tenant_id` is ignored; error bodies are tokens from the closed set; every refusal writes a denial audit |
| T-15 | R-9, C-15 | Audit asserts IP, UA, request id, before/after, the acting-forced actor, and the **link** from the `withdrawal.completed`/`withdrawal.failed` rows (written with `ActorSystem` in tenant sessions) to the resolution audit through the ledger transaction id |
| T-16 | — | The LIKE mutant is classified EQUIVALENT (§12 mutant table) |
| T-17 | C-37 | Fault injection after the evidence insert; then a **second session** verifies the row is absent after rollback |
| T-18 | D-10, C-16 | The whole-schema snapshot covers the R-1 objects and the R-2/R-4 policies |
| C-41 | L-3 | "Declare not paid" with `ever_possibly_sent = true` and basis `reconciliation_exhausted` is refused |
| C-42 | D-5 | A succeeded line with a different amount does not clear `pay_declared_paid_unconfirmed` |
| C-43 | D-7 | Exactly one finding per (attempt, reference) per run |
| C-44 | D-7 | A line in overlapping imports is deduplicated and never gives `pay_duplicate` |
| C-45 | D-4 | A MOCK import cannot clear or confirm a finding raised against a non-MOCK import; a MOCK-only run can |
| C-46 | D-3 | "Declare paid" on a reference-less attempt is refused at insert, in `payment_m2_admits` and in the executor |
| C-47 | D-9 | Go↔SQL allow-list parity: the DB allow-list, run against every classified reason, NULL and an unknown reason, refuses exactly what Go refuses |
| C-48 | Q-LF-2 | **MA020 exemption trio:** (1) a player with an open exposure receives the Step B credit; (2) every other credit, including a `compensating_entry` with non-Step-B causation, is still refused with MA020; (3) the audit records `open_payment_exposure_at_execution = true` |
| C-49 | LF | `RunLedgerVsProjection` = 0 after M2 "paid" and "not paid", with the `psp_clearing` delta asserted |
| C-50 | LF | Re-approving an executed resolution is refused |
| C-17b+ | LF | Non-vacuity: the accept and refuse sets are both non-empty; the deposit `→ declined` operator gate is exercised |
| C-19+ | LF | Checks detail text **stored by real runs**, not a source grep |
| C-34b | Q-LF-3 | §9.2 N1 row |

**C-5b hardened (D-9):** the static pin parses `receipt.go`, `payout.go`, `payout_sweep.go`,
`attempt.go` and `orchestrator.go`. It watches all five dispute writers
(`ApplyDisputeFromNonTerminal`, `ApplyDisputeFromNeverSent`, `ApplyDisputeFromDeclinedPayout`,
`ApplyTombstonePrecedesSuccess`, `applyPayoutLateEvidence`). Reasons written in the shared
`applyResolvedReceiptEvidence` count as payout-reachable. `invalid_provider_reference:` is expanded
over the closed `providerref` reasons. Variable reasons are resolved, never whitelisted. The pin
asserts a minimum site count, so a parser that finds nothing fails.

**Extra mutants (rev 4):**

| Mutant | Killed by |
|---|---|
| the entries fence admits any leg for (b)/(c) | T-2 |
| the entries fence drops `r.state = 'executing'` | T-2 |
| the `ledger_accounts` policy admits all account types | T-2 |
| the confirmation drops the amount check | C-42 |
| MOCK clearing allowed | C-45 |
| Y deferred check dropped | T-7 |
| S2 clears on any reference | C-34 |
| the Step B arm without the prefix | C-23 |
| MA020 exemption term negated, or widened to any `compensating_entry` | C-48 |
| system-read policy dropped | T-3 |
| DB recount deleted | T-4 |
| capability argument NULL | T-6 |
| R-5 closed-tenant refusal dropped | T-8 |
| R-6 pinned fields dropped | T-9 |
| R-7 trigger dropped | T-10 |

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
  in K3. It is the existing gap "payout disputes not alerted" (ADR 0102 §17.8).
  **Rev 4 (Q-SEC-3 ruling, accepted with conditions):** PAY-PAYOUT-DISPUTE-ALERT-1 is registered as
  a **hard prerequisite** before (1) any real payout provider and (2) **any platform policy row
  enabling `payment_force_resolve` for a real-money tenant**. "No platform row ⇒ disabled" is the
  enforcement point. The runbook precondition R-K3-5 stays. **Optional, adopted:** the executor
  refuses `m2_declare_not_paid` when no statement source is registered for (tenant, provider).
  **Launch flag:** until PAY-PAYOUT-DISPUTE-ALERT-1 and ALERT-DELIVERY-1 land, a double payout after
  "declare not paid" is detected only at rest and is never surfaced to a human. This keeps K3 off `internal/alerting/kind.go`, an
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

### 18.3 MA020 versus the M2 compensating credit: rev 4 decision

**ADOPTED (LF Q-LF-2 ruling: MA020 should not refuse the Step B credit; security Q-SEC-2: acceptable
only with conditions (a)–(d)).** In 0115's `ledger_adjustment_payload_refusal`, the MA020 test
becomes:

```sql
IF p_direction = 'credit_player'
   AND player_open_payment_exposure(p_tenant, p_player)
   AND NOT v_step_b_arm THEN
    RETURN 'MA020:open_payment_exposure';
END IF;
```

`v_step_b_arm` is **the same boolean** that admitted the causation through the Step B arm earlier in
the same function. It is never a free-standing "is compensating" flag. It is true only when
**every** C-4 (a)–(e) condition holds:
- `reason_code = 'compensating_entry'`, `direction = 'credit_player'`;
- the causation satisfies `left(provider_tx_id, 27) = payment_reserved_ref_prefix()` **and**
  `transaction_type = 'withdrawal_completed'`;
- the causation is the `ledger_transaction_id` of an **executed `m2_declare_paid`**;
- the causation has its `player_withdrawal_hold` leg on this wallet, in this asset;
- the cumulative cap is the hold-leg amount, checked under L2 at execution;
- `evidence_ref_hash` is present;
- no Person counted on the M2 resolution initiates or approves.

| Security condition | Where |
|---|---|
| (a) conjoined with the complete C-4 (a)–(e) predicate | the list above |
| (b) evaluated in the DB at insert and at `→ executing` | `ledger_adjustment_payload_refusal` is called by K2's request guard at insert and by the execution-status function at `→ executing` (the 0113 call sites are unchanged) |
| (c) audit records `open_payment_exposure_at_execution = true` | the K2 executor's `_executed` audit gains that attribute when the exemption applied. This is a Go change in `internal/adjustment` (a Rule 1 Touches addition for K3: one attribute, no logic change) |
| (d) a mutant widening the term to any `compensating_entry` is killed | C-48 (2) |

**Fallback (documented):** if security's text-delta confirmation of revision 4 withholds the
exemption, K3 ships with MA020 unchanged (fail closed). The stranding residual is then registered
(a player declared paid but unpaid, with an unrelated open exposure, cannot be restored until the
exposure clears), and a runbook step covers it. Neither choice blocks starting K3.

The text below is the revision-3 analysis, kept for the record.

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
folded into K3.** **Rev 4:** the design is now **ADR 0107**
(`docs/decisions/0107-closed-tenant-player-funds-staff-resolution.md`), status PROPOSED, design only,
**not part of the PRH-2 implementation**. It incorporates security CT-R1..CT-R7 and LF CT-1..CT-6.
The former design file is kept only as a pointer.

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

### 20.1 0115 object list (exact; rev 4)

This list is binding. An object not listed here is not created or replaced by 0115.

| # | Group | Object | Kind | RLS / grants |
|---|---|---|---|---|
| 1 | Prerequisite | `payment_attempts_id_tenant_key` UNIQUE `(id, tenant_id)` (D-8) | constraint | — |
| 2 | Reference | `payment_manual_resolution_codes` (7 rows, inserted before FORCE) | table | FORCE RLS; `reference_read` SELECT; grant SELECT |
| 3 | Functions | `payment_reserved_ref_prefix()`, `payment_m2_admits(...)` | new | — |
| 4 | Force-resolution | `payment_manual_resolutions` | table | FORCE RLS; T (`tenant_scope_select/insert/update`), A (`acting_read/insert/update`), **`tenant_system_read_executed`** (R-2); no P; grant SELECT/INSERT/UPDATE; no DELETE; TRUNCATE denied |
| 5 | Force-resolution | `payment_manual_resolution_approvals` | table | FORCE RLS; T and A select/insert; no system policy; grant SELECT/INSERT; immutable |
| 6 | Triggers on 4/5 | payload immutability and forced actor; class/allow-list/attempt checks at insert (incl. D-3, L-3, R-5, R-6 pinning); `payment_manual_resolutions_beneficiary_guard`; state machine with R-3 (i)–(viii); `payment_manual_resolutions_no_executing_commit` (deferred) | new | — |
| 7 | Folded evidence | `payment_attempt_reference_evidence` + BEFORE INSERT trigger + deferred `…_bound_to_park` + `ledger_deny_mutation` triggers | table | FORCE RLS; `system_insert`, `system_select` only; grant SELECT/INSERT |
| 8 | Folded indexes | `payment_statement_lines_ref`, `payment_statement_lines_merchant`, `payment_statement_lines_reversal_original` | index | — |
| 9 | Guard | `payment_attempts_guard()`: the exact §8.3 diff | replaced | — |
| 10 | Column discipline | `payment_attempts_operator_column_discipline` (R-7) | new trigger | — |
| 11 | Reserved prefix | CHECKs on `payment_attempts.provider_reference`; `payment_provider_events.{provider_reference, original_provider_reference, settlement_reference}`; `payment_statement_lines.{provider_reference, original_provider_reference, settlement_reference}`; `payment_attempt_reference_evidence.reference`; `deposit_intents.provider_reference`; `withdrawal_requests.provider_reference` | constraints | — |
| 12 | Reserved prefix | `ledger_transactions_reserved_prefix_guard` (all sessions) | new trigger | — |
| 13 | Fences | `ledger_governed_fence_allows` (a)+(b)+(c); `ledger_entries_governed_fence()` with (a) byte-identical and (b)/(c) per-entry shapes (§24.1) | replaced functions | — |
| 14 | Acting policies | `ledger_accounts` `acting_insert` (widened, §24.2); `payment_attempts` `acting_update`; `withdrawal_requests` `acting_read` + `acting_update`; `deposit_intents` `acting_update` (WITH CHECK false) | policies | — |
| 15 | K2 function | `ledger_adjustment_payload_refusal` (Step B arm + MA020 exemption term) | replaced | — |
| 16 | Reconciliation | `reconciliation_mismatches_mismatch_kind_check` (+3 kinds, name kept) | constraint | — |
| 17 | Up-time refusal | any reserved-prefix value present in the 11/12 columns or `ledger_transactions.provider_tx_id` | DO block | — |
| 18 | Grants | the in-migration `REVOKE ALL` then `GRANT` block for 2, 4, 5 and 7 (the K2 pattern) | — | matches `init-app-role.sql` |
| Down | — | refuses with resolutions, new-kind rows or evidence rows; restores the 0107 guard, the 0113 bodies of 13 and 15, the 0113 `ledger_accounts` `acting_insert`, and the previous kind CHECK; drops everything else; must pass on an empty scratch DB; verified by the whole-schema snapshot (D-10) | — | — |

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
| R-K3-8 (rev 4, LF L-1: complete list) | **Every payout hold M2 refuses keeps its hold with no resolution path:** `amount_asset_mismatch` and `callback_amount_asset_mismatch` (owner PAYOUT-AMOUNT-DISPUTE-1); `invalid_provider_reference` and `invalid_provider_reference:<reason>` (owner PAY-PAYOUT-UNBOUND-HOLD-1); **`reversal_tombstone_precedes_success`, `late_success_after_terminal`, `late_decline_after_terminal`, `late_contradicting_evidence`** (until now no owner; **new follow-up PAY-PAYOUT-CONTRADICTION-HOLD-1**, owner payments + LF, before any real payout provider) | the follow-ups named; for a closed tenant, also ADR 0107 (`retain` only) |
| R-K3-9 (rev 4, D-3) | A reference-less payout confirmed paid out of band has no "declare paid" path; its hold is retained | registered residual (§5.1); "declare not paid" stays available |
| R-K3-10 (rev 4, O-5, launch flag) | Under HD-PRH2-2 (c), co-approval is grant-level only | before real money, at least one **platform-scope** approver on `m2_declare_not_paid` (the double-payout direction), via HD-PRH2-8. Human decision, not set here |
| R-K3-11 (rev 4, O-6, accepted) | The acting `withdrawal_requests` USING clause (needed for `FOR UPDATE`) lets a validly acting principal briefly row-lock tenant X's withdrawals | accepted; the same class as K2's `staff_users` `acting_lock` |
| R-K3-12 (rev 4, R-9, accepted) | `withdrawal.Complete`/`Fail` write `ActorType: system` audit rows in tenant-scope sessions (`withdrawal.go:1513`); acting sessions are forced to staff by `audit_log_acting_actor` | accepted without a `withdrawal.go` edit, **only because** T-15/C-15 asserts the linkage to the resolution audit |
| R-K3-13 (launch flags carried) | TM-7, TM-10, HD-PRH2-8, LEDGER-MANUAL-ADJ-LINK-1, STAFF-LIFECYCLE-1 (revoke-on-suspend before real money), PAY-PAYOUT-DISPUTE-ALERT-1 + ALERT-DELIVERY-1 before any real payout provider or real-money enablement of M2 | via the orchestrator to the human |

## 22. Open questions: status at revision 4

| ID | To | Status |
|---|---|---|
| Q-LF-1 | ledger-finance | **CONFIRMED** with D-4..D-8 (§8.6, §9.2) |
| Q-LF-2 | ledger-finance | **Ruled: no MA020 on the Step B credit**; adopted with security (a)–(d) (§18.3); awaiting security's text-delta confirmation (fallback documented) |
| Q-LF-3 | ledger-finance | **Ruled: approximate attribution accepted**; C-34b; N1 disclosed (§9.2) |
| Q-LF-4 | ledger-finance | **CONFIRMED** (§5.1; R-K3-8) |
| Q-LF-5 | ledger-finance + devops | **ACCEPTED** non-concurrent build, explicit object list, runbook row counts (§8.6(b), §20.1) |
| Q-SEC-1 | security | **CONFIRMED** with R-8 (§5.4) |
| Q-SEC-2 | security | conditions (a)–(d) written into §18.3; **text-delta confirmation pending** |
| Q-SEC-3 | security | **ACCEPTED with conditions** (§12.3): PAY-PAYOUT-DISPUTE-ALERT-1 is a hard prerequisite |
| Q-SEC-4 | security | **SUFFICIENT** under R-4 (§8.6(a)) |
| Q-IC-1 | identity-compliance + LF | LF part concurred; **identity-compliance confirmation still OPEN** (not blocking K3 code per the reviews; the orchestrator decides) |
| Q-PAY-1 | payments | the provider-reference CHECKs are now unconditional (R-8(a)); `payment_attempts_id_tenant_key` added (D-8); **the basis/context vocabulary is still OPEN for `payments`** |
| Q-POP-1 | product-owner-proxy | **OPEN** (concurrence on the §18 folds) |

No threshold, recipient, retention period or legal rule is set by this ADR.

## 23. Registry and HANDOVER wording (proposed; orchestrator writes)

See the K3 design hand-off. The orchestrator is the single writer of `task-registry.md`,
`HANDOVER.md`, `progress.md` and `active-stage.md`.

## 24. Revision 4: binding design changes from the revision-3 reviews

The sources are `reviews/k3-design-security.md` (R-1..R-9, O-1..O-6, T-1..T-18) and
`reviews/k3-design-ledger-finance.md` (D-1..D-10, L-1..L-4, rulings). Where this section and an
earlier section differ, **this section wins**.

### 24.1 The entries fence: per-entry shapes for (b) and (c) (LF D-1 = security R-1)

0115 replaces `ledger_entries_governed_fence()` with CREATE OR REPLACE. The parent check is
unchanged: the transaction must pass `ledger_governed_fence_allows`, now (a)+(b)+(c). The function
then dispatches on the parent's `transaction_type`:

- **`manual_adjustment`:** the 0113 body (`0113:1478-1499`), **byte-identical**. That means the K2
  request lookup, the §4 leg shape, and at most two entries with one per direction.
- **`withdrawal_completed` (branch (b), "declare paid"):**
  - Look up the resolution `m` with `m.tenant_id = v_tx.tenant_id`, `m.kind = 'm2_declare_paid'`,
    `m.state = 'executing'`, `m.executed_txid = txid_current()` and
    `m.withdrawal_request_id = v_tx.correlation_id`, joined to `withdrawal_requests w` (same
    tenant). If none is found, raise `CG030`.
  - The entry must be exactly one of: **debit** `player_withdrawal_hold` with
    `wallet_id = w.wallet_id`, or **credit** `psp_clearing` with `wallet_id IS NULL`.
  - In both cases `NEW.asset_code = w.asset_code` and `NEW.amount = w.amount = m.amount`.
- **`withdrawal_failed` (branch (c), "declare not paid"):**
  - The same lookup, with `m.kind = 'm2_declare_not_paid'`.
  - The entry must be exactly one of: **debit** `player_withdrawal_hold` with
    `wallet_id = w.wallet_id`, or **credit** `player_cash` with `wallet_id = w.wallet_id` (the same
    wallet).
  - The same asset and amount rule applies.
- **Every branch:** at most two entries per linked transaction, one per direction (the 0113 count
  rule).
- **Any other `transaction_type`:** `CG030`. An acting session can add entries only to a governed
  header of a known shape, so K2-C1 cannot reopen.
- **After the resolution leaves `executing`** in the same transaction, no further entry passes:
  every branch requires `state = 'executing'`.
- **Down** restores the 0113 body byte-for-byte (C-16 / D-10).
- **Tests:** T-2. **Mutants:** "admit any leg for (b)/(c)"; "drop `state = 'executing'`".

### 24.2 Acting `ledger_accounts` INSERT, widened (LF D-2; security R-1)

`GetOrCreateAccount` always runs `INSERT … ON CONFLICT DO NOTHING` (`ledger.go:521-545`). RLS WITH
CHECK fires even when the row already exists (LF probe on PG 16.13). 0115 therefore drops and
re-creates `acting_insert` on `ledger_accounts`:

```sql
CREATE POLICY acting_insert ON ledger_accounts FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid
                AND (SELECT financial_acting_session_valid())
                AND (account_type IN ('player_cash', 'manual_adjustment')          -- 0113, unchanged
                     OR (account_type = 'player_withdrawal_hold'
                         AND EXISTS (SELECT 1 FROM payment_manual_resolutions m
                                       JOIN withdrawal_requests w ON w.id = m.withdrawal_request_id AND w.tenant_id = m.tenant_id
                                      WHERE m.tenant_id = ledger_accounts.tenant_id
                                        AND m.kind IN ('m2_declare_paid', 'm2_declare_not_paid')
                                        AND m.state = 'executing' AND m.executed_txid = txid_current()
                                        AND w.wallet_id = ledger_accounts.wallet_id
                                        AND w.asset_code = ledger_accounts.asset_code))
                     OR (account_type = 'psp_clearing' AND wallet_id IS NULL
                         AND EXISTS (SELECT 1 FROM payment_manual_resolutions m
                                       JOIN withdrawal_requests w ON w.id = m.withdrawal_request_id AND w.tenant_id = m.tenant_id
                                      WHERE m.tenant_id = ledger_accounts.tenant_id
                                        AND m.kind = 'm2_declare_paid'
                                        AND m.state = 'executing' AND m.executed_txid = txid_current()
                                        AND w.asset_code = ledger_accounts.asset_code))));
```

- Down restores the 0113 policy byte-for-byte.
- Test T-2 includes **M2 as the first-ever `psp_clearing` creation for a tenant and asset**.
- Mutant: "admit all account types".
- The subqueries read through the acting read policies (§8.2, §6.4).

### 24.3 DB-guard parity with K2 (security R-3)

Each control is enforced **in the DB**, not only in the executor.

| # | Control | K2 counterpart | K3 implementation |
|---|---|---|---|
| (i) | Insert scope is `tenant` or `platform_acting` only, from `financial_actor_session()`; `NEW.tenant_id` equals the session tenant | `ledger_adjustment_requests_guard` (`0113:954`) | the resolutions and approvals insert triggers |
| (ii) | **A capability-specific grant** at insert and in counting | `ledger_adjustment_eligible_grant(..)` and `_invisible_platform_grant` (`0113:898`, `:925`) | the same functions with `'payment_force_resolve:request'` (requester) and `':approve'` (approvers). `financial_acting_session_valid()` alone is **not** enough (T-6) |
| (iii) | S-2(iii) at insert: the requester is not a Person who authored or approved a contributing policy | `financial_policy_author_persons` (`0113:692`) | the insert trigger, and again at count |
| (iv) | **DB-side recount** at `pending → executing` (K2-C3 lesson) | `ledger_adjustment_execution_status` (`0113:1360`) | `payment_manual_resolution_execution_status(p_resolution)`, called by the state-machine trigger on `→ executing`. It recounts distinct, non-NULL, non-beneficiary, non-author Persons with in-force `:approve` grants (`FOR SHARE`), applies R-5, and compares the count with `financial_policy_required_approvals` at execution. Too few → refused (T-4) |
| (v) | Refuse every non-executed exit (`refused_at_execution`, `rejected`, `cancelled`, `expired`) once a governed posting exists in this transaction. "Governed posting" means a `ledger_transactions` row keyed `provider_id:platform-operator-declared:<id>`, or keyed `wr.id:failed` with `correlation_id = wr.id`, inserted by this transaction | K2-C1(i) / MA040 | the state-machine trigger (T-5) |
| (vi) | `expires_at` is DB-forced from the policy, never client-supplied | K2 guard | the insert trigger |
| (vii) | Only the requester may cancel. Reject happens only through a reject decision in the same transaction. `→ executing` and `→ refused_at_execution` happen only in the final approval's transaction (`decided_txid = txid_current()`) | K2 guard | the state-machine trigger |
| (viii) | Server-forced ids: `id` is a DB default and a client-supplied value is refused, so `reserved_provider_tx_id = prefix \|\| id` is never client-chosen | K2 guard | the insert trigger |

### 24.4 Y evidence table (security R-4; LF D-6)

Now specified in §8.6(a):
- split system-shape INSERT and SELECT policies;
- no UPDATE or DELETE policy, and no NULL arm;
- a live-state BEFORE INSERT check;
- the DEFERRABLE INITIALLY DEFERRED park binding;
- a plain INSERT, so any duplicate raises.

Tests T-7 and T-17.

### 24.5 Closed-tenant actor scope (security R-5)

- The insert, approval and execution triggers each read `tenants.status` in-tx.
- When it is `'closed'`:
  - a requester whose `financial_actor_session()` scope is `tenant` is refused (`MR030`);
  - an approval row from a tenant-scope approver is refused at insert;
  - at `→ executing`, the DB recount (§24.3 (iv)) counts **only** `platform_acting` approvals. This
    catches a tenant closed after submission.
- Only `platform_acting` actors holding `payment_force_resolve:*` grants for that tenant may act.
- The optional extension to `suspended` is **not adopted**. It is recorded as a candidate for
  HD-PRH2-8 / O-5 (R-K3-10).
- Test T-8.

### 24.6 Payload pinning of the factual basis (security R-6)

- `attempt_state_at_submission` and `terminal_reason_at_submission` are DB-forced from the attempt
  at insert, and included in `payload_hash` (§6.1).
- At `→ executing`, a changed state or reason ends the resolution `refused_at_execution` (an S-11
  stale-approval void).
- Tests C-12b and T-9.

### 24.7 Column discipline for operator-evidence UPDATEs (security R-7)

New trigger `payment_attempts_operator_column_discipline`, BEFORE UPDATE ON `payment_attempts` FOR
EACH ROW. It fires when `NEW.last_evidence_kind = 'operator'` and `NEW.state IS DISTINCT FROM
OLD.state`. It is **not** an edit of `payment_attempts_guard()`, so the §8.3 diff stays exact.

- **Allowed to change:** `state`, `last_evidence_kind`, `resolved_at`, `next_action_at`,
  `updated_at`.
- **`terminal_reason`:** must stay unchanged, except where a later ADR names an exact value for a
  named transition. K3 names none. ADR 0107's governed M3 names `closed_tenant_release` for
  `created → rejected`, and would add that one case.
- **Every other column** must be `IS NOT DISTINCT FROM` OLD. This explicitly includes
  `provider_reference`, `provider_id`, `ledger_transaction_id`, `amount`, `asset_code`,
  `ever_possibly_sent`, `claim_token` and the lease columns. Otherwise `MR040`.
- **Scope:** every session. That covers tenant sessions (`tenant_staff_scope` FOR ALL), not only
  acting ones.
- The executor never passes the reserved id as `provider_reference`. The reserved id lives only in
  the ledger key.
- Test T-10; mutant "drop the trigger".
- **Q-PAY-2 (non-blocking; at implementation):** the 0107 guard and its T5/M3 branches must accept
  an M2 transition that leaves `terminal_reason` unchanged. `payments` confirms with the C-17/T-11
  matrices.

### 24.8 Ingress conditions (security R-8)

Written into §5.4:
- (a) the unconditional `deposit_intents` and `withdrawal_requests` CHECKs;
- (b) the explicit C-9c evidence column and the `merchant_reference` exemption;
- (c) `sweeper.go:556` and `:636`;
- (d) the deferred receipt replay;
- (e) the composite FK prerequisite, which is `payment_attempts_id_tenant_key` (§8.4, §20.1 #1).

### 24.9 Routes, permissions, errors and audit (security R-9)

These mirror `manual_adjustment_routes.go`.

| Aspect | Rule |
|---|---|
| Static permissions | `payment_force_resolve:request` and `:approve` go to `finance` and `platform_admin` **only**, never `tenant_admin`. `payment_force_resolve:read` goes to `finance`, `platform_admin` and `compliance`. `backoffice/src/auth/permissions.ts` matches |
| Authority | The JWT role is only the route gate. The authority is the in-tx grant (§24.3 (ii)) |
| Tenant | Taken from the `canActOnTenant` path value, never the body. A body `tenant_id` is ignored. A mismatch is refused |
| Errors | DB refusals map to a **closed token set**: `force_resolve_disabled`, `force_resolve_not_permitted`, `force_resolve_precondition_failed`, `force_resolve_reason_not_resolvable`, `force_resolve_conflict`, `force_resolve_expired`, `force_resolve_not_found`. No SQL text and no Person ids |
| Denial audit | Every refusal writes `payment.manual_resolution_denied` (the K2 `recordAdjustmentDenied` pattern), with the token, actor, tenant, resolution or attempt id, IP, UA and request id |
| Executed audit | `payment.manual_resolution_executed` carries the resolution id, the ledger transaction id, and the withdrawal and attempt before/after (§11) |
| Linkage | R-K3-12; T-15 |

### 24.10 Optional findings and LOW items: disposition

| Item | Disposition |
|---|---|
| O-1 casino/sportsbook `MR020` | **Adopted** (§5.4): map it to the deterministic invalid-reference class, or record the residual; T-13 |
| O-2 refused poll echo | **Adopted** (§5.4): an audit plus `RescheduleNonTerminal`, never an error return; T-13 |
| O-3 unwindowed growth, no LIMIT | **Adopted** (§9.3) |
| O-4 one-transaction down; full-scan up-time refusal | **Adopted** (§8.5, §5.4) |
| O-5 a platform-scope approver on "declare not paid" before real money | **Launch flag** (R-K3-10); human, via HD-PRH2-8 |
| O-6 acting `withdrawal_requests` row lock | **Accepted** residual (R-K3-11) |
| L-1 every refused hold listed; an owner for tombstone/`late_*` | **Adopted** (R-K3-8); new **PAY-PAYOUT-CONTRADICTION-HOLD-1** |
| L-2 refused echo handling | **Adopted** (= O-2) |
| L-3 "not paid" after a possible dispatch needs `provider_confirmed_out_of_band` | **Adopted** (§5.1); C-41 |
| L-4 MA020 cannot see Y under acting | **Recorded** (§8.6(a)); MA020-SYNC-MISMATCH-1 adds an acting read or accepts this |
| Security optional: refuse `m2_declare_not_paid` when no statement source is registered | **Adopted** (§12.3) |

### 24.11 Follow-ups for the orchestrator to register

- **PAY-PAYOUT-DISPUTE-ALERT-1:** payout disputes, T14 included, raise no alert. **Hard prerequisite
  before any real payout provider and before any platform policy row enabling
  `payment_force_resolve` for a real-money tenant** (Q-SEC-3).
- **PAY-PAYOUT-CONTRADICTION-HOLD-1:** the holds of payouts disputed with
  `reversal_tombstone_precedes_success` or a `late_*` reason have no resolution path (LF L-1). Owner:
  payments + LF. Due before any real payout provider.
- **PAY-M3-STAFF-PATH-1:** M3 has no caller for active tenants (a KYC deny at T2, a credential
  permanently gone). Owner: payments. Needs a design decision. ADR 0107's governed M3 can be
  generalized later.
- **R-K3-9 residual:** a reference-less payout confirmed paid has no "declare paid" path. Record it
  as a note on PAYOUT-AMOUNT-DISPUTE-1, or under its own id, at the orchestrator's choice.

## 25. Revision 4 checklist (required change → where satisfied)

| Required change | Section(s) |
|---|---|
| **Security R-1** (fences (b)/(c) per entry; `ledger_accounts`; down; K2-C1/C2 analogs) | §24.1, §24.2, §8.4, §8.5, §20.1 #13–14, T-2 |
| **Security R-2** (system read of executed resolutions; real-session tests; mutant) | §8.2, §9.3, §20.1 #4, T-1, T-3 |
| **Security R-3** (DB-guard parity (i)–(viii)) | §24.3, §6.1, T-4, T-5, T-6 |
| **Security R-4** (Y policies and writer binding) | §8.6(a), §24.4, T-7, T-17 |
| **Security R-5** (closed tenant: platform_acting only) | §5.1, §24.5, T-8 |
| **Security R-6** (pin attempt state and reason) | §6.1, §24.6, T-9, C-12b |
| **Security R-7** (column discipline) | §6.4, §24.7, §20.1 #10, T-10 |
| **Security R-8** (ingress (a)–(e)) | §5.4, §24.8, §8.4, §20.1 #1, #11, T-13 |
| **Security R-9** (routes, permissions, errors, denial audit, linkage) | §24.9, R-K3-12, T-14, T-15 |
| Security O-1..O-6 | §24.10 |
| Security Q-SEC-1..4 | §22, §5.4, §18.3, §12.3, §8.6(a) |
| Security T-1..T-18 | §12.4 (T-16 = §12 mutant table) |
| LIKE mutant EQUIVALENT | §12 mutant table |
| **LF D-1** | §24.1 |
| **LF D-2** | §24.2 |
| **LF D-3** (+ residual) | §5.1, R-K3-9, C-46 |
| **LF D-4** | §9.2 "Eligible evidence", §9.1, C-45 |
| **LF D-5** | §9.1, C-42 |
| **LF D-6** | §8.6(a), T-7 |
| **LF D-7** | §9.2 S6, C-43, C-44 |
| **LF D-8** | §8.4, §8.6(a), §20.1 #1 |
| **LF D-9** | §12.4 "C-5b hardened", C-47 |
| **LF D-10** | §8.5, T-18 |
| LF L-1..L-4 | §24.10, R-K3-8, §5.1, §5.4, §8.6(a) |
| LF Q-LF-1..5 | §22, §8.6, §9.2, §18.3, §5.1 |
| LF test additions and extra mutants | §12.4 |
| **MA020 exemption** (Q-LF-2 + Q-SEC-2 (a)–(d)) | §18.3, §8.4, C-48, §7 |
| HD-PRH2-9 → ADR 0107 | §19 |
| Follow-ups | §24.11 |

**Still open after revision 4:**
- security's text-delta confirmation, including the MA020 exemption;
- LF confirmation of the D-* text;
- Q-IC-1 (identity-compliance);
- Q-PAY-1 vocabulary and Q-PAY-2 (payments);
- Q-POP-1.

No threshold, recipient, retention period or legal rule is set here.

## 26. Implementation-start conditions (applied before any 0115 SQL; `payments`, K3 implementation)

Sources: `reviews/k3-design-r4-security-delta.md` (K3-S1..S3, O-K1..O-K3) and
`reviews/k3-design-r4-lf-delta.md` (RC-1..RC-3, O-1..O-6). **Where this section and an earlier section
differ, this section wins** (as §24 does). The §20.1 object list stays exact except for the two
additive triggers of RC-1 and the function named in K3-S2, which are listed here and are added to
§20.1 as rows 6a and 6b.

### 26.1 Security conditions

- **K3-S1 (column discipline scope).** `payment_attempts_operator_column_discipline` fires on a
  `payment_attempts` UPDATE when **either** (i) `NEW.last_evidence_kind = 'operator'` and the state
  changes, **or** (ii) `financial_acting_gucs_present()` is true, state change or not. In case (ii) the
  same column rules apply (only `state`, `last_evidence_kind`, `resolved_at`, `next_action_at`,
  `updated_at` may change; `terminal_reason` unchanged), so a same-state acting UPDATE that sets
  `provider_reference` or `ledger_transaction_id` from NULL is refused (`MR040`). T-10 adds that case.
  Tenant-session same-state writes at HEAD are out of scope and unchanged.
- **K3-S2 (requester re-check at execution).** `payment_manual_resolution_execution_status(p_resolution)`
  returns, besides the recount, a boolean `requester_valid` = requester's staff row live AND the
  `payment_force_resolve:request` grant in force (`ledger_adjustment_eligible_grant`, with the
  invisible-platform fallback) AND the requester's Person unchanged since insert AND (when the tenant
  status read at execution is `closed`) the requester scope is `platform_acting`. `-> executing` is
  refused when it is false. T-4 adds "requester grant revoked before execution"; T-8 adds "tenant closed
  after a tenant-scope submission".
- **K3-S3 (Touches record; MA020 audit).** The Touches record is corrected: `internal/adjustment/execute.go`
  is **edited**, and only to add the `open_payment_exposure_at_execution` audit attribute. The value
  comes from an in-tx DB read of `player_open_payment_exposure(...)` after Step 6 under L2. A read error
  aborts the transaction (fail closed). The attribute has no effect on control flow. K2's other
  `internal/adjustment/*` Go code is unchanged. In §7 "Called, NOT edited" this file is no longer listed.
- **O-K1.** The R-3(v) refusal (§24.3 (v)) applies to transitions **out of** `executing` only (not to a
  `pending` M2 that is cancelled, rejected or expired because the withdrawal was failed by evidence).
  Test: a pending M2 can still be cancelled after the withdrawal was failed by sweeper evidence.
- **O-K2.** Tidy: C-37 (RC-2), §7 "K2 SQL objects" (§20.1 binding), C-16 (D-10 snapshot) are corrected in
  place or superseded by this section.
- **O-K3 (residual; wording corrected in the fix batch).** `withdrawal_requests.provider_reference`, `provider_id` and `state` are NOT in
  `withdrawal_requests_immutable_fields`, so an acting session inside an executing M2 transaction can rewrite them (the acting UPDATE policy
  binds the row to an executing M2, not the columns). The deferred verifier re-checks the withdrawal state and the release link at commit, and
  `withdrawal.Complete`/`Fail` do not write those columns, but nothing extends the column discipline to `withdrawal_requests` in K3.
  **Launch flag:** extend the column discipline (state, release link only) to `withdrawal_requests` under an acting session before any real-money enablement.

### 26.2 Ledger-finance conditions

- **RC-1 (Person separation of the Step B credit; `v_step_b_arm`).**
  - `v_step_b_arm` in `ledger_adjustment_payload_refusal` is defined as the **payload-arm conditions
    only**: reason `compensating_entry`; direction `credit_player`; the causation passes
    `left(provider_tx_id, 27) = payment_reserved_ref_prefix()` and has type `withdrawal_completed`; the
    causation is the `ledger_transaction_id` of an executed `m2_declare_paid`; the causation has a
    `player_withdrawal_hold` leg on `p_wallet` in `p_asset`; the cap check has passed (MA022 returns first);
    the evidence hash is present. It is assigned only on the Step B path, after all those checks, and is never
    a parameter.
  - Person separation is enforced by **two new additive triggers** (neither edits a K2 body):
    `ledger_adjustment_requests_step_b_person_sep` (BEFORE INSERT on `ledger_adjustment_requests`; the name
    sorts after `ledger_adjustment_requests_guard`, so it sees the forced `initiated_by_person_id`) and
    `ledger_adjustment_approvals_step_b_person_sep` (BEFORE INSERT on `ledger_adjustment_approvals`). Each
    refuses when the causation is an M2 Step B and the Person is the M2's requester or any counted M2
    approver.
  - The execution-time re-check sits inside the Step B arm of `ledger_adjustment_payload_refusal`, using
    `p_self` (at `-> executing` the request and its approvals are visible). If separation fails the arm is
    false and the function refuses (fail closed).
  - Both triggers are listed in §20.1 (rows 6a, 6b); the down migration drops them; the C-16/T-18 snapshot
    covers them. C-28 therefore has a DB site at insert (both triggers) and at execution.
- **RC-2.** C-37: "a duplicate plain INSERT raises".
- **RC-3.** MOCK evidence may clear or confirm only when no `is_mock = false` import exists for
  (tenant, provider). Raising from any import is unchanged. C-45 states the intent.
- **LF optional, adopted where cheap:** O-2 the entries-fence (b)/(c) lookup also binds
  `v_tx.idempotency_key` to the resolution; O-3 the execution-status function is named
  `payment_manual_resolution_execution_status`; O-4 the "no statement source registered" refusal decides from
  the in-process source registry, never from `payment_statement_imports`; O-5 the Y BEFORE INSERT trigger
  locks the attempt row `FOR SHARE`; O-6 `open_payment_exposure_at_execution` is derived by calling
  `player_open_payment_exposure()` inside the execution tx (C-48 adds a case where exposure opens between
  submission and execution).

### 26.3 Status

Revision 4 plus §26 is the binding implementation text. Still open (non-blocking, recorded): Q-IC-1,
Q-PAY-1 vocabulary (the code uses the §5.1 vocabulary unchanged), Q-PAY-2 (confirmed by the C-17/T-11
matrices at implementation), Q-POP-1.

## 27. Implementation record (`payments`, K3 implementation, 2026-10-04)

Branch `prh2-k3-impl` (from `prh2-k3-design`, merged with `claude/focused-wright-jw88w9`). Revision 4 plus §26 was
implemented as written; where this record differs it says so in §27.4.

### 27.1 Deliverable labels

| Deliverable | Label |
|---|---|
| Migration 0115 up/down (§20.1 object list), `payment_manual_resolutions` / `_approvals` / `_codes`, `payment_attempt_reference_evidence`, `payment_m2_admits`, guards, fences, policies, CHECKs, grants | `IMPLEMENTED` |
| M1 (deposit evidence only) and M2 "declare paid" / "declare not paid" via `withdrawal.Complete` / `Fail`, request/approve/reject/cancel/execute service, routes, permissions | `IMPLEMENTED` against the MOCK provider; behaviour against a real PSP is `PROVIDER DEPENDENT` |
| Reserved provider-tx namespace and ingress validation at every payments site (and the statement fetch) | `IMPLEMENTED` |
| Reconciliation: persisted statement-line lookup S1-S6, the three standing kinds, typed Y evidence | `IMPLEMENTED` against MOCK statement sources; real statement matching is `PROVIDER DEPENDENT` |
| Registration of a payment statement source in the platform binary (from the SAME list the scheduler runs; both M2 kinds refused per provider) | `IMPLEMENTED` against the MOCK source for `mock-payments` only (§28, PAY-K3-STATEMENT-SOURCE-WIRING-1); a real PSP source is `PROVIDER DEPENDENT` and `NOT IMPLEMENTED`; H-W1 (non-active tenants not swept) is an OPEN HUMAN DECISION; depends on migration 0116 merging (§28.1) |
| Alerts for payout disputes and T14 (PAY-PAYOUT-DISPUTE-ALERT-1) and alert delivery (ALERT-DELIVERY-1) | `NOT IMPLEMENTED` (open; nothing here delivers or pages anyone) |
| Closed-tenant player-funds path (ADR 0107) | design only, `NOT IMPLEMENTED` |
| Casino / sportsbook 4xx mapping of the all-sessions ledger trigger's `MR020` (O-1) | `NOT IMPLEMENTED`: the posting is refused by the database (tested), the HTTP layer returns its generic 5xx |
| Backoffice permissions (`backoffice/src/auth/permissions.ts` + test) | `IMPLEMENTED`; `payments` did not run vitest (no `node_modules` in its environment). QA reports PASS (local): vitest 17 files / 96 tests, `tsc --noEmit` clean (local, not CI) |

### 27.2 What was verified

Tests (all `-race -tags integration -count=1 -p 1`, private database; PASS unless stated):
- the K3 suites: `internal/payments` (`TestK3_*`: C-1..C-50, T-1..T-18 and the LF additions as numbered in §12), `internal/reconciliation`
  (`TestK3_*` S1-S4/C-34..C-36/C-38/C-43/C-44/C-14d/C-19+, plus the edited D2 tests), `internal/providerref`, `internal/httpserver`
  (`TestForceResolutionAPI_*`: T-12, T-14), `internal/auth`;
- the C-12 class (concurrent final approvals, M2 versus the sweeper, K2 compensation versus M2, the Step B cap) under `-race -count=50`: PASS;
- the touched and neighbouring packages in full: `internal/payments`, `internal/reconciliation/...`, `internal/withdrawal`, `internal/adjustment/...`,
  `internal/ledger/...`, `internal/auth/...`, `internal/providerref/...`, `internal/httpserver`, `internal/jurisdiction/...` (the 0075/0077 chain rollbacks;
  the 0077 whitelist stays at 11), `internal/db` (after the allowlist addition in §27.4), `cmd/...`: PASS. SWEEP_RESULT
- migration chain: `cmd/migrate verify` reports every applied migration clean; against this branch alone it reports the **0114 version gap** (E1's
  migration is not merged here); against this branch plus E1's `de8caba` migration files in a throwaway copy it reports "all applied migrations
  verified clean, no version gaps", and the C-16/T-18, C-17, C-9c, jurisdiction and `internal/db` chain tests PASS there;
- `go build ./...`, `go vet ./...` and `go vet -tags integration ./...`, `gofmt -l cmd internal`, `golangci-lint run ./...` (0 issues) and
  `--build-tags integration` on the changed packages (no finding in a K3 file).

### 27.3 Mutation testing

`docs/plans/payment-readiness/evidence/prh2-k3-mutation-kill.txt`: 106 mutants (every §12 mutant, the revision 4 extras, and extras found
while testing), run against a throwaway copy of the worktree with a restore and `cmp` after every mutant (0 differences) and a passing control
run of the unmutated copy. **92 killed, 14 survived**, and each survivor is classified in the evidence file:
- `S24` (LIKE instead of `left()`): **EQUIVALENT**, as recorded in §12 (T-16); never counted as killed.
- Equivalent or redundant under an invariant: `S01`/`S39` (`executed_txid = txid_current()`: an `executing` row is only ever visible inside its own
  transaction because the deferred check forbids it committing), `S03` (`target_state` is derived 1:1 from `kind`), `S20` (the prefix term of the Step B arm is
  implied by the executed-M2 link), `S36` (a CHECK duplicates the M1 no-link guard), `E02` (the Go allow-list restatement is unreachable behind the
  insert guard and the R-6 pin), `G14` (idempotent consumers of duplicate persisted lines).
- Redundant layers: `S10a/b/c`, `SJ2`, `SJ3` (the `executing` predicate in the fences: a second governed posting after `executed` is impossible
  through the ledger idempotency uniqueness and the two-leg cap), `G05` (the explicit `tenant_id` predicate of the persisted lookup: `FORCE` RLS in
  `WithTenantSnapshot` is the second line).
- Mutants killed only by a static pin and not behaviourally (`V02`, `V03`: a second validator downstream also refuses the prefix) are counted killed
  by the C-9b pin.
- The first run left 32 survivors, which produced real tests (not allow-list edits): C-5c (the attempt guard's own withdrawal-state re-check), C-45b/c
  (MOCK reversal eligibility, duplicate-copy eligibility), the C-8 settlement-reference exemption, and `TestK3_X01..X12` (the NULL-safe fence on a
  house-level hold account, the committed `executed` verifier, evidence back-fill and the evidence INSERT policy, the closed-tenant recount, the
  payload-hash oracle, the ledger prefix trigger's type and executing binding, the id sentinel, stale-hash approvals, direct reject/cancel/expire/
  execute attacks, expiry, policy-author approvers).

### 27.4 Deviations and amendments (nothing silently changed)

1. **PostgreSQL's 63-byte identifier limit** truncates two constraint names that §20.1 spells in full: `payment_provider_events_original_provider_reference_no_reserved_prefix`
   and `payment_statement_lines_original_provider_reference_no_reserved_prefix` exist as `..._no_reserved`. The C-16 object list uses the real names.
2. **The R-2 system-read policy** was first written as §20.1 states it (`state = 'executed'`), which also exposed executed **M1** rows. **Tightened in the
   fix batch (§27.6):** the policy now also requires `kind IN ('m2_declare_paid', 'm2_declare_not_paid')`; T-3 asserts an executed M1 is invisible.
3. **`internal/db/null_arm_replay_static_test.go`** (not in §7) gained one line: `payment_manual_resolution_codes` is added to the family-R `a18SelectAllowlist`
   (the K2 precedent for `ledger_adjustment_reason_codes`). Without it `TestA18_*` and `TestK2G1_*` fail on the new reference table.
4. **Audit metadata key names** are `tenant_status_at_submit` and `ever_possibly_sent_at_submission` (the first run wrote a truncated key; fixed and pinned
   by `TestK3_C15_T15_*`).
5. **Error mapping.** `MR030`/`MR031` and the other `MR*` codes without a named class map to the closed token `force_resolve_conflict` (409); `MR003`, `MR011`,
   `MR032`, `42501` map to `force_resolve_not_permitted` (403); `MR014` to `force_resolve_disabled`. A self-approval therefore returns 409, not 403 (T-12/T-14
   accept either; the body is always one of the closed tokens).
6. **Id sentinel:** the resolution `id` default is the nil UUID and the guard refuses any other client-supplied value (`MR030`), then forces `gen_random_uuid()`.
7. **C-17b scope:** the differential matrix tests the `payment_attempts_guard` function (0107 text installed under another name versus head) on shadow
   tables; the column-discipline trigger is tested separately (`TestK3_T10`).
8. **Expiry** is 24 hours, copied from K2 (a technical default, not a legal value). Tests backdate it in a scratch database only.
9. **C-9c exempt list** (closed, in the test): `payment_manual_resolutions.reserved_provider_tx_id`, the two `casino_callback_rejections` provider-tx columns (a
   rejection log; the ledger trigger covers postings) and `kyc_verifications.provider_reference` (a KYC vendor reference).
10. **`deploy/init-app-role.sql`:** the K3 block is appended; E1's `de8caba` also appends a block, so the merge will conflict trivially: keep both.

### 27.5 Residuals

PAY-PAYOUT-DISPUTE-ALERT-1 and ALERT-DELIVERY-1 (no delivery, no recipients); the `psp_clearing` residual of a declared-paid payout; MA020 stranding after
an M2; the closed-tenant hold-release path (ADR 0107); real-PSP behaviour; statement-source registration in the binary; casino/sportsbook 4xx mapping
of `MR020`.

### 27.6 Fix batch after the implementation reviews (2026-10-05)

Applied after the ledger-finance, code-review and security implementation reviews of `041fb55` (`docs/plans/prh2-hardening-round/reviews/k3-impl-*.md`), on
`prh2-k3-impl` merged with main `a642e8c` (E1, migration 0114; `deploy/init-app-role.sql` has one combined block listing both sets of tables; the 0095 §38/§39 and runbook §13/§14 sections keep both):
- **LF F-1 / security PM-S2 (real defect, fixed).** A confirming line now resolves to the attempt by provider reference, or by merchant reference only when no
  other attempt of the same kind holds the line's reference (`payMatcher.resolvesTo`); raising predicates stay broad. Regression `TestK3_C42b_*` and `TestK3_C42c_*`, killed mutants F01, F02, F03 (evidence file) and LFD8.
- **Security PM-S1 / code-review F-1.** Every function 0115 creates (18) carries `SET search_path = pg_catalog, public, pg_temp`. `TestK3_Y01` pins the
  live `proconfig` of exactly that list and counts the migration text; `TestK3_Y02` shows a TEMP table named `payment_manual_resolutions` cannot defeat the reserved
  namespace trigger. The down migration restores the previous bodies (no SET); the C-16/T-18 round trip, C-17 and C-9c pass.
- **Security PM-S3/PM-S4, PM-S5.** Direct UPDATEs of the pending payload columns raise `MR030` (`TestK3_Y03`); the pinned required count survives a lower policy
  (`TestK3_Y04`); the denied audit row now carries the SQLSTATE CODE (never message text) and the HTTP tests assert it for the 403 classes and the foreign tenant.
- **R-2 tightened** (above). **LF F-2..F-6:** `TestK3_Y05` (an unrelated debit never clears (d) or (c2)), `TestK3_Y06` ((c2) is raised by any succeeded line),
  `TestK3_Y07` (the A8 L1 share locks serialise a revoke against an execution, using `testHookResolutionAfterShareLocks`), `TestK3_C34b` (holder attempt in the S1 detail),
  and the (d) hint no longer promises an off-platform recording path. **Code-review F-2/F-3:** `TestK3_Y08` (requester Person unchanged at execution, scratch DB),
  and the HTTP leakage assertion now checks every collected refusal body (non-vacuous). `TestK3_Y09` (requester who later authors a policy), `TestK3_Y10` (`refused_at_execution` needs a same-transaction approval).
- **Wording** of ADR 0095 §35.4/§39.3/§39.4 and `reconciliation-model.md` applied as ledger-finance wrote it.
- **REAL-MONEY PRECONDITIONS (not optional, not docs-only):** PAY-K3-STATEMENT-SOURCE-WIRING-1 (IMPLEMENTED in §28 against the MOCK source; originally: populate the registry from the SAME list given to the
  scheduler, and refuse `m2_declare_paid` as well when no source is scheduled); ALERT-DELIVERY-1 and PAY-PAYOUT-DISPUTE-ALERT-1; WITHDRAWAL-REVERSAL-1 or a governed path for the
  `psp_clearing` residual; a real non-MOCK statement source (PROVIDER DEPENDENT); CAS-RECON-SCALE-1; the ledger-finance RM-1..RM-7 list. The orchestrator owns the registry row.
- **Deferred (docs only):** PAY-K3-MR020-HTTP-MAPPING-1 (casino/sportsbook: a crafted provider id gives a 5xx a provider may retry; the database refuses it);
  Z28/Z29 (optional static route-permission pin); LF F-8/F-9 and code-review F-4/F-5 (optional hardening: refuse a second M1 once one executed; expire a stale pending row in `Request`).
- **Second reference-allowlist edit (merge with E1).** E1's worker visibility gates (`internal/db/kyc_worker_identity_integration_test.go`,
  `workerReferenceAllowlist`) also gained `payment_manual_resolution_codes` (the same family-R reference table; the K2 precedent). Both allowlist edits are test-only.
- **Verification of this batch (local, private DB; not CI):** `go build`, `go vet` (both tag sets), `gofmt`, `golangci-lint run ./...` (0 issues; no finding in a K3 file with
  the integration tag); `cmd/migrate verify` clean with migrations 0001..0115 contiguous (E1's 0114 merged); `go test -race -tags integration -count=1 -p 1 -skip
  'TestStoreOutage_DoesNotPinPool|TestResolutionIsolation_' ./...` PASS for every package (the `internal/db` and `internal/kyc` gates re-run PASS after the allowlist line).
  Mutation: 21 new mutants, all KILLED after three test strengthenings (evidence file, "FIX BATCH" section); the 14 classified survivors of the original run are unchanged.
- **Sweep command (state the timeout):** `go test -race -tags integration -count=1 -p 1 -timeout 30m -skip 'TestStoreOutage_DoesNotPinPool|TestResolutionIsolation_' <pkgs>`; the full
  `internal/payments` package takes about 23 minutes under `-race`, so the default 10-minute `go test` timeout is NOT enough.
- **Code-review survivors N01, N03, N05, N10, N13, N18: accepted, with reasons (not test gaps in the money path).** N01 (insert-guard "withdrawal submitted" dropped) and N18 (executor Go
  `wr.State != submitted` re-check dropped): redundant while no payout cascade or staff-side withdrawal Fail/Complete path exists (a withdrawal leaves `submitted` only with its single attempt's terminal
  transition; `payment_m2_admits`, the `-> executing` guard and `withdrawal.Complete/Fail`'s CAS remain; C-5c pins the admits layer); required if such a path is ever added. N03 (prefix guard drops
  `correlation_id = withdrawal_request_id`): redundant (the provider+reserved-id key is unique per tenant and the deferred verifier checks the correlation). N05 (approvals guard drops `state = 'pending'`):
  reachable only by direct SQL, which can add `approve` rows to a non-pending resolution (pollutes the append-only approvals log; a `reject` still fails MR030); accepted. N10 (`poll_evidence.go` drops
  `live &&`): a DECLINED deposit that later gets a poll success with a different valid echo would abort the poll transaction at the evidence guard and lose the P1 contradiction audit; no cheap fixture
  exists without payments internals, so it is an accepted residual (the audit is lost, no money moves). N13 (the Step B `reverses_transaction_id` clearing branch of (c)): LATENT, no writer exists
  (WITHDRAWAL-REVERSAL-1 not implemented); **launch flag:** add the scratch-DB test with that writer, or remove the branch, before any real-money enablement.
- **Post-fix concurrency run (recorded by ledger-finance, local):** `-race -count=20 -run 'TestK3_C12|TestK3_C26|TestK3_Y07' ./internal/payments/` PASS (392s). The earlier `-count=50` run refers to `041fb55`.
- **Residual (security finding 1, pre-existing K2, HIGH, launch-blocking):** clearing of (d) and (c2) trusts executed K2 `compensating_entry` requests. Those can be forged through a TEMP-table
  shadow on the unpinned 0112/0113 helper functions until TRIGGER-SEARCH-PATH-1 is fixed for them; 0115 pins only its own 18 functions.

## 28. Implementation record: round-2 PSP prerequisites (2026-10-05, amendment; no migration)

Go and tests only; migration 0118 stays unused. Full text: ADR 0095 §40. Labels are `IMPLEMENTED` against MOCK; real-PSP behaviour `PROVIDER DEPENDENT`. Closure of the registry rows is the orchestrator's decision after independent review.

### 28.1 PAY-K3-STATEMENT-SOURCE-WIRING-1 (supersedes the §27.6 precondition wording)

- The statement-source registry is filled in `cmd/platform-api` from the SAME list given to `RunSchedulerLoop` (one variable, pinned by a static test) and injected through `httpserver.Deps.StatementSources`; the package-level `DefaultStatementSources` is removed. A nil registry refuses every M2.
- **Both** `m2_declare_paid` and `m2_declare_not_paid` are refused at submission and at execution with the existing `no_statement_source` refusal when no source is registered for the attempt's provider. "No non-MOCK source" is evaluated **per provider** (orchestrator engineering ruling H-W2, reversible; PRH-2-ROUND2-ENGINEERING-RULINGS): the MOCK source unlocks only `mock-payments` attempts; every real provider stays refused. The submission check is fail closed in shape.
- Startup fails on a nil source, an empty provider id or a duplicate; the readiness gate refuses a real payments adapter without a real statement source of the same provider id, and a MOCK source carrying a real adapter's id.
- **Dependency (sequencing).** The wiring makes `m2_declare_not_paid` executable for the MOCK provider, and the clearing of (d) and (c2) trusts executed K2 `compensating_entry` rows. Those rows are protected against forgery only once migration 0116 (REVOKE TEMP, TRIGGER-SEARCH-PATH-1; a separate branch) is merged. Do not enable this wiring in a shared environment before 0116.
- Residuals: the registry is in-process (scheduled, not "last run succeeded"); removing a source silently ends its standing findings; **H-W1**: the stream swept `active` tenants only, while R-5 admits M2 on a closed tenant through `platform_acting`. Sweep scope was not changed here; **superseded by §28.3 (owner decision 2026-10-05: non-active tenants are observed, evidence only)**.

- **Authorization order (security F-3) and audit (F-2):** the Go source check runs after the INSERT guard (rolled back on refusal), so a requester without a grant gets the audited 403 first, and an M2 naming an unknown or foreign attempt is refused and audited like M1. HTTP-level tests cover refused-when-unregistered (closed token + audit row), accepted-when-registered, and the foreign-tenant attempt.
- **Launch blocker (security F-4 / H-W1) and merge order (LF F-10):** see ADR 0095 §40.1: closed tenants and per-tenant fetch failures leave M2 unlocked without the detective control; this must be resolved before a real-money tenant on a real PSP. Migration 0116 must reach main before or with this branch.

### 28.2 Reconciliation (ADR 0095 §40.2, §40.3)

- PAY-RECON-PARKED-CAPTURE-STANDING-1: M-S1 cross-run test, M-S2 ruling (bound park matched by a pending/declined line still reports; `reversed` stays the in-run clear), sibling-success pin. `IMPLEMENTED` against MOCK; residuals R-S1..R-S4.
- PAY-RECON-POLL-REF-CLEAR-1: G-Y1 (`yAttributable`, including the shared-Y rule), G-Y2 (required by ledger-finance: with X and Y both evidenced, both must be cleared) and the G-Y3 tombstone test. `IMPLEMENTED` against MOCK; closure is the orchestrator's decision. MA020-SYNC-MISMATCH-1 stays OPEN and must reuse `yAttributable` and the G-Y2 rule.
- Mutation evidence: `docs/plans/payment-readiness/evidence/prh2-r2-psp-prereq-mutation-kill.txt`.

### 28.3 R-5 observation note: non-active tenants are observed, resolution is not changed (PRH-2 R3, H-W1; owner decision 2026-10-05; `IMPLEMENTED` against MOCK; no migration)

- R-5 is unchanged: a closed tenant admits M2 only through `platform_acting` actors (K3-S2, MR030 for a tenant-scope requester or approver), with the full four-eyes count of the policy in force. Nothing in the observation can request, approve, execute, cancel or expire a resolution, and nothing in it posts to the ledger or changes an attempt, withdrawal or intent. M2 on a closed tenant stays gated as before; ADR 0107 stays DESIGN ONLY and its mechanism stays disabled.
- What changed is detective only: the `payment_statement` stream now also runs, evidence only, for tenants whose status is not `active` (ADR 0095 §40.4), so the standing kinds that an M2 depends on (`pay_declared_paid_unconfirmed`, `pay_declared_not_paid_but_paid`, `pay_declared_paid_compensated_but_paid`, `pay_captured_unposted`, `pay_reference_mismatch`) keep being raised, alerted and visible to platform staff for a closed tenant. This closes the first half of the security F-4 / H-W1 launch blocker (closed tenants unswept) against the MOCK source. The second half (registered means scheduled, not "the last run for (tenant, provider) succeeded"), R-S1, a real statement source and ALERT-DELIVERY-1 remain real-money launch blockers.
- Staff can tell an observation run: its `reconciliation.sweep_run` audit record carries `non_active_tenant_observation: true` and `tenant_status`.
