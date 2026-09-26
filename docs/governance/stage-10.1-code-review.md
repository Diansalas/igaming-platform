# Stage 10.1 — Independent Code Review (`code-reviewer`)

- **Scope:** `git diff 8561ac2..HEAD` at `250828b` (commits `e9e0ad8` PAY-REV-1 + SB-T1-XMIN; `250828b` PAY-WH-TENANT-1).
- **Baseline:** CLAUDE.md, `docs/plans/stage-10.1-planning-gate-proposal.md`, ADR 0090, `docs/plans/stage-10.1-planning/11-pay-wh-tenant-1-design.md` (binding rulings), QA plans `06` and `16`.
- **Date:** 2026-09-26. Review only; no code was changed and nothing was committed.
- **Local run (this review):** `go build ./...` and `go vet -tags integration ./internal/...` are clean. `go test -tags integration -count=1 -p 1` passed for `internal/payments`, `internal/ledger`, `internal/sportsbook`, `internal/httpserver` and `internal/db`. The new payments integration tests were confirmed to execute, not skip (`-v`). gofmt is clean on all changed files.
- **Note:** four untracked test files appeared in the working tree while this review was running:
  - `internal/payments/webhook_no_write_before_verification_integration_test.go` (T11a);
  - `internal/payments/webhook_replay_duplicate_integration_test.go` (T8);
  - `internal/httpserver/payment_webhook_auth_failure_logging_integration_test.go` (probably T12);
  - `internal/httpserver/payment_webhook_simulate_tenant_binding_test.go` (probably T13).

  They are **not** part of the reviewed HEAD and were not reviewed. Because `go test` compiles untracked files, some of them may have been included in the payments/httpserver runs above. F3 is judged against HEAD. Once these files land, they need their own review against F3's criteria, and the F1 probes must be added.

## Verdict

**NOT READY for sign-off. Rework is required on the P2 items below.**

The financial core is sound:
- the PAY-REV-1 lock and re-check;
- migration 0092;
- ledger constraint routing;
- tenant isolation;
- the tenant-bound signature closing S-6 for the mock.

No P0 or P1 defect was found. Four P2 findings block sign-off (F1–F4). F5 is a governance gate. It blocks closure of SB-T1-XMIN, but not the code.

Per role authority, F2 goes to `ledger-finance` and F1 to `security` for their domain rulings. This review does not adjudicate them.

---

## Blocking findings

### F1 (P2, blocks PAY-WH-TENANT-1): the uniform-401 contract is broken for two attacker-controlled inputs, and one of them returns an unauthenticated 500

ADR 0090 item 3 requires that cross-tenant callbacks "fail closed without leaking tenant/provider information". Design §3.2 says every pre-verification failure gets the identical 401.

**Non-JSON body → 500 before verification.**
- `internal/payments/mock.go:471`: `HandleCallback` runs `json.Unmarshal(req.Body, &generic)` before the HMAC check. This is needed for the key-material scan.
- On failure it returns a plain `fmt.Errorf("payments/mock: parse callback: …")`.
- `ReceiveCallback` (`orchestrator.go`, after steps (a)–(c)) wraps it as `payments: handle callback: …`. It is neither `CallbackAuthError` nor a mapped sentinel.
- So `deposit_handlers.go:403-405` returns **500** and runs `logger.Error("payment_webhook_failed", "error", err, …)`.

Failure scenario, using an unauthenticated POST with any 64-hex `X-Payments-Signature`, `X-Payments-Key-Id: mock-v1` and the body `x`:
- an active tenant with the provider configured → **500** plus an error-level log;
- an unknown slug, a suspended tenant or an unconfigured provider → **401**.

Consequences:
- The response reveals which (tenant, provider) pairs are live.
- Anyone can raise error-level log lines, which is an alert-fatigue and paging vector.
- The logged `err` text includes a byte of the body. Design §3.3 says never to log `err` text for unauthenticated input.

The same path gives **500** for a *verified* body missing `provider_reference` or carrying an unknown `event_type`. Yet the OpenAPI entry (`docs/api/openapi/platform-api.yaml`, the `"400"` response) documents "Malformed body (after signature verification)" as 400, and no code path returns that 400. The documentation and the handler disagree.

**Oversized body → 400 after the tenant lookup.** `deposit_handlers.go:287-316` does the slug lookup and active check (401) before the 1 MiB check (400):
- a >1 MiB body at an unknown or suspended slug → 401;
- the same body at an active slug → 400 "request body too large".

The comment at `:304-307` ("not an enumeration oracle") is incorrect. The design's accepted residual covers *timing* only, not a status-code difference.

**Ruling 5 deviation (same fix).** Ruling 5 says header format validation happens "before any tenant or database work". At `:318-328` it runs after `GetTenantBySlug` (a DB read) and after the body read. The comment reinterprets the ruling as "before any tenant-*scoped* DB work".

**Fix:**
1. Validate the headers and the body size before `GetTenantBySlug`.
2. In `HandleCallback`, map a pre-verification JSON parse failure to `ErrCallbackSignatureInvalid`, or to a new reason in the closed enum, so it becomes a uniform 401.
3. Map post-verification structural errors to 400, as the OpenAPI entry claims.
4. Add all three probes to T9.

### F2 (P2, blocks PAY-REV-1 §D.4; route to `ledger-finance`): the denial audit does not identify the deposit, so operations cannot reconcile

`orchestrator.go:1194-1204` (`RecordDepositReversalRejection`) records:
- `TargetType: "payment_webhook"`;
- `TargetID: providerID`;
- metadata `{provider_id}` only.

`ErrDepositAlreadyReversed` is returned bare at `orchestrator.go:1123`, so the HTTP layer has no deposit identity to pass in.

Plan §D.4 makes this audit mandatory because "a second PSP reversal can mean money really moved twice at the PSP (for example a refund plus a chargeback), so operations and PSP reconciliation must see it". QA plan 06 #3 requires "tenant/actor/entity populated". CLAUDE.md requires the entity, and the IP, on audit records.

Failure scenario:
1. The PSP refunds deposit D, then processes a chargeback on D.
2. The second reversal gets 409.
3. The audit row says only "provider `mock-payments` rejected a reversal".
4. Operations cannot find D, the original ledger transaction, or the rejected reversal reference, so the real double movement at the PSP cannot be reconciled.

The allow-list restriction applies to the **log alert**, not to the tenant-scoped, append-only audit row.

**Fix:**
- Return a typed error carrying `deposit_intent_id`, `original_ledger_transaction_id` and `reversal_provider_reference`.
- Record them, plus the client IP, in the audit.
- Assert them in `TestPaymentWebhookHandler_DepositReversalAlreadyReversed_Maps409`.

### F3 (P2, blocks PAY-WH-TENANT-1): tests required by the binding QA plan are missing at HEAD, while the registry claims completion

`docs/governance/task-registry.md:3809` labels PAY-WH-TENANT-1 **"IMPLEMENTED (MOCK resolver only)"** and states "T1-T15 implemented". At `250828b` that is not true:

| Test | State at HEAD |
|---|---|
| **T11a** (statement-capturing: no INSERT/UPDATE/`FOR UPDATE` before verification) | Absent. It exists only as an untracked, unreviewed file. Without it, T2/T3/T14's "no rows" checks are weak: every rejected call runs in one `WithTenant` transaction that rolls back, so they would pass even if writes or locks happened *before* verification. T11a is the only test that proves invariant I1. |
| **T12** (log allow-list per reason; no `audit_log` row for any 401) | Absent. No test references `payment_webhook_auth_failed`, `logCallbackAuthFailure` or `callbackAuthFailureAllowlistFields`. |
| **T13** (the simulate route signs with `tc.TenantID`; the response exposes no body or signature) | Absent. The header of `internal/httpserver/payment_webhook_tenant_binding_test.go:5` claims T13, but the file contains no such test. |
| **T8a/b/c** (named replay and duplicate tests) | Absent at HEAD. The untracked file is in flight. Existing F-7 and stage-9 tests partly cover these cases. |
| **Test #10 alert-field restriction** | Not asserted. `payrev1_webhook_integration_test.go` checks the status, body and audit, but never the fields of the `payment_webhook_integrity_alert_deposit_already_reversed` log line. |

Also, design §0 says "a post-implementation `security` diff review is mandatory before anything is labelled IMPLEMENTED". Using the label before that review conflicts with the design and with CLAUDE.md's "no fake completion".

**Fix:**
- Land and review T11a, T8, T12 and T13, plus the test #10 log assertion.
- Relabel PAY-WH-TENANT-1 as **PARTIALLY IMPLEMENTED** until the tests exist and security has signed off.

### F4 (P2, blocks SB-T1-XMIN closure): T-1's new `pg_xact_status` branch has no test that fails when it is broken, and the recorded mutation-kill pair is wrong

`migrations/0093_…up.sql:285-287` rejects if `xact_full < xact_ref`, or if `pg_xact_status(...) IS DISTINCT FROM 'in progress'`.

**The mutation-kill pair does not work.** `docs/governance/stage-10-w1-mutation-and-sql-branch-coverage.md:267` claims `_SavepointRollbackIsAccepted` plus `_RejectsEarlierTransactionRollback` detect mutating the status predicate to `<>` or to a hardcoded accept. They do not:
- `_RejectsEarlierTransactionRollback`'s row has an xid below the current top-level xid, so it is rejected by `xact_full < xact_ref` and the status call is never evaluated.
- Replacing the status predicate with `false` (always accept) leaves both tests green.

**The one case only the status check catches is untested:**
1. A transaction that started *after* the current one inserts a rollback row for the same bet and commits.
2. That row is visible under READ COMMITTED, with an xid ≥ ours and status `'committed'`.
3. A void in our transaction then cites it.

This is precisely the cross-transaction citation T-1 exists to stop. No test reaches it.

**`TestSBT1XMIN_ReconstructionGuard_NullAndErrorCasesRejectClosed` is weaker than it claims:**
- `internal/sportsbook/settlement_migration_0093_integration_test.go:348` tests a hand-copied **replica** of the expression, not the migrated trigger, so drift between the two goes undetected.
- Its "NULL-status" probe (`:412`, raw=3) is rejected by `reconstructed < ref` before `pg_xact_status` is ever called, so G1 is never exercised.
- In fact G1's NULL path is unreachable in the real logic: any xid ≥ the current xid is never clog-truncated. `xact_full IS NULL` at `:285` is also dead, because `INTO STRICT` cannot yield NULL.

**Fix:**
- Add a trigger-level test for the "later-started, committed rollback row" case. Two connections are enough: open T1, start and commit T2's rollback, then T1 inserts a void citing it, and the insert must be rejected.
- Correct the coverage-doc row. State that G1 is defensive and unreachable, rather than tested.

### F5 (P2 governance, blocks SB-T1-XMIN closure): the R-2 epoch anchor was changed without a recorded re-ruling

0093 anchors reconstruction to the high bits of `pg_current_xact_id()` rather than to `pg_snapshot_xmax(pg_current_snapshot())`, as ruling R-2 and ADR 0090 item 2 require. The deviation is disclosed in two places:
- inline in 0093;
- in the ADR 0088 follow-up note.

**The technical reason is correct.** It was reproduced independently in this review on the local Postgres:

```
BEGIN; INSERT…; SAVEPOINT a; INSERT…; RELEASE a;
xmin(savepoint row)=2329462  pg_current_xact_id()=2329461  pg_snapshot_xmax=2329461
```

A released-savepoint row's xmin exceeds the snapshot xmax. So R-2's "largest candidate ≤ xmax" would reconstruct it one epoch too low and reject exactly the case the fix exists for.

The code says itself that the deviation "needs architect/ledger-finance re-review before this migration is treated as final". That re-ruling is not recorded. ADR 0090 item 2 still states the R-2 construction.

**Fix:**
- Record the architect and `ledger-finance` ruling.
- Amend ADR 0090 item 2.

**Also correct the claim in 0093's comment** (`up.sql:251`) that the wraparound-alias case "fails CLOSED". That holds only if the aliased low bits exceed the next xid. If they fall in `[ref, nextXid)`, the reconstruction names a real concurrent transaction and is accepted when that transaction is in progress. The case is theoretical: it needs a bet with a rollback more than 2^32 transactions old, and R-2 has the same alias class. Adding `AND cause.created_at = now()` (ledger-finance P3, plan §P) closes it cheaply, because `now()` is the transaction start time and is shared by all savepoints.

---

## Non-blocking findings (P3)

**P3-1. `MultiWebhookCredentialResolver` brings back the per-provider map that ruling C2 removed.**
- Where: `internal/payments/webhook_auth.go:46`, `cmd/platform-api/main.go:151`.
- The orchestrator holds one interface, so the letter of C2 is met. But production wiring is again `map[providerID]Resolver`, which C2 (paper 15 R2) rejected. Paper 15 says the MOCK resolver "returns `credential_unavailable` for any provider other than its own", and `MockWebhookCredentials.Resolve` already does that.
- With one adapter, `main.go` can pass `payments.NewMockWebhookCredentials(mockPaymentsProvider)` directly. The composite exists only for multi-mock test fixtures and could move to `_test.go`.
- `mock.go:115` still describes "the Orchestrator's single injected WebhookCredentialResolver map (keyed by provider_id…)", which contradicts C2.

**P3-2. Test #1 does not race two reversals.**
- Where: `internal/payments/payrev1_concurrency_integration_test.go:84`.
- `TestPayRev1_ConcurrentDistinctReferenceReversals_ExactlyOnePosts` uses an external `FOR UPDATE` blocker and **one** reversal, so "exactly one posts" is trivially true.
- It correctly proves the lock *placement*: it blocks at S2 with no projection lock held, and it failed genuinely before the fix at the ledger INSERT.
- The race and the loser's `ErrDepositAlreadyReversed` are proven by `TestPayRev1_DefectRepro_DistinctReferenceRaceNeverDoublePosts`. That test's pre-fix evidence is genuine: it shows 2 reversals posted, captured at `50d94af` without 0092.
- Together the two tests satisfy plan #1. The name of #1 is misleading. Rename it, for example `…_QueuesAtS2BeforeProjectionLocks`, or cross-reference the two tests.

**P3-3. The six-point "no financial effect" check is partial.**
- Where: `webhook_tenant_binding_integration_test.go:43-50`.
- `assertNoFinancialEffect` skips points 5 (debit = credit) and 6 (projection). Its comment says the caller asserts the projection, but T2, T3 and T14 never call `cashBalance`.
- QA §2 says "All six must be asserted together".

**P3-4. T9 is not byte-identical.**
- Where: `payment_webhook_tenant_binding_test.go` (T9).
- It decodes into `apierror.Error` and compares `code|message`, which ignores extra JSON fields and headers. The plan asks for byte-identical bodies (minus request_id) and identical headers.

**P3-5. T15's redaction assertion can never fail.**
- Where: `internal/payments/mock_test.go:368-374`.
- It checks that `fmt.Sprintf("%+v", cred)` does not contain `string(cred.Secret)`. Without the `String()` method, `%+v` renders `Secret:[12 34 …]` as decimal bytes, which never contains the raw 32-byte string, so the test would still pass.
- `LogValue()` is untested.
- Assert on `%x` or hex, and on the decimal-slice rendering, and add a `slog` handler check.

**P3-6. The C4 conformance case is effectively mock-only.**
- Where: `internal/payments/conformance_test.go`.
- The new case is `t.Skip`'d for any non-mock adapter, so no future real adapter is forced through it.
- The "no resolver ⇒ fail closed" and "rejected callback leaves no rows" conformance cases (paper 15, C4) are not in the suite.

**P3-7. The OpenAPI contract test is structural only.**
- Where: `openapi_paymentswebhook_contract_test.go`.
- It is a string search, and this is disclosed in the file and the registry. The QA §4 live-request conformance check is absent, and the gap is not recorded in `docs/testing/testing-strategy.md` as QA §4 requires.
- The test would not have caught the F1 mismatch (documented 400 vs. actual 500).

**P3-8. The error-mapper call is duplicated.**
- Where: `deposit_handlers.go:337-402`, `payment_deposit_simulation_handlers.go:222-254`.
- Each sentinel branch repeats `code, msg := mapReceiveCallbackError(...); apierror.Write(...)`. The public handler's final branch (`:403-405`) bypasses the mapper.
- Do the per-error side effects (log, audit) first, then make one mapper call. This removes about 30 lines and the risk of the two routes drifting apart.

**P3-9. Registry staleness.**
- Where: `task-registry.md:3803`, `:3807-3808`.
- The text still says "stage definition ADR 0090 (PROPOSED)" and "Nothing below is started".
- The blockers for PAY-REV-1 and SB-T1-XMIN still read "human approval".

**P3-10. The key-material alert was downgraded.**
- Where: `orchestrator.go`, the `ReasonKeyMaterial` branch.
- It went from `logger.Error("payment_webhook_rejected_key_material")` to the shared `Warn`-level `payment_webhook_auth_failed` line.
- The code argues the Warn line *is* the ADR 0022 §4.1 alert (C5). Whether a Warn meets the alerting requirement is for `security` to confirm.

**P3-11. Cosmetic issues.**
- `mock.go` `HandleCallback`'s doc comment list was broken by gofmt: step "2." is split mid-sentence ("point\n 2. - still BEFORE…").
- The S-6 evidence file `evidence/pay-wh-tenant-1-cross-tenant-prefix.txt` lacks the command and commit provenance header the other three evidence files carry. Its source test was retired per ruling 9, so the header is the only provenance.

---

## Items verified as correct (no finding)

**Migration 0092:**
- The refusal is built from the index build inside `DO … EXCEPTION WHEN unique_violation`. There is no `SELECT` pre-check, no `CONCURRENTLY` and no `row_security`.
- The index is tenant-leading and scoped to `deposit_reversal`.
- The refusal test seeds duplicates in two tenants on a scratch database.
- The runtime, owner and test-admin roles are all `NOSUPERUSER NOBYPASSRLS`, so FORCE RLS applies to the owner and the refusal is proven RLS-proof.
- The down migration drops only the index.

**Migration 0093 byte-identity:**
- The function body was extracted from 0091 (`up.sql:142-261`) and from 0093's up and down.
- Down vs. 0091 is **byte-identical**.
- Up vs. 0091 differs **only** in the DECLARE block (`cause_xmin` → `cause_xmin_raw`/`xact_ref`/`xact_full`) and in the composed-void causation branch. Every other branch is byte-identical.
- The function is still SECURITY INVOKER, and there are no grant or trigger changes.
- `TestMigration0093_UpChangesOnlyFunctionBody` catalog-snapshots columns, constraints, indexes, triggers, policies and RLS flags.

**`db.IdempotentInsert` signature change:** all five call sites are updated and compile:
- `ledger.go:380` uses the constraint name;
- `withdrawal.go:306`, `bonus/lifecycle.go:206`, `sportsbook/bets.go:100` and `payments/orchestrator.go:464` discard it correctly.

The other unique constraints on `ledger_transactions` keep their earlier behaviour. Routing is by exact constraint name.

**PAY-REV-1 ordering:**
- Verification finishes before any intent read or lock.
- S2 is `FOR UPDATE` with a tenant predicate. A missing row or a wrong type returns `ErrDepositReversalIntegrity`, never a tombstone.
- The S4 re-check runs after the lock, and the READ COMMITTED dependency is commented.
- The backstop `ErrReversalAlreadyExists` maps to the same sentinel.
- The denial audit is committed in a separate `WithTenant`, and a fresh transaction asserts it.
- The ADR 0082 class is L2, and no L0 or L1 lock appears on the path.

**PAY-WH-TENANT-1:**
- `signing_input` framing matches the design.
- The tenant and provider come from the route and are never read from the body.
- There is exactly one `Resolve` call, with an equality re-check. A nil resolver fails closed.
- Comparison uses `hmac.Equal` over 64-hex.
- The legacy body `signature` field is rejected after verification.
- `ProviderAcceptsWebhook` is a read-only `EXISTS` and ignores `status` (I4). T11b proves it is RLS-scoped.
- T5 (a shared secret still binds the tenant) is a meaningful test.
- There is no `MerchantAccountID` (C3), no config, no migration and no hard-coded secret.

**Error mapping:**
- 401 for all `CallbackAuthError` on the public route, and 503 on the simulate route.
- 404 only for the post-verification `ErrDepositIntentNotFound`.
- 409 for F-7 mismatch and for already-reversed.
- 500 otherwise. The exception is F1.

**Pre-fix evidence** exists and demonstrates the actual defects:
- PAY-REV-1: a double post, and blocking at the ledger INSERT rather than at S2;
- SB-T1-XMIN: both the service-level and the raw-SQL savepoint tests rejected at tip 91;
- S-6: a tombstone written in tenant B.

---

## Sign-off summary

| Workstream | Blocking | Owner to route to |
|---|---|---|
| PAY-REV-1 | F2 (audit entity) | payments; `ledger-finance` rules on audit content |
| SB-T1-XMIN | F4 (status branch untested, false kill-pair claim), F5 (R-2 re-ruling not recorded) | sportsbook; architect + `ledger-finance` |
| PAY-WH-TENANT-1 | F1 (401 contract / 500 on unauthenticated input), F3 (missing T11a/T8/T12/T13, #10 log assertion; premature IMPLEMENTED label) | payments/backend; `security` (mandatory post-implementation review still outstanding) |

P3 items can be fixed alongside or recorded as follow-ups. None of them alone blocks sign-off.
