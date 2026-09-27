# RV-PRH-I3 — Independent code review: ADR 0096 KYC enforcement (PRH-I3)

Reviewer: `code-reviewer` (independent of the implementer). Date: 2026-09-27.
Scope: commits `69f603f` `9dc362d` `2858ac2` `df0a0b2` `b3e1e85` and merge
`d4a0e08` (conflict in
`internal/operatingmarket/qa_migration_rls_survives_failed_rollback_test.go`),
branch `claude/focused-wright-jw88w9`. I read the diff itself, not only the
§15 summary. `security` and `ledger-finance` review separately. Items in their
domains are listed in §4 for them to decide. I have not ruled on them.

## Verdict: **NOT READY**

The evaluator core (`EvaluateEnforcement` on the withdrawal path, migration 0100
RLS and lifecycle) is careful and well tested. The change is still not ready to
be marked `IMPLEMENTED`, for three reasons:

1. The gates that are actually wired have no call-site tests. I disabled the
   `RequestWithdrawal` deny branch and every suite still passed (§2, T1).
2. The ADR's own §12.2 says conditions C1 and C7 "must hold before PRH-I3 is
   marked IMPLEMENTED". The C7 tests do not exist.
3. `DenyForCompliance` is labelled `IMPLEMENTED`, but nothing calls it and no
   test covers it.

Rework items R1–R6 are listed in §5.

## Verification performed

- `go test -tags=integration` against the local CI database
  (`igaming_platform_ci_local`, at migration 100):
  - PRH-I3 kyc tests `TestEvaluateEnforcement*` and `TestMigration0100_*`: 23/23
    PASS. This excludes the scratch-database round-trip.
  - `internal/withdrawal`, `internal/casino`: PASS.
  - `internal/sportsbook`: one FAIL,
    `TestDBConstraints_RLS_UpdateAndDeleteAreNoOpsUnderNormalScope`, with
    "permission denied" under the runtime role. It fails identically at the
    pre-change baseline `2a6f425`, so it is environmental and not a PRH-I3
    regression.
- **Not run by me:** `TestMigration0100_UpDownUpRoundTrip` and the derived
  chain-tip pin tests. Both need a CREATEDB admin database URL, which I did not
  have. They were reviewed by reading only.
- **Two extra mutations**, applied in a throwaway worktree that was then
  removed. Both survived:
  - **MX1:** `RequestWithdrawal`'s `if !decision.Allowed` changed to
    `if false && ...`. Result: `internal/kyc`, `internal/withdrawal`,
    `internal/wallet` and `internal/httpserver -run 'Withdraw|Financial|KYC'`
    all PASS. **SURVIVED.**
  - **MX2:** `evaluateDepositThreshold`'s
    `unavailableDecision("asset_not_covered_by_active_policy")` changed to
    `notRequiredDecision(...)`. This is security N2's fail-closed branch.
    Result: `internal/kyc` PASS. **SURVIVED.**

## 1. Correctness findings (most severe first)

### B1 — MEDIUM — Withdrawal handler maps the two failure classes to the wrong responses
`internal/httpserver/withdrawal_handlers.go` (new block after `RequestWithdrawal`).
`EvaluateEnforcement` reports a real DB failure as a nil error with
`Outcome=unavailable, Allowed=false`, so `RequestWithdrawal` returns a
`*KYCDeniedError`. `ErrKYCUnavailable` is returned only for a *caller bug*
(a nil tenant, brand, player or person id).

The handler inverts these:
- **Transient outage.** Scenario: `kyc_verifications` is briefly unreachable, or
  a statement timeout fires. The player gets **409 "withdrawal requires a passed
  identity verification: unavailable"**, and a durable `unavailable` decision row
  is written. The player is told to verify identity during an outage.
- **Programming error.** Scenario: a missing `PersonID`. The player gets
  **500 "temporarily unavailable, please retry"**, which invites endless retries
  of a request that can never succeed.

Fix: map `Decision.Outcome == OutcomeUnavailable` to a retryable 503/500, and map
`ErrKYCUnavailable` to a non-retryable internal error. No test covers either path.

### B2 — MEDIUM — Migration 0100 down silently destroys append-only compliance history
`migrations/0100_…down.sql` runs `DROP TABLE` on `kyc_enforcement_decisions` and
`kyc_enforcement_policies` without checking whether they hold rows. The repo's
own precedent (0075 down, and also 0048/0052) is to `RAISE EXCEPTION` while an
append-only table holds rows. 0100's header claims it mirrors 0075's conventions
"verbatim".

Scenario: an operator rolls back one migration on staging or production after
decisions have been recorded. The whole KYC decision audit trail (the §7.6
SAR-adjacent record) and all four-eyes policy provenance are gone, and nothing
warns. Add a refuse-while-non-empty guard, or record an explicit decision to do
otherwise with `security` (see §4).

### B3 — LOW — Staff read API pagination: the returned cursor cannot be sent back
`internal/httpserver/kyc_enforcement_handlers.go`:
- The response sets `next_before = "<RFC3339Nano>,<uuid>"`. The handler only
  reads `before_decided_at` and `before_id`. No parameter accepts `next_before`.
- A malformed or half-supplied cursor is silently ignored, and so is a malformed
  `limit`.

Scenario: a client echoes `next_before` back, or sends only
`before_decided_at`. It gets page 1 again, forever.

Fix: accept the cursor in the same shape it is emitted, and return 400 on a
malformed cursor. There is also no handler test at all, covering neither role
gating (tenant_admin must get 403) nor the cross-tenant 404.

### B4 — LOW — Dormancy report omits `play`, contradicting its own comment
`internal/kyc/enforcement_dormancy.go` cross-joins only
`('cumulative_deposit')`. Its comment says it reports "only trigger_types the
evaluator actually consults", and `play` *is* consulted. The doc also says "at
least one active tenant", but the query does not filter on tenant status.

Scenario: a jurisdiction that requires KYC before play never has an active
`play` row. The report never flags it as dormant, which is the exact
false-assurance gap that security condition 9 asked this report to close. It is
also untested.

### B5 — LOW — Doc comments contradict the implemented read key
Two comments say the KYC read key is per Person ("latest verification row across
every PlayerAccount … not per wallet/PlayerAccount"):
- `EnforcementParams.PersonID` (`internal/kyc/enforcement.go`)
- `RequestParams.PersonID` (`internal/withdrawal/withdrawal.go`)

The code, correctly per security N1, uses a per-PlayerAccount primary key plus a
deny-only cross-account overlay. PRH-I1 will read these comments when wiring the
deposit and payout gates, so they should describe the real behaviour.

### B6 — LOW — `DenyForCompliance` trusts whatever decision it is given
It never checks `decision.Allowed == false` or
`kycParams.Operation == EnforcementWithdrawalPayout`. Scenario: a future caller
passes the wrong decision value. The function then reverses a legitimate hold,
moves the request to `rejected`, and records an *allowed* decision row under the
audit action `withdrawal.rejected_kyc`. A two-line guard returning
`ErrInvalidInput` closes this.

### B7 — LOW — Same-key concurrent retry can report a KYC denial for a hold that exists
In `RequestWithdrawal`, the pre-insert replay lookup happens before the KYC read,
so two concurrent requests with the same key can both miss it. Scenario:
request A is allowed and places the hold. Before A commits, the KYC state
changes, so concurrent request B (same key) evaluates as denied. B returns
`KYCDeniedError` and the client sees "requires verification", although the
request exists. The window is narrow. It could be closed by re-checking for an
existing request before returning a deny, or at least documented.

Also noted: expiry is compared using the application clock (`time.Now()`), not
the DB clock (`now()`). This is harmless unless the clocks drift apart.

## 2. Test adequacy

**T1 — BLOCKING — The wired enforcement points have no negative tests, and the fixture changes hide that.**
Commit `2858ac2` seeds an `approved` verification into every existing fixture:
withdrawal, wallet, casino, and the httpserver helpers plus
`TestFinancialHappyPath_EndToEnd`. That is reasonable for unrelated tests. But no
test anywhere then drives the real call site with a *non*-approved player. In
detail:
- **`RequestWithdrawal`:** no test covers deny (pending, failed, or no row) with
  zero `withdrawal_requests` rows, zero postings, and a committed decision and
  audit. MX1 proves this.
- **HTTP handler:** no test covers the fresh-transaction `RecordDecision` path,
  so security condition 5 is unproven. No test covers the 409/500 mapping (B1).
- **Casino `postBet` and sportsbook `PlaceBet`:** zero tests with an active
  `play` policy on a licensed tenant, for either deny or allow. Every existing
  casino and sportsbook tenant has no licence, so both gates short-circuit to
  `not_required` before the policy read.
- **`DenyForCompliance`:** zero tests. Every C7 row is missing: one reversal,
  `SUM(DEBITS)==SUM(CREDITS)`, no drift, Reject/Cancel-vs-Deny race, replayed
  submit gets `ErrStateConflict`.
- **Stale reference:** the withdrawal fixture comment refers readers to
  "kyc_enforcement_integration_test.go" for "tests that specifically exercise
  the KYC gate". No such file exists in `internal/withdrawal`.

**T2 — BLOCKING — Only the withdrawal operation of `EvaluateEnforcement` is exercised.**

| Enforcement point | not_required | passed | pending | failed | unavailable |
|---|---|---|---|---|---|
| withdrawal_hold (evaluator) | n/a | yes | yes (both statuses) | yes (none, rejected, expired) | **no** |
| deposit (evaluator) | only the *unlicensed* branch | **no** | **no** | **no** | **no** (MX2 survives) |
| casino_play / sportsbook_play | **no** | **no** | **no** | **no** | **no** |
| withdrawal_payout | n/a | **no** | **no** | **no** | **no** |

Details:
- `evaluateDepositThreshold` and `sumSettledDeposits` are never executed by any
  test: not the licensed-dormant branch, not below or at threshold, not the N2
  asset-not-covered branch. I confirmed with `EXPLAIN` that the SQL at least
  parses against the schema.
- The stored statuses `unverified` and `expired` are not tested. They fall
  through the default branch.

**T3 — Evidence and claims in ADR §15 that are not accurate.**
- **§15.1, ledger-finance N2 row.** It says the genuine-race conflict path is
  tested by `TestRequestWithdrawal_ConcurrentRequestsOnlyOneSucceeds`. That test
  uses a distinct `uuid.New()` key per goroutine, so it never reaches the
  same-key conflict branch. It also claims the "replay after hold does not
  re-gate" behaviour is tested. `TestRequestWithdrawal_IsIdempotentOnRetry`
  never changes KYC state between the two calls, so it would pass with the gate
  placed before the lookup.
- **`evidence/prh-i3-mutation-kill.txt`.** It says `unavailable` fail-closed "IS
  exercised end-to-end … via TestEvaluateEnforcement_DepositDormantByDefault's
  own not_required/unavailable distinction". That test only asserts
  `not_required` on an unlicensed tenant and never produces `unavailable`.
- **Mutations.** The 3 real mutations are sound and killed, but all three target
  the withdrawal evaluator path. Relying on reasoning for the DB-error branches
  is acceptable as a disclosed gap. It is **not** adequate for the non-DB-error
  branches (deposit polarity and threshold comparison, the N2 fail-closed branch,
  play), which are ordinary logic and trivially testable. MX2 shows the gap.
- **`TestMigration0100_ActiveRequiresLegalReviewReference`.** It asserts only
  `err != nil`. It is correct today (the principal differs and the trigger is
  wired, so only the CHECK can fire), but it would still pass if the failure
  came from some other cause. Assert the constraint name or the SQLSTATE.
- **Concurrency.** No new concurrency test was added. Existing repeat counts are
  unchanged. By design (§5) there is no lock between the KYC read and the hold.
  That relies on the payout backstop, which is not wired (see §3).

**T4 — Fine as is.**
- **Migration and RLS tests:** up/down/up is structurally correct (by reading);
  FORCE RLS on both tables; tenant-scoped and platform-service writes are
  refused; genuine platform-admin write with provenance; cross-tenant isolation
  and append-only on the decisions table; lifecycle and four-eyes; refusal to
  activate an unwired trigger type.
- **Evaluator mapping tests:** latest row wins in both orderings; expiry is
  independent of stored status; the cross-account overlay; cross-tenant
  invisibility.

**T5 — Derived chain-tip pins: they still catch misordering.**
All four files (bonus, jurisdiction 0077, operatingmarket 0076, and the QA
failed-rollback test) still compare the *actual* `MigrateDown`/`MigrateUp`
sequence against a fully specified expected list. The derived above-99 part is
sorted newest-first, matching `MigrateDown`'s contract. So these are still
caught:
- a reordered historical migration
- a missing or non-applied 0100 (the filesystem expects it; the actual list
  lacks it)
- an under-count

The QA test only asserts `err != nil`, but an under-count would still stop
before 0076 and fail. That weakness is pre-existing.

The merge resolution is correct. It takes the derived count plus the updated
"0099" comment.

Nits:
- The helper's doc says it scans `*.up.sql` files, but it parses any file with a
  4-digit prefix (for example a stray `0101_notes.md`).
- The same helper is copied three times. One shared helper in
  `internal/testsupport` would be simpler.

## 3. Labels and registry

- **ADR 0096 header and §15** say "IMPLEMENTED" for "`withdrawal.DenyForCompliance`".
  - §12.2 makes C1 and C7 preconditions for `IMPLEMENTED`, and C7 is entirely
    missing.
  - `DenyForCompliance` has no caller and no test.
  - Correct label: **PARTIALLY IMPLEMENTED**. The function exists but is unwired
    and untested; its C7 tests and its caller are deferred to PRH-I1.
  - The withdrawal-request and play gates should also be qualified: wired, but
    with no call-site tests until T1 is fixed.
- **Registry `KYC-ENFORCE-1`** says "IMPLEMENTED … for … withdrawal-payout-dispatch
  backstop (`DenyForCompliance`, not yet wired …)".
  - An unwired backstop is not implemented. The row's own requirement is "at
    minimum before payout submission", and nothing at payout submission consults
    KYC today.
  - Concrete scenario: a player is approved, requests a withdrawal, and is then
    rejected before staff submit the payout. The payout proceeds.
  - The row should say **NOT IMPLEMENTED** for the payout backstop.
- **Registry `PRH-I3`** "IMPLEMENTED (…), pending review" should become
  **PARTIALLY IMPLEMENTED** until R1–R3 below land.
- **Dependency rows.** DR-PRHI3-01, -02 and -03 are present and accurately
  describe the casino, sportsbook and withdrawal edits. §15.4's two
  payments/PRH-I1 requests (deposit gate; payout T1p/T2/T12 plus retargeting the
  raw guard) have **no DR row**. ADR 0095's W-KYC rows cover the payout design,
  but the PRH-I1 registry row does not mention either KYC call. Register them so
  the launch-blocking wiring cannot be lost.
- **Disclosed deviations.** The §7.6 play-surface narrowing (no decision row for
  `not_required`) and the jurisdiction resolved internally are both
  honestly disclosed.

## 4. Surfaced to other owners (not decided here)

- **`security`:**
  - B2 (the down migration destroys append-only history).
  - The §7.6 narrowing: no decision row on the dormant play path.
  - `resolveLicensingJurisdictionID` does not filter on `licences.status`, the
    same pattern as SEC-4I-F10. A suspended licence still resolves, so policies
    keep applying under a licence the tenant no longer holds.
  - Ties on identical `created_at` (two rows in one transaction) are broken by a
    random UUID.
  - The player-facing message discloses the outcome enum (condition 8 appears
    satisfied).
- **`ledger-finance`:**
  - `DenyForCompliance`'s posting and exactly-once behaviour are untested (C7).
  - `sumSettledDeposits` counts gross `deposit` credits and does not net
    `deposit_reversal`. That is conservative (it over-counts toward the
    threshold), but HD-KYC-1 must confirm it.
  - Revocation-vs-hold TOCTOU with no payout backstop wired.
- **`identity-compliance` / product:** a newer `pending` re-verification row
  outranks an older `approved` one. A player who starts an optional
  re-verification or tier upgrade is blocked from withdrawing until it resolves.
  This is consistent with "latest row wins", but the policy should be confirmed.

## 5. Required rework before READY

- **R1.** Add call-site tests:
  - `RequestWithdrawal` deny for pending and failed, with zero rows, zero
    postings, and the decision and audit committed through the HTTP handler.
  - Replay after revocation is not re-gated (revoke between the two calls).
  - The same-key concurrent conflict path.
  - Casino and sportsbook deny and allow with an active `play` policy on a
    licensed tenant, including the decision row.
- **R2.** Add deposit-evaluator tests: licensed-dormant, below threshold, at
  threshold (the boundary), asset not covered → `unavailable`, and `passed`.
  MX1 and MX2 must then be killed.
- **R3.** Either add the C7 `DenyForCompliance` tests now, or relabel it
  (ADR §15, header, registry) as described in §3.
- **R4.** Fix B1 (the handler's outcome mapping) and B2 (the down-migration
  guard, or a recorded `security` decision not to add one).
- **R5.** Correct the inaccurate claims:
  - §15.1 N2 row
  - the mutation-evidence "unavailable IS exercised" sentence
  - the withdrawal fixture comment
  - the KYC-ENFORCE-1 and PRH-I3 labels
- **R6.** Register DR rows for the two PRH-I1 wiring requests.

Follow-ups, not blocking: B3, B4, B5, B6, B7, the `ActiveRequiresLegalReview…`
assertion, and the duplicated chain-tip helper.
