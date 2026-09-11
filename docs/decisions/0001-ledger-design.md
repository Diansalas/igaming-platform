# ADR 0001 — Append-Only Double-Entry Ledger as Sole Source of Financial Truth

Status: Accepted (derived directly from Blueprint §4.2 — not discretionary)

## Context

The platform must handle real money across casino, sportsbook, bonus, and
payment flows, under high callback volume with aggressive provider retry
behavior. Financial correctness must survive network failures, duplicate
callbacks, and concurrent access without ever double-crediting, double-
debiting, or losing history.

## Decision

- The ledger is append-only and double-entry. Balances are always a
  projection recomputed from ledger entries, never an authoritative
  mutable field.
- Money is represented as integers in minor units with a per-currency
  exponent; `NUMERIC(38,0)` plus a per-asset exponent (8 or 18) if crypto
  is in scope. Floating point is never used for money.
- Idempotency is enforced by a unique database constraint on
  `(provider_id, provider_tx_id)`, not application-level check-then-insert.
- Corrections are compensating entries. Rollbacks of never-seen
  transactions write tombstones.
- Redis or any cache is never authoritative for balances and is never read
  on the bet/settlement path.
- Reconciliation (recomputed ledger vs. projection) runs on a schedule
  (target: hourly); non-zero drift is a P1 incident.

## Consequences

- Every service that moves money must go through the wallet service's API
  — no service writes ledger tables directly except `wallet` itself.
- This is the first system built (Stage 3) because nothing downstream
  (payments, bonus, casino, sportsbook) can be trusted without it.
- This decision is not revisited casually — Blueprint explicitly frames it
  as "the part you can never retrofit."

## Owner

`ledger-finance` (see `.claude/agents/ledger-finance.md`).
