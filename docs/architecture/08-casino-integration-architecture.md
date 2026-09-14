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
distinguishable sentinel error: game exists (`ErrGameNotFound`) → platform-
enabled (`ErrGameDisabled`) → tenant/brand opted in (`ErrGameNotAvailable`)
→ not jurisdiction-blocked (`ErrJurisdictionBlocked`) → asset supported by
the game (`ErrInvalidInput`) → provider capability active and asset-
supporting (`ErrProviderUnavailable`) → provider registered
(`ErrUnknownProvider`) → provider healthy/circuit-closed
(`ErrProviderUnavailable`) → mint session → call `provider.Launch`. Per-
player jurisdiction resolution is `TODO(jurisdiction)` — the identical open
scope boundary `payment-orchestration.md` §4 already carries for payment
routing; a `nil` `JurisdictionCode` skips the check rather than silently
ignoring a resolved one.

## 6. Provider callback authentication — `ARCHITECTURAL DECISION`

A provider callback is **not** an authenticated platform principal — there
is deliberately no bearer-token middleware on
`POST /v1/webhooks/casino/{tenantSlug}/{providerID}`
(`internal/httpserver/casino_handlers.go`), identical to the payments
webhook's own reasoning. Two things establish trust instead, in order:

1. **Tenant resolution from the URL only.** The handler resolves tenant
   scope from the path's `tenantSlug` segment *before* opening a
   transaction and before any payload byte is inspected — an unknown slug
   and a suspended tenant return the identical not-found response
   (enumeration resistance), mirroring `newPaymentWebhookHandler` exactly.
2. **Adapter-specific signature verification inside `HandleCallback`.**
   The platform never invents a generic signature scheme; each adapter
   authenticates its own payload however its real vendor requires.
   `MockCasinoProvider` verifies an HMAC-SHA256 over the payload's
   identifying fields (`mock.go`'s `sign`/`HandleCallback`) using
   `hmac.Equal` (constant-time) — a caller who does not know the mock
   instance's `signingSecret` cannot construct a payload `HandleCallback`
   will accept, and a malformed/unsigned payload is rejected before any
   field is used to construct a `CallbackEvent`.

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
| Bet | 5 | debit `player_cash`, credit `house_gaming` | invariant #15: balance locked (`SELECT ... FOR UPDATE`) and checked **inside** the same transaction as the debit; insufficient funds → `OutcomeDeclined`, nothing posted |
| Win | 6 | debit `house_gaming`, credit `player_cash` | a win naming a round with no matching prior bet under the SAME tenant is `ErrBetNotFound` — an integrity alert (provider protocol violation), never silently posted |
| Rollback | 7 | exact inverse of whichever of Flow 5/6 the named original posted | looked up by `(tenant_id, provider_id, provider_tx_id)`; an original never seen writes a `tombstone` (mirrors `payments.postDepositReversalTombstone`), never an error |

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
`casino_game_availability`, `casino_provider_capabilities`, and
`casino_launch_sessions` all carry `ENABLE`+`FORCE ROW LEVEL SECURITY`.
The first two use the `tenant_isolation` staff-only pattern (with the
player-scope-exclusion guard — `app.player_account_id` must be unset);
`casino_launch_sessions` uses the dual `tenant_staff_scope` +
`player_self_scope` (SELECT-only) pattern, identical shape to
`withdrawal_requests` (migration 0026). Every composite foreign key pins a
launch session to its actual owning tenant/brand/player/wallet — see
migration 0035 for the exact constraints. Casino provider credentials are
config, never source code; Stage 4A introduces no real credential of any
kind.

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

## 15. Sportsbook is a separate future concern

Casino and sportsbook providers are explicitly **not** assumed to share
this interface. Sportsbook's own future chain — Platform Identity →
Wallet/Ledger → canonical Sportsbook interface → Sportsbook Adapter A/B →
external provider — gets its own abstraction in
`09-sportsbook-architecture.md`, built once real API documentation for both
contracted providers is supplied (per the business requirement: two
sportsbook providers via API, never invented ahead of the real docs).

## Cross-references

- ADR: `docs/decisions/0025-casino-provider-abstraction-and-game-session-model.md`
- Financial flows: `docs/architecture/financial-transaction-flows.md` §5-7
- Ledger model: `docs/architecture/ledger-accounting-model.md`
- Payment orchestration (the pattern this mirrors): `docs/architecture/payment-orchestration.md`
- Provider agnosticism precedent: `docs/decisions/0022-payment-provider-agnosticism-and-capability-model.md`
- Provider abstraction pattern: `docs/decisions/0004-provider-abstraction-pattern.md`
