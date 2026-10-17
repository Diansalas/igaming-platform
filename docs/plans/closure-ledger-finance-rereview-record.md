# Closure ledger-finance RE-REVIEW record (2daf352..d64d478)

Reviewer: `ledger-finance` specialist, acting as an INDEPENDENT reviewer. I did not write any of the reviewed changes. This is a
report only: no code, migration, test, ADR, registry or governance file was changed. Worktree `.claude/worktrees/lfrev2`, branch
`lfrev2`, created from the integration tip `claude/focused-wright-jw88w9` @ `d64d478`. Migration head under review: 0175.
All providers are MOCK and all data is synthetic. This is a software-capability review only; no regulatory claim is made.

Commits reviewed: `db2d737`/`b745a1b` (c1, 0173), `4df97f4`/`b973ecc` (c3, 0174), `f144a00`/`693c855` (tomb, 0175, F-3, F-4),
`d64d478` (ServerCursor.Fetch -> NextBatch rename; mechanical, three call sites, no semantic change).

## Verdict

**APPROVE WITH CONDITIONS** (conditions in section 7; none blocks the merge into the integration branch, and C-1 and C-2 are
go-live / vendor-onboarding gates).

The ledger invariants hold. The ledger is still append-only. Idempotency is unchanged: the same key, the same savepoint
conflict path and the same `AlreadyPosted` semantics. No balance is updated and no floating point is introduced. The
tombstone rule ("a rollback for a never-seen transaction writes a tombstone so a late original is rejected") holds for deposits,
casino rollbacks, sportsbook settlements and payout reversals on every production path I traced. No new double-credit path was
found. The residuals below are availability or stranding cases. Each needs a stolen runtime credential, an unusual
configuration or an operator decision. None is a ledger-integrity break.

## 1. Migration 0175 and `internal/payments/deposit_tombstone_proof.go`

### 1.1 Append-only and idempotency: preserved

- The guard is a BEFORE INSERT row trigger. It never UPDATEs a row. The new column `actor_proof_nonce` is set on `NEW` only:
  first forced to NULL, then set to the verifier-consumed nonce. `ledger_transactions_immutable` and `no_truncate` are untouched.
- `ledger.Post` still inserts inside `db.IdempotentInsert` (a savepoint). On a key conflict, the BEFORE trigger has already run
  `actor_proof_require` and inserted into `actor_proof_nonces`. The savepoint rollback undoes that insert, so a nonce is never
  burnt by a replay. The `AlreadyPosted` / payload-comparison path then runs unchanged. Tombstone correlation is still exempt
  from the comparison (`ledger/replay.go`), and the deposit tombstone still uses a random correlation, as before.
- A duplicate delivery stops at the receipt dedup (`UNIQUE (tenant_id, provider_id, event_fingerprint)`) before any signing.
  A second, distinct reversal event for the same original stores its own applied receipt, signs, and then hits the key
  conflict: the result is `AlreadyPosted`. This is the same as before 0175 (`TestF1Tomb_RealReversalCallback_...` covers it).
- No new lock. The guard's lookups are covered by indexes: payment_attempts uses the `(tenant_id, provider_id,
  provider_reference)` prefix, and payment_provider_events uses `(tenant_id, provider_id, event_fingerprint)`. The new partial
  unique index covers proof-bound rows only. `0175` sets `lock_timeout`.

### 1.2 Tombstone rule per writer (traced in code; casino and sportsbook paths run in my targeted runs)

| Writer | Path through the guard | Result |
|---|---|---|
| Deposit reversal, original never posted (`receipt.go` -> `postDepositReversalTombstone`) | The receipt (R0) is inserted `applied` in the same transaction before signing, so the provider always has a receipt. The proof is required and always attached. The target is the receipt id. The DT001 lookup matches the stored `original_provider_reference` verbatim. | Written and signed. The late original is still rejected by the `(tenant, provider_id, provider_tx_id)` index and `tombstoneExists`. Unchanged. |
| Casino rollback of an unseen bet (`casino/orchestrator.go:2844`) | Non-payment exemption. It needs the plain `WithTenant` context, which every casino callback path uses (verified: no `WithPlayerScope` on any casino callback or simulation path). It also needs a casino provider id with no payment rows in the tenant. | Written unsigned. Unchanged. |
| Sportsbook rollback of an unseen settlement (`sportsbook/settlement.go:762`) | No `provider_id`, so the guard returns at its first test. | Unchanged. |
| A5 payout reversal (`payout_reversal.go:307`) | Payout exemption. The Go side already requires the reference to be the request's or the open attempt's own (`known`, `withdrawal.ErrReversalPayloadMismatch` otherwise). The SQL side requires the same, plus a provider match. `payment_attempts_tenant_provider_ref` is unique across operations, so the "no deposit attempt holds it" clause can never trip for a genuine payout. | Written unsigned. WD027 governs it. Unchanged. |

### 1.3 Can a legitimate tombstone now be rejected? Quantified

The guard can refuse a legitimate tombstone in the following cases.

**(a) A casino provider id that "has payment rows" in a tenant.**
- I probed this as `igaming_runtime` in a rolled-back transaction on my private DB. A tombstone on a provider id with a stored
  payment receipt, in the plain tenant context, gives **AP001**. A tombstone on an unused provider id under a player GUC also
  gives **AP001**.
- How it can happen:
  - Configuration: a casino adapter and a PSP adapter registered under the same `provider_id`. Only a convention prevents this
    today (the comment at `cmd/platform-api/main.go` around line 293). The registered mocks use distinct ids (`mock-payments`
    and `mock-casino`).
  - A planted row: the stolen runtime credential INSERTs a `payment_attempts` or `payment_provider_events` row under the
    casino id.
- Duration: both tables are append-only for every role, so the condition is **permanent** for that (tenant, casino provider)
  until an owner-applied correction.
- Effect:
  - Every rollback of a never-seen bet from that provider fails, loudly (AP001 -> 5xx to the vendor).
  - Rollbacks of SEEN bets are unaffected. They are compensating entries, not tombstones.
  - Money harm only if the vendor gives up retrying and the original bet then arrives: it posts, and the player is debited for
    a bet the vendor voided.
- Detection: the casino statement and late-original reconciliation, plus the AP001 5xx rate.
- Exposure: zero today (pre-launch, mocks only). Needs a configuration error or a runtime-credential compromise. See
  condition C-2.

**(b) A deposit reversal on a key already held by an UNSIGNED tombstone.**
- Sources of an unsigned tombstone on that key:
  - a pre-0175 tombstone;
  - the tomb-record residual 1 forgery, on a provider the tenant had not yet used;
  - a payout tombstone on the same (provider, ref). This is impossible, because references are unique per tenant and
    provider across operations.
- What happens: the signed insert conflicts, and `ledger.Post` returns `AlreadyPosted`. The reversal is not lost, because the
  reference is already tombstoned, so the late original is still rejected. But the row carries no nonce, so it **never clears
  MA020**. See section 2.

**(c) Signing failures.**
- No issuer -> `ErrDepositTombstoneProofUnavailable` -> 503 + `Retry-After`. The whole transaction, receipt included, rolls
  back. `TestF1Tomb_NoIssuer_FailsClosed_ThenRetrySucceeds` shows the redelivery succeeds once the key is back.
- `ErrInvalidClaims` cannot fire. Every claim field is fixed or derived: the system actor, the scope, a non-nil tenant, the
  uuid target and a hex digest.
- Production startup already refuses to run with no `ACTOR_PROOF_KEYS` or no active kid.

**Is a permanent 503 loop possible?** Only while the actor-proof key configuration stays broken (no issuer). That is the same
dependency the F4 dispatch gate already has. There are two other failure classes. Both are retried, because they are 5xx, but
neither gets the dedicated 503 or log line. Both last only while the configuration is broken:
- **AP002**: the app's kid is not `active` in `actorproof_private.actor_proof_keys` (a rotation desync);
- **AP003**: the app clock is more than 5 s ahead of the DB clock.

In both cases the generic `payment_webhook_failed` 500 is returned (condition C-3).

No realistic data state makes a correctly configured deposit reversal fail forever. One pre-existing failure is NOT introduced
by 0175: a `deposit_reversal` with an EMPTY `original_provider_reference` already failed permanently before 0175, on the 0099
`provider_tx_id` CHECK. It now fails earlier, at DT001. The outcome is the same and it is recorded here for completeness.

## 2. Key shape, receipt binding and the MA020 `payment_ref_cleared` semantics

- Key shape: unchanged (`tombstone:<provider>:<ref>`). The idempotency key and the correlation are computed once, then both
  signed and posted.
- Binding: the payload hash covers tenant, provider, reference, ledger key, correlation and receipt id (`k2_canonical`). The
  Go and SQL digests agree on the real callback path (test green). DT001 runs after verification and requires all of:
  - `event_type = 'deposit_reversal'`;
  - `disposition_at_receipt = 'applied'`;
  - the same tenant, provider and original reference.

  All three values are written by `insertReceiptDeduped` in the same transaction, immediately before the tombstone. No
  deferred-receipt replay path calls `postDepositReversalTombstone`. Its only callers are `receipt.go:1469` and the
  integration-only fixture, both pinned.
- MA020: `payment_ref_cleared` = tombstone AND `actor_proof_nonce IS NOT NULL`. This closes the forgery for every
  classification.

**Cases where a genuinely reversed, never-posted deposit can no longer be cleared:**
1. **A pre-0175 unsigned tombstone on that reference.** Pre-launch, so the expected count is zero. It must be verified at go-live
   (C-1).
2. **The key is pre-occupied by an unsigned (forged) tombstone** (1.3(b)). Here the money can be stranded:
   - the genuine success is rejected (tombstone precedes success), and the attempt goes to `disputed`;
   - the MA020 exposure stays open;
   - the operational runbook states "**There is no override**" for MA020;
   - a PSP refund produces a reversal, but its tombstone only hits `AlreadyPosted`, so it **still** does not clear;
   - result: every K2 CREDIT to that player is refused indefinitely. Debits are unaffected. The deposited money is with the PSP
     (refundable to the player), not lost in the ledger.
   - Operator remedy today: none in-app. It needs an owner-governed correction, and the incident path must be written down
     (C-3).
   - Exposure: needs (i) the stolen runtime credential, (ii) a provider the tenant has never used for payments, and (iii) a
     correct prediction of a future PSP-assigned reference.
3. **A reversal seen only in a statement.** This is the pre-existing 0172 accepted cost, unchanged.

## 3. Migration 0173: first-deposit single grant

- No double credit:
  - App rule: any existing grant of the offer for (tenant, player), in any status, consumes the slot, under the sweep's
    per-tenant advisory lock.
  - DB backstop: a partial unique index on a derived, immutable `first_deposit_only` flag.
  - `TestFirstDepositBackstop_*` covers direct SQL inserts, a forged flag and 8 concurrent inserts.
- No retroactive swap: acceptable, and financially the right choice. Swapping would mean reversing an issued grant, its
  postings and its Progress trail.
- Player-harm residuals (product, not ledger):
  - (i) A cancelled, forfeited or expired FDO grant keeps the slot forever, with no in-app reset. The c1 record notes that
    today's FDO grants are cancelled by the jurisdiction gate. So once activation becomes possible, players whose first deposit
    fell in a gate-cancel window permanently lose that FDO offer. This needs an explicit owner/product decision (C-4).
  - (ii) The player is granted on the first VISIBLE deposit, not the chronologically first.
- Non-deposit grant paths (bulk job, code redemption, cashback) on an FDO offer that already has a grant:
  - they hit the new index;
  - `IssueGrant` reports any savepoint conflict as `ErrGrantAlreadyExists`;
  - `issueIdempotent`'s 5-tuple lookup then returns `ErrNoRows`, so the result is a hard error, never a silent "already
    granted" success and never a second grant.

  This fails closed, but the error is opaque (informational, I-1).
- The pre-check is safe:
  - It is read-only.
  - It refuses with BG173 and never deletes or rewrites financial rows.
  - `NO FORCE` / `FORCE` RLS is lifted and restored inside the migration transaction (`internal/db/migrate.go` wraps each file
    in `conn.Begin`), so a failure rolls the RLS change back with it.
  - Minor: no `lock_timeout` in 0173 (pre-launch, informational).

## 4. Migration 0174 and the C3 detector

- **Read-only.** One SELECT. It writes only the run's `reconciliation_mismatches` rows and the alert.
  `TestC3_DetectorIsReadOnly` passes.
- **Same predicate as the WD050 gate.** `rg.walletBalanceNegative` checks `wallet_balance_projection`, `player_cash` /
  `player_locked_cash`, `credit_total < debit_total`. The detector evaluates that exact predicate on the projection, plus the
  same predicate on the ledger rebuilt from `ledger_entries`. That is a superset. The two sides can differ only under
  projection drift, and the same run reports that drift as `balance_mismatch`. `TestC3_PredicateMatchesTheGate` passes.
- **Holds are not netted.** This is correct: netting would hide exactly the chargeback-with-open-withdrawal case.
- **False positives:**
  - (i) The legacy `updated_at` dispatch fallback can over-attribute a later chargeback to a payout. It applies only to
    requests with no attempt timestamps and no gate decision. No such rows exist pre-launch.
  - (ii) An unfunded hold triggers the detector. Only a test fixture (`wdpath.Plant`) can build one; production
    `RequestWithdrawal` checks funding under the L3 lock.
  - I found no false-positive path from live postings (`TestC3_ConcurrentLivePostings_NoFalseFinding`).
- **Idempotent findings and no P1 flooding.**
  - There is one stable discriminator per kind per tenant (`stream:ledger_wallet_negative`,
    `stream:ledger_payout_while_negative`), so a persisting condition is ONE open alert with growing occurrences
    (`TestC3_ConcurrentSweeps_OneAlertNoDuplicates`, `..._RepeatsIdempotent`).
  - Findings rows grow by (negative accounts × runs). Hourly runs give 24 rows/day per negative account. This is the existing
    append-only pattern, and acceptable.
- **Fail closed.** A blind read fails the whole `ledger_vs_projection` run. The drift check is then lost for that run, but the
  failure is loud (`raiseRunFailed`).
- **No gate consumer.** No consumer gates on a clean `ledger_vs_projection` status: the only run-status consumer outside
  reconciliation is the MA020/M2 gate, which reads `payment_statement`. So one negative wallet cannot block tenant-wide
  operations.
- **Cost (informational).** The check aggregates the tenant's player-account entries every hour. This is the same order of
  cost as the existing rebuild. Watch it at volume.

## 5. Test changes in 2daf352..d64d478: no weakened assertion

I diffed every MODIFIED `*_test.go` (`git diff --diff-filter=M 2daf352..d64d478 -- '*_test.go'`). **No assertion was removed or
loosened.** All removed lines are fixture construction code. The changes:

- **13 tombstone fixture sites.** Raw `ledger.Post(TxTombstone ...)` or direct `postDepositReversalTombstone` was replaced by
  `PlantVerifiedDepositReversalTombstone`, which stores a synthetic applied receipt and signs through the production writer.
  Files:
  - `adjustment/b23_b25`, `adjustment/b11_b17`;
  - `httpserver/a5_payout_reversal_http`;
  - `payments/a5_payout_reversal`, `b11_payout_unbound_hold`, `escalation_b6_b7` (2), `f3_payout_tombstone` (2),
    `iwire_tombstone`, `m4_resolve1_world`, `ma020_b3_e2e`, `poll_amount` (2);
  - `reconciliation/ma020_parity`, `prh2_r9_bound_clear`.

  Side effects:
  - Some fixtures change their key from a custom shape (`m4-tomb:`, `ma020-b3-tomb:`, `<pid>:tomb:`) to
    `tombstone:<p>:<ref>`. No assertion reads the key.
  - The extra synthetic receipt can only ADD reconciliation findings, never hide one.
  - F3/B11's "foreign tombstone" is now a signed one. The SR5R-1 backstop is still exercised.
- **`withoutUnfundedHoldFixtureNegatives`** was added to three D2/R9 world helpers (`prh2_d2_merchant_crosscheck`,
  `prh2_d2_parked_capture`, `prh2_r9_bound_clear`). It filters ONLY the two NEW C3 kinds, and ONLY for accounts whose sole debits
  are `withdrawal_requested` (fixture unfunded holds). Every pre-existing kind passes through, and
  `TestC3_FixtureFilter_KeepsRealNegatives` proves it keeps a chargeback-driven negative. This is a scoped exclusion of a new
  finding that the fixtures themselves create, not a loosening of an existing assertion. Recorded as an intentional change.
- **`adr0113_check_d_review`** funds the GBP wallet (`negbal.Deposit`, 130), so the fixture holds are funded. The assertions are
  unchanged.
- **`actorproof_test`**: the non-vacuity plant count went from 8 to 13 (strengthened). It also gained new pins and the
  `SystemActorDispatchGate()` call form.
- **`withdrawal/f4r1_signed_decision`**: the call form changed only.

## 6. Commands and counts (private DB `lfrevtwo170206p16347x01`, built by `priv_db.sh` at head 0175, dropped afterwards)

| Command (`priv_test.sh -tags integration -count=1 -p 1 -v`) | Result |
|---|---|
| `-run 'TestF1Tomb\|TestMigration0175\|TestF3_\|TestMA020\|TestB11\|TestIWire' ./internal/payments/` | ok, 119 PASS, 0 FAIL (170 s) |
| `-run 'FirstDeposit\|LateEarlier\|Backstop\|0173' ./internal/bonus/` | ok, 10 PASS |
| `-run 'TestC3\|Migration0174\|MA020\|ADR0113D\|R9\|D2' ./internal/reconciliation/` | ok, 115 PASS |
| `-run 'Rollback\|Tomb' ./internal/casino/` (incl. `RollbackOfNeverSeenOriginalWritesTombstone`, `LateOriginalAfterTombstoneIsRejected`, `F7Casino_ConcurrentTombstoneRedelivery`) | ok, 35 PASS |
| `-run 'Tomb\|Rollback' ./internal/sportsbook/` | ok, 29 PASS |
| `-run 'Reversal\|A5\|Tomb' ./internal/httpserver/` | ok, 17 PASS |
| `./internal/actorproof/...` (full, integration tag) | ok |
| SQL probe as `igaming_runtime`, rolled back: collision provider -> AP001; unused provider under player GUC -> AP001; unused provider in plain context -> admitted, caller nonce stripped to NULL, `payment_ref_cleared` = false | as described in 1.3 / 2 |
| `gofmt -l` (changed Go files); `go vet -tags integration` payments, reconciliation, bonus | clean |

Not run: the full payments, httpserver and casino suites (the orchestrator's verification run covers them).

## 7. Conditions and accepted residuals

Conditions:
- **C-1 (go-live gate).** Before production data exists, run a check that returns zero rows: `tombstone` rows with
  `actor_proof_nonce IS NULL` whose `provider_id` has a `payment_attempts` or `payment_provider_events` row in the same tenant.
  Build the standing detective version of this query (tomb-record residual 1, today `NOT IMPLEMENTED`) before the first
  real-PSP tenant goes live.
- **C-2 (before real vendor onboarding).** Enforce disjoint `provider_id` namespaces across payment, casino, sportsbook and KYC
  adapters, by a startup or registration check rather than a comment. A collision makes casino unseen-rollback tombstones fail
  closed permanently (1.3(a)).
- **C-3 (runbook / alerting).**
  - Document the stranding case of section 2 item 2 in `operational-runbooks.md` next to "MA020: There is no override". State
    that the remedy is an owner-governed correction plus a security incident, because it implies a runtime-credential
    compromise.
  - Make AP002/AP003 on the deposit-reversal webhook visible: either the dedicated 503 log line, or an alert on a sustained
    `payment_webhook_failed` rate for `deposit_reversal`.
- **C-4 (product decision, `bonus-engine` / `product-owner-proxy`).** Record as an explicit owner decision that a cancelled,
  forfeited or expired FirstDepositOnly grant permanently consumes the slot. This includes grants cancelled by the jurisdiction
  gate. If the decision is no, an operator "reset first-deposit eligibility" action is needed (scope decision, not built).

Accepted residuals (exposure / detection / mitigation):

| # | Residual | Exposure | Detection | Mitigation |
|---|---|---|---|---|
| R-a | Unsigned tombstone pre-occupying a reference on a provider the tenant has never used | Runtime credential + unused provider + predicted PSP reference | Casino C5/R9 late-original; the C-1 query once built | Proof-bound MA020 means it can never CLEAR an exposure. Stranding is in section 2. |
| R-b | Casino rollback outage from a provider-id collision or a planted payment row | Configuration error or runtime credential; permanent per (tenant, provider) | AP001 5xx rate; casino reconciliation | C-2; fails closed and loud |
| R-c | Key or clock desync -> deposit reversal 5xx until fixed | Operations | Generic webhook 500 | Startup key check; C-3 |
| R-d | Late-visible earlier deposit gets no FDO grant; cancelled grant keeps the slot | Every FDO offer | n/a (by design) | C-4 |
| R-e | C3 legacy `updated_at` over-attribution | Legacy rows only (none) | n/a | Attempt and gate timestamps take precedence |
| I-1 | Non-deposit grant on an FDO offer -> opaque `ErrNoRows` error | Bulk jobs / redemption on FDO offers | Job error | Fails closed; map the 0173 index name to a typed error (optional) |

## 8. Labels

- 0175 signed deposit tombstone + proof-bound MA020: `IMPLEMENTED` (ledger-finance: approved with C-1..C-3).
- 0173 FirstDepositOnly single grant + DB backstop: `IMPLEMENTED` (approved; C-4 is a product decision).
- 0174 / C3 negative-wallet and payout-while-negative detection: `IMPLEMENTED` (approved).
- Detective "unsigned tombstone on a payment provider" query: `NOT IMPLEMENTED` (C-1).
- Provider-id namespace disjointness enforcement: `NOT IMPLEMENTED` (C-2).
- F-3 pins and the F-4 bounded counter: reviewed only for financial impact (none). The security sign-off is `security`'s.
