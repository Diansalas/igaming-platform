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
specific, distinguishable error — never a generic 404, so an operator
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
transitions `active` → `consumed` on first successful resolution
(`ResolveLaunchToken`), never re-usable after — enforced by an atomic
conditional `UPDATE ... WHERE status = 'active'`, not a read-then-write, so
two concurrent resolution attempts for the same token can never both
succeed (proven under real concurrency by
`TestResolveLaunchToken_ConcurrentResolutionOnlyOneSucceeds`). The launch
token's single use governs *bootstrapping the game client only* — it is
not the credential subsequent bet/win/rollback callbacks throughout the
round authenticate against (§6 covers what those calls actually carry).
Migration 0036 additionally freezes every identity/token/expiry column and
the whole row once terminal (`consumed`/`expired`/`revoked`), closing a
specialist-review finding that an un-triggered table let a consumed
session be resurrected to `active` with an unbounded new expiry.

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
(`POST /v1/webhooks/casino/{tenantSlug}/{providerID}` — the same per-
tenant-path-selects-the-adapter resolution `payment-orchestration.md` §3
already uses for payments, carried forward rather than inventing a second
convention; the *operation* — bet/win/rollback — is carried inside the
signed payload's own `event_type` field rather than a further URL segment,
since the wire shape is entirely internal to the adapter and a URL-
embedded operation type would be redundant with what `HandleCallback`
already parses), and that adapter's own `HandleCallback` verifies the
payload's authenticity (HMAC/signature, vendor-specific) **before** any
payload field is used to resolve tenant, player, wallet, or game. An
unverified payload never reaches routing or the ledger. The tenant comes
from the URL's tenant slug (resolved, authenticated-tenant lookup — same
`identity.GetTenantBySlug` + active-status check `newPaymentWebhookHandler`
already performs, reused verbatim), never asserted by the payload.

### Amendment (Stage 10.2)

> **Amendment 2026-09-26 (Stage 10.2, ADR 0091, CAS-WH-TENANT-1). Recorded by
> `architect` from design
> `docs/plans/stage-10.2-planning/01-webhook-trust-design.md` §C and
> rulings §J (J5, J8, J14), plus review `07-review-architect-db.md` §5 C.
> The `casino` review (`06-review-casino.md`) approved without change.**
>
> **Tenant (§5).** "The tenant comes from the URL's tenant slug" is
> **superseded** by ADR 0022 §3 as amended (points 1–9). The slug selects
> one candidate tenant. The tenant is established only by a
> per-(tenant, provider) credential bound into the signing input (MOCK
> scheme: prefix `igaming.casino.webhook.v1`, headers
> `X-Casino-Signature`/`X-Casino-Key-Id`). Casino has strict I1 (point 9):
> before verification, no tenant-scoped statement runs. Every
> pre-verification failure is the uniform 401. That replaces the earlier
> 404 for an unknown or suspended tenant and the earlier pre-verification
> 400s.
>
> **Interface (§1).**
> - `HandleCallback` takes
>   `(ctx, in webhookauth.Inbound, cred webhookauth.Credential)`.
> - `NewOrchestrator` takes a `webhookauth.Resolver`, matching payments.
>   A nil resolver fails closed.
> - `ReceiveCallback(ctx, tx, tenantID, providerID, in)` first overwrites
>   `in.TenantID`/`in.ProviderID` from its parameters.
> - The adapter MACs the raw bytes, then parses. A verified-but-malformed
>   body is the distinct `casino.ErrCallbackMalformedBody`, mapped to 400.
> - `casino.ErrCallbackSignatureInvalid` becomes an alias of
>   `webhookauth.ErrSignatureInvalid`.
>
> **Mock (§7).** The mock's per-process `signingSecret` and its
> NUL-joined field MAC are replaced. The mock now uses a key derived per
> tenant from a per-process random master (label
> `igaming/casino-mock-webhook/v1`, key id `mock-v1`), computed over the
> raw bytes, which removes field shifting. The body carries no signature
> field. A legacy `signature` field is rejected as `ErrSignatureInvalid`.
>
> **Capability check.** The tenant capability check (`LoadCapability`,
> `status == active`; ADR 0025 review P1) runs **only after
> verification**. A verified caller whose capability is disabled gets 503.
> An unverified caller never observes capability state. The casino
> capability therefore stays a money-path kill switch for callbacks. This
> knowingly differs from ADR 0022 §3's "`ProviderCapability.status`
> governs routing only": a disabled capability 503s a verified rollback,
> and that can strand a debited stake. The behaviour is pre-existing and
> not changed in 10.2. It is registered as follow-up
> **CAS-CAP-ROLLBACK-1** (`casino` + `ledger-finance`,
> `docs/governance/task-registry.md`).
>
> **Money path unchanged.** Idempotency is still the ledger
> `(tenant, provider_id, provider_tx_id)` key, now keyed on the route
> `provider_id` the credential verified (point 6). F-7 still gives 409,
> `ErrAlreadyRolledBack` still gives 409, and per-tenant tombstones
> remain. A cross-tenant callback, including a rollback, is rejected with
> 401 before any tenant-scoped read. It writes no tombstone, ledger,
> projection, round or audit row in either tenant.
>
> **Resolver gating (ADR 0085).** The casino webhook route stays
> registered everywhere, because it is the real provider-facing route.
> `cmd/platform-api/wiring.go` `mockProviderWiring` wires the mock
> resolver only when `TestSupportRoutesEnabled()` is true. Otherwise the
> resolver is nil, every callback returns 401 (`no_resolver`), and no
> casino money moves. The mock adapter itself stays registered for
> catalogue and launch (**MOCK-ADAPTER-PROD-1**, pre-launch checklist).
>
> **Play simulation (ADR 0085/0048).** Play simulation signs in-process
> for `tc.TenantID`, the tenant from the JWT, and calls `ReceiveCallback`
> with that same tenant. An `AuthError` on this route is 503 ("simulated
> play is misconfigured"). The signed bytes are never returned. The same
> gate controls both the routes and the resolver.
>
> **Status.** CAS-WH-TENANT-1 closes for the **MOCK only**. A real
> aggregator resolver is `NOT IMPLEMENTED`. The casino tenant-binding
> conformance case is mandatory for the first real adapter.
> Implementation status is tracked in `docs/governance/task-registry.md`
> (Stage 10.2).

### Amendment (Stage 10.3, ADR 0092) — casino capability contract

> **Amendment 2026-09-26 (Stage 10.3, ADR 0092; CAS-CAP-ROLLBACK-1 and
> CAS-MULTIBET-WIN-1, wave W1c; CAS-RECON-1 pointer, wave W2b). Recorded
> by `architect`.**
>
> - **Sources.** The contract is `ledger-finance`'s (paper
>   `docs/plans/stage-10.3-planning/02-casino-financial-analysis.md`
>   §1.3–§1.10, §3 G-1). Security §5 ruled on it, and rulings R9 and C14
>   adopt it.
> - **Concurrence required.** `casino` must concur on the domain parts:
>   the E1/E3 response shape and the launch-coherence rule.
>
> **Supersedes:**
> - the Stage 10.2 amendment's "the casino capability therefore stays a
>   money-path kill switch for callbacks";
> - the Stage 4A review fix that checks the tenant-wide capability "for
>   every event type" in `ReceiveCallback`.
>
> The Stage 10.2 divergence from ADR 0022 §3's "`ProviderCapability.status`
> governs routing only" is **closed**.
>
> **Current behaviour being replaced** (verified at `c90e591`):
> - `ReceiveCallback` loads the **tenant-wide** capability after
>   verification (`LoadCapability(…, uuid.Nil, …)`, `orchestrator.go:662`).
> - It returns 503 for **every** event type when that capability is
>   missing or disabled, and for each event type whose `supports_*` flag
>   is off (`:670-686`).
> - As a result, a verified win or rollback can be refused, and the
>   rollback of an unseen original writes no tombstone.
>
> **Contract (binding).**
> 1. **Capability gates new bets only.** Settlement of existing exposure
>    is never blocked by capability status, a flag, or a missing row.
>    This covers win, rollback, replay and tombstone.
>    - The pre-dispatch capability check and the win/rollback flag checks
>      in `ReceiveCallback` are removed.
>    - Win and rollback no longer read the capability at all.
> 2. **Where the gate sits: `postBet`.** The gate runs after these steps:
>    - the L0.1 delivery lock;
>    - the idempotency short-circuit, so a replayed bet returns its
>      original result;
>    - the tombstone check (item 4);
>    - session resolution.
>
>    The gate resolves `LoadCapability(ctx, tx, tenantID,
>    session.BrandID, providerID)` using **the session's `BrandID`**, the
>    same way `LaunchGame` resolves it (`orchestrator.go:284`). This
>    closes the tenant-wide/brand-row mismatch.
>
>    A bet passes only if all of these hold: `found`, `status = active`,
>    `supports_bet`, and the session asset is in `supported_assets`.
>
>    The gate is a plain, lock-free read that runs before L0.2, so a
>    rejected bet never takes the player lock.
>
>    A rejected new bet posts nothing and keeps its existing 503. `casino`
>    may change that to a definitive `declined`.
> 3. **An unseen-original rollback always writes its tombstone**, whatever
>    the capability state.
>    - The tombstone `CorrelationID` becomes deterministic: the round
>      correlation when a `RoundID` is present, otherwise a v5 UUID of the
>      tombstone key. Today it is random (`uuid.New()`,
>      `postRollbackTombstone`).
>    - Tombstones are exempt from the correlation comparison on replay.
> 4. **A late original after its tombstone gets a named rejection**,
>    `ErrOriginalTombstoned`, not the current untyped unique-violation 500.
>    *(As implemented in W1c: a late bet (E3) is a 200 `declined` with
>    `decline_reason = "original_rolled_back"` plus a
>    `casino_bet.rejected_tombstoned` audit row. Only a late win (E10)
>    returns `ErrOriginalTombstoned`, mapped to 409. See ADR 0082 A6.)*
>    - For a bet (E3), the check runs before RG, Risk and round binding, so
>      it has no side effects.
>    - A win whose own `provider_tx_id` is tombstoned (E10) is rejected
>      the same way.
>    - Nothing posts, and the result is deterministic and not retryable.
>    - It is durably recorded in one of two ways:
>      - a `declined` result with `decline_reason = "original_rolled_back"`
>        and a `casino_bet.rejected_tombstoned` audit row in the committing
>        transaction; or
>      - a 409 plus the W2b rejection record.
>
>      `casino` chooses which.
> 5. **Schema invariant.** Migration 0094 adds
>    `CHECK (NOT supports_bet OR (supports_win AND supports_rollback))` on
>    `casino_provider_capabilities`.
>    - Building the constraint *is* the pre-flight. A `count(*)` under
>      FORCE RLS would see zero rows.
>    - The migration refuses rather than editing configuration.
>    - `WriteCapability` rejects a bet capability without win and rollback
>      (`ErrCapabilitySettlementIncomplete`, 400).
>    - A conformance case fails any adapter that declares `SupportsBet`
>      without `SupportsWin` and `SupportsRollback`.
>    - `supports_win` and `supports_rollback` become configuration
>      assertions, no longer runtime gates.
>    - Capability-write audit rows record the before and after values of
>      every `supports_*` flag, `status` and `supported_assets`.
> 6. **Lock order.** `postRollback` takes L0.1 on the **original**
>    reference before its L2 `FOR UPDATE`. A late original and its
>    rollback therefore serialize deterministically. See ADR 0082,
>    Amendment A6 (`ledger-finance`-owned). W1c also takes L0.1 in
>    `postWin` on its own reference (A6's permitted extension, for E10).
> 7. **G-1: multi-bet cash rounds.**
>    - When every un-reversed origin row is `player_cash` on one wallet and
>      one asset, `resolveWinOrigin` resolves to that wallet instead of
>      returning `ErrAmbiguousMultiOriginRound`.
>    - The following remain unchanged: `ErrCorrelationWalletCollision`,
>      `ErrMixedFundingUnsupported`, and every locked or bonus outcome.
>    - These errors map to a 409 integrity alert instead of a 500:
>      `ErrAmbiguousMultiOriginRound`, `ErrCorrelationWalletCollision`,
>      `ErrLockAlreadyReleased`, `ErrMixedFundingUnsupported` and
>      `ErrBonusBetNotLocked`.
>    - A characterization test comes first.
> 8. **Free rounds and jackpots.** A real adapter must not map a free-round
>    or jackpot payout that has no platform bet to a win, until a design
>    exists (ADR 0092 out of scope).
>    - *(Corrected at gate 10.3-W1, code review #7. The text at acceptance
>      said "the conformance rule enforces this". No such conformance case
>      exists at HEAD `bc72fe4`.)*
>    - This is a **documented requirement for the first real casino
>      adapter**. As a conformance case it is `NOT IMPLEMENTED`.
>    - The MOCK has no free-round or jackpot event, so there is nothing to
>      test against today. The case is written and reviewed with the first
>      real adapter, whose callbacks can carry such payouts.
> 9. **Redelivery audit rule and replay logging** *(added at gate
>    10.3-W1 close-out; `ledger-finance` ruling in its "Re-verification
>    after fix round A")*.
>    - **Rule: postings are audited once per fact; E3 rejections are
>      audited once per verified attempt.** A byte-identical redelivery
>      that resolves to `AlreadyPosted` mutates nothing, and the fact it
>      describes already has exactly one audit row, committed in the same
>      transaction as the posting. The one intentional exception is the
>      E3 `casino_bet.rejected_tombstoned` row, which records a rejection
>      decided on that delivery. A divergent redelivery still errors
>      (`ErrIdempotencyPayloadMismatch`/`ErrIdempotencyKeyReused` → 409
>      plus an integrity-alert log), so the gate cannot swallow a
>      conflicting attempt.
>    - **The audit-bloat defect, found and fixed in fix round A
>      (`5f98e23`).** Before the fix, `postWin` (all four branches) and
>      `postRollback`'s generic entry-inversion path wrote a new audit row
>      on every redelivery of an already-posted win or rollback. There was
>      no financial effect. Those writes are now gated on
>      `!postResult.AlreadyPosted`. The `postRollback` gate's killing test
>      is `TestPostRollback_C11_GenericPathAuditGate_RedeliverySequentialThenConcurrent`
>      (`98a7f08`, `ledger-finance` C11; mutation-killed and run as
>      `igaming_runtime`, `evidence/w1c-mutation-kill.txt`,
>      `evidence/w1c-c11-runtime-role.txt`). In `98a7f08` (R2) the
>      `casino_bet.posted` write in `postBet` and the
>      `casino_win.rolled_back` write in `postRollbackHeldWin` are gated
>      the same way. Both were unreachable on replay because of upstream
>      short-circuits; the gate makes the rule structural. The
>      `"already_posted"` metadata key on these rows is now always
>      `false` and is kept for schema stability.
>    - **R1, `casino_callback_replayed` (landed in `98a7f08`).**
>      `casino.ReceiveCallbackResult` gains `Replayed bool` and
>      `EventType`. `Replayed` is set at every replay short-circuit:
>      `postBet`'s E2 idempotency short-circuit, the E9 tombstone replay,
>      the `postWin`/`postRollback` `AlreadyPosted` gate, and
>      `postRollbackHeldWin`'s voided-by-same-reference short-circuit. On
>      a replay the casino webhook handler logs one structured `Info` line,
>      `casino_callback_replayed`, carrying only `request_id`,
>      `tenant_id`, `provider_id` and `event_type`. It deliberately omits
>      `provider_tx_id` and `ledger_transaction_id` (the R1 wording
>      suggested them), matching the handler's rule of never echoing
>      caller-supplied identifiers into logs; `request_id` joins it to the
>      request. Test:
>      `TestCasinoWebhook_ReplayedLogging_AbsentOnFirstDeliveryPresentOnRedelivery`.
>      This is a log line, not a durable record. A durable per-delivery
>      record belongs in W2b's callback/rejection record, never in
>      `audit_log`.
>    - **Not covered by this rule, carried forward:**
>      - E10, the G-1 409 classes, and an E9 rollback that names an
>        already-tombstoned original under a *different* reference (200,
>        with no ledger, audit or log record of its own) are captured only
>        by W2b's rejection record (`ledger-finance` C9 and its extension;
>        CAS-RECON-1).
>      - CAS-WIN-IDEMP-1 (F-9, Medium, pre-existing): `postWin` has no
>        `postBet`-style "already posted → verify match → return original"
>        short-circuit. A win redelivered after a rollback of its bet
>        returns 400 (`ErrBetNotFound`, direct cash) or 409
>        (`ErrLockAlreadyReleased`, locked branches) instead of the
>        original result. It never pays twice. It must be fixed before
>        bonus-funded or locked casino stakes (G-6) ship.
>    - The tombstone correlation fallback (item 3) is tenant-qualified
>      since `5f98e23`: `uuid.NewSHA1(OID, tenantID + ":" +
>      "tombstone:<provider>:<ref>")`. Replay stays safe because
>      tombstones are exempt from the correlation comparison.
>
> **Emergency stop: credential revocation, not a settlement freeze
> (R9/C14).**
> - **Disabling the capability stops new bets.**
> - **Revoking the credential** stops every callback from that key, bets
>   included. It takes effect immediately, per request (ADR 0093 §4). It
>   strands open exposure.
> - Revocation is single-actor and reason-coded. Tenant admins and
>   platform admins can both do it (ADR 0093 §3). It never requires
>   four-eyes.
> - **No four-eyes "settlement freeze" is built.**
> - **No real casino resolver or adapter is wired in any environment**
>   until W2a revocation is implemented and its immediacy is tested,
>   including revocation racing an in-flight request. W1c lands before W2a
>   only because casino is MOCK-only until then.
> - **Runbook.** Revocation after a compromise triggers a mandatory
>   `casino_consistency` run over the compromise window before a
>   replacement key is activated. Once a real source exists, a
>   `casino_statement` run is also required.
> - Forged wins are corrected only by compensating entries. That depends
>   on LEDGER-MANUAL-ADJ-4EYES-1, which is not built and is a go-live
>   blocker.
> - Registered, not built:
>   - CAS-WIN-ANOMALY-1, a detection-only alert before real-money casino
>     go-live;
>   - PROV-REVOKE-ALL-1, a cross-tenant revoke, triggered when a second
>     tenant shares a provider.
>
> **Suspended tenant: UNCHANGED (HD-10.3-4).** No new policy. The existing
> behaviour, verified in `internal/httpserver/webhook_preamble.go` at
> `c90e591`, is:
> - The shared `webhookPreamble` runs these steps in order:
>   1. the provider-id charset check;
>   2. the bounded body read;
>   3. the MOCK header format check (`Scheme.CheckPreamble`);
>   4. the platform-scoped `identity.GetTenantBySlug`;
>   5. `if t.Status != "active"`.
>
>   `tenants.status` is one of `active`, `suspended` or `closed` (migration
>   `0001`).
> - For a `suspended` or `closed` tenant, step 5 writes the **uniform 401
>   "callback rejected"** and logs one allow-listed auth-failure line with
>   reason `tenant_inactive`.
> - This happens **before** `WithTenant`, before credential resolution
>   and before verification. The event type lives inside the unverified
>   body and is never read.
> - The rejection is therefore identical for bet, win and rollback. It is
>   also identical for the payments and KYC routes, which share the
>   preamble.
> - Nothing is written: no ledger posting, no tombstone, no audit row. W2b
>   adds no rejection record either, because that record is written only
>   for verified callbacks.
> - **Consequences, disclosed:**
>   - While a tenant is suspended, its open rounds cannot settle.
>   - An unseen-original rollback delivered during the suspension leaves
>     no tombstone. If the provider gives up, a late original that arrives
>     after reactivation is not blocked. Detection relies on casino
>     reconciliation: C-stream metrics now, and the statement once a real
>     source exists.
>   - After reactivation, redelivered callbacks follow the normal contract.
> - W1a moves the scheme lookup and `Extract` ahead of the tenant lookup.
>   The tenant-status check stays a pre-verification uniform 401, so this
>   behaviour is unchanged.
> - Changing it, for example to let a suspended tenant's verified
>   settlement through, needs a new human decision. It may carry licensing
>   weight.
>
> **Reconciliation pointer (W2b/W3a).** Three pieces are specified in
> paper 02 §2 and follow the `sportsbook_settlement` pattern (advisory
> lock, append-only evidence, a P1 log line, never auto-correcting):
> - the `casino_consistency` stream, checks C1–C7;
> - the verified-only, append-only `casino_callback_rejections` record;
> - the `casino_statement` stream, with a MOCK `CasinoStatementSource`.
>
> **Status.** `NOT IMPLEMENTED` at acceptance. Target:
> `IMPLEMENTED — MOCK provider only` (W1c). A real aggregator resolver
> remains `NOT IMPLEMENTED`.
>
> **Status at gate 10.3-W1 (PASSED; see
> `docs/plans/stage-10.3-planning/05-gate-log.md`).** CAS-CAP-ROLLBACK-1
> and G-1 (CAS-MULTIBET-WIN-1): `IMPLEMENTED — MOCK provider only`.
> `ledger-finance` conditions C1–C6, C8 and C10 are met (re-verification
> after fix round A) and C11 is met in `98a7f08`. C7 (the security side of
> losing the settlement kill switch) is `security`'s: its gate review
> accepted credential revocation as the emergency stop (review §7, C14).
> C9 is W2b scope. Item 8's free-round/jackpot conformance case is
> `NOT IMPLEMENTED`. A real aggregator resolver and adapter remain
> `NOT IMPLEMENTED` (`PROVIDER DEPENDENT`), and are not wired until W2a
> revocation exists.

### 6. Bet / Win / Rollback — no second balance system, existing ledger only

`financial-transaction-flows.md` §5-7 (already `BLUEPRINT`-status, not
modified by this ADR) is the binding specification:

- **Bet**: debit `player_cash` (bonus-split deferred — see the scope note
  below), credit `house_gaming`. Idempotency key `(provider_id,
  provider_tx_id)` = the provider's own bet/round reference. Insufficient
  funds is checked and rejected **before** posting, inside the same
  transaction (invariant #15's read-then-write-atomically pattern,
  identical to `RequestWithdrawal`'s balance check) — a declined bet
  produces **no** `LedgerTransaction` row. **The wallet a bet debits is
  resolved from the platform's own `casino_launch_sessions` row the
  callback names (`session_id`), never from a payload-supplied
  `player_account_id` directly** — closed as a specialist-review finding
  (see this ADR's own findings section): a bet callback with no session
  binding let a validly-signed provider debit an arbitrary player within
  the tenant, with no record a launch ever occurred, and could not
  distinguish a demo round from a real-money one. `postBet` rejects a
  callback with no `session_id`, one naming an unknown/revoked session, a
  different provider's session, a demo-mode session, or a mismatched
  asset — all before touching the ledger.
- **Win**: debit `house_gaming`, credit `player_cash`. Idempotency key is
  the win's own `(provider_id, provider_tx_id)`, distinct from the
  triggering bet's. **The wallet a win credits is resolved from the
  round's own bet transaction's ledger entries — the same `player_cash`
  account that bet actually debited — never from the win callback's own
  `player_account_id`**, closed as a specialist-review finding (empirically
  reproduced: a win naming a different player was credited in full). A win
  callback naming a round with no matching, still-valid (never rolled
  back) prior bet transaction is rejected as an **integrity alert** (a
  provider protocol violation, not a routine failure) — logged and audited
  at elevated severity, never silently 404'd the way an ordinary not-found
  is. A win on a round whose bet has already been rolled back (a voided
  round) is the identical integrity violation, not a separate case.
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
  rather than posted after the fact. The original-transaction lookup takes
  a row lock (`SELECT ... FOR UPDATE`) before checking whether it has
  already been reversed — closed as a specialist-review finding
  (empirically reproduced: two concurrent, distinct rollback references
  for the same bet both succeeded, doubling the reversal credit) — and a
  redelivery of the identical rollback reference (including one targeting
  an already-tombstoned original) is a true idempotent no-op, never
  `ErrAlreadyRolledBack`.

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

## Specialist review findings and fixes

Seven parallel specialist reviews (casino integration architecture,
financial correctness, security, PostgreSQL/RLS, API/HTTP, multi-tenancy,
adversarial testing) ran against the initial Stage 4A implementation.
Several independently converged on the same root causes; all were fixed,
each with a dedicated regression test, before this stage was declared
complete:

- **P1 (multi-tenancy, security, architect — independently converged)**:
  the launch-session credential §3 designates as the binding between a
  player and a provider's callbacks was minted but never consulted on the
  bet/win posting path — `ReceiveCallback` trusted a payload-supplied
  `player_account_id` directly. Fixed by requiring a bet callback to carry
  `session_id`, resolving player/wallet/asset/mode from the platform's own
  `casino_launch_sessions` row, and rejecting a missing/unknown/wrong-
  provider/demo-mode/asset-mismatched session (§6's Bet bullet, above).
- **P1 (ledger-finance, empirically reproduced)**: a win callback credited
  whatever `player_account_id` its own payload named, with no binding to
  the round's actual bettor — a win naming a different player was credited
  in full. Fixed by resolving the win's payee from the round's own bet
  transaction's ledger entries instead (§6's Win bullet, above), which
  also closes the related "win on an already-rolled-back bet" gap.
- **P1 (ledger-finance, empirically reproduced)**: two concurrent, distinct
  rollback references for the same bet both succeeded, doubling the
  reversal credit. Fixed with a row lock (`SELECT ... FOR UPDATE`) on the
  original-transaction lookup inside `postRollback` (§6's Rollback bullet,
  above), proven under real concurrency by
  `TestReceiveCallback_ConcurrentDistinctRollbacksOnlyOneSucceeds`.
- **P1 (multi-tenancy)**: a tenant's own `CasinoProviderCapability` row —
  documented as an operator kill switch — had no effect on the bet/win/
  rollback path; only the process-global adapter registry decided whether
  a callback posted. Fixed by checking the tenant-wide capability
  (`status = active` and the specific operation's `supports_*` flag)
  inside `ReceiveCallback` before dispatch, for every event type.
- **P1 (qa, empirically reproduced)**: `CallbackEvent.Outcome` was parsed
  from the signed payload but never enforced on the bet/win posting path —
  a provider-declared `declined`/`ambiguous` outcome posted identically to
  `succeeded`. Fixed by rejecting any bet/win callback whose own `Outcome`
  is not `succeeded` (`ErrOutcomeNotSucceeded`).
- **P1 (casino architecture, qa)**: no concurrency tests existed for the
  casino financial/launch paths at all, contrary to CLAUDE.md's mandatory
  financial-test list. Closed with dedicated concurrent tests for
  duplicate-bet idempotency, distinct-rollback exclusivity, and single-use
  launch-token resolution.
- **P1 (API/HTTP, security)**: zero HTTP-level tests existed for any
  casino route, so the route table's authorization/tenant-scope/webhook-
  signature guarantees were unverified by anything but code inspection.
  Closed with `internal/httpserver/casino_flow_integration_test.go`
  (player-denied-on-admin-routes, platform-only-catalogue-management,
  tenant-scoped-config-routes, cross-tenant-availability-invisible,
  webhook tenant-enumeration-resistance, unsigned-payload-rejected).
- **P2s fixed**: a redelivered rollback whose original was never seen
  (tombstoned) previously errored instead of repeating its idempotent
  result; `ListAvailableGames` resolved most-specific-row-wins in the
  wrong order (filtering `enabled` before selecting the winning row, so a
  brand's explicit opt-out could be overridden by a tenant-wide opt-in);
  `casino_launch_sessions` had no immutability trigger (migration 0036
  closes this, mirroring `withdrawal_requests`'s); `token_hash` was a
  platform-global unique constraint rather than tenant-scoped (migration
  0036 closes this too, per migration 0021's own stated rule); the webhook
  echoed raw internal error text for `ErrInvalidInput`; a mock callback's
  HMAC canonicalization did not reject embedded NUL bytes; missing cross-
  tenant forged-insert tests for `casino_game_availability`/
  `casino_provider_capabilities`.
- **Not fixed this stage, explicitly deferred** (recorded here per
  CLAUDE.md's "record it as a decision" rule, not silently dropped): no
  player-account/wallet-status (suspended/self-excluded/frozen) check at
  game launch or bet time — a pre-existing, platform-wide gap (deposits
  have the same one), not introduced by this stage, but game launch is the
  canonical responsible-gaming enforcement point and this must close
  before any real-money go-live; the identical unguarded-reversal-lookup
  race this ADR fixes for casino also exists in `internal/payments`
  (`internal/payments/orchestrator.go`'s deposit-reversal check) and should
  receive the same `FOR UPDATE` fix in a future pass; per-tenant provider
  signing keys (today one secret per adapter *instance*, shared across
  every tenant that routes to it) are a Stage 4B precondition for any real
  provider, not a Stage 4A gap.

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
