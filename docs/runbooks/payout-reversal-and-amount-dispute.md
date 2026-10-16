# Runbook: governed payout reversal and payout amount/asset dispute (A5, migrations 0162 and 0163)

Status: written in A13 against the code at the A5 merge. `IMPLEMENTED` against the local database and MOCK providers; behaviour with a real
PSP is `PROVIDER DEPENDENT`. A provider-initiated "payout returned" event does not exist; staff record the return. Open items from the A5
record that an operator must know: the reverse decisions are NOT covered by the ADR 0110 signed actor proof (residual R1, security HIGH
before real money); a partial or different-asset return is refused (`409`), not handled.

## 1. When to use which procedure

| Situation | Procedure |
|---|---|
| The platform declared a payout paid (withdrawal `completed`) and the PSP returned the whole amount (money is back at `psp_clearing`) | **Section 2: governed payout reversal** (compensating `withdrawal_reversed` posting, E11 `completed -> reversed`) |
| The PSP returned the whole payout BEFORE the platform saw it succeed (withdrawal still `submitted`, an attempt open) | Section 2, tombstone branch (needs `payment_force_resolve:approve`) |
| A payout is parked `disputed` for `amount_asset_mismatch` / `callback_amount_asset_mismatch` and the provider has confirmed out of band what was paid | **Section 3: M2 with basis `provider_confirmed_out_of_band`** |
| Same parks, provider returned the whole payout | Section 2 tombstone branch |
| Partial or different-asset payout/return | **No exit.** It stays parked with its hold, P1 alert and audit. Owner design needed (A5 record 2c, 1h). Do not edit rows |
| Player wallet negative after a chargeback/reversal | Section 4 |

## 2. Governed payout reversal

`POST /v1/admin/withdrawals/{id}/payout-reversal` records ONE staff decision. Tenant-scoped finance staff with `withdrawal:approve`,
step-up **critical**. The decider is the authenticated caller; the reversal executes in the call that completes the quorum:
`max(2, withdrawal policy approvals)` distinct **Persons** at any amount (the database floor is 2), all agreeing on the same payload
digest. The beneficiary (the player's own Person) cannot decide; every approver needs a confirmed Person link and an active account.

Body (strict, every approver sends the identical payload):

```json
{"provider_id": "...", "original_provider_reference": "...", "return_reference": "...",
 "amount": "<decimal string of minor units, positive>", "asset_code": "...", "evidence_ref_hash": "<hex SHA-256 of the external evidence reference, never the reference>"}
```

Steps:
1. Establish with the PSP, out of band, that the whole payout came back, and obtain its return reference. Hash your evidence reference.
2. Approver 1 calls the route. Response `{"outcome":"decision_recorded","counted_approvals":1,"required_approvals":2}`.
3. A different Person (finance) calls the route with the same body. Outcome `reversed` and `ledger_transaction_id` (Dr `psp_clearing`, Cr the
   wallet's `player_cash`, key `<id>:reversed`), or `tombstoned_hold_released` (submitted branch), or `already_executed` (idempotent replay).
4. Verify: `GET` the withdrawal shows `reversed`; the next reconciliation cycle's check D has no `WD033`/`WD035`/`WD044` finding for it; the
   audit log has `withdrawal.payout_reversal.http` rows (IP, user agent, request id, payload digest).

Refusals (all `409` unless noted): payload does not match the payout (provider, reference, amount, asset); return reference already held by
the platform for that provider; already reversed; an attempt in flight (`created`/`submitting`, retry later); inconsistent attempts (engineering);
withdrawal not `completed` or `submitted`; you already decided; `403` for self-benefit, no Person link, inactive account, or a submitted-payout
reversal without `payment_force_resolve:approve`; `403 step_up_required`.

Rollback: none. A reversal is a ledger fact; the only correction is another governed, compensating action. The migration 0162 down
refuses (`WD097`) while a reversed request exists.

## 3. Amount/asset mismatch park: governed M2 exit

The platform parks any payout whose provider evidence carries a different amount or asset (callback or status query), keeps the hold,
raises a P1 and audits. Since migration 0163 the ONLY staff exit besides the reversal is the M2 force-resolution
(`operational-runbooks.md` section 14, routes `/v1/admin/tenants/{tid}/payment-force-resolutions`) with `basis_code`
`provider_confirmed_out_of_band` for BOTH reasons; `reconciliation_exhausted` is not accepted against contradicting evidence.
Everything else in section 14 applies (re-verify with the provider first, evidence hash, a statement source must be registered for the
provider, four-eyes, grants). Execution re-checks the basis rule; the database is the authority (`MR010`).

## 4. Negative balance after a reversal or chargeback (owner decision 13: block until cleared)

A wallet whose `player_cash` or `player_locked_cash` projection is negative is **blocked**: casino/sportsbook wagers, casino launch and
bootstrap, bonus eligibility, and withdrawals (WD050). Deposits, wins, rollbacks, settlements and voids are never blocked. The block is
derived: it lifts as soon as the balance is >= 0 by a governed K2 credit (`operational-runbooks.md` section 11) or by the player's own
deposit. A sticky block that survives a deposit is NOT built (owner question Q2). Never "fix" a balance by hand.

## 5. Alerts and monitoring

The tombstone branch raises a P1 last in its transaction. Check D (reconciliation) surfaces reversed-arm and orphan `withdrawal_reversed`
findings. Delivery of any alert to a person depends on `alert-delivery-channels.md` (a route must exist; email is MOCK). Payout-dispute
and T14 specific alerts remain `NOT IMPLEMENTED` (PAY-PAYOUT-DISPUTE-ALERT-1).
