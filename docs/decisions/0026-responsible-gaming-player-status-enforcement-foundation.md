# ADR 0026 — Responsible Gaming Player-Status Enforcement Foundation

Status: Accepted. Issued as "STAGE 4D-RG — RESPONSIBLE GAMING &
PLAYER-STATUS ENFORCEMENT FOUNDATION", directly following Stage 4A's own
specialist review, which found a production-blocking gap: no authoritative
platform-side player-account/wallet-status or self-exclusion check existed
at casino game launch or bet time (`docs/decisions/0025`'s own
"Specialist review findings and fixes" section, and
`docs/active-stage.md`'s Stage 4A "Blockers / genuine scope boundaries").
This stage closes exactly that gap - not the full KYC/AML/Responsible
Gaming subsystem `docs/architecture/11-kyc-aml-rg-architecture.md`
describes, which remains a Stage 0 proposal beyond self-exclusion. Owner:
`identity-compliance`, with `casino` on the launch/bet wiring,
`ledger-finance` on preserving every existing financial invariant, and
`security`/`code-reviewer` on RLS and the concurrency guarantees.

No real casino provider integration, no sportsbook, and no Bonus Engine
work is introduced or touched by this stage. The Stage 3D withdrawal
submit/resolve TOCTOU risk and Stage 4A's own carried-forward limitations
(the payments-side unguarded-reversal race, per-tenant provider signing
keys) are unaffected and remain separately tracked (see this ADR's own
"Carried-forward limitations" section).

## Context

Stage 2's identity model already has three independent, tenant/brand-aware
status concepts: `Person.Status` (platform-wide), `PlayerAccount.Status`
(`pending_verification | active | suspended | self_excluded | closed`,
per brand), and `Wallet.Status` (`active | frozen | closed`, per wallet).
None of them were ever consulted by `internal/casino`'s `LaunchGame` or
`postBet` - a suspended or wallet-frozen player could still launch a game
and place a bet, and `PlayerAccount.Status = 'self_excluded'` was
brand-scoped only, so a Person barred on one brand could register a new
account on a second brand of the same operator and keep playing. Stage
0's own `11-kyc-aml-rg-architecture.md` anticipated exactly this gap:
"self-exclusion at both brand and platform level (platform level requires
the cross-brand `person` cluster...)".

This stage does not invent a new identity model. It adds the ONE genuinely
new concept these existing types cannot express (a restriction bound to
the platform-wide Person, not a single brand relationship), and builds the
one new enforcement boundary that composes it with the existing signals.

## Decisions

### 1. No second identity model - one new signal, composed

The authoritative eligibility decision for "may this player gamble right
now" is a composition of exactly these signals, in this order, never a new
parallel status enum:

| Signal | Owner (existing) | New in this stage? |
|---|---|---|
| Identity status | `Person.Status` | No - not consulted (no identity-level gate exists yet; unverified/verified is a KYC-tier concern, out of scope) |
| Player-account status | `PlayerAccount.Status` | No - `!= active` now denies (previously never checked) |
| Wallet status | `Wallet.Status` | No - `!= active` now denies (previously never checked) |
| Responsible-gaming restriction / self-exclusion | `player_restrictions` (NEW) | **Yes** - the one new table this stage adds |
| Regulatory/jurisdiction restriction | `casino_games.jurisdiction_blocklist` (existing, per-game) | No - unchanged; a *player-level* jurisdiction restriction is a documented extension point (§14), not implemented |
| AML/KYC restriction | none yet | Not implemented - documented extension point (§14) |
| Technical/provider session status | `casino_launch_sessions.status` | No - unchanged (ADR 0025 §3) |

`PlayerAccount.Status = 'self_excluded'` (the existing brand-scoped value)
is left exactly as-is and still means "this specific brand relationship is
closed" - it is not repurposed or deprecated. The NEW, first-class,
cross-brand concept is `player_restrictions`.

### 2. `player_restrictions` — the self-exclusion record

Migration `0037` adds one table:

```sql
player_restrictions (
    id, person_id,                -- the enforcement anchor (platform-wide)
    tenant_id, brand_id,           -- administrative scope (NULL = platform-wide)
    player_account_id,             -- provenance only, never the enforcement key
    restriction_type,              -- 'self_exclusion' only, this stage
    starts_at, ends_at,            -- ends_at NULL = indefinite
    source,                        -- 'player_self_service' | 'staff'
    reason_code,
    created_by_actor_type, created_by_actor_id,
    created_at
)
```

Scope is derived from nullability, never a separately-stored (and
therefore independently-mutable) field: `tenant_id IS NULL` → platform;
`brand_id IS NULL` (tenant_id set) → tenant-wide; both set → brand-only.

**Append-only.** No `UPDATE`/`DELETE` path exists for anyone, ever - not
even the row's own author, not even staff. This is the single biggest
simplification this stage makes: a self-exclusion cannot be edited,
shortened, or silently revoked by anyone, which closes an entire class of
"player attempted to alter their own restriction" / "staff attempted an
unauthorized change" adversarial scenarios by construction rather than by
review discipline. A time-bound exclusion simply stops matching
`EvaluateEligibility`'s "currently active" query once `ends_at` passes -
there is no separate mutable status column to fall out of sync.
Enforced by RLS (no `UPDATE`/`DELETE` policy for the actual mutation - see
§10) **and** a `player_restrictions_deny_mutation()` trigger, mirroring
`audit_log`/`ledger_entries`/`ledger_transactions`/`withdrawal_approvals`'
identical defense-in-depth pattern (`docs/decisions/0013`).

**No "end restriction early" endpoint is implemented.** Most jurisdictions
treat early termination of a self-exclusion as requiring specific legal
process (a cooling-off/reconsideration period, documented proof of
capacity, etc.) that this stage has no mandate to invent. Building an
early-termination endpoint without that legal grounding would itself be a
compliance risk. **OPEN DECISION**: whether/how any jurisdiction this
platform operates in permits early reinstatement of a self-exclusion is a
legal question, not an engineering one - flagged for human/legal
input before any such endpoint is ever built.

### 3. Player self-exclusion is always platform-wide

A player's own self-service self-exclusion (`POST /v1/me/rg/self-exclusion`)
always creates a `tenant_id = NULL, brand_id = NULL` (platform-wide) row -
the player is never asked to choose a narrower scope. This is deliberate:
the entire point of self-exclusion is protecting the player from
themselves, including from opening a *different* brand account to evade
it (directive §4/§9) - a self-service control that only protected the one
brand the player happened to be looking at would defeat its own purpose.

**OPEN DECISION** (recorded per CLAUDE.md's hybrid-licensing note,
`docs/decisions/0006`): for a tenant operating under its **own** licence
in its **own** jurisdiction (rather than under this platform's licence),
whether platform-wide data-sharing of a self-exclusion event is legally
required, permitted, or even desirable is a jurisdiction/licensing
question this stage does not resolve - it defaults to maximal player
protection (platform-wide) for every tenant today, and a future stage may
need to make this configurable per tenant's `licensing_model` once that
legal question is answered.

### 4. Staff-initiated restrictions are tenant/brand-scoped only

`CreateStaffRestriction` accepts `Scope ∈ {tenant, brand}` - **never**
`platform`. This is not a simplification for its own sake: `player_accounts`
carries only a single-scope `tenant_isolation` RLS policy (migration
0010), so a tenant-scoped principal (`RoleCompliance`, the sole grantee of
`PermRGRestrictionWrite`) can only ever resolve a player account within
its OWN tenant to begin with - there is no existing platform-wide
player-lookup capability anywhere in this codebase for a hypothetical
platform-scoped RG-restriction endpoint to build on (`PermPlayerRead`/
`PermPlayerSuspend` are themselves `RequireTenantScope`-gated - even
`RolePlatformAdmin` cannot browse a specific tenant's players today).
Building a platform-wide player-lookup capability solely to support a
platform-wide staff-initiated restriction endpoint, with no other caller
and no test coverage from a real permission path, is exactly the kind of
"capability nothing can actually use" CLAUDE.md's "no fake completion"
rule warns against. **The only path that produces a platform-wide
restriction this stage is player self-service** (§3) - which needs no
such lookup, since it always acts on the caller's own, already-
authenticated identity. A genuinely platform-wide staff-initiated
restriction capability remains an explicit **OPEN DECISION**/future item.

### 5. `EvaluateEligibility` — the single authoritative policy boundary

```go
func EvaluateEligibility(ctx, tx, EligibilityParams{
    TenantID, BrandID, PlayerAccountID, WalletID,
}) (Decision, error)
```

`Decision{Allowed bool, Code string, Message string, PersonID uuid.UUID}`.
This is the ONLY function in the codebase that answers "may this player
gamble right now" - `internal/casino`'s `LaunchGame` and `postBet` both
call it (via a shared `evaluateAndAuditEligibility` wrapper), and any
future gambling-action entry point (a sportsbook bet, a future casino
financial operation) is expected to call the identical function rather
than re-implement its own copy of these checks (directive §5's explicit
"do not duplicate independent RG checks throughout handlers").

Order of checks, and why: (1) resolve the account and take the person
advisory lock (§8) BEFORE reading anything else - closes the TOCTOU race;
(2) `PlayerAccount.Status != active` → deny (`player_account_not_active`);
(3) an active `player_restrictions` row for this person, scoped to cover
this (tenant, brand) → deny (`self_excluded`); (4) if a wallet id is
given, `Wallet.Status != active` → deny (`wallet_not_active`); (5)
otherwise allow. A denial is never silent: see §11.

### 6. Casino launch enforcement

`LaunchGame` calls `evaluateAndAuditEligibility` immediately after the
existing game/tenant/jurisdiction/asset checks and **before** `CreateLaunchSession`
mints anything - a prohibited player never obtains a usable session, full
stop (directive §6's literal requirement). `LaunchGameResult` gains
`Denied bool`, `DenialCode string`, `DenialMessage string` fields - **not**
a Go error. This is a genuine, test-discovered correction to the initial
implementation: returning the denial as an error caused `db.WithTenant`'s
caller-supplied callback to return non-nil, which rolls back the ENTIRE
transaction - including the audit record `evaluateAndAuditEligibility`
had just written in the same transaction. A launch denial that silently
lost its own audit trail would have violated directive §5's "result must
be... auditable" requirement outright. The fix mirrors `postBet`'s
already-correct insufficient-funds decline (a result field, `nil` error)
exactly.

### 7. Casino bet enforcement

`postBet` calls the identical `evaluateAndAuditEligibility` immediately
after resolving the session's wallet, **before** any ledger account is
resolved or `lockCashBalance`/`ledger.Post` runs. A denial reports via
`ReceiveCallbackResult{Outcome: OutcomeDeclined, DeclineReason: <code>}`,
`nil` error - the exact same shape the pre-existing insufficient-funds
decline already used, so an RG-declined bet is exactly as
provider-protocol-normal as an insufficient-funds decline (a valid,
expected wallet-API response), never a transport-level error a provider
might retry oddly. **No ledger entry is ever created for a denied bet** -
proven by test (`TestReceiveCallback_BetDeclinedWhenSelfExcluded_NoLedgerEffect`,
`..._WhenWalletFrozen_NoLedgerEffect`), including the invariant-#1
debits=credits check across the whole tenant. Every existing financial
invariant (idempotency, the `FOR UPDATE` balance lock, the session-binding
fix from Stage 4A's own specialist review) is untouched - the eligibility
check is a pure precondition inserted before the financial work begins,
never interleaved with it.

Re-evaluating independently of `LaunchGame`'s own launch-time check is
deliberate, not redundant: a `casino_launch_sessions` row can span an
arbitrarily long round, and a restriction applied mid-round must still
stop the NEXT bet (directive §7/§8).

### 8. Concurrency: the person advisory lock

```sql
SELECT pg_advisory_xact_lock(hashtext('player_restrictions'), hashtext($1::text))
```

Every writer of a `player_restrictions` row (`CreateSelfExclusion`,
`CreateStaffRestriction`) and `EvaluateEligibility` itself take this SAME
transaction-scoped advisory lock, keyed on `person_id`, before reading or
writing anything else for that person. Released automatically at
`COMMIT`/`ROLLBACK` (`pg_advisory_xact_lock`, not the session-scoped
`pg_advisory_lock`). This is the mechanism that actually determines the
concurrency outcomes below - not a documented-but-unenforced intention.

**Directive §8(A) — self-exclusion concurrent with game launch.**
Whichever transaction acquires the person lock first runs to completion
(commit or rollback) before the other's own lock acquisition can proceed.
There are exactly two correct outcomes, and the implementation does not
claim to pick between them - it makes BOTH consistent: (a) the launch
transaction acquires the lock first, evaluates "not yet excluded", and
commits a usable session BEFORE the self-exclusion exists; or (b) the
self-exclusion commits first and the launch is cleanly denied, minting NO
session at all. What is structurally impossible is a THIRD outcome - a
launch that reports success while zero or a partial session row exists,
or a launch denial that leaves an active session behind.
`TestConcurrent_SelfExclusionDuringLaunch_Deterministic` (15 iterations,
`-race`) asserts exactly this: `launchSucceeded → sessionCount == 1`,
`launchDenied → sessionCount == 0`, never anything else.

**Directive §8(B) — self-exclusion concurrent with a bet.** Identical
reasoning: either the bet's transaction wins the lock and posts against
the balance as it stood before the exclusion committed, or the exclusion
wins and the bet is declined with zero ledger effect. Never both a posted
debit AND a `player_restrictions` row that had already fully committed
before the bet transaction's own lock acquisition returned.
`TestConcurrent_SelfExclusionDuringBet_Deterministic` asserts the balance
is EITHER exactly the funded amount (declined) OR funded-minus-stake
(posted), and invariant #1 (`debits == credits`) holds in both cases,
across 15 concurrent iterations.

**Directive §8(C) — a duplicate bet delivery concurrent with
self-exclusion.** This composes TWO existing guarantees rather than
needing a new one: `ledger.Post`'s own `(provider_id, provider_tx_id)`
idempotency key means at most one financial effect for a given
`provider_tx_id` regardless of how many times it is delivered or how it
interleaves with anything else, and the person-lock above still governs
whichever delivery reaches `postBet` first relative to the concurrent
self-exclusion. `TestConcurrent_DuplicateBetDeliveryDuringSelfExclusion`
proves both together: across 15 iterations with two concurrent identical
deliveries plus a concurrent self-exclusion, the balance is always exactly
one of the two valid end states and at most one `ledger_transactions` row
ever exists for that `provider_tx_id`.

No stronger serialization (e.g. `SERIALIZABLE` isolation, application-
level queueing) was found necessary - the advisory lock's mutual exclusion
on the one resource that actually needs it (a specific person's
restriction state) is sufficient, and adding broader serialization would
only cost throughput on every other, unrelated concurrent operation.

### 9. Cross-brand and tenant-isolation enforcement

Directive §9's two required properties, both proven with real Postgres
(no mocks) across separate tenants and separate brands of one tenant:

- **A Person with a PlayerAccount on Brand A and Brand B (same tenant)**:
  self-excluding via Brand A's account blocks eligibility checks made
  through Brand B's account, including a full `LaunchGame` HTTP-adjacent
  call (`TestLaunchGame_DeniedCrossBrandSelfExclusion`,
  `TestCreateSelfExclusion_CrossBrandSameTenantBlocksOtherAccount`).
- **A Person with a PlayerAccount at two entirely different Tenants**: the
  identical property holds across tenant boundaries, not just brand
  boundaries within one tenant (`TestCreateSelfExclusion_
  CrossTenantBlocksOtherAccount`) - this is what actually validates the
  platform-wide (`tenant_id IS NULL`) row design, not merely the
  tenant-wide case.
- **Tenant isolation for restriction ADMINISTRATION** (the converse
  property - one tenant must not be able to create or read another
  tenant's own tenant-scoped restrictions): proven by
  `TestPlayerRestrictions_RLS_TenantScopedRowInvisibleToOtherTenant`,
  `..._StaffCannotInsertForAnotherTenant`, and
  `..._ForgedTenantColumnRejected` (a raw SQL `INSERT` claiming a foreign
  tenant id from a tenant-A-scoped connection - not merely a Go-level
  check, an actual RLS `WITH CHECK` violation).

### 10. RLS design (migration 0037)

`player_restrictions` carries `FORCE ROW LEVEL SECURITY` and four policies
plus two visibility-only policies, deliberately NOT mirroring `audit_log`'s
`dual_scope_isolation` shape verbatim (an earlier draft tried to and found
it could not express "a player, authenticated normally with both
`app.tenant_id` and `app.player_account_id` set, must still be able to
insert a `tenant_id IS NULL` row for themselves" - see the migration
file's own comments for the exact reasoning):

- `staff_and_system_read` (`SELECT`): a staff/system context (no
  `app.player_account_id`) sees its own tenant's rows plus every
  platform-wide row - this is what makes the enforcement boundary itself
  (running under `db.WithTenant`) able to see a DIFFERENT tenant's
  platform-wide self-exclusion at all.
- `player_self_read` (`SELECT`): a player-scoped context sees only rows
  matching THEIR OWN person_id, resolved server-side.
- `staff_insert` (`INSERT`): a tenant-scoped connection may only insert
  `tenant_id = <its own tenant>`; a genuinely platform-scoped connection
  (`db.WithoutTenant`) may insert `tenant_id IS NULL` - never a
  tenant-scoped connection smuggling in a NULL tenant_id column value
  (the `WITH CHECK` requires `app.tenant_id` ITSELF be unset for that
  branch, not merely the inserted column).
- `player_self_insert` (`INSERT`): a player-scoped connection may insert
  ONLY a `tenant_id/brand_id`-NULL, `self_exclusion`/`player_self_service`
  row naming THEMSELVES (`person_id`/`player_account_id`/
  `created_by_actor_id` all re-derived server-side from
  `app.player_account_id`, never trusted from the request body).
- `staff_and_system_update_visibility` / `..._delete_visibility`: scope
  visibility ONLY (mirrors the read policy) - granted so the deny-mutation
  trigger has a real row to intercept and can therefore fail LOUDLY with
  its own named exception, rather than an `UPDATE`/`DELETE` silently
  affecting zero rows (which would be indistinguishable from targeting a
  nonexistent id, and untestable as a genuine guarantee). No policy of any
  kind ever permits the mutation itself to succeed - the trigger denies it
  unconditionally, regardless of what these policies make visible.

All four RLS/adversarial test categories directive §10 requires (cross-
tenant read/insert/update/delete; cross-brand unauthorized access; player
attempting to alter their own restriction; staff attempting an
unauthorized restriction change) have dedicated, passing tests - see §16.

### 11. Audit

Every restriction creation writes an `audit.Entry` (action
`rg.self_exclusion.created`) in the SAME transaction as the insert -
person id, target player account id, scope, indefinite-or-not, and (for
staff) the mandatory reason code, in `Metadata`; never a secret. Every
DENIED eligibility decision also writes an `audit.Entry` (action
`casino.launch_denied` / `casino_bet.denied_by_rg_policy`, outcome
`OutcomeDenied`) with the reason code and person id. An ALLOWED decision
is deliberately not separately audited - the downstream action's own
existing audit record (`casino.launched`, `casino_bet.posted`) is already
evidence the decision was "allow", and auditing every single allowed bet a
second time here would roughly double `audit_log`'s write volume for zero
additional information. This asymmetry (denials always audited here,
allows evidenced downstream) is deliberate, not an oversight.

### 12. Permissions

`PermRGRestrictionWrite`/`PermRGRestrictionRead` are new, standalone
permissions - never bundled into `PermPlayerSuspend`/`PermStaffManage`/
`PermTenantWrite`. `PermRGRestrictionWrite` is granted to `RoleCompliance`
alone (directive §12's explicit "do NOT automatically grant to every
broad administrator" - `RoleTenantAdmin`, which already holds
`PermPlayerSuspend`/`PermStaffManage`, does NOT get it, mirroring Stage
3D's identical withdrawal-approval separation-of-duties precedent).
`RolePlatformAdmin` gets NEITHER permission - see §4 for why it would be
unusable dead capability. `PermRGRestrictionRead` additionally goes to
`RoleTenantAdmin` (reasonable operational visibility into why a player is
restricted, without write authority).

### 13. API surface

Minimal, per directive §13 - no back-office UI, no invented legal
workflow:

| Route | Caller | Notes |
|---|---|---|
| `POST /v1/me/rg/self-exclusion` | Player (self) | Always platform-wide (§3); `duration_days` optional (indefinite if omitted) |
| `GET /v1/me/rg/status` | Player (self) | Own restriction history, RLS-scoped |
| `POST /v1/admin/rg/restrictions` | `PermRGRestrictionWrite` | `scope` ∈ {tenant, brand}; `reason_code` mandatory |
| `GET /v1/admin/rg/restrictions?player_account_id=` | `PermRGRestrictionRead` | One named account's restriction history |

No modify/end-restriction endpoint exists anywhere (§2's append-only
design). No platform-wide staff-initiated creation endpoint exists (§4).

### 14. Future KYC/AML integration point (not implemented)

`EvaluateEligibility`'s own shape is designed to grow a KYC/AML check as
ONE MORE step in its existing sequence (after the wallet-status check,
before returning `Allowed`), without any change to its callers
(`LaunchGame`/`postBet` would need zero code changes - they already treat
"call `EvaluateEligibility`, branch on `Decision.Allowed`" as opaque).
`PlayerAccount.KYCTier` (Stage 2) already exists as an unread hook for
this. Not implemented this stage: no KYC/AML vendor integration, no
tier-threshold enforcement logic. This is a documented extension point,
not a promise of a specific future design.

### 15. Future RG controls (documented extension points, not implemented)

Only self-exclusion is implemented end-to-end this stage. The following
remain explicitly **NOT IMPLEMENTED**, matching directive §15's "only
implement what's necessary for this foundation":

- **Deposit/loss/wagering/session limits**: would each need their own
  accumulator (a running total against a configurable ceiling, reset on a
  schedule) - a materially different shape than a binary restriction, not
  a `player_restrictions.restriction_type` value. `player_restrictions`'
  own `restriction_type` CHECK constraint intentionally admits only
  `'self_exclusion'` today (extending it is an additive future migration,
  not a redesign) - never added as a value nothing enforces.
- **Reality checks**: a client-side/session-timer concern, not a
  database-backed restriction at all.
- **Time-outs / cooling-off periods**: structurally identical to
  self-exclusion (a time-bound restriction) and could plausibly become a
  SECOND `restriction_type` value reusing this exact table/RLS/lock
  design - a natural, low-risk extension, but not built without a
  concrete requirement driving its specific parameters (typical duration
  ranges, whether it differs from self-exclusion in reversibility, etc.).

## Concurrency semantics (summary)

See §8 for full detail. Summary table:

| Race | Winner | Guarantee |
|---|---|---|
| Self-exclusion vs. launch | Whichever acquires the person lock first | No launch ever leaves a session in an inconsistent state relative to the exclusion's own commit |
| Self-exclusion vs. bet | Whichever acquires the person lock first | No bet ever posts a ledger effect inconsistent with the exclusion's own commit; invariant #1 always holds |
| Duplicate bet delivery vs. self-exclusion | `ledger.Post` idempotency + the person lock, composed | At most one financial effect per `provider_tx_id`, regardless of ordering |

## Database changes

Migration `0037_create_player_restrictions`: one new table, two triggers
(`player_restrictions_immutable`, `player_restrictions_no_truncate`), six
RLS policies. No existing table is altered. Round-tripped (`up` → `down`
→ `up`) cleanly on a database already carrying casino/withdrawal/ledger
data from prior stages' own test runs.

## Consequences

- The casino launch/bet path now has an authoritative, single, reusable,
  auditable, race-tested gate - the production-blocking gap Stage 4A
  identified is closed for self-exclusion and existing account/wallet
  status, though not for KYC/AML or player-level jurisdiction (§14, not
  yet implemented).
- Two OPEN DECISIONs are recorded for human/legal input: whether a
  bring-your-own-licence tenant's self-exclusions should ever be
  tenant-scoped rather than platform-wide by default (§3), and whether/how
  a self-exclusion may ever be terminated early (§2).
- A platform-wide, staff-initiated restriction capability remains
  unbuilt (§4) pending a genuine platform-wide player-lookup capability
  that does not exist anywhere else in this codebase yet.
- `LaunchGameResult` now has a `Denied` result-shape in addition to its
  existing error-based failure modes - a deliberate, documented
  inconsistency (§6) driven by the audit-durability requirement, not an
  oversight.

## Specialist review findings and fixes

_(Populated after the mandatory 7-specialist review pass - see
`docs/active-stage.md` for the in-progress record and the Stage 4D-RG
completion report for the final summary.)_

## Carried-forward limitations (not addressed by this stage)

- Stage 3D's withdrawal submit/resolve TOCTOU risk - untouched, this
  stage's changes do not intersect that code path.
- Stage 4A's payments-side unguarded-reversal race (`internal/payments`'
  own deposit-reversal check) - untouched.
- Stage 4A's per-tenant provider signing keys gap - untouched, still a
  precondition for any real casino provider, not a Stage 4D-RG concern.
- Stage 4A's Bonus Engine accounting open decisions (free-round/bonus-
  stake normalization, jackpot contribution splits) - untouched.
