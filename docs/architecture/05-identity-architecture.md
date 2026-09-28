# 05 — Identity Architecture Proposal

Status: Stage 0 proposal. Source: Blueprint §4.1.

## Model

- One `player` row per `(tenant_id, email)`. Player identity is per brand
  — the same human can legitimately hold accounts at multiple partner
  brands.
- One `person` cluster, resolved post-KYC from a hash of document number +
  date of birth, linking `player` rows across brands/tenants that belong
  to the same human.
- Self-exclusion, fraud links, and AML case history attach to `person`,
  never to `player` — otherwise platform-level self-exclusion and
  multi-accounting detection (both audited) are impossible.

## Session vs. game/product tokens

Two different token types, never conflated:

1. **Brand session token** — short-lived JWT, identifies player + tenant,
   used for brand-frontend/back-office API calls.
2. **Game/product launch token** — separate, single-use, opaque, bound to
   `(player, provider, game, currency, mode)`, short TTL, minted at launch
   and exchanged once by the provider for their own session.

A provider is never handed the brand session token. A leak at any one of
potentially thirty+ integrations must not become account takeover across
the whole platform — this is a `security`-reviewed invariant, not a
suggestion.

## Ownership

`identity-compliance` owns this model; `security` reviews token issuance
and scoping; `architect` signs off on the schema since it's referenced by
KYC/AML, RG, and audit.

## Open dependency

Cross-brand person resolution assumes B2B partners' players are visible to
the platform-level `person` cluster. Whether partners bring their own
KYC/AML vendor relationship (Blueprint §10 Q2) affects how much of this
resolution can be automated vs. requires a partner-side data-sharing
agreement — flagged in `docs/decisions/0005-open-business-decisions.md`.

## Stage mapping

Built in Stage 2 (Identity + tenancy + security). KYC/AML orchestration
that hangs off this model is Stage 4 (see `11-kyc-aml-rg-architecture.md`).

## Implementation status (Stage 2)

The model above is realized with one adjustment: `Player` is split into
two distinct concepts rather than one row, per
`docs/decisions/0012-brand-distinct-from-tenant.md` and the Stage 2
instructions' explicit requirement not to collapse Person/PlayerAccount/
Tenant/Brand for convenience:

- `Person` (`internal/identity/person.go`, `persons` table) — platform-
  wide, no `tenant_id` (not tenant-owned data), but NOT unprotected:
  RLS restricts `SELECT`/`UPDATE`/`DELETE` to the platform scope
  (`WithoutTenant`) while allowing `INSERT` from any scope, since
  registration creates a person from within a tenant-scoped transaction —
  see `docs/decisions/0015-persons-platform-scope-access-control.md`
  (this corrects an earlier Stage 2 gap where `persons` had no RLS at
  all). Carries only a partial-unique `person_key_hash` hook for the
  future post-KYC cross-brand resolution described above; Stage 2 does
  not populate or resolve it (`NOT IMPLEMENTED` — no KYC document data
  exists yet to hash). Each registration currently creates one `Person`
  per `PlayerAccount`; linking two `PlayerAccount`s to the same `Person`
  is Stage 4 work.
- `PlayerAccount` (`internal/identity/player_account.go`,
  `player_accounts` table) — the "player" row above: one per
  `(brand_id, email)`, `tenant_id`-owned, RLS-protected. This is the
  Brand session token's subject.
- `Brand` (`internal/identity/brand.go`, `brands` table) is new relative
  to this doc's original proposal — the consumer-facing product a
  `PlayerAccount` belongs to, distinct from `Tenant` (the commercial/
  legal entity). A `Tenant` may operate more than one `Brand`. See ADR
  0012 for why this split exists and why `brands` is deliberately
  publicly readable (brand metadata, not player data) while
  `player_accounts` is not.

Session vs. game/product tokens: the Brand session token described above
is implemented (`internal/auth/jwt.go`'s access token, short-lived,
HMAC-signed, carries `tenant_id`/subject/role/principal_type). The
game/product launch token is `NOT IMPLEMENTED` — out of scope until
Stage 3's casino/sportsbook provider integration exists to consume it;
nothing in Stage 2 hands a Brand session token to a provider, so the
"never conflate the two" invariant has nothing to violate yet.

Authentication itself moved well beyond a JWT proof-of-concept: Argon2id
password hashing, `kid`-based key rotation, single-use rotating refresh
tokens with reuse detection revoking the full session chain, Postgres-
backed login lockout, and a permission-oriented RBAC replacing Stage 1's
role-list skeleton. See the Stage 2 completion report for the full
authentication/authorization architecture and its security review.

## Implementation status (Stage 4E) — Person resolution

Stage 4D-RG's own specialist review found that "each registration
currently creates one `Person` per `PlayerAccount`" (above) was a P0: it
made cross-brand self-exclusion (ADR 0026's own platform-wide
`player_restrictions` mechanism) unable to ever actually reach a second
brand's registration, since nothing could recognize two registrations as
the same real person. Stage 4E (`docs/decisions/0027`) closes this by
inserting an identity-resolution boundary BEFORE Person creation, without
introducing a second identity model:

- `internal/identityresolution.PersonResolver` — a provider-neutral
  interface (`Match` / `NoMatch` / `Uncertain` / unavailable), mirroring
  `CasinoProvider`/`PaymentProvider`'s exact shape. `MockPersonResolver` is
  the only implementation shipped this stage - `NOT IMPLEMENTED` for any
  real KYC/identity-verification vendor.
- Registration flow: `identityresolution.RegisterPlayerWithResolution`
  calls the resolver BEFORE deciding whether to link an existing `Person`
  (`identity.RegisterPlayerLinkedToPerson`), create a brand-new one
  (`identity.RegisterPlayer`, Stage 2's original behavior), or land the
  account in a new safe status, `identity_review_required`
  (`identity.RegisterPlayerPendingReview`, migration 0039) - reusing the
  EXISTING `PlayerAccountStatus` enum `internal/rg.EvaluateEligibility`
  already denies, rather than inventing a parallel status/table.
- **Honest current limitation** (see ADR 0027's own "Carried-forward
  limitations" section for the full statement): every registration
  reachable over the live HTTP API resolves `NoMatch` today, because no
  trusted source of `VerifiedAttributes` (legal name, DOB, government ID
  reference, KYC provider reference) is wired into the register handler
  yet - matching on raw, client-supplied registration fields (this doc's
  original "hash of document number + date of birth" proposal, which
  assumed KYC had already run) would be worse than no matching at all.
  The resolution orchestration and cross-brand enforcement are proven
  correct at the package/integration-test level against a resolver a real
  future vendor integration will replace; they are not yet actively
  preventing evasion for a real player until that vendor exists.
- Platform-wide vs. tenant-scoped remains exactly as this doc's original
  "Model" section describes: `Person` is platform-wide, self-exclusion
  attaches to `Person`, never to `PlayerAccount` - Stage 4E does not
  change this, it makes the platform-wide `Person` actually reachable
  from a second brand's registration for the first time.

## Implementation status (Stage 4F) — player verification, documents, email/password auth

Stage 4F (`docs/decisions/0028`/`0029`/`0030`) adds a platform-owned KYC/
identity-verification and document-management subsystem, and email-
verification/password-reset authentication, without touching the Person/
PlayerAccount model above:

- `internal/kyc.Verification`/`Document` are entirely new, TENANT-owned
  concepts (`kyc_verifications`/`kyc_documents`, migration 0040) hanging
  off the existing model via `player_account_id`/`person_id` foreign
  keys - `person_id` is carried only as a denormalized anchor, never an
  RLS or enforcement key this stage (ADR 0028 §3). `kyc_verifications.
  status` never touches `PlayerAccountStatus`, and vice versa.
- **Still `NOT IMPLEMENTED`**: this stage does NOT supply `VerifiedAttributes`
  to `internal/identityresolution.PersonResolver` from KYC evidence - the
  connection point ADR 0027 left open remains open (ADR 0028 §7). A real
  KYC verification's approved evidence is a natural future source for
  `PersonResolver`, but wiring it requires the same care ADR 0027 already
  documented: never heuristic matching, never automatic Person merging.
  Cross-brand real-player protection (Stage 4E) therefore remains exactly
  as inactive for a real player as it was before this stage.
- `player_accounts.verified_at` (Stage 2's own reserved hook) is
  repurposed for EMAIL verification specifically
  (`internal/httpserver/credential_handlers.go`), entirely separate from
  KYC identity verification - see ADR 0030 §7 for the full rationale,
  including the dormant `PlayerStatusPendingVerification` gap it closes
  as a side effect.
- `internal/kyc.KYCProvider`/`Orchestrator`/`MockKYCProvider` mirror
  `CasinoProvider`/`PaymentProvider`'s exact provider-abstraction shape
  (ADR 0028 §4/§6) - `NOT IMPLEMENTED` for any real vendor, per this
  stage's own explicit non-goal (directive §1).

## Stage 4H-B0 — Retail identity impact (architecture only, `NOT IMPLEMENTED`)

Issued as part of "STAGE 4H-B0 — BONUS + GAMIFICATION + RETAIL
ARCHITECTURE/SCOPE FREEZE" (design-only; no code, no migration). The
confirmed business requirement is that the platform support retail
operations (a physical agent/cashier network, in-person player
registration) as another surface of the same platform, sharing identity/
KYC/RG with online. Owner: `identity-compliance`, covering identity and
KYC/RG impact only. The hierarchy data model itself
(`docs/architecture/26-retail-operations-architecture.md`) and hierarchy
RBAC (`docs/decisions/0036-retail-hierarchy-rbac-and-audit.md`) are
`architect`'s and `security`'s parallel work, not designed here — this
section is written against explicitly stated assumptions about that
parallel work (below), to be reconciled once those documents land.

**Assumptions this section makes about the parallel hierarchy/cashier
design** (stated explicitly per this stage's own directive, since neither
parallel document exists yet as of this writing): (1) a cashier
authenticates as a staff-shaped principal scoped to a specific retail
location within a tenant's hierarchy — not a `PlayerAccount`, not a new,
unrelated actor type; (2) a retail location/terminal is a client of the
same `platform-api` backend the online frontend already talks to — it is
never given direct database access or a local copy of any compliance
logic; (3) a hierarchy (agent network) is scoped to a single tenant by
default, with the possibility of a licence/operator's network spanning
more than one tenant left open by the business requirement (`docs/
architecture/26`'s own question to answer, not assumed here either way —
see the parallel KYC/AML/RG document's §4/§5 for why this matters to
self-exclusion scope). If any of these assumptions turns out to be wrong
once the two parallel documents land, this section's conclusions need a
re-check, not a silent carry-forward — recorded here so that
reconciliation is a checklist, not a rediscovery.

### 1. Player registration through retail — reuses the existing flow, plus one new provenance fact

Retail registration is the same lifecycle event as online registration —
"a player registers" — and reuses `identityresolution.
RegisterPlayerWithResolution` → `identity.RegisterPlayer*` exactly as it
stands today (ADR 0027). It does **not** need a second registration
code path, a second `Person`/`PlayerAccount` creation function, or a
parallel identity model — the same `Match`/`NoMatch`/`Uncertain`/
resolver-unavailable dispatch (ADR 0027 §3) applies identically regardless
of which channel the request came from, because none of that dispatch
logic is channel-aware today and has no reason to become so: person
resolution answers "is this the same real person," a question that does
not change meaning depending on whether the request arrived over HTTPS
from a browser or from a cashier's terminal calling the identical
backend endpoint.

What retail registration genuinely adds, and what does not yet exist to
express it:

- **`registration_channel` provenance — `ARCHITECTURAL DECISION`,
  `NOT IMPLEMENTED`.** `player_accounts` has no column today recording how
  an account was opened. A cashier-assisted registration is a materially
  different provenance fact from a self-service web/app registration —
  not because the identity model needs a second shape, but because at
  least three downstream, jurisdiction-configurable decisions legitimately
  need to read it (KYC initial-tier defaulting, RG defaults, and future
  AML/reporting channel-splitting — see the companion KYC/AML/RG
  document's §1/§2 for what specifically reads it). This should be a
  small, additive column — `registration_channel` (`online` | `retail`,
  extensible), `NOT NULL DEFAULT 'online'` for backward compatibility —
  following exactly the same additive-migration discipline already used
  for `PlayerAccountStatus`'s own widening (migration `0039`,
  `identity_review_required`) rather than a schema redesign. Not built
  this stage — no migration is written, per this stage's own
  design-only scope.
- **Actor provenance for the assisting cashier — `RECOMMENDATION`.**
  Mirroring `player_restrictions`' own `created_by_actor_type`/
  `created_by_actor_id` pattern (ADR 0026 §2) and
  `RegisterPlayerWithResolutionParams`' existing `RequestID`/`IPAddress`/
  `UserAgent` audit fields (ADR 0027 P2 fix), a retail registration's
  `identity_resolution.performed`/`player.registered` audit records should
  additionally carry the assisting cashier's staff-principal id — never
  folded into the `Person`/`PlayerAccount` row itself (that would be
  conflating an audit fact with an identity fact), but present in
  `audit.Entry.Metadata` exactly as every other actor-attributed action on
  this platform already is. **Wave-2 review correction (F7, P1)**: an
  earlier draft of this bullet claimed audit metadata is what makes a
  later "which cashier registered this account" query possible "without
  inventing a new table" — that is withdrawn. `architect`'s
  `retail_player_origins` table (`docs/architecture/26-retail-operations-
  architecture.md` §3.2/§5.1 entity 10, `UNIQUE(player_account_id)`) is
  the authoritative source of record for node/terminal/cashier
  attribution — it is what `security`'s subtree-scoped player accessor
  (ADR 0036 §5.5) and commission attribution actually query, because a
  JSONB audit-metadata value cannot serve as an indexable join target or
  an RLS predicate. The audit-metadata record above is a complementary
  audit trail, not the source of record — this document's own
  `registration_channel` column and architect's `retail_player_origins`
  table are the two pieces that together record retail provenance;
  neither replaces the other.
- **Does this need a new KYC tier? No — extend the existing tiered-trigger
  model, do not invent one.** Blueprint §4.7's tiered model (`docs/
  architecture/11-kyc-aml-rg-architecture.md`) already keys KYC
  requirements to lifecycle events, and "registration" is already one of
  those events — retail registration is the SAME event through a
  different channel, not a new event needing a new tier. What is
  genuinely open is which STARTING verification requirement/tier a
  retail-originated account defaults to, and whether in-person presence at
  a cashier counts toward satisfying part of it — that is a
  jurisdiction-configuration question, not a new tier taxonomy, and is
  addressed in the companion KYC/AML/RG document's §1/§2 (`OPEN DECISION`,
  compliance/legal sign-off required, not resolved here).

### 2. Cashier as an actor — extending Stage 3D's mandatory Person-linkage rule

Stage 3D (`docs/decisions/0024-stage3d-withdrawal-governance-final-gate.md`
§1) made a specific staff-integrity rule authoritative at the database
layer: a `staff_users` row must carry a non-`NULL`, append-only-once-set
`person_id` AND `status = 'active'` before it may approve or reject a
withdrawal — closing a self-dealing risk (a staff member approving their
own or a colluding party's payout) using the SAME `Person`/`StaffUser`
identity architecture already in place, never a second identity model.

**Extension, `ARCHITECTURAL DECISION`:** the identical mandatory
Person-linkage requirement should extend to any cashier action that is
financially or compliance-equivalent to a withdrawal decision — most
concretely, a cashier confirming a cash payout (a retail withdrawal
analogue) or a cashier confirming a cash deposit that immediately credits
a player wallet. The reasoning is the same self-dealing concern Stage 3D
already closed for withdrawal approval, applied to the same class of risk
at a different counter: an unlinked (`person_id IS NULL`) cashier account
is, by Stage 3D's own finding, "invisible to enforcement" — the SAME
statement is true whether the money movement is a withdrawal approval or
a retail cash payout, and there is no principled reason cashiers should
be exempt from a rule justified purely by "does this action move money
based on this staff member's own decision." **Whether a retail cash
payout is legally/operationally the same enforcement point as a
withdrawal approval, or needs its own distinct governance mechanism, is
for `payments`/`ledger-finance` to design when retail cash handling is
implemented** (retail cash flows are not this specialist's domain) — this
section states only that, from the identity-linkage angle, the SAME
`staff_users.person_id`-mandatory/`status = 'active'` rule should be the
default posture, not a weaker one invented for retail's sake, per this
specialist's Authority ("cannot weaken an RG or KYC enforcement rule to
ease a product flow").

**A genuinely new consideration Stage 3D did not need to address —
`ARCHITECTURAL DECISION`: staff/player identity collision (staff self-play
prohibition).** Stage 3D's self-approval check compares two `StaffUser`
principals' `person_id`s to each other. Retail introduces a new pairing:
a cashier (`StaffUser`, linked `person_id` per the extension above) and
the player they are registering or serving (`PlayerAccount`, linked to
its own `person_id` via ADR 0027's resolution flow). A cashier registering
or transacting for a player who resolves to the CASHIER'S OWN `person_id`
— i.e., staff attempting to play through their own retail terminal, a
well-known retail-gaming integrity risk this platform has not previously
needed to consider because no staff principal has ever been able to touch
a `PlayerAccount`'s money flow as both parties — is a distinct risk from
Stage 3D's peer-to-peer self-approval case. **This is flagged as an
`OPEN DECISION`, not resolved here**: whether to enforce it as a hard
platform-level block (a `person_id` equality check between the acting
staff principal and the resolved player at registration/cash-transaction
time, analogous in shape to Stage 3D's self-approval trigger but a NEW
mechanism, not a reuse of the withdrawal-specific one) or as a detective/
audit control (flagged for review, not blocked) is a policy call for
`identity-compliance` to make jointly with `security`/`architect` once the
retail transaction surface (owned by `payments`/`casino`/whoever ends up
building retail cash/bet handlers) is designed — recording the question
now so it is not silently absent from that design.

### 3. Cross-tenant hierarchy and Person scope — no change to the identity model itself

**Wave-2 review correction (F11, P2)**: an earlier draft framed this as
open ("if a hierarchy spans more than one tenant, left open by doc 26").
`docs/architecture/26-retail-operations-architecture.md` H5/§5.1 has
since resolved this definitively: a node's parent must be in the same
`tenant_id` and the same `network_id`, enforced by a composite foreign
key — a cross-tenant parent edge is a constraint violation. **A hierarchy
network never spans tenants.** What doc 26 §5.1 entity 4 does allow is
*several networks within one tenant* (e.g. one per licence/jurisdiction/
brand) — a materially different topology from what this section
originally assumed. Nothing about `Person` being platform-wide
(unchanged since Stage 2) or `PlayerAccount` being tenant-owned changes —
a cashier resolving or serving a player still goes through the exact same
`identity.RegisterPlayer*`/`internal/rg.EvaluateEligibility` surface,
scoped by the SAME `(tenant_id, brand_id, player_account_id)` triple every
other caller already resolves server-side. The genuinely open question,
corrected per doc 26's resolution above, is squarely about Responsible
Gaming restriction SCOPE — whether a staff-initiated restriction must
reach every **network and brand within one tenant/licence**, not "across
tenants" — not about the identity model — see the companion KYC/AML/RG
document's §4 for the full treatment; this document does not duplicate it
here to avoid two sources of truth for the same open question.

## Stage 4H-B1, Wave 1.5 Fix Wave — actor≠subject Person-linkage confirmation, and the affiliate identity/authority model (task 4HB1FW-05)

Status: **DESIGN ONLY, `NOT IMPLEMENTED`.** No code, migration, or Human
Decision Register item is selected here. This section feeds two parallel
dispatches this same round - `security`'s cross-cutting actor≠subject
invariant (4HB1FW-04, closing P0 SEC-W15-03) and `architect`'s Affiliate
architecture fix (4HB1FW-03, closing P0 SEC-W15-01/P1-4 on doc 32) - per
the orchestrator's own unifying contract that actor≠subject is "one
reusable platform invariant... with `identity-compliance` confirming the
underlying Person-linkage mechanism is the right primitive to build it
on." Since neither parallel dispatch's output is visible this round,
reconciliation happens at Phase 2 review; this section states the
foundation honestly, including what it cannot yet guarantee, rather than
asserting an outcome those dispatches haven't confirmed.

### A. Is `staff_users.person_id` <-> `player_accounts` -> `Person` sufficient for actor≠subject?

**Split answer: the subject side is sound; the actor side has a real,
named gap.**

**Subject/beneficiary side - sound.** Every `player_accounts` row has an
immutable `person_id`, set once at registration (the "Implementation
status (Stage 4E)" section above) and never re-pointed after creation -
there is no mutation path that changes which `Person` a given
`PlayerAccount` row resolves to. So "which `Person` does this specific
player subject/beneficiary resolve to" is a single, unambiguous,
always-populated join (`player_accounts.person_id`) for any concrete
`player_account_id` a Grant, a CRM `RequestOfferGrant` target, or a
`PlayerAttribution` names. This holds regardless of ADR 0027 Decision 8's
honest limitation that cross-brand *matching* of two different
`PlayerAccount`s to the *same* `Person` is not yet live for a real
player - that limitation is about whether two accounts get correctly
merged into one `Person`, not about whether a single, already-existing
account's `person_id` is a reliable, resolvable fact. It is. Any
comparison keyed on a named subject/beneficiary account stands on solid
ground.

**Actor side - a genuine, precisely-named gap, not an edge case.**
`staff_users.person_id` is nullable by design (migration 0029's own
comment: "the overwhelming majority of staff accounts have no
corresponding player account... that's expected, not an incomplete
migration") and is set **only** by a deliberate admin action
(`CreateStaffUser` with a non-nil `personID`, or the remediation path
`LinkStaffPersonID`) - there is no automatic process that ever populates
it. Concretely: **a staff member who is secretly also a player, and
whom no admin has ever linked, is invisible to any `person_id`
comparison.** `staff.PersonID` is `NULL`; the comparison has nothing to
compare against; the self-dealing case the invariant exists to catch
passes silently. This is not a hypothetical corner case - it is the
*expected* state of the overwhelming majority of `staff_users` rows, and
"linked" vs. "not linked" today reflects only "did an admin happen to
know and bother to link them," never a verified fact.

It is worse than "no automated check exists" - **there is currently no
data model at all to check against.** `StaffUser` (`internal/identity/
staff_user.go`) carries email, password hash, role, status, and an
optional `person_id`. It has no legal name, no date of birth, no
document reference - nothing `internal/identityresolution.
PersonResolver` (or any hypothetical future staff-side equivalent) could
match against even if one were built today. This is stated plainly
because CLAUDE.md's "no fake completion" rule forbids implying the
comparison is airtight when it structurally cannot be: **the actor≠subject
invariant, however well `security` implements the comparison logic, is
only as strong as which staff accounts happen to carry a `person_id` -
and today that is a matter of admin knowledge, not verified fact.**

**Is this closeable now, or does it need a new capability?**

- **Closeable now (a policy/process control, not a new technical
  capability), `RECOMMENDATION`:** require a mandatory, audited
  attestation at staff onboarding - and periodically re-attested, e.g.
  annually or on role change to any permission that can approve a
  Grant/CRM send/affiliate commission - asking each staff member to
  declare whether they are also a registered player at any brand on the
  platform, or a declared beneficial owner of any affiliate node (§C
  below). A "yes" answer routes to the existing, sanctioned
  `LinkStaffPersonID` remediation path (or the new beneficial-owner
  attestation field, §C.3); a "no" answer is itself recorded as a dated,
  auditable claim - so a later discovery that the attestation was false
  is a documented compliance/HR violation with a specific false
  statement on record, not silence. This is honesty-based, not
  detection-based, and must be described as exactly that: it closes "we
  never asked," not "they lied." It requires no new resolver, no new
  matching logic, and no new PII collection beyond a yes/no declaration
  plus (on "yes") the existing linkage flow - squarely within what this
  specialist can specify without inventing an automated-matching
  mechanism the directive forbids.
- **Not closeable now - a genuine future capability requiring a human/
  privacy decision, not invented here:** a *detective* control (an
  automated cross-check comparing staff identity data against player
  identity data, the way KYC-based `PersonResolver` evidence eventually
  will for cross-brand player matching) would require collecting
  staff-side identity evidence (legal name, DOB, government ID
  reference) equivalent to KYC evidence - a materially new, privacy-
  sensitive employee-data-collection decision (mirroring ADR 0027's own
  open question about collecting verified attributes at all), and
  explicitly the kind of "invent a way to be certain" mechanism this
  task's directive forbids building here. **This is recorded as a
  genuine platform gap, not resolved**: absent that future capability
  (or absent one being authorized), the honest fail-closed answer is
  that an *unattested, unlinked* staff actor cannot be affirmatively
  cleared as "not the subject" - the invariant should therefore not
  silently pass an unlinked actor as "no collision found" (see the
  fail-closed shape in §B below), and any residual exposure from a false
  "no" attestation is a policy/detection gap for compliance monitoring
  to own, not something identity-resolution's technical mechanism can
  close today.

### B. The primitive-level query/join shape - and the lesson from migrations 0029 -> 0034

`security` should build the unconditional comparison on the **same shape
migration 0034 already proved**, not migration 0029's earlier, weaker
shape - the difference between the two is the single most important fact
this confirmation can hand over, because it is a fail-open/fail-closed
distinction the codebase has already lived through once:

- **Migration 0029 (Stage 3C)** compared `approver_person_id` against
  `requester_person_id` but treated a `NULL` `approver_person_id` as "no
  data to compare, allow" - i.e., an *unlinked* staff member's
  self-approval passed silently, because the comparison itself, not
  eligibility to act, was the only gate.
- **Migration 0034 (Stage 3D)** fixed this by making linkage a
  **precondition of eligibility to act at all**: `approver_person_id IS
  NULL` now raises an exception outright ("approver has no confirmed
  Person linkage and is not eligible to record withdrawal decisions"),
  *before* the equality comparison is even reached. `internal/
  withdrawal.ApproverEligibility`'s Go-level mirror of this same rule
  documents the reasoning directly: "Stage 3C's optional, unenforced
  Person linkage was found insufficient."

**Confirmation:** any new unconditional actor≠subject trigger must
replicate 0034's shape, not 0029's. Concretely, for any table recording a
staff decision that grants, approves, or targets value/access
(`Grant`/adjustment issuance, a CRM `RequestOfferGrant`/send, a
`CommissionApproval`):

```
actor:    SELECT person_id, status FROM staff_users WHERE id = :actor_principal_id
          -- NOT FOUND + is_automated/service-identity  -> exempt (ADR 0014 pattern, migration 0034's own precedent)
          -- NOT FOUND + a human decision                -> REFUSE (identity cannot be resolved)
          -- FOUND, status != 'active'                   -> REFUSE
          -- FOUND, person_id IS NULL                     -> REFUSE  (0034's fix - NOT "skip comparison")

subject/beneficiary: resolved per domain, e.g.
          Grant beneficiary   -> SELECT person_id FROM player_accounts WHERE id = :player_account_id
          CRM target          -> SELECT person_id FROM player_accounts WHERE id = :target_player_account_id
          Affiliate commission-> SELECT declared beneficial_owner_person_id (§C.3) for the paying node,
                                  UNION the node's own StaffUser.person_id if that account is itself linked

compare:  IF actor.person_id = subject.person_id THEN REFUSE (self-dealing)
          IF subject-side ownership is UNKNOWN (no attestation on file) THEN REFUSE
              -- cannot exclude collision, per this task's own fail-closed instruction -
              -- "unknown beneficial ownership" is not evidence of independence.
```

This is exactly `internal/withdrawal`'s existing two-layer pattern
(`BeneficiaryCheck` + `ApproverEligibility` in Go, `withdrawal_approvals_
enforce_governance` in SQL as the backstop that holds "even if [the
Go-level check] is bypassed, disabled, or has a bug") generalized to
Grant/CRM/Affiliate - not a new mechanism. The one substantive addition
this confirmation flags for `security`'s design, beyond what 0034 already
proves: **a nil/absent eligibility check on a human decision must be a
hard error (`ErrInvalidInput`-shaped), never an implicit skip** -
`ApproverEligibility`'s own doc comment already states this discipline
("a nil ApproverEligibility on a non-automated call is itself a
fail-closed ERROR here... never a silent skip"); every new call site
`security` designs (Grant issuance, CRM send, commission approval) must
carry the identical contract, or a future caller that simply forgets to
wire the check reopens exactly the P0 this invariant exists to close.

### C. The affiliate identity/authority model

`docs/architecture/32-affiliate-and-acquisition-architecture.md` §3.2
already decided an affiliate user authenticates as an `identity.
StaffUser` with an affiliate-scoped role (no new principal type) and
flagged the harder question - internal staff vs. external commercial
counterparty isolation - to `security` as DEP-AFF-1. That decision is
sound and this section does not revisit it. What it does not yet
address, and what this task asks this specialist to name, is that
"affiliate" conflates several genuinely distinct concepts the identity
model has no way to express today. Naming them precisely is a
precondition for `architect`'s and `security`'s parallel fixes to close
SEC-W15-01 (two colluding affiliate accounts satisfying four-eyes) and
the separate, currently-unowned conflict-of-interest vector (an
employee who is personally the undisclosed beneficial owner of an
affiliate node).

**C.1 - Affiliate entity.** The actual external commercial counterparty
(a company, a person, a partnership) that may control **multiple**
affiliate accounts/nodes. **No existing mechanism expresses this.**
`Person` is the wrong fit - `Person` is this platform's model of a
*human*, built for KYC/self-exclusion/AML, and an affiliate entity may be
a company, not a human; forcing a corporate affiliate into `Person`
would corrupt a compliance-critical table with commercial-relationship
data it was never designed to hold, and is explicitly out of scope
(`Person` stays exactly what ADR 0027/this doc's "Model" section define
it as). **Minimal addition needed:** a lightweight `AffiliateEntity`
reference concept - commercial-relationship data (legal/trading name, a
business or tax reference, a primary contact), owned by whichever
package ends up owning `internal/affiliate`'s operational surface (doc
32 §3), not by `internal/identity`. This specialist's contribution is
naming the concept and its one identity-relevant property: an
`AffiliateEntity` is the thing a `beneficial_owner_*` reference (C.3)
ultimately resolves to when the owner is itself a company rather than an
individual - the schema shape is `architect`'s/`internal/affiliate`'s to
design, not designed here.

**C.2 - Affiliate account/node.** One login/access credential within
`internal/agentnetwork`'s hierarchy - per doc 32 §3.2's existing
decision, one `identity.StaffUser` row scoped to one node. No new
addition needed for the account/node concept itself; it is already
specified. **One extension this specialist does recommend, following
directly from the "Cashier as an actor" precedent above:** any
`StaffUser` used for affiliate-node access should be brought under the
**same** mandatory Person-linkage discipline this doc already extends to
cashiers, and that migration 0034 already enforces for withdrawal
approvers - not because every affiliate account is expected to resolve
to a `Person` (most, like most staff, will not), but because "is this
affiliate account secretly the same real person as an internal
approver, or as the player it attributes" is precisely a `person_id`
comparison, and it can only ever fire if the affiliate account's own
`person_id` is populated when it genuinely applies. This is the same
"closeable now via attestation, not automated matching" answer as §A.

**C.3 - Beneficial ownership.** The relationship between an affiliate
entity and the node(s) it controls, **including the case where an
internal staff member is personally the beneficial owner of an
affiliate node** - the conflict-of-interest vector `security`'s Phase 2
report flagged as real and currently unowned. **No existing mechanism
expresses this at all**: `agentnetwork` nodes (as specified in doc 26/32)
carry no ownership reference, `StaffUser` carries no "declared external
commercial interest" field, and `Person` (again, deliberately) carries
none either. This is the sharpest gap of the three, because it is
exactly what the orchestrator's own interim mitigation for SEC-W15-01
("the approver on any affiliate-financial decision must be an internal,
non-affiliate principal") does **not** close: an internal approver is
still fully capable of being the *undisclosed* beneficial owner of the
very affiliate node whose commission they are approving. "Internal, not
affiliate" answers *who the approver's login belongs to*; it does not
answer *whose money the approver personally benefits from*. Those are
different questions, and only the second is what a beneficial-ownership
fact can close.

**Minimal addition specified (per this task's explicit boundary - a
declared attestation, never automated/biometric matching):**

- A `beneficial_owner_person_id` (for an individual owner) or
  `beneficial_owner_entity_id` (for a corporate owner, resolving to
  C.1's `AffiliateEntity`) reference on the affiliate node record -
  schema ownership is `architect`'s/`internal/affiliate`'s, not
  designed here.
- Populated **only** through a declared, audited attestation process:
  the node's controlling party (or, where the node is created/managed by
  internal staff on an affiliate's behalf, the onboarding staff member)
  affirmatively declares beneficial ownership at node creation, and
  re-attests periodically (mirroring the staff attestation cadence in
  §A) or on any ownership change. Every attestation and re-attestation
  writes an audit record (actor, previous value, new value, reason) -
  the same append-only, reason-coded discipline `CLAUDE.md` already
  requires for every compliance-relevant action.
- **Fail-closed default, stated exactly as this task's directive
  requires:** a node with **no attestation on file has UNKNOWN
  beneficial ownership** - this must never be silently treated as "no
  known conflict, therefore independent." Any operation where the
  approver's ownership of the node cannot be affirmatively excluded (no
  attestation, or an attestation that is stale past its re-attestation
  window) must be **refused**, exactly like the actor-side `person_id IS
  NULL` case in §B is refused rather than skipped. This is the same
  fail-closed posture this specialist's own Authority requires
  elsewhere ("cannot weaken an RG or KYC enforcement rule... enforcement
  is not optional") applied to a new fact this platform has never
  needed to track before.
- **What this does and does not close, stated honestly:** an attestation
  is a declared fact, not a verified one - this closes "we never asked
  and therefore assumed independence," exactly like §A's staff
  attestation; it does not, and cannot without the forbidden automated-
  matching mechanism, catch a deliberately false attestation. A false
  attestation is a fraud/detection problem for compliance monitoring
  (e.g., a periodic reconciliation report flagging affiliate nodes that
  share bank/payout details, IP ranges, or other correlatable signals
  despite declaring independent ownership) - worth naming as a future
  compliance-analytics control, not built here, and explicitly not a
  substitute for the attestation-based fail-closed default above.
- **A related, currently out-of-scope question flagged for `architect`/
  product policy, not resolved here:** SEC-W15-01's interim fix (approver
  must be internal) closes affiliate-affiliate collusion for the
  four-eyes check specifically. It does not address one external entity
  legitimately holding **multiple** affiliate nodes and using the
  beneficial-ownership fact this section proposes to detect it - should
  shared beneficial ownership across two nodes be capped, disclosed, or
  blocked from certain commission structures (e.g. sub-affiliate
  override, doc 32 §6.4)? That is a commercial/Risk policy question this
  specialist is not positioned to answer and does not attempt to here;
  recording it so the beneficial-ownership field's existence doesn't
  quietly imply the policy question is already settled.

**C.4 - The five roles this task asks to be distinguished, mapped
explicitly** (concepts, not new tables beyond C.1/C.3 above):

| Concept | Definition | Existing mechanism |
|---|---|---|
| Actor identity | The authenticated principal performing an action | `identity.StaffUser` (unchanged, doc 32 §3.2) |
| Legal/organizational authority | Whose interest the actor is acting under when performing the action | Platform, for internal staff (implicit); the affiliate entity (C.1), for an affiliate account - **not currently a distinguishable fact for affiliate accounts**, since `StaffUser` has no entity reference (this is what C.1 closes) |
| Affiliate entity | The external commercial counterparty, may control N nodes | **Does not exist** - C.1 |
| Affiliate account/node | One credentialed access point, 1:1 with a `StaffUser` row, per node | Exists as specified (doc 32 §3.2) |
| Approver | The `StaffUser` recording a `CommissionApproval` (or Grant/CRM-equivalent) decision | Exists as a role; the *eligibility* rule (must be internal, must resolve to an active, linked Person) is what §B specifies |
| Subject | The player/account the underlying activity is about | `player_accounts.person_id` - sound (§A) |
| Beneficiary | Whoever receives the value from the decision being approved | Player, for a Grant; the affiliate node/entity, for a commission - **ownership of "the affiliate node" is exactly C.3's unresolved fact** |
| Commission owner | The affiliate entity with the contractual claim to a specific accrual | **Does not exist as a distinguishable fact from "beneficiary"** - relevant precisely when a payout is contractually owed to a different entity than the node that generated it (e.g. a sub-affiliate override paid to a parent, doc 32 §6.4) - resolved by the SAME `AffiliateEntity`/beneficial-ownership references above, not a sixth concept |

### D. Summary - what is closeable now vs. a genuine platform gap

**Achievable now, no new capability, feeds directly into `security`'s and
`architect`'s designs this round:**
- The subject-side join (`player_accounts.person_id`) is sound and
  needs no change.
- The fail-closed shape (missing linkage = refuse eligibility to act,
  not skip the comparison) is already proven at migration 0034 and
  should be replicated verbatim for every new actor≠subject enforcement
  point.
- A mandatory, audited staff attestation process (identity linkage +,
  separately, declared external commercial interests) is specifiable
  now and requires no automated matching.
- The affiliate identity/authority concepts (C.1-C.4) can be named and
  specified now; `AffiliateEntity` and `beneficial_owner_*` as
  attestation-backed reference fields, fail-closed on absence.

**Genuine platform gaps, not resolved here:**
- An unlinked, unattested (or falsely-attested) staff member who is also
  a player, or is personally the undisclosed beneficial owner of an
  affiliate node, is invisible to any `person_id`-based comparison - this
  is a detection/compliance-monitoring gap, not something identity-
  resolution's technical mechanism can close without the automated-
  matching capability this task's directive explicitly forbids building.
- Whether to invest in a future staff-side identity-evidence collection
  capability (to eventually feed the same kind of resolver player
  identity does) is a privacy-sensitive human/product/legal decision, not
  made here.
- Whether shared beneficial ownership across multiple affiliate nodes
  should be capped, disclosed, or restricted from certain commission
  structures is a commercial/Risk policy question, not an identity
  question, and is not resolved here.

## Stage 4H-B1, Wave 1.5 Fix Round 2 — identity-compliance independent re-verification

Four items, per the Round 2 dispatch (`docs/security/security-
architecture.md` commit `7fe8143`, `REQ-SEP-STAFF-1`) and this
specialist's own Phase 1 findings (`docs/governance/wave-1.5-fixwave-
phase2-report.md` §"Identity-compliance", against commit `0c6ff60`,
bonus-engine's rewritten G-2 mechanism, doc 10 §N1 / `ledger-accounting-
model.md` §7.7.2).

### E. `REQ-SEP-STAFF-1` — conditional sign-off, two changes required

**Schema fit, confirmed against the real migration
(`migrations/0011_create_staff_users.up.sql`, `person_id` added by
`0029`, made append-only by `0034`).** `status TEXT NOT NULL DEFAULT
'active' CHECK (status IN ('active','suspended'))` and `person_id UUID
REFERENCES persons(id)` (nullable) are exactly the column shapes the
proposed `CHECK (status <> 'active' OR person_id IS NOT NULL)` needs.
There is no type mismatch, no existing constraint it collides with, and
`ALTER TABLE ... ADD CONSTRAINT` validating every existing row is the
correct fail-closed behavior security's text already claims for it. The
mechanism itself is sound and I sign off on it **as a mechanism**.

**But the constraint as literally written is table-wide, and two things
about `staff_users`'s actual schema make that consequential, not merely
academic:**

1. **The remediation query's own `WHERE tenant_id = <t>` shape structurally
   cannot see platform-scoped staff.** `staff_users.tenant_id` is
   nullable by design — `migration 0011`'s own comment: "`NULL` means a
   platform-wide staff member (role `platform_admin`)" — and the
   constraint documented in that same migration (`role = 'platform_admin'
   AND tenant_id IS NULL`) means every `platform_admin` row has
   `tenant_id IS NULL`, always. A remediation query framed as "run per
   tenant" (`WHERE tenant_id = <t> AND status = 'active' AND person_id IS
   NULL`) never matches `tenant_id IS NULL` for any concrete `<t>` — SQL
   equality against NULL is never true, not even accidentally. Running
   that query once per tenant, however many times, leaves every active
   `platform_admin` row with a NULL `person_id` completely unexamined.
   The migration's `ALTER TABLE ADD CONSTRAINT` validates the *whole*
   table regardless, so any surviving row like that fails the migration
   outright at deploy time — the right fail-closed outcome, but only if
   whoever runs remediation knows to look there, which the "per tenant"
   framing as written does not tell them. **Required change:** state the
   remediation as two passes, mirroring the dual-scope pattern
   `staff_users`'s own RLS policy already uses (`migration 0011`'s
   `dual_scope_isolation`) — one per-tenant pass exactly as drafted, plus
   one platform-wide pass, `WHERE tenant_id IS NULL AND status = 'active'
   AND person_id IS NULL`, before the constraint is added.

2. **Whether `platform_admin` should be required to hold a `person_id` at
   all is a real, separate question, not a remediation detail.** `SEP-1`
   (`security-architecture.md` §W15.1.3 step 4) refuses whenever
   `staff_users.tenant_id <> ` the authorizing row's `tenant_id` — and
   every economically-consequential operation this contract enumerates
   (Bonus/CRM/Affiliate, §W15.1.2's table) is tenant-owned. A
   `platform_admin` row's `tenant_id` is always `NULL`, never equal to
   any concrete tenant's id, so a `platform_admin` acting as `A` or `p ∈
   P` on any of those operations is refused by step 4 regardless of
   whether `person_id` is set — **requiring `platform_admin` to carry a
   `person_id` buys `SEP-1` nothing**, since that role structurally
   cannot pass the trigger's own tenant-scope check to reach the
   comparison `person_id` exists to gate. (I am not re-deriving `SEP-1`'s
   own logic here, only observing what the constraint's necessity reduces
   to once `SEP-1`'s published step order is taken as given — `security`
   should confirm this reading independently before relying on it, since
   this touches the trigger's own semantics, which I don't own.) Absent
   that requirement, this constraint's only effect on `platform_admin` is
   to force fabricating a `Person` row for every platform-wide staff
   account that will never need one for the stated purpose — pure
   remediation cost with no `SEP-1` benefit, and in direct tension with
   `migration 0033`'s own documented invariant that `person_id` is
   "nullable and OPTIONAL: the overwhelming majority of staff accounts
   have no corresponding player account and never will, so most rows
   keep it NULL forever - that's expected, not an incomplete migration."
   **Required change, or an explicit override from `architect`/`security`
   stating the broader scope is intentional:** exempt `platform_admin`
   explicitly — `CHECK (status <> 'active' OR person_id IS NOT NULL OR
   role = 'platform_admin')` — consistent with the role-conditioned CHECK
   pattern `migration 0011` already uses on this same table, rather than
   requiring `person_id` for a role SEP-1 can never actually check it
   against. If this change is made, `migration 0033`'s doc comment above
   should also be updated (by whoever owns that migration file in
   practice) to state the new, narrower invariant — "optional and
   NULL-forever for `platform_admin`; mandatory for every other active
   role" — so it doesn't read as contradicted by the new constraint.

**Verdict: not signed off as drafted.** Sign off on the mechanism (a real
DB `CHECK`, correct types, correct fail-closed validate-on-add behavior);
require both changes above — the two-pass remediation and the
`platform_admin` carve-out (or an explicit, named decision to skip the
carve-out) — before this ships in the same migration alongside any
`SEP-1`/`AFF-4E-1` trigger, per the dispatch's own sequencing requirement.
`architect`'s cross-domain sign-off (also requested by the dispatch) should
independently confirm item 2's reading of `SEP-1` step 4 before relying on
it.

### F. Prior finding re-check — the held-disposition/`person_id` surface: **CLOSED** at the design level

Last round I found `REQ-PS-ID-1` (security-architecture.md, "the
self-exclusion open-bet enumeration must see `pending_settlement`
Grants") narrower than the actual gap: a `HeldDispositionRecord` can
exist on an already-fully-terminal Grant with no open bet at all, so no
generalization of *open-bet* enumeration would ever surface it, and a
second, independent surface joining held dispositions to `person_id`
was needed.

`bonus_held_dispositions`'s finalized schema (`ledger-accounting-
model.md` §7.7.2.5) gives exactly that surface, and gives it more
directly than I had assumed last round:

- `player_account_id UUID NOT NULL` is denormalized directly onto the
  row — no join through `grant_id`/`wallet_id`/`ledger_entries` is
  needed to reach it, and it is `NOT NULL`, not merely usually-present.
- `player_accounts.person_id` is itself `NOT NULL` (`migrations/
  0010_create_player_accounts.up.sql` line 15) — so the chain
  `bonus_held_dispositions.player_account_id → player_accounts.id →
  player_accounts.person_id` resolves to exactly one `person_id` for
  every disposition row, with no NULL-hole of the kind migration
  `0029`'s original defect had.
- `status` (`held` / `resolved_reforfeit` / `resolved_route_to_cash` /
  `voided_by_rollback`) is the row's **own** field, entirely independent
  of the attributed Grant's lifecycle state — so the exact gap I flagged
  (a terminal Grant hiding an open hold from any Grant-state-keyed
  enumeration) cannot recur here by construction: the new surface never
  needs to look at Grant state at all, only `WHERE status = 'held'`.
- `tenant_id` is present and RLS-scoped (§7.7.2.5's own note, closing
  `architect`'s Phase 2 finding that the record was originally missing
  `tenant_id`/RLS/brand scope entirely) — so this surface can be built
  as a per-tenant enumeration job following the same shape
  `self_exclusion_enumeration_runs` (migration `0043`/`0049`) already
  uses for the Grant-side enumeration, rather than a novel cross-tenant
  query.

The concrete shape (design-only — no code exists, none is claimed):
join `bonus_held_dispositions` (`status = 'held'`) → `player_accounts`
(on `player_account_id`) → `player_restrictions` (on `person_id`, per
`migrations/0037_create_player_restrictions.up.sql`, itself
platform-wide/person-keyed, `restriction_type = 'self_exclusion'`) —
run per-tenant, recorded the same way `self_exclusion_enumeration_runs`
already records a Grant-side run, so a dropped/incomplete run is
detectable the same way S-9 already requires for the existing
enumeration.

**Verdict: CLOSED at the data-model level.** The schema supports building
this surface with no missing field, no ambiguous join, and no
Grant-state dependency — strictly easier to build than what I described
last round, precisely because `player_account_id` now lives on the row
directly. Naming it formally so it has a tracked identity: **`REQ-PS-
ID-2`** (no other document has assigned this a number yet) — "a
`bonus_held_dispositions` row with `status = 'held'` attributed to a
self-excluded `person_id` must be visible to a self-exclusion
enumeration surface, independent of and in addition to `REQ-PS-ID-1`'s
Grant-side enumeration." Building the actual job/query is not required
to close this Phase 2 finding, per this round's own framing (design-only);
what closes it is that the data model no longer has a hole the design
could fall into.

One thing worth carrying forward when `REQ-PS-ID-2` is actually built:
apply `security`'s `SEP-1-H1`/`W15.1.9` lesson (a resolver that reads
zero rows under a partial-RLS-scope must be treated as a hazard, not a
clean "nothing found") to this resolver too — the same class of failure
(silently-inert enumeration passing every test because it never actually
ran under the right scope) applies here as much as it does to `SEP-1`'s
own resolvers.

### G. `LF-12` aging check — still open, correctly named this time, not yet decided

Last round's second finding: `LF-12`'s aging check did not prioritize
self-excluded persons. `ledger-accounting-model.md` §7.7.2.8 item 3 (the
finalized `LF-12` reconciliation stream) now says, verbatim: "prioritized
per `identity-compliance`'s Phase 2 note that self-excluded players' open
holds need priority review — **not decided here**, named so the
reconciliation job's own design does not have to rediscover it."

**Verdict: NOT CLOSED, but no longer silently dropped.** This is
progress over last round (the note is now on record in the owning
document, attributed, and won't be rediscovered from scratch), but the
actual prioritization design — how the aging job orders or escalates a
`held` row once it knows the attributed person is self-excluded — still
does not exist. Re-flagging it as open: `ledger-finance` (owner of
`LF-12`) still needs to specify how the aging sweep queries
self-exclusion status (presumably the same `player_restrictions` join
`REQ-PS-ID-2` above uses) and what "priority" means operationally
(shorter aging threshold before escalation, a distinct queue, an
immediate alert rather than waiting for the normal aging window) before
this is buildable, not just nameable.

### H. `voided_by_rollback` vs. self-exclusion enumeration — no new gap; one forward dependency noted

`ledger-accounting-model.md` §7.7.2.7 adds `voided_by_rollback` for a
provider rollback targeting a still-`held` win. Checked against
`REQ-PS-ID-2`'s proposed surface (§F above):

- A `voided_by_rollback` row is terminal (`status <> 'held'`) and
  economically zero — the win is fully reversed to `house_gaming`, no
  value remains attributed to the player. It falls out of a `WHERE
  status = 'held'` enumeration exactly the way `resolved_reforfeit`/
  `resolved_route_to_cash` already do, by the same mechanism, for the
  same reason: no open exposure remains to surface. This is symmetric
  with how the existing Grant-side enumeration already treats a fully
  terminal, zero-exposure Grant — no special case is needed.
- The row itself is never deleted (append-only, per this document's own
  ledger rules), so a compliance investigator reviewing a self-excluded
  person's full history still finds it on an unfiltered historical
  query against `bonus_held_dispositions` — only the *active-exposure*
  enumeration (which must exclude terminal rows to avoid false-positive
  alerts) needs the `status = 'held'` filter. No audit-trail gap.
- The one thing I am **not** closing here, because §7.7.2.7 itself
  doesn't: a rollback naming an **already-resolved** record (value
  already moved to `player_bonus`/`player_cash`, possibly already
  withdrawn) is explicitly routed to `LF-10` and left undecided. If a
  self-excluded person's held win was resolved (`resolved_route_to_cash`)
  and *then* a late rollback arrives, whatever `LF-10` eventually
  decides (clawback / receivable / reject-and-alert) needs to be checked
  against self-exclusion enumeration at that time — a self-excluded
  person with a disputed, already-cashed-out bonus win is exactly the
  kind of case a self-exclusion review would want visibility into.
  Flagging this as a dependency on `LF-10`'s resolution, not a defect of
  this round's design.

**Verdict: no new gap in the enumeration surface itself.** The new
transition is consistent with the terminal-state handling every other
disposition outcome already gets; the one open edge (late rollback of an
already-resolved record) is `LF-10`'s to resolve, and I'll re-check its
self-exclusion interaction once `LF-10` lands.

## PRH-2 K1 — scoped financial capability grants (ADR 0099)

`docs/decisions/0099-scoped-financial-capability-grants.md` is the source of
truth; this section is a pointer into the identity model for it, not a
restatement.

- **A new Person-linked, time-bounded staff authority mechanism**, distinct
  from the static `internal/auth` role → permission map. A `finance` staff
  member or a `platform_admin` does not automatically hold any of the four
  financial capabilities (`ledger_adjustment:initiate/approve`,
  `payment_force_resolve:request/approve`) - each capability is *granted*,
  per tenant, per staff principal, via a request/platform-co-approval flow
  (`staff_capability_grant_requests` → `staff_capability_grant_approvals` →
  `staff_capability_grants`), and can be revoked (a one-way, append-only
  action) at any time.
- **Three grant flows:** G-T (tenant-originated, for a tenant's own
  `finance` staff), G-P2 (platform-originated, for a `platform_admin`
  acting in a named tenant - the mechanism behind "platform principal
  acting in tenant X", ADR §6). **G-P1** (platform-originated, for a
  tenant's `finance` staff) is **DEFERRED out of PRH-2** by architect
  ruling (2026-09-28, ADR §4.1) - it is not implemented and has no route.
- **Identity invariants this depends on:** every grant request/approval
  requires three *distinct, non-NULL* Persons (requester, approver,
  grantee) - the sock-puppet defense, since one Person can hold multiple
  staff principals across tenants (§9's own identity caveat). This is why
  ADR 0099 forces and snapshots `grantee_person_id` at request time, and
  why it depends on migration 0034's `person_id` append-only trigger
  (once a staff row's Person is linked, it cannot be silently re-pointed
  at a different Person to defeat that check).
- **STAFF-LIFECYCLE-1 dependency (open):** this identity doc's own staff
  lifecycle model does not yet have a suspend/role-change/Person-unlink
  path that also cancels pending grant requests and revokes grants in the
  same transaction (ADR 0099 §8.4, security ruling S-b). Until
  STAFF-LIFECYCLE-1 exists, a suspended grantee only stops counting at the
  next request/approval/execution-time check (ADR §7), not immediately.
