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
6. **Retail withdrawal exceeding an agent's available float.** `OPEN
   DECISION` — whether this is rejected outright or draws from an upstream
   node in the hierarchy is a retail-accounting design question for
   `architect`/`ledger-finance` to resolve (ADR 0035), not one this
   document invents an answer to. Both branches are testable once decided:
   if rejected, the test asserts a clean deny with no partial posting; if
   it draws upstream, the test asserts the draw is itself an atomic,
   auditable, idempotent ledger operation across both nodes' floats (i.e.
   it inherits this entire testing floor, not a lighter one, because it is
   still a financial write). This test class cannot be written until the
   decision is made, and its absence blocks retail withdrawal from being
   marked `IMPLEMENTED` — not a gap qa will wave through as "pending
   design."
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
