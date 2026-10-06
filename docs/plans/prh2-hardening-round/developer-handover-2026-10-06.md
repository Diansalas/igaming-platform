# PRH-2 developer handover snapshot, 2026-10-06

Dated, point-in-time companion to [`../../HANDOVER.md`](../../HANDOVER.md) (live) and [`../../runbooks/developer-handover.md`](../../runbooks/developer-handover.md). Facts come from Git at the stated SHAs, the task registry, ADRs, and the orchestrator's verified facts file. All evidence is **LOCAL**; there is no GitHub CI evidence (CI-BILLING-1). All providers are MOCK; AWS is OFF. **PRH-2 is NOT complete.** Nothing here is a legal, regulatory or licensing approval.

## 1. Git state

| Item | Value |
|---|---|
| Remote | `origin` = `github.com/Diansalas/igaming-platform` |
| Code branch | `claude/focused-wright-jw88w9` |
| HEAD | `d149a645b6187f0ebb45c7d67d4528153e910ef9` (`d149a64`: "docs: registry - final PRH-2 gameplay/security owner decisions") = origin at snapshot time |
| Last code-changing merge | `3e77297` (B4, migration 0119); `17882bf` (R3 gameplay gate, 0118); HEAD is docs-only since `6a7b5f5` |
| Docs branch with this handover | `prh2-handover-docs` (from `d149a64`, **not pushed**) |
| Agent worktrees | 52 under `.claude/worktrees/` (+ the one that wrote this): preserve, never auto-delete |
| Merged-and-retained branches | many `prh2-*` branches (e.g. `prh2-r4-classb-b1-b2`, `-b3`, `-b4`, `-b9-b10`, `prh2-r4-game-postings-nonactive`, `prh2-r3-closed-tenant-recon`, `prh2-r2-*`, `prh2-k1..k3-*`); do not delete without classification |

### Unmerged branches (READY, MERGE PENDING; their content is NOT in HEAD)

| Branch | Tip SHA | Commits beyond `d149a64` | Migration | Summary |
|---|---|---|---|---|
| `prh2-r5-signed-actor-proof` | `8863e31eb34df89ba775b6981cc1571437e84087` | `b063c9e` (feature), `19fb378` (review conditions), `8863e31` (security delta C1-C4) | `0120_signed_actor_proof` | HMAC signed actor proof verified by an owner-owned SECURITY DEFINER verifier; last-firing trigger `zz_actor_proof_guard` on 9 tables (K2, K3, K1 grants, financial policy changes). ADR 0110. New package `internal/actorproof`. Security + ledger-finance + code review APPROVE WITH CONDITIONS, conditions met (per orchestrator facts) |
| `prh2-r5-stake-return-closure` | `35cd2fb87fdd6890c51d1844842aa47cb82a3185` | `2136f43` (feature), `7d7cf16` (review conditions), `35cd2fb` (LF delta) | `0121_gameplay_stake_return_and_closure_guard` | Q-GP-5: terminal stake returns allowed on non-active tenants for valid existing rounds; Q-GP-1: tenant closure refused with open sportsbook bets (`GP020`); casino open rounds not representable (Q-GP-6 open); once-only casino rollback unique index. Security + LF APPROVE WITH CONDITIONS, met |

Merge rule: only after the full per-package `-race` sweep completes; after merging both, `go run ./cmd/migrate verify` must show `0001..0121` gap-free. Registry text reserves exactly these numbers (0120, 0121).

## 2. Migration state

HEAD: `migrations/` holds `0001..0119` (119 up/down pairs, 238 files), gap-free. Latest: `0117_alert_routing_readiness`, `0118_gameplay_posting_tenant_status_gate`, `0119_ma020_sync_mismatch`. Branches add 0120 and 0121 (above). Numbers are allocated only by the orchestrator. `migrate down` is dev/staging only (explicit `APP_ENV`). `verify` checks checksums and gaps only.

## 3. Race sweep state

- Scope: per-package `-race -tags integration -count=1 -p 1`, timing-lane tests skipped by name and run in their own lane.
- Result at `d149a64`: **53 of 54 packages PASS.** The 54th, `internal/payments` (~20-30 min under `-race`), was killed by a container restart and relaunched; at the time of writing its status file read `RUNNING` (started 13:01 container clock). **Re-read the result before any final claim.**
- Earlier comparable sweeps: `f9dc8a3` 54/54 (round 2, `docs/plans/payment-readiness/evidence/prh2-r2-race-sweep-f9dc8a3.txt`); `741569e` 53/54 with one real defect fixed in `d0238ee`; combined-tree class-B verification at `25cdcec` (payments 1098 s, reconciliation 85 s, adjustment 44 s).
- Timing lane: environment-dependent (ADR 0094; bound 500 ms unchanged; characterized 40/40 on one host, fails on a slower host). Evidence: `docs/plans/payment-readiness/evidence/prh2-timing-lane-characterization.md`, `prh2-final-timing-lane.md`, `prh2-r2-final-timing-lane-idle.log`.
- Lint: golangci-lint v2.9.0 must be built with Go 1.26.x; agents' own binaries were go1.25 and could not lint the 1.26 module; the orchestrator's scratchpad v2.9.0 binary is used at the final gate.

## 4. Evidence document index

Plans and reports (all under `docs/plans/prh2-hardening-round/` unless noted):
- Plan: [`plan.md`](plan.md) (rev 3). Gate reports: [`final-gate-report.md`](final-gate-report.md), [`final-gate-report-round2.md`](final-gate-report-round2.md), [`blocker-clearing-report.md`](blocker-clearing-report.md). Cleanup: [`cleanup-inventory-2026-10-05.md`](cleanup-inventory-2026-10-05.md), [`analysis/cleanup-manifest-2026-10-05.md`](analysis/cleanup-manifest-2026-10-05.md).
- Analyses: [`analysis/payment-followups-classification-2026-10-05.md`](analysis/payment-followups-classification-2026-10-05.md), [`analysis/null-arm-write-1-security-analysis-2026-10-05.md`](analysis/null-arm-write-1-security-analysis-2026-10-05.md), [`analysis/decision-pack-hd-ctf-hq-e1.md`](analysis/decision-pack-hd-ctf-hq-e1.md), [`analysis/h-w1-security-review.md`](analysis/h-w1-security-review.md), [`analysis/h-w1-ledger-finance-review.md`](analysis/h-w1-ledger-finance-review.md), [`analysis/h-w1-qa-review.md`](analysis/h-w1-qa-review.md), [`analysis/psp-prerequisites-analysis.md`](analysis/psp-prerequisites-analysis.md), [`analysis/alert-delivery-design-v1.md`](analysis/alert-delivery-design-v1.md), [`analysis/classb-b1-b2-mutation-evidence-2026-10-05.md`](analysis/classb-b1-b2-mutation-evidence-2026-10-05.md); design: [`designs/pay-closed-tenant-funds-resolution-1.md`](designs/pay-closed-tenant-funds-resolution-1.md).
- Reviews: [`reviews/`](reviews/) holds the specialist reviews per workstream (a-, b-, c-, d1-, d2-, e1-, e2-, e3-, f-kyc-, fpay-, g1-, h-, i-core-, iwire-, j-, k1-, k2-, k3-, adr-0099-0101, adr-0102-0104). Examples: [`reviews/k3-impl-security.md`](reviews/k3-impl-security.md), [`reviews/k2-security-rereview.md`](reviews/k2-security-rereview.md), [`reviews/code-reviewer-verification.md`](reviews/code-reviewer-verification.md). Reviews for round 4/5 (B3, B4, B9/B10, R3 gate, signed actor proof, stake return) are recorded in the task-registry rows (`CLASSB-B3-MERGED-2026-10-06`, `CLASSB-B9-B10-MERGED-2026-10-06`, `MA020-B4-REVIEW-2026-10-06`, `R3-GAME-POSTINGS-NONACTIVE-1-MERGED-2026-10-06`, `B4-MA020-MERGED-2026-10-06`) rather than as separate files on HEAD.
- Mutation-kill evidence on HEAD (`docs/plans/payment-readiness/evidence/`): `prh2-c-`, `prh2-casino-a-`, `prh2-casino-b-`, `prh2-d1-`, `prh2-d2-`, `prh2-e1-`, `prh2-fpay-`, `prh2-g1-`, `prh2-h-`, `prh2-i-core-`, `prh2-iwire-`, `prh2-k1-`, `prh2-k2-`, `prh2-k3-`, `prh2-r2-alert-routing-`, `prh2-r2-psp-prereq-`, `prh2-r2-temp-revoke-`, `prh2-r3-closed-tenant-recon-`, `prh2-r4-game-postings-nonactive-mutation-kill.txt`, plus earlier `prh-i1..i5`, `prh-ref`. B3/B4/B9/B10 mutation counts (e.g. B3 11 killed + 1 equivalent; B4 10/11 killed + 1 equivalent; R3 gate 26+ killed) are in the registry rows. **On the unmerged branches only:** `docs/plans/prh2-hardening-round/prh2-r5-signed-actor-proof-mutation-kill.txt` and `docs/plans/payment-readiness/evidence/prh2-r5-stake-return-closure-mutation-kill.txt`.
- Governance: [`../../governance/task-registry.md`](../../governance/task-registry.md) (append-only; latest rows `DECISIONS-PRH2-FINAL-GAMEPLAY-SECURITY-2026-10-06`, `HANDOVER-LIVE-2026-10-06`), [`../../governance/human-decision-register.md`](../../governance/human-decision-register.md), [`../../governance/payment-readiness-completion-report.md`](../../governance/payment-readiness-completion-report.md), [`../../governance/incident-2026-09-27-local-db-credential-mutation.md`](../../governance/incident-2026-09-27-local-db-credential-mutation.md).
- ADRs added/amended this round: [0108](../../decisions/0108-revoke-temp-from-runtime-role.md), [0109](../../decisions/0109-prh2-round4-classb-amendments.md); ADR 0095 sections 40.4-40.6 (40.6 on the closure branch); ADR 0110 (signed actor proof, on branch).

## 5. Decisions made this round (with IDs)

| ID | Decision | Date |
|---|---|---|
| THREAT-MODEL-ARBITRARY-SQL-1 | YES: a stolen/compromised `igaming_runtime` credential or arbitrary SQL as that role is inside the production threat model for DB-enforced controls | 2026-10-05 |
| SIGNED-ACTOR-PROOF | AUTHORIZED, smallest mechanism only | 2026-10-06 |
| H-W1 | Closed/suspended tenants observed read-only by reconciliation; no auto dispatch/release/cancel/settle | 2026-10-05 |
| HD-CTF-10 | Closed tenants stay observable while unresolved player funds exist; read-only/reconciliation-scoped credential for a real PSP where supported | 2026-10-05 |
| R3-GAME-POSTINGS-NONACTIVE-1 | Fail closed for NEW gameplay postings on non-active tenants (migration 0118, merged) | 2026-10-05 |
| Q-GP-5 | Terminal stake returns ALLOWED for valid existing rounds on non-active tenants (branch, 0121) | 2026-10-06 |
| Q-GP-1 | Tenant closure REFUSED while open rounds exist (branch, 0121; casino half not representable) | 2026-10-06 |
| Sandbox before ALERT-DELIVERY-1 | YES, sandbox only; ALERT-DELIVERY-1 remains a production blocker; Class-B prerequisites first; adapter NOT authorized yet | 2026-10-05/06 |
| ADR 0094 | 500 ms bound kept unchanged | 2026-10-05 |
| HQ-E1-2 p2 | Confirmed | 2026-10-05 |
| Cleanup | Accepted; 44 UNKNOWN DBs preserved | 2026-10-05 |
| TRIGGER-SEARCH-PATH-1 route | REVOKE TEMP (0116, ADR 0108) | earlier, merged 2026-10-05 |

## 6. Residual register

### Threat model THREAT-MODEL-ARBITRARY-SQL-1: PARTIALLY MITIGATED (never "closed")

| # | Residual | State | Owner action |
|---|---|---|---|
| T1 | Impersonate real admins by GUC (K2/K3/K1/policy approvals) | CLOSED by signed proof (branch, merge pending) | merge |
| T2 | Mint/impersonate staff by SQL as actors | CLOSED for K1/K2/K3/policy (branch) | merge |
| T3 / H1 | Mint/take over staff row or forge refresh `sessions` row, then log in to receive genuine proofs | OPEN, deferred | authorize identity-store hardening |
| T4 | `player_credential_tokens` unkeyed hash | OPEN, deferred | authorize keyed hash |
| T5 | Non-governed posting paths (deposits, withdrawals, casino/sportsbook postings, callbacks) not proof-protected | OPEN; limited by ledger invariants and hourly drift | decide whether to extend |
| T6 | App-host compromise holds signing key | OPEN, inherent | accept |
| T7 | Owner/migration role can read symmetric key | OPEN, accepted | accept |
| **T8** | Other dual-control flows outside the nine tables get the actor from GUC/app values with no proof: kill-switch release (0105), provider credential handles (0096), KYC enforcement policy (0103), asset-registry (0047), withdrawal approvals/policies (0026/0034), bonus change governance (0063), casino catalogue (0086/0089) | NOT mitigated, deferred | authorize extension |
| s9.5 | Wire-captured proof usable once within 30 s for exactly its write | accepted | TLS in production |

### Gameplay questions

| ID | Status |
|---|---|
| Q-GP-1 | DECIDED (closure refused with open rounds); implemented on branch, merge pending |
| Q-GP-2 | OPEN: `tenant_not_active` class in `casino_callback_rejections` |
| Q-GP-3 | OPEN: public casino webhook 401 before verification leaves no durable provider-claim evidence |
| Q-GP-4 / R3-RECON-NONACTIVE-STREAMS-1 | OPEN: `casino_consistency` / `sportsbook_settlement` not run for non-active tenants |
| Q-GP-5 | DECIDED (stake returns allowed); branch, merge pending |
| Q-GP-6 | OPEN: what signal closes a casino round (no loss/round-close callback) |

### Class-B payment items (B1..B18)

Status per orchestrator facts; the repository classification file lists the underlying IDs. The exact titles of B14-B18 are not spelled out in files I could read; the facts file calls them "adapter acceptance criteria / sandbox readiness gate items".

| B | Item | Status |
|---|---|---|
| B1 | Panic value redaction (PAY-H-FOLLOWUPS-1 (9)/SEC-8), `7ab0db5` | CLOSED/MERGED (PANIC-LOG-VALUE-1 follow-up open) |
| B2 | T4 receipt drain killing tests (PAY-RECEIPT-T4-DRAIN-TEST-1) | CLOSED/MERGED (test-only) |
| B3 | PAY-CALLBACK-MISMATCH-BIND-1 (`f30f05c`, `d5284cf`) | IMPLEMENTED/MERGED; PAY-PARK-BIND-RACE-1 follow-up |
| B4 | MA020-SYNC-MISMATCH-1, migration 0119 | IMPLEMENTED/MERGED; MA020-K2-VISIBILITY-1, STMT-TABLE-INSERT-RLS-1, F-VIS stranding open |
| B5 | Poll echo hardening (PAY-POLL-ECHO-HARDENING-1) | OPEN, startable |
| B6 | Deposit escalation (PAY-DEPOSIT-ESCALATION-1) | OPEN, startable |
| B7 | No-reference deposit never polled (H(11)) | OPEN, startable |
| B8 | Tenant-status read inside claim tx (H(1)/SEC-1) | OPEN, startable |
| B9 | PAY-FPAY-HARDENING-1 F-L2 (`e6773c0`) | IMPLEMENTED/MERGED |
| B10 | PAY-PAYOUT-REFBIND-1 (`d51de3f`; ADR 0109) | IMPLEMENTED/MERGED; PAY-PAYOUT-CONTRADICTION-HOLD-1 tracked |
| B11 | Payout unbound-hold tests + ADR (PAY-PAYOUT-UNBOUND-HOLD-1) | OPEN, startable |
| B12 | Payout dispute alert (PAY-PAYOUT-DISPUTE-ALERT-1) | OPEN, startable |
| B13 | Payout destination binding (PAY-SEC-LAUNCH-1) | **BLOCKED** (architect + owner) |
| B14-B18 | Adapter acceptance criteria / sandbox readiness gate | DEFERRED to the sandbox gate |

### Other open register items (selected)
TRIGGER-SEARCH-PATH-1 (HIGH, launch blocker until deployed and verified), PLAT-ROLESPLIT-1 (staging verification prepared, not executed), NULL-ARM-WRITE-1, STAFF-LIFECYCLE-1, TENANT-STATUS-AUTHZ-1, ALERT-DELIVERY-1, R3-OBS-BOUND-1, R3-FIRST-REAL-SOURCE-READONLY-1, R3-LISTING-FAILURE-LOG-ONLY-1, PANIC-LOG-VALUE-1, REDACTEDREASON-PASSTHROUGH-1, HD-CTF-1..9, HQ-E1-1/3/4, no `idle_in_transaction_session_timeout` on the runtime role, DEPLOY-FPKEY-1, HD-10.3-2, ACCESS-ANALYZER-CHECK-1, CI-BILLING-1, BRANCH-PROTECTION-1.

### Launch blockers (current)
Real-money launch is BLOCKED: no licence/legal approval beyond the Anjouan scope question; all providers MOCK; ALERT-DELIVERY-1 (no human paged); TRIGGER-SEARCH-PATH-1 not yet deployed and verified, AWS OFF; backup/DR NOT MET; no green CI (billing) and timing lane environment-dependent; THREAT-MODEL residuals T3/H1, T4, T5, T8; Class-B and K3/R3 conditions; CAS-GAME-KILL-BET-1; KYC-ENFORCE-1 and WD-RG-1; hosting AUP; production launch authorization. Full list: `HANDOVER.md` section 33.

## 7. What changed in this round (2026-10-05 to 2026-10-06)

- Round 2 (2026-10-05): R2-A migration 0116 REVOKE TEMP (ADR 0108); R2-E migration 0117 alert routing readiness (ADR 0102 s18); R2-F statement-source wiring, parked-capture standing, poll-ref clearing.
- Blocker-clearing (2026-10-05): H-W1 closed-tenant observation merged (`741569e`); ADR 0094 environment qualification; cleanup (748 + 73 scratch databases dropped, one superseded worktree archived by tag `archive/qa-fh3-adjudication-20260927`); NULL-ARM-WRITE-1 review; PLAT-ROLESPLIT-1 staging procedure prepared; Mac runner prepared; payment follow-up classification.
- Round 4 (2026-10-05/06): R3 gameplay gate migration 0118; B1, B2, B3, B4 (0119), B9, B10; ADR 0109; class-B combined-tree verification.
- Round 5 (2026-10-06): owner decisions SIGNED-ACTOR-PROOF, Q-GP-5, Q-GP-1; two branches implemented and reviewed (above), merge pending; full per-package `-race` sweep at `d149a64` (53/54 PASS, payments re-running).
- This handover (`HANDOVER-LIVE-2026-10-06`): `docs/HANDOVER.md` rewritten, `docs/runbooks/developer-handover.md`, this file, and `docs/governance/human-decision-register.md` added; no code, migration, workflow or Terraform change.

## 8. Items not verifiable from the repository when this snapshot was written

- The final `internal/payments` race result at `d149a64`.
- The exact definitions of B14-B18 (only B1-B13 are traceable to registry/classification text).
- That the delta security/LF review records for 0120 and 0121 exist as files (they are reported by the orchestrator; ADR 0110's own header still says a further review is required).
- Existence and contents of the 44 UNKNOWN databases (no database was touched).
- Current AWS account state (taken from the 2026-09-26 teardown record; nothing was queried).
