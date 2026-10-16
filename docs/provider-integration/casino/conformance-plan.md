# Casino aggregator adapter: conformance test specification

Status: **SPECIFICATION (`NOT IMPLEMENTED`).** No test is written by this package. A real adapter is not accepted until
every case below passes against (1) the adapter's recorded-fixture double and (2) the vendor sandbox, with the evidence
in `integration-gates.md` gates C3 and C6. Provider-agnostic. Case ids `CAS-C-nn` are new; existing test names are cited
as they exist at base `4a3c85f`.

Rules for the suite author:

- A case that cannot run for a real adapter must **fail**, never skip (precedent:
  `internal/casino/conformance_skip_to_fail_test.go` and the `t.Fatalf(... a real adapter cannot skip it ...)` branches
  in `internal/casino/conformance_test.go`).
- Vendor behaviour is taken from vendor-documented examples or sandbox captures with a citation in the test (document URL
  + date, or sandbox capture id). A test must not encode an assumed vendor behaviour (CLAUDE.md "no invented provider
  behaviour"; `U-CAS-nn` items stay open until cited).
- Fixtures contain synthetic data only; no secret, no real player data, no real money.
- Financial tests end with the ledger invariants (SUM(D)=SUM(C), projection parity, `casino_consistency` clean) as the
  existing integration tests do.
- Where the core is vendor independent (posting, idempotency, tombstones, gates, reconciliation matcher), the existing
  integration suites already prove it against the MOCK and are **not duplicated** for the vendor. The vendor-specific
  risk is the translation layer: vendor wire -> `CallbackEvent`, `CallbackEvent`/result -> vendor wire, auth, outbound
  launch. The new cases concentrate there and prove the translated events drive the unchanged core.

## 1. Existing suites a real adapter must run unchanged

| Suite | Where | Applies as | Work to make it runnable for a real adapter |
|---|---|---|---|
| `RunProviderConformanceSuite(t, factory)`: 15 sub-tests (catalogue well-formed; launch succeeds / magic decline; launch requires `player_ref`; bet/win/rollback contract; decline vs ambiguous distinguishable; transport failure is an error; missing/invalid signature; per-tenant credential rejected for another tenant; malformed payload; redelivered payload parses identically; `player_ref` carried and raw account id refused; capabilities well formed; M-CAS-1; health) | `internal/casino/conformance_test.go` | mandatory | Six sub-tests (lines 173, 198, 239, 282, 305, 335) type-assert `*MockCasinoProvider` and `t.Fatalf` for anything else. **New work CAS-C-01**: a test-only fixture-hook interface (`SignedCallbackFixture`: build a vendor-correct signed inbound for {tenant, event type, outcome, amount, asset, round, tx id, original id, player ref, session id, timestamp}; `FailureFixture`: make the vendor transport fail) implemented by the adapter's test package; the suite uses it instead of the type assertion. The magic amounts/game ids of the mock (decline, ambiguous) need a sandbox-scenario map instead (CAS-C-02). The proof that converted cases fail rather than skip is `TestCasinoConformance_W1a_SkipToFail_NonMockAdapterFailsNotSkips`; it must keep passing |
| `webhookauthtest.RunSchemeConformance` SC1..SC13 with a **vendor known-answer vector** | `internal/webhookauth/webhookauthtest` (`conformance_test.go`, `registry_test.go`) | mandatory; scheme name added to `conformanceManifest` (`webhookauth/scheme.go:341`) in the same change | Known-answer vector from a vendor-documented example or sandbox capture. SC7 timestamp window is fail-not-skip for non-synthetic; SC8 signed-account binding if declared. `TestEveryRegisteredNonSyntheticSchemeRunsConformance` fails if the fixture is missing. Subject to PSP G-01/G-02 (see `../psp/provider-neutral-mapping.md` s12) |
| Casino scheme and retry semantics | `internal/casino/webhook_scheme_test.go`, `internal/webhookauth/retry_semantics_test.go` | mandatory | Retry semantics declared from the vendor's documented policy; neither-429-nor-503 refused |
| M-CAS-1: `SupportsBet` requires `SupportsWin` and `SupportsRollback` | `ValidateAdapterCapabilityDeclaration`, `TestCasinoConformance_BetWithoutRollback_FailsSuite`, migration 0094 CHECK | mandatory | adapter declares truthfully |
| Core posting and idempotency, against the unchanged core | `orchestrator_integration_test.go`, `failure_mode_matrix_integration_test.go`, `rollback_partial_failure_integration_test.go`, `tamper_matrix_reason_integration_test.go`, `unknown_event_type_integration_test.go`, `replay_f7_integration_test.go`, `a3_win_idemp_locked_integration_test.go`, `provider_reference_bound_integration_test.go`, `stage9_concurrency_integration_test.go`, `lockorder_integration_test.go` | run unchanged (vendor independent) | Re-run after the adapter lands to prove no core regression; **no vendor fixtures needed** |
| Player reference and session binding | `a3_player_ref_integration_test.go`, `a3_sr3_integration_test.go`, `a3_preadapter_gates_integration_test.go`, `bootstrap_*_integration_test.go`, `internal/httpserver/casino_bootstrap_integration_test.go` | run unchanged; then CAS-C-10..13 for the vendor shape | the bootstrap tests pin the PLATFORM contract; a vendor that cannot speak it needs the CAS-G01 hook and CAS-C-11 |
| Gates on launch and bet | `launch_market_guard_integration_test.go`, `jurisdiction_blocklist_integration_test.go`, `rg_enforcement_integration_test.go`, `kyc_play_integration_test.go`, `aml_hold_integration_test.go`, `brand_gameplay_gate_integration_test.go`, `tenant_not_active_integration_test.go`, `a3_preadapter_gates_integration_test.go` (game kill) | run unchanged | n/a; CAS-C-30..33 prove the same gates fire for a vendor-originated event |
| Tombstone and out-of-order | `provider_rounds_test.go`, `rejections_integration_test.go` (`TestCallbackRejection_E10_*`, `_E9_*`), `rejections_test.go` (`TestRejectionClassFor_EveryClassAndNothingElse`), `internal/httpserver/casino_e3_e10_audit_integration_test.go` | run unchanged | CAS-C-20..22 drive the vendor-shaped rollback-before-original |
| Bonus-funded settlement | `bonus_settlement_integration_test.go`, `wagering_contribution_integration_test.go`, `internal/httpserver/casino_multibet_win_prefix_test.go` | run unchanged | CAS-C-26 for the CAS-G03/G04 refusal behaviours |
| Reconciliation | `internal/reconciliation/casino_consistency_integration_test.go`, `casino_statement_integration_test.go` (divergent-source detection), `casino_statement_fetch_integration_test.go` (fetch outside tx, `CoverageEnd`), `internal/casino/statement_mock_test.go`, `receive_callback_statement_capture_integration_test.go`, `cross_tenant_statement_capture_integration_test.go`, `internal/httpserver/casino_statement_admin_integration_test.go` | mandatory | The real source must pass the SAME fetch-shape tests; CAS-C-40..48 add the real-source cases |
| Outbound-credential tripwire | `cmd/platform-api/outbound_precondition_test.go` (`TestOutboundPrecondition_EveryWiredAdapterIsSynthetic`), `internal/casino/outbound_kindsplit_test.go`, `callcontext_redaction_test.go`, `credential_reflection_test.go` | must be relaxed **deliberately**, scoped to the casino adapter, with `security` sign-off and owner authorization (PROV-OUTBOUND-CRED-1) | a test proving only the named adapter is exempt |
| Production guard | `internal/providerkind/guard_test.go`, `cmd/platform-api/no_mock_binaries_test.go` | mandatory | adapter implements `providerkind.ProductionEligible` only at gate C9 |

## 2. Binding rows (known open items to close or accept before the first real casino adapter)

States are as recorded in the cited documents at base `4a3c85f`; re-verify each at gate C0 (CLAUDE.md "verify actual state").

| Row | Source | Condition before the first real adapter | Case |
|---|---|---|---|
| CAS-RECON-SCALE-1 | `a3-casino-record.md` s7, `a11-payments-binding-recheck.md` | design + ADR for per-run cost (ledger-finance + architect + devops) before real multi-tenant volume | CAS-C-48 |
| CAS-STMT-STORE-1 (R-5) | `a3-casino-record.md` s4 | append-only stored copy of the fetched statement + a defined coverage window, with a real source's format | CAS-C-44 |
| CAS-BET-JURISDICTION-RECHECK-1 (R-4) | `a3-casino-record.md` s7 | decision: accept the 4 h session bound or add a per-bet recheck | CAS-C-32 |
| CAS-GAME-KILL-RACE-1 (R-3) | same | accepted residual (READ COMMITTED ordering) or closed | none new |
| ADR 0025 item 8: free-round/jackpot conformance case | ADR 0025 Stage 10.3 amendment | written and reviewed with the first adapter | CAS-C-26 |
| CAS-WIN-ANOMALY-1 | ADR 0025 amendment | detection-only alert before real-money go-live (registered, not built) | CAS-C-49 |
| LEDGER-MANUAL-ADJ-4EYES-1 | ADR 0025 amendment ("go-live blocker"; forged wins corrected only by compensating entries) | confirm closed by ADR 0100 governed manual adjustments; ledger-finance to state in writing | gate C5 evidence |
| PROV-REVOKE-ALL-1 | ADR 0025 amendment | when a second tenant shares a provider | gate C8 note |
| F-12 adapter-side parse hook | `docs/integrations/casino-launch-bootstrap.md` | built if the vendor cannot speak the platform bootstrap shape (CAS-G01) | CAS-C-11 |
| W2a credential revocation immediacy incl. racing an in-flight request | ADR 0025 amendment ("no real casino resolver or adapter is wired in any environment until W2a revocation is implemented and tested") | re-run the revocation tests against the adapter's resolver path | CAS-C-15 |
| LF-10 held-disposition rollback after resolution | doc 08 s16.20 | accept fail-closed or decide | CAS-C-26 |
| HD-10.3-4 suspended tenant: open rounds cannot settle, unseen-original rollback leaves no tombstone | ADR 0025 amendment | owner accepts the disclosed consequence for the vendor's redelivery window | gate C8 |
| ALERT-DELIVERY-1 | readiness report, PSP gates G8 | production blocker, same as PSP | gate C8 |
| T8 (payout-reversal proof-bound approvals) | owner decision 2026-10-16 | gate for "any real PSP"; casino does not move payouts, so only the PSP gate applies | gate C0 note |

## 3. New cases (adapter-level; no database unless stated)

Naming: `CAS-C-nn`. "Fixture" = recorded vendor example or sandbox capture. "Core" = the unchanged orchestrator via the
HTTP route or `receiveCallbackInTx`.

### 3.1 Authentication and wire translation

| Id | Case | Needs DB |
|---|---|---|
| CAS-C-01 | Fixture-hook refactor of the six mock-typed sub-tests (see s1). The suite must go red for a deliberately broken adapter fixture (self-test like `TestReasonBoundConformanceSelfTest_DetectsNonConformingReason` in KYC) | no |
| CAS-C-02 | Sandbox scenario map replaces the mock magic amounts/game ids for decline and ambiguous outcomes | no |
| CAS-C-03 | Known-answer vector for the inbound scheme verifies byte-exact; one flipped byte, a missing header, a malformed header, an expired/future timestamp, a wrong key id each yield `ErrSignatureInvalid`/`ErrTimestampOutOfWindow` with no body, secret or fingerprint in the text (SC11) | no |
| CAS-C-04 | Verification precedes parsing: a body that is not parseable but correctly signed is `ErrCallbackMalformedBody` (400); an unparseable unsigned body is the uniform 401 | no |
| CAS-C-05 | Tenant binding: a credential resolved for tenant A rejected on tenant B's route at MAC level, not only by metadata (existing sub-test, via CAS-C-01) | no |
| CAS-C-06 | Every vendor event type maps to exactly one of {bet, win, rollback} or to an explicitly documented ack-and-drop; any other type is `ErrCallbackMalformedBody`. The ack-and-drop set is asserted equal to the documented non-money events (round close, zero win) | no |
| CAS-C-07 | Amount conversion is exact: a vendor amount not exactly representable in the asset's minor units is refused (no rounding); the asset exponent comes from the registry; negative, zero (for bet/win), overflow `int64` refused | no |
| CAS-C-08 | `provider_tx_id` derivation: if the adapter namespaces ids per event type, the derivation is deterministic, bounded to 255 bytes, never collides across types and avoids reserved ledger prefixes (U-CAS-22/23) | no |
| CAS-C-09 | Vendor response encoding (only if CAS-G01 hook exists): for each core outcome (success, replay, decline reason, tombstoned, each 4xx/409/503 class) the vendor-shaped response is as documented and carries no detail the generic body withholds | no |

### 3.2 Launch and bootstrap

| Id | Case | Needs DB |
|---|---|---|
| CAS-C-10 | `Launch` with `PlayerRef` only: the request on the wire contains the opaque UUID, the token and the session id and nothing from the player's account (static check of the serialised request: no email, name, account id, tenant id, IP) | no |
| CAS-C-11 | Bootstrap: the vendor's token redemption (platform shape, or the CAS-G01 parse hook) yields the same `BootstrapLaunch` result, the same idempotent byte-identical replay for the same `request_id`, uniform 401 for every mismatch | yes |
| CAS-C-12 | Launch decline vs ambiguous vs transport failure are distinguishable; a timeout after send produces `ambiguous`/error and never a usable session without a vendor-confirmed launch (INV-IO-1: no tx held, `ErrProviderCallRefused` under `txscope.Held`) | yes |
| CAS-C-13 | Token TTL and single use: second redemption refused; redemption after 2 min refused; consumed session accepted for bets only | yes |
| CAS-C-14 | Demo launch never produces a postable session: a bet on a demo session is `ErrLaunchSessionRequired`; demo launch honours the jurisdiction and market checks | yes |
| CAS-C-15 | Credential revocation (ADR 0093) takes effect on the next callback and on a callback racing an in-flight request; open exposure then strands as documented | yes |

### 3.3 Idempotency, tombstones, ordering (vendor-shaped fixtures through the core)

| Id | Case | Needs DB |
|---|---|---|
| CAS-C-20 | Redelivery of a vendor bet, win and rollback (identical fixture, 12 parallel deliveries): exactly one posting, the rest `Replayed` | yes |
| CAS-C-21 | Rollback fixture BEFORE its original bet: tombstone written; the late original bet fixture is `declined original_rolled_back`; the late win is 409 `original_tombstoned` with a rejection row | yes |
| CAS-C-22 | Win fixture before its bet (if U-CAS-31 says possible): the win is refused 400 `bet_not_found`; the later-arriving bet then the redelivered win posts once. Records whether the vendor's documented retry covers a 400 (U-CAS-26) | yes |
| CAS-C-23 | Same `provider_tx_id`, changed amount/asset/round: `ErrProviderTxPayloadMismatch` 409, nothing posted | yes |
| CAS-C-24 | Declined bet redelivered later: re-evaluated, not replayed (documents the behaviour the vendor must tolerate) | yes |
| CAS-C-25 | Rollback of a win and of a bet are two independent reversals; a second distinct rollback of one original is `ErrAlreadyRolledBack` | yes |
| CAS-C-26 | Free round / jackpot / promo / multi-bet fixtures (per U-CAS-19/20): the adapter never maps a payout with no platform bet to a win; the core's refusal (`ErrBetNotFound` 400, `ErrAmbiguousMultiOriginRound` 409) is asserted; bonus-funded multi-bet titles are refused for bonus funding if CAS-G04 is built | yes |

### 3.4 Gates (the vendor path must not bypass them)

| Id | Case | Needs DB |
|---|---|---|
| CAS-C-30 | A vendor bet for a player under self-exclusion, time-out, RG limit, risk deny, KYC pending with an active play policy, AML hold: each `declined` with the platform reason, no posting; wins and rollbacks for the same player still settle | yes |
| CAS-C-31 | Game disabled / not available for the brand / tenant not active / brand not active / capability disabled: NEW bet refused; win and rollback of existing rounds still settle | yes |
| CAS-C-32 | Session past `CASINO_SESSION_MAX_LIFETIME` and market closed for the brand: new bet declined and audited; a mid-session jurisdiction change is NOT detected (documents CAS-G05 / R-4 as accepted or closed) | yes |
| CAS-C-33 | A vendor callback whose `player_ref` is another player's valid reference: refused, audited `casino_callback.player_ref_mismatch`, no posting | yes |

### 3.5 Reconciliation (the real statement source)

| Id | Case | Needs DB |
|---|---|---|
| CAS-C-40 | The real source is a `statement.CasinoStatementFetcher`; `Fetch` refuses under `txscope.Held`; the source cannot take the Synthetic renderer shape | no |
| CAS-C-41 | Statement fixture matches a ledger built from the corresponding callback fixtures: clean run; every kind of divergence (missing line, extra line, amount, asset, round, duplicate line, wrong original, total mismatch) is a P1 mismatch and nothing is auto-corrected | yes |
| CAS-C-42 | Tombstone pairing: a statement rollback whose original the ledger holds only as a tombstone is a match; the late-original-counted-by-provider case is flagged | yes |
| CAS-C-43 | `CoverageEnd`: absent, too old (> 1 h before period end), in the future (> 5 min), earlier than the previous run: the run fails closed (`ErrCasinoStatementCoverage`), never clean | yes |
| CAS-C-44 | Stored append-only copy of the fetched statement (CAS-STMT-STORE-1), if built: immutable, tenant-scoped, RLS, bound to the run | yes |
| CAS-C-45 | A configured provider with no eligible source is a failed, alerted run (needs the CAS-G07 registry), never clean | yes |
| CAS-C-46 | Read-only credential proof: the statement credential is refused by the vendor for a wager/refund/launch call (sandbox) | sandbox |
| CAS-C-47 | Real mismatch kind registered (migration) and rendered by the admin read route | yes |
| CAS-C-48 | Scale: run duration and memory with N synthetic rounds per tenant at the agreed volume (CAS-RECON-SCALE-1 target set by ledger-finance + devops) | yes |
| CAS-C-49 | CAS-WIN-ANOMALY-1 detection alert fires for a synthetic win spike without blocking settlement | yes |

## 4. Evidence required per case

- Unit/adapter cases: test name, fixture citation (document URL + retrieval date or sandbox capture id), `go test` output.
- DB cases: run as the NOBYPASSRLS runtime role (as the existing integration suites do), ledger invariants at the end.
- Sandbox cases: sandbox run log with request/response ids, redacted; no secret or real player data.
- A skipped mandatory case is a failure. A case that cannot be run because a `U-CAS-nn` item is still open is recorded
  `BLOCKED` with the item id, not omitted.
