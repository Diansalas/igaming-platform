# RV-0095 — Ledger-finance re-verification of ADR 0095 (revision 2)

**Reviewer:** `ledger-finance`. **Date:** 2026-09-27. **Repo:** requested base `13b44b5`; working
HEAD at review time was `9a2bf93`. The two commits since `13b44b5` touch only
`docs/governance/task-registry.md` and `prh-ref-provider-reference-bound.md`, so the ADR 0095
text reviewed is the text of `13b44b5`.

**Scope.** I checked my conditions LF95-C1..C14 (§21.6) against the ADR **design text**, not the
§27.1 revision record:
- §2 (INV-IO-4/7/9);
- §4.1–§4.8;
- §5.1, §5.2 and the new §5.2.1;
- §6.1–§6.4 and §7.1–§7.2, where the conditions depend on them;
- §8;
- §10.1 (registration rules);
- §12.3;
- §13.1, including the revised 0101 backfill;
- §14 (A7 and the lock order);
- §16.1 CP-D1/CP-W7/CP-W8 and §16.2 items 19 and 22.

I also cross-checked it against:
- ADR 0082 (R8, and the L0.4 RG person advisory);
- `internal/rg/rg.go` `EvaluateEligibility`, which takes `lockPerson` at :607;
- `internal/withdrawal/withdrawal.go` (the `Reject`/`Fail` lock sequence);
- migration 0026 (the `release_ledger_transaction_id` column);
- ADR 0096 at origin (see the LF95-C10(f) section below).

The ADR was not edited. Nothing is committed.

## Verdict

**SIGN-OFF STILL CONDITIONAL. No veto.** None of these is present:
- floating point;
- a historical mutation;
- a direct balance `UPDATE`;
- a money path without an idempotency key.

In substance, 12 of the 14 conditions are met in the text. Two are **OPEN**:
- **LF95-C6(d)**: a text contradiction that the trigger would turn into a runtime failure.
- **LF95-C10(f)**: the ADR 0096 side is still missing.

I also found four new defects:
- **N1**: a lock-order violation of ADR 0082 R8 in the deposit T2 per-item transaction.
- **N2**: payment-attempt INSERTs are not guarded by a trigger.
- **N3**: T12 can re-send a live sibling after T13.
- **N4**: the backfill leaves columns unset that decide convergence.

All of these are text fixes, with no redesign. **PRH-I1 must not start until N1–N4 and C6(d) are
fixed in the ADR text.** The §27.1 statuses "Satisfied" for C6 and C10(a)–(e) are accurate for
C10(a)–(e) only.

## Per-condition verdicts

| Cond. | Verdict | Basis (design text) |
|---|---|---|
| **C1** — `NotProcessed`/`NotSent` vs INV-IO-9 | **SATISFIED** | §8: "left no trace" → `NotSent`/T5, otherwise T6; a resend goes only through T12, for deposits and payouts. The "Yes for deposits" wording is gone. §4.3: the T5 guard has `NOT ever_possibly_sent` and `claim_token`. T6 lists "`NotSent` on a T12 resend". Consistent with the §13.1 CHECK. |
| **C2** — trigger-visible evidence kind | **SATISFIED (as worded), but see N2** | §4.2 has the column, written in the same UPDATE, and the audit record carries the same value. The §13.1 CHECK and `payment_attempts_guard` implement all three rules. MX16 is present. The guard is `BEFORE UPDATE` only, so INSERTs escape it (N2). §4.2's enumeration omits `legacy`, which §13.1 allows (L1). |
| **C3** — provider-bound resolution; conflicts decided before writes; T10 committed | **SATISFIED** | §4.4 preconditions 1–2, §6.1 step 4, and the §6.2 `anomaly` row (the state effect is committed, and the 409 rollback is superseded). Residual note: two attempts binding the same reference concurrently can still produce **one** unique-violation 5xx. On redelivery, step 4 then classifies it as `anomaly`. That converges and is not a loop, so it is acceptable. |
| **C4** — evidence completeness | **SATISFIED** | Covered by: §4.4 precondition 3; T7 (reference required); the §13.1 attempt CHECK `succeeded ⇒ provider_reference`; the receipt `cascadable`/`decline_stage`/`decline_reason` columns; the `succeeded ⇒ amount, asset` CHECK; the `declined ⇒ cascadable, decline_stage` CHECK; and §4.6 ("a receipt without it can never cascade"). |
| **C5** — no unconvergeable success | **SATISFIED** | Covered by: the §10.1 `CallbackEchoesMerchantReference` registration refusal; the §6.4 last row (P1 after `SettlementWindow`); §7.1; and §12.3 `pay_unresolved`. |
| **C6(a)** — per-attempt ledger link | **SATISFIED** | §5.1 "Ledger link"; T7; the §13.1 column, the unique index and the deposit CHECK `succeeded ⇒ ledger_transaction_id`. |
| **C6(b)** — reversal resolves through the attempt | **SATISFIED** | §5.4 "Reference". |
| **C6(c)** — T13 sibling handling | **SATISFIED for `created` siblings; see N3** | T13 moves a `created` sibling to T3 `intent_succeeded`, and the T2 CAS has `NOT EXISTS(succeeded …)`. The residual for a `submitting` sibling is stated in §20. The T12 guard lacks the same predicate (N3). |
| **C6(d)** — tombstone before success → terminal `disputed`, no loop | **OPEN (text contradiction)** | T7 → T10 is consistent: T10 is allowed from `submitting/pending/ambiguous`. **T13 is not.** §4.3 T13 says "Tombstone → T10 instead", but T10's CAS and the trigger allow only `{submitting,pending,ambiguous}`, so the trigger would reject `declined → disputed` for a deposit, and the only such pair, T14, is payout-only. §4.4 row `declined` says the opposite: "tombstone → P1 anomaly, no change". §4.1 `disputed` and §16.2 item 19 ("leads to `disputed`") assume the T10 outcome. Either outcome is money-safe (no posting, no loop), but an implementer following §4.3 hits a trigger exception, which means a rollback and a 5xx redelivery loop: exactly what C6(d) exists to prevent. **Fix:** pick one. My preference is to add `declined → disputed` for `operation='deposit'` with reason `reversal_tombstone_precedes_success`, as a named T (for example T13t), to the table, the trigger and the matrix, so the case sits in the M1 queue. The alternative is to state "P1 anomaly, no change" in §4.3 T13, §4.1 and item 19. Add a test for the T13 variant (the attempt starts in `declined`). |
| **C7** — intent projection with `disputed` | **SATISFIED** | §5.1 "Intent" evaluation order. |
| **C8(a)–(d)** — authoritative not-found | **SATISFIED** | §4.5 (a)–(c) cover this. The `cascadable=false` rule is "enforced in code" (§4.5, §4.6). §4.2 and §13.1 have `last_sent_at`, set by every claim. §10.1 has the `MerchantLookupAuthoritativeAfter` refusal. |
| **C9(a)–(e)** — A7 scope rules | **SATISFIED (as worded); see N1** | §14 bullets R0, "parent before attempt", receipt one-shot updates, the batch-lease exception and the harness. §7.2 steps 2–3. The §4.3 preamble. The `Reject` hold-reversal path takes no L2 (`withdrawal.go` :797), so the §14 W-KYC order `L1 → L3 → L4` is accurate. The per-item deposit T2 path, however, breaks R8 (N1). |
| **C10(a)** — T1p phase-A order, W-KYC listed | **SATISFIED** | The §5.2 flow order is (1)–(5). §4.3 has the W-KYC row. §4.7 has the `approved`/`rejected` rows. §5.2.1 row 1. |
| **C10(b)** — payout T2 re-claim re-gates; non-pass → `created` + escalation; M3 only | **SATISFIED** | §4.3 T2 and T16. §5.2 "Retryability". §5.2.1 row 2. §4.8 M3 (reason `kyc_denied`, never automated). §7.1. CP-W7. The gate runs in the per-item tx after the parent lock, not in the batch lease. Low note: see L2. |
| **C10(c)** — T12 payout re-gates | **SATISFIED** | §4.3 T12, §5.2.1 row 3, CP-W8. |
| **C10(d)** — no KYC-driven release after T1p | **SATISFIED** | §4.3 closing paragraph, INV-IO-7, §4.5, §5.2.1 row 4. |
| **C10(e)** — deposit T2 re-runs RG and KYC | **SATISFIED (substance); see N1 for the ordering** | §4.3 T2, §5.1 "Idempotency keys", §7.1. |
| **C10(f)** — re-check against ADR 0096's revision | **OPEN** | See the next section. The T2-reclaim → M3 route is still missing from ADR 0096. |
| **C11(a)** — every non-terminal intent backfilled | **SATISFIED** | §13.1 rows for intents with and without a reference, including the NULL-`provider_id` abort. |
| **C11(b)** — `id = parent id` | **SATISFIED** | §13.1. |
| **C11(c)** — `legacy_backfill`, T12 forbidden by trigger | **SATISFIED** | §4.2, §4.3 forbidden list, and §13.1 guard. |
| **C11(d)** — withdrawal mapping incl. `reversed` | **SATISFIED** | `submitted`→`pending` (abort on a NULL reference), `completed`→`succeeded`, `failed`→`declined` with kind `legacy` (stricter than my `sync`-marked-legacy; accepted), and `reversed`→`succeeded`. |
| **C11(e)** — `payment_method` sentinel | **SATISFIED** | `'legacy_unknown'` plus the CHECK tying it to `legacy_backfill`. |
| **C11(f)** — intent `failed`/`succeeded` rules | **SATISFIED** | `failed`→`declined`. `succeeded` aborts on a NULL `ledger_transaction_id` or a NULL reference. The ledger id is copied. |
| **C11(g)** — no ledger writes; post-checks | **SATISFIED** | The §13.1 post-checks run in the same tx. Convergence gaps in the backfilled rows are listed under N4. |
| **C12** — CP-D1 consistency | **SATISFIED** | The §5.1 flow (T1+T2 in one tx, no player-path `created`) and the §16.1 CP-D1 restatement agree. |
| **C13** — reconciliation ledger join | **SPECIFIED; gates `IMPLEMENTED` (PRH-I5)** | The §12.3 "Ledger join" table, §12.1 step 3 and §16.3. Low note: the payout side needs a named join path (L4). |
| **C14** — tests | **SPECIFIED; gates `IMPLEMENTED` (PRH-I1-i)** | Every bullet appears in §16.2 items 19 and 22, and MX16 exists. The fixes for N1–N4 and C6(d) each need a test (listed below). |

## LF95-C10(f) — ADR 0096 at origin

The file compared is `docs/decisions/0096-kyc-enforcement-boundary.md`:
- **At origin:** `origin/claude/focused-wright-jw88w9`, last changed in `d50327b`, 2041 lines.
- **At HEAD:** identical.
- **In the worktrees:** none of the `.claude/worktrees/*` has an uncommitted change to 0096. The
  `agent-a954225fe60ae5fb0` branch's last 0096 commit is an older snapshot (`3e6a478`).

The revision said to be in progress is therefore **not visible** in any ref or worktree I can read.
The findings below are against origin's text.

1. **The T2-reclaim → M3 route is still missing (IC condition 4 / LF95-C10(f): OPEN).**
   - What ADR 0096 does say (§5 "Coordination with ADR 0095" and §12.2 C5): a sweeper claim of a
     "prepared, not yet sent" request must re-run the gate, and `DenyForCompliance` is legal only
     from `approved`.
   - What it never says: what happens on a **non-pass** at that re-claim. There is no mention of
     `created`, T5/T2 or M3, and no statement that the hold may be released only by staff M3
     (`kyc_denied`), never by a second `DenyForCompliance` and never automatically.
   - Consequence: ADR 0095 §4.8/§5.2.1 carry this ADR's side, and the two ADRs do not yet agree
     in writing.
   - Owner: `identity-compliance`.
2. **The raw-guard test targets a function ADR 0095 removes.** ADR 0096 §8 item 3 requires a
   raw-guard test proving that `MarkSubmitted` is reachable only after a KYC evaluation in the same
   tx. ADR 0095 §4.7 splits `MarkSubmitted` into `ClaimForDispatch` and `RecordProviderReference`.
   The payout T2/T12 re-claims never pass through either one, because the withdrawal is already
   `submitted`. As written, the guard therefore cannot enforce C10(b)/(c). It must name:
   - `ClaimForDispatch` (T1p);
   - the payout T2 per-item claim;
   - the payout T12 per-item claim.
3. **Migration number collision.** ADR 0096 at origin still says "Allocated migration: **0101**"
   (line 7, §3.6 heading, §7, §8 item 6, §10, §13 C2). The orchestrator re-allocated 0100 to
   ADR 0096 and 0101 to ADR 0095 (ADR 0095 header and §13). ADR 0096 must be renumbered to 0100
   before either migration is written.
4. **The deny commit shape vs escalation (Low).** ADR 0096 §3.6 says a deny commits "decision +
   audit and **no** state-machine INSERT/UPDATE". ADR 0095's payout T2 non-pass also writes the
   T16-style escalation (`escalated_at`, `next_action_at`) on the attempt. That is not a state
   change, and it is correct. ADR 0096 should allow it explicitly for the re-claim case, so that
   its "exactly zero unintended state-machine transition" test does not reject it.

## New findings

**N1 (High; text fix; blocks PRH-I1): the deposit T2 per-item tx takes an L0 advisory after L1
row locks, which breaks ADR 0082 R8.**
- The ADR text: §7.1, §7.2 step 3 and §4.3 T2 say "lock parent, lock attempt, re-run gates
  (deposit: RG + KYC deposit gate)". §5.1 says the same for the player-resume path.
- The code: `rg.EvaluateEligibility` takes `lockPerson`, the L0.4 RG person advisory lock
  (`internal/rg/rg.go:607`).
- The rule: ADR 0082 R8 says advisory locks strictly precede row locks. Its only exception is
  E-1.
- So as written, the per-item tx takes L1 (`deposit_intents`, `payment_attempts`) and then L0.4.
  §14's per-path list does not cover this path at all.
- I found no current counterpart that holds L0.4 and then locks a `deposit_intents` row, so I
  cannot name a live deadlock today. It is still a violation of a binding rule in an accepted
  design, and the next RG-adjacent feature would make it one.
- **Fix:** in the deposit T2 per-item tx (both sweeper and player resume), run
  `rg.EvaluateEligibility` and the KYC deposit gate **first** (L0.4, then plain reads), then take
  the parent `FOR UPDATE`, then the attempt, then the CAS. This is still the same tx, so
  C10(e)/ADR 0096 C4(a) still hold.
- Add this path to §14's per-path list and to the A7 text I write into ADR 0082.
- The payout gate is unaffected: ADR 0096 `EvaluateEnforcement` takes no lock.
- Test: add the §16.2 item 16 harness case "sweeper T2 re-claim racing an RG self-exclusion write
  for the same person".

**N2 (Medium; text fix; blocks PRH-I1): payment-attempt INSERTs are not trigger-guarded, which
weakens INV-IO-4/INV-IO-7, and the `legacy` gate is not a boundary.**
- `payment_attempts_guard` is `BEFORE UPDATE` only. §4.3 allows exactly three INSERT shapes:
  - T1 (`created`);
  - T1+T2 (`submitting`);
  - T1p (`submitting`).
- The DB does not enforce that list. An application INSERT straight into `succeeded`, `declined`
  (a payout with any evidence kind) or `disputed`, or one with `legacy_backfill = true` or
  `last_evidence_kind = 'legacy'`, would pass. §13.1 says `legacy` is accepted "only in the
  migration's own session setting". A custom GUC can be set by any session, so that is
  discipline, not enforcement.
- **Fix:** add a `BEFORE INSERT` guard that requires:
  - `NEW.state IN ('created','submitting')`;
  - `NOT NEW.legacy_backfill`;
  - `NEW.last_evidence_kind <> 'legacy'`;
  - `NOT NEW.ever_possibly_sent`;
  - `ledger_transaction_id IS NULL`;
  - `provider_reference IS NULL`.
- In migration 0101, run the backfill **before** creating the INSERT guard, so no session-setting
  escape is needed at all.
- Also add `CHECK (last_evidence_kind <> 'legacy' OR legacy_backfill)`, and add `legacy` to the
  §4.2 enumeration.
- Mutation: removing the INSERT guard must fail a test that attempts each forbidden INSERT shape.

**N3 (Medium; text fix): T12 can re-send a live sibling after T13, which is an avoidable second
capture.**
- The sequence:
  1. Attempt 1 is declined, and cascade creates attempt 2.
  2. T2 claims attempt 2, and it becomes `ambiguous`.
  3. A late success on attempt 1 takes T13, and the intent becomes `succeeded`.
- T13 only rejects `created` siblings, and T12's CAS guard has no
  `NOT EXISTS(succeeded attempt for the same intent)`. The sweeper can therefore re-send attempt 2.
- If attempt 2's first send never reached the provider, the resend creates a brand-new capture
  for an intent that is already paid.
- The ledger stays correct: T7 posts it and P1 `multiple_success_for_intent` fires. The player is
  still double-charged, and the refund is BLOCKED on LEDGER-MANUAL-ADJ-4EYES-1.
- This differs from the §20 residual ("a sibling already `submitting`"), because a T12 is a
  platform-initiated new send that we can refuse.
- **Fix:** for `operation='deposit'`, add `NOT EXISTS(succeeded attempt for the same intent)` to
  the T12 CAS predicate (as T2 already has). The attempt stays `ambiguous` and is resolved by
  poll or callback only.
- Test: add to §16.2 item 19.

**N4 (Medium; text fix; blocks the 0101 backfill): backfilled rows lack columns that decide
convergence.** The §13.1 backfill table leaves these unspecified:
- **`next_action_at`.** Backfilled `pending`/`ambiguous` deposits and `pending` payouts get NULL,
  so `idx_payment_attempts_due` never returns them and **the sweeper never polls them**. They
  converge only if a callback happens to arrive. The C11(g) "exactly one live attempt" post-check
  passes anyway. Set `next_action_at = now()` for every non-terminal backfilled row.
- **`first_submitted_at` / `last_sent_at`.**
  - With NULL `first_submitted_at`, the deferred-receipt rule `received_at >= first_submitted_at`
    evaluates to NULL, so a legacy success receipt is never applied. It is resolved only by poll.
  - With NULL `last_sent_at`, a deposit `not_found` can never become authoritative (§4.5(b)).
    That is the safe direction, but it only escalates.
  - Set `first_submitted_at` = the parent's `created_at`. It is the earliest instant any genuine
    callback could describe, so all genuine receipts qualify.
  - Set `last_sent_at` = the migration's `now()`. It is the latest plausible send, so not-found
    authority is delayed by a full Δ and never premature.
- **`interactive`** (`NOT NULL`). Set it to `true` for backfilled deposits, matching the §10.1
  "unknown means true" default. An asynchronous decline of a legacy intent then finalizes it and
  never cascades unattended to a new provider. `excluded_provider_ids` is unknowable for legacy
  cascades, which is one more reason not to cascade them.
- **`ever_possibly_sent` for payouts.** The table states it only for intents. Every backfilled
  `submitted`/`completed`/`failed`/`reversed` withdrawal attempt must be `true`.
- **`amount`/`asset_code` source.** Name it explicitly: the parent's amount and asset, never
  re-derived.
- Test: extend §16.2 item 22 with "a backfilled `ambiguous` intent is picked up by the next sweep
  and converges by `QueryStatus`", and "a non-terminal intent without a reference is backfilled as
  `ambiguous`" as its own named fixture.

**Low**

- **L1.** §4.2 lists six `last_evidence_kind` values, and §13.1 allows seven (`legacy`). Align
  them; this is part of N2.
- **L2.** Payout T2 gate non-pass: state the retry cadence (`next_action_at` backoff, for example
  the escalated cadence) so the sweeper does not re-gate every lease period. Also state explicitly
  that a later **pass** resumes dispatch through T2. That is acceptable, because `created` was
  never sent, and it is mutually exclusive with M3 by the withdrawal lock.
- **L3.** A KYC deny at a T2 re-claim ends the withdrawal as `failed` (M3 → `withdrawal.Fail`,
  `withdrawal_failed`, key `request_id:failed`). A deny at T1p ends it as `rejected`
  (`withdrawal_rejected`, `<id>:kyc_denied`). Both are correct hold reversals. Compliance
  reporting must still count both. ADR 0096's `withdrawal.rejected_kyc` audit does not cover the
  M3 path, so the M3 audit must carry `kyc_denied`, which §4.8 already requires.
- **L4.** C13 payout join: `payment_attempts.ledger_transaction_id` is documented as the deposit
  posting only. Name the payout join path in §12.3, via `withdrawal_requests.release_ledger_transaction_id`
  (migration 0026) with `transaction_type = 'withdrawal_completed'`, or link Step B on the
  attempt as well.

## Open ledger-finance actions (mine, not ADR defects)

- Amendment A7 is **not yet written into ADR 0082**; `grep A7` returns nothing. The ADR header
  says `ledger-finance` writes it now that the ADR is ACCEPTED. I will include the N1 ordering
  rule, which puts the deposit T2 per-item tx at RG L0.4 before L1, when I write it.
- The `reconciliation-model.md` §2.2(b) amendment (§12.6/§21.4) is **not yet applied**; the file
  does not mention 0095.

## Required before PRH-I1 starts

1. Fix C6(d) (§4.3 T13 vs T10 vs §4.4), with a test.
2. Fix N1, N2, N3 and N4 in the ADR text, with the tests listed above.
3. `identity-compliance` lands ADR 0096's T2-reclaim → M3 route, the raw-guard retarget and the
   renumbering to 0100. `ledger-finance` then re-checks C10(f).

C13/C14 remain gates on `IMPLEMENTED`. Everything in ADR 0095 remains `NOT IMPLEMENTED`.
