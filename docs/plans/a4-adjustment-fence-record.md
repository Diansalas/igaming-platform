# A4 record: MANUAL-ADJ-LINK-1 (migration 0153)

Implementation record for audit item A4 (`docs/plans/platform-completion-audit-2026-10-14.md`), written by
`ledger-finance` on the A4 worktree branch. The orchestrator is the single writer of
`docs/governance/task-registry.md`, `HANDOVER`, `docs/progress.md` and `docs/active-stage.md`; this file
only **proposes** their text. The design, threat model and residuals are in ADR 0100 §21.

## Label

**IMPLEMENTED** on the unmerged A4 branch: migration 0153 (up and down), tests, ADR 0100 §21.
**Not yet:** `security` review (required: security-sensitive), independent `ledger-finance` / `code-reviewer`
review, hosted CI. It is not launch-cleared until those are done.

**Merge dependency:** 0153 follows 0151 on this branch; 0152 belongs to parallel work. Until 0152 merges,
`go run ./cmd/migrate verify` reports a version GAP at 0152 (CI's `migrate verify` step, and the two
integration tests that call `VerifyMigrations` and tolerate only their own named gaps:
`internal/reconciliation/ma020_parity_integration_test.go`, `internal/alerting/migration_0117_integration_test.go`).
Merge 0152 first, or renumber, before hosted CI.

## Proposed registry text

Replace the MANUAL-ADJ-LINK-1 row's status cell with:

> **IMPLEMENTED 2026-10-15 (A4, branch `worktree-agent-aae3ab5308d231dfa`, migration 0153, ADR 0100 §21); NOT
> MERGED; reviews pending (`security` required).** Two NOT DEFERRABLE AFTER INSERT constraint triggers, for every
> session and role (no GUC, role or environment condition; no SECURITY DEFINER; search_path pinned):
> `ledger_transactions_manual_adjustment_link` (MA050: the row must be the posting of a request visible to the
> session, executing in THIS transaction, payload hash intact, still counted as approved by
> `ledger_adjustment_execution_status()`, keys equal) and `ledger_entries_manual_adjustment_link` (MA051: every
> entry of a manual_adjustment is one leg of the closed §4 shape of the request executing in this transaction,
> one per direction). AFTER, not BEFORE, so replays keep the ADR 0020 idempotency contract. ADR 0082 lock order
> unchanged. Fixtures: 32 ungoverned `TxManualAdjustment` references in 20 files (the "19 files"; 4 needed no change: tenant/migration_0136, ledger_integration L247, lockorder_canonical_order, lockorder_multipost) moved to the owner-only fixture exemption
> (`internal/testsupport/adjlinkfix`: the existing ADR 0110 §9.4 owner DISABLE TRIGGER residual, integration-only,
> inventoried by a static test) or, where a trigger disable is impossible (a lock-order race; a runtime-role
> probe), to another non-governed type; no assertion weakened or deleted. Mutation pass: see
> `docs/plans/a4-adjustment-fence-record.md`. Residuals: owner/superuser DDL (detector `ledger_unlinked_manual_
> adjustment` kept as backstop), ADR 0110 T5 non-governed posting types, pre-0153 rows. B-21 kept.

Related rows to annotate (no status change proposed):

- LEDGER-MANUAL-ADJ-4EYES-1: "Not done; launch-blocking: LEDGER-MANUAL-ADJ-LINK-1" -> "LINK-1 IMPLEMENTED on the
  A4 branch (0153), pending review/merge".
- The I-wire P1 routing of `ledger_unlinked_manual_adjustment` stays required: the detector is now the backstop
  for the owner-DDL residual, not the primary control.

## What changed

| Area | Change |
|---|---|
| `migrations/0153_manual_adjustment_link_fence.{up,down}.sql` | The fence (2 functions, 2 constraint triggers); idempotent; down drops them, touches no row |
| `internal/testsupport/adjlinkfix` (new, integration-only) | `Without(ctx, tx, fn)`: disables exactly the two 0153 triggers in the caller's owner tx, re-enables in the same tx; fires only the two ledger tables' deferrable checks early (never another table's) so ALTER TABLE is legal; no-op below 0153 |
| `internal/testsupport/adjlinkpost` (new, integration-only) | `Post` = `adjlinkfix.Without` around one `ledger.Post` (separate package: `internal/ledger`'s own tests import adjlinkfix) |
| `internal/tenant/no_trigger_disable_test.go` | adjlinkfix added to the allowlist of integration-only trigger-disable fixtures (with launchfix, staffsv, pitest) |
| `internal/adjustment/link_fence_0153_integration_test.go` (new) | The fence tests (below) |
| `internal/adjustment/migration_0153_integration_test.go` (new) | Up/down/up whole schema + data |
| `internal/adjustment/link_fence_static_test.go` (new) | Exemption caller inventory; names match the migration; no bypass tokens in the SQL |
| ADR 0100 §21 (+ pointers in §12, §16, §20.4) | Decision, threat model, exemption inventory, residuals |

## Fixture disposition (every file)

| File | Sites | Disposition |
|---|---|---|
| casino/bonus_settlement_integration_test.go | 2 | owner exemption (`adjlinkpost.Post`) |
| casino/orchestrator_integration_test.go | 1 | owner exemption |
| idempotency/integration_test.go | 1 (4 first posts) | owner exemption for the FIRST post only (`mustPostFixture`); every replay still runs with the fence enabled |
| launchgov/funds_integration_test.go | 1 | owner exemption |
| ledger/bonus_mirror_integration_test.go | 5 | owner exemption (`mustPostFixture`, MA type only) |
| ledger/locked_origin_split_integration_test.go | 4 posted (+1 Go-validation-only, unchanged) | owner exemption |
| ledger/replay_integration_test.go | 1 | owner exemption for the original; replays unexempted |
| ledger/lockorder_integration_test.go | 2 | type -> `bonus_forfeiture` (a disable would serialize the ABBA racers); inputs otherwise identical |
| ledger/ledger_integration_test.go L247, lockorder_multipost, lockorder_canonical_order | 4 | unchanged (conflict / pre-insert paths never reach the fence) |
| reconciliation/casino_consistency_integration_test.go | 1 | owner exemption |
| reconciliation/sportsbook_settlement_integration_test.go | 1 | owner exemption |
| reconciliation/unlinked_manual_adjustment_helpers_integration_test.go | 0 | comment updated (tolerance still needed: fixtures still plant unlinked postings) |
| risk/cumulative_leg_integration_test.go | 1 | owner exemption |
| risk/cumulative_sportsbook_settlement_integration_test.go | 1 | owner exemption |
| sportsbook/orchestrator_integration_test.go | 1 | owner exemption |
| sportsbook/settlement_assertions_integration_test.go | 1 | owner exemption |
| sportsbook/tenant_not_active_integration_test.go | 1 | runtime role: probe -> `deposit` + NEW assertion that the ungoverned MA is MA050 (not GP010) |
| tenant/migration_0136_integration_test.go | 2 | unchanged (scratch schema below 0153) |
| wallet/locked_origin_summary_integration_test.go | 1 | owner exemption |
| adjustment/b11_b17 (B-12) | 4 forges | NEW fence assertions (MA051/MA050) + original MA040 kept, proven with the fence absent |
| adjustment/b23_b25 (B-25) | 1 | NEW MA050 assertion + detector still proven on an owner-planted rogue posting |
| adjustment/k2_security_conditions (K2-R1) | 3 | NEW MA050 assertions; MA040 exits kept (posting planted via the owner residual for the 2 pending cases) |
| adjustment/temp_revoke (K2 shadow attack) | 1 | NEW assertion: the fence refuses the TEMP forgery with proof triggers off; 0116 non-vacuity kept (fence also disabled on the scratch DB) |

## Changed assertions (complete list, with justification)

1. **B-12** (`TestB12_ShapeAndForgedLinkRefused`): the 4 forged-shape cases expected MA040 from the link
   UPDATE. They now first assert the new, earlier refusal (MA051 / MA050 at the posting) and then re-assert the
   original MA040 on a second request with the posting made under the owner residual. Nothing removed.
2. **B-25** (`TestB25_B18_...`): the rogue posting used to succeed; it now must be MA050, a refused rogue must
   leave no mismatch, and the original "exactly one ledger_unlinked_manual_adjustment per run" assertion is
   kept on a rogue planted through the owner residual.
3. **K2-R1**: the pending-state postings now must be MA050 (new loop); the original MA040 exit assertions are
   kept (posting planted via the owner residual for the two pending cases; the executing case unchanged and
   un-exempted).
4. **TempRevoke shadow attack**: the step "proof triggers disabled -> forgery executes" is now preceded by
   "-> the 0153 fence refuses it (MA050), nothing moves"; the original non-vacuity assertion is kept with the
   0153 triggers also disabled on that scratch DB (the test's own pattern for the proof triggers).
5. **Sportsbook closed tenant**: "a non-gameplay posting on a closed tenant posts" is kept with a `deposit`
   (the ungoverned `manual_adjustment` it used is now illegal for that runtime session); NEW: that MA is
   refused with MA050, not GP010. This is the one assertion whose *subject* changed (MA -> deposit); its
   claim (the gameplay gate is scoped to gameplay types) is unchanged.
6. **Lock-order racers**: transaction type only (manual_adjustment -> bonus_forfeiture); every assertion
   unchanged.

## Test evidence (private DB rebuilt from migrations by `priv_db.sh`, `priv_test.sh`; local, NOT CI)

| Command | Result |
|---|---|
| `gofmt -l ./internal ./cmd ./deploy` | clean |
| `go vet ./...` and `go vet -tags integration ./...` | clean |
| `golangci-lint run ./...` (2.9.0) | 0 issues |
| `golangci-lint run --build-tags integration <touched pkgs>` | 1 pre-existing finding in an untouched file (`internal/ledger/lockorder_harness_test.go:214` `loHoldWithRollback` unused) |
| `go test -count=1 ./...` (unit) | PASS |
| `-tags integration -count=1 -p 1` ledger, adjustment, reconciliation, db | PASS |
| `-tags integration -count=1 -p 1` casino, sportsbook, risk, wallet, idempotency, launchgov, tenant | PASS |
| `-tags integration -count=1 -p 1` aml, withdrawal, testsupport/... | PASS |
| `-tags integration -run 'ManualAdjustment\|Adjustment\|StaffIdentityRedaction\|StaffReadRoutes\|OpenAPIGoverned' ./internal/httpserver/` | PASS |
| `-tags integration -run '^Test[X]' ./internal/payments/`, X = A..Z | PASS for 23 letters; H, K, R exceeded go test's default 10m timeout (no test had failed) and were rerun with `-timeout 150m -v` (CI uses 170m): H 79 PASS, K 100 PASS, R 213 PASS, 0 FAIL, 0 SKIP |
| `-tags integration -race -count=1 -p 1` adjustment, ledger | PASS |
| `go run ./cmd/migrate verify` | FAIL only on `GAP missing migration version 152` (expected until 0152 merges) |

## Mutation pass (kill-or-justify)

44 mutants of 0153 applied to the live private schema: 39 killed, 5 survived, all 5 equivalent and
justified. Table: ADR 0100 §21.6.
