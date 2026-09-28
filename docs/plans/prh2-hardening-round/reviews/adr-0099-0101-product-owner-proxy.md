# Product-owner-proxy review — ADRs 0099–0101 (2026-09-28)

Read-only. Recorded by the orchestrator.

| ADR | Verdict |
|---|---|
| 0099 capability grants (K1) | **ACCEPT** |
| 0100 governed manual adjustments (K2) | **ACCEPT WITH CONDITIONS** (minor; see below) |
| 0101 force-resolve M1/M2 (K3) | **ACCEPT** |

## Q1: the closed four-capability enum is CONFIRMED

This satisfies security's pre-K1 condition.
- `ledger_adjustment:{initiate,approve}` and `payment_force_resolve:{request,approve}` map one-to-one onto the two decided capabilities.
- Extensibility is via migration plus ADR, and nothing speculative is added.
- The governance permissions (`capability_grant:*`, `financial_policy:*`) stay static RBAC. There is a sound bootstrap argument for this (§3.2).

## Q2: overbuilding

| Item | Verdict |
|---|---|
| "Platform acting in tenant X" session | Not overbuilt. HD-PRH2-6 requires it, and it is the minimal-exposure shape: deny-by-default with a closed table list. |
| Five policy levels | Not overbuilt. Each is named in HD-PRH2-3/-7, and they share one table with a level discriminator. |
| Re-attestation table and view (0099 §8.3/§10.5) | A minor simplification candidate, **not blocking**. Ask architect and security whether a `grant.reattested` audit action satisfies S-11 as cheaply. Keep the table if "attested" must gate a SQL view predicate. |
| `pending_suspense_allocation_b` finding code (0101 §3) | **Defer.** Do not seed it in 0114; add it with LEDGER-SUSPENSE-B-1 when that is authorized. |
| The four-eyes framework applied to M1 | Not overbuilt. It reuses M2's framework, and M1 can mask an integrity finding if left to one person. |

## Q3: HD-PRH2-8 is genuinely a human decision

CLAUDE.md's "above a configurable threshold" and HD-PRH2-1's "mandatory for the class" really are in tension, and the choice is a risk-appetite question. The stricter interim reading is enforced in the database (trigger: `base_required_approvals >= 1`). Route it to the human promptly, so the interim does not become permanent by default.

**Orchestrator dispositions:**
- The `pending_suspense_allocation_b` deferral is **adopted**: K3 must not seed it (recorded in plan §11).
- The re-attestation design question goes to security in its review of 0099.
