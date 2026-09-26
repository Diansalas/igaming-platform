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
