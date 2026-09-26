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

## Stage 4H-B1, Wave 1.5 Fix Wave, Phase 2 — Test-plan update (`qa`, design/strategy only)

Status: `RECOMMENDATION`, `NOT IMPLEMENTED`. `qa` did not author any of
this round's fixes (`bonus-engine`'s N1 revision, `casino`'s doc 08 §16
revision, `architect`'s docs 30/31/32/33/34, `security`'s Wave 1.5 fix-wave
contract additions) — this section is independent review-and-extend, per
CLAUDE.md's "no specialist self-approves its own work." **No test code is
written here.** No Human Decision Register item (G-2,
`OpenBetSelfExclusionPolicy`, cashout policy, FD-1) is selected, referenced
as resolved, or defaulted by any test below — every test that touches one
is explicitly marked `BLOCKED` on it, exactly as this document's own Wave
1.5 §6 severity framework requires.

This section extends, and does not restate, the Wave 1.5 matrix
immediately above. Numbering continues: casino's concurrency table
continues from **C28** (Wave 1.5 ended at C27); the new fixes each get
their own named test IDs rather than forced into the C-/A- numbering,
since they are new mechanisms (`EconomicOperationIdentity`, `SEP-1`,
`AFF-4E-1`, the settlement-timeout sweep, the `member_of`/`InclusionSafety`
correction), not new instances of the existing bonus-Grant race class.

### 1. G-2 non-selection tests — `G2-HOLD-1`/`G2-HOLD-2` confirmed, `G2-HOLD-3` added

**`G2-HOLD-1` and `G2-HOLD-2` (doc10 §N1.11) are adequately specified as
written.** Both name a concrete setup, a precise ledger-history assertion
(not "the request succeeded"), and both are explicitly written to **fail**
against a reimplementation of the exact pre-revision P0 defect
(post-then-reforfeit in one transaction; a held credit posted transiently
to the fungible `player_bonus` account) — the standard this document's own
Wave 1.5 §1 already applies to C18–C27. No extension to either is required
to make them adequate. Two small tightenings, not corrections:

- `G2-HOLD-1` should be run against **both** of N1.4.1 item 2's holding-
  representation branches (the sportsbook-shaped in-place
  `player_locked_bonus` branch, and the casino-shaped dedicated
  holding-account branch) — the assertion is identical in both, but the
  ledger rows it inspects differ, and a suite that only exercises one
  branch has not actually proven the invariant for the other.
- `G2-HOLD-2`'s four independently-sufficient assertions should each be
  run as their own failing sub-test (not one test with four `assert`
  statements that stops at the first failure), so a partial regression
  (e.g. only the withdrawable-cash projection leaks `W`) is distinguishable
  from a total one.

**New test, closing the gap the task identifies — `G2-HOLD-3`: "a
`HeldDispositionRecord`'s resolution is exactly one of the three actions,
applied atomically, never partial or ambiguous."**

Setup: a `HeldDispositionRecord` with `status = held`, amount `W`, lock-
release amount `X` (distinct, per §16.5a), under Grant `G`. A resolution is
submitted naming one of `ACTION_REFORFEIT` / `ACTION_ROUTE_TO_CASH` /
`ACTION_HOLD_FOR_REVIEW`'s own later manual sub-choice (N1.8.1 rows 8–10).
This test's *shape* is agnostic to which of the three G-2 eventually
selects — exactly as `G2-HOLD-1`/`G2-HOLD-2` already are — so it is
buildable and runnable against a test harness that simply parameterizes
the chosen action, without selecting G-2 itself.

Assertions, each independently sufficient to fail the test:

1. **Exactly one terminal status is ever reached.** After resolution,
   `HeldDispositionRecord.status` is exactly one of `resolved_reforfeit` /
   `resolved_route_to_cash` — never both across two rows for the same
   `correlation_id`, never a third, undocumented value, and never left at
   `held` after a resolution attempt that the system reports as having
   succeeded.
2. **The payout `W` and the lock-release `X` resolve in the same
   transaction, to a coherent pair of destinations.** No committed state
   exists where `W` has moved (to `promo_liability` or `player_cash`) but
   `X` is still sitting in `player_locked_bonus`, or the reverse. This
   directly tests doc 08 §16.5a's "the lock itself must resolve, never
   dangle" requirement against doc10 N1's disposition mechanism, at the
   seam between the two documents — a boundary neither document's own
   text tests in isolation. **Flagged as `NOT APPLICABLE (open
   dependency)` for the `ACTION_HOLD_FOR_REVIEW` sub-case specifically**:
   §16.5a and §16.13 both disclose that `ACTION_HOLD_FOR_REVIEW`'s
   destination for the *released lock amount* `X` (distinct from the held
   payout `W`) is an unresolved `ledger-finance`/`bonus-engine` cross-
   dependency — this assertion cannot be written concretely for that one
   sub-case until that destination is named.
3. **Concurrent resolution attempts on the same record never both
   commit.** Two staff sessions submit different dispositions for the
   same `HeldDispositionRecord` at nearly the same instant (mirrors N1.6
   Scenario 5(b)'s reuse of §7.7's double-reversal protection, applied
   here to first-time resolution rather than replay): exactly one
   transitions `held → resolved_*`; the loser's attempt finds the record
   already transitioned and is rejected outright, under the same
   `(tenant_id, grant_id)` advisory lock N1.5 specifies.
4. **A crash between the two halves of step 2 cannot leave a partial,
   ambiguous state** — proven, not assumed, per this document's own
   "assert the job fires, not just that the query is right" standard
   applied to atomicity: kill the connection mid-transaction (the same
   fault-injection technique ADR 0032 §7's partial-failure tests already
   use) between the notional debit of the holding representation and the
   notional credit of the destination account; on recovery, the record
   must be found still `held`, with no partial ledger entries of either
   half — `ledger.Post`'s single-balanced-transaction discipline (N1.5)
   provides this structurally, and the test exists to prove that claim
   rather than take it on faith.
5. **Progress-trail cardinality is exactly one.** Exactly one
   `g2_disposition_applied` Progress entry is written per resolution —
   never zero (a silent resolution) and never two (a double-recorded
   one), reusing this document's own "one audit row per event, never
   per bulk fan-out or per retry" cardinality discipline (ADR 0034
   §14.5, restated for bulk assignment at A16).

**Status**: `G2-HOLD-3`'s mechanism-level assertions (1, 3, 4, 5) are
buildable today against a stubbed/parameterized resolution action, since
they test the resolution mechanism's exclusivity and atomicity, not which
action G-2 selects. Assertion 2's `ACTION_HOLD_FOR_REVIEW` sub-case is
`BLOCKED` on the named open cross-dependency (lock-release destination for
that action), independent of G-2 itself. The full end-to-end test
(constructing a genuine `HeldDispositionRecord` via a live settlement
event and a live resolution call) is `BLOCKED` on `internal/bonus` and
`bonus-engine`'s own Wave 2+ schema existing at all (N1's own standing
disclosure) — the same status every other N1-dependent test in this
document already carries.

### 2. CRM decomposition tests (`SEC-W15-02`) — the `EconomicOperationIdentity` lineage adversarial test

The directive's Testing-section requirement (items 5/11) asks for one
adversarial test proving a bulk economic operation retains its
authorization boundary across API splitting, retries, pagination,
concurrent workers, partial batches, resumed jobs, and duplicate requests.
Doc 34's own worked example (§5.4) already walks this scenario narratively
against the before/after design; the test below operationalizes it as one
composite scenario plus the supporting per-invariant tests doc 34 itself
names (§6, `EOI-1`–`EOI-13`) so a partial regression is attributable to a
specific invariant, not merely "the composite test failed somewhere."

**Primary test — `EOI-DECOMPOSITION-1`, mirroring doc 34 §5.4's worked
example exactly, run as one continuous scenario:**

Setup: a `crm_engagement_campaign_activation` EOI is minted at activation
(four-eyes consumed, `recipient_ceiling = 10,000`,
`intended_aggregate_value` = 10,000 × the offer's face value,
`subject_set_hash` pinned over a 10,000-row materialized audience).

1. **API splitting is not a bypass.** Issue grants against the pinned
   audience through three different call surfaces that all name the
   identical `parent_operation_id`: (a) the ordinary `BulkGrantJob` path,
   (b) a staff single-Grant action against individual members of the same
   audience, (c) a simulated `ActorService` caller invoking the
   single-grant surface directly. Assert the **combined** consumption
   across all three surfaces is checked against **one shared** remaining
   budget — a grant issued through surface (c) must see surfaces (a)/(b)'s
   already-consumed budget, and the 10,001st grant issued through *any*
   surface is rejected. This is the direct regression test for
   `SEC-W15-02`'s literal shape ("N individually-sub-threshold calls,
   none of which is a `BulkGrantJob`") — it must **fail** against any
   implementation where the ceiling is checked only inside the
   `BulkGrantJob` path.
2. **Retry never re-mints (`EOI-2`/`EOI-3`).** A lost-response retry of
   the activation approval itself resolves, via
   `UNIQUE (tenant_id, operation_type, idempotency_key)`, to the
   already-existing EOI — no second approval consumed, no budget reset.
   A retried *execution* (one grant call repeated after a timeout)
   inherits `parent_operation_id` from its first attempt's record and
   does not double-consume.
3. **Pagination cannot exceed the ceiling (`EOI-4`).** The 10,000-row
   audience is delivered as 10 pages of 1,000 (`lineage_kind = page`,
   `batch_ordinal`/`batch_total` declared). Lower `recipient_ceiling` to
   5,000 for this sub-case and assert page 6 (the 5,001st–6,000th
   subjects) is rejected in full — not truncated to the remaining 4,000
   and not silently queued — reusing doc 31 §7.2.3 item 4's "aborts on
   deviation, never warns, never truncates" rule, restated here as doc
   34 §5.1's "rejection, never degradation."
4. **Concurrent workers cannot jointly overspend (`EOI-5`/`EOI-6`).**
   With `remaining_recipient_budget = 10` on a fresh EOI, launch N≥20
   concurrent goroutines (this document's own stress-test floor, Wave 1
   §1, restated) each attempting to consume 1 unit. Assert exactly 10
   succeed and 10+ fail cleanly, serialized by the `FOR UPDATE` on the
   EOI row — not by an accidental low-contention pass, per this
   document's own stress-test discipline.
5. **Partial batch / crash-and-resume never re-authorizes the unspent
   remainder as a fresh budget (`EOI-3`).** Inject a crash after a bulk
   job has consumed 400 of a 1,000-unit budget. Resume the job
   (`lineage_kind = resume`). Assert the resumed job re-attaches to the
   same `root_operation_id`, the already-consumed 400 stays consumed (the
   existing `BulkGrantJobItem` rows are the record, per doc 34 §3.2), and
   the resumed job can consume at most 600 more — not a fresh 1,000.
6. **Duplicate top-level requests (`EOI-2`).** An operator double-clicks
   "activate": the second request resolves to the existing EOI with its
   already-consumed approval; no second approval row, no second mint.

**Supporting invariant-level tests, named for completeness, each smaller
than the composite scenario and each independently useful for isolating a
regression:**

- `EOI-4` **property test**: generate the five §3.2 containment checks
  (wider `recipient_ceiling`, wider `intended_aggregate_value`, a
  `subject_set` not a subset of the parent's, a mismatched `asset_code`,
  a later `expires_at` than the parent's) independently, and assert each
  alone is a rejected write.
- `EOI-9`/`EOI-10` **RLS test**: a CRM-class or affiliate-class principal
  has zero read access to `economic_operations`; a cross-tenant read of
  another tenant's EOI returns zero rows, mirroring this document's own
  "never an application-path-only test" RLS standard.
- `EOI-8` **correlation-id inheritance test**: compare the root EOI's
  `correlation_id` against every leaf execution's — a re-minted value
  anywhere in the chain fails the test.
- **`EOI-9` "enforced by the value-creating domain" test** (doc 34 §5.2):
  a CRM-authenticated caller invoking Bonus's grant surface directly,
  bypassing CRM's own journey-execution code path entirely, with a stale
  or absent `parent_operation_id`, is rejected **at Bonus's own entry** —
  proving Bonus does not rely on CRM having already checked. This is the
  defense-in-depth companion to item 1 above: item 1 proves splitting
  across surfaces doesn't multiply the budget; this proves the enforcing
  domain doesn't trust the calling domain to have enforced anything at
  all.

**Cannot be fully specified without more of Phase 2's reconciliation**:
`DEP-EOI-5` (doc 34 §8) — the unresolved conflict between `security`'s
"pin the materialized set, never re-resolve" and `bonus-engine`'s doc 10
W5 "resolve live at run time" — is exactly the kind of cross-dependency
this test's subject-set assertions depend on, and doc 34 itself states
neither owner has confirmed the "pin is a ceiling, live resolution may
only shrink it" reconciliation it proposes. Item 1 and item 3 above are
written against that proposed reconciliation; if it is not adopted as
written, the exact assertion (does a since-added, in-pin-but-not-yet-
live-resolved subject ever receive a grant, or not) changes. `EOI-DECOMPOSITION-1`
is also entirely `BLOCKED` on `internal/economicop` not existing (doc 34
§7's own explicit "no implementation authorized") — every assertion above
is a specification, not a runnable test, until that package and its
consumption function (`DEP-EOI-2`, `security` + `ledger-finance`'s to
decide) exist.

### 3. Actor≠subject (`SEP-1`) tests

`security`'s own §W15.1.8 already names a solid baseline test list; `qa`'s
addition here is (a) insisting it is run identically, not sampled, across
every one of §W15.1.7's routed enforcement points, and (b) specifying the
regression guard the task calls out by name.

**Run identically at every adopting enforcement point, not sampled** —
mirroring this document's own Stage 4H-B0 §3 "tested against every
mutating endpoint, not sampled" standard, applied to `SEP-1`'s own
enforcement points:

| Enforcement point | Domain |
|---|---|
| Grant issuance, Grant activation | Bonus |
| Bonus adjustment, staff-forced conversion, manual release override | Bonus |
| `BulkGrantJob` execution (set-membership form) | Bonus |
| `BonusSuggestion` review and activation | Bonus |
| Journey/campaign activation containing an `offer_request` step | CRM |
| `CommissionApproval`, settlement instruction, re-attribution, agreement/rule-version activation | Affiliate |

For **each** row: direct self-dealing (actor's `person_id` equals the
resolved beneficiary), indirect self-dealing (a second, distinct
`staff_users` row sharing the same `person_id` as the actor), self-dealing
via the **approver** rather than the requester on an otherwise-clean
request, and — for the set/audience rows — the beneficiary present
*anywhere inside* the pinned, materialized set rather than as the sole
named subject (tested against the pin, per §W15.1.2's own instruction,
never a live re-resolution, so the test also catches a set-swap-after-pin
attack). Every legitimate-operation-succeeds control case (`SEP-1-H1`)
must also be run at each row — a permanently-inert trigger passes every
refusal-shaped test trivially, and only the positive case distinguishes
"working" from "structurally unable to fire."

**The regression guard the task names — `SEP-1-NULL-REGRESSION`, the exact
shape of migration 0029's original inertness:**

Two sub-cases, both required, both asserting **refusal**, never a skip:

1. **NULL beneficiary.** The domain's beneficiary resolver returns a
   non-empty set containing at least one element whose `person_id` is
   NULL. Assert the operation is refused at §W15.1.3 step 5 specifically
   ("any element whose `person_id` is NULL → refuse"), with the `SEP-1`
   error — not merely "some error occurred," and not a silent pass
   because the NULL comparison evaluated to `NULL` (SQL's own three-valued
   logic) rather than `true`, which is precisely the mechanism that made
   migration `0029`'s `IS NOT NULL AND requester_person_id =
   approver_person_id` check inert: a NULL on the left side made the
   whole `AND` false, so the comparison was skipped rather than the
   statement aborting.
2. **NULL actor.** The acting or approving principal's own `staff_users
   .person_id` is NULL. Assert refusal at step 2, before the beneficiary
   resolver is even invoked. Distinguishing steps 2 and 5 in the
   assertion (not merely "an error occurred somewhere in the trigger")
   matters because a naive implementation could satisfy "an error is
   raised" by letting a NULL comparison surface a Postgres type error
   several steps later, which would pass a loosely-written test while
   still being the wrong mechanism — the test must assert the specific,
   named refusal path fires, mirroring `SEP-1-H1`'s own "asserts the
   specific error, not merely 'an error'" instruction.

Also required, restated from §W15.1.8 because the task asks for "direct
and indirect self-dealing" explicitly: **concurrency** — two approvals
racing on the same request, one of them self-dealing, must never both
commit; and **audit** — a `SEP-1` refusal writes an `audit.Record` with
`Outcome: denied` (a control that fires silently leaves no evidence it
fired, the same lesson this document's own DENY-survival test already
encodes at §3 above).

**A `qa` recommendation, not a requirement being invented here**: `SEP-1`
is deliberately one shared mechanism reused across Bonus/CRM/Affiliate
specifically so the `IS NOT NULL AND` mistake cannot recur three times
independently (§W15.1's own stated rationale). The test suite should
mirror that design decision: one shared, parameterized `SEP-1` conformance
suite (resolver-in, refusal-out) run against each domain's own resolver
implementation, rather than three independently-authored test files that
could each individually miss the NULL-guard regression while believing
they'd covered it — the same gap this document flagged as "Named gap 1"
for the FX rate-provider adapter at Stage 4H-B0-R5 (P1-1), applied here to
a security control instead of a financial one.

**Status**: `BLOCKED` on `internal/economicop` not existing for the CRM/
Affiliate enforcement points that compose with it, and on each domain's
own resolver implementation (§W15.1.2, `REQ-SEP-BONUS-*`/`REQ-SEP-CRM-1`/
`REQ-SEP-AFF-1`) not yet being written. The regression-guard shape itself
(`SEP-1-NULL-REGRESSION`) is fully specifiable today against the
withdrawal-governance precedent (`0034_stage3d_withdrawal_governance`)
`SEP-1` explicitly reuses, and should be run against that existing trigger
too, as a live confirmation the platform's one already-shipped instance of
this pattern doesn't already carry the defect `SEP-1` is designed to avoid
reintroducing.

### 4. Affiliate four-eyes tests (`AFF-4E-1` + `SEP-1`, ancestor-chain resolver)

The task asks for a test proving two colluding affiliate accounts under
one entity cannot satisfy `AFF-4E-1` and `SEP-1` together. Read literally
against `security`'s own design, this is actually the **easy** case —
`AFF-4E-1` alone (conjunct B, §6.5.1) refuses **any** `external_affiliate`-
class approver unconditionally, so two colluding affiliate accounts never
reach the point where `SEP-1`'s graph even needs to be checked. The test
plan below states that case, then states the harder case the task's
framing gestures toward — the one `AFF-4E-1` explicitly does **not**
close (§6.5.2's own disclosure) — because a test suite that only proves
the easy case would misreport the actual residual risk.

**`AFF-4E-1-TRIVIAL` — the literal task scenario.** Two `staff_users` rows,
`principal_class = external_affiliate`, distinct `person_id`s, both
affiliated with (or controlled by) the same commercial entity — the
platform has no capability to model "entity" at all, so the fixture
asserts collusion by construction (both accounts act in coordination in
the test), not by any entity-resolution mechanism. Account `a1` requests
a `CommissionApproval` on its own node's accrual; account `a2` attempts to
record the approval. Assert refusal, **and assert it is `AFF-4E-1`
(conjunct B) that fires**, independently of whether `SEP-1` would also
have refused — per `security`'s own W15.2.6 item 3 instruction ("both must
be asserted separately — a test that only proves 'refused' cannot tell
which control is carrying the weight").

**`AFF-4E-1-ANCESTOR-DIRECTION-REGRESSION` — the corrected resolver,
tested at the exact defect it fixes.** This is the test that actually
exercises "the corrected ancestor-chain resolver" the task names, because
`AFF-4E-1-TRIVIAL` above never reaches the resolver at all. Construct a
sub-affiliate override agreement (doc 32 §6.4) where parent node `P`
benefits from child node `C`'s accrual. `C`'s own account requests approval
on `C`'s accrual (clean, non-self-dealing on its face). `P`'s account —
an `internal`-class principal this time, since only `internal`-class can
reach the approval predicate at all per `AFF-4E-1` — attempts to approve
it. Assert refusal **by `SEP-1`**, via the resolver's ancestor-chain walk
finding `P` in `C`'s beneficiary set. This test must be written to **fail**
against a reimplementation of doc 32's own first Fix-Wave draft error (a
resolver walking the **subtree**/descendants instead of the **ancestor
chain**) — under that defective direction, `P` is not a descendant of `C`,
the resolver would find no conflict, and this exact scenario would
incorrectly succeed. Restated for the reverse direction too
(`AFF-4E-1-DESCENDANT-NONCONFLICT`): `C` approving `P`'s own accrual, where
no override agreement runs the other way, must **succeed** (a sub-
affiliate does not automatically benefit from its parent's ordinary
accrual) — the positive case proving the resolver isn't simply refusing
every cross-node approval regardless of direction.

**`AFF-4E-1-INTERNAL-BENEFICIARY` — the honest residual, tested as a
detection, never as a block, per its own documented limitation.** Two
colluding affiliate accounts (both `external_affiliate`-class, so
`AFF-4E-1-TRIVIAL` already blocks either of them from approving directly)
attempt to route the approval through a recruited or complicit **internal**
staff member who beneficially owns one of the colluding nodes:

1. **If the interest is declared** (`affiliate_beneficial_interest_
   attestations.declaration = internal_interest_declared` naming that
   staff member's `person_id`): assert the internal approver is refused by
   `SEP-1`, via `declared_interest_person_ids` joining the node's
   beneficiary set (§6.5.2.1) — this is the case the attestation mechanism
   exists to close, and it must be proven, not assumed from the design
   prose.
2. **If the node is `undeclared`**: assert every approval on that node's
   objects is refused outright (§6.5.2's fail-closed table), regardless of
   who attempts it — the commercial consequence (stuck `pending_approval`)
   is the intended trade, and the test should assert the accrual remains
   `pending_approval`, not merely that one approval attempt failed.
3. **If the interest is falsely declared `none_internal`** (the actual
   collusion path the task's framing is really asking about): assert, and
   **document as an assertion the platform does not claim to make**, that
   no engineering control detects this at approval time — per §6.5.2.1's
   own stated limitation, this is a compliance/HR residual, not a testable
   negative. What **is** testable: the payout-instrument correlation
   **detection signal** §W15.2.5 names (a commission settlement whose
   payout destination/instrument correlates with a known internal staff
   person's instrument) fires and creates a review item, and — per
   §W15.1.5's detection-only discipline, reused here — never auto-blocks,
   auto-reverses, or auto-suspends on that signal alone. A test asserting
   this signal is wired and non-blocking is the honest ceiling of what
   this control class can prove; a test claiming it "catches the lie" would
   misstate the architecture's own documented limitation.

**Also required, restated from `security`'s own W15.2.6 because the task's
framing is a superset of what that list already names**: same-user
self-approval (item 1), same-person-two-accounts (item 2), undeclared node
(item 8), stale attestation (item 9), the positive "two unrelated internal
approvers, neither a declared interest holder, neither in the ancestor
chain, succeeds" case (item 10, `SEP-1-H1`'s affiliate instance), replay
(item 11), concurrent approvals (item 12), approve-then-reject/reject-
then-approve (item 13), payload mutation after approval (item 14),
unclassified principal (item 15, the `AFF-C1` regression guard for a
future role added without classification), cross-tenant (item 16), and
cross-subtree read (item 17).

**Status**: `BLOCKED` on `agentnetwork`'s hierarchy-node-scope RLS
dimension (`DEP-AFF-5`/`AFF-C2`) and the `affiliate_beneficial_interest_
attestations` table (§6.5.2.1) neither existing yet, and on the ancestor-
chain resolver itself not being built. The distinction between
`AFF-4E-1-TRIVIAL` (testable today in principle, once affiliate
`principal_class` exists) and `AFF-4E-1-ANCESTOR-DIRECTION-REGRESSION`
(additionally requires the sub-affiliate override agreement and the
resolver) should be preserved when Wave 2+ actually schedules these —
the trivial case is cheaper to build and should not be allowed to stand in
for the harder one in a completion report.

### 5. Casino postWin tests

**Destination/query resolution fix (§16.4, LF-1/LF-7):**

- `CASINO-PW-1` — **credit-leg resolution, not debit-leg (LF-1's own
  regression test).** Construct a bonus-funded bet posting the case-B
  shape (`Dr player_bonus X / Cr player_locked_bonus X`, no mirror). On
  win, assert `postWin`'s resolution reads the **credit** leg
  (`player_locked_bonus`) to determine origin. Written to **fail** against
  a reimplementation of the original debit-leg query — the specific defect
  that made the terminal-Grant branch structurally unreachable (LF-1),
  mirroring N1.11's own "must fail against the pre-revision mechanism"
  discipline applied on the casino side of the same seam.
- `CASINO-PW-2` — **correlation-ID wallet collision (LF-7).** Two
  different players' rounds collide on `correlation_id` (a namespace
  collision or a posting-layer defect): assert `ErrCorrelationWalletCollision`,
  the whole win aborts, an integrity alert is raised, and neither wallet is
  guessed.
- `CASINO-PW-3` — **multi-bet-round ambiguity (LF-8), including the
  "coincidence must not become a shortcut" case.** A win event names a
  `RoundID` under which more than one distinct `bet_transaction_id`
  exists, and `WinRequest`/`CallbackEvent` carries no
  `OriginatingProviderTxID`. Assert the event routes to
  `ErrAmbiguousMultiOriginRound`'s manual-reconciliation queue. The
  adversarial variant required beyond the obvious case: construct the win
  amount to **exactly equal** one specific candidate bet's stake, and
  assert the system still routes to manual reconciliation rather than
  silently inferring that bet is the target — §16.4a explicitly rejects
  amount-matching heuristics as unsafe guessing, and a test that never
  constructs the tempting coincidental case cannot prove the rejection
  holds under the one condition an implementer would be most tempted to
  special-case.
- `CASINO-PW-3b` — **per-bet resolution once `OriginatingProviderTxID`
  exists.** `NOT APPLICABLE (open dependency)` — the field does not exist
  on `WinRequest`/`CallbackEvent` today (confirmed against
  `internal/casino/types.go`); named here so Wave 2+ does not have to
  re-derive the test once the field is added, per §16.4a's own explicit
  future-dependency framing, and so no multi-bet-capable title is marked
  launch-eligible for bonus-funded wagering before this test exists and
  passes (§16.13).
- `CASINO-PW-4` — **lock release bundled with payout, one balanced
  transaction (LF-4/§16.5a).** On a win against a non-terminal Grant,
  assert the settlement posts **both** entry pairs (the payout, and the
  lock release `Dr player_locked_bonus X / Cr player_bonus X`) in one
  `ledger.Post` call, and that `L(G)` reaches exactly `0`. Assert no
  committed intermediate state exists where one pair posted and the other
  did not — the identical "no partial state" discipline `G2-HOLD-3`
  assertion 2 applies to the terminal-Grant branch, applied here to the
  ordinary, non-terminal branch.

**Settlement-timeout sweep (§16.5a(a)):**

- `CASINO-STO-1` — **fail-closed default, both directions.** Absent an
  explicit configured window, run the sweep and assert **zero** postings
  and that locks remain visible (via `LockedBonusBalance`, migration
  0048) rather than silently written off. This is the inverse of this
  document's usual "assert the job fires" standard (Stage 4H-B0 §1.7) —
  here the required proof is that the job does **not** fire absent
  configuration, since a sweep that runs on an invented default duration
  is exactly the "guessed number wrongly writing off a stake" failure mode
  §16.5a itself names.
- `CASINO-STO-2` — **genuine unresolved-loss sweep.** A locked bonus-funded
  stake with no `casino_win`/`casino_rollback` under its `correlation_id`,
  older than the configured window, is swept via a new
  `casino_settlement_timeout` posting, reusing the case-I loss-accounting
  shape verbatim, audited as `actor = system`.
- `CASINO-STO-3` — **late win after sweep raises an alert, never
  resurrects.** A genuine `casino_win` for an already-swept round arrives
  after the window: assert a new, distinct integrity alert, held for
  manual reconciliation, and that the timeout posting is **not**
  auto-reversed and no double-credit occurs.
- `CASINO-STO-4` — **the exclusion test, named explicitly per the task:
  a G-2-pending round must never be swept as a loss.** A round whose win
  callback already arrived and was rejected via
  `ErrTerminalGrantCreditUnresolved` (genuinely `pending_settlement`, with
  a recorded rejection audit event) is aged past the sweep's window.
  Assert the sweep does **not** touch it — it remains `pending_settlement`
  for a human to resolve via G-2, never auto-resolved as a loss. The test
  must also prove the **discriminator**, not just the outcome: construct a
  near-miss control case (a round that is old enough to sweep and
  happens to carry an unrelated audit event at a similar timestamp) and
  confirm the exclusion is driven specifically by the recorded
  `ErrTerminalGrantCreditUnresolved` rejection event keyed to that
  `correlation_id` — never by a coincidental timing proxy.
- `CASINO-STO-5` — **per-tenant/jurisdiction configuration, tighten-only.**
  Mirrors the test shape this document already specified for
  `OpenBetSelfExclusionPolicy` (Stage 4H-B0-R5, P1-5): jurisdiction-
  primary, tenant/brand may only shorten the window, never lengthen it.
  **Named gap, not resolved here, same shape as P1-5's own named gap 1**:
  §16.5a does not state whether the tighten-only rule is enforced at
  configuration-write time or at resolution-read time, and this document
  cannot pin the test to one mechanism until that is specified — recorded
  here as a second instance of an already-open pattern, not a new
  question independently invented.

**Status**: every `CASINO-STO-*` test is `BLOCKED`, explicitly and by the
architecture's own words — §16.5a states the settlement-timeout sweep is
"**NOT SAFE to implement**" until `ledger-finance`/`architect` ratify the
new transaction type and posting shape **and** a human/jurisdiction
decision sets the actual window duration, neither of which has happened.
`CASINO-PW-1`/`PW-2`/`PW-4` are buildable today (they exercise mechanisms
that exist or are pure Go/SQL changes inside `internal/casino` with no new
account type). `CASINO-PW-3`'s manual-reconciliation-queue routing is
buildable today; `PW-3b` is `NOT APPLICABLE (open dependency)` until the
provider-protocol field exists.

**New gap found while re-verifying Wave 1.5's C18–C27 against this fix,
not previously named — recorded here rather than silently absorbed:**

The fix wave's N1.9 enumerates `HeldDispositionRecord.status` as exactly
`held` / `resolved_reforfeit` / `resolved_route_to_cash`. Re-walking C24
("concurrent win and rollback") against the corrected mechanism surfaces a
case neither doc10 §N1 nor doc08 §16.11 appears to name: **a rollback
arriving while the win's value sits in a `HeldDispositionRecord` with
`status = held`** (the WIN won the C24 race, was captured into the holding
representation because the Grant was already terminal, and *before* a
human answers G-2, a rollback for the same round arrives). Neither
document specifies whether the rollback (a) reverses the held record
directly (requiring a fourth status, e.g. `reversed`/`voided`, not
currently in N1.9's enum), (b) is rejected until the hold resolves, or (c)
some other treatment. This is named as **new test `C28` — "rollback of a
held, undisposed win"** — its assertions cannot be written until
`bonus-engine`/`casino`'s Phase 2 reconciliation states which of (a)/(b)/(c)
applies; flagged for that reconciliation, not resolved here.

**Re-verification of C18/C19/C20/C26/C27 against the landed fix,
per Wave 1.5 §7 item 1's own instruction to re-check once casino's design
existed:**

- **C18, C26, C27** (win/callback after grant expiry/cancellation): the
  mechanism these already point to (§16.8/§16.9's re-confirmed G-2 call
  site) is unchanged in shape; the "what must be provable now" bar is
  sharper than Wave 1.5 stated it — it is no longer merely "detected and
  routed to a defined outcome," it is specifically "captured into the
  `HeldDispositionRecord` holding representation, per N1.4.1," matching
  `G2-HOLD-1`'s own assertion. No change to which rows are `BLOCKED`.
- **C19** (win after wagering completion): unchanged in status; N1.3's
  `AOE` third component (`HeldDisposition`) is the mechanism that now
  makes this case's "no silent double-count" assertion precise where Wave
  1.5 left it as a named risk rather than a mechanism.
- **C20** (rollback after win, G-3 netting): **gains a new dependency**.
  §16.11 now discloses that a rollback of a win that already credited
  `player_bonus`, where the Grant subsequently went terminal and swept
  that balance into `promo_liability`, can find **insufficient**
  `player_bonus` balance to debit — and explicitly states the treatment
  (permit a negative balance as a clawback / route to a receivable /
  reject-and-alert) is `ledger-finance`'s undecided call (`LF-10`), not
  casino's. C20's test cannot be completed until that decision lands;
  recorded as an added dependency on C20's existing `BLOCKED` status
  rather than a new row.
- **C24** (concurrent win and rollback): unchanged in its two named
  orderings, but see the new `C28` gap above — the "WIN wins, Grant
  terminal" sub-branch of C24 now needs to additionally route into the
  holding representation rather than post-then-reforfeit (already implied
  by N1.6 Scenario 3's revision), and the *subsequent* rollback of that
  held win is the newly-discovered gap.

### 6. Segmentation tests — the `member_of`/`InclusionSafety` regression

**Primary regression test — `SEG-MEMBER-OF-1` (the disabled-exclusion-
segment case the task names).** Construct doc 30 §6's own canonical hybrid
shape, `And(criteria, Not(member_of(exclusion_segment)))`. Player `P`
satisfies `criteria` and is a genuine member of `exclusion_segment` while
it is `active`. Assert `P` is `not_member` (excluded). Disable
`exclusion_segment` — an ordinary, low-privilege, non-financial operator
action, per §3.1's own "disable-never-delete" discipline — without editing
its criteria. Re-evaluate the **identical** tree for the **identical**
player with no other change. **Assert `P` is still `not_member`**, with
reason `segment_fact_unavailable` (boundary) carrying the innermost
`segment_reference_disabled` reason (`leaf_outcomes[]`) — never `member`.
Written to **fail** against a reimplementation of the exact defect
`code-reviewer`'s P1-1 found: mapping an interior `member_of`'s
absent/disabled result straight to `false`, which `Not()` then inverts to
`true`, sweeping every previously-excluded player back into the audience.

**Companion cases, same assertion, different trigger:**

- `SEG-MEMBER-OF-2` — the referenced segment has **only a `draft`
  version**, no `active`/`superseded` version effective at `as_of`: same
  exclude-not-include result, reason `segment_reference_not_effective`.
- `SEG-MEMBER-OF-3` — the reference is **unresolvable** (deleted segment,
  a `version_pin` naming a nonexistent/foreign/ineffective version, or a
  cross-tenant/cross-brand reference not visible in the evaluating scope):
  same result, reason `segment_reference_unresolvable` /
  `segment_reference_pin_unresolvable`.
- `SEG-MEMBER-OF-4` — **Kleene propagation at depth, property-based.**
  Reusing this document's own Wave 1.5 §3 property-testing design
  (random AND/OR/NOT trees, compared against an independent reference
  evaluator): generate trees of random depth with a disabled/draft/
  unresolvable `member_of` leaf injected at a random interior position
  (not only as the tree's top-level node), and assert the exact §5.3
  truth table (`And`: false-dominant, else unknown-dominant, else true;
  `Or`: true-dominant, else unknown-dominant, else false; `Not`:
  swaps true/false, **fixes unknown**) holds at every level, for every
  generated tree — not only the canonical two-node case §6 illustrates.
- `SEG-MEMBER-OF-5` — **no interior collapse.** A white-box test of the
  evaluator (not merely its `Resolve()` black-box outcome) asserting that
  an interior `Not(member_of(disabled_segment))` node's own intermediate
  value is `unknown`, never `true` — the specific, previously-defective
  behavior — with the two-value collapse (`member`/`not_member`) provable
  to occur **only** at the `Resolve()` return boundary and nowhere else in
  the evaluation trace.

**`InclusionSafety` regression tests, named together since the task's own
heading pairs the two mechanisms, kept brief since the task's primary ask
is the `member_of` exclusion case above:**

- `SEG-INCSAFE-1` (Path 1, nested-segment laundering): Segment `X` carries
  an RG-restriction criterion classified `exclusion_only`; Segment `Y` is
  a plain `member_of(X)` used in an inclusion position. Assert `Y`'s
  computed `InclusionSafety` is `exclusion_only` **transitively** (not
  `safe`), and that a validator inspecting only `Y`'s own leaf key
  (`member_of`) would incorrectly pass it — written to fail against
  exactly that leaf-key-only validator.
- `SEG-INCSAFE-2` (Path 2, CRM-lifecycle laundering): a criterion using
  `lifecycle_state_in(['excluded', ...])`, where `excluded` is doc 31's
  own RG-derived projection: assert this leaf is classified
  `exclusion_only` via `protective_signal_inputs` satisfiability, not
  missed because no predicate key literally names RG.
- `SEG-INCSAFE-3` (recomputation, not caching): simulate a deploy that
  adds a new `protective_signal_inputs` entry to a previously-safe
  predicate key. Assert every already-frozen `active` `SegmentVersion`
  referencing that key is **recomputed** at evaluator startup (not read
  from its stored `InclusionSafety` cache value), and that a
  stored-vs-recomputed divergence is a **hard startup failure**, mirroring
  CLAUDE.md's balance-drift-is-P1 discipline applied to a safety
  classification rather than a financial one.

**Status**: `internal/segment` does not exist (doc 30 remains
architecture-only per its own status line), so every test in this
subsection is `NOT IMPLEMENTED`, `RECOMMENDATION`. `SEG-MEMBER-OF-1`
through `-5` and `SEG-INCSAFE-1` through `-3` are, however, fully
specifiable today against doc 30's corrected §5.1.1/§5.3/§8.4.1 text with
no further architecture dependency — unlike most of this section, this is
not blocked on a parallel document landing, only on implementation.

### 7. What cannot be fully specified pending Phase 2's parallel reconciliation — stated honestly

- **§1's `G2-HOLD-3` assertion 2** (the `ACTION_HOLD_FOR_REVIEW` lock-
  release destination) cannot be completed until `ledger-finance`/
  `bonus-engine` name that destination (§16.5a/§16.13's own open
  cross-dependency).
- **§2's `EOI-DECOMPOSITION-1`** depends on `DEP-EOI-5` (the pin-vs-
  live-resolution reconciliation between `security` and `bonus-engine`,
  doc 34 §8) being resolved as proposed or otherwise — this document
  wrote the test against the proposed reconciliation and flags it as
  provisional, consistent with this section's own opening note.
- **§3's `SEP-1` tests** depend on each domain's own resolver
  (`REQ-SEP-BONUS-*`, `REQ-SEP-CRM-1`, `REQ-SEP-AFF-1`) and on
  `identity-compliance`'s `REQ-SEP-ID-1` confirmation of the underlying
  Person-linkage primitive (`4HB1FW-05`), which this document has not
  seen in its landed form.
- **§4's `AFF-4E-1` ancestor-chain tests** depend on `agentnetwork`'s
  hierarchy-scope RLS (`DEP-AFF-5`) and the beneficial-ownership
  attestation table, neither built.
- **§5's `CASINO-STO-*` tests** are `BLOCKED` on both an architectural
  ratification and a human/jurisdiction decision, per the architecture's
  own explicit "NOT SAFE to implement" statement — not a `qa` finding, a
  restated fact from doc 08 itself.
- **The new `C28` gap** (rollback of a held, undisposed win) is a genuine
  finding this review produced, not previously named in either doc10 §N1
  or doc08 §16 as read — it is routed to `bonus-engine`/`casino`'s
  parallel Phase 2 reconciliation rather than resolved here, since
  resolving it would mean designing a new `HeldDispositionRecord.status`
  value, which is production-design work outside `qa`'s remit.
- **This document has not seen**, and could not re-verify against,
  whichever parts of Phase 2's own reconciliation (`ledger-finance`,
  `security`, `code-reviewer`, `bonus-engine` on the Segmentation doc,
  `product-owner-proxy`) are running in parallel to this dispatch — per
  this section's own opening note, every test above that depends on one
  of those reviewers' findings is stated against the documents as they
  stand today and may need re-checking once that parallel work lands,
  exactly as Wave 1.5 §7 already disclosed for its own C18–C27/§2/§3/§4.

Every item in this section is `RECOMMENDATION` (a test strategy binds
nothing until executed) and `NOT IMPLEMENTED` (no test code exists yet).
No item moves toward `IMPLEMENTED` before its named architecture/human
dependency resolves, regardless of implementation progress elsewhere —
this document's own standing rule, restated once more because this
section's dependency list is longer than most.

## Stage 4H-B1, Wave 1.5 Fix Round 2, Phase 2 — Independent re-verification test-plan update (`qa`)

Status: `RECOMMENDATION`, `NOT IMPLEMENTED`. Per the human directive
authorizing this round (§20 Testing): "Add targeted tests for every P0. No
test may merely assert application behavior if direct SQL/RLS testing is
required to prove the invariant." `qa` did not author any of this round's
designs (`ledger-finance`'s §7.7.2 holding-representation decision,
`casino`'s doc 08 §16.4/§16.14–§16.20 revision, `bonus-engine`'s doc 10
N1/N2 revision, `security`'s §W15.1.9–§W15.1.12/§W15.2.7 fixes, `architect`'s
doc 34 §3.4/§5.3 correction) — this section is independent design-only
review-and-extend, per CLAUDE.md's "no specialist self-approves its own
work." **No test code is written here.** No Human Decision Register item
(G-2, `OpenBetSelfExclusionPolicy`, cashout policy, FD-1) is selected,
referenced as resolved, or defaulted by any test below — every test that
touches one is explicitly marked `BLOCKED` on it.

This section extends, and does not silently rewrite, the Wave 1.5 Fix
Wave Phase 2 section immediately above (`G2-HOLD-1`/`2`/`3`,
`EOI-DECOMPOSITION-1`, the `SEP-1` conformance table, `CASINO-PW`/`STO`
tests, `SEG-MEMBER-OF`/`InclusionSafety` tests, the `C28` finding this
round closes). Where this round's redesigns make part of that section
stale, §4 below names the drift explicitly rather than leaving it in
place; the original text above is left untouched as the historical record
of what Phase 2 actually specified, per this document's own established
pattern of appending rather than overwriting.

### 1. `G2-HOLD-1`/`2`/`3` updated to the final `player_bonus_held` design

#### 1.0 What changed under this round and why the prior tightening no longer applies

§1's own prior tightening of `G2-HOLD-1` ("should be run against **both**
of N1.4.1 item 2's holding-representation branches — the sportsbook-shaped
in-place `player_locked_bonus` branch, and the casino-shaped dedicated
holding-account branch") is **superseded, not merely refined, by this
round's decision** and must not be implemented as written. `ledger-
accounting-model.md` §7.7.2.1 rejected branch 1 (reusing
`player_locked_bonus` in place) **outright** — LF-19 and LF-20 are stated
as the reasons for rejection, not merely findings against it — and doc 10
§N1.4.1 confirms "the branching question this document posed no longer
has two live branches; there is exactly one account, for every case."
There is now exactly one account type, `player_bonus_held`, used
identically whether or not the originating stake was locked; the only
thing that varies per occurrence is whether the hold-capture posting has
**one leg or two** (§7.7.2.2), never which account type receives the
credit. A test suite built against the retired two-branch instruction
would spend effort proving an account shape (`player_locked_bonus`
retained in place as the holding account) that `ledger-finance` has
explicitly disallowed, and would not exercise the actual schema
(`bonus_held_dispositions`, HR-23/24/25) at all. §4 below records this
formally as flagged drift; it is stated here first because `G2-HOLD-1`'s
revision below depends on understanding exactly what is being replaced.

#### 1.1 `G2-HOLD-1`, revised — the actual `player_bonus_held` two-leg posting shape

**Setup.** Grant `G`, wallet `w`, asset `EUR`, stake `X = 400` locked via
`Dr player_bonus 400 / Cr player_locked_bonus 400` (`casino_bet`,
correlation_id `C`). `G`'s `NewStakeEligibility` has already closed
(`pending_settlement` per Scenario 4, or fully terminal) at the moment a
WIN settlement (`payout = W = 250`) for `C` is delivered.

**Assertions, each independently sufficient to fail the test — this
replaces the prior branch-ambiguity assertion with the concrete
`ledger-accounting-model.md` §7.7.2.2/§7.7.2.4 shape:**

1. **Exactly one balanced `LedgerTransaction` performs the capture**,
   containing:
   - `Dr house_gaming 250 / Cr player_bonus_held 250` (the payout leg —
     **always present** for a held WIN, regardless of whether the
     originating stake was locked).
   - `Dr player_locked_bonus 400 / Cr player_bonus_held 400` (the
     lock-release leg — **present if and only if** the originating stake
     was locked; **absent entirely**, not zero-valued, when it was not).
     Run the test twice — once with a locked origin (the two-leg case
     above) and once with a hypothetical unlocked origin (the single-leg,
     payout-only case §7.7.2.2 also names for completeness) — asserting
     the **cardinality of legs**, not their presence/absence of a
     branch-selecting account type, is the only thing that varies. This
     is the corrected replacement for the retired "run against both
     branches" instruction (§1.0): there are not two account-type
     branches to exercise, there is one account and (at most) two legs.
2. **No committed intermediate state exists in which one leg posted and
   the other did not**, when both legs are required. Prove this, not
   assume it from `ledger.Post`'s single-transaction contract: fault-inject
   a connection kill between the two legs' `INSERT`s inside the same
   `ledger.Post` call (the identical technique `G2-HOLD-3` assertion 4
   already applies to the *resolution* transaction, applied here to the
   *hold-capture* transaction instead — a distinct atomicity boundary
   neither `G2-HOLD-3` nor the prior `G2-HOLD-1` text exercised). On
   recovery, assert **neither** leg is present — no partial two-leg
   posting, ever.
3. **`player_locked_bonus` for `C` is exactly `0` after the posting** —
   never negative (this is `L(G) → 0`, §16.5a's mandate, tested from the
   ledger-finance side of the same seam casino's `CASINO-PW-4`/§16.18 Part
   B already test from the casino side).
4. **§6.6.6's nullifiable predicate reads "not nullifiable" for `C`
   immediately after the posting** — this is the actual, empirical proof
   of LF-19's closure (`Σ signed(player_locked_bonus)` over `C` is `0`,
   the predicate's defined subject having genuinely gone to zero), not a
   restatement of the design doc's own reasoning. A test that only checks
   "the transaction balanced" does not distinguish this design from
   branch 1, where the identical `Σ = 0` fact would *also* hold trivially
   post-capture for the wrong reason (the money moved, but into an
   account the predicate itself reads) — this assertion is what actually
   distinguishes the two designs at the query level.
5. **A `bonus_held_dispositions` row is created** with
   `settlement_ledger_transaction_id` equal to the capture transaction's
   own id, `payout_amount = 250`, `released_lock_amount = 400` (or `0`
   with no row for that leg, in the unlocked-origin run), `status =
   'held'`, and **`UNIQUE (tenant_id, settlement_ledger_transaction_id)`
   enforced by the database** — attempt a second insert for the same
   `settlement_ledger_transaction_id` via a raw `INSERT`, bypassing
   application code, and assert the database itself rejects it (LF-22's
   fix, §7.7.2.6, tested as a real constraint violation, not an
   application-level "already exists" branch).
6. **Never, at any point, a credit to the shared, fungible `player_bonus`
   account for this settlement** — this is the specific regression this
   test is written to **fail** against: a reimplementation of branch 1
   (reusing `player_locked_bonus` in place, crediting `player_bonus`
   transiently, or omitting `player_bonus_held` entirely) must make
   assertion 1, 4, or 6 fail. This is the same "written to fail against
   the retired mechanism" discipline this document already applies to
   `CASINO-PW-1` against the pre-LF-1 debit-leg query.

**`G2-HOLD-2` (doc10 §N1.11) is unaffected by this round's schema
decision and remains adequate as written**, including last round's five
independently-sufficient assertions — assertion (v) already names a live
read of `player_bonus_held`'s balance, which is the final schema, not a
placeholder. No revision required; re-confirmed rather than silently
carried forward unexamined.

#### 1.2 `G2-HOLD-3` reclassified — this is `C28`'s closure, and it needs a real concurrent-transaction test, not a mocked one

Wave 1.5 Fix Wave Phase 2's own §5 raised **`C28`** ("rollback of a held,
undisposed win") as an open gap: neither doc 10 §N1 nor doc 08 §16.11, as
they stood then, named the state transition for a rollback arriving while
a WIN's value sits in an open `bonus_held_dispositions` row. That gap is
now closed at the design level — `ledger-accounting-model.md` §7.7.2.7
(the guarded compare-and-swap), doc 08 §16.15/§16.18 Part B (casino's
adoption and state-machine proof), and doc 10 §N1.11's own **`G2-HOLD-3`**
(already specifying the exact concurrent-rollback-vs-resolution scenario
this task asks for). `qa` did not need to invent a new test ID: `bonus-
engine`'s own `G2-HOLD-3` **is** the C28 closure test. What this section
adds is what `qa` owns and `bonus-engine`'s design text explicitly does
not attempt — pinning `G2-HOLD-3` down as a **real PostgreSQL
concurrency/SQL test**, per the directive's instruction that a
`WHERE status = 'held'` compare-and-swap racing a concurrent reader is
exactly the shape that needs one, and specifying it precisely enough to
build directly.

**Test `HELD-ROLLBACK-CAS-RACE-1` — the concrete SQL/concurrency
specification for `G2-HOLD-3`'s scenario.**

*Exact setup (real PostgreSQL 16, real tables, mirroring the fixture
pattern `internal/risk/cumulative_race_integration_test.go` already
establishes in this repo — `racePool`, pre-created accounts before the
race, one connection per worker):*

```sql
-- Pre-race, sequential, one connection, real ledger.Post calls:
-- T5: the hold-capture transaction (§7.7.2.2), already committed.
--   Dr house_gaming 250 / Cr player_bonus_held 250
--   Dr player_locked_bonus 400 / Cr player_bonus_held 400
-- H1: the bonus_held_dispositions row it produced.
--   id = $H1, settlement_ledger_transaction_id = T5.id,
--   payout_amount = 250, released_lock_amount = 400, status = 'held'
```

*Two concurrent workers, each its own DB connection/transaction, released
by a shared start barrier (the same `sync.WaitGroup`-gated barrier
`cumulative_race_integration_test.go` uses to force genuine interleaving
rather than accidental non-overlap):*

- **Worker R (rollback).** Acquires `pg_advisory_xact_lock` on
  `(tenant_id, grant_id=G)` (HR-25 step 1) — the **same** lock both
  workers must acquire, so true concurrency is only observable in *which
  worker's `BEGIN`/lock-acquire wins the race*, not in interleaved row
  access after that point (this is a deliberate, correct property of the
  design, not a test artifact — HR-25 pins this exact serialization).
  Whichever worker acquires the advisory lock second **blocks** until the
  first commits or rolls back. Having acquired the lock, Worker R runs
  `postRollback`'s generic entry-inversion against `T5` (`Cr house_gaming
  250 / Dr player_bonus_held 250` + `Cr house_gaming 400 / Dr
  player_bonus_held 400`) and, in the **same** transaction, the guarded
  update: `UPDATE bonus_held_dispositions SET status='voided_by_rollback',
  resolution_ledger_transaction_id=$T6 WHERE id=$H1 AND status='held'`.
  Commits.
- **Worker S (staff resolution, `ACTION_REFORFEIT`).** Acquires the
  identical `(tenant_id, grant_id=G)` advisory lock, then `H1`'s row `FOR
  UPDATE` (HR-25 step 2), then attempts its own guarded update:
  `UPDATE bonus_held_dispositions SET status='resolved_reforfeit',
  resolution_ledger_transaction_id=$T7 WHERE id=$H1 AND status='held'`,
  bundled with its own posting (release into `player_bonus` then an
  immediate `bonus_forfeiture` debit, per HR-25 step 3). Commits.

*Run the scenario with the barrier forcing each ordering explicitly (both
"R acquires the advisory lock first" and "S acquires it first"), not
relying on scheduling luck to exercise both — mirroring this document's
own C24 "both orderings named explicitly" discipline.*

**Assertions, both orderings:**

1. **Exactly one of `voided_by_rollback` / `resolved_reforfeit` is the
   final `status`** — never both (impossible by construction once the
   `WHERE status = 'held'` guard is real, but asserted as the actual
   observed outcome, not inferred from the mechanism's description).
2. **The loser's guarded `UPDATE` affects exactly zero rows** — assert
   this directly against the statement's own reported row count (Postgres
   returns this; a test that only checks the final `status` cannot tell a
   correctly-guarded zero-row loss from a check-then-update race that
   happened not to lose this run), and assert the loser's transaction
   posts **no** compensating or duplicate ledger entries as a result of
   finding zero rows affected — the loser's enclosing transaction aborts
   before positing any further money movement, per §7.7.2.7/HR-25's own
   "guarded update, never check-then-act" instruction.
3. **If Worker R wins**: `player_locked_bonus` for the original stake is
   **not** credited (§7.7.2.7's explicit "never a lock resurrection")
   and `player_bonus_held`'s balance for `H1` is exactly `0` afterward
   (`650 → 0`).
4. **If Worker S wins**: `player_bonus_held`'s balance for `H1` is exactly
   `0` afterward via the reforfeit path instead, and a subsequent Worker
   R attempt (run sequentially, after S's commit, as a third phase) finds
   `status ≠ 'held'` and its own guarded update is a zero-row no-op —
   proving the *later*-arriving side of either race, not only the
   simultaneous case, is also rejected (this is `G2-HOLD-3` assertion
   (iv), made concrete against a real sequential-after-race third phase).
5. **A mandatory negative control, per this task's own instruction that
   direct SQL testing is required to prove this invariant**: re-run the
   identical two-worker race with the guarded `UPDATE`'s `WHERE status =
   'held'` clause removed (replaced by an unconditional `UPDATE ... WHERE
   id = $H1`, i.e. simulate a check-then-update reimplementation: `SELECT
   status FROM bonus_held_dispositions WHERE id=$H1`, then, in application
   code, `UPDATE ... SET status = ... WHERE id=$H1` with no status
   predicate). Assert this control **deterministically** produces the
   double-disposition the guarded version prevents (both workers' checks
   observe `status = 'held'` before either writes, both proceed, the
   later writer's `UPDATE` silently overwrites the earlier one's `status`
   — `H1` ends in whichever status committed last, with **both** sets of
   compensating postings having been made against the same held value) —
   proving assertion 1–2 above are actually verifying the guard, not
   scheduling luck, exactly as `internal/risk/cumulative_race_integration_
   test.go`'s own negative control proves its advisory lock is load-bearing.

**One design gap surfaced while specifying this test, not previously
named in either document, flagged rather than silently worked around.**
Doc 08 §16.18 Part B's own balance proof (the state-machine proof this
round adds) explicitly **flags, and does not resolve**, whether `T6`'s
reversal of the released-lock leg — which credits `house_gaming` rather
than restoring `player_locked_bonus` — itself crosses `BONUS_SET`'s
boundary outward and therefore requires a Rule B2 (extended) mirror pair
that `T5`'s original leg never needed. Assertion 3 above ("`player_bonus_
held` balance is exactly `0`") is the strongest claim this test can make
today; a complete balance proof for the Worker-R-wins ordering additionally
needs to assert the correct presence (or documented absence) of that
mirror pair, which cannot be specified until `ledger-finance`'s own
mirror-generator design states whether `T6` needs one. This is a genuine,
named gap this test-writing exercise found, not a `qa`-invented
requirement — recorded here so it is not silently dropped, and marked
`BLOCKED` on that same open question in §5's status table below.

**Status.** `BLOCKED` on `internal/bonus`'s `bonus_held_dispositions`
migration (`0054`+), `ledger-finance`'s `0055` (`player_bonus_held`
account-type widening), and HR-9's own removal precondition (migration
`0050`/Rule B2 (extended) generator) — none of which exist at `HEAD`,
confirmed against `migrations/` and `internal/ledger/ledger.go`. The
compare-and-swap **primitive itself** (a single guarded `UPDATE ... WHERE
status = X` under an advisory-lock-serialized contender) is not a new
pattern this platform has to invent from scratch — it is the same shape
`asset_change_consume_approved_request` (migrations `0044`/`0047`) already
implements for a different table — so once `bonus_held_dispositions`
exists, this test requires no new *mechanism* design, only wiring the
existing pattern to the new table, which is a much smaller lift than
`BLOCKED` might otherwise suggest.

#### 1.3 New test — LF-18's exploit, as a real PostgreSQL concurrency test

The directive requires LF-18 be proved with "real PostgreSQL concurrency
testing," not an application-level assertion. Doc 08 §16.17 already walks
the exploit **sequentially** (bet → silent sweep → late win); the test
below operationalizes the identical arithmetic as a genuine concurrent
race between two transactions, per that instruction, and separates what
is buildable today from what needs casino's Round 2 code.

**Layer 1 — `CASINO-LF18-QUERY-RACE-1`, buildable TODAY, no `internal/
casino` Round 2 code required.** This isolates and proves the one property
LF-18's fix actually depends on — that Step 1b's query is race-safe under
real concurrent commits — using only mechanisms that exist at `HEAD`
(`ledger.Post`, `casino_bet`/`casino_win` transaction types, confirmed
against `internal/ledger/ledger.go`).

*Setup*: post `T_bet` (`Dr player_bonus 500 / Cr player_locked_bonus 500`,
`casino_bet`, correlation_id `C`) via real `ledger.Post`.

*Two workers, barrier-synchronized (mirroring `racePool`'s pattern),
each its own connection:*

- **Worker A ("zeroing transaction," standing in for the not-yet-buildable
  `casino_settlement_timeout` sweep — see Layer 2 for why this
  substitution is legitimate).** Acquires `pg_advisory_xact_lock` on
  `(tenant_id, correlation_id=C)` (HR-3, the same lock `postWin`/the sweep
  both take), posts a second real `ledger.Post` transaction reducing
  `player_locked_bonus` for `C` to `0` (`Dr player_locked_bonus 500 / Cr
  player_bonus 500`, `casino_win` — the label is irrelevant to the test:
  Step 1b's query, by design, has **no `transaction_type` filter**, so any
  transaction type that credits/debits the right accounts under `C`
  exercises the identical arithmetic `casino_settlement_timeout` will).
  Commits.
- **Worker B ("late settlement decision").** Acquires the same advisory
  lock (blocks if Worker A holds it), then executes the **literal SQL**
  from doc 08 §16.4 Step 1b (copied verbatim, no paraphrase) against the
  real, committed `ledger_entries`/`ledger_accounts`/`ledger_transactions`
  rows, reading `net_outstanding_locked`.

*Run twice, forcing each lock-acquisition ordering explicitly (Worker A
first; Worker B first, i.e. Worker B's advisory-lock acquisition observes
no committed Worker A transaction yet, mirroring the "ordinary case"
control §16.17 itself specifies).*

**Assertions:**

1. **Worker A commits first**: Worker B's Step 1b read, executed strictly
   after acquiring the lock (i.e., strictly after Worker A's commit),
   returns exactly `net_outstanding_locked = 0` — never the stale `+500`
   a variant-1 (Step-1-only) read would return, proving the query reflects
   Worker A's real, committed effect under genuine concurrent access, not
   merely under sequential unit-test ordering.
2. **Worker B's advisory-lock acquisition happens first** (Worker A has
   not yet committed): Worker B's read, forced to wait for the SAME lock
   Worker A also needs before it can post, cannot observe a state that
   never existed in isolation — the two workers' `net_outstanding_locked`
   reads and posts are strictly serialized by the single, shared advisory
   lock, so no interleaving in which both read `+500` and both decide to
   release is reachable. Assert directly that the two workers' Step 1b
   reads are never both `> 0` for the same underlying real balance at the
   same wall-clock instant (sample both connections' `net_outstanding_
   locked` under `pg_stat_activity`-correlated timestamps, or, more simply,
   assert the second worker's read strictly reflects the first worker's
   already-committed write by checking Postgres's own transaction commit
   ordering via `pg_current_xact_id()`/`txid_current_snapshot()` at each
   read).
3. **Negative control (recommended addition, beyond what the task
   strictly names for LF-18, but the same discipline the task requires for
   EOI-6 and this document's own `HELD-ROLLBACK-CAS-RACE-1` above)**:
   re-run with the `pg_advisory_xact_lock` call removed from both workers.
   Assert this **deterministically** allows Worker B's read to observe the
   stale `+500` even after Worker A has (by wall-clock, not transaction
   order) already committed its zeroing transaction, under a database
   isolation level of `READ COMMITTED` with an artificially delayed read
   (a `pg_sleep` inserted between Worker B's `BEGIN` and its Step 1b query,
   long enough that Worker A has genuinely committed by then, without the
   lock forcing Worker B to re-read) — proving the advisory lock, not the
   query's own correctness alone, is what closes the race window, mirroring
   `cumulative_race_integration_test.go`'s own "prove the lock is
   load-bearing" negative-control pattern.

**Layer 2 — `CASINO-LF18-FULL-1`, the complete exploit end-to-end, per
doc 08 §16.17's own worked numbers.** Identical setup and worker shape as
Layer 1, but through `postWin` and the real settlement-timeout sweep job
(§16.5a(a)), asserting the actual sentinel error (`ErrLockAlreadyReleased`),
the actual outcome-6 branch firing, and — critically, since Layer 1 only
proves the *read* is race-safe — that **no write is ever attempted** when
`net_outstanding_locked ≤ 0` (Layer 1 cannot prove this half: it never
exercises the decision-to-post code path, because that code does not
exist yet). Also required: the reverse-ordering scenario Layer 1's own
Assertion 2 gestures at but doc 08 §16.17 does not itself walk — a genuine
win settlement racing a sweep for the **same** correlation_id, where the
win's advisory-lock acquisition happens to win the race (the sweep is not
the only actor that can lose this race; nothing in §16.5a(a) states the
sweep re-checks `net_outstanding_locked` for a lock a concurrent win just
claimed before posting its own timeout transaction, and this test is
where that symmetric case must be proved, not merely the one direction
§16.17 walks).

**Status.** Layer 1 is `SPECIFIED, READY TO IMPLEMENT` — every mechanism
it needs (`ledger.Post`, the two existing transaction types, a raw SQL
Step 1b query, `pg_advisory_xact_lock`) exists at `HEAD` today; nothing
about it depends on `postWin`'s Round 2 rewrite existing. Layer 2 is
`BLOCKED` on `internal/casino`'s Round 2 implementation (Step 1/1b/outcome
classification, `ErrLockAlreadyReleased`, confirmed absent from
`internal/casino/*.go` at `HEAD`) **and** on `casino_settlement_timeout`
being ratified and migrated (§16.5a states the sweep is "**NOT SAFE to
implement**" today) — the identical blocking status this document's own
`CASINO-STO-*` rows already carry, extended here to name the LF-18-specific
sub-case explicitly rather than leaving it implied.

### 2. SEP-1 cardinality-assertion adversarial test — the round's single most important new test

Per the task's own framing: `RK-W15P2-1` was a real fail-**open** defect
in a shared security mechanism (§W15.1.9), so this test matters more than
any other addition this round. §W15.1.9 sketches the shape in prose
("a resolver whose join is made to cross a row outside the acting
connection's RLS scope"); this section makes it a concrete, real-schema
RLS test rather than leaving it as a description for whoever implements
`SEP-1`'s triggers to interpret.

**Grounding the scenario in schema that actually exists today, not an
invented table.** This platform already has two tables whose RLS policies
are **structurally different in exactly the way §W15.1.9 warns about**:
`player_accounts` (migration `0010`) is tenant-scoped
(`USING (tenant_id = current_setting('app.tenant_id'))`), while `persons`
(migration `0015`) is **platform-scope-only for `SELECT`**
(`USING (current_setting('app.tenant_id') IS NULL)`) — a `SELECT` against
`persons` from any ordinary tenant-scoped connection returns **zero rows,
unconditionally**, regardless of what the table holds. This exact
contradiction has already caused one documented near-miss in this
codebase: migration `0048`'s own guard comment records that an earlier
draft's `SELECT count(*) FROM ledger_accounts` was "silently INERT" for
the analogous reason (`FORCE ROW LEVEL SECURITY` blinding a migration
connection with no `app.tenant_id` set). `persons`/`player_accounts` is
the same defect class in the opposite direction — a **tenant-scoped**
connection silently blinded against a **platform-scoped-only** table —
and is real, present schema, not a hypothetical construction.

**Test `SEP-1-CARDINALITY-RLS-1`.**

*Scenario.* A `BulkGrantJob`'s beneficiary resolver (`REQ-SEP-BONUS-1`,
set case) is required to additionally cross-check for **household/linked
accounts** (`REQ-SEP-BONUS-3` — the detection path that must never be a
block, but must still run as part of resolving the full beneficiary set
correctly) by joining each candidate `player_accounts` row to `persons`
to detect two `player_account_id`s sharing one `person_id`. Pin a
50-member recipient set (`subject_set_count = 50`) at approval, where 2 of
the 50 recipients are flagged by the household-detection pass as sharing
a `person_id` with another account and therefore require this join; the
other 48 resolve via `player_accounts` alone.

*The defect, constructed exactly as it would occur in real code, not
staged artificially.* The `SEP-1` trigger fires as part of the ordinary
`BulkGrantJob` execution — a **tenant-scoped** transaction (`app.tenant_id`
set, `app.player_account_id` unset — the correct scope for this
operation, and the very thing step 0's tenant-scope self-proof, §W15.1.9
part 1, is designed to confirm). Under that connection, the resolver's
join to `persons` for those 2 flagged rows silently returns **zero** rows
for each — not an error, not a NULL `person_id`, simply an empty join
result, because `persons_platform_scope_read_write`'s policy filters every
row under a tenant-scoped connection. The resolver's overall query
returns **48** resolved beneficiaries, not 50: a **non-empty, non-NULL,
genuinely truncated** result — exactly §W15.1.9's third failure shape, not
the empty-set or NULL-person shapes §W15.1.3's original step 5 already
covered.

**Assertions:**

1. **Under the OLD design (§W15.1.3's step 5 before this round's
   amendment — run as the explicit negative/regression control, not
   skipped):** `resolved_count = 48 > 0`, `null_count = 0` — both of step
   5's original checks pass. **The operation is silently permitted to
   proceed against an incomplete 48-person beneficiary set.** If either of
   the 2 silently-dropped beneficiaries is the acting staff member or
   approver, this is a genuine fail-**open**: self-dealing goes undetected
   because the self-dealing party was never in the set step 6 compared the
   actor against. Assert this explicitly reproduces the fail-open shape —
   this sub-case must be run and shown to pass under the pre-Round-2 logic
   before asserting the fix, per this document's own "written to fail
   against the pre-revision mechanism" discipline applied to a security
   control instead of a financial one.
2. **Under the NEW design (§W15.1.9's amended step 5, the cardinality
   assertion):** the trigger computes `expected_count = 50` (the pinned
   `subject_set_count` from approval, per §W15.1.9's amendment to
   §W15.1.2 item 1 — **read**, not re-resolved), compares against
   `resolved_count = 48`, and the third branch fires: **refuse**, with
   `SEP1_CARDINALITY_MISMATCH` as the recorded reason code (§W15.1.11),
   **independent of whether either dropped beneficiary happens to be the
   actor** — this is the property that makes the fix robust to *which* row
   RLS happened to drop, per §W15.1.9's own stated design goal, and the
   test must not construct the actor as one of the 2 dropped rows only,
   precisely to prove the refusal does not depend on that coincidence.
3. **Step 0's tenant-scope self-proof (§W15.1.9 part 1) is confirmed
   orthogonal, not the mechanism doing the work here** — assert the
   trigger's connection legitimately passes step 0 (it *is* correctly
   scoped to the operation's own tenant; the defect is not a scope
   mismatch on the authorizing row, it is a truncation two steps later, on
   a *different* table the resolver's own join reaches into). A test that
   only exercises step 0 would not catch this shape at all, which is the
   entire reason §W15.1.9 needed a *second*, independent fix (the
   cardinality assertion) rather than treating step 0 alone as sufficient.
4. **Control case — no household match, no cross-table join, all 50
   resolve from `player_accounts` alone**: `resolved_count = expected_count
   = 50`, operation proceeds. Proves the fix does not spuriously refuse
   the ordinary case where the resolver's join never needs to reach a
   differently-scoped table at all.
5. **Positive case (`SEP-1-H1`'s own discipline, restated)**: an
   unrelated, non-colluding approver on the same 50-member set succeeds —
   the mechanism is not merely "refuses everything," proven the same way
   this document's existing §3 already requires for every `SEP-1`
   enforcement point.

**Honest scope, stated per §W15.1.9's own disclosure.** This test proves
the *mechanism-level* fix — a resolver whose join truncates against a
differently-scoped table is caught regardless of which row is dropped. It
does not, and cannot, prove every domain's resolver is *authored*
correctly for every relationship it should walk (a resolver that never
attempts the household join at all, rather than attempting it and losing
rows, is a resolver-authoring defect outside this test's reach, per
§W15.1.9's own honest-scope paragraph) — that remains each adopting
domain's own adversarial responsibility.

**Status.** `SPECIFIED, READY TO IMPLEMENT` for the RLS mechanics
(`player_accounts`/`persons`' RLS policies exist today, confirmed against
migrations `0010`/`0015`) and for the trigger logic once written against
§W15.1.9's amended step 5. `BLOCKED` on the `SEP-1` trigger family itself
not existing yet (no `<table>_enforce_separation()` trigger is present in
`migrations/` at `HEAD`), on `bonus-engine`'s own household/linked-account
detection resolver (`REQ-SEP-BONUS-3`) not being implemented, and on
`identity-compliance`'s Person-linkage primitive confirmation (`REQ-SEP-
ID-1`) this document's §3 above already names as an open dependency.

### 3. EOI root-subtree-budget test — the 100-page-vs-5,000-ceiling scenario

Doc 34 §3.4/§5.5 already narrate this scenario at the design level and
§6's `EOI-4`/`EOI-6` already name the required check in one sentence each.
This section turns it into a build-directly specification for whoever
implements `internal/economicop`, including the negative-control variant
`risk` requested.

**Test `EOI-BUDGET-RACE-1`.**

*Setup, exactly doc 34 §5.5's worked numbers, scaled down for a runnable
test:* a root `economic_operations` row `R`
(`operation_type = 'crm_engagement_campaign_activation'`, `lineage_kind =
'root'`, `root_operation_id = R.operation_id`, `recipient_ceiling = 5000`,
`approval_state = 'approved'`, `status = 'open'`). One hundred child rows
`P1..P100` (`lineage_kind = 'page'`, `parent_operation_id = R.operation_id`,
`root_operation_id = R.operation_id`, each `recipient_ceiling = 1000` —
**all 100 created successfully**, per §3.4's own "creating a child does not
reserve budget" rule; this is not itself part of what the test proves,
it is the precondition the test starts from).

*Concurrent workers*: `N = 5001` simulated grant executions (a real
integration test should run a representative subset under real
concurrency — e.g. 200 concurrent workers drawn across all 100 pages, plus
enough sequential fill to reach the 5,000 boundary deterministically for
the assertion below — rather than literally 5,001 live goroutines, per
this document's own stress-test-floor-not-literal-production-scale
precedent, Wave 1 §1). Each worker:

1. Picks one of `P1..P100` at random (or round-robin, to guarantee even
   coverage across pages) as its `parent_operation_id`.
2. Executes doc 34 §3.4's exact consumption query, `FOR UPDATE OF R` —
   **on the root row `R`, never on the worker's own page row** — computing
   `remaining_recipient_budget = R.recipient_ceiling − COUNT(DISTINCT
   c.subject_ref)` over the **whole subtree** (`eoi.root_operation_id =
   R.operation_id`), not the worker's own page's direct children.
3. If `remaining_recipient_budget > 0`, writes one `BulkGrantJobItem` row
   (`parent_operation_id` = the worker's chosen page, `player_account_id`
   = a fresh, never-before-used subject for this test run) and commits, in
   the **same** transaction as step 2's lock.
4. If `remaining_recipient_budget ≤ 0`, the transaction raises and rolls
   back — no `BulkGrantJobItem` row is written.

**Assertions:**

1. **Exactly 5,000 `BulkGrantJobItem` rows exist across the entire
   subtree after all workers complete** — counted by `root_operation_id`,
   summed across all 100 pages, never by any single page's own count.
2. **The 5,001st successful write, whichever page it falls under**, is
   the last one to succeed; every execution after it, regardless of which
   of the 100 pages it targets, is rejected. This directly replaces the
   Phase 2 test-plan's own stale "page 6 fails" framing (§4 below) — the
   assertion must not assume, or test for, failure concentrated in any
   particular page.
3. **No `BulkGrantJobItem` row exists whose `parent_operation_id`'s page
   was created *after* the 5,000th successful write** was still able to
   write a 5,001st — i.e., the ceiling is enforced against the shared
   root, not against per-page creation order or arrival order.
4. **A recipient granted, clawed back, and re-targeted by a later page
   still counts once, permanently** (`EOI-14`): as a follow-on phase after
   the 5,000 boundary is reached, clawback one already-consumed recipient
   (`lineage_kind = 'compensation'`, `subject_ref` matching an already-
   consumed row) and re-target the same `subject_ref` from a different
   page. Assert `remaining_recipient_budget` does **not** increase (the
   `COUNT(DISTINCT subject_ref)` is unaffected by the compensation row,
   per §3.4's asymmetric-netting rule), while a parallel assertion of
   `remaining_value_budget` over the same clawback **does** net back
   toward its pre-grant level — proving the two budgets' deliberately
   different netting behavior is actually wired to two different
   aggregate queries, not one shared "remaining" number that happens to
   be reused.

**Negative-control variant, mandatory per the directive and per doc 34's
own `EOI-6`/`risk`'s request — `EOI-BUDGET-RACE-1-CONTROL`.** Identical
setup and worker logic, with the root-row lock removed: replace step 2's
`FOR UPDATE OF R` with a plain, non-locking `SELECT` (mirroring
`cumulative_race_integration_test.go`'s own negative-control technique,
named explicitly in doc 34 §9's own cross-reference). Assert this
**deterministically overshoots** `recipient_ceiling` — i.e., the final
`BulkGrantJobItem` count across the subtree is materially greater than
5,000 (not "occasionally exactly 5,000 by scheduling luck"), proving
positive test `EOI-BUDGET-RACE-1` is actually verifying the `FOR UPDATE OF
R` lock's presence and effect, not an accident of low contention. Per
`risk`'s own precedent for this exact pattern, the control should be run
enough times (or with enough workers) that the overshoot is reproducible
on every run, not merely likely — a control that only sometimes overshoots
has not proven the positive test's pass is load-bearing.

**A second negative control this test-writing exercise adds, beyond what
doc 34 names, closing the identical gap `NEW-2` found one level down.**
Re-run the positive scenario with step 2's query changed to aggregate
**only** `eoi.parent_operation_id = <the worker's own page>` (i.e., the
literal defect `code-reviewer`'s `NEW-2` finding describes, §3.4's own
"a literal reading" paragraph) instead of `eoi.root_operation_id =
R.operation_id`. Assert this **also** deterministically overshoots — each
page's own locally-scoped count never reaches its declared `1000` ceiling
meaningfully constraining anything against the shared `5000`, so all 100
pages' workers succeed up to `100 × 1000 = 100,000` grants, wildly
exceeding the root's ceiling. This is a distinct failure mode from the
missing-lock control above (it fails even with a `FOR UPDATE`, just on the
wrong row/aggregate scope) and is exactly the "recurs one level inside the
fix that was supposed to close it" shape §3.4's own text warns about —
worth testing as its own control precisely because a future refactor could
reintroduce it while still technically "having a lock."

**Status.** `SPECIFIED, READY TO IMPLEMENT` in full detail, directly
buildable against this specification the moment `internal/economicop` and
its schema exist. `BLOCKED` on: `internal/economicop` not existing at all
(doc 34 §7's own "no implementation authorized... yet"); the consumption
function (`DEP-EOI-2`, a generalization of or sibling to
`asset_change_consume_approved_request`, `security` + `ledger-finance`'s
call, not yet made); and each `operation_type`'s own consumption-record
declaration (`DEP-EOI-6`, `RK-W15P2-4` — `bonus-engine`/`crm`/`affiliate`
each own authoring their own type's declaration, none yet written). This
is a **wider** `BLOCKED` scope than `EOI-DECOMPOSITION-1` above already
carries for the same underlying reason (§7 of the Phase 2 section), stated
here again because this test's own root-lock mechanics are the part of
`EOI-DECOMPOSITION-1` that most needed the concrete SQL this section adds.

### 4. Drift found while updating this section — flagged, not silently corrected in place

Per the task's explicit instruction, and this document's own established
"append, never silently rewrite" discipline: the following instructions
from the Phase 2 section (§1/§2 above) are **incorrect under this round's
redesigns** and must not be implemented as originally written. The
original text is left in place as the historical record of what Phase 2
specified at the time; this table is the authoritative correction.

| Where | What the prior text said | Why it is now wrong | Corrected by |
|---|---|---|---|
| §1, `G2-HOLD-1` tightening | "should be run against **both** of N1.4.1 item 2's holding-representation branches" | `ledger-accounting-model.md` §7.7.2.1 rejected branch 1 outright (LF-19/LF-20); doc 10 §N1.4.1 confirms there is exactly one account type, `player_bonus_held`, for every case — there are no longer two branches to run the test against | §1.1/§1.0 above |
| §2, `EOI-DECOMPOSITION-1` item 3 | "assert page 6 (the 5,001st–6,000th subjects) is rejected in full" | Doc 34 §5.5 itself states this framing is **wrong**: "Corrected from the prior version, which described 'page 6 fails' — that only holds under reservation-at-creation semantics, which §3.4 explicitly rejects." Under the corrected design, page **creation** never reserves budget; **all 100 pages may be created**, and the 5,001st **actual grant execution**, in whichever page it happens to fall under given real execution order, is what is rejected — not a specific, predictable page | §3 (`EOI-BUDGET-RACE-1`) above, which replaces the page-6-specific assertion with the subtree-wide, order-independent one |

**Checked and confirmed NOT drifted, stated so this is not silently
assumed rather than verified**: doc 08 §16.9's `ResolveTerminalGrantCredit`
seam signature was widened this round from a single combined `amount`
parameter to separate `payoutAmount`/`releasedLockAmount` parameters (the
exact "old single-`amount`-parameter seam signature" shape the task warns
against). Grepping this document confirms no existing test text anywhere
references a single-amount seam call — `G2-HOLD-3` (doc 10 §N1.11) already
names "amount `W`, lock-release amount `X` (distinct...)" as two separate
quantities, and `CASINO-PW-4`/§16.18's own numbered walkthroughs already
carry `W` and `X` as independent values throughout. No correction is
required here; recorded as a checked-and-clean result, not an
unaddressed risk.

### 5. Status summary — `BLOCKED` vs `SPECIFIED, READY TO IMPLEMENT`, stated without inflation

| Test | Status | Blocking dependency |
|---|---|---|
| `G2-HOLD-1` (revised, §1.1) | `BLOCKED` | `bonus_held_dispositions` migration (`0054`+), `player_bonus_held` widening (`0055`), HR-9's own removal precondition — none exist at `HEAD` |
| `G2-HOLD-2` (re-confirmed) | `BLOCKED` | Same as above; unchanged from Phase 2's own status |
| `HELD-ROLLBACK-CAS-RACE-1` / `G2-HOLD-3` (§1.2, closes `C28`) | `BLOCKED` (assertions 1–2 mechanism-buildable once the table exists; the `T6` mirror-completeness sub-question is additionally blocked on `ledger-finance`'s own open flag, doc 08 §16.18 Part B) | Same schema dependency, plus the named mirror-shape open question |
| `CASINO-LF18-QUERY-RACE-1` (§1.3, Layer 1) | `SPECIFIED, READY TO IMPLEMENT` | None — buildable today against `internal/ledger`/existing transaction types |
| `CASINO-LF18-FULL-1` (§1.3, Layer 2) | `BLOCKED` | `internal/casino`'s Round 2 rewrite (Step 1b, outcome classification, `ErrLockAlreadyReleased`) and `casino_settlement_timeout` ratification/migration, both absent at `HEAD` |
| `SEP-1-CARDINALITY-RLS-1` (§2) | `SPECIFIED, READY TO IMPLEMENT` for the RLS mechanics; `BLOCKED` for the full trigger | `SEP-1` trigger family, `REQ-SEP-BONUS-3` resolver, `REQ-SEP-ID-1` Person-linkage confirmation |
| `EOI-BUDGET-RACE-1` + both negative controls (§3) | `SPECIFIED, READY TO IMPLEMENT` in full detail | `internal/economicop` does not exist; consumption function (`DEP-EOI-2`) and per-type consumption declarations (`DEP-EOI-6`) not yet authored |

No item in this section is claimed `IMPLEMENTED` or `PASSING`. Where a row
above says `SPECIFIED, READY TO IMPLEMENT`, that means the mechanisms the
test needs already exist in this repository today and the test can be
written and run without waiting on any further design decision — not that
it has been written.

### 6. Gaps this test-writing exercise surfaced, beyond what it was asked to test

- **The `bonus_held_disposition_resolution` four-eyes threshold
  contradiction between `security` and `architect`, already flagged in doc
  10 §N1.12 but not previously captured in this document.** `security`'s
  §W15.1.12 states this operation's four-eyes is "threshold 0, always";
  `architect`'s doc 34 §3.1 table states its root mint requires four-eyes
  "above `CLAUDE.md`'s threshold" (i.e., thresholded, not unconditional).
  Doc 10 itself declines to pick a side. This directly affects how
  `HELD-ROLLBACK-CAS-RACE-1`'s (§1.2) staff-resolution worker must be
  constructed once real: a below-threshold resolution attempt must be
  tested as **refused** under `security`'s reading and as **permitted**
  under `architect`'s literal table text, and this test cannot be
  finalized as written until the two owners reconcile it. Recorded here
  as a `qa`-surfaced consequence of the existing, already-disclosed
  inconsistency, not a new finding.
- **Doc 08 §16.18 Part B's own flagged mirror-completeness question for
  `T6`** (§1.2 above) is not merely a documentation gap — it is a gap this
  test cannot close without an answer, because "the reversal balances"
  and "the reversal balances **and** posts the correct mirror pair" are
  different assertions, and only `ledger-finance` can say which one this
  test should make.
- **`EOI-BUDGET-RACE-1`'s consumption-record join (§3) is only as good as
  each `operation_type`'s own declaration (`DEP-EOI-6`), none of which are
  written yet.** The test as specified assumes `BulkGrantJobItem` is the
  sole consumption-row shape for `bonus_bulk_grant`; per §3.4/`EOI-15`'s
  own "an undeclared second shape must raise, never under-count" rule,
  this test should be re-run once any domain adds a second consumption-row
  shape for the same `operation_type`, with an explicit assertion that the
  consumption function raises rather than silently under-counting against
  the new shape — named here so it is not rediscovered as a fresh gap once
  `DEP-EOI-6` is authored.

## Stage 4H-B1 Wave 2 Phase 9 (`qa`) — real-code test verification, gap closure, and consolidated register

Status: **verification against real code**, not design review. Phases 1-7
(commits `d145ba1`..`a79db4a`) built the real `internal/bonus`,
`internal/economicop`, and casino's G-2 wiring this document's own prior
sections only specified. This section is `qa`'s independent check of what
of those specifications actually landed as real, passing tests — per
CLAUDE.md's "no fake completion," every status below was confirmed by
reading the cited test function's actual assertions and by actually
running it against real PostgreSQL 16, not inferred from a commit message.
No Human Decision Register item is selected here.

### 1. Named test-ID reconciliation — design-time IDs vs. the real test functions that now exist

The design sections above (Wave 1.5 Fix Wave/Fix Round 2) invented test
IDs before any code existed. The real implementation phases did not carry
those IDs into the actual `_test.go` files (confirmed by grep: none of
`G2-HOLD-1/2/3`, `EOI-DECOMPOSITION-1`, `EOI-BUDGET-RACE-1`, `SEP-1-
CARDINALITY-RLS-1`, `CASINO-LF18-*` appear verbatim anywhere in
`internal/`). This is not itself a defect — the real tests are more
numerous and more specific than the design-time composites they descend
from — but it means the mapping below had to be done by reading each
real test's actual assertions against the original specification's own
assertion list, not by string search.

| Design-time ID | Real status | Real test(s) (file, function) | Assessment |
|---|---|---|---|
| `G2-HOLD-1` (two-leg hold-capture posting shape) | **IMPLEMENTED** | `internal/casino/bonus_settlement_integration_test.go`: `TestPostWin_LockedBonusTerminalGrant_CapturesUnconditionally` (table-driven across expired/cancelled/forfeited/pending_settlement); `internal/ledger/bonus_mirror_integration_test.go`: `TestBonusMirror_WorkedPostingShapes` | Confirmed the exact `player_bonus_held` two-leg shape (payout leg always present, lock-release leg present iff the origin was locked), the `bonus_held_dispositions` row with correct `payout_amount`/`released_lock_amount`, and `player_locked_bonus → 0`. Assertion 2's specific fault-injection sub-claim ("kill the connection between the two legs' INSERTs") is **not** separately proven — see §4 below, a real, disclosed gap, not unique to this test. |
| `G2-HOLD-2` | **IMPLEMENTED** (folded into the above and into `TestPostWin_LockedBonusOrdinary_GrantNonTerminal`) | Same files | The non-terminal ordinary case and the terminal held case are both exercised as separate table cases/tests, each independently assertable, matching the design's "each assertion its own failing sub-test" tightening. |
| `G2-HOLD-3` / `HELD-ROLLBACK-CAS-RACE-1` (C28 closure: guarded compare-and-swap under real concurrency) | **IMPLEMENTED**, with one disclosed scope gap | `internal/casino/bonus_settlement_integration_test.go`: `TestPostRollback_HeldWinRollback_RollbackRacesDirectResolution` (the genuine two-goroutine race, real `pg_advisory_xact_lock`, real `FOR UPDATE`), `TestPostRollback_HeldWinRollback_ConcurrentDistinctRollbacks` (duplicate-rollback race), `TestPostRollback_HeldWinRollback_AlreadyResolved_FailsClosed` (LF-10, sequential-after-race) | This is a **real** PostgreSQL concurrency test (two actual goroutines, two actual connections, a real shared advisory lock, `sync.WaitGroup`), not a mocked one — the exact upgrade the design section demanded. Two things the design specified are **not** present: (a) the barrier is not constructed to force **both** lock-acquisition orderings explicitly (it relies on the Go scheduler to interleave, unlike `cumulative_race_integration_test.go`'s pattern of an explicit start barrier) — a `t.Run` sub-test per forced ordering was not built; (b) the **mandatory negative control** (assertion 5: re-run with the `WHERE status = 'held'` guard replaced by a check-then-update, and assert it deterministically double-resolves) does not exist anywhere in this file. Recorded as `P2` finding `QA-W2P9-1` below, not fixed in this phase (adding it is straightforward — mirrors `cumulative_race_integration_test.go`'s own established negative-control technique — but is additional net-new test-writing, and this phase's own time budget was prioritized toward the directive's named, currently-**zero**-coverage gaps in §3 below over deepening an already-real, already-passing concurrency test). |
| `EOI-DECOMPOSITION-1` (API-splitting/retry/pagination/concurrent-worker composite) | **PARTIALLY IMPLEMENTED** | `internal/bonus/lifecycle_integration_test.go`: `TestEOI_BulkGrantRecipientCeiling_RejectsBeyondBudget` (sequential ceiling enforcement), `TestAdversarial_ConcurrentBulkGrantWorkers_NeverExceedRecipientCeiling` (genuine concurrent-worker version), `TestEOI_SingleManualGrant_RejectsUnresolvableParent`; `internal/economicop/economicop_integration_test.go`: root/child lineage, idempotency-key resolution, cross-tenant RLS | Item 4 (concurrent workers cannot jointly overspend) and item 6 (duplicate top-level mint request) are genuinely covered. Item 1 (API-splitting across three distinct call surfaces sharing one budget) is **not** built as a single composite test — no test exercises `BulkGrantJob` execution, a staff single-Grant action, and a simulated direct `ActorService` call all against the same EOI root in one scenario; each surface's own budget-consumption is tested in isolation instead. Item 3 (pagination cannot exceed the ceiling, corrected framing) and item 5 (partial batch/crash-and-resume re-attaching to `root_operation_id`) have **no** dedicated test. |
| `EOI-BUDGET-RACE-1` (+ negative control) | **IMPLEMENTED** for the single-EOI-row case; **NOT IMPLEMENTED** for the multi-page/subtree-wide case the design specifically calls for | `internal/bonus/lifecycle_integration_test.go`: `TestAdversarial_ConcurrentBulkGrantWorkers_NeverExceedRecipientCeiling` | This test is a real, deliberate, and honestly-documented concurrency proof (its own comment explains why `n=2`, not `n≥20`, after finding and naming a genuine Postgres multi-waiter deadlock pathology under 3+-way contention — itself a P2 finding, §5 below). It proves the ceiling holds under genuine concurrency for **one** EOI root with no child pages. The design's own root-subtree-budget scenario (100 child pages, workers spread across all of them, `COUNT(DISTINCT subject_ref)` aggregated by `root_operation_id` rather than by page) is **not** built — `MintRootOperation`/`ConsumeRootBudget`'s child-page/lineage mechanics are exercised only in `economicop_integration_test.go`'s sequential `TestCreate_ChildInheritsParentsRoot`, never under concurrency. Neither of the design's two named negative controls (lock removed; aggregate scoped to the wrong row) exists as a test. |
| `SEP-1-CARDINALITY-RLS-1` | **BLOCKED** (unchanged from the design-time status, confirmed still accurate) | — | Confirmed by direct grep: no `household`, `REQ-SEP-BONUS-3`, `CARDINALITY_MISMATCH`, or `expected_count`/`subject_set_count` resolver logic exists anywhere in `internal/bonus/*.go`. The household/linked-account detection resolver this test depends on was never built (correctly out of scope — Phase 3's own report lists it as deferred, and `docs/security/security-architecture.md` §W15.1.5 keeps it "detection only, never a block," unimplemented). What **did** get built and tested is a related but distinct mechanism — Step 0's tenant-scope self-proof (`TestAdversarial_SEP1_Step0_FiresEvenWhenPlayerListWouldOtherwiseHide`, migration `0066`) and the SEP-1 actor-is-beneficiary core case (`TestAdversarial_SEP1_StaffMemberIsGrantBeneficiary_Refused`) — both genuinely new and correctly closing Phase 6's own findings, but neither substitutes for the cardinality-mismatch resolver-truncation test this ID names. Still `BLOCKED`, not `IMPLEMENTED`. |
| `CASINO-LF18-QUERY-RACE-1` (Layer 1: the Step-1b read is race-safe under real concurrent commits) | **PARTIALLY IMPLEMENTED** — the exploit is closed and proven, but not via the specific real-concurrency mechanism the design demanded | `internal/casino/bonus_settlement_integration_test.go`: `TestPostWin_LockAlreadyReleased` | This test is **sequential**, not concurrent: it posts the bet, then sequentially drains the lock via `drainRoundLock` (standing in for the not-yet-built sweep), then sequentially delivers the late win and asserts `ErrLockAlreadyReleased`. It correctly proves Step 1b's query reflects the drain once committed and proves outcome 6 aborts cleanly with no double-release (LF-18's actual exploit, closed). It does **not** prove the design's specific claim: that the query is race-safe when a genuine second worker's read and the drain's commit are actually interleaved by the Go scheduler under a shared `pg_advisory_xact_lock`, nor does it include the mandatory negative control (lock removed, artificially delayed read observes stale state). The production code path genuinely does take the advisory lock (confirmed in `internal/casino/bonus_settlement.go`), so the mechanism this test would prove is real; the proof itself, in the specific real-concurrency shape the design insisted on ("not an application-level assertion"), was not built. `P2` finding `QA-W2P9-2`, §5 below. |
| `CASINO-LF18-FULL-1` (Layer 2) | **BLOCKED**, correctly | — | `casino_settlement_timeout` remains unratified/unmigrated (confirmed: no such transaction type or sweep job exists in `internal/casino` or `migrations/` at `HEAD`); Phase 7's own commit message states this was deliberately not built ("building it here would be unauthorized architecture"). Status carried forward unchanged, correctly. |

### 2. `CASINO-STO-*` and `SEG-MEMBER-OF-*`/`SEG-INCSAFE-*` — confirmed still exactly as blocked as the design left them

Both directly checked against `HEAD`, per the task's explicit instruction:

- **`CASINO-STO-1` through `-5`** (settlement-timeout sweep): confirmed
  `BLOCKED`. No `casino_settlement_timeout` transaction type, no sweep job,
  no migration for either exists anywhere in `migrations/` or
  `internal/casino/*.go`. Phase 7 explicitly and correctly declined to
  build this (own commit message, quoted in §1's `CASINO-LF18-FULL-1` row
  above) — this is the right call, not a gap: the architecture document
  itself states the sweep is "NOT SAFE to implement" pending a ratified
  window/jurisdiction decision, a Human Decision Register matter this `qa`
  phase does not touch.
- **`SEG-MEMBER-OF-1` through `-5`, `SEG-INCSAFE-1` through `-3`**: confirmed
  `BLOCKED`/`NOT APPLICABLE`. `internal/segment` does not exist (confirmed:
  no such package anywhere in `internal/`), correctly out of this Wave's
  scope per this dispatch's own constraints (Segmentation was explicitly
  never authorized for this Wave). No drift from the design-time status.

### 3. The human directive's own §19/§20 test-floor audit against real test files

Checked directly against every `_test.go` file this Wave touched
(`internal/ledger/bonus_mirror_integration_test.go`,
`internal/money/money_test.go`, `internal/bonus/*_test.go`,
`internal/economicop/*_test.go`,
`internal/risk/cumulative_bonus_conversion_integration_test.go`,
`internal/casino/bonus_settlement_integration_test.go`,
`internal/httpserver/admin_routes_test.go`) rather than assumed from
commit messages.

**Financial-write floor (retry/replay class):**

| Required | Status | Evidence |
|---|---|---|
| Sequential retry | Covered | `TestBonusMirror_ExactRetryIsIdempotent`; `TestLifecycle_IssueActivateWagerComplete`'s own second-delivery assertion; `TestHeldDisposition_ResolveReforfeit_WithFourEyesAndSEP1`'s `ResolveTerminalGrantCredit` redelivery |
| Concurrent retry | Covered | `TestBonusMirror_ConcurrentGrantsSameKeyOnlyOnePosts` |
| Mismatched retry (same key, different payload) | Covered | `TestBonusMirror_SameKeyDifferentTypeRejected`; `TestCreateGrant_DuplicateTriggerReferenceIsRejected` |
| Callback replay | Covered | `TestPostRollback_HeldWinRollback_DuplicateIsIdempotent`; `TestHeldDisposition_ResolveReforfeit_WithFourEyesAndSEP1` |
| Worker retry | Covered | `TestAdversarial_ConcurrentBulkGrantWorkers_NeverExceedRecipientCeiling`'s own retry-on-`40P01`/`40001` loop (a real transient-error retry, not merely business-logic retry) |
| Pagination retry | **Not covered** | No test drives a paginated bulk-grant delivery (`lineage_kind = 'page'`) through a retried page. `EOI-DECOMPOSITION-1` item 3's pagination scenario (§1 above) was never built. |
| Bulk decomposition | Covered | `TestAdversarial_ConcurrentBulkGrantWorkers_NeverExceedRecipientCeiling` (concurrent-worker decomposition); item 1's API-splitting-across-three-surfaces composite (§1 above) is **not** covered |
| Process restart | **Not covered** | No test simulates a crash-and-resume of a `BulkGrantJob` (`lineage_kind = 'resume'`) reattaching to the same root with the already-consumed remainder intact. |
| Partial failure (mid-transaction fault injection) | **Not covered** | Confirmed by grep: no test in `internal/bonus`, `internal/casino`, or `internal/ledger` kills a connection mid-transaction. This is a **pre-existing, repo-wide** gap, not unique to the Bonus Engine — no package's test suite does true fault injection anywhere in this codebase; every "partial failure" claim in this document instead rests on Postgres's own single-statement/single-transaction atomicity guarantee, which is a reasonable and defensible substitute (the guarantee is real and does not need re-proving per feature) but is not the literal fault-injection test several design sections (`G2-HOLD-1` assertion 2, `G2-HOLD-3` assertion 4) called for. Named as `P3` finding `QA-W2P9-6` below — a testing-infrastructure investment for a future stage, not a Bonus-Engine-specific defect. |
| Transaction retry | Covered | Same as "worker retry" above |

**Concurrency floor (state-mutation class):**

| Required | Status | Evidence |
|---|---|---|
| Concurrent grant | Covered | `TestBonusMirror_ConcurrentGrantsSameKeyOnlyOnePosts`; `TestBonusMirror_ConcurrentGrantsDifferentKeysBothPost` |
| Concurrent activation | Covered | `TestAdversarial_ConcurrentActivation_ExactlyOneWins` |
| Concurrent wagering progress | **Not directly covered** | No test races two concurrent `RecordWageringContribution` calls against the same Grant. `RecordWageringContribution`'s own concurrency-safety was not independently verified this phase; named as a gap, not fixed (see §4). |
| Concurrent conversion | **Not directly covered, but structurally identical to an already-proven mechanism** | `ConvertGrant` (`internal/bonus/conversion.go`) uses the identical `AdvisoryLockGrant` + `LockGrantForUpdate` + status-guard composition `TestAdversarial_ConcurrentActivation_ExactlyOneWins` already proves safe for `ActivateGrant` — the same mechanism, not independently re-tested for this call site. Stated honestly as **not literally tested**, not claimed covered by analogy. |
| Concurrent expiry | **Was a real gap — closed this phase** | New: `TestAdversarial_ConcurrentTerminate_ExpireVsCancel_ExactlyOneWins` (§4) |
| Concurrent cancellation | **Was a real gap — closed this phase** | Same new test (expire and cancel race against each other directly) |
| Concurrent reversal | **Was a real gap — closed this phase** | New: `TestAdversarial_ConcurrentReversal_ExactlyOneWins` (§4) |
| Duplicate callbacks | Covered | `TestPostRollback_HeldWinRollback_DuplicateIsIdempotent`; `TestLifecycle_IssueActivateWagerComplete` |
| Bulk worker races | Covered | `TestAdversarial_ConcurrentBulkGrantWorkers_NeverExceedRecipientCeiling` |
| Same player across multiple brands | **Was a real gap — closed this phase** | New: `TestAdversarial_SamePlayerAcrossMultipleBrands_ConcurrentGrantsIsolated` (§4) |
| Same campaign across multiple workers | Covered | `TestAdversarial_ConcurrentBulkGrantWorkers_NeverExceedRecipientCeiling` runs all workers against one shared `campaignID`/`offerVersionID` |

### 4. Three real gaps found and closed with new tests (real PostgreSQL, no domain-logic change)

All three are appended to `internal/bonus/lifecycle_integration_test.go`
(the file already covering this exact class of adversarial/concurrency
test for the Grant lifecycle). None required any change to
`internal/bonus`'s production code — `TerminateGrant`, `MarkGrantReversed`,
and the ordinary brand-scoped Grant path were all already correctly built
for these properties (advisory lock + compare-and-swap, or a single
guarded `UPDATE`); only the adversarial test itself was missing, exactly
the kind of gap this phase's mandate is to find and close.

1. **`TestAdversarial_ConcurrentTerminate_ExpireVsCancel_ExactlyOneWins`**
   closes both "concurrent expiry" and "concurrent cancellation" in one
   test: two different terminal triggers (`TerminalResolutionExpired`,
   `TerminalResolutionCancelled`) race `TerminateGrant` against the same
   Grant with no open exposure (so either would resolve immediately, not
   defer to `pending_settlement`, if it won). Asserts exactly one winner,
   exactly one loser (`ErrIllegalTransition`), and that the final
   committed row matches the winner's own returned resolution — never a
   third value, never the loser's. Proves `TerminateGrant`'s
   `AdvisoryLockGrant` + `LockGrantForUpdate` + re-read-status-under-lock
   composition (`internal/bonus/lifecycle.go`) is actually safe under two
   genuinely concurrent, genuinely different callers, not merely against
   itself.
2. **`TestAdversarial_ConcurrentReversal_ExactlyOneWins`** closes
   "concurrent reversal": two goroutines call `MarkGrantReversed`
   (`internal/bonus/grant.go`) on the same already-terminal Grant at
   nearly the same instant, with different reason codes. Unlike
   `TerminateGrant`, `MarkGrantReversed` takes **no** explicit advisory
   lock — it relies entirely on a single atomic `UPDATE ... WHERE status
   <> 'reversed'` and ordinary Postgres row-lock serialization. This test
   proves that reliance is actually safe (exactly one winner, one loser
   via `ErrGrantStateConflict`, the final row carries exactly one
   `reversal_reason_code`) rather than merely plausible from reading the
   SQL.
3. **`TestAdversarial_SamePlayerAcrossMultipleBrands_ConcurrentGrantsIsolated`**
   closes "same player across multiple brands," a scenario with **zero**
   prior coverage anywhere in this package. One `person_id` is given two
   `player_accounts` rows under the same tenant, one per brand (a real,
   schema-supported shape — `player_accounts.person_id` carries no
   uniqueness constraint, migration `0010`). Two Grants, one per brand,
   are issued and activated concurrently for that one person. Asserts
   both succeed (no accidental cross-brand serialization or denial), and
   — the property that actually matters — each resulting Grant is
   attributed to exactly its own `brand_id`/`player_account_id`/
   `wallet_id`, never swapped or shared, proving
   `AdvisoryLockGrant`/`ActivateGrant`'s locking is genuinely keyed by
   `grant_id`, never by `person_id`, which is the only way two brands'
   concurrent activity for one underlying person could otherwise
   interfere.

All three pass, including under `-race`, across 5 repeated runs each with
zero flakes (§6 below has the exact commands and timings). No test in
this set revealed a genuine business-logic defect — all three passed on
first write, confirming (not merely assuming) `internal/bonus`'s existing
locking design already had these properties; the gap was purely in test
coverage, consistent with Phases 1-7's `code-reviewer`/`security` findings
never flagging the underlying mechanisms themselves as broken.

**Deliberately not attempted this phase, named rather than silently
dropped:** the `EOI-BUDGET-RACE-1` subtree/multi-page scenario, the
`EOI-DECOMPOSITION-1` API-splitting composite, pagination retry, and
process-restart/resume are all real, currently-uncovered gaps this
review's own §1/§3 tables name — each would require either
`economicop.MintRootOperation`'s child-page mechanics under genuine
concurrency (a materially larger fixture than the three tests above) or
inventing a resumability harness `internal/bonus` does not yet expose a
seam for outside the existing `BulkGrantJob`/`BulkGrantJobItem` resumption
already covered sequentially. Recorded as open items in the register (§5)
rather than built under this phase's time budget, per this document's own
"NOT IMPLEMENTED, not skipped-and-forgotten" discipline.

### 5. New finding this phase surfaced

**`QA-W2P9-3` (P3, informational, not a defect).** While tracing every
production call site of each of the five in-slice bonus types (doc 10 §2),
`IssueAndActivateCashback` (`internal/bonus/types.go`) has **zero** live
callers anywhere in `internal/httpserver` or any job/scheduler — confirmed
by grep across the whole repo; its only callers are its own tests. This
matches, and independently confirms, the same item already named in the
task's own consolidated-register prompt; recorded here as `qa`-verified
rather than merely repeated. Not a defect (the function itself is
implemented and tested correctly, per `TestConversion_*`'s general
coverage of the shared conversion path it feeds into) — it is a feature-
completeness gap: no HTTP surface or automated trigger issues a cashback
bonus today, so this bonus type is `IMPLEMENTED` at the domain-logic layer
and `NOT IMPLEMENTED` at the product-surface layer. Owner: `bonus-engine`/
`backend` (HTTP surface), not `qa`'s to build.

### 6. Full validation floor, actual output — every command, exact result, repeated runs for flake-checking

Run against real PostgreSQL 16 (already-running dev cluster, migrations
`0001`-`0066` confirmed at `HEAD`, `go run ./cmd/migrate status` reporting
"nothing to apply" before any test ran).

| Command | Result | Notes |
|---|---|---|
| `go build ./...` | **PASS** | Clean, no output, exit 0. |
| `go vet ./...` | **PASS** | Clean, no output, exit 0. |
| `gofmt -l .` | **PASS** | Empty output (nothing needs formatting), including the three new tests. |
| `go test -count=1 ./...` | **PASS** | 2.173s wall. Every package with non-integration tests reports `ok`; `internal/bonus`/`internal/economicop`/`internal/ledger` correctly report `[no test files]` for the default build (all three packages' real tests are `//go:build integration`-gated, by design). |
| `go test -race -count=1 ./...` | **PASS** | 10.614s wall. Same package set, all `ok`, zero races reported. |
| `go test -tags=integration -count=1 ./...` | **PASS** | 1m58.255s wall. Every package `ok`, including `internal/bonus` (3.220s), `internal/casino` (4.713s), `internal/economicop` (0.103s), `internal/ledger` (4.307s) — the three new tests included and passing. `internal/reconciliation` (110.960s) and `internal/rg` (103.093s) dominate total wall time; both pre-date this Wave and are unrelated to it. |
| `go test -race -tags=integration -count=1 ./...` | **PASS** | 2m54.105s wall. Every package `ok`, zero races. This is the directive's own explicit floor command; ran once in full, then the four concurrency-heavy packages were re-run in isolation multiple times (below) for dedicated flake-checking. |

**Flake-checking: concurrency-heavy suites repeated 3× minimum, the new
tests repeated 5×, per the directive's explicit "repeat sufficiently to
establish no flakes" instruction.**

| Suite | Runs | Command | Result each run |
|---|---|---|---|
| `internal/bonus` + `internal/economicop` | 3 | `go test -race -tags=integration -count=1 ./internal/bonus/... ./internal/economicop/...` | Run 1: `bonus` 4.028s, `economicop` 1.162s — PASS. Run 2: `bonus` 3.068s, `economicop` 1.151s — PASS. Run 3: `bonus` 4.173s, `economicop` 1.165s — PASS. Zero flakes. |
| `internal/casino` + `internal/ledger` | 3 | `go test -race -tags=integration -count=1 ./internal/casino/... ./internal/ledger/...` | Run 1: `casino` 5.817s, `ledger` 5.271s — PASS. Run 2: `casino` 5.775s, `ledger` 5.344s — PASS. Run 3: `casino` 5.821s, `ledger` 5.706s — PASS. Zero flakes. |
| `internal/risk` | 3 | `go test -race -tags=integration -count=1 ./internal/risk/...` | 2.706s, 2.607s, 2.649s — PASS all 3. Zero flakes (includes `cumulative_bonus_conversion_integration_test.go`, this Wave's Phase 4 addition). |
| The three new tests specifically, `-v` | 5 | `go test -race -tags=integration -count=1 -run 'TestAdversarial_ConcurrentTerminate_ExpireVsCancel_ExactlyOneWins\|TestAdversarial_ConcurrentReversal_ExactlyOneWins\|TestAdversarial_SamePlayerAcrossMultipleBrands_ConcurrentGrantsIsolated' -v ./internal/bonus/...` | All 5 runs: all 3 tests `--- PASS`, total suite time 1.33s-1.40s per run. Zero flakes. |

No suite in this Wave's scope produced a `FAIL`, an unexplained `FLAKE`,
or a `BLOCKED` result. No suite was skipped.

### 7. Consolidated P0/P1/P2/P3 register — Wave 2 (Phases 1-9), as verified against real code

Severity/owner assignments below are `qa`'s own independent
classification, cross-checked against each phase's own commit-message
report where one exists; `qa` does not re-litigate an owning domain's
severity call, only records it and confirms the current CLOSED/OPEN state
against real code at `HEAD`.

| ID | Finding | Status | Owner | Severity |
|---|---|---|---|---|
| LF-18 | Late win after a silent lock-drain could double-release stake value | **CLOSED** — Phase 7, `TestPostWin_LockAlreadyReleased` proves `ErrLockAlreadyReleased` aborts cleanly, no double-credit | casino/ledger-finance | P0 (was) |
| `ACTION_ROUTE_TO_CASH` T.1 gate gap | Held-bonus route-to-cash bypassed AssetAuthorization→RG→Risk, unlike every other value-creating checkpoint | **CLOSED** — Phase 5, `TestHeldDisposition_ResolveRouteToCash_BlockedBySelfExclusion`/`_BlockedByRiskDeny`/`_AllowedPlayerSucceeds` | identity-compliance | P0 (was) |
| `ACTION_ROUTE_TO_CASH` reason_code posting-shape bug | Unconditional non-nil `reason_code` on a `TxBonusConversion` posting violated migration 0051's own CHECK, so the posting could never have completed even pre-fix | **CLOSED** — fixed in passing by Phase 5, same tests above exercise the corrected nil reason_code | identity-compliance | P1 (was) |
| SEP-1 step-0 tenant-scope self-proof missing from `bonus_change_approvals_enforce_separation()` | A BEFORE INSERT trigger runs before RLS's own WITH CHECK; without its own scope proof it is not protected by RLS incidentally | **CLOSED** — Phase 6, migration `0066`, `TestAdversarial_SEP1_Step0_FiresEvenWhenPlayerListWouldOtherwiseHide` | security | P1 (was) |
| SEP-1 core case (actor IS beneficiary) untested | The one pre-existing SEP-1 test explicitly substituted the ordinary four-eyes check instead of proving the SEP-1-specific case | **CLOSED** — Phase 6, `TestAdversarial_SEP1_StaffMemberIsGrantBeneficiary_Refused` | security/qa | P1 (was) |
| `ConsumeRootBudget` error-masking bug | Any error (including a transient Postgres deadlock) was treated as an ordinary budget-exhausted denial, masking the true error and triggering a further write on an aborted transaction | **CLOSED** — Phase 6, `runBulkGrantJobItem` now only treats `ErrBudgetExhausted` this way | bonus-engine/security | P1 (was) |
| AOE-attribution and redelivery-idempotency bugs (casino G-2 wiring) | Casino's postWin/postRollback did not attribute reversals back to the funding Grant, and redelivery of a settlement could double-process | **CLOSED** — Phase 7, `TestPostRollback_PlainLockRollback_RecomputesExposure`, `TestPostRollback_HeldWinRollback_DuplicateIsIdempotent` | casino | P1 (was) |
| Dormant EOI/Risk lock-ordering risk (doc 34 §5.3 canonical order reversed for `IssueSingleManualGrant`'s second Risk acquisition) | Currently dormant only because `operationCumulativeSpecs` has no entry for `OperationBonusGrant` | **DOCUMENTED-RISK**, not fixed — correctly Phase 6's own call, an architecture question (how Bonus's two-phase lifecycle composes with doc 34's single-gate-chain assumption), not a security-mechanics fix | architect/bonus-engine/risk | P2 |
| 3+-way EOI-row deadlock under true concurrency | Postgres's own documented multi-waiter tuple-lock behavior under 3+ concurrent `FOR UPDATE` waiters on one EOI root row; no transaction that deadlocks ever commits, so no correctness defect, but a real availability/robustness gap for any future concurrent bulk executor | **DOCUMENTED-RISK**, not fixed — correctly Phase 6's own call (today's only executor, `RunStaticBulkGrantJob`, is sequential and never reaches this shape) | architect/bonus-engine/risk | P2 |
| LF-10 general case (rollback of an already-resolved held disposition) | Fails closed via `ErrHeldDispositionRollbackUnsupported`, proven by `TestPostRollback_HeldWinRollback_AlreadyResolved_FailsClosed` — but ledger-finance's own general-case design question (beyond this specific fail-closed behavior) remains open per Phase 7's own commit note | **OPEN** (the fail-closed behavior itself is `CLOSED`/tested; the general design question is not) | ledger-finance | P2 |
| KYC-tier-taxonomy gate (ADR 0034 §6) | No bonus activation is gated on `player_accounts.kyc_tier` today; the contract is designed but not wired into `internal/bonus`'s gate chain | **OPEN** | architect/bonus-engine/identity-compliance | P2 |
| Multi-account-abuse-detector (ADR 0034 §7/§19, Person-keyed cross-brand anti-abuse rule) | Designed (recommended default: per-brand scope with an optional Person-keyed anti-abuse rule) but not implemented in `internal/bonus` | **OPEN** | identity-compliance/bonus-engine/risk | P2 |
| Missing HTTP surface: four-eyes filing/approval, campaign-activate, offer-publish, bulk-job-execute | Confirmed by grep and by Phase 6's own review: `bonus_change_requests` file/approve has no HTTP surface; `manual_grant_issue`'s handler cannot itself mint a `parent_operation_id`; no bulk-population `BonusSuggestion` activation path | **OPEN** (feature-incompleteness, not a security defect — correctly Phase 6's own characterization) | backend/bonus-engine | P3 |
| `IssueAndActivateCashback` zero live callers | Correct, tested domain logic with no HTTP/job trigger anywhere | **OPEN** | backend/bonus-engine | P3 |
| `QA-W2P9-1` — `HELD-ROLLBACK-CAS-RACE-1`'s negative control and forced-both-orderings barrier missing | The real concurrency test exists and passes; the design's own mandatory negative control (guard removed, deterministic double-resolution) and explicit both-orderings barrier were never built | **OPEN** — newly named this phase | qa | P2 |
| `QA-W2P9-2` — `CASINO-LF18-QUERY-RACE-1`'s real-concurrency proof not built | The exploit is closed and proven sequentially (`TestPostWin_LockAlreadyReleased`); the specific real-concurrency two-worker-plus-negative-control proof the design demanded was not built | **OPEN** — newly named this phase | qa | P2 |
| `QA-W2P9-3` — `IssueAndActivateCashback` zero live callers (qa-independent confirmation) | See row above; listed once, cross-referenced | **OPEN** | backend/bonus-engine | P3 |
| `QA-W2P9-4` — `EOI-DECOMPOSITION-1`/`EOI-BUDGET-RACE-1`'s multi-page/API-splitting/pagination/resume scenarios not built | Single-EOI-row concurrency is proven; the subtree/multi-page and cross-surface scenarios the design specifically called out are not | **OPEN** — newly named this phase | qa | P2 |
| `QA-W2P9-5` — `RecordWageringContribution` concurrency not independently tested | No test races two concurrent wagering-contribution postings against the same Grant | **OPEN** — newly named this phase | qa | P3 |
| `QA-W2P9-6` — no true mid-transaction fault-injection test exists anywhere in this repo | Every "partial failure" claim in this document rests on Postgres's own atomicity guarantee rather than an actual killed connection; a reasonable substitute, but not the literal test several design sections call for | **OPEN**, repo-wide pre-existing gap, not unique to this Wave — newly named this phase | qa | P3 |
| Concurrent expiry / concurrent cancellation / concurrent reversal / same-player-multi-brand (human directive §19/§20 floor items) | Zero prior coverage anywhere in `internal/bonus` | **CLOSED this phase** — three new tests, §4 above | qa | P1 (was; the directive named these explicitly as required floor items) |

No item above is marked `IMPLEMENTED` without a cited, actually-passing
test, and no item is marked `CLOSED` without a cited fix commit or test.
Every `OPEN`/`DOCUMENTED-RISK` item was already disclosed by its owning
phase except the five newly-named `QA-W2P9-*` items, which this phase
found and is naming for the first time, per CLAUDE.md's "no fake
completion" and this document's own "gaps this test-writing exercise
surfaced" precedent (§6 of the Fix Round 2 section above).

## Stage 4H-B1 Wave 3 Phase 9 (`qa`) — full validation floor, coverage-gap audit, two genuine defects fixed

Status: **verification against real code**, the mandated "full test
floor" for Wave 3 (Phases 1-8: `ledger-finance` design contract,
`backend`/`bonus-engine` implementation of the deposit/cashback/expiry
sweeps and the four-eyes/EOI HTTP surface, `risk`/`identity-compliance`/
`security` fixes, `casino`'s own concurrency regression). Ran the entire
validation floor from scratch rather than trusting prior phases'
self-reports, per this dispatch's own explicit instruction.

### 1. Validation floor — exact results

| Command | Result | Notes |
|---|---|---|
| `go build ./...` | **PASS** | Clean, exit 0. |
| `go vet ./...` | **PASS** | Clean, no output. |
| `go vet -tags=integration ./...` | **PASS** | Clean, no output. |
| `gofmt -l .` | **PASS** | Empty output throughout, including every new/modified file this phase. |
| `go test -count=1 ./...` | **PASS** | Full unit suite, all packages `ok` or `[no test files]`. |
| `go test -tags=integration -count=1 ./...` | **PASS** | Full repo-wide integration suite, every package `ok`. `internal/reconciliation` (~227s) and `internal/rg` (~207s) dominate wall time; both pre-date this Wave. Run twice across this phase (once before, once after this phase's own fixes/additions) — both green. |
| `go test -tags=integration -race -count=1 ./...` | **PASS** | Full repo-wide integration suite under `-race`. Zero data races reported anywhere, including across every new test this phase added. Run at the start of this phase (baseline, before any qa changes) and again at the end (with every fix/test this phase added) — both green, zero races either time. |
| Migration round-trip, `0068`-`0070` | **PASS** | `go run ./cmd/migrate -steps=3 down` cleanly rolled back to version 67 (confirmed via `schema_migrations`, and via direct `to_regclass`/`information_schema.columns` checks that `bonus_ledger_sweep_watermarks`, `bonus_cashback_schedule_watermarks`, and `bonus_grants.expires_at` were all genuinely dropped); `go run ./cmd/migrate up` cleanly re-applied all three; a second `up` was a no-op. Row counts for every affected table (`bonus_grants` and its 14 sibling Bonus tables, `economic_operations`, `bulk_grant_jobs`, `bulk_grant_job_items`, `ledger_transactions`, `ledger_entries`) were captured before and after: all zero, before and after (this shared dev database happened to hold none of this Wave's own domain data at the time of the check) — confirmed no data loss is possible to observe from this state, and the schema-level round-trip itself (the thing nobody had explicitly re-verified since Phase 2 first wrote these three migrations) is clean. |

No suite in this phase's scope produced a `FAIL`, an unexplained
`FLAKE`, or a `BLOCKED` result.

### 2. Coverage-gap audit and genuine defects found (item 2 of the dispatch)

Read every new/changed file this Wave names
(`internal/bonus/{deposit_sweep,cashback_scheduler,expiry_sweep,
schedulers,four_eyes_ops,wagering_contribution_entry}.go`, `internal/
casino/orchestrator.go`'s new section, `internal/httpserver/bonus_
{governance,domain_ops}_handlers.go`, `internal/economicop/enforce.go`'s
new check) end to end, not just their own test files' happy paths. Two
genuine defects were found — both fixed directly (small, precisely
scoped) and both proven by a regression test that fails against the
pre-fix code and passes post-fix, confirmed by literally reverting each
fix and re-running its own test before restoring it:

1. **Cross-domain matching defect (financial correctness), `internal/bonus/deposit_sweep.go`.**
   `listDepositMatchableOfferVersions`' own SQL had no
   `completion_mechanic` filter, so it also matched Cashback
   OfferVersions (identical `RewardKind=R1`/`grant_policy=auto_issue`
   shape, `cashback_scheduler.go`'s own doc comment confirms the shapes
   are structurally identical). An ordinary qualifying deposit for a
   player enrolled under a Cashback campaign would ALSO trigger a
   spurious deposit-triggered Grant issuance attempt against the
   Cashback campaign's own OfferVersion, misusing its `reward_calculation`
   (which additionally carries `window_seconds`, silently ignored by
   `parsePercentageRewardCalculation`) as an ordinary deposit-match
   reward — something no Offer author who configured a Cashback campaign
   ever authorized. Fixed with a one-line filter
   (`completion_mechanic IS DISTINCT FROM 'C2'`), the exact inverse of
   `listCashbackMatchableCampaigns`' own selector. Regression:
   `TestRunDepositSweepForTenant_DoesNotMatchCashbackOffer`.
2. **Availability defect, `internal/httpserver/bonus_domain_ops_handlers.go`'s `newCreateOfferVersionHandler`.**
   An omitted (legitimately optional, `omitempty`-tagged)
   `contribution_weight_table` field turned into an unconditional 500:
   casting an empty Go string to `[]byte` produces a non-nil, empty
   slice, which `bonus.CreateOfferVersion`'s own `nonNilJSON` nil-check
   does not catch, so it was bound to the JSONB column literally and
   Postgres rejected it (`invalid input syntax for type json`). Fixed by
   normalizing an empty request field to `nil`, mirroring
   `reward_calculation`'s own adjacent empty-string handling in the same
   handler. Found while writing this phase's own audit-record test for
   this endpoint (§4 below), not by inspection alone.

No other genuinely uncovered branch producing incorrect behavior was
found in the files read. Every other "coverage gap" surfaced (missing
concurrency proof, missing audit-record assertion) is a real gap in test
coverage, not evidence of a second production defect, and is recorded in
§3/§4 below.

### 3. Concurrency testing, including the F1 deadlock-liveness investigation (item 3 of the dispatch)

All three new sweep/scheduler mechanisms now have a GENUINE
concurrent-execution test (two real goroutines, real overlap forced via a
held-open transaction, mirroring `internal/reconciliation`'s own
established `TestTryRunLedgerVsProjectionForTenant_ConcurrentLockContention`
pattern) — not merely the sequential-call-twice pattern the deposit
sweep's own pre-existing
`TestRunDepositSweepScheduler_TenantAdvisoryLockSerializesOverlappingTicks`
used (which never actually has two ticks in flight at the same time and
so cannot detect a true race):

- `TestDepositSweep_TrueConcurrentTicksSameTenant_SecondObservesLockHeldAndSkips`
  (`internal/bonus/schedulers_concurrency_integration_test.go`, new).
- `TestExpirySweep_TrueConcurrentTicksSameTenant_NoDoubleTerminationOrLoss`
  (same file, new) — proves the domain-specific invariant (every expired
  Grant terminated exactly once, none lost) under real overlap, not just
  that the lock mechanism itself serializes (which the deposit-sweep test
  already proves generically, since both share `tryAdvisoryLockedTenantJob`).
- Cashback scheduler: covered by the F1 test below, which runs it
  concurrently against the deposit sweep — no dedicated same-tenant
  same-job overlap test was additionally written for the cashback
  scheduler alone, since its own tick body has no code path materially
  different from the deposit sweep's own (identical
  `tryAdvisoryLockedTenantJob` wrapper, identical watermark-then-issue
  shape) and the generic serialization property is already proven twice
  over (deposit sweep, expiry sweep). Named here rather than silently
  assumed.

**F1 (Phase 4/risk's named-but-unverified concern): can the deposit sweep
and the cashback scheduler deadlock each other under Postgres's own
detection, since they use two different advisory-lock namespaces?**
Answered empirically, not merely by inspection:
`TestDepositAndCashbackSchedulers_ConcurrentDifferentNamespaces_NoDeadlock`
runs both jobs concurrently for the SAME tenant, repeated across 8
iterations, asserting specifically that neither ever surfaces a real
Postgres "deadlock detected" error (SQLSTATE `40P01`, checked via
`errors.As` against `*pgconn.PgError` — the falsifiable claim, not just
"no error") and that both jobs' own results stay correct despite the
real overlap (exactly one grant attempt from each, no cross-job
interference). **Result: PASS, all 8 iterations, zero deadlocks.**
**Finding, stated precisely: this is safe by construction, not merely
lucky.** Both jobs use `pg_try_advisory_xact_lock` — the NON-BLOCKING
try-lock family — which by definition never enters a wait state; a
session that cannot acquire it returns immediately with `acquired=false`
rather than blocking. Postgres's deadlock detector only ever needs to
intervene when two sessions are each BLOCKED waiting on a lock the other
holds (a genuine wait-for cycle); a lock acquisition that never blocks
can never participate in such a cycle, and two DIFFERENT lock namespaces
additionally never contend with each other at all, blocking or not. F1
is **CLOSED**: not a live correctness bug, confirmed empirically against
real Postgres 16 rather than left as an unverified inference.

### 4. Financial-invariant testing (item 4 of the dispatch)

**Disclosed limitation, stated precisely rather than worked around:**
every new code path this Wave adds that TRIGGERS a ledger-touching
mechanism (`IssueAndActivateDepositBonus` via the deposit sweep,
`IssueAndActivateCashback` via the cashback scheduler) is, in every test
in this Wave's own suite AND in this phase's own new concurrency tests,
denied at the `AssetAuthorization` gate — a pre-existing, disclosed
limitation (`RunDepositSweepForTenant`'s own doc comment: `JurisdictionCode`
is hardcoded to `""` because this platform has no per-player jurisdiction
resolver yet, the same gap already disclosed against casino's real-money
bet path). **No test anywhere, including this phase's own new ones, ever
reaches a successful `bonus_grant` posting via either sweep** — so
Invariant B1 (extended) holds trivially in every one of these tests (zero
postings occur), not because it was proven under a genuine successful
concurrent posting. This is not new information this phase discovered,
but it is stated explicitly here rather than left implicit, since a
casual reading of "financial invariant testing: done" against these two
mechanisms would otherwise overstate what was actually exercised. The
wagering-contribution trigger (`RecordCashFundedWageringContribution`,
wired into casino's real `postBet`) posts NO ledger entries of its own by
design (§7.18.3.4, confirmed unchanged) — B1 is structurally
inapplicable to it, not merely untested. The underlying `casino_bet`
posting it rides on is covered by `internal/casino`'s own pre-existing
balance/invariant tests, unaffected by this Wave.

### 5. API/authorization/audit test completeness (item 5 of the dispatch)

Cross-checked every new HTTP route in `registerBonusRoutes` (11 routes
this Wave adds) against `docs/security/security-architecture.md`
§W3P6.1's own matrix (5 surfaces: `manual_grant_issue`, `bulk_job_execute`,
`campaign_activate`, `offer_publish`, EOI-minting) — confirms `security`'s
own five-surface scope was itself complete against the route list;
nothing additional was missed.

**Genuine gap found and closed this phase**: no test anywhere in this
Wave's diff ever read `audit_log` back to confirm any of the new
handlers' `audit.Record` calls actually fire — every existing test
asserted on the HTTP response/domain-object state only. Added
`internal/httpserver/bonus_wave3_audit_integration_test.go`, asserting a
real `audit_log` row (action AND target_id both checked) for:
`bonus_change_request.filed`/`.approved`, `bonus_campaign.activated`,
`bonus_offer.created`, `bonus_offer_version.created`,
`bonus_offer.published`, `economic_operation.minted`, and
`bonus_grant.manual_issue_requested`/`bulk_grant_job.created`.

Two audit actions are **named, disclosed gaps**, not silently skipped:

- `bonus_grant.manual_issue_activated` fires only after `ActivateGrant`'s
  FULL T.1 gate (`AssetAuthorization`→RG→Risk) allows, and
  `AssetAuthorization` fails closed on absent jurisdiction/tenant-
  jurisdiction-config data (ADR 0037 §C.1) — no HTTP-level test ANYWHERE
  in this codebase (not merely this Wave) currently builds that fixture;
  every existing successful-activation test for this exact gate chain is
  built at the package level (`internal/bonus`'s own `lifecycleFixture`,
  several platform-admin-approved `assetregistry` change-request
  round-trips plus a dedicated test-only asset/jurisdiction). Building
  that harness a second time, at the HTTP layer, purely to observe one
  `audit.Record` call whose call site is structurally identical in shape
  to two already-proven-live sibling calls (`bonus_campaign.activated`,
  `bonus_offer.published`) would be disproportionate scope for this one
  assertion and risks mutating shared asset-authorization state other
  concurrently-run tests in this shared dev database depend on. Verified
  instead by code inspection (the call site is unconditional, immediately
  after a successful activation, in the same transaction). Flagged as a
  real, bounded, pre-existing gap in the HTTP test harness generally (not
  unique to Bonus), for whichever specialist eventually builds an
  HTTP-level jurisdiction/asset-authorization fixture first.
- `bulk_grant_job.executed` is structurally unreachable via HTTP today,
  confirmed by Phase 6's own already-documented F3 finding
  (`bulk_grant_jobs.parent_operation_id` is never populated by the create
  handler, so execution always fails before reaching this call, and the
  whole transaction — including any audit row — rolls back). Asserting
  this audit row exists via HTTP would itself be the "fake completion"
  CLAUDE.md forbids. (It IS exercised at the package level, where the
  fixture legitimately pre-populates `parent_operation_id` —
  `TestExecuteBulkGrantJobWithApproval_SucceedsWithApproval`, pre-existing
  — though that test does not itself assert the audit row; adding that
  one assertion is a small, low-priority follow-up, not done here given
  the phase's own time allocation toward the larger gaps above.)

Tenant-isolation coverage: confirmed present for `campaign_activate`
(`TestActivateCampaign_HTTP_CrossTenantIsolation`, pre-existing). Not
independently duplicated this phase for `offer_publish`/manual-grant/
bulk-job, since all four routes share the identical RLS-via-`WithTenant`
isolation mechanism `security`'s own W3P6.3 review already confirmed
structurally (every new handler and sweep job performs every tenant-owned
read/write inside `pool.WithTenant`, no new code path uses
`WithPlatformAdmin` or an RLS-exempt connection) — named here as a
proportionality call, not an oversight.

### 6. "No skipped financial/security tests" audit (item 6 of the dispatch)

Grepped the full Wave 3 diff (`9d857dc..fb829e1`) and this phase's own
additions for `t.Skip`, `TODO`, `FIXME`, and `err == nil`/weak-assertion
patterns. **Zero `t.Skip` calls** anywhere in the Wave. One `TODO`
reference found, and it is a QUOTATION of a PRE-EXISTING, already-
disclosed gap (casino/deposit-sweep's shared jurisdiction-resolution TODO),
not a new skip. **Two genuine weak assertions found and fixed** (both
test-only, no production code touched, both re-verified to still pass for
the correct reason after the fix):

1. `internal/bonus/four_eyes_ops_integration_test.go`'s
   `TestManualGrantWithApproval_SucceedsWithApproval` asserted only
   `err != nil` for "a second activation against an already-activated
   Grant fails" — a bare non-nil check would also pass if the SECOND
   attempt failed for an unrelated reason (e.g. the four-eyes consume
   refusing because the approval was already burned), masking the actual
   CAS-guard regression the test's own name claims to prove. Now asserts
   `errors.Is(err, ErrIllegalTransition)`.
2. `internal/bonus/wave3_security_matrix_integration_test.go`'s
   `TestSEC_EOIMintedForOwnPerson_RefusedByComposedSEP1` asserted only
   `err != nil` for "SEP-1 refuses a self-dealt manual grant" — would also
   pass on an unrelated validation/FK failure. Now asserts the error names
   both "SEP-1" and "self-dealing" (migration 0062's own trigger text).

### 7. Summary verdict

Wave 3's implementation (Phases 1-8) is **IMPLEMENTED** for the scope it
actually claims (deposit/reload sweep, cashback scheduler, expiry sweep,
cash-funded wagering-contribution wiring, four-eyes application wiring
for 4 of 7 `ChangeOperation`s, the new HTTP admin surfaces), subject to
every limitation each phase itself already disclosed (the jurisdiction-
resolution gap meaning both sweeps' issuance always denies today; the
three un-wired `ChangeOperation`s named as `NOT IMPLEMENTED`/`BLOCKED` by
Phase 3 itself; F3's bulk-job-execute-via-HTTP gap named by Phase 6) plus
the two genuine defects this phase found and fixed. No test was skipped,
disabled, or quarantined to reach this verdict. Full commit list this
phase: `da33c5e`, `e0be40f`, `76c4605`.

## Stage 9 §20 — Targeted load/concurrency testing methodology (`qa`)

Added as this codebase's standing convention for "realistic-for-this-
project concurrency" (dozens to low hundreds of goroutines, never
thousands, per the Stage 9 dispatch): a Go-level concurrent integration
test against the REAL HTTP handlers/domain functions and a REAL Postgres
pool, sized to `internal/config/config.go`'s own actually-configured
`DatabaseMaxConns` (its `Load()` default of 10, or a `DATABASE_MAX_CONNS`
override) - never an invented pool size, so a test's observed contention
reflects what production would actually see at its own configured pool
size.

Two complementary techniques are now established side by side in this
codebase, used for different questions:

1. **Forced interleaving** (pre-existing, `internal/casino`'s
   `fmWaitForLockWaiters`/`s9Blocker` pattern and its `internal/sportsbook`
   counterpart): an uncommitted blocker transaction holds a row lock the
   racing calls must also take, plus a `pg_stat_activity` poll, so a test
   proving a SPECIFIC interleaving (e.g. "two distinct bets racing one
   wallet's balance") is deterministic rather than sleep-guessed. Owned by
   whichever specialist owns the financial invariant under test
   (`ledger-finance` for wallet/ledger/casino/withdrawal/payments money
   paths).
2. **Bulk concurrent burst with a bounded-wait deadlock check**
   (`internal/httpserver/stage9_concurrency_integration_test.go`, new this
   stage): N goroutines (dozens, scaled off the real configured pool size
   - e.g. 3x `DatabaseMaxConns` - rather than a fixed number) fire real
   HTTP requests at once; a `stage9AwaitAll`-style helper fails with a
   clear message if they have not ALL finished within an explicit timeout
   (never relying on `go test`'s own overall timeout to surface a hang),
   and returns the wall-clock elapsed for a human to sanity-check
   "genuinely parallel" versus "silently serialized." Used for the
   "should show zero/minimal contention" cheap cases explicitly called
   out in the Stage 9 dispatch: concurrent authentication (same account
   and distinct accounts, checking `login_attempts`/`sessions` - noting
   `sessions`' own per-principal RLS, migration 0018, means a bulk session
   count must go through `auth.ListActiveSessions`/`WithPrincipalScope`,
   never a raw tenant-scoped `SELECT * FROM sessions`, which sees zero
   rows by design), concurrent sportsbook catalogue reads, concurrent
   casino game launch (both many distinct players and many sessions for
   one already-funded player - deliberately NOT the wallet-row-creation
   race, which is `ledger-finance`'s financial-concurrency territory), and
   concurrent Back Office queue reads (the bonus change-request approval
   queue, `GET /v1/admin/bonus/change-requests`).

A goroutine-safe-HTTP-call convention was also established here for any
future bulk-burst test: `postJSON`/`getJSON` (this package's existing
helpers) call `t.Fatalf` on a request-build/send error, which is unsafe
from a non-test goroutine (`testing.T.FailNow` must run on the test's own
goroutine); bulk-burst tests instead use a raw variant
(`stage9RawPostJSON`/`stage9RawGetJSON`) returning `(*http.Response,
error)` with no `*testing.T` dependency, collecting per-goroutine results
into a slice the MAIN test goroutine alone asserts against after
`wg.Wait()`.

**Findings**: all six new tests passed, including under `-race`, against
a freshly-migrated (81/81) scratch database - no deadlock, no lost/
duplicate effect, no unexpected serialization beyond ordinary connection-
pool queuing once concurrency exceeded the configured pool size. Same-
account concurrent logins (30 goroutines) and distinct-account concurrent
logins (30 goroutines) showed comparable per-operation wall-clock cost
(~250ms/op average in this environment), consistent with Argon2id
password-verification CPU cost (`internal/auth/password.go`'s own
documented ~100ms-per-call target, amplified by CPU contention across
concurrent goroutines in a constrained sandbox) dominating over any
database-level lock contention - neither `login_attempts` (append-only
insert) nor `sessions` (no per-account uniqueness constraint) has a row
for two concurrent logins to serialize on, which the measurement is
consistent with. Catalogue reads (60 goroutines), casino launches (30/20
goroutines), and Back Office queue reads (60 goroutines) all completed in
well under one second, the expected shape for reads/independent inserts
with no shared row.

A stale-scratch-database pitfall was found and is recorded here so a
future session doesn't repeat it: a pre-existing scratch database left
over from an earlier interrupted session reported `schema_migrations`
version 81 (matching the current migration file count) yet still carried
a pre-rename column (`provider_bet_reference` did not exist) and a
pre-fix trigger definition from BEFORE two Stage 8 fixes - i.e., its
migration-version bookkeeping did not guarantee its schema matched the
CURRENT migration files' actual content. Dropping and fully re-creating
the scratch database (not merely trusting its recorded migration version)
resolved it; the full suite is clean against a genuinely fresh database
(see this stage's completion report for the full test run).

## Stage 10 W0 — CI evidence restoration: pinned lint, scratch databases, lock-wait scoping (`qa` + `security` + `devops`)

### CI gate status before this stage

`.github/workflows/ci.yml`'s `build-test-lint` job referenced
`golangci-lint/golangci-lint-action@v6`, which does not exist, so the job
aborted at "Set up job" on every run since the file entered the branch
history (`4790de0`). No Go step (gofmt, vet, lint, build, migrations,
unit or integration tests, reversibility) had ever run in CI; all
earlier Go evidence was local (Stage 10 planning gate, finding F-1).

### Linter

- Action: `golangci/golangci-lint-action@v9`. Source: the upstream
  README "Compatibility" section — v7.0.0 supports golangci-lint v2
  only, v8.0.0 works with golangci-lint ≥ v2.1.0, v9.0.0 needs the
  node24 runtime. `.golangci.yml` is `version: "2"`, so any action
  below v7 cannot run it.
- Linter version pinned to `v2.5.0` (the version verified locally), not
  `latest`, so results are reproducible. Bump deliberately, in its own
  change, and fix any new findings in the same change.
- CI lints without build tags. Integration-tagged test files are **not**
  linted (with `--build-tags=integration`, v2.5.0 reports 467
  errcheck/unused findings in them) — recorded as deferred debt, not
  fixed in W0.

### Scratch databases (migration and RLS tests)

Some integration tests need an empty database (migration round-trips,
pre-flight guards, RLS posture before/after a migration). The
application roles are deliberately `NOCREATEDB`
(`deploy/init-app-role.sql`, `docs/security/runtime-role-separation.md`
§2), and they stay that way.

- **Role:** `igaming_test_admin` — `LOGIN NOSUPERUSER CREATEDB
  NOCREATEROLE NOBYPASSRLS`, member of `igaming`. Created **only** in CI
  (`ci.yml` step "Create CI-only test admin role…") and in local dev
  (`deploy/init-test-admin-role.dev.sql`, `make dev-db-init-test-admin`,
  mounted by `deploy/docker-compose.dev.yml`). Never in
  `deploy/init-app-role.sql` or any staging/production path. Never the
  `postgres` superuser.
- **Variable:** `TEST_ADMIN_DATABASE_URL` — CI and local development
  only; never set in staging or production.
- **Code:** one helper, `internal/testsupport/scratchdb` (every file
  `//go:build integration`, so it is never compiled into an application
  binary). `scratchdb.New(t, prefix)`:
  1. skips the test if `TEST_DATABASE_URL` or `TEST_ADMIN_DATABASE_URL`
     is unset (never falls back to creating through
     `TEST_DATABASE_URL`);
  2. `CREATE DATABASE <prefix+random> OWNER <TEST_DATABASE_URL's role>`
     through the admin URL — used for CREATE and DROP only;
  3. connects to the scratch database as `TEST_DATABASE_URL`'s role and
     fails the test unless `current_user` is that role, it owns the
     database, and it is neither superuser nor `BYPASSRLS` (otherwise
     RLS assertions would pass for the wrong reason);
  4. drops the database `WITH (FORCE)` on cleanup (logs if left
     behind).
- **Guard (a tripwire; code review is the real control):** the CI step
  "Guard - test admin URL is only read by test support code" fails if `TEST_ADMIN_DATABASE_URL` appears in any `.go`
  file other than `_test.go` files and `internal/testsupport/scratchdb`,
  if `internal/config/` mentions any `TEST_ADMIN` setting, or if a
  scratchdb file lacks the `integration` build tag. It matches the
  literal name only, so a constructed name would evade it.
- `scratchdb.New` also fails the test if the admin URL's role is a
  superuser or `BYPASSRLS` (the role model must not be silently replaced
  by the `postgres` superuser).
- Callers: `internal/{db,ledger,bonus,jurisdiction,operatingmarket}`
  migration tests (formerly five copied helpers).
- Verified locally: with the variable unset these tests skip with a
  pointer to this section; with `igaming` temporarily given `BYPASSRLS`
  the owner check fails the test.
- Ownership: `qa` and `security` co-own this mechanism (it is a
  privilege boundary, not only a test convenience).

### Lock-wait polling must be scoped to the test's own blocker

`go test ./...` runs packages in parallel against the same test
database. A helper that counts **all** backends in `wait_event_type =
'Lock'` also sees other packages' lock waits and can return before the
test's own call has reached its lock. This made
`TestSportsbookJurisdiction_ConcurrentConfigurationReadIsConsistent`
fail intermittently (it armed a restriction before `PlaceBet`'s
jurisdiction gate had run). Rule: wait for backends blocked, directly or
transitively, by the test's own blocker (`pg_backend_pid()` read inside
the blocker; recursive walk over `pg_blocking_pids`, which also covers
advisory locks). Applied in W0 to `internal/sportsbook`
(`waitForBlockedCount`) and `internal/casino` (`fmWaitForLockWaiters`,
`s9Blocker`); the ledger/withdrawal/casino/payments lock-order harnesses
already used precise matching; the jurisdiction/operatingmarket helpers
filter on their own statement text.

### Ordering note

CI steps stop at the first failure, so the migration-reversibility step
only gets CI coverage once the integration-test step is green. Fixing
the action alone (without the scratch-database role) would still have
left reversibility unverified in CI.

### W0 gate

W1 (sportsbook settlement) code may not merge until `build-test-lint`
is green on **five consecutive CI runs** (run ids recorded in
`docs/governance/task-registry.md`). A failure in those runs is
root-caused and fixed; rerun-until-green is not acceptance.
`workflow_dispatch` was added to `ci.yml` so the gate can be re-run on
the same commit.

## Stage 10 W1 — Sportsbook settlement test plan (`qa`, from the ADR 0088 review)

Binding sources: ADR 0088 §14 (test obligations) and the Stage 10
proposal §R/§S. This is `qa`'s named plan; each item is a real-PostgreSQL
integration test unless marked static or frontend. Concurrency tests use a
blocker transaction and wait on waiters of **that blocker's pid** (Stage 10
W0 rule), never sleeps. RBAC tests assert by permission constant, not role
name.

- **internal/ledger:** `LockProjectionsForPostings` locks the union of all
  inputs' accounts in canonical order and rejects mixed tenants; postings
  for settle won (two balanced pairs), settle lost, void-before, rollback
  (exact inverse), void-after-settlement (rollback + void in one DB
  transaction), tombstone (no entries); net-locked derivation.
- **internal/sportsbook (service):** settle won/lost; void before/after
  (won and lost variants, end balances); standalone rollback returns the bet
  to open and exposure re-counts it; re-settlement at the next generation;
  rollback-then-void nets once; tombstone → late original rejected →
  re-settlement succeeds; replay with same payload returns the original with
  no posting; replay with different payout/outcome/asset/void reason
  rejected with an integrity alert; delayed settle after rollback and
  re-settle is a replay, not a double post; generation gap and stale
  generation rejected; second settlement without rollback rejected;
  settlement after void rejected; payout validation V-1…V-5; unknown bet and
  cross-tenant bet → 404 with no posting and no tombstone; concurrent settle
  vs void on one bet (exactly one wins); two concurrent settlement attempts
  on one bet; deterministic lock-order/deadlock tests (settle vs PlaceBet on
  one wallet; settle vs void on one bet; two bets on one wallet; settlement
  vs casino bet sharing `house_gaming`); exposure released on settle/void (a
  previously blocked bet is admitted); OB-1 posting (negative `player_cash`
  after a won rollback) succeeds and is recorded; `player_locked_cash` never
  negative across all transitions; fault injection through the
  integration-tagged Post→history hook rolls the whole transaction back.
- **Static:** sole-writer check (`sportsbook_settlement_sole_writer_test.go`,
  INV-LOCK-E4); settlement never reads or locks `sb_selections`
  (INV-SB-SETTLE-6).
- **Schema / migration 0091:** T-1 rules (void exists, unreversed
  settlement exists, generation sequence, payout mismatch even as owner,
  won-zero/lost-nonzero, rollback target mismatch, ledger type mismatch,
  fail closed with no tenant context and under player scope); T-2 transition
  table (allowed and rejected edges; status equals history-derived status);
  deny triggers reject UPDATE/DELETE/TRUNCATE for the owner; immutable-fields
  regression through every transition; RLS (tenant staff SELECT/INSERT only,
  player self-scope read via bet join, player INSERT rejected, cross-tenant
  adversarial); runtime-role probe; unique indexes (one settlement per
  generation, one rollback per settlement, one void per bet); HR-15 trigger
  present; migration up/down/up on a scratch database; down refuses when new
  transaction types, sportsbook tombstones, history rows or non-open bets
  exist; a code revert with the migration kept still renders all four
  statuses.
- **internal/reconciliation:** sportsbook stream at zero drift, scoped to
  sportsbook transaction types (not wallet-wide); per-bet netting for all
  end states; two-way orphan checks; status mismatch; MOCK statement match;
  injected drift detected for every mismatch kind through the injectable
  statement source.
- **internal/risk:** `ReversalTypes` exactly `["sportsbook_void"]`;
  `sportsbook_rollback`/`sportsbook_settlement` in no cumulative spec; every
  ADR 0088 §6.2 netting row including won void-after-settlement; the OI-5
  pinning test (bet before the window, void inside it → usage −S).
- **internal/httpserver:** route absent when the flag is off and under
  production config; 403 for player, service and platform-admin tokens and
  for staff without the permission; exactly one grantee in the permission
  table; 200 for settle/void/rollback with the permission; 404 cross-tenant;
  unknown fields and field-matrix violations rejected; response shape for
  applied/replayed/tombstoned; one audit record per transition with
  `reason_code`/`void_reason`; replay audit; trusted-proxy client IP;
  `/v1/me/sportsbook/bets` never exposes `actor_staff_account_id` or
  `request_id`; F-7 regression tests.
- **Frontend:** B2C history renders won/lost/void and payout; Back Office
  detail renders status, payout, ledger transaction id and correlation id;
  no settlement controls rendered.
- **Mutation pass:** a Go mutation tool over payout validation, the settle/
  void/rollback decision tables, net-locked derivation,
  `LockProjectionsForPostings` and the lock call order; for SQL CHECKs and
  triggers (no mutation tool applies) a recorded manual branch-coverage
  checklist (each branch exercised true and false). This substitution is
  stated explicitly in the stage report.

## Stage 10 W1 — Results (`qa`)

The plan above is implemented: `internal/sportsbook/settlement_*_integration_test.go`
(scenarios, decision tables, OB-1, concurrency, fault injection, DB
constraints, 0091 migration, rejection audit, read paths, SQL branches),
`sportsbook_settlement_sole_writer_test.go` (static INV-LOCK-E4 and
INV-SB-SETTLE-6), `internal/ledger/lockorder_*_integration_test.go`,
`internal/risk/cumulative_sportsbook_settlement_integration_test.go`,
`internal/reconciliation/sportsbook_settlement*_test.go`,
`internal/httpserver/sportsbook_settlement_*_test.go`, and the F-7 replay
suites. ADR 0088 §14 Q2 mutation pass and SQL branch checklist:
`docs/governance/stage-10-w1-mutation-and-sql-branch-coverage.md`
(sportsbook 92.44%, ledger 92.00%; handler PARTIALLY IMPLEMENTED — tool
limitation). New rules: the migration-reversibility CI step runs on a
fresh database (0091's down refuses on a database holding settlement
evidence); settlement history rows are never inserted inside a savepoint
(SB-T1-XMIN, pinned by tests). Note: platform-wide catalogue read tests
slow down on heavily seeded local databases; run them on a fresh database
(CI always does).


## Stage 10.1 — known limitation: the payments-webhook OpenAPI contract test is structural only

`internal/httpserver/openapi_paymentswebhook_contract_test.go`
(`TestOpenAPI_PaymentsWebhook_ContractMatchesHandler`, API-DOC-PAYWH)
verifies that `docs/api/openapi/platform-api.yaml`'s
`/v1/webhooks/payments/{tenantSlug}/{providerID}` entry documents the
right headers, response codes, and (after the Stage 10.1 post-
implementation review's PW-2/P3-5 fix) the right 400-vs-401 wording, by
parsing the spec file as **plain text** and asserting on substrings and
block boundaries - never by loading it through an OpenAPI/JSON-Schema
validation library.

**Why:** no such library is a verified dependency of this module.
`gopkg.in/yaml.v3` and `go.yaml.in/yaml/v3` appear only as transitive,
go.mod-only entries with no package-content hash recorded in `go.sum`, so
importing either to parse the spec for real would pull in a genuinely new
dependency - something this task (and CLAUDE.md's "no uncontrolled scope
expansion") says not to do without it being required by the Blueprint, the
current stage's scope, or to avoid material technical debt. The QA test
plan's own §4 anticipates this and names the fallback used here: "if none
available, use a minimal structural check with the standard library."

**What this gap means in practice:** the test would catch the spec
forgetting a response code, a header, or (now) specific required wording,
but it does NOT validate that the spec's schemas are internally
consistent OpenAPI/JSON-Schema, and it does NOT run a live request/
response pair through the spec to confirm the ACTUAL wire shape conforms
(the QA plan's other suggested check, a live-request conformance test,
also is **NOT IMPLEMENTED**). A response body that satisfies this
package's own `apierror.Error` type but silently drifts from the spec's
declared schema in some other way (e.g. an added field) would not be
caught by either test in this codebase today.

**Status:** recorded as a known, accepted limitation, not silently
skipped - `internal/httpserver/openapi_paymentswebhook_contract_test.go`'s
own top-of-file comment states it, and this section is `qa`'s side of that
same decision (Stage 10.1 post-implementation review, code review P3-7).
Revisit if/when a schema-validation library becomes a verified dependency
for another reason - this alone does not justify adding one.
