# Provider-neutral mapping: casino platform requirement -> aggregator supply -> neutral interface -> gap

Status: **SPECIFICATION (`NOT IMPLEMENTED` as an adapter).** Provider-agnostic: applies to any casino aggregator or
single studio. Every "aggregator must supply" cell is a requirement on the vendor, not a statement of what any vendor
does. Code references were read at base `4a3c85f` of `claude/focused-wright-jw88w9` (casino code is unchanged from
`12edec0`). `U-CAS-nn` = unverified vendor assumption (registry in section 12); `CAS-Gnn` = platform-gap candidate
(section 11).

Column legend: **Interface** = how the neutral interface carries it today. **Gap** = what is missing or at risk;
`PW` platform-wide (any real aggregator), `PS` provider-specific; reviewer in brackets.

## 1. Catalogue and game identity

| Platform requirement | Aggregator must supply | Interface (code) | Gap / UNVERIFIED |
|---|---|---|---|
| Platform owns game identity; provider id is never promoted to platform identity (ADR 0025 s2) | A stable, unique per-title game identifier that is the same in catalogue, launch and callbacks | `CatalogueEntry.ProviderGameID` -> `casino_games (provider_id, provider_game_id)` UNIQUE, platform `id` minted once (`catalogue.go` `UpsertGame`); bounded by `providerref` 1..255 bytes | U-CAS-01 (id stability, whether the id is per operator or global) |
| Title attributes: name, type, volatility, features, assets, mobile, demo | A catalogue feed carrying them, or a documented manual export | `CatalogueEntry` (`types.go:557`): `GameType`, `RTPVariant`, `Volatility`, `FeatureFlags`, `SupportedAssets`, `MobileSupported`, `DemoSupported` | CAS-G06: **nothing calls `CasinoProvider.Catalogue()` outside tests** (grep of non-test code: only the sportsbook catalogue is fetched in `cmd/platform-api/main.go`). Catalogue reaches `casino_games` only through `PUT /v1/admin/casino/games` (`casino_admin_handlers.go`, platform-admin scoped, audited). A sync driver is NOT IMPLEMENTED |
| RTP variants (one title offered at several certified RTPs) | For each title: the certified RTP value(s) and which variant a given operator/launch uses | `Game.RTPVariant` is ONE nullable free-text column on a platform-wide row (no tenant or brand column; `casino_games` has no `tenant_id`) | CAS-G06 [casino, identity-compliance]: a variant is a distinct `provider_game_id` or it is not representable; no per-tenant/brand/jurisdiction RTP selection; no numeric RTP and no minimum-RTP rule per jurisdiction. U-CAS-02 (how the vendor exposes variants and whether the operator picks) |
| Jurisdiction blocklists | Per-title territory restrictions (licence-driven) so the platform can seed `jurisdiction_blocklist` | `Game.JurisdictionBlocklist` (platform-wide, set by admin upsert, NOT carried by `CatalogueEntry`); loosening a blocklist or re-enabling a disabled game requires a four-eyes `casino_catalogue_change_requests` (migration 0086 trigger, ADR 0081 s5; `catalogue_governance.go`) | Vendor territory data cannot be ingested by the interface; a human must map it. A future automated sync must NOT be able to remove a code: the trigger refuses without an approved request (good). U-CAS-03 (whether vendor restrictions are per title or per studio, and their format) |
| Per-tenant/brand availability | n/a (platform-side) | `casino_game_availability` (`SetGameAvailability`, `IsGameAvailable`; brand row overrides tenant row; missing row = not available, fail closed) | none |
| Platform kill switch for a title | n/a | `casino_games.status` active/disabled; a disabled game declines NEW bets and refuses launch; wins/rollbacks never blocked (CAS-GAME-KILL-BET-1, `orchestrator.go` `gameBlockReasonForNewBet`) | none |
| Demo mode | A demo/fun-play launch that never calls the wallet | `LaunchRequest.Mode` real/demo; demo sessions can never post a bet (`ErrLaunchSessionRequired`); the jurisdiction/market/RG checks run for demo too (K3-4) | CAS-G06: `Game.DemoSupported` is **informational only**; no non-test code refuses a demo launch for `demo_supported=false` (grep). U-CAS-04 (whether demo needs a vendor-side flag or a different URL form) |
| Bonus-eligibility / contribution metadata per title | Game category (slots/table/live) for wagering contribution | `Game.GameType` read for wagering contribution (`orchestrator.go` ~2204-2222, `wagering_contribution_integration_test.go`) | U-CAS-05 (whether the vendor game type taxonomy maps to the platform's contribution categories) |

## 2. Launch contract

| Platform requirement | Aggregator must supply | Interface (code) | Gap / UNVERIFIED |
|---|---|---|---|
| Launch only after market, game, RG, risk, jurisdiction, capability gates, outside any DB transaction for the vendor call | A launch endpoint that returns a launch URL for (game, opaque player, asset, mode, token, session) | `Orchestrator.LaunchGame` (`orchestrator.go:293`): phase A tx (all gates + session mint + audit), phase B vendor call with no tx (`ErrProviderCallRefused` under `txscope.Held`), phase C. `LaunchRequest{ProviderGameID, PlayerRef, AssetCode, Mode, LaunchToken, SessionID, Call}` (`types.go:580`) | none platform-side. U-CAS-06 (launch call shape, sync/async, idempotency of repeated launch) |
| Single-use, short-lived launch token embedded in the launch URL; never the player JWT | A way to carry our opaque token in the URL and present it back | `generateLaunchToken` 256-bit, stored hashed; `DefaultLaunchTokenTTL = 2 minutes` un-consumed (`launch.go:33`); `LaunchResult.LaunchURL` | U-CAS-07 (token length/charset limits in the vendor URL, TTL compatibility: 2 min un-consumed) |
| Vendor redeems the token ("bootstrap") before its first bet (CAS-BET-REQUIRES-BOOTSTRAP-1) | The ability to call `POST /v1/webhooks/casino/{tenantSlug}/{providerID}/launch-bootstrap` with a signed JSON body (`launch_token`, `request_id`, `provider_game_id`, `asset_code`, `mode`) and read `session_id`, `player_ref` | `BootstrapLaunch` (`bootstrap.go:368`), `docs/integrations/casino-launch-bootstrap.md`; idempotent per `(tenant, provider, request_id)`; reasons uniform 401 / constant 403 | **CAS-G01 [PW, casino+architect]**: the body is decoded against ONE fixed platform JSON shape with `DisallowUnknownFields`; the deferred adapter-side parse hook (doc "F-12") does not exist. A vendor that presents the token in its own wallet "authenticate" call, not ours, is unrepresentable. U-CAS-08 |
| Opaque player identity: vendor never sees the platform account id | Acceptance of an opaque UUID as the player identifier and echo of it on every bet/win | `casino_provider_player_refs` (migration 0111): random v4 per (tenant, provider, account), issued at session mint, UNIQUE both ways, append-only; `ResolveProviderPlayerRef` scoped to the route tenant/provider; a valid ref of another player is refused (`ErrPlayerRefMismatch`, SR3-1); body naming `player_account_id` is malformed | U-CAS-09 (vendor player-id charset/length: a UUID string must be accepted; whether the vendor can echo it on rollback; vendor-side display name requirement, which the platform must NOT satisfy with PII) |
| Session bound to (tenant, brand, player, wallet, asset, mode, game) | Echo of `session_id` on every bet | `casino_launch_sessions`; bet requires `consumed` real-mode session of THIS provider; asset must equal session asset; wallet/player always from the session, never the payload | U-CAS-10 (whether the vendor can echo a platform session id on bets; if it only carries its own session token, the adapter must map it, see CAS-G01) |
| Session lifetime bounds new exposure | n/a | `DefaultSessionMaxLifetime = 4h` from creation (config `CASINO_SESSION_MAX_LIFETIME`); past it a NEW bet is declined `session_expired`, win/rollback unaffected (`market_exposure.go`) | U-CAS-11 (vendor games that outlive 4 h: the player must relaunch) |
| Vendor-side game session identifier for support/reconciliation | Its own session/round identifiers | `casino_provider_rounds` (`BindProviderRound`, `rounds.go`): a provider round id binds to one (session, player, brand); a round id seen for another player/brand is `ErrProviderRoundOwnershipConflict` (409, integrity alert) | U-CAS-12 (round id uniqueness scope: per operator, global, per game) |

## 3. Wallet callbacks

All callbacks enter at `POST /v1/webhooks/casino/{tenantSlug}/{providerID}` (`casino_routes.go:68`) and are translated by the
adapter's `HandleCallback` into `CallbackEvent{EventType bet|win|rollback, ProviderTxID, OriginalProviderTxID, RoundID,
ProviderGameID, Amount int64 minor units, AssetCode, Outcome, PlayerRef, SessionID}` (`types.go:692`). Amount and asset come
from the event only; wallet/player come from the platform's own records.

| Callback | Aggregator must supply | Platform handling (code) | Gap / UNVERIFIED |
|---|---|---|---|
| Balance query | (a) outbound: n/a; (b) inbound: if the vendor calls the operator for a balance, a route to call | `CasinoProvider.Balance(ctx, BalanceRequest)` exists on the interface (outbound shape, platform asks); **there is no inbound balance route and no balance event type**; the only mock implementation is `mock.go:287`. Redis never holds a balance (CLAUDE.md); a balance reply would need an authoritative DB read | **CAS-G02 [PW, casino+ledger-finance]**: a wallet-model aggregator that calls "get balance" at game start and after every spin has no endpoint here. U-CAS-13 |
| Wager / bet (debit) | Unique transaction id per bet, round id, amount, asset, session, player ref | `postBet` (`orchestrator.go:1561`): L0.1 delivery lock, player-ref resolve, replay/payload-mismatch check, tombstone check E3, tenant gate, session checks, game kill, brand gate, market gate, capability gate, RG, Risk, KYC (with AML hold), ledger lock + balance check inside the tx, Post `casino_bet` Dr `player_cash` Cr `house_gaming` (or locked/bonus split), BindProviderRound, audit | Response carries no balance (CAS-G01). Insufficient funds / RG / risk / KYC / market / game / session denials are `outcome=declined` + `decline_reason`, HTTP 200 (a vendor must treat that as "stake not taken"). U-CAS-14 (how the vendor expects a decline: a business error code, a specific HTTP status, a retryable vs terminal distinction) |
| Win (credit) | Unique transaction id, round id, payout amount (total payout, not net), asset, reference to the bet | `postWin`: credits the wallet the round's OWN bet debited (never the payload player), amount = payout, `verifyPostedWinMatchesEvent` on redelivery, integrity alert if the round has no live bet (`ErrBetNotFound`, 400) | U-CAS-15 (is the win amount gross or net of stake; zero-win rounds: does the vendor send a win of 0? `validateCallbackEvent` requires `Amount > 0` and a non-nil `player_ref`, so a 0-amount win is `ErrInvalidInput` -> 400, `orchestrator.go:1406`; the adapter must ack-and-drop zero wins, CAS-G02) |
| Rollback / refund / cancel of a bet or win | A rollback transaction id of its own and the original transaction id | `postRollback` (`orchestrator.go:2460`): exact inverse entries of whichever flow posted, one reversal per original, `FOR UPDATE` on the original; a second distinct rollback reference on an already reversed original is `ErrAlreadyRolledBack` (409) | U-CAS-16 (does the vendor distinguish rollback of a bet from rollback of a win; does its rollback carry the player/amount) |
| Round close / round end | Optionally a close event | **No event type**: an unknown `event_type` is `ErrCallbackMalformedBody` (400) after verification (`orchestrator.go:1281` default branch). A round has no closed state; the ledger does not need one for cash rounds; locked-stake rounds release on win/rollback | CAS-G02: an aggregator that sends a mandatory "end round" call would receive a 400 forever; the adapter must acknowledge-and-drop it (it is not a money event) without reaching `ReceiveVerifiedCallback`, which requires a documented adapter hook (CAS-G01). U-CAS-17 |
| Bet+win in one call, "play" calls, tip/adjustment, tournament prize | Possibly one transaction carrying both stake and payout | Not representable as one event: the adapter would have to split into two `CallbackEvent`s with two distinct `provider_tx_id`s, but `HandleCallback` returns ONE event | **CAS-G02 [PW]**: single-event return limits it. U-CAS-18 |
| Free rounds / free spins / jackpot / promo payout with no platform bet | Event types and a way to tell them from normal wins | **No design.** ADR 0025 Stage 10.3 amendment item 8: "a real adapter must not map a free-round or jackpot payout that has no platform bet to a win, until a design exists"; the conformance case is `NOT IMPLEMENTED`; a win with no bet is an integrity alert (400) | **CAS-G03 [PW, casino+ledger-finance+bonus-engine]**. U-CAS-19 (do the chosen titles use them; can they be disabled per operator) |
| Multiple bets in one round (side bets, re-bets) | A per-bet reference on the win | `CallbackEvent` has no `OriginatingProviderTxID`; cash multi-bet rounds resolve to the shared wallet (G-1, `classifyDirectOriginRows`), bonus/mixed multi-bet is `ErrAmbiguousMultiOriginRound` -> 409 manual reconciliation | **CAS-G04 [PW]**. U-CAS-20 |
| Amount representation | Minor units or a decimal that converts exactly | `Amount int64` minor units, asset exponent from the Asset registry (`internal/assetregistry`); a fractional minor unit is not representable | Adapter rule: convert with exact arithmetic and refuse (do not round) any amount not exactly representable. CAS-G10 (int64 cannot carry 18-decimal crypto; out of scope under the fiat-first decision). U-CAS-21 (vendor decimals, currency list, unsupported currencies) |
| Transaction id uniqueness across event types | `provider_tx_id` distinct for bet, win, rollback of the same round | Idempotency key `providerID:provider_tx_id` and DB unique `(tenant_id, provider_id, provider_tx_id)`; bet and win with the same id would collide (`ErrProviderTxPayloadMismatch`) | Adapter rule: if the vendor reuses one id across event types, the adapter must derive a deterministic distinct id, avoiding the reserved ledger prefixes (migration 0115 reserved-prefix guards exist on payment tables; the ledger prefix guard noted in `docs/plans/prh2-hardening-round/reviews/k3-design-security.md` O-1). U-CAS-22 |
| Provider reference bounds | Ids 1..255 bytes, UTF-8, no control characters | `providerref.ValidateAll` before any domain statement (`validateCallbackReferences`); a violation is `ErrProviderReferenceInvalid`, nothing recorded | U-CAS-23 (vendor ids longer than 255 bytes) |

## 4. Authentication, signatures, webhookauth

| Platform requirement | Aggregator must supply | Interface (code) | Gap / UNVERIFIED |
|---|---|---|---|
| Verify-first: signature over the RAW bytes before parsing, constant-time, tenant-bound, run by the Orchestrator not the adapter | A request authentication scheme the platform can express as a `webhookauth.VerificationScheme` | `CasinoProvider.WebhookScheme()`, `Orchestrator.VerifyCallback` (`webhook_verify.go:83`), scheme registry `conformanceManifest` empty today (`webhookauth/scheme.go:341`) | Same three platform gaps as the PSP package apply unchanged to casino: **PSP G-01** (`Extract` is header-only, body-free: a signature in the body or query is unregistrable), **PSP G-02** (a non-synthetic scheme must sign a timestamp; a plain checksum cannot register), **PSP G-08** (one secret + one `vendor_account_id`). See `../README.md` X-G03. U-CAS-24 (scheme: header/body/query, MAC vs asymmetric, timestamp, key id, per-operator keys) |
| Tenant binding by route and per-tenant key, never by payload | One callback URL and one key per operator account per tenant | Route `/{tenantSlug}/{providerID}`; credential `providercred` domain `casino`, purposes `webhook_verify` and `outbound_api` (ADR 0093) | U-CAS-25 (can the vendor configure a distinct callback URL and secret per operator account; one aggregator account per tenant is required, a shared account cannot bind a tenant) |
| Redelivery semantics declared | Documented retry policy for non-2xx | `webhookauth.WebhookRetrySemantics` (`Retries429`, `Retries503`, `HonorsRetryAfter`, `RetryWindow`); `MustRequireRetrySemantics("casino", ...)` panics for a non-MOCK adapter that declares none (ADR 0097) | U-CAS-26 (does the vendor retry a 4xx, a 409, a 5xx; for how long; does it stop on 200 with `declined`) |
| Admission control and rate limits | Source IP ranges if published | ADR 0097 admission domain; edge allow-list is an environment input | U-CAS-27 (published IPs/ranges) |
| Outbound launch authentication | Outbound API credential for `Launch`/`Catalogue` | `OutboundCredentialResolver` per call, `CallContext` redacted everywhere (`launch.go:66-81`); `casino.NewOutboundKindSplitResolver` serves the MOCK only to a Synthetic adapter; `PROV-OUTBOUND-CRED-1` tripwire `TestOutboundPrecondition_EveryWiredAdapterIsSynthetic` must be relaxed deliberately for one adapter | U-CAS-28 (outbound auth scheme, token lifetime) |
| Suspended/closed tenant | n/a | Pre-verification uniform 401 (`webhook_preamble.go`); open rounds cannot settle while suspended; an unseen-original rollback delivered then leaves no tombstone (HD-10.3-4, unchanged, ADR 0025 amendment) | Disclosed consequence; the vendor must redeliver after reactivation. U-CAS-26 covers redelivery |

## 5. Idempotency, tombstones, out-of-order

| Platform requirement | Aggregator must supply | Interface (code) | Gap / UNVERIFIED |
|---|---|---|---|
| Every financial write idempotent via `(provider_id, provider_tx_id)` enforced by the database | A stable transaction id that is identical on every redelivery of the same event | Unique index `(tenant_id, provider_id, provider_tx_id)` on `ledger_transactions` + `ledger.Post` key `providerID:provider_tx_id`; plus a per-reference advisory lock (L0.1, `acquireProviderTxDeliveryLock`) so concurrent deliveries serialize; redelivery returns the original result with `Replayed=true`; a differing payload is `ErrProviderTxPayloadMismatch` (409, integrity alert) | U-CAS-29 (does the vendor ever resend with a changed amount/round/currency for the same id) |
| Declined bet redelivery | Same id on redelivery | A DECLINED bet (RG, insufficient funds, market, game, session) posts no row, so a redelivery is re-evaluated fresh against current state and can succeed later (`findPostedBetTransaction` doc, `orchestrator.go:1431`); only a SUCCEEDED post is replayed verbatim | U-CAS-14, U-CAS-29: the vendor must not assume a declined id stays declined |
| Win redelivery (CAS-WIN-IDEMP-1) | Same | `postWin` finds the posted win first, verifies payout/asset/round, returns the original (also after rollback of its bet or the win); a FIRST delivery of a win whose bet was rolled back is refused | none |
| Rollback for a transaction never seen writes a tombstone; a late original is rejected | The ability to send a rollback before the original, and to redeliver the original later | `postRollbackTombstone` (`ledger.TxTombstone`); late bet (E3) -> `declined`, `decline_reason=original_rolled_back`, audit `casino_bet.rejected_tombstoned`; late win (E10) -> `ErrOriginalTombstoned` 409 + `casino_callback_rejections` row; a second distinct rollback of a tombstoned original is acknowledged (E9); tombstone `CorrelationID` deterministic | U-CAS-30 (does the vendor ever roll back an unseen original; does it reuse the original id on rollback) |
| Out-of-order bet/win | Win after bet | A win before its bet is `ErrBetNotFound` 400 integrity alert (the platform never accepts a win with no live bet) | If a vendor can deliver a win before its bet (parallel sends), the win is refused terminally while the vendor may stop retrying 4xx: U-CAS-26, U-CAS-31 |
| Verified rejections recorded durably | n/a | `casino_callback_rejections` (migration 0097, verified-only, append-only), `RecordCallbackRejection` in a separately committed transaction; classes: `original_tombstoned`, `ambiguous_round`, `wallet_collision`, `mixed_funding`, `lock_already_released`, `bonus_bet_not_locked`, `rollback_of_tombstoned_original` | none |

## 6. Bonus-funded play and free rounds

| Platform requirement | Aggregator must supply | Interface (code) | Gap / UNVERIFIED |
|---|---|---|---|
| Platform bonus engine owns bonus value; a bonus-funded stake locks, never immediately absorbs (doc 08 s16.10.1) | Nothing vendor-specific: stakes arrive as ordinary bets; the platform decides funding from its own wallets | `postBet` funds from `player_cash` first / locked splits per the bonus engine; `bonus_settlement.go` resolves the win destination from the ledger (`resolveWinOrigin`), hold-capture into `player_bonus_held`, held-win rollback (`postRollbackHeldWin`), `RecheckGrantExposure`; wagering contribution recorded after a cash-funded bet (`HasActiveWageringGrant`) | The package comment in `types.go:13` still says bonus splits are "explicitly NOT implemented"; the code (`bonus_settlement.go`) implements them. Documentation drift, no behaviour issue; fix in a docs pass |
| Multi-bet rounds under bonus | Per-bet win reference | `ErrAmbiguousMultiOriginRound` -> manual reconciliation; doc 08 s16.13: "multi-bet-capable titles should not be marked launch-eligible for bonus-funded wagering" (flagged for product-owner-proxy/architect, not decided) | CAS-G04: the catalogue has no flag for "multi-bet per round"; nothing enforces it. U-CAS-20 |
| Vendor-side free rounds / bonus feature (provider's own bonus engine, ADR 0033) | The vendor's bonus-engine events | Not integrated. ADR 0033 covers `ExternalRewardProvider` for sportsbook-style external grants; casino free-round normalization is the "long-standing OPEN DECISION" of doc 08 s1 | CAS-G03 |
| LF-10 (rollback of an already-resolved held disposition) | n/a | `ErrHeldDispositionRollbackUnsupported`: fails closed, posts nothing, owner of the decision is ledger-finance (open) | Open ledger-finance decision; affects bonus-funded play only |

## 7. Gates on launch and bet (RG / market / KYC / AML / jurisdiction)

The vendor sees only a decline. The platform enforces; the vendor is never trusted to enforce (CLAUDE.md "enforcement is our code").

| Gate | Launch (`LaunchGame`, `orchestrator.go:293`) | NEW bet (`postBet`) | Settlement (win/rollback) | Gap |
|---|---|---|---|---|
| Operating-market (A8) | `MarketGuard` with request country evidence (`marketgate`); unknown evidence refuses | Brand-level "any country permits casino wagering" check (`brandMarketOpen`) + 4 h session max lifetime; a vendor callback has NO player country evidence | never blocked | Per-bet country evidence needs a vendor geolocation signal (PROVIDER DEPENDENT residual, `market_exposure.go` header, `docs/plans/a8-market-blocking-record.md`). Owner decision 2026-10-16: market evidence = KYC-verified residence now, vendor geolocation later. See `../kyc-aml/` |
| Game jurisdiction blocklist | `jurisdiction.Resolve` once; armed only when the game's blocklist is non-empty; unresolved player jurisdiction while armed = refuse (`jurisdiction_unresolved`, distinguishable internally, one player-facing message) | **Not re-run per bet** (pre-existing R-4, `CAS-BET-JURISDICTION-RECHECK-1`); the session carries the launch-time jurisdiction snapshot | never | CAS-G05 [security, identity-compliance, casino] |
| Responsible gaming (ADR 0115) | `rg.EvaluateAction` (real) / `EvaluateEligibility` (demo) | `rg.EvaluateAction` on every new bet (self-exclusion, time-out, limits where enabled) | never | RG enforcement is DEFAULT OFF behind a per-brand switch pending compliance floor values (HDR-A6-1, OQ-2 still open). Not a vendor matter |
| Risk (ADR 0031) | real mode only; DENY/REVIEW both refuse | every new bet | never | none |
| KYC play trigger + AML hold | none at launch | `kyc.EvaluateEnforcement(EnforcementCasinoPlay)` composed with the AML hold read (`aml_hold_active`): a held player gets the SAME public decline as KYC pending (`account_review`, ADR 0116 s5.1) | never | Tenant-visible code is identical for KYC pending and AML hold by design (no tipping-off) |
| Tenant/brand status | tenant gate in the route preamble | `requireActiveTenantForNewPosting`, `requireActiveBrandForNewStake` (ADR 0112) | new postings for a non-active tenant refused (`ErrTenantNotActive`) except replays; brand not gated for settlement | none |
| Capability / kill switch | `supports_launch`, `supports_bet` for real mode, asset in `supported_assets` | capability re-read for the session's brand (CAS-CAP-ROLLBACK-1); disabled = retryable 503 on NEW bets only | never gated | Emergency stop is credential revocation and it strands open exposure (R9/C14); runbook requires a `casino_consistency` run before a replacement key is activated |
| Game kill | refuse | decline `game_unavailable` (audited) | never | none |

## 8. Reconciliation and the statement source

| Platform requirement | Aggregator must supply | Interface (code) | Gap / UNVERIFIED |
|---|---|---|---|
| Daily reconciliation of the ledger against the aggregator's own record | A statement/report: per transaction (id, kind bet/win/rollback, original id, round, asset, amount) and optionally per-asset GGR totals, with a statement coverage horizon | `statement.CasinoStatementFetcher.Fetch(ctx, CasinoFetchRequest{TenantID, PeriodStart, PeriodEnd}) (CasinoStatement{CoverageEnd, Lines, Totals})` (`internal/reconciliation/statement/statement.go:157`); fetched OUTSIDE any tx (`FetchCasinoStatement` refuses under `txscope.Held`), matched inside a REPEATABLE READ tx (`casino_statement.go`); `CoverageEnd` REQUIRED and validated for a provider source (not older than `CasinoCoverageMaxLag = 1h` before period end, not > 5 min in the future, monotonic against the previous run, SR3-3) | U-CAS-32 (report availability: API vs file; latency; cut-off semantics; time zone; whether tombstoned rollbacks appear; whether totals/GGR are given; currency of totals) |
| Counterparty match is zero-tolerance and never auto-corrects | n/a | `casino_statement` stream: key match on `(provider_id, provider_tx_id)` with kind/amount/asset/round/original; totals match on net `house_gaming`; a statement rollback whose original the ledger holds only as a tombstone is a MATCH; every mismatch is a P1, never fixed by posting | The mismatch kind is named `cas_mock_statement_mismatch` ("the first real source adds its own kind", migration 0098 CHECK): a real source needs a migration. CAS-G07 |
| Internal consistency checks C1..C7 | n/a | `casino_consistency` (round binding, posting shape, orphan win, rollback linkage, tombstone conflict, unposted provider event, evidence metrics) | none; runs against the MOCK today and is vendor independent |
| Source registration | A read-only statement credential | `reconciliation.RunSweep*(.., casSource, ..)` takes ONE `CasinoStatementSource` for all tenants and providers (`scheduler.go:281-295`); the production registration list only guards that the source is not Synthetic (`registrations.go:401`). The payments side has a per-(tenant, provider) registry with `ReadOnlyAttested` + `MarkProductionEligible` + coverage refusal (`cmd/platform-api/payment_statement_registry.go`); casino has **none** of these | **CAS-G07 [PW, ledger-finance+security+architect]**: no per-(tenant, provider) source resolution, no read-only attestation, no eligibility marker, a configured provider with no source is not refused. Also PSP G-04 (no `statement_read` credential purpose; `providercred` purposes are `webhook_verify`, `outbound_api` only) |
| Stored copy of the fetched statement (append-only) | n/a | `CAS-STMT-STORE-1` / A3 residual R-5: **NOT IMPLEMENTED** (needs a real source format + migration) | CAS-G07 |
| Period semantics and timing window | Settlement cut-off definition | MOCK is all-time; a real period-bounded source needs a timing window (a key on one side only is a mismatch only if also absent from the adjacent period) - "never an amount tolerance"; exact cut-off PROVIDER DEPENDENT, NOT IMPLEMENTED (`casino_statement.go` header) | CAS-G07. U-CAS-32 |
| Unpaired tombstone / duplicate statement rollback flags | n/a | Deliberately NOT flagged today; "to be decided with the first real source" | Decision for ledger-finance at G5 |
| Scale | n/a | CAS-RECON-SCALE-1 (NOT IMPLEMENTED, Medium, design+ADR): each run recomputes the whole casino population of a tenant; `RunSweepTenants` exists for tests only | CAS-G07: must be sized before real multi-tenant volume |
| Revocation runbook | n/a | After a compromise: mandatory `casino_consistency` run over the window before a replacement key is activated; once a real source exists, a `casino_statement` run too (ADR 0025 amendment) | none |

## 9. Failure, retry, timeout, kill-switch semantics

| Platform requirement | Aggregator must supply | Interface (code) | Gap / UNVERIFIED |
|---|---|---|---|
| No vendor call while a DB transaction is held (INV-IO-1) | n/a | Launch phase B and statement fetch run with no tx; `ErrProviderCallRefused`; static guard `internal/txscope` flags `Fetch`/`FetchCasinoStatement`/`Launch` names | none |
| Ambiguous outcome never treated as success or failure | A distinguishable outcome for unknown results | `Outcome` succeeded/declined/ambiguous on the adapter's synchronous methods; callback events with `outcome != succeeded` are rejected (`ErrOutcomeNotSucceeded`) | U-CAS-33 |
| Unfinished rounds | Round expiry / auto-rollback policy | **No sweep located.** A grep of non-test code finds no settlement-timeout sweep (comments in `types.go:377`, `bonus_settlement.go:148` mention one hypothetically). A bonus-locked stake stays locked until win/rollback; a cash stake is already in `house_gaming` | CAS-G08 (new): who closes a round the vendor never settles (vendor timeout policy) is open. Reviewer ledger-finance+casino. U-CAS-34 |
| Provider health / circuit | n/a | `HealthStatus`, `ProviderHealth`; routing by `ListRoutingCandidates` priority (`capability.go`) | Routing is by game's `provider_id`; a second aggregator for the same title is not a failover path |

## 10. Credentials and environments

| Platform requirement | Interface (code) | Gap / UNVERIFIED |
|---|---|---|
| Per-tenant credentials as handles (`secret_ref`), never in the DB or source | `providercred` domain `casino` (migration 0096 CHECK allows `payments`,`kyc`,`casino`); purposes `webhook_verify`, `outbound_api`; four-eyes activation; revocation immediate per request (ADR 0093 s4) | PSP G-08 applies (one secret + one `vendor_account_id`; non-secret operator/site ids need a per-tenant config home). Studios needing a second credential (e.g. separate catalogue/report keys) have no purpose (PSP G-04) |
| Sandbox vs production switching per tenant | Environment `APP_ENV` + synthetic guard (ADR 0085/MOCK-ADAPTER-PROD-1): production refuses any Synthetic component; real adapters implement `providerkind.ProductionEligible` | The bundle holds a concretely typed `*casino.MockCasinoProvider` (`cmd/platform-api/registrations.go:40`); wiring a real adapter is a code change in `registrations.go`, `casinoAdapters()`, the resolver split and the tripwire test. Expected adapter-build work (gate C2), not a design gap |

## 11. Platform-gap candidates (for `architect`, `security`, `ledger-finance`; no core change is made here)

Each: scope, what breaks, reviewer, blocks the first real adapter?, minimal proposal (only if confirmed necessary).
Shared with the PSP package and not repeated: PSP G-01 (header-only `Extract`), G-02 (mandatory signed timestamp), G-04
(`statement_read` credential purpose), G-08 (one secret + one account id; non-secret per-tenant config). They apply to
casino identically.

| Id | Gap | Scope | Reviewer | Blocks first real adapter? | Minimal proposal (only if confirmed necessary) |
|---|---|---|---|---|---|
| CAS-G01 | Vendor-facing wire contract is platform-defined: callback response body (`outcome`, `tombstoned`, `ledger_transaction_id`, `decline_reason`) and status mapping (400/401/409/503) live in `casino_handlers.go`; bootstrap body parsed against one fixed shape; no response or ack-only translation hook | PW (any wallet-callback aggregator) | casino, architect, security | Potentially, probably yes (depends on U-CAS-08/14/17) | Additive adapter-side hooks `ParseBootstrap`, `FormatCallbackResponse(result, err) (status, body)`, `AcknowledgeNonMoney(event)` with a closed rule set: the handler still decides the outcome; the hook only encodes it. No change to `postBet/postWin/postRollback`. Security reviews that a vendor-shaped body can never carry detail the generic body withholds |
| CAS-G02 | Only bet/win/rollback exist: no inbound balance route, no round-close, no combined bet+win, no adjustment/tip/prize, single event per `HandleCallback` | PW | casino, ledger-finance | Depends on U-CAS-13/17/18 | Balance: a signed read-only route that reads inside a tx (never cache). Round-close: ack-and-drop at the adapter hook. Combined: let `HandleCallback` return a slice (additive). Each new money-moving type needs ledger-finance design first |
| CAS-G03 | Free rounds / jackpot / promo payouts with no platform bet: requirement exists (ADR 0025 amendment 8), conformance case and design do not | PW | casino, ledger-finance, bonus-engine, architect | Only if the chosen titles emit them and are enabled | Do not map them. Disable such titles (use `feature_flags` + a launch refusal rule, which does not exist yet) until an ADR defines the accounting (promo expense vs bonus) |
| CAS-G04 | No `OriginatingProviderTxID` on win/rollback; no catalogue flag for "multi-bet per round" and no refusal for bonus-funded play on such titles | PW | casino, ledger-finance, bonus-engine | No for cash-only play; yes for bonus-funded play on multi-bet titles | Catalogue flag + launch/bet refusal of bonus funding for flagged titles; add the optional field only when a real vendor proves it sends per-bet win references |
| CAS-G05 | Game jurisdiction blocklist and market evidence are not re-run per bet; mid-session travel inside an open market is undetected (R-4 `CAS-BET-JURISDICTION-RECHECK-1`, SR8-2 residual) | PW | security, identity-compliance, casino | No (compliance decision) | Bound by the 4 h session lifetime today; shorten per jurisdiction by configuration or add a per-bet vendor signal later. Owner/compliance decision |
| CAS-G06 | Catalogue: no sync driver; RTP is one free-text field per platform-wide game row; no per-tenant/jurisdiction RTP; `demo_supported` and `feature_flags` not enforced | PW | casino, identity-compliance | No (manual admin load works) | A catalogue sync driver under a platform-admin context that can only add/disable, never loosen (the dual-control trigger already enforces this); a launch refusal for demo on `demo_supported=false`. Regulatory RTP rules are a compliance input |
| CAS-G07 | Casino statement source: one global source object; no per-(tenant, provider) registry; no read-only attestation or production-eligibility marker; no stored append-only copy; no real mismatch kind; period/timing window NOT IMPLEMENTED; matcher O(history) (CAS-RECON-SCALE-1) | PW | ledger-finance, security, architect | **Yes for real money**; not for a no-money sandbox | Mirror the payments registry (`registerPaymentStatementSources`) for casino; migration for a stored statement + real mismatch kind; windowed matcher; scale design ADR. All with the first real source |
| CAS-G08 | No round-timeout / stale-round policy located in code; vendor behaviour for abandoned rounds unknown | PW | ledger-finance, casino | Depends on U-CAS-34 | Decide with the vendor's documented policy; if a sweep is needed it is a ledger-finance-owned posting path |
| CAS-G09 | Wire gap documentation drift: `types.go` package comment says bonus splits are not implemented; `docs/integrations/casino-launch-bootstrap.md` is the MOCK wire shape, not a vendor contract | docs | casino | No | Doc fix in the adapter PR |
| CAS-G10 | `CallbackEvent.Amount` is `int64`; crypto assets (NUMERIC(38,0)) not representable on the casino path | PW (deferred) | ledger-finance, casino | No (fiat-first, owner decision 2026-10-15) | Defer; record when crypto casino is scheduled |

## 12. UNVERIFIED registry (needs provider documentation/sandbox)

34 entries. Closure is by citation (URL + retrieval date), vendor written answer or sandbox capture.

| Id | Question |
|---|---|
| U-CAS-01 | Game id stability and scope (per operator vs global; changes on re-release) |
| U-CAS-02 | RTP variant exposure and operator selection |
| U-CAS-03 | Territory restriction data: granularity and format |
| U-CAS-04 | Demo launch form (flag vs separate URL; whether demo ever touches the wallet) |
| U-CAS-05 | Game category taxonomy vs platform contribution categories |
| U-CAS-06 | Launch call shape, sync/async, repeated-launch idempotency |
| U-CAS-07 | Token embedding limits and TTL compatibility (2 min un-consumed) |
| U-CAS-08 | How the vendor redeems our token (our bootstrap shape vs its own authenticate call) |
| U-CAS-09 | Player identifier format accepted (UUID string), display-name requirements |
| U-CAS-10 | Whether a platform session id can be echoed on bets |
| U-CAS-11 | Behaviour when a game outlives the 4 h session lifetime |
| U-CAS-12 | Round id uniqueness scope |
| U-CAS-13 | Balance flow: inbound balance calls and when |
| U-CAS-14 | Expected decline format (business code vs HTTP status; retry vs terminal) |
| U-CAS-15 | Win amount gross/net; zero-win rounds |
| U-CAS-16 | Rollback granularity and payload |
| U-CAS-17 | Round-close/end event and required acknowledgement |
| U-CAS-18 | Combined bet+win or multi-transaction requests |
| U-CAS-19 | Free rounds / jackpot / promo events and whether they can be disabled |
| U-CAS-20 | Multi-bet rounds and per-bet win references |
| U-CAS-21 | Amount decimals, supported currencies |
| U-CAS-22 | Transaction id uniqueness across event types |
| U-CAS-23 | Id length limits (platform bound 255 bytes) |
| U-CAS-24 | Inbound authentication scheme (location, algorithm, timestamp, key id) |
| U-CAS-25 | Per-operator callback URL and secret configurability |
| U-CAS-26 | Retry policy for 4xx/409/5xx, window, Retry-After |
| U-CAS-27 | Published source IP ranges |
| U-CAS-28 | Outbound authentication scheme and token lifetime |
| U-CAS-29 | Whether the same id is ever resent with a changed payload |
| U-CAS-30 | Rollback of unseen originals and id reuse on rollback |
| U-CAS-31 | Whether win can precede bet (parallel sends) |
| U-CAS-32 | Statement/report: format, latency, cut-off, time zone, tombstone representation, totals |
| U-CAS-33 | Ambiguous/timeout outcome signalling |
| U-CAS-34 | Abandoned-round policy and timeouts |

## 13. What must be true before any of this is real

See `integration-gates.md`. In short: documentation review (C1), adapter against recorded fixtures (C2), conformance (C3),
`security` and `ledger-finance` review (C4, C5), sandbox on a synthetic tenant (C6), operational acceptance (C7),
pre-production readiness (C8), explicit production authorization (C9). No real money until C9, in writing, from the owner.
