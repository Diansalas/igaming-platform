# Sports-data provider-integration package

Status: **DOCUMENT / SPECIFICATION PACKAGE ONLY.** No adapter, no feed connection, no credential, no account, no agreement.
Prepared 2026-10-16 by `integrations` on branch `provider-packages-rest` (base: `claude/focused-wright-jw88w9` at `4a3c85f`).
Provider-integration phase item 5 of 5 (PSP -> casino -> KYC/AML -> sportsbook -> **sports data**; see `../README.md`).
Same structure and rigour as `../psp/`. Sports data is last in the order and depends on the sportsbook decision
(`../sportsbook/`): it is **DEFERRED with sportsbook** until a sportsbook brand is scheduled (owner decision 2, 2026-10-15).

Labels (CLAUDE.md): everything in this directory is `NOT IMPLEMENTED` as software. No data provider is selected and no
vendor documentation is available. **This package asserts no vendor behaviour.** Vendor-side statements are requirements
the platform places on a vendor or are marked `UNVERIFIED (needs provider documentation/sandbox)` with an id `U-SD-nn`.
The platform side is verified against code and migrations at base `4a3c85f`.

## The central finding: there is no ingestion seam

Verified by search of all Go and SQL at base `4a3c85f`: there is **no** sports-data ingestion seam. No `FeedProvider`,
`DataFeedProvider` or `datafeed` package, no `sb_feed_*`, `sb_market_results`, `sb_price_history`, `sb_state_changes`
table, no `odds_version` or `feed_version` column. What exists:

| Piece | State |
|---|---|
| `sportsbook.Provider.Catalogue(ctx)` -> `CatalogueResult` (sport > competition > event > market > selection with integer odds and statuses) | `IMPLEMENTED` (MOCK provider); `FetchCatalogue` outside any tx, `SyncCatalogue` under the `sportsbook_catalogue_sync` platform-service scope; **called once at startup** (`cmd/platform-api/main.go:361-391`) |
| Result/settlement input | only the non-production test-support route `simulate-settlement-event` (ADR 0088); no result entity at all |
| Market/event state changes | catalogue re-sync only (last writer wins, section 5); no suspend/resume, no heartbeat, no watchdog |
| Doc 09 s4 `DataFeedProvider` (`Fixtures`, `LiveState`, `SuggestedOdds`, `Results`, `HandleCallback`) | architecture sketch, not code |
| ADR 0117 Part C `FeedProvider` (`Capabilities`, `Poll`, `Health`; versioned `FeedMessage`; push admission; ingest seals; result hold; settlement fan-out) | **ACCEPTED pending implementation; nothing implemented** ("Nothing is implemented and no stage is authorised"). Slices C1..C5 unscheduled |

So the answer to "how would the platform consume sports data" is: it would not, today. The smallest neutral seam that
could be proposed is described in section 8 as a **candidate**, not an implementation, and it is the first slice (C1) of
ADR 0117 Part C, restricted to MOCK.

## Index

| File | Content |
|---|---|
| `provider-neutral-mapping.md` | What a sports-data provider must supply, how (if at all) the current code can carry it, the smallest neutral seam candidate, platform-gap candidates `SD-G01..` (section 9), UNVERIFIED registry `U-SD-nn` (section 10). |
| `conformance-plan.md` | Existing suites that apply (catalogue sync, settlement, sportsbook reconciliation), the conformance suite the first feed adapter must bring (specified from ADR 0117 C.6), new cases `SD-C-nn`. |
| `integration-gates.md` | Gates D0..D9, entry criteria, evidence, exact owner/vendor inputs. |
| `open-questions.md` | The minimum decisions/inputs `OQ-SD-nn`, each with a recommendation. |

## Headline gaps (detail in `provider-neutral-mapping.md` section 9)

1. `SD-G01` No ingestion seam exists (above). Candidate: ADR 0117 Part C slice C1 with a MOCK feed only.
2. `SD-G02` The catalogue sync is last-writer-wins on a globally UNIQUE `external_ref` (no provider/feed namespace): a
   re-sync can re-open a finished event or closed market, re-parent a market and overwrite odds with no ordering or
   version. A feed cannot safely write through it.
3. `SD-G03` No credential seam for a data feed (`providercred` domains `payments`, `kyc`, `casino` only); ADR 0117 chooses
   a platform-scope credential and a new push-admission domain, both unbuilt.
4. `SD-G04` Results are money-moving input with no result entity, no sealing, no hold, no four-eyes confirmation and no
   fan-out consumer; the only driver is a test route. Dead-heat and fractional outcomes are unrepresentable.
5. `SD-G05` Bet acceptance reads price and status unlocked with no staleness bound and allows live events: a real feed
   makes this unsafe (SB-G06).

## Principles applied

- A feed message never posts money (SBF-3). Only `settleBet`/`voidBet`/`rollbackBet` post, under the bet's lock; the
  payout comes from the bet's frozen odds, never from a feed (SBF-4, ADR 0088 s2.4).
- Stale, unconfigured or unauthenticated data fails closed: no new bets, no resume, no settlement (SBF-6, SBF-9).
- The tenant scope of any fan-out comes from the `tenants` table and the bet rows, never from feed data (SBF-8).
- Providers adapt to a canonical shape; the core is not redesigned around the first feed. Gaps are numbered with scope,
  reviewer and a minimal proposal. No core change is made by this package.
- Rights and licensing of sports data (official data, display, redistribution) are commercial/legal inputs for the owner;
  this package says nothing about them.
