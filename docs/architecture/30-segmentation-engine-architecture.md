# 30 — Player Segmentation Engine Architecture

Status: **DESIGN/ARCHITECTURE ONLY — `NOT IMPLEMENTED`.** No Go code, no
schema, no migration, no route is authorized by this document. Produced
in Stage 4H-B1, Wave 1.5 (Architecture Reconciliation Gate), directive
§B; **revised in the Wave 1.5 Fix Wave (task `4HB1FW-03`) to close
`code-reviewer`'s findings P1-1 and P1-2 — see §13.1 for the changelog.**
Owned by `architect` per `docs/governance/ownership.md`
("Cross-domain architecture | architect | `docs/architecture/*`") and by
the ratified `internal/segment` ownership row ("`architect` — interface/
contract/schema shape; `bonus-engine` — first consumer's call sites").

Numbering: `docs/architecture/` was populated through `29`. This document
takes the next free number, **30**.

## 0. What this document is

It elevates the placement decision already made and ratified in
`29-bonus-implementation-contract.md` §3 (and independently reached by
`bonus-engine` in `10-bonus-engine-architecture.md` **W7**) into the full
standalone domain architecture the Wave 1.5 directive requires. It is the
authoritative document for the segmentation domain; doc 29 §3 and doc 10
W7 remain valid and are **not** contradicted anywhere below — §12 records
the two places where this document goes *further* than either, and why
that is an extension rather than a change.

**Nothing here authorizes building a segmentation engine.** §11 restates,
unchanged, what is still not built and why.

### 0.1 Authority boundaries observed

- This document does not override `ledger-finance` on any financial
  invariant, `security` on any security requirement, `risk` on what a
  risk classification is, or `identity-compliance` on RG/KYC/consent.
  Where it names a fact owned elsewhere, it names the owning domain and
  its read interface, never a segmentation-local re-definition.
- It does not decide any item in `docs/decisions/0039-human-decision-
  register-stage-4h-b0-r7.md` (G-2, `OpenBetSelfExclusionPolicy`,
  cashout policy, FD-1). None is touched.
- It does not make a legal/regulatory determination. §8.4 raises one
  genuine compliance question and routes it to the human via the
  Orchestrator rather than answering it.
- Amendments it needs in documents owned by others (`bonus-engine`'s
  eligibility-snapshot schema, doc 22's taxonomy,
  `docs/governance/ownership.md`) are **recorded as routed dependencies**
  in §13, following the Wave-1 precedent, not applied unilaterally.

---

## 1. Why segmentation is a domain at all

A segment answers exactly one question: **"is this player, right now, a
member of this named, versioned audience definition?"** Nothing else.

Five already-frozen architecture documents treat that answer as an input
they consume but do not own:

| Document | Uses a segment as | Owns segmentation? |
|---|---|---|
| `10-bonus-engine-architecture.md` §2, W2.2, W7 | Offer eligibility axis (`segment_id` + `segment_version_id` pin); VIP-tier reference is *itself* a segment reference, never a Bonus-local enum | No — explicitly declines |
| `17-gamification-engine-architecture.md` | programme audience (`segment_ref`) | No |
| `18-tournament-architecture.md` | tournament audience (`segment_ref`) | No |
| `19-mission-architecture.md` §9 | mission audience; open decision 5 is "where do segments come from — buy vs. build" | No — explicitly declines |
| `20-reward-marketplace-architecture.md` | marketplace item audience (`segment_ref`) | No |

Plus the two new domains this same gate creates:
`31-crm-engine-architecture.md` (audiences, journey entry/exit
conditions) and `32-affiliate-and-acquisition-architecture.md`
(acquisition-source-derived audiences).

Seven named consumers, none of whom may own it, is what makes this a
domain boundary rather than a field on someone else's table.

### 1.1 What segmentation is NOT — the load-bearing negative

Segmentation is **commercial audience targeting**. It is not, and must
never become:

| Not this | Because that belongs to |
|---|---|
| A second eligibility engine | `AssetAuthorization` → RG → Risk, live, at every value-moving checkpoint (`10-…` §T.1, §T.11) |
| A second risk engine, risk score, or risk tier | `internal/risk` (ADR 0031) |
| A second RG/self-exclusion concept | `internal/rg` (ADR 0026, ADR 0034) |
| A second KYC state model | `internal/kyc` (ADR 0028) |
| A second jurisdiction/licensing resolver | ADR 0006, `15-jurisdiction-and-licensing-model.md`, ADR 0037 |
| A second player-value/LTV ledger | `internal/ledger` (ADR 0019); reporting aggregates are `data-analytics`'s (doc 12) |
| A CRM journey builder | `31-crm-engine-architecture.md` |
| A customer-360 profile store | `31-…` §4 (a read projection, itself not authoritative) |
| A behavioural recompute/scoring service | not authorized anywhere; §11 |

This mirrors, one domain over, the "Gamification is NOT a Risk
replacement / NOT an RG replacement" boundaries
`02-domain-and-service-boundaries.md` already records, and Retail's
identical list in the same document. The failure mode being prevented is
the one this project has already named twice: **two unreconciled policy
systems governing the same player drift silently.**

---

## 2. Placement, package, and ownership — confirmed, not re-decided

`internal/segment` — **one flat Go package**, no subpackages, per the
convention doc 29 §1.1 verified by inspection (`find internal -mindepth 2
-type d` returns nothing; every domain package is flat and concern-split).
Ratified at Stage 4H-B1 Wave 1 reconciliation and already recorded in
`docs/governance/ownership.md`.

Proposed concern-split files (a recommendation to the implementer, not a
constraint): `types.go` (package doc, `Segment`, `SegmentVersion`,
`Criteria` AST node types, `Membership`, `Evaluation`, `ReasonCode`,
sentinel errors), `resolver.go` (the `Resolve`/`IsMember` boundary and
the Kleene evaluator, §5), `predicates.go` (the closed predicate
registry, §4), `static.go` (explicit membership), `admin.go` (definition
CRUD/versioning). HTTP, if any, lives in
`internal/httpserver/segment_admin_handlers.go` per the platform-wide
handler convention.

This document changes nothing about that ratified decision and confirms
it holds against the fuller requirement list below: every criterion in §4
is expressible as a leaf predicate over a fact read from its owning
domain, and none of them requires a second package, a rule DSL runtime,
or a subpackage split.

---

## 3. Domain model

Six objects. Every one carries doc 10 **W1**'s common object contract
(server-generated UUID identity; `tenant_id` resolved from authenticated
server-side context only; explicit status enum; `clock_timestamp()`-
sourced immutable `created_at`; `actor_type`/`actor_id` never omitted;
audit correlation; DB-enforced idempotency where externally triggerable;
no mutable history).

### 3.1 `Segment` — the stable identity

`id`, dual scope (`tenant_id IS NULL` = platform-wide template, set =
tenant-owned, optionally narrowed by `brand_id` — mirroring `risk_rules`
per ADR 0031 §3, doc 19 §10, and doc 26's node-type rule), `key` (a
stable operator-facing slug, unique per scope), `name`, `description`,
`kind` (`static` | `dynamic` | `hybrid`, §6), `status`
(`draft`/`active`/`disabled`, disable-never-delete), `created_by`,
`created_at`.

A `Segment` row carries **no criteria**. It is an identity that criteria
versions hang off, exactly as `bonus_offers` carries no rule content and
`OfferVersion` does (doc 10 §1.1, W2.2).

### 3.2 `SegmentVersion` — the immutable criteria

`id`, `segment_id`, `version_no` (monotonic per segment), `criteria` (the
AST of §4/§5), `criteria_hash` (a canonical, deterministic digest of the
normalized AST — see §5.5), `effective_from`, `created_by`,
`created_at`, `status` (`draft`/`active`/`superseded`).

**Immutable once referenced by any consumer** — the identical "frozen
version, never a live reference" discipline doc 10 §1.1 applies to Offer
versions and W2.1 applies to Campaign versions. A change is always a new
version row. This is what makes §7's reconstruction requirement
satisfiable by reference rather than by copying criteria into every
consumer.

### 3.3 `SegmentMembership` — explicit (static) membership

Append-only add/remove **events**, never a mutable membership row:
`id`, `segment_id`, `player_account_id`, `operation` (`add`|`remove`),
`source` (`staff` | `import` | `system_rule` | `api`), `actor_type`/
`actor_id`, `reason_code`, `occurred_at`, `idempotency_key`. Current
static membership is the fold of these events as-of an instant — which is
what makes a historical evaluation reproducible at all. A mutable
membership table would destroy §7 for static segments.

Bulk import is an ordinary batch of these events under one
`import_batch_id`, each row individually audited; `bonus-engine`'s
bulk-assignment safety analysis (doc 10 W5, and its parallel Wave 1.5
deliverable) governs anything that then *grants* on the strength of such
a list — segmentation itself grants nothing.

### 3.4 `SegmentEvaluation` — the snapshot record

The output of one resolution, recorded by the **consumer** in the same
transaction as the decision it informed (§7). Carries `segment_id`,
`segment_version_id`, `criteria_hash`, `evaluated_as_of`, `result`
(`member`/`not_member`/`unknown`), `reason_code`, `evaluator_version`,
and the per-leaf outcome trail where the consumer's own audit obligations
require it.

**Where this row physically lives is the consumer's schema decision, not
segmentation's** — for Bonus it is part of the eligibility-decision
record specified in §7 and owned by `bonus-engine`/`ledger-finance`.
`internal/segment` does not keep a global evaluation log; a segmentation-
owned copy would be a second source of truth for a fact the consumer's
own audit trail must already carry.

### 3.5 `PredicateDefinition` — the closed registry

Not operator-editable. A compiled-in registry entry per supported leaf
predicate: `predicate_key`, the owning domain, the read interface it
calls, its parameter schema, its `as_of` semantics, its
unknown-condition, and its **protective-signal declaration** (§8.4 —
replacing the original's static `inclusion_safe` boolean, per
`code-reviewer`'s P1-2). New entries land through the documented
extension process of §9, which mirrors ADR 0031 §12's five-step model —
never an ad hoc addition and never a runtime-authored expression.

The protective-signal declaration has three parts, all compiled-in and
none operator-editable:

| Field | Meaning |
|---|---|
| `protective_signal` (bool) | The predicate reads a harm-protective fact *directly* from its owning domain (RG restriction state, self-exclusion, cooling-off, a future harm indicator) |
| `protective_signal_inputs[]` | The predicate's owning domain declares that **some** parameter values cause a protective signal to be read *indirectly* — e.g. doc 31's `PlayerLifecycleState`, whose state set includes the RG-derived `excluded`. Each entry names the parameter and the value subset that carries the taint |
| `taint_free_domain` (optional) | The explicitly enumerated parameter subset for which the predicate carries **no** protective signal — the escape hatch that lets a lifecycle predicate remain usable for `dormant`/`churned` without laundering `excluded` |

A predicate that declares neither `protective_signal` nor
`protective_signal_inputs[]` is taint-free by *declaration*, not by
omission. The §9 extension process (step 3) makes that declaration a
mandatory, `security`-reviewed field, so a new predicate that silently
forgets it fails review rather than defaulting to permissive.

### 3.6 `SegmentCriterionParameterSet` — the operator-defined half

Operators compose and parameterize registry predicates; they do not
author predicates. See §9.

---

## 4. Criteria taxonomy — every dimension, and who actually owns it

This is the directive's full list. For each: the authoritative owning
domain, the read path, and an honest status. **Segmentation computes
none of these facts. It reads them, and it reads them through the owning
domain's interface, never that domain's tables.** (`10-…` Dependency
Contract Freeze §2 already establishes this rule for Bonus: "a domain
reading those tables directly for the purpose of an eligibility decision"
is the anti-pattern — ADR 0037 §C.)

| # | Criterion | Authoritative owner | Read path | Status today |
|---|---|---|---|---|
| C-01 | **VIP tier** | Gamification (levels/tiers, doc 17) | Gamification read API | **BLOCKED** — `internal/gamification` does not exist. Until it does, VIP tier is expressible **only** as static membership (§3.3). A segmentation-computed tier is forbidden (§4.1) |
| C-02 | **Player value** (LTV, NGR contribution, deposit total) | `ledger-finance` (ledger truth) + `data-analytics` (aggregation, doc 12) | A ledger-derived read; for multi-period aggregates, the reporting read model | **PARTIAL** — single-asset ledger-derived aggregates are expressible; cross-asset value is **not** (§4.2) |
| C-03 | **Lifecycle state** (new / active / inactive / dormant / churned / reactivated) | `crm` (derived classification, doc 31 §3) — *not* Identity, which owns account status only | CRM read projection, with its own `as_of` | **BLOCKED** on CRM; a lifecycle criterion must not be reimplemented here. **Use-restricted — §8.4.3**: doc 31's state set includes the RG-derived `excluded`, so C-03 carries a declared `protective_signal_inputs[]` and computes `exclusion_only` whenever its `states` parameter is not a subset of the enumerated taint-free domain |
| C-04 | **New player** | Identity (`created_at` of `PlayerAccount`) + `payments` (first deposit) | Identity read; `payments.deposit.settled` history via ledger | Expressible |
| C-05 | **Active / inactive / dormant** | Same as C-03 (a windowed derivation over activity) | CRM projection, or a direct ledger/activity read with an explicit window parameter | Expressible as a windowed activity predicate; the *named* lifecycle labels are C-03's |
| C-06 | **Retention / reactivation** targeting | CRM (journey state) + activity windows | CRM read projection | **BLOCKED** on CRM for the labelled form |
| C-07 | **Casino activity** (frequency, recency, volume, game/category) | `casino` + ledger (`casino_bet`/`casino_win` transaction types) | Ledger-derived, per doc 29 §2.2's already-chosen "derive from ledger entries" discipline | Expressible |
| C-08 | **Sportsbook activity** | `sportsbook` | ADR 0038's transaction types | **BLOCKED** — no `internal/sportsbook` package exists |
| C-09 | **Deposit behaviour** (count, recency, size band, method, first-deposit) | `payments` + ledger | Ledger-derived + `payments` method metadata | Expressible (single-asset, §4.2) |
| C-10 | **Withdrawal behaviour** | `payments`/`internal/withdrawal` + ledger | Ledger-derived + withdrawal state machine reads | Expressible (single-asset) |
| C-11 | **Turnover** (staked volume over a window) | ledger | Ledger-derived; the *same* derived-measure discipline as `P_net`/`P_firm` (`ledger-accounting-model.md` §6.6), never a stored counter | Expressible (single-asset) |
| C-12 | **Product preference** (casino vs. sportsbook vs. live vs. retail) | derived over C-07/C-08 | Ledger-derived by `transaction_type` | Partially expressible (casino only, until sportsbook exists) |
| C-13 | **Game / sport preference** | `casino` catalogue / `sportsbook` | Casino catalogue read — **provider-neutral**: a criterion references a platform game/category id, never a provider game id (doc 02's provider-neutrality rule, verifiable by import inspection) | Expressible for casino |
| C-14 | **Jurisdiction** | ADR 0006 / doc 15 / `TenantJurisdictionConfig` | The canonical jurisdiction resolver | **BLOCKED-ish** — the standing `TODO(jurisdiction)` gap (doc 15, ADR 0031 §9: no per-player jurisdiction resolver exists) applies here exactly as it does to Risk, Casino and Bonus. Not newly introduced, not newly closed. Until closed, a jurisdiction criterion returns UNKNOWN → fail-closed (§5.3) |
| C-15 | **KYC state / tier** | `internal/kyc` (ADR 0028) | `kyc` read API: `VerificationStatus` (`unverified`/`pending`/`review_required`/`approved`/`rejected`/`expired`) + `kyc_tier` | Expressible for status; `kyc_tier` is a schema hook ADR 0028 records, not a populated model — **PARTIAL** |
| C-16 | **Asset / currency** | `internal/assetregistry` (ADR 0037) | `GetAsset` metadata read (layer 1 — explicitly *not* an eligibility decision), plus the player's wallet set | Expressible |
| C-17 | **Payment method** | `payments` (provider capability model, ADR 0022) | `payments` read — **capability/method class, never an instrument identifier**; no PAN, token, or instrument fingerprint may enter a criterion (§10.2) | Expressible at the method-class level |
| C-18 | **Bonus history** (grants received, converted, forfeited, abuse-review outcomes) | `bonus-engine` | Bonus read API over Grant/Progress | **BLOCKED** on `internal/bonus`; and note the loop (§8.5) |
| C-19 | **Risk classification** | `risk` — **and only `risk`** | `internal/risk` read | **BLOCKED — no such artifact exists.** §8.3 |
| C-20 | **Operator-defined custom criteria** | operator, within the closed registry | §9 | Expressible within §9's bounds |
| C-21 | **Acquisition source / affiliate attribution** (added — required by doc 32's chain) | `affiliate` (doc 32 §5) | Affiliate attribution projection (the immutable `PlayerAttribution` row) | **BLOCKED** on Affiliate |
| C-22 | **RG status** | `internal/rg` | `rg` read | **Expressible but use-restricted — §8.4.** Never an inclusion criterion; declares `protective_signal = true`, so any tree containing it computes `exclusion_only` unless the leaf sits under a `Not` (§8.4.1) |

### 4.1 The hard rule this table encodes

**A criterion whose authoritative owner does not yet exist is `BLOCKED`,
not "approximated in segmentation."** Approximating C-01 with a
segmentation-local spend threshold, or C-19 with a segmentation-local
"suspicious behaviour" heuristic, is precisely how a second, unreconciled
classification system gets born. The only sanctioned stand-in is §3.3's
explicit static membership — which is visibly a staff-maintained list,
carries its own actor/reason/audit trail, and cannot be mistaken for a
computed authoritative classification.

### 4.2 Monetary criteria are single-asset by construction

A monetary threshold (C-02, C-09, C-10, C-11) is denominated in exactly
one `asset_code`, compared in integer minor units against that asset's
registry exponent (`CLAUDE.md`'s financial rules; ADR 0021; ADR 0037).

**Segmentation performs no FX conversion, ever.** A cross-asset criterion
("total deposits over €1,000 equivalent") requires either (a) a
per-asset criterion composed with OR, or (b) an explicit,
`ledger-finance`-owned aggregation with a declared, versioned conversion
policy that segmentation consumes as a single already-converted fact.
Segmentation inventing (b) would create a second FX path beside ADR
0037's Conversion Service, whose 8-condition fail-closed rule and
rate-plausibility controls exist for exactly this reason. Recorded as
open item **OI-SEG-4**.

---

## 5. Composable criteria and deterministic evaluation

### 5.1 The expression shape

A `SegmentVersion.criteria` is a finite tree:

```
Criteria   := Leaf | And(Criteria…) | Or(Criteria…) | Not(Criteria)
Leaf       := PredicateKey + ParameterSet          (§3.5, §3.6)
```

`And`/`Or` are n-ary. Nesting depth and total node count are bounded by a
compiled-in maximum (rejected at authoring time, not at evaluation time).
There are no variables, no loops, no user-supplied code, no regular
expressions over free text, and no reference to another segment's
criteria body — a segment may reference another **segment** as a leaf
(`member_of(segment_id, version_pin)`), and that reference graph is
validated acyclic at authoring time.

#### 5.1.1 The `member_of` leaf — resolution semantics (corrected, Wave 1.5 Fix Wave)

**This subsection closes a real defect** `code-reviewer` found in the
Wave 1.5 original of this document (finding **P1-1**), and it is
load-bearing enough to state before the truth tables that depend on it.

The original text mapped *both* the evaluator's internal `member_of`
outcome and `Resolve`'s external terminal outcome through one rule —
"an absent/disabled/draft segment definition is `member = false`." That
rule is correct at the **boundary** and wrong **inside a tree**, because
`Not(false) = true`. §6's own canonical hybrid shape,
`And(criteria, Not(member_of(exclusion_list)))`, therefore had a
reachable accidental-ALLOW: disable the referenced exclusion segment (an
ordinary, low-privilege, non-financial operator action — and
`disable-never-delete`, §3.1, makes it the *expected* way to retire one)
and every previously-excluded player is swept back into the audience,
silently, with a `true` that looks indistinguishable from a legitimate
one. This is the identical accidental-ALLOW §5.3 exists to close, reached
through the segment-reference door instead of the predicate door.

**The binding rule, replacing the original in this respect:**

| `member_of(segment_id, version_pin?)` resolves against | Leaf result |
|---|---|
| An `active` segment with a resolvable `active` version (or a resolvable pinned version) | `true` / `false` — the referenced tree's own evaluated outcome, three-valued, propagated **unmapped** |
| A segment row that does not exist, or is not visible in the evaluating tenant/brand scope | **`unknown`** — reason `segment_reference_unresolvable` |
| A segment whose `status` is `disabled` | **`unknown`** — reason `segment_reference_disabled` |
| A segment whose only candidate version is `draft` (no `active`/`superseded` version at `as_of`) | **`unknown`** — reason `segment_reference_not_effective` |
| A `version_pin` naming a version id that does not exist, belongs to another segment, or is not effective at `as_of` | **`unknown`** — reason `segment_reference_pin_unresolvable` |
| A referenced tree that itself evaluates to `unknown` (any cause: a blocked predicate, a cost-budget exhaustion, a nested unresolvable `member_of`) | **`unknown`**, propagated, with the *innermost* reason code preserved in `leaf_outcomes[]` |
| A reference whose resolution would exceed the evaluation's cost budget (§5.2 rule 7) | **`unknown`** — reason `segment_evaluation_budget_exceeded` |

Three consequences, each binding:

1. **A `member_of` leaf never yields `false` for a definitional reason** —
   only for a substantive one (the referenced criteria genuinely evaluated
   to `false` for this player at this `as_of`). "I could not tell you" and
   "the answer is no" are different answers, and only the second one is
   safe to negate.
2. **Static (`§3.3`) membership is not exempt.** `member_of` over a
   `static` segment that exists and is `active` returns `false` for a
   player with no `add` event in the fold — that is a substantive `false`,
   and negating it is correct. An *empty* static segment is still a
   substantive `false` for every player; an *absent or disabled* one is
   `unknown`. The distinction is the segment's existence and status, never
   the list's length.
3. **Terminal mapping is a boundary operation, not a node operation** —
   see the corrected §5.3.

**Authoring-time mitigation (necessary but NOT sufficient, and stated as
such):** the acyclicity validator (§5.1) is extended to also reject
authoring a version whose tree references a segment that is absent, or
`disabled`, or has no effective version — so the common case fails at
authoring rather than silently at evaluation. This does **not** replace
the evaluation-time rule above, because a referenced segment can be
disabled *after* the referencing version is frozen and in production
use, which is precisely the exploit path. Authoring validation narrows
the window; the `unknown` rule closes it.

**A second-order obligation this creates, stated rather than hidden:**
because disabling a segment now degrades every referencing segment's
answer to `unknown` (and, at an inclusion boundary, to `not_member`), the
`disabled` transition is operationally *more* consequential than it
looks. The admin surface (§2, `admin.go`) must, before disabling,
enumerate and display the referencing `SegmentVersion`s — the same
"blast radius disclosed before the action" posture §8.4 and doc 31 §12.2
require elsewhere. Recorded as **OI-SEG-8**.

### 5.2 Determinism — the eight rules

An evaluation is a pure function of `(criteria_version, player identity,
as_of)`. Concretely:

1. **One `as_of` for the whole evaluation.** `clock_timestamp()` is read
   once, at entry, and passed to every leaf. Two leaves can never observe
   different instants. (The same discipline ADR 0034 §14.12 and doc 10
   W1 already require for time-sensitive bonus/RG computation.)
2. **Side-effect free.** No writes anywhere, no event emission, no
   locking read, no provider call. An evaluation may be run twice, or
   replayed a year later, with no consequence.
3. **No randomness, no wall-clock-derived bucketing.** A percentage
   rollout is *not* a segmentation primitive. If an audience split is
   needed it belongs to CRM's experimentation layer (doc 31 §10) and must
   be a deterministic hash of `(player_account_id, experiment_key,
   salt_version)` recorded with the salt version — never `rand()`, never
   `id % 10`.
4. **Normalized operand order.** `And`/`Or` children are canonically
   sorted before hashing and before evaluation, so two textually
   different authorings of the same logical criteria produce the same
   `criteria_hash` and the same result.
5. **Kleene three-valued logic** for unknowns — §5.3.
6. **Fail-closed terminal mapping** — §5.3.
7. **Bounded cost.** Each predicate declares a bounded read; an
   evaluation that exceeds a configured budget returns UNKNOWN (which is
   fail-closed), never a partial result treated as complete.
8. **No cross-player data.** A predicate may read only facts about the
   player being evaluated (plus tenant/brand/jurisdiction configuration).
   "Is this player in the top 5% of depositors" is a *ranking*, i.e. a
   cross-player read, and is therefore **not** a segmentation predicate —
   it is a `data-analytics` computation whose *output* may be imported as
   static membership (§3.3). Recorded as **OI-SEG-5**.

### 5.3 Unknown handling — Kleene logic, and why `NOT` makes it load-bearing

Any leaf may return UNKNOWN: the owning domain errored, the fact has no
resolver yet (C-14, C-19), the player has no such record, or the cost
budget was exceeded.

Mapping UNKNOWN to `false` is the obvious implementation and it is
**wrong**, because `Not(false) = true`: a player whose jurisdiction
cannot be resolved would *satisfy* `Not(jurisdiction_in([...]))` and be
swept into an audience they were meant to be excluded from. This is the
same class of accidental-ALLOW the `risk` specialist closed seven of in
Stage 4H-B0-R6, reached through a different door.

The rule, binding:

| Operator | Truth table |
|---|---|
| `And` | `false` if any child is `false`; else `unknown` if any child is `unknown`; else `true` |
| `Or` | `true` if any child is `true`; else `unknown` if any child is `unknown`; else `false` |
| `Not` | `true`↔`false`; **`unknown` → `unknown`** |

This is strong Kleene K3. It is order-independent (so short-circuit
evaluation is safe and cannot change the answer), and it makes UNKNOWN
non-erasable by negation.

**Terminal mapping — a BOUNDARY operation, applied exactly once
(corrected, Wave 1.5 Fix Wave, `code-reviewer` P1-1):**

The three-valued result is carried, three-valued, through *every* node of
the tree, including every `member_of` reference and every nested
sub-tree. It is collapsed to two values **only** at the top-level
`Resolve` return boundary, and never at an interior node:

```
   interior nodes  ──▶  {true, false, unknown}     (Kleene, §5.3's table,
                                                    NO collapse anywhere)
   Resolve() return ─▶  {member, not_member}       (collapse happens HERE,
                                                    once, and only here)
```

| At the `Resolve` boundary | Terminal result | Reason code |
|---|---|---|
| `true` | `member` | the tree's own reason |
| `false` | `not_member` | ordinary non-membership |
| `unknown` | `not_member` | **`segment_fact_unavailable`** — distinguishable from an ordinary `not_member`, and carrying the innermost contributing reason from `leaf_outcomes[]` |
| A non-nil `error` from `Resolve` | `member = false` for every caller | fail-closed, ADR 0037 §C.1 / `internal/risk`'s error contract |
| The **top-level** `segment_id` itself is absent, `disabled`, draft-only, or its pin is unresolvable | `member = false` | never "no restriction configured, therefore everyone" |

The last two rows are the original text's rule and are unchanged — they
were always correct **at the boundary**. What is corrected is that they
are no longer applied at an interior `member_of` node, where the
subsequent `Not` could and did invert them (§5.1.1). Concretely, for
§6's canonical hybrid shape:

| Referenced exclusion segment's state | Old (defective) | Corrected |
|---|---|---|
| `active`, player is a member | `Not(true)=false` ⇒ excluded ✓ | `Not(true)=false` ⇒ excluded ✓ |
| `active`, player is not a member | `Not(false)=true` ⇒ included ✓ | `Not(false)=true` ⇒ included ✓ |
| **`disabled` / `draft` / deleted / bad pin** | `Not(false)=true` ⇒ **included ✗ (accidental ALLOW)** | `Not(unknown)=unknown` ⇒ `And(…, unknown)` ⇒ at worst `unknown` ⇒ **`not_member` ✓** |

`Evaluation.result` retains its third value `unknown` in the returned
struct (§5.4 already declares it) so a consumer with an *exclusion* use —
whose fail-closed direction inverts, §5.3's asymmetry note and doc 31 §5
— can discharge its own obligation without re-deriving what the evaluator
already knew. **A consumer that ignores `unknown` and reads only
`result == member` is fail-closed for an inclusion use and fail-OPEN for
an exclusion use**; doc 31 §5's inverted mapping is what makes the
exclusion case correct, and it depends on this value being carried
rather than pre-collapsed.

**The asymmetry that makes fail-closed correct here**: a segment is only
ever an *inclusion* input (§8.1). Failing closed therefore withholds a
promotion from a player who may have deserved it — a commercial loss —
and never grants value to a player who should not have received it, and
never lets a player past a safety gate. If a future consumer wants to use
segment membership to *exclude* (a suppression list), fail-closed must
invert for that consumer, and doing so correctly is that consumer's
obligation, stated explicitly in §8.4 and doc 31 §8.

### 5.4 `Resolve` — the interface

Superseding nothing in doc 29 §3.2, which named the same boundary in its
minimal `IsMember` form; this is that interface with the evidence the
fuller requirement list makes mandatory:

```
Resolve(ctx, tx, ResolveRequest) (Evaluation, error)

ResolveRequest:  tenant_id, brand_id, player_account_id,
                 segment_id, segment_version_id (optional pin),
                 as_of (optional; defaults to clock_timestamp()),
                 use (inclusion | exclusion — MANDATORY, §8.4.2;
                      no default, an absent value is an error)
Evaluation:      result (member|not_member|unknown), reason_code,
                 segment_version_id, criteria_hash, evaluated_as_of,
                 evaluator_version, inclusion_safety, leaf_outcomes[]
```

Two fields added in the Wave 1.5 Fix Wave, both load-bearing rather than
informational:

- **`ResolveRequest.use` is mandatory and has no default.** It is what
  makes §8.4.2's check possible at all, and it is what tells a consumer's
  own fail-closed direction apart (inclusion collapses `unknown` →
  `not_member`; exclusion must collapse `unknown` → *excluded*, doc 31
  §5). A default would silently pick one, and the one it picked would be
  wrong half the time. `IsMember`'s convenience wrapper therefore also
  takes `use` — there is no zero-argument shortcut.
- **`Evaluation.inclusion_safety`** carries the tree's computed
  classification (§8.4.1) so a consumer recording evidence (§7) can
  reconstruct *why* a resolution was permitted, not merely that it was.

`IsMember` remains available as a convenience wrapper returning
`Evaluation.result == member`, but **no consumer with an audit obligation
may use it** — it discards exactly the evidence §7 requires. A `tx` is
taken so the evaluation reads inside the caller's transaction, the same
reason `assetregistry.CheckEligibility` takes one (task-registry
DR-4HB0R6-01, point 1).

Server-side resolution only. A segment id is never accepted from a
client; `tenant_id`/`brand_id` are never client-supplied
(`CLAUDE.md` multi-tenancy).

### 5.5 `criteria_hash`

A canonical digest over the normalized AST (§5.2 rule 4) plus the
`evaluator_version`. Its purpose is §7: a reconstruction can prove the
criteria it is replaying are byte-identical to the criteria that were in
force, without the consumer copying the whole criteria blob into its own
schema.

---

## 6. Static, dynamic, and hybrid segments

| Kind | Membership source | Evaluation | Reproducibility |
|---|---|---|---|
| `static` | §3.3's append-only event log only | A fold of membership events as-of `as_of` | Exact, forever, by replaying the event log |
| `dynamic` | Criteria only; **membership is never stored** | Live evaluation at `as_of` | Exact *if* every leaf predicate's owning domain can answer as-of a past instant; see below |
| `hybrid` | `Or(member_of_static_list, criteria)` or `And(criteria, Not(member_of_exclusion_list))` | As dynamic, with a static leaf | As dynamic |

`hybrid` is not a third mechanism — it is composition (§5.1) of the two,
named here only because operators will ask for it and it must not be
mistaken for a new engine feature.

**The `And(criteria, Not(member_of(exclusion_list)))` shape above is
exactly the shape `code-reviewer`'s P1-1 exploited**, and it is safe only
under §5.1.1's corrected `member_of` semantics plus §5.3's
boundary-only terminal mapping. An implementation that maps an
unresolvable `member_of` to `false` at the node makes this canonical,
operator-facing, documented shape an accidental-ALLOW generator. It is
called out here, in the section that recommends the shape, and not only
in §5, so that an implementer reading §6 in isolation cannot miss it.

**Dynamic membership is never materialized.** Doc 10 W7 already commits
to this ("never stored; evaluated live against the current SegmentVersion's
criteria at the instant of use"), and it is the property that prevents a
stale membership cache from becoming a de facto second source of truth.

**The honest limit on dynamic reproducibility**: replaying a dynamic
evaluation at a past `as_of` is exact only where the owning domain is
itself append-only/temporal (the ledger is; `audit_log` is; KYC status
history is a state machine with recorded transitions). Where it is not,
the **snapshot** (§3.4/§7) — not a re-evaluation — is the authoritative
reconstruction. This is why §7 requires recording the *result*, and not
merely the inputs.

**Caching**: a resolution result may be cached only within one request/
transaction. No cross-request cache, no Redis, no materialized membership
table. `CLAUDE.md`'s "Redis never holds an authoritative balance and is
never read on the bet/settlement path" is a financial rule, but its
reasoning — an authoritative answer is read inside the transaction that
acts on it — applies unchanged to an answer that gates a value-creating
Grant.

---

## 7. Reconstruction — the Eligibility Decision Record

The directive requires that a Bonus eligibility decision record enough
versioned information to reconstruct: **segment(s), segment version(s),
criteria, eligibility result, Risk result, RG result, KYC result,
jurisdiction result.** This section specifies exactly what that requires.

### 7.1 The two rules that make reconstruction possible

- **EDR-R1 — reference the immutable, copy the mutable.** A fact that
  lives in an immutable, versioned row (`segment_version_id`,
  `offer_version_id`, `campaign_version_id`, `risk_rule` version,
  `rounding_rule_id`) is recorded **by reference**, because the referent
  can never change. A fact that lives in a mutable row (a player's KYC
  status, an RG restriction set, a resolved jurisdiction) is recorded
  **by value, with its `as_of`**, because the referent will change. Any
  field recorded neither way is unreconstructable, and its absence is a
  defect, not a simplification.
- **EDR-R2 — one record per checkpoint, not per Grant.** Doc 10 **T.11**
  is explicit that RG, Risk and all seven `AssetAuthorization` layers are
  **re-evaluated live** at every value-moving checkpoint (activation,
  reward credit, conversion), while the Offer's eligibility axis is
  snapshotted once at `issued`. A single per-Grant record therefore
  cannot represent the truth. The record is keyed
  `(tenant_id, grant_id, checkpoint, occurrence_ordinal)` — the
  `occurrence_ordinal` discipline ADR 0038 §14 already established for
  repeated same-type operations.

### 7.2 Required content

| Group | Field | Rule | Source of truth |
|---|---|---|---|
| Identity | `grant_id`, `checkpoint`, `occurrence_ordinal`, `decided_at` (`clock_timestamp()`), `correlation_id` | — | bonus |
| Offer | `offer_id`, `offer_version_id`, `campaign_id`, `campaign_version_id` | EDR-R1 reference | bonus (already in T.2) |
| **Segment** | per qualifying segment: `segment_id`, `segment_version_id`, `criteria_hash`, `result`, `reason_code`, `evaluated_as_of`, `evaluator_version` | EDR-R1 reference + result by value | **this document, §3.4/§5.4** |
| Eligibility | the Offer eligibility-axis outcome, per axis dimension, with a distinguishable reason code per dimension | by value | bonus (T.2/T.4) |
| **Risk** | `risk_decision` outcome, `reason_code`, matched `rule_id`s **with their rule versions**, `evaluated_at` | outcome by value; rules by reference-plus-version | `risk` (ADR 0031) |
| **RG** | `rg.Decision` code, `evaluated_at`, and the effective self-exclusion/restriction policy identifiers in force (including the jurisdiction-floor policy row's identity, per ADR 0034 §14 / migration 0043) | by value + as-of | `identity-compliance` |
| **KYC** | `VerificationStatus`, `kyc_tier` (when populated), `evaluated_at` | by value + as-of; **never any document, evidence, or PII** (doc 22's identity-evidence rule; doc 16) | `kyc` (ADR 0028) |
| **Jurisdiction** | `jurisdiction_code`, **how it was resolved** (resolver source + its version), `licensing_mode`, the `TenantJurisdictionConfig` version in force | by value + as-of | ADR 0006 / doc 15 |
| Asset authorization | the layer-by-layer `CheckEligibility` outcome + `reason_code` | by value | `architect` (ADR 0037) |
| Provenance | `actor_type`/`actor_id`, `trigger_type`, `trigger_reference` | by value | bonus (W1) |
| CRM/Affiliate (when those domains exist) | `crm_journey_id` + `journey_version_id` + `experiment_variant_id`; `attribution_id` + `attribution_model_version` | EDR-R1 reference | doc 31 §10, doc 32 §5 |

Written **append-only, in the same database transaction as the
transition it explains**, exactly as `audit.Record` already is
(ADR 0013; doc 16's "same database transaction as the action it
records"). Never updated, never deleted.

### 7.3 This is a dependency, not a redefinition

`bonus-engine` owns doc 10 and `ledger-finance` owns the accounting
schema. Doc 10 **W2.4** already commits to a specific and — in my
reading — correct shape: `GrantActivation` is *not* a new table but a
projection over the `bonus_progress` row plus the ledger transaction
group, exposing "the three-way gate outcome consulted at activation
(`AssetAuthorization`/RG/Risk decision codes)".

What §7.2 asks for is **strictly more than decision codes**: it asks for
versioned provenance (segment version + criteria hash, risk rule
versions, jurisdiction resolver + config version, RG policy identity,
KYC as-of). Whether that lands as additional columns on `bonus_progress`,
a `bonus_eligibility_decisions` child table, or a structured
`decision_evidence` document on the Progress row, is **`bonus-engine`'s
and `ledger-finance`'s decision, not mine.**

Filed as dependency **DEP-SEG-1** (§13). `architect` specifies the
required *content* and the two invariants (EDR-R1/EDR-R2); the owning
specialists specify the shape. This document does not unilaterally
redefine their Wave-1 eligibility-decision-reference design, and §12
records that it does not contradict it either.

---

## 8. The safety boundary

### 8.1 SEG-3 — a segment is an eligibility INPUT, never an eligibility DECISION

Binding, and the single most important statement in this document:

> Segment membership may make a player **eligible for an Offer's terms**.
> It can never make a player **authorized to receive value**.
> A `true` from `Resolve` is worth exactly nothing at the gate.

Composition order at every value-creating checkpoint is unchanged from
doc 10 §T.1 and doc 29 §8 (BI-7):

```
   segment resolution (commercial targeting; may return member)
        ↓  (an input, carried as evidence — not an authorization)
   AssetAuthorization.CheckEligibility   ── DENY ⇒ stop
        ↓
   rg.EvaluateEligibility                ── DENY ⇒ stop
        ↓
   risk.Evaluate                         ── DENY ⇒ stop
        ↓
   effect (the Grant transition + posting, same transaction)
```

Segment resolution sits **before** and **outside** that chain. It cannot
be inserted into it, cannot short-circuit it, and cannot be consulted
after a DENY.

### 8.2 SEG-4 — no segment may override an authoritative DENY

There is no "VIP override", no "trusted segment", no bypass flag, and no
configuration in which membership weakens a gate. Any proposal of that
shape is a blocking finding for `code-reviewer` and a rejected design for
`architect`. A mechanically checkable form: **no `internal/segment`
symbol may ever appear in a code path between a gate call and its
enforcement branch**, and `internal/rg`, `internal/risk`,
`internal/assetregistry` and `internal/kyc` must never import
`internal/segment` (§10.1, invariant SI-2).

### 8.3 SEG-6 — "Risk classification" reads FROM `internal/risk`, and today there is nothing to read

Verified directly against `HEAD`, not inferred: `internal/risk` exposes
`Rule`, `RiskRequest`, `RiskDecision`, `Outcome` and `MatchedRule`. It
produces a **per-operation decision**. It has **no persistent player risk
score, risk tier, or risk classification**, and the only table migration
`0041`/`0046` create is `risk_rules`. `grep` for `risk_score`/`risk_tier`/
"classification" across `docs/decisions/0031-risk-and-limits-engine.md`
returns nothing.

Therefore:

- A C-19 segment criterion is **`BLOCKED`**, not "to be implemented by
  reading some risk field."
- Segmentation **must not** compute a risk score, a risk tier, a
  velocity counter, a "suspicious" heuristic, or any proxy for one. Doc
  02 already records this failure shape twice ("Gamification is NOT a
  Risk replacement", "Retail is NOT a Risk replacement"); this is the
  third occurrence and it is prevented the same way.
- If a player risk classification is ever wanted, it is **`risk`'s to
  design**, through ADR 0031 §12's extension process, and segmentation
  becomes a read-only consumer of whatever `risk` defines. Recorded as
  **OI-SEG-1**, routed to `risk`.
- `bonus-engine`'s own abuse detector (doc 10 §1.4 — a Bonus-Engine-owned
  detector, explicitly *not* a Risk rule) is likewise **not** a
  segmentation input in either direction: its outcomes reach segmentation
  only as C-18 bonus history, through `bonus-engine`'s read API.

### 8.4 SEG-7 — protective signals may only narrow an audience, never widen one

RG status (C-22) and any future harm-risk indicator are **asymmetric**:

| Use | Permitted? |
|---|---|
| Exclude an RG-restricted/self-excluded player from a marketing audience or a promotional Offer | **Yes — and doc 31 §8 makes it mandatory, enforced by our code** |
| Include a player in an audience *because* they are RG-restricted, self-excluded, on a cooling-off, or carry a harm indicator — for any promotional, re-engagement, reactivation or bonus purpose | **No. Structurally forbidden.** |

#### 8.4.1 `InclusionSafety` is a computed property of a TREE, not a flag on a key (corrected, Wave 1.5 Fix Wave)

The Wave 1.5 original stated this as "every entry in §3.5's predicate
registry carries an `inclusion_safe` flag; a predicate with
`inclusion_safe = false` may appear only under a `Not(...)` in an
exclusion position." `code-reviewer`'s finding **P1-2** is that this has
no propagation rule through composition, and it demonstrated two concrete
laundering paths that defeat it without ever tripping the validator:

- **Path 1 — nested segment.** `member_of` is *necessarily* a
  taint-free predicate key (legitimate inclusion uses of segment
  references exist and are the norm — §6's hybrid shape, doc 10 W2.2's
  Offer eligibility axis). So an operator defines Segment X containing an
  RG-restriction criterion under an exclusion-use declaration, then
  Segment Y whose tree is a plain inclusion `member_of(X)`. A validator
  that inspects only leaf keys sees `member_of` and passes.
- **Path 2 — via CRM lifecycle (C-03).** Doc 31 §3's
  `PlayerLifecycleState` set includes `excluded`, which doc 31 itself
  defines as "a projection of `internal/rg`'s authoritative status." The
  state set is tenant-scoped, operator-extensible configuration. C-03
  reads it through CRM's projection. `lifecycle_state_in([...])` cannot
  sanely be marked wholesale unsafe (most of its state set —
  `dormant`, `churned`, `reactivated` — is exactly what commercial
  targeting is for), so RG status reaches an inclusion criterion with the
  validator never seeing an RG predicate at all.

Both are real. The fix is to stop treating safety as a property of a
*key* and make it a property of an evaluated *tree*, computed bottom-up.

**`InclusionSafety(node)`, binding, computed at authoring time over the
normalized AST (§5.2 rule 4) and recorded on the `SegmentVersion` row:**

```
InclusionSafety : Node → { safe, exclusion_only }

Leaf(key, params):
    exclusion_only  if registry[key].protective_signal
    exclusion_only  if any p ∈ registry[key].protective_signal_inputs
                       is satisfiable by params
                       (i.e. params could select a tainted value;
                        a parameter that is not a closed, enumerated,
                        authoring-time-fixed set is conservatively
                        treated as satisfiable — fail closed)
    safe            otherwise

member_of(seg, pin):
    InclusionSafety(resolved SegmentVersion's own criteria tree)
    — computed transitively, NOT read from the leaf key.
      Unresolvable at authoring time ⇒ exclusion_only (fail closed).

And(c…), Or(c…):
    exclusion_only  if ANY child is exclusion_only
    safe            otherwise

Not(c):
    safe            (negation is the SANCTIONED position for a
                     protective signal — this is the one node type
                     that clears the taint, and only here)
```

Read plainly: **taint propagates upward through every composition except
a `Not`**, and `Not` clears it because "exclude the RG-restricted" is
precisely the permitted use. `Or` is deliberately as strict as `And`: an
`Or` containing a tainted branch is an audience a restricted player can
enter *because* they are restricted, which is the forbidden direction.

**Two properties this gives that the flag-per-key version did not:**

- It closes Path 1, because `member_of`'s safety is the *referenced
  tree's* computed safety, not the `member_of` key's.
- It is **stable against later edits** only if the referenced version is
  immutable — which §3.2 already guarantees. A referenced segment that is
  *disabled* after the fact degrades to `unknown` at evaluation (§5.1.1),
  not to a safety downgrade, so the two corrections compose rather than
  interfere.

**The recomputation obligation, stated because it is the obvious way to
get this wrong:** `InclusionSafety` is recorded on the `SegmentVersion`
at authoring time, and a `SegmentVersion` is immutable — but the
*registry* is compiled-in and can change with a deploy. Adding a
`protective_signal_inputs[]` entry to an existing predicate key (e.g.
`identity-compliance` later declares a second lifecycle state
RG-derived) can retroactively make an already-frozen version
`exclusion_only`. Therefore: the stored value is a **cache, not the
authority**; it is recomputed at evaluator startup for every `active`
`SegmentVersion`, a divergence is a startup-time hard failure (not a
warning), and the recomputation result — never the stored one — is what
SI-7 enforces against. This mirrors the "recompute and diff against the
projection" discipline `CLAUDE.md` mandates for balances, applied to a
safety classification.

#### 8.4.2 Enforcement points

An **inclusion position** is any consumer call whose declared use is
inclusion. Consumers declare their use at the call site, not by
convention:

```
ResolveRequest.use := inclusion | exclusion
```

- `use = inclusion` with `InclusionSafety(tree) = exclusion_only`
  ⇒ `Resolve` **returns an error** (not `not_member`) —
  `ErrSegmentNotInclusionSafe`. A misconfiguration must be loud, and the
  fail-closed error contract (§5.3) already makes an error deny.
- `use = exclusion` accepts either classification.
- **The authoring-time validator rejects the same condition earlier**,
  where the consumer's declared use is known at authoring time (doc 31's
  `Audience` include/exclude terms are, doc 10's Offer eligibility axis
  is). Authoring-time rejection is the primary control; the
  `Resolve`-time check is the backstop for the case authoring cannot see.

#### 8.4.3 C-03 and C-05 — the concrete resolution of Path 2

Binding, and this is the resolution `code-reviewer`'s P1-2 required
(**"doc 30 C-03 must either exclude RG-derived lifecycle states from its
parameter domain or inherit doc 31's `excluded` as
`inclusion_safe=false`"**) — this document adopts **both halves**, which
is stricter than the either/or:

1. **C-03's predicate (`lifecycle_state_in([...])`) declares
   `protective_signal_inputs[] = { parameter: states, tainted values:
   every state doc 31 marks RG-derived }`.** Today that is exactly
   `excluded`. The set is **not** hardcoded here: doc 31 §3 owns the
   lifecycle state set, and doc 31 is amended (this round, §7.2/§3 of
   that document) to mark each state's derivation source so that
   segmentation reads the taint set rather than guessing it. A state
   whose derivation source is unknown or absent is treated as tainted
   (fail closed).
2. **`taint_free_domain` for C-03 is the explicitly enumerated set of
   non-RG-derived states** (`prospect`, `registered`, `verified`,
   `activated`, `active`, `at_risk`, `dormant`, `churned`,
   `reactivated`). A C-03 leaf whose `states` parameter is a subset of
   the taint-free domain computes `safe`; one that names `excluded`, or
   that uses any non-enumerated/dynamic parameter form, computes
   `exclusion_only`.
3. **An operator-extended lifecycle state** (doc 31 permits the set to be
   tenant-scoped configuration) is tainted unless its configuration row
   carries an explicit, audited, `identity-compliance`-reviewed
   non-RG-derivation declaration. Operator-extensible + permit-by-omission
   is how Path 2 got created; this is permit-by-enumeration instead.
4. **C-05 ("active/inactive/dormant" as a *windowed activity*
   predicate)** is unaffected and remains `safe`: it derives from ledger/
   activity recency, reads no RG fact, and is the sanctioned way to
   express "inactive" without touching CRM's lifecycle label. This is
   also the migration path for an operator whose real intent was
   commercial rather than protective.

#### 8.4.4 The residual gap, stated honestly

`InclusionSafety` catches taint that flows through *declared* channels:
a predicate's own protective signal, a declared protective input, and a
`member_of` reference. It **cannot** catch taint that flows through an
undeclared correlate — a criterion over, say, "deposit count dropped to
zero in the last 30 days" is statistically correlated with
self-exclusion, and no mechanical property will ever distinguish that
from ordinary churn targeting. This is not a defect this design can
close; it is why §8.4's rule is an engineering default routed to
compliance (**OI-SEG-2**) rather than a claim of completeness, and why
doc 31 §8.2's send gate calls `rg.EvaluateEligibility` **live, at send
time, unconditionally** — a second, independent control that does not
depend on the audience having been classified correctly. Stated so that
no reader concludes the audience-level control is sufficient on its own.

**This is a compliance-adjacent rule stated as an engineering default,
and I am explicitly not making a legal determination** (`CLAUDE.md`:
software capability and legal approval are different things; legal
interpretation is a stop-and-ask item). The default above is the
conservative direction and is reversible only *tighter*, never looser,
without a human/compliance decision. Routed to the Orchestrator for the
human as **OI-SEG-2**.

### 8.5 The circularity that must not close

C-18 (bonus history) means a segment can depend on Bonus, while Bonus
depends on segments for eligibility. This is safe **only** because
segment evaluation is side-effect free (§5.2 rule 2) and Bonus reads it
before, never during, its gate chain. Two concrete prohibitions:

- A segment criterion must not read a Grant that the very transaction
  being evaluated is creating. `as_of` is the transaction's entry
  instant (§5.2 rule 1), so an in-flight Grant is not visible to its own
  eligibility evaluation.
- `member_of` segment references are validated acyclic at authoring time
  (§5.1), so no segment can transitively depend on itself.

---

## 9. Operator-defined custom criteria, without a DSL

The directive requires operator-defined criteria. The anti-requirement is
equally real: a runtime expression language over arbitrary player data is
a new attack surface, an unbounded-cost surface, and an un-reviewable
policy surface.

**Resolution: operators compose and parameterize; they never author
predicates.**

| Operators may | Operators may not |
|---|---|
| Create/version segments in their own tenant/brand scope | Add a new predicate key |
| Compose registry predicates with AND/OR/NOT to the depth/size bound | Write an expression, SQL fragment, script, or template |
| Supply parameters within each predicate's declared schema and bounds | Reference a field no predicate exposes |
| Maintain static membership lists with audit and reason codes | Read or reference another player's data (§5.2 rule 8) |
| Reference another segment by id + version pin | Create a cycle, or exceed the node bound |

Adding a predicate to §3.5's registry follows the **five-step additive
extension model ADR 0031 §12 already established** for new Risk
`Operation`/`LimitKind` values — documented steps, owning-domain
sign-off, never ad hoc. Specifically: (1) name the fact and its
authoritative owning domain; (2) obtain that domain's read interface and
its `as_of` semantics; (3) declare the unknown-condition **and the full
protective-signal declaration of §3.5 — `protective_signal`,
`protective_signal_inputs[]` and `taint_free_domain` — as a mandatory,
non-defaultable field**, with the owning domain confirming which of its
own values are protective (this is the step Path 2 of P1-2 got past, and
it is now the step that catches it); (4) `security` review for
data-exposure and abuse;
(5) record it, with the consumer that needs it. A predicate with no named
first consumer is not added (`CLAUDE.md`'s scope test).

---

## 10. Multi-tenancy, security, and audit

### 10.1 Invariants (for `qa` and `code-reviewer`; each mechanically checkable)

| ID | Invariant | Check |
|---|---|---|
| **SI-1** | `segments`, `segment_versions`, `segment_memberships` carry `FORCE ROW LEVEL SECURITY`; the `tenant_staff_scope` policy carries the `app.player_account_id IS NULL` conjunct; no player-facing read policy exists at all (a player is never told which segments they are in — §10.3) | Schema inspection + cross-tenant and cross-player RLS tests. This is the exact gap `code-reviewer` found on `self_exclusion_enumeration_runs` in Stage 4H-B0-R6 (F2) |
| **SI-2** | `internal/rg`, `internal/risk`, `internal/kyc`, `internal/assetregistry`, `internal/ledger`, `internal/wallet` never import `internal/segment` | Import inspection (the technique doc 02 already uses for the risk/rg separation) |
| **SI-3** | `internal/segment` imports no provider package and contains no provider id, provider game id, brand-name, tenant-name or jurisdiction-code literal | grep + import inspection |
| **SI-4** | `Resolve` performs no write, no `SELECT … FOR UPDATE`, no publish, no external call | Code review + a test asserting zero rows change across an evaluation |
| **SI-5** | Evaluation is deterministic: the same `(criteria_version, player, as_of)` yields the identical `Evaluation`, including `reason_code`, across repeated runs and across operand re-orderings | Property test over the Kleene table and the normalization in §5.2 rule 4 |
| **SI-6** | `unknown` never becomes `member`, under any composition including negation | Property/fuzz test over the three-valued truth tables (§5.3) |
| **SI-7** | **(rewritten, P1-2)** No `SegmentVersion` whose **computed** `InclusionSafety` (§8.4.1) is `exclusion_only` is ever resolved with `use = inclusion` — enforced at authoring time AND as a `Resolve`-time backstop returning `ErrSegmentNotInclusionSafe` | Authoring-time validator + a `Resolve`-time test per tainted predicate + the three adversarial trees in SI-12 |
| **SI-11** | **(new, P1-1)** A `member_of` leaf over an absent / disabled / draft-only / unresolvable-pin / out-of-scope segment evaluates to `unknown`, never `false`; terminal `{true,false,unknown} → {member,not_member}` collapse occurs exactly once, at the `Resolve` return boundary, and at no interior node | Property test: for every interior node type, assert no collapse. Adversarial test: build `And(criteria, Not(member_of(X)))`, resolve it with X `active` (expect include/exclude correctly), then disable X and re-resolve — the player must **not** be swept in. The stored/returned `Evaluation.result` on the second run must be `not_member` with `segment_fact_unavailable` |
| **SI-12** | **(new, P1-2)** `InclusionSafety` propagates: (a) `member_of(X)` inherits X's computed safety transitively, not `member_of`'s key safety; (b) `And`/`Or` are `exclusion_only` if any child is; (c) only `Not` clears taint; (d) a stored `SegmentVersion.inclusion_safety` that diverges from recomputation at evaluator startup is a hard startup failure | Three adversarial trees as tests: nested-segment laundering (Path 1), lifecycle-state laundering (Path 2, `lifecycle_state_in([excluded])` as a plain inclusion), and an `Or` with one tainted branch. Plus a registry-change test that mutates a predicate's `protective_signal_inputs[]` and asserts the startup diff fails |
| **SI-8** | No dynamic membership is stored, cached across requests, or materialized | Schema inspection + grep |
| **SI-9** | `SegmentVersion` is immutable once referenced; `segment_memberships` is append-only (no `UPDATE`/`DELETE`) | Trigger/constraint inspection, mirroring `audit_log`'s enforcement (ADR 0013) |
| **SI-10** | A monetary predicate never compares across `asset_code`s and never performs a conversion | Code review + a test with two assets of different exponents |

### 10.2 Data minimization

A predicate's parameter set and a criteria tree may contain **no PII**: no
email, no name, no date of birth, no address, no document number, no PAN,
no payment-instrument token or fingerprint, no IP address. Criteria
reference *classes* and *thresholds* (method class, KYC status, amount
band), never identifying values. This follows doc 16's minimization
posture and doc 22's rule that an event carries an identity *reference*,
never identity evidence.

### 10.3 Player-facing exposure

A player is never told which segments they belong to, and a segment id
never appears in a player-facing API response. Doc 29 §5.1 already binds
this for Bonus ("must not leak Offer rule internals, campaign budget,
**segment membership logic**, or the existence of Offers the player is
not eligible for"); it is restated here as a property of segmentation
itself so that a future consumer cannot reintroduce the leak.

### 10.4 Audit

Every mutation of a segment definition, version, status, or static
membership writes an `audit.Record` in the same transaction, with actor,
tenant, entity, before/after and reason code (`CLAUDE.md`; ADR 0013).
Segment *resolution* is not audited by segmentation — it is recorded by
the consumer, as §7's evidence, where it is joined to the decision it
actually informed. A separate segmentation-side resolution log would be a
high-volume second record of the same fact with no decision attached.

---

## 11. What is NOT authorized, restated

Unchanged from doc 29 §3.2/§3.4 and doc 10 W7, and not widened by
anything above:

- No rule/expression DSL or runtime interpreter.
- No behavioural recompute job, scoring model, or scheduler.
- No real-time membership streaming or change-feed.
- No materialized membership table for dynamic segments.
- No segment-overlap/audience-size/analytics surface (that is
  `data-analytics`, doc 12).
- No journey builder, no campaign management UI (that is doc 31).
- No provider integration and no bought-CRM adapter (the interface exists
  so that a bought product *can* become a second implementation later —
  building the adapter now is not authorized).
- No player-facing surface of any kind.
- No predicate whose authoritative owner does not exist (§4.1).

A first implementation slice, whenever one is authorized, ships the
minimum its first consumer actually uses — per doc 29 §3.2, that is
static membership plus the small set of predicates the five in-slice
bonus types need, and nothing else.

---

## 12. Consistency check against Wave 1 — SEG-1 and SEG-2 re-confirmed

The directive asks explicitly whether doc 29 §3.6's two invariants still
hold against this fuller requirement list. **Both hold, unchanged.**

- **SEG-1 (resolve at eligibility time, snapshot the result)** — holds,
  and §6's honest limit on dynamic reproducibility makes it *more*
  load-bearing than doc 29 stated: where an owning domain is not
  temporal, the snapshot is the only faithful reconstruction. §7's EDR is
  SEG-1's concrete schema requirement.
- **SEG-2 (a segment is never the mechanism for a safety gate)** — holds
  verbatim. §8.1/§8.2 restate it as SEG-3/SEG-4 with an enforcement
  mechanism (SI-2) rather than a convention. One clarification the fuller
  list forces, which is an extension rather than a change: doc 29 §3.6
  listed "VIP tier" and "KYC state" among things that "are not segments."
  Read strictly that would conflict with C-01/C-15 and with doc 10
  W2.2, which requires the Offer's VIP-tier field to *be* a segment
  reference rather than a Bonus-local enum. The precise statement, which
  both documents intend and which this one adopts: **a segment may
  reference such a fact as a criterion, read from its owning domain; a
  segment may never be the place that fact is defined, computed, or
  enforced.** Recorded here rather than silently reconciled.

Doc 10 **W7** is consistent with all of the above — its hard boundary
("segmentation never becomes a second source of truth for RG/Risk/KYC/
licensing status, and segment membership never overrides or bypasses a
hard DENY") is the same rule as §8.1/§8.2, and its Segment/SegmentVersion/
static/dynamic shape is the same as §3. No contradiction was found
between doc 10 W7, doc 29 §3, and this document.

---

## 13. Open items and routed dependencies

| ID | Item | Owner | Blocking? |
|---|---|---|---|
| **DEP-SEG-1** | The Eligibility Decision Record (§7.2) extends doc 10 W2.4's activation projection from *decision codes* to *versioned provenance*. `bonus-engine` + `ledger-finance` decide the physical shape; `architect` specifies content + EDR-R1/EDR-R2 | bonus-engine + ledger-finance | Yes, before any Grant write path |
| **OI-SEG-1** | A player risk classification does not exist (§8.3). If one is ever wanted, `risk` designs it via ADR 0031 §12; segmentation stays a read-only consumer. C-19 is `BLOCKED` until then | risk | No (criterion simply unavailable) |
| **OI-SEG-2** | §8.4's asymmetric-use rule for RG/harm signals is an engineering default with a compliance dimension. Needs human/compliance confirmation; may be tightened, never loosened, without one | Orchestrator → human | No |
| **OI-SEG-3** | The standing `TODO(jurisdiction)` gap (doc 15, ADR 0031 §9) makes C-14 return UNKNOWN. Not newly introduced here; noted because a jurisdiction criterion is commercially obvious and would silently fail closed | identity-compliance / architect | No |
| **OI-SEG-4** | Cross-asset monetary criteria need a `ledger-finance`-owned, versioned aggregation+conversion policy (§4.2). Segmentation will not invent one | ledger-finance | No |
| **OI-SEG-5** | Ranking/percentile audiences ("top 5% of depositors") are cross-player reads and are therefore not segmentation predicates (§5.2 rule 8). The sanctioned path is a `data-analytics` computation imported as static membership | data-analytics | No |
| **OI-SEG-6** | C-01 (VIP tier) has no authoritative owner until Gamification exists. Static membership is the only sanctioned stand-in (§4.1) | Orchestrator | No |
| **OI-SEG-7** | An ADR recording the three new domain boundaries created by this gate (Segmentation, CRM, Affiliate) is recommended. Number deliberately not claimed here — parallel-dispatch ADR-number collision has already happened once in this project (Stage 4H-B0-R6's `0049`) | Orchestrator | No |
| **OI-SEG-8** | **(new, Fix Wave)** Disabling a `Segment` now degrades every referencing `SegmentVersion`'s answer to `unknown` (§5.1.1). The admin surface must enumerate and display referencing versions before permitting the disable — blast radius disclosed before the action, the same posture §8.4/doc 31 §12.2 require elsewhere | architect (interface) + whoever implements `admin.go` | No, but must land with the first `member_of` implementation |
| **DEP-SEG-2** | **(new, Fix Wave)** §8.4.3 requires doc 31's `PlayerLifecycleState` set to carry a per-state **derivation-source** declaration (RG-derived vs. not), readable by segmentation, with an unknown/absent source treated as tainted. `architect` applies the doc 31 half this round; the *operator-extended-state* declaration requires `identity-compliance` review of what counts as RG-derived | identity-compliance (+ architect) | Yes, before C-03 is implementable |
| **DEP-SEG-3** | **(new, Fix Wave)** `security` owns the final word on whether §8.4.1's `Not`-clears-taint rule is sufficient, and on whether `exclusion_only` trees need a distinct authoring permission (authoring a suppression audience is a different act from authoring a marketing one). `architect` specifies the mechanism; `security` decides the authority model | security | No, but recommended before implementation |

### 13.1 Fix Wave changelog — what changed in this document and why

Recorded explicitly so a reader diffing against the Wave 1.5 original
knows which statements were defective rather than merely expanded.

| Change | Driver | Section |
|---|---|---|
| `member_of` over an absent/disabled/draft/unresolvable-pin segment now returns `unknown`, not `false` | `code-reviewer` **P1-1** (real defect: reachable accidental-ALLOW through §6's own canonical hybrid shape) | new §5.1.1 |
| Terminal `{true,false,unknown}` collapse is now explicitly a boundary-only operation, with the before/after exploit table | `code-reviewer` **P1-1** | §5.3 (rewritten) |
| §6's hybrid shape carries an in-place warning that it is the exploited shape | `code-reviewer` **P1-1** | §6 |
| `inclusion_safe` (static flag on a predicate key) replaced by `InclusionSafety` (computed property of a tree, propagating through `member_of` and through declared protective inputs) | `code-reviewer` **P1-2** (two demonstrated laundering paths) | §3.5, new §8.4.1–§8.4.4 |
| C-03 gains a protective-signal declaration + an enumerated taint-free domain; C-22 gains an explicit `protective_signal = true` | `code-reviewer` **P1-2** | §4 table, §8.4.3 |
| `ResolveRequest.use` added as a mandatory, non-defaultable field; `Evaluation.inclusion_safety` added | Required to make §8.4.2 enforceable at the call boundary | §5.4 |
| §9's extension process step 3 now requires the full protective-signal declaration, not a boolean | `code-reviewer` **P1-2** | §9 |
| SI-7 rewritten; SI-11 and SI-12 added with named adversarial tests | Both findings; `qa` needs a mechanically checkable form | §10.1 |
| OI-SEG-8, DEP-SEG-2, DEP-SEG-3 added | Consequences the two fixes create | §13 |

**Not changed, and deliberately so:** SEG-3/SEG-4 (§8.1/§8.2), the gate
composition order, the "segmentation computes no fact" rule (§4.1), the
non-materialization rule (§6), and §7's EDR content. None of the two
findings touched them, and neither fix weakens any of them.

## 14. Cross-references

- Placement decision and its reversal cost: `29-bonus-implementation-contract.md` §3
- Bonus's own segmentation shape and hard boundary: `10-bonus-engine-architecture.md` **W7**; eligibility axis W2.2; immutable-vs-live table **T.11**
- CRM consumption: `31-crm-engine-architecture.md`
- Affiliate/acquisition criterion (C-21): `32-affiliate-and-acquisition-architecture.md`
- Flow diagrams and build order: `33-cross-domain-commercial-flow-map.md`
- Risk contract: `docs/decisions/0031-risk-and-limits-engine.md`
- RG/KYC integration: `docs/decisions/0034-…`, ADR 0026, ADR 0028
- Asset authorization / fail-closed precedent: `docs/decisions/0037-…` §C
- Event taxonomy: `22-canonical-activity-event-taxonomy.md`
- Privacy/minimization: `16-privacy.md`
