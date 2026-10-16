# Casino aggregator provider-integration package

Status: **DOCUMENT / SPECIFICATION PACKAGE ONLY.** No adapter, no vendor call, no credential, no account, no agreement.
Prepared 2026-10-16 by `integrations` on branch `provider-packages-rest` (base: `claude/focused-wright-jw88w9` at `4a3c85f`).
Provider-integration phase item 2 of 5 (PSP -> **casino** -> KYC/AML -> sportsbook -> sports data; see
`../README.md`). Same structure and rigour as `../psp/`.

Labels (CLAUDE.md): everything in this directory is `NOT IMPLEMENTED` as software. No casino aggregator is selected, no
vendor documentation is available (owner statement 2026-10-16), and `docs/integrations/dummy-casino.md` records that the
earlier "Dummy Casino" contract was never available. **This package therefore asserts no vendor behaviour.** Every
statement about what a vendor does is either a requirement the platform places on a vendor ("must supply") or is marked
`UNVERIFIED (needs provider documentation/sandbox)` with an id `U-CAS-nn`. The platform side (what the existing
provider-neutral interfaces require and expect) is verified against code at base `4a3c85f`, with paths cited.

Nothing here is a statement that any aggregator, game studio or game is licensed, contracted or approved, or that any
game is legal in any jurisdiction (CLAUDE.md "Compliance": software capability is not legal approval).

## Index

| File | Content |
|---|---|
| `provider-neutral-mapping.md` | Platform concept -> what the aggregator must supply -> how the neutral interface carries it -> gap/UNVERIFIED, with code paths. Platform-gap candidates `CAS-G01..` (section 11) and the UNVERIFIED registry `U-CAS-nn` (section 12). |
| `conformance-plan.md` | The existing casino suites a real adapter must run unchanged, what must change to make them runnable for a non-mock adapter, the new cases `CAS-C-nn`, evidence per case. |
| `integration-gates.md` | Gates C0..C9 (documentation review -> adapter -> conformance -> security -> ledger-finance -> sandbox -> operational acceptance -> pre-production -> explicit production authorization), entry criteria, evidence, and the exact owner/vendor inputs. |
| `open-questions.md` | The minimum decisions/inputs `OQ-CAS-nn`, each with a recommendation. |

## What the platform already owns for casino (verified, MOCK only)

The platform owns the adapter boundary, the session/launch model, idempotent wallet-callback posting, the tombstone for
out-of-order rollback, the bonus-funded settlement-credit resolution, the RG/Risk/KYC/AML/market/game-kill gates on new
exposure, and the casino reconciliation streams. The vendor owns the games, RNG, RTP certification and round logic
(CLAUDE.md "does not own games"). All of it is exercised only against `casino.MockCasinoProvider`
(`internal/casino/mock.go`) and `casino.MockStatementSource` (`internal/casino/statement_mock.go`).

## Headline gaps (detail in `provider-neutral-mapping.md` section 11)

1. `CAS-G01` The vendor-facing wire contract is platform-defined: the callback response body and HTTP status mapping are
   fixed in `internal/httpserver/casino_handlers.go`, the bootstrap request is parsed against one fixed JSON shape
   (deferred hook F-12). Whether a real aggregator can live with that is `UNVERIFIED`; the likely need is an additive
   adapter-side request/response translation hook.
2. `CAS-G02` Only `bet`, `win` and `rollback` callback event types exist. There is no inbound balance route, no round-close,
   no combined bet+win, no adjustment, no free-round or jackpot event. An unknown event type is a 400.
3. `CAS-G03` Free rounds/jackpot/promo payouts with no platform bet: a documented requirement (ADR 0025 amendment item 8)
   with no conformance case and no design.
4. `CAS-G07` The casino statement source is one process-global object with no per-(tenant, provider) registry, no
   read-only attestation, no stored append-only copy, no real mismatch kind, and the matcher is O(history)
   (CAS-RECON-SCALE-1). This blocks real money, not the sandbox.
5. `CAS-G06` Nothing calls `CasinoProvider.Catalogue()` outside tests; catalogue enters through the admin route. RTP variant
   is one free-text field on a platform-wide game row; `demo_supported` and `feature_flags` are informational.

## Principles applied

- Providers adapt to the neutral interface (`internal/casino/types.go` `CasinoProvider`); the core is not redesigned around
  the first aggregator. Where a real aggregator is likely to expose a missing abstraction it is a numbered gap with scope
  (provider-specific vs platform-wide), the reviewing specialist and, only if necessary, a minimal core change proposal.
  No core change is made by this package.
- The vendor never receives a platform player id, tenant id, email or PII (opaque `player_ref` only; CAS-PLAYER-REF-1).
- Settlement of existing exposure is never blocked by capability, kill switch, RG, KYC or market state; only NEW exposure
  is gated (ADR 0025 Stage 10.3 amendment). The emergency stop is credential revocation, which strands open exposure.
- Sandbox only until explicit production authorization; synthetic tenant; no real money; no real player data.
