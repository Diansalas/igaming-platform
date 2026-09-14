# Active Stage

## Stage 4D-RG — Responsible Gaming Player-Status Enforcement Foundation — Complete

Status: **Complete, pending human approval to authorize the next stage.**
Issued immediately after Stage 4A's own specialist review identified a
production-blocking gap: no authoritative platform-side player-account/
wallet-status or self-exclusion check existed at casino game launch or
bet time. Real casino provider integration, sportsbook, the Bonus Engine,
full KYC/AML, deposit/loss/wagering/session limits, reality checks,
time-outs/cooling-off, a player-facing lobby UI, and a full back-office
RG UI were explicitly out of scope.

### What was built

1. **`internal/rg` package** (`internal/rg/rg.go`) - `player_restrictions`
   domain type, `CreateSelfExclusion` (player self-service, always
   platform-wide scope), `CreateStaffRestriction` (staff-initiated,
   tenant/brand-scoped only - never platform-wide, see "Blockers" below),
   `ListRestrictionsForAccount`, and `EvaluateEligibility` - the single
   authoritative "may this player gamble right now" policy boundary.
2. **No second identity model** - composes the EXISTING `PlayerAccount.
   Status` and `Wallet.Status` with exactly ONE new signal
   (`player_restrictions`), anchored on the platform-wide `Person`.
3. **Migrations `0037`/`0038`** - `player_restrictions`: append-only
   (RLS + a row-level AND statement-level deny-mutation trigger), dual-
   scope RLS (platform-wide vs. tenant vs. brand), separate player-self-
   service vs. staff INSERT policies, tenant-scoped SELECT policies (both
   hardened during specialist review - see below), a composite `(player_
   account_id, tenant_id)` FK.
4. **Casino launch/bet enforcement** - `internal/casino`'s `LaunchGame`
   and `postBet` both consult `EvaluateEligibility` via a shared
   `evaluateAndAuditEligibility` wrapper, before a launch session becomes
   usable and before a bet's financial debit commits respectively. A
   denial is a RESULT, never a Go error (see below).
5. **Concurrency**: a transaction-scoped Postgres advisory lock keyed on
   `person_id` closes the self-exclusion-vs-launch/bet TOCTOU race
   deterministically - proven under `-race` with real concurrent
   goroutines against real Postgres.
6. **New permissions** - `PermRGRestrictionWrite` (RoleCompliance only),
   `PermRGRestrictionRead` (RoleCompliance/RoleTenantAdmin) - never
   RolePlatformAdmin.
7. **Minimal HTTP API** - `POST/GET /v1/me/rg/self-exclusion`,`/status`
   (player self-service); `POST/GET /v1/admin/rg/restrictions`
   (staff, tenant/brand-scoped).
8. **Bet-callback idempotency hardening** (found and fixed during this
   stage's own review, not a pre-planned item): `postBet` now short-
   circuits on an already-posted `(provider_id, provider_tx_id)` BEFORE
   the RG/session/balance checks run at all, so a redelivery is correctly
   idempotent even across an intervening RG state change.

Full design, rationale, and the complete specialist-review findings/fixes
list: `docs/decisions/0026-responsible-gaming-player-status-enforcement-
foundation.md`. Updated architecture doc:
`docs/architecture/11-kyc-aml-rg-architecture.md`'s "Implementation
status" section.

### Specialist review: one P0, four P1s found and fixed

An independent 7-specialist parallel review (RG architecture, security,
financial correctness, identity/Person model, PostgreSQL/RLS, API/HTTP,
adversarial testing) - each with live-Postgres empirical verification,
not just static reading - found:

1. **P0 (reported independently by three reviewers)**: the cross-brand/
   cross-tenant self-exclusion PROTECTION is mechanism-correct but
   currently unreachable in production - `internal/identity.
   RegisterPlayer` mints a fresh, unlinked `Person` on every registration,
   with no resolution/dedup logic anywhere in this codebase. A real
   player who self-excludes and re-registers today is NOT blocked.
   **Fixed as an explicit documentation correction and tracked open
   decision** (ADR `0026`'s Context/§9/"Carried-forward limitations"; see
   "Blockers" below) - not a code defect in this stage's own work, but a
   precondition its protective claim depends on that does not exist yet.
2. **P1 - financial correctness**: a bet callback redelivered after
   already succeeding, and after the RG state it depended on later
   changed (e.g. the player self-excluded), could incorrectly report a
   fresh decline instead of its original success - violating the
   documented idempotent-replay contract, empirically reproduced.
   **Fixed**: an early already-posted-transaction short-circuit in
   `postBet`, plus a new regression test and a strengthened concurrency
   assertion.
3. **P1 - RLS leak**: the player-facing "my RG status" endpoint leaked a
   DIFFERENT tenant's confidential restriction `reason_code` to a player
   with accounts at two tenants, empirically reproduced. **Fixed**:
   migration `0038` adds tenant scoping to `player_self_read`.
4. **P1 - defense in depth**: `EvaluateEligibility`'s own SQL relied
   solely on RLS/caller scope, not an explicit predicate, to exclude a
   different tenant's rows. **Fixed**: added an explicit tenant predicate.
5. **P1 - silent-bypass risk**: `EvaluateEligibility` silently skipped
   brand-scoped restrictions if a caller forgot to resolve `BrandID`.
   **Fixed**: `BrandID` is now a required, validated parameter.

Several P2s were fixed (a stale doc comment two reviewers independently
flagged, an RLS UPDATE/DELETE visibility-scope hardening replacing row-
visibility policies with a statement-level trigger, a composite FK
addition) and several more explicitly recorded as accepted/deferred
(full list, with reasoning for each, in ADR `0026`'s own findings
section) - none silently dropped. The adversarial testing review also
found `duration_days` completely untested at every layer, plus several
HTTP-layer authorization/validation gaps (RolePlayer/RoleFinance token
denial, cross-tenant admin read, invalid `scope` rejection) - all closed
with new tests.

### Verification performed

`gofmt -l .` clean. `go build ./...`, `go vet ./...`, `go vet
-tags=integration ./...` clean. `go test ./...`, `go test -tags=integration
./...`, and `go test -race -tags=integration ./...` all pass across the
full repository. Migrations `0037` and `0038` both round-tripped (`up` →
`down` → `up`) cleanly on a database already carrying prior stages' own
test data. Concurrency tests for all three directive-required race
scenarios (self-exclusion during launch, during a bet, and during a
duplicate bet delivery) pass repeatably under `-race` against real
Postgres, with strengthened assertions proving the two concurrent
deliveries' own reported results never disagree with each other.

### Pending (to close out this stage)

- Stage 4D-RG Completion Report delivered to the human, ending with the
  required closing statement. No further stage work begins until
  explicitly authorized.

### Blockers / genuine scope boundaries (not defects)

None block Stage 4D-RG's own approved scope, which is complete. The
following are honestly labeled boundaries and open decisions for future
stages, recorded per CLAUDE.md's "record it as a decision" rule rather
than silently dropped - full detail in ADR `0026`:

- **Cross-brand/cross-tenant self-exclusion evasion via re-registration
  remains open** (the P0 finding above). Closing it requires a genuine
  Person-resolution capability (KYC-driven document/identity matching, or
  some other deliberate cross-brand linkage) - a substantial future body
  of work `docs/architecture/05-identity-architecture.md` already
  anticipated as "Stage 4's KYC-driven hash matching," not a small fix.
  **This is the single most significant residual gap this stage leaves**
  and should weigh heavily on any decision about real-money go-live.
- **No platform-wide, staff-initiated restriction capability exists** -
  `CreateStaffRestriction` only accepts tenant/brand scope, because no
  platform-wide player-lookup capability exists anywhere else in this
  codebase for it to build on (even `RolePlatformAdmin` cannot browse a
  specific tenant's players today - `PermPlayerRead` is itself
  `RequireTenantScope`-gated).
- **No early termination of a self-exclusion is implemented** - most
  jurisdictions treat this as requiring specific legal process this stage
  has no mandate to invent; recorded as an open decision requiring legal
  input before any such endpoint is built.
- **Whether a bring-your-own-licence tenant's self-exclusions should ever
  default to tenant-scoped rather than platform-wide** is an open,
  jurisdiction/licensing-model question this stage does not resolve -
  every tenant defaults to maximal player protection (platform-wide)
  today.
- **KYC/AML, player-level jurisdiction restriction, deposit/loss/
  wagering/session limits, reality checks, and time-outs/cooling-off**
  remain **NOT IMPLEMENTED** - documented extension points only
  (`EvaluateEligibility`'s own shape is designed to grow a KYC/AML check
  as one more step without changing its callers).
- The Stage 3D TOCTOU risk (submit/resolve eligibility window) is carried
  forward unchanged - this stage's implementation did not touch the
  affected withdrawal-approval path.
- Stage 4A's own carried-forward limitations (the payments-side
  unguarded-reversal race, per-tenant provider signing keys, Bonus Engine
  accounting open decisions) are unaffected and remain separately
  tracked - none silently closed.

### Decisions/input still useful from the human before the next stage

1. Approve Stage 4D-RG and authorize the next stage (per CLAUDE.md's
   stage gate - a real casino provider integration, sportsbook, bonus,
   B2C frontend, partner console, production deployment, real PSP, and
   real crypto integrations do not begin automatically).
2. Decide the priority/timeline for closing the cross-brand self-
   exclusion evasion gap (a Person-resolution capability) relative to any
   real-money go-live - this is arguably the single highest-priority open
   RG item on the platform right now.
3. Decide whether/how a self-exclusion may ever be terminated early in
   any jurisdiction this platform will operate in (legal input required,
   not an engineering decision).
4. Decide whether a bring-your-own-licence tenant's self-exclusions
   should ever default to tenant-scoped rather than platform-wide.
5. Decide whether/when a platform-wide staff-initiated restriction
   capability (and the platform-wide player-lookup capability it would
   require) should be built.
6. The already-open, non-blocking items carried forward from Stage 0-4A
   remain open (`docs/decisions/0005`; ADRs 0017/0018's open items;
   `brands`' public-read RLS breadth; the promo_liability/bank-treasury/
   crypto-custodian accounting decisions that still block bonus and
   crypto financial posting specifically; the Stage 3D TOCTOU risk; the
   payments-side unguarded-reversal race; per-tenant provider signing
   keys).
