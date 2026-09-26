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

Local CI replay: <pending>

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
