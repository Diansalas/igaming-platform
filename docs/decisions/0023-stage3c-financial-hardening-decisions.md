# ADR 0023 — Stage 3C Financial Hardening: Cross-Cutting Decisions

Status: `IMPLEMENTED` (mechanisms below) with several explicitly
documented open decisions (§6). Issued at the close of Stage 3C
(Financial Hardening & Operational Controls) to record, in one place,
the cross-cutting architectural decisions that stage made — per
CLAUDE.md's rule that cross-cutting changes go through the architect and
are recorded here, not left implicit in code comments across five
packages.

## Context

Stage 3B shipped the core financial infrastructure (wallet, ledger,
withdrawal state machine, payment orchestration, an initial
reconciliation framework) and its own specialist review found and fixed
that stage's P0/P1 defects, but left several gaps explicitly documented
as open: withdrawal self-approval was audit-detectable but not
authoritatively prevented; `provider_capability_amount_limits` had
ambiguous tenant ownership; a `submitted` withdrawal with no provider
callback had no automated resolution path; the ledger-vs-projection
reconciliation stream existed but nothing ever scheduled it; and the
four-eyes approval threshold was a single flat, asset-blind constant.
Stage 3C closed each of these. This ADR is the durable record of how,
superseding the informal "OPEN DECISION" markers Stage 3B left in code
comments for the items resolved below.

## Decisions

### 1. Self-approval enforcement: person-linked, trigger-authoritative, defense-in-depth

`staff_users` gained a nullable `person_id` (migration `0029`), reusing
the existing cross-tenant `persons` identity rather than inventing a new
identity model. A `BEFORE INSERT` trigger on `withdrawal_approvals`
(`withdrawal_approvals_deny_self_approval`) denies any approval whose
approver resolves to the same person as the withdrawing player,
independent of the application layer — the authoritative layer the
Stage 3C directive required ("not merely audit-detectable"). A
service-layer `BeneficiaryCheck` closure in
`newApproveWithdrawalHandler` gives a clean HTTP 403 for the same
condition, as a defense-in-depth pairing, not a substitute.

Specialist review additionally found that the **four-eyes distinct-
approver count** (a separate mechanism from the self-approval check) was
keyed on `approver_principal_id` alone, so one person holding two staff
logins linked to the same `person_id` could supply both required
approvals for a THIRD PARTY's withdrawal without ever tripping the
self-approval trigger. `internal/withdrawal.Approve`'s readiness count
now dedupes via `COALESCE(staff_users.person_id, approver_principal_id)`
— migration `0033` also removed a redundant `is_automated_approval`
trust-flag shortcut from the trigger itself, so the guard is purely
data-driven.

**Known limitation, not closed by this stage**: `person_id` linkage is
optional and asserted by whichever admin creates the staff account, with
no independent verification and no update path once set. A staff account
created without a `person_id` (the common case, and the default for
every account created before migration `0029`) is invisible to both
enforcement layers. Closing this fully requires either verified identity
binding for staff accounts (KYC-adjacent, explicitly out of this
platform's current scope) or a business decision to segregate
staff-management and withdrawal-approval permissions so the same
principal can never both mint an unlinked staff identity and approve
withdrawals. Self-approval enforcement is therefore `PARTIALLY
IMPLEMENTED`: authoritative and trigger-level for any pair where linkage
is established, structurally unable to detect a pair where it is not.

### 2. `provider_capability_amount_limits`: tenant-owned, RLS matching sibling tables

Resolved as tenant-owned (migration `0030`), inheriting the parent
`provider_capabilities.tenant_id`, with a composite
`(provider_capability_id, tenant_id)` FK and a direct `tenant_isolation`
RLS policy replacing the prior subquery-based one. Specialist review
found this new direct policy (and the sibling new `withdrawal_policies`
table's) omitted the player-scope exclusion guard every other
staff/system-only financial table carries since migration `0028` —
migration `0033` added it to both. The backfill required temporarily
disabling RLS on both tables inside the migration's own transaction
(DDL, table-owner privilege, not a role-level bypass) — see migration
`0030`'s own comment for why this does not weaken the "even migrations
run as a NOBYPASSRLS role" invariant.

### 3. Withdrawal stranded-hold resolution: query-only, never resubmit or re-route

`LockSubmittedForResolution` + a single `provider.QueryStatus` call
(`POST /v1/admin/withdrawals/{id}/resolve`) is the only automated path
out of `submitted` when no callback arrives. It never calls `Withdraw`
again and never resolves a different provider than the one
`MarkSubmitted` originally recorded — verified by specialist review
reading the actual call path, not merely by design intent. Specialist
review also found the handler did not cross-check the provider's
confirmed `Amount`/`AssetCode` against the original request before
completing — the same class of check the deposit-reversal path already
makes via `payments.ErrCallbackProviderMismatch`. The resolve handler
now makes the identical check and refuses (409, request left at
`submitted` for investigation) rather than completing on mismatched
data. This endpoint is a **manual, staff-triggered** recovery path, not
an automated sweep — no scheduler currently drives `ListSubmittedForTenant`.

### 4. Reconciliation: scheduled, per-tenant-isolated, transaction-scoped-advisory-locked

`internal/reconciliation.RunSchedulerLoop` runs an hourly (configurable
via `RECONCILIATION_INTERVAL_SECONDS`) sweep over every `active` tenant,
each inside its own transaction with a `pg_try_advisory_xact_lock`
keyed by tenant id (`hashtextextended`, 64-bit, after specialist review
flagged the original 32-bit `hashtext` as collision-prone at scale). One
tenant's failure is isolated and separately audited; the sweep only ever
inserts `reconciliation_runs`/`reconciliation_mismatches`/`audit_log`
rows, never mutating ledger or projection data. A mismatch found is now
logged at `Error` level (previously `Info`) — CLAUDE.md treats non-zero
drift as a P1 incident, and the runtime log should say so, not just the
durable row. The scheduler goroutine (`cmd/platform-api/main.go`) has
its own panic recovery (specialist review, P0: without it, a panic
inside a reconciliation sweep would crash the whole API process, not
just reconciliation) and graceful shutdown now waits, bounded, for an
in-flight tick to finish.

**Cross-tenant read pattern**: `allTenantIDs` reads `SELECT id FROM
tenants WHERE status = 'active'` via `db.Pool.WithoutTenant` — the one
legitimate platform-level enumeration in this package, matching the
existing `WithoutTenant` pattern used elsewhere (e.g.
`internal/identity`) for genuinely tenant-spanning platform operations.
It is not a precedent for ordinary domain code to read across tenants;
every subsequent read/write in the sweep opens that tenant's own
`WithTenant` scope.

**Known limitations, not closed by this stage**: `reconciliation_runs.
period_start`/`period_end` are recorded but do not actually bound the
comparison (`RunLedgerVsProjection` always compares all-time ledger vs.
projection totals) — an operator investigating a specific hour's drift
via this column would be misled. An unresolved mismatch is re-detected
and re-inserted as a new `open` row on every subsequent hourly sweep,
with no deduplication against an existing open mismatch for the same
key. One transaction per tenant, spanning that tenant's entire account
set, does not yet subdivide for a very large tenant on a shared cluster.
None of these are correctness bugs (no ledger/projection data can be
corrupted by any of them), but they are real operability gaps recorded
here as follow-up work rather than silently left undocumented.

### 5. Withdrawal approval policy: tenant/brand/asset-scoped, fails closed when unconfigured

`withdrawal_policies` (migration `0032`, hardened by `0033`) replaces
the Stage 3B flat, asset-blind threshold constant with a real
configuration boundary — see
`docs/architecture/withdrawal-policy-configuration.md` for the full
design. Two specialist-review findings changed its final shape:

- The original zero-config default derived "1000 major units of this
  asset" from the asset's own `decimal_exponent`, intending to represent
  comparable real-world value across assets. `ledger-finance` rejected
  this: decimal precision is not market value, and for BTC specifically
  it would have raised the effective four-eyes bar to roughly 1000 BTC —
  a large WEAKENING of protection relative to even the Stage 3B constant
  it replaced. Building a genuinely value-equivalent default requires
  FX/market-price data, explicitly out of this stage's scope. The
  default now fails closed instead: `ThresholdAmount = 0` for every
  asset when unconfigured, requiring the full `RequiredApprovals` for
  any non-zero withdrawal until a tenant configures a real,
  asset-appropriate threshold.
- `ResolveApprovalPolicy`'s selection query had no deterministic final
  tiebreaker; two rows tying on every specificity/recency key made which
  policy "wins" unspecified. Added `created_at DESC, id DESC` as a final
  tiebreaker, and (migration `0033`) fixed `withdrawal_policies.brand_id`
  to a composite `(brand_id, tenant_id)` FK matching every sibling
  table, closing a cross-tenant-dead-row class of bug the same review
  found.

`required_approver_roles` and `jurisdiction_code` are both schema-present
for future use but not yet resolvable/enforceable (see the architecture
doc's §5-6) — migration `0033` added `CHECK` constraints forcing both to
`NULL` until real resolution/enforcement exists, so a configured value
can never create a false sense of protection the platform does not
actually provide.

### 6. Remaining open business decisions (unchanged or newly surfaced by this stage)

- The real production withdrawal approval threshold(s) per tenant/brand/
  asset are not decided — nothing in this stage should be read as
  proposing a specific number.
- Whether policy-config-edit and withdrawal-approval permissions must be
  held by disjoint roles remains open (Stage 3B's original bypass #3,
  unchanged).
- Self-approval enforcement's completeness depends on staff `person_id`
  linkage being established and honest — see §1's known limitation.
- Approver-role requirements (`required_approver_roles`) and
  jurisdiction-scoped policy (`jurisdiction_code`) are schema-ready but
  functionally inert until their respective resolution mechanisms exist.
- MFA/step-up itself remains `NOT IMPLEMENTED` (ADR 0017 unchanged) —
  Stage 3C only added the enforcement boundary
  (`ApprovalPolicy.RequireStepUp` / `ErrStepUpRequired`), which fails
  closed if configured before MFA ships.
