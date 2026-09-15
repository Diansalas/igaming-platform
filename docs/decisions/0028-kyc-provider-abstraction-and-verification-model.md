# ADR 0028 — Platform-Owned Verification Model & KYC Provider Abstraction

Status: Accepted. Issued as part of "STAGE 4F — PLAYER VERIFICATION,
DOCUMENTS & AUTHENTICATION FOUNDATION", following Stage 4E's Person-
resolution foundation. This stage does NOT implement real KYC/AML: no
vendor is selected, no real provider API is integrated, and no vendor API
is invented. It builds the platform-owned verification state model and a
provider-neutral boundary a real vendor adapter slots into later.

## Context

Stage 2 already reserved two unused hooks on `player_accounts` for a
future KYC subsystem: `kyc_tier INT` and `verified_at TIMESTAMPTZ`, both
explicitly commented "not enforced or interpreted by anything in Stage
2." Directive §4 is explicit: **do not collapse KYC status into
`PlayerAccountStatus`** and **do not create a second identity model**.
The right shape is therefore a genuinely new, small subsystem - its own
table(s), its own state machine - that hangs off the EXISTING
`Person`/`PlayerAccount`/`Tenant`/`Brand` model via foreign keys, exactly
as Stage 4D-RG's `player_restrictions` and Stage 4E's
`internal/identityresolution` both already did for their own new concepts.

## Decisions

### 1. KYC status is never written to `PlayerAccountStatus`, and vice versa

`kyc_verifications.status` (migration `0040`) is a completely independent
column in a completely independent table. No code path in this stage
ever writes a `kyc_verifications` outcome into `player_accounts.status`,
and no code path reads `player_accounts.status` to decide a KYC outcome.
The two are orthogonal facts about the same account, not a single
boolean, matching directive §15's explicit instruction not to mix
"email verification / identity verification / KYC verification /
responsible gaming restriction / authentication" into one flag.

`player_accounts.verified_at` (Stage 2's own reserved column) is
REPURPOSED this stage for EMAIL verification specifically (a
brand-relationship-level fact - see ADR 0030) - not for KYC identity
verification, which lives entirely in `kyc_verifications`/`kyc_documents`
and never touches `player_accounts` at all. This is a deliberate,
disclosed interpretation of a previously-ambiguous column, recorded here
rather than silently assumed.

### 2. Verification state machine

```
unverified -> pending -> review_required -> approved
                       -> rejected
                       -> expired
```

`kyc_verifications.status` CHECK-constrains exactly these six values.
`suspended`/`blocked` (directive §4's "where applicable" hedge) are
deliberately NOT added this stage: there is no distinct "verification
suspended" concept the platform needs beyond `PlayerAccount.Status`'s own
`suspended` (an account-level fact) and `player_restrictions` (an
RG-level fact) - inventing a third, KYC-specific suspension state with no
concrete trigger for it would be exactly the speculative schema CLAUDE.md
warns against. If a real future requirement needs it, it is a small,
additive CHECK-constraint migration, not a redesign.

### 3. Which facts are platform-wide vs. tenant/brand-specific

`kyc_verifications`/`kyc_documents` are **tenant-owned** (`tenant_id NOT
NULL`, plain `tenant_isolation` RLS - migration `0040`), NOT
platform-wide like `persons` or `player_restrictions`. Both carry
`person_id` purely as a **denormalized, non-enforcing anchor** to the
platform-wide `Person` - never used as an RLS key or a cross-tenant
lookup key this stage.

This is a deliberate, narrower scope decision, not an oversight:

- The verification RECORD (who requested it, under what business
  relationship, reviewed by which tenant's compliance staff) is
  genuinely tenant/brand-specific - a real KYC vendor relationship is
  typically contracted per-tenant, and a tenant's own compliance team
  reviews its own players' evidence.
- Whether an APPROVED verification should ever be reusable platform-wide
  (a Person verified at Brand A being treated as pre-verified at Brand
  B) is an **OPEN DECISION**, not resolved by this schema. Building
  cross-tenant reuse now, with no real evidence source and no legal/
  compliance sign-off on whether that reuse is even permissible across
  a hybrid-licensing platform (some tenants under the platform's own
  licence, others bring-your-own in a different jurisdiction - ADR
  0006), would be exactly the kind of premature, undecided architecture
  CLAUDE.md's "no uncontrolled scope expansion" rule warns against.

### 4. KYC provider interface

```go
type KYCProvider interface {
    ID() string
    CreateVerification(ctx, CreateVerificationInput) (ProviderResult, error)
    GetVerification(ctx, providerReference string) (ProviderResult, error)
    SubmitVerification(ctx, providerReference string, documents []SubmittedDocument) (ProviderResult, error)
    HandleCallback(ctx, rawPayload []byte) (ProviderResult, error)
    GetCapabilities() Capabilities
    HealthStatus(ctx) error
}
```

Mirrors `casino.CasinoProvider`/`payments.PaymentProvider`'s exact shape
(ADR 0025/0022). `SubmittedDocument` carries only `DocumentID`/
`DocumentType` references, never raw bytes - a real adapter retrieves
actual content from this platform's own `DocumentStorageProvider`
separately (ADR 0029), keeping this interface stable regardless of
whether a future vendor wants pull-by-reference, a signed upload URL, or
direct bytes.

### 5. Result normalization

`ProviderResult.Outcome` is one of exactly six normalized values
(`approved`/`rejected`/`pending`/`review_required`/`expired`/`error`) -
never a raw vendor status string. `ProviderResult.Reason` is a short,
non-sensitive, machine-readable code, mirroring
`identityresolution.ResolutionResult`'s identical contract (ADR 0027) -
a real adapter must never put raw KYC evidence into it. `error` is
distinct from every legitimate outcome and NEVER changes a
verification's stored status - `internal/kyc.Orchestrator.ReceiveCallback`
audits it (`OutcomeFailure`) and leaves the row untouched, so a vendor
outage can never corrupt platform state.

### 6. Provider replaceability

Adding a real provider requires exactly: an adapter implementing
`KYCProvider`, its registration in `Orchestrator`'s provider map
(`cmd/platform-api/main.go`), and its own webhook path segment (already
parameterized: `POST /v1/webhooks/kyc/{tenantSlug}/{providerID}`).
Nothing in `Person`/`PlayerAccount`/`kyc.Verification`/`kyc.Document`/
`internal/rg`/`internal/wallet`/`internal/casino` needs to change.
Callback tenant resolution and payload authentication follow the EXACT
precedent `casino.Orchestrator.ReceiveCallback`/
`payments.Orchestrator.ReceiveCallback` already established (ADR 0025
§5/§7): the HTTP layer resolves tenant from the URL's tenant slug only,
never trusts the payload for it, and payload signature verification
happens inside the adapter's own `HandleCallback`, never generically at
the HTTP layer.

`MockKYCProvider` is the only implementation shipped. Its callback
payload is a platform-invented minimal JSON shape, HMAC-signed with a
dev-only secret - explicitly NOT modeled on any real vendor's actual
webhook format, so it never has to be reconciled with (or accidentally
become) a real vendor's contract before one exists.

### 7. Cross-tenant reuse and Person-resolution integration — OPEN DECISIONS

Recorded explicitly, not resolved here:

- **Should an approved verification ever apply platform-wide to the
  Person**, letting a second tenant's brand treat the player as
  pre-verified? Requires product/legal/compliance input on data-sharing
  across tenants (especially bring-your-own-licence ones) before any
  schema or RLS change.
- **Should a future real KYC verification supply `VerifiedAttributes` to
  `internal/identityresolution.PersonResolver`** (Stage 4E), closing the
  "no real evidence source" gap ADR 0027 left open? This stage does NOT
  wire that connection - `kyc_verifications` and `identityresolution` do
  not reference each other at all today. The connection point (a future
  `PersonResolver` implementation reading approved `kyc_verifications`
  rows, or a real vendor's own identity-matching output, to populate
  `VerifiedAttributes`) is a natural next step, not implemented here, and
  requires the SAME care ADR 0027 already documented: never implement
  heuristic matching, never automatically merge historical Persons.

## Specialist review findings and fixes

See `docs/progress.md`'s Stage 4F entry for the full itemized review pass
(identity architecture, security/privacy, document security, KYC
provider architecture, PostgreSQL/RLS, API/RBAC, adversarial testing) and
every P0/P1 finding raised and fixed before this stage was considered
complete.
