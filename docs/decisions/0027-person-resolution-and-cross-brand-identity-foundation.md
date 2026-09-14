# ADR 0027 — Person Resolution & Cross-Brand Identity Foundation

Status: Accepted. Issued as "STAGE 4E — PERSON RESOLUTION & CROSS-BRAND
IDENTITY FOUNDATION", directly following the human's own review of Stage
4D-RG, which found the stage NOT approved for progression on a P0: Stage
4D-RG's own specialist review had already identified that
`internal/identity.RegisterPlayer` created a brand-new, unlinked `Person`
on EVERY registration, with no capability to recognize that two
registrations correspond to the same real person. This meant a Person
self-excluded via one brand could register again at a second brand,
receive a second `Person` row, and gamble - the platform-wide
`player_restrictions` mechanism ADR 0026 built was structurally correct
but had nothing to key its cross-brand lookup on in practice. This stage
closes exactly that gap. Owner: `identity-compliance`, with `architect` on
the registration-flow composition, `security`/`ledger-finance` on
concurrency/idempotency, and `code-reviewer`/`qa` on adversarial
verification.

This stage does **not** implement real KYC/AML. No real identity-
verification vendor is integrated, no production identity documents are
collected, no biometric data is stored, no AML transaction monitoring is
added, and no jurisdiction-specific identity rule is implemented. It
builds the PLATFORM IDENTITY-RESOLUTION BOUNDARY only - the interface and
registration-flow shape a real vendor slots into later without a rewrite.

No sportsbook or Bonus Engine work is introduced or touched. Stage 3D's
withdrawal submit/resolve TOCTOU, the payments-side unguarded-reversal
race, casino per-tenant provider signing keys, and Stage 4D-RG's own
carried-forward limitations are unaffected and remain separately tracked
(see this ADR's own "Carried-forward limitations" section).

## Context

`internal/identity.RegisterPlayer` (Stage 2) always called `CreatePerson`
unconditionally, then created a `PlayerAccount` under that brand-new
Person. There was no code path anywhere that could recognize "this
registration is the same real person as an existing Person row." ADR
0026's `player_restrictions` table and `EvaluateEligibility` function are
keyed on `person_id` and are genuinely platform-wide - the mechanism was
never the problem. The problem was that nothing in the registration flow
could ever produce the SAME `person_id` twice for the same real person, so
the cross-brand check always had, in practice, exactly one restricted
Person and zero ways for a second registration to ever resolve to it.

## Decisions

### 1. No second identity model - one new package, composed with the existing one

`internal/identityresolution` is a new package sitting strictly BEFORE
`internal/identity.RegisterPlayer*` in the registration flow. It does not
duplicate `Person`/`PlayerAccount`/`Brand`/`Tenant`, does not add a new
identity table, and does not touch `player_restrictions` or
`EvaluateIligibility`'s own logic. It answers exactly one question -
"does this registration correspond to an existing Person" - and hands the
answer to `internal/identity`, which already knows how to create a Person,
create a PlayerAccount under an existing Person, or land a PlayerAccount
in a safe non-gambling status.

### 2. `PersonResolver` — a provider-neutral interface, four distinct outcomes

```go
type Outcome string
const (
    Match      Outcome = "match"       // an existing Person was found - MatchedPersonID is set
    NoMatch    Outcome = "no_match"    // no existing Person - a NEW Person is the correct outcome
    Uncertain  Outcome = "uncertain"   // resolution ran but could not confidently decide - NOT an error
)
var ErrResolverUnavailable = errors.New(...) // resolution could not run at all

type PersonResolver interface {
    Resolve(ctx context.Context, input ResolutionInput) (ResolutionResult, error)
}
```

Mirrors `CasinoProvider`/`PaymentProvider`'s exact shape (ADR 0025/0022):
a real vendor implements this same interface later; no caller changes.
The four outcomes are deliberately never collapsed - directive's own
explicit requirement, and the whole point of this stage: an `Uncertain`
result forced into `NoMatch` would recreate the P0 this stage exists to
close, and a resolver outage silently treated as `NoMatch` would too.

`MockPersonResolver` is the only implementation this stage ships - an
exact-match lookup table a test configures via `SetMatch`/`SetUncertain`/
`SetUnavailable`, mirroring `MockCasinoProvider`'s identical "magic value"
testing convention. Its untouched default (no configuration) resolves
`NoMatch` for every input carrying no verified attributes - the honest,
correct behavior for every registration reachable over HTTP today (see
Decision 10).

### 3. Registration flow: resolve BEFORE creating a Person

```
Registration request
  -> identityresolution.RegisterPlayerWithResolution
       -> resolver.Resolve(...)
            Match      -> identity.RegisterPlayerLinkedToPerson (existing Person, NO new Person)
            NoMatch    -> identity.RegisterPlayer                (new Person - Stage 2's original behavior)
            Uncertain  -> identity.RegisterPlayerPendingReview    (new Person, but status = identity_review_required)
            error wrapping ErrResolverUnavailable
                       -> identity.RegisterPlayerPendingReview    (SAME safe path as Uncertain)
            any other error (non-conformant resolver)
                       -> registration FAILS, nothing is created
```

All three `identity.RegisterPlayer*` functions run inside the SAME
transaction `RegisterPlayerWithResolution` was called with (mirroring
`RegisterPlayer`'s own pre-existing contract), so a resolution outcome, its
audit record, and the resulting Person/PlayerAccount rows commit or roll
back as one atomic unit - never a resolution decision that succeeded
while the account creation it justified silently failed or vice versa.

`resolver.Resolve` is called INSIDE the enclosing `pgx.Tx`, extending the
SAME precedent `internal/casino.LaunchGame` already established for
calling an external provider from inside a transaction (ADR 0025). For
`MockPersonResolver` this is free; a real future vendor integration should
revisit whether holding a DB transaction open across a network call
remains acceptable once real network latency and failure modes are in
play - recorded as an OPEN CONSIDERATION below, mirroring the identical
open note Stage 4D-RG's own specialist review left for `LaunchGame`
holding a person advisory lock across `provider.Launch`.

### 4. Match semantics

`Match` means the resolver has positively identified an existing Person
this registration belongs to (`MatchedPersonID` is non-nil). The
registration NEVER calls `CreatePerson` on this path -
`RegisterPlayerLinkedToPerson` inserts a new `PlayerAccount` row (a new
brand relationship) pointed at the EXISTING `person_id`. This is precisely
what closes the P0: the person self-excluded on Brand A keeps the SAME
`person_id` when they register at Brand B, so
`internal/rg.EvaluateEligibility`'s existing `person_id`-keyed restriction
lookup sees them correctly with zero changes to that function.

### 5. Uncertain semantics — a safe state, not an error

`Uncertain` is a SUCCESSFUL resolution reporting genuine ambiguity (e.g. a
real future vendor's low-confidence fuzzy match) - not a failure. Forcing
it into `Match` risks linking an unrelated person's account to someone
else's restriction/history (a privacy and correctness violation in the
other direction); forcing it into `NoMatch` risks exactly the evasion this
stage exists to prevent. The safe answer is a THIRD outcome: a new Person
is created (there is no candidate to link to), but the resulting
`PlayerAccount` lands in a new status, `identity_review_required`
(migration 0039), rather than `pending_verification`. The account exists
but CANNOT log in again (the existing login handler denies any status
other than `active`/`pending_verification`, exactly like `suspended`/
`self_excluded`/`closed` - an earlier draft of this ADR and of
`player_account.go`'s own doc comment incorrectly stated this account
"can log in," corrected during this stage's own security review) and is
not gambling-capable, until staff clears the review.

### 6. Provider-unavailable semantics — treated identically to Uncertain

`ErrResolverUnavailable` is NEVER silently treated as `NoMatch` - that
would be exactly "silently create a second Person because the resolution
service was temporarily unavailable," which the directive explicitly
forbids. It is treated IDENTICALLY to `Uncertain`: a new Person is
created, but the account starts in `identity_review_required`, never
`active`. A resolver-unavailable registration can therefore never become
gambling-capable without a staff decision, exactly like a genuinely
uncertain one.

**OPEN DECISION (recorded for human/product input, not resolved here):** a
stricter interpretation of the directive would hard-block registration
entirely when the resolver is unavailable, rather than allowing a
restricted account to be created. This stage deliberately chose the softer
behavior - the registration itself still succeeds (the player gets back a
working token pair, per Decision 6's own semantics), and staff can review
and clear the account once the review-required status is lifted -
reasoning that "never silently create an unrestricted Person" is satisfied
by the review-required status without also outright refusing a legitimate
player's registration attempt during a vendor outage. **Correction from an
earlier draft of this ADR (security specialist review, Stage 4E):** the
account created this way does NOT let the player log in again before
staff clears it - the existing login handler denies any status other than
`active`/`pending_verification`. The only practical effect of the softer
choice over a hard block, given that, is that the initial registration
response still succeeds and the Person/PlayerAccount rows exist for staff
to review, rather than the request being rejected outright; it does not
grant the player any interim access. If product/compliance prefers the
stricter hard-block regardless, that is a
small, isolated change to `RegisterPlayerWithResolution`'s
`ErrResolverUnavailable` branch.

### 7. Existing Person match is never silently forced into an unrecognized outcome

`RegisterPlayerWithResolution` fails closed on TWO additional cases beyond
the three documented outcomes: (a) a resolver returning an error that does
NOT wrap `ErrResolverUnavailable` is treated as a genuine bug in that
resolver implementation and FAILS the whole registration (nothing is
created) rather than guessing which safe path applies; (b) a resolver
returning an `Outcome` value outside `{Match, NoMatch, Uncertain}` is
treated identically to `Uncertain` (safe, review-required), never
`NoMatch`. Both are defense against a future, buggy real vendor adapter,
not reachable by `MockPersonResolver` today.

### 8. Verified attributes vs. raw claims - and why every HTTP registration resolves `NoMatch` today

```go
type RawClaims struct { Email, BrandSlug string }               // unverified, client-supplied, NEVER a matching signal
type VerifiedAttributes struct {                                  // evidence from a TRUSTED source only
    LegalName, DateOfBirth, Address, Phone, Email,
    GovernmentIDReference, KYCProviderReference string
}
```

`RawClaims` exists purely for audit/context. A real `PersonResolver` must
never use it to decide Match/NoMatch/Uncertain - matching on a single weak,
unverified attribute like a self-reported email would be worse than no
matching at all (confidently wrong, not merely absent). `VerifiedAttributes`
is deliberately never populated from a bare HTTP registration request body
today, because no trusted verification source (KYC vendor, document
check, liveness) is integrated yet. This means the HTTP
`POST /v1/auth/register` endpoint's registrations honestly and correctly
resolve `NoMatch` today, every time - this is NOT a gap in this stage's
own work, it is the truthful state of a platform with no real KYC pipeline
yet. The valuable, tested capability this stage ships is the orchestration
logic and interface itself (proven via directly-constructed
`ResolutionInput`s in `internal/identityresolution`'s own test suite,
bypassing HTTP exactly where a future vendor's `VerifiedAttributes` would
plug in), not a claim that cross-brand resolution is active in production
today.

No single field is ever assumed to uniquely identify a Person - a future
real resolver decides its own matching confidence internally; this
package only carries whatever evidence it is given.

### 9. Privacy and data minimization

No new table stores verified identity attributes or resolution events.
`VerifiedAttributes` exists only as an in-memory parameter passed to
`resolver.Resolve` for the duration of one request - it is never persisted
by this package. The only persistent trace of a resolution is one
`audit_log` row per attempt (`identity_resolution.performed`), carrying:
the outcome, the resolver's own short machine-readable `Reason` string
(which must never itself contain identity evidence - a resolver
implementation's own responsibility, documented on `ResolutionResult`),
and, for `Match`, the matched `person_id` (an opaque identifier, never
evidence). No verified attribute value (legal name, DOB, address, phone,
government ID reference) is ever written to a log or audit record.

**OPEN CONSIDERATION:** if a real KYC/identity-resolution vendor is
integrated in a future stage, a dedicated `identity_resolution_events`
table (with its own encryption-at-rest and access-control requirements)
may become warranted once there is real evidence worth retaining
structurally. Building it now, with no real data to put in it, would be
exactly the speculative, untested-by-any-real-path code CLAUDE.md's "no
fake completion" rule warns against.

### 10. Cross-tenant / cross-brand model — identity resolution ≠ tenant data access

`Person` remains platform-wide (Stage 2's own model, unchanged); `Tenant`/
`Brand`/`PlayerAccount` remain tenant-scoped, exactly as before. This
stage does NOT grant any new cross-tenant READ capability - a tenant's
compliance staff still cannot see another tenant's `PlayerAccount` rows,
`player_restrictions` rows they don't already have visibility into (ADR
0026's own RLS), or any verified identity attribute. What crosses tenant
boundaries is exactly one opaque fact per registration: "this new
registration corresponds to Person X" (or does not) - resolved entirely
server-side, by a resolver that is never handed a tenant-scoped database
connection, only the `ResolutionInput` value. `RegisterPlayerWithResolution`
itself still runs inside the CALLER's own tenant-scoped `pgx.Tx`
(`db.WithTenant`) - it never opens a second, differently-scoped
connection. Identity resolution answering "is this the same Person" is a
platform-level fact; it is not, and must never become, a tenant data
access grant.

### 11. RG integration — zero duplicated logic

`internal/rg.EvaluateEligibility` is UNCHANGED by this stage. It already
denies any `PlayerAccount.Status != active`
(`CodePlayerAccountNotActive`) and already checks `player_restrictions`
keyed on `person_id` regardless of which brand/tenant created the
restriction. Landing a Person-resolution-uncertain account in
`identity_review_required` (a new value added to the EXISTING
`PlayerAccountStatus` enum, migration 0039) is therefore enforced by that
SAME pre-existing check with zero new RG code path - directly satisfying
the directive's explicit "do not duplicate self-exclusion logic"
requirement. `internal/identityresolution/register_integration_test.go`'s
`TestCrossBrandSelfExclusion_ResolvedPersonCannotEvadeViaSecondBrand`
proves, end to end against real Postgres, that a Person self-excluded via
a Brand A account, once correctly resolved (via a configured Mock
`Match`) to the same person on registering at an entirely different
Brand B/Tenant B, is denied by `EvaluateEligibility` with
`CodeSelfExcluded` - not merely `CodePlayerAccountNotActive` - proving the
restriction itself, not just account status, is what stops them.

### 12. Existing-data migration strategy — no automatic merging

This stage does NOT retroactively examine or merge any Person created
before it shipped. There is no safe, automated way to determine that two
PRE-EXISTING Person rows are the same real person from the data this
platform holds today (no verified attributes were ever collected). Per
the directive's own explicit instruction, no heuristic merge (matching on
email, name similarity, or any other weak signal) is implemented. Existing
Persons remain exactly as they are; a NEW registration is the only thing
this stage's resolution boundary touches. Reconciling historical
duplicate Persons - if and when that is ever warranted - is documented
here as a FUTURE, CONTROLLED, KYC-backed identity-reconciliation project,
never an automatic batch job, since an incorrect automated merge could
itself violate a real player's data segregation or wrongly apply one
person's restriction to another.

### 13. Concurrency — two simultaneous registrations resolving to the same Person

Because the `Match` path never calls `CreatePerson` (Decision 4), the
"two concurrent registrations for the same identity" race has no window
in which a NEW Person could be created twice: both concurrent
transactions resolve to the SAME, already-existing `person_id` (supplied
by the resolver, itself not created by either registration), and each
inserts its own new `PlayerAccount` row pointed at it - `player_accounts`
has no unique constraint that would make two such concurrent inserts
conflict with each other, and none is needed, since correctness here does
not depend on serializing them: whichever order they commit in, exactly
one Person exists throughout and both accounts end up correctly linked to
it.
`internal/identityresolution/register_integration_test.go`'s
`TestRegisterPlayerWithResolution_ConcurrentMatchingRegistrations_NeverCreateDuplicatePerson`
proves this directly against real Postgres with 10 concurrent goroutines,
each verifying (a) every resulting account references the single
pre-existing `person_id` and (b) the `persons` table still contains
exactly one row for it afterward.

The genuinely hard version of this race - two concurrent registrations
BOTH carrying the SAME real-world identity where NEITHER yet has an
existing Person row to resolve to, and a real resolver would need to
decide, mid-flight, "are these the same new person" - is not addressed by
this stage, because no real resolver (only the exact-match `MockPersonResolver`)
exists yet to have that race in the first place. This is recorded as an
OPEN CONSIDERATION for whatever future real vendor integration
introduces genuine first-time-matching logic: such an integration will
need its own concurrency story (e.g. a vendor-side dedup key, or an
application-level advisory lock keyed on the verified evidence itself,
mirroring `internal/rg.lockPerson`'s own pattern) before two simultaneous
first-time registrations for the same real, still-unregistered person
can be guaranteed to converge on one Person.

### 14. Idempotency

Registration's existing idempotency boundary is UNCHANGED: `player_accounts`'
`UNIQUE(brand_id, email)` constraint (Stage 2) is what makes a repeated
registration request for the same brand/email idempotent AT THE
PLAYERACCOUNT level - a retried request hits `identity.ErrEmailTaken`
exactly as before, regardless of which of the three
`identity.RegisterPlayer*` paths would have been chosen. This stage adds
no new idempotency key and needs none: a resolution decision itself has
no independent persistent identity to deduplicate (only its downstream
account-creation effect does, and that effect is exactly what the existing
constraint already guards).

### 15. Staff/admin access — `PermIdentityReviewManage`

Clearing a `PlayerAccount` out of `identity_review_required` is gated by a
NEW, dedicated permission, `identity_review:manage`
(`internal/auth/permission.go`), granted ONLY to `RoleCompliance` -
mirroring `PermRGRestrictionWrite`'s exact separation-of-duties precedent
(ADR 0026 §12): a broad tenant administrator (`RoleTenantAdmin`) does NOT
get it automatically for holding `PermPlayerSuspend`/`PermStaffManage`,
and `RolePlatformAdmin` does not get it either (it has no path to resolve
a tenant-scoped `PlayerAccount` at all - identical reasoning to
`PermRGRestrictionWrite`'s own). The handler
(`newClearIdentityReviewHandler`) additionally verifies the account's
CURRENT status is genuinely `identity_review_required` before
transitioning it to `active` - without this check the same endpoint could
be misused to reactivate a suspended or self-excluded account, which would
be a strictly LARGER capability than `PermPlayerSuspend` already grants
and must never be reachable through this narrower permission.

### 16. Database / RLS

Migration `0039` widens the EXISTING `player_accounts_status_check` CHECK
constraint by exactly one value (`identity_review_required`) - it does not
add a new table, so no new RLS policy is required: `player_accounts`' own
pre-existing tenant-isolation RLS (migration 0010/0037) already covers
every status value, including the new one, and `audit_log`'s own
pre-existing dual-scope RLS already covers the new `identity_resolution.
performed`/`identity_review.cleared` audit actions. No platform-wide data
became newly tenant-readable, and no tenant-owned data became newly
platform-readable, by this migration.

### 17. Audit trail

Every `RegisterPlayerWithResolution` call writes exactly one
`identity_resolution.performed` audit record (action outcome:
`match` / `no_match` / `uncertain` / `resolver_unavailable` /
`unrecognized_outcome`), in the SAME transaction as the resulting
Person/PlayerAccount creation. Clearing a review writes a separate
`identity_review.cleared` record (actor, reason code, previous status).
Neither ever logs a raw verified-attribute value (Decision 9).

## Carried-forward limitations (not resolved by this stage)

- Stage 3D withdrawal submit/resolve TOCTOU (`docs/decisions/0024`) -
  unaffected, unresolved, still tracked there.
- Payments-side unguarded-reversal race (Stage 4A's own carried-forward
  finding) - unaffected, unresolved.
- Casino per-tenant provider signing keys, and the real-provider API
  documentation requirement (Stage 4A) - unaffected, unresolved.
- Bonus Engine accounting decisions - not started, unaffected.
- **Forward note for future gambling/money endpoints** (security review):
  `identity_review_required` is enforced ONLY via
  `internal/rg.EvaluateEligibility`, whose only non-test caller today is
  `internal/casino`'s launch/bet path. Nothing is exploitable today (no
  other player-money HTTP route exists yet), but any FUTURE endpoint that
  can move money or let a player gamble (a sportsbook bet, a future
  casino financial operation, a player-initiated withdrawal) MUST call
  `EvaluateEligibility` before proceeding - it must never be assumed
  covered by virtue of checking `PlayerAccount.Status` some other way.
- Stage 4D-RG's own residual/deferred scope (deposit/loss/wagering/session
  limits, reality checks, time-outs/cooling-off, jurisdiction-at-player-level,
  AML/KYC restriction as a distinct signal) - unaffected, unresolved.
- **This stage's own honest limitation, restated plainly:** no real
  identity-verification vendor is integrated. Every registration reachable
  over the platform's live HTTP API resolves `NoMatch` today (Decision 8) -
  the cross-brand self-exclusion evasion this stage's mechanism prevents is
  proven correct at the package level (Decision 11's integration test)
  against a resolver a real vendor integration will replace, but is NOT
  yet actively preventing evasion for a real player over the live API,
  because no source of verified identity evidence exists yet to feed it.
  Closing that final gap is future, provider-dependent work explicitly out
  of this stage's scope (§3).
- **Newly discovered during this stage's validation gate, NOT introduced
  by it:** `internal/casino`'s pre-existing
  `TestConcurrent_DuplicateBetDeliveryDuringSelfExclusion`
  (`internal/casino/rg_enforcement_integration_test.go`, Stage 4D-RG) is
  intermittently flaky under `go test -race -tags=integration` - observed
  failing roughly 1 run in 3 in this session, reporting the two concurrent
  bet-delivery goroutines disagreeing about whether the bet posted while
  self-exclusion was concurrently being applied (one reports
  `declined/self_excluded`, the other `succeeded`, while the wallet balance
  agrees with the `succeeded` result). This is a genuine, pre-existing race
  in the casino bet-delivery/self-exclusion interaction, not a Stage 4E
  regression - no file this stage touches is on that code path. It is
  recorded here, unresolved, as a carried-forward risk requiring
  `casino`/`ledger-finance`/`identity-compliance` specialist attention in a
  future stage; fixing it was out of scope for this identity-resolution
  stage.

## Specialist review findings and fixes

An independent 7-specialist parallel review (identity architecture,
security/privacy, RG/self-exclusion integration, PostgreSQL/RLS, multi-
tenancy, API/HTTP, adversarial testing) found **zero P0s** - no reviewer
found a way to actually bypass the platform-wide restriction mechanism
through code introduced this stage. It found **four P1s**, all fixed
before this stage was considered complete, plus several P2s recorded as
accepted/deferred rather than silently dropped.

### P1s found and fixed

1. **TOCTOU in the identity-review-clear admin handler** (security
   review). The original `newClearIdentityReviewHandler` read the
   account's status, then wrote a new one in a separate statement - a
   concurrent status change (e.g. a staff `suspend` racing this exact
   request) between the read and the write could be silently clobbered,
   reactivating an account that had just been suspended or self-excluded
   by someone else. **Fixed**: added
   `identity.SetPlayerAccountStatusIfCurrent` - a single atomic
   `UPDATE ... WHERE status = identity_review_required` - replacing the
   read-then-write. Regression test:
   `TestClearIdentityReview_ConcurrentWithSuspend_NeverReactivatesASuspendedAccount`.
2. **Inaccurate "can log in" claim** (security review). An earlier draft
   of this ADR and of `player_account.go`'s own doc comment stated a
   review-required account "exists and can log in" - false: the existing
   login handler denies any status other than `active`/
   `pending_verification`, identically to `suspended`/`self_excluded`/
   `closed`. **Fixed**: corrected both doc comments (Decision 5/6 above)
   and the OPEN DECISION rationale in Decision 6, which had relied on the
   same false premise.
3. **Concurrency test overstated its own claim** (RG integration review).
   `TestRegisterPlayerWithResolution_ConcurrentMatchingRegistrations_
   NeverCreateDuplicatePerson`'s name/framing implied it closed the hard
   "two simultaneous first-time registrations for the same real person"
   race; it actually only proves the (still valuable, but narrower)
   guarantee that concurrent registrations resolving to an
   ALREADY-EXISTING person_id never duplicate it. **Fixed**: rewrote the
   test's own doc comment to state precisely what is and is not proven,
   and cross-referenced Decision 13's own "OPEN CONSIDERATION" for the
   harder case (not reachable by any resolver this stage ships, so not a
   deferred bug - a real future resolver integration's own responsibility).
4. **No test for idempotency through the actual dispatch path, or for
   cross-tenant identity-review-clearing** (adversarial testing and
   multi-tenancy reviews, independently). The pre-existing
   `TestRegisterPlayer_DuplicateEmailSameBrandRejected` calls
   `identity.RegisterPlayer` directly, never exercising
   `RegisterPlayerWithResolution`'s own dispatch logic; and no test proved
   a compliance principal from one tenant is denied clearing another
   tenant's review-required account. **Fixed**: added
   `TestRegisterPlayerWithResolution_DuplicateEmailSameBrandRejected`
   (through the Uncertain/pending-review path specifically),
   `TestRegisterPlayerWithResolution_ConcurrentDuplicateEmail_
   ExactlyOneSucceeds` (genuine concurrency, not just sequential retry),
   and `TestClearIdentityReview_CrossTenantClearingRejected`.

### P2s fixed

- Audit records for `resolver_unavailable`/`unrecognized_outcome` were
  hardcoded to `OutcomeSuccess`, hiding every resolver failure from an
  audit scan for failures. **Fixed**: both now record `OutcomeFailure`.
- The `identity_resolution.performed` audit record carried no
  `RequestID`/`IPAddress`/`UserAgent`, unlike every sibling audit record
  in this codebase, making it joinable to the paired `player.registered`
  record only by timestamp. **Fixed**: `RegisterPlayerWithResolutionParams`
  now carries these optional fields, populated by the HTTP handler.

### P2s recorded as accepted/deferred, not fixed

- **A losing registration in a concurrent duplicate-email race leaves
  behind one extra, permanently unlinked (but harmless - no restrictions,
  no account, no data) `Person` row**, because `identity.CreatePerson`
  runs before the `UNIQUE(brand_id, email)` check. This is inherited
  unchanged from Stage 2's original `RegisterPlayer` (not a Stage 4E
  regression) and is disclosed, not silently fixed - closing it would
  require moving Person creation inside the same constraint check as the
  account insert, out of this stage's scope. Proven and documented by
  `TestRegisterPlayerWithResolution_ConcurrentDuplicateEmail_ExactlyOneSucceeds`.
- **`ResolutionInput.Raw` (client-supplied email/brand slug) is passed to
  every resolver, with only a doc comment forbidding its use as a
  matching signal** (security review) - a future real resolver
  implementation could still misuse it. Not restructured this stage (no
  real resolver exists yet to misuse it); recorded as a contract a real
  vendor integration's own code review must enforce.
- **The resolver's own `Reason` string is audited verbatim, with only a
  doc comment requiring it stay non-sensitive** (security review) - a
  real future vendor returning something like `"dob_mismatch: 1984-02-11"`
  would put PII into the append-only audit log. `MockPersonResolver`'s
  own `Reason` values are all safe today; recorded as a hard requirement
  a real resolver integration must sanitize/allowlist before shipping,
  not merely document.
- **A false-positive `Match` is trusted with no independent verification
  beyond `MatchedPersonID != uuid.Nil`** (multi-tenancy review) - the
  resolver is now effectively a trusted component with Person-linking
  authority. Unreachable today (`MockPersonResolver` only returns a
  test-configured value); recorded as an invariant a real resolver
  integration's own review must hold to.
- **Migration 0039's CHECK-constraint widening takes an `ACCESS
  EXCLUSIVE` lock and full table scan** (PostgreSQL/RLS review) - a
  pre-existing pattern in this codebase (migration 0035 does the same),
  not a 0039 regression; will need `NOT VALID`/`VALIDATE CONSTRAINT`
  splitting at production table size.
- **The down migration for 0039 is a one-way door once any row uses the
  new status** (PostgreSQL/RLS review) - already documented as intentional
  in the down migration's own comment; noted here again for visibility.
- **Cross-tenant restriction-metadata visibility is a pre-existing ADR
  0026 design, not a new leak, but Stage 4E is what makes it reachable in
  practice** (multi-tenancy review): once a Match links a second brand's
  account to an existing Person, that tenant's staff can see (via the
  EXISTING `rg_restriction:read` endpoint) that Person's platform-wide
  restriction rows, including their `reason_code`, `source`, and
  `created_at` - by ADR 0026's own explicit design (a platform-wide
  restriction is deliberately visible to every tenant a Person interacts
  with). Recorded here as a sharper statement of Decision 10's own
  "identity resolution ≠ tenant data access" claim: no new tenant DATA
  access is granted, but a pre-existing design's cross-tenant
  RESTRICTION-METADATA visibility becomes reachable for the first time
  once resolution actually links two tenants' accounts to one Person -
  worth revisiting (e.g. whether `reason_code` should ever be withheld
  cross-tenant) if this becomes a real product concern.
- **No per-tenant opt-in/opt-out for Person resolution** (multi-tenancy
  review) - resolution is unconditionally platform-wide
  (`cmd/platform-api/main.go`'s single resolver instance). Sound for this
  stage (no real evidence is ever collected or shared today), but a
  bring-your-own-licence tenant under a different jurisdiction (ADR 0006)
  may be a separate data controller that cannot legally share registrants
  against a platform-wide identity graph. `ResolutionInput` already
  carries `TenantID`/`BrandID` for a future per-tenant policy to key on;
  nothing consumes it today. Recorded as an open question for human/
  product/legal input before any real vendor is wired - not built
  speculatively now.
- **`go test ./...`/`-tags=integration` in `.github/workflows/ci.yml` does
  not pass `-race`** (adversarial testing review) - this session's own
  validation gate runs `-race -tags=integration` manually (see
  "Verification performed" in `docs/progress.md`'s Stage 4E entry), but
  CI itself does not. Pre-existing gap, not introduced by this stage;
  recorded for a future CI hardening pass.
- **Two unreachable/low-severity hardening notes** (identity architecture
  and security reviews): `RegisterPlayerLinkedToPerson` does not
  independently verify a resolver-supplied `personID` exists before
  insert (relies on the FK constraint, surfacing a generic DB error
  rather than a labeled one on a buggy resolver); a typed-nil
  `PersonResolver` value (as opposed to a bare `nil` interface) would
  defeat the `deps.PersonResolver == nil` fail-closed check in
  `auth_routes.go` - not reachable via any current wiring (`cmd/platform-
  api/main.go` and every test construct a real, non-nil
  `*MockPersonResolver`), recorded as a Go-idiom hardening note only.

See `docs/progress.md`'s Stage 4E entry for the same summary in the
project's overall progress log.
