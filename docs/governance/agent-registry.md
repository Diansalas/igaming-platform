# Agent Registry

Permanent project governance document (Stage 4G, Part A). Defines every
role the project operates under and how each maps to this repository's
actual specialist agents (`.claude/agents/*.md`) or, where no dedicated
agent exists, to the specialist whose scope the directive's role maps
onto. This registry is the authoritative role list — `docs/governance/
ownership.md` maps roles to files/directories; `docs/governance/
integration-protocol.md` defines how they hand off work.

## Master Orchestrator

**Role**: the main Claude Code session for this project. The only role
authorized to decompose stage objectives into tasks, assign work,
sequence dependencies, resolve cross-domain conflicts, authorize
integration, order reviews/final testing, prepare the completion report,
declare a stage complete, and stop for human approval.

**Working pattern, stated explicitly**: across every stage to date
(0 through 4F) and continuing in 4G, the Orchestrator performs
cross-cutting and new-domain implementation directly (not by spawning
code-writing subagents), and delegates INDEPENDENT REVIEW to specialist
subagents run in parallel at the end of a stage (via the `Agent` tool).
This is a deliberate choice, not an oversight: Part A §2's "no
overwriting between agents" rule is trivially satisfied when one actor
performs the integration-sensitive implementation, and specialist review
subagents never write production code — they report findings back to the
Orchestrator, who decides which become code/tests/docs/task-registry
entries. If a future stage's scope genuinely calls for parallel
implementation by multiple specialists (e.g., two genuinely independent
packages with no shared files), the Orchestrator establishes file
ownership BEFORE dispatching work, per the Integration Protocol.

**Authority**: final say on scope, sequencing, and completion. No
specialist may override an Orchestrator decision; a specialist that
disagrees escalates via the reasoning in its own findings, and the
Orchestrator decides.

**Absolute constraint on every specialist (Stage 4G-FINAL, made explicit
because it was previously only implied by the working pattern above)**:
a specialist — whether the Orchestrator acting in that capacity, or a
dispatched `Agent` review/implementation task — MUST NOT (a) implement a
change in a file outside its own `ownership.md` entry without a recorded
dependency request first, (b) begin work not assigned to it by the
Orchestrator (no self-assigned scope expansion — `product-owner-proxy`'s
job exists precisely to catch this), or (c) mark its own significant
work "reviewed" or "integrated." Violating any of these three is a
process defect to be recorded in `task-registry.md`'s Dependency Request
Log or Integration Approval Log (below) exactly like any other finding,
even when the Orchestrator itself is the specialist that violated it.

## How the Orchestrator assigns every task to an owner

No task exists un-owned. Concretely:

1. Before any implementation begins, the Orchestrator adds a row to
   `task-registry.md`'s current stage table with an **Owner** already
   filled in — never blank, never "TBD." The owner is either a named
   specialist (`risk`, `casino`, `security`, …) or, per the Working
   Pattern above, "Orchestrator (implementing as `<domain>`)" when the
   Orchestrator performs the domain's own implementation directly. Both
   forms are a real, recorded assignment — "Orchestrator implemented it"
   is not an exemption from ownership, it is a stated ownership value.
2. **Files owned** is filled in from `ownership.md` at assignment time,
   not invented per-task — if a task needs a file `ownership.md` assigns
   to a different domain, that need becomes a Dependency Request (below)
   filed on the SAME row, not a silent broadening of "files owned."
3. A task with no dependencies starts immediately; a task with unresolved
   **Dependencies** or **Blockers** does not start until the registry
   shows them resolved.
4. Reassigning an owner mid-task (rare — e.g., a specialist proves the
   wrong fit) is itself a recorded event: the old row's Status becomes a
   terminal note ("Reassigned — see 4X-NN") and a new row is opened; the
   old row is never edited to silently swap the Owner column, preserving
   the historical record `task-registry.md` §5 already requires.

## Specialist roles and their repository mapping

| Directive role | Repository agent | Notes |
|---|---|---|
| Architecture | `architect` | Cross-domain architecture, service boundaries, ADR ownership for cross-cutting decisions. |
| Financial/Ledger | `ledger-finance` | Wallet/ledger/reconciliation/idempotency invariants — consulted before any code that debits/credits/reports a balance. |
| Identity | `identity-compliance` | Person/PlayerAccount model, cross-brand resolution (`internal/identityresolution`). |
| KYC | `identity-compliance` | `internal/kyc` — document/verification lifecycle, provider abstraction (Stage 4F). Same specialist as Identity: KYC is identity evidence, not a separate domain owner. |
| Responsible Gaming | `identity-compliance` | `internal/rg` — self-exclusion, RG controls. Kept conceptually distinct from Risk Management (see below) even though the same specialist owns both today. |
| Risk Management | `risk` (new, Stage 4G) | `internal/risk` — the central Risk & Limits engine. Never collapses into RG; see `docs/decisions/0031`. |
| Casino | `casino` | `internal/casino` — game gateway, launch, bet/win/rollback callbacks. |
| Sportsbook | `sportsbook` | Not yet active — no code exists under this domain. |
| Payments | `payments` | `internal/payments` — PSP orchestration, deposit/withdrawal flows. |
| Security | `security` | Auth, sessions, RBAC, secrets, tenant isolation, threat modeling. Reviews every security-sensitive change regardless of author. |
| PostgreSQL/RLS | `security` (primary) or `architect`, task-scoped | No dedicated agent file exists; live-database adversarial RLS review is commissioned as a scoped `Agent` task naming the exact tables/policies under review (precedent: Stage 4F's migration-0040 review). Revisit creating a dedicated agent if this workload grows. |
| API/HTTP | `backend` | `internal/httpserver` outside a more specific domain's own handlers (each domain specialist owns its own handler files — see `ownership.md`). |
| QA | `qa` | Testing strategy, gates, cross-cutting integration/concurrency/tenant-isolation tests. |
| Adversarial Testing | `qa` (primary) or `security`, task-scoped | No dedicated agent file exists; commissioned as a scoped review task asking the exact adversarial questions for the stage (precedent: every stage 3B-4F completion review). Revisit if this workload grows independently of `qa`. |
| Documentation | `architect` for architecture docs; each specialist for its own domain's ADRs/status sections | No single "documentation" agent — every specialist documents its own domain; the Orchestrator owns `docs/progress.md`/`docs/active-stage.md`/`docs/governance/*`. |
| Code review | `code-reviewer` | Independent review of significant changes — correctness, CLAUDE.md adherence, simplification, consistency with `docs/architecture/`. |
| Overengineering guard | `product-owner-proxy` | Checks new work against B2C-MVP-first/future-B2B-compatible scope before it starts. |

## Per-agent responsibility/boundary/dependency summary

Full detail lives in each agent's own `.claude/agents/<name>.md` file —
this table is a navigation aid, not a duplicate of that content.

| Agent | Explicit responsibility | Explicit boundary | Depends on |
|---|---|---|---|
| architect | Cross-domain architecture, service boundaries, multi-tenancy isolation strategy | Does not implement domain-specific business logic | All domain specialists for their own scope |
| ledger-finance | Wallet/ledger/balance/reconciliation/idempotency correctness | Does not own provider integration business logic | `security` for RLS on financial tables |
| identity-compliance | Person/PlayerAccount/KYC/RG | Does not own auth session mechanics (that's `security`) | `security`, `architect` |
| risk | `internal/risk` central Risk & Limits engine | Does not duplicate RG, does not implement per-domain limit logic | `ledger-finance`, `identity-compliance`, `security` |
| casino | `internal/casino` game gateway/launch/callbacks | Does not change ledger schema or RG/Risk logic directly — calls their interfaces | `ledger-finance`, `identity-compliance` (RG), `risk` |
| sportsbook | Sportsbook provider integration | Not yet active | `ledger-finance`, `risk` |
| payments | PSP orchestration, deposit/withdrawal | Does not change ledger schema directly | `ledger-finance` |
| security | AuthN/authZ, RBAC, RLS, secrets, threat modeling | Cross-domain review only unless explicitly assigned implementation | none (top-level review authority) |
| backend | General backend services not owned by a more specific specialist | Does not touch wallet/ledger, payments, KYC/AML/RG, or auth internals | Domain specialists for those areas |
| qa | Testing strategy and gates | Does not own production code | All specialists (test coverage of their code) |
| code-reviewer | Independent review of completed changes | Does not review before implementation is complete | none |
| product-owner-proxy | Scope-creep guard | Does not implement | none |

## Escalation path

1. A specialist that needs a change outside its own ownership files a
   documented change/dependency request (see `integration-protocol.md`).
2. The Orchestrator assigns it to the owning specialist (per
   `ownership.md`), or performs it directly when the Orchestrator itself
   is the de facto implementer for that domain this stage.
3. The owning specialist implements, reports the interface/change back.
4. The dependent specialist/work integrates against the reported change.
5. Any disagreement about ownership or scope is decided by the
   Orchestrator, recorded in `docs/governance/task-registry.md`.
