# ADR 0025 — Casino Provider Abstraction and Game Session Model

Status: Accepted. Issued for Stage 4A (Casino Integration Foundation), an
explicit, tightly-scoped "provider-agnostic foundation" stage: no real
casino provider is contracted or integrated, no production credentials
exist, and sportsbook/bonus-engine/KYC-AML/responsible-gaming remain out
of scope. Mirrors ADR 0004 (every integration is a subsystem), ADR 0022
(payment provider agnosticism and capability model), and
`payment-orchestration.md` — the same pattern applied to casino, not a
new one invented. Owner: `casino`, with `ledger-finance` sign-off on the
bet/win/rollback posting logic and `security` on the game-launch/session
and callback-authentication boundary.

## Context

`docs/architecture/08-casino-integration-architecture.md` (Stage 0) and
`docs/architecture/financial-transaction-flows.md` §5-7 (Stage 3A,
`BLUEPRINT`) already fix the *what*: an internal `launch()`/`catalogue()`/
`bet()`/`win()`/`rollback()`/`balance()` interface, single-use opaque
launch tokens never reusing the player's own session token, and the exact
ledger accounts/transaction types/idempotency keys for bet, win, and
rollback. Nothing in those documents was implemented. This ADR fixes the
*how*: the Go interface shape, the database schema, the routing/capability
model, and the launch-session mechanism — so Stage 4A can implement
against a frozen design, the same role `payment-orchestration.md` played
for Stage 3B.

## Decisions

### 1. `CasinoProvider` interface — provider-neutral, no leaked provider concepts

```go
type CasinoProvider interface {
    Catalogue(ctx) ([]CatalogueEntry, error)
    Launch(ctx, LaunchRequest) (LaunchResult, error)
    Balance(ctx, BalanceRequest) (BalanceResult, error)
    Bet(ctx, BetRequest) (BetResult, error)
    Win(ctx, WinRequest) (WinResult, error)
    Rollback(ctx, RollbackRequest) (RollbackResult, error)
    HandleCallback(ctx, rawPayload []byte) (CallbackEvent, error)
    Capabilities() AdapterCapability
    HealthStatus(ctx) (ProviderHealth, error)
}
```

Same shape discipline as `PaymentProvider` (`payment-orchestration.md`
§2, `docs/decisions/0022` §4.1): every request/response struct is
explicitly enumerated and typed, with **no free-form passthrough field**
(no `RawPayload`/`Extra`/`Metadata` map) and **no provider SDK type**
anywhere in the signature. `Bet`/`Win`/`Rollback` exist on the interface
as the *synchronous call shape* a real aggregator's wallet-callback API
uses (the provider calls the platform, not the reverse — see §5), but the
canonical entry point this stage actually wires up is `HandleCallback`,
exactly mirroring `PaymentProvider.HandleCallback`: an inbound HTTP
request from the provider is parsed and signature-verified inside the
named adapter, producing one canonical `CallbackEvent`, before any ledger
effect is even considered. `Bet`/`Win`/`Rollback` are what an adapter's
own `HandleCallback` implementation calls internally to talk to *that*
provider's specific wallet-API shape if it is the platform being called
synchronously per-operation (the common real-world shape) — both shapes
resolve to the same canonical `CallbackEvent` the orchestrator consumes,
so the orchestrator itself never branches on which transport shape a
given provider uses.

`MockCasinoProvider` (§7) implements this interface with no shortcut path
— it is the first adapter to pass the conformance suite (§8), not a
stand-in that skips it, mirroring `docs/decisions/0022` §6's identical
rule for the mock PSP.

### 2. Game catalogue — platform identity distinct from provider identity

Two tables, mirroring the adapter-declared/operator-configured split
`docs/decisions/0022` §2 established for payment capabilities:

```
casino_games                        -- platform-wide, no tenant_id, no RLS (like `assets`)
  id                    UUID (platform's own canonical game identity)
  provider_id           TEXT
  provider_game_id      TEXT         -- the provider's OWN identifier; never promoted to platform identity
  name                  TEXT
  game_type             TEXT         -- 'slot' | 'live_dealer' | 'table' | ... open-ended, never a fixed enum
  rtp_variant           TEXT NULL    -- same title can ship at different RTP - contractual, not technical
  volatility            TEXT NULL
  feature_flags         TEXT[]
  supported_assets      TEXT[]       -- asset codes this game accepts stakes in
  mobile_supported      BOOLEAN
  demo_supported        BOOLEAN
  jurisdiction_blocklist TEXT[]      -- jurisdiction codes this title may NEVER be launched into (licence, not a bug)
  status                TEXT         -- 'active' | 'disabled' — a PLATFORM-level kill switch (e.g. licence pulled), distinct from per-tenant availability
  UNIQUE (provider_id, provider_game_id)

casino_game_availability            -- tenant-owned, RLS-protected
  tenant_id             UUID NOT NULL
  brand_id              UUID NULL    -- NULL = every brand under the tenant, same nullable-brand-fallback rule as docs/decisions/0022 §3
  game_id               UUID REFERENCES casino_games(id)
  enabled               BOOLEAN      -- operator's own on/off switch for this game at this tenant/brand
  UNIQUE (tenant_id, brand_id, game_id) WHERE brand_id IS NOT NULL
  UNIQUE (tenant_id, game_id) WHERE brand_id IS NULL
```

`casino_games` is platform-wide (like `assets`, migration 0003) because
game *content* — which studio, which RTP variant, which jurisdictions it
may never serve — is a fact about the provider relationship, shared
across every tenant that can reach that provider, not tenant-owned
configuration. `casino_game_availability` is the tenant-owned layer: a
tenant/brand opts a subset of the platform catalogue in, never a superset
— the same narrowing-only rule `docs/decisions/0022` §2.1 states for
payment capabilities, applied here as "a tenant may enable a game the
platform has onboarded; it may never register a new game or widen a
platform-level jurisdiction block."

A launch is only permitted when: the game's own `status = 'active'`, the
tenant/brand's `casino_game_availability.enabled = true`, the resolved
provider's own capability row (§4) is active for this tenant/brand/asset,
and the tenant's jurisdiction is not in the game's
`jurisdiction_blocklist`. Missing any one of these fails closed with a
specific, distinguishable error (§9) — never a generic 404, so an operator
can tell "game doesn't exist" from "game exists but isn't enabled here"
from "provider unavailable" from "blocked in this jurisdiction" (directive
items C/O/P/Q's distinct adversarial cases depend on this).

**Not built this stage** (CLAUDE.md's scope-expansion test): a catalogue
sync job (nightly provider pull), a jurisdiction-*resolution* engine (this
stage checks a game's static blocklist against the tenant's own
`tenant_jurisdiction_configs` entries, the same TODO(jurisdiction) scope
boundary `payment-orchestration.md` §4 already carries forward
unimplemented — jurisdiction is asserted per tenant config, not derived
per-player), free-round/bonus normalization (`08-casino-integration-
architecture.md`'s own "free rounds — the messiest part" section, owned
by `bonus-engine`, explicitly out of scope), and a back-office catalogue
management UI (a minimal admin API only, mirroring Stage 3D's "no full
Back Office UI" precedent).

### 3. Game launch — a distinct, opaque, single-use session credential

**Binding**: the player's own JWT/session token is NEVER passed to, or
derivable by, an external casino provider. A game launch mints its own
credential, entirely independent of `internal/auth`'s player/staff token
machinery:

```
casino_launch_sessions              -- tenant-owned, RLS-protected
  id                UUID
  tenant_id         UUID NOT NULL
  brand_id          UUID NOT NULL
  player_account_id UUID NOT NULL
  wallet_id         UUID NOT NULL
  game_id           UUID NOT NULL REFERENCES casino_games(id)
  provider_id       TEXT NOT NULL
  provider_game_id  TEXT NOT NULL   -- denormalized from casino_games at launch time, so a later catalogue edit never changes what an in-flight session resolves to
  asset_code        TEXT NOT NULL
  mode              TEXT NOT NULL   -- 'real' | 'demo'
  token_hash        TEXT NOT NULL UNIQUE  -- SHA-256 of the opaque token; the raw token is NEVER stored, mirroring auth.Session's refresh-token pattern
  status            TEXT NOT NULL   -- 'active' | 'consumed' | 'expired' | 'revoked'
  expires_at        TIMESTAMPTZ NOT NULL
  consumed_at       TIMESTAMPTZ NULL
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
```

The launch token is an **opaque, cryptographically random string** (same
`crypto/rand` + `base64.RawURLEncoding` generation `auth.generateRefreshToken`
already uses), hashed with SHA-256 before storage exactly like a refresh
token — **never a JWT**. Two independent reasons this is the right choice,
not merely "consistent with an existing pattern":

1. **Trust-domain separation.** The player's JWT is signed by, and
   verified against, the platform's own key registry
   (`internal/auth/keys.go`) shared across every internal authenticated
   surface (player, staff, service identities — ADR 0014). Handing an
   external, semi-trusted casino provider a token verifiable with that
   same key registry would let a provider-side bug or compromise forge
   claims verifiable as genuine platform tokens elsewhere. An opaque,
   single-purpose, database-backed token has no such blast radius: it
   verifies against exactly one table, for exactly one operation class
   (resolving *this* launch's own tenant/player/wallet/game/asset
   context), and nothing else.
2. **Scope minimization.** A JWT's claims (subject, role, tenant) are
   *generic* platform-identity facts. A launch credential's job is
   narrower and different in kind — Blueprint's own "single-use, opaque,
   bound to `(player, provider, game, currency, mode)`" — and `08-casino-
   integration-architecture.md`'s pre-existing design already specified
   exactly this shape; this ADR does not invent it, it fixes the concrete
   schema and generation mechanism.

**Single-use, per §"Game launch"'s explicit instruction**: `status`
transitions `active` → `consumed` on first successful resolution (the
provider's own `Launch`/game-bootstrap call against the platform, which
this stage models via `ResolveLaunchToken` — see §9), never re-usable
after. A provider that needs a longer-lived session for its own internal
game-client reconnect logic is expected to maintain that session itself,
downstream of this one-time resolution — the platform's own credential is
deliberately narrower than "how long the player's game client stays
open."

**What crosses to the provider**: `provider_game_id`, `mode`,
`asset_code`, and the opaque launch token itself (as a URL parameter or
bearer credential on the provider's own launch URL, adapter-specific).
Player PII (email, KYC tier, real name) is **never** included — minimizing
exposed player data per the directive's explicit instruction. If a
provider's own launch contract genuinely requires a player identifier for
its game-state persistence, the platform's own `player_account_id` (an
opaque UUID, meaningless outside the platform) is the only identifier
ever shared, never email/name/PII.

### 4. Casino provider capability model

```
casino_provider_capabilities         -- tenant-owned, RLS-protected (mirrors provider_capabilities, docs/decisions/0022 §2)
  id, tenant_id, brand_id (nullable)
  provider_id
  supports_catalogue, supports_launch, supports_balance,
    supports_bet, supports_win, supports_rollback   BOOLEAN  -- which of the six capabilities this adapter actually implements for this tenant
  supported_assets       TEXT[]
  supported_game_types   TEXT[]
  callback_capabilities  TEXT        -- 'webhook' | 'polling_only' | 'both' — same enum as docs/decisions/0022 §2, reused verbatim
  priority               INT
  status                 TEXT        -- 'active' | 'disabled'
```

Same two-layer split as payments (§2.1/§2.2 of `docs/decisions/0022`,
adopted verbatim, not re-litigated): `provider_id` and which of the six
operations an adapter implements are **adapter-declared facts**, read
from `CasinoProvider.Capabilities()`, never tenant-editable; `supported_
assets`/`supported_game_types` may only **narrow** what the adapter
declares; `priority`/`status` are pure tenant configuration. No amount-
limits child table exists here (unlike payments) — casino stake limits
are a game-level/jurisdiction-level responsible-gaming control, not a
provider-capability fact, and RG limit enforcement is explicitly out of
Stage 4A's scope (`identity-compliance`, a later stage).

### 5. Provider callback authentication — adapter-specific, never an unauthenticated endpoint

Identical rule to `payment-orchestration.md` §10, restated for casino
because the directive explicitly re-demands it: **a provider callback is
never processed by an unauthenticated handler.** The platform knows which
adapter is responsible for verifying a given callback from the URL path
(`POST /v1/webhooks/casino/{tenantSlug}/{providerID}/{operation}` — the
same per-tenant-path-selects-the-adapter resolution
`payment-orchestration.md` §3 already uses for payments, carried forward
rather than inventing a second convention), and that adapter's own
`HandleCallback` verifies the payload's authenticity (HMAC/signature,
vendor-specific) **before** any payload field is used to resolve tenant,
player, wallet, or game. An unverified payload never reaches routing or
the ledger. The tenant comes from the URL's tenant slug (resolved,
authenticated-tenant lookup — same `identity.GetTenantBySlug` +
active-status check `newPaymentWebhookHandler` already performs, reused
verbatim), never asserted by the payload.

### 6. Bet / Win / Rollback — no second balance system, existing ledger only

`financial-transaction-flows.md` §5-7 (already `BLUEPRINT`-status, not
modified by this ADR) is the binding specification:

- **Bet**: debit `player_cash` (bonus-split deferred — see the scope note
  below), credit `house_gaming`. Idempotency key `(provider_id,
  provider_tx_id)` = the provider's own bet/round reference. Insufficient
  funds is checked and rejected **before** posting, inside the same
  transaction (invariant #15's read-then-write-atomically pattern,
  identical to `RequestWithdrawal`'s balance check) — a declined bet
  produces **no** `LedgerTransaction` row.
- **Win**: debit `house_gaming`, credit `player_cash`. Idempotency key is
  the win's own `(provider_id, provider_tx_id)`, distinct from the
  triggering bet's. A win callback naming a round with no matching prior
  bet transaction is rejected as an **integrity alert** (a provider
  protocol violation, not a routine failure) — logged and audited at
  elevated severity, never silently 404'd the way an ordinary not-found
  is.
- **Rollback**: a new `LedgerTransaction` with `reverses_transaction_id`
  pointing at the original bet (and, separately, at the win, if one was
  also posted for the same round — two independent reversal postings, one
  per original, since `reverses_transaction_id` is a single FK per
  `ledger-accounting-model.md` §1.2) — exact inverse entries of whichever
  flow actually posted. Idempotency key is the rollback's own reference.
  A rollback naming a `provider_tx_id` the ledger never posted a bet for
  writes a **tombstone** (`ledger.TxTombstone`, the identical mechanism
  `postDepositReversalTombstone` already implements for payments), so a
  late-arriving original bet callback for that same reference is rejected
  rather than posted after the fact.

**Explicit scope boundary, not a silent gap**: `player_bonus`-funded
stakes (bonus-wagering split) and jackpot contribution splits are
`OPEN DECISION`s this ADR does not resolve (`ledger-accounting-model.md`
§2's own open items), because the Bonus Engine that would drive them is
explicitly out of Stage 4A's scope. Stage 4A's `Bet`/`Win` posting logic
handles the 100%-`player_cash`-funded case only; a player with no active
bonus is the only case exercised, and the code path is written so that
adding the bonus split later is a change *inside* the bet/win posting
function, never a redesign of the interface or the ledger schema it
targets (same "adding a provider never touches the ledger" checklist
philosophy `docs/decisions/0022` §5 states for payments, applied here to
"adding bonus-awareness never touches the interface").

`ledger_transactions.transaction_type`'s CHECK constraint (migration
0021) gains three new values this migration: `casino_bet`, `casino_win`,
`casino_rollback` — an additive migration, exactly as migration 0021's
own comment anticipated ("casino/sportsbook/bonus/crypto types are
explicitly blocked this stage and are added by an additive migration when
their owning stage implements them").

### 7. `MockCasinoProvider`

Implements `CasinoProvider` with deterministic, amount/reference-keyed
synthetic behavior, mirroring `MockProvider`'s own documented "magic
value" convention (`internal/payments/mock.go`'s doc comment) rather than
inventing a different testing idiom: a fixed synthetic catalogue, a
`Launch` call that always succeeds for an enabled game, and `Bet`/`Win`/
`Rollback` behavior selectable by a magic stake amount (insufficient-funds
decline, provider failure, ambiguous/timeout outcome) so the conformance
suite (§8) can drive every required scenario without a real provider
sandbox.

### 8. Provider conformance suite

One parameterized test suite, run identically against `MockCasinoProvider`
now and any future real adapter later — the same binding rule
`docs/decisions/0022` §6 states for payments, applied verbatim to casino:
the mock is the first adapter to pass the suite, not exempt from it.
Minimum coverage: catalogue listing, launch (including single-use
enforcement and session-isolation), balance query, bet/win/rollback
posting and their idempotency, duplicate-callback handling, provider-
authentication failure, provider failure/timeout, and capability
declaration shape — directive items A-T are the authoritative list; see
`docs/progress.md`'s Stage 4A section for the exact test-to-item mapping.

## Consequences

- No change to `Wallet`, `LedgerAccount`, `LedgerTransaction`,
  `LedgerEntry`, the balance projection mechanism, or any Mandatory
  Financial Invariant beyond the additive `transaction_type` CHECK values
  in §6 — casino integration is new *rows* (new transaction types posted
  through the existing `ledger.Post` API), never new financial-truth
  machinery.
- `internal/casino` has zero dependency on `internal/auth`'s JWT/session
  internals — the launch-session mechanism is its own, narrower,
  database-backed credential, per §3.
- Sportsbook is **not** assumed to share this interface. A future
  sportsbook integration (two providers, per business requirement) gets
  its own canonical interface and its own adapters
  (`docs/architecture/09-sportsbook-architecture.md`), because
  sportsbook's settlement/liability model (`player_locked`, partial
  cash-out, void-after-settlement — Flows 8-11) is materially different
  from casino's near-instant bet/win/rollback — collapsing the two into
  one interface would either force sportsbook's richer state machine into
  casino's shape or silently drop casino's simplicity. This ADR
  establishes the *pattern* (canonical interface, adapter registry,
  capability model) both integrations follow, not one shared interface
  both must implement.
- Adding a second real casino provider later requires only: an adapter
  implementing `CasinoProvider`, a `casino_provider_capabilities` row, a
  `casino_games` catalogue sync for that provider's titles, and that
  adapter's own conformance-suite run — never a change to
  `internal/casino`'s orchestration/routing code or the ledger.

## Owner

`casino`, with `ledger-finance` sign-off on §6 and `security` sign-off on
§3 (launch-session credential) and §5 (callback authentication).
