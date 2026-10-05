# Security review: prh2-r3-closed-tenant-recon @ d1a41f0 (base 66d2968)

Reviewer: security specialist. Read-only review. Nothing was committed, pushed, merged or edited in tracked files. Worktree HEAD is d1a41f0 and `git status` is clean.

## Verdict: ACCEPT WITH CONDITIONS

The change only adds observation, and that holds up. Non-active tenants are swept by the `payment_statement` stream in exactly the same per-tenant session shape as active tenants. RLS isolation is intact. The sweep's session shape cannot create a manual resolution. M2 on a closed tenant is still gated by MR030 and by platform_acting four-eyes. Nothing in production code outside `internal/reconciliation` changed. There is no migration and no TEMP use. One of the stated fail-closed claims is not tested (F-3). Two design gaps need to be recorded and decided before a real PSP or a tenant-closure flow exists: the credential lifecycle for closed tenants (F-1) and unbounded observation work (F-2).

## Scope

- **Reviewed:**
  - the full diff of `internal/reconciliation/scheduler.go`, `payment_statement.go`, `r3_observation_static_test.go` and the R3 integration tests;
  - doc deltas in ADR 0095 §40.4, ADR 0101 §28.3, the ADR 0107 pointer and the runbook;
  - the outbound path: `MockStatementSource.Fetch` → `callProvider` (ReadOnly) → `OutboundCredentialResolver`, and the real `providercred.OutboundResolver.Resolve`;
  - the `RunSchedulerLoop` windowing, the alert and audit sinks, and the tenant-staff audit-log route.
- **Not reviewed:** the pre-existing payment_statement matcher (covered by round-2 evidence), a real PSP statement source (none exists), and the alert delivery path (ALERT-DELIVERY-1).

## Evidence run (private scratch DB `sec_r3`, built from `git archive d1a41f0`, dropped at the end)

- `-tags integration -count=1 -p 1 -run TestR3_ ./internal/payments/ ./internal/reconciliation/`: all 13 behavioural tests and all 4 static tests PASS.
- **Raw SQL probe as igaming_runtime** (`rolsuper=f`, `rolbypassrls=f`). I seeded closed tenant A and active tenant B through the owner, in a platform_admin-shaped transaction on the private DB only.
  - The sweep shape (only `app.tenant_id=A`) can insert A's own `reconciliation_runs` row. That is the intended behaviour.
  - A tenant-B session sees 0 of A's runs, mismatches, `payment_statement_lines` and `audit_log` rows.
  - A tenant-B session inserting a run for A is refused: "new row violates row-level security policy".
  - A no-tenant session (the listing shape) sees 0 runs. It can see `tenants`, but that is the pre-existing `tenants_read` policy.
  - The sweep shape inserting a `payment_manual_resolutions` row for closed A is refused before any MR check: "financial_actor_session: unresolvable session shape". A buggy sweep could not create a resolution even if the static pins were bypassed.
  - UPDATE on `reconciliation_runs` is refused because the table is append-only.
- **MR030 and four-eyes:** `TestR3_ClosedTenant_M2StaysGatedByTheStaffPath` passes with the sweep both run and not run:
  - a tenant-scope requester gets MR030;
  - a platform_acting request with 1 of 2 approvals is not executed;
  - a later sweep does not change the pending resolution.
  - The DB guard (0115 R-5 branch, `tenant_status = 'closed' AND scope <> 'platform_acting'` → MR030) is unchanged because there is no migration.
- **Transitive imports:** `go list -deps ./internal/reconciliation` contains only txscope, db, alerting, audit, ledger, providerref and reconciliation/statement. There is no payments, withdrawal, wallet or resolution package, even transitively. The only `ledger.Post` reference is in a comment.
- **Mutation re-run** on a throwaway export. Each mutant had to match exactly once; the file was restored from a backup and compared with `cmp` after every mutant, and every comparison was identical. I re-expressed the patterns myself, so they are equivalent to the ones in the evidence file but not byte-identical.
  - M2 drops closed: KILLED by `TestR3_ParkedDepositCapture_StandingFindingForNonActiveTenants`.
  - M3 predicate TRUE: KILLED by `TestR3_TenantReactivatedDuringTheSweepIsNotObservedAsNonActive`.
  - M4 no already-swept skip: KILLED by `TestR3_TenantStatusChangesBetweenAndDuringRuns`.
  - M6 audit flag false: KILLED by `TestR3_ParkedDepositCapture_...`.
  - M13 loop aborts on first error: KILLED by `TestR3_FailureOfOneTenantDoesNotStopOthers`.
  - M14 ordinary sweep includes non-active tenants: KILLED by `TestR3_ParkedDepositCapture_...`.
  - M17 no-source guard removed: KILLED by `TestR3_NoSource_NonActiveTenantIsNotSwept`.
  - X1 (new): every observation runs under the first non-active tenant's id, a cross-tenant scope bug. KILLED by `TestR3_FailureOfOneTenantDoesNotStopOthers`.
  - **X2 (new): `RunSweepTenants` swallows the non-active listing error. SURVIVED** (see F-3).

## Findings

### F-1 (MEDIUM; launch condition for any tenant-closure flow and for a real PSP; does not block this MOCK change)

**Problem:** for a closed or suspended tenant, the outbound statement fetch uses that tenant's still-active outbound credential, and no policy governs this.

- `providercred.OutboundResolver.Resolve` checks only the handle (`status='active'`, `not_after`) and never `tenants.status`.
- ADR 0093 and ADR 0094 define no credential revocation or retention on tenant closure, and no closure flow exists.
- This change makes the closed-tenant credential live on purpose, so one of the two outcomes below now happens by default with no decision behind it.

**Concrete failure scenarios:**

- **(a) The handle is left active.**
  - Every tick, the platform keeps authenticating to an offboarded operator's PSP merchant account with that operator's credential.
  - For an `own_licence` tenant, that account is the operator's, under its own licence and contract.
  - There is no end condition, so this goes on forever.
  - This is a contractual, data-protection and licensing question for the human. It is not an engineering call.
- **(b) Closure revokes the handle.**
  - Every tick, every closed tenant fails at the fetch phase. Each failure writes a `reconciliation.sweep_run_failed` P1 audit row and a `reconciliation.run_failed` alert, forever.
  - The detective control the owner asked for is silently replaced by permanent noise. Operators learn to ignore it, so a real closed-tenant finding is lost.

**Condition:**

- Record this as an open decision next to §40.4: the credential treatment for observation on closure, and an observation end condition such as "all hold-bearing work resolved and N clean runs".
- Make the tenant-closure flow (already blocked by H-SEC-5 and HD-CTF) depend on that decision.
- The doc's "PROVIDER DEPENDENT for what a real PSP allows for a closed merchant" covers the PSP side only. It does not cover this question.

### F-2 (MEDIUM for a real PSP, LOW today; DoS and volume)

**Problem:** observation work is unbounded, and a long sweep can widen a pre-existing coverage gap.

- Non-active tenants are never dropped from observation, and the sweep runs serially.
- Each tick does (number of non-active tenants) × (number of sources) fetches.
- Each fetch is bounded only by the gate's per-call timeout (`DefaultCallTimeout` or the manifest's `CallTimeout`). The sweep as a whole has no budget.
- Observation runs after the active tenants, so it does not delay the current tick's active runs. It does lengthen the tick.
- `RunSchedulerLoop` uses the window `[now-interval, now)` with a `time.Ticker`, which drops ticks while a sweep is still running. A sweep longer than `interval` therefore skips statement windows for ACTIVE tenants too.
- That coverage gap already existed, but it was only reachable with a slow active set. It now grows with the number of closed tenants, which only increases.

**What is fine:** advisory locks and the skip on lock contention are unchanged and tested (two concurrent schedulers both complete), and per-tenant failures are isolated.

**Condition (before a real PSP):** do at least one of the following and record it as a residual now:

- give observation a time budget or an observation horizon/end condition;
- bound its concurrency separately from the active sweep;
- make the period contiguous from the last successful run instead of `now-interval`.

### F-3 (LOW; required before marking complete because it is cheap)

**Problem:** the "fail closed on listing failure" claim is untested, and in production it degrades to a log line.

- ADR 0095 §40.4 and the code comment say "a failure to list them is returned, never swallowed".
- Mutant X2, which swallows that error in `RunSweepTenants`, SURVIVED. No test forces the non-active listing to fail.
- In production, the only consumer is `RunSchedulerLoop`, and it only logs the error at Error level. Unlike a per-tenant failure, a listing failure produces no audit record and no `reconciliation.run_failed` alert.
- A persistent listing failure (for example a future policy change on `tenants` for the WithoutTenant shape) would therefore silently switch off closed-tenant observation.
- The same pre-existing behaviour applies to the active-tenant listing.

**Condition:**

- Add a test that makes `nonActiveTenants` fail and asserts that both `RunSweep` and `RunSweepTenants` return the error with the active outcomes still present.
- Either raise a platform-scope alert on a listing failure, or record "log-only" as a residual in §40.4.

### F-4 (LOW / INFO; tracked under ADR 0107 Q-CT-SEC-1 / HD-CTF-2)

**Problem:** closed-tenant staff can read new post-closure audit rows.

- `internal/auth` has no `tenants.status` check, so tenant-staff sessions are not refused when their tenant is closed.
- R3 now writes post-closure `audit_log` rows with `tenant_id` set to the closed tenant: `reconciliation.sweep_run` and `sweep_run_failed`.
- Staff of the closed tenant who have audit-read can see these rows through `GET /v1/admin/audit-log`.

**Why this is low:**

- The content is benign: counts, `provider_id`, `import_id`, `statement_source`, status, the observation flag and `tenant_status`.
- Error text goes through the gate's `redactedReason`, so it carries no secrets and no PII.
- Payment findings and statement lines have no HTTP route; only casino reconciliation routes exist.
- Alerts are platform-scope.

**Condition:** no code change needed now. Note in ADR 0107 that observation creates new post-closure tenant-scoped audit rows, so HD-CTF-2's answer also covers them.

### F-5 (INFO; carry into security D-5 for the real source)

**Problem:** the static "read-only" pins do not cover the injected statement source.

- The pins cover `internal/reconciliation` and its transitive dependencies. They do not cover the injected `PaymentStatementSource` implementation, which lives in `payments` and runs through `callProvider{ReadOnly: true}`. ReadOnly skips the claim/attempt-state check.
- A real source that called a mutating PSP endpoint, or a source wrongly reused for a non-read call, would pass every R3 pin.
- **Condition for the first real source:** review it as a read-only statement call and add a pin that it calls only the statement endpoint.

### F-6 (INFO)

**Problem:** the ledger pin can be evaded by a future function name.

- `TestR3_Static_LedgerUsedReadOnly` allows any `ledger.Tx*` or `ledger.Account*` symbol by name prefix.
- Today these are all constants; I verified that no exported ledger function has those prefixes.
- A future `ledger.TxSomething` function would evade the pin.
- **Suggestion:** use go/types to restrict the allowance to constants. This is optional.

## Checked and found sound

- **Session shape:** observation uses the same per-tenant `WithTenant` / `WithTenantSnapshot` shape as an active tenant, with only `app.tenant_id` set to that tenant. Listing uses `WithoutTenant` and reads only `tenants(id, status)`.
- **Cross-tenant isolation:** a closed tenant's lines cannot clear another tenant's finding (matching runs in the tenant's own snapshot, and `TestR3_CrossTenantIsolation` covers it). My SQL probes confirmed the RLS behaviour.
- **No new endpoint:** `RunSweepTenants` has no production caller, and `RunSweep` is called only by the scheduler loop.
- **Audit metadata:** adds only `non_active_tenant_observation` and `tenant_status`. There are no secrets in audit or alert attributes, and the alert schema is unchanged.
- **Payments sweeper:** no production code in `internal/payments` or `cmd` changed, only the test counters in `k3_world`. The sweeper stays resolution-only.
- **0116/0117:** no TEMP use and no migration.
- **Per-tenant failure isolation:** a fetch failure is audited, alerted and does not stop the other tenants (killed M13 and X1).

## Conditions summary

1. F-3: add the listing-failure test, and either add an alert or record log-only as a residual. Required before marking complete.
2. F-1: record the closed-tenant credential and observation-end decision as an open human decision, and as a prerequisite of the tenant-closure flow and the real PSP. Doc only; required before marking complete.
3. F-2: record the bounded-observation and contiguous-window requirement as a real-PSP launch condition. Doc only; required before marking complete. Implementation comes before a real PSP.
4. F-4, F-5, F-6: tracking notes. They do not block completion.

**Launch flags:** F-1 and F-2 should block launch of any tenant-closure flow and of real-money operation on a real PSP until they are resolved. This is in addition to the residuals §40.4 already lists. This review does not declare the feature secure beyond the scope above.
