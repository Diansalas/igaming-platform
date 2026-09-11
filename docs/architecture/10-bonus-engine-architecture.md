# 10 — Bonus Engine Architecture Proposal

Status: Stage 0 proposal. Source: Blueprint §4.5. This is the component
partner operators evaluate hardest and the one most often built as an
afterthought — treated here as core infrastructure, not a later add-on.

## Architecture

Event-driven rule engine subscribed to the platform event bus:
`player.registered`, `deposit.settled`, `round.settled`, `bet.settled`,
`session.started`.

## Data model (four layers)

**Campaign** (marketing container) → **Offer** (the rules) → **Grant**
(one player's instance) → **Progress** (append-only trail of every
wagering change). The Progress trail is what gets shown to a player who
disputes a forfeited bonus — and there will be such players every week.

## Configuration axes (must be operator-editable without an engineer)

| Axis | Controls |
|---|---|
| Eligibility | Segment, country, currency, deposit method, min/max deposit, first-deposit-only, VIP tier, opt-in |
| Reward | Match % and cap, fixed amount, free-spins package (game, count, bet level), free bets, cashback % and window |
| Wagering | Multiplier, contribution % by game/category/provider, max bet while wagering, excluded games, time limit |
| Payout | Max cashout, cash-first/bonus-first bet ordering, forfeiture rules, partial release thresholds |
| Abuse controls | Low-risk betting detection, device/payment fingerprint linking, velocity caps, manual review queue |

## Ledger relationship

Bonus balances and liabilities must remain financially auditable — bonus
funds are split at the **ledger level** (`player_cash` vs. `player_bonus`,
see `06-wallet-ledger-architecture.md`), never tracked only in a bonus-
engine side table disconnected from the ledger. `bonus-engine` supplies
the split instructions; `ledger-finance` owns the posting mechanism.

## Free rounds

Every provider exposes free spins differently. The bonus engine talks to a
normalised internal interface; `casino` implements the per-provider
adaptation behind it (see `08-casino-integration-architecture.md`).

## Ownership and stage mapping

Owned by `bonus-engine`, ledger interactions reviewed by `ledger-finance`.
Stage 4, alongside KYC/AML/RG — the Blueprint groups these because bonus
abuse and compliance controls are closely related in practice.
