# 29 — Bonus Engine Master Implementation Contract (Stage 4H-B1, Wave 1)

Status: **DESIGN/CONTRACT ONLY — `NOT IMPLEMENTED`.** No Go code, no
schema, no migration, no route registration is authorized by this
document. Owned by `architect` per `docs/governance/ownership.md`
("Cross-domain architecture | architect | `docs/architecture/*`").

Numbering note: the stage directive suggested `15-…`; `15` is taken
(`15-jurisdiction-and-licensing-model.md`) and `docs/architecture/` is
populated through `28`. This document takes the next free number, **29**.

## 0. What this document is, and what it is not

Stage 4H-B1 Wave 1 dispatched eight specialists in parallel
(`architect`, `bonus-engine`, `ledger-finance`, `risk`,
`identity-compliance`, `sportsbook`, `casino`, `security`), each
producing one piece of the Wave-1 implementation contract. **This
document is the frame those pieces plug into**, per stage directive §3:
a single traceability map from architecture document → ADR → domain
object → database object → service → API → event → ledger transaction →
audit event → test suite.

It is **not** an attempt to author every cell. `architect` owns and has
filled: the map's structure and identifier scheme (§7), the package/
module boundary (§1), event-taxonomy integration and the honest
transport finding (§2), segmentation placement (§3), the API
player/staff boundary (§5), the not-in-B1 boundary and the exact
interface seams B1 must leave behind (§6), the cross-domain
architectural invariants `qa`/`code-reviewer` verify against (§8), and
one ADR-0037-owned decision this document is the correct place to make
(§4.2). Every other cell carries an explicit **OWED BY** attribution and
is filled in the reconciliation round, not asserted here.

Per `CLAUDE.md`'s no-fake-completion rule, every row in §7 carries a
status label. Most are `NOT IMPLEMENTED`. Two are `BLOCKED`. None is
`IMPLEMENTED`.

### 0.1 Authority boundaries observed by this document

- It does **not** override `ledger-finance` on any financial invariant
  or `security` on any security requirement. Where this document names a
  posting, an account type or a transaction type, it is citing ADR 0032
  / `ledger-accounting-model.md` verbatim as a map key, never designing
  accounting treatment.
- It does **not** decide anything reserved to the human: the three open
  decisions in `docs/decisions/0039-human-decision-register-stage-4h-b0-
  r7.md` (G-2 terminal-Grant credit, `OpenBetSelfExclusionPolicy`
  default, bonus-funded cashout policy) remain unmade and unselected.
- It does **not** authorize a stage transition, expand the five-type
  first slice frozen in `10-bonus-engine-architecture.md`'s Stage 4H-B0
  MVP scope plan §1, or shrink it.
- Amendments it requires to documents owned by others
  (`22-canonical-activity-event-taxonomy.md`, owned by the Master
  Orchestrator; `docs/governance/ownership.md`, owned by the Master
  Orchestrator) are **recorded as required amendments in §9**, not
  applied unilaterally.

---

## 1. Service and module boundary — where the Bonus Engine lives

`ARCHITECTURAL DECISION` (`architect`), `NOT IMPLEMENTED`.

### 1.1 Verified current convention (not assumed — inspected)

Every domain package under `internal/` today is **flat**: `find internal
-mindepth 2 -type d` returns nothing. There is no subpackage anywhere in
the tree. Files are split by concern inside one Go package, HTTP
handlers live in `internal/httpserver`, and the first file carries the
package doc comment:

| Package | Files | Shape |
|---|---|---|
| `internal/casino` | `types.go` (package doc + domain types), `orchestrator.go` (composition/dispatch), `capability.go`, `catalogue.go`, `launch.go`, `mock.go`, 6 test files | flat, concern-split |
| `internal/risk` | `types.go` (package doc), `evaluator.go` (the `Evaluate` boundary), `policy_service.go` (admin CRUD), `cumulative.go`, `denomination.go`, 7 test files | flat, concern-split |
| `internal/withdrawal` | `withdrawal.go` (package doc + state machine), `policy.go` (configuration resolution), 3 test files | flat, concern-split |
| `internal/rg` | `rg.go`, `self_exclusion_policy.go`, `self_exclusion_enumeration.go`, `enumeration_sweep.go`, tests | flat, concern-split |

HTTP surface convention (`ownership.md`, "API / HTTP (domain
handlers)"): `internal/httpserver/<domain>_handlers.go`,
`<domain>_admin_handlers.go`, `<domain>_routes.go` — owned by the
domain's specialist, with `server.go`/`routes.go` owned by `backend`.

### 1.2 Decision

**`internal/bonus` — one flat Go package, no subpackages** — confirming
the path `ownership.md` already reserves and `10-bonus-engine-
architecture.md`'s B0 scope plan §2 already names, and matching the
verified convention above rather than inventing a new one.

Proposed file layout (concern-split, names are a recommendation to
`bonus-engine`, not a constraint on its internal organization):

| File | Concern |
|---|---|
| `types.go` | Package doc comment, `Campaign`/`Offer`/`Grant`/`ProgressEntry`/`GrantStatus`/`TransitionType`/`ReasonCode` types, the five configuration axes, sentinel errors |
| `orchestrator.go` | The three-way gate composition (`AssetAuthorization` → `rg.EvaluateEligibility` → `risk.Evaluate`, doc 10 §T.1), the advisory-lock discipline, every Grant state transition, audit + Progress append in the same transaction — the direct analogue of `internal/casino/orchestrator.go` |
| `campaign.go` | Campaign/Offer authoring, versioning, dual-scope resolution (most-specific-row-wins) |
| `grant.go` | Grant issuance/activation, idempotency-key composition, Offer-version freezing (doc 10 §T.2) |
| `progress.go` | Model C's `P_net`/`P_firm` derivations and the nullification predicate (`ledger-accounting-model.md` §6.6.3–§6.6.6) — derivations only, never a stored counter |
| `wagering.go` | Split-instruction computation (doc 10 §6 item 1), per-category contribution weighting, rounding via the one shared `rounding_rules` function (ADR 0021 DS-1/DS-2) |
| `conversion.go` | Completion/conversion evaluation, payout ordering, max-cashout |
| `cashback.go` | Cashback window settlement job (`clock_timestamp()`-based, doc 10 §2) |
| `coupon.go` | Coupon-code validation/redemption |
| `fulfillment.go` | The `RewardFulfiller` seam and its single B1 implementation (§6.3) |
| `activity.go` | The `ActivityEvent` inbound contract type and its single B1 adapter (§2.5) |
| `*_test.go`, `*_integration_test.go`, `adversarial_*_test.go` | colocated, per `ownership.md`'s testing row |

HTTP: `internal/httpserver/bonus_handlers.go` (player),
`bonus_admin_handlers.go` (staff), `bonus_routes.go`, owned by
`bonus-engine`; the single `Deps`/`server.go` wiring edit is filed as a
dependency request against `backend` per `integration-protocol.md`,
because `ownership.md` rule 2 serializes same-file edits.

### 1.3 Explicitly rejected alternatives, with reasons

- **Subpackages (`internal/bonus/campaign`, `internal/bonus/grant`, …)**
  — rejected. Zero precedent in this tree; Go would force either
  exported-everything or an `internal/bonus/internal` shim; and the
  Grant transition path must hold one `pgx.Tx` across campaign lookup,
  gate evaluation, posting and Progress append (doc 10 §9/§T.1), which
  subpackages would push toward passing a transaction across package
  boundaries — the exact shape that makes the `internal/casino`
  orchestrator readable today.
- **A separate deployable** — rejected. ADR 0010 (single-service
  foundation) is unchanged; `02-domain-and-service-boundaries.md`'s
  service table is explicitly "the target shape for later stages, not
  what exists." Bonus adds a package, not a deployable.
- **Putting bonus HTTP handlers in `internal/bonus`** — rejected. Every
  domain's handlers live in `internal/httpserver`; moving one domain's
  breaks `routes.go`'s single-route-table visibility property.

---

## 2. Event/Activity taxonomy integration

### 2.1 The honest transport finding — there is no event bus

`FINDING` (`architect`, verified against `HEAD`, not inferred from prose).

`internal/eventbus` exists. It is labeled `STATUS: STUB` in its own
package doc. It has **zero usages anywhere else in the repository** —
`grep -rn "eventbus" --include=*.go .` excluding the package itself
returns nothing. No producer publishes. No consumer subscribes. No
broker is deployed. `InMemoryBus` is synchronous, in-process,
non-durable and does not survive a restart, and its own doc comment says
so.

Its `Event` envelope carries **5 fields** (`EventID`, `Type`,
`TenantID`, `Payload`, `OccurredAt`).
`22-canonical-activity-event-taxonomy.md`'s envelope requires **17**
(adding `source`, `brand_id`, `person_id`/`player_account_id`,
`recorded_at`, `is_real_money`, `funding_source`, `correlation_id`,
`reverses_ref`, `operation_ref`/`provider_ref`,
`asset_code`/`amount_minor_units`, `idempotency_key`,
`schema_version`). Doc 22 is itself labeled "Design only — no event is
implemented, no producer is wired, no consumer is wired this stage."

**What actually exists today as the platform's fact stream** is two
durable, append-only stores plus direct in-process calls:

1. **`ledger_transactions` + `ledger_entries`** — carrying
   `transaction_type`, `correlation_id` (migration 0021),
   `reverses_transaction_id`, `provider_id`/`provider_tx_id`, and a
   database-enforced idempotency key. This is the only durable, ordered,
   per-operation-correlated monetary fact log the platform has.
2. **`audit_log`** (`internal/audit`) — append-only, actor-attributed,
   with `Action` strings like `casino_bet.posted`,
   `casino_rollback.tombstoned`.
3. **Direct synchronous in-process calls** —
   `internal/casino/orchestrator.go` calling `rg.EvaluateEligibility`,
   `risk.Evaluate` and `ledger.Post` inside one `pgx.Tx`.

**So: "event" in this codebase today is a ledger `transaction_type` plus
a `correlation_id` convention, plus an audit `Action` string. Doc 22's
taxonomy is aspirational, not operational.** Any B1 design written as
"Bonus subscribes to `payments.deposit.settled`" is describing
infrastructure that does not exist and is not authorized to be built in
B1 (a broker is `docs/decisions/0003`'s deliberately deferred decision,
restated as doc 22's open decision 1).

### 2.2 Consequence for B1 — the seam, not the bus

`ARCHITECTURAL DECISION` (`architect`), `NOT IMPLEMENTED`.

B1 **does not build, deploy or depend on a message broker**, and does
not wire `internal/eventbus`. Instead:

1. **B1 defines the canonical event envelope as a Go type** —
   `bonus.ActivityEvent`, carrying doc 22's 17 envelope fields verbatim.
   This is the *only* shape Bonus's lifecycle logic ever reads. Doc 22's
   hard rule ("Bonus never accepts a provider-specific or
   domain-internal payload directly") is satisfied at the type level,
   independently of transport.
2. **B1 supplies exactly one adapter** that constructs an
   `ActivityEvent` from an already-committed platform fact. Two sources,
   both already durable:
   - **Ledger-derived** for every monetary trigger (deposit settled, bet
     settled, win settled, rollback). This is not a new mechanism
     invented here — `ledger-accounting-model.md` §6.6.3's **Model C**
     already mandates that wagering progress be *derived from ledger
     entries*, never accrued into a counter, precisely because "a
     derived predicate is idempotent by construction." B1 extends that
     already-chosen discipline from progress to triggering.
   - **In-process post-commit call** from the producing domain where a
     non-monetary fact has no ledger row (e.g. a coupon redemption
     initiated by the player's own API call, a staff action).
3. **The adapter is the only thing that changes when a broker lands.**
   Bonus's lifecycle code, its idempotency keys, its dedupe discipline
   and its test suite are transport-agnostic by construction.

**Rejected: calling Bonus synchronously from inside `postBet`/`postWin`/
the deposit posting transaction.** A bonus-side failure would roll back
the bet or the deposit — the money path must never be made to depend on
the promotional path's availability. Rejected equally: a fire-and-forget
post-commit call with no durable backstop, which silently drops a grant
trigger on a crash between commit and call. The ledger-derived read with
a per-`(tenant_id, consumer)` watermark is the durable backstop; the
post-commit call is a latency optimization on top of it, never the sole
delivery mechanism.

**Dedupe rule, binding and inherited unchanged from doc 22**: Bonus
dedupes on `idempotency_key` (unique per *business fact*), never on
`event_id` (unique per *publish*). For a ledger-derived event the
`idempotency_key` is derived deterministically from the source ledger
transaction's own identity, so redelivery and re-derivation produce the
identical key by construction.

### 2.3 Consumed events — do they fit the existing taxonomy?

**Yes, for the five in-slice bonus types, with zero new consumed event
types required.** Doc 22's table already contains every input B1 needs.

| B1 need | Doc 22 canonical type | Verdict |
|---|---|---|
| Deposit/reload grant + activation trigger | `payments.deposit.settled` | Fits, no change |
| Wagering progress contribution; excluded-game and max-bet-while-wagering breach detection | `casino.bet.settled` | Fits; `funding_source` and `is_real_money` envelope fields (doc 22's own P1 additions) are load-bearing for both |
| Win credit destination + `P_net` movement | `casino.win.settled` | Fits, no change |
| Model C nullification (gate G-3's netting) | `casino.bet.rolled_back` | Fits. Doc 22's explicit scope note — this event does **not** fire for a tombstoned rollback — is correct and safe for Bonus: no progress was ever credited for an original that never posted |
| Registration-triggered Offers (not required by the five in-slice types; subscribed so a future no-deposit Offer needs no new plumbing) | `identity.person.registered` | Fits, no change |
| RG status change (analytics/CRM only) | `rg.status.changed` | Fits. **Binding**: consumed for *informational* purposes only. Enforcement is always the synchronous `rg.EvaluateEligibility` call (doc 22 consumer contract item 3, ADR 0034 §2.1) |
| KYC-gated grant status (`awaiting_verification`, ADR 0034 §6) | `kyc.verification.updated` | Fits *in shape*. **OWED BY `identity-compliance`**: whether B1's five types require a KYC gate at all, and if so at which checkpoint |

**Resolved here — doc 10's "Genuine gaps" item 3.** Doc 10 §1.3's five
informal names (`player.registered`, `deposit.settled`,
`round.settled`, `bet.settled`, `session.started`) are reconciled onto
doc 22's namespaced taxonomy as above. Specifically: **`round.settled`
does not exist and must not be created** — doc 22 correctly splits
settlement into `casino.bet.settled` and `casino.win.settled`, and
Bonus's contribution rules key on the *bet* (the stake is what
contributes), with the *win* affecting only the credit destination.
**`session.started` (`casino.launch.started`) is not consumed by B1 at
all** — no in-slice Offer's eligibility or progress depends on a launch,
and doc 22 itself flags that `LaunchGame` cannot currently distinguish
real from demo at the producer.

**Not consumed by B1, explicitly**: `sportsbook.*` (no
`internal/sportsbook` package exists), `gamification.*` (not
authorized), `payments.withdrawal.settled`, `casino.launch.denied`,
`risk.decision.denied`.

### 2.4 Produced events — the minimal taxonomy extension required

Doc 22 assigns Bonus **four** produced types. The stage directive names
**nine** Bonus lifecycle facts. Reconciled:

| Directive fact | Doc 22 today | Required action |
|---|---|---|
| granted | `bonus.grant.created` | None — fits |
| activated | *absent* | **ADD `bonus.grant.activated`** |
| progress-changed | *absent* | **ADD `bonus.grant.progress_changed`** |
| wagering-completed | `bonus.grant.completed` | None — fits |
| converted | *absent* | **ADD `bonus.grant.converted`** |
| cancelled | conflated into `bonus.grant.reversed` ("A bonus Grant is cancelled/reversed") | **SPLIT** — see below |
| expired | `bonus.grant.expired` | None — fits |
| (forfeited — named in doc 10 §1.2 as a state distinct from `cancelled`, omitted from the directive's list and from doc 22) | *absent* | **ADD `bonus.grant.forfeited`** |
| reward-requested | *absent* | **ADD `bonus.reward.requested`** (reserved type string only — no producer in B1, §6.3) |

**Two real defects found in doc 22, not cosmetic gaps:**

- **D-1 (correctness).** `bonus.grant.reversed` is documented as firing
  when "a bonus Grant is cancelled/reversed." ADR 0032 §3.1 — the
  binding event→posting map — assigns `cancelled` the
  `bonus_forfeiture` transaction type and `reversed` the
  `bonus_reversal` transaction type. These are different postings with
  different financial meaning (`cancelled` writes down an outstanding
  balance; `reversed` compensates a prior terminal state because its
  upstream justification was itself reversed). One event type covering
  both makes the reversal/forfeiture distinction unrecoverable for any
  downstream consumer and, worse, makes `reverses_ref` ambiguous —
  doc 22 requires that field "only on an event that reverses/voids/
  cancels a previously-published event." Requires
  `bonus.grant.cancelled` as its own type.
- **D-2 (completeness).** `forfeited` is a distinct terminal Grant state
  in doc 10 §1.2 ("always carries a reason code"), reachable by
  wagering-rule breach or manual-review outcome, and is the state a
  disputing player's case most often turns on (doc 10 §10's audit
  table). Doc 22 has no event for it.

**This is an additive extension, not a redesign.** Every new type reuses
doc 22's existing envelope unchanged, adds no field, and changes no
existing type's meaning except by splitting D-1's conflation.
**Doc 22 is owned by the Master Orchestrator**; §9 records this as a
required amendment routed there, not applied here.

### 2.5 Event property compliance

Each required property, and where B1 satisfies it:

| Property | How B1 satisfies it |
|---|---|
| Tenant-scoped | `tenant_id` is a mandatory envelope field, resolved server-side from the Grant's own owning records; never client-supplied (`CLAUDE.md` multi-tenancy) |
| Auditable | Every produced lifecycle event has a 1:1 `audit.Record` in the same transaction (§7 Table B's audit column) and a `bonus_progress` row (doc 10 §10.1) |
| Idempotent where applicable | Dedupe on `idempotency_key`; DB unique constraints per doc 10 §9's three keys; `pg_advisory_xact_lock` on `(tenant_id, grant_id)` |
| Provider-neutral | No `provider_id`-keyed branch in any Bonus rule. `provider_ref` is carried as an opaque correlation value only. Verifiable by the same import/grep inspection `02-domain-and-service-boundaries.md` already uses for the risk/rg separation |
| Correlation-aware | `correlation_id` threaded from the triggering ledger transaction through the Grant's Progress trail, the resulting posting, and the audit record — the same value, never re-minted |
| Transport-agnostic | §2.2: Bonus reads `bonus.ActivityEvent`, never a bus type. `internal/bonus` must not import `internal/eventbus` in B1 |

---

## 3. Player segmentation — placement decision

`ARCHITECTURAL DECISION` (`architect`), `NOT IMPLEMENTED`. This is a real
decision, made here, with its reversal cost stated.

### 3.1 What the existing documents actually say

- `10-bonus-engine-architecture.md` §1.1: a Campaign carries a "target
  segment definition"; §T.2/§T.11: the eligibility-axis snapshot
  (including segment) is frozen onto the Grant at `issued`.
- `19-mission-architecture.md` §9: a mission's audience is a **segment
  reference**, and segment definition is "a capability this document
  assumes rather than designs." Its open decision 5 is "**where do
  segments come from — buy vs. build**, and on what timeline. Not a
  mission-domain decision."
- `17`/`18`/`20` each already carry `segment_ref` as a first-class
  definition field (programme, tournament, marketplace item).
- `02-domain-and-service-boundaries.md`, "What is explicitly NOT a
  separate service (yet)": "CRM/campaign journey builder (**buy
  first**)."

### 3.2 The decision

**Create `internal/segment` as a separate, thin, `architect`-specified
package in B1 — and deliberately do not build a segmentation *engine*
inside it.**

Two separable questions were conflated in the directive's framing, and
they get opposite answers:

| Question | Answer |
|---|---|
| Where does the segment **boundary** live — a shared package, or inside `internal/bonus`? | **A shared package, `internal/segment`, from day one.** |
| How much segment **capability** does B1 build? | **The minimum Bonus actually consumes. No rule DSL, no behavioural recomputation scheduler, no CRM journey builder, no campaign-management UI, no reporting surface.** |

**B1's `internal/segment` ships exactly:**

- A `Segment` definition row — dual-scope (`tenant_id IS NULL` =
  platform-wide template, set = tenant-owned, optionally narrowed by
  `brand_id`), mirroring `risk_rules` (ADR 0031 §3) and doc 19 §10's own
  rule, versioned and disable-not-delete.
- One interface, with one implementation:
  `IsMember(ctx, tx, tenantID, brandID, playerAccountID, segmentID)
  (member bool, reason ReasonCode, err error)`.
- Fail-closed, without exception: a non-nil error is `member = false`; an
  absent segment definition is `member = false`, never "no restriction
  configured, therefore everyone" — inherited verbatim from ADR 0037
  §C.1's fail-closed default and `internal/risk`'s error contract.
- Explicit membership (a staff/import-populated membership table) plus a
  **closed, non-extensible** set of declarative predicates that Bonus's
  five in-slice types actually need. New predicates go through a
  documented extension process, mirroring ADR 0031 §12 — never an ad hoc
  addition.
- Server-side resolution only; a segment id is never accepted from a
  client.

**B1's `internal/segment` explicitly does not ship**: a query/expression
language, a behavioural recompute job, real-time membership streaming, a
journey builder, a segment-overlap/audience-size analytics surface, or
any provider integration.

### 3.3 Why a shared package rather than "build it in Bonus with a seam"

1. **The separation is free today and the extraction is not.** ADR 0010
   keeps one deployable; `internal/segment` is a package, not a service
   — no network hop, no new ops surface, no new deployment artifact. The
   cost is one directory and one ownership row. The extraction cost
   later is not symmetric: a segment reference is **denormalized onto
   `bonus_campaigns` and snapshotted onto `bonus_grants`** (doc 10 §T.2/
   §T.11) and will appear in audit metadata and reporting reads. Moving
   the owning table after those references exist means either a
   cross-domain FK rewrite across a domain that does not own it, or a
   second segment table that immediately drifts.
2. **The second consumer is specified, not hypothetical.** `CLAUDE.md`'s
   scope test asks whether something is "required by … the future B2B
   architecture." Four frozen Stage 4H-A architecture documents (17, 18,
   19, 20) already name `segment_ref` as a definition field. This is the
   difference between hedging against a named, documented boundary and
   building generality on spec — the latter is what §3.2's capability
   answer refuses.
3. **It is the correct place for the buy-vs-build seam.** Doc 02 says
   CRM/segmentation is bought before it is built, and doc 19's open
   decision 5 leaves that unmade. An `IsMember` interface is exactly the
   shape `CLAUDE.md`'s provider-abstraction rule prescribes: when a CRM
   is bought, it becomes a second implementation behind the same
   interface and **no Bonus code changes**. Had the interface been
   `internal/bonus`-private, the bought CRM would arrive as a Bonus
   dependency, which is backwards.
4. **It keeps Bonus out of a second policy engine.** Doc 10's "Bonus
   must never build" list forbids a parallel risk engine, RG engine and
   asset registry for one reason — two unreconciled policy systems
   governing the same player drift silently. A Bonus-private segment
   engine that Gamification later duplicates is that same failure, one
   domain over.

### 3.4 Why the capability stays minimal (the anti-overengineering half)

A generic segmentation engine is precisely the "generic platform service
on spec" `product-owner-proxy` exists to block, and building one would
also **pre-empt an unmade product decision** (doc 19 open decision 5's
buy-vs-build). B1 therefore builds the narrowest thing that makes a
Campaign's target-audience field real and keeps the eventual buy
decision open.

### 3.5 Reversibility, stated explicitly

If `product-owner-proxy` or the Orchestrator rejects this in
reconciliation, the fallback is **`internal/bonus/segment.go` carrying
the identical interface and the identical fail-closed contract** — a
rename and an ownership-row deletion, not a redesign. The decision that
is *not* cheaply reversible, and which this document therefore commits
to regardless of where the code lives, is the pair of invariants below.

### 3.6 Two binding invariants (independent of placement)

- **SEG-1 — resolve at eligibility time, snapshot the result.** Segment
  membership is evaluated once, at `issued`, and the *result* is frozen
  onto the Grant (doc 19 §9's own requirement, doc 10 §T.4's
  eligibility-axis snapshot rule). A player does not lose an in-flight
  Grant because a segment definition changed underneath them; and a
  disputing player's Progress trail explains the decision using the
  membership that was actually in force.
- **SEG-2 — a segment is never the mechanism for a safety gate.**
  Jurisdiction, licensing mode, RG status, risk exposure, asset
  authorization, brand, VIP tier, product and KYC state are **not**
  segments (doc 19 §9's own list) and must never be expressed as one.
  Segments are commercial audience targeting; safety/compliance gates
  are `AssetAuthorization` → RG → Risk, live, at every value-moving
  checkpoint (doc 10 §T.1/§T.11).

### 3.7 Ownership

`internal/segment` is **not** `bonus-engine`-owned. Per `ownership.md`
rule 4 (ambiguous ownership defaults to the Orchestrator, recorded
explicitly), `architect` recommends: **`architect` owns the interface,
the fail-closed contract and the schema shape; `bonus-engine` implements
and owns the first consumer's call sites.** A new `ownership.md` row is
required — recorded in §9, not written here.

---

## 4. Cross-domain decisions this document makes or records

### 4.1 Roster gap — the missing `bonus-finance` specialist

`OPEN ITEM — TO BE CONFIRMED IN RECONCILIATION` (stage directive §3
item 4).

The stage directive names a `bonus-finance` specialist. No such
specialist exists in this environment's configured roster
(`.claude/agents/`, `docs/governance/agent-registry.md`). Its concerns
are being covered jointly by `ledger-finance` and `bonus-engine` in
parallel Wave-1 dispatches. `architect` cannot confirm sufficiency in
this dispatch — those outputs are not visible here.

**Recorded now as reconciliation checklist item OI-1**, with the exact
seven concerns that must each have a named owner and a filled §7 row
after Wave 1. If any is unclaimed, the roster gap has materialized as a
real hole rather than a naming artifact:

| # | Concern | Natural owner |
|---|---|---|
| a | ADR 0032 §3.1's event→posting map instantiated per in-slice bonus type, including the direct-cash-reward two-entry shape | ledger-finance |
| b | Model C's `P_net`/`P_firm` derivation queries and the nullification predicate, made concrete (`ledger-accounting-model.md` §6.6.4–§6.6.6) | ledger-finance + bonus-engine |
| c | HR-9's fail-closed posting guard removal, sequenced with the Rule B2 (extended) mirror generator in `internal/ledger` (doc 10 B0 §3 item 3a) | ledger-finance |
| d | ADR 0021 DS-1/DS-2 rounding applied at the three named boundaries (grant amount, contribution split, conversion), via the one shared function | ledger-finance |
| e | Campaign-level budget-cap enforcement — currently has **no designed mechanism and no owner** (doc 10 §1.1's P2, Genuine gaps item 7) | **unassigned** |
| f | The bonus reconciliation stream (ledger vs. Grant/Progress; the memo/audit stream for `inside_provider` Grants, `reconciliation-model.md` §2.10) | ledger-finance |
| g | `bonus_expense`'s statutory/reporting presentation (ADR 0032 §2 `OPEN DECISION`) | ledger-finance / human |

### 4.2 `AssetAuthorization.Operation` for a bonus checkpoint — decided

`ARCHITECTURAL DECISION` (`architect`), closing
`10-bonus-engine-architecture.md`'s **"Genuine gaps found" item 1**.

ADR 0037 is `architect`-owned (`ownership.md`, "Asset Registry +
Authorization"), so this document is the correct place to decide it, and
doc 10 explicitly requires an explicit decision rather than an
implementer's assumption.

**Decision: every Bonus Engine checkpoint that calls
`AssetAuthorization.CheckEligibility` passes `operation = wagering`.**
This applies to all three value-creating checkpoints — activation
(§T.3), reward credit (§T.8), and conversion (§T.12) — and to nothing
else (value-reducing transitions are never gated, §T.5.1).

Reasoning:

- Bonus value's only permitted use is wagering. `player_bonus` is not
  withdrawable and cannot be converted across assets (doc 10 Dependency
  Contract Freeze §8: bonus accounting is always same-asset, and a
  `ConversionOperation` is forbidden without its own ADR). ADR 0037's
  `conversion` value means FX/asset conversion and is therefore the
  wrong value for a same-asset bonus→cash release.
- Conversion's *output* is `player_cash`, which is separately and
  independently gated: by `wagering` when it is staked and by
  `withdrawal` when it is withdrawn. Passing `withdrawal` at conversion
  would gate a movement that is not a withdrawal and would let an asset
  with withdrawals paused block an already-earned entitlement — the
  exact perverse outcome ADR 0034 §2 and doc 10 §5 already reject.
- **A seventh `bonus` operation value is deliberately not added.** Doc 10
  §7 already fixes the two correct mechanisms for "this jurisdiction
  bans this bonus type": a Risk `HARD_LIMIT` scoped by
  `JurisdictionCode` on `bonus_grant`, or Offer-level eligibility
  configuration. A third mechanism expressing the same rule at layer 7
  would create exactly the two-unreconciled-policy-surfaces drift ADR
  0037 §C.2's "one canonical authorization concept — not five
  reimplementations" exists to prevent.
- **Reversible.** Should a concrete operator need appear, adding a
  `bonus` value is an additive CHECK widening on migration 0045's
  `(product, operation)` dimension plus a Go constant — the same
  five-step additive shape ADR 0031 §12 uses. Nothing in this decision
  forecloses it.

Requires a reflected amendment in ADR 0037 §C.2's operation list and the
removal of doc 10's Genuine-gaps item 1 — recorded in §9.

### 4.3 A B1-blocking finding: gate G-2 is reachable in the casino-only slice

`FINDING` (`architect`), **P1**, verified against `HEAD`.

ADR 0039 Decision 2 frames the terminal-Grant late-credit question (gate
**G-2**) as sportsbook-specific — "because a portion of that Grant's
value remained locked inside an open sportsbook bet" — and closes with
"cash-only sportsbook wagering is unaffected." Doc 10 §T.7/§T.9 frames
it around `player_locked_bonus`. Both framings assume a *locked* stake,
which casino does not have.

**The trigger condition does not actually require a locked stake — only
a timing gap between the bonus-funded stake and its later credit.**
`internal/casino` posts `postBet` and `postWin` as two separate provider
callbacks with two separate `provider_tx_id`s and an arbitrary interval
between them, and `postRollback` can arrive later still. So:

> A bonus-funded casino bet posts at T. The Grant's time limit fires at
> T+1 and it goes `expired` (or staff cancels it, or another bet
> forfeits it). The win settles at T+2 and credits `player_bonus` under
> a Grant that is already terminal.

That is precisely G-2's shape, reached without any sportsbook, any
`player_locked_bonus` account, and any of the four trigger paths doc 10
§T.13 enumerates. Since bonus-funded casino stakes are exactly what the
B1 first slice exists to enable (doc 10 §6 item 1's split instruction),
**ADR 0039 Decision 2 is a Stage 4H-B1 blocker, not a deferred
sportsbook concern.**

A second, related implementation gap confirms the exposure is real:
`internal/casino`'s `postWin` **hardcodes `ledger.AccountPlayerCash` as
the credit destination** (inspected at `orchestrator.go`). The design
rule already exists — `ledger-accounting-model.md` §6.4 case F: "Win
after bonus-funded bet … payout `Cr player_bonus (S+W)`, continuing
wagering progress … Same treatment ADR 0032 already gives a bonus-funded
casino win" — but the code does not implement it. **If B1 wires
bonus-funded casino stakes without a `casino`-owned change to
`postWin`'s destination resolution, a bonus-funded bet's win credits
withdrawable cash with no wagering requirement** — a direct, trivially
exploitable bonus-abuse path. Recorded as BC-21/BC-07's cross-domain
dependency in §7 and as OI-5 in §9.

`architect` does not select among ACTION_REFORFEIT / ACTION_ROUTE_TO_CASH
/ ACTION_HOLD_FOR_REVIEW — that remains the human's decision per ADR
0039. What this finding changes is only its **urgency and scope**, which
is a factual correction to ADR 0039 Decision 2's "where implementation
actually stands" section, routed to `ledger-finance`/the Orchestrator in
§9.

---

## 5. API surface skeleton — player vs. staff boundary

`RECOMMENDATION`, `NOT IMPLEMENTED`. Conventions inherited unchanged
from `04-api-architecture.md` and `25-bonus-gamification-api-
architecture.md` §0 (tenant always server-resolved via
`tenant.FromContext`; `internal/apierror.Error` shape and existing
`Code` enum, no new error taxonomy; `/v1/` prefix, no per-domain
version; cursor pagination for time-ordered lists, server-capped
`limit`; DB-enforced idempotency, never check-then-insert). Paths are
illustrative of the convention, not a route commitment.

### 5.1 Player surface — `internal/httpserver/bonus_handlers.go`

Player JWT only; `player_account_id` from the JWT subject, `tenant_id`
and `brand_id` resolved server-side from the player's own account.

| Endpoint | Purpose | Notes |
|---|---|---|
| `GET /v1/me/bonuses` | Own Grants, active + historical, cursor-paginated | |
| `GET /v1/me/bonuses/{grantID}` | Grant detail + wagering progress | **Binding**: the player-facing progress number is **`P_net`** (`ledger-accounting-model.md` §6.6.3 names it "the player-facing number"). `P_firm` is an internal authorizing measure and is never exposed |
| `GET /v1/me/bonuses/offers` | Offers currently available to this player | Eligibility filtering is server-side. Must not leak Offer rule internals, campaign budget, segment membership logic, or the existence of Offers the player is not eligible for |
| `POST /v1/me/bonuses/{grantID}/activate` | Explicit opt-in activation, where the Offer requires one | Idempotent on `grant_id`; a repeat returns the existing state, never a second `bonus_grant` posting |
| `POST /v1/me/bonuses/{grantID}/cancel` | Player opt-out where the Offer permits it | Reason recorded as `player_opt_out`; posts the `bonus_forfeiture` write-down shape (ADR 0032 §3.1) |
| `POST /v1/me/bonuses/coupons` | Redeem a coupon code | Idempotent on `(tenant_id, player_account_id, code)`. **OWED BY `security`**: the response must not become a code-enumeration oracle (invalid / valid-but-ineligible / already-redeemed must not be distinguishable to an unauthenticated-in-effect attacker), and the endpoint needs rate limiting. `architect` records the requirement; `security` owns the exact response discipline |

**A player never sees**, under any circumstance: another player's data,
campaign budget or spend, Offer rule content beyond the player-facing
terms, an RG/Risk/`AssetAuthorization` decision code verbatim, or a
manual-review queue entry. This generalizes doc 25 §1.7's corrected
two-projection rule (player-facing projection carries no
withholding/disqualification reason and never an RG-derived code) from
leaderboards to every Bonus player endpoint.

### 5.2 Staff surface — `internal/httpserver/bonus_admin_handlers.go`

| Endpoint | Purpose | Proposed permission |
|---|---|---|
| `GET /v1/admin/bonus/campaigns`, `POST /v1/admin/bonus/campaigns` | Campaign authoring; create is idempotent via caller-supplied `client_reference` | `bonus_config:read` / `bonus_config:manage` |
| `POST /v1/admin/bonus/campaigns/{id}/disable` | Disable-not-delete state transition, mirroring `risk_rules` | `bonus_config:manage` |
| `GET /v1/admin/bonus/offers`, `POST /v1/admin/bonus/offers`, `POST /v1/admin/bonus/offers/{id}/versions` | Offer authoring; a new version is always a new immutable row, never an edit (doc 10 §1.1) | `bonus_config:read` / `bonus_config:manage` |
| `GET /v1/admin/bonus/coupons`, `POST /v1/admin/bonus/coupons` | Coupon batch authoring | `bonus_config:manage` |
| `GET /v1/admin/players/{id}/bonuses` | One player's Grant/Progress history (support, compliance, dispute resolution) | `bonus:read` |
| `POST /v1/admin/players/{id}/bonuses` | **Manual grant** (goodwill, VIP desk) | `bonus:adjust` — reason code mandatory; four-eyes above a configurable threshold |
| `POST /v1/admin/bonus/grants/{id}/cancel` \| `/forfeit` \| `/convert` | Staff-driven terminal transitions and manual conversion override | `bonus:adjust` — reason code mandatory; four-eyes above threshold for `/convert` (it creates withdrawable value) |
| `GET /v1/admin/bonus/review-queue`, `POST /v1/admin/bonus/review-queue/{id}/resolve` | Abuse-detector queue (doc 10 §1.4) | `bonus:review` |
| `GET /v1/admin/bonus/campaigns/{id}/summary` | Campaign liability/expense/spend reporting | `bonus:read`. **Binding**: every figure is derived from `ledger_entries`, never from a Bonus-owned balance column (doc 10 §6's "never a side table nobody can reconcile") |

### 5.3 Permission model — one addition to doc 25's proposal

Doc 25 §2 proposes `bonus:read` and `bonus_config:read`/
`bonus_config:manage`. `architect` adds **`bonus:adjust`** and
**`bonus:review`**, and states why rather than minting permissions by
reflex:

Doc 25's own P1 finding F2 established the principle for tournaments —
"the role that authors a prize structure must not also be the role that
pays it out unaccompanied" — after finding `/settle` wrongly bundled
under `tournament_config:manage`. The identical separation applies here
and is arguably sharper: an actor holding only `bonus_config:manage` can
already author a narrow-eligibility, high-value Offer (which doc 25's F3
finding correctly identifies as functionally a manual grant, and for
which it already requires four-eyes above a threshold). If that same
actor could also manually grant and manually convert, there would be no
second pair of eyes anywhere on the creation of withdrawable player
value. **`bonus:adjust` is therefore never bundled with
`bonus_config:manage` by default and never granted to the same role as a
matter of course** — mirroring `internal/auth/permission.go`'s existing
`RoleFinance`/`RoleRiskManager`/`RoleCompliance` dedicated-narrow-role
shape.

Doc 25's recorded build-time requirement stands and is restated: mint a
dedicated role (`RolePromotionsManager`) holding the `manage`-class set
and nothing else; a permission with no grantee gets bundled into
`RoleTenantAdmin` by default, which is the failure `permission.go`'s own
comments warn against. **`security` owns the final permission/role
decision** — `architect` proposes names and the separation-of-duties
shape only.

### 5.4 Tenant / brand / jurisdiction / hierarchy scoping

- **Tenant/brand**: server-resolved only. Campaign/Offer are dual-scope
  (nullable `tenant_id`, optionally narrowed by `brand_id`,
  most-specific-row-wins whole-row replacement); Grant/Progress are
  always `tenant_id NOT NULL` + brand + player scoped with dual
  `tenant_staff_scope` + read-only `player_self_scope` RLS (doc 10 §8).
- **Platform-wide Campaign writes**: the shape is expressible but **no
  platform-scoped write path exists for any domain** (doc 10 §8, ADR
  0031 §8). B1's admin API must therefore **reject** a platform-wide
  Campaign creation attempt with an explicit, honest error rather than
  silently accepting a nil `tenant_id` it cannot correctly scope. This
  is a concrete B1 requirement, not a note.
- **Jurisdiction**: consumed, never resolved by Bonus (doc 10 §7). The
  pre-existing `TODO(jurisdiction)` gap (doc 15, ADR 0031 §9 — no
  per-player jurisdiction resolver exists) applies to Bonus exactly as
  it applies to Risk and Casino, and is not newly introduced or newly
  closed here.
- **Hierarchy (retail agent network)**: `NOT IMPLEMENTED`. **No
  `bonus_*` table in B1 carries a `hierarchy_node_id` column and no
  Bonus endpoint accepts a node id.** Doc 10's B0 §5 explicitly defers
  retail-specific eligibility axes, cashier-initiated grants and
  in-person coupon redemption; doc 02's retail section confirms
  `internal/agentnetwork`/`internal/retail` do not exist. Adding the
  dimension later is additive.

---

## 6. What B1 must NOT build, and the exact seams it must leave behind

### 6.1 Out of scope — re-confirmed explicitly (stage directive §2)

Every item below is **`NOT IMPLEMENTED`** and must remain so in B1. Each
is a restatement of an already-recorded boundary, not a new prohibition.

| Not built in B1 | Authority |
|---|---|
| The **Gamification Engine** in full | `14-mvp-scope-and-roadmap.md` "Features deliberately deferred"; doc 02's Gamification section ("No `internal/gamification` package, no schema, no migration, no API exists") |
| **Points / XP**, levels, and the points ledger | doc 17; `24-points-accounting-architecture.md` (not written); ADR 0031 §15h |
| **Missions** | doc 19 |
| **Achievements / badges / streaks** | doc 17 |
| **Tournaments** | doc 18 |
| **Leaderboards** | doc 17 §8, doc 18 |
| **Reward Marketplace** | doc 20 |
| The **complete Reward Orchestrator** | doc 21; doc 10 B0 §4 ("until a second concrete reward-producing domain exists… no separate orchestration package in between") |
| **Free spins / free bets** fulfillment | doc 10 B0 §1 — `CasinoProvider` has no free-round method; `internal/sportsbook` does not exist |
| The **`ExternalRewardProvider`** contract (doc 23) | doc 10 B0 §4 — every first-slice Campaign is `fulfillment_owner: internal` by construction |
| **Tournament / mission / loyalty** reward *types* | doc 10 B0 §1 — no authorized producer exists to emit the triggering signal |
| A **cash reward with no wagering requirement** | doc 10 B0 §1 — architecturally trivial and fully specified by ADR 0032 §3, but **not** among the five types `product-owner-proxy` named. A one-line scope addition for the human/Orchestrator to make explicitly, never assumed |
| A parallel **wallet / ledger / risk engine / RG engine / asset registry** | doc 10's "Bonus must never build" list |
| A **message broker** or any `internal/eventbus` wiring | §2.1; ADR 0003; doc 22 open decision 1 |
| A **segmentation engine** (rule DSL, recompute job, journey builder) | §3.2/§3.4; doc 02 "CRM/campaign journey builder (buy first)"; doc 19 open decision 5 |

### 6.2 The requirement: an interface, not a stub

The directive is precise and `architect` restates it as binding: what B1
leaves behind for Gamification and the Reward Orchestrator is **an
interface, not a stub implementation**. Concretely, B1 must **not**
ship: a no-op `RewardOrchestrator` type, a `MockGamification`, a
`PointsLedger` placeholder, an `ExternalRewardProvider` adapter, a
registry with one entry, a feature flag guarding unbuilt code, or a type
whose only implementation returns `ErrNotImplemented`. Every one of
those is the "conceptual work labeled as completion" `CLAUDE.md`
forbids, and `code-reviewer` should treat any of them as a blocking
finding.

### 6.3 Seam 1 — `RewardFulfiller` (Bonus → Reward Orchestrator)

**Shape**: one Go interface in `internal/bonus/fulfillment.go`, with
**exactly one implementation** in B1 (the direct `wallet`/`ledger`
path), and **no second implementation, no mock, no registry**.

```
Fulfil(ctx, tx, RewardDecision) (FulfilmentOutcome, error)
```

`RewardDecision` carries doc 21's already-frozen field set verbatim —
`decision_id` (the Grant id, the producer-generated idempotency key),
`tenant_id`/`brand_id`/`player_account_id`, `source_domain`,
`reward_type`, `amount_minor_units`/`asset_code` (decimal string, never
`int64` — doc 21's Wave-2 `ledger-finance` correction for exponent-18
assets), `jurisdiction_code`/`licensing_mode`, and doc 10 §3.2's
`fulfillment_destination` flag (`into_platform_wallet` |
`inside_provider`).

**Why this is a seam and not overengineering.** It is one interface at
one call site, required by an already-frozen boundary:
`02-domain-and-service-boundaries.md`'s contract discipline — "Only the
Reward Orchestrator turns a reward *decision* into a fulfilled reward.
Domains that decide a reward is owed … emit a decision and stop." B1
legitimately collapses the decision and the fulfilment into one
transaction because only one fulfilment mechanism exists
(`into_platform_wallet`, doc 10 B0 §4), but it must not **erase the
boundary** in the process — if Grant transition code calls `ledger.Post`
from a dozen sites, introducing the Orchestrator later is a rewrite
rather than a second implementation.

**Constraints**: B1's implementation supports `reward_type ∈
{bonus_credit}` only. Any other value is rejected at Offer
*configuration* time — never guessed at posting time, mirroring ADR 0032
§6(c)'s own rule for the fulfilment-destination flag. There is no
default arm and no fallback.

### 6.4 Seam 2 — `ActivityIngestor` (platform activity → Bonus)

One interface, one implementation (§2.2's adapter), consuming
`bonus.ActivityEvent` (doc 22's envelope as a Go type). This is what
makes doc 22's hard rule enforceable now — no Bonus rule ever reads a
provider payload or another domain's row — and makes the future broker
an adapter swap. B1 must not import `internal/eventbus`.

### 6.5 Seam 3 — the reserved produced-event vocabulary

B1 **publishes nothing** (no broker exists). What it must do is **emit
each lifecycle fact through one named, single-purpose function** whose
name and payload match §2.4's canonical type string, and record it in
`bonus_progress` + `audit_log`. The cost is zero; the benefit is that
when Gamification or a CRM later subscribes, the type strings, field
names and semantics already exist and nothing is renamed. Reserving
`bonus.reward.requested` matters most: it is the type a future
free-spins/free-bets Grant will use, and reserving the name now prevents
a competing one being minted inside `casino` or `sportsbook`.

### 6.6 The Gamification boundary B1 must not blur

Restated from doc 02 because B1 is the first stage that could
accidentally cross it: **Gamification is a sibling of the Bonus Engine,
not a layer of it.** A future gamification reward that happens to be
bonus-shaped is fulfilled by the Bonus Engine *via* the Reward
Orchestrator — never by Gamification calling Bonus directly, and never
by Bonus growing a mission/tournament/points concept to meet it
halfway. B1 must contain **no table, column, enum value, endpoint, Risk
rule or Offer axis that references a mission, tournament, level, badge,
streak, point or marketplace item** (doc 10 B0 §4's own closing
requirement).

---

## 7. THE MASTER MAP

The traceability map the stage directive requires. Split into two joined
tables sharing the `BC-nn` capability key, because a single ten-column
table is unreadable and unmaintainable; the join key makes them one map.

**Reading the map**: `OWED BY` marks a cell whose content belongs to a
parallel Wave-1 dispatch and is filled in reconciliation, not asserted
here. `—` means "genuinely none, and that is the design" (e.g. `issued`
produces no ledger transaction because ADR 0032 §3.1 says "a decision,
not a movement"). Database objects, event type strings, audit action
strings and test-suite slots below are **`architect`'s proposed naming**,
consistent with existing conventions (`casino_bet.posted`,
`casino_launch_sessions`, `<domain>_flow_integration_test.go`); the
owning specialist may refine a name, but not silently drop a row.

Migration numbers: next free is **0050**. The ordering authority is doc
10's B0 scope plan §3 (ledger CHECK widenings first, then Rule B2's
mirror generator + HR-9 guard removal, then `bonus_campaigns` →
`bonus_offers` → `bonus_grants` → `bonus_progress`). Exact numbers are
assigned by the Orchestrator at implementation time to avoid the
parallel-dispatch collision that has already occurred once in this
project (Stage 4H-B0-R6's `0049` self-resolution).

### 7.1 Table A — design provenance and persistent shape

| ID | Capability | Architecture doc | ADR | Domain object | Database object | Status |
|---|---|---|---|---|---|---|
| BC-01 | Campaign authoring | 10 §1.1, §8; 29 §5.2 | 0002 (RLS), 0012 (brand≠tenant) | `bonus.Campaign` (incl. `fulfillment_owner`, `fulfillment_destination`, budget cap field) | `bonus_campaigns` + RLS (dual-scope, nullable `tenant_id`) | NOT IMPLEMENTED |
| BC-02 | Offer authoring + immutable versioning | 10 §1.1, §2, §T.2 | 0021 (rounding inputs) | `bonus.Offer` (5 axes: Eligibility, Reward, Wagering, Payout, Abuse) | `bonus_offers` + RLS (append-only versions; FK → `bonus_campaigns`) | NOT IMPLEMENTED |
| BC-03 | Segment definition + membership resolution | 29 §3; 19 §9 | **new ADR required, §9 OI-3** | `segment.Segment`, `segment.Resolver` | `segments`, `segment_members` + RLS (dual-scope) | NOT IMPLEMENTED |
| BC-04 | Grant issuance (`issued`) | 10 §1.2, §1.3, §9, §T.2 | 0031 §15a-ii, 0032 §3.1, 0034 §1 | `bonus.Grant` (frozen: asset, exponent, Offer version, funding, destination, eligibility snapshot) | `bonus_grants` + RLS (dual `tenant_staff_scope` + `player_self_scope`) | NOT IMPLEMENTED |
| BC-05 | Grant activation (`activated`) | 10 §1.3, §T.3, §T.12 | 0031, 0032 §3.1, 0034, 0037 §C.2 | `Grant.Activate` transition | `bonus_grants` (status), `bonus_progress` (append) | NOT IMPLEMENTED |
| BC-06 | Coupon redemption | 10 B0 §1 item 5 | — | `bonus.Coupon` (a code + Offer reference; no new persistent layer) | column/unique index on `bonus_offers` **or** `bonus_coupons` — **OWED BY `bonus-engine`** | NOT IMPLEMENTED |
| BC-07 | Wagering split instruction (bet-time cash/bonus split) | 10 §6 item 1; ledger §6.4 case F | 0021 (DS-2 boundary), 0032 | `bonus.SplitInstruction` | none (computed, never stored) | NOT IMPLEMENTED — **cross-domain: requires a `casino`-owned change to `postBet`/`postWin`/`postRollback`, §4.3** |
| BC-08 | Wagering-progress derivation (`P_net`, `P_firm`) | ledger §6.6.3–§6.6.6; 10 §1.1 | 0032 §0 | `bonus.Progress` (two derived measures, never stored) | `bonus_contribution_records` (append-only, one row per (Grant, lock/bet transaction)) — **OWED BY `ledger-finance`** | NOT IMPLEMENTED |
| BC-09 | Progress nullification on void/rollback (gate G-3) | ledger §6.6.5, §6.6.7 | 0032 §7 | nullification predicate | none (derived over `ledger_transactions.reverses_transaction_id` + `correlation_id`) | NOT IMPLEMENTED |
| BC-10 | Completion (`completed`) | 10 §1.3, §9 | 0031 §15a-ii, 0032 §3.1 | `Grant.Complete` transition | `bonus_grants`, `bonus_progress` | NOT IMPLEMENTED |
| BC-11 | Conversion (`completed`→`converted`) | 10 §1.3, §T.12 | 0031 §16 (`bonus_conversion` Operation), 0032 §4 | `Grant.Convert` (max-cashout, cash/bonus payout ordering) | `bonus_grants`, `bonus_progress` | NOT IMPLEMENTED — **depends on ADR 0031 §16's six-step Risk extension, 0/6 complete** |
| BC-12 | Cashback window settlement | 10 §2, B0 §1 item 3 | 0021 (no residual accumulation) | cashback settlement job (`clock_timestamp()`) | `bonus_grants`, `bonus_progress` | NOT IMPLEMENTED |
| BC-13 | Expiry (`expired`) | 10 §1.3, §T.9 | 0032 §5, §3.1 | `Grant.Expire` | `bonus_grants`, `bonus_progress` | NOT IMPLEMENTED |
| BC-14 | Cancellation (`cancelled`) | 10 §1.3, §T.10 | 0032 §3.1 | `Grant.Cancel` (player opt-out / staff) | `bonus_grants`, `bonus_progress` | NOT IMPLEMENTED |
| BC-15 | Forfeiture (`forfeited`) | 10 §1.2, §1.4 | 0032 §5 | `Grant.Forfeit` (breach / review outcome; reason code mandatory) | `bonus_grants`, `bonus_progress` | NOT IMPLEMENTED |
| BC-16 | Reversal (`reversed`) | 10 §1.2, §9 | 0032 §7 | `Grant.Reverse` + tombstone for a never-seen original | `bonus_grants`, `bonus_progress` | NOT IMPLEMENTED |
| BC-17 | Abuse detector → manual-review queue | 10 §1.4, §4.1 rule 3 | 0031 §12 (no fake `count` LimitKind) | `bonus.ReviewItem` (queue entry; the Grant does **not** change state) | `bonus_review_queue` + RLS | NOT IMPLEMENTED |
| BC-18 | Staff manual grant / conversion override (four-eyes) | 10 §10; 29 §5.2–5.3 | 0024 (four-eyes precedent), 0013 (audit) | `bonus.ManualAction` + approval | `bonus_grants`, `bonus_progress`, approval rows — **OWED BY `security`** | NOT IMPLEMENTED |
| BC-19 | Player bonus read surface | 25 §1.1; 29 §5.1 | 0011 (player tokens) | read models over BC-04/08 | none (reads) | NOT IMPLEMENTED |
| BC-20 | Campaign financial reporting | 10 §6; ledger §2 | 0032 §2 | ledger-derived aggregates | none (derived; **no bonus-owned balance column**) | NOT IMPLEMENTED |
| BC-21 | Terminal-Grant late credit | 10 §T.7, §T.13 | **0039 Decision 2** | one of ACTION_REFORFEIT / ACTION_ROUTE_TO_CASH / ACTION_HOLD_FOR_REVIEW | depends on the selected action | **BLOCKED — unmade human decision; reachable in the casino-only B1 slice, §4.3** |
| BC-22 | Campaign budget-cap enforcement | 10 §1.1 (P2), Genuine gaps 7 | 0031 §15d (confirmed **outside** `risk.Evaluate`) | — | `bonus_campaigns.budget_cap` exists as an advisory field only | **NOT IMPLEMENTED — no mechanism, no owner (§4.1 item e)** |
| BC-23 | `RewardFulfiller` seam | 29 §6.3; 02 contract discipline; 21 | 0032 §6(c) | `bonus.RewardFulfiller`, `bonus.RewardDecision` | none | NOT IMPLEMENTED (interface + 1 impl) |
| BC-24 | `ActivityIngestor` seam | 29 §2.2, §6.4; 22 | — | `bonus.ActivityEvent`, `bonus.ActivityIngestor` | consumer watermark row — **OWED BY `bonus-engine`** | NOT IMPLEMENTED (interface + 1 impl) |

### 7.2 Table B — runtime surface and verification

| ID | Service (`internal/…`) | API | Canonical event | Ledger `transaction_type` | Audit `Action` | Test suite | Filled by |
|---|---|---|---|---|---|---|---|
| BC-01 | `bonus.CreateCampaign`, `bonus.DisableCampaign` | `POST/GET /v1/admin/bonus/campaigns`, `POST …/{id}/disable` | — | — | `bonus_campaign.created`, `bonus_campaign.disabled` | `bonus_campaign_test.go`; `httpserver/bonus_admin_*_test.go` | bonus-engine |
| BC-02 | `bonus.CreateOffer`, `bonus.CreateOfferVersion` | `POST/GET /v1/admin/bonus/offers`, `POST …/{id}/versions` | — | — | `bonus_offer.created`, `bonus_offer.version_created` | `bonus_offer_test.go` | bonus-engine |
| BC-03 | `segment.IsMember`, `segment.CreateSegment` | `GET/POST /v1/admin/segments` (minimal) | — | — | `segment.created`, `segment.disabled` | `segment/segment_integration_test.go` | architect + bonus-engine |
| BC-04 | `bonus.IssueGrant` | (system-triggered; visible via BC-19) | `bonus.grant.created` | — (ADR 0032 §3.1: a decision, not a movement) | `bonus_grant.issued`; on denial `bonus_grant.denied_by_rg` / `…_by_risk` / `…_by_asset_authorization` | `bonus_grant_integration_test.go`; `adversarial_grant_idempotency_test.go` | bonus-engine |
| BC-05 | `bonus.ActivateGrant` | `POST /v1/me/bonuses/{id}/activate` | **`bonus.grant.activated`** (NEW, §2.4) | `bonus_grant` (Dr `promo_liability` · Cr `player_bonus`) | `bonus_grant.activated` | `bonus_activation_integration_test.go`; RG/Risk/AssetAuth enforcement tests mirroring `casino/rg_enforcement_integration_test.go` | bonus-engine + ledger-finance |
| BC-06 | `bonus.RedeemCoupon` | `POST /v1/me/bonuses/coupons` | (folds into `bonus.grant.created`) | — | `bonus_coupon.redeemed`, `bonus_coupon.rejected` | `bonus_coupon_integration_test.go`; enumeration/rate-limit adversarial test | bonus-engine + security |
| BC-07 | `bonus.ComputeSplit` | — (internal) | (rides `casino.bet.settled`) | `casino_bet` (destination split) | `casino_bet.posted` (existing, metadata extended) | `bonus_split_test.go`; **cross-domain** `casino` regression suite | bonus-engine + ledger-finance + casino |
| BC-08 | `bonus.ProgressNet`, `bonus.ProgressFirm` | `GET /v1/me/bonuses/{id}` (`P_net` only) | **`bonus.grant.progress_changed`** (NEW) | — (derived) | — (Progress row, not an audit action) | `bonus_progress_integration_test.go`; property test `P_firm ≤ P_net` | ledger-finance + bonus-engine |
| BC-09 | nullification predicate in `bonus.ProgressNet` | — | (rides `casino.bet.rolled_back`) | `casino_rollback` | — | `adversarial_progress_farming_test.go` (gate G-3 regression) | ledger-finance |
| BC-10 | `bonus.EvaluateCompletion` | — | `bonus.grant.completed` | — (eligibility, not a movement) | `bonus_grant.completed` | `bonus_completion_integration_test.go`; concurrent double-completion race test | bonus-engine |
| BC-11 | `bonus.ConvertGrant` | `POST /v1/admin/bonus/grants/{id}/convert` (override only) | **`bonus.grant.converted`** (NEW) | `bonus_conversion` (Dr `player_bonus` · Cr `player_cash` · Dr `bonus_expense` · Cr `promo_liability`) | `bonus_grant.converted`, `bonus_grant.conversion_overridden` | `bonus_conversion_integration_test.go`; conversion-denial-leaves-`completed` test | bonus-engine + ledger-finance + risk |
| BC-12 | `bonus.SettleCashbackWindow` | — (scheduled) | `bonus.grant.completed` then `…converted` | `bonus_grant` / `bonus_conversion` per ADR 0032 §3.1 | `bonus_cashback.settled` | `bonus_cashback_integration_test.go`; `clock_timestamp()` regression test | bonus-engine + ledger-finance |
| BC-13 | `bonus.ExpireGrant` | — (scheduled) | `bonus.grant.expired` | `bonus_forfeiture` | `bonus_grant.expired` | `bonus_expiry_integration_test.go` | bonus-engine + ledger-finance |
| BC-14 | `bonus.CancelGrant` | `POST /v1/me/bonuses/{id}/cancel`, `POST /v1/admin/bonus/grants/{id}/cancel` | **`bonus.grant.cancelled`** (NEW — D-1 split) | `bonus_forfeiture` | `bonus_grant.cancelled` | `bonus_cancellation_integration_test.go` | bonus-engine + ledger-finance |
| BC-15 | `bonus.ForfeitGrant` | `POST /v1/admin/bonus/grants/{id}/forfeit` | **`bonus.grant.forfeited`** (NEW — D-2) | `bonus_forfeiture` | `bonus_grant.forfeited` (reason code mandatory) | `bonus_forfeiture_integration_test.go` | bonus-engine + ledger-finance |
| BC-16 | `bonus.ReverseGrant` | — (system) | `bonus.grant.reversed` | `bonus_reversal` | `bonus_grant.reversed`, `bonus_grant.reversal_tombstoned` | `bonus_reversal_integration_test.go`; never-seen-original tombstone test | bonus-engine + ledger-finance |
| BC-17 | `bonus.RouteToReview`, `bonus.ResolveReview` | `GET/POST /v1/admin/bonus/review-queue…` | — | — | `bonus_review.opened`, `bonus_review.resolved` | `bonus_review_queue_integration_test.go` | bonus-engine + risk |
| BC-18 | `bonus.ManualGrant`, `bonus.ApproveManualAction` | `POST /v1/admin/players/{id}/bonuses` etc. | `bonus.grant.created` (`trigger_type = staff`) | per the underlying transition | `bonus_grant.manual_issued`, `bonus_manual_action.approved` | `bonus_four_eyes_integration_test.go`; self-approval-rejection test mirroring `stage3c_self_approval_test.go` | security + bonus-engine |
| BC-19 | `bonus.ListGrantsForPlayer`, `bonus.GetGrant` | `GET /v1/me/bonuses…`, `GET /v1/admin/players/{id}/bonuses` | — | — | — (reads) | `httpserver/bonus_flow_integration_test.go`; cross-tenant + cross-player RLS tests | bonus-engine + security |
| BC-20 | `bonus.CampaignSummary` | `GET /v1/admin/bonus/campaigns/{id}/summary` | — | — | — (reads) | `bonus_reporting_integration_test.go`; ledger-vs-report reconciliation test | ledger-finance |
| BC-21 | **undecided** | — | — | `bonus_forfeiture` (a) / settlement-destination substitution (b) / hold (c) | `bonus_grant.terminal_credit_*` | `adversarial_terminal_grant_test.go` | **BLOCKED on ADR 0039 Decision 2** |
| BC-22 | — | — | — | — | — | — | **unassigned (§4.1 item e)** |
| BC-23 | `bonus.RewardFulfiller` | — | reserved: **`bonus.reward.requested`** | via the underlying transition | — | interface-conformance test, one implementation | architect + bonus-engine |
| BC-24 | `bonus.ActivityIngestor` | — | consumes §2.3's six types | — | — | `bonus_ingest_integration_test.go`; redelivery/dedupe-on-`idempotency_key` test | architect + bonus-engine |

---

## 8. Architectural invariants (for `qa` and `code-reviewer` to verify)

`architect`'s testing responsibility is to specify invariants, not tests.
`qa` owns the strategy and the gate; `code-reviewer` treats a violation
as a blocking finding. Each invariant below is mechanically checkable.

| ID | Invariant | Check |
|---|---|---|
| **BI-1** | Every `bonus_*` and `segment*` table has `FORCE ROW LEVEL SECURITY`; every `tenant_staff_scope` policy carries the `app.player_account_id IS NULL` conjunct; every player-readable table has a **SELECT-only** `player_self_scope` policy | Schema inspection + cross-tenant and cross-player RLS integration tests. This is the exact gap `code-reviewer` found on `self_exclusion_enumeration_runs` in Stage 4H-B0-R6 (finding F2) |
| **BI-2** | `internal/bonus` is never imported by `internal/ledger`, `internal/wallet`, `internal/risk`, `internal/rg`, `internal/payments`. **`internal/casino` importing it is permitted** — see the correction note below this table | Import inspection, the same technique doc 02 uses for the risk/rg separation |
| **BI-16** *(new, Stage 4H-B1 Wave 3 Phase 10, `architect`)* | Every table `internal/bonus` reads that belongs to **another domain's Go package** is declared, by name and by the columns read, in this row. Currently exactly one: **`deposit_intents`** (owned by `internal/payments`), read by `deposit_sweep.go` for `brand_id`, `player_account_id`, `wallet_id`, `asset_code`, `amount`, `payment_method` and joined on `ledger_transaction_id`/`tenant_id`. (`ledger_transactions`/`ledger_entries`/`ledger_accounts`, `player_accounts`, `wallets`, `staff_users`, `assets`, `jurisdictions`, `tenants`, `economic_operations` and `audit_log` are platform-shared or already-established reads, not peer-domain reads, and are out of this row's scope) | A test asserting the set of non-`bonus_*` tables appearing in `internal/bonus`'s SQL equals the declared set — the same mechanical shape as BI-15's import inspection, one level down. **Why this exists:** the deposit sweep satisfied the *letter* of the transport design (no call into `internal/payments`) by taking a **schema-level** dependency on it instead, which the compiler cannot see and a payments migration can break silently at runtime. That is an acceptable trade — `ledger_transactions` alone carries no `payment_method`, which the deposit-method eligibility axis needs — but an **undeclared** one is not. `payments` owns `deposit_intents` and must treat these six columns as a published interface |
| **BI-3** | No `bonus_*` table stores a balance. Every player-visible bonus *balance* is derived from `ledger_entries`. (`bonus_grants.granted_amount` is an immutable computation input, not a balance — the distinguishing test is whether any code path ever `UPDATE`s it) | Schema inspection + grep for `UPDATE bonus_` on amount columns |
| **BI-4** | Wagering progress is never a stored counter. `bonus_progress` and `bonus_contribution_records` are append-only; no `UPDATE`/`DELETE` | Trigger/constraint inspection, mirroring `audit_log`'s enforcement (ADR 0013) |
| **BI-5** | Every Grant state transition appends a `bonus_progress` row **in the same transaction**. No transition path exists without one | Code review of every transition + a test asserting Progress-row count equals transition count |
| **BI-6** | `pg_advisory_xact_lock` keyed on **`(tenant_id, grant_id)`** is acquired before any transition that can race, before the idempotency check, not after | Mutation test: remove the lock and the concurrent-completion test must fail (the `internal/risk` TOCTOU precedent from Stage 4H-B0-R6's fix wave) |
| **BI-7** | Composition order at every value-creating checkpoint is `AssetAuthorization.CheckEligibility` → `rg.EvaluateEligibility` → `risk.Evaluate`, all three inside the same transaction as the effect, all three fail-closed (non-nil error ⇒ deny, no fallback to a previously-known-good answer) | Code review + fail-closed integration tests mirroring `internal/risk/fail_closed_integration_test.go` |
| **BI-8** | A denial at **conversion** time leaves the Grant in `completed` (non-terminal, retryable) and **never** forfeits or cancels | Dedicated test per denying source (RG, Risk, AssetAuthorization) |
| **BI-9** | Only **`P_firm`** authorizes a conversion. `P_net` is display-only | Test: a Grant whose `P_net` meets the target but whose bonus-origin locked exposure is non-zero must not convert |
| **BI-10** | Every financial write's idempotency is enforced by a **database** unique constraint, never check-then-insert | Schema inspection + redelivery tests per doc 10 §9's three keys |
| **BI-11** | No brand-name, tenant-name or jurisdiction-code literal appears in a Bonus code path. Jurisdiction and brand variation are configuration rows | grep + code review (`CLAUDE.md` multi-tenancy; doc 10 §7) |
| **BI-12** | No bonus posting occurs while HR-9's fail-closed guard (`assertNoBonusSetEntries`) is still in place. Its removal is conjunctive on both `bonus_expense` existing **and** the Rule B2 (extended) mirror generator existing | Sequencing gate, `ledger-finance`-owned (doc 10 B0 §3 item 3a) |
| **BI-13** | Rule B2 holds without exception: every `player_bonus` entry has an equal, opposite `promo_liability` entry in the **same** `LedgerTransaction`; Invariant B1 nets to zero per `(tenant_id, asset_code)` at every instant, no tolerance band | `ledger-finance`-owned invariant sweep — `ledger-finance` has final authority here, not `architect` |
| **BI-14** | No B1 artifact references a mission, tournament, level, badge, streak, point or marketplace item (§6.6) | grep across `internal/bonus`, migrations, route table |
| **BI-15** | `internal/bonus` does not import `internal/eventbus` (§2.2) | Import inspection |

**BI-2 correction note (Stage 4H-B1 Wave 3 Phase 10, `architect`).**
BI-2's prior parenthetical conditioned the `casino → bonus` import on
"ADR 0039 Decision 2 selecting ACTION_ROUTE_TO_CASH". That condition is
stale and, read literally, marks shipped code as a violation of an
invariant it does not actually violate — so it is corrected here rather
than left to be rediscovered by every future reviewer. Two things
changed under it:

1. **Decision 2 was not resolved by selecting one option; the design
   stopped needing it to be.** Wave 2 built all three named actions
   (`ACTION_REFORFEIT`, `ACTION_ROUTE_TO_CASH`, `ACTION_HOLD_FOR_REVIEW`)
   as staff-resolvable `bonus_held_dispositions` outcomes, so the code
   selects nothing — a human does, per disposition, under four-eyes. The
   import exists to support the holding representation itself,
   **whichever** option is eventually chosen. Decision 2 remains an
   **open Human Decision Register item** and nothing here resolves it
   (BC-21 stays `BLOCKED`); only the import's stated precondition is
   corrected.
2. **Stage 4H-B1 Wave 3 added a `casino → bonus` call site that has
   nothing to do with G-2 at all**: `postBet`'s cash-funded
   wagering-contribution trigger (`ledger-accounting-model.md`
   §7.18.3.3), which must run in the same transaction as the bet posting
   (HR-10) and therefore cannot be inverted. §7.18.1 states the position
   this note adopts: a `casino → bonus` call is architecturally
   unremarkable and requires no exception. BI-2's real content — the
   **one-way** rule that `ledger`, `wallet`, `risk`, `rg` and `payments`
   never depend on Bonus — is unchanged and still holds.

---

## 9. Open items for the reconciliation round

| ID | Item | Owner | Blocking? |
|---|---|---|---|
| **OI-1** | Confirm the `bonus-finance` roster gap was genuinely covered — all seven concerns in §4.1 have a named owner and a filled §7 row. Item (e), campaign budget-cap enforcement, is **currently unassigned** | Orchestrator | Yes, for the gate |
| **OI-2** | Amend `22-canonical-activity-event-taxonomy.md`: add `bonus.grant.activated`, `…progress_changed`, `…converted`, `…cancelled`, `…forfeited`, `bonus.reward.requested`; split the `cancelled`/`reversed` conflation (defect D-1); record the `round.settled`/`session.started` reconciliation (§2.3) | Master Orchestrator (doc 22's owner), `architect` reviews | Yes — event names must be fixed before any producer is written |
| **OI-3** | Ratify §3's segmentation placement; add the `internal/segment` row to `docs/governance/ownership.md`; record the decision as an ADR (number to be assigned by the Orchestrator to avoid a parallel-dispatch collision) | Orchestrator + `product-owner-proxy` | Yes |
| **OI-4** | Reflect §4.2's `operation = wagering` decision into ADR 0037 §C.2's operation list and strike doc 10's Genuine-gaps item 1 | `architect` (self), on reconciliation | No |
| **OI-5** | **§4.3's P1**: correct ADR 0039 Decision 2's scope ("sportsbook-only" → also reachable casino-only), and file the `casino`-owned dependency request for funding-source-aware `postWin`/`postRollback` destination resolution (`ledger-accounting-model.md` §6.4 case F is designed; `postWin` hardcodes `player_cash`) | `ledger-finance` (ADR 0039) + `casino` (code) + Orchestrator (human decision) | **Yes — B1 blocker** |
| **OI-6** | ADR 0031 §16's six-step `bonus_conversion` Risk extension: 0/6 complete as of the last verification. Must land before BC-11's write path is wired | `risk`, via `bonus-engine`'s dependency request | Yes, for BC-11 |
| **OI-7** | Confirm the `player_locked_cash`/`player_locked_bonus` dependency status. Migration 0048 landed cash-only; doc 10's Dependency Contract Freeze §9 marks Bonus's dependency **provisional**. B1 is casino-only, so this should be a no-op for B1 — needs explicit confirmation, not assumption | `ledger-finance` | No, if confirmed |
| **OI-8** | Confirm whether any of the five in-slice bonus types needs a KYC gate (ADR 0034 §6's `awaiting_verification` status), and if so at which checkpoint | `identity-compliance` | Unknown until answered |
| **OI-9** | Ratify §5.3's `bonus:adjust` / `bonus:review` permissions and the `RolePromotionsManager` requirement; own the coupon-endpoint enumeration/rate-limit discipline (BC-06) | `security` | Yes, for BC-06/BC-18 |
| **OI-10** | Confirm §5.4's requirement that a platform-wide Campaign creation attempt is explicitly **rejected** (no platform-scoped write path exists for any domain) rather than silently accepted | `architect` + `security` | No |
| **OI-11** | Ratify §2.2's transport decision (ledger-derived + in-process adapter, no broker) — it touches doc 22's ownership and should be recorded as an ADR at reconciliation | Orchestrator | Yes |
| **OI-12** | Confirm whether a **cash reward (no wagering requirement)** is in or out of the first slice. Doc 10 B0 §1 deliberately left it out because `product-owner-proxy` did not name it; it is fully specified by ADR 0032 §3. A one-line scope decision for the human, never an assumption | Orchestrator / human | No |

---

## 10. Cross-references

- Bonus lifecycle, type matrix, Terminal-Grant contract:
  `10-bonus-engine-architecture.md` (`bonus-engine`)
- Bonus accounting, posting map: `docs/decisions/0032-bonus-accounting.md`
  (`ledger-finance`)
- Locked-origin split, cases A–L, Model C wagering-progress integrity:
  `ledger-accounting-model.md` §6.3–§6.6 (`ledger-finance`)
- Risk contract and the `bonus_conversion` extension:
  `docs/decisions/0031-risk-and-limits-engine.md` §12–§18 (`risk`)
- RG/KYC integration: `docs/decisions/0034-bonus-gamification-rg-kyc-
  identity-integration.md` (`identity-compliance`)
- Sportsbook accounting (not in B1's path, but shares G-2/G-3):
  `docs/decisions/0038-sportsbook-accounting-and-ledger-integration.md`
- Unmade human decisions: `docs/decisions/0039-human-decision-register-
  stage-4h-b0-r7.md`
- Event taxonomy: `22-canonical-activity-event-taxonomy.md` (Master
  Orchestrator)
- Reward Orchestrator and `RewardDecision`:
  `21-reward-orchestration-architecture.md` (Master Orchestrator)
- API conventions: `04-api-architecture.md`,
  `25-bonus-gamification-api-architecture.md` (`backend-platform`)
- Domain boundaries: `02-domain-and-service-boundaries.md` (`architect`)
- Asset authorization: `docs/decisions/0037-asset-currency-registry-and-
  fx-conversion-architecture.md` (`architect`)
- Ownership and dependency-request procedure:
  `docs/governance/ownership.md`, `docs/governance/integration-
  protocol.md`

## 11. Ownership

Owned by `architect`. §1, §2, §3, §4.2, §4.3, §5, §6, §7's structure,
§8 and §9 are `architect`'s own content. Every cell in §7 marked
`OWED BY` belongs to the named specialist and is filled in the
reconciliation round. Nothing in this document authorizes writing
`internal/bonus`, `internal/segment`, a migration, a route, or a test.
