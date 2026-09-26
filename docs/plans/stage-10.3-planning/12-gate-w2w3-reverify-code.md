> Gate 10.3-W2/W3: `code-reviewer` re-verification (2026-09-26, against `e80114b`, branch
> `claude/focused-wright-jw88w9`). Re-checks the 10 findings of `10-gate-w2w3-review-code.md`
> (`3f77254`). Each finding was checked against the code, not the commit messages. Not committed.

# Gate 10.3-W2/W3: code re-verification

**Verdict: READY WITH FOLLOW-UPS.** #1 to #9 are CLOSED. #10 was not addressed; it is accepted as one
Low follow-up (disposition below). No new correctness bug was found. There are two new Low
observations (N-A, N-B). Neither blocks the gate.

## How this was verified

- I read the diffs `3f77254..e80114b` for `casino_consistency.go`, `casino_statement.go`, the
  rejection helper, `awssm`, the auth tests, the 0092 test and the registry/ADR rows. I cross-checked
  the C2 positive rules against every `postWin` branch (`internal/casino/bonus_settlement.go`
  direct-cash, locked-cash, locked-bonus and held) and against `postBet` (`orchestrator.go:1257-1282`).
- Tests run locally with Go 1.26.8, all passing with no skips:
  - vet on the touched packages;
  - unit tests for `internal/auth`, `internal/secretstore/...` and `cmd/platform-api`;
  - integration tests for `internal/reconciliation` (full package, 31 s), `internal/casino` (full),
    `internal/httpserver -run TestCasinoRejectionRecord` and `internal/ledger -run TestMigration0092`.
  - gofmt reports nothing.
- I ran two mutants myself (file restored afterwards):
  - Reverting C4's exemption to `IN (r.id, o.id)` fails
    `C4/cash_bet_rollback_credits_player_bonus`. **Killed.**
  - Dropping the lock-release subtraction from the C2 win rule fails
    `C2_PositiveHouseRulesCleanControls/locked_cash_win_with_lock_release`. **Killed.**

## Per-finding status

| # | Status | Verification |
|---|---|---|
| 1 | CLOSED (security's domain) | `security` re-verified W2A-SEC-1/-2 as CLOSED in `11-gate-w2w3-reverify-security.md`. I checked that the artefacts exist and pass: the tripwire `cmd/platform-api/outbound_precondition_test.go`, the three `*_keylog_test.go` files and `webhookauth/verified_log.go`. I did not adjudicate the security content. |
| 2 | CLOSED | The C4 exemption now checks the original only (`WHERE e.ledger_transaction_id = o.id`). The negative subtest (a cash bet whose rollback credits `player_bonus` must be the only finding and must name the bonus leg) and the positive control (a BONUS_SET original stays exempt) both exist. My mutant is killed. |
| 3 | CLOSED | Bet rule: with no BONUS_SET leg, `house_gaming` credits must equal the stake (the sum of debits on the player's `player_cash`/`player_bonus` wallet accounts, the same definition `casino_statement` uses). Win rule: `house_gaming` debits must equal wallet credits minus lock-release debits. That is exact for all four `postWin` shapes: payout A plus released lock L credited to the wallet, L debited from `player_locked_*`, A debited from house. Rule B2 mirror legs are tenant-level and never `house_gaming`, so they fall outside both sides. The BONUS_SET bet exemption is a recorded deviation in the file comment and in `reconciliation-model.md`, and is defensible because G-6 is unshipped. There are 4 detection subtests, each asserting this is the only finding with exact evidence, plus clean controls. |
| 4 | CLOSED | The class sets match the paper 02 §2.19 ruling: 6 finding classes and 5 evidence-only. SQL: `reason_class = ANY($2)` with a fresh slice. E9 (a different reference) is now evidence-only, which removes the conflict with `casino_statement`. Tests: a partition test compares migration 0097's CHECK, `internal/casino`'s constants and an independent restatement of the ruling. There is one subtest per class, plus a real-path test for E7 and E9. The class-by-class metric is copied and is never null. |
| 5 | CLOSED | ADR 0093 now says awssm is `PARTIALLY IMPLEMENTED` (code, wiring and fake tests `IMPLEMENTED`; IAM `NOT IMPLEMENTED`; real AWS `STAGING REQUIRED`). The string "NOT IMPLEMENTED in this build" is gone from `cmd/`. The registry rows for PROV-CRED-RESOLVER-1, PROV-OUTBOUND-CRED-1, KYC-PROVIDER-SELECT-1 and SECRETSTORE-AWS-1 are updated, and PROV-OUTBOUND-CRED-1 carries its launch-blocking precondition. |
| 6 | CLOSED | `TestMain` installs a tripwire HTTP client used by both the credential client and the Secrets Manager client. `TestAWSSM_New_SuccessPathMakesNoNetworkCall` shows `New` makes zero requests and that `Get` reaches only the ECS agent and the Secrets Manager host, both through the tripwire. `TestAWSSM_HangingStoreBoundedThroughFetcher` shows a hang returns as unavailable within 2 s + 1 s through `Fetcher.Fetch`. An AST guard keeps `awssm.New` out of tests outside the package. |
| 7 | CLOSED | `casino_statement.go` now documents unpaired tombstones and many-to-one pairing. The "real source may want to flag both" note is correctly marked PROVIDER DEPENDENT. |
| 8 | CLOSED | `RequireAnyPermission` tests cover: holding only the first permission passes; holding only the second passes (Finance and TenantAdmin hold disjoint permissions); holding neither gives 403 with the same body as `RequirePermission`; the rule is any-of, not all-of; an empty list is denied; unauthenticated is 401. Checked against `permission.go:872-890`. The C4 boundary is covered by #2's subtests. |
| 9 | CLOSED (0092) | The hold-back is now derived from each file's version: `migrationFileVersion` parses the 4-digit prefix, refuses a short or non-numeric name loudly, and holds back everything `> 92`. That matches the `0097`/`0098` `DirThroughSelf` precedent, and the stale comments are refreshed. **Residual (Low, part of the follow-up below):** `internal/jurisdiction/migration_0075_integration_test.go` still hard-codes `MigrateDown(dir, 24)` and a 24-entry `wantDown` list. It will fail at 0099. It fails loudly, not silently, so it does not block. Its comments at lines 351 and 602 still say "twenty-two" while the code uses 24. |
| 10 | OPEN, accepted as a Low follow-up | See the disposition below. |

## #10 disposition: register one Low follow-up, nothing needs removing now

None of these items is on a money path or crosses a tenant, and none has a production caller that
could misbehave today. Register one row, **CODE-HYGIENE-10.3-1 (Low)**, owned by the orchestrator
together with the named specialists, covering:

1. **`db.Pool.WithTenantSnapshot` duplicates `WithTenant`** (`internal/db/tenant_snapshot.go:25-48`
   against `tenant_rls.go:30-53`). The only difference is `pgx.TxOptions{IsoLevel: RepeatableRead}`.
   - Failure scenario: a later change to how `WithTenant` sets tenant context (another GUC, a
     `statement_timeout`, a role switch) is not copied to the snapshot variant. The `casino_statement`
     stream then runs with different session settings than every other stream.
   - Fix: extract one private `withTenantTx(ctx, tenantID, pgx.TxOptions, fn)`.
   - Owner: architect (it is the data-access layer).
2. **`providercred.DerivedTokenCache` has no size bound and only evicts lazily**
   (`internal/providercred/outbound.go:157-230`).
   - Entries are keyed on the fingerprint, so after a rotation the old fingerprint's entry is never
     read again and never evicted. It is removed only on a `Get` for that same key. That means derived
     secret material stays in process memory after rotation or revocation. It cannot be served,
     because the key no longer matches, but it is retained.
   - There is no production caller today.
   - **Condition:** bound or sweep it before the first production caller. Tie this to the
     PROV-OUTBOUND-CRED-1 precondition and put it to `security`; the retention question is theirs to
     judge, not mine.
3. **`secretstore.Router.Backends()`** (`internal/secretstore/router.go:105`) has no callers, including
   tests.
   - It is an exported path to the raw `Store`s that skips the `Fetcher`'s fingerprint check and
     circuit breaker.
   - Recommendation: delete it. It is one function with nothing to migrate, so this can be done in any
     later commit.
4. **`reconciliation.ListRunsForStream`** (`read.go:56-60`) is a three-line wrapper with no callers,
   since the handlers use `ListRunsForStreams`. Delete it.
5. **`RunSweepTenants`** is used only by tests today. Keep it: the CAS-RECON-SCALE-1 row names it as the
   intended on-demand single-tenant re-run entry point. The row should say it is not yet wired.
6. The `casino_statement` alias identifiers noted in the original review: delete them, or keep them
   with a one-line reason.
7. **#9 residual:** switch the migration 0075 chain test to the version-derived pattern and fix the
   "twenty-two" comments.

Removing items 3 and 4 now would be cheap, but it is not a gate condition.

## New observations (both Low, neither blocks)

- **N-A. `awssm.NewWithSDKFake` is exported from a non-test file** (`internal/secretstore/awssm/sdkfake.go`),
  so it is compiled into the production binary.
  - It is mitigated three ways:
    - it runs the same `preflight` as `New`;
    - its store fails closed, returning not_found for every `Get`;
    - an AST test (`TestNewWithSDKFake_OnlyFromTests`) forbids non-test callers, including references
      to it as a function value.
  - Residual: the fake is still a `*Store`, so it carries `MarkProductionEligible`. If the AST guard
    were ever bypassed, the synthetic guard would treat a fail-closed fake as a real backend. That
    produces an outage, not a leak.
  - Acceptable. Optionally move it behind an `export_test`-style seam when `cmd/platform-api`'s
    wiring tests can take an injected constructor another way. Adding to the follow-up row is enough.
- **N-B. The C6 partition test's coupling to migration 0097 is deliberate and fails loudly.**
  - It reads the CHECK with `QueryRow ... LIKE '%reason_class%'`. Today exactly one CHECK matches
    (0097:38). The two named constraints, `rollback_shape` and `rollback_amount`, do not mention
    `reason_class`.
  - If a future migration adds a second CHECK that mentions `reason_class`, `QueryRow` would pick one
    of them arbitrarily. The test would then fail with a confusing "class sets diverge" message rather
    than pass silently, which is the correct direction.
  - The test also hard-codes `len(inCheck) != 11`, so a new class forces a ruling, as intended.
  - Suggested hardening, optional: filter on the constraint name, or on `LIKE '%reason_class IN%'` /
    `'%reason_class = ANY%'`.

## Checked and found sound

- **R-1 context handling.**
  - `context.WithTimeout(context.WithoutCancel(ctx), 2s)` keeps the request's values (request id,
    tenant) and drops its cancellation. It is applied once, in the shared helper, so the webhook route
    and the play-simulation routes both get it.
  - `defer cancel()` is present.
  - The test covers both a cancelled context and an expired deadline. The failure-path test now forces
    the failure through the helper's own nanosecond bound, which also proves the bound is enforced.
  - A mid-callback cancellation that surfaces as a context error, not `*CallbackRejectedError`,
    correctly writes no row, because no rejection decision was made.
- **C6 metrics semantics.**
  - `rejections_unposted` still counts every class with no ledger transaction. That is documented on
    the field, and the real-path test pins `RejectionsUnposted == 2` for E7 and E9.
  - It is not a mismatch source, so it cannot raise a false P1.
- **C2 SQL.**
  - All the new sums are `COALESCE(...,0)::text` and are parsed with `parseBig`, so there is no float
    and no overflow.
  - `stake` is limited to accounts with a `wallet_id`, so a tenant-level `player_*` account (not
    possible today) could not inflate it.
  - A win crediting `player_bonus_held` is counted in `walletCr`, because held accounts carry
    `wallet_id`, which is correct for the held branch.
- **Toolchain/CI.**
  - `go.mod` pins `go 1.26.0` and `toolchain go1.26.8`. The Dockerfile uses `golang:1.26.8-alpine`.
  - govulncheck is `@v1.8.0` and golangci-lint is v2.9.0.
  - The claim of no local govulncheck run is disclosed in the registry, and CI remains the verifier.

## Not verified here

- GitHub CI on `e80114b`.
- golangci-lint and govulncheck: I did not run them locally.
- The security content of W2A-SEC-1/-2, R-1/R-2 and N-1. That belongs to `security` (see
  `11-gate-w2w3-reverify-security.md`); I checked only that the tests exist and pass.
- Financial sign-off on the C2 deviation and the C6 ruling belongs to `ledger-finance`. I checked only
  that the code and tests match the recorded ruling.

## Working-tree note

At review time `internal/httpserver/stage9_concurrency_integration_test.go` had an uncommitted
modification that this review did not make. It is outside this review's scope and was left untouched.
