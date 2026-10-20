# Licensing/jurisdiction configurable operating model: stage completion report (2026-10-20)

Branch `claude/licensing-jurisdiction-completion` (pushed; no PR; not merged into any protected branch). **Verified code commit: `f121436adacb0043166dd36ccb6bf0c999440d6f`** (a later commit may add this report and registry rows; documentation only). Frozen baseline `1505a165e9152729b5df573211c80b1c0a35bc87` untouched. Migration head at baseline **0175**, final head **0186**. All providers MOCK; AWS and real providers untouched. This is not a legal opinion; no jurisdiction or licence is approved and no country/threshold/policy value is shipped.

## 1. Verdict
Technical stage: **accepted on the evidence below, with disclosed limitations (section 8)**. No legal or business decision blocks engineering; several block enabling things for real money (section 9).

## 2. Delivered (components)
- Operating bases (0180, 0183): platform licence / partner licence / explicit documented no-licence basis per tenant, optional brand, country, operation, product; evidence, mandatory review-due, withdrawal, versioning, two-approver widening, rulebook jurisdiction follows the basis (ADR 0124).
- Licence lifecycle + governed licensing changes + read APIs + Back Office (0176-0179; earlier report).
- AML screening triggers (MOCK), withdrawal screening gate with production guards, officer routes, trapped-funds alert; residence maker-checker per tenant; KYC policy authoring with signed Person-distinct writes (0181, 0185, 0186; ADR 0126).
- Per-tenant withdrawal restriction policy for licence causes (0182, 0184; ADR 0125).
- Back Office screens: operating bases, KYC policies, residence approval, withdrawal restrictions, AML screening; real-stack Playwright e2e harness (`backoffice/e2e`).
- Records: docs/plans/lic9..lic24 records; ADRs 0123-0126; decision register and task registry rows.

## 3. Test evidence (exact)
- **Full local integration suite, tree `c59bc38`:** `go test -tags integration -count=1 -p 1 -timeout 170m ./...` (private freshly migrated DB, head 0186), 2026-10-20 14:53:48Z to 17:25:00Z, exit 1. 81 packages ok (httpserver 1281 s, payments 5322 s); **one failure**: `TestR32_Migration0127_BackfillsExistingDestinationParks`. Cause: the test migrates to an old schema then calls the production withdrawal path that now reads the lic11 policy function. Fixed in lic24 (integration-build-only switch, pinned by a static test); the three R32 migration tests and the affected subsets passed after the fix; the **full payments package was not re-run** after the fix (hosted CI shard `pay-r` was).
- **Commits after `c59bc38` (tested subsets only, not the full integration run):** merges of the lic21/lic22 e2e branches (test code, e2e tools), lint fixes, lic24. After them: `gofmt`, `go build ./...`, `go vet ./...`, `go vet -tags integration ./internal/... ./cmd/...`, `go test ./...` (unit) clean; `launchgov`, `withdrawal`, `residenceapproval`, `operatingmarket` unit/integration subsets passed.
- **Race-detector reproduction:** `-race -run '^TestR' ./internal/payments` (1299 s): only the R32 migration test failed (the CI failure). Shard `core-rg-recon-aml` packages with `-race` (rg, reconciliation, aml, launchgov, tenant): all ok (exit 0).
- **Real-stack Back Office e2e** (built UI + real platform-api + real migrated private Postgres, Chromium): lic21 37/37 (specs 01-08, 11.1 min) and lic22 33/33 (specs 01-06, 09, 10, 20.2 min), both on the merged tree `c59bc38`, run by the agents (not re-run by the orchestrator). Covers: licensing and operating-basis workflows incl. step-up prompt; KYC policy authoring (author/same-Person/unlinked refusals, activation, supersede, real mock-PSP deposit enforcement); residence maker-checker incl. 33 runtime-role bypass attempts with SQLSTATE and unchanged-DB assertions; withdrawal restriction (default/tighten/loosen/block/hold_for_review/tenant suspension/basis-reliance, ledger invariants); AML (heartbeat refusal, mock worker, needs_officer, sanctions rules, hold lifts, runtime-role boundaries). UI-level mock tests (vitest, MSW) are separate: 952+ tests, build passing.
- **Migrations:** `migrate up` from empty to 0186 (every DB build); `go run ./cmd/migrate verify` OK through 0186 (no gaps, no drift); up/down/up snapshot tests for 0176-0186 passed in the suite; CI job "Migration reversibility (down then up)" green.
- **Hosted CI on `f121436`** (run 39469518309): attempt 1: 25/27 (failed: shard `core-rg-recon-aml`; `integration-evidence` as consequence); **attempt 2 (rerun of failed jobs): 27/27 success**. Earlier tips: 218daaa lint failed (fixed), 46a9c76 `pay-r` failed (fixed by lic24).

## 4. Independent reviews and findings
All implementers were reviewed by agents that did not write the code (security, ledger-finance, code review by orchestrator diff reads). Findings fixed: lic9 H-1/M-1/M-2 (lic14); lic10 S1-S4/H-1(ledger) (lic18/19/23); lic11 F-1 HIGH wrong-licence evaluation, F-2, F-3, F-4 (lic17); final-tree confirmation: ledger-finance N-1..N-9 and security SN-1..SN-6 (lic23). Final-tree confirmation verdicts: ledger invariants untouched, no new HIGH from security. Remaining accepted/open items in section 8. Specialist reviewers confirmed the lic23 fixes only by static reading; the lic23 fixes were then tested by their authors and the suites above, but **no reviewer re-read the final tree after lic23/lic24**.

## 5. Governance
Frozen baseline preserved; separate branch; no PR; no merge to protected branch (approval gate untouched). Incident: a parallel e2e agent ran `pkill -f x` once (against the no-pkill rule); no observable impact (other runs completed); disclosed in lic22 record.

## 6. A/B/C/D classification of outstanding items
**A. Engineering that can and should be completed now:** RG floors scope-aware by basis jurisdiction (F1; interim: acknowledgeable readiness fail `bases_present_licence_rulebook_only`); Back Office filing for ceilings/operating policies/jurisdiction configs and jurisdiction creation; tenant read API for own operating bases; screening policy (interval/grace) HTTP route and Back Office fields; closed machine-readable error codes for generic 409/400 refusals (heartbeat, sanctions clear, LC codes); show parked count/age in the withdrawal-policy screen; AML transaction-monitoring configurable mechanism (empty by default); system actor for the screening worker (pre-real-provider); full payments re-run after lic24.
**B. Future business configuration:** country/operation/arrangement list per tenant/brand (operating bases), withdrawal policy values per licence cause, KYC thresholds, RG floors, residence mode per tenant, AML grace/interval/trapped-alert age, alert recipients/routes.
**C. Legal/compliance determinations:** whether a country needs a licence and whether a no-licence basis is valid and for how long; sanctions false-positive resolution policy and whether a review expires; monitoring-at-launch acceptance; withdrawal treatment under licence suspension; residence vs location; KYC/RG/AML legal values; privacy review of the match-reference fingerprint.
**D. Genuine blockers:** none for provider-independent engineering. Provider-dependent: production KYC vendor, screening/monitoring vendor, geolocation if required, PSP/casino/sportsbook credentials. AWS-dependent: runtime-role split, kill switch, backups, environment anchor in a deployed environment.

## 7. Business/legal decisions (separate from blockers)
Register rows: LIC-OPBASIS-1, WD-RESTRICT-1 (incl. governed per-withdrawal release for hold_for_review, not built), AML-GATE-1, KYC-THRESH-1, RESIDENCE-MODE-1 and the earlier LIC-Q1..Q25 (Q1 country list is operational, not a blocker).

## 8. Remaining risks and limitations (honest)
- `core-rg-recon-aml` hosted shard failed once on `f121436` attempt 1 and passed on rerun; not reproduced locally (race run exit 0). Treated as a **disclosed intermittent**, cause unknown (hosted log truncated), plus the earlier disclosed `TEST-DCONC-INTERMITTENT-1`.
- Full payments integration suite not re-run on the final commit locally (hosted CI shards cover it: green on attempt 2).
- Migration 0181 was edited in place twice (unreleased); any dev/staging DB that applied an earlier 0181 must be rebuilt, never rolled back.
- RG floors/payout rulebook for licence+basis tenants still licence-jurisdiction based (F1); sanctions review never expires as built; worker API proof-less (MOCK only; hard prerequisites before any real provider); Back Office lic16/lic20 screens for the policy areas are covered by real-stack e2e for KYC, residence, withdrawal and AML flows through the UI only where the specs say so (see lic21/lic22 records "not covered"); `hold_for_review` has no per-withdrawal release; trapped-funds alert pages nobody until an alert route exists.
- Not performed: any AWS RPO/RTO claim, real provider validation, browser coverage other than Chromium, load/performance testing.

## 9. Merge recommendation and next item
Recommend review and merge to the integration line only through the project's approval gate (not done here). Next provider-independent item chosen from class A: **F1, scope-aware RG floors for basis-governed scopes** (dependencies satisfied by ADR 0124 rulebook selection); then the Back Office/API items listed in A.
