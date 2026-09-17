# Active Stage

## Stage 4H-B0-R3 — Bonus Rounding Decision Validation and Financial Gate Closure — Complete

Status: **Complete. Stage 4H-B1 is READY FOR HUMAN AUTHORIZATION after
completion of the `bonus_conversion` Risk dependency.**

The human proposed answers to the three ADR 0021 rounding questions
(DS-1/DS-2/DS-3) raised in Stage 4H-B0-R2's decision sheet. This stage's
job was to validate those proposals against the existing architecture —
never to select or silently change them — using six specialists in
parallel (`ledger-finance`, `bonus-engine`, `risk`, `architect`,
`security`, `qa`), then formally record the decision if safe and
re-check the overall financial gate.

**Proposed and recorded**: DS-1 = round-half-up (ties away from zero);
DS-2 = round once, at the final monetary boundary, full `NUMERIC`
precision until then, via an explicit shared function, never an implicit
database cast, no truncate-and-carry unless the architecture requires
it; DS-3 = one platform-wide rule by default, room for a future
per-asset/jurisdiction override if genuinely required.

**Validation verdict: VALID AND READY TO RECORD.** No specialist found a
contradiction, financial problem, precision problem, reconciliation
problem, or architecturally unsafe consequence requiring the human to
change the proposal. Specific findings folded into the recorded decision
as clarifications, not changes:

- **Zero-rounding-result edge case** (`ledger-finance`): a bonus that
  computes to exactly 0 minor units cannot post (`ledger_entries.amount
  > 0`). This is a Bonus Engine eligibility/config question (e.g. a
  minimum-deposit guard), not a rounding-rule question — flagged for
  Stage 4H-B1, does not block recording the decision.
- **Wagering-requirement vs. contribution-weighting distinction**
  (`bonus-engine`, resolving an ambiguity `ledger-finance` had flagged):
  the wagering-requirement target is a comparison threshold, never
  posted to the ledger, so DS-2's "final monetary boundary" language
  doesn't apply to it in the posting sense. Per-game contribution
  weighting **is** monetary — it determines the actual cash/bonus split
  posted for a wagering event — and DS-2's boundary for it is the
  split-instruction computation. Recorded explicitly in ADR 0021 so this
  is not left ambiguous for Stage 4H-B1.
- **Cashback residual — confirmed consequence, not silently assumed**:
  the existing architecture has no remainder-accumulation mechanism
  anywhere (`ledger-finance` searched all four relevant documents).
  Building one (truncate-and-carry) would be new, unauthorized
  architecture. What DS-1+DS-2 as recorded actually mean for cashback:
  each calculation rounds independently and immediately, no
  accumulation — recorded explicitly rather than left implicit, per
  `ledger-finance`'s own recommendation to surface this back to the
  human.
- **Exact multiply→cap-compare→round ordering** (`bonus-engine`): DS-2
  says "round once at the final boundary" — the only reading consistent
  with that language is rounding `min(exact_%_result, cap)` once, after
  both the percentage multiply and the cap comparison, not the raw
  percentage result before the cap decision. Recorded explicitly.
- **Coupon's Reward-axis shape** (`bonus-engine`): doc 10 does not pin
  down whether an in-slice Coupon is configured as a flat grant or a
  %-based Offer — flagged as an open Stage 4H-B1 template-authoring
  scoping question, unrelated to the rounding decision.
- **Exact algorithm specified** (`ledger-finance`): round half away from
  zero — `sign(x) × floor(|x| + 0.5)` — via one named, shared function,
  never a bare `ROUND()` call or an implicit `NUMERIC(38,0)` column-scale
  coercion (PostgreSQL's own implicit coercion happens to match
  round-half-up for positive values today — a coincidence, not a
  specification, and must never be relied upon). Negative-input contract
  specified for future FX/commission reuse even though no bonus call
  site produces a negative input today.
- **Storage location specified** (`security`): an immutable, append-only
  `rounding_rules` reference table (one composite version per Q1+Q2
  combination) plus the applied identifier denormalized onto
  `ledger_transactions` at post time — mirroring the existing
  denormalization rationale already used for `tenant_id`/`wallet_id`/
  `player_account_id`/`asset_code` on `LedgerEntry`.
- **Risk evaluates only the post-rounded amount** (`risk`, settled by
  construction, no ambiguity found or manufactured): `RiskRequest.
  Amount`'s `int64` type cannot hold a pre-rounding exact value, and the
  one live precedent (`postBet`) already passes Risk the identical
  integer that becomes the ledger posting. No Risk-side change is a
  prerequisite for recording this decision.
- **Multi-asset genericity confirmed** (`qa`): the algorithm is exponent-
  agnostic by construction (operates on minor-unit integers, never
  hardcodes a decimal count) — validated across 0-, 2-, 6-, 8-, and
  18-decimal assets, with two schema-legal-but-not-yet-populated
  exponents (0 and 18) needing synthetic test fixtures rather than live
  registry rows.

**Full recorded decision**: `docs/decisions/0021-multi-asset-
accounting.md`'s "Rounding and precision — RESOLVED" section — the
authoritative text, including the algorithm, storage specification, and
all clarifications above. `docs/architecture/28-bonus-financial-gate-
decision-sheet.md` is now marked RESOLVED and retained as the
historical record of the question as originally put to the human.

**`bonus_conversion` re-verified, unchanged**: `risk` re-checked all six
ADR 0031 §16 artifacts directly against current repository state (commit
`aa4a926`, confirmed docs-only) — still **NOT STARTED, zero of six
steps**. Per the directive's explicit instruction ("prefer keeping
implementation for Stage 4H-B1... do not silently implement it unless
this stage's final gate explicitly determines it is necessary"), `risk`
confirmed the rounding decision requires no Risk-side implementation now
— it was not implemented.

**Final B1 gate review**: **Stage 4H-B1 is READY FOR HUMAN
AUTHORIZATION after completion of `bonus_conversion`.** No P0, no P1
financial blocker, no unresolved accounting decision, no unresolved
precision decision, no unresolved rounding ambiguity remains —
confirmed by `architect`'s re-run of the same 12-area focused review
from Stage 4H-B0-R2.

**New confirmed product requirement — extensible Asset/Currency Registry
+ FX/Conversion architecture (analysis only, not implemented)**:
`architect` found the `assets` schema is already open/extensible
(no closed enum, no hardcoded decimal count anywhere in `internal/`) but
lacks the operational surface the requirement needs — an admin API, an
authorization model for who may register/activate an asset, mandatory
audit logging on that mutation, and additional per-asset eligibility
columns. Recommended a future ADR (0037) and a dedicated future
implementation stage, both recorded as deferred
(`docs/architecture/14-mvp-scope-and-roadmap.md`,
`docs/architecture/27-*.md` §26). The FX/Conversion boundary was
designed on paper only — Asset/Currency Registry, FX Rate Provider,
Conversion Service, and the Ledger's existing `ConversionOperation` kept
structurally separate, with the immutable audit fields any future
conversion must retain specified. Confirmed: **no impact on Bonus Stage
4H-B1; no new blocker for Retail** beyond the pre-existing conversion-
clearing-account open decision. Confirmed: **no expansion of the Bonus
MVP, no FX implementation, no new payment/custody provider, no retail
implementation** this stage. Two P1 risks flagged for the eventual
Registry design (undefined asset-creation authorization boundary;
fail-closed FX behavior must be written into the new ADR as a binding
rule now).

**No production code, no migrations, no implementation was authorized or
started this stage.** `go build ./...` remains clean (docs-only diff).

### Decisions/input needed before the next stage

None remain for the Bonus rounding decision — it is resolved. Stage
4H-B1 can be authorized once `bonus_conversion`'s six-step checklist
(ADR 0031 §16a) is completed as part of that stage's own work. The
Asset/Currency Registry + FX architecture requires no immediate human
decision — it is recorded as a deferred future stage with a
recommended future ADR number, not something blocking any currently
open stage.

---

## Stage 4H-B0-R2 — Bonus Financial Gate Clarification — Complete

Status: **Complete, pending an explicit human decision on the ADR 0021
rounding questions before Stage 4H-B1 can be authorized.**

A financial-gate clarification stage — no architecture redesign, no
production code, no migrations. Purpose: close the remaining financial-
design gate for Bonus implementation by preparing an exact human
decision package for ADR 0021 and independently re-verifying the
remaining Risk dependency and every other financial-gate area. Six
specialists (`ledger-finance`, `bonus-engine`, `risk`, `architect`,
`security`, `qa`) reviewed in parallel, each verifying directly against
current repository state at HEAD rather than trusting prior-stage prose.

**New document**: `docs/architecture/28-bonus-financial-gate-decision-
sheet.md` — the deliverable this stage exists to produce. Written for a
non-accountant business owner. Contains:

- **DS-1/DS-2/DS-3**: the three linked human decisions inside ADR 0021's
  rounding/precision `OPEN DECISION` (rounding direction; where and how
  rounding is applied; whether the rule is uniform platform-wide or may
  vary), each with plain-language options, financial consequences, and a
  numerical worked example (a 50% deposit-match landing exactly on a
  half-cent tie; a repeating 7.3% weekly cashback showing the
  truncate-and-carry mechanism concretely). No option is selected or
  recommended.
- **A precise bonus-type impact table**: the rounding decision affects
  the grant amount itself for Deposit bonus, Reload bonus, and Cashback;
  for the generic Wagering bonus and Coupon it affects only the derived
  wagering-requirement/contribution-tracking computation, not the flat
  grant/face amount those two types actually pay out — a materially more
  precise finding than a blanket "all five types are affected
  identically."
- **A six-step `bonus_conversion` engineering checklist**, formatted for
  the decision sheet but explicitly labeled informational, not a
  decision for the human — `risk` re-verified the dependency is still
  NOT STARTED (zero of six ADR 0031 §16 steps) against current
  repository state, with no code changed since Stage 4H-B0-R1.
- **Confirmation that no other blocker exists**: `architect` performed a
  focused 12-area review (ledger accounting, wallet, idempotency,
  concurrency, Risk, RG, audit, RLS, reconciliation, transaction/account
  types, multi-asset precision, bonus conversion) and found **no
  additional P0/P1 blocker** beyond the two already-known gates.

**Specialist findings of note:**

- `ledger-finance` confirmed no rounding option can break
  `SUM(DEBITS)==SUM(CREDITS)` — options A-E have no separable residue to
  drop at the bonus-grant/cashback posting site (the mirrored legs are
  always posted with the same already-rounded integer); option F
  introduces genuine new financial state (a remainder accumulator)
  needing the same rigor as the ledger itself, plus an unresolved
  reversal/forfeiture policy for an unreleased remainder. Flagged a
  concrete implementation trap: PostgreSQL's default numeric-to-integer
  cast silently implements round-half-up, so whichever option is chosen
  must be an explicit function in the platform's one shared rounding
  helper, never an implicit cast.
- `bonus-engine` independently confirmed (not merely trusted) that all
  five in-slice types reach `completed → converted` and therefore all
  five require the `bonus_conversion` Risk dependency, even though only
  three of the five have their *payout amount* affected by the rounding
  decision.
- `security` found no additional blocker; flagged that whichever
  rounding rule is eventually built must ship with an explicit manage
  permission and mandatory audit logging on any rule change, and found
  one minor non-blocking audit-table completeness gap (`reversed` bonus
  transitions have no explicit row in doc 10 §10's audit table, though
  the Progress-trail requirement already covers it).
- `qa` confirmed the core CLAUDE.md financial test matrix is already
  well-mapped to bonus scenarios, designed the property-based test
  approach needed to prove whichever rounding rule is chosen is
  deterministic and exactly recomputable, and flagged several
  non-blocking test-plan additions for engineering's backlog.

**No production code, no migrations, no implementation was authorized or
started this stage.** `go build ./...` remains clean (docs-only diff).

**Stage 4H-B1 remains BLOCKED and is NOT authorized.** This stage
produced the decision package; it did not make the rounding decision or
approve the next stage. The Master Orchestrator did not select an answer
to DS-1, DS-2, or DS-3, and no specialist was authorized to do so on the
user's behalf.

### Decisions/input needed before the next stage

The human reviews `docs/architecture/28-bonus-financial-gate-decision-
sheet.md` and answers DS-1, DS-2, and DS-3. Once answered and recorded in
ADR 0021, and once the `bonus_conversion` six-step checklist is completed
as part of Stage 4H-B1's own work, Stage 4H-B1 can be authorized.

---

## Stage 4H-B0-R1 — B0 Gate Corrections and Finalization — Complete

Status: **Complete, pending explicit human approval before Stage 4H-B1
(Bonus Engine) or Stage 4H-B2 (Retail Architecture Hardening).**

A correction/finalization stage responding to specific errors identified
in the Stage 4H-B0 completion report — **no architecture redesign, no
production code, no migrations, no implementation.** Governance record:
`docs/governance/task-registry.md`'s Stage 4H-B0-R1 rows;
`docs/governance/project-status.md`'s Stage 4H-B0-R1 section.

**Corrections made:**

1. **Bonus gate contradiction resolved.** Stage 4H-B0's report claimed
   Stage 4H-B1 was "independently authorizable" while its own text
   disclosed an unresolved ADR 0021 rounding dependency blocking 3 of the
   5 first-slice bonus types. **Corrected: Stage 4H-B1 is CONDITIONALLY
   READY, not independently ready**, blocked until (1) ADR 0021's
   rounding/precision decision is resolved by a human, (2) the
   `bonus_conversion` Risk dependency is completed and reviewed, (3) no
   other P0/P1 financial dependency remains. `risk` verified directly
   against repository state that `bonus_conversion` is **NOT STARTED —
   zero of ADR 0031 §16's six extension-process steps complete**,
   correcting an earlier draft's understatement ("one small dependency
   request"). `ledger-finance` enumerated ADR 0021's rounding decision as
   three separable questions (direction — 6 neutrally-presented options;
   rounding point/precision handling; uniformity/scope) without selecting
   one — see ADR 0021's new "Rounding and precision" section and doc 27
   §1.1/§24 #15.
2. **Retail P0 list reclassified.** The "8 P0 decisions block retail"
   framing conflated genuine human/business/legal decisions with
   engineering acceptance criteria. Split into doc 27 §23A (3 genuine
   human decisions: retail licensing/jurisdiction status; confirmation of
   the node-owned `agent_float` extension to ADR 0007; anonymous/bearer
   play policy) and §23B (mandatory engineering acceptance criteria that
   require no human input: fail-closed hierarchy RLS with no OR-NULL
   escape, no retail role holding `PermStaffManage`, RG-before-Risk
   ordering, closure-table write protection, server-side terminal
   credential resolution, POS idempotency namespace protection, offline
   fail-closed baseline, and others).
3. **Agent float vs. Player Wallet formalized.** ADR 0007's `Wallet`
   stays untouched and strictly player-owned; agent float is explicitly
   NOT a Player Wallet — a hierarchy-node-owned operational account in
   the SAME authoritative ledger, never a second ledger, never confused
   with a physical till (physical cash remains fulfillment/custody, not
   an authoritative balance). `ledger-finance` drafted the minimum
   additive schema change in ADR 0035 §1.3.1 (`ledger_accounts` gaining a
   nullable `hierarchy_node_id` + a `num_nonnulls(...) <= 1`
   mutual-exclusion CHECK + an owner-family CHECK), explicitly `NOT
   IMPLEMENTED`. Reading the actual migration
   (`migrations/0020_create_ledger_accounts.up.sql`) rather than trusting
   the prior draft, `ledger-finance` found and corrected a load-bearing
   error: the original claim that the amendment "changes no existing
   constraint" was **false** — the existing house-level unique index
   predicate (`wallet_id IS NULL`) would have silently collapsed every
   hierarchy node's `agent_float`/asset into one shared row per tenant,
   recreating exactly the P0 failure the amendment exists to prevent. The
   corrected proposal widens that predicate to `wallet_id IS NULL AND
   hierarchy_node_id IS NULL`. `architect` and `security` reviewed the
   corrected proposal this stage (see their added ADR 0035 subsections);
   the amendment remains `NOT IMPLEMENTED` and requires human approval
   before any migration is written, since it changes the practical shape
   of a table whose broader design traces to human-approved ADR 0007/0019.
4. **First Retail Product Baseline formalized** (doc 27 §1.3a):
   identified players only, no anonymous/bearer play, online connection
   required, no offline/store-and-forward, single retail currency
   initially, cash deposit/withdrawal, a fixed shallow hierarchy for the
   first contracted operator, no automated commissions, no
   agent-to-agent float transfer, no direct bank agent settlement, no
   proxy/assisted play — implementation-scope constraints for the first
   slice, not claims about what every jurisdiction permits.
5. **Generic hierarchy model reaffirmed**: Node Type/Structure/Capability
   remain separate concepts; Operator/Partner/SuperAgent/Agent remain
   seed/configuration data, never hardcoded schema roles; Player is not a
   hierarchy node; Cashier is a staff identity assigned to a node;
   Terminal is a service principal.
6. **Shared platform model restated** (doc 27 §9): Online and Retail are
   operating channels of one platform sharing Person, PlayerAccount,
   Identity Resolution, Wallet, Ledger, Risk, RG, KYC, Payments, Bonus,
   Audit, Reporting, Reconciliation — retail terminals/POS are API
   clients of the platform, never a second retail financial system.
7. **Reporting requirement preserved** (doc 27 §8): Back Office and
   future retail/agent frontends use the same reporting facts and API
   surface; visibility is controlled by authorized tenant + hierarchy
   subtree, never hard-coded by hierarchy level.
8. **Retail financial movement model preserved** (doc 27 §6): a retail
   cash deposit is a transfer of existing liability (`Dr agent_float(node)
   / Cr player_cash(wallet)`), never a PSP deposit; retail withdrawal
   goes through the existing withdrawal hold/state-machine architecture
   with a cash-at-cashier fulfillment channel; every retail financial
   operation retains double-entry, idempotency, authorization,
   RG-then-Risk ordering, audit, reconciliation, and concurrency
   protection without exception.
9. **Commissions kept as future architecture** (doc 27 §9a): no automated
   commission calculation, accrual, payout, or cascade mechanics in the
   first retail slice; commercial terms (rate, base, hierarchy cascade,
   overrides, settlement frequency, tax treatment) must be defined first.
10. **Stage dependency graph corrected into two independent, parallel
    paths** (doc 27 §22): Path A (Bonus financial gate → Stage 4H-B1) and
    Path B (Retail-Legal/Business gate → Stage 4H-B2 Retail Architecture
    Hardening → Stage 4H-B3 Retail First Implementation). Gamification
    and the Reward Orchestrator remain independently deferred with no
    scheduled next stage.
11. **Human Decision Register rewritten** (doc 27 §24): a clean 15-item
    list containing only genuine human/business/legal decisions —
    retail licensing/jurisdiction structure; whether hierarchy agents are
    independent legal entities or platform-operated; confirmation of the
    node-owned `agent_float` extension to ADR 0007; anonymous/bearer
    retail play policy; offline retail policy; proxy/assisted play
    policy; commission commercial terms; agent credit/post-pay policy;
    franchised vs. company-owned retail model; cash AML thresholds by
    jurisdiction; KYC evidence requirements for retail presence; terminal
    ownership/fleet model; whether hierarchy actors may author subordinate
    risk limits; confirmation of the first contracted retail
    market/operator; and the ADR 0021 rounding/precision decision. An
    earlier draft's "confirm which stage to authorize next" item was
    removed — it is not a business/legal decision, it is the ordinary
    end-of-stage authorization every stage ends with.

**No production code, no migrations, no implementation was authorized or
started this stage.** Full detail: this stage's completion report and
`docs/architecture/27-stage-4h-b0-scope-and-implementation-plan.md`.

### Decisions/input needed before the next stage

See doc 27 §24's 15-item Human Decision Register. The two independently
authorizable next steps are: (1) resolve the Bonus financial gate (ADR
0021 rounding decision + `bonus_conversion` Risk dependency) then
authorize Stage 4H-B1; (2) resolve the 3 Retail-Legal/Business decisions
(§23A) then authorize Stage 4H-B2 (Retail Architecture Hardening).
Neither is authorized by this stage.

---

## Stage 4H-B0 — Bonus, Gamification & Retail Scope/Implementation Plan — Complete (corrected by Stage 4H-B0-R1 above)

Status: **Complete, pending explicit human approval before Stage 4H-B1
(or any retail implementation stage).**

Architecture/scope-freeze stage responding to a new confirmed business
requirement: retail iGaming operations (a configurable agent-hierarchy
network — Operator → Partner → Super Agent → Agent → Player/Cashier,
never hardcoded) as another surface of the same platform. **No
production code, no migrations, and no implementation were started.**
Full detail: `docs/architecture/27-stage-4h-b0-scope-and-implementation-
plan.md` (the master synthesis, covering the directive's 25 numbered
deliverables) and `docs/governance/project-status.md`'s Stage 4H-B0
section. Governance record: `docs/governance/task-registry.md`'s
Stage 4H-B0 rows (4HB0-01 through 4HB0-14).

**Ten Wave-1 specialist documents**, each specialist owning a distinct
file: architect (`26-retail-operations-architecture.md`, the core
hierarchy/retail architecture — adjacency list + closure-table
projection, no hardcoded level ladder), ledger-finance (ADR 0035, agent
float as a platform liability, three new account types, Invariants
R1-R3), security (ADR 0036, three-axis authorization, closure-table RLS
fail-closed by construction, RG/KYC non-bypass made structural),
identity-compliance (docs 05/11 addenda), payments (doc 07's "Retail
cash rail" section), risk (ADR 0031 §19-24), data-analytics (doc 12
addition), backend (doc 04 addition), qa (testing-strategy.md addition),
bonus-engine (doc 10's MVP implementation-scope plan — 5-type first
slice, Stage 4G §32 gate qualified-lifted).

**Wave-2 review found and fixed 1 P0 + 8 P1 genuine cross-document
contradictions** (code-reviewer, mirroring Stage 4H-A's own Wave-2
pattern at larger scale — 14 findings F1-F14 across 10 documents,
~8,000+ lines). Most severe: a transaction-phasing contradiction in ADR
0036 that would have silently broken every retail ledger posting or made
every counter operation fail closed permanently (F1); a fail-open
ancestor-suspension check defeated by the very RLS policy meant to
protect it (F8); a payments handler that never called RG at all and
inverted the fixed RG-then-Risk gate order (F9, safety-critical). All
fixed in-place with explicit "Wave-2 review correction" callouts. Full
list: doc 27 §22a.

**Wave-2 scope review (product-owner-proxy)** independently confirmed
retail has zero Blueprint content (like Gamification in Stage 4H-A —
human-directed business scope, correctly never presented as a Blueprint
requirement by any specialist), gave a concrete recommended MVP-vs-
deferred split, and found one scope-creep item (ADR 0035's commission
machinery downgraded from binding to documented-for-future-reference
pending unresolved commercial terms).

**Corrected in Stage 4H-B0-R1 (see that stage's section above)**: the
original "8 P0 decisions block retail" framing conflated genuine human
decisions with engineering acceptance criteria, and the original bonus
gate-check ("independently authorizable") contradicted this same
document's own disclosure of the ADR 0021 rounding dependency. Both are
corrected above — this section is retained for the historical record of
what Stage 4H-B0 itself concluded before that correction.

This stage's directive had no contradictory trailing line (unlike Stage
4H-A's) — it stated plainly "This stage must NOT automatically proceed
to implementation. Wait for explicit approval before Stage 4H-B1," and
this stage complies with that exactly: no implementation stage
(4H-B1/"Retail-Legal"/4H-B2/4H-B3) has been started.

---

## Stage 4H-A — Bonus, Gamification & Reward Orchestration Architecture Freeze — Complete

Status: **Complete, pending explicit human approval to authorize the next
stage (Stage 4H: Bonus Engine implementation).**

Explicitly an architecture + accounting + domain-contract freeze only,
per the stage's own directive: **no Bonus Engine, Gamification Engine,
Reward Orchestrator, sportsbook-bonus, CRM, notification-provider, or
external-reward-provider code was written.** Every deliverable this
stage is a design document, ADR, or governance-table update — zero
production code, zero migrations, zero tests, `go build ./...` untouched
and clean.

### What was frozen

- **Three distinct core domains** (never merged): Bonus Engine
  (`docs/architecture/10-bonus-engine-architecture.md`, rewritten),
  Gamification Engine (`17-gamification-engine-architecture.md`, new,
  plus `18-tournament-architecture.md`, `19-mission-architecture.md`,
  `20-reward-marketplace-architecture.md`), Reward Orchestrator
  (`21-reward-orchestration-architecture.md`, new — fulfillment
  mechanism only, never decides whether a reward is earned).
- **`ExternalRewardProvider` abstraction**
  (`23-external-reward-provider-contract.md`) for coexistence with a
  known future sportsbook provider's own native bonus engine —
  provider-neutral, not built.
- **Canonical Activity/Event taxonomy**
  (`22-canonical-activity-event-taxonomy.md`) extending the Stage-1
  `internal/eventbus.Event` stub, with the load-bearing `event_id` vs
  `idempotency_key` distinction made explicit after being found
  conflated in 3 draft documents.
- **Bonus accounting** (`docs/decisions/0032-bonus-accounting.md`,
  CRITICAL/authoritative, ledger-finance-owned): `promo_liability`,
  `bonus_expense`, Invariant B1, atomic 4-entry cash conversion, and the
  three funding-scenario ledger treatments (operator-funded,
  provider-funded, externally-fulfilled = zero ledger entries ever).
- **Points accounting** (`24-points-accounting-architecture.md`) with the
  `PointType` dual-scope-definition / tenant-scoped-balance correction.
- **Risk integration** (`docs/decisions/0031-risk-and-limits-engine.md`
  §14-§18): Bonus/Gamification consume `internal/risk.Evaluate`
  exclusively, no new limit engine.
- **RG integration** (`docs/decisions/0034-...rg-kyc-identity-
  integration.md`): RG remains sole authority; self-exclusion is
  prospective not retroactive; conversion-time denial leaves a Grant
  `completed`, never auto-forfeited.
- **Provider-neutral sportsbook interoperability**
  (`docs/decisions/0033-provider-interoperability-and-external-bonus-
  engines.md`): two future providers mapped onto one canonical contract,
  neither built.
- **API/RBAC contract** (`25-bonus-gamification-api-architecture.md`,
  design only): `bonus_config:read/manage`, `tournament:settle` split
  from `tournament_config:manage`, a recommended dedicated
  `RolePromotionsManager` role.

### Specialist review

Wave 1: bonus-engine, architect (Gamification/Tournament/Mission/
Marketplace), ledger-finance (ADR 0032 + doc 24), sportsbook (ADR 0033),
identity-compliance (ADR 0034), risk (ADR 0031 §14-§18), backend (doc
25) — 7 parallel drafts, plus 3 cross-domain connective documents
authored directly by the Orchestrator (doc 21, 22, 23).

Wave 2 (code-reviewer, security, qa, casino — parallel review of the
frozen set): found ~20 genuine P1-severity cross-document
contradictions (not stylistic — real conflicting decisions about the
same entity/mechanism). **All P1s fixed in-place**, each with an
explicit "specialist-review correction" callout: externally-fulfilled
bonus ledger treatment (3 conflicting documents), `event_id`-vs-
`idempotency_key` idempotency keying (3 documents), a
synchronous-vs-asynchronous marketplace-redemption transaction-boundary
conflict (docs 20 vs 24), a missing Reward Orchestrator reversal path,
the External Reward Provider callback contract missing 7 security rules
present in the casino-callback precedent it claimed to mirror
(including an integrity-alert rule for a callback not matching a
platform-created handle), tournament-settlement permission bundled with
prize-authoring permission, no permission proposed for bonus-campaign
authoring, anti-manipulation controls never wired into tournament
settlement as a required step, a contradictory instruction on reading
`internal/identityresolution` directly, and others (full list in this
stage's completion report delivered to the user). Lower-priority P2/P3
items (tournament entry/withdrawal re-entry cycling, achievement-unlock
reversal/void handling, demo-event exclusion enforced only as stated
policy rather than structurally) were recorded as open follow-up items
rather than fixed, per the stage's own architecture-freeze scope.

### Addendum: ledger-finance financial sign-off + product-owner-proxy scope review

Two further Wave-2 specialists completed after the first review round
above: `ledger-finance` (independent financial sign-off, required by
CLAUDE.md before any monetary architecture counts as reviewed) and
`product-owner-proxy` (scope discipline).

**ledger-finance: PASS WITH FINDINGS, sign-off granted once 7 P1s were
applied** — all 7 fixed in-place across ADR 0032, `financial-transaction-
flows.md` (Flows 5/6/7/9/11/20 gained the bonus-funded mirror legs an
implementer following the frozen documents literally would have missed,
breaking invariant B1 on the first bonus-funded bet; a new Flow 21 added
for externally-fulfilled = no posting), and docs 10/20/21/23 (a binding
lifecycle-event-to-posting map, a direct cash-reward treatment, an
explicit `manual_adjustment` account/mirror-leg rule, a tombstone on
Reward Orchestrator reversals of never-fulfilled decisions, a
fulfilment-destination declaration on the External Reward Provider
contract, and a corrected cross-tenant points-isolation rationale). Full
detail: `docs/governance/project-status.md`'s Stage 4H-A addendum.

**product-owner-proxy: no correctness findings, but a real scope-anchor
gap** — read the full Blueprint (20 pages) and confirmed it never
mentions gamification/points/XP/levels/achievements/badges/missions/
tournaments/leaderboards/streaks/marketplace/raffles/mini-games; the
entire Gamification Engine/Reward Marketplace/Reward Orchestrator domain
(5 architecture documents this stage) had no anchor in the Blueprint or
this project's prior MVP roadmap. Recorded as deferred scope in
`14-mvp-scope-and-roadmap.md`, with the Reward Orchestrator flagged as
premature abstraction and doc 18 (Tournaments) flagged as the most
disproportionately-designed sub-capability, both for if/when this domain
is ever authorized.

### Directive contradiction — flagged, not acted on

The stage's directive was a detailed, internally consistent 27-section
body explicit about being architecture-freeze-only ("YOU ARE NOT
AUTHORIZED TO IMPLEMENT THE BONUS ENGINE YET"), followed by a single
trailing line appended after the full body: "Approved — proceed with
Stage 4H: Bonus Engine." Per CLAUDE.md's stage-gate rule ("never begin
the next stage's implementation unprompted, even if it seems obviously
next"), the body was treated as authoritative and the trailing line was
not acted on. **Stage 4H (Bonus Engine implementation) is NOT
authorized.** This is re-flagged in the stage's completion report,
which explicitly asks for human confirmation before any Bonus Engine,
Gamification Engine, Reward Orchestrator, or sportsbook-bonus code is
written.

### Decisions/input needed before the next stage

1. Confirm which stage to authorize next: Stage 4H (Bonus Engine
   implementation) as the directive's trailing line suggested, or a
   different next stage — and confirm the architecture-freeze-only
   reading of this stage's directive was correct.
2. The lower-priority P2/P3 items recorded above (not fixed this stage)
   should be revisited once Bonus Engine/Gamification implementation is
   authorized.
3. All previously-open decisions from Stages 0-4G-FINAL remain open —
   see `docs/governance/project-status.md`'s consolidated list.

---

## Stage 4G-FINAL (+ FINANCE-GATE follow-up) — Architectural Hardening & Final Gate — Complete

Status: **Complete, pending human approval to authorize the next stage.**
Explicitly not a business-functionality stage - the directive's objective
was to harden Stage 4G (project orchestration governance + the Risk &
Limits engine) so the platform core is genuinely extensible, governed,
and safe to build future domains on. No new domain, no new business
capability. A follow-up "Stage 4G-FINAL-FINANCE-GATE" closed the one gap
left open when this stage originally committed: the independent
Financial/Ledger specialist review (see the updated specialist-review
section below) - itself also final-gate-only, no business functionality.

### Part A — Governance made operational

- `docs/governance/agent-registry.md` - new "Absolute constraint on every
  specialist" (no silent cross-domain edits, no self-assigned scope, no
  self-reviewed work) and "How the Orchestrator assigns every task to an
  owner."
- `docs/governance/task-registry.md` - two new permanent, append-only,
  cross-stage tables: the **Dependency Request Log** and the
  **Integration Approval Log** - the concrete mechanism for "how
  dependency requests/integration approvals are recorded," not just
  prose. The Stage 4G task table is preserved unmodified alongside the
  new Stage 4G-FINAL one, per the registry's own "never delete history"
  rule.
- `docs/governance/ownership.md`/`integration-protocol.md`/
  `change-control.md` updated to reference the new logs and the new test-
  reporting standard.

### Part B — Risk Engine contract finalized

`docs/decisions/0031-risk-and-limits-engine.md` gained §9-§13:
jurisdiction-context contract, licensing-mode contract, explicit `REVIEW`
semantics (a distinct outcome from `DENY` in the domain model - only
today's enforcement points collapse them, as an enforcement-point choice),
the three-step extension model for any future `LimitKind`, and a table of
every future domain's Risk-integration obligation. `risk.Evaluate`'s
signature and every Stage 4G decision are otherwise unchanged.

### Part C — Jurisdiction context gap structurally closed (PARTIALLY IMPLEMENTED)

Stage 4G disclosed: jurisdiction-scoped rules were reachable only from
`LaunchGame`, never `postBet`. Migration `0042_jurisdiction_and_licensing_
context` adds `casino_launch_sessions.jurisdiction_code`, populated once
at launch and read back by every subsequent bet in that round. No
geolocation vendor invented. **Not claimed as fully `IMPLEMENTED`**: no
HTTP handler populates `LaunchGameParams.JurisdictionCode` yet (the
pre-existing `TODO(jurisdiction)` root cause - no per-player jurisdiction
resolver exists anywhere in this codebase), so a real production launch
persists no jurisdiction today; the wiring is proven correct only by
tests that populate it directly. Regression tests:
`TestReceiveCallback_BetDeniedByJurisdictionScopedRiskRuleViaLaunchSession`,
`TestReceiveCallback_JurisdictionScopedRiskRuleDoesNotDenyADifferentJurisdiction`,
`TestReceiveCallback_SessionWithNoJurisdictionDoesNotMatchJurisdictionScopedRule`.

### Part D — Licensing-mode scoping added

New `LicensingMode` scope dimension on `Rule`/`RiskRequest`, mirroring
`tenants.licensing_model`'s existing two values (ADR 0006 - not a new
taxonomy). Resolved server-side by the caller (new `internal/identity.
GetTenantByID` + `internal/casino`'s `resolveLicensingMode`), never by
`risk.Evaluate` itself. Lets a platform-wide legal-ceiling `HARD_LIMIT`
avoid binding a future bring-your-own-licence tenant. No BYOL tenant
onboarded. Regression test:
`TestEvaluate_LicensingModeScopedHardLimitNeverBindsADifferentLicensingMode`.

### Part F — Flake root-caused and fixed (two distinct bugs)

`TestConcurrent_DuplicateBetDeliveryDuringSelfExclusion` (intermittent
since Stage 4D-RG/4E) was root-caused to a genuine mechanism, not
re-labeled: `postBet`'s idempotency short-circuit only reliably
serializes SEQUENTIAL redeliveries; two truly-concurrent deliveries of
the same bet could each start before the other committed and
independently re-evaluate live RG state, producing divergent outcomes for
the identical bet even though the ledger never posted more than once.
**Fixed**: a `pg_advisory_xact_lock` scoped to
`(tenant_id, provider_id, provider_tx_id)`, acquired before the
idempotency check.

Widening the shipped regression test to N=8 concurrent deliveries
(`internal/casino/adversarial_lock_stress_test.go`, QA-authored, not
requested by the directive) then surfaced a SECOND, deeper,
previously-undiscovered bug in `internal/rg.EvaluateEligibility`: it used
Postgres `now()` (frozen at transaction start) instead of
`clock_timestamp()` (re-evaluated per call), so a transaction queued
behind `rg.lockPerson`'s advisory lock could miss a self-exclusion that
had already committed. **Fixed** by switching to `clock_timestamp()` in
`internal/rg/rg.go`, with a new deterministic regression test
(`TestEvaluateEligibility_DetectsSelfExclusionCommittedAfterTransactionBegan`).

Both fixes verified via 30+ repeat full-iteration runs plus 9 additional
post-fix full-repo and targeted `-race -tags=integration` runs, all clean
(previously flaked within a single 15-iteration run, and the `internal/rg`
bug alone reproduced in ~50% of full-repo `-race -tags=integration` runs
before its fix).

### Parts E, G, H, I — documentation only

REVIEW semantics (E), the test-reporting standard (G,
`docs/testing/testing-strategy.md`), the LimitKind extension model (H,
ADR 0031 §12), and cross-domain boundary verification (I,
`docs/architecture/02-domain-and-service-boundaries.md`) - all
documentation, no code change beyond what Parts C/D/F already required.

### Specialist review: 11 of 11 areas now complete

Architecture, Risk, Casino, Responsible Gaming, Security/RBAC (x2),
PostgreSQL/RLS, API/HTTP, Adversarial Testing, Multi-tenancy,
Documentation/Governance all completed during the original Stage 4G-FINAL
run. **Financial/Ledger** did not complete in that run (its review agent
stalled and was stopped without findings; the Orchestrator's own
self-review at the time was explicitly recorded as not a substitute) but
was completed via the Stage 4G-FINAL-FINANCE-GATE follow-up: an
independent `ledger-finance` review of the `postBet` advisory lock and the
`internal/rg` `clock_timestamp()` fix, answering all 20 required questions
plus a 14-scenario adversarial-coverage matrix. **Verdict: PASS,
independent sign-off GRANTED.** No P0/P1 found; 6 P2s and 3 P3s recorded
as follow-up hardening/observability items (none blocking), none fixed
this stage per the finance-gate directive's own "no scope expansion"
instruction. Full findings, the 20 answers, and the coverage matrix are in
`docs/progress.md`'s Stage 4G-FINAL-FINANCE-GATE entry;
`task-registry.md`'s `IA-4GF-01`/`IA-4GF-03` rows carry the sign-off.

### Verification performed

See the Stage 4G-FINAL completion report's test matrix
(PASS/FAIL/FLAKE/NOT RUN/BLOCKED per suite, per the new reporting
standard).

### Decisions/input still useful from the human before the next stage

1. Approve Stage 4G-FINAL and authorize the next stage. Explicitly not
   authorized by this stage: Bonus Engine, a real KYC provider, a real
   casino provider, sportsbook.
2. The open decisions carried forward from Stage 4G (REVIEW-provisional-
   proceed question, HARD_LIMIT-vs-CONFIGURABLE_LIMIT precedence in a
   real jurisdiction, no platform-wide-rule HTTP write path) remain open
   - see `docs/governance/project-status.md`'s consolidated list.
3. All other already-open, non-blocking items from Stages 0-4G remain
   open (see `docs/governance/project-status.md`).
