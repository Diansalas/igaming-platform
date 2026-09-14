# Active Stage

## Stage 4A — Casino Integration Foundation — Complete

Status: **Complete, pending human approval to authorize Stage 4B.** Issued
immediately after the human approved Stage 3D, as the "CASINO INTEGRATION
FOUNDATION" directive - a production-grade, provider-agnostic casino
integration layer, NOT a real casino provider integration. Real provider
contracts, production credentials, sportsbook, the Bonus Engine, KYC/AML,
Responsible Gaming enforcement, a player-facing lobby UI, and a full
back-office catalogue UI were explicitly out of scope.

### What was built

1. **`CasinoProvider` interface** (`internal/casino/types.go`) -
   provider-neutral, no SDK types, no free-form passthrough fields:
   `Catalogue`, `Launch`, `Balance`, `Bet`, `Win`, `Rollback`,
   `HandleCallback`, `Capabilities`, `HealthStatus`.
2. **Game catalogue split** - `casino_games` (platform-wide, no RLS, like
   `assets`) vs. `casino_game_availability` (tenant-owned, RLS, opt-in
   only, fail-closed).
3. **Game launch and session model** - an opaque, single-use,
   database-backed launch token (never the player's JWT), mirroring
   `internal/auth`'s refresh-token generation in a wholly separate trust
   domain; the session row itself persists as the round's own identity
   anchor for subsequent bet/win callbacks.
4. **Two-layer provider capability model** - adapter-declared vs.
   operator-configured, narrowing-only, mirroring `docs/decisions/0022`'s
   payment-capability model.
5. **Bet/Win/Rollback financial boundary** on the already-approved ledger
   (Flows 5-7) - wallet/ledger remain sole financial truth; a provider's
   own reported balance is never consulted to authorize a posting.
6. **`MockCasinoProvider`** - the only registered adapter this stage,
   HMAC-signed synthetic callbacks, deterministic decline/ambiguous/
   failure behavior, run through the identical `CasinoProvider` contract
   any future real adapter must also pass.
7. **HTTP layer** - player catalogue/launch, the provider callback webhook
   (adapter-verified signature, no bearer-auth middleware), and admin
   endpoints for platform catalogue management, tenant capability
   configuration, and tenant game-availability configuration.
8. **Provider conformance suite** covering directive items A-T, plus
   dedicated regression tests for every specialist-review finding and
   concurrency tests for the financial/launch paths, plus a new HTTP-layer
   authorization/tenant-isolation/webhook-authentication test file.

Full design, rationale, and the complete specialist-review findings/fixes
list: `docs/decisions/0025-casino-provider-abstraction-and-game-session-
model.md`. Narrative architecture doc:
`docs/architecture/08-casino-integration-architecture.md`.

### Specialist review: six P1s found and fixed

An independent 7-specialist parallel review (casino integration
architecture, financial correctness, security, PostgreSQL/RLS, API/HTTP,
multi-tenancy, adversarial testing) found six P1s before this stage was
considered complete, three empirically reproduced during review:

1. The launch-session credential was minted but never consulted on the bet
   path - a payload-supplied `player_account_id` could authorize an
   arbitrary player's debit, and demo vs. real-money was indistinguishable
   at posting time. **Fixed**: `postBet` now requires and resolves
   player/wallet/asset/mode from the platform's own `casino_launch_
   sessions` row.
2. A win callback credited whatever player its own payload named, not the
   round's actual bettor (empirically reproduced: a different player was
   credited in full). **Fixed**: `postWin` resolves the payee from the
   round's own bet transaction's ledger entries instead.
3. Two concurrent, distinct rollback references for the same bet both
   succeeded, doubling the reversal credit (empirically reproduced).
   **Fixed**: a row lock (`SELECT ... FOR UPDATE`) on the original-
   transaction lookup in `postRollback`.
4. A tenant's own `CasinoProviderCapability` - documented as a kill switch
   - had no effect on the bet/win/rollback path. **Fixed**: `ReceiveCallback`
   now checks it before dispatch.
5. A provider-declared `declined`/`ambiguous` `Outcome` on a bet/win
   callback posted identically to `succeeded` (empirically reproduced).
   **Fixed**: rejected outright (`ErrOutcomeNotSucceeded`).
6. Zero concurrency tests existed for the casino financial/launch paths,
   and zero HTTP-level tests existed for any casino route. **Fixed**:
   dedicated concurrency tests plus a new HTTP-layer test file.

All six were fixed, each with a dedicated regression test. Several P2s
(redelivered-tombstoned-rollback idempotency, a catalogue most-specific-
row-wins ordering bug, a missing `casino_launch_sessions` immutability
trigger, a platform-global rather than tenant-scoped `token_hash`
uniqueness, a webhook raw-error-text leak, an HMAC NUL-byte canonicalization
gap) were also fixed. Full detail for every finding is in ADR `0025`'s own
"Specialist review findings and fixes" section.

### Verification performed

`gofmt -l .` clean. `go build ./...`, `go vet ./...`, `go vet -tags=integration
./...` clean. `go test ./...`, `go test -race ./...`, `go test -tags=integration
./...`, `go test -race -tags=integration ./...` all pass across the full
repository. Migration `0036` round-tripped (`up` → `down` → `up`) cleanly;
migration `0035`'s round-trip was verified clean on a fresh database
(directly, and independently by the architect specialist review) before
the test suite itself posted real casino ledger rows to the dev database,
after which `0035`'s own down migration correctly refuses to run (an
append-only-ledger property, documented in that migration's own down file,
not a defect).

### Pending (to close out this stage)

- Commit and push this work to `claude/focused-wright-jw88w9`.
- Stage 4A Completion Report delivered to the human, ending with the
  required closing statement. No Stage 4B work begins until explicitly
  authorized.

### Blockers / genuine scope boundaries (not defects)

None block Stage 4A's own approved scope, which is complete. The
following are honestly labeled boundaries and open decisions for future
stages, recorded per CLAUDE.md's "record it as a decision" rule rather
than silently dropped - full detail in ADR `0025`:

- **No player-account/wallet-status (suspended/self-excluded/frozen) check
  at game launch or bet time.** A pre-existing, platform-wide gap
  (deposits have the identical one), not introduced by this stage - but
  game launch is the canonical responsible-gaming enforcement point and
  this must close before any real-money go-live.
- **The identical unguarded-reversal-lookup race this stage fixed for
  casino also exists in `internal/payments`** (its own deposit-reversal
  check) - should receive the same `FOR UPDATE` fix in a future pass.
- **Per-tenant provider signing keys are not implemented** - today one
  secret per adapter *instance*, shared across every tenant routed to it.
  A Stage 4B precondition for any real provider, not a Stage 4A gap (no
  real provider exists yet to exploit this).
- Free-round/bonus-stake normalization and jackpot-contribution splits
  remain `OPEN DECISION`s owned by the Bonus Engine stage - Stage 4A's
  bet/win posting assumes 100%-`player_cash`-funded stakes only.
- A catalogue sync job, a jurisdiction-*resolution* engine (beyond the
  static per-game blocklist check), and a back-office catalogue UI remain
  **NOT IMPLEMENTED**, per this stage's explicit scope-control list.
- Sportsbook, the Bonus Engine, KYC/AML, full Responsible Gaming, a
  player-facing lobby UI, production payment providers, and crypto
  custodian integration remain **NOT IMPLEMENTED**, per this stage's
  explicit block list.
- The Stage 3D TOCTOU risk (submit/resolve eligibility window) is carried
  forward unchanged - this stage's implementation did not touch the
  affected withdrawal-approval path, so there was nothing here that would
  close or worsen it.

### Decisions/input still useful from the human before the next stage

1. Approve Stage 4A and authorize Stage 4B (per CLAUDE.md's stage gate - a
   real casino provider integration, sportsbook, bonus, B2C frontend,
   partner console, production deployment, real PSP, and real crypto
   integrations do not begin automatically).
2. Decide whether the two explicitly-deferred items above (RG/self-
   exclusion enforcement at launch/bet, the payments-side concurrent-
   reversal race) should be closed before any production deployment, or
   whether they are acceptable to carry forward further.
3. When a real casino provider is contracted, its actual API documentation
   must be supplied before Stage 4B implementation begins - this stage
   invents nothing about a real provider's wire format.
4. The already-open, non-blocking business/compliance tracks carried
   forward from Stage 0-3D remain open (`docs/decisions/0005`; ADRs
   0017/0018's open items; `brands`' public-read RLS breadth; the
   promo_liability/bank-treasury/crypto-custodian accounting decisions
   that still block bonus and crypto financial posting specifically; the
   Stage 3D TOCTOU risk).
