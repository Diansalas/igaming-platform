# 08 — Casino Integration Architecture

Status: Stage 4A `IMPLEMENTED` (foundation only — mock adapter, no real
casino provider, no production credentials). Supersedes the Stage 0
proposal this document originally held; that proposal's intent is carried
forward and refined below, not discarded. Authoritative decision record:
`docs/decisions/0025-casino-provider-abstraction-and-game-session-model.md`
(ADR 0025) — this document explains the design in narrative/diagram form at
`payment-orchestration.md`'s depth; ADR 0025 is the binding record of
*why* each choice was made and is the source of truth if the two ever
disagree.

## 1. Scope

Stage 4A builds the provider-agnostic foundation a real casino aggregator
integration will plug into later: the `CasinoProvider` interface, a
`MockCasinoProvider` implementing it, a game catalogue split into
platform-wide content vs. tenant/brand opt-in, an opaque single-use
game-launch-token mechanism, a two-layer capability/routing model, and the
bet/win/rollback financial boundary on top of the already-approved ledger
(`financial-transaction-flows.md` Flows 5-7). Explicitly **not** in scope
this stage: a real provider contract, production credentials, sportsbook,
the Bonus Engine, KYC/AML/RG, a player-facing lobby UI, or a back-office
catalogue-management UI (API only). Free-round/bonus-stake normalization
(the "messiest part" the Stage 0 proposal called out) is `OPEN DECISION`,
deferred to the Bonus Engine stage — see §8.

## 2. Core principle: what the platform owns vs. what a provider owns

Identical split to payments (`payment-orchestration.md` §1), restated for
casino:

- **Platform owns**: identity, player account, wallet, ledger, financial
  truth, tenant/brand configuration, authorization, audit, RG enforcement
  boundaries, game-session orchestration, and the provider integration
  *contract* itself.
- **Provider owns**: game catalogue content, game rendering/mechanics,
  provider-side game execution, and the provider's own API surface.

Provider-specific data models or business rules must never leak into
wallet/ledger/identity/tenant/authorization/financial-truth code. Every
place this boundary is enforced in code is called out in §3-§7 below.

## 3. `CasinoProvider` interface — `ARCHITECTURAL DECISION`

```go
type CasinoProvider interface {
    Catalogue(ctx context.Context) ([]CatalogueEntry, error)
    Launch(ctx context.Context, req LaunchRequest) (LaunchResult, error)
    Balance(ctx context.Context, req BalanceRequest) (BalanceResult, error)
    Bet(ctx context.Context, req BetRequest) (BetResult, error)
    Win(ctx context.Context, req WinRequest) (WinResult, error)
    Rollback(ctx context.Context, req RollbackRequest) (RollbackResult, error)
    HandleCallback(ctx context.Context, rawPayload []byte) (CallbackEvent, error)
    Capabilities() AdapterCapability
    HealthStatus(ctx context.Context) (ProviderHealth, error)
}
```

(`internal/casino/types.go`.) Mirrors `PaymentProvider`'s exact shape
discipline (`docs/decisions/0022` §2.1): no provider SDK types anywhere in
a method signature, no free-form passthrough field, every request/response
field explicitly enumerated and typed. `Bet`/`Win`/`Rollback` are the
adapter's own synchronous request/response contract (exercised directly by
the conformance suite); `HandleCallback` is the orchestrator's actual entry
point for a push-style (webhook) provider — it parses and *authenticates* a
raw payload into the canonical `CallbackEvent` before the orchestrator ever
sees a field from it. A provider whose real transport is one shape or the
other implements both: `HandleCallback` for a push provider translates
webhook bytes into `CallbackEvent`; for a provider whose game server calls
the platform synchronously, `HandleCallback` still exists (translating that
provider's own wire format) so the orchestrator's dispatch code has exactly
one call site regardless of adapter transport.

## 4. Game catalogue — platform identity vs. provider identity

Two tables, mirroring the adapter-declared/operator-configured split ADR
0022 established for payment capabilities:

- **`casino_games`** (platform-wide, no RLS, no `tenant_id` — same shape as
  the `assets` registry): the provider's own declared facts about a title —
  `provider_id`, `provider_game_id`, name, type, RTP variant, volatility,
  feature flags, supported assets, mobile/demo support, a jurisdiction
  blocklist, and a platform-level `active`/`disabled` status. `provider_id
  + provider_game_id` is unique; the platform's own `id` (a fresh UUID) is
  assigned once at first sync and never re-derived — `provider_game_id` is
  never promoted to the platform's canonical identity anywhere (ADR 0025
  §2). Written only via `casino.UpsertGame`, gated by the platform-only
  `PermCasinoCatalogueManage` permission (§7).
- **`casino_game_availability`** (tenant-owned, RLS-protected): a tenant's
  opt-in layer over the platform catalogue — `enabled` per
  `(tenant, game)` or `(tenant, brand, game)`, resolved most-specific-row-
  wins (a brand-specific row replaces a tenant-wide one entirely, never a
  per-field merge — identical resolution rule to ADR 0022 §3). No row at
  all means **not available** — fail-closed; a tenant must explicitly opt a
  title in. Written via `casino.SetGameAvailability`, gated by
  `PermCasinoConfigWrite`.

`casino.ListAvailableGames` joins the two (platform-`active` title +
tenant/brand-`enabled` row) to produce the player-facing catalogue view —
never a table a player reads directly. Not built this stage: a catalogue
sync job, a jurisdiction-resolution engine beyond the static blocklist
check, free-round/bonus normalization, or a back-office catalogue UI (ADR
0025 §2's explicit "not built this stage" list).

## 5. Game launch and session model — `ARCHITECTURAL DECISION`

**The player's platform JWT is never passed to a casino provider.**
`LaunchGame` (`internal/casino/orchestrator.go`) instead mints an entirely
separate, opaque, single-use, database-backed launch credential
(`internal/casino/launch.go`), for two reasons recorded in ADR 0025 §3:

1. **Trust-domain separation** — a JWT verifiable against the platform's
   own signing keys must never be handed to an external party; a
   provider-facing credential must be independently revocable/expirable
   without touching session infrastructure.
2. **Scope minimization** — the credential should carry exactly what a game
   launch needs (player/tenant/brand/game/provider/provider-game-id/
   jurisdiction-context/asset/mode/session/expiry), nothing about the
   player's broader session.

Mechanically, it mirrors `internal/auth`'s own refresh-token generation
(`crypto/rand` → 256 bits → `base64.RawURLEncoding` → SHA-256 hash
persisted, raw value returned exactly once) but in a wholly separate table
(`casino_launch_sessions`) and trust domain — zero dependency on
`internal/auth`'s JWT/session internals. Single-use enforcement is an
atomic conditional `UPDATE ... WHERE status = 'active'` checked via
`RowsAffected()`, not a read-then-write — two concurrent resolution
attempts for the same token can never both succeed
(`ResolveLaunchToken`). Default TTL is 2 minutes (`DefaultLaunchTokenTTL`).
A session that fails after minting (provider launch call itself errors, or
the provider declines) is `RevokeLaunchSession`'d, never deleted — the row
is the audit-visible record that a launch attempt occurred.

`LaunchGame`'s full eligibility chain, each failure mode returning a
distinguishable sentinel error or, for the jurisdiction gate, a
`LaunchGameResult{Denied: true, DenialCode: ...}` (see below): game exists
(`ErrGameNotFound`) → platform-enabled (`ErrGameDisabled`) → tenant/brand
opted in (`ErrGameNotAvailable`) → not jurisdiction-blocked (K-3, below) →
asset supported by the game (`ErrInvalidInput`) → provider capability
active and asset-supporting (`ErrProviderUnavailable`) → provider
registered (`ErrUnknownProvider`) → provider healthy/circuit-closed
(`ErrProviderUnavailable`) → mint session → call `provider.Launch`.

**K-3 jurisdiction blocklist remediation (Stage 4I,
`docs/governance/stage-4i-canonical-model.md` §9) — `IMPLEMENTED`.** The
per-game jurisdiction gate resolves the player's jurisdiction exactly once
per launch (`internal/jurisdiction.Resolve`, platform core, consumed —
never re-implemented — by this package) and feeds that ONE value to three
consumers: the blocklist check below, the Risk request just after it, and
the launch session's persisted `jurisdiction_code` snapshot. The control is
only **armed** for a game whose own `jurisdiction_blocklist` array is
non-empty (`cardinality(...) > 0`, a static, jurisdiction-resolution-free
test) — a game with an empty blocklist has no jurisdiction-dependent
policy in force and launches exactly as before. Within an armed game, an
unresolved player jurisdiction denies with the internal-only
`DenialCodeJurisdictionUnresolved`; a resolved-but-listed jurisdiction
denies with `DenialCodeJurisdictionBlocked` — deliberately distinguishable
internally (audit metadata, logs, `jurisdiction_resolutions`) but collapsed
into one byte-identical player-facing HTTP response
(`internal/httpserver/casino_handlers.go`'s `writeCasinoLaunchDenial`) to
avoid an HTTP-boundary oracle for jurisdiction-resolution manipulation.
This check runs identically for real and demo launches — demo is
jurisdiction-bearing by default (architect's ruling, canonical-model §9.6):
catalogue availability ("may this title be offered in this market") does
not depend on which endpoint is asked. Since no player-side jurisdiction
signal exists anywhere in this codebase yet (HDR-J-3 is unanswered), every
player-scoped resolution is `unresolved(no_signal)` in Stage 4I — so this
control currently denies launches ONLY for a game an operator has
explicitly given a non-empty blocklist to (statically enumerable via
`SELECT ... WHERE cardinality(jurisdiction_blocklist) > 0`), never for the
platform's ordinary catalogue.

## 6. Provider callback authentication — `ARCHITECTURAL DECISION`

A provider callback is **not** an authenticated platform principal — there
is deliberately no bearer-token middleware on
`POST /v1/webhooks/casino/{tenantSlug}/{providerID}`
(`internal/httpserver/casino_handlers.go`), identical to the payments
webhook's own reasoning. Trust is established instead by the shared
inbound-callback contract, `docs/decisions/0022` §3 as amended (points 1–7
from Stage 10.1, points 8–9 from Stage 10.2; primitives in
`internal/webhookauth`). The steps, in order:

*(Steps 1 and 2 rewritten 2026-09-26, Stage 10.2, ADR 0091,
CAS-WH-TENANT-1; see `docs/decisions/0025` "Amendment (Stage 10.2)". The
earlier text described the superseded model: tenant from the URL slug,
plus a per-process key with no tenant in the MAC.)*

1. **The route slug is a lookup hint only.** The shared preamble checks
   the provider id charset, the signature and key-id headers, and the body
   size (1 MiB). The handler then resolves exactly one *candidate* tenant
   with the platform-scoped `GetTenantBySlug` and opens `WithTenant` for
   it. Before verification succeeds, no other statement runs, and in
   particular no tenant-scoped one (strict I1, ADR 0022 §3 point 9). An
   unknown slug, an inactive tenant, an unregistered provider, a nil
   resolver, a missing credential and a bad signature all get the same
   uniform 401. Unauthenticated failures write no audit row.
2. **One credential; the tenant is bound into what is verified.** The
   orchestrator resolves one credential for (candidate tenant, route
   `provider_id`, key id) through its injected `webhookauth.Resolver`, and
   checks that the credential's tenant and provider equal the route's.
   The adapter's `HandleCallback(ctx, in, cred)` then verifies the **raw
   bytes** before it parses anything. A real adapter uses its vendor's own
   scheme; the platform never invents a generic signature scheme for
   vendors. `MockCasinoProvider` uses the platform-defined MOCK scheme:
   HMAC-SHA256 over
   `igaming.casino.webhook.v1‖0x00‖tenant_id‖0x00‖provider_id‖0x00‖key_id‖0x00‖raw body`,
   with headers `X-Casino-Signature`/`X-Casino-Key-Id`, key id `mock-v1`,
   and a key derived per tenant (label `igaming/casino-mock-webhook/v1`)
   from a per-process random master. So bytes signed for tenant A never
   verify when posted to tenant B's route. A verified-but-malformed body
   is a distinct 400.
3. **Only after verification:** the tenant capability check
   (`status == active`, otherwise 503; see CAS-CAP-ROLLBACK-1), then
   dispatch and posting. Every write uses the verified tenant as both the
   RLS context and the binding.

**Gating (ADR 0085).** The mock resolver is wired only when
`TestSupportRoutesEnabled()` is true (`cmd/platform-api/wiring.go`
`mockProviderWiring`). Otherwise the route still exists, but every
callback returns 401 `no_resolver` and no casino money moves. A real
aggregator resolver is `NOT IMPLEMENTED`.

The orchestrator itself (`ReceiveCallback`) only ever consumes
`HandleCallback`'s already-verified `CallbackEvent` output — it never
branches on which adapter produced it.

## 7. Bet/Win/Rollback financial boundary — `CRITICAL`

**The casino provider is never the source of truth for player funds.**
`Balance`/`BalanceResult` (a read-only, provider-reported figure) is never
consulted to authorize or size a ledger posting anywhere in
`orchestrator.go` — the wallet/ledger remain sole financial truth exactly
as `payment-orchestration.md`'s equivalent boundary already established for
payments.

`ReceiveCallback` dispatches a verified `CallbackEvent` to one of three
posting functions, each implementing an already-BLUEPRINT flow from
`financial-transaction-flows.md`:

| Event | Flow | Posting | Ledger accounts |
|---|---|---|---|
| Bet | 5 | debit `player_cash`, credit `house_gaming` | invariant #15: balance locked (`SELECT ... FOR UPDATE`) and checked **inside** the same transaction as the debit; insufficient funds → `OutcomeDeclined`, nothing posted. The wallet debited is resolved from the platform's own `casino_launch_sessions` row named by the callback's `session_id` — never from a payload-supplied `player_account_id` — and rejected outright for a missing/unknown/wrong-provider/demo-mode/asset-mismatched session |
| Win | 6 | debit `house_gaming`, credit `player_cash` | a win naming a round with no matching, still-valid (never rolled back) prior bet under the SAME tenant is `ErrBetNotFound` — an integrity alert (provider protocol violation), never silently posted. The wallet credited is resolved from the round's own bet transaction's ledger entries, never from the win callback's own `player_account_id` |
| Rollback | 7 | exact inverse of whichever of Flow 5/6 the named original posted | looked up by `(tenant_id, provider_id, provider_tx_id)` under a row lock (`SELECT ... FOR UPDATE`, closing a concurrent-double-reversal race found in review); an original never seen writes a `tombstone` (mirrors `payments.postDepositReversalTombstone`), never an error |

**Lock ordering:** the "balance locked (`SELECT ... FOR UPDATE`)"
mechanism named in the Bet row above is superseded in form (never in
effect) by `docs/decisions/0082-canonical-financial-lock-ordering.md`,
which closes finding LOCK-1 — the real ABBA deadlock between `postBet`
and `postWinDirectCash` on the same two projection rows. Under ADR 0082
`postBet` no longer takes its own projection lock: it pre-locks its
complete account set through `ledger.LockProjectionsForPosting` and reads
the balance from that result. Invariant #15 (balance read and debit in
one transaction, under a held lock) is unchanged. Read ADR 0082 before
changing any locking behaviour in `internal/casino`.

Every bet/win/rollback callback is additionally gated by the tenant's own
`CasinoProviderCapability` (§9) inside `ReceiveCallback`, before dispatch —
a disabled or unconfigured capability is a real kill switch on the money
path, not merely a launch-time check. A bet/win callback whose own
`Outcome` field is not `succeeded` is rejected outright
(`ErrOutcomeNotSucceeded`) rather than posted as though it were.

Every posting call goes through the existing `ledger.Post` API — no second
balance system, no mutable "casino balance" anywhere. All amounts are
`int64` minor units against the existing `NUMERIC(38,0)` + per-asset-
exponent schema; nothing here is floating point. `player_bonus`-funded
stakes and jackpot-contribution splits are an explicit, ADR-documented
`OPEN DECISION` — Stage 4A's posting logic assumes a 100%-`player_cash`-
funded stake only, written so a later bonus-aware split is a change
*inside* `postBet`/`postWin`, never a redesign of the interface or ledger
schema (ADR 0025 §6).

A bet/win/rollback correlates to its round via a **deterministic**
`uuid.NewSHA1`-derived `correlation_id` computed identically from
`(tenant_id, provider_id, round_id)` by whichever posting call sees it
first — not a separate "rounds" table. A rollback names exactly ONE
original reference (`OriginalProviderTxID`); a round needing both its bet
and its win rolled back requires two separate rollback events, one per
original — mirroring how payments' deposit-reversal handles one original
per reversal event.

## 8. Idempotency model

Every financial write's idempotency key is `providerID + ":" +
event.ProviderTxID` — namespaced by provider, since nothing in the
`CasinoProvider` contract guarantees `provider_tx_id` uniqueness *across*
providers (the same rationale as payments' deposit-posting key). A
redelivered bet or win callback is a true no-op via `ledger.Post`'s own
`(tenant_id, idempotency_key)` uniqueness — never "check then insert"
application logic. A redelivered *rollback* required one specific fix
during this stage's own adversarial testing: the "has this original
already been rolled back" check must distinguish a genuine second, distinct
rollback reference (reject with `ErrAlreadyRolledBack`) from a redelivery
of the *same* rollback reference (fall through to `ledger.Post`'s own
idempotent no-op) — see §17 of the Stage 4A completion report for the
finding and fix.

## 9. Provider capability model — two-layer split

`AdapterCapability` (layer a — what the adapter itself declares:
`Capabilities()`, static, never tenant-scoped) vs. `ProviderCapability`
(layer a + layer b — the tenant/brand-configured subset actually routed
to: which of catalogue/launch/balance/bet/win/rollback is enabled, which
assets/game types, priority, active/disabled status). `casino.
WriteCapability`'s `validateNarrowing` enforces the one-way rule: a tenant
configuration may only **narrow** what the adapter declares, never assert
support the adapter doesn't have — mirrors ADR 0022 §2.1 exactly. Resolved
most-specific-row-wins, same nullable-brand-fallback pattern as
availability (§4). No amount-limits table exists for casino — responsible-
gaming stake limits are out of scope this stage (ADR 0025 §4).

`ProviderHealth`/`CircuitState` are in-memory, per-adapter-instance values
(never persisted) — identical shape and rationale to
`payment-orchestration.md` §6's circuit breaker.

### 9a. Capability contract and casino reconciliation (Stage 10.3 pointer)

*Added 2026-09-26 (Stage 10.3, ADR 0092).* Status: `NOT IMPLEMENTED` until
W1c and W2b land. The binding text lives in the ADRs and papers below and
is not repeated here.

- **Capability contract.** Source: ADR 0025, "Amendment (Stage 10.3,
  ADR 0092)".
  - The capability gates **new bets only**. The gate is in `postBet` and
    uses the session's `BrandID`.
  - Win, rollback, replay and tombstone are never gated.
  - A rollback of an unseen original always writes a tombstone.
  - A late original after its tombstone gets a named rejection.
  - A CHECK enforces
    `NOT supports_bet OR (supports_win AND supports_rollback)`.
  - The emergency stop is credential revocation. There is no settlement
    freeze.
  - Suspended-tenant behaviour is unchanged (HD-10.3-4): the shared
    webhook preamble rejects the callback before verification.
  - Lock order: L0.1 in `postRollback` (ADR 0082 Amendment A6).
- **Casino reconciliation.** Source:
  `docs/plans/stage-10.3-planning/02-casino-financial-analysis.md` §2.
  - The **`casino_consistency`** stream runs checks C1–C7:
    - C1 round binding;
    - C2 posting shape;
    - C3 orphan win;
    - C4 rollback linkage;
    - C5 tombstone backstop;
    - C6 unposted provider-asserted event;
    - C7 tombstone later matched by an original.
  - The stream runs on the existing `reconciliation_runs` and
    `reconciliation_mismatches` tables.
  - Its input is the verified-only, append-only
    **`casino_callback_rejections`** record.
  - It detects only and never auto-corrects. Ageing cash rounds are a
    metric, not a P1.
  - The **`casino_statement`** stream matches against a provider-neutral
    `CasinoStatementSource`. Its only source today is **MOCK** (W3a,
    first to cut). A real source is `PROVIDER DEPENDENT`.
  - Compensation depends on LEDGER-MANUAL-ADJ-4EYES-1, which is not
    built.

## 10. Provider routing

`Orchestrator.LaunchGame` resolves a game's provider from `casino_games.
provider_id` (never a client-supplied value), then looks up that
provider's effective `ProviderCapability` for the caller's
`(tenant, brand)`, checks it is `active` and asset-supporting, checks the
registered adapter's own `HealthStatus` isn't `CircuitOpen`, and only then
proceeds. `casino.ListRoutingCandidates` exposes every provider configured
for a `(tenant, brand)` for future multi-provider-per-game routing
(e.g. the same title mirrored by two aggregators) — Stage 4A's catalogue
model assumes one provider per `provider_game_id`, so today this resolves
to exactly the game's own provider, but the routing boundary itself does
not hardcode that assumption anywhere (no provider-specific conditional
exists in `orchestrator.go` or the HTTP layer).

## 11. `MockCasinoProvider`

Implements the full `CasinoProvider` contract with deterministic, amount/
reference-keyed synthetic behavior (`internal/casino/mock.go`), mirroring
`MockProvider`'s documented "magic value" convention rather than inventing
a different testing idiom: `MockBetAmountDecline`/`MockBetAmountAmbiguous`
drive `Bet`'s synchronous outcome; `MockGameIDDeclineLaunch` drives
`Launch`'s decline path; `FailNextCall` simulates a one-shot transport
failure; `SetHealth` drives circuit-breaker-gated routing tests. `MOCK`
per CLAUDE.md's "no fake completion" rule — a synthetic double for
development/testing, never a real aggregator; no real credentials, no real
commercial relationship.

## 12. Provider conformance suite

One parameterized suite (`RunProviderConformanceSuite`,
`internal/casino/conformance_test.go`), run identically against
`MockCasinoProvider` now and any future real adapter later — the same
binding rule ADR 0022 §6 states for payments, applied verbatim. The
adapter-contract-only half (no database) covers catalogue, launch,
bet/win/rollback contracts, ambiguous/timeout outcome, provider
authentication, provider failure, and capability/health shape. The real-
Postgres half (`orchestrator_integration_test.go`, `//go:build
integration`) covers session isolation, idempotency/duplicate/replay,
cross-tenant isolation, every launch-eligibility failure mode, the
player-scope-exclusion RLS guard, auditability, and financial-invariant
preservation across a mixed bet/win/rollback sequence. Full item-to-test
mapping is in the Stage 4A completion report §16.

## 13. Security/RLS

`casino_games` carries no RLS (platform-wide, like `assets`).
`casino_game_availability`, `casino_provider_capabilities`,
`casino_launch_sessions`, and `casino_provider_rounds` (Stage 8, below)
all carry `ENABLE`+`FORCE ROW LEVEL SECURITY`. The first two use the
`tenant_isolation` staff-only pattern (with the player-scope-exclusion
guard — `app.player_account_id` must be unset); `casino_launch_sessions`
and `casino_provider_rounds` both use the dual `tenant_staff_scope` +
`player_self_scope` (SELECT-only) pattern, identical shape to
`withdrawal_requests` (migration 0026). Every composite foreign key pins a
launch session to its actual owning tenant/brand/player/wallet — see
migration 0035 for the exact constraints. Casino provider credentials are
config, never source code; Stage 4A introduces no real credential of any
kind.

## 13a. Provider-round persistence (Stage 8, ADR 0080 Decision 1)

`casino_provider_rounds` (migration 0080) durably binds a
provider-declared `provider_round_id` to the platform's own
`launch_session_id`/`player_account_id`/`brand_id`/`game_id`/
`correlation_id`, resolving the residual limitation ADR 0048 (Stage 7)
documented: the round read model in `history.go` re-derives
`correlation_id` from `roundCorrelationID(tenantID, providerID, roundID)`,
a convention Stage 7's play-simulation seam introduced by always setting
`RoundID = session.ID.String()` — a real provider's own round id is never
assumed to equal a session id, so without this table a real provider's
round would have had nowhere durable to be looked up from by provider
identifiers alone.

Uniqueness is `(tenant_id, provider_id, provider_round_id)` — a
documented conservative assumption (mirroring
`idx_ledger_transactions_tenant_provider_tx`'s own tenant+provider
scoping), not a fact about any real provider's contract; see ADR 0080
Decision 1 for the full reasoning and the revisit condition. The
ownership-conflict guard (an atomic `INSERT ... ON CONFLICT ... DO UPDATE
... WHERE <ownership match> RETURNING id`, verified race-free under
concurrent binding attempts) checks `player_account_id`+`brand_id` only —
a round belongs to exactly one player and brand, not to exactly one
launch session, so a same-player/same-brand continuation across two
launch sessions (e.g. a free-spins round outliving a session timeout)
succeeds, while a different player or brand naming the same round is
rejected with `ErrProviderRoundOwnershipConflict`, mapped to an HTTP 409
(never a 500) at both `POST /v1/webhooks/casino/.../{providerID}` and the
play-simulation endpoints, with no round id or identity echoed back to
the caller. Binding happens in `postBet`, positioned immediately before
`ledger.Post` (after RG/Risk/insufficient-funds all pass), so a declined
bet leaves no row — the table's population semantics are "rounds actually
bet on," not "rounds a provider attempted." `postWin`/`postRollback`
remain unchanged, resolving accounts entirely from the ledger's own prior
entries. The table carries the same `BEFORE UPDATE` immutability-trigger
convention as `casino_launch_sessions` (migration 0036) and
`withdrawal_requests` (migration 0026): only `last_seen_at` and
`launch_session_id` may change on an existing row.

## 14. Observability

Every casino operation carries the existing structured-logging/audit
correlation context (tenant, provider, game, provider transaction id,
platform operation id where applicable) through the same mechanisms
established in earlier stages — no separate casino-specific logging
pipeline. Audit actions: `casino.launched`, `casino_bet.posted`,
`casino_bet.declined`, `casino_win.posted`, `casino_bet.rolled_back` /
`casino_win.rolled_back`, `casino_rollback.tombstoned`,
`casino_provider_capability.configured`, `casino_game_availability.
configured`, `casino_game.upserted` — all through the existing immutable
`audit_log` architecture, never a bespoke mechanism.

## 14a. Non-active tenants: new postings are refused (R3-GAME-POSTINGS-NONACTIVE-1)

Owner decision 2026-10-05, fail closed (ADR 0095 §40.5, migration 0119). A verified bet, win or rollback
(including the rollback tombstone of an unseen original) that would create a NEW ledger posting for a
suspended or closed tenant is refused inside the posting transaction with `ErrTenantNotActive`: no ledger
write, no wallet change, answered `409` (never a retryable 5xx), and a durable audit row
`casino_callback.rejected_tenant_not_active`. The check is `tenant.RequireActiveForGameplay` (advisory-lock
pair with a status-change trigger, so there is no check-then-act race) placed after each function's own replay
short-circuit; an exact replay of an already-posted callback still returns the original outcome. A DB trigger
on the casino ledger types is the backstop. Consequence: rounds open at closure strand until a staff path
exists, so closing a tenant must first settle or void open rounds (owner questions Q-GP-1..4 in ADR 0095
§40.5). Casino launch and reads are unchanged.

## 15. Sportsbook is a separate future concern

Casino and sportsbook providers are explicitly **not** assumed to share
this interface. Sportsbook's own future chain — Platform Identity →
Wallet/Ledger → canonical Sportsbook interface → Sportsbook Adapter A/B →
external provider — gets its own abstraction in
`09-sportsbook-architecture.md`, built once real API documentation for both
contracted providers is supplied (per the business requirement: two
sportsbook providers via API, never invented ahead of the real docs).

## 16. Bonus-funded casino settlement-credit resolution — G-2 boundary specification (Stage 4H-B1, Wave 1.5 Reconciliation Gate)

**Status: `NOT IMPLEMENTED`. DESIGN ONLY.** Issued per the Wave 1.5
Architecture Reconciliation Gate (`docs/governance/task-registry.md`,
Stage 4H-B1 Wave 1.5 §4HB1W15-01), in response to `architect`'s Wave 1
escalation that gate **G-2** (`docs/decisions/0039-*.md` Decision 2) is
reachable in the casino-only slice, and that `postWin`
(`internal/casino/orchestrator.go`) hardcodes its credit destination. This
section closes the design gap **without selecting G-2's answer** and
**without authorizing any bonus-funded-casino code**. No production code
is changed by this section; §16.1 proves the defect against already-
committed code rather than by writing a reproduction.

**Fix-wave revision note (Stage 4H-B1, Wave 1.5 Fix Wave, task
4HB1FW-02).** `ledger-finance`'s Phase 2 review (findings LF-1, LF-4,
LF-7, LF-8, LF-10) found this section's *own* first draft self-
contradictory or incomplete in five places, independent of G-2 itself.
All five are fixed below, in §16.3, §16.4, new §16.4a, new §16.5a, §16.7,
§16.8, §16.11, §16.12, and §16.13. Summary, each attributed:

- **LF-1** (fixed in §16.4): the original query read the bet's *debit*
  leg with no `account_type`-conditioned amount, then the destination map
  treated `player_bonus` (which is what a lock-shaped bonus bet's debit
  leg actually is) as the *unconditional, no-G-2* row — meaning the
  G-2-gated row, keyed on `player_locked_bonus`, could never be reached by
  the debit leg at all. §16.7's exploit-closure proof accordingly did not
  hold as originally written. Re-walked in full below; it holds now.
- **LF-4** (fixed in new §16.5a): the original design changed the win
  credit destination but never released the lock, and had no mechanism at
  all for a *losing* bonus-funded round (casino has no loss callback).
  §16.5a designs both the win-side release (falls out of the LF-1 fix)
  and a new loss-resolution mechanism (a bounded, per-jurisdiction
  settlement-timeout sweep, recommended over a provider-protocol
  extension, with a fail-closed default and a late-win alerting
  contract).
- **LF-7** (fixed in §16.3, §16.4, §16.11): a fourth query outcome — one
  distinct `account_type`, more than one distinct `wallet_id` — was
  unhandled, and §16.3's claim that this was "already caught" by the
  existing asset check was false (that check compares only `AssetCode`,
  never `wallet_id` or player identity, confirmed against
  `orchestrator.go:867`). Now a named, aborting fourth outcome.
- **LF-8** (fixed in new §16.4a): the ">1 distinct `account_type` is
  structurally unreachable by HR-2" claim was wrong — HR-2 only blocks a
  *single* posting instruction with two funding origins, not two separate
  `casino_bet` transactions sharing one round-level `correlation_id`
  (legitimate multi-bet rounds/re-bets/side bets). §16.4a designs a real
  handling path and names the actual missing capability (a bet-level
  identifier on win events, which `internal/casino/types.go` confirms
  does not exist today).
- **LF-10** (fixed in §16.5, §16.11, §16.12, §16.13): this section's own
  "fail closed on insufficient balance" pre-commitment for a rollback of
  an already-forfeited win is withdrawn — that is `ledger-finance`'s
  decision, not casino's, and this document no longer pre-selects an
  answer.

**Fix-wave Round 2 revision note (Stage 4H-B1, Wave 1.5 Fix Wave, Phase 2
closure — task 4HB1FW2-01).** Phase 2 independent re-verification
(`ledger-finance`) found a real, reachable ledger-invariant violation in
this section's own Round-1 fix, vetoed it, and named it **LF-18** — a P0
the human directive authorizing this round requires closed. This
section's own Phase 2 self-review found three more gaps. All four are
fixed below, together with one binding contract adopted from
`ledger-finance`'s own just-completed Round 2 decision
(`ledger-accounting-model.md` §7.7.2), which is this section's
load-bearing dependency for the rest of this round — read it before
reading anything below.

- **LF-18** (fixed in §16.4, exploit re-walked in new §16.17). §16.4 Step
  1's query answered the wrong question: it summed only the *original*
  bet-time credit leg (`ledger-accounting-model.md` §6.3.3.1 "variant 1"),
  never the *currently outstanding* net-locked amount (variant 2), and
  §16.5a's lock-release step then treated that stale bet-time figure as
  "the exact quantity to release." Concrete, reachable exploit: §16.5a(a)'s
  settlement-timeout sweep posts a new `casino_settlement_timeout`
  transaction debiting the lock to zero on a presumed loss (it does not
  reverse the original bet); a genuine late win then arrives; Step 1 still
  returned the unchanged original bet-time amount; no outcome branch
  detected the lock had already been zeroed by the sweep; §16.5a released
  the (already-released) amount a second time — `player_locked_bonus`
  driven negative and restricted bonus value materialized into
  `player_bonus` from nothing, invisible to `SUM(DEBITS)==SUM(CREDITS)`.
  **Fixed**: Step 1 is split into an identity query (unchanged — still
  needed for LF-7/LF-8) and a new Step 1b running `ledger-accounting-
  model.md` §6.3.3.1's "variant 2" shape (no `transaction_type` filter,
  signed sum) to get the currently-outstanding net-locked amount, plus a
  new, sixth named outcome that aborts — never releases — when that net is
  zero or negative.
- **Held-win rollback gap** (this section's own Phase 2 self-review,
  corroborated by `qa` test `C28`; addressed in new §16.15). A provider
  rollback targeting a WIN currently parked `held` in a
  `bonus_held_dispositions` row had no defined state transition anywhere.
  `ledger-finance`'s Round 2 (`ledger-accounting-model.md` §7.7.2.7)
  already closed the *still-held* sub-case on its own initiative, via the
  new `player_bonus_held` account and a guarded compare-and-swap status
  update. This section adopts that transition into `postRollback` and
  proves it under concurrency. The *already-resolved-then-rolled-back*
  case remains **LF-10**'s general, still-open question, explicitly routed
  to `ledger-finance` and not re-decided here (new §16.20).
- **Idempotency key correction (LF-22, adopted from `ledger-finance`).**
  A round-scoped key (`correlation_id`) is wrong, proven so by this
  document's own §16.4a multi-bet-round analysis (one `correlation_id` can
  legitimately contain several, separately-settled bets, each needing its
  own hold record). `ledger-finance` fixed this at the hold-record level:
  the real per-occurrence key is `settlement_ledger_transaction_id`, never
  `correlation_id` and never `grant_id` (`ledger-accounting-model.md`
  §7.7.2.6). Adopted verbatim here, new §16.14.
- **`ACTION_HOLD_FOR_REVIEW` contradiction** (this document's §16.7 stated
  it releases the lock; `bonus-engine`'s text was read as stating it is a
  pure money no-op). **Resolved** (new §16.16), conforming exactly to
  `ledger-finance`'s actual status vocabulary (`held` /
  `resolved_reforfeit` / `resolved_route_to_cash` / `voided_by_rollback` —
  no invented fourth "reviewing" sub-state).

**Fix Round 2, final closing pass — two more items resolved, both
`ledger-finance`/`bonus-engine`-adjacent, neither selecting G-2:**

- **Hold-capture invocation timing** (`ledger-finance`'s new finding this
  round, surfaced while fixing the `ACTION_REFORFEIT` posting-sequence
  contradiction). §16.9/§16.14 previously implied the two-leg hold-capture
  posting into `player_bonus_held` fires only once `ACTION_HOLD_FOR_REVIEW`
  specifically is the eventual outcome — flagged as an unresolved
  contradiction against `10-bonus-engine-architecture.md` §N1.4 step
  5b/5c's own text at the end of §16.7's re-walk. **Resolved this round**:
  capture is **unconditional** for every terminal-Grant win credit,
  decided before any of the three dispositions is known; see the rewritten
  §16.9 and §16.14.
- **The missing Grant-status-finalization seam** (`casino`'s own Phase 2
  finding against `bonus-engine`'s work this round, not previously fixed).
  Doc 10's value-reducing closing-event mechanism (N1.4 step 5a, and the
  `voided_by_rollback` sub-case of step 5c) asserts `G.status` flips out of
  `pending_settlement` "in the same transaction" as casino's own
  `postRollback`/settlement-timeout-sweep posting, "with no window" — but
  names no concrete call through which that transaction would actually
  reach `bonus-engine`'s Grant-status write. **Resolved this round**: new
  §16.21 names the seam, `bonusengine.RecheckGrantExposure`, symmetric to
  §16.9's `ResolveTerminalGrantCredit`.

This round adopts `ledger-finance`'s `player_bonus_held` account, the
two-leg hold-capture posting, and the widened `ResolveTerminalGrantCredit`
seam signature (§16.9, §16.14) exactly as specified in `ledger-accounting-
model.md` §7.7.2.10's contract for `casino`. Nothing in this round selects
G-2, decides §16.10.1's lock-shape ratification, or re-decides LF-10's
general (already-resolved) case — all three remain explicitly open,
restated in §16.20 and the revised §16.13.

No G-2 selection is made anywhere below. No code or migration is written.

### 16.1 The defect, confirmed against live code (not asserted from memory)

`postWin` (`orchestrator.go:838-906`) resolves the round's wallet from the
bet's own debit leg (`betWalletID`, lines 844-861: joins
`ledger_transactions`/`ledger_entries` on `correlation_id` +
`transaction_type = 'casino_bet'` + `direction = 'debit'`, excluding an
already-reversed bet) — this half is correct and is the pattern this
section extends, not replaces. But it then **discards** what the bet
actually debited: line 871 unconditionally resolves
`ledger.AccountPlayerCash` for the credit destination, regardless of
what `account_type` the bet's own debit leg actually used. Today this is
inert — `postBet` (line 748) only ever debits `AccountPlayerCash`, so the
resolved destination happens to match by construction (ADR 0025 §6: every
bet is 100% `player_cash`-funded). The defect is latent, not yet
exploitable, because no code path can post a bonus-funded casino bet at
all. It becomes live the day a bonus-funded `postBet` variant ships: a
bonus-funded bet's win would still be hardcoded to `player_cash`,
crediting unrestricted, withdrawable cash for a stake that was, by
construction, restricted, wagering-gated `player_bonus` value — exactly
the exploit `architect` flagged (task registry, finding 1).

`postRollback` (`orchestrator.go:921-1053`) has **no equivalent defect**.
`loadEntries`/inversion (lines 998-1019) reads whatever accounts the
original transaction actually touched and inverts their direction
generically — it never names an account type. It is origin-safe by
construction today and needs no design change here. This asymmetry (one
function hardcoded, the sibling function generic) is itself informative:
`postWin` is not a *new* posting mirroring the bet's own accounts (which
would need no destination decision at all) — it is a *new* transaction
crediting money the house did not previously hold, which is exactly why
it needs an explicit destination-resolution step `postRollback` never
needed.

### 16.2 Complete casino lifecycle, traced against actual code

| Transition | Function / lines | DB transaction boundary | Correlation mechanism |
|---|---|---|---|
| Launch | `LaunchGame`, `orchestrator.go:112-268` | One tx; mints `casino_launch_sessions` row, single-use opaque token (ADR 0025 §3) | `SessionID` — never reused for bet/win correlation |
| Bet | `ReceiveCallback` → `postBet`, `orchestrator.go:445-491`, `577-811` | **Its own, separate DB transaction** — one inbound webhook call, one tx | Debits resolved wallet from `event.SessionID` (never payload `PlayerAccountID`, line 647-670); posts under `roundCorrelationID(tenantID, providerID, event.RoundID)` (lines 420-422, 789) |
| Win | `ReceiveCallback` → `postWin`, lines `445-491`, `838-906` | **Its own, separate DB transaction, at an arbitrary later time** — a distinct webhook delivery, not a continuation of the bet's transaction | Resolves wallet/origin from the bet's own posted entries via the **same deterministic `correlation_id`** (§16.1) — never from its own payload's `PlayerAccountID` |
| Rollback | `ReceiveCallback` → `postRollback`, lines `445-491`, `921-1053` | Its own, separate DB transaction; row-locks the named original (`FOR UPDATE`, line 939) to serialize concurrent rollback references | Looked up by `(tenant_id, provider_id, provider_tx_id)` naming the specific original (`event.OriginalProviderTxID`) — **not** `correlation_id` (a round with both a bet and a win rolled back needs two rollback events, one per original, per §7's own doc comment) |
| Replay (redelivery) | `findPostedBetTransaction` (bet, lines 520-541, 641-645); win/rollback rely on `ledger.Post`'s own `(tenant_id, idempotency_key)` uniqueness | Idempotent no-op — see §16.2.1 for the gap this section does not create but does confirm still exists | Same `provider_id:provider_tx_id` idempotency key for every event type |
| Late callback | No distinguished handling — a win/rollback arriving after an arbitrary delay is processed identically to one arriving immediately | Whatever tx is open when the callback is finally delivered | Same correlation/idempotency mechanisms — timing is invisible to the posting logic by design (financial-transaction-flows.md's flows carry no timeout) |
| Concurrent callback | `pg_advisory_xact_lock` on `casino_bet_delivery:<tenant>:<provider>:<provider_tx_id>` serializes concurrent **bet** redeliveries only (lines 616-621); win/rollback have no equivalent named lock beyond `postRollback`'s row-level `FOR UPDATE` on the original (line 939) | — | — |
| Settlement timeout (system-initiated, new, LF-4) | **No inbound webhook at all** — no third-party event exists for a loss. §16.5a's scheduled sweep, not `ReceiveCallback`, is the only mechanism that can ever resolve a bonus-funded locked stake with no win/rollback callback | Its own, separate DB transaction per swept lock, distinct from any webhook delivery | `correlation_id` (to find the lock) plus a new `casino_settlement_timeout` transaction type (§16.5a) — **`NOT IMPLEMENTED`, requires ratification**, unlike every other row in this table |

**16.2.1 — the false premise this section corrects.** `ledger-accounting-
model.md` §6.3.3.1 states: *"casino resolves a bet atomically inside one
transaction and therefore still holds the split in memory... Casino never
needed a recovery mechanism; sportsbook cannot work without one."* This is
**factually incorrect against the code in §16.2 above** — `postBet` and
`postWin` are two separate `ReceiveCallback` invocations, each its own
database transaction, with the identical arbitrary time gap sportsbook's
lock/settlement separation has. This is not a hypothetical: it is the
entire reason `CasinoProvider.HandleCallback` exists as a webhook-style
entry point rather than a single synchronous `Bet`-then-`Win` call. §16.4
below reuses §6.3.3.1's own recovery-query mechanism for casino (per this
gate's own instruction to reuse, not invent a parallel mechanism) —
precisely because the premise that casino didn't need it was wrong, not
because a new mechanism was designed. **Routed to `ledger-finance`, not
edited here** (§16.12, item 1).

### 16.3 How a bet's economic origin is preserved without trusting the payload

The mechanism already exists and needs no new field, migration, or side
table — it is the same reading pattern Model C (`ledger-accounting-
model.md` §6.3.3.1, §6.6.6) already established for wagering-progress
nullifiability, applied here for the first time to casino:

1. **`ledger_transactions.correlation_id`** — deterministically derived
   server-side (`roundCorrelationID`, never payload-supplied as a raw
   value: it is a SHA1-namespaced UUID over `(tenant_id, provider_id,
   round_id)`, computed identically by whichever posting call sees the
   round first). A malicious or buggy `round_id` value can at most cause a
   *lookup miss* (→ `ErrBetNotFound`, already fail-closed), because the
   query in §16.4 is scoped to `tenant_id` (server-derived) and
   `transaction_type = 'casino_bet'` (fixed), not to any payload field.
   **Corrected claim (LF-7):** a prior draft of this section asserted that
   a genuine `correlation_id` collision (two different players' rounds
   hashing to the same value) would be "caught by the wallet/asset checks
   already in `postWin`." That is false against the actual code:
   `orchestrator.go:867`'s only cross-check is `wl.AssetCode !=
   event.AssetCode` — an asset-code equality check, nothing else. It does
   not compare `wallet_id`, `player_account_id`, or any other player-
   identity dimension, so it cannot detect a collision between two
   players' rounds that happen to share a currency. §16.4 now adds an
   explicit fourth query outcome — one distinct `account_type` but more
   than one distinct `wallet_id` — that aborts by name (LF-7); the asset
   check remains a separate, narrower defense that only ever catches a
   currency mismatch on the wallet the query *did* resolve, never a
   wallet-identity divergence within the query result itself.
2. **`ledger_transactions.transaction_type`** — fixes which flow posted
   the row (`casino_bet`), never inferred from payload.
3. **`ledger_accounts.account_type`** — the actual server-side ledger
   fact of which account a bet's stake came from. This is the field
   `postWin` currently ignores (§16.1) and is exactly what §16.4 reads
   instead.
4. **`NOT EXISTS (... reverses_transaction_id ...)`** — already present in
   `postWin`'s existing query (line 849), excludes a rolled-back bet from
   being treated as a valid origin for a later win.

None of `event.PlayerAccountID`, `event.AssetCode` (used only as a
*cross-check* against the wallet already resolved from ledger truth, line
867-869, never as a *resolution* input), or any other payload field is
ever used to choose an account. This is not a new design decision — it is
the existing, reviewed rule (`postWin`'s own doc comment, lines 820-829)
extended one field further (`account_type`, not just `wallet_id`).

### 16.4 `postWin`'s exact destination-resolution design (revised — LF-1, LF-7, LF-18)

**Defect in the original draft, confirmed by `ledger-finance` (LF-1) and
accepted in full.** The original design replaced `orchestrator.go:843-874`
with a query that read the bet's **debit** leg across all four
`player_*` account types, with no amount and no wallet-collision check,
and then built a destination map as if that query could return a *locked*
account. It cannot: under migration 0048's case-B lock shape — the shape
§16.10.1 requires for any future bonus-funded casino bet — the posting is
`Dr player_bonus X / Cr player_locked_bonus X`. The **debit** leg of that
transaction is `player_bonus`, never `player_locked_bonus`. The original
destination map's `player_bonus` row read "`player_bonus` → `player_bonus`,
unconditionally, no G-2 check" — so a lock-shaped bonus bet's win would
have been routed through the *ungated* row, and the G-2-gated row (keyed
on `player_locked_bonus`) could never be reached by a debit-leg query at
all. §16.7's exploit-closure proof did not hold as originally written;
it is re-walked in full below and does hold under the fix that follows.

**Fix chosen: read the credit leg for the locked case (option (a) from
the fix-wave directive), not just correct the map.** This is deliberately
**not** the minimal fix (b) — patching the map so `player_bonus` (debit)
routes to the gated row — because that minimal fix leaves a structural
ambiguity: `player_bonus` as a debit leg is legitimately produced by
*both* an ordinary immediate-absorb bonus shape (ADR 0032 §3's original,
now-disallowed-for-casino shape, §16.10.1) *and*, incidentally, by the
first half of the mandated lock shape — the debit leg alone cannot tell
these apart. Reading the **credit** leg of the lock instead answers "is
this bet currently locked, and in which account" directly, which is the
actual question `postWin` needs answered for destination purposes.

**Corrected by LF-18, Round 2 — this is not the same fix as Round 1
claimed.** Round 1's text stopped there and additionally used this same
credit-leg read's `SUM(e.amount)`, filtered to `transaction_type =
'casino_bet'` only, as "the locked stake `X`" — i.e. it answered the
destination question and the *quantity-to-release* question with the
same single query. `ledger-finance` (LF-18) found this wrong: a
bet-time-only credit-leg sum is `ledger-accounting-model.md` §6.3.3.1's
**"variant 1"** — the *original* lock amount, frozen at the moment the
bet posted. It is silently stale the instant *any* later transaction has
already touched the same locked account under this `correlation_id` — a
prior win, a prior rollback, or (the concrete exploit) §16.5a(a)'s
settlement-timeout sweep. The *quantity* question needs
**"variant 2"** instead — the same query with the `transaction_type`
filter dropped and the sum signed, netting the original lock against
*every* later transaction sharing the round — because that is the only
read that reports what is genuinely still outstanding right now. Round 1
conflated "which account is this bet's origin" (a question the bet-time
credit leg answers correctly and permanently) with "how much of that
origin is still locked" (a question only variant 2 answers correctly,
because the answer can change after bet time). The fix below splits these
into two independent reads, Step 1 (identity, permanent, variant-1
shaped) and Step 1b (outstanding amount, live, variant-2 shaped, new this
round) — never conflates them again.

**Step 1 — bet-identity resolution** (unchanged from Round 1; still the
correct, permanent answer to "which bet, which account, which wallet" —
LF-7/LF-8's outcomes 2–4 below depend on exactly this shape and are
unaffected by the LF-18 fix):

```sql
-- DESIGN ONLY. ledger-accounting-model.md §6.3.3.1 "variant 1", reused
-- verbatim (transaction_type substituted). Reads the CREDIT leg -- the
-- account the stake was ORIGINALLY posted into -- restricted to the
-- locked account types only, grouped so multi-row outcomes (LF-7/LF-8)
-- are visible to the caller rather than collapsed by a bare LIMIT 1.
-- Never reads event.PlayerAccountID or any other payload field.
--
-- IDENTITY ONLY (LF-18). This is a permanent, bet-time fact -- WHICH bet,
-- WHICH account_type, WHICH wallet -- and remains correct forever, no
-- matter what posts later. It is NEVER used as a release quantity. The
-- release quantity is Step 1b, below, and nothing in this query's result
-- set may be substituted for it.
SELECT t.id AS bet_transaction_id, la.account_type, la.wallet_id,
       la.asset_code
  FROM ledger_entries      e
  JOIN ledger_accounts     la ON la.id = e.ledger_account_id
  JOIN ledger_transactions t  ON t.id  = e.ledger_transaction_id
 WHERE t.tenant_id = $1
   AND t.correlation_id = $2   -- roundCorrelationID(tenantID, providerID, roundID)
   AND t.transaction_type = 'casino_bet'
   AND e.direction = 'credit'
   AND la.account_type IN ('player_locked_cash','player_locked_bonus')
   AND NOT EXISTS (SELECT 1 FROM ledger_transactions r
                    WHERE r.reverses_transaction_id = t.id)
 GROUP BY t.id, la.account_type, la.wallet_id, la.asset_code;
```

**Step 1b — outstanding-lock resolution (NEW, LF-18's fix).** Run only
once Step 1's result set has been narrowed, by the outcome classification
below, to exactly one `(account_type, wallet_id, asset_code)` triple —
i.e. only when identity is already unambiguous and the only remaining
question is amount:

```sql
-- DESIGN ONLY. ledger-accounting-model.md §6.3.3.1 "variant 2" -- THE
-- LF-18 FIX. Drops the transaction_type filter entirely and signs the
-- sum, so it nets the original lock against EVERY later transaction
-- sharing this round's correlation_id against this exact account --
-- casino_win, casino_rollback, casino_settlement_timeout, and any future
-- bonus-engine hold-capture/resolution posting alike, whichever of them
-- have posted so far. Answers "how much is actually still locked right
-- now", never "how much was locked at bet time" (Step 1's question).
--
-- This is the IDENTICAL shape INV-TG's own L(G) already uses (08
-- §16.10.2: "L(G) = Σ signed(player_locked_bonus) attributable to G ...
-- the §6.3.3.1/§6.6.6 recovery-query pattern, reused verbatim") -- not a
-- new invention, a second, consistent application of an invariant this
-- document already relies on elsewhere. §16.4's post-fix divergence from
-- that invariant is exactly what LF-18 found (see §16.6's revised
-- Sportsbook row and §16.17).
SELECT SUM(CASE WHEN e.direction = 'credit' THEN e.amount ELSE -e.amount END)
         AS net_outstanding_locked
  FROM ledger_entries      e
  JOIN ledger_accounts     la ON la.id = e.ledger_account_id
  JOIN ledger_transactions t  ON t.id  = e.ledger_transaction_id
 WHERE t.tenant_id      = $1
   AND t.correlation_id = $2
   -- NO transaction_type filter -- the load-bearing difference from Step 1
   AND la.account_type  = $3   -- the single account_type Step 1 identified
   AND la.wallet_id      = $4  -- the single wallet_id Step 1 identified
   AND la.asset_code     = $5; -- the single asset_code Step 1 identified
```

**Interaction with §16.4a's future `OriginatingProviderTxID` field, named
now so it is not rediscovered later.** Once that field lands and a
multi-bet round becomes resolvable per-bet rather than only via the
manual-reconciliation queue, Step 1's identity query gains `AND
t.provider_tx_id = $6` (§16.4a already specifies this for Step 1/Step 2).
**Step 1b must gain the identical `AND t.provider_tx_id = $6` clause at
the same time** — without it, a multi-bet round's Step 1b would net *all*
that round's bets' locked/settled activity together under one
`correlation_id`, silently reintroducing a cross-bet aggregation error of
the same shape LF-8 named for the identity query, one level down in the
amount query instead. This is a required extension to make at that time,
not a gap in today's design (today, outcome 3 below routes every
multi-bet round to the manual queue before Step 1b ever runs, so the two
queries cannot yet be run against an ambiguous `correlation_id`
simultaneously).

**Step 2 — direct-absorb (cash) resolution**, run only if Step 1 returns
zero rows (no lock exists for this round):

```sql
-- DESIGN ONLY. No lock found -- resolve the DEBIT leg, restricted to
-- player_cash only. player_bonus is deliberately EXCLUDED here: per
-- §16.10.1, a bonus-funded casino bet must ALWAYS lock. A bare
-- player_bonus debit leg with no matching Step-1 lock is therefore not a
-- second legitimate origin -- it is a structural inconsistency (the
-- disallowed immediate-absorb shape was posted somehow) and is detected,
-- not silently routed (see outcome table below).
SELECT t.id AS bet_transaction_id, la.account_type, la.wallet_id,
       la.asset_code
  FROM ledger_entries      e
  JOIN ledger_accounts     la ON la.id = e.ledger_account_id
  JOIN ledger_transactions t  ON t.id  = e.ledger_transaction_id
 WHERE t.tenant_id = $1
   AND t.correlation_id = $2
   AND t.transaction_type = 'casino_bet'
   AND e.direction = 'debit'
   AND la.account_type IN ('player_cash','player_bonus')
   AND NOT EXISTS (SELECT 1 FROM ledger_transactions r
                    WHERE r.reverses_transaction_id = t.id)
 GROUP BY t.id, la.account_type, la.wallet_id, la.asset_code;
```

**Shared outcome classification, applied identically to whichever step's
result set is non-empty** — evaluated in this fixed order, each a named,
fail-closed branch, none a guess. Outcomes 1–4 are Round 1's, unchanged
(they resolve *identity*, which LF-18 does not touch); outcome 5 is
Round 1's "clean case" **narrowed**, and outcome 6 is **new — this is
LF-18's required sixth branch**:

1. **Zero rows from both steps** → `ErrBetNotFound`, unchanged from today
   (no bet, or the bet was rolled back).
2. **More than one distinct `wallet_id` across the result set** →
   **new fourth outcome (LF-7).** One `account_type` naming two different
   players' wallets under the same `correlation_id` cannot be a legitimate
   multi-bet round (a wallet is scoped to one player+asset; nothing
   legitimate ever splits one round's stake across two players' wallets).
   This is either a `correlation_id` hash collision or a posting-layer
   defect — never guessed. Abort the whole win with a new, distinct
   sentinel error, `ErrCorrelationWalletCollision`, raised as a loud
   integrity/ops alert exactly like `ErrBetNotFound`. Checked **before**
   outcome 3, since a cross-wallet collision is a more severe integrity
   condition than an ordinary same-wallet multi-bet round.
3. **More than one distinct `bet_transaction_id`** (all sharing one
   wallet) → the **multi-bet-round case**, handled by §16.4a below (LF-8)
   — **not** an automatic abort-forever condition. Step 1b is **not** run
   for this outcome (see the `OriginatingProviderTxID` interaction note
   above) — resolution is deferred to a human until the round is
   disambiguated.
4. **Exactly one `bet_transaction_id`, but that single transaction's own
   result rows span more than one distinct `account_type`** → a genuine
   same-instruction mixed-origin posting. This is the case HR-2 is
   actually meant to make unreachable (a single posting instruction with
   two funding origins) — kept as a named, defense-in-depth sentinel,
   `ErrMixedFundingUnsupported`, distinct from outcome 3 precisely so an
   operator can tell "this looks like an HR-2 regression" apart from
   "this is an expected, unhandled multi-bet round."
5. **Exactly one `bet_transaction_id`, one `account_type`, one
   `wallet_id`, and Step 1b's `net_outstanding_locked > 0`** → the clean
   case. Proceed to the destination map below, using Step 1b's
   `net_outstanding_locked` — **never** Step 1's identity-only result —
   as the exact quantity §16.5a's lock-release step debits.
6. **[NEW, LF-18] Exactly one `bet_transaction_id`, one `account_type`,
   one `wallet_id`, but Step 1b's `net_outstanding_locked ≤ 0`.** A
   `casino_bet` credit leg genuinely exists — this is not outcome 1's "no
   bet at all" — yet nothing is actually left locked: some earlier
   transaction under this `correlation_id` has already driven this
   account's balance to zero (an ordinary prior win or rollback already
   correctly handled by `ledger.Post`'s idempotency key, **or** — the
   exploit LF-18 named — §16.5a(a)'s settlement-timeout sweep having
   already posted `casino_settlement_timeout` against this exact lock).
   **Abort the whole win with a new, distinct sentinel,
   `ErrLockAlreadyReleased`, raised as a loud integrity/ops alert exactly
   like outcome 2. Never release. Never post.** This is the branch that
   did not exist in Round 1 and whose absence LF-18 exploited — see
   §16.17 for the exploit walked step by step against this fix.

**Destination map** (the only part of `postWin` that changes what account
receives the credit; the posting mechanics — `ledger.Post`, idempotency
key, audit record — are unchanged). Every `locked_amount` reference below
is Step 1b's `net_outstanding_locked`, per outcome 5 — **never** Step 1's
identity-only result:

| Resolution | Win credit destination | Lock release (§16.5a)? | G-2 involved? |
|---|---|---|---|
| Outcome 5: `player_locked_cash` | `player_cash` | Yes — `Dr player_locked_cash net_outstanding_locked / Cr player_cash net_outstanding_locked`, bundled in the same transaction | No — G-2 is defined only over `player_bonus`/`player_locked_bonus` (doc10 §T.7). Not producible by today's `postBet` (§16.10); included for completeness only |
| Outcome 5: `player_locked_bonus`, Grant non-terminal | `player_bonus` | Yes — `Dr player_locked_bonus net_outstanding_locked / Cr player_bonus net_outstanding_locked`, bundled in the same transaction | No — ordinary case. This is the shape LF-1 restores as actually reachable |
| Outcome 5: `player_locked_bonus`, Grant terminal | §16.9's seam **always** captures into `player_bonus_held` (§16.14) — never `ACTION_REFORFEIT`/`ACTION_ROUTE_TO_CASH`/`ACTION_HOLD_FOR_REVIEW` decided inline; those three are a **later**, separate resolution (§16.16) | Yes, bundled into the same unconditional two-leg hold-capture posting (§16.5a, §16.14) | **The credit enters G-2's held state here, but nothing about G-2 is decided at this call site — capture is technical and unconditional (§16.9). G-2's own selection happens only later, in a separate resolution transaction, if/when a human resolves the `held` record (§16.16)** |
| Step 2: `player_cash` | `player_cash` | No — never locked | No. **Unchanged** from today's actual behavior for every bet `postBet` can currently produce |
| Step 2: `player_bonus`, no Step-1 lock found | **Abort** — new sentinel `ErrBonusBetNotLocked`, integrity alert | N/A | N/A — under the mandated shape (§16.10.1) this is unreachable in correctly-functioning code; never silently credited, since guessing its destination is exactly the exploit this section closes |
| **Outcome 6 (LF-18): identity found, `net_outstanding_locked ≤ 0`** | **Abort** — `ErrLockAlreadyReleased`, integrity alert | **No — never. This is the branch that prevents a second release** | N/A — never reaches a G-2 read; this is a pure integrity abort on the *amount* dimension, orthogonal to whichever Grant status a live read would have found |

A `default`/unmatched case in the eventual Go `switch` **must** return an
error, mirroring the exhaustive-classification discipline
`ledger-accounting-model.md` §6.6.5 already established for the identical
reason (an allowlist-with-silent-fallback fails open).

### 16.4a Multi-bet rounds: the legitimate case HR-2 does not cover (LF-8)

**Corrected claim.** The original draft asserted ">1 distinct
`account_type` [is] structurally unreachable by HR-2." This is wrong.
HR-2 (`ledger-accounting-model.md` §6.5's fail-closed rejection at
placement) blocks a **single posting instruction** that would fund one
bet from two origins at once. It does not, and structurally cannot, block
**two separate `casino_bet` transactions**, each internally single-origin,
that share one round-level `correlation_id` — nothing in `postBet`
prevents a provider from sending two bet callbacks naming the same
`RoundID` (a multi-bet round, a re-bet, or a side bet), and nothing
requires the two bets to share a funding origin. This is a legitimate,
reachable shape today, independent of any bonus-funded work: outcome 3 in
§16.4's classification (`>1` distinct `bet_transaction_id`) generalizes
LF-8's own literal ">1 distinct `account_type`" trigger to the actual
underlying condition — even two *same-origin* separate bets under one
round are equally ambiguous for the purpose that matters here, which is
resolving a **specific bet's own stake** for lock-release/credit purposes,
not merely picking a destination account type.

**The real gap, confirmed against actual code, not assumed.**
`internal/casino/types.go`'s `WinRequest`/`CallbackEvent` carries only
`RoundID` — no bet-level identifier equivalent to
`RollbackRequest.OriginalProviderTxID`, which exists for exactly this
purpose on the rollback side. A win event today cannot say which specific
bet within a multi-bet round it is paying out. This is a genuine
provider-protocol/schema gap, not something `postWin`'s query logic alone
can resolve by being cleverer.

**Design, concrete enough to build once the schema gap is closed.** Add
an `OriginatingProviderTxID` field to `WinRequest`/`CallbackEvent`,
mirroring `RollbackRequest.OriginalProviderTxID`'s existing naming and
precedent exactly, populated whenever the provider's own win event names
the specific bet it settles (the norm for real aggregator protocols that
support multi-bet rounds, not the exception). When present, both of
§16.4's queries add `AND t.provider_tx_id = $3` (`ledger_transactions`
already stores `provider_tx_id`; no new column) — this scopes resolution
to the exact bet's own ledger rows and the multi-bet-round case stops
being ambiguous at all, resolved per-bet rather than per-round.

**Until that field exists** (true of every mock/sandbox this platform
runs today, all of which only exercise 1:1 bet:win rounds): a win event
that resolves to outcome 3 (`>1` distinct `bet_transaction_id`) is
**neither silently guessed nor a permanent black hole**. It aborts that
one win with a new, distinct sentinel error, `ErrAmbiguousMultiOriginRound`,
routed to a named manual-reconciliation queue (not a generic failure log)
— an ops reviewer inspects the round's actual bet legs against the
provider's own round detail and posts the correct settlement through the
existing four-eyes, reason-coded manual-adjustment path (CLAUDE.md's
compensating-entry rule), rather than the platform inferring one.
Amount-matching heuristics (e.g., "the win amount equals one specific
bet's stake") are explicitly rejected as unsafe guessing, per CLAUDE.md's
fail-closed financial-write rule — never implemented as a shortcut around
the manual queue.

**Named as a required future dependency, not decided here:** until
`OriginatingProviderTxID` (or equivalent) is added to the win protocol and
adopted by whichever provider adapter is built, no multi-bet-capable game
title should be marked launch-eligible in the catalogue for a tenant with
bonus-funded wagering enabled — a catalogue/launch-eligibility
consideration flagged for `product-owner-proxy`/`architect` scoping, not
a code change and not decided by this section.

### 16.5 Resolution mechanism per funding case

| Case | Mechanism | G-2 involved? |
|---|---|---|
| Cash-funded wager | §16.4 origin = `player_cash`/`player_locked_cash` → credit `player_cash` | No — G-2 is defined only over `player_bonus`/`player_locked_bonus` credits (doc10 §T.7) |
| Bonus-funded wager, Grant non-terminal | §16.4 origin = `player_locked_bonus` → live `FOR UPDATE` read of `G.status` under the `(tenant_id, grant_id)` advisory lock (doc10 §9) finds a non-terminal status → credit `player_bonus` normally | No — this is the ordinary case; G-2 is specifically about a **terminal** Grant |
| Bonus-funded wager, Grant already terminal | §16.9's seam is invoked — **always** performs the unconditional hold-capture posting (§16.14); never decides among the three G-2 actions inline | G-2-relevant (the credit enters the `held` state), but **not decided here** — the three actions are a later, separate resolution (§16.16) |
| Locked cash | Same row as cash-funded above; a lock has no Grant, so no G-2 dimension exists | No |
| Locked bonus | Same row as "Grant non-terminal"/"Grant terminal" above — "locked bonus" *is* the `player_locked_bonus` origin case, not a fifth case | See above |
| Mixed funding (single-instruction, HR-2 regression) | §16.4's outcome 4 (`ErrMixedFundingUnsupported`) — whole transaction aborts, nothing posted | N/A — rejected before any credit is considered |
| Multi-bet round (legitimate, LF-8) | §16.4's outcome 3 → §16.4a's manual-reconciliation path, **not** an automatic permanent abort | Deferred to manual review; may involve G-2 once disambiguated |
| Correlation-ID wallet collision (LF-7) | §16.4's outcome 2 (`ErrCorrelationWalletCollision`) — whole transaction aborts, integrity alert | N/A — rejected before any credit is considered |
| Rollback (of an already-resolved win) | `postRollback`'s existing generic entry-inversion (§16.1) — reverses whatever the original actually posted, origin-safe by construction. **A rollback of a WIN that already credited `player_bonus`, where the Grant has since gone terminal and swept that balance into `promo_liability` (§16.10), can find insufficient `player_bonus` balance to debit.** What happens then — permit the negative balance as a recorded clawback, route the shortfall to a receivable, or reject-and-alert — is **`ledger-finance`'s decision, not casino's** (LF-10, still open per `ledger-accounting-model.md` §7.7.2.11 — see §16.20); this document does not pre-select an answer (§16.11, §16.12) | Not casino's call — see note |
| Rollback (of a still-`held` win) | **Closed this round.** `ledger-finance`'s guarded compare-and-swap (§7.7.2.7, adopted in new §16.15) — the generic entry-inversion above, plus an atomic `UPDATE bonus_held_dispositions SET status='voided_by_rollback' ... WHERE status='held'` in the same transaction. Zero rows affected is the loud failure signal if the record already left `held` | No — a rollback is never a G-2 answer, it is a technical undo |
| Loss (no callback exists) | No inbound event signals a loss at all (§16.2). §16.5a designs the resolution mechanism a locked bonus-funded stake requires | No — value-reducing, never gated by G-2 (§16.8) |
| Late settlement | Timing is invisible to §16.4's query — a win arriving an hour or a month after its bet resolves identically. Only the *outcome* of the live Grant-status read at settlement time can differ, which is exactly G-2's own subject matter, not a separate "lateness" mechanism | See above |
| Idempotent replay | Unaffected by this section — `ledger.Post`'s existing `(tenant_id, idempotency_key)` uniqueness already no-ops a redelivered win before any destination decision is re-evaluated. §16.5's own concern (destination) is decided once, at first delivery, and never re-decided on replay | No |

### 16.5a Lock release and loss-settlement resolution (LF-4)

**The gap, confirmed.** The original design changed the win's credit
*destination* but never specified debiting the lock itself — `L(G)` (the
locked-bonus component of open exposure, §16.10.2) could never reach zero
on a win, and casino has **no loss callback at all** (§16.2's lifecycle
table: only bet/win/rollback exist), so a losing bonus-funded round left
`player_locked_bonus` permanently nonzero with no event to post the
loss-absorption case against. Both halves are fixed here.

**Win case — falls out of §16.4's fix, with one addition, and revised
this round (LF-18) to source the release quantity correctly.** Once
§16.4 correctly resolves a `player_locked_bonus` origin (outcome 5) and
Step 1b's `net_outstanding_locked` (**not** Step 1's identity-only
result — LF-18), the settlement transaction bundles **two** entry pairs
in one `ledger.Post` call (a single balanced transaction, `ledger.Post`'s
existing multi-entry-pair support — no new capability). Let `X =
net_outstanding_locked` (Step 1b) and `W` = the win's own payout amount
(from the provider's callback, cross-checked, never trusted blindly for
destination — §16.3):

1. The ordinary win payout: `Dr house_gaming W / Cr player_bonus W` (Grant
   non-terminal) or, for a **terminal** Grant, `Dr house_gaming W / Cr
   player_bonus_held W` — **always**, unconditionally, regardless of which
   of the three G-2 actions this credit will eventually receive
   (§16.9/§16.14's corrected framing, closed this round — see the note
   below).
2. The lock release: `Dr player_locked_bonus X / Cr player_bonus X` (Grant
   non-terminal) or, for a terminal Grant, `Dr player_locked_bonus X / Cr
   player_bonus_held X` — again always, in the same posting as 1, never
   conditioned on a disposition.

This closes `L(G)` to zero on every win, including the terminal-Grant
branch — **explicitly required by this fix, not left to `bonus-engine`**:
per the fix-wave directive, `internal/casino` does not need to design
`bonus-engine`'s own holding mechanism, but it **does** need the lock
itself to resolve rather than dangle, on every win, unconditionally —
capture into `player_bonus_held` is *itself* that resolution for a
terminal Grant; it does not wait for G-2 to be answered `ACTION_HOLD_FOR_
REVIEW` or anything else. **Cross-dependency found during Round 1, closed
this round (§16.14):** the seam must name a destination for the *released
lock amount* `X` (distinct from wherever it holds the *payout* `W`) —
`ledger-finance`'s `player_bonus_held` account (§7.7.2.2) is that
destination for both, in one balanced transaction, adopted in full in
§16.14 below.

**The hold-capture posting — adopted this round from `ledger-finance`'s
§7.7.2.2/§7.7.2.10 contract, in full, in place of the suspense/hold
placeholder Round 1 left open, and corrected this round (Fix Round 2,
final closing pass) to be unconditional rather than conditioned on
`ACTION_HOLD_FOR_REVIEW` specifically.** See §16.9 for the seam's
corrected invocation semantics, §16.14 for the exact two-leg posting
shape, §16.15 for the rollback-of-a-held-win transition, and §16.16 for
the resolution of the `ACTION_HOLD_FOR_REVIEW` lock-releases-but-doesn't-
spend contradiction. `ACTION_HOLD_FOR_REVIEW` itself is not a posting at
all (§16.16) — it is simply the name for "the `held` record has not yet
been resolved," so it cannot be what gates whether the posting above
occurs.

**Loss case — the harder, previously entirely unaddressed gap.** No
inbound event signals a loss. Two designs, per this fix-wave's own (a)/(b)
menu:

- **(a) Bounded, per-provider/per-jurisdiction-configurable settlement
  window — RECOMMENDED.** A scheduled sweep (operationally the same
  pattern as the existing hourly reconciliation sweep, not a new
  architectural primitive) finds `player_locked_bonus` locks — identified
  by a `casino_bet` transaction with a Step-1-shaped identity (§16.4) whose
  Step-1b-shaped `net_outstanding_locked` is **still `> 0`** — that are
  **both** (i) older than a configured window `W` and (ii) genuinely
  unresolved: no `casino_win` or `casino_rollback` transaction exists
  under the same `correlation_id`, **and** no `ErrTerminalGrantCreditUnresolved`
  rejection (§16.9) was ever recorded for it (see the exclusion below —
  found during the §16.7 re-walk, not assumed at the outset). For each
  genuinely unresolved, expired lock, the sweep posts a new,
  system-initiated transaction type, `casino_settlement_timeout`,
  resolving the stake as a loss with the identical case-I accounting
  shape a real loss would use: `Dr player_locked_bonus
  net_outstanding_locked / Cr house_gaming net_outstanding_locked`
  (Step 1b's amount, read fresh at sweep time — never a cached bet-time
  figure), mirrored per the existing `BONUS_SET`
  rule (B1 extended) exactly as an ordinary loss would be — no new
  posting shape, reused verbatim.

  **Grant-status finalization, closed this round (§16.21).** Each swept
  lock's `casino_settlement_timeout` posting is exactly the kind of
  value-reducing closing event that can clear a Grant's last outstanding
  `AOE` component. Per Grant, in the **same** transaction as that Grant's
  own `casino_settlement_timeout` posting, and only if the live
  Grant-status read finds it `pending_settlement`, the sweep calls
  `bonusengine.RecheckGrantExposure(ctx, tx, grantID,
  triggeringLedgerTransactionID, TriggerCasinoSettlementTimeout)` (§16.21)
  before committing that Grant's batch — the sweep does not itself decide
  whether this was the last remaining exposure; it only ensures
  `bonus-engine` gets the chance to decide, in the same transaction, every
  time it might have been.

  Required properties, per the fix-wave directive:
  - **Per-tenant and per-jurisdiction configurable**, jurisdiction-primary
    and tenant/brand tighten-only — the same pattern already established
    for `OpenBetSelfExclusionPolicy` (ADR 0034 §14) — since "how long can
    a bonus-funded stake legitimately go unresolved" is a regulatory
    question, not a product-taste one.
  - **Fail-closed default: sweeping is disabled absent an explicit
    configured window.** No default duration is invented here. Absent
    config, locks accumulate, visible via `LockedBonusBalance`
    (migration 0048) and the hourly reconciliation sweep, rather than a
    guessed number wrongly writing off a stake that may still be
    genuinely in flight — mirroring CLAUDE.md's "absent config = deny"
    convention, already cited in this document's own Asset Authorization
    discussion (§16.6).
  - **Alerting integrity event on a late win.** If a genuine `casino_win`
    callback for an already-swept round arrives after `W` has elapsed, it
    is **not** silently re-posted or used to reverse the timeout posting
    automatically — that could double-count or resurrect a Grant
    sub-state that has already finalized. It raises a new, distinct,
    loud integrity alert (mirroring `ErrBetNotFound`'s framing) and is
    held for manual reconciliation. This is an accepted, real trade-off
    of the window design (a genuine late win becomes an ops incident, not
    a silent loss of player funds) and is why the window duration is a
    per-jurisdiction operational-tuning decision, not a ledger-correctness
    one.
  - **Exclusion, found while re-walking §16.7 (not previously stated):**
    the sweep must **never** treat a round whose win callback already
    arrived and was rejected via `ErrTerminalGrantCreditUnresolved`
    (§16.9) as a silent, no-signal loss — that round had a genuine win,
    only blocked pending a human G-2 decision. Such rounds are excluded
    from the sweep's candidate set entirely (matched by the recorded
    rejection audit event, not by timing) and remain in
    `pending_settlement` until a human resolves G-2, never auto-resolved
    as a loss.
  - Every timeout posting is audit-recorded identically to a normal
    settlement (actor = system, action = `casino_settlement_timeout.posted`,
    full metadata) — CLAUDE.md's audit-on-every-mutating-financial-action
    rule applies unchanged.
  - Requires no change to the provider-facing wallet-callback contract —
    entirely internal, which is why it is recommended over (b).

- **(b) Provider-protocol extension: an explicit round-close/loss-
  confirmation callback** (a fourth inbound event type, e.g.
  `CallbackEventRoundClose`). More precise — a provider-attested
  resolution moment rather than an inferred timeout — but not a universal
  casino wallet-callback primitive: many real aggregators do not send one
  (a loss is implicit-by-silence in most wallet-callback specs, which is
  the entire reason this gap exists). Named here as a **future
  dependency**, not designed further — this document cannot confirm any
  specific aggregator offers this callback without a confirmed commercial
  relationship (CLAUDE.md's mocks-only boundary), and building against an
  assumed protocol feature no contracted provider has confirmed would be
  exactly the kind of invented-ahead-of-the-real-docs work this project
  avoids.

**Recommendation: (a).** It needs no provider cooperation (safe against
every aggregator regardless of what protocol primitives it exposes,
including every mock/sandbox this platform runs today, none of which
expose either primitive), it reuses existing case-I loss accounting and
the existing reconciliation-sweep operational pattern rather than
inventing a new posting shape, and its one real cost — a bounded window
during which a genuinely late win becomes an alerted incident rather than
a silent auto-loss — is bounded and tunable per jurisdiction. (b) remains
a valid future enhancement (a faster-resolving path that could race the
timeout sweep) if a specific contracted aggregator is later confirmed to
offer an explicit round-close signal; nothing here precludes adding it
later.

**Explicit statement this fix-wave directive requires:** without either
(a) or (b) actually built, the lock-shaped bonus-funded casino wagering
design described in §16.10.1 is **NOT SAFE to implement** —
`ledger-finance`'s own characterization of the lock shape as
"conditionally ratifiable only as the package described" includes this
resolution mechanism as a required component, not an optional
enhancement. This section recommends (a) but does not authorize building
it: it remains `NOT IMPLEMENTED`, requires the same `ledger-finance`/
`architect` ratification as §16.10.1's posting-shape finding (the two are
two halves of one required package, §16.13), and additionally requires a
new human/jurisdiction decision — the actual per-jurisdiction window
durations — that this section does not select.

### 16.6 Cross-domain reconciliation

| Domain | Requires a change? | Detail |
|---|---|---|
| **Ledger** | No schema change. `ledger.Post`, `GetOrCreateAccount`, the idempotency key shape, and HR-9's fail-closed guard are all reused unmodified. §16.4's query is new Go code inside `internal/casino`, not a ledger API change | — |
| **Wallet** | No change | `wallet.GetByID`/`GetSummary` already expose all four account types (migration 0048, `IMPLEMENTED`) |
| **Bonus Grant** | **Requires new plumbing, not yet built**: a Grant-status read callable from `internal/casino` (§16.9's seam) and, if §16.10's recommendation is accepted, a new `pending_settlement`-shaped Grant sub-state | See §16.9/§16.10 |
| **Wagering Progress (Model C)** | **Requires a correction, not a design change**: §6.6.6's "for casino the quantity is always zero, so `P_firm == P_net`" claim shares §16.2.1's false premise and is incorrect for the identical reason once a bonus-funded casino bet exists — casino's `player_locked_bonus` exposure is nonzero between `postBet` and `postWin`/`postRollback`, exactly like sportsbook. §6.6.5's classification table already lists `casino_bet`/`casino_win`/`casino_rollback` correctly (risk-preserving/nullifying, identical to the sportsbook types) — only §6.6.6's summary sentence is wrong. **Routed to `ledger-finance`, not edited here** (§16.12, item 2) | |
| **Bonus Conversion** | No change. §16.4/§16.9 never touch `bonus_conversion`; a terminal Grant cannot convert regardless (§6.6.6, `P_firm` for a terminal Grant authorizes nothing) | — |
| **Casino (this domain)** | Yes — this section's own deliverable. `postWin`'s two hardcoded lines change; `postBet`/`postRollback` are unaffected | — |
| **Sportsbook** | **Round 1's "no divergence" claim was wrong, and LF-18 is exactly where the divergence lived — re-examined here as this fix-wave directive requires.** Round 1's §16.4 reused §6.3.3.1's variant-1 query for a purpose §6.3.3.1 itself warns against conflating (§6.3.3.1's own text: "using (1) where (2) is meant would over-release"). That is precisely what happened: Round 1 used variant 1 (the original split) where variant 2 (the currently-remaining amount) was required, for the exact reason §6.3.3.1 names. **Corrected this round**: §16.4 Step 1 (identity, variant-1-shaped) is unchanged and legitimately variant-1 — it answers "what was the original split," which is a permanent fact and the correct question for *identity*. §16.4 Step 1b (new) is variant-2-shaped, used only for the *amount* question, exactly as §6.3.3.1 itself prescribes. Casino now applies both variants exactly as sportsbook's own document defines them, for exactly the questions each variant is meant to answer — no remaining divergence, and §16.9's seam is unaffected (still product-agnostic) | **Was a real divergence (LF-18); closed this round by applying both of §6.3.3.1's variants for their own stated purposes, not by inventing a casino-specific rule** |
| **Risk** | No change. `risk.Evaluate` is not consulted by `postWin` today (doc comment, lines 830-837) and this section does not add a call — a terminal-Grant credit decision is not a Risk-shaped exposure/limit question | — |
| **RG** | No change. `postWin` deliberately does not call `evaluateAndAuditEligibility` (unchanged rationale, lines 830-837) — settling an already-legitimately-placed bet is not gated by the player's current RG status. G-2's own resolution (whichever action a human selects) may itself have RG/AML implications (a routed-to-cash credit becoming withdrawable) but that is Decision 2's own province, not a new RG call site this design adds | — |
| **Reconciliation** | No change to the hourly ledger-vs-projection sweep mechanism. A held/failed-closed G-2 credit (§16.9) must not create an unreconciled ledger residue — see §16.13's open item on the holding mechanism, explicitly deferred by doc10 §T.7 itself to `ledger-finance`. **New (LF-4):** §16.5a's settlement-timeout sweep is a second, distinct scheduled job operating on the same locked-balance projections — it must run and be reconciled independently of the hourly ledger-vs-projection sweep, and its own postings (`casino_settlement_timeout`) are ordinary, audited ledger transactions the existing sweep already covers without modification | Open item (holding mechanism) plus one new, not-yet-authorized mechanism (settlement-timeout sweep, §16.5a) |

### 16.7 Exploit closure proof — a bonus-funded win can never become unrestricted cash before wagering/conversion rules permit it (re-walked, LF-1; amount references updated for LF-18 — see §16.17 for the LF-18 exploit itself)

**This proof did not hold as originally written.** LF-1 found that the
original query/destination-map mismatch meant branch 2 below was
structurally unreachable — a bonus-funded win posted through the
*ungated* `player_bonus` row regardless of Grant status, so §16.8's "G-2
is reached exactly once" was false as designed; it was reached zero
times. The walkthrough below is re-derived line by line against §16.4's
fixed resolution mechanism and §16.5a's lock-release addition, not
re-asserted from the original text.

**Walkthrough.** Suppose a future `postBet` variant posts a bonus-funded
bet: `Dr player_bonus X / Cr player_locked_bonus X` (case B, no mirror —
§16.10 explains why this shape, not ADR 0032 §3's immediate-absorb shape,
is required). The player later wins. Two exhaustive branches:

1. **Grant `G` is non-terminal at settlement time** (the overwhelmingly
   common case). §16.4 Step 1 finds the credit leg's identity —
   `player_locked_bonus`, wallet `w` — this is now actually reached,
   because Step 1 reads the credit leg, not the debit leg the original
   design read. Outcome 5 fires (net_outstanding_locked > 0), so Step 1b
   resolves `net_outstanding_locked = X` — **the currently outstanding
   amount, confirmed nonzero, not the possibly-stale bet-time figure**
   (LF-18). §16.5's "Grant non-terminal" row applies: the live `FOR UPDATE`
   Grant-status read finds `G` non-terminal, so the win posts **both**
   entry pairs from §16.5a — (i) the payout `Dr house_gaming W / Cr
   player_bonus W`, and (ii) the lock release `Dr player_locked_bonus X /
   Cr player_bonus X` — in one balanced transaction. The ordinary case-G
   mirror pair (`bonus_expense`/`promo_liability`) posts against (i) only;
   (ii) is `BONUS_SET`-neutral bookkeeping (§16.10.3) and mirrors nothing.
   `L(G)` is now `0` for this stake (LF-4 closed), and the resulting
   `player_bonus` balance remains **restricted, non-withdrawable value
   subject to `G`'s wagering requirement** — invariant W1
   (`ledger-accounting-model.md` §6.6.6) already forbids any conversion
   until `P_firm ≥ T`, and `P_firm` cannot count a still-nullifiable
   contribution. No path from this branch reaches `player_cash` except
   through the existing, already-reviewed `bonus_conversion` posting
   (ADR 0032 §4), which is itself gated by W1. **Never `player_cash`
   directly, at any point. Holds.**
2. **Grant `G` is already terminal at settlement time.** §16.4 Step 1
   still resolves the same identity — credit leg `player_locked_bonus`,
   wallet `w` — and Step 1b still resolves `net_outstanding_locked = X`
   (unchanged from branch 1 — the *origin*/*amount* resolution never
   depends on Grant status, only the *destination* decision does — and,
   unlike the original design, this resolution path is actually reachable
   now that it keys off the credit leg rather than a debit leg the
   destination map mis-mapped). §16.5's "Grant terminal" row applies:
   the credit is **not** posted to either `player_bonus` or `player_cash`
   by `postWin` itself — it is handed to §16.9's seam.

   **Today, before `internal/bonus` exists** (`bonusengine.
   ResolveTerminalGrantCredit` does not exist, and the `player_bonus_held`
   account type itself does not exist either, HR-9-gated) — the whole
   win-posting transaction is rejected with `ErrTerminalGrantCreditUnresolved`,
   a distinct, loud, alerting error (mirroring `ErrBetNotFound`'s
   "integrity alert, not a routine failure" framing, §7), and — per
   §16.5a's exclusion, found during this re-walk — that rejection is
   itself recorded so the settlement-timeout sweep (§16.5a(a)) can never
   later misclassify this genuine win as a silent loss. **Nothing posts.
   No money moves.** `G` remains in `pending_settlement` (§16.10.2) with
   `L(G) = X > 0` — a real, disclosed residual.

   **Once the seam exists** (§16.9, §16.14, corrected this round —
   Fix Round 2, final closing pass): capture is **unconditional**, decided
   the instant this branch is reached, **before** any of the three G-2
   actions is known, let alone selected. `ResolveTerminalGrantCredit`
   posts, in the same transaction as the rest of the settlement, `Dr
   house_gaming W / Cr player_bonus_held W` (always) plus `Dr
   player_locked_bonus X / Cr player_bonus_held X` (since the stake was
   locked) — §16.14's two-leg hold-capture posting, in full. `L(G) → 0`
   immediately, on capture, regardless of which G-2 action this credit
   will eventually receive. **Nothing here waits for a human decision, and
   nothing here is "the fail-closed behavior retried once G-2 is
   answered" — capture is TECHNICAL, exactly like an ordinary win, once
   the mechanism exists; only what happens *afterward* to the captured
   value is POLICY-DEPENDENT.** `bonus-engine` also writes a
   `bonus_held_dispositions` row, `status = 'held'`, in the same
   transaction (§16.14).

   The three G-2 actions never fire inline here at all — they are a
   **later, separate resolution transaction** (doc10 §N1.4 step 5c,
   adopted in §16.16), keyed by the `bonus_held_dispositions` row's own
   `id`, run whenever a human actually resolves it (which may be seconds
   or months after capture, or never): `ACTION_REFORFEIT` posts `Dr
   player_bonus_held (W+X) / Cr promo_liability (W+X)` — **never via
   `player_bonus`, not even transiently** (corrected in Round 2: this
   sentence formerly read "releases into `player_bonus` then immediately
   re-forfeits," which `ledger-accounting-model.md` §7.7.2.9/HR-25 has
   confirmed is wrong and withdrawn); `ACTION_ROUTE_TO_CASH` posts `Dr
   player_bonus_held (W+X) / Cr player_cash (W+X)` — the **only** route by
   which a bonus-origin stake ever reaches `player_cash` directly, gated
   entirely behind a human decision, never `postWin`'s default, and never
   something `postWin`/capture itself performs; `ACTION_HOLD_FOR_REVIEW`
   is not a posting at all — it is simply the record staying `held`,
   i.e. no resolution transaction has run yet (§16.16). In every case,
   resolution **moves** an already-`player_bonus_held`-resident value
   onward — it never posts the win credit for the first time (doc10 §N1.4
   step 5c, quoted verbatim). **No withdrawable cash is ever created until
   a human selects and `bonus-engine` runs one of `ACTION_REFORFEIT`/
   `ACTION_ROUTE_TO_CASH`. Holds.**

   **Why unconditional capture, not capture-on-`ACTION_HOLD_FOR_REVIEW`,
   is correct — reasoning, not assertion.** An earlier reading of this
   section (Round 2, pre-closing-pass) implied capture happens only when
   the eventual disposition turns out to be `ACTION_HOLD_FOR_REVIEW`
   specifically — i.e., that `ACTION_REFORFEIT`/`ACTION_ROUTE_TO_CASH`
   post directly to their final destination the moment a human decides,
   with no intervening capture step at all. This was flagged as an
   unresolved contradiction against `10-bonus-engine-architecture.md`
   N1.4 step 5b, which requires the hold-capture posting **unconditionally,
   for every** value-creating credit reaching a terminal Grant, before any
   disposition is known, and whose step 5c states resolution "never posts
   the win credit for the first time... for all three actions." The two
   models are genuinely different, not a wording variance: conditional
   capture means the credit sits **nowhere in the ledger** between
   settlement and a human's eventual decision (an unbalanced, unrecorded
   gap the moment the win callback is accepted but not yet resolved);
   unconditional capture means the credit is posted, balanced, and
   auditable in `player_bonus_held` from the instant settlement completes,
   with only its *final resting place* still undecided. Given the entire
   reason `player_bonus_held` and `bonus_held_dispositions` exist —
   closing the original P0 (LF-2: a terminal-Grant win credit reaching a
   player-visible balance before a human decides its disposition) — a
   design that leaves the credit unposted anywhere until a human acts
   reopens exactly that gap for every credit awaiting `ACTION_REFORFEIT`
   or `ACTION_ROUTE_TO_CASH`, not only ones awaiting `ACTION_HOLD_FOR_
   REVIEW`: it would mean `postWin`'s own transaction either (a) blocks
   indefinitely on a synchronous human decision, which no financial
   settlement callback can safely do, or (b) fails closed and is retried
   later, in which case the "win happened" fact is not durably recorded
   anywhere until that retry succeeds — a redelivery/crash window with no
   ledger-visible trace of the win at all. `ledger-finance`'s own §7.7.2.2/
   §7.7.2.10 contract settles this: it requires `casino` to "post the
   hold-capture transaction... in the same `ledger.Post` call as the rest
   of the settlement, never as a second transaction" (§7.7.2.10 item 1,
   with no conditional on which action later applies), and doc10 N1.4 step
   5b is equally unconditional. **Unconditional capture is therefore not
   merely "very likely intended" — it is what both of this document's own
   upstream dependencies already specify; this document's prior
   conditional framing was the outlier, now corrected.**

In neither branch does `player_cash` receive value that traces back to a
bonus-origin stake without first passing through either (a) the existing,
wagering-gated `bonus_conversion` posting, or (b) a not-yet-selected,
not-yet-built G-2 action the human has not authorized. The specific
exploit `architect` flagged — `postWin`'s hardcode routing a bonus-funded
win straight to `player_cash` — is closed by §16.4's fixed resolution
alone, **unconditionally, independent of G-2's eventual answer**: even if
G-2 is one day answered `ACTION_ROUTE_TO_CASH` for a given held record,
that routing only ever fires through the later, separate resolution
transaction §16.9/§16.16 name — never inline in `postWin`, and never as
`postWin`'s default behavior for an ordinary, non-terminal settlement.
`postWin`'s own, unconditional act is capture into `player_bonus_held`
(§16.9/§16.14) — a disjoint, non-spendable account — never a direct
credit to `player_cash`, regardless of which G-2 action a human eventually
selects.

**What this re-walk changed versus the original proof:** branch 2 is now
actually reachable (LF-1); both branches now explicitly close `L(G)` to
zero, or explain precisely why it stays open and how it will close
(LF-4); and one new, previously-unnamed cross-dependency surfaced
(`ACTION_HOLD_FOR_REVIEW`'s lock-release destination, §16.5a) rather than
being silently assumed away.

### 16.8 Every place G-2 is reached, enumerated (re-confirmed, LF-1)

Given §16.10's recommended posting shape (bonus-funded casino bets always
lock, never immediately absorb) **and** §16.4's fixed resolution
mechanism (LF-1) — the original debit-leg query made this claim false in
practice (reached zero times, not once, per LF-1); it is re-confirmed
true here against the corrected mechanism — G-2 is reached **exactly
once per successfully-resolved win event**, structurally, in this domain:

1. **`postWin`, at the destination-resolution step (§16.4), when Step 1
   resolves a single, unambiguous `player_locked_bonus` origin and the
   live, `FOR UPDATE` Grant-status read finds `G.status ∈ {expired,
   cancelled, forfeited}` (or, under §16.10's refined invariant,
   `G.status = pending_settlement` and the inbound event is the specific
   value-creating credit that triggered it).** This is the sole call
   site. It presupposes §16.4 did not first abort via
   `ErrCorrelationWalletCollision` (LF-7) or route to §16.4a's
   manual-reconciliation path (LF-8) — those cases defer resolution
   entirely, including any G-2 read, until a human has disambiguated
   which specific bet the win event names; the eventual re-attempt, once
   disambiguated, reaches this same call site exactly once, as above.

Two adjacent, deliberately-**not**-G-2 cases, named so they are never
confused with it (mirroring doc10 §T.12's own distinction):

- A **loss** settling a locked bonus-funded stake (`player_locked_bonus`
  absorbed into `house_gaming`, case I) is value-**reducing** and is
  never gated by G-2 at all, terminal Grant or not (§16.10's B0/L0 split,
  §T.5.1's asymmetry). **Correction (LF-4):** it does not "always post
  immediately" as the original text claimed — casino has no loss
  callback, so it posts only once §16.5a's settlement-timeout sweep (or,
  if built later, a provider round-close signal) resolves it; the
  correction is to *when* it posts, not to whether G-2 applies (it never
  does).
- A **rollback of the lock itself** (the bet is voided/rolled back before
  any win/loss is known) returning `X` to `player_bonus` is, per §16.10's
  analysis, a candidate for the **same** immediate/uncontested treatment
  as a loss (it is bookkeeping-neutral within the `BONUS_SET` aggregate,
  restoring rather than creating value) — **flagged as a finding for
  `bonus-engine`/`architect` reconciliation, not decided here**, because
  doc10 §T.7's own trigger-condition text currently folds this case into
  the same G-2 bucket as a WIN settlement without distinguishing them
  (§16.10.3). **Independent of how that tension resolves**, this rollback
  is, either way, a value-reducing closing event under doc10 N1.4 step 5a
  — so whichever posting shape §16.10.3 eventually settles on, `postRollback`
  must call §16.21's `RecheckGrantExposure` seam in the same transaction
  whenever the live Grant-status read finds `pending_settlement`, exactly
  as the settlement-timeout sweep now does (§16.5a(a)).

`postRollback` itself never reaches G-2 for a rollback **of a win** that
already credited `player_bonus` before the Grant went terminal — that
credit already passed through G-2 (or was never bonus-funded), and the
rollback's own concern is purely mechanical entry inversion, subject only
to the ordinary balance-sufficiency check (§16.5's rollback row).

### 16.9 The G-2 injection boundary — preserving human ownership

**Corrected this round (Fix Round 2, final closing pass) — this seam
performs unconditional capture, never a disposition decision.** Round 1
and Round 2's earlier text described this seam as returning one of
`ACTION_REFORFEIT`/`ACTION_ROUTE_TO_CASH`/`ACTION_HOLD_FOR_REVIEW`
synchronously, from inside `postWin`'s own settlement transaction — i.e.,
as if G-2's answer were decided at this call site. It is not. Reconciled
against `10-bonus-engine-architecture.md` §N1.4 step 5b/5c (built against
`ledger-accounting-model.md` §7.7.2.2's binding contract, §16.7's
sub-branch 2 now walks the reasoning in full): the three actions are a
**separate, later resolution**, run by a human via `bonus-engine`'s own
resolution flow, in its own transaction, keyed by the `bonus_held_
dispositions` row's `id` — never inline in `postWin`, never decided by
this seam. This seam's entire job, every time it is called, is the
**unconditional hold-capture posting** (§16.14) plus handing off enough
information for `bonus-engine` to record the resulting `held` row — full
stop. See §16.7 sub-branch 2 for the reasoning this correction rests on
(why conditional-on-`ACTION_HOLD_FOR_REVIEW` capture would reopen the
exact P0, LF-2, this mechanism exists to close).

**Named seam, widened this round (LF-18/§7.7.2.10 item 2 for the
`payoutAmount`/`releasedLockAmount` split; widened again this round to
carry the posted transaction id, closing the gap between this signature
and §16.14's own prose, which already required handing it over):**

```
bonusengine.ResolveTerminalGrantCredit(
    ctx context.Context,
    tx <db tx>,
    grantID uuid.UUID,
    correlationID uuid.UUID,
    creditKind CreditKind,
    payoutAmount *big.Int,      // W, §16.4's win payout
    releasedLockAmount *big.Int, // X, Step 1b's net_outstanding_locked
    settlementLedgerTransactionID uuid.UUID, // new this round — see below
) (heldDispositionID uuid.UUID, err error)
```

**Signature reconciled against the real, shipped, tested code (Stage
4H-B1 Wave 2 Phase 3/7; certified by Wave 2's independent composition
review) — this pseudocode previously carried `correlationID string` and
`decimal.Decimal` amounts, both illustrative placeholders never actually
ratified against a concrete type. The platform has no `decimal.Decimal`
dependency anywhere (money is integer minor-units throughout, per
CLAUDE.md); `correlation_id` is a `uuid.UUID` column, like every other
identity field this document already types that way. `internal/bonus.
ResolveTerminalGrantCredit`'s real signature (`internal/bonus/
held_disposition_ops.go`) uses `correlationID uuid.UUID` and
`payoutAmount, releasedLockAmount *big.Int` — CLAUDE.md's own money rule
made `*big.Int` the only correct choice once implementation reached
these two amount parameters, not a divergence from this document's
intent. No `tenantID` parameter exists on this seam, in either the
pseudocode above or the real code — `tenantID` is resolved from the
Grant row itself, exactly as this section's own prose already states.**

Called from exactly the one site in §16.8, inside the same database
transaction as the settlement posting, under the same `(tenant_id,
grant_id)` advisory lock doc10 §9 already specifies. `payoutAmount` (`W`)
and `releasedLockAmount` (`X`) remain **separate parameters, never a
single combined `amount`**, per LF-18/§7.7.2.10 item 2's binding
requirement, unchanged this round. `payoutAmount`/`releasedLockAmount`
are threaded through **not** so this seam can pick a destination for them
— capture's destination is fixed, always `player_bonus_held` — but so
`bonus-engine`'s `bonus_held_dispositions` row can carry `payout_amount`/
`released_lock_amount` as its own columns (doc10 N1.4 step 5b.iii)
without re-deriving them from the ledger.

**Call order, made explicit this round (previously left implicit, and
inconsistent with §16.14's own prose, which already assumed a posted
transaction id existed to hand over):**

1. `postWin` (already inside its settlement transaction, `tx`) builds and
   posts the two-leg hold-capture `LedgerTransaction` **itself**, via its
   own existing `ledger.Post` call — the identical mechanism, idempotency
   key shape, and audit record every other settlement uses (§16.14) —
   landing in `player_bonus_held`. This is `casino`'s own posting; the
   seam does not perform it. This yields `settlementLedgerTransactionID`.
2. `postWin` then calls `ResolveTerminalGrantCredit`, passing that id
   along with `grantID`/`correlationID`/`creditKind`/`payoutAmount`/
   `releasedLockAmount`. Inside the same transaction, `bonus-engine`'s
   implementation writes the `bonus_held_dispositions` row (`status =
   'held'`, `settlement_ledger_transaction_id` = the id from step 1,
   `payout_amount = W`, `released_lock_amount = X`) and returns that row's
   own `id` as `heldDispositionID`.

`postWin` does **not** branch on `heldDispositionID` — it exists for
logging/observability/testing only (mirroring §16.14's "hand over the
transaction id" framing, now completed with an explicit return value on
the `bonus-engine` side). There is no return value here that could carry
a disposition, because no disposition is decided at this call site.

**Today, before `internal/bonus` exists at all, and before the
`player_bonus_held` account type exists (HR-9-gated, §7.7.2.3)**: neither
half of the two-step sequence above can run. `ledger.Post` has no valid
account type to post step 1 against, and `ResolveTerminalGrantCredit`
does not exist for step 2. The correct, fail-closed behavior for
`internal/casino` in this state is unchanged from Round 1 in *outcome*,
but reframed in *reasoning* — it is a mechanism-does-not-exist-yet
rejection, never a G-2-unanswered rejection, because capture never needed
G-2 answered in the first place: detect the terminal-Grant condition
(§16.8's live status read, which *can* be built today — it needs only a
Grant status table to query, not the resolution function) and **reject
the whole settlement transaction with a distinct sentinel error** (e.g.
`ErrTerminalGrantCreditUnresolved`), surfaced as an integrity/ops alert
exactly like `ErrBetNotFound` — never silently posted to either account,
never retried automatically, never guessed. This is the "fails closed /
queues for review rather than guessing" behavior this gate requires
(§A.8).

**Once `internal/bonus` and the `player_bonus_held` account type both
exist**, capture runs on **every** terminal-Grant win credit,
unconditionally — this requires no G-2 answer, ever, and is never gated
on which of the three actions a human will eventually pick. G-2's own
answer only governs the **separate, later** resolution transaction
(doc10 §N1.4 step 5c, adopted in §16.16): `ACTION_REFORFEIT` posts the
held amount straight into `promo_liability` — **never via `player_bonus`,
not even transiently** (the §T.7 phrasing this bullet previously carried,
"post normally, then a compensating `bonus_forfeiture`," described
exactly the posted-then-reversed shape `ledger-accounting-model.md`
§7.7.2.9/HR-25 confirms is wrong, and remains withdrawn); `ACTION_ROUTE_
TO_CASH` posts the held amount into `player_cash`; `ACTION_HOLD_FOR_
REVIEW` performs no posting at all — it is simply the absence of a
resolution act, the record remaining `status = 'held'` (§16.16). **No
code in this section selects among these three or implements any of
them — this section only names the boundary at which capture happens
(unconditionally) and the boundary at which resolution happens (later,
separately, never here).**

**The requirement on all three, found while designing §16.5a's
lock-release step, closed this round.** The released lock amount (`X`,
distinct from the win payout `W`) must land somewhere alongside `W` at
capture time, regardless of which action a human eventually selects —
`player_bonus_held` (§16.14) is that destination for both, in the single
unconditional two-leg posting above, exactly as `ledger-finance`'s
§7.7.2.2 specifies. The lock is never left dangling, and never waits for
a disposition to exist before it closes to zero.

### 16.10 The Grant terminal-state invariant — formalized, stress-tested, and found insufficient as originally proposed

**As originally proposed** (`docs/governance/task-registry.md`, Stage
4H-B1 Wave 1 reconciliation finding 1): *"A Grant may only reach a
genuinely terminal status... once its attributable locked balance
reaches zero. A terminal trigger firing while locked funds remain
outstanding defers the Grant into a pending-settlement sub-state that
finalizes automatically once the locked stake resolves (win or loss)."*

**16.10.1 — A precondition the original proposal did not state, and
without which it cannot be built for casino at all.** The proposal
assumes a nonzero `player_locked_bonus` balance exists between bet and
settlement, as the signal that "locked funds remain outstanding." This
is true under migration 0048's case-B/G/I shape (already approved for
sportsbook: `Dr player_bonus / Cr player_locked_bonus` at lock, no
mirror, settlement absorbs from the locked account). It is **false**
under ADR 0032 §3's original casino shape, which this document (§7) still
cites as the model for a future bonus-funded casino bet: `Dr player_bonus
X / Cr house_gaming X` **with the mirror pair posting immediately**
(`bonus_expense` recognized at bet time, not at settlement). Under that
shape, the instant a bonus-funded bet posts, its value has **already**
left `player_bonus`/`promo_liability` entirely — there is no
`player_locked_bonus` balance, or any other ledger-visible fact, marking
"this Grant still has a bet outstanding." A Grant could go fully terminal
while a bet is in flight with **zero signal anywhere in the ledger** that
anything is still at risk against it — the deferred-terminal mechanism
would have nothing to key off.

**Finding, routed to `ledger-finance`/`architect` for ratification, not
decided unilaterally here**: casino's bonus-funded bet posting, whenever
it is built, **must** adopt migration 0048's lock shape (case B at bet
time, case G/I at settlement) rather than ADR 0032 §3's immediate-absorb
shape, specifically for the bonus-funded case (the cash-funded case is
**unaffected** — it keeps today's immediate `Dr player_cash / Cr
house_gaming` shape unchanged, since G-2 never applies to cash). This is
not a new posting shape invented here — it is the same shape §6.4's
case B/G/I already specify and three independent specialists already
validated for sportsbook (§6.4.11) — only its *application to casino*,
previously assumed unnecessary under the now-corrected §16.2.1 premise,
is new. **This is a required precondition for §16.9's seam to have
anything to act on**, and is recorded here as an open cross-domain
dependency (§16.13), not authorized or implemented by this section.

**16.10.2 — the corrected invariant, given 16.10.1's precondition
holds.** Split a Grant's exposure at the instant a terminal trigger
fires into two parts, both read live under the `(tenant_id, grant_id)`
advisory lock:

> **INV-TG.** Let `B(G)` = `G`'s current non-locked `player_bonus`
> balance and `L(G)` = `Σ signed(player_locked_bonus)` attributable to
> `G` (the §6.3.3.1/§6.6.6 recovery-query pattern, reused verbatim). When
> a terminal trigger fires for `G` (natural expiry, cancellation, or a
> forfeiture-causing breach on a **different** bet under `G`):
>
> 1. `B(G)` is written off **immediately, unconditionally, using the
>    existing, unmodified §3.1/§T.9 forfeiture posting** (`Dr player_bonus
>    B(G) / Cr promo_liability B(G)`) — this part was never subject to any
>    settlement-timing race and needs no G-2 involvement, ever.
> 2. If `L(G) > 0`, `G` enters a new sub-state, `pending_settlement`
>    (distinct from `activated`/`in_progress` — no **new** wagering may
>    be authorized against `G` while pending; distinct from true
>    terminal — no "this Grant is closed" signal is emitted yet). If
>    `L(G) = 0`, `G` transitions straight to its ordinary terminal state
>    (§3.1/§T.9's existing mechanism, unchanged) — INV-TG adds nothing
>    in this branch.
> 3. While `pending_settlement`, `L(G)`'s eventual resolution is
>    classified exactly as §6.6.5's exhaustive switch already classifies
>    it: **value-reducing** (a loss, or — per §16.10.3's flagged, unresolved
>    question — possibly a rollback-of-the-lock) posts ordinarily, no G-2
>    — for casino specifically, "posts" means via whichever mechanism
>    §16.5a specifies (the settlement-timeout sweep, since no loss
>    callback exists), not necessarily immediately upon the outcome being
>    known provider-side (LF-4: the original text's "posts immediately"
>    assumed a signal casino does not have); **value-creating** (a win) is
>    the **sole** trigger for §16.9's seam.
> 4. Once `L(G)` reaches zero **and** any pending G-2 disposition has
>    resolved, `G` finalizes into the terminal state its original trigger
>    named, with a Progress-trail entry recording both the original
>    trigger and (if applicable) the G-2 resolution.

**16.10.3 — a tension found between doc10 §T.5.1 and §T.7, flagged for
reconciliation with `bonus-engine`'s parallel invariant proof, not
resolved here.** §T.5.1 states a void/rollback credit that "merely
restores a previously-locked stake to its origin account" is "a
reversal, not new value" and is **never** gated (there, specifically
against `AssetAuthorization`). A rollback of the lock itself (`Dr
player_locked_bonus X / Cr player_bonus X`) is exactly this shape, and is
also `BONUS_SET`-neutral (§6.3.2: a lock posts no mirror, so its reversal
posts none either — the aggregate `player_bonus + player_locked_bonus +
promo_liability` is unchanged by a lock or its reversal). By that
reasoning, restoring `X` to `player_bonus` and then immediately sweeping
it into the **already-triggered** write-down (INV-TG step 1's mechanism,
re-applied) would be uncontested and would need no G-2 involvement at
all. Yet §T.7's own trigger-condition text explicitly lists "a settlement
... **or a void/rollback event**" together as reaching the same G-2
decision point, with no textual carve-out for the "restores, doesn't
create" sub-case. **This document does not resolve the tension** — doing
so would narrow G-2's scope, which is not casino's call — but flags it
because, if resolved in the direction §T.5.1's own reasoning suggests, it
would remove one of the two named G-2 trigger paths (leaving only genuine
WIN settlements, §16.8's enumerated case) without any human decision
required, which may simplify `bonus-engine`'s own parallel proof of the
Grant terminal-state invariant. Flagged, not adopted.

**16.10.4 — is INV-TG sufficient against the async casino callback
model?** Stress-tested against §16.2's full lifecycle:

- **Replay**: `L(G)`/`B(G)` are derived, stateless reads over posted
  ledger facts (identical pattern to `P_net`/`P_firm`, §6.6.7 case 10) —
  a redelivered win is rejected by `ledger.Post`'s own idempotency key
  before §16.4/§16.9 ever re-run, so INV-TG cannot be evaluated twice for
  one event. **Sufficient.**
- **Late callback**: INV-TG's mechanism does not depend on how much time
  elapses between the terminal trigger and `L(G)` reaching zero — the
  advisory lock and live `FOR UPDATE` reads make the gap's length
  irrelevant to correctness (only to how long `G` visibly sits in
  `pending_settlement`). **Sufficient.**
- **Concurrent callback** (a terminal trigger racing the locked stake's
  own settlement): both acquire the same `(tenant_id, grant_id)` advisory
  lock, so they serialize. Whichever commits first is read live by the
  second — if the trigger commits first, the settlement sees
  `pending_settlement` and reaches §16.9 (or the value-reducing branch);
  if the settlement commits first, `L(G)` is already zero when the
  trigger evaluates, and the trigger proceeds straight to ordinary
  termination with no `pending_settlement` interval at all. **Sufficient
  — no ordering produces an unhandled state.**
- **Callback after Grant expiry/cancellation specifically** (as opposed
  to forfeiture): identical mechanism — §T.9/§T.10 already establish
  expiry and cancellation as symmetric to forfeiture for this purpose;
  INV-TG treats all three terminal-trigger kinds uniformly, per doc10's
  own "for any reason" framing (§T.7).
- **Where INV-TG is *not* sufficient on its own**: it is a **timing/
  sequencing** invariant only — it guarantees G-2 is reached at most once
  per locked stake, in a well-defined transactional order, but it
  **does not itself decide** what happens at that reach point. That
  remaining decision is G-2, by design, and INV-TG's entire purpose is to
  make sure nothing before G-2 quietly resolves it by accident (e.g., an
  auto-finalizing "pending_settlement" that swept a late win into the
  ordinary forfeiture posting would **silently select ACTION_REFORFEIT**
  — this is precisely why step 3 above routes a value-creating credit to
  §16.9's seam instead of the ordinary write-down, and is the one place
  an earlier, looser reading of the original task-registry.md proposal
  ("finalizes automatically... win or loss") would have crossed into
  selecting G-2's answer without a human decision. **This distinction —
  auto-finalize only the value-reducing branch, always route the
  value-creating branch through the seam — is the correction this
  section makes to the original proposal, and is the reason §16.10 is
  titled "found insufficient as originally proposed."**

### 16.11 Adversarial scenarios

**Round 2 note.** The eight scenarios the human directive names explicitly
(duplicate win, duplicate rollback, concurrent win/rollback, late
callback, callback after expiry, callback after cancellation, rollback of
a held win, rollback of a resolved win) are all present in this table
already or added below — restated as their own named, numbered set in new
§16.19, per that directive's explicit format requirement, rather than
only inline here.

| Scenario | Expected behavior | Why safe |
|---|---|---|
| Win after Grant state change (non-terminal → terminal, no lock outstanding) | Ordinary termination already completed (INV-TG step 2's `L(G)=0` branch) before the win arrives; the win then finds no matching, non-reversed bet under this Grant's lock — but the bet's own ledger row still exists, so §16.4 still resolves an origin. If `G` is already fully terminal with `L(G)` having been zero at trigger time, no `pending_settlement` interval ever existed for this stake, meaning the win could only be a genuinely late artifact of a *different*, still-locked amount — in which case `pending_settlement` (still active) is found and §16.9 fires | INV-TG's `L(G)` check is re-evaluated live, not cached from trigger time |
| Win after wagering completion (`completed`/`converted`) | Not a terminal status (§1.2) — G-2 does not apply; ordinary settlement path, credit posts to `player_bonus` normally, subject to whatever `bonus_conversion` has already released | `completed`/`converted` are excluded from G-2's trigger set by definition (doc10 §T.13) |
| Rollback of a **resolved** win (`player_bonus`/`player_cash` already credited, or a `bonus_held_dispositions` row already `resolved_*`) | `postRollback`'s generic inversion (§16.1) debits the resolved account for the win amount; if the Grant went terminal and swept that balance into `promo_liability` in the interim (or the hold already resolved and moved on), the debit finds insufficient balance. **What happens then is `ledger-finance`'s decision, not casino's (LF-10, confirmed still open by `ledger-finance`'s own Round 2 text, §7.7.2.11 — see §16.20) — this document does not pre-select "fail closed," "permit negative as a clawback," or "route to a receivable."** Whichever `ledger-finance` decides, it must generalize `ledger.Post`'s existing sufficiency mechanism to `player_bonus` (not yet built, HR-9 currently blocks all `player_bonus` posting regardless), flagged in §16.13 | Never posts a negative/impossible balance *without an explicit, ledger-finance-owned decision permitting one*; the mechanism generalizes the existing `SELECT ... FOR UPDATE` pattern (already used for `player_cash`, `orchestrator.go` line 762) — the treatment of a failed check is the open item, not the check's existence |
| Rollback of a **still-`held`** win (`bonus_held_dispositions.status = 'held'`) | **Closed this round (§16.15, adopting `ledger-finance` §7.7.2.7).** `postRollback`'s generic inversion (`Cr house_gaming payout_amount`, and `Cr player_locked_bonus released_lock_amount` if present — never a lock resurrection) plus, in the **same** transaction, `UPDATE bonus_held_dispositions SET status='voided_by_rollback', resolution_ledger_transaction_id=<reversal id> WHERE id=? AND status='held'` | The `WHERE status='held'` clause is a DB-enforced compare-and-swap — zero rows affected is the loud, checkable failure signal, never check-then-update; gated under the same row `FOR UPDATE` HR-25 already requires for resolution (§16.15) |
| Genuine win event resolves to §16.4's **outcome 6** (LF-18: identity found, `net_outstanding_locked ≤ 0`) | Whole win aborted with `ErrLockAlreadyReleased`, integrity alert, held for manual reconciliation — never released a second time | This is precisely the branch LF-18's exploit needed and Round 1 did not have (§16.17) |
| Win after rollback | `postWin`'s existing `NOT EXISTS (... reverses_transaction_id ...)` clause (line 849, reused unchanged in §16.4's query) finds no valid origin → `ErrBetNotFound`, an integrity alert | Unchanged, already correct |
| Duplicate win | `ledger.Post`'s `(tenant_id, idempotency_key)` uniqueness no-ops the second delivery before §16.4/§16.9 re-run | Idempotency key is provider-namespaced and DB-enforced, not "check then insert" |
| Duplicate rollback | Existing `postRollback` logic (distinguishes a same-reference redelivery, idempotent no-op, from a distinct new reference against an already-reversed original, `ErrAlreadyRolledBack`) — unaffected by this section | Unchanged, already correct and tested (Stage 4A completion report §17) |
| Concurrent win and rollback (same bet) | Both name the same original bet; `postRollback` takes `FOR UPDATE` on the original transaction row (line 939); a concurrent win reaching §16.4's query before or after that lock resolves either finds the bet not-yet-reversed (proceeds normally, then a following rollback of the *win* is required — the round now needs its win rolled back separately, per §7's "two independent reversals" rule) or finds it already reversed (`ErrBetNotFound`) — no interleaving produces a double-credit or a lost debit | Serialized by the original's row lock; `ledger.Post`'s own idempotency and the deferred `ledger_entries_balanced` constraint provide defense in depth |
| Late callback | Handled uniformly regardless of elapsed time (§16.10.4) | Timing is not part of any correctness condition in this design |
| Callback after Grant expiry | INV-TG's `pending_settlement`/G-2 path (§16.8) | Live status read, not a cached grant-time snapshot (doc10 §T.11) |
| Callback after Grant cancellation | Identical mechanism to expiry — §T.10 confirms cancellation is symmetric to expiry for this purpose | Same as above |
| Two players' rounds collide on `correlation_id` (LF-7) | §16.4's outcome 2 fires: one distinct `account_type`, more than one distinct `wallet_id` → `ErrCorrelationWalletCollision`, whole win aborted, integrity alert raised | Never guessed which of the two wallets is correct; a SHA1-namespaced UUID collision or a posting-layer defect is treated as an incident, not resolved silently |
| Multi-bet round: win event cannot say which bet it settles (LF-8) | §16.4's outcome 3 fires: more than one distinct `bet_transaction_id` under one `correlation_id` → §16.4a's manual-reconciliation queue, not a silent guess and not a permanent abort | `WinRequest`/`CallbackEvent` has no bet-level identifier today (confirmed against `internal/casino/types.go`); resolved by a human today, automatable once `OriginatingProviderTxID` is added (§16.4a) |
| Losing bonus-funded round (no callback ever arrives) | §16.5a(a)'s settlement-timeout sweep resolves it as a loss once the configured per-jurisdiction window elapses; absent config, nothing sweeps and the lock is visible, not silently written off | Fail-closed default (no sweep without explicit config); every timeout posting is audited identically to a normal settlement |
| Genuine win arrives after the settlement-timeout window already swept the round as a loss | Not silently re-posted or auto-reversed — raises a new, distinct integrity alert and is held for manual reconciliation (§16.5a(a)) | Prevents a double-count or a resurrected Grant sub-state; the accepted cost of a bounded window is an alerted incident, not a silent loss of funds |
| Win arrives for a round already rejected via `ErrTerminalGrantCreditUnresolved`, before its settlement-timeout window would otherwise elapse | Excluded from the timeout sweep's candidate set by the recorded rejection audit event (§16.5a(a)'s exclusion, found during the §16.7 re-walk) — stays in `pending_settlement` for a human to resolve via G-2, never auto-resolved as a loss | A genuine win must never be silently reclassified as a loss merely because it is also G-2-blocked |

### 16.12 Corrections routed to `ledger-finance`, not edited here

Per this gate's own instruction ("route it, don't edit `ledger-finance`'s
file directly"):

1. `ledger-accounting-model.md` §6.3.3.1's claim that "casino resolves a
   bet atomically inside one transaction... never needed a recovery
   mechanism" is factually incorrect against `internal/casino`'s actual
   `ReceiveCallback`/`postBet`/`postWin` structure (§16.2.1). Casino needs
   the identical recovery query sportsbook needs, reused verbatim by
   §16.4.
2. `ledger-accounting-model.md` §6.6.6's claim "for casino the quantity is
   always zero, so no casino contribution is ever nullifiable and
   `P_firm == P_net`" shares the same false premise and is incorrect for
   the identical reason, once a bonus-funded casino bet exists. §6.6.5's
   own classification table already lists `casino_bet`/`casino_win`/
   `casino_rollback` correctly — only §6.6.6's summary sentence needs
   correcting.
3. §16.10.1's finding — that a future bonus-funded casino bet must use
   migration 0048's lock shape (case B/G/I), not ADR 0032 §3's
   immediate-absorb shape — is a recommendation for `ledger-finance`/
   `architect` to ratify or reject when casino's bonus-funded bet posting
   is actually designed; it is not adopted as binding by this section.
4. §16.10.3's flagged tension between doc10 §T.5.1's asymmetry reasoning
   and §T.7's trigger-condition text (whether a rollback-of-the-lock
   should be treated as G-2-relevant or as an uncontested, immediately-
   resolvable value-reducing event) is left open for `bonus-engine`'s
   parallel Grant-terminal-invariant proof to reconcile.
5. **New (LF-4 fix wave).** §16.5a proposes a new named transaction type,
   `casino_settlement_timeout`, and a new scheduled sweep mechanism,
   neither of which exist in `ledger-accounting-model.md` today. This is
   a proposal for `ledger-finance`/`architect` to ratify or reject — the
   same posture as item 3 above — not adopted as binding by this section.
   It also proposes reusing the existing case-I loss-accounting shape
   verbatim for the sweep's posting, which `ledger-finance` should confirm
   is a correct reuse, not a divergence.
6. **LF-10 (unchanged posture, reconfirmed Round 2).** §16.5's and
   §16.11's rollback-of-a-**resolved**-win insufficient-balance rows still
   do not assert "fail closed" as this document's answer. `ledger-finance`
   itself, in the course of closing the *still-held* rollback sub-case
   this round, explicitly reconfirmed the resolved-win sub-case "remains
   open, still routed to `ledger-finance` generally" (`ledger-accounting-
   model.md` §7.7.2.7/§7.7.2.11) — this document does not attempt to close
   it either, for the reasons detailed in new §16.20 (it is a
   ledger-wide compensating-entry policy question, not a casino-specific
   one, and CLAUDE.md/this project's ownership model reserves it for
   `ledger-finance` + `architect`).
7. **New (LF-18 fix wave — for `ledger-finance`'s own required independent
   re-verification, not an open decision).** §16.4's Step 1b (variant-2
   query) and its sixth outcome branch are `casino`'s own fix to `casino`'s
   own defect, per the Phase 2 report's explicit ownership ("Owner:
   casino, reviewed by ledger-finance"). This is recorded here so
   `ledger-finance`'s review has an exact, named target — not because the
   fix itself is `ledger-finance`'s decision to make.

### 16.13 What remains open

- **G-2 itself** — not selected, per this gate's explicit instruction.
  §16.9 names the exact injection boundary; §16.9's fail-closed behavior
  (reject with an alert) is the only behavior authorized until a human
  decides.
- **`internal/bonus` does not exist.** Every mechanism in this section
  (`bonusengine.ResolveTerminalGrantCredit`, the Grant-status read, the
  `pending_settlement` sub-state) is a design against a package that has
  not been written. Nothing here is buildable in isolation by `casino` —
  it requires `bonus-engine`'s own Wave 2+ schema/state-machine work to
  exist first.
- **§16.10.1's posting-shape precondition** requires `ledger-finance`/
  `architect` ratification before any bonus-funded casino bet is designed
  at the posting-shape level, independent of G-2.
- **§16.10.3's tension** requires reconciliation with `bonus-engine`'s
  parallel proof, not resolved here.
- **The `pending_settlement` sub-state itself** is a new Grant lifecycle
  state. Per doc10 §T.5's own words, "adding a lifecycle state is exactly
  the kind of cross-cutting redesign CLAUDE.md reserves for `architect` +
  `ledger-finance` agreement" — this section proposes it and stress-tests
  it from casino's own angle, but does not have authority to adopt it
  unilaterally.
- **§16.9's `ACTION_HOLD_FOR_REVIEW` holding mechanism — CLOSED this
  round, adopted from `ledger-finance`.** Round 1 left this an unresolved
  `ledger-finance` design question, including the released-lock-amount
  destination. `ledger-finance`'s Round 2 (§7.7.2, adopted in §16.14) now
  names `player_bonus_held` as the destination for both the payout and the
  released lock, in one balanced transaction. **Still open, unchanged by
  this closure**: that account's interaction with the hourly
  reconciliation sweep is specified by `ledger-finance`'s own new `LF-12`
  stream (§7.7.2.8), not by this document — this document adopts that
  stream's existence but does not design it.
- **The rollback-of-a-credited-win-against-a-later-forfeited-balance
  insufficient-balance case (the *resolved*-win sub-case, LF-10) —
  STILL OPEN, reconfirmed this round.** Depends on `ledger-finance`'s
  balance-sufficiency check generalizing to `player_bonus`, not yet built
  (HR-9 currently blocks all `player_bonus` posting regardless). This
  document still does not assert "fail closed" as its own answer — see
  §16.20 for why this document does not attempt to close it now, even
  though the *still-held* sub-case of the same rollback gap is closed
  this round (§16.15).
- **§16.5a's settlement-timeout sweep (LF-4)** is a new mechanism and a
  new transaction type (`casino_settlement_timeout`), proposed but not
  authorized — requires `ledger-finance`/`architect` ratification
  (§16.12 item 5) **and** a new human/jurisdiction decision (the actual
  per-jurisdiction window durations) neither selected nor defaulted by
  this section. Until both exist, §16.10.1's lock-shaped bonus-funded
  casino wagering design is **NOT SAFE to implement** (§16.5a's explicit
  statement).
- **§16.4a's `OriginatingProviderTxID` capability (LF-8)** does not exist
  on `WinRequest`/`CallbackEvent` today (confirmed against
  `internal/casino/types.go`) and is not added by this section — a
  required future provider-protocol/schema capability before multi-bet
  rounds can be resolved automatically rather than via manual
  reconciliation. Until it exists, multi-bet-capable titles should not be
  marked launch-eligible for bonus-funded wagering (flagged for
  `product-owner-proxy`/`architect` scoping, not decided here).
- **Free-round/bonus-stake normalization** (§1's own long-standing
  `OPEN DECISION`) is untouched by this section — it concerns how a
  provider-side free-round is normalized into the platform's bet/win
  shape, not the settlement-credit destination question this section
  answers.
- **LF-18 — CLOSED this round** (§16.4, exploit re-walked in §16.17).
  Pending `ledger-finance`'s independent re-verification per the Phase 2
  report's own required review posture (§16.12 item 7).
- **The held-win rollback gap — CLOSED for the still-held sub-case this
  round** (§16.15, adopting `ledger-finance` §7.7.2.7). The
  already-resolved sub-case remains LF-10, still open (§16.20).
- **Idempotency key (LF-22) — CLOSED, adopted from `ledger-finance`**
  (§16.14): `settlement_ledger_transaction_id` per hold occurrence, never
  `correlation_id`.
- **`ACTION_HOLD_FOR_REVIEW`'s semantics — CLOSED this round** (§16.16):
  it both releases the lock (a real posting, `L(G) → 0`) and creates no
  spendable value (the value lands in `player_bonus_held`, disjoint from
  `player_bonus`/`player_cash`) — these are not contradictory once stated
  against the correct pair of accounts.
- **Step 1b's `OriginatingProviderTxID` extension** (§16.4's interaction
  note) is a required but not-yet-needed follow-up: today no multi-bet
  round ever reaches Step 1b (outcome 3 routes it to the manual queue
  first), so the gap is dormant, not live — it must be closed in the same
  change that adds `OriginatingProviderTxID` to Step 1/Step 2 (§16.4a),
  not before.
- **Hold-capture invocation timing — CLOSED this round (Fix Round 2, final
  closing pass)** (§16.9, §16.14, reasoning in §16.7 sub-branch 2).
  Capture into `player_bonus_held` is unconditional for every
  terminal-Grant win credit, decided before any of the three G-2 actions
  is known — never conditioned on the credit eventually staying
  `ACTION_HOLD_FOR_REVIEW`. `ResolveTerminalGrantCredit`'s signature is
  widened again to carry `settlementLedgerTransactionID` and return
  `heldDispositionID`, closing the gap between §16.9's prior signature and
  §16.14's own prose (which already required handing the id over).
- **The Grant-status-finalization seam for value-reducing closing
  events — CLOSED this round** (new §16.21). `casino`'s own Phase 2
  finding against `bonus-engine`'s work this round — that doc10 N1.4 step
  5a/5c's "`G.status` flips in the same transaction, no window" claim
  named no concrete call by which `postRollback` or the settlement-timeout
  sweep would reach it — is closed by naming `bonusengine.
  RecheckGrantExposure`, symmetric to `ResolveTerminalGrantCredit`.

### 16.14 Adopting `ledger-finance`'s `player_bonus_held` account and the two-leg hold-capture posting

**Adopted in full from `ledger-accounting-model.md` §7.7.2 — this section
selects nothing new, it implements the contract already decided there
(§7.7.2.10, "what `casino` must do").** Round 1 left `ACTION_HOLD_FOR_
REVIEW`'s destination as an unnamed cross-dependency (§16.5a, §16.9,
§16.13). `ledger-finance`'s Round 2 named it. This section adopts that
answer at every site Round 1 flagged as open, and nowhere invents a
variant of it.

**Corrected this round (Fix Round 2, final closing pass): this posting is
unconditional, not conditioned on `ACTION_HOLD_FOR_REVIEW`.** An earlier
reading of this section framed the posting below as built "at exactly the
site `ACTION_HOLD_FOR_REVIEW` is returned" — implying it fires only for
that one outcome, with `ACTION_REFORFEIT`/`ACTION_ROUTE_TO_CASH` posting
directly to their final destination instead. That is not what `doc10`
§N1.4 step 5b (built against this document's own §7.7.2.2 contract)
requires, and §16.9/§16.7 sub-branch 2 now state the corrected model in
full: this posting happens for **every** terminal-Grant win credit,
**before** any of the three actions is known, let alone selected. What
`ACTION_HOLD_FOR_REVIEW` actually names is not this posting — it is the
**absence of any further posting**, i.e. the record staying `status =
'held'` because no one has resolved it yet (§16.16). The text below is
revised accordingly.

**The account.** `player_bonus_held` — a third, disjoint, Grant-attributed
ledger account type (per `(wallet_id, account_type, asset_code)`, the same
shape every other `player_*` type already has), a member of `BONUS_SET`
(§7.7.2.3). `casino` never creates this account type (that is `ledger-
finance`'s migration `0055`, §7.7.2.4) — `casino` only posts against it,
through `ledger.GetOrCreateAccount`, exactly as it already does for every
other account type today.

**The posting — one balanced `LedgerTransaction`, built and posted by
`postWin` itself, unconditionally, at exactly the one site §16.8 names
(the live Grant-status read finds a terminal/`pending_settlement` Grant),
*before* `ResolveTerminalGrantCredit` is even called (§16.9's corrected
call order) — never conditioned on which of the three G-2 actions this
credit will eventually receive:**

> - `Dr house_gaming payout_amount(W) / Cr player_bonus_held W` — **always**
>   present, the win's own value-creating credit, captured rather than
>   posted to `player_bonus`.
> - `Dr player_locked_bonus released_lock_amount(X) / Cr player_bonus_held
>   X` — present **only if** the originating stake was locked (true for
>   every bonus-funded casino bet reachable under §16.10.1's mandated
>   shape; would be absent entirely for a hypothetical never-locked
>   origin, which §16.10.1 makes structurally unreachable for casino
>   today).
>
> Both legs post in the **same** `ledger.Post` call — never as two
> transactions — so there is no intermediate state where `L(G)` has
> closed but the payout has not yet been captured, or vice versa (`ledger-
> accounting-model.md` §7.7.2.2, quoted verbatim in this round's
> dispatch).

`W` is `postWin`'s own resolved payout amount (from the provider's
callback, §16.3 — an *amount*, never a *destination*: the provider
supplies how much was won, never which account receives it). `X` is
§16.4's Step 1b `net_outstanding_locked` — **the LF-18-corrected,
currently-outstanding figure, never Step 1's stale bet-time amount**. Both
are threaded through `ResolveTerminalGrantCredit`'s widened signature
(§16.9) as `payoutAmount`/`releasedLockAmount`, separately, so the
posting layer never needs to re-derive either from a single combined
`amount`.

**The record.** `bonus_held_dispositions` (§7.7.2.5) is `bonus-engine`-
owned, not `casino`-owned — `casino`'s only obligation is to post the
hold-capture transaction above and hand `bonus-engine`'s seam
implementation the transaction id it posted under
(`settlement_ledger_transaction_id`, now an explicit parameter on
`ResolveTerminalGrantCredit`, §16.9), so `bonus-engine` can write the
corresponding row (`status = 'held'`) in the **same** database
transaction and return its own `heldDispositionID` (§16.9). `casino` does
not design, migrate, or query this table directly, and does not branch on
`heldDispositionID` — it is a hand-off and a confirmation, not a decision
input.

**Idempotency (LF-22, adopted verbatim).** The per-occurrence key is
`settlement_ledger_transaction_id` — **not** `correlation_id` (this
document's own §16.4a already proved a round-scoped key wrong: one round
can legitimately contain several, separately-settled bets, each needing
its own hold) and **not** `grant_id`. `ledger.Post`'s own `(tenant_id,
idempotency_key)` uniqueness has already collapsed a redelivered win into
one settlement transaction *before* hold-creation logic ever runs, so
`settlement_ledger_transaction_id` is guaranteed to name exactly one real
occurrence by construction — `casino` inherits this guarantee for free
from its own existing idempotency mechanism (§16.2's lifecycle table);
`bonus_held_dispositions`'s own `UNIQUE (tenant_id,
settlement_ledger_transaction_id)` constraint is `bonus-engine`'s
independent second line of defense, not something `casino` needs to
build.

**Concurrency (HR-21/HR-25, confirmed unaffected on the creation side).**
The hold-capture posting runs inside `postWin`'s own transaction, under
the same `(tenant_id, correlation_id)` then `(tenant_id, grant_id)` lock
order this document already uses (§16.9) — it acquires no new lock
participant, exactly as `ledger-accounting-model.md` §7.7.2.9 confirms
("HR-21 is not violated and needs no new participant for creation").
Resolution (a human applying G-2's eventual answer) and the rollback
transition (§16.15) are the paths that introduce the new, fourth
participant (HR-25, the `bonus_held_dispositions` row lock) — neither is
part of `postWin`'s own write path.

**Not casino's to build alone.** `bonus_held_dispositions`'s schema,
migration, and `HeldDisposition(G, t)` redefinition are `bonus-engine`'s
(§7.7.2.10's own ownership split). This section only specifies what
`casino` itself must do — post the two-leg transaction, widen the seam
signature, hand over the transaction id — per that same contract's
`casino`-facing items 1–4.

### 16.15 Held-win rollback transition (`postRollback`, adopting `ledger-finance` §7.7.2.7)

**The gap, closed for the reachable case this document owns.** A provider
rollback naming a `settlement_ledger_transaction_id` whose
`bonus_held_dispositions` record is still `status = 'held'` had no
defined transition in Round 1 (`casino`'s own Phase 2 finding,
corroborated by `qa` test `C28`). `ledger-finance` closed the mechanism in
§7.7.2.7; this section is `casino`'s adoption of it into `postRollback`,
per §7.7.2.10 item 4 ("implement §7.7.2.7's rollback-of-a-held-win
transition ... inside `postRollback`").

**The mechanism, adopted verbatim — one transaction, two effects:**

1. **Not a literal, leg-by-leg entry-inversion — corrected here (this
   document's own prior wording self-contradicted, caught during Phase 7's
   real implementation, resolved against §16.18's unambiguous worked proof
   and the actual shipped code, `internal/casino/bonus_settlement.go`'s
   held-win rollback path).** A prior revision of this bullet described the
   reversal as "the exact inverse of every entry the hold-capture posting
   made," and then, inconsistently with its own very next sentence, wrote
   the released-lock leg's inverse as `Cr player_locked_bonus
   released_lock_amount` — literally restoring the lock — while the next
   sentence simultaneously said restoring the locked balance is
   "deliberately never attempted." Only one of those can be true; per
   `ledger-accounting-model.md` §7.7.2.7 (quoted verbatim) and §16.18 Part
   B step 3's worked numeric proof, the **never-restore** reading is
   correct and is what ships. **Both legs reverse straight to
   `house_gaming`, debiting `player_bonus_held` for the full amount each
   leg originally credited it:** `Dr player_bonus_held payout_amount / Cr
   house_gaming payout_amount`, and `Dr player_bonus_held
   released_lock_amount / Cr house_gaming released_lock_amount` **only if
   that leg was present**. Restoring the locked balance is **deliberately
   never attempted** — `L(G)` already closed to zero at hold-capture time,
   and reversing a win that never should have happened is a straight
   reversal to `house_gaming`, not a resurrection of a lock
   (`ledger-accounting-model.md` §7.7.2.7). If the underlying **bet**
   itself also needs unwinding, that is a second, independent rollback
   naming the bet's own `provider_tx_id` — this document's existing "two
   independent reversals" rule (§16.11's concurrent win/rollback row),
   unaffected here.
2. **In the same transaction**, a guarded status update:
   `UPDATE bonus_held_dispositions SET status = 'voided_by_rollback',
   resolution_ledger_transaction_id = <the reversal's own id> WHERE id = ?
   AND status = 'held'`. The `WHERE status = 'held'` clause **is** the
   compare-and-swap — one atomic statement, DB-enforced, never
   check-then-update. Zero rows affected is the loud, checkable failure
   signal.

**New lock-order requirement on `postRollback` specifically, stated so it
is not missed during implementation.** Before today, `postRollback` only
ever needed a `FOR UPDATE` on the named original transaction's own row
(`orchestrator.go:939`). Resolving *this* transition additionally requires
HR-25's lock order (`ledger-accounting-model.md` §7.7.2.9): (1) the
`(tenant_id, grant_id)` advisory lock, **then** (2) `SELECT ... FOR
UPDATE` on the specific `bonus_held_dispositions` row. `postRollback`
must acquire both, in that order, **whenever its target transaction has
an associated hold record** (a lookup by
`settlement_ledger_transaction_id` that `postRollback` must perform before
deciding which of the two rollback shapes — ordinary generic inversion,
or this held-aware variant — applies). This is a genuinely new
requirement on `postRollback`, not a restatement of its existing
same-original-row lock.

**Proof this prevents double-resolution under concurrent callbacks**
(the human directive's explicit requirement) — three interleavings,
exhaustive:

- **Rollback races a human resolution (`ACTION_REFORFEIT`/
  `ACTION_ROUTE_TO_CASH`) of the same held record.** Both acquire
  `(tenant_id, grant_id)` first, then contend for the row's `FOR UPDATE`
  — they serialize. Whichever commits first leaves `status` in a terminal
  state (`voided_by_rollback`, or `resolved_reforfeit`/
  `resolved_route_to_cash`); the second finds `status ≠ 'held'` and its
  own guarded `UPDATE ... WHERE status = 'held'` affects zero rows —
  rejected, never applied, never a second disposition of the same value.
  **No interleaving resolves the same held value twice.**
- **Duplicate rollback of the same held record.** The redelivered
  rollback's own reversal transaction hits `ledger.Post`'s idempotency key
  first (same `provider_tx_id`, same target) — an idempotent no-op,
  identical to the existing, already-tested duplicate-rollback path
  (§16.11's "Duplicate rollback" row). The guarded status update is never
  even re-attempted, because the reversal transaction itself never
  re-posts.
- **Two genuinely distinct rollback attempts naming the same held
  record** (a defensive case beyond simple redelivery — e.g. a
  provider-side retry with a *new* `provider_tx_id` by mistake): both
  reach the row's `FOR UPDATE` and serialize exactly as the first bullet
  describes; the loser's guarded update is a zero-row no-op regardless of
  which reversal transaction it is attached to.

**What this does not close — LF-10, restated precisely, not silently
worked around here.** A rollback naming an **already-resolved**
(`resolved_reforfeit`/`resolved_route_to_cash`) record is the harder,
general case: the held value has already moved to a different account
entirely, potentially already spent or converted. This is **not** the
transition this section closes. §16.20 states precisely why it stays open
and routed to `ledger-finance`, not decided here.

### 16.16 `ACTION_HOLD_FOR_REVIEW` — resolving the contradiction, conforming to `ledger-finance`'s actual vocabulary

**The contradiction, stated precisely.** This document's own §16.7 (Round
1) said `ACTION_HOLD_FOR_REVIEW` "releases the lock ... into a
suspense/hold destination." Read against `bonus-engine`'s parallel text,
this was interpreted as conflicting with a claim that the action is a
"money no-op." Both readings are half-right, about **different
accounts**, and the ambiguity is resolved — not by picking one, but by
stating both halves against the correct pair of accounts, conforming
exactly to `ledger-accounting-model.md` §7.7.2's now-binding vocabulary.

**The resolution.** The hold-capture posting itself — which, per §16.9's
correction this round, happens **unconditionally for every terminal-Grant
win credit, not only ones that end up staying `ACTION_HOLD_FOR_REVIEW`**
— is a real, balanced ledger transaction, **never a literal no-op**, that
does exactly two things, simultaneously, in the one posting (§16.14):

1. **It releases the lock, unconditionally.** `Dr player_locked_bonus X /
   Cr player_bonus_held X` is a real posting; `Σ signed(player_locked_bonus)`
   for this stake reaches zero, exactly as every other win outcome
   requires (§16.5a's `L(G) → 0` mandate). §16.7's "releases the lock"
   claim is **true**, and remains true this round.
2. **It creates no spendable value.** Neither leg credits `player_bonus`
   or `player_cash` — the entire value (`W` and `X` alike) lands in
   `player_bonus_held`, an account disjoint from every player-facing
   balance, unreadable by `WageringProgress`, `bonus_conversion`, or any
   withdrawal path. From the perspective of "has anything become
   wagerable, convertible, or withdrawable," **nothing has** — the
   "money no-op" reading is **true of spendable balances specifically**,
   and remains true this round.

Neither claim needed to be withdrawn; they were never actually about the
same account. `player_locked_bonus` genuinely empties (claim 1);
`player_bonus`/`player_cash` genuinely do not move (claim 2).
`player_bonus_held` is the account both claims were describing without
naming.

**No fourth "reviewing" sub-state — conforming to the actual schema.**
`bonus_held_dispositions.status` admits exactly `held`,
`resolved_reforfeit`, `resolved_route_to_cash`, `voided_by_rollback`
(`ledger-accounting-model.md` §7.7.2.5). **`held`, persisting, *is*
`ACTION_HOLD_FOR_REVIEW`'s complete state** — there is no separate
"under review" or "reviewing" value, and this document does not introduce
one. A prior looser reading of this document might have implied a
distinct in-flight sub-state between "captured" and "resolved"; there is
none — the row exists with `status = 'held'` from the instant the
hold-capture posting commits until a human (via `bonus-engine`'s
resolution flow) or a rollback (§16.15) moves it to one of the three
terminal values. Nothing here selects G-2, or which of the two
disposition-bearing terminal values a given hold eventually receives —
only the vocabulary and the interim state are conformed to the schema
`ledger-finance` has already fixed.

### 16.17 LF-18 exploit-closure proof — walked against the fix, not asserted

**The exploit, walked step by step, exactly as found.** Bonus-funded bet,
Grant `G`, wallet `w`, stake `X = 500`, correlation_id `C`.

1. **Bet.** `postBet` posts `T_bet`: `Dr player_bonus 500 / Cr
   player_locked_bonus 500` under `C`. Ledger fact: `player_locked_bonus`
   for `C` = `+500`.
2. **Silence.** No win, no rollback arrives — casino has no loss callback
   (§16.2). §16.5a(a)'s settlement-timeout sweep, per its configured
   window, finds this lock old and unresolved and posts `T_sweep`:
   `Dr player_locked_bonus 500 / Cr house_gaming 500` (case-I loss shape,
   §16.5a) — **it does not reverse `T_bet`**; it is a new, independent
   transaction. Ledger fact after: `player_locked_bonus` for `C` =
   `500 (T_bet, credit) − 500 (T_sweep, debit) = 0`.
3. **A genuine late win callback arrives** (the round actually won; the
   provider's message was merely slow, or a prior delivery was lost —
   indistinguishable from `postWin`'s point of view). `postWin` runs
   §16.4.

**Under Round 1's design (the defect).** Step 1 (old) computed
`SUM(e.amount) WHERE transaction_type = 'casino_bet' AND direction =
'credit'` — this reads **only `T_bet`**, which `T_sweep` never touches
(`T_sweep` is not a `casino_bet` transaction and does not reverse `T_bet`
either, so the `NOT EXISTS (reverses_transaction_id)` guard does not
exclude it). Old Step 1 returns `locked_amount = 500`, **unchanged by
`T_sweep`**. The old classification finds one bet, one account, one
wallet — the "clean" case — and §16.5a releases `500` a **second time**:
`Dr player_locked_bonus 500 / Cr player_bonus 500`. This transaction is
internally balanced (`500` debit `=` `500` credit — it passes `SUM(DEBITS)
== SUM(CREDITS)` for itself, and for the ledger overall, trivially,
because every well-formed transaction does). What it is **not** checked
against is whether `player_locked_bonus` actually had `500` left to give:
it did not (step 2 already reduced it to `0`). The debit drives the
account to `−500` — a locked-bonus balance below zero, violating the
ledger's own "never negative" invariant — while the credit manufactures
`500` of live, spendable-eventually `player_bonus` value with no
corresponding real exposure behind it. **This is value created from
nothing, and it is invisible to the balanced-transaction check
specifically because that check only ever verifies internal balance, never
sufficiency against a live account balance** — exactly the gap LF-18
named.

**Under this round's fix.** Step 1 (identity, unchanged) still correctly
identifies `T_bet`/`player_locked_bonus`/`w` — a permanent fact, unaffected
by `T_sweep`. Because that identity is unambiguous, Step 1b now runs:

```
net_outstanding_locked = Σ signed(player_locked_bonus) over C
                        = (+500 from T_bet's credit)
                        + (−500 from T_sweep's debit)
                        = 0
```

`net_outstanding_locked ≤ 0` → **outcome 6 fires.** `ErrLockAlreadyReleased`
is raised as a loud integrity/ops alert. **Nothing posts. No debit, no
credit, no transaction at all.** `player_locked_bonus` for `C` remains
exactly `0` — correct, matching the true ledger state — and no
`player_bonus` value is manufactured. The genuine late win is now a
named, alerted, manually-reconciled incident (an ops reviewer determines
whether the player is owed a compensating credit for the sweep's
incorrect loss-write-off, posted only through the existing four-eyes,
reason-coded manual-adjustment path — never through `postWin`'s automatic
path), exactly the accepted trade-off §16.5a(a) already discloses for a
genuine late win racing the settlement-timeout window.

**Why the ordinary, non-exploited case is unaffected (no false positive
introduced).** Re-run the same walkthrough without step 2 (no sweep, no
intervening posting): Step 1b nets only `T_bet`'s `+500` → `net_outstanding_
locked = 500 > 0` → outcome 5, the ordinary path, unchanged from what a
correctly-functioning Round 1 design would have done for a never-touched
lock. LF-18's fix only changes behavior when something has already
touched the lock — precisely the condition the defect required and the
ordinary case never has.

**Why a duplicate delivery of the *original* win cannot itself trigger
outcome 6.** A redelivered win event carries the same `provider_tx_id` as
whichever win already posted (if any) — `ledger.Post`'s own `(tenant_id,
idempotency_key)` uniqueness intercepts it **before** §16.4 ever runs
again (§16.2's existing, unmodified mechanism). Outcome 6 is reached only
by a **new** settlement attempt (a distinct `provider_tx_id`, or the
first-ever win attempt) finding the lock already exhausted by some
**other**, already-posted transaction — never by the ordinary
redelivery-of-the-same-event path already handled upstream.

**This closes LF-18.** The exploit required an outcome that reported "the
lock is still `X`" after some other transaction had already reduced it to
`0`. Step 1b cannot report that: it is defined as the live net over every
transaction sharing `C`, so by construction it reflects `T_sweep` (or any
other intervening transaction) the instant that transaction commits. There
is no query in this design that still reads the stale, bet-time-only
figure at the point a release decision is made.

### 16.18 State-machine proof, end to end

**Scope and reading of the human directive's chain.** The requested
sequence — Bet → Hold → Win → Rollback → late Win → duplicate Win →
duplicate Rollback → concurrent Win/Rollback — is proved below in two
parts. **Part A** reads "Hold" as the *locked state* itself (the stake
resting in `player_locked_bonus` between bet and settlement — this
document's ordinary usage of "the lock"/"the hold on funds") and proves
the ordinary, non-G-2 chain end to end with concrete numbers. **Part B**
separately proves the chain's distinct, explicitly-required "held value"
sense — a WIN parked in `bonus_held_dispositions` pending G-2 — since
Part A's chain, read literally, never actually produces a
`bonus_held_dispositions` row, and the human directive explicitly requires
proving "no held value is resolved after being consumed/reversed," which
only Part B's chain can exercise. Both parts share every mechanism this
section has already specified; nothing new is introduced here.

**Standing invariant, restated crisply before either walkthrough, per the
human directive's explicit requirement.** At every step below, the
provider's callback payload supplies **amounts** (the win payout `W`; the
identity of which `provider_tx_id`/`RoundID` a callback concerns) — it
never supplies, and is never consulted for, **which ledger account
receives a credit**. Every destination in both parts below is derived
exclusively from §16.3's server-side facts (`correlation_id`, `ledger_
transactions.transaction_type`, `ledger_accounts.account_type`) via
§16.4's Step 1/Step 1b queries and the live Grant-status read — never from
`event.PlayerAccountID`, never from any other payload field. This is
Round 1's invariant, reused, not weakened, by every mechanism this round
adds.

**Part A — ordinary chain (Grant non-terminal throughout).** Stake `X =
500`, payout `W = 300`, correlation_id `C2`.

| # | Transition | Posting | Running balance: `player_bonus` / `player_locked_bonus` / `house_gaming` | Balanced? |
|---|---|---|---|---|
| 1 | **Bet** | `T1`: `Dr player_bonus 500 / Cr player_locked_bonus 500` | `−500 / +500 / 0` | `500=500` ✓ |
| 2 | **Hold** (state, not a posting) | Stake rests locked; Step 1b, if queried now, reads `net_outstanding_locked = 500` | `−500 / +500 / 0` (unchanged) | n/a |
| 3 | **Win** | §16.4 outcome 5 (`net_outstanding_locked=500>0`); `T2`: `Dr house_gaming 300/Cr player_bonus 300` + `Dr player_locked_bonus 500/Cr player_bonus 500`, one transaction | `−500+300+500=+300 / 500−500=0 / −300` | `800=800` ✓; `L(G)=0` |
| 4 | **Rollback** (of `T2`) | Generic inversion of `T2`: `Cr house_gaming 300/Dr player_bonus 300` + `Cr player_locked_bonus 500/Dr player_bonus 500` | `+300−300−500=−500 / 0+500=+500 / −300+300=0` — **identical to the post-Bet state** | `800=800` ✓ |
| 5 | **late Win** | A further, separate provider event under `C2` arrives after the round sits unresolved past the settlement-timeout window: `T3` (sweep) posts `Dr player_locked_bonus 500/Cr house_gaming 500` first (net now `500−500+500−500=0`); the late win then runs §16.4 → Step 1b nets `0` → **outcome 6**, `ErrLockAlreadyReleased`, abort | `−500 / 0 / 0+500−500=0` — **unchanged by the aborted win** | n/a (nothing posts) |
| 6 | **duplicate Win** | Redelivery of `T2`'s original event (same `provider_tx_id`) — `ledger.Post`'s idempotency key returns `T2` unchanged; §16.4 never re-runs | Unchanged | n/a |
| 7 | **duplicate Rollback** | Redelivery of step 4's rollback event (same `OriginalProviderTxID = T2`) — recognized as a same-reference redelivery, idempotent no-op (existing, unmodified `postRollback` logic) | Unchanged | n/a |
| 8 | **concurrent Win/Rollback** | A second provider round under a fresh correlation_id `C3` (stake `500`, no prior events): a win callback and a rollback-of-the-bet callback arrive concurrently. The rollback takes `FOR UPDATE` on `T_bet(C3)`; whichever commits first determines the other's outcome — if the rollback commits first, `T_bet(C3)` is now reversed and the win's Step 1 (its `NOT EXISTS (reverses_transaction_id)` clause) finds zero rows → `ErrBetNotFound`; if the win commits first, it posts normally and the round now requires a **second**, explicit rollback of the *win* itself to fully undo it (this document's existing "two independent reversals" rule, §16.11) — no interleaving double-credits or loses a debit | — | Both outcomes individually balanced; no interleaving violates `SUM(DEBITS)=SUM(CREDITS)` or double-releases a lock |

**Balance proof, Part A.** At every row, the transaction posted (if any)
has equal debits and credits (checked per row above), and the *cumulative*
state after row 4 exactly equals the state after row 1 — a rollback of a
win is a true inverse, not an approximation. Row 5 proves outcome 6
prevents the exact LF-18 exploit from firing inside this same chain (a
second worked instance, independent of §16.17's dedicated walkthrough).
Rows 6–7 prove idempotency prevents duplicate application regardless of
which transaction type is redelivered. Row 8 proves the one legitimate
interleaving this document has always disclosed (§16.11) is unaffected by
any Round 2 change.

**Part B — the `held` chain (Grant terminal, disposition never resolved —
`ACTION_HOLD_FOR_REVIEW` is simply the name for this state), proving the
still-held-rollback case explicitly.** Stake `X = 400`, payout `W = 250`,
correlation_id `C4`, Grant `G2` (`pending_settlement` at settlement time).

1. **Bet.** `T4`: `Dr player_bonus 400 / Cr player_locked_bonus 400`
   under `C4`.
2. **Win, captured unconditionally.** §16.4 finds `net_outstanding_locked
   = 400 > 0` (outcome 5); the live Grant-status read finds `G2`
   terminal/`pending_settlement`. Per §16.9's corrected call order,
   `postWin` **first** posts the two-leg hold-capture transaction (§16.14)
   `T5`: `Dr house_gaming 250 / Cr player_bonus_held 250` + `Dr
   player_locked_bonus 400 / Cr player_bonus_held 400`, one balanced
   transaction (`650=650` ✓) — unconditionally, before any of the three
   G-2 actions is known. `postWin` then calls `ResolveTerminalGrantCredit`
   with `settlementLedgerTransactionID = T5.id`; `bonus-engine` writes
   `bonus_held_dispositions` row `H1`: `settlement_ledger_transaction_id =
   T5.id`, `payout_amount=250`, `released_lock_amount=400`,
   `status='held'`, and returns `heldDispositionID = H1.id`. `L(G2)=0`.
   `player_bonus_held` balance for `H1` `= 650`. No human has acted yet —
   `H1` simply sits `held`; this **is** `ACTION_HOLD_FOR_REVIEW`'s
   complete state (§16.16), not a distinct posting.
3. **Rollback of the still-held win.** A rollback naming `T5`'s
   `provider_tx_id` arrives. `postRollback` looks up
   `bonus_held_dispositions` by `settlement_ledger_transaction_id = T5.id`,
   finds `H1` with `status='held'`, and — per §16.15 — acquires `(tenant_
   id, grant_id=G2)` then `H1`'s row `FOR UPDATE`. `T5`'s original entries
   were `Dr house_gaming 250 / Cr player_bonus_held 250` (payout leg) and
   `Dr player_locked_bonus 400 / Cr player_bonus_held 400` (released-lock
   leg). Per §7.7.2.7, the reversal does **not** credit
   `player_locked_bonus` back (that would resurrect a lock that has
   already, correctly, closed to zero) — both legs reverse straight to
   `house_gaming` instead. `postRollback` posts, in the **same**
   transaction as the guarded status update, `T6`: `Dr player_bonus_held
   250 / Cr house_gaming 250` **+** `Dr player_bonus_held 400 / Cr
   house_gaming 400` — debiting `player_bonus_held` for the full `650`
   the hold-capture posting credited it, crediting `house_gaming` back the
   same `650` it originally paid out, and **never** touching
   `player_locked_bonus`. In the same transaction:
   `UPDATE bonus_held_dispositions SET status='voided_by_rollback',
   resolution_ledger_transaction_id=T6.id WHERE id=H1.id AND
   status='held'` — one row affected, succeeds. `player_bonus_held`
   balance for `H1` after: `650 − 650 = 0`. **Balanced** (`650=650`), and
   `H1` is now terminal (`voided_by_rollback`) — **no held value remains
   to ever be resolved.** Since `H1` was `G2`'s only outstanding `AOE`
   component, `postRollback` also calls `bonusengine.
   RecheckGrantExposure(ctx, tx, G2, T6.id, TriggerHeldDispositionResolved)`
   in the **same** transaction (§16.21) — `bonus-engine` finds
   `AOE(G2, ·) = ∅` and flips `G2.status` from `pending_settlement` to its
   already-recorded `terminal_resolution` value, with a Progress entry
   citing `T6`.
4. **A late resolution attempt arrives after the void** (e.g. a queued
   staff action to apply G-2's answer, processed after the rollback
   already committed). It attempts the same guarded update pattern
   (`UPDATE ... WHERE status='held'`) as part of its own resolution
   transaction (HR-25, `ledger-accounting-model.md` §7.7.2.9) — finds
   `status='voided_by_rollback' ≠ 'held'` — **zero rows affected,
   rejected**. **No held value is resolved after being reversed.** This
   is the exact property the human directive requires proved.
5. **duplicate Rollback of `H1`.** A redelivered rollback naming the same
   `T5`/`provider_tx_id` hits `ledger.Post`'s idempotency key on the
   reversal transaction itself — no second `T6`, and thus no second
   attempt at the guarded update at all.
6. **concurrent Win/Rollback, generalized to concurrent
   Resolve/Rollback of the same held record** (the directive's
   requirement to prove no double-resolution under concurrency, restated
   for the `held` chain specifically). Suppose instead of step 4's
   sequential late arrival, a human's resolution action and a rollback of
   `T5` are dispatched **at the same instant**. Both follow HR-25's lock
   order: `(tenant_id, G2)` advisory lock first (they serialize on this
   alone already), **then** `H1`'s row `FOR UPDATE`. Whichever acquires
   the row lock first commits its guarded update
   (`status: 'held' → {'voided_by_rollback' | 'resolved_reforfeit' |
   'resolved_route_to_cash'}`) and releases it; the second finds
   `status ≠ 'held'` and its own guarded update is a zero-row no-op —
   its enclosing transaction is rejected before positing any further
   money movement (a resolution action that finds its guard clause
   affected zero rows does not proceed to move value at all, by the same
   "guarded update, never check-then-act" discipline). **No
   interleaving produces two dispositions of `H1`, and no interleaving
   resolves value that the other side has already reversed or the
   reverse.**

**Balance proof, Part B.** `T5` and `T6` are each internally balanced. At
the total-value level, `T5` credited `player_bonus_held` `650` and `T6`
debited it back to `0` — no value survives in a held state after the
void, which is the property that matters for "no held value is resolved
after being reversed." **One sub-detail flagged, not fully re-derived
here, per this document's own discipline against inventing ledger-finance's
posting shapes**: because `T6`'s reversal of the released-lock leg
deliberately credits `house_gaming` rather than restoring
`player_locked_bonus` (§7.7.2.7's own instruction), that leg's reversal
newly crosses `BONUS_SET`'s boundary in the outward direction, which by
Rule B2 (extended, §6.3.2) may itself require a mirror pair
(`Dr promo_liability` against the reversed `bonus_expense`, or equivalent)
that `T5`'s original leg never needed (it was `BONUS_SET`-internal).
Whether `T6` needs such a mirror, and its exact shape, is a posting-detail
question for `ledger-finance`'s own mirror-generator design (§6.3.2's Rule
B2 is `ledger-finance`-owned) — this document proves the **state-machine**
property (no double-resolution, no resurrection of a closed lock, `H1`
correctly terminal) and flags the mirror-completeness question rather than
asserting a specific answer it has not verified against Rule B2's actual
implementation. No step manufactures value at the level this document can
verify, no step destroys the audit trail (both `T5` and `T6` remain as
permanent, append-only ledger rows; `H1`'s row is never deleted, only
status-transitioned), and no step in either race (step 4 or step 6) allows
a `held` record to be resolved twice or resolved after being voided.

### 16.19 Adversarial scenarios, Round 2 — the human directive's named set

The eight scenarios named explicitly by the human directive authorizing
this round, each restated as its own item with the invariant tested and
why this design satisfies it (or, for #8, why it explicitly does not yet
and is routed elsewhere):

1. **Duplicate win.** *Invariant*: no double credit. *Satisfied*:
   `ledger.Post`'s `(tenant_id, idempotency_key)` uniqueness intercepts a
   redelivered win before §16.4/§16.9 ever re-run (§16.2, unchanged this
   round; proved again in §16.18 Part A row 6).
2. **Duplicate rollback.** *Invariant*: no double reversal. *Satisfied*:
   existing same-reference-redelivery detection in `postRollback`
   (unchanged), now confirmed to also cover a rollback targeting a `held`
   record (§16.15 — the reversal's own idempotency key prevents a second
   `T6`-shaped transaction, so the guarded status update is never even
   re-attempted).
3. **Concurrent win/rollback.** *Invariant*: no interleaving
   double-credits, loses a debit, or double-resolves a held record.
   *Satisfied*: the original bet/win race is serialized by the original
   transaction's row lock (§16.11, unchanged); the held-record-specific
   race is serialized by HR-25's `(tenant_id, grant_id)`-then-row lock
   order, proved exhaustively in §16.15 and walked concretely in §16.18
   Part B step 6.
4. **Late callback.** *Invariant*: correctness independent of elapsed
   time. *Satisfied*: §16.10.4's timing-invariance proof (unchanged) plus,
   new this round, LF-18's outcome 6 specifically guarantees a late
   callback arriving after an intervening settlement-timeout sweep can
   never over-release (§16.17).
5. **Callback after Grant expiry.** *Invariant*: no stale Grant-status
   snapshot is ever used. *Satisfied*: INV-TG's live `FOR UPDATE`
   Grant-status read, re-evaluated every time, never cached from trigger
   time (§16.10.4, unchanged).
6. **Callback after Grant cancellation.** *Invariant*: identical to
   expiry. *Satisfied*: §T.10's symmetry between expiry and cancellation,
   unchanged, confirmed to extend to every mechanism this round adds
   (nothing in §16.14–§16.18 distinguishes trigger *kind*, only trigger
   *timing* relative to settlement).
7. **Rollback of a held win.** *Invariant*: a `held` disposition is never
   resolved after being voided, and voiding never resurrects a lock that
   has already, correctly, closed to zero. *Satisfied*: §16.15's guarded
   compare-and-swap, proved against concurrent resolution in §16.15's own
   three-interleaving proof and walked concretely in §16.18 Part B steps
   3–4 and 6. **Closed this round.**
8. **Rollback of a resolved win.** *Invariant*: a rollback must never
   silently succeed against value that has already left the ledger's
   held/locked representation, nor silently fail in a way that loses the
   audit trail or posts an impossible (negative, unbalanced, or
   unauthorized) balance. *Not satisfied by a decided mechanism* — this
   is **LF-10**, `ledger-finance`'s general, still-open question (its own
   Round 2 text reconfirms it: `ledger-accounting-model.md` §7.7.2.11).
   This document's own contribution is negative but load-bearing: it
   never guesses an answer, never posts a negative/impossible balance
   without an explicit `ledger-finance`-owned decision permitting one, and
   never silently reuses the `held`-case mechanism (§16.15) for this
   structurally different case (the value is no longer in a ledger
   account this document's own mechanisms can locate and reverse
   symmetrically — see §16.20).

### 16.20 LF-10's posture, stated precisely — why this document does not close it now

**What is being asked, precisely.** A rollback arrives for a win whose
credit already left the ledger's held/locked representation entirely —
either an ordinary win that posted to `player_bonus` and was later swept
into `promo_liability` by an unrelated forfeiture (§16.11's "resolved
win" row), or a `bonus_held_dispositions` record already moved to
`resolved_reforfeit`/`resolved_route_to_cash` (this round's new case,
enabled by §16.14's own mechanism). In both shapes, the debit `postRollback`
would need to post has no guarantee of finding a sufficient balance to
debit, because the value has already been spent, converted, or
re-forfeited by a transaction with no knowledge that a rollback might one
day arrive.

**Why `casino` does not decide this, even though it now owns strictly
more of the mechanism than it did in Round 1.** Three independent
reasons, none of which weaken with this round's other closures:

1. **It is a ledger-wide policy question, not a casino-specific one.**
   The identical shape arises for sportsbook (a settled bet's win swept
   into `promo_liability`, then rolled back) and, after this round, for
   the `bonus_held_dispositions` resolved case too. Deciding "permit a
   negative balance as a recorded clawback / route the shortfall to a
   receivable / reject-and-alert" once, for casino only, would produce
   exactly the kind of per-domain divergence §16.6's Sportsbook row (as
   revised this round) already found to be a defect, not a feature, when
   Round 1 made a smaller version of the same mistake with the LF-18
   query itself.
2. **`ledger-finance` has already, explicitly, kept this open — twice.**
   Round 1's own LF-10 finding withdrew this document's prior "fail
   closed" pre-commitment specifically because that was casino unilaterally
   answering a question `ledger-finance` owns. This round, `ledger-
   finance`'s own text — closing the *adjacent*, narrower still-held
   sub-case in the same section — states in its own words that the
   resolved-win sub-case "remains open, still routed to `ledger-finance`
   generally" (`ledger-accounting-model.md` §7.7.2.7, §7.7.2.11). Deciding
   it here now would silently override `ledger-finance`'s own, just-made,
   explicit choice not to decide it yet — a worse violation of ownership
   than Round 1's original mistake, not a correction of it.
3. **CLAUDE.md's own ownership rule applies directly.** "No specialist
   redesigns shared architecture unilaterally — cross-cutting changes go
   through the architect and are recorded in `docs/decisions/`." A
   balance-insufficiency/compensating-entry policy for a late rollback is
   exactly this kind of cross-cutting ledger invariant (it interacts with
   B1's aggregate, `ledger.Post`'s sufficiency-check mechanism, and
   whatever HR-9-gated `player_bonus` posting eventually ships) — squarely
   `ledger-finance` + `architect` territory, not a casino integration
   detail.

**What this document does assert, and has always asserted, per CLAUDE.md's
own compensating-entry rule** ("Corrections are compensating entries,
never edits or deletions of historical entries"): whatever shape
`ledger-finance` eventually chooses, it is a **new posting**, never an
edit to `T5` (or any historical transaction), and the `bonus_held_
dispositions`/ledger audit trail for the original event remains intact
regardless of the eventual answer. This constrains the *shape* any answer
must take without selecting *which* of the three named options
(clawback / receivable / reject-and-alert) it is.

**Status: `NOT IMPLEMENTED`, routed to `ledger-finance`, unchanged in
substance from Round 1's own posture — this round only narrows its scope**
(the still-held sub-case is no longer part of what LF-10 covers; the
resolved-win sub-case is all that remains of it).

### 16.21 The Grant-status-finalization seam — naming `bonusengine.RecheckGrantExposure` (`casino`'s own Phase 2 finding, closed this round)

**The gap, stated precisely.** `10-bonus-engine-architecture.md` N1.4
step 5a asserts that every value-reducing closing event — a loss-grading
settlement, a VOID, a ROLLBACK, or (casino only, N1.7) an elapsed
settlement window with no WIN observed — that clears a Grant's last
outstanding `AOE` component flips `G.status` from `pending_settlement` to
its already-recorded `terminal_resolution` value **"in the same
transaction"** as the closing event itself, with (per doc10's own
concurrency proof, N1.5) **no window** between the two. Step 5c makes the
identical claim for the `voided_by_rollback` sub-case (a held win rolled
back). Both claims are correct **as a requirement on the outcome** — but
neither claim, nor anything else in doc10, nor this document's own prior
text, ever named a concrete call, seam, or function through which
`casino`'s own transaction (`postRollback`, or §16.5a(a)'s
settlement-timeout sweep) would actually reach `bonus-engine`'s
Grant-status write. This is the exact same category of gap §16.9 already
closed for the *value-creating* side (`ResolveTerminalGrantCredit`,
named against doc10 §T.7's own "a new call-back... that does not exist
today" framing) — left open here, on the *value-reducing* side, until
now. Contrast the WIN path, which has always correctly named
`ResolveTerminalGrantCredit` as this seam (§16.9); no equivalent existed
for a LOSS, VOID, ROLLBACK, or settlement-timeout resolution. This section
closes that gap, symmetrically.

**The seam:**

```
bonusengine.RecheckGrantExposure(
    ctx context.Context,
    tx <db tx>,
    tenantID uuid.UUID,
    grantID uuid.UUID,
    triggeringLedgerTransactionID uuid.UUID,
    triggerKind GrantExposureTriggerKind,
) (newStatus bonusengine.GrantStatus, err error)
```

**Signature reconciled against the real, shipped, tested code (Stage
4H-B1 Wave 2 Phase 3/7) — this pseudocode previously omitted `tenantID`
entirely. `internal/bonus.RecheckGrantExposure`'s real signature
(`internal/bonus/held_disposition_ops.go`) takes an explicit `tenantID
uuid.UUID` parameter alongside `grantID`, used to scope its live `AOE`
recompute's read queries (§7.10's R1–R4 family plus the `player_bonus_
held` balance read) — every one of which is tenant-scoped by
construction elsewhere in this codebase, so a seam that reads across
those queries needs `tenantID` explicitly rather than re-deriving it a
second time from the Grant row (`ResolveTerminalGrantCredit`, by
contrast, performs exactly one such re-derivation via its own
`GetGrantByID` call and needs no separate parameter — the two seams are
not required to share an identical resolution strategy, and do not).
`casino`'s three call sites (§16.21 below) already hold `tenantID` from
their own enclosing transaction and pass it through unchanged; this is
not a new value `casino` must derive.**

**This is a named seam, not a direct write — matching this document's own
domain-boundary discipline everywhere else in §16 (§16.9's identical
framing for the value-creating side; §16.14's "`casino` does not design,
migrate, or query [`bonus_held_dispositions`] directly" for the holding
record).** `casino` never executes `UPDATE bonus_grants SET status =
... WHERE id = ?` itself, in any of the call sites below — it only calls
this function and lets `bonus-engine`'s own implementation read `AOE(G,
·)` live (N1.3) and decide.

**Inputs, and why each is there:**
- `grantID` — which Grant's exposure to recheck. `casino` already has
  this at every call site below, from the same live Grant-status read
  each site already performs for its own, unrelated reason (deciding how
  to post the value-reducing event itself).
- `triggeringLedgerTransactionID` — the id of the value-reducing ledger
  transaction `casino` has already built (and, depending on call order
  below, already posted or is about to post) in this same database
  transaction — carried through purely so `bonus-engine`'s own Progress
  entry (doc10 §1.3) can cite exactly which ledger fact triggered this
  recheck, mirroring `settlement_ledger_transaction_id`'s identical role
  at §16.9/§16.14. `casino` does not need this field to serve any purpose
  of its own.
- `triggerKind` — a descriptive tag (e.g. `TriggerCasinoRollback`,
  `TriggerCasinoSettlementTimeout`, `TriggerHeldDispositionResolved`),
  used only for `bonus-engine`'s own audit trail/observability, never
  branched on by the recheck logic itself, never by `casino`.

**What it returns/does.** Inside the same transaction and the same
`(tenant_id, grant_id)` advisory lock `casino`'s own call site already
holds, `bonus-engine`'s implementation recomputes `AOE(G, ·)` live (N1.3
— `casino` does not need to know its internals, only that this recompute
is what decides the outcome): if `AOE(G, ·) = ∅`, it flips `G.status` from
`pending_settlement` to the value already recorded in that Grant's
`terminal_resolution` field (N1.4) and appends a Progress entry citing
`triggeringLedgerTransactionID`, returning that new terminal status as
`newStatus`; if `AOE(G, ·)` remains nonzero, `G.status` is left at
`pending_settlement` and `newStatus` reports that unchanged value.
`casino` does not branch on `newStatus` — like `heldDispositionID`
(§16.9), it exists for logging/observability/testing, not for `casino`'s
own control flow. **`casino` never computes or asserts, itself, whether a
given closing event was "the last remaining `AOE` component" — that
determination belongs entirely to `bonus-engine`'s own live recompute.**
`casino`'s only obligation is the trigger condition below: call the seam
whenever this event *might* have been the last component, and let
`bonus-engine` decide whether it actually was.

**Trigger condition — the one thing `casino` must get right, stated so it
requires no knowledge of `AOE`'s internals.** Call this seam, in the same
transaction, immediately after building (and, at the sweep, after
posting) the value-reducing entry, **whenever the live Grant-status read
that call site already performs finds `G.status = pending_settlement`.**
That live read already exists at every site below for an unrelated
reason (deciding whether the value-reducing posting interacts with a
still-open Grant at all, per §16.10.2's INV-TG mechanism) — this section
adds no new read, only the follow-up call once that read comes back
`pending_settlement`. If the Grant is ordinarily non-terminal, or already
fully terminal outside of `pending_settlement` (N1.6 Scenario 4), no call
is made — there is nothing for `bonus-engine` to finalize.

**Exactly where `casino` must call it — every site enumerated, none
open-ended:**

1. **`postRollback`, for the plain lock-rollback of a bet before any
   win/loss is known** (§16.8's second bullet). After posting the
   rollback's own entries (whatever shape §16.10.3's still-open tension
   eventually settles on — this seam's call is unaffected by how that
   tension resolves, since either shape is value-reducing under doc10
   N1.4 step 5a), if the live Grant-status read finds `pending_settlement`,
   call the seam with `TriggerCasinoRollback` before commit.
2. **§16.5a(a)'s settlement-timeout sweep**, per Grant, per swept lock.
   After posting that Grant's `casino_settlement_timeout` transaction, if
   the live Grant-status read finds `pending_settlement`, call the seam
   with `TriggerCasinoSettlementTimeout` before that Grant's write commits
   (§16.5a(a) now states this as a required property, not merely
   cross-referenced here).
3. **`postRollback`, for the held-win rollback transition** (§16.15). This
   is `voided_by_rollback` clearing a `HeldDisposition` `AOE` component
   (doc10 N1.4 step 5c's own closing bullet), not step 5a — but the same
   gap applies: after the guarded compare-and-swap (`UPDATE
   bonus_held_dispositions SET status='voided_by_rollback' ... WHERE
   status='held'`) affects exactly one row, if the live Grant-status read
   finds `pending_settlement`, call the seam with
   `TriggerHeldDispositionResolved` before commit — walked concretely in
   §16.18 Part B step 3. A zero-row compare-and-swap (the record already
   left `held`) calls nothing; there is no new closing event to recheck
   against.

**Not called from `postWin`'s hold-capture path (§16.9/§16.14).**
Capture never reduces `AOE(G, ·)` — it *replaces* a `LockedExposure`/
`InFlightExposure` member with a `HeldDisposition` member of equal value
(N1.3, LF-20's "replaced, not cleared"), so it can never be the event that
brings `AOE` to `∅`. Only a genuinely `AOE`-reducing event (a value
disappearing from the set entirely, not moving within it) is ever a
candidate trigger for this seam.

**Concurrency — no new lock participant.** Every call site above already
holds `(tenant_id, grant_id)` before this seam is ever reached (INV-TG's
own live-read requirement, §16.10.2; HR-25's row lock, for site 3
specifically, §16.15) — this seam acquires nothing beyond what its own
call site already holds, mirroring §16.14's identical "no new lock
participant" finding for hold-capture *creation*.

**What this does not do.** It does not select G-2, does not decide
§16.10.3's open tension about whether an ordinary lock-rollback is G-2-
relevant at all (site 1's call fires either way, per that bullet's
correction), and does not redesign any of `bonus-engine`'s own N1.3/N1.4
mechanism — it names the boundary `casino`'s own transactions cross to
reach it, exactly as §16.9 already does for the value-creating side.

## Cross-references

- ADR: `docs/decisions/0025-casino-provider-abstraction-and-game-session-model.md`
- Financial flows: `docs/architecture/financial-transaction-flows.md` §5-7
- Ledger model: `docs/architecture/ledger-accounting-model.md` §6.3.3.1, §6.4 (cases A/B/F/G/H/I), §6.5 (migration 0048), §6.6 (Model C)
- Bonus-origin holding representation (load-bearing this round): `docs/architecture/ledger-accounting-model.md` §7.7.2 (`player_bonus_held`, `bonus_held_dispositions`, HR-25, LF-12, LF-19/LF-20/LF-22)
- Phase 2 findings this round closes/adopts: `docs/governance/wave-1.5-fixwave-phase2-report.md` (LF-18, held-win rollback gap), `docs/governance/task-registry.md`
- Bonus accounting: `docs/decisions/0032-bonus-accounting.md` §3, §5
- Terminal-Grant Technical Contract: `docs/architecture/10-bonus-engine-architecture.md` §T.1-T.13
- Grant terminal-state invariant/`AOE`/hold-capture mechanism (load-bearing this round, §16.9/§16.14/§16.21): `docs/architecture/10-bonus-engine-architecture.md` §N1, especially N1.3 (`AOE`), N1.4 step 5a/5b/5c (the mechanism §16.9/§16.14/§16.21 reconcile against)
- Human Decision Register: `docs/decisions/0039-human-decision-register-stage-4h-b0-r7.md`, Decision 2
- Task registry: `docs/governance/task-registry.md`, Stage 4H-B1 Wave 1 reconciliation finding 1, Wave 1.5
- Payment orchestration (the pattern this mirrors): `docs/architecture/payment-orchestration.md`
- Provider agnosticism precedent: `docs/decisions/0022-payment-provider-agnosticism-and-capability-model.md`
- Provider abstraction pattern: `docs/decisions/0004-provider-abstraction-pattern.md`
