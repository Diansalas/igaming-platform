# Active Stage

## Stage 4G — Project Orchestration Governance + Risk & Limits Engine — Complete

Status: **Complete, pending human approval to authorize the next stage.**
Two parts: (A) permanent project orchestration governance
(`docs/governance/*`, a Master Orchestrator model, agent/task registries,
ownership, integration protocol, change control), and (B) the platform's
first central Risk & Limits engine (`internal/risk`) - ONE reusable
rule/policy architecture, never a separate limit engine per domain.

### Part A — Governance

- `docs/governance/agent-registry.md`, `ownership.md`,
  `integration-protocol.md`, `change-control.md`, `task-registry.md`,
  `project-status.md` - the permanent process documents this and every
  future stage operates under.
- `.claude/agents/risk.md` - a new specialist definition for the Risk
  Management domain, mapped in the agent registry alongside every
  existing specialist.
- The Master Orchestrator role formalizes this session's own established
  working pattern (direct implementation for cross-cutting/new-domain
  work, parallel independent specialist review at stage end) rather than
  introducing a new, untested multi-agent code-writing pipeline.

### Part B — Risk & Limits engine

1. **Central decision boundary**: `risk.Evaluate(ctx, tx, RiskRequest) ->
   (RiskDecision, error)` - `Outcome` is exactly `allow`/`deny`/`review`,
   with matched-rule reporting and a fail-closed contract (any error MUST
   be treated as deny, never allow).
2. **Rule model**: `risk_rules` (migration 0041) - one dual-scope table
   (platform-wide/tenant-owned, mirroring `player_restrictions`'
   precedent), with HARD_LIMIT/CONFIGURABLE_LIMIT/RISK_SIGNAL kinds and
   deterministic precedence (most-specific-configurable-wins, ALL hard
   limits always enforced, conflicting rules fail closed rather than
   guessing).
3. **Limit kinds implemented**: `min_amount`/`max_amount`/
   `cumulative_amount` only - `count`/`velocity`/`exposure`/`loss` are
   documented future extensions, deliberately not database-configurable
   yet (directive's own "prioritize the architecture" instruction).
4. **Enforcement**: wired into `internal/casino`'s `LaunchGame`
   (real-mode only) and `postBet` (every delivery, after RG eligibility,
   before the balance lock) - the only two of six designed operations
   actually enforced this stage; `deposit`/`withdrawal`/`sportsbook_bet`/
   `bonus_grant` are designed, documented integration points, not wired.
5. **Risk vs Responsible Gaming**: kept strictly separate domains -
   `internal/rg.EvaluateEligibility` remains the sole self-exclusion
   authority; `internal/risk` has no self-exclusion concept; both are
   consulted by casino's orchestrator in a fixed order (RG first, always
   short-circuiting Risk on denial).
6. **New RBAC**: `risk_manager` StaffRole/Role, `risk_config:read`
   (risk_manager + tenant_admin) / `risk_config:manage` (risk_manager
   only) permissions - mirroring the Stage 4F verification-permission
   separation-of-duties precedent exactly, including the identical
   self-escalation guard in staff creation.

Full design, rationale, every recorded open decision, and the complete
specialist-review findings/fixes list:
`docs/decisions/0031-risk-and-limits-engine.md`.

### Specialist review: 9 areas, real P0/P1s found and fixed

A 9-area independent parallel review (risk architecture, financial
correctness, casino integration, RG integration/domain separation,
security, PostgreSQL/RLS, API, adversarial testing, multi-tenancy) found
genuine, concrete bugs - not merely documentation gaps - before this
stage was considered complete:

1. **P1 (found independently by 4 reviewers) - specificity scoring
   bug**: a rule scoped by `(tenant)` and a rule scoped by `(tenant,
   asset_code)` were scored as an ARTIFICIAL TIE, forcing
   `ErrConflictingRules` - a fail-closed outage of the entire operation
   for that tenant, for two rules never actually in conflict. **Fixed**:
   rescored as a bitmask summing every present scope dimension.
2. **P1 - non-deterministic deny-vs-review aggregation**: a Go map's
   randomized iteration order could let a later REVIEW-action breach
   silently downgrade an earlier DENY-action breach. **Fixed**:
   deny-priority merge, order-independent by construction; added
   `ORDER BY id` to the base query for defense in depth.
3. **P1 (found independently by 2 reviewers) - int64 overflow in the
   cumulative-amount check**: summing many `NUMERIC(38,0)` ledger
   entries into a plain `int64` could silently wrap negative for an
   18-exponent asset, failing OPEN exactly where fail-closed matters
   most. **Fixed**: scanned as `pgtype.Numeric`, compared via
   `math/big`.
4. **P1 - rolled-back bets permanently consumed cumulative capacity**:
   the query counted a bet's debit even after a `casino_rollback`
   reversed it. **Fixed**: nets debits minus credits across both
   transaction types.
5. **P1 (live-database confirmed) - `risk_rules` RLS missing the
   player-scope guard**: a `db.WithPlayerScope` connection could read
   every tenant's risk rules and successfully disable one, contradicting
   this codebase's own documented isolation contract (no player-facing
   code path reaches this today - defense-in-depth, not a live
   incident). **Fixed**: added the guard to all four RLS policies.
6. **P0 (adversarial) - two real code paths had zero test coverage**:
   `RuleRiskSignal` contributing to `REVIEW`, and a `HARD_LIMIT` rule
   with `action=review`. **Fixed** with new tests proving both actually
   work.
7. **P0/P1 (adversarial) - missing integration-level tests**: `Evaluate`
   itself skipping an out-of-window rule (only a unit test of the helper
   existed), and cross-tenant denial of the DISABLE HTTP endpoint. Both
   added.
8. **P1 (this ADR itself) - ADR 0031 and the progress/active-stage
   entries did not exist** at the time code already cited them. Closed
   by this document's own existence.

Several P2s were fixed (OpenAPI documentation for the three new
endpoints; audit records for rule creation/disabling now carry IP/user-
agent/request-id; HTTP-layer enum validation for every rule field; risk
denial audit metadata enriched with provider/game/asset/amount; the down
migration documents the `risk_manager`-row one-way-door). Several more
were explicitly recorded as accepted/deferred with reasoning in ADR
0031's own findings section - none silently dropped.

### Newly disclosed limitations (not defects, recorded explicitly)

- Jurisdiction-scoped rules are reachable only from `LaunchGame`, never
  from `postBet` - `casino_launch_sessions` does not persist the
  jurisdiction resolved at launch time (the same pre-existing
  "TODO(jurisdiction)" gap named elsewhere in this codebase, not new).
- No licence-mode scoping dimension exists yet - a platform-wide
  `HARD_LIMIT` would apply identically inside a future bring-your-own-
  licence tenant under a different jurisdiction's own legal regime. No
  such tenant exists yet; recorded as an open decision before one does.
- No role can create a genuinely platform-wide rule via HTTP this stage
  (mirrors `internal/rg.CreateStaffRestriction`'s identical, already-
  established precedent).

### Verification performed

`gofmt -l .` clean. `go build ./...` clean. `go vet -tags=integration
./...` clean. `go test ./...`, `go test -race ./...`, `go test
-tags=integration ./...` all pass cleanly across the full repository. `go
test -race -tags=integration ./...` passes except for the pre-existing,
already-documented `TestConcurrent_DuplicateBetDeliveryDuringSelfExclusion`
flake (Stage 4D-RG/4E, unrelated to this stage, confirmed via repeated
isolated re-runs to still be intermittent and pre-existing). Migration
`0041` round-tripped (`up` -> `down` -> `up`) cleanly against the live dev
database, including after the RLS player-scope-guard fix.

### Decisions/input still useful from the human before the next stage

1. Approve Stage 4G and authorize the next stage (per CLAUDE.md's stage
   gate). Directive explicitly forbids starting Bonus, a real KYC
   provider, a real casino provider, or sportsbook automatically.
2. Decide the licensing-mode scoping question before any bring-your-own-
   licence tenant onboards (ADR 0031 §8).
3. Decide whether a `REVIEW` risk outcome should ever proceed
   provisionally pending a compliance workflow (ADR 0031 §8) - today it
   blocks identically to `DENY` at both enforcement points.
4. The already-open, non-blocking items carried forward from Stages
   0-4F remain open (see `docs/governance/project-status.md`'s
   consolidated list).
