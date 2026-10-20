# lic17 record: withdrawal restriction policy fixes (migration 0184)

Branch `lic17-withdrawal-fixes` from integ-b `5503334` (lic9 + lic10 + lic11 + lic14). Input: the independent security review of
lic11 on the integrated tree (BLOCK until F-1) and the ledger-finance review addendum (M-1..M-3, L-1..L-4).
MONEY-PATH GOVERNANCE: `security`, `ledger-finance` and `code-reviewer` must review before merge. This is a record, not an ADR. It
proposes no ADR text, and no legal or regulatory requirement is claimed or invented. No real money and no real provider are
involved (MOCK only). The licensing model stays HYBRID: platform licence, partner licence per brand, and documented no-licence
bases are all evaluated.

## 1. Deliverables

| Item | Label | What changed |
|---|---|---|
| F-1 HIGH (R-7): evaluate EVERY licence relied on | IMPLEMENTED | `operatingmarket.TenantBasisReliance` (new, read-only, tenant-scope asserted) reads the approved (`change_request_id IS NOT NULL`), active, in-force `operating_bases` rows of the tenant, tenant-wide or of the brand. `withdrawal.EvaluateRestriction(ctx, tx, tenantID, brandID, stage)` (signature now carries the brand) evaluates `tenants.licence_id` plus every basis licence with `jurisdiction.EvaluateLicenceValidity`. All causes in force are listed and the strictest configured action wins. A documented no-licence basis contributes no cause. `licence_missing` applies only with no licence AND no basis. |
| F-2 MEDIUM: first configured `allow` over the default is a loosening | IMPLEMENTED | 0184 `withdrawal_restriction_is_widening` returns true when `p_action = 'allow'` and the effective source at any stage is `default_unconfigured`. Go `withdrawalpolicy.IsWidening` matches. Down restores the 0182 body byte for byte (verified, section 4). |
| F-3 LOW: status key at the top of `evaluateRedispatchGate` | IMPLEMENTED | `tenant.ShareStatusGateKey` (`pg_advisory_xact_lock_shared(tenant_status_gate_key)`) is the first statement of the T2 / T12 gate. That is the ADR 0082 position: status keys before L0.4 and before the L1 row lock. The helper lives in `internal/tenant`, so the payments advisory static pin (`TestSweeperAdvisoryLock_OnlyInsideClaimBatch`) stays intact. |
| F-4 LOW: with no policy rows, an unreadable licence must not 503 | IMPLEMENTED | Set version 0 and an unreadable or unmapped licence state give `allow` / `default_unconfigured` / `restriction_licence_state = unknown` (`LicenceUnknown`). With policy rows, the evaluation fails closed (`ErrGateUnavailable`). The reliance read runs in a SAVEPOINT, so a failed read never aborts the caller's transaction. Static pin: `TestLic17_Static_ValidityCauseCoversEveryValidityAndStatus`. |
| M-2: LicensingInvalid / visibility LicenceNotFound is UNAVAILABLE, not `licence_missing` | IMPLEMENTED (reconciled with F-4) | `validityCause`: LicenceNotFound and LicenceNotBound mean unknown. A LicensingInvalid chain means unknown. Unknown with policy rows is fail closed; unknown without policy rows is allow, recorded as unknown (F-4). Never `licence_missing`. |
| M-3: bind the policy functions to the session tenant (AM026 pattern) | IMPLEMENTED | 0184 adds `withdrawal_restriction_assert_scope(p_tenant)`, which raises WR003. Both `withdrawal_restriction_effective` and `withdrawal_restriction_set_version` call it first. It refuses a player or acting session, tenant + platform GUCs together, another tenant, and no scope. A platform session (no tenant) may read any tenant. |
| L-1: consistent set version + action at T2 / T12 | IMPLEMENTED (by F-3) | The whole re-dispatch tick holds the status key SHARED, and policy writers take it EXCLUSIVE, so the set version and every effective action are read under one version. The request and T1p already held it (lic11). |
| L-2: a non-allow registry default cannot produce a malformed WD056 row | IMPLEMENTED | `RestrictionDecision.assertRecordable()` runs on the request path and at every dispatch point. A refusal must be source `configured`, have a configurable deciding cause, and an action of `hold_for_review` / `block`; otherwise it returns `ErrGateUnavailable`. A database pin checks that every configurable registry default is `allow` today. |
| L-3: KYC rulebook resolution under player scope | IMPLEMENTED | `kyc.resolveEnforcementJurisdiction` clears `app.player_account_id` for the tenant-scoped basis / licence / rulebook reads and restores it, as `rg.tenantJurisdiction` does. A restore failure is an error (the caller answers `unavailable`). Tenant id assertions are unchanged. |
| L-4: `marketgate.prefilterByCeiling` swallowed errors | IMPLEMENTED | It returns `([]string, error)`. Every read, scan or iteration error, including a tenant row that is not visible (ErrNoRows), is returned, and `AnyCountryPermits` returns it ("could not decide" = closed). A tenant with no licence keeps the unfiltered list (unchanged: the resolver decides). |
| M-1: `hold_for_review` can strand funds | PARTIALLY IMPLEMENTED | IMPLEMENTED: the read API states the semantics plainly (`action_semantics`) and reports `parked` (count, `oldest_parked_since`, `oldest_age_seconds`) for a tenant caller. A platform caller gets `visible: false` and a pointer to the WD056 alerts, whose `first_seen_at` is the park start. The runbook states the only exits. NOT IMPLEMENTED: a governed per-withdrawal release, and an automatic age-threshold alert (residuals R-M1, R-M1b). Rationale in section 3. |
| R-8: HTTP integration tests for the 8 routes | IMPLEMENTED | `TestLic17_R8_WithdrawalRestrictionPolicyHTTP_AuthzScopeValidation`: permission (403), scope / tenant isolation (404), step-up class (critical refuses a stale factor, none does not), request validation (400), domain refusals (409 codes), and the four-eyes flow over HTTP. |
| F-5 / F-6 | NOT IMPLEMENTED (residual) | The coordinator did not provide the text of F-5 / F-6. They are recorded in section 5 as open residuals; nothing was guessed. |
| Pre-0182 schema tolerance (found by the full `internal/withdrawal` run) | IMPLEMENTED | A lic11 defect, present on integ-b: the five migration up / down / up tests (0132, 0162, 0165, 0169, 0171) run `RequestWithdrawal` on a schema rolled back below 0182 and failed (`withdrawal_restriction_set_version` does not exist, then the 0182 provenance columns). Now the evaluation probes the policy table and function: if NEITHER exists, no policy can exist, so the result is the pre-0182 behaviour (`SchemaAbsent`, no provenance; `RecordDispatchGateDecision` writes the pre-0182 column list). A partial schema fails closed. This matches the lic14 precedent for pre-0180. |
| Runbook, OpenAPI | IMPLEMENTED | `docs/runbooks/withdrawal-restriction-policy.md` (sections 1a, 1b, 2, 3, 4, 5, 6); `docs/api/openapi/platform-api.yaml` (`action_semantics`, `parked`). No route was added or removed, so the route-freeze / golden tables are unchanged. |

## 2. Intentional behaviour changes (assertions changed, said plainly)

1. **`licenceCause` became `validityCause` (M-2).** `TestLic11_LicenceCause_Mapping` asserted LicenceNotFound and LicenceNotBound
   meant `licence_missing`. It is replaced by `TestLic11_ValidityCause_Mapping`, which asserts they are UNKNOWN. This is the
   intended fix: an unreadable licence is not a missing licence. The tenant with no licence and no basis still derives
   `licence_missing` (pinned by `TestLic17_ReliedCauses` and `TestLic17_F1_NoBases_Unchanged`).
2. **`withdrawalpolicy` unit `TestIsWidening_Matrix`.** The existing case "allow over allow is not widening" now runs over a
   CONFIGURED allow (`cur()` sets source configured). A new `def()` helper asserts the F-2 rule over the default. No case was
   removed.
3. **lic11 runbook correction.** The lic11 runbook said a parked request could be "rejected (releases the hold)". That was
   WRONG: `withdrawal.Reject` accepts `pending_review` only. Corrected in runbook section 2 (see M-1).
4. **Step-up golden pin (`TestStepUp_GoldenTable_EveryClassifiedRoute`).** Integ-b commit `9c415a4` summed lic11 into the
   critical count only (78), but left none-mutating 44 / read 101 while its own comment says 46 / 105. The test failed on integ-b.
   The pin now asserts 46 / 105, the documented composition. No route changed, and this is not a weakening: the counts are exact.
5. **`marketgate.AnyCountryPermits`** returns an error where it previously continued with the unfiltered list (L-4). Callers
   already treat an error as closed.
6. **`withdrawal_restriction_set_version`** is now plpgsql (it was SQL), because it needs the scope assert.

## 3. M-1 decision: no per-withdrawal release in lic17 (justified)

The review preferred a minimal governed per-withdrawal release where it fits. It does not fit without an owner decision:
- A release that DISPATCHES a parked withdrawal despite the configured policy is an override that bypasses an applicable
  restriction. The owner instruction (lic11) forbids that, and who may grant it, on what evidence, is a policy decision this
  slice must not invent.
- A release that returns the hold to the PLAYER for an ACTIVE tenant would extend HSEC-APPROVED-HOLD-RELEASE-1 (ADR 0111,
  `release_hold_to_player`). That path is deliberately limited to a non-active tenant or brand (A-19,
  `tenant_brand_active_again`) by owner decisions ADR 0095 section 44 d13-18. Changing that eligibility changes an owner-decided
  authority and a security-reviewed capability (M-9: a new kind needs a security review).
- Both are irreversible-authority choices and belong with the owner, security and ledger-finance, not a fix slice.

What lic17 does instead: it states the truth everywhere an operator looks (API `action_semantics`, runbook section 2) and makes
the parked population and its age visible (API `parked`; the existing WD056 alert's `first_seen_at` for platform operators).
The funds are never at risk of loss: they stay on `player_withdrawal_hold`, and the ledger is balanced and unchanged by a park
(`TestLic17_M1_ParkedSummary_AndHoldForReviewSemantics` asserts the hold balance, `loAssertBalanced` and projection = rebuild).
They can be stranded only while the cause stays in force and nobody widens the policy, and that is now visible and documented.

## 4. Verification (commands and results)

Private scratch DB `lic17100507p2484x01` (harness `priv_db.sh` / `priv_test.sh`), rebuilt from this worktree's migrations
(0001..0184).

- 0184 up / down / up: the down restores the `prosrc` of `withdrawal_restriction_effective`, `_set_version` and `_is_widening`
  byte for byte. The md5s match the bodies in the 0182 file: `ec92528d...`, `0a263517...`, `e2d157c4...`. The down drops
  `withdrawal_restriction_assert_scope`. Re-applying the up gives an identical function fingerprint (`APP_ENV=development
  go run ./cmd/migrate down|up`).
- New integration tests (all PASS):
  - `internal/licensingchange`: `TestLic17_F1_PlatformLicenceBasis_SuspendedLicenceIsTheCause` (with and without a configured
    block), `TestLic17_F1_PartnerLicencePerBrand`, `TestLic17_F1_DocumentedNoLicenceBasisOnly`,
    `TestLic17_F1_StrictestWinsAcrossReliedLicences` (both orientations), `TestLic17_F1_NoBases_Unchanged`,
    `TestLic17_F4_M2_UnreadableLicence_UnknownWithoutPolicy_FailClosedWithPolicy` (LicensingInvalid chain, LicenceNotFound,
    failing basis read; the failure is induced by a TEMP shadow table in a fresh OWNER connection, since the runtime role has no
    TEMP), `TestLic17_L3_KYCEnforcementUnderPlayerScope`. Run twice (random map order): PASS.
  - `internal/withdrawalpolicy`: `TestLic17_F2_FirstConfiguredAllowOverTheDefault_NeedsTwoApprovers`,
    `TestLic17_M3_RestrictionFunctionsAreBoundToTheSessionScope` (8 session shapes x 2 functions, plus the Go evaluator gives
    ErrGateUnavailable), `TestLic17_L2_RegistryDefaultsAreAllow`.
  - `internal/payments`: `TestLic17_F3_RedispatchGate_SerialisesWithPolicyWriters` (lockorder harness: `policy_writer_first`,
    where the tick waits on the EXCLUSIVE key and then sees the committed block; and `tick_first`, where the tick, parked at L1,
    holds the key SHARED, the writer waits, and both commit with no deadlock). Discrimination check: with the
    `ShareStatusGateKey` call disabled, both subtests FAIL; restored afterwards. Also
    `TestLic17_M1_ParkedSummary_AndHoldForReviewSemantics`.
  - `internal/httpserver`: `TestLic17_R8_WithdrawalRestrictionPolicyHTTP_AuthzScopeValidation`; `TestStepUp_GoldenTable_EveryClassifiedRoute` (pin completed, PASS).
- New unit tests: `TestLic17_Static_ValidityCauseCoversEveryValidityAndStatus`, `TestLic17_ReliedCauses`,
  `TestLic17_L2_RefusingDefaultIsNeverRecordable` (withdrawal); `TestLic17_L4_PrefilterByCeiling_ReturnsReadErrors` (marketgate);
  F-2 cases in `TestIsWidening_Matrix` (withdrawalpolicy).
- Regression suites re-run on 0184: the lic11 suites (withdrawal, payments, withdrawalpolicy) and the lic14 hardening suites
  (licensingchange). The full-package results are in the hand-back report.

Commands:
```
export PRIV_DB=lic17100507p2484x01 PRIV_SRC=/home/user/igaming-platform/.claude/worktrees/lic17
bash <scratchpad>/priv_db.sh
bash <scratchpad>/priv_test.sh -tags integration -count=1 -p 1 ./internal/licensingchange ./internal/withdrawalpolicy ./internal/withdrawal ./internal/payments ./internal/kyc ./internal/marketgate ./internal/httpserver -run '...'
gofmt -l cmd internal; go vet ./...; go vet -tags integration ./...; go test -count=1 ./...; go test -count=1 ./internal/db
```

## 5. Residuals (open; for the orchestrator)

- **R-M1 (owner decision): a governed per-withdrawal release for WD056-parked withdrawals of an ACTIVE tenant.** It needs:
  release to the player vs to dispatch (the latter is an override of the restriction); the authority (platform / tenant
  compliance; distinct Persons; grant model); the evidence; and whether a release survives a policy change. A
  release-to-player path would most naturally extend ADR 0111 `release_hold_to_player` eligibility with a WD056 precondition.
  That is a security-reviewed change (M-9). It also needs a new path for a never-sent escalated attempt (M3 has no caller today).
- **R-M1b: an automatic age-threshold alert for withdrawals parked longer than N.** It is not built (it needs a periodic
  scanner and a new alert Kind in the closed vocabulary, plus a migration). Today the age is visible through the API `parked`
  field and the WD056 alert's `first_seen_at`. The runbook's N = 72 h is an ENGINEERING DEFAULT, not a legal figure; the owner
  sets it.
- **F-5, F-6:** listed by the coordinator as residuals to record; their text was not provided to this slice. OPEN; to be
  filled from the security review.
- **Platform visibility of parked counts:** `withdrawal_gate_decisions` is tenant-scoped (RLS), so the platform read reports
  `visible: false`. A platform-wide parked view would need a platform-scoped read policy (a security decision).
- **Unchanged from lic11:** Q12 (the interim default itself) and the launch-readiness informational item (lic11 R-5).
