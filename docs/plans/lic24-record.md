# lic24 record: migration tests that reach the withdrawal restriction gate on a pre-0182 schema

Branch `lic24-payments-migration-tests` from integ-c `46a9c76`. No migration. Author: `ledger-finance`. Status: IMPLEMENTED (test
infrastructure only; production behaviour unchanged and fail-closed).

## Problem

`internal/payments` `TestR32_Migration0127_BackfillsExistingDestinationParks` migrates its scratch database DOWN to 0126 and builds
its fixture payouts through the production request and payout-claim path (`m.payout`). Since lic11 (0182) that path evaluates
the withdrawal restriction policy, which fails closed on a schema without it (`withdrawal_restriction_set_version(unknown) does not
exist`, 42883). The lic17 test-only context marker (`markPre0182Schema`) cannot be used from another package: its key is
unexported.

## Fix

- `internal/withdrawal/restriction_schema_hook_integration.go` (NEW, first line `//go:build integration`) exports
  `SetPre0182SchemaForTest() (restore func())`. It follows the precedent of `internal/kyc/hold_hook_integration.go`
  (`SetHoldEvaluatorForTest`), which this same test already uses for the same reason (the AML hold on a pre-0141 schema).
  - The file exists ONLY in integration builds, so no production binary contains the setter. Production cannot enable the
    hook: in a normal build nothing can write `pre0182SchemaHook`, so the hook is constantly false.
- `internal/withdrawal/restriction.go`: `pre0182SchemaForTest(ctx)` now returns true when the lic17 context marker is set OR
  the integration-only hook is set. When true, the decision is the pre-0182 behaviour: no evaluation, allow, and no provenance,
  so `RecordDispatchGateDecision` names the pre-0182 columns, which matches the test's 0169 stub table. Otherwise production
  is unchanged: a missing function is an error (fail closed). There is no schema probe in production.
- `internal/payments/m4_integrity_review_integration_test.go`: the hook is switched on right after the down to 0126 (beside
  the existing kyc hook), registered with `t.Cleanup`, and restored EXPLICITLY before the `MigrateUp` to head. So everything
  after the up runs the full production gate. No assertion changed; the test still proves the 0127 backfill
  (`unknown_pre_0127/migration_0127` evidence and the not-paid refusal).
- Static pin `TestLic24_Static_Pre0182HookIsIntegrationOnly` (a plain unit test). Across every non-`_test.go` file in the
  repository:
  - the setter is defined exactly once, in the withdrawal hook file, which must start with `//go:build integration`;
  - no other non-test file names the setter or writes the hook;
  - the hook is read exactly once, in `restriction.go`.
  The lic17 pin `TestLic17_Static_Pre0182MarkerIsTestOnly` is unchanged.

The coordinator preferred seeding the rows with SQL fixtures instead. I did not do that here: the test needs genuinely parked
payout attempts (request, claim, provider callback, dispute) created by the code paths whose pre-0127 rows the migration
backfills. Hand-seeding them would stop proving the backfill against rows the real writers produce. The hook keeps the fixture
faithful and limits the bypass to integration builds and to the pre-up phase of this one test.

## Search for other cases

Candidates: every integration test file that migrates to a version (`MigrateUp` / `MigrateDown` / `scratchThroughVersion`
/ `m132Dir`), crossed with the packages whose tests reach the production withdrawal path (`RequestWithdrawal`,
`ClaimForDispatch`, `EvaluateDispatchGate`, `EvaluateRestriction`, sweeper reclaim / resubmit, `wdpath`, `approvedWithdrawal`).
Those packages are: payments, httpserver, tenant, payoutinstrument, launchgov, reconciliation, alerting, casino, risk, wallet,
licensingchange, withdrawalpolicy (plus withdrawal itself, fixed in lic17).

I ran the old-schema-capable tests of every one of those packages on the unfixed code. The payments subset was
`-run 'Migration|^TestR32|^TestM4|^TestHSEC|^TestK3|^TestRR|^TestA5|^TestR31'`. The others used
`-run 'Migration|Ratify|MA020|INVDEP1|LaunchGov|BrandsGrantPin|SportsbookCumulative|PaymentStatement|SRF2R'`. licensingchange,
withdrawalpolicy, withdrawal, kyc and aml ran with `-run 'Migration|Lic1[17]|Lic24'`. Result: the ONLY failure with the
restriction error was `TestR32_Migration0127_BackfillsExistingDestinationParks`. No other old-schema test reaches the gate.

## Results

- `TestR32_Migration0127_BackfillsExistingDestinationParks`: PASS. `TestR32_Migration0127UpDownUp_WholeSchema` and
  `TestR32_Migration0127DownRefusals`: PASS.
- payments subset above (unfixed code): every other test PASS.
- httpserver, tenant, payoutinstrument, launchgov, reconciliation, alerting, casino, risk (subsets above): PASS.
- licensingchange, withdrawalpolicy, withdrawal, kyc, aml (`Migration|Lic1[17]|Lic24`): PASS.
- gofmt is clean. `go vet ./...` and `go vet -tags integration ./...` are clean. `go test ./...` passes, as does
  `go test ./internal/db`. The production binary (`cmd/platform-api`, no tags) builds.
- I did not run the full payments package (about 50-60 minutes; the coordinator runs it).
