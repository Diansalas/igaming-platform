# RV-PRH-I1 — Security review: payout dispatch (ADR 0095 T1p / phase B / phase C, §27.11–§27.13)

- Reviewer: `security` specialist
- Date: 2026-09-27
- Branch / base: `claude/focused-wright-jw88w9` @ `091ed1e`. HEAD moved to `6e968b5` during the review, but `git diff 091ed1e 6e968b5` touches none of the reviewed files.
- Read first, to avoid duplicating them:
  - `rv-prh-i1-payout-code-review.md` (Re-review 3: APPROVE);
  - `rv-prh-i1-payout-ledger.md` (Re-review 2: APPROVE WITH CONDITIONS, R4 open);
  - `rv-prh-i1-callback-ledger.md` (re-review 1, incl. N3);
  - `rv-prh-i1-killswitch-security.md` (re-verification 2).
- In scope:
  - `internal/payments/payout.go`, `payout_sweep.go`;
  - the payout branches of `receipt.go` (R4 wiring, `158ac86`);
  - `claimBatch` in `sweeper.go`;
  - the kill-switch predicates in `attempt.go`;
  - `internal/withdrawal/withdrawal.go`: `LockApprovedForSubmission`, `DenyForCompliance`, `LockForPayoutEvidence`, `MarkSubmittedPending`, `AttachProviderReference`, `Complete`, `Fail`;
  - `internal/httpserver/withdrawal_handlers.go` (submit, `/resolve`) and `financial_routes.go`;
  - the payout tests.
- Out of scope:
  - the kill-switch authority model (reviewed separately; closed in its re-verification 2);
  - the deposit side of the callback cutover;
  - penetration testing.

  This review does not declare payout dispatch "secure" in general. It covers code- and design-level properties at `091ed1e`.

## Verdict

**APPROVE WITH CONDITIONS for the payout dispatch path (T1p, phase B, phase C, `/resolve`, and the T2/T12 sweeper logic) as currently wired.**

**R4 (payout callbacks through `receipt.go`) is NOT security-approved.** Payout-typed callbacks must stay refused until S-H1 and S-M1 are fixed.

The dispatch path holds on every question asked:
- authorization, including cross-tenant 404 with no effect;
- four-eyes cannot be bypassed through submit;
- staff audit in the same transaction;
- no raw vendor text in audit or logs;
- QueryStatus evidence cross-checks;
- the kill switch and the KYC gate fail closed at T1p, T2 and T12.

The two confirmed defects are both on the callback payout-evidence path (R4). That path is **not reachable from the network today**:
- `Orchestrator.ReceiveCallback` accepts only `deposit`/`deposit_reversal`;
- `MockProvider.HandleCallback` rejects any other `event_type`.

They therefore do not block marking the dispatch step complete. They **do block** enabling payout callbacks for any provider, and so they block launch for any payout provider that relies on webhooks.

## Findings by severity

### S-H1 — HIGH (latent; blocks enabling payout callbacks): a deposit-typed decline releases a payout's hold via the deferred-receipt backstop

This is a security escalation of ledger-finance callback N3 (rated Low there, by reading only). The probe below shows the concrete effect.

Path:
1. `ApplyReceiptEvidence` (`receipt.go` L489) calls `ApplyDeferredReceiptsForAttempt` after any receipt that changed an attempt, including a **payout** attempt.
2. That function selects deferred receipts with `event_type = 'deposit'` only.
3. It applies them through `applyResolvedReceiptEvidence` without checking `attempt.Operation`.

So the R4(b) cross-operation guard, which sits only in `ApplyReceiptEvidence`'s direct path, is bypassed.

Probe SP-B2 (private DB, direct `ApplyReceiptEvidence` calls, because the route does not accept payout events yet):
1. T1p claim for a 500 EUR payout.
2. A verified **deposit**-typed `declined` receipt with reference `R` arrives. No attempt holds `R`, so it is stored `deferred_unresolved`.
3. Phase C: `Withdraw` returns Pending with reference `R` (attempt `pending`).
4. A verified **payout**-typed `ambiguous` receipt for `R` (T11) changes the attempt, and the backstop replays step 2's receipt.

Result:
- attempt `declined`;
- **withdrawal `failed`, hold released to `player_cash`**;
- the deposit receipt is resolved `applied`.

Failure scenario: a provider whose pay-in and pay-out reference spaces overlap. This is common: separate sequences for deposits and payouts. It does not require a compromised provider. A stale deposit decline for id `12345` is applied to the payout that is later assigned `12345`. If the payout actually executed, the player is paid twice (the funds leave at the PSP and the hold returns to cash).

The success variant would complete a payout on deposit evidence.

Required:
- Enforce the event-type/operation match in **one** place that every evidence application passes through (`applyResolvedReceiptEvidence`, or the backstop's query keyed on `attempt.Operation`).
- Make it an **allow-list**: `deposit`→deposit, `payout`→payout. Today's check is two negative comparisons, so any other type (e.g. `payout_returned`) passes it for either operation.
- Resolve a mismatched deferred receipt as an anomaly.
- Add the SP-B2 test (decline variant: assert withdrawal stays `submitted`).

### S-M1 — MEDIUM (latent; blocks enabling payout callbacks): the N6 reference-mismatch rule is not applied to callback payout success

The QueryStatus path disputes a success that echoes a reference different from the one on file (N6, `applyPayoutSuccessCheckedFromStatus`). The receipt path calls `applyPayoutSuccess` directly. It checks amount and asset, but not the reference.

Probe SP-A:
- setup: attempt and withdrawal hold `X`;
- a verified payout success arrives, resolved by **merchant reference**, carrying `Y`, with the correct amount and asset;
- result: disposition `applied`, attempt `succeeded` (ref `X`), withdrawal **`completed`** (ref `X`), ledger `provider_tx_id = Y`.

The same evidence would be disputed if it arrived via `/resolve` or the sweeper. The settlement is recorded against a reference the platform never had on file, and reconciliation cannot match it to either reference column.

Required: move the stored-reference comparison into `applyPayoutSuccess` itself, so every evidence source (sync, QueryStatus, callback) shares one rule, or apply it in the receipt branch. Add a test.

### S-M2 — MEDIUM (required before the stage gate): the `/resolve` staff audit records neither outcome nor before/after state

`payoutResolveAudit` writes `withdrawal.resolve_attempted.http` with `Outcome: success` unconditionally and **no metadata**. The row is identical for all of these:
- a reschedule no-op;
- a provider-unreachable reschedule;
- a T6;
- a T10 dispute;
- a completion (`withdrawal.completed`, as system);
- a failure (`withdrawal.failed`, as system).

An investigator cannot tell from the staff row what the action did. The system rows carry no actor, and nothing links them to the staff row except timing.

CLAUDE.md requires "actor, tenant, entity, before/after state, IP, reason code". Actor, tenant, entity, IP, UA and request ID are present. Before/after is missing. The transaction placement is correct (N3 fixed).

Required: record `attempt_id`, the attempt and withdrawal state before and after, the evidence class, and `terminal_reason` when set. Set `Outcome` to reflect a dispute or no-op.

The submit audit (`withdrawal.submit.http`) is adequate:
- its action implies approved→submitted;
- it carries `attempt_id` and `provider_id`;
- the deny path records `OutcomeDenied` with the KYC code.

### S-L1 — LOW: no approver/beneficiary separation on submit or `/resolve`

`approverEligibilityCheck` checks linked and active only. Probe HP3: a `finance` staff account linked to the **same Person** as the withdrawing player got 200 on submit (approved→submitted) and 200 on `/resolve`.

The four-eyes approvals themselves were made by others (the `withdrawal_approvals` trigger blocks self-approval), so the amount and the approval are unaffected. What the beneficiary controls is dispatch timing and the payout rail (`payment_method` is staff-supplied). ADR 0024's separation principle should cover the payout-triggering act too.

Required before launch: pass the same `BeneficiaryCheck` used by `Approve` in both handlers (returning 403), and add a test.

### S-L2 — LOW: ruling on `lease_owner` (N7), plus a stale-reschedule path that relabels a live T12 lease

**Ruling.** `lease_owner` cannot be influenced by a client, a staff request body or provider evidence. Every write takes a server-side string literal:
- `payout-dispatch`, `player-request`;
- `sweeper-payout-reclaim`, `sweeper-payout-resubmit`, `player-request-cascade`;
- `sweeper` (both `claimBatch` and the deposit-only `drive.go`).

The column is tenant-scoped under RLS. The `'sweeper'` exemption is correct at `091ed1e`. I **agree with code review's hardening**: a distinct `sweeper-batch` constant, used by `claimBatch`, the Go check and the CAS. Required **before any binary wires the payout sweeper**.

**The mechanism the literal hides.** `claimBatch` selects on `next_action_at` alone and overwrites `lease_owner`/`lease_until` of whatever it claims. `RescheduleNonTerminal` has no state or lease predicate. So the exemption is only safe while nothing pulls a live `submitting` row's `next_action_at` ahead of its lease.

Probe SP-C shows one path that does:
1. `/resolve` reads the attempt as `ambiguous`.
2. A T12 resend commits: `submitting`, lease owned by `sweeper-payout-resubmit`, 2 min.
3. `/resolve`'s `PollPayoutStatus` runs on the stale snapshot and reschedules, so `next_action_at` falls 2 min before `lease_until`.
4. The next `RunOnce` claims the row, relabels it `sweeper`, and T6s it while the resend's phase B may still be running.

This is money-safe today:
- T12 runs only on `IdempotentSubmission=true` manifests, with the same key;
- phase C converges from `ambiguous`.

Hence LOW.

Required with the constant change:
- `claimBatch` must not claim a `submitting` row whose live lease is held by a non-batch owner;
- alternatively, `RescheduleNonTerminal` must never set `next_action_at` below a live `lease_until`.

### S-L3 — LOW: `/resolve` has no rate limiting or throttling (DoS and sweeper starvation)

Each call makes up to 60 s of outbound `QueryStatus` on a context detached from the request, so a client disconnect frees nothing. Each also runs 2–3 DB transactions and writes one audit row.

Every non-transitioning call also runs `RescheduleNonTerminal`, which sets `next_action_at = now+30s` and bumps `poll_count`. A finance user calling it repeatedly can therefore:
- defer the sweeper's T12 or escalation for that attempt indefinitely;
- push its backoff to the cap;
- overwrite the escalated cadence.

Blast radius:
- the breaker is keyed per (tenant, provider), and `callProvider` does not feed it, so there is no cross-tenant breaker effect;
- the role is finance-only.

Hence LOW (insider or compromised account).

Required before launch:
- a per-attempt minimum interval for staff polls (409/429);
- staff polls must not advance `next_action_at` or `poll_count` unless the state changes.

**Sweeper amplification is bounded:**
- `BatchPerTenant` per tick;
- at most `payoutMaxResubmits` (3) sends in total, and only for idempotent manifests;
- non-idempotent attempts never resend;
- escalated attempts keep polling at the capped backoff.

No finding there.

### S-L4 — LOW: T1p is not fully in ADR §5.2 phase-A order

- The staff eligibility check runs in a **separate** transaction before `ClaimForDispatch`. §5.2 step (1) places it inside the phase-A transaction. A staff account deactivated in that millisecond window can still claim.
- The §5.2 step (4) in-transaction capability re-read is still absent. A capability disabled between A0 routing and the T1p commit is still claimed.

The kill switch, which is evaluated inside the claim statement, remains the emergency stop. Move eligibility into the T1p transaction when `ClaimForDispatch` next changes.

### Test gaps (from mutation, required before the stage gate)

- **SM2a SURVIVED.** Removing the T12 Go-level `checkPayoutKillSwitch` passes the payout suite. The CAS predicate still refuses (SM2b is killed), so the payout is still fail-closed, but it then escalates (`resubmit_cas_refused`) instead of rescheduling. §27.11 item 3's classification is therefore untested at T12. Add a payout T12 kill-switch test, mirroring the T2 one.
- **SM7 SURVIVED.** Removing the linked-staff check from `/resolve` passes the httpserver suite. There is no unlinked or inactive-staff test for `/resolve`; submit has one.
- **SM10 SURVIVED.** Disabling the callback amount/asset cross-check passes the receipt, callback and payout tests. There is no payout-callback mismatch test. This belongs with S-H1/S-M1.
- **No cross-tenant test for submit or `/resolve`, and no role-denial test for `/resolve`.**
  - My probes HP1/HP2 show the behaviour is correct: 404 with zero attempts, zero audit rows and no state change; 403 for a player, `tenant_admin` and `support`.
  - These must become permanent tests: a tenant B token against tenant A's withdrawal must get 404 and cause no effect, on both routes.

### Informational and tracked launch conditions (not new findings)

- **No binary constructs a payout `Sweeper`.** `NewSweeper` has no non-test caller. In any deployment today, crash recovery, T2, T12 and escalation do not run. The submit handler's "it will be retried automatically" message is false until the sweeper is wired. Tracked with R5/CP-W1.
- **The payout destination is not modelled.** `WithdrawRequest` carries no destination, and `payment_method` is supplied by staff. Label: `PROVIDER DEPENDENT`. A real payout adapter must take the destination from a player-bound, verified instrument resolved server-side, never from the staff request body. This is a launch condition for any real payout adapter.
- **M3 has no route.** M3 is the release of a KYC-escalated `created` payout, so such a hold stays frozen. That is fail-closed and compliance-correct. Tracked with HD-0095-1.
- **Callback F2 applies to payout receipts too.** `payment_provider_events.decline_reason` stores raw vendor text, and more than 64 bytes causes a redelivery loop. Tracked in `rv-prh-i1-callback-code-review.md` F2.

## What holds (verified, not assumed)

| Question | Result | Evidence |
|---|---|---|
| 1. Permissions, tenant scoping, player reachability | Both routes: `RequireTenantScope` + `RequirePermission(PermWithdrawalSubmit)`, with the permission granted only to `finance`, + linked/active staff. The staff id comes from the token subject and the tenant from the token, never from the body. | Probe HP1: cross-tenant submit 404, withdrawal stays `approved`, 0 attempts; cross-tenant `/resolve` 404, audit count unchanged. Probe HP2: `/resolve` 403 for player, `tenant_admin`, `support`. Existing tests cover player and platform-admin denial on submit. |
| 1. Four-eyes cannot be bypassed via submit | Submit requires `approved` under an L1 lock (`LockApprovedForSubmission`, re-checked in `MarkSubmittedPending`'s CAS). Only `Approve` writes `approved` (grep). Distinct approvers and the threshold are enforced there and by the `withdrawal_approvals` trigger. Below the threshold, one approver plus the same person submitting is the configured policy, not a bypass. | Code reading. SM13 (submit accepting `pending_review`) was invalidated by the environment; see Method. |
| 2. Staff audit in the same transaction (B6/N3) | Submit (allow and KYC-deny) and `/resolve` audits are written inside the transaction that commits the effect, with actor, tenant, entity, IP, UA and request ID. Outcome and before/after on `/resolve`: see S-M2. | Code; code-review M5/N3 mutants killed. |
| 2. No raw vendor text in audit or logs | `canonicalDeclineReason` covers every payout decline path: phase C, status, and receipt via `applyPayoutDecline`. `providerref.Error` carries length and hash only. `callProvider` step 8 redacts transport errors. `gr.Err` is never logged or audited. The provider references in N6 audit metadata have passed `providerref` validation; only invalid values are forbidden in logs by the PROVIDER-REF-BOUND policy, and valid references already appear in `withdrawal.completed`. | Code; code-review M11 killed. |
| 3. Provider evidence trust | Sync success requires a validated reference; `WithdrawResult` echoes no amount, and the request is built from the attempt. QueryStatus is read-only, with a validated reference, an amount/asset check (T10) and a reference check (N6). Callbacks resolve only within the verified provider (INV-IO-14) and check amount and asset. The callback path has the S-H1 and S-M1 gaps and is not live. | Code; `orchestrator.go` event-type switch; `mock.go` `HandleCallback`. |
| 4. Kill switch fail-closed at T1p/T2/T12 | T1p: `NOT EXISTS` inside the `INSERT … SELECT`; 0 rows → `ErrPayoutKillSwitchEngaged` → the whole transaction rolls back → 503, state stays `approved`, no `Withdraw`. T2/T12: a Go pre-check reschedules, plus the CAS predicate. A DB error aborts the claim. | SM1 killed (2 tests), SM2b killed, SM3 killed; SM2a survived (classification only). |
| 5. KYC gate at T1p/T2/T12 and ordering | T1p: L1 lock → gate → `DenyForCompliance` (same transaction, hold reversal, `kyc_denied` key, `OutcomeDenied` staff audit) or claim. The two are mutually exclusive under L1. T2/T12: withdrawal lock → kill switch → gate → CAS; a deny escalates with no release; a repeated deny is idempotent. A gate error fails closed (503; rollback and retry next tick). **RG:** ADR 0095 §4.3 assigns no RG gate to payout dispatch (RG gates deposits only, T1/T2). The ordering question does not arise, and its absence is not a finding. | SM4 killed, SM5 killed (2 tests). |
| 6. `lease_owner` | Not influenceable. Ruling: see S-L2. | Code; SP-C. |
| 7. DoS | See S-L3. Sweeper amplification is bounded. | Code. |

## Method

- Detached worktree at `091ed1e` and a private database `secpay_i1_payout` (via `priv_db.sh`, migrated to head).
- Baseline: the payout, sweeper, claim and poll subset of `internal/payments` and the withdrawal and financial subset of `internal/httpserver` both pass.
- Probes, run and not committed:
  - `internal/payments`: SP-A, SP-B (not reproduced as first written: the L489 call passes the pre-change attempt copy, so a reference first learned via callback does not replay), SP-B2 (confirmed), SP-C (confirmed);
  - `internal/httpserver`: HP1, HP2, HP3.
- Mutation: 13 anchored mutants, each reverted with `git checkout`; tree confirmed clean.
  - Valid results: SM1, SM2b, SM3, SM4, SM5, SM6 killed; SM2a, SM7, SM10 survived.
  - **SM11–SM14 are invalid, not run.** Partway through the run, another session reset the shared Postgres role passwords. Every test then failed at connect in 0.00 s: the payments harness reported `password authentication failed for user "igaming_test_admin"`, and a direct `psql` as the DB owner (`igaming`) was refused too. SM6 was killed by its specific test in 0.62 s, and SM7/SM10 then passed their suites, which confirms the DB was reachable up to SM10. SM11–SM14 covered the resolve and submit audit ActorID, the four-eyes `pending_review` bypass, and the deny-audit outcome. The audit ones overlap code review's killed M5/N3 mutants. SM13 needs a re-run.
- **Cleanup:**
  - The worktree has been removed.
  - **The private database `secpay_i1_payout` could NOT be dropped.** The admin credential in `priv_db.sh` is no longer accepted, and I did not seek other credentials. It holds only synthetic fixture data. Whoever reset the passwords should drop it: `DROP DATABASE secpay_i1_payout WITH (FORCE)`.

## Conditions

**To mark PRH-I1 payout dispatch complete (security):**
1. S-M2: the `/resolve` audit records outcome, before/after state and `attempt_id`.
2. The test gaps above: a payout T12 kill-switch test (SM2a); an unlinked/inactive-staff test on `/resolve` (SM7); permanent cross-tenant 404 tests on submit and `/resolve`; a role-denial test on `/resolve`.
3. Re-run SM13 (the four-eyes bypass mutant) on a working database and confirm it is killed; if it survives, add the test.

**Before payout callbacks are enabled for any provider (blocks R4 closure):**
- S-H1, with the SP-B2 test;
- S-M1, with the SP-A test;
- SM10 pinned;
- callback F2.

**Before the payout sweeper is wired into any binary:**
- S-L2: the `sweeper-batch` constant, and no relabelling of a live non-batch lease.

**Before production launch (payouts):**
- S-L1: beneficiary separation on submit and `/resolve`;
- S-L3: `/resolve` throttling;
- S-L4: eligibility inside the T1p transaction;
- the destination-binding condition;
- the tracked R5/CP-W1 and HD-0095-1 items.

Launch authorization itself remains the human's decision.

---

# Re-verification 1 — payout-callback findings on the callback branch (`worktree-agent-adce273a3a5339f77` @ `0a96a01`)

- Date: 2026-09-27.
- Scope: the callback agent's payment commits on that branch (`be15a11`, `7641332`, `dd44d04`, `0a96a01`): `receipt.go`, `drive.go`, `sweeper.go`, `cascade.go`, and small edits in `orchestrator.go`/`types.go`.
- Checked against the claims: S-H1 closed, S-M1 closed (pinned by the branch's "SM10" test), R4(a)–(d) intact, and the new reversal pending/ambiguous rule plus decline-reason bounding.
- Method:
  - detached worktree at `0a96a01`; private DB `secpay_i1_cb` via `priv_db.sh`, with the harness's configured credentials only (no role or password changes);
  - probes re-run, then removed;
  - 9 anchored mutants against the payments receipt/callback/payout/reversal/deferred subset (baseline green). Each was reverted and the tree confirmed clean. The three first-pass results that looked like environment noise were re-run one at a time.
- Cleanup:
  - the worktree has been removed;
  - `secpay_i1_cb` **and** the round-1 leftover `secpay_i1_payout` have both been dropped (0 `secpay%` databases remain).

## Verdict

**The payout-callback path (R4) is APPROVED WITH CONDITIONS from `security`.** S-H1 and S-M1 are closed. The remaining conditions are test pinning and a review of the future route/adapter wiring. No finding blocks it.

This is not an enablement decision:
- payout-typed callbacks are still refused at the route (`ReceiveCallback` accepts only `deposit`/`deposit_reversal`);
- no adapter emits them (**PROVIDER DEPENDENT**).

## Results

| Item | Result | Evidence |
|---|---|---|
| **S-H1** (deferred deposit receipt replayed onto a payout) | **CLOSED** | `ApplyDeferredReceiptsForAttempt` now derives the `event_type` filter from `attempt.Operation` (unknown operation → no replay). Probe SP-B2: the withdrawal stays `submitted`, the attempt goes to `ambiguous` (from the payout-typed evidence only), and the deposit-typed receipt is not applied. The merchant-reference-T4 variant (SP-B), which became reachable once the stale-copy re-read was fixed, is also safe. Mutant MH1 (filter reverted to `'deposit'`) is KILLED by `TestRVLF_N3_DeferredApplyNeverReplaysADepositDeclineAsPayoutEvidence`. MH1b (filter removed) is KILLED (6+ tests). |
| **S-M1** (callback success with a conflicting reference) | **CLOSED** | Probe SP-A: attempt `disputed` (`provider_reference_mismatch`), withdrawal `submitted`, reference unchanged, ledger balanced, mismatch audit written. MM1 (check disabled) is KILLED by `TestRVLF_SM10_PayoutSuccessProviderReferenceMismatchDisputes`. **MM1b SURVIVED**: removing the fallback to the withdrawal's reference passes, so the attempt-reference-NULL shape is unpinned. |
| My SM10 (callback amount/asset check disabled) | **KILLED** | `TestRVLF_P4_MismatchedSuccessOnTerminalAttempt` |
| R4(b) allow-list (unknown `event_type` → anomaly) | **Present, but unpinned** | **MA SURVIVED**: reverting to the old deny-list passes the subset. The first-pass "kill" was environmental; the single re-run exited 0. It is defence in depth while the route rejects unknown types. |
| R4(a)–(d) | Intact | The payout decline and success branches still go through `applyPayoutDecline`/`applyPayoutSuccess` (the success branch now behind the S-M1 check). Baseline subset green. |
| Reversal pending/ambiguous (ledger H1 rule 2) | Correct | Stored as an anomaly under the real wire outcome, resolved, P1-audited; no post, no tombstone, no lock. MR is KILLED by `TestRVLF_P1_NonFinalReversalOutcomeNeverPosts`. **MRF SURVIVED**: the rule-3 fingerprint over the raw outcome (`RawOutcome`) is unpinned. The effect is only whether a `succeeded` and a `declined`-carrier delivery of the same reversal dedupe to one. That is not a double-post: the reversal posting keeps its own idempotency. |
| Decline-reason bounding before persistence | Correct; callback F2 closed from `security`'s side | Bounded at the top of `ApplyReceiptEvidence`, **after** `validateReceiptReferences`, so the audit `target_id` built from the reference is already validated. An oversized reason writes a redacted audit entry (byte length plus a 16-hex SHA-256 prefix, never the text). The same applies in `drive.go`/`sweeper.go`. MD is KILLED by 3 tests (`TestRVLF_F2_*`, `TestRVLF2_Q3_*`). |
| Webhook tenant binding, verification, idempotency, replay | **Not weakened** | No change to `internal/httpserver`, webhook signature verification, `ReceiveVerifiedCallback`'s provider/tenant binding, `ResolveAttemptForEvidence` (INV-IO-14), or `validateReceiptReferences`. Receipt dedup is unchanged, `ON CONFLICT (tenant_id, provider_id, event_fingerprint)`. The reversal branch now inserts R0 before the lock and returns early on a duplicate. That is safe: a committed first delivery committed its effect atomically, and a concurrent duplicate waits on the unique index. The fingerprint change affects `deposit_reversal` only. The httpserver `Webhook\|Callback\|Deposit\|Withdrawal` tests pass on the private DB. |

## Residuals (LOW, non-blocking)

- **Pin MA and MM1b.** Add two tests:
  - an unknown `event_type` (e.g. `payout_returned`) resolving to either operation → anomaly with no effect;
  - a payout success-reference mismatch where the attempt reference is NULL but the withdrawal's is set → dispute.
- **Pin MRF**, preferably in the same change.
- **A cross-operation deferred receipt is left unresolved forever**, not resolved as an anomaly. It is skipped by the operation filter and counts toward the 10 000 per-(tenant, provider) deferred cap. Resolve it as `anomaly_other` when an attempt of the other operation learns the reference.
- **The sync phase-C success path still lacks the reference rule.** `ApplyPayoutResult`'s `ErrorClassSucceeded` calls `applyPayoutSuccess` without the S-M1 comparison: a T12 resend returning a different reference settles against it. That evidence comes from our own idempotent request, so the risk is low. Moving the comparison into `applyPayoutSuccess`, as recommended in S-M1, would cover every source with one rule.
- **Informational:**
  - a payout `pending` receipt (T4) records the reference on the attempt but not on the withdrawal;
  - SP-A's dispute returns disposition `applied` (the response body is disposition-only, per §6.2).

## Conditions still standing before payout callbacks are enabled for a provider

1. Pin MA and MM1b (above).
2. A `security` review of the change that makes the route and an adapter accept `payout` events. That wiring does not exist yet and is where tenant binding and verification for payout webhooks will actually be exercised.
3. The other reviewers' open conditions on the callback cutover (ledger H1-R etc.) are theirs, not decided here.

The S-L1–S-L4 dispatch conditions from the original review are unchanged.
