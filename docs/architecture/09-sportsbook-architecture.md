# 09 — Sportsbook Architecture Proposal

Status: Stage 0 proposal. Source: Blueprint §4.4.

## Build/buy line

Odds, trading, and risk management are never built in-house. Two
integration shapes exist:

- **Widget/iframe** (Altenar, BetBy, Digitain) — provider renders the
  whole betting experience; platform supplies authentication and a
  seamless wallet. Weeks of work. **RECOMMENDATION: start here**, per
  Blueprint's explicit guidance.
- **Feed and API** — platform receives odds and places bets
  programmatically, builds its own UI. Months of work, more presentation
  control, still no ownership of risk. Deferred unless a specific
  commercial reason emerges.

Start on the same wallet as casino so the player sees one balance.

## What is genuinely platform-owned

An open sportsbook bet is a liability that can span days or months, unlike
a casino round that settles in milliseconds. Model:

- Stake moves to `player_locked` at placement (ledger account, owned by
  `ledger-finance`).
- An open-bet record carries potential return.
- Settlement, void, partial settlement (bet-builder legs), and cashout are
  each **distinct ledger events** — never conflated into a single
  "resolve bet" operation.
- Markets can be corrected after initial settlement, requiring re-
  settlement as its own event, not a silent balance edit.

## Reporting consequence

Sportsbook GGR is only known at settlement. A daily revenue report that
ignores open liability overstates today and understates the day a big
accumulator settles. Every sportsbook report needs an explicit
open-liability line (owned jointly with `data-analytics`), or partners
will not trust the numbers — and would be right not to.

## Ownership and stage mapping

Owned by `sportsbook`, with `ledger-finance` review on all ledger-event
postings. Stage 5 in the build sequence — deliberately after wallet/ledger
(Stage 3) and compliance/bonus (Stage 4), since it depends on both a
working shared wallet and RG controls already being in place.
