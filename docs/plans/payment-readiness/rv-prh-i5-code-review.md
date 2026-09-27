# RV-PRH-I5: Independent code review of payment statement reconciliation (PRH-I5)

Reviewer: `code-reviewer`, independent of the implementer. Date: 2026-09-27.
Scope: commit `16c69b7`, merged in `4693189`, on branch `claude/focused-wright-jw88w9` (HEAD `f6ded0a`).
The design sources are ADR 0095 §12, §12.7, §13.3 and §16.3, and reconciliation-model.md §2.2.
I read the diff itself, not only the §12.7 record.

Some questions fall to `ledger-finance` or `security`. Those are routed in §4. I have not ruled on them.

## Verdict: **NOT READY** (one rework item; everything else is a follow-up)

The stream is correct against the ADR's classification table, and it is carefully built:

- Fetch runs outside any transaction.
- Ingest is idempotent and the statement store is append-only.
- The match runs under enforced REPEATABLE READ.
- All eight kinds are implemented and tested.
- 28/28 mutants are killed, and I reproduced that result.

It is still not ready, for one reason. **As wired in `cmd/platform-api`, the stream raises a P1 on every deposit and withdrawal that goes through the live HTTP paths.** §12.7 does not disclose this (F1). The fix is small: either disclose it or gate it. But the decision belongs to `ledger-finance`, and the current label overstates what the wired stream delivers.

## 1. Verification performed

The work used a private database, `igaming_prh_i5_cr`, created fresh and migrated to 0102. The parent commit `e927ed4` was checked on `igaming_prh_i5_cr_parent`.

- `go build ./...` and `go vet -tags=integration` pass on the touched packages.
- PRH-I5 suite `-run 'PaymentStatement|Migration0102'`: 35/35 PASS.
- `TestInitAppRole_RerunKeepsPaymentStatementGrants` PASSES when `TEST_RUNTIME_DATABASE_URL` is set. Without it the test SKIPs, and the CI list edit (§3.6) guards against that.
- Mutation harness `prh-i5-mutate.py`, re-run independently: **28 killed, 0 survived**, baseline PASS after restore, working tree clean.
- I added 6 mutants of my own. 5 survived (§3.5).
- I wrote temporary probe tests (deleted afterwards, not committed). They confirmed F1, F3 and F4 empirically.
- Full `go test -tags=integration ./...` at HEAD. Failures:
  - `TestMigration0082_DepositIntentsDenyTruncate` fails with `cannot truncate a table referenced in a foreign key constraint`. **Report claim verified.** It fails identically at `e927ed4`. The cause is 0101's `payment_attempts.deposit_intent_id REFERENCES deposit_intents (id)`. It pre-dates PRH-I5.
  - `TestMigration0100_UpDownUpRoundTrip` and `TestMigration0100_DownRefusesWhileDecisionsHoldRows` also fail. They hard-code migration 100 as the chain tip and roll back 1. At `e927ed4` they fail the same way ("got [101]"). **They also pre-date PRH-I5, but the PRH-I5 report does not mention them.** Register them as a follow-up (§3.7).
  - `TestResolutionIsolation_*` in `internal/httpserver` failed once under the full parallel run. They passed 3 of 3 times when run alone at HEAD. These are timing-lane tests that CI runs alone. Not related to PRH-I5.

## 2. Rework item (blocking)

### F1 (High): the wired stream flags every live-path deposit and withdrawal as a P1

`cmd/platform-api` wires `providers.PaymentsStmt` into `RunSchedulerLoop`. That source is the MOCK over `b.Payments`, provider id `mock-payments`.

The live paths do not go through `payment_attempts`:

- **Deposits.** The live handler is `internal/httpserver/deposit_handlers.go:139`. It still calls the legacy `Orchestrator.InitiateDeposit`, which creates no `payment_attempts` row.
- **Withdrawals.** `withdrawal_handlers.go:918` and `:1117` call `withdrawal.Complete` with the payments provider id, and there are no payout attempts at all yet (§12.7: payout dispatch through the gate is not wired).

The success posting still carries `(provider_id, provider_tx_id)`. The LF95-C13 ledger join, direction (b) in `checkLedgerJoin`, therefore finds a `deposit` or `withdrawal_completed` posting in the window with 0 succeeded attempts. The coverage window runs from MockProvider construction (process start) to now, so the whole process lifetime is covered. The join records `pay_missing_platform_record`. That repeats every hourly sweep, for every such posting since the process started.

Evidence:

- **Deposits, by probe.** I called the legacy `InitiateDeposit`, then sent a verified success callback, then ran the MOCK stream. Result: exactly one `pay_missing_platform_record ... type=deposit check=ledger_join ... succeeded attempts=0`.
- **Withdrawals, by code reading.** The same branch applies to `withdrawal_completed`.

By the ADR's definition this is the "dual-write orphan class", so the classifier is not wrong. The problem is operational. Until the deposit and withdrawal cutover, the reconciliation P1 signal is saturated by expected behaviour, which trains operators to ignore it. §12.7 does not mention this. It says only that gate-less records are "untagged and appear on no tenant's statement". That covers the statement side, not the ledger-join side.

Required, as one of the following, with a `ledger-finance` ruling:

- (a) Do not wire the payment source in `cmd/platform-api` until the deposit and payout cutovers land. Keep the stream, tests and store.
- (b) Exclude legacy postings from direction (b), with a stated rule. Example: a deposit whose `deposit_intents` row has no attempts, or a withdrawal with no payout attempt. Label it `PARTIALLY IMPLEMENTED` until the cutover.
- (c) Keep it as-is, but disclose it explicitly in §12.7, reconciliation-model §2.2 and the task registry. Also add a test that pins the behaviour.

Whichever is chosen, add a test that exercises the live legacy path against the wired source. Today no test covers it.

## 3. Follow-ups (non-blocking), most severe first

### F2 (Medium): absent-line `pay_unresolved` cannot fire when the window is no longer than the horizon

`checkUnmatchedAttempts` requires `inCoverage(sentAt)`, meaning `sentAt >= cs`, and also `aged`, meaning `sentAt < ce - horizon`. Both hold only if `ce - cs > horizon`.

For any periodic real statement whose window is at most 24 h (a daily file with the 24 h horizon), an in-flight attempt with no line is **never** flagged. Once its window has passed, it is outside every later window.

With the MOCK, the rule fires only after 24 h of process uptime, and every redeploy resets it. My probe with window = horizon produced 0 mismatches across two consecutive windows.

This follows the literal text of §12.3 ("for each platform attempt in the coverage window"), and payments T16 escalation also covers stuck attempts. But the ageing rule should not be coverage-gated: coverage exists to protect `pay_missing_provider_record`. Route to `ledger-finance` (PROVIDER DEPENDENT until the first real source).

### F3 (Low): a declined or pending `deposit_reversal` line with no posting is a P1

`matchReversal` flags `pay_missing_platform_record` whatever the line status. The probe confirmed that a `declined` reversal line (for example, a chargeback the merchant won) produces a P1, although no posting is expected. That matches the ADR's "any status" wording, but it is semantically doubtful. Route to `ledger-finance`.

### F4 (Low): a payout line's `settlement_reference` is never compared

When a payout line resolves by instruction reference, a `SettlementReference` that contradicts the ledger's `withdrawal_completed.provider_tx_id` is not flagged. The probe confirmed 0 mismatches. §12.3 does not require the check. A `pay_reference_mismatch` sub-check would be cheap.

### F5 (Low): the coverage window is not sanity-bounded at fetch

`FetchPaymentStatement` checks only `start != 0` and `end > start`. If a source reports `CoverageEnd` in the future, `agedBefore` (which is `ce - horizon`) moves forward and in-flight attempts are flagged `pay_unresolved` early. A very old `CoverageStart` widens the missing-record scope.

Suggestion: refuse `CoverageEnd > fetchedAt + skew`. This matters for real sources only.

### F6 (Low): MOCK growth and idempotency in practice

The MOCK's `CoverageEnd` is `now`, so every sweep stores a new import of the whole lifetime statement. The table is append-only and cannot be deleted from. §12.7 discloses this as dev-only growth. As a consequence, idempotent ingest is never exercised by the wired source, only by `payFixedSource`. When the tenant's MOCK record count passes 1,000,000, every run fails as a P1 (line cap). This is acceptable for dev and already disclosed. Noted for completeness.

### 3.5 Test gaps: claimed coverage versus what is exercised

§16.3 is met at the per-kind level. Several sub-rules inside the kinds have no test. My own mutants (same method as the harness: build the mutant, run `-run PaymentStatement`, restore):

| Mutant | Result |
|---|---|
| RM1: deposit branch of ledger join (a) disabled | killed |
| RM2: reversal status check (posted reversal vs `pending`/`declined` line) removed | **survived** |
| RM3: `duplicate_match` (two lines resolving one attempt) removed | **survived** |
| RM4: payout lookup via the separate `SettlementReference` field removed | **survived** (only the "line ref = settlement ref" form is tested) |
| RM5: reversal asset check removed | **survived** |
| RM6: ledger join (b) ignores `withdrawal_completed` | **survived** (only the deposit orphan is tested) |

RM6 matters most. It is the payout half of LF95-C13, which the record says is "proven with fixture sources".

Two harness and test notes:

- The harness counts any non-zero `go test` exit as KILLED, so a mutant that fails to compile would be counted as killed. The recorded `fail:` lines show real assertion failures for all 28, so the evidence holds. The harness should still tell a build failure apart from a test failure.
- The down migration's second guard (a `pay_*` mismatch exists) cannot be reached: a mismatch implies an import, and the first guard already refuses that. It is harmless defence in depth, but it is untested.

### 3.6 CI evidence list edit

The four added names exist and PASS locally, and CI sets `TEST_RUNTIME_DATABASE_URL`, so the grants test cannot skip silently.

`TestMigration0102_RuntimeGrantsMinimal` can `t.Skip` when there is no runtime role, and it is not in the list. Consider adding it.

### 3.7 Pre-existing failures (not PRH-I5, but unregistered)

- `TestMigration0100_UpDownUpRoundTrip` and `TestMigration0100_DownRefusesWhileDecisionsHoldRows` hard-code a chain tip. They have failed since 0101 merged. They should derive the tip the way `migration0075MigrationsAbove` does.
- `TestMigration0082_DepositIntentsDenyTruncate` fails for the reason the report gives (0101's FK).

Both should be registered. Neither appears in the PRH-I5 report's failure list except 0082.

## 4. Item-by-item confirmation of the brief

- **Eight detectors.** All present and matching the §12.3 tables. Asset is checked before amount (`else if`). Amounts are compared as `big.Int`, parsed from `NUMERIC::text`, with no float anywhere. Line matching is provider-bound (`WHERE a.provider_id = $2`; PM16). A duplicate line is reported once per key, because matching walks lines sorted by (reference, kind, line_no) and skips any line whose key equals the previous one.
- **Ledger join on `(provider_id, provider_tx_id)`, both directions.**
  - (a) Deposit: the posting under the attempt's `provider_reference` must be the attempt's own `ledger_transaction_id`.
  - (a) Payout: `withdrawal_requests.release_ledger_transaction_id` must be this provider's `withdrawal_completed`.
  - (b) Every in-window posting must map to exactly one succeeded attempt.
  - Correct as specified. See F1 for the wiring consequence, and RM6 for the test gap.
- **Payout instruction vs settlement reference.** Lookup order is instruction (`byRef`), then settlement (`l.ref`), then settlement (`l.settlement`), then merchant reference. Correct. See F4 and RM4.
- **Coverage window.** Applied to unmatched attempts, the ledger join and platform duplicates. Lines are always matched. Correct (PM12). See F2.
- **Unresolved horizon keyed to coverage end.** `agedBefore = coverage_end - horizon`, with no wall clock in matching. Deterministic. The equality `DefaultPaymentUnresolvedHorizon == payments.DefaultSettlementWindow` is asserted by a test.
- **REPEATABLE READ.** Enforced by `requireSnapshotIsolationFor` (PM20). The snapshot test uses a real between-reads hook with a READ COMMITTED kill control, and the control does raise the false P1. That makes it a meaningful test.
- **Idempotent ingest.** `ON CONFLICT (…6-tuple…) DO NOTHING`, followed by a re-select. Safe under concurrent identical ingests at READ COMMITTED. The line count is bound to stored rows by two triggers: a statement-level cap, and a deferred exact-count trigger (PM27, PM28).
- **Scheduler variadic sources.** `RunSweep`, `RunSweepTenants` and `RunSchedulerLoop` gained a trailing `...PaymentStatementSource`. No existing caller changed, and `go build ./...` is clean. A nil source fails closed and is audited as a P1 (tested).
- **MOCK fidelity.** `statementFor` renders only the MockProvider's in-memory, tenant-tagged records. It holds no DB handle, and the tag comes from the gate's `CallContext` (PM25). It is fetched through `callProvider` with `ReadOnly: true`. The `tagRecord` defer runs before `Unlock` (LIFO), so it runs under the mutex.
- **Disclosed deviations.**
  - The `Fetch(PaymentFetchRequest)` signature has no credential-shaped leaf type. Acceptable.
  - Provider-succeeded vs platform-in-flight is not age-gated. This matches §12.3. With the MOCK it can raise a transient false P1 between a provider-side success and the callback being applied. Disclosed as PROVIDER DEPENDENT.
  - The MOCK gaps (no payouts, no reversals, single-replica, reset on restart) are disclosed accurately. F1 is the one undisclosed consequence.
- **No-ledger-write test.** `assertOnlyRunWritten` checks all of the following, for both a clean run and a mismatch run:
  - +1 run and +N mismatches;
  - ledger transaction and entry counts;
  - `SUM(debit) == SUM(credit)`, with both totals unchanged;
  - projection totals;
  - attempt, intent, receipt and withdrawal fingerprints;
  - statement store counts.

  It is killed by the harness's MX9 mutant (PM23, `ledger.Post` from the match) and by PM24.
- **Migration 0102 up/down.**
  - Tables: RLS FORCE with `tenant_staff_scope`, and append-only triggers for UPDATE, DELETE and TRUNCATE that also bind the owner.
  - `pay_*` kinds: a strict superset of 0098.
  - Grants: runtime SELECT and INSERT only, re-asserted in `init-app-role.sql`.
  - Down: uses the constraint-validation guard, which RLS cannot blind. The clean round trip and the refusal once an import exists are both tested.
- **Chain-tip pins.** The PRH-I5 tests derive 0102's version from the filename and migrate only up to it, so migration 0103 will not break them. The KYC 0100 pins are broken but pre-existing (§3.7).
- **Invariants.** No float money. No balance UPDATE. `tenant_id` comes only from the server-side sweep (`PaymentFetchRequest.TenantID`, the RLS-bound ingest and match). No secrets. There is no HMAC or webhook-scheme code in this change, so the constant-time checklist item does not apply.

Routed (not adjudicated here):

- **`ledger-finance`:** F1 (the choice among options a, b and c), F2, F3, and the payments finding recorded in §12.7 (the T13 `payment_attempts_tenant_ledger_tx` unique violation).
- **`security`:** nothing new from this review. The `rv-prh-i5-security.md` review is separate.
