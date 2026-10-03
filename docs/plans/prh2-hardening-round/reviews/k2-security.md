# PRH-2 K2 — Security review

_Reviewer: `security` specialist. Branch `prh2-k2-manual-adjustments` @ `18c357a` (base `9c6c705`). Recorded verbatim by the orchestrator._

Security review — PRH-2 K2 (branch prh2-k2-manual-adjustments @ 18c357a, base main 9c6c705; ADR 0100 incl. §19/§20, ADR 0099 §20)

VERDICT: ACCEPT WITH CONDITIONS. There are 3 pre-merge conditions:
- K2-C1: a real DB-level defect on the acting money path, proven by execution.
- K2-C2 and K2-C3: two surviving mutants on core controls.

All my K1 hard gates (K2-G1..G4) are CLOSED.

Launch flags, unchanged and still open: TM-7, TM-10, HD-PRH2-8 (human), LEDGER-MANUAL-ADJ-LINK-1 (launch-blocking), and STAFF-LIFECYCLE-1 revoke-on-suspend. Given the §7.6 residual below, that last one is now also a launch prerequisite.

Method:
- git archive of 18c357a. build and vet clean.
- Private DB sec_k2_rv_20260928 (dropped WITH (FORCE); verified 0 left).
- `-race -count=1 -tags integration -p 1`, all ok: internal/adjustment, db, ledger, audit, reconciliation/statement, and httpserver `-run 'ManualAdjustment|Capability|K2'`.
- internal/reconciliation had 2 failures: TestINVDEP1_Recon_M_DuplicateDetector_LegacyDataShape and TestCasinoConsistency_C5_TombstoneConflict ("column subject_tenant_id … does not exist" in their older-migration scratch DBs). I reproduced both on base main 9c6c705, so they are PRE-EXISTING and not caused by K2. Please route them separately.
- Mutants: 9 of my own on 0113 plus M9/M10 on 0112, each with a DB rebuild, then reverted and cmp-verified identical.
- 1 execution probe (scratch copy only).
- No role or credential changes, no sudo, pipefail on, grepped for FAIL.

------------------------------------------------------------------
PRE-MERGE

K2-C1 (High, proven): the acting ledger fence does not enforce the closed §4 shape. The executing -> refused_insufficient_funds exit skips the link check.
- Branch (a) of ledger_governed_fence_allows checks only four things: type = manual_adjustment, idempotency key, correlation = request id, and state = 'executing' AND executed_txid = txid_current().
- The entries fence checks only that the parent transaction passes that test. It never checks account, wallet, amount, asset or direction.
- The shape is verified only by ledger_adjustment_verify_link, on the executing -> executed transition.
- The executing -> refused_insufficient_funds branch requires only NEW.ledger_transaction_id IS NULL. It never checks that no governed-key transaction was posted.

Probe (real code, valid acting session, base-1 policy, request = credit 500):
1. Insert the final approval.
2. Move the request to 'executing'. The DB recount passes.
3. ledger.Post a manual_adjustment with the governed key and correlation, but 5000 instead of 500.
4. Set state = 'refused_insufficient_funds'.
5. Commit.

Result: commit OK, request refused_insufficient_funds, player cash 0 -> 5000. The same unchecked entries path would let the entries target any tenant-X ledger account the acting session can read, including another player's player_cash. I did not execute that variant; it follows from the same code. The `ledger_unlinked_manual_adjustment` detector should flag this after the fact. That is detective only, and it is not P1-routed yet (I-wire).
- Exploiting this needs a bug in, or compromise of, internal/adjustment, but the DB fence is the stated defence for exactly that case.

Fix, at minimum:
- (i) executing -> refused_insufficient_funds (and any other non-executed exit) refuses if any ledger_transactions row exists with idempotency_key = 'manual_adjustment:'||id in the tenant.
- (ii) the entries fence validates each acting-inserted entry against the executing request. The account must be the request wallet's player_cash, or the tenant's manual_adjustment house account for the asset. Asset, amount and direction must match the payload, and the entry count stays at 2 or fewer.

Add my probe, plus a wrong-wallet variant, as tests. LEDGER-MANUAL-ADJ-LINK-1 (all sessions, deferred) does not cover this: this is the acting-session invariant ADR 0099 §6.6 claims now.

K2-C2 (Medium, test gap): mutant F2 SURVIVED. I dropped `r.state = 'executing'` from ledger_governed_fence_allows, leaving only executed_txid = txid_current(). The full adjustment suite passes. Without the state arm, an acting session can keep appending entries to the linked transaction in the same transaction AFTER the executed transition, which is after verify_link ran. That gives the same over-credit outcome as K2-C1 via the executed exit. Required: a B16 case that, in one acting transaction, executes a request and then inserts an extra entry into the linked transaction, expecting CG030.

K2-C3 (Medium, test gap on HD-PRH2-1's DB backstop): mutant F7 SURVIVED. I removed the -> executing guard's recount (`IF NOT v_exec.enabled OR NOT v_exec.initiator_valid OR v_exec.counted < v_exec.required`) and the full suite passes. This is the DB-level four-eyes backstop. Without it, a session that bypasses the Go executor can go pending -> executing -> post -> executed with fewer counted approvals than required. Required: a test that drives pending -> executing directly, inserting one valid approval where required = 2 (and a variant with an invalid initiator), expecting MA030.

------------------------------------------------------------------
Hard gates: CLOSED
- K2-G1:
  - The hardened A-18 has positive-only guards, true-arm detection and a SELECT allowlist, plus planted cases for my three false negatives and the dynamic acting-visibility probe (TestK2G1_ActingSessionRowVisibilityMatchesAllowlist). All pass.
  - The two findings are REAL. The 0043/0045 `tenant_and_platform_read` SELECT policies admit `tenant_id IS NULL` rows to any player-unset session, acting included; the 0049 narrowing covered writes only.
  - Sensitivity is low: platform-level eligibility and self-exclusion config, already visible to every tenant staff session.
  - The fences are correct: AS RESTRICTIVE FOR SELECT USING (NOT financial_acting_gucs_present()).
  - The executor needs neither table. K2-b's suspended-asset rule reads asset_authorizations and fails CLOSED when blind (NOT EXISTS means suspended).
  - Low, not blocking: a18IsGuarded still accepts a positive guard anywhere in the statement, so an OR-ed guard (`tenant_id IS NULL OR <positive guard>`) would pass lexically. The dynamic probe covers reads only. Consider a planted OR case or a dynamic write probe.
- K2-G2: re-killed M9 (grants acting_read tenant predicate bypassed) -> KILLED by K2G1 and K2G2_ActingSessionCannotReadAnotherTenantsGrants. The "widen acting" layered test is present and passes.
- K2-G3: re-killed M10 (acting validity status re-read dropped) -> KILLED by K2G3_SuspendedGranteeActingOpenRefused.
- K2-G4:
  - A-1/A-12 over HTTP pass (manual_adjustment_api_integration_test.go).
  - TestK2P3_ActingSetterCallSitePinned passes. There is one call site (adjustment.go:169), with the subject taken from tenant.FromContext and the target from NewTarget.
  - Use-time re-checks: my F8 (live person = grant snapshot removed) -> KILLED by K2G4_TenantUnchangedAndLivePersonRechecks.
  - FOR SHARE locks the grant IN FORCE AT now() (execute.go lockStaffAndGrants: revoked_at IS NULL AND valid_from <= now() AND valid_until check), in ascending id order, after the staff rows.

My other mutants on 0113:
- F3, acting ledger_accounts INSERT widened to house_gaming: KILLED by B16.
- F4, base >= 1 check removed: KILLED by B20.
- F5, approvals S-12 check removed: KILLED by B5.
- F6, grant function dropped from ledger_entries acting_insert: KILLED by TestActingPolicies_EveryK2ActingPolicyRequiresTheGrantFunction.
- F9, beneficiary exclusion removed from counting: KILLED by TestLayered_ExecutionCountIgnoresBadApprovals.
- F1, executed_txid arm dropped from the fence: survived, but it is EQUIVALENT and accepted. A row in 'executing' is only ever visible inside its own transaction: MA041 forbids committing it, and READ COMMITTED hides other transactions' uncommitted rows. So the state arm alone already binds to the current transaction. Keep the txid arm as defence in depth.

------------------------------------------------------------------
Items requested
2. ADR 0099 §20 amendments: ACCEPTED.
   - acting SELECT on `licences`: own licence only (via tenants.licence_id).
   - acting SELECT on `asset_authorizations` and `staff_capability_grant_requests`: tenant-X rows only, each gated by financial_acting_session_valid(). All three are needed so the evaluator, K2-b and K2-G4 do not fail closed. No cross-tenant or PII exposure beyond X's own grant metadata.
   - `tenant_system_read_executed`: accepted. It is same-tenant and executed-only, and admits no principal, platform, player, service or acting shape. The rows are no more sensitive than the tenant's ledger, which that session shape already reads.
   - `schema_migrations` without RLS: accepted. It holds versions and checksums only, and the runtime role has SELECT only.
   - acting `ledger_accounts` INSERT limited to player_cash/manual_adjustment: accepted (stricter; F3 killed).
   - §20.3 items 1–14: accepted. Item 7 (note hash vs audit) is an accepted residual, because both are written in one transaction by the executor. Item 10 (four pre-K2 tests now tolerate this mismatch kind) is accepted only because LEDGER-MANUAL-ADJ-LINK-1 is launch-blocking.
3. Ledger fence branch (a): NOT as claimed. See K2-C1 and K2-C2. The projection fence and "type/key/correlation only" are correct and tested (B16).
4. The policy rules hold:
   - HD-PRH2-1: four-eyes cannot be disabled. required = GREATEST(1, max over in-force rows); the mandatory class's base must be >= 1 at insert; enabled requires a platform row. The DB backstop is unpinned (K2-C3).
   - HD-PRH2-7: platform, jurisdiction and profile rows and profile assignments need a platform requester and a platform approver with financial_policy:author. Tenant rows are tighten-only, and evaluation is max-combined, so a tenant can never go below the platform baseline.
   - HD-PRH2-3: no threshold is seeded; with no platform row the operation is DISABLED (MA014).
   - HD-PRH2-8 interim (b): enforced (F4 killed).
5. S-12 and LF-11: CONFIRMED.
   - The beneficiary is excluded at initiation, approval and counting (F5 and F9 killed).
   - Distinct live Person, with live person = grant snapshot at use (F8 killed).
   - Tenant-scope staff rows and in-force grants are taken FOR SHARE before the recount.
   - A NULL player Person makes the initiator invalid, so it fails closed.
   - §7.6 residual (a platform approver's own row is invisible, so its status is not re-checked at execution; only its grant and Person snapshot are): ACCEPTED FOR MERGE. Before any real-money use it depends on STAFF-LIFECYCLE-1 revoking grants in the same transaction on suspend, DB-enforced as I required in S-b. Please record that dependency.
6. "Drop the grant function" mutant: covered for all 24 acting policies. I re-killed one (F6 on ledger_entries acting_insert).
7. 0113 down defect fix: CONFIRMED, no RLS-off window.
   - db.MigrateDown runs the whole down file plus the schema_migrations delete in one transaction.
   - NO FORCE is transactional and takes ACCESS EXCLUSIVE, so other sessions never observe it.
   - It only affects the owner; the runtime role stays under ENABLE RLS.
   - A refusal rolls back and FORCE is restored. reconciliation_mismatches FORCE is re-asserted explicitly. B19/D01 pin it.
   - Low: add a header note that the file must run inside a single transaction. Run by hand with psql autocommit, a refusal would leave FORCE lifted for the owner.
8. 68 mutants: my sample does not reproduce the "68/68 killed" claim.
   - 11 run: 8 killed, F1 equivalent, F2 and F7 SURVIVED.
   - The evidence file should add F2 and F7 (they are not the same as its "drop the ledger fence" / "drop the entries fence" mutants) and record the F1 equivalence.

Scope: code- and DB-level only; no penetration test. I did not review K3's Step-B arm (not built), the backoffice UI (flags only), or HD-PRH2-8 (human decision).

Relevant paths (commit 18c357a):
- migrations/0113_governed_manual_adjustments.up.sql:
  - ledger_governed_fence_allows and the entries fence (~1415–1462);
  - the executing -> refused_insufficient_funds branch in ledger_adjustment_requests_guard (~1098–1108);
  - the -> executing recount (~1080–1090);
  - verify_link (~1151).
- migrations/0113_governed_manual_adjustments.down.sql
- internal/adjustment/execute.go (steps 7–10)
- internal/adjustment/b11_b17_integration_test.go (TestB16_ActingFences, the place to add the K2-C1/C2 cases)
- internal/db/null_arm_replay_static_test.go
- internal/db/k2_gates_integration_test.go
- docs/plans/payment-readiness/evidence/prh2-k2-mutation-kill.txt
