# Ledger-finance review — ADRs 0099–0101 (2026-09-28)

Reviewed at `160b557`, read-only. Checked against LF-1..LF-18, 0098 §5, plan §11 and product-owner-proxy's review. PostgreSQL behaviour marked *(not executed)* was reasoned about, not run.

| ADR | Verdict | Blocking before |
|---|---|---|
| 0099 (K1) | **ACCEPT WITH CONDITIONS** | K1 may start; the F1 projection fence is needed before K2 |
| 0100 (K2) | **ACCEPT WITH CONDITIONS** | K2 code: F4–F8 and rulings 1–4, 6 and 7 written into the ADR |
| 0101 (K3) | **ACCEPT WITH CONDITIONS** | K3 code: F9–F11 (HIGH) and ruling 5 |

No veto trigger applies: integer minor units (NUMERIC(38,0) with an int64 ceiling), no historical mutation, no direct balance write (only `ledger.Post`/`Complete`/`Fail`), and a DB idempotency key on every money path.

## Mandatory checks, verified in code

| Check | Result |
|---|---|
| Deposits never leave `disputed` | Holds. 0114 adds only payout pairs; there is no `failed` state (0101:94-97); the 0107 whitelist has no `disputed→*` pair (172-192). See F12. |
| M1 is evidence-only and never clears `pay_captured_unposted` | Holds. The clearing predicate reads only reversals and tombstones (`payment_statement.go:977-979`), and the check is unwindowed (989-1002). The detail text is stale (F13). |
| 0114 deposit branches and both 0107 indexes byte-identical | The indexes are untouched (0107:78-80, 87-89). "Byte-identical branches" cannot be literally true, because the evidence gate at 0107:259 is shared by deposits and payouts. See F12. |
| `decided_txid`/`executed_txid`, never `xmin` | Holds. `txid_current()` is the top-level xid inside `ledger.Post`'s savepoint *(not executed)*. |
| Closed posting shapes | Holds. `manual_adjustment` ↔ `player_cash`, same asset, two legs; `player_cash` is not in BONUS_SET. |
| No-negative check via `LockProjectionsForPosting` | Holds (`lockorder.go:94-124`, `:86`). |
| Idempotency, correlation and causation keys | Hold (0021:43; `ledger.go:265-296`; 0051:72; `lockorder.go:346,355`). |
| M2 requires `submitted` | Holds, three times (`withdrawal.go:1139-1147,1417,1507`), plus the 0114 guard. |
| Reserved-prefix refusal | Holds at payout ingress, callbacks and statement lines. **No `providerref` call on the deposit sync or poll paths**, so "every ingress" is not verified; the DB CHECK and trigger are the backstop (F14). |

## Findings

| ID | Sev | ADR | Finding | Required change |
|---|---|---|---|---|
| F1 | **HIGH** | 0099 §6.4–6.5, TM-4 | The acting family gets INSERT/UPDATE on `wallet_balance_projection`, and the fence covers only `ledger_*`. So an acting session could UPDATE a projection directly, or call `RebuildProjectionRow`: a direct balance-write path for a new privileged family. | A trigger on `wallet_balance_projection`, in acting sessions only: INSERT allowed only with zero totals (`lockorder.go:214-219`); UPDATE only when `pg_trigger_depth() >= 2`, i.e. from 0023's projection trigger *(not executed; test A-4b)*. |
| F2 | MED | 0099 §6.5, 0101 §5.3 | The fence predicate is loose. | Pin it exactly: (a) `manual_adjustment` binds the idempotency key to the request, `executed_txid`, tenant and `correlation_id`; (b) `withdrawal_completed` binds to an executing `declare_paid` resolution on `correlation_id` and `reserved_provider_tx_id`; (c) `withdrawal_failed` binds to `declare_not_paid` with key `wr.id||':failed'`; (d) the reserved-prefix trigger on `ledger_transactions` applies to **all** sessions and uses `left()`, not LIKE. |
| F3 | MED | 0099 §7.4, 0100 §7 | Plain READ COMMITTED reads let an execution count a grant whose revoke commits concurrently. | Read the counted grants and `staff_users` rows `FOR SHARE` at execution, in L1 (ruling 6). |
| F4 | **HIGH** | 0100 §11, §14.3 | With detection only, any credit adjustment can hand-pay a captured-but-unposted deposit, and a later PSP refund then pays it again, bypassing INV-DEP-1. The detector exists only where a statement source is wired, and all sources are MOCK. | Make it preventive (ruling 2). |
| F5 | MED | 0100 §5.2, §5.4 | `compensating_entry` is unconstrained beyond "causation required". | Ruling 1. |
| F6 | MED | 0100 §5.4 | `external_instruction`'s reference is not covered by the payload hash. | Pin `evidence_ref_hash` in the payload (required for `external_instruction` and `compensating_entry`). |
| F7 | MED | 0100 §3.4 | Option (a) is described as "a one-line change". | Ruling 7. |
| F8 | LOW | 0100 §12 | The fixture count is **19** files, not 24 (worktree copies inflated the count). | Correct it (ruling 4). |
| F9 | **HIGH** | 0101 §5.1 | M2 on an `amount_asset_mismatch` payout would post the full `wr.Amount`, which provider evidence contradicts, or release the full hold. | Allow-list: `provider_reference_mismatch`, `success_for_never_sent_attempt` and `ambiguous`. **Refuse** `amount_asset_mismatch` for both M2 kinds, in the guard and a trigger. Register PAYOUT-AMOUNT-DISPUTE-1. |
| F10 | **HIGH** | 0101 §5.3–5.4, §14.2 | Disputed attempts are excluded from status comparison (`payment_statement.go:950`). So "declare not paid, then paid" (a real double payout) and "declared paid, never confirmed" drop out of reconciliation. | Ruling 5: two unwindowed standing kinds. |
| F11 | MED | 0101 §5.3–5.4 | Correcting via `compensating_entry` restores the player but leaves `psp_clearing` misstated. | The ADR and runbook say so; the residual is tracked by the F10 kinds; register WITHDRAWAL-REVERSAL-1 (`TxWithdrawalReversed` exists; the transition is not implemented). Not K3-blocking. |
| F12 | MED | 0101 §2.5, §8, C-17 | "Byte-identical" is imprecise. | 0114 = 0107 except: (i) inserted `OR (payout AND <M2>)` whitelist lines; (ii) the two evidence gates rewritten as `<0107 predicate> AND NOT (payout AND <M2>)`; nothing else. C-17 asserts exactly this diff, plus an exhaustive deposit matrix test giving identical results on 0107 and 0114. |
| F13 | LOW | 0101 §4 | The detail text says "or M1/allocation (BLOCKED)" (`:949,:1002`). | Change it to "PSP reversal/tombstone, or allocation (LEDGER-SUSPENSE-B-1); M1 only acknowledges". |
| F14 | LOW | 0101 §5.3 | The deposit sync and poll paths are not validated by `providerref`, which is shared with casino and sportsbook. | Validate at deposit sync and poll, or restate the claim. C-9 covers every ingress. |
| F15 | LOW | 0100 §7 | Approval inserts are mislabelled "L4". | Ruling 6. |
| F16 | INFO | 0101 §3 | Concur with deferring `pending_suspense_allocation_b`. | — |
| F17 | INFO | 0101 §6.3 | `Fail` and `Complete` keys and releases are correct. | — |

## Rulings

1. **Catalogues.**
   - `operational_error_correction`: both directions. Causation optional; if present, it must touch this wallet's `player_cash`.
   - `compensating_entry`: both directions; causation **required**. The causation must have a `player_cash` leg on **this wallet** in the same asset, and must not be of type `deposit`, `deposit_reversal` or `tombstone`. Cumulative executed compensations per causation, per direction, must not exceed that leg's amount (checked at execution under the lock). `evidence_ref_hash` required.
   - `goodwill_credit`: credit only; withdrawable cash only (wagered goodwill goes through bonus-engine). Refused in a suspended asset and under an open payment exposure.
   - `external_instruction`: both directions; `evidence_ref_hash` required and pinned.
   - No deposit-allocation code.
   - Finding codes confirmed: `awaiting_psp_refund`, `refund_requested_from_psp`, `investigated_no_platform_action`. Do not seed `pending_suspense_allocation_b`.
   - Basis codes: every M2 needs `evidence_ref_hash` NOT NULL. `m2_declare_paid` requires `provider_confirmed_out_of_band` or `reconciliation_exhausted` (a success line). `m2_declare_not_paid` requires `provider_confirmed_out_of_band` or `reconciliation_exhausted` (coverage past the attempt with no line). `provider_unqueryable` and `past_resubmission_horizon` are secondary `context_code` values only. A CHECK enforces the pairs. The vocabulary is `payments`' to confirm.
2. **The goodwill residual is PREVENTIVE.**
   - Refuse every `credit_player` adjustment, whatever its reason code, at submission **and** at execution (SQLSTATE class `MA`, `open_payment_exposure`) while the player has a `disputed`/`multiple_success_for_intent` deposit attempt with no reversal or tombstone on its reference.
   - This is the `capturedUnposted` predicate, evaluated from the ledger and attempts, with **no statement source needed**. One SQL function is used by the trigger and the executor.
   - The acting family needs SELECT on `payment_attempts`/`deposit_intents` for tenant X (add to 0099 §6.4).
   - No override.
3. **Suspended asset.** Allowed: `compensating_entry`, `operational_error_correction` and `external_instruction`. Refused: `goodwill_credit`. The asset must exist and equal the wallet's; re-evaluated at execution.
4. **Linking `manual_adjustment` to a request.**
   - **In K2:** the fence plus the static single-caller test, plus a **detective P1** mismatch kind `ledger_unlinked_manual_adjustment` for any post-cutover `manual_adjustment` that no executed request links (widen the 0112 CHECK).
   - **Preventive:** registered as LEDGER-MANUAL-ADJ-LINK-1, launch-blocking for the first real-money tenant. A BEFORE INSERT trigger for all sessions; the 19 fixtures move to request-backed helpers. **No test-bypass GUC in the production schema.**
5. **Reconciliation matching for M2-declared payouts (K3 implements):**
   - (a) A reserved-prefix `withdrawal_completed` skips the settlement-reference comparison; amount and asset are still compared.
   - (b) Its lines resolve `byRef`, then `byMerchant`. A matching succeeded line counts as a confirmation (metric); a declined line → `pay_status_mismatch`.
   - (c) Standing, unwindowed **`pay_declared_paid_unconfirmed`**, until a confirming line or a `compensating_entry` with causation = that Step B.
   - (d) Standing, unwindowed **`pay_declared_not_paid_but_paid`**, on T14 `disputed` or a succeeded line, until a recovery debit with causation = `withdrawal_failed`. It overrides the `:950` disputed exclusion.
   - (e) The reserved id is never expected on a statement.
6. **ADR 0082 A8: ACCEPTED, corrected.**
   - L1 order: … `withdrawal_requests` … `deposit_intents`, `payment_attempts`, then **`payment_manual_resolutions`, `ledger_adjustment_requests`**, then **`staff_users`, `staff_capability_grants` `FOR SHARE` (ascending id)**, all before L3.
   - M2 order: withdrawal → attempt → resolution. M1 order: intent → attempt → resolution.
   - Approval inserts happen while the L1 request/resolution is held, so they belong to no lock class (not L4).
7. **HD-PRH2-8 interim: CONFIRMED.**
   - The trigger applies at every level. `financial_policy_required_approvals()` returns `GREATEST(1, MAX(...))` for the mandatory class. `above >= base` stays a CHECK.
   - Moving to (a) needs ledger-finance sign-off **and** an implemented cumulative-window (structuring) control per initiator and per player first. It remains a human decision.

## Required tests (in addition to the ADRs' own)

1. A-4b: an acting session cannot write a projection directly (UPDATE, rebuild, non-zero INSERT all refused), while a governed post succeeds and drift stays zero.
2. Fence binding: a W′ correlation, a mismatched `provider_tx_id`, or a reserved prefix on any other type in any session is refused.
3. Revoke vs execution (`FOR SHARE`): revoke first → not counted; execute first → the revoke waits.
4. Ruling 2: a credit under an open exposure is refused (at submission and execution) without any statement source; admitted after a tombstone or reversal; debits unaffected.
5. Ruling 1: `compensating_entry` with another wallet, another asset, a `deposit_reversal`/`tombstone` causation, a cumulative excess, or a missing hash is refused.
6. Ruling 3: in a suspended asset, goodwill is refused and compensation succeeds.
7. Ruling 4: an unlinked post-cutover `manual_adjustment` produces `ledger_unlinked_manual_adjustment`.
8. Ruling 7: a fixture policy row with `base = 0` still evaluates to a requirement of 1.
9. F9: M2 on `amount_asset_mismatch` is refused by the executor and the guard; `provider_reference_mismatch` and `ambiguous` are admitted.
10. F10/ruling 5: a standing `pay_declared_not_paid_but_paid` across N runs (including runs whose coverage excludes the payout), clearing only on the linked recovery debit; `pay_declared_paid_unconfirmed` standing; a real matching line gives no `pay_reference_mismatch` or missing-record finding.
11. F12: an exhaustive deposit matrix, identical on 0107 and 0114, run on a HEAD-migrated DB; C-17 asserts the diff.
12. F13: no detail string says M1 clears.
13. F14: a reserved prefix is refused at every ingress (deposit sync and poll, payout sync and poll, callback, statement).
14. Ledger invariants on every K2/K3 test: SUM(D) = SUM(C); projection = recomputed; M1 produces zero ledger transactions; the 0107 index definitions are unchanged.

**Not verified:** the `payment_provider_events` column names; adapter-side deposit reference validation; a late decline after "declare paid"; `pg_trigger_depth()` and savepoint `txid_current()` behaviour (tests 1–2 must prove them).
