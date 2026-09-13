# Active Stage

## Stage 3C — Financial Hardening & Operational Controls — Complete

Status: **Complete, pending human approval to authorize Stage 4.**
Stage 3B (Core Financial Infrastructure) was substantively approved by
the human, who then issued the Stage 3C directive: a focused hardening
pass closing five specific gaps Stage 3B's own specialist review left
documented but open, before Stage 4 domain implementation begins.
Casino, sportsbook, bonus, B2C frontend, partner console, real PSP, and
real crypto integrations were explicitly out of scope.

### Objectives (as instructed at the Stage 3C gate)

1. Eliminate the documented possibility that a staff member who is also
   the withdrawing player can approve their own withdrawal, at the
   strongest appropriate layer (database, not merely audit-detectable).
2. Resolve `provider_capability_amount_limits`' ambiguous tenant
   ownership.
3. Give a `submitted` withdrawal with no provider callback a bounded,
   safe resolution path - never blind retry, never automatic re-routing
   to a different provider.
4. Operationalize the ledger-vs-projection reconciliation stream Stage
   3B built but never scheduled.
5. Replace the flat, asset-blind four-eyes threshold constant with a
   real tenant/brand/asset-scoped configuration boundary, with a clean
   (not-yet-implemented) enforcement point for a future MFA/step-up
   requirement.
6. Run a focused specialist review (`ledger-finance`, `payments`,
   `security`, `architect`, `backend`, `qa`, `code-reviewer`) attempting
   to break each of the above, and fix every P0/P1 finding before
   completion - not merely the items originally listed.

### Completed work

Full itemized account, including every specialist-review finding and its
resolution: `docs/progress.md`'s "Stage 3C" section. Summary:

- **Five new migrations** (`0029`-`0033`): staff-person linkage +
  authoritative self-approval trigger; `provider_capability_amount_limits`
  tenant ownership + RLS; reconciliation mismatch classification; the
  `withdrawal_policies` configuration table; a specialist-review fix
  migration (RLS player-scope guards, composite brand FK, fail-closed
  `CHECK` constraints on not-yet-enforceable columns, a simplified
  self-approval trigger).
- **New/changed Go code**: `internal/withdrawal/policy.go` (the approval
  policy configuration boundary), `internal/reconciliation/scheduler.go`
  (the hourly per-tenant sweep with panic recovery and bounded graceful
  shutdown), stranded-hold resolution (`LockSubmittedForResolution`,
  `newResolveWithdrawalHandler`), and the four-eyes distinct-approver
  count now deduped by `staff_users.person_id`.
- **Adversarial tests A-O** (the Stage 3C directive's full mandatory
  list) plus additional tests the specialist review's own findings
  required (a direct multi-account four-eyes bypass proof, a
  provider-amount-mismatch-on-resolve proof, asset-precision policy
  proofs) - all against real PostgreSQL 16.
- **Independent specialist review** (all seven roles above) found
  several confirmed P0/P1s across ledger-finance, payments, security,
  architect, backend, and code-reviewer - most significantly: a
  four-eyes bypass where one person with two staff logins could supply
  both required approvals for a third party's withdrawal; a
  zero-config default policy threshold that was asset-precision-aware
  but not value-aware (ledger-finance rejected sign-off pending its
  fix); a real RLS gap on the two new tenant-owned tables; an unguarded
  panic path in the new reconciliation scheduler goroutine; and a
  missing provider-amount cross-check on withdrawal resolution. **All
  were fixed and each has a dedicated adversarial test proving the
  fix.**

### Verification performed

`go build ./...`, `go vet ./... -tags=integration`, and `gofmt -l .`
clean (one pre-existing, unrelated `errcheck` lint finding in
`internal/payments/mock.go`, predating this stage, not touched here).
Full test suite (`go test -tags=integration ./...`) passes with real
PostgreSQL 16. Migration round-trip (`up` → `down` → `up`) verified for
all five new migrations both individually and as a full chain. `git
status`/`git diff --stat` confirmed no production credentials and every
changed file traces to an approved Stage 3C item or a specialist-review
fix.

### Pending (to close out this stage)

- Commit and push this work to `claude/focused-wright-jw88w9`.
- Stage 3C Completion Report delivered to the human, ending with the
  required closing statement. No Stage 4 work begins until explicitly
  authorized.

### Blockers / genuine scope boundaries (not defects)

None block Stage 3C's own approved scope, which is complete. The
following are honestly labeled boundaries and open decisions for future
stages, not silent gaps - full detail in ADR 0023:

- **Self-approval enforcement is `PARTIALLY IMPLEMENTED`.** Authoritative
  and database-enforced for any staff/player pair where `staff_users.
  person_id` linkage is established. That linkage is optional,
  admin-asserted at staff-creation time, unverified, and has no update
  path - a staff account created without it (the default, including
  every pre-Stage-3C account) is invisible to both enforcement layers.
  Closing this fully needs either verified staff identity binding
  (KYC-adjacent, out of current scope) or a business decision to
  segregate staff-management and withdrawal-approval permissions.
- **Withdrawal resolution is manual, not automated.** `POST
  /v1/admin/withdrawals/{id}/resolve` gives staff a safe, bounded
  recovery path for a stuck `submitted` request; nothing currently
  schedules a call to it, so a human still has to notice and invoke it.
- **Reconciliation still implements one of the Blueprint's streams** and
  has known operability gaps: `period_start`/`period_end` don't actually
  bound the comparison, and an unresolved mismatch is re-inserted as a
  new `open` row every sweep with no deduplication. Neither is a
  correctness bug (no ledger/projection data can be corrupted by them).
- **No real production withdrawal approval threshold is decided.** The
  zero-config default fails closed (requires full approval for any
  non-zero withdrawal) specifically because no value-equivalent
  cross-asset default is possible without FX/market-price data, which
  remains out of scope. No admin API exists yet to configure a real
  `withdrawal_policies` row.
- Approver-role requirements (`required_approver_roles`) and
  jurisdiction-scoped policy (`jurisdiction_code`) are schema-ready
  (and, after this stage, `CHECK`-constrained to `NULL` to prevent a
  false sense of enforcement) but functionally inert.
- MFA/step-up itself remains **`NOT IMPLEMENTED`** (ADR 0017 unchanged).
  This stage only added the enforcement boundary
  (`ApprovalPolicy.RequireStepUp`/`ErrStepUpRequired`), which fails
  closed if a tenant configures it before MFA ships.
- Bonus financial posting, crypto deposit/withdrawal financial posting,
  crypto-custodian ledger settlement, and any PSP batch-settlement flow
  remain **NOT IMPLEMENTED**, per this stage's explicit block list. No
  bonus/crypto/bank-treasury accounting was invented to make code
  compile.

### Decisions/input still useful from the human before the next stage

1. Approve Stage 3C and authorize Stage 4 (per CLAUDE.md's stage gate,
   casino/sportsbook/bonus/B2C frontend/partner console/production
   deployment/real PSP/real crypto integrations do not begin
   automatically).
2. Decide whether closing the residual self-approval gap (unverified,
   optional `person_id` linkage) should be prioritized before any B2C
   launch where staff may also be players - and, separately, whether
   staff-management and withdrawal-approval permissions must be held by
   disjoint roles.
3. Decide the real production withdrawal approval policy (thresholds per
   tenant/brand/asset, required-approver-role rules, when step-up should
   be required) - none of it is invented here.
4. The already-open, non-blocking business/compliance tracks carried
   forward from Stage 0-3B remain open (`docs/decisions/0005`; ADRs
   0017/0018's open items; `brands`' public-read RLS breadth; the
   promo_liability/bank-treasury/crypto-custodian accounting decisions
   that still block bonus and crypto financial posting specifically).
