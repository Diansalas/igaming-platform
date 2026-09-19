# Stage 4I — Security Phase 4: the security-control model for platform-wide jurisdiction resolution

**Status of this document: RULINGS inside `security`'s own authority, plus
requirements routed to later phases.** This is `security`'s contribution to
Stage 4I's chain (`architect` reconnaissance → `identity-compliance` →
`risk` → **`security`** → `backend` → `casino` → `bonus-engine` →
`payments` → `sportsbook` → `qa` → `architect` final → independent
security/compliance final), per the directive. It is the **last
design-phase voice before `backend` implements**, and its purpose is to
close the remaining security-control questions so that `backend` has a
fully-specified contract rather than a set of principles.

Items **S-1** (C-4), **S-2** (audit/provenance content), **S-3**
(RLS/tenant-isolation contract), **S-4** (H-2 lock ordering), **S-6**
(adversarial test specification) and **S-7** (caching) are **rulings**
inside this specialist's stated Authority — session/auth architecture,
RBAC enforcement, tenant-isolation enforcement, PII handling in audit
stores, and the authorization/tenant-isolation tests every domain's `qa`
coverage must include. **S-5** is a **review** of `risk`'s recommendation
to `casino`, confirming it and adding what a pure security-control read
finds that a risk-domain read would not.

**No code, migration or schema was modified to produce this document.**
Nothing here is implemented. Every claim marked "confirmed" was checked
against the live repository at HEAD `61c020e` (branch
`claude/focused-wright-jw88w9`), not inherited from a prior report's
summary.

This document **cross-references rather than restates**:

- `docs/governance/stage-4i-reconnaissance.md` — hereafter **"recon"**.
  Every bare `§N`, `C-N`, `K-N`, `P-N`, `Q-N`, `HDR-J-N` is to that
  document.
- `docs/governance/stage-4i-identity-compliance-model.md` — hereafter
  **"IC"**, cited `IC §N`.
- `docs/governance/stage-4i-risk-model.md` — hereafter **"RISK"**, cited
  `RISK §N` / `R-1` / `R-2` / `H-1`…`H-3`.
- `docs/security/security-architecture.md` — hereafter **"the security
  doc"**, cited by its own section ids (`§B1.3`, `§B1.4`, `§B1.5`,
  `§W15.1`, …). **This document does not duplicate those sections; it
  extends them.** On final acceptance, §S-2/§S-3/§S-6 below should be
  folded into the security doc as a new numbered section by whoever owns
  that merge — `architect`'s synthesis call, not made here.

**HDR discipline.** None of HDR-J-1 … HDR-J-6 is decided here. §9 states
this domain's security-relevant *position on*, or *binding constraint
upon any answer to*, four of them, exactly as IC §3 and RISK §7 did. The
decisions remain the orchestrator's.

**Rulings that build on, and do not re-litigate, RISK.** R-1 (Risk's
conditional fail-closed is correct and retained), R-2 (no shadow mode;
an authoring-time precondition instead), and RISK §3.1 (Risk consumes an
opaque code for matching; confidence thresholds live in the resolver per
operation class) are taken as settled for the purposes of this document
and are endorsed where this document depends on them. RISK §1.4's
correction to C-3 — **two correct specialisations of one rule, plus two
genuine defects** — is likewise accepted and is the framing §S-5 works
from.

---

## S-1 — RULING on C-4: jurisdiction is never a request-body field on an enforcement-facing surface

### S-1.1 The ruling

**Ruling JV-1 (binding).** Every jurisdiction value on this platform is
exactly one of two things, and never both:

- **(a) A scope declaration on a configuration row** — "this rule /
  this authorization / this blocklist entry applies *to* jurisdiction X."
  This is staff-authored by design, is legitimately carried in a request
  body, and is controlled by RBAC + FK + audit + (per R-2b) an
  authoring-time precondition.
- **(b) A resolved fact about an operation** — "*this* operation, by
  *this* player, is governed by jurisdiction X." This is **server-resolved
  only**, from the authenticated context and stored platform facts. **No
  request body, header, query parameter or path segment — staff-supplied
  or player-supplied — may ever carry a class (b) value.**

**Ruling JV-2 (binding).** Every Bonus admin request struct that carries
`jurisdiction_code` today carries a class (b) value. **The field is
removed entirely; it is not kept-and-cross-validated.**

**Precision correction to recon C-4, confirmed at HEAD: there are FOUR
such structs across FIVE handler surfaces, not five structs.** Recon C-4
lists six line references
(`bonus_handlers.go:399`/`:633`,
`bonus_domain_ops_handlers.go:497`/`:546`/`:719`/`:806`), but `:546` and
`:806` are *call sites*, not struct fields, and
`newIssueManualGrantRequestHandler`
(`bonus_domain_ops_handlers.go:402`) **reuses** `issueManualGrantRequest`
rather than declaring its own. The distinction matters for JV-2's
execution: removing the field from `issueManualGrantRequest` changes
**two** endpoints at once, and a migration plan written against "five
structs, five endpoints" will mis-sequence.

| # | Struct | Declared | Field | Handler surfaces | Consumed by |
|---|---|---|---|---|---|
| 1 | `issueManualGrantRequest` | `internal/httpserver/bonus_handlers.go:389` | `:399` | `newIssueManualGrantHandler` (`bonus_handlers.go:413`) **and** `newIssueManualGrantRequestHandler` (`bonus_domain_ops_handlers.go:402`) | `bonus.IssueSingleManualGrant` (`bonus_handlers.go:479`); `bonus.IssueManualGrantRequest` (`bonus_domain_ops_handlers.go:462`) |
| 2 | `resolveHeldDispositionRequest` | `internal/httpserver/bonus_handlers.go:625` | `:633` | `newResolveHeldDispositionHandler` | `bonus.ResolveHeldDispositionAction` (`:685`) |
| 3 | `activateManualGrantRequest` | `internal/httpserver/bonus_domain_ops_handlers.go:495` | `:497` | `newActivateManualGrantHandler` | `bonus.ActivateManualGrantWithApproval` (`:546`) |
| 4 | `executeBulkGrantJobRequest` | `internal/httpserver/bonus_domain_ops_handlers.go:718` | `:719` | `newExecuteBulkGrantJobHandler` | `bonus.ExecuteBulkGrantJobWithApproval` (`:806`) |

All five surfaces feed `bonus.GateCheckpoint`
(`internal/bonus/eligibility.go:79-120`), which feeds both
`assetregistry.CheckEligibility` layer 6 and `risk.Evaluate`. They are
enforcement-facing without qualification.

**Ruling JV-3 (binding).** The *configuration* surfaces keep their field
and are explicitly **not** in scope of JV-2:
`createRuleRequest.JurisdictionCode` (`internal/httpserver/risk_handlers.go:127`,
a `risk_rules` scope), `AuthorizeScope`'s jurisdiction
(`internal/assetregistry/authorization_admin.go:68`, an
`asset_authorizations` scope), and `casino_games.jurisdiction_blocklist`
on the catalogue upsert (`internal/httpserver/casino_admin_handlers.go:87`).
These are class (a). They acquire their own controls in §S-2.4 and §S-5.4,
not JV-2's removal.

### S-1.2 Why removal, not cross-validation — argued against this platform's own `tenant_id` pattern

The alternative on the table was "keep the field, resolve server-side
anyway, reject on mismatch." **Rejected**, for five reasons, four of them
grounded directly in how this codebase already treats `tenant_id`.

**1. The platform's established pattern for an authoritative scope value
is the absence of a parse path, not validated presence.**
`internal/tenant/tenant.go`'s package doc states it outright: "this
package is the only sanctioned way to read or attach it — **there is
deliberately no function here that accepts a tenant id from a header,
query parameter, or request body**." There is no
`tenant.FromRequestBody(...)` that validates and rejects on mismatch;
there is simply no such function. JV-2 is the identical construction
applied to jurisdiction. A cross-validating variant would make
jurisdiction the *only* enforcement-scoping dimension on this platform
that has a client-facing input at all.

**2. `tenant_id`'s backstop is two independent layers, and
cross-validation supplies only the weaker one.** Layer one is the absent
parse path above. Layer two is
`assetregistry.assertTenantScope` (`internal/assetregistry/authorization.go:199-212`),
which compares the caller-passed tenant against the transaction's own
`app.tenant_id` GUC and returns `ErrTenantContextMismatch` — a backstop
that exists precisely because "a handler that read a tenant id out of a
request body would produce a decision about a tenant the caller never
proved it was" (`authorization.go:96-100`). Cross-validation gives
jurisdiction layer two only. Layer two is a *backstop*; it was never
intended to be the primary control, and the code says so.

**3. Cross-validation manufactures a fail-open branch that does not
otherwise exist.** A handler holding both a request-body value and a
resolver output has three states, not two: agree, disagree, and
*resolver-unavailable-but-field-present*. The third state is the whole
problem. Under incident pressure — the resolver is degraded, manual
grants are failing, a support queue is backing up — "just trust what the
operator typed, they're authenticated staff" is a one-line change with an
obvious business justification and no obvious security tripwire. Removal
makes that change impossible to write without also re-adding an API
field, a schema-visible act that shows up in review. This is the same
reasoning `docs/security/security-architecture.md` §B1.3 uses to refuse a
`tenant_id IS NULL` policy arm for bonus tables: do not build the arm of a
conditional that only a bug can select.

**4. `decodeJSON` already gives removal the loud-failure property for
free.** `internal/httpserver/json.go:11-16` calls
`dec.DisallowUnknownFields()`. Deleting the struct field therefore does
not cause a submitted `jurisdiction_code` to be *silently ignored* — it
causes the request to fail decoding with a 400. Removal is strictly
better than cross-validation on the very axis cross-validation was
proposed to win (detecting a caller that still sends one), and it is
better **at zero implementation cost**, because the mechanism already
exists and is already applied to every handler in this package. This is
the single most decisive argument and it is purely a fact about the
current code.

**5. An API field is a template.** Recon §3's P-3 records five structs;
they are five copies of the first. A kept-but-validated field is copied
into the sixth admin surface by an author who sees it as the house
pattern, and the validation is what gets dropped in the copy. Removal
leaves nothing to copy.

### S-1.3 The mandatory mechanism, in three layers

Every jurisdiction-consuming enforcement path must have **all three**. One
or two is not JV-1 compliance.

**Layer 1 — no parse path (API + type system).**
- The field is absent from every enforcement-facing request struct;
  `DisallowUnknownFields` turns a submitted one into a 400.
- The resolver package exposes **no exported constructor for a resolution
  value**. The resolved record's fields are unexported, or the type is
  produced only by `Resolve(...)`, so that no caller anywhere in the
  process can synthesise `Resolution{Code: "MT"}` from a string it
  received. This is the in-process analogue of
  `tenant.WithContext`'s "only internal/auth's middleware should call
  this." **Requirement for `backend`: a struct literal of the resolved
  type must not compile outside the resolver package.** This is
  mechanically checkable and is not a comment.
- Resolution inputs are exactly: tenant from `tenant.FromContext`, the
  player account identified by the authenticated subject or by a
  path-parameter id that is itself validated against the tenant scope,
  the brand derived from the player account row (the existing pattern —
  `internal/httpserver/casino_handlers.go:183` uses `account.BrandID`,
  never a body field), and the operation class, which is a compile-time
  constant at each call site, never a string from the wire.

**Layer 2 — context cross-check (runtime).**
- The resolver asserts the transaction's `app.tenant_id` GUC matches the
  tenant it is resolving for, by the same query
  `assertTenantScope` uses (`authorization.go:199-212`). A resolution
  produced on a transaction with no tenant scope, or a mismatched one, is
  an error and therefore a denial — never a resolution.
- Where a resolution is carried from the point of resolution to a gate
  (§S-4 requires this to cross a transaction boundary), the carrier
  records the tenant, brand and player it was resolved for, and each
  consuming gate re-asserts those against its own authenticated context
  before use. A resolution for player X must be structurally unusable in
  an operation for player Y.

**Layer 3 — database (structural).**
- Every persisted jurisdiction value carries `FOREIGN KEY ... REFERENCES
  jurisdictions (code)` (migration 0042's precedent for
  `casino_launch_sessions.jurisdiction_code` and
  `risk_rules.jurisdiction_code`) or `jurisdictions (id)`. An
  unregistered code can never be stored, which closes half of RISK §3.3's
  "unresolved vs. unknown code" collapse at the schema level.
- Operation snapshots are **write-once**, by the immutability-trigger
  pattern migration 0042 already uses.
- Surfaces that are *not yet* resolution-active keep the migration-0033
  precedent — `CHECK (jurisdiction_code IS NULL)` on
  `withdrawal_policies`, with the accompanying handler comment at
  `internal/httpserver/withdrawal_policy_handlers.go:39-47` ("this API
  must not let an admin write a value that would silently never take
  effect"). **That handler is the correct model already in the
  repository**, and `withdrawal_policy_handlers.go` should be cited by
  `backend` as the reference implementation of JV-1 rather than the Bonus
  handlers.

### S-1.4 Sequencing — removal must land *with* the resolver call, never before

This is a consequence `backend` must not discover late. Today, a
staff-supplied `jurisdiction_code` on those five surfaces is **the only
way a non-empty jurisdiction reaches a gate in production at all** (recon
§3, P-3): without it, `bonus.resolveJurisdictionID`
(`internal/bonus/eligibility.go:139-152`) returns `uuid.Nil`, and
`CheckEligibility` denies unconditionally at
`ReasonJurisdictionContextMissing` (`authorization.go:83-85`).

**Therefore: removing the field before the resolver is wired into those
five handlers converts "manual grant issuance works when staff supply a
code" into "manual grant issuance always denies."** That is fail-closed,
so it is not a security regression — but it is an availability regression
on a staff remediation path, and it must be a deliberate, sequenced
choice, not a surprise.

**Binding sequencing requirement:** JV-2's removal and the resolver call
site land in the **same change**, per handler. Until that change lands for
a given handler, the interim control in §S-1.5 applies.

### S-1.5 Interim control (applies from now until JV-2 lands): audit the staff-supplied value as staff-supplied

**Finding SEC-4I-F2 — severity MEDIUM, confirmed at HEAD.** The one live
producer of a real jurisdiction value on this platform is not recorded
anywhere. `newIssueManualGrantHandler`'s audit entry
(`internal/httpserver/bonus_handlers.go:492-497`) writes
`Action: "bonus_grant.manual_issue"` with
`Metadata: {"reason_code", "parent_operation_id"}` — **and not
`jurisdiction_code`**. `bonus_grants.jurisdiction_code` (migration 0057)
is likewise never assigned (recon §1.1 item 12, C-5). So a manual grant
issued today under a staff-chosen regulatory scope leaves **no record of
which scope was chosen**, by anyone, anywhere.

*Failure scenario, concretely:* a promotions operator issues a manual
grant with `"jurisdiction_code": "MT"` for a player the tenant serves
under `KM-ANJ`. The grant passes the `asset_authorizations` layer-6 check
because an MT row exists for that tenant/asset. Six months later a
regulator asks which jurisdiction's bonus rules governed that grant. The
answer is not recoverable from `audit_log`, from `bonus_grants`, or from
anything else — the value existed only in a request body that was never
persisted.

**Interim requirement (for whichever phase touches these handlers first,
ahead of JV-2):** add `jurisdiction_code` to the manual-grant,
manual-activate, held-disposition-resolve and bulk-execute audit
metadata, **explicitly labelled as staff-supplied** — e.g.
`"jurisdiction": {"code": "MT", "basis": "staff_supplied"}`. The `basis`
label is not cosmetic: it is what lets a later reader distinguish records
written before JV-2 from records written after, without which the
post-resolver audit trail is contaminated by values of unknown
provenance. This is cheap, is correct independently of Stage 4I's
outcome, and is the same one-line improvement RISK §3.2 recommends for
`evaluateAndAuditRisk`'s denial metadata.

### S-1.6 Where staff legitimately need to influence jurisdiction

`security` **endorses IC §6's argument 3 without reservation**: the
legitimate need is to correct the *underlying identity fact*, audited,
which then flows through the same resolver every other caller trusts —
not a per-call override that leaves no durable fact for the next
operation to be consistent with.

Two additions this domain owns:

1. **A correction to a player's jurisdiction-determining fact is an
   enforcement-relevant mutation and must be audited with actor, tenant,
   target, before/after and a reason code** (CLAUDE.md's audit rule, no
   exception available). The *value* recorded is governed by §S-2.3.
2. **Recommendation, not a ruling:** such a correction should be a
   four-eyes-eligible operation class, reusing the existing
   `bonus_approval_policies` / `withdrawal_policies` threshold pattern
   rather than a new mechanism. It changes which regulator's ruleset
   governs a real account, which is a larger blast radius than several
   operations that already require dual control. This is gated on HDR-J-3
   (the fact may not be collected at all) and is routed to
   `identity-compliance` + the orchestrator, not decided here. **No new
   `StaffRole` is proposed by this document.**

### S-1.7 One new permission is implied, and only one

Recon's P-11 records that `jurisdictions` has **no production write
surface at all**, and §5 makes registry-writability a hard prerequisite of
any resolver. That write surface needs a permission.

**Requirement:** a single new **platform-only** permission for the
platform jurisdiction registry (`jurisdictions`, and `licences` if a write
surface for it is built), following `PermCasinoCatalogueManage`'s exact
precedent (`internal/auth/permission.go:76-82`) — "deliberately its own,
platform-only permission, never granted to `RoleTenantAdmin`" — and
granted only to `RolePlatformAdmin`. `jurisdictions` is a platform fact
with no `tenant_id` and no RLS (migration 0002), structurally identical to
`assets` and `casino_games`; a tenant-scoped role must never write it,
for the same reason a tenant must never add a title to the shared
catalogue.

Any **per-tenant** jurisdiction configuration write surface
(`tenant_jurisdiction_configs`, and RISK §2.4b's per-`(tenant, operation)`
resolution-active fact) is tenant-scoped and belongs with
`PermAssetAuthorizationWrite`'s precedent (`permission.go:179-191`):
granted to `RoleTenantAdmin`, never to `RolePlatformAdmin`, "a platform
principal has no tenant scope to write these rows in, and RLS would
reject the write anyway."

**No other permission and no new role is authorized by this ruling.**
Naming the exact permission strings is `backend`'s, subject to
`security` review of the role wiring before it lands — the same review
gate §B1.1 applied to the bonus permissions.

---

## S-2 — RULING on the resolution record's audit and provenance content

### S-2.1 Two artefacts, deliberately separate

The directive requires recording "who/what requested resolution, tenant,
brand where applicable, source, resolved jurisdiction, outcome, timestamp,
relevant policy/version." That is **not** a single `audit_log` row per
resolution, and building it as one would reintroduce a problem this
platform has already reasoned about twice.

**Ruling AR-1 (binding): the per-operation resolution record lives in its
own append-only, tenant-scoped table (working name
`jurisdiction_resolutions`), not in `audit_log`.** `audit_log` receives a
jurisdiction entry only for the four event classes in §S-2.4.

Reasons:

1. **Volume.** Resolution happens on every launch, every bet, every
   deposit, every gate checkpoint. The security doc §B1.4's volume note
   already flags `bonus_grant.progressed` as a per-bet-class event that
   would dominate `audit_log`; jurisdiction resolution is strictly more
   frequent. RISK §2.5 refused to audit ALLOW decisions for exactly this
   reason ("an audit-volume and throughput regression on the single path
   CLAUDE.md's financial rules are most protective of").
2. **Access control.** `audit_log` is readable by any `audit:read`
   holder in the tenant (migration 0014's `dual_scope_isolation`
   policy). A resolution record contains the *basis* selected and the
   bases *rejected*, which is KYC/identity-adjacent. It must be behind
   its own read permission, not inside a store whose read permission was
   scoped for a different purpose. This is the §B1.3 rule —
   "`bonus:read` must never become a side channel around
   `verification:read`" — applied to jurisdiction.
3. **Referential integrity.** An operation row
   (`casino_launch_sessions`, `bonus_grants`, a future withdrawal or
   payment row) must be able to carry a **foreign key to the resolution
   that governed it**. `audit_log.target_id` is `TEXT` with no FK and is
   not a usable anchor.

**Ruling AR-2 (binding): every operation that consumes a jurisdiction
persists a reference to the resolution that governed it, inside the same
transaction as the effecting write.** The two snapshot columns that exist
(`casino_launch_sessions.jurisdiction_code`,
`bonus_grants.jurisdiction_code`) stay — they carry the *code*, which is
what the immutability triggers and FKs already protect — and gain a
`jurisdiction_resolution_id` FK alongside. Storing only the code and not
the resolution id would preserve C-5's defect in a new form: you would
know *which* jurisdiction, but never *on what basis*.

### S-2.2 What MUST be recorded

Per resolution, in `jurisdiction_resolutions`:

| Field | Notes |
|---|---|
| `id` | FK target for the operation rows (AR-2) |
| `tenant_id` | NOT NULL, RLS key (§S-3) |
| `brand_id` | nullable, composite FK `(brand_id, tenant_id) → brands (id, tenant_id)` |
| `player_account_id` | nullable (a system sweep may have one; a config-time resolution may not), composite FK to `(id, tenant_id)` |
| `operation_class` | the class the confidence threshold was applied for (RISK §3.1) — an enum, never a free string from the wire |
| `requested_by_actor_type` / `requested_by_actor_id` | `staff` \| `player` \| `service` \| `system`, matching `audit_log`'s existing `CHECK` vocabulary (migration 0014). For `system`, actor id NULL and the **job name** recorded (`deposit_sweep`, `cashback_scheduler`) — recon's P-4 sweeps are exactly the case this must cover |
| `outcome` | `resolved` \| `unresolved` \| `refused` — three states, never two. `unresolved` is a first-class recorded outcome, not an absent row |
| `jurisdiction_code` | the resolved code, NULL iff outcome ≠ `resolved`, FK to `jurisdictions (code)` |
| `selected_basis` | the enum value only (`player_declared_residence`, `player_verified_residence`, `tenant_licence`, `retail_node`, …) |
| `considered_bases` | **enum values with per-basis status only** (`selected` / `rejected_lower_precedence` / `unavailable` / `disagreed`), never the values those bases held. See §S-2.3 |
| `confidence_class` | the bucket/enum the resolver applied for this operation class, never a raw vendor score |
| `resolver_policy_version` | the version of the precedence configuration applied — without this, a decision made under an older precedence rule is not reproducible |
| `registry_version` / `config_effective_from` | which `tenant_jurisdiction_configs` / `licences` row was in force |
| `as_of` | `TIMESTAMPTZ NOT NULL` — load-bearing for §S-4's staleness bound |
| `created_at` | append-only insert time |

`refused` vs. `unresolved` is a deliberate distinction: `unresolved` means
"the inputs do not determine a jurisdiction"; `refused` means "the
resolver declined to answer" (scope mismatch, unavailable dependency,
precondition failure). They have different remediations and must not be
collapsed.

### S-2.3 What MUST NOT be recorded — and the principle that makes the boundary non-arbitrary

**The governing principle, stated first so the list below is derivable
rather than memorised:**

> **A resolution record persists the DECISION and REFERENCES to its
> evidence. It never persists the evidence VALUES.** Reconstruction of a
> decision is performed by re-reading the referenced evidence under that
> evidence's own access control and retention rule — never by reading a
> copy of it that was made into a store with different access control and
> a longer retention.

This resolves what would otherwise look like an inconsistency, and it is
worth stating explicitly because a careless reader will raise it: *the
resolved `jurisdiction_code` is itself recorded, and when residence is
the selected basis, it approximately discloses residence.* That is
unavoidable and correct — the jurisdiction **is** the decision, C-5 is
the finding that not recording it is a defect, and a decision that cannot
be reconstructed cannot be defended to a regulator. What is **not**
unavoidable, and is therefore forbidden, is additionally recording the
raw inputs, and the values of bases that were considered and *not*
selected. Recording that a `kyc_corroboration` basis was consulted and
disagreed is a decision fact. Recording that it said `"UA"` is a copy of
a KYC-derived personal attribute, in an append-only store, about a
determination that did not even use it.

**The prohibited list. An implementation that records any of these is a
blocking finding.** This extends the security doc §B1.4's existing
never-log list, which continues to apply in full (credentials, tokens,
PAN, keys, segment membership, full Risk/RG rule rows).

1. **`kyc_documents.issuing_country`'s value — never, under any
   circumstance, including when it was a corroborating input.** Record
   instead: `basis = kyc_corroboration`, `evidence_ref =
   kyc_documents.id` (a uuid, in a tenant-owned RLS-protected table,
   readable only by `verification:read`), `document_type` (which is
   already needed, because IC §2 bullet 3 requires the resolver to treat
   types differently), and the agreement flag
   (`agreed` / `disagreed` / `unavailable`). This is the directive's
   explicit "no sensitive KYC/document evidence in the resolution log"
   requirement, made concrete: the *reference* is loggable, the *country
   value* is not.
2. **The player's declared or verified residence country value**
   (IC §5's `declared_residence_country` /
   `verified_residence_country`, if HDR-J-3 ever authorises them).
   Record `basis`, `player_accounts.id` (already the row's own key),
   and the `captured_at`/`verified_at` timestamp. The current value is
   always readable from the row under that row's own RLS; a copy in an
   append-only multi-year store is a PII duplicate that outlives every
   erasure request made against the original — the identical argument
   §B1.4 makes for segment membership lists
   (`docs/architecture/16-privacy.md`).
3. **Nationality, in any form.** IC §1 recommends it not be collected at
   all this phase; if it ever is, it is evidence, never a recorded value.
4. **Any raw IP address, geo-coordinate, city, ISP, or
   geolocation-vendor payload, as a jurisdiction-evidence field.**
   `audit_log.ip_address`, `sessions.ip_address` and
   `login_attempts.ip_address` (migrations 0012–0014) already capture the
   request's IP under their own retention and access rules. A second copy
   inside a resolution record — and worse, a *derived* precise location —
   is a new, more sensitive category of personal data created as a side
   effect of an audit requirement. Record `basis = geo_signal`,
   `evidence_ref = sessions.id` or the request id, and nothing more.
5. **`persons.person_key_hash`, or any cross-brand identity
   correlator.** `jurisdiction_resolutions` is tenant-scoped and readable
   by tenant staff; `persons` is deliberately platform-scoped and
   RLS-restricted to the platform scope
   (`docs/decisions/0015-persons-platform-scope-access-control.md`, and
   IC §1 Argument 1's whole case rests on not widening that). Putting the
   cross-brand correlator into a tenant-readable table hands every tenant
   a join key for correlating the same human across other operators'
   brands. This is a **cross-tenant privacy leak with no attacker
   required**, and it is the single most likely accidental version of the
   mistake, because a future implementer will want it for reporting.
6. **Full name, date of birth, address, phone, email, document number,
   document image, or document URL** — restated from §B1.4 for the
   avoidance of doubt, because "provenance" is precisely the word under
   which someone will propose attaching them.
7. **Vendor raw responses, verbatim.** If a provider-abstracted signal
   ever exists (recon Q-7), store the mapped enum + the vendor's opaque
   reference id, never the response body. Vendor payloads are
   unbounded-by-contract and routinely contain more personal data than
   the field that was asked for — this is the same rule ADR 0028 already
   applies to KYC vendor responses.
8. **Provider or vendor API credentials, HMAC secrets, or per-tenant
   geolocation-vendor keys** — restated from §B1.4; a resolution record
   names the provider id, never the credential used to reach it.

### S-2.4 The four `audit_log` event classes (and the amplification guard)

`audit_log` — not `jurisdiction_resolutions` — receives an entry for, and
only for:

1. **`jurisdiction_registry.*`** — a write to `jurisdictions`,
   `licences`, or `tenant_jurisdiction_configs`. Staff actor, before/after
   in `Metadata`, reason code required. These are configuration writes
   that change enforcement for every subsequent operation.
2. **`jurisdiction_fact.corrected`** — a staff correction to a player's
   jurisdiction-determining identity fact (§S-1.6). Reason code required,
   before/after recorded **as a reference and a change flag, not as the
   two country values** (§S-2.3 item 2 applies to `audit_log` at least as
   strongly as to the resolution table).
3. **`jurisdiction_resolution_active.changed`** — enabling or disabling
   resolution for a `(tenant, operation)` pair (RISK §2.4b's fact).
   Reason code required. Disabling it is a control-weakening act and must
   be as visible as using it — the §B1.4 rule for
   `bonus_approval_policy.written`.
4. **`jurisdiction.resolver_unavailable`** — resolver/dependency
   unavailability, **aggregated**, per §S-2.5.

Per-request denials caused by an unresolved jurisdiction are **not**
individually written to `audit_log`; they are recorded by the existing
per-domain denial mechanisms (`casino.launch_denied_by_risk_policy`'s
shape, `internal/casino/orchestrator.go:383-392`) plus the
`jurisdiction_resolutions` row with `outcome = unresolved`. The control
still leaves evidence it fired — §B1.4's requirement is satisfied — but
not in the store that cannot absorb the volume.

### S-2.5 Audit amplification is an attack, not just a cost

**This is a security-specific concern neither prior document raises.** If
every resolver failure wrote an `audit_log` row, then an attacker (or a
single degraded dependency) who can make the resolver fail can make the
platform write one immutable, never-deletable row per attempt. `audit_log`
is append-only by trigger (`audit_log_deny_mutation`, migration 0014) —
rows cannot be pruned by the application even deliberately. A sustained
failure therefore converts a transient outage into **permanent,
unreclaimable storage growth on the platform's most retention-sensitive
table**, and drowns genuine security events in noise during exactly the
window when someone is reading them.

**Requirement (binding):** `jurisdiction.resolver_unavailable` is emitted
**at most once per `(tenant, operation_class, time_bucket)`**, carrying a
count, never once per failed request. The per-request record stays in
`jurisdiction_resolutions` (a tenant-scoped operational table with its own
retention policy, which *can* be partitioned and aged) and in the
operation's own denial path. `qa` must include a test that N failed
resolutions in one bucket produce exactly one `audit_log` entry.

---

## S-3 — RULING on the RLS / tenant-isolation contract for the new schema

### S-3.1 Universal rules for every new jurisdiction table

These are not negotiable per-table and they follow the security doc
§B1.3's already-established set. `backend` implements against this list;
`qa` asserts it.

- **`tenant_id UUID NOT NULL`** on `jurisdiction_resolutions` and on any
  per-tenant resolution-configuration table. The one exception is the
  platform registry itself — see §S-3.2.
- **`ENABLE ROW LEVEL SECURITY` *and* `FORCE ROW LEVEL SECURITY`.**
  FORCE is the load-bearing half: the application role owns these tables
  and bypasses non-FORCE policies entirely.
- **No `BYPASSRLS` assumption anywhere.** The application role is
  `NOBYPASSRLS` by construction (`deploy/init-app-role.sql`), and every
  read/write goes through `db.Pool.WithTenant` / `WithPlayerScope` /
  `WithPrincipalScope` / `WithoutTenant` (`internal/db/tenant_rls.go`). A
  resolver query issued on a bare pool connection reads zero rows under
  FORCE RLS and is therefore a **silent-wrong-answer** bug — which, for a
  resolver, means a silent `unresolved`, which means a fail-closed denial
  with a misleading cause. `qa` must cover it (§S-6, case G-3).
- **Composite foreign keys, never plain ones.**
  `(brand_id, tenant_id) → brands (id, tenant_id)` and
  `(player_account_id, tenant_id) → player_accounts (id, tenant_id)`
  (migration 0043's precedent, restated in §B1.3). A plain
  `brand_id REFERENCES brands(id)` does not prevent tenant A's resolution
  row from naming tenant B's brand; the composite FK makes cross-tenant
  attachment structurally impossible rather than policy-dependent.
- **Per-command policies. No `FOR ALL` policy anywhere. No DELETE
  policy. No UPDATE policy on `jurisdiction_resolutions`.** The §B1.3
  argument applies with full force here: a resolution row is the *record
  of a decision*; an UPDATE to it is a rewrite of history, and a DELETE
  is the removal of the only evidence that a control ran. Append-only is
  enforced by a `BEFORE UPDATE OR DELETE` trigger
  (`ledger_deny_mutation()`'s pattern), not only by the absence of a
  policy — a trigger is not bypassed by table ownership, which is
  precisely why `audit_log_immutable` exists.
- **A `BEFORE TRUNCATE ... FOR EACH STATEMENT` deny trigger.**
- **No player-read policy on `jurisdiction_resolutions`.** This is
  deliberate and is a constraint on the API design, not an oversight: the
  row records which basis was selected and which were rejected. A player
  who can read it learns exactly which signal the platform trusted and
  which it ignored — directly attack-useful for steering a future
  resolution (§S-6 cases E-1/E-2). Identical reasoning to §B1.3's
  `bonus_progress` ruling. Anything player-facing is a **curated
  server-side projection** (at most: "your account is registered under
  jurisdiction X"), never a passthrough.
- **Staff read is its own permission.** Reading a resolution record is
  not implied by `audit:read`, `bonus:read` or `player:read`. §B1.3's
  side-channel rule applies: a caller without `verification:read` must
  not learn from a resolution record that a KYC basis was consulted,
  disagreed, or was unavailable — those three values are KYC-derived
  facts. **Requirement for `backend`: the `considered_bases` column is
  projected out for a caller lacking `verification:read`.**

### S-3.2 The platform registry stays platform-scoped

`jurisdictions` (migration 0002) has no `tenant_id` and no RLS — the same
shape as `assets` (0003) and `casino_games` (0035), and correctly so: it
is a platform fact and an FK target that every tenant-scoped transaction
must be able to read. **It stays that way.** Adding RLS to it would break
every FK-validating read from a tenant scope.

What changes is only the **write** side (§S-1.7): writes occur under
`WithoutTenant` / `WithPlatformAdmin` with the new platform-only
permission. `qa` must assert that a tenant-scoped transaction can
`SELECT` from `jurisdictions` and cannot `INSERT`/`UPDATE`/`DELETE` — the
same split `internal/assetregistry/registry_admin.go:25` already
documents for `assets`.

`licences` and `tenants.licence_id` are platform-level today and have
never been read or written by production code (recon §10). If Stage 4I
builds a write surface for either, the same platform-only treatment
applies. `tenant_jurisdiction_configs` already carries `ENABLE` + `FORCE`
RLS (migration 0005) and needs no change — only a production reader and
writer, which it has never had.

### S-3.3 The nine required behaviours — the explicit contract for `backend` and `qa`

The directive names nine scenarios. For each: the required correct
behaviour, where it is enforced, and what must **not** happen. "Never
data" throughout means: never another tenant's/brand's/player's row
content, never a partial field, never an error message that discloses
existence.

| # | Scenario | Required correct behaviour | Enforced at | Must NOT happen |
|---|---|---|---|---|
| 1 | **Cross-tenant access** — tenant B's *valid* staff token requests tenant A's resolution record | RLS returns zero rows; the handler surfaces **404** (the platform's existing convention — §B1.5 "403/404, never data"). A resolution for tenant A must also be **unusable** as an input to any tenant B operation, even if an id were guessed | RLS `tenant_isolation_read` on `jurisdiction_resolutions`; plus the §S-1.3 Layer-2 re-assert at each consuming gate | A 200 with data; a 500 whose text distinguishes "exists but forbidden" from "does not exist"; a resolution carried across a tenant boundary in-process |
| 2 | **Cross-brand access** — a staff token scoped to brand B requests, or consumes, a resolution produced for brand A of the same tenant | The composite FK makes a cross-tenant brand attachment impossible; **within** a tenant, brand narrowing is an application-level scope check at the gate, and a brand-mismatched resolution is **refused, not silently widened** | Composite FK `(brand_id, tenant_id)`; §S-1.3 Layer-2 re-assert | A resolution produced for brand A being accepted for a brand-B operation because the tenant matched — the C-2 conflation (configuration has no brand dimension; three enforcement layers do) made live |
| 3 | **Player-scope access** — a player-scoped connection (`WithPlayerScope`, which sets both `app.tenant_id` and `app.player_account_id`) reads `jurisdiction_resolutions` | **Zero rows.** There is no player-read policy (§S-3.1) | RLS: the `app.player_account_id IS NULL` half of each staff predicate | A player reading their own resolution record, including via a "my account" endpoint that passes the row through instead of projecting it |
| 4 | **Forged jurisdiction payload** — any surface receives a jurisdiction value from the wire | **400** from `decodeJSON`'s `DisallowUnknownFields` (the field does not exist), and the server-resolved value is used regardless. Never a 200 in which the supplied value took effect, and never a 200 in which it was silently ignored without the resolver having run | §S-1.3 Layer 1 | The supplied value reaching `GateParams`, `RiskRequest.JurisdictionCode`, `CheckEligibility`'s `jurisdiction` argument, or any persisted snapshot. Full case list in §S-6 |
| 5 | **Forged tenant payload** — a request body carries a `tenant_id`, or a handler passes one through | Tenant comes from `tenant.FromContext` only; `assertTenantScope` (`authorization.go:199-212`) raises `ErrTenantContextMismatch` if any caller-passed tenant disagrees with the `app.tenant_id` GUC. The resolver must perform the identical assertion | `internal/tenant`, `db.Pool.WithTenant`, `assertTenantScope` | A resolution produced for a tenant the caller never proved it was. This is an existing, working control — the requirement is that the resolver **adopts** it, not that it be invented |
| 6 | **Forged brand payload** — a request body carries `brand_id` for a player-scoped operation | Brand is derived server-side from the player account row (the existing pattern: `internal/httpserver/casino_handlers.go:183`, `bonus_handlers.go:275`/`:473` use `account.BrandID` / `*playerAccount.brandID`). A body-supplied brand on an enforcement path is refused | Handler; composite FK as the structural backstop | A body brand narrowing or widening an `asset_authorizations` layer-5 answer or a `risk_rules` brand-scoped match |
| 7 | **Stale jurisdiction context** — a session or a resolution predates a change to the player's determining fact or to the jurisdiction configuration | Every resolution carries `as_of` and a `resolver_policy_version`; the enforcement point **rejects a resolution older than the configured maximum age for that operation class and fails closed** (§S-4.4). A frozen per-round snapshot (`casino_launch_sessions.jurisdiction_code`, write-once under migration 0042's trigger) remains frozen — that is deliberate — but a *frozen* snapshot and a *stale* resolution are different things and must be distinguishable in the record | Resolver + each gate; `as_of` column | An unbounded-age resolution being reused indefinitely; a session established before a jurisdiction change continuing to authorise operations under the old jurisdiction with nothing recording that it did. **Note:** whether the old or new jurisdiction *governs* is HDR-J-4 and is not decided here — what is required here is only that staleness is bounded, detected, and recorded |
| 8 | **Conflicting source** — two bases disagree (e.g. a declared residence and a corroborating KYC document) | Deterministic precedence per the resolver's configuration, keyed per RISK §4.2 on something knowable *before* player-side resolution; the disagreement is **recorded** (`considered_bases[i].status = disagreed`, values omitted per §S-2.3); and where the precedence configuration does not determine an answer, the outcome is **`unresolved`**, never an arbitrary pick | Resolver only — never a gate | A gate re-resolving or overriding (RISK §4.1's one-operation-one-jurisdiction invariant, which `security` endorses); a silent "prefer the more permissive"; a silent "prefer the most recently written" |
| 9 | **Unavailable resolver** — the resolver's dependency (a query, a config row, a future provider) is unavailable | Outcome `refused`; the operation **fails closed** with a distinguishable internal reason code; `audit_log` receives an **aggregated** unavailability entry (§S-2.5); and the player-facing response does **not** distinguish "unavailable" from "blocked" (§S-5.3) | Resolver + each gate + §S-2.5's aggregation | Falling back to a previously-known-good answer (the exact prohibition `authorization.go:30-42` already states for `CheckEligibility`: "no exception and no fallback to a previously-known-good answer"); falling back to a tenant/brand default (that is HDR-J-1 and is **not** an engineering decision); one `audit_log` row per failed request |

---

## S-4 — RULING on H-2: the resolver is read-only on the evaluation path

### S-4.1 Confirmed, and elevated from a correctness constraint to a security one

**`security` confirms RISK §6.3's requirement and adopts it as binding.**
The resolver takes no `FOR UPDATE`, acquires no advisory lock, and
performs no write of any kind while any gate in the chain is running.

RISK framed H-2 as a lock-ordering correctness hazard. It is also, and
this is `security`'s addition, a **remotely triggerable availability
attack**:

- Both candidate lock holders are **player-keyed**. Risk's advisory lock
  key is `fmt.Sprintf("%s:%s:%s:%s:%s", req.TenantID,
  req.PlayerAccountID, req.Operation, r.LimitKind, req.AssetCode)`
  (`internal/risk/evaluator.go:337-340`); a resolve-and-cache row would be
  keyed by player or player+tenant.
- "Two concurrent operations on the same player" is not an exotic
  condition an operator has to engineer. It is **two browser tabs**. Any
  player can produce it at will, deliberately, repeatedly, at no cost.
- Under `evaluator.go:363-377`'s fail-closed contract, the resulting
  `40P01` is an `Evaluate` error and therefore a **DENY of a real bet**.

So the shape is: an unauthenticated-difficulty, zero-cost, client-driven
action that produces deterministic denials on the money path. That is a
denial-of-service primitive, not only a latent deadlock. It raises the
required strength of the constraint from "design guidance for `backend`"
to "a structural property that must be enforced by something other than
developer discipline," which is §S-4.2.

### S-4.2 Refinement 1 — "read-only" must be structurally enforced, not asserted in a comment

A doc comment saying "this resolver must not write" is worth exactly as
much as recon §1.3 showed `LaunchGameParams.JurisdictionCode`'s "MUST be
resolved server-side… NEVER from client-supplied input" was worth: the
comment is correct, and the one production caller never sets the field at
all. Comments do not enforce.

**Requirement for `backend` (binding), three mechanisms, all of them
cheap:**

1. **The resolver does not accept a `pgx.Tx`.** It accepts a narrow
   read-only interface exposing only `Query` and `QueryRow` — no `Exec`,
   no `CopyFrom`, no `Begin`. A write inside the resolver then does not
   compile. This is the same technique §S-1.3 Layer 1 uses for the
   resolution type, and it is the only one of the three that is a
   *compile-time* guarantee.
2. **An integration test resolves inside a `BEGIN ... READ ONLY`
   transaction** and asserts success. Any write, including an incidental
   one added later by an unrelated change, fails that test loudly at the
   database level.
3. **An integration test asserts the resolver holds no locks**: query
   `pg_locks` for the backend pid after a resolution and assert no
   `advisory` lock and no row-level lock attributable to it. This catches
   a `SELECT ... FOR SHARE` that mechanisms 1 and 2 both permit.

`qa` owns 2 and 3; `backend` owns 1.

### S-4.3 Refinement 2 — prefer *outside* the transaction, and say why the TOCTOU trade is acceptable

RISK §6.3 offers `backend` a choice: persistence happens "(a) entirely
outside the guarded transaction, or (b) strictly after the whole gate
chain completes." `security`'s refinement concerns the **resolution
read**, not only the persistence:

**Preferred: resolution runs before the guarded transaction begins.**
Not merely "before the first gate within it." Reasons: it makes the
read-only property observable from outside (a transaction that never
contains resolution cannot be made to contain a resolver write by a later
edit); it avoids pinning the gate transaction's snapshot earlier than
necessary; and it makes mechanism §S-4.2(2) trivially applicable.

**The honest cost, stated rather than glossed:** resolving outside the
guarded transaction creates a TOCTOU window between resolution and the
effecting write. CLAUDE.md requires the authoritative *balance* read to
happen inside the same transaction as the write — and that rule stands
untouched. Jurisdiction is not a balance: it is a scope selector, not a
monetary quantity, which is precisely the distinction RISK §6.5 draws
(`assetExponents` is read in-transaction "so no exponent is ever cached
across transactions"; a cached *exposure total* would be unacceptable in
a way a cached scope selector is not). `security` accepts that
distinction and accepts the window, **on two conditions**, which are
§S-4.4.

### S-4.4 Refinement 3 — the window must be bounded and auditable, not merely tolerated

**Condition 1 — the resolution id is persisted inside the guarded
transaction** (AR-2). The window is then never invisible: for any
operation, you can always answer "which resolution, produced when, under
which policy version, governed this write," even though the resolution
itself was computed a moment earlier. An unbounded *and* unrecorded
window is a compliance defect; a bounded *and* recorded one is a
documented design property.

**Condition 2 — `as_of` staleness is bounded per operation class, and
exceeding it fails closed.** A resolution older than the configured
maximum for the operation class is not usable; the gate refuses and the
operation re-resolves or denies. The bound is configuration, not a
constant, and its default must be conservative. Without this, "resolve
outside the transaction" silently becomes "resolve once at login and
reuse forever," which is §S-3.3 case 7 with no detection.

### S-4.5 Position on doc 34 §5.3's "rule 0"

**`security`'s position: the ordering constraint is MANDATORY. Its
documentation location is `architect`'s call.**

Two notes for `architect`'s synthesis:

1. **Numbering it "rule 0" is slightly misleading and should be worded
   around.** §5.3's rules 1–4 are an *in-transaction lock order*. The
   whole point of this constraint is that jurisdiction resolution is
   **outside** that order — it holds no lock and ideally is not even in
   the transaction. Recommended wording: record it as a **precondition**
   of §5.3's list, stated as *"Jurisdiction resolution completes before
   the guarded transaction begins. It holds no lock, performs no write,
   and therefore does not participate in this ordering at all"* — which
   is stronger than being the first element of the order, because a rule
   that says "this never participates" cannot be reordered, whereas
   "rule 0" invites a future author to ask whether something could
   legitimately precede it.
2. **With the constraint in place, the total order RISK §6.3 wants holds
   and AB-BA stays structurally unreachable** — which is the property
   §5.3 was written to guarantee. `security` confirms RISK's observation
   that casino's existing `CreateLaunchSession` write
   (`internal/casino/launch.go:124`) is already compliant: it is a
   post-gate-chain write, and nothing in `internal/casino` needs to
   change for H-2. The hazard is created only by how the *resolver* is
   built, which is why §S-4.2's enforcement mechanisms all target the
   resolver.

**`security` also endorses H-1 and H-3 unchanged.** H-3's "no network I/O
of any kind between Risk's advisory lock and commit" is the general rule
that §S-7 makes specific for caches.

---

## S-5 — REVIEW of the casino per-game blocklist remediation (K-3 / C-3(c))

### S-5.1 Verdict on RISK §5

**Confirmed sound.** All four of RISK §5.2's items are correct and
`security` supports them as written:

1. Fail closed — remove `params.JurisdictionCode != nil &&` at
   `internal/casino/orchestrator.go:138`. A jurisdiction-*dependent*
   control that is silently not run when jurisdiction is absent is a
   defect, not a contract. It is also the only *fail-open* jurisdiction
   consumer on the platform, on a **legal/market-availability** control —
   the class of control where failing open is a licensing exposure rather
   than a bug.
2. A distinguishable sentinel, never reusing `ErrJurisdictionBlocked` —
   correct, and consistent with casino's own stated discipline at
   `orchestrator.go:109-112`. **With the qualification in §S-5.3.**
3. Single resolution point — required independently by RISK §4.1's
   one-operation-one-jurisdiction invariant, which `security` endorses.
4. Demo-mode decided explicitly — correct that it must be decided;
   `security`'s own reading is in §S-5.2.

`security` also confirms RISK §5.1's conclusion that the blocklist should
**not** be absorbed into `risk_rules`. The argument that settles it from a
security angle is the third one: a single `game_id`-scoped `casino_launch`
rule makes `missingScopeContext`'s operation-wide gate demand a game id on
**every** `casino_launch` request (ADR 0031 §42(b)'s documented P1 trap).
Converting a catalogue fact into a rule row would therefore turn a
per-game availability edit into a platform-wide evaluation precondition —
a far larger blast radius than the defect being fixed.

### S-5.2 What RISK's proposal missed, item 1 — the availability blast radius is now a platform-wide single point of failure

Making K-3 fail closed means **every casino launch now has a hard
dependency on jurisdiction resolution succeeding**. Today, launches
succeed with no jurisdiction at all. After the fix, a resolver that
cannot answer stops the lobby.

`security` still supports fail-closed — a market-availability control
that fails open is worse than an outage, and CLAUDE.md's compliance
section does not offer an availability exception. But the change creates a
new obligation that must be stated before it is discovered in production:

**Requirement (binding on `backend` and `casino`): for this stage, the
resolver must have no external network dependency.** Its inputs are
Postgres rows — `player_accounts`, `kyc_verifications`,
`tenant_jurisdiction_configs`, `jurisdictions`, `licences`,
`tenants.licensing_model`. With that property, resolver availability
**equals database availability**, and fail-closing K-3 adds no new
failure domain: if the database is down, no launch was going to succeed
anyway. That property holds trivially today (confirmed: there is no cache
or message-broker client anywhere in `internal/` — the only match for
"redis" in the tree is an unrelated comment in
`internal/identity/login_attempt.go`), and it must be held **on purpose**,
not by accident.

**Forward constraint:** the day a geolocation or KYC vendor becomes an
input to resolution (recon Q-7), that vendor becomes a **hard dependency
of the casino launch path**. That is a materially different operational
and security posture — a third party can then stop the lobby — and it
requires its own decision, its own timeout/degradation design, and its own
security review. It must not arrive as an implementation detail of a
provider adapter. **Flagged for `architect` and the orchestrator; not
decided here.**

### S-5.3 What RISK's proposal missed, item 2 — the distinguishable error is a feedback oracle, and the fix is to split internal from player-facing

**This is the finding a pure security-control read adds, and it is in
direct tension with RISK §5.2 item 1 unless resolved carefully.**

RISK correctly requires the *internal* sentinels to stay distinguishable
("jurisdiction could not be determined" ≠ "blocked in this
jurisdiction"). Confirmed at HEAD, the current HTTP mapping surfaces that
distinction **to the player**:

```
internal/httpserver/casino_handlers.go:200-206
  ErrGameDisabled | ErrGameNotAvailable -> 404 "game not available"
  ErrJurisdictionBlocked                -> 403 "game is not available in your jurisdiction"
```

Once a real jurisdiction flows, and especially if a geo-derived basis
ever exists, that mapping becomes an **oracle for resolver
manipulation**. An attacker probing whether a VPN, a proxy, a changed
declared residence, or a timing trick altered their resolved jurisdiction
gets a clean, free, unauthenticated-difficulty binary signal from a
single launch attempt: a different status code and a different message
mean the resolution changed. That converts blind manipulation into a
guided search.

**Ruling (binding):**

- **Internal distinctness is preserved exactly as RISK requires.** The
  sentinel, the denial record, the `jurisdiction_resolutions` outcome and
  the operator-facing logs distinguish `jurisdiction_blocked` from
  `jurisdiction_unresolved`. Operability and incident response require
  this, and it is not negotiable.
- **The player-facing response MUST NOT distinguish
  `jurisdiction_unresolved` from `jurisdiction_blocked`.** Both map to a
  single response code and a single message. This is the specific
  distinction that carries attack value, because it is the one that tells
  the attacker whether their manipulation *registered*.
- **The player-facing response MAY continue to distinguish
  "jurisdiction-related denial" from "game not available."** This is a
  graded ruling, not blanket paranoia: the residual enumeration value (a
  player can map the operator's per-game blocklist by iterating the
  catalogue) is low, the blocklist is not a secret, and there is genuine
  consumer-transparency value in telling a player why they cannot play a
  game. `security` is not asking casino to degrade that message.

Stated as one rule for `casino` to implement against: **collapse the
unresolved/blocked pair at the HTTP boundary; keep every distinction
below it.**

### S-5.4 What RISK's proposal missed, item 3 — the write surface becomes a live platform-wide denial control with no dual control

`casino_games` is a **platform-level** table: migration 0035's own header
records "tenant_id, no RLS, same shape as the `assets` registry," and
`jurisdiction_blocklist TEXT[]` lives on it. The write is gated by
`PermCasinoCatalogueManage`, which is correctly platform-only and granted
only to `RolePlatformAdmin` (`internal/auth/permission.go:76-82`,
`:251`) — so the RBAC scoping is right and there is **no** cross-tenant
privilege defect here.

What changes is the **consequence** of that write. Today the blocklist is
inert (recon §1.3: the check is unreachable). After the fix, a single
platform-admin adding one code to one game's array **immediately denies
that game's launches for every tenant and every brand on the platform**,
with no four-eyes, no change request, and — see below — no record of what
changed.

**Finding SEC-4I-F3 — severity MEDIUM, confirmed at HEAD.** The catalogue
upsert's audit entry (`internal/httpserver/casino_admin_handlers.go:92-97`)
writes `Action: "casino_game.upserted"` with
`Metadata: {"provider_id", "provider_game_id", "status"}` — **no
before/after state, and specifically not the `jurisdiction_blocklist`**,
even though `req.JurisdictionBlocklist` is written on line 87.

*Failure scenario:* a platform admin adds `"MT"` to a popular game's
blocklist. Launches for every Malta-resolved player across every tenant
begin failing. `audit_log` records that the game was upserted, by whom, at
what time — and gives an investigator no way to see *what changed* or to
reconstruct the prior value. The remediation path (restore the previous
array) requires a value the platform did not keep.

**Requirements:**

1. **Binding, before K-3 goes live:** `casino_game.upserted` records
   before/after for `jurisdiction_blocklist` (and, while the change is
   being made, for the other enforcement-relevant catalogue fields:
   `status`, `supported_assets`, `demo_supported`). CLAUDE.md's audit
   rule already requires before/after for a mutating administrative
   action; the current entry does not satisfy it for this field. This is
   a prerequisite of fail-closing K-3, not a follow-up: a control that can
   deny platform-wide without leaving a diff is not operable.
2. **Recommendation, not binding:** a blocklist *addition* is a
   denial-widening change and is a reasonable candidate for the existing
   change-governance/dual-control pattern
   (`bonus_change_requests` / `asset_change_requests`). `security` is
   deliberately **not** requiring it — the platform already accepts
   single-actor platform-admin catalogue writes, this would be a scope
   expansion beyond Stage 4I's directive, and CLAUDE.md's
   no-uncontrolled-scope rule applies to security additions too. Routed to
   `casino` and the orchestrator as a named future consideration.

### S-5.5 Security's position on demo mode

RISK §5.2 item 3 correctly requires `casino` to *decide* this rather than
inherit it, and flags that the blocklist check at `orchestrator.go:138`
sits **above** the `params.Mode == ModeReal` branch at `:179`, so
fail-closing it starts denying demo launches too.

`security`'s position, offered to `casino` whose decision it is:

**The question is not "is demo risky." It is "what kind of control is the
blocklist."** RISK §5.1 answers that correctly: it is a **catalogue
availability** fact — *may this game be offered in this jurisdiction*.
Offering, advertising and demo play are regulated activities in several
real regimes, independently of whether money moves. A control that
encodes "we may not offer this title in this market" therefore has no
principled reason to exempt the demo surface, and exempting it would make
the platform's answer to "do you offer this game in market X" depend on
which endpoint you ask.

So `security` leans toward **applying the blocklist to demo as well** —
while noting this doubles the availability blast radius of §S-5.2 and
should therefore be sequenced behind the resolver-availability property.

**What `security` does require, regardless of which way `casino`
decides:** the mode-gating must be **explicit in code, with a comment
stating the reason** — never inherited from whether a line happens to sit
above or below the `Mode == ModeReal` branch. The current arrangement is
an accident of line ordering, and an accident that produces the right
answer is still an accident. This is the same defect shape RISK §6.2
names for `assetExponents` ("avoids this by accident, not by contract").

---

## S-6 — RULING: what "forged jurisdiction payload" adversarial testing must cover

Specified per `.claude/agents/security.md`'s testing responsibility. These
are **mandatory** for Stage 4I's `qa` gate; the stage is not complete
without them. They extend, and do not replace, the security doc §B1.5
list. Each case names the code it targets so `qa` is not starting from
"test forgery."

### A. Payload forgery — the value arrives from the wire

- **A-1.** `POST /v1/admin/bonus/grants` (and each of the other four JV-2
  surfaces) with `"jurisdiction_code": "MT"` in the body returns
  **400** after JV-2 lands, via `decodeJSON`'s `DisallowUnknownFields`
  (`internal/httpserver/json.go:14`). **One case per handler surface —
  five cases, not four**, even though `issueManualGrantRequest` backs two
  of them: routing, not the struct, is what a regression would break,
  and §S-1.2's argument 5 is that these get copied.
- **A-2.** The same five surfaces, with the field absent: the operation
  proceeds using the **server-resolved** jurisdiction, and the persisted
  `bonus_grants.jurisdiction_code` / `jurisdiction_resolution_id` match
  the resolver's output — not any value the test could have supplied.
  (A-1 without A-2 would pass against a handler that simply ignores
  jurisdiction entirely.)
- **A-3.** A jurisdiction value supplied as an **HTTP header**
  (`X-Jurisdiction`, `X-Forwarded-Country`, `CF-IPCountry`), a **query
  parameter** (`?jurisdiction_code=MT`), and a **path segment**: all
  ignored, with the resolved value unchanged. Headers are the case
  `DisallowUnknownFields` does not cover, and `CF-IPCountry`-shaped
  headers are exactly what a future geo implementation would be tempted
  to trust.
- **A-4.** A jurisdiction value nested inside an **otherwise-legitimate
  JSON field** — e.g. a `reason_code` or `metadata` blob containing
  `jurisdiction_code` — never reaches any gate.
- **A-5.** `casino.LaunchGameParams.JurisdictionCode`
  (`internal/casino/orchestrator.go:66-81`) is populated **only** by the
  server-side resolver in the handler at
  `internal/httpserver/casino_handlers.go:182-184`. A unit/integration
  test asserts there is no path from request body to that field. This is
  the field whose own doc comment says a client-influenced value "would
  let a player pick a jurisdiction that dodges a jurisdiction-scoped
  HARD_LIMIT" — the test that the comment is true has never existed.

### B. Context forgery — the value arrives from a manipulated identity

- **B-1.** A **valid** staff token for tenant B cannot cause a resolution
  for a player of tenant A: 404/403, never a resolution, never data
  (§S-3.3 case 1).
- **B-2.** A resolver invoked on a transaction with **no** `app.tenant_id`
  GUC (a bare pool connection) **errors** — it does not return
  `unresolved`. These are different outcomes with different remediations,
  and under FORCE RLS a bare connection's reads return zero rows, which
  an unwary implementation would report as "no residence on file."
- **B-3.** A resolver invoked with a tenant argument that disagrees with
  `app.tenant_id` raises the `ErrTenantContextMismatch` analogue
  (mirroring `assertTenantScope`, `internal/assetregistry/authorization.go:199-212`).
- **B-4.** A player JWT for player X cannot produce, read, or consume a
  resolution for player Y — same tenant **and** cross-tenant.
- **B-5.** A **platform-scoped** token (nil tenant,
  `docs/decisions/0011-platform-scoped-identity-tokens.md`) cannot
  resolve a player jurisdiction at all: there is no tenant scope in which
  the player's facts are readable, and the attempt must fail loudly
  rather than resolve against an empty read.

### C. Temporal — racing a change

- **C-1.** A resolution produced **before** a
  `tenant_jurisdiction_configs` row's `effective_from`/`effective_to`
  boundary is not reused past its staleness bound; the operation
  re-resolves or fails closed (§S-4.4 condition 2).
- **C-2.** A request **racing a jurisdiction-configuration change**: a
  config write commits between resolution and the gate chain. The
  operation must either use one consistent resolution (recorded, with
  `as_of` and `resolver_policy_version`) or fail closed — never a mix
  where one gate saw the old configuration and another the new. This is
  RISK §4.1's invariant under concurrency and is the concrete version of
  §S-3.3 case 7.
- **C-3.** A request on a **session established before** the player's
  determining fact changed: the operation does not silently authorise
  under the pre-change jurisdiction beyond the staleness bound, and the
  record shows which resolution governed. *(Which jurisdiction **should**
  govern is HDR-J-4 and is not asserted by this test — the test asserts
  only that the reuse is bounded, detected and recorded.)*
- **C-4.** A **frozen per-round snapshot** is not confused with a stale
  resolution: `casino_launch_sessions.jurisdiction_code` stays immutable
  (migration 0042's trigger rejects the UPDATE) and a bet in that round
  uses the frozen value, with the record distinguishing "frozen by
  design" from "stale."
- **C-5.** RISK §1.6's stuck-round hazard: a jurisdiction-scoped
  `casino_bet` rule authored *after* a launch with a NULL snapshot
  produces `ErrMissingJurisdiction` on the next bet in that round, and
  the round is not completable. After R-2b's authoring-time precondition
  lands, the rule **cannot be authored** and the hazard is unreachable.
  `qa` should test both the hazard (pre-fix) and its unreachability
  (post-fix).

### D. Provenance forgery — the value is fabricated in-process

- **D-1.** A resolution value cannot be constructed outside the resolver
  package (§S-1.3 Layer 1). This is a compile-fail test — an
  `// ERROR:`-style vet/build assertion or a deliberately
  non-compiling example file — not a runtime assertion.
- **D-2.** A resolution produced for `(tenant A, player X, brand B1)` is
  refused when presented to a gate running for a different tenant,
  player, or brand (§S-1.3 Layer 2). Three cases.
- **D-3.** A fallback value (if HDR-J-1 ever authorises one) never
  reaches `RiskRequest.JurisdictionCode` as if it were a resolved player
  jurisdiction — RISK §7's binding constraint on any HDR-J-1 answer.
  Until J-1 is answered, the assertion is that **no fallback exists**:
  a sweep with no resolvable player jurisdiction denies, and the
  `deposit_sweep.go:435-449` disclosed-limitation behaviour is preserved
  exactly.
- **D-4.** RISK §3.3's latent fail-open:
  `bonus.resolveJurisdictionID` (`internal/bonus/eligibility.go:139-152`)
  returns `uuid.Nil` for **both** an empty code and an *unknown* code. A
  test must distinguish them: an unknown-but-non-empty code must **not**
  resolve to ALLOW at Risk (where it would match zero rules) while
  slipping past the empty-check gate. Whatever shared translator replaces
  it must keep the two distinct and both non-ALLOW.

### E. Oracle / feedback channels

- **E-1.** The player-facing response for `jurisdiction_unresolved` is
  **byte-identical** to the response for `jurisdiction_blocked` — same
  status code, same message, same body shape (§S-5.3). Assert on the
  serialised response, not on the sentinel.
- **E-2.** The two cases are **not** distinguishable by **timing** either,
  to within a coarse threshold. A resolver that short-circuits on
  "unresolved" and does a catalogue array scan on "blocked" leaks the
  distinction through latency even with identical bodies. A coarse
  assertion is sufficient; this is not a constant-time-crypto
  requirement.
- **E-3.** A player-facing endpoint never returns
  `jurisdiction_resolutions` content: no basis, no considered-bases list,
  no confidence class, no evidence reference (§S-3.1). At most a curated
  projection.
- **E-4.** A staff caller **without** `verification:read` receives a
  resolution record with `considered_bases` projected out, and cannot
  infer from any other field that a KYC basis was consulted, disagreed,
  or was unavailable (§S-3.1, §B1.3's side-channel rule).

### F. Availability and fail-closed behaviour

- **F-1.** Resolver unavailable ⇒ the operation denies; it does **not**
  fall back to a previously-known-good answer, a cached value, a tenant
  default, or a brand default (§S-3.3 case 9;
  `internal/assetregistry/authorization.go:30-42`'s existing prohibition).
- **F-2.** N failed resolutions within one bucket produce **exactly one**
  `audit_log` entry, with a count (§S-2.5's amplification guard).
- **F-3.** The deadlock case: two concurrent operations on the **same
  player** — the shape
  `internal/risk/cumulative_race_integration_test.go` already exercises —
  complete without a `40P01`, with the resolver in the picture. This is
  H-2's regression guard and it must exist before the resolver ships, not
  after.
- **F-4.** The resolver holds no advisory lock and no row lock
  (§S-4.2 mechanism 3), and succeeds inside a `BEGIN ... READ ONLY`
  transaction (§S-4.2 mechanism 2).

### G. Isolation and RLS

- **G-1.** Every §S-3.3 row (all nine) has at least one test.
- **G-2.** `DELETE` against `jurisdiction_resolutions` removes zero rows;
  `UPDATE` raises (append-only trigger); `TRUNCATE` raises.
- **G-3.** A resolver query issued on a **bare pool connection** (no
  `WithTenant`) reads zero rows — and the resolver reports this as an
  **error**, not as `unresolved` (the B-2 pairing; §S-3.1's
  silent-wrong-answer warning).
- **G-4.** A tenant-scoped transaction can `SELECT` from `jurisdictions`
  and cannot `INSERT`/`UPDATE`/`DELETE` (§S-3.2).
- **G-5.** A tenant-scoped role cannot write `casino_games`
  (`PermCasinoCatalogueManage` is platform-only, `permission.go:76-82`) —
  a regression guard that matters more once the blocklist is live
  (§S-5.4).
- **G-6.** RISK §2.2's enumeration query returns **zero rows** at
  activation (RISK §2.4a makes this `qa`'s assertion, not a checklist
  item). Restated here because it is an authorization-surface assertion
  and belongs in this list too.

---

## S-7 — RULING on caching

### S-7.1 Ruling: no jurisdiction cache in Stage 4I

**Ruling CA-1 (binding for this stage): `backend` does not build a
jurisdiction resolution cache.** This is not a deferral of a decision; it
is the decision for this stage, and it is reversible later on the
conditions in §S-7.2.

Grounds, all factual about the current repository:

1. **There is no cache infrastructure to use.** Confirmed: no Redis,
   Memcached, or message-broker client exists anywhere in `internal/`
   (the single "redis" match in the tree is an unrelated comment in
   `internal/identity/login_attempt.go`). Building one for jurisdiction
   would introduce a new infrastructure dependency, a new failure domain,
   and a new data store holding player-derived facts — for a resolver
   whose inputs are all rows on a connection the request already holds.
2. **It would create H-3's hazard where none exists.** RISK §6.4 is
   precise: a network cache read placed after
   `evaluator.go:338`'s advisory lock serialises every operation for that
   player behind an external service's latency, "converting a
   cache-latency blip into cascading fail-closed denials under load." The
   safest way to satisfy H-3 is to have no network I/O available to place
   there.
3. **It would undo §S-5.2's availability property.** The reason
   fail-closing K-3 is acceptable is that resolver availability equals
   database availability. A cache makes that false: resolver availability
   becomes database availability **and** cache availability, which is
   strictly worse, on the casino launch path, for a latency win nobody has
   measured a need for.
4. **CLAUDE.md's no-uncontrolled-scope rule applies.** No measured
   performance requirement for a cache has been stated anywhere in this
   stage's chain.

### S-7.2 If a cache is ever added — the binding conditions

Recorded now so that a future implementer inherits the constraints rather
than rediscovering them. All of these are `security` requirements; none is
optional.

**Position in the request lifecycle — the only safe place.**

> A cache read is permitted **only before the guarded transaction
> begins**, as an input to the single read-only resolution §S-4 requires,
> and **never** after any gate in the chain has started. Specifically:
> never between `pg_advisory_xact_lock` (`internal/risk/evaluator.go:338`)
> and commit; never inside `Rule.breach()`; never inside
> `CheckEligibility`; never inside an RG evaluation.

This is H-3 restated as a placement rule. It composes with §S-4.3's
preference for resolution before the transaction: if resolution already
happens there, the cache read is in the only place it could be anyway.

**What must NEVER be cached** — each with the reason, because a reason
survives a refactor and a rule does not:

1. **Any `unresolved` or `refused` outcome.** Caching a negative turns a
   single transient failure into N sticky denials for the cache's
   lifetime, and hands an attacker cheap amplification: induce one
   failure, receive many denials. Negative caching on a fail-closed
   control is a denial-of-service multiplier.
2. **Any resolution whose selected basis is a fact the platform cannot
   observe changing.** The test is *observability of the invalidating
   write*, not volatility:
   - A declared residence on `player_accounts` changes by a write **we
     make** ⇒ invalidatable ⇒ cacheable.
   - A `kyc_verifications` row reaching `approved` is a write **we make**
     ⇒ invalidatable ⇒ cacheable.
   - A **vendor-side** determination we do not hold, an **in-flight KYC
     review** whose outcome lands outside our write path, or a
     **geolocation/IP-derived** basis (which changes on every request by
     definition) are **not** observable ⇒ **never cacheable**. Caching a
     geo-derived basis is additionally caching the player's location,
     which is both semantically wrong (concept 1 is a point-in-time
     signal, IC §1) and a new PII store.
3. **The evidence.** The cache stores the *outcome* (code, basis enum,
   confidence class, `as_of`) and never the evidence values §S-2.3
   forbids in the audit record. A cache is a store with weaker access
   control, no RLS, and no retention policy; every argument in §S-2.3
   applies to it *more* strongly, not less.
4. **Anything keyed without the tenant.** The cache key includes
   `tenant_id`, and a cache hit is validated against the requesting
   tenant before use. A cache is the one place on this platform where
   RLS does not protect you, which makes it the one place a cross-tenant
   key collision is a cross-tenant data leak rather than an empty result.
5. **A resolution for a player whose account status has changed** — a
   suspension, a self-exclusion, or a brand reassignment invalidates.

**Explicit invalidation — write-driven, never TTL-only.** TTL-only means a
stale *permissive* resolution survives a compliance-driven change for the
whole TTL window, which is the exact failure a regulator asks about. The
enumerated invalidating writes: `player_accounts`' residence/status/brand
fields; `kyc_verifications` reaching a terminal state; a
`tenant_jurisdiction_configs` insert or expiry; a `jurisdictions` registry
change; a `licences.permitted_markets` change; a
`tenants.licensing_model` change; and any RISK §2.4b resolution-active
toggle. Invalidation is driven from the write path (in-transaction or a
post-commit hook), with a TTL as a **backstop**, never as the mechanism.

**Never authoritative — the operational form of the rule.** The
directive's "a cache must never become authoritative" is too abstract to
test. The testable form:

> **A cache hit may never authorise an operation that a fresh resolution
> would refuse.** A cache may only ever return the same answer sooner. If
> a cached resolution's `confidence_class` or `as_of` does not satisfy the
> operation class's requirement, it is a miss, not a downgrade.

`security` endorses RISK §6.5's boundary unchanged — "a cached
jurisdiction is acceptable in a way a cached exposure total is not… **but
only under rule 0**" — and adds that the converse must be explicit: **no
cached value may ever influence a cumulative or exposure evaluation.**
`cumulativeUsage` (`internal/risk/cumulative.go:262-312`) reads through
the same `pgx.Tx` the caller posts its effect in, and that must remain
true with no exception.

**Historical reproducibility — reads come from the record, never the
cache and never a recomputation.** A reporting, audit, dispute or
regulator-facing read of "which jurisdiction governed operation X"
resolves by reading the persisted `jurisdiction_resolutions` row via
AR-2's FK. It must **never** re-run the resolver (the inputs have
changed) and must never consult the cache. `qa`: a reproducibility test
that changes every input fact, then asserts the historical read still
returns the original resolution, basis and policy version.

---

## S-8 — Incidental finding outside Stage 4I's scope, surfaced by this review

### SEC-4I-F1 — staff-supplied `required_approvals` on held-disposition resolution — severity HIGH

This is **not** a jurisdiction finding. It was found on the same request
struct as C-4 (`resolveHeldDispositionRequest`), it is the **same
anti-pattern** — a client-supplied enforcement parameter with no
server-side backstop — and it falls squarely inside this specialist's
mandatory review scope (four-eyes / authorization). It is reported here
rather than held, per this specialist's Authority.

**Confirmed at HEAD, the full chain:**

```
internal/httpserver/bonus_handlers.go:633
    RequiredApprovals int32 `json:"required_approvals"`   // from the REQUEST BODY

internal/httpserver/bonus_handlers.go:672-674
    requiredApprovals := req.RequiredApprovals
    if requiredApprovals == 0 { requiredApprovals = 2 }   // defaults, does not clamp

internal/bonus/held_disposition_ops.go:286
    ConsumeApprovedChangeRequest(..., p.RequiredApprovals, p.ActorID)

migrations/0063_bonus_change_governance.up.sql:438-467
    bonus_change_consume_approved_request(..., p_required_approvals, ...)
      ... COUNT(DISTINCT approver_principal_id) ... >= p_required_approvals
```

The database function compares the approval count against **whatever
integer the caller passed**. It never re-reads
`bonus_approval_policies`.

**Why this is a real weakening and not a theoretical one:** every *other*
four-eyes wrapper in the same package resolves the threshold
**server-side** via `resolveRequiredApprovals`
(`internal/bonus/four_eyes_ops.go:53-58`), which reads
`ResolveApprovalPolicy` and falls back to
`fallbackApprovalPolicy`'s conservative `RequiredApprovals: 2`
(`change_governance.go:214-216`). Confirmed call sites doing it correctly:
`ChangeOpCampaignActivate` (`four_eyes_ops.go:83`), `ChangeOpOfferPublish`
(`:127`), `ChangeOpManualGrantIssue` (`:225`), `ChangeOpBulkJobExecute`
(`:345`). **`ResolveHeldDispositionAction` is the one operation that takes
the number from its caller, and its caller is an HTTP request body.**

*Failure scenario, concretely:* a tenant sets
`bonus_approval_policies.required_approvals = 3` for
`held_disposition_resolve`, because routing forfeited bonus funds to cash
is a money-moving action they want triple-signed. A staff member holding
`bonus_held_disposition:resolve` files a change request, obtains **one**
colleague's approval, and calls
`POST /v1/admin/bonus/held-dispositions/{id}/resolve` with
`"action": "route_to_cash"` and `"required_approvals": 1`. The DB function
finds one distinct non-requester approval, `1 >= 1` holds, the request is
consumed, and the funds route to cash. The tenant's configured control was
reduced from three approvers to one by a single integer in a request body,
and nothing in the audit trail records that the threshold was overridden —
only that the operation succeeded.

**Bounded, honestly:** this is a *degradation* of dual control, not a full
bypass. The DB function still requires at least one `approve` from a
principal other than the requester, still requires a matching `pending`
request with a matching payload, and still refuses if any `reject` exists.
A lone actor cannot self-approve. Severity is therefore HIGH, not
CRITICAL.

**Required fix (not implemented here — `internal/bonus` and
`internal/httpserver`'s bonus surfaces are read-only for this phase, and
this is a design phase):** delete `required_approvals` from
`resolveHeldDispositionRequest` and resolve it server-side via
`resolveRequiredApprovals(ctx, tx, tenantID, ChangeOpHeldDispositionResolve,
brandID, assetCode)`, exactly as the other four wrappers do. This is the
same shape as JV-2 and for the same reason. A defence-in-depth addition
worth making at the same time: have
`bonus_change_consume_approved_request` read the policy itself and use
`GREATEST(p_required_approvals, policy_required)` so that a future caller
passing a low number cannot weaken the control even if a handler
regresses.

**Required tests** (extending §B1.5's four-eyes list): a
held-disposition resolve with a body `required_approvals` **lower** than
the tenant's configured policy fails; with `0` fails or falls back to the
policy value (never to a hardcoded 2 when the policy says more); and the
`fallbackApprovalPolicy` path is exercised with **no** policy row present.

**Routing:** flagged to the orchestrator for scheduling outside Stage 4I's
jurisdiction scope. `security` does **not** block Stage 4I on it — it is
pre-existing, independent of jurisdiction, and fixing it inside this
stage would be scope expansion. `security` **does** record it as an
unresolved finding that must be scheduled, and it should not be closed by
this stage's completion report.

---

## S-9 — Positions on Human Decision Register candidates — POSITIONS ONLY, NONE DECIDED

Per the directive's constraints. Four of the six have a genuine
security-control dimension.

**HDR-J-1 (fallback to tenant/brand jurisdiction) — genuinely human;
`security` adds one binding constraint on any "yes" answer.** Agree with
IC §3 and RISK §7 that the interim no-fallback posture is safe to build
unilaterally because it is a strict subset of every possible answer.
`security`'s constraint: **if a fallback is ever authorised, it must be a
distinct, recorded `basis` value, and the consuming gate must be able to
refuse it per operation class.** A fallback that is indistinguishable from
a resolved player jurisdiction, once written into a snapshot column
protected by an immutability trigger, becomes a **permanent,
unfalsifiable record of a fact that was never established** — worse than
having no record, because it looks authoritative to every future reader
including a regulator. `RISK §7`'s version of this constraint is about
`Rule.matches` comparing a bare string; `security`'s is about the
persisted artefact. Both must hold.

**HDR-J-2 (which signal governs which operation class) — genuinely human;
no security position on the legal question.** One control note:
whichever precedence is chosen, it is **configuration that changes
enforcement**, so it needs a version (`resolver_policy_version`, §S-2.2),
an audit entry on change (§S-2.4 class 1), and a four-eyes posture at
least as strong as `bonus_approval_policies`' own. `security` endorses
RISK §4.2's bootstrap-circularity correction (key the precedence config on
the tenant/licence side, which is knowable before player-side resolution)
as the only shape that does not smuggle in a global default while
appearing configuration-driven.

**HDR-J-3 (privacy/lawful basis for a player geographic attribute) —
genuinely human; `security` concurs with IC §3's reasoning in full and
adds the access-control half.** IC argues the lawful-basis/retention
question. `security` adds: if the fact is collected, it inherits **new
access-control obligations that do not exist today**. `player_accounts`
currently holds no PII of this class, so every existing reader of that
table — every handler that joins it for a brand id or a status —
becomes a potential reader of a residence attribute. **Requirement on any
"yes" answer:** the attribute is projected out of every existing read that
does not specifically need it (a column-level discipline, enforced by
explicit `SELECT` lists rather than `SELECT *`), and reading it requires
its own permission rather than riding on `player:read`. This is a
concrete implementation cost of a "yes" and the orchestrator should know
it before deciding, not after.

**HDR-J-4 (obligations in flight when jurisdiction changes) — no security
position on which jurisdiction governs; one control requirement on either
answer.** Whatever is decided, the **record must show both** — the
jurisdiction frozen at the operation's start and the one current at its
completion — when they differ (§S-2.2's `considered_bases`, §S-3.3 case
7). A design that silently uses one and does not record the other makes
the decision unreviewable after the fact, which is the C-5 defect
reappearing at a different temporal grain. `security` takes no position
between IC §3's "apply the stricter" posture and RISK §7's
merge-outcomes-not-rule-sets mechanism; RISK's objection that rule sets
are not totally ordered while `Outcome` is appears correct on the code,
and that is `architect`'s to adjudicate.

**HDR-J-5 (BYOL — whose determination governs) — genuinely human;
`security` endorses RISK §7's interim constraint and adds the shape
requirement.** If J-5 resolves as "tenant-supplied or tenant-overridable,"
then a tenant-influenced jurisdiction is a **class (b) value arriving from
outside the platform's control** — which is exactly what JV-1 forbids for
a request body, and the fact that the supplier is a B2B tenant rather than
an end user does not change the control question, only its likelihood.
**Constraint on any "yes":** a tenant-supplied determination must be a
distinct `basis` (`tenant_asserted`), never merged into
`platform_resolved`, and platform-licence ceilings must continue to be
scoped by `licensing_mode` — a value resolved server-side from
`tenants.licensing_model` that a tenant cannot influence — exactly as
RISK §7 requires and as `internal/risk/types.go:176-185` already
documents. `security` endorses IC §3's observation that the provenance
shape must be able to name both from day one.

**HDR-J-6 (permitted markets) — no security position.** It is a licence-
scope business/legal fact. One boundary, agreeing with RISK §7: validating
a resolved jurisdiction against `licences.permitted_markets` is an
**authorization** check and must be distinguishable in its reason code
from a limit breach and from a blocklist hit. A licence-scope violation
that surfaces as a generic denial is an incident nobody can triage.

---

## S-10 — Scope of this review: what was and was not covered

Per this specialist's stated Limitations — a feature is not "secure"
because it passed one review, and the boundaries of the review are part of
its output.

**In scope, and reviewed at code level:** the five Bonus admin surfaces
carrying `jurisdiction_code` and their handlers; `assetregistry`'s
`CheckEligibility` input contract and `assertTenantScope`; `internal/risk`'s
jurisdiction gate, advisory lock and rule-authoring path (read-only, and
R-1/R-2 accepted as `risk`'s rulings, not re-derived); `internal/casino`'s
`LaunchGame` gate order, blocklist check and HTTP error mapping;
`internal/tenant`'s context contract; `internal/db/tenant_rls.go`'s scope
helpers; `internal/httpserver/json.go`'s decoder; the
`bonus_change_requests` four-eyes consume path; migrations 0002, 0005,
0014, 0033, 0035, 0040, 0042, 0045, 0057, 0063 in the regions touching
jurisdiction, RLS or approvals; `deploy/init-app-role.sql`; and
`internal/auth/permission.go`'s scoping comments for the permissions this
ruling touches.

**In scope but reviewed at design level only, because no code exists:**
the resolver itself, `jurisdiction_resolutions`, the registry write
surface, the resolution-active fact, and any caching.

**Explicitly NOT in scope, and NOT certified by this document:**

- The **correctness of the jurisdiction determinations themselves.** This
  document specifies how a determination is protected, recorded and
  isolated. Whether the determination is *legally right* is
  `identity-compliance`'s and the licence holder's, per
  `docs/architecture/15-jurisdiction-and-licensing-model.md`. Nothing here
  should be read as "security has validated that the platform resolves
  jurisdiction correctly."
- **Penetration testing and certification-grade audit.** Not performed;
  they require external, human-run engagements.
- **The ledger postings, EOI semantics, and RG evaluation logic** that the
  gate chain composes around — assumed correct, not verified here.
- **Payments' dimension-2 defect (C-3(d))** beyond noting RISK §1.4's
  classification. `payments` owns its own phase; the ISO-country vs.
  `jurisdictions.code` code-space confusion is a genuine second defect
  and `security` will need to review whatever mapping is proposed,
  because a wrong country→jurisdiction mapping is an authorization defect,
  not a data-format one.
- **Sportsbook, Retail/POS, Back Office, Partner Console, B2C frontend** —
  not implemented, not reviewed.
- **Any real vendor integration.** §S-5.2's forward constraint names the
  security consequence of introducing one on the launch path; it does not
  review one.

**This document does not authorise production launch**, and no finding
here should be read as a launch clearance. The one finding that `security`
flags as **launch-blocking if unresolved** is SEC-4I-F1 (§S-8) — a
tenant-configured dual-control threshold that a request body can lower is
not acceptable in production regardless of schedule. It does not block
Stage 4I.

---

## S-11 — Summary and routing

**Rulings (binding within `security`'s authority):**

| Id | Ruling | Where |
|---|---|---|
| **JV-1** | Every jurisdiction value is either a configuration scope declaration (staff-authored, controlled) or a resolved operation fact (server-only). Never both; no request body carries the latter | §S-1.1 |
| **JV-2** | The Bonus admin `jurisdiction_code` fields (four structs, five handler surfaces) are **removed**, not cross-validated — `decodeJSON`'s `DisallowUnknownFields` then makes a submitted value a 400 for free | §S-1.1, §S-1.2 |
| **JV-3** | `risk_rules` / `asset_authorizations` / catalogue-blocklist jurisdiction fields are configuration and stay | §S-1.1 |
| — | Three-layer mechanism: no parse path (incl. a non-forgeable resolution type) + GUC/context cross-check + FK/immutability/CHECK at the database | §S-1.3 |
| — | JV-2 lands **with** the resolver call per handler, never before | §S-1.4 |
| **AR-1** | Per-operation resolution records live in their own append-only tenant-scoped table, **not** `audit_log`; `audit_log` gets four named event classes | §S-2.1, §S-2.4 |
| **AR-2** | Every jurisdiction-consuming operation persists a FK to the resolution that governed it, in the same transaction as the effecting write | §S-2.1 |
| — | **Record the decision and references to evidence; never the evidence values.** Eight-item prohibited list, incl. `kyc_documents.issuing_country`'s value, residence values, nationality, raw IP/geo, and `persons.person_key_hash` | §S-2.3 |
| — | Resolver-unavailability audit entries are **aggregated**; per-request entries are an amplification attack on an immutable store | §S-2.5 |
| — | RLS contract: `NOT NULL tenant_id`, `ENABLE`+`FORCE`, per-command policies, no DELETE/UPDATE policy, append-only + TRUNCATE-deny triggers, composite FKs, **no player-read policy**, `considered_bases` projected out without `verification:read` | §S-3.1 |
| — | The nine required behaviours (cross-tenant, cross-brand, player-scope, forged jurisdiction/tenant/brand, stale, conflicting source, unavailable resolver), each with its required outcome and its must-not | §S-3.3 |
| — | **H-2 confirmed and elevated**: the resolver is read-only on the evaluation path, enforced by a read-only interface type + a `READ ONLY` transaction test + a `pg_locks` test — not by a comment. It is a remotely triggerable DoS, not only a deadlock | §S-4.1, §S-4.2 |
| — | Prefer resolution **before** the guarded transaction; the TOCTOU window is accepted only if the resolution id is persisted in-transaction and `as_of` staleness is bounded per operation class, failing closed | §S-4.3, §S-4.4 |
| — | Doc 34 §5.3: the constraint is **mandatory**; record it as a **precondition** rather than "rule 0" (a rule that never participates cannot be reordered) — `architect`'s documentation call | §S-4.5 |
| — | **Player-facing responses must not distinguish `jurisdiction_unresolved` from `jurisdiction_blocked`**; internal distinctness is preserved in full | §S-5.3 |
| — | Mandatory adversarial test specification for `qa`: 7 categories, ~30 concrete code-grounded cases | §S-6 |
| **CA-1** | **No jurisdiction cache in Stage 4I.** If ever added: read only before the guarded transaction; never cache negatives, unobservable-invalidation bases, geo-derived bases, evidence, or anything keyed without the tenant; write-driven invalidation with TTL as a backstop only; a cache hit may never authorise what a fresh resolution would refuse; historical reads come from the record | §S-7 |

**Findings:**

| Id | Severity | Finding | Blocks |
|---|---|---|---|
| **SEC-4I-F1** | HIGH | `resolveHeldDispositionRequest.RequiredApprovals` is taken from the request body and passed unclamped to `bonus_change_consume_approved_request`; every other four-eyes wrapper resolves it server-side. A staff member can lower a tenant's configured dual-control threshold with one integer | Launch, if unresolved. **Does not block Stage 4I** |
| **SEC-4I-F2** | MEDIUM | The one live producer of a real jurisdiction value (staff-supplied, P-3) is recorded nowhere — `bonus_grant.manual_issue`'s audit metadata omits it and `bonus_grants.jurisdiction_code` is never assigned | Nothing today; interim control specified |
| **SEC-4I-F3** | MEDIUM | `casino_game.upserted`'s audit metadata carries no before/after and omits `jurisdiction_blocklist`, which is about to become a platform-wide live denial control | **Blocks fail-closing K-3** until fixed |

**Routed onward:**

| To | Item | Where |
|---|---|---|
| `architect` | Fold §S-2/§S-3/§S-6 into `docs/security/security-architecture.md` as a numbered section during synthesis | header |
| `architect` | Doc 34 §5.3 wording: precondition, not "rule 0" | §S-4.5 |
| `architect` / orchestrator | A future vendor input to resolution makes a third party a hard dependency of the casino launch path — needs its own decision and review | §S-5.2 |
| `backend` | Non-forgeable resolution type; read-only resolver interface; `assertTenantScope` adoption; the schema/RLS contract; the registry write surface + one platform-only permission (role wiring subject to `security` review before it lands) | §S-1.3, §S-1.7, §S-3, §S-4.2 |
| `backend` | Resolver has **no external network dependency** this stage — availability equals database availability | §S-5.2 |
| `casino` | Fail-close K-3 with SEC-4I-F3 fixed first; collapse the unresolved/blocked distinction at the HTTP boundary only; make demo-mode gating explicit in code with its reason | §S-5.3, §S-5.4, §S-5.5 |
| `bonus-engine` | JV-2 removal sequenced with the resolver call; SEC-4I-F2's interim audit metadata; RISK §3.3's `resolveJurisdictionID` empty-vs-unknown collapse | §S-1.4, §S-1.5 |
| `qa` | §S-6's seven categories are mandatory for this stage's gate; §S-2.5's amplification test; §S-4.2's READ-ONLY and `pg_locks` tests; §S-7's reproducibility test | §S-6 |
| `payments` | The ISO-country → `jurisdictions.code` mapping is an authorization surface and needs `security` review when proposed | §S-10 |
| `identity-compliance` / orchestrator | §S-1.6's recommendation that a jurisdiction-fact correction be four-eyes-eligible (gated on HDR-J-3); §S-9's column-projection cost of a "yes" on HDR-J-3 | §S-1.6, §S-9 |
| Orchestrator | SEC-4I-F1 scheduling; HDR positions/constraints on J-1, J-2, J-3, J-5. **None decided here** | §S-8, §S-9 |

**Labelled status of this deliverable, per CLAUDE.md's "No fake
completion":** `NOT IMPLEMENTED` — this is a design-phase document. No
production code, migration or schema changed at this commit. The
`jurisdiction_code` fields at `bonus_handlers.go:399`/`:633` and
`bonus_domain_ops_handlers.go:497`/`:719` are unchanged and still
client-supplied; `casino`'s blocklist at `orchestrator.go:138` is
unchanged and still fail-open; SEC-4I-F1 is unfixed; and no resolver
exists.
