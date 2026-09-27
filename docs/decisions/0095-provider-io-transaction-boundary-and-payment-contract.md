# ADR 0095 — Provider-I/O Transaction Boundary and Provider-Neutral Payment Contract

- **Status:** **ACCEPTED (design) — NOT IMPLEMENTED** (revision 2, 2026-09-27).
  - Revision 1 was PROPOSED. Six reviews followed (§21 ledger-finance, §22 security, §23
    payments, §24 QA, §25 identity-compliance, §26 casino).
  - Revision 2 writes every condition into the design text. §27 maps each condition ID to the
    section that satisfies it; none is dropped.
  - The acceptance bar set by the orchestrator is met in text: ledger-finance LF95-C1..C12, and
    the launch-blocking security conditions S95-C1, C5, C6, C8 and C13.
  - LF95-C13/C14 and QA changes 1–8 gate `IMPLEMENTED`, not acceptance; they are specified in
    §12, §16 and §18.
  - Acceptance of the design is not a claim that anything is built, secure or launch-ready.
    Every deliverable below stays `NOT IMPLEMENTED` until an implementing change lands and passes
    its own reviews.
- **Decision type:** architecture, cross-domain. It touches `payments`, `withdrawal`, `ledger`
  (lock-class inventory only), `reconciliation`, `providercred`, `casino` (launch only), `kyc`
  (create and submit only), `httpserver` and `cmd/platform-api`.
- **Owner:** `architect`.
- **Sign-off required before implementation:**
  - `ledger-finance`: mandatory, per LF-C2 (review 19 §4). Covers §4, §6, §7, §12, §14 and §16.
  - `security`: covers §9.4, §10, §11 and §6.5.
  - `payments`: covers §4 through §12.
  - `casino` and `identity-compliance`: cover §15 only.
  - `qa`: covers §16.
- **Migration numbers (orchestrator re-allocation, 2026-09-27):** 0101 payment attempts and
  receipts (was 0100; 0100 is now ADR 0096's KYC enforcement migration), 0102 kill switch, 0103
  payment statement reconciliation.
- **Registry rows closed by implementing this ADR** (each one per domain, only after review):
  - F-POOL-2;
  - the "not implemented" half of PROV-OUTBOUND-CRED-1;
  - PRH-D1 (this document).
- **Trigger:**
  - Human instruction "Payment Readiness & Provider-Independent Hardening" (PRH), 2026-09-27.
  - F-POOL-2, including ledger-finance's High (P1-class) dual-write sub-finding.
  - Security W2A-SEC-1, the launch-blocking precondition on PROV-OUTBOUND-CRED-1.
- **Human requirements, restated as binding:**
  - No DB transaction is held across any external provider call.
  - Financial correctness does not depend on a long transaction.
  - Correctness comes from idempotency plus durable state.
  - The platform ledger stays authoritative. The provider is never the source of truth.
  - The sequence "tx → provider call succeeds → tx rolls back → customer charged, platform
    unaware" must be impossible.
- **Amends (proposed; each amended owner must accept its own part):**
  - **ADR 0022 §2 and §6.** The `PaymentProvider` interface and canonical shapes change (§9).
    The adapter-declared capability layer gains an operation manifest (§10). The conformance
    suite gains cases (§16).
  - **ADR 0082 §2.1.** Amendment A7, owned by `ledger-finance`: accepted in §21.3 with the
    LF95-C9 scope rules (§14), and written into ADR 0082 by `ledger-finance` now that this ADR is
    ACCEPTED. It adds two L1 tables and places the callback-receipt insert (R0) between L0 and L1.
  - **ADR 0093 §5.** The resolution point and the adapter request types are fixed (§11).
  - **`docs/architecture/reconciliation-model.md` §2.2.** Mismatch state (b) no longer auto-posts
    from a statement line (§12.6).
  - **`docs/architecture/withdrawal-state-machine.md`.** `submitted` now means "dispatch claimed;
    the provider may have received it". `provider_reference` may be NULL while `submitted`
    (§4.7).
  - **`docs/architecture/payment-orchestration.md` §5.** Cascade from an *asynchronous* decline
    is limited to non-interactive methods (§4.6).
- **Depends on:**
  - **PROVIDER-REF-BOUND-1 / migration 0099** (`ledger-finance`, running in parallel). Every
    provider-reference column this ADR creates carries a CHECK against the platform maximum
    that 0099 defines. This ADR refers to that value as `PROVIDER_REF_MAX` and does not choose it.
    Migration 0101 (payment attempts; renumbered from 0100 by the orchestrator, §27) must not merge before 0099.
  - **ADR 0097** (webhook admission/rate limiting). Its order is admission → verification →
    binding → parsing → domain. §6 only changes the *domain* step.
- **Not in scope:**
  - any real vendor, vendor signature scheme or invented vendor behaviour;
  - AWS, IAM, IRSA, KMS or a proxy;
  - KYC enforcement (ADR 0096);
  - webhook rate limiting (ADR 0097);
  - outbound refund initiation (the contract is defined, but no flow uses it, §5.5);
  - payout cascade;
  - crypto custody (ADR 0008).

---

## 0. Decisions at a glance

| # | Decision |
|---|---|
| D1 | **Commit-intent → call-without-tx → commit-evidence.** Every external operation runs as: a short transaction that durably commits the *intent to call*; the provider call with **no transaction and no pooled connection held**; a short transaction that applies the *evidence* returned, under a compare-and-set (CAS) guard. Correctness comes from durable state, CAS and DB unique constraints. It never comes from holding a transaction open. |
| D2 | **One state machine for every outbound money operation.** Deposits and payouts both run through `payment_attempts` rows (migration 0101). There are 8 states, 4 of them non-terminal, and every transition is CAS-guarded, audited and backstopped by a DB trigger (§4). |
| D3 | **The attempt row is the outbox.** A cascade, a retry or a recovery is a committed `payment_attempts` row with `next_action_at`. A bounded worker (the sweeper) drives these rows with leases and `FOR UPDATE SKIP LOCKED` claims (§7). No provider I/O happens inside a webhook transaction. |
| D4 | **Deterministic external identity.** The merchant reference and the external idempotency key are both derived from the committed attempt id. They are never generated per try (§5). |
| D5 | **Evidence decides; the absence of evidence never does.** Success is applied only on verified, amount- and asset-matching evidence that names a provider reference, from the provider the attempt was sent to. Timeout, not-found and ambiguous results are never treated as failure for a payout, and never as success for anything. A deposit may be declined on "not found" only when the adapter declares that lookup authoritative and the provider never acknowledged the attempt (§4.5). |
| D6 | **Callbacks resolve by merchant reference and by provider reference, always bound to the verified provider.** They are accepted in any non-terminal state and durably receipted (`payment_provider_events`). A callback that cannot be resolved yet is deferred (capped), then applied when the attempt learns its reference, but only if it was received after the attempt was first sent (§6). |
| D7 | **A provider-neutral payment contract** (§9) with: `CallContext` (tenant plus resolved credential plus idempotency key plus deadline); a closed `ErrorClass` for retry and timeout classification; `StatusQuery` by either reference; and a code-declared `OperationManifest`. The MOCK adapter only exercises the state machine. |
| D8 | **Capability manifest and payment kill switch** (migration 0102, §10). Unsupported operations fail closed, first at registration and then at new activity, never at settlement. The kill switch is server-authoritative, tenant- and provider-scoped, audited, fail-safe, and evaluated atomically in the claim transaction. |
| D9 | **PROV-OUTBOUND-CRED-1** (§11). The credential is resolved per call, outside any transaction, and passed in `CallContext`. The adapter verifies the tenant and provider binding. No credential is held in an adapter. Credential access is refused while a transaction or a financial lock is held. |
| D10 | **Payment statement reconciliation, MOCK source** (migration 0103, §12). The statement is fetched outside any transaction, stored append-only, and matched in a REPEATABLE READ transaction. There are 8 mismatch kinds. The stream is detection only: remediation happens only through the evidence path or LEDGER-MANUAL-ADJ-4EYES-1. |
| D11 | **Casino launch and KYC create/submit get the same boundary with minimal redesign** (§15). No migration, no new state, no change to their financial or enforcement semantics. |

---

## 1. Context — where external I/O is inside a transaction today

Verified at HEAD `1560ad0`. Every adapter is an in-process MOCK, so none of these hazards is
reachable today (tripwire `TestOutboundPrecondition_EveryWiredAdapterIsSynthetic`).

| # | Site | What is held during the call | Hazard once non-MOCK |
|---|---|---|---|
| 1 | `payments.InitiateDeposit` → `attemptDeposit` → `provider.Deposit` (`internal/payments/orchestrator.go:595`) | The tenant tx (from `deposit_handlers.go:138`) holds the uncommitted `deposit_intents` INSERT, the audit rows, and RG advisory locks (L0.4) | Dual write (review 19 §6): the PSP accepts, then the tx rolls back, so the player is charged but not credited. A retry mints a new intent, so the player is charged twice. Pool pinning (F-POOL-2). |
| 2 | `resolveAmbiguous` → `provider.QueryStatus` (`:678`), repeated through cascade up to `MaxCascadeDepth` | Same tx | Same, multiplied by the cascade depth |
| 3 | Webhook `receiveDepositCallback` → `handleDecline` → `attemptDeposit` → `provider.Deposit`; `resolveAmbiguous` → `QueryStatus` (`:994-996`) | The webhook domain tx | PSP I/O inside the webhook tx. On rollback the PSP redelivers and the cascade fires again (a second charge at the next PSP). |
| 4 | `RouteProvider` → `provider.HealthStatus` (`:207`) | Every caller's tx | Pool pinning. Health lookups are I/O for a real adapter. |
| 5 | Withdrawal submit: `LockApprovedForSubmission` (L1 `FOR UPDATE`) → `provider.Withdraw` → `MarkSubmitted` (`internal/httpserver/withdrawal_handlers.go:827-850`) | The L1 row lock on `withdrawal_requests`, plus the tx | **Double payout.** The payout is sent, then the tx rolls back and the state is still `approved`, so a staff retry sends it again (LF-C2 #9). The L1 lock is also held across vendor latency. |
| 6 | Withdrawal resolve: `LockSubmittedForResolution` → `provider.QueryStatus` (`:1031-1047`) | The L1 row lock plus the tx | Pool pinning and a held L1 lock |
| 7 | `casino.LaunchGame` → `CreateLaunchSession(tx)` → `provider.Launch` (`internal/casino/orchestrator.go:320-340`), plus `HealthStatus` | The tenant tx, plus the RG (L0.4) and risk (L0.5) advisory locks | Pool pinning. The orphan vendor session moves no money (review 19). |
| 8 | `kyc.CreateVerification` (`internal/kyc/verification_service.go:95`) | The tenant tx | Pool pinning. An orphan vendor verification. No money path. |
| 9 | `kyc.submitVerificationDocuments` → `SubmitVerification` (`internal/kyc/document_service.go:201`) | The upload tx | Pool pinning. A lost submission result. |

Two further structural gaps this ADR also closes:

- **Cascade history.** `deposit_intents` stores only the last attempt, so
  `providerExclusionSoFar` (`orchestrator.go:951`) cannot exclude earlier providers after an
  asynchronous callback.
- **Non-deduplicating MOCK.** The MOCK mints a fresh reference on every `Deposit`/`Withdraw`
  call (`mock.go:346`). It cannot model provider-side idempotency on our key, so no test today
  can prove that a same-attempt retry is deduplicated.

---

## 2. Invariants (the checkable contract for `qa` and `code-reviewer`)

| ID | Invariant | Enforced by |
|---|---|---|
| **INV-IO-1** | No adapter outbound method (`Deposit`, `Withdraw`, `QueryStatus`, a statement fetch, `Launch`, `CreateVerification`, `SubmitVerification`, and any future outbound method) runs while the calling goroutine's context holds a pooled DB transaction. | (a) **API shape.** No function that can reach an adapter outbound method takes a `pgx.Tx`, and none is called from inside a `db.Pool.With*` callback. (b) **Runtime.** The provider-call gate (§3.2) refuses under `txscope.Held(ctx)` and returns `ErrorClassNotSent` without calling. (c) **Static test.** A source scan (in the spirit of `lockorder_static_test`) fails on an adapter-method call lexically inside a `With*` closure. (d) **Adversarial test.** The capture-style tests in §16. |
| **INV-IO-2** | Every money-moving outbound call (deposit submission, payout submission, and refund when built) is preceded by a **committed** `payment_attempts` row in state `submitting` that carries the provider id, merchant reference and external idempotency key used for the call. | The CAS `created→submitting` (or the payout claim) commits before the gate lets the call run. The gate takes the attempt's *committed* claim token and refuses without one. |
| **INV-IO-3** | The merchant reference and external idempotency key are pure functions of `payment_attempts.id`. They are persisted, and they are identical on every send of that attempt. | Immutable columns (trigger); `UNIQUE (tenant_id, merchant_reference)`; `UNIQUE (tenant_id, external_idempotency_key)`. |
| **INV-IO-4** | Every state change of an attempt is one CAS `UPDATE … WHERE id = $1 AND state = ANY($allowed_from)`, in the same tx as its audit record and its ledger effect (if any). A DB trigger rejects any (OLD, NEW) pair not in §4.3 and any change to an immutable column. | CAS in code; `payment_attempts_guard` trigger (0101). |
| **INV-IO-5** | Ledger posting never spans external I/O. Each posting runs in one short domain tx holding the ADR 0082 locks in class order, with the authoritative balance read in that same tx. | The D1 pattern; ADR 0082 A7 (§14); existing `ledger.Post`. |
| **INV-IO-6** | No success without evidence. `succeeded` is entered only by a verified callback, a `QueryStatus` result or a synchronous result reporting a definite success **whose amount and asset equal the attempt's**. A mismatch goes to `disputed`, never to `succeeded`. | `applyEvidence` (§4.4) is the only function that writes `succeeded`; trigger. |
| **INV-IO-7** | No payout failure without definite decline evidence. `withdrawal.Fail` (the hold release) is reachable from a dispatched payout only through `declined` evidence from the provider. Timeout, not-found, ambiguous, a KYC outcome and operator impatience are never enough. The only other release of a claimed payout's hold is M3, from `created` (never sent, INV-IO-9). | §4.5 asymmetry. The `payment_attempts.last_evidence_kind` column is written in the same UPDATE as every state change, and the guard trigger reads it (§13.1): `*→declined` for a payout requires `last_evidence_kind ∈ {sync, callback, query_status}`; any `→declined` for a deposit requires `≠ operator`; `→succeeded` requires `∈ {sync, callback, query_status}`. |
| **INV-IO-8** | At most one live (non-terminal) attempt per deposit intent, and exactly one payout attempt per withdrawal request. | Partial unique indexes (0101). |
| **INV-IO-9** | An attempt in `created` has never had a call that may have reached the provider (`ever_possibly_sent = false`). | `CHECK (state <> 'created' OR NOT ever_possibly_sent)`; `submitting→created` only on `ErrorClassNotSent`. |
| **INV-IO-10** | A verified callback is never lost. It is durably receipted in the same tx as its effect (or its deferral). If that tx fails, the response is retryable (5xx), so the provider redelivers. | `payment_provider_events` (0101); §6. |
| **INV-IO-11** | Provider credentials are resolved outside any tx, per call, and never while a financial lock is held. They are never stored in an adapter, client or cache, apart from `DerivedTokenCache`. | §11; `OutboundResolver.Resolve` `txscope` refusal (exists); adapter-field reflection test (§16). |
| **INV-IO-12** | Reconciliation never writes the ledger, a projection, an attempt, an intent or a withdrawal. It writes only run, mismatch and statement-import rows. | Stream code; statement-capture test (§16). |
| **INV-IO-13** | After every failure-injection test: `SUM(debits) == SUM(credits)` and projection == rebuild (LF-C2 #8). | Shared test helper, reused from the existing suites. |
| **INV-IO-14** | Provider binding. Evidence carried by a callback, a status result or a statement line from provider P can change, post against, dispute or match only attempts whose `provider_id = P`. P always comes from the verified identity (route plus verified credential), never from a payload. | The resolution predicate `attempt.provider_id = $verified_provider` in every lookup (§6.1, §6.4, §7, §12.3); cross-provider matches are `anomaly` receipts with no state change (S95-C1). Test plus mutation MX15. |
| **INV-IO-15** | The kill switch cannot be bypassed. A claim (T2, T1p, T12) succeeds only if no engaged switch covers it, evaluated inside the claim statement under the same tenant context. A switch can be released only through an approved four-eyes release request. | `NOT EXISTS` predicate inside the CAS statement (§10.3); `payment_kill_switches_guard` trigger (§13.2). Tests §16.2 item 12. |

---

## 3. Execution pattern

### 3.1 The three phases

```
 phase A (short tx, WithTenant)        phase B (NO tx, NO pooled conn)            phase C (short tx, WithTenant)
 ─────────────────────────────         ──────────────────────────────────         ────────────────────────────────────
 validate, RG/risk, kill switch,       resolve outbound credential (§11)          lock (ADR 0082 order) + CAS attempt
 insert/claim attempt (CAS),           provider-call gate (§3.2)                  applyEvidence (§4.4): state, ledger,
 audit "…requested/claimed"            adapter call with CallContext               withdrawal Complete/Fail, cascade row,
 COMMIT  ◄── durable intent to call    classify result → Evidence | ErrorClass     deferred receipts, audit
                                                                                  COMMIT
```

- Phase A commits **before** phase B starts. If phase A fails, no call is made.
- Phase B runs holding no transaction, no pooled connection, no row lock and no advisory lock.
  Session-level advisory locks are forbidden anywhere on this path.
- Phase C is idempotent and may be retried at any time. The CAS makes a repeat a no-op. For
  example, after a connection lost during COMMIT, re-running phase C finds the state already
  moved.
- If phase C never runs (crash, timeout, deploy), the committed phase-A state plus the sweeper
  (§7) and callbacks (§6) converge the attempt. Nothing depends on the original goroutine
  finishing.

### 3.2 Provider-call gate (one place per domain)

`callProvider(ctx, attempt, fn)` is the only path to an adapter outbound method in payments.
It:

1. **Refuses under `txscope.Held(ctx)`.** It returns `ErrorClassNotSent`, logs
   `provider_call_refused_tx_held`, and does not call. This is defence in depth: the primary
   control is API shape.
2. Refuses a money-moving call unless the attempt was loaded **after** the claim commit, with
   `state = submitting` and a matching `claim_token`.
3. Resolves the credential (§11) and builds `CallContext`. The `CallContext` tenant is taken only
   from the attempt row returned by *this* tenant's claim transaction, never from a payload, a
   cache or an earlier attempt (S95-C9(i)).
4. **Checks the credential binding itself** (S95-C8(b)): `Credential.TenantID == CallContext.TenantID`,
   `Credential.ProviderID == CallContext.ProviderID` and `Credential.Domain == <caller domain>`
   (`payments`, `casino` or `kyc`). A mismatch is `NotSent` with no call. The adapter repeats the
   check (§9.1), so neither check alone is load-bearing.
5. Applies the per-call deadline: `min(ctx deadline, resolve budget + manifest call timeout)`.
6. Calls the adapter.
7. Maps panics and unknown errors to `ErrorClassAmbiguous` for money-moving operations, and to
   `ErrorClassNotSent` only if the adapter provably did not dispatch.
8. **Redacts transport errors** (S95-C8(a)). A transport error (including Go's `*url.Error`,
   which embeds the request URL) is mapped to `ErrorClass` plus an allow-listed reason code
   before it leaves the gate. Where a URL is logged at all, it is logged without its query
   string and userinfo. Vendor response bodies never reach logs, errors, audit or receipts.
9. Feeds the orchestrator-owned breaker (§9.6).

Casino and KYC use the same gate shape (steps 1, 3, 4, 5, 6 and 8) through a small shared helper.
They do not import payments.

---

## 4. Payment attempt state machine (deposits and payouts)

### 4.1 States

| State | Meaning | Terminal? | Provider may hold it? |
|---|---|---|---|
| `created` | Durable intent to submit. No call that may have reached the provider has ever been made (INV-IO-9). `provider_id` may be NULL (not yet routed; cascade rows). Reached by cascade (T1), by a `NotSent` revert (T5), and by nothing else. | no | **no** |
| `submitting` | Claimed for a provider call. The call may be in flight or done. The provider **may** have received it. | no | yes |
| `pending` | The provider acknowledged receipt and assigned a `provider_reference`; the outcome is not final. **"Accepted" ≡ `pending` with `accepted_at` set.** Provider-neutrally, the platform cannot distinguish "accepted" from "pending", so they are one state. | no | yes |
| `ambiguous` | Outcome unknown: a timeout or transport failure after a possible send, a non-definitive `QueryStatus`, a lookup that is not authoritative, or success evidence without a provider reference. **Never a failure, never a success.** | no | yes |
| `succeeded` | Definite success on verified, amount- and asset-matching evidence that names a provider reference. The ledger effect is posted in the same tx (deposit: Flow 1, linked on `payment_attempts.ledger_transaction_id`; payout: Flow 3 Step B via `withdrawal.Complete`). | yes | yes |
| `declined` | Definite negative from the provider (sync decline, verified decline callback, `QueryStatus` declined, or an authoritative not-found for a deposit, §4.5). This covers the human's "**failed**" (`decline_stage = after_acceptance`) and a provider-side "rejected at submission" (`decline_stage = at_submission`). Terminal for automation. Only verified success evidence can override it (§4.4, anomaly). | yes* | yes |
| `rejected` | Platform-side refusal **before any call that may have reached the provider**. Reached only from `created`: kill switch (deposits), unsupported capability, no routable provider, credential unavailable after retries are exhausted, the interactive presence window expiring, a sibling attempt having succeeded (`intent_succeeded`), or M3. | yes | **no** |
| `disputed` | Contradictory or mismatched evidence that automation must not resolve. Examples: a success with a different amount or asset; success evidence (from the attempt's own provider) for a `rejected` or `created` attempt; a success for a declined payout; a provider-reference conflict; a reversal tombstone that precedes the success (`reversal_tombstone_precedes_success`). It is a P1. Exit is manual only (§4.8). | yes | yes |

\* `declined → succeeded` exists only for deposits, only on verified matching success
evidence, and always raises P1 `contradictory_provider_outcome` (§4.4, T13).

Mapping of the human's vocabulary:

| Term | Where it lives |
|---|---|
| request | the intent row plus attempt 1 (T1+T2 in one phase-A tx for the player path; T1 alone for a cascade row) |
| provider submission | `created→submitting` (T2), or the combined player/payout claim |
| accepted | `pending` (`accepted_at`) |
| pending | `pending` |
| succeeded | `succeeded` |
| failed | `declined` (`after_acceptance`); withdrawal `failed` |
| rejected | `rejected`; withdrawal `rejected` via `DenyForCompliance` (pre-dispatch, W-KYC) |
| timeout | call timeout → `ambiguous` (T6); settlement timeout → escalation, **no state change** (T16) |
| ambiguous provider result | `ambiguous` |
| retry | T5 (NotSent), T12 (idempotent resubmit of the same key), player retry resume (§5.1) |
| duplicate callback | receipt dedupe plus no-op evidence (§6.3) |
| callback after timeout | `ambiguous→*` (T7–T9); `declined→succeeded` anomaly (T13) |
| reconciliation | §12 (detection) plus T17 re-verify (operator or the payments re-drive job, §12.5) |
| manual intervention | M1–M3 (§4.8) |

### 4.2 Columns that make the machine deterministic

- `ever_possibly_sent`: set to true, permanently, the first time a call's classification is
  anything other than `NotSent`. Set in phase C, so a `submitting` attempt with the flag still
  false is **not** proof that nothing was sent (CP-W1).
- `submit_count`: bounded by the manifest's `max_resubmits`.
- `claim_token` and `lease_until`: who may call, and until when.
- `first_submitted_at` (set once, by the first claim, from the DB clock) and `last_sent_at` (set
  by every claim, T2/T1p/T12). `first_submitted_at` gates deferred receipts (S95-C3);
  `last_sent_at` measures the not-found window (LF95-C8(b)).
- `next_action_at`: when the sweeper should look. NULL when terminal.
- `last_evidence_kind`, one of `sync | callback | query_status | sweeper | operator | platform`,
  written in the **same UPDATE** as every state change and read by the guard trigger
  (LF95-C2). The transition's audit record carries the same value.
- `interactive`: from the manifest. True means a player must be present to use the result, for
  example a redirect URL.
- `legacy_backfill`: true only for rows created by the 0101 backfill (§13.1). T12 is forbidden on
  them (trigger), because the provider never received a `pa:<id>` key (LF95-C11(c)).

### 4.3 Transition table (the complete set; anything else is rejected by code and by trigger)

The "Allowed from" column is exactly the CAS predicate. Every transition sets
`last_evidence_kind` and writes its audit record in the same tx. Every tx that changes a
`payment_attempts` row first locks its parent (`deposit_intents` or `withdrawal_requests`) with
`FOR UPDATE`, except the sweeper's lease-only batch claim (§7.2, §14).

| T | From → To | Trigger / evidence | Performed by | CAS guard (in addition to `id = $1`) | Ledger / domain effect (same tx) | Audit action |
|---|---|---|---|---|---|---|
| T1 | ∅ → `created` | Cascade (attempt n+1, §4.6), committed in the tx that commits the previous attempt's decline | The callback, sweeper or player-request tx that commits the decline | INSERT; the partial unique index enforces one live attempt per intent | none | `payment.attempt_created` |
| T1+T2 | ∅ → `submitting` | Player path: `InitiateDeposit` phase A inserts the intent and attempt 1 and claims it in **one** tx, after RG, the KYC deposit gate (ADR 0096) and the in-statement kill-switch predicate | Player-request driver | INSERT with `state='submitting'` guarded by the same predicates as T2 (the kill switch is evaluated in the INSERT … SELECT … WHERE NOT EXISTS statement) | Intent inserted, status `pending` | `deposit.requested`, `payment.attempt_claimed` |
| T1p | ∅ → `submitting` | Payout claim, the **payout KYC hook** (§5.2): withdrawal `approved→submitted` in the **same** tx, after the KYC gate and with the kill-switch predicate in the claim statement | Eligible staff (submit endpoint) | Withdrawal CAS `state='approved'` under L1 `FOR UPDATE`; `UNIQUE(withdrawal_request_id)`; `NOT EXISTS` engaged switch | Withdrawal → `submitted` (`provider_id` set, `provider_reference` NULL) | `withdrawal.dispatch_claimed`, `withdrawal.submit.http` (staff) |
| W-KYC | withdrawal `approved` → `rejected` (**no attempt**) | ADR 0096 `DenyForCompliance`: the payout KYC gate denies in T1p's phase-A tx, before the T1p CAS | System, inside the staff submit request | Withdrawal `state='approved'` under the same L1 lock as T1p, so the two are mutually exclusive by lock | Hold reversal per ADR 0096 (`withdrawal_rejected`, key `<id>:kyc_denied`); no attempt, no call | `withdrawal.rejected_kyc` (ADR 0096) |
| T2 | `created` → `submitting` | Claim for the call. Runs in a **per-item** tx that locks the parent first and re-evaluates, in that tx: the tenant capability row; the kill switch (inside the CAS statement); for a deposit, RG and the KYC deposit gate (LF95-C10(e)); for a payout, the payout KYC gate (LF95-C10(b)) | Player-request driver (resume) or sweeper | `state='created' AND (provider_id IS NULL OR provider_id=$p) AND NOT EXISTS(engaged switch, same tenant_id) AND NOT EXISTS(succeeded attempt for the same intent)`; sets `provider_id`, `claim_token`, `lease_until`, `submit_count+1`, `last_sent_at`, and `first_submitted_at` if NULL | none. A non-pass of a gate claims nothing and makes no call: a deposit goes to T3 (`rg_ineligible`/`kyc_required`); a payout stays `created` with a compliance escalation (T16-style), and its hold is released only by M3 (`kyc_denied`), never automatically. | `payment.attempt_claimed` / `payment.attempt_claim_denied` |
| T3 | `created` → `rejected` | Pre-call refusal (kill switch for deposits, unsupported, no route, credential exhausted, presence window expired, gate deny for a deposit), or `intent_succeeded` (LF95-C6(c)) | Driver, sweeper, or the T7/T13 tx | `state='created'` | Deposit: intent projection recomputed (§5.1). Payout: only via M3 (§4.8). | `payment.attempt_rejected` (+reason) |
| T4 | `submitting` → `pending` | Sync `Pending` with a reference | Phase C | `state='submitting'` | Sets `provider_reference`, `accepted_at`; applies deferred receipts (§6.4); withdrawal `provider_reference` set | `payment.attempt_accepted` |
| T5 | `submitting` → `created` | `ErrorClassNotSent` on a first send (provably not dispatched: credential unavailable, gate refusal, connection refused before write), or a `NotProcessed` code the vendor documents as leaving no trace (§8) | Phase C (same claimant) | `state='submitting' AND claim_token=$t AND NOT ever_possibly_sent` | `next_action_at` = backoff | `payment.attempt_not_sent` |
| T6 | `submitting` → `ambiguous` | Sync `Ambiguous`; `ErrorClassAmbiguous` (timeout after a possible send, reset after write, unmapped 5xx); a `NotProcessed` code that may leave a trace (§8); lease expired and `QueryStatus` not definitive; **`NotSent` on a T12 resend** (`ever_possibly_sent` already true, LF95-C1) | Phase C or sweeper | `state='submitting'`; sets `ever_possibly_sent=true` | `next_action_at` = poll backoff (`now()` for `NotProcessed`) | `payment.attempt_ambiguous` (+cause `timeout`/`not_processed`/`resend_not_sent`/…) |
| T7 | `submitting`/`pending`/`ambiguous` → `succeeded` | Verified success evidence **from the attempt's own provider** with amount = attempt amount, asset = attempt asset, and a non-empty provider reference in the evidence or already on the attempt (callback, `QueryStatus`, or sync where the manifest allows sync success) | Phase C, callback tx or sweeper | `state = ANY('{submitting,pending,ambiguous}')`; trigger requires `last_evidence_kind ∈ {sync, callback, query_status}` | **Deposit:** `ledger.Post` Flow 1 with `provider_tx_id = provider_reference` (idempotent on `(tenant, provider_id, provider_tx_id)`); `payment_attempts.ledger_transaction_id` set; the intent's `ledger_transaction_id` set only while NULL; intent → `succeeded`. If a tombstone already occupies `(provider_id, provider_reference)`, T10 (`reversal_tombstone_precedes_success`) instead, with no posting and no error. **Payout:** `withdrawal.Complete` (Flow 3 Step B). | `deposit.posted` / `withdrawal.completed`, `payment.attempt_succeeded` |
| T8 | `submitting`/`pending`/`ambiguous` → `declined` | Definite decline evidence from the attempt's own provider (sync `Declined`, verified decline callback, `QueryStatus` declined, or a deposit authoritative not-found, §4.5) | Phase C, callback tx or sweeper | `state = ANY('{submitting,pending,ambiguous}')`; trigger: payout requires `last_evidence_kind ∈ {sync, callback, query_status}`, deposit requires `≠ operator` | **Deposit:** cascade row (T1) if eligible (§4.6), else intent projection recomputed. **Payout:** `withdrawal.Fail` (hold released). | `payment.attempt_declined`, `withdrawal.failed` |
| T9 | `ambiguous` → `pending` | Evidence that the provider holds it and has not finished | Callback or sweeper | `state='ambiguous'` | Sets `provider_reference` if it was unknown; applies deferred receipts (§6.4) | `payment.attempt_accepted` |
| T10 | `submitting`/`pending`/`ambiguous` → `disputed` | Mismatched success (amount, asset or reference), a provider-reference conflict (§6.1), or a tombstone preceding the success (LF95-C6(d)) | Any evidence path. The state change is **committed** with its receipt, whatever HTTP code is returned (LF95-C3). | `state = ANY('{submitting,pending,ambiguous}')` | none; P1 | `payment.attempt_disputed` |
| T11 | `pending` → `ambiguous` | Explicit "unknown" or not-found evidence for an accepted attempt (the provider forgot it) | Sweeper | `state='pending'` | none; P1 anomaly | `payment.attempt_ambiguous` |
| T12 | `ambiguous` → `submitting` | Idempotent resubmission of the **same** attempt with the **same** key. Only if the manifest has `IdempotentSubmission=true`, `submit_count < max_resubmits` and `NOT legacy_backfill`. For a payout the per-item tx re-runs the payout KYC gate first; a non-pass means no resend and the attempt stays `ambiguous`, resolvable only by poll or callback (LF95-C10(c)). | Sweeper (per-item tx, parent locked first) | `state='ambiguous' AND submit_count < $max AND NOT legacy_backfill AND NOT EXISTS(engaged switch)`; new `claim_token`, lease, `last_sent_at` | none | `payment.attempt_resubmitted` |
| T13 | `declined` → `succeeded` | **Deposit only.** Verified matching success from the attempt's own provider after a decline, with a provider reference | Callback or sweeper | `state='declined' AND operation='deposit'` | Flow 1 posted to `player_cash` (the money is real; LF-Q1 ruling), linked on the attempt; the intent's link is left alone if already set; intent → `succeeded`; any sibling `created` attempt → `rejected` (T3, `intent_succeeded`); P1 `contradictory_provider_outcome`. If another attempt of the intent is already `succeeded`, also P1 `multiple_success_for_intent`. A sibling already `submitting` is a stated residual under the same P1 (§20). Tombstone → T10 instead. | `payment.attempt_succeeded_after_decline` |
| T14 | `declined` → `disputed` | **Payout only.** Success evidence after `withdrawal.Fail` released the hold (a double payout has already happened) | Callback or sweeper | `state='declined' AND operation='payout'` | none; P1 | `payment.attempt_disputed` |
| T15 | `created`/`rejected` → `disputed` | Success evidence **from the attempt's own `provider_id`** for an attempt the platform never sent (adapter misclassification or a hostile verified sender). Evidence from any other provider, or for an attempt with a NULL `provider_id`, is an `anomaly` receipt with **no** state change (S95-C1). | Callback | `state = ANY('{created,rejected}') AND provider_id = $verified_provider` | **Nothing posted**; P1 | `payment.attempt_disputed` |
| T16 | (no state change) escalation | `pending`/`ambiguous`/`submitting` older than the manifest `SettlementWindow`, or a payout `created` blocked by a gate | Sweeper | `escalated_at IS NULL` | Sets `escalated_at`; poll cadence drops to the escalated rate; alert | `payment.attempt_escalated` |
| T17 | (no state change) re-verify | Operator asks for fresh evidence on any attempt, or the payments re-drive job does so after a `pay_status_mismatch` (§12.5) | Staff with `payments_attempt:reverify`, or the named system principal of the re-drive job | none (sets `next_action_at=now()` only; never calls a provider inline) | Evidence is applied through the matrix like any sweeper poll | `payment.attempt_reverify_requested` |
| M1–M3 | manual | §4.8 | §4.8 | §4.8 | §4.8 | §4.8 |

**Forbidden, and enforced by trigger:**

- any move into `created`, except T5;
- `rejected → *`, except T15;
- `succeeded → *` (a reversal is a separate ledger fact, §5.4);
- `disputed → *`, except M1/M2;
- `ambiguous → declined` with `last_evidence_kind='operator'`;
- any `*→declined` for a payout unless `last_evidence_kind ∈ {sync, callback, query_status}`;
- any `→succeeded` unless `last_evidence_kind ∈ {sync, callback, query_status}`;
- T12 when `legacy_backfill` is true;
- any `→rejected` for a payout unless `OLD.state='created' AND NOT ever_possibly_sent` (M3);
- any backward move to `submitting` other than T12.

`DenyForCompliance` (W-KYC) is legal **only from withdrawal `approved`**. Once T1p has
committed, no KYC outcome ever causes `Fail`, a hold reversal or any other automated release;
only T8 evidence or M3 from `created` can release the hold (LF95-C10(d), ADR 0096 C5).

### 4.4 `applyEvidence` — the single evidence matrix

One function, called by phase C, the callback path, the sweeper and T17. Rows are the current
state; columns are the evidence outcome. Every cell is also audited.

**Preconditions checked before any cell is chosen, and before any write** (LF95-C3, S95-C1):

1. The attempt was resolved with `provider_id = verified provider` (INV-IO-14). Otherwise the
   evidence is an `anomaly` receipt with no state change.
2. If the evidence carries both a provider reference and a merchant reference and they resolve
   to **different** attempts, or its provider reference is already bound to another attempt, it
   is an `anomaly` receipt plus P1, with no state change and no posting. This can therefore
   never surface as a unique violation followed by a 5xx redelivery loop.
3. Success evidence with no provider reference, on an attempt that has none either, is treated
   as `ambiguous` evidence plus an immediate poll (LF95-C4). It can never post, because it has no
   ledger key.

| Current \ Evidence | `pending` | `succeeded` (match) | `succeeded` (mismatch) | `declined` | `ambiguous` | `not_found` |
|---|---|---|---|---|---|---|
| `created` | anomaly log, no change | T15 → `disputed` | T15 | anomaly log | no-op | no-op |
| `submitting` | T4 | T7 (tombstone → T10) | T10 | T8 | T6 | §4.5 |
| `pending` | no-op (reschedule) | T7 (tombstone → T10) | T10 | T8 | no-op | T11 |
| `ambiguous` | T9 | T7 (tombstone → T10) | T10 | T8 | no-op (reschedule) | §4.5 |
| `succeeded` | no-op | no-op (duplicate; the ledger is idempotent) | P1 anomaly, receipt `anomaly`, no change | P1 anomaly (a reversal needs a reversal event), no change | no-op | P1 anomaly |
| `declined` | no-op | deposit T13 (tombstone → P1 anomaly, no change) / payout T14 | P1 anomaly | no-op | no-op | no-op |
| `rejected` | anomaly log | T15 | T15 | no-op | no-op | no-op |
| `disputed` | recorded only | recorded only | recorded only | recorded only | recorded only | recorded only |

- "Tombstone" means a ledger tombstone already holds `(provider_id, provider_reference)` because
  a reversal arrived first. The cell is terminal `disputed` (`reversal_tombstone_precedes_success`,
  P1), with no posting and no error, never a rollback, 5xx and re-poll loop (LF95-C6(d)).
- Evidence that carries a `provider_reference` different from a non-NULL
  `attempt.provider_reference` is a *reference mismatch*. It is T10 from a non-terminal state,
  otherwise a P1 anomaly.
- **One exception:** a payout **settlement reference** (`CallbackEvent.SettlementReference`)
  may differ by design. It becomes the ledger `provider_tx_id` of Step B, exactly as
  `withdrawal.Complete` allows today.
- **LF-Q1, ruled by ledger-finance (§21.2):** T13 posts to `player_cash`; no suspense account.
  The ruling holds with LF95-C6, which is written into T7/T13 and §5.1/§5.4 above and below.

### 4.5 Deposit/payout asymmetry (binding)

- **Deposit failure is money-safe to assume; a late success still posts (T13).** On
  `not_found`, a deposit moves to `declined` (`decline_stage=at_submission`,
  `reason=not_received`, **`cascadable=false` enforced in code**) only if **all** of these hold
  (LF95-C8):
  - (a) `accepted_at IS NULL AND provider_reference IS NULL`: the provider never acknowledged the
    attempt. Otherwise `not_found` is T11/`ambiguous`.
  - (b) the manifest declares `MerchantLookupAuthoritativeAfter = Δ` and
    `now() − last_sent_at > Δ`, measured from the **latest** send;
  - (c) registration has already refused any manifest with
    `MerchantLookupAuthoritativeAfter < lease + CallTimeout` (§10.1), so a lookup cannot outrun
    a call still in flight.

  Otherwise, or when there is no merchant-reference lookup at all, it moves to `ambiguous`,
  and T12 applies if the manifest allows it.
- **Payout failure is never assumed.** Releasing a hold for a payout that later lands is a
  double payout.
  - `not_found`, timeout, ambiguous results and KYC outcomes always leave the payout
    unreleased.
  - The only automated exits are T7 (success evidence), T8 (definite decline evidence) and T12
    (idempotent resubmission, whose result is itself evidence).
  - Everything else is escalation (T16), then M2 (four-eyes, currently BLOCKED). M3 applies only
    to `created`.

### 4.6 Cascade (deposits only) — via the attempt row, never inline in a webhook

A decline is **cascade-eligible** when all of these hold:

- `cascadable = true` on the evidence. For a deferred receipt, the persisted `cascadable` column
  is used; a receipt without it can never cascade (LF95-C4);
- the decline is not a §4.5 authoritative not-found (always `cascadable=false`);
- `attempt_no < MaxCascadeDepth`;
- the intent is not `succeeded`;
- no other live attempt exists;
- the kill switch is not engaged for the next candidate (re-checked at T2);
- and either (a) the decline arrived **synchronously on the player-request path**, or (b) the
  attempt is `interactive = false`.

When eligible, the tx that commits T8 also inserts attempt n+1 (T1, `provider_id NULL`,
`excluded_provider_ids = all previous attempts' providers`). This closes the
`providerExclusionSoFar` limitation. Case (a) is driven immediately by the same request's
driver (its own T2 per-item tx). Case (b) is driven by the sweeper.

An **asynchronous** decline of an **interactive** attempt finalizes the intent as `declined`.
The redirect URL of a new provider could never reach the player, which is the latent defect of
today's webhook cascade. The player retries with a new idempotency key.

This amends payment-orchestration.md §5 (recorded above). A cascade **never** follows
`ambiguous`, `not_found` or a timeout (LF-C2 #5).

### 4.7 Withdrawal state mapping

| Withdrawal state (unchanged set) | Payout attempt state | Notes |
|---|---|---|
| `approved` | none | The KYC gate may take W-KYC (`approved→rejected`) here and only here. |
| `rejected` | none | Staff `Reject`, or W-KYC `DenyForCompliance` (ADR 0096) |
| `submitted` | `submitting` / `created` (NotSent retry, or blocked by a gate at T2) / `pending` / `ambiguous` / `disputed` | New meaning: "dispatch claimed; the provider may hold it". `provider_reference` is NULL until T4/T9/T7. The resolve handler's "structurally unreachable" check (`withdrawal_handlers.go:1034`) becomes a normal case (query by merchant reference). |
| `completed` | `succeeded` | Via T7 → `withdrawal.Complete` in the same tx |
| `failed` | `declined`, or `rejected` via M3 | Via T8 → `withdrawal.Fail`, or M3 |

`withdrawal.MarkSubmitted` is split into:

- `ClaimForDispatch(tx, id, providerID)`: `approved→submitted`, no reference;
- `RecordProviderReference(tx, id, ref)`: sets it once, NULL→value.

`Reject`/`Cancel`/`DenyForCompliance` from `approved` race the claim on the same L1 row, and the
CAS guarantees exactly one wins. **No payout cascade** (current behaviour, kept): a declined
payout fails the withdrawal.

`docs/architecture/withdrawal-state-machine.md` (lines 92–93 today say the provider fields are
"set on `approved` → `submitted`") is updated to these semantics by `payments` in I1-f, before
PRH-I1 is marked complete (P95-C2).

### 4.8 Manual intervention (only where genuinely necessary)

| M | What | When it is the only option | Governance | Status |
|---|---|---|---|---|
| M1 | Resolve a `disputed` **deposit** attempt: attach a compensating ledger transaction, or record "no ledger action", with evidence | Contradictory or mismatched evidence | Via LEDGER-MANUAL-ADJ-4EYES-1 (four-eyes above a threshold) | **BLOCKED** on LEDGER-MANUAL-ADJ-4EYES-1. Until then the attempt stays `disputed`, is visible, and is a P1. |
| M2 | Force-resolve an `ambiguous`/`disputed` **payout** (declare paid → `Complete`, or declare not-paid → `Fail`) without provider evidence | The provider cannot be queried, has no idempotent resubmission, is past the horizon, and reconciliation has not resolved it | Four-eyes; threshold per **HD-0095-1** | **BLOCKED** on HD-0095-1 and LEDGER-MANUAL-ADJ-4EYES-1. No API is built. |
| M3 | Abandon a payout attempt in `created` (never sent, INV-IO-9) → `rejected`, then `withdrawal.Fail` in the same tx | The credential or provider is permanently gone, **or** a KYC deny was found at a T2 re-claim (reason `kyc_denied`; ADR 0096's T2-reclaim case routes here, never to a second `DenyForCompliance`) | Staff with today's resolve eligibility (linked, active) plus a mandatory reason code, audited with IP/UA. **Never automated.** The CAS requires `state='created' AND NOT ever_possibly_sent` in SQL and in the trigger (S95-C13). **No double-payout risk**, because `created` has provably never been sent. | Designed here; built in PRH-I1. |

The operator re-verify (T17) and kill switch (§10) are not "manual resolution". They only
fetch evidence or stop new activity.

---


## 5. Per-operation specification

All operations share these properties. Idempotency is DB-enforced. Audit goes into
`audit_log` in the same tx as the effect. Nothing is authoritative in Redis.

### 5.1 Deposit (priority; fully specified)

| Aspect | Specification |
|---|---|
| Intent | `deposit_intents` row (unchanged table, status set unchanged). Its status is a projection of its attempts, updated in the same tx as every attempt transition, evaluated in this order: any `succeeded` → `succeeded` (sticky); any `disputed` (and no `succeeded`) → `ambiguous`, never `declined`, because funds may have been captured (LF95-C7); a live attempt that is `ambiguous` → `ambiguous`; any other live attempt → `pending`; none live → `declined`. `failed` stays unused. RG denial keeps today's behaviour: intent → `declined` with `rg_ineligible:*` and no attempt, in phase A. A KYC deposit deny (ADR 0096) is the same shape. |
| Ledger link (LF95-C6(a)) | Every deposit posting is linked on `payment_attempts.ledger_transaction_id`. `deposit_intents.ledger_transaction_id` is written only while it is NULL (the migration 0082 trigger already forbids repointing it), so it keeps pointing at the **first** posting. Bonus deposit detection (`internal/bonus/deposit_sweep.go`) therefore sees only the first capture; `bonus-engine` confirms that is intended (LF95-R2, §20). |
| Provider reference | `payment_attempts.provider_reference`: set once by T4, T7 or T9, bounded by `PROVIDER_REF_MAX` (0099), and unique per `(tenant, provider_id)`. `deposit_intents.provider_id/provider_reference` keep mirroring the latest routed attempt for existing readers; `TestMigration0082_DepositIntentsProviderColumnsStayMutable` must keep passing, and a new test asserts the mirror through a cascade (P95-C1). |
| Idempotency keys | **Player:** `UNIQUE(tenant, player, idempotency_key)` on the intent (exists). A retry **resumes**: it returns the intent; if its live attempt is `created`, the retry drives it (T2 CAS in a per-item tx that re-runs RG and the KYC deposit gate, so concurrent retries yield exactly one claimant and a stale eligibility is never reused); if `submitting`/`pending`/`ambiguous`, it returns the status (the redirect URL is not persisted; it is re-obtained only by T12 when `IdempotentSubmission`). **External:** `external_idempotency_key = "pa:" + attempt.id`, `merchant_reference = attempt.id` (INV-IO-3). **Ledger:** `(tenant, provider_id, provider_tx_id = provider_reference)` plus `idempotency_key = provider_id:provider_reference` (exists). |
| Flow (player request) | Phase A0 (read-only tx): load candidates → health filter **outside** the tx (§9.6) → pick a provider. Phase A (**one** tx): RG check and the ADR 0096 KYC deposit gate (a deny commits the intent as `declined` with no attempt) → insert intent plus attempt 1 directly in `submitting` (T1+T2, kill-switch predicate inside the INSERT statement) → commit. There is therefore no player-path `created` row; `created` exists only for cascade rows and NotSent reverts (LF95-C12, CP-D1). Phase B: resolve credential, `Deposit(CallContext, req)`. Phase C: `applyEvidence` (T4/T6/T8/T7), with a synchronous cascade loop (§4.6), each step its own A/B/C. The handler no longer wraps the call in `WithTenant`; `InitiateDeposit` takes `*db.Pool` (INV-IO-1a). |
| State / retryability | §4.3. `NotSent` → T5 (the driver retries within its request budget, else the sweeper takes over if non-interactive; for an interactive attempt the player sees "temporarily unavailable" and the attempt expires via T3). `Ambiguous` → T6, never cascaded, T12 only if the manifest allows it. |
| Callback | §6. Resolution by `(provider_id, provider_reference)` **or** `merchant_reference`, both bound to the verified provider (INV-IO-14). Accepted in `submitting`. |
| Reconciliation | §12: `pay_*` kinds keyed by `(provider_id, provider_reference)` with a merchant-reference fallback (same provider only), plus the ledger join (§12.3, LF95-C13). |
| Failure recovery | Crash points CP-D1..CP-D8 (§16.1), each converged by CAS phase C, callbacks or the sweeper. |
| Audit | `deposit.requested`, `payment.attempt_*` (every transition, with `last_evidence_kind`, provider id, reference, `error_class`, credential **fingerprint and handle id only**), `deposit.posted`, `deposit.declined`, `payments.deposit_denied_by_rg` (exists). |

### 5.2 Withdrawal payout dispatch (LF-C2 #9)

| Aspect | Specification |
|---|---|
| Intent | `withdrawal_requests` (existing four-eyes approval flow unchanged) plus exactly one payout attempt (INV-IO-8). |
| Provider reference | The attempt's `provider_reference` (the instruction reference), copied once into `withdrawal_requests.provider_reference`. A settlement or confirmation reference may differ (Flow 3) and is the Step B `provider_tx_id`. |
| Idempotency keys | Staff double-submit: the L1 `FOR UPDATE` plus the CAS `approved→submitted` plus `UNIQUE(withdrawal_request_id)` on attempts, so the second caller gets `ErrStateConflict` **before any call**. External: `"pa:" + attempt.id`. Ledger: Step B `provider_id:provider_tx_id` (exists); Fail `request_id:failed` (exists). |
| Flow | Phase A0 (read-only tx, no lock): load the request plus candidates → health outside the tx → route. Phase A (one tx; LF95-C10(a)), in this exact order: (1) approver eligibility (exists); (2) L1 `withdrawal_requests FOR UPDATE`, `state='approved'`; (3) **the payout KYC gate (ADR 0096)**: on deny, `DenyForCompliance` (W-KYC, `approved→rejected`, hold reversal `<id>:kyc_denied`) commits, no attempt is created and no call is made; (4) capability re-read; (5) **T1p**: withdrawal `approved→submitted` plus the attempt INSERT in `submitting`, with the kill-switch `NOT EXISTS` predicate inside the claim statement (an engaged switch claims nothing, the tx rolls back to a 503, and the withdrawal stays `approved`) → commit. **T1p is THE payout KYC hook**: the last commit before the first `Withdraw` for that request; KYC deny and T1p both require `state='approved'` under the same L1 lock, so they are mutually exclusive by the lock, not by convention. Phase B: resolve credential (NotSent on failure → T5, and the sweeper retries unattended because payouts are non-interactive), `Withdraw`. Phase C: L1 lock (withdrawal) → attempt → `applyEvidence` (T4/T6/T7→`Complete`/T8→`Fail`). |
| Retryability | NotSent → T5, then the sweeper re-claims (T2) while the kill switch is released. The T2 re-claim runs in a per-item tx that locks the withdrawal first and **re-runs the payout KYC gate**; on a non-pass it claims nothing, makes no call, leaves the attempt `created` with a compliance escalation, and the hold is released only by staff M3 (`kyc_denied`), never automatically (LF95-C10(b); ADR 0096's T2-reclaim case). Ambiguous → never resubmitted except T12, which also re-runs the gate (LF95-C10(c)). Never re-routed. After T1p, no KYC outcome releases the hold (LF95-C10(d)). |
| Callback | New canonical event `payout` (§9.3), handled by the same receipt/resolution path. `payout_returned` (after completion) is declared **unsupported** (NOT IMPLEMENTED): receipt `unsupported_event`, P1, verified-malformed 4xx class. |
| Reconciliation | §12, line kind `payout`. |
| Failure recovery | CP-W1..CP-W6 (§16.1). The existing staff resolve endpoint becomes: read (no lock) → `QueryStatus` outside the tx → phase C. It is T17-equivalent evidence, never a resubmission. |
| Audit | `withdrawal.dispatch_claimed`, `withdrawal.submit.http` (staff, IP, UA; exists), `payment.attempt_*`, `withdrawal.completed`/`withdrawal.failed` (exist), `withdrawal.resolve_attempted.http` (exists). |

#### 5.2.1 Payout KYC gate placement (ADR 0096; LF95-C10; IC conditions 3–4)

The gate is the ADR 0096 enforcement function exported by `internal/kyc` (being implemented in
PRH-I3), called with operation `withdrawal_payout`. This ADR names it only generically.

| Point | Where the gate runs | On allow | On deny / non-pass |
|---|---|---|---|
| **T1p claim tx (THE payout KYC hook)** | After the L1 `withdrawal_requests FOR UPDATE` (`state='approved'`), before the `approved→submitted` claim | T1p proceeds | `withdrawal.DenyForCompliance` in the **same** tx (W-KYC, `approved→rejected`, hold reversal `<id>:kyc_denied`), no attempt row, no call. Mutually exclusive with T1p by the L1 lock. |
| **Sweeper re-claim T2** (payout attempt `created` after T5; withdrawal already `submitted`) | Per-item tx, after locking the withdrawal and the attempt, before the T2 CAS | T2 proceeds | No claim, no call; the attempt stays `created` with a compliance escalation. The hold is released **only** by staff M3 (`kyc_denied`), which is legal because `created` is provably never sent (INV-IO-9). `DenyForCompliance` is never applied a second time (it requires `approved`). |
| **Resubmission T12** (attempt `ambiguous`; withdrawal `submitted`) | Per-item tx, after locking the withdrawal and the attempt, before the T12 CAS | T12 proceeds | No resend. The attempt is **not** M3-eligible (it may have been sent), so it stays `ambiguous` and is resolved only by poll or callback evidence. |
| **After possible dispatch** (`submitting`/`pending`/`ambiguous`/`disputed`, or `ever_possibly_sent`) | — | — | **No KYC outcome ever triggers `Fail`, a hold reversal or any automated release.** Only T8 evidence (or M3 from `created`) releases the hold; a post-dispatch KYC finding is a compliance case, not a ledger action. |

### 5.3 Status query (`QueryStatus`)

| Aspect | Specification |
|---|---|
| Intent | No row of its own. It always concerns an existing attempt. |
| Reference | `StatusQuery{ProviderReference, MerchantReference}`. The adapter uses the provider reference when known, and the merchant reference only if the manifest has `StatusQuery = by_provider_or_merchant_reference`. |
| Idempotency | Read-only at the provider. Concurrent duplicates are harmless; the lease prevents waste. |
| State | Never changes state itself. Its `StatusResult` is evidence for `applyEvidence`. |
| Retryability | Always retryable: `NotSent`/`Ambiguous` → reschedule with backoff. |
| Callback | n/a |
| Reconciliation | The poll outcome is recorded on the transition audit only when it changes state or escalates. Polls are not receipted (volume). |
| Failure recovery | Sweeper backoff, then escalation (T16). |
| Audit | Only on a state change, escalation or anomaly. |

### 5.4 Deposit reversal (inbound: refund or chargeback initiated by the provider)

| Aspect | Specification |
|---|---|
| Intent | None. The reversal is a provider-initiated fact about a `succeeded` deposit attempt. The attempt state does **not** change: the reversal is the ledger's `reverses_transaction_id` fact (Flow 2; PAY-REV-1 L2 lock unchanged). |
| Reference | The reversal's own `provider_reference` plus `original_provider_reference`. The original is resolved through `payment_attempts (provider_id = verified provider, provider_reference = original_provider_reference)` → **the attempt's** `ledger_transaction_id`, never the intent's; otherwise a reversal of a second capture (T13) would reverse the first (LF95-C6(b)). No matching attempt, or an attempt with no posting, takes the existing tombstone branch. |
| Idempotency | Existing ledger uniqueness plus INV-PAY-REV-1 (migration 0092) plus the receipt dedupe. |
| Callback | The same receipt path. An unseen original still writes the tombstone (exists). |
| Reconciliation | Line kind `deposit_reversal`. |
| Failure recovery | Unchanged: 5xx → redelivery. LF-C1 applies (§6.6). |
| Audit | `deposit.reversed`, `deposit.reversal_tombstoned`, `deposit.reversal_rejected` (all exist). |

### 5.5 Refund / reversal initiated by the platform (outbound)

`NOT IMPLEMENTED`, and not built by PRH. No platform flow initiates a refund today.

- The contract reserves `Refund(ctx, CallContext, RefundRequest)`, and the manifest has
  `supports_refund = false` for every adapter. Registration refuses an adapter declaring
  `true` until a flow exists. A call fails closed with `ErrCapabilityUnsupported`.
- When it is built, it is a third `operation = 'refund'` on `payment_attempts`. It has the
  same states and payout-style asymmetry (§4.5), because refunding money out is a payout.

### 5.6 Casino launch and KYC create/submit

These are specified in §15, with minimal redesign.

---

## 6. Callbacks

### 6.1 Order (the domain step of ADR 0097's pipeline; phase 1 and phase 2 of ADR 0094 unchanged)

This all happens in one domain tx (`WithTenant`), with **no provider I/O**. The verified
provider `P` is the route-resolved provider whose credential verified the callback; it is
never read from the payload.

1. **Redeem** (exists). A DB/transport failure of the post-verification re-check returns the
   typed `RecheckUnavailableError` → retryable 503 (§6.6, S-Q3); every definitive re-check
   result stays the uniform 401.
2. **`HandleCallback`** (exists).
3. **PROVIDER-REF-BOUND-1 boundary validation** (0099; a deterministic non-retryable
   rejection, before any write).
4. **Resolve the attempt, read-only and bound to `P`** (INV-IO-14, S95-C1, LF95-C3):
   - (a) by `(tenant, provider_id = P, provider_reference)`;
   - (b) by `(tenant, merchant_reference)` **with `attempt.provider_id = P`**, if the event
     carries one;
   - if (a) and (b) both resolve and name **different** attempts, or (b) matches an attempt
     whose `provider_id` differs from `P` or is NULL, or the evidence's provider reference is
     already bound to another attempt, the event is an **`anomaly`** (P1): it is receipted and
     changes nothing, and T15 is **not** taken;
   - (c) if nothing resolves, the event is **unresolved**.
5. **Deferred cap probe** (unresolved only; S95-C2(i)): count unapplied receipts for
   `(tenant, P)` with `LIMIT cap+1` on the partial index (`RECOMMENDATION` cap 10 000). Above
   the cap the tx stores nothing and returns a retryable **503** plus a P1 alert. It never
   returns a 200 without storing, and never a 404.
6. **Receipt insert (R0; the first write of the tx, §14).** `INSERT … ON CONFLICT (tenant_id,
   provider_id, event_fingerprint) DO NOTHING RETURNING id`. On conflict the event is a
   **duplicate delivery**: evidence is still applied (idempotently), but no new receipt row is
   written. The receipt persists every field `applyEvidence` consumes, including `cascadable`,
   `decline_stage` and a bounded, allow-listed `decline_reason` code (LF95-C4).
7. **Lock** in ADR 0082 order (§14): parent (`deposit_intents` or `withdrawal_requests`) `FOR
   UPDATE`, then the attempt. Re-read the attempt under the lock, then `applyEvidence` (§4.4,
   including its preconditions).
8. **Set the receipt's one-shot resolution columns** (`attempt_id`, `resolution`,
   `resolved_at`), or leave it unresolved (`deferred_unresolved`).
9. Commit, then return the response from §6.2.

### 6.2 Dispositions and HTTP mapping

**Every 200 has a byte-identical body**, apart from the request id (S95-C4). The disposition
is recorded only in the receipt, the audit record and metrics, so a verified sender cannot
learn whether a reference exists.

| Disposition | Condition | Response |
|---|---|---|
| `applied` | The evidence changed state or posted | 200 (uniform body) |
| `duplicate_effect` | The receipt already existed, or the evidence was a no-op for the current state | 200 (uniform body; unchanged replay semantics) |
| `deferred_unresolved` | Verified, but no attempt is resolvable yet (the callback raced phase C before the reference was known, and there is no merchant-reference echo), and the §6.1 step 5 cap is not exceeded | **200 after durable receipt** (INV-IO-10). This changes today's 404 for this case only. |
| `anomaly` | A precondition failure of §4.4 (cross-provider, cross-attempt reference conflict), a P1 cell of §4.4, or a mismatched success (T10) | 200 (uniform body) after durable receipt plus alert. **The state effect (T10 `disputed`, or none) is committed together with the receipt; it is never rolled back to produce an error code** (LF95-C3). This supersedes today's rollback-and-409 for `ErrCallbackProviderMismatch` on the deposit-success path. |
| typed reversal rejections | `ErrCallbackPayloadMismatch`, `ErrDepositAlreadyReversed` on the reversal path (no attempt state effect exists for them) | Unchanged: 409, with the existing separate-tx evidence record (`RecordDepositReversalRejection` pattern). The separate tx takes at most one R0 insert as its first write (§14). |
| `unsupported_event` | A verified event type the manifest does not support | Existing verified-malformed 4xx class; receipt written in the separate-tx pattern |
| cap exceeded | §6.1 step 5 | Retryable 503 (ADR 0097 §6.1 generic body, `Retry-After`) plus P1; nothing stored |
| re-check unavailable | §6.1 step 1, `RecheckUnavailableError` | Retryable 503 (ADR 0097 §6.1 generic body); nothing written |
| (none) | DB error or a rollback of any kind, including a DB CHECK reached at insert (which means the application bound and the DB bound disagree: 5xx **plus alert**, S95-C2(iii)) | 5xx, which is retryable; nothing is committed; the provider redelivers (LF-C1, §6.6) |

### 6.3 Duplicate and concurrent callbacks

Two concurrent deliveries of one event serialize on the receipt-key insert (R0) first. The
second waits, then conflicts, then applies idempotently. They then serialize on the parent and
attempt locks and the ledger unique constraint. The result is exactly one posting (INV-IO-13
test).

Two *different* events for one attempt (for example, a decline and then a success) serialize on
the parent lock, and the matrix decides.

### 6.4 Callback before or after the internal transition

| Race | Outcome |
|---|---|
| Callback arrives while the attempt is `submitting` (phase C not yet committed) | With a merchant-reference echo, it resolves by merchant reference (same provider only) and T7/T8 applies. Phase C later finds `succeeded`/`declined`, and its CAS from `submitting` fails. The late sync result is then passed to `applyEvidence` against the new state and is a no-op or anomaly (reference cross-checked). |
| The same race without a merchant-reference echo | `deferred_unresolved`. Phase C's T4/T9 (which sets `provider_reference`), **in the same tx and holding the parent and attempt locks**, selects deferred receipts for `(provider_id = attempt.provider_id, provider_reference)` and applies them in ascending receipt id (LF95-C9(b)). A receipt is applied **only if `received_at >= attempt.first_submitted_at`** (both from the DB clock); an earlier receipt cannot describe this submission, so it is resolved as `anomaly_predates_submission` and alerts, and is never applied (S95-C3). The sweeper backstop does the same, locking the parent and attempt first. Both orders converge to the same final state. |
| Callback after timeout (the attempt is `ambiguous`) | T7/T8/T9 per the matrix. This is the normal resolution path. |
| Callback after the sweeper declined a deposit on an authoritative not-found | T13 (post plus P1). |
| Callback for an attempt the platform never sent, from that attempt's own provider | T15 (no post, P1). From any other provider: `anomaly`, no change. |
| A deferred receipt is never claimed | After the manifest `SettlementWindow` (24 h if undeclared) it raises a P1 "unmatched verified callback" and is reported by the §12 stream as `pay_unresolved` (LF95-C5, S95-C2(ii)). It never creates an attempt. |

### 6.5 What a callback may never do

- Call a provider: no cascade I/O, no `QueryStatus`.
- Hold a transaction longer than its own DB work.
- Trust a payload tenant or a payload provider.
- Touch an attempt of a provider other than the verified one (INV-IO-14).
- Create an attempt, except the cascade row (T1) committed with a decline.
- Post a ledger entry for anything other than matching verified success (with a provider
  reference) or reversal evidence.

`handleDecline → attemptDeposit` and `resolveAmbiguous` are removed from the callback path. An
ambiguous-outcome callback sets `next_action_at = now()` and nudges the sweeper.

### 6.6 LF-C1 (401 and 5xx redelivery) and webhook retry semantics, carried into the manifest

Every adapter manifest declares `RedeliveryOn401` and `RedeliveryOn5xx`, each one of
`redelivers | terminal | unknown`, as recorded from vendor intake #28, and the mandatory
`WebhookRetrySemantics` (§10.1, P95-C3, ADR 0097 §19 condition 2).

**Option (b), enforced structurally.** At startup, a **production-eligible** adapter with
`terminal` or `unknown` for 401 is refused unless a production-eligible
`PaymentStatementSource` is registered for the same provider id. The §12 stream reports
"provider-settled, platform-unposted" as `pay_status_mismatch`/`pay_missing_platform_record`,
which are P1.

**Option (a), approved narrowly by `security` (§22.3) and additive to (b).** A **DB or
transport failure** during the post-verification re-check (`HandleRecheckSQL` inside
`ReceiveVerifiedCallback`, ADR 0094 §5) returns a retryable 503 instead of the uniform 401:

1. Only a connection error, a serialization or statement failure, or a context deadline at the
   re-check qualifies. It is classified by a typed `RecheckUnavailableError`, distinct from
   `AuthError`, never from error strings. A definitive result (handle not found, not active,
   revoked, expired, fingerprint or key-id mismatch) stays the uniform 401 with
   `credential_unavailable`. **Anything unclassified defaults to 401.**
2. The 503 uses ADR 0097 §6.1's generic body and headers (with `Retry-After`); no reason text.
3. Nothing is written: the domain tx rolls back. `TestReceiveVerified_RecheckDBErrorRollsBack`
   is extended to assert the 503 as well as the zero rows.
4. A metric plus an alert on the re-check-unavailable rate per (domain, tenant, provider).
5. Option (b) still applies to every production-eligible adapter with `RedeliveryOn401 ∈
   {terminal, unknown}`.
6. It applies to all three webhook domains through the shared `ReceiveVerifiedCallback` error
   typing (payments in PRH-I1; casino and KYC in PRH-I2). It does **not** extend to
   pre-verification DB errors, which stay the uniform 401.

---


## 7. Outbox and sweeper (bounded work)

### 7.1 Work set

These are `payment_attempts` rows with `next_action_at <= now()`:

| State | Sweeper action |
|---|---|
| `created`, `interactive = false`, or any payout | route if `provider_id IS NULL` (A0) → per-item tx: lock parent, lock attempt, re-run gates (deposit: RG + KYC deposit gate; payout: payout KYC gate), T2 with the in-statement kill-switch predicate → commit → resolve credential → call → C. A payout gate non-pass leaves the attempt `created` with a compliance escalation (M3 only). |
| `created`, `interactive = true`, older than `presence_window` | T3 (`expired_before_submission`) |
| `submitting`, `lease_until < now()` | `QueryStatus` (§5.3) → matrix; `not_found` → §4.5 |
| `pending` | `QueryStatus` → matrix; still pending → backoff; past the horizon → T16 |
| `ambiguous` | `QueryStatus` → matrix; or T12 if the manifest allows it; else backoff/T16 |

Deferred receipts whose `(provider_id, provider_reference)` now resolves **to an attempt of the
same provider** are also re-applied (with the `received_at >= first_submitted_at` rule, §6.4),
bounded by the same per-tenant batch, in a per-item tx that locks the parent and the attempt
first. Deferred receipts still unresolved after the `SettlementWindow` raise the §6.4 P1 once
(LF95-C5, S95-C2(ii)).

### 7.2 Claim protocol

1. **Tenant iteration.** Active tenants in round-robin order, as the reconciliation scheduler
   does (`activeTenantIDs`). Each tenant is handled in its own `WithTenant` tx.
2. **Batch lease tx (short; the sole exception to "parent first", LF95-C9(d)).** `SELECT … FROM
   payment_attempts WHERE next_action_at <= now() ORDER BY next_action_at LIMIT $batch FOR
   UPDATE SKIP LOCKED`. For each row it writes **only** `lease_owner`, `lease_until = now() +
   lease` and `next_action_at = lease_until`, so a crashed worker's items reappear. It never
   changes a state, never reads or locks a parent row and never waits on a lock (SKIP LOCKED
   only). Commit.
3. **Per-item claim tx (state-changing items).** For `created` (T2/T3) and `ambiguous` (T12)
   items: lock the parent, then the attempt, re-check the lease owner, run the gates, run the CAS
   with its in-statement predicates → commit. For polls no claim tx is needed.
4. **Calls.** Each item runs outside any tx, bounded by: a global concurrency cap; a
   per-(tenant, provider) concurrency cap; a per-call deadline shorter than `lease / 2`; and
   the breaker (§9.6).
5. **Phase C per item.** It locks the parent, then the attempt, applies evidence, clears the
   lease, sets `next_action_at` = backoff (exponential with jitter, capped) or NULL when
   terminal, and bumps `poll_count`.
6. **Identity.** The sweeper runs under the ordinary application role with RLS (never
   BYPASSRLS), and its audit actor is a named system principal (S95-C9(ii)).

### 7.3 Bounds and defaults

`RECOMMENDATION` defaults. All of these are configuration; `payments` and `qa` tune them.

| Parameter | Value |
|---|---|
| Sweep tick | 15 s, plus an in-process nudge channel for immediate work (for example after an ambiguous callback) |
| Batch per tenant per tick | 20 |
| Global concurrent calls | 16 |
| Per-(tenant, provider) concurrent calls | 4 |
| Lease | 60 s. It must exceed resolve budget + call timeout + phase C, and it must exceed 2 × call timeout. |
| Poll backoff | Base 30 s, cap 30 min |
| Resolution horizon | Manifest `SettlementWindow`; 24 h if undeclared |
| Unapplied-receipt cap per (tenant, provider) | 10 000 (§6.1 step 5) |
| Escalated poll cadence | 6 h |
| Interactive presence window | 30 min |
| `max_resubmits` | 3 |
| `MaxCascadeDepth` | 3 (exists) |

**Work is bounded per tick** by (tenants × batch) and by the concurrency caps. **Fairness:**
one tenant's slow provider consumes at most its per-(tenant, provider) slots and never a
pooled connection (no connection is held during calls; the F-POOL-1 lesson). **Idle cost:**
one indexed range scan per tenant per tick (`idx_payment_attempts_due`).

### 7.4 Why at-most-one concurrent money call per attempt

A money-moving call requires the claimant to hold `state = submitting` with its `claim_token`.
Entering `submitting` is T1+T2, T1p (guarded inserts), T2 or T12 (CAS from `created`/`ambiguous`). The lease only
governs *re-polling*: an expired `submitting` lease leads to `QueryStatus`, never to a second
submission. The one exception is T12, which re-sends the **same key**, and only when the
provider deduplicates it.

---

## 8. Error, retry and timeout classification

The adapter maps every outcome of an outbound call to exactly one of:

| `ErrorClass` / outcome | Meaning | Source (typical) | State effect | Automatic retry |
|---|---|---|---|---|
| `NotSent` | Provably never dispatched | Credential unavailable; gate refusal; `httpclient` `Sent=false` (build error, dial refused before write, breaker open) | First send (`ever_possibly_sent=false`): T5. **On a T12 resend** (`ever_possibly_sent` already true): T6, back to `ambiguous`, never T5 (LF95-C1) | Yes, same attempt, first send only (INV-IO-9) |
| `NotProcessed` | Dispatched, but the vendor **documents** that this response means "not processed" (for example a specific 4xx or 429). Each code cites its vendor-documentation source in the intake (S95-C12(ii)). | The vendor error model (intake #20). **Adapter-declared per code.** Anything undocumented is `Ambiguous`. | If the vendor documents that the request **left no trace**: exactly `NotSent` (T5 on a first send). Otherwise: **T6** (`ambiguous`, `ever_possibly_sent=true`, `next_action_at=now()`) (LF95-C1). | Only through T12, which requires `IdempotentSubmission`, for deposits **and** payouts. Registration refuses a payout `NotProcessed` mapping without `IdempotentSubmission`. |
| `DefiniteDecline` | The vendor definitively refused (canonical `Declined`, with `Cascadable` and `decline_stage`) | Business decline, validation 4xx the vendor documents as final | T8 | No (cascade is a new attempt, §4.6) |
| `Ambiguous` | Anything else: timeout after a possible send, reset, unmapped 5xx, malformed response, panic | `httpclient` `Sent=true`, `ErrProviderMalformedResponse` | T6 | Only T12 (same key, `IdempotentSubmission`) |
| `Pending` | Accepted, with a reference | — | T4 | — |
| `Succeeded` (sync) | Definite success, only where the manifest has `SyncSuccessPossible` and a provider reference is returned | — | T7 | — |

- **Timeout classification.**
  - A timeout before dispatch (credential or queue) is `NotSent`.
  - A timeout after dispatch is `Ambiguous` and **never a failure**.
  - A settlement timeout (no final outcome within the horizon) is an escalation (T16), not a
    state.
- **httpclient idempotent retry.** It is enabled for a money-moving request only if the
  manifest has `IdempotentSubmission`, because it re-sends the same key. It is always
  enabled for `QueryStatus`.

---

## 9. Provider-neutral payment contract

This is a Go-shaped sketch. The final names belong to `payments` in PRH-I1. No field is
vendor-specific, and there is still no free-form passthrough (ADR 0022 §4.1).

### 9.1 Call context (every outbound request type embeds it)

```go
type CallContext struct {
    TenantID       uuid.UUID                    // from the attempt row (server-side), never a payload
    ProviderID     string
    Credential     providercred.OutboundCredential // resolved per call (§11); MOCK: synthetic credential
    IdempotencyKey string                       // "pa:"+attempt.id (money ops); "" for QueryStatus
    Deadline       time.Time
}
```

The adapter **must** fail with `NotSent` if `Credential.TenantID != TenantID`,
`Credential.ProviderID != ProviderID`, or `Credential.Domain != "payments"`. The gate performs
the same check first (§3.2 step 4), so neither is load-bearing alone (S95-C8(b)).

`CallContext` implements its own `String`, `GoString`, `Format`, `LogValue` and `MarshalJSON`,
which render only the credential's existing redacted form (handle id, key id, fingerprint) and
never the secret (S95-C8(c)).

### 9.2 Operations

```go
Deposit(ctx, DepositRequest{Call CallContext; MerchantReference string; Amount int64; AssetCode, PaymentMethod string}) (DepositResult, error)
Withdraw(ctx, WithdrawRequest{Call CallContext; MerchantReference string; Amount int64; AssetCode, PaymentMethod string}) (WithdrawResult, error)
QueryStatus(ctx, StatusQuery{Call CallContext; ProviderReference, MerchantReference string}) (StatusResult, error) // StatusResult gains Found bool
// Refund: reserved, NOT IMPLEMENTED (§5.5)
HandleCallback(ctx, InboundCallback, WebhookCredential) (CallbackEvent, error)          // unchanged signature
WebhookScheme() webhookauth.VerificationScheme                                          // unchanged
Capabilities() AdapterCapability                                                        // gains Manifest (§10.1)
HealthStatus(ctx) (ProviderHealth, error)   // contract tightened (§9.6)
```

Every result and error carries a `Class ErrorClass` (§8). A returned Go `error` without a class
is treated as `Ambiguous` for money operations.

`Amount int64` carries forward the existing platform-wide int64 amount path (the same as
`ledger.EntryInput`). This is not new here; an 18-exponent asset routed through
`PaymentProvider` would need its own decision, and custody-asset precision stays with ADR 0008
(LF95-R3).

### 9.3 Callback event (canonical)

`CallbackEvent` gains:

- `MerchantReference string` (optional; the echo of our reference, if the vendor supports it);
- `SettlementReference string` (optional; payout confirmation);
- `DeclineStage` (`at_submission | after_acceptance`) alongside the existing `Cascadable`;
- `EventType ∈ {deposit, deposit_reversal, payout, payout_returned}`. `payout_returned` is
  unsupported (§5.2).

`DeclineReason` (and the attempt's `decline_reason`/`terminal_reason`) holds a canonical,
allow-listed code, never vendor free text. The adapter conformance suite asserts that no
payer-identifying vendor field (name, email, IBAN, PAN or masked PAN, wallet address) maps into
`CallbackEvent`, `StatusResult` or a statement line (S95-C10).

The receipt's `event_fingerprint` is SHA-256 over the canonical, length-prefixed tuple
`(event_type, provider_reference, original_provider_reference, merchant_reference, outcome,
amount, asset_code, settlement_reference)`. It is never computed over raw bytes, because a
vendor may re-sign or reorder a redelivery.

### 9.4 Provider reference

References are opaque and bounded by `PROVIDER_REF_MAX` (0099). They are validated at the
adapter boundary after verification as a deterministic rejection, and they are never truncated.
Uniqueness is scoped per `(tenant, provider_id)`, never globally.

### 9.5 Reconciliation interface

```go
// internal/reconciliation/statement (dependency-free leaf, as for casino/sportsbook)
type PaymentStatementLine struct {
    ProviderID, ProviderReference, MerchantReference, OriginalProviderReference string
    Kind   string // "deposit" | "deposit_reversal" | "payout"
    Status string // "pending" | "succeeded" | "declined" | "reversed"
    Amount int64; AssetCode string; OccurredAt time.Time
}
type PaymentStatement struct { CoverageStart, CoverageEnd time.Time; Lines []PaymentStatementLine }
type PaymentStatementSource interface {
    Label() string                                   // MOCK label must contain "MOCK"
    // Fetch runs with NO transaction held (it is provider I/O for a real source; INV-IO-1).
    // It takes the real CallContext: credential resolved per §11 and passed through the §3.2 gate (S95-C11).
    Fetch(ctx context.Context, call CallContext, periodStart, periodEnd time.Time) (PaymentStatement, error)
}
```

This **deliberately differs from `CasinoStatementSource`**, which reads inside the run's
tx. See §12.2 and finding CAS-STMT-IO-1 in §20.

### 9.6 Health and circuit state

- `HealthStatus` is tightened to "**in-memory snapshot, no I/O, returns promptly**". A
  conformance case gives the adapter a transport that fails the test on any request and a
  cancelled context, and asserts a prompt return.
- The orchestrator owns a **breaker per (tenant, provider)**. It is fed by `ErrorClass`: a
  consecutive `Ambiguous`/`NotSent` transport failure counts; `DefiniteDecline` does not.
  States are closed, open and half-open; the half-open probe is the next real call.
- The effective circuit is open if either the adapter snapshot or the breaker says open.
  Routing reads it **outside** any tx. `RouteProvider` is split into
  `ListRoutingCandidates(tx)` (inside a tx) and `Rank(candidates, health)` (no tx).
- A breaker open for tenant A does not affect tenant B. A credential-store outage is
  `NotSent`, is not counted as a provider failure, and never causes a cascade.

---

## 10. Capability manifest and kill switch (payment scope only)

### 10.1 Operation manifest (adapter-declared, code only, never tenant-editable; ADR 0022 §2.1)

`AdapterCapability.Manifest`:

| Field | Type | Fail-safe when absent or false |
|---|---|---|
| `SupportsDeposit`, `SupportsPayout`, `SupportsRefund`, `SupportsDepositReversalEvents` | bool | The operation is refused at registration (if required) or at new activity; events are received as `unsupported_event` |
| `StatusQuery` | `none \| by_provider_reference \| by_provider_or_merchant_reference` | `none` is refused for any adapter with `CallbackCapabilities = polling_only`, and for any production-eligible payout adapter |
| `MerchantLookupAuthoritativeAfter` | duration or 0 | 0 means `not_found` is never authoritative (§4.5) |
| `IdempotentSubmission` | bool | false means no T12, and no httpclient retry on money calls |
| `SyncSuccessPossible` | bool | false means a sync `Succeeded` is treated as `Ambiguous` and triggers a query (defensive) |
| `Interactive` | per payment method: bool | Unknown means `true` (the safer choice: no unattended submission) |
| `CallTimeout`, `SettlementWindow` | durations | Defaults per §7.3 |
| `RedeliveryOn401`, `RedeliveryOn5xx` | `redelivers \| terminal \| unknown` | `unknown` is treated as `terminal` for LF-C1 (§6.6) |
| `ErrorClassMapping` | declared list of `NotProcessed` codes | Empty means every undocumented response is `Ambiguous` |
| `StatementSource` | bool | Required for production eligibility when LF-C1 demands it |
| `CallbackEchoesMerchantReference` | bool | Registration refuses a production-eligible adapter supporting deposit or payout unless this is true **or** `StatusQuery = by_provider_or_merchant_reference`, so a success can never be unconvergeable (LF95-C5) |
| `WebhookRetrySemantics{Retries429, Retries503, HonorsRetryAfter, RetryWindow}` | struct | **Mandatory and fail-closed** for every non-MOCK payments adapter: registration refuses without it (P95-C3; closes ADR 0097 §19 condition 2). ADR 0097 §6.3's interim per-provider config key moves into this code manifest in PRH-I1, so no retry-safety property stays config-editable (S95-C12(iii)). `RetryWindow` is also the floor for receipt retention (S95-C3). |

Registration also refuses (S95-C12, LF95-C8(c)):

- an unknown enum value in any field, and a missing manifest on any non-MOCK adapter;
- `MerchantLookupAuthoritativeAfter > 0 AND < lease + CallTimeout`;
- a `NotProcessed` mapping for payouts without `IdempotentSubmission`;
- `SupportsRefund = true` (no flow exists, §5.5).

Order of refusal (planning gate §9, adopted):

1. **At registration:** an inconsistent manifest or a missing prerequisite refuses to start.
2. **At new activity:** checked at T1/T2 and T1p.
3. **Never at settlement.** Callbacks, polls and postings for in-flight attempts always
   proceed.

The existing tenant layer (`provider_capabilities`, including `status`) still narrows routing
and is unchanged.

### 10.2 Kill switch — data (migration 0102)

`payment_kill_switches(tenant_id, provider_scope TEXT ('*' or provider_id), operation_scope
('deposit' | 'payout' | '*'), engaged BOOLEAN, engaged_by_scope ('platform' | 'tenant'),
reason_code, changed_by, changed_at, version)`. It has `UNIQUE(tenant_id, provider_scope,
operation_scope)`, `FORCE RLS` with policies that also require `app.player_account_id` unset
(ADR 0093 A1 pattern), CAS on `version`, and every change audited (actor, tenant, before/after,
IP, UA, reason code). Every engage also raises an alert, not only an audit row (S95-C7).

`payment_kill_switch_release_requests` supports four-eyes release (§10.4). A guard trigger on
`payment_kill_switches` makes the four-eyes release structural (S95-C5):

1. `DELETE` is rejected, so "missing row = not engaged" cannot be produced by deleting an
   engaged row.
2. `engaged` true→false is allowed only in the same transaction that moves a
   `payment_kill_switch_release_requests` row `open→approved` with `expected_version =
   OLD.version`, an approver distinct from the requester, and an unexpired request.
3. `version` is strictly monotonic. A re-engage between request and approval invalidates the
   request through the version.

A release request is one-shot and expires (`RECOMMENDATION` 24 h). A platform-wide,
cross-tenant switch is **not built**; it is PROV-REVOKE-ALL-1, deferred. Its interim form is
a platform principal engaging each tenant's `'*'` row through the existing `WithPlatformAdmin`
pattern, with audit; such rows carry `engaged_by_scope = 'platform'` (§10.4).

### 10.3 Semantics

**Engaged means no new outbound submission for the scope.**

- It is evaluated **inside the claim statement itself** (T1+T2, T2, T1p, T12) as a `NOT EXISTS
  (SELECT 1 FROM payment_kill_switches k WHERE k.tenant_id = payment_attempts.tenant_id AND
  k.engaged AND k.provider_scope IN ('*', $provider) AND k.operation_scope IN ('*', $op))`
  subquery, under the same RLS context as the CAS (S95-C6). A misbound or unset tenant context
  therefore claims zero attempts: the claim can never succeed with the switch skipped. There is
  no time-of-check/time-of-use gap. The in-flight window is at most one already-claimed call.
- Deposits: `created` attempts are moved to `rejected` (T3, `kill_switch`) and the player sees
  "unavailable".
- Payouts: `created` attempts are **left** in `created`. Their holds stay; they are neither
  failed nor claimed until release.
- A payout claim (T1p) is refused, and the withdrawal stays `approved`.
- **Never stopped:** callbacks, `QueryStatus` polls, postings of existing exposure, and
  reconciliation.
- **Fail-safe:** the switch row is read in the claim tx, so any read error aborts the claim and
  no call is made. A missing row means "not engaged", which is the only non-engaged encoding.
  There is no cache: the switch is read from the DB on every claim and never from Redis.

### 10.4 Authority

- The switch is exposed only on the staff admin API. Permissions are
  `payments_kill_switch:engage` and `payments_kill_switch:release`, enforced server-side and
  scoped by tenant. It is never exposed on a player route; a route-table test asserts this.
- **Engage:** a single actor with a reason code. That is the safe direction, mirroring ADR
  0093 §3 "single actor on disabling".
- **Release:** four-eyes, ruled by `security` (§22.1) and not relaxed. The request is created by
  one principal and approved by a **distinct** one, reusing the ADR 0093 A1 request-binding
  shape; the §10.2 trigger enforces it in the database.
- **Platform-engaged switches** (`engaged_by_scope = 'platform'`) can be released only by
  platform principals, requester and approver both. Tenant staff of a B2B operator cannot undo a
  platform containment (S95-C7).
- The full route and permission list for the new staff surface is in §10.5.

### 10.5 New and changed staff routes (S95-C13)

| Route (staff admin API only) | Permission | Notes |
|---|---|---|
| Kill switch engage | `payments_kill_switch:engage` | Single actor, reason code |
| Kill switch release request / approve / cancel, and list | `payments_kill_switch:release` (list: `payments_kill_switch:read`) | Four-eyes; platform-engaged switches only by platform principals |
| T17 re-verify | `payments_attempt:reverify` | Sets `next_action_at` only; never calls a provider inline, so it cannot bypass the sweeper caps |
| Attempt and receipt read | `payments_attempt:read` | Read-only, RLS-scoped |
| M3 abandon | Today's withdrawal-resolve eligibility plus a reason code | CAS `state='created' AND NOT ever_possibly_sent` in SQL and trigger |
| Withdrawal submit and resolve (rewritten) | Existing permissions and eligibility | §5.2 |

For every route: never reachable with a player principal or on a player or public route (the
route-table test asserts it); permissions enforced server-side and tenant-scoped from the
authenticated context, with no tenant id taken from the path or body; object lookups run under
RLS, and another tenant's id returns 404, never data; every mutation writes audit (actor,
tenant, entity, before/after, IP, UA, reason code). The request/response shapes are pinned in
OpenAPI with a conformance test (§16.2 item 20).

---

## 11. PROV-OUTBOUND-CRED-1 (approved scope)

| Concern | Rule |
|---|---|
| Resolution point | In phase B only: `providercred.OutboundResolver.Resolve(ctx, pool, tenantID, providerID)` in its own short committed tx (exists). It already refuses under `txscope.Held`. A MOCK uses a `Synthetic` credential source with the **same** refusal, so tests exercise the rule. |
| Passing | Through `CallContext.Credential` in every request type (payments §9.1; casino `LaunchRequest.Call`; KYC `CreateVerificationInput.Call` and the `SubmitVerification` call context). The adapter builds a per-call `httpclient.Authenticator` and drops it on return (exists, ADR 0093 §5). |
| Caching | None for `OutboundCredential` beyond one call. Derived tokens are cached only in `DerivedTokenCache`, keyed (tenant, handle, fingerprint) and served only after this call's handle read (exists). The secret value cache stays inside the `Fetcher` (ADR 0094, unchanged). |
| Invalidation and rotation | A handle read happens on every call, so a revoke takes effect from the next call. The in-flight exposure is one call (ADR 0093 precision 1, accepted). Rotation activates a new handle (four-eyes, exists). `DerivedTokenCache.Put` evicts the old fingerprint (exists). |
| Timeout | A resolve budget (`RECOMMENDATION` 2 s, within the Fetcher's own bounds) separate from the provider call timeout. Exceeding it is `NotSent` (T5). |
| Outage | `ErrOutboundCredentialUnavailable` is `NotSent`: no call, no fallback credential, no unauthenticated call, no cascade, and no breaker count against the provider. The attempt stays or returns to `created` with backoff. After `max_credential_retries` (`RECOMMENDATION` 10 over the presence window for deposits), a deposit goes to T3. A payout stays `created` until the operator's M3 or recovery. |
| Tenant and provider isolation | The tenant comes only from the attempt row returned by this tenant's claim tx (S95-C9(i)), and the credential from `Resolve(tenant, provider)`. The gate **and** the adapter assert equality of tenant, provider and domain (§3.2 step 4, §9.1). The worker processes one tenant's items per claim tx. |
| No credential held by an adapter (S95-C8(c)) | A reflection test walks **constructed** registered adapter values recursively (pointers, structs, slices, maps, interfaces and unexported fields) for payments, casino and KYC, and fails on any `OutboundCredential`, `secretstore.Secret`, `httpclient.Authenticator` or derived token. Func-typed fields are forbidden unless allow-listed. A static test forbids package-level variables of those types in adapter packages, and forbids adapter packages importing `secretstore` or the Fetcher. |
| No leakage | Audit, logs, errors and receipts carry `handle_id` and `fingerprint` only. Transport errors are redacted by the gate (§3.2 step 8): no URL query string or userinfo, no vendor response body (which may echo headers). A secret-in-query MOCK test asserts the secret is absent from logs, errors, audit and receipts (S95-C8(a)). `CallContext` has redacting renderers (§9.1). |
| Observability | An alert on the `ErrOutboundCredentialUnavailable` rate per (tenant, provider) (S95-C9(iii)). |
| No credential access while a financial lock is held | This follows from INV-IO-1: phase B holds no tx, and therefore no row, advisory or projection lock. Session-level advisory locks are forbidden on the call path. `txscope` refusal is the runtime backstop. |
| Excluded | AWS IAM, IRSA, KMS, egress proxy, mTLS (PROVIDER DEPENDENT; ADR 0093 A4). |
| Tripwire | `TestOutboundPrecondition_EveryWiredAdapterIsSynthetic` stays. It may be relaxed **per domain** only after that domain's PRH implementation and `security` review. The row pointer it prints is updated to this ADR. |

---

## 12. Payment reconciliation — MOCK foundation (stream `payment_statement`, migration 0103)

### 12.1 Pipeline

1. **Fetch** (no tx). Per (tenant, provider with a registered source), call `Fetch(ctx,
   CallContext, periodStart, periodEnd)` through the §3.2 gate, with the credential resolved
   per §11. The source response body is capped, and a per-import line-count cap applies
   (`RECOMMENDATION` 1 000 000): above it the import is refused, nothing is stored, and a P1
   alert fires (S95-C11).
2. **Ingest** (short tx). Insert one `payment_statement_imports` row and its
   `payment_statement_lines`, both append-only (trigger). The import is idempotent on
   `(tenant, provider, source_label, coverage_start, coverage_end, content_digest)`.
3. **Match** (`WithTenantSnapshot`; REPEATABLE READ required and enforced; transaction-scoped
   advisory lock as in `TryRunCasinoStatementForTenant`). Read the import's lines, the
   `payment_attempts`, the unresolved `payment_provider_events` and the ledger. Write one `reconciliation_runs` row plus
   `reconciliation_mismatches` rows (`persistRun`). **Nothing else is written** (INV-IO-12).
4. **Audit.** The run's audit metadata carries the `import_id`, source label, `is_mock`,
   coverage window and line count, as `CasinoStatementInfo` does.

### 12.2 Why not the casino pattern verbatim

`CasinoStatementSource.Statement(ctx, tx, …)` reads inside the run tx. That is correct only for
a source that renders DB rows (the MOCK). A real source is provider I/O and would violate
INV-IO-1.

The payment stream therefore separates fetch from match, and persists the statement.
Persistence is also the append-only statement storage that casino_statement lists as missing.
Everything else follows the casino_statement pattern: `Stream` constant, `MismatchKind`s,
`persistRun`, snapshot isolation, scheduler wiring, `Label()` containing "MOCK",
state-type findings re-detected every run, and every mismatch a P1.

### 12.3 Keys and deterministic classification

A statement line of provider P resolves only attempts with `provider_id = P` (INV-IO-14,
S95-C1). A payout line matches on either the instruction reference (`provider_reference`) or
the Step B settlement reference (LF95-C13).

For each line, in sorted `(provider_id, provider_reference, kind, line_no)` order:

| Kind | Condition |
|---|---|
| `pay_duplicate` | More than one line with the same `(provider_id, provider_reference, kind)` in an import (reported once per key); or more than one `succeeded` attempt for one intent (platform side) |
| `pay_missing_platform_record` | A line (any status) with no attempt resolvable by `(provider_id, provider_reference)` or `merchant_reference`; or a `deposit_reversal` line with no ledger reversal or tombstone under its reference |
| `pay_reference_mismatch` | Resolved by merchant reference, but `attempt.provider_reference` is non-NULL and differs |
| `pay_asset_mismatch` | Asset differs (checked before amount) |
| `pay_amount_mismatch` | Amount differs (compared as `big.Int`; zero tolerance, reconciliation-model §1) |
| `pay_status_mismatch` | Provider `succeeded` versus platform attempt not `succeeded` ("provider-settled, platform-unposted", the LF-C1(b) case); or platform `succeeded` versus provider `declined`; or platform `declined` versus provider `succeeded` |

Then, for each platform attempt in the coverage window, excluding `created`/`rejected` (never
sent) and matched by no line:

| Kind | Condition |
|---|---|
| `pay_missing_provider_record` | The attempt is `succeeded` (ledger posted) |
| `pay_unresolved` | The attempt is `submitting`/`pending`/`ambiguous` and older than the horizon, whether the line is absent or still `pending`. Also: a verified callback receipt still unresolved after the `SettlementWindow` (a possible lost success; LF95-C5, S95-C2(ii)). These are the only age-gated checks, to avoid false P1s inside the settlement window. |

**Ledger join (LF95-C13).** Independently of the statement, in the same match tx:

| Kind | Condition |
|---|---|
| `pay_missing_platform_record` | A `deposit` or `withdrawal_completed` ledger transaction in the window, carrying a `provider_id`, that maps to no `succeeded` attempt (or to more than one) |
| `pay_status_mismatch` | A `succeeded` attempt without exactly one ledger transaction whose `(provider_id, provider_tx_id)` is the deposit's `provider_reference` or the payout's Step B settlement reference |

These rules make the reconciliation key the **ledger's** `(provider_id, provider_tx_id)`, as
reconciliation-model §2.2 requires, not only the attempt row.

`declined` attempts that are absent from the statement are **not** flagged. Whether a real
statement lists declines is PROVIDER DEPENDENT; the MOCK lists them, so the matcher tests cover
both.

**Coverage window.** Platform records outside `[CoverageStart, CoverageEnd)` are never flagged
as missing. This is the general period rule, and it keeps the MOCK honest across restarts.

### 12.4 MOCK source

`payments.MockStatementSource` (`Synthetic`; label "MOCK in-process payment provider
statement — renders the MockProvider's own records, not the platform DB; non-production").

- It renders the **MockProvider's own per-tenant records**. These are tenant-tagged once
  `CallContext` exists (§9.1).
- `CoverageStart` is the MockProvider's construction time.
- This is **not tautological** against the platform DB: a lost callback, an unposted success,
  or a dual-write orphan genuinely diverges.
- It is deterministic (sorted, no clock inside matching). It is single-process (a dev
  limitation, stated in the label).
- Divergence detection is proven by fixture sources in tests, one per kind (§16.3).
- The production guard (`providerkind`) refuses it in production.

### 12.5 Remediation (never through the stream)

| Kind | Remediation path (authoritative mechanisms only) |
|---|---|
| `pay_status_mismatch` (provider succeeded) | A `payments`-owned re-drive job (not the reconciliation stream) reads new `pay_status_mismatch` rows and requests T17 on the named attempts, so a dropped success converges without waiting for an operator (LF95-R1, adopted). An operator may also request T17. Either way: `QueryStatus` → matrix → T7/T13 posting through the normal idempotent path. The stream itself still writes nothing. |
| `pay_missing_platform_record` | Investigate: the dual-write orphan class this ADR removes, or foreign or forged activity. Credit only via LEDGER-MANUAL-ADJ-4EYES-1 (BLOCKED). |
| `pay_missing_provider_record`, `pay_amount_mismatch`, `pay_asset_mismatch`, `pay_reference_mismatch`, `pay_duplicate` | Escalate; never auto-resolve (reconciliation-model §2.2(a)); compensation via LEDGER-MANUAL-ADJ-4EYES-1 |
| `pay_unresolved` | The sweeper is already polling; the escalation (T16) alert is the same incident |

### 12.6 Amendment to reconciliation-model.md §2.2(b)

"The reconciliation job itself becomes the trigger to post the missing transaction" is
replaced by: "the reconciliation job flags it; the posting happens only through fresh
provider evidence (`QueryStatus`) applied by the payments state machine".

A statement line is not verified evidence of one payment. Treating a bulk file as posting
authority is exactly the "unreviewed path to arbitrary credits" §2.2 itself warns about.
Accepted by `ledger-finance` (LF-Q3, §21.4); `ledger-finance` edits `reconciliation-model.md`
when this ADR is ACCEPTED, together with the ledger-join rule above.

---

## 13. Data model sketch

All tables: `tenant_id NOT NULL`, `ENABLE` + `FORCE ROW LEVEL SECURITY`, `tenant_staff_scope`
policy (the 0025 pattern, which requires `app.player_account_id` unset), no player policy
(players read intents, not attempts), excluded from CDC. Every application-level length bound
equals its DB CHECK (one constant per field, S95-C2(iii)).

**Migration numbering (orchestrator re-allocation, 2026-09-27):** 0099 PROVIDER-REF-BOUND-1;
0100 KYC enforcement policy and decisions (ADR 0096); **0101 payment attempts, receipts,
triggers and backfill (this ADR; formerly allocated 0100)**; 0102 kill switch; 0103 payment
statement reconciliation.

### 13.1 Migration 0101 — payment attempts and provider-operation state (after 0099)

```sql
CREATE TABLE payment_attempts (
  id                       UUID PRIMARY KEY,
  tenant_id                UUID NOT NULL,
  operation                TEXT NOT NULL CHECK (operation IN ('deposit','payout')),
  deposit_intent_id        UUID NULL REFERENCES deposit_intents(id),
  withdrawal_request_id    UUID NULL REFERENCES withdrawal_requests(id),
  CHECK ((operation = 'deposit') = (deposit_intent_id IS NOT NULL)),
  CHECK ((operation = 'payout')  = (withdrawal_request_id IS NOT NULL)),
  attempt_no               INT  NOT NULL CHECK (attempt_no >= 1),
  provider_id              TEXT NULL,
  excluded_provider_ids    TEXT[] NOT NULL DEFAULT '{}',
  payment_method           TEXT NOT NULL,       -- 'legacy_unknown' allowed only when legacy_backfill (below)
  asset_code               TEXT NOT NULL,
  amount                   NUMERIC(38,0) NOT NULL CHECK (amount > 0),
  interactive              BOOLEAN NOT NULL,
  legacy_backfill          BOOLEAN NOT NULL DEFAULT false,                      -- LF95-C11(c)
  CHECK (payment_method <> 'legacy_unknown' OR legacy_backfill),
  merchant_reference       TEXT NOT NULL,       -- = id::text (INV-IO-3); for backfilled rows id = the parent id
  external_idempotency_key TEXT NOT NULL,       -- = 'pa:' || id::text (never sent for legacy rows; T12 forbidden)
  provider_reference       TEXT NULL CHECK (octet_length(provider_reference) <= PROVIDER_REF_MAX /* from 0099 */),
  state                    TEXT NOT NULL CHECK (state IN ('created','submitting','pending','ambiguous',
                                                          'succeeded','declined','rejected','disputed')),
  last_evidence_kind       TEXT NOT NULL CHECK (last_evidence_kind IN
                               ('sync','callback','query_status','sweeper','operator','platform','legacy')),  -- LF95-C2
  ever_possibly_sent       BOOLEAN NOT NULL DEFAULT false,
  CHECK (state <> 'created' OR NOT ever_possibly_sent),               -- INV-IO-9
  CHECK (state IN ('created','rejected') OR provider_id IS NOT NULL), -- rejected may be unrouted; T15 needs provider_id = verified provider
  CHECK (state <> 'pending' OR provider_reference IS NOT NULL),
  CHECK (state <> 'succeeded' OR provider_reference IS NOT NULL),     -- LF95-C4
  CHECK (operation <> 'deposit' OR state <> 'succeeded' OR ledger_transaction_id IS NOT NULL),
  submit_count             INT NOT NULL DEFAULT 0,
  claim_token              UUID NULL, lease_owner TEXT NULL, lease_until TIMESTAMPTZ NULL,
  first_submitted_at       TIMESTAMPTZ NULL,    -- set once by the first claim (DB clock); S95-C3
  last_sent_at             TIMESTAMPTZ NULL,    -- set by every claim (T1+T2, T1p, T2, T12); LF95-C8(b)
  next_action_at           TIMESTAMPTZ NULL,
  CHECK (state NOT IN ('succeeded','declined','rejected','disputed') OR next_action_at IS NULL),
  poll_count               INT NOT NULL DEFAULT 0,
  escalated_at             TIMESTAMPTZ NULL,
  decline_reason           TEXT NULL CHECK (octet_length(decline_reason) <= 64),   -- canonical code, S95-C10
  decline_stage            TEXT NULL CHECK (decline_stage IN ('at_submission','after_acceptance')),
  cascadable               BOOLEAN NULL,
  terminal_reason          TEXT NULL CHECK (octet_length(terminal_reason) <= 64),  -- canonical code, S95-C10
  ledger_transaction_id    UUID NULL REFERENCES ledger_transactions(id),   -- this attempt's deposit posting (LF95-C6(a))
  created_at, accepted_at, resolved_at, updated_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX ON payment_attempts (tenant_id, merchant_reference);
CREATE UNIQUE INDEX ON payment_attempts (tenant_id, external_idempotency_key);
CREATE UNIQUE INDEX ON payment_attempts (tenant_id, provider_id, provider_reference) WHERE provider_reference IS NOT NULL;
CREATE UNIQUE INDEX ON payment_attempts (tenant_id, ledger_transaction_id) WHERE ledger_transaction_id IS NOT NULL;
CREATE UNIQUE INDEX ON payment_attempts (tenant_id, deposit_intent_id, attempt_no) WHERE operation = 'deposit';
CREATE UNIQUE INDEX payment_attempts_one_live_per_intent ON payment_attempts (tenant_id, deposit_intent_id)
  WHERE operation = 'deposit' AND state IN ('created','submitting','pending','ambiguous');          -- INV-IO-8
CREATE UNIQUE INDEX payment_attempts_one_per_withdrawal ON payment_attempts (tenant_id, withdrawal_request_id)
  WHERE operation = 'payout';                                                                         -- INV-IO-8
CREATE INDEX idx_payment_attempts_due ON payment_attempts (tenant_id, next_action_at) WHERE next_action_at IS NOT NULL;
-- payment_attempts_guard (BEFORE UPDATE):
--   * allowed (OLD.state, NEW.state) pairs = §4.3 exactly, including the "Forbidden" list;
--   * last_evidence_kind rules (LF95-C2): NEW.state='declined' AND operation='payout' requires
--     NEW.last_evidence_kind IN ('sync','callback','query_status'); NEW.state='declined' for a deposit
--     requires <> 'operator'; NEW.state='succeeded' requires IN ('sync','callback','query_status');
--   * T12 (ambiguous→submitting) rejected when legacy_backfill (LF95-C11(c));
--   * a payout →rejected only from OLD.state='created' AND NOT OLD.ever_possibly_sent (M3, S95-C13);
--   * immutable: id, tenant_id, operation, subject ids, attempt_no, amount, asset_code, payment_method,
--     interactive, legacy_backfill, merchant_reference, external_idempotency_key, first_submitted_at once set;
--     provider_id, provider_reference and ledger_transaction_id NULL→value once;
--     ever_possibly_sent false→true only; submit_count and last_sent_at monotonic.
-- payment_attempts_no_delete (BEFORE DELETE): RAISE.

CREATE TABLE payment_provider_events (   -- verified callback receipts (INV-IO-10)
  id UUID PRIMARY KEY, tenant_id UUID NOT NULL, provider_id TEXT NOT NULL,
  event_type TEXT NOT NULL CHECK (event_type IN ('deposit','deposit_reversal','payout','payout_returned')),
  provider_reference TEXT NOT NULL CHECK (octet_length(provider_reference) <= PROVIDER_REF_MAX),
  original_provider_reference TEXT NULL CHECK (octet_length(original_provider_reference) <= PROVIDER_REF_MAX),
  merchant_reference TEXT NULL CHECK (octet_length(merchant_reference) <= 64),
  settlement_reference TEXT NULL CHECK (octet_length(settlement_reference) <= PROVIDER_REF_MAX),
  outcome TEXT NOT NULL CHECK (outcome IN ('pending','succeeded','declined','ambiguous')),
  amount NUMERIC(38,0) NULL, asset_code TEXT NULL CHECK (octet_length(asset_code) <= 16),
  CHECK (outcome <> 'succeeded' OR (amount IS NOT NULL AND asset_code IS NOT NULL)),       -- LF95-C4
  cascadable BOOLEAN NULL,                                                                   -- LF95-C4
  decline_stage TEXT NULL CHECK (decline_stage IN ('at_submission','after_acceptance')),    -- LF95-C4
  decline_reason TEXT NULL CHECK (octet_length(decline_reason) <= 64),                      -- canonical code
  CHECK (outcome <> 'declined' OR (cascadable IS NOT NULL AND decline_stage IS NOT NULL)),
  event_fingerprint BYTEA NOT NULL,
  disposition_at_receipt TEXT NOT NULL CHECK (disposition_at_receipt IN
      ('applied','duplicate_effect','deferred_unresolved','anomaly','unsupported_event')),
  attempt_id UUID NULL REFERENCES payment_attempts(id),   -- one-shot NULL→value
  resolution TEXT NULL CHECK (resolution IN ('applied','anomaly_cross_provider','anomaly_reference_conflict',
      'anomaly_predates_submission','anomaly_other')),     -- one-shot NULL→value (S95-C3)
  resolved_at TIMESTAMPTZ NULL,                            -- one-shot NULL→value
  received_at TIMESTAMPTZ NOT NULL DEFAULT now(),          -- DB clock
  UNIQUE (tenant_id, provider_id, event_fingerprint)
);
CREATE INDEX payment_provider_events_unresolved ON payment_provider_events (tenant_id, provider_id, provider_reference)
  WHERE resolved_at IS NULL;                               -- also the S95-C2 cap probe index
-- guard trigger: append-only except the three one-shot columns; no DELETE. No raw body, header, signature,
-- key id or credential is ever stored. Retention (PAY-ATTEMPT-RETENTION-1) must never delete receipts of
-- non-terminal attempts, or receipts younger than the longest declared WebhookRetrySemantics.RetryWindow
-- (S95-C3); `security` reviews that design.
```

**Backfill with pre-flight (LF95-C11; synthetic/dev data only, but correct regardless).** The
backfill writes **no ledger row**. It runs as one migration transaction; the pre-flight
aborts, listing the offending ids, before any write, and never coerces.

| Source row | Backfilled attempt |
|---|---|
| Every `deposit_intents` row, whatever its status | `id = intent.id` (so `merchant_reference = id::text` equals the value actually sent as `MerchantReference`, LF95-C11(b)); `attempt_no = 1`; `legacy_backfill = true`; `last_evidence_kind = 'legacy'`; `ever_possibly_sent = true` unless the intent has no `provider_id` (RG-declined, never sent) |
| intent `pending`/`ambiguous` **with** `provider_reference` | state `pending` / `ambiguous` |
| intent `pending`/`ambiguous` **without** `provider_reference` | state `ambiguous` (it may have reached the provider). **Pre-flight abort** if its `provider_id` is NULL. |
| intent `succeeded` | state `succeeded`, `ledger_transaction_id` copied. **Pre-flight abort** if the intent's `ledger_transaction_id` is NULL or its `provider_reference` is NULL. |
| intent `declined`/`failed` | state `declined` (`failed` maps to `declined`), `cascadable = false`; with no `provider_id`: state `rejected` |
| `withdrawal_requests` `submitted` | `id = withdrawal.id`; state `pending` (the reference is already set; **pre-flight abort** if it is NULL) |
| `completed` | `succeeded` |
| `failed` | `declined`, `last_evidence_kind = 'legacy'` (the trigger accepts `legacy` only in the migration's own session setting; application code cannot write it) |
| `reversed` | `succeeded` (it was omitted before; LF95-C11(d)) |
| withdrawal `payment_method` | `withdrawal_requests` has no such column, so every backfilled payout gets the sentinel `'legacy_unknown'`, allowed only with `legacy_backfill` (LF95-C11(e)); nothing is invented |

Post-checks inside the same transaction (LF95-C11(g)): `SUM(debits) == SUM(credits)` and
projection == rebuild are unchanged; every non-terminal intent and every `submitted` withdrawal
has exactly one live attempt; no two backfilled rows violate a new unique index. Any failure
rolls the whole migration back.

`deposit_intents` and `withdrawal_requests` get **no schema change**. The intent status CHECK
is unchanged. `withdrawal_requests.provider_reference` is already nullable; only its
application-level meaning changes (§4.7).

### 13.2 Migration 0102 — kill switch

```sql
CREATE TABLE payment_kill_switches (
  id UUID PRIMARY KEY, tenant_id UUID NOT NULL,
  provider_scope TEXT NOT NULL,                       -- '*' or a provider_id (charset per webhookauth.ValidProviderID)
  operation_scope TEXT NOT NULL CHECK (operation_scope IN ('deposit','payout','*')),
  engaged BOOLEAN NOT NULL,
  engaged_by_scope TEXT NOT NULL CHECK (engaged_by_scope IN ('platform','tenant')),   -- S95-C7
  reason_code TEXT NOT NULL CHECK (octet_length(reason_code) BETWEEN 1 AND 64),
  changed_by UUID NOT NULL, changed_at TIMESTAMPTZ NOT NULL, version BIGINT NOT NULL,
  UNIQUE (tenant_id, provider_scope, operation_scope));
CREATE TABLE payment_kill_switch_release_requests (
  id UUID PRIMARY KEY, tenant_id UUID NOT NULL, kill_switch_id UUID NOT NULL REFERENCES payment_kill_switches(id),
  expected_version BIGINT NOT NULL, requested_by UUID NOT NULL, requested_by_scope TEXT NOT NULL,
  reason_code TEXT NOT NULL CHECK (octet_length(reason_code) BETWEEN 1 AND 64),
  approved_by UUID NULL CHECK (approved_by IS DISTINCT FROM requested_by), approved_by_scope TEXT NULL,
  status TEXT NOT NULL CHECK (status IN ('open','approved','cancelled','expired')),
  expires_at TIMESTAMPTZ NOT NULL, created_at TIMESTAMPTZ NOT NULL, decided_at TIMESTAMPTZ NULL);
CREATE UNIQUE INDEX ON payment_kill_switch_release_requests (kill_switch_id) WHERE status = 'open';
-- payment_kill_switches_guard (S95-C5, S95-C7): no DELETE; version strictly monotonic;
--   engaged true→false only if, in the same transaction, a release request for this row moved
--   open→approved with expected_version = OLD.version, approved_by <> requested_by, now() < expires_at,
--   and, when OLD.engaged_by_scope = 'platform', requested_by_scope = approved_by_scope = 'platform'.
-- release_requests guard: one-shot status transitions (open→approved|cancelled|expired only); no DELETE.
-- Both tables: FORCE RLS, tenant policies also require app.player_account_id unset.
-- No manifest table: the manifest is code-declared (ADR 0022 §2.1). History of switch changes = audit_log.
```

### 13.3 Migration 0103 — payment statement reconciliation (MOCK)

```sql
CREATE TABLE payment_statement_imports (
  id UUID PRIMARY KEY, tenant_id UUID NOT NULL, provider_id TEXT NOT NULL,
  source_label TEXT NOT NULL CHECK (octet_length(source_label) <= 256), is_mock BOOLEAN NOT NULL,
  coverage_start TIMESTAMPTZ NOT NULL, coverage_end TIMESTAMPTZ NOT NULL, CHECK (coverage_end > coverage_start),
  line_count INT NOT NULL CHECK (line_count BETWEEN 0 AND 1000000),          -- S95-C11
  content_digest BYTEA NOT NULL, fetched_at TIMESTAMPTZ NOT NULL,
  UNIQUE (tenant_id, provider_id, source_label, coverage_start, coverage_end, content_digest));
CREATE TABLE payment_statement_lines (
  id UUID PRIMARY KEY, tenant_id UUID NOT NULL, import_id UUID NOT NULL REFERENCES payment_statement_imports(id),
  line_no INT NOT NULL, provider_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('deposit','deposit_reversal','payout')),
  provider_reference TEXT NOT NULL CHECK (octet_length(provider_reference) <= PROVIDER_REF_MAX),
  merchant_reference TEXT NULL CHECK (octet_length(merchant_reference) <= 64),                          -- S95-C11
  original_provider_reference TEXT NULL CHECK (octet_length(original_provider_reference) <= PROVIDER_REF_MAX),
  settlement_reference TEXT NULL CHECK (octet_length(settlement_reference) <= PROVIDER_REF_MAX),      -- LF95-C13
  status TEXT NOT NULL CHECK (status IN ('pending','succeeded','declined','reversed')),
  amount NUMERIC(38,0) NOT NULL, asset_code TEXT NOT NULL CHECK (octet_length(asset_code) <= 16),
  occurred_at TIMESTAMPTZ NOT NULL,
  UNIQUE (import_id, line_no));   -- duplicates across line_no are kept so pay_duplicate is detectable
-- both: append-only triggers (no UPDATE/DELETE), FORCE RLS.
-- reconciliation_mismatches.mismatch_kind widened (strict superset of 0098) with:
--   pay_missing_platform_record, pay_missing_provider_record, pay_amount_mismatch, pay_asset_mismatch,
--   pay_reference_mismatch, pay_status_mismatch, pay_duplicate, pay_unresolved.
```

---

## 14. Lock ordering — ADR 0082 Amendment A7 (accepted by `ledger-finance`, §21.3; written into ADR 0082 when this ADR is ACCEPTED)

- **L1 list extended.** `deposit_intents` and `payment_attempts` are appended to the L1 table
  list, in that order, after the existing tables. So `withdrawal_requests` comes before
  `payment_attempts`, and `deposit_intents` comes before `payment_attempts`.
- **R0, between L0 and L1** (LF95-C9(a)). The `payment_provider_events` receipt-key insert is
  an index-insertion wait that serializes duplicate deliveries, analogous to L0.1. A tx takes
  **at most one** R0, as its **first write**, and only in a callback tx or in a separate-tx
  rejection/`unsupported_event` record. Nothing that already holds an L1+ lock ever inserts a
  receipt. Deadlock-freedom depends on exactly that, so the rule is binding.
- **Parent before attempt, everywhere** (LF95-C9(c)). Every tx that locks a
  `payment_attempts` row locks its parent (`deposit_intents` or `withdrawal_requests`) first,
  including T3, T5, the per-item claim tx and per-item phase C.
- **Receipt one-shot updates** (`attempt_id`, `resolution`, `resolved_at`) happen only while
  the tx holds the attempt's parent **and** attempt locks, in ascending receipt id. The sweeper
  backstop locks the parent and the attempt first (LF95-C9(b)).
- **The batch lease tx is the sole exception** (LF95-C9(d)). It uses `FOR UPDATE SKIP LOCKED`
  only, writes only lease columns (never a state, never T2), never locks or writes a parent
  row, and never waits on a lock. Ordering it by `next_action_at` instead of by id is safe only
  because nothing in it blocks.
- **Kill-switch read takes no lock.** It is a plain `NOT EXISTS` subquery inside the claim
  statement.
- **Per-path order:**
  - deposit evidence: R0 (callback tx only) → intent `FOR UPDATE` → attempt CAS → (L2 original,
    reversal only) → L3 (`LockProjectionsForPosting` inside `Post`) → L4;
  - payout evidence: withdrawal `FOR UPDATE` → attempt CAS → L3 → L4;
  - payout claim and W-KYC: withdrawal `FOR UPDATE` (L1) → KYC gate reads → (`DenyForCompliance`
    posting: L3 → L4) or (attempt INSERT);
  - cascade insert: while holding intent and attempt, insert attempt n+1. The partial unique
    index is the only wait, and it is a key the concurrent contender needs under the same
    intent lock, so there is no cycle.
- **Harness** (LF95-C9(e)). The §16.2 item 16 harness adds the sweeper lease and claim racing a
  callback and phase C on the same intent and on the same withdrawal.
- **No lock is held across I/O.** This is the whole point of the ADR. The L1 lock that
  `LockApprovedForSubmission` held across `Withdraw` today is removed.

---


## 15. Casino launch and KYC (same analysis, minimal redesign)

### 15.1 Casino launch (`casino.LaunchGame`)

| Aspect | Specification |
|---|---|
| Intent | The `casino_launch_sessions` row (exists), `status='active'`, bounded by `expires_at`. No new state. |
| Reference / idempotency key | `SessionID` (exists in `LaunchRequest`) is the deterministic external reference. The vendor's session reference is not persisted today (unchanged). |
| Flow | Phase A: all existing checks (RG, risk, jurisdiction, capability, `supports_bet`) plus `CreateLaunchSession` plus audit `casino.launch_requested` → commit (this releases the L0.4 and L0.5 advisory locks). Health: snapshot outside the tx (§9.6 contract applied to `CasinoProvider.HealthStatus`). Phase B: resolve credential → `Launch(LaunchRequest{Call: CallContext, …})`. Phase C: success → audit `casino.launched`; failure or ambiguity → `RevokeLaunchSession` (CAS `status='active'`, exists) plus audit `casino.launch_failed`. |
| Retryability | No automatic retry. A player retry mints a new session (existing behaviour). |
| Callbacks | Unchanged: bets resolve the session. A revoked or expired session gives `ErrLaunchSessionRequired`, and no money moves. |
| Failure recovery | Crash after A: the session is active, the token was never delivered, and it expires. Crash after the vendor accepted: the session is active and usable by the vendor, the player lost the URL, and it expires. Both are harmless: no ledger effect without a verified bet on a resolvable session. |
| Reconciliation | casino_consistency and casino_statement are unchanged. CAS-STMT-IO-1 (§20) is a hard precondition on the first real casino statement source: that adapter must land already split per §12.1, and its remediation is verified (not just referenced) when it is proposed, gated on `architect` and `ledger-finance` sign-off (casino review §26). |
| Webhook re-check | S-Q3 option (a) applies to casino callbacks too: a re-check DB or transport failure returns the typed `RecheckUnavailableError` → retryable 503; definitive results stay 401 (§6.6; PRH-I2). |
| Audit | `casino.launch_requested` (new), `casino.launched` (moved to phase C), `casino.launch_failed` (new). |
| Migration | None. |

### 15.2 KYC `CreateVerification`

| Aspect | Specification |
|---|---|
| Intent | The `kyc_verifications` row inserted in phase A with `status='unverified'`, `provider_id` set and `provider_reference NULL`, plus audit `kyc.verification_requested`. |
| Reference / idempotency key | `CreateVerificationInput` gains `Call CallContext` and `ExternalReference = verification.id`. The vendor idempotency key is `"kv:" + id` **only if the vendor supports keys, which is PROVIDER DEPENDENT and recorded at intake**. Where it does not, a player retry before the orphan is known creates a second, unrelated vendor-side verification under a second platform row. That is not a financial or enforcement hazard (each row is evaluated on its own merits), but it must never be assumed deduplicated. |
| Flow | A (commit) → B (`CreateVerification`) → C: CAS `UPDATE … SET provider_reference=$r, status=$s WHERE id=$1 AND provider_reference IS NULL AND status='unverified'` plus audit `kyc.verification_submitted` (existing action, moved). |
| Retryability | No automatic retry. A player retry creates a new row (existing behaviour). An orphan `unverified` row without a reference has **no enforcement effect** (it is not verified). |
| Callbacks | KYC has **no receipt table**, so a callback that races phase C (reference unknown) returns a **retryable 5xx**, never a 200: a 200 would discard the only copy of the evidence (IC-Q1). A 200 is reserved for a callback the platform actually applied, including a duplicate or no-op apply. Whether the vendor redelivers on 5xx is **PROVIDER DEPENDENT** and is confirmed at real-vendor intake, the same discipline as §6.6/LF-C1. The merchant-reference echo is PROVIDER DEPENDENT. S-Q3 option (a) applies (re-check DB failure → 503). |
| Failure recovery | Crash after A: an orphan row, harmless. After B: an orphan vendor verification. KYC enforcement (ADR 0096) reads only platform rows, so it is harmless. |
| Migration | None. |

### 15.3 KYC `SubmitVerification` (document upload)

| Aspect | Specification |
|---|---|
| Flow | A: insert document plus audit → commit. A short read-only tx gathers the current non-rejected document set. B: `SubmitVerification(Call, providerReference, docs)` with the idempotency key `"ks:" + verification_id + ":" + sha256(sorted document ids)`. C: the existing `normalizeProviderResult` plus the terminal-guarded `updateVerificationStatus` plus audit (existing). |
| Ambiguous result (IC condition 2) | **An ambiguous, timeout or transport-error `SubmitVerification` result leaves `kyc_verifications.status` unchanged.** Phase C maps it to the existing `ProviderError` branch (audit with outcome failure, no status update); it is never passed to `statusForOutcome` and never read by `normalizeProviderResult` as a definitive outcome. §15.3 does not reuse §4.4's matrix, so this rule has its own test (§16.2 item 18). |
| Failure recovery | Identical to today's `ProviderError` outcome: the verification stays as it is, and the next upload re-submits the full set. A durable KYC submission outbox is **deferred** as KYC-SUBMIT-OUTBOX-1, and it is a **hard precondition on the first real KYC adapter**: no real KYC adapter is accepted into PRH-I2 (or later) without a durable submission outbox design landing first (IC condition 5). |
| Migration | None. |

---

## 16. Failure injection and required tests

### 16.1 Crash and failure-injection points

These are hooks in the MOCK adapter and orchestrator seams, test-only. **"Connection lost"
points (CP-D5, and every "DB error causes a rollback/5xx" case in §16.2 item 4) use one named,
deterministic mechanism** (QA change 5): a test-only `net.Conn` proxy between pgx and Postgres
(`internal/testsupport/pgfault`, name final in I1-i) that can sever the connection at a chosen
protocol point, in particular **after the COMMIT message bytes are written and before the
CommandComplete is read**, or return an injected error on the Nth statement. No sleep- or
timing-based fault injection is used anywhere except the one intentional exception in item 14.

| ID | Point | Required converged outcome |
|---|---|---|
| CP-D1 | Player path: after phase A commit (T1+T2 in one tx, §5.1), before the call | Attempt `submitting` with `ever_possibly_sent=false`, which is **not** proof it was unsent; the outcome is exactly CP-D2's (LF95-C12). For a **cascade row** (T1 only): attempt `created`; the sweeper (non-interactive) or the same request's driver claims it via T2; an interactive, abandoned row → T3 after the window. The provider never saw a `created` row. |
| CP-D2 | After the claim commit (T1+T2 or T2), before the call | `submitting`. Lease expiry → sweeper `QueryStatus` → `not_found`: authoritative per §4.5 (never acknowledged, Δ elapsed since `last_sent_at`) → `declined` with `cascadable=false`; else `ambiguous` → T12 if allowed. No double submission without the same key. |
| CP-D3 | During the call: timeout after the provider accepted | T6 `ambiguous`. A later callback or poll → T7. No cascade. |
| CP-D4 | Provider accepted; crash before phase C | The callback resolves by merchant reference (T7), or it is deferred and applied at T4, or the sweeper resolves it. Exactly one posting. |
| CP-D5 | Phase C COMMIT connection lost (unknown commit) | Re-running phase C is a no-op or completes. Exactly one posting. |
| CP-D6 | Callback tx rolls back after `ledger.Post` | Nothing committed. Redelivery posts once. |
| CP-D7 | Crash after the decline commit, before the cascade attempt runs | Attempt n+1 exists (`created`). Driven by the sweeper (non-interactive) or expired (interactive). |
| CP-D8 | Worker crash holding leases | Items reappear at `lease_until`. No double money call. |
| CP-W1 | After the T1p commit, before `Withdraw` | Withdrawal `submitted`, attempt `submitting`. Sweeper `QueryStatus` by merchant reference, else `ambiguous`. **Never `Fail`.** |
| CP-W2 | `Withdraw` accepted; crash before phase C | Resolved by callback or poll. Exactly one Step B. |
| CP-W3 | `Complete` posting fails in phase C | Rollback. The retry posts once (`provider_id:provider_tx_id`). |
| CP-W4 | Concurrent staff double-submit | Exactly one T1p. The second gets `ErrStateConflict`. **Zero** extra provider calls (the MOCK counts calls). |
| CP-W5 | `Reject` racing the claim | Exactly one wins. If the claim wins, `Reject` is a 409 and no hold is released. |
| CP-W6 | Credential store outage at payout | T5 loop. The withdrawal stays `submitted`, the hold stays, and no call is made. |
| CP-W7 | KYC revoked between T5 and the sweeper T2 re-claim | No claim, no call; the attempt stays `created`, escalated; the hold is released only by staff M3 (`kyc_denied`). |
| CP-W8 | KYC revoked while the payout attempt is `ambiguous`, before T12 | No resend; no release; resolved only by poll or callback evidence. |

### 16.2 Adversarial test list (each financial test ends with INV-IO-13)

1. **No double credit.** Concurrent duplicate success callbacks, a success callback racing the
   sweeper poll, and a success callback racing phase C: exactly one Flow 1. **≥ 100 iterations
   in CI with `-race`** (QA change 8).
2. **No double debit or payout.** CP-W4 and CP-W5 (each **≥ 100 iterations with `-race`**). T12 resubmission with the MOCK in
   idempotent mode returns the original reference; in non-idempotent mode T12 never fires.
3. **No lost successful payment.**
   - CP-D2, CP-D4 and CP-W2 converge to `succeeded`, with the ledger posted, and without any
     callback (poll only).
   - The same without any poll (callback only).
   - Callback dropped, with reconciliation then flagging `pay_status_mismatch`, followed by T17
     posting.
4. **No lost callback.** A deferred receipt applied at T4. The receipt survives a crash between
   receipt and T4, and the sweeper backstop applies it. A DB error causes a rollback and a 5xx,
   and the redelivery applies it.
5. **No unsafe retry.**
   - `Ambiguous` never re-calls `Deposit`/`Withdraw` unless `IdempotentSubmission`.
   - `NotSent` after `ever_possibly_sent=true` is refused by the trigger (direct SQL attempt).
   - A payout is never re-routed.
6. **Ambiguous never treated as success.** An ambiguous sync result, poll or callback is never
   `succeeded`, and no posting occurs. A sync `Succeeded` from a manifest without
   `SyncSuccessPossible` is treated as `Ambiguous`.
7. **Duplicate callback idempotent.** The same event ×N sequentially and concurrently gives one
   receipt, one effect and a 200 each time. A duplicate of a rejected class keeps its existing
   HTTP code.
8. **Callback before or after the internal transition.** Every row of §6.4, both orders, with
   and without a merchant-reference echo, including callback-before-T2-commit (T15 path is
   *not* taken while `submitting`).
9. **Timeout is not auto-failure.** Deposit and payout pending past the horizon give T16 only,
   with the state unchanged. A payout `not_found` is never `declined`. A deposit `not_found` is
   `declined` only with `MerchantLookupAuthoritativeAfter` elapsed.
10. **Contradictions.** T13 (post plus P1), T14 (disputed, no `Complete`), T15 (no post), and a
    mismatched amount or asset → T10.
11. **Cascade.**
    - Synchronous cascade across 3 MOCKs with full exclusion history.
    - Asynchronous decline, non-interactive: cascade via the sweeper. Asynchronous decline,
      interactive: no cascade.
    - Concurrent cascades from two decline deliveries give exactly one attempt n+1 (partial
      unique index), **≥ 100 iterations with `-race`**.
    - No cascade on ambiguous.
12. **Kill switch.**
    - Engaged between routing and T2 means no call (CAS predicate).
    - Engaged payout claim gives 503 and the withdrawal stays `approved`.
    - In-flight polls and callbacks still apply.
    - Release requires a distinct approver.
    - A player route cannot reach it (route-table test).
    - A read error means no call.
    - Audit before/after.
13. **Capability fail-closed.** Registration refuses an inconsistent manifest, an adapter
    declaring `SupportsRefund`, or a production-eligible adapter with `RedeliveryOn401 ∈
    {terminal, unknown}` and no statement source. An unsupported operation at new activity
    gives T3 or 503. An unsupported event gives `unsupported_event`.
14. **INV-IO-1.**
    - The gate refuses under `txscope` (unit test).
    - A static scan finds no adapter-method call inside a `With*` closure (repo-wide, including
      casino and KYC).
    - A capture test: while the MOCK's `Deposit`/`Withdraw`/`QueryStatus`/`Launch`/
      `CreateVerification`/`SubmitVerification` executes, `pg_stat_activity` shows no
      `idle in transaction` backend for the test's application name, and the pool's acquired
      count is 0 for the calling goroutine.
    - A pool-starvation variant (**the one intentional sleep in this plan**: a
      connection-holding fixture reused from F-POOL-1, not a race-dependent assertion): N
      concurrent slow MOCK calls (sleep > 1 s) with a 20-connection
      pool; unrelated tenant queries meet the existing latency bound. This reuses the F-POOL-1
      fixture approach **in its own isolated CI lane under the security ruling-B rules**.
15. **PROV-OUTBOUND-CRED-1.**
    - Credential tenant or provider mismatch gives `NotSent` with no call.
    - Revoke between two calls: the second call is refused.
    - Rotation: the second call uses the new fingerprint and the derived token is evicted.
    - Store outage gives T5 with no breaker count.
    - A reflection test: no registered adapter struct (payments, casino, KYC) has a field of
      type `OutboundCredential`, `secretstore.Secret`, `[]byte` named `*secret*`/`*key*`, or an
      `*http.Client` with a non-nil default authenticator.
    - Redaction of `CallContext` in logs, errors and JSON.
16. **Lock order.** ADR 0082 harness extended with deposit-evidence and payout-evidence paths
    racing reversal, `RequestWithdrawal`, `Reject` and `Fail` on the same wallet. No deadlock
    across 500 iterations (existing harness style).
17. **State-machine exhaustiveness.** A table-driven test drives every (state, evidence) cell of
    §4.4 and every forbidden transition of §4.3 via direct SQL, which the trigger must reject.
18. **Casino and KYC.**
    - Launch: crash after phase A, and crash after vendor accept. A bet on a revoked session is
      rejected with no posting.
    - KYC create: crash after A leaves an orphan row that is not verified. The callback race
      gives a retryable response.
    - KYC create: the callback race returns a retryable 5xx, never 200 (IC condition 1).
    - KYC submit: crash after the upload commit is recovered by the next upload.
    - KYC submit: an ambiguous, timeout or transport-error `SubmitVerification` result leaves
      `kyc_verifications.status` unchanged (IC condition 2).
    - Casino and KYC re-check DB failure → 503 with zero rows; revoked handle → 401 with zero
      rows (S-Q3).
19. **Ledger-finance additions (LF95-C14; each ends with INV-IO-13).**
    - Player retry with the same key, concurrent with an in-flight `submitting` attempt: one
      intent, and a MOCK call count of 1.
    - T13 with a `created` sibling: the sibling is rejected and never called.
    - A tombstone before success leads to `disputed`, with no loop and no 5xx.
    - A deferred **decline** receipt applied at T4 cascades per the persisted `cascadable`.
    - A disputed attempt leaves the intent `ambiguous`.
    - The LF95-C3 cross-attempt reference conflict gives `anomaly`, committed, with no 5xx; a
      mismatched success is committed as `disputed` with its receipt.
    - The LF95-C10 (a)–(c) KYC cases (T1p deny → `DenyForCompliance` with no attempt; CP-W7;
      CP-W8) and (d) no release after possible dispatch; (e) a deposit T2 resume re-runs RG and
      the KYC deposit gate.
    - `NotSent` on a T12 resend returns to `ambiguous`, never `created` (LF95-C1).
    - A reversal of a T13 second capture reverses the second capture, not the first
      (LF95-C6(b)).
    - `P95-C1`: `TestMigration0082_DepositIntentsProviderColumnsStayMutable` keeps passing, and
      a new test asserts the intent's provider columns mirror the latest attempt through a
      cascade.
20. **Security authorization and isolation tests (§22.5, all ten).**
    - Tenant A staff against each §10.5 route with tenant B's switch, attempt or withdrawal id →
      404/403, never data; B unchanged.
    - A player token → 401/403 on every new route (route table plus one live request each).
    - A release requester approving their own request is refused by the application **and**
      the CHECK; a direct `UPDATE engaged=false` or `DELETE` is refused by the trigger (S95-C5).
    - A platform-engaged switch cannot be released by tenant principals (S95-C7).
    - A cross-provider, same-tenant merchant-reference callback gives no posting and no state
      change, including for `created`/`rejected` targets (S95-C1); cross-tenant is pinned too.
    - The claim under a misbound or unset tenant context claims nothing and calls nothing
      (S95-C6).
    - The secret-in-query leak test and the recursive adapter reflection test (S95-C8).
    - Re-check DB error → 503 with zero rows; re-check revoked → 401 with zero rows (S-Q3).
    - Deferred-receipt cap → 503 above the cap, nothing stored; a receipt predating
      `first_submitted_at` is not applied (S95-C2, S95-C3).
    - The four 200 dispositions return byte-identical bodies apart from the request id (S95-C4).
    - OpenAPI conformance for the §10.5 routes (kill switch engage/release/approve/list, T17
      re-verify, M3, attempt read): request/response shapes pinned, OpenAPI diff checked in CI
      (QA change 4).
    - The adapter conformance suite asserts no payer-identifying vendor field reaches
      `CallbackEvent`, `StatusResult` or a statement line (S95-C10).
21. **Breaker** (QA change 6). Force a (tenant, provider) breaker closed → open → half-open →
    closed with MOCK transport failures; assert tenant B's breaker for the same provider is
    unaffected; `DefiniteDecline` does not count; a credential-store outage does not count; an
    open breaker removes the candidate from routing without a DB transaction being held.
22. **Migrations** (QA changes 2 and 3).
    - Up/down/up round-trip for 0101, 0102 and 0103 leaves no orphaned constraint, trigger or
      policy.
    - Backfill over a fixture with a pre-existing violation (two live attempts; a non-terminal
      intent with NULL `provider_id`; a `succeeded` intent with NULL `ledger_transaction_id`)
      aborts, lists the ids, and writes nothing.
    - Backfill over clean synthetic data produces exactly the §13.1 mapping, including
      `reversed` withdrawals, `id = parent id`, `legacy_backfill = true` and
      `'legacy_unknown'`; a legacy merchant-reference callback resolves; T12 on a legacy row is
      refused by the trigger (LF95-C11, LF95-C14).
    - RLS cross-tenant tests for every new table (`payment_attempts`,
      `payment_provider_events`, `payment_kill_switches`, `payment_kill_switch_release_requests`,
      `payment_statement_imports`, `payment_statement_lines`), reusing the existing
      `tenant_staff_scope` RLS harness: tenant A can neither read nor affect tenant B's rows.
    - The mismatch-kind CHECK in 0103 is a strict superset of 0098.

### 16.3 Reconciliation tests (PRH-I5)

- One divergent fixture source per mismatch kind (8), each detected exactly once, with a
  deterministic mismatch ordering.
- A clean MOCK run over a mixed history (deposits, reversal, tombstone, payouts, declines,
  cascade).
- Coverage window: a platform record before `CoverageStart` is not flagged.
- `pay_unresolved` is flagged only past the horizon.
- The run refuses outside REPEATABLE READ.
- A statement-capture test proves the match tx writes only run and mismatch rows (INV-IO-12),
  and that `Fetch` runs with `txscope.Held == false`.
- Ingest is idempotent on re-fetch of the same content.
- Append-only triggers reject UPDATE and DELETE.
- The `providerkind` guard refuses the MOCK source in production.
- The end-to-end LF-C1(b) path: a dropped success callback is flagged, the payments re-drive
  job (LF95-R1) requests T17, T17 posts it, and the next run is clean.
- The ledger join (LF95-C13): a `succeeded` attempt with no posting, and a `psp_clearing`
  posting with no attempt, are each flagged; a payout line matching only the settlement
  reference is a match.
- A statement line of provider A never matches provider B's attempt (S95-C1).
- An unresolved verified receipt older than the `SettlementWindow` is `pay_unresolved`
  (LF95-C5).
- An import above the line cap is refused with nothing stored; oversized fields are refused by
  the CHECKs (S95-C11).

### 16.4 Mutation checks (to record in the implementing review)

| ID | Mutation | Test that must fail |
|---|---|---|
| MX1 | Call the adapter inside phase A | 14 |
| MX2 | Drop the kill-switch predicate from T2 | 12 |
| MX3 | Allow `ambiguous→declined` for a payout on timeout | 9 and 17 |
| MX4 | Generate the external key per try | 2 (MOCK idempotent mode) |
| MX5 | Drop the merchant-reference resolution | 8 |
| MX6 | Skip the receipt insert | 4 |
| MX7 | Remove `payment_attempts_one_live_per_intent` | 11 |
| MX8 | Treat `not_found` as a decline without the manifest | 9 |
| MX9 | Let the reconciliation stream call `ledger.Post` | 16.3 capture |
| MX10 | Call the provider without a **committed** `submitting` claim token (INV-IO-2) | gate unit test; CP-D1/CP-W1 |
| MX11 | Bypass the CAS predicate in application code (unconditional `UPDATE … SET state`) (INV-IO-4) | 17 (application path) and 1 |
| MX12 | Post the ledger effect from phase B, or take the attempt lock before the parent (INV-IO-5) | 14 capture and 16 |
| MX13 | Set `ever_possibly_sent` without a send, or skip setting it on `Ambiguous` (INV-IO-9) | 5 |
| MX14 | Let an adapter keep the credential across two calls (INV-IO-11) | 15 (reflection and revoke-between-calls) |
| MX15 | Drop the `provider_id = verified provider` predicate from merchant-reference resolution (INV-IO-14, S95-C1) | 20 (cross-provider) |
| MX16 | Remove the `last_evidence_kind` trigger check (LF95-C2) | 17 |
| MX17 | Drop the kill-switch release trigger (S95-C5) | 20 (direct `UPDATE`/`DELETE`) |
| MX18 | Drop the `received_at >= first_submitted_at` rule (S95-C3) | 20 (pre-submission receipt) |

Every INV-IO row now has at least one mutation and a named failing test (QA change 1).

### 16.5 Placement, CI lanes and time budgets (QA change 7; `RECOMMENDATION`, confirmed by `qa` in I1-i)

| Suite | Package | Budget |
|---|---|---|
| State machine, evidence matrix, trigger and mutation tests | `internal/payments` | adds ≤ 30 s to the package |
| Crash-point and adversarial suite (§16.1, §16.2 items 1–13, 15–17, 19–22) | new `internal/payments/ioboundary`, sharing the existing Postgres harness | ≤ 180 s with `-race`, including the ≥ 100-iteration cases |
| Pool-starvation and `pg_stat_activity` capture tests (§16.2 item 14) | their own isolated CI lane, under the security ruling-B rules (blocking, no retries, name guard) | ≤ 120 s, not counted against `internal/httpserver`'s 310 s |
| Casino and KYC (§16.2 item 18) | `internal/casino`, `internal/kyc` | adds ≤ 20 s each |
| Reconciliation (§16.3) | `internal/reconciliation` (payment_statement files beside casino_statement) | ≤ 60 s |

If a budget is exceeded, `qa` re-measures and records the number; tests are never weakened to
fit a budget.

---

## 17. Conformance to ledger-finance LF-C2 and LF-C1

| Constraint | Where satisfied |
|---|---|
| LF-C2 #1: intent committed in `submitting` before any call; no tx held | D1; INV-IO-1, INV-IO-2; T2/T1p commit before phase B |
| LF-C2 #2: deterministic, persisted external key; player retry resumes | INV-IO-3; §5.1 "Idempotency keys"; for backfilled rows `id = parent id` and T12 forbidden (§13.1, LF95-C11) |
| LF-C2 #3: explicit CAS state machine; illegal and backward transitions rejected; every transition audited | §4.3; INV-IO-4; trigger with `last_evidence_kind` (LF95-C1, LF95-C2) |
| LF-C2 #4: callback resolves by merchant reference and provider reference, and accepts `submitting` | §6.1 step 4 (bound to the verified provider, INV-IO-14); §6.4; T7/T8 from `submitting`; receipts persist every applied field (LF95-C3, LF95-C4); no unconvergeable success (LF95-C5) |
| LF-C2 #5: sweeper `QueryStatus` with no tx; never auto-declines on unknown; never cascades on unknown | §7; §4.5 (LF95-C8); §4.6 |
| LF-C2 #6: cascade through an outbox, never inline in a webhook tx | D3; §4.6; §6.5 |
| LF-C2 #7: ledger posting never spans I/O; ADR 0082 order; authoritative read in the same tx | INV-IO-5; §14 (A7 with LF95-C9 scope rules) |
| LF-C2 #8: failure-injection tests end with the SUM and rebuild assertions | INV-IO-13; §16 (incl. item 19, LF95-C14) |
| LF-C2 #9: the same rule for payout dispatch | §5.2, §5.2.1 (KYC gate, LF95-C10); T1p; CP-W1..W8; INV-IO-7 |
| LF-C1: record redelivery semantics; (a) or (b) | §6.6: (b) enforced at startup; (a) approved narrowly by `security` and specified (typed `RecheckUnavailableError` → 503) |

---

## 18. Implementation work breakdown

Every item starts `NOT IMPLEMENTED`. The ADR must reach ACCEPTED (ledger-finance plus security
sign-off) before I1 starts.

### PRH-I1 — payments (+ `ledger-finance`; reviewers `security`, `code-reviewer`, `qa`)

| # | Item | Depends on |
|---|---|---|
| I1-a | Migration **0101** (tables, indexes, guard triggers, pre-flight plus backfill per §13.1, RLS tests, up/down tests) | 0099 merged; 0100 (ADR 0096) merged; ADR 0082 A7 written by `ledger-finance` |
| I1-b | Contract (§9): `CallContext` (with redacting renderers), `ErrorClass`, `StatusQuery`, `CallbackEvent` fields (`MerchantReference`, `SettlementReference`, `DeclineStage`, canonical `DeclineReason`), manifest (§10.1, including `CallbackEchoesMerchantReference` and `WebhookRetrySemantics`), `HealthStatus` contract. MOCK extended: tenant-tagged records; merchant-reference lookup; idempotent-by-key mode; `NotSent`/`NotProcessed`/`Ambiguous`/timeout/crash hooks; call counters. Conformance suite cases. | — |
| I1-c | Provider-call gate (§3.2, including the binding check and transport-error redaction) plus the outbound credential plumbing (§11) plus the synthetic credential source; static scan; recursive reflection test; secret-in-query test | I1-b |
| I1-d | `applyEvidence`, transition functions, audit; `InitiateDeposit` rewrite (takes `*db.Pool`); routing split; breaker | I1-a, I1-c |
| I1-e | Callback path: provider-bound resolution, receipts (cap, predates-submission rule), deferred application, uniform 200 bodies, `RecheckUnavailableError` → 503, removal of inline I/O and cascade | I1-d |
| I1-f | Payout dispatch and resolve handlers rewritten with the §5.2.1 KYC gate placement (consuming the `internal/kyc` enforcement function from PRH-I3 and ADR 0096's `withdrawal.DenyForCompliance`); `withdrawal.ClaimForDispatch`/`RecordProviderReference`; M3; `withdrawal-state-machine.md` update (P95-C2, before PRH-I1 is marked complete) | I1-d; PRH-I3 KYC function available |
| I1-g | Sweeper worker (§7) with its `cmd/platform-api` wiring and nudge | I1-d, I1-e |
| I1-h | Migration 0102 plus kill-switch service, guard triggers, `engaged_by_scope`, the §10.5 routes and permissions, four-eyes release, engage alerts, audit, OpenAPI; manifest registration checks (incl. the LF-C1(b), LF95-C5, LF95-C8(c), `WebhookRetrySemantics` and S95-C12 rules) | I1-b |
| I1-i | Tests §16.1, §16.2 items 1–17 and 19–22, mutations MX1–MX8 and MX10–MX18, §16.5 lanes and budgets; tripwire message update (the tripwire itself stays) | all of the above |

I1-b, I1-h and I1-a can proceed in parallel. I1-d through I1-g are sequential.

### PRH-I2 — casino plus KYC (`casino`, `identity-compliance`)

| # | Item | Depends on |
|---|---|---|
| I2-a | Shared call-gate helper (tiny; `txscope` refusal plus deadline). It is either extracted from I1-c or written first and consumed by I1-c. It is the only coordination point. | — |
| I2-b | Casino launch split (§15.1), `LaunchRequest.Call`, `HealthStatus` outside the tx, audits; casino `RecheckUnavailableError` → 503 (S-Q3) | I2-a |
| I2-c | KYC create and submit split (§15.2–15.3), `Call` on the inputs, callback race → retryable 5xx, ambiguous submit leaves status unchanged; KYC `RecheckUnavailableError` → 503 (S-Q3) | I2-a |
| I2-d | Tests §16.2 item 18, plus casino and KYC in the scans for item 14 and item 15 | I2-b, I2-c |

PRH-I2 does not depend on migrations 0101/0102/0103 and can run fully in parallel with I1.
KYC-SUBMIT-OUTBOX-1 is a hard precondition on accepting any real KYC adapter (§15.3).

### PRH-I5 — payment reconciliation (`payments` + `ledger-finance`)

| # | Item | Depends on |
|---|---|---|
| I5-a | `statement.PaymentStatementSource` leaf types (§9.5) | — |
| I5-b | Migration 0103 | 0101 (FK-free, but it reads `payment_attempts` and `payment_provider_events` at match time) |
| I5-c | Fetch (through the gate, capped) → ingest → match stream, the eight classifiers, the ledger join (LF95-C13), provider-bound matching, `TryRun…ForTenant`, scheduler wiring; the `payments`-owned re-drive job (LF95-R1) | I5-a, I5-b, I1-a |
| I5-d | `payments.MockStatementSource` | I1-b (tenant-tagged MOCK records) |
| I5-e | Tests §16.3, MX9; `reconciliation-model.md` §2.2 amendment and a new §2.2 implementation status | I5-c, I5-d |

The matcher (I5-c) can be written against fixture sources before I1-d lands.

### Specialist-owned document changes (not code)

- `ledger-finance`: ADR 0082 A7.
- `payments`: `payment-orchestration.md` §5 note; `withdrawal-state-machine.md` (P95-C2, no later
  than I1-f).
- `identity-compliance`: ADR 0096 names the T2-payout-reclaim case and routes it to M3 (IC
  condition 4; §5.2.1 here is this ADR's side).
- `ledger-finance`: `reconciliation-model.md`.
- `security`: the ADR 0093 §5 pointer.

---

## 19. Decisions

### 19.1 Human decisions (only the unavoidable one)

**HD-0095-1: manual resolution of an unresolvable payout, and of disputed attempts.** Decide who
may force-resolve (M1/M2), above what amount four-eyes applies, and whether a payout may
*ever* be declared "not paid" without provider evidence. This ties to LEDGER-MANUAL-ADJ-4EYES-1.

- This ADR **does not decide it**.
- Until decided, M1 and M2 are **BLOCKED**: no API is built, and affected attempts stay
  `ambiguous`/`disputed`, visible and P1.
- M3 (never-sent payouts) does not need the decision.

### 19.2 Specialist review questions — all resolved by the reviews

| ID | Owner | Ruling | Where written |
|---|---|---|---|
| LF-Q1 | `ledger-finance` | T13 posts to `player_cash`; no suspense account (§21.2), with LF95-C6 | §4.3 T13, §4.4 |
| LF-Q2 | `ledger-finance` | A7 accepted; R0 between L0 and L1, with LF95-C9 scope rules (§21.3) | §14 |
| LF-Q3 | `ledger-finance` | §2.2(b) amendment accepted, with the ledger join LF95-C13 (§21.4) | §12.3, §12.6 |
| S-Q1 | `security` | Engage single actor; release four-eyes, not relaxed (§22.1) | §10.2, §10.4 |
| S-Q2 | `security` | `deferred_unresolved` 200 acceptable, with S95-C2..C4 (§22.2) | §6.1, §6.2, §6.4 |
| S-Q3 | `security` | LF-C1 option (a) approved narrowly, additive to (b) (§22.3) | §6.6, §15 |
| IC-Q1 | `identity-compliance` | KYC race → retryable 5xx, never 200 (§25) | §15.2 |
| P-Q1 | `payments` | §7.3 defaults accepted; `TestMigration0082_DepositIntentsProviderColumnsStayMutable` relies on the mirror (§23) | §5.1, §16.2 item 19 |

---

## 20. Consequences, residuals and deferred items

### Positive

- The dual-write hazard (review 19 §6, High) is structurally impossible. Every money call
  follows a committed `submitting` row with a deterministic key, and every outcome converges
  through CAS evidence.
- Pool pinning by provider latency is eliminated for payments, casino launch and KYC. That is
  F-POOL-2's whole scope, including `HealthStatus` and the webhook cascade.
- The double-payout path on withdrawal submit is closed.
- Cascade history is complete.
- The platform gains a durable, auditable record of every verified callback.
- The PSP reconciliation stream exists, as a MOCK foundation.

### Costs

- A deposit is now 3 or more short transactions instead of 1.
- There is a background worker.
- The attempt and receipt tables grow with traffic. Retention and partitioning are deferred
  until a volume trigger.
- `deferred_unresolved` changes one 404 to a 200 (S-Q2, accepted by `security`), and a
  mismatched deposit success now commits `disputed` and returns the uniform 200 instead of
  rolling back to a 409 (LF95-C3, S95-C4).
- Kill-switch release needs a second person (S-Q1).
- The asynchronous interactive cascade is removed: a behaviour change, but a correct one
  (§4.6).

### Residuals, honestly stated

- **In-flight window.** A revoke, rotation or kill switch committed after a claim affects the
  next call, not the one already in flight (ADR 0093 precision 1).
- **Contradictory providers.** A provider that contradicts itself can still cause a double
  capture (T13) or a double payout (T14). The platform detects it (P1) but cannot prevent the
  provider's own behaviour. Remediation is BLOCKED on HD-0095-1 and
  LEDGER-MANUAL-ADJ-4EYES-1.
- **Vendor capabilities.** Payout resolution for a vendor with neither merchant-reference
  lookup nor idempotent submission ends in manual resolution (BLOCKED). The intake (planning
  gate §6 #11 and #16) must surface this before selection; this is a vendor-selection
  criterion.
- **MOCK reconciliation.** It is single-process and in-memory. Its evidence proves the
  matching and plumbing only, never agreement with any real provider (`MOCK`).
- **T13 with a sibling already `submitting`.** T13 rejects a `created` sibling, but a sibling
  already `submitting` may still capture a third time; it is detected under the same P1
  (`multiple_success_for_intent`), with remediation BLOCKED on LEDGER-MANUAL-ADJ-4EYES-1
  (LF95-C6(c)).
- **Pre-planted receipts from a verified sender** are neutralised by the
  `received_at >= first_submitted_at` rule and the cap; a verified sender can still post
  anything it could post after T4, because it is the same trust principal (S95-C3).
- **KYC create without vendor idempotency** can create a second vendor-side verification on a
  player retry (PROVIDER DEPENDENT; no financial or enforcement hazard).

### Findings and deferred candidates (for the orchestrator to register)

| ID | Kind | Item |
|---|---|---|
| **CAS-STMT-IO-1** | Finding, Low, not reachable today | `CasinoStatementSource.Statement(ctx, tx, …)` reads inside the run's transaction. A real casino statement source would be provider I/O inside a tx (the INV-IO-1 class). It must adopt the fetch → ingest → match split of §12.1 before the first real casino statement source; `casino` verifies the remediation (not just a reference) when that source is proposed, with `architect` and `ledger-finance` sign-off (§26). No redesign now. |
| KYC-SUBMIT-OUTBOX-1 | Deferred, **hard precondition** | Durable KYC submission outbox. No real KYC adapter is accepted without it (IC condition 5). Owner `identity-compliance`. |
| PAY-ATTEMPT-RETENTION-1 | Deferred | Retention and partitioning of `payment_attempts`/`payment_provider_events`. Trigger: a volume threshold. Binding constraint: never delete receipts of non-terminal attempts, or receipts younger than the longest declared `WebhookRetrySemantics.RetryWindow`; `security` reviews the design (S95-C3). |
| BONUS-T13-ELIGIBILITY-1 | Confirmation, owner `bonus-engine` | Confirm that a T13 second capture is intentionally not bonus-eligible (the intent link keeps pointing at the first posting) (LF95-R2). |
| VENDOR-INTAKE-REF-PII-1 | Intake item, owner `payments` + `security` | Add to the planning-gate §6 intake checklist: "the vendor's reference formats contain no cardholder or payer-identifying data", and each `NotProcessed` code's documentation source (S95-C10, S95-C12(ii)). |
| PAY-PAYOUT-CASCADE-1 | Deferred | Product decision: payout cascade. Not needed now. |
| PROV-REVOKE-ALL-1 | Existing | Cross-tenant kill switch or revoke. |

### Labels

This ADR is `NOT IMPLEMENTED` in its entirety. After PRH-I1, I2 and I5:

- the payments state machine, casino and KYC will be `IMPLEMENTED` against `MOCK` adapters
  only;
- the payment reconciliation stream will be `MOCK`;
- real-vendor behaviour (manifest values, error mapping, statement semantics, redelivery) stays
  `PROVIDER DEPENDENT`;
- M1 and M2 are `BLOCKED`.

---

## 21. Ledger-finance review

- Reviewer: `ledger-finance` (financial-invariant owner; author of LF-C1/LF-C2, review 19 §4/§6)
- Date: 2026-09-27. Base: HEAD `eb15ac6`, working-tree text of this ADR (sections §0–§20).
- Scope: §4, §6, §7, §12, §14 and §16 per the sign-off list, plus the §17 LF-C2 mapping,
  LF-Q1–LF-Q3, Amendment A7, the migration 0100 backfill and the ADR 0096 payout-gate fit.
  Code read to check the design against reality: `internal/payments/orchestrator.go`
  (`postDepositSuccess`, `receiveDepositCallback`, `receiveDepositReversalCallback`,
  `postDepositReversalTombstone`), `internal/withdrawal/withdrawal.go` (`Complete`, `Fail`,
  `LockApprovedForSubmission`), `internal/httpserver/withdrawal_handlers.go:826-850`,
  `internal/bonus/deposit_sweep.go`, migrations 0025, 0026 and 0082 §1.2.

### Verdict: **SIGN-OFF WITH CONDITIONS**

The architecture is correct and closes the review 19 §6 dual-write hazard. The pattern is
commit-intent → call without a tx → CAS evidence, with a deterministic key, one live attempt,
the payout asymmetry and detection-only reconciliation. `SUM(DEBITS) == SUM(CREDITS)` is never
at risk from this design, because every posting remains one `ledger.Post` in one short tx.

The conditions below fix concrete defects in the ADR **text**, two of which contradict the
schema the ADR itself defines (LF95-C1, LF95-C2). They also close convergence gaps where a real
payment could stay unposted, or a hold could be mishandled, without any P1. **LF95-C1 to
LF95-C12 must be folded into this ADR before it is marked ACCEPTED. LF95-C13 and LF95-C14 gate
PRH-I1/PRH-I5 being labelled `IMPLEMENTED`.** No redesign is needed.

### 21.1 LF-C2 constraints 1–9: is §17 true?

| LF-C2 | Met? | Finding |
|---|---|---|
| #1 intent committed before the call; no tx held | **Met** | T2 (deposit) and T1p (payout) commit before phase B; gate step 2 requires the committed `claim_token`; INV-IO-1 has API-shape, runtime, static and capture enforcement. CP-D1 text is inconsistent with §5.1 (LF95-C12), but that does not affect safety. |
| #2 deterministic persisted key; player retry resumes | **Met for new rows; not for backfilled rows** | INV-IO-3 and §5.1 are correct. Legacy intents and withdrawals were sent with `MerchantReference = intent.ID` / `withdrawal id` (`orchestrator.go:596`, `withdrawal_handlers.go:840`) and never with `pa:<id>`. A backfilled attempt with a fresh id breaks both lookup and T12 safety (LF95-C11). |
| #3 CAS state machine, illegal/backward rejected, audited | **Met, with two defects** | §8 `NotProcessed` → "T5 with `ever_possibly_sent` still true" is forbidden by the §13.1 CHECK and by T5's own guard (LF95-C1). INV-IO-7's trigger check keys on `evidence_kind`, which lives only on the audit row, so the trigger cannot see it (LF95-C2). |
| #4 callback resolves by merchant ref **and** provider ref; accepts `submitting` | **Met, with gaps** | Merchant-reference resolution is not bound to the verified provider. I concur with S95-C1 and extend it in LF95-C3. Success evidence without a provider reference has no ledger key. Deferred receipts do not persist `cascadable`/`decline_stage` (LF95-C4). |
| #5 sweeper `QueryStatus` without tx; no auto-decline or cascade on unknown | **Met, with conditions** | The payout asymmetry (§4.5) is correct. The deposit authoritative-not-found rule needs tightening (LF95-C8). An adapter with no merchant-reference path at all leaves CP-D4/CP-W2 unconvergeable (LF95-C5). |
| #6 cascade through an outbox, never inline in a webhook | **Met** | D3, §4.6 and §6.5. `handleDecline → attemptDeposit` and `resolveAmbiguous` are removed from the callback path. Restricting async cascade to non-interactive attempts is correct. |
| #7 no posting across I/O; ADR 0082 order; authoritative read in the same tx | **Met, subject to A7 conditions** | The per-path orders in §14 are correct. The sweeper batch claim and receipt-row updates are not yet placed in the order (LF95-C9). |
| #8 required tests, each ending with SUM and rebuild | **Mostly met** | INV-IO-13 plus §16. There is no explicit test for "player retry with the same idempotency key while the attempt is in flight → exactly one provider call, one intent". It was listed in LF-C2 #8 (LF95-C14). |
| #9 same rule for payout dispatch | **Met, subject to the KYC gate** | T1p puts the claim and `approved→submitted` in one tx under L1. Staff double-submit gets `ErrStateConflict` before any call. The L1 lock is no longer held across `Withdraw`. The ADR 0096 C5 placement needs explicit text (LF95-C10). |

### 21.2 LF-Q1: T13 posts to `player_cash`, not a suspense account

**Ruling: post to `player_cash`. No new account type.**

A verified, amount- and asset-matching success means the platform received the player's money,
so `psp_clearing` must be debited and the liability is owed to the player. A suspense account
would understate a real player liability and still need a manual release posting. That posting
is BLOCKED (LEDGER-MANUAL-ADJ-4EYES-1), so the funds would be stranded. The refund of an
unwanted second capture is a business process through the normal reversal flow or a four-eyes
adjustment. It is not a reason to mis-state the ledger. This ruling holds only with LF95-C6,
because T13 as drafted collides with three existing mechanisms:

- **The immutable intent link.** `deposit_intents.ledger_transaction_id` is frozen once set
  (migration 0082 §1.2 trigger). A second posting for the same intent cannot be recorded
  there.
- **Reversal resolution.** `receiveDepositReversalCallback` resolves the original deposit
  through the intent row.
- **The tombstone.** A reversal that arrived first leaves `(provider_id, provider_tx_id)`
  occupied by a tombstone (`postDepositReversalTombstone`), so the late T7/T13 `ledger.Post`
  returns an error. Under this ADR that means a tx rollback, a 5xx, and a sweeper that re-polls
  and re-fails for ever.

### 21.3 LF-Q2: Amendment A7

**Accepted, with the placement and scope rules in LF95-C9.** Appending `deposit_intents` then
`payment_attempts` after the existing L1 tables is consistent with every path in the ADR:

- deposit evidence: intent → attempt → (L2) → L3 → L4;
- payout evidence: `withdrawal_requests` → `payment_attempts` → L3 → L4.

I checked that no current path holds either new table's lock and then takes an earlier L1 table.
`internal/bonus/deposit_sweep.go` joins `deposit_intents` with plain reads, and deposit posting
calls no bonus code.

"R0" is accepted as a named step **between L0 and L1**. It is the receipt-key index-insertion
wait. A tx takes at most one, as its first write, and nothing that already holds an L1+ lock
ever inserts a receipt. Its deadlock-freedom depends on exactly that, so the rule is binding.

The A7 text is written into ADR 0082 by `ledger-finance` when this ADR is ACCEPTED, not before.

### 21.4 LF-Q3: the reconciliation-model §2.2(b) amendment

**Accepted.** A bulk statement line is not verified evidence of one payment. Posting from it
would be exactly the "unreviewed path to arbitrary credits" §2.2 warns about. Detection-only
(INV-IO-12, MX9) plus posting only through fresh `QueryStatus` evidence in the state machine
(T17) is the correct split. One condition applies: the §2.2 reconciliation key is the
**ledger's** `(provider_id, provider_tx_id)`, and §12.3 matches statement lines only against
`payment_attempts`. The stream must also join the ledger (LF95-C13). Otherwise a divergence
between an attempt and the ledger (for example a `succeeded` attempt with no posting, or a
`psp_clearing` posting with no attempt) goes undetected. `ledger-finance` edits
`reconciliation-model.md` when the ADR is ACCEPTED.

### 21.5 Adversarial check

| Hazard | Result |
|---|---|
| Double credit | Safe. Guards: the attempt CAS (`succeeded` is terminal for T7), `(tenant, provider_id, provider_reference)` uniqueness on attempts, the ledger's `(tenant, provider_id, provider_tx_id)` plus idempotency-key backstops, and receipt dedupe. Residual: T13 is two real captures, by design (§21.2). Two defects, fixed by LF95-C3 and LF95-C6: a cross-provider merchant-reference callback (S95-C1), and a `created` cascade sibling that is still driven after T13. |
| Double debit / double payout | Safe on the platform side. Guards: T1p under L1 plus `UNIQUE(withdrawal_request_id)`, T12 only with the same key and only when the provider dedupes, a payout is never re-routed, and a payout never fails without definite decline evidence. Two conditions: backfilled legacy attempts must never T12, because the provider never saw their key (LF95-C11), and a `NotProcessed` payout resend must go through T12 only (LF95-C1). Residual: T14 (a contradictory provider), which is detected and P1. |
| Lost successful payment | Converges through a callback, a poll or reconciliation plus T17, with **one gap**: no merchant-reference echo and no merchant-reference status query, plus a crash between provider accept and phase C, leaves a deferred success receipt that is never applied and an attempt that can never be polled. LF95-C5 closes this. A second path: a tombstone collision loops for ever (LF95-C6). |
| Lost callback | Safe. The receipt is committed in the same tx as its effect, and a failure returns 5xx. `deferred_unresolved` returns 200 only after a durable receipt. Condition: the receipt must hold every field needed to apply it later (LF95-C4), and old deferred receipts must alert (LF95-C5). |
| Unsafe retry | Safe for new rows (INV-IO-9, T5 guard, T12 gating). Unsafe for backfilled rows (LF95-C11) and for the `NotProcessed` wording (LF95-C1). |
| Ambiguous treated as success | Safe. INV-IO-6: `applyEvidence` is the only writer. Without `SyncSuccessPossible`, a sync success is treated as `Ambiguous`. Mismatches go to T10. |
| Duplicate callback idempotency | Safe. The fingerprint is canonical rather than raw bytes, and a duplicate still applies idempotently. Condition: a mismatched success (T10) must be **committed**. Today's typed 409 path rolls back and leaves the attempt live. §6.2's "keep reviewed codes" must not mean "roll back the state effect" (LF95-C3). |
| Callback before or after the internal transition | Safe. Phase C's CAS fails harmlessly after a callback applied, and deferred receipts are applied at T4/T9 in the same tx. Condition: LF95-C4. |
| Timeout ≠ failure where the provider may have accepted | Safe. A timeout after dispatch is `Ambiguous` and a settlement timeout is T16 only. A deposit `not_found` decline must not apply to an attempt the provider once acknowledged (LF95-C8). |
| Payout failed only on a definite decline (hold release) | Correct in design. It needs a trigger-visible evidence column (LF95-C2). No KYC outcome, operator action or timeout may release a possibly-dispatched hold (LF95-C10). M2 stays BLOCKED on HD-0095-1. M3 is safe only because `created` guarantees the payout was never sent (INV-IO-9). |
| Reconciliation detects divergence and never writes the ledger | Never writes: correct (INV-IO-12, MX9, statement-capture test). Detects: incomplete without the ledger join (LF95-C13). |
| Deposit "authoritative not-found" decline and late success | Accepted as money-safe for the platform, because a late success still posts via T13. Conditions in LF95-C8. |
| Backfill (0100) | Defective as drafted (LF95-C11): intents in progress without a reference are skipped, legacy merchant references are lost, T12 is possible on keys that were never sent, `reversed` withdrawals are omitted, and the payout `payment_method` does not exist on `withdrawal_requests`. |
| The 14 crash points | CP-D2–D8 and CP-W1–W6 converge as stated, subject to LF95-C5 for CP-D4/CP-W2 without echo. CP-W1 is correctly never `Fail`: a `submitting` attempt with `ever_possibly_sent=false` is **not** proof it was not sent, because the flag is only written in phase C. CP-D1 contradicts §5.1 (LF95-C12). |
| ADR 0096 payout KYC gate | Fits: T1p is "the last commit before the first `Withdraw`". Re-gating the sweeper T2 re-claim, gating T12, and the no-release-after-possible-dispatch rule must be written into this ADR (LF95-C10). Checked against the working-tree ADR 0096 (revision in progress). Re-check once that revision lands. |

### 21.6 Conditions

**LF95-C1 (ADR defect: §8 `NotProcessed` contradicts INV-IO-9).** "Treated as T5, but
`ever_possibly_sent` stays true" violates `CHECK (state <> 'created' OR NOT ever_possibly_sent)`
and T5's own guard. Replace it with the following:

- If the vendor documents that the request left no trace, the outcome is `NotSent` and takes T5.
- Otherwise the outcome takes T6 (`ambiguous`, `ever_possibly_sent=true`, `next_action_at=now()`).
  A resend is **only** T12, which requires `IdempotentSubmission`, for deposits **and** payouts.
  Delete "Yes for deposits".
- A `NotSent` result on a T12 resend (`ever_possibly_sent` already true) returns to `ambiguous`
  via T6. It never takes T5.

Add `submitting→ambiguous` on NotSent-after-resend explicitly to §4.3.

**LF95-C2 (ADR defect: INV-IO-7 is not trigger-enforceable).** Add
`last_evidence_kind TEXT NOT NULL` to `payment_attempts`, set in the same UPDATE as every state
change. The guard trigger enforces these rules on it:

- `*→declined` for `operation='payout'` requires `last_evidence_kind ∈ {sync, callback,
  query_status}`;
- any `→declined` for a deposit requires `≠ operator`;
- `→succeeded` requires `∈ {sync, callback, query_status}`.

The audit record carries the same value.

**LF95-C3 (resolution binding; concur with S95-C1 and extend it).**

- Merchant-reference resolution matches only `attempt.provider_id = verified provider_id`, as
  S95-C1 requires.
- If the provider reference and the merchant reference are both present and resolve to
  **different** attempts, or if the evidence's provider reference is already bound to another
  attempt, the outcome is an `anomaly` receipt plus P1, with no state change and no posting.
  This must be decided **before** any write, so it can never surface as a unique violation
  followed by a 5xx redelivery loop.
- A mismatched success (T10) is **committed** in the domain tx together with its receipt,
  whatever HTTP code §6.2 returns. The state effect is never rolled back to produce a 409.

**LF95-C4 (evidence completeness).**

- T7/T13 require a non-empty `provider_reference` in the evidence, or an already-set
  `attempt.provider_reference`. That value is the deposit ledger `provider_tx_id`. Success
  evidence without one is treated as `ambiguous` plus a poll.
- `payment_provider_events` must persist every field `applyEvidence` consumes:
  - `cascadable`;
  - `decline_stage`;
  - `decline_reason` (bounded).
- Add `CHECK (outcome <> 'succeeded' OR (amount IS NOT NULL AND asset_code IS NOT NULL))`.
- A deferred receipt that lacks a required field can never be applied as success or as a
  cascade.

**LF95-C5 (no unconvergeable success).**

- The manifest gains `CallbackEchoesMerchantReference`.
- Registration refuses a production-eligible adapter supporting deposit or payout unless it has
  `CallbackEchoesMerchantReference = true` **or** `StatusQuery = by_provider_or_merchant_reference`.
- Verified deferred receipts still unapplied after the resolution horizon raise a P1. The §12
  stream reports them as a platform-side kind (reuse `pay_unresolved`, or add one) so a lost
  success is never silent.

**LF95-C6 (T13 and posting integrity; conditions of the LF-Q1 ruling).**

- (a) Each deposit posting is linked on `payment_attempts.ledger_transaction_id`. The intent's
  `ledger_transaction_id` is written only while it is NULL; the 0082 trigger already forbids
  repointing it.
- (b) §5.4 reversal resolution uses `payment_attempts (provider_id, provider_reference)` →
  **the attempt's** `ledger_transaction_id`, never the intent's. Otherwise a reversal of the
  second capture reverses the first.
- (c) The T13 tx, holding intent → attempt locks, also moves any sibling `created` attempt to
  `rejected` (T3, `intent_succeeded`). T2's CAS predicate adds `NOT EXISTS (succeeded attempt
  for the same intent)`. The race in which a sibling is already `submitting` is a stated
  residual under the same P1.
- (d) If T7/T13 finds a tombstone on `(provider_id, provider_reference)`, the attempt moves
  terminally to `disputed` (`reversal_tombstone_precedes_success`, P1), with no posting and no
  error. It is never a rollback/5xx/re-poll loop. Add this cell to §4.4.

**LF95-C7 (intent projection).** When an intent has a `disputed` attempt and no `succeeded`
attempt, its status is `ambiguous`, never `declined`. A `declined` intent invites the player to
retry while funds may have been captured.

**LF95-C8 (authoritative not-found, §4.5).** A deposit may take T8 on `not_found` only when all
of these hold:

- (a) `accepted_at IS NULL AND provider_reference IS NULL`. The provider never acknowledged the
  attempt. Otherwise `not_found` means T11/`ambiguous`.
- (b) Δ is measured from the **latest** send. T12 must update a `last_sent_at`.
- (c) Registration refuses `MerchantLookupAuthoritativeAfter < lease + CallTimeout`, so a lookup
  cannot outrun a call still in flight.
- (d) The resulting decline carries `cascadable=false`. This is enforced in code, not only
  stated in §4.6.

A late success after that decline is T13 (posted plus P1). Covered by §16.2 item 9.

**LF95-C9 (A7 scope rules, binding for acceptance).**

- (a) R0 sits between L0 and L1. A tx takes at most one, as its first write, only in a callback
  tx or in a separate-tx rejection/`unsupported_event` record.
- (b) Receipt one-shot UPDATEs (`attempt_id`, `applied_at`) happen only while the tx holds the
  attempt's parent **and** attempt locks, in ascending receipt id. The sweeper backstop locks the
  parent and the attempt first.
- (c) Every tx that locks a `payment_attempts` row locks its parent (`deposit_intents` or
  `withdrawal_requests`) first, including T3, T5 and per-item phase C.
- (d) The §7.2 batch claim tx is the sole exception. It uses `FOR UPDATE SKIP LOCKED` only,
  writes only lease columns and T2, never locks or writes a parent row, and never waits on a
  lock. This works because the lease is ordered by `next_action_at`, not by id, and it is safe
  only because nothing in the claim tx blocks.
- (e) The §16.2 item 16 harness adds the sweeper claim racing a callback and phase C on the same
  intent and the same withdrawal.

**LF95-C10 (ADR 0096 payout KYC gate fit; my ADR 0096 C4/C5, binding here).**

- (a) §5.2 phase A order: approver eligibility → L1 `withdrawal_requests` → **KYC gate** →
  kill-switch predicate → T1p. On deny, `DenyForCompliance` commits from `approved` (a hold
  reversal via the `Reject` shape, `kyc_denied`), no attempt is created and no call is made.
  List it in §4.3/§4.7 as a pre-dispatch withdrawal transition.
- (b) The sweeper's T2 re-claim of a payout attempt in `created` (after T5) **re-runs the gate**
  in the claim path. On a non-pass there is no claim and no call, and the attempt stays
  `created`, escalated (T16-style) to a compliance case. The hold is released **only** by M3
  with reason `kyc_denied`. This is safe only because of INV-IO-9, and it is never automated.
  Because of LF95-C9(d), the gate read runs in a per-item tx that locks the parent first, not in
  the batch claim.
- (c) T12 for a payout re-runs the gate. On a non-pass there is no resend and the attempt stays
  `ambiguous`, resolvable only by poll or callback.
- (d) Once T1p has committed, no KYC outcome ever triggers `Fail`, a hold reversal or any
  automated release. Only T8 evidence or M3-from-`created` can.
- (e) A deposit T2 re-claim (player resume or sweeper) re-runs the RG and KYC deposit gates
  (ADR 0096 C4(b)).
- (f) Re-verify these rules against ADR 0096's revised text once that revision lands.

**LF95-C11 (migration 0100 backfill).**

- (a) Create an attempt for **every** intent whose status is non-terminal (`pending`,
  `ambiguous`), whether or not it has a `provider_reference`. With no reference, the attempt
  state is `ambiguous`. If `provider_id` is NULL on a non-terminal intent, the pre-flight aborts
  and lists the ids.
- (b) The backfilled attempt's `id` is the parent's id (intent id or withdrawal id), so
  `merchant_reference = id::text` equals the value actually sent (INV-IO-3 holds).
- (c) Add a `legacy_backfill BOOLEAN` column. T12 is forbidden when it is true (by trigger),
  because the provider never received `pa:<id>`.
- (d) Withdrawal mapping is explicit:
  - `submitted` → `pending` (the reference is already set);
  - `completed` → `succeeded`;
  - `failed` → `declined` with `last_evidence_kind='sync'` marked legacy;
  - `reversed` → `succeeded`. Include `reversed`; it was omitted.
- (e) `withdrawal_requests` has no `payment_method`. Backfilled payouts take an explicit
  `'legacy_unknown'` sentinel (or a column nullable only when `legacy_backfill`); nothing is
  invented.
- (f) Intents with status `failed` map to `declined`. An intent in `succeeded` with a NULL
  `ledger_transaction_id` aborts the pre-flight. `ledger_transaction_id` is copied.
- (g) The backfill writes no ledger row. A post-check asserts SUM equality and
  projection == rebuild unchanged, and that every non-terminal intent or `submitted` withdrawal
  has exactly one live attempt.

This backs QA's §24 item 2.

**LF95-C12 (text consistency).** CP-D1 says "attempt `created`" after phase A, but §5.1 puts T1
and T2 in the same phase A tx. Either split T2 into its own short tx (then CP-D1 stands) or
restate CP-D1 as `submitting` with the CP-D2 outcome.

**LF95-C13 (reconciliation ledger join; PRH-I5).** The match tx also checks the following, all
detection-only:

- every `succeeded` attempt has exactly one ledger transaction whose `(provider_id,
  provider_tx_id)` is the deposit's `provider_reference`, or the payout's Step B settlement
  reference;
- every `deposit`/`withdrawal_completed` ledger transaction in the window maps to exactly one
  `succeeded` attempt;
- payout statement lines match on either the instruction reference or the settlement reference.

Any miss is a P1 mismatch.

**LF95-C14 (tests; PRH-I1-i additions, each ending with INV-IO-13).**

- Player retry with the same key, concurrent with an in-flight `submitting` attempt: one
  intent, and a MOCK call count of 1.
- T13 with a `created` sibling: the sibling is rejected and never called.
- A tombstone before success leads to `disputed` with no loop.
- A deferred **decline** receipt applied at T4 cascades per the persisted `cascadable`.
- A disputed attempt leaves the intent `ambiguous`.
- The LF95-C3 cross-attempt reference conflict gives `anomaly` with no 5xx.
- The LF95-C10 (a)–(c) KYC cases.
- The LF95-C11 fixtures: legacy T12 refused by trigger, legacy merchant-reference callback
  resolves, a non-terminal intent without a reference is backfilled.
- A mutation removing the LF95-C2 trigger check must fail item 17.

**Recommendations (non-blocking).**

- **LF95-R1.** A `payments`-owned job, **not** the reconciliation stream, sets
  `next_action_at=now()` (a T17 equivalent) on attempts named by a new `pay_status_mismatch`,
  so a dropped success converges without waiting for an operator. Reconciliation itself still
  writes nothing.
- **LF95-R2.** `bonus-engine` should confirm that a T13 second capture is intentionally not
  bonus-eligible. `deposit_sweep.go` finds deposits via `deposit_intents.ledger_transaction_id`,
  which LF95-C6(a) keeps pointing at the first posting.
- **LF95-R3.** The §9.2 `Amount int64` carries forward the existing platform-wide int64 amount
  path (the same as `ledger.EntryInput`). This is not new here, but it is noted for any
  18-exponent asset routed through `PaymentProvider`. Custody-asset precision stays with ADR 0008
  and its open decisions.

Custody, settlement-timing and HD-0095-1 matters are not ruled on here. M1/M2 remain BLOCKED.
Everything in this ADR remains `NOT IMPLEMENTED`.

---

## 22. Security review

- **Reviewer:** `security`, 2026-09-27, against HEAD `eb15ac6` (design review of this PROPOSED
  ADR; no code exists to review).
- **Verdict: APPROVE WITH CONDITIONS.** The D1 boundary, INV-IO-1/-2/-11, the refusal to treat
  a statement line as posting authority (§12.6), and "success only on verified, matching
  evidence" (INV-IO-6) are sound and are the right shape. The ADR may move to ACCEPTED on the
  security side once the conditions below are written into it (text only). **S95-C1 is a
  design defect with a concrete credit-without-funds scenario; PRH-I1 must not start without
  it.** Every condition is also an implementation-review item for PRH-I1/I2/I5.
- **In scope:** §6 (callbacks, receipts, dispositions), §9 (contract, `CallContext`), §10
  (manifest, kill switch), §11 (PROV-OUTBOUND-CRED-1), §12 fetch/ingest, §13 table shapes as
  they bear on isolation, secrets and PII, the new and changed staff routes, and the ruling on
  LF-C1 option (a).
- **Not in scope:** ledger correctness of T13/A7 (`ledger-finance`), sweeper tuning
  (`payments`), any real vendor behaviour, ADR 0097's own mechanisms (reviewed there), and any
  penetration test. Passing this review does not make the implementation secure; PRH-I1, I2
  and I5 each need their own `security` code review, and the outbound tripwire stays until
  then (§11 "Tripwire", agreed).

### 22.1 S-Q1 — kill-switch authority: RULED

**Engage: single actor with a mandatory reason code. Release: four-eyes, as proposed. Not
relaxed.** Engage is the money-safe direction (it only stops *new* submissions and never stops
settlement, §10.3), so it must never wait for a second person; that mirrors ADR 0093 §3.
Release re-opens outbound money movement, often after a credential compromise or a
misbehaving provider. A single compromised or coerced staff account must not be able to undo
an incident containment. The operational cost (a second person to resume after an outage) is
accepted.

Conditions: S95-C5, S95-C6, S95-C7.

### 22.2 S-Q2 — `deferred_unresolved` → 200 after a durable receipt: ACCEPTABLE, with conditions

**Oracle.** No new oracle. The disposition is reached only after `VerifyCallback`, the
post-verification re-check and PROVIDER-REF-BOUND-1 validation, so an unauthenticated caller
still sees only ADR 0091's uniform 401 or ADR 0097's pre-verification 429/503. For a
*verified* sender, the change removes an oracle that exists today: 404 says "this reference is
unknown to the platform"; a uniform 200 does not. This holds only if all 200 dispositions are
byte-identical (S95-C4).

**Why a 200 is correct.** A 404 may be terminal for a vendor, which is silent loss of a real
payment event. ADR 0097 §6.1 forbids a 2xx for an *unprocessed* event; a durably receipted,
deferred event is processed (INV-IO-10), so there is no conflict.

**Replay.** An exact redelivery collapses on `UNIQUE (tenant_id, provider_id,
event_fingerprint)`, and then applies idempotently. Freshness (timestamp windows, nonces)
stays with ADR 0091 and the adapter scheme. The fingerprint is not a replay control by
itself: its protection lasts only as long as the row. So PAY-ATTEMPT-RETENTION-1 must not
purge receipts of non-terminal attempts, or any receipt younger than the longest vendor
redelivery window (S95-C3).

**Abuse by a verified sender.** This means a compromised provider credential, a hostile
provider, or a buggy vendor. Today, an unresolvable callback writes nothing. After this ADR,
each distinct fingerprint writes one bounded row, and it *stays pending* until something
claims its reference. Two concrete risks follow:

- **Storage growth.** The size of each row is bounded by PROVIDER-REF-BOUND-1 and the §13.1
  CHECKs. The row *count* is bounded only by ADR 0097 B1's rate multiplied by time, because
  retention is deferred. B1 limits the rate; it does not limit the backlog. → S95-C2.
- **Pre-planting.** A sender can store a `succeeded` receipt for a `provider_reference` it
  expects to be assigned to a future attempt. T4 applies it in the same transaction that
  learns the reference. That gives no more power than the same sender posting after T4 (it is
  the same trust principal, and amount and asset must still match). However, a receipt that
  *predates the submission it claims to describe* cannot be legitimate. Refusing to apply it
  costs nothing and closes the reference-recycling case. → S95-C3.

**Interaction with ADR 0097.** Correct as specified: B1/B2 run on the `VerifiedCallback`
before the domain transaction, so a receipt is never written for a limited request (ORD-3). A
deferred 200 is still counted by B1 like any admitted request.

**Interaction with PROVIDER-REF-BOUND-1.**
- Oversized references are rejected at step 3, before the receipt insert. That is
  deterministic, non-retryable, and writes no row.
- The §13.1 CHECKs are the backstop. If a CHECK violation is ever reached (the application
  bound and the DB bound disagree), it surfaces as a 5xx. That fails safe (redelivery, no
  silent loss), but it must alert, because it means the adapter validation has drifted.
  → S95-C2.

### 22.3 S-Q3 — LF-C1 option (a): RULED — APPROVED, narrowly, additive to (b)

A DB error during the **post-verification** re-check (`HandleRecheckSQL` inside
`ReceiveVerifiedCallback`, ADR 0094 §5) may return a **retryable 503**, instead of today's
uniform 401. The constraints are:

1. **Only a transport or DB failure qualifies:** a connection error, a serialization or
   statement failure, or a context deadline at the re-check. A *definitive* result, meaning
   the handle is not found, not active, revoked, expired, or its fingerprint or key id does
   not match, stays the uniform 401 with `credential_unavailable`, exactly as today. The
   classification is typed: a new `RecheckUnavailableError`, distinct from `AuthError`. It
   is never derived from error strings. Anything unclassified defaults to **401**. That keeps
   the fail-closed direction: an unknown error is never a retry invitation that could mask a
   revocation.
2. **No oracle.** The re-check runs only after a valid signature, so the 401/503 split is
   visible only to a verified sender, and it reveals only "our DB was unhealthy". It is
   independent of signature validity; ADR 0091 T9 is unaffected. The 503 body and headers
   are ADR 0097 §6.1's generic 503 (with `Retry-After`). There is no reason text.
3. **Nothing is written.** The domain transaction rolls back: no receipt, ledger entry,
   tombstone, audit row or casino rejection record. ADR 0094 review item 6's test
   (`TestReceiveVerified_RecheckDBErrorRollsBack`) is extended to assert the 503 as well as
   the zero rows.
4. **Bounded and observable.** A metric plus an alert on the re-check-unavailable rate per
   (domain, tenant, provider). A persistent non-transient DB fault (for example a broken
   grant or RLS policy) would otherwise turn into indefinite vendor retries.
5. **Additive, not a replacement.** The §6.6 startup rule, option (b), still applies to every
   production-eligible adapter whose `RedeliveryOn401` is `terminal` or `unknown`: a
   revocation 401 is still terminal for such a vendor.
6. **Scope.**
   - It applies to all three webhook domains, through the shared `ReceiveVerifiedCallback`
     error typing: payments in PRH-I1, and casino and KYC in PRH-I2.
   - It does **not** extend to *pre-verification* DB errors, which stay uniform 401. That
     extension would need its own review against ADR 0091's uniformity tests, and it is not
     approved here.

### 22.4 Findings and conditions

| # | Sev. (once reachable) | Finding / failure scenario | Condition |
|---|---|---|---|
| **S95-C1** | **High** (design defect; blocks PRH-I1 start) | §6.1 step 5(b) resolves an attempt by `(tenant, merchant_reference)` without binding it to the **verified provider**. Tenant T routes a player's deposit to provider B. The player abandons the redirect, so no funds move. Provider A's verified credential for T (compromised, or a hostile PSP) then sends `succeeded` with that attempt's merchant reference and the player-chosen amount and asset. T7 credits `player_cash` with no money received. The merchant reference is an attempt UUID and can reach the player via the redirect, so it is not a secret. In the same way, a callback naming a `created`/`rejected` attempt of another provider triggers T15, and any provider of the tenant can then push other providers' attempts into `disputed` (a P1 flood and a denial of service on deposits). | Merchant-reference resolution matches only `attempt.provider_id = VerifiedCallback.provider_id`. The provider id always comes from the verified identity, never the payload. A merchant reference that matches an attempt with a different or NULL `provider_id` changes **no** state. It is receipted as `anomaly` with an alert, and T15 is **not** taken. T15 applies only when `attempt.provider_id` equals the verified provider. The same binding applies to §6.4's deferred application, the sweeper backstop and §12 matching (a statement line resolves only attempts of its own provider). Test: a cross-provider, same-tenant merchant-reference callback yields no posting and no state change. Mutation: dropping the predicate must fail that test. |
| S95-C2 | Medium | Receipt backlog is unbounded under a verified sender (§22.2), and a reached CHECK is silent drift. | (i) A per-(tenant, provider) cap on **unapplied** receipts, checked in the callback transaction with a bounded probe (`LIMIT cap+1` on the partial index; `RECOMMENDATION` 10 000). Above the cap, a `deferred_unresolved` event returns a retryable **503** plus a P1 alert. It never returns a 200 without storing, and never a 404. (ii) A deferred receipt that no attempt has claimed after the manifest `SettlementWindow` (24 h if undeclared) is surfaced as a P1 alert/metric ("unmatched verified callback"). It never creates an attempt (§6.5 agreed). (iii) The application-level bounds equal the §13.1 CHECKs (one constant per field). A CHECK violation reached at insert maps to 5xx plus an alert. |
| S95-C3 | Medium | A pre-planted or stale receipt is applied to a later attempt that reuses a reference, and retention could weaken replay suppression. | Deferred application (T4/T9 and the sweeper backstop) applies a receipt only if `received_at >= attempt.submitted_at` of the **first** claim. Both values come from the DB clock. An earlier receipt is marked `anomaly`, not applied, and alerts. PAY-ATTEMPT-RETENTION-1 must not delete receipts of non-terminal attempts, or receipts younger than the longest declared vendor redelivery window. `security` reviews that design. |
| S95-C4 | Low | Response-body variance between `applied`, `duplicate_effect`, `deferred_unresolved` and `anomaly` would re-create the reference-existence oracle for a verified sender. | All 200 dispositions return an identical body, apart from the request id. The disposition is recorded only in the receipt, audit and metrics. Test: byte comparison across the four dispositions. |
| S95-C5 | Medium | Kill-switch release bypass. Nothing in §13.2 prevents a single actor from releasing via `UPDATE … SET engaged=false`, or by `DELETE` (missing row = not engaged, §10.3). Either one silently defeats the four-eyes release. | A DB trigger on `payment_kill_switches` enforces three things. (1) `DELETE` is rejected. (2) `engaged` true→false is allowed only in the same transaction that moves a `payment_kill_switch_release_requests` row `open→approved`, with `expected_version = OLD.version` and an approver distinct from the requester, following the ADR 0093 A1 binding shape. (3) `version` is strictly monotonic. The release request is one-shot and has an expiry (`RECOMMENDATION` 24 h). A re-engage between request and approval invalidates the request through the version. Tenant policies on both tables require `app.player_account_id` unset (the ADR 0093 A1 pattern). |
| S95-C6 | Medium | The "missing row = not engaged" encoding fails open if the switch is read under the wrong or unset tenant context (FORCE RLS returns zero rows). | The switch predicate is evaluated **inside** the T2/T1p CAS statement, as a `NOT EXISTS` subquery with an explicit `tenant_id = payment_attempts.tenant_id`, both under the same RLS context. A misbound context then claims zero attempts, and the claim can never succeed with the switch skipped. Test: running the claim under another tenant's context claims nothing and calls nothing. A switch-read error aborts the claim (agreed, §10.3). |
| S95-C7 | Medium | Tenant-scoped release of a platform-engaged switch. Under the hybrid model a B2B operator's staff could release a switch that platform security engaged (for example after a credential compromise). | Record `engaged_by_scope ∈ {platform, tenant}`. A switch engaged by a platform principal (interim PROV-REVOKE-ALL-1 form, via the existing `WithPlatformAdmin` pattern with audit) can be released only by platform principals, requester and approver both. Every engage emits an alert, not only an audit row. |
| S95-C8 | Medium | PROV-OUTBOUND-CRED-1 leak paths not covered by §11. (a) A Go `*url.Error` from the transport embeds the full request URL, and vendors that take an API key in the query string would leak it into logs and errors. (b) The adapter-side binding check is per-adapter discipline. (c) The reflection test is type- and name-based and misses closures, interfaces, maps, package-level variables and unexported nested values. | (a) The gate maps transport errors to `ErrorClass` plus an allow-listed reason. Where any URL is logged it is logged without its query string and userinfo. A test uses a secret-in-query MOCK and asserts the secret is absent from logs, errors, audit and receipts. (b) The **gate** checks `Credential.TenantID/ProviderID/Domain` against the `CallContext` before calling the adapter, as well as the adapter's own check (§9.1), so neither alone is load-bearing. The domain is `payments`, `casino` or `kyc` per caller. (c) The reflection test walks **constructed** registered adapter values recursively (pointers, structs, slices, maps, interfaces and unexported fields). It fails on any `OutboundCredential`, `secretstore.Secret`, `httpclient.Authenticator` or derived token. It forbids func-typed fields unless allow-listed. A static test forbids package-level variables of those types in adapter packages, and forbids adapter packages importing `secretstore` or the Fetcher. `CallContext` has its own `String/GoString/Format/LogValue/MarshalJSON` that render only the credential's existing redacted form. |
| S95-C9 | Low | Credential resolution timing and scope. | Agreed as specified: `Resolve` runs in phase B only; its own short transaction commits before the fetch; it is refused under `txscope.Held`; there is no fallback credential and no unauthenticated call; an outage is `NotSent` with no breaker count and no cascade; derived tokens are cached only in `DerivedTokenCache` after this call's handle read; the in-flight exposure is one call. Additions: (i) the `CallContext` tenant is taken only from the attempt row returned by *this* tenant's claim transaction, never from a payload, cache or earlier attempt; (ii) the sweeper runs under the ordinary app role with RLS, with no BYPASSRLS, and its audit actor is a named system principal; (iii) an alert fires on the `ErrOutboundCredentialUnavailable` rate per (tenant, provider). |
| S95-C10 | Low | Receipt and attempt text fields could carry vendor free text. That text may echo PII or cardholder fragments into `decline_reason`/`terminal_reason`, which would pull PCI or GDPR data into our store. | `decline_reason` and `terminal_reason` hold canonical, allow-listed codes, never vendor text. The adapter conformance suite asserts that no payer-identifying vendor field (name, email, IBAN, PAN or masked PAN, wallet address) maps into `CallbackEvent`, `StatusResult` or a statement line. The receipt contents in §13.1 are otherwise accepted: references, outcome, amount, asset and fingerprint; no raw body, headers, signature, key id or credential; bounded; FORCE RLS; excluded from CDC. The vendor-intake item "the reference format contains no cardholder data" is added to the §6 intake checklist. |
| S95-C11 | Low | §9.5/§13.3 statement ingestion is unbounded. `merchant_reference`, `original_provider_reference`, `asset_code` and `source_label` have no length CHECK. There is no line-count or body cap, so a faulty or hostile source can exhaust memory or storage. `CallContextLike` is looser than `CallContext`. | CHECKs mirror §13.1 (`merchant_reference ≤ 64`, `original_provider_reference ≤ PROVIDER_REF_MAX`, bounded `asset_code`/`source_label`). The source response body is capped, and a per-import `line_count` cap applies (`RECOMMENDATION` 1 000 000; above it the import is refused and a P1 alert fires). `Fetch` takes the real `CallContext`, with the credential resolved per §11 and the same gate. The §12 fetch-outside-tx design and the "detection only" rule (INV-IO-12, §12.6) are agreed. |
| S95-C12 | Low | Capability manifest fail-closed. | Agreed as specified: code-declared, never tenant-editable, refused at registration and new activity, never at settlement. Additions: (i) unknown enum values and a missing manifest on a non-MOCK adapter refuse registration; (ii) each `NotProcessed` code cites its vendor-documentation source in the intake; registration refuses `NotProcessed` for payouts without `IdempotentSubmission`; (iii) ADR 0097 §6.3's interim per-provider config key for `WebhookRetrySemantics` moves into this code manifest in PRH-I1, so no retry-safety property stays config-editable. |
| S95-C13 | Medium | New and changed staff routes: kill switch engage, release request and release approve (plus list), T17 re-verify, M3 abandon, the rewritten withdrawal submit and resolve, and any attempt or receipt read. | Each route is on the staff admin API only; the route-table test asserts none is reachable with a player principal or on a player or public route. Permissions are enforced server-side and tenant-scoped from the authenticated context, with no tenant id taken from the path or body: `payments_kill_switch:engage`/`:release`, `payments_attempt:reverify`, `payments_attempt:read`, and M3 with today's resolve eligibility plus a reason code. Object lookups run under RLS, and another tenant's id returns 404, never data (test for each route). Every mutation writes audit (actor, tenant, entity, before/after, IP, UA, reason code). T17 only sets `next_action_at` and never calls a provider inline, so staff cannot use it to bypass the sweeper caps (agreed). The M3 CAS requires `state='created' AND NOT ever_possibly_sent` in SQL and in the trigger. |

### 22.5 Required authorization and tenant-isolation tests (for `qa`, added to §16)

1. A tenant A staff token against each new route with tenant B's switch, attempt or
   withdrawal id returns 404 or 403 and never data, and B's rows are unchanged.
2. A player token gets 401 or 403 on every new route (the route-table test, plus one live
   request per route).
3. The release requester approving their own request is refused by the application **and** by
   the CHECK. A direct `UPDATE engaged=false` or `DELETE` is refused by the trigger (S95-C5).
4. A platform-engaged switch cannot be released by tenant principals (S95-C7).
5. Cross-provider merchant-reference callbacks give no posting and no state change (S95-C1).
   Cross-tenant callbacks are impossible by construction (a verified tenant), and a test pins
   it.
6. The claim under a misbound tenant context claims nothing (S95-C6).
7. The secret-in-query leak test and the recursive adapter reflection test (S95-C8).
8. The re-check DB error gives a 503 with zero rows, and a re-check revoked result gives a 401
   with zero rows (S-Q3).
9. The deferred-receipt cap gives a 503 above the cap, and a pre-submission receipt is not
   applied (S95-C2/C3).
10. The four 200 bodies are identical (S95-C4).

### 22.6 Launch relevance

S95-C1, C5, C6, C8 and C13 are **launch-blocking for the first non-MOCK payments adapter**,
together with the existing PROV-OUTBOUND-CRED-1 and F-POOL-2 rows. The rest must be closed in
PRH-I1/I2/I5 before their respective `security` code reviews. None of this is a claim that the
platform is secure or launch-ready; production launch authorization remains the human's
decision.

---

## 23. Payments review

**Verdict: APPROVE WITH CONDITIONS**

Reviewed as `payments` owner, scope §4–§12 and §18 per the ADR's own sign-off list, plus P-Q1
(§19.2) and the ADR 0097/architect `WebhookRetrySemantics` cross-review item.

**P-Q1 (§19.2).**

- **§7.3 defaults.** Accepted as reversible engineering `RECOMMENDATION` values (15 s tick, 20
  batch, 16 global / 4 per-(tenant, provider) concurrency, 60 s lease, 30 s/30 min backoff, 24 h
  horizon, 30 min presence window, 3 resubmits). `MaxCascadeDepth = 3` is unchanged from today's
  running value. No objection; these should be re-measured against real traffic once a
  non-MOCK adapter exists, exactly as the ADR already says for the burst/rate numbers elsewhere.
- **Does an existing adapter test rely on today's inline webhook cascade? Yes.**
  `TestMigration0082_DepositIntentsProviderColumnsStayMutable`
  (`internal/payments/migration_0082_immutability_integration_test.go:111`) exists specifically
  to pin `deposit_intents.provider_id`/`provider_reference` as mutable in place, because
  `handleDecline → attemptDeposit → setIntentAttempt` today overwrites them on the *same* intent
  row on cascade. §5.1 keeps `deposit_intents` as an unchanged mirror that "keep[s] mirroring
  the latest routed attempt for existing readers," so this test should keep passing once cascade
  authority moves to `payment_attempts` — but that must be proven, not assumed (P95-C1).

**The three behaviour changes — all accepted:**

1. **Async cascade removed for interactive/redirect methods (§4.6).** Correct and overdue. An
   asynchronous decline of an interactive attempt cascading to a new provider is a latent defect
   today: the new redirect URL can never reach a player who has already left the page. Confining
   post-decline cascade continuation to non-interactive methods, driven by the sweeper, is a bug
   fix, not a lost capability. The player-retry-with-new-idempotency-key fallback for the
   interactive case is the correct, and only sound, alternative.
2. **Unresolved verified callbacks get 200 after durable receipt, not 404 (§6.2
   `deferred_unresolved`).** Accepted. The 200 is issued only *after* the event passes
   verification and is durably receipted, and the response is identical whether or not the
   reference ever resolves, so it opens no existence oracle. Concur with S-Q2's framing that
   this is a post-verification-only change.
3. **`submitted` withdrawals may lack a `provider_reference` (§4.7).** Accepted; this is the
   direct fix for the double-payout hazard (LF-C2 #9, hazard #5 in §1). `provider_reference`
   moving from "set at submission" to "set once by T4/T9/T7" is the correct fix, but
   `docs/architecture/withdrawal-state-machine.md` lines 92–93 still document `provider_id` and
   `provider_reference` as "set on `approved` -> `submitted`" today. That doc is stale the
   moment this ADR is implemented and is `payments`-owned per §18's "Specialist-owned document
   changes" list; it must be updated no later than I1-f, not left implicit (P95-C2).

**8-state model vs. routing / cascade-on-decline / health-based failover (Blueprint via
`docs/architecture/payment-orchestration.md` §4–§6 and `docs/decisions/0022`) — nothing is
lost:**

- **Routing** (`payment-orchestration.md` §4: tenant/brand → jurisdiction → currency/asset →
  method → amount → health) is unchanged in substance. §9.6's split of `RouteProvider` into
  `ListRoutingCandidates(tx)` (in-tx) and `Rank(candidates, health)` (no-tx) only relocates the
  health read outside the transaction; the resolution order and dimensions are untouched.
- **Cascade-on-decline** (`payment-orchestration.md` §5) is preserved and materially improved.
  §5 itself documents today's limitation: a single `deposit_intents` row keeps only the latest
  attempted provider, so `providerExclusionSoFar` can exclude only the most recently tried
  provider once a callback arrives asynchronously. The attempt-per-try model (§4.6, T1 on every
  cascade with `excluded_provider_ids = all previous attempts' providers`) closes that gap
  completely — this is a strict improvement over the documented current behaviour, not a
  reduction.
- **Health-based failover** (`payment-orchestration.md` §6: `ProviderHealth`, `circuit_state`)
  is preserved. §9.6 tightens `HealthStatus` to an in-memory, no-I/O snapshot contract and adds
  an orchestrator-owned breaker per `(tenant, provider)` fed by `ErrorClass` — a strict superset
  of today's in-memory health/circuit design (which this ADR does not turn into a stored table
  either), not a different mechanism.
- The one genuine behaviour reduction is the interactive-cascade restriction already addressed
  above, and it is a correctness fix, not a lost feature.

**Sweeper design (§7).** Sound. Tenant round-robin reusing the existing
`activeTenantIDs` reconciliation-scheduler pattern; a short claim tx using
`FOR UPDATE SKIP LOCKED` plus per-row lease/`lease_owner`, so a crashed worker's items reappear
at `lease_until`; global and per-(tenant, provider) concurrency caps that keep one tenant's slow
provider from starving another (the F-POOL-1 lesson, correctly generalized); exponential backoff
with jitter and a cap; and escalation (T16) at the manifest's `resolution_horizon` with no state
change. §7.4's claim that at most one live money call per attempt exists (entry to `submitting`
is only via CAS T2/T12) is correct and is the actual mechanism that prevents a double
submission, not the lease.

**Provider-neutral contract (§9).** Endorsed, with one gap:

- The closed outcome set (`NotSent`/`NotProcessed`/`DefiniteDecline`/`Ambiguous`, §8) is
  exhaustive and is what makes INV-IO-7 (no payout failure without definite decline evidence)
  enforceable by CAS and trigger rather than by convention — this is the single load-bearing
  design choice in the ADR from a payments-correctness standpoint, and it is right.
- `StatusQuery` by either provider or merchant reference (§5.3, §9.2, manifest field
  `StatusQuery`) is correct and is specifically what makes the withdrawal
  `submitted`-with-no-reference case resolvable.
- The no-I/O `HealthStatus` snapshot plus the platform breaker per `(tenant, provider)` (§9.6)
  is correct and is the direct fix for F-POOL-2's health-check pool-pinning hazard (hazard #4 in
  §1).
- Refunds reserved, `NOT IMPLEMENTED` (§5.5): correctly scoped. No flow initiates a refund
  today; refusing an adapter that declares `SupportsRefund=true` before a flow exists is the
  right fail-closed default, and the label is accurate per CLAUDE.md's no-fake-completion rule.
- Retry/timeout classification (§8) is correct throughout, in particular that a timeout *after*
  dispatch is always `Ambiguous` and never a failure, and that `NotProcessed` requires the
  vendor to *document* the response as safe-to-resend — every undocumented outcome correctly
  degrades to `Ambiguous`.
- **Gap: `WebhookRetrySemantics` is not in the §10.1 manifest table.** ADR 0097 §19 (payments
  review) condition 2 already commits `payments` to requiring
  `WebhookRetrySemantics{Retries429, Retries503, HonorsRetryAfter, RetryWindow}` as a
  **mandatory, fail-closed** field in this ADR's capability manifest, and that condition was
  accepted. §10.1's manifest field table as drafted here does not list it. This must be added
  before this ADR is treated as satisfying that condition (P95-C3) — it is a table-row addition,
  not a design change, since §18's I1-h ("manifest registration checks (incl. LF-C1(b) startup
  rule)") is already the right place to enforce it.

**MOCK adapter scope (§9.2, §16.1).** Correctly bounded. Tenant-tagged records,
merchant-reference lookup, idempotent-by-key mode, and the `NotSent`/`NotProcessed`/
`Ambiguous`/timeout/crash hooks are state-machine and test-harness plumbing to exercise the
contract described above — none of it invents a vendor wire format, signature scheme, or
vendor-specific error taxonomy. This matches CLAUDE.md's "mocks/sandboxes, clearly labelled"
rule and ADR 0022 §6's requirement that the MOCK pass the same conformance suite as a real
adapter, not a special-cased one.

**Work breakdown PRH-I1 a–i (§18).** Sequencing (I1-b/h/a in parallel; I1-d→e→f→g sequential) is
correct and consistent with the code dependencies described in §1 and §14. Two acceptance
criteria are missing from the item descriptions, both closed by the conditions below rather than
requiring resequencing: the migration-0082 mirror-invariant regression (I1-d/e, P95-C1) and the
`WebhookRetrySemantics` manifest field (I1-h, P95-C3).

**Conditions for APPROVE (P95-C#):**

- **P95-C1.** I1-d/I1-e must keep `TestMigration0082_DepositIntentsProviderColumnsStayMutable`
  passing and add an explicit test asserting `deposit_intents.provider_id`/`provider_reference`
  continue to mirror the latest `payment_attempts` row through a cascade, so §5.1's "keep
  mirroring" claim is proven by a test, not assumed.
- **P95-C2.** `docs/architecture/withdrawal-state-machine.md` (currently documenting
  `provider_id`/`provider_reference` as "set on `approved` -> `submitted`") must be updated to
  the §4.7 semantics no later than I1-f, before PRH-I1 is marked complete.
- **P95-C3.** Add `WebhookRetrySemantics{Retries429, Retries503, HonorsRetryAfter, RetryWindow}`
  to the §10.1 operation manifest table as a mandatory, fail-closed field for every non-MOCK
  payments adapter (registration refuses without it), which is what closes ADR 0097 §19
  condition 2 and the architect cross-review item on this ADR's own text, not just on the interim
  config key.
- **P95-C4 (informational, not blocking).** On LF-Q1 (T13's double post to `player_cash` versus
  a suspense account): `payments` prefers the ADR's stated default — post and raise P1 — over a
  new suspense-account type at this stage, since it keeps the ledger reflecting real received
  funds without adding an account type outside this ADR's scope. This is `ledger-finance`'s
  decision to make; recorded here only as the requesting domain's preference.

None of these conditions require a redesign of §4–§12; all four are documentation or test
additions to sections already inside this ADR's scope. Payments sign-off is granted on that
basis, conditional on P95-C1–C3 landing in PRH-I1/I1-f/I1-h as described.

---

## 24. QA test-plan review

**Verdict: CONFIRMED WITH CHANGES.**

Scope of this review is §16 only (crash points, adversarial groups, reconciliation tests,
mutations) plus the invariant table (§2) as the checkable contract those tests must close.

### What is solid

- The plan is invariant-first, not scenario-first: every adversarial group (§16.2 items 1–18)
  and every crash point (§16.1 CP-D1..CP-W6) states a **converged outcome**, which is a
  measurable pass/fail (state, posting count, call count), not "it worked." This is the right
  shape for financial code and satisfies "no fake completion."
- Crash injection is specified as **hooks in the MOCK adapter and orchestrator seams,
  test-only** (§16.1 heading), not timing/sleep races. The one place a sleep appears (item 14's
  pool-starvation variant, "N concurrent slow MOCK calls (sleep > 1 s)") is a deliberate
  connection-holding fixture reused from F-POOL-1, not a race-dependent assertion — acceptable,
  but it should be called out explicitly as the one intentional exception so a future reviewer
  doesn't read it as a stray sleep.
- Every financial adversarial test is required to end with INV-IO-13 (SUM(debits)==SUM(credits)
  plus projection==rebuild), which is exactly "prove no double credit/debit" made checkable
  rather than asserted.
- Tests run against real Postgres (triggers, RLS, `FOR UPDATE`, REPEATABLE READ, `pg_stat_activity`
  checks in item 14) rather than in-process mocks of the DB layer, which satisfies "do not rely
  solely on mock unit tests for financial correctness" — the MOCK is only the *provider*, never
  the ledger or the transaction boundary.
- The human's specific list is each directly covered: no double credit (#1, INV-IO-13), no
  double debit/payout (#2, CP-W4/W5), no lost successful payment (#3, three independent
  convergence paths: poll-only, callback-only, reconciliation+T17), no lost callback (#4,
  INV-IO-10), no unsafe retry (#5), ambiguous never silently success (#6, #9), duplicate
  callback idempotent (#7), callback before/after internal transition (#8, §6.4 both orders),
  timeout not auto-failure (#9), reconciliation detects divergence (§16.3, one fixture per
  mismatch kind).
- Repeated-run discipline exists at least for lock ordering ("no deadlock across 500
  iterations," item 16) and duplicate-callback ("sequentially and concurrently," item 7).

### Required changes before this test plan can gate `IMPLEMENTED`

1. **Mutation coverage is incomplete against the invariant list.** §16.4's MX1–MX9 map cleanly
   to INV-IO-1, 3, 6/7, 8, 10, 12, and the kill switch, but there is no mutation exercising
   INV-IO-2 (calling the provider without a *committed* `submitting` claim token — distinct from
   MX1, which is calling before an attempt exists at all), INV-IO-4 (bypass the CAS predicate
   itself in application code, as opposed to the trigger-rejection direct-SQL test in item 17),
   INV-IO-5 (post the ledger effect from inside phase B, or out of ADR 0082 lock order), INV-IO-9
   (flip `ever_possibly_sent` without a real send), or INV-IO-11 (let an adapter cache a
   credential across two calls). Add MX10–MX14 (or fold into existing IDs) so every INV-IO row
   has at least one mutation and a named failing test, per the ADR's own "checkable contract for
   qa" framing in §2.
2. **No migration up/down test, and no explicit 0100-backfill pass/fail test.** The PRH list
   requires migration up/down including the 0100 backfill; §13.1 describes the backfill's
   abort-on-violation behavior in prose but §16 never turns it into a test. Add: (a) up/down/up
   round-trip for 0100, 0102, 0103 leaves no orphaned constraints or triggers; (b) backfill over
   a fixture set with a pre-existing two-live-attempt violation aborts and lists the offending
   ids without partial writes; (c) backfill over clean synthetic data produces exactly the
   states §13.1 specifies.
3. **No RLS cross-tenant test named for the four new tables.** `payment_attempts`,
   `payment_provider_events`, `payment_kill_switches`/`_release_requests`, and
   `payment_statement_imports`/`_lines` are all `FORCE RLS` (§13), but §16 has no item asserting
   that a session scoped to tenant A cannot read or affect tenant B's rows in any of them (the
   existing `tenant_staff_scope` pattern presumably has a reusable harness — reuse it here by
   name).
4. **No API/OpenAPI conformance test for the new staff-admin surface.** The kill-switch
   engage/release endpoints (§10.4) are new HTTP surface with new permissions
   (`payments_kill_switch:engage/release`); §16.2 item 12 tests the domain effect and the
   route-table exclusion for players, but nothing pins the request/response shape or an OpenAPI
   diff for these routes plus the `T17` re-verify endpoint.
5. **Deterministic mechanism for "connection lost" fault points is unstated.** CP-D5 ("Phase C
   COMMIT connection lost") and the general "DB error causes a rollback" cases in §16.2 item 4
   need a named, deterministic injection mechanism (e.g., a `pgx` connection wrapper or
   `net.Conn` proxy that severs after the wire bytes for COMMIT are sent but before the ack is
   read) — otherwise this is either untestable or accidentally timing-dependent. Name the hook
   before implementation, not during.
6. **No breaker state-transition test.** §9.6 defines a closed/open/half-open breaker per
   (tenant, provider), fed by `ErrorClass`, and §16.2's list has no adversarial case forcing it
   through open → half-open → closed, verifying a tenant-A breaker does not affect tenant B, or
   that `DefiniteDecline` does not count against it. This is the "provider outage" case from the
   PRH list and is currently only implied by CP-W6 (credential outage, not transport outage).
7. **CI lane/time budget and package placement are not stated.** §16.2 item 14 says the
   pool-starvation variant runs "in its own isolated CI lane," which is right, but no time
   budget is given anywhere in §16, and no package location is proposed for the new suite.
   Given the existing `internal/httpserver` (~310 s) and full-suite (~600 s) budgets, propose:
   state-machine/evidence-matrix and mutation tests in `internal/payments` (fast, no adapter
   I/O, should not materially move the `internal/payments` budget); the crash-point and
   adversarial suite in a new `internal/payments/ioboundary` package sharing the existing
   Postgres test harness; the pool-starvation and `pg_stat_activity` capture tests
   (§16.2 item 14) in their own CI lane, budgeted separately and **not** counted against the
   `internal/httpserver` 310 s figure, since they intentionally hold slow connections. Reconciliation
   tests (§16.3) belong under `internal/reconciliation/payment_statement` alongside the existing
   casino_statement suite. This needs a number from `qa`/`payments` before I1-i lands, not an
   open-ended "isolated lane."
8. **Repeat-run / flake bound not stated for most concurrency cases.** Only the lock-order
   harness (500 iterations) and duplicate-callback ("sequentially and concurrently") specify a
   repetition count. CP-W4/W5, the concurrent-cascade case (item 11), and the concurrent
   duplicate-success-callback case (item 1) should each state a minimum iteration count (e.g.
   ≥100 runs in CI, `-race` enabled) so "no double X" is a statistically meaningful claim rather
   than a single lucky interleaving.

None of the above blocks starting PRH-I1 work — items 1–8 are additions to I1-i (§18) and to
§16 itself, not redesigns of the boundary. This ADR stays `NOT IMPLEMENTED`; when I1-i is
written, it must close items 1–8 and this section's verdict updates. `qa` does not sign off
`IMPLEMENTED` for PRH-I1 until they are closed or a specialist explicitly accepts the gap in
writing.

---

## 25. Identity-compliance review

Reviewer: `identity-compliance`, 2026-09-27. Scope: §15 (KYC create/submit split), IC-Q1
(§19.2), and the payout KYC gate contract against ADR 0096 (revised concurrently). Withdrawal
financial posting shape, ledger accounts and lock-order arithmetic are `ledger-finance`'s scope
(§12 of 0096), not re-litigated here.

### IC-Q1 — the KYC callback-race response class (§15.2)

**Resolved: retryable `5xx`, never a bare `200`.** §15.2 says a callback racing phase C "gets a
retryable response class, so the vendor redelivers after C commits," but does not say which
class. Payments can safely turn an unresolved race into a `200` (§6.2 `deferred_unresolved`)
only because `payment_provider_events` durably receipts the event first (INV-IO-10) — the
evidence is never lost even if nothing applies yet. §15.2 is explicit that KYC has **no receipt
table**. A `200` on an unresolved KYC callback would durably discard the only copy of that
evidence, which is worse than today's behaviour, not equivalent to it. §15.2 must state
plainly: an unresolved race returns a retryable `5xx`; a `200` is reserved for a callback the
platform actually applied (including a duplicate/no-op apply). Whether the vendor actually
redelivers on `5xx` is `PROVIDER DEPENDENT` and must be confirmed at real-vendor intake, the
same discipline §6.6/LF-C1 already applies to payments — this ADR should say so explicitly for
KYC rather than leaving it implied by analogy.

### §15 KYC create/submit split — state safety, idempotency, retries, KYC-SUBMIT-OUTBOX-1

**Provider-accepted-but-our-tx-failed is safe as designed, with two precisions required.**

- `CreateVerification`: a crash between phase B (vendor accepted) and phase C leaves an orphan
  `unverified` row with `provider_reference NULL`. This has no enforcement effect — ADR 0096's
  `EvaluateEnforcement` only allows on `passed`, and `unverified`/no-row already maps to
  `failed` — so an orphan never lets a player through. Confirmed safe. **Precision required:**
  the vendor idempotency key (`"kv:" + id`) is honored only "if the vendor supports keys"
  (`PROVIDER DEPENDENT`). Where it does not, a player retry before the orphan is known creates a
  second, unrelated vendor-side verification under a second platform row. That is not a
  financial or enforcement hazard (both rows are independently evaluated on their own merits),
  but it must be recorded as `PROVIDER DEPENDENT` vendor behaviour at intake, not silently
  assumed deduplicated.
- `SubmitVerification`: a crash between phase B and phase C is identical to today's
  `ProviderError` handling — the verification stays as-is, and the next document upload
  re-submits the full set under a new content-derived key
  (`"ks:" + verification_id + ":" + sha256(sorted document ids)`). Confirmed safe, and
  idempotent on a real retry. **Precision required:** §15.3 must state explicitly that an
  ambiguous or timeout `SubmitVerification` response leaves `kyc_verifications.status`
  unchanged — it must not be misread by `normalizeProviderResult` as a definitive outcome.
  Unlike payments, §15.3 does not reuse `applyEvidence`'s matrix, so this guarantee needs its
  own stated rule and its own test, not an inherited one.
- **`KYC-SUBMIT-OUTBOX-1` deferral: accepted.** No real KYC vendor exists yet (mock only); the
  worst case under deferral (a lost submission result, corrected by the next upload) is a
  strict improvement over today's tx-held-across-call hazard (§1 row #9), since it removes the
  pool-pinning risk while adding no new financial or enforcement exposure. This specialist will
  treat "the first real KYC adapter's intake" as a hard precondition, not a suggestion: no real
  KYC adapter is accepted into PRH-I2 without a durable submission outbox design landing first.

### Payout KYC gate contract — naming the hook so ADR 0095 and ADR 0096 agree

**Confirmed: T1p is the correct, and the only correct, hook — with one addition ADR 0096 must
make explicit.**

The hook both ADRs must name identically is: **T1p — the phase-A transaction that performs
`withdrawal.ClaimForDispatch` (`approved→submitted`) together with the payout attempt's
`∅→submitting` insert (§4.3, §5.2)**. This is, by construction, the last transaction that
commits before the first outbound `Withdraw` call for that request. ADR 0096's
`DenyForCompliance` (0096 §12 C2) must run inside this same phase-A transaction, evaluated
before T1p's own CAS is attempted, sharing the identical guard predicate
(`withdrawal_requests.state = 'approved'` under the same L1 `FOR UPDATE`, §14). Because both a
KYC deny and a T1p claim require `state = 'approved'` under the same row lock, they are
mutually exclusive by the lock itself, not by application-code ordering discipline — whichever
commits first structurally forecloses the other on that request. This is exactly the "last
committing tx before the first provider Withdraw call" property 0096 C5 asks for, and it is
already present in §4.3/§5.2/§14 as written; it only needs naming, not redesigning.

**Sweeper re-check — the case ADR 0096 does not yet name.** A payout attempt can return
`submitting→created` via T5 (`NotSent`, provably not dispatched — INV-IO-9), and is later
re-claimed by the sweeper via **T2** (`created→submitting`), not by a second T1p. 0096 §12 C5
correctly requires "the sweeper's claim transaction must re-run the gate before that first
send," but 0096 does not yet say which withdrawal-side transition a T2-time KYC deny should
produce, and it should not reuse `DenyForCompliance`'s `approved→rejected` edge unmodified —
by the time T2 fires, the withdrawal is already `submitted` (T1p already ran once), not
`approved`. Since a T2 reclaim only ever occurs while the payout attempt is provably `created`
with `ever_possibly_sent = false` (T5's own guard, INV-IO-9), the platform can prove no call has
reached the provider yet — this is exactly the eligibility 0095 §4.8 already defines for **M3**
("no double-payout risk, because `created` has provably never been sent"). ADR 0096 should
route a T2-time KYC deny through **M3's existing edge** (`created→rejected`, then
`withdrawal.Fail`), not invent a parallel one. Recommend 0096 §12 C5 (or a new sub-bullet) name
this explicitly: *"a KYC deny discovered at a T2 payout reclaim is M3, never `DenyForCompliance`
applied a second time."*

**Never releasing a hold once dispatch may have occurred — confirmed structural, not
discretionary.** Once a payout attempt is `submitting`/`pending`/`ambiguous` with
`ever_possibly_sent = true`, INV-IO-7 forbids any `*→declined` transition without provider
evidence, and the trigger's `evidence_kind` enum (`callback | query_status | sync | sweeper`,
§4.2) has no member for a KYC decision. A KYC outcome is therefore structurally incapable of
driving T8 once the attempt may have reached the provider — this guarantee comes from the
state-machine trigger itself, not from an application-level check that a future code path could
bypass. No additional mechanism is required beyond the naming corrections above.

### Verdict: **APPROVE WITH CONDITIONS**

1. §15.2 must state the callback-race response class explicitly as retryable `5xx` (IC-Q1),
   with vendor redelivery-on-`5xx` flagged `PROVIDER DEPENDENT` for confirmation at real-vendor
   intake.
2. §15.3 must state explicitly that an ambiguous/timeout `SubmitVerification` result leaves
   verification status unchanged, with its own test (not inherited from §4.4's matrix, which
   §15.3 does not reuse).
3. §15/§5.2 (or a cross-reference to 0096) must name the payout KYC gate hook precisely as
   **T1p** for the initial claim, sharing the `state='approved'` L1 guard with
   `DenyForCompliance`, so the two are mutually exclusive by lock, not by convention.
4. ADR 0096 (0096 §12 C5) must be extended, before either ADR is marked implementable, to name
   the T2-payout-reclaim case explicitly and route it through **M3** (not a second
   `DenyForCompliance`), per the reasoning above — this is a required cross-ADR consistency fix,
   not a new design.
5. `KYC-SUBMIT-OUTBOX-1`'s deferral is accepted as a documented precondition on the first real
   KYC adapter intake (PRH-I2), not merely a flagged future item.

None of the above requires a redesign of §15's mechanism or of ADR 0096's enforcement boundary;
all are naming/precision fixes needed so the two documents describe the same hook, plus two
missing explicit statements in §15.2/§15.3.

---

## 26. Casino review

**Verdict: APPROVE WITH CONDITIONS**

Reviewed as casino-integration owner, scoped to §15.1 (`casino.LaunchGame`) and CAS-STMT-IO-1
only.

**§15.1 launch split.** Agreed. Phase A commits the `casino_launch_sessions` intent
(`status='active'`, `expires_at`-bounded) with no lock and no transaction held across the
vendor `Launch` call in phase B — consistent with the no-lock-across-I/O principle in §14 and
the general split pattern in §12. Phase C's CAS on `RevokeLaunchSession`
(`status='active'` guard, exists today) correctly handles both an explicit vendor failure and
an ambiguous outcome by revoking rather than assuming success, so a launch token is never
minted or handed out for a session the platform can't account for. Orphaned-session handling
is correct and matches this agent's non-negotiable: a crash on either side of the vendor call
leaves the session `active` and unresolved-to-the-player; it simply expires, and a bet against
an unknown/expired/revoked session returns `ErrLaunchSessionRequired` with no ledger effect —
no money moves without a verified bet on a resolvable session. Idempotency is sound: `SessionID`
is the deterministic external reference/key for phase A, retries after a crash mint a new
session (existing behaviour, no new state), and there is no path where a duplicate `Launch`
call or a duplicate token can be produced from the same intent. Launch-token minting itself
(single-use, opaque, bound to player/provider/game/currency/mode, short TTL, never the brand
session token) is unchanged by this ADR and is out of scope here — no new review trigger for
`security`. No objection to §15.1 as specified; no redesign needed on this agent's surface.

**CAS-STMT-IO-1.** Severity agreed: Low, not reachable today. The mock `CasinoStatementSource`
is in-memory and single-process, so the provider-I/O-inside-a-tx hazard this finding names does
not exist yet — there is no real vendor call to hold a transaction open across. Fix timing
agreed: no redesign now; the fetch → ingest → match split from §12.1 is a hard precondition
before wiring any real casino statement/reconciliation source, not before this stage's mock
work. This agent will treat "first real casino statement source" as the trigger and will not
build or accept a real statement-source adapter that reads inside the reconciliation
transaction — that adapter must land already split per §12.1, gated on `architect` and
`ledger-finance` sign-off, consistent with this agent's mock-only-without-a-commercial-
relationship constraint.

**Conditions for full APPROVE:** none blocking merge of this ADR's casino sections as written;
tracking condition only — CAS-STMT-IO-1's remediation must be verified (not just referenced) at
the time a real casino statement source is proposed, before that adapter is accepted.

---

## 27. Revision record

**Revision 2 (2026-09-27, `architect`).** Folded every review condition into the design text,
reordered the review sections numerically (§21–§26, text unchanged), applied the orchestrator's
migration renumbering, and set the status to ACCEPTED (design) — NOT IMPLEMENTED.

**Migration renumbering (orchestrator, 2026-09-27).** Payment attempts, receipts, triggers and
backfill moved from **0100 to 0101**, because 0100 is now ADR 0096's KYC enforcement migration.
0102 (kill switch) and 0103 (payment statement reconciliation) are unchanged. The body uses the
new numbers throughout (header, §0 D2, §2, §4.2, §13, §16.2 item 22, §18). The review sections
§21–§26 are quoted as written, so where they say "migration 0100" for payment attempts they mean
0101.

**Cross-ADR consistency with ADR 0096** (read at its current working-tree text):

- T1p is named as THE payout KYC hook, and `DenyForCompliance` (W-KYC) is listed as a
  pre-dispatch withdrawal transition, legal only from `approved`, in T1p's tx (§4.3, §5.2,
  §5.2.1).
- ADR 0096 §12 C5 asks for the gate to be re-run on any sweeper dispatch; this ADR does so at
  the T2 re-claim and at T12 (§5.2.1).
- IC condition 4 (ADR 0096 must name the T2-reclaim case and route it to M3) is **ADR 0096's
  side, owned by `identity-compliance`**. This ADR's side is written (§4.8 M3, §5.2.1). At the
  time of this revision ADR 0096's text does not yet name it. Re-check when its revision lands
  (LF95-C10(f)).
- The KYC function is named generically as "the ADR 0096 enforcement function exported by
  `internal/kyc` (PRH-I3)".

### 27.1 Ledger-finance (§21)

| ID | Status | Where satisfied |
|---|---|---|
| LF95-C1 | Satisfied | §8 `NotSent`/`NotProcessed` rows; §4.3 T5 and T6 (resend `NotSent` → T6) |
| LF95-C2 | Satisfied | §4.2 `last_evidence_kind`; §2 INV-IO-7; §4.3 forbidden list; §13.1 column and trigger |
| LF95-C3 | Satisfied | §4.4 preconditions 1–2; §6.1 step 4; §6.2 `anomaly` row (T10 committed) |
| LF95-C4 | Satisfied | §4.4 precondition 3; T7; §4.6 persisted `cascadable`; §6.1 step 6; §13.1 receipt columns and CHECKs |
| LF95-C5 | Satisfied | §10.1 `CallbackEchoesMerchantReference` plus registration rule; §6.4 last row; §7.1; §12.3 `pay_unresolved` |
| LF95-C6 (a)–(d) | Satisfied | (a) §5.1 "Ledger link", T7, §13.1; (b) §5.4 "Reference"; (c) T2 predicate, T13, T3 `intent_succeeded`, §20 residual; (d) T7/T13, §4.4 tombstone cells |
| LF95-C7 | Satisfied | §5.1 intent projection |
| LF95-C8 (a)–(d) | Satisfied | §4.5; §4.2 `last_sent_at`; §10.1 registration rule; §4.6 |
| LF95-C9 (a)–(e) | Satisfied | §14; §7.2; §4.3 preamble; §6.4 |
| LF95-C10 (a)–(f) | Satisfied (f: re-check pending ADR 0096 revision) | §5.2 flow; §5.2.1; §4.3 T1p, W-KYC, T2, T12; §4.8 M3; §16.1 CP-W7/W8 |
| LF95-C11 (a)–(g) | Satisfied | §13.1 backfill table and post-checks; `legacy_backfill` and trigger; §16.2 item 22 |
| LF95-C12 | Satisfied | §5.1 flow (T1+T2 in one tx); §16.1 CP-D1 restated |
| LF95-C13 | Specified; gates `IMPLEMENTED` (PRH-I5) | §12.3 ledger join; §12.1 step 3; §16.3 |
| LF95-C14 | Specified; gates `IMPLEMENTED` (PRH-I1-i) | §16.2 items 19 and 22 |
| LF95-R1 | Adopted | §12.5 `payments` re-drive job; §18 I5-c |
| LF95-R2 | Deferred, owner `bonus-engine` | §20 BONUS-T13-ELIGIBILITY-1; §5.1 |
| LF95-R3 | Noted | §9.2 |
| LF-Q1/Q2/Q3 rulings | Applied | §19.2; §4.4; §14; §12.6 |

### 27.2 Security (§22)

| ID | Status | Where satisfied |
|---|---|---|
| **S95-C1 (High, launch-blocking)** | Satisfied | §2 INV-IO-14; §4.3 T7/T8/T15; §4.4 precondition 1; §6.1 step 4; §6.4; §6.5; §7.1; §12.3; tests §16.2 item 20; mutation MX15 |
| S95-C2 (i)–(iii) | Satisfied | §6.1 step 5 cap; §6.2 cap and CHECK rows; §6.4; §7.3; §13 intro; §13.1 index |
| S95-C3 | Satisfied | §4.2 `first_submitted_at`; §6.4; §13.1 `resolution`; §20 PAY-ATTEMPT-RETENTION-1 constraint; MX18 |
| S95-C4 | Satisfied | §6.2 uniform 200 body; §16.2 item 20 |
| **S95-C5 (launch-blocking)** | Satisfied | §10.2 trigger; §13.2 guard; §16.2 item 20; MX17 |
| **S95-C6 (launch-blocking)** | Satisfied | §10.3 in-statement `NOT EXISTS`; §2 INV-IO-15; §4.3 T1+T2/T1p/T2/T12; §16.2 item 20 |
| S95-C7 | Satisfied | §10.2 `engaged_by_scope`, engage alert; §10.4; §13.2 |
| **S95-C8 (a)–(c) (launch-blocking)** | Satisfied | §3.2 steps 4 and 8; §9.1; §11 rows; §16.2 items 15 and 20 |
| S95-C9 (i)–(iii) | Satisfied | §3.2 step 3; §7.2 step 6; §11 "Observability" |
| S95-C10 | Satisfied | §9.3; §13.1 CHECKs; §20 VENDOR-INTAKE-REF-PII-1; §16.2 item 20 |
| S95-C11 | Satisfied | §9.5 `Fetch(CallContext)`; §12.1 step 1; §13.3 CHECKs; §16.3 |
| S95-C12 (i)–(iii) | Satisfied | §10.1 registration refusals and `WebhookRetrySemantics`; §8 `NotProcessed` |
| **S95-C13 (launch-blocking)** | Satisfied | §10.5 routes; §4.8 M3 CAS; §13.1 trigger; §16.2 item 20 |
| S-Q1 / S-Q2 / S-Q3 rulings | Applied | §10.4; §6.2; §6.6 option (a), §15.1, §15.2 |
| §22.5 tests 1–10 | Specified | §16.2 item 20 |

### 27.3 Payments (§23)

| ID | Status | Where satisfied |
|---|---|---|
| P95-C1 | Specified | §5.1 "Provider reference"; §16.2 item 19 |
| P95-C2 | Specified (due by I1-f) | §4.7; §18 I1-f and specialist documents |
| P95-C3 | Satisfied | §10.1 `WebhookRetrySemantics` (mandatory, fail-closed); §6.6 |
| P95-C4 | Informational; consistent with the LF-Q1 ruling | §4.4 |

### 27.4 QA (§24)

| Change | Status | Where satisfied |
|---|---|---|
| 1 Mutation per invariant | Specified | §16.4 MX10–MX18 |
| 2 Migration up/down and backfill tests | Specified | §16.2 item 22 |
| 3 RLS cross-tenant tests, new tables | Specified | §16.2 item 22 |
| 4 API/OpenAPI conformance | Specified | §10.5; §16.2 item 20 |
| 5 Deterministic connection-loss hook | Specified | §16.1 preamble (`pgfault` proxy) |
| 6 Breaker state-transition test | Specified | §16.2 item 21 |
| 7 CI lanes, budgets and placement | Specified (`RECOMMENDATION` numbers; `qa` confirms in I1-i) | §16.5 |
| 8 Repeat-run bounds | Specified | §16.2 items 1, 2 and 11 (≥ 100 with `-race`) |
| Intentional sleep called out | Done | §16.2 item 14 |

### 27.5 Identity-compliance (§25)

| Condition | Status | Where satisfied |
|---|---|---|
| 1 KYC race → retryable 5xx; redelivery PROVIDER DEPENDENT | Satisfied | §15.2 "Callbacks"; §16.2 item 18 |
| 2 Ambiguous `SubmitVerification` leaves status unchanged, own test | Satisfied | §15.3; §16.2 item 18 |
| 3 T1p named as the payout KYC hook, shared L1 guard | Satisfied | §4.3 T1p and W-KYC; §5.2; §5.2.1 |
| 4 T2-reclaim KYC deny → M3 | This ADR's side satisfied (§4.8 M3, §5.2.1); **ADR 0096's side owned by `identity-compliance`**, pending in its revision | §18 specialist documents |
| 5 KYC-SUBMIT-OUTBOX-1 as a hard precondition | Satisfied | §15.3; §18; §20 |
| CreateVerification vendor idempotency PROVIDER DEPENDENT | Satisfied | §15.2 |

### 27.6 Casino (§26)

| Condition | Status | Where satisfied |
|---|---|---|
| CAS-STMT-IO-1 remediation verified (not referenced) when a real casino statement source is proposed | Tracking condition, owner `casino` (+ `architect`, `ledger-finance`) | §15.1 "Reconciliation"; §20 |

### 27.7 Not satisfiable in this ADR

These are escalated or deferred, and none is silently dropped:

| Item | Status |
|---|---|
| HD-0095-1: M1/M2 manual resolution authority and thresholds | Human decision, not taken; M1/M2 stay BLOCKED (§19.1) |
| LEDGER-MANUAL-ADJ-4EYES-1 (compensation mechanism) | Registry item, not built; remediation of `disputed` and of several `pay_*` kinds is BLOCKED on it |
| ADR 0096 naming of the T2-reclaim → M3 route | Owner `identity-compliance` (§27.5) |
| Re-verification of LF95-C10 against ADR 0096's landed revision | Owner `ledger-finance` (LF95-C10(f)) |

