# Active Stage

## Stage 3D — Withdrawal Governance Final Gate — Complete

Status: **Complete, pending human approval to authorize Stage 4.** Issued
immediately after the human approved Stage 3C, as an explicit, tightly-
scoped governance-hardening pass - "NOT a new financial architecture
stage." Casino, sportsbook, bonus, B2C frontend, partner console, real
PSPs, real crypto, and production MFA were explicitly out of scope (per
the directive's scope-restriction list, plus the completion gate's
additional exclusion of crypto financial posting, production KMS/HSM, and
regulatory certification).

### The approved business decision (verbatim)

> Withdrawal approval requires attributable Person identity and
> approver/beneficiary separation.

1. Any staff identity that can approve, reject, or submit withdrawals
   MUST be linked to a Person identity.
2. Withdrawal approval must always be attributable to a real Person.
3. A withdrawal approver must never be the same Person as the
   beneficiary/player requesting the withdrawal.
4. Staff-management authority and withdrawal-approval authority must be
   permission-separated.
5. A broad administrative role must NOT implicitly grant withdrawal
   approval authority unless the explicit withdrawal-approval permission
   is present.
6. Staff accounts without a verified Person linkage must NOT be eligible
   for withdrawal approval.
7. Do not create a second identity model. Use the existing Person/
   StaffUser/PlayerAccount identity architecture.

Full design, rationale, and the specialist-review findings/fixes:
`docs/decisions/0024-stage3d-withdrawal-governance-final-gate.md`.

### Completed work

Full itemized account: `docs/progress.md`'s "Stage 3D" section and ADR
`0024`. Summary:

- **Migration `0034`**: `staff_users.person_id` append-only (remediable,
  never launderable); `withdrawal_approvals` governance trigger requiring
  every human decision's approver to resolve to a linked, active staff
  account; `withdrawal_policies` gains a deny-`UPDATE` trigger.
- **RBAC**: withdrawal review/approve/reject/submit split into four
  permissions, held only by `RoleFinance`; `RoleTenantAdmin` holds none of
  them; a new `PermWithdrawalPolicyWrite` (tenant_admin only) gates the
  policy admin API.
- **Service-layer + HTTP-layer enforcement**: mandatory
  `ApproverEligibility` in `internal/withdrawal.Approve`/`Reject`; explicit
  equivalent checks in the submit/resolve HTTP handlers (no DB trigger
  covers those two transitions).
- **Staff→Person remediation endpoint** and a **minimal, insert-only
  withdrawal-policy admin API** (list/create/delete, delete requiring a
  reason code and capturing a before-image).
- **Adversarial tests A-H**, DB-level direct-SQL bypass tests, and policy
  security tests, all passing against real PostgreSQL 16.
- **Independent 7-specialist review** found and this stage fixed a **P0**
  (an early trigger draft let `is_automated_approval = true` bypass
  self-approval for a real staff member) and a **P1**, confirmed by three
  specialists independently (a tenant_admin could mint a brand-new
  `finance`-role staff account via `PermStaffManage` and self-escalate,
  closed by restricting `finance`-role creation to platform-scoped
  callers) - plus several P2s, all fixed with dedicated regression tests.

### Verification performed

`gofmt -l .`, `go build ./...`, `go vet -tags=integration ./...` clean.
Full test suite (`go test -tags=integration ./...`) passes against real
PostgreSQL 16, including a fresh `-count=1 -v` run of the three
most-affected packages (127 tests, 0 failures). Migration `0034`
round-tripped (`up` → `down` → `up`) twice - once mid-review after the P0
fix, once as final validation. `git status`/`git diff --stat` confirmed
no production credentials and no unrelated scope creep.

### Pending (to close out this stage)

- Commit and push this work to `claude/focused-wright-jw88w9`.
- Stage 3D Completion Report delivered to the human, ending with the
  required closing statement. No Stage 4 work begins until explicitly
  authorized.

### Blockers / genuine scope boundaries (not defects)

None block Stage 3D's own approved scope, which is complete. The
following are honestly labeled boundaries and open decisions for future
stages, not silent gaps - full detail in ADR `0024`:

- **A TOCTOU window exists on submit/resolve eligibility specifically**
  (payments specialist review finding): the Go-level eligibility check
  for those two transitions takes no row lock and is not re-verified
  immediately before the transition commits, unlike approve/reject (which
  the database trigger re-verifies authoritatively, inside the same
  transaction, immediately before commit). Not fixed this stage - the
  practical exploit window requires a concurrent, independent admin
  action against the SAME staff account mid-request, and the blast
  radius is a single already-approved payout, not a new unauthorized
  approval. Tracked as a defect to close in a future pass.
- **`is_automated_approval` remains a self-asserted boolean** with no
  service-principal registry or FK - unreachable via HTTP today (the only
  caller always passes `false`), but a future automated caller must be
  introduced carefully.
- **No real production withdrawal approval threshold, required-approver-
  role rule, or step-up requirement is decided** - unchanged from Stage
  3C; the policy admin API is a configuration boundary, not a business
  decision.
- MFA/step-up itself remains **`NOT IMPLEMENTED`** (ADR `0017` unchanged).
- Jurisdiction-scoped and approver-role-scoped policy rows remain
  schema-ready but functionally inert - unchanged from Stage 3C.
- A rolling per-player structuring check remains unimplemented -
  unchanged from Stage 3B/3C, an `OPEN DECISION` owned by
  `identity-compliance`.
- Bonus financial posting, crypto deposit/withdrawal financial posting,
  crypto-custodian ledger settlement, production KMS/HSM, and regulatory
  certification remain **NOT IMPLEMENTED**, per this stage's explicit
  block list.

### Decisions/input still useful from the human before the next stage

1. Approve Stage 3D and authorize Stage 4 (per CLAUDE.md's stage gate,
   casino/sportsbook/bonus/B2C frontend/partner console/production
   deployment/real PSP/real crypto integrations do not begin
   automatically).
2. Decide whether the submit/resolve TOCTOU window (above) should be
   closed before any production deployment where staff accounts can be
   suspended mid-session, or whether the documented low blast radius is
   acceptable to carry forward.
3. Decide the real production withdrawal approval policy (thresholds,
   required-approver-role rules, when step-up should be required) - none
   of it is invented here.
4. The already-open, non-blocking business/compliance tracks carried
   forward from Stage 0-3C remain open (`docs/decisions/0005`; ADRs
   0017/0018's open items; `brands`' public-read RLS breadth; the
   promo_liability/bank-treasury/crypto-custodian accounting decisions
   that still block bonus and crypto financial posting specifically).
