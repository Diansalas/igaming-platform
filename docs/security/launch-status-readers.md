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
  style (two non-gate lines are allow-listed: the `player_accounts` constants; the former `ChangeStatus` input validation was removed with `ChangeStatus` in slice 3).
  The inventory pin recognises a reader whether the status column appears before or after the `FROM` / `JOIN` of
  `tenants` / `brands` (`SELECT t.status ... JOIN tenants`). `TestStatusPins_NegativeCases_ReviewerViolationsAreDetected`
  proves the reviewer's two violating changes and the missed shape now fail the pins. Still heuristics: they do not
  parse SQL, and a status read built by string concatenation would escape them.

## 1. Go readers (non-test)

| File | Reads | Decision made | Class | `pending_launch` behaviour |
|---|---|---|---|---|
| `internal/tenant/gameplay_gate.go` | `tenants.status` under the shared per-tenant lock (R3, 0118/0121); since ADR 0112 slice 2 also `brands.status` under the shared per-brand lock (`RequireBrandActiveForGameplay`, LF1, 0129; plain read bound to `tenant_id`, no row lock) | refuse new gameplay unless the tenant is `active`; refuse a NEW stake (casino bet, sportsbook placement) unless the brand is `active` | A | refused (tenant: GP010 / `tenant_not_active`; brand: `brand_not_active`, DB backstop GP011) |
| `internal/tenant/payment_initiation_gate.go` | `brands.status` `FOR SHARE` (H-SEC-5/11, H(8)) | refuse initiation unless `active` | A | refused |
| `internal/launchgov/execute.go` | `tenants.status` / `brands.status` `FOR NO KEY UPDATE` (the executor's subject lock) and the status UPDATE | the current status the governed request must match (`status <> from_status` refuses at execution) | W/D | values compared against the request's own `from_status`, never a literal; a status change is refused by the database unless the governed transition of an executing request exists in the same transaction (LA020) |
| `internal/launchgov/hard.go` | `tenants.status` / `brands.status` in the read-only readiness / hard-precondition transaction | H-1 (legal transition from the current status: `moveFor`) | D | the status is returned to `moveFor`, which names every status explicitly through constants (no literal in the file) |
| `internal/launchgov/read.go` | `tenants` / `brands` joined in the platform launch queue read (slice 6) | projects `tenant_name` and `brand_name` onto queue rows | D (display only) | no status value is read, compared or branched on; the join exists only for names; the queue is platform-only and re-authorised on every call |
| `internal/identity/player_account.go` | brand and tenant status in the registering transaction, plain non-locking read (ADR 0112 7.3, S4) | refuse registration unless both `active` | A | refused (409 `BRAND_NOT_ACCEPTING_REGISTRATIONS`) |
| `internal/identity/brand.go`, `tenant.go` | `status` of a brand / tenant row (slug / id lookups, list) | none (returned to the caller) | D | returned as `pending_launch` |
| `internal/identity/tenant.go` (`ListActiveTenantSlugs`) | `WHERE status = 'active'` | public listing of live tenants | A | not listed |
| `internal/rg/enumeration_sweep.go` | `WHERE status = 'active'` | which tenants the RG sweep enumerates | A | not swept |
| `internal/bonus/schedulers.go` | `WHERE status = 'active'` | which tenants the bonus schedulers enumerate | A | not scheduled |
| `internal/reconciliation/scheduler.go` | `status = 'active'` (ordinary sweep) and `status <> 'active'` (observation-only sweep, LF6) | sweep selection | A | observed like any non-active tenant; brand status is not a reconciliation dimension |
| `internal/kyc/enforcement_dormancy.go` | `t.status = 'active'` | dormant-trigger report counts live tenants only | A | not counted |
| `internal/kyc/outbox_worker.go` | `SELECT status FROM tenants`, `!= "active"` | defer KYC submission without consuming an attempt | A | deferred |
| `internal/payments/sweeper_resolution_only.go` | `tenants.status`, `brands.status` (`!= "active"`) | resolution-only sweeper mode | A | resolution-only (a pending tenant has no payments) |
| `internal/httpserver/withdrawal_hold_candidates_routes.go` | `tenants.status`, `brands.status` (`<> 'active'`), returned as display fields (A10c, ADR 0114 R3) | read-only list of the path tenant's approved withdrawals whose tenant or brand is not active (the only ones the HSEC release can act on); platform_admin only, platform-action audited, decides nothing | A (+ D) | a `pending_launch` tenant or brand counts as not active and would be listed if it held an approved withdrawal (it cannot hold one: no payments); listing authorises nothing, the four-eyes release and its database guards decide |
| `internal/stagingseed/market.go`, `internal/stagingseed/world.go` | `tenants.status`, `brands.status` (`!= 'active'` refusal) | staging seeding library (STAGING-PREP-3 / SA-1): refuses to seed a tenant or brand whose status is not `active` (activation is a governed launch decision, not this tool's); gated by APP_ENV AND the `database_environment` anchor; imported only by `cmd/seed-market` and `cmd/seed-staging-world` | A | no gameplay or payment effect; staging/development only |

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
| `launch_subject_status_guard` (0129, CREATE OR REPLACE of 0128's) | on `brands`, a real status change first takes the per-brand gameplay key EXCLUSIVE (`brand_status_gate_key`, LF1); otherwise 0128's body unchanged, including the allow-listed `OLD.status = 'closed'` | W / L (restated 0128 literal) | a change into or out of any status serialises against in-flight new-stake placements of the brand |
| `launch_subject_status_guard` (0130, CREATE OR REPLACE of 0129's) | adds C2: for a tenant change, counts governed brand transitions of that tenant in the same transaction (`from_status <> to_status`) and refuses | W | a tenant change after a brand change of the same tenant in one transaction is refused (LA020) |
| `launch_subject_status_guard` (0133, CREATE OR REPLACE of 0130's, ADR 0113 slice 2) | 0130's body byte for byte plus LA025: on a transition INTO `closed`, inside the same RLS-bound window, counts the subject's `player_withdrawal_hold` accounts whose balance rebuilt from `ledger_entries` is not zero (brand: joined through `wallets.brand_id`) | W | no new status reader; a closure from any status (including `pending_launch`) over a non-zero withdrawal hold balance is refused (LA025) |
| `launch_suspension_no_stale_pending_at_commit`, `launch_requests_reject_carryover`, `actor_proof_launch_requests_guard`, `actor_proof_launch_approvals_guard` (0130) | the governance records only (request status / action / payload hash); no tenant or brand status | W | by design |
| `ledger_wager_brand_active_guard` (0129, deferred constraint trigger on `ledger_transactions`, `casino_bet` / `sportsbook_bet` only) | `brands.status` of every player wallet the posting moves, under the shared per-brand key, `IS DISTINCT FROM 'active'` refuses | A | GP011 for a new stake; terminal stake returns, wins and settlements are not covered |

Row-level-security policies: none reads `tenants.status` or `brands.status` (checked against `pg_policies`).
`CHECK` constraints: only `tenants_status_check` and `brands_status_check` (widened by 0128 to include `pending_launch`); no other
constraint has a tenant / brand status vocabulary (checked against `pg_constraint`).

## 3. Reviewed allow-list of `'suspended'` / `'closed'` literals (the pin's allow-list)

| Where | Literal | Why it is safe for `pending_launch` |
|---|---|---|
| `migrations/0115_payment_force_resolution.up.sql` and `migrations/0125_payout_unbound_resolution_m4.up.sql` (each: 3 sites, 3 allow-listed lines) | `tenant_status = 'closed'` / `IS DISTINCT FROM 'closed'` | LF7: `pending_launch` is NOT closed; harmless, a pending tenant has no payments |
| `migrations/0163_payout_amount_dispute_m2_admission.up.sql` (A5; 1 site: `payment_manual_resolutions_guard`, the 0125 body verbatim plus the amount/asset-mismatch reasons) | `v_policy.tenant_status = 'closed'` | LF7, unchanged from 0125 |
| `migrations/0121_gameplay_stake_return_and_closure_guard.up.sql` | `IF NEW.status = 'closed'` in `tenants_status_change_gate` | the closure gate fires for a transition INTO `closed`, from any status |
| `migrations/0128_launch_authorisation_status_governance.up.sql` | `IF OLD.status = 'closed'` in `launch_subject_status_guard` | closed is terminal (ADR 0112 3.1) |
| `migrations/0129_brand_gameplay_gate.up.sql` | the same `IF OLD.status = 'closed'` (0129 restates 0128's guard with the LF1 brand lock added) | closed is terminal, unchanged |
| `migrations/0130_launch_authorisation_actor_proof.up.sql` | the same `IF OLD.status = 'closed'` (0130 restates the guard again with the C2 one-subject-per-transaction rule added) | closed is terminal, unchanged |
| `migrations/0130_launch_authorisation_actor_proof.up.sql` | `IF NEW.status = 'closed'` (the closure backstop: LA023 hold-bearing withdrawals, LA024 brand open bets) | it reacts to a status being SET to closed (any prior status) and reads no subject status |
| `migrations/0133_launch_closure_withdrawal_hold_balance_arm.up.sql` | the same `IF OLD.status = 'closed'` and `IF NEW.status = 'closed'` (0133 restates 0130's guard byte for byte and adds the LA025 ledger closure arm, ADR 0113 slice 2) | closed is terminal, unchanged; LA025 reacts to a status being SET to closed and reads the subject's `player_withdrawal_hold` accounts and their `ledger_entries` (and `wallets.brand_id` for a brand), no subject status |
| `migrations/0136_player_brand_binding_immutable.up.sql` | `AND b.status = 'closed'` in the up pre-flight (a one-shot `DO` block, not a function or trigger) | ADR 0118 s5 class 4: an INFORMATIONAL count of non-zero `player_cash` / `player_withdrawal_hold` balances (rebuilt from `ledger_entries`) on wallets of a closed brand, reported by `RAISE NOTICE` with counts only; it refuses nothing and gates nothing. `pending_launch` is not closed and a pending brand has no player funds, so it is not reported (correct) |
| `migrations/0139_staff_lifecycle.up.sql` | `OR (OLD.status = 'suspended' AND NEW.status IN ('active', 'offboarded'))) THEN` in `sf_staff_transition` (SF010) | ADR 0119 S5 + S6: the STAFF status graph of `staff_users` (invited / active / suspended / offboarded). Not a tenant or brand status; it is matched only because the trigger reads `OLD.status`. Gates no tenant / brand behaviour |

## 4. Writers

Since ADR 0112 slice 3 the ONLY non-test writer of either column is the `launchgov` executor
(`internal/launchgov/execute.go`, `updateSubjectStatus`: one `UPDATE tenants` and one `UPDATE brands`);
`internal/launchgov/status_writer_static_test.go` pins that (a heuristic over source text, with a non-vacuity test), and that
`ChangeStatus` no longer exists (`tenant.ChangeStatus`, the former unauthorised writer, TENANT-STATUS-AUTHZ-1, was removed; its
GP020 typed error and translation stay in `internal/tenant/status.go`). The database refuses any status change that is not
accompanied, in the same transaction, by a governed launch transition of an executing request (`zz_launch_status_governed`,
SQLSTATE LA020), and since migration 0130 every request and approval carries a signed actor proof (ADR 0110), so a runtime
session that merely sets session settings can no longer fabricate the decision. A tenant status change after a brand status
change of that tenant in one transaction is refused (security C2, LA020). `identity.CreateTenant` / `CreateBrand` insert
`pending_launch` explicitly; every other role is refused any other initial status (LA021). The integration-tag-only
`internal/testsupport` fixtures (`launchfix`, `launchdrive`) are exempt from the writer pin; `internal/tenant/launch_fixture_pin_test.go`
pins EACH of the two packages separately: every source file carries the `integration` build tag (removing the tag from either
package fails a test) and no non-test file outside `internal/testsupport` imports it (a planted importer fails a test; both
checks have a negative test over a temporary tree). Before the security review of slice 3 this document said both fixtures were
pinned while the test covered `launchfix` only; that was incorrect and is corrected here and in the test.

The writer scan (`status_writer_static_test.go`) is an aid, not the control: it recognises `UPDATE tenants|brands ... SET ... status =`
including quoted identifiers (`"tenants"`, `"status"`, `public."brands"`), the column-list form `SET (status, ...) =`, and
`INSERT ... ON CONFLICT ... DO UPDATE SET status` / `MERGE ... UPDATE SET status`, each with a planted-offender test. It does not
parse SQL, and a status write assembled by string concatenation escapes it. The binding control is the database trigger
(`zz_launch_status_governed`, LA020), which refuses every such write without a governed same-transaction decision.
