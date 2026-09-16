# 27 — Stage 4H-B0: Bonus, Gamification & Retail Scope/Implementation Plan

Status: **Architecture/scope freeze — `NOT IMPLEMENTED`.** Authored directly
by the Master Orchestrator, synthesizing ten parallel Wave-1 specialist
documents (architect, ledger-finance, security, identity-compliance,
payments, risk, data-analytics, backend, qa, bonus-engine) plus a Wave-2
cross-document consistency review (code-reviewer) and scope-discipline
review (product-owner-proxy). This document does not redesign anything —
it cross-references the authoritative specialist documents below and
resolves only the genuinely cross-cutting questions (scope split,
dependency ordering, aggregated risk register, human decisions) that no
single specialist owns.

This is the directive's own required "STAGE 4H-B0 — BONUS, GAMIFICATION &
RETAIL SCOPE/IMPLEMENTATION PLAN," covering its 25 numbered deliverables.
**No production code, no migrations, and no implementation were started
this stage.**

## Source documents (all Wave-1, all `NOT IMPLEMENTED`)

| # | Document | Owner | Covers |
|---|---|---|---|
| 1 | `docs/architecture/26-retail-operations-architecture.md` | architect | Configurable hierarchy model, cashier/terminal actors, retail registration, money-movement authorization shape, ERD, domain reuse/extend/new matrix, package ownership |
| 2 | `docs/decisions/0035-retail-agent-network-accounting.md` | ledger-finance | Agent float accounting, new account types, invariants R1-R3, retail deposit/withdrawal postings, commission recognition, idempotency, reconciliation |
| 3 | `docs/decisions/0036-retail-hierarchy-rbac-and-audit.md` | security | Three-axis authorization, closure-table RLS, cashier/terminal identity, audit extension, RG/KYC non-bypass structure |
| 4 | `docs/architecture/05-identity-architecture.md` (Stage 4H-B0 addendum) | identity-compliance | Retail registration provenance, cashier identity-linkage |
| 5 | `docs/architecture/11-kyc-aml-rg-architecture.md` (Stage 4H-B0 addendum) | identity-compliance | Retail KYC tiering, cash AML thresholds, RG/self-exclusion non-bypass, offline fail-closed |
| 6 | `docs/architecture/07-payments-architecture.md` (Retail cash rail section) | payments | Retail cash as a fulfillment channel (not a `PaymentProvider`), deposit/withdrawal procedural flow, terminal-as-client boundary |
| 7 | `docs/decisions/0031-risk-and-limits-engine.md` §19-24 | risk | Retail limit integration via `internal/risk`, new scope dimensions, aggregate-exposure gap disclosure |
| 8 | `docs/architecture/12-audit-reporting-architecture.md` (Stage 4H-B0 section) | data-analytics | Hierarchy-scoped reporting, one shared pipeline for online+retail |
| 9 | `docs/architecture/04-api-architecture.md` (Stage 4H-B0 section) | backend | Retail/POS + hierarchy-management API surface |
| 10 | `docs/testing/testing-strategy.md` (Stage 4H-B0 section) | qa | Retail financial/isolation/RG-bypass test strategy |
| 11 | `docs/architecture/10-bonus-engine-architecture.md` (Stage 4H-B0 section) | bonus-engine | Bonus Engine MVP implementation-scope plan |
| 12 | `docs/architecture/02-domain-and-service-boundaries.md` (Retail/Agent Network section) | architect | Domain boundary registration |

Per this project's established precedent (ADR 0032 for financial accounting,
ADR 0036 for RBAC/RLS), where two documents disagree, the domain owner
listed above holds the veto for questions inside its own domain. Where a
disagreement crosses domains, it is resolved below or recorded as an open
decision for the Master Orchestrator/human.

---

## 1. Exact MVP scope

**Nothing in Bonus, Gamification, or Retail is authorized for
implementation this stage.** This section states what a *first slice*
would contain **if and when** each is separately authorized, so the next
stage's scope is unambiguous rather than "whatever seems obviously next."

### 1.1 Bonus Engine first slice (per bonus-engine's Stage 4H-B0 plan)

- **In**: Deposit bonus, Reload bonus, Cashback, generic Wagering bonus,
  Coupon (5 of `10-bonus-engine-architecture.md` §2's type-matrix rows).
- **Out, explicitly**: Free spins/free bets (blocked — no casino/sportsbook
  free-round interface exists), Tournament/Mission/Loyalty-reward rows
  (blocked — Gamification deferred, see 1.2), Cash reward (architecturally
  ready but deliberately not self-added to the list — a scope call for the
  Orchestrator/product owner, not bonus-engine).
- **Gate check answered directly by bonus-engine**: the Stage 4G §32
  block on `internal/bonus` is **lifted, qualified** — Risk & Limits is
  stable enough (Stage 4G-FINAL 11/11-area review plus the finance-gate
  follow-up, PASS/no P0/P1) and ADR 0031 §14-18 already specifies the
  bonus-Risk contract. Two conditions attach: first-slice Risk rules are
  `min_amount`/`max_amount` only (no `cumulative_amount`/velocity yet),
  and the `bonus_conversion` `Operation` value needs one small dependency
  request to `risk` before conversion-time checks can be wired.
- **No Reward Orchestrator this slice** — Bonus Engine fulfills split
  instructions/lifecycle events directly through `wallet`/`ledger` for all
  five in-slice types (all `into_platform_wallet`), per doc 10 §6 and the
  Stage 4H-A product-owner-proxy finding that a standalone Orchestrator is
  premature abstraction until a second concrete reward-producing domain
  exists.
- **Migration order** (no SQL written): (1) `bonus_expense` account-type
  CHECK widening; (2) `bonus_grant`/`bonus_conversion`/`bonus_forfeiture`/
  `bonus_reversal` transaction-type CHECK widening; (3) Risk's
  `bonus_conversion` `Operation` value; (4)-(7) `bonus_campaigns` →
  `bonus_offers` → `bonus_grants` → `bonus_progress`, each on its own FK
  dependency.

### 1.2 Gamification — deferred in full (unchanged from Stage 4H-A)

Re-confirmed, not re-litigated: the Blueprint has zero gamification
content (Stage 4H-A `product-owner-proxy` finding, recorded in
`14-mvp-scope-and-roadmap.md`). No Stage 4H-B0 specialist found a reason
to revisit this. Points/XP/levels/missions/tournaments/leaderboards/
marketplace/raffles remain fully deferred, architecture-frozen only.

### 1.3 Retail — first-slice recommendation

Retail's full frozen design (configurable N-level hierarchy, closure-table
RLS, three retail account types, dedicated RBAC axis, hierarchy-scoped
reporting) is the *architecture ceiling*, not the *first implementation
slice*. Per the Wave-2 product-owner-proxy scope review §below, a first
retail slice — **if and when retail is separately authorized** — should
be scoped narrowly: a fixed, shallow hierarchy depth actually contracted
with a real operator; cash deposit and cash withdrawal only (no
agent-to-agent float transfer, no commission automation); single currency;
online-only (no offline terminal tolerance); no anonymous play. Every
piece of "don't hardcode 5 levels" generality in the frozen design is
cheap to keep (it's a data-model choice, not a build-it-all mandate) and
should stay in the architecture even though the first slice exercises
only 2-3 of the configurable levels.

**Retail implementation is not authorized this stage or any adjacent one
without a separate, explicit stage.** See §22 (dependencies) and §24
(human decisions) — several P0 items (licensing, node-float/Wallet
conflict, anonymous-play policy) must be resolved by humans before any
retail implementation stage can even be scoped, let alone started.

---

## 2. Exact deferred scope

- Full Gamification Engine (points/XP/levels/missions/tournaments/
  leaderboards/marketplace/raffles/mini-games) — unchanged Stage 4H-A
  deferral.
- Reward Orchestrator as a standalone domain — deferred until a second
  concrete reward-producing domain exists (Stage 4H-A finding, reconfirmed
  by bonus-engine's Stage 4H-B0 plan).
- `ExternalRewardProvider` contract implementation — deferred until an
  actual sportsbook provider relationship and `09-sportsbook-architecture.md`
  exist (Stage 4H-A finding).
- Free spins/free bets fulfillment — deferred until `internal/casino`
  gains a free-round method and `internal/sportsbook` exists.
- **Retail implementation in its entirety** — deferred pending: (a)
  explicit human authorization that retail is in scope at all (no
  Blueprint anchor — see §3), (b) resolution of the licensing question
  (§24 #1), (c) resolution of the node-float/`Wallet` conflict (§24 #2),
  (d) resolution of the anonymous-play policy question (§24 #4), and (e)
  a separate implementation-authorization stage, per the stage-gate rule.
- Within retail's own frozen design, explicitly deferred even once retail
  itself is authorized: agent-to-agent float transfers beyond simple
  funding/settlement, commission automation (a periodic recognition
  *mechanism* is designed, but commission *terms* are commercial and
  unset), multi-currency retail counters (blocked on ADR 0021's
  unresolved conversion-clearing account), offline/store-and-forward
  terminals (a deliberate fail-closed "online required" baseline, with
  offline explicitly named as a separate human decision if ever needed),
  direct-bank agent funding/settlement (blocked on Flow 18's unresolved
  bank-rail decision — funding over an existing PSP rail works), physical
  till as a ledger account (deliberately never modeled — mirrors ADR
  0032 §6(c)'s "never mirror a balance in someone else's custody").
- Retail proxy/assisted play (an agent placing bets on a player's behalf)
  — not designed, flagged by security as a human decision with serious
  RG/KYC implications if required.
- Anonymous/bearer retail play — not designed, flagged by three
  independent specialists (architect, ledger-finance, risk) as a
  fundamental, unresolved human/business/legal decision.

---

## 3. Blueprint-anchor status (retail)

Independently confirmed by architect and ledger-finance (full-text search
of the 20-page Blueprint): **retail has zero Blueprint content.** The only
occurrences of "cashier" in the Blueprint are the online brand-frontend
deposit/withdraw screen. Retail is **human-directed business scope**,
arriving via direct instruction, exactly as Gamification was in Stage
4H-A — never to be presented as a Blueprint requirement. Recorded in
`14-mvp-scope-and-roadmap.md` alongside the Gamification entry (see the
governance-update commit for this stage).

---

## 4. Retail architecture (summary; authoritative source: doc 26)

Configurable hierarchy: **node type**, **structure** (parent/child
relation), and **capability** are three separate concerns — no fixed
Operator/Partner/SuperAgent/Agent enum anywhere in schema or code; that
chain is seed *data*. Storage: **adjacency list authoritative, closure
table as a derived, same-transaction-maintained projection** (mirrors the
ledger/balance-projection pattern already used platform-wide), rejecting
path-enumeration and a policy-time recursive CTE. Neither a Player nor a
Cashier is a hierarchy node — a Player is an attribution edge, a Cashier
is an N:M effective-dated staff assignment to a node. A cashier is an
`identity.StaffUser` with a retail role (no new actor type); a terminal is
a `service` principal (ADR 0014 option 2's first real consumer); every
money-touching retail operation requires **both** principals. Full detail,
including the 9-area conflict-check table and 3 P0/5 P1/6 P2 risks: doc 26
§§1-9.

## 5. Hierarchy/agent-network architecture

See doc 26 §1 (storage/invariants H1-H5) and ADR 0036 §2-§3 (the
authorization/RLS consumption of that storage). Precedence for
configuration rules (limits, capability grants) follows a most-specific-
wins bitmask model directly analogous to ADR 0031 §5, extended with two
new scope dimensions (`HierarchyLevel`, `HierarchyNodeID`) per risk's
ADR 0031 §20 — **explicitly no ancestor/subtree inheritance** (a rule
binds the node it's on; risk's own reasoning: inheritance would require
`matches()` to become set-membership plus a within-dimension nearest-
ancestor tiebreak a presence bitmask cannot express).

## 6. Financial model for retail funding/withdrawal

Authoritative: ADR 0035. Headline decision: **a retail cash deposit is a
transfer of an existing platform liability (`agent_float`) to the player,
not a deposit into the platform** — `Dr agent_float(node) / Cr
player_cash(wallet)`, two entries, no `psp_clearing` leg. Three new
account types (`agent_float`, `agent_commission_payable`,
`agent_commission_expense`). Invariant R1 (retail transactions never touch
PSP/bank rails), R2 (pool separation: player / agent-operational /
commission / house, with a permitted-transition matrix), R3 (shift
reconciliation against the terminal's *declared* movements, zero
tolerance). Retail withdrawal is two steps reusing
`player_withdrawal_hold` unchanged. Commission recognized by a periodic
run, not per-event. **One P0 blocking precondition, not resolved this
stage**: `ledger_accounts` today supports only wallet-scoped or
house-level ownership; a node-scoped `agent_float` account fits neither
without an additive schema change (a `hierarchy_node_id` column + a third
partial unique index + a `CHECK num_nonnulls(...) <= 1`) — this changes an
already-implemented shared table and requires architect + security +
human sign-off before any retail migration, per §24 #2 below.

## 7. RBAC/authorization model

Authoritative: ADR 0036. Three orthogonal axes: capability
(`auth.Permission`, unchanged), tenant (`app.tenant_id`, unchanged), and a
new hierarchy-scope axis (`app.hierarchy_node_id` GUC + `WithNodeScope`).
Explicitly rejected: a Role per hierarchy level (depth is
tenant-configurable, so a fixed role ladder would be a brand-specific code
path — forbidden by CLAUDE.md). RLS via a closure-table `EXISTS` predicate,
**fail-closed by construction** (no `OR guc IS NULL` escape branch — an
unset scope denies, it does not fall through to unrestricted access).
Scope is resolved from the DB per request, never a JWT claim (so
suspension/reassignment takes effect on the next request, not at token
expiry). 5 P0 findings (subtree-reassignment permission bundling,
fail-open RLS risk, retail roles holding `PermStaffManage`, a retail money
path skipping RG→Risk, writable closure/reachability data) are all
*design requirements the frozen architecture forbids*, not existing bugs
— nothing is built yet.

## 8. Reporting permission model

Authoritative: `12-audit-reporting-architecture.md`'s Stage 4H-B0 section,
consuming ADR 0036's permission shape. One shared reporting pipeline for
online + retail (new `hierarchy_node_id`/`hierarchy_node_type`/`channel`
dimensions on the existing CDC fact tables — never a parallel retail
reporting system). One shared API surface for BO and a future retail/agent
console, parameterized by the caller's own authorized subtree — never
dedicated retail endpoints (rejected explicitly, as reproducing the
"second computation of the same fact" failure CLAUDE.md's ledger-authority
rule already forbids one layer down).

## 9. Online + retail shared-domain model

From doc 26 §6's reuse/extend/new assessment: **8 of 16 assessed domains
reused as-is** (wallet, ledger core postings, RG, KYC state machine,
identity resolution, audit primitive, tenant/brand, jurisdiction
config), **7 bounded extensions** owned by their existing specialist
(identity — provenance; payments — new fulfillment channel; risk — two
new scope dimensions; audit — two nullable columns; reporting — three new
dimensions; API — new endpoint groups on the existing surface;
withdrawal state machine — a `cash_at_cashier` fulfillment-method
sibling), **1 genuinely new domain** (the hierarchy/agent-network
primitive itself, `internal/agentnetwork` — see §17). This 8:7:1 ratio is
architect's own stated test of whether "one platform, not two products"
is real, and it holds.

## 10. Bonus implementation plan

See §1.1 above; full detail in `10-bonus-engine-architecture.md`'s Stage
4H-B0 section.

## 11. Gamification implementation plan

**Not authorized, not scoped beyond "fully deferred."** No specialist
this stage found a reason to begin scoping a Gamification MVP slice — see
§1.2 and §2.

## 12. Canonical activity/event model

Unchanged from Stage 4H-A (`22-canonical-activity-event-taxonomy.md`).
Retail introduces new event-worthy facts (deposit-at-retail,
withdrawal-at-retail, float-funded, float-settled) that would extend this
taxonomy **when retail implementation is authorized** — not designed this
stage, since the taxonomy document itself was not in this wave's scope
and no specialist proposed concrete new event shapes needing it yet.

## 13. External sportsbook/provider-native bonus integration boundary

Unchanged from Stage 4H-A (ADR 0033, `23-external-reward-provider-
contract.md`). No retail interaction identified this stage.

## 14. Reward Orchestrator minimum boundary

**Not built.** Per §1.1 and the Stage 4H-A product-owner-proxy finding
(reconfirmed by bonus-engine this stage): Bonus Engine fulfills directly
through `wallet`/`ledger` for its first slice. The Reward Orchestrator
architecture frozen in Stage 4H-A (`21-reward-orchestration-
architecture.md`) remains the design-on-file for whenever a second
concrete reward-producing domain (most likely Gamification) is authorized
alongside Bonus Engine — it is not scheduled to be built in the same
stage as the Bonus Engine first slice.

## 15. Database entities and relationships required

**Bonus first slice**: `bonus_campaigns`, `bonus_offers`, `bonus_grants`,
`bonus_progress` (doc 10 §1.1's four persistent layers), plus the account/
transaction-type CHECK widenings in §1.1's migration list. No new tables
for Gamification (deferred) or Reward Orchestrator (not built).

**Retail** (architecture-level ERD only, doc 26 §5; **not migration-ready
— the P0 in §6/§24#2 must resolve first**): hierarchy node, node
type/relation/capability definitions (dual-scope, mirroring `risk_rules`/
`PointType`), node closure-table projection, player-to-node attribution
edge, cashier-to-node effective-dated assignment, terminal registration/
credential record, `retail_player_origins` (provenance — see §16's open
item), plus ledger-finance's new account types (`agent_float`,
`agent_commission_payable`, `agent_commission_expense`) and retail
transaction types. Entities deliberately absent and named as absent by
ledger-finance: a node "settlement statement" table, a cash-drawer balance
table (the till is not a ledger account, §6/ADR 0035 §6.2).

## 16. API surface required

See `04-api-architecture.md`'s Stage 4H-B0 section (backend): cashier/POS
conceptual endpoint groups (registration-at-retail, deposit/withdrawal
confirmation, shift/till open-close, balance/float inquiry) and hierarchy
management (create/view/reassign nodes, subtree views, limit/commission
*references*) on the **same API surface** as the existing Back Office,
parameterized by caller scope. Two open items requiring architect/security
sign-off before implementation: whether "Player is not a node" (backend's
assumption, matching architect's) needs architect's explicit confirmation
in doc 26 (it does — doc 26 §1 already states this independently, so this
is resolved, not open), and the player-balance-inquiry dual-auth question
(explicit `OPEN DECISION` for security/product).

## 17. Migration plan

**Bonus first slice**: see §1.1's 7-item ordered list — the only migration
sequence actually authorized to be *planned* in detail, because it's the
only domain with a lifted implementation gate this stage.

**Retail**: no migration plan is authorized to be written yet. The
P0 account-ownership-model gap (§6/§24#2) must resolve first, since it
determines the shape of the single most load-bearing new table set
(hierarchy nodes + node accounts). Once resolved, the migration order
would follow doc 26 §5's ERD dependency order (node type/relation/
capability definitions → node instances → closure-table triggers →
player/cashier/terminal assignment edges → ledger-finance's new account
types → retail transaction types), but this is not committed to this
stage.

## 18. Package/domain ownership

New governance entries (applied to `docs/governance/ownership.md` in this
stage's governance-update commit):

| Domain | Owner | Paths |
|---|---|---|
| Retail/Agent-Network architecture | architect | `docs/architecture/26-*`, cross-cutting retail ADRs |
| Retail/Agent-Network implementation (if authorized) | **OPEN DECISION** — `backend` (adequate while architecture-only) vs. a new dedicated `retail` specialist (architect's recommendation if implementation is ever authorized) | `internal/agentnetwork` (hierarchy primitive, deliberately NOT named `internal/retail` — a B2B sub-operator tree or affiliate chain is the same graph, so a retail-specific name guarantees a second consumer duplicates it), `internal/retail` (operational surface) |
| Retail financial accounting | ledger-finance | `docs/decisions/0035-*`, retail account/transaction types, `internal/ledger` extensions |
| Retail RBAC/RLS/audit | security | `docs/decisions/0036-*`, hierarchy-scope RLS mechanics |

`bonus-engine` retains ownership of `internal/bonus` (Stage 4G §32's
block now qualified-lifted per §1.1).

## 19. Test strategy

See `docs/testing/testing-strategy.md`'s Stage 4H-B0 section (qa): a
9-item retail financial testing floor mirroring ADR 0032's format,
cross-hierarchy-node isolation named as "the single most important new
isolation class this stage introduces" (weighted equal to tenant
isolation), a dedicated adversarial test for RG-bypass-through-retail
modeled directly on the Stage 4G-FINAL `clock_timestamp()` race, and
hierarchy/RBAC subtree-scoping adversarial tests including concurrent
node-reassignment races. Bonus/Gamification: ADR 0032's existing testing
floor applies unchanged to whatever bonus slice ships; Gamification test
design is explicitly deferred, matching the deferred domain.

## 20. Security/RLS strategy

See ADR 0036 in full — this is the authoritative document. Key structural
guarantee: RG/KYC non-bypass is enforced by *shape*, not policy — no
permission exists that could plausibly mean "skip RG" (`rg.EvaluateEligibility`
takes no override parameter), so a bypass would require a code change, not
a misconfiguration. 36 mandatory tests specified, several explicitly
requiring adversarial SQL rather than HTTP-level testing (an HTTP test
cannot distinguish RLS enforcement from equivalent application-code
filtering, per the exact gap ADR 0016's review closed for `sessions`).

## 21. Reconciliation strategy

Retail adds two new reconciliation streams (ADR 0035): shift/till
reconciliation (Invariant R3, zero-tolerance, comparing ledger to
terminal-declared movements — never a human cash count, which would
reintroduce a second, unverifiable truth source) and the existing
ledger-vs-projection stream extended to cover the three new retail
account types. Reporting-side reconciliation views (data-analytics'
document) read these same streams — never an independent re-derivation.

## 22. Dependencies between stages

```
Stage 4H-B1 (Bonus Engine implementation) — CAN be authorized independently
  of retail. Depends only on: this stage's frozen doc 10 scope plan +
  ADR 0032 (Stage 4H-A) + a small risk dependency request (bonus_conversion
  Operation value).

Stage "Retail-Legal" (human/business, not engineering) — MUST resolve
  before ANY retail implementation stage is even scoped:
  - Licensing status of retail per target jurisdiction (§24 #1)
  - Node-float vs. ADR 0007 Wallet conflict resolution (§24 #2) — needs
    architect + security + ledger-finance + human
  - Anonymous/bearer retail play policy (§24 #4)
  - Commercial retail relationship existing at all (no current deal)

Stage 4H-B2 (Retail architecture hardening / Wave 3, if authorized) —
  would resolve the P0/P1 items in §23 that are engineering-answerable
  (audit_log RLS extension, ledger_accounts schema extension, terminal
  credential mechanism hardening) BEFORE any retail migration is written.
  Depends on Stage "Retail-Legal" landing first — building the hardening
  stage before the legal gate is answered risks building on an
  unauthorized premise.

Stage 4H-B3 (Retail implementation, if authorized) — depends on 4H-B2.
```

Gamification and the Reward Orchestrator have no dependency edge into
either Bonus Engine's or Retail's implementation stages — they remain
independently deferred.

## 23. P0/P1/P2 risks (aggregated across all ten Wave-1 documents)

### P0 — must resolve before any retail implementation stage is authorized

1. **Licensing gap** (architect) — the platform's only current licence
   (Anjouan) is online-only; land-based retail is typically licensed
   per-jurisdiction separately. This can invalidate the commercial premise
   entirely, not just the design. **Human/legal.**
2. **Node-float vs. ADR 0007 `Wallet` conflict** (architect, ledger-finance)
   — `ledger_accounts` supports only wallet-scoped or house-level
   ownership; a node-scoped `agent_float` fits neither without an
   additive schema change that touches an already-implemented, human-
   approved shared table. **Architect + security + ledger-finance +
   human.**
3. **Anonymous/bearer retail play would make RG/KYC/Risk structurally
   unsatisfiable** (risk, independently corroborated by architect and
   ledger-finance) — every player-scoped enforcement mechanism
   presupposes an identified `player_accounts` row; no Risk/RG
   configuration can compensate for its absence. **Human/legal,
   jurisdiction by jurisdiction.**
4. **Delegated limit authoring by hierarchy actors** (risk) — no
   Partner/SuperAgent/Agent/Cashier may hold `risk_config:manage`; if "a
   SuperAgent sets its own sub-agents' limits" is a real product
   requirement, it needs a new bounded-authoring permission with an
   unexceedable hard ceiling, explicitly not risk's to add unilaterally.
   **Architect + security + Orchestrator.**
5. **`audit_log` RLS does not survive retail as designed** (security,
   finding C1) — the existing ADR 0013 policy is tenant-wide with no
   subtree guard, and Postgres's OR-of-permissive-policies semantics mean
   a narrower policy added beside it cannot narrow anything; a naive
   implementation lets any retail principal with audit-read see the whole
   tenant's trail. **Architect + a fresh security review of the actual
   migration** — this modifies an accepted Stage-2 decision.
6. **Five RBAC/RLS design invariants that must hold by construction**
   (security): node-reassignment permission must not be bundled with
   general node-manage; the RLS closure predicate must have no
   fail-open/`OR NULL` branch; no retail role may hold `PermStaffManage`;
   no retail money path may skip RG→Risk; the closure table itself must
   be write-protected as authorization data. These are binding
   requirements ON the eventual implementation, not open questions.
7. **POS idempotency namespace-squatting** (ledger-finance) — if a
   terminal's identity were taken from request payload rather than its
   authenticated credential, one terminal could replay or squat another
   terminal's idempotency keys. Closed in design by mandating
   server-side credential resolution + random operation IDs — a binding
   implementation requirement, not an open question.
8. **RG bypass via POS-offline fail-open** (identity-compliance, qa,
   ledger-finance) — an offline terminal has no valid RG/Risk decision;
   the binding design position is fail-closed (deny), and any bounded
   offline tolerance is a separate, explicit human/business decision,
   never a default.

### P1

- Daily/periodic funding limit "by hierarchy level" is **not yet
  expressible** even after risk's proposed extensions — `internal/risk`'s
  cumulative-amount aggregation is keyed on `player_account_id` and a
  float advance between two agent nodes has no player at all; per-
  transaction caps work today, per-period caps do not (risk).
- Subtree/network-wide aggregate exposure has no mechanism (risk,
  ledger-finance) — the same class of gap as Stage 4H-A's bonus-campaign-
  budget-cap finding; a Partner with 200 Agents each capped at €10k/day
  has no enforceable network-wide cap.
- Player-registration provenance mechanism disagreement between
  identity-compliance (a `registration_channel` column on
  `player_accounts`) and architect/security (a separate
  `retail_player_origins` table) — flagged for Wave-2 review, resolution
  recorded in the governance-update commit for this stage once the Wave-2
  review's finding is applied.
- Retail cash rail vs. retail ledger accounting: whether payments' and
  ledger-finance's two independently-designed idempotency mechanisms
  (a state-transition guard vs. a `(tenant_id, idempotency_key)` shape)
  describe the same mechanism or two that would conflict if both were
  implemented — flagged for Wave-2 review.
- Cashier step-up/MFA exemption named as a completion blocker if no
  compensating control is designed (security).
- Jurisdiction-derived capability restrictions vs. hierarchy-level
  precedence: doc 26's most-specific-wins model would let a tenant-scoped
  configuration row beat a legal constraint unless ADR 0031 §5's
  HARD_LIMIT/CONFIGURABLE_LIMIT distinction is explicitly applied to the
  hierarchy dimension too (security finding C16, routed to architect).
- ADR 0021's still-open bonus/rounding-direction decision blocks precise
  computation for 3 of the Bonus Engine's 5 first-slice types (deposit,
  reload, cashback all multiply money).

### P2 (representative; full lists live in each Wave-1 document)

- `governance/project-status.md`'s "Blocked stages" table is stale
  relative to `progress.md`/`active-stage.md` and this stage's bonus gate
  check — corrected in this stage's governance-update commit.
- Physical-cash multi-currency and cash-rounding are blocked on ADR
  0021's unresolved items (ledger-finance) — inherited, not re-answered.
- Retail read surfaces could leak KYC/RG state without an explicit
  mitigation (security, mirroring Stage 4H-A's F15 finding for bonus).
- Small-cell inference and 403-vs-404 enumeration risks in hierarchy-
  scoped reporting/RBAC (security, data-analytics).

---

## 24. Human business decisions still required

1. **Is retail licensed to operate at all, in which jurisdictions, under
   what structure** (own licence vs. local partner/sub-licensee)? — legal,
   blocks everything.
2. **Does a hierarchy node's float amend ADR 0007's `Wallet` definition,
   or fit inside it unchanged?** — this is a human-approved architecture
   decision (ADR 0007 itself), so amending it needs the same authority
   that approved it, not a specialist's unilateral resolution.
3. **Are hierarchy agents independent legal entities/sub-licensees, or
   platform staff?** — determines RBAC/liability/tax treatment.
4. **Is anonymous/bearer retail play permitted or required in any target
   jurisdiction?** — fundamentally different accounting object and
   compliance posture; common in some LATAM retail markets.
5. **Is any bounded offline/store-and-forward terminal tolerance
   commercially required?** — the binding default is fail-closed
   (online-required); relaxing it is a deliberate, recorded trade-off, not
   a convenience.
6. **Commission structure, base, rate, and cascade** — commercial contract
   terms; also determines whether commission is tax-withheld at source.
7. **Do agents ever hold player funds or operate on credit (post-pay
   float)?** — credit-risk and, in several jurisdictions, a legal question
   (credit-funded gambling is regulated or prohibited in some markets).
8. **Is retail proxy/assisted play (an agent placing bets on a player's
   behalf) a requirement?** — common in some retail markets, serious
   RG/KYC hazard if so; not designed this stage.
9. **Franchised vs. company-owned retail locations** — determines custody/
   insurance/AML treatment of physical cash and whether a
   `retail_cash_on_hand` account is even meaningful.
10. **Cash-specific AML reporting thresholds by jurisdiction** —
    compliance/legal, feeds a transaction-monitoring engine that doesn't
    exist yet.
11. **Does in-person presence at a retail counter satisfy any part of a
    jurisdiction's KYC evidence requirement?** — jurisdiction-configurable,
    not a platform default.
12. **Terminal fleet ownership and decommissioning process** (own hardware
    vs. third-party POS vendor) — commercial/vendor decision.
13. **Whether "a SuperAgent sets its own sub-agents' limits" is a real
    product requirement** — if yes, needs new bounded-authoring RBAC
    design (§23 P0 #4); if no, the current "no hierarchy actor holds
    `risk_config:manage`" design stands as-is.
14. Confirm which stage to authorize next: Bonus Engine implementation
    (independently ready, per §1.1/§22), a dedicated legal/licensing
    workstream for retail, both, or neither yet.

---

## 25. Recommended implementation order

```
Immediately authorizable (no blocking human decision, per bonus-engine's
own direct gate-check):
  Stage 4H-B1 — Bonus Engine implementation (5-type first slice, §1.1)

Requires a human/legal decision BEFORE any engineering work starts:
  Stage "Retail-Legal" — resolve licensing (§24 #1), node-float/Wallet
    conflict (§24 #2), anonymous-play policy (§24 #4), and confirm a
    commercial retail relationship exists at all

Only after Retail-Legal resolves:
  Stage 4H-B2 — Retail architecture hardening: close the engineering-
    answerable P0s (§23: audit_log RLS extension, ledger_accounts schema
    extension, terminal credential mechanism, RBAC invariant
    implementation) — still architecture/migration-design work, reviewed
    by architect + security + ledger-finance together, before any retail
    code is written
  Stage 4H-B3 — Retail implementation (first slice per §1.3's narrowed
    recommendation: fixed shallow hierarchy, cash deposit/withdrawal
    only, single currency, online-only, no anonymous play, no commission
    automation)

Gamification and the Reward Orchestrator: remain deferred with no
scheduled next stage, per Stage 4H-A's standing finding, unchanged by
this stage.
```

This stage (4H-B0) explicitly STOPS here per its own directive. **No
Stage 4H-B1, "Retail-Legal," 4H-B2, or 4H-B3 work has been started.**
Await explicit human authorization naming which of the above to begin.
