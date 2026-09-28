# Casino Vendor Launch-Token Bootstrap — Integration Contract

## Status: MOCK. IMPLEMENTED for the MOCK adapter; PROVIDER DEPENDENT for any real vendor.

This is the platform's own contract for the endpoint a casino aggregator/vendor calls to redeem
("bootstrap") the single-use launch token `LaunchGame` embeds in the launch URL, per ADR 0025 §3 and
ADR 0103 (`docs/decisions/0103-casino-launch-token-bootstrap-contract.md` — the authoritative design
record; this page is a shorter, implementer-facing summary of the same contract, plus its actual,
verified implementation status).

**A real vendor speaking this contract does not exist.** Everything below is exercised only by
`internal/casino.MockCasinoProvider`'s own bootstrap client (`BootstrapPayload`), which signs a
request with the same per-tenant derived key `CallbackPayload` uses and sends it over a real
`net/http` call in tests — never an in-process shortcut. **CAS-PLAYER-REF-1 must close before this
contract is offered to any real vendor** (see "Known limitation" below).

## Endpoint

```
POST /v1/webhooks/casino/{tenantSlug}/{providerID}/launch-bootstrap
```

Same authentication surface as the bet/win/rollback callback route
(`POST /v1/webhooks/casino/{tenantSlug}/{providerID}`): the tenant slug in the URL is a lookup hint
only; the request must be signed with the tenant+provider's own registered inbound webhook
credential (the same scheme, the same credential resolver, the same admission bulkhead). No bearer
token — this is not an authenticated platform principal.

## Request

Signed JSON body (the signature covers these exact bytes, headers per the scheme in force):

```json
{
  "launch_token": "<the opaque token from the launch URL's own token= query parameter>",
  "request_id": "<caller-chosen idempotency key, ^[A-Za-z0-9_.:-]{1,128}$>",
  "provider_game_id": "<the vendor's own game id>",
  "asset_code": "EUR",
  "mode": "real"
}
```

- The tenant and provider are **never** accepted from the body — only from the route/credential.
  An unknown field (including a body-supplied `tenant_id`) is refused.
- `launch_token` is the same opaque, single-use, 256-bit token `LaunchGame` minted — never the
  player's own session/JWT.
- `request_id` is the vendor's own idempotency key for this specific bootstrap attempt.

## Response

**200 OK** on success:

```json
{
  "session_id": "<platform launch session id, UUID>",
  "player_ref": "<opaque, provider-scoped UUID>",
  "provider_game_id": "<echoed>",
  "asset_code": "<echoed>",
  "mode": "<echoed>"
}
```

A retried request with the same `request_id` and the same signed body returns this **exact same
byte sequence** (an idempotent replay, not a fresh consume) — see "Idempotency" below.

**401** `{"code":"unauthorized","message":"callback rejected", ...}` — the uniform refusal for
*every* pre-verification failure and *every* non-matching replay: bad/missing signature, unknown
tenant/provider, the token not resolving to an active session bound to this exact
(provider, tenant, mode, asset, provider_game_id), an expired token, a reused `request_id` with a
different token or different fields, or a credential handle revoked between verification and the
transaction. **The reason is never distinguishable from the response** — server-side logs carry a
closed reason enum for operators only.

**403** `{"code":"forbidden","message":"launch not permitted"}` — a constant body for a
**definitive** policy denial (the game or capability is off, or the player is RG-ineligible) found
during the bootstrap's own gate re-check. The session is revoked as a result; the reason is never
disclosed to the vendor (RG status is player-sensitive).

**5xx** — a genuine platform-side failure (a database error, or an internal invariant check
failing). The session is left exactly as it was; nothing is written.

## Idempotency

- The idempotency key is `(tenant_id, provider_id, request_id)`, bound to the launch token's own
  hash and to a digest of the other signed fields (`request_digest` — never the token itself).
- A replay is honoured (the stored response returned, byte-identical) **only** when the token hash
  matches, the digest matches, **and** the session is currently `consumed` — a session a later
  revoke (workstream A, migration 0108) has since moved to `revoked` is never resurrected by a
  replay.
- Exactly one bootstrap ever succeeds per session (`UNIQUE (launch_session_id)`), and exactly one
  bootstrap row exists per `(tenant_id, provider_id, request_id)`.

## Player reference

`player_ref` is a random UUID, unique per `(tenant, provider, player_account_id)`, created on first
bootstrap and stable thereafter. It is unlinkable across tenants and providers, and never derived
from a secret. **Known limitation (CAS-PLAYER-REF-1, registered, must close before any real
vendor):** the platform's own `LaunchRequest` (the earlier, still-unchanged phase-B call to
`CasinoProvider.Launch`) still passes the player's raw `player_account_id`, not `player_ref` — so
the opacity this contract introduces holds only against a vendor that does not also receive the
`Launch` call with the raw id. This bootstrap contract does not by itself close that gap.

## What this endpoint does NOT do

- It does not re-run jurisdiction resolution, the game blocklist, or risk evaluation — those are
  `LaunchGame`'s own job at mint time (frozen onto the session) and `postBet`'s own job on every
  bet. It re-checks only: the game's own platform status, the provider capability
  (active/supports_launch/supports_bet for real-money/asset support), and RG eligibility.
- It does not decide whether a real-money bet should require a bootstrapped (`consumed`) session —
  that is a separate, still-open decision (CAS-BET-REQUIRES-BOOTSTRAP-1, registered; `postBet`
  currently accepts both `active` and `consumed`).
- It does not widen the pre-existing B2C "play simulation" routes
  (`POST /v1/me/casino/sessions/{id}/wager` and siblings, `casino_play_handlers.go`), which retain
  their own, separately-reviewed `'active'`-only precondition (security finding P2-2) and are
  therefore **not** usable against a session this endpoint has consumed. The real provider path
  (a genuinely-signed bet callback through `postBet`) is unaffected.

## Reference implementation

- `internal/casino/bootstrap.go` — `Orchestrator.BootstrapLaunch`.
- `internal/casino/mock.go` — `MockCasinoProvider.BootstrapPayload` (the MOCK client).
- `internal/httpserver/casino_bootstrap_handlers.go` + `casino_routes.go` — the HTTP route.
- `migrations/0115_casino_launch_bootstrap.{up,down}.sql` — `casino_launch_bootstraps` and
  `casino_provider_player_refs`.
- Tests: `internal/casino/bootstrap_integration_test.go`,
  `bootstrap_rls_integration_test.go`, `bootstrap_sb1_integration_test.go`,
  `migration_0115_bootstrap_integration_test.go`;
  `internal/httpserver/casino_bootstrap_integration_test.go`.

## Path to a real vendor

1. Close CAS-PLAYER-REF-1 (switch the `Launch` call to `player_ref`).
2. Decide CAS-BET-REQUIRES-BOOTSTRAP-1 (whether a real-money bet requires a bootstrapped session).
3. Confirm a real commercial/technical relationship exists (CLAUDE.md: never build against real
   vendor credentials without one).
4. Build the real adapter behind `casino.CasinoProvider`, composing
   `internal/providers/httpclient` and `internal/providers/config`, validated against this same
   contract shape (the platform side of the contract does not change per vendor — only the vendor's
   own signature scheme/credential type might, via `webhookauth.VerificationScheme`).
