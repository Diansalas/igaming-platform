# ADR 0095 — Provider-I/O Transaction Boundary and Provider-Neutral Payment Contract

- **Status:** PROPOSED (design only, 2026-09-27). Nothing in this ADR is implemented. Every
  deliverable below is `NOT IMPLEMENTED` until an implementing change lands and is reviewed.
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
  - **ADR 0082 §2.1.** Proposed Amendment A7, which `ledger-finance` owns and must accept (§14).
    It adds two L1 tables and places the callback-receipt insert before L1.
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
    Migration 0100 must not merge before 0099.
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
| D2 | **One state machine for every outbound money operation.** Deposits and payouts both run through `payment_attempts` rows (migration 0100). There are 8 states, 4 of them non-terminal, and every transition is CAS-guarded, audited and backstopped by a DB trigger (§4). |
| D3 | **The attempt row is the outbox.** A cascade, a retry or a recovery is a committed `payment_attempts` row with `next_action_at`. A bounded worker (the sweeper) drives these rows with leases and `FOR UPDATE SKIP LOCKED` claims (§7). No provider I/O happens inside a webhook transaction. |
| D4 | **Deterministic external identity.** The merchant reference and the external idempotency key are both derived from the committed attempt id. They are never generated per try (§5). |
| D5 | **Evidence decides; the absence of evidence never does.** Success is applied only on verified, amount- and asset-matching evidence. Timeout, not-found and ambiguous results are never treated as failure for a payout, and never as success for anything. A deposit may be declined on "not found" only when the adapter declares that lookup authoritative (§4.5). |
| D6 | **Callbacks resolve by merchant reference and by provider reference.** They are accepted in any non-terminal state and durably receipted (`payment_provider_events`). A callback that cannot be resolved yet is deferred, then applied when the attempt learns its reference (§6). |
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
| **INV-IO-4** | Every state change of an attempt is one CAS `UPDATE … WHERE id = $1 AND state = ANY($allowed_from)`, in the same tx as its audit record and its ledger effect (if any). A DB trigger rejects any (OLD, NEW) pair not in §4.3 and any change to an immutable column. | CAS in code; `payment_attempts_guard` trigger (0100). |
| **INV-IO-5** | Ledger posting never spans external I/O. Each posting runs in one short domain tx holding the ADR 0082 locks in class order, with the authoritative balance read in that same tx. | The D1 pattern; ADR 0082 A7 (§14); existing `ledger.Post`. |
| **INV-IO-6** | No success without evidence. `succeeded` is entered only by a verified callback, a `QueryStatus` result or a synchronous result reporting a definite success **whose amount and asset equal the attempt's**. A mismatch goes to `disputed`, never to `succeeded`. | `applyEvidence` (§4.4) is the only function that writes `succeeded`; trigger. |
| **INV-IO-7** | No payout failure without definite decline evidence. `withdrawal.Fail` (the hold release) is reachable from a dispatched payout only through `declined` evidence from the provider. Timeout, not-found, ambiguous and operator impatience are never enough. | §4.5 asymmetry; trigger forbids `ambiguous→declined` without `evidence_kind ∈ {callback, query_status, sync}`. |
| **INV-IO-8** | At most one live (non-terminal) attempt per deposit intent, and exactly one payout attempt per withdrawal request. | Partial unique indexes (0100). |
| **INV-IO-9** | An attempt in `created` has never had a call that may have reached the provider (`ever_possibly_sent = false`). | `CHECK (state <> 'created' OR NOT ever_possibly_sent)`; `submitting→created` only on `ErrorClassNotSent`. |
| **INV-IO-10** | A verified callback is never lost. It is durably receipted in the same tx as its effect (or its deferral). If that tx fails, the response is retryable (5xx), so the provider redelivers. | `payment_provider_events` (0100); §6. |
| **INV-IO-11** | Provider credentials are resolved outside any tx, per call, and never while a financial lock is held. They are never stored in an adapter, client or cache, apart from `DerivedTokenCache`. | §11; `OutboundResolver.Resolve` `txscope` refusal (exists); adapter-field reflection test (§16). |
| **INV-IO-12** | Reconciliation never writes the ledger, a projection, an attempt, an intent or a withdrawal. It writes only run, mismatch and statement-import rows. | Stream code; statement-capture test (§16). |
| **INV-IO-13** | After every failure-injection test: `SUM(debits) == SUM(credits)` and projection == rebuild (LF-C2 #8). | Shared test helper, reused from the existing suites. |

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
3. Resolves the credential (§11) and builds `CallContext`.
4. Applies the per-call deadline: `min(ctx deadline, resolve budget + manifest call timeout)`.
5. Calls the adapter.
6. Maps panics and unknown errors to `ErrorClassAmbiguous` for money-moving operations, and to
   `ErrorClassNotSent` only if the adapter provably did not dispatch.
7. Feeds the orchestrator-owned breaker (§9.6).

Casino and KYC use the same gate shape (steps 1, 3, 4 and 5) through a small shared helper.
They do not import payments.

---

## 4. Payment attempt state machine (deposits and payouts)

### 4.1 States

| State | Meaning | Terminal? | Provider may hold it? |
|---|---|---|---|
| `created` | Durable intent to submit. No call that may have reached the provider has ever been made (INV-IO-9). `provider_id` may be NULL (not yet routed; cascade rows). | no | **no** |
| `submitting` | Claimed for a provider call. The call may be in flight or done. The provider **may** have received it. | no | yes |
| `pending` | The provider acknowledged receipt and assigned a `provider_reference`; the outcome is not final. **"Accepted" ≡ `pending` with `accepted_at` set.** Provider-neutrally, the platform cannot distinguish "accepted" from "pending", so they are one state. | no | yes |
| `ambiguous` | Outcome unknown: a timeout or transport failure after a possible send, a non-definitive `QueryStatus`, or a lookup that is not authoritative. **Never a failure, never a success.** | no | yes |
| `succeeded` | Definite success on verified, amount- and asset-matching evidence. The ledger effect is posted in the same tx (deposit: Flow 1; payout: Flow 3 Step B via `withdrawal.Complete`). | yes | yes |
| `declined` | Definite negative from the provider (sync decline, verified decline callback, `QueryStatus` declined, or an authoritative not-found for a deposit). This covers the human's "**failed**" (`decline_stage = after_acceptance`) and a provider-side "rejected at submission" (`decline_stage = at_submission`). Terminal for automation. Only verified success evidence can override it (§4.4, anomaly). | yes* | yes |
| `rejected` | Platform-side refusal **before any call that may have reached the provider**. Reached only from `created`: kill switch, unsupported capability, no routable provider, credential unavailable after retries are exhausted, or the interactive presence window expiring. | yes | **no** |
| `disputed` | Contradictory or mismatched evidence that automation must not resolve. Examples: a success with a different amount or asset; success evidence for a `rejected` or `created` attempt; a success for a declined payout; a provider-reference conflict. It is a P1. Exit is manual only (§4.8). | yes | yes |

\* `declined → succeeded` exists only for deposits, only on verified matching success
evidence, and always raises P1 `contradictory_provider_outcome` (§4.4 row 7).

Mapping of the human's vocabulary:

| Term | Where it lives |
|---|---|
| request | `created` (plus the intent row) |
| provider submission | `created→submitting` |
| accepted | `pending` (`accepted_at`) |
| pending | `pending` |
| succeeded | `succeeded` |
| failed | `declined` (`after_acceptance`); withdrawal `failed` |
| rejected | `rejected` |
| timeout | call timeout → `ambiguous` (T6); settlement timeout → escalation, **no state change** (T16) |
| ambiguous provider result | `ambiguous` |
| retry | T5 (NotSent), T12 (idempotent resubmit), player retry resume (§5.1) |
| duplicate callback | receipt dedupe plus no-op evidence (§6.3) |
| callback after timeout | `ambiguous→*` (T9–T11); `declined→succeeded` anomaly (T13) |
| reconciliation | §12 (detection) plus T17 re-verify |
| manual intervention | M1–M3 (§4.8) |

### 4.2 Columns that make the machine deterministic

- `ever_possibly_sent`: set to true, permanently, the first time a call's classification is
  anything other than `NotSent`.
- `submit_count`: bounded by the manifest's `max_resubmits`.
- `claim_token` and `lease_until`: who may call, and until when.
- `next_action_at`: when the sweeper should look. NULL when terminal.
- `evidence_kind` on every transition's audit record: `sync | callback | query_status | sweeper
  | operator | platform`.
- `interactive`: from the manifest. True means a player must be present to use the result, for
  example a redirect URL.

### 4.3 Transition table (the complete set; anything else is rejected by code and by trigger)

The "Allowed from" column is exactly the CAS predicate.

| T | From → To | Trigger / evidence | Performed by | CAS guard (in addition to `id = $1`) | Ledger / domain effect (same tx) | Audit action |
|---|---|---|---|---|---|---|
| T1 | ∅ → `created` | `InitiateDeposit` phase A (attempt 1), or cascade (attempt n+1, §4.6) | Player-request driver; the callback or sweeper tx that commits the decline | INSERT; the partial unique index enforces one live attempt per intent | Intent inserted (T1 on attempt 1 only) | `deposit.requested`, `payment.attempt_created` |
| T1p | ∅ → `submitting` | Payout claim: withdrawal `approved→submitted` in the **same** tx | Eligible staff (submit endpoint) | Withdrawal CAS `state='approved'` under L1 `FOR UPDATE`; `UNIQUE(withdrawal_request_id)` | Withdrawal → `submitted` (`provider_id` set, `provider_reference` NULL) | `withdrawal.dispatch_claimed`, `withdrawal.submit.http` (staff) |
| T2 | `created` → `submitting` | Claim for the call. The kill switch and the tenant capability row are re-read **in this tx** (§10.3). | Player-request driver or sweeper | `state='created' AND (provider_id IS NULL OR provider_id=$p)` plus `NOT kill_switch_engaged(...)`; sets `provider_id`, `claim_token`, `lease_until`, `submit_count+1`, `submitted_at` | none | `payment.attempt_claimed` |
| T3 | `created` → `rejected` | Pre-call refusal (kill switch, unsupported, no route, credential exhausted, presence window expired) | Driver or sweeper | `state='created'` | Deposit: intent → `declined` if there is no other live attempt. Payout: `created→rejected` only via M3 (§4.8). | `payment.attempt_rejected` (+reason) |
| T4 | `submitting` → `pending` | Sync `Pending` with a reference | Phase C | `state='submitting'` | Sets `provider_reference`, `accepted_at`; applies deferred receipts (§6.4); withdrawal `provider_reference` set | `payment.attempt_accepted` |
| T5 | `submitting` → `created` | `ErrorClassNotSent` (provably not dispatched: credential unavailable, gate refusal, connection refused before write) | Phase C (same claimant) | `state='submitting' AND claim_token=$t AND NOT ever_possibly_sent` | `next_action_at` = backoff | `payment.attempt_not_sent` |
| T6 | `submitting` → `ambiguous` | Sync `Ambiguous`; `ErrorClassAmbiguous` (timeout after a possible send, reset after write, unmapped 5xx); lease expired and `QueryStatus` not definitive | Phase C or sweeper | `state='submitting'`; sets `ever_possibly_sent=true` | `next_action_at` = poll backoff | `payment.attempt_ambiguous` (+cause `timeout`/…) |
| T7 | `submitting`/`pending`/`ambiguous` → `succeeded` | Verified success evidence with amount = attempt amount and asset = attempt asset (callback, `QueryStatus`, or sync where the manifest allows sync success) | Phase C, callback tx or sweeper | `state = ANY('{submitting,pending,ambiguous}')` | **Deposit:** `ledger.Post` Flow 1 (idempotent on `(tenant, provider_id, provider_tx_id)`); intent → `succeeded`. **Payout:** `withdrawal.Complete` (Flow 3 Step B). | `deposit.posted` / `withdrawal.completed`, `payment.attempt_succeeded` |
| T8 | `submitting`/`pending`/`ambiguous` → `declined` | Definite decline evidence (sync `Declined`, verified decline callback, `QueryStatus` declined, or a deposit authoritative not-found, §4.5) | Phase C, callback tx or sweeper | `state = ANY('{submitting,pending,ambiguous}')`; the trigger requires `evidence_kind ≠ operator` | **Deposit:** cascade row (T1) if eligible (§4.6), else intent → `declined`. **Payout:** `withdrawal.Fail` (hold released). | `payment.attempt_declined`, `withdrawal.failed` |
| T9 | `ambiguous` → `pending` | Evidence that the provider holds it and has not finished | Callback or sweeper | `state='ambiguous'` | Sets `provider_reference` if it was unknown | `payment.attempt_accepted` |
| T10 | `submitting`/`pending`/`ambiguous` → `disputed` | Mismatched success (amount, asset or reference) | Any evidence path | `state = ANY('{submitting,pending,ambiguous}')` | none; P1 | `payment.attempt_disputed` |
| T11 | `pending` → `ambiguous` | Explicit "unknown" or not-found evidence for an accepted attempt (the provider forgot it) | Sweeper | `state='pending'` | none; P1 anomaly | `payment.attempt_ambiguous` |
| T12 | `ambiguous` → `submitting` | Idempotent resubmission of the **same** attempt with the **same** key. Only if the manifest has `idempotent_submission=true` and `submit_count < max_resubmits` | Sweeper | `state='ambiguous' AND submit_count < $max`; new `claim_token`, lease | none | `payment.attempt_resubmitted` |
| T13 | `declined` → `succeeded` | **Deposit only.** Verified matching success after a decline | Callback or sweeper | `state='declined' AND operation='deposit'` | Flow 1 posted (the money is real); intent → `succeeded`; P1 `contradictory_provider_outcome`. If another attempt of the intent is already `succeeded`, also P1 `multiple_success_for_intent` (§4.4 note). | `payment.attempt_succeeded_after_decline` |
| T14 | `declined` → `disputed` | **Payout only.** Success evidence after `withdrawal.Fail` released the hold (a double payout has already happened) | Callback or sweeper | `state='declined' AND operation='payout'` | none; P1 | `payment.attempt_disputed` |
| T15 | `created`/`rejected` → `disputed` | Any success evidence for an attempt the platform never sent (adapter misclassification or a hostile verified sender) | Callback | `state = ANY('{created,rejected}')` | **Nothing posted**; P1 | `payment.attempt_disputed` |
| T16 | (no state change) escalation | `pending`/`ambiguous`/`submitting` older than the manifest `resolution_horizon` | Sweeper | `escalated_at IS NULL` | Sets `escalated_at`; poll cadence drops to the escalated rate; alert | `payment.attempt_escalated` |
| T17 | (no state change) re-verify | Operator asks for fresh evidence on any attempt, for example after a reconciliation `pay_status_mismatch` | Staff with `payments_attempt:reverify` | none (sets `next_action_at=now()` only) | Evidence is applied through the matrix like any sweeper poll | `payment.attempt_reverify_requested` |
| M1–M3 | manual | §4.8 | §4.8 | §4.8 | §4.8 | §4.8 |

**Forbidden, and enforced by trigger:**

- any move into `created`, except T5;
- `rejected → *`, except T15;
- `succeeded → *` (a reversal is a separate ledger fact, §5.4);
- `disputed → *`, except M1/M2;
- `ambiguous → declined` with `evidence_kind='operator'`;
- any `*→declined` for a payout without provider evidence;
- any backward move to `submitting` other than T12.

### 4.4 `applyEvidence` — the single evidence matrix

One function, called by phase C, the callback path, the sweeper and T17. Rows are the current
state; columns are the evidence outcome. Every cell is also audited.

| Current \ Evidence | `pending` | `succeeded` (match) | `succeeded` (mismatch) | `declined` | `ambiguous` | `not_found` |
|---|---|---|---|---|---|---|
| `created` | anomaly log, no change | T15 → `disputed` | T15 | anomaly log | no-op | no-op |
| `submitting` | T4 | T7 | T10 | T8 | T6 | §4.5 |
| `pending` | no-op (reschedule) | T7 | T10 | T8 | no-op | T11 |
| `ambiguous` | T9 | T7 | T10 | T8 | no-op (reschedule) | §4.5 |
| `succeeded` | no-op | no-op (duplicate; the ledger is idempotent) | P1 anomaly, receipt `anomaly`, no change | P1 anomaly (a reversal needs a reversal event), no change | no-op | P1 anomaly |
| `declined` | no-op | deposit T13 / payout T14 | P1 anomaly | no-op | no-op | no-op |
| `rejected` | anomaly log | T15 | T15 | no-op | no-op | no-op |
| `disputed` | recorded only | recorded only | recorded only | recorded only | recorded only | recorded only |

- Evidence that carries a `provider_reference` different from a non-NULL
  `attempt.provider_reference` is a *reference mismatch*. It is T10 from a non-terminal state,
  otherwise a P1 anomaly.
- **One exception:** a payout **settlement reference** (`CallbackEvent.SettlementReference`)
  may differ by design. It becomes the ledger `provider_tx_id` of Step B, exactly as
  `withdrawal.Complete` allows today.
- **Note for ledger-finance sign-off (LF-Q1).** T13 posts a second Flow 1 when a PSP reports
  success after a definite decline and the cascade already succeeded elsewhere. That credits
  the player twice for two real captures, and the refund of one is a business process
  (reversal or manual adjustment).
  - The alternative is a suspense or unallocated account instead of `player_cash`. That needs
    a new account type, so it is `ledger-finance`'s call.
  - The ADR's default is to post and raise a P1, because the ledger must reflect received
    funds.

### 4.5 Deposit/payout asymmetry (binding)

- **Deposit failure is money-safe to assume; a late success still posts.** On `not_found`:
  - The deposit moves to `declined` (`decline_stage=at_submission`, `reason=not_received`) only
    if the manifest declares `merchant_lookup_authoritative_after = Δ` and `now − submitted_at
    > Δ`.
  - Otherwise, or when there is no merchant-reference lookup at all, it moves to `ambiguous`,
    and T12 applies if the manifest allows it.
- **Payout failure is never assumed.** Releasing a hold for a payout that later lands is a
  double payout.
  - `not_found`, timeout and ambiguous results always go to `ambiguous`.
  - The only automated exits are T7 (success evidence), T8 (definite decline evidence) and T12
    (idempotent resubmission, whose result is itself evidence).
  - Everything else is escalation (T16), then M2 (four-eyes, currently BLOCKED).

### 4.6 Cascade (deposits only) — via the attempt row, never inline in a webhook

A decline is **cascade-eligible** when all of these hold:

- `cascadable = true` on the evidence;
- `attempt_no < MaxCascadeDepth`;
- the intent is not `succeeded`;
- no other live attempt exists;
- the kill switch is not engaged for the next candidate (re-checked at T2);
- and either (a) the decline arrived **synchronously on the player-request path**, or (b) the
  attempt is `interactive = false`.

When eligible, the tx that commits T8 also inserts attempt n+1 (T1, `provider_id NULL`,
`excluded_provider_ids = all previous attempts' providers`). This closes the
`providerExclusionSoFar` limitation. Case (a) is driven immediately by the same request's
driver. Case (b) is driven by the sweeper.

An **asynchronous** decline of an **interactive** attempt finalizes the intent as `declined`.
The redirect URL of a new provider could never reach the player, which is the latent defect of
today's webhook cascade. The player retries with a new idempotency key.

This amends payment-orchestration.md §5 (recorded above). A cascade **never** follows
`ambiguous`, `not_found` or a timeout (LF-C2 #5).

### 4.7 Withdrawal state mapping

| Withdrawal state (unchanged set) | Payout attempt state | Notes |
|---|---|---|
| `approved` | none | — |
| `submitted` | `submitting` / `created` (NotSent retry) / `pending` / `ambiguous` / `disputed` | New meaning: "dispatch claimed; the provider may hold it". `provider_reference` is NULL until T4/T9/T7. The resolve handler's "structurally unreachable" check (`withdrawal_handlers.go:1034`) becomes a normal case (query by merchant reference). |
| `completed` | `succeeded` | Via T7 → `withdrawal.Complete` in the same tx |
| `failed` | `declined`, or `rejected` via M3 | Via T8 → `withdrawal.Fail`, or M3 |

`withdrawal.MarkSubmitted` is split into:

- `ClaimForDispatch(tx, id, providerID)`: `approved→submitted`, no reference;
- `RecordProviderReference(tx, id, ref)`: sets it once, NULL→value.

`Reject`/`Cancel` from `approved` race the claim on the same L1 row, and the CAS guarantees
exactly one wins. **No payout cascade** (current behaviour, kept): a declined payout fails the
withdrawal.

### 4.8 Manual intervention (only where genuinely necessary)

| M | What | When it is the only option | Governance | Status |
|---|---|---|---|---|
| M1 | Resolve a `disputed` **deposit** attempt: attach a compensating ledger transaction, or record "no ledger action", with evidence | Contradictory or mismatched evidence | Via LEDGER-MANUAL-ADJ-4EYES-1 (four-eyes above a threshold) | **BLOCKED** on LEDGER-MANUAL-ADJ-4EYES-1. Until then the attempt stays `disputed`, is visible, and is a P1. |
| M2 | Force-resolve an `ambiguous`/`disputed` **payout** (declare paid → `Complete`, or declare not-paid → `Fail`) without provider evidence | The provider cannot be queried, has no idempotent resubmission, is past the horizon, and reconciliation has not resolved it | Four-eyes; threshold per **HD-0095-1** | **BLOCKED** on HD-0095-1 and LEDGER-MANUAL-ADJ-4EYES-1. No API is built. |
| M3 | Abandon a payout attempt in `created` (never sent, INV-IO-9) → `rejected`, then `withdrawal.Fail` | The credential or provider is permanently gone | The same eligibility as today's resolve handler (linked, active staff) plus a reason code. **No double-payout risk**, because `created` has provably never been sent. | Designed here; built in PRH-I1. |

The operator re-verify (T17) and kill switch (§10) are not "manual resolution". They only
fetch evidence or stop new activity.

---

## 5. Per-operation specification

All operations share these properties. Idempotency is DB-enforced. Audit goes into
`audit_log` in the same tx as the effect. Nothing is authoritative in Redis.

### 5.1 Deposit (priority; fully specified)

| Aspect | Specification |
|---|---|
| Intent | `deposit_intents` row (unchanged table, status set unchanged). Its status is a projection of its attempts, updated in the same tx as every attempt transition: any `succeeded` → `succeeded` (sticky); a live attempt that is `ambiguous` → `ambiguous`; any other live attempt → `pending`; none live → `declined`. `failed` stays unused. RG denial keeps today's behaviour: intent → `declined` with `rg_ineligible:*` and no attempt, in phase A. |
| Provider reference | `payment_attempts.provider_reference`: set once by T4, T7 or T9, bounded by `PROVIDER_REF_MAX` (0099), and unique per `(tenant, provider_id)`. `deposit_intents.provider_id/provider_reference` keep mirroring the latest routed attempt for existing readers. |
| Idempotency keys | **Player:** `UNIQUE(tenant, player, idempotency_key)` on the intent (exists). A retry **resumes**: it returns the intent; if its live attempt is `created`, the retry drives it (T2 CAS, so concurrent retries yield exactly one claimant); if `submitting`/`pending`/`ambiguous`, it returns the status (the redirect URL is not persisted; it is re-obtained only by T12 when `idempotent_submission`). **External:** `external_idempotency_key = "pa:" + attempt.id`, `merchant_reference = attempt.id` (INV-IO-3). **Ledger:** `(tenant, provider_id, provider_tx_id = provider_reference)` plus `idempotency_key = provider_id:provider_reference` (exists). |
| Flow (player request) | Phase A0 (read-only tx): load candidates → health filter **outside** the tx (§9.6) → pick a provider. Phase A (tx): insert intent plus attempt 1 (T1), RG check, T2 claim (kill switch in-tx) → commit. Phase B: resolve credential, `Deposit(CallContext, req)`. Phase C: `applyEvidence` (T4/T6/T8/T7), with a synchronous cascade loop (§4.6), each step its own A/B/C. The handler no longer wraps the call in `WithTenant`; `InitiateDeposit` takes `*db.Pool` (INV-IO-1a). |
| State / retryability | §4.3. `NotSent` → T5 (the driver retries within its request budget, else the sweeper takes over if non-interactive; for an interactive attempt the player sees "temporarily unavailable" and the attempt expires via T3). `Ambiguous` → T6, never cascaded, T12 only if the manifest allows it. |
| Callback | §6. Resolution by `(provider_id, provider_reference)` **or** `merchant_reference`. Accepted in `submitting`. |
| Reconciliation | §12: `pay_*` kinds keyed by `(provider_id, provider_reference)` with a merchant-reference fallback. |
| Failure recovery | Crash points CP-D1..CP-D8 (§16.1), each converged by CAS phase C, callbacks or the sweeper. |
| Audit | `deposit.requested`, `payment.attempt_*` (every transition, with `evidence_kind`, provider id, reference, `error_class`, credential **fingerprint and handle id only**), `deposit.posted`, `deposit.declined`, `payments.deposit_denied_by_rg` (exists). |

### 5.2 Withdrawal payout dispatch (LF-C2 #9)

| Aspect | Specification |
|---|---|
| Intent | `withdrawal_requests` (existing four-eyes approval flow unchanged) plus exactly one payout attempt (INV-IO-8). |
| Provider reference | The attempt's `provider_reference` (the instruction reference), copied once into `withdrawal_requests.provider_reference`. A settlement or confirmation reference may differ (Flow 3) and is the Step B `provider_tx_id`. |
| Idempotency keys | Staff double-submit: the L1 `FOR UPDATE` plus the CAS `approved→submitted` plus `UNIQUE(withdrawal_request_id)` on attempts, so the second caller gets `ErrStateConflict` **before any call**. External: `"pa:" + attempt.id`. Ledger: Step B `provider_id:provider_tx_id` (exists); Fail `request_id:failed` (exists). |
| Flow | Phase A0 (read-only tx, no lock): load the request plus candidates → health outside the tx → route. Phase A (tx): approver eligibility (exists) → L1 lock → T1p (withdrawal `approved→submitted`, attempt `submitting`, kill switch plus capability re-read in-tx; an engaged switch aborts the claim with 503 and the withdrawal stays `approved`) → commit. Phase B: resolve credential (NotSent on failure → T5, and the sweeper retries unattended because payouts are non-interactive), `Withdraw`. Phase C: L1 lock (withdrawal) → attempt → `applyEvidence` (T4/T6/T7→`Complete`/T8→`Fail`). |
| Retryability | NotSent → T5, then the sweeper re-claims (T2) while the kill switch is released. Ambiguous → never resubmitted except T12. Never re-routed. |
| Callback | New canonical event `payout` (§9.3), handled by the same receipt/resolution path. `payout_returned` (after completion) is declared **unsupported** (NOT IMPLEMENTED): receipt `unsupported_event`, P1, verified-malformed 4xx class. |
| Reconciliation | §12, line kind `payout`. |
| Failure recovery | CP-W1..CP-W6 (§16.1). The existing staff resolve endpoint becomes: read (no lock) → `QueryStatus` outside the tx → phase C. It is T17-equivalent evidence, never a resubmission. |
| Audit | `withdrawal.dispatch_claimed`, `withdrawal.submit.http` (staff, IP, UA; exists), `payment.attempt_*`, `withdrawal.completed`/`withdrawal.failed` (exist), `withdrawal.resolve_attempted.http` (exists). |

### 5.3 Status query (`QueryStatus`)

| Aspect | Specification |
|---|---|
| Intent | No row of its own. It always concerns an existing attempt. |
| Reference | `StatusQuery{ProviderReference, MerchantReference}`. The adapter uses the provider reference when known, and the merchant reference only if the manifest has `status_by_merchant_reference`. |
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
| Reference | The reversal's own `provider_reference` plus `original_provider_reference`. The original is resolved through `payment_attempts` (then the intent's `ledger_transaction_id`). |
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

This all happens in one domain tx (`WithTenant`), with **no provider I/O**:

1. **Redeem** (exists).
2. **`HandleCallback`** (exists).
3. **PROVIDER-REF-BOUND-1 boundary validation** (0099; a deterministic non-retryable
   rejection).
4. **Receipt insert.** `INSERT … ON CONFLICT (tenant_id, provider_id, event_fingerprint) DO
   NOTHING RETURNING id`. On conflict the event is a **duplicate delivery**: evidence is still
   applied (idempotently), but no new receipt row is written.
5. **Resolve the attempt:**
   - (a) by `(provider_id, provider_reference)`;
   - (b) otherwise by `(tenant, merchant_reference)` if the event carries one;
   - (c) otherwise the event is **unresolved**.
6. **Lock** in ADR 0082 order (§14), then `applyEvidence`.
7. **Set the receipt's one-shot `attempt_id`/`applied_at`** (`deferred` if unresolved).
8. Commit, then return 200.

### 6.2 Dispositions and HTTP mapping

| Disposition | Condition | Response |
|---|---|---|
| `applied` | The evidence changed state or posted | 200 |
| `duplicate_effect` | The receipt already existed, or the evidence was a no-op for the current state | 200 (unchanged replay semantics) |
| `deferred_unresolved` | Verified, but no attempt is resolvable yet (the callback raced phase C before the reference was known, and there is no merchant-reference echo) | **200 after durable receipt** (INV-IO-10). This changes today's 404 for this case only. |
| `anomaly` | A P1 cell of §4.4 | 200 after durable receipt plus alert. The existing typed rejections (`ErrCallbackPayloadMismatch` 409, `ErrDepositAlreadyReversed` 409, `ErrCallbackProviderMismatch`) **keep their current HTTP mapping and their separate-tx evidence records** (`RecordDepositReversalRejection` pattern). This ADR does not change reviewed response codes. |
| `unsupported_event` | A verified event type the manifest does not support | Existing verified-malformed 4xx class; receipt written in the separate-tx pattern |
| (none) | DB error or a rollback of any kind | 5xx, which is retryable; nothing is committed; the provider redelivers (LF-C1, §6.6) |

### 6.3 Duplicate and concurrent callbacks

Two concurrent deliveries of one event serialize on the receipt-key insert first. The second
waits, then conflicts, then applies idempotently. They then serialize on the L1 attempt lock
and the ledger unique constraint. The result is exactly one posting (INV-IO-13 test).

Two *different* events for one attempt (for example, a decline and then a success) serialize on
the L1 lock, and the matrix decides.

### 6.4 Callback before or after the internal transition

| Race | Outcome |
|---|---|
| Callback arrives while the attempt is `submitting` (phase C not yet committed) | With a merchant-reference echo, it resolves by merchant reference and T7/T8 applies. Phase C later finds `succeeded`/`declined`, and its CAS from `submitting` fails. The late sync result is then passed to `applyEvidence` against the new state and is a no-op or anomaly (reference cross-checked). |
| The same race without a merchant-reference echo | `deferred_unresolved`. Phase C's T4/T9 (which sets `provider_reference`), **in the same tx**, selects deferred receipts for `(provider_id, provider_reference)` and applies them in `received_at` order. The sweeper also re-applies deferred receipts whose reference has since appeared (bounded, §7), as a backstop. Both orders converge to the same final state. |
| Callback after timeout (the attempt is `ambiguous`) | T7/T8/T9 per the matrix. This is the normal resolution path. |
| Callback after the sweeper declined a deposit on an authoritative not-found | T13 (post plus P1). |
| Callback for an attempt the platform never sent | T15 (no post, P1). |

### 6.5 What a callback may never do

- Call a provider: no cascade I/O, no `QueryStatus`.
- Hold a transaction longer than its own DB work.
- Trust a payload tenant.
- Create an attempt, except the cascade row (T1) committed with a decline.
- Post a ledger entry for anything other than matching verified success or reversal evidence.

`handleDecline → attemptDeposit` and `resolveAmbiguous` are removed from the callback path. An
ambiguous-outcome callback sets `next_action_at = now()` and nudges the sweeper.

### 6.6 LF-C1 (401 and 5xx redelivery), carried into the manifest

Every adapter manifest declares `redelivery_on_401` and `redelivery_on_5xx`, each one of
`redelivers | terminal | unknown`, as recorded from vendor intake #28.

At startup, a **production-eligible** adapter with `terminal` or `unknown` for 401 is refused
unless a production-eligible `PaymentStatementSource` is registered for the same provider id.
That is LF-C1 option (b), enforced structurally: the §12 stream reports "provider-settled,
platform-unposted" as `pay_status_mismatch`/`pay_missing_platform_record`, which are P1.

Option (a), a retryable 5xx for a post-verification re-check DB error, remains a `security`
decision and is not taken here.

---

## 7. Outbox and sweeper (bounded work)

### 7.1 Work set

These are `payment_attempts` rows with `next_action_at <= now()`:

| State | Sweeper action |
|---|---|
| `created`, `interactive = false`, or any payout | route if `provider_id IS NULL` (A0) → resolve credential → T2 → call → C |
| `created`, `interactive = true`, older than `presence_window` | T3 (`expired_before_submission`) |
| `submitting`, `lease_until < now()` | `QueryStatus` (§5.3) → matrix; `not_found` → §4.5 |
| `pending` | `QueryStatus` → matrix; still pending → backoff; past the horizon → T16 |
| `ambiguous` | `QueryStatus` → matrix; or T12 if the manifest allows it; else backoff/T16 |

Deferred receipts whose `(provider_id, provider_reference)` now resolves are also re-applied,
bounded by the same per-tenant batch.

### 7.2 Claim protocol

1. **Tenant iteration.** Active tenants in round-robin order, as the reconciliation scheduler
   does (`activeTenantIDs`). Each tenant is handled in its own `WithTenant` tx.
2. **Claim tx (short).** `SELECT … FROM payment_attempts WHERE next_action_at <= now() ORDER BY
   next_action_at LIMIT $batch FOR UPDATE SKIP LOCKED`. For each row it sets `lease_owner`,
   `lease_until = now() + lease` and `next_action_at = lease_until`, so a crashed worker's
   items reappear. For `created` rows, the claim **is** T2 (with the kill-switch predicate).
   Commit.
3. **Calls.** Each item runs outside any tx, bounded by: a global concurrency cap; a
   per-(tenant, provider) concurrency cap; a per-call deadline shorter than `lease / 2`; and
   the breaker (§9.6).
4. **Phase C per item.** It clears the lease, sets `next_action_at` = backoff (exponential with
   jitter, capped) or NULL when terminal, and bumps `poll_count`.

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
| Resolution horizon | Manifest `settlement_window`; 24 h if undeclared |
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
Entering `submitting` is T2 or T12, which are CAS from `created`/`ambiguous`. The lease only
governs *re-polling*: an expired `submitting` lease leads to `QueryStatus`, never to a second
submission. The one exception is T12, which re-sends the **same key**, and only when the
provider deduplicates it.

---

## 8. Error, retry and timeout classification

The adapter maps every outcome of an outbound call to exactly one of:

| `ErrorClass` / outcome | Meaning | Source (typical) | State effect | Automatic retry |
|---|---|---|---|---|
| `NotSent` | Provably never dispatched | Credential unavailable; gate refusal; `httpclient` `Sent=false` (build error, dial refused before write, breaker open) | T5 | Yes, same attempt (INV-IO-9) |
| `NotProcessed` | Dispatched, but the vendor **documents** that this response means "not processed, safe to resend" (for example a specific 4xx or 429) | The vendor error model (intake #20). **Adapter-declared per code.** Anything undocumented is `Ambiguous`. | Treated as T5, but `ever_possibly_sent` stays **true** unless the vendor documents that the request left no trace. The re-send uses the same key. | Yes for deposits; for payouts only with `idempotent_submission` |
| `DefiniteDecline` | The vendor definitively refused (canonical `Declined`, with `Cascadable` and `decline_stage`) | Business decline, validation 4xx the vendor documents as final | T8 | No (cascade is a new attempt, §4.6) |
| `Ambiguous` | Anything else: timeout after a possible send, reset, unmapped 5xx, malformed response, panic | `httpclient` `Sent=true`, `ErrProviderMalformedResponse` | T6 | Only T12 (same key, `idempotent_submission`) |
| `Pending` | Accepted, with a reference | — | T4 | — |
| `Succeeded` (sync) | Definite success, only where the manifest has `sync_success_possible` | — | T7 | — |

- **Timeout classification.**
  - A timeout before dispatch (credential or queue) is `NotSent`.
  - A timeout after dispatch is `Ambiguous` and **never a failure**.
  - A settlement timeout (no final outcome within the horizon) is an escalation (T16), not a
    state.
- **httpclient idempotent retry.** It is enabled for a money-moving request only if the
  manifest has `idempotent_submission`, because it re-sends the same key. It is always
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
`Credential.ProviderID != ProviderID`, or `Credential.Domain != "payments"`.

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

### 9.3 Callback event (canonical)

`CallbackEvent` gains:

- `MerchantReference string` (optional; the echo of our reference, if the vendor supports it);
- `SettlementReference string` (optional; payout confirmation);
- `EventType ∈ {deposit, deposit_reversal, payout, payout_returned}`. `payout_returned` is
  unsupported (§5.2).

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
    Fetch(ctx context.Context, call CallContextLike, periodStart, periodEnd time.Time) (PaymentStatement, error)
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

Order of refusal (planning gate §9, adopted):

1. **At registration:** an inconsistent manifest or a missing prerequisite refuses to start.
2. **At new activity:** checked at T1/T2 and T1p.
3. **Never at settlement.** Callbacks, polls and postings for in-flight attempts always
   proceed.

The existing tenant layer (`provider_capabilities`, including `status`) still narrows routing
and is unchanged.

### 10.2 Kill switch — data (migration 0102)

`payment_kill_switches(tenant_id, provider_scope TEXT ('*' or provider_id), operation_scope
('deposit' | 'payout' | '*'), engaged BOOLEAN, reason_code, changed_by, changed_at, version)`.
It has `UNIQUE(tenant_id, provider_scope, operation_scope)`, `FORCE RLS`, CAS on `version`, and
every change audited (actor, tenant, before/after, IP, reason code).

`payment_kill_switch_release_requests` supports four-eyes release (§10.4). A platform-wide,
cross-tenant switch is **not built**; it is PROV-REVOKE-ALL-1, deferred. Its interim form is
the platform admin engaging each tenant's `'*'` row.

### 10.3 Semantics

**Engaged means no new outbound submission for the scope.**

- It is evaluated **atomically inside the claim tx** (T2, T1p) as a predicate of the CAS.
  There is no time-of-check/time-of-use gap between the check and the claim. The in-flight
  window is at most one already-claimed call.
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
- **Release:** four-eyes. The request is created by one principal and approved by a
  **distinct** one, reusing the ADR 0093 A1 request-binding shape. `security` may relax this in
  review.

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
| Tenant and provider isolation | The tenant comes from the attempt row, and the credential from `Resolve(tenant, provider)`. The adapter asserts equality (§9.1). The worker processes one tenant's items per claim tx, and no adapter struct field may hold an `OutboundCredential`, secret bytes or an authenticated client (reflection test over every registered adapter, §16). |
| No leakage | Audit, logs, errors and receipts carry `handle_id` and `fingerprint` only. Adapter error text is mapped to the closed `ErrorClass` plus an allow-listed reason, never vendor response bodies (which may echo headers). |
| No credential access while a financial lock is held | This follows from INV-IO-1: phase B holds no tx, and therefore no row, advisory or projection lock. Session-level advisory locks are forbidden on the call path. `txscope` refusal is the runtime backstop. |
| Excluded | AWS IAM, IRSA, KMS, egress proxy, mTLS (PROVIDER DEPENDENT; ADR 0093 A4). |
| Tripwire | `TestOutboundPrecondition_EveryWiredAdapterIsSynthetic` stays. It may be relaxed **per domain** only after that domain's PRH implementation and `security` review. The row pointer it prints is updated to this ADR. |

---

## 12. Payment reconciliation — MOCK foundation (stream `payment_statement`, migration 0103)

### 12.1 Pipeline

1. **Fetch** (no tx). Per (tenant, provider with a registered source), call `Fetch(ctx,
   CallContext, periodStart, periodEnd)`.
2. **Ingest** (short tx). Insert one `payment_statement_imports` row and its
   `payment_statement_lines`, both append-only (trigger). The import is idempotent on
   `(tenant, provider, source_label, coverage_start, coverage_end, content_digest)`.
3. **Match** (`WithTenantSnapshot`; REPEATABLE READ required and enforced; transaction-scoped
   advisory lock as in `TryRunCasinoStatementForTenant`). Read the import's lines, the
   `payment_attempts` and the ledger. Write one `reconciliation_runs` row plus
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
| `pay_unresolved` | The attempt is `submitting`/`pending`/`ambiguous` and older than the horizon, whether the line is absent or still `pending`. This is the only kind gated by age, to avoid false P1s inside the settlement window. |

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
| `pay_status_mismatch` (provider succeeded) | Operator T17 re-verify → `QueryStatus` → matrix → T7/T13 posting through the normal idempotent path |
| `pay_missing_platform_record` | Investigate: the dual-write orphan class this ADR removes, or foreign or forged activity. Credit only via LEDGER-MANUAL-ADJ-4EYES-1 (BLOCKED). |
| `pay_missing_provider_record`, `pay_amount_mismatch`, `pay_asset_mismatch`, `pay_reference_mismatch`, `pay_duplicate` | Escalate; never auto-resolve (reconciliation-model §2.2(a)); compensation via LEDGER-MANUAL-ADJ-4EYES-1 |
| `pay_unresolved` | The sweeper is already polling; the escalation (T16) alert is the same incident |

### 12.6 Amendment to reconciliation-model.md §2.2(b)

"The reconciliation job itself becomes the trigger to post the missing transaction" is
replaced by: "the reconciliation job flags it; the posting happens only through fresh
provider evidence (`QueryStatus`) applied by the payments state machine".

A statement line is not verified evidence of one payment. Treating a bulk file as posting
authority is exactly the "unreviewed path to arbitrary credits" §2.2 itself warns about.

---

## 13. Data model sketch

All tables: `tenant_id NOT NULL`, `ENABLE` + `FORCE ROW LEVEL SECURITY`, `tenant_staff_scope`
policy (the 0025 pattern), no player policy (players read intents, not attempts), excluded
from CDC.

### 13.1 Migration 0100 — payment attempts and provider-operation state (after 0099)

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
  payment_method           TEXT NOT NULL,
  asset_code               TEXT NOT NULL,
  amount                   NUMERIC(38,0) NOT NULL CHECK (amount > 0),
  interactive              BOOLEAN NOT NULL,
  merchant_reference       TEXT NOT NULL,       -- = id::text (INV-IO-3)
  external_idempotency_key TEXT NOT NULL,       -- = 'pa:' || id::text
  provider_reference       TEXT NULL CHECK (octet_length(provider_reference) <= PROVIDER_REF_MAX /* from 0099 */),
  state                    TEXT NOT NULL CHECK (state IN ('created','submitting','pending','ambiguous',
                                                          'succeeded','declined','rejected','disputed')),
  ever_possibly_sent       BOOLEAN NOT NULL DEFAULT false,
  CHECK (state <> 'created' OR NOT ever_possibly_sent),               -- INV-IO-9
  CHECK (state IN ('created','rejected') OR provider_id IS NOT NULL),   -- rejected may be unrouted (no_routable_provider); T15 sets provider_id from the evidence
  CHECK (state <> 'pending' OR provider_reference IS NOT NULL),
  submit_count             INT NOT NULL DEFAULT 0,
  claim_token              UUID NULL, lease_owner TEXT NULL, lease_until TIMESTAMPTZ NULL,
  next_action_at           TIMESTAMPTZ NULL,
  CHECK (state NOT IN ('succeeded','declined','rejected','disputed') OR next_action_at IS NULL),
  poll_count               INT NOT NULL DEFAULT 0,
  escalated_at             TIMESTAMPTZ NULL,
  decline_reason           TEXT NULL CHECK (octet_length(decline_reason) <= 128),
  decline_stage            TEXT NULL CHECK (decline_stage IN ('at_submission','after_acceptance')),
  cascadable               BOOLEAN NULL,
  terminal_reason          TEXT NULL CHECK (octet_length(terminal_reason) <= 128),
  ledger_transaction_id    UUID NULL REFERENCES ledger_transactions(id),   -- deposit success
  created_at, submitted_at, accepted_at, resolved_at, updated_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX ON payment_attempts (tenant_id, merchant_reference);
CREATE UNIQUE INDEX ON payment_attempts (tenant_id, external_idempotency_key);
CREATE UNIQUE INDEX ON payment_attempts (tenant_id, provider_id, provider_reference) WHERE provider_reference IS NOT NULL;
CREATE UNIQUE INDEX ON payment_attempts (tenant_id, deposit_intent_id, attempt_no) WHERE operation = 'deposit';
CREATE UNIQUE INDEX payment_attempts_one_live_per_intent ON payment_attempts (tenant_id, deposit_intent_id)
  WHERE operation = 'deposit' AND state IN ('created','submitting','pending','ambiguous');          -- INV-IO-8
CREATE UNIQUE INDEX payment_attempts_one_per_withdrawal ON payment_attempts (tenant_id, withdrawal_request_id)
  WHERE operation = 'payout';                                                                         -- INV-IO-8
CREATE INDEX idx_payment_attempts_due ON payment_attempts (tenant_id, next_action_at) WHERE next_action_at IS NOT NULL;
-- payment_attempts_guard (BEFORE UPDATE): allowed (OLD.state, NEW.state) pairs = §4.3 exactly;
--   immutable: id, tenant_id, operation, subject ids, attempt_no, amount, asset_code, payment_method,
--   interactive, merchant_reference, external_idempotency_key; provider_id and provider_reference
--   NULL→value once; ever_possibly_sent false→true only; submit_count monotonic.
-- payment_attempts_no_delete (BEFORE DELETE): RAISE.

CREATE TABLE payment_provider_events (   -- verified callback receipts (INV-IO-10)
  id UUID PRIMARY KEY, tenant_id UUID NOT NULL, provider_id TEXT NOT NULL,
  event_type TEXT NOT NULL CHECK (event_type IN ('deposit','deposit_reversal','payout','payout_returned')),
  provider_reference TEXT NOT NULL CHECK (octet_length(provider_reference) <= PROVIDER_REF_MAX),
  original_provider_reference TEXT NULL CHECK (octet_length(original_provider_reference) <= PROVIDER_REF_MAX),
  merchant_reference TEXT NULL CHECK (octet_length(merchant_reference) <= 64),
  settlement_reference TEXT NULL CHECK (octet_length(settlement_reference) <= PROVIDER_REF_MAX),
  outcome TEXT NOT NULL CHECK (outcome IN ('pending','succeeded','declined','ambiguous')),
  amount NUMERIC(38,0) NULL, asset_code TEXT NULL,
  event_fingerprint BYTEA NOT NULL,
  disposition_at_receipt TEXT NOT NULL CHECK (disposition_at_receipt IN
      ('applied','duplicate_effect','deferred_unresolved','anomaly','unsupported_event')),
  attempt_id UUID NULL REFERENCES payment_attempts(id),   -- one-shot NULL→value
  applied_at TIMESTAMPTZ NULL,                            -- one-shot NULL→value
  received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, provider_id, event_fingerprint)
);
CREATE INDEX ON payment_provider_events (tenant_id, provider_id, provider_reference) WHERE applied_at IS NULL;
-- guard trigger: append-only except the two one-shot columns; no DELETE. No raw body is ever stored.

-- Backfill (pre-flight, synthetic/dev data only): one attempt per deposit_intents row with a
-- provider_reference (state mapped: pending→pending, succeeded→succeeded, declined→declined,
-- ambiguous→ambiguous, ever_possibly_sent=true), and one payout attempt per withdrawal_requests row
-- in submitted/completed/failed. The pre-flight aborts if a row would violate a new constraint
-- (e.g. two live attempts), listing ids; it never coerces.
```

`deposit_intents` and `withdrawal_requests` get **no schema change**. The intent status CHECK
is unchanged. `withdrawal_requests.provider_reference` is already nullable; only its
application-level meaning changes (§4.7).

### 13.2 Migration 0102 — kill switch

```sql
CREATE TABLE payment_kill_switches (
  id UUID PRIMARY KEY, tenant_id UUID NOT NULL,
  provider_scope TEXT NOT NULL,                       -- '*' or a provider_id (charset per webhookauth.ValidProviderID)
  operation_scope TEXT NOT NULL CHECK (operation_scope IN ('deposit','payout','*')),
  engaged BOOLEAN NOT NULL, reason_code TEXT NOT NULL CHECK (octet_length(reason_code) BETWEEN 1 AND 64),
  changed_by UUID NOT NULL, changed_at TIMESTAMPTZ NOT NULL, version BIGINT NOT NULL,
  UNIQUE (tenant_id, provider_scope, operation_scope));
CREATE TABLE payment_kill_switch_release_requests (
  id UUID PRIMARY KEY, tenant_id UUID NOT NULL, kill_switch_id UUID NOT NULL REFERENCES payment_kill_switches(id),
  expected_version BIGINT NOT NULL, requested_by UUID NOT NULL, reason_code TEXT NOT NULL,
  approved_by UUID NULL CHECK (approved_by IS DISTINCT FROM requested_by), status TEXT NOT NULL
  CHECK (status IN ('open','approved','cancelled','expired')), created_at, decided_at);
-- No manifest table: the manifest is code-declared (ADR 0022 §2.1). History of switch changes = audit_log.
```

### 13.3 Migration 0103 — payment statement reconciliation (MOCK)

```sql
CREATE TABLE payment_statement_imports (
  id UUID PRIMARY KEY, tenant_id UUID NOT NULL, provider_id TEXT NOT NULL,
  source_label TEXT NOT NULL, is_mock BOOLEAN NOT NULL,
  coverage_start TIMESTAMPTZ NOT NULL, coverage_end TIMESTAMPTZ NOT NULL, CHECK (coverage_end > coverage_start),
  line_count INT NOT NULL, content_digest BYTEA NOT NULL, fetched_at TIMESTAMPTZ NOT NULL,
  UNIQUE (tenant_id, provider_id, source_label, coverage_start, coverage_end, content_digest));
CREATE TABLE payment_statement_lines (
  id UUID PRIMARY KEY, tenant_id UUID NOT NULL, import_id UUID NOT NULL REFERENCES payment_statement_imports(id),
  line_no INT NOT NULL, provider_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('deposit','deposit_reversal','payout')),
  provider_reference TEXT NOT NULL CHECK (octet_length(provider_reference) <= PROVIDER_REF_MAX),
  merchant_reference TEXT NULL, original_provider_reference TEXT NULL,
  status TEXT NOT NULL CHECK (status IN ('pending','succeeded','declined','reversed')),
  amount NUMERIC(38,0) NOT NULL, asset_code TEXT NOT NULL, occurred_at TIMESTAMPTZ NOT NULL,
  UNIQUE (import_id, line_no));   -- duplicates across line_no are kept so pay_duplicate is detectable
-- both: append-only triggers (no UPDATE/DELETE), FORCE RLS.
-- reconciliation_mismatches.mismatch_kind widened (strict superset of 0098) with:
--   pay_missing_platform_record, pay_missing_provider_record, pay_amount_mismatch, pay_asset_mismatch,
--   pay_reference_mismatch, pay_status_mismatch, pay_duplicate, pay_unresolved.
```

---

## 14. Lock ordering — proposed ADR 0082 Amendment A7 (`ledger-finance` to accept)

- **L1 list extended.** `deposit_intents` and `payment_attempts` are appended to the L1 table
  list, in that order, after the existing tables. So `withdrawal_requests` comes before
  `payment_attempts`, and `deposit_intents` comes before `payment_attempts`.
- **Receipt insert first.** The `payment_provider_events` receipt-key insert runs **first** in
  a callback tx, before any L1. It is an index-insertion wait that serializes duplicate
  deliveries, analogous to L0.1. `ledger-finance` to classify it as a named step (proposed
  "R0").
- **Kill-switch read takes no lock.** It is a plain `SELECT`.
- **Per-path order:**
  - deposit evidence: R0 → intent `FOR UPDATE` → attempt CAS → (L2 original, reversal only) → L3
    (`LockProjectionsForPosting` inside `Post`) → L4;
  - payout evidence: withdrawal `FOR UPDATE` → attempt CAS → L3 → L4;
  - cascade insert: while holding intent and attempt, insert attempt n+1. The partial unique
    index is the only wait, and it is a key the concurrent contender needs under the same
    intent lock, so there is no cycle.
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
| Reconciliation | casino_consistency and casino_statement are unchanged. |
| Audit | `casino.launch_requested` (new), `casino.launched` (moved to phase C), `casino.launch_failed` (new). |
| Migration | None. |

### 15.2 KYC `CreateVerification`

| Aspect | Specification |
|---|---|
| Intent | The `kyc_verifications` row inserted in phase A with `status='unverified'`, `provider_id` set and `provider_reference NULL`, plus audit `kyc.verification_requested`. |
| Reference / idempotency key | `CreateVerificationInput` gains `Call CallContext` and `ExternalReference = verification.id`. The vendor idempotency key is `"kv:" + id` if the vendor supports keys (PROVIDER DEPENDENT). |
| Flow | A (commit) → B (`CreateVerification`) → C: CAS `UPDATE … SET provider_reference=$r, status=$s WHERE id=$1 AND provider_reference IS NULL AND status='unverified'` plus audit `kyc.verification_submitted` (existing action, moved). |
| Retryability | No automatic retry. A player retry creates a new row (existing behaviour). An orphan `unverified` row without a reference has **no enforcement effect** (it is not verified). |
| Callbacks | A callback that races phase C (reference unknown) gets a retryable response class, so the vendor redelivers after C commits. The merchant-reference echo is PROVIDER DEPENDENT. `identity-compliance` confirms the exact mapping in PRH-I2. No receipt table. |
| Failure recovery | Crash after A: an orphan row, harmless. After B: an orphan vendor verification. KYC enforcement (ADR 0096) reads only platform rows, so it is harmless. |
| Migration | None. |

### 15.3 KYC `SubmitVerification` (document upload)

| Aspect | Specification |
|---|---|
| Flow | A: insert document plus audit → commit. A short read-only tx gathers the current non-rejected document set. B: `SubmitVerification(Call, providerReference, docs)` with the idempotency key `"ks:" + verification_id + ":" + sha256(sorted document ids)`. C: the existing `normalizeProviderResult` plus the terminal-guarded `updateVerificationStatus` plus audit (existing). |
| Failure recovery | Identical to today's `ProviderError` outcome: the verification stays as it is, and the next upload re-submits the full set. A durable KYC submission outbox is **deferred** (candidate KYC-SUBMIT-OUTBOX-1, triggered by the first real KYC adapter's intake). |
| Migration | None. |

---

## 16. Failure injection and required tests

### 16.1 Crash and failure-injection points

These are hooks in the MOCK adapter and orchestrator seams, test-only.

| ID | Point | Required converged outcome |
|---|---|---|
| CP-D1 | After phase A commit, before the call | Attempt `created`. Player retry → drives T2. Interactive and abandoned → T3 after the window. The provider never saw it. |
| CP-D2 | After the T2 commit, before the call | `submitting`. Lease expiry → sweeper `QueryStatus` → `not_found`: authoritative → `declined`; else `ambiguous` → T12 if allowed. No double submission without the same key. |
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

### 16.2 Adversarial test list (each financial test ends with INV-IO-13)

1. **No double credit.** Concurrent duplicate success callbacks, a success callback racing the
   sweeper poll, and a success callback racing phase C: exactly one Flow 1.
2. **No double debit or payout.** CP-W4 and CP-W5. T12 resubmission with the MOCK in
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
   - `Ambiguous` never re-calls `Deposit`/`Withdraw` unless `idempotent_submission`.
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
      unique index).
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
    - A pool-starvation variant: N concurrent slow MOCK calls (sleep > 1 s) with a 20-connection
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
    - KYC submit: crash after the upload commit is recovered by the next upload.

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
- The end-to-end LF-C1(b) path: a dropped success callback is flagged, T17 posts it, and the
  next run is clean.

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

---

## 17. Conformance to ledger-finance LF-C2 and LF-C1

| Constraint | Where satisfied |
|---|---|
| LF-C2 #1: intent committed in `submitting` before any call; no tx held | D1; INV-IO-1, INV-IO-2; T2/T1p commit before phase B |
| LF-C2 #2: deterministic, persisted external key; player retry resumes | INV-IO-3; §5.1 "Idempotency keys" |
| LF-C2 #3: explicit CAS state machine; illegal and backward transitions rejected; every transition audited | §4.3; INV-IO-4; trigger |
| LF-C2 #4: callback resolves by merchant reference and provider reference, and accepts `submitting` | §6.1 step 5; §6.4; T7/T8 from `submitting` |
| LF-C2 #5: sweeper `QueryStatus` with no tx; never auto-declines on unknown; never cascades on unknown | §7; §4.5; §4.6 |
| LF-C2 #6: cascade through an outbox, never inline in a webhook tx | D3; §4.6; §6.5 |
| LF-C2 #7: ledger posting never spans I/O; ADR 0082 order; authoritative read in the same tx | INV-IO-5; §14 |
| LF-C2 #8: failure-injection tests end with the SUM and rebuild assertions | INV-IO-13; §16 |
| LF-C2 #9: the same rule for payout dispatch | §5.2; T1p; CP-W1..W6; INV-IO-7 |
| LF-C1: record redelivery semantics; (a) or (b) | §6.6 (manifest fields plus startup enforcement of (b)); (a) left to `security` |

---

## 18. Implementation work breakdown

Every item starts `NOT IMPLEMENTED`. The ADR must reach ACCEPTED (ledger-finance plus security
sign-off) before I1 starts.

### PRH-I1 — payments (+ `ledger-finance`; reviewers `security`, `code-reviewer`, `qa`)

| # | Item | Depends on |
|---|---|---|
| I1-a | Migration 0100 (tables, indexes, guard triggers, pre-flight plus backfill, RLS tests) | 0099 merged; ADR 0082 A7 accepted |
| I1-b | Contract (§9): `CallContext`, `ErrorClass`, `StatusQuery`, `CallbackEvent` fields, manifest (§10.1), `HealthStatus` contract. MOCK extended: tenant-tagged records; merchant-reference lookup; idempotent-by-key mode; `NotSent`/`NotProcessed`/`Ambiguous`/timeout/crash hooks; call counters. Conformance suite cases. | — |
| I1-c | Provider-call gate (§3.2) plus the outbound credential plumbing (§11) plus the synthetic credential source; static scan; reflection test | I1-b |
| I1-d | `applyEvidence`, transition functions, audit; `InitiateDeposit` rewrite (takes `*db.Pool`); routing split; breaker | I1-a, I1-c |
| I1-e | Callback path: receipts, merchant-reference resolution, deferred application, removal of inline I/O and cascade | I1-d |
| I1-f | Payout dispatch and resolve handlers rewritten; `withdrawal.ClaimForDispatch`/`RecordProviderReference`; M3; `withdrawal-state-machine.md` update | I1-d |
| I1-g | Sweeper worker (§7) with its `cmd/platform-api` wiring and nudge | I1-d, I1-e |
| I1-h | Migration 0102 plus kill-switch service, admin API, permissions, four-eyes release, audit; manifest registration checks (incl. LF-C1(b) startup rule) | I1-b |
| I1-i | Tests §16.1, §16.2 items 1–17, mutations MX1–MX8; tripwire message update (the tripwire itself stays) | all of the above |

I1-b, I1-h and I1-a can proceed in parallel. I1-d through I1-g are sequential.

### PRH-I2 — casino plus KYC (`casino`, `identity-compliance`)

| # | Item | Depends on |
|---|---|---|
| I2-a | Shared call-gate helper (tiny; `txscope` refusal plus deadline). It is either extracted from I1-c or written first and consumed by I1-c. It is the only coordination point. | — |
| I2-b | Casino launch split (§15.1), `LaunchRequest.Call`, `HealthStatus` outside the tx, audits | I2-a |
| I2-c | KYC create and submit split (§15.2–15.3), `Call` on the inputs, callback-race response class | I2-a |
| I2-d | Tests §16.2 item 18, plus casino and KYC in the scans for item 14 and item 15 | I2-b, I2-c |

PRH-I2 does not depend on migrations 0100/0102/0103 and can run fully in parallel with I1.

### PRH-I5 — payment reconciliation (`payments` + `ledger-finance`)

| # | Item | Depends on |
|---|---|---|
| I5-a | `statement.PaymentStatementSource` leaf types (§9.5) | — |
| I5-b | Migration 0103 | 0100 (FK-free, but it reads `payment_attempts` at match time) |
| I5-c | Fetch → ingest → match stream, the eight classifiers, `TryRun…ForTenant`, scheduler wiring | I5-a, I5-b, I1-a |
| I5-d | `payments.MockStatementSource` | I1-b (tenant-tagged MOCK records) |
| I5-e | Tests §16.3, MX9; `reconciliation-model.md` §2.2 amendment and a new §2.2 implementation status | I5-c, I5-d |

The matcher (I5-c) can be written against fixture sources before I1-d lands.

### Specialist-owned document changes (not code)

- `ledger-finance`: ADR 0082 A7.
- `payments`: `payment-orchestration.md` §5 note; `withdrawal-state-machine.md`.
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

### 19.2 Specialist review questions (not human decisions)

| ID | Owner | Question |
|---|---|---|
| LF-Q1 | `ledger-finance` | T13 posting to `player_cash` versus a suspense account (§4.4) |
| LF-Q2 | `ledger-finance` | Acceptance of Amendment A7 and the "R0" receipt-insert placement (§14) |
| LF-Q3 | `ledger-finance` | The §12.6 amendment to reconciliation-model §2.2(b) |
| S-Q1 | `security` | Four-eyes on kill-switch release (§10.4), or single actor |
| S-Q2 | `security` | The `deferred_unresolved` 200 response (§6.2) versus today's 404; confirm it opens no oracle, since it is post-verification only |
| S-Q3 | `security` | LF-C1 option (a) remains open |
| IC-Q1 | `identity-compliance` | The KYC callback-race response class (§15.2) |
| P-Q1 | `payments` | The §7.3 defaults, and whether any existing adapter test relies on today's inline webhook cascade |

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
- `deferred_unresolved` changes one 404 to a 200 (S-Q2).
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

### Findings and deferred candidates (for the orchestrator to register)

| ID | Kind | Item |
|---|---|---|
| **CAS-STMT-IO-1** | Finding, Low, not reachable today | `CasinoStatementSource.Statement(ctx, tx, …)` reads inside the run's transaction. A real casino statement source would be provider I/O inside a tx (the INV-IO-1 class). It must adopt the fetch → ingest → match split of §12.1 before the first real casino statement source. No redesign now. |
| KYC-SUBMIT-OUTBOX-1 | Deferred | Durable KYC submission outbox. Trigger: the first real KYC adapter intake. |
| PAY-ATTEMPT-RETENTION-1 | Deferred | Retention and partitioning of `payment_attempts`/`payment_provider_events`. Trigger: a volume threshold. |
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
