# RV-FH3 — Security review: FH-3 / PAY-DOUBLE-CREDIT-1 (ADR 0095 §28, INV-DEP-1)

- Reviewer: `security` specialist
- Date: 2026-09-27
- Subject: branch `worktree-agent-adce273a3a5339f77` @ `a927aed` (FH-3 commit `8ce538c` plus follow-ups `109ef04`…`a927aed`). Not merged into `claude/focused-wright-jw88w9`: `8ce538c` is not an ancestor of main-branch HEAD `f0ff196`.
- Spec: ADR 0095 §28 (at `a927aed`).
- In scope:
  - the INV-DEP-1 choke point (`resolvedForOtherDeposit`, `postDepositSuccessOrDispute`, `postDepositSuccess`) and every `TxDeposit` call site;
  - `ledger.ErrDepositAlreadyPostedForIntent`;
  - migration 0107 (up and down, `payment_attempts_guard()`);
  - the uniform-200 surfaces;
  - audit and P1 log content;
  - `testHookBeforeReferenceConflictRecheck`;
  - scenario L.
- Out of scope:
  - the financial correctness of INV-DEP-1, and reconciliation semantics (`ledger-finance`);
  - QA's A–O matrix adjudication;
  - penetration testing.

  This review does not declare FH-3 "secure" in general.
- Method:
  - detached worktree at `a927aed` and a private DB `secfh3_db` (via `priv_db.sh`, using the harness's configured credentials only: no `sudo`, no `ALTER ROLE`, no password changes; DB access worked throughout);
  - a HEAD guard probe (not committed);
  - 3 anchored security mutants (each reverted, tree confirmed clean);
  - suites on the private DB: the payments subset `INVDEP1|Migration0107|RVLF|Receipt|Callback|Deposit|T13|Reversal`; `internal/httpserver` `Webhook|Callback|Deposit|CrossTenant`; `internal/reconciliation`, `internal/ledger`, `internal/idempotency`. **All pass.**
  - Cleanup: the worktree has been removed and `secfh3_db` dropped (0 matching databases remain).

## Verdict

**CHANGES REQUIRED (narrow). FH-3 must not be marked complete until F-M1 and F-M2 are closed.**

The INV-DEP-1 money path itself holds from a security standpoint:
- every production deposit posting goes through the choke point;
- the ledger backstop index is enforced whatever the RLS scope;
- the uniform 200 discloses nothing;
- no amounts or references reach log lines;
- the test hook cannot be set outside the package;
- scenario L holds.

The blocking problem is migration 0107. Its `CREATE OR REPLACE` of `payment_attempts_guard()` **weakened a guard beyond the T13d reason** (F-M1). The tests of the replaced function still run against the 0101 body, which is why the regression went unnoticed (F-M2).

## Findings

### F-M1 — MEDIUM (blocks completion): 0107's guard accepts a deposit `declined → disputed` with a NULL `terminal_reason`

The T13t/T13d line was changed:
- from `NEW.terminal_reason IS DISTINCT FROM 'reversal_tombstone_precedes_success'`;
- to `NEW.terminal_reason NOT IN ('reversal_tombstone_precedes_success', 'multiple_success_for_intent')`.

For a NULL reason, the old test is TRUE (raise), but `NULL NOT IN (…)` is NULL, so the `IF` does not raise. No CHECK constraint requires a reason on `disputed`: `payment_attempts_terminal_reason_check` only bounds the length.

Probe (same fixture shape as `TestMigration0101_T13t_…`; runtime role; one declined deposit attempt per case):

| Case | At 0101 | At HEAD (0107) |
|---|---|---|
| `terminal_reason='some_other_reason'` | refused | refused |
| reason column untouched (NULL) | refused | **accepted → `disputed/<NULL>`** |
| `terminal_reason='multiple_success_for_intent'` | refused | accepted (intended) |

Impact: the trigger is the defence-in-depth layer for §28.4's forbidden list. A deposit attempt can now be parked `disputed` with no reason by any runtime-role writer: a future code path, a bug, or a mis-ordered `UPDATE`. Such a row:
- is invisible to `pay_captured_unposted`, which keys on the reason;
- is invisible to the M1-queue classification;
- carries no record of why it left `declined`.

This contradicts §28.8 item 3 ("Nothing else in the 0101 body changes") and §28.4 ("allowed only with `terminal_reason ∈ {…}`"). Application code always sets a reason today, so there is no direct money effect. Hence MEDIUM, not HIGH.

Required:
- Write the predicate NULL-safe, e.g. `AND (NEW.terminal_reason IS NULL OR NEW.terminal_reason NOT IN (…))`. 0107 is unmerged, so editing it in place is acceptable. If it has been applied to any shared environment by then, use a follow-up migration instead.
- Add the HEAD test described in F-M2 with a NULL case and an other-reason case.
- `0107….down.sql` restores 0101's code exactly (the diff is comments only), so down is not affected.

### F-M2 — MEDIUM (blocks completion): the behaviour tests of the replaced `payment_attempts_guard()` still run against 0101, not HEAD

The following all build their scratch database with `migration0101Scratch(t, …, 101)` and never migrate further:
- `TestMigration0101_InsertGuard_RejectsForbiddenShapes`;
- `TestMigration0101_UpdateGuard_TransitionWhitelist`;
- `TestMigration0101_T13t_TombstonePrecedesSuccess_RequiresNamedTerminalReason` (the only test of "another reason is refused").

They therefore exercise the superseded 0101 function body.

This breaks two rules:
- the project rule restated in §28.8 item 3 (`rv-prh-architect.md` §5: every behaviour test of `payment_attempts_guard` must run against a HEAD-migrated scratch DB in the same change; only 0101 up/down-history tests may stay pinned, and must say so);
- §28.12's "Trigger: a deposit `declined → disputed` with any other `terminal_reason` is refused (run against HEAD)".

`migration_0107_integration_test.go` has no raw-SQL trigger test. It is the same class of defect as kill-switch re-verification N1.

Required:
- Move these three tests to a HEAD-migrated scratch database: `migration0101Scratch` followed by `MigrateUp(realMigrationsDir)`, as `depositV2ScratchPool` already does.
- Add a HEAD trigger test with three cases:
  - (a) `multiple_success_for_intent` is accepted;
  - (b) another reason is refused;
  - (c) a NULL reason is refused. This case fails on today's 0107 (F-M1).
- Label any test that stays pinned to 0101.

### F-L1 — LOW (test gap; flagged to `qa`): the choke point's tenant binding and both application layers are unpinned

- **MT SURVIVED.** Calling `resolvedForOtherDeposit` with `uuid.Nil` as the tenant at both sites passes the subset.
- **MC SURVIVED.** Disabling **both** the rule-1.4 choke-point check and the rule-2 re-check passes the subset.

In both cases the migration-0107 ledger index refuses the second posting. The error maps to the same T10/T13d dispute, differing only in an extra `payments_deposit_intent_index_backstop_fired` log line that no test observes. Money stays safe because that unique index is enforced regardless of RLS. But the §28.12 "mutation kills: drop the choke-point check; drop the re-check inside `postDepositSuccess`" are **not achieved**, and a regression that silently reduced INV-DEP-1 to a single DB layer would go unnoticed.

Required before the stage gate: in the choke-point tests, assert that the backstop P1 did **not** fire (capture `slog`), or test `postDepositSuccess`'s re-check directly.

### F-L2 — LOW (test gap): the P1 alert log content (S-5) is unpinned

**ML SURVIVED:** adding `amount` to `payments_multiple_success_for_intent_alert` passes the subset.

No test captures these lines:
- `payments_multiple_success_for_intent_alert`;
- `payments_deposit_intent_index_backstop_fired`;
- `deposit.multiple_success_refused`.

The code is correct today (tenant, intent and attempt ids only). Add a `slog`-capturing test asserting the exact attribute set, with no amount or reference keys.

### F-L3 — LOW (hardening): `testHookBeforeReferenceConflictRecheck` is compiled into production

Ruling: **it cannot be set in production builds.**
- It is an unexported package-level `func()` var in `receipt.go`.
- Only code inside package `payments` can assign it, and `-ldflags -X` cannot set a func value.
- The only assignment (and `t.Cleanup` reset) is in `rvlf_i1_regression_integration_test.go` (an `_test.go` file).
- The production read is a nil-check no-op.

Residual: a future non-test file in `payments` could assign it. Add a static test (`go/ast` scan, as `no_provider_call_in_tx_closure_static_test.go` already does) that fails if any non-`_test.go` file assigns it. Alternatively, move the declaration behind a build-tagged pair of files.

### Informational (no action for FH-3 unless noted)

- **`RecordDepositMultipleSuccessRefusal` has no caller.**
  - Nothing ever writes `deposit.multiple_success_refused`, the legacy `InitiateDeposit` refusal audit required by §28.3 rule 3.
  - Acceptable only while `InitiateDeposit` has no production caller, which is true (doc comment and grep).
  - Wire it or delete it together with the legacy path (security P2-L2).
- **The RLS asymmetry does not open the predicate.**
  - Under a mixed tenant+platform GUC, `payment_attempts` is hidden (0106) but `ledger_transactions` is not.
  - Every resolved intent has a deposit posting, and T7 posts before `ApplySuccess`, so the ledger half still answers correctly.
  - Under a player-scoped or wrong-tenant session, every table is hidden, and the posting itself is refused by RLS `WITH CHECK`.
  - The predicate would fail open (empty = "not resolved") in such a scope, but no write can follow in that scope.
- **Scenario L.** A mis-bound callback stores a deferred receipt under the wrong tenant (A). It would replay only if a tenant-A attempt later acquired the same `(provider_id, provider_reference)`. That needs both a tenant mis-binding (outside the threat model: HTTP binding is by webhook credential, `TestPaymentWebhook_ForgedCrossTenantProviderReferenceDenied`) and a reference collision. No action.

## What holds (verified, not assumed)

| Question | Result | Evidence |
|---|---|---|
| Tenant binding of the choke-point reads | `resolvedForOtherDeposit` filters `tenant_id = $1` explicitly **and** runs under RLS (`FORCE ROW LEVEL SECURITY` on `payment_attempts`, `ledger_transactions`, `deposit_intents`; tenant-only policies require `app.player_account_id` to be NULL). The tenant passed in is `attempt.TenantID`/`intent.TenantID`, taken from rows read under the caller's own `WithTenant` scope, never from evidence. Every caller holds `deposit_intents … FOR UPDATE` (A7 order unchanged). The ledger index `(tenant_id, correlation_id) WHERE transaction_type='deposit'` is scope-independent. | Code; `pg_policies`/`pg_class`; MT/MC show the index backstop holds (F-L1 for the test gap). |
| Every production posting goes through the choke point | The only `TxDeposit` poster is `postDepositSuccess`. It is reached via `postDepositSuccessOrDispute` from `receipt.go` (`applyDepositSuccessAndPost`), `drive.go` (phase C) and `sweeper.go` (poll/T17), and directly only from legacy `resolveAmbiguous` (test-only), which keeps the internal re-check. `deposit.second_capture_posted` is deleted. | grep of `postDepositSuccess(`, `ledger.TxDeposit`. |
| The uniform 200 discloses nothing | The webhook answers `{request_id, received: true}` for every disposition; disposition goes to an Info log line only. Player `GET /v1/me/deposits/{id}` exposes the intent `status` only, which stays `succeeded` (sticky), never attempt state or `terminal_reason`. The mock simulate-callback route requires the intent to be awaiting callback (a resolved intent is refused before evidence) and returns intent status only. The removed `ErrDepositIntentNotFound` mapping only shrinks the surface. | `deposit_handlers.go` L578–593; `payment_deposit_simulation_handlers.go`; `payment_callback_errors.go` diff. |
| Audit and logs | `payment.attempt_disputed` metadata: reason, evidence kind, `provider_id`, `provider_reference` (already `providerref`-validated; the §28.11 list), intent id, existing ledger txn id. **No amounts.** Amounts appear only in `reconciliation_mismatches` rows (§28.9). The 3 new log lines carry ids only. No raw provider text (decline-reason handling is unchanged by FH-3). Error wrapping carries ids only (`correlation_id=%v`). | Code; diff grep of added log calls (3, ids only). Unpinned: F-L2. |
| Test hook | Cannot be set in production (F-L3 ruling). | grep; the Go language rules for unexported func vars. |
| Migration 0107 guard replacement | **Weakened for NULL (F-M1).** Otherwise only the T13d reason was added; the whitelist and every other branch are code-identical to 0101. `0107….down.sql` restores 0101's code exactly. The tests of the replaced function do not run at HEAD (F-M2). | Body diff 0101↔0107up↔0107down; HEAD probe. |
| Scenario L (cross-tenant) | `TestINVDEP1_L_CrossTenantCallback_NoEffectInEitherTenant`: tenant-B evidence applied under tenant A's scope resolves to nothing (`deferred_unresolved`); no balance, intent or attempt change in either tenant. It passes on the private DB. HTTP-level binding: `TestPaymentWebhook_ForgedCrossTenantProviderReferenceDenied` passes. | Suites above. |

## Conditions

**To mark FH-3 complete (security):**
- F-M1: a NULL-safe guard;
- F-M2: guard tests at HEAD, including the NULL, other-reason and T13d cases.

**Before the stage gate:** F-L1, F-L2 and F-L3.

**Tracked, not FH-3:** wire or delete `RecordDepositMultipleSuccessRefusal` together with the removal of the legacy path.

Launch authorization remains the human's decision.

---

# Re-verification 1 — FH-3b @ `cb03868` (commits `6fce7a6`, `a9ef2f9`, `135127a`, `cb03868`)

- Date: 2026-09-27.
- Scope of the change:
  - production code: `migrations/0107_…up.sql` only;
  - tests: `migration_0101_integration_test.go`, `migration_0107_integration_test.go`, `testhookrefconflict_static_test.go`;
  - no production Go changes.
- Method:
  - detached worktree at `cb03868` and a private DB `secfh3b_db` (via `priv_db.sh`, harness credentials only; no `sudo`, `ALTER ROLE` or password changes);
  - a code-only, line-by-line diff of the three `payment_attempts_guard()` definitions (comments stripped, whitespace normalized, each file's single `CREATE … FUNCTION` statement through its closing `$$ LANGUAGE …`);
  - a HEAD probe (not committed);
  - 7 anchored mutants against `INVDEP1|Migration0107|Migration0101|FL3` (plus `RVLF|Receipt|Deposit|T13` for the two single-layer mutants). Baseline green. Each mutant was reverted and the tree confirmed clean.
- Cleanup: the worktree has been removed and `secfh3b_db` dropped (0 remaining).

## Verdict

**APPROVE from `security` — F-M1, F-M2, F-L1, F-L2 and F-L3 are all closed.** One LOW residual (the MC2 test gap below) is handed to `qa`; it does not block.

## Results

| Item | Result | Evidence |
|---|---|---|
| **"The NULL-safe predicate is the ONLY functional difference"** | **Confirmed.** 0101 vs 0107 up differ in exactly three places: `CREATE` → `CREATE OR REPLACE`; the T13t/T13d predicate, from `IS DISTINCT FROM 'reversal_tombstone_precedes_success'` to `(NEW.terminal_reason IS NULL OR NEW.terminal_reason NOT IN ('reversal_tombstone_precedes_success','multiple_success_for_intent'))`; and the `RAISE` message text (now includes the offending reason, which is bounded to 64 bytes by the CHECK). Every other code line of the 125-line body is identical. 0107 **down** is code-identical to 0101 apart from `CREATE OR REPLACE`. 0107 creates or drops no trigger. | code-only diff |
| **F-M1** (NULL reason accepted) | **CLOSED** | HEAD probe, deposit `declined → disputed` with: column untouched (NULL), explicit NULL, `''`, `'some_other_reason'` → **all refused**; `reversal_tombstone_precedes_success` and `multiple_success_for_intent` → accepted. MF1 (NULL-safety reverted) is KILLED by `TestMigration0107_T13tT13d_TerminalReasonTrigger_HEAD`. |
| **F-M2** (guard tests at 0101) | **CLOSED** | `InsertGuard`, `UpdateGuard_TransitionWhitelist`, `T13t_…RequiresNamedTerminalReason` (and RLS) now use `depositV2ScratchPool` (0101 → HEAD). The new `TestMigration0107_T13tT13d_TerminalReasonTrigger_HEAD` covers T13d accepted, other reason refused, NULL refused. The tests left pinned (0101 up/down round trip, backfill and pre-flight) are migration-boundary tests, each labelled `PINNED TO 0101` with its reason, as the project rule allows. |
| **F-L1** (choke point vs DB backstop) | **CLOSED** for the mutants I raised | MT (wrong tenant at both sites) KILLED, MC (both application layers off) KILLED, MC1 (the rule-1.4 choke point alone off) KILLED, all by `TestINVDEP1_FL1_ApplicationChokePointCatchesItBeforeTheDBBackstop`, which asserts the backstop-fired P1 does not fire on a correctly-tenanted T13d. |
| **F-L2** (P1 log content) | **CLOSED** | ML (amount added to the alert line) KILLED by `TestINVDEP1_FL2_MultipleSuccessAlertLogContentIsPinned`. It requires `tenant_id`, `deposit_intent_id` and `attempt_id`, and forbids `amount=`, `asset_code=`, `provider_id=`, `provider_reference=` and the literal amount. |
| **F-L3** (static hook guard) | **CLOSED** | MF3 (a non-test file assigning the hook in `init`) KILLED by `TestFL3_…NeverAssignedOutsideTests`. The guard parses every non-`_test.go` file in the package, so build tags don't hide any. It fails if it scanned nothing or never saw the identifier. `TestFL3_GuardCatchesPlantedViolation` is its own self-test. |

## Residuals (LOW, non-blocking)

- **MC2 SURVIVED (to `qa`).** Disabling only the rule-2 re-check inside `postDepositSuccess` passes the subset. The choke point always catches the case first, and the only caller that skips the wrapper is the test-only legacy `resolveAmbiguous`. §28.12 lists "drop the re-check inside `postDepositSuccess`" as a required kill. A direct test would close it: call `postDepositSuccess` on a resolved intent and assert `ErrDepositIntentAlreadyResolved` with no ledger insert attempted. Money stays safe because of the RLS-independent ledger index.
- **FL2 uses a denylist.** An exact allow-list of the alert line's attribute keys would also catch a renamed key (e.g. `ref=`). Optional hardening.
- **F-L3 only matches direct identifier assignment.** A pointer-alias write (`p := &hook; *p = f`) would evade it. That is contrived, and it is confined to package `payments` code in any case. Accepted.

`RecordDepositMultipleSuccessRefusal` (informational in the original review) is unchanged and still tracked with the removal of the legacy path.
