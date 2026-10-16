# Provider-neutral mapping: sportsbook platform requirement -> provider supply -> neutral interface -> gap

Status: **SPECIFICATION (`NOT IMPLEMENTED` as an adapter).** Provider-agnostic. **Sportsbook is DEFERRED** until a
sportsbook brand is scheduled (owner decision 2, 2026-10-15). Every "provider must supply" cell is a requirement on the
vendor, not a statement of what any vendor does. Code references were read at base `4a3c85f` of
`claude/focused-wright-jw88w9`. `U-SB-nn` = unverified vendor assumption (registry in section 11); `SB-Gnn` =
platform-gap candidate (section 10).

Column legend: **Interface** = how the neutral interface carries it today. **Gap** = what is missing or at risk; `PW`
platform-wide (any real provider), `PS` provider-specific; reviewer in brackets.

## 0. Operating model: what the platform owns and what it does not

| Question | Platform today (verified) | Consequence for a provider |
|---|---|---|
| Who prices and trades? | Not the platform (CLAUDE.md). Odds come in through `Provider.Catalogue()` as integer numerator/denominator per selection (`types.go` `CatalogueSelection`); no in-house trading engine, no margin model | A provider (or a sports-data feed, `../sports-data/`) supplies prices |
| Who accepts the bet? | The platform: `PlaceBet` (`orchestrator.go:147`) accepts at the selection's current stored odds, same process, synchronous; `sportsbook_bets.provider_id` / `provider_bet_reference` exist (migration 0081) but are always NULL | A vendor that must accept the bet (it owns the risk or the bet ticket) has no placement call: **SB-G01** |
| Who carries the liability? | The tenant/platform book: the stake sits in `player_locked_cash` until settlement; `house_gaming` is credited at settlement; potential return is a domain projection, not a ledger fact (ADR 0038 s2) | Risk transfer to a vendor is not modelled |
| Who decides the payout? | The platform: payout = `bet.potential_return` frozen at acceptance; a provider's stated payout is a claim that must equal it (ADR 0088 s2.4, DB trigger T-1) | Dead-heat, rule deductions, boosted odds, cashout and free-bet payouts cannot post: **SB-G03** |
| Who decides the result? | A settlement event (settle won/lost, void, rollback). Only the non-production test-support route drives it today (`sportsbook_settlement_handlers.go`), registered only when `Environment != production` and the test flag is set | A real result source is sports data (`../sports-data/`) or a provider settlement push (**SB-G04**) |

The decision between "platform books against a data feed" and "a sportsbook vendor books and the platform is the wallet"
is OQ-SB-01. This package covers the neutral requirements for both without choosing.

## 1. Bet placement and open-bet liability

| Platform requirement | Provider must supply | Interface (code) | Gap / UNVERIFIED |
|---|---|---|---|
| Idempotent placement keyed by the client's key, short-circuiting before any gate or evaluation | n/a (in-house) / for a vendor-accepted bet: an acceptance reference unique per bet | `PlaceBetParams.IdempotencyKey`; `findBetByIdempotencyKey` scoped by (tenant, player); ledger key `sportsbook_bet:<player_account_id>:<client key>`; a match with different selection/stake/asset/odds is a key reuse error (`betMatchesParams`) | `Bet.ProviderBetReference` stored nullable with a partial unique index `(tenant_id, provider_id, provider_bet_reference)` (migration 0081): a landing place, no code writes it. U-SB-01 (acceptance reference, sync/async, pending states) |
| Stake locked as the open liability | n/a | Dr `player_cash` / Cr `player_locked_cash` (`TxSportsbookBet`), bet row inserted AFTER `ledger.Post` (ADR 0082 E-3); `NetLocked` assertions at settlement (ADR 0088 s2.2) | **A vendor-accepted bet with asynchronous acceptance** (pending, then accepted or rejected) has no pending state: `BetStatus` is open / settled_won / settled_lost / void only. SB-G01 |
| Bet shapes | Single selection | Singles only; no multiples, systems, bet-builder, live-specific logic (`types.go` package doc) | **SB-G05** |
| Potential return | n/a | `computePotentialReturn` with `*big.Rat` and ADR 0021 rounding, frozen, immutable by trigger (migrations 0082, 0087) | rounding rule must match the vendor's published rounding if the vendor states returns: U-SB-02 |
| Odds acceptance | Current price per selection | exact equality of numerator/denominator against the stored selection (`RejectionOddsChanged`); event finished/cancelled, market not open, selection not active refuse (`RejectionEventNotOpen`); **live events are not refused** (comment: "live is allowed since this stage builds no in-play-specific logic") | The stored price is whatever the last catalogue sync wrote (once at startup). Status and price are read without a lock (ADR 0117 C.1). No staleness bound, no odds version, no price-change policy: **SB-G06**, `../sports-data/` SD-G06 |
| Gate chain on a NEW bet | n/a | order in `PlaceBet`: params -> `MarketGuard` (A8) -> idempotency short-circuit -> tenant active -> brand active -> selection/event/market/odds -> jurisdiction restriction (`sb_jurisdiction_restrictions`, `jurisdiction.go`) -> RG `EvaluateAction` -> Risk (`OperationSportsbookBet`) -> KYC play + AML hold (`kyc.EnforcementSportsbookPlay`) -> cross-player exposure (`evaluateExposureLimits`, unarmed by default, HDR-SB-1) -> wallet lock + balance + post. Rejection categories closed: `odds_changed`, `event_not_open`, `insufficient_funds`, `rg_denied`, `risk_denied`, `jurisdiction_denied`, `exposure_limit`, `kyc_denied`, `tenant_not_active`, `brand_not_active` | none platform-side. Settlement, void and rollback are NOT checkpoints for these gates (ADR 0038 s13, ADR 0088 s5.4) |
| Jurisdiction restrictions on events/markets | Per-event/market territory data | `sb_jurisdiction_restrictions` (migration 0087), platform-wide, annotated into catalogue reads as `available=false` + `not_available_in_your_jurisdiction` (marked, never omitted) | Provider territory data is not ingestible by `Provider.Catalogue` (no field); a human or a sync extension must map it. U-SB-03 |
| Cross-player book exposure ceiling | n/a | `sb_exposure_limits` (migration 0088) per scope kind and asset; unarmed = no rows = a single no-op SELECT | HDR-SB-1 (arming policy) open; a vendor-hosted book needs vendor-side limits instead |
| Bonus / free-bet funded stakes | A promotion mechanism if any | **BLOCKED platform-wide** pending the `player_locked_cash`/`player_locked_bonus` origin-split decision for sportsbook (ADR 0038 s9, HR-9); external grants (ADR 0033: `ExternalRewardProvider`, vendor free-bet tokens) are designed, not built | **SB-G05**. U-SB-04 (vendor-side free bets, boosts) |

## 2. Markets and odds: what the platform owns vs not

| Item | Owner | Detail |
|---|---|---|
| Prices, margins, trading decisions, market creation | Provider / data feed | The platform stores the last-synced catalogue and never prices |
| Canonical catalogue identity | Platform | Platform mints its own ids; vendor `ExternalRef` retained per level, bounded by `providerref` (`catalogue.go` `validateCatalogueReferences`); upsert keyed by `(parent id, external_ref)`; `SyncCatalogue` requires the `sportsbook_catalogue_sync` platform service scope (migration 0084) |
| Catalogue refresh | Platform (mechanism) | Fetched outside any tx, validated whole before any write, upserted once at startup (`cmd/platform-api/main.go:361-391`); no periodic refresh, no odds history, no versions: **SB-G06** |
| Market/selection kinds | Provider | One `Market` is a named set of fixed `Selection`s with integer odds; no market type template, no handicap/line parameter, no player props, no dynamic selection sets: the mapping of a vendor's market catalogue onto this shape is lossy. U-SB-05 |
| Odds representation | Platform | `int64` numerator/denominator; decimal odds `250/100` = 2.50; a vendor's American/fractional/decimal-with-many-places must convert exactly or be refused. U-SB-06 |

## 3. Settlement, void, cancellation, rollback

| Platform requirement | Provider must supply | Interface (code) | Gap / UNVERIFIED |
|---|---|---|---|
| Settle won / lost for a cash-funded single | A result per bet or per market, with a per-occurrence ordinal | `SettlementEvent{BetID, EventType settle|rollback|void, Generation, Outcome won|lost, ClaimPayoutAmount, ClaimAssetCode, VoidReason}` (`settlement.go:146`); `SimulateSettlementEvent`; decision table ADR 0088 s4.3 evaluated under the bet row lock; entries ADR 0088 s2.3 (lost: Dr `LOCKED` S / Cr `HOUSE` S; won: that plus Dr `HOUSE` P / Cr `CASH` P, full payout not winnings) | The key is `sportsbook_settlement:<bet_id>#<g>` with `g` **caller-supplied, server-validated against the max** (never counted). A real provider must supply a per-bet occurrence signal; if it only sends "settled" repeatedly (resend vs correction) the adapter cannot tell a duplicate from generation g+1. **SB-G02**. U-SB-07 |
| Payout is derived, never an input | n/a | V-1..V-5 (ADR 0088 s2.4): claim must equal `potential_return` for won, 0 for lost; mismatch = `PAYOUT_INVALID` rejection with integrity alert, nothing posted; the T-1 trigger repeats the check at the database | **SB-G03 [PW, ledger-finance+sportsbook]**: any provider-stated payout that legitimately differs (dead heat, rule 4 style reductions, boosted/insured/cashed-out bets, rounding) cannot post |
| Void before settlement (cancelled market, push, data error) | A void event with a reason | `void_reason` closed set `market_cancelled`, `push`, `data_error` (migration 0091); stake returned Dr `LOCKED` / Cr `CASH`; **void is terminal** (ADR 0088) | A correction after a void is manual (ADR 0117 SBF-5 alerts). U-SB-08 (the vendor's void taxonomy incl. partial void, dead-heat, abandoned event) |
| Void after settlement | n/a | composed rollback-then-void in ONE tx (two history rows) | none |
| Rollback of a settlement and re-settlement (correction) | A correction signal | `rollback(g)` then `settle(g+1)`; a rollback for a never-seen settlement writes a tombstone occupying the same key, so a late original is refused (`SETTLEMENT_TOMBSTONED`); `causation_record_id` rules and the **XMIN check** (trigger T-1, migration 0091 amended by 0093: `pg_xact_status` of the reconstructed xid8 so a row inserted under a released SAVEPOINT of the SAME transaction counts as same-transaction, while an earlier committed rollback is rejected) | The XMIN rule means a batch or webhook driver MAY wrap each bet in a SAVEPOINT (test `TestSettlementScenario_ComposedVoid_InsideOuterSavepoint_ServiceLevel`); a market-wide settlement fan-out must call this per bet under the L1 lock |
| Partial settlement (per-leg, half-win/half-lose, dead heat) | n/a | **NOT IMPLEMENTED**; singles only; dead-heat and fractional outcomes rejected with an alert in the feed design (ADR 0117 C.3) | **SB-G05** |
| Cashout | n/a | **NOT IMPLEMENTED**; hook only, a `CashoutPricer` needs its own ADR (ADR 0117 C.2 point 9); doc 09 s2 folds it into `sportsbook_settlement` shaped entries | **SB-G05**. U-SB-09 |
| Cancellation of an open bet by the player/operator | n/a | No cancel path other than void-by-event; self-exclusion auto-void consumer NOT IMPLEMENTED (R-2, ADR 0042 default) | **SB-G09** |
| Tenant not active | n/a | new settlement, void, rollback and tombstone postings refused (`SETTLEMENT_TENANT_NOT_ACTIVE`, R3-GAME-POSTINGS-NONACTIVE-1) except replays | the vendor's retry window vs a suspension: U-SB-10 |
| Actor | n/a | in-house: authenticated staff principal (`ActorStaffID`), audit action `sportsbook_bet.settled` etc.; the feed design sets `driver='feed'` and a NULL staff actor (ADR 0117 C.2 point 5) | a provider-driven path needs a non-human actor identity and fence (like KYC's `PlatformService`) |

## 4. Provider callbacks and authentication

| Platform requirement | Provider must supply | Interface (code) | Gap / UNVERIFIED |
|---|---|---|---|
| Authenticated inbound settlement/result/bet-status notifications, verify-first, tenant-bound by route and per-tenant key | A signed push or a pollable endpoint | **No sportsbook route, no `WebhookScheme` on `Provider`, no `VerifiedCallback`, no admission domain.** The sibling routes are `POST /v1/webhooks/{payments|casino|kyc}/{tenantSlug}/{providerID}`; there is no `/v1/webhooks/sportsbook` (grep of `internal/httpserver`) | **SB-G04 [PW, security+sportsbook+architect]**. The same shared gaps apply once designed: PSP G-01 (header-only `Extract`), G-02 (mandatory signed timestamp), G-08. U-SB-11 |
| A provider-keyed event must resolve to a tenant and a bet without trusting the payload | A bet reference we can map | The tenant would come from the route + per-tenant credential; the bet from `(provider_id, provider_bet_reference)` which is NULL for every bet today | SB-G01/G04 |
| Market-wide result pushes (one result, many tenants/bets) | n/a | The feed design fans out under a platform-scope source and a tenant set read from the `tenants` table (SBF-8), ordered per market, never dead-lettering the market partition (SBF-10) | `../sports-data/`; a per-tenant sportsbook webhook does not fit a market-wide result |
| Outbound credentials and call context | An API credential | `providercred` domains are `payments`, `kyc`, `casino` only (migration 0096 CHECK); `Provider.Catalogue(ctx)` takes no `CallContext` (`types.go`) | **X-G01** (`../README.md`); the feed design uses a platform-scope credential in the secret store instead (ADR 0117 C.2 point 2) |
| Retry semantics | Documented redelivery policy | `MustRequireRetrySemantics` exists for payments/casino/kyc | add `sportsbook` when SB-G04 is designed. U-SB-12 |

## 5. Idempotency (summary)

| Concern | State |
|---|---|
| Placement | unique `(tenant_id, idempotency_key)` on `ledger_transactions` (the only idempotency authority in in-house mode, ADR 0088 s4.1) |
| Settlement / rollback / void | `sportsbook_settlement:<bet>#<g>`, `sportsbook_rollback:<bet>#<g>`, `sportsbook_void:<bet>`; history table partial unique indexes enforce state (one settlement per generation, one rollback per settlement, one void per bet); payload comparison on replay; tombstone shares the settlement key |
| Provider references | `(provider_id, provider_bet_reference)` is the CLAUDE.md-equivalent key for provider-mode (ADR 0038 s14.6) and is **non-goal / NOT IMPLEMENTED** in W1 (ADR 0088 s1.2: "provider-mode idempotency and provider references (s I; R-1)"). The provider's own settlement-event id is stored nowhere. **SB-G02** |

## 6. Reconciliation and the statement source

| Platform requirement | Provider must supply | Interface (code) | Gap / UNVERIFIED |
|---|---|---|---|
| Ledger consistency (vendor independent) | n/a | `sportsbook_settlement` stream checks (a) per wallet+asset `player_locked_cash` net over sportsbook types == sum of open stakes and `player_locked_bonus` net == 0, (b) per bet nets equal the end state for its status, (c) two-way orphan check + status == history-derived status, (d) statement match; zero tolerance, `big.Int` sums; mismatch kinds `sb_locked_mismatch`, `sb_bet_net_mismatch`, `sb_orphan_ledger`, `sb_orphan_history`, `sb_status_mismatch`, `sb_mock_statement_mismatch` (`reconciliation/sportsbook_settlement.go`) | none for (a)-(c) |
| Provider settlement statement | A per-bet report of its current settlement state | `statement.SportsbookSettlementSource.StatementLines(ctx, tx, tenantID) []SportsbookSettlementLine{BetID uuid, Generation *int, Outcome, PayoutAmount, AssetCode, Void}`; a bet with no current state has no line | **SB-G07 [PW, ledger-finance+security+architect; blocks real money]**: (1) the line is keyed by the PLATFORM bet id, a provider statement is keyed by the provider's bet ref; (2) the interface takes the run's `pgx.Tx` (it reads inside the transaction), the opposite of the casino/payment fetcher shape (fetch outside, match inside, `txscope`); a real source must be a fetcher; (3) no stake, odds, commission or settlement time field; (4) one global `sbSource` for the sweep (`scheduler.go`), no per-(tenant, provider) registry, no read-only attestation or eligibility marker (the payments registry has them); (5) mismatch kind is `sb_mock_statement_mismatch` ("real adds its own kind"); (6) the stream recomputes the tenant's whole population each run (scale). U-SB-13 |
| Stored copy / coverage window | n/a | none for sportsbook (casino has `CoverageEnd`; sportsbook has no coverage anchor) | SB-G07 |
| Reconciliation source of truth | n/a | doc 09 s9: a provider's own settlement file is "used for reconciliation against the ledger ... never adopted as an alternative source of truth" | none |

## 7. Sportsbook GGR and open liability

| Concept | State |
|---|---|
| GGR timing | "sportsbook GGR is only known at settlement" (doc 09 s9): `house_gaming` is credited at settlement (lost) or debited (won payout); stake sits in `player_locked_cash` meanwhile |
| Open-liability line | Required in every sportsbook report, computed from the ledger's `player_locked` projection, never from the trading engine's exposure figure; owned jointly with `data-analytics`. **No such report exists in code** (grep for open-liability in `internal` and `backoffice/src` finds none): **SB-G08 [data-analytics+ledger-finance]** |
| Exposure vs liability | Exposure (aggregate potential return per outcome across players) is a different number for a different purpose; unarmed by default |
| Reconciliation of the liability | check (a) above proves locked net equals open stakes per wallet |

## 8. Gates: RG, market, KYC, AML (summary; the provider sees only a rejection)

Enforcement is platform code on NEW bets only, in the order of section 1. Market (A8) uses request-country evidence with
unknown = refuse (`marketgate` `OpSportsbookWager`). KYC play and AML hold share one decline (`account_review`). There is
no per-bet vendor geolocation signal. A vendor-accepted bet would bypass `PlaceBet` entirely unless the adapter routes
acceptance through the same gate chain first: **SB-G01** requires that whatever operating model is chosen, the gate chain
runs BEFORE the stake is committed at the vendor.

## 9. Credentials and environments

`providercred` has no sportsbook domain (X-G01). The provider bundle holds `Sportsbook` as the mock catalogue provider and
`SettlementStmt` as the mock statement source (`registrations.go`); production refuses Synthetic components. Settlement
simulation is registered only outside production and any production settlement row with `driver='simulation'` is designed
as a P1 mismatch (ADR 0117 C.2 point 10; the driver column is part of the unbuilt feed design).

## 10. Platform-gap candidates (for `architect`, `security`, `ledger-finance`, `sportsbook`; no core change is made here)

Shared with the PSP package: PSP G-01/G-02/G-08 (webhook scheme shape, credential model) apply once SB-G04 is designed;
PSP G-04 (`statement_read` purpose) applies to the statement source.

| Id | Gap | Scope | Reviewer | Blocks first real adapter? | Minimal proposal (only if confirmed necessary) |
|---|---|---|---|---|---|
| SB-G01 | `Provider` has `Catalogue` only; no bet-placement / bet-status / cancel / cashout method; no pending acceptance state; `provider_bet_reference` never written; the operating model (platform books vs vendor books) is undecided | PW | architect, sportsbook, ledger-finance | **Yes (it is the first decision)** | Decide OQ-SB-01. Then a sibling interface for the chosen model, with a pending/accepted/rejected acceptance state added to `BetStatus` only if a vendor-acceptance model is chosen. Gate chain runs before the vendor commits the stake. ADR needed |
| SB-G02 | Settlement identity is `(bet_id, caller-supplied generation)`; provider event ids/versions are not stored or deduplicated; a provider that resends "settled" cannot be told from a correction | PW | ledger-finance, sportsbook | Yes for provider-driven settlement | Store `(provider_id, provider_settlement_ref)` on the history row with a unique index and map a provider version/ordinal to `generation` in the adapter; ledger key unchanged |
| SB-G03 | Payout is derived from frozen odds; legitimate provider payout differences cannot post (dead heat, deductions, boosts, cashout) and a mismatch is a rejection + integrity alert | PW | ledger-finance, sportsbook | Depends on U-SB-02/07/09 | Keep anti-minting. If needed, an explicit adjusted-payout settlement kind with provider evidence, four-eyes above a threshold, and a DB check that bounds it to a stated range; ledger-finance design only |
| SB-G04 | No inbound sportsbook route, scheme, retry declaration or admission domain; bet-keyed events need a tenant-safe mapping | PW | security, sportsbook, architect | Yes if the vendor pushes | Mirror `/v1/webhooks/{domain}/{tenantSlug}/{providerID}` with the shared preamble; market-wide results use the feed design (`../sports-data/`) instead |
| SB-G05 | Bet shapes and funding: singles only; cash only; no partial, cashout, multiples, systems, bet-builder, live; bonus/free-bet funding BLOCKED | PW | sportsbook, ledger-finance, bonus-engine | Depends on the vendor's product scope | Restrict the first sportsbook brand to pre-match singles, cash-funded (OQ-SB-03); each extension is its own ADR |
| SB-G06 | Acceptance reads status and price unlocked, exact-odds equality only, live allowed, catalogue synced once, no odds version, no staleness bound | PW | sportsbook, security, risk | Yes for any live or feed-driven price | The ADR 0117 C.2 acceptance rules (odds version, staleness refusal, `FOR SHARE` lock class C0) with the feed; refuse live events until then |
| SB-G07 | Sportsbook statement source: per platform bet id, takes the run tx, global source, no registry/attestation/eligibility, no coverage anchor, mock mismatch kind, whole-population run | PW | ledger-finance, security, architect | **Yes for real money** | Fetcher-shaped source (fetch outside, match inside, `CoverageEnd`) keyed by provider bet ref via the mapping; registry like payments; real mismatch kind (migration); scale design with CAS-RECON-SCALE-1's sportsbook counterpart |
| SB-G08 | Open-liability reporting not built; exposure limits unarmed (HDR-SB-1) | PW | data-analytics, ledger-finance, risk | Yes before real-money sportsbook | Build the open-liability line from the `player_locked` projection; owner/risk decide arming policy |
| SB-G09 | No player/operator cancel path; no self-exclusion auto-void (R-2) | PW | sportsbook, risk, identity-compliance | Owner/compliance decision | Decide per licence; the deferred consumer needs its own migration (ADR 0034 s14.7) |
| SB-G10 | Credential domain absent (X-G01) | PW | architect, security | Yes | The shared X-G01 decision |

## 11. UNVERIFIED registry (needs provider documentation/sandbox)

13 entries. Closure is by citation (URL + retrieval date), vendor written answer or sandbox capture.

| Id | Question |
|---|---|
| U-SB-01 | Bet acceptance reference; sync vs async; pending/rejected states; who accepts the risk |
| U-SB-02 | Potential-return computation and rounding the vendor publishes |
| U-SB-03 | Territory restriction data for events/markets |
| U-SB-04 | Vendor-side free bets, boosts, insurance and how they affect payout |
| U-SB-05 | Market taxonomy: handicap lines, props, dynamic selection sets vs fixed selections |
| U-SB-06 | Odds formats and precision |
| U-SB-07 | Settlement event shape: per-bet vs per-market, ordinal/version, resend vs correction |
| U-SB-08 | Void taxonomy: partial void, abandoned event, dead-heat |
| U-SB-09 | Cashout offer/acceptance flow and settlement representation |
| U-SB-10 | Retry window vs tenant suspension |
| U-SB-11 | Inbound authentication scheme (location, algorithm, timestamp, key id, per-operator keys) |
| U-SB-12 | Documented retry policy |
| U-SB-13 | Settlement statement/report: format, keys, latency, cut-off, includes stake/odds/commission |

## 12. What must be true before any of this is real

See `integration-gates.md`. Not before a sportsbook brand is scheduled. Then: documentation review (S1), adapter against
recorded fixtures (S2), conformance (S3), `security` and `ledger-finance` review (S4, S5), sandbox on a synthetic tenant
(S6), operational acceptance (S7), pre-production (S8), explicit production authorization (S9). No real money until S9,
in writing, from the owner.
