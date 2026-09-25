# Stage 10 W1 item 0: F-7 `ledger.Post` replay audit

| Field | Value |
|---|---|
| Owner | `ledger-finance` |
| Audited commit | `94bc863b8ae461eaa3f56410efe5b3c01effc570` (HEAD, "Stage 10 W0 complete") |
| Method | ADR 0088 §11.1 / §11.2 (ACCEPTED) |
| Status of this record | Audit IMPLEMENTED (read-only; §1-§6 describe HEAD 94bc863 and are unchanged). The §6 fix was subsequently implemented as the separate F-7 remediation item - see §7 Outcome. |
| Date | 2026-09-25 |

**Scope note.** When this audit ran, the working tree had uncommitted W1 changes:
`internal/ledger/ledger.go` (new `TxSportsbook{Settlement,Void,Rollback}` consts),
`internal/ledger/lockorder.go` (`LockProjectionsForPostings`),
`migrations/0091_*` and `.github/workflows/ci.yml`. This audit covers **HEAD only**. The
`Post` body at HEAD is byte-identical to the working tree; the only differences are the
new consts and the new pre-lock function. Evidence tests were re-run against a clean
`git archive 94bc863` export, not against the dirty tree. W1's own settlement, rollback
and void call sites are not in this audit. When they land they must be added to §3 (they
are designed to be class A under ADR 0088 §4.2 and §4.3).

---

## 1. The defect (F-7), confirmed at HEAD

`internal/ledger/ledger.go:316-325` (HEAD):

```go
if conflict {
    existingID, existingType, lookupErr := lookupByIdempotencyKey(ctx, tx, in.TenantID, in.IdempotencyKey)
    ...
    if existingType != in.TransactionType { return ..., ErrIdempotencyKeyReused ... }
    return PostResult{TransactionID: existingID, AlreadyPosted: true}, nil
```

On a key conflict, only `transaction_type` is compared. Everything else is not compared:
`Entries` (accounts, directions, amounts), `BonusCost` (and therefore the generated Rule
B2 legs), `CorrelationID`, `ReversesTransactionID`, `ProviderID`/`ProviderTxID`,
`ReasonCode` and `CausationID`. The call returns the original transaction as a no-op.

Two things mislead readers about this:
- The doc comment on `ErrIdempotencyKeyReused` (`ledger.go:163-168`) says it guards "a
  same-key-different-payload replay". It only guards a different *type*.
- `TestPost_SameKeyDifferentPayloadRejected` (`internal/ledger/ledger_integration_test.go:226`)
  only varies the type.

`TestIntegration_ReplayWithChangedAmountAssetOrPlayerIsWhyTheContractMatters`
(`internal/idempotency/integration_test.go:376`) **pins F-7 as current behaviour**. It asserts
that a changed amount (100 to 999) and a changed wallet both return `AlreadyPosted` with
the original id. The test passed at HEAD.

A secondary observation (fail-closed, not F-7): `db.IdempotentInsert` treats *any* unique
violation as a conflict. That includes `idx_ledger_transactions_tenant_provider_tx`. If
the key is new but `(provider_id, provider_tx_id)` is taken (for example, a tombstoned
reference), `lookupByIdempotencyKey` gets `pgx.ErrNoRows`. `Post` then returns an untyped
wrapped error. This is fail-closed and correct, but the error is not typed.

## 2. Call-site inventory

`rg 'ledger\.Post\(ctx'` over `internal/` and `cmd/` at HEAD, excluding `_test.go`, finds
**21 direct production call sites**. This matches ADR 0088 §11.1's list exactly.

Indirect callers:
- No wrapper takes a `TransactionInput` and forwards it to `Post`.
- `Post` is never used as a function value (`rg 'ledger\.Post[^(]'` finds only comments
  and tests).
- No interface-typed `Post` exists.
- `internal/idempotency` (`ResolveOccurrence`/`ComposeOccurrenceKey`) is not used by any
  production caller.

The helper functions that contain call sites are counted once, at their `Post` line:
`postRollbackTombstone`, `postDepositReversalTombstone`, `terminalWriteDown`,
`applyGrantActivation`, `applyGrantConversion`, `ResolveHeldDisposition` and the
`postWin*` family. Their upstream reachability is recorded per row. `ledger.go` and
`lockorder.go` are the only files that INSERT into `ledger_transactions` or
`ledger_entries`.

**Manual adjustment:** at HEAD there is **no** production path that posts
`TxManualAdjustment`. It is only referenced in `ledger.go`/`lockorder.go` validation and in
the comment at `reconciliation.go:228`. NOT IMPLEMENTED, so there is nothing to audit.
Whatever path is built for it later must be class B or better from day one.

Test-only callers (18 `_test.go` files, including `forceActivateGrantForTest` and
`forceConvertGrantForTest`) are out of scope. They are not compiled into the binary.

## 3. Per-site audit (HEAD 94bc863)

Legend:
- **A**: the key determines the payload.
- **A-sg**: A by state gate. The payload is state-derived, not key-derived, but a locked
  status check (CAS) makes a second `Post` with the same key unreachable. The first
  committed payload is therefore the only one that can ever exist for that key.
- **B**: the caller compares the full payload before trusting `AlreadyPosted`.
- **C**: exposed. A different-payload replay is reachable and silently returns the
  original.
- **C-latent**: C by code, but no producer of the precondition exists at HEAD.
- **Reserved?**: can a client or provider produce a key starting with
  `sportsbook_settlement:`, `sportsbook_rollback:` or `sportsbook_void:`?

| # | Site (HEAD) | Domain / type | Key composition | Client/provider-influenced inputs | Same key, different payload reachable? | Caller-side comparison | Reserved? | Class |
|---|---|---|---|---|---|---|---|---|
| 1 | `withdrawal/withdrawal.go:379` | withdrawal / `withdrawal_requested` | `requestID.String()` (fresh server `uuid.New()`) | amount/asset from player body (keyed at the `withdrawal_requests` layer) | No. The key is fresh per row. A retry conflicts on `withdrawal_requests` before `Post` | Row compare, wallet/asset/amount, `:325` | No (bare UUID) | A + B |
| 2 | `withdrawal.go:797` (Reject) | `withdrawal_rejected` | `requestID + ":rejected"` | none (amount from locked row) | No. `FOR UPDATE` plus `State == pending_review` gate (`:746`); state changes in the same tx | state gate | No | A-sg |
| 3 | `withdrawal.go:1075` (Complete) | `withdrawal_completed` | `providerID + ":" + providerTxID` | `providerTxID` = `result.ProviderReference` / `*wr.ProviderReference` | No. The ref is unique per request (`idx_withdrawal_requests_tenant_provider_ref`, mig 0026) and the `submitted` gate is at `:1060` | state gate + DB unique ref; resolve path compares provider amount/asset (`withdrawal_handlers.go:1066`) | No (providerID ∈ registry, see §4) | A-sg |
| 4 | `withdrawal.go:1179` (Fail) | `withdrawal_failed` | `requestID + ":failed"` | none | No (`submitted` gate `:1150`) | state gate | No | A-sg |
| 5 | `withdrawal.go:1264` (Cancel) | `withdrawal_rejected` | `requestID + ":cancelled"` | none | No (`requested` gate `:1243`) | state gate | No | A-sg |
| 6 | `casino/orchestrator.go:987` (postBet) | casino / `casino_bet` | `providerID + ":" + ProviderTxID` | provider (signed webhook) or player (sim route: `sha256(session:wager:idempotency_key)`); amount, round, session from payload | **Yes, but `Post` is never reached.** The short-circuit at `:745` returns the original **without comparing** amount, round or session. Concurrent duplicates are serialized by an advisory lock, so they also hit the short-circuit | **None** | No | **C (caller-level)** |
| 7 | `casino/orchestrator.go:1312` (postRollback, generic) | `casino_rollback` | `providerID + ":" + ProviderTxID` (rollback ref) | provider: rollback ref and `OriginalProviderTxID` | **Yes.** A redelivered rollback ref R1 that names a *different*, not-yet-reversed original B2 passes the `:1231` check (it only looks at B2's reversals), reaches `Post`, and gets back R1's reversal of B1 | Only `existingReversalProviderTxID` for the *named* original | No | **C** |
| 8 | `casino/orchestrator.go:1366` (tombstone) | `tombstone` | `"tombstone:" + providerID + ":" + OriginalProviderTxID` | provider ref | No. Payload is empty entries plus the provider ref from the key. `CorrelationID: uuid.New()` differs on every call (§6.3) | n/a | No | A |
| 9 | `casino/bonus_settlement.go:340` (postWinDirectCash) | `casino_win` | `providerID + ":" + ProviderTxID` | provider or player-sim: amount, round | **Yes.** Same win ref with a different amount (or a different round on the same wallet) reaches `Post` and returns the original. This is the **only production-reachable win path** | None (asset cross-check only) | No | **C** |
| 10 | `bonus_settlement.go:386` (postWinLockedCash) | `casino_win` | same | same | Only if a replay names a different round with a live lock. The same-round replay aborts earlier (`ErrLockAlreadyReleased`) | None | No | C-latent (no casino locked-cash producer at HEAD) |
| 11 | `bonus_settlement.go:477` (locked bonus, non-terminal) | `casino_win` + BonusCost | same | same | as #10 | None | No | C-latent |
| 12 | `bonus_settlement.go:527` (terminal hold-capture) | `casino_win` + BonusCost | same | same | as #10 | None | No | C-latent |
| 13 | `bonus_settlement.go:702` (postRollbackHeldWin) | `casino_rollback` + BonusCost | `providerID + ":" + ProviderTxID` | provider | A reused rollback ref naming a different *held* win reaches `Post`, gets `AlreadyPosted`, skips the void, and reports success | Voided-status ref compare covers the same disposition only | No | C-latent (no held dispositions reachable at HEAD) |
| 14 | `bonus/lifecycle.go:458` (applyGrantActivation) | bonus / `bonus_grant` | `"bonus_grant:" + grantID` | amount from `ActivateGrantParams` (staff/rule) | No. Advisory lock + `FOR UPDATE` + `issued` gate (`:362`) | state gate | No | A-sg |
| 15 | `bonus/lifecycle.go:753` (terminalWriteDown) | `bonus_forfeiture` | `"bonus_forfeiture:" + grantID + ":" + reasonCode` | amount from live outstanding balance | No. The open-status gate at `:804` runs under the grant lock and moves the grant to terminal or pending_settlement in the same tx | state gate | No | A-sg |
| 16 | `bonus/held_disposition_ops.go:497` | `bonus_forfeiture` / `bonus_conversion` | `"bonus_held_disposition_resolve:" + dispositionID` | action/reason from staff (four-eyes) | No. `held` gate under lock (`:309`). The two actions use different types, so a mismatch is rejected as `ErrIdempotencyKeyReused` anyway | state gate + type | No | A-sg |
| 17 | `bonus/conversion.go:228` | `bonus_conversion` | `"bonus_conversion:" + grantID` | none (live remaining balance, capped) | No (`completed` gate `:105` under lock) | state gate | No | A-sg |
| 18 | `sportsbook/orchestrator.go:428` (PlaceBet) | sportsbook / `sportsbook_bet` | `"sportsbook_bet:" + playerID + ":" + clientKey` (`:379`) | player: key, selection, stake, asset, odds | Reachable only in the concurrent race (both calls miss `findBet`, then serialize at L3). `Post` absorbs the loser, then `insertBet` re-reads the winner and compares | `findBetByIdempotencyKey` compare (`:152`), `insertBet` conflict compare, `LedgerTransactionID` equality (`:468`) | No. The fixed prefix is `sportsbook_bet:` and the client string is a suffix after the player UUID | B |
| 19 | `payments/orchestrator.go:730` (postDepositSuccess) | payments / `deposit` | `providerID + ":" + providerReference` | provider: ref, amount, asset | No. The ref maps to exactly one intent (`idx_deposit_intents_tenant_provider_ref`, mig 0025). Amount and asset are compared to the intent (`:708`). There is a succeeded short-circuit (`:705`). Correlation = `intent.ID` | intent compare | No | B (+A via unique ref) |
| 20 | `payments/orchestrator.go:1000` (deposit reversal) | `deposit_reversal` | `providerID + ":" + reversalRef` (`:1003`) | provider: reversal ref, original ref, amount | **Yes.** A reversal ref R1 redelivered naming a *different*, unreversed deposit D2 passes the `EXISTS` check (`:978`, D2 only), reaches `Post`, returns R1's reversal of D1, and reports D2 reversed | Amount/asset vs D2 (`:959-966`) and already-reversed(D2) | No | **C** |
| 21 | `payments/orchestrator.go:1038` (reversal tombstone) | `tombstone` | `"tombstone:" + providerID + ":" + originalRef` | provider ref | No. `CorrelationID: uuid.New()` (`:1042`) differs on every call | n/a | No | A |

### Evidence excerpts (HEAD)

Site 6, the short-circuit with no payload compare (`casino/orchestrator.go:745-748`):
```go
if existingID, found, err := findPostedBetTransaction(ctx, tx, tenantID, providerID, event.ProviderTxID); err != nil {
    return ReceiveCallbackResult{}, err
} else if found {
    return ReceiveCallbackResult{Outcome: OutcomeSucceeded, LedgerTransactionID: &existingID}, nil
```

Site 7, the only guard is per named original (`casino/orchestrator.go:1227-1232`):
```go
`SELECT provider_tx_id FROM ledger_transactions WHERE reverses_transaction_id = $1`, originalID,
...
if existingReversalProviderTxID != nil && *existingReversalProviderTxID != event.ProviderTxID {
    return ReceiveCallbackResult{}, ErrAlreadyRolledBack
```

Site 9, the amount comes straight from the payload and is keyed only on the provider ref (`casino/bonus_settlement.go:340-347`):
```go
IdempotencyKey: providerID + ":" + event.ProviderTxID,
...
{LedgerAccountID: houseAccountID, Direction: ledger.Debit, Amount: event.Amount},
{LedgerAccountID: cashAccountID, Direction: ledger.Credit, Amount: event.Amount},
```

Site 20, the already-reversed check is scoped to the named original (`payments/orchestrator.go:976-980`):
```go
`SELECT EXISTS (SELECT 1 FROM ledger_transactions WHERE reverses_transaction_id = $1)`,
original.LedgerTransactionID,
```

Site 19, the B-compare (`payments/orchestrator.go:705-710`):
```go
if intent.Status == DepositIntentSucceeded { return intent, nil }
if amount != intent.Amount || assetCode != intent.AssetCode {
    return intent, fmt.Errorf("%w: ...", ErrCallbackProviderMismatch, ...)
```

Site 18, the B-compare (`sportsbook/orchestrator.go:152-154`):
```go
if existing.SelectionID != params.SelectionID || existing.StakeAmount != params.StakeAmount || existing.AssetCode != params.AssetCode ||
    existing.OddsNumerator != params.ExpectedOddsNumerator || existing.OddsDenominator != params.ExpectedOddsDenominator {
    return PlaceBetResult{}, fmt.Errorf("%w: existing bet %s", ErrBetIdempotencyKeyReused, existing.ID)
```

### Named evidence tests (run against clean HEAD export, all PASS)

- Ledger F-7 pinned: `TestIntegration_ReplayWithChangedAmountAssetOrPlayerIsWhyTheContractMatters`; type-only: `TestPost_SameKeyDifferentPayloadRejected`.
- Casino (6-9): `TestReceiveCallback_RedeliveredBetWinRollbackAreIdempotent`, `TestReceiveCallback_RollbackOfNeverSeenOriginalWritesTombstone`.
- Payments (19-21): `TestReceiveCallback_RedeliveredSuccessIsIdempotent`, `TestReceiveCallback_SecondReversalOfSameDepositRejected`, `TestReceiveCallback_ReversalOfNeverPostedDepositWritesTombstone`, `TestReceiveCallback_ReversalAmountCannotExceedOriginal`.
- Sportsbook (18): `TestPlaceBet_IdempotentRetrySameKeyOneEffect`.
- Withdrawal (1): `TestRequestWithdrawal_IsIdempotentOnRetry`.

**Evidence gaps.** ADR §11.1 item 5 requires a named test per classification. None exists yet for:
- the C scenarios: 6 (bet, same ref, different amount), 7 (rollback ref reused for a different original), 9 (win ref reused with a different amount) and 20 (reversal ref reused for a different deposit);
- site 18's different-stake same-key rejection (`ErrBetIdempotencyKeyReused` has no test);
- site 3's cross-request ref reuse.

They are listed as required in §6.4. They were not written here because this audit is read-only.

## 4. Reserved-namespace check (ADR 0088 §4.2)

**Result: no existing caller can produce a key beginning with `sportsbook_settlement:`,
`sportsbook_rollback:` or `sportsbook_void:`.**

Every key at HEAD starts with one of these server-fixed forms:
- a bare UUID (#1);
- `<uuid>:` (#2, #4, #5);
- `tombstone:` (#8, #21);
- `bonus_grant:`, `bonus_forfeiture:`, `bonus_held_disposition_resolve:`, `bonus_conversion:` (#14-17);
- `sportsbook_bet:` (#18);
- `<providerID>:` (#3, #6, #7, #9-13, #19, #20).

`providerID` is always a key of a compile-time adapter registry:
- `cmd/platform-api/main.go:141` `"mock-payments"`, `:155` `"mock-casino"`;
- both `ReceiveCallback`s reject unknown ids (`ErrUnknownProvider`);
- `RouteProvider` requires `o.providers[candidate.ProviderID]` (`payments/orchestrator.go:170`);
- withdrawal `*wr.ProviderID` is copied from that routed capability.

Client-chosen strings only ever appear as suffixes (#18 after the player UUID, #6/#9
through the sim route after `mock-casino:sim-`).

Residual risk: registering an adapter whose id is `sportsbook_settlement`,
`sportsbook_rollback` or `sportsbook_void` would open the namespace. Recommended guard
(cheap, optional): at registry construction, reject provider ids equal to any reserved
prefix stem.

## 5. Conclusions

### 5.1 Is F-7 an exploitable defect in an existing domain today?

**Players and external attackers cannot use it to extract value, in any domain.** Absorbing
a replay always keeps the *first* committed posting. Nothing is ever double-credited or
over-credited. `SUM(DEBITS) == SUM(CREDITS)` and projection/ledger agreement are
unaffected.

**It is still a real, reachable silent-divergence defect (class C) in three production
paths, plus a caller-level analogue in a fourth.** In each case the trigger is a
provider-originated callback that reuses a provider transaction reference with a
different payload: a provider defect, or a compromised provider signing key.

- **Casino win (#9).** A win ref reused with a different amount or round returns success.
  Only the first amount is credited. The provider believes the second amount was paid.
  The result is player-facing underpayment and provider/ledger drift. The audit record
  (`casino_win.posted`) logs the *new* amount with `already_posted: true`, which makes
  the audit trail misleading.
- **Casino rollback (#7).** A rollback ref reused to name a different original bet
  returns success, but the named bet is **not** refunded. The player is underpaid versus
  the provider's view. The audit `casino_bet.rolled_back` names the wrong
  `original_transaction_id`.
- **Deposit reversal (#20).** A reversal ref reused to name a different deposit returns
  success (`DepositIntentID = D2`), but D2 is **not** debited. The operator absorbs the
  chargeback until psp_clearing reconciliation against the PSP statement finds it. That
  statement reconciliation is MOCK/NOT IMPLEMENTED at HEAD.
- **Casino bet (#6), caller-level, not `Post`'s branch.** A bet ref reused with a
  different amount, round or session gets back the original "succeeded" result. A
  ledger-level fix does **not** cover this path, because `Post` is never reached.

Reachability today:
- All providers are mocks. No real provider contract exists.
- The player-facing simulation routes are gated behind `CasinoPlaySimulationEnabled` and
  `PaymentsMockSettlementEnabled`, which are non-production. The wager/win routes let a
  player replay their *own* key with a different amount. The only effect is that the
  first result is returned, so there is no gain. The rollback route mints a fresh ref per
  call, so it cannot reach #7.

Severity: an integrity and reconciliation defect, not a value-extraction exploit.

These sites are **not exposed** (A, A-sg or B): withdrawals (all 5), bonus (all 4),
deposit success, both tombstones, and sportsbook placement. The locked-origin and
held-win casino paths (#10-13) are C-latent: C by code, with no producer at HEAD. They
become C the moment bonus-funded or locked casino bets ship.

### 5.2 Consequence under ADR 0088 §11.2

At least one site is class C. Therefore:
- (a) The Orchestrator must open **F-7 remediation as its own item**, with its own review
  (`ledger-finance` + `security` + `code-reviewer`). It is **not** folded into W1.
- (b) W1 does not depend on the outcome (ADR 0088 §4.7). W1 must still never rely on
  `Post`'s replay path.
- (c) The reserved-namespace precondition for W1 is **satisfied** (§4).
- (d) The "no defect, do not change `Post`" branch of the rule does **not** apply. The doc
  comment corrections in ADR 0088 §13 are still required.

## 6. Recommended fix (NOT IMPLEMENTED; requires ADR 0020 amendment + `ledger-finance` review)

### 6.1 Ledger level: compare the canonical payload on replay (recommended)

This is defense in depth. It makes every caller at least B and closes #7, #9 and #20 (and
#10-13) with no per-caller logic. On `conflict`, after the existing type check, add the
following. The type check stays first and keeps returning `ErrIdempotencyKeyReused`,
because ADR 0088 §4.5 maps it to `ErrSettlementTombstoned`.

1. Load the original row's `correlation_id, causation_id, reverses_transaction_id,
   provider_id, provider_tx_id, reason_code`. Load its entries
   `(ledger_account_id, direction, amount)` in the same query or a second one. No new
   locks are needed: the L3 set is already held, and ledger rows are immutable.
2. Compare the **canonical final entry multiset**. That is `entriesToPost`, the post-Rule
   B2 set `Post` already computed for the L3 pre-lock. Compare it as a sorted multiset
   keyed `(ledger_account_id, direction, amount)`, order-insensitive, **without netting**.
   Asset is implied by the account.
3. Also compare `ReversesTransactionID`, `ProviderID`, `ProviderTxID`, `ReasonCode`,
   `CausationID` and `CorrelationID`, with nil equal only to nil. Exception: for
   `TxTombstone`, skip `CorrelationID` (see §6.3).
4. If everything is equal, return `AlreadyPosted: true` (unchanged). If anything differs,
   return a new typed sentinel `ErrIdempotencyPayloadMismatch`, wrapped with the existing
   transaction id and the *field class* that differed (`entries`, `reversal_link`,
   `correlation`, `provider_ref`, `reason_code`). Put no amounts or account ids in the
   error string, so an HTTP mapper cannot leak them. Keep it distinct from
   `ErrIdempotencyKeyReused`; do not merge the two.

Why comparing the post-mirror set is safe: `applyBonusMirror` reads no balances. It only
resolves account types and runs `GetOrCreateAccount` for house accounts. A legitimate
retry with identical caller entries and `BonusCost` therefore regenerates byte-identical
B2 legs. A retry with a different `BonusCost.Funding` (bonus_expense vs provider_payable)
is correctly rejected.

### 6.2 Caller-level fixes (needed with or without 6.1)

- **#6 casino postBet.** Before returning the short-circuit result, compare the posted
  bet's entries (amount, wallet via session) and `correlation_id` (round) against the
  event. On mismatch, emit `casino_play_integrity_alert_bet_payload_mismatch` and reject
  with 409. 6.1 cannot cover this site.
- **#7 / #13 casino rollback and #20 deposit reversal.** Map
  `ErrIdempotencyPayloadMismatch` to an integrity alert plus 409, following the
  `ErrAlreadyRolledBack` / `ErrCallbackProviderMismatch` pattern. Optionally pre-check
  "key exists with a different `reverses_transaction_id`" so the rejection happens before
  L3.
- **#9-12 casino wins.** Map the new error to an integrity alert plus 409. Stop logging
  the *new* amount as if it were posted when `already_posted` is true.
- **#18 PlaceBet.** `CorrelationID = betID = uuid.New()` differs on every attempt. So
  under 6.1 the *legitimate* concurrent duplicate (same key, same stake) will now get
  `ErrIdempotencyPayloadMismatch` instead of `AlreadyPosted`. PlaceBet must handle that
  error by re-running its `findBetByIdempotencyKey` compare: return the existing bet if
  the params are equal, otherwise `ErrBetIdempotencyKeyReused`. This is a plain SELECT
  with no lock, so R8 is respected.
- **#3 withdrawal.Complete.** Assert `providerTxID == *wr.ProviderReference`, or add a
  one-line doc invariant, so a future caller cannot pass a different confirmation id
  under the same key.
- **Observation (not F-7, fail-closed).** The `EXISTS` check at `payments/orchestrator.go:978`
  does not exclude the reversal's own reference. A *sequential* same-ref redelivery of a
  reversal therefore returns `ErrDepositAlreadyReversed` rather than idempotent success,
  which contradicts the inline comment at `:970-975`. `payments` should fix this in the
  same item.

### 6.3 Callers affected by 6.1 (legitimate replays that currently reach `Post`)

| Site | Legit replay reaches `Post`? | Payload stable across retries? | Action |
|---|---|---|---|
| #8, #21 tombstones | Yes (casino concurrent race; payments sequential re-tombstone) | Entries yes; **correlation no** (`uuid.New()`) | Exempt `TxTombstone` from the correlation compare, or derive correlation deterministically (e.g. UUIDv5 of the key). Exempting avoids mismatches against historical tombstones. |
| #18 PlaceBet | Yes (concurrent race) | Entries yes; **correlation no** | Caller handling as in §6.2 |
| #7, #9, #19, #20 | Yes (concurrent or sequential redelivery) | Yes: entries are state- or intent-derived; correlation comes from the round or the intent | None beyond error mapping |
| #13 | No (voided short-circuit) | n/a | Error mapping only |
| #1-5, #14-17 | No (state-gated) | n/a | None; backstop only |
| #6 | No (short-circuit) | n/a | §6.2 caller fix |
| W1 (ADR 0088) | Must not (§4.7) | Keys and correlation deterministic (`bet.id`) | Treat `ErrIdempotencyPayloadMismatch` like `AlreadyPosted`: as `ErrSettlementIntegrity` |

### 6.4 Required regression tests

**Ledger (`internal/ledger`):**
- (L1) Same key, same type, identical final entries: `AlreadyPosted`, and the same id.
- (L2) Different amount: `ErrIdempotencyPayloadMismatch`. No new ledger or projection
  rows. Balances unchanged.
- (L3) Different account/wallet: mismatch.
- (L4) Same multiset in a permuted order: `AlreadyPosted`.
- (L5) Different or nil-vs-set `ReversesTransactionID`: mismatch.
- (L6) Different correlation on a non-tombstone: mismatch. Different correlation on a
  tombstone: `AlreadyPosted`.
- (L7) Different `ReasonCode`: mismatch.
- (L8) Bonus posting with identical caller entries and `BonusCost`: `AlreadyPosted`
  (mirror legs regenerated identically). Same entries with a different
  `BonusCost.Funding`: mismatch.
- (L9) Type mismatch still returns `ErrIdempotencyKeyReused`, not the new error (ADR 0088
  §4.5 depends on this).
- (L10) Concurrency: N goroutines on one key, some with equal payload and some with a
  different amount. Exactly one posts, equal-payload losers get `AlreadyPosted`, the
  others get mismatch, and there is zero drift.
- (L11) Provider-index-only conflict. Today this is an untyped error; if it becomes
  typed, pin that.

**Update existing tests:**
- Rewrite `TestIntegration_ReplayWithChangedAmountAssetOrPlayerIsWhyTheContractMatters`,
  which pins F-7, to expect mismatch.
- Rename `TestPost_SameKeyDifferentPayloadRejected` to `...DifferentTypeRejected`.

**Per caller (ADR §11.2: prove legitimate retries still match):**
- casino: a redelivered identical bet, win (direct cash) and rollback each still return
  success with the original id. Add the C-scenario tests for #6, #7 and #9, each asserting
  rejection, zero ledger effect and an integrity alert. Add a held-win rollback
  redelivery test (#13).
- payments: deposit success redelivery (existing), concurrent identical reversal
  redelivery, the #20 C-scenario, and concurrent tombstone redelivery (#21) still
  idempotent.
- sportsbook: concurrent identical `PlaceBet` returns one bet and one posting, with no
  500. A different-stake same key returns `ErrBetIdempotencyKeyReused`.
- withdrawal: Request retry with a different amount returns `ErrIdempotencyKeyReused`
  (row layer). Reject/Complete/Fail/Cancel double-invoke returns `ErrStateConflict` with
  one posting.
- bonus: ActivateGrant, ConvertGrant, TerminateGrant and held-disposition resolve, each
  double-invoked, yield one posting.
- Existing: `internal/sportsbook/exposure_integration_test.go:583` expects
  `ErrIdempotencyKeyReused` from a type collision. It must stay green.

### 6.5 Records to amend (on approval of the remediation item)

- ADR 0020: amend the "Idempotency key scope" / replay semantics to define the
  canonical-payload comparison, the tombstone correlation exemption and the new error.
- `ledger.go`: fix the `ErrIdempotencyKeyReused` and `Post` doc comments. This is required
  **now** under ADR 0088 §13 regardless of the fix.
- ADR 0038 §11 wording, per ADR 0088 §13.
- `docs/architecture/ledger-accounting-model.md`: add the replay rule.

These are architecture/record changes. They go through `architect` and the Orchestrator;
they are not made by this audit.

---

## 7. Outcome: F-7 remediation (2026-09-25)

| Field | Value |
|---|---|
| Owner | `ledger-finance` |
| Remediation commit | _to be filled by the orchestrator_ |
| Record | ADR 0020, "Amendment 2026-09-25: Stage 10 F-7 remediation" (ACCEPTED pending `ledger-finance` + `code-reviewer` sign-off, which the orchestrator records) |
| Status | Ledger fix IMPLEMENTED. Class-C caller mappings IMPLEMENTED. Casino `postBet` caller-level compare IMPLEMENTED. Withdrawal `Complete` equality assertion NOT IMPLEMENTED by design (see 7.4). Remaining record and caller items DEFERRED (see 7.4). |

### 7.1 What changed

**`ledger.Post` (§6.1, implemented as recommended).** On a key conflict:

1. The type check runs first and still returns `ErrIdempotencyKeyReused`.
2. If the type matches, Post then compares the canonical payload:
   - the final entry multiset after Rule B2 legs, as `(account, direction, amount)`, order-insensitive and not netted;
   - `reverses_transaction_id`;
   - `provider_id`/`provider_tx_id`;
   - `reason_code`;
   - `causation_id`;
   - `correlation_id`, except for `tombstone`.
3. Any difference returns the new `ErrIdempotencyPayloadMismatch`, which names only the field classes. Nothing is posted.
4. `AlreadyPosted` is returned only for an equal payload.

Implementation: `internal/ledger/replay.go` and the conflict branch in `internal/ledger/ledger.go`. The doc comments on `Post`, `PostResult.AlreadyPosted` and `ErrIdempotencyKeyReused` were corrected.

The tombstone correlation exemption is verified. Both existing tombstone writers mint `uuid.New()` per call (casino `orchestrator.go` `postRollbackTombstone`, payments `postDepositReversalTombstone`). Historical rows carry those random ids. A tombstone's meaning is fully carried by its key and its provider reference, and both are still compared.

**Callers:**

| # | Site | Change |
|---|---|---|
| 6 | casino `postBet` | New caller-level compare before the short-circuit (`verifyPostedBetMatchesEvent`) → `casino.ErrProviderTxPayloadMismatch`. It compares: stake (sum of player-owned debit legs), asset, round (correlation), session wallet. A different session of the *same* wallet is the same fact and is accepted. |
| 7, 9-13 | casino win / rollback / held-win rollback | `ReceiveCallback` maps `ledger.ErrIdempotencyPayloadMismatch` → `casino.ErrProviderTxPayloadMismatch`. The `casino_win.posted` audit can no longer record a new amount with `already_posted: true`, because `AlreadyPosted` now implies equal entries. |
| 6-13 HTTP | `casino_handlers.go`, `casino_play_handlers.go` | 409 with a generic body. Alerts: `casino_webhook_integrity_alert_payload_mismatch`, `casino_play_integrity_alert_payload_mismatch`. |
| 19, 20 | payments deposit success / reversal | Map to the new `payments.ErrCallbackPayloadMismatch`. Webhook: 409 with alert `payment_webhook_integrity_alert_payload_mismatch`. Simulation route: 409 with alert `payment_simulation_integrity_alert_payload_mismatch`. |
| 20 (obs.) | payments already-reversed check | Now excludes the reversal's own reference (`IS DISTINCT FROM`). A sequential same-reference redelivery is idempotent success. A distinct reference is still `ErrDepositAlreadyReversed`. |
| 18 | sportsbook `PlaceBet` | The concurrent-duplicate loser now gets the mismatch, because correlation is the per-attempt `betID`. `resolveConcurrentDuplicateBet` re-reads the committed bet with plain SELECTs, so R8 still holds: equal params return that bet (after checking that the ledger key's owner is the bet's own transaction); different params return `ErrBetIdempotencyKeyReused`; no owning bet is an integrity error. |
| 3 | withdrawal `Complete` | Doc invariant only (see 7.4). A confirmation reference reused across requests is now rejected by the ledger, which a test pins. |
| 1, 2, 4, 5, 8, 14-17, 21 | none | A / A-sg: state gates prevent a second `Post`. The ledger compare is a backstop. Tombstones rely on the exemption, which a test pins. |

### 7.2 Tests (all PASS, `go test -race -tags=integration`)

**Ledger** (`internal/ledger/replay_integration_test.go`), L1-L11 of §6.4:

- `TestReplay_IdenticalAndPermutedEntriesAreAlreadyPosted` (L1, L4)
- `TestReplay_DifferentAmountRejected` (L2)
- `TestReplay_DifferentAccountRejected` (L3, plus a direction flip and a non-netted superset)
- `TestReplay_DifferentReversalLinkRejected` (L5)
- `TestReplay_ProviderRefAndCausationCompared`
- `TestReplay_CorrelationComparedExceptTombstone` (L6)
- `TestReplay_DifferentReasonCodeRejected` (L7)
- `TestReplay_BonusMirrorLegsCompared` (L8, uses a conversion because a grant has no Funding-dependent leg)
- `TestReplay_TypeMismatchStillErrIdempotencyKeyReused` (L9)
- `TestReplay_ConcurrentMixedPayloadsOneEffect` (L10)
- `TestReplay_ProviderIndexOnlyConflictIsUntypedAndPostsNothing` (L11, pins the untyped error)

Every rejection test asserts: zero new ledger rows, unchanged balances, and not `ErrIdempotencyKeyReused`.

**Updated tests that pinned the old behaviour:**

- `TestIntegration_ReplayWithChangedAmountAssetOrPlayerIsWhyTheContractMatters` now asserts rejection for changed amount, player and asset. The exact replay is still `AlreadyPosted`. The behaviour change is documented in the test.
- `TestPost_SameKeyDifferentPayloadRejected` was renamed to `TestPost_SameKeyDifferentTypeRejected`.
- `depositInput` (ledger tests) and `TestBonusMirror_ConcurrentGrantsSameKeyOnlyOnePosts` used a fresh `uuid.New()` correlation per retry. That no longer models a legitimate retry, so they now reuse one correlation.

**Per caller:**

- casino (`internal/casino/replay_f7_integration_test.go`):
  - `TestF7Casino_LegitimateRedeliveriesStillResolveToOriginal` (bet, win, rollback)
  - `TestF7Casino_ConcurrentTombstoneRedeliveryIsIdempotent` (#8)
  - `TestF7Casino_BetReplayWithDifferentPayloadRejected` (#6: amount, round, missing session, unknown session)
  - `TestF7Casino_WinReplayWithDifferentAmountRejected` (#9)
  - `TestF7Casino_RollbackRefReusedForDifferentOriginalRejected` (#7)
  - #13 is covered by the existing `TestPostRollback_HeldWinRollback_DuplicateIsIdempotent`.
- casino HTTP: `TestCasinoPlay_F7_SameIdempotencyKeyDifferentAmountIs409` checks that the wager and win replays with changed amounts return 409, and that an identical retry returns 200 with the same transaction.
- payments (`internal/payments/replay_f7_integration_test.go`):
  - `TestF7Payments_SequentialReversalRedeliveryIsIdempotent`
  - `TestF7Payments_ConcurrentIdenticalReversalRedelivery`
  - `TestF7Payments_ConcurrentTombstoneRedeliveryIsIdempotent` (#21)
  - `TestF7Payments_ReversalRefReusedForDifferentDepositRejected` (#20)
- sportsbook (`internal/sportsbook/placebet_replay_f7_integration_test.go`):
  - `TestPlaceBet_F7_ConcurrentIdenticalDuplicateReturnsOneBet` (deterministic L3 race; verified to FAIL without the `PlaceBet` change)
  - `TestPlaceBet_F7_ConcurrentSameKeyDifferentStakeRejected`
  - `TestPlaceBet_F7_SequentialSameKeyDifferentStakeRejected` (closes the §3 evidence gap)
- withdrawal (`internal/withdrawal/replay_f7_integration_test.go`):
  - `TestF7Withdrawal_RequestRetryWithDifferentAmountRejected`
  - `TestF7Withdrawal_TerminalTransitionsDoubleInvokeOnePosting` (Reject, Complete, Fail, Cancel)
  - `TestF7Withdrawal_CompleteWithAnotherRequestsConfirmationRejected` (#3, closes the §3 cross-request evidence gap)
- bonus (`internal/bonus/replay_f7_integration_test.go`):
  - `TestF7Bonus_ActivateAndConvertTwiceOnePostingEach`
  - `TestF7Bonus_TerminateTwiceOnePosting`
  - #16 is covered by the existing `TestHeldDisposition_ResolveReforfeit_WithFourEyesAndSEP1`.
- The existing `internal/sportsbook/exposure_integration_test.go` type-collision test still expects `ErrIdempotencyKeyReused` and is unaffected.

### 7.3 Records

ADR 0020 amendment: written.

Not edited here (the orchestrator or architect applies these):

- ADR 0038 §11 wording (ADR 0088 §13);
- `docs/architecture/ledger-accounting-model.md`, which should gain the replay rule (§6.5).

### 7.4 Deferred / open

1. **W1 settlement mapping.** Owner: sportsbook / W1. `internal/sportsbook/settlement.go` `lockAndPost` must map `ledger.ErrIdempotencyPayloadMismatch` to `ErrSettlementIntegrity` (§6.3, last row). The W1 owner has added this mapping in the working tree, but it is not committed as of this writing, and it was not written by this item. With it in place, `TestSettlementFaultInjection_LedgerKeyBackstop` passes. The orchestrator must make sure the mapping lands in the same merge as this remediation, or before it.
2. **Withdrawal `Complete` equality assertion (§6.2).** Not implemented, by design. `Complete`'s own contract (Flow 3) says the send-confirmation reference is *distinct* from `MarkSubmitted`'s instruction reference, so asserting equality would contradict the documented design. Instead:
   - a doc invariant was added;
   - the ledger now rejects cross-request reuse, pinned by `TestF7Withdrawal_CompleteWithAnotherRequestsConfirmationRejected`.
3. **Pre-L3 reversal-link pre-check (§6.2, optional).** Not implemented. The ledger compare already rejects the case before anything is written; the only cost is that the rejection happens after the L3 locks.
4. **Reserved provider-id guard (§4, optional).** Not implemented. It is not part of F-7.
5. **New finding, not F-7 (P1 candidate, reported to the orchestrator).** Payments `receiveDepositReversalCallback` takes no lock on the original deposit before its already-reversed `EXISTS` check. Concurrent reversal callbacks under **distinct** references for one deposit therefore each post a reversal. A temporary probe was run and then removed: 6 concurrent distinct-reference reversals of one 1,000 deposit posted 4-6 reversals, leaving `player_cash` at -3,000 to -5,000. Casino `postRollback` avoids this with `FOR UPDATE` on the original. The analogous payments fix is an L2 `FOR UPDATE` on the original deposit's ledger row before the check. It needs its own item, with ledger-finance and ADR 0082 lock-order review.
