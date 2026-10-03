# PRH-2 K2 — Security re-review

_Reviewer: `security`. Branch `prh2-k2-manual-adjustments` @ `309f33b`. Recorded verbatim by the orchestrator._

Security re-review — PRH-2 K2 (prh2-k2-manual-adjustments @ 309f33b; fixes in 99d5a8f, evidence in d17b382 and 309f33b, main merges 49cf696 and c458bb8; previous review k2-security.md on 18c357a)

VERDICT: ACCEPT. K2-C1, K2-C2 and K2-C3 are CLOSED. The fix introduced no new finding. Nothing is pre-merge from security.

The launch flags are unchanged and remain open: TM-7, TM-10, HD-PRH2-8 (human), LEDGER-MANUAL-ADJ-LINK-1 (launch-blocking), and STAFF-LIFECYCLE-1 revoke-on-suspend as the §7.6 launch prerequisite.

Method:
- Local PostgreSQL is reachable after the restart. I used a fresh private DB, sec_k2b_rv_20261003, then DROPPED it WITH (FORCE); verified 0 left. No role or credential changes, no sudo, pipefail on, grepped for FAIL.
- git archive of 309f33b. build and vet clean.
- `-race -count=1 -tags integration -p 1`, all ok: internal/adjustment, db, ledger, audit, reconciliation (the two failures I reported last time are gone with the main merges), reconciliation/statement, and httpserver `-run 'ManualAdjustment|Capability|K2'`.
- 6 mutants on 0113, each with a DB rebuild, then reverted and cmp-verified identical.
- 4 probes in a scratch-only test file. The scratch copy is removed.

------------------------------------------------------------------
Probes at 309f33b. Each is a valid acting session, base-1 policy, request = credit 500.
- Original probe (post 5000 under the governed key, then refused_insufficient_funds): REFUSED at the post with CG030. Nothing committed; request still pending; player cash 0.
- Wrong wallet, then the executed exit (correct amount to another player's player_cash): REFUSED at the post with CG030.
- Wrong wallet, then the refused exit: REFUSED at the post with CG030.
- Post after the refused exit (refused_insufficient_funds first, then an exactly-correct posting in the same transaction): REFUSED at the post with CG030.

Mutants (all KILLED):
- F2, the fence state arm dropped: killed by TestK2C2_FenceStateArmAfterExit/governed_posting_after_the_refused_exit, and independently by my post-after-refused probe.
- F7, the -> executing DB recount removed: killed by both subtests of TestK2C3_DBRecountOnExecuting (1 of 2 approvals, and a revoked initiator grant). Both assert MA030 on the UPDATE itself.
- G1, the C1(i) non-executed exit check removed: killed by TestK2C1 (the correct-shape-then-refused and wrong-wallet cases) and by TestK2R1, across three tenant-session exits.
- G2, the C1(ii) amount check dropped: killed by TestK2C1/probe over-credit.
- G3, the C1(ii) wallet match dropped: killed by TestK2C1/wrong wallet.
- G4, the C1(ii) one-leg-per-direction / at-most-2 cap dropped: killed by TestK2C1/duplicated balanced legs.

------------------------------------------------------------------
Rulings

K2-C1: CLOSED.
- (i) Every non-executed exit (refused_insufficient_funds, refused_at_execution, rejected, cancelled, expired) refuses with MA040 when a governed-key transaction exists. This applies to all session types, so the only exit that leaves a posting behind is executed, and that one runs verify_link.
- (ii) Each acting-inserted entry must now be a leg of the request's closed §4 shape:
  - the request wallet's player_cash in the player direction, or the tenant's manual_adjustment house account (wallet_id IS NULL) in the opposite direction;
  - the request's asset and amount;
  - at most one leg per direction and at most two entries.
- The implementer's comment that earlier rows of the same multi-row INSERT are visible to the BEFORE ROW trigger is correct for a VOLATILE plpgsql trigger function. G4 confirms the cap is effective.
- Residual, unchanged and already flagged: a TENANT-session executor has no entry-level fence. C1(i) still blocks the unlinked-exit escape there, and the rest is LEDGER-MANUAL-ADJ-LINK-1 (deferred, launch-blocking).

K2-C2: CLOSED. "Posting after the refused exit" is the RIGHT gate for F2. The implementer is correct that the new count cap alone refuses my original "executed, then append an entry" case: the linked transaction already holds both legs. After a refused exit, executed_txid stays equal to txid_current(), and C1(i) has already run, before the posting. So the state = 'executing' arm is the only control that stops a correctly shaped posting from committing alongside a refused request. The F2 kill pins exactly that unique job.

K2-C3: CLOSED. The HD-PRH2-1 DB backstop is now pinned independently of the Go executor, and F7 is killed by both cases.

------------------------------------------------------------------
New code checked
- R-3 token exposure: NO LEAK.
  - RefusalToken returns a token only if it is in a closed 12-entry allowlist; otherwise it falls back to a fixed per-code token (asset_rule, reason_code_rule, note_invalid, invalid). DB message text is never written to the response.
  - wallet_not_found and causation_not_found are evaluated inside the caller's own tenant (session-tenant RLS plus explicit tenant predicates), so a foreign-tenant id and a nonexistent id return the same token. That gives no cross-tenant existence oracle.
  - The routes are gated by the static ledger_adjustment:initiate/approve permissions, so only already-authorized finance or platform staff see the tokens.
  - The mapping 40001/40P01 -> retryable 409 commits nothing.
  - Cosmetic: the request guard's own "wallet % not found in tenant" (MA021) maps to the generic "asset_rule" token. That is safe, just less legible.
- R-2: the expired path commits the pending -> expired transition and its audit row, then returns ErrRequestExpired (409) only after the session has committed. No decision row is written. Acceptable.
- A-18 a18IsGuarded: now parses the AND/OR structure, with planted cases in both directions, and the suite passes. This closes my Low.
- The 0113 down header now states it must run in a single transaction and gives the safe psql invocation. This closes my Low.
- Evidence: 79 run, 78 killed, and F1 recorded as SURVIVED/EQUIVALENT with my reasoning. The disclosed history, including the malformed first C1ii mutant, is consistent with what I observed. My independent sample at this head: 6 of 6 killed.

Scope: code- and DB-level only; no penetration test. Not reviewed: the merged main content (J, F-kyc, E2 and the webhook metrics commits, already reviewed separately), K3's Step-B arm, and the backoffice UI.

Relevant paths (commit 309f33b):
- migrations/0113_governed_manual_adjustments.up.sql (the C1(i) exit check in ledger_adjustment_requests_guard; the C1(ii) shape and cap in ledger_entries_governed_fence)
- migrations/0113_governed_manual_adjustments.down.sql (header)
- internal/adjustment/k2_security_conditions_integration_test.go
- internal/adjustment/adjustment.go (RefusalToken, ErrRequestExpired)
- internal/httpserver/manual_adjustment_routes.go
- internal/db/null_arm_replay_static_test.go
- docs/plans/payment-readiness/evidence/prh2-k2-mutation-kill.txt
