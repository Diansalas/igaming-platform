# Change Control

Permanent project governance document (Stage 4G, Part A). Defines what
kind of review a given kind of change requires before the Orchestrator
may mark it integrated/complete.

| Change type | Required |
|---|---|
| Architectural change (new package, new cross-domain interface, new service boundary) | An ADR under `docs/decisions/`, reviewed by `architect` (or the Orchestrator acting in that capacity, per `agent-registry.md`'s working-pattern note) |
| Schema change (new table, new column, new constraint, new RLS policy) | A migration (`up`/`down`, round-tripped against a real database) + tests (RLS/tenant-isolation adversarial tests at minimum for any tenant-owned table) |
| API change (new/changed endpoint) | `docs/api/openapi/platform-api.yaml` update + a test exercising the endpoint over real HTTP |
| Security-relevant change (auth, sessions, RBAC, secrets, PII, tenant isolation) | `security` specialist review before the change is marked complete |
| Financial change (anything touching `internal/ledger`, `internal/wallet`, or a balance-affecting flow) | `ledger-finance` specialist review before the change is marked complete |
| Cross-domain change (touches files owned by more than one domain per `ownership.md`) | Master Orchestrator approval, via the dependency-request procedure in `integration-protocol.md` — never a direct multi-domain edit by one specialist |
| New StaffRole / new RBAC permission | `security` review + an explicit entry in the relevant permission table (`internal/auth/permission.go`) with tests proving the permission is neither over- nor under-granted |
| Anything the Orchestrator judges "significant" | Independent `code-reviewer` pass in addition to the above |
| Infrastructure change (Terraform, IAM policy documents, deployment scripts, CI workflow) | `devops` owner + `security` review for IAM/network/secrets; offline `deploy/aws/tests/run-static-checks.sh`; IAM policy changes re-validated with Access Analyzer `ValidatePolicy` and `deploy/aws/tests/simulate-deployer-policies.py`; no apply against a running environment without explicit human authorization (source: `CLAUDE.md` "Environment safety"; `.claude/agents/devops.md`; ADR 0086; `docs/runbooks/stage-9-4-staging-lifecycle-runbook.md` §1 "re-validate it (Access Analyzer + simulate-deployer-policies.py)"; Stage 10 planning-gate finding F-5) |

## Rules

1. **An architectural change without an ADR is not complete** — even if
   the code compiles and tests pass, CLAUDE.md's own rule stands: never
   present a recommendation as if it were settled, never silently
   contradict a documented decision.
2. **A schema change without a round-tripped migration is not complete**
   — `up` then `down` then `up` against a real database, verified live,
   not merely "the SQL looks reversible."
3. **A security-relevant change is not complete without `security`'s
   review**, regardless of how confident the implementing specialist is.
   This mirrors `security`'s own stated authority in
   `.claude/agents/security.md`: "this block stands even under schedule
   pressure."
4. **A financial change is not complete without `ledger-finance`'s
   review**, for the identical reason — CLAUDE.md's "consult before any
   other specialist writes code that debits, credits, or reports a
   balance."
5. **No specialist marks its own significant work "reviewed."** Review is
   always performed by a different specialist (or, for adversarial/
   PostgreSQL-RLS review, a scoped `Agent` task run independently of the
   implementing turn) than the one that wrote the change.
6. **P0/P1 findings from any required review block completion.** A stage
   is not "done" while an unresolved P0/P1 exists — see each stage's own
   governing directive for the exact bar (this project's convention to
   date: fix all P0/P1 before the completion report, record P2s
   explicitly rather than silently dropping them).
7. **Test results are reported per the standard in
   `docs/testing/testing-strategy.md`'s "Test reporting standard"**
   (Stage 4G-FINAL) — PASS/FAIL/FLAKE/NOT RUN/BLOCKED per suite, never a
   blanket "all clean" when any suite actually failed or flaked. A FLAKE
   verdict requires stated evidence, not just the label.
8. **CI is the evidence gate.** A change is not complete while
   `.github/workflows/ci.yml`'s `build-test-lint` job is red or skipped
   on its commit; local runs are supporting evidence, not a substitute
   (source: the human's Stage 10 approval directive, 2026-09-25 — "Do
   not count local runs as CI runs"; context: Stage 10 planning-gate
   finding F-1 — the Go job never ran from `4790de0` until Stage 10 W0).

### Existing binding rules recorded here for visibility (Stage 10 W0)

These rules were already binding through the cited ADRs and stage
directives; they are listed here so they are not only discoverable in
those records. **No new rule is created by this section.**

- **Fail closed where the architecture requires it** — an unconfigured,
  unknown or unresolvable decision input denies rather than permits
  (`.claude/agents/risk.md`; ADR 0031; ADR 0085 APP_ENV;
  `docs/governance/stage-4i-exit-register.md` "Integration contract";
  ADR 0083 §6.1.4).
- **Do not reopen a decided human decision or completed architecture
  without a concrete defect** — decisions are revised only through a
  recorded decision (`MASTER-BUILD-PROMPT.md` preamble; Stage 9.2
  operating principle in `docs/governance/task-registry.md`; human
  answers in ADR 0042 are binding).
- **Infrastructure is not changed casually** — see the "Infrastructure
  change" row above (`CLAUDE.md` "Environment safety";
  `.claude/agents/devops.md`; ADR 0086).

