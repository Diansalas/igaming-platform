# ADR 0112 - Brand and tenant launch authorisation (manual, four-eyes, audited)

- **Status:** PROPOSED (`architect`, 2026-10-10). DESIGN ONLY. Nothing here is implemented. No Go, SQL or
  migration is part of this document. Every item below is `NOT IMPLEMENTED` until a later change says otherwise.
- **Decision type:** cross-domain architecture and security control (`tenant`, `identity`, `jurisdiction`,
  `operatingmarket`, `actorproof`, `audit`, `httpserver`, back office). It closes TENANT-STATUS-AUTHZ-1 by design.
- **Owner:** `architect`. **Mandatory reviewers before ACCEPTED:** `security` (all of it), `ledger-finance` (sections
  2, 6 and 7: interplay with the 0118/0121 gameplay and closure gates, and with frozen player funds),
  `identity-compliance` (sections 4 and 5: licensing, jurisdiction, attestations), `product-owner-proxy` (scope),
  `qa` (section 10).
- **Binding inputs:** owner directive of 2026-10-10 (technical readiness is separate from legal/licensing approval;
  explicit, manual activation governance); ADR 0006 (hybrid licensing); ADR 0012 (brand distinct from tenant);
  ADR 0045 (operating market, licence validity, kill-switch asymmetry); ADR 0046 (tenants/licences RLS); ADR 0095
  sections 40.5/40.6 (R3 gameplay gate, 0118/0121), 43 (H-SEC-5/11), 44-47 (H(8)); ADR 0099 (governance permissions,
  session families, Person distinctness); ADR 0100 (four-eyes conventions); ADR 0104 (`subject_tenant_id`);
  ADR 0107 (closed-tenant funds, HD-CTF-9); ADR 0110 (signed actor proof); ADR 0111 section 16 (HSEC pattern:
  request/approval tables, execution in the final approval's transaction, closed SQLSTATE class).
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
| Brand UPDATE RLS is tenant-GUC only (`brand_tenant_update`); the runtime role holds UPDATE on `brands` and it is load-bearing for the H-SEC/H(8) `FOR SHARE` read (pinned by `brands_grant_pin_integration_test.go`). **A tenant-scoped session can therefore rewrite its own `brands.status` by SQL today, and no route or trigger governs it** | `0008:60-63`; ADR 0095 43.2(b) |
| `tenants.status` is writable only from a platform session (`tenants_platform_admin_update`, 0077) | ADR 0046 |
| `tenant.ChangeStatus` has **no authorization** and no route (TENANT-STATUS-AUTHZ-1, register: OPEN, owner + security) | `internal/tenant/status.go:109-128` |
| Every runtime gate tests `status = 'active'` / `!= 'active'` and fails closed on anything else: R3 gameplay (`GameplayStatus`, 0118/0121 trigger GP010), H-SEC-5/11 (`RequireActiveForPaymentInitiation`), H(8) (`RequireBrandActive`), webhook preamble, reconciliation `<> 'active'` observation | `internal/tenant/*.go`, `0118`, `0121`, `webhook_preamble.go:151`, `reconciliation/scheduler.go:311-324` |
| Tenant closure is refused with open sportsbook bets (GP020), under the per-tenant advisory lock | `0121` item B |
| Player **registration and login read `brands` but never check `brands.status`** (a suspended brand still registers players) | `identity.GetBrandBySlug`, `RegisterPlayer*` |
| `tenants.licensing_model` is `under_platform_licence|own_licence`, coupled to `licences.licensee` by the 0007 composite FK and to credential ownership (ADR 0006) | `0001`, `0007`, `tenant_licence_admin.go` |
| `jurisdiction.EvaluateLicenceValidity` is the single technical licence-validity predicate | ADR 0045 4.1 |
| `providerkind.RefuseSyntheticInProduction` already refuses to START a production process with any Synthetic or unmarked provider component | `internal/providerkind/guard.go` |
| Actor proof `platform` scope is admitted only for `capability_grant:` and `financial_policy_change:` operations; only `adjustment`, `payments`, `capability` may sign | `actorproof.go:151-153`; ADR 0110 section 7 |

Consequence: today a brand is "live" the moment it is created, by a tenant-scoped role, with no recorded licensing
basis, jurisdiction reference, readiness view or approver. The owner directive requires the opposite.

## 2. Decision summary

1. **Status stays the single runtime truth.** `tenants.status` and `brands.status` remain the only values every gate
   reads. Gate code (R3, H-SEC-5/11, H(8), 0121) is **not changed**. What changes is *how a status may change*: only
   through a governed, audited, proof-bound decision, enforced by the database.
2. **New status `pending_launch`** on both columns, and it becomes the **default** for new rows. Every existing gate
   already refuses it (it is not `active`). No existing row changes value.
3. **Append-only decision records**: `launch_authorisation_requests`, `launch_authorisation_approvals`,
   `launch_status_transitions` (section 4). A status UPDATE without a same-transaction transition row bound to an
   executing request is refused (`LA020`), for every writer, including direct SQL by the runtime role.
4. **Manual, four-eyes, signed.** Activation, reactivation, closure and ratification need a requester and at least one
   distinct platform approver (distinct staff row AND distinct Person), each write carrying an ADR 0110 proof. The
   system never activates anything on its own, whatever the readiness result.
5. **Readiness is advisory input**, computed server-side and snapshotted into the request. A failing advisory check
   never blocks by itself; it must be **explicitly acknowledged** by code in the request and re-confirmed by the
   approver. Only the minimum hard preconditions of section 5.3 refuse.
6. **Licensing is recorded, never judged.** Four licensing models, including `other_manually_approved` and
   `not_applicable_recorded_determination`. No rule of the form "no licence => not launchable" exists anywhere.
7. **Existing rows** get a `legacy_baseline` transition with their current status (section 3.4): zero behaviour change
   for the first tenant; the record says plainly that no launch decision was taken through this mechanism.

## 3. State machine

### 3.1 Subject statuses (both `tenants.status` and `brands.status`)

```
                 activate (4E)                suspend (S)
 pending_launch ───────────────▶ active ◀──────────────────┐
      │                            │  ▲                     │
      │ close (4E)                 │  │ reactivate (4E)     │
      ▼                            ▼  │                     │
   closed ◀──── close (4E) ───── suspended ─────────────────┘
   (terminal)                       (suspend from active only)
```

| Action | From | To | Approval | Notes |
|---|---|---|---|---|
| `activate` | `pending_launch` | `active` | 4E | first launch |
| `suspend` | `active` | `suspended` | S (section 6.3) | fail-closed direction |
| `reactivate` | `suspended` | `active` | 4E | same inputs as `activate` (fresh snapshot) |
| `close` | `pending_launch`, `active`, `suspended` | `closed` | 4E | terminal; tenant closure keeps GP020 (0121); brand closure gets the mirror check (5.3 H-6) |
| `ratify` | `active` or `suspended` with a `legacy_baseline` as latest transition | unchanged | 4E | records licensing/jurisdiction/operator for a pre-existing row; no status change |

- `closed` is terminal. Reopening is **refused by the database** until HD-CTF-9 ("may a closed tenant ever be
  reopened", OPEN) is decided; the same refusal applies to brands. This makes `tenant.ChangeStatus`'s current
  `closed -> active` path impossible (it has no caller today).
- `4E` = requester plus the required number of distinct platform approvers (section 6.2). `S` = single-actor
  immediate suspension, pending owner question OQ-2.
- Tenant and brand are **separate subjects with separate decisions** (H(8) decision 23 analogue). Suspending or
  closing a tenant never rewrites its brands' status; the gates already refuse a brand of a non-active tenant.
  Activating a brand of a non-active tenant is allowed but flagged (advisory `R-TENANT-ACTIVE`): it has no runtime
  effect until the tenant is active, and it permits staged launches.

### 3.2 Request states

`pending -> executed | rejected | cancelled | expired | refused_at_execution`. `executed` happens only inside the
transaction of the approval that meets the required count (ADR 0100 / 0111 pattern). `cancelled` is requester-only.
`expired` only when `now() >= expires_at` (technical TTL 72 h, forced by trigger; not a policy value).
`refused_at_execution` when a hard precondition fails at the final approval (that transaction then commits the
refusal state and its audit row, but no status change). One `pending` request per subject (partial UNIQUE).

### 3.3 Mapping onto the existing gates (no gate code changes)

| Gate | Reads | Effect of `pending_launch` | Effect of governed transitions |
|---|---|---|---|
| R3 gameplay (0118/0121, `GameplayStatus`) | `tenants.status` under the shared advisory lock | refused (GP010); terminal stake returns are moot (no rounds can exist) | a governed tenant status UPDATE still fires `tenants_status_change_gate` (exclusive lock, GP020 on closure): race freedom unchanged |
| H-SEC-5/11 (`RequireActiveForPaymentInitiation`) | tenant (lock) + brand `FOR SHARE` | refused (`TENANT_OR_BRAND_NOT_ACTIVE`) | a governed brand UPDATE waits for in-flight `FOR SHARE` readers exactly as today |
| H(8) (`RequireBrandActive`, cascade creation/claim/receipt site) | brand `FOR SHARE` / plain read | deferred / skipped | unchanged |
| HSEC hold release (0124) | tenant + brand status | non-active => platform path only | unchanged |
| Reconciliation observation | `<> 'active'` | observed as non-active (no rows to observe) | unchanged |
| Registration (NEW, section 7.3) | brand + tenant status | refused | - |

Implementation task T2 must classify **every** reader of either status column (the grep in section 1 is the start,
not the proof) as `= 'active'` / `<> 'active'` (safe for a new value) or an explicit list of values (must be
reviewed); a static test pins that no Go or SQL code matches `'suspended'` or `'closed'` on these two columns without
an entry in a reviewed allow-list.

### 3.4 Safe migration path for existing rows (first tenant unaffected)

One migration (provisional number 0128; the orchestrator allocates), in this order:

1. Create the three tables, enums, RLS, grants and guards of section 4 **without** the status-change guard.
2. Widen both CHECKs to `('pending_launch','active','suspended','closed')`. Existing values remain valid; no row is
   updated.
3. Backfill, as the migration role, one `launch_status_transitions` row per existing tenant and per existing brand:
   `kind = 'legacy_baseline'`, `from_status NULL`, `to_status = <current status>`, `request_id NULL`,
   `actor_type = 'system'`, `note_code = 'pre_governance_state_recorded'`. The backfill must read the source rows
   with RLS satisfied the way 0008's backfill lesson requires (count source rows first, assert inserted count equals
   source count, else `RAISE`), never silently zero.
4. Only then `ALTER COLUMN status SET DEFAULT 'pending_launch'` on both tables and install the status-change guard
   (`zz_launch_status_governed`, BEFORE UPDATE OF status) and the INSERT guard (section 4.5).
5. Down: drop guards first, restore defaults to `'active'`, refuse (`RAISE`) if any row holds `pending_launch`
   (never rewrite it silently), narrow the CHECKs, drop the tables.

Effect on the first tenant: its tenant and brands keep their exact status; every gate reads the same value; no
request is created; nothing expires or suspends automatically. The back office shows "legacy baseline - no launch
decision recorded through ADR 0112" until a `ratify` decision is executed. Whether ratification becomes mandatory
before a stated milestone is OQ-3. The migration test asserts byte-identical `(id, status)` sets before and after,
and the existing H-SEC-5/11, H(8), R3 and 0121 suites run unchanged as regression evidence.

## 4. Data model

All three tables carry `tenant_id NOT NULL` (FK `tenants`), FORCE RLS, and an append-only guard. No column stores a
natural person's name, email, phone or document number; people are referenced by `staff_users.id` and
`person_id` only.

### 4.1 Closed vocabularies (CHECK constraints plus Go constants; adding a value needs a migration and an ADR)

| Name | Values |
|---|---|
| `subject_kind` | `tenant`, `brand` |
| `action` | `activate`, `suspend`, `reactivate`, `close`, `ratify` |
| `licensing_model` | `platform_licence`, `tenant_licence`, `other_manually_approved`, `not_applicable_recorded_determination` |
| `licensing_status` | `in_force`, `applied_pending`, `conditional`, `suspended`, `expired`, `not_required_per_determination` |
| `condition_code` | `restricted_markets`, `restricted_products`, `deposit_cap`, `review_by_date`, `staged_rollout`, `regulator_notification_pending`, `other` |
| `attestation_code` | section 5.2 |
| `check_code` | section 5.1 |
| `transition kind` | `governed`, `legacy_baseline`, `owner_provisioned` (section 4.5) |

`licensing_model` here is the **recorded basis of this launch decision**, deliberately separate from
`tenants.licensing_model` (two values, which keeps driving licence binding and credential ownership per ADR 0006 and
is not altered). Consistency rules are hard preconditions (5.3 H-2).

### 4.2 `launch_authorisation_requests` (content immutable after INSERT)

| Column | Type / rule |
|---|---|
| `id` | uuid, server-forced |
| `tenant_id`, `subject_kind`, `brand_id` | `brand_id` NOT NULL iff `subject_kind='brand'`; composite FK `(brand_id, tenant_id) -> brands(id, tenant_id)` |
| `action`, `from_status`, `to_status` | `from_status` = the subject's status read under lock at INSERT (forced by trigger, never caller-supplied); `to_status` derived from `action` |
| `licensing_model`, `licensing_status` | required for `activate`, `reactivate`, `ratify`; NULL allowed for `suspend`/`close` |
| `licence_id` | uuid NULL, FK `licences` |
| `determination_reference` | text 1..200, NULL; an external document reference (file id, register number), never the document |
| `determination_by_role` | text 1..100, NULL; the responsible function (e.g. "external counsel", "operator compliance officer"), not a person's name |
| `responsible_operator_name` | text 1..200; the legal entity responsible for operating the brand |
| `responsible_operator_registration` | text 0..100; company/registry number of that entity |
| `jurisdiction_ids` | uuid[] 1..32 distinct, each FK-checked by trigger |
| `condition_codes` | text[] 0..10 from the closed list |
| `conditions_note` | text 0..2000 (CHECK), PII lint in Go (section 8 item 9) |
| `attestations` | jsonb: `{code: true}` for the closed attestation list; bounded (CHECK on `jsonb` size <= 4 KB) |
| `readiness_snapshot` | jsonb <= 32 KB: `{version, computed_at, environment, checks: [{code, kind, result, detail_code}]}`, codes only |
| `readiness_snapshot_hash` | 64 hex; SHA-256 of the canonical snapshot |
| `acknowledged_check_codes` | text[]: every advisory check whose `result <> 'pass'` must be listed (5.3 H-5) |
| `reason_code` | from the ADR 0100 reason catalogue style; required |
| `requested_by`, `requested_by_scope`, `requested_by_person_id` | forced from the session (`platform` or `tenant`) |
| `required_approvals` | forced by trigger from section 6.2 (never caller-supplied) |
| `payload_hash` | forced: SHA-256 `k2_canonical` of every content column including the snapshot hash |
| `status`, `created_at`, `expires_at`, `decided_at`, `refusal_code` | state columns; only these may change, only by the guard's transitions |

### 4.3 `launch_authorisation_approvals` (append-only)

`id`, `tenant_id`, `request_id`, `decision` (`approve|reject`), `decided_by`, `decided_by_scope` (`platform` only),
`decided_by_person_id`, `payload_hash` (must equal the request's: the approver approves exactly that content and
snapshot), `readiness_snapshot_hash_at_decision` (recomputed by Go at decision time, recorded), `reason_code`,
`attestations_confirmed boolean NOT NULL` (must be `true` for `approve`), `decided_at` (forced `now()`),
`decided_txid`. UNIQUE `(request_id, decided_by_person_id)`.

### 4.4 `launch_status_transitions` (append-only; the history the back office shows)

`id`, `tenant_id`, `subject_kind`, `brand_id`, `kind`, `from_status`, `to_status`, `request_id` (NOT NULL iff
`kind='governed'`), `executed_by` (the final approver, or the suspending actor), `approver_ids uuid[]`,
`actor_type`, `note_code`, `created_at`, `txid` (forced `txid_current()`). UNIQUE `(subject, txid)`: one transition
per subject per transaction.

### 4.5 Database guards (SQLSTATE class `LA`, unused today; callers branch on code only)

| Guard | Rule | Code |
|---|---|---|
| Request INSERT | session is a valid `platform` (0099 family) or `tenant` principal session (`WithPrincipalScope`), never acting/player/service; tenant session only for its own tenant and only for `brand` subjects; forces actor columns, `from_status`, `required_approvals`, `payload_hash`, `expires_at`; validates vocabularies, bounds, FKs, the licensing consistency rules (5.3 H-2) | `LA001` session, `LA010` content |
| Request UPDATE | only `status`/`decided_at`/`refusal_code`; only the legal request transitions of 3.2; `cancelled` only by the requester (proof-bound); `expired` only when actually expired | `LA011` |
| Approval INSERT | platform session only; approver is not the requester (staff id AND Person), not already decided (Person), request `pending` and not expired; `payload_hash` matches | `LA012` |
| Transition INSERT | `governed`: an `executing` (same txid) request for that subject with matching `from`/`to`; `legacy_baseline`: never from any runtime session (migration only); `owner_provisioned`: see below | `LA013` |
| `zz_launch_status_governed` (BEFORE UPDATE OF status on `tenants` and `brands`) | a status change needs exactly one transition row for this subject in this txid with `from = OLD.status`, `to = NEW.status`; `closed` is never left (HD-CTF-9) | `LA020` |
| Subject INSERT guard (`tenants`, `brands`) | a runtime-role INSERT must have `status = 'pending_launch'`; an INSERT by the table owner (seed, fixtures) with another status writes an `owner_provisioned` transition row (system actor) automatically, so history is never missing | `LA021` |
| Brand closure open rounds (inside `zz_launch_status_governed`, transition into `closed` on `brands`) | no open sportsbook bet of a player of this brand, counted under the 0121 lock/GUC technique (H-6) | `LA022` |
| Deferred commit check | every transition row of this txid has its subject's status equal to `to_status` (no dangling transition) | `LA030` |
| Append-only | UPDATE/DELETE/TRUNCATE on approvals and transitions, DELETE on requests: refused | `LA099` |
| Proof (`zz_actor_proof_guard`, last BEFORE trigger) | ADR 0110 verifier on request INSERT, request cancel, approval INSERT | `AP001`..`AP005` |

Ordering: `zz_launch_status_governed` fires after the existing `tenants_status_change_gate` (0118/0121), so the
exclusive advisory lock and GP020 keep their current behaviour and code. Every trigger function pins
`search_path` (ADR 0108). None is `SECURITY DEFINER`. Where a guard must read tenant-RLS tables from a platform
session (5.3 H-3 jurisdiction config, H-6 brand open bets) it uses the 0121 technique: set `app.tenant_id` to the
subject's own tenant (never a caller value) transaction-locally for that one read and restore both GUCs; a test
proves the read fails closed (refuses) when visibility is lost.

### 4.6 RLS and grants

| Table | Tenant session (`app.tenant_id = X`, principal set) | Platform session | Acting / player / service |
|---|---|---|---|
| requests | SELECT, INSERT, UPDATE (cancel) own tenant | SELECT, INSERT, UPDATE all | none (+ 0099 restrictive acting fence entry) |
| approvals | SELECT own tenant | SELECT, INSERT | none |
| transitions | SELECT own tenant | SELECT, INSERT | none |
| `brands` | existing policies unchanged | NEW `brands_platform_governed_update` FOR UPDATE (platform GUC shape exact, tenant unset); the status column is still governed by `zz_launch_status_governed` | `acting_lock` unchanged (lock only) |

Grants to `igaming_runtime`: SELECT, INSERT on all three; UPDATE on requests only; no DELETE/TRUNCATE. The UPDATE
grant on `brands` stays (load-bearing for `FOR SHARE`); its status column is protected by the trigger, not by the
grant. Static test A-18 (0099) must stay green: no new NULL-arm policy.

## 5. Launch readiness

### 5.1 Derived checks (computed server-side by `launchgov.EvaluateReadiness`, never supplied by the client)

Each yields `pass | fail | not_evaluable | not_applicable`. `not_evaluable` counts as not passing.

| Code | Derivation (read-only) | Class |
|---|---|---|
| `R-SUBJECT` | subject exists, brand belongs to tenant, transition legal | HARD (H-1) |
| `R-LIC-BASIS` | licensing model/status recorded and consistent (5.3 H-2) | HARD (H-2) |
| `R-JUR-CONFIG` | each referenced jurisdiction has an in-effect `tenant_jurisdiction_configs` row for the tenant | HARD (H-3) |
| `R-LIC-VALID` | for a licence-backed model, `EvaluateLicenceValidity(licence, now) = LicenceValid` | advisory |
| `R-LIC-STATUS` | `licensing_status` in `{in_force, not_required_per_determination}` | advisory |
| `R-MKT-POLICY` | `ResolveOperatingCountryPolicy` resolves at least one enabled country for the tenant/brand (ADR 0045) | advisory |
| `R-KYC` | `kyc_ruleset_id` set and a `kyc_enforcement_policies` row exists for each jurisdiction | advisory |
| `R-AML` | `aml_ruleset_id` set per jurisdiction | advisory |
| `R-RG` | `rg_ruleset_id` set per jurisdiction; brand RG defaults present | advisory |
| `R-CURRENCIES` | every `allowed_currencies` entry is an active asset in the registry | advisory |
| `R-PAY-METHODS` | `allowed_payment_methods` non-empty and an approved provider credential handle (ADR 0096) exists for the tenant | advisory |
| `R-PROVIDER-TIER` | the running process's registered components are production-eligible (`providerkind` markers); in any non-production environment this is `fail` with `detail_code = synthetic_environment` | advisory (production start is already hard-gated by `RefuseSyntheticInProduction`) |
| `R-CATALOGUE` | the brand has at least one enabled casino game or sportsbook product | advisory |
| `R-ALERTS` | ALERT-DELIVERY-1 routing has a real recipient for P1 kinds (today: always `fail`) | advisory |
| `R-ACTOR-PROOF` | the active proof kid is provisioned (`actor_proof_key_active`) | advisory (production start is hard-gated already) |
| `R-TENANT-ACTIVE` | for a brand subject: tenant status `active` | advisory |
| `R-LEGACY` | latest transition is not an unratified `legacy_baseline` | advisory (informational for `ratify`) |
| `R-OPEN-ROUNDS` | for `close`: count of open sportsbook bets (tenant, or players of the brand) | HARD for `close` (H-6) |

### 5.2 Human attestations (recorded as `true` by the requester, confirmed by the approver; a record, not a claim)

`A-LICENSING-DETERMINATION` (the responsible legal/compliance determination behind the chosen licensing model has
been obtained and is referenced), `A-JURISDICTION-SCOPE` (the referenced jurisdictions are the intended ones),
`A-PLAYER-TERMS` (player terms, privacy notice and RG information are published for the brand),
`A-AML-PROGRAMME`, `A-RG-PROGRAMME`, `A-SUPPORT-READY`, `A-PROVIDER-CONTRACTS` (the vendor contracts the brand
relies on permit this use, ADR 0006's sub-operator question). An attestation left `false` is allowed and becomes a
listed acknowledgement like a failing advisory check. UI copy for every attestation states that the platform has not
verified it.

### 5.3 Hard preconditions (refuse; the minimum, each justified)

| # | Precondition | Why it is hard (not a legal judgement) |
|---|---|---|
| H-1 | subject integrity and a legal transition of section 3 | data integrity; `closed` terminal pending HD-CTF-9 |
| H-2 | a licensing model and status are recorded and **internally consistent**: `platform_licence` => `licence_id` set, `licences.licensee='platform'`, `tenants.licensing_model='under_platform_licence'`, `tenants.licence_id = licence_id`; `tenant_licence` => same with `tenant`/`own_licence`; `other_manually_approved` => `determination_reference` and `determination_by_role` set (`licence_id` optional, same consistency if set); `not_applicable_recorded_determination` => `licence_id` NULL, `determination_reference` and `determination_by_role` set, `licensing_status = 'not_required_per_determination'` | the directive requires the basis to be recorded; the record must not contradict the licence registry. Every model is always available, so this never encodes "no licence = no launch" |
| H-3 | at least one jurisdiction referenced, each with an in-effect tenant jurisdiction configuration | jurisdiction is first-class (CLAUDE.md, ADR 0006); without a config row the compliance rulesets resolve nothing for that jurisdiction |
| H-4 | four-eyes count met, distinct staff and Persons, valid proofs, approver confirms attestations | governance itself |
| H-5 | every advisory check whose result is not `pass` **and** every `false` attestation is listed in `acknowledged_check_codes`; at execution Go re-evaluates readiness and refuses (`refused_at_execution`, `readiness_changed_unacknowledged`) if any check not acknowledged in the request now fails | makes advisory input explicit; never auto-blocks a human decision, never lets a silent regression through |
| H-6 | `close`: no open gaming rounds (tenant: existing GP020; brand: open sportsbook bets of the brand's players, same lock discipline) | mirrors owner decision Q-GP-1 at brand level, in the refusing direction only |

Nothing else refuses. In particular a non-production environment, missing alert recipients, an expired licence
under a licence-backed model, or an unratified legacy baseline are acknowledged inputs, not blocks.

## 6. Authorization, four-eyes and actor proof

### 6.1 Static governance permissions (ADR 0099 3.2 style: not grantable, no K1 grant, no acting session)

| Permission | Roles |
|---|---|
| `launch_authorisation:request` | `platform_admin` (any tenant); `tenant_admin` and `compliance` (own tenant, `brand` subject only) |
| `launch_authorisation:approve` | `platform_admin` only |
| `launch_authorisation:suspend` | `platform_admin` (any); `tenant_admin`, `compliance` (own tenant's brands only) |
| `launch_authorisation:read` | `platform_admin`, `tenant_admin`, `compliance` (own tenant) |

Tenant subjects (tenant activation, suspension, reactivation, closure, ratification) are platform-only on both
sides. These are not financial capabilities; they are not added to the 0112 grant catalogue and an acting session
cannot reach them (R-10 analogue, restrictive fence). `PermTenantWrite`/`PermBrandWrite` keep creating rows, now
always `pending_launch`.

### 6.2 Four-eyes

- `required_approvals` (forced by trigger): `1` distinct platform approver for brand `activate`/`reactivate`/`ratify`;
  `2` distinct platform approvers for any `close` and for tenant `activate`. These floors are the engineering default
  pending OQ-1; they are not lowerable by any tenant row or runtime setting.
- Self-approval ban: approver != requester by staff id AND by `person_id`; two approvals by one Person count once
  (UNIQUE). A NULL `person_id` on requester or approver refuses (0099 R-4). The S-1 caveat of ADR 0099 section 9
  applies: Person distinctness is defence in depth; the structural control is that the approver is a platform
  principal a tenant cannot mint over HTTP.
- Execution happens in the final approval's transaction: lock request `FOR UPDATE` -> re-check H-1..H-6 -> insert
  transition -> UPDATE subject status (fires the 0118/0121 gate and `zz_launch_status_governed`) -> mark request
  `executed` -> one audit row. `SET LOCAL lock_timeout = '5s'` as in `ChangeStatus` (the status change waits for
  in-flight gameplay postings and `FOR SHARE` gate readers).

### 6.3 Suspension (fail-closed direction)

Default design (pending OQ-2): one actor with `launch_authorisation:suspend` suspends immediately: a request row with
`action='suspend'`, `required_approvals = 0`, inserted and executed in one transaction with a proof, reason code
mandatory, audit row. This follows the kill-switch asymmetry (ADR 0045 7.1 item 4): stopping must not wait for a
second approver. Reactivation is always four-eyes. A tenant-scoped suspender may suspend only its own brands and can
never reactivate them alone.

### 6.4 Signed actor proof (ADR 0110 extension; `security` owns this change)

| Write | Operation | Scope | Target | Payload hash |
|---|---|---|---|---|
| request INSERT | `launch_authorisation:request` (or `:suspend`) | `platform` (NULL tenant) or `tenant` | `new` | digest of all caller-supplied content columns incl. `readiness_snapshot_hash` |
| request cancel | `launch_authorisation:cancel` | requester's scope | request id | request `payload_hash`; requester only |
| approval INSERT | `launch_authorisation:approve` / `:reject` | `platform` | request id | request `payload_hash` |

Changes required: `actor_proof_require` admits `platform` scope for the `launch_authorisation:` prefix (SQL) and
`platformOperation` does the same (Go), in lockstep; the governed-table list grows by two (requests, approvals) and
the "last BEFORE trigger" catalog test covers them; the new signing package `internal/launchgov` is added to
`TestStatic_OnlyFourEyesPackagesSign`'s allow-list (security review of that allow-list change is mandatory). The
`platform_acting` scope is refused for these operations. Transitions are bound through their same-txid executing
request, as 0110 section 3 does for K2/K3 execution.

## 7. Fixing TENANT-STATUS-AUTHZ-1 and adjacent gaps

1. `tenant.ChangeStatus` stops being a public entry point: its body becomes the unexported executor step inside
   `internal/launchgov` (or is deleted and re-expressed there), keeping the GP020 translation and the separate-
   transaction refusal audit. A static test pins that the only non-test writer of `tenants.status` and
   `brands.status` is the launchgov executor (go/ast over `UPDATE tenants`/`UPDATE brands ... status`).
2. Independently of Go, `zz_launch_status_governed` refuses any status change without a same-txid governed transition,
   so a runtime-role SQL session (tenant or platform shape) cannot change either status. The tenant-scoped
   `brands.status` rewrite path of section 1 is closed by the same trigger.
3. **Registration gate (new):** `RegisterPlayer*` refuses unless the brand and its tenant are `active` (read in the
   registering transaction; brand `FOR SHARE`, tenant via `GameplayStatus`), fail closed, 409
   `BRAND_NOT_ACCEPTING_REGISTRATIONS`. For `pending_launch` this is required (no players before launch). For
   `suspended` it is a behaviour change for suspended brands only (none exist in the first tenant's data); it is
   included in OQ-2. Login is refused only for `pending_launch` (no player can exist there; defence in depth);
   login on `suspended`/`closed` is unchanged so players can still see balances.
4. `identity.CreateBrand` and `CreateTenant` insert `pending_launch` (and the `brand.created`/`tenant.created` audit
   `after` shows it). `tenant_admin` keeps brand creation; creation no longer means launch.
5. Closure refusal keeps its own audit action; HD-CTF-9 stays OPEN and is not decided here.

## 8. Audit, isolation and fail-closed rules

1. **Audit once.** Exactly one `audit_log` row per event, in the event's own transaction: `launch_authorisation.
   requested|approved|rejected|cancelled|expired|executed|refused_at_execution|suspended|ratified`. A refusal that
   rolls back (proof, guard, GP020) writes one `..._denied` row in a separate transaction (the `ChangeStatus`
   precedent), at most once per call; the denial row carries codes and counts only. `tenant.status_changed` is
   superseded by `launch_authorisation.executed` (metadata: subject, before/after status, request id, approver ids,
   licensing model, jurisdiction codes, acknowledged codes, snapshot hash).
2. Platform-scope rows set `subject_tenant_id` (ADR 0104) so the tenant sees platform decisions on its own tenant
   and brands; the actions are added to `audit.TenantPresentation` with `ReasonCode` and `BeforeAfter` only.
3. Tenant isolation: RLS on all three tables; tenant callers are confined by `canActOnTenant` at the route and by
   RLS at the database; ids from another tenant answer 404 (K1-C1 discipline: every query filters by the
   route-validated tenant, including platform callers).
4. `tenant_id` and `brand_id` come only from the route and the server-side brand lookup, never the body. Actor
   columns are forced by triggers from the session; proofs come only from the authenticated principal.
5. Fail closed: an unreadable subject, licence, config or count refuses; a missing proof issuer is a 500 (ADR 0110
   section 7), never a silent skip; readiness `not_evaluable` is not `pass`.
6. Readiness evaluation is read-only and has no side effects; `GET .../launch-readiness` writes nothing (no audit).
7. Conditions are records. No condition code is machine-enforced by this ADR; enforcing one needs its own control
   (for example an operating-market policy row). The UI says so.
8. Bounded inputs: text lengths and array sizes by CHECK; jsonb size by CHECK; reason codes from a closed catalogue.
9. PII lint (Go, before INSERT): `conditions_note`, `determination_reference`, operator fields refuse email
   addresses, phone-number shapes, IBAN/PAN-like digit runs (>= 9 digits) and wallet-address shapes; the database
   bounds length only. The lint is a guard against accidents, not a classification guarantee.

## 9. HTTP API and Back Office (list only; OpenAPI added with the routes)

**Routes** (all under `auth.Middleware`, `RequirePermission`, `canActOnTenant`; JSON; closed error codes `LAUNCH_*`):

1. `GET  /v1/admin/tenants/{tenantID}/launch-readiness?brand_id=` - derived checks and attestation list (read-only).
2. `POST /v1/admin/tenants/{tenantID}/launch-authorisations` - create a request (`subject_kind`, `brand_id`, `action`,
   licensing fields, jurisdictions, operator, conditions, attestations, acknowledged codes, reason code; the server
   computes and snapshots readiness and returns the snapshot it bound).
3. `GET  /v1/admin/tenants/{tenantID}/launch-authorisations` and `.../{requestID}` - list/detail incl. approvals.
4. `POST .../launch-authorisations/{requestID}/approve` | `/reject` | `/cancel`.
5. `POST /v1/admin/tenants/{tenantID}/brands/{brandID}/suspend` and `POST /v1/admin/tenants/{tenantID}/suspend` -
   immediate suspension (section 6.3).
6. `GET  /v1/admin/tenants/{tenantID}/launch-status-history?brand_id=` - transitions, including legacy baseline rows.

Errors: 409 `LAUNCH_PRECONDITION_FAILED` (with check codes), `LAUNCH_UNACKNOWLEDGED_CHECKS`, `LAUNCH_REQUEST_PENDING`,
`LAUNCH_SELF_APPROVAL`, `LAUNCH_ILLEGAL_TRANSITION`, `LAUNCH_SUBJECT_CLOSED`, `TENANT_CLOSE_BLOCKED_OPEN_ROUNDS`
(existing); 404 for foreign ids; 403 for permission; 500 for a signing failure.

**Back Office / partner console screens:** launch status panel on tenant and brand detail (status badge incl.
"pending launch" and "legacy baseline - not ratified"); readiness checklist split into derived checks and human
attestations, with per-item acknowledgement checkboxes and the fixed disclaimer "a recorded decision, not a legal
authorisation"; request form; platform approval queue (content diff, snapshot, who requested, approve disabled for
the requester and for the same Person); immediate-suspend action with mandatory reason; history timeline; tenant
partner-console view of its own requests, history and the ADR 0104 platform-actions projection.

## 10. Test plan (runtime role, private scratch databases, `-race -tags integration`)

**Migration:** up/down/up whole-schema snapshot; existing `(id, status)` byte-identical; one baseline row per
existing tenant and brand (count asserted, including a seeded non-empty fixture with RLS active); down refuses with a
`pending_launch` row; defaults are `pending_launch` after up and `active` after down.

**Behaviour preservation (golden):** the existing H-SEC-5/11, H(8) (incl. receipt site), R3/0121 and HSEC suites run
unchanged and green; an explicit matrix proves every gate refuses `pending_launch` for tenant and brand.

**Happy paths:** brand activate with all checks passing; with failing advisory checks acknowledged; tenant activate
with two approvers; suspend/reactivate cycle; close; ratify a legacy baseline (status unchanged, transition
recorded); each writes exactly one audit row per event with the right `subject_tenant_id`.

**Adversarial (each must refuse with the stated code and change nothing):**
1. Direct SQL `UPDATE brands SET status='active'` from a tenant-scoped runtime session (`LA020`); same for
   `tenants` from a platform session; same inside a transaction that inserted a forged transition row without an
   executing request (`LA013`); a transition row with no matching UPDATE (`LA030`).
2. `INSERT INTO brands ... status='active'` by the runtime role (`LA021`); `CreateBrand` always yields
   `pending_launch`.
3. Self-approval by staff id; by a second staff account of the same Person; approval by a tenant session; approval
   by an acting session (`LA012`, `LA001`).
4. Proofs: none (AP001), wrong key (AP002), expired (AP003), wrong operation/target/payload/scope, including a
   `platform_acting` proof and a proof for another request (AP004), replayed nonce (AP005).
5. Cross-tenant: request/approve/cancel a request id of tenant B via tenant A's path (404); a tenant admin naming a
   brand of another tenant (FK and 404); a tenant session requesting a `tenant` subject (`LA001`).
6. Content tampering after INSERT (`LA011`); approval with a different `payload_hash` (`LA012`).
7. Unacknowledged failing check at request (`LA010`) and a check that flips to `fail` between request and final
   approval (`refused_at_execution`, no status change, audit row).
8. Licensing consistency: `platform_licence` with a tenant-licensee licence, with a licence not bound to the tenant,
   `not_applicable_recorded_determination` with a licence id or without a determination reference (`LA010`); every
   model accepted when consistent, including `not_applicable` (asserts no hard-coded licence requirement).
9. Reopen a `closed` subject (`LA020`); close a tenant with an open sportsbook bet (GP020, refusal audit once);
   close a brand with an open bet of its player (`LA022`, H-6).
10. Concurrency (repeat x50 under `-race`): two final approvals -> exactly one execution and one transition;
    suspension racing an activation approval -> serialised, final status consistent with the transition log;
    a governed status change racing a gameplay posting and a deposit initiation -> the 0118 lock and the brand
    `FOR SHARE` serialise them (posting either sees the new status or delays the change); two pending requests
    for one subject (unique violation).
11. Registration on `pending_launch` and (subject to OQ-2) `suspended` brands refused; login on `pending_launch`
    refused; other brands of the same tenant unaffected.
12. PII lint rejects email/phone/IBAN-like content; size bounds enforced by CHECK.
13. Static: only `internal/launchgov` writes either status column; only allowed packages sign; no code compares
    these columns to `'suspended'`/`'closed'` outside the reviewed list; `zz_actor_proof_guard` and
    `zz_launch_status_governed` ordering pinned by catalog tests; no new NULL-arm policy (A-18).
14. Mutation-kill evidence for every guard branch (remove the txid binding, the Person check, the closed-terminal
    rule, the acknowledgement rule, the 0121-technique GUC restore).

## 11. Decisions: owner versus engineering

**Owner questions (minimum; each with a recommendation; the design fails closed until answered):**

- **OQ-1 Approval composition.** Recommendation: brand activation, reactivation and ratification need the requester
  plus 1 distinct platform approver; tenant activation and any closure need 2 distinct platform approvers; tenant
  staff may request (never approve) for their own brands. Alternative: 2 platform approvers for every launch action.
- **OQ-2 Suspension and registrations.** Recommendation: a single authorised actor may suspend immediately (reason
  code, audited, reactivation four-eyes), and a suspended (not only pending) brand refuses new player registrations.
  Alternative: four-eyes for suspension as well, and/or keep registrations open on suspended brands.
- **OQ-3 Existing first-tenant rows.** Recommendation: record them as `legacy_baseline` with no behaviour change, and
  require a `ratify` decision before the first non-MOCK (real-money) provider traffic for that tenant, surfaced as an
  advisory/launch-checklist item only, never as an automatic suspension. Alternative: no ratification requirement.

**Already open elsewhere, not re-asked:** HD-CTF-9 (reopening closed), NULL-ARM-WRITE-1, STAFF-LIFECYCLE-1 (S-1),
ADR 0110 T3/T4 (identity store), ALERT-DELIVERY-1. They bound the residuals in section 12.

**Engineering calls made here (reversible, within scope):** status stays the runtime truth; `pending_launch` value
and default; three-table model and SQLSTATE class `LA`; the advisory/hard split of section 5.3; explicit
acknowledgement rule; 72 h request TTL; `closed` terminal pending HD-CTF-9; static (non-grantable) permissions;
proof scopes; brand closure open-bet mirror; registration gate for `pending_launch`; legacy baseline backfill; PII
lint; separate `licensing_model` on the decision rather than altering `tenants.licensing_model`; tenant and brand as
separate subjects.

## 12. Residuals (stated honestly)

1. Readiness checks prove configuration presence, not legal or operational adequacy. Attestations are unverified
   human statements.
2. Identity-store integrity (ADR 0110 T3): an attacker who can make the application authenticate a minted platform
   admin obtains genuine proofs and could approve. Same residual as every four-eyes flow.
3. The table-owner/migration role can bypass every trigger (ADR 0110 section 9.4); `owner_provisioned` rows make
   owner-path inserts visible but do not prevent them.
4. A suspension that commits after an initiation transaction committed does not stop that already-claimed attempt
   (ADR 0095 43.2, unchanged).
5. `R-PROVIDER-TIER` describes the running process, not a per-tenant provider binding; per-tenant provider tiering
   does not exist yet and is not invented here.
6. Brand-level jurisdiction scoping remains tenant-level (ADR 0012); the jurisdictions recorded on a brand decision
   are a record, enforced only through existing tenant-level config and operating-market policy.

## 13. Work breakdown (ordered; nothing starts before this ADR is ACCEPTED and OQ-1..3 are answered or defaulted by the orchestrator)

| # | Task | Owner | Review |
|---|---|---|---|
| T0 | Put OQ-1..OQ-3 to the owner; record answers as an amendment here | orchestrator | - |
| T1 | Reviews of this ADR; apply conditions; status ACCEPTED | architect | security, ledger-finance, identity-compliance, product-owner-proxy, qa |
| T2 | Status-reader inventory (Go and SQL) and the static vocabulary pin | backend | code-reviewer, architect |
| T3 | Actor-proof extension (ops, platform-scope allowance, signer allow-list) | security (implements) | code-reviewer, ledger-finance |
| T4 | Migration: tables, vocabularies, RLS, grants, guards, CHECK widening, baseline backfill, defaults, down | backend | security, ledger-finance (0118/0121 interplay), code-reviewer, qa |
| T5 | `internal/launchgov`: readiness evaluator, request/approve/reject/cancel/suspend, executor (absorbs `ChangeStatus`), audit | backend | security, identity-compliance, code-reviewer |
| T6 | `CreateTenant`/`CreateBrand` pending; registration/login gate | identity-compliance | security, code-reviewer |
| T7 | HTTP routes, permissions, OpenAPI, error codes, ADR 0104 presentation entries | backend | security, code-reviewer |
| T8 | Back Office and partner-console screens with fixed disclaimers | backoffice (UI by frontend, copy by ux-design) | product-owner-proxy, security |
| T9 | Test suite of section 10, concurrency x50, mutation-kill evidence | qa | code-reviewer |
| T10 | Docs: architecture 15 and 36, task registry, decision register (TENANT-STATUS-AUTHZ-1 -> closed only after security review of T4-T7), HANDOVER | architect + orchestrator | - |

Labels until each task lands: all `NOT IMPLEMENTED`. After T4-T9 the mechanism may be labelled `IMPLEMENTED`; the
licensing and attestation content it records is always `PROVIDER DEPENDENT`/human-dependent and never a statement
of legal authorisation.
