# Sportsbook adapter: conformance test specification

Status: **SPECIFICATION (`NOT IMPLEMENTED`).** **Sportsbook is DEFERRED** until a sportsbook brand is scheduled (owner
decision 2, 2026-10-15). No test is written by this package. A real adapter is not accepted until every applicable case
below passes against (1) the adapter's recorded-fixture double and (2) the vendor sandbox, with the evidence in
`integration-gates.md` gates S3 and S6. Provider-agnostic. Case ids `SB-C-nn` are new; existing test names are cited as
they exist at base `4a3c85f`.

Note on shape: unlike payments and casino there is **no neutral conformance suite for a sportsbook provider** because
the `sportsbook.Provider` interface is `Catalogue` only (`internal/sportsbook/types.go`). The first sportsbook adapter
must bring its own suite in the same style as `casino.RunProviderConformanceSuite` once SB-G01 fixes the interface; the
cases in s3 are that suite's specification.

Rules for the suite author:

- A case that cannot run for a real adapter must **fail**, never skip (precedent in `internal/casino/conformance_skip_to_fail_test.go`).
- Vendor behaviour is taken from vendor-documented examples or sandbox captures with a citation. A test must not encode
  an assumed vendor behaviour (`U-SB-nn` items stay open until cited).
- Fixtures contain synthetic data only. Money tests end with the ledger invariants (SUM(D)=SUM(C), projection parity,
  `sportsbook_settlement` stream clean) as the existing integration tests do.

## 1. Existing suites a real adapter must run unchanged

| Suite | Where | Applies as | Work to make it runnable for a real adapter |
|---|---|---|---|
| Catalogue fetch/sync boundary | `internal/sportsbook/catalogue_io_boundary_integration_test.go`, `catalogue_write_authorization_integration_test.go`, `provider_reference_bound_test.go`, `provider_reference_bound_integration_test.go`, `provider_reference_integration_test.go`, `jurisdiction_catalogue_integration_test.go` | mandatory | The adapter is substitutable for `MockSportsbookProvider` at `FetchCatalogue`; `FetchCatalogue` refuses under `txscope.Held`; one bad `ExternalRef` rejects the whole fetch; `SyncCatalogue` needs the platform-service scope |
| PlaceBet and its gates (vendor independent) | `orchestrator_integration_test.go`, `failure_mode_matrix_integration_test.go`, `placebet_replay_f7_integration_test.go`, `market_guard_integration_test.go`, `jurisdiction_integration_test.go`, `kyc_play_integration_test.go`, `kyc_play_outage_integration_test.go`, `aml_hold_integration_test.go`, `rg_enforcement_s4_integration_test.go`, `exposure_integration_test.go`, `exposure_lockorder_integration_test.go`, `tenant_not_active_integration_test.go`, `brand_gameplay_gate_integration_test.go`, `internal/httpserver/sportsbook_flow_integration_test.go`, `sportsbook_rejection_collapse_test.go`, `stage6_b2c_sportsbook_acceptance_test.go` | run unchanged | Re-run after the adapter lands; no vendor fixtures needed unless SB-G01 puts the vendor in the placement path (then SB-C-10..13) |
| Settlement lifecycle (vendor independent) | `settlement_decision_table_integration_test.go`, `settlement_scenarios_integration_test.go`, `settlement_concurrency_integration_test.go`, `settlement_db_constraints_integration_test.go`, `settlement_fault_injection_integration_test.go`, `settlement_assertions_integration_test.go`, `settlement_rejection_audit_integration_test.go`, `settlement_composed_void_savepoint_integration_test.go`, `settlement_migration_0091_integration_test.go`, `settlement_migration_0093_integration_test.go`, `settlement_ob1_integration_test.go`, `settlement_sql_branches_integration_test.go`, `settlement_readpath_integration_test.go`, `sportsbook_settlement_sole_writer_test.go`, `settlement_actor_role_test.go`, `internal/httpserver/sportsbook_settlement_*_test.go` | run unchanged | These pin the XMIN same-transaction rule (migration 0093), idempotency by `(bet, generation)`, anti-minting, tombstone, composed void, sole-writer rule. A provider-driven driver must go through the same `SimulateSettlementEvent`-equivalent entry point (never a second writer) |
| Closure and brand backstops | `tenant_closure_integration_test.go`, `tenant_closure_runtime_guard_integration_test.go`, `brand_closure_backstop_integration_test.go`, `brand_closure_lf9_integration_test.go`, `brand_gate_closed_pre0130_integration_test.go`, `stake_return_integration_test.go` | run unchanged | n/a |
| Reconciliation | `internal/reconciliation/sportsbook_settlement_integration_test.go`, `sportsbook_settlement_dedup_test.go`, `internal/sportsbook` mock statement source | mandatory | The real source must pass the same detection tests with divergent statements; SB-C-40..47 |
| Generic HTTP client boundary | `internal/providers/httpclient/client_test.go` and `conformance/conformance.go` (success, timeout, transport failure, 4xx/5xx, malformed body, flaky-then-succeeds, duplicate-call observation) | mandatory for the adapter's outbound calls | Point the harness at a fake server shaped like the documented contract |
| Production guard and tripwire | `internal/providerkind` completeness scan, `cmd/platform-api/outbound_precondition_test.go`, `cmd/platform-api/no_mock_binaries_test.go` | mandatory | tripwire relaxation scoped to this adapter only; `ProductionEligible` only at S9 |
| Test-support settlement route | `internal/httpserver/sportsbook_settlement_handlers.go` tests; registered only outside production | run unchanged | production must never register it; SB-C-50 |

## 2. Binding rows (known open items to close or accept before the first real sportsbook adapter)

States as recorded in the cited documents at base `4a3c85f`; re-verify at gate S0.

| Row | Source | Condition | Case |
|---|---|---|---|
| OQ-SB-01 operating model | this package | decided | gate S0 |
| Provider-mode idempotency and provider references (s I; R-1) | ADR 0088 s1.2 | SB-G02 designed and built for a provider-driven path | SB-C-20..24 |
| Webhook | ADR 0088 s1.2 | SB-G04 designed (scheme, admission, retry semantics, tenant binding) | SB-C-30..34 |
| Cashout, partial settlement, accumulators, live odds | ADR 0087/0088 | out of scope unless the first brand needs them (OQ-SB-03) | none by default |
| Bonus/mixed-funded settlement (HR-9) | ADR 0038 s9 | BLOCKED; first brand cash-only | SB-C-14 |
| Self-exclusion auto-void consumer (R-2) | ADR 0042 / ADR 0034 s14.7 | decision or accepted gap | SB-C-15 |
| OB-1 | ADR 0088 s2.5 | still open: recorded decision | gate S5 evidence |
| HDR-SB-1 exposure arming | ADR 0083 | arming policy decided if a book is held | SB-C-16 |
| Liability reporting | doc 09 s9 | built (SB-G08) | SB-C-60 |
| `NUMERIC(38,0)` widening debt | ADR 0088 s3.6 | accepted for fiat | none |
| ALERT-DELIVERY-1 | readiness report | production blocker | gate S8 |

## 3. New cases

### 3.1 Adapter and acceptance (needs SB-G01 decided)

| Id | Case | Needs DB |
|---|---|---|
| SB-C-01 | Fixture-hook design for the sportsbook suite (signed inbound builder, failure hook) from day one; no `*Mock...` type assertions | no |
| SB-C-02 | Catalogue mapping fixtures: each documented market type maps to a `Market` + fixed `Selection`s or is explicitly dropped with a reason; odds formats convert exactly (numerator/denominator), a non-exact odd is refused (U-SB-05/06) | no |
| SB-C-03 | Catalogue bounds: every `ExternalRef` within 1..255 bytes, UTF-8, no control characters; statuses map to the closed enums; a vendor status outside them is refused, never defaulted open | no |
| SB-C-10 | Vendor-accepted bet (only if the model chooses it): the gate chain (market, tenant, brand, jurisdiction, RG, Risk, KYC+AML, exposure) runs BEFORE the stake is committed at the vendor; a denial never reaches the vendor | yes |
| SB-C-11 | Acceptance outcomes: accepted, rejected, pending, ambiguous/timeout are distinguishable; ambiguous never debits twice nor releases the stake silently; a pending bet has a bounded lifetime and a defined resolution | yes |
| SB-C-12 | Placement idempotency across client retry and vendor retry: one ledger posting, one bet row, one vendor ticket; `provider_bet_reference` unique per tenant+provider | yes |
| SB-C-13 | Odds/price change at acceptance: vendor re-price vs platform `odds_changed` policy; no acceptance at stale odds beyond the configured policy | yes |
| SB-C-14 | Bonus or free-bet funded stakes are refused (BLOCKED) and a vendor-side free-bet token cannot create a platform bet | yes |
| SB-C-15 | Self-exclusion starts while bets are open: documented behaviour (no auto-void, or the built consumer) and settlement still pays/refunds correctly | yes |
| SB-C-16 | Exposure arming: with limits armed the vendor-accepted path respects the cross-player ceiling or the vendor carries its own | yes |

### 3.2 Settlement and idempotency (provider-driven path)

| Id | Case | Needs DB |
|---|---|---|
| SB-C-20 | Settlement fixture (won, lost) drives the same entry set as the in-house path (ADR 0088 s2.3); the posted payout equals the frozen `potential_return`; a fixture whose stated payout differs is `PAYOUT_INVALID` with an integrity alert and nothing posted | yes |
| SB-C-21 | Duplicate delivery of one provider settlement event: one posting, `replayed`; the provider event id/version is deduplicated (SB-G02) | yes |
| SB-C-22 | Correction: provider re-settles with a different outcome: rollback(g) + settle(g+1) in one transaction with the generation derived from the database, never from the payload; void after settlement composes; void is terminal and a later correction alerts for manual handling | yes |
| SB-C-23 | Rollback for a settlement never seen writes the tombstone; the late original is `SETTLEMENT_TOMBSTONED` | yes |
| SB-C-24 | Out-of-order and concurrent events for one bet (settle, rollback, void interleaved, 12 parallel): the L1 bet lock serializes; final state and ledger invariants hold; each bet inside its own SAVEPOINT in a batch does not trip the XMIN check (existing savepoint test with the adapter's batch driver) | yes |
| SB-C-25 | Market-wide result fan-out: tenants and open bets from the `tenants` table and bet rows only, never from the payload; a refused bet (inactive tenant) never blocks the others; non-active tenants' refusals are recorded | yes |
| SB-C-26 | Void reasons map to the closed set; an unknown vendor reason is refused or mapped by a documented rule, never defaulted | yes |
| SB-C-27 | Unsupported outcome shapes (dead heat, half-win, partial void, cashout) are refused with an alert and nothing posted, until built | yes |

### 3.3 Authentication (needs SB-G04 designed)

| Id | Case | Needs DB |
|---|---|---|
| SB-C-30 | Known-answer vector for the inbound scheme (SC1..SC13), added to `conformanceManifest` in the same change | no |
| SB-C-31 | Verify-first, uniform 401, tenant binding at MAC level, suspended tenant uniform 401 | yes |
| SB-C-32 | Retry semantics declared; neither-429-nor-503 refused | no |
| SB-C-33 | A bet-keyed event for a bet of another tenant or provider resolves to nothing (uniform response), never a cross-tenant effect | yes |
| SB-C-34 | Replay window and body bounds per the admission domain | no |

### 3.4 Reconciliation (the real statement source)

| Id | Case | Needs DB |
|---|---|---|
| SB-C-40 | The real source is a fetcher: it refuses under `txscope.Held`, fetches outside the run transaction, and cannot take the in-transaction renderer shape (SB-G07) | no |
| SB-C-41 | Statement fixture matches a ledger built from the corresponding settlement fixtures: clean; each divergence (missing bet, extra, outcome, payout, asset, void vs settled, generation) is a P1 mismatch, nothing auto-corrected | yes |
| SB-C-42 | Keys: provider bet reference maps to exactly one platform bet; an unmapped line is a finding, not ignored | yes |
| SB-C-43 | Coverage anchor present, validated and monotonic (as for casino); absent or stale fails closed | yes |
| SB-C-44 | A bet with no current state (open, or open after a rollback, or tombstone only) has no line and is not a false P1 | yes |
| SB-C-45 | Read-only statement credential refused for a bet/cancel/payout call (sandbox) | sandbox |
| SB-C-46 | Real mismatch kind registered (migration) and rendered by the admin read | yes |
| SB-C-47 | Registry: a configured provider with no eligible source is a failed, alerted run, never clean | yes |
| SB-C-50 | Production never registers the simulation route; any settlement row with `driver='simulation'` in production is a P1 (needs the driver column of the feed design) | yes |
| SB-C-60 | Open-liability line equals the sum of open `player_locked_cash` per asset and agrees with check (a) of the stream | yes |

## 4. Evidence required per case

- Unit/adapter cases: test name, fixture citation (URL + retrieval date or sandbox capture id), `go test` output.
- DB cases: run as the NOBYPASSRLS runtime role; ledger invariants at the end; `sportsbook_settlement` stream clean.
- Sandbox cases: run log with request/response ids, redacted; no real money, no real player data.
- A skipped mandatory case is a failure. A case blocked by an open `U-SB-nn` is recorded `BLOCKED` with the item id.
