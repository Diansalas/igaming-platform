# Stage 10 Completion Report — CI Evidence Restoration + Sportsbook Settlement Lifecycle

- **Date:** 2026-09-25
- **Approval:** ADR 0087 (human approval against planning-gate commit `2355ab7`)
- **Contract:** ADR 0088 (ACCEPTED; §13 amendments applied)
- **Branch:** `claude/focused-wright-jw88w9`
- **Status:** Stage 10 **COMPLETE**. All seven gates passed; one validation item is isolated and BLOCKED on an external credential (item 9). The Orchestrator **stops here**: Stage 11 is not started without explicit human authorization.
- *Status note 2026-09-26: the next stage was named Stage 10.1, not "Stage 11" (stage definition `docs/decisions/0090-stage-10-1-definition-payment-reversal-and-settlement-trigger-remediation.md`; `docs/governance/stage-10.1-completion-report.md`). The master stage map naming is registered as STAGE-NAMING-1 in `docs/governance/task-registry.md` "Records hygiene 2026-09-26".*

Labels follow CLAUDE.md "No fake completion". Sportsbook settlement is **IN-HOUSE MOCK MODE**: the only driver is a non-production, test-support staff route. It is **not** a real sportsbook provider integration. Real provider settlement and statement matching are **PROVIDER DEPENDENT**.

## Gate summary

| # | Gate | Result |
|---|---|---|
| 1 | W0 implementation complete | PASSED |
| 2 | Five consecutive green CI runs | PASSED (#239–#243) |
| 3 | W1 implementation complete | PASSED |
| 4 | Full Stage 10 acceptance suite passes | PASSED (CI #251–#259 green; local CI replay 3/3) |
| 5 | Specialist, code and security review complete | PASSED (all findings resolved or recorded as deferred) |
| 6 | Documentation complete | PASSED |
| 7 | Clean git tree and pushed commit | PASSED (verified at report time; see item 13) |

## 1. W0 result — IMPLEMENTED

The CI Go gate had never executed on this branch (finding F-1: the lint action repository did not exist). W0 restored it:
- **Lint:** `golangci/golangci-lint-action@v9`, with golangci-lint pinned at v2.5.0.
- **Scratch databases:** a CI/dev-only `igaming_test_admin` role (CREATEDB, NOSUPERUSER, NOBYPASSRLS) plus `internal/testsupport/scratchdb`. Application roles are unchanged and were never granted CREATEDB.
- **Flake fix:** lock-wait polling is now scoped to each test's own blocker PID.
- **Lint findings:** 19 fixed.
- **Evidence step:** a new "Assert integration evidence" step fails the job on any database-URL skip.

Detail: `docs/governance/task-registry.md` "Stage 10 / W0".

## 2. Five-CI-run evidence — PASSED

`build-test-lint` was green on five consecutive runs:

| Run | Run id | Commit | Event |
|---|---|---|---|
| #239 | 36142847281 | `77a9293` | push |
| #240 | 36142996408 | `012d99f` | push |
| #241 | 36143224956 | `6c9b9f3` | push |
| #242 | 36143564397 | `6c9b9f3` | workflow_dispatch |
| #243 | 36143929805 | `6c9b9f3` | workflow_dispatch |

Each run: 32 packages ok, 2,202 tests passed, 5 expected skips (HDR-J-7 rung-2 placeholders), 0 failed, reversibility passed. Local runs were not counted.

**W1 CI:** every W1 commit was green.

| Run | Commit | Contents |
|---|---|---|
| #247 | `eb3912f` | core |
| #251 | `0d943fd` | record amendments |
| #252 | `36616f1` | F-7 fix |
| #253 | `f72d864` | route and reconciliation |
| #257 | `a318c10` | review-gap tests |
| #258 | `dac94be` | review fixes |
| #259 | `312db16` | mutation and SQL-branch tests |

Run #259 (id 36162444009, job 108162042886) passed all 20 `build-test-lint` steps, including the new runtime-role narrowing step, "Assert integration evidence" and reversibility. The frontend, frontend-image and infrastructure jobs were also green.

## 3. W1 result — IMPLEMENTED (in-house MOCK mode)

Cash-funded single-bet settlement runs on the canonical ledger. By file:
- **`migrations/0091`:**
  - three new transaction types;
  - the append-only `sportsbook_bet_settlements` history table: FORCE RLS, split select/insert policies, deny triggers, T-1 validation;
  - T-2, a status-transition trigger;
  - sportsbook reconciliation mismatch kinds;
  - a guarded runtime-role REVOKE;
  - a down migration that refuses once any settlement evidence exists.
- **`internal/ledger`:** the transaction-type constants and `LockProjectionsForPostings`.
- **`internal/sportsbook/settlement.go`:** `SimulateSettlementEvent`.
- **`internal/reconciliation`:** a `sportsbook_settlement` stream with a MOCK statement source.
- **HTTP:** the test-support route `POST /v1/admin/sportsbook/bets/{id}/simulate-settlement-event`.
- **Permission:** `sportsbook_settlement:simulate`, granted only to `risk_manager`.
- **Read surfaces:** player and admin APIs, b2c and back office. They are read-only; there are no settlement controls.
- **OpenAPI:** the route is marked `x-test-support`.

Out of scope and not built:
- real provider settlement and webhooks;
- cashout;
- bonus-funded or mixed-funded settlement;
- operator manual settlement;
- the self-exclusion auto-void consumer.

## 4. Settlement scenarios tested

| Scenario | Tests |
|---|---|
| Settle won | `TestSettlementScenario_SettleWon` |
| Settle lost | `TestSettlementScenario_SettleLost` |
| Void before settlement | `TestSettlementScenario_VoidBeforeSettlement` |
| Void after settlement (won/lost) | `TestSettlementScenario_VoidAfterSettlement_Won/_Lost` |
| Rollback (won/lost) | `TestSettlementScenario_Rollback_Won/_Lost` |
| Re-settlement | `TestSettlementScenario_Resettlement` |
| Rollback then void | `TestSettlementScenario_RollbackThenVoid` |
| Tombstone (rollback of a never-seen settlement) | `TestSettlementScenario_TombstoneNeverSeen` |

Also covered:
- **Decision tables (every row of ADR 0088 §4.3):** `TestSettlementDecision_*`.
  - replay;
  - payload mismatch;
  - tombstone, then a late settle, then the next generation;
  - a delayed settle(1);
  - rollback redelivery after a composed void;
  - generation gap and stale generation;
  - `BET_VOIDED`, `BET_ALREADY_SETTLED`;
  - unknown and cross-tenant bets;
  - payout validation V-1 to V-4.
- **Field matrix:** `TestSettlementValidation_FieldMatrix`.
- **OB-1:** `TestOB1_RollbackOfWonSettlementCanDriveCashNegative`.
- **Concurrency (deterministic, blocker-PID scoped):** `TestSettlementConcurrency_*`, including a lost void-after-settlement racing `PlaceBet` on the same wallet.
- **Fault injection:** `TestSettlementFaultInjection_*`.
- **Database level:** `TestDBConstraints_*` covers T-1, T-2, the deny triggers and RLS. `TestMigration0091_*` covers the down-migration refusals with FORCE RLS active.
- **SQL branches:** 24 tests in `settlement_sql_branches_integration_test.go`.
- **HTTP:** `TestSettlementSimulate_*`.
- **Risk netting:** `TestSportsbookCumulative_EveryADR0088NettingRow`.
- **Reconciliation:** `TestSportsbookSettlement*` for drift injection per mismatch kind and the divergent MOCK statement.

## 5. Ledger and financial invariants verified

| Invariant | How it holds |
|---|---|
| Double entry | Every posting balances per asset (deferred balance trigger, forced immediate). |
| Append-only | Deny triggers on the history table; ledger unchanged. |
| No balance UPDATE | Balances are projections only. |
| No floats | Minor-unit `int64` postings; NUMERIC sums read into `big.Int`. |
| Payout never taken from the request | INV-SB-SETTLE-1: payout = frozen `potential_return`; enforced in Go and again by T-1. |
| Stake and accounts derived from the placement posting | Checked with NetLocked (ADR 0088 §2.2). |
| Idempotency | Enforced by the database: `UNIQUE (tenant_id, idempotency_key)` with server-composed keys, plus partial unique indexes for state. The races proving the indexes back-stop T-1 are tested. |
| Lock order L1→L2→L3→L4→E-4 | INV-LOCK-E4, backed by a static sole-writer test. |
| `LockProjectionsForPostings` | Pre-locks the union of all postings (R3). |
| Status is a derived cache | INV-SB-SETTLE-3 (T-2). |
| Cumulative limits (INV-SB-CUM-1) | `ReversalTypes = ["sportsbook_void"]`; every ADR 0088 §6.2 row pinned. |
| Reconciliation | Zero tolerance across four checks: locked vs open stakes, per-bet end-state nets, two-way orphans with causation and status, MOCK statement match. |

Ledger-finance signed off: `docs/governance/stage-10-w1-ledger-finance-signoff.md`.

**Mutation pass (gremlins):**
- `internal/sportsbook`: 466 mutants, efficacy 92.44%.
- `internal/ledger`: 129 mutants, efficacy 92.00%.
- In the ADR 0088 §14 scope, every surviving mutant was either killed by a new test or justified as equivalent.
- The handler part is PARTIALLY IMPLEMENTED: the tool cannot scope mutation to one file in a package that large.

Detail: `docs/governance/stage-10-w1-mutation-and-sql-branch-coverage.md`.

## 6. F-7 impact and result — IMPLEMENTED (own item, ADR 0088 §11.2)

**Audit:** `docs/governance/stage-10-f7-ledger-replay-audit.md`, against `94bc863`. It covers 21 call sites plus the W1 site. The result was class C: `ledger.Post` silently returned the original transaction for a same-key replay with a different payload. This was reachable at:
- casino win;
- casino rollback;
- payments deposit reversal;
- casino bet, at caller level.

**Remediation (`36616f1`, ADR 0020 amendment 2026-09-25):** the new `ErrIdempotencyPayloadMismatch`.
- **What it compares:** the final entry multiset including bonus-mirror legs, the reversal link, the provider reference, the reason code, causation, and correlation (tombstones are exempt from the correlation check).
- **Unchanged:** `ErrIdempotencyKeyReused` for a type mismatch.
- **Caller changes:**
  - `PlaceBet` resolves a legitimate concurrent duplicate before calling Post;
  - casino and payments map the mismatch to integrity errors with HTTP 409 and integrity alerts;
  - a same-reference deposit-reversal redelivery is now idempotent.
- **Tests:** ledger regression tests L1–L11, plus a replay regression test per caller proving legitimate retries still match.

This is a deliberate, recorded semantic change, not a silent one. Ledger-finance and the code review approved it.

## 7. Security and RLS result — APPROVED WITH FINDINGS (no P0/P1)

`docs/governance/stage-10-w1-security-review.md`. All ten security findings from the ADR 0088 review (S1–S10) are implemented:
- **Route gate:** the route is registered only when test-support routes are enabled, never with `APP_ENV=production`; otherwise it returns 404.
- **Auth chain:** RequireTenantScope, then RequireStaffPrincipal, then the permission, whose sole grantee is `risk_manager`. The actor is re-checked in the database as an active `risk_manager` in the same tenant.
- **Tenant isolation:** the bet is resolved under RLS; a cross-tenant id returns 404 and writes nothing to the owning tenant.
- **Audit:** complete, with the IP from `trustedProxyClientIP` and no raw `X-Forwarded-For`.
- **Alerts:** fields are allow-listed.
- **Player read surface:** omits the staff and request ids.
- **Triggers:** SECURITY INVOKER, and they RAISE when the bet is not visible or the connection is player-scoped.
- **Runtime role:** loses UPDATE, DELETE and TRUNCATE on the history table in the migration, `init-app-role.sql` and CI.

Both P2 findings (alert name `bet_not_found`, alert and audit tests) and P3-1 to P3-7 were fixed in `dac94be` and `a318c10`.

## 8. Specialist review result

| Reviewer | Verdict | Disposition |
|---|---|---|
| security | APPROVE WITH FINDINGS | P2 ×2 and P3-1…7 fixed; P3-8/9 informational |
| code-reviewer | APPROVE WITH CONDITIONS (B-1..B-3) | B-1 tests plus the SB-T1-XMIN precondition recorded; B-2 mutation pass and SQL checklist; B-3 rejection-audit tests; P3s fixed |
| ledger-finance | APPROVED WITH FINDINGS (P3 only) | P3s fixed or documented; PAY-REV-1 confirmed P1 (out of scope, item 9) |
| qa | Acceptance suite, review-gap tests, mutation/SQL checklist | Found and fixed one real cross-package defect (payload-mismatch → `ErrSettlementIntegrity`) |
| risk | §6.2 netting and OI-5 pin | Every row matches the ADR |
| architect | ADR 0088 §13 and ADR 0082 A4 applied | Deviations recorded in the amendments |
| devops | Runtime-role narrowing | Done |

## 9. Remaining blockers

1. **`b22d5c4` live IAM re-validation — BLOCKED (isolated external dependency).**
   - What is blocked: the live Access Analyzer `ValidatePolicy` check and the 30-case policy simulation.
   - Why: the deployer credential is denied `access-analyzer:ValidatePolicy` and `iam:SimulateCustomPolicy`.
   - What unblocks it: a credential holding those two read-only actions.
   - Offline checks pass: 42 terraform tests and 7 node tests.
2. **PAY-REV-1 — P1, outside Stage 10 scope, not fixed here.** It predates Stage 10 (since Stage 3B).
   - The defect: concurrent deposit reversals of one deposit under different references each post. The original deposit is not locked and nothing enforces one reversal per deposit.
   - Why reconciliation misses it: the ledger still balances.
   - Reachability: only through a signature-verified PSP webhook, and only mock PSPs exist.
   - Deadline: it must be fixed before any real PSP or production launch.
   - Proposed fix: a `FOR UPDATE` lock on the original deposit, a partial unique index on `reverses_transaction_id`, and a regression test.
   - Owner: payments. Reviews: ledger-finance and architect.

## 10. Remaining human decisions (OPEN, not reopened)

- ADR 0009 hosting/AUP
- HDR-J-6, HDR-J-7, HDR-J-8, HDR-J-9
- HDR-M-1, HDR-M-2
- HDR-SB-1
- **OB-1:** the receivable when rolling back a won settlement leaves `player_cash` negative. The posting is correct and implemented; the business treatment is OPEN.
- Vendor, legal and retail decisions

**New decision requested:** authorize a stage to fix PAY-REV-1 (item 15).

## 11. Deferred work (recorded, not built)

- **SB-T1-XMIN:** replace T-1's composed-void causation check with `pg_xact_status` in a new migration before any savepoint-using settlement driver. Today history inserts run outside savepoints; this precondition is documented and pinned by tests.
- **Named debt R-3 / OI-5:** cumulative limits measure net outflow by posting time. This becomes P1 if limits must be gross-by-placement.
- **Rollback re-opening residual:** no L0.6 lock. Must be revisited before any exposure limit is armed (HDR-SB-1).
- **Money width:** crypto-precision sportsbook needs `NUMERIC(38,0)` / `big.Int` postings.
- **Reconciliation scale:** each run recomputes the whole population. Also, the unbounded platform-wide catalogue read gets slow on heavily seeded databases (observed locally; fine on fresh CI databases).
- **Test-support route removal:** remove it when a real sportsbook settlement provider is registered (ADR 0048 pattern).
- **Reserved-prefix guard:** add a guard on provider ids for the reserved `sportsbook_` key prefixes in the real-provider stage.
- **Accepted mutation gap:** the `pgx.Batch.Close()` error branch needs a fault seam.
- **Handler mutation scope:** PARTIALLY IMPLEMENTED (tool limitation).
- **Carried from W0:**
  - the ALB security-group description, which would force replacement in staging;
  - 467 lint findings in integration-tagged files;
  - three statement-text lock-wait helpers.
- **Record tidy-ups:** stray `player_locked` wording in ADR 0038 sections outside §13 and ADR 0083 §7.3 "safe today" wording; the ADR 0082 P3-3 arming note was never applied.

## 12. Final commit SHA

- **W1 code head:** `312db16` (CI #259 green).
- **Records commit:** this report and the record updates are in the commit immediately after `312db16`. Its SHA is given in the Orchestrator's hand-off message and can be verified with `git log origin/claude/focused-wright-jw88w9 -1`.

## 13. GitHub push verification

- Every Stage 10 W1 commit is pushed to `origin/claude/focused-wright-jw88w9`:
  - `eb3912f`, `d26b3b9`, `49d6fda`, `38e4875`, `0d943fd`, `36616f1`, `f72d864`;
  - `6bffd3f`, `3c50f50`, `329897a`, `a318c10`, `dac94be`, `312db16`;
  - and the records commit.
- Local and remote heads are compared after the final push (hand-off message).
- CI results are read from the GitHub Actions API for this branch.

## 14. AWS staging untouched

Confirmed:
- No `deploy.sh`, terraform apply or AWS API call was made in Stage 10 W1.
- No IAM, CIDR or Dockerfile change.
- No credentials were written to the repository.
- Staging stays running on deployed commit `9190d5d`, unmodified.
- Migration 0091 has not been applied to staging, and does not need to be for Stage 10.

## 15. Next recommended stage

**Stage 10.1 — PAY-REV-1 remediation plus Stage 10 residual hardening.** This is a small, bounded, financial-correctness stage. Scope:
1. Fix PAY-REV-1: an L2 lock on the original deposit, a partial unique index on `reverses_transaction_id` (refusing if duplicates exist), and a regression test. Reviews: ledger-finance and architect.
2. The SB-T1-XMIN migration.
3. Optionally, lint coverage for integration-tagged test files.

It needs a planning gate and explicit human authorization. **Not started.** A real-provider sportsbook stage or a staging redeploy with migration 0091 each need their own human-authorized plan.
