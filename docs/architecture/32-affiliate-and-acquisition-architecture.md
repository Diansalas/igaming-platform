# 32 — Affiliate and Acquisition Engine Architecture

Status: **DESIGN/ARCHITECTURE ONLY — `NOT IMPLEMENTED`.** No Go code, no
schema, no migration, no route, no tracking pixel, no commission
calculation, no settlement is authorized by this document. Produced in
Stage 4H-B1, Wave 1.5 (Architecture Reconciliation Gate), directive §F;
**revised in the Wave 1.5 Fix Wave (task `4HB1FW-03`) to close
`security`'s SEC-W15-01 (P0), SEC-W15-05/06/07/12/13, `code-reviewer`'s
P1-4, and `ledger-finance`'s LF-6/LF-9 — see §15 for the changelog.**
Authored by `architect` under the roster adaptation recorded in
`docs/governance/task-registry.md` (no `affiliate` specialist exists;
same precedent as Gamification doc 17, Retail doc 26 and ADR 0037), with
`code-reviewer` as the independent architectural reviewer.

**Revision (Wave 1.5 Fix Round 2, `code-reviewer`, one precise correction
only — see §15's last row):** §6.5.2's `B(O)` resolver definition
corrected from a strict ancestor chain to a reflexive ancestor closure,
closing `code-reviewer`'s own NEW-6 (fail-open on a same-node declared
interest; deadlock on a flat/root node). Made by `code-reviewer`, not
`architect` (this document's owner, who did not touch this document this
round), under this round's explicit task authorization, since the fix is
small, mechanical and already fully specified by both NEW-6 and
`security`'s independent §W15.2.7 restatement of the identical
correction. Nothing else in this document was changed by this revision.

Numbering: next free after `31`. This document takes **32**.

## 0. Scope anchor — stated honestly

- The Blueprint **does** require affiliate capability:
  `01-requirements-inventory.md` lists "Affiliate attribution capture and
  FTD/deposit/NGR postbacks" as a functional requirement, and
  `13-dependency-map-and-risk-register.md` names third-party affiliate
  platforms (Income Access, MyAffiliates) as the assumed vendor class.
- The Blueprint's own bias, recorded in `02-domain-and-service-boundaries.md`
  and `14-mvp-scope-and-roadmap.md`, is **third-party first; in-house
  rebuild is a year-two consideration** (Blueprint §1 table).
- The Wave 1.5 **human directive** elevates Affiliate/Acquisition to a
  first-class architectural domain and requires this document.

As with CRM (doc 31 §0), these are not in conflict and this document does
not overturn the buy-first posture. It defines the boundary, the data
ownership, the attribution-evidence rules and the financial contract —
which is exactly what a bought affiliate platform must plug into rather
than replace. **Buy-vs-build remains an OPEN DECISION for the human**
(§12, OI-AFF-1); this architecture is compatible with both, and §6's
financial contract is the part that must exist either way, because a
vendor computing commission is not a vendor authorized to move our money.

### 0.1 Authority boundaries observed

Does not override `ledger-finance` on financial invariants (and
explicitly declines to design the commission posting shape — §7),
`security` on security requirements, `identity-compliance` on identity/
privacy/consent, `risk` on Risk, `bonus-engine` on bonus mechanics, or
`data-analytics` on reporting. Selects no Human Decision Register item
(G-2, `OpenBetSelfExclusionPolicy`, cashout policy, FD-1).

---

## 1. The boundary

> **Affiliate decides WHO INTRODUCED WHOM, and WHAT COMMISSION IS OWED
> under which versioned rule. It never moves money, never touches a
> player's balance, never gates play, and never grants a bonus.**

| Affiliate owns | Affiliate never owns |
|---|---|
| Affiliate/partner records and their hierarchy position | The hierarchy primitive itself (§3 — reused, not rebuilt) |
| Affiliate users and their scoped access | A new identity model, principal type or actor type |
| Tracking links, promo codes (as *attribution tokens*, §5.4), clicks | Bonus value, coupons that grant value, or any Offer's terms |
| The immutable attribution evidence chain and the attribution *decision* | Player identity, KYC state, RG state, jurisdiction |
| Commission **rules** (versioned configuration) and commission **accrual** (a claim) | Commission **settlement** — the money movement (`ledger-finance`, §7) |
| Affiliate-facing reporting scoped to its own subtree | The reporting/BI pipeline (`data-analytics`, doc 12) or any monetary figure computed outside the ledger |
| Sub-affiliate override rules | The wallet, the ledger, bonus accounting, Risk, RG |

### 1.1 The invariant that makes all of this safe

**AFF-1 — Affiliate is a read-and-record domain on the player side.** It
records facts *about* acquisition (click, attribution) and *claims*
against revenue (accrual). It writes nothing into any player-owned table,
posts nothing to the ledger, and cannot cause a player-facing effect
except indirectly, through a domain that decides for itself (Segmentation
→ CRM → Bonus, §8).

This is ADR 0035's retail precedent applied one domain over: Retail "emits
the *reason* for a movement and the *authorization* for it;
`ledger-finance` owns every account, posting and monetary invariant."
Affiliate emits the reason (an approved accrual) and the authorization (an
approval record); `ledger-finance` owns the posting.

---

## 2. Domain model

Every object carries doc 10 **W1**'s common object contract (UUID
identity; `tenant_id` from authenticated server context only; explicit
status enum; `clock_timestamp()`-sourced immutable `created_at`;
`actor_type`/`actor_id`; audit correlation; DB-enforced idempotency where
externally triggerable; append-only history).

```
  Partner structure
    Affiliate(node ref) · AffiliateUser · AffiliateAgreement(+Version)
  Acquisition
    TrackingLink · PromoCode · Click · ReferralSignal
  Attribution
    AttributionModel(+Version) · AttributionCandidate · PlayerAttribution
  Commercials
    CommissionRule(+Version) · CommissionPeriod · CommissionAccrual ·
    CommissionAdjustment · CommissionApproval · CommissionPayable
  Surfaces
    AffiliateReport (read model)
```

---

## 3. The hierarchy — reuse `internal/agentnetwork`, do not rebuild

**Decision, with the justification the directive asks for.**

`26-retail-operations-architecture.md` §7.1 already made the naming
decision for exactly this case, in its own words:

> "the hierarchy is not inherently a retail concept. A B2B sub-operator
> tree, **an affiliate/sub-affiliate structure**, and a white-label
> reseller chain are all the same graph with different node types. Naming
> it `retail` would guarantee that the second consumer either duplicates
> it or imports a package whose name lies."

`docs/governance/ownership.md` records the same reasoning verbatim on the
`internal/agentnetwork` row. **Affiliate is that predicted second
consumer.** Building `affiliate_nodes` + its own closure table would
falsify the prediction the platform already committed to in writing.

**Therefore:**

- The affiliate/sub-affiliate tree is **node instances in
  `internal/agentnetwork`**, with node *types* (`affiliate`,
  `sub_affiliate`, `network`, …) as **configuration rows**, never enum
  values, Go types, constraints or permissions — doc 26's "the hierarchy
  is configuration, never a code path" rule, unchanged. A different
  tenant's affiliate network is a different set of rows with zero code
  change.
- Node type/relation/capability **definitions** are dual-scope
  (`tenant_id IS NULL` = platform-wide template), mirroring `risk_rules`
  (ADR 0031 §3); every node **instance** is `tenant_id NOT NULL`.
- **`internal/affiliate`** is the operational surface — tracking,
  attribution, commission rules/accrual/approval, settlement
  *instruction*, affiliate reporting — composing `agentnetwork` +
  `identity` + `segment` + `ledger` (for the instruction call and
  ledger-derived reads). The split mirrors doc 26's
  `agentnetwork`/`retail` split exactly and for the same reason —
  including the composition list: doc 26 §7.1 defines `internal/retail`
  as composing `agentnetwork` + `identity` + `rg` + `risk` + **`ledger`**,
  and Affiliate's relationship to `ledger` is the same one. **What is
  forbidden is deciding monetary treatment, not importing the package** —
  see the corrected §7 rule 1.
- Subtree scoping (an affiliate sees only its own subtree) uses the
  **same** node-subtree RLS dimension doc 26 §8 already flags as
  P1 `architect` + `security` work (`app.hierarchy_node_id` +
  `WithNodeScope`, including the permissive-policy OR-widening split
  ADR 0019 documents for `app.player_account_id`). Affiliate does not
  invent a second scoping mechanism.

### 3.1 The honest cost of this decision

`internal/agentnetwork` **does not exist.** Retail is architecture-only
and unbuilt. So reuse creates a real build-order dependency:

- Either the hierarchy primitive is built by whichever of {Retail,
  Affiliate} is authorized first, and the second consumes it;
- or Affiliate's first slice is restricted to a **flat** affiliate list
  (no sub-affiliates), which needs no closure table, with the hierarchy
  arriving when the primitive does.

The second option is the lower-risk sequencing and is this document's
`RECOMMENDATION` if Affiliate is ever authorized before Retail —
sub-affiliate commission (§6.4) is the only capability it defers, and
deferring it costs a migration, not a redesign. **Neither is authorized
here**; recorded for the human's stage sequencing in doc 33 §4 and as
OI-AFF-2.

### 3.2 Affiliate users — storage reuse accepted, authorization-model reuse REJECTED (DEP-AFF-1, ruled)

The Wave 1.5 original proposed that an affiliate user simply *is* an
`identity.StaffUser` with an affiliate role, mirroring doc 26's cashier
decision, while flagging that "a cashier is *our* staff; an affiliate is
an *external commercial counterparty* authenticating into the platform's
staff principal space" and routing the judgment to `security` as
**DEP-AFF-1**.

**`security` has ruled: conditional accept, four binding conditions**
(`docs/security/security-architecture.md` §W15.2.2).
The distinction it drew is the one the original flag gestured at but did
not make precise — **storage reuse is fine; authorization-model reuse is
not**:

| Reused | Not reused |
|---|---|
| The `identity.StaffUser` **row/table/credential/session machinery** — no second identity model, no second credential store, no second session mechanism (`CLAUDE.md`'s one-identity rule, doc 33 §3.1 item 3) | The **permission model**. An affiliate principal is a **distinct principal class**, and its permissions are not "staff permissions minus a few" |
| `audit.ActorType`'s existing `staff` value — no new actor type, per doc 26's precedent and doc 10 N2.3's "never invent a parallel enum" | **Permit-by-omission.** Every existing back-office permission is denied to the affiliate class **by enumeration**, not by nobody having remembered to grant it |

**The four binding conditions, as published by `security` in
`docs/security/security-architecture.md` §W15.2.2.** Without **all
four**, the reuse is refused and a distinct `auth.PrincipalType` is
required instead. Recorded here verbatim in substance, because they are
preconditions on Affiliate's own design, not merely on `security`'s:

| ID | Condition | Affiliate's obligation |
|---|---|---|
| **AFF-C1** | **Positive principal classification, never a blocklist.** `staff_users` gains a `principal_class` column (`internal` \| `external_affiliate`, extensible), `NOT NULL`, **with no permissive default**; every existing row is backfilled explicitly as `internal` in the same migration that mints the first affiliate role. Every control meaning "our own staff" tests `principal_class = 'internal'` **positively** — never `role NOT IN (<affiliate roles>)` | §6.5.1's conjunct B is written as the positive test. Affiliate never introduces a role-name-based or email-domain-based inference of who is internal |
| **AFF-C2** | **Subtree scoping must be structural before any affiliate-facing surface ships.** The `app.hierarchy_node_id` / `WithNodeScope` RLS dimension (DEP-AFF-5, already P1 from doc 26 §8) must exist and be enforced **at the database**, not as an application query filter. Until it lands, **no affiliate principal exists in any non-development environment** | §10's "subtree scoping is structural" is now a *precondition*, not an assertion. AI-9's fail-closed-on-absent-`app.hierarchy_node_id` test is Affiliate's check on it |
| **AFF-C3** | **Affiliate roles hold a DISJOINT permission set.** No affiliate-class principal holds any permission that exists today — never `staff:manage`, `audit:read`, `verification:read`, any `rg_*`, `risk_*`, `withdrawal:*`, `bonus_*` or `crm_*`. Mechanically checkable: **the intersection of every affiliate role's permission set with every non-affiliate role's permission set is empty** | Stronger than the "deny-by-enumeration allow-list" this document first proposed, and better: an empty-intersection assertion is a single test that cannot be forgotten as the permission catalogue grows, whereas an allow-list is a list someone must remember not to add to |
| **AFF-C4** | **`AFF-4E-1`** — every approval decision on any affiliate-financial object is recorded by a principal whose `principal_class = 'internal'`. **Unconditional: no threshold, no delegation, no emergency override, no service-identity carve-out** | §6.5.1 conjunct B |

**`security` explicitly decided AGAINST a new `auth.PrincipalType`**, and
its reasoning is recorded rather than paraphrased: a new principal type
would fork session issuance, audit actor typing, the `sessions` table's
`principal_type` semantics, every RLS predicate and every permission
check — a large cross-cutting change whose entire security benefit is
reproducible by one positively-stored classification column plus AFF-C1–C4.
The Wave 1.5 original's proposal (reuse) is therefore accepted, but
**only** with the authorization model forked rather than the storage.

*Note on this document's own first Fix-Wave draft*: it stated three
conditions and left "new `PrincipalType` vs. column" open. `security`'s
published decision supersedes both — four conditions, and the column. The
looser statement is not retained.

An affiliate is never a `Person`/`PlayerAccount`, and a player is never a
node (doc 26 §1.6's rule) — a player attaches to a node by reference,
through `PlayerAttribution` (§5.3).

---

## 4. Agreements and commission rules — versioned configuration

- **`AffiliateAgreement`** (+ immutable `AffiliateAgreementVersion`): the
  commercial relationship — the affiliate node, effective window, the
  currency/asset of settlement, the attribution model reference, the
  commission rule set reference, negative-carryover policy, minimum
  payout threshold, and the agreement's status.
- **`CommissionRule`** (+ immutable `CommissionRuleVersion`): the
  calculation configuration. Supported model classes (each a
  configuration shape, never a code branch per affiliate):
  `cpa` (per qualifying acquisition), `revenue_share` (a % of a canonical
  revenue measure), `hybrid`, `tiered` (bands over a measured quantity),
  `sub_affiliate_override` (§6.4).
- **Immutable once referenced.** Once any accrual references a rule
  version, that version is frozen forever — doc 10 §1.1's Offer-version
  rule and W2.1's Campaign-version rule, applied to commercial terms. A
  renegotiation is a new version with an effective date; it never
  retroactively rewrites an accrual already computed.
- **Scope**: rules are `tenant_id NOT NULL`, optionally narrowed by
  brand, product, jurisdiction and asset. Nothing affiliate-specific is a
  code path (`CLAUDE.md`).

---

## 5. The attribution lifecycle and its immutable evidence

This is the section the directive weights most heavily, because
attribution is where money later gets decided and where a dispute is
later litigated.

### 5.1 Stage 1 — Click (or equivalent inbound signal)

A `Click` row is **append-only and immutable**, written before any
identity exists:

| Field | Rule |
|---|---|
| `id`, `tenant_id`, `brand_id` | tenant/brand resolved **server-side** from the tracking link's own registration, never from a query parameter (`CLAUDE.md`: never trust a client-supplied tenant id) |
| `tracking_link_id` / `promo_code_id` | resolved from a registered link/code; an unregistered value is recorded as `unattributed` with the raw token, never silently attributed to anyone |
| `affiliate_node_id`, `agreement_version_id` | resolved server-side at click time and **frozen on the row** |
| `tracking_token` | **A server-minted opaque LOOKUP KEY, not a self-contained assertion** — see §5.1.2 (corrected, SEC-W15-06). The client never supplies affiliate identity at the qualifying event |
| `occurred_at` | `clock_timestamp()`, server clock only |
| `landing_context` | destination path/campaign parameters, size-bounded, no free-form injection into later queries |
| `jurisdiction_hint`, `device_class` | coarse, non-identifying |
| `ip_address`, `user_agent` | **PII** — see §5.6. Retained under doc 16's (unresolved) retention regime, minimized/truncated where the attribution model does not require full fidelity |
| `consent_state` | the tracking/cookie consent state at click time, by reference (§5.6) |

No `UPDATE`, no `DELETE` — enforced by trigger, mirroring `audit_log`'s
`audit_log_deny_mutation` (ADR 0013), not by application discipline.

#### 5.1.1 The unauthenticated-write hazard (SEC-W15-05, corrected in the Fix Wave)

`security` observed something the original table understated: **a click
is this platform's first unauthenticated public write path.** Every other
write in this system happens behind an authenticated, tenant-resolved
context. A click does not. The original row for `tracking_link_id`
casually allowed "an unregistered value is recorded as `unattributed`
with the raw token" — which quietly implies a row whose `tenant_id` may
be unresolvable, in a platform whose entire isolation model is
`tenant_id` + RLS.

**The four binding corrections:**

**(a) `tenant_id NOT NULL` on every `affiliate_*` table, without
exception — including `Click`.** There is no `unattributed` *tenant*.
`unattributed` is a valid value for the **affiliate** (`affiliate_node_id
IS NULL`, `attribution_status = 'unattributed'`); it is never a valid
value for the tenant. A row with no resolvable tenant is a row RLS cannot
scope, which is a row that does not belong in this database.

**(b) Explicit fail-closed rejection when the tenant cannot be
resolved.** Tenant/brand resolution for an unauthenticated click comes
from the **registered tracking link or promo code**, or from the
**registered inbound domain/host** the request arrived on (which is
tenant-scoped configuration per `CLAUDE.md`'s "domains" in the brand
configuration list) — **never** from a query parameter, a header, a
referrer, or a cookie. If neither resolves:

> The request is **rejected** (an ordinary HTTP error and a metric), and
> **no row is written at all**. It is not written to a
> "pending"/"orphan"/"unknown-tenant" table, because such a table would
> be an unscoped, attacker-fillable, cross-tenant store — precisely the
> thing `tenant_id NOT NULL` exists to prevent.

The information loss is real and accepted: an unresolvable click is a
click we cannot attribute anyway, so discarding it costs nothing an
`unattributed` row would have recovered.

**(c) Rate ceilings, at three levels, because this endpoint is
publicly reachable and write-bearing:** per source IP / per tracking link
/ per tenant, each configurable with a **fail-closed default ceiling**
rather than "unlimited until configured." Exceeding a ceiling sheds the
request; it never queues it, and it never degrades into writing an
unscoped row. Without this, the click endpoint is a free, unauthenticated
storage-amplification and cost-amplification vector against any tenant
whose link ids are guessable — and tracking links are, by construction,
public.

**(d) `landing_context` is a sink, and must be treated as one.** The
original said only "size-bounded, no free-form injection into later
queries." That is necessary and not sufficient. `landing_context` is
attacker-controlled, is later **rendered** (in an affiliate-facing report
and potentially in a back-office screen) and is later **used as a
redirect destination**. Binding:

| Risk | Control |
|---|---|
| **Open redirect** | A destination is selected from a **registered, per-tenant allow-list of landing paths**, by key. The raw value is never used as a redirect target. An unmatched key redirects to the brand's configured default, never to the supplied value |
| **Stored XSS / HTML injection** | Stored as opaque text, never as markup; escaped at every render point; never rendered as HTML in a back-office or affiliate surface |
| **Log/report injection** | Size-bounded, character-class-restricted, and never interpolated into a query, a report header, or a CSV field without the corresponding escaping |
| **PII smuggling** | The allow-list shape means an operator cannot accidentally accept an email or document number in a landing parameter; free-form key/value capture beyond the allow-list is **not** built |

Recorded as invariants **AI-16**–**AI-18** (§11).

#### 5.1.2 `tracking_token` — a lookup key with subject binding, not a signed assertion (SEC-W15-06, corrected)

The Wave 1.5 original specified an HMAC over
`(tenant_id, link_id, issued_at, nonce)` with a server-held key. That is
integrity-protected and **still hijackable**, which is `security`'s
finding: the token proves *that the platform minted it*, and says nothing
about *who is allowed to redeem it*. Anyone who obtains the token — from
a shared device, a copied URL, a referrer leak, a browser-history scrape,
a chat message, a support screenshot — can present it at their own
registration and have that affiliate's attribution attach to them. For a
CPA agreement, that is a directly monetizable theft, and the platform's
own evidence chain would record it as a legitimate attribution.

**Four corrections, all required together:**

**(1) Lookup key, not self-contained assertion.** The token is a
**high-entropy opaque random value** that indexes a server-side `Click`
row. It carries no claims and needs none: every fact — tenant, brand,
link, node, agreement version — is read from the row the platform itself
wrote. An HMAC over embedded claims invites exactly one failure the
lookup design cannot have: a verifier that validates the signature and
then trusts the embedded values without re-reading the row. The row is
the authority; the token is only the pointer to it.

**(2) Subject binding.** A token is bound, at first redemption, to the
subject that redeemed it, and thereafter **only that subject may redeem
it**. Concretely: the first qualifying event binds `bound_subject_ref`
(the registering `person_id`/`player_account_id`) on an append-only
binding row; any later presentation by a different subject is
**rejected**, not silently re-attributed. The narrow, legitimate
shared-device case (two household members registering from one browser)
is handled by the *second* registration being `unattributed` rather than
stolen — the conservative direction, consistent with §5.2's
"a forged or expired token yields `unattributed`, never a fallback
attribution."

**(3) A TTL, enforced server-side.** Every token carries an expiry
derived from the attribution model's own configured window (§5.5), with a
platform maximum. An expired token yields `unattributed`. An
indefinitely-valid tracking token is an indefinitely-valid bearer
credential for money, and this platform has no other object of that
shape.

**(4) One-token-many-subjects is a first-class FRAUD SIGNAL, not an
error to swallow.** Repeated redemption attempts of one token by
different subjects are recorded as **evidence** (an append-only
`ReferralSignal` of a rejection class, §2) and routed to the domains that
own fraud determination — `risk`, `bonus-engine`'s abuse detector, and
`identity-compliance` (§9.1, OI-AFF-4). **Affiliate does not score it,
does not threshold it, and does not act on it** — that would be the
"second risk engine" failure §9.1 and doc 30 §8.3 already forbid. It
records and routes.

**Key management (also SEC-W15-06).** Even as a lookup key, the mint path
needs key material for the per-tenant derivation below, and the original
document was silent on where any of it lives:

| Requirement | Rule |
|---|---|
| Storage | **Vault or a cloud KMS** (doc 13's "Secrets/KMS" dependency row). Never in code, never in a committed config file, never in an environment variable baked into an image (`CLAUDE.md`: never commit secrets or hardcode credentials) |
| Per-tenant derivation | Token-space key material is **derived per tenant** from a root key, so that compromise or rotation of one tenant's material cannot forge or invalidate another tenant's tokens. This is the same per-tenant-credential discipline `CLAUDE.md` mandates for provider credentials |
| Rotation | A declared rotation cadence with **overlapping validity windows** — a `key_version` is recorded on the token/binding row so tokens minted under an older version remain redeemable for their TTL and no longer. Rotation must never invalidate in-flight attribution, and must never be impossible to perform |
| Revocation | A per-tenant key version can be revoked immediately (incident response); revocation invalidates outstanding tokens of that version, which is the correct direction (they become `unattributed`) |

Recorded as invariants **AI-19**–**AI-21** (§11) and, for the KMS
dependency, as **DEP-AFF-8**.

### 5.2 Stage 2 — Attribution candidate

At registration (and at any later qualifying event the model uses), the
platform resolves **candidates**: the set of clicks/codes that the
attribution model considers, each with its evidence reference. An
`AttributionCandidate` row is append-only and records *what was
considered*, not merely what won — a model that only stores the winner
cannot answer "why not the other affiliate", which is the exact question
a commercial dispute asks.

Binding: the affiliate is resolved from the **server-side token**, not
from a client-supplied affiliate id. A client may present the opaque
`tracking_token` it was given; the platform looks up the `Click` row it
indexes and checks the subject binding, TTL and key version (§5.1.2). A
forged, expired, unknown or wrongly-bound token yields `unattributed`,
never a fallback attribution.

#### 5.2.1 `AttributionCandidate` is STAFF-ONLY, with no subtree carve-out (SEC-W15-13 — determined here)

`security` found (**SEC-W15-13**) that this object's stated purpose —
"a model that only stores the winner cannot answer *why not the other
affiliate*, which is the exact question a commercial dispute asks" — is
**incompatible with the node-subtree access model** §10 otherwise applies
to affiliate-readable data. The candidate set is, by construction, a
**cross-affiliate** record: its whole value is that it names the losing
affiliates. Any subtree-scoped read of it leaks a competitor's activity
to the affiliate positioned to act on it, and a partially-filtered read
("show me only my own candidate rows") is worse than useless for a
dispute while still disclosing that a competing candidate existed, when,
and for which player.

**Binding determination, made here rather than routed** (`security`
asked for exactly this, and it is a data-access boundary, which is
`architect`'s to specify with `security`'s requirement as the input):

> **`AttributionCandidate` is a staff-only table. There is no
> affiliate-readable policy on it, no subtree carve-out, no
> "own-rows-only" view, and no affiliate-facing API that exposes it in
> any projection, aggregate, count or existence check.**

Consequences, each deliberate:

| Consequence | Resolution |
|---|---|
| An affiliate cannot self-serve a dispute | **Correct.** Dispute resolution is an **internal** process: internal staff read the candidate set, apply the immutable model version (§5.5), and communicate a *decision and its reasoning*, never the raw competing evidence. This is how a commercial dispute between two counterparties is adjudicated everywhere else in this platform |
| §5.2's stated purpose still holds | Yes — the record exists so the *platform* can answer "why not the other affiliate." It never required the affiliate to read it |
| The RLS shape | `tenant_id NOT NULL`, `FORCE ROW LEVEL SECURITY`, `tenant_staff_scope` with the `app.player_account_id IS NULL` conjunct, **and** an explicit conjunct excluding the affiliate principal class (§3.2 condition 2's deny-by-enumeration makes this the default; the explicit conjunct makes it checkable) |
| Existence/aggregate leakage | Also denied. "How many candidates competed for this player" is a competitor-activity disclosure in aggregate form; affiliate-facing reporting (§10) exposes neither |
| A bought affiliate platform | Must not be given a feed of this table. If a vendor's dispute workflow requires it, that is a reason to keep dispute resolution internal, not a reason to widen the policy |

Recorded as invariant **AI-22** (§11).

### 5.3 Stage 3 — `PlayerAttribution`, the frozen decision

The attribution decision is computed **at the qualifying event** using
the evidence as-of that instant, and the **result is snapshotted** —
structurally the same discipline as doc 30 §SEG-1 (resolve at decision
time, snapshot the result) and doc 10 T.2's eligibility snapshot:

| Field | Note |
|---|---|
| `id`, `tenant_id`, `brand_id`, `player_account_id` | |
| `affiliate_node_id` | the winner |
| `attribution_model_id` + `attribution_model_version_id` | **by reference** (immutable version) — doc 30 §7.1's EDR-R1 |
| `evidence_refs[]` | the candidate/click ids considered |
| `decided_at`, `decided_by` (`actor_type=system`), `reason_code` | |
| `qualifying_event_ref` | the canonical event/ledger transaction that triggered the decision |
| `status` | `active` \| `superseded` |

**Immutable.** A re-attribution is a **new row superseding the old**,
carrying `supersedes_attribution_id`, a mandatory reason code, the
authorizing actor, and four-eyes approval per §5.3.1. The original is
never edited or deleted. This is the compensating-entry discipline the
ledger uses, applied to a commercial fact that determines money.

#### 5.3.1 Re-attribution four-eyes — the full requirements, stated HERE (SEC-W15-07, corrected)

The Wave 1.5 original stated the four-eyes requirement in one clause
("above a configurable materiality threshold") and left everything else
to §6.5, four sections away. `security` found two problems with that:
the control as specified **would ship inert** (a config-absent default
that means "no threshold, therefore no approval" — the same defect shape
the R6 precedent it cited was corrected *for*), and a reader of §5.3
implementing re-attribution would never reach §6.5's conjuncts. Both are
fixed by restating the requirements at the point of use, not by
cross-reference.

**A re-attribution requires, at §5.3 itself:**

| # | Requirement | Note |
|---|---|---|
| 1 | **All three of §6.5.1's conjuncts — A (distinct active person), B (`AFF-4E-1`: approver `principal_class = 'internal'`, unconditional), C (`SEP-1`: no requester or approver in the resolved beneficiary set)** | A re-attribution moves future commission from one affiliate to another, so **both** parties' beneficiary sets are in scope. `B(O)` is the union of the **reflexive ancestor closures** (each node itself plus its ancestor chain, per §6.5.2's Fix Round 2 correction) of the **new** `affiliate_node_id` and the **superseded** one, each expanded to its affiliate-account persons plus its declared beneficial-interest persons (§6.5.2). Running it against only the beneficiary would let the *losing* side's people drive a reversal, and running it down-tree would miss the override-earning parent entirely |
| 2 | **A fail-closed default threshold: `0`, `required_approvals = 2`** | No configured policy ⇒ **every** re-attribution is four-eyes. Mirrors `internal/withdrawal/policy.go`'s `defaultApprovalPolicy` (`policy.go:136`). The Wave 1.5 wording would have resolved an absent config to "below threshold," i.e. no approval at all, for every re-attribution ever performed until someone noticed |
| 3 | **`threshold_at_decision` and `amount_at_decision` captured on the decision row, by value** | Without them, a later policy change makes it impossible to say whether a historical re-attribution was correctly governed. The materiality measure is the **commission already accrued and not yet discharged against the superseded attribution**, plus the projected forward exposure under the agreement — computed from ledger-derived measures, never from an affiliate-owned counter (AFF-3) |
| 4 | **The approval is CONSUMED, not referenced** | Same pattern as §7.1 rule 10 — the approval is spent in the same transaction as the superseding row is written, matched against a payload containing the superseded id, the new node id and the reason code. A re-attribution whose approval was already spent on a different re-attribution fails at the database |
| 5 | **A literal self-approval-rejection test, and a literal affiliate-principal-rejection test** | The Stage 4H-B0-R6 P1's lesson is that an inert four-eyes check looks identical to a working one until someone writes the adversarial test. Both tests are required, not one |
| 6 | **`parent_operation_id`** (doc 34) | A retried re-attribution resolves to the same economic operation, never a fresh authorization |

**This duplicates §6.5 deliberately.** The two controls have the same
shape and different subjects (one governs money leaving, one governs who
money will belong to), and a reader implementing either one must see the
full requirement without a cross-document scavenger hunt. Where they
could drift, §6.5 is the normative statement of the conjuncts and this
table is its application; any future change to the conjuncts must be
applied to both, and invariant **AI-6** (§11) checks them together.

### 5.4 Promo codes are attribution tokens, not a second coupon system

`bonus-engine` owns coupons (doc 29 BC-06; doc 10 W4 "Coded bonuses").
An affiliate promo code must not become a parallel value-granting
mechanism.

**Decision:** an affiliate `PromoCode` is an **attribution token** that
resolves to `(affiliate_node_id, agreement_version)` and *may* carry an
optional **reference** to a Bonus Offer. Entering it records attribution;
**any bonus value it implies is granted only by the Bonus Engine, through
Bonus's own coupon/redemption path, under Bonus's own full gate**
(`AssetAuthorization` → RG → Risk, doc 10 §T.1). Affiliate supplies no
amount, no terms, no eligibility override.

Code-space collisions between affiliate promo codes and bonus coupon
codes are prevented by a single namespace check at authoring time, owned
by `bonus-engine` (the domain that already owns code redemption). Filed
as **DEP-AFF-3**. Doc 29 §5.1's enumeration/rate-limit discipline for the
coupon endpoint (`security`-owned, OI-9) applies to any affiliate code
entry surface unchanged.

### 5.5 Attribution model — versioned configuration, never a code path

Model classes: `last_click`, `first_click`, `last_click_within_window`,
`multi_touch(weights)`, `code_overrides_click`, `manual`. Each with a
configured attribution window, a tie-break rule, and a declared
precedence between a code and a click.

- The model is **tenant/brand-scoped configuration**, versioned, with the
  applied version recorded on every decision (§5.3).
- The model is **deterministic**: same evidence + same model version =
  same decision. No randomness, no wall-clock-dependent tie-break; ties
  break on a declared, recorded rule (e.g. earliest `occurred_at`, then
  lowest click id).
- **Replay** is possible for any past decision because the evidence
  (§5.1/§5.2) is immutable and the model version is immutable. Where a
  model input is *not* immutable, the decision snapshot (§5.3) is the
  authoritative reconstruction — the same honest limit doc 30 §6 records
  for dynamic segments.

### 5.6 Tamper-resistance, privacy, and what is deliberately not built

**Tamper-resistance mechanisms, in order of load-bearingness:**

1. Server-minted, integrity-protected `tracking_token` — a client cannot
   assert an affiliate identity (§5.1).
2. Server-side tenant/brand/affiliate resolution — never a query
   parameter (`CLAUDE.md`).
3. Append-only clicks/candidates/attributions with DB-level mutation
   denial (ADR 0013's trigger pattern).
4. Supersede-never-edit for re-attribution, with reason code, actor and
   four-eyes above a threshold (§5.3).
5. Every mutation of a link, code, agreement, rule, accrual or attribution
   writes an `audit.Record` in the same transaction (`CLAUDE.md`;
   ADR 0013).
6. Idempotency on click and conversion ingestion, DB-enforced, never
   check-then-insert.

**Deliberately NOT built** (`CLAUDE.md`'s no-scope-expansion rule): a
cryptographic hash chain or Merkle log over click records. Append-only
tables plus the immutable `audit_log` are the platform's established
tamper-evidence mechanism and are consistent with how far this project
has gone for *financial* records; a bespoke chain for a pre-identity
marketing record would be a heavier control than the ledger itself
carries. Recorded as a conscious decision, not an omission.

**Privacy (routed to `identity-compliance`/`security`, DEP-AFF-2):**
click tracking collects IP and user agent before any account exists, and
it is the first place in this platform where a *tracking-consent*
question (cookies/identifiers) arises. Doc 16 has no model for it and
doc 31 §8.1 already establishes that consent is `identity-compliance`'s
to own. Affiliate must not invent a second consent concept; it records a
`consent_state` **reference** and fails closed where the model requires
consent it does not have. Retention of click PII inherits doc 16's
unresolved retention-period question (a legal decision, not an
engineering one).

---

## 6. Commission — calculation as a claim, never as money

### 6.1 The revenue measure is not Affiliate's to define

**AFF-3, binding:** a revenue-share commission is computed from a
**canonical, ledger-derived revenue measure** (NGR/GGR as defined by
`ledger-finance` and produced through `data-analytics`'s ledger-derived
models, doc 12). Affiliate **never** computes its own revenue figure from
its own tables, and no `affiliate_*` table maintains a running revenue or
liability counter.

This is doc 29 §5.2's rule for campaign reporting ("every figure is
derived from `ledger_entries`, never from a Bonus-owned balance column")
and doc 26's "Retail owns no money", applied here. The failure being
prevented is concrete and common in this industry: an affiliate platform
whose NGR disagrees with the operator's ledger, with no way to say which
is right.

What "NGR" means for commission (which deductions: bonus cost, provider
fees, PSP fees, chargebacks, tax) is a **financial and commercial
definition**, owned by `ledger-finance` (definition) and the human
(commercial policy) — not invented here. Routed as **DEP-AFF-4**.

### 6.2 `CommissionAccrual` — a claim, with its inputs

Per `(affiliate_node, CommissionPeriod, rule_version, asset)`:
append-only, immutable once written:

| Field | Rule |
|---|---|
| `commission_rule_version_id`, `agreement_version_id`, `attribution_model_version_id` | EDR-R1 references (immutable) |
| `input_refs[]` + input values with their `as_of` | the ledger-derived measures used, by reference **and** by value (EDR-R1: copy the mutable with its as-of), so the accrual is reconstructable even if a report definition later changes |
| `asset_code`, `amount` | **integer minor units** against the Asset Registry's exponent, `NUMERIC(38,0)`-compatible decimal representation over any wire (ADR 0021; doc 21's exponent-18 correction — never `int64`). **Never floating point** |
| `rounding_rule_id` | the applied `rounding_rules` row (ADR 0021 DS-1/DS-2: round once, at the final monetary boundary, via the one shared function, never an implicit cast) |
| `computed_at`, `computed_by` | `actor_type = system` |
| `status` | **(corrected, Fix Wave — LF-9)** `draft` → `pending_approval` → `approved` \| `rejected` → `recognized` → `discharged` \| `carried_forward`. The single `settled` value is withdrawn: it collapsed two economically distinct events (liability recognition and its discharge) into one word and left rule 5's liability question with no trigger. §7.1 rule 7 |

A recomputation never edits an accrual; it writes a
`CommissionAdjustment` (a compensating record, `CLAUDE.md`'s corrections
rule applied one domain up from the ledger).

### 6.3 Negative commission, clawback, and carryover — flagged, not invented

Voided deposits, chargebacks, bonus abuse and player-fraud reversals can
make a period's commission negative. Three candidate policies exist
(carry the negative forward within the affiliate's own future periods;
claw back already-settled money; absorb it), and choosing between them is
**a commercial decision with a financial posting consequence** — not an
engineering judgment call.

- `architect` does **not** select one.
- The *mechanism* this architecture supports without prejudging the
  policy: negative `CommissionAdjustment` records plus an
  agreement-version-level `negative_carryover_policy` field.
- Clawback of already-settled money is a `ledger-finance` posting
  question and a human commercial question. Routed as **OI-AFF-3** and
  **DEP-AFF-4**.

### 6.4 Sub-affiliate override

A sub-affiliate override is a commission rule whose measured quantity is
a *descendant node's* accrual, resolved through the hierarchy closure
(§3). Two guards:

- **Depth and total-payout bounds are configuration**, validated at rule
  authoring time, so a deep or mis-built tree cannot produce an unbounded
  or >100% total payout. (The check is Affiliate's own authoring
  validation, not a Risk rule — it caps a *commercial term*, not a
  player-facing value movement; the doc 31 §8.4 line applies in the same
  shape.)
- **No cycles** — guaranteed by `agentnetwork`'s own acyclicity
  invariant, not re-implemented here.

### 6.5 Approval — corrected for SEC-W15-01 (P0)

#### 6.5.0 What was wrong

The Wave 1.5 original required only that "the approver must be a
distinct, resolved, active person from the requester." `security` found
(**SEC-W15-01**, P0) that this is **satisfiable by two colluding
external affiliate accounts**: §3.2 makes an affiliate user an
`identity.StaffUser`, so an affiliate principal *is* a staff principal,
and two of them in the same affiliate organisation are two "distinct,
resolved, active" persons. The four-eyes control on the platform's
affiliate-money path would have been discharged entirely by the
counterparty being paid.

The distinctness test was necessary and nowhere near sufficient. Three
conjuncts are required, and — `security`'s exact wording — **the
principal-class test must be an explicit conjunct of the governance
trigger, not emergent**.

#### 6.5.1 The three conjuncts

`CommissionApproval` requires a mandatory reason code and **four-eyes**,
where a valid approval satisfies **all three** of:

| # | Conjunct | Mechanically checkable? |
|---|---|---|
| **A** | **Distinct, resolved, active person from the requester.** The Stage 4H-B0-R6 P1 shape: the self-approval check was *inert* because no code path could set `person_id` on the approving account. Any affiliate four-eyes control must not repeat that, and must be verified by a **literal self-approval-rejection test** (doc 29 BC-18's precedent) | Yes — existing `staff_users.person_id` linkage |
| **B** | **`AFF-4E-1` — the approver's `principal_class` is `internal`.** Not "an affiliate principal who happens to be on our side": the positively-stored class column of AFF-C1 (§3.2), tested positively. This is an **explicit conjunct in the approval predicate**, evaluated as part of the same selection the approval is consumed by (§7.1 rule 10) — never an emergent property of "well, only internal staff have this permission." **Unconditional: no threshold, no delegation, no emergency override, no service-identity carve-out** | Yes — a column comparison in the same trigger family as every other control here |
| **C** | **`SEP-1` — no approver, and no requester, is in the operation's resolved beneficiary set.** This is `security`'s platform-wide actor≠subject/beneficiary invariant (`security-architecture.md` §W15.1), adopted here rather than reinvented. Affiliate supplies a **beneficiary resolver** and an **enforcement point**; the comparison, NULL handling and refusal semantics are the shared template | Yes for the graph half; attested for the ownership half — §6.5.2 |

#### 6.5.2 Conjunct C — Affiliate's beneficiary resolver

`security` §W15.1.2 requires each adopting domain to supply exactly three
things: a beneficiary resolver, an enforcement point, and nothing else.
Affiliate's:

**The resolver — and note it runs UP the tree, not down.** `B(O)` for a
`CommissionApproval`, a `CommissionSettlementInstruction`, a
re-attribution, or an agreement/rule-version activation is:

> the commission owner node's **reflexive ancestor closure** — the node
> **itself**, together with its ancestor chain — under any sub-affiliate
> override agreement (§6.4 — **a parent node benefits from a child's
> accrual**, so the parent's people are beneficiaries too, **and the
> node's own people are beneficiaries of its own accrual**, which a
> strict-ancestors-only reading silently excluded), expanded to that
> closure's **affiliate-account persons** plus its **declared
> beneficial-interest persons** (§6.5.2.1).

**Correction (Wave 1.5 Fix Round 2, `code-reviewer`'s NEW-6 — see §15):**
the prior text on this line read "ancestor chain" (strict ancestors,
excluding the node itself). That has two independently reachable defects,
both closed by making the closure reflexive: (1) a **fail-open** — an
internal staff member with a declared beneficial interest in the node
*being approved for itself* (not a parent) was not in a strict-ancestor
set and so was not blocked, directly against this control's own stated
purpose; (2) a **deadlock** — a flat/root affiliate node (this platform's
own recommended first slice, §14) has no ancestors at all, so a
strict-ancestor `B(O)` resolves **empty**, and per §6.5.2's own
fail-closed table an empty result is a refusal, not a pass — meaning no
commission on a flat/root node could ever be approved. Reflexivity closes
both: the node is trivially a member of its own closure, so `B(O)` is
never empty and always includes the node's own beneficiaries. This
mirrors `security-architecture.md` §W15.2.7's identical correction to its
own restatement of this resolver, which explicitly left the edit to this
document's owning revision.

This corrects a real error in this document's own first Fix-Wave draft,
which wrote the test as "the approver is not within the **subtree** the
decision benefits" — i.e. **descendants**. That is the wrong direction
and would have missed the most obvious conflict: the *parent* who earns
an override on the accrual being approved. A descendant-only test is not
merely incomplete; it checks the one set of nodes that does **not**
automatically benefit.

**The enforcement point:** the row whose insertion *authorizes* the
operation — the `CommissionApproval` row and the settlement instruction's
change-request row — never a row that merely reports the outcome
afterwards.

**Totality and fail-closed, per §W15.1.2:** the resolver is **total** — it
returns a row set or raises, and **an empty result is a refusal, not a
pass**. "This operation benefits nobody identifiable" is exactly as
ineligible as "this operation benefits the actor." Every one of these
refuses:

| Condition | Result |
|---|---|
| Approver's `principal_class` is NULL, absent, or unrecognized | **Refuse** |
| The reflexive ancestor closure cannot be fully resolved (hierarchy primitive unavailable, unknown node, unresolvable agreement version) | **Refuse** |
| A beneficiary person cannot be resolved from an affiliate account | **Refuse** |
| The node's beneficial-ownership attestation is **missing** (`undeclared`) | **Refuse** every approval on that node's objects |
| The attestation exists but is **stale** past its re-attestation period | **Refuse** — explicitly not "stale ⇒ warn" |
| An affiliate-class principal attempts an approval | **Refuse** (`AFF-4E-1`) |

The commercial consequence is that an accrual stays `pending_approval`
until the missing information exists, and that is the correct trade: an
**unpaid** affiliate is a commercial problem with a queue, an owner and a
remedy; an **incorrectly paid** one is an unrecoverable outflow to an
external party, usually outside our recovery options.

##### 6.5.2.1 Beneficial-ownership attestation — the physical home

`security` §W15.2.5 specifies the control and leaves the physical home
(node vs. agreement) to `architect`. **Decision: the node.**

An append-only **`affiliate_beneficial_interest_attestations`** table —
never a mutable column set — with the **node** carrying a pointer to its
current row:

| Field | Rule |
|---|---|
| `node_id`, `tenant_id` | scope, composite-FK'd |
| `declaration` | `none_internal` \| `internal_interest_declared` \| `undeclared`. **`undeclared` is the default state of a new node**, and it refuses approvals |
| `declared_interest_person_ids[]` | resolved `persons` references for every internal person declared to hold an interest |
| `attested_by_principal_id`, `attested_at`, `attestation_reason_code` | who declared, when, why |
| `attestation_period_days` | tenant configuration |
| `next_attestation_due_at` | derived; past it the node is `attestation_stale` |
| `supersedes` / `superseded_by` | a change is a **new row**, never an edit (the `withdrawal_policies_deny_update` precedent) |

**Why the node rather than the agreement**, since the choice is
`architect`'s: an interest attaches to the *structural position that
earns*, and it must survive an agreement being renegotiated, expiring, or
being replaced — an agreement-homed attestation would lapse exactly when
a new agreement version is cut, which is a moment of *heightened* rather
than reduced conflict risk. The reflexive ancestor-closure resolver above
also walks **nodes**, so a node-homed attestation is readable in the same traversal
rather than requiring a second join per ancestor. The cost is that an
entity holding several nodes attests several times; that is the correct
direction (over-declaration, not under-declaration) and is the same
trade-off §6.5.2's fail-closed table makes everywhere else.

**Effect, and this is the whole point of the control:**
`declared_interest_person_ids` **join the node's `SEP-1` beneficiary
set**. A declared internal owner is therefore structurally unable to act
on, or approve, that node's money. Declaring costs the declarer nothing
discretionary; it is the mechanism that prevents them from ever being the
single point of failure on their own node.

**Stated limitation, not hidden:** an attestation is a *declaration*. It
catches an honest conflict, and it makes a later-discovered undeclared
one an auditable falsehood rather than an ambiguity. It does not detect a
deliberate lie at the moment it is told, and no engineering control in
this platform can. That residual is a compliance/HR control. It is named
here so nobody reads this half of conjunct C as equal in strength to the
graph half.

**What `AFF-4E-1` + `SEP-1` together do NOT close**, per `security`
§W15.2.3 and restated so it is not lost in this document:

1. Two **internal** staff colluding. Not affiliate-specific, not claimed
   closed here.
2. Anything about whether the commission **amount** is right — these are
   independence controls, not correctness controls. AI-7's ledger-derived
   measure and DEP-AFF-4 own that.
3. An internal staff member with an *undeclared* interest, until the
   attestation regime catches it.

#### 6.5.3 The threshold, and its fail-closed default

**Four-eyes above a configurable materiality threshold** — the identical
control `CLAUDE.md` mandates for manual balance adjustments and ADR 0024
established for withdrawals. **Corrected in the Fix Wave (`security`
SEC-W15-07's defect shape, applied here too):**

- **The default when no threshold is configured is `0`**, with
  `required_approvals = 2` — i.e. *every* commission approval requires
  four-eyes until a tenant explicitly configures a lighter policy. This
  mirrors `internal/withdrawal/policy.go`'s `defaultApprovalPolicy`
  (`policy.go:136`, `ThresholdAmount: 0`), which exists precisely because
  a config-absent default of "no threshold ⇒ no approval" ships the
  control **inert**.
- **`threshold_at_decision` and `amount_at_decision` are captured on the
  approval row**, by value. Without them, a later threshold change makes
  it impossible to say whether a historical approval was correctly
  governed — the EDR-R1 rule (doc 30 §7.1: copy the mutable with its
  as-of) applied to a control decision rather than to an eligibility one.
- **Conjuncts A, B and C apply at every value, including below the
  threshold.** The threshold governs *how many* approvals, never *who may
  give them*. A below-threshold single approval still may not come from
  an affiliate principal or from inside the benefiting subtree.

#### 6.5.4 What Affiliate owns here, and what it does not

`identity-compliance` is producing the affiliate identity/authority model
in parallel this round (task `4HB1FW-05`: entity vs. account vs.
beneficial owner), and `security` owns the actor≠subject invariant
(`4HB1FW-04`). **This document does not invent either.** What it
specifies is Affiliate's own side of the contract — the fields and checks
the trigger needs:

| Affiliate supplies | Depends on |
|---|---|
| `benefiting_node_id` on every approval and re-attribution | — |
| The **reflexive ancestor-closure** beneficiary resolver (§6.5.2) — total, deterministic, evaluated in the same transaction as the authorizing write, empty-result-is-refusal | `agentnetwork` (DEP-AFF-5, AFF-C2, OI-AFF-2) |
| `approver_principal_class` captured on the approval row, and the positive `= 'internal'` predicate conjunct (`AFF-4E-1`) | `security` — **decided**: `staff_users.principal_class`, `NOT NULL`, no permissive default (AFF-C1) |
| The `affiliate_beneficial_interest_attestations` table, node-homed (§6.5.2.1), and the `undeclared`/stale refusals | `security` — **specified** (§W15.2.5); `identity-compliance` for who may attest and the cadence |
| `threshold_at_decision`, `amount_at_decision`, `required_approvals_at_decision` | — |
| Consumption of the approval at settlement (§7.1 rule 10) — `security`'s **REQ-AFF-CONSUME-1**, routed to `architect` | `security` + `ledger-finance` (DEP-AFF-6) |

---

## 7. Commission settlement — the contract handed to `ledger-finance`

**`architect` does not design the posting shape.** This mirrors exactly
how ADR 0035 handled retail: `ledger-finance` owned `agent_float`
accounting while the hierarchy/operational architecture was
`architect`'s, and doc 26 records the rule that if the two ever disagree
on monetary treatment, **ADR 0035 wins.** The same precedence applies
here: if this document and `ledger-finance`'s eventual commission ADR
disagree on anything monetary, **`ledger-finance` wins.**

What `architect` specifies is the **trigger and the data contract**:

```
CommissionSettlementInstruction            ← Affiliate hands this to Ledger
  instruction_id            (idempotency key; DB-unique, never check-then-insert)
  tenant_id                 (server-resolved; NOT NULL; RLS home per §7.1 rule 10)
  affiliate_node_id
  period_id                 (CommissionPeriod)
  asset_code                (Asset Registry code)
  amount                    (integer minor units, decimal-string wire form)
                            ── a DECLARED EXPECTATION, re-derived and
                               cross-checked by ledger; §7.1 rule 6
  rounding_rule_id          (ADR 0021; the re-derivation applies THIS row)
  accrual_refs[]            (the approved accruals being settled)
  adjustment_refs[]         (negative/compensating records applied)
  accrual_set_hash          (content hash over the sorted accrual_refs +
                             adjustment_refs; the payload the four-eyes
                             approval is matched against; §7.1 rule 10)
  approval_refs[]           (CommissionApproval rows — CONSUMED at
                             settlement, not merely referenced; §7.1 rule 10)
  reverses_instruction_id   (nullable; non-null ⇒ this is a clawback/
                             reversal, §7.1 rule 8)
  commission_rule_version_id, agreement_version_id
  reason_code
  requested_by              (actor_type/actor_id)
  requested_at              (clock_timestamp())
  parent_operation_id       (EconomicOperationIdentity, doc 34 — a retry
                             or a resumed/paginated period settlement
                             resolves to the SAME parent operation,
                             never a fresh authorization)
```

And the **five rules that bind whatever posting shape `ledger-finance`
chooses**, each inherited rather than invented:

1. **Affiliate never decides monetary treatment itself; it supplies the
   instruction and calls `Post` with the split `ledger-finance`
   specifies.** *(Corrected in the Fix Wave —* `code-reviewer` *finding
   **P1-4**.)*

   The Wave 1.5 original stated an **absolute** ban: "Affiliate never
   calls `ledger.Post` itself, and `internal/affiliate` never imports
   `internal/ledger`/`internal/wallet`… doc 26's identical rule for
   retail." That misstated the precedent it cited and, as written, made
   commission settlement unimplementable from `internal/affiliate`:

   - doc 02's actual rule carries a **qualifier this document dropped**:
     *"No retail package may import `internal/ledger` or `internal/wallet`
     **to decide anything monetary itself**."*
   - doc 26 §7.1's own package table defines `internal/retail` as
     composing `agentnetwork` + `identity` + `rg` + `risk` + **`ledger`**,
     with "its own balance" in the *never* column — i.e. the precedent is
     explicitly *import-and-call, never decide*.

   **The corrected rule, binding:**

   | Affiliate may | Affiliate may never |
   |---|---|
   | Import `internal/ledger` and call `ledger.Post` with the account/split/transaction-type instruction `ledger-finance` specifies | Choose which accounts a commission posting touches, or invent a transaction type, account type or posting shape |
   | Hand `ledger` a `CommissionSettlementInstruction` (§7's contract) and let `ledger` derive the monetary content from it (§7.1 rule 6) | Compute or assert the settled amount as an authority (§7.1 rule 6 — the ledger re-derives it) |
   | Read ledger-derived measures through `data-analytics`'s canonical models | Read `ledger_entries` directly to compute a revenue figure (AFF-3, unchanged) |
   | Post inside the same `pgx.Tx` as the accrual-state transition and the audit record, so settlement is atomic | Hold, cache or project a balance; import `internal/wallet` at all |

   `internal/wallet` remains fully off-limits: a commission settlement
   never touches a player wallet (rule 2), so there is no legitimate
   reason for the import, and its absence is a mechanically checkable
   proxy for rule 2.
2. **Never a player account.** A commission settlement moves value
   between platform/affiliate-side accounts. It must never debit or
   credit `player_cash`, `player_bonus`, `player_locked_cash` or
   `player_locked_bonus`, and must never appear in a player's balance
   projection. (An affiliate who is *also* a player is two distinct
   entities; the platform must not net them.)
3. **Double-entry, append-only, idempotent** by DB constraint on
   `instruction_id` (or its composition into the existing
   `(provider_id, provider_tx_id)` / `(tenant_id, idempotency_key)`
   routing, ADR 0038 §14/§14.6 — `ledger-finance`'s call).
4. **Compensating entries only** for corrections; never an edit of a
   settled posting.
5. **Whether an approved-but-unsettled accrual is a recognized liability
   on the balance sheet** is `ledger-finance`'s determination (the same
   class of question as ADR 0032 §2's open `bonus_expense` presentation
   decision), not this document's. **What this document must supply
   either way is the trigger** — see §7.1 rule 7.

### 7.1 Five further rules, added in the Fix Wave

`ledger-finance`'s Phase 2 review (**LF-6**, **LF-9**) and `security`'s
(**SEC-W15-12**) found five real gaps in the contract above. Each is
fixed here as a structural requirement on the instruction, not as a
posting decision — the posting shape remains `ledger-finance`'s.

#### Rule 6 — the ledger RE-DERIVES `amount`; it never trusts the caller's claim

The original instruction carried both `amount` and `accrual_refs[]`, with
no stated relationship between them. That makes `amount` an unverified
assertion from the domain that most benefits from it being wrong.

**Binding:** `CommissionSettlementInstruction.amount` is a **declared
expectation**, not an authority. `ledger-finance`'s settlement path
re-derives the amount by summing the referenced approved accruals and
applied adjustments — inside the settlement transaction, from the
immutable accrual/adjustment rows — and **aborts on any mismatch**
against the declared value. The declared field is retained precisely so
the mismatch is detectable: dropping it would remove the cross-check, not
add safety. Rounding applies once, at the final monetary boundary, via
`rounding_rule_id`'s shared function (ADR 0021 DS-1/DS-2) — the re-derivation
must apply the *same* rule row the instruction names, or the mismatch is
the rounding rule's identity, which is equally a defect.

#### Rule 7 — "settled" splits into RECOGNITION and DISCHARGE, each with its own idempotent trigger

The original status enum ended `… → approved → settled`, collapsing two
economically distinct events into one word and leaving the liability
question (rule 5) with no trigger at all. `ledger-finance` **LF-9**:

| Event | What it means | Trigger | Idempotency |
|---|---|---|---|
| **Recognition** | The obligation is recorded — an approved accrual becomes a liability the platform owes | `CommissionApproval` completing (§6.5), **if** `ledger-finance` selects the recognise-at-approval model | The approval's own consumed four-eyes record (rule 10) |
| **Discharge** | The obligation is extinguished — money has actually left | A **`payments`**-originated settlement confirmation, **not** an Affiliate-originated one | Its own DB-enforced key, minted by `payments`, referencing the instruction |

Two consequences the original text hid:

- **Whether recognition happens at approval or only at discharge is
  `ledger-finance`'s model choice** (rule 5, DEP-AFF-4). Affiliate must
  supply **both** triggers regardless of which is selected, because the
  cost of supplying an unused trigger is a no-op and the cost of a
  missing one is an unimplementable model.
- **Discharge is `payments`' event, not Affiliate's.** Affiliate does not
  know when a bank transfer cleared, and an Affiliate-asserted "settled"
  would be exactly the "vendor computing commission is not a vendor
  authorized to move our money" failure §0 warns about, one layer in.
  The status enum therefore becomes
  `draft → pending_approval → approved | rejected → recognized →
  discharged | carried_forward`, with `recognized` collapsing into
  `approved` if `ledger-finance` selects discharge-only recognition.

#### Rule 8 — clawback and reversal are an explicit instruction shape

§6.3 records negative commission as a *policy* question (correctly, and
it stays unselected). But the original left the **mechanism** for
reversing an already-settled instruction entirely unspecified, which
`ledger-finance` **LF-9** flagged.

**Binding:** a reversal is a `CommissionSettlementInstruction` carrying
**`reverses_instruction_id`** (the instruction being reversed) — never an
edit, never a deletion, never a negative row silently folded into the
next period. It carries its own `instruction_id`, its own approval refs,
its own reason code, and its own consumed four-eyes record. Rules 1–7
apply to it unchanged. `ledger-finance` decides the posting (a
compensating entry, per rule 4); Affiliate supplies the instruction and
the lineage. A reversal instruction whose `reverses_instruction_id`
names an instruction that was never discharged is a *cancellation* of a
recognized-but-undischarged liability, which is a different posting —
the distinction is `ledger-finance`'s, and the field that lets them tell
the two apart is the referenced instruction's own status.

#### Rule 9 — accrual-level idempotency (LF-6): `UNIQUE (tenant_id, accrual_id)` on a settled-accrual link table

`ledger-finance` **LF-6**, and this one is a genuine double-settlement
vector, not a theoretical one. The original made settlement idempotent at
the **instruction** level (`instruction_id`, DB-unique). That does not
prevent **two distinct instructions with overlapping `accrual_refs[]`**
from both posting: each has a unique `instruction_id`, so each passes the
constraint, and the same accrual is paid twice. Two plausible paths to it
with no attacker involved: an operator retries a settlement whose
response was lost and the retry mints a new `instruction_id`; or two
period-close runs overlap at a boundary.

**Binding:** an append-only **settled-accrual link table** carrying
`(tenant_id, accrual_id, instruction_id)` with

```
UNIQUE (tenant_id, accrual_id)
```

written **in the same transaction as the posting**. A second instruction
referencing an already-linked accrual fails on the constraint, at the
database, never by a check-then-insert read (`CLAUDE.md`: "enforced by
the database, not 'check then insert' application logic").

This deliberately **mirrors the conversion-occurrence pattern** this
platform already uses for the same class of problem — a unique key on the
*thing consumed*, not only on the *operation consuming it* — and it is
the same shape as doc 10 W5's `BulkGrantJobItem`
`UNIQUE (tenant_id, bulk_grant_job_id, player_account_id)`, which is what
makes a resumed bulk job safe.

Two details that are easy to get wrong and are therefore stated:

- **The uniqueness is on `(tenant_id, accrual_id)`, not on
  `(tenant_id, accrual_id, instruction_id)`.** Including the instruction
  in the key would make the constraint satisfiable by any new
  instruction, i.e. would restore exactly the defect.
- **A reversal (rule 8) must not be blocked by it.** The link row is
  *released* — by an append-only `unlinked` successor row plus a partial
  unique index on the live rows, never by a `DELETE` — when, and only
  when, a reversal instruction referencing its instruction is posted. An
  accrual whose settlement was reversed is legitimately re-settleable;
  one that simply exists twice in two instructions is not.
- **Adjustments get the same treatment**: `UNIQUE (tenant_id,
  adjustment_id)` on the same link table's adjustment half. A
  compensating adjustment applied to two instructions is the same
  double-count with the opposite sign.

#### Rule 10 — the four-eyes approval is CONSUMED, atomically, at settlement (SEC-W15-12)

`security` **SEC-W15-12**: the original carried `approval_refs[]` as a
**data field on the instruction**. A data field is not a control. Nothing
in the design prevented an instruction from naming an approval that was
already used by an earlier instruction, or one that does not satisfy the
four-eyes predicate, or one belonging to a different period — the
reference was recorded, never *checked* and never *spent*.

**Binding:** settlement consumes the approval through the platform's
already-implemented, already-reviewed pattern —
`asset_change_consume_approved_request` (migrations
`0044_asset_registry_failclosed_and_dual_control.up.sql:474`, hardened in
`0047_asset_registry_dual_control_hardening.up.sql:271`). That function's
properties are exactly the ones needed here and are adopted wholesale:

| Property of the existing pattern | Why it is needed here |
|---|---|
| `SELECT … FOR UPDATE` on the pending request row | Two concurrent settlements cannot consume one approval |
| `EXISTS (… approver_principal_id <> requested_by_principal_id)` as part of the *selection predicate*, not a separate check | The four-eyes test cannot be skipped by a caller that forgets to call it — an approval that fails it is simply not selectable |
| `NOT EXISTS (… decision = 'reject')` | A rejected-then-re-approved request cannot be consumed |
| Payload containment (`r.payload @> p_payload_match`) | The approval must match *this* settlement's pinned content (period, node, asset, accrual set hash), not merely exist |
| `UPDATE … SET state = 'applied'` in the same transaction | The approval is **spent**. A replay finds nothing pending and raises |
| `RAISE EXCEPTION` when nothing matches | Fail closed and loud, aborting the whole statement |

`security` routed this to `architect` as **REQ-AFF-CONSUME-1**, in its
§B1.2 item 4 shape ("select the matching `pending` request `FOR UPDATE`,
require the approval rows, mark it `applied` in the same statement — so
that one approval authorizes one settlement, once"). The implemented
function in this repository matching that shape is the **`asset_change_*`**
one named above, and it is the concrete precedent being adopted. Whether the commission version is a new sibling function
or a generalization of the existing one is `security`'s and
`ledger-finance`'s call; the *properties* above are binding either way.
Recorded as **DEP-AFF-6**.

**RLS home (also SEC-W15-12).** The original never said where
`CommissionSettlementInstruction` lives. It is an `affiliate_*` table:
`tenant_id NOT NULL`, `FORCE ROW LEVEL SECURITY`, the
`app.player_account_id IS NULL` conjunct on its staff-scope policy, and —
binding, and **not** subject to the ordinary subtree carve-out — **no
affiliate-readable policy at all**. An affiliate may see the *outcome*
of a settlement in its own statement (§10); it may never read the
instruction, its approval refs, its accrual link rows, or another node's
anything. Recorded as invariant **AI-15**.

Routed as **DEP-AFF-4** (§12). Payout *rails* (how money actually leaves
— bank transfer, PSP payout, crypto) are `payments`', not Affiliate's,
and are out of scope here — but note that rule 7's **discharge trigger
is `payments`'**, which makes `payments` a named participant in the
settlement contract rather than a downstream detail. Recorded as
**DEP-AFF-7**.

---

## 8. The full chain: Affiliate → Acquisition → Segment → CRM Journey → Bonus Offer → Bonus Grant

The directive asks exactly how this works **without coupling Affiliate to
financial posting.** Step by step, with what crosses each boundary:

| # | Step | What crosses | What does NOT cross |
|---|---|---|---|
| 1 | **Affiliate → Acquisition** | A click is recorded (§5.1); on registration/first qualifying event, `PlayerAttribution` is written (§5.3) — a *fact about origin*, tenant-scoped, server-resolved | No player balance, no bonus, no eligibility effect. Attribution does not authorize anything |
| 2 | **Acquisition → Segmentation** | Segmentation reads the attribution projection as criterion **C-21** (doc 30 §4) — "acquired via affiliate node subtree X", "acquired in the last 30 days" — through Affiliate's read interface, never its tables | Segmentation does not compute attribution, does not re-run the model, and does not store a copy. C-21 is `BLOCKED` until this domain exists |
| 3 | **Segmentation → CRM** | An `Audience` include/exclude term resolves via `segment.Resolve`, returning an `Evaluation` (result + `segment_version_id` + `criteria_hash` + `as_of`), which CRM records (doc 31 §5) | A segment result is not an authorization (doc 30 §8.1). CRM builds no criteria of its own |
| 4 | **CRM Journey → Bonus Offer** | **(corrected, Fix Wave)** `bonus-engine`'s own surfaces, per doc 10 **N2.4** and doc 31 §7.2.1 — the identical `BulkGrantJob`-creation surface (doc 10 W5) or single-Grant staff-equivalent surface, as an `ActorService` caller, plus the read-only `CheckOfferEligibility` preview. An `offer_id` + `offer_version_id` **reference**, the player/list/segment/segment-set target, an idempotency key, the trigger reference, the audience evidence, and the `parent_operation_id` of the approved `EngagementCampaign` activation (doc 31 §7.2.4) | **No amount, no wagering terms, no eligibility override, no CRM-specific target shape.** CRM cannot grant, cannot retry with altered parameters, and cannot issue a grant-causing call with no approved parent operation. `bonus.RequestOfferGrant` is **withdrawn** — it never existed in `bonus-engine`'s design |
| 5 | **Bonus Offer → Bonus Grant** | Bonus runs its unmodified gate — `AssetAuthorization.CheckEligibility(operation = wagering)` → `rg.EvaluateEligibility` → `risk.Evaluate`, in the same transaction as the effect, all fail-closed (doc 10 §T.1; doc 29 §4.2, §8 BI-7) — then issues, or denies | Affiliate has **no** presence in this step. No affiliate identity, node id, code or commission term appears in the gate, the Grant, or the posting |
| 6 | **Grant → Ledger** | `ledger.Post` under ADR 0032's posting map, with the eligibility decision record (doc 30 §7) written in the same transaction | Nothing affiliate-owned. A Grant is not a commission event |
| 7 | **Ledger → revenue measure → Commission** (the *parallel*, decoupled path) | Commission accrual (§6.2) reads a canonical **ledger-derived** revenue measure for the period; the player's attribution (§5.3) determines *which affiliate* the revenue is attributed to | The commission path is **triggered by period close and ledger-derived revenue, never by a Grant, a CRM send, or a journey step.** Affiliate never observes an individual Grant's terms |
| 8 | **Commission → settlement** | §7's `CommissionSettlementInstruction`, handed to `ledger-finance`; Affiliate calls `ledger.Post` with the split `ledger-finance` specifies (§7.1 rule 1), consuming the four-eyes approval and writing the accrual link rows in the same transaction (§7.1 rules 9–10) | Affiliate never **decides** monetary treatment, never touches a player account, never computes the posting shape, and never asserts the settled amount as an authority (the ledger re-derives it, §7.1 rule 6) |

**The decoupling, stated as an invariant — AFF-2:** the acquisition chain
(steps 1–6) and the commission chain (steps 7–8) share exactly one thing:
the immutable `PlayerAttribution` row. Neither chain calls the other.
Removing the entire commission subsystem would not change a single
player-facing outcome in steps 1–6; removing CRM would not change a
single commission figure.

---

## 9. Integration with the other domains

| Domain | Affiliate's relationship | Never |
|---|---|---|
| **CRM** (doc 31) | Attribution feeds audiences (via Segmentation); CRM's `crm.journey.entered`/`crm.communication.sent` events may be recorded as engagement touches for multi-touch models | Affiliate never triggers a CRM send, never reads a player's messages, never reads CRM's profile projection for commercial terms |
| **Segmentation** (doc 30) | Provides C-21 as a read | Affiliate does not define segments |
| **Bonus** (doc 10) | Promo code may *reference* an Offer (§5.4); bonus **cost** is an input to a revenue measure `ledger-finance` defines | Affiliate never grants, configures, or overrides a bonus; never sees Offer terms |
| **Gamification** (interface-only, doc 17) | If Gamification ever exists, its events are ordinary engagement touches for multi-touch attribution, consumed through the canonical taxonomy | No `affiliate_*` table carries a point, level, badge, streak, mission or tournament column (doc 29 §6.6's rule) |
| **Reporting** (doc 12) | Affiliate report *definitions* are Affiliate's; the pipeline is `data-analytics`'s; every monetary figure is ledger-derived with `ledger-finance` review | Affiliate builds no CDC/warehouse/BI component and publishes no monetary figure it computed itself |
| **Ledger** (ADR 0019/0032/0035) | §7's instruction contract | No posting, no import, no player account, no balance |
| **Identity** (ADR 0027) | Players are ordinary `Person`+`PlayerAccount`; affiliate users are `StaffUser`s with affiliate roles (§3.2) | No second identity model, no player creation path of its own |
| **Risk** (ADR 0031) | Affiliate-fraud *player-side* controls (self-referral, bonus abuse) are Risk rules and `bonus-engine`'s abuse detector — not affiliate-local heuristics | Affiliate has no limit, cap, counter, velocity or player-risk concept of its own (doc 30 §8.3's third-occurrence rule) |
| **RG / KYC** | Consumed only as context in reporting-safe, aggregate form | Affiliate never gates play, never sees RG/KYC detail, never influences either decision |
| **Payments** | Settlement payout rails | Affiliate does not orchestrate payouts |

### 9.1 Self-referral and affiliate fraud — named, routed, not solved here

A player who is also an affiliate self-referring for CPA is the canonical
affiliate-fraud vector, and it sits precisely at the intersection of
`risk` (player-side limits), `bonus-engine` (abuse detection, doc 10
§1.4, and its device/payment-fingerprint-linking detector) and
`identity-compliance` (person resolution, ADR 0027). **It is not an
affiliate-local heuristic**, and Affiliate must not grow one (that would
be the fourth occurrence of the "second risk engine" failure doc 02
already records twice and doc 30 §8.3 a third time).

What Affiliate contributes: the immutable evidence (§5) those domains
evaluate, plus the ability to mark an attribution `superseded` with a
reason code after their determination. Routed as **OI-AFF-4**.

---

## 10. Affiliate-facing surface and privacy

- **Subtree scoping is structural**, via §3's node-subtree RLS dimension
  plus server-side authorization — never a query filter the application
  remembers to apply (`CLAUDE.md`: isolation is enforced by RLS, not by
  discipline in application code).
- **No player PII.** An affiliate report exposes aggregates and, where a
  per-player line is commercially necessary (FTD confirmation), a
  **pseudonymous player reference** — never email, name, date of birth,
  document data, IP, payment instrument, or exact balance. This is
  doc 16's minimization posture applied to an *external* reader, which is
  the strictest case in the platform.
- **Postbacks** (the Blueprint's "FTD/deposit/NGR postbacks") are an
  outbound integration behind an adapter, with per-affiliate credentials,
  DB-enforced idempotency, retry/state machine, and the same minimization
  rule. A postback carries a pseudonymous reference and an event class —
  never PII, never a raw ledger row.
- **No affiliate-facing write path** touches any player-owned table.

---

## 11. Invariants (for `qa` and `code-reviewer`; mechanically checkable)

| ID | Invariant | Check |
|---|---|---|
| **AI-1** | **(corrected, P1-4)** `internal/affiliate` never imports `internal/wallet` at all, and never **decides** monetary treatment: no account type, transaction type, split or posting shape is chosen in affiliate code; `ledger.Post` is called only with the instruction `ledger-finance` specifies, and the settled amount is re-derived by the ledger (§7.1 rule 6) | Import inspection for `wallet`; for `ledger`, grep for account/transaction-type literals in `internal/affiliate` (there must be none) + a test that a tampered `amount` aborts settlement |
| **AI-2** | No `affiliate_*` table holds a player balance, and no settlement instruction names a player ledger account type | Schema inspection + a test asserting rejection |
| **AI-3** | Clicks, attribution candidates, `PlayerAttribution` and accruals are append-only (no `UPDATE`/`DELETE`), enforced by trigger | Trigger/constraint inspection (ADR 0013's pattern) |
| **AI-4** | **(corrected, SEC-W15-06)** Affiliate identity at the qualifying event is resolved by looking up the server-side `Click` row a **server-minted opaque lookup key** indexes; a forged, unknown, expired or wrongly-subject-bound token yields `unattributed`, never a fallback attribution | Adversarial test: forged token, replayed token, token presented by a second subject, expired token, token from a revoked key version |
| **AI-5** | Attribution is deterministic: same evidence + same model version = same decision, including tie-breaks | Property test |
| **AI-6** | **(strengthened, SEC-W15-01/07; corrected Wave 1.5 Fix Round 2, `code-reviewer`'s NEW-6 — §15)** Re-attribution (§5.3.1) and `CommissionApproval` (§6.5) both supersede-never-edit and both require **all three conjuncts** — distinct active person **AND** `AFF-4E-1` (approver `principal_class = 'internal'`, unconditional) **AND** `SEP-1` (no requester or approver in the resolved beneficiary set: the node's **reflexive ancestor closure** — the node itself plus its ancestor chain, not ancestors alone — expanded to affiliate-account persons plus declared beneficial-interest persons) — with a fail-closed default threshold of `0`/`required_approvals = 2`, `threshold_at_decision`/`amount_at_decision` captured, and the approval **consumed** not referenced | Tests: literal self-approval attempt; **two colluding affiliate principals** (must fail on `AFF-4E-1`); an internal approver who is an **ancestor** node's affiliate-account person (must fail on `SEP-1` — the case a descendant-only test misses); an internal approver who is the **same node's own** affiliate-account person or declared interest holder, with **no** ancestors involved (must fail on `SEP-1` — the case a strict-ancestor-only reading misses, NEW-6's fail-open branch); a **flat/root node with no ancestors** (its reflexive closure must still resolve non-empty and approvals must remain possible — NEW-6's deadlock branch); an internal approver with a matching declared beneficial interest (must fail); an `undeclared` or stale attestation (must fail closed); an unresolvable reflexive ancestor closure (must fail closed); an **empty** beneficiary set (must fail closed, not pass); an absent policy row (must require 2 approvals, not 0); a replayed approval (must fail at the DB) |
| **AI-7** | No commission figure is computed from an affiliate-owned counter; every revenue input traces to a ledger-derived measure | Code review + reconciliation test (accrual inputs vs. ledger) |
| **AI-8** | No floating-point money anywhere; amounts are integer minor units against the registry exponent, rounded once via the shared `rounding_rules` function | Type inspection + multi-exponent test (0/2/6/8/18) |
| **AI-9** | **(strengthened)** Every `affiliate_*` table has `tenant_id NOT NULL`, `FORCE ROW LEVEL SECURITY`, node-subtree scoping, and the `app.player_account_id IS NULL` conjunct on staff-scope policies. Subtree scope **fails closed on absence** of `app.hierarchy_node_id` (DEP-AFF-1 condition 3): unset ⇒ zero rows, never all rows | Schema inspection + cross-tenant/cross-subtree RLS tests + an explicit unset-`app.hierarchy_node_id` test asserting zero rows |
| **AI-10** | No affiliate-facing response contains player PII | Response-shape review + test |
| **AI-11** | `internal/affiliate` contains no limit/cap/counter/velocity/risk-score concept | Code review |
| **AI-12** | No affiliate node type, relation or capability appears as a Go enum, table constraint or permission string — all are configuration rows | grep (doc 26's rule) |
| **AI-13** | Affiliate builds no second hierarchy/closure table; the tree is `agentnetwork`'s | Schema inspection |
| **AI-14** | Every mutation of a link, code, agreement, rule, accrual, approval or attribution writes an `audit.Record` in the same transaction | Count-matching test |
| **AI-15** | **(new, SEC-W15-12)** No accrual is settled twice: a settled-accrual link table carries `UNIQUE (tenant_id, accrual_id)` (and the adjustment equivalent), written in the posting transaction; the four-eyes approval is **consumed** (`SELECT … FOR UPDATE` + state transition + payload containment + the `approver <> requester` conjunct inside the selection predicate), not referenced; `CommissionSettlementInstruction` is an `affiliate_*` table with no affiliate-readable policy | Concurrency test: two instructions with overlapping `accrual_refs` — exactly one must post. Replay test: a consumed approval must not be re-consumable. RLS test: an affiliate principal reads zero instruction rows |
| **AI-16** | **(new, SEC-W15-05)** `tenant_id NOT NULL` on **every** `affiliate_*` table including `Click`; an unauthenticated click whose tenant cannot be resolved from a registered link/code/host is **rejected with no row written** — never an orphan/unknown-tenant row | Schema inspection (`NOT NULL` on every table) + a test posting an unresolvable click and asserting a rejection and zero rows anywhere |
| **AI-17** | **(new, SEC-W15-05)** The click endpoint enforces per-IP, per-link and per-tenant rate ceilings with fail-closed defaults; exceeding one sheds the request and never writes an unscoped row | Load/adversarial test at each level |
| **AI-18** | **(new, SEC-W15-05)** `landing_context` cannot produce an open redirect (destinations come from a registered per-tenant allow-list, by key) and cannot produce stored XSS or report/CSV injection | Adversarial test: `javascript:`/absolute-URL/protocol-relative destinations, HTML/script payloads, CSV formula prefixes |
| **AI-19** | **(new, SEC-W15-06)** `tracking_token` is a high-entropy opaque **lookup key** carrying no claims; every attribution-relevant fact is read from the server-side `Click` row, never from the token | Code review + a test that a structurally valid but unknown token yields `unattributed` |
| **AI-20** | **(new, SEC-W15-06)** A token is subject-bound at first redemption and has a server-enforced TTL; a second subject presenting it is rejected and the attempt is recorded as a `ReferralSignal` fraud **evidence** row routed to `risk`/`bonus-engine`/`identity-compliance` — never scored or acted on inside Affiliate | Adversarial test + a grep asserting no threshold/score concept in `internal/affiliate` (AI-11) |
| **AI-21** | **(new, SEC-W15-06)** Token key material lives in Vault/KMS, is derived per tenant, carries a `key_version` on the row, and supports rotation with overlapping validity and immediate per-tenant revocation | Config/secret-scan inspection + a rotation test asserting in-flight tokens remain redeemable for their TTL and revoked-version tokens do not |
| **AI-22** | **(new, SEC-W15-13)** `AttributionCandidate` has **no** affiliate-readable policy — no subtree carve-out, no own-rows view, no affiliate-facing projection, aggregate, count or existence check | RLS test with an affiliate principal (zero rows) + a response-shape review of every affiliate-facing endpoint and export |
| **AI-23** | **(new, doc 34)** Every commission settlement, re-attribution and bulk affiliate operation carries a `parent_operation_id`; a retry, a resumed period close and a paginated settlement all resolve to the **same** `EconomicOperationIdentity`, never a fresh one | Test: interrupt and resume a period settlement; assert one operation id, one approval consumption, and no second posting |

---

## 12. Open items and routed dependencies

| ID | Item | Owner | Blocking? |
|---|---|---|---|
| **DEP-AFF-1** | **RULED (Fix Wave): conditional accept.** Storage/credential/session reuse of `identity.StaffUser` accepted; **authorization-model reuse rejected** — a distinct principal class, deny-by-enumeration for every existing and future back-office permission, and subtree scope that fails closed on absence (§3.2). Still open on `security`'s side: whether the class is a new `auth.PrincipalType` value or a column, and the exact enumerated allow-list | security | Yes, before implementation (mechanism only) |
| **DEP-AFF-2** | Tracking/cookie consent and click-PII retention (§5.6). No consent model exists (doc 31 §8.1 / DEP-CRM-1); Affiliate must not invent a second one | identity-compliance (+ human on retention) | Yes, before any click is recorded |
| **DEP-AFF-3** | Promo-code namespace collision with Bonus coupons; single authoring-time check owned by `bonus-engine` (§5.4) | bonus-engine | Yes, for codes |
| **DEP-AFF-4** | **Commission settlement posting shape, the canonical revenue (NGR/GGR) definition for commission, liability recognition for approved-unsettled accruals, and clawback treatment** — all `ledger-finance`'s, per the ADR 0035 precedent (§6.1, §6.3, §7). **Narrowed in the Fix Wave**: §7.1 rules 6–9 now supply the *triggers and structure* `ledger-finance` said were missing (LF-6, LF-9) — amount re-derivation, the recognition/discharge split with both triggers, the `reverses_instruction_id` reversal shape, and accrual-level idempotency. What remains `ledger-finance`'s is the **posting** for each | ledger-finance (+ human on commercial policy) | **Yes** — no settlement without it |
| **DEP-AFF-5** | Node-subtree RLS dimension (`app.hierarchy_node_id` + `WithNodeScope`) is already flagged P1 in doc 26 §8 for `architect` + `security`; Affiliate is its second consumer | architect + security | Yes, for subtree scoping |
| **DEP-AFF-6** | **(new, SEC-W15-12)** The four-eyes **consumption** function for commission settlement and re-attribution — a new sibling of, or a generalization of, the implemented `asset_change_consume_approved_request` (migrations `0044`/`0047`). Its properties are binding (§7.1 rule 10); its mechanism is `security` + `ledger-finance`'s | security + ledger-finance | **Yes** — without it the four-eyes control is data, not a control |
| **DEP-AFF-7** | **(new, LF-9)** The **discharge** trigger (§7.1 rule 7) is `payments`-originated, not Affiliate-originated. `payments` must expose an idempotent settlement-confirmation callback referencing the `instruction_id`. Payout rails remain out of Affiliate's scope; the trigger is not | payments (+ ledger-finance) | Yes, for discharge |
| **DEP-AFF-8** | **(new, SEC-W15-06)** Tracking-token key material in Vault/KMS with per-tenant derivation, `key_version` on the row, rotation with overlapping validity, and immediate per-tenant revocation. This is the first concrete consumer of doc 13's "Secrets/KMS" dependency row | security + devops | **Yes** — before any token is minted |
| **DEP-AFF-9** | **(new, SEC-W15-01)** The **beneficial-interest attestation** model (§6.5.2 C2) — its record shape, its re-attestation cadence, and who may attest — is `identity-compliance`'s, alongside the affiliate entity/account/beneficial-owner boundary it is producing in parallel (`4HB1FW-05`, not seen by this document). Affiliate specifies only the fields and the fail-closed check | identity-compliance | Yes, before implementation |
| **DEP-AFF-10** | **(new, doc 34)** `EconomicOperationIdentity` (`docs/architecture/34-economic-operation-identity.md`) is referenced by §5.3.1 item 6, §7's instruction shape and AI-23. `risk` is independently reviewing the concept this round | architect (+ risk review) | Yes, for retry/resume safety |
| **OI-AFF-1** | **Buy vs. build** the affiliate platform (Blueprint §1: third-party first). This architecture is compatible with both; §7's financial contract is required either way | Orchestrator → human | No |
| **OI-AFF-2** | Build-order: `internal/agentnetwork` does not exist (§3.1). Either it is built first, or Affiliate's first slice is flat (no sub-affiliates) | Orchestrator → human | No |
| **OI-AFF-3** | Negative-commission policy: carry forward / claw back / absorb. Commercial decision with a financial consequence; deliberately not selected (§6.3) | Orchestrator → human | No |
| **OI-AFF-4** | Self-referral/affiliate fraud belongs to `risk` + `bonus-engine` + `identity-compliance`, not to an affiliate-local heuristic (§9.1) | risk + bonus-engine + identity-compliance | No |
| **OI-AFF-5** | doc 22 amendments for affiliate events — see §12.1 | Master Orchestrator (doc 22's owner) | Yes, before any producer |

### 12.1 Required doc 22 amendments — routed, not applied

Same pattern as doc 31 §14.1: recorded here, applied by doc 22's owner.

**Consumed — no change required.** `identity.person.registered`,
`payments.deposit.settled` (FTD and ongoing), `payments.withdrawal.settled`,
`casino.*`/`sportsbook.*` (revenue-relevant activity), `bonus.grant.*`
(bonus cost as a revenue-measure input), `crm.journey.entered`/
`crm.communication.sent` (engagement touches for multi-touch models).

**Produced — two genuinely new affiliate-owned types**, each with a named
consumer:

| Proposed type | Fires when | Why needed |
|---|---|---|
| `affiliate.player.attributed` | A `PlayerAttribution` becomes `active` (including a supersede, which carries `reverses_ref` to the superseded attribution — doc 22's field, used exactly as specified) | Consumed by Segmentation (C-21 invalidation), CRM (acquisition-source journeys) and reporting. Without it, every consumer must poll Affiliate's tables, which no domain may read |
| `affiliate.commission.approved` | A `CommissionApproval` completes (including four-eyes where required) | The trigger boundary between the commission domain and `ledger-finance`'s settlement path (§7). Carries the `instruction_id`, never the posting |

**Deliberately NOT proposed**: `affiliate.click.recorded` (very high
volume, pre-identity, PII-bearing, no cross-domain consumer — it belongs
in Affiliate's own store and, if ever needed, `data-analytics`'s
pipeline; doc 22 already excludes clickstream for exactly this reason);
`affiliate.commission.calculated` (an intermediate computation, not a
business fact — `approved` is the fact that matters);
`affiliate.commission.settled` (that is a **ledger** fact, and publishing
an affiliate-sourced duplicate of it would violate doc 22's
one-producer-per-fact rule).

---

## 13. What Affiliate does NOT build — restated

No wallet, no ledger, no balance, no posting, no bonus grant or coupon
value, no eligibility gate, no Risk/RG/KYC model, no player risk score,
no second identity model, no second hierarchy/closure table, no consent
store, no segmentation engine, no CRM journey engine, no BI pipeline, no
cryptographic chain ledger, no payout rail — and no implementation of
anything in this document. `internal/affiliate` does not exist and is not
authorized to exist by this document.

## 14. Cross-references

- Hierarchy primitive and its naming rationale: `26-retail-operations-architecture.md` §7.1, §8; `docs/governance/ownership.md`
- Retail financial-ownership precedent (the model for §7): `docs/decisions/0035-retail-agent-network-accounting.md`
- Retail RBAC/RLS/audit precedent: `docs/decisions/0036-…`
- Segmentation (C-21, EDR-R1): `30-segmentation-engine-architecture.md`
- CRM (steps 3–4 of §8): `31-crm-engine-architecture.md`
- Flow diagrams and build order: `33-cross-domain-commercial-flow-map.md`
- Bonus gate and coupon ownership: `10-bonus-engine-architecture.md` §T.1, W4; `29-bonus-implementation-contract.md` §5.1
- Rounding/precision: `docs/decisions/0021-multi-asset-accounting.md`
- Idempotency routing: `docs/decisions/0038-…` §14, §14.6
- Four-eyes precedent: `docs/decisions/0024-…`
- Event taxonomy: `22-canonical-activity-event-taxonomy.md`
- Reporting ownership: `12-audit-reporting-architecture.md`
- Privacy: `16-privacy.md`
- Requirements source: `01-requirements-inventory.md`; `13-dependency-map-and-risk-register.md`
- Economic operation identity (§5.3.1, §7, AI-23): `34-economic-operation-identity.md`
- The four-eyes consumption pattern adopted in §7.1 rule 10: `migrations/0044_asset_registry_failclosed_and_dual_control.up.sql`, `migrations/0047_asset_registry_dual_control_hardening.up.sql`
- The fail-closed zero-config default mirrored in §5.3.1/§6.5.3: `internal/withdrawal/policy.go` (`defaultApprovalPolicy`)

---

## 15. Fix Wave changelog — what changed in this document and why

| Change | Driver | Section |
|---|---|---|
| §7 rule 1's absolute ledger-import ban replaced with the correct qualifier: Affiliate **never decides monetary treatment itself; it supplies the instruction and calls `Post` with the split `ledger-finance` specifies**. §3's composition list corrected to include `ledger`, matching doc 26 §7.1's actual retail precedent | `code-reviewer` **P1-4** — the original misstated the ADR 0035/doc 26 precedent and made commission settlement unimplementable as written | §3, §7 rule 1, AI-1 |
| §3.2 DEP-AFF-1 **ruled**: storage reuse accepted, authorization-model reuse rejected. Three binding conditions — distinct principal class, deny-by-enumeration for every existing *and future* back-office permission, subtree scope fails closed on absence | `security` **DEP-AFF-1** conditional accept | §3.2, AI-9 |
| §6.5 rewritten: four-eyes now requires **three conjuncts** — distinct active person, `AFF-4E-1` (approver `principal_class = 'internal'`, unconditional), and `SEP-1` (no requester/approver in the resolved beneficiary set) | `security` **SEC-W15-01 (P0)** — the original was satisfiable by two colluding affiliate accounts | §6.5 |
| §3.2 aligned to `security`'s **published** §W15.2.2: **four** conditions AFF-C1–C4, a `staff_users.principal_class` column (a new `auth.PrincipalType` explicitly rejected), and a **disjoint** affiliate permission set (empty intersection) rather than this document's first-draft allow-list | `security` §W15.2.2, published after this document's first Fix-Wave draft | §3.2 |
| **A real error in this document's own first Fix-Wave draft corrected**: conjunct C was written as "not within the benefiting **subtree**" (descendants). `security` §W15.1.2's resolver runs **up** the tree — a parent earning a sub-affiliate override benefits from a child's accrual, so the ancestor chain is the beneficiary set. A descendant-only test checks the one set of nodes that does not automatically benefit | `security` **SEP-1** / §W15.1.2 | §6.5.2, §5.3.1, AI-6 |
| The beneficial-ownership attestation's physical home **decided** (the node, not the agreement), with the reasoning — `security` left this choice to `architect` | `security` §W15.2.5 | §6.5.2.1 |
| Fail-closed default threshold (`0`, `required_approvals = 2`) plus `threshold_at_decision`/`amount_at_decision` capture, on **both** commission approval and re-attribution | `security` **SEC-W15-07** — the original would have shipped the control inert | §5.3.1, §6.5.3 |
| Re-attribution's full four-eyes requirements restated **at §5.3 itself**, including the closure test running against *both* the new and superseded nodes | `security` **SEC-W15-07** | new §5.3.1 |
| Accrual-level idempotency: an append-only settled-accrual link table with `UNIQUE (tenant_id, accrual_id)` (and the adjustment equivalent), written in the posting transaction, released only by an explicit reversal | `ledger-finance` **LF-6** — instruction-level idempotency did not prevent two instructions with overlapping `accrual_refs` from both posting | §7.1 rule 9, AI-15 |
| Four LF-9 gaps closed: amount **re-derived** by the ledger (rule 6); `settled` split into **recognition** and **discharge** with both triggers supplied and discharge owned by `payments` (rule 7); an explicit reversal instruction carrying `reverses_instruction_id` (rule 8); the status enum corrected | `ledger-finance` **LF-9** | §6.2, §7.1 rules 6–8 |
| The four-eyes approval is **consumed atomically** at settlement via the `asset_change_consume_approved_request` pattern (`FOR UPDATE` + payload containment + approver≠requester inside the selection predicate + state transition + `RAISE` on no match); `CommissionSettlementInstruction` given an explicit RLS home with no affiliate-readable policy | `security` **SEC-W15-12** — approval refs were data, not a control | §7.1 rule 10, AI-15 |
| Click hardened: `tenant_id NOT NULL` everywhere (there is no `unattributed` *tenant*), fail-closed rejection with **no row written** when tenant is unresolvable, three-level rate ceilings with fail-closed defaults, and `landing_context` treated as an open-redirect/XSS/injection sink with an allow-list destination model | `security` **SEC-W15-05** — the first unauthenticated public write path in the platform | new §5.1.1, AI-16–AI-18 |
| `tracking_token` redesigned: an opaque **lookup key** (not a self-contained signed assertion), **subject-bound** at first redemption, TTL-bounded, with one-token-many-subjects recorded as routed fraud **evidence**; key material in Vault/KMS with per-tenant derivation, `key_version`, rotation and revocation | `security` **SEC-W15-06** — the original was integrity-protected but hijackable by anyone who obtained the token | new §5.1.2, AI-19–AI-21, DEP-AFF-8 |
| `AttributionCandidate` determined **staff-only**: no affiliate-readable policy, no subtree carve-out, no aggregate/existence disclosure; dispute resolution is an internal process producing a decision, never raw competing evidence | `security` **SEC-W15-13** — its stated dispute purpose was incompatible with its own subtree access model | new §5.2.1, AI-22 |
| `parent_operation_id` added to the settlement instruction and to re-attribution; AI-23 added | The general retry/decomposition mechanism (doc 34) | §5.3.1, §7, AI-23 |
| §8 steps 4 and 8 corrected; DEP-AFF-6 through DEP-AFF-10 added | Consequences of the above | §8, §12 |
| **(Wave 1.5 Fix Round 2)** `B(O)`'s definition corrected from a strict **ancestor chain** to a **reflexive ancestor closure** (the node itself, plus its ancestor chain) everywhere this document states it (§6.5.2, its fail-closed table, §6.5.2.1's cross-reference, AI-6, §5.3.1's re-attribution union). Strict ancestors had a fail-open branch (a same-node declared interest holder was never in `B(O)`) and a deadlock branch (a flat/root node's ancestor chain is empty, and an empty `B(O)` is a refusal, blocking all commission approval on this platform's own recommended first-slice topology). **This specific, narrow correction was made by `code-reviewer`, not `architect`**, under this round's explicit task authorization: `architect` (this document's owner) did not touch this document in Fix Round 2 (confirmed by `git log`), and `security`'s own Fix Round 2 restatement of the same resolver (`security-architecture.md` §W15.2.7, closing the identical finding as `NEW-6`) explicitly stated it was correcting only its own restatement and left the edit to this, the owning document, to `architect`'s "separate dispatch this round" — which did not occur. No other content in this document was touched by this correction | `code-reviewer` **NEW-6** (Phase 2, Wave 1.5 Fix Wave), mirroring `security-architecture.md` §W15.2.7/§W15.2.8's identical fix to its own text | §6.5.2, §6.5.2.1, AI-6, §5.3.1 |

**Not changed, and deliberately so:** AFF-1 (read-and-record), AFF-2 (the
two-chain decoupling), AFF-3 (ledger-derived revenue measure), §4's
versioning rules, §5.4's promo-code-is-an-attribution-token decision,
§5.5's determinism/replay rules, §6.3's deliberate non-selection of the
negative-commission policy, §6.4's override bounds, §9.1's routing of
self-referral fraud away from an affiliate-local heuristic, §10's
no-player-PII rule, and §13. None of the findings touched them, and none
of the fixes weakens any of them.
