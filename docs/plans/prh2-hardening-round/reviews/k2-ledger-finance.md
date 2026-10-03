# Ledger-finance review (independent) — PRH-2 K2 (2026-09-28)

**Reviewer:** `ledger-finance`, independent of the implementer. The orchestrator recorded this review.

**Scope:** `9c6c705..18c357a`. Private DBs, dropped afterwards.

## Verdict: ACCEPT WITH CONDITIONS

**Acting-session money probe with a valid K2 grant:**

| Non-governed write | Result |
|---|---|
| Ungoverned `manual_adjustment` | **CG030** (fence) |
| Forged-key `manual_adjustment` | **CG030** |
| `casino_bet` | **CG030** |
| Entry appended to an existing transaction | **CG030**; N-2 closed |
| `ledger_accounts` of type `psp_clearing` | 42501 |
| Projection UPDATE or non-zero INSERT | **CG031** |
| Projection DELETE | 0 rows |
| UPDATE of an existing transaction | P0001 |

The tenant read-back afterwards is unchanged. The only acting money write possible is the governed shape, inside the executor.

**Money-path mutants:** all **KILLED**.
- MK1: the sufficiency check disabled.
- MK2: the compensation cap disabled, including under concurrency.
- MK3: the projection fence UPDATE branch disabled.

**Rulings met:**
- LF-9: the closed shape, verified by `verify_link`.
- LF-10: payload-bound approvals with `decided_txid`/`executed_txid`.
- LF-11: live-Person independence.
- LF-12.
- LF-13: no negative balance, and no approved-but-unexecuted window (MA041 catches a crash).
- LF-14: the keys, and ruling 1 applied literally.
- K2-a: tombstone-only exposure.
- K2-b: the 0045 tenant layer, fail-closed.
- MA020.
- C-K1-2: the fences sit in the same migration, before the acting policies.
- C-K1-3: `FOR SHARE` from both families, and in-force at `now()`.
- N-2.
- The A8 lock order.
- The invariants hold throughout.

**The detector placement inside `RunLedgerVsProjection`: ACCEPTED.** It is unwindowed, per-tenant and hourly, and it reads executed requests only.

## Conditions

| ID | Sev | Condition | When |
|---|---|---|---|
| C-K2-1 | Low | A `ledger_unlinked_manual_adjustment` finding must not be reported as "projection drift". I-wire carries a mismatch-kind attribute or a distinct discriminator for ADR 0102 §8 row 3, and the runbook calls it a **governance breach**. | Binding on I-wire; the docs now |
| C-K2-2 | Medium (not K2; G1 fallout) | Two `internal/reconciliation` legacy-shape tests fail because pre-0109 scratch schemas meet the `subject_tenant_id` write. | **Fixed on main by the orchestrator** in `9fcd08e` (`audit.Record` names the column only when it is set) |

**Carried forward:** N-1, LEDGER-MANUAL-ADJ-LINK-1 (launch-blocking) and HD-PRH2-8.
