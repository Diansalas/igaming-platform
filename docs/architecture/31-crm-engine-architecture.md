# 31 — CRM Engine Architecture

Status: **DESIGN/ARCHITECTURE ONLY — `NOT IMPLEMENTED`.** No Go code, no
schema, no migration, no route, no channel provider, no message is
authorized by this document. Produced in Stage 4H-B1, Wave 1.5
(Architecture Reconciliation Gate), directive §E; **revised in the
Wave 1.5 Fix Wave (task `4HB1FW-03`) to close `security`'s SEC-W15-02
(P0) and `code-reviewer`'s P1-3 plus four P2s — see §17 for the
changelog. The `bonus.RequestOfferGrant` interface this document
originally specified is WITHDRAWN (§7.2.0).** Authored by `architect`
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
    Experiment · Variant · EngagementGoal · GoalAttainment ·
    EngagementCampaignPerformance (read model)
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

**Compliance audit of this document against its own rule (Fix Wave).**
`code-reviewer` found that the Wave 1.5 original violated the rule it had
just written, in roughly fifteen places — §5's "one-shot campaigns", §6.1's
"campaign version", §8.1's "any campaign", §8.2/§8.4's "campaign
performance"/"per campaign", §11's "campaign/journey/variant", §12.2's
"activate a campaign", and OI-CRM-3's "Campaign monetary budget cap"
(which meant the *Bonus* Campaign, in a CRM document, immediately after a
sentence about CRM campaigns). A naming rule violated by its own
authoring document has no chance of surviving an implementer. Every
occurrence has been corrected: to `EngagementCampaign` where CRM's object
was meant, to "Bonus Campaign" where Bonus's was, and left as bare
"campaign" only in (a) verbatim quotations of the Blueprint and doc 02,
and (b) the statement of this rule itself. `CampaignPerformance` is
likewise renamed **`EngagementCampaignPerformance`**.

The same audit found a **second** collision, on "conversion" — settled in
§11.0.

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
- **`excluded` is not CRM's decision, and — corrected in the Fix Wave —
  it is not a cached RG answer either.** `code-reviewer` found a genuine
  contradiction between this state existing at all and CI-5's "CRM never
  caches an RG answer." Both statements were meant, and as written they
  could not both be true. The resolution:

  > `excluded` is a **stale, non-authoritative, engagement-only
  > classification with an `as_of`**, carried so that a journey does not
  > *enter* a plainly restricted player and so that reporting can explain
  > a cohort's shrinkage. It is **never** read as an RG answer, **never**
  > the basis of a suppression decision, and **never** consulted at the
  > §8.2 send gate. Step 4 of that gate is, always and only, a literal
  > live `rg.EvaluateEligibility` call inside the sending transaction.

  Concretely, the three places it may and may not appear:

  | Use | Permitted? |
  |---|---|
  | Suppress/skip a journey **entry** for a player already classified `excluded` | Yes — a cheap, early, *conservative-direction* filter. Being wrong in the stale direction only withholds engagement |
  | Explain a funnel/cohort figure in reporting, with its `as_of` shown | Yes |
  | **Decide whether a communication may be sent** | **No.** That is `rg.EvaluateEligibility`, live, every time, no exceptions, no "the projection is fresh enough" carve-out — doc 21's corrected P1 removed exactly such a carve-out |
  | **Constitute an audience** ("target the `excluded`") | **No.** Structurally forbidden — doc 30 §8.4, and see the derivation-source rule below |

  The asymmetry is what makes this safe: the projection may only ever
  cause *less* engagement than the live answer would, never more. A CRM
  path that could use it to cause *more* is a blocking defect.
- **Every lifecycle state carries a declared derivation source**
  (new, Fix Wave — doc 30's **DEP-SEG-2**). Doc 30 §8.4.3 needs to know,
  mechanically, which states are RG-derived so that criterion C-03 can
  compute `exclusion_only` rather than launder a protective signal into
  an inclusion audience (`code-reviewer` P1-2, Path 2). Therefore each
  state's configuration row carries `derivation_source`:

  | State | `derivation_source` | Doc 30 taint |
  |---|---|---|
  | `prospect`, `registered`, `verified`, `activated`, `active`, `at_risk`, `dormant`, `churned`, `reactivated` | `activity` / `identity` / `payments` — no protective signal | taint-free |
  | `excluded` | **`rg`** | **tainted — `protective_signal`** |
  | any operator-added state | **`unknown` unless explicitly declared and `identity-compliance`-reviewed** | **tainted by default** |

  Permit-by-enumeration, never permit-by-omission: an operator-extended
  state with no reviewed declaration is treated as RG-derived, which is
  the conservative direction. This is the enumeration doc 30 §8.4.3
  item 3 requires, and it lives here because doc 31 owns the state set.
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
   not). **Clarified in the Fix Wave**: `PlayerLifecycleState` is
   CRM-owned as a *classification*, and it is nonetheless fully
   recomputable from source (§3's "derived, never asserted") — including
   its `excluded` state, whose source is `internal/rg`. Being CRM-owned
   does not make it CRM-authoritative for anything but engagement
   sequencing; §3's corrected `excluded` rule binds where it may be
   read. A field that is *both* CRM-classified and externally-derived is
   the normal case here, not an exception, which is why each state
   carries an explicit `derivation_source` (§3).
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

- **CRM builds no audience criteria.** Every include/exclude term
  resolves through `segment.Resolve` (doc 30 §5.4), with CRM supplying
  the now-mandatory `use` discriminator: `inclusion` for include terms,
  `exclusion` for exclude terms. If an operator wants a new targeting
  dimension, the answer is a new segment or a new registry predicate via
  doc 30 §9 — never a CRM-local rule. (CI-8, as narrowed in the Fix
  Wave: this binds *audience* criteria; journey **branch** conditions are
  §6.4's separate, deliberately smaller mechanism.)
- **An exclusion term may not be authored over an `exclusion_only` tree
  by accident, and an inclusion term may not be authored over one at
  all.** Doc 30 §8.4.2 makes `Resolve` return `ErrSegmentNotInclusionSafe`
  for an inclusion use of a protective-signal-tainted tree; CRM's
  authoring-time validator rejects the same condition earlier, because
  CRM knows each term's use at authoring time. This is CRM's half of
  doc 30's P1-2 fix.
- **Resolution evidence is carried, not discarded.** `Resolve` returns an
  `Evaluation`, not a boolean; CRM records `segment_version_id`,
  `criteria_hash` and `evaluated_as_of` on the audience membership record
  so §13's reconstruction works end-to-end.
- **`snapshot` freezes an audience at a moment** (an `AudienceSnapshot`,
  append-only, with its own id and `resolved_as_of`) — required for
  one-shot `EngagementCampaign`s and for reproducing "who was targeted."
  `live` mode
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
  the `EngagementGoal`(s) (§11), and the frequency policy it opts into.
  Versioning follows doc 10 W2.1's rule exactly: once any Journey version
  references an `EngagementCampaignVersion`, that version's content is
  immutable forever.
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

### 6.4 Branch conditions — CRM's own bounded evaluator, NOT `segment.Resolve` (corrected, Fix Wave)

§6.1 lists `branch` as "a condition over profile/segment/event fields",
and CI-8 as originally written said "every audience term resolves via
`segment.Resolve`" — which a reader could, and `code-reviewer` did, take
to mean every branch must too. Routing branches through `segment.Resolve`
is wrong, for three concrete reasons:

1. **A branch is per-instance, per-step, in a hot loop.** A journey with
   a wait-then-branch shape evaluates branches orders of magnitude more
   often than it resolves audiences. `segment.Resolve` is deliberately a
   bounded but non-trivial multi-domain read (doc 30 §5.2 rule 7).
2. **Most branch conditions are about the journey instance, not the
   player**: "did step 4's send succeed", "has the wait deadline
   elapsed", "which experiment variant is this instance on", "what was
   the `OfferGrantOutcome` at step 6". None of those is a player fact and
   none belongs in a segment definition. Forcing them through
   segmentation would push journey-instance state into `internal/segment`
   — the exact inversion doc 30 §1.1 forbids.
3. **Creating a `SegmentVersion` per branch is worse, not better.** It
   would fill the segment registry with single-use, journey-coupled
   definitions, defeating both the versioning discipline and the reuse
   that makes segmentation a domain at all.

**The rule, binding:**

| A branch condition may read | A branch condition may NOT |
|---|---|
| `JourneyInstance`-local state: current step, step history, per-step outcome codes, wait deadlines, entry reason | Any player fact not already carried on the instance |
| The instance's `experiment_variant_id` | `member_of(...)` in any form |
| The instance's recorded entry-time audience evidence (by value, with its `as_of`) | A freshly-resolved segment membership |
| CRM-owned attributes set by a `tag`/`set_attribute` step in this same journey | An RG, KYC, Risk, balance, or jurisdiction fact |
| A canonical event reference the instance is waiting on | A monetary threshold in any form (CI-4) |

The grammar is **closed, bounded, non-composable beyond a fixed depth,
and contains no predicate registry** — it is deliberately *smaller* than
doc 30 §5.1's, not a second copy of it. It is closer to a `switch` than
to an expression language.

**The escape hatch, and the only one:** a journey that genuinely needs to
branch on a player fact declares an explicit `segment.Resolve` term as a
named step input, exactly as an audience term does — with the same
`use` discriminator, the same evidence recording, and the same
`inclusion_safety` check. That is a deliberate, visible, authored
segment reference, not an inline predicate. §4's staleness rule applies:
a branch whose declared input is staler than its tolerance re-reads
rather than branching on the stale value.

Recorded as **CI-16** (§12.1).

---

## 7. Integration with the reward-producing domains

### 7.1 The rule

**CRM never creates player value.** It is not a reward-deciding domain in
doc 21's sense, it emits no `RewardDecision`, and it never calls the
Reward Orchestrator. It *requests that a deciding domain consider a
decision*, and that domain decides.

### 7.2 CRM → Bonus — reconciled with `bonus-engine`'s own N2.4 (corrected, Fix Wave)

#### 7.2.0 What was wrong, stated before the corrected version

The Wave 1.5 original of this section specified a **new, CRM-specific
single-grant command surface**, `bonus.RequestOfferGrant`. That was
defective in two independent ways, both found in Phase 2 review:

- **`code-reviewer` P1-3 — it diverged, unreconciled, from the domain
  owner's own same-wave specification.** `bonus-engine`'s doc 10 **N2.4**
  (written in the same wave, from the same directive) specifies that CRM
  calls the **identical** surfaces a staff member's back-office UI
  already calls, and says so explicitly: *"no CRM-specific target shape
  is invented."* `bonus-engine` owns the Bonus interface. Two documents
  specifying two different CRM→Bonus surfaces is exactly the class of
  drift this gate exists to catch, and the resolution is not a
  negotiation: **`bonus-engine`'s N2.4 is adopted verbatim in mechanism**,
  and this section is rewritten to reference it rather than to restate
  it differently.
- **`security` SEC-W15-02 (P0) — it decomposed a controlled bulk
  operation into N individually-sub-threshold calls.** `security`'s own
  Wave 1 §B1.2 item 2 makes `bonus_bulk:execute` four-eyes **always**,
  regardless of per-player value, because the blast radius of a bulk
  grant is *recipients × value*, not value. A CRM journey with an
  `offer_request` step running against a 100,000-member audience would
  have issued 100,000 individual `RequestOfferGrant` calls, **none of
  which is a `BulkGrantJob`**, and therefore none of which touches the
  bulk control at all. The mass-grant control would have been present,
  correct, and completely bypassed — not by an attacker, but by the
  platform's own supported configuration.

The second defect is the more important one, because adopting N2.4
alone does **not** close it: N2.4's own §1 explicitly permits a
single-Grant staff-action-equivalent call as one of the two surfaces, and
a journey step calling *that* one N times is the same decomposition
through a different door. The volume control must therefore attach to a
different object entirely — §7.2.3.

#### 7.2.1 The two surfaces — `bonus-engine`'s, not CRM's

Per doc 10 **N2.4**, unchanged in mechanism and restated here only so
this document is readable on its own:

1. **Command surface — the identical `BulkGrantJob`-creation surface
   (doc 10 W5) or the single-Grant staff-action-equivalent surface
   (doc 10 §1.3).** CRM, having decided by its own internal logic that a
   player, an explicit list, a segment, or a **segment set** (doc 10
   N2.2) should receive an Offer, calls the same surface the back-office
   UI calls, as an **`ActorService`** caller (doc 10 N2.3 — `actor_type`
   is `internal/audit.ActorType`'s existing `service` value; there is no
   `provider` value and no new actor type), with a caller-supplied
   **Bonus** `campaign_id`/`offer_version_id` (per §2.1, "campaign"
   unqualified is always the Bonus Campaign — this field is Bonus's, not
   CRM's `EngagementCampaign`) and a `target` in any of N2.2's shapes. CRM passes a `segment_id` + `segment_version_id` reference
   **exactly as staff does**. No CRM-specific target shape exists.
2. **Query surface — `CheckOfferEligibility(offer_version_id,
   player_account_id) → (eligible bool, reason)`** (doc 10 N2.4 item 2):
   read-only, composing the same live `AssetAuthorization` →
   `rg.EvaluateEligibility` → `risk.Evaluate` chain in T.1's order,
   writing no Grant row, no Progress entry and no ledger effect, not
   idempotency-keyed, and **explicitly non-binding** — T.4's
   "repeated live" rule governs the real attempt regardless of what the
   preview returned. This is what CRM uses to size and validate an
   audience *before* committing a job.

**`bonus.RequestOfferGrant` is withdrawn.** It does not exist, is not
proposed, and no CRM object, event, permission or API path references
it. Any future reader finding the name in a downstream document (docs 32
§8 step 4 and 33 §1.3 row 7 carried it; both are corrected in this same
Fix Wave) should treat those as stale rather than as a second opinion.

#### 7.2.2 What CRM supplies, and what it still may not

Unchanged from the original section — none of these properties depended
on the defective surface:

- **CRM supplies no amount, no percentage, no wagering multiplier, no
  max-cashout, no expiry, no forfeiture rule.** All live on the
  `OfferVersion` (doc 10 W2.2) and are `bonus-engine`'s. If an
  `EngagementCampaign` needs different economics, an operator authors a
  different Offer in the Bonus admin surface — never a parameter on a CRM
  step. (CI-3.)
- **Bonus runs its full, unmodified gate, per item, every time.**
  `AssetAuthorization` → RG → Risk, fixed order, same transaction as the
  effect, all fail-closed (doc 10 §T.1; doc 29 §8 BI-7). Doc 10 W5's
  guarantee holds identically for a CRM-originated job: *"bulk assignment
  is N individual `issued` transitions sharing one job correlation id,
  never a batch-level bypass."* A CRM-originated `BulkGrantJob` cannot
  bypass anything an identical staff-originated one could not also
  bypass — i.e. nothing (doc 10 N2.6).
- **Live segment resolution, not a stale snapshot** (doc 10 W5): a
  segment-targeted job resolves its member list against the current
  `SegmentVersion` at **job-run time**. A player who has since become
  RG-excluded, Risk-denied, or left the segment is never included from a
  stale list. This interacts with CRM's own `Audience.resolution_mode`
  (§5) and the interaction is specified in §7.2.3 item 3 rather than left
  to the reader.
- **A denial is a normal outcome, not an error to retry around.** CRM
  records it, may branch on it, and must never re-request with altered
  parameters to obtain a different answer. Retries are governed by
  §7.2.4's `EconomicOperationIdentity` binding, not by CRM's own
  discretion.
- **The audience evidence rides along** — `segment_id`,
  `segment_version_id`, `criteria_hash`, `evaluated_as_of`,
  `inclusion_safety` (doc 30 §5.4) — so the Grant's Eligibility Decision
  Record (doc 30 §7) can name the segment version, `EngagementCampaign`
  version and journey version that produced it.

#### 7.2.3 DEP-CRM-4 — the volume control, and why it attaches to ACTIVATION

**This is `security`'s specification (SEC-W15-02 / DEP-CRM-4), recorded
here because it constrains CRM's object model, not because CRM owns it.
`security` owns the control set; `architect` records what CRM must carry
for the control to be enforceable.** The control's own enforcement point
is stated first because it is the whole point of the fix:

> **The volume control attaches to the object that AUTHORIZES the N
> future calls — `EngagementCampaign` activation — and it is enforced
> IN BONUS, not in CRM. CRM must never be the enforcer of its own
> budget.**

The reasoning is the same one that puts the balance read inside the
posting transaction rather than in the caller: a control a domain
enforces against itself is a control an error, a refactor, or a bought
CRM product's own journey engine can quietly remove. Bonus is the domain
that creates the value, so Bonus holds the ceiling.

**The seven required items, as specified by `security`:**

| # | Control | CRM's obligation |
|---|---|---|
| **1** | **Any `EngagementCampaign` containing an `offer_request` step is four-eyes on activation, ALWAYS — regardless of audience size.** There is no threshold, no exemption, and no "it's only 3 players" case. This mirrors `bonus_bulk:execute`'s always-four-eyes rule (`security` Wave 1 §B1.2 item 2) and exists because a size-1 audience is below every threshold, which is precisely SEC-W15-03's inversion | The presence of an `offer_request` step anywhere in **any** `JourneyVersion` reachable from the campaign version is a computed, stored, recomputed-on-activation property of `EngagementCampaignVersion`. It is not an operator-set flag |
| **2** | **Campaigns *without* an `offer_request` step are four-eyes above a configurable audience-size threshold, with a FAIL-CLOSED default**: threshold `0`, `required_approvals` `2`, mirroring `internal/withdrawal/policy.go`'s zero-config default (`defaultApprovalPolicy`, `policy.go:136`, which sets `ThresholdAmount: 0` so *every* non-zero amount needs approval until a tenant configures otherwise). A tenant that wants lighter touch configures it explicitly | An absent policy row must resolve to the fail-closed default, never to "no threshold configured, therefore no approval." This is the exact defect shape `security` SEC-W15-07 found in the affiliate re-attribution control |
| **3** | **The approved payload PINS**: `engagement_campaign_version_id`, `journey_version_id`, every referenced `offer_id` + `offer_version_id`, and an **audience-definition content hash**. A campaign using `resolution_mode = live` additionally requires a **hard per-activation recipient ceiling** and a **per-window grant ceiling**, both **enforced in Bonus** | All five are fields on the approval request. The audience hash covers the resolved include/exclude segment references *and their version pins*, not the resolved member list (which for `live` mode does not exist yet). Approving a `live` campaign without both ceilings is rejected at approval time — a `live` audience is an unbounded authorization otherwise, and §5's `resolution_mode` is what makes that reachable |
| **4** | **Audience-size disclosure is computed at approval AND re-verified at execution, aborting on deviation** | The approval record carries `audience_size_at_approval` and its `computed_as_of`. At execution, Bonus (holding the ceiling) re-verifies against the approved ceiling and **aborts the job** — not "warns", not "truncates" — on a deviation beyond the approved tolerance. The tolerance is part of the approved payload, and its absence means zero tolerance |
| **5** | **Dry-run/preview writes zero Grant, zero Communication and zero ledger effect; is separately permissioned, rate-limited and audited; and its output is AGGREGATE-ONLY by default** — never a downloadable player-id list without a separate export authority | CRM's preview composes `segment.Resolve` (`use = inclusion`) plus Bonus's `CheckOfferEligibility` (§7.2.1 item 2). A per-player breakdown requires `crm_profile:export` (item 7) and is itself audited. An unrate-limited preview over a 100,000-member audience is a player-enumeration oracle, which is why the rate limit is part of the control rather than an operational nicety |
| **6** | **A kill switch mirroring `bonus_campaign:suspend`: granted widely, never four-eyes, and it halts the `offer_request` path FIRST** | `EngagementCampaign` status gains an operator-reachable halt that suspends `offer_request` steps before it suspends sends. Stopping the money before stopping the messages is the correct order under incident conditions, and stating the order prevents an implementer choosing the other one. The kill switch is deliberately *asymmetric* with activation: hard to start, trivial to stop |
| **7** | **Permission-set additions** (below) | §12.2 |

**Permission set (item 7), binding on CRM's side:**

| Permission | Rule |
|---|---|
| `crm_offer_request:configure` | A **distinct authority**, separate from `crm_config:manage`. Authoring a journey that can cause bonuses to exist is a different act from authoring a journey that sends emails, and must be separately grantable and separately revocable |
| `crm:send` | **Must not confer `offer_request` capability.** Holding "may send a message" has never implied "may cause a grant", and the permission model must make that structural rather than conventional |
| `crm_offer_request:configure` + `bonus_offer:manage` | **No principal may hold both.** Authoring the Offer's economics *and* authoring the journey that mass-distributes it is single-actor control of the full mass-grant path. This is a deny-by-conjunction rule checked at grant-of-permission time, not only at use time |
| `crm_profile:export` | A **distinct** authority for any per-player output — preview breakdowns, audience exports, profile dumps. `crm:read` (support: one player's journey history) does not confer it |
| `crm:approve` | The four-eyes authority of items 1–2. Never bundled with `crm_config:manage` or `crm_offer_request:configure` |

**What CRM does NOT do as a result of this control**, stated because
it is the failure mode the control is designed against:

- CRM does not count grants, does not hold a budget, does not maintain a
  recipient counter, and does not decide when to stop. It carries the
  *approved ceiling* as data and passes it to Bonus; Bonus enforces.
  (CI-2/CI-4 are unchanged by this — a recipient ceiling is a count of
  *authorizations*, not of value, and it lives in Bonus regardless.)
- CRM does not gain a second approval workflow. The activation approval
  is `crm:approve`'s four-eyes, recorded once, referenced by the
  `EconomicOperationIdentity` (§7.2.4) that every downstream Bonus call
  carries.

#### 7.2.3.1 Reconciliation with `security`'s PUBLISHED §W15.3

`security` published its formal DEP-CRM-4 decision
(`docs/security/security-architecture.md` §W15.3) as `CRM-BR-1` …
`CRM-BR-6` plus three permission amendments, after the seven-item
specification above was drafted from its Phase 2 report. The two are
substantively the same control set; where the published version differs,
**`security`'s published text governs** — it owns the control set. The
mapping and the three genuine differences:

| §7.2.3 item | `security` §W15.3 | Status |
|---|---|---|
| 1 (always-four-eyes when an `offer_request` step exists) | **`CRM-BR-2`** — the volume control attaches to activation, gated on `resolved_audience_size × max_per_player_reward_value(offer_version)`, **enforced in Bonus**. Routed as **REQ-CRM-VOL-1** (this document) and **REQ-BONUS-VOL-1** (doc 10 N2.4) | Same requirement. `security` adds the **gating measure** and the explicit routing, both adopted |
| 2 (fail-closed default threshold) | Consistent with `security`'s standing fail-closed posture | Retained as specified |
| 3 (pinned payload + audience hash + ceilings) | **`CRM-BR-1`** — the audience is resolved, **materialized**, hashed and disclosed at approval, with the hash **and row count** pinned. §W15.1.2 adds: set-membership tests run against the **already-pinned, materialized** set, never a re-resolution at execution | **Differs — see the tension below**, which this document does not resolve unilaterally |
| 4 (size disclosure at approval, re-verified at execution) | `CRM-BR-1`'s pinned row count is the disclosure; the pinning is what makes the execution check meaningful | Same; §W15.1.2 makes the check a *membership* test against the pin rather than a re-count |
| 5 (dry-run writes nothing, permissioned, rate-limited, aggregate-only) | **`CRM-BR-3`** — grants and sends nothing **and is itself audited**: "a preview is a bulk read of player data and an unaudited preview is an unlogged mass export" | Same, and `security`'s framing is the sharper one. Adopted |
| 6 (kill switch, granted widely, never four-eyes) | **`CRM-BR-4`** — **single-actor, no four-eyes**, the fail-closed direction | Same |
| 7 (permission additions) | Three **amendments**, not the additions drafted above — see §12.2 | **Differs — `security`'s published set governs** |
| — | **`CRM-BR-5`** — preference-centre/unsubscribe tokens: single-purpose, ≥128 bits CSPRNG, **stored hashed**, bound to `(tenant, player, purpose)`, expiring, revoked on use, never containing or derivable from a player id, with identical response **and timing envelope** for valid/invalid/expired | **New** — closes what DEP-CRM-4 had left open. Adopted into §12.2 |
| — | **`CRM-BR-6`** — `SEP-1` adoption at the `offer_request` targeting point (**REQ-SEP-CRM-1**) | **New** — §7.2.3.2 |

**The one genuine tension, surfaced rather than silently resolved.**
`CRM-BR-1`/§W15.1.2 require the audience to be **materialized and pinned
at approval**, with execution testing membership against that pin and
**never re-resolving**. Doc 10 **W5** requires the opposite for a
segment-targeted `BulkGrantJob`: *"live segment resolution, not a stale
snapshot… so a player who has since become RG-excluded, Risk-denied, or
left the segment is never silently included from a stale list."* Both
have good reasons and they point in opposite directions.

They are reconcilable, and this is `architect`'s proposed reconciliation
— **routed, not imposed**, because it touches `security`'s control and
`bonus-engine`'s mechanism:

> The pinned, materialized set is a **ceiling**, not a worklist. It bounds
> the audience **from above**: no player outside the pin may ever be
> granted under this authorization. Live resolution then runs at execution
> and can only **shrink** that set — a player who left the segment, or who
> became RG-excluded, is dropped; a player who *joined* the segment after
> approval is **not** added, because they are outside the pin.
>
> This preserves both properties exactly: `security`'s set-swap vector is
> closed (the pin is the upper bound, so swapping the audience definition
> after approval grants nothing new), and `bonus-engine`'s staleness
> hazard is closed (the live per-item T.1 gate still runs, and still
> denies). The `resolution_mode = live` case (§5) is then not a licence to
> exceed the pin; it is a licence to fall short of it.

Routed as **DEP-CRM-7** to `security` (does the ceiling reading satisfy
`CRM-BR-1`?) and `bonus-engine` (does it satisfy W5?). Until both
confirm, **the conservative composition applies**: pin **and** re-resolve,
intersect, and grant only to the intersection — which is what the
reconciliation describes anyway, and which is strictly safer than either
rule alone.

#### 7.2.3.2 `SEP-1` at the `offer_request` targeting point (`CRM-BR-6` / REQ-SEP-CRM-1)

`security`'s `SEP-1` (§W15.1) is the platform-wide actor≠subject/
beneficiary invariant closing SEC-W15-03. CRM adopts it rather than
inventing a CRM-local rule, and supplies the two things §W15.1.2 requires
of an adopting domain:

- **A beneficiary resolver**: for an `EngagementCampaign`/journey
  activation containing an `offer_request` step, `B(O)` is **the set of
  persons behind the pinned, resolved audience**. It is **total** (returns
  a row set or raises — an empty audience is a refusal, not a pass) and
  is evaluated in the same transaction as the activation write.
- **An enforcement point**: the activation approval row — the row whose
  insertion *authorizes* the N future grants — never a row that reports
  the campaign's outcome afterwards.

`SEP-1` is **unconditional and threshold-independent**, and the reason is
exactly why item 1 above has no threshold: **a size-based four-eyes
threshold is inverted for the self-deal vector**, because a size-1
audience containing only the approver's own player account is below every
threshold a tenant would plausibly set. `SEP-1` fires at audience size 1.
It is also **orthogonal to four-eyes** — neither substitutes for the
other: two independent approvers who are not beneficiaries do not help if
the *requester* is the beneficiary, and `SEP-1` holding says nothing about
whether a second pair of eyes saw the audience size.

#### 7.2.4 Binding to `EconomicOperationIdentity`

Items 3 and 4 above only work if a retry, a resumed job, a paginated
resend, or a second journey-step firing resolves to the **same**
authorization, rather than minting a fresh one that has never been
approved. That is not a CRM-local mechanism — Bonus and Affiliate need
the identical property — and it is specified once, as a cross-cutting
concept, in `docs/architecture/34-economic-operation-identity.md`.

CRM's obligations against it, stated here as this document's half of the
contract:

- An `EngagementCampaignVersion` **activation approval mints exactly one
  `EconomicOperationIdentity`** (`operation_type = crm_engagement_campaign_activation`),
  and that identity carries the approval state, the pinned payload of
  item 3, and the ceilings of item 3.
- Every Bonus call the `EngagementCampaign` subsequently causes — every
  `BulkGrantJob`, every single-Grant equivalent, every retry, every
  resumed page — carries that identity as its **`parent_operation_id`**,
  never a fresh one. A journey step does not mint; it inherits.
- A call arriving at Bonus with **no** resolvable `parent_operation_id`,
  or with one whose approval state is not `approved`, is **rejected by
  Bonus** — fail closed. This is what makes item 1's always-four-eyes
  rule unbypassable rather than merely stated.
- CRM never mutates an `EconomicOperationIdentity` it did not mint, and
  never mints one at step-execution time.

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
and may emit `GoalAttainment`-anchoring events (`crm.journey.entered`,
`crm.communication.sent`) that a multi-touch attribution model consumes as
engagement touches; CRM never computes commission, never emits a
commission input, and never references an affiliate's commercial terms.

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
> attaches to the Person/PlayerAccount rather than to any
> `EngagementCampaign`, and
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
| Operational suppression (bounce/complaint lists, do-not-contact, `EngagementCampaign`-level exclusions, cooldowns) | **CRM** | operational deliverability state |
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
dropped message is indistinguishable from a bug, and
`EngagementCampaignPerformance` (§11) is meaningless without the
denominator.

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

CRM owns communication frequency caps (per channel, per
`EngagementCampaign`, per player, per window) and post-send cooldowns. This is deliberately *not*
routed to `internal/risk`, and the distinction matters: ADR 0031 §15h
already places non-monetary, non-convertible concerns outside Risk's
scope, and doc 02 records the standing rule that Risk governs limits on
*value-affecting operations*. A message-frequency cap gates no value
movement and denominates in no Asset.

The line, stated so it cannot drift: **anything that caps how often we
*talk* to a player is CRM's; anything that caps how much *value* moves is
Risk's.** A **Bonus** Campaign's monetary budget cap is therefore **not**
CRM's —
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

## 11. Measurement, goal attainment, and reporting

### 11.0 A second naming collision, settled here (Fix Wave)

`code-reviewer` found that this document's marketing use of
"**conversion**" collides with **two** already-established platform
meanings, both financial and both load-bearing:

| Existing meaning | Owner |
|---|---|
| `ConversionOperation` — moving value between two wallets of different assets, with an FX rate, a rate plausibility check and an 8-condition fail-closed rule | ADR 0037 / `CLAUDE.md`'s financial rules |
| Grant **conversion** — a bonus balance becoming withdrawable cash after wagering requirements are met, posting `bonus_conversion` | `bonus-engine` / ADR 0032 |

A CRM "conversion" is neither. It is a *marketing measurement* with no
posting, no asset and no wallet. Carrying the same word for all three
guarantees a future `conversion_id` column whose meaning depends on which
document the reader last opened — the identical ambiguity §2.1 settled
for "campaign" and the identical one that forced the Stage 4H-B0-R4
`Cancellation` correction.

**Binding decision, mirroring §2.1's form:** CRM's marketing concept is
an **`EngagementGoal`**, its achievement record is a **`GoalAttainment`**,
and the measured quantity is an **attainment rate**. No CRM document,
type, table, column, API path or event string uses the bare word
`conversion`, `converted` or `conversion_rate`. "Conversion" unqualified
always means a financial conversion (asset or bonus). The word appears in
this document only where it refers to one of those two, or in this
subsection.

### 11.1 The measurement objects

- **`EngagementGoal`** is declarative: a canonical event type plus a
  window plus optional qualifying conditions (e.g.
  `payments.deposit.settled` within 72h of `crm.communication.sent`).
  Attribution of a `GoalAttainment` to an `EngagementCampaign`/journey/
  variant is computed from recorded evidence, is **versioned** (the
  attribution rule version is recorded on the attainment record), and is
  never retro-edited — a corrected attribution is a new record
  superseding the old, with reason and actor (the compensating-entry
  discipline `CLAUDE.md` mandates for the ledger, applied to a
  non-financial record).
- **`GoalAttainment` is not a financial record and never becomes one.**
  It names no asset, holds no amount, and is never an input to a
  commission calculation directly — doc 32's commission accrual reads a
  ledger-derived revenue measure, never a CRM attainment count (doc 32
  §6.1, AFF-3). A CRM attainment and an affiliate-attributable revenue
  event may both be caused by the same deposit; they are not the same
  record and neither is derived from the other.
- **Any figure presented as money is ledger-derived.** Campaign revenue,
  bonus cost, NGR contribution and player value are read from the ledger
  or from `data-analytics`'s ledger-derived models — never from a
  CRM-maintained counter. Doc 29 §5.2's binding rule for Bonus Campaign
  reporting ("every figure is derived from `ledger_entries`, never from a
  Bonus-owned balance column") applies unchanged to CRM. (CI-2/CI-10.)
- **`data-analytics` owns reporting/BI** (doc 02's services table;
  doc 12's "Reporting and BI" and its ownership section: "Reporting/BI:
  `data-analytics`, with `ledger-finance` review on anything presented as
  a financial figure"). CRM builds **no** BI pipeline, no CDC, no
  ClickHouse schema, no warehouse. `CampaignPerformance` is a read model
  *defined* by CRM (what an `EngagementCampaign`'s funnel means) and
  *served* by
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
| **CI-5** | **(clarified, Fix Wave)** `internal/crm` never reads or writes `player_restrictions`, and never uses a stored RG-derived value as an *answer*; the §8.2 step-4 check is a literal `rg.EvaluateEligibility` call inside the sending transaction. The `excluded` lifecycle state (§3) is a stale engagement classification with an `as_of` that may only ever cause **less** engagement, never more, and never appears in the send gate | Code review + a test that self-excludes mid-journey and asserts suppression **even when the projection still says `active`** (the projection must not be able to authorize) + a grep asserting no send-path reference to the lifecycle state |
| **CI-6** | CRM defines no verification status/tier of its own; KYC facts are read from `internal/kyc` with an `as_of` | Schema + code review |
| **CI-7** | No CRM path writes `player_accounts`, `persons`, `kyc_*`, `risk_rules`, ledger or wallet tables | Grep + integration test |
| **CI-8** | **(narrowed, Fix Wave)** `internal/crm` contains no **audience-criteria** evaluator: every `Audience` include/exclude term resolves via `segment.Resolve`, and no CRM object holds a player-fact predicate over a domain segmentation already covers. A journey **branch condition** is explicitly *outside* this invariant — see §6.4 | Import + code review; plus a test that a CRM `Audience` cannot be authored with an inline predicate |
| **CI-16** | **(new, Fix Wave)** A journey branch condition is evaluated by CRM's own bounded, closed, non-composable evaluator (§6.4) over journey-instance-local state only; it reads no player fact, has no `member_of`, and cannot express a targeting criterion. Any branch needing a player fact uses an explicit `segment.Resolve` term instead | Authoring-time validator + a test asserting the branch grammar rejects a player-fact reference |
| **CI-17** | **(new, Fix Wave)** No grant-causing CRM path executes without a resolvable, `approved`, unexpired `EconomicOperationIdentity` minted at `EngagementCampaign` activation; a retry, a resumed journey and a paginated resend all carry the **same** `parent_operation_id`, never a fresh one; the ceiling is enforced **in Bonus**, and CRM holds no grant counter | Fail-closed test at the Bonus surface; an interrupt/resume test asserting one root operation; a grep asserting no recipient/grant counter in `internal/crm` |
| **CI-18** | **(new, Fix Wave, `CRM-BR-6`/REQ-SEP-CRM-1)** `SEP-1` is enforced at the activation-approval row for any campaign containing an `offer_request` step: neither requester nor any approver may be in the persons set behind the pinned audience. **Unconditional and threshold-independent** — it fires at audience size 1. An unresolvable or **empty** beneficiary set is a refusal, not a pass | A size-1 audience containing the approver's own linked `PlayerAccount` (must refuse, with the `SEP-1` error); an empty audience (must refuse); an unresolvable person linkage (must refuse) |
| **CI-19** | **(new, Fix Wave, `CRM-BR-5`)** Preference-centre/unsubscribe tokens are ≥128-bit CSPRNG, stored hashed, bound to `(tenant, player, purpose)`, expiring, revoked on use, never derivable from a player id; the endpoint returns an identical response **and timing envelope** for valid, invalid and expired tokens | Entropy/storage inspection + an enumeration-oracle test measuring the timing envelope across the three cases |
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
- **Staff permissions** — `security` owns the final set (as doc 29 §5.3
  established for Bonus); this is the CRM-side list, **updated in the Fix
  Wave to carry `security`'s DEP-CRM-4 item 7 additions verbatim**:

  **`security`'s published §W15.3 governs this set** (it owns DEP-CRM-4);
  the Wave 1.5 proposal is accepted **with three amendments**, recorded
  here as decided rather than proposed:

  | Permission | Scope |
  |---|---|
  | `crm_config:read` / `crm_config:manage` | `EngagementCampaign` / journey / audience authoring. Accepted as named |
  | `crm:read` | One player's journey/communication history, for support. **Amendment 3: per-field gated** — a `crm:read` holder without `verification:read` / the RG read permission sees a generalized `suppressed`, **never** a KYC- or RG-derived suppression reason. A journey history is otherwise a convenient side channel around both (the same rule §B1.3 applies to `bonus:read`) |
  | `crm:send` | **Amendment 1: splits.** `crm:send` now covers a send to an **individually named** player in a support context, only |
  | **`crm_bulk:execute`** | **(new, Amendment 1)** Any send whose recipients are **resolved** rather than hand-enumerated. **Always four-eyes, regardless of audience size** — the same reasoning as `bonus_bulk:execute`: a per-send authority applied to a resolved audience is not a control, it is an accounting error waiting for reconciliation to find it |
  | `crm:approve` | The four-eyes authority of §7.2.3 items 1–2 |
  | `crm_campaign:suspend` | The `CRM-BR-4` kill switch. **Single-actor, no four-eyes** — the fail-closed direction |

  **Role-wiring constraints (Amendment 2) — enforced in code and tested,
  not sentences in a document:**

  - No role holds both `crm_config:manage` and `crm:approve`.
  - No role holds both `crm_config:manage` and `crm_bulk:execute`.
  - **No role holds `crm:approve` together with `bonus_offer:manage` or
    `bonus_campaign:activate`.** Without this, the CRM approval and the
    Bonus approval on the same mass grant are the same human, and the
    two-domain control chain collapses to one pair of eyes.

  *Divergence disclosed*: this document's first Fix-Wave draft proposed
  `crm_offer_request:configure` and `crm_profile:export` (from
  `security`'s Phase 2 report) and a mutual exclusion between
  `crm_offer_request:configure` and `bonus_offer:manage`. `security`'s
  published set instead splits `crm:send` into `crm:send` /
  `crm_bulk:execute` and places the mutual exclusion on `crm:approve` ×
  `bonus_offer:manage`/`bonus_campaign:activate`. The published set is
  adopted. The two dropped permissions are **not** re-proposed here; if
  `security` wants a separate per-player-export authority it will say so,
  and `CRM-BR-3`'s "a preview is a bulk read of player data and an
  unaudited preview is an unlogged mass export" already carries the
  substantive control (§7.2.3 item 5's aggregate-by-default and
  rate-limit requirements stand, under `crm_bulk:execute` + audit).

  **Preference-centre and unsubscribe tokens (`CRM-BR-5`, closing what
  DEP-CRM-4 left open):** single-purpose, **≥128 bits of CSPRNG
  entropy**, **stored hashed**, bound to `(tenant, player, purpose)`,
  expiring, revoked on use for one-shot purposes, and **never containing
  or derivable from a player id**. The endpoint must not be an
  enumeration oracle: **identical response and identical timing envelope**
  for valid, invalid and expired tokens. This supersedes §12.2's earlier
  "unguessable and single-purpose tokens, never player ids", which was
  directionally right and under-specified.

  **`crm:approve` is never bundled with `crm_config:manage` or
  `crm_offer_request:configure`** — the identical separation-of-duties
  reasoning doc 25 finding F2 established for tournaments and doc 29 §5.3
  applied to Bonus: the actor who authors an `EngagementCampaign` that
  grants bonuses to 100,000 players must not be the only pair of eyes on
  activating it.

  **The `crm:approve` threshold is audience-size only — never a monetary
  threshold** (corrected, Fix Wave; `code-reviewer` CI-4/§12.2). The
  original text said "above a configurable audience-size **or monetary-
  exposure** threshold," which contradicted CI-4 (no value-denominated
  concept in `internal/crm`) and §8.4's own line ("anything that caps how
  often we *talk* is CRM's; anything that caps how much *value* moves is
  Risk's"). Audience size is sufficient, because the thing being
  authorized is *how many authorizations exist*, not how much each is
  worth. A value-denominated cap on bonus exposure belongs to Risk, added
  through ADR 0031 §12's extension process, or to the Bonus Campaign's
  own `budget_cap` (BC-22) — in neither case to CRM. §7.2.3's ceilings
  are **counts**, held as approved data and enforced in Bonus, which is
  why they are not a CI-4 violation.
- **Mass-action blast radius is a first-class control — now specified,
  not merely required.** A CRM `EngagementCampaign` is the
  highest-leverage object in this platform: one mis-scoped audience plus
  one `offer_request` step is a mass unauthorized grant. The Wave 1.5
  original listed four desired mitigations and deferred the control set
  to `security`. `security` has since supplied it (SEC-W15-02 / the
  seven-item DEP-CRM-4 specification), and it is recorded in **§7.2.3**,
  including the finding that the original design's own interface
  *decomposed* the bulk control rather than triggering it. §7.2.3 is
  binding; this bullet is a pointer to it.
- **Per-player rate limits and enumeration resistance** on any
  player-facing surface — now specified as `CRM-BR-5` above.

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
| **DEP-CRM-2** | CRM dimensions in the reporting pipeline (`EngagementCampaign`/journey/variant/`GoalAttainment`) are a `data-analytics`-owned extension of doc 12, not a CRM-built BI stack | data-analytics | Yes, for §11 |
| **DEP-CRM-3** | doc 22 amendments (§14.1 below) | Master Orchestrator (doc 22's owner) | Yes — event names must be fixed before any producer is written |
| **DEP-CRM-4** | **(now SPECIFIED, not merely routed)** `security` supplied the full seven-item mass-action control set in its Wave 1.5 Phase 2 report (SEC-W15-02, P0). It is recorded and made binding in **§7.2.3**, with the permission additions in §12.2. What remains open on `security`'s side: the preference-centre/unsubscribe token design, and final ratification of the control set into `docs/security/security-architecture.md` (`security`'s parallel Fix Wave task `4HB1FW-04`, not seen by this document) | security | Yes, before implementation |
| **DEP-CRM-5** | **(new, Fix Wave)** The `EconomicOperationIdentity` binding of §7.2.4 requires **Bonus** to reject a grant-causing call with no resolvable, approved `parent_operation_id`. That rejection is `bonus-engine`'s to implement at its own surface; `architect` specifies the identity (doc 34) and CRM's side of the contract. `bonus-engine` is independently revising doc 10 §N1–N2 in this same Fix Wave and has not seen §7.2.4 | bonus-engine (+ architect) | **Yes** — §7.2.3 items 1/3/4 are unenforceable without it |
| **DEP-CRM-6** | **(new, Fix Wave — now PUBLISHED by `security` as `SEP-1`, §W15.1)** The actor≠subject/beneficiary invariant. CRM adopts it at the activation-approval row with a beneficiary resolver over the pinned audience (§7.2.3.2, `CRM-BR-6`/REQ-SEP-CRM-1, CI-18). Remaining dependency: the Person-linkage primitive underneath (`identity-compliance`, `4HB1FW-05`, whose output this document has not seen) and `security`'s shared trigger template | identity-compliance (+ security) | Yes, before implementation |
| **DEP-CRM-7** | **(new, Fix Wave)** `security`'s `CRM-BR-1`/§W15.1.2 require the audience to be **materialized and pinned** at approval with membership tested against the pin and never re-resolved; `bonus-engine`'s doc 10 **W5** requires **live** segment resolution at job-run time so a since-excluded player is never included from a stale list. `architect`'s proposed reconciliation (§7.2.3.1): the pin is a **ceiling** that live resolution may only shrink, never widen. Needs confirmation from both owners; until then the conservative composition (pin ∩ live re-resolve) applies | security + bonus-engine | Yes, before the first CRM-originated bulk grant |
| **OI-CRM-1** | **Event transport** (§6.2): CRM's trigger engine requires a durable outbox or broker. Outbox vs. broker is an engineering choice; *that a choice is required* is a build-order fact for the human's stage sequencing (doc 33 §4) | Orchestrator → human | Yes, for journeys |
| **OI-CRM-2** | **Buy vs. build the journey execution engine** remains open (doc 02's "buy first"; doc 19's open decision 5 precedent). This document is deliberately compatible with both | Orchestrator → human | No |
| **OI-CRM-3** | The **Bonus** Campaign's **monetary** budget cap stays BC-22's unowned gap (doc 29 §4.1(e)). CRM must not close it by capping spend itself; §7.2.3's recipient/grant ceilings are counts, not value, and are enforced in Bonus | Orchestrator | No, but it is a real hole an `EngagementCampaign` makes easier to hit |
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
| `crm.communication.sent` | A communication is accepted by a channel adapter | The `GoalAttainment`-attribution anchor (§11) and the only durable record other domains can correlate against |
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
- Bonus's own CRM-boundary spec (the interface this document now adopts): `10-bonus-engine-architecture.md` **N2.2/N2.3/N2.4/N2.5/N2.6**, **W5**
- Economic operation identity (§7.2.4): `34-economic-operation-identity.md`

---

## 17. Fix Wave changelog — what changed in this document and why

| Change | Driver | Section |
|---|---|---|
| `bonus.RequestOfferGrant` **withdrawn**; CRM→Bonus adopts `bonus-engine`'s own N2.4 surfaces (`BulkGrantJob`/single-Grant staff-equivalent + read-only `CheckOfferEligibility`) | `code-reviewer` **P1-3** — unreconciled divergence from the domain owner's same-wave spec | §7.2.0, §7.2.1 |
| Volume control moved to `EngagementCampaign` **activation**, enforced **in Bonus**; full seven-item DEP-CRM-4 control set recorded as binding | `security` **SEC-W15-02 (P0)** — the original interface decomposed a bulk operation into N sub-threshold calls, escaping the always-four-eyes bulk control entirely | new §7.2.3 |
| `EconomicOperationIdentity` binding: activation mints one identity; every downstream Bonus call inherits it as `parent_operation_id`; an unresolvable/unapproved parent is rejected by Bonus | The general mechanism SEC-W15-02 needs; a retry must never mint a fresh authorization | new §7.2.4, doc 34 |
| Permission set: `crm_offer_request:configure`, `crm_profile:export`, `crm_campaign:suspend` added; `crm:send` explicitly does not confer offer_request; mutual exclusion with `bonus_offer:manage` | `security` DEP-CRM-4 item 7 | §12.2 |
| `crm:approve`'s **monetary-exposure** threshold dropped — audience size only | `code-reviewer` CI-4/§12.2 — it contradicted CI-4 and §8.4's own value/message line; value-denominated caps are Risk's under ADR 0031 §12 | §12.2 |
| CI-8 narrowed to *audience* criteria; journey branch conditions get CRM's own closed, bounded, player-fact-free evaluator, with `segment.Resolve` as the only escape hatch; CI-16 added | `code-reviewer` P2 (CI-8) | new §6.4, §12.1 |
| `excluded` lifecycle state reconciled with CI-5: it is a stale engagement classification that may only ever cause *less* engagement, never read at the send gate; every lifecycle state gains a `derivation_source`, permit-by-enumeration | `code-reviewer` P2 (CI-5/§4 rule 2) + doc 30's P1-2 fix (DEP-SEG-2) | §3, §4 rule 2, §12.1 |
| `Audience` terms now pass doc 30's mandatory `use` discriminator and are validated against `InclusionSafety` at authoring time | doc 30 P1-1/P1-2 fixes | §5 |
| "Conversion" collision settled: `EngagementGoal` / `GoalAttainment` / attainment rate; bare "conversion" reserved for financial conversion | `code-reviewer` P2 (naming) | new §11.0 |
| ~15 bare-"campaign" violations of this document's own §2.1 rule corrected; `CampaignPerformance` → `EngagementCampaignPerformance` | `code-reviewer` P2 (naming) | §2.1 audit note + throughout |
| §7.2.3 reconciled against `security`'s **published** §W15.3 (`CRM-BR-1`…`CRM-BR-6`), which landed after this section's first draft; where they differ the published text governs | `security` §W15.3 | new §7.2.3.1 |
| `SEP-1` adopted at the `offer_request` targeting point, with CRM's beneficiary resolver (persons behind the **pinned** audience) and enforcement point (the activation-approval row); unconditional and threshold-independent | `security` **SEC-W15-03** / `CRM-BR-6` / REQ-SEP-CRM-1 | new §7.2.3.2, CI-18 |
| Permission set re-aligned to `security`'s published amendments: `crm:send` splits into `crm:send` / **`crm_bulk:execute`** (always four-eyes); `crm:read` is **per-field gated**; role-wiring constraints on `crm:approve` × `bonus_offer:manage`/`bonus_campaign:activate`. The first draft's `crm_offer_request:configure`/`crm_profile:export` are **not** retained | `security` §W15.3 Amendments 1–3 | §12.2 |
| `CRM-BR-5` preference-centre/unsubscribe token specification adopted (≥128-bit CSPRNG, stored hashed, `(tenant, player, purpose)`-bound, expiring, revoked on use, identical response **and timing envelope**) | `security` `CRM-BR-5` | §12.2, CI-19 |
| CI-17, CI-18, CI-19 added | `EconomicOperationIdentity`, `SEP-1`, `CRM-BR-5` | §12.1 |
| DEP-CRM-5, DEP-CRM-6 and **DEP-CRM-7** added; DEP-CRM-7 records a genuine, unresolved tension between `CRM-BR-1`'s pinned/materialized audience and doc 10 W5's live segment resolution, with a proposed ceiling reconciliation routed to both owners | §7.2.4; `security` SEC-W15-03; `security` `CRM-BR-1` vs. `bonus-engine` W5 | §14 |

**Not changed, and deliberately so:** §1's spine, §1.1's must-not-own
list, §4's projection rules, §8.1's consent finding and DEP-CRM-1, §8.2's
send-gate order, §8.3's live-RG rule, §8.4's message/value line, §9's
channel abstraction, §10's experiment constraints, §12.3's package
placement, and §15. None of the findings touched them.
