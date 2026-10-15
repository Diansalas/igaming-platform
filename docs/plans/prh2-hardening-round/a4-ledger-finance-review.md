# A4 independent ledger-finance review: MANUAL-ADJ-LINK-1 (migration 0153)

- **Reviewer:** `ledger-finance` (independent; did not write the change), 2026-10-15.
- **Subject:** commit `4c77ad5` on `worktree-agent-aae3ab5308d231dfa` (base `73b2836`): migration 0153
  (two NOT DEFERRABLE AFTER INSERT constraint triggers), `internal/testsupport/adjlinkfix` + `adjlinkpost`,
  32 fixture sites, ADR 0100 §21, `docs/plans/a4-adjustment-fence-record.md`.
- **Verdict:** **APPROVED (ledger-finance sign-off)**, no blocking finding. Three Low findings (documentation
  accuracy and test hygiene) should be fixed before or at merge; three Info notes. The `security` review is
  still required (ADR 0100 §21 status) and is not replaced by this review.
- **Environment:** private databases rebuilt from this worktree's migrations (`priv_db.sh`), local only, not CI.
  All three private databases were dropped afterwards. No role, password or shared-infrastructure change.

## 1. Test evidence (this review, local)

| Command (`-tags integration -count=1 -p 1`) | Result |
|---|---|
| `./internal/ledger` | ok (55.8s) |
| `./internal/adjustment` | ok (142.7s) |
| `./internal/reconciliation` | ok (222.8s) |
| `./internal/db` | ok (84.0s) |
| `./internal/casino` | ok (118.1s) |
| `./internal/sportsbook` | ok (146.5s) |
| `./internal/launchgov` | ok (58.9s) |
| `./internal/tenant` | ok (97.1s) |
| `-race ./internal/adjustment ./internal/ledger` (`-timeout 150m`) | ok (381.3s / 58.6s) |
| `./internal/payments -run '^TestH' -timeout 150m` (own private DB) | ok (869.9s) |
| `./internal/payments -run '^TestS' -timeout 150m` (own private DB) | ok (660.4s) |
| `go test -count=1 ./internal/adjustment ./internal/tenant ./internal/ledger` (unit / static, incl. B-21 and the exemption inventory) | ok |
| `go vet -tags integration ./internal/testsupport/... ./internal/adjustment` | clean |

`migrate verify` was not run as a gate: the version gap at 152 is expected in this worktree (orchestrator adds
152 to `reservedUnusedVersions` at merge). Not a finding.

## 2. Probe results

| Probe | Result |
|---|---|
| Governed credit and debit, four-eyes / threshold | Pass. `TestLinkFence0153_GovernedPathPostsIdempotentlyAndAudited` (credit + debit through the real Service, posting keys, audit, clean recon) plus the unchanged threshold suites (`policy_integration`, B-1, B-7, K2-C1) all pass under the fence, including `-race`. |
| Idempotent replay, ADR 0020 results unchanged (AFTER vs BEFORE) | Agree with AFTER. A replay conflicts on the `(tenant_id, idempotency_key)` unique index while the heap tuple is inserted; AFTER ROW triggers are queued only for rows actually inserted, so `AlreadyPosted`, `ErrIdempotencyKeyReused` and `ErrIdempotencyPayloadMismatch` are untouched. A BEFORE trigger would fire before the conflict check (also under `ON CONFLICT`) and turn every replay of a committed adjustment into a fence error; mutant T19 pins this. Positive evidence is stronger than before: the idempotency and replay suites now run every REPLAY with the fence enabled (only the first post is exempted). |
| Concurrent final approvals post once | Pass (`TestLinkFence0153_ConcurrentFinalApprovalsPostOnce`, also under `-race`). Serialization is still the L1 request `FOR UPDATE`; the fence adds no lock. |
| Partial failure rolls back | Pass (`..._PartialFailureRollsBackAndRetryPostsOnce`: injected failure after `-> executing`, nothing posted, request pending, no `executed` audit; the retry posts once). |
| Compensating entry / over-compensation MA022 | Pass (`..._CompensatingEntryReversesExecutedCredit`). |
| Fence re-evaluation of `ledger_adjustment_execution_status` at posting vs at `-> executing` under READ COMMITTED | See finding L2. The trigger body is VOLATILE plpgsql, so its statements take a fresh snapshot, but `execution_status` is evaluated **as of `now()`** (policies `effective_from <= now()`, grants in force at `now()`), and staff + grant rows are held `FOR SHARE` by the executor (L1). I ran a throwaway probe (not committed): a tenant policy raising `required` 1 -> 2 committed by another session from `testHookBeforePost` (after `-> executing`, before `ledger.Post`). Result: the decision **executed** with required 1, while a fresh evaluation afterwards reported required 2. So the fence's recount equals the guard's for every time-versioned input; only non-versioned inputs (tenant / brand status, licence-derived jurisdiction, player person link) can differ between the two evaluations, and any divergence makes the fence refuse (MA050 -> `ErrClassConflict`, whole decision rolled back, request stays pending). Fail closed; no money risk; consistent with the existing "tightened after submission" semantics in `policy_integration_test.go`. |
| Lock order ADR 0082 unchanged | Agree. Both functions issue plain SELECTs (no `FOR UPDATE/SHARE`), call only STABLE functions, and read the L1-held request row and immutable ledger rows. The lock-order suites (incl. the ABBA racer and `-race`) pass. |
| SUM(debits) = SUM(credits) | Held: `assertInvariants` closes every new money test; `ledger_entries_balanced` (deferred) is unchanged; the entry fence additionally limits a manual_adjustment to one leg per direction with the request's amount. A global cross-tenant SQL check on the private DB returned zero visible rows because RLS binds the reviewer roles too (by design; not bypassed). |
| No balance UPDATE, no float, append-only | Confirmed. 0153 contains only `CREATE OR REPLACE FUNCTION`, `DROP/CREATE TRIGGER`; no DML. `ledger_transactions_immutable` / `ledger_entries_immutable` (BEFORE UPDATE/DELETE, deny) mean an INSERT-only fence is complete: a row can never become a manual_adjustment, and an entry can never be re-parented onto one, after insert. |
| Reconciliation kind `ledger_unlinked_manual_adjustment` still fires | Pass. B-25 now asserts the rogue posting is refused (MA050, leaves no mismatch) and that the detector still raises an owner-planted rogue (the ADR 0110 §9.4 owner residual) on every run. Reconciliation suite passes. |

## 3. Fixture sites: substitutions and assertion audit

I diffed every `*_test.go` against `73b2836` and inspected every removed line. **No assertion was removed or
relaxed.** Each removed check is still asserted, either unchanged or behind the owner residual, and new
assertions were added:

- **B-12** (`TestB12_ShapeAndForgedLinkRefused`): each of the 4 forged shapes now first asserts the new fence code
  (MA051 x3 / MA050 for wrong keys) with the request still pending. The original MA040 assertion is kept
  byte-for-byte on a second request, with the posting made through `withoutLinkFence`. Equivalent to the old
  proof plus a stronger one.
- **B-25**: the old "rogue posts, detector raises it" becomes "rogue refused (MA050), a refused rogue leaves no
  mismatch", and then "owner-planted rogue is raised on every run". Nothing weakened.
- **K2-R1**: new loop asserts MA050 for governed-key postings on a pending request (with and without a
  same-transaction approval). The three MA040 exits are kept; the executing case runs **without** the exemption
  (proves the fence admits the approved shape); only the two pending cases plant the posting via the owner
  residual, which is required because the fence now refuses them first.
- **TempRevoke shadow attack**: new assertion that the fence alone refuses the TEMP-shadow forgery (MA050, no
  posting, cash unchanged); 0116's non-vacuity is still proven with the 0153 triggers also disabled. The
  `ALTER TABLE` runs on the test's **scratch database** (`owner` = scratch pool), not the shared DB; verified.
- **Sportsbook `TestTenantNotActive_DatabaseBackstopRefusesGameplayTypesOnly`**: legitimate, and the original
  claim survives for the manual_adjustment type too. `ledger_gameplay_tenant_active_guard` (0118, GP010) is a
  **BEFORE** INSERT trigger, so it fires before the AFTER constraint trigger; the new assertion "the ungoverned
  manual_adjustment on the closed tenant fails with MA050, not GP010" therefore still proves the gameplay gate
  does not cover `manual_adjustment`, and the `deposit` probe proves a non-gameplay posting succeeds on a closed
  tenant from the runtime role. Nothing weakened.
- **Lock-order racers** (`TestLockOrder_ConcurrentFirstPostingsCreateSameAccounts_NoDeadlock`, `bonus_forfeiture`):
  legitimate. Verified: no migration defines a trigger or condition specific to `bonus_forfeiture` other than the
  `ledger_transactions_check1` reason-code equality (satisfied, same as manual_adjustment); the only Go branch is
  the shared reason-code validation at `internal/ledger/lockorder.go:364`; the racers' entries touch no BONUS_SET
  account, so the Rule B2 mirror generator adds no legs; no advisory lock applies. The lock set and order are
  identical, so the ABBA proof is unchanged. The owner exemption is correctly avoided there (its SHARE ROW
  EXCLUSIVE table lock would serialize the racers).
- **Owner-exemption sites** (casino, idempotency first posts, launchgov, ledger, reconciliation, risk, sportsbook,
  wallet): same `ledger.Post` call, same inputs, wrapped in `adjlinkfix.Without`. None of these tests is about a
  manual adjustment; they need funded wallets or an arbitrary balanced shape. `Without` fires the ledger tables'
  own deferrable constraints early (never skips them) and re-enables the triggers in the same transaction.
- **Production callers:** `internal/adjustment/execute.go:356` (`buildPosting`) remains the only non-test
  `TxManualAdjustment` poster (B-21 kept and passing), so the fence cannot break a production path.

## 4. Rows posted before 0153

Agree with residual §21.5 item 4. The fence is INSERT-only and the ledger is immutable, so existing rows are
neither re-validated nor at risk of change; the entry fence also guarantees no entry can ever be appended to a
pre-0153 manual_adjustment (its request is never "executing in this transaction"). The §12 detector covers rows
since `governed_since`. No real-money environment exists yet, so no backfill is needed now. See I2 for the
operational step to add before 0153 is applied to any environment holding real ledger data.

## 5. Equivalent mutants

The record and ADR §21.6 name five survivors; the table and the author's own results log list **six**: T06,
T07, E01, E08, E09, E18 (see L1). I agree all six are equivalent and demand no new tests:

- **T06 / T07** (one provider arm off): `CHECK ((provider_id IS NULL) = (provider_tx_id IS NULL))` (0021) makes
  each arm alone sufficient. I re-ran **T06b** (both arms off) independently on a private DB: killed by
  `TestLinkFence0153_ExecutingTxAdmitsOnlyTheApprovedShape` ("provider reference set: want MA050, got nil"),
  so the pair is covered.
- **E01** (parent not-found arm off): the composite FK `(ledger_transaction_id, tenant_id)` plus identical
  INSERT/SELECT RLS on both ledger tables make it unreachable; and if reached, the NULL record skips the type
  gate and the request lookup fails closed with MA051 anyway.
- **E08** (request tenant condition off): request ids are unique; the request row was already bound to the
  transaction's tenant by the transaction fence, and RLS limits visible requests to the entry's tenant.
- **E09 / E18** (account not-found arm off; COALESCE removed): each is covered by the other plus the composite
  account FK. Note they are **mutually covering** (removing both would let a NULL account pass, were it
  reachable): keep both, as the author does.

## 6. Findings

| Id | Severity | Finding | Repro | Fix |
|---|---|---|---|---|
| L1 | Low | Mutation tally misstated. ADR 0100 §21.6 and the record say "44 mutants: 39 killed, 5 survived". The listed survivors are six (T06, T07, E01, E08, E09, E18), so the tally is 38 killed / 6 survived. The author's results log has 43 entries (37 killed, 6 survived); T06b is in the driver but not in the log (I re-ran it: killed). No mutation driver or log was committed, unlike previous PRH-2 rounds. | Count the §21.6 table; compare with the scratchpad's `a4_mutation_results.txt`. | Correct §21.6 and the record to 44 / 38 / 6. Commit the driver and the kill log (with T06b) under `docs/plans/prh2-hardening-round/`, as for adr0113/0115. |
| L2 | Low | ADR §21.2 / migration header (d) say the count is "re-evaluated at the posting" and "re-checks ... rather than trusting the state flag". This is true, but it is the same **as-of `now()`** evaluation as the guard, so a reader may assume the fence catches a policy tightening or grant revocation committed while the decision is in flight. It does not (probe in §2: executed with required 1 while a fresh evaluation reported 2). Not a regression and no money risk: same semantics as 0113 and the existing "tightened after submission" tests. | Throwaway test: in `testHookBeforePost`, `w.approvePolicy(LevelTenant, BaseRequiredApprovals 2)`; `w.decide` executes. | Add one sentence to §21.2: the recount is as of the deciding transaction's `now()`; only non-time-versioned inputs can differ, and a divergence fails closed (MA050 -> Conflict, full rollback). Optional: pin it with a deterministic `testHookBeforePost` test. |
| L3 | Low | The exemption inventory regex (`link_fence_static_test.go:20`) matches only `adjlinkfix.Without(`, `adjlinkpost.Post(`, `mustPostFixture(`, `withoutLinkFence(`. Raw `ALTER TABLE ... DISABLE TRIGGER ledger_*_manual_adjustment_link` in test files is not inventoried (today: `link_fence_0153_integration_test.go`, savepoint/transaction-scoped; `temp_revoke_integration_test.go`, scratch DB; both legitimate). A future test could disable the fence by raw SQL on the shared test DB without failing the inventory. | `grep -rn "DISABLE TRIGGER ledger_.*manual_adjustment_link" internal --include=*_test.go` | Extend the regex to the two trigger names in a DISABLE context and add these two files to the inventory with justifications. |
| I1 | Info | The entry trigger runs for every ledger entry of every type (one PK lookup of the parent per entry, on the bet/settlement hot path). Acceptable and stated in §21.2; include it in the next load-test budget. | n/a | None now. |
| I2 | Info | Pre-0153 rows (§4). | n/a | Add a runbook step: before 0153 is applied to any environment holding real ledger data, run `ledger_vs_projection` for every tenant and require zero (or individually classified) `ledger_unlinked_manual_adjustment`; after 0153 any new one implies owner DDL and is a P1. |
| I3 | Info | Residual §21.5 item 3: the tenant `manual_adjustment` house account is not fenced against other transaction types. Agree it is ADR 0110 T5 and out of scope. | n/a | Record as a deferred consideration (a fence binding that account type to manual_adjustment transactions), not built now. |

## 7. Sign-off

Financial correctness of 0153 and the fixture migration: **approved**. Governed path, idempotency contract,
concurrency, partial-failure rollback, compensation cap, lock order, double-entry balance, append-only ledger
and the reconciliation backstop are preserved; no assertion was weakened; both type substitutions are
legitimate. Fix L1 before merge (it is a factual claim in an ADR); L2 and L3 may follow in the same change or
as a tracked follow-up. Security review remains a separate, required gate.
