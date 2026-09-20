# Dummy Casino API — Integration Status

## Status: PENDING — no documentation available

Stage 8 was originally scoped to integrate a "Dummy Casino API" the
platform owner stated would be available. At the start of Stage 8, this
session performed reconnaissance across the repository, `.env.example`
files, `deploy/docker-compose.dev.yml`, `/etc/hosts`, and the local
filesystem, and found **no trace of this API anywhere reachable from the
development environment**: no base URL, no credentials, no OpenAPI spec,
no SDK, no sandbox host.

The platform owner confirmed the documentation/contract is **temporarily
unavailable** and explicitly redefined Stage 8 to be provider-integration
*readiness* work only, with an explicit instruction not to invent, guess,
or assume this API's contract, and not to make any external network call
against it.

**Nothing in this document describes a real, discovered contract.** This
file exists only to record that fact plainly, per CLAUDE.md's "No fake
completion" rule and Stage 8's own explicit instruction ("Create
documentation stating that external API integration is pending actual
provider documentation").

## What is genuinely unknown (do not assume any of the following)

- Base URL / environment endpoints
- Authentication method (API key, OAuth, HMAC signing, mTLS, or other)
- Catalogue retrieval shape (sports/games/provider IDs, pagination)
- Launch/session request and response shapes
- Bet/win/rollback request and response shapes, and whether these are
  provider-initiated (webhook/push) or platform-initiated (synchronous
  call), or both
- Callback/webhook delivery mechanism, signature scheme, and retry
  behavior
- Idempotency guarantees the provider itself makes (if any)
- Error response shapes and error codes
- Timeout and retry semantics the provider expects callers to use
- Provider-side session/round/transaction ID formats and their scoping
  (per-tenant, per-game, globally unique, etc.)
- Balance/settlement/rollback semantics
- Any provider-specific metadata this platform would need to store

## What Stage 8 built instead (see ADR 0080)

Because the real contract is unknown, Stage 8's actual work was
provider-integration **readiness**, not integration:

- `docs/decisions/0080-provider-integration-readiness-without-external-contracts.md`
  — the full design record for everything below.
- `casino_provider_rounds` (migration 0080) — a provider-neutral table
  able to durably bind a future real provider's own round id to a
  platform session/player/tenant/brand/game, resolving ADR 0048's
  documented residual limitation, without assuming any specific
  provider's round-id format or scoping (the chosen uniqueness scope,
  `(tenant_id, provider_id, provider_round_id)`, is documented in ADR
  0080 Decision 1 as a conservative assumption to be revisited once a
  real contract exists).
- `internal/providers/httpclient` — a generic, provider-name-agnostic
  outbound HTTP client (timeout, bounded idempotent-only retry, error
  classification, OpenTelemetry spans) that a real adapter will compose
  once its contract is known.
- `internal/providers/config.go` — a generic provider configuration
  loader (enabled/disabled, base URL, credential *reference*, timeout,
  retries) with fail-closed defaults. No specific provider is configured
  or wired into `cmd/platform-api/main.go` this stage.
- `internal/casino.MockCasinoProvider` remains the only registered
  `CasinoProvider` adapter. It is a deterministic, same-process test
  double — never a network call, never a stand-in for a real contract.

## Path to real integration

Once real documentation is provided, a new adapter package (e.g.
`internal/casino/dummyprovider` or equivalent) should be built that:

1. Implements `casino.CasinoProvider` exactly as documented in
   `internal/casino/types.go`.
2. Composes `internal/providers/httpclient.Client` for its network calls,
   configured via `internal/providers/config.LoadProviderConfig`.
3. Is validated with `internal/providers/httpclient/conformance`'s
   reusable test harness pointed at a fake server shaped like the real
   documented contract.
4. Revisits `casino_provider_rounds`' uniqueness scope (ADR 0080 Decision
   1) against the real provider's actual round-id scoping rules before
   going live.

This document should be rewritten in full — with the actual discovered
contract — the moment real documentation is available. Until then, it
records only what is not known.
