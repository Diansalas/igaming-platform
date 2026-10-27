# Licensing enforcement slice 3 record (branch lic3-enforcement)

Source: `docs/plans/licensing-audit-enforcement.md` section 9 and section 6 step 1. Software capability only; nothing here is a legal or regulatory statement. No migration was added (head stays 0175; 0178 was not needed). No ADR or governance file was edited; proposed ADR/registry text is at the end.

## Per-gap outcome

| Gap | Label | Outcome |
|---|---|---|
| G4 launch/bootstrap status gate | IMPLEMENTED | `casino.CheckLaunchStatusGate` (tenant key then brand key, the shared-lock primitives postBet uses) runs in the launch HTTP handler BEFORE the wallet get-or-create and again as the first statement of LaunchGame phase A, and `bootstrapGateDenialReason` checks tenant then brand before the other gates. A denial writes audit `casino.launch_denied_by_status`, mints nothing, creates no wallet. HTTP: `409 TENANT_OR_BRAND_NOT_ACTIVE`. Bootstrap denial reasons `tenant_not_active` / `brand_not_active` revoke the token and audit, like the other definitive denials. |
| G6 jurisdiction-scoped risk rules | IMPLEMENTED (refusal) | `risk.CreateRule` refuses any `jurisdiction_code` with `ErrJurisdictionScopeUnsupported` (code `jurisdiction_scope_unsupported`, HTTP 400). |
| G7 deposit risk rules | IMPLEMENTED (refusal) | `risk.CreateRule` refuses `deposit` with `ErrOperationNotEvaluated` (code `operation_not_evaluated`, HTTP 400). |
| G15 shared chain helper | IMPLEMENTED | `jurisdiction.ResolveLicensingChain` (below). |
| G16 stale comments | IMPLEMENTED | `withdrawal/policy.go`, `risk/types.go` corrected. |
| G14 architecture doc claims | IMPLEMENTED (doc) | `docs/architecture/15-jurisdiction-and-licensing-model.md` status correction plus per-bullet NOT IMPLEMENTED / PARTIALLY IMPLEMENTED labels. |
| G9 KYC default-allow | PARTIALLY IMPLEMENTED | Signal only, no behaviour change: `kyc_enforcement_dormant` WARN once per process per (tenant, operation, reason) for no-licence and no-active-policy. The resolver change "no licence is allowed only if the recorded launch determination says not_applicable" is NOT done (see residual rows). |
| G17 sportsbook vs casino settlement asymmetry | NO CHANGE, audit claim is superseded | There is no asymmetry in code. ADR 0095 section 40.5 (owner decision R3-GAME-POSTINGS-NONACTIVE-1) refuses NEW casino win and rollback postings AND sportsbook settlement postings for non-active tenants; section 40.6 (Q-GP-5) keeps TERMINAL STAKE RETURNS (casino rollback of a posted bet, sportsbook void of an open bet or after settlement) allowed so stakes are not stranded. The audit matrix row "Casino win / rollback: settlement never blocked" is wrong for win/rollback-of-win; the recorded residual is Q-GP-6 (open casino win entitlements at closure), a human decision. I changed nothing: aligning settlement to "continue for suspended tenants" would reverse a recorded owner decision. |

## Remaining gaps (one line each, class)

- G1 no automatic sanctions/PEP screening or monitoring: human decision (F1-F3, ADR 0116 Q1) plus provider dependent (mock provider); trigger wiring needs another slice.
- G2 RG enforcement OFF by default, activation not forced: human decision (changes ADR 0112 "readiness is informational").
- G3 player-scoped jurisdiction unresolved (steps 6.2 1-6): human decision (HDR-J-7 purpose mapping, PC-GAP-1/2, country-to-jurisdiction mapping); needs another slice. G6's refusal stays until this is done.
- G5 licence status mutator and payout behaviour while a licence is suspended, `permitted_products/markets`: human decision (payout policy, product vocabulary); mutator needs another slice.
- G8 single-reviewer residence determination: human decision (policy); needs another slice.
- G9 remainder (no licence = allowed only on recorded not_applicable; `unavailable` for an active tenant with none; readiness item "active tenant with zero active KYC policies"; `edd_amount`/`registration_tier` wiring): human decision for thresholds (HD-KYC-1..8); the readiness item needs another slice (a new readiness check changes `SnapshotVersion`, OpenAPI, Back Office labels and invalidates bound requests, so it was not done here).
- G10 per-bet country for casino: provider dependent (geolocation vendor); the only lever is the session max lifetime configuration.
- G11 registration has no player-country or sanctions check: human decision (confirm or reverse PD-MKT-REG-1).
- G12 hybrid-model scope for AML/adjustments/hold resolution and RG floor approval: human decision.
- G13 no API/UI for market ceilings/policies and other regulated config: needs another slice (platform-only audited routes plus Back Office pages).
- G17: see table; Q-GP-6 is a human decision.

## G4 decisions

- Order: tenant gate, brand gate (the postBet order), then market gate, then everything else. The status gate sits before the market gate so a suspended brand does not even evaluate residence evidence.
- Demo mode is GATED. Decision: demo is not real money, but a demo launch still (a) is preceded by `wallet.GetOrCreate` in the HTTP handler (a wallet effect) and (b) mints a wallet-bound session, a token and a vendor launch URL. Keeping it allowed would break "a suspended or closed brand must not mint launch sessions or wallets". It creates no ledger effect either way; it is gated because of the wallet and session side effects.
- Fail closed: any error reading either status aborts the launch (no denial result, no session). A missing tenant/brand row is a denial (the shared primitive returns "not active" for a row it cannot see).
- Concurrency: the shared status-gate advisory locks are taken SHARED, so a launch waits for an in-flight governed suspension and then sees the committed status; a launch that committed first makes the suspension wait. Test: `TestLaunchGame_BrandSuspensionRacingLaunchWinsAndNothingIsMinted` (suspension held open, launch blocked, then denied). Lock order is the same as postBet (tenant key, brand key); bootstrap takes them AFTER its session-row `FOR NO KEY UPDATE`, which does not conflict with the postBet path (the postBet takes them before the FK key-share lock on the session). ledger-finance lock-order review is still requested (see below).
- Test seam: a launch orchestrator built with `WithUngatedMarketOpenForTests` skips the BOOTSTRAP status check, because three migration-pinned scratch-database tests run at schema 0111, which has no `tenant_status_gate_key`. The seam is never used in production (`TestProductionCodeNeverUsesTheUngatedSentinels`). LaunchGame's gate is unaffected by it.
- Not changed: postBet (already gated), win/rollback paths, KYC submission boundary for suspended tenants (HQ-E1-1 open).

## G6 / G7 decisions

- ADR 0031 (R-2b) designed `jurisdiction.IsActive` as the authoring precondition. I decided NOT to implement it as the gate: `IsActive` only records that someone switched resolution on for a (tenant, class); the resolver is still `unresolved(no_signal)` for every player (`TestResolve_PlayerScopedOperationAlwaysUnresolvedNoSignal`), so a rule authored under an "active" fact would still be refused by every evaluation. The minimal correct fail-closed fix is refusal of ALL jurisdiction-scoped rules for ALL operations until a player-scoped jurisdiction resolves (G3). This is a stricter, interim form of R-2b. When G3 lands, replace the refusal by the `IsActive` precondition.
- Existing jurisdiction-scoped or deposit rule ROWS are not touched; they keep their current evaluator behaviour (jurisdiction-scoped rows still produce `ErrMissingJurisdiction`). Operators who already authored one must disable it; a readiness check naming them is part of G3's recommended fix and is not built here.
- `licensing_mode` scoped rules are unaffected (the mode is always supplied).
- Existing tests that author such rules on purpose now set `CreateRuleParams.TestOnlyBypassAuthoringRefusals` through the two shared fixtures `risk.createTestRule` and `casino.createCasinoRiskRule`. This is an intended behaviour change; no assertion was weakened. A static test (`TestAuthoringBypassIsNeverSetByProductionCode`) pins that no non-test file references the field. The refusal itself is pinned by `TestCreateRule_RefusesUnevaluatedOrUnresolvableScopesAtAuthoring` (nil transaction, proving nothing is attempted) and by the HTTP test `TestRiskRules_DepositAndJurisdictionScopedRulesAreRefusedAtAuthoring` (400, nothing stored).

## Chain helper (G15)

`jurisdiction.ResolveLicensingChain(ctx, q, tenantID) -> LicensingChain{Outcome, InvalidReason, TenantID, LicenceID, JurisdictionID, JurisdictionCode}` with `Outcome` in `resolved | no_licence | licence_invalid` (`InvalidReason`: `tenant_not_visible`, `licence_or_jurisdiction_not_found`). A genuine read failure is an error, never a state. It states what is bound; it does NOT judge licence status or dates (`EvaluateLicenceValidity` does), so no caller's behaviour changed through the helper. The brand has no licence of its own, so the chain is read from the brand's tenant. It is operator-side only and is not player jurisdiction resolution (HDR-J-1 untouched).

Callers migrated and what each does with each state:

| Caller | resolved | no_licence | licence_invalid | Behaviour change |
|---|---|---|---|---|
| `kyc.resolveLicensingJurisdictionID` | policy lookup | `not_required` (unchanged) plus the G9 dormant signal | error, so `unavailable` (fail closed) | none (previously an unreadable tenant row was also an error) |
| `rg.tenantJurisdiction` | jurisdiction code | nil = no configuration (unchanged) | error, request refused | FIXED a fail-open: a tenant row the session could not see used to fold into "no configuration" (no ceiling, no mandatory limit); it now errors |
| `launchgov` R-RG-CONFIG (2 reads) | joins the set | `fail no_licence_jurisdiction` (unchanged) | `not_evaluable read_error` | none (previously an unreadable tenant was `read_error`) |
| `payoutinstrument.maxAgeFor` | reads the per-jurisdiction window | nil window (unchanged) | error | none in practice (FK) |
| `operatingmarket` resolver step 1 | proceeds to validity | `licensing_unknown` (unchanged) | `licensing_unknown` (fail closed) | none |

Not migrated (reason): `marketgate/open.go prefilterByCeiling` (a documented optimisation prefilter whose input is re-checked by the fail-closed resolver; it needs only the licence id), `jurisdiction/resolver.go` and `evaluation_policy.go` (inside the jurisdiction package, different semantics: they set the tenant-subject resolution basis), `launchgov/hard.go` H-2 (validates a recorded launch licensing determination against the tenant row; not the same join), the AML policy selector (takes a caller-supplied `JurisdictionID`), `kyc/enforcement_dormancy.go` (set-based SQL over all tenants). A follow-up could move the first two onto the helper's licence-id field.

## Verification

Tests written first: `internal/risk/authoring_refusal_test.go` (failed before the refusal existed), `internal/jurisdiction/licensing_chain_test.go` and `..._integration_test.go`, `internal/casino/launch_status_gate_integration_test.go` (matrix of tenant/brand x pending_launch/suspended/closed x real/demo, guard-order, active still launches, suspension race, bootstrap tenant and brand), `internal/httpserver/casino_launch_status_gate_integration_test.go` (409, no session, no new wallet, audit), `internal/httpserver/risk_flow_integration_test.go` (authoring refusals), `internal/kyc/enforcement_dormant_signal_test.go`.

Limit of the wallet assertion: the HTTP test asserts the wallet row count is unchanged; if the player already owned a wallet for that asset the assertion alone would not prove the get-or-create was skipped. The ordering is proven by code (status gate precedes `wallet.GetOrCreate` in the same transaction) and by the LaunchGame test that the market guard is never called.

## Requests and merge notes

- `ledger-finance` review requested for the bootstrap and launch lock order (tenant key then brand key after the session row lock).
- `security` review requested for the new denial/revoke path in bootstrap (token handling is unchanged: no token or hash is logged or put in audit metadata).
- No migration, no route table change, no new step-up class. OpenAPI: launch `409` response and the risk-rule create description were updated. `docs/runbooks/observability-and-alerting.md` gained two log events.

## Proposed ADR / registry text (for the orchestrator to place)

1. ADR 0112 section on `postBet`-only brand gate: amend "casino brand gate applies to postBet only" to "applies to postBet, LaunchGame and the launch-token bootstrap (G4); demo launches are gated too".
2. ADR 0031 R-2b: record "interim stricter form: `CreateRule` refuses every jurisdiction-scoped rule and every deposit rule until (a) a player-scoped jurisdiction resolves and (b) a deposit path evaluates risk".
3. Registry rows: G9 remainder, G3 readiness item "armed jurisdiction controls exist", G17 audit correction (cite ADR 0095 40.5/40.6), Q-GP-6 still open.
