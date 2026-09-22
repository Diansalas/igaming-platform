# 0048 — Casino Play-Simulation Trust Boundary (Stage 7)

## Status

Accepted. Superseded automatically the moment a real `CasinoProvider`
adapter is integrated (see "Removal condition" below) — this is not a
pattern intended to survive Stage 7.

## Context

Stage 7 builds the first B2C casino vertical slice: lobby → catalogue →
launch → wager → win → rollback → history → Back Office → audit. There is
no real, hosted casino-provider game client to embed this stage (CLAUDE.md's
Stage 7 scope explicitly bans real provider integration). In a real
integration, the provider's own hosted game calls the platform's public
webhook (`POST /v1/webhooks/casino/{tenantSlug}/{providerID}`,
`casino_handlers.go`) with bet/win/rollback events as the player plays,
each one signed with that provider's own credential.

To let the B2C "game/session screen" demonstrate the same flow without a
real game client, Stage 7 added three player-authenticated endpoints
(`POST /v1/me/casino/sessions/{id}/wager|win|rollback`,
`casino_play_handlers.go`). Each one asks the tenant's own registered
`*casino.MockCasinoProvider` to construct a correctly-signed callback
payload **on the authenticated player's own behalf**, then feeds it
through the exact same `Orchestrator.ReceiveCallback` pipeline the public
webhook uses.

## Decision

Accept this design for Stage 7, with the trust-boundary inversion it
creates made explicit and independently mitigated, rather than either (a)
building a second, parallel financial/session model for the vertical slice
(rejected — CLAUDE.md's "no uncontrolled scope expansion" and "no fake
completion"), or (b) blocking Stage 7 entirely until a real provider
contract exists (rejected — no commercial relationship is required to
prove the platform's own architecture supports a real player-facing casino
flow, which is Stage 7's actual objective).

## The trust-boundary inversion, precisely

`postBet`/`postWin`/`postRollback` (`internal/casino/orchestrator.go`) were
designed and hardened (Stage 4A onward) around one invariant: a payload
whose signature verifies is provider-attested, and every identifying field
on it (`provider_tx_id`, `original_provider_tx_id`, session binding) can be
trusted to the extent ADR 0025 already documents. That invariant holds for
the real webhook, where the signer is an independent, credentialed
provider with its own reasons to tell the truth about which transaction it
is naming.

It does **not** hold here: the mock adapter's signature is minted by this
process, at the request of the very same player whose authenticated
session triggered the call. The signature still verifies — HandleCallback
still checks it before touching any other field — but it authenticates
nothing about the player's own choice of `provider_tx_id`/
`original_provider_tx_id`/amounts, because the platform signed exactly
what the player asked it to sign.

## Consequence discovered during Stage 7's specialist review round

Four independent reviews (architect, security, ledger-finance,
database/RLS) each independently reproduced the concrete instance of this
inversion: the rollback endpoint's `original_provider_tx_id` was passed
through to `postRollback` with no additional authorization, because
`postRollback`'s own lookup (`tenant_id, provider_id, provider_tx_id` only)
is exactly right for a provider-attested field and was never designed to
receive a player-supplied one. This let a player, using their own
genuinely-owned launch session, name and reverse a **different** player's
(or their own, different round's) bet or win, and let a forged reference
write a permanent ledger tombstone (a tenant-wide denial-of-service, since
the ledger is append-only).

## Mitigations required by this ADR (all implemented before Stage 7 closed)

1. **Deployment gate, not just a provider-type gate.** `requireMockCasinoProvider`'s type assertion restricts these routes to the mock adapter, but a mock provider is, by definition, one that says yes to everything — that is not itself a safety property. `Deps.CasinoPlaySimulationEnabled` (`internal/httpserver/server.go`) gates route *registration* itself. These routes must never exist in a deployment where "production" means real money.

   **Update (Stage 9.4, `docs/decisions/0085-app-env-fail-closed-and-stateless-activation-seam.md`):** the single-condition gate this point originally described (`Environment != "production"` alone) was found, by two independent Stage 9.3 security reviews, to fail OPEN on a mis-set `APP_ENV` — a typo, wrong case, or unset value all silently registered this route on what an operator believed was production. `CasinoPlaySimulationEnabled` is now computed as `cfg.TestSupportRoutesEnabled()` (`internal/config/config.go`), a two-layer AND: (i) `Environment` validated against a closed set at startup, never inferred, and (ii) a second, independent, explicit opt-in (`TestSupportEndpointsEnabled`/`TEST_SUPPORT_ENDPOINTS_ENABLED`) that must ALSO be true. A single mistake in either layer alone can no longer register this route in production; see ADR 0085 for the full design and the config source of truth in `internal/config/config.go`'s `Environment` doc comment.
2. **Every field a player supplies through this seam that `postBet`/`postWin`/`postRollback` would otherwise trust as provider-attested must be independently re-authorized in `casino_play_handlers.go` before the payload is ever signed.** Concretely: `requireRollbackTargetOwnedByRound`/`casino.TransactionBelongsToRound` require the named `original_provider_tx_id` to belong to the calling session's own round (`correlation_id`) and wallet before it is ever handed to `ReceiveCallback`. `requireRealMode`/`requireActiveUnexpiredSession` re-check session mode/status/expiry that `postBet` alone does not fully cover and `postWin`/`postRollback` never consult (they resolve accounts via correlation id, not a session lookup).
3. **Bounded amounts.** `stake_amount`/`win_amount` are capped
   (`maxCasinoPlaySimulationAmount`) — a player declares their own win
   amount here, since there is no real game outcome to derive it from, so
   the value must be bounded even inside a gated non-production
   environment.
4. **Idempotent by construction, not by luck.** `wager`/`win` require a
   client `idempotency_key` and derive a deterministic
   `provider_tx_id` from it (mirroring `POST /v1/me/sportsbook/bets`'
   established contract), so a client retry after a lost response cannot
   mint a second financial transaction.
5. **Player-attributed audit trail.** Every successful call additionally
   writes a `casino_play_simulation.{wager,win,rollback}` audit entry with
   `actor_type=player` and the true originating player's id — alongside,
   not instead of, `postBet`/`postWin`/`postRollback`'s own existing
   `ActorSystem` records, which remain correct for their real intended
   caller (a provider webhook) and are unchanged by this ADR.

## What this ADR does not do

It does not change `postBet`/`postWin`/`postRollback` themselves, the
ledger schema, or any RLS policy. Every mitigation above lives entirely in
`internal/httpserver/casino_play_handlers.go` and the small
`casino.TransactionBelongsToRound` read-only helper it calls
(`internal/casino/history.go`) — the orchestrator's own contract, correct
for its real intended caller, is untouched.

## Known residual limitation (accepted, not a defect)

The Back Office "minimum-visibility" round queue and the player's own
round history (`internal/casino/history.go`) recompute a round's ledger
effects by re-deriving `correlation_id` from the session's own id, a
convention **this play-simulation seam introduces** (a real provider's own
round id is whatever it declares, per `CallbackEvent.RoundID`'s own doc
comment, and is never assumed to equal a session id). This means the
Stage 7 visibility views are accurate for simulation-originated rounds but
would report a real provider's own round as blank if one ever posted
directly to the public webhook without an accompanying session-anchored
round id. Since no real provider exists yet, this was disclosed at the
time as a known limitation of the read model, not fixed in Stage 7 (see
task-registry.md's Stage 7 technical-debt entry).

**Update (Stage 8, `docs/decisions/0080-provider-integration-readiness-
without-external-contracts.md`, Decision 1):** the durable fix this
section originally deferred has been added. A new table,
`casino_provider_rounds` (migration 0080), binds every provider-declared
`(tenant_id, provider_id, provider_round_id)` to the platform's own
`launch_session_id`/`player_account_id`/`brand_id`/`game_id`/
`correlation_id` the first time `postBet` (`internal/casino/
orchestrator.go`) sees it — durable, provider-neutral, and independently
queryable by provider identifiers alone (`casino.LookupProviderRound`),
not only by session id. This resolves the limitation described above for
any caller that queries by provider identifiers, including a future real
provider's own round id: it now has somewhere durable to be looked up
from, and a `provider_round_id` already bound to a different session/
player/brand in the same tenant is rejected outright
(`casino.ErrProviderRoundOwnershipConflict`) rather than silently
misattributed.

This is purely additive and does **not** change `history.go`'s own
existing correlation-id-recompute read path described above, which
remains correct and unmodified — Decision 1 adds a durable, independently
-queryable index alongside it, it does not replace it. `history.go` itself
was not updated to consult the new table this stage (out of Stage 8's own
scope); a future stage may choose to have the Back Office/history views
prefer `casino_provider_rounds` when a binding exists, falling back to the
recompute path otherwise.

## Removal condition

The moment a real `CasinoProvider` adapter is registered for any tenant,
these three endpoints' entire reason to exist disappears — a real
provider's own hosted game client drives bet/win/rollback via the public
webhook, signed with its own credential, and Stage 7's play-simulation
routes should be removed from the route table (or left permanently
`CasinoPlaySimulationEnabled=false` in every environment that also
configures a real provider) rather than left running alongside it.
