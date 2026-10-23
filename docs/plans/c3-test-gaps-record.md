# c3 record: test gaps of the market-configuration / licensing-change flows

Branch `c3-test-gaps` (from `4974b0d`, migration head 0189, `c1-precheck-officer` merged). **No migration. No production code changed.** Test code, one
test tool (`backoffice/e2e/tools/mcprobe`, two optional args) and docs only. Providers MOCK. Synthetic data. Nothing here is a legal approval of any
country or jurisdiction.

## Verdict (labels)

| Item | Label |
|---|---|
| Go integration tests, `internal/licensingchange/c3_*` (16 tests) | IMPLEMENTED (16/16 pass, also under `-race` for the concurrency/clock ones) |
| Go integration tests through HTTP, `internal/httpserver/c3_licensing_gaps_integration_test.go` (6 tests) | IMPLEMENTED (6/6 pass; concurrency also under `-race`) |
| Playwright `13-market-config-gaps` (12 tests) | IMPLEMENTED (12/12 pass in the final full run) |
| Placeholders "awaiting C1 merge" | FILLED after the C1 merge (no `fixme` left): i1 ceiling_not_enabled_for_country, i2 duplicate_jurisdiction_code, i3 currency_not_active_asset (each: wire status + generic code + exact `check_codes` + exact UI wording + nothing stored), Go wire test for five more codes, i4 platform_officer_required |
| Product defects found | none that required product code to change; four findings below (F1-F4), none fixed (tests only) |

## Coverage matrix (gap -> tests -> layer -> result)

| # | Gap | Tests | Layer | Result |
|---|---|---|---|---|
| 1 | Ceiling WITHDRAWAL (narrowing, single actor): DB effect, resolver, audit, UI | `TestC3_CeilingWithdrawal_NarrowingSingleActor_ClosesTheMarket` (executed in the filing call, 0 approvals, 2nd version append-only, tenant row untouched, resolver `not_permitted_by_licence` for all 4 ops, `requested`+`executed` audit and no `approved`, before/after + reason in the domain row, tenant staff 403, never-enabled country, re-enable is widening); spec 13 `(a)` | Go integ (DB) + Playwright (UI, real resolver via mcprobe) | pass |
| 2 | BRAND rung enable/disable/withdraw; PRODUCT-scoped operation rung; widening needs 2 distinct Persons; precedence | `TestC3_BrandRung_EnableDisableWithdraw_PrecedenceAndTwoPersons` (withdraw needs a reference, same Person/twin and requester's Person refused, tenant disable beats brand enable, sibling brand inherits, foreign brand refused), `TestC3_ProductScopedOperationRung` (casino-only disable, brand+product disable, product-less query unaffected, product-scoped ENABLE under a broader disabled row refused at execution (F2), withdraw widening, unknown product refused); spec 13 `(b)` brand, `(c)` product | Go integ + Playwright | pass |
| 3 | 24 h approval FRESHNESS and 72 h request EXPIRY | `TestC3_Freshness_StaleApprovalsDoNotCount_GoLayer`, `_Boundary` (23h50 counts, 24h01 does not), `_DatabaseIsAuthoritative` (Go clock skewed, DB refuses LC020), `TestC3_Expiry_72h_...` (71h executes; 73h: approve/reject/cancel NOT_PENDING, expiry materialised and audited once, listed as expired), `TestC3_Expiry_DatabaseIsAuthoritative` (LC031/LC013, direct state moves LC013), `TestC3_Expiry_CHARACTERISATION_CancelOfUntouchedExpiredRequest`; spec 13 `(d)` stale approval (UI marks it old, server 409 `LICENSING_CHANGE_APPROVALS_STALE`, fingerprint unchanged, cancel recovers), `(e)` expiry (stale UI click, fresh load shows Expired, untouched expired shows `(expired)` + Approve disabled) | Go integ (service + DB) + Playwright | pass |
| 4 | AUDIT for ceiling/policy/config: requested/approved/executed/rejected/cancelled; actor, tenant, entity, before/after, reason, IP; append-only; refused attempts | `TestC3_Audit_CeilingPolicyConfig_FullLifecycle` (3 kinds x executed/rejected/cancelled; exact event counts; actor, IP, user agent, request id, reason, hash, widening, required approvals; platform row vs `subject_tenant_id`; tenant-scoped domain row; denied rows are `outcome=denied` with only `{refusal}`; duplicate/unknown id/forbidden/not-pending write NO row), `TestC3_Audit_AppendOnly_RuntimeAndOwner` (UPDATE/DELETE on audit_log refused for runtime and owner; change tables: runtime 42501/LC013, owner LC012/LC013 or no visible row; TRUNCATE refused; data unchanged), `TestC3_Audit_TenantActorRowsAreTenantScoped`, `TestC3HTTP_AuditCarriesIPUserAgentAndTenantScope` | Go integ | pass |
| 5 | CONCURRENT approvers, duplicate approval, self, approve-after-cancel/execute, double execution, replay | `TestC3_Concurrency_ApproversRace_ExactlyOneExecution` (20 rounds x 5 racers: ap1, ap2, ap3, ap2 duplicate, ap1 twin), `TestC3_Concurrency_FinalApprovalVersusCancel` (20 rounds), `TestC3_ApprovalRefusals_Matrix`, `TestC3_ReplayedAndMisboundActorProofs_Refused` (wrong request, wrong hash, other actor, other operation: AP004; replay AP005; after execution LC031), `TestC3HTTP_ConcurrentApprovers_ExactlyOneExecution_20Rounds` (6 racers incl. self and twin), `TestC3HTTP_ConcurrentFinalApprovalVersusCancel_20Rounds`; spec 13 `(h)` (real platform-api binary, rounds inside the step-up window) | Go integ (+`-race`) + HTTP + Playwright | pass |
| 6 | Server authoritative / stale or tampered UI state | `TestC3HTTP_ServerIsAuthoritative_StaleAndTamperedClientState` (cancelled/executed meanwhile, identical retry, suspended account -> 401 `session_revoked`, stale role claim, tenant admin 403, stale step-up 403 `step_up_required`, class/required_approvals/status/requested_by in body and payload -> 400, mislabelled class: server classifies, wrong/nil/malformed ids, cancel by non-requester, reject by requester; fingerprint of requests/approvals/ceilings/audit unchanged after every refusal); spec 13 `(f)` (dialog open, API cancels / executes, stale Approve click, server refusal rendered, DB exactly `{cancelled, 0 approvals}` / `{executed, 2 approvals}`), `(g)` (tampered bodies, wrong ids, tenant staff, anonymous on the real stack) | HTTP + Playwright | pass |
| 7 | Jurisdiction creation single-actor path | `TestC3HTTP_CHARACTERISATION_JurisdictionCreationSingleActor` (one platform admin, 201; only `jurisdictions` +1; no licence/ceiling/policy/config/change request/approval/staff/basis; resolver outcomes for 2 tenants x 3 countries x 4 ops identical; one audit row, tenant NULL, actor, IP, reason; duplicate 400, tenant staff 403, anonymous 401 create nothing) | HTTP | pass (pins CURRENT behaviour; must be rewritten if creation moves behind a licensing-change kind) |
| 8 | Precheck closed codes (C1) | `TestC3HTTP_ClosedRefusalCodesOnTheWire_UIReachable` (ceiling_not_enabled_for_country, tenant_without_licence_or_basis, currency_not_active_asset, licence_unknown, duplicate_jurisdiction_code, brand_not_owned foreign == unknown; exact status, generic code, one check code, counts unchanged); spec 13 `(i1)`, `(i2)`, `(i3)` exact UI wording copied (not imported) | HTTP + Playwright | pass |
| 9 | platform_officer_required (C1 correction of lic32 finding 5) | spec 13 `(i4)`: real tenant-originated grant (tenant admin requests, PA_B approves), tenant compliance POST -> 403 `["platform_officer_required"]`, nothing stored; UI offers no authoring to that role. C1's Go matrix not duplicated | Playwright (real stack) | pass |

"Dbprobe": `tools/dbprobe` only implements `market-evidence`; the unchanged-database assertions use the owner-role psql ground-truth helpers of
`lib/db.ts`/`lib/wd_db.ts` (`changeFingerprint`, `globalCounts` in `lib/mc_gaps.ts`), the same path specs 11/12 use. `tools/mcprobe` was
extended additively (`resolve <tenant> <country> <operation> [brand|-] [product|-]`).

## Clock control (no production code touched)

1. `Config.Clock` (already injectable, used by existing tests) skewed into the past: Go believes the approval is fresh / the request open, so only the
   database can refuse (proves LC020, LC031, LC013 are enforced in the database).
2. `c3Shift` / `shiftChange` (owner path, PRIVATE test database only): in one transaction the change table's own guard trigger (and, for approvals, row
   level security, which has no UPDATE policy even for the owner) is switched off for ONE UPDATE of `created_at`/`expires_at`/`decided_at` and switched on
   again before COMMIT (transactional DDL). Every test re-asserts both triggers `tgenabled='O'` and RLS enabled+forced on both tables. The runtime role cannot do this.
   Worth a security-reviewer glance because it is the only place a test disables a guard.

## Findings (tests only; nothing fixed)

- **F1 (characterisation, pinned):** expiry is recorded lazily. An expired, untouched request reads `status=requested` in the database and the API
  (`TestC3_Expiry_72h_...`); the Back Office renders `(expired)` from `expires_at` and disables Approve (spec 13 `(e)`), a client MUST do the same.
  The requester can still cancel such a request (`TestC3_Expiry_CHARACTERISATION_...`).
- **F2:** a product-scoped (or any more specific) operation-rung ENABLE under a broader disabled in-force row is accepted at filing and by the first approver, and
  only refused at the FINAL approval (execution) by the database trigger as a domain refusal (the second approver gets 409 `LICENSING_CHANGE_REFUSED`
  `policy_exceeds_permitted_scope`; the request stays open with 1 approval until cancelled). Safe (nothing written, resolver unchanged, pinned by
  `TestC3_ProductScopedOperationRung`), but approvers spend effort on a request that cannot execute; a precheck would be friendlier. Product decision.
- **F3:** a stale approval (>24 h) makes a request permanently unexecutable (a third Person cannot rescue it: `ErrApprovalsStale` again); the only exit is
  cancel + refile. The UI says so; pinned by `TestC3_Freshness_StaleApprovalsDoNotCount_GoLayer` and spec 13 `(d)`.
- **F4:** an "unassigned" country code of valid shape (`XX`) is refused only as the generic 400 `validation_error` with no closed code (`TestC3HTTP_ClosedRefusalCodesOnTheWire_UIReachable`
  logs the observation); C1 defines no code for it. Not a defect, a gap in the closed set if the orchestrator wants one.
- Not covered here: UI rendering of `session_revoked` (401) in the licensing dialogs (Go wire only); `tenant_without_licence_or_basis` in the UI (needs a licence-less tenant;
  Go wire only); tenant-originated filing of configuration/brand kinds beyond what C1 and lic32 already pin; the step-up PROMPT is exercised implicitly by `clickWithStepUp`
  in every UI test of spec 13 (60 s window) but not asserted as its own case (spec 06(h) does).

## Intentional pin changes

- `backoffice/e2e/specs/11-market-config.spec.ts`, two assertions, each commented in the file: (c)(1) the precheck refusal text, from "The change was refused by the domain rules" to
  the exact C1 wording ("... has no enabled country ceiling for this country"); (e) the duplicate jurisdiction code text, from "rejected the request as invalid" to
  "A jurisdiction with this code already exists". Cause: the C1 merge (closed codes with exact wording). Status and generic code assertions in spec 11 are unchanged; nothing weakened.
- Additive: `backoffice/e2e/lib/mc_gaps.ts`, `backoffice/e2e/specs/13-market-config-gaps.spec.ts`, README rows, `mcprobe` optional args.

## Files

New: `internal/licensingchange/c3_market_rungs_integration_test.go`, `internal/licensingchange/c3_freshness_audit_concurrency_integration_test.go`,
`internal/httpserver/c3_licensing_gaps_integration_test.go`, `backoffice/e2e/specs/13-market-config-gaps.spec.ts`, `backoffice/e2e/lib/mc_gaps.ts`,
this record. Edited: `backoffice/e2e/tools/mcprobe/main.go`, `backoffice/e2e/README.md`, `backoffice/e2e/specs/11-market-config.spec.ts` (two assertions).

## Commands and results

Private databases `c3_main` (Go) and `c3_e2e*` (Playwright, fresh per run), dropped at the end.

```
PRIV_DB=c3_main PRIV_SRC=<worktree> bash priv_db.sh
bash priv_test.sh -tags integration -count=1 -p 1 -v -run 'TestC3_'     ./internal/licensingchange/   -> exit 0, 16 PASS
bash priv_test.sh -tags integration -count=1 -p 1 -v -run 'TestC3HTTP_' ./internal/httpserver/        -> exit 0, 6 PASS
bash priv_test.sh -tags integration -race -count=1 -p 1 -run 'TestC3_Concurrency|TestC3_Freshness|TestC3_Expiry' ./internal/licensingchange/ -> exit 0
bash priv_test.sh -tags integration -race -count=1 -p 1 -run 'TestC3HTTP_Concurrent' ./internal/httpserver/ -> exit 0
bash priv_test.sh -tags integration -count=1 -p 1 ./internal/licensingchange/   (whole package, before the C1 merge) -> ok 88s
go vet -tags integration ./internal/licensingchange/ ./internal/httpserver/ ; gofmt -l -> clean
go test -count=1 ./internal/licensingchange/ ./internal/httpserver/ ./internal/actorproof/... ./internal/auth/... ./internal/rg/ -> ok
E2E_SPEC=13- bash backoffice/e2e/run.sh (fresh DB, before the C1 merge, 8 tests) -> run.sh exit=0 (8 passed, 4 fixme)
bash backoffice/e2e/run.sh (FULL, specs 01-13, fresh DB c3_e2e4, tip of this branch) -> run.sh exit=0, 80 passed, 0 failed, 0 skipped, 0 fixme (47.8 min)
```

## Playwright results

Final FULL run (specs 01-13, fresh private database, tip of this branch after the C1 merge): **80 passed, 0 failed, 0 skipped, 0 fixme** (47.8 min, `run.sh` exit 0).
Specs 01-12: 68 passed (unchanged count; spec 11 with its two intentional wording assertions); spec 13: 12 passed ((a) to (h) = 8, (i1) to (i4) = 4).

History, not hidden: (1) spec 13 alone, before the C1 merge: 6 passed + (a) failed on a SQL operator-precedence slip in my helper, (e) failed on re-opening the same URL (cached
query); both fixed, 8/8 on a fresh DB. (2) first FULL run after the C1 merge and the placeholder fill: 77 passed, 3 failed, all in spec 13 and all test-side: (a) and (b)
used countries SE and FI which spec 06 had already given an operating basis on the shared synthetic tenant (resolver `not_permitted_by_basis` instead of `permitted`), and
(i4) looked the tenant compliance grantee up with a platform-scoped query (tenant staff rows are RLS tenant-scoped). Fixed (countries EE and LT, `sqlT`), then the final
full run above. The C1 fill is NOT weakened: the exact wording strings in spec 13 are copied, not imported.

Not run: other browsers (Chromium only), a real provider.

## Cleanup

Private databases `c3_main`, `c3_e2e1` to `c3_e2e4` dropped; `run.sh` removed its run directory and the `node_modules` symlink.
