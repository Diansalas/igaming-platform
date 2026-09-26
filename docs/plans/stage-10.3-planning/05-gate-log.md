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
