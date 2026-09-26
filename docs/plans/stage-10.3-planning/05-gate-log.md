# Stage 10.3 gate log

## GATE 10.3-W0 — ADR / stage-definition consistency — PASSED 2026-09-26

- ADR 0092 (Stage 10.3 definition, ACCEPTED) and ADR 0093 (provider credential model and secret store)
  written; amendments to ADR 0022 §3, 0025, 0028, 0082 (A6), 0085 §1 and architecture doc 08 §9a
  (commit `6f4ee7c`). Registry and active-stage updated. Human rulings HD-10.3-1..4 (§22 of the
  proposal) reflected; PAYWH-TS-1/BRAND-1/RL-1 remain deferred.
- Pre-fix evidence E1–E6 captured against `c90e591` and committed (`4a2a978`); all six defects reproduced.
- Architect code-check findings and Orchestrator dispositions:
  1. Four-eyes precedent = migrations 0044/0086 as fixed by 0047/0089 (not ADRs); ADR 0093 requires the
     fixed shape (active principal, linked Person, distinct Person) plus a new 24 h approval expiry (C2).
  2. Scope bridge for tenant-scoped handle insert consuming a platform-scope approval — recorded in ADR
     0093; `security` concurrence at gate W2 (design review of W2a before code merges).
  3. "Task role is empty" lives in ADR 0084 (and `deploy/aws/modules/iam/main.tf`), not 0086 — ADR 0093
     cites both; unchanged in 10.3 (HD-10.3-2).
  4. Suspended/closed tenants: preamble rejects every route pre-verification (401 `tenant_inactive`); an
     unseen-original rollback rejected during suspension leaves no tombstone. HD-10.3-4 = UNCHANGED;
     disclosed in the ADR 0025 amendment with reconciliation as the detector.
  5. Staff review also writes `kyc_verifications.reason` → W1d validates staff input (no 500 on oversize).
  6. `kyc_documents.rejection_reason` (staff-entered, unbounded, shown on player document routes) →
     registered KYC-DOC-REJECTION-BOUND-1 (not 10.3 scope).
  7. Delivering the fingerprint HMAC key to staging needs a new platform secret + execution-role secret
     ARN in `deploy/` → **outside 10.3 (HD-10.3-2 excludes `deploy/` IAM code)**; locally the key comes
     from the existing config/secret path; recorded as STAGING REQUIRED and as a human decision to be
     raised before the future staging deployment (DEPLOY-FPKEY-1).
  8. "Outbound credentials never cached" interpreted as: handle row read per call; no caching in adapter,
     Authenticator, HTTP client or SDK session; resolver's secret-by-pinned-version cache allowed —
     `security` confirms at gate W2.
  9. E10 (late win racing rollback of that win) — W1c tests it; ADR 0082 A6 pre-authorizes L0.1 in `postWin`.
  10. `db.VerifyRuntimeRoleInProduction` skipped when APP_ENV missing — offered to W1b; else registered.
- Concurrences still to record (moved to gate W1, where the same specialists review the code): `casino`
  (E1/E3 response shape), `ledger-finance` (A6), `identity-compliance` (narrowed ADR 0028 amendment),
  `security` (items 2 and 8 at gate W2).
- No human decision required to proceed. W1a–W1d started in parallel worktrees.

## GATE 10.3-W1 — PASSED 2026-09-26

Branch `claude/focused-wright-jw88w9`; gate range `4a2a978..98a7f08`. Close-out recorded by
`architect` (docs only). Every claim below was checked against the code at `98a7f08`.

**Waves (W1a–W1d, parallel worktrees, merged).**
- W1a WH-VENDOR-SCHEME-1: `3ed6470` (non-mock conformance skips → failures, with pre-conversion red
  evidence), `45c3ca1` (per-adapter `VerificationScheme`, suite SC1–SC13, constant-time rule),
  `2059b56` (orchestrator-enforced Verify for payments, KYC, casino), `8ea13b9` (OpenAPI),
  `32ff812` (mutation record, 22/22 killed).
- W1b MOCK-ADAPTER-PROD-1: `4932c80` (synthetic/production-eligible markers, pure guard before
  `db.Connect`, missing `APP_ENV` = production).
- W1c CAS-CAP-ROLLBACK-1 + G-1: `46cf28b` (capability gates new bets only, tombstone always,
  E3/E10, L0.1 in `postRollback`/`postWin`, migration 0094).
- W1d KYC-REASON-BOUND-1: `ee2192f` (bound and sanitise, players see status only, migration 0095).
- `5d2c997`: chain-tip migration pins for 0094 + 0095 (code review #1).

**Reviews and verdicts** (`06-gate-w1-review-*.md`).
- `identity-compliance` (W1d + KYC parts of W1a): APPROVE WITH CONDITIONS (1: truncation flag in
  audit metadata; 2: staff-UI escaping test). Concurred with the narrowed ADR 0028 amendment.
- `ledger-finance` (W1c, with `casino` domain concurrence): APPROVE WITH CONDITIONS C1–C10.
  Concurred with A6 including the `postWin` extension, and with E1 503 / E3 200 `declined` /
  E10 409. **Re-verification after fix round A:** C1–C6, C8, C10 and both cleanup items met;
  A6 may move to `IMPLEMENTED`; new binding C11; R1, R2, H1 recommended; F-9 recorded.
- `security` (W1a/W1b/W1d, security side of W1c): APPROVE WITH CONDITIONS S-1 (Medium) to S-6
  (Low). No finding lets an unauthenticated callback be accepted or crosses tenants. Accepted
  credential revocation as the casino emergency stop (§7, C14), which answers `ledger-finance` C7.
- `code-reviewer`: NOT READY, 14 findings (no confirmed money-path or tenant-isolation bug).
  Findings #1–#13 were addressed in `5d2c997`, fix rounds A–C and the follow-ups (below). Its
  re-verification raised the remaining documentation items (#14 stale labels, a false ADR 0022
  checklist claim, undocumented `MarkProductionEligible()` and `ReasonTruncated`), closed by this
  close-out. The same re-verification checked `security` S-1..S-6 and `identity-compliance` 1–2 and
  found them FIXED, except the S-3 `code-reviewer.md` checklist item (agent configuration; human
  action, CR-CHECKLIST-HMAC-1) — recorded in `06-gate-w1-reverify-code.md`. `security` and
  `identity-compliance` did not run separate re-verifications.

**Fix rounds.**
- **C (docs, `575f9c6`):** ADR 0022, 0025, 0028, 0082, 0085, 0092, 0093 corrected to match the
  code (code review #5, #7; overclaims such as the `CallbackFixture` hook, the manifest-driven run,
  `crypto/subtle`, the free-round/jackpot case, late-bet shape).
- **B (`1a6287e`, `8324aa0`, `f275298`; evidence `3f9849b`, 35/35 killed, one equivalent mutant
  M7b disclosed):** S-1 (synthetic = the domain's canonical MOCK on a `SyntheticComponent()`
  adapter; `NewAdapterSchemeSet`; pre-DB `validateWebhookSchemes`; schemes registered with the
  guard); S-2 (`timestampBeforeMAC`; red set pinned as {SC7, SC2} — the SC2 overlap is
  unavoidable and disclosed); S-3 (whole-package lint scope); S-4 (resolvers in the bundle and
  registered, `mockProviderWiring` on `GuardEnvironment()`, AST test); S-5 / code review #4 /
  identity-compliance 1 (platform-side `NormalizeReason`, `reason_truncated`,
  `ProviderResult.ReasonTruncated`); S-6 / identity-compliance 2 (`KycCaseDetail.test.tsx`);
  code review #8, #9, #10, #12.
- **A (`5f98e23`, `a94e610`; evidence `e45831c`, 8/8 killed):** `ledger-finance` C1–C5, C10,
  cleanup (E3 audit target, tenant-qualified tombstone correlation fallback, dead `txs` map),
  code review #3 and #11; casino constructor routed through `MustAdapterSchemeSet`.
- **Follow-ups:** `8516951` (casino adapter-rule test `TestNewOrchestrator_MockSchemeOnlyFromSyntheticAdapter`,
  mutation M34; `NormalizeReason` idempotency test; dead casino `Orchestrator.now` removed);
  `98a7f08` (C11, R1 `casino_callback_replayed`, R2, H1).

**Audit-bloat defect, found and fixed.** C4's new assertions surfaced a real defect: `postWin`
(all four branches) and `postRollback`'s generic inversion path wrote a new audit row on every
redelivery of an already-posted fact. No financial effect. Fixed in `5f98e23` by gating on
`!postResult.AlreadyPosted`; `ledger-finance` ruled the fix correct and set the rule "postings
are audited once per fact; E3 rejections once per verified attempt" (ADR 0025 amendment item 9).
R2 (`98a7f08`) extended the gate to `postBet` and `postRollbackHeldWin`, where it was unreachable,
to make the rule structural.

**C11.** The `postRollback` generic-path gate had no killing test. `98a7f08` adds
`TestPostRollback_C11_GenericPathAuditGate_RedeliverySequentialThenConcurrent` (one reversal
row, the same `ledger_transaction_id` in all 10 responses, exactly one `casino_bet.rolled_back`
row). Mutation-killed (`evidence/w1c-mutation-kill.txt`) and run as the NOBYPASSRLS
`igaming_runtime` role (`evidence/w1c-c11-runtime-role.txt`), which also closes C6's record gap.
C11 met; W1c moves to `IMPLEMENTED — MOCK provider only`.

**Replay / sequencing note.** Run 3 of the first local CI replay failed to build because an
agent edited the working tree mid-run. That failure was an artefact of concurrent editing, not
of the code; the replay was re-run clean. Agents must not edit the tree while a CI replay runs
(this close-out was docs-only for that reason).

Local CI replay (at `98a7f08` code; only docs changed after): gofmt, vet, golangci-lint (0 issues), build,
migrate up + verify, race unit (31 packages), **3× race integration: 36 packages ok, 0 skips each**,
reversibility on a fresh DB (down 4 / up 4 / verify clean) — ALL LOCAL CI STEPS PASSED. (A first replay's run 3
failed to build because an agent edited the tree mid-run; that run was discarded and the replay re-run with the
tree frozen.)

GitHub CI: #332 (`3f9903a`) green. **#331 (`98a7f08`, same code) failed once** in `internal/db`
`TestCatalogueRLS_DeleteAndTruncateAreRefusedLoudly/casino_games_TRUNCATE` (1.00s) — identified on first
occurrence by the per-test annotation step added for CI-FLAKE-281. **Root cause (proven by live
reproduction, `evidence/ci-331-truncate-lock-repro.txt`):** `TRUNCATE … CASCADE` on the shared CI database formed
a genuine two-way lock cycle with another package's concurrent transaction; Postgres's default deadlock detector
(`deadlock_timeout = 1s`; no `lock_timeout` is configured anywhere) aborted it with SQLSTATE 40P01 after 1001 ms,
matching CI's 1.00s. (The first hypothesis — a lock timeout, 55P03 — was wrong and is corrected here.) **Fix
(`13b6407`):** the three CASCADE-TRUNCATE immutability tests (catalogue `casino_games`/`sb_sports`, ledger
`ledger_accounts`, sportsbook `sportsbook_bets`) now run on isolated scratch databases with the full migration
chain; isolation proven by holding conflicting locks on the shared DB while each passes. No retries, no raised
timeouts, nothing skipped. Repo-wide sweep: `08-ci-331-lock-contention.md` §4. CI-FLAKE-281 is **not**
reclassified — its evidence is inconclusive for this class. CI-331-LOCK: RESOLVED.

**Labels at this gate** (registry, ADR 0092 status):
- WH-VENDOR-SCHEME-1: `IMPLEMENTED` (MOCK schemes + real-scheme contract). Real vendors
  `PROVIDER DEPENDENT`; `KeyImplicit` `NOT IMPLEMENTED` until W2a; callback-fixture hook
  `NOT IMPLEMENTED`.
- MOCK-ADAPTER-PROD-1: `IMPLEMENTED`; an all-mock production binary refuses to start, by design.
- CAS-CAP-ROLLBACK-1, CAS-MULTIBET-WIN-1: `IMPLEMENTED — MOCK provider only`. ADR 0082 A6:
  `IMPLEMENTED`.
- KYC-REASON-BOUND-1: `IMPLEMENTED`.

**Documentation close-out (this entry).** ADR 0022 §3 amendment: the false "`code-reviewer`
checklist item" claim corrected to `NOT IMPLEMENTED` (CR-CHECKLIST-HMAC-1); stale "in progress"
lines refreshed; S-2 SC2 overlap and the casino constructor rule (`a94e610`, test `8516951`)
recorded; the requirement that a real scheme **type** implement `MarkProductionEligible()`
recorded (schemes are registered with the guard as their own components, so the first real
scheme otherwise fails production startup) — also in ADR 0085 §1 point 6. ADR 0028:
`ProviderResult.ReasonTruncated`. ADR 0025: item 9 (redelivery audit rule, R1 log). ADR 0082 A6
status and Tests text. ADR 0092 status.

**Open items carried forward.**
- CAS-WIN-IDEMP-1 (F-9, Medium; casino + ledger-finance): `postWin` has no already-posted
  short-circuit; a win redelivered after a rollback gets 400/409 instead of its original result;
  never pays twice; fix before G-6.
- PAY-SB-REPLAY-AUDIT-1 (Low): payments `orchestrator.go:796`, sportsbook `orchestrator.go:484`
  ungated audit on replay; owners confirm reachability.
- CAS-RECON-1 (W2b): the rejection record must capture E10, the G-1 409s and an E9 rollback
  naming an already-tombstoned original under a different reference (C9 and its extension).
- CI-FLAKE-281: proposed for W3.
- Unchanged from planning: security C1/C2/C4–C8/C12/C15/C17 (W2a/W3b), the W2 concurrences
  (items 2 and 8 of gate W0), launch-blocking list (`04-review-security.md` §8), free-round/
  jackpot conformance case, DEPLOY-FPKEY-1, LEDGER-MANUAL-ADJ-4EYES-1, CAS-WIN-ANOMALY-1.
- Informational, no condition: security I-2..I-5 (I-1 closed by the pre-DB validation); the
  capability audit before-image unlocked read; `ledger-finance` recommendation for a locked-branch
  replay case.

**Human decisions.** No human decision is required to proceed to W2, except
**CR-CHECKLIST-HMAC-1**: adding the "`hmac.Equal` only" item to `.claude/agents/code-reviewer.md`
is an edit to agent configuration, which needs the human. It does not block W2 (the lint is the
enforcing control).

## GATE 10.3-W2/W3 — PASSED 2026-09-26 (open findings carried)

Branch `claude/focused-wright-jw88w9`; gate range `6e3d74c..103b033` (W2a merged at `3ef18f2`; W2b
`a41acdf`; W3a `8a69412`; W3b `becc5c2`; close-out and fix rounds `cf775ef`..`103b033`). Close-out
recorded by the orchestrator's documentation agent (docs only; first drafted pending CI-342 in
`b10c6df`, finalized after CI-342-STOREOUTAGE was resolved). W2 and W3 are recorded together because
the W2 security condition closure and the W3 reviews landed in the same close-out. Passing this gate
carries open findings forward (below), notably **F-POOL-1 (Medium, launch-blocking)**.

**Waves (merged).**
- W2a PROV-CRED-RESOLVER-1 / PROV-OUTBOUND-CRED-1 / KYC-PROVIDER-SELECT-1: `8ce70ec`..`3ef18f2`
  (migration 0096, `internal/{secretstore,providercred}`, resolver, four-eyes activation, admin API,
  O4 selection; mutation record `96fc567`, 24/24 killed).
- W2b CAS-RECON-1: `a41acdf` (migration 0097 `casino_callback_rejections`, `casino_consistency`
  C1–C7, verified-only rejection record, read-only admin views; 25/25 killed); merge fix `a01f9b3`
  (init-app-role grants for 0097); `03f5ba6` (pre-existing rollback-count bug in an
  operating-market test); `7054e6e` (sweep tests scoped to their own tenants; CAS-RECON-SCALE-1
  registered).
- W3a CAS-RECON-STMT-1: `8a69412` (migration 0098, `casino_statement` stream, MOCK source,
  `db.Pool.WithTenantSnapshot`; 34/34 killed).
- W3b SECRETSTORE-AWS-1: `becc5c2` (`awssm` backend, local SDK fake only).
- W3 CI-FLAKE-281: `00f02ef` (`13-ci-flake-281-disposition.md`).

**Reviews and verdicts.**
- `security`, W2a code (`09-gate-w2-review-security-w2a.md`, at `3ef18f2`): APPROVE WITH
  CONDITIONS — W2A-SEC-1 (Medium; register the PROV-OUTBOUND-CRED-1 launch-blocking precondition)
  and W2A-SEC-2 (Low; log the matched `key_id` for `KeyImplicit` verification). Observations
  O-1..O-7, no action required. Accepted PROV-OUTBOUND-CRED-1 as `PARTIALLY IMPLEMENTED`
  conditional on W2A-SEC-1; approved the three tightenings (PC031, tenant-binding composite FKs,
  stricter ref regexes).
- `security`, W2b/W3a/W3b (`10-gate-w2w3-review-security.md`, at `becc5c2`): APPROVE WITH
  CONDITIONS. W2b, W3a, `a01f9b3`, `7054e6e` approved with no blocking finding (R-1 Low, R-2 Low,
  R-3 Info). W3b approved as delivered (unwired) with S-1 and S-2 (Medium) blocking any wiring of
  `awssm`; S-3 (Medium; Go toolchain), S-4 (Low; govulncheck unpinned), S-5 (Low; import-guard
  gaps). SDK version ruling: no security-driven bump required.
- `code-reviewer` (`10-gate-w2w3-review-code.md`, at `becc5c2`, committed in `3f77254`): NOT READY
  — small fixes; 10 findings (#1 High = W2A-SEC-1/-2 still open; #2–#5 Medium; #6 Low–Medium;
  #7–#10 Low). No money-writing bug; merge integration, permission union and tenant isolation
  verified.
- `security` re-verification (`11-gate-w2w3-reverify-security.md`, at `e5b6e17`, committed in
  `1138062`): **APPROVE WITH CONDITIONS.** W2A-SEC-1, W2A-SEC-2, S-1, S-2, S-4, S-5 CLOSED; S-3
  CLOSED in code with CI govulncheck evidence required (supplied by CI #341, below). Wiring `awssm`
  into `cmd/platform-api` approved. New: N-1 (Low; ambient `AWS_DEFAULTS_MODE`/`AWS_MAX_ATTEMPTS`/
  `AWS_RETRY_MODE` — IMDS calls at `New`, retries under the breaker), N-2 (Info; Secrets Manager
  client honours `HTTPS_PROXY`), N-3 (Info; ARN account not pinned against platform
  configuration). **N-1 fixed in `e80114b`** (refused at startup plus pinned defaults mode, retry
  mode and 2 attempts; 7/7 mutants killed). `security` verified the fix: **N-1 CLOSED** (addendum
  to `11-gate-w2w3-reverify-security.md`). The addendum raised **L-N1a** (Low; the rationale for
  the pinned retry count was inaccurate), fixed in `99bb5b2`: `awssm` SDK retry attempts 2 → 1, so
  the `secretstore` Fetcher is the only retry layer; mutant M46 killed; ADR 0093 updated. **N-2 and
  N-3 remain open under HD-10.3-2.**
- `code-reviewer` re-verification (`12-gate-w2w3-reverify-code.md`, at `e80114b`, committed in
  `302433d`): **READY WITH FOLLOW-UPS.** #1–#9 CLOSED (two mutants run by the reviewer, both
  killed); #10 OPEN, accepted as one Low follow-up **CODE-HYGIENE-10.3-1**; of its items,
  `secretstore.Router.Backends()` and `reconciliation.ListRunsForStream` were removed in
  `302433d`. New Low observations N-A (`awssm.NewWithSDKFake` in a non-test file, AST-guarded,
  fails closed) and N-B (C6 partition test finds the 0097 CHECK by `LIKE`), both folded into
  CODE-HYGIENE-10.3-1. Financial sign-off of the C2 deviation and C6 ruling is `ledger-finance`'s
  (paper 02 §2.19), not the reviewer's.
- `qa`, CI-FLAKE-281 (`13-ci-flake-281-disposition.md`): `IMPLEMENTED` — the 30 s value is a
  hang/deadlock guard; replaced by a calibrated ceiling (floor 30 s, cap 3 min), counts and
  assertions unchanged; 3× green local race-integration runs; injected-hang mutation still fails
  at ~2m17s; no recurrence in CI #331–#342. *(Discrepancy in that paper's §3 table: it lists
  #340 as a `govulncheck` failure. The GitHub job record shows #340 failed at the `golangci-lint`
  step, with govulncheck and every later step skipped. The registry row GO-TOOLCHAIN-VULN-1 is
  correct. Paper 13's §3 row was corrected by the orchestrator in `b10c6df`.)*
- `qa`, CI-342-STOREOUTAGE (`14-ci-342-store-outage-test.md`) and `security` ruling
  (`15-ci-342-security-ruling.md`): ruling A **REJECT** (`9df5869`, 64-connection test pool —
  removes the pool-starvation coverage); ruling B **ACCEPT WITH CONDITIONS** (`25a3537`, the test
  runs alone in its own blocking CI step: blocking, no retries, name guard, nothing else joins that
  step without its own ruling, failures are investigated, never re-run to green). New design
  finding **F-POOL-1 (Medium)**, below.

**Fix rounds.**
- **Close-out `040329a` (merged `c3bc313`):** W2A-SEC-1 (registry precondition + tripwire
  `TestOutboundPrecondition_EveryWiredAdapterIsSynthetic`), W2A-SEC-2 (`webhookauth.LogVerifiedKey`,
  per-domain log-capture tests), S-1 (credentials by allow-list: explicit ECS container provider,
  empty shared-file lists, source checked per retrieval, SDK aliases refused), S-2 (trust-root,
  endpoint and credential-source overrides refused; `awssm://` refs must be full ARNs in
  `ParseRef`), S-5 (guards widened to `github.com/aws/`), code review #5 and #6, W3b wiring. 38/38
  mutants killed (`evidence/w2w3-closeout-mutation-kill.txt`).
- **Toolchain `cf775ef`, `e5b6e17`, `0fbd0dc` (S-3, S-4, GO-TOOLCHAIN-VULN-1):** CI #337's first
  govulncheck run found reachable stdlib findings against go1.25.0 plus `golang.org/x/text` and
  `go.opentelemetry.io/otel/sdk`. Moved to **go1.26.8** (`go.mod` `toolchain` line, CI via
  `go-version-file`, Dockerfile `golang:1.26.8-alpine`); `golang.org/x/text` **v0.42.0**; otel
  **v1.46.0** (otelhttp v0.71.0); **govulncheck@v1.8.0**. golangci-lint was first bumped to
  v2.6.2, whose release binary is built with go1.25 and fails against the go1.26 module (CI #340);
  **v2.9.0** (first release built with go1.26) pinned in `0fbd0dc`.
- **Reconciliation fix round `dd30350` (merged `4fe39c6`; `ledger-finance`):** code review #2 (C4
  exemption decided by the original only), #3 (positive `house_gaming` rules for bets and wins;
  BONUS_SET bets a recorded deviation, G-6 unshipped), #4 (**C6 class ruling, paper 02 §2.19**: 6
  finding classes, 5 evidence-only; E9 different-reference is evidence only, removing the conflict
  with `casino_statement`), #7 (tombstone pairing disclosed), security R-1 (rejection write on
  `context.WithoutCancel` bounded by 2 s), R-2 registered as PROVIDER-REF-BOUND-1. 15/15 killed
  (`evidence/w2w3-fixround-recon-mutation-kill.txt`). Reconciliation still writes no money.
- **`294e0a0`:** code review #8 (`RequireAnyPermission` unit tests) and #9 (0092 hold-back derived
  from migration version). Mutation-tested per the commit message (all-of rewrite, empty-list
  allow, disabled cutoff: each fails a test); no separate evidence file.
- **`e80114b`:** N-1 (above). **`99bb5b2`:** L-N1a (above). **`302433d`:** dead code removed;
  CODE-HYGIENE-10.3-1 registered.
- **CI-342-STOREOUTAGE (`c5f05a9`, `fea3b8d`, `9df5869`, `25a3537`, `103b033`).**
  `TestStoreOutage_DoesNotPinPool` (`internal/providercred`) failed in CI #342 (1.27 s, the
  unrelated-query bound) and #347 (`99bb5b2`, 2.66 s, the held-long count). **Root cause:** CPU
  scheduling delay while every package's `-race` test binary runs concurrently on the 4-vCPU
  runner; not a product defect in the bounded paths the test measures. History, recorded as it
  happened: `qa`'s first fix `c5f05a9` (slack 400 → 900 ms) was **rejected by the orchestrator** as
  a timing weakening; the second, `9df5869` (64-connection test pool), was **rejected by `security`**
  (ruling A). Both commits remain in history and are **superseded**. **Final:** `103b033` returns
  to the shared 20-connection pool with every bound unchanged (4 slots / 4 / 500 ms / slack 400 ms)
  and richer failure diagnostics; `25a3537` runs the test alone in its own blocking CI step (ruling
  B); `fea3b8d` makes the CI annotation step capture the lines printed before `--- FAIL`.
- **F-POOL-1 (Medium, `security`, found while measuring CI-342): `NOT IMPLEMENTED`.** ADR 0093 §5's
  property "a store outage does not pin the pool" does **not** hold at the production pool size of
  10: an unrelated query waits about 1.4 s until the breaker opens. Launch-blocking unless fixed or
  explicitly accepted by the human; needs an `architect` + `security` decision (ADR / §5
  amendment).

**Mutation evidence (all `docs/plans/stage-10.3-planning/evidence/`).** `w2a-mutation-kill.txt`
24/24 (M22 first NOT KILLED, test corrected, re-run killed); `w2b-mutation-kill.txt` 25/25 (M-C4a
first SURVIVED, test strengthened, disclosed); `w3a-mutation-kill.txt` 34/34;
`w2w3-closeout-mutation-kill.txt` 38/38 plus 7/7 for N-1 (M34 first did not compile, M36 first
SURVIVED, both corrected and disclosed; two defence-in-depth lines not mutated, with reasons);
`w2w3-fixround-recon-mutation-kill.txt` 15/15. Every survivor was fixed by strengthening a test,
never by weakening the mutant.

**Process notes (recorded as they happened).**
- The W2a `security` review file (`09-gate-w2-review-security-w2a.md`) was not committed on its
  own: it was swept into the W3a feature commit `8a69412` because it was in the working tree when
  that commit was made. Its content is unchanged; the record is only less tidy.
- A `code-reviewer`-side agent closing findings #8/#9 committed `5dc8abb`, which also swept in the
  untracked `11-gate-w2w3-reverify-security.md`. The agent then soft-reset (`reset: moving to
  HEAD~1`) and recommitted without it as `294e0a0` (visible in the reflog). The security
  re-verification report was then committed separately and unchanged in `1138062`. `5dc8abb` is
  not on the branch.
- `12-gate-w2w3-reverify-code.md` notes an uncommitted modification to
  `internal/httpserver/stage9_concurrency_integration_test.go` at its review time; that was the
  CI-FLAKE-281 change, later committed in `00f02ef`.

**CI (GitHub Actions, branch `claude/focused-wright-jw88w9`).**
- #337–#339: failure — first `govulncheck` findings (GO-TOOLCHAIN-VULN-1).
- #340 (`e5b6e17`): **failure** at the `golangci-lint` step (v2.6.2 binary built with go1.25);
  fixed in `0fbd0dc`.
- #341 (`0fbd0dc`): **green**, including govulncheck (the S-3 CI evidence the security
  re-verification asked for).
- #342 (`1138062`, docs-only commit on top of `294e0a0`): **FAILED** —
  `TestStoreOutage_DoesNotPinPool` (CI-342-STOREOUTAGE; root cause and fix above).
- #343 (`e80114b`), #344 (`302433d`), #345 (`00f02ef`), #346 (`b10c6df`): green.
- #347 (`99bb5b2`): **FAILED** — the same test (CI-342, second mechanism).
- #348 (`25a3537`) and #349 (`103b033`): **green, all jobs**, including govulncheck and the isolated
  timing step.
- **Local CI replay at `103b033`** (scratchpad `ci-local.sh` mirroring the CI split, golangci-lint
  v2.9.0): gofmt, vet, lint (0 issues), build; migrate up to 98 + verify; race unit tests 36
  packages ok; **3× race integration: 40 packages ok, 0 skips each**, plus the isolated timing test
  3/3; reversibility 98 → 94 → 98, verify clean — ALL PASSED. `qa`'s earlier local
  `internal/httpserver` failures were caused by concurrent agents sharing the local database; the
  clean replay passed 3/3.

**Labels at this gate** (registry Stage 10.3 section; ADRs 0092/0093):
- PROV-CRED-RESOLVER-1: `IMPLEMENTED` (`memory` tests only, `devfile` development only; `awssm`
  see SECRETSTORE-AWS-1).
- PROV-OUTBOUND-CRED-1: `PARTIALLY IMPLEMENTED` — launch-blocking precondition registered and
  enforced by the tripwire. (ADR 0092's target was `IMPLEMENTED`; not met.)
- KYC-PROVIDER-SELECT-1 (O4): `IMPLEMENTED`.
- CAS-RECON-1: `IMPLEMENTED` (code + tests; C3 order sub-check `NOT IMPLEMENTED`; compensation
  LEDGER-MANUAL-ADJ-4EYES-1 `NOT IMPLEMENTED`).
- CAS-RECON-STMT-1: `MOCK` — the match against the MOCK source is tautological; detection is
  proven by test-only divergent sources; the real statement source is `PROVIDER DEPENDENT`.
- SECRETSTORE-AWS-1: `PARTIALLY IMPLEMENTED` — code, wiring and fake tests `IMPLEMENTED`;
  IAM/KMS/`deploy/` `NOT IMPLEMENTED` (HD-10.3-2); real-AWS use and drills `STAGING REQUIRED`.
- CI-FLAKE-281: `IMPLEMENTED` (disposition).

**Open items carried forward.** **F-POOL-1** (Medium; launch-blocking unless fixed or accepted by
the human; architect + security); the ruling-B conditions on the isolated CI step;
CODE-HYGIENE-10.3-1 (Low; includes the `DerivedTokenCache` bound, which is a precondition for its
first production caller); PROVIDER-REF-BOUND-1 (Low, before real-provider go-live);
CAS-RECON-SCALE-1 (Medium, before real-money multi-tenant load); N-2/N-3 and the IAM/KMS,
`HTTPS_PROXY` and IRSA questions (HD-10.3-2); DEPLOY-FPKEY-1; R-3 (Info, future Back Office
rendering); W2A-SEC-2 residual (`matched_predecessor` bool, Info); branch protection + CODEOWNERS
on `.github/` (security S-4 "CI step integrity"; not verifiable from the repository); all W1
carry-forwards not closed here (CAS-WIN-IDEMP-1, PAY-SB-REPLAY-AUDIT-1, CR-CHECKLIST-HMAC-1).
(The stale SECRETSTORE-AWS-1 registry text about an outstanding `awssm` re-review has been
corrected.)

**Gate verdict: PASSED 2026-09-26**, with the open findings above carried (F-POOL-1 is
launch-blocking, not gate-blocking). Stage 10.3 completion report:
`docs/governance/stage-10.3-completion-report.md`. **Stop: the next stage requires explicit human
authorization.**
