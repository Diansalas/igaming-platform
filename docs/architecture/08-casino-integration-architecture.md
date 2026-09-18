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
| Bet | 5 | debit `player_cash`, credit `house_gaming` | invariant #15: balance locked (`SELECT ... FOR UPDATE`) and checked **inside** the same transaction as the debit; insufficient funds → `OutcomeDeclined`, nothing posted. The wallet debited is resolved from the platform's own `casino_launch_sessions` row named by the callback's `session_id` — never from a payload-supplied `player_account_id` — and rejected outright for a missing/unknown/wrong-provider/demo-mode/asset-mismatched session |
| Win | 6 | debit `house_gaming`, credit `player_cash` | a win naming a round with no matching, still-valid (never rolled back) prior bet under the SAME tenant is `ErrBetNotFound` — an integrity alert (provider protocol violation), never silently posted. The wallet credited is resolved from the round's own bet transaction's ledger entries, never from the win callback's own `player_account_id` |
| Rollback | 7 | exact inverse of whichever of Flow 5/6 the named original posted | looked up by `(tenant_id, provider_id, provider_tx_id)` under a row lock (`SELECT ... FOR UPDATE`, closing a concurrent-double-reversal race found in review); an original never seen writes a `tombstone` (mirrors `payments.postDepositReversalTombstone`), never an error |

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

### 16.4 `postWin`'s exact destination-resolution design (revised — LF-1, LF-7)

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
these apart, and more importantly it carries no **amount** for the
quantity §16.5a's lock-release mechanism needs (the locked stake `X`,
which is a different number from the win's own payout amount). Reading
the **credit** leg of the lock instead — genuinely reusing
`ledger-accounting-model.md` §6.3.3.1's own "variant 1" query verbatim,
restricted to the locked account types, with a wallet/asset-scoped
aggregate amount — answers "is this bet currently locked, in which
account, for how much" directly, which is the actual question `postWin`
needs answered, and gives §16.5a's release step the exact `X` it needs
without a second query or a second design.

**Step 1 — locked-origin resolution** (only outcome that ever requires a
G-2 read; only outcome a bonus-funded bet under the mandated shape can
produce):

```sql
-- DESIGN ONLY. ledger-accounting-model.md §6.3.3.1 "variant 1", reused
-- verbatim (transaction_type substituted). Reads the CREDIT leg -- the
-- account the stake is actually SITTING IN today -- restricted to the
-- locked account types only, grouped so multi-row outcomes (LF-7/LF-8)
-- are visible to the caller rather than collapsed by a bare LIMIT 1.
-- Never reads event.PlayerAccountID or any other payload field.
SELECT t.id AS bet_transaction_id, la.account_type, la.wallet_id,
       la.asset_code, SUM(e.amount) AS locked_amount
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
fail-closed branch, none a guess:

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
   — **not** an automatic abort-forever condition.
4. **Exactly one `bet_transaction_id`, but that single transaction's own
   result rows span more than one distinct `account_type`** → a genuine
   same-instruction mixed-origin posting. This is the case HR-2 is
   actually meant to make unreachable (a single posting instruction with
   two funding origins) — kept as a named, defense-in-depth sentinel,
   `ErrMixedFundingUnsupported`, distinct from outcome 3 precisely so an
   operator can tell "this looks like an HR-2 regression" apart from
   "this is an expected, unhandled multi-bet round."
5. **Exactly one `bet_transaction_id`, one `account_type`, one
   `wallet_id`** → the clean, single-bet case. Proceed to the destination
   map below, using this row's `locked_amount` (Step 1) as the exact
   quantity §16.5a's lock-release step debits.

**Destination map** (the only part of `postWin` that changes what account
receives the credit; the posting mechanics — `ledger.Post`, idempotency
key, audit record — are unchanged):

| Resolution | Win credit destination | Lock release (§16.5a)? | G-2 involved? |
|---|---|---|---|
| Step 1: `player_locked_cash` | `player_cash` | Yes — `Dr player_locked_cash locked_amount / Cr player_cash locked_amount`, bundled in the same transaction | No — G-2 is defined only over `player_bonus`/`player_locked_bonus` (doc10 §T.7). Not producible by today's `postBet` (§16.10); included for completeness only |
| Step 1: `player_locked_bonus`, Grant non-terminal | `player_bonus` | Yes — `Dr player_locked_bonus locked_amount / Cr player_bonus locked_amount`, bundled in the same transaction | No — ordinary case. This is the shape LF-1 restores as actually reachable |
| Step 1: `player_locked_bonus`, Grant terminal | §16.9's seam decides | Yes, but bundled into whatever §16.9's disposition specifies (§16.5a) | **Yes — this is G-2 itself** |
| Step 2: `player_cash` | `player_cash` | No — never locked | No. **Unchanged** from today's actual behavior for every bet `postBet` can currently produce |
| Step 2: `player_bonus`, no Step-1 lock found | **Abort** — new sentinel `ErrBonusBetNotLocked`, integrity alert | N/A | N/A — under the mandated shape (§16.10.1) this is unreachable in correctly-functioning code; never silently credited, since guessing its destination is exactly the exploit this section closes |

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
| Bonus-funded wager, Grant already terminal | §16.9's seam is invoked | **Yes — this is G-2 itself** |
| Locked cash | Same row as cash-funded above; a lock has no Grant, so no G-2 dimension exists | No |
| Locked bonus | Same row as "Grant non-terminal"/"Grant terminal" above — "locked bonus" *is* the `player_locked_bonus` origin case, not a fifth case | See above |
| Mixed funding (single-instruction, HR-2 regression) | §16.4's outcome 4 (`ErrMixedFundingUnsupported`) — whole transaction aborts, nothing posted | N/A — rejected before any credit is considered |
| Multi-bet round (legitimate, LF-8) | §16.4's outcome 3 → §16.4a's manual-reconciliation path, **not** an automatic permanent abort | Deferred to manual review; may involve G-2 once disambiguated |
| Correlation-ID wallet collision (LF-7) | §16.4's outcome 2 (`ErrCorrelationWalletCollision`) — whole transaction aborts, integrity alert | N/A — rejected before any credit is considered |
| Rollback | `postRollback`'s existing generic entry-inversion (§16.1) — reverses whatever the original actually posted, origin-safe by construction. **A rollback of a WIN that already credited `player_bonus`, where the Grant has since gone terminal and swept that balance into `promo_liability` (§16.10), can find insufficient `player_bonus` balance to debit.** What happens then — permit the negative balance as a recorded clawback, route the shortfall to a receivable, or reject-and-alert — is **`ledger-finance`'s decision, not casino's** (LF-10); this document does not pre-select an answer (§16.11, §16.12) | Not casino's call — see note |
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

**Win case — falls out of §16.4's fix, with one addition.** Once §16.4
correctly resolves a `player_locked_bonus` origin and its `locked_amount`
`X` (Step 1), the settlement transaction bundles **two** entry pairs in
one `ledger.Post` call (a single balanced transaction, `ledger.Post`'s
existing multi-entry-pair support — no new capability):

1. The ordinary win payout: `Dr house_gaming W / Cr player_bonus W`
   (Grant non-terminal) or whatever §16.9's disposition specifies (Grant
   terminal).
2. The lock release: `Dr player_locked_bonus X / Cr player_bonus X` (or,
   under a terminal-Grant disposition, credited to wherever that
   disposition routes it instead — see below).

This closes `L(G)` to zero on every win, including the terminal-Grant
branch — **explicitly required by this fix, not left to `bonus-engine`**:
per the fix-wave directive, `internal/casino` does not need to design
`bonus-engine`'s own holding mechanism for a *held* win payout (if G-2 is
answered `ACTION_HOLD_FOR_REVIEW`), but it **does** need the lock itself
to resolve rather than dangle. **New cross-dependency found during this
fix, not previously named**: `ACTION_HOLD_FOR_REVIEW`'s seam disposition
must name a destination for the *released lock amount* `X` (distinct from
wherever it holds the *payout* `W`) — a suspense/hold account this
document does not name, since it belongs to `bonus-engine`'s own parallel
hold-mechanism design. Flagged in §16.13 as an open cross-dependency for
that design to specify, not invented here.

**Loss case — the harder, previously entirely unaddressed gap.** No
inbound event signals a loss. Two designs, per this fix-wave's own (a)/(b)
menu:

- **(a) Bounded, per-provider/per-jurisdiction-configurable settlement
  window — RECOMMENDED.** A scheduled sweep (operationally the same
  pattern as the existing hourly reconciliation sweep, not a new
  architectural primitive) finds `player_locked_bonus` locks — identified
  by a `casino_bet` transaction with a Step-1-shaped credit leg — that are
  **both** (i) older than a configured window `W` and (ii) genuinely
  unresolved: no `casino_win` or `casino_rollback` transaction exists
  under the same `correlation_id`, **and** no `ErrTerminalGrantCreditUnresolved`
  rejection (§16.9) was ever recorded for it (see the exclusion below —
  found during the §16.7 re-walk, not assumed at the outset). For each
  genuinely unresolved, expired lock, the sweep posts a new,
  system-initiated transaction type, `casino_settlement_timeout`,
  resolving the stake as a loss with the identical case-I accounting
  shape a real loss would use: `Dr player_locked_bonus locked_amount /
  Cr house_gaming locked_amount`, mirrored per the existing `BONUS_SET`
  rule (B1 extended) exactly as an ordinary loss would be — no new
  posting shape, reused verbatim.

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
| **Sportsbook** | No casino-specific assumption breaks sportsbook's own future design — §16.4 reuses sportsbook's own §6.3.3.1 query verbatim rather than diverging from it, and §16.9's seam is written product-agnostically (keyed on `correlation_id`/Grant, not on casino specifically) so `internal/sportsbook` can call the identical seam later without a second design pass | Confirmed no divergence introduced |
| **Risk** | No change. `risk.Evaluate` is not consulted by `postWin` today (doc comment, lines 830-837) and this section does not add a call — a terminal-Grant credit decision is not a Risk-shaped exposure/limit question | — |
| **RG** | No change. `postWin` deliberately does not call `evaluateAndAuditEligibility` (unchanged rationale, lines 830-837) — settling an already-legitimately-placed bet is not gated by the player's current RG status. G-2's own resolution (whichever action a human selects) may itself have RG/AML implications (a routed-to-cash credit becoming withdrawable) but that is Decision 2's own province, not a new RG call site this design adds | — |
| **Reconciliation** | No change to the hourly ledger-vs-projection sweep mechanism. A held/failed-closed G-2 credit (§16.9) must not create an unreconciled ledger residue — see §16.13's open item on the holding mechanism, explicitly deferred by doc10 §T.7 itself to `ledger-finance`. **New (LF-4):** §16.5a's settlement-timeout sweep is a second, distinct scheduled job operating on the same locked-balance projections — it must run and be reconciled independently of the hourly ledger-vs-projection sweep, and its own postings (`casino_settlement_timeout`) are ordinary, audited ledger transactions the existing sweep already covers without modification | Open item (holding mechanism) plus one new, not-yet-authorized mechanism (settlement-timeout sweep, §16.5a) |

### 16.7 Exploit closure proof — a bonus-funded win can never become unrestricted cash before wagering/conversion rules permit it (re-walked, LF-1)

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
   common case). §16.4 Step 1 finds the credit leg `player_locked_bonus`,
   wallet `w`, `locked_amount = X` — this is now actually reached, because
   Step 1 reads the credit leg, not the debit leg the original design
   read. §16.5's "Grant non-terminal" row applies: the live `FOR UPDATE`
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
   still resolves the same credit leg = `player_locked_bonus`, wallet `w`,
   `locked_amount = X` (unchanged from branch 1 — the *origin* resolution
   never depends on Grant status, only the *destination* decision does —
   and, unlike the original design, this resolution path is actually
   reachable now that it keys off the credit leg rather than a debit leg
   the destination map mis-mapped). §16.5's "Grant terminal" row applies:
   the credit is **not** posted to either `player_bonus` or `player_cash`
   by `postWin` itself — it is handed to §16.9's seam, which today (no
   G-2 answer exists, `bonusengine.ResolveTerminalGrantCredit` does not
   exist) **fails closed**: the whole win-posting transaction is rejected
   with `ErrTerminalGrantCreditUnresolved`, a distinct, loud, alerting
   error (mirroring `ErrBetNotFound`'s "integrity alert, not a routine
   failure" framing, §7), and — per §16.5a's exclusion, found during this
   re-walk — that rejection is itself recorded so the settlement-timeout
   sweep (§16.5a(a)) can never later misclassify this genuine win as a
   silent loss. **Nothing posts. No money moves.** `G` remains in
   `pending_settlement` (§16.10.2) with `L(G) = X > 0` — a real, disclosed
   residual: the lock does **not** release in this sub-branch, by design,
   because releasing it would require choosing a destination, which is
   exactly G-2's undecided question. Once a human selects and
   `bonus-engine` builds one of the three actions, the retried transaction
   posts the win **and** the lock release together, per whichever
   disposition applies: `ACTION_REFORFEIT` releases into `player_bonus`
   then immediately re-forfeits (INV-TG step 1's mechanism, re-applied,
   `L(G) → 0`); `ACTION_ROUTE_TO_CASH` releases directly to `player_cash`
   per the seam's override (`L(G) → 0`, and this is the **only** route by
   which a bonus-origin stake ever reaches `player_cash` directly — gated
   entirely behind a human decision, never `postWin`'s default);
   `ACTION_HOLD_FOR_REVIEW` releases the lock into a suspense/hold
   destination `bonus-engine`'s own hold mechanism must name (§16.5a's
   newly-found cross-dependency) — `L(G) → 0` there too, the lock is never
   left dangling once a disposition exists. **No withdrawable cash is
   ever created by this branch until a human selects and `bonus-engine`
   implements one of these three actions. Holds.**

In neither branch does `player_cash` receive value that traces back to a
bonus-origin stake without first passing through either (a) the existing,
wagering-gated `bonus_conversion` posting, or (b) a not-yet-selected,
not-yet-built G-2 action the human has not authorized. The specific
exploit `architect` flagged — `postWin`'s hardcode routing a bonus-funded
win straight to `player_cash` — is closed by §16.4's fixed resolution
alone, **unconditionally, independent of G-2's eventual answer**: even if
G-2 is one day answered `ACTION_ROUTE_TO_CASH`, that routing only ever
fires through §16.9's named seam, deliberately, for the narrow
terminal-Grant case — never as `postWin`'s default behavior for an
ordinary, non-terminal settlement.

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
  (§16.10.3).

`postRollback` itself never reaches G-2 for a rollback **of a win** that
already credited `player_bonus` before the Grant went terminal — that
credit already passed through G-2 (or was never bonus-funded), and the
rollback's own concern is purely mechanical entry inversion, subject only
to the ordinary balance-sufficiency check (§16.5's rollback row).

### 16.9 The G-2 injection boundary — preserving human ownership

**Named seam, not built**: `bonusengine.ResolveTerminalGrantCredit(ctx,
tx, grantID, correlationID, creditKind, amount) (disposition, err)` —
called from exactly the one site in §16.8, inside the same database
transaction as the settlement posting, under the same `(tenant_id,
grant_id)` advisory lock doc10 §9 already specifies. This mirrors doc10
§T.7's own framing verbatim ("a new call-back into Bonus Engine's
Grant-status read that does not exist today... regardless of which action
is ultimately chosen") — this section names the exact function shape so
it is not discovered mid-implementation, per that framing's own stated
intent.

**Today, before G-2 is answered, and before `internal/bonus` exists at
all**: this function does not exist. `postWin` cannot call it. The
correct, fail-closed behavior for `internal/casino` in this state is:
detect the terminal-Grant condition (§16.8's live status read, which
*can* be built today — it needs only a Grant status table to query, not
the resolution function) and **reject the whole settlement transaction
with a distinct sentinel error** (e.g. `ErrTerminalGrantCreditUnresolved`),
surfaced as an integrity/ops alert exactly like `ErrBetNotFound` — never
silently posted to either account, never retried automatically, never
guessed. This is the "fails closed / queues for review rather than
guessing" behavior this gate requires (§A.8), and it requires **no
G-2 answer to build** — only the live Grant-status read, itself
independent of which action is eventually selected.

**Once G-2 is answered**, exactly one of the three bodies from doc10
§T.7 is dropped into `ResolveTerminalGrantCredit`'s implementation:
ACTION_REFORFEIT (post normally, then a compensating `bonus_forfeiture`,
both owned by the settlement-posting layer, no new cross-domain call
needed beyond the seam itself), ACTION_ROUTE_TO_CASH (the seam returns a
destination override, which `postWin` substitutes for `player_bonus`),
or ACTION_HOLD_FOR_REVIEW (the seam returns a "hold" disposition; the
actual holding mechanism is explicitly out of scope here, per doc10
§T.7's own disclaimer that it is "a `ledger-finance` design question this
contract does not resolve"). **No code in this section selects among
these three or implements any of them.**

**One requirement on all three, found while designing §16.5a's lock-
release step, not previously stated:** whichever disposition
`ResolveTerminalGrantCredit` returns must also specify where the
*released lock amount* (`X`, distinct from the win payout `W`) lands —
`ACTION_REFORFEIT`/`ACTION_ROUTE_TO_CASH` already imply an answer
(`promo_liability` via re-forfeiture, or `player_cash` via the override,
respectively), but `ACTION_HOLD_FOR_REVIEW`'s disposition must additionally
name a suspense/hold account for `X` — this document does not name one,
since it is part of `bonus-engine`'s own hold-mechanism design (§16.13),
but the lock must not be left dangling regardless of which action is
eventually selected.

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

| Scenario | Expected behavior | Why safe |
|---|---|---|
| Win after Grant state change (non-terminal → terminal, no lock outstanding) | Ordinary termination already completed (INV-TG step 2's `L(G)=0` branch) before the win arrives; the win then finds no matching, non-reversed bet under this Grant's lock — but the bet's own ledger row still exists, so §16.4 still resolves an origin. If `G` is already fully terminal with `L(G)` having been zero at trigger time, no `pending_settlement` interval ever existed for this stake, meaning the win could only be a genuinely late artifact of a *different*, still-locked amount — in which case `pending_settlement` (still active) is found and §16.9 fires | INV-TG's `L(G)` check is re-evaluated live, not cached from trigger time |
| Win after wagering completion (`completed`/`converted`) | Not a terminal status (§1.2) — G-2 does not apply; ordinary settlement path, credit posts to `player_bonus` normally, subject to whatever `bonus_conversion` has already released | `completed`/`converted` are excluded from G-2's trigger set by definition (doc10 §T.13) |
| Rollback after win | `postRollback`'s generic inversion (§16.1) debits `player_bonus` for the win amount; if the Grant went terminal and swept that balance into `promo_liability` in the interim, the debit finds insufficient balance. **What happens then is `ledger-finance`'s decision, not casino's (LF-10) — this document does not pre-select "fail closed," "permit negative as a clawback," or "route to a receivable."** Whichever `ledger-finance` decides, it must generalize `ledger.Post`'s existing sufficiency mechanism to `player_bonus` (not yet built, HR-9 currently blocks all `player_bonus` posting regardless), flagged in §16.13 | Never posts a negative/impossible balance *without an explicit, ledger-finance-owned decision permitting one*; the mechanism generalizes the existing `SELECT ... FOR UPDATE` pattern (already used for `player_cash`, `orchestrator.go` line 762) — the treatment of a failed check is the open item, not the check's existence |
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
6. **New (LF-10 fix wave).** §16.5's and §16.11's rollback-insufficient-
   balance rows no longer assert "fail closed" as this document's answer.
   The actual treatment (permit negative balance as a recorded clawback /
   route to a receivable / reject-and-alert) is explicitly routed to
   `ledger-finance` as an open decision, not pre-selected here.

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
- **§16.9's `ACTION_HOLD_FOR_REVIEW` holding mechanism** remains, as
  doc10 §T.7 itself already discloses, an unresolved `ledger-finance`
  design question — a held G-2 credit's interaction with the hourly
  reconciliation sweep (§16.6's Reconciliation row) is unspecified. **New
  (LF-4 fix wave):** that mechanism must additionally name a destination
  for the *released lock amount* `X` (distinct from the held payout `W`)
  — found while designing §16.5a, flagged in both §16.5a and §16.9,
  not resolved here.
- **The rollback-of-a-credited-win-against-a-later-forfeited-balance
  insufficient-balance case** (§16.11) depends on `ledger-finance`'s
  balance-sufficiency check generalizing to `player_bonus`, not yet built
  (HR-9 currently blocks all `player_bonus` posting regardless). **Revised
  (LF-10 fix wave):** this document no longer asserts "fail closed" as
  its own answer for what a failed check should do — that treatment
  (permit negative balance as a recorded clawback / route to a
  receivable / reject-and-alert) is `ledger-finance`'s decision. Only the
  need for the check to exist and to cover `player_bonus` is asserted
  here.
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

## Cross-references

- ADR: `docs/decisions/0025-casino-provider-abstraction-and-game-session-model.md`
- Financial flows: `docs/architecture/financial-transaction-flows.md` §5-7
- Ledger model: `docs/architecture/ledger-accounting-model.md` §6.3.3.1, §6.4 (cases A/B/F/G/H/I), §6.5 (migration 0048), §6.6 (Model C)
- Bonus accounting: `docs/decisions/0032-bonus-accounting.md` §3, §5
- Terminal-Grant Technical Contract: `docs/architecture/10-bonus-engine-architecture.md` §T.1-T.13
- Human Decision Register: `docs/decisions/0039-human-decision-register-stage-4h-b0-r7.md`, Decision 2
- Task registry: `docs/governance/task-registry.md`, Stage 4H-B1 Wave 1 reconciliation finding 1, Wave 1.5
- Payment orchestration (the pattern this mirrors): `docs/architecture/payment-orchestration.md`
- Provider agnosticism precedent: `docs/decisions/0022-payment-provider-agnosticism-and-capability-model.md`
- Provider abstraction pattern: `docs/decisions/0004-provider-abstraction-pattern.md`
