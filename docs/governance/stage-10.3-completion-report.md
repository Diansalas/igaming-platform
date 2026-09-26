# Stage 10.3 completion report — Real Provider Trust & Casino Financial Readiness

**DRAFT — gate 10.3-W2/W3 verdict [PENDING CI-342].** This report must not be treated as final until
CI-342-STOREOUTAGE is root-caused and fixed and GitHub CI is green on the fixed head (§6).

Stage definition: ADR 0092 (ACCEPTED 2026-09-26). Credential model: ADR 0093. Branch
`claude/focused-wright-jw88w9`. Planning papers, reviews, gate log and evidence:
`docs/plans/stage-10.3-planning/` (gate log `05-gate-log.md`; mutation and red-before-green evidence
in `evidence/`). Registry: `docs/governance/task-registry.md`, section "Stage 10.3".

**Status: W0–W3 delivered for the MOCK providers and local backends only; stopped at the Stage 10.3
completion gate, with the final CI item pending.** No AWS action was taken. Staging is OFF. No real
provider is supported, and no production, provider, licensing or regulatory readiness is claimed.

## Gates

| Gate | Result | Evidence |
|---|---|---|
| 10.3-W0 ADR / stage-definition consistency | PASSED 2026-09-26 | `05-gate-log.md` "GATE 10.3-W0"; ADR 0092, ADR 0093, amendments to ADRs 0022 §3, 0025, 0028, 0082 (A6), 0085 §1 |
| 10.3-W1 (W1a–W1d) | PASSED 2026-09-26 | `05-gate-log.md` "GATE 10.3-W1"; `06-gate-w1-*.md` |
| 10.3-W2/W3 (W2a, W2b, W3a, W3b, CI-FLAKE-281) | **[PENDING CI-342]** — all reviews at APPROVE WITH CONDITIONS / READY WITH FOLLOW-UPS; one unexplained CI failure open | `05-gate-log.md` "GATE 10.3-W2/W3"; `09`–`13` papers; §5, §6 |

## 1. Scope authorized (human rulings, ADR 0092 §"Human decisions")

| Ruling | Decision | Effect in this stage |
|---|---|---|
| HD-10.3-1 | APPROVED: full scope | W0 → W1a–W1d → W2a/b → W3a/b delivered; W3a was not cut |
| HD-10.3-2 | **AWS IAM code EXCLUDED** | No IAM, KMS or egress code in `deploy/`. `awssm` is backend code wired behind the existing boundaries (config allow-list, `ValidateSecretBackendScheme`, synthetic guard, import guards), tested only against a local SDK fake. ADR 0084's empty/minimal task role and ADR 0086 §17 unchanged |
| HD-10.3-3 | **Players see STATUS ONLY** | No player-facing `reason_code` and no provider text on any player surface. The bounded, sanitised provider reason is visible only to authorized staff and compliance (migration 0095 adds the bound only) |
| HD-10.3-4 | **Suspended-tenant casino settlement UNCHANGED** | Shared webhook preamble still returns a uniform 401 (`tenant_inactive`) before verification; no posting, tombstone or audit; exposure may strand. Documented in the ADR 0025 Stage 10.3 amendment (SUSP-TENANT-SETTLE-1); reconciliation is the detector. A change needs a new human decision |

Further binding instructions honoured: PAYWH-TS-1, PAYWH-BRAND-1, PAYWH-RL-1 stayed deferred; the
Stage 10.2 MOCK scheme is not declared any real provider's protocol; AWS staging stayed OFF; no Bonus
Engine Wave 4 and no AI implementation (ADR 0089 remains architecture only).

## 2. Deliverables and labels

One label per item, taken from the registry "Stage 10.3" section and ADRs 0092/0093 as they stand at
`00f02ef`. Where an ADR 0092 target label was not reached, that is stated.

| Wave | Item | Label | Notes |
|---|---|---|---|
| W0 | STAGE-10.3-W0 (ADRs 0092/0093 + five amendments) | `IMPLEMENTED` (docs) | Gate 10.3-W0 PASSED |
| W1a | WH-VENDOR-SCHEME-1 | `IMPLEMENTED` | Platform contract, SC1–SC13 suite, MOCK schemes, real-scheme contract. Real vendor schemes `PROVIDER DEPENDENT` (each scheme type must implement `MarkProductionEligible()`). Domain callback-fixture hook `NOT IMPLEMENTED`. `KeyImplicit` resolution now `IMPLEMENTED` (W2a) |
| W1b | MOCK-ADAPTER-PROD-1 | `IMPLEMENTED` | By design, today's all-mock production binary refuses to start |
| W1c | CAS-CAP-ROLLBACK-1 | `IMPLEMENTED` — MOCK provider only | Capability gates new bets only; tombstone always; ADR 0082 A6 `IMPLEMENTED` |
| W1c | CAS-MULTIBET-WIN-1 (G-1) | `IMPLEMENTED` — MOCK provider only | |
| W1d | KYC-REASON-BOUND-1 | `IMPLEMENTED` | Bounded (512 B, sanitised), staff-only; players see status only |
| W2a | PROV-CRED-RESOLVER-1 | `IMPLEMENTED` | Handle table (migration 0096, FORCE RLS), resolver, four-eyes activation, admin API; backends `memory` (tests only) and `devfile` (development only). Matched-`key_id` log (W2A-SEC-2) `IMPLEMENTED`. `awssm`: see W3b |
| W2a | PROV-OUTBOUND-CRED-1 | `PARTIALLY IMPLEMENTED` | Per-call `OutboundResolver`, per-call `Authenticator`, `DerivedTokenCache`, static `APIKeyEnvVar` removed. **Not implemented:** tenant + resolved credential through adapter request types; outbound calls moved out of the domain DB transaction. Launch-blocking precondition registered and enforced by a tripwire test. ADR 0092's target (`IMPLEMENTED`) **not met** |
| W2a | KYC-PROVIDER-SELECT-1 (O4) | `IMPLEMENTED` | Fails closed when none or several handles; lone synthetic adapter only in test-support deployments |
| W2b | CAS-RECON-1 | `IMPLEMENTED` | Migration 0097, `casino_consistency` C1–C7 with the C6 class ruling (paper 02 §2.19), verified-only rejection record (11 classes), read-only admin views. C3 order sub-check `NOT IMPLEMENTED` (recorded, sound per review). BONUS_SET bets outside the C2 positive rule (recorded deviation; G-6 unshipped). Compensation mechanism (LEDGER-MANUAL-ADJ-4EYES-1) `NOT IMPLEMENTED`. Reconciliation writes no money |
| W3a | CAS-RECON-STMT-1 | `MOCK` | Migration 0098, provider-neutral `CasinoStatementSource`, key + totals match, REPEATABLE READ snapshot. Against the MOCK source the match is **tautological** (plumbing only); detection proven with test-only divergent sources. Real statement ingestion `PROVIDER DEPENDENT` |
| W3b | SECRETSTORE-AWS-1 | `PARTIALLY IMPLEMENTED` | Backend code, wiring into `cmd/platform-api` and SDK-fake tests `IMPLEMENTED`; security S-1/S-2/S-5 and N-1 fixed. IAM/KMS/`deploy/` `NOT IMPLEMENTED` (HD-10.3-2). Real-AWS use, latency, cold cache, rotation/outage drills `STAGING REQUIRED` |
| W3 | CI-FLAKE-281 | `IMPLEMENTED` | Stage 9 hang guard scaled by calibrated Argon2 cost (floor 30 s never lowered, cap 3 min); no recurrence in CI #331–#342 (`13-ci-flake-281-disposition.md`) |
| — | GO-TOOLCHAIN-VULN-1 (found by the new govulncheck step) | `IMPLEMENTED` | go1.26.8; `golang.org/x/text` v0.42.0; otel v1.46.0; govulncheck@v1.8.0; golangci-lint v2.9.0; CI #341 govulncheck green |
| — | Real PSP / KYC / casino adapters | `NOT IMPLEMENTED` (`PROVIDER DEPENDENT`) | |

Registered during the stage (not built): CAS-RECON-SCALE-1 (Medium), PROVIDER-REF-BOUND-1 (Low),
CODE-HYGIENE-10.3-1 (Low), CAS-WIN-IDEMP-1 (Medium), PAY-SB-REPLAY-AUDIT-1 (Low),
CR-CHECKLIST-HMAC-1 (human), KYC-DOC-REJECTION-BOUND-1, DEPLOY-FPKEY-1, VERIFY-TEARDOWN-ECS-1,
PROV-REVOKE-ALL-1, CAS-WIN-ANOMALY-1, KYC-HOSTED-SESSION-1, KYC-SANCTIONS-IF-1.

## 3. Reviews and verdicts

| Review | Verdict | Record |
|---|---|---|
| Planning: product-owner-proxy, qa, security | APPROVE WITH CONDITIONS; rulings R1–R15 | `04-review-*.md` |
| Gate W1: identity-compliance | APPROVE WITH CONDITIONS (1–2 met) | `06-gate-w1-review-identity-compliance.md` |
| Gate W1: ledger-finance (+ casino) | APPROVE WITH CONDITIONS C1–C10; re-verified; C11 met | `06-gate-w1-review-ledger-finance.md` |
| Gate W1: security | APPROVE WITH CONDITIONS S-1..S-6 (fixed; S-3 checklist item needs the human) | `06-gate-w1-review-security.md` |
| Gate W1: code-reviewer | NOT READY (14) → re-verified | `06-gate-w1-review-code.md`, `06-gate-w1-reverify-code.md` |
| W2a design: security | design approved with amendment (ADR 0093) | `07-w2a-design-review-security.md` |
| W2a code: security | APPROVE WITH CONDITIONS W2A-SEC-1 (Medium), W2A-SEC-2 (Low) — both CLOSED | `09-gate-w2-review-security-w2a.md` |
| W2b/W3a/W3b: security | APPROVE WITH CONDITIONS (S-1/S-2 Medium blocking `awssm` wiring; S-3 Medium; S-4/S-5, R-1/R-2 Low; R-3 Info) | `10-gate-w2w3-review-security.md` |
| W2/W3: code-reviewer | NOT READY — 10 findings, no money-path bug | `10-gate-w2w3-review-code.md` |
| W2/W3 re-verification: security | **APPROVE WITH CONDITIONS** — all prior conditions CLOSED; N-1 Low (fixed later in `e80114b`); N-2, N-3 Info, open under HD-10.3-2 | `11-gate-w2w3-reverify-security.md` |
| W2/W3 re-verification: code-reviewer | **READY WITH FOLLOW-UPS** — #1–#9 CLOSED; #10 → CODE-HYGIENE-10.3-1 (`Router.Backends`, `ListRunsForStream` removed in `302433d`) | `12-gate-w2w3-reverify-code.md` |
| CI-FLAKE-281: qa | disposition `IMPLEMENTED` | `13-ci-flake-281-disposition.md` |
| C6 class ruling: ledger-finance | 6 finding classes, 5 evidence-only | paper `02-casino-financial-analysis.md` §2.19; fix `dd30350` |

Review gaps stated plainly:
- The N-1 fix (`e80114b`) postdates the security re-verification; it has no separate `security`
  sign-off (the code re-verification only confirmed its tests exist and pass).
- The `security` reviews did not run integration tests or govulncheck themselves (egress blocked);
  CI #341 supplies the govulncheck evidence.
- The SECRETSTORE-AWS-1 registry row still says `awssm` "must be re-reviewed by `security` now that
  it is wired"; `11-gate-w2w3-reverify-security.md` did review and approve the wiring. The row is
  stale (it is not edited by this report).

## 4. Mutation evidence

All under `docs/plans/stage-10.3-planning/evidence/`. "Killed" means the named test failed on its own
assertion, not on a build error.

| Record | Result | Disclosed corrections |
|---|---|---|
| `w1a-mutation-kill.txt`, `w1-fixround-b-mutation-kill.txt`, `w1c-mutation-kill.txt` | W1 (22/22, 35/35 with one equivalent mutant M7b disclosed, 8/8 + C11) | see gate W1 |
| `w2a-mutation-kill.txt` | 24/24 | M22 first NOT KILLED (test compared against the mutated constant); fixed |
| `w2b-mutation-kill.txt` | 25/25 | M-C4a first SURVIVED; test strengthened, M-C4d added |
| `w3a-mutation-kill.txt` | 34/34 | none in the final run |
| `w2w3-closeout-mutation-kill.txt` | 38/38 + 7/7 (N-1) | M34 first did not compile; M36 first SURVIVED; two defence-in-depth lines not mutated, with reasons |
| `w2w3-fixround-recon-mutation-kill.txt` | 15/15 | — |
| `294e0a0` (commit message only, no evidence file) | 3 mutants, all failing a test | — |
| `13-ci-flake-281-disposition.md` §4 | injected 5-minute hang fails at ~2m17s | — |

## 5. Security and financial invariants (summary)

- Tenant isolation: new tables (0096, 0097, 0098) under FORCE RLS; tenant-binding composite FKs on
  credential handles; no `SECURITY DEFINER`; rejection record written only after verification, with
  route-derived tenant and provider.
- Four-eyes credential activation is live with the admin API (no single-actor form); 24 h approval
  expiry; single use.
- No secret in logs, errors, OpenAPI, audit or responses (reviewed in `09` §4; tests scan raw, hex,
  base64 forms).
- Reconciliation (C1–C7, `casino_statement`) inserts only run, mismatch and audit rows; never
  auto-corrects; any drift is P1. No ledger write, balance update or compensation was added.
- `awssm`: credentials only from the ECS task-role endpoint; trust-root, endpoint, credential-source
  and ambient-tuning overrides refused; refs must be full ARNs; no network at `New`.
- Passing these reviews does not make any component "secure" in a certification sense; they are
  code-level development-stage reviews.

## 6. CI

GitHub Actions, branch `claude/focused-wright-jw88w9` (verified against the Actions run list):

| Run | Head | Result |
|---|---|---|
| #337–#339 | W3b / review docs | failure — first `govulncheck` findings (GO-TOOLCHAIN-VULN-1; per registry) |
| #340 | `e5b6e17` | **failure** at `golangci-lint` (v2.6.2 binary built with go1.25); fixed by `0fbd0dc` |
| #341 | `0fbd0dc` | **green**, all steps incl. dependency drift, govulncheck, the AWS-SDK and memory-store import guards, golangci-lint, race unit, race integration, migration reversibility |
| #342 | `1138062` | **FAILED** — `TestStoreOutage_DoesNotPinPool` (`internal/providercred`). CI-342-STOREOUTAGE, under investigation. **[PENDING: CI-342 root cause + fix]** |
| #343 | `e80114b` | green |
| #344 | `302433d` | green |
| #345 | `00f02ef` | in progress when drafted — result to be recorded |

`TestStoreOutage_DoesNotPinPool` is the test that security W2a observation O-7 relies on (the inbound
secret fetch bounded by the 4-slot semaphore while a callback transaction is open). Later green runs
do not dispose of #342; it stays an open, gate-blocking item until explained.

No 3× local CI replay is recorded for the W2/W3 range. Toolchain: go1.26.8 (`go.mod` toolchain line,
CI, Dockerfile builder), `golang.org/x/text` v0.42.0, otel v1.46.0, govulncheck@v1.8.0,
golangci-lint v2.9.0.

## 7. Roadmap reconciliation (vs `00-roadmap-reconciliation.md`)

- **§3 NEXT → done.** The recommended "pre-real-provider hardening" scope (WH-VENDOR-SCHEME-1,
  CAS-CAP-ROLLBACK-1 with casino reconciliation, MOCK-ADAPTER-PROD-1, secret-store design) was
  defined (ADR 0092), authorized and delivered, for MOCK providers and local backends.
- **§6 DEFERRED → moved.** KYC-REASON-BOUND-1, WH-VENDOR-SCHEME-1, MOCK-ADAPTER-PROD-1,
  CAS-CAP-ROLLBACK-1 and CI-FLAKE-281 left the deferred list (§2 labels). PAYWH-BRAND-1,
  PAYWH-RL-1, PAYWH-TS-1 remain deferred.
- **§7 production/provider blockers.** "Real `WebhookCredentialResolver` + secret store" moves from
  `NOT IMPLEMENTED` to: resolver `IMPLEMENTED` on local backends, `awssm` `PARTIALLY IMPLEMENTED`
  (IAM excluded, drills STAGING REQUIRED). Every real adapter remains `PROVIDER DEPENDENT`. All
  other §7 blockers unchanged.
- **§2 IN PROGRESS.** The staging teardown is now recorded
  (`docs/governance/staging-teardown-2026-09-26.md`); staging is OFF.
- **§5 WAITING FOR HUMAN DECISION.** Unchanged; nothing decided by this stage beyond HD-10.3-1..4.
- **§8 STAGING REQUIRED.** Unchanged, plus the Stage 10.3 items in §9 below.
- **Discrepancies §1–§13.** Items 9–11 were registered in the "Records hygiene 2026-09-26" section
  (ACC-EVIDENCE-1, STAGING-9.4-VERIFY-1, STAGE-NAMING-1); item 13 is resolved by ADR 0092. Other
  items were not re-audited for this report.

## 8. Remaining blockers

**Gate-blocking (this stage):**
- CI-342-STOREOUTAGE. **[PENDING: CI-342 root cause + fix]**

**Launch-blocking preconditions (no real provider may go live before these):**
- **PROV-OUTBOUND-CRED-1** — no non-synthetic payments, casino or KYC adapter may be registered or
  make an outbound call until outbound calls run outside any domain DB transaction (owners
  architect + ledger-finance + domain specialists; tripwire
  `TestOutboundPrecondition_EveryWiredAdapterIsSynthetic`).
- **PROVIDER-REF-BOUND-1** — platform-wide bound on provider reference length, before real-provider
  go-live.
- **`DerivedTokenCache` bound** (CODE-HYGIENE-10.3-1 item 2) — bound or sweep before its first
  production caller; retention question to `security`.
- **DEPLOY-FPKEY-1** — delivering the fingerprint HMAC key to AWS needs a `deploy/` change and a
  human decision before the staging deployment.
- Also before real money / real providers: CAS-RECON-SCALE-1 (before multi-tenant real-money load),
  CAS-WIN-IDEMP-1 (before G-6), CAS-WIN-ANOMALY-1, LEDGER-MANUAL-ADJ-4EYES-1, the free-round/jackpot
  conformance case and the domain callback-fixture hook (first real casino adapter), and every
  earlier-stage blocker in `00-roadmap-reconciliation.md` §7.

## 9. STAGING REQUIRED

From `00-roadmap-reconciliation.md` §8 (unchanged): Stage 10, 10.1 and 10.2 acceptance runs; edge
allowlist; multi-replica test; `b22d5c4` live IAM validation; Secrets Manager/RDS rotation,
secrets-in-state and log checks; RDS behaviour and fresh migration chain; post-apply `terraform plan`;
alarms; teardown behaviour; deployer network-policy tag scoping; stale ALB SG description; F-6 /
ACC-EVIDENCE-1 acceptance evidence; STAGING-9.4-VERIFY-1.

Added by Stage 10.3 (ADR 0093 §8, registry):
- migrations 0094–0098 applied by `role-init` + `migrate` on AWS, and the Stage 10.3 flows exercised
  there;
- task-role `secretsmanager:GetSecretValue` on `<prefix>/provider-creds/*` (+ `kms:Decrypt` with
  `kms:ViaService` if a customer-managed key) — needs HD-10.3-2 first;
- network path to Secrets Manager (VPC endpoint vs proxy, N-2);
- fingerprint key delivery (DEPLOY-FPKEY-1);
- real `GetSecretValue` latency and throttling, cold cache, rotation and store-outage drills with a
  **synthetic** secret;
- alarms on `credential_store_unavailable` and `credential_integrity`;
- confirming no secret in container logs, task metadata or Terraform state.

## 10. Still deferred / not authorized

- PAYWH-BRAND-1, PAYWH-RL-1, PAYWH-TS-1 (deferred; PAYWH-TS-1 closure was to be decided "at the 10.3
  completion gate on evidence" — no evidence supporting closure is recorded, so it stays open).
- Bonus Engine Wave 4 — not authorized.
- AI / agent implementation — not authorized; ADR 0089 remains architecture only.
- Out of scope per ADR 0092: casino G-3/G-4/G-7, free rounds/jackpots/bonus-funded casino stakes,
  KYC-HOSTED-SESSION-1, KYC-SANCTIONS-IF-1, partner-console secret writing, per-tenant IAM/KMS,
  PROV-REVOKE-ALL-1, B2B, retail, cashout.

## 11. Human decisions still open

- ADR 0009 residual: gambling AUP, contractual permission, data residency, final production cloud
  provider.
- HDR-J-6, HDR-J-7, HDR-J-8, HDR-J-9.
- HDR-M-1, HDR-M-2.
- HDR-SB-1 (trading-book liability; exposure ceiling).
- OB-1 (receivable / negative `player_cash` after a won settlement is rolled back).
- Licensing, legal, vendor (KYC/AML, PSP, casino aggregator, sportsbook feed, crypto custodian),
  production credentials, commercial pricing, and retail items 1–14.
- LEDGER-MANUAL-ADJ-4EYES-1 (own later stage; also the recovery path after a casino key compromise).
- Sportsbook jurisdiction Rung 2 (SB-JUR-RUNG2-1, blocked on HDR-J-7).
- **HD-10.3-2 follow-up:** IAM/KMS architecture for `awssm`; `HTTPS_PROXY` for the Secrets Manager
  client (N-2; recommendation on record: VPC endpoint, proxy not honoured); IRSA/web identity as a
  credential source (refused until decided); account pinning (N-3; scope the task-role policy to
  the platform account and `provider-creds/` prefix).
- **CR-CHECKLIST-HMAC-1:** a human edit of `.claude/agents/code-reviewer.md` to add the
  "`hmac.Equal` only" item (the lint is the enforcing control meanwhile).
- **Branch protection + CODEOWNERS on `.github/`:** confirm that CI is a required status check and
  `.github/` changes need owner review (cannot be verified from the repository).
- **Access Analyzer check from the teardown:** the deployer credential cannot run
  `access-analyzer:ListAnalyzers`/`ListFindings`; the account owner must run the runbook §6
  session-end check (`staging-teardown-2026-09-26.md`).
- STAGE-NAMING-1 (revise the master stage map by recorded decision); O7 (raw provider payload
  retention period); the other open items in `00-roadmap-reconciliation.md` §5.
- Staging deployment authorization and production launch authorization.

## 12. Explicit statements

- **No AWS deployment** and no AWS action of any kind was performed in Stage 10.3. No `deploy/` IAM,
  KMS or Terraform change.
- **Staging is OFF** (torn down 2026-09-26). All work used local synthetic PostgreSQL, MOCK
  providers, the `memory`/`devfile` backends and an SDK fake.
- **No production readiness and no provider readiness is claimed.** An `APP_ENV=production` binary
  refuses to start today, by design, because every bundled adapter is synthetic.
- **No real provider is declared supported.** The MOCK schemes are not any vendor's protocol.
- Software capability is not legal, regulatory or licensing approval; none is claimed.
- No secret, credential or key material is included in this report.

## 13. Final commit

Last code/test commit at drafting time: `00f02ef`. The final Stage 10.3 commit will be the one
carrying the CI-342 fix and this report's final version; its SHA and CI result are to be recorded
here. **[PENDING: CI-342 root cause + fix]**

## STOP — awaiting human authorization for the next stage

Nothing beyond Stage 10.3 has been started. Decisions requested from the human:

1. Accept Stage 10.3 as complete once CI-342-STOREOUTAGE is closed (or direct otherwise).
2. Choose the next stage. Candidates on record, none started: the PROV-OUTBOUND-CRED-1 restructuring
   (outbound calls out of DB transactions) with PROVIDER-REF-BOUND-1 and the `DerivedTokenCache`
   bound; CAS-RECON-SCALE-1; LEDGER-MANUAL-ADJ-4EYES-1; or the governed staging deployment.
3. Authorize (or not) the single governed staging deployment from the final approved commit, and
   decide DEPLOY-FPKEY-1 for it.
4. Decide HD-10.3-2 follow-ups: IAM/KMS for `awssm`, `HTTPS_PROXY` / VPC endpoint, IRSA, account
   pinning.
5. Make the CR-CHECKLIST-HMAC-1 edit to `.claude/agents/code-reviewer.md` (or decline it).
6. Confirm branch protection (required CI check) and CODEOWNERS on `.github/`.
7. Run the Access Analyzer session-end check left open by the teardown.
8. Vendor selection (KYC/AML, PSP, casino aggregator, sportsbook, custodian) — the gate to any real
   adapter.
9. The carried decisions in §11 (ADR 0009/AUP, HDR-J-6/7/8/9, HDR-M-1/2, HDR-SB-1, OB-1, licensing,
   legal, retail, STAGE-NAMING-1).
