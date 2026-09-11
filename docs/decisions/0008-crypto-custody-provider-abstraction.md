# ADR 0008 — Crypto Custody via Institutional Provider Abstraction

Status: Accepted (human decision, resolves Q4 from ADR 0005)

## Context

Stage 0 flagged self-custody vs. custodian as an open, insurance/legal-
weighted decision, with the Blueprint's own read favoring a custodian at
year one given team size.

## Decision

No self-custody or private-key management inside the core platform. The
platform integrates an institutional/professional custody provider
(Fireblocks/BitGo-shaped) behind a `CryptoCustodyProvider` interface:

- **Platform owns**: player crypto wallet/account representation, ledger,
  balances, transaction state, deposit/withdrawal orchestration,
  reconciliation against custodian statements, audit, and custodian
  provider references.
- **Custodian owns**: private-key custody, blockchain signing, secure key
  management, custody infrastructure.

The interface must allow adding or replacing a custodian without a core
rewrite — no permanent single-custodian assumption.

## Consequences

- Crypto rail implementation (Stage 4) is contracted, not built —
  engineering effort goes into the adapter and reconciliation, not key
  management infrastructure.
- This decision does not by itself satisfy every jurisdiction's regulatory
  requirements for holding customer crypto assets; that determination is
  legal/compliance work, tracked separately and never claimed as solved by
  this architecture alone.
- Selecting the actual custodian vendor remains a commercial/legal
  decision for the human, made before Stage 4 crypto-rail work begins in
  earnest.

## Owner

`payments` for the adapter/interface; the custodian vendor selection
itself is a human decision, not delegated to any specialist.
