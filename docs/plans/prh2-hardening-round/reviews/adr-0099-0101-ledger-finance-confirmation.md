# Ledger-finance confirmation — ADR 0099–0101 rev 2 (2026-09-28)

Reviewer: `ledger-finance`. The orchestrator recorded this review. Scope: HEAD `8635251`. ADR 0099, 0100 and 0101 rev 2 were read in full
and checked against [`adr-0099-0101-ledger-finance.md`](adr-0099-0101-ledger-finance.md), against security's
confirmation §3/C-4, and against the code wherever the ADRs make claims. The review was read-only.

**Verdict: CONFIRMED WITH CONDITIONS.**

**Ruling 1 vs 5(c): ADOPT with C-4, amended.** The compensation does **not** clear
`pay_declared_paid_unconfirmed`.

- K1 is unblocked financially.
- K2 needs K2-a and K2-b.
- K3 needs K3-a.

The orchestrator wrote all three into ADR 0100 §5.2, §5.4, §13 and §16, and ADR 0101 §8.4, §9, §12 and §15.

## 1. Earlier findings

| Item | Status | Where |
|---|---|---|
| F1 projection fence | Addressed | 0099 §6.7; A-4b |
| F2 exact fence predicates | Addressed | 0099 §6.6. The keys match `withdrawal.go:1442` and `:1539`. (d) is the all-sessions prefix guard with `left()`. |
| F3 `FOR SHARE` | Addressed | 0099 §7.4; 0100 §6.2 and §6.5; B-7 |
| F4 / ruling 2 | Addressed, with one fix (K2-a) | 0100 §5.2; MA020; INV-ADJ-5; B-23 |
| F5 / ruling 1 | Addressed | 0100 §5.4; B-13; INV-ADJ-6 |
| F6 | Addressed | 0100 §5.1 |
| F7 / ruling 7 | Addressed | 0100 §3.2 and §3.4; B-20 |
| F8 / ruling 4 | Addressed | 0100 §12, §10.8; B-25 |
| F9 | Addressed | 0101 §5.1; MR010; C-5 |
| F10 / ruling 5(c)/(d) | Addressed; 5(c) amended (§2) | 0101 §9. (d) "debits total at least the withdrawn amount" is confirmed: a partial recovery must not clear it. |
| F11 | Addressed | 0101 §5.3; WITHDRAWAL-REVERSAL-1 |
| F12 | Addressed | 0101 §8.3; C-17; C-17b |
| F13 | Addressed | 0101 §4; C-19 |
| F14 | Addressed | `ValidatePaymentReference` covers payments only |
| F15 / ruling 6 | Addressed; the L2 addition is confirmed | 0100 §8 |
| F16 | Addressed | `pending_suspense_allocation_b` not seeded |
| Ruling 3 | Partially addressed (K2-b) | 0100 §5.2 |

**Pins:**

- **Exposure key column (K2-a).**
  - The tombstone writer keys on the original reference (`orchestrator.go:1526-1528`).
  - A `deposit_reversal` is keyed on its own reference (`receipt.go:1302-1304`) and requires a posted original, which a `multiple_success_for_intent` attempt never has. So that arm never matches and is conceptually wrong. It is removed.
  - The pinned predicate is `t.transaction_type = 'tombstone' AND t.provider_id = a.provider_id AND t.provider_tx_id = a.provider_reference`, which uses `idx_ledger_transactions_tenant_provider_tx`.
  - A NULL reference keeps the exposure true (fail-closed).
  - A refund visible only on a statement does not clear it (fail-closed, accepted).
- **Suspended asset (K2-b).**
  - `assets.active = false OR assets.platform_authorized = false` is correct for the platform layers (`0044:96`, `0044:434`).
  - Ruling 3 also covers the 0045 tenant authorization layer (`asset_authorizations`, `0045:155`) when the authorization is absent or not in force.
  - K2 pins the exact column predicate.
- **A8 L2 addition: CONFIRMED.**
  - The lock order is request (L1) → staff → grants (L1, `FOR SHARE`) → the causation `ledger_transactions` row `FOR UPDATE` (L2) → L3 → L4.
  - Other L2 takers take their own L1 rows first and never take adjustment requests, so no cycle exists.
  - The acting UPDATE USING with WITH CHECK `false` is the minimal grant.

## 2. Ruling 1 vs 5(c): ADOPT with C-4, 5(c) amended

**Why adopt.** A literal ruling 1 does not prevent a player-restoring credit, because
`operational_error_correction` and `external_instruction` allow uncausated credits. Admitting the
reserved-prefix Step B as causation is tighter: the credit becomes capped, linked, serialised under L2 and
evidenced.

**Security C-4 (a)–(e) are accepted, with these ledger notes:**
- **(a)** The causation must also be the `ledger_transaction_id` of an executed `m2_declare_paid`, and its `player_withdrawal_hold` leg must be on this wallet in this asset.
- **(d)** The cap is the hold-leg amount (= `wr.Amount`), cumulative over executed credits, under L2.

**Amendment: the compensation does not clear `pay_declared_paid_unconfirmed`.**
- Step B's credit to `psp_clearing` still stands, and the compensation books the cost to `manual_adjustment`. The books remain wrong until WITHDRAWAL-REVERSAL-1 exists.
- So the finding stays standing and unwindowed. The compensation is shown only as a read-time annotation.
- It clears only on (i) a confirming `succeeded` line, or (ii) a future WITHDRAWAL-REVERSAL-1 posting.
- C-22 becomes "compensation annotates but does not clear".

**C-4(b) is confirmed as ledger-correct.**
- Consider a compensated M2 that is later confirmed paid. The player's over-benefit is the compensating-credit total.
- A new standing, unwindowed P1 kind, `pay_declared_paid_compensated_but_paid`, covers it. It clears only when compensating debits whose causation is the credit's own `manual_adjustment` transaction total at least the credited amount.
- A partial recovery, or a recovery refused for insufficient funds, leaves it standing.

**Required K3 tests** (written into ADR 0101 §12 as C-22 to C-30):
1. The C-4(a) mutant.
2. A causation pointing at another wallet's Step B is refused.
3. A debit with this causation is refused.
4. Concurrent credits respect the cap.
5. A missing evidence hash is refused.
6. C-4(c) Person separation, at insert and at execution.
7. Compensation leaves the finding standing, annotated, across N runs.
8. Compensation followed by a confirming line: the finding clears, the new P1 is raised, a partial recovery does not clear it and a full recovery does.
9. SUM(D) = SUM(C), and projection = recomputed, throughout.

INV-ADJ-5 is unchanged.

## 3. Conditions

| ID | Blocks | Condition |
|---|---|---|
| K2-a | K2 (0112) | Tombstone-only exposure key. Remove the `deposit_reversal` arm. B-23 adds a refund-tombstone case. |
| K2-b | K2 | The suspended-asset definition includes the 0045 tenant layer. B-24 adds a tenant-disabled-asset case. |
| K3-a | K3 (0114) | Apply §2: the causation arm, the annotation instead of clearing, the new kind in 0114's kind CHECK and in `reconciliation-model.md`, and tests 1–9. 0100 §5.4 and §16.2 are marked resolved. |
| — | informational | 0100 §10.9: once any `ledger_unlinked_manual_adjustment` row exists, 0112's down refuses, so 0112 is effectively irreversible from then on. This is accepted. |

Nothing else blocks K1, K2 or K3 financially. The launch flags are unchanged: HD-PRH2-8, LEDGER-MANUAL-ADJ-LINK-1, TM-7 and TM-10.

**Not verified:**
- the exact 0045 column names;
- the 0099 §6.2 RLS enumeration (security's scope);
- whether any check compares `psp_clearing` against the PSP (§2 assumes none does).
