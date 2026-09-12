# Active Stage

## Security Hardening Pass (pre-Stage-3) — Complete

Status: **Complete, pending human approval to authorize Stage 3.** Stage
2 (Identity + Tenancy + Security) was completed, reviewed, and approved
by the human. Before authorizing Stage 3, the human directed a focused
security hardening pass addressing the Stage 2 completion report's
flagged debt. This is explicitly NOT Stage 3 — no wallet, ledger,
payments, PSP integrations, casino, sportsbook, bonus engine, KYC/AML,
responsible gaming, frontend, or back office work is in scope.

### Objectives (as instructed at the hardening-pass gate)

1. Fix the sessions RLS exposure identified as the most significant
   remaining Stage 2 debt, preferring database-enforced isolation over
   application-level `principal_id` filtering, while preserving
   authentication/session-lookup, self-service listing/revocation, and
   any genuinely-required platform-admin operations.
2. Define (architecture only, not implementation) the production design
   for staff MFA and step-up authentication.
3. Define (architecture only, not implementation) the production
   authentication signing model: platform-owned identity with KMS/HSM-
   managed asymmetric signing, keeping dev/test on the current simpler
   mechanism.
4. Run an independent security + code-reviewer regression review of the
   changes.
5. Full test suite (unit, integration, RLS, HTTP-security, auth, session,
   authorization, migration round-trip), lint, format, build.

### Completed work

See `docs/progress.md`'s "Security Hardening Pass (pre-Stage-3)" section
for the full itemized inventory. Summary:

- **Sessions RLS (migration 0018)**: the `FOR SELECT USING (true)` policy
  on `sessions` is replaced by three narrower policies (exact-token-hash
  match, tenant+principal match, tenant-scoped internal-operation-id
  match), each gated by a Postgres session variable set only by trusted
  code for the lifetime of one transaction — no new database role, no
  `BYPASSRLS`. Full design: `docs/decisions/0016-sessions-rls-hardening.md`.
- **Two architecture-only ADRs**, both explicitly `NOT IMPLEMENTED`:
  `docs/decisions/0017-staff-mfa-and-step-up-authentication.md` and
  `docs/decisions/0018-production-authentication-signing-architecture.md`.
- An independent `security` + `code-reviewer` pass on the sessions RLS
  change found **one blocking defect** (`revokeChainFrom`, the
  refresh-token-reuse chain-revocation walk, stopped early at an
  already-revoked mid-chain node, leaving live sessions further down an
  otherwise-compromised chain) and several should-fix items (an ignored
  `RowsAffected` on the chain-link write, no audit trail on the
  rotation-race-loser path, a missing tenant conjunct on one new policy,
  duplicated GUC-setting SQL, and test-coverage gaps for cross-principal
  writes and GUC isolation/leakage) — all fixed and re-verified, with new
  regression tests for each. Full itemized list:
  `docs/decisions/0016`'s "Corrections" section and `docs/progress.md`.

### Verification performed (all against a real local PostgreSQL 16, not mocked)

- `gofmt`/`go build`/`go vet` (including `-tags=integration`)/
  `golangci-lint`: all clean, 0 issues.
- Full unit and integration suite passing, including new direct-SQL RLS
  proofs (cross-tenant and cross-principal read AND write denial, exact
  token-hash-only visibility, internal-op-id single-row visibility, GUC
  non-leakage across transactions on a reused pooled connection, a 4-hop
  chain revocation with a pre-revoked mid-chain node) and the full
  pre-existing Stage 2 regression suite (unaffected).
- All 18 migrations (0001–0018) applied, fully rolled back, and
  re-applied cleanly.

### Pending (to close out this pass)

- Commit and push this work to `claude/focused-wright-jw88w9`.
- Security Hardening Completion Report delivered to the human, ending
  with the required closing statement. No Stage 3 work begins until
  Stage 3 is explicitly authorized.

### Blockers

None technical. The same non-blocking open business/commercial tracks
from Stage 0/1/2 remain open (`docs/decisions/0005`; retention-period
decisions in `docs/architecture/16-privacy.md`). New from this pass, also
non-blocking for engineering: the open human/compliance decisions listed
in ADRs 0017 and 0018 (which specific operations require step-up and
their thresholds; TOTP vs. WebAuthn; KMS/HSM provider and migration
timeline).

### Remaining security debt after this pass

- `brands`' public-read RLS policy remains broader than strictly needed
  (cross-tenant brand enumeration) — flagged in Stage 2 review, out of
  this pass's scope.
- The refresh-rotation race-loser path is now audited but deliberately
  does not revoke the winner's chain (see `docs/decisions/0016`'s
  reasoning) — worth revisiting if production data shows this path
  correlating with confirmed theft.
- MFA/step-up and KMS-based signing remain architecture only.

### Decisions/input still useful from the human

1. Approve this hardening pass and authorize Stage 3.
2. The open human/compliance decisions listed in ADRs 0017 (MFA policy)
   and 0018 (KMS/HSM provider, migration timeline) — non-blocking for
   Stage 3 engineering start, but worth resolving before either is
   actually built.
