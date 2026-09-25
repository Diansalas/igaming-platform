# Stage 10 W1 — Security Review (sportsbook settlement, in-house MOCK)

| Field | Value |
|---|---|
| Reviewer | `security` specialist |
| Date | 2026-09-25 |
| Contract | `docs/decisions/0088-sportsbook-settlement-implementation-contract.md` (§3.2, §3.3, §3.5, §4.4, §4.6, §4.7, §9, §10, §11, §15 S1–S10) |
| Reviewed state | `eb3912f` (migration 0091, `settlement.go`), `d26b3b9` (`deploy/init-app-role.sql`), `36616f1` (F-7), `f72d864` (route, auth, read surfaces, reconciliation). The "uncommitted working tree" named in the brief was committed as `f72d864` during this review; the working tree was clean when the review finished, and the review covers `f72d864`'s content. |
| Mode | Code-level and design-level review only. Not a penetration test, not a certification audit. |

## Verdict

**APPROVE WITH FINDINGS.** No P0 or P1 findings. Two P2 findings must be fixed before W1 is marked complete. P3 findings are advisory. None of the findings blocks launch, because this route never exists in production.

Severity scale: P0 is exploitable now or causes loss of funds or cross-tenant data. P1 must be fixed before the change merges. P2 must be fixed before the W1 stage gate closes. P3 is advisory or hardening.

## 1. What was verified

### 1.1 ADR 0088 §15 security findings

| # | Requirement | Status | Evidence |
|---|---|---|---|
| S1 | `RoleRiskManager` is the only grantee. It is not in `backoffice/src/auth/permissions.ts`. | **IMPLEMENTED** | `internal/auth/permission.go` (the constant, plus the grant inside the `RoleRiskManager` set only). `internal/auth/sportsbook_settlement_permission_test.go` checks for exactly one grantee by permission constant, and has named negative cases that include `RoleFinance`. `backoffice/src/auth/permissions.ts` has no reference. See P3-4 about how complete the role list is. |
| S2 | The deny triggers are the binding control. There is a guarded REVOKE in the migration and in the init script, plus notes in the docs and CI. | **IMPLEMENTED** | `migrations/0091_sportsbook_settlement.up.sql:126-132` (triggers), `:330-335` (guarded REVOKE). `deploy/init-app-role.sql:122-131` sits after the `:85` backfill GRANT. Also `docs/security/runtime-role-separation.md:362-376` and `.github/workflows/ci.yml:169-183`. I checked the live CI-local database: `igaming_runtime` has INSERT, and has no UPDATE, DELETE or TRUNCATE. `TestRuntimeRole_SportsbookBetSettlementsPrivileges` passes. |
| S3 | The trigger-only denial is proven on its own. | **IMPLEMENTED** | `TestDBConstraints_DenyTriggers_BlockMutationEvenWithAPermissiveRLSPolicy`. It uses a scratch database, runs as the owner, adds permissive UPDATE/DELETE policies, and confirms UPDATE, DELETE and TRUNCATE are all rejected. |
| S4 | Split policies: `FOR SELECT` plus `FOR INSERT … WITH CHECK`, player self-scope SELECT, no UPDATE/DELETE policy, and a test for player-scoped INSERT. | **IMPLEMENTED** | `0091…up.sql:96-124`. On the live database the policies are `tenant_staff_select` (r), `tenant_staff_insert` (a) and `player_self_scope` (r), with `relrowsecurity` and `relforcerowsecurity` both true. None of the roles (`igaming`, `igaming_runtime`, `igaming_test_admin`) is SUPERUSER or BYPASSRLS, so the RLS tests are meaningful. Test: `TestDBConstraints_RLS_PlayerScopedInsertRejected`. See P3-6. |
| S5 | `RequireStaffPrincipal` admits exactly `PrincipalStaff` and returns 403 otherwise. `tc.Subject` is parsed as a UUID or the request fails closed. The staff user must be active in the tenant. | **IMPLEMENTED** | `internal/auth/middleware.go` (`RequireStaffPrincipal`). `internal/httpserver/sportsbook_settlement_handlers.go:206-210`. `internal/sportsbook/settlement.go:307-318`: the staff user must exist, have `TenantID == ev.TenantID`, and be `active`, all inside the posting transaction. Tests cover player, service (including a service token that carries `risk_manager`), staff, and no-context principals, plus `TestSettlementActor_NotFoundOrInactiveOrCrossTenant`. See P3-3. |
| S6 | Client IP comes from `trustedProxyClientIP`. The raw `RemoteAddr` goes in metadata. The raw `X-Forwarded-For` is never copied. | **IMPLEMENTED, not tested** | `sportsbook_settlement_handlers.go:244-245`. `settlement.go` `settlementAuditEntry` / `settlementAuditMetadata` only put `ev.IPAddress` in `IPAddress` and `ev.RemoteAddr` in `metadata.remote_addr`. No header value is read anywhere else. No test asserts this (see P2-2). ADR 0086's audit-IP launch gate is neither affected nor partially closed. |
| S7 | Alert fields are limited to the allow-list. | **IMPLEMENTED, not tested**, with one naming deviation | `sportsbook_settlement_handlers.go:163-176` emits exactly `tenant_id, bet_id, reason, event_type, generation, bet_status, actor_staff_account_id, request_id`. The context logger (`observability.LoggerFromContext`) only adds `request_id` and `tenant_id`, which are both on the allow-list. The logger receives no body, headers, token or PII. The alert name for an unknown bet deviates from §4.6 (P2-1). No test covers this (P2-2). |
| S8 | The player surface omits `actor_staff_account_id` and `request_id`. | **IMPLEMENTED** | `betResponse` in `internal/httpserver/sportsbook_handlers.go` has neither field. The staff-only `settlementLifecycleEntryResponse` also omits both. The player list runs under `WithPlayerScope`. Test: `TestListMyBets_PlayerSurfaceOmitsStaffAndRequestFields` passes. |
| S9 | `reason_code = test_support_simulation` for settle and rollback. Void records carry `void_reason`. Rejections use the `rejection_code` key. | **IMPLEMENTED** | `settlement.go` `settlementAuditMetadata`, `RecordSettlementRejection`. The composed void clears `VoidReason` on the rollback's audit event, so the rollback record carries `reason_code`. Tested in `settlement_scenarios_integration_test.go:139-206`. |
| S10 | The F-7 audit covers indirect callers, checks each site for the reserved prefix, and is recorded against a SHA. | **IMPLEMENTED** | `docs/governance/stage-10-f7-ledger-replay-audit.md:6` (audited `94bc863`), `:62` (indirect callers), `:96-99`, `:184-186` (reserved-namespace result: no caller can produce the prefixes). See P3-5 for the unfilled remediation-commit field. |

### 1.2 Brief-specific checks

| Check | Result |
|---|---|
| Fails closed when test support is disabled or the environment is production | **Pass.** `sportsbook_routes.go` only calls `mux.Handle` when `SportsbookSettlementSimulationEnabled && SportsbookEnabled`. Otherwise the pattern is absent and Go's mux returns 404. There is no sibling pattern on that path, so a 405 cannot occur. `main.go` sets the flag from `cfg.TestSupportRoutesEnabled()`, which is `Environment != "production" && TestSupportEndpointsEnabled`. `Load()` rejects an invalid `APP_ENV` and rejects the contradictory production+enabled configuration (`config.go:411-432`). Test: `TestSettlementSimulate_RouteAbsentWhenFlagOff`. |
| Staff-only and tenant-scoped | **Pass.** The chain is `Middleware → RequireTenantScope → RequireStaffPrincipal → RequirePermission`, which matches §9.1. The tenant comes only from `tc.TenantID`. The request body has no tenant, player, wallet, account or posted amount, and unknown fields are rejected by `decodeJSON`, which also caps body size. The bet is resolved with `SELECT … FOR UPDATE` under `WithTenant` (RLS). |
| Cross-tenant returns 404 with no writes | **Pass.** The RLS miss goes to the `NOT_FOUND` rejection with the same code and message as a nonexistent id. Nothing is posted, no tombstone or history row is written, and only the caller's own-tenant rejection audit is recorded (required by §4.6). There is a defence-in-depth tenant equality check at `settlement.go:328-332`. Tests: `TestSettlementSimulate_CrossTenantBetID_404NoRowsWritten` and `TestSettlementDecision_CrossTenantBet_NotFound`. See P3-7. |
| Never a player capability and no production bypass | **Pass.** Player tokens get 403 from `RequireStaffPrincipal`, and `RolePlayer` has no permissions. T-1 and T-2 also RAISE under a player-scoped connection. The fault-injection hook is `//go:build integration` only, and the non-test build is a no-op (`settlement_hook.go`). No environment variable, header or role enables the route in production. |
| Audit completeness | **Pass**, with the test gap in P2-2. There is one `audit.Record` per transition, in the posting transaction. It records `ActorType=staff`, `ActorID` (the staff UUID from the token, checked to be active in the tenant), `TenantID` from the context, `TargetType/ID`, `before_status/after_status`, the event fields, ledger and record ids, `replayed`, `driver`, `mode`, `remote_addr`, and `reason_code` or `void_reason`. Replays and rejections are audited. For §4.7 aborts the audit is written in a separate transaction (`sportsbook_settlement_handlers.go:271-276`). |
| F-7 client messages | **Pass.** `ledger.go:371-380` errors name only the transaction id and the field classes (`entries`, `reversal_link`, …), never amounts or accounts (`replay.go:26-35`). Every HTTP mapping returns a generic body: casino webhook and play "callback rejected" / "request rejected", payment webhook and simulation "callback rejected" / "deposit could not be settled", settlement "settlement integrity check failed". Withdrawal `Complete` mismatches fall through to the existing generic 500. |
| Runtime role narrowing | **Pass.** See S2. |
| RLS and trigger correctness | **Pass.** Both functions are `prosecdef = false` (SECURITY INVOKER) and neither sets `search_path`. I ran live probes as `igaming_runtime` inside rolled-back transactions. With no tenant context, INSERT raises `parent bet … is not visible`. With player scope, INSERT raises `history rows are never written under a player-scoped connection`. UPDATE and TRUNCATE fail with `permission denied`. T-2 can only see an updatable bet under staff tenant scope, where that bet's history is visible too, so an empty history read cannot be mistaken for "open". The xmin check on the composed void fails closed if a subtransaction is used. |
| No secrets committed | **Pass.** The diff `0d943fd..f72d864` adds no credentials. Test fixtures use synthetic passwords. `igaming_runtime_dev_password` in `deploy/init-app-role.sql` is an existing dev-only value and is not new in W1. |

### 1.3 Tests run

All passed:

- `go test ./internal/auth -run 'RequireStaffPrincipal|SportsbookSettlementSimulate'`
- `go test -tags=integration ./internal/httpserver -run 'TestSettlementSimulate_|TestListMyBets_PlayerSurfaceOmits|TestListAdminBets_Lifecycle'`
- `go test -tags=integration ./internal/sportsbook -run 'TestDBConstraints_|TestSettlementDecision_|TestSettlementActor_|TestSoleWriter_'`
- `go test -tags=integration ./internal/db -run Runtime`

I did not run the full suite, because another full run was already in progress.

## 2. Findings

### P2-1 — The unknown-bet alert name does not match ADR 0088 §4.6

- **Where:** `internal/httpserver/sportsbook_settlement_handlers.go:183-185, :288`; `internal/sportsbook/settlement.go:83, :324`.
- **What:** §4.6 requires `sportsbook_settlement_integrity_alert_bet_not_found`. The alert suffix is built by `strings.ToLower(rejectionCode)`, and the rejection code is `"NOT_FOUND"`, so the event actually emitted is `sportsbook_settlement_integrity_alert_not_found`.
- **Failure scenario:** A monitoring rule written against the ADR name never fires. A staff account probing another tenant's bet ids (cross-tenant enumeration) then goes unalerted. The audit row still exists, but nothing pages.
- **Fix:** In `alertReasonFromRejectionCode`, map `sportsbook.SettlementRejectBetNotFound` to `"bet_not_found"` explicitly. Leave the wire code as `NOT_FOUND`. Pin the result in a test (P2-2).

### P2-2 — No tests for the alert allow-list (S7) or the audit IP fields (S6)

- **Where:** No test covers `logSettlementIntegrityAlert` or the audit `ip_address` / `remote_addr` for this route. `settlement_assertions_integration_test.go:121` reads only `actor_type, actor_id, action, outcome, target_id, metadata`.
- **What:** S6 and S7 are implemented correctly today, but nothing stops a regression. For example, someone could add `"body", req` to the alert, or pass `r.Header.Get("X-Forwarded-For")` into `IPAddress`, and every test would still pass.
- **Fix:**
  - (a) Add a handler test with a capturing `slog.Handler`. Trigger `SETTLEMENT_PAYLOAD_MISMATCH` and `NOT_FOUND`. Assert the event names (including `…_bet_not_found`) and assert the attribute key set is exactly the §4.4 allow-list.
  - (b) Add an HTTP integration test that sends `X-Forwarded-For: 203.0.113.9` with `TrustedProxyCount = 0`. Assert that `audit_log.ip_address` equals the `RemoteAddr` host, that `metadata.remote_addr` equals `RemoteAddr`, and that the string `203.0.113.9` appears nowhere in the audit row.

### P3-1 — The cause of a §4.7 integrity abort is never logged

- **Where:** `sportsbook_settlement_handlers.go:263-278`.
- **What:** In the `ErrSettlementIntegrity` branch, `err` is discarded. The alert and the separate-transaction audit carry only `SETTLEMENT_INTEGRITY`. The wrapped cause (ledger key reused, payload-mismatch field class, T-1 RAISE text) is lost, which slows incident triage. The cause contains ids and field classes but no amounts or PII.
- **Fix:** Log `err` under a separate, non-alert event (for example `sportsbook_settlement_integrity_detail`) with `bet_id` and `request_id`, or add a `cause_class` enum to the audit metadata. Keep the alert event itself on the allow-list.

### P3-2 — An inactive or unknown actor gets 403 with no audit record

- **Where:** `sportsbook_settlement_handlers.go:255-258`; `settlement.go:311-318`.
- **What:** When a valid, unexpired staff token belongs to a deactivated or moved account, the request is refused but nothing is recorded. §10 does not require an audit here, but a deactivated account still using its token is a security-relevant event.
- **Fix:** Write a `sportsbook_bet.settlement_rejected` audit record with `rejection_code = "ACTOR_NOT_ACTIVE"` in a tenant-scoped transaction, or emit a warn-level log.

### P3-3 — The actor's role is not re-checked against the database

- **Where:** `settlement.go:316`.
- **What:** The permission comes only from the JWT role claim. If a risk manager is demoted, they can keep simulating settlements until the access token expires. This is the same pattern as the rest of the platform, and the route is non-production.
- **Fix (cheap):** Also require `staff.Role == identity.StaffRoleRiskManager` in the same check.

### P3-4 — The "exactly one grantee" test depends on a hand-maintained role list

- **Where:** `internal/auth/sportsbook_settlement_permission_test.go:13-16` iterates `allRoles` (`bonus_permission_test.go:8-11`).
- **What:** A role added to `rolePermissions` but not to `allRoles` would escape the S1 assertion.
- **Fix:** Iterate over `rolePermissions`' keys instead (the test is in the same package), or assert `len(allRoles) == len(rolePermissions)`.

### P3-5 — The F-7 audit record has no remediation commit

- **Where:** `docs/governance/stage-10-f7-ledger-replay-audit.md:406` still reads `_to be filled by the orchestrator_`.
- **Fix:** Record `36616f1`. The optional reserved-provider-id guard at `:208` / `:511` is still not implemented. Until a registry-side check exists, no provider may be registered with the id `sportsbook_settlement`, `sportsbook_rollback` or `sportsbook_void`. Carry this forward as a requirement for the real-provider stage.

### P3-6 — The player-scoped INSERT test does not identify which control rejected the insert

- **Where:** `settlement_db_constraints_integration_test.go:369-383` (asserts only `err != nil`). There is no automated test for the T-1 invisible-bet RAISE with no tenant context; I verified it by hand (section 1.2).
- **Fix:** Assert the T-1 message substrings (`player-scoped connection`, `is not visible`). Add a no-tenant-context INSERT case. Optionally, add a case that disables only T-1 on a scratch database, to prove the `tenant_staff_insert` WITH CHECK rejects the insert on its own.

### P3-7 — The cross-tenant tests do not assert that the ledger is unchanged

- **Where:** `sportsbook_settlement_flow_integration_test.go:165-208`; `settlement_decision_table_integration_test.go:286-294`.
- **What:** The HTTP test checks for no history rows. Neither test checks the owning tenant's `ledger_transactions` for `correlation_id = bet`. Neither test checks that no audit row lands in the owning tenant, or that the 404 body is byte-identical to the one for a nonexistent id.
- **Fix:** Add those three assertions.

### P3-8 (informational) — `request_id` is influenced by the caller

- **Where:** `internal/httpserver/middleware.go:28-33`.
- **What:** `X-Request-Id` is accepted if it is printable ASCII and at most 128 characters. It is then stored in the history `request_id` column, the audit record and the alerts. It is bounded and injection-safe, but it is not an authoritative correlation value, and a staff caller can choose it. This is existing platform behaviour and is not new in W1. Do not treat `request_id` as proof of which request produced a row. The authoritative attribution is `actor_staff_account_id` plus the audit record.

### P3-9 (informational) — The `player_self_scope` policies have no tenant predicate

- **Where:** `0091…up.sql:116-124` (and `0078…up.sql:183-187`, which it builds on).
- **What:** Isolation depends on `player_account_id` being a globally unique UUID that is set only by `WithPlayerScope` from the verified token subject. This is sound today and follows the existing pattern. As defence in depth, a future hardening migration could add `tenant_id = app.tenant_id` to both player policies.

## 3. Out of scope for this review

- Real provider settlement, webhooks, signature verification, cashout, partial settlement, and bonus-funded settlement. All of these are still PROVIDER DEPENDENT or NOT IMPLEMENTED.
- ADR 0086's audit-IP production launch gate. It is still open, and W1 neither closes it nor affects it.
- OB-1: a won-then-rolled-back settlement can drive cash negative. This is a ledger-finance item and is still OPEN.
- The ledger arithmetic, lock order and reconciliation logic, beyond checking that the reconciliation sweep reads each tenant under `WithTenant` with no RLS bypass. These belong to `ledger-finance`, `architect` and `qa`.
- Behaviour when the actual deployment topology has `TRUSTED_PROXY_COUNT > 0`.

## 4. Launch relevance

None of these findings blocks launch, because the route is absent in production by construction. The removal condition in ADR 0088 §9.1 still binds: when the first real sportsbook settlement provider is registered, this route must be removed or permanently disabled in the same change. That future change will need its own security review of provider authentication, replay protection and the reserved-namespace guard (P3-5).
