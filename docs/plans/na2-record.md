# NA-2 record: PAY-DEPOSIT-MISMATCH-ALERT-1 (raise only)

Status: IMPLEMENTED (code + tests; no provider dependency). No migration, no new alert Kind, no ADR edit.
Branch na2-deposit-mismatch-alert (from 76187cc).

## What was found
`receipt.go` (ApplyReceiptEvidence, callback success cells) already detects `mismatched := attempt.Amount != ev.Amount || attempt.AssetCode != ev.AssetCode`
on an already-SUCCEEDED and an already-DECLINED attempt and writes the `payments.callback_amount_asset_mismatch_terminal` audit row (once per new
receipt), then called `raisePayoutDisputeAlert`, which is a no-op for deposits. So a DECLINED deposit that the provider actually captured
(capture without credit) paged nobody. The pin test `deposit_mismatch_pin_integration_test.go` asserted exactly that.

## Decisions
- Two new closed reasons, own closed set `depositSignalReasons` in `internal/payments/alerts.go` (not terminal dispute reasons, not escalations;
  `alertReasonFor` / `DepositDisputeTerminalReasons` untouched):
  - `mismatched_success_on_declined_deposit` (money captured without credit)
  - `mismatched_success_on_succeeded_deposit` (conflicting evidence on a credited deposit)
- `raiseDepositSignalAlert` raises the existing P1 Kind `payment.webhook_integrity` (platform-owned, no migration) with discriminator
  `attempt:<attempt_id>:reason:<reason>` (same shape as the other deposit raises), attributes `provider_id` only. A repeat is one open alert with
  growing occurrences. Unknown reason maps to `unclassified`, never echoed. Payout attempts are a no-op there (payouts keep R-5/R-6).
- Placement: the LAST statement of each receipt cell, after the audit row, inside the alerting.InTx owner (webhook handler / sweeper / drive) whose
  Pending is flushed after commit; RaiseGuarded swallows only deterministic alert failures, transient ones roll the delivery back (redelivery retries).
  Same as R-5/R-6. No state change, no ledger effect, no audit change.
- Scope: amount/asset mismatch on the callback (receipt) path, the case named by the gate. The poll path already raises
  `poll_evidence_contradicts_terminal_attempt:*` for terminal contradictions (unchanged). A SUCCEEDED deposit success with the same amount but a different
  provider reference is NOT part of this item (the payout twin PAY-PAYOUT-SUCCEEDED-REF-MISMATCH-1 is payout-only); unchanged.

## Deliberate change to an existing test
`internal/payments/deposit_mismatch_pin_integration_test.go`: the old test
`TestDepositMismatchedSuccess_OnSucceededAndDeclined_AuditOnly_NoAlert_NoStateChange` asserted `len(alertinject.ForSubject) == 0`. That single assertion
was intentionally replaced (behaviour intentionally changed) by: exactly ONE open p1 payment.webhook_integrity row with the exact discriminator and
provider_id-only attributes. All other old assertions (state unchanged, terminal reason nil, one terminal-mismatch audit row, ledger balanced) are kept,
plus a ledger-transaction-count-unchanged check. The test was renamed `..._AuditAndOneP1_NoStateChange`; file header documents it.

## Tests added
payments (integration): ...AuditAndOneP1_NoStateChange (succeeded/declined; normal path raises nothing first), Replays_OneAlertRow_OccurrencesGrow (duplicates,
idempotent discriminator), TestDepositBenignDuplicates_RaiseNoAlert, Concurrent_OneAlertRow (6 parallel deliveries, one row, 6 occurrences),
TransientRaiseFailure_RollsBackDelivery and DeterministicRaiseFailure_DetachedRetryRaises (retry), alerts_test.go TestDepositSignalReasons_ClosedSet (unit).
httpserver (integration): `pay_deposit_mismatch_alert_integration_test.go` real webhook handler, declined and succeeded deposits: uniform 200, no state change,
no posting, one P1, provider_id-only attributes, redelivery stays one row.

## Residuals
- Alert routing/delivery remains ALERT-DELIVERY-1 (production blocker, unchanged); this raises the durable row only.
- A dedicated Kind (ALERT-KINDS-DEDICATED-1) remains class B.
- Review chain still owed: ledger-finance (code path next to ledger posting cells; this adds no posting) and security (alert content). Orchestrator to request.

## Proposed registry text (orchestrator applies)
PAY-DEPOSIT-MISMATCH-ALERT-1: CLOSED (IMPLEMENTED). A provider success with mismatched amount/asset on a DECLINED deposit raises P1 payment.webhook_integrity
`mismatched_success_on_declined_deposit`; on a SUCCEEDED deposit `mismatched_success_on_succeeded_deposit`; raise only, no state/ledger effect, no migration.
ADR 0095 s42.8 hard gate satisfied for this item.

## Other existing tests changed deliberately (behaviour intentionally changed; none weakened)
- `internal/payments/payout_callback_audit2_integration_test.go` TestCallbackAudit2_Deposit_OrphanedReceipt_Redelivered_AuditedOnce_Resolved: the assertion
  "a deposit raises no alert today" (0 rows) became "exactly one alert with the mismatched_success_on_succeeded_deposit discriminator". Audit-once, receipt
  resolution, state and ledger assertions unchanged.
- `internal/payments/iwire_lf9_integration_test.go` (LF9 race, succeeded branch): "no park alert" now permits exactly the one new
  mismatched_success_on_succeeded_deposit signal row (a losing contradicting success landing after the credited success) and still fails on any other alert.
  The disputed branch is unchanged.
- `internal/alerting/static_wiring_test.go`, `static_derived_raise_set_test.go`: the reviewed-helper lists gained `raiseDepositSignalAlert` (called only from the
  receipt cells, themselves already in the reviewed set); the B-1 / CR2-S4 / LF-F4 guards all stay in force and pass.

## Verification
Private DB per run; all integration commands `-tags integration -count=1 -p 1`.
- gofmt clean; go vet plain and -tags integration clean on internal/payments, internal/alerting, internal/httpserver.
- payments `-run 'Callback|Receipt|Poll|Alert|Mismatch|Deposit'` (-timeout 50m): first run had exactly 2 failures (the two tests above, which asserted the old no-alert behaviour); after the
  deliberate updates those two plus the new tests pass (-count=3: ok).
- payments new/related subset (DepositMismatched*, DepositBenign*, DepositSignal*, R5_*, R6_*): all PASS (incl. payout R-5/R-6 unchanged).
- internal/alerting full: first run failed only the two static wiring guards (new helper unlisted); after listing it, `-run Static` ok. (Full alerting package ~250s, all other tests passed.)
- internal/httpserver `-run 'Webhook|IWire|Deposit'`: ok, including the new TestWebhook_DepositMismatchedSuccess_DeclinedAndSucceeded_RaiseOneP1.
