# QA review — PRH-2 plan (2026-09-28)

Reviewer: `qa`, read-only. Plan at `a3547f5`. Read: the plan, CLAUDE.md, `docs/testing/testing-strategy.md` and `.github/workflows/ci.yml` (249-318). Spot-checked `launch_two_phase_integration_test.go:703-795` and the registry rows for CI-BILLING-1, F-POOL-1 and TEST-RESISO-RACE-1. No tests were run.

**Verdict: APPROVE WITH CONDITIONS.**
- Every workstream A–L has a concrete, falsifiable test list mapped to CLAUDE.md's financial matrix.
- Local and CI evidence are separated honestly.
- F1–F3 must be resolved before the affected workstreams start. F4–F6 can be resolved at those workstreams' own gates.

| ID | Severity | Plan section | Required change |
|---|---|---|---|
| F1 | Medium | §5-A | Name the existing characterization test `launch_two_phase_integration_test.go:703-795` (it asserts that a bet on a consumed session is still accepted). WS A's DoD must say to invert or replace it explicitly, so it is never discovered as a surprise failure or quietly skipped. |
| F2 | Medium | §5-H, §5-E1, §5-I | The sweeper interval, outbox backoff/`next_attempt_at` and alert-dispatcher backoff are wall-clock driven. Add a cross-cutting rule: tests drive time through an injectable clock, or write lease and next-attempt timestamps into fixtures directly. Never `time.Sleep` and hope (see TEST-RESISO-RACE-1 and CI-FLAKE-281). |
| F3 | Medium | §7 / §5-D, H, K | Any new test with a wall-clock assertion must be redesigned to assert on order or outcome, so it never needs the isolated timing lane. If a real wall-clock bound is unavoidable, escalate it as a CI-config decision before merge; never append it silently. |
| F4 | Low | §4 rows 0112/0114 | State FORCE RLS and the RLS family (tenant, validated platform GUC, or dual-scope) explicitly for the K2 and K3 tables, as the other rows do. |
| F5 | Low | §5-D | Add a reconciliation-impact test: the new poll dispute reasons are classified correctly and drift stays zero. |
| F6 | Low | §1 | QA verified only rows A and the CI block. `code-reviewer` should independently re-verify the full file:line evidence table before W0 sign-off. |
| F7 | Info | §9 | CI-BILLING-1, F-POOL-1 K1 and TEST-RESISO-RACE-1 are handled honestly (no workaround; local runs are never labelled CI). |

## Per-wave gate checklist (QA will enforce)

**W0 (docs):**
- ADRs 0099–0104 drafted and reviewed: security plus ledger-finance for 0099–0101; security for 0102–0104.
- `docs/HANDOVER.md` skeleton, links only.
- Registry rows added: SB-CATALOGUE-IO-1, HANDOVER-1, CAP-GRANT-1, ALERT-DELIVERY-1.
- F1–F3 resolved in the plan.
- HD-PRH2-1/2/3/6 answered, or deferred with the consequence noted.

**W1 (A, E2, F-kyc, G1, I-core, K1):**
- **A:**
  - the trigger matrix passes;
  - the characterization test is inverted, not left failing;
  - mutants killed: drop whole-row equality, allow `expired`, drop `NEW.status='revoked'`, restore to `active`;
  - the cross-tenant revoke test passes.
- **E2:** ledger-finance signs a mutant-parity report showing every INV-DEP-1, A7 and reconciliation mutant is still killed. The txscope static test passes.
- **F-kyc:** the lock-timeout fault-injection test passes; one decision row per evaluation; MB1, MB6 and MPLAYREC killed.
- **G1:** the cross-tenant audit-visibility test passes (A sees platform actions on A, never on B); the RLS-predicate mutant is killed; the down migration refuses.
- **I-core:** dedup, concurrency, failure and rollback tests pass, including "a rolled-back event raises no alert"; the F2 clock discipline is applied.
- **K1:** the full authz matrix passes (role × scope × capability); self-grant is refused; the distinct-Person trigger works; the sock-puppet case is refused; RLS dual-family tests pass.

**W2 (B, C+G2, J, K2):**
- **B:**
  - security sign-off is a hard gate;
  - two concurrent consumes produce exactly one, under `-race`;
  - cross-tenant, cross-provider, expired and revoked tokens are all refused;
  - the MOCK is exercised over HTTP, with no in-process shortcut.
- **C:** the adversarial references and the callback-versus-sync race pass; G2's non-kill-switch T2 test passes and mutant K1 is killed; A7 stays green.
- **J:** tests with a no-op meter and a failing exporter prove enforcement never depends on metrics.
- **K2:** F4 resolved; approver ≠ initiator and distinct-Person enforced; the idempotency-key UNIQUE constraint is tested; property test SUM(D) = SUM(C) holds over random adjustment sequences.

**W3 (D, F-pay, H, E1):**
- **D:** F5 resolved; race tests pass at `-count=50` under `-race`; F3 applied; all four comparison mutants killed.
- **H:**
  - the `-race` integration run is labelled local-only;
  - two concurrent loops never double-process;
  - per-tenant fault isolation holds;
  - shutdown drains cleanly.
- **E1:** a crash between phases A→B and B→C recovers; two workers with `SKIP LOCKED` never double-process; F2 is applied.

**W4 (I-wire, K3):**
- **I-wire:** the multiple-success alert is durable, in the same transaction as the refusal record; logs are retained.
- **K3:**
  - F4 resolved;
  - the guard-transition mutant is killed;
  - a failed execution leaves no partial rows;
  - ledger-finance and security both sign off.

**W5 (final gate):**
- The per-workstream DoD is actually filed, not just claimed.
- No test is skipped or quarantined without an orchestrator-recorded decision.
- Every test result is reported as PASS / FAIL / FLAKE / NOT RUN / BLOCKED.
- CI-BILLING-1 is stated plainly.
- Human sign-off is obtained before any production-facing step.
