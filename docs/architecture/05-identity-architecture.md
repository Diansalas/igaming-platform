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
