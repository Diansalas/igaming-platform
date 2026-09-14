# Active Stage

## Stage 4E — Person Resolution & Cross-Brand Identity Foundation — Complete

Status: **Complete, pending human approval to authorize the next stage.**
Issued because Stage 4D-RG was NOT approved for progression: its own
specialist review found a P0 - `internal/identity.RegisterPlayer` created
a brand-new, unlinked `Person` on every registration, so a Person
self-excluded via one brand could register again as a "new" Person at
another brand and evade restriction. Stage 4D-RG's self-exclusion
mechanism (platform-wide `player_restrictions`, `EvaluateEligibility`) was
already correct - the missing capability was reliable Person resolution.
This stage builds exactly that, and only that. No real KYC/AML vendor
integration, no production identity document collection, no biometric
data storage, no document verification, no AML transaction monitoring,
and no jurisdiction-specific identity rule were in scope.

### What was built

1. **`internal/identityresolution` package** - a provider-neutral
   `PersonResolver` interface (`Match`/`NoMatch`/`Uncertain` outcomes,
   plus an `ErrResolverUnavailable` sentinel for a genuinely failed
   resolution attempt) mirroring `CasinoProvider`/`PaymentProvider`'s
   exact shape. `RawClaims` (unverified, client-supplied) is kept
   strictly separate from `VerifiedAttributes` (verified evidence from a
   trusted source) - a resolver must never use the former as a matching
   signal.
2. **`RegisterPlayerWithResolution`** - the ONE registration entry point
   every caller now uses. It calls the resolver BEFORE any Person is
   created, then dispatches: `Match` → link the EXISTING Person
   (`identity.RegisterPlayerLinkedToPerson`, no new Person); `NoMatch` →
   create a brand-new Person, exactly Stage 2's original behavior
   (`identity.RegisterPlayer`); `Uncertain` OR resolver-unavailable OR an
   unrecognized outcome → create a new Person but land the account in a
   safe, non-gambling-capable status (`identity.RegisterPlayerPendingReview`).
   A non-conformant resolver error (not wrapping `ErrResolverUnavailable`)
   fails the whole registration - nothing is created.
3. **No second identity model, one new status, zero new RG code** -
   migration `0039` adds exactly one value, `identity_review_required`,
   to the EXISTING `PlayerAccountStatus` enum.
   `internal/rg.EvaluateEligibility`'s pre-existing `status != active`
   check already denies it - Stage 4D-RG's own RG code needed zero
   changes for this stage's new safe state to actually block gambling.
4. **`MockPersonResolver`** - the only implementation shipped this stage
   (an exact-match lookup table a test configures via
   `SetMatch`/`SetUncertain`/`SetUnavailable`), mirroring
   `MockCasinoProvider`/`MockPaymentProvider`'s identical "magic value"
   testing convention. A real vendor slots in behind the SAME interface
   later without touching any caller.
5. **Fail-closed HTTP wiring** - the register handler now returns 503
   (`apierror.CodeUnavailable`) if `deps.PersonResolver` is nil, rather
   than silently falling back to the old resolution-blind behavior -
   deliberately different from `PaymentOrchestrator`/`CasinoOrchestrator`'s
   existing "nil means feature disabled" convention, since silently
   skipping resolution here is exactly the unsafe fallback this stage
   exists to prevent.
6. **New admin capability** - `POST /v1/admin/players/{id}/identity-
   review/clear`, gated by a new `identity_review:manage` permission
   granted only to `RoleCompliance` (mirroring `rg_restriction:write`'s
   separation-of-duties precedent - never `RoleTenantAdmin`/
   `RolePlatformAdmin`). Uses a single atomic conditional status
   transition (`identity.SetPlayerAccountStatusIfCurrent`, a new function
   this stage added) rather than a read-then-write, closing a TOCTOU the
   security review found in an earlier draft of this handler.
7. **Cross-brand/cross-tenant proof, not just a mechanism claim** -
   unlike Stage 4D-RG (which could only prove its mechanism was correct,
   not that a real registration could ever reach it), this stage's own
   integration test
   (`TestCrossBrandSelfExclusion_ResolvedPersonCannotEvadeViaSecondBrand`)
   proves end to end against real Postgres: register at Brand A (new
   Person), self-exclude via the real player self-service endpoint,
   register again at an entirely different Brand B/Tenant B resolved (via
   a configured Mock `Match`) to the SAME person, and confirm
   `EvaluateEligibility` denies it with `CodeSelfExcluded` specifically -
   using Stage 4D-RG's `EvaluateEligibility` completely unmodified.

Full design, rationale, every recorded open decision, and the complete
specialist-review findings/fixes list:
`docs/decisions/0027-person-resolution-and-cross-brand-identity-
foundation.md`. Updated architecture doc:
`docs/architecture/05-identity-architecture.md`'s "Implementation status
(Stage 4E)" section.

### The honest limit of what this stage closes

Every registration reachable over the live HTTP API resolves `NoMatch`
today, because no trusted source of verified identity evidence
(`VerifiedAttributes`: legal name, DOB, address, phone, government ID
reference, KYC provider reference) is wired into the register handler -
no real KYC/identity-verification vendor is integrated, per this stage's
own explicit non-goal. The cross-brand evasion-prevention mechanism and
its orchestration logic are proven correct at the package/integration-
test level, against a resolver a real future vendor integration will
replace - they are **not yet actively preventing evasion for a real
player** over the live API, because no source of verified evidence exists
yet to feed the resolver. This is the single most important thing to
understand about this stage's actual, current effect, and it is stated
plainly rather than buried, per CLAUDE.md's "no fake completion" rule.

### Specialist review: zero P0s, four P1s found and fixed

An independent 7-specialist parallel review (identity architecture,
security/privacy, RG/self-exclusion integration, PostgreSQL/RLS, multi-
tenancy, API/HTTP, adversarial testing) - each reviewing live code, not
just design - found:

1. **P1 - TOCTOU in the identity-review-clear handler**: a read-then-write
   could be raced by a concurrent status change (e.g. a staff suspend),
   silently clobbering it and reactivating a since-suspended account.
   **Fixed**: `identity.SetPlayerAccountStatusIfCurrent` - a single atomic
   `UPDATE ... WHERE status = identity_review_required` - replacing the
   read-then-write, plus a regression test proving a concurrent suspend
   always wins.
2. **P1 - inaccurate documentation**: this stage's own ADR and a code
   comment incorrectly claimed a review-required account "can log in" -
   false; the existing login handler denies it exactly like a suspended
   account. **Fixed**: corrected in both places, including the OPEN
   DECISION rationale that had relied on the same false premise.
3. **P1 - a test overstated its own proof**: the concurrency test's
   name/framing implied it closed the hardest possible race (two
   simultaneous first-time registrations for one real, still-unregistered
   person); it actually proves the narrower, still-valuable guarantee
   that concurrent registrations resolving to an ALREADY-EXISTING person
   never duplicate it. **Fixed**: rewrote the test's own doc comment to
   state precisely what is and is not proven, and why the harder case is
   not reachable by any resolver this stage ships.
4. **P1 - missing test coverage**: no test exercised idempotency through
   the ACTUAL dispatch path (`RegisterPlayerWithResolution`, not the
   older `RegisterPlayer` called directly), and no test proved cross-
   tenant identity-review-clearing is denied. **Fixed**: three new tests
   (duplicate-email through the dispatch path, genuine concurrent
   duplicate-email registration proving exactly one succeeds, cross-
   tenant clearing denial returning 404).

Two further P2s were fixed (resolver-failure audit records were
hardcoded to `OutcomeSuccess`, hiding outages from an audit scan for
failures; the resolution audit record carried no `RequestID`/
`IPAddress`/`UserAgent` unlike every sibling audit record in this
codebase). Several more P2s were explicitly recorded as accepted/
deferred with reasoning, none silently dropped - full list in ADR
`0027`'s own findings section, including: a benign orphan-Person row from
a losing registration race (inherited unchanged from Stage 2, not a
regression); the resolver's `Raw`/`Reason` fields being trusted by doc
comment rather than enforced in code (no real resolver exists yet to
misuse them); a false-positive Match's trust boundary; migration lock/
down-migration one-way-door notes; a pre-existing ADR-0026 cross-tenant
restriction-metadata visibility design now reachable in practice for the
first time; no per-tenant opt-in/opt-out for resolution (a hybrid-
licensing open question); and CI's `go test` steps not passing `-race`.

### Newly discovered during this stage's validation, NOT introduced by it

`internal/casino`'s pre-existing `TestConcurrent_
DuplicateBetDeliveryDuringSelfExclusion` (Stage 4D-RG) is intermittently
flaky under `go test -race -tags=integration` - observed failing roughly
1 run in 3 in this session, reporting the two concurrent bet-delivery
goroutines disagreeing about whether a bet posted while self-exclusion
was concurrently being applied. No file this stage touches is on that
code path - confirmed via repeated isolated re-runs. This is a genuine,
pre-existing race in the casino bet-delivery/self-exclusion interaction
requiring `casino`/`ledger-finance`/`identity-compliance` specialist
attention in a future stage; it was NOT fixed here (out of this
identity-resolution stage's scope) and is NOT silently closed - see
"Blockers" below.

### Verification performed

`gofmt -l .` clean. `go build ./...` clean. `go vet -tags=integration
./...` clean. `go test ./...`, `go test -tags=integration ./...`, and
`go test -race ./...` all pass cleanly across the full repository. `go
test -race -tags=integration ./...` passes except for the pre-existing,
intermittently flaky casino test noted above. Migration `0039`
round-tripped (`up` → `down` → `up`) cleanly before this stage's own
tests populated the new status value (the down migration is a documented,
intentional one-way door once any row uses `identity_review_required` -
verified live against the dev database).

### Pending (to close out this stage)

- Stage 4E Completion Report delivered to the human, ending with the
  required closing statement. No further stage work begins until
  explicitly authorized.

### Blockers / genuine scope boundaries (not defects)

None block Stage 4E's own approved scope, which is complete. The
following are honestly labeled boundaries and open decisions for future
stages, per CLAUDE.md's "record it as a decision" rule - full detail in
ADR `0027`:

- **Cross-brand self-exclusion evasion prevention is not yet ACTIVE for a
  real player** (see "The honest limit" above) - closing this fully
  requires a real, contracted identity-verification/KYC vendor to
  populate `VerifiedAttributes`, which is explicitly out of this stage's
  scope. This remains the single most significant residual gap and
  should weigh heavily on any decision about real-money go-live, exactly
  as Stage 4D-RG's own equivalent note said.
- **No automatic reconciliation of pre-existing duplicate Persons** -
  directive §14 explicitly forbids heuristic auto-merging; any future
  cleanup of historical duplicates is a controlled, KYC-backed project,
  not an automatic batch job.
- **No per-tenant opt-in/opt-out for Person resolution** - resolution is
  unconditionally platform-wide today (a single resolver instance). A
  bring-your-own-licence tenant under a different jurisdiction may
  eventually need this as a legal/data-controller concern; `ResolutionInput`
  already carries the fields a future policy would key on, but nothing
  consumes them yet.
- **Newly discovered casino concurrency flake** (see above) - carried
  forward, unresolved, not silently closed.
- The Stage 3D TOCTOU risk (withdrawal submit/resolve eligibility
  window), the payments-side unguarded-reversal race, casino per-tenant
  provider signing keys, the casino real-provider API documentation
  requirement, and Bonus Engine accounting open decisions are all
  unaffected by this stage and remain separately tracked - none silently
  closed.
- Every Stage 4D-RG carried-forward limitation not specifically about
  Person duplication (no platform-wide staff-initiated restriction
  capability; no early termination of self-exclusion; whether a bring-
  your-own-licence tenant's self-exclusions should ever default to
  tenant-scoped; KYC/AML, player-level jurisdiction restriction, deposit/
  loss/wagering/session limits, reality checks, time-outs/cooling-off)
  remain **NOT IMPLEMENTED**, unchanged, and not silently closed.

### Decisions/input still useful from the human before the next stage

1. Approve Stage 4E and authorize the next stage (per CLAUDE.md's stage
   gate).
2. Decide the priority/timeline for contracting a real identity-
   verification/KYC vendor to actually activate cross-brand evasion
   prevention for real players - this is arguably the single highest-
   priority open item on the platform right now, directly inherited from
   Stage 4D-RG's own equivalent item.
3. Decide whether a bring-your-own-licence tenant should be able to
   opt out of platform-wide Person resolution for data-controller/legal
   reasons (ADR `0027`'s own recorded open question).
4. Prioritize the newly discovered casino bet-delivery/self-exclusion
   concurrency flake for a future stage's specialist attention.
5. The already-open, non-blocking items carried forward from Stage
   0-4D-RG remain open (`docs/decisions/0005`; ADRs 0017/0018's open
   items; `brands`' public-read RLS breadth; the promo_liability/bank-
   treasury/crypto-custodian accounting decisions; the Stage 3D TOCTOU
   risk; the payments-side unguarded-reversal race; per-tenant provider
   signing keys; every Stage 4D-RG item not specifically about Person
   duplication, listed above).
