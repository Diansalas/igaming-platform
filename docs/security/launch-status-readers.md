# Inventory of readers of `tenants.status` and `brands.status` (ADR 0112 slice 1, "T2")

- **Scope.** ADR 0112 section 3.3 requires slice 1 to inventory every reader of either status column (Go and SQL) and
  classify it, because migration 0128 adds a fourth value, `pending_launch`, and makes it the default for new rows.
- **Classification.** `A` = compares with `'active'` (`= 'active'` / `<> 'active'` / `!= "active"`): a `pending_launch`
  subject is refused exactly as a `suspended` one is, so the reader is **safe for the new value without change**. `L` = an
  explicit value list or an explicit `'closed'` / `'suspended'` literal: reviewed individually below. `D` = display only
  (returns the value, makes no decision). `W` = writer.
- **Result.** No reader needed a code change. Every gate reader is `A`; the only `L` entries are the reviewed allow-list
  (LF7 and the 0121 closure gate); there is no reader that treats an unknown status as permissive.
- **Pins.** `internal/tenant/status_readers_static_test.go`:
  `TestStatusLiteralPin_NoSuspendedOrClosedComparisonOutsideAllowList` (no code compares either column with
  `'suspended'` / `'closed'` outside the allow-list below) and
  `TestStatusReaderInventory_GoReadersAreExactlyTheReviewedSet` (the set of Go files reading either column by SQL is
  exactly the table in section 1; a new reader fails the test until it is added here and classified). Both are heuristics
  over source text, the same family as `no_trigger_disable_test.go`. Hardened after code review C-1 / L-1: the literal pin
  now scans EVERY `migrations/*.up.sql` (not a hand-picked file list) and
  `TestStatusLiteralPin_ReviewedReaderFilesCarryNoSuspendedOrClosedLiteral` bans any quoted `suspended` / `closed` code
  literal in the reviewed reader files whatever the variable, operator (`==`, `!=`, `IN`, `NOT IN`, `case`) or quote
  style (three non-gate lines are allow-listed: `ChangeStatus` input validation and two `player_accounts` constants).
  The inventory pin recognises a reader whether the status column appears before or after the `FROM` / `JOIN` of
  `tenants` / `brands` (`SELECT t.status ... JOIN tenants`). `TestStatusPins_NegativeCases_ReviewerViolationsAreDetected`
  proves the reviewer's two violating changes and the missed shape now fail the pins. Still heuristics: they do not
  parse SQL, and a status read built by string concatenation would escape them.

## 1. Go readers (non-test)

| File | Reads | Decision made | Class | `pending_launch` behaviour |
|---|---|---|---|---|
| `internal/tenant/gameplay_gate.go` | `tenants.status` under the shared per-tenant lock (R3, 0118/0121) | refuse new gameplay unless `active` | A | refused (GP010) |
| `internal/tenant/payment_initiation_gate.go` | `brands.status` `FOR SHARE` (H-SEC-5/11, H(8)) | refuse initiation unless `active` | A | refused |
| `internal/tenant/status.go` | `tenants.status FOR UPDATE` (`ChangeStatus`) | audited before-state of a change | W/D | a real change is refused by the database (LA020); same-status is a no-op |
| `internal/identity/player_account.go` | brand and tenant status in the registering transaction, plain non-locking read (ADR 0112 7.3, S4) | refuse registration unless both `active` | A | refused (409 `BRAND_NOT_ACCEPTING_REGISTRATIONS`) |
| `internal/identity/brand.go`, `tenant.go` | `status` of a brand / tenant row (slug / id lookups, list) | none (returned to the caller) | D | returned as `pending_launch` |
| `internal/identity/tenant.go` (`ListActiveTenantSlugs`) | `WHERE status = 'active'` | public listing of live tenants | A | not listed |
| `internal/rg/enumeration_sweep.go` | `WHERE status = 'active'` | which tenants the RG sweep enumerates | A | not swept |
| `internal/bonus/schedulers.go` | `WHERE status = 'active'` | which tenants the bonus schedulers enumerate | A | not scheduled |
| `internal/reconciliation/scheduler.go` | `status = 'active'` (ordinary sweep) and `status <> 'active'` (observation-only sweep, LF6) | sweep selection | A | observed like any non-active tenant; brand status is not a reconciliation dimension |
| `internal/kyc/enforcement_dormancy.go` | `t.status = 'active'` | dormant-trigger report counts live tenants only | A | not counted |
| `internal/kyc/outbox_worker.go` | `SELECT status FROM tenants`, `!= "active"` | defer KYC submission without consuming an attempt | A | deferred |
| `internal/payments/sweeper_resolution_only.go` | `tenants.status`, `brands.status` (`!= "active"`) | resolution-only sweeper mode | A | resolution-only (a pending tenant has no payments) |

Indirect readers (they use the `Status` field returned by `identity.GetTenantBySlug` / `GetBrandBySlug` /
`GetBrandByID`, no SQL of their own):

| File | Decision | Class | `pending_launch` behaviour |
|---|---|---|---|
| `internal/httpserver/webhook_preamble.go` (`t.Status != "active"`) | refuse a provider webhook for a non-active tenant | A | refused (401, indistinguishable) |
| `internal/httpserver/auth_routes.go` (login) | `brand.Status == identity.StatusPendingLaunch` answers 404 `unknown brand`; suspended / closed stay allowed so players can see balances (ADR 0112 7.3) | L (single literal, new) | refused |
| `internal/httpserver/admin_routes.go` | echoes `Status` in tenant / brand responses; `status` list filter is a bound parameter | D | shown as `pending_launch` |
| `internal/httpserver/credential_handlers.go` | none (brand lookup by slug only) | - | unchanged |

## 2. SQL readers (functions and triggers in the database)

Read from `pg_proc` on a fully migrated database (functions whose body reads either column), 2026-10-10:

| Function (migration) | Reads | Class | `pending_launch` behaviour |
|---|---|---|---|
| `ledger_gameplay_tenant_active_guard` (0118, 0121) | `tenants.status`, `= 'active'` pass; otherwise only terminal stake returns | A | GP010 for new wagering |
| `ledger_sportsbook_rollback_requires_void` (0121) | `tenants.status = 'active'` | A | as above |
| `tenants_status_change_gate` (0118, 0121) | writes the lock; counts open bets for a transition INTO `'closed'` | L (`NEW.status = 'closed'`, allow-listed) | `pending_launch -> closed` counts 0 open bets and proceeds |
| `financial_policy_required_approvals` (0113, 0124) | tenant and brand status, `= 'active'` / `<> 'active'` | A | non-active branch |
| `kyc_submission_outbox_guard` (0114) | `tenants.status IS NOT DISTINCT FROM 'active'` | A | not active |
| `ledger_adjustment_payload_refusal` (0113) | `p_tenant_status IS DISTINCT FROM 'active'` | A | non-active |
| `ledger_adjustment_*`, `payment_manual_resolution_*`, `withdrawal_hold_resolution_*`, `withdrawal_requests_approved_hold_freeze` (0113, 0115, 0124) | tenant / brand status to record `tenant_status_at_*`, `brand_status_at_*`, or `= 'active'` tests | A / D | recorded verbatim; none of the recording columns has a vocabulary CHECK (checked), so `pending_launch` is storable |
| `payment_manual_resolutions_guard`, `payment_manual_resolution_approvals_guard`, `payment_manual_resolution_execution_status` (0115, restated in 0125) | **`tenant_status = 'closed'` / `IS DISTINCT FROM 'closed'`** | **L, LF7** | `pending_launch` is treated as NOT closed; tenant-scope force-resolution stays allowed for a pending tenant, harmless because a pending tenant has no payments (ledger-finance LF7, ADR 0112 section 14.1) |
| `operating_country_policies_enforce_ceiling` (0076) | licence ceiling; no status decision on the tenant | - | n/a |
| `launch_subject_status_guard`, `launch_subject_insert_guard`, `launch_subject_owner_provisioned`, `launch_transitions_match_subject_at_commit`, `launch_requests_guard` (0128) | the new governance guards | W | by design |

Row-level-security policies: none reads `tenants.status` or `brands.status` (checked against `pg_policies`).
`CHECK` constraints: only `tenants_status_check` and `brands_status_check` (widened by 0128 to include `pending_launch`); no other
constraint has a tenant / brand status vocabulary (checked against `pg_constraint`).

## 3. Reviewed allow-list of `'suspended'` / `'closed'` literals (the pin's allow-list)

| Where | Literal | Why it is safe for `pending_launch` |
|---|---|---|
| `migrations/0115_payment_force_resolution.up.sql` and `migrations/0125_payout_unbound_resolution_m4.up.sql` (each: 3 sites, 3 allow-listed lines) | `tenant_status = 'closed'` / `IS DISTINCT FROM 'closed'` | LF7: `pending_launch` is NOT closed; harmless, a pending tenant has no payments |
| `migrations/0121_gameplay_stake_return_and_closure_guard.up.sql` | `IF NEW.status = 'closed'` in `tenants_status_change_gate` | the closure gate fires for a transition INTO `closed`, from any status |
| `migrations/0128_launch_authorisation_status_governance.up.sql` | `IF OLD.status = 'closed'` in `launch_subject_status_guard` | closed is terminal (ADR 0112 3.1) |

## 4. Writers

The only non-test writer of either column is `tenant.ChangeStatus` (`internal/tenant/status.go`, tenants only). Since
migration 0128 the database refuses any status change that is not accompanied, in the same transaction, by a governed
launch transition of an executing request (`zz_launch_status_governed`, SQLSTATE LA020), so `ChangeStatus` fails closed for
every real change (`ErrGovernedStatusChangeRequired`) until the slice-3 `launchgov` executor absorbs it. Brands have no Go
status writer at all. `identity.CreateTenant` / `CreateBrand` insert `pending_launch` explicitly; every other role is
refused any other initial status (LA021).
