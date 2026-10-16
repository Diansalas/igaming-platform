# Provider-neutral mapping: sports-data requirement -> provider supply -> platform carrier -> gap

Status: **SPECIFICATION (`NOT IMPLEMENTED`).** Provider-agnostic: any fixtures/odds/results feed. **DEFERRED with
sportsbook** (owner decision 2, 2026-10-15). Every "provider must supply" cell is a requirement on the vendor, not a
statement of what any vendor does. Code and migrations were read at base `4a3c85f` of `claude/focused-wright-jw88w9`.
`U-SD-nn` = unverified vendor assumption (registry in section 10); `SD-Gnn` = platform-gap candidate (section 9).

Column legend: **Carrier** = what in the platform could carry it today (`none` where nothing exists). **Gap** = what is
missing or at risk; `PW` platform-wide, `PS` provider-specific; reviewer in brackets.

## 0. Where sports data enters the platform (consumption model)

```
provider feed --(adapter, canonicalization once)--> canonical sports data
   --> sb_* catalogue (fixtures, markets, selections, prices, status)  --> PlaceBet acceptance (reads, never prices)
   --> results (per market)  --> settlement entry points settleBet/voidBet/rollbackBet (the only money writers)
```

(doc 09 s4.2; ADR 0117 C.2.) Today only the first arrow's tail exists, as the once-at-startup `Provider.Catalogue` sync,
and the result path has only the test-support driver. Sports data never prices on the platform's behalf unless a feed
supplies suggested odds that a human/trading function adopts; the platform does not own odds/trading (CLAUDE.md).

## 1. Transport

| Platform requirement | Provider must supply | Carrier (code) | Gap / UNVERIFIED |
|---|---|---|---|
| Pull (poll) with a durable cursor and bounded responses | A poll endpoint returning ordered batches after a cursor | `FetchCatalogue` is a one-shot full snapshot with a context deadline and no cursor (`catalogue.go:62`); ADR 0117 designs `FeedProvider.Poll(ctx, FeedCursor, limit) (FeedBatch, error)` + `sb_feed_sources.poll_cursor` (unbuilt). Polling endpoints only from compiled adapter config or dual-control platform config with an HTTPS host allowlist; private ranges denied; cross-host redirects refused (the generic `httpclient` already refuses redirects); response size caps; cursors and feed fields never used as URLs | **SD-G01**. U-SD-01 (poll availability, rate limits, batch size, snapshot vs delta) |
| Push (webhook / stream) | A signed push, or a persistent connection | **No carrier.** ADR 0117 C.2 point 2 designs a new platform-scope admission domain `sportsbook_feed` (own signing prefix and headers, platform credential in the secret store, mTLS or a source allowlist ahead of the pre-auth buckets, a dedicated pre-auth bucket, rate buckets keyed by `feed_id`, mandatory `ReplayTimestampWindow`). `webhookauth/domains.go` has only payments/kyc/casino prefixes. A persistent stream (socket, message queue) has no carrier at all: the platform has no long-lived inbound/outbound stream consumer | **SD-G03**. A streaming protocol that is neither HTTP push nor poll is outside the design: U-SD-02 |
| Both push and poll yield the same result | n/a | designed test C.6 "push and poll produce the same result" | |
| Outage and backpressure | Documented SLAs | heartbeat watchdog designed (C.2 point 8); nothing built | section 6 |
| No vendor call while a DB transaction is held | n/a | `FetchCatalogue` refuses under `txscope.Held` (`ErrCatalogueFetchRefused`); the feed design repeats it (ADR 0095) | none |

## 2. Authentication

| Platform requirement | Provider must supply | Carrier (code) | Gap / UNVERIFIED |
|---|---|---|---|
| Inbound push authenticated verify-first over raw bytes, constant time, replay window | A signature scheme the platform can express as a `VerificationScheme` | `webhookauth.VerificationScheme`, `conformanceManifest` (empty), SC1..SC13; a feed is **platform-scope** (market-wide, not per-tenant): ADR 0117 chooses a platform credential held in the secret store, not per-tenant `providercred` | PSP G-01 (header-only `Extract`), G-02 (mandatory signed timestamp) apply identically; G-08 less so (one platform credential). U-SD-03 (scheme) |
| Outbound poll authentication | An API credential | `providercred` domain CHECK allows `payments`, `kyc`, `casino` only (migration 0096); `Provider.Catalogue(ctx)` takes no `CallContext` | **SD-G03 / X-G01** (`../README.md`) |
| Source allow-listing | Published IP ranges / mTLS | ADR 0117 C.2 point 2: mTLS or source allowlist ahead of the pre-auth buckets | environment input. U-SD-04 |
| Message integrity beyond transport | Optionally per-message signatures or hashes | `sb_feed_messages.payload_sha256` designed | U-SD-03 |

## 3. ID mapping

| Platform requirement | Provider must supply | Carrier (code) | Gap / UNVERIFIED |
|---|---|---|---|
| Platform identity is its own; vendor ids are non-authoritative external references (doc 09 s2.5, s4.2) | Stable ids per sport, competition, event, market, selection (and participants for results) | `sb_sports/competitions/events/markets/selections` each have `external_ref TEXT NOT NULL UNIQUE` (migration 0078) and a platform `id`; upsert `ON CONFLICT (external_ref)` (`catalogue.go:191-266`); bounded 1..255 bytes by `providerref` | **SD-G02 [PW, sportsbook+architect]**: `external_ref` is **globally** UNIQUE with no provider or feed column. Two feeds (or a feed and a sportsbook provider) with overlapping ids collide; one feed id space cannot be retired or replaced without id surgery. Entity move: `ON CONFLICT` also overwrites the parent id, so a re-sync can re-parent a market or event |
| Cross-feed identity (the same fixture from two sources) | n/a | none: no cross-reference table | needed only for a second source (OQ-SD-03). U-SD-05 |
| Unmapped references: never create from a live message | n/a | designed: "unmapped references are recorded, never created" (ADR 0117 C.2 point 3); today the sync CREATES everything it sees | **SD-G02**: pre-provisioning vs auto-creation is a policy choice (OQ-SD-05) |
| Participants / teams / players | Participant ids and names | **No participant entity**: names are folded into `Event.Name` ("Arsenal vs Chelsea") (`types.go`); results by participant, player props, and name-based matching cannot be represented | **SD-G06**. U-SD-06 |
| Market type vs instance | Market type code + line parameters | no `MarketType`/line: a market is a name and fixed selections; handicap/total lines, in-running variants and player props have no carrier (also SB, U-SB-05) | SD-G06 |
| Entity lifecycle (event postponed, rescheduled, abandoned, merged ids) | Explicit lifecycle messages | `EventStatus` scheduled/live/finished/cancelled only; no postponed/abandoned/suspended-event state; `start_time` overwritten by sync | SD-G06. U-SD-07 |

## 4. Sequencing, ordering, duplicates

| Platform requirement | Provider must supply | Carrier (code) | Gap / UNVERIFIED |
|---|---|---|---|
| Per-entity monotonic ordering (SBF-1) | A per-entity sequence number or a trustworthy timestamp | **No version column anywhere.** Current sync is last-writer-wins (`ON CONFLICT ... DO UPDATE SET status = EXCLUDED.status, odds = EXCLUDED...`): an older snapshot arriving after a newer one overwrites it, and a snapshot can set an event back from `finished`/`cancelled` to `scheduled` or a market from `closed` to `open`. ADR 0117 designs `feed_version`, `odds_version`, `state_updated_at` and `stale_dropped` (unbuilt); an adapter must declare `entity_sequence` or `timestamp_sequence`, and "an adapter that supports neither is not admissible" | **SD-G02**. U-SD-08 (which ordering primitive exists; per entity or per feed; resets on feed restart) |
| Duplicate suppression (SBF-2) | A message id | designed `(feed_id, message_id)` primary key + dedupe; none today | U-SD-09 |
| Gaps in a sequence | Gap detection or replay-from-cursor | designed: poll cursor; push has no gap detection | U-SD-10 (replay window, full-snapshot resync) |
| Clock | Provider timestamps in a known time base | the platform uses its own clock (DB clock) for freshness; `produced_at` recorded | U-SD-08 |

## 5. Corrections

| Case | Platform handling designed / existing | Gap |
|---|---|---|
| Price update | designed: append-only `sb_price_history (selection_id, odds_version, num, den, source, effective_at)`; bets freeze odds at acceptance, so an update never changes an open bet | none for open bets (frozen). The catalogue today stores current odds only, no history |
| Event/market state change (suspend, resume, close, cancel) | designed: `sb_state_changes` append-only with source feed/staff/watchdog; applied only forward in `feed_version`; a market resumes only on an authenticated fresh state message or a staff action | **SD-G05** unbuilt |
| Result before the event starts / for a market not suspended or closed | designed hold triggers (C.2 point 4) | unbuilt |
| Result correction after settlement | designed (SBF-5): rollback(g) + settle(g+1) in ONE transaction, `g` from the database; a lower result version is a no-op; the same outcome under a newer version is a non-financial adoption row; a payout-raising correction is held for four-eyes | settlement entry points exist (ADR 0088); the dispatch consumer does not |
| Correction after a void | void is terminal (ADR 0088); a correction raises an alert for manual handling | accepted by design |
| Dead heat, fractional or partial outcomes | a message carrying one is rejected and alerts (C.3) | **SD-G06**: unrepresentable; NOT IMPLEMENTED |
| Late or retroactive corrections after the bet window / after payout | no time limit modelled | policy decision (OQ-SD-06); a correction long after settlement may find the winner's balance spent: rollback posting then relies on the ledger rule for negative balances (the casino path has LF-10 as the analogous open decision) |

## 6. Stale data, suspension, heartbeat

| Platform requirement | Provider must supply | Carrier (code) | Gap / UNVERIFIED |
|---|---|---|---|
| A stale or unconfigured feed means no new bets (SBF-6) | A heartbeat or a guaranteed update cadence | designed: `heartbeat_max`, `suspend_after`, `live_price_max_age`, `RejectionFeedStale`, unconfigured staleness also refuses (C.2 point 7c). **Today**: `PlaceBet` reads price/status with no age check, allows live events, and the catalogue is synced once at startup (`orchestrator.go`, `main.go:361`) | **SD-G05 [PW, sportsbook+security+risk]**: with a live feed this is unsafe; until built, refuse live events and run pre-match only. U-SD-11 |
| Heartbeat loss suspends the feed's live markets, audited, alerted | n/a | designed (C.2 point 8): watchdog with its own allowlisted identity and a single-call-site test; resume only on an authenticated fresh state message or a staff action | unbuilt |
| Acceptance race: price/status read while a message applies | n/a | designed: event/market/selection rows read `FOR SHARE` as lock class C0, an explicit ADR 0082 R8 exception (C0 taken before L0-L4, never after); the grant needed for `FOR SHARE` (UPDATE privilege) is a C2 check with `security` | unbuilt |
| Price-change policy at acceptance | n/a | designed: `exact` by default; `accept_better` only with the C0 live-price rule; today exact equality | unbuilt |
| Staff trading operations (suspend, void event, manual result) | n/a | designed as ADR 0110 actor-proof operations, manual results four-eyes | unbuilt; Back Office read route belongs to the gap-audit item 9 |

## 7. Settlement data (results)

| Platform requirement | Provider must supply | Carrier (code) | Gap / UNVERIFIED |
|---|---|---|---|
| A result per market naming exactly the market's own selections | Winner/loser/void per selection, official result status, result version | **No result entity.** Designed `sb_market_results` (append-only, sealed: `market_id`, `result_version`, `selection_outcomes`, `is_void`, status applied/pending_confirmation/confirmed/rejected, `ingest_seal`; UNIQUE `(market_id, result_version)`); a result naming anything but the market's own selections is `rejected_invalid` and alerted | **SD-G04 [PW, sportsbook+ledger-finance+security]**. U-SD-12 (result granularity, void/dead-heat expression, final vs provisional results) |
| Results are money-moving: mandatory hold | n/a | designed (C.2 point 4, OQ-C2): hold in `pending_confirmation` when the market's total or any single payout exceeds the platform / per-tenant per-asset threshold, a correction raises payout, the result arrives before `start_time`, or the market was not suspended/closed under C0; confirmation is ADR 0110 actor-proof four-eyes; **no configured threshold disables feed-driven settlement** (fail closed); MOCK may use 0 = off | unbuilt; threshold values are owner/risk inputs (OQ-SD-04), never invented |
| Provisional vs official results | A finality flag | none | U-SD-12 |
| Settlement fan-out | n/a | designed (C.2 point 5): `sb_settlement_dispatch` ordered per `market_id`; tenant set from `tenants` incl. non-active tenants with open bets; per `(result_id, bet_id)` item states; per bet under the L1 lock re-read the market's latest `result_version` vs the bet's last applied `result_id`: unsettled -> settle g1; different outcome -> rollback(g)+settle(g+1) in one tx; same outcome newer version -> adoption row; lower -> no-op; void -> void; refused bet never dead-letters the partition; `driver='feed'`, NULL staff actor | unbuilt. The three money writers it would call exist and are pinned by tests (ADR 0088) |
| Payout | n/a | always from the bet's frozen odds (SBF-4) | SB-G03 (provider payout differences) |
| Settlement-source trust (single feed) | n/a | the design has one result source plus staff manual results; no second-source cross-check | **SD-G07**: a single wrong result mass-settles; OQ-SD-04 decides second source/mandatory hold threshold |

## 8. The smallest neutral seam (a CANDIDATE, not an implementation)

If and when a sportsbook brand is scheduled, the smallest seam that lets a first sports-data provider be integrated
without redesigning the core is **ADR 0117 Part C slice C1, MOCK feed only, no settlement**:

1. A `FeedProvider` interface (`Capabilities`, `Poll`, `Health`) and canonical `FeedMessage{FeedID, MessageID, Kind
   (fixture|event_state|market_state|price|result|heartbeat), EntityRef, Version, ProducedAt, Body}` in
   `internal/sportsbook/datafeed` (doc 09 s4.3 package placement), with `sb_feed_sources.kind CHECK ('mock')` until an ADR
   admits a real kind.
2. Ingest `Apply`: one message per transaction under a dedicated platform-service identity (`sportsbook_feed_ingest`),
   dedupe `(feed_id, message_id)`, version monotonicity, unmapped references recorded not created, append-only history
   (`sb_feed_messages`, `sb_price_history`, `sb_state_changes`), and ingest seals (S-B2).
3. Feed-namespaced references on the catalogue rows (`feed_id` + `feed_version`) replacing the global `external_ref`
   uniqueness for fed entities (SD-G02).
4. No settlement in C1. Results (SD-G04) and the dispatch consumer are C3, ledger-finance-owned.

Everything else (push admission C4, acceptance rules C2, cashout, multiples, a real adapter C5) follows in later slices.
This is the architect's recorded design (ADR 0117), summarised here; it is not re-designed, and nothing is built or
authorised by this package.

## 9. Platform-gap candidates (for `architect`, `security`, `ledger-finance`, `sportsbook`; no core change is made here)

Shared with other packages and not repeated: PSP G-01/G-02 (webhook scheme shape), X-G01 (credential domain).

| Id | Gap | Scope | Reviewer | Blocks first real adapter? | Minimal proposal (only if confirmed necessary) |
|---|---|---|---|---|---|
| SD-G01 | No ingestion seam: no feed interface, tables, versions, history, watchdog, push admission | PW | architect, sportsbook, security | **Yes** | Slice C1 of ADR 0117 Part C, MOCK only (section 8); needs `architect` + `sportsbook` scheduling |
| SD-G02 | Catalogue identity and writes: `external_ref` globally UNIQUE (no feed namespace); sync is unordered last-writer-wins on status, odds, parent and `start_time`; it can reopen a finished event or closed market; it creates unseen entities | PW | sportsbook, architect, security | Yes for any live or periodic sync | Feed-namespaced refs, `feed_version` monotonic apply, forward-only status guard, unmapped-recorded-not-created policy (OQ-SD-05). Until built, sync only at startup from a trusted snapshot and refuse live |
| SD-G03 | No credential seam for a feed; no push-admission domain or platform-scope credential path | PW | security, architect | Yes | X-G01 decision; ADR 0117 C.2 point 2 (platform credential in the secret store, `sportsbook_feed` admission domain with mTLS/allowlist) |
| SD-G04 | Results: no result entity, seal, hold, four-eyes confirmation, fan-out consumer, or driver column; only a test route settles | PW | ledger-finance, sportsbook, security | Yes (to settle from a feed) | ADR 0117 C.2 points 4-5, slice C3 (ledger-finance-owned); `sb_simulation_driver_in_production` P1 check |
| SD-G05 | Acceptance has no staleness bound, no odds version, no price-change policy, no C0 lock, allows live; no watchdog | PW | sportsbook, risk, security | Yes for live/feed prices | ADR 0117 C.2 points 7-8, slice C2/C4; until then pre-match only and refuse live |
| SD-G06 | Canonical model too small: no participants, market types/lines, postponed/abandoned states, dead-heat/fractional outcomes, props | PW | sportsbook, architect | Depends on product scope | Restrict the first brand's scope (OQ-SB-03); extend the model per feature with its own ADR |
| SD-G07 | Result-source trust: a single feed, no cross-check; thresholds are owner inputs | PW | risk, ledger-finance, security | Policy decision | Mandatory hold above thresholds (designed) and/or a second source for high-value markets (OQ-SD-04) |
| SD-G08 | Licensing/rights of sports data (display, redistribution, official data) | non-code | owner, legal | Owner/legal input | Commercial term; not a platform concern |

## 10. UNVERIFIED registry (needs provider documentation/sandbox)

12 entries. Closure is by citation (URL + retrieval date), vendor written answer or sandbox capture.

| Id | Question |
|---|---|
| U-SD-01 | Poll availability, rate limits, batch size, snapshot vs delta |
| U-SD-02 | Streaming protocols (sockets, queues) vs HTTP push/poll |
| U-SD-03 | Push authentication scheme and per-message integrity |
| U-SD-04 | Source IPs / mTLS availability |
| U-SD-05 | Cross-feed identity for the same fixture |
| U-SD-06 | Participant ids/names and props |
| U-SD-07 | Event lifecycle states (postponed, abandoned, suspended, id merge) |
| U-SD-08 | Ordering primitive (per entity or feed sequence, timestamps, base, restart behaviour) |
| U-SD-09 | Message ids and duplicate delivery |
| U-SD-10 | Gap detection, replay window, full resync |
| U-SD-11 | Update cadence, heartbeat, latency SLA |
| U-SD-12 | Result granularity, void/dead-heat expression, provisional vs final, correction policy |

## 11. What must be true before any of this is real

See `integration-gates.md`. Not before a sportsbook brand is scheduled and the operating model is decided
(`../sportsbook/open-questions.md` OQ-SB-01). Then: the ADR 0117 C1 seam with a MOCK feed (gate D0), documentation review
(D1), adapter against recorded fixtures (D2), conformance (D3), `security` (D4) and `ledger-finance` (D5) review, sandbox
(D6), operational acceptance (D7), pre-production (D8), explicit production authorization (D9). No real money until D9, in
writing, from the owner.
