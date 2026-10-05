# PRH-2 FINAL GATE REPORT (round 2) - 2026-10-05 - STOPPED, awaiting owner authorization

Supersedes the status sections of `final-gate-report.md` (first gate). Nothing here is an approval, a legal/licensing claim or a statement that PRH-2 is complete. All providers MOCK. AWS OFF. GitHub CI: no evidence (CI-BILLING-1). All evidence below is LOCAL.

## 1. Exact HEAD
Code tip `f9dc8a3` (branch `claude/focused-wright-jw88w9`); this report and the status/evidence documents are the next commit on top of it (see `git log -1` on the branch for the final pushed SHA). No code changed after `f9dc8a3`.

## 2. Migrations
`0001..0117`, 117 up/117 down pairs, gap-free (`go run ./cmd/migrate verify`: all applied migrations verified clean). New this round: **0116** `revoke_temp_from_runtime`, **0117** `alert_routing_readiness`. 0118 reserved and UNUSED (the PSP prerequisites needed no migration).

## 3. A / E / F status (all merged, all LOCAL evidence, MOCK providers)
| WS | Branch tip merged | Content | Reviews | Mutation |
|---|---|---|---|---|
| A | `prh2-r2-temp-revoke` @ `8de2f4d` | 0116 + ADR 0108: TEMPORARY revoked from PUBLIC and `igaming_runtime`; in-migration assertion; production startup gate (TEMP/ownership/memberships); `migrate down` refused outside development/staging; provisioning scripts carry the revoke | security (impl+delta), ledger-finance, code-review, QA: all ACCEPT/READY WITH CONDITIONS, code conditions applied | killed except classified equivalents/test-weakening |
| E | `prh2-r2-alert-routing` @ `a00177f` | 0117 + ADR 0102 §18: routing readiness, smallest cut | security (impl+delta), code-review | 53/53 killed |
| F | `prh2-r2-psp-prereq` @ `88269f1` | statement-source wiring (per provider), M-S2/M-S1 standing, `yAttributable` + shared-Y + G-Y2; no migration | ledger-finance (impl+delta), security (impl+delta), code-review | all killed except classified equivalents |
Integration findings fixed at merge time (not in any branch-local run): `internal/providercred` allowlist (earlier), and the E test-fixture `World` helper tripping the A-14b and E route-seeding static guards (moved to the allow-listed `internal/testsupport/alertworld`; guards NOT loosened beyond one single-file exemption; commits `31ff530`, `f9dc8a3`).

## 4. TEMP security status (TRIGGER-SEARCH-PATH-1)
Exploitation path CLOSED in the code for the runtime role and other non-owner roles. Verified explicitly (30 tests PASS, `evidence/prh2-r2-temp-security-verification.txt`): PUBLIC and `igaming_runtime` cannot create TEMP (all 7 statement forms incl. DO blocks, SET LOCAL search_path); the K2 forged-approval attack fails at TEMP creation and posts no entry; non-vacuity test (TEMP granted back in a throwaway DB -> the forgery executes); a genuine two-person adjustment still executes exactly once (self-approval MA031, ungranted MA003, replay refused); production gate fails closed; `down` guarded end to end. **The item stays a LAUNCH BLOCKER until deployed and verified per environment** (runbook §7 step 5 verification SQL; service connects as `igaming_runtime`, never the owner - PLAT-ROLESPLIT-1; 0116 applied before runtime traffic or backends recycled). NOT IMPLEMENTED: pinning the 0026..0113 functions (defence in depth). Only `security` may lower the rating.

## 5. Alert delivery
ALERT-DELIVERY-1 **OPEN**. Routing readiness smallest cut IMPLEMENTED/MOCK: routes disabled by default, DB guard refuses any human-notification route, unrouted rows with reasons, retryable/permanent split + severity budgets (real windows documented: dead after ~3-4 min p1, ~1 min p2, ~30 s p3; no redrive), config-only readiness (always 0 today), platform-only route/status/list endpoints under `alert:route_manage`, refusal audit, MOCK test channel + conformance suite. NOT IMPLEMENTED/PROVIDER DEPENDENT: any real channel, any recipient, vendor, secrets, four-eyes route governance, enforce mode. No recipient was invented; log output is never a human notification. Operator inputs: H1 vendor, H2 recipients (HD-PRH2-4-OPS), H3/H4 on-call policy and windows, H5 enforce vs report, H6 secrets, H8 external monitor, H9 launch gating. Binding before the first human-notification kind: ADR 0102 §18.4.

## 6. PSP prerequisites (engineering vs real money)
Per ledger-finance delta ruling, recorded CLOSED AS ENGINEERING ITEMS AGAINST MOCK (registry `PAY-R2-CLOSURE-WORDING`): PAY-RECON-POLL-REF-CLEAR-1, PAY-K3-STATEMENT-SOURCE-WIRING-1, PAY-RECON-PARKED-CAPTURE-STANDING-1. **The real-money precondition is NOT satisfied**: a real PSP statement source is NOT IMPLEMENTED/PROVIDER DEPENDENT; **H-W1** (statement sweep covers active tenants only while M2 is permitted on a closed tenant) is an OPEN HUMAN DECISION and LAUNCH-BLOCKING for a real-money tenant on a real PSP; R-S1 (removing a source silently ends its standing findings); guard-list coverage test only builds the mock config (add real-source coverage with the first real PSP); ALERT-DELIVERY-1 gate of ADR 0095 §35.4. MA020-SYNC-MISMATCH-1 stays OPEN and must reuse `yAttributable` + G-Y2. No new financial semantics were introduced; every change fails closed.

## 7. Timing-lane result (500 ms bound UNCHANGED, ADR 0094 NOT edited)
- Earlier today, container instance reporting Xeon 2.80 GHz, 4 vCPU: `NormalOperation` failed 0/25 at HEAD (also 1/10 at `ecd2b74`, 2/10 at the pre-PRH-2 baseline `94b4ae5`); full characterization in `evidence/prh2-timing-lane-characterization.md`; first measurable shift attributable to migration 0115 (~10%), E1 no shift.
- Final run, idle, same code `f9dc8a3`, after a container restart onto an instance reporting Xeon 2.10 GHz: CI lane commands, `-race -count=1`, 5 reps = **40/40 PASS** (all 8 lane tests), plus 8 more `NormalOperation` reps all PASS with **worst callback 420-467 ms** vs 500 ms (`evidence/prh2-r2-final-*.log`).
- Conclusion: **environment-dependent with a thin margin; NOT resolved.** `TestResolutionIsolation_FinancialDuringOutage` passed in every idle run (13/13 earlier, 5/5 + final). Do not call the lane green without GitHub CI or a controlled runner. ADR 0094 amendment decision (environment-calibrated relative assertion / p95+ceiling / dedicated runner) is OPEN for owner/architect/security.

## 8. Unresolved human decisions
HD-CTF-1..9 (ADR 0107; none blocks a real PSP for active tenants), HQ-E1-1..4 (p2 severity stays; confirm before 0114 reaches a real tenant), H-W1, ADR 0094 amendment, TRIGGER-SEARCH-PATH-1 deployment verification + PLAT-ROLESPLIT-1, NULL-ARM-WRITE-1 (launch-blocking or not), ALERT-DELIVERY-1 operator inputs H1..H9, HD-PRH2-8, HD-PRH-1, HD-KYC-1..8, CI-BILLING-1, BRANCH-PROTECTION-1, ACCESS-ANALYZER-CHECK-1, HD-10.3-2, DEPLOY-FPKEY-1, HDR-J-7/8/9, HDR-M-1/2, HDR-SB-1, WD-RG-1 scope, vendor/custodian/contract/credential/launch decisions. Decision pack: `analysis/decision-pack-hd-ctf-hq-e1.md`. Nothing was invented or silently closed.

## 9. Complete test evidence (LOCAL)
- Build, `go vet` (default + integration tags), gofmt, golangci-lint 2.9.0 (default; integration-tag on code new since `95b17c5`): clean at `f9dc8a3`.
- `migrate verify`: 0001..0117 clean, no gaps.
- Whole repository, `-race -tags integration -count=1`, per-package resumable runner with the 8 timing-lane tests skipped (they are run separately, §7): **54/54 PASS** (`evidence/prh2-r2-race-sweep-f9dc8a3.txt`). The first pass had 6 ENVIRONMENT failures ('package ... is not in std' / testmain, coincident with a `go clean -cache` as builds started; toolchain healthy afterwards) that all PASSED on retry, and 1 real defect (§3) found and fixed earlier in the same run.
- Explicit TEMP security verification: 30 tests PASS (§4).
- Mutation evidence: `evidence/prh2-r2-temp-revoke-mutation-kill.txt`, `prh2-r2-alert-routing-mutation-kill.txt`, `prh2-r2-psp-prereq-mutation-kill.txt` (survivors classified); E1/K3 evidence files unchanged and still valid.
- NOT run: GitHub Actions (blocked), full-package `-race` of the 8 lane tests as a single sweep (run separately), any real provider, any AWS resource.

## 10. Remaining launch blockers
See `docs/HANDOVER.md` "Launch blockers (current)": TRIGGER-SEARCH-PATH-1 (until deployed+verified), PLAT-ROLESPLIT-1, H-W1, ALERT-DELIVERY-1 (+HD-PRH2-4-OPS), CI-BILLING-1 / TEST-RESISO-RACE-1, real PSP statement source and the first-real-PSP checklist, PAY-K3-FOLLOWUPS-1 (O-K3 column discipline, N13), PAY-K3-MR020-HTTP-MAPPING-1, KYC-ENFORCE-1 / KYC-E1-FOLLOWUPS-1, STAFF-LIFECYCLE-1, MANUAL-ADJ-LINK-1, NULL-ARM-WRITE-1, backup/DR NOT MET, CAS-RECON-SCALE-1, casino/sportsbook items, staging/AWS items.

## 11. AWS / GitHub CI
AWS OFF (no deploy, no Terraform, no IAM change). GitHub CI blocked by CI-BILLING-1 (billing untouched); the only CI-workflow edit this round is `APP_ENV=development` on the existing reversibility `migrate down` step (required by the new down guard).

## 12. Cleanup status
INVENTORY ONLY. About 740 scratch databases (~13 GB, growing: test-hygiene leak TEST-HYGIENE-1), 42+ worktrees (none locked/dirty; `worktree-agent-aae2d0af6d7fc8c3d` has 5 commits already superseded on main - archive as tag first), disk ~3 GB free. Deletion plan `analysis/cleanup-deletion-plan-NOT-EXECUTED.md`. Nothing deleted; only `go clean -cache` was used.

## 13. HANDOVER
`docs/HANDOVER.md` updated (migration range, TEMP status, alert status, PSP wording, blockers, human decisions, mock-vs-real alert row); registry rows appended (R2-A/E/F merge states, TRIGGER-SEARCH-PATH-1-0116, PAY-R2-CLOSURE-WORDING, H-W1, PLAT-ROLESPLIT-1, TEST-HYGIENE-1, final verification, timing, ENV-GO-CACHE-1); status notes in active-stage/progress/project-status. Handover completeness audit: `analysis` + registry `REGISTRY-HYGIENE-2026-10-05`; items marked 'to confirm' remain (PAYWH-RL-1, F-POOL-2 re-status, CP-W1 naming, Wave 4 status).

## 14. Recommended next step (needs owner authorization; nothing started)
1. Owner answers the ranked questions in the decision pack (esp. H-W1, TRIGGER-SEARCH-PATH-1 deployment path/PLAT-ROLESPLIT-1, ADR 0094 route, HQ-E1-2 confirm p2).
2. Restore GitHub CI (CI-BILLING-1) and obtain a CI run of this tree; only then can LOCAL evidence be complemented by CI evidence.
3. Authorize the cleanup (archive tag, then deletion plan) to remove the disk-pressure cause of the repeated test-infrastructure failures.
4. Only after 1-3: decide whether to authorize a real-PSP integration planning gate (first real statement source + coverage test + the first-real-PSP checklist). Do NOT start real-provider integration, AWS deployment or ADR 0107 implementation without explicit authorization.
