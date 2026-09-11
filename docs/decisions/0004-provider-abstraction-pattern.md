# ADR 0004 — Every External Integration Is a Subsystem, Not a Connector

Status: Accepted (derived from Blueprint §1, §3, §4.3)

## Context

Most of what a player touches (games, odds, KYC verification, PSP rails)
is licensed from vendors. It would be tempting to treat each integration
as "just call their API." The Blueprint explicitly rejects this framing:
several components that look like integrations carry a large platform-side
subsystem the vendor does not supply, and the vendor's API is the smallest
part of the work.

## Decision

For every external provider (game aggregator, sportsbook provider, PSP,
KYC/AML vendor, affiliate platform), the platform builds, once per
integration, on top of the vendor's API:

1. An adapter mapping the vendor's model onto our internal interface.
2. Idempotency, retry, and timeout semantics.
3. Per-tenant credentials, limits, and toggles.
4. A state machine (pending → settled → reversed).
5. Daily reconciliation against the ledger.

Domain specialists (`casino`, `sportsbook`, `payments`, `identity-
compliance`) own the vendor-specific adapter logic; `integrations` owns
the shared scaffolding (retry policy, idempotency helpers, state-machine
base, credential storage pattern) that every adapter builds on, so it
isn't reinvented per integration.

## Consequences

- Integration effort is measured in weeks, not days, and integrations
  never finish — provider APIs change, currencies get added, sandboxes
  drift from production. Budget roughly one engineer per 15–20 live
  integrations for maintenance alone (Blueprint §1).
- The internal provider interface (not the vendor's API shape) is what the
  rest of the platform codes against — swapping or adding a vendor behind
  an existing interface should not require changes outside the adapter
  layer.

## Owner

`integrations` for the shared pattern; `architect` for the interface
contracts each domain specialist's adapters implement.
