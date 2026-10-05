# Ledger-finance review: prh2-r3-closed-tenant-recon @ d1a41f0 (base 66d2968, no migration)

**VERDICT: ACCEPT WITH CONDITIONS.** C-1 is a required documentation correction before merge. C-2 and C-3 are follow-ups to register.

This was a read-only review. Nothing was committed, pushed or edited in the worktree (`git status` is clean, HEAD is d1a41f0). I ran nothing privileged and made no role or credential changes. All work ran in the private DB `lf_r3`, built from `git archive d1a41f0` and dropped at the end.

## Evidence run
- **TestR3_* (`-tags integration -p 1`):** all 13 behavioural tests (internal/payments) and all 4 static tests (internal/reconciliation) PASS. Note that without `-tags integration` only the 4 static tests run.
- **My own probe, scratch only (`TestLFProbe_UnscopedSweepClosedTenantDigest`).**
  - Setup:
    - Closed tenant W holds 5 attempts (pending, ambiguous, disputed, a second ambiguous payout, and a parked deposit), 5 ledger tx, 10 entries, 4 withdrawals and 1 deposit intent.
    - W also has a PENDING `platform_acting` M2 request.
    - W has a second, active tenant V on the same pool.
  - Action: an UNSCOPED `RunSweep` with the real `MockStatementSource` (through the `callProvider` gate) plus a scripted source whose line names V's reference.
  - Check: full-row md5 (`to_jsonb` of every row) of 15 money tables in three RLS scopes (tenant, principal, platform_admin), taken before and after:
    - ledger_transactions, ledger_entries, ledger_accounts, wallets and wallet_balance_projection
    - payment_attempts, payment_attempt_reference_evidence and payment_provider_events
    - deposit_intents, withdrawal_requests and withdrawal_approvals
    - payment_manual_resolutions and payment_manual_resolution_approvals
    - ledger_adjustment_requests and payment_kill_switches
  - Result:
    - No digest changed, for W or for V.
    - The provider call counters did not move.
    - The pending resolution stayed pending.
    - W was observed exactly once, with ObservationOnly=true and status=closed, and both runs were `mismatches_found`.
    - Invariants held for both tenants.
- **Mutation, on a throwaway copy, with the restore checked by `cmp`.**
  - The CONTROL run passed first.
  - I re-ran 13 of the 22 listed mutants, all KILLED: M1, M2, M3, M4, M6, M7, M10, M11, M13, M14, M17, S3 and S5.
  - I also ran one extra mutant, LF1 (observation called with empty `PaymentStatementOptions{}`), also KILLED.
  - Every restore was cmp-identical, S3/S5's `zz_mut.go` was removed each time, and the final cmp of scheduler.go against the export matched.
  - Log: `$S/r3lf-mut.log`, harness: `$S/r3lf_mut.py`.

## (1) Guarantee: observation means reads plus findings/alerts only
- **Structural: holds.**
  - `go list -deps ./internal/reconciliation` contains only these internal packages: txscope, db, alerting, audit, ledger, providerref and reconciliation/statement.
  - It reaches no payments, withdrawal, wallet or resolution package.
  - `ledger` is used only for `RebuildBalance`, `GetProjectedBalance` and type constants.
  - SQL writes are limited to reconciliation_runs, reconciliation_mismatches, payment_statement_imports and payment_statement_lines (plus audit_log and alerts through their own packages).
  - `ResolveMismatch` is not on any sweep path.
  - `observeNonActiveTenants` calls only `nonActiveTenants` and `ReconcilePaymentStatementForTenant`.
  - `ObservationOnlyStatus` is read only by `observationMetadata`; the matcher has no tenant-status dependency.
- **Behavioural: holds.** See the R3-3 and R3-4 tests and my digest probe above.

## (2) Scheduler
- **Predicate.** `status <> 'active'` is fail-safe for a read-only path.
- **Ordering.** Observation runs after the full active sweep, with the same period window.
- **No double observation within a call.** The `alreadySwept` de-duplication relies on `sweepTenants` always emitting one outcome per tenant, which it does.
- **Concurrency.** Two concurrent calls are serialised by the per-tenant `reconciliation:payment_statement:<tenant>` try-lock; the loser records a clean skip. Re-imports are de-duplicated by the `(tenant, provider, label, coverage, digest)` ON CONFLICT.
- **Reactivation race.** A tenant that is closed at the active listing and reactivated before the observation listing is skipped for one tick. That is acceptable and pinned by a test.
- **Failure isolation.** Failures are isolated per tenant.
- **No mis-attribution.** I found no path that attributes one tenant's data to another:
  - `t.ID` flows unchanged into `WithTenant`, `WithTenantSnapshot` and `Fetch(TenantID)`.
  - Imports and lines are written with tenant_id under RLS.
  - The matcher loads lines by importID inside the tenant's own RLS scope.
  - Pinned by R3-5, where closed A's reversal line naming B's reference does not clear B's `pay_captured_unposted`.

## (3) K3 effects
- The same matcher and the same rules apply: clearing (d)/(c2), `pay_declared_paid_unconfirmed` (raised, then cleared by a confirming line — R3-2), and yAttributable/G-Y2, which stays tenant-scoped exactly as before.
- M2 on a closed tenant is still MR030 for a tenant-scope requester and needs the full `platform_acting` four-eyes count. A sweep neither creates, approves nor executes a resolution (R3-4, and my probe's pending resolution).

## (4) Outbound statement fetch for a closed tenant
**PROVIDER DEPENDENT.**
- **How the fetch is gated.** `MockStatementSource.Fetch` goes through `callProvider` with ReadOnly=true and Domain="payments". For a real PSP, `providercred.OutboundResolver` would resolve the tenant's ACTIVE or verify_only payments handle. Neither the resolver nor the gate checks tenants.status (the payments sweeper also relies on this, since it is resolution-only).
- **What a closed tenant's live credential could do.** Whatever the PSP key's scope allows: statement and status reads, and potentially deposit, payout and refund calls if the key is not scoped. The R3 code path only reads. The read-only property of the fetch is a contract on the `PaymentStatementSource` implementation, which lives outside `internal/reconciliation`. The static import pin does not and cannot enforce it.
- **Accepted for MOCK.** For the first real source, see C-3.

## (5) Audit
- `non_active_tenant_observation=true` and `tenant_status` appear on both `sweep_run` (including lock-skipped runs) and `sweep_run_failed`. Active runs carry no flag.
- The status recorded is the status at selection, not at run time. This is disclosed.
- The `reconciliation_runs` row and the alerts carry no marker; you need the audit join to see it. Disclosed and acceptable.

## (6) Docs honesty
- ADR 0095 §40.4, ADR 0101 §28.3, reconciliation-model item 12 and runbook item 7 label the work IMPLEMENTED against MOCK. A real source is NOT IMPLEMENTED / PROVIDER DEPENDENT.
- None of them claims real-PSP readiness.
- The second half of H-W1 (scheduled is not the same as last-run-succeeded), R-S1, R-S3/R-S4, ALERT-DELIVERY-1 and D-5 are still disclosed as launch blockers.
- ADR 0107 stays "PROPOSED — DESIGN ONLY", with a pointer that it is not implemented.
- There is one false statement; see C-1.

## Findings / conditions
1. **C-1 (MEDIUM, REQUIRED before merge, docs only).** ADR 0095 §40.4 item 1 says "a frozen tenant's ledger does not move ... Revisit if a non-active tenant can still post (it cannot today through the payments path, which is resolution-only)". This is false.
   - **Probe evidence (`TestLFProbe_ClosedTenantLedgerMovesViaStaffM2`).** On a CLOSED tenant, a `platform_acting` M2 declare_not_paid executed with four-eyes and posted a `withdrawal_failed` ledger transaction (ledger_transactions went from 2 to 3).
   - **A second path.** `sweeper_resolution_only.go` explicitly allows "evidence application (including dispute and T17 re-drive)" for non-active tenants, which posts ledger entries.
   - **The gap.** `ledger_vs_projection` never runs for a non-active tenant (the same probe showed a zero ledger run id). A closed tenant's postings therefore get no hourly drift check, against CLAUDE.md ("recompute and diff ... hourly; any non-zero drift is a P1").
   - **Scope.** This gap is pre-existing and R3 does not make it worse, but the new text gives the wrong reason for deferring it.
   - **Fix.** Replace the sentence with an accurate statement: non-active tenants still post through staff M2 (R-5) and sweeper evidence application, and `ledger_vs_projection` for them is a deferred gap. Register C-2.
2. **C-2 (follow-up, ledger-finance P1-class gap, not blocking R3).** Register and schedule a ledger_vs_projection run for non-active tenants. It is purely internal (no outbound call, no credential) and read plus findings only, so it is the lowest-risk stream to extend under the same H-W1 owner decision. It must exist before a closed tenant can carry real-money staff resolutions.
3. **C-3 (follow-up, PROVIDER DEPENDENT, for the first real statement source).** The real `PaymentStatementSource.Fetch` must be reviewed by ledger-finance and security as read-only. Wherever the PSP supports it, it should use a read- or reporting-scoped credential and not the full payments key. Also decide the credential lifecycle on tenant closure: if credentials are revoked or the merchant account is closed, every observation fails with an hourly `run_failed` P1 per closed tenant, with no mute or expiry. Document that operator expectation.
4. **N-1 (LOW).** If the non-active listing fails, `RunSchedulerLoop` only logs it, under the misleading message "failed to list tenants"; there is no audit or alert, the same as the pre-existing active-listing failure. A persistent failure silently stops closed-tenant observation. Recommend an audit record plus a `run_failed` alert in a follow-up.
5. **N-2 (LOW, test-strength note).**
   - The static pins check direct imports only, not `go list -deps` transitively. Today the transitive set is clean.
   - `TestR3_Static_LedgerUsedReadOnly` matches the identifier `ledger`, so an aliased import would bypass it.
   - Optional hardening for both.
6. **N-3 (environment note).** One k3World scratch DB (`m0101v2_25fbf73a3f974914`) and possibly others from my test runs were left behind ("permission denied to terminate process"). I did not escalate to clean them up. `lf_r3` itself was dropped.

## Files
- `/home/user/igaming-platform/.claude/worktrees/agent-aa4b1afb856cae77f/internal/reconciliation/scheduler.go`
- `/home/user/igaming-platform/.claude/worktrees/agent-aa4b1afb856cae77f/docs/decisions/0095-provider-io-transaction-boundary-and-payment-contract.md` (the §40.4 item 1 sentence, C-1)
- `/home/user/igaming-platform/.claude/worktrees/agent-aa4b1afb856cae77f/internal/payments/sweeper_resolution_only.go` (the evidence-application allowance)
- Probe (scratch only): `$S/r3lf-src/internal/payments/zz_lfprobe_integration_test.go`. Mutation log: `$S/r3lf-mut.log`.
