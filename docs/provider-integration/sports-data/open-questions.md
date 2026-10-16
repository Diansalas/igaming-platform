# Sports-data package: minimum decisions and inputs

Each item: the question, why it is needed, and `sportsbook`'s recommendation. The owner decides. **All items are parked
until a sportsbook brand is scheduled** (owner decision 2, 2026-10-15) and the sportsbook operating model is decided
(`../sportsbook/open-questions.md` OQ-SB-01). Nothing here is decided by this package.

| Id | Needed by | Question | Recommendation |
|---|---|---|---|
| OQ-SD-01 | D0, D1 | Which sports-data provider(s) are to be evaluated, and is one provider expected to supply fixtures, prices and results, or are prices (odds) separate from results? | If the sportsbook model is "vendor books" (OQ-SB-01 b), the vendor supplies prices and results and this package shrinks to settlement-result verification. If "platform books" (a), evaluate two data providers for fixtures/results with a documented ordering primitive, and treat odds as a separate trading decision. Owner, not an agent, handles vendor contact |
| OQ-SD-02 | D0 | First-feed scope: pre-match only, or also live/in-play? | Pre-match only. Live needs the staleness, watchdog, C0 lock and price-change rules (SD-G05) and a live-capable provider; defer |
| OQ-SD-03 | D0 | Second source: one feed only, or a second source for cross-checking results (and fixtures)? | One feed plus staff manual results and the mandatory hold for the first brand; a second source only for high-value markets if risk asks (SD-G07) |
| OQ-SD-04 | D5, D8 | Result-hold thresholds: the platform and per-tenant per-asset payout thresholds above which a result needs four-eyes confirmation, and whether any market type is always held | `risk` + `ledger-finance` propose from the first brand's expected stakes; the owner approves values (none invented here). Until configured, feed-driven settlement stays disabled for that tenant (fail closed). In the MOCK phase 0 = off |
| OQ-SD-05 | D0 | Entity creation policy: pre-provision fixtures/markets through a controlled workflow ("unmapped references are recorded, never created"), or auto-create from the feed? | Pre-provision/approve (recorded, never created) for money-bearing entities; auto-creation of fixtures only if the provider is trusted and the creation is audited. Never auto-create a market that can take bets without review at first |
| OQ-SD-06 | D5 | Correction window: how long after settlement may a corrected result roll back a payout, and what happens when the winner has spent it? | A defined window set by compliance/risk; a correction outside it is a manual case (alert), not an automatic rollback. `ledger-finance` decides the negative-balance rule, as for casino LF-10 |
| OQ-SD-07 | D0 | Credential and admission model for the feed (X-G01 and SD-G03): platform-scope credential in the secret store and a `sportsbook_feed` admission domain (ADR 0117), versus extending `providercred` | Platform-scope, per ADR 0117: a feed is market-wide, not per-tenant. `architect` + `security` |
| OQ-SD-08 | D1 | Data rights and licensing (display, redistribution, official-data requirements, use in fixtures) | Owner and legal inputs per provider; engineering does not interpret licences. Keep recorded fixtures minimal and synthetic |
| OQ-SD-09 | D6 | A single platform-scope sandbox account, or one per environment? | One per environment (staging, production), each with its own credential and cursor; never share between environments |

## Inputs (not decisions) the owner must eventually supply

Listed in `integration-gates.md` ("Exact inputs needed"): the sportsbook scheduling decision, vendor documentation, the
data licence, sandbox terms and credentials via the secret store, hold-threshold values, written vendor answers to the open
documentation items, and later (D9 only) the signed contract and production credentials. None is requested now.
