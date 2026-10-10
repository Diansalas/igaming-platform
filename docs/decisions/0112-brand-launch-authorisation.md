# ADR 0112 - Brand and tenant launch authorisation (manual, four-eyes, audited)

- **Status:** ACCEPTED pending implementation reviews (`architect`, 2026-10-10). Revision 2. DESIGN ONLY: no Go, SQL
  or migration is part of this document and every item is `NOT IMPLEMENTED` until a later change says otherwise.
  Implementation slices still need the reviews named in section 13.
- **Revision history:** rev 1 PROPOSED (2026-10-10). Rev 2 applies the `security` review (S1-S15), the
  `ledger-finance` review (LF1-LF9) and the `product-owner-proxy` scope trim, under the orchestrator's rulings of
  2026-10-10. Where the trim conflicted with a security condition, the security condition was kept.
- **Decision type:** cross-domain architecture and security control (`tenant`, `identity`, `jurisdiction`,
  `actorproof`, `audit`, `casino`, `sportsbook`, `httpserver`, back office). It closes TENANT-STATUS-AUTHZ-1 by design.
- **Owner:** `architect`. **Reviewers per slice:** section 13.
- **Binding inputs:** owner directive of 2026-10-10 (technical readiness is separate from legal/licensing approval;
  explicit, manual activation governance); ADR 0006; ADR 0012; ADR 0045; ADR 0046; ADR 0095 sections 40.5/40.6
  (R3 gameplay gate, 0118/0121, Q-GP-1, Q-GP-5, Q-GP-6), 43 (H-SEC-5/11), 44-47 (H(8)); ADR 0099; ADR 0100;
  ADR 0104; ADR 0107 (closed-tenant funds, HD-CTF-4, HD-CTF-9); ADR 0110; ADR 0111 section 16.
- **No legal or regulatory claim.** This ADR provides configurable controls and a recorded human decision. It does
  not determine whether a licence is legally required, whether a brand is legally authorised, or whether any
  jurisdiction permits operation. A record produced by this mechanism is evidence that named humans took a decision
  on stated inputs, nothing more. The software gives no legal advice.

## 1. Context (verified at `81d3f42`)

| Fact | Where |
|---|---|
| `tenants.status` is `active|suspended|closed`, **default `active`** | `migrations/0001` line 14 |
| `brands.status` is `active|suspended|closed`, **default `active`**; `identity.CreateBrand` inserts `'active'` explicitly | `migrations/0008` line 11; `internal/identity/brand.go:133-143` |
| `identity.CreateTenant` inserts with the default (so `active`) | `internal/identity/tenant.go:221-234` |
| `POST /v1/admin/tenants/{tenantID}/brands` is `PermBrandWrite`, held by `tenant_admin` (tenant scope) | `routes.go:85`, `permission.go:818` |
| Brand UPDATE RLS is tenant-GUC only (`brand_tenant_update`); the runtime role's UPDATE grant on `brands` is load-bearing for the H-SEC/H(8) `FOR SHARE` read. **A tenant-scoped session can rewrite its own `brands.status` by SQL today; nothing governs it** | `0008:60-63`; ADR 0095 43.2(b) |
| `tenants.status` is writable only from a platform session (`tenants_platform_admin_update`, 0077) | ADR 0046 |
| `tenant.ChangeStatus` has **no authorization** and no route (TENANT-STATUS-AUTHZ-1, OPEN) | `internal/tenant/status.go:109-128` |
| Runtime gates test `= 'active'` / `<> 'active'` and fail closed: R3 gameplay (tenant only, 0118/0121 GP010), H-SEC-5/11, H(8), webhook preamble, reconciliation observation | `internal/tenant/*.go`, `0118`, `0121`, `webhook_preamble.go:151`, `reconciliation/scheduler.go:311-324` |
| **Gameplay is not gated on brand status at all** (R3 reads the tenant only) | `gameplay_gate.go`, `0118` |
| Tenant closure is refused with open sportsbook bets (GP020); casino rounds have no open state (Q-GP-6) | `0121` item B |
| Player registration and login never check `brands.status` | `identity.GetBrandBySlug`, `RegisterPlayer*` |
| `tenants.licensing_model` is `under_platform_licence|own_licence`, coupled to `licences.licensee` (0007 FK) and to credential ownership | `0001`, `0007`, ADR 0006 |
| Actor proof `platform` scope is admitted only for `capability_grant:` and `financial_policy_change:`; only `adjustment`, `payments`, `capability` may sign | `actorproof.go:151-153`; ADR 0110 section 7 |

## 2. Decision summary

1. **Status stays the single runtime truth.** `tenants.status` and `brands.status` remain the values every gate
   reads. What changes is *how a status may change*: only through a governed, audited, proof-bound decision,
   enforced by the database for every writer.
2. **New status `pending_launch`** on both columns; it becomes the **default** for new rows. Every existing gate
   already refuses it. No existing row changes value.
3. **Brand status gates new gameplay (LF1, decided).** A new per-brand gate, mirroring the tenant R3 gate, refuses new
   wagering on a non-active brand under a per-brand advisory lock that governed brand status changes take
   exclusively; terminal stake returns stay allowed (Q-GP-5 analogue). Its own slice and ADR 0095/0118 amendment.
4. **Append-only decision records**: requests, approvals, status transitions (section 4). A status change without a
   same-transaction `governed` transition bound to an executing request is refused (`LA020`).
5. **Manual, four-eyes, signed.** Activation, reactivation, closure and (optional) ratification need a requester and
   distinct platform approvers (distinct staff row AND distinct Person), each write carrying an ADR 0110 proof. The
   system never activates anything on its own, whatever the readiness result.
6. **Readiness is informational input**, computed server-side and snapshotted. A non-passing check never blocks by
   itself but must be explicitly acknowledged. Only the hard preconditions of section 5.2 refuse.
7. **Licensing is recorded, never judged.** Four licensing models including `other_manually_approved` and
   `not_applicable_recorded_determination`. No rule "no licence => not launchable" exists anywhere.
8. **Existing rows** get a `legacy_baseline` transition with their current status: zero behaviour change for the
   first tenant; the back office shows a "legacy baseline" badge; ratification is optional.

## 3. State machine

### 3.1 Subject statuses (both `tenants.status` and `brands.status`)

```
                 activate (4E)                suspend (S)
 pending_launch ───────────────▶ active ─────────────────▶ suspended
      │                            │  ▲                     │
      │ close (4E)                 │  └──── reactivate (4E)─┘
      ▼                            ▼                        │
   closed ◀──────── close (4E) ────┴──── close (4E) ────────┘
   (terminal)
```

| Action | From | To | Approval | Notes |
|---|---|---|---|---|
| `activate` | `pending_launch` | `active` | 4E | first launch |
| `suspend` | `active` | `suspended` | S (6.3) | fail-closed direction |
| `reactivate` | `suspended` | `active` | 4E | fresh readiness snapshot |
| `close` | `pending_launch`, `active`, `suspended` | `closed` | 4E | terminal; hard preconditions 5.2 H-6 |
| `ratify` | `active` or `suspended`, latest transition `legacy_baseline` | unchanged | 4E | optional; records licensing/jurisdiction/operator; no status change |

- `closed` is terminal. Reopening is **refused by the database** until HD-CTF-9 (OPEN) is decided, for tenants and
  brands. This removes `tenant.ChangeStatus`'s current `closed -> active` path (it has no caller today).
- Tenant and brand are **separate subjects with separate decisions** (H(8) decision 23 analogue). A tenant change never
  rewrites its brands' status. Activating a brand of a non-active tenant is allowed (staged launch) and has no runtime
  effect until the tenant is active.

### 3.2 Request states (S1, S3)

```
pending ──▶ executing ──▶ executed
   │            └──────▶ refused_at_execution
   ├──▶ rejected     (only with a same-txid reject approval)
   ├──▶ cancelled    (requester only, proof-bound)
   ├──▶ expired      (only when now() >= expires_at)
   └──▶ superseded   (only in the txid that executes a suspension of the same subject)
```

- **S1.** `pending -> executing` is admitted only (a) when the count of `approve` approvals by distinct Persons
  (excluding the requester) decided **in the same txid as the final one** reaches `required_approvals`, the final
  approval being inserted in this txid, or (b) for `action = 'suspend'` with `required_approvals = 0`. `executing` is
  a closed state: in the same transaction it must end `executed` or `refused_at_execution`; a deferred commit check
  refuses any request still `executing` at commit (`LA030`). `pending -> rejected` needs a `reject` approval decided
  in the same txid. **As implemented in slice 1 (S-8, C-3):** the approvals may be decided in earlier transactions
  (the count spans txids) but the FINAL approval must be inserted in the executing txid; a recorded `reject` approval
  blocks `pending -> executing` for good (`LA011`); the database does NOT bound approval age or the readiness-evidence
  hash at execution time. **Slice 3 (`launchgov`) must** reject an execution whose approvals are older than the policy
  window and whose recomputed readiness hash differs from the one the approvers saw; until then neither is enforced
  by any layer. The deferred commit check fails closed: a request it cannot see at COMMIT (cleared session scope) is
  refused with `LA030`, never passed. `superseded` is admitted only once the suspension's `governed` transition exists
  in the same txid (S-2).
- **S3.** The one-pending-request-per-subject partial UNIQUE **excludes** `action = 'suspend'`, so a suspension can
  never be blocked by a pending activation/closure. Executing a suspension marks every other `pending` request of the
  same subject `superseded` in the same transaction (audit row each). Expiry is computed on access: every read and the
  request INSERT guard treat `pending` with `expires_at <= now()` as expired, and the INSERT guard first moves such
  rows of the subject to `expired` so the UNIQUE never deadlocks a new request. TTL: technical 72 h (kept only for
  this purpose; not a policy value).
- **S8.** A tenant session may drive `pending -> executing -> executed` **only** for a suspension of its own brand
  (0 approvals). Every other execution happens in a platform session.

### 3.3 Mapping onto the gates

| Gate | Reads | `pending_launch` | Governed transitions |
|---|---|---|---|
| R3 tenant gameplay (0118/0121) | `tenants.status` under the shared per-tenant lock | refused (GP010) | a governed tenant change fires `tenants_status_change_gate` first (exclusive lock, GP020 on closure); race freedom unchanged |
| **Brand gameplay (new, LF1, slice 2)** | `brands.status` under a shared per-brand advisory lock | refused | the status guard takes the per-brand lock exclusively; same race freedom as R3 |
| H-SEC-5/11 | tenant (lock) + brand `FOR SHARE` | refused | brand UPDATE waits for in-flight `FOR SHARE` readers; suspend/close freezes withdrawal initiation (LF3), exactly as today |
| H(8) | brand `FOR SHARE` / plain read | deferred / skipped | unchanged |
| HSEC hold release (0124) | tenant + brand status | platform path only | unchanged |
| Reconciliation (LF6) | tenant status only (`<> 'active'`) | a `pending_launch` tenant is observed like any non-active tenant; brand status is not a reconciliation dimension | unchanged |
| Registration / login (new, 7.3) | plain non-locking read (S4) | refused | - |

Slice 1 must inventory **every** reader of either status column (Go and SQL) and classify it as `= 'active'` /
`<> 'active'` (safe for the new value) or an explicit value list (reviewed); a static test pins that no code compares
these columns to `'suspended'`/`'closed'` outside a reviewed allow-list (LF7-LF9: see section 14).

### 3.4 Safe migration path for existing rows (first tenant unaffected)

One migration (provisional number 0128; the orchestrator allocates), in this order:

1. Create the three tables, vocabularies and guards of section 4, **without** RLS enabled and without the subject
   status guard. All FKs from the new tables to `tenants`/`brands`/`licences`/`jurisdictions` are `ON DELETE
   RESTRICT` (S13).
2. Widen both CHECKs to `('pending_launch','active','suspended','closed')`. No row is updated.
3. **S12.** Backfill **before** `ENABLE`/`FORCE ROW LEVEL SECURITY` on the new tables: one `launch_status_transitions`
   row per existing tenant and brand, `kind = 'legacy_baseline'`, `from_status NULL`, `to_status = <current status>`,
   system actor. Count the source rows first and `RAISE` unless the inserted count equals the source count (0008
   lesson: never silently zero).
4. Enable and force RLS on the new tables, create policies and grants.
5. `ALTER COLUMN status SET DEFAULT 'pending_launch'` on both tables; install `zz_launch_status_governed` and the
   subject INSERT guard.
6. **Down (S13):** the first statement refuses (`RAISE`) if **any** `governed` transition exists, before anything is
   dropped; it also refuses if any row holds `pending_launch`. Otherwise drop guards, restore defaults to `'active'`,
   narrow the CHECKs, drop the tables.

Effect on the first tenant: same status on every row, same gate results, no request, nothing automatic. The back
office shows a "legacy baseline - no launch decision recorded through ADR 0112" badge. "Unchanged" means **the
existing assertions are unchanged** (LF2): fixtures move to the test-only path of 4.6, and the H-SEC-5/11, H(8),
R3/0121 and HSEC suites keep every assertion.

## 4. Data model

All three tables carry `tenant_id NOT NULL`, FORCE RLS, and append-only guards. No column stores a natural person's
name, email, phone or document number; people are referenced by `staff_users.id` and `person_id` only.

### 4.1 Closed vocabularies (CHECK plus Go constants; a new value needs a migration and an ADR)

| Name | Values |
|---|---|
| `subject_kind` | `tenant`, `brand` |
| `action` | `activate`, `suspend`, `reactivate`, `close`, `ratify` |
| `licensing_model` | `platform_licence`, `tenant_licence`, `other_manually_approved`, `not_applicable_recorded_determination` |
| `licensing_status` | `in_force`, `applied_pending`, `conditional`, `suspended`, `expired`, `not_required_per_determination` |
| request `status` | `pending`, `executing`, `executed`, `refused_at_execution`, `rejected`, `cancelled`, `expired`, `superseded` |
| transition `kind` | `governed`, `legacy_baseline`, `owner_provisioned` |

`licensing_model` here is the **recorded basis of this decision**, separate from `tenants.licensing_model` (which keeps
driving licence binding and credential ownership per ADR 0006 and is not altered). Consistency is hard precondition H-2.

### 4.2 `launch_authorisation_requests` (content immutable after INSERT)

| Column | Type / rule |
|---|---|
| `id` | uuid, server-forced |
| `tenant_id`, `subject_kind`, `brand_id` | `brand_id` NOT NULL iff `subject_kind = 'brand'`; composite FK `(brand_id, tenant_id) -> brands(id, tenant_id)` RESTRICT |
| `action`, `from_status`, `to_status` | `from_status` read from the subject at INSERT (forced, never caller-supplied); `to_status` derived from `action` |
| `licensing_model`, `licensing_status` | required for `activate`, `reactivate`, `ratify`; NULL for `suspend`/`close` |
| `licence_id` | uuid NULL, FK `licences` |
| `determination_reference` | text 1..200 NULL; an external document reference, never the document |
| `determination_by_role` | text 1..100 NULL; the responsible function ("external counsel"), not a person's name |
| `responsible_operator_name` | text 1..200 (required for `activate`/`reactivate`/`ratify`); legal entity operating the brand |
| `responsible_operator_registration` | text 0..100; company/registry number |
| `jurisdiction_ids` | uuid[] 1..32 distinct (required for `activate`/`reactivate`/`ratify`) |
| `conditions_note` | text 0..2000; the single bounded free-text conditions/notes field; PII lint (8.8) |
| `readiness_snapshot` | jsonb <= 32 KB: `{version, computed_at, environment, checks: [{code, result, detail_code}]}`, codes only |
| `readiness_snapshot_hash` | 64 hex, SHA-256 of the canonical snapshot |
| `acknowledged_check_codes` | text[]: every check whose result is not `pass` (H-5) |
| `reason_code` | closed reason catalogue; required |
| `requested_by`, `requested_by_scope`, `requested_by_person_id` | forced from the session (`platform` or `tenant`) |
| `required_approvals` | forced by trigger from 6.2, never caller-supplied |
| `payload_hash` | forced SHA-256 `k2_canonical` of every content column (incl. `tenant_id`, `subject_kind`, `brand_id`, `action`, `reason_code`, snapshot hash) |
| `status`, `created_at`, `expires_at`, `decided_at`, `refusal_code`, `executing_txid` | state columns; only these change, only by the guard |

### 4.3 `launch_authorisation_approvals` (append-only)

`id`, `tenant_id`, `request_id`, `decision` (`approve|reject`), `decided_by`, `decided_by_scope` (`platform` only),
`decided_by_person_id`, `payload_hash` (must equal the request's), `readiness_snapshot_hash_at_decision` (recomputed
by Go, recorded), `reason_code`, `decided_at` (forced `now()`), `decided_txid` (forced `txid_current()`).
UNIQUE `(request_id, decided_by_person_id)`.

### 4.4 `launch_status_transitions` (append-only history)

`id`, `tenant_id`, `subject_kind`, `brand_id`, `kind`, `from_status`, `to_status`, `request_id` (NOT NULL iff
`governed`), `executed_by`, `approver_ids uuid[]`, `actor_type`, `created_at`, `txid` (forced). UNIQUE
`(subject, txid)`.

### 4.5 Database guards (SQLSTATE class `LA`; callers branch on code only)

| Guard | Rule | Code |
|---|---|---|
| Request INSERT | session is a valid `platform` or `tenant` principal session (never acting/player/service); a tenant session only for its own tenant and only `brand` subjects; forces actor columns, `from_status`, `required_approvals`, `payload_hash`, `expires_at`; expires stale `pending` rows of the subject first (S3); validates vocabularies, bounds, FKs, H-2 | `LA001` session, `LA010` content |
| Request UPDATE | only state columns; only the transitions of 3.2 with the S1/S3/S8 conditions; `cancelled` by the requester only | `LA011` |
| Approval INSERT | platform session only; the request row was locked `FOR UPDATE` in this txid by the caller (S9; Go contract plus the UNIQUE); approver != requester by staff id and Person; Person not already decided; request `pending` and not expired; `payload_hash` matches | `LA012` |
| Transition INSERT (S2) | `governed`: a same-txid `executing` request for the same subject with matching `from`/`to`; `legacy_baseline` and `owner_provisioned`: `from_status IS NULL` and `current_user` is the table owner; `legacy_baseline` additionally refused if any transition exists for the subject | `LA013` |
| `zz_launch_status_governed` on `tenants` and `brands` (S10: `BEFORE UPDATE`, **no column list**, acts when `NEW.status IS DISTINCT FROM OLD.status`) | requires exactly one `governed` transition for this subject in this txid with `from = OLD.status`, `to = NEW.status`, whose request is `executing` in this txid; `closed` is never left; on `brands` takes the per-brand gameplay lock exclusively (LF1) | `LA020` |
| Subject INSERT guard on `tenants`, `brands` | `status` must be `pending_launch` unless `current_user` is the table owner; an owner INSERT with another status writes an `owner_provisioned` transition automatically (LF2) | `LA021` |
| Deferred commit check | no request left `executing`; every transition of this txid matches its subject's status | `LA030` |
| Append-only | UPDATE/DELETE/TRUNCATE on approvals and transitions; DELETE/TRUNCATE on requests | `LA099` |
| Proof (`zz_actor_proof_guard`, last BEFORE trigger) | ADR 0110 verifier on request INSERT, cancel, approval INSERT | `AP001`..`AP005` |

**Trigger order (LF5).** On `tenants`: `tenants_status_change_gate` (0118/0121: exclusive tenant lock, GP020) fires
before `zz_launch_status_governed`. On `brands`: `zz_launch_status_governed` takes the per-brand lock. A catalog test
pins the firing order on both tables and on the governed tables (`zz_actor_proof_guard` last). All functions pin
`search_path` (ADR 0108); none is `SECURITY DEFINER`.

### 4.6 RLS, grants, session shapes and the fixture path

| Table | Tenant session | Platform session | Acting / player / service |
|---|---|---|---|
| requests | SELECT, INSERT, UPDATE (cancel; own-brand suspend execution) own tenant | SELECT, INSERT, UPDATE | none (+ 0099 restrictive acting fence entry) |
| approvals | SELECT own tenant | SELECT, INSERT | none |
| transitions | SELECT own tenant; INSERT only for own-brand suspension (S8) | SELECT, INSERT | none |

- **S6: no new `brands` policy.** A platform-session executor changes `brands.status` with the 0121 technique: set
  `app.tenant_id` transaction-locally to the subject's own tenant (never a caller value), run the UPDATE through the
  existing `brand_tenant_update` policy, restore both GUCs, then write the platform-scope audit row. Tenant status
  changes use the existing `tenants_platform_admin_update`.
- Grants to `igaming_runtime`: SELECT, INSERT on all three; UPDATE on requests; no DELETE/TRUNCATE. The `brands`
  UPDATE grant stays (load-bearing for `FOR SHARE`); the status column is protected by the trigger, not the grant.
- **S11.** Production startup refuses to run when the connected role is the owner of `tenants`/`brands` (extend
  `db.VerifyRuntimeRoleInProduction` or add an equivalent check), so the owner-only paths of S2/LF2 are unreachable
  from a production process.
- **LF2 test-only fixture path.** A helper behind the `integration` build tag opens an owner-role pool and inserts
  tenants/brands with an explicit `'active'`; the subject INSERT guard writes the `owner_provisioned` transition. It is
  never reachable from the runtime role or a non-test build (static pin: no non-test importer, prooftest-style). All
  existing fixtures migrate to it in slice 1.

## 5. Launch readiness

### 5.1 Derived informational checks (computed by `launchgov.EvaluateReadiness`, read-only, never client-supplied)

Each yields `pass | fail | not_evaluable`; `not_evaluable` is not `pass`.

| Code | Derivation |
|---|---|
| `R-JUR-CONFIG` | each referenced jurisdiction has an in-effect `tenant_jurisdiction_configs` row (also hard, H-3) |
| `R-LIC-VALID` | licence-backed model: `EvaluateLicenceValidity(licence, now) = LicenceValid` |
| `R-RULESETS` | `kyc_ruleset_id`, `aml_ruleset_id`, `rg_ruleset_id` set for each referenced jurisdiction |
| `R-CURRENCIES` | every `allowed_currencies` entry is an active asset in the registry |
| `R-PAY-METHODS` | `allowed_payment_methods` non-empty and an approved provider credential handle exists for the tenant |
| `R-CATALOGUE` | the brand has at least one enabled casino game or sportsbook product |
| `R-PROVIDER-TIER` | the running process's components are production-eligible (`providerkind`); non-production is `fail` with `detail_code = synthetic_environment` (production start is hard-gated already) |
| `R-ALERTS` | ALERT-DELIVERY-1 routing has a real recipient for P1 kinds (today always `fail`) |
| `R-PLAYER-FUNDS` (LF3; `close`, and shown for `suspend`) | count of players with a non-zero balance and per-asset sums; count of open sportsbook bets. Requires acknowledgement; the funds fall under ADR 0107 / HD-CTF-4 (no new policy here) |

### 5.2 Hard preconditions (refuse; the minimum)

| # | Precondition | Why (not a legal judgement) |
|---|---|---|
| H-1 | subject integrity and a legal transition of section 3 | data integrity; `closed` terminal pending HD-CTF-9 |
| H-2 | licensing record internally consistent: `platform_licence` => `licence_id` set, licensee `platform`, `tenants.licensing_model = 'under_platform_licence'`, `tenants.licence_id = licence_id`; `tenant_licence` => same with `tenant`/`own_licence`; `other_manually_approved` => `determination_reference` and `determination_by_role` set (`licence_id` optional, same consistency if set); `not_applicable_recorded_determination` => `licence_id` NULL, both determination fields set, `licensing_status = 'not_required_per_determination'` | the directive requires the basis to be recorded and the record must not contradict the registry; every model is always available |
| H-3 | at least one jurisdiction referenced, each with an in-effect tenant jurisdiction configuration | jurisdiction is first-class (CLAUDE.md, ADR 0006) |
| H-4 | four-eyes count met (6.2), distinct staff and Persons, valid proofs | governance itself |
| H-5 | every check not `pass` is listed in `acknowledged_check_codes`; at execution Go re-evaluates and refuses (`refused_at_execution`, `readiness_changed_unacknowledged`) if a check not acknowledged in the request now fails | explicit human acknowledgement; no silent regression |
| H-6 | `close`: tenant: existing GP020 (open sportsbook bets; LF4: sportsbook only, casino rounds are the Q-GP-6 residual). Tenant **and brand**: no hold-bearing withdrawal request of the subject's players (LF3; states holding funds per ADR 0107), refused `LA023` | mirrors Q-GP-1 and keeps frozen-funds cases out of a terminal state; no new funds policy |

**Brand-closure open-bet mirror: CUT (LF1/LF4).** Rev 1's `LA022` is removed. Once the brand gameplay gate exists,
open bets of a closed brand can only be voided (terminal stake return), never settled with a payout or added to;
tenant closure keeps GP020. The open-bet count is shown in `R-PLAYER-FUNDS` for acknowledgement. Nothing else refuses:
a non-production environment, missing alert recipients or an expired licence under a licence-backed model are
acknowledged inputs, not blocks.

## 6. Authorization, four-eyes and actor proof

### 6.1 Static governance permissions (ADR 0099 3.2 style: not grantable, no acting session)

| Permission | Roles |
|---|---|
| `launch_authorisation:request` | `platform_admin` (any tenant); `tenant_admin`, `compliance` (own tenant, `brand` subject only) |
| `launch_authorisation:approve` | `platform_admin` only |
| `launch_authorisation:suspend` | `platform_admin` (any); `tenant_admin`, `compliance` (own tenant's brands only) |
| `launch_authorisation:read` | `platform_admin`, `tenant_admin`, `compliance` (own tenant) |

Tenant subjects are platform-only on both sides. `PermTenantWrite`/`PermBrandWrite` keep creating rows, now always
`pending_launch`.

### 6.2 Four-eyes

- `required_approvals` (forced): **1** distinct platform approver for brand `activate`/`reactivate`/`ratify`; **2**
  for tenant `activate`, for any `close`, and (S14) for a brand `activate`/`reactivate` with `licensing_model =
  'platform_licence'` and a tenant-scoped requester. Not lowerable by any tenant row or runtime setting.
- Self-approval ban: approver != requester by staff id AND `person_id`; one Person counts once; a NULL `person_id`
  on requester or approver refuses (0099 R-4).
- **S9** approval flow: `SELECT ... FOR UPDATE` on the request, then the approval INSERT, then (if the count is met)
  `pending -> executing`, H-1..H-6 re-check, transition INSERT, subject UPDATE (fires the gates), `executing ->
  executed` (or `refused_at_execution`), one audit row. `SET LOCAL lock_timeout = '5s'` as in `ChangeStatus`; a lock
  timeout on a four-eyes execution rolls back and returns a retryable 409.

### 6.3 Suspension (fail-closed direction)

One actor with `launch_authorisation:suspend` suspends immediately: a request with `action = 'suspend'`,
`required_approvals = 0`, inserted and executed in one transaction with a proof and a mandatory reason code (ADR 0045
7.1 item 4 asymmetry). It supersedes other pending requests of the subject (S3). **S4:** because a suspension waits for
in-flight gameplay/payment lock holders, the executor retries a suspension on lock timeout (bounded, e.g. 3 attempts
with backoff) before reporting failure; the refusal of every attempt is audited once. Reactivation is always
four-eyes; a tenant-scoped suspender can never reactivate alone.

### 6.4 Signed actor proof (ADR 0110 extension; `security` implements)

| Write | Operation | Scope | Target | Payload hash |
|---|---|---|---|---|
| request INSERT, `action = 'suspend'` | `launch_authorisation:suspend` | `platform` or `tenant` | `new` | request digest |
| request INSERT, any other action | `launch_authorisation:request` | `platform` or `tenant` | `new` | request digest |
| request cancel | `launch_authorisation:cancel` | requester's scope | request id | request `payload_hash` |
| approval INSERT | `launch_authorisation:approve` / `:reject` | `platform` | request id | request `payload_hash` |

**S7:** the request digest binds `tenant_id`, `subject_kind`, `brand_id`, `action`, `reason_code` and every other
caller-supplied content column including `readiness_snapshot_hash`; the verifier refuses `:suspend` unless
`action = 'suspend'` and `:request` when it is (AP004). SQL `actor_proof_require` and Go `platformOperation` admit the
`launch_authorisation:` prefix for `platform` scope, in lockstep; `platform_acting` is refused. The governed-table list
grows by two; `internal/launchgov` joins `TestStatic_OnlyFourEyesPackagesSign`'s allow-list (security review of that
change is mandatory). Transitions are bound through their same-txid executing request.

## 7. Fixing TENANT-STATUS-AUTHZ-1 and adjacent gaps

1. `tenant.ChangeStatus` stops being a public entry point: its body becomes the unexported executor step in
   `internal/launchgov`, keeping the GP020 translation and the separate-transaction refusal audit. A static test pins
   that the only non-test writer of either status column is that executor.
2. Independently of Go, `zz_launch_status_governed` refuses any status change without a same-txid governed transition
   of an executing request, so a runtime-role SQL session of any shape cannot change either status without ALSO
   fabricating that request, its approvals and its transition; the tenant-scoped `brands.status` rewrite of section 1
   is closed against a bare UPDATE. **S-3, stated precisely:** until the slice-3 `zz_actor_proof_guard` lands, the
   database four-eyes control rests on trusting session settings (GUCs): a runtime session that can set them can
   impersonate the requester and the approvers and drive a complete governed sequence. Slice 1 therefore makes the
   status change auditable and non-accidental, not unforgeable by a hostile runtime session.
3. **Registration / login gate.** `RegisterPlayer*` refuses unless brand and tenant are `active`; login refuses on a
   `pending_launch` brand. **S4:** a plain non-locking read in the registering transaction (no `FOR SHARE`, no
   advisory lock: registration moves no money and must not contend with a suspension); fail closed on an unreadable
   row; 409 `BRAND_NOT_ACCEPTING_REGISTRATIONS` for `suspended`/`closed`. **S-5/S-6:** a `pending_launch` brand, or an
   `active` brand of a `pending_launch` tenant, is hidden: registration AND login answer the same 404
   `unknown brand` as an unknown slug, so a pre-launch brand's existence is not disclosed. Login on `suspended`/`closed`
   stays allowed so players can see balances.
4. `CreateBrand`/`CreateTenant` insert `pending_launch`; creation no longer means launch.
5. HD-CTF-9 stays OPEN and is not decided here.

## 8. Audit, isolation and fail-closed rules

1. **Audit once.** One `audit_log` row per event in its own transaction: `launch_authorisation.requested|approved|
   rejected|cancelled|expired|superseded|executed|refused_at_execution|suspended|ratified`. A refusal that rolls back
   (proof, guard, GP020, lock timeout) writes one `..._denied` row in a separate transaction, at most once per call,
   codes and counts only. `launch_authorisation.executed` supersedes `tenant.status_changed`.
2. Platform-scope rows set `subject_tenant_id` (ADR 0104), so the existing tenant read path shows them; no new
   presentation entries in this scope (Deferred).
3. Tenant isolation: RLS on all three tables; `canActOnTenant` at the route; every query filters by the route-validated
   tenant, foreign ids answer 404 (K1-C1).
4. `tenant_id`/`brand_id` come only from the route and server-side lookups; actor columns are forced from the session;
   proofs only for the authenticated principal.
5. Fail closed: unreadable subject/licence/config/count refuses; a missing proof issuer is a 500; `not_evaluable` is
   not `pass`.
6. Readiness evaluation and `GET .../launch-readiness` write nothing.
7. Conditions are records; nothing in `conditions_note` is machine-enforced. The UI says so.
8. PII lint (Go, before INSERT) on `conditions_note`, `determination_reference` and operator fields: refuse email
   addresses, phone shapes, IBAN/PAN-like digit runs (>= 9 digits), wallet-address shapes; the database bounds length.
   An accident guard, not a classification guarantee.

## 9. HTTP API and Back Office (list only)

Routes (all `auth.Middleware`, `RequirePermission`, `canActOnTenant`; closed error codes `LAUNCH_*`):

1. `GET  /v1/admin/tenants/{tenantID}/launch-readiness?brand_id=` (read-only).
2. `POST /v1/admin/tenants/{tenantID}/launch-authorisations` (create; the server computes, snapshots and returns the
   readiness it bound).
3. `GET  /v1/admin/tenants/{tenantID}/launch-authorisations` and `.../{requestID}`.
4. `POST .../launch-authorisations/{requestID}/approve` | `/reject` | `/cancel`.
5. `POST /v1/admin/tenants/{tenantID}/brands/{brandID}/suspend`, `POST /v1/admin/tenants/{tenantID}/suspend`.
6. `GET  /v1/admin/tenants/{tenantID}/launch-status-history?brand_id=`.

Errors: 409 `LAUNCH_PRECONDITION_FAILED` (with check codes), `LAUNCH_UNACKNOWLEDGED_CHECKS`, `LAUNCH_REQUEST_PENDING`,
`LAUNCH_SELF_APPROVAL`, `LAUNCH_ILLEGAL_TRANSITION`, `LAUNCH_SUBJECT_CLOSED`, `LAUNCH_WITHDRAWALS_HOLDING_FUNDS`,
`LAUNCH_RETRYABLE_LOCK_TIMEOUT`, `TENANT_CLOSE_BLOCKED_OPEN_ROUNDS` (existing); 404 foreign ids; 403 permission; 500
signing failure.

Back Office panel (one panel on tenant and brand detail): status badge (incl. "pending launch", "legacy baseline");
readiness checklist with per-item acknowledgement and the fixed disclaimer "a recorded decision, not a legal
authorisation"; request form; platform approval queue (approve disabled for the requester and the same Person);
suspend action with mandatory reason; history timeline.

## 10. Test plan (as `igaming_runtime` (S11), private scratch databases, `-race -tags integration`)

**Migration:** up/down/up whole-schema snapshot; `(id, status)` byte-identical; baseline count equals source count with
data present; down refuses with any `governed` transition and with a `pending_launch` row, before dropping anything;
defaults `pending_launch` after up, `active` after down; FKs RESTRICT.

**Behaviour preservation:** H-SEC-5/11, H(8) incl. receipt site, R3/0121 and HSEC suites keep every assertion after the
fixture move (LF2); a matrix proves every gate refuses `pending_launch` for tenant and brand.

**Happy paths:** brand activate (all pass; non-passing acknowledged); tenant activate with two approvers; suspend /
reactivate; close; optional ratify; one audit row per event with the right `subject_tenant_id`.

**Adversarial (each refuses with the stated code and changes nothing):**
1. `UPDATE brands SET status` from a tenant-scoped runtime session and `UPDATE tenants SET status` from a platform
   session (`LA020`); the same after inserting a `governed` transition without an executing request (`LA013`); a
   `legacy_baseline`/`owner_provisioned` row from the runtime role (`LA013`); a transition with no UPDATE and a request
   left `executing` (`LA030`); an UPDATE that sets `status` to its own value plus another column (no refusal, no
   transition required: S10 semantics pinned).
2. `INSERT ... status='active'` into `tenants`/`brands` by the runtime role (`LA021`).
3. `pending -> executing` with too few approvals, with approvals from an earlier txid only, or by a tenant session for
   anything but its own brand suspension (`LA011`); `pending -> rejected` without a same-txid reject (`LA011`).
4. Self-approval by staff id, by a second account of the same Person, by a tenant or acting session (`LA012`/`LA001`);
   S14 case approved by only one platform approver (stays pending).
5. Proofs: none (AP001), wrong key (AP002), expired (AP003), wrong op/target/payload/scope, `platform_acting`, `:suspend`
   on a non-suspend request and `:request` on a suspend, a digest that omits `brand_id`/`action` (AP004), replay (AP005).
6. Cross-tenant ids via another tenant's path (404); a tenant session requesting a `tenant` subject (`LA001`).
7. Content tampering after INSERT (`LA011`); approval with a different `payload_hash` (`LA012`).
8. Unacknowledged non-passing check at request (`LA010`) and a check flipping between request and final approval
   (`refused_at_execution`, no status change, one audit row).
9. Licensing consistency failures for each model (`LA010`); every model accepted when consistent, including
   `not_applicable_recorded_determination` (no hard-coded licence requirement).
10. Reopen `closed` (`LA020`); tenant close with an open bet (GP020, refusal audited once); close with a hold-bearing
    withdrawal (`LA023`).
11. Concurrency (x50 under `-race`): two final approvals -> one execution; suspension racing an activation approval and
    a pending request -> suspension executes, the pending request is `superseded` or its approval refused; governed
    tenant/brand change racing a gameplay posting (tenant and new brand lock) and a deposit initiation -> serialised;
    suspension retry on lock timeout; a pending request past `expires_at` does not block a new one (S3).
12. Registration refused on `pending_launch`/`suspended`/`closed` brands without taking a lock (S4); login refused on
    `pending_launch`; other brands of the tenant unaffected.
13. PII lint and size bounds.
14. Static and catalog: only the executor writes status; only allowed packages sign; status-literal allow-list; trigger
    firing order (LF5); no new `brands` policy (S6); no new NULL-arm policy (A-18); fixture helper unreachable from
    non-test builds (LF2); startup refuses the table-owner role in production (S11).
15. Mutation-kill evidence for every guard branch (same-txid binding, Person check, closed-terminal rule, acknowledgement
    rule, GUC restore, owner-only kinds).

## 11. Decision items

**One combined item - orchestrator default, reversible by owner (LO-1).** Until the owner says otherwise:
(a) brand activate/reactivate/ratify need the requester plus 1 distinct platform approver; tenant activation, any
closure, and a `platform_licence` brand launch requested by tenant staff need 2; tenant staff may request but never
approve; (b) one authorised person may suspend immediately with a reason code (audited; reactivation always four-eyes),
and a non-active brand refuses new player registrations; (c) existing first-tenant rows keep their status as
`legacy_baseline` with a badge only; ratification is optional and has no deadline or automatic effect.

**Already decided here by orchestrator ruling:** LF1 (brand status gates new gameplay; terminal stake returns allowed).

**Open elsewhere, not re-asked:** HD-CTF-9, HD-CTF-4 (ADR 0107), Q-GP-6, NULL-ARM-WRITE-1, STAFF-LIFECYCLE-1,
ADR 0110 T3/T4, ALERT-DELIVERY-1.

**Engineering calls (reversible, within scope):** status as runtime truth; `pending_launch`; three-table model and class
`LA`; informational/hard split; acknowledgement rule; 72 h TTL (S3 only); static permissions; proof scopes; registration
gate; legacy backfill; PII lint; separate decision `licensing_model`; tenant and brand as separate subjects.

## 12. Residuals (stated honestly)

1. Readiness checks prove configuration presence, not legal or operational adequacy.
2. **S-1 / identity store (S15).** Person distinctness is defence in depth: Persons are optional, unverified and
   mintable (ADR 0099 section 9), so the structural control is that approvers are platform principals a tenant cannot
   mint over HTTP. An attacker who can make the application authenticate a minted or taken-over platform admin (ADR 0110
   T3, NULL-ARM-WRITE-1) obtains genuine proofs and could approve. Same residual as every four-eyes flow; not closed.
3. The table-owner role can bypass every trigger (ADR 0110 9.4); S11 keeps it out of production processes and
   `owner_provisioned` rows make owner inserts visible, but neither prevents a human with that role.
4. A suspension that commits after an initiation transaction committed does not stop that already-claimed attempt
   (ADR 0095 43.2).
5. Registration uses a non-locking read (S4): a registration racing a suspension may complete; it moves no money.
6. Casino rounds have no open state (Q-GP-6); closure checks count sportsbook bets only (LF4).
7. `R-PROVIDER-TIER` describes the process, not a per-tenant provider binding (none exists yet).
8. Brand-level jurisdiction scoping stays tenant-level (ADR 0012); jurisdictions on a brand decision are a record.
9. **S-3 (hard prerequisite).** Until `zz_actor_proof_guard` (slice 3) exists, DB four-eyes trusts GUCs (see 7.2).
   No HTTP or console route may write the three launch tables, and no real launch may be authorised through them,
   before that guard is in place and `security` has reviewed it.
10. **C-3(b).** A committed suspension supersedes pending requests of the same subject in its own transaction, but
    nothing yet fails a COMMIT of a suspension that left a stale `pending` activation/closure behind (a deferred
    check over the subject at commit). Not done soundly in slice 1; the slice-3 executor performs the supersede and
    its test asserts no stale `pending` request survives a suspension.
11. **Snapshot isolation (security r2).** An approval-based `pending -> executing` is refused (`LA011`) unless the
    transaction is READ COMMITTED, because a reject committed after a REPEATABLE READ / SERIALIZABLE snapshot is
    invisible to the reject check and the approvals guard only row-locks the request (SSI does not abort). The slice-3
    executor must run in READ COMMITTED.
12. **Slice-3 notes.** (i) A recorded `reject` is bound to its request only: it does NOT carry over to a later
    identical request on the same subject, so an approver can be "shopped" by re-requesting. Slice 3 must decide and
    record the policy (for example a cool-down, or surfacing prior rejects of the same payload hash to the next
    approvers); nothing in slice 1 prevents it. (ii) A tenant's own-brand suspension supersedes and discards the
    platform's pending requests for that subject (S3). The audit trail and the console must show that explicitly
    (request, superseding suspension, actor), not as a silent disappearance.
13. **C-5.** The slice-1 closure tests realise an admitted closure through the owner-run governed fixture, and the
    `tenant.status_change` audit assertion of the pre-0128 tests is not made (ChangeStatus fails closed). Slice 3
    restores that assertion on the executor. Runtime-role raw-UPDATE refusal (GP020 / LA020) is covered separately.

## 13. Work breakdown (slices; none starts before LO-1 is put to the owner or explicitly defaulted)

| Slice | Content | Owner | Reviewers |
|---|---|---|---|
| 1 | Migration (tables, vocabularies, RLS, grants, guards, CHECK widening, S12 backfill, defaults, S13 down); status-reader inventory and literal pin; LF2 test-only fixture path and migration of existing fixtures; S11 startup check; `CreateTenant`/`CreateBrand` pending; registration/login gate | backend (identity-compliance for the registration gate) | security, ledger-finance (0118/0121 interplay, fixtures), code-reviewer, qa |
| 2 | Brand gameplay gate (LF1): per-brand advisory lock key function, Go `RequireBrandActiveForGameplay`, DB backstop on new casino/sportsbook wagering, terminal stake returns allowed, lock taken exclusively by `zz_launch_status_governed`; ADR 0095 / 0118 amendment note | casino + sportsbook (design by ledger-finance) | **ledger-finance sign-off required**, security, code-reviewer, qa |
| 3 | **Hard prerequisites (S-3, S-8, C-3(b)): `zz_actor_proof_guard`, approval-age and readiness-hash bounds, suspension-supersede commit check.** `internal/launchgov`: readiness evaluator, request/approve/reject/cancel/suspend, executor (absorbs `ChangeStatus`), audit; permissions; ADR 0110 proof extension (ops, platform-scope allowance, signer allow-list) | backend (proof extension: security) | security, ledger-finance (LF3 closure preconditions), identity-compliance, code-reviewer, qa |
| 4 | HTTP routes, OpenAPI, error codes | backend | security, code-reviewer |
| 5 | Back Office panel with fixed disclaimers; a `pending_launch` badge and list filter for tenants and brands (C-8: the console must show the new status, not render it as an unknown value) | backoffice (UI frontend, copy ux-design) | product-owner-proxy, security |

Docs (architecture 15 and 36, task registry, decision register, HANDOVER) are updated by `architect` and the
orchestrator per slice; TENANT-STATUS-AUTHZ-1 is closed only after security reviews slices 1, 3 and 4. Labels: all
`NOT IMPLEMENTED` until each slice lands; the licensing content recorded is human-dependent and never a statement of
legal authorisation.

## 14. Review disposition (rev 2)

- **Security S1-S15:** applied in 3.2 (S1, S3, S8), 4.5 (S2, S10), 3.3/7.3/6.3 (S4), 4.6 (S6, S11), 6.4 (S7), 6.2
  (S9, S14), 3.4 (S12, S13), section 10 (S11 test role), 12.2 (S15). S5 was not restated in the orchestrator's ruling;
  it must be checked against the security review record before slice 1 starts.
- **Ledger-finance:** LF1 (2 item 3, 3.3, slice 2; brand-closure mirror cut), LF2 (4.6, 3.4), LF3 (5.1 `R-PLAYER-FUNDS`,
  5.2 H-6, 3.3), LF4 (5.2, 12.6), LF5 (4.5), LF6 (3.3). **LF7-LF9 were ruled "apply as stated" but their text is not
  reproduced in this ADR's inputs; they must be inserted verbatim from the ledger-finance review record before slice 1
  starts.** No interpretation of them is made here.
- **Product-owner-proxy trim:** applied: one `conditions_note` instead of attestation and condition vocabularies; eight
  derived checks plus `R-PLAYER-FUNDS`; ratify optional; partner-console extras deferred; TTL kept only for S3.

### 14.1 Review texts inserted by the orchestrator (verbatim from the review records)

- **LF7 (LOW):** T2's static pin must allow-list migration 0115's explicit `'closed'` comparisons
  (`tenant_status = 'closed'` / `IS DISTINCT FROM 'closed'`) as reviewed. `pending_launch` is treated as NOT closed there;
  tenant-scope force-resolution therefore stays allowed for a pending tenant, which is harmless because a pending tenant
  has no payments.
- **LF8 (LOW):** where section 6.1 says "the 0112 grant catalogue" it means the **migration-0112 scoped financial
  capability grant catalogue**, not this ADR.
- **LF9 (LOW):** section 10 adds a test of a governed brand closure racing a sportsbook bet placement. Before slice 2
  (brand gameplay gate) this race is expected to let the bet in (documenting LF1); after slice 2 it must be refused.
- **S5 (CONDITION, security review):** brand suspension and closure do not stop gameplay today: the 0118/0121 posting
  triggers and `GameplayStatus` read only `tenants.status`. Amendment text: either the 0118 posting gate also reads
  `brands.status` under a per-brand advisory key that brand status changes take exclusively (a gate change needing
  ledger-finance review), or the ADR states that brand-level status does not gate gameplay and H-6 is not race-free.
  **Disposition: the first alternative is chosen (slice 2, LF1).**

## 15. Deferred (recorded, not built)

- Human attestation vocabulary (licensing determination, player terms, AML/RG programme, support, provider contracts)
  and condition codes; machine-enforced conditions.
- Further derived checks: operating-market policy resolution (ADR 0045), actor-proof key provisioned, tenant-active
  for a brand subject, legacy-baseline ratification status, brand RG defaults.
- Mandatory ratification of legacy rows or any ratification deadline.
- Partner-console launch screens and ADR 0104 `TenantPresentation` entries for the new actions (the rows are already
  tenant-visible through `subject_tenant_id`).
- Scheduled effective time (`not_before`) for an approved activation; per-tenant provider tiering; brand-level
  jurisdiction configuration.
