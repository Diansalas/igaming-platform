# Stage 10.2 completion report — Webhook trust hardening

Stage definition: ADR 0091 (ACCEPTED). Branch `claude/focused-wright-jw88w9`.
Design, reviews and rulings: `docs/plans/stage-10.2-planning/` (01 design with rulings §J and §K;
02 CI-FLAKE-281; 03–07 design reviews; 08 test traceability; 09 security final; 10 ledger-finance;
11 code review; `evidence/` pre-fix reproductions).

**Status: IMPLEMENTED for the MOCK providers; stopped at the Stage 10.2 deployment gate.**
No AWS action was taken. Staging still runs `9190d5d` and remains exposed to KYC-WH-1 until the human
authorizes the governed `deploy.sh down` / `deploy.sh up` refresh (§16).

## Gates

| Gate | Result | Evidence |
|---|---|---|
| G1 KYC done | PASS | §1; K1–K16 traceability (08) |
| G2 casino done | PASS | §2; C1–C14 traceability (08) |
| G3 security/RLS review | PASS (APPROVE WITH CONDITIONS; all conditions met) | 09; statement-capture tests K7/C7 incl. cross-tenant; §4 |
| G4 full regression | PASS | local CI replay (3× integration, race); GitHub CI; §6 |
| G5 CI evidence + CI-FLAKE-281 disposition | PASS (disposition: not reproduced, most likely cause identified, diagnostics added) | §3, §6, 02 |
| G6 OpenAPI and docs | PASS | §7; ADR amendments 0022/0025/0028/0019/0085 |
| G7 no secrets | PASS | §8 |
| G8 pushed | PASS | §14 |
| G9 clean tree | PASS | §14 |
| G10 deployment plan | PASS (plan written; execution needs human authorization) | §16 |

## 1. KYC-WH-1 — IMPLEMENTED (MOCK provider only)

Pre-fix evidence (`evidence/kyc-wh-1-e1..e3-*.txt`, commit `45b5908`): a player forged their own
approval (200, `approved`, audit row); the secret was a compile-time string constant; the route was
reachable with test support off.

Fix (commits `7862b5f`…`b2200ac`, gap-closure `c520e76`, final-review fixes `1ce4da0`…`b35171a`):
- **Committed secret removed.** `NewMockKYCProvider()` takes no argument; keys are derived per
  (tenant, provider) from a per-process `crypto/rand` master (`webhookauth.NewMockMaster`). A permanent
  guard test (`TestG7_NoConstStringFeedsWebhookCredential`) fails if a string constant ever feeds a mock
  credential again. The old value remains in git history before this stage and is treated as
  compromised; history was not rewritten (it authenticates nothing after the fix).
- **Environment gating.** The KYC mock provider, its resolver, the orchestrator and the webhook route
  exist only when `cfg.TestSupportRoutesEnabled()` (`APP_ENV != production` AND
  `TEST_SUPPORT_ENDPOINTS_ENABLED`), all from one `mockProviderWiring` value, so route and resolver
  cannot diverge. Otherwise the webhook is 404 and player self-service creation is 503. There is no
  simulate route; staff review (`PermVerificationReview`, held only by the compliance role) is the
  acceptance path. A player cannot self-approve: no production code path signs KYC callbacks.
- **No trust in player-supplied references.** `provider_reference` is removed from every player
  response and from the OpenAPI `PlayerVerification` schema; staff responses keep it. The lookup is
  `tenant_id AND provider_id AND provider_reference`.
- **Tenant binding, verify first.** The route tenant selects the per-(tenant, provider) credential; the
  signature covers prefix ‖ tenant ‖ provider ‖ key id ‖ raw body. Before verification only the
  platform-wide tenant lookup and `set_config` run (statement capture K7). Every pre-verification failure
  is a byte-identical 401 with an allow-listed log line.
- **Forward-only status.** Rank `unverified < pending < review_required < terminal`; compare-and-set with
  `tenant_id` and current status, ≤3 re-reads, audit row in the same transaction. Replays, lower ranks,
  anything after terminal or after a staff decision: 204, no change, no audit row, allow-listed
  `kyc_webhook_noop` log. `outcome: error`: no state change; one failure audit row per delivery while
  non-terminal, none once terminal. Success is 204 with no body.

## 2. CAS-WH-TENANT-1 — IMPLEMENTED (MOCK provider only)

Pre-fix evidence (`evidence/cas-wh-tenant-1-e4-cross-tenant-prefix.txt`): a rollback posted to tenant B's
URL, signed by the single process-wide key, wrote a tombstone in B.

Fix (commits `07d4b6c`…`3398c88`, tests `ab70570`, `9db2a52`, final-review fixes `cd652f3`, `f6db31f`):
- Casino adopts the shared contract: `HandleCallback(ctx, Inbound, Credential)`; `NewOrchestrator` takes a
  resolver; `ReceiveCallback` overwrites tenant/provider from the route, resolves the credential,
  verifies raw bytes, then parses. Zero statements run before verification (C7, including the
  cross-tenant shape); the capability check is post-verification.
- Per-(tenant, provider) derived mock keys; headers `X-Casino-Signature` / `X-Casino-Key-Id`; the old
  per-process key and NUL-joined field MAC are gone.
- The mock resolver is wired only with test support; otherwise every casino callback is 401
  `no_resolver` and no money moves. Play simulation signs in-process, returns 503 on an auth error and
  never returns signed bytes.
- Money path unchanged (ledger-finance verified): idempotency key `(tenant, provider_id,
  provider_tx_id)`, F-7 409, tombstones, ADR 0082 lock order, debits = credits. A cross-tenant callback
  writes nothing in either tenant (no tombstone, ledger, audit, casino rows or projection change).
- An unknown event type from a verified caller is now 400, not 500.

## 3. CI-FLAKE-281 — investigated; not reproduced; not blocking

Full paper: `02-ci-flake-281-investigation.md`. The failing test in run #281 cannot be identified (the
per-test annotation step postdates it; the full log is proxy-blocked here). Three local reproduction
runs, including CPU-starved and shuffled runs, all passed. **Classification: most likely test
concurrency / fixed-timeout margin** — the Stage 9 concurrent-login tests in `internal/httpserver` run
Argon2id (64 MiB) across many goroutines under a fixed 30 s ceiling, and used 22.1 s of it under local
contention. Secondary candidate: the 3 s lock-wait polling tests. Not a genuine defect on the evidence
(same commit green before and after; green under stress). Nothing was skipped, weakened or re-run to
green. **Monitoring improvement:** the per-test failure annotations (`444e6e1`) plus the full
`integration-test.log` uploaded as a 14-day artifact on failure (`b9f842c`). The item stays open until
it recurs or a later stage raises that ceiling deliberately.

## 4. Security and RLS

- No migration and no policy change in 10.2. Tenant isolation remains FORCE RLS plus the explicit
  tenant predicates, now proven independently of RLS for the KYC lookup (statement capture with bound
  args; mutation-kill recorded).
- Strict I1 for KYC and casino: before verification only `GetTenantBySlug` (platform-wide) and
  `WithTenant`'s `set_config`. Payments keeps its one read-only capability lookup.
- Domain separation: distinct prefixes (`igaming.{payments|kyc|casino}.webhook.v1`), headers and mock
  labels; a signature valid in one domain never verifies in another under an equal key (all 6 pairs).
- PAYWH-GATE-1 included: the payments mock resolver is also wired only with test support.
- Security final review: APPROVE WITH CONDITIONS; SC-1 (C7 incl. cross-tenant), SC-2 (casino no-effect
  including casino rows, projections, debits = credits) and SC-3 (casino log allow-list) met; SC-4 is
  §11 of this report. Accepted residuals: unknown-slug timing gap (carried from 10.1); duplicate
  signature headers use the first value (Info).

## 5. Tests

- Every rejection test asserts the no-effect checklist on both tenants from a fresh transaction
  (`internal/testsupport/noeffect`).
- Traceability: K1–K16 and C1–C14 → test → file in `08-webhook-test-traceability.md`, with a logged
  call-site sample (≈10% of 90 `NewOrchestrator` and 126 `CallbackPayload` sites, 100% of status-code
  changes).
- Guards shown load-bearing by mutation (each reverted): verify-before-parse (KYC, casino), tenant in the
  MAC (casino, cross-domain, cross-tenant capture), KYC tenant predicate, CAS status guard, wiring gates,
  legacy-field guards, K4 no-op log, K5 terminal check, K10 malformed mapping, log allow-lists, OpenAPI
  contract checks, capability-read ordering.
- Payments regression: all existing payments tests passed unedited through the extraction (J9).

## 6. CI

- Local CI replay at `d032a03` (fresh DB): gofmt, vet, golangci-lint (0 issues), build, migrate up +
  verify, race unit tests (29 packages), **3× race integration runs: 34 packages ok, 0 skips each**,
  reversibility on a fresh DB (down 4 / up 4 / verify clean). "ALL LOCAL CI STEPS PASSED".
- Integration suites run as the NOBYPASSRLS runtime role (ledger-finance condition 3), including
  `replay_f7`, `lockorder`, `failure_mode_matrix` and the C1/E4 cross-tenant test.
- GitHub CI green on every pushed Stage 10.2 commit through `d032a03` (runs #289–#302); results for the
  final commits are verified in §14.

## 7. OpenAPI

`docs/api/openapi/platform-api.yaml`: KYC webhook entry rewritten (headers, signing-input description,
`security: []`, test-support gated, 204/400/401/404/500); new `PlayerVerification` (no
`provider_reference`) and `KYCMockCallback`; new casino webhook entry and `CasinoMockCallback`. Contract
tests `openapi_kycwebhook_contract_test.go` and `openapi_casinowebhook_contract_test.go` match spec to
handler; payments contract test unchanged and green.

## 8. Secrets scan (G7)

The old KYC constant was extracted at runtime from `69e80c1` into a scratch file (never printed) and
searched with `git grep -F -f`: absent from HEAD; since `69e80c1` it appears in exactly one diff line, the
removal in `f982d68`; no commit adds it. It appears in no doc, test, fixture or evidence file. The Stage
10.1 design paper quoting it was redacted before this stage's work began (`69e80c1`). New mock keys are
per-process random; the one key-like test fixture was replaced with `NewMockMaster()`. No passwords,
tokens, AWS credentials or provider credentials were added.

## 9. Specialist reviews

| Review | Verdict | Record |
|---|---|---|
| QA test plan | binding plan adopted | 03 |
| backend | APPROVE (+ alias test, PAYWH-GATE-1 recommended) | 04 |
| identity-compliance | CONCUR (forward-only rank granted) | 05 |
| casino | APPROVE | 06 |
| architect + DB/RLS | APPROVED WITH CONDITIONS R1–R5 (all applied) | 07 |
| security (final) | APPROVE WITH CONDITIONS SC-1..SC-4 (met) | 09 |
| ledger-finance | CONCUR WITH CONDITIONS (met) | 10 |
| code review + re-verification | findings M1–M5, L1–L8 fixed or recorded (rulings §K); doc items D1–D4 fixed | 11 |

Disagreements: only PAYWH-GATE-1 (design left it optional; backend and architect recommended inclusion;
ruled in, J9). All rulings: design §J (J1–J17) and §K (K1–K17).

## 10. Remaining webhook work (not 10.2 scope)

- Real webhook credential resolvers (secret store, per-tenant handles) for payments, KYC and casino —
  NOT IMPLEMENTED / PROVIDER DEPENDENT.
- WH-VENDOR-SCHEME-1: header parsing must become an adapter/`Scheme` capability before any real vendor.
- PAYWH-BRAND-1, PAYWH-RL-1, PAYWH-TS-1: deferred (all reviewers agree).
- KYC-REASON-BOUND-1: bound the provider `reason` text.
- The tenant-binding conformance cases now fail (not skip) for any non-mock adapter in all three domains.

## 11. Remaining production / provider blockers (disclosures, SC-4 / J17)

- **Staging stays forgeable until the human-authorized refresh.** Staging runs `9190d5d`, which carries
  KYC-WH-1. Its KYC rows must be treated as untrusted synthetic data.
- **Both fixes are MOCK-only.** No real KYC vendor, casino aggregator or PSP exists.
- **MOCK-ADAPTER-PROD-1:** the mock payments and casino adapters remain registered in production for
  initiation, catalogue and launch (webhooks fail closed there). Pre-launch checklist item.
- **CAS-CAP-ROLLBACK-1:** a disabled casino capability 503s verified wins/rollbacks and writes no
  tombstone for unseen rollbacks; no casino reconciliation exists. Hard pre-condition for any real
  casino resolver or live aggregator.
- Production launch also remains blocked by earlier-stage items (licensing, real providers, production
  credentials) — unchanged by 10.2.

## 12. AI status

ADR 0089 preserved unchanged. No AI implementation and no AI dependency was added in Stage 10.2.

## 13. Final commit

The last code/test commit is `9db2a52`; the final commit is the one carrying this report and the records
update. Its SHA is stated in the orchestrator's final message and equals `origin/claude/focused-wright-jw88w9`.

## 14. GitHub and push verification

Verified after the final push: local HEAD equals `origin/claude/focused-wright-jw88w9`, working tree clean,
GitHub CI status of the final head reported in the orchestrator's final message.

## 15. AWS confirmation

**AWS and staging were not touched.** No `deploy.sh`, Terraform, ECS, RDS, IAM, CloudFront or network
action. Staging is still `9190d5d` at migration 0090. All work used local synthetic PostgreSQL.

## 16. Exact staging deployment plan (requires separate human authorization; human-executed)

1. **Pre-conditions.** Final Stage 10.2 head green in GitHub CI; approved deployer credential and
   allowlisted CIDR available; accept that the refresh recreates the (synthetic) staging database.
2. **Lifecycle.** `deploy.sh up` deploys one commit per environment lifetime
   (`docs/runbooks/stage-9-4-staging-lifecycle-runbook.md` §3), so moving from `9190d5d` requires
   `deploy/aws/scripts/deploy.sh down`, teardown verification (runbook §6), then
   `deploy/aws/scripts/deploy.sh up` at the final Stage 10.2 head. No bypass.
3. **What `up` applies.** Phases 1–5 including `role-init` then `migrate`: migrations 0091–0093
   (Stage 10 W1 and 10.1) plus the runtime-role REVOKEs. Stage 10.2 adds no migration.
4. **Configuration.** Unchanged: staging runs `APP_ENV=staging`, `TEST_SUPPORT_ENDPOINTS_ENABLED=true`
   behind the edge allowlist, so the mock resolvers are wired with per-process random keys. Nothing
   needs a new secret. Simulate routes sign and deliver in-process, so this also holds at 2 replicas.
5. **Acceptance** (runbook §4, the Stage 10.1 checks, plus):
   - `/readyz` 200 from the allowlisted network, 403 outside;
   - KYC: a player creates a verification; the player response has no `provider_reference`; a KYC
     webhook signed with any key the caller knows returns 401 and the verification is unchanged;
     compliance staff approve through the staff review route;
   - casino: launch and simulated play work; a callback posted to another tenant's casino webhook
     route returns 401 with no ledger or tombstone effect;
   - payments: deposit, reversal and 409 via the simulate route; a cross-tenant payments webhook 401;
   - sportsbook settle/void/rollback simulation and the Back Office bet lifecycle view work;
   - CloudWatch logs contain no key material, signatures or provider references in auth-failure lines.
6. **After acceptance:** record the new staging SHA in `docs/progress.md`; the old staging KYC data
   is gone with the database.

## 17. Next-stage recommendation

Human authorization is required; nothing has been started.
1. **Authorize the staging refresh (§16)** to close KYC-WH-1's live exposure on staging — the highest-value
   next step.
2. Then a planning gate for a **pre-real-provider hardening stage**: WH-VENDOR-SCHEME-1,
   CAS-CAP-ROLLBACK-1 (with casino reconciliation), MOCK-ADAPTER-PROD-1, and the secret-store design for
   real webhook resolvers — the prerequisites for any first real vendor.
3. Optionally raise the Stage 9 concurrent-login timeout margin (CI-FLAKE-281) as a reviewed test change
   if it recurs.
