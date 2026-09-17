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
- **Gate check, corrected (Stage 4H-B0-R1), then closed (Stage 4H-B0-R3)**:
  an earlier draft stated Stage 4H-B1 is "independently authorizable."
  That contradicted this same document's own §23 (formerly), which
  discloses that ADR 0021's unresolved rounding/precision decision blocks
  precise computation for 3 of the 5 first-slice bonus types (deposit,
  reload, cashback all multiply money by a percentage). Stage 4H-B0-R1
  corrected this to CONDITIONALLY READY pending three items. **Stage
  4H-B0-R3 update: item 1 is now RESOLVED.** The human decided DS-1 =
  round-half-up, DS-2 = round once at the final monetary boundary (full
  precision until then, explicit function, never an implicit cast), DS-3
  = one platform-wide rule by default — validated by six specialists
  (`ledger-finance`, `bonus-engine`, `risk`, `architect`, `security`,
  `qa`) with no contradiction, financial problem, or unsafe consequence
  found. Full recorded decision, algorithm, storage location, and a
  handful of non-blocking implementation-time clarifications:
  `docs/decisions/0021-multi-asset-accounting.md`'s "Rounding and
  precision — RESOLVED" section. **Corrected status: Stage 4H-B1 is
  READY FOR HUMAN AUTHORIZATION after completion of `bonus_conversion`.**
  The Stage 4G §32 block on `internal/bonus` is **lifted, qualified** —
  Risk & Limits is stable enough (Stage 4G-FINAL 11/11-area review plus
  the finance-gate follow-up, PASS/no P0/P1) and ADR 0031 §14-18 already
  specifies the bonus-Risk contract — but production implementation
  cannot **begin** until:
  1. ~~The ADR 0021 rounding/precision decision is explicitly resolved
     by a human.~~ **RESOLVED, Stage 4H-B0-R3** — see §24 #15.
  2. **The Risk dependency for the `bonus_conversion` Operation is
     completed and reviewed.** Verified directly against repository
     state by `risk` (not inferred from prior documentation): **NOT
     STARTED — zero of ADR 0031 §16's six extension-process steps
     completed.** `bonus_conversion` exists nowhere in code today (no
     `Operation` constant, no migration CHECK value, no HTTP-allowlist
     entry, no OpenAPI enum entry — confirmed present in three places,
     not the two an earlier draft assumed — no ledger transaction-type
     mapping, no enforcement call site — `internal/bonus` does not
     exist). Full detail and exact remaining steps: ADR 0031 §16a.
     Re-verified unchanged by `risk` in both Stage 4H-B0-R2 and Stage
     4H-B0-R3 — no code has been touched since Stage 4H-B0-R1, and the
     rounding decision introduces no new Risk-side dependency (Risk
     evaluates only the already-rounded, posted minor-unit integer, by
     construction of `RiskRequest.Amount`'s `int64` type — confirmed,
     not assumed, Stage 4H-B0-R3). **This is on the first slice's
     critical path** — all five in-slice bonus types run through
     `completed → converted`, so the first slice cannot ship without it;
     it is not deferrable to a later slice. **This is now the sole
     remaining blocker to Stage 4H-B1.**
  3. **No other P0/P1 financial dependency remains.** `risk` confirmed
     directly: `bonus_conversion` is the only Risk-owned P0/P1 dependency
     for the five-type first slice. Grant issuance/activation need zero
     Risk changes (`bonus_grant` is already a real, working `Operation`);
     `min_amount`/`max_amount` rules are sufficient for all five types;
     campaign-level budget caps and RG/self-exclusion are confirmed
     out of Risk's scope by construction, not blockers. `architect`
     independently re-confirmed this with a focused 12-area review in
     both Stage 4H-B0-R2 and Stage 4H-B0-R3 — no additional P0/P1 found.
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
(human decisions) — the genuine human-decision subset of §23A (licensing,
node-float/Wallet amendment approval, anonymous-play policy) must be
resolved before any retail implementation stage can even be scoped, let
alone started.

### 1.3a First Retail Product Baseline (formal, Stage 4H-B0-R1)

The following is the recorded baseline for the **first** retail
implementation slice, **if and when** retail is authorized. These are
**implementation-scope constraints for a first slice, not claims that
every target jurisdiction permits or requires this exact model** —
anonymous play, offline operation, and proxy play in particular may be
evaluated later as separate, jurisdiction-specific product/legal
decisions (§24), and adopting a narrow first slice today does not
foreclose a broader one later once the relevant human decisions land.

- **Identified players only** — no anonymous/bearer play (§24 #4 must
  resolve "no" or be scoped out entirely for this slice).
- **Online connection required** — no offline/store-and-forward gambling
  (§23B #8's fail-closed baseline, unrelaxed).
- **Single retail currency initially** — no multi-currency retail
  counters (blocked on ADR 0021 regardless, see §1.1).
- **Cash deposit** — supported.
- **Cash withdrawal** — supported, through the existing withdrawal hold/
  state-machine architecture (§6, §8 below).
- **Fixed, shallow hierarchy for the first contracted operator** — the
  underlying schema stays fully configurable (§5); the first slice's
  *workflow* exercises only as many levels as that operator's actual
  structure needs, not the full generality.
- **Configurable hierarchy architecture retained in full** — nothing
  above narrows the frozen doc 26 data model; it narrows which parts of
  it the first slice's UI/workflow exercises.
- **No automated commissions initially** — see §9's commission note; the
  accounting *mechanism* is documented for future reference, not built.
- **No agent-to-agent float transfer initially** — only node funding/
  settlement with the node's own direct parent, per doc 26 §4.1's
  depth-1-default authorization shape.
- **No direct-bank agent settlement initially**, unless separately
  approved — funding/settlement over an existing PSP rail is in scope;
  a new direct-bank rail is not (blocked on Flow 18's own unresolved
  bank-rail decision regardless, §2).
- **No proxy/assisted play initially** — an agent placing bets on a
  player's behalf is out of scope for the first slice (§24 #6).

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
  (§24 #1), (c) resolution of the node-float/`Wallet` amendment approval
  (§24 #3),
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
`player_withdrawal_hold` unchanged, through the existing withdrawal
hold/state-machine architecture with a `cash_at_cashier` fulfillment
channel — never a parallel withdrawal mechanism. Commission recognized
by a periodic run, not per-event (and not built in the first slice — see
§9a).

**Every retail financial operation retains, without exception** (Stage
4H-B0-R1 restatement of CLAUDE.md's standing financial rules, applied
explicitly to retail): double-entry posting through the one authoritative
ledger; DB-enforced idempotency (never check-then-insert); server-side
authorization (RBAC + hierarchy scope, never client-asserted); RG
evaluation before Risk, both before commit; audit recording (actor,
tenant, entity, before/after, reason code); reconciliation against the
same ledger (never an independent re-derivation); and the same
concurrency-safety discipline (row-level locking, advisory locks where
established elsewhere in this codebase) every other financial domain
already uses.

**Agent float vs. Player Wallet — formal relationship (Stage 4H-B0-R1)**:
- **Player Wallet (ADR 0007) is completely unchanged** — it remains the
  player-owned wallet abstraction, one wallet per player per asset.
- **Agent float is explicitly NOT a Player Wallet.** It is a
  hierarchy-node-owned **operational financial account**, represented
  through the SAME authoritative double-entry ledger as every other
  account in this platform — never a second ledger, never a
  node-shaped `Wallet` row.
- **Agent float must never be confused with a physical till/cash
  drawer.** The till is never an authoritative balance (ADR 0035 §6.2,
  reconfirmed): physical cash is a **fulfillment/custody concern**, not
  an independent source of financial truth. `agent_float` is the
  ledger's own record of what the platform owes/is owed relative to that
  node; the till is what the cashier physically counts, reconciled
  *against* the ledger (Invariant R3), never the other way around.
- **All financial movements — player, agent, and house — remain in the
  one authoritative ledger.** No domain, retail included, gets its own
  parallel financial truth system (ADR 0032 §0's "there is exactly one
  financial truth system" principle, restated here for retail).

**Minimum additive schema change (drafted by ledger-finance, reviewed by
architect and security this stage — full text: ADR 0035 §1.3.1-§1.3.3)**:
`ledger_accounts` gains a nullable `hierarchy_node_id` column alongside
the existing nullable `wallet_id` and house-ownership shape, with a
`CHECK (num_nonnulls(wallet_id, hierarchy_node_id) <= 1)` plus a second
CHECK binding `account_type` to the correct owner column, ensuring
**ownership is mutually exclusive** — a row is wallet-owned, node-owned,
or house-level, never more than one, and never zero where one is
required. Reading the actual migration
(`migrations/0020_create_ledger_accounts.up.sql`) rather than the
original draft's prose, ledger-finance found and corrected a
load-bearing error: the claim that this change "changes no existing
constraint" was false — the existing house-level unique index predicate
(`wallet_id IS NULL`) would have silently collapsed every hierarchy
node's `agent_float`/asset into one shared row per tenant. The corrected
proposal widens that predicate to `wallet_id IS NULL AND
hierarchy_node_id IS NULL`.

**architect's review (§1.3.2): sound, with caveats** — the SQL was
verified byte-for-byte against the actual migration; the shape does not
redefine `Wallet` or create a second ledger; `hierarchy_nodes` (doc 26
§5.1, itself still `NOT IMPLEMENTED`) does carry `id`/`tenant_id`/
`status` as assumed, so node-account status can derive from the node's
own status; the CHECK-enumeration approach matches the platform's
existing precedent over a registry table; the `ledger-accounting-
model.md`/ADR 0032 §2 "house-level == `wallet_id IS NULL`" shorthand
needs matching edits in the same future change (flagged, not fixed —
out of this stage's scope). **security's review (§1.3.3): safe, with
implementation-time caveats** — confirmed by direct reading of the RLS
policies that a player cannot read an agent_float row under current
policy (`tenant_staff_scope` composes unchanged; `player_self_scope`
cannot match a node-owned row, and `ledger_accounts` has `FORCE ROW
LEVEL SECURITY`); confirmed the three new CHECKs are closed-form with no
OR-NULL fail-open pattern; confirmed the composite FK's MATCH SIMPLE
reasoning is sound (no cross-tenant leakage) but must ship in the same
migration as the column, since until it exists a node row's `tenant_id`
is unvalidated caller input; confirmed the widened index predicate
closes the schema-level collision, with the caveat that the Go-level
get-or-create lookup must also be updated to filter on
`hierarchy_node_id` or the bug persists functionally; confirmed the
pre-existing owner-family hole (nothing today ties `account_type` to
`wallet_id`) is real and closed by the new CHECK; found no audit-logging
gap.

**This amends the practical schema shape of a table whose broader design
traces back to a human-approved decision (ADR 0007/ADR 0019) — it
remains explicitly `NOT IMPLEMENTED`. Both specialist reviews are
commentary, not approval: human sign-off is still required before any
retail migration is written, and this does NOT redefine `Wallet` itself**
(ADR 0007 is untouched; this is an additive shape on `ledger_accounts`,
not a change to what a `Wallet` is). See §24 #3 for the exact human
decision this narrows to.

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

**Explicit requirement restated (Stage 4H-B0-R1)**: Back Office and any
future retail/agent frontend consume the **same underlying reporting
facts and the same reporting API surface** — visibility is controlled
exclusively by authorized tenant + hierarchy subtree (permissions +
scope, per ADR 0036 §2.6/§2.4), never by a second, independently-built
retail reporting calculation. **Report visibility is never hard-coded by
level** (the exact hard-coded-per-level table Wave-2 review found and
fixed in doc 12, F10) — one rule applies uniformly: every node's scope is
itself plus all its descendants. Applied to the directive's own named
roles, purely as illustrations of the one uniform rule, never as
special-cased branches in code:

| Role (illustrative node type) | What "itself plus descendants" resolves to |
|---|---|
| Operator (network root) | The full authorized network — the widest case of the same rule, not a separate "tenant-wide" branch (ADR 0036 §2.4's multi-network correction) |
| Partner | Its authorized subtree |
| Super Agent | Its authorized subtree |
| Agent | Its authorized subtree/players |
| Cashier | Operational/transaction reporting appropriate to its own granted permissions (a narrower permission set than a node-administering role, per ADR 0036 §2.7 — the scope mechanism is identical, only the permission grant differs) |

The eventual retail-facing UI naturally narrows what a given role's
console shows (a Cashier's screen likely never renders the same reports
a Partner's does) — that is a **frontend/product decision**, not a second
reporting calculation, and it is out of this architecture-freeze stage's
scope (frontend/UX work, not backend/reporting architecture).

## 9. Online + retail shared-domain model

**Formal statement (Stage 4H-B0-R1)**: online and retail are two
**operating channels/surfaces of one platform**, never two products and
never two financial systems. Concretely, retail shares, unmodified in
their core mechanics:

- **Person** — platform-wide, unchanged (ADR 0027).
- **PlayerAccount** — tenant-owned, unchanged; retail adds only an
  additive `registration_channel` column and a separate attribution
  table (`retail_player_origins`), never a parallel account model.
- **Identity Resolution** — the exact same `identityresolution.
  RegisterPlayerWithResolution` flow; retail is a different *channel*
  into the same flow, never a second registration path.
- **Wallet** — ADR 0007's player-owned wallet abstraction, completely
  unchanged; see §6 for how a hierarchy node's own account relates to it
  (it is explicitly not a Wallet).
- **Ledger** — the one authoritative `internal/ledger`; every retail
  financial movement posts through it, append-only, double-entry,
  idempotent, exactly like every other domain (§6).
- **Risk** — `internal/risk.Evaluate`, never a second limit engine (ADR
  0031 §19, restated for retail exactly as it was for Bonus/Gamification
  in Stage 4H-A).
- **Responsible Gaming** — `internal/rg.EvaluateEligibility`, called
  identically from retail as from online, never a retail-specific
  reimplementation (doc 11 §3, ADR 0036 §8).
- **KYC** — the existing tiered-trigger state machine; retail is a new
  *evidence channel* (a cashier's in-person check), never a new tier
  taxonomy.
- **Payments architecture** — retail cash is a new *fulfillment channel*
  inside the existing payments domain, not a parallel payments system
  (doc 07's "Retail cash rail" section).
- **Bonus** — unaffected this stage; a retail-originated `deposit.
  settled`/`player.registered` event triggers a deposit/reload bonus
  with zero Bonus Engine change, if/when both are live (doc 10 §5).
- **Audit** — the one `internal/audit` primitive, extended additively
  (two nullable columns), never a second audit trail.
- **Reporting** — one shared CDC/reporting pipeline, extended with three
  additive dimensions, never a parallel retail reporting system (§8).
- **Reconciliation** — retail adds new *streams* onto the existing
  reconciliation framework, never a second, independently-computed
  source of truth (§21).

**Retail terminals/POS are API clients of the platform** (§16) — they
call the platform's own APIs and display platform-returned state; they
are never a separate financial system, never a second source of
authoritative balance or decision state, and never granted direct
database access.

From doc 26 §6's reuse/extend/new assessment underlying the statement
above: **8 of 16 assessed domains reused as-is** (wallet, ledger core
postings, RG, KYC state machine, identity resolution, audit primitive,
tenant/brand, jurisdiction config), **7 bounded extensions** owned by
their existing specialist (identity — provenance; payments — new
fulfillment channel; risk — two new scope dimensions; audit — two
nullable columns; reporting — three new dimensions; API — new endpoint
groups on the existing surface; withdrawal state machine — a
`cash_at_cashier` fulfillment-method sibling), **1 genuinely new domain**
(the hierarchy/agent-network primitive itself, `internal/agentnetwork` —
see §17). This 8:7:1 ratio is architect's own stated test of whether "one
platform, not two products" is real, and it holds.

### 9a. Commissions (Stage 4H-B0-R1 restatement)

The commission-accounting **architecture** (ADR 0035 §5) remains
documented as future reference. **Automated commission calculation,
accrual, payout, or cascade mechanics are explicitly NOT built in the
first retail slice** (§1.3a). Per the Wave-2 product-owner-proxy scope
finding already applied to ADR 0035 (§5.3/§5.4 downgraded from
`RESOLVED` to `documented for future reference, not binding`), the
following commercial terms must be defined before this machinery is
frozen as binding, let alone implemented:

- Commission **rate**.
- Commission **base** (GGR, NGR, turnover, net-loss, or per-transaction —
  each implies a different ledger query and a different dispute surface).
- **Hierarchy cascade** (does an upstream node earn an override on a
  downstream node's commission, and to what depth).
- **Overrides** (exceptions to the standard rate/cascade for a specific
  node or agreement).
- **Settlement frequency** (how often commission is actually paid out).
- **Tax treatment** (is commission withheld at source in any
  jurisdiction — this can change the correct posting shape, per ADR 0035
  §5's own review finding).

See §24 #7 for the corresponding human decision register entry.

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

**Stage 4H-B0-R1 correction**: an earlier draft of this graph stated
Stage 4H-B1 was reachable directly from Stage 4H-B0, contradicting §1.1's
own disclosure that ADR 0021's rounding decision and the `risk`
dependency both remain open. Corrected below — Bonus and Retail now each
have their own explicit gate stage before their respective
implementation stage.

```
                         B0-R1 (this stage)
                            |
              +-------------+-------------+
              |                           |
              v                           v
   Bonus financial gate           Retail-Legal / Business Gate
   resolution (human +                 (human/business, not
   ledger-finance + risk):             engineering) — MUST
   - ADR 0021 rounding/precision       resolve before ANY retail
     decision — RESOLVED, Stage        implementation stage is
     4H-B0-R3 (§1.1, §24 #15)          even scoped:
   - risk dependency for the         - Licensing status of retail
     bonus_conversion Operation        per target jurisdiction
     — STILL OPEN, NOT STARTED,        (§24 #1)
     0/6 steps, re-verified          - Node-float/ADR 0007
     Stage 4H-B0-R3 (§1.1)             amendment approval (§24 #3)
   - no other P0/P1 financial        - Anonymous/bearer retail
     dependency remaining              play policy (§24 #4)
     — re-confirmed Stage             - Commercial retail
     4H-B0-R2 and 4H-B0-R3              relationship existing at
              |                          all (no current deal)
              v                                |
     Stage 4H-B1 — Bonus                       v
     Engine implementation             Stage 4H-B2 — Retail
     (5-type first slice,              Architecture Hardening:
     §1.1) — READY FOR HUMAN
     AUTHORIZATION once
     bonus_conversion lands
                                        resolve the engineering-
                                        answerable P0s (§23B: audit_log
                                        RLS extension, the drafted
                                        ledger_accounts schema
                                        amendment's final ratification,
                                        terminal credential mechanism)
                                        BEFORE any retail migration is
                                        written — reviewed by architect
                                        + security + ledger-finance
                                        together, still architecture/
                                        migration-design work, no
                                        retail code yet
                                                |
                                                v
                                        Stage 4H-B3 — Retail First
                                        Implementation (per §1.3a's
                                        formal baseline: identified
                                        players only, online-required,
                                        single currency, cash deposit +
                                        withdrawal, fixed shallow
                                        hierarchy for the first
                                        contracted operator, no
                                        automated commissions, no
                                        agent-to-agent transfer, no
                                        direct-bank settlement unless
                                        separately approved, no proxy
                                        play)

Gamification — remains deferred, no scheduled next stage (unchanged from
Stage 4H-A).

Reward Orchestrator — remains deferred until a second concrete
reward-producing domain exists (unchanged from Stage 4H-A and this
stage's own bonus-engine finding).
```

The Bonus gate and the Retail-Legal gate are **independent of each
other** — resolving one does not require or block the other. Both
depend only on B0-R1 (this stage) having landed. Gamification and the
Reward Orchestrator have no dependency edge into either path — they
remain independently deferred.

## 22a. Wave-2 cross-document consistency review (code-reviewer) and scope review (product-owner-proxy)

After all ten Wave-1 documents landed, two Wave-2 reviews ran: a
cross-document consistency review (mirroring Stage 4H-A's own Wave-2
review, which found ~20 genuine cross-document contradictions in that
stage's parallel-authored set) and a scope-discipline review.

**code-reviewer found 14 findings (F1-F14): 1 P0, 8 P1, 4 P2, 1
consolidated P3 list.** All P0/P1 findings were fixed in-place, each with
an explicit "Wave-2 review correction" callout in the affected document
(this project's established practice — never a silent edit):

- **F1 (P0)**: ADR 0036's retail transaction design required
  `app.hierarchy_node_id` set for the whole handler (§2.5) while also
  requiring it unset for the posting engine, dual-scope config reads, and
  set again for the audit insert — an internal contradiction that would
  have manufactured P1 drift by construction (the exact failure ADR 0035
  §1.3 warned about) or made every counter operation fail closed
  permanently. Fixed by adding an explicit three-phase transaction model
  (§2.5a: authorization phase scoped, posting phase unscoped, audit
  phase re-scoped or parameterized) to ADR 0036.
- **F2 (P1)**: payments' and backend's documents required only the
  cashier's own principal for a counter operation, contradicting doc
  26/ADR 0036's two-principal (terminal + cashier) requirement — and
  silently defeating ADR 0035's terminal-identity-based idempotency
  namespace-squatting mitigation, which depends on a terminal credential
  existing. Fixed across both documents.
- **F3 (P1)**: two incompatible deposit/withdrawal flow shapes (ADR
  0035/doc 26 assumed counter-originated; payments/backend assumed a
  player-pre-request flow only) and three different idempotency-key
  encodings across four documents. Resolved: counter-originated is
  primary, the player-pre-request flow is an optional second entry
  point converging on the same posting/key; ADR 0035's `(tenant_id,
  idempotency_key)` encoding is authoritative (ledger-finance holds the
  financial veto), corrected in docs 04/12/26.
- **F4 (P1)**: four different `risk.Operation` name sets proposed
  independently across ADR 0031/ADR 0035/doc 26/payments, plus an
  outbound agent-settlement money path with no `Operation` gating it at
  all. Resolved: `retail_deposit`/`retail_withdrawal`/`retail_funding`
  is authoritative (risk owns naming), corrected in all four documents;
  the settlement-path gap recorded as unresolved (§23 P1 below).
- **F5 (P1)**: risk's `hierarchy_level` scope dimension contradicted
  architect's explicit rejection of any level/ladder concept. Renamed to
  `hierarchy_node_type` with a tenant-required CHECK constraint added
  (a platform-wide rule cannot reference a tenant-authored type code).
- **F6 (P1)**: backend's API document contradicted ADR 0036 in three
  load-bearing ways (claimed no second RLS dimension exists; proposed a
  `hierarchy_node_id` JWT claim ADR 0036 explicitly rejects; proposed a
  `staff_users` column ADR 0036 explicitly rejects in favor of an N:M
  assignment table). All three corrected to match ADR 0036.
- **F7 (P1)**: player-registration provenance was a three-way
  disagreement (identity-compliance: audit metadata only, no new table;
  architect/security: a dedicated `retail_player_origins` table; backend:
  a third, different FK). Resolved: both `retail_player_origins`
  (attribution source of record) and `registration_channel` (identity-
  compliance's own column) are correct and additive to each other;
  backend's third mechanism withdrawn.
- **F8 (P1)**: ADR 0036's own ancestor-suspension check (a Partner
  suspension must cascade to its cashiers) was silently defeated by ADR
  0036's own closure-table RLS policy, which made the check's query
  return zero rows regardless of real data — fail-open, not fail-closed.
  Fixed with a `SECURITY DEFINER` accessor design that bypasses the
  closure-table policy for this specific, audited check.
- **F9 (P1, safety-critical)**: payments' retail deposit handler never
  called `rg.EvaluateEligibility` at all and ran the Risk check before
  anything else, contradicting the fixed RG-first-then-Risk order every
  sibling document states as binding. Fixed.
- P2s F10-F13 (reporting's hard-coded per-level scope ladder contradicting
  the uniform descendant-scope rule; identity-compliance's cross-tenant
  premise invalidated by architect's own topology decision; the anonymous-
  play assumption stated silently rather than explicitly in two documents;
  offline-retail floating a mechanism shape other documents pre-reject)
  — all fixed. F14's consolidated one-line drift items (stale ADR 0031
  section citations, `scope_source` naming drift, column-name drift,
  a now-resolved qa `OPEN DECISION`, a stale StaffRole hedge) — the
  safety/correctness-relevant ones fixed; a handful of pure cosmetic
  items (shift/session table naming across three documents:
  `retail_terminal_sessions`/`retail_shift`/`/v1/retail/shifts`; backend's
  float-inquiry endpoint conflating the ledger-side `agent_float`
  projection with the till, which ADR 0035 explicitly is not a ledger
  account; a stale "does not exist yet" note in bonus doc 10 §5) recorded
  here rather than fixed, to avoid further scope expansion in an
  architecture-freeze stage — these are naming/wording inconsistencies
  with no correctness impact, to be reconciled when retail implementation
  is actually scoped.

**product-owner-proxy independently confirmed the Blueprint-anchor
finding** (zero retail content in the Blueprint, exactly like
Gamification in Stage 4H-A) and gave a concrete recommended MVP-vs-
deferred split (§1.3/§2 above already incorporate it), plus one scope-
creep finding: **ADR 0035 §5's commission-accounting machinery
(periodic-run mechanism, accrual→payable state machine, hierarchical
override cascades) is more fully designed than the unresolved commercial
terms (rate, base) justify** — the boundary-setting invariants (pool
separation, commission is computable not a stored balance) are cheap and
worth keeping frozen; the override-cascade posting mechanics should be
treated as "documented for future reference," not binding, until the
commercial terms are set, since those terms may change the correct
posting shape anyway.

## 23. P0/P1/P2 risks (aggregated across all ten Wave-1 documents)

**Stage 4H-B0-R1 correction**: an earlier draft of this section listed 8
items under a single undifferentiated "P0" heading, worded as if all 8
were open human decisions blocking retail. That conflated two entirely
different kinds of thing: a small number of genuine human/business/legal
decisions nobody but a human can make, and a larger number of mandatory
engineering acceptance criteria that this stage's own specialist
documents (chiefly ADR 0036) already state as binding design
requirements — not open questions, and not something a human needs to
weigh in on. Corrected below into two separate lists per the human
directive's explicit instruction. The severity label "P0" is kept for
both — both block retail going live — but only 23A requires a human/
business decision; 23B requires nothing from a human except confirming
these are indeed non-negotiable, and requires an implementer/reviewer to
verify they hold, exactly like any other acceptance test.

### 23A. P0 — genuine human/business/legal decisions

These cannot be resolved by any specialist or by the Orchestrator. See
§24 for the full, deduplicated human decision register — the items below
are the subset that specifically blocks any retail implementation stage
from being scoped at all (§24 carries the complete list, including lower-
urgency items that don't block scoping).

1. **Licensing gap** (architect) — the platform's only current licence
   (Anjouan) is online-only; land-based retail is typically licensed
   per-jurisdiction separately. This can invalidate the commercial premise
   entirely, not just the design. **Human/legal.** (= §24 #1)
2. **Node-float vs. ADR 0007 `Wallet` conflict — confirmation, not design**
   (architect, ledger-finance, security jointly propose; human approves)
   — `ledger_accounts` supports only wallet-scoped or house-level
   ownership today; §6 below now carries the specific additive-schema
   amendment ledger-finance has drafted and architect/security are
   reviewing (Stage 4H-B0-R1). What remains a **human** decision is
   narrower than an earlier draft implied: not "how should this be
   designed" (that's now drafted and under specialist review) but
   **"is this specific amendment to a human-approved decision (ADR 0007)
   approved?"** — the same authority that approved ADR 0007 approves or
   rejects amending its practical schema shape. (= §24 #3)
3. **Anonymous/bearer retail play would make RG/KYC/Risk structurally
   unsatisfiable** (risk, independently corroborated by architect and
   ledger-finance) — every player-scoped enforcement mechanism
   presupposes an identified `player_accounts` row; no Risk/RG
   configuration can compensate for its absence. **Human/legal,
   jurisdiction by jurisdiction.** (= §24 #4)

Item 4 from the earlier draft ("delegated limit authoring by hierarchy
actors") is **reclassified below, into 23B**, per the human directive:
it is a mandatory engineering acceptance criterion (no hierarchy actor
may hold `risk_config:manage`) *unless and until* a human confirms
delegated authoring is a real product requirement — at which point it
becomes design work for architect+security, not a standing human
decision that blocks anything today. The current binding default (no
delegation) requires no human input to remain in force; a human is only
needed if someone wants to change it. See §24 #13 for the conditional
framing.

### 23B. P0 — mandatory engineering acceptance criteria (NOT human decisions)

These are binding requirements ON any future implementation, already
stated as such by the owning specialist's own document (chiefly ADR
0036). No human sign-off is needed to adopt them — they are not choices,
they are the frozen architecture's non-negotiable floor. An
implementation that violates any of these is non-conformant, full stop;
the acceptance test is "does the code do this," not "should the business
want this." Human input is genuinely required only where noted below
(none is, at present).

1. **Fail-closed hierarchy RLS.** The closure-table RLS predicate (ADR
   0036 §3.3) must deny, never fall through to unrestricted access, when
   the hierarchy-scope GUC is unset.
2. **No `OR <guc> IS NULL` RLS escape branch.** Any policy shape that
   would grant broader access when the scope GUC happens to be unset is
   non-conformant by construction (ADR 0036 §3.3, and the specific
   phasing fix in §2.5a for the transaction-scoping contradiction Wave-2
   review found, F1).
3. **No retail role may hold `PermStaffManage`.** Structurally prevents a
   retail-domain permission grant from escalating into general staff
   administration (ADR 0036 §2.7/§11).
4. **RG → Risk ordering, unconditionally, inside the posting transaction.**
   Every money-touching retail operation calls `rg.EvaluateEligibility`
   before `risk.Evaluate`, both before the posting commits — no retail
   code path may skip, reorder, or locally reimplement either check (ADR
   0036 §8.1, ADR 0035 §3.2, ADR 0031 §22; this is also exactly the class
   of bug Wave-2 review found and fixed in payments' own document, F9).
5. **Closure-table write protection.** The `hierarchy_node_closure`
   table is authorization data — anyone who can write a closure row
   grants themselves ancestry, so it must be trigger-maintained only,
   with a DML guard trigger preventing direct writes (ADR 0036 §3.3).
6. **Server-side terminal-credential resolution.** A terminal's identity
   for idempotency/audit purposes is resolved from its own authenticated
   credential — never taken from request payload, which is exactly the
   POS idempotency namespace-squatting attack (see item 7 below and ADR
   0035 §8.2).
7. **POS idempotency namespace protection.** The `(tenant_id,
   idempotency_key)` shape (ADR 0035 §8.1) plus server-side credential
   resolution (item 6) together close a genuinely new attack class: one
   terminal replaying or squatting another terminal's idempotency keys.
   Binding implementation requirement, not an open question.
8. **Offline fail-closed baseline.** An offline/degraded-connectivity
   terminal has no valid RG/Risk decision and must therefore deny, never
   default-allow (ADR 0031 §19/§24, ADR 0035 §8.4, doc 11 §4). This is
   the **binding default** — a human decision is only needed if someone
   wants a *bounded exception* to it (§24 #5), not to adopt the default
   itself.

Two items from the earlier draft's undifferentiated P0 list are also
engineering acceptance criteria, not decisions, and are retained as such
(not renumbered above to avoid disturbing existing cross-references, but
reclassified in substance):

9. **`audit_log` RLS extension** (security, finding C1) — the existing
   ADR 0013 policy is tenant-wide with no subtree guard; a retail
   principal with audit-read would see the whole tenant's trail unless
   this is fixed. This is engineering work (a migration + a fresh
   security review of that migration, since it touches an accepted
   Stage-2 decision) — no human business/legal input is needed to know
   this must be fixed before retail audit reads are meaningful.
10. **Delegated limit-authoring self-defeat prevention** — see the note
    under 23A above: the *default* (no hierarchy actor holds
    `risk_config:manage`) is a binding engineering criterion requiring
    no human sign-off; only a *change* to that default needs one.

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
- **`agent_settlement`/`agent_commission_payout` — an outbound real-money
  payment to an agent node — has no `risk.Operation` gating it at all**
  under the now-authoritative `retail_deposit`/`retail_withdrawal`/
  `retail_funding` set (Wave-2 review finding F4). ADR 0035 §11.3 itself
  notes this money path also has no approval state machine designed.
  `risk` + `ledger-finance` must jointly decide whether it needs a fourth
  `Operation` or is gated entirely by an approval workflow that does not
  yet exist — not resolved this stage.
- Nine more P1-severity cross-document contradictions (F2, F3, F5, F6,
  F7, F8, F9, plus two more) were found and fixed in-place by the Wave-2
  cross-document review — see §22a below for the complete list and
  resolutions. Listed here only for risks that remain genuinely open
  after fixing; the contradictions themselves are closed, not open
  risks.
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

## 24. Human Decision Register

**Stage 4H-B0-R1**: this register contains **only** decisions that
genuinely require human/business/legal input — no engineering acceptance
criterion appears here (those are §23B, binding regardless of human
input). Kept deliberately concise; each item states what's being decided
and why it can't be resolved by a specialist or the Orchestrator.

1. **Retail licensing/jurisdiction structure.** Is retail licensed to
   operate at all, in which jurisdictions, under what structure (own
   licence vs. local partner/sub-licensee)? The platform's only current
   licence (Anjouan) is online-only. Legal; blocks everything else in
   this register from mattering if the answer is "not licensable."
2. **Are hierarchy agents independent legal entities/sub-licensees, or
   platform/company-operated entities?** Determines RBAC/liability/tax
   treatment.
3. **Confirmation of the proposed node-owned `agent_float` extension to
   ADR 0007.** The technical amendment is now drafted (ledger-finance)
   and under architect + security review (§6) — what remains is
   confirming this specific amendment to a human-approved decision is
   approved, not designing it (that part is done).
4. **Anonymous/bearer retail play policy, by jurisdiction.** Would make
   RG/KYC/Risk enforcement structurally unsatisfiable as currently
   designed if permitted anywhere (§23A #3). Common in some LATAM retail
   markets.
5. **Offline retail policy.** Is any bounded offline/store-and-forward
   terminal tolerance commercially required? The binding default is
   fail-closed (online-required, §23B #8); relaxing it is a deliberate,
   recorded trade-off with a hard per-terminal exposure cap, never a
   convenience default.
6. **Proxy/assisted play policy.** Is an agent placing bets on a
   player's behalf a requirement? Common in some retail markets, serious
   RG/KYC hazard if so; not designed this stage.
7. **Commission commercial terms**: rate, base (GGR/NGR/turnover/net-loss/
   per-transaction), hierarchy cascade, overrides, settlement frequency,
   tax treatment (§9a). Also determines whether commission is
   tax-withheld at source.
8. **Agent credit/post-pay policy.** Do agents ever hold player funds or
   operate on credit (post-pay float)? Credit-risk and, in several
   jurisdictions, a legal question (credit-funded gambling is regulated
   or prohibited in some markets).
9. **Franchised vs. company-owned retail model.** Determines custody/
   insurance/AML treatment of physical cash and whether a
   `retail_cash_on_hand` account is even meaningful.
10. **Cash-specific AML reporting thresholds by jurisdiction.**
    Compliance/legal; feeds a transaction-monitoring engine that doesn't
    exist yet.
11. **KYC evidence requirements for retail presence.** Does in-person
    presence at a retail counter satisfy any part of a jurisdiction's
    KYC evidence requirement? Jurisdiction-configurable, not a platform
    default.
12. **Terminal ownership/fleet model.** Own hardware vs. third-party POS
    vendor, and the decommissioning process. Commercial/vendor decision.
13. **Whether hierarchy actors may author subordinate Risk limits.**
    The binding default (§23B #10) is no — no Partner/SuperAgent/Agent/
    Cashier holds `risk_config:manage`, and this default requires no
    human input to remain in force. A human decision is needed **only
    if** "a SuperAgent sets its own sub-agents' limits" is a genuine
    product requirement; if so, it becomes a bounded-authoring RBAC
    design task for architect + security, not a standing block.
14. **Confirmation of the first contracted retail market/operator, when
    known.** Feeds §1.3a's "fixed shallow hierarchy for the first
    contracted operator" baseline — the actual shape (how many levels,
    what they're called) is this operator's real structure, not a
    platform default.
15. **ADR 0021 rounding/precision decision — RESOLVED (Stage 4H-B0-R3).**
    Previously blocked precise computation for 3 of the Bonus Engine's 5
    first-slice bonus types (deposit, reload, cashback). The human
    decided DS-1 = round-half-up, DS-2 = round once at the final
    monetary boundary, DS-3 = one platform-wide rule by default; six
    specialists (`ledger-finance`, `bonus-engine`, `risk`, `architect`,
    `security`, `qa`) validated the decision against the existing
    architecture and found it safe, deterministic, and reconciliation-
    compatible, with no contradiction requiring a change to the human's
    proposal. Full recorded decision: `docs/decisions/0021-multi-asset-
    accounting.md`'s "Rounding and precision — RESOLVED" section. Retained
    here, marked resolved rather than removed, since this register is an
    append-only historical record of every decision requiring human
    input, not only the still-open ones.

Two items from an earlier draft of this register are corrected/removed:
"does a hierarchy node's float amend ADR 0007" is narrowed to item 3
above (the design is now drafted, only approval remains); "confirm which
stage to authorize next" is removed from this register entirely — it is
not a business/legal decision, it is the ordinary end-of-stage
authorization request every stage in this project ends with (see the
completion report).

---

## 25. Recommended implementation order

**Stage 4H-B0-R1 correction**: superseded by §22's corrected dependency
graph, which this section now matches exactly rather than restating
inconsistently. See §22 for the diagram; summarized here for quick
reference:

```
Path A — Bonus:
  Resolve the Bonus financial gate (§1.1: ADR 0021 rounding decision —
    RESOLVED, Stage 4H-B0-R3, per §24 #15; the bonus_conversion Risk
    dependency — STILL OPEN, NOT STARTED per ADR 0031 §16a, re-verified
    Stage 4H-B0-R3; no other P0/P1 remains, re-confirmed)
  → THEN Stage 4H-B1 — Bonus Engine implementation (5-type first slice)
  READY FOR HUMAN AUTHORIZATION after completion of bonus_conversion,
  per §1.1's Stage 4H-B0-R3 update.

Path B — Retail (independent of Path A):
  Resolve the Retail-Legal / Business Gate (human/legal, before ANY
    engineering work starts): licensing (§24 #1), node-float/ADR 0007
    amendment approval (§24 #3), anonymous-play policy (§24 #4), confirm
    a commercial retail relationship exists at all
  → THEN Stage 4H-B2 — Retail Architecture Hardening: close the
    engineering-answerable P0s (§23B: audit_log RLS extension, final
    ratification of the drafted ledger_accounts schema amendment,
    terminal credential mechanism) — still architecture/migration-design
    work, reviewed by architect + security + ledger-finance together,
    before any retail code is written
  → THEN Stage 4H-B3 — Retail First Implementation (per §1.3a's formal
    baseline)

Gamification and the Reward Orchestrator: remain deferred with no
scheduled next stage, per Stage 4H-A's standing finding, unchanged by
this stage.
```

This stage (4H-B0-R1) explicitly STOPS here. **No
Stage 4H-B1, "Retail-Legal," 4H-B2, or 4H-B3 work has been started.**
Await explicit human authorization naming which of the above to begin.

---

## 26. Extensible Asset/Currency Registry + FX/Conversion architecture (Stage 4H-B0-R3, analysis only)

New confirmed product requirement, analyzed this stage by `architect` at
the Master Orchestrator's direction: the platform must not hard-code a
closed list of currencies/assets — an extensible Asset/Currency Registry
must let authorized operators create/activate assets as needed (EUR,
USD, GBP, MXN, BRL, ARS, BTC, USDT and future/custom assets are
illustrative, not exhaustive). **Analysis only. Not implemented. Does
not expand the Bonus MVP, does not trigger FX implementation, does not
add a payment/custody provider, does not add retail implementation** —
confirmed explicitly by `bonus-engine` and `qa` as having zero effect on
the five-type first slice's scope or test plan.

### 26.1 Current architecture — partial support, additive extension needed

The `assets` table (migrations 0003/0006) is already generic in spirit:
an open `TEXT` primary key with no closed enum, per-row `decimal_
exponent` (0-18) that every consumer already looks up rather than
assuming a fixed value, and no code path anywhere in `internal/` that
hardcodes a decimal count (confirmed by `qa`/`architect` independently).
A new asset row could be inserted today with zero schema change and
nothing downstream would break. **What is missing is the entire
operational surface the requirement actually needs**: no admin API, no
RBAC/authorization model, no audit logging on the mutation, and none of
the additional per-asset eligibility columns the requirement names
(wallet/deposit/withdrawal/settlement eligibility — currently expressed
only indirectly, and only per-provider, through `ProviderCapability`,
which is a different concept). Tenant/jurisdiction availability is
**already solved** by the existing `tenant_jurisdiction_configs.allowed_
currencies` mechanism — the Registry design does not need to reinvent
that dimension.

**Conclusion: not "already satisfied" — an explicit, additive extension
is required**, not a redesign of the storage shape.

### 26.2 Recommended documentation changes (not drafted this stage)

- **A new ADR** (recommended: ADR 0037, parallel to how payments/KYC/risk
  each got their own ADR rather than being folded into ADR 0021) to
  define the extended Registry field set, the FX Rate Provider interface
  (no vendor named), the Conversion Service boundary, and an explicit
  cross-reference to ADR 0021's existing `ConversionOperation` as the
  ledger-side terminus — not a redefinition of it.
- `docs/architecture/financial-domain-model.md`'s scoping table (the
  `Asset | Platform (registry) | Unchanged from Stage 1` row) needs a
  pointer to the new ADR once written, and its framing corrected from
  "Stage-1 seed set" to "open, extensible platform registry" so a future
  reader doesn't mistake the 7 currently-seeded rows for a closed set.
- `docs/architecture/06-wallet-ledger-architecture.md`'s already-
  superseded cross-currency sketch needs a forward pointer to the new
  ADR for the Registry/FX-provider boundary specifically.
- `docs/architecture/13-dependency-map-and-risk-register.md` and
  `docs/architecture/14-mvp-scope-and-roadmap.md` should record the
  deferred future stage (§26.3) so it is not lost — done this stage, see
  the "Features deliberately deferred" section of doc 14.

None of the above ADR/document content was drafted this stage — only
the requirement and the plan to draft it later were recorded, per this
stage's explicit "analysis only" scope.

### 26.3 Deferred future stage

A dedicated future Asset/FX implementation stage should be recorded as
deferred (not numbered/sequenced now), covering: the Registry admin API
+ RBAC, the FX Rate Provider interface + a mock/sandbox implementation,
the Conversion Service, and the conversion-clearing-account open
decision ADR 0021 already parks (see 26.4). Recorded in doc 14's
deferred-features list this stage.

### 26.4 FX/Conversion boundary — four distinct components, never coupled

Per the human directive's explicit requirement, the design (analysis
only) keeps four things separate: (1) the Asset/Currency Registry (what
assets exist and their properties), (2) an FX Rate Provider interface
(external market-data source, no vendor named), (3) a Conversion
Service (applies a rate to produce a conversion, sitting between the
Rate Provider and the Ledger), (4) the Ledger transaction itself (ADR
0021's existing `ConversionOperation`). A live FX provider response must
never be the sole historical source of truth for a completed conversion
— the following must be persisted at conversion time: source asset,
destination asset, source amount, destination amount, exchange rate
used, rate timestamp, provider/source identifier, the provider's own
rate/reference ID if available, a conversion operation ID, the
precision/exponent used, the rounding policy/version applied (tying back
to this stage's resolved ADR 0021 rounding decision), and placeholder
fields for fees/spread if introduced later. Most of these map directly
onto fields ADR 0021's `ConversionOperation` already reserves
(`exchange_rate`, `rate_source`, `rate_timestamp`, `fee_amount`/
`fee_asset_code`/`spread`, `provider_reference`); the new ADR needs to
make the rounding-rule-version field and the provider's own reference ID
(distinct from the platform's internal `provider_reference`) explicit.

**Genuine pre-existing blocker, not new**: the conversion-clearing
account type does not exist in the ledger's account-type list and
remains an `OPEN DECISION` (ADR 0021, `ledger-accounting-model.md` §2).
No FX conversion can post to the ledger until that is resolved,
independent of how the Registry/FX-provider boundary is designed — the
Registry work makes this visible sooner but does not itself unblock it.

### 26.5 Rounding + conversion interaction — no hidden double-rounding

Confirmed, not assumed: Bonus-amount rounding, FX-conversion rounding,
and ledger minor-unit normalization are **three separate financial
boundaries**, even though ADR 0021 item 4 requires them to share **one
implementation** (the shared rounding helper). Because ADR 0021 scoped
Q1/Q2/Q3 as one platform-wide decision from the start (its own
dependent-set table lists FX conversion alongside bonus), this stage's
resolution of DS-1/DS-2 for Bonus **does** also set the FX-conversion
rounding rule, by design. What does **not** follow automatically: the
future Conversion Service must still explicitly invoke the shared
rounding helper with the *destination asset's* exponent as its own
deliberate step — it must never inherit "already rounded" from an
upstream, unrelated computation. A value must never be rounded once for
a bonus-grant purpose and rounded again for an unrelated FX-conversion
purpose on the same figure without both roundings being separately
recorded, deliberate steps.

### 26.6 Custom/future asset handling

An asset without a valid, approved conversion-rate source must never be
presented as having a real exchange rate. A conversion requiring a
market rate must **fail closed** when no valid rate exists — never
substitute, infer, or peg a rate from an unrelated asset without an
explicit, separately-approved conversion path. A custom/internal asset
with no external market at all needs its own explicit conversion path
(or none) rather than a fallback/default rate.

### 26.7 Impact assessment

- **Bonus Stage 4H-B1**: none. Bonus computations operate on wallets
  already denominated in already-registered assets; nothing about
  bonus grant/wagering/conversion math depends on whether the asset
  list is open or closed, or on whether an FX boundary exists.
- **Retail (ADR 0035)**: no new blocker. ADR 0035 §9.4 already states
  multi-currency retail counters are `BLOCKED` on the same conversion-
  clearing-account decision; Registry extensibility makes it easier to
  register a new single-currency counter's asset but does not unblock
  cross-currency retail, which was never in this analysis's scope.
- **Dependencies**: the Registry itself has no ledger dependency (a
  lookup table); the FX/Conversion boundary depends entirely on the
  existing `ConversionOperation` design and inherits its currently-open
  clearing-account blocker.

### 26.8 New risks flagged (not resolved this stage)

- **P1** — asset-creation/activation authorization boundary is
  undefined: `assets` is platform-wide with no RLS, so a poorly-scoped
  "authorized operator" grant could let one tenant's actor add a row
  every tenant's ledger then references. `security` recommends the
  eventual design evaluate a two-tier split (platform-admin-only
  registration vs. tenant-scoped activation) before any Registry API is
  built — the same shape of gap ADR 0031 §8 already discloses for
  platform-wide risk rules, with larger blast radius here. Mandatory
  audit logging on this mutation, no exception (CLAUDE.md).
- **P1** — fail-closed FX behavior (§26.6) should be written into the
  new ADR as a binding rule now, so it isn't improvised at
  implementation time.
- **P2** — documentation drift risk until the new ADR exists (addressed
  this stage via the doc 14 deferred-stage entry and this section).
- **P2** — no test today exercises "insert a new asset row and confirm
  every consuming code path picks up its exponent/type with no hardcoded
  assumption" — recorded for `qa`'s backlog once a Registry API exists.

**No code, no migrations, no ADR 0037, and no Registry/FX implementation
were created this stage.** This section records the confirmed
requirement and the analysis needed to scope a future stage — nothing
more.
