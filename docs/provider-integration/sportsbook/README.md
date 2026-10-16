# Sportsbook provider-integration package

Status: **DOCUMENT / SPECIFICATION PACKAGE ONLY.** No adapter, no vendor call, no credential, no account, no agreement.
Prepared 2026-10-16 by `integrations` on branch `provider-packages-rest` (base: `claude/focused-wright-jw88w9` at `4a3c85f`).
Provider-integration phase item 4 of 5 (PSP -> casino -> KYC/AML -> **sportsbook** -> sports data; see `../README.md`).
Same structure and rigour as `../psp/`.

> **DEFERRED.** Owner decision 2 of 2026-10-15: casino is the first-launch priority and **sportsbook is deferred until a
> sportsbook brand is scheduled.** This package exists so the work is ready when that decision is made. It authorises and
> schedules nothing, and no sportsbook vendor should be contacted before a sportsbook brand is scheduled.

Labels (CLAUDE.md): everything in this directory is `NOT IMPLEMENTED` as software. No sportsbook provider is selected and
no vendor documentation is available; `docs/integrations/dummy-sportsbook.md` records that the earlier "Dummy
Sportsbook" contract was never available. **This package asserts no vendor behaviour.** Vendor-side statements are
requirements the platform places on a vendor or are marked `UNVERIFIED (needs provider documentation/sandbox)` with an
id `U-SB-nn`. The platform side is verified against code at base `4a3c85f`, with paths cited.

The platform does **not** own odds or trading (CLAUDE.md "does not own ... odds/trading"). The code today nevertheless
accepts a bet in-house at the catalogue's current odds and derives the payout from the odds frozen at acceptance
(`internal/sportsbook/orchestrator.go`, ADR 0088 s2.4). That is a platform-side accounting model, not a pricing or
trading capability; whether it can coexist with a real vendor's bet and settlement model is the central open question of
this package (`SB-G01`).

## Index

| File | Content |
|---|---|
| `provider-neutral-mapping.md` | Platform concept -> what the sportsbook provider must supply -> how the neutral interface carries it -> gap/UNVERIFIED. Platform-gap candidates `SB-G01..` (section 10), UNVERIFIED registry `U-SB-nn` (section 11). |
| `conformance-plan.md` | Existing sportsbook suites a real adapter must run unchanged, what a provider-driven settlement path needs, new cases `SB-C-nn`. |
| `integration-gates.md` | Gates S0..S9 (documentation review -> adapter -> conformance -> security -> ledger-finance -> sandbox -> operational acceptance -> pre-production -> explicit production authorization), evidence and exact owner/vendor inputs. |
| `open-questions.md` | The minimum decisions/inputs `OQ-SB-nn`, each with a recommendation. |

## What the platform already owns (verified)

- Fixed-odds **singles** bet acceptance: one synchronous in-house `PlaceBet` with the full gate chain (market, tenant,
  brand, jurisdiction restriction, RG, Risk, KYC with AML hold, cross-player exposure), stake moved from `player_cash` to
  `player_locked_cash` (the open-bet liability in the ledger), potential return frozen at acceptance.
- The settlement lifecycle for cash-funded singles in **in-house mock mode**: settle won/lost, void before/after
  settlement, rollback (seen/never-seen with tombstone), re-settlement by generation, composed rollback-then-void,
  idempotency and payload integrity (ADR 0088), driven today only by the non-production test-support route.
- The `sportsbook_settlement` reconciliation stream (four checks) with a MOCK statement source.
- A catalogue sync (sport / competition / event / market / selection) fetched outside any transaction, run once at startup.
- Everything else (webhooks, provider references, cashout, partial settlement, accumulators, live odds, bonus-funded bets,
  liability reporting, sports-data ingestion) is `NOT IMPLEMENTED` and listed as non-goals in ADR 0087/0088.

## Headline gaps (detail in `provider-neutral-mapping.md` section 10)

1. `SB-G01` The `sportsbook.Provider` interface has `Catalogue` only. There is no provider bet-placement, bet-status,
   cancel or cashout method, and the doc 09 `SportsbookProvider` is a conceptual sketch. A vendor that accepts the bet
   itself (wallet-integration style) or carries the risk is unrepresentable, and the choice of operating model must be made
   before any adapter.
2. `SB-G02`/`SB-G03` Settlement is keyed by platform bet id and a caller-supplied generation; the payout is always
   derived from the frozen odds and a vendor payout is only a claim that must equal it. Dead-heat, rule deductions, boosts
   and cashout payouts cannot post; provider settlement references are not stored or deduplicated.
3. `SB-G04` No inbound sportsbook webhook route, scheme or admission domain exists.
4. `SB-G07` The sportsbook statement line is per platform bet id with no stake/odds/commission; a single global source,
   no registry, no read-only attestation, no real mismatch kind.
5. `SB-G08` Open-liability reporting ("sportsbook GGR is only known at settlement") is a design requirement with no
   report in code; exposure limits ship unarmed.

## Principles applied

- Providers adapt to the neutral interfaces; the core is not redesigned around the first sportsbook vendor. Gaps are
  numbered with scope, reviewer and a minimal proposal. No core change is made by this package.
- Money-moving paths are platform-owned and idempotent: only `settleBet`, `voidBet` and `rollbackBet` post, under the bet's
  row lock (ADR 0088, ADR 0082 lock order). A feed or provider message never posts money (SBF-3).
- A settlement-derived payout is never taken from the provider (anti-minting, DB-enforced trigger T-1).
- Sandbox only until explicit production authorization; synthetic tenant; no real money.
