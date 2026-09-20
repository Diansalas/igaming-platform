# Dummy Sportsbook API — Integration Status

## Status: PENDING — no documentation available

Stage 8 was originally scoped to integrate a "Dummy Sportsbook API" the
platform owner stated would be available. Reconnaissance at the start of
Stage 8 (repo grep, `.env.example` files, `deploy/docker-compose.dev.yml`,
`/etc/hosts`, filesystem search) found **no trace of this API anywhere
reachable from the development environment**.

The platform owner confirmed the documentation/contract is **temporarily
unavailable** and explicitly redefined Stage 8 to be provider-integration
*readiness* work only, with an explicit instruction not to invent, guess,
or assume this API's contract, and not to make any external network call
against it.

**Nothing in this document describes a real, discovered contract.** This
file exists only to record that fact plainly, per CLAUDE.md's "No fake
completion" rule and Stage 8's own explicit instruction.

## What is genuinely unknown (do not assume any of the following)

- Base URL / environment endpoints
- Authentication method
- Catalogue sync shape beyond the platform's own already-generic
  `sportsbook.Provider.Catalogue()`/`ExternalRef` convention (which
  makes no assumption about a specific provider either)
- Whether real bet placement is supported by this provider at all, and
  if so, whether it is synchronous (immediate ack) or asynchronous
  (pending/settled later), and what its acceptance reference looks like
- Settlement, void, partial-settlement, and cashout event shapes and
  delivery mechanism (all explicitly out of scope for the existing
  Stage 6 canonical domain model regardless — see
  `internal/sportsbook`'s own package doc comment)
- Provider-side event/market/selection/bet ID formats and scoping
- Error response shapes, timeout/retry semantics, idempotency guarantees

## What Stage 8 built instead (see ADR 0080)

- `docs/decisions/0080-provider-integration-readiness-without-external-contracts.md`
  — the full design record.
- `sportsbook_bets.provider_id` / `provider_bet_reference` (migration 0081,
  both nullable, symmetric-null constraint, partial unique index scoped
  `(tenant_id, provider_id, provider_bet_reference)`) — a provider-neutral
  place for a future real provider's own bet acceptance reference to be
  recorded, always NULL today since bet placement remains the existing
  synchronous, same-process `PlaceBet` flow.
- `sportsbook.Provider`'s interface is deliberately **not** extended with
  a bet-placement method this stage — adding one would require guessing
  a contract shape (sync vs. async, ack format) that is not known. This
  is documented as an intentional deferral in the interface's own doc
  comment, not an oversight.
- The same generic `internal/providers/httpclient` client and
  `internal/providers/config.go` loader built for the casino side (see
  `docs/integrations/dummy-casino.md`) are provider-agnostic and would
  be reused by a real sportsbook adapter too.
- `internal/sportsbook.MockSportsbookProvider` remains the only
  registered `Provider` adapter — deterministic, same-process, never a
  network call.

## Path to real integration

Once real documentation is provided:

1. If bet placement is supported: extend `sportsbook.Provider` (or add a
   sibling interface) with the actual, documented bet-placement method
   shape — determine the correct ordering between provider acceptance
   and financial posting from the real contract's own acknowledgement
   semantics; do not assume it mirrors casino's callback-based flow.
2. If bet placement is not supported: this platform's existing
   synchronous `PlaceBet` flow (stake locked into `player_locked_cash`
   entirely in-process) remains the strongest supported integration, and
   this limitation should be documented as a real, permanent constraint
   rather than worked around by inventing an endpoint.
3. Build the adapter composing `internal/providers/httpclient.Client`,
   configured via `internal/providers/config.LoadProviderConfig`, and
   validate it with the `internal/providers/httpclient/conformance`
   harness against a fake server shaped like the real contract.

This document should be rewritten in full — with the actual discovered
contract — the moment real documentation is available. Until then, it
records only what is not known.
