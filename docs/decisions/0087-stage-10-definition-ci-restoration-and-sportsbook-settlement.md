# ADR 0087 — Stage 10 Definition: CI Evidence Restoration + Sportsbook Settlement Lifecycle

- **Status:** ACCEPTED — approved by the human on 2026-09-25 against
  planning-gate commit `2355ab7a679af67178c9166fab5b9b02a6aa2c9a`.
- **Decision type:** stage definition (`MASTER-BUILD-PROMPT.md`
  "Stages": a new stage is proposed only as a recorded decision).
- **Source proposal:** `docs/plans/stage-10-planning-gate-proposal.md`
  (§8 scope, §9 specialist review consolidation).
- **Owner:** Master Orchestrator.

> Numbering note: this is ADR 0087 in `docs/decisions/`. Migration
> `0087_sportsbook_jurisdiction_restrictions` in `migrations/` is an
> unrelated file.

## Context

After Stage 9.4 the human deployed staging (`9190d5d`) and reported the
B2C and Back Office acceptance as passed. The Stage 10 planning gate
reconstructed the project state from the repository and found that
CI's Go gate had never executed on this branch (F-1), and that the only
unimplemented core B2C money flow needing no new human decision is
sportsbook settlement for cash-funded singles (ADR 0038 §5/§8.1/§10;
ADR 0083 §6.1.2).

## Decision

Stage 10 consists of two ordered workstreams:

1. **W0 — CI evidence restoration** (proposal §8 H "W0"): fix the lint
   action and pin the linter; a CI/dev-only `igaming_test_admin` role for
   scratch databases (application roles unchanged); fix lint findings;
   re-validate `b22d5c4`; refresh records.
2. **W1 — Sportsbook settlement lifecycle** (proposal §8 H "W1"):
   cash-funded singles, in-house mock mode — settle won, settle lost,
   void before settlement, void after settlement, rollback, re-settlement,
   rollback-then-void — on the canonical ledger, with idempotency,
   audit, authorization, RLS/tenant isolation, concurrency safety and
   deterministic tests; F-7 (`ledger.Post` replay payload comparison)
   audited across all callers and fixed if required.

**Gate between them:** W1 code may not start until `build-test-lint` is
green on **five consecutive CI runs** (not local runs), recorded in
`docs/governance/task-registry.md`.

**Out of scope** (proposal §I, restated by the human): real sportsbook
provider settlement and webhooks; provider references; cashout;
bonus-funded or mixed-funded settlement; operator manual settlement;
self-exclusion auto-void consumer; liability reporting; jurisdiction
rung 2; any AWS deployment; any change to the running staging
environment.

## Human answers recorded with the approval (proposal §V)

| # | Item | Answer |
|---|---|---|
| 1 | Approve Stage 10 as proposed | **Approved** — "exactly within the approved scope" |
| 2 | W0 test-admin mechanism | **Approved** — dedicated test-only/admin role for CI/dev only; do not weaken application roles; do not grant CREATEDB to runtime application roles |
| 3 | Verification credential for `b22d5c4` live checks | **Not supplied.** Do not modify IAM, do not create a privileged user; determine whether existing credentials suffice; if not, document the exact blocked validation and permissions as an isolated external dependency and continue |
| 4 | Staging disposition | **Keep the current AWS staging environment running**; no `deploy.sh down`, no destroy/recreate, no modification for W0/W1 development |
| 5 | OB-1 | **Remains OPEN** and documented as such; not resolved in Stage 10 |

## Binding constraints (from the approval)

- No reopening of decided human decisions; no invented requirements; no
  scope expansion.
- A blocker affecting one validation item is isolated and documented;
  independent work continues.
- The mock sportsbook settlement is never described as a real provider
  integration; future provider settlement/webhook requirements stay
  recorded for the real-provider stage (proposal §9.1).
- The test-support settlement route is strictly test-only: fails closed
  when test support is disabled, staff-authorized, tenant-scoped,
  audited, never a production bypass, never a player capability.
- Gates: (1) W0 implementation complete; (2) five consecutive green CI
  runs; (3) W1 implementation complete; (4) full Stage 10 acceptance
  suite passes; (5) specialist/code/security review complete;
  (6) documentation complete; (7) clean git tree and pushed commit.

## Consequences

- The Stage 10 settlement implementation contract is recorded separately
  as ADR 0088 before any W1 code.
- Human decisions that remain OPEN and are not touched by Stage 10:
  ADR 0009 hosting/AUP; HDR-J-6/J-7/J-8/J-9; HDR-M-1/M-2; HDR-SB-1;
  OB-1; vendor, legal and retail decisions (proposal §7).
