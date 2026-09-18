# Testing Strategy

Status: Stage 0 draft. Owned by the `qa` specialist going forward. A
feature is not complete because it compiles, the server starts, or a
happy-path request works — per `CLAUDE.md`.

## Baseline requirement (every non-trivial change)

- Unit tests for business logic.
- Integration tests for API endpoints.
- Authorization tests (a request without the right role/scope must be
  rejected).
- Tenant-isolation tests (a valid token for tenant A must never read or
  write tenant B's data) for anything touching tenant-scoped data.

## Financial functionality (wallet, ledger, payments, bonus postings) — mandatory full matrix

Per `CLAUDE.md`, no financial code is done without tests covering:

1. Normal transactions.
2. Duplicate/replayed requests (idempotency).
3. Concurrent requests against the same account (race conditions must not
   corrupt the balance).
4. Retries (provider retries the same `provider_tx_id` — must be a no-op
   returning the same result).
5. Partial failures (network drop after write, before response).
6. Rollback, including rollback of a transaction never seen (tombstone
   behavior) and rollback of a transaction seen (compensating entry).
7. Settlement (including partial settlement for sportsbook).
8. Reconciliation (recomputed balance vs. projection; drift detection).
9. Provider callbacks (the actual inbound bet/win/rollback/balance path).
10. Idempotency under concurrency, not just sequentially.
11. Authorization on financial endpoints.
12. Auditability (every financial mutation produces a traceable audit
    record).

## Provider integrations

Adapter tests against sandbox/mock providers covering: happy path, timeout
+ retry, malformed/unexpected response shapes, and state-machine
transition correctness (pending → settled/reversed, never skipping or
reversing invalid transitions).

## End-to-end coverage

Critical flows get end-to-end tests once the relevant services exist:
registration → KYC tier gate → deposit → bet/spin → withdrawal;
self-exclusion → subsequent login/play attempt blocked across brands;
bonus grant → wagering progress → payout or forfeiture.

## Quality gates

- CI must fail the build on any failing test — `devops` owns verifying the
  pipeline actually enforces this rather than swallowing failures.
- No test is skipped, disabled, or quietly quarantined to unblock a
  merge; if that's ever genuinely necessary, it is a recorded decision via
  the orchestrator, not a silent QA call.
- `code-reviewer` checks that claimed test coverage actually exercises the
  failure modes it claims to, not just that tests exist.

## Test reporting standard (Stage 4G-FINAL)

Every completion report's test section states, per test suite/command
run, one of exactly five results — never a blanket "all clean" when any
suite actually flaked or failed:

- **PASS** — ran, every test passed, no flake observed.
- **FAIL** — ran, at least one test failed and the failure is real
  (reproducible, not investigated-and-dismissed as infrastructure noise).
  A stage is not complete with an unresolved FAIL on anything the stage
  touched.
- **FLAKE** — ran, failed at least once, but investigated and determined
  to be genuine non-determinism (not a correctness defect) — the report
  states the evidence for that conclusion (reproduction rate, isolated
  re-run results, root-cause mechanism if known), never just the label.
  A FLAKE the investigation cannot explain is a FAIL, not a FLAKE.
- **NOT RUN** — the suite exists but was not executed this stage (e.g. a
  suite gated on infrastructure unavailable in this environment) — the
  report states why.
- **BLOCKED** — the suite could not run due to an environment/dependency
  problem outside the change being validated (e.g. the database was
  unreachable) — distinct from NOT RUN in that it was ATTEMPTED and
  failed to execute at all, not skipped by choice.

For every suite, the report states: the exact command, the result, the
specific failure/flake if any, whether it blocks stage completion, and
the reason for that blocking determination. See any Stage 4G-FINAL-or-
later completion report's own test matrix for the applied format.

## What "done" requires

Every deliverable is labeled one of `IMPLEMENTED`, `PARTIALLY
IMPLEMENTED`, `MOCK`, `STUB`, `PROVIDER DEPENDENT`, `NOT IMPLEMENTED`,
`BLOCKED` — `qa` sign-off is what allows a label to move to `IMPLEMENTED`.

## Stage 4H-B0 — Retail, Hierarchy/RBAC, RG-Bypass and Bonus Test Strategy

Status: `RECOMMENDATION`, `NOT IMPLEMENTED`. Architecture/test-strategy
only — no test code exists for anything in this section, and none of it
binds until the corresponding domain (retail hierarchy — `architect`;
retail accounting — `ledger-finance`, ADR 0035; RBAC — `security`, ADR
0036; payments — `payments`) is itself authorized for implementation.
Owned by `qa` per `docs/governance/ownership.md`'s "Testing strategy"
row. This section is written against the general architectural patterns
this platform has already established and proven (tenant-isolation-by-RLS,
the `postBet` advisory-lock + `clock_timestamp()` self-exclusion race
fix, ADR 0020's idempotency mechanism, ADR 0032's testing-floor format) —
it does **not** assume any specific shape for the retail hierarchy or RBAC
designs still being produced in parallel this wave, and is explicitly
provisional on those designs actually supporting the test classes named
below (see "Testability gaps in parallel designs" at the end of this
section, carried in the QA report rather than duplicated here).

### 1. Retail financial testing floor

Mirrors ADR 0032's testing-floor format (CLAUDE.md's financial test list,
specialized for retail). Non-negotiable once retail ledger postings exist;
a happy-path-only retail test suite does not satisfy this, exactly as a
happy-path bonus grant test does not satisfy ADR 0032's floor.

1. **Agent float funding/replenishment — normal transaction.** An upstream
   node (operator, super-agent, or the platform itself) funds an agent's
   float; the posting balances per CLAUDE.md's ledger invariants and the
   agent's available float increases by exactly the funded amount.
2. **Concurrent funding requests to the same node (race).** Two funding
   requests for the same agent float arrive concurrently (from two staff
   sessions, or a retry racing the original) — the resulting balance must
   equal the sum of exactly the requests that were genuinely distinct, with
   no lost update and no double-count. Modeled on the same class of test as
   `internal/casino`'s concurrent-bet stress tests; requires the retail
   posting path to take the same balance-row lock discipline ADR 0020
   already mandates for wallet balances.
3. **Duplicate cashier confirmation (idempotency).** A cashier's
   confirmation of a retail deposit/withdrawal is retried (double
   click, network retry, terminal resubmission) — the retried request is a
   no-op returning the original result, enforced by a DB-level unique
   constraint on the operation's idempotency key, never "check then
   insert" application logic (CLAUDE.md, ADR 0020). A same-key,
   different-amount retry is rejected, not silently applied.
4. **Cashier confirms a deposit that never reaches the ledger (partial
   failure).** The cashier-side confirmation succeeds but the ledger write
   fails or the connection drops mid-transaction — the system must not
   leave the player credited at the cashier terminal without a
   corresponding ledger entry, and must not leave a dangling ledger entry
   with no corresponding cashier confirmation. Test both directions of the
   gap explicitly; whichever side is authoritative (almost certainly the
   ledger, per CLAUDE.md's "the authoritative balance read happens inside
   the same database transaction as the write") must be stated and tested
   as such, not assumed.
5. **Commission calculation and settlement.** Normal calculation (correct
   rate, correct rounding — `NUMERIC`, never floating point, per
   CLAUDE.md and ADR 0032 §9's identical rule for bonus rates), commission
   settlement as its own auditable posting, and a test that commission
   figures are recomputable from stored inputs (mirrors ADR 0032 §9's
   reconciliation-by-recomputation requirement).
6. **Retail withdrawal exceeding an agent's available float.** **Resolved
   (Wave-2 review correction, F14, P3)**: an earlier draft of this item
   left the reject-vs-draw-upstream question open. `docs/decisions/0035-
   retail-agent-network-accounting.md` §1.4 has since decided it: the
   debit is **rejected**, not clamped, and there is no implicit upstream
   draw. This test is therefore **required**, not blocked: assert a clean
   deny with no partial posting when a withdrawal would exceed the
   node's own available float.
7. **Reconciliation drift detection.** The new retail reconciliation
   stream ledger-finance is designing (analogous to ADR 0032's Invariant
   B1 stream) needs its own hourly, zero-tolerance drift test:
   recomputed float/commission balances vs. their projections, with any
   non-zero drift treated as P1 per CLAUDE.md. A test asserting the
   reconciliation job actually runs and actually alerts on injected drift
   (not just that the SQL is correct) is required — the same "assert the
   job fires, not just that the query is right" lesson this platform's
   hourly ledger-vs-projection sweep already established.
8. **Reversal/rollback of a retail transaction.** Follows the established
   pattern (ADR 0032 §7, `internal/casino`'s `postRollback`): a
   compensating transaction with `reverses_transaction_id`, never an edit
   or deletion; double-reversal protection under concurrency (`FOR UPDATE`
   row lock, mirroring the casino rollback race fix); rollback of a
   transaction never seen writes a tombstone so a late-arriving original
   is rejected (CLAUDE.md, ADR 0032 §7); rollback of a partially-consumed
   retail credit (e.g. an agent float already re-lent downstream) must
   fail loudly rather than create a negative float, mirroring ADR 0032
   §7's "reversal of a partly consumed grant must fail" rule.
9. **Cross-hierarchy-node isolation.** See its own subsection (§2) —
   called out here too because it is also, structurally, a financial-data
   test: an Agent must never be able to read or act on a sibling Agent's
   float, transactions, or commission figures.

### 2. Cross-hierarchy-node isolation — the retail-domain equivalent of tenant isolation

This is flagged as the single most important **new** isolation class this
stage introduces, and is treated with the same weight CLAUDE.md gives
tenant isolation — not a lesser, "authorization test" checkbox item.

- **Baseline adversarial test, required, not optional**: a valid
  credential scoped to Agent/Node A must never be able to read or write
  Node B's float, transactions, commission, or player data, where B is a
  sibling, a cousin, or any node not in A's own subtree — including
  attempts via direct object reference (an id for B's resource supplied to
  an endpoint authenticated as A), not just the UI's own navigation
  paths. Mirrors this platform's own tenant-isolation baseline ("a valid
  token for tenant A must never read or write tenant B's data") applied
  one level down inside a single tenant's retail hierarchy.
- **Enforcement mechanism must be tested, not just its outcome.** Per
  CLAUDE.md's "enforced by ... RLS bound to a connection-level setting —
  not by discipline in application code" principle, if the hierarchy
  isolation is (or should be) enforced at the database layer via an
  analogous connection-scoped setting, the test suite needs the same class
  of test `TestRiskRules_PlayerScopeConnectionCannotReadOrWrite` and
  `TestRiskRules_CrossTenantDisableDenied` already established for
  `risk_rules`: a raw query issued under Node A's scope against Node B's
  row, proving the database itself refuses it, independent of application
  code remembering to filter. If instead isolation is enforced only in
  application code, that is itself a P1 finding for `qa` to raise (the
  same class of gap CLAUDE.md's RLS rule exists to prevent) — not
  something this document can silently accept as adequate this early.
- **A node acting on itself as if it were an ancestor** (privilege
  escalation upward — e.g. an Agent attempting an operation reserved for
  its parent Super-Agent or the operator) is a distinct test from sibling
  isolation and must be tested separately: "not my subtree" and "above my
  own position" are different failure modes with potentially different
  root causes in the authorization code.
- **Node reassignment does not leak historical access.** If a node is
  moved to a new parent, a test must confirm the node's *old* ancestors
  lose access to its *post-move* data (not just that the new ancestors
  gain it) — a lingering read grant from a stale parent relationship is
  the retail-hierarchy analogue of a tenant-migration data leak.

### 3. Hierarchy/RBAC testing

- **Subtree scoping, happy path and adversarial, both required.** A node
  can see and act on its own subtree (happy path) — but per CLAUDE.md's
  "No fake completion" rule, a happy-path-only test suite for this is
  explicitly insufficient. The required adversarial companion: an attempt
  to act *outside* the caller's subtree is rejected, tested against every
  mutating and every read endpoint the hierarchy exposes, not sampled.
- **The recursive-subtree-scoping mechanism needs its own concurrency/
  correctness test class**, regardless of which concrete mechanism
  `architect`/`security`'s parallel documents settle on (closure table,
  materialized path, recursive CTE over an adjacency list, or otherwise):
  1. **Node reassignment mid-query.** A node is reassigned to a new parent
     while a subtree-scoped authorization check or a subtree-scoped report
     query is in flight. The test asserts the in-flight operation resolves
     to a single, well-defined, non-corrupted answer (either the pre-move
     or post-move subtree, explicitly whichever the design commits to —
     never a half-applied mix of both from reading a partially-updated
     path/closure structure mid-transaction). This is this stage's
     hierarchy-domain instance of the same lesson the `now()` vs
     `clock_timestamp()` and the concurrent-rollback advisory-lock fixes
     already taught this platform: a mechanism that is correct in a
     single-threaded read is not automatically correct against a
     concurrent write to the structure it's reading.
  2. **Concurrent reassignment race.** Two staff sessions attempt to
     reassign the same node to two different new parents concurrently —
     exactly one must win, deterministically and auditably (an audit
     record naming both the attempted and the actual resulting parent),
     never a silently-inconsistent structure (e.g. a closure table with
     rows reflecting neither parent cleanly).
  3. **Cycle prevention under concurrency.** Reassigning node A under
     descendant B (creating a cycle) must be rejected — including when two
     concurrent reassignments would only jointly create a cycle (A→B and,
     concurrently, B's new-parent chain closing back to A), which a
     check-then-write cycle check evaluated non-atomically would miss.
- **Authorization tests remain mandatory per the baseline requirement**
  (this document's own "Baseline requirement" section) for every retail
  endpoint independent of the hierarchy-specific tests above — role
  possession (does this staff role have the retail permission at all) and
  subtree scope (does it apply to *this* node) are two different checks
  and both need their own test, since a design could get one right and the
  other wrong.

### 4. RG/self-exclusion bypass-through-retail testing — required, highest severity

This is the single highest-severity test class this stage's directive
identifies (hard requirement #16), and it is stated here as a concrete,
**required** test, not a gestured-at concern — it blocks retail
sign-off whenever retail deposit/withdrawal implementation is authorized,
regardless of who implements it.

**Required scenario**: a player self-excludes via the online platform
while a retail cashier transaction (deposit or withdrawal) for that same
player is concurrently in flight at a physical location. This is the
retail-path instance of the exact race this platform already discovered
and fixed once, at Stage 4G-FINAL, in the online path
(`TestEvaluateEligibility_DetectsSelfExclusionCommittedAfterTransactionBegan`,
root cause: `internal/rg.EvaluateEligibility` read Postgres `now()`
— frozen at transaction start — instead of `clock_timestamp()` —
re-evaluated per call — so a transaction already queued behind
`rg.lockPerson`'s advisory lock could miss a self-exclusion that
committed while it waited).

The retail path introduces a **new, distinct way to reach the identical
race**, because it is a second, physically separate origination point for
a transaction against the same player, with its own transaction timing,
its own possible queueing/latency (a cashier terminal, not a player's own
browser), and — per §2 above — its own node-scoped authorization layer
sitting in front of the RG check, which must not short-circuit RG
evaluation or evaluate it against stale state cached at the retail
terminal or node level.

Required test (naming suggested, not binding): a
`TestConcurrent_RetailCashierTransactionDuringConcurrentSelfExclusion`-
style adversarial test, structured identically to the online-path
regression test above:

1. Begin a retail cashier transaction (deposit or withdrawal) for player P,
   held at the point just after RG eligibility would ordinarily be
   evaluated (or, for a true concurrency test, racing under
   `rg.lockPerson`'s advisory lock exactly as the original online-path
   test does).
2. Commit a self-exclusion for player P from a separate, concurrent
   session.
3. Assert the retail transaction's RG evaluation — evaluated with
   `clock_timestamp()` semantics, not a value frozen before the
   self-exclusion committed — denies it, and that no ledger effect from
   the retail transaction is visible.
4. Assert the reverse ordering also holds (self-exclusion commits first,
   retail transaction starts after): denial is not merely a timing
   accident of one interleaving.

This test is **required** for retail deposit/withdrawal to be marked
`IMPLEMENTED` under this document's "What 'done' requires" section — a
retail RG path that has not been proven against this specific race,
however plausible its code looks, remains `PARTIALLY IMPLEMENTED` at
best, per CLAUDE.md's "No fake completion" rule. It further requires the
retail transaction path to call `rg.EvaluateEligibility` (or its retail
equivalent) inside the same database transaction as its own
state-changing effect, before that effect commits — the same positional
contract ADR 0031 §14 already imposes on Risk, and the online path
already proves is the only correctness boundary for this exact class of
race. Whether retail should evaluate RG via the existing
`internal/rg.EvaluateEligibility` directly or through a retail-specific
call site is an implementation detail for whichever domain builds the
retail path — but it must be the **same authority**, per CLAUDE.md's
"Enforcement ... is our code, not the vendor's" and ADR 0031 §1's
"`internal/rg.EvaluateEligibility` remains the sole authority" — a
second, retail-local self-exclusion check would be exactly the "second
financial/compliance truth system" failure ADR 0032 §0 already named and
rejected for bonus accounting, applied here to RG.

### 5. Bonus/Gamification test strategy — proportionate to what actually ships

Per this stage's own product-owner-proxy finding (Stage 4H-A Wave 2) that
Gamification's build may be deferred to a small MVP-only Bonus Engine
slice, the test strategy is stated proportionately rather than designed in
full for a domain that may not ship this decade.

- **Bonus Engine (whatever slice is actually authorized first)**: ADR
  0032's existing testing floor (grant/convert/forfeit/reverse happy
  paths; duplicate and replayed grants; same-key-different-amount
  rejection; concurrent grants to one wallet; concurrent conversion vs.
  bonus-funded bet on one wallet; conversion exceeding balance; reversal
  of a partly consumed grant, must fail; double reversal race; reversal of
  a never-seen grant, tombstone then late-original rejection; rollback of
  a bonus-funded bet restoring all four accounts; provider-funded
  recognition landing in `provider_payable` and never `bonus_expense`; an
  externally-fulfilled reward posting zero ledger entries; Invariant B1
  asserted after every one of the above) **applies unchanged and in full**
  to whatever bonus types actually ship in the first slice, whether that
  is the entire ADR 0032 surface or a deliberately narrowed MVP subset
  (e.g. operator-funded deposit-match bonuses only, no provider-funded or
  externally-fulfilled path yet). It is not repeated here in full; ADR
  0032's list is the authoritative floor and this document defers to it
  rather than maintaining a second copy that could drift.
- **A narrowed first slice narrows which rows of ADR 0032's table apply,
  never which tests are required for the rows that do.** If the first
  slice ships only operator-funded grant/wagering/conversion/forfeiture,
  the provider-funded and externally-fulfilled test rows are simply not
  yet applicable (`NOT IMPLEMENTED`, not skipped-and-forgotten) — they
  become required the moment that scope is added, not before.
- **Gamification-specific test design is explicitly deferred.** No test
  strategy, test class, or test scenario is designed in this section (or
  anywhere else in this document) for tournaments, missions, the reward
  marketplace, points/XP/levels, or the Reward Orchestrator. Per
  `product-owner-proxy`'s finding that the Blueprint never mentions any of
  this domain and its build is not currently authorized, designing tests
  for it now would itself be the kind of speculative, unauthorized scope
  expansion CLAUDE.md's "No uncontrolled scope expansion" section warns
  against — applied to test design, not just production code. This
  deferral is revisited only if/when Gamification implementation is
  separately authorized.

### 6. Status and labels

Every item in this section is `RECOMMENDATION` (a test strategy binds
nothing until executed) and `NOT IMPLEMENTED` (no test code exists yet).
None of it can move to `IMPLEMENTED` — for the retail floor, hierarchy/
RBAC, or the RG-bypass test — before the corresponding architecture (ADR
0035, ADR 0036, the retail hierarchy design) is itself frozen and
authorized for implementation, and this document's test classes are
re-checked against that design's actual mechanism (closure table vs.
materialized path vs. recursive CTE; how float is represented in the
ledger; how the retail RBAC scope is carried on a request) rather than
assumed to fit it. Item 1.6 (withdrawal exceeding float) is explicitly
`OPEN DECISION`, referred to `architect`/`ledger-finance`, and its test
cannot be written until that decision is made.

## Stage 4H-B0-R5 — Testability review of five Stage 4H-B0-R4 P1s (`qa`, independent, first look)

`RECOMMENDATION`, documentation-only — no test code written here. This is
`qa`'s first, independent review of ADR 0037 (FX/asset-authorization), ADR
0038 §14 (idempotency), `ledger-accounting-model.md` §6.3 (origin split),
and ADR 0034 §14 (self-exclusion open-bet policy), none of which `qa`
authored or previously reviewed, per CLAUDE.md's "no specialist
self-approves its own work." Findings below are testability gaps only —
they do not re-litigate financial/authorization correctness, which is
`ledger-finance`/`security`/`architect`'s domain.

### P1-1 — FX rate-plausibility (ADR 0037 §B.6/§B.7): testable-as-specified, with one named gap

The `FXRateProvider` interface (B.2) exposes `Rate` as a plain struct
(`value`, `precision`, `as_of`, `provider_reference`) returned directly by
`GetCurrentRate`/`GetHistoricalRate`, and `HealthCheck` as a separate,
independently-controllable method. This means a hand-written mock
implementing the three-method interface can deterministically construct
every one of B.6/B.7's category-A conditions (zero rate, negative rate,
future `as_of`, missing/zero-value `as_of`, insufficient `precision`
relative to a fixed destination-asset `decimal_exponent`, a failing
`HealthCheck`) and both category-B conditions (B.7.2's magnitude
plausibility, by controlling what `GetHistoricalRate` returns relative to
`GetCurrentRate`; B.7.3's provider disagreement, by registering two mock
providers with independently controlled `HealthCheck`/`GetCurrentRate`
results) purely through the documented interface — no real-provider
behavior is required to be simulated. This is a materially better
position than most provider-abstraction ADRs on this platform start from.

**Named gap 1 — no FX adapter conformance-suite requirement exists.** ADR
0038 §14.5 requires, for the analogous sportsbook adapter obligation
(`occurrence_ordinal` composition), that "a conformance-suite check for
whichever adapter is built first ... must verify it before that adapter is
marked complete." ADR 0037 states an equivalent adapter obligation (a
provider binding must declare, as a one-time capability fact, whether it
issues `provider_reference` values — B.7.1's resolution of the "missing
provider reference" row) but never requires a conformance suite any real
`FXRateProvider` adapter must pass before being trusted with the fail-closed
contract. Without one, a mock that behaves correctly is not evidence a real
adapter will. **Required test-strategy addition**: an `FXRateProvider`
conformance suite (mirroring the sportsbook adapter conformance-suite
citation), covering at minimum: malformed-response handling surfaces
consistently (whether as an `error` return or a zero-valued `Rate` field —
B.2 does not say which, so the suite must pin one contract and every real
adapter must be tested against it), the capability declaration is queryable
before any quote is requested, and `GetHistoricalRate`'s "not supported"
error is distinguishable from a zero/failed rate rather than silently
approximated.

**Named gap 2 — the provider-reference-capability declaration has no
documented storage/query shape.** B.7.1 states an `FXRateProvider` binding
"declares... whether it issues `provider_reference` values at all," but
B.2's interface has no method for it, and Part A/B name no config table for
it either. A test fixture cannot construct this declaration without
inventing a shape the architecture doesn't specify. Flagged for
`architect`/`ledger-finance` to close before implementation, not resolved
here.

Everything else in B.6/B.7 is testable-as-specified; no other gap found.

### P1-2 — Asset Authorization RBAC (ADR 0037 Part C/§C.5): missing-test-hooks — layers 4 and 6 are not independently distinguishable

`AssetAuthorization.CheckEligibility` (§C.2) is specified to return a
"specific, distinguishable `ReasonCode`" identifying *which layer* failed,
and A.3's AND-chain lists tenant-authorization (layer 4) and
jurisdiction-authorization (layer 6) as two separate, sequentially
evaluated gates with brand-authorization (layer 5) between them. Layers 1,
2, 3, 5, and 7 are each independently testable in isolation: for each,
a fixture can hold every other layer passing and make exactly that one
layer fail, then assert the returned `ReasonCode` names that layer — this
works cleanly because each of those layers has its own distinct storage
fact (`assets.active`, `assets.platform_authorized`, a brand-scoped
allow-list row, an `AssetOperationEligibility` row).

**Layers 4 and 6 do not have this property, and the architecture says so
itself.** A.5 states plainly: "Layers 4 and 6 (tenant and jurisdiction
authorization) are the same underlying mechanism, not two separate ones,
because `tenant_jurisdiction_configs` is already keyed on `(tenant_id,
jurisdiction_id)` — a row's presence and its `allowed_currencies` array
jointly answer [both questions] in one place." Given that, a single query
against one `(tenant_id, jurisdiction_id)` row cannot, by itself, produce
"tenant-authorized but jurisdiction-unauthorized" or the reverse — the
row's presence/content answers both simultaneously. No mechanism is
documented (a secondary query, e.g. "does this tenant have an authorized
row for *any* jurisdiction, to decide whether to blame layer 4 or layer 6
specifically") for `CheckEligibility` to produce a `ReasonCode`
distinguishing a layer-4 failure from a layer-6 failure on this same
underlying fact. **Concretely: a test asserting "layer 4 passes, layer 6
independently fails" (or vice versa) cannot be constructed against the
design as documented**, which contradicts §C.2's own claim that the
chain's seven layers are each independently distinguishable failure points.
This is either (a) an acceptable, deliberate collapse that §C.2's
"distinguishable per layer" language should be corrected to reflect (e.g.
one combined `ReasonCode` for "tenant/jurisdiction not authorized for this
pair"), or (b) evidence that a second, tenant-scoped-only query is needed
and simply wasn't specified. Either way it needs an explicit answer before
implementation, not an assumption at test-writing time. Flagged for
`architect`/`security` to resolve; not a `qa` call.

Everything else in Part C — the fail-closed-on-absent-configuration
default (C.1), the non-nil-error-is-always-ineligible rule (C.2, directly
testable via a fault-injected data-access layer, mirroring the existing
`risk.Evaluate` precedent), the C.5.1 four-eyes/dual-control matrix (one
test per catalogued operation asserting required-vs-not), the C.5.4
immutable-field rule (testable as "no operation accepts these fields after
creation"), and the C.5.5 forced-defaults-on-creation rule — are testable
as specified, each with a clear, isolable fixture.

### P1-3 — Idempotency design (ADR 0038 §14/§14.6): testable-as-specified for the mechanisms named; missing-test-hooks for one adversarial case

The worked retry example (§14.6) is sufficient to derive a concrete
sequential-retry test case (same `idempotency_key`, same payload → original
result returned, no second row; a hypothetically differing payload →
`ErrIdempotencyKeyReused`, named explicitly). The true-concurrency case
(two connections racing the same composed key) is **not** re-derived from
scratch in §14 — it doesn't need to be, because §14 states this mechanism
is "unchanged" from ADR 0020, and ADR 0020's own "Concurrent-duplicate
behavior" section already specifies the exact arbitration mechanism
(Postgres serializes on the unique index; the loser's `SAVEPOINT`
rollback-and-lookup path is spelled out precisely) and already states, in
its own Consequences section, that "test coverage for every flow... must
include: exact retry, same-key-different-payload, concurrent-duplicate,
and a genuine concurrency race (two goroutines/connections)." So the
concurrent case for §14's composed keys is testable by applying an already
-specified, already-required pattern to a new key shape — not a gap, just
not re-stated in §14 itself.

**Named gap — no adversarial test for out-of-order/gapped
`occurrence_ordinal` delivery is named anywhere.** §14.1 requires
`occurrence_ordinal` be derived from something intrinsic to the specific
occurrence, explicitly never inferred by "counting existing rows and adding
one," specifically so it survives redelivery and non-sequential arrival.
But no worked example or named test case anywhere in ADR 0038 exercises
*why* that matters: e.g., two genuinely distinct partial-settlement
occurrences whose ordinals are non-adjacent (ordinal 1 then ordinal 5, with
2–4 never posted because they belong to lifecycle events that never
occurred or were themselves deduplicated away) or delivered **out of
order** (ordinal 5's event reaches the ledger before ordinal 1's, e.g. due
to provider redelivery/network reordering). Nothing in the ledger posting
layer is documented to depend on ordinal contiguity or arrival order —
each composed `provider_tx_id`/`idempotency_key` is independently unique —
so this is very likely a non-issue in practice, but that equivalence is
never stated explicitly, and ADR 0038 has no "Tests" checklist for §14 at
all (contrast `ledger-accounting-model.md` §6.3.4 item 7, which names an
explicit, enumerated test list for the origin-split proposal). **Required
test-strategy addition**: (1) a named test asserting two occurrences with
non-adjacent ordinals both post independently and correctly with no gap
-related validation error; (2) a named test asserting out-of-order delivery
(higher ordinal's event processed before a lower ordinal's) produces the
same two independently-correct postings, order-independent — both derived
from, but not stated in, §14.1's own reasoning.

### P1-4 — `player_locked` origin-split (`ledger-accounting-model.md` §6.3): testable-as-specified — no gap found

The worked cases (C-void, C-loss, C-win, C-partial) are written as literal
debit/credit tables with exact amounts and running B1 balances after each
step (§6.3.3.2) — these translate directly into fixture-and-assertion test
cases with no interpretation required. §6.3.4's implementation checklist
already names, as item 7, the exact `GetSummary` regression test this
review was asked to check for: "a regression test asserting
`GetSummary().LockedBalance` is non-zero and correct when a wallet holds
**both** `player_locked_cash` and `player_locked_bonus` (item 1's defect,
caught by a test rather than by a player)" — alongside mixed-funded
lock/void/loss/win/partial-settlement-ratio-survival/rollback tests, the
`bonus_share == 0` degenerate no-zero-entry case, B1 (extended) asserted
after every transaction, Rule B2 (extended)'s no-mirror-on-lock /
one-pair-per-crossing assertion, and repetition on an 18-exponent asset.
This is the one P1 of the five where the architecture already anticipates
and names its own required tests to the same standard `qa` would otherwise
have to add. No addition made here. Two items remain correctly gated as
`OPEN QUESTION`/unreviewed (case C-win's proportional-payout rule, case
C-cashout) and are correctly marked untestable-as-final until resolved —
that is accurate self-disclosure, not a gap.

### P1-5 — `OpenBetSelfExclusionPolicy` (ADR 0034 §14): missing-test-hooks — two gaps

Both policy values are independently testable at the settlement/void
level: `SETTLE_NORMALLY` requires only asserting the existing settlement
path is unaffected plus one new `audit.Entry` (open→open, per §14.5);
`VOID_ON_SELF_EXCLUSION` reuses ADR 0038 §8.1's existing void posting shape
verbatim, so it inherits that shape's own tests plus the same new audit
assertion (open→void). Both are testable independently with no shared
fixture dependency.

**Named gap 1 — the jurisdiction-floor tighten-only rule (§14.2) does not
specify whether it is enforced at configuration-write time or at
resolution-read time, and the two require different tests.** §14.2 says "no
tenant or brand ... may configure `SETTLE_NORMALLY`" where the
jurisdiction floor is `VOID_ON_SELF_EXCLUSION` — phrasing that reads as a
write-time rejection — but also describes "resolution" falling back through
scopes, which reads as a read-time algorithm that would simply never select
a looser value regardless of what was written. A write-time-rejection
design needs a test asserting the write itself is refused; a read-time
-floor design needs a test asserting a successfully-written loosening
override is simply never the effective value. These are different code
paths and only one can be right. **Required test-strategy addition**: this
must be pinned to one mechanism before a test can be written; recorded here
as a named gap for `identity-compliance`/`architect` to resolve, not
decided by `qa`.

**Named gap 2 — the new self-exclusion-commit listener/enumeration step
(§14.5) has no named test case anywhere, despite ADR 0034 itself stating
this is "a genuinely new system behavior" with no prior precedent in this
codebase.** Unlike P1-4's §6.3.4 item 7, ADR 0034 §14 contains no
enumerated test list for this new component. At minimum the following are
implied by the architecture's own text but not named as tests anywhere:
(a) a multi-tenant/brand/jurisdiction fan-out case — one Person with open
bets under two tenants in two different jurisdictions, self-excludes once,
and each open bet independently resolves its own policy per its own bet
-level jurisdiction/tenant/brand scope (§14.4's own example, not yet a
test); (b) the effective-time rule (§14.3) — a policy version change
occurring *between* self-exclusion-commit and a `SETTLE_NORMALLY` bet's
eventual natural settlement must not retroactively change which version
governed that bet; (c) audit cardinality — zero, one, and many open bets at
the moment of a single self-exclusion commit each produce the correct
number of `audit.Entry` rows (one per bet, never one per event, per
§14.5). None of these currently exist as named test cases in this document
or in ADR 0034.

Every item in this section is `RECOMMENDATION` (a test strategy binds
nothing until executed) and `NOT IMPLEMENTED` (no test code exists yet).
None of it can move to `IMPLEMENTED` — for the retail floor, hierarchy/
RBAC, or the RG-bypass test — before the corresponding architecture (ADR
0035, ADR 0036, the retail hierarchy design) is itself frozen and
authorized for implementation, and this document's test classes are
re-checked against that design's actual mechanism (closure table vs.
materialized path vs. recursive CTE; how float is represented in the
ledger; how the retail RBAC scope is carried on a request) rather than
assumed to fit it. Item 1.6 (withdrawal exceeding float) is explicitly
`OPEN DECISION`, referred to `architect`/`ledger-finance`, and its test
cannot be written until that decision is made.

## Stage 4H-B1 — Bonus Engine test strategy (Wave 1: design/contract only)

Status: `RECOMMENDATION`, `NOT IMPLEMENTED`. Issued for Stage 4H-B1
("Bonus Engine Implementation"), Wave 1 — a design/contract dispatch, per
that stage's own gating rule: no `internal/bonus` code, no migration, and
no test code is authorized by this section. This is the test matrix Waves
2–7 are held to, and the floor Wave 7's dedicated abuse-testing dispatch
builds its actual test suite against. It is written against
`docs/architecture/10-bonus-engine-architecture.md` in full (§1–§10, the
"Bonus Dependency Contract Freeze," and the "Terminal-Grant Technical
Contract," §T.1–§T.13, cited below as "doc10") and
`docs/architecture/ledger-accounting-model.md` §6.3–§6.6 (cited as
"ledger-model"), using §6.5.8's ten-item migration-`0048` test set and
§6.6.9's eleven-property Model-C test set as the rigor template the stage
directive named. It supersedes nothing in this document's existing
Stage 4H-B0 §5 ("Bonus/Gamification test strategy — proportionate to what
actually ships") or ADR 0032's own testing floor — both remain the
authoritative baseline this section extends with the concurrency,
adversarial, property, RLS, and gating detail the B0 stage explicitly
deferred to "whatever slice is actually authorized first."

**Scope note, stated once rather than per test class below**: this section
is written against Stage 4H-B0's authorized first slice — deposit bonus,
reload bonus, cashback, generic wagering bonus, coupon; internal
fulfillment only; no free spins/free bets, no external bonus engine, no
cash reward, no mission/tournament/loyalty trigger (doc10, "Stage 4H-B0 —
MVP Implementation Scope Plan," §1). Test rows for anything outside that
slice are named where the architecture already specifies their shape (so a
later scope expansion does not have to re-derive the test), but are marked
`NOT APPLICABLE (out of first-slice scope)` rather than required for
Waves 2–7's own sign-off.

### 1. Concurrency/race test list

Every row's **mechanism** column cites the locking discipline doc10
already specifies (§9's `(tenant_id, grant_id)` advisory-lock family, or
the specific idempotency key for the operation) — no new locking primitive
is invented here. Every row's **invariant** column is the exact property a
test must assert, not "the request succeeded."

| # | Scenario | Mechanism under test | Invariant that must hold |
|---|---|---|---|
| C1 | Two redeliveries/near-simultaneous triggers of the same qualifying `deposit.settled` event, both attempting to issue a Grant for the same `(tenant_id, campaign_id, offer_version_id, player_account_id, trigger_reference)` | Grant-issuance idempotency key (doc10 §9, first bullet) | Exactly one `bonus_grants` row is created; the loser's attempt returns the original Grant, not an error and not a second row; no double `bonus_grant` ledger posting results even if both racers proceed to activation |
| C2 | Two concurrent activation triggers for the *same* Grant (e.g. a redelivered no-opt-in `deposit.settled` racing a duplicate delivery, or racing an explicit staff manual-activation on the same Grant) | `(tenant_id, grant_id)` advisory lock (doc10 §9, extended by this document to activation — doc10's text names it explicitly only for "redemption/completion," so this row is this document's own extension, flagged in §6 below as needing `bonus-engine` confirmation before Wave 2 relies on it) | Exactly one `bonus_grant` posting (Dr `promo_liability`/Cr `player_bonus`) per Grant, ever; the loser observes the winner's already-`activated` state and performs no second RG/Risk/AssetAuthorization evaluation that could produce a second posting |
| C3 | Two concurrent activations across **different** Grants issued under the **same Campaign**, both counting against that Campaign's budget cap | None designed — doc10 §1.1/Genuine-gap-7 confirms campaign-budget-cap enforcement has **no owner and no mechanism** as of this stage | **`BLOCKED`, not `NOT APPLICABLE`**: this is the literal "two simultaneous campaign activations" scenario the stage directive names, and it cannot be written until a budget-cap enforcement mechanism exists. Recorded here so a future implementer does not assume the cap is race-safe because no test failed — no test can exist yet. Once designed, the required invariant is standard TOCTOU-safe: concurrent activations against a shared counter must never jointly exceed the cap, proven under a real concurrent stress run, not a two-goroutine happy path |
| C4 | Two concurrent triggers computing "this Grant's wagering multiplier is now satisfied," racing `completed→converted` for the same Grant | `(tenant_id, grant_id)` advisory lock (doc10 §9, second bullet, explicit) | Exactly one `bonus_conversion` posting per Grant; the loser's retry returns the original conversion result; Invariant B1 (ledger-model §6, `promo_liability` mirror) holds after either racer's view is read |
| C5 | A conversion attempt races a staff/player cancellation of the same, still-`completed` Grant | Same advisory lock as C4; both paths must acquire it before reading `G.status` | Exactly one of `{converted, cancelled}` is the final state; never both a `bonus_conversion` and a `bonus_forfeiture` posting for the same Grant; whichever loses observes the winner's terminal state and performs no posting of its own — this is the identical "read current status `FOR UPDATE` inside the lock, never assume a prior read" rule doc10 §T.11's table states for exactly this reason |
| C6 | A conversion attempt races the Grant's own automated time-limit expiry job for the same Grant | Same advisory lock as C4/C5 | Same invariant as C5, substituting `expired` for `cancelled` |
| C7 | A `round.settled`/`bet.settled` progress-contribution event races a `casino_rollback`/`sportsbook_rollback` of a **different** round under the same Grant, both mutating what the completion job would read as current progress | The Model-C progress derivation is read-time, not stored (ledger-model §6.6) — there is nothing to lock in the classical sense, but the completion job's read of "is progress ≥ target" must be a single, consistent snapshot, not two independent queries that could see different committed states | The completion decision is made from one consistent read of the netted progress derivation; a completion transition never fires on a progress value that a concurrently-committing rollback has already nullified, and never fails to fire once the true, fully-netted progress (post-rollback) has genuinely crossed the target — race resolves to exactly one correct answer, not a a stale one from either side |
| C8 | A `round.settled`/`bet.settled` progress-contribution event races the **void** of the very same bet it is trying to count | Same read-consistency requirement as C7, narrower: this is the "lock-then-void" cycle ledger-model §6.6.10 names as the anti-structuring case | The netted progress derivation must reflect the void (`q_eff = 0` per ledger-model §6.6.9 property 2) regardless of which of the two events' postings the completion job's read happens to observe first — i.e., the race must never let a voided bet's contribution count even transiently in a way that authorizes a conversion before the void's effect is visible |
| C9 | Two identical, concurrently-delivered provider callbacks reporting the same external-Grant status change (external-fulfillment path) | `(grant_id, completion_trigger_reference)` idempotency key (doc10 §9, third bullet, adapted) | Exactly one Progress append and one lifecycle-event emission (if any — `inside_provider` Grants emit none per doc10 §3.2/Dependency Contract §10); the second racer is a no-op. **`NOT APPLICABLE` for Waves 2–7's own sign-off** per this section's scope note (external fulfillment is out of the first slice) — retained here because the mechanism is identical to C1/C9's in-house analogue and should not be re-derived when external fulfillment ships |
| C10 | Two duplicate `deposit.settled` events (genuine provider retry, not a distinct deposit) delivered concurrently | Same key as C1 | No second Grant; no second `bonus_grant` posting if the deposit also triggers no-opt-in activation |
| C11 | Duplicate sportsbook/casino activity events (redelivered `bet.settled`/`round.settled` for the same occurrence) delivered concurrently | ADR 0038 §14's composed idempotency key / `occurrence_ordinal` discipline (doc10 Dependency Contract §6's binding dedupe rule: dedupe on `idempotency_key`, never `event_id`) | Progress increments by exactly the genuine occurrence's contribution once, never twice, regardless of delivery order or concurrency; a non-adjacent or out-of-order `occurrence_ordinal` delivery (ADR 0038 §14.1, testing-strategy P1-3's named gap above) still nets correctly |
| C12 | Two concurrent staff sessions attempt a manual grant-size override / manual conversion release on the same Grant, at least one exceeding the four-eyes threshold | doc10 §10's manual-adjustment audit row + CLAUDE.md's four-eyes-above-threshold rule (approval workflow itself is explicitly **not designed** by doc10 §10 — "flagged here as a requirement the backoffice/RBAC implementation stage must satisfy") | **`BLOCKED`** on the same undesigned mechanism as C3: the specific adversarial property that must eventually hold — a race can never let two different first-approvers each count as the other's second approver, and can never let two independent overrides both post — cannot be tested until the approval workflow exists. Recorded so it is required the moment that workflow lands, not discovered later |
| C13 | An RG self-exclusion commits for player P concurrently with an in-flight Grant activation/conversion for the same P | `rg.EvaluateEligibility` inside the same transaction as the Grant's state-changing effect, `clock_timestamp()` semantics, queued behind the identical class of advisory lock `TestEvaluateEligibility_DetectsSelfExclusionCommittedAfterTransactionBegan` already proves for casino (doc10 §5, Dependency Contract §4) | Structured identically to that existing regression test, retargeted at Bonus Engine's own checkpoints: assert denial regardless of which of the two orderings (self-exclusion-commits-first vs. Grant-transition-starts-first) actually occurs — a `TestConcurrent_BonusActivationDuringConcurrentSelfExclusion`-shaped test, required at both the activation and conversion checkpoints, is the Bonus-domain instance of this platform's own highest-precedent concurrency regression and must not be treated as "covered by the generic RG test suite" without its own Bonus-specific reproduction, exactly as retail required its own version (this document's Stage 4H-B0 §4) rather than inheriting the online-path test |
| C14 | A jurisdiction/tenant Risk rule change (a hard-limit tightening, e.g.) commits concurrently with an in-flight Grant activation/conversion for a player it would now deny | `risk.Evaluate` live, in-transaction, same composition point as C13 | Same shape as C13: the in-flight transition observes the rule as of `clock_timestamp()`-consistent read time, not a value frozen before the rule change committed; both orderings tested |
| C15 | An asset is deactivated (`assets.active = false`) concurrently with an in-flight activation/reward-credit/conversion attempt against a Grant denominated in that asset | `AssetAuthorization.CheckEligibility`, live, T.1's composition order (doc10 §T.3/§T.8/§T.12) | The value-creating transition observes the live authorization state at the moment of its own check — either it commits before the deactivation is visible (a legitimate race the deactivating actor accepts, per §T.5.1's asymmetry — deactivation is fail-closed going forward, not retroactive) or it is denied cleanly per §T.12's already-specified per-checkpoint consequence; it must never partially post (a `bonus_grant`/`bonus_conversion` posting with no corresponding Progress entry, or vice versa) |
| C16 | A settlement (WIN) or void/rollback credit against a lock-time debit under Grant `G` arrives concurrently with `G`'s **own** transition to a terminal state (expiry firing, staff cancellation, or a wagering-rule-breach forfeiture on a different bet) | The exact `(tenant_id, grant_id)` `FOR UPDATE` read doc10 §T.7 specifies, "read live... using the identical advisory lock doc10 §9 already specifies for Grant-completion races" | **`BLOCKED` on the G-2 human decision (doc10 §T.13)** — the mechanism to acquire a consistent read of `G.status` at the instant of the competing credit is fully specified, but which of `ACTION_REFORFEIT`/`ACTION_ROUTE_TO_CASH`/`ACTION_HOLD_FOR_REVIEW` fires is not selected, so the test's own assertion cannot be written. What **can** and must be tested before G-2 is resolved: that the race is detected and routed to *some* defined, non-silent outcome (i.e., the credit is never simply posted to `player_bonus` as if `G` were still non-terminal) — a placeholder assertion Wave 2's implementers should not skip merely because the final action is undecided |
| C17 | Conversion is in flight for Grant `G` when an upstream event reverses the original transaction that justified `G` (a deposit chargeback, or a casino/sportsbook rollback of a round `G`'s Progress had already credited) | `(grant_id, reversing_event_reference)` idempotency key (doc10 §9, "Reversal" bullet); the reversal and the conversion race for the same `(tenant_id, grant_id)` lock | Exactly one of the two effects (the conversion's `bonus_conversion` posting, or the reversal's compensating entries) is the one that lands first and is authoritative; the loser must not silently proceed as if the other had not happened — a converted Grant later discovered to rest on a reversed deposit needs its own compensating entry (ADR 0032 §7, not a re-opened conversion), and a reversal racing a not-yet-committed conversion must block that conversion rather than let both post independently |

**Stress-test requirement, not merely a two-goroutine happy path**: per this
platform's own established precedent (`docs/progress.md`'s casino
concurrency work — "8 goroutines, `-race`, proves exactly one of many
simultaneous [attempts] wins"), every row above marked with a real
mechanism (C1, C2, C4–C11, C13–C15, C17) requires, in addition to the exact
two-actor race, an N-way stress variant (minimum 8 concurrent goroutines
against a real Postgres instance, `-race` enabled) before it is credited as
tested — a two-goroutine pass proves the lock exists; it does not prove the
lock holds under real contention.

### 2. Adversarial abuse test list

Framed per the stage directive's own instruction: each row states what a
real exploit attempt looks like and the specific thing the test must prove
does **not** work — a passing happy-path test or "the code has coverage
here" is explicitly not sufficient evidence for any row below.

| # | Abuse vector | What a real attempt looks like | What the test must prove |
|---|---|---|---|
| A1 | Deposit/reversal farming | Player deposits just enough to trigger a deposit-match Grant, lets it activate (crediting `player_bonus`), then reverses the deposit (chargeback / payment-method dispute) after having already extracted wagering progress or converted value, repeating the cycle across payment methods/cards | The `reversed` transition (doc10 §1.3's "Any terminal state → `reversed`" row, ADR 0032 §7) always posts a compensating entry against whatever the Grant produced, even if the Grant has already converted — the test must show a player cannot net a real cash gain from a bonus whose triggering deposit was later reversed, across every terminal state the Grant could have reached before the reversal arrived (issued/activated/completed/converted), not only the simplest case |
| A2 | Void farming | Player repeatedly places a bonus-funded bet and voids it (or exploits a market that is frequently voided) purely to cycle wagering progress up with no genuine stake at risk | Per ledger-model §6.6.9 property 2 / §6.6.10: a full lock-then-void round-trip changes net progress by exactly zero, with **no residue**, and this holds under **unbounded repetition** — the test must run a real repeated cycle (not one iteration) and assert the netted progress after N cycles equals the netted progress after 1 cycle (i.e., strictly zero marginal gain per cycle), not merely that a single void nets correctly |
| A3 | Rollback farming | Same as A2 but via casino/sportsbook rollback of settled rounds rather than void, and — the platform-wide gap ledger-model §6.6.9 property 9 names — via a `casino_rollback` reversing a bonus-funded `casino_bet` specifically to test whether that channel (historically unbroken for cash, but not previously verified to net bonus progress) also nets to zero | Identical proof to A2, run through the rollback channel specifically, including the "improved" case ledger-model §6.6.9 item 9 flags as newly netting under Model C — this is a **regression test for a fix**, not merely a new-feature test, and must be labeled as such in the completion report |
| A4 | Repeated cashback claim | Player attempts to trigger the cashback settlement job's credit more than once per window (double-submitting a claim, or racing the scheduled job with a manual trigger if one exists) | Exactly one cashback credit posts per Grant per window, identified by an idempotency key scoped to `(grant_id, window)` (or equivalent) — the settlement job itself must be idempotent on redelivery/re-invocation, not merely "runs once by convention," mirroring the "assert the job fires, not just that the query is right" lesson this document's own Stage 4H-B0 §1.7 already states for retail reconciliation, applied here to the cashback job |
| A5 | Campaign/Offer version manipulation | Staff (or a compromised admin session) edits a live Campaign's Offer to a looser version (larger cap, lower wagering multiplier) attempting to retroactively improve the terms of an already-issued Grant, or a player attempts to reference a newer Offer version's id on an API call touching an older Grant | A Grant's every downstream computation (reward amount, wagering target, contribution %, payout ordering) reads only the immutable Offer version frozen at `issued` time (doc10 §1.1, §T.2, §T.11's table) — the test must edit the live Offer after Grant issuance and assert every subsequent computation for that Grant is provably unaffected, including an attempt to pass a different `offer_version_id` on a request touching that Grant, which must be rejected as a request/state mismatch, not silently substituted |
| A6 | Asset deactivation abuse | An insider (or a race won deliberately) times an asset deactivation/reactivation to influence which of the not-yet-decided G-2 actions a terminal-Grant credit receives, or to selectively freeze one player's Grant while leaving others unaffected for improper reasons | **Cannot be fully proven until G-2 (doc10 §T.13) is resolved** — recorded as a required test the moment it is. What can and must be tested now: §T.5.1's asymmetry itself is not exploitable in the *other* direction — i.e., an asset deactivation must never be usable to **block** a value-reducing transition (expiry/cancellation/forfeiture write-down), since §T.5.1 explicitly requires those to proceed ungated; a test forcing a deactivation immediately before an expiry/cancellation fires must show the write-down still posts |
| A7 | Grant duplication | Rapid-fire replay of the same qualifying deposit webhook (N requests, not 2) attempting to obtain N Grants from one qualifying event | Extension of C1/C10 to N ≥ 20 rapid, real (not simulated) concurrent deliveries against a real idempotency-keyed unique constraint — exactly one Grant results, proven by count, not by absence of an observed error |
| A8 | Identity duplication | Player re-registers under a new account (or uses the retail-channel registration path once it exists) to claim a first-deposit-only Offer more than once | This is explicitly a **dependency, not a Bonus Engine test**: doc10 §1.4 states device/payment-fingerprint linking is a Bonus-Engine-owned *detector*, but the underlying identity-graph fact ("this device/payment instrument is linked to N other accounts that already claimed this Offer") is not designed by doc10 at all — flagged in §6 below as a gap `bonus-engine`/`identity-compliance` must close before this row can be tested rather than assumed covered |
| A9 | Cross-brand abuse | A player active on two brands under the same tenant both claim a Campaign that was configured tenant-wide (intending one claim per player, not per brand-relationship) | Doc10 §8's "most-specific-row-wins, never a per-field merge" rule must be proven under attack: a brand-specific Campaign row replaces the tenant-wide one *entirely* for that brand; the test must show a tenant-wide Campaign's own eligibility axis (if configured "once per player" rather than "once per brand") is evaluated against the player's cross-brand history, not brand-scoped in isolation — this requires the eligibility axis to actually carry a "scope of uniqueness" concept, which is not confirmed to exist in doc10's five-axis model; flagged in §6 as needing confirmation before this row can be written as more than a negative/placeholder test |
| A10 | Cross-tenant abuse | Under the hybrid licensing model, a person with accounts under two tenants (one under-platform-licence, one own-licence) attempts to claim a platform-wide Campaign (`tenant_id IS NULL`) once per tenant relationship | RLS-level cross-tenant isolation (§4 below) proves a tenant A staff/player connection cannot read/write tenant B's Grant rows. The **business-logic** question — whether a platform-wide Campaign should be deduplicated across a single Person's multiple tenant relationships — is **not answered anywhere in doc10**; flagged in §6 as an open product/architecture question, not decided by this document, and this row cannot be marked more than `NOT APPLICABLE (open question)` until it is |
| A11 | Replay | An attacker captures and replays a previously-successful conversion (or activation) request verbatim, including any client-supplied identifiers | The full request replay must hit the same idempotency key as the original and return the original result with zero new ledger effect — this is C4/C9's mechanism restated as an explicit adversarial replay of a full request rather than a benign redelivery, and the test must include a **stale** replay (minutes/hours after the original, not immediately) to rule out a time-window-limited idempotency implementation |
| A12 | Amount/asset/player/provider tampering | A client-supplied field (amount, `asset_code`, `player_account_id`, `provider_id`/`provider_tx_id`) on any Bonus Engine-facing request is altered to a value inconsistent with the authenticated session or the upstream event | Every one of these fields must be proven to originate from server-side authenticated context or the trusted event-bus payload, never a client-supplied value that is merely validated against — mirroring CLAUDE.md's `tenant_id` rule extended to this domain's own money/identity fields; the test supplies a tampered value on an otherwise-valid, authenticated request and asserts the tampered value has **zero effect** on which account is credited/debited or by how much, not merely that the request is rejected (a request that is "rejected" but nonetheless used the tampered value for a partial side effect before rejecting would still fail this test) |
| A13 | Race-condition exploitation | An attacker deliberately automates a burst of concurrent requests (not 2 — a real burst, e.g. 50–100) at a known race window (activation, conversion, the reward-credit checkpoint) to try to win a double-credit through sheer volume rather than precise timing | This is §1's stress-test requirement (C1, C2, C4) run adversarially rather than as a correctness check — the test must show the *count* of resulting postings equals the count of genuinely distinct events regardless of burst size, and must be run at a burst size large enough to saturate the connection pool/lock queue, since a race that only manifests under real contention will not be caught by a burst too small to create contention |
| A14 | Micro-operation exploitation (structuring) | Player splits what would be one qualifying deposit/stake into many small ones to (a) exploit WP-1's rounding-structuring vector (ledger-model §6.6.10) if progress were ever rounded, (b) fall under a `min_amount`/`max_amount` transaction-window Risk rule's threshold repeatedly since `cumulative_amount`/`count`-shaped rules are **not enforced in the first slice** (doc10 §"Package/domain ownership," condition 1), or (c) claim a per-transaction bonus cap N times instead of once | WP-1 itself: prove the progress computation is genuinely never rounded (ledger-model §6.6.10's "the vector dissolves if the progress quantity is never rounded") — a structuring test that splits a stake into many small ones and asserts the summed progress exactly equals the single-stake progress, no more. For (b)/(c): this is an **accepted, documented first-slice gap**, not a test failure — the test suite must include a test that *demonstrates* the gap (a player can currently bypass a would-be cumulative/velocity cap by structuring) and the completion report must carry it as a named residual risk per this document's "No fake completion" standard, not omit it because "the architecture already said so" |

### 3. Property/invariant testing plan

Per test-writing discipline, not per architecture section — classifying
each of the seven named classes as property-based (generative, randomized
inputs asserting an invariant holds across the input space) vs.
example-based (fixed, hand-picked cases), and stating why:

| Class | Property-based or example-based | Reasoning |
|---|---|---|
| **Rounding** | **Both.** Property-based for the algorithm itself (`result = sign(x) × floor(\|x\| + 0.5)`, ADR 0021 DS-1) across randomized minor-unit amounts/rates/caps, asserting determinism (same input always yields same output) and the "round once, at the final monetary boundary" property (DS-2 — intermediate values carried at full precision never affect the final result differently than a single-shot computation would). Example-based for the named boundary cases: exact tie (`x.5`), zero, the cap exactly equal to the computed amount, and the negative-direction case if ever reachable | A rounding algorithm's correctness is a property over the whole input space; specific boundary values are exactly where hand-written examples catch what random generation under-samples |
| **Wagering progress** | **Property-based**, primarily, for the netting/idempotency guarantees ledger-model §6.6.9 states as eleven required properties — generate random orderings/interleavings of lock/settle/void/rollback events against a Grant and assert: `P_net ≤ P_firm` always; the derivation is order-independent for commuting operations; a full lock-then-void round trip nets to exactly zero regardless of position in the sequence (A2's property, generalized). **Example-based** for the twelve named worked cases (ledger-model §6.3.3.2's C-void/C-loss/C-win/C-partial, §6.6.7's cases 1–12) which exist precisely because they are the specific scenarios reviewers already reasoned through by hand and whose numeric answers are already known | The netting model is a genuinely generative space (arbitrary event orderings); the worked cases are the concrete, already-reviewed ground truth a generative test's output should never contradict — both are required, neither substitutes for the other |
| **Grant state transitions** | **Both.** Example-based (state-machine conformance): every row in doc10 §1.3's transition table is individually reachable, and every transition **not** in that table is rejected — this is naturally an exhaustive enumeration, not a generative one, since the state machine is small and finite. **Property-based** as a random-walk companion: drive a Grant through random sequences of triggers and assert two invariants hold at every step regardless of path taken — (a) the Grant never occupies two states at once and never regresses from a terminal state except via the single `→ reversed` transition, and (b) every transition produces exactly one Progress entry in the same transaction (§10.1's completeness requirement), with a strictly monotonic sequence number | The transition table itself is a closed enumeration best proven exhaustively; a random walk additionally catches an accidentally-reachable illegal transition that a hand-written test suite, which only tries transitions someone thought to write, would miss |
| **Idempotency** | **Both**, mirroring ADR 0020's own already-required list (cited by doc10's Dependency Contract §6 and by this document's existing financial-functionality baseline, item 2/4/10 above): example-based for exact retry, same-key-different-payload rejection, and the tombstone-then-late-original-rejected sequence; **property-based** for "N concurrent/redelivered/out-of-order copies of the same event, in any order and at any repetition count, always converge to exactly one effect" — randomizing delivery count, ordering, and timing | The specific named idempotency failure modes (ADR 0020's list) are known, finite scenarios; "any redelivery pattern converges to one effect" is precisely the kind of property that generative, randomized-ordering testing is suited to and example-based testing structurally under-samples |
| **Ledger conservation** (Invariant B1, and platform-wide `SUM(DEBITS) == SUM(CREDITS)`) | **Both, and mandatory after every other test in this entire matrix**, not a standalone class. Example-based: assert B1/SUM after each of the twelve worked cases and after every scenario in §1/§2 above (mirroring "Invariant B1 asserted after every one of the above," ADR 0032's own testing floor). **Property-based**: a fuzzer that drives a Grant through random, architecture-legal lifecycle sequences and asserts B1 holds after every single posting, not only at the end of the sequence | B1 is a zero-tolerance invariant (CLAUDE.md: "any non-zero drift is a P1 incident") — it must be checked continuously through a random sequence, not only at a final assertion, since an intermediate violation that self-corrects by the end would otherwise go undetected |
| **Cancellation/reversal** | **Both.** Example-based for the named required cases (reversal of a partly-consumed Grant fails loudly; double-reversal race is blocked; reversal of a never-seen Grant writes a tombstone and a late-arriving original is then rejected — mirroring the financial-functionality baseline's item 6 and doc10 §9's "Reversal" idempotency key). **Property-based** for "reversal delivered before vs. after the original is fully processed, in any interleaving, produces the same final ledger state" — randomizing the relative arrival order of an original event and its eventual reversal | The specific failure modes are known and must be individually proven; the order-independence property (a reversal genuinely arriving out of order relative to a slow-processing original) is exactly the class of bug advisory-lock/idempotency-key mechanisms are meant to prevent and benefits from randomized-ordering coverage |
| **Asset exponent handling** | **Example-based**, deliberately not generative — every monetary computation path (grant-amount rounding, wagering-target computation, per-game contribution split, conversion payout, forfeiture write-off) must be **explicitly run at exponents 0, 2, 6, 8, and 18**, not sampled randomly from the registry's 0–18 range | See exponent-coverage confirmation below — this is a fixed, named checklist, not a space to explore generatively; a random exponent generator could easily never select 0 or 18 (the two extremes where bugs actually concentrate — see below) across a finite run |

**Exponent coverage requirement, confirmed and extended, not merely
repeated.** The stage directive asks this document to confirm 0/2/6/8/18
decimal coverage "mirroring migration 0048's own test set." That mirror is
**deliberately not a full repetition**: migration 0048's own
implementation-status note (ledger-model §6.5, header) records that its
item 9 ("repeated on an 18-exponent and a 0-exponent asset") was executed
at exactly two exponents, 0 and 18, plus the pre-existing 2-exponent
default the rest of the suite already ran at — **6 and 8 were never
exercised**, despite being named in this document's own confirmation
requirement and being the exponents a live BTC/ETH-class asset would
actually use. Bonus Engine's own test set must not silently inherit that
gap: **every** listed monetary computation path is required at all five
named exponents (0, 2, 6, 8, 18), and a completion report that repeats
migration 0048's shortfall (covering only 0/2/18 and calling the exponent
requirement satisfied "by precedent") is itself a `FAIL` against this
document's reporting standard, not a partial pass.

### 4. RLS/tenancy test requirements

Every row below is a **direct SQL test** (a raw connection with the
relevant session-level scope set, issuing `SELECT`/`INSERT`/`UPDATE`/
`DELETE` directly against the table) — never an application-path-only test
— mirroring `internal/wallet/locked_origin_summary_integration_test.go`'s
`TestRLS_LockedSplitAccountsScopedToOwnerAndTenant` and
`internal/risk/risk_integration_test.go`'s
`TestRiskRules_PlayerScopeConnectionCannotReadOrWrite`/
`TestRiskRules_CrossTenantDisableDenied` naming and structural pattern.
Per CLAUDE.md, "enforced by PostgreSQL row-level security... not by
discipline in application code" — a suite that only ever exercises the
API layer cannot distinguish "the database enforces this" from "the
application always remembers to filter," which is precisely the gap this
document's Stage 4H-B0 §2 already refused to accept as adequate for retail
hierarchy isolation.

Scoped to the four new tables migration sequencing item names (doc10,
"Migration sequencing for the first slice ONLY"), in dependency order:

| Table | Scope shape (doc10 §8) | Required direct-SQL tests (naming suggested, not binding) |
|---|---|---|
| `bonus_campaigns` | Dual, nullable `tenant_id`, optionally narrowed by `brand_id`; a genuinely platform-wide row (`tenant_id IS NULL`) is a deliberate, by-design exception to per-tenant isolation | `TestRLS_BonusCampaignsScopedToOwnerTenant` — a tenant-A-scoped connection cannot read/write a tenant-B-scoped Campaign row; `TestRLS_BonusCampaignsPlatformWideReadableByAllTenants` — a platform-wide (`tenant_id IS NULL`) row **is** readable across tenant-scoped connections, proven as an intentional assertion, not an accidental leak (mirroring `TestRiskRules_CrossTenantDisableDenied`'s pattern of proving both the isolation and its documented exception); `TestRLS_BonusCampaignsBrandRowReplacesTenantRowEntirely` — a brand-specific Campaign row is read in place of (never merged with) a tenant-wide one for that brand, direct-SQL, not inferred from application resolution logic |
| `bonus_offers` | Inherits owning Campaign's scope, narrowing only | `TestRLS_BonusOffersInheritCampaignScope` — an Offer's effective scope under direct SQL matches its Campaign's, including the platform-wide/tenant-wide/brand-specific cases above; `TestRLS_BonusOffersCannotWidenCampaignScope` — an attempt to write an Offer row with a broader scope than its owning Campaign (e.g. brand-specific Campaign, tenant-wide Offer) is rejected by the database, not merely by application validation |
| `bonus_grants` | Always tenant + brand + player scoped; dual `tenant_staff_scope` + `player_self_scope` (read-only), mirroring `casino_launch_sessions`/`withdrawal_requests` (migration 0026) | `TestRLS_BonusGrantsScopedToOwnerAndTenant` (direct structural mirror of `TestRLS_LockedSplitAccountsScopedToOwnerAndTenant`) — a tenant-staff connection cannot read/write another tenant's Grant; `TestRLS_BonusGrantsPlayerScopeCannotReadOtherPlayer` — a player-self-scoped connection (session set to player A) cannot read player B's Grant row even under the identical tenant/brand, via direct object reference (a known Grant id for B supplied under A's session), not merely absent from a list endpoint; `TestRLS_BonusGrantsPlayerScopeIsReadOnly` — the player-self-scope policy permits `SELECT` and rejects `INSERT`/`UPDATE`/`DELETE` outright at the database layer |
| `bonus_progress` | Same scope as owning Grant; append-only (mirrors `audit_log`'s enforcement pattern) | `TestRLS_BonusProgressScopedToOwnerAndTenant` and `TestRLS_BonusProgressPlayerScopeCannotReadOtherPlayer` (identical shape to the Grant-level pair above); `TestRLS_BonusProgressAppendOnly` — a direct `UPDATE`/`DELETE` against an existing `bonus_progress` row fails at the database layer regardless of role, mirroring whatever mechanism `audit_log`'s own append-only enforcement uses (a `BEFORE UPDATE OR DELETE` trigger raising, or a `REVOKE` on those privileges from the application role) — the exact mechanism is `bonus-engine`'s implementation choice, but the test must prove the outcome at the SQL layer, not assume it from the table's intended design |

**Cross-player isolation, called out because it is not the same test as
cross-tenant isolation**: every Grant/Progress row test above must be run
twice — once proving tenant A cannot see tenant B (cross-tenant), and once
proving player X cannot see player Y **within the same tenant and brand**
(cross-player) — a design that gets tenant isolation right can still leak
between two players of the same tenant if the player-scope policy is
missing or malformed, and the two failure modes have historically had
different root causes on this platform (this document's own Stage 4H-B0
§2 makes the identical point for retail hierarchy nodes).

### 5. Testing gate checklist for Waves 2–7

**At every wave (2 through 6), before that wave's work is reported as
complete**, the following run cleanly, using this platform's own
established command set (`docs/progress.md`'s existing convention, not
invented here):

1. `gofmt -l .` — zero files listed.
2. `go build ./...` — clean.
3. `go vet ./...` and `go vet -tags=integration ./...` — clean.
4. `go test ./...` — unit suite, clean.
5. `go test -tags=integration ./...` — integration suite (against a real
   Postgres instance, per this platform's existing convention — never a
   mocked database standing in for RLS/constraint behavior), clean.
6. `go test -race ./...` and `go test -race -tags=integration ./...` —
   clean, for **every** package touched that wave, not only new packages —
   a race introduced in a shared helper (e.g. a change to
   `internal/wallet.GetSummary` for the bonus-balance fields) must be
   caught by the race detector run against the packages that already
   depend on it, not only against `internal/bonus` itself.
7. Every new test named in §1–§4 above that is in scope for that wave's
   own deliverable is present and passing — a wave that ships a table
   without its RLS direct-SQL tests, or a lifecycle transition without its
   concurrency-race counterpart from §1, is not reportable as complete for
   that deliverable, per this document's "What 'done' requires" section.

**The final gate (Wave 7, the dedicated abuse-testing dispatch), in full,
before Bonus Engine is eligible for a `qa` `IMPLEMENTED` sign-off on any
in-scope bonus type:**

1. The complete integration suite (`go test -tags=integration -count=1
   ./...`), full repository, not scoped to `internal/bonus` alone — a
   bonus-domain change that regresses casino/sportsbook/wallet/risk/rg
   behavior must be caught here.
2. The complete race suite (`go test -race -tags=integration ./...`),
   full repository, at least once with `-count=1` and once repeated (this
   platform's own precedent, `docs/progress.md`'s casino work, ran
   repeated `-race` passes specifically because a race that only manifests
   probabilistically can pass once and still be real).
3. Every RLS test from §4, run directly, all four tables, both
   cross-tenant and cross-player variants.
4. Migration round-trip: every migration in doc10's "Migration sequencing"
   list applies cleanly up, and — for the additive-widening migrations
   specifically (the `bonus_expense` account-type and
   `bonus_grant`/`bonus_conversion`/`bonus_forfeiture`/`bonus_reversal`
   transaction-type CHECK widenings) — the down-migration is rehearsed
   against a database already holding rows of the new types and is proven
   to fail loudly (a named `SQLSTATE 23514`, not a silent no-op), mirroring
   migration `0048`'s own rehearsed-down-migration requirement
   (ledger-model §6.5.8 item 7).
5. The complete adversarial abuse suite from §2, all fourteen rows, each
   either passing with the stated proof, or explicitly labeled
   `BLOCKED`/`NOT APPLICABLE (out of first-slice scope)`/`NOT APPLICABLE
   (open question)` per this document's own labels — never silently
   omitted.
6. The complete concurrency suite from §1, all seventeen rows, under the
   same labeling discipline, including the required N-way stress variants
   (minimum 8 concurrent goroutines, real Postgres, `-race`) for every row
   where a mechanism exists to stress.
7. Every financial invariant from CLAUDE.md's financial-testing list
   (normal transaction, duplicate, concurrent, retry, partial failure,
   rollback, settlement, reconciliation, provider callback, idempotency
   under concurrency, authorization, auditability) is proven for **every
   in-scope bonus type**, not once generically — a deposit bonus and a
   cashback bonus have different completion triggers (event-driven vs.
   settlement-job-driven) and neither's test coverage substitutes for the
   other's.
8. Idempotency, proven under concurrency (not only sequentially) for every
   named idempotency key in doc10 §9 (grant issuance, redemption/
   completion, reversal).
9. Multi-asset coverage: every test in §1/§2/§3 that involves a monetary
   computation is run against at least two distinct assets of different
   exponents, not only the platform's default asset, to catch an
   exponent-specific bug the default asset's exponent happens to mask.
10. The exponent-coverage matrix from §3 (0, 2, 6, 8, 18), confirmed
    complete — all five, not the 0/2/18 subset migration `0048` actually
    delivered — for every monetary computation path named in §3's table.

**What "0 failures, any skip explicitly justified" means operationally for
this stage**, per CLAUDE.md's "No fake completion" rule and this
document's own reporting standard (see "Test reporting standard" above):

- Every one of the ten final-gate items above, and every row of §1/§2/§4,
  gets its own line in the completion report using exactly one of
  `PASS`/`FAIL`/`FLAKE`/`NOT RUN`/`BLOCKED` — never a blanket "all tests
  pass" summary covering rows that were actually skipped, deferred, or
  never written.
- A row marked `NOT APPLICABLE (out of first-slice scope)` is not a
  "skip" requiring justification beyond citing the scope boundary that
  already excludes it (doc10's MVP scope plan) — but it must still be
  named, not omitted, so a future reader can distinguish "excluded by
  documented scope" from "forgotten."
- A row marked `BLOCKED` (C3, C12, C16, A6, and any row this document
  flags as pending an unresolved architecture gap) is **not** eligible to
  be silently treated as passing, skipped, or quarantined to ship the
  wave — per CLAUDE.md's authority split, `qa` can refuse sign-off on the
  affected bonus type's completion label but cannot itself decide to
  proceed without the blocked test; if shipping the first slice without
  resolving C3/C12/C16/A6 is genuinely necessary on a timeline basis, that
  is a decision for the orchestrator to make and record explicitly (per
  CLAUDE.md's "Cannot itself decide to skip... escalates to the
  orchestrator"), never a quiet QA call and never something Wave 7's
  report frames as "tested."
- A `FLAKE` label on any row in this matrix requires the same evidence
  standard this document already sets platform-wide (reproduction rate,
  isolated re-run results, root-cause mechanism) — and, specific to this
  domain, a flake on any concurrency-suite row (§1) or any test asserting
  Invariant B1 must be treated with elevated suspicion before being
  accepted as genuine non-determinism rather than a real race, given how
  many of this platform's own past "flakes" in adjacent domains
  (`docs/progress.md`'s `now()` vs. `clock_timestamp()` history) turned
  out to be real defects.
- The specific bonus type(s) actually shipped in Wave 7's report must be
  named individually with their own `IMPLEMENTED`/`PARTIALLY
  IMPLEMENTED`/etc. label (per this document's "What 'done' requires")
  — a report that labels "Bonus Engine" `IMPLEMENTED` as a single unit
  when, e.g., cashback's settlement-job-specific tests (A4, the cashback
  row of C-worked-cases) are `BLOCKED` or `NOT RUN` is exactly the kind of
  aggregation CLAUDE.md's "No fake completion" rule exists to prevent.

### 6. Dependencies on unfinished domain/ledger work — flagged, not resolved here

Per the dispatch's own instruction, named explicitly rather than silently
assumed resolved:

1. **G-2 (doc10 §T.13) is unresolved** and directly blocks C16, A6, and
   any test asserting a specific outcome for a settlement/void credit
   against an already-terminal Grant. The test *shape* is fully specified
   (§T.7); only the selected action is missing. Waves 2–7 must not invent
   an answer to unblock these tests — that is the orchestrator's decision
   to make or escalate, per CLAUDE.md.
2. **G-3 / Genuine gap 9's progress-netting fix (ledger-model §6.4.11,
   §6.6)** is design-complete (Model C, §6.6.9's eleven properties) but its
   own status line records it as gating **bonus-only** sportsbook
   placement specifically, and its "new Progress-trail trigger point" (a
   transition row for "previously-counted progress reversed") does not yet
   exist in doc10 §1.3's table — C7/C8/A2/A3 are written against the
   *design*, and cannot be executed until `bonus-engine`'s own transition
   table is updated to carry that row.
3. **`bonus_conversion`'s Risk `Operation` value is specified but not
   started** (doc10 Dependency Contract §3, "zero of six required
   extension-process steps are complete" as of the last verification) —
   C4/C5/C6/C14 and every conversion-checkpoint Risk test cannot execute
   against real code until this lands; this is a `risk`-owned dependency
   `bonus-engine` must file, not a Bonus Engine defect.
4. **`AssetAuthorization`/`internal/assetregistry` does not exist as Go
   code** (doc10 Dependency Contract §2, Genuine gap 2) — every test in
   this document referencing `AssetAuthorization.CheckEligibility` (C15,
   A6, T.3/T.8/T.12's checkpoints) is written against ADR 0037's
   architecture-only signature and must be re-verified against whatever
   Workstream A actually ships before Wave 2 relies on it.
5. **Which `AssetAuthorization.Operation` value a bonus checkpoint passes
   is undecided** (Genuine gap 1) — a candidate (`wagering`) is named but
   not committed; every AssetAuthorization-dependent test above is written
   generically ("the live authorization check") rather than pinned to a
   specific `Operation` constant, and must be updated once that decision
   lands.
6. **`player_locked_bonus` is schema-provisional, not human-approved**
   (doc10 Dependency Contract §9; ledger-model §6.3.5.1) — C7/C8/C16's
   locked-stake scenarios assume this account type exists and is postable;
   as of this stage it is schema-present but HR-9-blocked (no posting path
   exists at all), so these rows cannot be executed against real code until
   both the human approval and the Rule B2 (extended) mirror generator
   land (doc10 migration-sequencing item 3a).
7. **Campaign-level budget-cap enforcement has no design or owner**
   (doc10 §1.1, Genuine gap 7) — directly blocks C3, and any future test
   of a jurisdiction-scoped promotional cap under concurrency.
8. **The manual-adjustment four-eyes approval workflow is not designed**
   (doc10 §10) — directly blocks C12.
9. **Device/payment-fingerprint identity-linking (A8) and the
   "scope of uniqueness" concept an Offer's eligibility axis would need for
   A9's cross-brand test are not specified anywhere in doc10** — both are
   named as open items requiring `bonus-engine`/`identity-compliance`
   design work before their rows can be more than placeholders.
10. **A10's cross-tenant-per-Person deduplication question for
    platform-wide Campaigns is an open product/architecture question**, not
    named or resolved by doc10, ADR 0006, or ADR 0032.
11. **The event-taxonomy reconciliation gap (doc10 Genuine gap 3)** —
    doc10's own five named event-bus inputs are not yet confirmed against
    doc22's actual canonical `type` strings — means every test in §1/§2
    that subscribes to `deposit.settled`/`round.settled`/`bet.settled` is
    written against doc10's assumed names and must be re-verified against
    whatever `type` strings Wave 2's actual event subscription code uses.

Every item above is carried forward from already-approved architecture
documents, not newly discovered by this dispatch — this section exists so
Waves 2–7's implementers do not have to re-derive the dependency list from
the underlying architecture documents themselves.

### 7. Status and labels

Every test named in this section is `RECOMMENDATION` (a test strategy
binds nothing until executed) and `NOT IMPLEMENTED` (no test code exists
yet — this is a Wave 1 design/contract dispatch; §T.13, §6 above, and this
section's own header all restate that no schema, Go code, or test code is
authorized here). None of it can move toward `IMPLEMENTED` before the
corresponding Wave 2–6 implementation exists to test, and rows flagged
`BLOCKED` in §6 above cannot move to `IMPLEMENTED` before their named
architecture/product decision is made, regardless of implementation
progress elsewhere in Bonus Engine.

## Stage 4H-B1, Wave 1.5 — Cross-Domain Test Matrix (Commercial Ecosystem + Casino Win + Segmentation Architecture Reconciliation Gate)

Status: `RECOMMENDATION`, `NOT IMPLEMENTED`. Issued for the Wave 1.5
architecture-reconciliation gate (`docs/governance/task-registry.md`,
Stage 4H-B1 Wave 1.5 section; dispatch `4HB1W15-05`). **Design/strategy
only** — no `internal/segment`, `internal/crm`, `internal/affiliate`, or
bonus-funded-wagering code exists yet, and none is authorized by this
section. This section extends, and does not restate, the Stage 4H-B1 Wave
1 test matrix immediately above: the concurrency table's numbering
continues from **C18** (Wave 1 ended at C17), the adversarial-abuse
table's numbering continues from **A15** (Wave 1 ended at A14), and every
mechanism cited (advisory locks, idempotency keys, RLS dual-scope
patterns, the `(tenant_id, grant_id)`/`(tenant_id, provider_id,
provider_tx_id)` lock families) is the same one Wave 1 already specified —
no new locking primitive is invented here.

**What this section is written against, and what it is not**: casino's
own postWin financial-resolution design (`4HB1W15-01`), bonus-engine's
Grant terminal-state formal proof (`4HB1W15-02`, §A.9), and architect's
Segmentation/CRM/Affiliate architecture documents (`4HB1W15-03`) are
parallel Phase 1 dispatches not visible to this document at the time it
was written. Every item below that depends on one of those three is
explicitly flagged as provisional and re-verifiable, not silently assumed
correct — consistent with this document's own Wave 1 §6 practice of
naming dependencies rather than inventing answers to unresolved
architecture. §6 below (the severity framework) is the one part of this
section that does not depend on any of the three and can be applied
immediately to Phase 2's findings once they exist.

### 1. Casino postWin financial-resolution adversarial scenarios (C18–C27)

Per gate directive §A.10, formalized as test cases in the identical
"mechanism under test / invariant that must hold" shape Wave 1's C1–C17
table already established. These scenarios are the concrete adversarial
instances of the **G-2** gap the Wave 1 reconciliation escalated
(`docs/governance/task-registry.md`, Stage 4H-B1 reconciliation finding
1: "gate G-2 is reachable in the casino-only slice, not only sportsbook")
and of the Terminal-Grant Technical Contract's §T.7/§T.13 mechanism
(`10-bonus-engine-architecture.md`). **Every row's actual expected
posting/consequence is re-derived, not invented, from §T.7's already-
modeled data and the three named candidate actions — this table does not
select among ACTION_REFORFEIT/ACTION_ROUTE_TO_CASH/ACTION_HOLD_FOR_REVIEW,
exactly as §T.13 itself does not.**

| # | Scenario | Mechanism under test | Invariant that must hold |
|---|---|---|---|
| C18 | **Win after grant state change** — a WIN settlement for a bonus-funded, previously-locked stake arrives after Grant `G`'s own status changed (`activated`→`expired`/`cancelled`/`forfeited`) between lock and settlement | The `(tenant_id, grant_id)` `FOR UPDATE` read at settlement time, T.7's exact mechanism, same lock family as C4–C6/C16 | If `G` is still non-terminal at the live read, ordinary settlement proceeds and progress increments correctly (this branch is testable today). If `G` is terminal at the live read, the credit is never posted to `player_bonus` as though `G` were still active — **`BLOCKED` on G-2** for this branch only, identically to C16; what must be provable now is that the race is *detected* and routed to a defined, non-silent outcome |
| C19 | **Win after wagering completion** — a stake locked under `G` is still open when `G`'s wagering requirement is separately satisfied by other bets and `G` reaches `converted`; the locked stake's own WIN settlement arrives afterward (§T.7's "rare fourth entry path," explicitly folded into G-2, "not treated as a distinct case") | Identical to C18, with `G.status == converted` observed at the live read | The system must not silently double-count this stake as still contributing to an already-satisfied wagering requirement (no second `bonus_conversion` posting is ever produced) and must not credit `player_bonus` on a Grant with no further wagering use for it — **`BLOCKED` on G-2**, same action space as C18/C16, restated here because a `converted` Grant is easy to mis-treat as "no longer relevant" rather than as a fourth G-2 entry path |
| C20 | **Rollback after win** — a WIN settlement posts (crediting `player_bonus`/`player_cash` per the funding split), then a `casino_rollback`/`sportsbook_rollback` of that same round arrives afterward | Existing `postRollback` compensating-entry mechanism (ADR 0032 §7, ADR 0038), extended by the G-3 netting fix (`ledger-accounting-model.md` §6.4.11 V-1) to net the wagering-progress credit the WIN produced, not only the cash/bonus payout | The compensating entry restores both the ledger balance **and** the wagering-progress derivation to exactly their pre-WIN state — no residual progress credit survives the rollback of the WIN that produced it; Invariant B1 holds after the compensating entry. **`BLOCKED` on the G-3 netting-query fix landing** (Wave 1 §6, dependency item 2) for the bonus-funded case; unaffected and testable today for cash-only, and that cash-only case is a required regression test in its own right (mirrors A3's "this is a regression test for a fix" framing) |
| C21 | **Win after rollback (reordering/tombstone)** — the round's rollback is processed *before* its own original WIN settlement event (out-of-order delivery), so the rollback initially finds no original to reverse | Rollback-of-a-never-seen-original tombstone mechanism (ADR 0032 §7, mirrors `payments.postDepositReversalTombstone` and casino's existing rollback-of-never-seen handling) | The rollback writes a tombstone; the late-arriving original WIN settlement is then rejected outright — for **both** the cash and bonus-funded portions of the split. The bonus-specific gap this test must prove beyond the existing cash-only tombstone test: the tombstone also blocks the wagering-progress side (a late WIN must not retroactively credit progress against a Grant whose stake was already tombstoned as void), not only the ledger posting |
| C22 | **Duplicate win** — two identical WIN-settlement callbacks for the same occurrence (provider redelivery), at least one racing concurrently | ADR 0038 §14's composed idempotency key / `occurrence_ordinal`, identical to C11 | Exactly one WIN posting and one wagering-progress increment regardless of delivery order/concurrency. Restated as its own row (rather than assumed covered by C11's generic "activity events" framing) because the gate directive names the WIN-callback path explicitly |
| C23 | **Duplicate rollback** — two identical rollback callbacks for the same occurrence, at least one racing concurrently | Same idempotency key as C22, plus the existing double-reversal-protection `FOR UPDATE` row lock (mirrors this document's Stage 4H-B0 §1.8 retail-reversal pattern) | Exactly one compensating entry posts; the second rollback is a no-op returning the original result, never a second reversal of an already-reversed transaction |
| C24 | **Concurrent win and rollback** — a WIN settlement and a rollback for the *same* round/occurrence are delivered concurrently, racing to be processed first | The `(tenant_id, provider_id, provider_tx_id)`-scoped advisory lock this platform's own `postBet` precedent already establishes for exactly this race class, serializing the two attempts | Exactly one of the two effects is authoritative and the loser observes the winner's already-committed state: if WIN wins, the round settles and the rollback then correctly reverses that settlement (never a race-losing no-op that silently drops the rollback); if the rollback wins first (rollback-of-not-yet-seen-original), C21's tombstone mechanism applies and the WIN is rejected on arrival. There is never a state where both an unreconciled WIN posting and an unreconciled rollback-with-no-original-found coexist |
| C25 | **Late callback** — a WIN or rollback callback for a bonus-funded stake arrives with unusually high latency (hours/days), well after other lifecycle events for the same Grant/player have already occurred | The same live, `clock_timestamp()`-consistent, in-transaction re-evaluation this document already requires for RG/Risk/AssetAuthorization (C13–C15) and G-2's own live `FOR UPDATE` read (T.7) | The callback's outcome is a pure function of the CURRENT committed state at processing time (Grant status, RG status, asset authorization) — never of how long the callback took to arrive, and never of a value cached or assumed from whenever the bet was originally placed |
| C26 | **Callback after grant expiry** — the WIN/rollback callback for a locked stake arrives after `G` has already naturally expired (time-limit reached) while that stake was still outstanding — G-2 trigger path (i), §T.13 | Identical mechanism to C18 | **This is G-2 itself**, restated as its own named row because the gate directive names it as a distinct scenario — `BLOCKED` on G-2's selection exactly as C16/C18; what must be provable now (mirroring C16's "what can be tested now" carve-out) is that the race is detected and routed to a defined, non-silent outcome, never silently posted to `player_bonus` as though `G` were still active |
| C27 | **Callback after grant cancellation** — identical to C26, substituting a staff/player-initiated `cancelled` transition (G-2 trigger path (ii), §T.13) for natural expiry | Identical mechanism to C18/C26 | Same `BLOCKED`-on-G-2 status and the same "detect and route, never silently post" minimum bar as C26 |

**Stress-test requirement, restated from Wave 1 §1, applies unchanged**:
C22, C23, C24 each require the N-way (minimum 8 concurrent goroutines,
real Postgres, `-race`) stress variant in addition to the two-actor race,
before being credited as tested — a burst-size requirement is
particularly load-bearing for C24 (win/rollback racing), since a lock
that only serializes correctly at low contention is not proof it holds
under a real redelivery storm.

**Re-verification flag**: every mechanism cited above (T.7's live `FOR
UPDATE` read, the `postWin`/`postRollback` destination-resolution fix the
Wave 1 reconciliation already ordered independently of G-2) is written
against the architecture as it stands today. Casino's own postWin
financial-resolution design (`4HB1W15-01`) may specify additional
adversarial detail (e.g. a specific destination-resolution algorithm for
mixed cash/bonus splits) this table does not yet know about — this table
must be re-checked against that design once it lands, not assumed to
anticipate it fully.

### 2. Grant terminal-state invariant test plan

This is a test-shape specification, not a test — bonus-engine's own
formal proof of the deferred-terminal design (`4HB1W15-02`, §A.9) is a
parallel dispatch not yet visible here, and the Wave 1 reconciliation's
own description of the design (`docs/governance/task-registry.md`, Stage
4H-B1 reconciliation finding 1) is itself only an engineering-design
sketch, not a finished mechanism: "a Grant may only reach a genuinely
terminal status... once its attributable locked balance... reaches zero.
A terminal trigger firing while locked funds remain outstanding defers
the Grant into a pending-settlement sub-state that finalizes
automatically once the locked stake resolves."

**The category of test this design requires, once it exists, stated now
so Wave 2+ does not have to re-derive it:**

1. **The core millisecond-race test** (the exact scenario the task names):
   construct Grant `G` with a locked stake `L` attributable to it
   (non-zero, via `correlation_id`, per the G-3 netting discriminator
   `ledger-accounting-model.md` §6.4.11 already specifies). Hold two
   competing transactions at a synchronization barrier immediately before
   commit — `G`'s own terminal trigger (an expiry-timer firing, a staff
   cancellation, a forfeiture) and `L`'s settlement/void event — and
   release them simultaneously, forcing genuine commit-order contention
   rather than a scripted two-step sequence. This requires a test-only
   synchronization hook (e.g. a `SAVEPOINT`-and-wait pattern, or an
   injected barrier channel two goroutines block on before their final
   `COMMIT`), mirroring the harness class this platform's own
   `postBet`/`rg.lockPerson` adversarial-lock stress tests already use to
   force real contention rather than relying on incidental scheduler
   timing.
2. **The invariant under test, stated precisely**: at no committed instant
   does `G` show a hard-terminal status (`expired`/`cancelled`/
   `forfeited`, final) while its attributable locked balance is non-zero.
   Regardless of which of the two racing transactions the database
   actually serializes first, the only two legal outcomes are (a) the
   terminal trigger's effect is deferred into the pending-settlement
   sub-state because live-read locked balance was still non-zero at that
   instant, or (b) the locked stake had already resolved to zero before
   the terminal trigger's own live read, in which case ordinary hard-
   terminal transition proceeds normally — there is no third, "half-
   applied" outcome (e.g. a hard-terminal status written while `L` is
   later discovered still outstanding).
3. **Automatic finalization, tested as its own transition, not assumed**:
   once the last outstanding locked stake attributable to a
   pending-settlement Grant resolves (settles or voids to zero), the
   Grant's finalization to its correct hard-terminal status must be
   proven to fire automatically, exactly once, with its own Progress
   entry — mirroring this document's own "assert the job fires, not just
   that the query is right" standard (Stage 4H-B0 §1.7) rather than
   assuming a query being correct implies the transition actually
   happens.
4. **Idempotency of the finalization step itself**: the automatic
   finalization must be provably idempotent under redelivery/re-invocation
   (e.g. two near-simultaneous resolving events for two different locked
   stakes under the same Grant, both observing "now zero" and racing to
   finalize) — exactly one finalization transition and one Progress entry
   results, using the same `(tenant_id, grant_id)` advisory lock family
   already governing every other Grant-mutating race in this matrix.
5. **N-way / multi-stake variant**: a Grant with several outstanding
   locked stakes at the moment its terminal trigger fires must remain in
   the pending-settlement sub-state until **all** of them resolve to
   zero, tested with concurrent partial resolutions (some stakes settling
   while others are still locked) to prove no premature finalization on a
   stale or partial read of "locked balance."
6. **Non-regression on the ordinary case**: a Grant whose terminal trigger
   fires with zero locked balance outstanding must transition immediately
   and normally — the deferred sub-state must never introduce latency or
   an extra Progress entry for the overwhelming majority case where no
   race exists at all.
7. **B1 continuously, not only at the end**: Invariant B1 must be asserted
   after every state transition in this sequence (deferral, partial
   resolution, finalization), not only after the final state, per this
   document's own property-testing-plan discipline (§3 of the Wave 1
   section, "Ledger conservation").

**What is explicitly not specified here, pending the formal proof**:
whether "attributable locked balance" is a stored, incrementally
maintained counter or a live-derived read (Model C's own read-time
derivation approach, `ledger-accounting-model.md` §6.6, would suggest the
latter, for the same reasons Model C rejected a stored counter for
ordinary wagering progress) — the test design above is written to be
agnostic to that choice (it asserts the *outcome*, "no hard-terminal
status while locked balance is nonzero," not a specific query shape), but
the exact SQL/locking primitive the proof settles on must be re-checked
against item 1's harness once it lands, since a stored-counter design and
a live-derived-read design have different concurrency hazards worth
naming explicitly in a future revision of this section.

### 3. Segmentation Engine test requirements

Architect's Segmentation Engine architecture document (`4HB1W15-03`, §B)
is a parallel dispatch not yet visible here. This section is written
against the task's own description of the required properties
(deterministic AND/OR/NOT composability, static-vs-dynamic membership,
DENY-survival) and this platform's own established patterns (the RLS
dual-scope pattern already used for `bonus_grants`, the fail-closed
composition order RG/Risk/AssetAuthorization already establish) — not
against a concrete schema, which does not exist yet.

**Example dynamic segments** (illustrative criteria trees, not literal
config syntax, chosen to exercise AND/OR/NOT composition and nesting):

1. **"VIP retention"** — `tier ∈ {gold, platinum} AND lifetime_deposits_minor_units > threshold` (a two-leaf AND).
2. **"Reactivation target"** — `(days_since_last_deposit > 90 OR account_status == dormant) AND NOT self_excluded` — exercises OR nested inside an AND, and a NOT leaf. **Flag**: `self_excluded` must never be a Segmentation-owned fact read directly from `player_restrictions` — if a segment criterion needs it at all, it must be a read-only, denormalized copy the Segmentation Engine is fed, never a second reader of RG's own authoritative table, mirroring ADR 0034 §1's "must not invent its own restriction table or enum" rule applied one layer up. This is a required architectural check on `4HB1W15-03`'s actual design, not assumed satisfied here.
3. **"New depositor, geo-scoped"** — `country ∈ {ES, MX, CO} AND first_deposit_at IS NOT NULL AND first_deposit_at > (now - 30 days)` — a three-leaf AND with a time-window leaf, chosen specifically to exercise the static-vs-dynamic and as-of-time properties below (a "new depositor" segment is definitionally time-relative).
4. **"Churn-risk with open bonus exposure"** — `(session_frequency_7d < 0.5 * session_frequency_28d_avg) AND has_active_grant == true` — exercises a criterion that reads a fact from a different domain (Bonus Engine's own Grant state) as a segment input, which is the kind of cross-domain read §4 below's boundary tests must confirm is read-only.
5. **"Suspected multi-accounting"** — `device_fingerprint_link_count > 1 OR payment_instrument_link_count > 1` — deliberately the same identity-graph fact this document's Wave 1 §2 already named as an unresolved dependency for abuse test A8; recorded here as the same open dependency reached from the segmentation side, not a new one.

**Deterministic evaluation, property-based**: generate random AND/OR/NOT
trees of arbitrary depth over a fixed set of boolean/numeric leaf
predicates and random player-fact fixtures; assert the Segmentation
Engine's evaluation result exactly matches an independent, naively-written
reference evaluator (a straightforward recursive boolean-tree walker) for
every generated tree/fixture pair, and that repeated evaluation of the
identical tree against the identical fixture always yields the identical
result (no hidden non-determinism, e.g. floating-point comparison on a
monetary threshold — every numeric leaf must compare in integer minor
units, mirroring CLAUDE.md's money rule extended to segment criteria).

**Static vs. dynamic membership — two genuinely different tests, not one**:

- **Static segment** (an explicit, staff-curated list of player ids,
  snapshotted at creation/edit time): a direct-SQL/API test confirming
  membership is exactly the persisted list — adding or removing a player
  from the underlying criteria that originally justified their inclusion
  must have **zero** effect on a static segment's membership until a
  staff actor explicitly edits the list.
- **Dynamic segment** (computed live from criteria): membership must be
  re-derived at the moment of use, not cached indefinitely — a test that
  changes an underlying player fact (e.g. crosses `lifetime_deposits`
  over the threshold) and confirms the very next evaluation reflects the
  new membership, with no stale-cache window longer than whatever
  explicit staleness bound the architecture states (if the architecture
  permits a bounded cache, the test must assert the bound is actually
  enforced, not merely documented).

**As-of reconstructability — the audit requirement, tested explicitly**:
per the gate directive, a player's segment membership must be
reconstructable as of any past eligibility decision. This requires more
than a live-evaluation test: `TestSegmentMembership_ReconstructableAsOfPastDecision`
— given a historical Grant's `issued`-time Progress entry (which already,
per doc10 §1.3/§10.1, records "why `issued` was granted" as a snapshot),
re-run the dynamic segment's own criteria evaluation using the exact
player-fact-history **as of** that recorded instant (not current facts)
and assert the result is identical to what the Progress entry's own
recorded eligibility snapshot states. **This is flagged as a genuine
open architecture dependency, not assumed satisfiable by construction**:
it requires either (a) player facts to be themselves time-versioned/
event-sourced so an "as of T" read is possible at all, or (b) the
Segmentation Engine's own evaluation result to be snapshotted and stored
at decision time (in which case "reconstructable" means "stored," not
"re-derivable," and the test changes shape accordingly to a direct
snapshot-equality check rather than a re-evaluation). Which of these two
shapes `4HB1W15-03`'s design commits to changes which test is actually
written — this section names the requirement and both candidate test
shapes; it does not resolve the design question, which belongs to
`architect`.

**The DENY-survival test, named exactly as the gate directive requires**:

`TestSegmentMembership_NeverOverridesAuthoritativeDeny` — construct a
player who satisfies 100% of a segment's criteria (segment-eligible by
criteria alone), independently combined with each of the following,
tested as four separate cases, at each of Bonus Engine's three live
checkpoints (grant, activation, conversion — T.1's composition order):

1. An active self-exclusion (RG `Decision.Allowed == false`).
2. A Risk `DENY`/hard-limit hit (`risk.Evaluate` `Outcome == DENY`).
3. A KYC-blocking status (however identity-compliance's own Bonus/KYC
   integration expresses it — cited, not re-derived, from ADR 0034).
4. An `AssetAuthorization.CheckEligibility` denial for the Grant's asset.

In every one of the four cases, the eligibility **decision** must be
DENY (the transition blocked, per T.3/T.12's already-frozen consequence
shape), while the segment-**match** fact itself is still correctly
recorded as "matched" in the Progress/audit trail — i.e. the system must
never misreport a segment non-match to make the denial look like
ordinary ineligibility; segment membership and the RG/Risk/KYC/
AssetAuthorization gate are two independent, separately auditable facts,
and composition order is fixed: segment membership is an input to
*whether an Offer is offered/considered at all*, never a bypass of, or
substitute for, the mandatory live gates. This is C13/C14/C15's shape
extended to segment-targeted grants specifically, and must be re-run
identically for CRM-triggered and Affiliate-attributed grants (§4/§5
below) once those trigger paths exist, per §5's A19.

### 4. CRM/Affiliate integration-boundary tests

Architect's CRM Engine and Affiliate Engine architecture documents
(`4HB1W15-03`, §E/§F) are parallel dispatches not yet visible here. The
following states the **shape** of the required boundary-invariant tests
— mirroring the pattern `architect`'s own cross-domain boundary-invariant
documents elsewhere in this project use (an enumerated, independently
checkable list of "X cannot do Y" architectural invariants) — without
inventing a schema or literal Go test name for either domain, since
neither exists yet.

- **BI-CRM-1 — CRM cannot post to the ledger directly.** A direct-SQL/
  role-grant test proving whatever database role/credential the CRM
  service runs under has no `INSERT`/`UPDATE`/`DELETE` grant on
  `ledger_transactions`/`ledger_entries` — proven at the database
  permission layer, mirroring this document's own "never an application-
  path-only test" standard for RLS (Wave 1 §4), not merely "the CRM
  codebase doesn't currently call a ledger-write function."
- **BI-CRM-2 — CRM cannot directly mutate a Grant's status or a player's
  wallet balance.** Both a DB-role test (same shape as BI-CRM-1, scoped
  to `bonus_grants`/wallet balance tables) and an application-layer test:
  a CRM-authenticated request against any Grant-mutation endpoint
  reserved for Bonus Engine's own internal call path is rejected
  (403/equivalent), including a direct-object-reference attempt against a
  known `grant_id`.
- **BI-CRM-3 — a CRM-triggered Grant goes through the identical RG/Risk/
  AssetAuthorization gate as a player-self-triggered Grant.** Issue two
  Grants under an identical Offer/eligibility fixture, one via the
  ordinary automated/player-triggered path and one via a CRM-initiated
  manual/bulk-assignment path; assert (a) equivalent RG/Risk/
  AssetAuthorization calls occur for both (spy/mock assertion on call
  parameters), and (b) a fixture where RG/Risk/AssetAuthorization would
  deny the player-triggered path denies the CRM-triggered one identically
  — a CRM-initiated grant attempt against a self-excluded player must
  never silently succeed because "staff/CRM initiated it."
- **BI-CRM-4 — CRM tenant/brand isolation.** A CRM actor scoped to
  tenant A must never read, list, or trigger a Grant for a player of
  tenant B — direct-SQL RLS test at whatever new CRM-owned tables land,
  mirroring the standing cross-tenant baseline (this document's own
  "Baseline requirement" section) and Wave 1 §4's dual cross-tenant/
  cross-player discipline.
- **BI-CRM-5 — audit parity.** Every CRM-triggered Grant or segment-based
  bulk assignment writes the identical `audit.Record` shape doc10 §10
  already requires for a staff-driven manual grant, with `ActorType`
  distinguishing a CRM-system actor from a human staff actor, and a
  reason code where doc10 §10's table requires one for the corresponding
  human-equivalent action.
- **BI-Affiliate-1 — Affiliate cannot credit a player balance directly.**
  Same DB-role-grant shape as BI-CRM-1, scoped to whatever affiliate-
  commission/attribution tables exist. An affiliate-attribution event
  must never itself be a ledger-posting instruction — it must route
  through the identical Bonus Engine Grant/Offer mechanism (for a
  referral bonus) or a `ledger-finance`-owned commission posting (for an
  affiliate payout), never an Affiliate-owned write path into
  `ledger_transactions`.
- **BI-Affiliate-2 — affiliate attribution is read-only eligibility input,
  never a bypass.** An affiliate-referred player must traverse the
  identical RG/Risk/AssetAuthorization/segment-eligibility chain as an
  organically-registered one — a test asserting an affiliate-sourced
  signup carrying a self-exclusion flag is denied a referral bonus
  identically to a non-affiliate signup with the same flag.
- **BI-Affiliate-3 — Affiliate tenant/brand isolation.** Same shape as
  BI-CRM-4, applied to whatever Affiliate-owned attribution/commission
  tables land.
- **BI-Affiliate-4 — idempotency.** A redelivered/duplicate affiliate
  conversion-attribution event must not produce two Grants or two
  commission postings — mirrors C1/C10's mechanism, applied to whatever
  idempotency key the (not-yet-designed) affiliate attribution event
  carries.

**Cannot be fully specified without `4HB1W15-03` landing**: the exact
table/schema for a CRM-triggered grant request, the affiliate
attribution event's own idempotency-key shape, whether CRM runs under its
own database role or a scoped view of Bonus Engine's, and whether
"affiliate commission" is itself a financial ledger posting (in which
case it inherits the full CLAUDE.md financial-testing floor in its own
right, not merely the boundary tests above) or a non-ledger side record
reconciled separately — all four are named here as open items the
Segmentation/CRM/Affiliate architecture must answer before BI-CRM-1
through BI-Affiliate-4 can be written as literal tests rather than
invariant statements.

### 5. Bulk-assignment abuse/correctness matrix (A15–A19)

Extends Wave 1's A1–A14 (cross-referenced, not duplicated) with the new
targeting/triggering surfaces this gate introduces: segment-based
targeting, CRM-triggered grant issuance, affiliate attribution, and
promo-code redemption at scale.

| # | Abuse vector | What a real attempt looks like | What the test must prove |
|---|---|---|---|
| A15 | Segment-boundary gaming | A player deliberately times an action (a deposit, a withdrawal, a dormancy-then-return cycle) to repeatedly cross into/out of a favorable dynamic segment's boundary, attempting to re-qualify for a segment-targeted Campaign more than once | The segment's own "scope of uniqueness"/dedup for that Campaign (cross-references A9's already-flagged open gap: no document yet confirms an Offer's eligibility axis carries a "scope of uniqueness" concept) prevents repeat qualification; boundary-timing manipulation must not create a race allowing double-claim, extending A7/C1's idempotency mechanism to segment re-evaluation specifically. **`NOT APPLICABLE (open dependency)`** until the "scope of uniqueness" concept A9 already flagged is designed — restated here as the identical gap reached from the segmentation side |
| A16 | CRM-triggered bulk-assignment amplification | A compromised, over-permissioned, or buggy CRM bulk-assignment tool issues N Grants beyond a Campaign's own configured tenant/brand/segment targeting (e.g. mass-assigning a brand-specific Campaign platform-wide) | (a) A bulk job cannot target a scope broader than the Campaign's own configuration (§8) — rejected at the same RLS/authorization boundary as any other request, not merely a UI guardrail; (b) the bulk job is itself audited per-recipient, one `audit.Record`/Progress row per Grant, never one row per bulk job hiding N grants — mirrors ADR 0034 §14.5's "one audit row per bet, never per event" cardinality rule applied to a different bulk fan-out case |
| A17 | Affiliate self-referral / attribution fraud | An affiliate refers an account they themselves control, or manipulates attribution (cookie-stuffing, referral-link replay across many self-controlled accounts) to claim N referral Grants | Extends the identical A8 identity-duplication dependency (device/payment fingerprint linking) to the affiliate-attribution path specifically. **`NOT APPLICABLE (open dependency)`**, identical status to A8 itself — not a new mechanism to design, the same unresolved gap reached via a second trigger path |
| A18 | Promo-code abuse at scale | (a) Brute-force/enumeration of coupon codes rather than legitimate receipt of one; (b) redemption-volume abuse — a single-use(-per-player) code redeemed N times before a uniqueness constraint catches it, under real concurrent load | (a) An enumeration attempt (many sequential/parallel invalid-code guesses) must not leak, via timing or error-message variance, whether a guessed code is "close" to a valid one — a security-adjacent correctness property named here as a required test but owned by `security`'s own review, not resolved by `qa`; (b) the coupon redemption-uniqueness constraint (per-code or per-code-per-player, per the Offer's configuration) is enforced at the database layer under N≥20 concurrent redemption attempts against the coupon-validation HTTP endpoint specifically (doc10 §1's "one new surface," an endpoint rather than an event-bus consumer, so it needs its own concurrency proof rather than inheriting A7's event-delivery-shaped one) |
| A19 | Cross-reference: abuse-control parity for new trigger paths | A CRM-initiated or Affiliate-attributed Grant is assumed, without being tested, to inherit the protections A1–A18 already prove for player-triggered/automated Grants | Rather than re-deriving a parallel A-series for CRM/Affiliate, this item requires A1 (deposit farming), A2/A3 (void/rollback farming), A5 (Offer-version manipulation), A7 (grant duplication), A11 (replay), A12 (tampering), and A13 (race/burst) each be **re-run once with a CRM-triggered or Affiliate-attributed origin substituted for the ordinary origin** — not assumed covered by the original run, exactly as this document's "No fake completion" standard requires for any new trigger surface reaching an already-tested effect through a new path |

### 6. P0/P1/P2/P3 severity framework for Phase 2 findings

Not filled in here — Phase 2 reviews (`ledger-finance`, `security`,
`code-reviewer`, `bonus-engine` on the Segmentation doc, `product-owner-
proxy`) have not yet run. This is the criteria `qa` will apply when
triaging their findings into the "complete P0/P1/P2/P3 register" the
gate directive asks for, stated in advance so triage is consistent rather
than improvised per finding, and consistent with how this project has
applied these labels at every prior gate (Stage 4G-FINAL, Stage
4H-B0-R6's fix-wave findings, Stage 4H-B0-R7's S-1).

- **P0 — Critical, launch-blocking, no deferral without an explicit
  orchestrator/human decision.** A finding that permits real financial
  loss, a compliance/RG bypass (self-exclusion, KYC, or a jurisdiction
  ban circumvented), a cross-tenant or cross-player fund/data leak, or a
  privilege escalation — exploitable today, or the moment the affected
  code ships, with no compensating control already in place. Cannot be
  quietly fixed-later or quarantined; per CLAUDE.md, deferring a P0 past
  its blocking point is the orchestrator's decision to make and record
  explicitly, never a quiet `qa` call. Precedent: Stage 4H-B0-R6's
  four-eyes-unconditionally-inert bug (`4HB0R6-13`'s P1-A1, later
  confirmed exploitable and treated as launch-blocking despite its
  original P1 label — this framework deliberately places an
  unconditionally-inert dual-control gate at P0, not P1, given that
  precedent).
- **P1 — High, must-fix (or explicitly resolved) before the affected
  capability is labeled `IMPLEMENTED`.** A genuine correctness,
  financial-integrity, or security gap that is either (a) not yet
  reachable in the currently authorized scope (gated behind an
  unresolved human decision or an unbuilt dependency — e.g. this
  document's own `BLOCKED` rows C16/C18/C19/C26/C27, A6, C3, C12), or (b)
  reachable but with a narrower blast radius than P0 (a single domain's
  double-posting risk under a specific race, a missing test for an
  already-designed control). May ship as `BLOCKED`/`NOT IMPLEMENTED` on
  the specific affected sub-scope while the rest of the capability
  proceeds, per this document's own C3/C12/C16/A6 labeling precedent —
  but the affected sub-scope itself is never labeled `IMPLEMENTED` while
  the P1 stands. **Any finding touching a CLAUDE.md financial invariant
  (`SUM(DEBITS) == SUM(CREDITS)`, idempotency, RG/self-exclusion bypass,
  tenant isolation) defaults to at least P1 even if narrowly scoped,
  never silently P2, absent a specific, documented reason it is already
  fully mitigated elsewhere.**
- **P2 — Medium, hardening, non-blocking.** An observability, defense-
  in-depth, reconciliation-latency, or precision/rounding-boundary gap
  that does not itself permit fund loss or compliance bypass under
  currently authorized scope, but should close before broader production
  exposure. Recorded as a residual risk with an owner in the completion
  report; may be deferred to a follow-up wave with reasoning stated,
  mirroring Stage 4G-FINAL-FINANCE-GATE's precedent (6 P2s recorded, none
  fixed that stage, judged non-blocking with reasoning given).
- **P3 — Low, cosmetic/documentation.** Naming or terminology drift, a
  redundant check, a minor cross-document inconsistency, or a finding
  whose only cost is future-reader confusion rather than a correctness
  defect. Never blocks sign-off; tracked so it is not silently lost,
  mirroring this project's existing practice of recording P3s in a
  consolidated list rather than omitting them (Stage 4H-B0-R6's
  `4HB0R6-12` consolidated-P3-list precedent).

**Triage rule, stated once rather than per severity level**: severity is
assessed against *actual reachable impact given the currently authorized
scope*, not a theoretical worst case unrelated to what is actually being
built — but a finding gated behind an unresolved human decision (G-2
being the live example) is never *downgraded* on account of being
gated; its severity is assessed as if the gate were resolved in the
worst available direction, and it is the `BLOCKED` label — not a lowered
severity number — that defers it, exactly as this document's own Wave 1
§5 already insists a `BLOCKED` row is never silently treated as passing
or low-priority. A P0 or P1 finding is never downgraded by the specialist
whose own design produced the underlying gap — closing or re-rating it
requires an independent reviewer, mirroring CLAUDE.md's "no specialist
self-approves its own work."

### 7. Status, labels, and what remains unverifiable pending parallel deliverables

Every item in this section is `RECOMMENDATION` (a test strategy binds
nothing until executed) and `NOT IMPLEMENTED` (no test code exists yet).
Named explicitly, per this section's own opening note, rather than
silently assumed resolved:

1. **§1 (C18–C27)** is written against the Terminal-Grant Technical
   Contract (§T.7/§T.13) and the existing casino rollback/idempotency
   precedent, not against casino's own postWin financial-resolution
   design (`4HB1W15-01`), which may add adversarial detail (e.g. a
   specific mixed-funding destination-resolution algorithm) this table
   does not yet account for. Re-verify before Wave 2 relies on it.
2. **§2** is written against the Wave 1 reconciliation's own engineering-
   sketch description of the deferred-terminal design, not against
   bonus-engine's formal proof (`4HB1W15-02`, §A.9). The test *shape* is
   designed to be agnostic to a stored-counter-vs-live-derived-read
   implementation choice, but the exact harness/locking primitive named
   in item 1 of §2 must be re-checked once the proof lands.
3. **§3 and §4** are written against the task's own description of the
   required properties and this platform's own established patterns
   (RLS dual-scope, fail-closed composition order), not against
   architect's Segmentation/CRM/Affiliate architecture documents
   (`4HB1W15-03`), which do not exist yet. Two genuine open design
   questions are flagged inline rather than resolved: whether dynamic
   segment membership is reconstructable-as-of-a-past-instant via
   time-versioned player facts or via a stored per-decision snapshot
   (§3), and the exact schema/idempotency-key shape for CRM-triggered
   grants and affiliate attribution events (§4).
4. **§5's A15 and A17** both restate already-open Wave 1 dependencies
   (the Offer eligibility axis's undesigned "scope of uniqueness"
   concept, A9; the undesigned device/payment fingerprint identity-graph
   fact, A8) reached via new trigger paths — they are not new gaps this
   section discovered, and do not become testable until those two Wave 1
   items are separately closed.
5. **§6 (the severity framework) has no external dependency** and can be
   applied to Phase 2's findings as soon as they exist.

None of this section's items can move toward `IMPLEMENTED` before the
corresponding Segmentation/CRM/Affiliate/casino-postWin/Terminal-Grant
implementation exists to test, and rows flagged `BLOCKED` (C18's terminal
branch, C19, C20 pending G-3, C26, C27) or `NOT APPLICABLE (open
dependency)` (A15, A17) cannot move to `IMPLEMENTED` before their named
human decision or architecture dependency is resolved, regardless of
implementation progress elsewhere.
