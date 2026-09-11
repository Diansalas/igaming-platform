# Active Stage

## Stage 2 — Identity + Tenancy + Security

Status: **Implementation, local verification, and specialist-review
reconciliation complete, pending human approval to authorize Stage 3.**

### Objectives (as instructed at the Stage 1→2 gate)

Build the real identity/tenancy/security foundation: a distinct Person/
PlayerAccount/Tenant/Brand/Wallet-hook model; a production-oriented
authentication design (not just Stage 1's HMAC JWT proof-of-concept) with
key rotation, refresh/revocation, session tracking, and lockout;
foundational (not deferred) tenancy including the licensing-model/licence
consistency fix; a structural jurisdiction foundation (not every
jurisdiction's rules); a permission-oriented RBAC expansion; least-
privilege service-identity handling; an immutable audit foundation; a
meaningfully-tested set of security controls; a privacy architecture
document; API/OpenAPI updates; and strong security-focused testing
against real PostgreSQL for RLS/tenant-isolation proofs. Explicitly not
wallet/ledger, payments, casino/sportsbook providers, bonus engine,
production KYC/AML, complete RG engine, B2C frontend, back office, or
Partner Console.

### Completed work

See `docs/progress.md` for the full, labeled inventory. Summary: 8 new
reversible migrations (0007–0014) establishing brands/persons/
player_accounts/staff_users/sessions/login_attempts/audit_log with RLS
(three distinct patterns depending on the table's actual access shape)
plus a database-enforced tenant-licence consistency constraint; a new
`internal/audit` package; a rewritten `internal/auth` package (Argon2id,
key-rotation registry, kid/aud-validated JWTs with a legitimately-nilable
tenant claim, single-use rotating refresh tokens with reuse detection,
permission-based RBAC); a new `internal/identity` domain package; 15 new
HTTP endpoints across player auth/self-service, staff auth, and
platform-admin/tenant-admin provisioning and administration; a
`cmd/seed-admin` bootstrap CLI; an updated OpenAPI spec (17 paths); four
new ADRs (0011–0014); and a new privacy architecture document
(`docs/architecture/16-privacy.md`).

A six-specialist review pass (`architect`, `identity-compliance`,
`security`, `backend`, `qa`, `code-reviewer`) is complete. It found and
this session fixed six blocking defects (a silent-data-loss bug in
migration 0008's up script mirroring the down-script bug already fixed
this session, a genuine RLS gap on `persons`, non-atomic audit writes on
tenant creation, a staff-lockout bypass via email case variation, an
audit-log `TRUNCATE` gap, and a refresh-rotation concurrency race) plus
several should-fix items (slug-conflict error mapping, a login-timing
side-channel, unaudited refresh-token reuse, and more) and three
test-coverage gaps (session self-service, audit-write verification,
rotation concurrency) — all fixed and re-verified. One item was reviewed
and deliberately left as documented technical debt rather than fixed:
`sessions`' necessarily-public-read RLS policy. See `docs/progress.md`
for the itemized list.

### Verification performed (all against a real local PostgreSQL 16, not mocked)

- `go build`/`go vet` (including `-tags=integration`), `gofmt`,
  `golangci-lint`: all clean, 0 issues.
- Unit test suite: all passing.
- Integration test suite (build tag `integration`, real Postgres): all
  passing, including RLS tenant-isolation proofs on every new
  tenant-owned table (specific Postgres SQLSTATEs asserted, not "any
  error"), the audit-log immutability trigger proof, the licensing-model/
  licence consistency constraint proof, and full HTTP-level identity
  flows (registration, login/lockout, refresh rotation + reuse
  detection, RBAC role-distinction, cross-tenant denial,
  platform-admin-only provisioning).
- All 14 migrations (0001–0014) applied, fully rolled back, and
  re-applied cleanly — one real bug (migration 0008's down script
  ordering RLS enforcement before its own backfill insert) was found and
  fixed during this round-trip.
- OpenAPI spec validated: parses, every `$ref` resolves, 17 paths.

### Pending (to close out Stage 2)

- Commit and push this work to `claude/focused-wright-jw88w9`.
- Stage 2 Completion Report delivered to the human, ending with the
  required approval question. No Stage 3 work begins until that approval
  is given.

### Blockers

None technical. The same two non-blocking open business/commercial
tracks from Stage 1 remain open (cloud provider AUP confirmation, crypto
custodian vendor selection — `docs/decisions/0005`), plus retention-
period decisions flagged in the new `docs/architecture/16-privacy.md`
(deliberately not invented, deferred to a future human/legal decision;
does not block Stage 2 or Stage 3 engineering work).

### Decisions/input still useful from the human (non-blocking for Stage 3 start)

1. Approve Stage 2 and authorize Stage 3.
2. No new *business* decisions surfaced this stage beyond the residual
   items already tracked in ADR 0005 and the retention-period question in
   `docs/architecture/16-privacy.md` (neither blocks engineering work).
