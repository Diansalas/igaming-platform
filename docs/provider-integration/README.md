# Provider-integration packages: master index

Status: **DOCUMENT / SPECIFICATION PACKAGES ONLY.** No real adapter, no provider call, no credential, no account, no
agreement exists or is requested by anything in this directory tree. Prepared 2026-10-16 (PSP package by `payments`;
casino, KYC/AML, sportsbook and sports-data packages by `integrations`) on base `claude/focused-wright-jw88w9`.

Labels (CLAUDE.md): every package is `NOT IMPLEMENTED` as software. No provider is selected for any area, and no provider
documentation is available (owner statement 2026-10-16). The only provider-named material is the PSP package's Nuvei
*candidate* evaluation, which is itself `BLOCKED` on documentation access. Every vendor-side statement in these packages
is a requirement on the vendor or is `UNVERIFIED`. The platform side is verified against code and cited by path.

## Provider order

The owner's objective of 2026-10-16: staging validation, then "TECHNICAL PLATFORM READY FOR PROVIDER INTEGRATION ->
PSP -> casino -> KYC/AML -> sportsbook -> sports data -> provider conformance -> manual brand activation -> production".

| # | Area | Package | Interfaces it maps onto | Scheduling | UNVERIFIED items | Platform-gap candidates |
|---|---|---|---|---|---|---|
| 1 | Fiat PSP | [`psp/`](psp/README.md) | `payments.PaymentProvider`, `statement.PaymentStatementSource`, `internal/payoutinstrument` | first priority | 34 (Nuvei candidate, N-U01..N-U34) | G-01..G-11 |
| 2 | Casino aggregator | [`casino/`](casino/README.md) | `casino.CasinoProvider`, `statement.CasinoStatementFetcher` | first-launch priority (owner decision 1 of 2026-10-15) | 34 (U-CAS-01..34) | CAS-G01..G10 |
| 3 | KYC / AML (documents, screening, geolocation if selected) | [`kyc-aml/`](kyc-aml/README.md) | `kyc.KYCProvider`, `aml.ScreeningProvider`, `geolocation.LocationProvider` / `Locator` | after casino | 23 (U-KYC-01..23) | KYC-G01..G10 |
| 4 | Sportsbook | [`sportsbook/`](sportsbook/README.md) | `sportsbook.Provider` (catalogue only), settlement entry points, `statement.SportsbookSettlementSource` | **DEFERRED** until a sportsbook brand is scheduled (decision 2 of 2026-10-15) | 13 (U-SB-01..13) | SB-G01..G10 |
| 5 | Sports data | [`sports-data/`](sports-data/README.md) | none today (ADR 0117 Part C is design only) | **DEFERRED with sportsbook** | 12 (U-SD-01..12) | SD-G01..G08 |

Each package has the same five files as the PSP package: `README.md`, `provider-neutral-mapping.md`,
`conformance-plan.md`, `integration-gates.md`, `open-questions.md` (the PSP package additionally has
`nuvei-evaluation.md`, a candidate evaluation; the other areas have none because no candidate exists).

## Common rules (all areas)

1. **No real money, no real customer data, no production credential and no production traffic until the owner gives
   explicit written production authorization** for that provider (gates G9 / C9 / K9 / S9 / D9). A sandbox authorization
   (G6 / C6 / K6 / S6 / D6) is a separate, narrower owner decision and is not a production authorization.
2. **Legal/licensing approval and brand activation are manual per-brand owner decisions** (decision register 2026-10-16;
   CLAUDE.md "Compliance"). Passing a technical gate says nothing about a gambling or betting licence, a game's or market's
   legality in a territory, a regulator's acceptance of a KYC/screening/geolocation method, a data licence or a vendor
   agreement. Software capability is never described as approval.
3. **No agent signs, accepts an agreement, creates an account or supplies a credential.** The owner contacts vendors and
   provisions credentials through the approved secret store, never in chat, source, docs or logs. Environment safety rules in
   CLAUDE.md apply (no production credentials, no real customer data, no private keys).
4. **Do not invent provider behaviour.** Vendor statements stay `UNVERIFIED` until closed by citation (URL + retrieval date),
   a vendor written answer or a sandbox capture. A conformance test must cite its source and must not encode an assumption.
5. **Providers adapt to the neutral interfaces; the core is not redesigned around the first provider.** A real provider
   that violates a platform assumption is recorded as a numbered platform-gap candidate (document -> provider-specific or
   platform-wide -> which specialist reviews -> change core only if necessary). Cross-cutting changes go through `architect`
   and are recorded in `docs/decisions/` (CLAUDE.md "Specialist agents").
6. **Enforcement is platform code and fails closed.** Unknown, unavailable, error, timeout, stale or unauthenticated is never
   a pass (KYC `unavailable`, screening `error`, geolocation non-`resolved`, stale feed, unverified callback).
7. **Gate sequence per provider** (names differ by area, shape is the same): provider documentation review -> adapter
   (recorded fixtures only) -> conformance -> `security` review -> `ledger-finance` review (where money posts) -> sandbox ->
   operational acceptance -> pre-production readiness -> explicit production authorization. A later gate is not entered
   while an earlier one is open; the orchestrator is the single writer of status documents.
8. **Reuse the shared scaffolding, do not reinvent it:** `internal/providers/httpclient` (explicit timeouts, bounded retry
   only for requests the caller marks idempotent, no redirects, four error classes, one span per call),
   `internal/providers/config`, `internal/webhookauth` (`VerificationScheme`, SC1..SC13 conformance, retry semantics,
   `conformanceManifest`), `providercred` (per-tenant handles, per-call outbound credential), `internal/providerref`
   (reference bounds), `internal/providerkind` (Synthetic / ProductionEligible), `internal/txscope` (no provider I/O while a
   transaction is held). A new adapter that reimplements any of these is rejected at review.
9. **Test fixtures are synthetic.** No real PAN, identity document, screening subject, player data, or licensed official
   data in the repository.

## Platform-gap candidates across ALL areas

`Blocks first real adapter` means: the gap must be decided or closed before the first real adapter of that area can be
built or enabled in a sandbox, as judged from the code and documents; `Potentially` means it depends on a still-`UNVERIFIED`
vendor property; `Real money` means it blocks production enablement but not a no-money sandbox. Owner specialist = who
reviews and, if a change is confirmed necessary, owns it (cross-cutting changes through `architect`).
Detail, scope and the minimal proposal for each are in the cited file; none is a decision and no core change is made.

### Cross-cutting (apply to more than one area)

| Id | Gap | Areas | Owner specialist | Blocks first real adapter? | Where detailed |
|---|---|---|---|---|---|
| X-G01 | `providercred` domains are `payments`, `kyc`, `casino` only (migration 0096 CHECK; `providercred.Domains`); no credential domain and no `CallContext` for AML screening, geolocation, sportsbook, sports data. Feeds are market-wide and want a platform-scope credential (ADR 0117) | KYC/AML (screening, geolocation), sportsbook, sports data | `architect`, `security` | Yes for screening, geolocation, sportsbook, sports data; no for PSP/casino/KYC document vendor | `kyc-aml` KYC-G06, `sportsbook` SB-G10, `sports-data` SD-G03 |
| X-G02 | Provider bundle is one concretely-typed mock per domain (`providerBundle`, `registrations.go`); wiring a real adapter is a code change to bundle, resolver split, registrations and the outbound tripwire test | all | `integrations`, domain specialist, `security` | No: expected adapter-build work, listed in each C2/K2/S2/D2 gate | `casino` s10, `kyc-aml` s10, `sportsbook` s9; gates C2/K2/S2/D2 |
| X-G03 | Webhook scheme shape: PSP G-01, G-02, G-08 apply identically to casino, KYC, sportsbook push and sports-data push (the scheme registry is shared) | PSP, casino, KYC, sportsbook, sports data | `security`, `architect` | Potentially (depends on each vendor's scheme) | `psp` G-01/G-02/G-08 |
| X-G04 | Statement-source pattern: only payments has the per-(tenant, provider) registry, read-only attestation, eligibility marker, coverage refusal and `statement_read` credential question; casino and sportsbook use one global source object | PSP, casino, sportsbook | `ledger-finance`, `security`, `architect` | Real money (all); no for a no-money sandbox | `psp` G-04/G-09, `casino` CAS-G07, `sportsbook` SB-G07 |
| X-G05 | Per-tenant non-secret vendor configuration (operator/site/workflow ids, data region) has no home: one secret and one `vendor_account_id` per handle | PSP, casino, KYC, sportsbook | `security`, `architect` | No (workaround exists) | `psp` G-08 |

### PSP (from `psp/provider-neutral-mapping.md` section 12)

| Id | Gap | Scope | Owner specialist | Blocks first real adapter? |
|---|---|---|---|---|
| G-01 | `VerificationScheme.Extract` is header-only and body-free | PW | `security`, `architect` | Potentially |
| G-02 | A non-synthetic scheme must sign a timestamp (ADR 0022 s3 pt 10) | PW | `security`, `architect` | Potentially |
| G-03 | Two-reference lifecycle (hosted session token vs transaction id) | PW | `ledger-finance` | Potentially |
| G-04 | No credential purpose for a read-only statement credential | PW | `security`, `architect` | Yes (first real statement source) |
| G-05 | Dispute lifecycle: only final whole-deposit reversal representable | PW | `ledger-finance` | No for sandbox; Real money at volume |
| G-06 | Statement line has one amount + asset (settlement-currency reports) | PW | `ledger-finance` | Depends on N-U22/N-U28 |
| G-07 | File-based settlement reports vs record-time `OccurredAt` and paging | PW | `ledger-finance`, `payments` | Depends on N-U23..25 |
| G-08 | One secret and one `vendor_account_id` per credential | PW (small) | `security` | No |
| G-09 | Per-provider settle lag NOT IMPLEMENTED | PW | `payments`, `ledger-finance` | Yes (first real statement source) |
| G-10 | `QueryStatus` takes only a provider reference | PW | `payments`, `architect` | Only if N-U19 shows it is the sole lookup |
| G-11 | `OperationManifest` lacks `RedeliveryOn401/5xx`, `ErrorClassMapping`, `StatementSource` fields | PW | `architect` | Fields needed once a real adapter exists |

### Casino (`casino/provider-neutral-mapping.md` section 11)

| Id | Gap | Scope | Owner specialist | Blocks first real adapter? |
|---|---|---|---|---|
| CAS-G01 | Vendor-facing wire contract is platform-defined (callback response and status mapping, bootstrap parse hook F-12) | PW | `casino`, `architect`, `security` | Potentially, probably yes |
| CAS-G02 | Only bet/win/rollback exist: no inbound balance, round-close, combined, adjustment event; single event per callback | PW | `casino`, `ledger-finance` | Depends on U-CAS-13/17/18 |
| CAS-G03 | Free rounds / jackpot / promo payouts with no platform bet: no design, no conformance case | PW | `casino`, `ledger-finance`, `bonus-engine`, `architect` | Only if the chosen titles emit them |
| CAS-G04 | No per-bet win reference (multi-bet) and no catalogue flag/refusal for bonus-funded multi-bet titles | PW | `casino`, `ledger-finance`, `bonus-engine` | No (cash-only); yes for bonus-funded multi-bet |
| CAS-G05 | Game jurisdiction blocklist and market evidence not re-run per bet; mid-session travel undetected | PW | `security`, `identity-compliance`, `casino` | No (compliance decision) |
| CAS-G06 | Catalogue: no sync driver, RTP is one free-text field, `demo_supported`/`feature_flags` not enforced | PW | `casino`, `identity-compliance` | No |
| CAS-G07 | Casino statement source: global, no registry/attestation/eligibility, no stored copy, no real mismatch kind, no window, O(history) | PW | `ledger-finance`, `security`, `architect` | Real money |
| CAS-G08 | No round-timeout / abandoned-round policy | PW | `ledger-finance`, `casino` | Depends on U-CAS-34 |
| CAS-G09 | Documentation drift (`types.go` header, bootstrap doc is the MOCK shape) | docs | `casino` | No |
| CAS-G10 | `CallbackEvent.Amount` is `int64` (no crypto) | PW (deferred) | `ledger-finance`, `casino` | No (fiat-first) |

### KYC / AML (`kyc-aml/provider-neutral-mapping.md` section 11)

| Id | Gap | Scope | Owner specialist | Blocks first real adapter? |
|---|---|---|---|---|
| KYC-G01 | No hosted/redirect session in `ProviderResult`; documents platform-held with mock-only store and scanner (KYC-HOSTED-SESSION-1) | PW | `identity-compliance`, `security`, `architect` | Yes |
| KYC-G02 | Vendor cannot supply verified residence, validity, level; `expires_at` has no writer | PW | `identity-compliance`, `security`, `architect` | Yes if vendor-derived residence is wanted |
| KYC-G03 | Terminal states final on the provider path; post-approval revocation is a no-op | PW | `identity-compliance`, `security` | Yes if the vendor sends such events |
| KYC-G04 | `registration_tier` / `edd_amount` triggers not consulted; no level notion | PW | `identity-compliance` | Only if the first jurisdiction needs tiers |
| KYC-G05 | One `provider_reference` per verification (applicant vs check), no event id | PW | `identity-compliance`, `architect` | Potentially |
| KYC-G06 | No credential domain or `CallContext` for screening and geolocation (= X-G01) | PW | `architect`, `security` | Yes (screening, geolocation) |
| KYC-G07 | Screening: pseudonymous input only (Q6), no triggers/outbox/system actor/gate predicate/monitoring | PW | `identity-compliance`, `security`, `architect`, `ledger-finance` | Yes (screening vendor) |
| KYC-G08 | Potential sanctions match is single-officer-liftable though it holds all value movement | PW | `identity-compliance`, `security` | No (compliance decision; decide before a real vendor) |
| KYC-G09 | `SignalResult` cannot carry confidence/VPN/device/subdivision; no per-bet or client-side evidence | PW | `security`, `identity-compliance`, `architect` | Only if a geolocation vendor is selected |
| KYC-G10 | Data residency / retention / DPA not modelled | PW (non-code + config) | `architect`, `identity-compliance`, `devops` | Owner/legal input |

### Sportsbook (`sportsbook/provider-neutral-mapping.md` section 10) - DEFERRED

| Id | Gap | Scope | Owner specialist | Blocks first real adapter? |
|---|---|---|---|---|
| SB-G01 | `Provider` is `Catalogue` only; no placement/status/cancel/cashout; no pending state; operating model undecided | PW | `architect`, `sportsbook`, `ledger-finance` | Yes (first decision) |
| SB-G02 | Settlement identity is `(bet_id, caller-supplied generation)`; provider event ids not stored or deduplicated | PW | `ledger-finance`, `sportsbook` | Yes (provider-driven settlement) |
| SB-G03 | Payout derived from frozen odds; legitimate provider payout differences cannot post | PW | `ledger-finance`, `sportsbook` | Depends on U-SB-02/07/09 |
| SB-G04 | No inbound sportsbook route, scheme, retry declaration or admission domain | PW | `security`, `sportsbook`, `architect` | Yes if the vendor pushes |
| SB-G05 | Singles, cash only; no partial, cashout, multiples, bet-builder, live, bonus/free bets | PW | `sportsbook`, `ledger-finance`, `bonus-engine` | Depends on vendor scope |
| SB-G06 | Acceptance unlocked reads, exact odds only, live allowed, catalogue synced once | PW | `sportsbook`, `security`, `risk` | Yes (live or feed prices) |
| SB-G07 | Sportsbook statement source: per platform bet id, takes the run tx, global, no registry/attestation, mock mismatch kind | PW | `ledger-finance`, `security`, `architect` | Real money |
| SB-G08 | Open-liability reporting not built; exposure limits unarmed | PW | `data-analytics`, `ledger-finance`, `risk` | Real money |
| SB-G09 | No player/operator cancel path; no self-exclusion auto-void | PW | `sportsbook`, `risk`, `identity-compliance` | Owner/compliance decision |
| SB-G10 | Credential domain absent (= X-G01) | PW | `architect`, `security` | Yes |

### Sports data (`sports-data/provider-neutral-mapping.md` section 9) - DEFERRED

| Id | Gap | Scope | Owner specialist | Blocks first real adapter? |
|---|---|---|---|---|
| SD-G01 | No ingestion seam at all (candidate: ADR 0117 Part C slice C1, MOCK only) | PW | `architect`, `sportsbook`, `security` | Yes |
| SD-G02 | Catalogue `external_ref` globally UNIQUE; sync is unordered last-writer-wins; can reopen finished/closed; creates unseen entities | PW | `sportsbook`, `architect`, `security` | Yes (any live or periodic sync) |
| SD-G03 | No credential seam or push-admission domain for a feed (= X-G01) | PW | `security`, `architect` | Yes |
| SD-G04 | No result entity/seal/hold/four-eyes/fan-out consumer/driver column | PW | `ledger-finance`, `sportsbook`, `security` | Yes (settling from a feed) |
| SD-G05 | No staleness bound, odds version, price-change policy, C0 lock, watchdog; live allowed | PW | `sportsbook`, `risk`, `security` | Yes (live/feed prices) |
| SD-G06 | Canonical model too small (participants, market types/lines, lifecycle, dead heat) | PW | `sportsbook`, `architect` | Depends on product scope |
| SD-G07 | Single result source, no cross-check; thresholds are owner inputs | PW | `risk`, `ledger-finance`, `security` | Policy decision |
| SD-G08 | Data rights / licensing | non-code | owner, legal | Owner/legal input |

## How to use these packages

- To start an area: the owner names a provider and runs gate 0/1 of that area (documentation review only: no account, no
  credential, no real data). Each area's `open-questions.md` lists the minimum decisions with recommendations.
- To review a new vendor adapter: `integrations` checks it against the common rules above (reuse of the shared
  scaffolding), the domain specialist checks it against the area's mapping and conformance plan, `security` and
  `ledger-finance` run their gates.
- To change anything cross-cutting (X-G01..X-G05, a webhook scheme amendment, a new credential domain): through `architect`
  and a record in `docs/decisions/`; these packages record the need, they do not make the decision.
