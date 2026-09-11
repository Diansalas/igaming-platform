# 08 — Casino Integration Architecture Proposal

Status: Stage 0 proposal. Source: Blueprint §4.3.

## Internal provider interface

Even though one aggregator integration unlocks thousands of games, the
platform defines its own internal interface — `launch()`, `catalogue()`,
and inbound `bet`/`win`/`rollback`/`balance` — rather than coding directly
against a single aggregator's API. Realistically the platform ends up with
two or three aggregators plus direct studio integrations (Evolution,
Pragmatic Play increasingly want direct commercial relationships), and
premium content is what B2B partners shop for.

## Catalogue

Per game: provider game id, RTP variant (the same title can ship at 96% or
88% — contractually determined, not a technical choice), volatility,
feature flags, supported currencies, mobile/demo support, and a
jurisdiction blocklist (some titles are illegal in some markets — serving
one is a licence problem, not a bug). Synced nightly plus on webhook.

## Free rounds — the messiest part

Every provider exposes free spins differently (some issue provider-side
against a campaign id, some expect the platform to credit them). This is
why the bonus engine talks to a normalised internal interface (owned by
`bonus-engine`, implemented per-provider by `casino`) rather than calling
provider APIs directly from bonus logic.

## The wallet-callback path

This is the highest-traffic, lowest-latency, most correctness-critical
surface in the system (Blueprint §3). The retry sequence:

1. Provider calls `POST /wallet/bet {txId, round, amt}`.
2. `INSERT ... ON CONFLICT DO NOTHING` against the ledger, unique on
   `(provider, txId)`.
3. Network drops the response before the provider sees the 200.
4. Provider retries with the **same** `txId` (this is the normal case, not
   an edge case — busy integrations see thousands of duplicate ids/day).
5. Second insert is 0 rows — no second debit — same balance returned.

The database constraint makes correctness hold under concurrency;
application-level "check then insert" does not. This endpoint must meet
the p99 < 150ms target (Blueprint §6) or providers will retry more
aggressively and eventually suspend the integration.

## Launch tokens

Single-use, opaque, bound to `(player, provider, game, currency, mode)`,
short TTL — never the player's brand session token (see `05-identity-
architecture.md`).

## Ownership and stage mapping

Owned by `casino`, with `ledger-finance` sign-off required on the wallet-
callback endpoint's ledger-posting logic. One aggregator adapter (mocked
initially, real once contracted) is Stage 3/4; full catalogue and
free-round normalisation mature through Stage 4.
