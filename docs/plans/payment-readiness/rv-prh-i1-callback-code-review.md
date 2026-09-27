# RV-PRH-I1 callback cutover: independent code review

- Reviewer: `code-reviewer` (independent of the implementer)
- Date: 2026-09-27
- Subject: PRH-payments-callback-cutover, merged at `c078968` (local HEAD `316f048`).
  Commits reviewed: `0580d0d`, `067b3b3`, `54c94c7`, `f048a88`, `c075499`, `d9b0b6d`,
  `367b017`, `05b3684`.
- Scope: `internal/payments/{receipt.go,orchestrator.go}`, `internal/httpserver/deposit_handlers.go`,
  the OpenAPI entry for `/v1/webhooks/payments/{tenantSlug}/{providerId}`, the migrated tests,
  `internal/payments/receive_bridge_test.go`,
  `internal/httpserver/payment_webhook_contract_integration_test.go`, and ADR 0095 §27.10.
- Method: read the actual diff (not the summary). Ran probes and mutations in a detached worktree at
  `316f048` under the session scratchpad, against a private scratch DB (`rv_prhi1cb_scratch`: created via
  `TEST_ADMIN_DATABASE_URL`, migrated 1-105, grants taken from `deploy/init-app-role.sql`). The DB was
  dropped and the worktree removed afterwards. The baseline `go test -tags=integration ./internal/payments/`
  passed (134 s).

## Verdict: NOT READY

Three correctness bugs are confirmed by probe on the path this cutover makes live (F1-F3), plus one
confirmed regression that lets a reversal hit a payout (F4). The §6.2 uniform-200 body itself is
correct and does not leak disposition. The PAY-REV-1 / tenant-isolation / replay tests kept their
adversarial strength. However, several invariants that this cutover introduced or relies on have
surviving mutants (F5), and the §27.10 record overstates how much of this was verified (F6).

F3 and F4 are financial/security-domain concerns. They are raised here with concrete reproductions
but must be adjudicated by `ledger-finance` (F3, F4) and `security` (F4). This review does not
override their veto either way.

---

## F1 (High, confirmed): reversal and anomaly receipts are never resolved, so they permanently fill the §6.1 unapplied-receipt cap

`applyReversalReceiptEvidence` (receipt.go, tombstone branch and posting branch) inserts its receipt
with `insertReceiptDeduped(..., DispositionApplied)` but never calls `ResolveReceipt`. The anomaly
branches of `ApplyReceiptEvidence` (receipt.go:305-310, 335-343) do the same. That code predates the
cutover but is live now. `CountUnappliedReceipts` counts `resolved_at IS NULL` per (tenant, provider),
and nothing ever resolves these rows.

Probe (`internal/payments`, via `receiveCallbackInTx`): one deposit success, one posted reversal, and
one tombstone reversal. Observed:

```
receipt event_type=deposit           disposition=applied unresolved=false
receipt event_type=deposit_reversal  disposition=applied unresolved=true
receipt event_type=deposit_reversal  disposition=applied unresolved=true
CountUnappliedReceipts after 1 applied deposit + 2 applied reversals = 2
```

Failure scenario: every chargeback, refund or anomaly ever received for a (tenant, PSP) pair counts
toward `DeferredReceiptCap` (10,000) for the life of the platform. Once that lifetime count is
reached, every callback that cannot be resolved yet gets 503 plus the P1
`payment_webhook_deferred_receipt_cap_exceeded` line, with nothing stored. That includes the ordinary
race where a PSP's success callback arrives before phase C commits the provider reference. The
condition never clears. Evidence for any event that never becomes resolvable is lost once the vendor's
retry window expires. A related latent hazard: `ApplyDeferredReceiptsForAttempt` selects unresolved
receipts by `provider_reference` alone, with no `event_type` filter. A reversal receipt, whose stored
outcome is normalized to `succeeded`, is therefore eligible to be replayed as deposit-success evidence
if its own reference ever equals an attempt's reference. Today the tombstone check absorbs this, but
only by coincidence.

Required: resolve reversal receipts in the same transaction (terminal resolution, attempt_id =
resolved original or NULL), resolve anomaly receipts with their `anomaly_*` resolution, and restrict
the deferred-apply query to `event_type = 'deposit'` (or the payout types, when wired). Add a test
asserting `CountUnappliedReceipts == 0` after applied reversal and anomaly receipts.

## F2 (High, confirmed): a verified decline whose vendor `decline_reason` exceeds 64 bytes fails every delivery

`receiveCallbackViaReceiptPath` passes `event.DeclineReason` through unbounded.
`payment_provider_events.decline_reason` has `CHECK (octet_length <= 64)`, and so does
`payment_attempts.decline_reason`. Neither the mock adapter's parser nor `validateReceiptReferences`
bounds or normalizes it. Before the cutover the callback path only wrote the reason into audit JSON
metadata, so this is a regression.

Probe: pending attempt, then a signed `deposit`/`declined` callback with reason
`"Transaction declined by issuer: Do Not Honor (05) - contact card issuer for details"` (83 bytes):

```
payments: insert receipt: ERROR: new row for relation "payment_provider_events" violates check
constraint "payment_provider_events_decline_reason_check" (SQLSTATE 23514)
```

Failure scenario: a real PSP sends a normal-length human-readable decline message. The handler
returns 500 (`payment_webhook_failed`). The PSP redelivers the identical payload forever and it fails
identically every time. The attempt never records its decline from the callback. The sweeper's
QueryStatus path also passes the vendor reason into `ApplyDecline`, so it fails the same way if the
vendor returns the same text. The cascade never happens and the player's deposit stays pending. An
`asset_code` over 16 bytes on a success callback is the same class of failure (less realistic).

Required: normalize every free-text vendor field at the adapter boundary (bounded, charset-cleaned,
or mapped to a code with the raw text only in redacted audit metadata). Add a test with an oversized
reason at both the receipt and QueryStatus paths.

## F3 (High, confirmed; ledger-finance to adjudicate): the cascade "intent already succeeded" guard is dead, so a cascade attempt is created for an already-credited deposit

In `applyResolvedReceiptEvidence`'s `OutcomeDeclined` branch (receipt.go:474-497), `finalizeDeclined`
runs first. It unconditionally writes `deposit_intents.status='declined'` and overwrites
`provider_id`/`provider_reference` via `setIntentAttempt`. `liveIntent` is then re-read, so
`cascadeEligible(..., liveIntent.Status, ...)` always sees `declined`, and its
`intentStatus == DepositIntentSucceeded` guard can never fire. `recomputeDepositIntentProjection`
repairs the status only afterwards. `drive.go:271/282` and `sweeper.go:384/395` share the same
pattern, and `drive.go`'s cascade T2 path has no intent-status check before submitting.

Probe (two mock PSPs, `MaxCascadeDepth=5`):

1. A1 gets a cascadable decline, which creates A2.
2. A late T13 success arrives for A1, which posts and makes the intent `succeeded`.
3. A2 is claimed and accepted, then gets a cascadable decline callback.

Observed:

```
attempts=1:succeeded,2:declined,3:created intent.status=succeeded
intent.provider_reference==A2's ref: true   deposit.declined audits=2   cash=5000
```

Failure scenario: a deposit the platform has already credited gets a new `created` cascade attempt
(A3). The sweeper routes it to a third PSP with no intent-status check, which asks the player to pay
again for the same deposit request. Along the way the intent's `provider_reference` is repointed at
a declined attempt while `ledger_transaction_id` still names A1's posting. A `deposit.declined`
audit record is also written against an intent that succeeded.

Suggested fix, for ledger-finance to confirm: evaluate `cascadeEligible` against the projection
computed before `finalizeDeclined`, or against `recomputeDepositIntentProjection`'s result. Stop
`finalizeDeclined` from overwriting a succeeded intent. Separately, have ledger-finance decide
whether a still-`created` sibling (A2 above, before step 3) may be dispatched at all once a T13
success lands.

## F4 (Medium, confirmed; ledger-finance and security to adjudicate): a `deposit_reversal` naming a payout's reference is applied as a tombstone and then blocks that payout's completion

`applyReversalReceiptEvidence` resolves `OriginalProviderReference` with
`GetAttemptByProviderReference`, which covers any operation, deposits and payouts alike. Payout
attempts never carry `ledger_transaction_id` (`payout.go:366-377` completes through
`withdrawal.Complete`). So the `unresolved || original.LedgerTransactionID == nil` tombstone branch
runs before the `original.Operation != AttemptOperationDeposit` integrity guard. That guard is
unreachable for payouts, even though the code comment says a payout "is data corruption ... never
routed to the tombstone branch".

Probe: pending payout attempt (ref R), then a verified `deposit_reversal` with `original=R`, then
`withdrawal.Complete(R)`:

```
reversal naming payout ref: disposition=applied tombstoned=true err=<nil>
subsequent withdrawal.Complete: withdrawal: post completion: ledger: look up existing transaction
for idempotency key: no rows in result set
```

Failure scenario: a mistaken or malicious (but correctly signed) reversal naming a payout reference
writes a `TxTombstone` on (provider, R). When the payout then succeeds, `withdrawal.Complete` hits the
(tenant, provider_id, provider_tx_id) uniqueness conflict on every retry. The withdrawal stays
`submitted` with the player's funds held, while the PSP has actually paid out. The pre-cutover code
resolved reversals against `deposit_intents` only, so it could not do this.

Required: resolve reversal originals with `operation='deposit'` only, or check the operation before
the tombstone branch. A non-deposit match should be `ErrDepositReversalIntegrity`. Add the probe as a
regression test. This also turns surviving mutant M5 into a killable one.

## F5 (Medium): adversarial-strength gaps: surviving mutants on the new branches

Each mutant below was applied to a byte-exact anchor, the named tests were run under
`-tags=integration`, and the file was restored. `git status` was clean after every run.

| ID | Mutation | Tests run | Result |
|---|---|---|---|
| M1 | reversal: drop `deposit_intents ... FOR UPDATE` | PayRev1/LockOrder/Reversal | SURVIVED (harmless: the S2 ledger lock still serializes; §14 intent-first order is unpinned) |
| M2 | reversal: drop both intent and S2 ledger locks | same | KILLED (`payrev1_concurrency:126`, statement-pinned) |
| M14 | reversal: drop only the S2 ledger `FOR UPDATE` | same | KILLED (same) |
| M3 | reversal: amount cross-check disabled | full pkg | KILLED |
| **M4** | reversal: `ReversesTransactionID: intent.LedgerTransactionID` (reverse the intent's first capture, not the resolved attempt's) | full pkg | **SURVIVED**. LF95-C6(b) is the stated reason this function exists, and no test reverses a T13 second capture. |
| **M5** | reversal: payout/parentless-attempt guard disabled | full pkg | **SURVIVED**. Equivalent today because of F4 (guard unreachable). |
| **M6b** | projection: `disputed` no longer projects to `ambiguous` (falls to `declined`) | full pkg | **SURVIVED**. The LF95-C7 "never declined, funds may be captured" rule is unpinned. The T10/T13t tests assert attempt state but never intent status. |
| **M7** | projection recompute removed from `ApplyDeferredReceiptsForAttempt` | full pkg | **SURVIVED**. The sweeper-backstop half of the round-2 fix is untested. |
| M8 | S4 already-reversed re-check disabled | full pkg | SURVIVED (the 0092 index backstop returns the same typed error, so this is observationally equivalent; acceptable, but note S4 is not independently pinned) |
| M9 | callback `DeclineStage=after_acceptance` not set | full pkg | KILLED |
| **M10** | cap boundary `n > cap` changed to `n >= cap` | httpserver cap/uniform | **SURVIVED**. The test fills `cap+1` rows. As coded, the 10,001st unresolved receipt is still accepted, while the handler comment and P1 text say "at or past the limit". |
| **M11** | handler sets `X-Receipt: <disposition>` on the 200 | httpserver `PaymentWebhook\|Webhook_` | **SURVIVED**. The uniform test checks a hard-coded deny-list of six invented header names instead of comparing header key sets across dispositions. |
| **M12** | reversal provider-mismatch log line gets `"error", err` (err embeds claimed/actual amounts) | httpserver webhook/reversal | **SURVIVED**. The old test's log-redaction assertion was removed (`_ = captured`) when the deposit mismatch moved to 200/disputed. The reversal path, the only remaining `ErrCallbackProviderMismatch` producer, has no HTTP test. |
| **M13** | handler's `ErrCallbackProviderMismatch` branch removed (falls to 500 and logs raw err) | same | **SURVIVED**. Same gap: the reversal-mismatch status and redaction are unpinned. |

Also in `TestPaymentWebhook_UniformResponseAcrossDispositions`: the case labelled "anomaly_mismatch"
and the "tombstone_collision" case both produce `DispositionApplied` (T10 disputes count as
`changed=true`). A real `DispositionAnomaly` (cross-provider merchant reference, or reference
conflict) is never sent over HTTP, so "6 dispositions" is really 3.

Old-vs-new comparison for the specific areas requested:

- **PAY-REV-1 concurrency / defect repro**: unchanged assertions. The fixture now builds the original
  through a real receipt-path success callback, so the reversal exercises the real attempt linkage.
  The exact S2 statement is still pinned (M2/M14 killed). Not weakened.
- **Tenant isolation (PAY-REV-1)**: the corruption target moved to
  `payment_attempts.ledger_transaction_id`, which is the column S2 now reads. This is correct and
  still exercises RLS on the S2 lock. Not weakened.
  `TestReceiveCallback_CrossTenantProviderReferenceIsInvisible` still stops at signature
  verification, so no test has a correctly signed tenant-B callback naming a tenant-A reference
  reach the receipt resolver. This gap predates the cutover, but the new resolver (including the
  RLS-only `GetAttemptByMerchantReference`) is uncovered.
- **Replay/duplicate**: unchanged assertions. Not weakened.
- **Lock order**: `TestLockOrder_ConcurrentDepositAndDepositReversal_NoDeadlock` is fixture-only
  change. The new intent-then-ledger order in the reversal path is not pinned (M1).
- **RG enforcement**: `rg_enforcement_integration_test.go` is untouched and still valid. The callback
  path has no RG gate, same as before. Cascade RG now runs at drive T2, which is outside this diff.
- **Removed assertion**: `TestWebhook_ProviderMismatchAfterVerification_*`'s log-line redaction check
  (see M12). The ADR says "No test was deleted, skipped, or weakened", which is inaccurate on this
  point.

## Test bridge assessment (`receive_bridge_test.go`)

The bridge does not mask the behaviours the migrated tests actually exercise. Deposit confirmations in
the PAY-REV-1, F7, replay and lock-order fixtures go through the real receipt path, and the bridge only
creates the `submitting`/`pending` attempt row. Where it does invent transitions, the shapes have no
real-path analogue:

- `DepositIntentFailed` is mapped to a live `pending` attempt. A failed intent would never have one in
  production.
- For an intent that `InitiateDeposit` cascaded in-process, only the last provider's attempt is
  backfilled. Earlier attempts do not exist, so a callback for an earlier provider's reference
  resolves to `deferred_unresolved` instead of the T13 cell.
- `DepositIntentSucceeded` is linked with `EvidenceSync` to the legacy posting.

No current test sends a callback against the first two shapes, so nothing is masked today. Anyone
adding such a test would be testing fiction. A structural concern remains: the fixture engine is the
production-dead legacy `InitiateDeposit`/`attemptDeposit`/`handleDecline`/`resolveAmbiguous` chain
(see F7), not `InitiateDepositAttempt`.

## §6.2 uniform-200 leak check

- **Body**: correct. `webhookReceivedResponse{request_id, received}` is used for every 200. OpenAPI and
  its contract test agree.
- **Headers**: no disposition header is set today, but the test would not catch one (M11). `Retry-After`
  appears only on the 503s.
- **Logs**: `payment_webhook_applied` logs `disposition` at Info, which is acceptable because it is
  operator-only. The event name is misleading for deferred and anomaly receipts (nit). No raw vendor
  text reaches logs on the 200 path. On the 500 fallback, `payment_webhook_failed` logs `err`. Today
  that can carry pg CHECK messages (F2) but not vendor text, because pgx's `Error()` omits `Detail`.
  Under M13, it would carry the reversal amounts.
- **Residual verified-sender oracle (informational)**: for `deposit_reversal`, an unknown or unposted
  original returns 200 (and writes a tombstone), an already-reversed one returns 409, an
  amount/asset mismatch returns 400, and corruption returns 500. §6.2's carve-out for typed reversal
  rejections sanctions this. Recorded for `security` awareness only.

## F6 (Low): implementation record and evidence accuracy

- ADR 0095 §27.10 says "No test was deleted, skipped, or weakened", but one log assertion was removed
  (M12).
- §27.10(a) says a resolved reversal original "is locked ... before the integrity check", but payouts
  take the tombstone branch without reaching the integrity check (F4).
- The evidence file (round 2) counts six dispositions in the uniform test, but the test has no true
  `anomaly` (F5). It also records "migrations 1-104" for a branch that carries 0105.
- The mutation evidence for this cutover covers mutants A and B and the projection call site only.
  It is credible for what it covers: I reproduced the M9 kill and the analogous kills, and A/B are
  consistent with the code. It has no mutant on the new reversal locks, T13 reversal targeting,
  the operation guard, the projection rule itself, the deferred-path recompute, the cap boundary or
  the header or log contract. All of those except the locks survive (F5).

## F7 (Low): dead and stale code left by the cutover

- `Orchestrator.InitiateDeposit` → `attemptDeposit` → `handleDecline`/`resolveAmbiguous` has no
  non-test caller. It is production-dead and now exists only as the test bridge's fixture engine.
  Either retire it with a real-path fixture or label it explicitly as a test-support shim.
- The `ErrDepositIntentNotFound` branch in `newPaymentWebhookHandler` (deposit_handlers.go:506-513) and
  the public-route branch in `mapReceiveCallbackError` would emit a 404 that the OpenAPI now says this
  route never returns. `GetDepositIntentByID` can only reach it inside the receipt path on data
  corruption. Map it to the integrity 500 or delete it.
- Stale references to removed functions:
  - deposit_handlers.go:499 (`receiveDepositReversalCallback`)
  - payment_deposit_simulation_handlers.go:41-80. The safety rationale still cites
    "receiveDepositCallback's own terminal-state short-circuit (line ~894)", which no longer exists.
    The handler's own pending-only gate is still the operative defence, so the text should say so.
  - payrev1 test headers and webhook_admission_t6 test comments.
- receipt.go:786-789: `tombstoneExists`'s doc comment now sits above `recomputeDepositIntentProjection`.
- insertReceiptDeduped's `&& ev.EventType == deposit` guard is redundant, because reversal outcomes are
  already normalized. It is harmless.
- The OpenAPI operation description and its 400 response still say an amount/asset contradiction on a
  deposit returns 400. A deposit mismatch now returns 200 and the attempt goes to disputed; only a
  reversal mismatch returns 400.

## Required before this can be marked complete

1. Fix F1, F2 and F4, each with a regression test that fails on the current code. The F1-F4 probes in
   this review are ready-made starting points.
2. Get `ledger-finance` sign-off on F3 and on the T13 sibling-dispatch question, and `security`
   sign-off on F4.
3. Kill M4, M6b, M7, M10, M11, M12 and M13 with real assertions:
   - a T13 second-capture reversal test;
   - intent-status assertions on the disputed paths;
   - a deferred-apply projection test;
   - a cap boundary test at exactly `cap` rows;
   - a header key-set equality check across dispositions;
   - an HTTP reversal-mismatch test asserting the status and the absence of `error` on the log line.
4. Correct the §27.10 and evidence claims (F6).

---

## Re-review 1: fix round merged at `b3563c8` (2026-09-27)

- Commits reviewed: `08b84d1`, `91ead85`, `d25a3fd`, `158ac86`. I read the diff `b3563c8^1..b3563c8`
  directly rather than relying on the commit messages.
- Environment: a detached worktree at `b3563c8` and a private DB `rv_prhi1cb2_scratch`, built with
  `priv_db.sh` plus the grants from `deploy/init-app-role.sql`.
  - All four test URLs, including `TEST_ADMIN_DATABASE_URL`, pointed at that DB.
  - The DB was dropped and the worktree removed afterwards.
- Baseline results:
  - `go test -tags=integration ./internal/payments/` passed (228 s).
  - The payments/webhook subset of `./internal/httpserver/` passed.
- Mutants: the implementer supplied no transcripts this round, so every mutant below is my own.
  - Each edit was applied to a byte-exact anchor and the file restored afterwards. `git status` was clean
    after every batch.
  - Payments mutants ran against the full `internal/payments` package. HTTP mutants ran against the named
    webhook tests.
  - Four first-attempt mutants did not compile. They were redone as N2b–N5b and are not counted.

### Verdict: still NOT READY

F1 (reversal half), F4 and F5's M4/M6b/M10–M13 are genuinely closed and pinned. Two points block:

- **F2 is not fixed.** My original probe fails byte-for-byte the same way as before.
- **F3 is only half-fixed.** The sticky status/reference part works, but the receipt path still creates
  a cascade child for an intent that has already succeeded. The new F3 regression test cannot detect
  either half.

### Per-finding status

| Finding | Status | Evidence |
|---|---|---|
| **F1** resolved receipts + `event_type` filter | **Partially closed.** Reversal receipts: closed. Anomaly receipts and the event_type filter: fixed in code but **unpinned**. | Probe: `CountUnappliedReceipts` stayed at 0 after two declines, and the reversal cases pass P5. N2b (posting-branch resolve removed) and N3b (tombstone-branch resolve removed) are KILLED by `TestRVLF_P5`. **N4b, N5b and N5c SURVIVED**: removing `ResolveReceipt` from the precondition-anomaly, reference-conflict or cross-operation anomaly branches passes the whole package, so the anomaly half of F1 has no test. **N1 SURVIVED**: dropping `AND event_type = 'deposit'` from `ApplyDeferredReceiptsForAttempt` passes the whole package. |
| **F2** bounded decline reason | **OPEN (not fixed).** | See R1 below. N6 (make `boundedDeclineReason` the identity function) **SURVIVED**. No test anywhere references `vendor_reason_too_long` or `boundedDeclineReason`. |
| **F3** cascade ordering + sticky reference | **Half-fixed.** The sticky status/provider_reference and audit suppression work. The cascade guard is still defeated on the receipt path. | See R2 below. N7 (provider_reference no longer sticky) and N8 (`finalizeDeclined`'s no-audit guard removed) both **SURVIVED**. |
| **F4** payout-reference reversal | **Closed and pinned.** | The integrity check now runs before the tombstone branch. N9 (guard disabled) is KILLED by `TestRVLF_M4_ReversalNamingPayoutReferenceNeverTombstoned`, which asserts both the `ErrDepositReversalIntegrity` error and that zero tombstones are written. |
| **F5 / M10** exact cap | **Closed.** | N16 (`>=`) is KILLED by `TestPaymentWebhook_DeferredReceiptCapExceeded_ExactBoundary` (fills exactly `cap` rows, expects 200). |
| **F5 / M11** header allow-list | **Closed.** | N17 (`X-Receipt` header) is KILLED by the allow-list plus key-set equality check. |
| **F5 / M12, M13** reversal-mismatch HTTP | **Closed.** | N18 (`"error", err` on the log line) is KILLED at `payment_webhook_post_verification_400_integration_test.go:271`. N19 (branch removed, now returns 500) is KILLED at `:247`. The restored deposit-mismatch redaction loop is present. |
| **F5 / M4** (not claimed) | **Closed.** | N10 (reverse the intent's ledger transaction) is KILLED by `TestRVLF_P6_ReversalOfSecondCaptureReversesItsOwnTransaction`. |
| **F5 / M6b** (not claimed) | **Closed.** | N11 (disputed no longer projects to ambiguous) is KILLED by `TestRVLF_P7_ReplayOrderings` (`intent=declined`). |
| **F5 / M7** (not claimed) | **Still OPEN.** | N12 (projection recompute removed from `ApplyDeferredReceiptsForAttempt`) **SURVIVED**. `TestRVLF_P8` exercises the deferred path but never asserts intent status. |
| **F6** ADR §27.10 / evidence corrections | **NOT DONE**, as the implementer disclosed. | The §27.10 inaccuracies remain. Commit `d25a3fd`'s tests were labelled "NOT executed against a live database". They pass on my private DB, but three of them are vacuous for the property they claim to pin (see R2 and the F1 row). |
| **F7** dead/stale code | **Partial**, as disclosed. | The dead `ErrDepositIntentNotFound`→404 handler branch is removed, and the OpenAPI 400 text and simulation-handler rationale are corrected. Still left: `mapReceiveCallbackError`'s public-route `ErrDepositIntentNotFound` branch (dead); `orchestrator.go:1077` and `payment_deposit_simulation_handlers.go:172` still name the removed functions, as do three test comments; `setIntentAttempt` now carries two stacked, overlapping doc comments; the legacy `InitiateDeposit` chain is unchanged. |
| H2 / H4 (new code, reviewed incidentally) | H4 pinned. **H2 in-callback path unpinned.** | N13 (`rejectCreatedSiblings` removed from the receipt T13 branch) is KILLED by `TestRVLF_P3`. **N14 SURVIVED**: the new T2 `NOT EXISTS(succeeded sibling)` claim predicate is untested, because P3's child is already `rejected`, so the claim CAS fails on state alone. **N15 SURVIVED**: removing the new `ApplyDeferredReceiptsForAttempt` call from `ApplyReceiptEvidence` passes everything. |

### R1 (High, confirmed): F2 is not fixed. The receipt insert still stores the raw vendor reason, so it still hits the CHECK.

`boundedDeclineReason` is applied only where the reason is passed to `finalizeDeclined` and `ApplyDecline`.
`insertReceiptDeduped` still stores `ev.DeclineReason` verbatim. This fix round also moved that insert
to be the transaction's **first** write (R0), so it runs before any bounding.

I re-ran my original probe on `b3563c8`: a deposit `declined` callback with the same 83-byte issuer
text, plus a second one with an 80-byte, 40-rune multibyte reason. Both fail:

```
payments: insert receipt: ERROR: new row for relation "payment_provider_events" violates check
constraint "payment_provider_events_decline_reason_check" (SQLSTATE 23514)
```

The failure scenario is unchanged from F2: 500 → identical PSP redelivery → permanent loop. N6 surviving
shows there is no test of bounding on any path.

Two more points:

- Replacing the reason with a sentinel is defensible. But the raw text is then discarded entirely: it
  appears in no audit metadata and no redacted hash. Ops lose the vendor's reason with no record.
- Required fix:
  - bound `ev.DeclineReason` once, at the top of `receiveCallbackViaReceiptPath` or `ApplyReceiptEvidence`,
    before any fingerprint or insert;
  - add a test for an oversized reason on the receipt path, and one on each of the drive/sweeper paths.

### R2 (High, confirmed; ledger-finance to adjudicate): F3's cascade guard is still dead on the receipt path

`finalizeDeclined` now returns early when the sticky no-op fires. But on that early return it hands back
the caller's `intent` argument **unchanged**. In `applyResolvedReceiptEvidence` that argument is a stub,
`DepositIntent{ID, TenantID}`, so `updated.Status == ""`. `cascadeEligible(attempt, "", ...)` then
returns true, and `insertCascadeAttemptIfEligible` runs.

The same shape exists in `drive.go` and `sweeper.go`: there the argument is whatever `intent` was loaded
earlier, which can be stale if a sibling's T13 success committed in between.

Probe on `b3563c8`:

1. A1 declines (cascadable), which creates A2.
2. A2 is claimed and accepted while A1 is still declined.
3. A late T13 success lands on A1. `rejectCreatedSiblings` finds nothing, because A2 is already live.
4. A2 then declines (cascadable).

```
attempts=1:succeeded,2:declined,3:created intent.status=succeeded
intent.provider_reference==A1's ref: true   deposit.declined audits=1   cash=5000
```

The sticky reference and the audit suppression work: only A1's pre-success decline was audited. But
attempt 3 is still created for a credited deposit.

The new T2 predicate means A3 can never be claimed, so there is no second charge. From reading
`drive.go:133`, though, every sweeper tick will then fail A3's claim CAS and return the error. The
result is a permanent `created` orphan plus a recurring error on an intent that has succeeded. N14
survived, so that T2 backstop itself has no test either.

Required fix:

- in `finalizeDeclined` and `finalizeAmbiguous`, set `intent.Status = actual` before returning on the
  sticky path, or decide cascade eligibility from `actual` directly;
- add the probe above as a regression test.

`TestRVLF_F3_SucceededInputNeverRegressedByLaterDeclineOnAnotherSibling` does not pin F3. Its parent
attempt was synchronously declined at submission, so the late decline callback lands on an attempt that
is already `declined`. That hits `applyResolvedReceiptEvidence`'s terminal no-op before `setIntentAttempt`
or `finalizeDeclined` ever runs. The test would also pass on the pre-fix code, and N7 and N8 confirm it.
It needs a **live** sibling declining after the intent has succeeded, as in the probe above.

### Flags for ledger-finance (not ruled on here)

- **H1 (unconditional reversal-outcome normalization).** Every `deposit_reversal` that resolves a posted
  deposit posts a full-amount debit, whatever its wire `Outcome` value. That includes `pending` and
  `ambiguous`, which a real PSP could plausibly use for "chargeback opened / not yet final".
  - The wire value is not preserved anywhere after normalization. The receipt stores `succeeded`, and
    neither the `deposit.reversed` nor the `deposit.reversal_tombstoned` audit metadata records it.
  - The fingerprint also collapses deliveries that differ only in wire outcome.
  - If the reason-carrier reading is upheld, recommend recording the raw wire outcome in the audit
    metadata, and pinning the adapter contract (or rejecting `pending`/`ambiguous` for this event type).
- **M1 rewrite, two new silent cells.**
  - A **mismatched-amount success on a `declined` deposit attempt** returns `(false, anomaly_other)`. That
    is a resolved receipt with no dispute, no alert and no state change, even though money of a different
    amount may have been captured. The previous code attempted a dispute; it failed on the CAS, but it
    was not silent.
  - A **mismatched-amount success on a `succeeded` attempt** is classified `ResolutionApplied`, because the
    `AttemptSucceeded` case returns before the mismatch is checked. That mislabels the receipt, and the
    mismatch leaves no trace.
- **H2 stale attempt copy.** `ApplyReceiptEvidence` calls `ApplyDeferredReceiptsForAttempt(attempt)` with
  the pre-transition copy. The function returns 0 if `attempt.ProviderReference` is nil, which is exactly
  the T4-by-merchant-reference case H2 describes. This is latent today, because the callback path never
  populates `MerchantReference`, but it will matter when that is wired. N15 surviving shows the
  in-callback H2 call has no test.

### Required before this can be marked complete

1. Fix R1 (F2) at the receipt insert, with tests on all three paths so that N6 is killed.
2. Fix R2 (F3's stub/stale intent status), and replace the vacuous F3 test with the live-sibling probe so
   that N7, N8 and the R2 scenario are all killed. Add a T2 succeeded-sibling claim test to kill N14.
3. Pin F1's remaining halves (kill N1, N4b, N5b and N5c), M7 (kill N12) and in-callback H2 (kill N15).
4. Get ledger-finance rulings on the H1 and M1 flags above.
5. Complete F6. Finish F7, or record what remains as deliberate.

---

## Re-review 2: FH-5 callback security round at `0a96a01` (range `9dab8f3..0a96a01`, 2026-09-27)

- Commits in scope: `be15a11`, `bf5813f`, `7641332`, `dd44d04`, `0a96a01`. INV-DEP-1 is not in this range;
  it follows as FH-3.
- I read the source diff (`receipt.go`, `orchestrator.go`, `drive.go`, `sweeper.go`, `cascade.go`,
  `attempt.go`, `types.go`) and the new tests directly.
- Environment: a detached worktree at `0a96a01` and a private DB `rv_prhi1cb3_scratch`, built with
  `priv_db.sh`. All four test URLs pointed at that DB.
  - `deploy/init-app-role.sql` was **not** re-applied this round, because it contains a (guarded)
    `CREATE ROLE`. No role, password or shared-infrastructure change was made at any point.
- Baseline results:
  - `go test -tags=integration ./internal/payments/` passed (304 s).
  - The payments/webhook subset of `./internal/httpserver/` passed.
- Environment incident, not a code finding:
  - Partway through the mutation run, the shared host ran out of disk. Postgres went into crash
    recovery, and later the container restarted.
  - Every mutant whose run hit `No space left on device`, `recovery mode` or `Terminated` was discarded
    and re-run once access returned. The re-runs used the same private DB, with no credential or role
    changes. One killed run had left a mutated `receipt.go` in the worktree; it was reverted with
    `git checkout` before resuming.
  - Every verdict below comes from a clean run. `git status` was clean after every batch. The DB was
    dropped and the worktree removed afterwards.
  - The cluster now holds 143 `m0101%` scratch databases created by `depositV2ScratchPool` (99 before my
    runs). I cannot tell mine from other agents', so I did not drop any. These leftovers are a plausible
    contributor to the disk exhaustion, and whoever owns shared test infrastructure should look at them.
- Mutant method:
  - Byte-exact anchors, each verified to occur exactly once, with the file restored after each run.
  - First pass used `-run 'RVLF|Receipt|PayoutDispatch|PollPayout'`.
  - Every survivor was then re-run against the **full** `internal/payments` package, and still survived.

### Verdict: READY WITH CONDITIONS (the code findings are closed; test pinning gaps remain)

All previously blocking correctness items are now fixed, and each is pinned by a test that fails when the
fix is removed: R1/F2, R2/F3, F4, F1 (reversal, precondition-anomaly and cross-operation receipts), and
H4. S-M1 and S-H1's deferred-apply half are also pinned.

The conditions below are all test-pinning gaps on behaviours that are either correct today or reachable
only through a race or a future code path. None is a live correctness bug that I could reproduce.
Financial and security sign-off remain with `ledger-finance` and `security`.

### Previous items

| Item | Status | Evidence |
|---|---|---|
| **R1/F2** bounded decline reason, all 3 paths + multibyte | **CLOSED, pinned** | The bounding now runs once at the top of `ApplyReceiptEvidence`, before the receipt insert and before the branch to the reversal handler. `drive.go` and `sweeper.go` call `boundedDeclineReasonAudited`. My original probe was re-run with five deliveries: 83-byte ASCII, 80-byte/40-rune multibyte, unresolved (deferred), posting reversal and tombstone reversal. All five returned no error (4× `applied`, 1× `deferred_unresolved`) and wrote 5 redacted `payment.decline_reason_bounded` audits (length plus SHA-256 prefix only). Mutants: Q1 (receipt path) killed by `TestRVLF2_Q3`; Q2 (drive) killed by `TestRVLF_F2_DriveGo_*`; Q3 (sweeper) killed by `TestRVLF_F2_SweeperGo_*`; Q4 (redacted audit dropped) killed by `TestRVLF_F2_OversizeDeclineReasonBounded`. |
| **R2/F3** live-sibling probe replacing the vacuous test | **CLOSED, pinned** | `finalizeDeclined` and `finalizeAmbiguous` now set `intent.Status = actual` on the sticky path. My live-sibling probe (A1 declines, A2 claimed and pending, A1 late success, A2 cascadable decline) now ends with `1:succeeded,2:declined`, the intent `succeeded`, and no A3. The new `TestRVLF_F3_LiveSiblingDeclineAfterT13NeverCreatesOrphanCascade` really does drive a live A2, through `InitiateDepositAttempt`'s synchronous cascade. Mutants: Q5 (no `intent.Status = actual`) killed by `TestRVLF2_Q2`; N7 (provider_reference not sticky) killed by `TestRVLF_F3_LiveSibling...:661`; N8 (sticky guard removed) killed by `TestRVLF2_Q2`. |
| **N1** event_type filter | **CLOSED** | The filter is now an allow-list derived from `attempt.Operation`. N1 is killed by `TestRVLF_N1_*` and `TestRVLF_N3_*`. SH1b (the payout filter accepting deposit receipts) is killed by `TestRVLF_N3_DeferredApplyNeverReplaysADepositDeclineAsPayoutEvidence`. |
| **N4b** precondition anomaly resolved | **CLOSED** | Killed by `TestRVLF_N4b_*` (`:1344`). |
| **N5b** reference-conflict anomaly resolved | **SURVIVED (full package), effectively equivalent.** See C1. | — |
| **N5c** cross-operation anomaly resolved | **CLOSED** | Killed by `TestRVLF_N5c_*` (`:1425`). |
| **N12** (M7) deferred-path recompute | **SURVIVED (full package).** See C2. | — |
| **N14** T2 succeeded-sibling guard | **CLOSED** | Killed by `TestRVLF_F3_T2ClaimRefusesCreatedSiblingOfSucceededIntent`. |
| **N15** in-callback deferred apply | **SURVIVED (full package).** See C3. | — |
| **H2** post-transition attempt | **Fixed in code, unpinned.** | `ApplyReceiptEvidence` re-reads the attempt under lock before `ApplyDeferredReceiptsForAttempt`. Mutant H2r (re-read removed) **SURVIVED** the full package. See C3. |
| **F6** ADR / evidence corrections | **CLOSED** | ADR 0095 now carries an explicit "Corrections to §27.10" block. It retracts "no test was deleted, skipped, or weakened", corrects the §27.10(a) lock-ordering claim for payout references, discloses that the "6 scenarios" test covers only 3 distinct dispositions (and records that as still open), corrects the stale "migrations 1-104" citations, and relabels the ambiguous-callback adaptation as changed-intent. Original text is kept, and the corrections are appended rather than edited in. |
| **F7** dead and stale code | **Still partial** | (a) `mapReceiveCallbackError`'s public-route `ErrDepositIntentNotFound` → 404 branch (`payment_callback_errors.go:51-60`) is still present; nothing on the public route can produce it any more, and it would contradict the OpenAPI if reached. (b) `payment_deposit_simulation_handlers.go:172` still names `receiveDepositCallback`. `orchestrator.go:1088` also still names it, but that line describes history, which is acceptable. (c) `setIntentAttempt` still carries two stacked doc comments. (d) The legacy `InitiateDeposit` → `attemptDeposit` → `handleDecline`/`resolveAmbiguous` chain is **not** labelled test-only. Its doc comment still reads as a live entry point, even though it has no non-test caller. |

### S-H1 / S-M1 / H1 / M1 / N2 (security and ledger items in this range)

- **S-M1** (payout receipt reference mismatch disputes): **pinned.** SM1 is killed by
  `TestRVLF_SM10_PayoutSuccessProviderReferenceMismatchDisputes`, which asserts state `disputed`, a
  withdrawal that was not completed, and one audit record.
- **S-H1 deferred-apply half** (a deposit decline never replayed as payout evidence): **pinned** (SH1b
  killed, see above).
- **S-H1 event_type allow-list in `ApplyReceiptEvidence`**: **unpinned.** Mutant SH1a
  (`!known || op != …` changed to `known && op != …`, so an unknown event_type slips through)
  **SURVIVED** the full package. Today it is equivalent in production, because no caller produces an
  event type other than `deposit`/`deposit_reversal` (reversal diverts earlier), and `payout` receipts
  are only built in tests. It is still the stated S-H1 control. See C4.
- **H1 rule 2** (pending/ambiguous reversal never posts or tombstones): **pinned.** H1a is killed by
  `TestRVLF_P1_NonFinalReversalOutcomeNeverPosts`.
- **H1 rule 3** (raw wire outcome kept in the fingerprint): **unpinned.** H1b (`ev.RawOutcome` never set)
  **SURVIVED** the full package. See C5.
- **M1** (terminal amount/asset mismatch is audited): **pinned** for the `succeeded` cell (M1a killed).
- **N2** (a success after a tombstone disputes instead of hitting the unique-index error, in drive and
  sweeper): **pinned.** N2d and N2s are both killed.

### Conditions (test pinning; none is a reproduced live bug)

- **C1 (N5b, Low).** `TestRVLF_N5b_ReferenceConflictAnomalyReceiptResolved` does not exercise the
  branch it is named for. The evidence combines provider-ref A with merchant-ref B, so
  `ResolveAttemptForEvidence` already returns `Anomaly/ReferenceConflict`, and the precondition-anomaly
  branch (N4b's) resolves the receipt. The precondition-2 block (`receipt.go` around line 510, "provider
  reference already bound to a DIFFERENT attempt") can only be reached in a READ COMMITTED race: the
  reference becomes bound between the two reads. Either delete it and rely on the resolver, or cover it
  with a deliberately interleaved test. Rename the existing test either way.
- **C2 (M7/N12, Low).** Nothing asserts the intent-status projection after
  `ApplyDeferredReceiptsForAttempt` applies a state-changing receipt (ambiguous or dispute).
  `TestRVLF_P8` asserts only the success path, where `postDepositSuccess` writes the status directly.
  Add a deferred ambiguous- or mismatch-evidence case that asserts `deposit_intents.status`.
- **C3 (H2/N15/H2r, Low).** The in-callback `ApplyDeferredReceiptsForAttempt` call and its
  post-transition re-read are unpinned. The only path that makes them matter is a receipt resolved by
  merchant reference that performs T4. The live callback path never sets `MerchantReference`, so this
  is latent in production, but `ApplyReceiptEvidence` is exported and tests drive it that way.
  Suggested test: a submitting attempt, a stored deferred success for reference R, then
  `ApplyReceiptEvidence{MerchantReference: attempt, ProviderReference: R, Outcome: pending}`. Assert the
  deferred success was applied. That kills both N15 and H2r.
- **C4 (S-H1 allow-list/SH1a, Low; for `security` to confirm).** Add one test that sends an unrecognized
  `EventType` (for example `payout_returned`) through `ApplyReceiptEvidence` against a resolvable
  attempt, and asserts `DispositionAnomaly`, no state change, and a resolved receipt.
- **C5 (H1 rule 3/H1b, Low; for `ledger-finance` to confirm).** Add a test that delivers the same
  reversal twice with different final wire outcomes (`succeeded`, then `declined`-as-carrier). Assert two
  distinct receipt rows, exactly one ledger reversal, and both wire outcomes present in audit metadata.
- **C6 (F7 remainder, Low).** Remove the dead public-route `ErrDepositIntentNotFound` mapper branch, fix
  the stale simulation-handler comment, merge the duplicated `setIntentAttempt` doc comment, and label the
  legacy `InitiateDeposit` chain test-support-only (or retire it).

Observations, not conditions:

- `boundedDeclineReasonAudited` on the receipt path writes one audit per delivery, including exact
  duplicates. That is acceptable (append-only and redacted), but noisy under PSP redelivery storms.
- Collapsing every oversized reason to one sentinel means two deliveries that differ only in their long
  reason text produce the same fingerprint. That is acceptable, because the reason is not an effect-bearing
  field.
