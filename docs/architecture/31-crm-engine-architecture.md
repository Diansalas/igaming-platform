# 31 — CRM Engine Architecture

Status: **DESIGN/ARCHITECTURE ONLY — `NOT IMPLEMENTED`.** No Go code, no
schema, no migration, no route, no channel provider, no message is
authorized by this document. Produced in Stage 4H-B1, Wave 1.5
(Architecture Reconciliation Gate), directive §E. Authored by `architect`
under the roster adaptation recorded in
`docs/governance/task-registry.md` (no `crm` specialist exists; every
prior brand-new cross-cutting domain — Gamification doc 17, Retail doc 26,
Asset Registry ADR 0037 — was authored by `architect` on the same
grounds), with `code-reviewer` standing in as the independent
architectural reviewer.

Numbering: next free after `30`. This document takes **31**.

## 0. Scope anchor — stated honestly before anything else

`CLAUDE.md` forbids presenting a recommendation as a Blueprint
requirement. So, precisely:

- `iGaming-Platform-Blueprint.pdf` requires CRM-adjacent *capability*
  (retention cohorts, player LTV, campaign management in back office —
  `01-requirements-inventory.md`, Blueprint §4.8/§4.9).
- `02-domain-and-service-boundaries.md` records "CRM/campaign journey
  builder (**buy first**)" under "What is explicitly NOT a separate
  service (yet)", and `14-mvp-scope-and-roadmap.md` defers "advanced
  CRM/journey builder" explicitly.
- The Wave 1.5 **human directive** elevates CRM to a first-class
  architectural domain and requires this document.

These are not in conflict, and this document does not silently overturn
doc 02. **Architecting the domain boundary is what makes "buy first"
safe.** The same reasoning `CLAUDE.md`'s provider-abstraction rule
applies to casino/payments/KYC vendors applies here: the platform owns
the boundary, the data ownership rules, the consent/suppression
enforcement and the state machine; a vendor — if one is bought — supplies
the journey-execution engine behind that boundary. Had CRM been bought
without this document, the bought product would have arrived owning
player facts, deciding who gets money, and holding a second consent
store.

**Buy-vs-build for the journey execution engine remains an OPEN
DECISION for the human** (mirroring `19-mission-architecture.md`'s open
decision 5 for segmentation). This document deliberately does not select
it; §11 shows both paths land on the same boundary.

### 0.1 Authority boundaries observed

This document does not override `ledger-finance` on any financial
invariant, `security` on any security requirement, `identity-compliance`
on identity/RG/KYC/privacy, `bonus-engine` on bonus mechanics, `risk` on
Risk, or `data-analytics` on reporting. It selects no Human Decision
Register item (G-2, `OpenBetSelfExclusionPolicy`, cashout policy, FD-1).
Amendments it needs in others' documents — doc 22's taxonomy (Master
Orchestrator), a consent model (`identity-compliance`) — are **routed**
in §14, following Wave 1's disclosed-defect-correction precedent, not
applied here.

---

## 1. The spine — the canonical boundary

Every section of this document is a consequence of one sentence, taken
verbatim from the gate directive:

> **CRM decides WHO / WHEN / WHAT CAMPAIGN.
> Bonus Engine decides the economic bonus mechanics.
> Risk / RG / KYC decide eligibility controls.
> Ledger decides financial truth.**

Read as four exclusive ownerships:

| Question | Answered by | CRM's role |
|---|---|---|
| Which players should we talk to, and when? | **CRM** | owns it |
| Through which channel, how often, with what message? | **CRM** | owns it |
| Which offer should be presented to this audience? | **CRM** selects *which* Offer (by reference) | owns the selection, never the terms |
| How much bonus, on what wagering terms, with what forfeiture rules? | **Bonus Engine** (doc 10) | consumes; never configures |
| Is this player allowed to receive it? | **AssetAuthorization → RG → Risk** (+ KYC) | consumes; never decides |
| Did value actually move, and what is the balance? | **Ledger** (ADR 0019) | reads a projection; never a truth |

### 1.1 What CRM MUST NOT own — the explicit list

| CRM must not own | Owner | Enforcement |
|---|---|---|
| Authoritative wallet balance | `internal/wallet`/`internal/ledger` (ADR 0019 — balances are projections recomputed from entries) | `internal/crm` never imports `internal/ledger`/`internal/wallet`; CI-1 (§12.1) |
| The ledger, or any second ledger | `ledger-finance` | no `crm_*` table holds a monetary balance, accrual or liability; CI-2 |
| Bonus accounting (grant amounts, wagering requirements, forfeiture, conversion) | `bonus-engine` + ADR 0032 | CRM references an `offer_id` + `offer_version_id`; it supplies no amount, multiplier or term; CI-3 |
| Risk decisions | `internal/risk` (ADR 0031) | no threshold, cap, counter or velocity concept in `internal/crm`; CI-4 |
| RG decisions | `internal/rg` (ADR 0026/0034) | never reads/writes `player_restrictions`; never caches an eligibility answer; CI-5 |
| KYC decisions | `internal/kyc` (ADR 0028) | consumes status; never a second verification model; CI-6 |
| Identity truth (person/player account, email as an identifier, account status) | `internal/identity`/`internal/identityresolution` (ADR 0027) | §4's profile is a read projection; CI-7 |
| Segmentation | `internal/segment` (doc 30) | consumes `Resolve`; builds no criteria engine; CI-8 |
| Reward fulfilment | Reward Orchestrator (doc 21) | CRM is not a reward-deciding domain and never calls the Orchestrator; §7.3 |
| Marketing consent lawfulness | `identity-compliance` (§8.1 — **does not exist yet**) | CRM fails closed on absent consent; CI-9 |
| Reporting/BI infrastructure | `data-analytics` (doc 12) | CRM performance is a read model over doc 12's pipeline; CI-10 |

This mirrors, item for item, the "Never does" columns
`02-domain-and-service-boundaries.md` already carries for Gamification
and Retail. CRM is the third domain to get one, for the same reason.

---

## 2. Domain model overview

Fourteen objects, grouped. Every one carries doc 10 **W1**'s common
object contract (server-generated UUID; `tenant_id` from authenticated
server context only; explicit status enum; `clock_timestamp()`-sourced
immutable `created_at`; `actor_type`/`actor_id` always recorded; audit
correlation; DB-enforced idempotency where externally triggerable;
append-only history).

```
  Lifecycle / profile (read projections)
    PlayerLifecycleState · CustomerProfile
  Targeting
    Audience · (segment refs → internal/segment)
  Orchestration
    EngagementCampaign · EngagementCampaignVersion · Journey ·
    JourneyVersion · JourneyInstance · Trigger · Step
  Delivery
    Communication · ChannelProvider(adapter) · CommunicationPreference ·
    Suppression · FrequencyPolicy
  Measurement
    Experiment · Variant · ConversionGoal · CampaignPerformance (read model)
```

### 2.1 A naming collision that must be settled now, not discovered later

`bonus-engine` owns `Campaign`/`CampaignVersion`/`Offer`/`OfferVersion`
(doc 10 §1.1, W2.1, W2.2) with `bonus_campaigns`/`bonus_offers` as the
tables. A CRM "campaign" is a **different object with a different
lifecycle**: it targets and communicates; it has no budget in a registered
Asset, no wagering terms, no grant.

**Binding decision:** CRM's object is an **`EngagementCampaign`**,
persisted as `crm_engagement_campaigns`, and no CRM document, type, table,
column, API path or event string uses the bare word `campaign` for it.
"Campaign" unqualified always means the Bonus Campaign. The cost of not
deciding this is two domains whose `campaign_id` columns mean different
things — the exact ambiguity that made `Cancellation` mean two things in
doc 09 vs. ADR 0038 and required a Stage 4H-B0-R4 correction.

---

## 3. Player lifecycle

`PlayerLifecycleState` is a **derived classification**, owned by CRM,
authoritative for nothing but itself.

Candidate states (configuration, not a code path — the set and its
window parameters are tenant-scoped configuration rows, per `CLAUDE.md`'s
"nothing brand-specific may become a code path"):

```
prospect → registered → verified → activated(first deposit) → active
   ↘ at_risk ↘ dormant ↘ churned → reactivated → active
   ↘ excluded (RG-driven, terminal for engagement purposes)
```

Rules that keep it honest:

- **It is derived, never asserted.** Each state's entry/exit condition is
  a declarative rule over facts owned elsewhere (Identity's `created_at`,
  ledger-derived deposit/activity recency, RG status). Recomputation from
  source must reproduce the current value — the same "projections
  recomputed from entries" discipline `CLAUDE.md` mandates for balances,
  applied to a non-financial projection (doc 22 already extends that
  principle to non-financial state).
- **`excluded` is not CRM's decision.** It is a projection of
  `internal/rg`'s authoritative status. CRM may not set, clear, or
  reason past it, and enforcement of the underlying restriction is always
  `rg.EvaluateEligibility` at the acting domain's own gate.
- **Account status is Identity's, not CRM's.** `churned` is a marketing
  classification; `suspended`/`closed` is an Identity fact. CRM never
  writes the latter and never infers one from the other.
- Transitions are append-only with `occurred_at`, the rule version that
  produced them, and the evaluating actor — so "why did this player enter
  the reactivation journey" is reconstructable (§13's evidence rule).

---

## 4. Customer profile / customer view — a read projection, and only that

The directive is explicit and this document makes it structural:
**never a second source of truth for identity or wallet facts already
owned elsewhere.**

`CustomerProfile` is a **materialized read projection** assembled from
owning domains. Five binding properties:

1. **Every field carries `source_domain` + `as_of`.** A profile field is
   never a bare value; it is a value with a provenance and an age. A
   consumer that cannot tolerate staleness must read the owning domain
   directly.
2. **It is recomputable and disposable.** Dropping and rebuilding the
   entire projection from the owning domains must lose nothing. If any
   field cannot be rebuilt, it is a CRM-*owned* fact and does not belong
   in the projection — it belongs in a CRM table of its own with its own
   audit trail (`PlayerLifecycleState`, preferences, suppression, journey
   state are exactly that; balance, KYC status, RG status, email are
   not).
3. **It is never written back.** No CRM path updates
   `player_accounts`, `persons`, wallet/ledger tables, `kyc_*`,
   `player_restrictions`, or `risk_rules`. (Doc 22's consumer contract
   item 4 already binds this for Bonus/Gamification: "Never mutate the
   producing domain's own tables." Same rule, third domain.)
4. **It is never the basis of a money or compliance decision.** A
   balance shown in a CRM screen or a message template is decorative. Any
   action that moves value re-reads the authoritative source inside the
   acting transaction (`CLAUDE.md`: "the authoritative balance read
   happens inside the same database transaction as the write"). A
   segmentation criterion likewise reads the owning domain (doc 30 §4),
   not this profile — except where the fact is CRM's own (C-03/C-05/C-06
   lifecycle), which CRM genuinely owns.
5. **It is minimized.** The projection holds no password hash, no
   KYC document or evidence, no PAN/instrument token, no raw IP history,
   no session material (doc 16; doc 22's identity-evidence rule). It
   holds identity *references* plus the small set of engagement-relevant
   attributes a message or a journey condition actually needs.

**Staleness is a first-class, configured property**, not an accident: the
projection declares a per-field freshness target, and any journey step
whose branch condition depends on a field staler than its declared
tolerance must re-read the owning domain rather than branch on the stale
value. Doc 02's existing rule — "no service caches tenant config longer
than its documented TTL" — generalized.

---

## 5. Audiences and segments

An `Audience` is **a reference to segments plus CRM's own exclusions**,
never a criteria engine:

```
Audience := include: [segment_ref(segment_id, version_pin?) …]
            exclude: [segment_ref …] ∪ suppression_lists ∪ consent_filter
            resolution_mode: live | snapshot
```

- **CRM builds no criteria.** Every include/exclude term resolves through
  `segment.Resolve` (doc 30 §5.4). If an operator wants a new targeting
  dimension, the answer is a new segment or a new registry predicate via
  doc 30 §9 — never a CRM-local rule. (CI-8.)
- **Resolution evidence is carried, not discarded.** `Resolve` returns an
  `Evaluation`, not a boolean; CRM records `segment_version_id`,
  `criteria_hash` and `evaluated_as_of` on the audience membership record
  so §13's reconstruction works end-to-end.
- **`snapshot` freezes an audience at a moment** (an `AudienceSnapshot`,
  append-only, with its own id and `resolved_as_of`) — required for
  one-shot campaigns and for reproducing "who was targeted." `live` mode
  re-resolves per evaluation and is the default for continuous journeys.
- **Exclusions always win**, evaluated after includes, and are never
  overridable by an include term (§8's ordering).
- **Fail-closed inverts for exclusion terms.** Doc 30 §5.3's terminal
  mapping is fail-closed *for inclusion*: `unknown → not_member`. For an
  **exclusion** term, `unknown` must mean **excluded**, not "not
  excluded" — otherwise a transient failure in the suppression path lets
  a message reach a player it must not reach. CRM therefore evaluates
  exclusion terms with the inverted terminal mapping and records the
  reason code. Doc 30 §5.3 explicitly hands this obligation to the
  consumer; this is CRM discharging it.

---

## 6. Journeys, triggers, and steps

### 6.1 Objects

- **`EngagementCampaign`** (+ immutable `EngagementCampaignVersion`):
  scope (tenant, optional brand), name, objective, window, status
  (`draft`/`active`/`paused`/`ended`/`archived`), the Audience reference,
  the conversion goal(s), and the frequency policy it opts into.
  Versioning follows doc 10 W2.1's rule exactly: once any Journey version
  references a campaign version, that version's content is immutable
  forever.
- **`Journey`** (+ immutable `JourneyVersion`): a **directed acyclic
  graph** of steps. Acyclic is a hard constraint validated at authoring
  time — a cyclic journey is an unbounded message generator, and the
  frequency cap (§8) would be the only thing standing between it and a
  player's inbox. Loops are expressed as bounded repeat steps with an
  explicit maximum iteration count, never as a back-edge.
- **`JourneyInstance`**: one player's traversal of one journey version.
  Pinned to the version it entered on (a live journey edit never
  retroactively rewrites a player's in-flight path — doc 10 T.11's
  reasoning applied one domain over). Carries entry reason, current step,
  wait deadlines, and an append-only step-history trail.
- **`Trigger`**: `event` (a canonical doc 22 event type), `schedule`
  (cron-like, `clock_timestamp()`-anchored), `manual` (staff, audited),
  or `api` (an authenticated server-side call). No trigger accepts a
  client-supplied player or tenant id.
- **`Step`**: `send` (a Communication), `wait` (duration or until-event),
  `branch` (a condition over profile/segment/event fields),
  `offer_request` (§7.2), `tag`/`set_attribute` (CRM-owned attributes
  only), `exit`.

### 6.2 The transport problem — a genuine build-order blocker, not a note

Doc 29 §2.1's finding stands and applies harder to CRM than it did to
Bonus: **there is no event bus.** `internal/eventbus` is a Stage 1 stub
with zero usages, a 5-field envelope against doc 22's 17, in-process,
synchronous, non-durable. Doc 22 is explicitly "design only — no event is
implemented, no producer is wired, no consumer is wired."

Bonus could route around this (doc 29 §2.2: derive triggers from the
already-durable ledger, with an in-process post-commit call as a latency
optimization) because **every Bonus trigger has a monetary fact behind
it**. CRM's triggers do not: "registered but never deposited for 72
hours", "abandoned a deposit", "viewed but did not enter a tournament",
"KYC stuck in `review_required` for 5 days" are *absences* and
*non-monetary states*. A ledger-derived stream cannot express them.

Honest consequence, recorded as the primary build-order finding
(doc 33 §4): **CRM's journey/trigger engine is the first domain in this
platform that genuinely requires either a durable broker or a
transactional outbox.** It cannot be built on top of nothing, and it must
not be built on top of best-effort in-process calls — a dropped trigger
is a silently un-sent message today and a silently un-granted bonus
tomorrow.

Two viable shapes, neither selected here (both are ordinary engineering
decisions for a future implementation stage, but the *choice* has a cost
worth surfacing):

1. **Transactional outbox** inside the existing single deployable
   (ADR 0010 untouched): producers write an outbox row in the same
   transaction as the fact; a relay reads it and drives consumers. No new
   infrastructure, durable, ordered per `operation_ref`.
2. **A broker** (doc 22's open decision 1, ADR 0003's deferred choice) —
   more capability, new operational surface, a real decision for the
   human.

CRM must be written against doc 22's envelope as a Go type either way
(the seam doc 29 §2.2 already established for Bonus), so the transport
choice stays an adapter swap.

### 6.3 Scheduled and absence-based triggers

An "absence" trigger is a **scheduled evaluation**, not an event: there
is no producer for "nothing happened." CRM evaluates such conditions on a
schedule with `clock_timestamp()`, records the evaluation, and is
idempotent per `(journey_version, player, window)` so a re-run or a
restart cannot double-enter a player. This is the same discipline the
cashback settlement job uses (doc 10 §2) and the same stalled-run
detection shape Stage 4H-B0-R6 built for self-exclusion enumeration.

---

## 7. Integration with the reward-producing domains

### 7.1 The rule

**CRM never creates player value.** It is not a reward-deciding domain in
doc 21's sense, it emits no `RewardDecision`, and it never calls the
Reward Orchestrator. It *requests that a deciding domain consider a
decision*, and that domain decides.

### 7.2 CRM → Bonus: the `OfferPresentationRequest`

The only interface by which a CRM journey can cause a bonus to exist:

```
bonus.RequestOfferGrant(ctx, tx, OfferGrantRequest) (OfferGrantOutcome, error)

OfferGrantRequest:
  idempotency_key      (deterministic: journey_instance_id + step_id + occurrence)
  tenant_id, brand_id, player_account_id   (server-resolved, never client-supplied)
  offer_id, offer_version_id               (a reference — CRM supplies no terms)
  trigger_type = crm_journey
  trigger_reference = journey_instance_id + step_id
  reason_code
  audience_evidence    (segment_id/version/criteria_hash/evaluated_as_of …)
  experiment_variant_id (optional, §10)
OfferGrantOutcome:
  granted | denied(reason_code) | already_granted(grant_id)
```

Binding properties:

- **CRM supplies no amount, no percentage, no wagering multiplier, no
  max-cashout, no expiry, no forfeiture rule.** All of those live on the
  `OfferVersion` (doc 10 W2.2) and are `bonus-engine`'s. If a campaign
  needs different economics, an operator authors a different Offer in the
  Bonus admin surface — not a parameter on the CRM step. (CI-3.)
- **Bonus runs its full, unmodified gate.** `AssetAuthorization` → RG →
  Risk, in that fixed order, in the same transaction as the effect, all
  fail-closed (doc 10 §T.1; doc 29 §8 BI-7). A CRM-originated grant has
  **no** shortcut — exactly as doc 10 W6 requires for an activated
  `BonusSuggestion` ("Activation grants no shortcut through eligibility/
  RG/Risk/AssetAuthorization").
- **A denial is a normal outcome, not an error to retry around.** CRM
  records it, may branch on it, and must never re-request with altered
  parameters to obtain a different answer. A retry with the same
  `idempotency_key` returns the same outcome by database constraint,
  never a second Grant.
- **The audience evidence rides along** so the Grant's eligibility record
  (doc 30 §7) can name the segment version and journey version that
  produced it.

### 7.3 CRM → Gamification, CRM → Reward Orchestrator (interface-only)

Neither domain exists. What this document fixes is the *shape*, so that
when they do, nothing is renamed:

- **Gamification**: CRM may (a) consume `gamification.*` canonical events
  as triggers, and (b) request enrolment in a programme/mission/tournament
  through a Gamification-owned interface symmetrical to §7.2 (a
  reference + player + idempotency key; Gamification decides). CRM never
  awards points, XP, levels, badges or tournament entries, and no
  `crm_*` table carries a points, level, badge, streak, mission or
  tournament column (doc 29 §6.6's rule, third domain).
- **Reward Orchestrator**: CRM has **no** interface to it, deliberately.
  The Orchestrator fulfils decisions from deciding domains; inserting CRM
  would make CRM a value-creating domain by the back door. If a future
  CRM-native non-monetary reward genuinely needs fulfilment, it goes
  through a deciding domain first. Recorded as a deliberate omission so
  that a future implementer does not read it as an oversight.

### 7.4 CRM ↔ Affiliate

See `32-affiliate-and-acquisition-architecture.md` §8. In summary: CRM
consumes acquisition attribution as an audience dimension (doc 30 C-21)
and may emit conversion events the affiliate commission model consumes;
CRM never computes commission and never references an affiliate's
commercial terms.

---

## 8. Consent, preferences, suppression, and frequency

### 8.1 Consent — the finding, and who must own it

**Verified, not assumed: this platform has no consent model.** A
repository-wide search of `docs/` for "consent" returns zero occurrences
outside this gate's own documents; ADR 0034, doc 11 (KYC/AML/RG) and doc
05 (identity) contain none; `16-privacy.md` inventories PII and access
controls but defines no lawful-basis, consent, or marketing-permission
concept.

The directive says: cross-reference identity-compliance's existing
consent/privacy architecture if any exists — don't invent a parallel
consent model. **None exists.** The honest answer is therefore not for
CRM to invent one:

> **Binding: marketing/communication consent is `identity-compliance`'s
> to design and own**, alongside `16-privacy.md`, because it is a
> lawful-basis question about personal-data processing, varies by
> jurisdiction (`CLAUDE.md`: jurisdiction is first-class and pluggable),
> attaches to the Person/PlayerAccount rather than to any campaign, and
> must outlive any CRM product that is bought or replaced. **CRM must not
> ship a consent store.**

Routed as dependency **DEP-CRM-1** (§14). Until it exists, CRM's consent
gate reads an interface that has no implementation, and **fails closed**:
no consent record ⇒ no communication. Not "no restriction configured,
therefore send" — the identical fail-closed default as ADR 0037 §C.1 and
doc 30 §5.3.

What CRM legitimately owns, and what it does not:

| Concept | Owner | Why |
|---|---|---|
| Lawful basis / marketing consent, its capture evidence, withdrawal, jurisdictional variation, retention | **identity-compliance** (DEP-CRM-1) | personal-data lawfulness; survives CRM |
| Channel *preference* ("I prefer email over SMS", quiet hours, language) | **CRM** | an engagement preference, not a lawful basis |
| Operational suppression (bounce/complaint lists, do-not-contact, campaign-level exclusions, cooldowns) | **CRM** | operational deliverability state |
| RG-driven marketing suppression | **`internal/rg` decides; CRM enforces** | §8.3 |

A preference can never be *more* permissive than consent. Preference and
suppression narrow; only consent permits.

### 8.2 The send gate — fixed order, fail-closed at every step

No communication leaves the platform unless **every** check passes, in
this order, evaluated as close to send time as possible and inside the
sending transaction:

```
1. Consent            (identity-compliance; absent ⇒ DENY)          ← DEP-CRM-1
2. Channel preference (CRM; absent ⇒ channel-default, configured)
3. Suppression        (CRM; unknown ⇒ suppress, §5's inverted mapping)
4. RG status          (rg.EvaluateEligibility; DENY ⇒ suppress)     ← §8.3
5. Frequency/cooldown (CRM; §8.4; over cap ⇒ suppress or defer)
6. Jurisdiction rules (jurisdiction config; unknown ⇒ DENY)
7. Channel provider   (adapter; §9)
```

Every suppression is **recorded with its reason code** — a silently
dropped message is indistinguishable from a bug, and campaign performance
(§11) is meaningless without the denominator.

### 8.3 RG and marketing — enforcement is our code

`CLAUDE.md`: "Enforcement (blocking play/withdrawal) is our code, not the
vendor's." Applied to CRM: a bought CRM product's own suppression list is
never the control. The RG check at step 4 is a literal call to
`rg.EvaluateEligibility` (ADR 0034 §2.1's absolute rule — "every
forward-going action re-evaluates fresh, every time"; and doc 21's
corrected *unconditional* re-check, which removed a "only if the delay is
non-trivial" carve-out for exactly this failure shape). No cached
answer, no event-derived status, no vendor-side list.

Doc 22's `rg.status.changed` is consumed for **triggering a
re-evaluation**, never as the authoritative status (doc 22's consumer
contract item 3; doc 29 §2.3). And per doc 30 §8.4, an RG state may
**suppress** an audience but may never **constitute** one.

### 8.4 Frequency, cooldown, and fatigue — CRM's own, and not a Risk rule

CRM owns communication frequency caps (per channel, per campaign, per
player, per window) and post-send cooldowns. This is deliberately *not*
routed to `internal/risk`, and the distinction matters: ADR 0031 §15h
already places non-monetary, non-convertible concerns outside Risk's
scope, and doc 02 records the standing rule that Risk governs limits on
*value-affecting operations*. A message-frequency cap gates no value
movement and denominates in no Asset.

The line, stated so it cannot drift: **anything that caps how often we
*talk* to a player is CRM's; anything that caps how much *value* moves is
Risk's.** A campaign's monetary budget cap is therefore **not** CRM's —
it is doc 10 §1.1's `bonus_campaigns.budget_cap`, which doc 29 §4.1 item
(e) records as still having **no mechanism and no owner** (BC-22). CRM
must not close that gap by capping spend itself; doing so would create
the second, unreconciled budget authority BC-22 exists to avoid.
Recorded as **OI-CRM-3**.

---

## 9. Channel abstraction

`CommunicationChannelProvider` — an internal interface, per `CLAUDE.md`'s
provider-abstraction rule and ADR 0004's pattern:

```
Send(ctx, SendRequest) (SendReceipt, error)
Capabilities() ChannelCapabilities
```

Channels: `email`, `sms`, `push`, `in_app`, `webhook`. Each concrete
adapter is **a subsystem, not a connector** (`CLAUDE.md`): the platform
owns the adapter, idempotency and retry semantics, per-tenant
credentials, the delivery state machine (`queued`/`sent`/`delivered`/
`bounced`/`failed`/`suppressed`), and reconciliation of provider
callbacks against `crm_communications`.

Binding:

- **Per-tenant credentials, never hardcoded, never in code or config
  committed to the repo** (`CLAUDE.md` security).
- **Idempotency is DB-enforced** on `(tenant_id, communication_id)` —
  never check-then-insert (`CLAUDE.md`; doc 10 §9's pattern).
- **Provider specifics never leak.** No provider id, template id or
  vendor field appears in a journey, audience, or segment definition —
  verifiable by import inspection, the same technique doc 02 uses for the
  risk/rg separation.
- **Minimum payload.** An adapter receives only what the message needs: a
  contact endpoint, a rendered body, and a correlation id. Never a
  password hash, KYC document/evidence, PAN or instrument token, session
  material, or a full profile dump (doc 16; doc 22's identity-evidence
  rule).
- **Message content referencing money is decorative** (§4 rule 4): a
  balance or bonus figure in a template is a projection read, never a
  guarantee, and must never be the basis of a subsequent automated
  action.
- **Inbound provider callbacks are authenticated** to the same standard
  as the hardened casino callback precedent, and are idempotent.

---

## 10. Experimentation / A-B testing

- **Deterministic assignment.** Variant = a stable hash of
  `(player_account_id, experiment_key, salt_version)`. Never `rand()`,
  never time-based, never `id % n`. The assignment is recorded
  (`experiment_id`, `variant_id`, `assigned_at`, `salt_version`) so the
  experiment is reproducible and so §13's reconstruction can say which
  variant produced a decision.
- **Experiments vary presentation, timing, channel, and which Offer is
  referenced. They never vary a safety control.** No experiment may
  change an RG, Risk, KYC, AssetAuthorization, jurisdiction, consent,
  suppression or frequency outcome — not even "as a holdout." A variant
  that would do so is rejected at authoring time. (CI-11.)
- **Holdout groups are an audience exclusion**, expressed as a
  deterministic assignment, not as a silent skip.
- **A variant that references a different Bonus Offer is still bound by
  §7.2**: Bonus decides, per variant, with its own full gate.

---

## 11. Measurement, conversion, and reporting

- **`ConversionGoal`** is declarative: a canonical event type plus a
  window plus optional qualifying conditions (e.g. `payments.deposit.settled`
  within 72h of `crm.message.sent`). Attribution of a conversion to a
  campaign/journey/variant is computed from recorded evidence, is
  **versioned** (the attribution rule version is recorded on the
  conversion record), and is never retro-edited — a corrected attribution
  is a new record superseding the old, with reason and actor (the
  compensating-entry discipline `CLAUDE.md` mandates for the ledger,
  applied to a non-financial record).
- **Any figure presented as money is ledger-derived.** Campaign revenue,
  bonus cost, NGR contribution and player value are read from the ledger
  or from `data-analytics`'s ledger-derived models — never from a
  CRM-maintained counter. Doc 29 §5.2's binding rule for campaign
  reporting ("every figure is derived from `ledger_entries`, never from a
  Bonus-owned balance column") applies unchanged to CRM. (CI-2/CI-10.)
- **`data-analytics` owns reporting/BI** (doc 02's services table;
  doc 12's "Reporting and BI" and its ownership section: "Reporting/BI:
  `data-analytics`, with `ledger-finance` review on anything presented as
  a financial figure"). CRM builds **no** BI pipeline, no CDC, no
  ClickHouse schema, no warehouse. `CampaignPerformance` is a read model
  *defined* by CRM (what a campaign's funnel means) and *served* by
  doc 12's pipeline. Extending that pipeline with CRM dimensions is a
  `data-analytics`-owned extension, filed as **DEP-CRM-2**, exactly as
  doc 26 filed the retail node-subtree reporting dimension.

---

## 12. Multi-tenancy, security, audit, package placement

### 12.1 Invariants (for `qa` and `code-reviewer`; mechanically checkable)

| ID | Invariant | Check |
|---|---|---|
| **CI-1** | `internal/crm` never imports `internal/ledger` or `internal/wallet` | Import inspection |
| **CI-2** | No `crm_*` table holds a balance, accrual, liability or monetary counter; every money figure CRM displays is derived at read time | Schema inspection + grep |
| **CI-3** | No CRM object carries a bonus amount, percentage, wagering multiplier, max-cashout, expiry or forfeiture rule; only `offer_id` + `offer_version_id` | Schema inspection |
| **CI-4** | `internal/crm` contains no limit, threshold, cap, counter or velocity concept over *value* (frequency caps over *messages* are permitted, §8.4) | Code review |
| **CI-5** | `internal/crm` never reads or writes `player_restrictions`, and never caches an RG answer; the §8.2 step-4 check is a literal `rg.EvaluateEligibility` call inside the sending transaction | Code review + a test that self-excludes mid-journey and asserts suppression |
| **CI-6** | CRM defines no verification status/tier of its own; KYC facts are read from `internal/kyc` with an `as_of` | Schema + code review |
| **CI-7** | No CRM path writes `player_accounts`, `persons`, `kyc_*`, `risk_rules`, ledger or wallet tables | Grep + integration test |
| **CI-8** | `internal/crm` contains no criteria/predicate evaluator; every audience term resolves via `segment.Resolve` | Import + code review |
| **CI-9** | No communication is sent without a positive consent result; absent consent denies | Fail-closed integration test (mirrors `internal/risk/fail_closed_integration_test.go`) |
| **CI-10** | CRM builds no CDC/warehouse/BI component | Repo inspection |
| **CI-11** | No experiment variant alters an RG/Risk/KYC/AssetAuthorization/jurisdiction/consent/suppression/frequency outcome | Authoring-time validator + test |
| **CI-12** | Every `crm_*` table has `FORCE ROW LEVEL SECURITY`; `tenant_staff_scope` carries the `app.player_account_id IS NULL` conjunct; any player-readable table (in-app inbox, preference centre) has a **SELECT-only** `player_self_scope` policy | Schema inspection + cross-tenant/cross-player RLS tests (the Stage 4H-B0-R6 F2 gap) |
| **CI-13** | Journeys are acyclic; repeat steps carry a bounded maximum | Authoring-time validator + test |
| **CI-14** | Every send, suppression, journey entry/exit and staff mutation writes an `audit.Record` in the same transaction | Code review + count-matching test |
| **CI-15** | No brand-name, tenant-name or jurisdiction-code literal in a CRM code path; lifecycle states, windows and frequency policies are configuration rows | grep (`CLAUDE.md` multi-tenancy) |

### 12.2 Security specifics (routed to `security` for review, §14)

- **`tenant_id` server-side only**, from authenticated context; no CRM
  API accepts a tenant, brand, player or segment id from a client as an
  authority (`CLAUDE.md`).
- **Staff permissions**, proposed (`security` owns the final decision, as
  doc 29 §5.3 established for Bonus): `crm_config:read`/`crm_config:manage`
  (campaign/journey/audience authoring), `crm:read` (player journey
  history, support), `crm:send` (trigger an ad hoc/manual send),
  `crm:approve` (activate a campaign above a configurable audience-size
  or monetary-exposure threshold). **`crm:approve` is never bundled with
  `crm_config:manage`** — the identical separation-of-duties reasoning
  doc 25 finding F2 established for tournaments and doc 29 §5.3 applied
  to Bonus: the actor who authors a campaign that grants bonuses to
  100,000 players must not be the only pair of eyes on activating it.
  Four-eyes above the threshold, mirroring ADR 0024's withdrawal
  precedent.
- **Mass-action blast radius is a first-class control.** A CRM campaign
  is the highest-leverage object in this platform: one mis-scoped
  audience plus one `offer_request` step is a mass unauthorized grant.
  Mitigations required before any implementation: audience-size
  disclosure at activation, a dry-run/preview that resolves the audience
  without sending or granting, a configurable size threshold requiring
  four-eyes, and a kill switch that halts a running campaign. `security`
  owns the final control set; `architect` records the requirement.
- **Per-player rate limits and enumeration resistance** on any
  player-facing surface (preference centre, unsubscribe links must be
  unguessable and single-purpose tokens, never player ids).

### 12.3 Package placement

**`internal/crm` — one flat Go package, no subpackages**, per the
convention doc 29 §1.1 verified by inspection (no subpackage exists
anywhere under `internal/`; every domain is flat and concern-split), and
per the same three reasons that decided `internal/bonus`: zero precedent
for subpackages in this tree; Go would force exported-everything or an
`internal/crm/internal` shim; and the send path must hold one `pgx.Tx`
across the §8.2 gate chain, the communication write and the audit record,
which subpackages push toward passing a transaction across package
boundaries.

Proposed concern-split files: `types.go` (package doc + domain types),
`profile.go` (the §4 projection), `lifecycle.go`, `audience.go`,
`journey.go` (DAG validation + traversal), `trigger.go`, `send.go` (the
§8.2 gate chain — the analogue of `internal/casino/orchestrator.go`),
`preference.go`, `suppression.go`, `frequency.go`, `experiment.go`,
`performance.go`, `channel_*.go` (adapters, alongside the package as
`internal/casino/mock.go` sits alongside `internal/casino`).

**Explicitly rejected — a Retail-style two-package split.** Doc 26 split
`internal/agentnetwork` from `internal/retail` because the hierarchy
graph is a genuinely reusable primitive with a named second consumer
(affiliate — see doc 32 §3). CRM has no such extractable primitive: its
one reusable concept, segmentation, is **already** a separate package
(doc 30). Splitting further would be speculative.

**Buy-first compatibility**: if a CRM product is bought, it becomes an
implementation behind `internal/crm`'s journey/channel interfaces — the
platform keeps §4's projection ownership, §8's gate chain, §7.2's Bonus
boundary, and the audit trail; the vendor supplies journey execution and
channel delivery. That is the same seam shape doc 29 §3.3 argued for
segmentation, and it is the concrete reason this document exists before
any purchase.

---

## 13. Reconstruction — CRM's half of the evidence chain

Doc 30 §7's Eligibility Decision Record answers "why did this player get
this bonus." CRM must be able to answer the two questions that precede
it:

1. **Why was this player targeted?** — `EngagementCampaignVersion` +
   `JourneyVersion` + `JourneyInstance` entry reason + the audience's
   segment evidence (`segment_id`, `segment_version_id`, `criteria_hash`,
   `evaluated_as_of`) + `experiment_variant_id` + `salt_version`.
2. **Why was this message sent (or not sent)?** — the §8.2 gate chain's
   per-step outcome and reason code, the consent record reference and its
   `as_of`, the channel adapter's receipt, and the delivery state
   transitions.

Both are recorded append-only, in the same transaction as the action,
under doc 30 §7.1's **EDR-R1** rule (reference the immutable, copy the
mutable with its `as_of`). The `correlation_id` threads
journey instance → communication → offer request → Grant → ledger
transaction → audit record as one value, never re-minted (doc 22's
`correlation_id` contract).

---

## 14. Open items, routed dependencies, and required amendments

| ID | Item | Owner | Blocking? |
|---|---|---|---|
| **DEP-CRM-1** | **No consent model exists anywhere in this platform** (§8.1, verified). `identity-compliance` must design marketing/communication consent (lawful basis, capture evidence, withdrawal, jurisdictional variation, retention) alongside `16-privacy.md`. CRM fails closed until then and must not ship a parallel store | identity-compliance | **Yes** — no CRM implementation may send anything without it |
| **DEP-CRM-2** | CRM dimensions in the reporting pipeline (campaign/journey/variant/conversion) are a `data-analytics`-owned extension of doc 12, not a CRM-built BI stack | data-analytics | Yes, for §11 |
| **DEP-CRM-3** | doc 22 amendments (§14.1 below) | Master Orchestrator (doc 22's owner) | Yes — event names must be fixed before any producer is written |
| **DEP-CRM-4** | `security` owns the final CRM permission/role set, the mass-action blast-radius controls, and the preference-centre/unsubscribe token design (§12.2) | security | Yes, before implementation |
| **OI-CRM-1** | **Event transport** (§6.2): CRM's trigger engine requires a durable outbox or broker. Outbox vs. broker is an engineering choice; *that a choice is required* is a build-order fact for the human's stage sequencing (doc 33 §4) | Orchestrator → human | Yes, for journeys |
| **OI-CRM-2** | **Buy vs. build the journey execution engine** remains open (doc 02's "buy first"; doc 19's open decision 5 precedent). This document is deliberately compatible with both | Orchestrator → human | No |
| **OI-CRM-3** | Campaign **monetary** budget cap stays BC-22's unowned gap (doc 29 §4.1(e)). CRM must not close it by capping spend itself | Orchestrator | No, but it is a real hole a CRM campaign makes easier to hit |
| **OI-CRM-4** | Retention/deletion of CRM-held personal data (communications, preferences, suppression) inherits doc 16's unresolved retention-period question — a legal decision, not an engineering one | identity-compliance → human | No |
| **OI-CRM-5** | Whether a CRM-originated grant needs a distinct `trigger_type` value in Bonus's enum, or reuses an existing one, is `bonus-engine`'s call (doc 10 W2.2's `grant_policy` / §1.3 trigger list) | bonus-engine | No |

### 14.1 Required doc 22 amendments — routed, not applied

Following Wave 1's precedent exactly (`architect` records; the Master
Orchestrator, who owns doc 22, applies), and following the same
disclosed-defect-correction pattern that produced doc 22's
`bonus.grant.cancelled`/`forfeited` additions.

**Consumed — no change required.** Every trigger CRM needs is already in
doc 22's table: `identity.person.registered`, `identity.email.verified`,
`kyc.verification.updated`, `payments.deposit.settled`,
`payments.withdrawal.settled`, `casino.*`, `sportsbook.*`, `bonus.grant.*`,
`gamification.*`, `rg.status.changed`, `risk.decision.denied`.

Two notes on existing rows, offered as review input rather than as
defects:

- doc 22 open decision 2 asks whether `risk.decision.denied` is worth
  publishing "before a concrete CRM/analytics consumer exists for it."
  **A concrete consumer now exists in architecture**: a repeated-denial
  pattern is a legitimate support/education journey trigger. That does
  not by itself settle the decision (it is still the Orchestrator's), but
  the stated reason for considering removal no longer holds.
- `casino.launch.started`'s real/demo producer ambiguity (doc 22's own
  note) matters to CRM exactly as it does to Gamification: a demo launch
  must never trigger a real-money re-engagement journey. `is_real_money`
  is load-bearing here too.

**Produced — four genuinely new CRM-owned types**, each with a named
consumer, per doc 22's one-producer-per-type rule:

| Proposed type | Fires when | Why it is genuinely needed |
|---|---|---|
| `crm.journey.entered` | A `JourneyInstance` is created | Consumed by Affiliate (attribution of an engagement touch, doc 32 §8) and by reporting; without it, journey entry is only reconstructable by scanning CRM's own tables, which no other domain may read |
| `crm.journey.exited` | A `JourneyInstance` terminates (completed / exited / cancelled / superseded) | Needed for funnel measurement and to release frequency budget; carries a terminal reason code |
| `crm.communication.sent` | A communication is accepted by a channel adapter | The conversion-attribution anchor (§11) and the only durable record other domains can correlate against |
| `crm.communication.suppressed` | The §8.2 gate chain denies a send | **The most important of the four**: without it, a suppressed send is invisible outside CRM, and RG-driven suppression in particular has a compliance-evidence value that must not live only in one domain's private table. Carries the denying step and reason code, never the underlying RG/KYC detail (doc 22's identity-evidence rule) |

**Deliberately NOT proposed**, per `CLAUDE.md`'s no-scope-expansion rule
and doc 22's own restraint:

- `crm.message.delivered`/`opened`/`clicked` — channel-provider feedback,
  high volume, no cross-domain consumer today. It belongs in CRM's own
  delivery state machine and, if ever needed for analytics, in
  `data-analytics`'s pipeline. Doc 22 already excludes clickstream for
  the same reason.
- `crm.player.lifecycle_changed` — tempting and wrong: lifecycle is a
  *derived* CRM classification (§3), and publishing a derivation as a
  canonical event invites other domains to treat it as authoritative.
  Any consumer that needs it should read CRM's projection with its
  `as_of`.
- `crm.audience.resolved` — an evaluation, not a business fact.
- Any event carrying message content, contact endpoints or profile
  fields.

Also required, in doc 22's envelope discussion rather than its table: a
CRM-produced event's `source` is `crm`, and `crm.communication.*` events
carry `person_id`/`player_account_id` as a **reference only**, never an
email address, phone number or device token.

---

## 15. What CRM does NOT build — restated

No wallet, no ledger, no balance, no bonus accounting, no Risk engine, no
RG engine, no KYC model, no consent store, no segmentation engine, no
reward fulfilment, no points/levels/badges/missions/tournaments, no BI
pipeline, no broker, no second identity model, no client-trusted scope,
no offline/vendor-side enforcement of any platform rule, and no
implementation of anything in this document. `internal/crm` does not
exist and is not authorized to exist by this document.

## 16. Cross-references

- Segmentation (consumed): `30-segmentation-engine-architecture.md`
- Affiliate/acquisition (peer): `32-affiliate-and-acquisition-architecture.md`
- Flow diagrams, per-domain non-duplication table, build order: `33-cross-domain-commercial-flow-map.md`
- Bonus lifecycle, Offer/Campaign versioning, gate order: `10-bonus-engine-architecture.md` (§1, §T.1, W1, W2, W6)
- Bonus implementation contract, transport finding, seams: `29-bonus-implementation-contract.md` (§2, §3, §5, §6)
- Reward Orchestrator (deliberately not integrated, §7.3): `21-reward-orchestration-architecture.md`
- Event taxonomy: `22-canonical-activity-event-taxonomy.md`
- Reporting/BI ownership: `12-audit-reporting-architecture.md`
- Privacy/minimization/retention: `16-privacy.md`
- RG/KYC integration rules: `docs/decisions/0034-…`, ADR 0026, ADR 0028
- Risk scope boundary: `docs/decisions/0031-…` §13, §15h
- Provider-abstraction pattern: `docs/decisions/0004-…`
- Domain boundary registry: `02-domain-and-service-boundaries.md`
